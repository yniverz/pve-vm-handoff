// Package proxmox is a minimal Proxmox VE API client covering what the handoff needs:
// listing guests and starting/stopping them.
package proxmox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yniverz/pve-vm-handoff/internal/config"
)

// Resource is one guest from /cluster/resources.
type Resource struct {
	VMID   int    `json:"vmid"`
	Node   string `json:"node"`
	Type   string `json:"type"` // qemu or lxc
	Name   string `json:"name"`
	Status string `json:"status"` // running, stopped, ...
	Uptime int64  `json:"uptime"`
}

// TaskStatus is the state of a Proxmox task (UPID).
type TaskStatus struct {
	Status     string `json:"status"` // running or stopped
	ExitStatus string `json:"exitstatus"`
}

// API is what the engine needs from a host; tests swap in a fake.
type API interface {
	Resources(ctx context.Context) ([]Resource, error)
	Action(ctx context.Context, node, typ string, vmid int, action string, params url.Values) (upid string, err error)
	Task(ctx context.Context, node, upid string) (TaskStatus, error)
}

type Client struct {
	name string
	base string
	auth string
	http *http.Client
}

func New(name string, h *config.Host) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if !h.VerifyTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &Client{
		name: name,
		base: strings.TrimRight(h.URL, "/") + "/api2/json",
		auth: "PVEAPIToken=" + h.TokenID + "=" + h.TokenSecret,
		http: &http.Client{Transport: tr, Timeout: h.Timeout.D()},
	}
}

func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.auth)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", c.name, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		// Proxmox puts the useful message in the status line.
		msg := strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprint(resp.StatusCode)))
		if extra := strings.TrimSpace(string(raw)); extra != "" && extra != `{"data":null}` {
			msg += ": " + truncate(extra, 300)
		}
		return fmt.Errorf("%s: %s %s: %d %s", c.name, method, path, resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	env := struct {
		Data json.RawMessage `json:"data"`
	}{}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: decode %s: %w", c.name, path, err)
	}
	return json.Unmarshal(env.Data, out)
}

func (c *Client) Resources(ctx context.Context) ([]Resource, error) {
	var out []Resource
	err := c.do(ctx, http.MethodGet, "/cluster/resources?type=vm", nil, &out)
	return out, err
}

func (c *Client) Action(ctx context.Context, node, typ string, vmid int, action string, params url.Values) (string, error) {
	if params == nil {
		params = url.Values{}
	}
	var upid string
	path := fmt.Sprintf("/nodes/%s/%s/%d/status/%s", url.PathEscape(node), typ, vmid, action)
	err := c.do(ctx, http.MethodPost, path, params, &upid)
	return upid, err
}

func (c *Client) Task(ctx context.Context, node, upid string) (TaskStatus, error) {
	var out TaskStatus
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/tasks/%s/status", url.PathEscape(node), url.PathEscape(upid)), nil, &out)
	return out, err
}

// WaitTask polls a task until it finishes or ctx ends. A nil error means the task
// ended with OK (or only warnings), or that its status can't be read; callers
// confirm the guest status afterwards either way.
func WaitTask(ctx context.Context, api API, node, upid string, every time.Duration) error {
	if upid == "" {
		return nil
	}
	for {
		st, err := api.Task(ctx, node, upid)
		if err != nil {
			return nil
		}
		if st.Status == "stopped" {
			if st.ExitStatus == "OK" || strings.HasPrefix(st.ExitStatus, "WARNINGS") {
				return nil
			}
			return fmt.Errorf("task failed: %s", st.ExitStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
