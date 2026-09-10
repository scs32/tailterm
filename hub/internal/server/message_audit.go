package server

import (
	"net/http"
	"strconv"

	"github.com/scs32/tailterm/hub/internal/api"
)

func messageAuditSeq(w http.ResponseWriter, r *http.Request) (int64, bool) {
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil || seq < 1 {
		writeError(w, http.StatusNotFound, "not found")
		return 0, false
	}
	return seq, true
}

func strictNonnegativeQuery(r *http.Request, key string, fallback int64) (int64, bool) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	return value, err == nil && value >= 0
}

func messageAuditPage(r *http.Request) (int, bool) {
	value, ok := strictNonnegativeQuery(r, "limit", api.DefaultMessageAuditPage)
	return int(value), ok && value >= 1 && value <= api.MaxMessageAuditPage
}

func (s *Server) getMessageAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	seq, ok := messageAuditSeq(w, r)
	if !ok {
		return
	}
	result, err := s.store.GetMessageAudit(r.Context(), task, seq)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listMessageAuditHistory(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	seq, ok := messageAuditSeq(w, r)
	if !ok {
		return
	}
	after, validAfter := strictNonnegativeQuery(r, "afterVersion", 0)
	limit, validLimit := messageAuditPage(r)
	if !validAfter || !validLimit {
		writeError(w, http.StatusBadRequest, "invalid audit history query")
		return
	}
	result, err := s.store.ListMessageAuditHistory(r.Context(), task, seq, after, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) correctMessageAudit(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	seq, ok := messageAuditSeq(w, r)
	if !ok {
		return
	}
	var req api.CorrectMessageAuditRequest
	if !decode(w, r, &req) {
		return
	}
	result, replay, err := s.store.CorrectMessageAudit(r.Context(), task, seq, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

func (s *Server) resolveMessageAudit(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	seq, ok := messageAuditSeq(w, r)
	if !ok {
		return
	}
	var req api.ResolveMessageAuditRequest
	if !decode(w, r, &req) {
		return
	}
	result, replay, err := s.store.ResolveMessageAudit(r.Context(), task, seq, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

func (s *Server) getMessageAuditReceipt(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.caller(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	result, err := s.store.GetMessageAuditReceipt(r.Context(), task, r.PathValue("requestID"), r.URL.Query().Get("operation"), r.URL.Query().Get("agentId"), caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listMessageAuditChanges(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	limit, valid := messageAuditPage(r)
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid audit change limit")
		return
	}
	result, err := s.store.ListMessageAuditChanges(r.Context(), task, r.URL.Query().Get("cursor"), r.URL.Query().Get("checkpoint"), limit, r.URL.Query().Get("kind"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) createMessageAuditAssociation(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.CreateMessageAuditAssociationRequest
	if !decode(w, r, &req) {
		return
	}
	result, replay, err := s.store.CreateMessageAuditAssociation(r.Context(), task, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusCreated
	if replay {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

func (s *Server) getMessageAuditAssociationReceipt(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.caller(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	result, err := s.store.GetMessageAuditAssociationReceipt(r.Context(), task, r.PathValue("requestID"), r.URL.Query().Get("agentId"), caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
