package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yniverz/pve-vm-handoff/internal/config"
	"github.com/yniverz/pve-vm-handoff/internal/proxmox"
)

// fakePVE simulates a Proxmox host. Power actions take effect immediately unless configured otherwise.
type fakePVE struct {
	mu          sync.Mutex
	vms         map[int]*proxmox.Resource
	tasks       map[string]proxmox.TaskStatus
	calls       []string
	ignoreACPI  map[int]bool // guest ignores shutdown requests
	failStart   map[int]string
	unreachable bool
	n           int
}

func newFake(vms ...proxmox.Resource) *fakePVE {
	f := &fakePVE{vms: map[int]*proxmox.Resource{}, tasks: map[string]proxmox.TaskStatus{}, ignoreACPI: map[int]bool{}, failStart: map[int]string{}}
	for _, v := range vms {
		f.vms[v.VMID] = &v
	}
	return f
}

func (f *fakePVE) Resources(context.Context) ([]proxmox.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreachable {
		return nil, errors.New("dial tcp: connection refused")
	}
	var out []proxmox.Resource
	for _, v := range f.vms {
		out = append(out, *v)
	}
	return out, nil
}

func (f *fakePVE) Action(_ context.Context, node, typ string, vmid int, action string, _ url.Values) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s %d", action, vmid))
	vm, ok := f.vms[vmid]
	if !ok || vm.Node != node || vm.Type != typ {
		return "", fmt.Errorf("no such guest %s/%s/%d", node, typ, vmid)
	}
	f.n++
	upid := fmt.Sprintf("UPID:%d", f.n)
	st := proxmox.TaskStatus{Status: "stopped", ExitStatus: "OK"}
	switch action {
	case "start":
		if msg := f.failStart[vmid]; msg != "" {
			st.ExitStatus = msg
		} else {
			vm.Status = "running"
		}
	case "shutdown":
		if f.ignoreACPI[vmid] {
			st.ExitStatus = "VM quit/powerdown failed - got timeout"
		} else {
			vm.Status = "stopped"
		}
	case "stop":
		vm.Status = "stopped"
	}
	f.tasks[upid] = st
	return upid, nil
}

func (f *fakePVE) Task(_ context.Context, _ string, upid string) (proxmox.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks[upid], nil
}

func (f *fakePVE) set(vmid int, status string) {
	f.mu.Lock()
	f.vms[vmid].Status = status
	f.mu.Unlock()
}

func (f *fakePVE) status(vmid int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vms[vmid].Status
}

const baseConfig = `
hosts:
  pve:
    url: https://pve.local:8006
    token_id: handoff@pve!handoff
    token_secret: secret
defaults:
  shutdown_timeout: 1s
  start_timeout: 1s
vms:
  mine:
    host: pve
    vmid: 101
    name: Mine
  friend:
    host: pve
    vmid: 102
    name: Friend
    actions: [start, shutdown, stop]
groups:
  gpu:
    members: [mine, friend]
    default: mine
    return_delay: 60s
`

type harness struct {
	t   *testing.T
	pve *fakePVE
	eng *Engine
	now time.Time
}

func setup(t *testing.T, yaml string, mine, friend string) *harness {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	pve := newFake(
		proxmox.Resource{VMID: 101, Node: "n1", Type: "qemu", Status: mine},
		proxmox.Resource{VMID: 102, Node: "n1", Type: "qemu", Status: friend},
	)
	h := &harness{t: t, pve: pve, now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	h.eng = New(context.Background(), cfg, map[string]proxmox.API{"pve": pve}, Options{
		StepInterval: time.Millisecond,
		Now:          func() time.Time { return h.now },
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h.eng.Tick(context.Background())
	return h
}

func (h *harness) tickAfter(d time.Duration) {
	h.now = h.now.Add(d)
	h.eng.Tick(context.Background())
}

func (h *harness) expect(mine, friend string) {
	h.t.Helper()
	if got := h.pve.status(101); got != mine {
		h.t.Errorf("mine is %s, want %s", got, mine)
	}
	if got := h.pve.status(102); got != friend {
		h.t.Errorf("friend is %s, want %s", got, friend)
	}
}

func (h *harness) lastEvent() Event {
	ev := h.eng.Snapshot("").Events
	if len(ev) == 0 {
		return Event{}
	}
	return ev[0]
}

func TestSwitchShutsDownOtherMember(t *testing.T) {
	h := setup(t, baseConfig, "running", "stopped")
	if err := h.eng.Execute(context.Background(), "friend@x", "friend", "start"); err != nil {
		t.Fatal(err)
	}
	h.expect("stopped", "running")
	if ev := h.lastEvent(); ev.Kind != "switch" || ev.User != "friend@x" {
		t.Errorf("unexpected event %+v", ev)
	}
	if got := strings.Join(h.pve.calls, ","); got != "shutdown 101,start 102" {
		t.Errorf("calls = %s", got)
	}
}

func TestReturnsToDefaultAfterHandoff(t *testing.T) {
	h := setup(t, baseConfig, "running", "stopped")
	if err := h.eng.Execute(context.Background(), "", "friend", "start"); err != nil {
		t.Fatal(err)
	}
	h.pve.set(102, "stopped") // friend shuts down from inside the guest

	h.tickAfter(time.Second) // countdown starts
	h.tickAfter(30 * time.Second)
	h.expect("stopped", "stopped")
	if in := h.eng.Snapshot("").Groups[0].ReturnIn; in == nil || *in != 30 {
		t.Errorf("return_in = %v, want 30", in)
	}
	h.tickAfter(31 * time.Second)
	h.expect("running", "stopped")
	if ev := h.lastEvent(); ev.Kind != "auto_return" {
		t.Errorf("unexpected event %+v", ev)
	}
}

func TestNoReturnWhenDefaultWasShutDownByOwner(t *testing.T) {
	h := setup(t, baseConfig, "running", "stopped")
	h.pve.set(101, "stopped")
	for range 5 {
		h.tickAfter(time.Minute)
	}
	h.expect("stopped", "stopped")
}

func TestNoReturnWithoutKnownOccupant(t *testing.T) {
	h := setup(t, baseConfig, "stopped", "stopped")
	for range 5 {
		h.tickAfter(time.Minute)
	}
	h.expect("stopped", "stopped")
}

func TestReturnAlways(t *testing.T) {
	h := setup(t, strings.Replace(baseConfig, "return_delay: 60s", "return_delay: 60s\n    return_to_default: always", 1), "stopped", "stopped")
	h.tickAfter(59 * time.Second)
	h.expect("stopped", "stopped")
	h.tickAfter(2 * time.Second)
	h.expect("running", "stopped")
}

func TestPausedGroupDoesNotReturn(t *testing.T) {
	h := setup(t, baseConfig, "stopped", "running")
	if err := h.eng.SetPaused("gpu", true, ""); err != nil {
		t.Fatal(err)
	}
	h.pve.set(102, "stopped")
	for range 5 {
		h.tickAfter(time.Minute)
	}
	h.expect("stopped", "stopped")

	if err := h.eng.SetPaused("gpu", false, ""); err != nil {
		t.Fatal(err)
	}
	h.tickAfter(time.Second)
	h.tickAfter(time.Minute)
	h.expect("running", "stopped")
}

func TestStateFileKeepsPauseAndOccupant(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	yaml := baseConfig + "server:\n  state_file: " + file + "\n"
	h := setup(t, yaml, "stopped", "running")
	h.eng.SetPaused("gpu", true, "")

	h2 := setup(t, yaml, "stopped", "stopped")
	if !h2.eng.Snapshot("").Groups[0].Paused {
		t.Error("pause flag was not restored")
	}
	h2.eng.SetPaused("gpu", false, "")
	h2.tickAfter(time.Second)
	h2.tickAfter(time.Minute)
	h2.expect("running", "stopped") // last occupant "friend" was restored, so the return fires
}

func TestShutdownTimeoutWithoutForceAborts(t *testing.T) {
	h := setup(t, baseConfig, "running", "stopped")
	h.pve.ignoreACPI[101] = true
	err := h.eng.Execute(context.Background(), "", "friend", "start")
	if err == nil || !strings.Contains(err.Error(), "graceful shutdown of Mine") {
		t.Fatalf("err = %v", err)
	}
	h.expect("running", "stopped")
	if ev := h.lastEvent(); ev.Kind != "error" {
		t.Errorf("unexpected event %+v", ev)
	}
}

func TestShutdownTimeoutWithForceStops(t *testing.T) {
	h := setup(t, strings.Replace(baseConfig, "start_timeout: 1s", "start_timeout: 1s\n  force_stop_after_timeout: true", 1), "running", "stopped")
	h.pve.ignoreACPI[101] = true
	if err := h.eng.Execute(context.Background(), "", "friend", "start"); err != nil {
		t.Fatal(err)
	}
	h.expect("stopped", "running")
	if got := strings.Join(h.pve.calls, ","); got != "shutdown 101,stop 101,start 102" {
		t.Errorf("calls = %s", got)
	}
}

func TestStartFailureIsReported(t *testing.T) {
	h := setup(t, baseConfig, "running", "stopped")
	h.pve.failStart[102] = "PCI device 0000:01:00.0 already in use"
	err := h.eng.Execute(context.Background(), "", "friend", "start")
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("err = %v", err)
	}
}

func TestNotDisplaceable(t *testing.T) {
	h := setup(t, strings.Replace(baseConfig, "    name: Mine\n", "    name: Mine\n    displaceable: false\n", 1), "running", "stopped")
	if err := h.eng.Execute(context.Background(), "", "friend", "start"); err == nil {
		t.Fatal("expected an error")
	}
	h.expect("running", "stopped")
	if v := h.eng.Snapshot("").VMs[1]; v.Blocked == "" || len(v.Actions) != 0 {
		t.Errorf("friend view = %+v", v)
	}
}

func TestPermissions(t *testing.T) {
	h := setup(t, strings.Replace(baseConfig, "    actions: [start, shutdown, stop]\n", "    actions: [start, shutdown, stop]\n    allowed_users: [friend@x]\n", 1), "running", "stopped")
	if err := h.eng.Execute(context.Background(), "someone@x", "friend", "start"); !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want forbidden", err)
	}
	if err := h.eng.Execute(context.Background(), "", "mine", "stop"); !errors.Is(err, ErrForbidden) {
		t.Errorf("stop is not in mine's actions, err = %v", err)
	}
	if err := h.eng.Execute(context.Background(), "Friend@X", "friend", "start"); err != nil {
		t.Errorf("err = %v", err)
	}
	if v := h.eng.Snapshot("someone@x").VMs[1]; len(v.Actions) != 0 {
		t.Errorf("someone sees actions %v", v.Actions)
	}
}

func TestUnreachableHostRefusesSwitch(t *testing.T) {
	h := setup(t, baseConfig, "running", "stopped")
	h.pve.unreachable = true
	if err := h.eng.Execute(context.Background(), "", "friend", "start"); err == nil {
		t.Fatal("expected an error")
	}
	if v := h.eng.Snapshot("").VMs[0]; v.Status != StatusUnknown {
		t.Errorf("status = %s", v.Status)
	}
}

func TestSnapshotActions(t *testing.T) {
	h := setup(t, baseConfig, "running", "stopped")
	v := h.eng.Snapshot("")
	if got := strings.Join(v.VMs[0].Actions, ","); got != "shutdown" {
		t.Errorf("mine actions = %s", got)
	}
	if got := strings.Join(v.VMs[1].Actions, ","); got != "start" || len(v.VMs[1].Displaces) != 1 {
		t.Errorf("friend actions = %s, displaces = %v", got, v.VMs[1].Displaces)
	}
}
