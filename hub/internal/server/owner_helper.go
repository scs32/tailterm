package server

import (
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

// registerOwnerHelper binds the project's owner helper to the owner's session
// (docs/owner-helper.md). Workspace token only; it replays by request ID.
func (s *Server) registerOwnerHelper(w http.ResponseWriter, r *http.Request) {
	var req api.RegisterOwnerHelperRequest
	s.ownerRoute(w, r, &req, func(c api.Caller, id string) (api.OwnerActionResult, error) {
		return s.store.RegisterOwnerHelper(r.Context(), id, req, c)
	})
}

// getOwnerHelperRegistration returns the receipt of the registration that
// started one owner-helper run. Read-only; any identified caller may read it,
// and the Discord bridge credential cannot (it is not a bridge route).
func (s *Server) getOwnerHelperRegistration(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	run := r.PathValue("run")
	if !api.ValidOwnerHelperRun(run) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	list, err := s.store.ListOwnerHelperRegistrations(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	for _, reg := range list {
		if reg.RunID == run {
			writeJSON(w, http.StatusOK, reg)
			return
		}
	}
	writeError(w, http.StatusNotFound, "no owner-helper registration started run "+run+" in this project")
}
