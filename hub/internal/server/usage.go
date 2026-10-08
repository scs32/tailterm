package server

import (
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
	"net/http"
)

func (s *Server) reportUsage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	a, ok := s.agentInTask(w, r)
	if !ok {
		return
	}
	var b api.UsageBatch
	if !decode(w, r, &b) {
		return
	}
	out, err := s.store.ReportUsage(r.Context(), a.TaskID, a.ID, b)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) usageReport(w http.ResponseWriter, r *http.Request) {
	q := api.UsageQuery{Item: r.URL.Query().Get("item")}
	var err error
	if q.From, err = store.ParseUsageTime(r.URL.Query().Get("from")); err != nil {
		fail(w, err)
		return
	}
	if q.To, err = store.ParseUsageTime(r.URL.Query().Get("to")); err != nil {
		fail(w, err)
		return
	}
	out, err := s.store.Usage(r.Context(), r.PathValue("id"), q)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) usagePrices(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.UsagePrices(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) setUsagePrices(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	var req api.UsagePriceRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.SetUsagePrices(r.Context(), r.PathValue("id"), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) usageWarnings(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.UsageWarnings(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) setUsageWarning(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	var req api.UsageWarningRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.SetUsageWarning(r.Context(), r.PathValue("id"), req, by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
