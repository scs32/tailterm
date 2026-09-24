package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

func withStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	_, _ = w.WriteString(input)
	w.Close()
	defer func() { os.Stdin = old }()
	fn()
}

// b9 and b11 from the CLI: the stop hook blocks only on unacknowledged
// obligations, tt ack clears it, and tt obligations lists what is owed.
func TestObligationCommandsAndStopHook(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
	ctx := context.Background()
	self, err := c.GetAgent(ctx, task.ID, e.agent)
	if err != nil {
		t.Fatal(err)
	}
	e.runID = self.RunID
	assign, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: e.agent, Envelope: &api.Envelope{
		Kind: "assign", To: e.agentName, Subject: "Record the order for the builder",
		Body: api.EnvelopeBody{Objective: "save it", Owns: []string{"records"}, Acceptance: map[string]string{"a1": "saved"}}}})
	if err != nil {
		t.Fatal(err)
	}
	hook := func(input string) string {
		t.Helper()
		var out string
		withStdin(t, input, func() {
			out, err = captureCLIOutput(t, func() error { return cmdHook(e, []string{"stop"}) })
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := hook(`{}`); !strings.Contains(out, `"block"`) || !strings.Contains(out, "unacknowledged") {
		t.Fatalf("stop hook did not block on an unacknowledged obligation: %q", out)
	}
	if out := hook(`{"stop_hook_active":true}`); strings.Contains(out, "block") {
		t.Fatalf("stop hook ignored stop_hook_active: %q", out)
	}
	listed, err := captureCLIOutput(t, func() error { return cmdObligations(e, nil) })
	if err != nil || !strings.Contains(listed, "Record the order for the builder") || !strings.Contains(listed, "delivered") {
		t.Fatalf("tt obligations: %q %v", listed, err)
	}
	if out, err := captureCLIOutput(t, func() error { return cmdObligationAction(e, "ack", []string{"#" + itoa(assign.Seq)}) }); err != nil || !strings.Contains(out, "acknowledged") {
		t.Fatalf("tt ack: %q %v", out, err)
	}
	// Acknowledged work does not hold the turn open, even though the inbox
	// cursor has not moved past it (round-one F12).
	if out := hook(`{}`); strings.Contains(out, "block") {
		t.Fatalf("stop hook blocked on acknowledged work: %q", out)
	}
	// Board-wide chatter never blocks; directed free text from an agent does.
	if _, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, Text: "board-wide status note"}); err != nil {
		t.Fatal(err)
	}
	if out := hook(`{}`); strings.Contains(out, "block") {
		t.Fatalf("board-wide message blocked the turn: %q", out)
	}
	if _, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: e.agent, Text: "quick question for you"}); err != nil {
		t.Fatal(err)
	}
	if out := hook(`{}`); !strings.Contains(out, "addressed to you") {
		t.Fatalf("directed free text did not block: %q", out)
	}
	if out, err := captureCLIOutput(t, func() error {
		return cmdObligationAction(e, "progress", []string{itoa(assign.Seq), "--text", "halfway"})
	}); err != nil || !strings.Contains(out, "working") {
		t.Fatalf("tt progress: %q %v", out, err)
	}
	// Another agent cannot acknowledge it.
	other := e
	other.agent, other.runID = lead.ID, lead.RunID
	if err := cmdObligationAction(other, "ack", []string{itoa(assign.Seq)}); err == nil {
		t.Fatal("lead acknowledged the builder's obligation")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// b8 through the relay: a broker wake is leased, queued through the runtime
// and reported; a failed queue is reported and the obligation stays undelivered.
func TestRelayDeliversBrokerWakeJobs(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
	ctx := context.Background()
	self, err := c.GetAgent(ctx, task.ID, e.agent)
	if err != nil {
		t.Fatal(err)
	}
	b := runtimeBinding{Hub: e.hub, Task: task.ID, Agent: e.agent, Run: self.RunID, Thread: "00000000-0000-0000-0000-000000000000", Codex: "codex"}
	if _, err := c.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: e.agent, RunID: self.RunID}); err != nil {
		t.Fatal(err)
	}
	m, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: e.agent, Envelope: &api.Envelope{
		Kind: "request", To: e.agentName, Subject: "Save the governing order record", Body: api.EnvelopeBody{Ask: "save it"}}})
	if err != nil {
		t.Fatal(err)
	}
	var prompts []string
	failing := func(context.Context, runtimeBinding, string) error {
		return errors.New("Codex queue failed: exit status 1")
	}
	// A failed wake reports handled=false so the other relay paths still run.
	if handled, err := relayWakeJob(ctx, b, &relayProgress{}, c, time.Now(), failing); handled || err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("failed wake: handled %v err %v", handled, err)
	}
	list, _ := c.ListObligations(ctx, task.ID, "", "", true, false)
	if len(list) != 1 || list[0].State != api.ObligationQueued {
		t.Fatalf("a failed wake must not deliver: %+v", list)
	}
	// Nothing further is due until the broker schedules the next wake.
	ok := func(_ context.Context, _ runtimeBinding, p string) error { prompts = append(prompts, p); return nil }
	if handled, err := relayWakeJob(ctx, b, &relayProgress{}, c, time.Now(), ok); handled || err != nil {
		t.Fatalf("no wake should be due: handled %v err %v", handled, err)
	}
	if _, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: e.agent, Envelope: &api.Envelope{
		Kind: "notice", To: e.agentName, Subject: "Integration window closes today", Body: api.EnvelopeBody{Text: "rebase"}}}); err != nil {
		t.Fatal(err)
	}
	if handled, err := relayWakeJob(ctx, b, &relayProgress{}, c, time.Now(), ok); !handled || err != nil || len(prompts) != 1 || !strings.Contains(prompts[0], "#"+itoa(m.Seq)) {
		t.Fatalf("accepted wake: handled %v err %v prompts %q", handled, err, prompts)
	}
	list, _ = c.ListObligations(ctx, task.ID, "", "", false, false)
	for _, o := range list {
		if (o.Needs == api.ObligationNeedsDelivery && o.State != api.ObligationClosed) || (o.Needs != api.ObligationNeedsDelivery && o.State != api.ObligationDelivered) {
			t.Fatalf("after accepted wake: %+v", o)
		}
	}
}

// Round one F14/f9: broker notices never trigger roster-wide inbox wakes, and
// broker wakes are spaced so the other relay paths keep their turns.
func TestRelayBrokerFairness(t *testing.T) {
	broker := api.Message{Seq: 5, From: api.Sender{Node: api.BrokerNode, User: "broker"}, To: "", Text: "Project stalled"}
	human := api.Message{Seq: 6, From: api.Sender{User: "owner"}, To: "", Text: "owner announcement"}
	if _, eligible := wakeThrough([]api.Message{broker}, "self"); eligible {
		t.Fatal("a board-wide broker notice woke the roster")
	}
	if _, eligible := wakeThrough([]api.Message{human}, "self"); !eligible {
		t.Fatal("owner announcements must still wake")
	}
	p := &relayProgress{LastBrokerWake: time.Now()}
	called := false
	if handled, err := relayWakeJob(context.Background(), runtimeBinding{}, p, nil, time.Now().Add(5*time.Second), func(context.Context, runtimeBinding, string) error { called = true; return nil }); handled || err != nil || called {
		t.Fatalf("broker wake inside the spacing window: handled %v err %v called %v", handled, err, called)
	}
}

// Round-two focused fix N1/N2 (Codex, Fable): directed messages that create no
// obligation still wake their recipient through the inbox path, and a page of
// broker-covered messages never freezes the relay cursor.
func TestRelayStillWakesNonObligatingDirectedMessages(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "cli-test", User: "owner"}
	ctx := context.Background()
	task, _ := st.CreateTask(ctx, api.CreateTaskRequest{Name: "P", Orchestrator: "lead"}, by)
	lead, _ := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "h", Session: "lead", Runtime: "codex"}, by)
	worker, _ := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "h", Session: "worker", Runtime: "codex"}, by)
	srv := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	t.Cleanup(srv.Close)
	c, _ := api.NewClient(srv.URL, 0)
	relay := func(a api.Agent) bool {
		t.Helper()
		if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: a.ID, RunID: a.RunID}, by); err != nil {
			t.Fatal(err)
		}
		b := runtimeBinding{Hub: srv.URL, Task: task.ID, Agent: a.ID, Run: a.RunID, Thread: "00000000-0000-0000-0000-000000000000", Codex: "/bin/codex"}
		p := &relayProgress{Run: a.RunID, Thread: b.Thread, BrokerWakes: true}
		called := false
		if err := relayOne(ctx, b, p, c, time.Now(), func(context.Context, runtimeBinding, string) error { called = true; return nil }); err != nil {
			t.Fatal(err)
		}
		return called
	}
	q, err := st.CreateDecision(ctx, task.ID, api.CreateDecisionRequest{AgentID: worker.ID, RequestID: "d1", DecisionRequest: api.DecisionRequest{
		Question: "Ship it?", Options: []api.DecisionOption{{ID: "yes", Label: "Yes", Description: "Ship"}, {ID: "no", Label: "No", Description: "Hold"}}, RecommendedOptionID: "yes", RecommendationReason: "ok"}}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AnswerDecision(ctx, task.ID, q.Seq, api.AnswerDecisionRequest{RequestID: "a1", OptionID: "yes"}, by); err != nil {
		t.Fatal(err)
	}
	if !relay(worker) {
		t.Fatal("a decision answer no longer wakes the asking agent")
	}
	if _, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: worker.ID, RunID: worker.RunID, To: lead.ID, Envelope: &api.Envelope{Kind: "result", To: "lead", Subject: "Finished the rebase onto release root",
		Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}}, Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "go test", Outcome: "ok"}}}}, by); err != nil {
		t.Fatal(err)
	}
	if !relay(lead) {
		t.Fatal("a typed result no longer wakes the lead")
	}
	// 200 broker-covered assignments, then directed agent free text: the
	// relay must look past the covered page.
	reviewer, _ := st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "reviewer", Host: "h", Session: "reviewer", Runtime: "codex"}, by)
	for i := 0; i < 200; i++ {
		if _, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: reviewer.ID, Envelope: &api.Envelope{Kind: "assign", To: "reviewer", Subject: "Review candidate number " + strconv.Itoa(i),
			Body: api.EnvelopeBody{Objective: "x", Owns: []string{"f"}, Acceptance: map[string]string{"a1": "y"}}}}, by); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: reviewer.ID, Text: "quick question for you"}, by); err != nil {
		t.Fatal(err)
	}
	b := runtimeBinding{Hub: srv.URL, Task: task.ID, Agent: reviewer.ID, Run: reviewer.RunID, Thread: "00000000-0000-0000-0000-000000000000", Codex: "/bin/codex"}
	p := &relayProgress{Run: reviewer.RunID, Thread: b.Thread, BrokerWakes: true}
	if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: reviewer.ID, RunID: reviewer.RunID}, by); err != nil {
		t.Fatal(err)
	}
	woke := false
	for i := 0; i < 3 && !woke; i++ {
		p.LastAttempt = time.Time{}
		if err := relayOne(ctx, b, p, c, time.Now(), func(context.Context, runtimeBinding, string) error { woke = true; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if !woke {
		t.Fatal("free text after a page of broker-covered messages never woke the agent")
	}
}

// Round-two focused fix B17: a failed broker wake leaves the other paths to run.
func TestFailedBrokerWakeDoesNotSuppressFallback(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
	ctx := context.Background()
	self, _ := c.GetAgent(ctx, task.ID, e.agent)
	if _, err := c.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: e.agent, RunID: self.RunID}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: e.agent, Envelope: &api.Envelope{Kind: "request", To: e.agentName, Subject: "Save the governing order record", Body: api.EnvelopeBody{Ask: "x"}}}); err != nil {
		t.Fatal(err)
	}
	b := runtimeBinding{Hub: e.hub, Task: task.ID, Agent: e.agent, Run: self.RunID, Thread: "00000000-0000-0000-0000-000000000000", Codex: "codex"}
	handled, err := relayWakeJob(ctx, b, &relayProgress{}, c, time.Now(), func(context.Context, runtimeBinding, string) error {
		return errors.New("Codex queue failed: exit status 1")
	})
	if handled || err == nil {
		t.Fatalf("failed wake must report handled=false so fallback runs: handled %v err %v", handled, err)
	}
}

// Round-two focused fix N3: the stop-hook fallback reads past the first page.
func TestStopHookFallbackReadsAllUnreadPages(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
	ctx := context.Background()
	self, _ := c.GetAgent(ctx, task.ID, e.agent)
	e.runID = self.RunID
	old := unreadPageSize
	unreadPageSize = 5
	t.Cleanup(func() { unreadPageSize = old })
	for i := 0; i < 12; i++ {
		if _, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, Text: "board note " + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: e.agent, Text: "directed question after the board noise"}); err != nil {
		t.Fatal(err)
	}
	if n := unreadDirectedFreeText(e); n != 1 {
		t.Fatalf("unread directed free text = %d, want 1", n)
	}
}

// Round-two focused fix B15: tt ack and tt progress send request IDs; progress
// IDs change each minute so later progress still counts.
func TestObligationRequestIDs(t *testing.T) {
	now := time.Date(2026, 9, 24, 10, 0, 5, 0, time.UTC)
	ack1 := obligationRequestID("run_1", "ack", 12, "", now)
	if ack1 == "" || ack1 != obligationRequestID("run_1", "ack", 12, "", now.Add(30*time.Second)) {
		t.Fatal("an ack retried within the minute must reuse its request ID")
	}
	// Focused verification R1: a later ack (resuming a block) must act again.
	if ack1 == obligationRequestID("run_1", "ack", 12, "", now.Add(2*time.Minute)) {
		t.Fatal("a later ack reused the earlier request ID and could never resume a block")
	}
	p1 := obligationRequestID("run_1", "progress", 12, "halfway", now)
	if p1 != obligationRequestID("run_1", "progress", 12, "halfway", now.Add(30*time.Second)) || p1 == obligationRequestID("run_1", "progress", 12, "halfway", now.Add(2*time.Minute)) {
		t.Fatal("progress request IDs must dedupe within a minute and differ across minutes")
	}
}

// Focused verification R1 end to end: ack, block, then a later tt ack resumes.
func TestAckResumesABlockLater(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
	ctx := context.Background()
	self, _ := c.GetAgent(ctx, task.ID, e.agent)
	e.runID = self.RunID
	m, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: e.agent, Envelope: &api.Envelope{Kind: "request", To: e.agentName, Subject: "Save the governing order record", Body: api.EnvelopeBody{Ask: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	ack := func(at time.Time) api.Obligation {
		t.Helper()
		o, err := c.ObligationAction(ctx, task.ID, m.Seq, "ack", api.ObligationActionRequest{AgentID: e.agent, RunID: e.runID, RequestID: obligationRequestID(e.runID, "ack", m.Seq, "", at)})
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	start := time.Now()
	ack(start)
	if _, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: e.agent, RunID: e.runID, To: lead.ID, ReplyTo: m.Seq, Envelope: &api.Envelope{Kind: "block", To: "lead", Subject: "Waiting on the order number",
		Body: api.EnvelopeBody{Reason: "no order yet", Needs: "lead", ResumeWhen: "order posted"}}}); err != nil {
		t.Fatal(err)
	}
	if o := ack(start.Add(3 * time.Minute)); o.State != api.ObligationAcknowledged {
		t.Fatalf("a later ack did not resume the block: %+v", o)
	}
}
