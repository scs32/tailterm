package server

import (
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http"
)

func (s *Server) getTeamCloseReceipt(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	v, err := s.store.GetTeamCloseReceipt(r.Context(), task, r.PathValue("requestID"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) listTeamQueue(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	v, err := s.store.ListTeamQueue(r.Context(), task)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
func (s *Server) getTeamQueueEntry(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	v, err := s.store.GetTeamQueueEntry(r.Context(), task, r.PathValue("entry"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
func (s *Server) teamQueueAction(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.TeamQueueRequest
	if !decode(w, r, &req) {
		return
	}
	v, err := s.store.TeamQueueAction(r.Context(), task, req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
func (s *Server) teamQueuesByHost(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	v, err := s.store.TeamQueuesByHost(r.Context(), r.URL.Query().Get("host"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
