package server

import (
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

// createIntervention records an owner intervention. The store refuses a
// request carrying an agent identity with 403.
func (s *Server) createIntervention(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	var request api.CreateInterventionRequest
	if !decode(w, r, &request) {
		return
	}
	message, err := s.store.CreateIntervention(r.Context(), task, request, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, message)
}

func (s *Server) listInterventions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	after, ok := decisionQueryInt(r, "after", 0)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid after cursor")
		return
	}
	limit, ok := decisionQueryInt(r, "limit", api.MaxInterventionPage)
	if !ok || limit < 1 || limit > api.MaxInterventionPage {
		writeError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	list, err := s.store.ListInterventions(r.Context(), task, after, int(limit), r.URL.Query().Get("tz"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
