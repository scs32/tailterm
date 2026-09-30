package server

import (
	"net/http"
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
