// Package server exposes the hub store over HTTP on the tailnet.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

// Identity resolves the caller of a request. Production uses Tailscale WhoIs.
type Identity func(r *http.Request) (api.Caller, error)

type Server struct {
	store    *store.Store
	identity Identity
	mux      *http.ServeMux
	limiter  *limiter
	logf     func(string, ...any)
}

func New(st *store.Store, identity Identity) *Server {
	s := &Server{store: st, identity: identity, mux: http.NewServeMux(), limiter: newLimiter(20, 40), logf: log.Printf}
	s.profileRoutes()
	m := s.mux
	m.HandleFunc("GET /v1/whoami", s.whoami)
	m.HandleFunc("GET /v1/capabilities", s.capabilities)
	m.HandleFunc("GET /v1/tasks", s.listTasks)
	m.HandleFunc("POST /v1/tasks", s.createTask)
	m.HandleFunc("GET /v1/work-items", s.listAllWorkItems)
	m.HandleFunc("GET /v1/tasks/{id}", s.getTask)
	m.HandleFunc("PATCH /v1/tasks/{id}", s.updateTask)
	m.HandleFunc("POST /v1/tasks/{id}/lead", s.assignLead)
	m.HandleFunc("DELETE /v1/tasks/{id}", s.closeTask)
	m.HandleFunc("GET /v1/tasks/{id}/pause", s.getProjectPause)
	m.HandleFunc("POST /v1/tasks/{id}/pause", s.pauseProject)
	m.HandleFunc("POST /v1/tasks/{id}/pause/handoff", s.resolvePauseHandoff)
	m.HandleFunc("POST /v1/tasks/{id}/resume", s.resumeProject)
	m.HandleFunc("POST /v1/tasks/{id}/resume/confirm", s.confirmProjectResume)
	m.HandleFunc("POST /v1/tasks/{id}/agents", s.addAgent)
	m.HandleFunc("POST /v1/tasks/{id}/allocation-intents", s.createAllocationIntent)
	m.HandleFunc("GET /v1/tasks/{id}/allocation-intents/{agentId}", s.getAllocationIntent)
	m.HandleFunc("GET /v1/tasks/{id}/agents", s.listAgents)
	m.HandleFunc("GET /v1/tasks/{id}/agents/{aid}", s.getAgent)
	m.HandleFunc("GET /v1/tasks/{id}/agents/{aid}/work-context", s.getAgentWorkContext)
	m.HandleFunc("GET /v1/tasks/{id}/agents/{aid}/current-assignment", s.currentAssignment)
	m.HandleFunc("GET /v1/tasks/{id}/agents/{aid}/delivery-coverage", s.deliveryCoverage)
	m.HandleFunc("GET /v1/tasks/{id}/agents/{aid}/delivery-coverages", s.deliveryCoverages)
	m.HandleFunc("PATCH /v1/tasks/{id}/agents/{aid}", s.updateAgent)
	m.HandleFunc("DELETE /v1/tasks/{id}/agents/{aid}", s.closeAgent)
	m.HandleFunc("POST /v1/tasks/{id}/agents/{aid}/cleanup", s.agentCleanup)
	m.HandleFunc("POST /v1/tasks/{id}/messages", s.postMessage)
	m.HandleFunc("GET /v1/tasks/{id}/messages", s.listMessages)
	m.HandleFunc("GET /v1/tasks/{id}/message-checks", s.listMessageChecks)
	m.HandleFunc("GET /v1/tasks/{id}/obligations", s.listObligations)
	m.HandleFunc("POST /v1/tasks/{id}/messages/{seq}/ack", s.obligationAction("ack"))
	m.HandleFunc("POST /v1/tasks/{id}/messages/{seq}/progress", s.obligationAction("progress"))
	m.HandleFunc("POST /v1/tasks/{id}/obligations/{oid}/reassign", s.reassignObligation)
	m.HandleFunc("POST /v1/tasks/{id}/obligations/{oid}/nudge", s.nudgeObligation)
	m.HandleFunc("POST /v1/tasks/{id}/obligations/{oid}/extend", s.extendObligation)
	m.HandleFunc("POST /v1/tasks/{id}/obligations/{oid}/answer", s.answerObligation)
	m.HandleFunc("POST /v1/tasks/{id}/obligations/{oid}/cancel", s.cancelObligation)
	m.HandleFunc("POST /v1/tasks/{id}/obligations/{oid}/withdraw", s.withdrawObligation)
	m.HandleFunc("POST /v1/tasks/{id}/agents/{aid}/resume", s.resumeAgent)
	m.HandleFunc("POST /v1/tasks/{id}/agents/{aid}/wake-jobs/lease", s.leaseWakeJob)
	m.HandleFunc("POST /v1/tasks/{id}/wake-jobs/{jid}/report", s.reportWakeJob)
	m.HandleFunc("GET /v1/tasks/{id}/messages/receipts/{requestID}", s.getMessagePostReceipt)
	m.HandleFunc("GET /v1/tasks/{id}/message-audit/messages/{seq}", s.getMessageAudit)
	m.HandleFunc("GET /v1/tasks/{id}/message-audit/messages/{seq}/history", s.listMessageAuditHistory)
	m.HandleFunc("POST /v1/tasks/{id}/message-audit/messages/{seq}/corrections", s.correctMessageAudit)
	m.HandleFunc("POST /v1/tasks/{id}/message-audit/messages/{seq}/resolve", s.resolveMessageAudit)
	m.HandleFunc("GET /v1/tasks/{id}/message-audit/receipts/{requestID}", s.getMessageAuditReceipt)
	m.HandleFunc("GET /v1/tasks/{id}/message-audit/changes", s.listMessageAuditChanges)
	m.HandleFunc("POST /v1/tasks/{id}/message-audit/associations", s.createMessageAuditAssociation)
	m.HandleFunc("GET /v1/tasks/{id}/message-audit/associations/receipts/{requestID}", s.getMessageAuditAssociationReceipt)
	m.HandleFunc("POST /v1/tasks/{id}/audit-exports", s.createAuditExport)
	m.HandleFunc("GET /v1/tasks/{id}/audit-exports/{exportID}", s.getAuditExportChunk)
	m.HandleFunc("GET /v1/tasks/{id}/queue", s.listQueue)
	m.HandleFunc("GET /v1/tasks/{id}/queue/changes", s.listQueueChanges)
	m.HandleFunc("GET /v1/tasks/{id}/queue-receipts/{requestID}", s.getQueueReceipt)
	m.HandleFunc("GET /v1/tasks/{id}/queue/{qid}", s.getQueueEntry)
	m.HandleFunc("GET /v1/tasks/{id}/queue/{qid}/history", s.listQueueHistory)
	m.HandleFunc("POST /v1/tasks/{id}/queue/{qid}/actions", s.queueAction)
	m.HandleFunc("POST /v1/tasks/{id}/decisions", s.createDecision)
	m.HandleFunc("GET /v1/tasks/{id}/decisions", s.listDecisions)
	m.HandleFunc("POST /v1/tasks/{id}/decisions/{seq}/answer", s.answerDecision)
	m.HandleFunc("POST /v1/tasks/{id}/messages/read", s.markRead)
	m.HandleFunc("POST /v1/tasks/{id}/required-deliveries", retiredWriter)
	m.HandleFunc("POST /v1/tasks/{id}/operational-records", retiredWriter)
	m.HandleFunc("POST /v1/tasks/{id}/operational-records/{recordId}/commit", retiredWriter)
	m.HandleFunc("GET /v1/tasks/{id}/operational-records/{recordId}", s.getOperationalRecord)
	m.HandleFunc("POST /v1/tasks/{id}/deliveries/{deliveryId}/follow-through/check", retiredWriter)
	m.HandleFunc("POST /v1/tasks/{id}/deliveries/{deliveryId}/follow-through/report", retiredWriter)
	m.HandleFunc("POST /v1/tasks/{id}/deliveries/{deliveryId}/recovery-incidents", retiredWriter)
	m.HandleFunc("POST /v1/tasks/{id}/deliveries/{deliveryId}/{operation}", retiredWriter)
	m.HandleFunc("POST /v1/tasks/{id}/deliveries/{deliveryId}/blocks/{blockId}/resolutions", retiredWriter)
	m.HandleFunc("POST /v1/tasks/{id}/events", s.postEvent)
	m.HandleFunc("GET /v1/tasks/{id}/events", s.taskEvents)
	m.HandleFunc("GET /v1/tasks/{id}/work-items", s.listTaskWorkItems)
	m.HandleFunc("POST /v1/tasks/{id}/work-items", s.createWorkItem)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}", s.getWorkItem)
	m.HandleFunc("PATCH /v1/tasks/{id}/work-items/{wid}", s.updateWorkItem)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/revisions", s.listWorkItemRevisions)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/revisions/{revision}", s.getWorkItemRevision)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/history-gaps", s.listWorkItemHistoryGaps)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/messages", s.listWorkItemMessages)
	m.HandleFunc("POST /v1/tasks/{id}/work-items/{wid}/updates", s.createWorkItemUpdate)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/updates/receipts/{requestID}", s.getWorkItemUpdateReceipt)
	m.HandleFunc("POST /v1/tasks/{id}/work-items/{wid}/dispatch", s.dispatchWorkItem)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/narrative", s.getNarrativeOverview)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/narrative/timeline", s.listNarrativeTimeline)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/narrative/artifacts", s.listNarrativeArtifacts)
	m.HandleFunc("POST /v1/tasks/{id}/work-items/{wid}/narrative/artifacts", s.putNarrativeArtifact)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/narrative/artifacts/{artifact}/versions", s.listNarrativeArtifactVersions)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/narrative/artifacts/{artifact}/versions/{version}", s.getNarrativeArtifactVersion)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/narrative/links", s.listNarrativeLinks)
	m.HandleFunc("POST /v1/tasks/{id}/work-items/{wid}/narrative/links", s.putNarrativeLink)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/narrative/coverage", s.listNarrativeCoverage)
	m.HandleFunc("POST /v1/tasks/{id}/work-items/{wid}/narrative/coverage", s.putNarrativeCoverage)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/narrative/reports", s.listNarrativeReports)
	m.HandleFunc("POST /v1/tasks/{id}/work-items/{wid}/narrative/reports", s.putNarrativeReport)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/narrative/reports/{report}/versions/{version}", s.getNarrativeReportVersion)
	m.HandleFunc("GET /v1/tasks/{id}/work-items/{wid}/narrative/receipts/{requestID}", s.getNarrativeReceipt)
	m.HandleFunc("GET /v1/events", s.globalEvents)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The Discord bridge credential reaches only its allowlisted routes.
	if r.Header.Get("Authorization") == "" {
		// Tailnet identity: no bearer token, so never the bridge.
	} else if c, err := s.identity(r); err == nil && c.Node == api.BridgeNode {
		if _, pattern := s.mux.Handler(r); !api.BridgeRoutes[pattern] {
			writeError(w, http.StatusForbidden, "the bridge credential cannot use this route")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

type ctxKey struct{}

func (s *Server) caller(w http.ResponseWriter, r *http.Request) (api.Caller, bool) {
	c, err := s.identity(r)
	if err != nil {
		writeError(w, http.StatusForbidden, "identity unavailable: "+err.Error())
		return c, false
	}
	return c, true
}

func (s *Server) writer(w http.ResponseWriter, r *http.Request) (api.Caller, bool) {
	c, ok := s.caller(w, r)
	if !ok {
		return c, false
	}
	if !s.limiter.allow(c.Node) {
		writeError(w, http.StatusTooManyRequests, "rate limited")
		return c, false
	}
	return c, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, api.ErrorResponse{Error: msg})
}

func fail(w http.ResponseWriter, err error) {
	var envelopeErr *api.EnvelopeError
	var unacked *api.UnacknowledgedError
	switch {
	case errors.As(err, &unacked):
		writeJSON(w, http.StatusConflict, api.ErrorResponse{Error: unacked.Error(), Code: "unacknowledged"})
	case errors.Is(err, api.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, api.ErrAgentSpawnLimit):
		writeError(w, 409, err.Error())
	case errors.Is(err, api.ErrAgentSpawnDisabled):
		writeError(w, http.StatusForbidden, "Agents cannot add agents to this task. Ask the owner to enable Allow agents to add other agents in task settings.")
	case errors.Is(err, api.ErrContextLimit):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrDecisionAnswerForbidden):
		writeError(w, http.StatusForbidden, "only a human can answer a decision request")
	case errors.As(err, &envelopeErr):
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: "invalid envelope", Code: "invalid-envelope", Problems: envelopeErr.Problems})
	case errors.Is(err, api.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid request")
	case errors.Is(err, api.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, api.ErrNarrativeReportRequired):
		writeJSON(w, http.StatusConflict, api.ErrorResponse{Error: err.Error(), Code: "report-required"})
	case errors.Is(err, api.ErrNarrativeReportStale):
		writeJSON(w, http.StatusConflict, api.ErrorResponse{Error: err.Error(), Code: "report-stale"})
	case errors.Is(err, api.ErrLimit):
		writeError(w, http.StatusConflict, "limit reached")
	case errors.Is(err, api.ErrClosed):
		writeError(w, http.StatusConflict, "task is closed")
	case errors.Is(err, api.ErrExpired):
		writeJSON(w, http.StatusGone, api.ErrorResponse{Error: "audit export expired or unavailable", Code: "export-expired"})
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); ok {
		writeJSON(w, http.StatusOK, api.CurrentCapabilities())
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeLimited(w, r, v, api.MaxBody)
}

func decodeLimited(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON")
		return false
	}
	return true
}

func taskID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !api.ValidID(id, "tsk") {
		writeError(w, http.StatusNotFound, "not found")
		return "", false
	}
	return id, true
}

func agentID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("aid")
	if !api.ValidID(id, "agt") {
		writeError(w, http.StatusNotFound, "not found")
		return "", false
	}
	return id, true
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	if c, ok := s.caller(w, r); ok {
		writeJSON(w, 200, c)
	}
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	tasks, err := s.store.ListTasks(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, api.TaskList{Tasks: tasks})
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	var req api.CreateTaskRequest
	if !decode(w, r, &req) {
		return
	}
	t, err := s.store.CreateTask(r.Context(), req, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, t)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	t, err := s.store.GetTask(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	agents, err := s.store.ListAgents(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	latest, err := s.store.LatestSeq(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, api.TaskDetail{Task: t, Agents: agents, LatestSeq: latest})
}

func (s *Server) updateTask(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.UpdateTaskRequest
	if !decode(w, r, &req) {
		return
	}
	t, err := s.store.UpdateTask(r.Context(), id, req, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, t)
}

func (s *Server) closeTask(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	t, err := s.store.CloseTask(r.Context(), id, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, t)
}

func (s *Server) getProjectPause(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	status, err := s.store.ProjectPauseStatus(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) pauseProject(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.PauseProjectRequest
	if !decode(w, r, &req) {
		return
	}
	status, err := s.store.PauseProject(r.Context(), id, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) resolvePauseHandoff(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.ResolvePauseHandoffRequest
	if !decode(w, r, &req) {
		return
	}
	status, err := s.store.ResolvePauseHandoff(r.Context(), id, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) resumeProject(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.ResumeProjectRequest
	if !decode(w, r, &req) {
		return
	}
	status, err := s.store.ResumeProject(r.Context(), id, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) confirmProjectResume(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.ConfirmProjectResumeRequest
	if !decode(w, r, &req) {
		return
	}
	status, err := s.store.ConfirmProjectResume(r.Context(), id, req, caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) addAgent(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.AddAgentRequest
	if !decodeLimited(w, r, &req, api.MaxAgentRegistrationBody) {
		return
	}
	a, err := s.store.AddAgent(r.Context(), id, req, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, a)
}

func (s *Server) createAllocationIntent(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.CreateAllocationIntentRequest
	if !decodeLimited(w, r, &req, api.MaxAgentRegistrationBody) {
		return
	}
	in, err := s.store.CreateAllocationIntent(r.Context(), id, req, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, in)
}

func (s *Server) getAllocationIntent(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	aid := r.PathValue("agentId")
	if !api.ValidID(aid, "agt") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	in, err := s.store.GetAllocationIntent(r.Context(), id, aid)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, in)
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.GetTask(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	agents, err := s.store.ListAgents(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, api.AgentList{Agents: agents})
}

func (s *Server) agentInTask(w http.ResponseWriter, r *http.Request) (api.Agent, bool) {
	tid, ok := taskID(w, r)
	if !ok {
		return api.Agent{}, false
	}
	aid, ok := agentID(w, r)
	if !ok {
		return api.Agent{}, false
	}
	a, err := s.store.GetAgent(r.Context(), aid)
	if err != nil || a.TaskID != tid {
		writeError(w, http.StatusNotFound, "not found")
		return a, false
	}
	return a, true
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	if a, ok := s.agentInTask(w, r); ok {
		writeJSON(w, 200, a)
	}
}

func (s *Server) getAgentWorkContext(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	a, ok := s.agentInTask(w, r)
	if !ok {
		return
	}
	context, err := s.store.GetAgentWorkItemContext(r.Context(), a.TaskID, a.ID, r.URL.Query().Get("runId"))
	if err != nil {
		fail(w, err)
		return
	}
	// Preserve the admitted RawMessage bytes and digest on the context read path.
	data, err := api.MarshalAgentWorkItemContext(context)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	a, ok := s.agentInTask(w, r)
	if !ok {
		return
	}
	var req api.UpdateAgentRequest
	if !decode(w, r, &req) {
		return
	}
	a, err := s.store.UpdateAgent(r.Context(), a.ID, req, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) closeAgent(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	a, ok := s.agentInTask(w, r)
	if !ok {
		return
	}
	a, err := s.store.CloseAgentRun(r.Context(), a.ID, r.URL.Query().Get("runId"), c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.PostMessageRequest
	if !decode(w, r, &req) {
		return
	}
	// Only the bridge records a Discord source, and the bridge only ever
	// speaks for the owner, never as an agent.
	if (req.Source != nil) != (c.Node == api.BridgeNode) || (c.Node == api.BridgeNode && (req.AgentID != "" || req.RunID != "")) {
		writeError(w, http.StatusBadRequest, "a message source is required from the bridge and allowed only from the bridge")
		return
	}
	m, err := s.store.PostMessage(r.Context(), id, req, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, m)
}

func (s *Server) getMessagePostReceipt(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	message, err := s.store.GetMessagePostReceipt(
		r.Context(), id, r.PathValue("requestID"), r.URL.Query().Get("agentId"), c,
	)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, message)
}

func queryInt(r *http.Request, key string, fallback int64) int64 {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
}

// listObligations lists a task's obligations (docs/broker-phase-2a.md). When the
// caller names an agent and its current run, listing counts as delivery to
// that run (never as acknowledgement).
func (s *Server) listObligations(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.GetTask(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	q := r.URL.Query()
	now := time.Now().UTC()
	agent, run := q.Get("agentId"), q.Get("runId")
	if agent != "" && run != "" {
		if err := s.store.MarkObligationsDelivered(r.Context(), id, agent, run, now); err != nil {
			fail(w, err)
			return
		}
	}
	list, err := s.store.ListObligations(r.Context(), id, store.ObligationFilter{AgentID: agent, OpenOnly: q.Get("open") == "1", Overdue: q.Get("overdue") == "1", FromSeq: queryInt(r, "fromSeq", 0), ToSeq: queryInt(r, "toSeq", 0)}, now)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, api.ObligationList{Obligations: list})
}

func (s *Server) obligationAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.writer(w, r); !ok {
			return
		}
		id, ok := taskID(w, r)
		if !ok {
			return
		}
		seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
		if err != nil || seq < 1 {
			writeError(w, http.StatusBadRequest, "invalid message sequence")
			return
		}
		var req api.ObligationActionRequest
		if !decode(w, r, &req) {
			return
		}
		o, err := s.store.ObligationAction(r.Context(), id, seq, action, req, time.Now().UTC())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, o)
	}
}

// leaseWakeJob gives the host relay the next broker wake for an agent's run;
// 204 means nothing is due.
func (s *Server) leaseWakeJob(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.ObligationActionRequest
	if !decode(w, r, &req) {
		return
	}
	job, err := s.store.LeaseWakeJob(r.Context(), id, r.PathValue("aid"), req.RunID, time.Now().UTC())
	if err != nil {
		fail(w, err)
		return
	}
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, 200, job)
}

func (s *Server) reportWakeJob(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.WakeJobReport
	if !decode(w, r, &req) {
		return
	}
	if err := s.store.ReportWakeJob(r.Context(), id, r.PathValue("jid"), req, time.Now().UTC()); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reassignObligation(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.ObligationReassignRequest
	if !decode(w, r, &req) {
		return
	}
	if c.Node == api.BridgeNode && (req.ActorAgentID != "" || req.ActorRunID != "") {
		writeError(w, http.StatusBadRequest, "the bridge reassigns as the owner, never as an agent")
		return
	}
	m, err := s.store.ReassignObligation(r.Context(), id, r.PathValue("oid"), req, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, m)
}

// nudgeObligation is the owner's immediate re-wake (broker phase 2b).
func (s *Server) nudgeObligation(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.ObligationNudgeRequest
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	result, err := s.store.NudgeObligation(r.Context(), id, r.PathValue("oid"), req.RequestID, c)
	var tooSoon *api.ErrNudgeTooSoon
	if errors.As(err, &tooSoon) {
		w.Header().Set("Retry-After", strconv.Itoa(int(tooSoon.RetryAfter.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, api.ErrorResponse{Error: tooSoon.Error(), Code: "nudge-too-soon"})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// listMessageChecks pages broker phase-1 shadow checks (docs/broker-phase-1.md).
func (s *Server) listMessageChecks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.GetTask(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	checks, err := s.store.ListMessageChecks(r.Context(), id, queryInt(r, "after", 0), int(queryInt(r, "limit", 500)))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, api.MessageCheckList{Checks: checks})
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.GetTask(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	to := r.URL.Query().Get("to")
	if to != "" && !api.ValidID(to, "agt") {
		writeError(w, http.StatusBadRequest, "invalid agent id")
		return
	}
	after := queryInt(r, "after", 0)
	if r.URL.Query().Get("latest") == "1" {
		after = -1
	}
	msgs, err := s.store.ListMessages(r.Context(), id, after, to, int(queryInt(r, "limit", 50)))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, api.MessageList{Messages: msgs})
}

func (s *Server) markRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.writer(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.MarkReadRequest
	if !decode(w, r, &req) {
		return
	}
	a, err := s.store.GetAgent(r.Context(), req.AgentID)
	if err != nil || a.TaskID != id {
		writeError(w, http.StatusBadRequest, "invalid agent id")
		return
	}
	if err := s.store.MarkRead(r.Context(), id, req); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) postEvent(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writer(w, r)
	if !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	var req api.PostEventRequest
	if !decode(w, r, &req) {
		return
	}
	e, err := s.store.PostEvent(r.Context(), id, req, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, e)
}

func waitFor(r *http.Request) time.Duration {
	v := r.URL.Query().Get("wait")
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		if secs, err2 := strconv.Atoi(v); err2 == nil {
			d = time.Duration(secs) * time.Second
		} else {
			return 0
		}
	}
	if d < 0 {
		return 0
	}
	if d > api.MaxWait {
		return api.MaxWait
	}
	return d
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, id string) {
	after := queryInt(r, "after", 0)
	limit := int(queryInt(r, "limit", 100))
	events, err := s.store.WaitEvents(r.Context(), id, after, limit, waitFor(r))
	if err != nil {
		fail(w, err)
		return
	}
	next := after
	if len(events) > 0 {
		next = events[len(events)-1].Seq
	}
	writeJSON(w, 200, api.EventList{Events: events, Next: next})
}

func (s *Server) taskEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	id, ok := taskID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.GetTask(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	s.events(w, r, id)
}

func (s *Server) globalEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	s.events(w, r, "")
}

// limiter is a per-key token bucket for write requests.
type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(perSecond, burst float64) *limiter {
	return &limiter{rate: perSecond, burst: burst, buckets: map[string]*bucket{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// StaticIdentity returns a fixed caller; used by the dev listener and tests.
func StaticIdentity(c api.Caller) Identity {
	return func(*http.Request) (api.Caller, error) { return c, nil }
}

// Shutdown is a placeholder for symmetry with net/http servers.
func (s *Server) Shutdown(context.Context) error { return nil }

func (s *Server) agentCleanup(w http.ResponseWriter, r *http.Request) {
	by, ok := s.writer(w, r)
	if !ok {
		return
	}
	a, ok := s.agentInTask(w, r)
	if !ok {
		return
	}
	var req api.CleanupRequest
	if !decode(w, r, &req) {
		return
	}
	a, err := s.store.ReportCleanup(r.Context(), a.ID, req, by)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, a)
}
