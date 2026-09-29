package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Handler rotation tests use an isolated SQLite hub per test and synthetic
// agents only (wi_611e4d3c6992a664, order #11573).

type rotationFixture struct {
	s      *Store
	path   string
	task   api.Task
	items  []api.WorkItem
	orders []api.Message
	old    api.Agent
	worker api.Agent
	by     api.Caller
}

func newRotationFixture(t *testing.T) *rotationFixture {
	t.Helper()
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	f := &rotationFixture{s: s, task: task, items: items, orders: orders, by: api.Caller{Node: "fixture", User: "owner"}}
	var err error
	if err = s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&f.path); err != nil {
		t.Fatal(err)
	}
	agents, err := s.ListAgents(ctx, task.ID)
	if err != nil || len(agents) != 1 || agents[0].Role != api.AgentRoleDatabaseHandler {
		t.Fatalf("fixture handler: %+v %v", agents, err)
	}
	f.old = agents[0]
	f.worker = f.agent(t, "builder", "")
	f.idle(t, f.old)
	return f
}

// agent registers an online agent; a role makes it a handler on the old one's host.
func (f *rotationFixture) agent(t *testing.T, name, role string) api.Agent {
	t.Helper()
	ctx := context.Background()
	req := api.AddAgentRequest{Name: name, Host: "mini", Session: name, Runtime: f.old.Runtime, Cwd: f.old.Cwd}
	if role != "" {
		req.Role, req.AgentID = role, api.NewID("agt")
	}
	a, err := f.s.AddAgent(ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted}, f.by); err != nil {
		t.Fatal(err)
	}
	a, err = f.s.GetAgent(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *rotationFixture) activity(t *testing.T, a api.Agent, state, tool string) {
	t.Helper()
	now := time.Now().UTC()
	activity := api.AgentActivity{State: state, ObservedAt: now, LastEventAt: now, PendingTool: tool}
	if tool != "" {
		activity.PendingSince = now
	}
	if _, err := f.s.ReportActivity(context.Background(), f.task.ID, a.ID, api.ActivityReport{RequestID: api.NewID("act"), RunID: a.RunID, Activity: activity}); err != nil {
		t.Fatal(err)
	}
}

func (f *rotationFixture) idle(t *testing.T, a api.Agent) { f.activity(t, a, "idle", "") }

func (f *rotationFixture) send(t *testing.T, from api.Agent, kind, to, subject string, links bool) api.Message {
	t.Helper()
	env := &api.Envelope{Kind: kind, To: to, Subject: subject, Body: api.EnvelopeBody{Ask: "Please record this synthetic fixture item.", Text: "synthetic fixture notice"}}
	if kind == api.EnvelopeKindQuestion {
		env.Body = api.EnvelopeBody{Question: "Which synthetic fixture option applies?"}
	}
	req := api.PostMessageRequest{Envelope: env, AgentID: from.ID, RunID: from.RunID}
	if links {
		item := f.items[0]
		req.WorkItems = []api.MessageWorkItem{{ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
		req.WorkOrderMessage = &api.MessageReference{TaskID: f.task.ID, Seq: f.orders[0].Seq}
		req.RequestID = api.NewID("req")
	}
	if to != "" && to[:5] != "role:" {
		req.To = to
	}
	req.Text = api.RenderText(*env)
	m, err := f.s.PostMessage(context.Background(), f.task.ID, req, f.by)
	if err != nil {
		t.Fatalf("send %s to %s: %v", kind, to, err)
	}
	return m
}

func (f *rotationFixture) prepare(t *testing.T, key string, successor api.Agent, name string) (api.HandlerRotation, error) {
	t.Helper()
	task, err := f.s.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := successor.ID
	if id == "" {
		id = api.NewID("agt")
	}
	if name == "" {
		name = api.HandlerSuccessorName(f.old.Name, task.HandlerRevision)
	}
	return f.s.HandlerRotationAction(context.Background(), f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: key,
		ExpectedHandlerRevision: task.HandlerRevision, OldAgentID: f.old.ID, OldRunID: f.old.RunID, SuccessorAgentID: id, SuccessorName: name,
		Reason: api.HandlerRotationReasonManual, Trigger: api.HandlerRotationTriggerOwner}, f.by)
}

func (f *rotationFixture) commit(r api.HandlerRotation, key string) (api.HandlerRotation, error) {
	return f.s.HandlerRotationAction(context.Background(), f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationCommit, RequestID: key, RotationID: r.ID}, f.by)
}

func refusalCode(err error) string {
	var refusal *api.HandlerRotationRefusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	return ""
}

type rotationObligation struct {
	id, agent, state, outcome, viaRole string
	seq                                int64
	links                              int
}

func openObligations(t *testing.T, s *Store, task string) []rotationObligation {
	t.Helper()
	rows, err := s.db.Query(`SELECT o.id,o.agent_id,o.state,o.outcome,o.via_role,o.message_seq,(SELECT count(*) FROM message_work_item_links l WHERE l.message_task_id=o.task_id AND l.message_seq=o.message_seq)
 FROM obligations o WHERE o.task_id=? ORDER BY o.message_seq`, task)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []rotationObligation
	for rows.Next() {
		var o rotationObligation
		if err := rows.Scan(&o.id, &o.agent, &o.state, &o.outcome, &o.viaRole, &o.seq, &o.links); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	return out
}

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestHandlerRotationPolicyDefaultsRevisionAndOwnerOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	by := api.Caller{Node: "fixture", User: "owner"}
	existing, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "existing project"}, by)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a hub that predates rotation: the first migration turns
	// rotation off for projects that already exist.
	if _, err = s.db.Exec(`DROP TABLE handler_rotation_policy`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.HandlerRotationPolicy(ctx, existing.ID)
	if err != nil || p.Enabled || p.Revision != 1 || p.MaxItems != 10 || p.MaxTotalTokens != 300_000_000 || !p.OnTemplateChange {
		t.Fatalf("existing project must be opt-in with option A limits: %+v %v", p, err)
	}
	fresh, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "new project"}, by)
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.HandlerRotationPolicy(ctx, fresh.ID)
	if err != nil || !p.Enabled || p.Revision != 0 || p.MaxItems != api.DefaultHandlerRotationMaxItems || p.MaxTotalTokens != api.DefaultHandlerRotationMaxTotalTokens || !p.OnTemplateChange {
		t.Fatalf("new project default must be owner option A, enabled: %+v %v", p, err)
	}
	set := api.HandlerRotationPolicyRequest{ExpectedRevision: 0, Enabled: true, MaxItems: 3, MaxTotalTokens: 5000, OnTemplateChange: false}
	agentSet := set
	agentSet.ActorAgentID = api.NewID("agt")
	if _, err = s.SetHandlerRotationPolicy(ctx, fresh.ID, agentSet); refusalCode(err) != api.HandlerRotationRefusedAgentCaller {
		t.Fatalf("agent caller: %v", err)
	}
	stale := set
	stale.ExpectedRevision = 4
	if _, err = s.SetHandlerRotationPolicy(ctx, fresh.ID, stale); refusalCode(err) != api.HandlerRotationRefusedStalePolicy {
		t.Fatalf("stale revision: %v", err)
	}
	saved, err := s.SetHandlerRotationPolicy(ctx, fresh.ID, set)
	if err != nil || saved.Revision != 1 || saved.MaxItems != 3 || saved.MaxTotalTokens != 5000 || saved.OnTemplateChange || !saved.Enabled {
		t.Fatalf("saved: %+v %v", saved, err)
	}
	if _, err = s.SetHandlerRotationPolicy(ctx, fresh.ID, set); refusalCode(err) != api.HandlerRotationRefusedStalePolicy {
		t.Fatalf("reused revision: %v", err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	p, err = s.HandlerRotationPolicy(ctx, fresh.ID)
	if err != nil || p.Revision != 1 || p.MaxItems != 3 || p.MaxTotalTokens != 5000 || p.OnTemplateChange {
		t.Fatalf("policy did not persist: %+v %v", p, err)
	}
}

func TestHandlerRotationCommitMovesEveryObligationOnceAndSnapshotsHandoff(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	s := f.s
	lead := f.agent(t, "lead", "")
	// Handoff fixtures: an open request the old handler authored.
	authored := f.send(t, f.old, api.EnvelopeKindRequest, f.worker.ID, "Please confirm the fixture handoff", false)
	// Obligations held by the old handler: by role and by name, some with
	// item links, then moved into every non-closed state.
	states := []string{api.ObligationQueued, api.ObligationDelivered, api.ObligationAcknowledged, api.ObligationWorking, api.ObligationBlocked}
	var held []api.Message
	for i, state := range states {
		to := "role:" + api.RoleDatabaseHandler
		if i%2 == 1 {
			to = f.old.ID
		}
		m := f.send(t, f.worker, api.EnvelopeKindRequest, to, "Record synthetic fixture item "+state, i%2 == 0)
		if _, err := s.db.Exec(`UPDATE obligations SET state=? WHERE message_seq=? AND agent_id=?`, state, m.Seq, f.old.ID); err != nil {
			t.Fatal(err)
		}
		held = append(held, m)
	}
	question := f.send(t, lead, api.EnvelopeKindQuestion, f.old.ID, "Which fixture option applies", true)
	held = append(held, question)
	done := f.send(t, lead, api.EnvelopeKindRequest, f.old.ID, "Already closed fixture work", false)
	if _, err := s.db.Exec(`UPDATE obligations SET state=?,outcome=? WHERE message_seq=?`, api.ObligationClosed, api.OutcomeResult, done.Seq); err != nil {
		t.Fatal(err)
	}
	// A pending scope confirmation: a queued entry whose order is unconfirmed.
	pending, err := s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "rotation-pending-scope", Operation: "add", ItemID: f.items[1].ID, OrderMessageSeq: f.orders[1].Seq, Host: "mini", Cwd: "/pending"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DELETE FROM work_order_scope_confirmations WHERE item_id=?`, f.items[1].ID); err != nil {
		t.Fatal(err)
	}
	intentAgent := api.NewID("agt")
	if _, err = s.db.Exec(`INSERT INTO agent_allocation_intents(agent_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,team_role,target_task_id,author_agent_id,author_run_id,created_by_node,created_by_user,created_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, intentAgent, f.task.ID, f.items[0].ID, f.items[0].Revision, f.task.ID, f.orders[0].Seq, api.TeamRoleMember, f.task.ID, f.old.ID, f.old.RunID, "fixture", "owner", ts(s.now())); err != nil {
		t.Fatal(err)
	}
	queueEntry := api.NewID("que")
	if _, err = s.db.Exec(`INSERT INTO queue_entries(id,target_task_id,source_task_id,item_id,cycle,revision,state,queue_priority,eligible,stale,review_needed,pending_update,offered_item_revision,current_item_revision,claimant_agent_id,claimant_run_id,item_kind,item_title,item_status,item_priority,first_enqueued_at,latest_enqueued_at,updated_at)
 VALUES(?,?,?,?,1,1,?,?,1,0,0,0,1,1,?,?,?,?,?,?,?,?,?)`, queueEntry, f.task.ID, f.task.ID, f.items[0].ID, api.QueueStateClaimed, "normal", f.old.ID, f.old.RunID, "feature", "Queued work", "open", "normal", ts(s.now()), ts(s.now()), ts(s.now())); err != nil {
		t.Fatal(err)
	}
	delivery := api.NewID("dlv")
	if _, err = s.db.Exec(`INSERT INTO required_deliveries(id,task_id,message_seq,kind,agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_digest,generation,phase,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,1,?,?,?)`, delivery, f.task.ID, f.orders[0].Seq, "work_order", f.old.ID, f.old.RunID, f.task.ID, f.items[0].ID, f.items[0].Revision, f.task.ID, f.orders[0].Seq, "fixture-digest", "pending", ts(s.now()), ts(s.now())); err != nil {
		t.Fatal(err)
	}
	// An auxiliary handler created between the old handler and the successor.
	aux := f.agent(t, "aux-handler", api.AgentRoleDatabaseHandler)
	before := openObligations(t, s, f.task.ID)
	openBefore := 0
	for _, o := range before {
		if o.state != api.ObligationClosed && (o.agent == f.old.ID) {
			openBefore++
		}
	}
	if openBefore != len(held) {
		t.Fatalf("fixture holds %d open obligations, want %d", openBefore, len(held))
	}
	r, err := f.prepare(t, "rotate-prepare", api.Agent{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != api.HandlerRotationPrepared || r.SuccessorName != "database-r2" || r.HandlerRevision != 1 {
		t.Fatalf("prepared: %+v", r)
	}
	if again, err := f.prepare(t, "rotate-prepare", api.Agent{ID: r.SuccessorAgentID}, ""); err != nil || again.ID != r.ID {
		t.Fatalf("keyed prepare replay: %+v %v", again, err)
	}
	successor, err := s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: r.SuccessorAgentID, Role: api.AgentRoleDatabaseHandler, Name: r.SuccessorName, Host: "mini", Session: "tt-handler-successor", Runtime: f.old.Runtime, Cwd: f.old.Cwd}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	// While prepared, the old handler keeps role:database_handler, even
	// though the successor is now the newest open handler.
	task, _ := s.GetTask(ctx, f.task.ID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := resolveRole(ctx, tx, task, api.RoleDatabaseHandler, "")
	tx.Rollback()
	if err != nil || holder.ID != aux.ID {
		t.Fatalf("prepared successor must not win the legacy role rule: %+v %v", holder, err)
	}
	if _, err = f.commit(r, "rotate-commit"); refusalCode(err) != api.HandlerRotationRefusedSuccessor {
		t.Fatalf("an offline successor must be refused: %v", err)
	}
	if _, err = s.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: successor.ID, RunID: successor.RunID, Kind: api.EventStarted}, f.by); err != nil {
		t.Fatal(err)
	}
	messagesBefore := countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID)
	committed, err := f.commit(r, "rotate-commit")
	if err != nil {
		t.Fatal(err)
	}
	if committed.State != api.HandlerRotationCommitted || committed.Receipt == nil || committed.Handoff == nil {
		t.Fatalf("committed: %+v", committed)
	}
	after := openObligations(t, s, f.task.ID)
	byID := map[string]rotationObligation{}
	for _, o := range after {
		byID[o.id] = o
	}
	newBySource := map[int64][]rotationObligation{}
	for _, pair := range committed.Handoff.Reissued {
		newBySource[pair.OldMessageSeq] = append(newBySource[pair.OldMessageSeq], byID[pair.NewObligationID])
	}
	for _, o := range before {
		if o.agent != f.old.ID {
			continue
		}
		now := byID[o.id]
		if o.state == api.ObligationClosed {
			if now.outcome != api.OutcomeResult {
				t.Fatalf("closed obligation changed: %+v", now)
			}
			continue
		}
		if now.state != api.ObligationClosed || now.outcome != api.OutcomeSuperseded {
			t.Fatalf("old obligation %s not superseded once: %+v", o.id, now)
		}
		moved := newBySource[o.seq]
		if len(moved) != 1 || moved[0].agent != successor.ID || moved[0].state == api.ObligationClosed || moved[0].viaRole != o.viaRole || moved[0].links != o.links {
			t.Fatalf("obligation #%d must have exactly one open successor copy keeping via_role and links: before %+v after %+v", o.seq, o, moved)
		}
	}
	if n := countRows(t, s, `SELECT count(*) FROM obligations WHERE task_id=? AND outcome=?`, f.task.ID, api.OutcomeRecipientGone); n != 0 {
		t.Fatalf("%d obligations closed recipient_gone", n)
	}
	openAfter := countRows(t, s, `SELECT count(*) FROM obligations WHERE task_id=? AND agent_id IN (?,?) AND state<>'closed' AND message_seq<>?`, f.task.ID, f.old.ID, successor.ID, committed.Receipt.NoticeSeq)
	if openAfter != openBefore || len(committed.Handoff.Reissued) != openBefore {
		t.Fatalf("open obligations old+successor changed: before %d after %d reissued %d", openBefore, openAfter, len(committed.Handoff.Reissued))
	}
	// Exactly one directed handoff NOTICE, citing the rotation.
	if n := countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=? AND to_agent=? AND envelope LIKE '%"handlerRotation":"`+r.ID+`"%'`, f.task.ID, successor.ID); n != 1 {
		t.Fatalf("handoff notices: %d", n)
	}
	h := committed.Handoff
	if len(h.LiveLeases) != 0 || len(h.PendingScopeConfirmations) != 1 || h.PendingScopeConfirmations[0].EntryID != pending.ID ||
		len(h.AuthoredOpen) != 1 || h.AuthoredOpen[0].MessageSeq != authored.Seq || len(h.AllocationIntents) != 1 || h.AllocationIntents[0].AgentID != intentAgent ||
		len(h.QueueClaims) != 1 || h.QueueClaims[0].EntryID != queueEntry || len(h.RequiredDeliveries) != 1 || h.RequiredDeliveries[0].ID != delivery {
		t.Fatalf("handoff snapshot: %+v", h)
	}
	// Keyed replay returns the identical receipt and writes nothing.
	messagesAfter := countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID)
	obligationsAfter := countRows(t, s, `SELECT count(*) FROM obligations WHERE task_id=?`, f.task.ID)
	replay, err := f.commit(r, "rotate-commit")
	if err != nil || !reflect.DeepEqual(replay.Receipt, committed.Receipt) || !reflect.DeepEqual(replay.Handoff, committed.Handoff) {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID) != messagesAfter || countRows(t, s, `SELECT count(*) FROM obligations WHERE task_id=?`, f.task.ID) != obligationsAfter {
		t.Fatal("commit replay wrote messages or obligations")
	}
	if messagesAfter != messagesBefore+openBefore+1 {
		t.Fatalf("commit wrote %d messages, want %d reissues and one notice", messagesAfter-messagesBefore, openBefore)
	}
	// The old handler is closed with its successor recorded; the successor is primary.
	task, _ = s.GetTask(ctx, f.task.ID)
	oldNow, _ := s.GetAgent(ctx, f.old.ID)
	if task.PrimaryHandlerID != successor.ID || task.HandlerRevision != 2 || oldNow.Status != api.AgentClosed || oldNow.SuccessorID != successor.ID {
		t.Fatalf("primary %q rev %d old %+v", task.PrimaryHandlerID, task.HandlerRevision, oldNow)
	}
	tx, _ = s.db.BeginTx(ctx, nil)
	holder, err = resolveRole(ctx, tx, task, api.RoleDatabaseHandler, "")
	tx.Rollback()
	if err != nil || holder.ID != successor.ID {
		t.Fatalf("role:database_handler must resolve to the successor over a newer-than-old auxiliary: %+v %v", holder, err)
	}
	// A RESULT replying to a request the old handler authored reaches the successor.
	result := api.Envelope{Kind: api.EnvelopeKindResult, Subject: "Fixture handoff confirmed", Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}}, Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "fixture check -> ok"}}}
	reply, err := s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{AgentID: f.worker.ID, RunID: f.worker.RunID, ReplyTo: authored.Seq, Envelope: &result, Text: api.RenderText(result)}, f.by)
	if err != nil || reply.To != successor.ID {
		t.Fatalf("reply to rotated author: to=%q %v", reply.To, err)
	}
	// The record survives a hub restart unchanged.
	s.Close()
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	readback, err := reopened.GetHandlerRotation(ctx, f.task.ID, r.ID)
	if err != nil || !reflect.DeepEqual(readback.Handoff, committed.Handoff) || !reflect.DeepEqual(readback.Receipt, committed.Receipt) || readback.State != api.HandlerRotationCommitted {
		t.Fatalf("restart readback: %+v %v", readback, err)
	}
}

func TestHandlerRotationRefusedWhileLeasedOrWorkingChangesNothing(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	s := f.s
	f.send(t, f.worker, api.EnvelopeKindRequest, f.old.ID, "Fixture work held during refusals", false)
	snapshot := func() [4]int {
		return [4]int{countRows(t, s, `SELECT count(*) FROM handler_rotations`), countRows(t, s, `SELECT count(*) FROM agents WHERE task_id=?`, f.task.ID),
			countRows(t, s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>'closed'`, f.old.ID), countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID)}
	}
	base := snapshot()
	q, err := s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "lease-add", Operation: "add", ItemID: f.items[0].ID, OrderMessageSeq: f.orders[0].Seq, Host: "mini", Cwd: "/lease"})
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range []struct{ state, released string }{{"launching", ""}, {"running", ""}, {"failed", ""}} {
		if _, err = s.db.Exec(`UPDATE team_queue_entries SET state=?,released_at=?,handler_id=?,handler_run_id=?,handler_lease_generation=1 WHERE id=?`, lease.state, lease.released, f.old.ID, f.old.RunID, q.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = f.prepare(t, "leased-"+lease.state, api.Agent{}, ""); refusalCode(err) != api.HandlerRotationRefusedLiveLease {
			t.Fatalf("%s lease: %v", lease.state, err)
		}
		if snapshot() != base {
			t.Fatalf("refused prepare changed state (%s)", lease.state)
		}
	}
	// A released failure and a finished item are not live leases.
	if _, err = s.db.Exec(`UPDATE team_queue_entries SET state='failed',released_at=? WHERE id=?`, ts(s.now()), q.ID); err != nil {
		t.Fatal(err)
	}
	for _, busy := range []struct{ state, tool, code string }{
		{"working", "", api.HandlerRotationRefusedWorking},
		{"hung_tool", "", api.HandlerRotationRefusedWorking},
		{"looping", "", api.HandlerRotationRefusedWorking},
		{"finished_silent", "Bash", api.HandlerRotationRefusedPendingTool},
		{"hung_tool", "Bash", api.HandlerRotationRefusedPendingTool},
	} {
		f.activity(t, f.old, busy.state, busy.tool)
		if _, err = f.prepare(t, "busy-"+busy.state+busy.tool, api.Agent{}, ""); refusalCode(err) != busy.code {
			t.Fatalf("%s/%s: %v", busy.state, busy.tool, err)
		}
		if snapshot() != base {
			t.Fatalf("refused prepare changed state (%s)", busy.state)
		}
		f.idle(t, f.old)
	}
	// Commit re-checks: busy between prepare and commit leaves it prepared.
	r, err := f.prepare(t, "recheck-prepare", api.Agent{}, "")
	if err != nil {
		t.Fatal(err)
	}
	successor := api.AddAgentRequest{AgentID: r.SuccessorAgentID, Role: api.AgentRoleDatabaseHandler, Name: r.SuccessorName, Host: "mini", Session: "tt-handler-recheck", Runtime: f.old.Runtime, Cwd: f.old.Cwd}
	succ, err := s.AddAgent(ctx, f.task.ID, successor, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: succ.ID, RunID: succ.RunID, Kind: api.EventStarted}, f.by); err != nil {
		t.Fatal(err)
	}
	f.activity(t, f.old, "working", "")
	if _, err = f.commit(r, "recheck-commit"); refusalCode(err) != api.HandlerRotationRefusedWorking {
		t.Fatalf("commit re-check: %v", err)
	}
	if _, err = s.db.Exec(`UPDATE team_queue_entries SET state='running',released_at='',handler_id=?,handler_run_id=? WHERE id=?`, f.old.ID, f.old.RunID, q.ID); err != nil {
		t.Fatal(err)
	}
	f.idle(t, f.old)
	if _, err = f.commit(r, "recheck-commit-2"); refusalCode(err) != api.HandlerRotationRefusedLiveLease {
		t.Fatalf("commit lease re-check: %v", err)
	}
	task, _ := s.GetTask(ctx, f.task.ID)
	still, _ := s.GetHandlerRotation(ctx, f.task.ID, r.ID)
	if still.State != api.HandlerRotationPrepared || task.PrimaryHandlerID != "" || task.HandlerRevision != 1 || countRows(t, s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>'closed'`, succ.ID) != 0 {
		t.Fatalf("refused commit changed state: %+v %+v", still, task)
	}
	// Resumes when idle (h2): the lease releases, the same rotation commits.
	if _, err = s.db.Exec(`UPDATE team_queue_entries SET state='finished',released_at=? WHERE id=?`, ts(s.now()), q.ID); err != nil {
		t.Fatal(err)
	}
	committed, err := f.commit(r, "recheck-commit-3")
	if err != nil || committed.State != api.HandlerRotationCommitted || committed.Receipt.Reissued != 1 {
		t.Fatalf("idle commit: %+v %v", committed, err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM agents WHERE task_id=? AND role=?`, f.task.ID, api.AgentRoleDatabaseHandler); n != 2 {
		t.Fatalf("handlers: %d", n)
	}
}

func TestHandlerRotationRunnerTriggerNeedsObservedIdle(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	if _, err := f.s.db.Exec(`DELETE FROM agent_activity WHERE agent_id=?`, f.old.ID); err != nil {
		t.Fatal(err)
	}
	task, _ := f.s.GetTask(ctx, f.task.ID)
	req := api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: "runner-unknown", ExpectedHandlerRevision: task.HandlerRevision, OldAgentID: f.old.ID, OldRunID: f.old.RunID,
		SuccessorAgentID: api.NewID("agt"), SuccessorName: "database-r2", Reason: api.HandlerRotationReasonTokens, Trigger: api.HandlerRotationTriggerRunner}
	if _, err := f.s.HandlerRotationAction(ctx, f.task.ID, req, f.by); refusalCode(err) != api.HandlerRotationRefusedWorking {
		t.Fatalf("runner with unobserved activity: %v", err)
	}
	req.Trigger, req.RequestID, req.Reason = api.HandlerRotationTriggerOwner, "owner-unknown", api.HandlerRotationReasonManual
	if _, err := f.s.HandlerRotationAction(ctx, f.task.ID, req, f.by); err != nil {
		t.Fatalf("owner command treats unobserved activity as idle: %v", err)
	}
}

func TestHandlerRotationPrepareGuardsAndAbort(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	s := f.s
	agentReq := api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: "agent-caller", ActorAgentID: f.worker.ID}
	if _, err := s.HandlerRotationAction(ctx, f.task.ID, agentReq, f.by); refusalCode(err) != api.HandlerRotationRefusedAgentCaller {
		t.Fatalf("agent caller: %v", err)
	}
	if _, err := f.prepare(t, "wrong-name", api.Agent{}, "database-r9"); refusalCode(err) != api.HandlerRotationRefusedNameTaken {
		t.Fatalf("successor name: %v", err)
	}
	stale := api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: "not-primary", ExpectedHandlerRevision: 1, OldAgentID: f.worker.ID, OldRunID: f.worker.RunID,
		SuccessorAgentID: api.NewID("agt"), SuccessorName: "database-r2", Reason: api.HandlerRotationReasonManual, Trigger: api.HandlerRotationTriggerOwner}
	if _, err := s.HandlerRotationAction(ctx, f.task.ID, stale, f.by); refusalCode(err) != api.HandlerRotationRefusedNotPrimary {
		t.Fatalf("not primary: %v", err)
	}
	r, err := f.prepare(t, "abort-prepare", api.Agent{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.prepare(t, "second-prepare", api.Agent{}, ""); refusalCode(err) != api.HandlerRotationRefusedOpen {
		t.Fatalf("second open rotation: %v", err)
	}
	if _, err = s.HandlerRotationAction(ctx, f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: "abort-prepare", Reason: "manual"}, f.by); err == nil {
		t.Fatal("reused prepare key with different input accepted")
	}
	succ, err := s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: r.SuccessorAgentID, Role: api.AgentRoleDatabaseHandler, Name: r.SuccessorName, Host: "mini", Session: "tt-handler-abort", Runtime: f.old.Runtime, Cwd: f.old.Cwd}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	obligations := countRows(t, s, `SELECT count(*) FROM obligations WHERE task_id=?`, f.task.ID)
	aborted, err := s.HandlerRotationAction(ctx, f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationAbort, RequestID: "abort", RotationID: r.ID}, f.by)
	if err != nil || aborted.State != api.HandlerRotationAborted {
		t.Fatalf("abort: %+v %v", aborted, err)
	}
	succ, _ = s.GetAgent(ctx, succ.ID)
	old, _ := s.GetAgent(ctx, f.old.ID)
	task, _ := s.GetTask(ctx, f.task.ID)
	if succ.Status != api.AgentClosed || old.Status == api.AgentClosed || task.PrimaryHandlerID != "" || task.HandlerRevision != 1 ||
		countRows(t, s, `SELECT count(*) FROM obligations WHERE task_id=?`, f.task.ID) != obligations {
		t.Fatalf("abort must close the successor and move nothing: succ %+v old %+v task %+v", succ, old, task)
	}
	if _, err = f.commit(r, "commit-after-abort"); err == nil {
		t.Fatal("aborted rotation committed")
	}
	if again, err := f.prepare(t, "after-abort", api.Agent{}, ""); err != nil || again.SuccessorName != "database-r2" {
		t.Fatalf("a new prepare after abort: %+v %v", again, err)
	}
}

func TestHandlerRotationLeaseExclusionAndDueList(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	s := f.s
	q, err := s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "exclusion-add", Operation: "add", ItemID: f.items[0].ID, OrderMessageSeq: f.orders[0].Seq, Host: "mini", Cwd: "/exclusion"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.prepare(t, "exclusion-prepare", api.Agent{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "exclusion-claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"}); err == nil {
		t.Fatal("a queued entry was leased to the handler of an open rotation")
	}
	if _, err = s.HandlerRotationAction(ctx, f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationAbort, RequestID: "exclusion-abort", RotationID: r.ID}, f.by); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "exclusion-claim-2", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
	if err != nil || claimed.HandlerID != f.old.ID {
		t.Fatalf("after abort the handler leases again: %+v %v", claimed, err)
	}
	// Due list: counts the exact run's finished leases and total tokens.
	if _, err = s.db.Exec(`UPDATE team_queue_entries SET state='finished',released_at=? WHERE id=?`, ts(s.now()), q.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = s.ReportActivity(ctx, f.task.ID, f.old.ID, api.ActivityReport{RequestID: "tokens", RunID: f.old.RunID, Activity: api.AgentActivity{State: "finished_silent", ObservedAt: now, LastEventAt: now, Tokens: api.TokenTotals{Total: 1234}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO handler_runs(task_id,agent_id,run_id,template_digest,created_at) VALUES(?,?,?,?,?)`, f.task.ID, f.old.ID, f.old.RunID, "aaaa", ts(s.now())); err != nil {
		t.Fatal(err)
	}
	list, err := s.HandlerRotationsDue(ctx, "mini", "aaaa")
	if err != nil || len(list.Entries) != 1 {
		t.Fatalf("due: %+v %v", list, err)
	}
	d := list.Entries[0]
	if d.Agent.ID != f.old.ID || d.FinishedItems != 1 || d.TotalTokens != 1234 || !d.DigestMatches || !d.Idle || len(d.DueReasons) != 0 || d.OpenRotation != nil {
		t.Fatalf("due entry: %+v", d)
	}
	if _, err = s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{Enabled: true, MaxItems: 1, MaxTotalTokens: 1234, OnTemplateChange: true}); err != nil {
		t.Fatal(err)
	}
	list, _ = s.HandlerRotationsDue(ctx, "mini", "bbbb")
	if got := list.Entries[0].DueReasons; !reflect.DeepEqual(got, []string{"items", "tokens", "template"}) {
		t.Fatalf("due reasons: %v", got)
	}
	if list, _ = s.HandlerRotationsDue(ctx, "other-host", "aaaa"); len(list.Entries) != 0 {
		t.Fatalf("other host: %+v", list)
	}
	if _, err = s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: 1, Enabled: false, MaxItems: 1}); err != nil {
		t.Fatal(err)
	}
	if list, _ = s.HandlerRotationsDue(ctx, "mini", "aaaa"); len(list.Entries) != 0 {
		t.Fatalf("disabled policy listed: %+v", list)
	}
}

func TestHandlerRotationTemplateDigestRecordedPerRun(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	a, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Role: api.AgentRoleDatabaseHandler, Name: "digest-handler", Host: "mini", Session: "digest", TemplateDigest: digest}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	var recorded string
	if err = f.s.db.QueryRow(`SELECT template_digest FROM handler_runs WHERE run_id=?`, a.RunID).Scan(&recorded); err != nil || recorded != digest {
		t.Fatalf("digest: %q %v", recorded, err)
	}
	// Replaying the launch of an existing run records nothing new.
	legacy := api.AddAgentRequest{AgentID: f.old.ID, Role: api.AgentRoleDatabaseHandler, Name: f.old.Name, Host: f.old.Host, Session: f.old.Session, Runtime: f.old.Runtime, Cwd: f.old.Cwd, TemplateDigest: digest}
	if again, err := f.s.AddAgent(ctx, f.task.ID, legacy, f.by); err != nil || again.RunID != f.old.RunID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM handler_runs WHERE run_id=?`, f.old.RunID); n != 0 {
		t.Fatal("a replayed launch marked a legacy run's template current")
	}
	if _, err = f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "worker-digest", Host: "mini", Session: "w", TemplateDigest: digest}, f.by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("a non-handler template digest: %v", err)
	}
}
