package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
	"github.com/scs32/tailterm/hub/internal/store"
)

// Broker phase 3 Discord controls (docs/broker-phase-3.md c10, c11).
func TestPhase3Commands(t *testing.T) {
	h := newHarness(t)
	assign := h.post(typed(h.lead, h.builder, api.EnvelopeKindAssign, "Fix the stale smoke test"))
	question := h.post(typed(h.builder, h.lead, api.EnvelopeKindQuestion, "Which fixture should the test use"))
	cmd := func(id, name string, opts ...discord.InteractionOption) string {
		return editContent(h.interact(discord.Interaction{ID: id, Type: discord.InteractionCommand, Data: discord.InteractionData{Name: name, Options: opts}}))
	}
	if got := cmd("940000000000000001", "extend", stringOption("obligation", itoa(assign.Seq)), stringOption("for", "2h"), stringOption("reason", "CI is slow")); !strings.Contains(got, "Extended #"+itoa(assign.Seq)) {
		t.Fatalf("/extend = %q", got)
	}
	if got := cmd("940000000000000002", "extend", stringOption("obligation", itoa(assign.Seq)), stringOption("for", "10s")); !strings.Contains(got, "1m to 168h") {
		t.Fatalf("/extend with a bad duration = %q", got)
	}
	if got := cmd("940000000000000003", "answer", stringOption("obligation", itoa(question.Seq)), stringOption("text", "Use the static fixture.")); !strings.Contains(got, "answered #"+itoa(question.Seq)) {
		t.Fatalf("/answer = %q", got)
	}
	msgs := h.boardMessages()
	last := msgs[len(msgs)-1]
	if last.Envelope == nil || last.Envelope.Kind != api.EnvelopeKindAnswer || last.To != h.builder.ID || last.Source != nil && last.From.Node != api.BridgeNode {
		t.Fatalf("the answer on the board = %+v", last)
	}
	if got := cmd("940000000000000004", "cancel", stringOption("obligation", itoa(assign.Seq)), stringOption("reason", "superseded by the new plan")); !strings.Contains(got, "Cancelled #"+itoa(assign.Seq)) {
		t.Fatalf("/cancel = %q", got)
	}
	// A stale Extend button on the cancelled obligation changes nothing.
	if got := editContent(h.interact(discord.Interaction{ID: "940000000000000005", Type: discord.InteractionComponent, Data: discord.InteractionData{CustomID: "extend30:" + h.obligationID(assign.Seq)}})); !strings.Contains(got, "Already handled") {
		t.Fatalf("a stale Extend button = %q", got)
	}
	retired := api.AgentRetired
	if _, err := h.st.UpdateAgent(h.ctx, h.builder.ID, api.UpdateAgentRequest{Status: &retired}, owner); err != nil {
		t.Fatal(err)
	}
	if got := cmd("940000000000000006", "resume", stringOption("agent", "builder")); !strings.Contains(got, "Resumed builder-41b1c632") {
		t.Fatalf("/resume = %q", got)
	}
	if a, _ := h.st.GetAgent(h.ctx, h.builder.ID); a.Status != api.AgentDone {
		t.Fatalf("after /resume the builder is %s", a.Status)
	}
}

func (h *harness) obligationID(seq int64) string {
	h.t.Helper()
	all, err := h.st.ListObligations(h.ctx, h.task.ID, store.ObligationFilter{}, time.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, o := range all {
		if o.MessageSeq == seq {
			return o.ID
		}
	}
	h.t.Fatalf("no obligation for #%d", seq)
	return ""
}

// c11: hub services render by name, never as the owner.
func TestSystemSendersRenderByName(t *testing.T) {
	r := roster{}
	m := api.Message{From: api.Sender{Node: "system", User: "schedule-monitor"}}
	if got := r.sender(m); got != "schedule-monitor" {
		t.Fatalf("sender = %q", got)
	}
}

// Round one F9, F11: an ambiguous /resume is refused, and a hub outage is
// never reported as "already handled".
func TestResumeAmbiguityAndOutages(t *testing.T) {
	h := newHarness(t)
	other := h.agent("builder-99999999")
	retired := api.AgentRetired
	for _, a := range []api.Agent{h.builder, other} {
		if _, err := h.st.UpdateAgent(h.ctx, a.ID, api.UpdateAgentRequest{Status: &retired}, owner); err != nil {
			t.Fatal(err)
		}
	}
	got := editContent(h.interact(discord.Interaction{ID: "950000000000000001", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "resume", Options: []discord.InteractionOption{stringOption("agent", "builder")}}}))
	if !strings.Contains(got, "matches 2 agents") {
		t.Fatalf("/resume with two builders = %q", got)
	}
	h.hubDown.Store(true)
	got = editContent(h.interact(discord.Interaction{ID: "950000000000000002", Type: discord.InteractionCommand, Data: discord.InteractionData{Name: "cancel", Options: []discord.InteractionOption{stringOption("obligation", "1"), stringOption("reason", "x")}}}))
	h.hubDown.Store(false)
	if strings.Contains(got, "Already handled") {
		t.Fatalf("a hub outage was reported as handled: %q", got)
	}
}
