package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// wi_a31c079e98518efc (#11759): a message addressed to a work-item-bound agent
// reaches its inbox and unread count whether or not it links that item, while
// board-wide and broadcast traffic for other items stays out.
func TestBoundInboxIncludesUnlinkedDirected(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	other, err := f.s.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Another item", AgentID: f.lead.ID, RequestID: "inbox-other-item"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	posted := 0
	post := func(req api.PostMessageRequest) api.Message {
		t.Helper()
		posted++
		if len(req.WorkItems) > 0 {
			req.RequestID = fmt.Sprintf("inbox-linked-%d", posted)
		}
		m, err := f.s.PostMessage(ctx, f.task.ID, req, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	otherLink := []api.MessageWorkItem{{ItemTaskID: other.TaskID, ItemID: other.ID, ItemRevision: other.Revision, Relationship: "primary"}}
	ownLink := []api.MessageWorkItem{{ItemTaskID: f.item.TaskID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}
	orderRef := &api.MessageReference{TaskID: f.task.ID, Seq: f.order.Seq}

	human := post(api.PostMessageRequest{Text: "unlinked owner instruction", To: f.worker.ID})
	agentText := post(api.PostMessageRequest{Text: "unlinked lead free text", To: f.worker.ID, AgentID: f.lead.ID})
	elsewhere := post(api.PostMessageRequest{Text: "directed but linked to another item", To: f.worker.ID, WorkItems: otherLink})
	board := post(api.PostMessageRequest{Text: "unlinked board-wide post"})
	own := post(api.PostMessageRequest{Text: "board-wide post for the bound item", WorkItems: ownLink, WorkOrderMessage: orderRef})
	self := post(api.PostMessageRequest{Text: "worker's own progress", AgentID: f.worker.ID, WorkItems: ownLink, WorkOrderMessage: orderRef})
	toLead := post(api.PostMessageRequest{Text: "unlinked message to the lead", To: f.lead.ID})
	if _, err = f.s.db.ExecContext(ctx, `UPDATE tasks SET swarm=1 WHERE id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	broadcastElsewhere := post(api.PostMessageRequest{Text: "broadcast linked to another item", To: f.lead.ID, WorkItems: otherLink})
	if !broadcastElsewhere.Broadcast {
		t.Fatalf("fixture did not broadcast: %+v", broadcastElsewhere)
	}
	broadcastDirected := post(api.PostMessageRequest{Text: "broadcast addressed to the worker", To: f.worker.ID, WorkItems: otherLink})

	inbox, err := f.s.ListMessages(ctx, f.task.ID, f.worker.ReadUpTo, f.worker.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, m := range inbox {
		got[m.Seq] = true
	}
	for name, m := range map[string]api.Message{"human": human, "agent free text": agentText, "linked elsewhere": elsewhere,
		"own item": own, "self": self, "broadcast directed": broadcastDirected} {
		if !got[m.Seq] {
			t.Errorf("bound inbox is missing the %s message #%d: %+v", name, m.Seq, inbox)
		}
	}
	for name, m := range map[string]api.Message{"unlinked board": board, "to lead": toLead, "broadcast elsewhere": broadcastElsewhere} {
		if got[m.Seq] {
			t.Errorf("bound inbox leaked the %s message #%d", name, m.Seq)
		}
	}
	if len(inbox) != 6 {
		t.Errorf("bound inbox has %d messages, want 6: %+v", len(inbox), inbox)
	}
	nonSelf := 0
	for _, m := range inbox {
		if m.From.AgentID != f.worker.ID {
			nonSelf++
		}
	}
	unread, err := f.s.Unread(ctx, f.task.ID, f.worker.ID)
	if err != nil || unread != nonSelf || unread != 5 {
		t.Errorf("Unread = %d (%v), want the %d non-self inbox messages", unread, err, nonSelf)
	}

	// Directed-only newest paging returns only what is addressed to the worker.
	directed, err := f.s.ListMessagesPage(ctx, f.task.ID, api.MessagePageQuery{After: f.worker.ReadUpTo, To: f.worker.ID, DirectedOnly: true, Newest: true, Limit: 2})
	if err != nil || len(directed) != 2 || directed[0].Seq != elsewhere.Seq || directed[1].Seq != broadcastDirected.Seq {
		t.Errorf("directed newest page: %+v %v", directed, err)
	}
	if _, err = f.s.ListMessagesPage(ctx, f.task.ID, api.MessagePageQuery{DirectedOnly: true}); err == nil {
		t.Error("directed paging without a recipient was accepted")
	}
}

// A wake prompt shows only a short subject per obligation, so it names the
// command that prints a message in full (wi_a31c079e98518efc, i3).
func TestObligationWakePromptNamesFullTextCommand(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx := context.Background()
	text := strings.Repeat("owner instruction ", 10) + "FULL-TAIL"
	m, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: text, To: f.worker.ID}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	obligations, err := f.s.ListObligations(ctx, f.task.ID, ObligationFilter{AgentID: f.worker.ID, OpenOnly: true}, f.s.now())
	if err != nil || len(obligations) != 1 || obligations[0].MessageSeq != m.Seq || strings.Contains(obligations[0].Subject, "FULL-TAIL") {
		t.Fatalf("expected one obligation with a truncated subject: %+v %v", obligations, err)
	}
	if _, err = f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{Kind: api.EventHeartbeat, AgentID: f.worker.ID, RunID: f.worker.RunID}, f.by); err != nil {
		t.Fatal(err)
	}
	job, err := f.s.LeaseWakeJob(ctx, f.task.ID, f.worker.ID, f.worker.RunID, f.s.now())
	if err != nil || job == nil {
		t.Fatalf("no wake job: %+v %v", job, err)
	}
	if !strings.Contains(job.Prompt, "#"+itoa(m.Seq)+" human") || !strings.Contains(job.Prompt, "`tt inbox --seq SEQ`") {
		t.Fatalf("wake prompt does not name the full-text command: %q", job.Prompt)
	}
	full, err := f.s.ListMessagesPage(ctx, f.task.ID, api.MessagePageQuery{After: m.Seq - 1, Limit: 1})
	if err != nil || len(full) != 1 || full[0].Seq != m.Seq || full[0].Text != text {
		t.Fatalf("single-message page did not return the full text: %+v %v", full, err)
	}
}
