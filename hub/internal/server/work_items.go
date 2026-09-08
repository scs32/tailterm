package server

import (
	"net/http"

	"github.com/scs32/tailterm/hub/internal/api"
)

func workItemID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("wid")
	if !api.ValidID(id, "wi") {
		writeError(w, http.StatusNotFound, "not found")
		return "", false
	}
	return id, true
}

func workItemFilters(r *http.Request) (string, string, int64, int) {
	return r.URL.Query().Get("kind"), r.URL.Query().Get("status"), queryInt(r, "after", 0), int(queryInt(r, "limit", 50))
}

func (s *Server) listAllWorkItems(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task := r.URL.Query().Get("taskId")
	if task != "" && !api.ValidID(task, "tsk") {
		writeError(w, http.StatusBadRequest, "invalid task id")
		return
	}
	kind, status, after, limit := workItemFilters(r)
	items, err := s.store.ListWorkItems(r.Context(), task, kind, status, after, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) listTaskWorkItems(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	kind, status, after, limit := workItemFilters(r)
	items, err := s.store.ListWorkItems(r.Context(), task, kind, status, after, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) createWorkItem(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.CreateWorkItemRequest
	if !decode(w, r, &req) {
		return
	}
	item, err := s.store.CreateWorkItem(r.Context(), task, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) getWorkItem(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	itemID, ok := workItemID(w, r)
	if !ok {
		return
	}
	item, err := s.store.GetWorkItem(r.Context(), task, itemID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) updateWorkItem(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	itemID, ok := workItemID(w, r)
	if !ok {
		return
	}
	var req api.UpdateWorkItemRequest
	if !decode(w, r, &req) {
		return
	}
	item, err := s.store.UpdateWorkItem(r.Context(), task, itemID, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) dispatchWorkItem(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	itemID, ok := workItemID(w, r)
	if !ok {
		return
	}
	var req api.DispatchWorkItemRequest
	if !decode(w, r, &req) {
		return
	}
	result, err := s.store.DispatchWorkItem(r.Context(), task, itemID, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}
