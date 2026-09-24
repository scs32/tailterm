package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

// Broker phase 2b hub changes (docs/broker-phase-2b.md d1–d4).

const (
	ownerToken  = "owner-token-0123456789abcdef0123456789"
	bridgeToken = "bridge-token-0123456789abcdef012345678"
)

type tokenHub struct {
	t   *testing.T
	st  *store.Store
	url string
}

func newTokenHub(t *testing.T) tokenHub {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	identity, err := TokenIdentities(map[string]api.Caller{
		ownerToken:  {Node: "workspace", User: "owner"},
		bridgeToken: {Node: api.BridgeNode, User: "owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, identity))
	t.Cleanup(srv.Close)
	return tokenHub{t: t, st: st, url: srv.URL}
}

func (h tokenHub) do(token, method, path string, body, out any) (int, http.Header) {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			h.t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, h.url+path, &buf)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode, res.Header
}

// fixture: a project with a lead and a builder, and one owner assignment
// directed at the builder, which creates an ack_outcome obligation.
func (h tokenHub) project() (api.Task, api.Agent, api.Obligation) {
	h.t.Helper()
	ctx := context.Background()
	owner := api.Caller{Node: "workspace", User: "owner"}
	task, err := h.st.CreateTask(ctx, api.CreateTaskRequest{Name: "Bridge", Orchestrator: "lead"}, owner)
	if err != nil {
		h.t.Fatal(err)
	}
	var agents []api.Agent
	for _, name := range []string{"lead", "builder"} {
		a, err := h.st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Host: "mini", Session: "tt-" + name, Runtime: "codex"}, owner)
		if err != nil {
			h.t.Fatal(err)
		}
		agents = append(agents, a)
	}
	m, err := h.st.PostMessage(ctx, task.ID, api.PostMessageRequest{To: agents[1].ID, Text: "please rebase and report"}, owner)
	if err != nil {
		h.t.Fatal(err)
	}
	obligations, err := h.st.ListObligations(ctx, task.ID, store.ObligationFilter{}, time.Now())
	if err != nil || len(obligations) != 1 || obligations[0].MessageSeq != m.Seq {
		h.t.Fatalf("fixture obligation: %v %+v", err, obligations)
	}
	return task, agents[1], obligations[0]
}

// d1: the bridge token reaches only its routes; the owner token is unchanged.
func TestBridgeTokenIsRouteLimited(t *testing.T) {
	h := newTokenHub(t)
	task, builder, _ := h.project()
	base := "/v1/tasks/" + task.ID
	for _, ok := range []struct{ method, path string }{
		{"GET", "/v1/tasks"}, {"GET", base}, {"GET", base + "/agents"}, {"GET", base + "/messages"},
		{"GET", base + "/obligations"}, {"GET", "/v1/whoami"},
	} {
		if code, _ := h.do(bridgeToken, ok.method, ok.path, nil, nil); code != 200 {
			t.Errorf("bridge %s %s = %d, want 200", ok.method, ok.path, code)
		}
	}
	for _, denied := range []struct{ method, path string }{
		{"POST", "/v1/tasks"}, {"PATCH", base}, {"DELETE", base}, {"POST", base + "/pause"},
		{"POST", base + "/agents"}, {"PATCH", base + "/agents/" + builder.ID}, {"DELETE", base + "/agents/" + builder.ID},
		{"POST", base + "/messages/read"}, {"POST", base + "/work-items"}, {"GET", base + "/queue"},
		{"POST", base + "/decisions"}, {"GET", "/v1/nonexistent"},
	} {
		if code, _ := h.do(bridgeToken, denied.method, denied.path, map[string]any{}, nil); code != http.StatusForbidden {
			t.Errorf("bridge %s %s = %d, want 403", denied.method, denied.path, code)
		}
	}
	if code, _ := h.do("wrong-token-0123456789abcdef0123456789", "GET", "/v1/tasks", nil, nil); code != http.StatusForbidden {
		t.Errorf("wrong token = %d, want 403", code)
	}
	if code, _ := h.do("", "GET", "/v1/tasks", nil, nil); code != http.StatusForbidden {
		t.Errorf("no token = %d, want 403", code)
	}
	var who api.Caller
	if code, _ := h.do(bridgeToken, "GET", "/v1/whoami", nil, &who); code != 200 || who.Node != api.BridgeNode {
		t.Errorf("bridge whoami = %d %+v", code, who)
	}
	// The owner token still reaches everything, including routes the bridge cannot.
	if code, _ := h.do(ownerToken, "PATCH", base, api.UpdateTaskRequest{}, nil); code != 200 {
		t.Errorf("owner PATCH task = %d, want 200", code)
	}
	if _, err := TokenIdentities(map[string]api.Caller{"short": {}}); err == nil {
		t.Error("a short token was accepted")
	}
}

// d2: bridged posts carry their Discord source; retries are idempotent.
func TestBridgePostsRecordTheirSource(t *testing.T) {
	h := newTokenHub(t)
	task, builder, _ := h.project()
	path := "/v1/tasks/" + task.ID + "/messages"
	src := &api.MessageSource{Kind: api.SourceDiscord, ID: "1552740000000000001", UserID: "709503984238592120"}
	req := api.PostMessageRequest{To: builder.ID, Text: "from my phone: status please", RequestID: "discord-msg-1552740000000000001", Source: src}

	var first, again api.Message
	if code, _ := h.do(bridgeToken, "POST", path, req, &first); code != 201 {
		t.Fatalf("bridge post = %d", code)
	}
	if first.Source == nil || *first.Source != *src || first.From.Node != api.BridgeNode || first.From.AgentID != "" {
		t.Fatalf("bridged message = %+v", first)
	}
	if code, _ := h.do(bridgeToken, "POST", path, req, &again); code != 201 || again.Seq != first.Seq {
		t.Fatalf("retry = %d seq %d, want the original %d", code, again.Seq, first.Seq)
	}
	obligations, _ := h.st.ListObligations(context.Background(), task.ID, store.ObligationFilter{}, time.Now())
	count := 0
	for _, o := range obligations {
		if o.MessageSeq == first.Seq {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("a retried bridged post made %d obligations, want 1", count)
	}
	// The same Discord message under another request ID is refused, not duplicated.
	dup := req
	dup.RequestID = "discord-msg-other"
	if code, _ := h.do(bridgeToken, "POST", path, dup, nil); code != http.StatusConflict {
		t.Errorf("same source, new request ID = %d, want 409", code)
	}
	// The source survives a read.
	var page api.MessageList
	h.do(ownerToken, "GET", path+"?after=0", nil, &page)
	found := false
	for _, m := range page.Messages {
		if m.Seq == first.Seq {
			found = m.Source != nil && m.Source.ID == src.ID
		}
	}
	if !found {
		t.Error("the listed message lost its source")
	}
	for name, bad := range map[string]struct {
		token string
		req   api.PostMessageRequest
	}{
		"owner sets a source":     {ownerToken, api.PostMessageRequest{Text: "x", Source: src}},
		"bridge omits the source": {bridgeToken, api.PostMessageRequest{Text: "x"}},
		"bridge speaks as agent":  {bridgeToken, api.PostMessageRequest{Text: "x", AgentID: builder.ID, Source: &api.MessageSource{Kind: "discord", ID: "2", UserID: "3"}}},
		"bridge sets a run":       {bridgeToken, api.PostMessageRequest{Text: "x", RunID: builder.RunID, Source: &api.MessageSource{Kind: "discord", ID: "4", UserID: "3"}}},
		"non-numeric source ID":   {bridgeToken, api.PostMessageRequest{Text: "x", Source: &api.MessageSource{Kind: "discord", ID: "abc", UserID: "3"}}},
		"another source kind":     {bridgeToken, api.PostMessageRequest{Text: "x", Source: &api.MessageSource{Kind: "slack", ID: "5", UserID: "3"}}},
	} {
		if code, _ := h.do(bad.token, "POST", path, bad.req, nil); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
}

// d3: the owner nudge queues one immediate wake, rate-limited per obligation.
func TestOwnerNudge(t *testing.T) {
	h := newTokenHub(t)
	task, builder, o := h.project()
	now := time.Now().UTC()
	h.st.SetClockForTest(func() time.Time { return now })
	path := "/v1/tasks/" + task.ID + "/obligations/" + o.ID + "/nudge"

	var result api.ObligationNudgeResult
	if code, _ := h.do(bridgeToken, "POST", path, nil, &result); code != http.StatusCreated || result.WakeJobID == "" || result.Obligation.ID != o.ID {
		t.Fatalf("nudge = %d %+v", code, result)
	}
	if _, err := h.st.PostEvent(context.Background(), task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: builder.ID, RunID: builder.RunID}, api.Caller{Node: "mini", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	job, err := h.st.LeaseWakeJob(context.Background(), task.ID, builder.ID, builder.RunID, now)
	if err != nil || job == nil || job.ObligationID != o.ID {
		t.Fatalf("the nudge's wake job is not due now: %+v %v", job, err)
	}
	now = now.Add(time.Minute)
	code, header := h.do(ownerToken, "POST", path, nil, nil)
	if code != http.StatusTooManyRequests || header.Get("Retry-After") == "" {
		t.Fatalf("second nudge within 2 min = %d retry-after %q", code, header.Get("Retry-After"))
	}
	now = now.Add(api.ObligationNudgeInterval)
	if code, _ := h.do(ownerToken, "POST", path, nil, nil); code != http.StatusCreated {
		t.Fatalf("nudge after the interval = %d", code)
	}
	if code, _ := h.do(ownerToken, "POST", "/v1/tasks/"+task.ID+"/obligations/obl_0000000000000000/nudge", nil, nil); code != http.StatusNotFound {
		t.Errorf("unknown obligation = %d, want 404", code)
	}
	// A closed obligation conflicts and queues nothing.
	if _, err := h.st.ReassignObligation(context.Background(), task.ID, o.ID, api.ObligationReassignRequest{ToAgentID: h.agentNamed(task, "lead").ID, Reason: "test"}, api.Caller{Node: "workspace", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(api.ObligationNudgeInterval)
	if code, _ := h.do(ownerToken, "POST", path, nil, nil); code != http.StatusConflict {
		t.Errorf("nudge of a closed obligation = %d, want 409", code)
	}
	// The bridge reassigns only as the owner.
	fresh, _ := h.st.ListObligations(context.Background(), task.ID, store.ObligationFilter{}, now)
	for _, ob := range fresh {
		if ob.State != api.ObligationClosed {
			body := api.ObligationReassignRequest{ToAgentID: builder.ID, ActorAgentID: ob.AgentID, ActorRunID: "run_0000000000000000"}
			if code, _ := h.do(bridgeToken, "POST", "/v1/tasks/"+task.ID+"/obligations/"+ob.ID+"/reassign", body, nil); code != http.StatusBadRequest {
				t.Errorf("bridge reassign with an actor = %d, want 400", code)
			}
		}
	}
}

func (h tokenHub) agentNamed(task api.Task, name string) api.Agent {
	h.t.Helper()
	agents, err := h.st.ListAgents(context.Background(), task.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, a := range agents {
		if a.Name == name {
			return a
		}
	}
	h.t.Fatalf("no agent %s", name)
	return api.Agent{}
}

// d4: escalation notices say which level they are.
func TestEscalationNoticesCarryTheirLevel(t *testing.T) {
	h := newTokenHub(t)
	task, _, _ := h.project()
	ctx := context.Background()
	now := time.Now().UTC().Add(time.Hour)
	for level, want := range map[int]string{1: "lead", 2: "owner"} {
		open, err := h.st.BrokerOpenObligations(ctx)
		if err != nil || len(open) == 0 {
			t.Fatalf("open obligations: %v %d", err, len(open))
		}
		var target store.BrokerObligation
		for _, o := range open {
			if o.TaskID == task.ID && o.SourceKind == "human" {
				target = o
			}
		}
		if err := h.st.BrokerEscalate(ctx, target, level, "not acknowledged", now); err != nil {
			t.Fatal(err)
		}
		messages, err := h.st.ListMessages(ctx, task.ID, 0, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		last := messages[len(messages)-1]
		if last.Envelope == nil || last.Envelope.Refs["escalation"] != want || last.Envelope.Refs["obligation"] != target.ID {
			t.Errorf("level %d notice refs = %+v, want escalation=%s", level, last.Envelope, want)
		}
	}
}
