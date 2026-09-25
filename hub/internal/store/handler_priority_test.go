package store

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

type handlerPriorityFixture struct {
	s                               *Store
	ctx                             context.Context
	by                              api.Caller
	task                            api.Task
	item                            api.WorkItem
	queuedItem                      api.WorkItem
	order                           api.Message
	queuedOrder                     api.Message
	lead, planner, builder, handler api.Agent
	queued                          api.Agent
	now                             time.Time
}

func newHandlerPriorityFixture(t *testing.T) *handlerPriorityFixture {
	s, ctx, by := workItemStore(t)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	task, lead := workItemProject(t, s, ctx, by, "priority fixture", "lead")
	item := createWorkItem(t, s, ctx, by, task, "priority-item")
	order := contextLinkedMessage(t, s, task, item, "bounded fixture order", "priority-order", nil)
	queuedItem := createWorkItem(t, s, ctx, by, task, "queued-item")
	queuedOrder := contextLinkedMessage(t, s, task, queuedItem, "queued fixture order", "queued-order", nil)
	add := func(name, role string) api.Agent {
		req := api.AddAgentRequest{Name: name, Host: "host", Session: name, Runtime: "codex", Role: role}
		if role == api.AgentRoleDatabaseHandler {
			req.AgentID = api.NewID("agt")
		}
		a, err := s.AddAgent(ctx, task.ID, req, by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	f := &handlerPriorityFixture{s: s, ctx: ctx, by: by, task: task, item: item, queuedItem: queuedItem, order: order, queuedOrder: queuedOrder, lead: lead,
		planner: add("planner", ""), builder: add("builder", ""), handler: add("database", api.AgentRoleDatabaseHandler), queued: add("queued", ""), now: now}
	for _, a := range []api.Agent{f.lead, f.planner, f.builder, f.handler, f.queued} {
		if _, err := s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventRunning}, by); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []api.Agent{f.lead, f.planner, f.builder} {
		_, err := s.db.ExecContext(ctx, `INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			a.ID, a.RunID, task.ID, item.ID, item.Revision, task.ID, order.Seq, order.Seq, strings.Repeat("a", 64), []byte(`{"synthetic":true}`), ts(now))
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		f.queued.ID, f.queued.RunID, task.ID, queuedItem.ID, queuedItem.Revision, task.ID, queuedOrder.Seq, queuedOrder.Seq, strings.Repeat("b", 64), []byte(`{"synthetic":true}`), ts(now))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *handlerPriorityFixture) request(t *testing.T, from api.Agent, linked bool) api.Message {
	return f.requestFor(t, from, linked, f.item, f.order)
}

func (f *handlerPriorityFixture) requestFor(t *testing.T, from api.Agent, linked bool, item api.WorkItem, order api.Message) api.Message {
	return f.requestForKey(t, from, linked, item, order, api.NewID("req"))
}

func (f *handlerPriorityFixture) requestForKey(t *testing.T, from api.Agent, linked bool, item api.WorkItem, order api.Message, key string) api.Message {
	t.Helper()
	req := api.PostMessageRequest{AgentID: from.ID, RunID: from.RunID, To: f.handler.ID, RequestID: key,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: f.handler.Name, Subject: "Verify the exact Start", Body: api.EnvelopeBody{Ask: "Check the bound run"}}}
	if linked {
		req.WorkItems = []api.MessageWorkItem{{ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
		req.WorkOrderMessage = &api.MessageReference{TaskID: f.task.ID, Seq: order.Seq}
	}
	m, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *handlerPriorityFixture) humanIntake(t *testing.T, key string) api.Message {
	t.Helper()
	m, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{To: f.handler.ID, Text: "Owner intake: record a queued-only item", RequestID: key}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *handlerPriorityFixture) result(t *testing.T, source api.Message, at time.Time) api.Message {
	t.Helper()
	f.s.now = func() time.Time { return at }
	m, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, ReplyTo: source.Seq, RequestID: api.NewID("req"),
		Envelope: &api.Envelope{Kind: api.EnvelopeKindResult, Subject: "The gate is verified", Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}}, Evidence: map[string]api.Evidence{"e1": {Type: "record", Value: "verified"}}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHandlerPriorityLiveGateBeforeQueuedRecords(t *testing.T) {
	f := newHandlerPriorityFixture(t)
	// Source order reproduces the reported problem: queued records first.
	queued := f.humanIntake(t, "queued-human-intake")
	if _, err := f.s.db.ExecContext(f.ctx, `UPDATE agents SET status=? WHERE id=?`, api.AgentRetired, f.queued.ID); err != nil {
		t.Fatal(err)
	}
	live := f.request(t, f.planner, true)
	got, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.handler.ID, OpenOnly: true}, f.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].MessageSeq != live.Seq || !got[0].HandlerPriority || got[1].MessageSeq != queued.Seq || got[1].HandlerPriority || got[1].HandlerRequest || got[1].SourceKind != "human" {
		t.Fatalf("priority order: %+v", got)
	}
	if got[0].PendingAgeMillis != 60000 {
		t.Fatalf("pending age: %+v", got[0])
	}
	// A late live request must appear in the first five wake entries even when
	// more than fifty earlier queued records exist.
	for i := 0; i < 55; i++ {
		f.humanIntake(t, "older-human-intake-"+strconv.Itoa(i))
	}
	f.s.now = func() time.Time { return f.now.Add(time.Second) }
	late := f.request(t, f.builder, true)
	before, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.handler.ID, OpenOnly: true}, f.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(before) < 2 || before[0].MessageSeq != live.Seq || before[1].MessageSeq != late.Seq {
		t.Fatalf("live FIFO tie: %+v", before)
	}
	job, err := f.s.LeaseWakeJob(f.ctx, f.task.ID, f.handler.ID, f.handler.RunID, f.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || !strings.Contains(job.Prompt, "#"+itoa(late.Seq)+" request") {
		t.Fatalf("late gate missing from wake: %+v", job)
	}
	firstResult := f.result(t, live, f.now.Add(30*time.Second))
	lateResult := f.result(t, late, f.now.Add(31*time.Second))
	all, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.handler.ID}, f.now.Add(32*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if all[0].MessageSeq != queued.Seq || all[0].HandlerPriority {
		t.Fatalf("queued work did not drain after gate results: %+v", all[:2])
	}
	queuedResult := f.result(t, queued, f.now.Add(32*time.Second))
	completed, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.handler.ID, FromSeq: queued.Seq, ToSeq: queued.Seq}, f.now.Add(33*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || completed[0].State != api.ObligationClosed || completed[0].OutcomeSeq != queuedResult.Seq || completed[0].ResponseMillis != 32000 {
		t.Fatalf("queued intake did not receive a result: %+v", completed)
	}
	for _, o := range all {
		if o.MessageSeq == live.Seq && (o.OutcomeSeq != firstResult.Seq || o.ResponseMillis != 30000) {
			t.Fatalf("first response: %+v", o)
		}
		if o.MessageSeq == late.Seq && (o.OutcomeSeq != lateResult.Seq || o.ResponseMillis != 30000) {
			t.Fatalf("late response: %+v", o)
		}
	}
	if _, err := f.s.db.ExecContext(f.ctx, `UPDATE agents SET status=? WHERE id IN (?,?,?)`, api.AgentClosed, f.lead.ID, f.planner.ID, f.builder.ID); err != nil {
		t.Fatal(err)
	}
	history, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.handler.ID}, f.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range history {
		if o.MessageSeq == live.Seq && (!o.HandlerRequest || o.ResponseMillis != 30000) {
			t.Fatalf("closeout changed response: %+v", o)
		}
	}
}

func TestHandlerPriorityRequiresExactBoundSourceAndKeepsInboxOrder(t *testing.T) {
	f := newHandlerPriorityFixture(t)
	plain := f.request(t, f.lead, false)
	gate := f.request(t, f.planner, true)
	list := func() []api.Obligation {
		rows, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.handler.ID, OpenOnly: true}, f.now)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if rows := list(); rows[0].MessageSeq != gate.Seq {
		t.Fatalf("valid gate not first: %+v", rows)
	}
	// The inbox still follows message sequence, never the priority comparator.
	inbox, err := f.s.ListMessages(f.ctx, f.task.ID, 0, f.handler.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) < 2 || inbox[len(inbox)-2].Seq != plain.Seq || inbox[len(inbox)-1].Seq != gate.Seq {
		t.Fatalf("inbox reordered: %+v", inbox)
	}
	if _, err := f.s.db.ExecContext(f.ctx, `UPDATE messages SET from_run_id=? WHERE seq=?`, "run_0000000000000000", gate.Seq); err != nil {
		t.Fatal(err)
	}
	if rows := list(); rows[0].MessageSeq != plain.Seq || rows[1].HandlerRequest {
		t.Fatalf("wrong run gained priority: %+v", rows)
	}
	if _, err := f.s.db.ExecContext(f.ctx, `UPDATE messages SET from_run_id=? WHERE seq=?`, f.planner.RunID, gate.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.ExecContext(f.ctx, `UPDATE message_work_item_links SET item_id=? WHERE message_seq=?`, f.queuedItem.ID, gate.Seq); err != nil {
		t.Fatal(err)
	}
	if rows := list(); rows[1].HandlerRequest {
		t.Fatalf("unrelated item gained priority: %+v", rows)
	}
	unbound, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "unbound", Host: "host", Session: "unbound", Runtime: "codex"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{AgentID: unbound.ID, RunID: unbound.RunID, Kind: api.EventRunning}, f.by); err != nil {
		t.Fatal(err)
	}
	orphan := f.request(t, unbound, false)
	if _, err = f.s.db.ExecContext(f.ctx, `INSERT INTO message_work_item_links(message_seq,message_task_id,item_task_id,item_id,item_revision,relationship,work_order_task_id,work_order_message_seq,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		orphan.Seq, f.task.ID, f.task.ID, f.item.ID, f.item.Revision, "primary", f.task.ID, f.order.Seq, ts(f.now)); err != nil {
		t.Fatal(err)
	}
	if rows := list(); rows[len(rows)-1].HandlerRequest {
		t.Fatalf("unbound source gained priority: %+v", rows)
	}
	// Another recipient's obligations stay in source FIFO order.
	for i := 0; i < 2; i++ {
		_, err = f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: f.builder.ID, RequestID: api.NewID("req"),
			Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: f.builder.Name, Subject: "Check normal work", Body: api.EnvelopeBody{Ask: "Please check"}}}, f.by)
		if err != nil {
			t.Fatal(err)
		}
	}
	worker, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.builder.ID, OpenOnly: true}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(worker) != 2 || worker[0].MessageSeq >= worker[1].MessageSeq || worker[0].HandlerPriority || worker[1].HandlerPriority {
		t.Fatalf("non-handler order: %+v", worker)
	}
}

// Admission pins revision N while subsequent native gate messages cite the
// current revision N+1. The item and exact sender run still identify the team.
func TestHandlerPriorityNewerMessageRevisionKeepsGateFirst(t *testing.T) {
	f := newHandlerPriorityFixture(t)
	queued := f.humanIntake(t, "revision-repro-human-intake")
	gate := f.request(t, f.planner, true)
	if _, err := f.s.db.ExecContext(f.ctx, `UPDATE message_work_item_links SET item_revision=item_revision+1 WHERE message_seq=?`, gate.Seq); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.handler.ID, OpenOnly: true}, f.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].MessageSeq != gate.Seq || !rows[0].HandlerRequest || !rows[0].HandlerPriority || rows[1].MessageSeq != queued.Seq {
		t.Fatalf("revision N+1 gate lost priority: %+v", rows)
	}
}

func TestHandlerPriorityRetryAndRetirementKeepResponseIdentity(t *testing.T) {
	f := newHandlerPriorityFixture(t)
	key := "same-handler-gate-request"
	gate := f.requestForKey(t, f.planner, true, f.item, f.order, key)
	if replay := f.requestForKey(t, f.planner, true, f.item, f.order, key); replay.Seq != gate.Seq {
		t.Fatalf("retry created message #%d, want #%d", replay.Seq, gate.Seq)
	}
	result := f.result(t, gate, f.now.Add(30*time.Second))
	if replay := f.requestForKey(t, f.planner, true, f.item, f.order, key); replay.Seq != gate.Seq {
		t.Fatalf("post-outcome retry created message #%d", replay.Seq)
	}
	open := f.request(t, f.builder, true)
	check := func(wantPriority bool) {
		rows, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.handler.ID}, f.now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 2 {
			t.Fatalf("retry changed obligation count: %+v", rows)
		}
		for _, o := range rows {
			if o.MessageSeq == gate.Seq && (!o.HandlerRequest || o.OutcomeSeq != result.Seq || o.ResponseMillis != 30000) {
				t.Fatalf("response identity changed: %+v", o)
			}
			if o.MessageSeq == open.Seq && (!o.HandlerRequest || o.HandlerPriority != wantPriority) {
				t.Fatalf("open priority changed: %+v", o)
			}
		}
	}
	check(true)
	if _, err := f.s.db.ExecContext(f.ctx, `UPDATE agents SET status=? WHERE id=?`, api.AgentRetired, f.planner.ID); err != nil {
		t.Fatal(err)
	}
	check(true) // other admitted members remain live
	if _, err := f.s.db.ExecContext(f.ctx, `UPDATE agents SET status=? WHERE id IN (?,?,?)`, api.AgentRetired, f.lead.ID, f.builder.ID, f.handler.ID); err != nil {
		t.Fatal(err)
	}
	check(false) // no live team remains; historical request and duration survive
}

func TestHandlerPriorityReplayRechecksBetweenQueuedWrites(t *testing.T) {
	f := newHandlerPriorityFixture(t)
	first := f.request(t, f.lead, true)
	queued := f.humanIntake(t, "replay-human-intake-0")
	queued2 := f.humanIntake(t, "replay-human-intake-1")
	if _, err := f.s.db.ExecContext(f.ctx, `UPDATE agents SET status=? WHERE id=?`, api.AgentRetired, f.queued.ID); err != nil {
		t.Fatal(err)
	}
	sources := map[int64]api.Message{first.Seq: first}
	intakes := []api.Message{queued, queued2}
	results := make([]api.Message, 0, 2)
	var trace []string
	for i := 0; i < 2; i++ {
		for {
			rows, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.handler.ID, OpenOnly: true}, f.now)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 || !rows[0].HandlerPriority {
				break
			}
			f.result(t, sources[rows[0].MessageSeq], f.now.Add(time.Duration(i+1)*time.Second))
			trace = append(trace, "gate")
		}
		if _, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Queued intake", Description: "fixture", RequestID: "queued-record-" + strconv.Itoa(i)}, f.by); err != nil {
			t.Fatal(err)
		}
		trace = append(trace, "queued")
		results = append(results, f.result(t, intakes[i], f.now.Add(time.Duration(i+1)*time.Second)))
		if i == 0 {
			late := f.request(t, f.builder, true)
			sources[late.Seq] = late
		}
	}
	if got := strings.Join(trace, ","); got != "gate,queued,gate,queued" {
		t.Fatalf("replay order %s", got)
	}
	rows, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.handler.ID, OpenOnly: true}, f.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("queued human intakes did not drain after gate results: %+v", rows)
	}
	all, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{AgentID: f.handler.ID}, f.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for i, intake := range intakes {
		found := false
		for _, o := range all {
			if o.MessageSeq == intake.Seq {
				found = true
				if o.OutcomeSeq != results[i].Seq || o.ResponseMillis != int64(i+1)*1000 || o.SourceKind != "human" {
					t.Fatalf("queued intake outcome: %+v", o)
				}
			}
		}
		if !found {
			t.Fatalf("queued intake #%d missing", intake.Seq)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
