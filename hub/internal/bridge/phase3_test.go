package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
	"github.com/scs32/tailterm/hub/internal/server"
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
	for _, from := range []api.Sender{
		{Node: "system", User: "schedule-monitor"},
		{Node: "team_queue", User: "runner"},
		{Node: "team_queue", User: "rebind"},
		{Node: "handler_arms", User: "hub"},
		{Node: "verification", User: "hub"},
	} {
		if got := r.sender(api.Message{From: from}); got != from.User {
			t.Fatalf("%+v: sender = %q, want %q", from, got, from.User)
		}
	}
}

// A queue-runner, handler-arms or verification notice the bridge mirrors as a
// line names that service, not the owner.
func TestQueueAndHandlerArmsSendersRenderByName(t *testing.T) {
	h := newHarness(t)
	callers := []api.Caller{{Node: "team_queue", User: "runner"}, {Node: "handler_arms", User: "hub"}, {Node: "verification", User: "hub"}}
	for _, by := range callers {
		note := &api.Envelope{Kind: api.EnvelopeKindNotice, To: h.lead.Name, Subject: "A hub service notice for the lead", Refs: map[string]string{"escalation": "lead"}, Body: api.EnvelopeBody{Text: "fyi"}}
		m, err := h.st.PostMessage(h.ctx, h.task.ID, api.PostMessageRequest{To: h.lead.ID, Envelope: note, Text: api.RenderText(*note)}, by)
		if err != nil {
			t.Fatal(err)
		}
		if m.From != (api.Sender{Node: by.Node, User: by.User}) {
			t.Fatalf("stored author = %+v, want %+v", m.From, by)
		}
	}
	h.cycle()
	lines := h.lines()
	if len(lines) != len(callers) {
		t.Fatalf("want %d mirrored lines, got %d", len(callers), len(lines))
	}
	for i, by := range callers {
		if got := lines[i].Content; !strings.HasPrefix(got, "**"+by.User+"** → **"+h.lead.Name+"**") || strings.Contains(got, "**owner") {
			t.Fatalf("%s line header = %q", by.Node, got)
		}
	}
}

// A relay-authored notice the bridge mirrors as a line names the relay, not
// the owner whose token the relay posts with.
func TestRelaySenderRendersAsRelay(t *testing.T) {
	if relayNode != server.RelayNode {
		t.Fatalf("relay node %q differs from the server's %q", relayNode, server.RelayNode)
	}
	h := newHarness(t)
	note := &api.Envelope{Kind: api.EnvelopeKindNotice, To: h.lead.Name, Subject: "A relay notice for the lead", Refs: map[string]string{"escalation": "lead"}, Body: api.EnvelopeBody{Text: "fyi"}}
	m, err := h.st.PostMessage(h.ctx, h.task.ID, api.PostMessageRequest{To: h.lead.ID, Envelope: note, Text: api.RenderText(*note)}, api.Caller{Node: server.RelayNode, User: server.RelayUser})
	if err != nil {
		t.Fatal(err)
	}
	if m.From != (api.Sender{Node: server.RelayNode, User: server.RelayUser}) {
		t.Fatalf("stored author = %+v", m.From)
	}
	h.cycle()
	lines := h.lines()
	if len(lines) != 1 {
		t.Fatalf("want one mirrored line, got %d", len(lines))
	}
	if got := lines[0].Content; !strings.HasPrefix(got, "**relay** → **"+h.lead.Name+"**") || strings.Contains(got, "**owner") {
		t.Fatalf("relay line header = %q", got)
	}
	// Any other agentless node is still the owner.
	if got := (roster{}).sender(api.Message{From: api.Sender{Node: "workspace", User: "owner"}}); got != "owner" {
		t.Fatalf("owner sender = %q", got)
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

// wi_6c053438bd76c7bd: only a request the hub holds an owner obligation for
// is drawn as the Request to owner card. A board-wide request without exact
// context has none and mirrors as an ordinary typed line.
func TestOwnerCardNeedsOwnerObligation(t *testing.T) {
	h := newHarness(t)
	item, err := h.st.CreateWorkItem(h.ctx, h.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Owner card fixture", RequestID: "owner-card-item"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	request := func(id, subject string, links []api.MessageWorkItem) api.Message {
		env := &api.Envelope{Kind: api.EnvelopeKindRequest, Subject: subject, ExpectedAnswer: "yes", Body: api.EnvelopeBody{Ask: "Please approve"}}
		return h.post(api.PostMessageRequest{AgentID: h.builder.ID, RunID: h.builder.RunID, RequestID: id, Envelope: env, Text: api.RenderText(*env), WorkItems: links})
	}
	linked := request("owner-card-linked", "Approve the linked fixture", []api.MessageWorkItem{{ItemTaskID: h.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}})
	unlinked := request("owner-card-unlinked", "Approve the unlinked fixture", nil)
	h.obligationID(linked.Seq)
	h.cycle()
	byMarker := map[int64]*fakeMessage{}
	for _, line := range h.lines() {
		for _, seq := range []int64{linked.Seq, unlinked.Seq} {
			if strings.Contains(line.Content, "-# #"+itoa(seq)) {
				byMarker[seq] = line
			}
		}
	}
	approve := func(line *fakeMessage) int {
		n := 0
		for _, row := range line.Components {
			for _, button := range row.Components {
				if button.Label == "Approve" {
					n++
				}
			}
		}
		return n
	}
	card := byMarker[linked.Seq]
	if card == nil || !strings.HasPrefix(card.Content, "**Request to owner**") || !strings.Contains(card.Content, "owner-request") || approve(card) != 1 {
		t.Fatalf("the obligated request lost its owner card: %+v", card)
	}
	line := byMarker[unlinked.Seq]
	header := "**" + h.builder.Name + "** → **everyone** · REQUEST · Approve the unlinked fixture"
	if line == nil || !strings.HasPrefix(line.Content, header) || strings.Contains(line.Content, "owner-request") || strings.Contains(line.Content, "Request to owner") || approve(line) != 0 || len(line.Embeds) != 1 {
		t.Fatalf("the unobligated request is not an ordinary line: %+v", line)
	}

	// A failed obligation lookup keeps the card rather than hide a real one.
	h.hubDown.Store(true)
	rows := h.b.renderMessage(h.task, roster{h.builder.ID: h.builder}, unlinked)
	h.hubDown.Store(false)
	if len(rows) == 0 || !strings.HasPrefix(rows[0].Key, "owner-request:") {
		t.Fatalf("a failed lookup dropped the owner card: %+v", rows)
	}
}
