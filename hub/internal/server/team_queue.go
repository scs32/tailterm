package server

import (
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http"
	"strconv"
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
	opts, err := teamQueueListOptions(r)
	if err != nil {
		fail(w, err)
		return
	}
	v, err := s.store.TeamQueuePage(r.Context(), task, opts)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// teamQueueListOptions reads view, item, limit and after. A given limit must
// be 1..MaxLimit and after must not be negative.
func teamQueueListOptions(r *http.Request) (api.TeamQueueListOptions, error) {
	q := r.URL.Query()
	opts := api.TeamQueueListOptions{View: q.Get("view"), Item: q.Get("item")}
	if q.Has("view") && opts.View != api.TeamQueueViewActive {
		return opts, api.ErrInvalid
	}
	if q.Has("item") && !api.ValidID(opts.Item, "wi") {
		return opts, api.ErrInvalid
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > api.MaxLimit {
			return opts, api.ErrInvalid
		}
		opts.Limit = n
	}
	if q.Has("after") {
		n, err := strconv.ParseInt(q.Get("after"), 10, 64)
		if err != nil || n < 0 {
			return opts, api.ErrInvalid
		}
		opts.After = n
	}
	return opts, nil
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
