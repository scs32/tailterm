package bridge

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

func TestCardShowsWithdrawnWithoutCountingItOpen(t *testing.T) {
	task := api.Task{Name: "Card"}
	a := api.Agent{ID: "agt_0000000000000001", Name: "worker", Status: api.AgentRunning}
	rows := []api.Obligation{
		{MessageSeq: 10, AgentID: a.ID, Subject: "old request", State: api.ObligationClosed, Outcome: api.OutcomeWithdrawn},
		{MessageSeq: 11, AgentID: a.ID, Subject: "current request", State: api.ObligationQueued, Needs: api.ObligationNeedsOutcome, Overdue: "ack"},
	}
	card, _ := renderCard(task, []api.Agent{a}, rows)
	if len(card.Embeds) != 1 {
		t.Fatalf("card: %+v", card)
	}
	e := card.Embeds[0]
	if !strings.Contains(e.Description, "1 open") || !strings.Contains(e.Description, "1 unacknowledged") || !strings.Contains(e.Description, "1 overdue") {
		t.Fatalf("open count changed: %+v", e)
	}
	if len(e.Fields) != 2 || e.Fields[1].Name != "Withdrawn (1)" || !strings.Contains(e.Fields[1].Value, "#10") {
		t.Fatalf("withdrawn not visible: %+v", e.Fields)
	}
}

func TestPinnedAndSlashCardsShowWithdrawnFromHub(t *testing.T) {
	h := newHarness(t)
	m := h.post(typed(h.lead, h.builder, api.EnvelopeKindAssign, "Old assigned review"))
	rows, err := h.st.ListObligations(h.ctx, h.task.ID, store.ObligationFilter{FromSeq: m.Seq, ToSeq: m.Seq}, time.Now())
	if err != nil || len(rows) != 1 {
		t.Fatalf("source: %+v %v", rows, err)
	}
	if _, err := h.st.WithdrawObligation(h.ctx, h.task.ID, rows[0].ID, api.ObligationWithdrawRequest{AgentID: h.lead.ID, RunID: h.lead.RunID, Reason: "new review request"}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.MarkObligationsDelivered(h.ctx, h.task.ID, h.builder.ID, h.builder.RunID, time.Now()); err != nil {
		t.Fatal(err)
	}
	check := func(label string, embeds []string) {
		t.Helper()
		joined := strings.Join(embeds, "\n")
		if !strings.Contains(joined, "Withdrawn (1)") || !strings.Contains(joined, "#"+strconv.FormatInt(m.Seq, 10)) || strings.Contains(joined, "1 unacknowledged") || strings.Contains(joined, "1 overdue") {
			t.Fatalf("%s: %s", label, joined)
		}
	}
	slash := h.b.statusReply(h.ctx, h.task.ID)
	if len(slash.Embeds) != 1 {
		t.Fatalf("slash: %+v", slash)
	}
	var fields []string
	for _, f := range slash.Embeds[0].Fields {
		fields = append(fields, f.Name+" "+f.Value)
	}
	check("slash", fields)
	h.cycle()
	h.b.cfg.CardInterval = 0
	mapping, err := h.state.Mapping(h.ctx, h.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.b.refreshCard(h.ctx, mapping); err != nil {
		t.Fatal(err)
	}
	mapping, _ = h.state.Mapping(h.ctx, h.task.ID)
	card := h.fake.find(h.channel, mapping.CardMessageID)
	fields = nil
	for _, f := range card.Embeds[0].Fields {
		fields = append(fields, f.Name+" "+f.Value)
	}
	check("pinned", fields)
}
