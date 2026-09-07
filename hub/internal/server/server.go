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
	m := s.mux
	m.HandleFunc("GET /v1/whoami", s.whoami)
	m.HandleFunc("GET /v1/tasks", s.listTasks)
	m.HandleFunc("POST /v1/tasks", s.createTask)
	m.HandleFunc("GET /v1/tasks/{id}", s.getTask)
	m.HandleFunc("PATCH /v1/tasks/{id}", s.updateTask)
	m.HandleFunc("DELETE /v1/tasks/{id}", s.closeTask)
	m.HandleFunc("POST /v1/tasks/{id}/agents", s.addAgent)
	m.HandleFunc("GET /v1/tasks/{id}/agents", s.listAgents)
	m.HandleFunc("GET /v1/tasks/{id}/agents/{aid}", s.getAgent)
	m.HandleFunc("PATCH /v1/tasks/{id}/agents/{aid}", s.updateAgent)
	m.HandleFunc("DELETE /v1/tasks/{id}/agents/{aid}", s.closeAgent)
	m.HandleFunc("POST /v1/tasks/{id}/messages", s.postMessage)
	m.HandleFunc("GET /v1/tasks/{id}/messages", s.listMessages)
	m.HandleFunc("POST /v1/tasks/{id}/messages/read", s.markRead)
	m.HandleFunc("POST /v1/tasks/{id}/events", s.postEvent)
	m.HandleFunc("GET /v1/tasks/{id}/events", s.taskEvents)
	m.HandleFunc("GET /v1/events", s.globalEvents)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
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
	switch {
	case errors.Is(err, api.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, api.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid request")
	case errors.Is(err, api.ErrLimit):
		writeError(w, http.StatusConflict, "limit reached")
	case errors.Is(err, api.ErrClosed):
		writeError(w, http.StatusConflict, "task is closed")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, api.MaxBody))
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
	if !decode(w, r, &req) {
		return
	}
	a, err := s.store.AddAgent(r.Context(), id, req, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, a)
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
	a, err := s.store.CloseAgent(r.Context(), a.ID, c)
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
	m, err := s.store.PostMessage(r.Context(), id, req, c)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, m)
}

func queryInt(r *http.Request, key string, fallback int64) int64 {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
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
	msgs, err := s.store.ListMessages(r.Context(), id, queryInt(r, "after", 0), to, int(queryInt(r, "limit", 50)))
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
