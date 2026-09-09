package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

func historyPage(r *http.Request) (int64, int, bool) {
	after, limit := int64(0), api.DefaultWorkItemHistoryPage
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		after = n
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > api.MaxWorkItemHistoryPage {
			return 0, 0, false
		}
		limit = n
	}
	return after, limit, true
}

func fitsHistoryBody(v any) bool {
	b, err := json.Marshal(v)
	return err == nil && len(b)+1 <= api.MaxWorkItemHistoryBytes
}

func boundRevisionList(list api.WorkItemRevisionList, limit int) (api.WorkItemRevisionList, bool) {
	hasMore := len(list.Revisions) > limit
	if hasMore {
		list.Revisions = list.Revisions[:limit]
	}
	for len(list.Revisions) > 0 && !fitsHistoryBody(list) {
		hasMore = true
		list.Revisions = list.Revisions[:len(list.Revisions)-1]
	}
	if len(list.Revisions) == 0 && !fitsHistoryBody(list) {
		return list, false
	}
	if hasMore && len(list.Revisions) > 0 {
		list.NextAfter = list.Revisions[len(list.Revisions)-1].Revision
	}
	return list, true
}

func boundGapList(list api.HistoryGapList, limit int) (api.HistoryGapList, bool) {
	hasMore := len(list.Gaps) > limit
	if hasMore {
		list.Gaps = list.Gaps[:limit]
	}
	for len(list.Gaps) > 0 && !fitsHistoryBody(list) {
		hasMore = true
		list.Gaps = list.Gaps[:len(list.Gaps)-1]
	}
	if len(list.Gaps) == 0 && !fitsHistoryBody(list) {
		return list, false
	}
	if hasMore && len(list.Gaps) > 0 {
		list.NextAfter = list.Gaps[len(list.Gaps)-1].Seq
	}
	return list, true
}

func boundMessageList(list api.WorkItemMessageList, limit int) (api.WorkItemMessageList, bool) {
	hasMore := len(list.Links) > limit
	if hasMore {
		list.Links = list.Links[:limit]
	}
	for len(list.Links) > 0 && !fitsHistoryBody(list) {
		hasMore = true
		list.Links = list.Links[:len(list.Links)-1]
	}
	if len(list.Links) == 0 && !fitsHistoryBody(list) {
		return list, false
	}
	if hasMore && len(list.Links) > 0 {
		list.NextAfter = list.Links[len(list.Links)-1].Message.Seq
	}
	return list, true
}

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

func (s *Server) listWorkItemRevisions(w http.ResponseWriter, r *http.Request) {
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
	after, limit, ok := historyPage(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid history cursor or limit")
		return
	}
	list, err := s.store.ListWorkItemRevisions(r.Context(), task, item, after, limit+1)
	if err != nil {
		fail(w, err)
		return
	}
	list, ok = boundRevisionList(list, limit)
	if !ok {
		writeError(w, http.StatusInternalServerError, "history page exceeds response limit")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getWorkItemRevision(w http.ResponseWriter, r *http.Request) {
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
	revision, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
	if err != nil || revision < 1 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	result, err := s.store.GetWorkItemRevision(r.Context(), task, item, revision)
	if err != nil {
		var gap *store.WorkItemHistoryGapError
		if errors.As(err, &gap) {
			writeJSON(w, http.StatusConflict, api.WorkItemHistoryGapResponse{Error: gap.Error(), Gap: gap.Gap})
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listWorkItemHistoryGaps(w http.ResponseWriter, r *http.Request) {
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
	after, limit, ok := historyPage(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid history cursor or limit")
		return
	}
	list, err := s.store.ListWorkItemHistoryGaps(r.Context(), task, item, after, limit+1)
	if err != nil {
		fail(w, err)
		return
	}
	list, ok = boundGapList(list, limit)
	if !ok {
		writeError(w, http.StatusInternalServerError, "history page exceeds response limit")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) listWorkItemMessages(w http.ResponseWriter, r *http.Request) {
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
	after, limit, ok := historyPage(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid history cursor or limit")
		return
	}
	revision := int64(0)
	if raw := r.URL.Query().Get("revision"); raw != "" {
		var err error
		revision, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || revision < 1 {
			writeError(w, http.StatusBadRequest, "invalid revision")
			return
		}
	}
	list, err := s.store.ListWorkItemMessages(r.Context(), task, item, revision, after, limit+1)
	if err != nil {
		fail(w, err)
		return
	}
	list, ok = boundMessageList(list, limit)
	if !ok {
		writeError(w, http.StatusInternalServerError, "history page exceeds response limit")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createWorkItemUpdate(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
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
	var req api.CreateWorkItemUpdate
	if !decode(w, r, &req) {
		return
	}
	result, replay, err := s.store.CreateWorkItemUpdate(r.Context(), task, item, req, caller)
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

func (s *Server) getWorkItemUpdateReceipt(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
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
	result, err := s.store.GetWorkItemUpdateReceipt(r.Context(), task, item, r.PathValue("requestID"), r.URL.Query().Get("agentId"), caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
