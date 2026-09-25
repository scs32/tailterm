package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The metadata-only endpoints reject item fields instead of silently dropping
// them, so callers cannot mistake a bookkeeping receipt for a scope edit.
func decodeScope(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, api.MaxBody))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid scope metadata request")
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid scope metadata request")
		return false
	}
	return true
}

func (s *Server) confirmWorkOrderScope(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	item, ok := workItemID(w, r)
	if !ok {
		return
	}
	var req api.ConfirmWorkOrderScopeRequest
	if !decodeScope(w, r, &req) {
		return
	}
	v, err := s.store.ConfirmWorkOrderScope(r.Context(), task, item, req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) getWorkOrderScopeConfirmation(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	item, ok := workItemID(w, r)
	if !ok {
		return
	}
	revision, err1 := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	order, err2 := strconv.ParseInt(r.URL.Query().Get("order"), 10, 64)
	if err1 != nil || err2 != nil || revision < 1 || order < 1 {
		fail(w, api.ErrInvalid)
		return
	}
	v, err := s.store.GetWorkOrderScopeConfirmation(r.Context(), task, item, revision, order)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) saveWorkOrderBookkeeping(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	item, ok := workItemID(w, r)
	if !ok {
		return
	}
	var req api.WorkOrderBookkeepingRequest
	if !decodeScope(w, r, &req) {
		return
	}
	v, err := s.store.SaveWorkOrderBookkeeping(r.Context(), task, item, req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) getWorkOrderBookkeepingReceipt(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	item, ok := workItemID(w, r)
	if !ok {
		return
	}
	v, err := s.store.GetWorkOrderBookkeepingReceipt(r.Context(), task, item, r.PathValue("requestID"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
