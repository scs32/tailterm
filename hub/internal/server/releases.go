package server

import (
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http"
)

func (s *Server) listReleases(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	out, err := s.store.Releases(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) releaseAction(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	var req api.ReleaseRequest
	if !decodeScope(w, r, &req) {
		return
	}
	out, err := s.store.ReleaseAction(r.Context(), r.PathValue("id"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}
