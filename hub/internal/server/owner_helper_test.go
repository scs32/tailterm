package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestOwnerHelperRegisterAndDelegateHTTP(t *testing.T) {
	h := newTokenHub(t)
	task, builder, _ := h.project()
	base := "/v1/tasks/" + task.ID
	reg := api.RegisterOwnerHelperRequest{Host: "owner-mac", Session: "owner", Runtime: "claude", Cwd: "/work/tailterm", RequestID: "ohreg-1"}
	var first api.OwnerActionResult
	if code, _ := h.do(ownerToken, "POST", base+"/owner-helper", reg, &first); code != http.StatusCreated || first.Agent == nil || first.Agent.Role != api.AgentRoleOwnerHelper || first.Registration == nil || first.Registration.Mode != api.OwnerHelperCreated {
		t.Fatalf("register %d %+v", code, first)
	}
	var replay api.OwnerActionResult
	if code, _ := h.do(ownerToken, "POST", base+"/owner-helper", reg, &replay); code != http.StatusOK || !replay.Replay || replay.Agent.RunID != first.Agent.RunID {
		t.Fatalf("replay %d %+v", code, replay)
	}
	changed := reg
	changed.Session = "elsewhere"
	if code, _ := h.do(ownerToken, "POST", base+"/owner-helper", changed, nil); code != http.StatusConflict {
		t.Fatalf("reused request id %d", code)
	}
	for name, bad := range map[string]api.RegisterOwnerHelperRequest{
		"runtime": {Host: "h", Session: "owner", Runtime: "unsupported", RequestID: "bad-runtime"},
		"session": {Host: "h", Session: "no spaces", Runtime: "claude", RequestID: "bad-session"},
	} {
		if code, _ := h.do(ownerToken, "POST", base+"/owner-helper", bad, nil); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", name, code)
		}
	}
	// The Discord bridge credential may not register a helper.
	bridged := reg
	bridged.RequestID = "ohreg-bridge"
	if code, _ := h.do(bridgeToken, "POST", base+"/owner-helper", bridged, nil); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("bridge register %d", code)
	}
	again := reg
	again.RequestID = "ohreg-2"
	var second api.OwnerActionResult
	if code, _ := h.do(ownerToken, "POST", base+"/owner-helper", again, &second); code != http.StatusCreated || second.Agent.ID != first.Agent.ID || second.Registration.PreviousRunID != first.Agent.RunID {
		t.Fatalf("replace %d %+v", code, second)
	}
	helper := *second.Agent

	// A window for the helper by name routes an owner request; the helper answers with a rationale.
	open := api.OpenDelegationWindowRequest{Delegate: api.DefaultOwnerHelperName, EndsAt: time.Now().Add(3 * time.Hour), Scope: api.DelegationScopeDecisions, Source: &api.DelegationSource{Kind: "tt"}, RequestID: "open-helper"}
	var opened api.OwnerActionResult
	if code, _ := h.do(ownerToken, "POST", base+"/delegation-windows", open, &opened); code != http.StatusCreated || opened.Window.DelegateAgentID != helper.ID {
		t.Fatalf("open %d %+v", code, opened)
	}
	var item api.WorkItem
	if code, _ := h.do(ownerToken, "POST", base+"/work-items", api.CreateWorkItemRequest{Kind: "feature", Title: "Owner helper HTTP fixture", RequestID: "item"}, &item); code != 201 {
		t.Fatal(code)
	}
	var m api.Message
	req := api.PostMessageRequest{AgentID: builder.ID, RunID: builder.RunID, To: "owner", RequestID: "ask", Envelope: &api.Envelope{Kind: "request", To: "owner", Subject: "Choose the helper fixture rollout order", Body: api.EnvelopeBody{Ask: "Which?"}},
		WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}
	if code, _ := h.do(ownerToken, "POST", base+"/messages", req, &m); code != 201 {
		t.Fatalf("ask %d", code)
	}
	var list api.ObligationList
	h.do(ownerToken, "GET", base+"/obligations?owner=1&open=1", nil, &list)
	var o api.Obligation
	for _, ob := range list.Obligations {
		if ob.MessageSeq == m.Seq {
			o = ob
		}
	}
	answer := func(agent api.Agent, key, rationale string) int {
		code, _ := h.do(ownerToken, "POST", base+"/obligations/"+o.ID+"/answer", api.ObligationAnswerRequest{AgentID: agent.ID, RunID: agent.RunID, Text: "Staged first", Rationale: rationale, RequestID: key}, nil)
		return code
	}
	if code := answer(helper, "blank", ""); code != http.StatusBadRequest {
		t.Fatalf("no rationale %d", code)
	}
	if code := answer(*first.Agent, "old-run", "because"); code != http.StatusForbidden {
		t.Fatalf("old run %d", code)
	}
	if code := answer(helper, "ok", "Staged limits the blast radius."); code != http.StatusCreated {
		t.Fatalf("helper answer %d", code)
	}
}

func TestOwnerHelperRegisterRefusedWhilePaused(t *testing.T) {
	c := newClient(t)
	task := c.task("synthetic helper pause")
	lead := c.agent(task, "lead")
	base := "/v1/tasks/" + task.ID
	register := func(key string) (int, api.OwnerActionResult) {
		var out api.OwnerActionResult
		code := c.do("POST", base+"/owner-helper", api.RegisterOwnerHelperRequest{Host: "owner-mac", Session: "owner", Runtime: "claude", RequestID: key}, &out)
		return code, out
	}
	code, helper := register("before")
	if code != http.StatusCreated {
		t.Fatalf("register %d", code)
	}
	// The helper is a pause target like any agent; it owes no host cleanup.
	request := api.PauseProjectRequest{Version: 1, RequestID: "pause", Targets: []api.ProjectPauseTargetRequest{
		{AgentID: lead.ID, RunID: lead.RunID, ServiceDisposition: api.PauseServiceNone},
		{AgentID: helper.Agent.ID, RunID: helper.Agent.RunID, ServiceDisposition: api.PauseServiceNone}}}
	var paused api.ProjectPauseStatus
	if code := c.do("POST", base+"/pause", request, &paused); code != http.StatusOK || paused.State != api.ProjectPauseCleanupPending || paused.CleanupPending != 1 {
		t.Fatalf("pause %d %+v", code, paused)
	}
	if code, _ := register("cleanup-pending"); code != http.StatusConflict {
		t.Fatalf("cleanup_pending register %d", code)
	}
	if code := c.do("POST", base+"/agents/"+lead.ID+"/cleanup", api.CleanupRequest{RunID: lead.RunID}, nil); code != http.StatusOK {
		t.Fatalf("cleanup %d", code)
	}
	if code := c.do("GET", base+"/pause", nil, &paused); code != http.StatusOK || paused.State != api.ProjectPausePaused {
		t.Fatalf("paused %d %+v", code, paused)
	}
	if code, _ := register("paused"); code != http.StatusConflict {
		t.Fatalf("paused register %d", code)
	}
	planned := api.ProjectResumeOrchestrator{AgentID: api.NewID("agt"), RunID: api.NewID("run"), Name: "fresh-lead"}
	var resumed api.ProjectPauseStatus
	if code := c.do("POST", base+"/resume", api.ResumeProjectRequest{Version: 1, RequestID: "resume", ExpectedPauseGeneration: 1, ExpectedLifecycleGeneration: 1, RetainedHandoffDigest: paused.RetainedHandoffDigest, SelectedTeamID: "synthetic-team", Orchestrator: planned}, &resumed); code != http.StatusOK || resumed.State != api.ProjectPauseResuming {
		t.Fatalf("resume %d %+v", code, resumed)
	}
	if code, _ := register("resuming"); code != http.StatusConflict {
		t.Fatalf("resuming register %d", code)
	}
	admission := api.AddAgentRequest{AgentID: planned.AgentID, ExpectedRunID: planned.RunID, ResumeReceiptID: resumed.Receipt.ID, Name: planned.Name, Host: "fixture", Session: "fresh", ExpectedLifecycleGeneration: 2}
	var fresh api.Agent
	if code := c.do("POST", base+"/agents", admission, &fresh); code != http.StatusCreated {
		t.Fatalf("admission %d", code)
	}
	confirm := api.ConfirmProjectResumeRequest{Version: 1, RequestID: "confirm", ExpectedLifecycleGeneration: 2, ResumeReceiptID: resumed.Receipt.ID, AgentID: fresh.ID, RunID: fresh.RunID}
	if code := c.do("POST", base+"/resume/confirm", confirm, &resumed); code != http.StatusOK || resumed.State != api.ProjectPauseActive {
		t.Fatalf("confirm %d %+v", code, resumed)
	}
	// After resume the owner registers again: pause closed the old helper, so it is a fresh agent.
	code, after := register("after-resume")
	if code != http.StatusCreated || after.Agent.ID == helper.Agent.ID || after.Registration.Mode != api.OwnerHelperCreated {
		t.Fatalf("after resume %d %+v", code, after)
	}
}

// helperHTTPState is what a registration can change, read over HTTP and from
// the store's receipts, for the conditional registration's "no writes" checks.
type helperHTTPState struct {
	agents   []api.Agent
	receipts []string
	events   []int64
}

func helperHTTPStateOf(c *client, task string) helperHTTPState {
	c.t.Helper()
	var s helperHTTPState
	if code := c.do("GET", "/v1/tasks/"+task+"/agents", nil, &s.agents); code != http.StatusOK {
		c.t.Fatalf("agents %d", code)
	}
	for i := range s.agents {
		// Derived on read; the stored row is what must not move.
		s.agents[i].Online, s.agents[i].Unread = false, 0
	}
	list, err := c.st.ListOwnerHelperRegistrations(context.Background(), task)
	if err != nil {
		c.t.Fatal(err)
	}
	for _, r := range list {
		s.receipts = append(s.receipts, r.ID+" "+r.RunID+" "+r.PreviousRunID+" "+r.Host)
	}
	var events []api.Event
	if code := c.do("GET", "/v1/tasks/"+task+"/events?limit=1000", nil, &events); code != http.StatusOK {
		c.t.Fatalf("events %d", code)
	}
	for _, e := range events {
		s.events = append(s.events, e.Seq)
	}
	return s
}

func TestOwnerHelperExpectedRunHTTP(t *testing.T) {
	type fixture struct {
		c     *client
		task  api.Task
		base  string
		agent api.Agent
	}
	register := func(c *client, base, key, host, expected string) (int, api.OwnerActionResult, string) {
		c.t.Helper()
		// The body is decoded twice: a result on success, the error text on refusal.
		var raw json.RawMessage
		code := c.do("POST", base+"/owner-helper", api.RegisterOwnerHelperRequest{Host: host, Session: "owner", Runtime: "claude", RequestID: key, ExpectedRunID: expected}, &raw)
		var out api.OwnerActionResult
		var failed api.ErrorResponse
		_ = json.Unmarshal(raw, &out)
		_ = json.Unmarshal(raw, &failed)
		return code, out, failed.Error
	}
	start := func(t *testing.T) fixture {
		c := newClient(t)
		task := c.task("synthetic helper expected run")
		f := fixture{c: c, task: task, base: "/v1/tasks/" + task.ID}
		code, out, _ := register(c, f.base, "reg-1", "owner-mac", "")
		if code != http.StatusCreated || out.Agent == nil {
			t.Fatalf("register %d %+v", code, out)
		}
		f.agent = *out.Agent
		return f
	}
	current := func(f fixture) api.Agent {
		f.c.t.Helper()
		var a api.Agent
		if code := f.c.do("GET", f.base+"/agents/"+f.agent.ID, nil, &a); code != http.StatusOK {
			f.c.t.Fatalf("agent %d", code)
		}
		return a
	}
	// refused requires 409 with the reason in the response and nothing written.
	refused := func(f fixture, key, expected, reason string) {
		f.c.t.Helper()
		before := helperHTTPStateOf(f.c, f.task.ID)
		code, out, text := register(f.c, f.base, key, "owner-mac-2", expected)
		if code != http.StatusConflict || !strings.Contains(text, reason) || out.Agent != nil || out.Registration != nil {
			f.c.t.Fatalf("want 409 naming %q, got %d %q %+v", reason, code, text, out)
		}
		if after := helperHTTPStateOf(f.c, f.task.ID); !reflect.DeepEqual(before, after) {
			f.c.t.Fatalf("refusal (%s) wrote something:\nbefore %+v\nafter  %+v", reason, before, after)
		}
	}
	event := func(f fixture, kind string) {
		f.c.t.Helper()
		if code := f.c.do("POST", f.base+"/events", api.PostEventRequest{Kind: kind, AgentID: f.agent.ID, RunID: f.agent.RunID}, nil); code != http.StatusCreated {
			f.c.t.Fatalf("%s event %d", kind, code)
		}
	}
	retire := func(f fixture) {
		f.c.t.Helper()
		status := api.AgentRetired
		if code := f.c.do("PATCH", f.base+"/agents/"+f.agent.ID, api.UpdateAgentRequest{Status: &status}, nil); code != http.StatusOK {
			f.c.t.Fatalf("retire %d", code)
		}
	}
	closeHelper := func(f fixture) {
		f.c.t.Helper()
		if code := f.c.do("DELETE", f.base+"/agents/"+f.agent.ID+"?runId="+f.agent.RunID, nil, nil); code != http.StatusOK {
			f.c.t.Fatalf("close %d", code)
		}
	}

	t.Run("matching run registers", func(t *testing.T) {
		for _, kind := range []string{"", api.EventDone, api.EventNeedsInput} {
			f := start(t)
			want := api.AgentRunning
			if kind != "" {
				event(f, kind)
				want = kind
			}
			if got := current(f); got.Status != want {
				t.Fatalf("fixture status %s, want %s", got.Status, want)
			}
			code, out, _ := register(f.c, f.base, "restore-1", "owner-mac-2", f.agent.RunID)
			if code != http.StatusCreated || out.Agent == nil || out.Agent.ID != f.agent.ID || out.Agent.RunID == f.agent.RunID || out.Agent.Status != api.AgentRunning ||
				out.Registration == nil || out.Registration.PreviousRunID != f.agent.RunID || out.Registration.Mode != api.OwnerHelperReplaced {
				t.Fatalf("status %q: %d %+v", kind, code, out)
			}
			if got := current(f); got.RunID != out.Agent.RunID || got.Host != "owner-mac-2" {
				t.Fatalf("status %q: helper %+v", kind, got)
			}
		}
	})
	t.Run("a different run is 409", func(t *testing.T) {
		f := start(t)
		refused(f, "restore-1", api.NewID("run"), "run changed")
		if got := current(f); got.RunID != f.agent.RunID || got.Status != api.AgentRunning {
			t.Fatalf("helper %+v", got)
		}
	})
	t.Run("retired is 409", func(t *testing.T) {
		f := start(t)
		retire(f)
		refused(f, "restore-1", f.agent.RunID, "retired; only tt resume re-enables it")
	})
	t.Run("exited is 409", func(t *testing.T) {
		f := start(t)
		event(f, api.EventExited)
		refused(f, "restore-1", f.agent.RunID, "exited")
		if got := current(f); got.Status != api.AgentExited || got.RunID != f.agent.RunID {
			t.Fatalf("helper %+v", got)
		}
	})
	t.Run("closed is 409", func(t *testing.T) {
		f := start(t)
		closeHelper(f)
		refused(f, "restore-1", f.agent.RunID, "closed")
	})
	t.Run("a malformed run is 400", func(t *testing.T) {
		f := start(t)
		before := helperHTTPStateOf(f.c, f.task.ID)
		for _, bad := range []string{"run_1", "agt_0123456789abcdef", f.agent.RunID + "0"} {
			if code, _, _ := register(f.c, f.base, "bad-run", "owner-mac-2", bad); code != http.StatusBadRequest {
				t.Fatalf("%q: %d", bad, code)
			}
		}
		if after := helperHTTPStateOf(f.c, f.task.ID); !reflect.DeepEqual(before, after) {
			t.Fatalf("an invalid request wrote something")
		}
	})

	// The four interleavings of the store test, through HTTP. A read of the
	// helper's run R stands for a restore whose guards have passed.
	t.Run("i two restores started together", func(t *testing.T) {
		f := start(t)
		r := current(f).RunID
		type result struct {
			code int
			out  api.OwnerActionResult
			text string
		}
		results := make([]result, 2)
		begin := make(chan struct{})
		var wg sync.WaitGroup
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-begin
				var raw json.RawMessage
				// c.do is not used here: it may call t.Fatal, which a goroutine must not.
				body, _ := json.Marshal(api.RegisterOwnerHelperRequest{Host: "restore-host-" + string(rune('a'+i)), Session: "owner", Runtime: "claude", RequestID: "restore-" + string(rune('a'+i)), ExpectedRunID: r})
				res, err := http.Post(f.c.srv.URL+f.base+"/owner-helper", "application/json", bytes.NewReader(body))
				if err != nil {
					results[i].text = err.Error()
					return
				}
				defer res.Body.Close()
				_ = json.NewDecoder(res.Body).Decode(&raw)
				var failed api.ErrorResponse
				_ = json.Unmarshal(raw, &results[i].out)
				_ = json.Unmarshal(raw, &failed)
				results[i].code, results[i].text = res.StatusCode, failed.Error
			}()
		}
		close(begin)
		wg.Wait()
		winner, loser := -1, -1
		for i, got := range results {
			switch {
			case got.code == http.StatusCreated && got.out.Agent != nil:
				winner = i
			case got.code == http.StatusConflict && strings.Contains(got.text, "run changed"):
				loser = i
			default:
				t.Fatalf("restore %d: %d %q %+v", i, got.code, got.text, got.out)
			}
		}
		if winner < 0 || loser < 0 {
			t.Fatalf("want one new run and one refusal: %+v", results)
		}
		r2 := results[winner].out.Agent.RunID
		if r2 == r || results[winner].out.Registration.PreviousRunID != r {
			t.Fatalf("winner %+v", results[winner].out.Registration)
		}
		// The loser, sent again with R, is refused again.
		refused(f, "restore-"+string(rune('a'+loser)), r, "run changed")
		refused(f, "restore-again", r, "run changed")
		if got := current(f); got.RunID != r2 || got.Host != "restore-host-"+string(rune('a'+winner)) {
			t.Fatalf("after the race %+v", got)
		}
		if state := helperHTTPStateOf(f.c, f.task.ID); len(state.receipts) != 2 {
			t.Fatalf("receipts %+v", state.receipts)
		}
	})
	t.Run("ii retired in the gap", func(t *testing.T) {
		f := start(t)
		r := current(f).RunID
		retire(f)
		refused(f, "restore-1", r, "retired")
		if got := current(f); got.Status != api.AgentRetired || got.RunID != r {
			t.Fatalf("helper %+v", got)
		}
	})
	t.Run("iii another host registers in the gap", func(t *testing.T) {
		f := start(t)
		r := current(f).RunID
		code, won, _ := register(f.c, f.base, "other-host", "other-host", "")
		if code != http.StatusCreated {
			t.Fatalf("other host %d", code)
		}
		refused(f, "restore-1", r, "run changed")
		got := current(f)
		state := helperHTTPStateOf(f.c, f.task.ID)
		if got.RunID != won.Agent.RunID || got.Host != "other-host" || len(state.receipts) != 2 || !strings.HasPrefix(state.receipts[0], won.Registration.ID+" ") {
			t.Fatalf("the other registration does not stand: %+v %+v", got, state.receipts)
		}
	})
	t.Run("iv closed in the gap", func(t *testing.T) {
		f := start(t)
		r := current(f).RunID
		closeHelper(f)
		before := len(helperHTTPStateOf(f.c, f.task.ID).agents)
		refused(f, "restore-1", r, "closed")
		if after := len(helperHTTPStateOf(f.c, f.task.ID).agents); after != before {
			t.Fatalf("agents %d -> %d", before, after)
		}
	})
}
