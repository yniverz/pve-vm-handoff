// Package config loads and validates the YAML configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Actions a VM can expose in the UI.
const (
	ActionStart    = "start"
	ActionShutdown = "shutdown"
	ActionStop     = "stop"
)

// Return-to-default modes of a group.
const (
	ReturnAfterHandoff = "after_handoff"
	ReturnAlways       = "always"
	ReturnNever        = "never"
)

// Event kinds, usable in notification filters.
var EventKinds = []string{"start", "shutdown", "stop", "switch", "auto_return", "pause", "error"}

type Config struct {
	Server        Server
	Hosts         map[string]*Host
	VMs           map[string]*VM
	Groups        map[string]*Group
	Notifications []*Notification

	// YAML order, so the UI shows things the way they were written.
	VMOrder    []string
	GroupOrder []string
}

type Server struct {
	Listen       string   `yaml:"listen"`
	Title        string   `yaml:"title"`
	PollInterval Duration `yaml:"poll_interval"`
	// Header set by the auth proxy that carries the user name or email.
	// Empty means every visitor is anonymous and allowed_users lists cannot match.
	UserHeader string `yaml:"user_header"`
	// File that keeps pause flags and the last occupant of each group across restarts.
	StateFile string `yaml:"state_file"`
}

type Host struct {
	URL         string   `yaml:"url"`
	TokenID     string   `yaml:"token_id"`
	TokenSecret string   `yaml:"token_secret"`
	VerifyTLS   bool     `yaml:"verify_tls"`
	Timeout     Duration `yaml:"timeout"`
}

type VM struct {
	ID string `yaml:"-"`

	Host        string `yaml:"host"`
	VMID        int    `yaml:"vmid"`
	Node        string `yaml:"node"` // optional, discovered from the cluster when empty
	Type        string `yaml:"type"` // qemu or lxc, discovered when empty
	Name        string `yaml:"name"`
	Description string `yaml:"description"`

	Actions      []string `yaml:"actions"`
	AllowedUsers []string `yaml:"allowed_users"`

	ShutdownTimeout Duration `yaml:"shutdown_timeout"`
	// Hard-stop the VM when a graceful shutdown did not finish within shutdown_timeout.
	ForceStopAfterTimeout bool     `yaml:"force_stop_after_timeout"`
	StartTimeout          Duration `yaml:"start_timeout"`
	// Whether starting another member of a shared group may shut this VM down.
	Displaceable bool `yaml:"displaceable"`
}

type Group struct {
	ID string `yaml:"-"`

	Name            string   `yaml:"name"`
	Description     string   `yaml:"description"`
	Members         []string `yaml:"members"`
	Default         string   `yaml:"default"`
	ReturnToDefault string   `yaml:"return_to_default"`
	ReturnDelay     Duration `yaml:"return_delay"`
	// Who may pause/resume the automatic return. Empty means everyone.
	AllowedUsers []string `yaml:"allowed_users"`
}

type Notification struct {
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Format  string            `yaml:"format"` // json or text
	Headers map[string]string `yaml:"headers"`
	Events  []string          `yaml:"events"` // empty means all
}

// Duration accepts Go duration strings ("90s", "3m") or plain numbers of seconds.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	s := strings.TrimSpace(n.Value)
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*d = Duration(f * float64(time.Second))
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, s)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// Load reads the config from CONFIG_YAML (inline, handy for Portainer's stack editor)
// or from the given file path.
func Load(path string) (*Config, error) {
	data := []byte(os.Getenv("CONFIG_YAML"))
	if len(strings.TrimSpace(string(data))) == 0 {
		var err error
		if data, err = os.ReadFile(path); err != nil {
			return nil, fmt.Errorf("read config: %w (set CONFIG_YAML or mount a config file)", err)
		}
	}
	return Parse(data)
}

type rawConfig struct {
	Server        Server          `yaml:"server"`
	Hosts         yaml.Node       `yaml:"hosts"`
	Defaults      yaml.Node       `yaml:"defaults"`
	VMs           yaml.Node       `yaml:"vms"`
	Groups        yaml.Node       `yaml:"groups"`
	Notifications []*Notification `yaml:"notifications"`
}

func Parse(data []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := expandEnv(&doc); err != nil {
		return nil, err
	}
	raw := rawConfig{Server: Server{Listen: ":8080", Title: "VM Handoff", PollInterval: Duration(5 * time.Second)}}
	if err := doc.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg := &Config{
		Server:        raw.Server,
		Hosts:         map[string]*Host{},
		VMs:           map[string]*VM{},
		Groups:        map[string]*Group{},
		Notifications: raw.Notifications,
	}

	if err := eachEntry(&raw.Hosts, "hosts", func(id string, n *yaml.Node) error {
		h := &Host{VerifyTLS: true, Timeout: Duration(15 * time.Second)}
		if err := n.Decode(h); err != nil {
			return err
		}
		cfg.Hosts[id] = h
		return nil
	}); err != nil {
		return nil, err
	}

	if err := eachEntry(&raw.VMs, "vms", func(id string, n *yaml.Node) error {
		vm := &VM{
			ID:              id,
			Actions:         []string{ActionStart, ActionShutdown},
			ShutdownTimeout: Duration(3 * time.Minute),
			StartTimeout:    Duration(2 * time.Minute),
			Displaceable:    true,
		}
		if raw.Defaults.Kind != 0 {
			if err := raw.Defaults.Decode(vm); err != nil {
				return fmt.Errorf("defaults: %w", err)
			}
		}
		if err := n.Decode(vm); err != nil {
			return err
		}
		cfg.VMs[id] = vm
		cfg.VMOrder = append(cfg.VMOrder, id)
		return nil
	}); err != nil {
		return nil, err
	}

	if err := eachEntry(&raw.Groups, "groups", func(id string, n *yaml.Node) error {
		g := &Group{ID: id, ReturnToDefault: ReturnAfterHandoff, ReturnDelay: Duration(time.Minute)}
		if err := n.Decode(g); err != nil {
			return err
		}
		cfg.Groups[id] = g
		cfg.GroupOrder = append(cfg.GroupOrder, id)
		return nil
	}); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func eachEntry(n *yaml.Node, section string, fn func(id string, n *yaml.Node) error) error {
	if n.Kind == 0 {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: expected a mapping of id -> settings (line %d)", section, n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		id := n.Content[i].Value
		if err := fn(id, n.Content[i+1]); err != nil {
			return fmt.Errorf("%s.%s: %w", section, id, err)
		}
	}
	return nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// expandEnv replaces ${VAR} and ${VAR:-default} in scalar values, so secrets can stay in the environment.
func expandEnv(n *yaml.Node) error {
	var missing []string
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "${") {
			n.Value = envRef.ReplaceAllStringFunc(n.Value, func(m string) string {
				p := envRef.FindStringSubmatch(m)
				if v, ok := os.LookupEnv(p[1]); ok {
					return v
				}
				if p[2] != "" {
					return p[3]
				}
				missing = append(missing, p[1])
				return ""
			})
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(n)
	if len(missing) > 0 {
		return fmt.Errorf("config references unset environment variables: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Server.PollInterval.D() < time.Second {
		add("server.poll_interval must be at least 1s")
	}
	if len(c.Hosts) == 0 {
		add("hosts: at least one Proxmox host is required")
	}
	for id, h := range c.Hosts {
		if !strings.HasPrefix(h.URL, "http://") && !strings.HasPrefix(h.URL, "https://") {
			add("hosts.%s.url must start with https:// (or http://)", id)
		}
		if !strings.Contains(h.TokenID, "!") {
			add("hosts.%s.token_id must look like user@realm!tokenname", id)
		}
		if h.TokenSecret == "" {
			add("hosts.%s.token_secret is empty", id)
		}
	}

	type hostVM struct {
		host string
		vmid int
	}
	seen := map[hostVM]string{}
	for _, id := range c.VMOrder {
		vm := c.VMs[id]
		if _, ok := c.Hosts[vm.Host]; !ok {
			add("vms.%s.host %q is not defined under hosts", id, vm.Host)
		}
		if vm.VMID <= 0 {
			add("vms.%s.vmid is required", id)
		}
		if other, dup := seen[hostVM{vm.Host, vm.VMID}]; dup {
			add("vms.%s and vms.%s point at the same VM", other, id)
		}
		seen[hostVM{vm.Host, vm.VMID}] = id
		if vm.Type != "" && vm.Type != "qemu" && vm.Type != "lxc" {
			add("vms.%s.type must be qemu or lxc", id)
		}
		for _, a := range vm.Actions {
			if a != ActionStart && a != ActionShutdown && a != ActionStop {
				add("vms.%s.actions: unknown action %q (start, shutdown, stop)", id, a)
			}
		}
		if vm.Name == "" {
			vm.Name = id
		}
	}

	for _, id := range c.GroupOrder {
		g := c.Groups[id]
		if g.Name == "" {
			g.Name = id
		}
		if len(g.Members) < 2 {
			add("groups.%s needs at least two members", id)
		}
		for _, m := range g.Members {
			if _, ok := c.VMs[m]; !ok {
				add("groups.%s.members: %q is not defined under vms", id, m)
			}
		}
		if g.Default != "" && !slices.Contains(g.Members, g.Default) {
			add("groups.%s.default %q is not a member of the group", id, g.Default)
		}
		switch g.ReturnToDefault {
		case ReturnAfterHandoff, ReturnAlways, ReturnNever:
		default:
			add("groups.%s.return_to_default must be after_handoff, always or never", id)
		}
	}

	for i, n := range c.Notifications {
		if n.URL == "" {
			add("notifications[%d].url is required", i)
		}
		if n.Method == "" {
			n.Method = "POST"
		}
		if n.Format == "" {
			n.Format = "json"
		}
		if n.Format != "json" && n.Format != "text" {
			add("notifications[%d].format must be json or text", i)
		}
		for _, e := range n.Events {
			if !slices.Contains(EventKinds, e) {
				add("notifications[%d].events: unknown event %q (%s)", i, e, strings.Join(EventKinds, ", "))
			}
		}
	}
	return errors.Join(errs...)
}

// Allowed reports whether user may act given an allow list; an empty list allows everyone.
func Allowed(list []string, user string) bool {
	if len(list) == 0 {
		return true
	}
	for _, u := range list {
		if strings.EqualFold(u, user) {
			return true
		}
	}
	return false
}

// GroupsOf returns the groups a VM belongs to.
func (c *Config) GroupsOf(vmID string) []*Group {
	var out []*Group
	for _, id := range c.GroupOrder {
		if slices.Contains(c.Groups[id].Members, vmID) {
			out = append(out, c.Groups[id])
		}
	}
	return out
}

// Conflicts returns every VM that shares a group with vmID, in config order.
func (c *Config) Conflicts(vmID string) []string {
	var out []string
	for _, g := range c.GroupsOf(vmID) {
		for _, m := range g.Members {
			if m != vmID && !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	return out
}
