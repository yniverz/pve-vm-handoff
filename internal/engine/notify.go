package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/yniverz/pve-vm-handoff/internal/config"
)

// Notify sends events to the configured webhooks until ctx ends.
// "json" posts the event object, "text" posts the message as plain text (works with ntfy, Gotify via headers, etc.).
func Notify(ctx context.Context, events <-chan Event, targets []*config.Notification, log *slog.Logger) {
	client := &http.Client{Timeout: 10 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			for _, t := range targets {
				if len(t.Events) > 0 && !slices.Contains(t.Events, ev.Kind) {
					continue
				}
				if err := send(ctx, client, t, ev); err != nil {
					log.Warn("notification failed", "url", t.URL, "err", err)
				}
			}
		}
	}
}

func send(ctx context.Context, client *http.Client, t *config.Notification, ev Event) error {
	var body []byte
	contentType := "text/plain; charset=utf-8"
	if t.Format == "json" {
		body, _ = json.Marshal(ev)
		contentType = "application/json"
	} else {
		body = []byte(ev.Message)
		if ev.User != "" {
			body = append(body, " (by "+ev.User+")"...)
		}
	}
	req, err := http.NewRequestWithContext(ctx, t.Method, t.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &httpError{resp.Status}
	}
	return nil
}

type httpError struct{ status string }

func (e *httpError) Error() string { return "unexpected response " + e.status }
