package server

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/scs32/tailterm/hub/internal/store"
)

func (s *Server) profileRoutes() {
	s.mux.HandleFunc("GET /v1/profiles", func(w http.ResponseWriter, r *http.Request) {
		id, err := s.store.ProfileServiceID(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"service": "tailterm-profiles", "version": 1, "instanceId": id})
	})
	s.mux.HandleFunc("POST /v1/profiles/{username}", s.createProfile)
	s.mux.HandleFunc("GET /v1/profiles/{username}", s.getProfile)
	s.mux.HandleFunc("PUT /v1/profiles/{username}", s.putProfile)
}
func profileCredential(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Profile ") {
		return ""
	}
	return strings.TrimPrefix(value, "Profile ")
}
func profileFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrProfileAuth):
		writeError(w, 403, "Username or passphrase did not match a saved profile.")
	case errors.Is(err, store.ErrProfileConflict):
		writeError(w, 409, "This profile already exists or has changed on another device.")
	default:
		fail(w, err)
	}
}
func (s *Server) profileLimit(w http.ResponseWriter, r *http.Request) bool {
	// Rate-limit reads too; usernames and addresses come from an untrusted client.
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		address = r.RemoteAddr
	}
	if !s.limiter.allow("profile:" + address) {
		writeError(w, 429, "Try again shortly.")
		return false
	}
	return true
}
func profileBody(w http.ResponseWriter, r *http.Request) (json.RawMessage, int64, bool) {
	var body struct {
		Envelope json.RawMessage `json:"envelope"`
		Revision int64           `json:"revision"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, store.MaxProfileBytes+1024))
	if err != nil {
		writeError(w, 413, "Profile is too large.")
		return nil, 0, false
	}
	if json.Unmarshal(raw, &body) != nil {
		writeError(w, 400, "Invalid profile.")
		return nil, 0, false
	}
	return body.Envelope, body.Revision, true
}
func (s *Server) createProfile(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	envelope, _, ok := profileBody(w, r)
	if !ok {
		return
	}
	p, err := s.store.CreateProfile(r.Context(), r.PathValue("username"), r.Header.Get("X-Profile-Key"), envelope)
	if err != nil {
		profileFailure(w, err)
		return
	}
	writeJSON(w, 201, p)
}
func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	if !s.profileLimit(w, r) {
		return
	}
	p, err := s.store.ReadProfile(r.Context(), r.PathValue("username"), profileCredential(r))
	if err != nil {
		profileFailure(w, err)
		return
	}
	writeJSON(w, 200, p)
}
func (s *Server) putProfile(w http.ResponseWriter, r *http.Request) {
	if !s.profileLimit(w, r) {
		return
	}
	envelope, rev, ok := profileBody(w, r)
	if !ok {
		return
	}
	p, err := s.store.WriteProfile(r.Context(), r.PathValue("username"), profileCredential(r), rev, envelope)
	if err != nil {
		profileFailure(w, err)
		return
	}
	writeJSON(w, 200, p)
}
