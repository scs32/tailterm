package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

type client struct {
	t   *testing.T
	srv *httptest.Server
	who api.Caller
}

func newClient(t *testing.T) *client {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &client{t: t, who: api.Caller{Node: "devbox", User: "stephen@example.com"}}
	s := New(st, func(r *http.Request) (api.Caller, error) {
		if r.Header.Get("X-Test-Deny") != "" {
			return api.Caller{}, fmt.Errorf("denied")
		}
		return c.who, nil
	})
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
	if code := c.do("POST", "/v1/tasks", api.CreateTaskRequest{Name: "bad name!"}, nil); code != 400 {
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
