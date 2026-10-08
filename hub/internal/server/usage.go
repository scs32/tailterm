package server

import (
	"errors"
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

// The token budget: owner-set budget rows, lane default estimates, the
// provider readings host relays report, and the holds the relay reads before
// it wakes a run (docs/usage-accounting.md, "Token budget").
// failBudget answers a refused budget setting with its named reason: which
// value was wrong. Every other error is answered as fail answers it.
func failBudget(w http.ResponseWriter, err error) {
	if errors.Is(err, api.ErrInvalid) && err != api.ErrInvalid {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	fail(w, err)
}
func (s *Server) usageBudgets(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.UsageBudgets(r.Context(), r.PathValue("id"), r.URL.Query().Get("host"))
	if err != nil {
		failBudget(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) setUsageBudget(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	var req api.UsageBudgetRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.SetUsageBudget(r.Context(), r.PathValue("id"), req, by)
	if err != nil {
		failBudget(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) deleteUsageBudget(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	var req api.UsageBudgetRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.DeleteUsageBudget(r.Context(), r.PathValue("id"), req, by)
	if err != nil {
		failBudget(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) usageEstimateDefaults(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.UsageEstimateDefaults(r.Context(), r.PathValue("id"))
	if err != nil {
		failBudget(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) setUsageEstimateDefaults(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	var req api.UsageEstimateDefaultsRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.store.SetUsageEstimateDefaults(r.Context(), r.PathValue("id"), req, by)
	if err != nil {
		failBudget(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// usageHolds lists the project's holds; with agent and run it says only
// whether that exact run is held.
func (s *Server) usageHolds(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := s.store.BudgetHolds(r.Context(), r.PathValue("id"), q.Get("agent"), q.Get("run"))
	if err != nil {
		failBudget(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) reportProviderUsage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	var report api.ProviderUsageReport
	if !decode(w, r, &report) {
		return
	}
	out, err := s.store.ReportProviderUsage(r.Context(), report)
	if err != nil {
		failBudget(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) providerUsage(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.ProviderUsage(r.Context(), r.URL.Query().Get("host"))
	if err != nil {
		failBudget(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
