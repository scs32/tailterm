package server

import (
	"net/http"
	"strconv"

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

// Owner helper continuity routes (docs/session-handoff.md, "The continuity
// record on the hub"). Reads need an identity; saves are writes. The Discord
// bridge credential reaches none of them (they are not bridge routes).

func (s *Server) getOwnerHelperContinuity(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var rev int64
	if v := r.URL.Query().Get("revision"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid revision")
			return
		}
		rev = n
	}
	out, err := s.store.OwnerHelperContinuity(r.Context(), id, rev)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listOwnerHelperContinuityRevisions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	out, err := s.store.OwnerHelperContinuityRevisions(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": out})
}

// saveOwnerHelperContinuity reads a body large enough for a maximal record
// whose characters JSON escapes; the store enforces the record's own limit.
func (s *Server) saveOwnerHelperContinuity(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.SaveOwnerHelperContinuityRequest
	if !decodeLimited(w, r, &req, api.MaxOwnerHelperContinuityRequest) {
		return
	}
	out, err := s.store.SaveOwnerHelperContinuity(r.Context(), id, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}
