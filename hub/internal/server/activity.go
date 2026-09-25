package server

import (
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

func (s *Server) reportAgentActivity(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	a, ok := s.agentInTask(w, r)
	if !ok {
		return
	}
	var req api.ActivityReport
	if !decode(w, r, &req) {
		return
	}
	activity, err := s.store.ReportActivity(r.Context(), a.TaskID, a.ID, req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, activity)
}
