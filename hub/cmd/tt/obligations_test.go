package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
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
	// Acknowledged work, even when later overdue, does not hold the turn open.
	if out := hook(`{}`); strings.Contains(out, "unacknowledged") {
		t.Fatalf("stop hook blocked on acknowledged work: %q", out)
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
	m, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: e.agent, Envelope: &api.Envelope{
		Kind: "request", To: e.agentName, Subject: "Save the governing order record", Body: api.EnvelopeBody{Ask: "save it"}}})
	if err != nil {
		t.Fatal(err)
	}
	var prompts []string
	failing := func(context.Context, runtimeBinding, string) error {
		return errors.New("Codex queue failed: exit status 1")
	}
	if handled, err := relayWakeJob(ctx, b, c, failing); !handled || err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("failed wake: handled %v err %v", handled, err)
	}
	list, _ := c.ListObligations(ctx, task.ID, "", "", true, false)
	if len(list) != 1 || list[0].State != api.ObligationQueued {
		t.Fatalf("a failed wake must not deliver: %+v", list)
	}
	// Nothing further is due until the broker schedules the next wake.
	ok := func(_ context.Context, _ runtimeBinding, p string) error { prompts = append(prompts, p); return nil }
	if handled, err := relayWakeJob(ctx, b, c, ok); handled || err != nil {
		t.Fatalf("no wake should be due: handled %v err %v", handled, err)
	}
	if _, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: e.agent, Envelope: &api.Envelope{
		Kind: "notice", To: e.agentName, Subject: "Integration window closes today", Body: api.EnvelopeBody{Text: "rebase"}}}); err != nil {
		t.Fatal(err)
	}
	if handled, err := relayWakeJob(ctx, b, c, ok); !handled || err != nil || len(prompts) != 1 || !strings.Contains(prompts[0], "#"+itoa(m.Seq)) {
		t.Fatalf("accepted wake: handled %v err %v prompts %q", handled, err, prompts)
	}
	list, _ = c.ListObligations(ctx, task.ID, "", "", false, false)
	for _, o := range list {
		if (o.Needs == api.ObligationNeedsDelivery && o.State != api.ObligationClosed) || (o.Needs != api.ObligationNeedsDelivery && o.State != api.ObligationDelivered) {
			t.Fatalf("after accepted wake: %+v", o)
		}
	}
}
