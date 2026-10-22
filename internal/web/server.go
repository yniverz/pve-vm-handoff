// Package web serves the single-page UI and its JSON API.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/yniverz/pve-vm-handoff/internal/engine"
)

//go:embed static
var static embed.FS

type Server struct {
	eng        *engine.Engine
	userHeader string
}

func New(eng *engine.Engine, userHeader string) http.Handler {
	s := &Server{eng: eng, userHeader: userHeader}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/vms/{id}/{action}", s.action)
	mux.HandleFunc("POST /api/groups/{id}/pause", s.pause)
	return securityHeaders(mux)
}

func (s *Server) user(r *http.Request) string {
	if s.userHeader == "" {
		return ""
	}
	return strings.TrimSpace(r.Header.Get(s.userHeader))
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	data, _ := static.ReadFile("static/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.eng.Snapshot(s.user(r)))
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeErr(w, http.StatusForbidden, errors.New("cross-site request rejected"))
		return
	}
	err := s.eng.Start(s.user(r), r.PathValue("id"), r.PathValue("action"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeErr(w, http.StatusForbidden, errors.New("cross-site request rejected"))
		return
	}
	var body struct {
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.eng.SetPaused(r.PathValue("id"), body.Paused, s.user(r)); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"paused": body.Paused})
}

// sameOrigin blocks cross-site form posts: the UI always sends this header, which a
// plain HTML form cannot set and a foreign page cannot add without a CORS preflight.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	return r.Header.Get("X-Requested-With") == "pve-vm-handoff"
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, engine.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, engine.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, engine.ErrBusy):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}
