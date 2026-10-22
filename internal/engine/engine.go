// Package engine holds the handoff logic: it polls guest status, performs
// start/shutdown/switch operations one at a time, and returns a group to its
// default VM once it has been idle long enough.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/yniverz/pve-vm-handoff/internal/config"
	"github.com/yniverz/pve-vm-handoff/internal/proxmox"
)

// Guest status values beyond what Proxmox reports.
const (
	StatusUnknown = "unknown" // host unreachable or not polled yet
	StatusMissing = "missing" // host reachable but the vmid does not exist
	StatusStopped = "stopped"
	StatusRunning = "running"
)

var (
	ErrBusy      = errors.New("another operation is in progress")
	ErrForbidden = errors.New("not allowed")
	ErrNotFound  = errors.New("not found")
)

type VMState struct {
	Status string
	Node   string
	Type   string
	Uptime int64
	Error  string
}

// occupied means the guest may hold the shared resource. Unknown counts too, to stay safe.
func (s *VMState) occupied() bool { return s.Status != StatusStopped }

type Event struct {
	Time    time.Time `json:"time"`
	Kind    string    `json:"kind"`
	Group   string    `json:"group,omitempty"`
	VM      string    `json:"vm,omitempty"`
	User    string    `json:"user,omitempty"`
	Message string    `json:"message"`
}

type Options struct {
	// How often task and status polls repeat while an operation is running.
	StepInterval time.Duration
	Now          func() time.Time
	Logger       *slog.Logger
}

type Engine struct {
	cfg  *config.Config
	apis map[string]proxmox.API
	ctx  context.Context
	opts Options
	log  *slog.Logger
	sink chan Event

	opMu sync.Mutex // held for the whole of one power operation

	mu           sync.Mutex
	states       map[string]*VMState
	hostErr      map[string]string
	busy         string
	idleSince    map[string]time.Time
	lastOccupant map[string]string
	paused       map[string]bool
	events       []Event
	lastRefresh  time.Time
}

func New(ctx context.Context, cfg *config.Config, apis map[string]proxmox.API, opts Options) *Engine {
	if opts.StepInterval == 0 {
		opts.StepInterval = 2 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	e := &Engine{
		cfg: cfg, apis: apis, ctx: ctx, opts: opts, log: opts.Logger,
		sink:         make(chan Event, 64),
		states:       map[string]*VMState{},
		hostErr:      map[string]string{},
		idleSince:    map[string]time.Time{},
		lastOccupant: map[string]string{},
		paused:       map[string]bool{},
	}
	for id := range cfg.VMs {
		e.states[id] = &VMState{Status: StatusUnknown}
	}
	e.loadState()
	return e
}

// Events delivers every recorded event, for notifications.
func (e *Engine) Events() <-chan Event { return e.sink }

// Run polls until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(e.cfg.Server.PollInterval.D())
	defer t.Stop()
	for {
		e.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick refreshes status and applies the return-to-default rule, unless an operation is running.
func (e *Engine) Tick(ctx context.Context) {
	if !e.opMu.TryLock() {
		return
	}
	defer e.opMu.Unlock()
	e.refresh(ctx)
	e.checkDefaults(ctx)
}

// Permitted checks whether user may run action on vmID.
func (e *Engine) Permitted(vmID, action, user string) error {
	vm, ok := e.cfg.VMs[vmID]
	if !ok {
		return ErrNotFound
	}
	if !slices.Contains(vm.Actions, action) {
		return fmt.Errorf("%w: %s is not enabled for %s", ErrForbidden, action, vm.Name)
	}
	if !config.Allowed(vm.AllowedUsers, user) {
		return fmt.Errorf("%w: %s may not control %s", ErrForbidden, displayUser(user), vm.Name)
	}
	return nil
}

// Start runs an action in the background. It fails fast when not permitted or busy.
func (e *Engine) Start(user, vmID, action string) error {
	if err := e.Permitted(vmID, action, user); err != nil {
		return err
	}
	if !e.opMu.TryLock() {
		return ErrBusy
	}
	go func() {
		defer e.opMu.Unlock()
		_ = e.execute(e.ctx, user, vmID, action)
	}()
	return nil
}

// Execute runs an action and waits for it. Used by tests and the CLI path.
func (e *Engine) Execute(ctx context.Context, user, vmID, action string) error {
	if err := e.Permitted(vmID, action, user); err != nil {
		return err
	}
	if !e.opMu.TryLock() {
		return ErrBusy
	}
	defer e.opMu.Unlock()
	return e.execute(ctx, user, vmID, action)
}

func (e *Engine) execute(ctx context.Context, user, vmID, action string) error {
	vm := e.cfg.VMs[vmID]
	defer e.setBusy("")
	defer e.refresh(ctx)

	var err error
	switch action {
	case config.ActionStart:
		e.setBusy("Starting " + vm.Name)
		err = e.switchTo(ctx, user, vm)
	case config.ActionShutdown:
		e.setBusy("Shutting down " + vm.Name)
		e.refresh(ctx)
		err = e.shutdown(ctx, vm, vm.ForceStopAfterTimeout)
		if err == nil {
			e.record(Event{Kind: "shutdown", VM: vm.ID, User: user, Message: vm.Name + " shut down"})
		}
	case config.ActionStop:
		e.setBusy("Stopping " + vm.Name)
		e.refresh(ctx)
		err = e.stop(ctx, vm)
		if err == nil {
			e.record(Event{Kind: "stop", VM: vm.ID, User: user, Message: vm.Name + " force-stopped"})
		}
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		e.record(Event{Kind: "error", VM: vm.ID, User: user, Message: fmt.Sprintf("%s %s failed: %v", action, vm.Name, err)})
	}
	return err
}

// switchTo shuts down every running VM that shares a group with vm, then starts vm.
func (e *Engine) switchTo(ctx context.Context, user string, vm *config.VM) error {
	e.refresh(ctx)
	st := e.state(vm.ID)
	if st.Status == StatusUnknown || st.Status == StatusMissing {
		return fmt.Errorf("status of %s is %s: %s", vm.Name, st.Status, st.Error)
	}

	var displace []*config.VM
	for _, id := range e.cfg.Conflicts(vm.ID) {
		other := e.cfg.VMs[id]
		ost := e.state(id)
		if ost.Status == StatusUnknown {
			return fmt.Errorf("status of %s is unknown (%s), refusing to start %s", other.Name, ost.Error, vm.Name)
		}
		if ost.occupied() && ost.Status != StatusMissing {
			if !other.Displaceable {
				return fmt.Errorf("%s is running and may not be shut down by others", other.Name)
			}
			displace = append(displace, other)
		}
	}
	if st.Status == StatusRunning && len(displace) == 0 {
		return nil
	}

	var names []string
	for _, other := range displace {
		e.setBusy(fmt.Sprintf("Shutting down %s to start %s", other.Name, vm.Name))
		if err := e.shutdown(ctx, other, other.ForceStopAfterTimeout); err != nil {
			return err
		}
		names = append(names, other.Name)
	}
	e.setBusy("Starting " + vm.Name)
	if err := e.start(ctx, vm); err != nil {
		return err
	}
	if len(names) > 0 {
		e.record(Event{Kind: "switch", VM: vm.ID, User: user, Message: fmt.Sprintf("Switched from %s to %s", joinNames(names), vm.Name)})
	} else {
		e.record(Event{Kind: "start", VM: vm.ID, User: user, Message: vm.Name + " started"})
	}
	return nil
}

func (e *Engine) start(ctx context.Context, vm *config.VM) error {
	st := e.state(vm.ID)
	if st.Status == StatusRunning {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, vm.StartTimeout.D())
	defer cancel()
	upid, err := e.apis[vm.Host].Action(ctx, st.Node, st.Type, vm.VMID, "start", nil)
	if err != nil {
		return err
	}
	if err := proxmox.WaitTask(ctx, e.apis[vm.Host], st.Node, upid, e.opts.StepInterval); err != nil {
		return fmt.Errorf("start %s: %w", vm.Name, err)
	}
	return e.waitStatus(ctx, vm, StatusRunning)
}

func (e *Engine) shutdown(ctx context.Context, vm *config.VM, force bool) error {
	st := e.state(vm.ID)
	if st.Status == StatusStopped {
		return nil
	}
	if st.Status == StatusUnknown || st.Status == StatusMissing {
		return fmt.Errorf("status of %s is %s: %s", vm.Name, st.Status, st.Error)
	}
	timeout := vm.ShutdownTimeout.D()
	params := url.Values{"timeout": {strconv.Itoa(int(timeout.Seconds()))}}
	sctx, cancel := context.WithTimeout(ctx, timeout+15*time.Second)
	defer cancel()
	upid, err := e.apis[vm.Host].Action(sctx, st.Node, st.Type, vm.VMID, "shutdown", params)
	if err == nil {
		err = proxmox.WaitTask(sctx, e.apis[vm.Host], st.Node, upid, e.opts.StepInterval)
	}
	if err == nil {
		err = e.waitStatus(sctx, vm, StatusStopped)
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	e.refresh(ctx)
	if e.state(vm.ID).Status == StatusStopped {
		return nil
	}
	if !force {
		return fmt.Errorf("graceful shutdown of %s did not finish within %s: %w", vm.Name, timeout, err)
	}
	e.log.Warn("graceful shutdown failed, force-stopping", "vm", vm.ID, "err", err)
	e.setBusy("Force-stopping " + vm.Name)
	return e.stop(ctx, vm)
}

func (e *Engine) stop(ctx context.Context, vm *config.VM) error {
	st := e.state(vm.ID)
	if st.Status == StatusStopped {
		return nil
	}
	if st.Status == StatusUnknown || st.Status == StatusMissing {
		return fmt.Errorf("status of %s is %s: %s", vm.Name, st.Status, st.Error)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	upid, err := e.apis[vm.Host].Action(ctx, st.Node, st.Type, vm.VMID, "stop", nil)
	if err != nil {
		return err
	}
	if err := proxmox.WaitTask(ctx, e.apis[vm.Host], st.Node, upid, e.opts.StepInterval); err != nil {
		return fmt.Errorf("stop %s: %w", vm.Name, err)
	}
	return e.waitStatus(ctx, vm, StatusStopped)
}

// waitStatus refreshes until the VM reaches want; the task may finish a moment before the status flips.
func (e *Engine) waitStatus(ctx context.Context, vm *config.VM, want string) error {
	for {
		e.refresh(ctx)
		if e.state(vm.ID).Status == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not reach %s: %w", vm.Name, want, ctx.Err())
		case <-time.After(e.opts.StepInterval):
		}
	}
}

// refresh polls every host once and updates guest status and group occupants.
func (e *Engine) refresh(ctx context.Context) {
	type result struct {
		host string
		res  []proxmox.Resource
		err  error
	}
	results := make(chan result, len(e.apis))
	for name, api := range e.apis {
		go func() {
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			res, err := api.Resources(cctx)
			results <- result{name, res, err}
		}()
	}
	byHost := map[string]result{}
	for range e.apis {
		r := <-results
		byHost[r.host] = r
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for host, r := range byHost {
		msg := ""
		if r.err != nil {
			msg = r.err.Error()
		}
		if msg != e.hostErr[host] {
			if msg != "" {
				e.log.Warn("host unreachable", "host", host, "err", msg)
			} else {
				e.log.Info("host reachable again", "host", host)
			}
			e.hostErr[host] = msg
		}
	}
	for id, vm := range e.cfg.VMs {
		r := byHost[vm.Host]
		st := e.states[id]
		if r.err != nil {
			st.Status, st.Error = StatusUnknown, r.err.Error()
			continue
		}
		found := false
		for _, res := range r.res {
			if res.VMID != vm.VMID || (vm.Type != "" && res.Type != vm.Type) {
				continue
			}
			found = true
			st.Status, st.Node, st.Type, st.Uptime, st.Error = res.Status, res.Node, res.Type, res.Uptime, ""
			if vm.Node != "" && res.Node != vm.Node {
				st.Error = fmt.Sprintf("found on node %s instead of configured %s", res.Node, vm.Node)
			}
			break
		}
		if !found {
			st.Status, st.Error = StatusMissing, fmt.Sprintf("vmid %d not found on %s", vm.VMID, vm.Host)
		}
	}

	changed := false
	for _, g := range e.cfg.Groups {
		var running []string
		for _, m := range g.Members {
			if e.states[m].Status == StatusRunning {
				running = append(running, m)
			}
		}
		if len(running) == 1 && e.lastOccupant[g.ID] != running[0] {
			e.lastOccupant[g.ID] = running[0]
			changed = true
		}
	}
	e.lastRefresh = e.opts.Now()
	if changed {
		e.saveStateLocked()
	}
}

// checkDefaults starts a group's default VM once every member has been stopped for return_delay.
func (e *Engine) checkDefaults(ctx context.Context) {
	now := e.opts.Now()
	for _, gid := range e.cfg.GroupOrder {
		g := e.cfg.Groups[gid]
		if !e.returnArmed(g) {
			e.clearIdle(gid)
			continue
		}
		e.mu.Lock()
		since, counting := e.idleSince[gid]
		if !counting {
			e.idleSince[gid] = now
		}
		e.mu.Unlock()
		if !counting || now.Sub(since) < g.ReturnDelay.D() {
			continue
		}

		def := e.cfg.VMs[g.Default]
		if blocker := e.blockedBy(def.ID); blocker != "" {
			continue // another group the default belongs to is in use
		}
		e.clearIdle(gid)
		e.setBusy("Returning " + g.Name + " to " + def.Name)
		err := e.start(ctx, def)
		e.setBusy("")
		if err != nil {
			e.record(Event{Kind: "error", Group: gid, VM: def.ID, Message: fmt.Sprintf("Returning %s to %s failed: %v", g.Name, def.Name, err)})
			continue
		}
		e.record(Event{Kind: "auto_return", Group: gid, VM: def.ID, Message: fmt.Sprintf("%s was free for %s, started %s again", g.Name, g.ReturnDelay.D(), def.Name)})
	}
}

// returnArmed reports whether the group should count down towards starting its default.
func (e *Engine) returnArmed(g *config.Group) bool {
	if g.Default == "" || g.ReturnToDefault == config.ReturnNever {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.paused[g.ID] {
		return false
	}
	for _, m := range g.Members {
		if e.states[m].occupied() {
			return false
		}
	}
	if g.ReturnToDefault == config.ReturnAfterHandoff {
		last := e.lastOccupant[g.ID]
		return last != "" && last != g.Default
	}
	return true
}

// blockedBy returns the name of a running VM that shares any group with vmID.
func (e *Engine) blockedBy(vmID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range e.cfg.Conflicts(vmID) {
		if e.states[id].occupied() {
			return e.cfg.VMs[id].Name
		}
	}
	return ""
}

func (e *Engine) clearIdle(gid string) {
	e.mu.Lock()
	delete(e.idleSince, gid)
	e.mu.Unlock()
}

// SetPaused pauses or resumes the automatic return for a group.
func (e *Engine) SetPaused(gid string, paused bool, user string) error {
	g, ok := e.cfg.Groups[gid]
	if !ok {
		return ErrNotFound
	}
	if !config.Allowed(g.AllowedUsers, user) {
		return fmt.Errorf("%w: %s may not change %s", ErrForbidden, displayUser(user), g.Name)
	}
	e.mu.Lock()
	if e.paused[gid] == paused {
		e.mu.Unlock()
		return nil
	}
	e.paused[gid] = paused
	delete(e.idleSince, gid)
	e.saveStateLocked()
	e.mu.Unlock()
	verb := "resumed"
	if paused {
		verb = "paused"
	}
	e.record(Event{Kind: "pause", Group: gid, User: user, Message: fmt.Sprintf("Automatic return for %s %s", g.Name, verb)})
	return nil
}

func (e *Engine) state(id string) VMState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return *e.states[id]
}

func (e *Engine) setBusy(s string) {
	e.mu.Lock()
	e.busy = s
	e.mu.Unlock()
}

func (e *Engine) record(ev Event) {
	ev.Time = e.opts.Now()
	e.log.Info(ev.Message, "kind", ev.Kind, "vm", ev.VM, "group", ev.Group, "user", ev.User)
	e.mu.Lock()
	e.events = append(e.events, ev)
	if len(e.events) > 100 {
		e.events = e.events[len(e.events)-100:]
	}
	e.mu.Unlock()
	select {
	case e.sink <- ev:
	default:
		e.log.Warn("notification queue full, dropping event")
	}
}

type persisted struct {
	Paused       map[string]bool   `json:"paused"`
	LastOccupant map[string]string `json:"last_occupant"`
}

func (e *Engine) loadState() {
	path := e.cfg.Server.StateFile
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			e.log.Warn("read state file", "err", err)
		}
		return
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		e.log.Warn("parse state file", "err", err)
		return
	}
	for g, v := range p.Paused {
		if _, ok := e.cfg.Groups[g]; ok {
			e.paused[g] = v
		}
	}
	for g, v := range p.LastOccupant {
		if _, ok := e.cfg.Groups[g]; ok {
			e.lastOccupant[g] = v
		}
	}
}

func (e *Engine) saveStateLocked() {
	path := e.cfg.Server.StateFile
	if path == "" {
		return
	}
	data, _ := json.MarshalIndent(persisted{Paused: e.paused, LastOccupant: e.lastOccupant}, "", "  ")
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, path)
		}
		if err != nil {
			e.log.Warn("write state file", "err", err)
		}
	}
}

func displayUser(u string) string {
	if u == "" {
		return "anonymous user"
	}
	return u
}

func joinNames(n []string) string {
	switch len(n) {
	case 0:
		return ""
	case 1:
		return n[0]
	}
	out := n[0]
	for _, s := range n[1 : len(n)-1] {
		out += ", " + s
	}
	return out + " and " + n[len(n)-1]
}
