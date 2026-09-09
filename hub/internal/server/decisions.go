package server

import (
	"net/http"
	"strconv"

	"github.com/scs32/tailterm/hub/internal/api"
)

func (s *Server) createDecision(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	var request api.CreateDecisionRequest
	if !decode(w, r, &request) {
		return
	}
	message, err := s.store.CreateDecision(r.Context(), task, request, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, message)
}

func decisionQueryInt(r *http.Request, name string, fallback int64) (int64, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	return value, err == nil && value >= 0
}

func (s *Server) listDecisions(w http.ResponseWriter, r *http.Request) {
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
	limit, ok := decisionQueryInt(r, "limit", api.MaxDecisionPage)
	if !ok || limit < 1 || limit > api.MaxDecisionPage {
		writeError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	list, err := s.store.ListDecisions(r.Context(), task, after, int(limit))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) answerDecision(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq < 1 {
		writeError(w, http.StatusBadRequest, "invalid request sequence")
		return
	}
	var request api.AnswerDecisionRequest
	if !decode(w, r, &request) {
		return
	}
	message, err := s.store.AnswerDecision(r.Context(), task, seq, request, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, message)
}
