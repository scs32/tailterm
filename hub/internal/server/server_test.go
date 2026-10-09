package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
	who api.Caller
	s   *Server
}

func newClient(t *testing.T) *client {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &client{t: t, st: st, who: api.Caller{Node: "devbox", User: "stephen@example.com"}}
	s := New(st, func(r *http.Request) (api.Caller, error) {
		if r.Header.Get("X-Test-Deny") != "" {
			return api.Caller{}, fmt.Errorf("denied")
		}
		return c.who, nil
	})
	c.s = s
	c.srv = httptest.NewServer(s)
	t.Cleanup(c.srv.Close)
	return c
}

func (c *client) do(method, path string, body any, out any) int {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if s, ok := body.(string); ok {
			buf.WriteString(s)
		} else if err := json.NewEncoder(&buf).Encode(body); err != nil {
			c.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, c.srv.URL+path, &buf)
	if err != nil {
		c.t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func (c *client) task(name string) api.Task {
	c.t.Helper()
	var task api.Task
	if code := c.do("POST", "/v1/tasks", api.CreateTaskRequest{Name: name, Goal: "ship it"}, &task); code != 201 {
		c.t.Fatalf("create task: %d", code)
	}
	return task
}

func (c *client) agent(task api.Task, name string) api.Agent {
	c.t.Helper()
	var a api.Agent
	code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{Name: name, Host: "devbox", Session: "tt-" + name, Runtime: "claude"}, &a)
	if code != 201 {
		c.t.Fatalf("add agent: %d", code)
	}
	return a
}

func TestWhoamiAndIdentity(t *testing.T) {
	c := newClient(t)
	var who api.Caller
	if code := c.do("GET", "/v1/whoami", nil, &who); code != 200 || who != c.who {
		t.Fatalf("whoami %d %+v", code, who)
	}
	req, _ := http.NewRequest("GET", c.srv.URL+"/v1/tasks", nil)
	req.Header.Set("X-Test-Deny", "1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("expected 403 without identity, got %d", res.StatusCode)
	}
}

func TestTaskAndAgentLifecycle(t *testing.T) {
	c := newClient(t)
	task := c.task("alpha")
	if task.CreatedBy != c.who || task.Status != api.TaskOpen {
		t.Fatalf("task %+v", task)
	}
	a := c.agent(task, "planner")
	if a.Status != api.AgentStarting || a.TaskID != task.ID {
		t.Fatalf("agent %+v", a)
	}
	// Idempotent registration with a supplied id.
	var again api.Agent
	c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{AgentID: a.ID, Name: "other", Host: "x", Session: "y"}, &again)
	if again.ID != a.ID || again.Name != "planner" {
		t.Fatalf("expected idempotent add, got %+v", again)
	}
	// Lifecycle events update status.
	var ev api.Event
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/events", api.PostEventRequest{Kind: api.EventStarted, AgentID: a.ID}, &ev); code != 201 {
		t.Fatalf("post event %d", code)
	}
	var detail api.TaskDetail
	c.do("GET", "/v1/tasks/"+task.ID, nil, &detail)
	if len(detail.Agents) != 1 || detail.Agents[0].Status != api.AgentRunning {
		t.Fatalf("detail %+v", detail)
	}
	c.do("POST", "/v1/tasks/"+task.ID+"/events", api.PostEventRequest{Kind: api.EventNeedsInput, AgentID: a.ID, Text: "permission"}, nil)
	var got api.Agent
	c.do("GET", "/v1/tasks/"+task.ID+"/agents/"+a.ID, nil, &got)
	if got.Status != api.AgentNeedsInput {
		t.Fatalf("status %s", got.Status)
	}
	// Non-postable kinds are rejected.
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/events", api.PostEventRequest{Kind: api.EventAgentAdded}, nil); code != 400 {
		t.Fatalf("expected 400 for agent_added, got %d", code)
	}
	// Closing the task closes agents and refuses further writes.
	var closed api.Task
	if code := c.do("DELETE", "/v1/tasks/"+task.ID, nil, &closed); code != 200 || closed.Status != api.TaskClosed || closed.ClosedAt == nil {
		t.Fatalf("close %d %+v", code, closed)
	}
	c.do("GET", "/v1/tasks/"+task.ID+"/agents/"+a.ID, nil, &got)
	if got.Status != api.AgentClosed {
		t.Fatalf("agent should be closed, got %s", got.Status)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{Text: "late"}, nil); code != 409 {
		t.Fatalf("expected 409 on closed task, got %d", code)
	}
	var events api.EventList
	c.do("GET", "/v1/tasks/"+task.ID+"/events", nil, &events)
	kinds := []string{}
	for _, e := range events.Events {
		kinds = append(kinds, e.Kind)
	}
	want := "task_created agent_added started needs_input closed task_closed"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("event kinds %q, want %q", strings.Join(kinds, " "), want)
	}
}

func TestMessagesAndUnread(t *testing.T) {
	c := newClient(t)
	task := c.task("beta")
	a := c.agent(task, "a")
	b := c.agent(task, "b")
	var m api.Message
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{Text: "hello all", AgentID: a.ID}, &m); code != 201 || m.From.AgentID != a.ID {
		t.Fatalf("post %d %+v", code, m)
	}
	c.do("POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{Text: "just for b", AgentID: a.ID, To: b.ID}, nil)
	c.do("POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{Text: "from the human"}, nil)
	var got api.Agent
	c.do("GET", "/v1/tasks/"+task.ID+"/agents/"+b.ID, nil, &got)
	if got.Unread != 3 {
		t.Fatalf("b unread %d, want 3", got.Unread)
	}
	c.do("GET", "/v1/tasks/"+task.ID+"/agents/"+a.ID, nil, &got)
	if got.Unread != 1 {
		t.Fatalf("a unread %d, want 1 (own messages excluded, direct to b excluded)", got.Unread)
	}
	var list api.MessageList
	c.do("GET", "/v1/tasks/"+task.ID+"/messages?to="+a.ID, nil, &list)
	if len(list.Messages) != 2 {
		t.Fatalf("a should see 2 messages, saw %d", len(list.Messages))
	}
	c.do("POST", "/v1/tasks/"+task.ID+"/messages/read", api.MarkReadRequest{AgentID: b.ID, UpTo: 2}, nil)
	c.do("GET", "/v1/tasks/"+task.ID+"/agents/"+b.ID, nil, &got)
	if got.Unread != 1 {
		t.Fatalf("b unread after read %d, want 1", got.Unread)
	}
	// Addressing an agent from another task is invalid.
	other := c.task("gamma")
	x := c.agent(other, "x")
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{Text: "cross", To: x.ID}, nil); code != 400 {
		t.Fatalf("expected 400 for cross-task recipient, got %d", code)
	}
}

func TestHumanDirectMessageResumesOnlineRetiredAgent(t *testing.T) {
	c := newClient(t)
	task := c.task("resume-message")
	a := c.agent(task, "worker")
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/events", api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: a.ID, RunID: a.RunID}, nil); code != 201 {
		t.Fatalf("heartbeat status = %d", code)
	}
	retired := api.AgentRetired
	if code := c.do("PATCH", "/v1/tasks/"+task.ID+"/agents/"+a.ID, api.UpdateAgentRequest{Status: &retired}, nil); code != 200 {
		t.Fatalf("retire status = %d", code)
	}
	var message api.Message
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{To: a.ID, Text: "Please continue"}, &message); code != 201 {
		t.Fatalf("message status = %d", code)
	}
	if message.From.AgentID != "" || message.To != a.ID {
		t.Fatalf("message contract changed: %+v", message)
	}
	var got api.Agent
	c.do("GET", "/v1/tasks/"+task.ID+"/agents/"+a.ID, nil, &got)
	if got.Status != api.AgentDone || got.ID != a.ID || got.RunID != a.RunID || got.Session != a.Session {
		t.Fatalf("agent was not resumed in place: before=%+v after=%+v", a, got)
	}
	var events api.EventList
	c.do("GET", "/v1/tasks/"+task.ID+"/events", nil, &events)
	resumeCount := 0
	for _, event := range events.Events {
		if event.Kind == api.EventResumed && event.AgentID == a.ID {
			resumeCount++
		}
	}
	if resumeCount != 1 {
		t.Fatalf("resume events = %d, want 1", resumeCount)
	}
}

func TestLongPollWakesAndTimesOut(t *testing.T) {
	c := newClient(t)
	task := c.task("delta")
	var initial api.EventList
	c.do("GET", "/v1/tasks/"+task.ID+"/events", nil, &initial)
	after := initial.Next
	// Timeout path.
	start := time.Now()
	var empty api.EventList
	c.do("GET", fmt.Sprintf("/v1/tasks/%s/events?after=%d&wait=200ms", task.ID, after), nil, &empty)
	if len(empty.Events) != 0 || empty.Next != after {
		t.Fatalf("expected no events, got %+v", empty)
	}
	if el := time.Since(start); el < 180*time.Millisecond || el > 2*time.Second {
		t.Fatalf("timeout wait took %v", el)
	}
	// Wake path.
	done := make(chan api.EventList, 1)
	go func() {
		var list api.EventList
		c.do("GET", fmt.Sprintf("/v1/tasks/%s/events?after=%d&wait=5s", task.ID, after), nil, &list)
		done <- list
	}()
	time.Sleep(50 * time.Millisecond)
	start = time.Now()
	c.agent(task, "worker")
	select {
	case list := <-done:
		if len(list.Events) != 1 || list.Events[0].Kind != api.EventAgentAdded {
			t.Fatalf("woke with %+v", list)
		}
		if el := time.Since(start); el > 500*time.Millisecond {
			t.Fatalf("wake took %v", el)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long-poll did not wake")
	}
	// Global feed sees both tasks.
	other := c.task("epsilon")
	var all api.EventList
	c.do("GET", "/v1/events", nil, &all)
	seen := map[string]bool{}
	for _, e := range all.Events {
		seen[e.TaskID] = true
	}
	if !seen[task.ID] || !seen[other.ID] {
		t.Fatalf("global feed missing tasks: %+v", seen)
	}
}

func TestLimitsAndValidation(t *testing.T) {
	c := newClient(t)
	if code := c.do("POST", "/v1/tasks", api.CreateTaskRequest{Name: "bad\nname"}, nil); code != 400 {
		t.Fatalf("expected 400 for bad name, got %d", code)
	}
	if code := c.do("POST", "/v1/tasks", "{not json", nil); code != 400 {
		t.Fatalf("expected 400 for malformed json, got %d", code)
	}
	big := strings.Repeat("x", api.MaxBody+1)
	if code := c.do("POST", "/v1/tasks", `{"name":"ok","goal":"`+big+`"}`, nil); code != 413 {
		t.Fatalf("expected 413 for oversized body, got %d", code)
	}
	task := c.task("limits")
	for i := 0; i < api.MaxAgentsPerTask; i++ {
		c.agent(task, fmt.Sprintf("a%d", i))
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{Name: "extra", Host: "h", Session: "s"}, nil); code != 409 {
		t.Fatalf("expected 409 at agent cap, got %d", code)
	}
	if code := c.do("GET", "/v1/tasks/tsk_ffffffffffffffff", nil, nil); code != 404 {
		t.Fatalf("expected 404, got %d", code)
	}
	if code := c.do("GET", "/v1/tasks/../etc", nil, nil); code != 404 {
		t.Fatalf("expected 404 for bad id, got %d", code)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{Text: "bad\x00byte"}, nil); code != 400 {
		t.Fatalf("expected 400 for control chars, got %d", code)
	}
}

func TestRateLimit(t *testing.T) {
	c := newClient(t)
	limited := false
	for i := 0; i < 60; i++ {
		if code := c.do("POST", "/v1/tasks", api.CreateTaskRequest{Name: fmt.Sprintf("t%d", i)}, nil); code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("expected the write rate limit to trigger")
	}
}

func TestTaskHelperBudgetAPI(t *testing.T) {
	c := newClient(t)
	var task api.Task
	if code := c.do("POST", "/v1/tasks", map[string]any{"name": "budget", "allowAgentSpawn": true, "maxNewAgents": 0}, &task); code != 201 || task.MaxNewAgents != 0 {
		t.Fatalf("create: %d %+v", code, task)
	}
	parent := c.agent(task, "parent")
	request := api.AddAgentRequest{Name: "helper", Host: "host", Session: "helper", ParentAgentID: parent.ID}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", request, nil); code != 409 {
		t.Fatalf("zero budget allowed helper: %d", code)
	}
	if code := c.do("PATCH", "/v1/tasks/"+task.ID, map[string]any{"maxNewAgents": 1}, &task); code != 200 || task.MaxNewAgents != 1 {
		t.Fatalf("update: %d %+v", code, task)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", request, nil); code != 201 {
		t.Fatalf("enabled budget: %d", code)
	}
	// An agent identity retained across an explicit project scope must not be
	// silently converted into an owner/manual registration.
	other := c.task("other-scope")
	crossScope := api.AddAgentRequest{Name: "cross", Host: "host", Session: "cross", ParentAgentID: parent.ID}
	if code := c.do("POST", "/v1/tasks/"+other.ID+"/agents", crossScope, nil); code != 400 {
		t.Fatalf("cross-project parent provenance = %d", code)
	}
	var agents api.AgentList
	c.do("GET", "/v1/tasks/"+other.ID+"/agents", nil, &agents)
	if len(agents.Agents) != 0 {
		t.Fatalf("cross-project request registered manually: %+v", agents.Agents)
	}
}

// TestAllocationIntentEndpoint exercises the HTTP surface (independent
// review #2300/#2771/#2840 finding 5, as clarified by #2844/#2850/#2867/#2870):
// a handler/lead can author a pre-admission allocation intent over the API,
// and a fresh parented member/extra admission is authorized by it.
func TestAllocationIntentEndpoint(t *testing.T) {
	c := newClient(t)
	var task api.Task
	if code := c.do("POST", "/v1/tasks", api.CreateTaskRequest{Name: "Allocation intent endpoint", Orchestrator: "lead", AllowAgentSpawn: true}, &task); code != 201 {
		t.Fatalf("create task: %d", code)
	}
	lead := c.agent(task, "lead")
	ctx := context.Background()
	item, err := c.st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Endpoint item", AgentID: lead.ID, RequestID: "endpoint-item"}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	order, err := c.st.PostMessage(ctx, task.ID, api.PostMessageRequest{
		Text: "order", RequestID: "endpoint-order",
		WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}},
	}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	orderRef := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	confirmHTTPContextOrder(t, c, task, item, order)
	agentID := api.NewID("agt")
	bundle := syntheticServerTestContext(t, item, orderRef, order)
	digestBytes := sha256.Sum256(bundle)
	digest := hex.EncodeToString(digestBytes[:])
	expectedRunID := api.NewID("run")
	var intent api.AllocationIntent
	req := api.CreateAllocationIntentRequest{
		AgentID: agentID, TargetTaskID: task.ID, ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef,
		ContextDigest: digest, TeamRole: api.TeamRoleMember, AuthorAgentID: lead.ID, AuthorRunID: lead.RunID, ExpectedRunID: expectedRunID,
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/allocation-intents", req, &intent); code != 201 || intent.AgentID != agentID || intent.TeamRole != api.TeamRoleMember {
		t.Fatalf("create allocation intent: %d %+v", code, intent)
	}
	// A second intent for the same agent identity is a conflict, not an
	// update -- authored once, never mutated.
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/allocation-intents", req, nil); code != 409 {
		t.Fatalf("re-authoring an intent should conflict: %d", code)
	}
	var admitted api.Agent
	addReq := api.AddAgentRequest{
		AgentID: agentID, Name: "worker", Host: "devbox", Session: "worker", Runtime: "claude", ParentAgentID: lead.ID,
		WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef, ContextBundle: bundle, TeamRole: api.TeamRoleMember},
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", addReq, &admitted); code != 201 || admitted.WorkItem == nil || admitted.WorkItem.TeamRole != api.TeamRoleMember || admitted.RunID != expectedRunID {
		t.Fatalf("admission authorized by the recorded intent, using its expected run id: %d %+v", code, admitted)
	}
	// Durable readback via GET, even after consumption.
	var readback api.AllocationIntent
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/allocation-intents/"+agentID, nil, &readback); code != 200 || readback.ConsumedAt == nil || readback.ConsumedByRunID != expectedRunID {
		t.Fatalf("GET allocation intent readback: %d %+v", code, readback)
	}
	// Missing intent for a different fresh agent identity is rejected.
	missing := addReq
	missing.AgentID = api.NewID("agt")
	missing.Name, missing.Session = "worker-2", "worker-2"
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", missing, nil); code != 409 {
		t.Fatalf("admission without a recorded intent should be rejected: %d", code)
	}
}

func syntheticServerTestContext(t *testing.T, item api.WorkItem, order api.MessageReference, orderMessage api.Message) []byte {
	t.Helper()
	revision := api.WorkItemRevision{
		ItemID: item.ID, TaskID: item.TaskID, Kind: item.Kind, Title: item.Title, Description: item.Description,
		Status: item.Status, Priority: item.Priority, ItemSeq: item.Seq, Revision: item.Revision,
		CreatedBy: item.CreatedBy, UpdatedBy: item.UpdatedBy, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		AttributionKind: "shared_workspace_claim", ChangeKind: "created", Provenance: "native",
	}
	link := api.WorkItemMessageLink{Message: orderMessage, RevisionCoverage: "verified", Relationship: "primary", ItemRevision: item.Revision}
	data, err := json.Marshal(map[string]any{
		"version": 1, "itemTaskId": item.TaskID, "itemId": item.ID, "itemRevision": item.Revision,
		"workOrderMessage": order,
		"history": map[string]any{
			"revision": revision, "revisions": []api.WorkItemRevision{revision}, "messages": []api.WorkItemMessageLink{link},
			"coverage": api.HistoryCoverage{Complete: true, ObservedCurrentRevision: item.Revision, LatestMaterialized: item.Revision, SnapshotCount: item.Revision, ConversationLinks: "explicit_only"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// getAs reads path as one agent run (or with no agent identity when agent is
// empty) and returns the status, the response headers and the decoded error.
func (c *client) getAs(agent, run, path string, out any) (int, http.Header, api.ErrorResponse) {
	req, err := http.NewRequest("GET", c.srv.URL+path, nil)
	if err != nil {
		c.t.Error(err)
		return 0, nil, api.ErrorResponse{}
	}
	if agent != "" {
		req.Header.Set(agentHeader, agent)
		req.Header.Set(runHeader, run)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Error(err)
		return 0, nil, api.ErrorResponse{}
	}
	defer res.Body.Close()
	var refusal api.ErrorResponse
	if res.StatusCode >= 400 {
		_ = json.NewDecoder(res.Body).Decode(&refusal)
	} else if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	// Drain so a flood reuses its connections instead of opening new ones.
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode, res.Header, refusal
}

// gateLoad reads one caller's in-flight and waiting counts.
func (c *client) gateLoad(key string) (inFlight, waiting int) {
	c.s.gate.mu.Lock()
	defer c.s.gate.mu.Unlock()
	if l := c.s.gate.callers[key]; l != nil {
		return l.inFlight, l.waiting
	}
	return 0, 0
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestCallerFloodLeavesOtherCallerServed(t *testing.T) {
	c := newClient(t)
	task := c.task("flood")
	flooder, other := c.agent(task, "flooder"), c.agent(task, "other")
	floodKey := "agent:" + flooder.ID + "/run_flood"
	var peak atomic.Int64
	c.s.admitted = func(key string, _ *http.Request) {
		if key != floodKey {
			return
		}
		inFlight, _ := c.gateLoad(key)
		for {
			seen := peak.Load()
			if int64(inFlight) <= seen || peak.CompareAndSwap(seen, int64(inFlight)) {
				break
			}
		}
		// Each flood read is slow, as on a busy database connection.
		time.Sleep(5 * time.Millisecond)
	}
	path := "/v1/tasks/" + task.ID + "/messages?after=0&limit=50"
	stop := make(chan struct{})
	var served, refused atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				code, _, refusal := c.getAs(flooder.ID, "run_flood", path, nil)
				switch {
				case code == 200:
					served.Add(1)
				case code == 429 && refusal.Code == callerBusyCode:
					refused.Add(1)
				default:
					t.Errorf("flood read: %d %+v", code, refusal)
					return
				}
			}
		}()
	}
	waitUntil(t, "the flood to fill its share", func() bool { return peak.Load() == agentCallerInFlight && refused.Load() > 0 })
	var slowest time.Duration
	for i := 0; i < 20; i++ {
		started := time.Now()
		code, _, refusal := c.getAs(other.ID, "run_other", path, nil)
		took := time.Since(started)
		slowest = max(slowest, took)
		if code != 200 || took > 2*time.Second {
			t.Fatalf("other caller request %d during the flood: %d %+v in %s", i, code, refusal, took)
		}
	}
	close(stop)
	wg.Wait()
	if got := peak.Load(); got != agentCallerInFlight {
		t.Fatalf("flood peak in flight = %d, want %d", got, agentCallerInFlight)
	}
	if served.Load() == 0 || refused.Load() == 0 {
		t.Fatalf("flood served %d, refused %d: want both", served.Load(), refused.Load())
	}
	if inFlight, waiting := c.gateLoad(floodKey); inFlight != 0 || waiting != 0 {
		t.Fatalf("flood left %d in flight and %d waiting", inFlight, waiting)
	}
	t.Logf("flood served %d and refused %d; the other caller's slowest request took %s", served.Load(), refused.Load(), slowest)
}

func TestCallerBusyRefusalNamesReason(t *testing.T) {
	c := newClient(t)
	task := c.task("busy")
	a := c.agent(task, "busy")
	key := "agent:" + a.ID + "/run_one"
	hold := make(chan struct{})
	c.s.admitted = func(admittedKey string, _ *http.Request) {
		if admittedKey == key {
			<-hold
		}
	}
	path := "/v1/tasks/" + task.ID + "/agents"
	codes := make(chan int, agentCallerInFlight+agentCallerWaiting)
	send := func() {
		code, _, _ := c.getAs(a.ID, "run_one", path, nil)
		codes <- code
	}
	for i := 0; i < agentCallerInFlight; i++ {
		go send()
	}
	waitUntil(t, "two requests in flight", func() bool { inFlight, _ := c.gateLoad(key); return inFlight == agentCallerInFlight })
	for i := 0; i < agentCallerWaiting; i++ {
		go send()
	}
	waitUntil(t, "eight requests waiting", func() bool { _, waiting := c.gateLoad(key); return waiting == agentCallerWaiting })
	if inFlight, _ := c.gateLoad(key); inFlight != agentCallerInFlight {
		t.Fatalf("in flight = %d with a full waiting room, want %d", inFlight, agentCallerInFlight)
	}
	code, header, refusal := c.getAs(a.ID, "run_one", path, nil)
	if code != 429 || refusal.Code != callerBusyCode || header.Get("Retry-After") != "1" ||
		refusal.Error != "this caller already has 10 hub requests in flight or waiting; run hub reads one at a time" {
		t.Fatalf("eleventh request: %d retry-after=%q %+v", code, header.Get("Retry-After"), refusal)
	}
	// Another run of the same agent is another caller.
	if code, _, refusal := c.getAs(a.ID, "run_two", path, nil); code != 200 {
		t.Fatalf("a different run of the agent: %d %+v", code, refusal)
	}
	close(hold)
	for i := 0; i < agentCallerInFlight+agentCallerWaiting; i++ {
		if code := <-codes; code != 200 {
			t.Fatalf("held or waiting request finished with %d", code)
		}
	}
	if n := len(c.s.gate.callers); n != 0 {
		t.Fatalf("%d idle callers kept after every request finished", n)
	}
}

func TestReservedLaneServedWhileAgentLaneFull(t *testing.T) {
	c := newClient(t)
	task := c.task("lanes")
	path := "/v1/tasks/" + task.ID + "/agents"
	hold := make(chan struct{})
	c.s.admitted = func(key string, _ *http.Request) {
		if strings.HasPrefix(key, "agent:") {
			<-hold
		}
	}
	held := agentLaneInFlight / agentCallerInFlight
	codes := make(chan int, agentLaneInFlight+1)
	var agents []api.Agent
	for i := 0; i <= held; i++ {
		agents = append(agents, c.agent(task, fmt.Sprintf("agent%d", i)))
	}
	for _, a := range agents[:held] {
		for i := 0; i < agentCallerInFlight; i++ {
			go func() {
				code, _, _ := c.getAs(a.ID, "run_held", path, nil)
				codes <- code
			}()
		}
	}
	lane := func() int {
		c.s.gate.mu.Lock()
		defer c.s.gate.mu.Unlock()
		return c.s.gate.agentInFlight
	}
	waitUntil(t, "every agent slot held", func() bool { return lane() == agentLaneInFlight })
	// One more agent run has room of its own but none in the lane: it waits.
	last := agents[held]
	lastKey := "agent:" + last.ID + "/run_late"
	go func() {
		code, _, _ := c.getAs(last.ID, "run_late", path, nil)
		codes <- code
	}()
	waitUntil(t, "a seventh agent request to wait", func() bool { _, waiting := c.gateLoad(lastKey); return waiting == 1 })
	within := func(what, agent, run, target string) {
		t.Helper()
		started := time.Now()
		code, _, refusal := c.getAs(agent, run, target, nil)
		if took := time.Since(started); code != 200 || took > 2*time.Second {
			t.Fatalf("%s with the agent lane full: %d %+v in %s", what, code, refusal, took)
		}
	}
	// A caller with no agent identity (the relay, the deployer watcher, the
	// owner) is served from the reserved lane.
	within("a request without agent headers", "", "", path)
	within("a team queue listing without agent headers", "", "", "/v1/tasks/"+task.ID+"/team-queue?view=active")
	// Events routes are long polls: they are never counted, even for an agent.
	within("global events from a waiting agent", last.ID, "run_late", "/v1/events?after=0")
	within("task events from a holding agent", agents[0].ID, "run_held", "/v1/tasks/"+task.ID+"/events?after=0")
	if got := lane(); got != agentLaneInFlight {
		t.Fatalf("agent lane holds %d after the exempt and reserved requests, want %d", got, agentLaneInFlight)
	}
	if inFlight, waiting := c.gateLoad("node:" + c.who.Node); inFlight != 0 || waiting != 0 {
		t.Fatalf("reserved lane kept %d in flight and %d waiting", inFlight, waiting)
	}
	close(hold)
	for i := 0; i < agentLaneInFlight+1; i++ {
		if code := <-codes; code != 200 {
			t.Fatalf("held or waiting agent request finished with %d", code)
		}
	}
}

func TestSingleMessageReadLoopRefused(t *testing.T) {
	c := newClient(t)
	task := c.task("history")
	a, b := c.agent(task, "looper"), c.agent(task, "bystander")
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/messages", api.PostMessageRequest{Text: "one"}, nil); code != 201 {
		t.Fatalf("post: %d", code)
	}
	if singleReadBurst != 60 || singleReadsPerSecond != 1 {
		t.Fatalf("single-read bound is %d in a burst refilling %d a second, want 60 and 1", singleReadBurst, singleReadsPerSecond)
	}
	// Stop the refill so a slow host cannot earn a read back mid-burst.
	c.s.singleReads.rate = 0
	single := "/v1/tasks/" + task.ID + "/messages?after=0&limit=1"
	for i := 1; i <= singleReadBurst; i++ {
		var page api.MessageList
		if code, _, refusal := c.getAs(a.ID, "run_loop", single, &page); code != 200 || len(page.Messages) != 1 {
			t.Fatalf("single read %d: %d %+v %+v", i, code, refusal, page)
		}
	}
	code, header, refusal := c.getAs(a.ID, "run_loop", single, nil)
	if code != 429 || refusal.Code != singleReadLoopCode || header.Get("Retry-After") != "1" ||
		!strings.Contains(refusal.Error, "tt inbox --board --after N --limit 200") {
		t.Fatalf("read 61 of the burst: %d retry-after=%q %+v", code, header.Get("Retry-After"), refusal)
	}
	// The paged read stays open to the refused caller, as do reads that are
	// not a walk of the Board: its own inbox and the newest message.
	for _, query := range []string{"after=0&limit=200", "after=0&limit=1&to=" + a.ID, "limit=1&latest=1"} {
		var page api.MessageList
		if code, _, refusal := c.getAs(a.ID, "run_loop", "/v1/tasks/"+task.ID+"/messages?"+query, &page); code != 200 || len(page.Messages) != 1 {
			t.Fatalf("read %q after the refusal: %d %+v %+v", query, code, refusal, page)
		}
	}
	// Another agent run, and a caller with no agent identity, have their own bound.
	if code, _, refusal := c.getAs(b.ID, "run_other", single, nil); code != 200 {
		t.Fatalf("another caller's single read: %d %+v", code, refusal)
	}
	if code, _, refusal := c.getAs("", "", single, nil); code != 200 {
		t.Fatalf("a single read without agent headers: %d %+v", code, refusal)
	}
}

func TestMessagesWithoutRecipientReturnWholeBoard(t *testing.T) {
	c := newClient(t)
	task := c.task("board")
	a, b, reader := c.agent(task, "a"), c.agent(task, "b"), c.agent(task, "reader")
	post := func(req api.PostMessageRequest) {
		t.Helper()
		if code := c.do("POST", "/v1/tasks/"+task.ID+"/messages", req, nil); code != 201 {
			t.Fatalf("post %q: %d", req.Text, code)
		}
	}
	post(api.PostMessageRequest{Text: "for everyone", AgentID: a.ID})
	post(api.PostMessageRequest{Text: "just for b", AgentID: a.ID, To: b.ID})
	post(api.PostMessageRequest{Text: "just for the reader", AgentID: a.ID, To: reader.ID})
	texts := func(query string) string {
		t.Helper()
		var page api.MessageList
		if code, _, refusal := c.getAs(reader.ID, "run_reader", "/v1/tasks/"+task.ID+"/messages?after=0&limit=200"+query, &page); code != 200 {
			t.Fatalf("read %q: %d %+v", query, code, refusal)
		}
		var out []string
		for _, m := range page.Messages {
			out = append(out, m.Text)
		}
		return strings.Join(out, "|")
	}
	if got := texts(""); got != "for everyone|just for b|just for the reader" {
		t.Fatalf("read without a recipient = %q, want the whole Board", got)
	}
	if got := texts("&to=" + reader.ID); got != "for everyone|just for the reader" {
		t.Fatalf("read with a recipient = %q, want the reader's own view", got)
	}
}

// A validation refusal that names its reason reaches an HTTP client with that
// reason; a bare one still answers "invalid request". Both are 400.
func TestValidationRefusalReasonReachesHTTPClient(t *testing.T) {
	c := newClient(t)
	task := c.task("validation-reason")
	add := api.TeamQueueRequest{RequestID: "add-comma", Operation: "add", ItemID: api.NewID("wi"), OrderMessageSeq: 1, Host: "mini", Cwd: "/tmp", Ownership: []string{"src/a.go,src/b.go"}}
	var refused api.ErrorResponse
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/team-queue/actions", add, &refused); code != http.StatusBadRequest {
		t.Fatalf("comma ownership add = %d, want 400", code)
	}
	if want := `invalid request: ownership path "src/a.go,src/b.go" contains a comma; give each path separately`; refused.Error != want || refused.Code != "" {
		t.Fatalf("comma ownership refusal = %+v, want %q", refused, want)
	}
	var list api.TeamQueueList
	if code := c.do("GET", "/v1/tasks/"+task.ID+"/team-queue", nil, &list); code != http.StatusOK || len(list.Entries) != 0 {
		t.Fatalf("refused add left entries: %d %+v", code, list.Entries)
	}
	// The same action without a host is refused by a bare api.ErrInvalid.
	add.RequestID, add.Host, add.Ownership = "add-no-host", "", []string{"src/a.go"}
	var bare api.ErrorResponse
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/team-queue/actions", add, &bare); code != http.StatusBadRequest || bare.Error != "invalid request" {
		t.Fatalf("bare refusal = %d %+v, want 400 invalid request", code, bare)
	}
}

// fail keeps only a reason that directly follows api.ErrInvalid; text put in
// front of it, and an error class with its own answer, are left as they were.
func TestFailInvalidRequestText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		want   string
	}{
		{"bare", api.ErrInvalid, 400, "invalid request"},
		{"reason", fmt.Errorf("%w: scope needs at least one owned path", api.ErrInvalid), 400, "invalid request: scope needs at least one owned path"},
		{"reason then suffix", fmt.Errorf("%w (entry tq_1)", fmt.Errorf("%w: unknown queue template", api.ErrInvalid)), 400, "invalid request: unknown queue template (entry tq_1)"},
		{"blank reason", fmt.Errorf("%w:  ", api.ErrInvalid), 400, "invalid request"},
		{"text in front", fmt.Errorf("read /private/state: %w", api.ErrInvalid), 400, "invalid request"},
		{"text in front of a reason", fmt.Errorf("decode: %w", fmt.Errorf("%w: reason", api.ErrInvalid)), 400, "invalid request"},
		{"joined", errors.Join(errors.New("other"), api.ErrInvalid), 400, "invalid request"},
		{"not found", fmt.Errorf("%w: no such entry", api.ErrNotFound), 404, "not found"},
		{"conflict", fmt.Errorf("%w: unresolved failed queue entry", api.ErrConflict), 409, "conflict: unresolved failed queue entry"},
		{"unknown", errors.New("disk full"), 500, "internal error"},
	} {
		rec := httptest.NewRecorder()
		fail(rec, tc.err)
		var got api.ErrorResponse
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if rec.Code != tc.status || got.Error != tc.want {
			t.Errorf("%s: %d %q, want %d %q", tc.name, rec.Code, got.Error, tc.status, tc.want)
		}
	}
}
