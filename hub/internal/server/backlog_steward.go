package server

import (
	"net/http"
	"strconv"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Backlog steward routes (docs/backlog-steward.md).

func (s *Server) getBacklogSteward(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	out, err := s.store.BacklogStewardStatus(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getBacklogSummary(w http.ResponseWriter, r *http.Request) {
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
	out, err := s.store.BacklogSummary(r.Context(), id, rev)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listBacklogSummaryRevisions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	out, err := s.store.BacklogSummaryRevisions(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": out})
}

// saveBacklogSummary reads a body large enough for a maximal summary whose
// characters JSON escapes; the store enforces the summary's own limit.
func (s *Server) saveBacklogSummary(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.SaveBacklogSummaryRequest
	if !decodeLimited(w, r, &req, api.MaxBacklogSummaryRequest) {
		return
	}
	out, err := s.store.SaveBacklogSummary(r.Context(), id, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) getStewardRotationPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	p, err := s.store.StewardRotationPolicy(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) setStewardRotationPolicy(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.StewardRotationPolicyRequest
	if !decode(w, r, &req) {
		return
	}
	p, err := s.store.SetStewardRotationPolicy(r.Context(), id, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) stewardRotationAction(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.StewardRotationRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.StewardRotationAction(r.Context(), id, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listStewardRotations(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	out, err := s.store.ListStewardRotations(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rotations": out})
}

func (s *Server) getStewardRotation(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	out, err := s.store.GetStewardRotation(r.Context(), id, r.PathValue("rid"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) stewardRotationsDue(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	q := r.URL.Query()
	out, err := s.store.StewardRotationsDue(r.Context(), q.Get("host"), q.Get("templateDigest"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
