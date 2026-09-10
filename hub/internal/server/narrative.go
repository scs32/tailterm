package server

import (
	"net/http"
	"strconv"

	"github.com/scs32/tailterm/hub/internal/api"
)

func narrativePage(r *http.Request) (string, int, bool) {
	limit := api.DefaultNarrativePage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > api.MaxNarrativePage {
			return "", 0, false
		}
		limit = n
	}
	return r.URL.Query().Get("cursor"), limit, true
}

func narrativeIDs(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	task, ok := taskID(w, r)
	if !ok {
		return "", "", false
	}
	item, ok := workItemID(w, r)
	if !ok {
		return "", "", false
	}
	return task, item, true
}

func (s *Server) getNarrativeOverview(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	result, err := s.store.GetNarrativeOverview(r.Context(), task, item)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listNarrativeTimeline(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	cursor, limit, ok := narrativePage(r)
	if !ok {
		writeError(w, 400, "invalid narrative cursor or limit")
		return
	}
	q := r.URL.Query()
	result, err := s.store.ListNarrativeTimeline(r.Context(), task, item, cursor, limit, q.Get("kind"), q.Get("source"), q.Get("relationship"), q.Get("captureState"), q.Get("sourceFrom"), q.Get("sourceTo"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) listNarrativeArtifacts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	cursor, limit, ok := narrativePage(r)
	if !ok {
		writeError(w, 400, "invalid narrative cursor or limit")
		return
	}
	result, err := s.store.ListNarrativeArtifacts(r.Context(), task, item, cursor, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (s *Server) putNarrativeArtifact(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	var req api.PutNarrativeArtifactRequest
	if !decodeLimited(w, r, &req, api.MaxNarrativeRequestBody) {
		return
	}
	result, replay, err := s.store.PutNarrativeArtifact(r.Context(), task, item, req, caller)
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
func (s *Server) listNarrativeArtifactVersions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	cursor, limit, ok := narrativePage(r)
	if !ok {
		writeError(w, 400, "invalid narrative cursor or limit")
		return
	}
	result, err := s.store.ListNarrativeArtifactVersions(r.Context(), task, item, r.PathValue("artifact"), cursor, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (s *Server) getNarrativeArtifactVersion(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || version < 1 {
		writeError(w, 404, "not found")
		return
	}
	result, err := s.store.GetNarrativeArtifactVersion(r.Context(), task, item, r.PathValue("artifact"), version)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}

func (s *Server) listNarrativeLinks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	cursor, limit, ok := narrativePage(r)
	if !ok {
		writeError(w, 400, "invalid narrative cursor or limit")
		return
	}
	result, err := s.store.ListNarrativeLinks(r.Context(), task, item, cursor, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (s *Server) putNarrativeLink(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	var req api.PutNarrativeLinkRequest
	if !decode(w, r, &req) {
		return
	}
	result, replay, err := s.store.PutNarrativeLink(r.Context(), task, item, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	status := 201
	if replay {
		status = 200
	}
	writeJSON(w, status, result)
}

func (s *Server) listNarrativeCoverage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	cursor, limit, ok := narrativePage(r)
	if !ok {
		writeError(w, 400, "invalid narrative cursor or limit")
		return
	}
	result, err := s.store.ListNarrativeCoverage(r.Context(), task, item, cursor, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (s *Server) putNarrativeCoverage(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	var req api.PutNarrativeCoverageRequest
	if !decode(w, r, &req) {
		return
	}
	result, replay, err := s.store.PutNarrativeCoverage(r.Context(), task, item, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	status := 201
	if replay {
		status = 200
	}
	writeJSON(w, status, result)
}

func (s *Server) listNarrativeReports(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	cursor, limit, ok := narrativePage(r)
	if !ok {
		writeError(w, 400, "invalid narrative cursor or limit")
		return
	}
	result, err := s.store.ListNarrativeReports(r.Context(), task, item, cursor, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (s *Server) putNarrativeReport(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	var req api.PutNarrativeReportRequest
	if !decodeLimited(w, r, &req, api.MaxNarrativeRequestBody) {
		return
	}
	result, replay, err := s.store.PutNarrativeReport(r.Context(), task, item, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	status := 201
	if replay {
		status = 200
	}
	writeJSON(w, status, result)
}
func (s *Server) getNarrativeReportVersion(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || version < 1 {
		writeError(w, 404, "not found")
		return
	}
	result, err := s.store.GetNarrativeReportVersion(r.Context(), task, item, r.PathValue("report"), version)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (s *Server) getNarrativeReceipt(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	task, item, ok := narrativeIDs(w, r)
	if !ok {
		return
	}
	result, err := s.store.GetNarrativeReceipt(r.Context(), task, item, r.URL.Query().Get("operation"), r.PathValue("requestID"), r.URL.Query().Get("agentId"), caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}
