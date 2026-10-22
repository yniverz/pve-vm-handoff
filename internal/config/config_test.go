package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseDefaultsAndEnv(t *testing.T) {
	t.Setenv("PVE_SECRET", "s3cr3t")
	cfg, err := Parse([]byte(`
server:
  title: GPU
hosts:
  pve:
    url: https://pve:8006
    token_id: a@pve!b
    token_secret: ${PVE_SECRET}
    verify_tls: false
defaults:
  shutdown_timeout: 90
  force_stop_after_timeout: true
vms:
  b:
    host: pve
    vmid: 2
    shutdown_timeout: 5m
  a:
    host: pve
    vmid: 1
    name: ${NAME_A:-Alpha}
groups:
  g:
    members: [a, b]
    default: a
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hosts["pve"].TokenSecret != "s3cr3t" || cfg.Hosts["pve"].VerifyTLS {
		t.Errorf("host = %+v", cfg.Hosts["pve"])
	}
	if cfg.VMs["a"].ShutdownTimeout.D() != 90*time.Second || !cfg.VMs["a"].ForceStopAfterTimeout {
		t.Errorf("defaults not applied: %+v", cfg.VMs["a"])
	}
	if cfg.VMs["b"].ShutdownTimeout.D() != 5*time.Minute || !cfg.VMs["b"].Displaceable {
		t.Errorf("override not applied: %+v", cfg.VMs["b"])
	}
	if cfg.VMs["a"].Name != "Alpha" || cfg.VMs["b"].Name != "b" {
		t.Errorf("names = %q %q", cfg.VMs["a"].Name, cfg.VMs["b"].Name)
	}
	if strings.Join(cfg.VMOrder, ",") != "b,a" {
		t.Errorf("order = %v", cfg.VMOrder)
	}
	if g := cfg.Groups["g"]; g.ReturnToDefault != ReturnAfterHandoff || g.ReturnDelay.D() != time.Minute {
		t.Errorf("group = %+v", g)
	}
	if cfg.Server.Listen != ":8080" || cfg.Server.Title != "GPU" {
		t.Errorf("server = %+v", cfg.Server)
	}
}

func TestValidation(t *testing.T) {
	_, err := Parse([]byte(`
hosts:
  pve:
    url: pve:8006
    token_id: nobang
vms:
  a: {host: pve, vmid: 1, actions: [reboot]}
  b: {host: other, vmid: 1}
groups:
  g:
    members: [a, c]
    default: b
    return_to_default: sometimes
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"url must start", "token_id", "token_secret", "unknown action", "not defined under hosts", `"c" is not defined`, "not a member", "return_to_default"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestMissingEnv(t *testing.T) {
	_, err := Parse([]byte("hosts:\n  p:\n    token_secret: ${DEFINITELY_UNSET_VAR}\n"))
	if err == nil || !strings.Contains(err.Error(), "DEFINITELY_UNSET_VAR") {
		t.Fatalf("err = %v", err)
	}
}

func TestConflictsAcrossGroups(t *testing.T) {
	cfg, err := Parse([]byte(`
hosts:
  p: {url: https://p, token_id: a@pve!b, token_secret: x}
vms:
  a: {host: p, vmid: 1}
  b: {host: p, vmid: 2}
  c: {host: p, vmid: 3}
groups:
  gpu: {members: [a, b]}
  nvme: {members: [b, c]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Conflicts("b"), ","); got != "a,c" {
		t.Errorf("conflicts(b) = %s", got)
	}
	if got := strings.Join(cfg.Conflicts("a"), ","); got != "b" {
		t.Errorf("conflicts(a) = %s", got)
	}
}
