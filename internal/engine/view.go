package engine

import (
	"slices"
	"time"

	"github.com/yniverz/pve-vm-handoff/internal/config"
)

type View struct {
	Title       string      `json:"title"`
	User        string      `json:"user"`
	Busy        string      `json:"busy"`
	LastRefresh time.Time   `json:"last_refresh"`
	Groups      []GroupView `json:"groups"`
	VMs         []VMView    `json:"vms"`
	Events      []Event     `json:"events"` // newest first
}

type GroupView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Members     []string `json:"members"`
	Default     string   `json:"default"`
	ReturnMode  string   `json:"return_mode"`
	ReturnDelay float64  `json:"return_delay"`
	Paused      bool     `json:"paused"`
	CanPause    bool     `json:"can_pause"`
	// Seconds until the default VM is started again, when the countdown is running.
	ReturnIn *float64 `json:"return_in"`
	Occupant string   `json:"occupant"` // running member, if any
}

type VMView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Host        string   `json:"host"`
	Node        string   `json:"node"`
	VMID        int      `json:"vmid"`
	Type        string   `json:"type"`
	Status      string   `json:"status"`
	Uptime      int64    `json:"uptime"`
	Error       string   `json:"error"`
	Actions     []string `json:"actions"`   // what this user may do right now
	Displaces   []string `json:"displaces"` // running VMs a start would shut down
	Blocked     string   `json:"blocked"`   // why a start is impossible
}

// Snapshot returns the current state as seen by user.
func (e *Engine) Snapshot(user string) View {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.opts.Now()

	v := View{Title: e.cfg.Server.Title, User: user, Busy: e.busy, LastRefresh: e.lastRefresh}
	for _, gid := range e.cfg.GroupOrder {
		g := e.cfg.Groups[gid]
		gv := GroupView{
			ID: gid, Name: g.Name, Description: g.Description, Members: g.Members, Default: g.Default,
			ReturnMode: g.ReturnToDefault, ReturnDelay: g.ReturnDelay.D().Seconds(),
			Paused:   e.paused[gid],
			CanPause: g.Default != "" && g.ReturnToDefault != config.ReturnNever && config.Allowed(g.AllowedUsers, user),
		}
		if since, ok := e.idleSince[gid]; ok {
			left := max(0, (g.ReturnDelay.D() - now.Sub(since)).Seconds())
			gv.ReturnIn = &left
		}
		for _, m := range g.Members {
			if e.states[m].occupied() && e.states[m].Status != StatusUnknown && e.states[m].Status != StatusMissing {
				gv.Occupant = m
				break
			}
		}
		v.Groups = append(v.Groups, gv)
	}

	for _, id := range e.cfg.VMOrder {
		vm := e.cfg.VMs[id]
		st := e.states[id]
		vv := VMView{
			ID: id, Name: vm.Name, Description: vm.Description, Host: vm.Host, Node: st.Node, VMID: vm.VMID,
			Type: st.Type, Status: st.Status, Uptime: st.Uptime, Error: st.Error, Actions: []string{}, Displaces: []string{},
		}
		if config.Allowed(vm.AllowedUsers, user) {
			for _, c := range e.cfg.Conflicts(id) {
				cs := e.states[c]
				if cs.Status == StatusUnknown {
					vv.Blocked = "status of " + e.cfg.VMs[c].Name + " is unknown"
				} else if cs.occupied() && cs.Status != StatusMissing {
					if !e.cfg.VMs[c].Displaceable {
						vv.Blocked = e.cfg.VMs[c].Name + " is running and may not be shut down by others"
					}
					vv.Displaces = append(vv.Displaces, e.cfg.VMs[c].Name)
				}
			}
			for _, a := range vm.Actions {
				switch {
				case a == config.ActionStart && st.Status == StatusStopped && vv.Blocked == "":
				case (a == config.ActionShutdown || a == config.ActionStop) && st.Status == StatusRunning:
				default:
					continue
				}
				vv.Actions = append(vv.Actions, a)
			}
		}
		v.VMs = append(v.VMs, vv)
	}

	v.Events = slices.Clone(e.events)
	slices.Reverse(v.Events)
	return v
}
