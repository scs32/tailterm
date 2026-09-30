package server

import (
	"net/http"
	"strconv"
)

// workItemTriage answers the read-only backlog triage suggestions. An agent
// session names its exact run; the store admits only a database handler.
func (s *Server) workItemTriage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, ok := taskID(w, r)
	if !ok {
		return
	}
	staleDays := 0
	if raw := r.URL.Query().Get("staleDays"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "staleDays must be a positive number of days")
			return
		}
		staleDays = n
	}
	out, err := s.store.WorkItemTriage(r.Context(), task, staleDays, r.URL.Query().Get("agentId"), r.URL.Query().Get("runId"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
