package server

import (
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

func (s *Server) closeItemTeam(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	taskID, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.TeamCloseRequest
	if !decode(w, r, &req) {
		return
	}
	result, err := s.store.CloseItemTeam(r.Context(), taskID, req, by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
