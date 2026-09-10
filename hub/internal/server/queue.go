package server

import (
	"net/http"
	"strconv"

	"github.com/scs32/tailterm/hub/internal/api"
)

func queueLimit(r *http.Request) (int, bool) {
	limit := api.DefaultQueuePage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > api.MaxQueuePage {
			return 0, false
		}
		limit = value
	}
	return limit, true
}

func (s *Server) listQueue(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	limit, ok := queueLimit(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid Queue limit")
		return
	}
	includeTerminal := r.URL.Query().Get("includeTerminal") == "1"
	list, err := s.store.ListQueue(r.Context(), task, r.URL.Query().Get("cursor"), limit, includeTerminal)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getQueueEntry(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	entry, err := s.store.GetQueueEntry(r.Context(), task, r.PathValue("qid"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) listQueueHistory(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	limit, ok := queueLimit(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid Queue limit")
		return
	}
	page, err := s.store.ListQueueHistory(r.Context(), task, r.PathValue("qid"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) listQueueChanges(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	limit, ok := queueLimit(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid Queue limit")
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil || after < 0 {
		writeError(w, http.StatusBadRequest, "invalid Queue checkpoint")
		return
	}
	cutoff := int64(0)
	if raw := r.URL.Query().Get("cutoff"); raw != "" {
		cutoff, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || cutoff < 1 {
			writeError(w, http.StatusBadRequest, "invalid Queue cutoff")
			return
		}
	}
	page, err := s.store.ListQueueChanges(r.Context(), task, after, cutoff, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) queueAction(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.QueueActionRequest
	if !decode(w, r, &req) {
		return
	}
	result, err := s.store.QueueAction(r.Context(), task, r.PathValue("qid"), req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusCreated
	if result.Replay {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

func (s *Server) getQueueReceipt(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.caller(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	result, err := s.store.GetQueueReceipt(r.Context(), task, r.PathValue("requestID"), r.URL.Query().Get("agentId"), caller)
	if err != nil {
		fail(w, err)
		return
	}
	result.Replay = true
	writeJSON(w, http.StatusOK, result)
}
