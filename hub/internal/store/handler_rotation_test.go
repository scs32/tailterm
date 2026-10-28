package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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

// commit makes the registered successor ready, as its first successful turn
// would (readyTurn), then commits. commitAsIs leaves readiness to the test.
func (f *rotationFixture) commit(r api.HandlerRotation, key string) (api.HandlerRotation, error) {
	if err := f.readyTurn(r); err != nil {
		return api.HandlerRotation{}, err
	}
	return f.commitAsIs(r, key)
}

func (f *rotationFixture) commitAsIs(r api.HandlerRotation, key string) (api.HandlerRotation, error) {
	return f.s.HandlerRotationAction(context.Background(), f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationCommit, RequestID: key, RotationID: r.ID}, f.by)
}

// readyTurn reports, for the rotation's successor when it is registered and
// not yet ready, a working turn and then a completed one with a model reply,
// observed no earlier than the prepare: the readiness a commit needs (bug
// wi_ce7310979bc45479, order #46760).
func (f *rotationFixture) readyTurn(r api.HandlerRotation) error {
	ctx := context.Background()
	a, err := scanAgent(f.s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=?`, f.task.ID, r.SuccessorAgentID))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (a.Status == api.AgentClosed || a.Status == api.AgentExited)) {
		return nil
	}
	if err != nil {
		return err
	}
	if ok, err := successorReady(ctx, f.s.db, f.task.ID, a, r.CreatedAt); err != nil || ok {
		return err
	}
	return reportReadyTurn(f.s, f.task.ID, a, r.CreatedAt)
}

// reportReadyTurn reports a working turn, then a completed one with output.
func reportReadyTurn(s *Store, task string, a api.Agent, notBefore time.Time) error {
	at := s.now().UTC()
	if at.Before(notBefore) {
		at = notBefore
	}
	for i, state := range []string{"working", "idle"} {
		activity := api.AgentActivity{State: state, ObservedAt: at, LastEventAt: at, Tokens: api.TokenTotals{Output: 1, Total: 1}}
		if _, err := s.ReportActivity(context.Background(), task, a.ID, api.ActivityReport{RequestID: fmt.Sprintf("ready-%s-%d-%s", a.RunID, i, api.NewID("act")), RunID: a.RunID, Activity: activity}); err != nil {
			return err
		}
	}
	return nil
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

// p2 (wi_0f6d57014268f0ee): a run's template digest, model and reasoning are
// written in the transaction that starts the run, so a failed record leaves
// no half-registered run.
func TestHandlerRotationRunRecordIsAtomicWithRegistration(t *testing.T) {
	ctx := context.Background()
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	failOn := func(t *testing.T, f *rotationFixture, table string) func() {
		t.Helper()
		if _, err := f.s.db.Exec(`CREATE TRIGGER fixture_fail BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
			t.Fatal(err)
		}
		return func() {
			if _, err := f.s.db.Exec(`DROP TRIGGER fixture_fail`); err != nil {
				t.Fatal(err)
			}
		}
	}
	added := func(t *testing.T, f *rotationFixture, id string) int {
		t.Helper()
		return countRows(t, f.s, `SELECT count(*) FROM events WHERE task_id=? AND agent_id=? AND kind=?`, f.task.ID, id, api.EventAgentAdded)
	}
	t.Run("fresh handler", func(t *testing.T) {
		f := newRotationFixture(t)
		req := api.AddAgentRequest{AgentID: api.NewID("agt"), Role: api.AgentRoleDatabaseHandler, Name: "atomic-handler", Host: "mini", Session: "atomic",
			TemplateDigest: digest, HandlerModel: "gpt-5", HandlerReasoning: "high"}
		restore := failOn(t, f, "handler_runs")
		if _, err := f.s.AddAgent(ctx, f.task.ID, req, f.by); err == nil {
			t.Fatal("a failed run record registered the handler")
		}
		if n := countRows(t, f.s, `SELECT count(*) FROM agents WHERE id=?`, req.AgentID); n != 0 || added(t, f, req.AgentID) != 0 {
			t.Fatalf("a failed record left %d agent row(s) or an agent_added event", n)
		}
		restore()
		a, err := f.s.AddAgent(ctx, f.task.ID, req, f.by)
		if err != nil {
			t.Fatal(err)
		}
		var d, m, r string
		if err = f.s.db.QueryRow(`SELECT template_digest,model,reasoning FROM handler_runs WHERE agent_id=? AND run_id=?`, a.ID, a.RunID).Scan(&d, &m, &r); err != nil || d != digest || m != "gpt-5" || r != "high" {
			t.Fatalf("retried record: %q %q %q %v", d, m, r, err)
		}
	})
	t.Run("exited handler restart", func(t *testing.T) {
		f := newRotationFixture(t)
		if _, err := f.s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, f.old.ID); err != nil {
			t.Fatal(err)
		}
		req := api.AddAgentRequest{AgentID: f.old.ID, Role: api.AgentRoleDatabaseHandler, Name: f.old.Name, Host: f.old.Host, Session: f.old.Session, Runtime: f.old.Runtime, Cwd: f.old.Cwd,
			ExpectedRunID: f.old.RunID, TemplateDigest: digest, HandlerModel: "gpt-5"}
		events := added(t, f, f.old.ID)
		restore := failOn(t, f, "handler_runs")
		if _, err := f.s.AddAgent(ctx, f.task.ID, req, f.by); err == nil {
			t.Fatal("a failed run record restarted the handler")
		}
		if a, err := f.s.GetAgent(ctx, f.old.ID); err != nil || a.Status != api.AgentExited || a.RunID != f.old.RunID || added(t, f, f.old.ID) != events {
			t.Fatalf("a failed record changed the exited handler: %+v %v", a, err)
		}
		restore()
		a, err := f.s.AddAgent(ctx, f.task.ID, req, f.by)
		if err != nil || a.RunID == f.old.RunID {
			t.Fatalf("restart: %+v %v", a, err)
		}
		var d, m string
		if err = f.s.db.QueryRow(`SELECT template_digest,model FROM handler_runs WHERE agent_id=? AND run_id=?`, a.ID, a.RunID).Scan(&d, &m); err != nil || d != digest || m != "gpt-5" {
			t.Fatalf("restart record: %q %q %v", d, m, err)
		}
	})
	t.Run("steward", func(t *testing.T) {
		f := newRotationFixture(t)
		id := api.NewID("agt")
		req := api.AddAgentRequest{Role: api.AgentRoleBacklogSteward, AgentID: id, Name: "backlog-steward", Host: "mini", Session: "tt-steward-" + strings.TrimPrefix(id, "agt_"),
			Runtime: "claude", Cwd: "/tmp", TemplateDigest: digest}
		restore := failOn(t, f, "steward_runs")
		if _, err := f.s.AddAgent(ctx, f.task.ID, req, f.by); err == nil {
			t.Fatal("a failed run record registered the steward")
		}
		if n := countRows(t, f.s, `SELECT count(*) FROM agents WHERE id=?`, id); n != 0 || added(t, f, id) != 0 {
			t.Fatalf("a failed record left %d steward row(s) or an agent_added event", n)
		}
		restore()
		a, err := f.s.AddAgent(ctx, f.task.ID, req, f.by)
		if err != nil {
			t.Fatal(err)
		}
		var d string
		if err = f.s.db.QueryRow(`SELECT template_digest FROM steward_runs WHERE agent_id=? AND run_id=?`, a.ID, a.RunID).Scan(&d); err != nil || d != digest {
			t.Fatalf("steward record: %q %v", d, err)
		}
	})
	t.Run("invalid values write nothing", func(t *testing.T) {
		f := newRotationFixture(t)
		for name, req := range map[string]api.AddAgentRequest{
			"digest":    {AgentID: api.NewID("agt"), Role: api.AgentRoleDatabaseHandler, Name: "bad-digest", Host: "mini", Session: "bad", TemplateDigest: "XYZ"},
			"model":     {AgentID: api.NewID("agt"), Role: api.AgentRoleDatabaseHandler, Name: "bad-model", Host: "mini", Session: "bad", HandlerModel: "bad model\n"},
			"reasoning": {AgentID: api.NewID("agt"), Role: api.AgentRoleDatabaseHandler, Name: "bad-reasoning", Host: "mini", Session: "bad", HandlerReasoning: "bad reasoning\n"},
		} {
			if _, err := f.s.AddAgent(ctx, f.task.ID, req, f.by); !errors.Is(err, api.ErrInvalid) {
				t.Fatalf("%s: %v", name, err)
			}
			if n := countRows(t, f.s, `SELECT count(*) FROM agents WHERE id=?`, req.AgentID); n != 0 {
				t.Fatalf("%s: an invalid record registered the agent", name)
			}
		}
	})
}

// Owner-authorized change of the primary's runtime, model or arm
// (wi_f250a91c85367e6f, order #20511).

// asRun gives the old primary an arm's runtime and recorded model.
func (f *rotationFixture) asRun(t *testing.T, arm api.HandlerArm) {
	t.Helper()
	if _, err := f.s.db.Exec(`UPDATE agents SET runtime=? WHERE id=?`, arm.Runtime, f.old.ID); err != nil {
		t.Fatal(err)
	}
	f.old.Runtime = arm.Runtime
	if _, err := f.s.db.Exec(`INSERT OR REPLACE INTO handler_runs(task_id,agent_id,run_id,template_digest,model,reasoning,created_at) VALUES(?,?,?,?,?,?,?)`,
		f.task.ID, f.old.ID, f.old.RunID, digestP, arm.Model, arm.Reasoning, ts(time.Now())); err != nil {
		t.Fatal(err)
	}
}

// armPolicy saves an enabled S (Claude) and O (Codex) arm policy.
func (f *rotationFixture) armPolicy(t *testing.T) {
	t.Helper()
	if _, err := f.s.SetHandlerArmPolicy(context.Background(), f.task.ID, api.HandlerArmPolicyRequest{RequestID: "arms", Enabled: true, Fallback: true, Seed: "K",
		TemplateDigest: digestP, Arms: []api.HandlerArm{armS, armO}}); err != nil {
		t.Fatal(err)
	}
}

func (f *rotationFixture) armPolicyRow(t *testing.T) string {
	t.Helper()
	var row string
	if err := f.s.db.QueryRow(`SELECT enabled||'|'||seed||'|'||fallback||'|'||limit_hold_minutes||'|'||template_digest||'|'||arms_json||'|'||revision||'|'||updated_at FROM handler_arm_policy WHERE task_id=?`, f.task.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	return row
}

func (f *rotationFixture) prepareAs(key, trigger, authorizedBy, reason string) (api.HandlerRotation, error) {
	return f.s.HandlerRotationAction(context.Background(), f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: key,
		ExpectedHandlerRevision: 1, OldAgentID: f.old.ID, OldRunID: f.old.RunID, SuccessorAgentID: api.NewID("agt"), SuccessorName: api.HandlerSuccessorName(f.old.Name, 1),
		Reason: api.HandlerRotationReasonManual, Trigger: trigger, AuthorizedBy: authorizedBy, AuthorizationReason: reason}, f.by)
}

// successor registers the prepared successor online on the old handler's
// host and directory, as tt spawn records its runtime, model and reasoning.
func (f *rotationFixture) successor(t *testing.T, r api.HandlerRotation, arm api.HandlerArm) api.Agent {
	t.Helper()
	ctx := context.Background()
	a, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: r.SuccessorAgentID, Role: api.AgentRoleDatabaseHandler, Name: r.SuccessorName, Host: "mini",
		Session: "successor-" + r.ID, Runtime: arm.Runtime, Cwd: f.old.Cwd, TemplateDigest: digestP, HandlerModel: arm.Model, HandlerReasoning: arm.Reasoning}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted}, f.by); err != nil {
		t.Fatal(err)
	}
	if a, err = f.s.GetAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	return a
}

// assertNothingMoved checks a refused commit: the old handler stays the open
// primary at revision 1 and every obligation is as it was.
func (f *rotationFixture) assertNothingMoved(t *testing.T, before []rotationObligation) {
	t.Helper()
	ctx := context.Background()
	task, err := f.s.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	old, err := f.s.GetAgent(ctx, f.old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.PrimaryHandlerID != "" || task.HandlerRevision != 1 || old.Status == api.AgentClosed || old.SuccessorID != "" || !reflect.DeepEqual(before, openObligations(t, f.s, f.task.ID)) {
		t.Fatalf("a refused commit moved something: primary %q revision %d old %+v", task.PrimaryHandlerID, task.HandlerRevision, old)
	}
}

func TestHandlerRotationAuthorizedRuntimeChange(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	s := f.s
	f.asRun(t, armO)
	f.armPolicy(t)
	policy := f.armPolicyRow(t)
	authored := f.send(t, f.old, api.EnvelopeKindRequest, f.worker.ID, "Please confirm the cross-runtime handoff", false)
	held := []api.Message{
		f.send(t, f.worker, api.EnvelopeKindRequest, "role:"+api.RoleDatabaseHandler, "Record the role-addressed fixture item", true),
		f.send(t, f.worker, api.EnvelopeKindRequest, f.old.ID, "Record the name-addressed fixture item", false),
	}
	before := openObligations(t, s, f.task.ID)
	const who, why = "owner", "Owner decision: move the primary handler from Codex to Sonnet"
	r, err := f.prepareAs("prepare", api.HandlerRotationTriggerOwner, who, why)
	if err != nil {
		t.Fatal(err)
	}
	if a := r.Authorization; a == nil || a.AuthorizedBy != who || a.Reason != why || a.OldRuntime != "" || a.SuccessorRuntime != "" {
		t.Fatalf("prepared authorization: %+v", r.Authorization)
	}
	var prepared map[string]any
	var data string
	if err = s.db.QueryRow(`SELECT data FROM events WHERE task_id=? AND text='Handler rotation prepared'`, f.task.ID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(data), &prepared); err != nil || prepared["authorizedBy"] != who || prepared["authorizationReason"] != why {
		t.Fatalf("prepared event: %s %v", data, err)
	}
	successor := f.successor(t, r, armS)
	if successor.Runtime != "claude" || f.old.Runtime != "codex" {
		t.Fatalf("fixture runtimes: old %q successor %q", f.old.Runtime, successor.Runtime)
	}
	committed, err := f.commit(r, "commit")
	if err != nil {
		t.Fatal(err)
	}
	if committed.State != api.HandlerRotationCommitted || committed.Receipt == nil || committed.Handoff == nil || committed.Receipt.PrimaryHandlerID != successor.ID || committed.Receipt.ClosedAgentID != f.old.ID {
		t.Fatalf("committed: %+v", committed)
	}
	// Every open obligation moved once, to a new message seq.
	h := committed.Handoff
	if len(h.Reissued) != len(held) || len(h.AuthoredOpen) != 1 || h.AuthoredOpen[0].MessageSeq != authored.Seq || h.PendingScopeConfirmations == nil ||
		h.AllocationIntents == nil || h.QueueClaims == nil || h.RequiredDeliveries == nil || len(h.LiveLeases) != 0 {
		t.Fatalf("handoff: %+v", h)
	}
	after := map[string]rotationObligation{}
	for _, o := range openObligations(t, s, f.task.ID) {
		after[o.id] = o
	}
	for i, pair := range h.Reissued {
		moved := after[pair.NewObligationID]
		if pair.OldMessageSeq != held[i].Seq || pair.NewMessageSeq <= held[len(held)-1].Seq || moved.seq != pair.NewMessageSeq || moved.agent != successor.ID || moved.state == api.ObligationClosed {
			t.Fatalf("obligation #%d must move to the successor with a new seq: %+v -> %+v", held[i].Seq, pair, moved)
		}
		if old := after[pair.OldObligationID]; old.state != api.ObligationClosed || old.outcome != api.OutcomeSuperseded {
			t.Fatalf("old obligation not superseded: %+v", old)
		}
	}
	for _, o := range before {
		if o.agent == f.old.ID && after[o.id].outcome != api.OutcomeSuperseded {
			t.Fatalf("obligation %s stayed with the old handler: %+v", o.id, after[o.id])
		}
	}
	// The successor is primary, the old handler is closed.
	task, _ := s.GetTask(ctx, f.task.ID)
	oldNow, _ := s.GetAgent(ctx, f.old.ID)
	if task.PrimaryHandlerID != successor.ID || task.HandlerRevision != 2 || oldNow.Status != api.AgentClosed || oldNow.SuccessorID != successor.ID {
		t.Fatalf("primary %q revision %d old %+v", task.PrimaryHandlerID, task.HandlerRevision, oldNow)
	}
	// One directed handoff NOTICE names the rotation and who authorized it.
	var notice string
	if err = s.db.QueryRow(`SELECT text FROM messages WHERE task_id=? AND seq=? AND to_agent=? AND envelope LIKE '%"handlerRotation":"`+r.ID+`"%'`, f.task.ID, committed.Receipt.NoticeSeq, successor.ID).Scan(&notice); err != nil {
		t.Fatalf("handoff notice: %v", err)
	}
	for _, want := range []string{r.ID, " This change from runtime codex to claude and model gpt-6.1-sol to claude-sonnet-5-5 was authorized by " + who + ": " + why} {
		if !strings.Contains(notice, want) {
			t.Fatalf("handoff notice misses %q:\n%s", want, notice)
		}
	}
	if strings.Contains(notice, "reasoning") {
		t.Fatalf("handoff notice names an unchanged reasoning:\n%s", notice)
	}
	// The record and the committed event carry the authorization and runtimes.
	want := api.HandlerRotationAuthorization{AuthorizedBy: who, Reason: why, OldRuntime: "codex", SuccessorRuntime: "claude"}
	readback, err := s.GetHandlerRotation(ctx, f.task.ID, r.ID)
	if err != nil || committed.Authorization == nil || *committed.Authorization != want || readback.Authorization == nil || *readback.Authorization != want {
		t.Fatalf("authorization on the record: committed %+v readback %+v %v", committed.Authorization, readback.Authorization, err)
	}
	var event map[string]any
	if err = s.db.QueryRow(`SELECT data FROM events WHERE task_id=? AND text='Handler rotation committed'`, f.task.ID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(data), &event); err != nil || event["authorizedBy"] != who || event["authorizationReason"] != why || event["oldRuntime"] != "codex" || event["successorRuntime"] != "claude" {
		t.Fatalf("committed event: %s %v", data, err)
	}
	// The arm policy is not written.
	if now := f.armPolicyRow(t); now != policy {
		t.Fatalf("arm policy changed:\n%s\n%s", policy, now)
	}
	// A keyed replay returns the same record and writes nothing.
	messages := countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID)
	replay, err := f.commit(r, "commit")
	if err != nil || !reflect.DeepEqual(replay.Receipt, committed.Receipt) || replay.Authorization == nil || *replay.Authorization != want || countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID) != messages {
		t.Fatalf("replay: %+v %v", replay, err)
	}
}

// The handoff NOTICE names only what an authorized change changed: a model-only
// change never reads "from runtime X to X" (wi_7dd116c81497132e).
func TestHandlerRotationAuthorizedNoticeNamesOnlyChanges(t *testing.T) {
	for name, tc := range map[string]struct {
		successor api.HandlerArm
		change    string
	}{
		"model only":     {api.HandlerArm{Runtime: "codex", Model: "gpt-6.1-mini", Reasoning: "high"}, " from model gpt-6.1-sol to gpt-6.1-mini"},
		"reasoning only": {api.HandlerArm{Runtime: "codex", Model: "gpt-6.1-sol", Reasoning: "medium"}, " from reasoning high to medium"},
		"all three":      {api.HandlerArm{Runtime: "claude", Model: "claude-sonnet-5-5", Reasoning: "medium"}, " from runtime codex to claude, model gpt-6.1-sol to claude-sonnet-5-5 and reasoning high to medium"},
		"nothing":        {armO, ""},
		"unrecorded":     {api.HandlerArm{Runtime: "codex"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRotationFixture(t)
			f.asRun(t, armO)
			const who, why = "owner", "Owner decision: try another handler model"
			r, err := f.prepareAs("prepare", api.HandlerRotationTriggerOwner, who, why)
			if err != nil {
				t.Fatal(err)
			}
			successor := f.successor(t, r, tc.successor)
			committed, err := f.commit(r, "commit")
			if err != nil {
				t.Fatal(err)
			}
			var notice string
			if err = f.s.db.QueryRow(`SELECT text FROM messages WHERE task_id=? AND seq=? AND to_agent=?`, f.task.ID, committed.Receipt.NoticeSeq, successor.ID).Scan(&notice); err != nil {
				t.Fatalf("handoff notice: %v", err)
			}
			if want := " This change" + tc.change + " was authorized by " + who + ": " + why; !strings.HasSuffix(notice, want) {
				t.Fatalf("handoff notice must end %q:\n%s", want, notice)
			}
			if tc.successor.Runtime == armO.Runtime && strings.Contains(notice, "runtime codex") {
				t.Fatalf("handoff notice names an unchanged runtime:\n%s", notice)
			}
		})
	}
}

func TestHandlerRotationRuntimeChangeNeedsAuthorization(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, old api.HandlerArm) (*rotationFixture, []rotationObligation) {
		f := newRotationFixture(t)
		f.asRun(t, old)
		f.armPolicy(t)
		f.send(t, f.worker, api.EnvelopeKindRequest, "role:"+api.RoleDatabaseHandler, "Record the role-addressed fixture item", true)
		f.send(t, f.worker, api.EnvelopeKindRequest, f.old.ID, "Record the name-addressed fixture item", false)
		return f, openObligations(t, f.s, f.task.ID)
	}
	t.Run("a runtime change is refused", func(t *testing.T) {
		f, before := setup(t, armO)
		policy := f.armPolicyRow(t)
		r, err := f.prepareAs("prepare", api.HandlerRotationTriggerOwner, "", "")
		if err != nil || r.Authorization != nil {
			t.Fatalf("prepare: %+v %v", r, err)
		}
		f.successor(t, r, armS)
		if _, err = f.commit(r, "commit"); refusalCode(err) != api.HandlerRotationRefusedSuccessor || !strings.Contains(err.Error(), "--authorized-by") {
			t.Fatalf("unauthorized runtime change: %v", err)
		}
		f.assertNothingMoved(t, before)
		if got, err := f.s.GetHandlerRotation(ctx, f.task.ID, r.ID); err != nil || got.State != api.HandlerRotationPrepared || got.Authorization != nil || f.armPolicyRow(t) != policy {
			t.Fatalf("refused rotation: %+v %v", got, err)
		}
	})
	t.Run("an arm change on the same runtime is refused", func(t *testing.T) {
		f, before := setup(t, armS)
		r, err := f.prepareAs("prepare", api.HandlerRotationTriggerOwner, "", "")
		if err != nil {
			t.Fatal(err)
		}
		f.successor(t, r, api.HandlerArm{Runtime: "claude", Model: "claude-opus-5-5", Reasoning: "high"})
		if _, err = f.commit(r, "commit"); refusalCode(err) != api.HandlerRotationRefusedArmChanged {
			t.Fatalf("unauthorized arm change: %v", err)
		}
		f.assertNothingMoved(t, before)
	})
	t.Run("half or runner authorization is invalid", func(t *testing.T) {
		f, before := setup(t, armO)
		for name, c := range map[string]struct{ trigger, by, reason string }{
			"no reason":    {api.HandlerRotationTriggerOwner, "owner", ""},
			"no name":      {api.HandlerRotationTriggerOwner, "", "move to Sonnet"},
			"blank name":   {api.HandlerRotationTriggerOwner, "  ", "move to Sonnet"},
			"blank reason": {api.HandlerRotationTriggerOwner, "owner", " \t"},
			"long name":    {api.HandlerRotationTriggerOwner, strings.Repeat("n", 201), "move to Sonnet"},
			"long reason":  {api.HandlerRotationTriggerOwner, "owner", strings.Repeat("r", 1001)},
			"control char": {api.HandlerRotationTriggerOwner, "owner", "move\x00"},
			"runner":       {api.HandlerRotationTriggerRunner, "owner", "move to Sonnet"},
		} {
			if _, err := f.prepareAs("prepare-"+name, c.trigger, c.by, c.reason); !errors.Is(err, api.ErrInvalid) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if n := countRows(t, f.s, `SELECT count(*) FROM handler_rotations WHERE task_id=?`, f.task.ID); n != 0 {
			t.Fatalf("invalid authorization wrote %d rotation row(s)", n)
		}
		f.assertNothingMoved(t, before)
	})
}

func TestHandlerRotationModelChangeWithoutArmPolicy(t *testing.T) {
	ctx := context.Background()
	opus := api.HandlerArm{Runtime: "claude", Model: "claude-opus-5-5", Reasoning: "max"}
	rotate := func(t *testing.T, f *rotationFixture, key string, to api.HandlerArm) (api.HandlerRotation, api.Agent, error) {
		t.Helper()
		r, err := f.prepareAs("prepare-"+key, api.HandlerRotationTriggerOwner, "", "")
		if err != nil {
			t.Fatal(err)
		}
		a := f.successor(t, r, to)
		committed, err := f.commit(r, "commit-"+key)
		if err == nil && (committed.State != api.HandlerRotationCommitted || committed.Authorization != nil) {
			t.Fatalf("committed: %+v", committed)
		}
		return r, a, err
	}
	primary := func(t *testing.T, f *rotationFixture) string {
		t.Helper()
		task, err := f.s.GetTask(ctx, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return task.PrimaryHandlerID
	}
	t.Run("no policy", func(t *testing.T) {
		f := newRotationFixture(t)
		f.asRun(t, armS)
		if _, a, err := rotate(t, f, "no-policy", opus); err != nil || primary(t, f) != a.ID {
			t.Fatalf("a model change without an arm policy: %v", err)
		}
	})
	t.Run("primary in no arm", func(t *testing.T) {
		f := newRotationFixture(t)
		f.asRun(t, api.HandlerArm{Runtime: "claude", Model: "claude-haiku-4-5", Reasoning: "high"})
		f.armPolicy(t)
		if _, a, err := rotate(t, f, "no-arm", opus); err != nil || primary(t, f) != a.ID {
			t.Fatalf("a model change for a primary in no arm: %v", err)
		}
	})
	t.Run("arm handler stays in its arm", func(t *testing.T) {
		f := newRotationFixture(t)
		f.asRun(t, armS)
		f.armPolicy(t)
		before := openObligations(t, f.s, f.task.ID)
		r, a, err := rotate(t, f, "other-model", opus)
		if refusalCode(err) != api.HandlerRotationRefusedArmChanged {
			t.Fatalf("another model for an arm handler: %v", err)
		}
		f.assertNothingMoved(t, before)
		if _, err = f.s.HandlerRotationAction(ctx, f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationAbort, RequestID: "abort", RotationID: r.ID}, f.by); err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.CloseAgent(ctx, a.ID, f.by); err != nil && !errors.Is(err, api.ErrClosed) {
			t.Fatal(err)
		}
		if _, a, err = rotate(t, f, "same-arm", armS); err != nil || primary(t, f) != a.ID {
			t.Fatalf("a same-arm rotation: %v", err)
		}
	})
}

// A hub that predates the authorization columns gains them on open; its old
// rotations read back with no authorization.
func TestHandlerRotationAuthorizationColumnsMigrate(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	r, err := f.prepareAs("prepare", api.HandlerRotationTriggerOwner, "", "")
	if err != nil {
		t.Fatal(err)
	}
	f.successor(t, r, api.HandlerArm{Runtime: f.old.Runtime})
	if _, err = f.commit(r, "commit"); err != nil {
		t.Fatal(err)
	}
	columns := func(s *Store) int {
		return countRows(t, s, `SELECT count(*) FROM pragma_table_info('handler_rotations') WHERE name IN ('authorized_by','authorization_reason','old_runtime','successor_runtime')`)
	}
	for _, column := range []string{"authorized_by", "authorization_reason", "old_runtime", "successor_runtime"} {
		if _, err = f.s.db.Exec(`ALTER TABLE handler_rotations DROP COLUMN ` + column); err != nil {
			t.Fatal(err)
		}
	}
	if n := columns(f.s); n != 0 {
		t.Fatalf("old schema still has %d authorization column(s)", n)
	}
	f.s.Close()
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatalf("an old database must open: %v", err)
	}
	defer reopened.Close()
	if n := columns(reopened); n != 4 {
		t.Fatalf("migration added %d of 4 columns", n)
	}
	old, err := reopened.GetHandlerRotation(ctx, f.task.ID, r.ID)
	if err != nil || old.State != api.HandlerRotationCommitted || old.Authorization != nil || old.Receipt == nil {
		t.Fatalf("old rotation readback: %+v %v", old, err)
	}
	list, err := reopened.ListHandlerRotations(ctx, f.task.ID)
	if err != nil || len(list) != 1 || list[0].Authorization != nil {
		t.Fatalf("old rotation list: %+v %v", list, err)
	}
	// Opening again changes nothing.
	reopened.Close()
	again, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if n := columns(again); n != 4 {
		t.Fatalf("second open left %d columns", n)
	}
}

// Dead primary rotation (wi_b863e669073858f4, owner order #28057). The store
// clock and the host's evidence are injected; no process, tmux server or
// handler is read, started, stopped or rotated.

type deadPrimaryFixture struct {
	*rotationFixture
	clock    time.Time
	running  api.TeamQueueEntry
	frozen   api.TeamQueueEntry
	plan     string
	revision api.WorkItemRevision
	helper   api.Agent
	requests []api.Message
}

const deadPrimaryStarted = "Wed Oct  7 06:00:00 2026"

// newDeadPrimaryFixture is a primary last recorded as working with a pending
// tool, holding a running lease and a frozen-launch lease, with two open
// requests and one work-item revision it saved. Its last recorded activity is
// at the fixture clock; advance moves the clock on from there.
func newDeadPrimaryFixture(t *testing.T) *deadPrimaryFixture {
	t.Helper()
	f := &deadPrimaryFixture{rotationFixture: newRotationFixture(t), clock: time.Now().UTC().Truncate(time.Millisecond)}
	ctx := context.Background()
	s := f.s
	s.now = func() time.Time { return f.clock }
	helper, err := s.RegisterOwnerHelper(ctx, f.task.ID, api.RegisterOwnerHelperRequest{Host: "owner-host", Session: "owner", Runtime: "claude", Cwd: "/work/tailterm", RequestID: "dead-helper"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if f.helper, err = s.GetAgent(ctx, helper.Agent.ID); err != nil {
		t.Fatal(err)
	}
	for i, cwd := range []string{"/dead-running", "/dead-frozen"} {
		q, err := s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "dead-add-" + cwd[1:], Operation: "add", ItemID: f.items[i].ID, OrderMessageSeq: f.orders[i].Seq, Host: "mini", Cwd: cwd})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			f.running = q
		} else {
			f.frozen = q
		}
	}
	// The dead run's own writes: a work-item save and a message.
	title := "Queued work, saved by the handler"
	saved, _, err := s.CreateWorkItemUpdate(ctx, f.task.ID, f.items[1].ID, api.CreateWorkItemUpdate{ExpectedRevision: f.items[1].Revision, Title: &title, AgentID: f.old.ID, RunID: f.old.RunID, RequestID: "dead-save"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	f.revision = saved.Revision
	f.send(t, f.old, api.EnvelopeKindNotice, f.worker.ID, "Fixture notice from the handler", false)
	f.requests = []api.Message{
		f.send(t, f.worker, api.EnvelopeKindRequest, f.old.ID, "First fixture request held by the handler", false),
		f.send(t, f.worker, api.EnvelopeKindRequest, "role:database_handler", "Second fixture request held by role", false),
	}
	// A frozen plan with spacing and key order a re-marshal would not keep.
	f.plan = `{"version":1, "hub":"http://fixture", "task":"` + f.task.ID + `","item":"` + f.items[1].ID + `", "revision":1,"order":` + jsonInt(f.orders[1].Seq) +
		`, "handlerId":"` + f.old.ID + `" ,"handlerRunId": "` + f.old.RunID + `","handlerLeaseGeneration":5, "context":{"b":1,"a":[1, 2]},"members":[{"fields":{"agentId":"agt_0000000000000001","name":"lead-x","cwd":"/dead-frozen"},"state":"unstarted","runId":"run_0000000000000001"}]}`
	if _, err = s.db.Exec(`UPDATE team_queue_entries SET state='running',handler_id=?,handler_run_id=?,handler_lease_generation=1 WHERE id=?`, f.old.ID, f.old.RunID, f.running.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE team_queue_entries SET state='launching',handler_id=?,handler_run_id=?,handler_lease_generation=5,launch_json=? WHERE id=?`, f.old.ID, f.old.RunID, f.plan, f.frozen.ID); err != nil {
		t.Fatal(err)
	}
	if err = withTx(s, func(tx *sql.Tx) error {
		return insertHandlerArmAssignment(ctx, tx, f.task.ID, api.TeamQueueEntry{ID: f.running.ID, ItemID: f.items[0].ID},
			api.TeamQueueHandlerArm{LeaseGeneration: 1, PolicyRevision: 3, Draw: "0a", DrawnArm: "S", Arm: "S", HandlerID: f.old.ID, HandlerRunID: f.old.RunID, HandlerDigest: "digest-as-leased", PolicyDigest: "policy", LeasedAt: ts(f.clock), SkippedLimited: []string{}})
	}); err != nil {
		t.Fatal(err)
	}
	f.busy(t, "working", "Bash")
	f.heartbeat(t)
	if f.running, err = s.GetTeamQueueEntry(ctx, f.task.ID, f.running.ID); err != nil {
		t.Fatal(err)
	}
	if f.frozen, err = s.GetTeamQueueEntry(ctx, f.task.ID, f.frozen.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func withTx(s *Store, fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (f *deadPrimaryFixture) advance(d time.Duration) { f.clock = f.clock.Add(d) }

// busy records the old run's activity as observed at the fixture clock.
func (f *deadPrimaryFixture) busy(t *testing.T, state, tool string) {
	t.Helper()
	activity := api.AgentActivity{State: state, ObservedAt: f.clock, LastEventAt: f.clock, PendingTool: tool}
	if tool != "" {
		activity.PendingSince = f.clock
	}
	if _, err := f.s.ReportActivity(context.Background(), f.task.ID, f.old.ID, api.ActivityReport{RequestID: api.NewID("act"), RunID: f.old.RunID, Activity: activity}); err != nil {
		t.Fatal(err)
	}
}

// heartbeat is the old run's wrapper heartbeat at the fixture clock.
func (f *deadPrimaryFixture) heartbeat(t *testing.T) {
	t.Helper()
	if _, err := f.s.db.Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(f.clock), f.old.ID); err != nil {
		t.Fatal(err)
	}
}

// evidence is what a host that confirmed the death sends, observed now.
func (f *deadPrimaryFixture) evidence() *api.HandlerDeathEvidence {
	return &api.HandlerDeathEvidence{Host: f.old.Host, AgentID: f.old.ID, RunID: f.old.RunID, ObservedAt: f.clock, SessionName: "tt-handler-fixture", SessionID: "$7", SessionCreated: "1700000000",
		SessionState: api.HandlerDeathStateGone, PID: 4242, PanePID: 4200, ProcessStarted: deadPrimaryStarted, ProcessState: api.HandlerDeathStateGone}
}

func (f *deadPrimaryFixture) prepareDead(t *testing.T, key string, change func(*api.HandlerRotationRequest)) (api.HandlerRotation, error) {
	t.Helper()
	task, err := f.s.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: key, ExpectedHandlerRevision: task.HandlerRevision, OldAgentID: f.old.ID, OldRunID: f.old.RunID,
		SuccessorAgentID: api.NewID("agt"), SuccessorName: api.HandlerSuccessorName(f.old.Name, task.HandlerRevision), Reason: api.HandlerRotationReasonDeadPrimary,
		Trigger: api.HandlerRotationTriggerRunner, DeathEvidence: f.evidence()}
	if change != nil {
		change(&req)
	}
	return f.s.HandlerRotationAction(context.Background(), f.task.ID, req, f.by)
}

func (f *deadPrimaryFixture) commitDead(r api.HandlerRotation, key string, ev *api.HandlerDeathEvidence) (api.HandlerRotation, error) {
	if err := f.readyTurn(r); err != nil {
		return api.HandlerRotation{}, err
	}
	return f.s.HandlerRotationAction(context.Background(), f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationCommit, RequestID: key, RotationID: r.ID, DeathEvidence: ev}, f.by)
}

// succeed registers the prepared rotation's successor, online at the clock.
func (f *deadPrimaryFixture) succeed(t *testing.T, r api.HandlerRotation) api.Agent {
	t.Helper()
	ctx := context.Background()
	a, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: r.SuccessorAgentID, Role: api.AgentRoleDatabaseHandler, Name: r.SuccessorName, Host: f.old.Host, Session: "tt-handler-successor", Runtime: f.old.Runtime, Cwd: f.old.Cwd}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted}, f.by); err != nil {
		t.Fatal(err)
	}
	if a, err = f.s.GetAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	return a
}

// deadState is everything a refused dead primary request must leave alone.
type deadState struct {
	rotations, agents, openOld, messages, handlerRevision int
	primary, runningLease, frozenLease, plan, assignment  string
}

func (f *deadPrimaryFixture) state(t *testing.T) deadState {
	t.Helper()
	s := f.s
	var d deadState
	d.rotations = countRows(t, s, `SELECT count(*) FROM handler_rotations WHERE state<>'aborted'`)
	d.agents = countRows(t, s, `SELECT count(*) FROM agents WHERE task_id=? AND status<>'closed'`, f.task.ID)
	d.openOld = countRows(t, s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>'closed'`, f.old.ID)
	d.messages = countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID)
	d.handlerRevision = countRows(t, s, `SELECT handler_revision FROM tasks WHERE id=?`, f.task.ID)
	lease := `SELECT handler_id||'/'||handler_run_id||'/'||handler_lease_generation||'/'||revision FROM team_queue_entries WHERE id=?`
	for _, q := range []struct {
		to    *string
		query string
		arg   string
	}{{&d.primary, `SELECT primary_handler_id FROM tasks WHERE id=?`, f.task.ID}, {&d.runningLease, lease, f.running.ID}, {&d.frozenLease, lease, f.frozen.ID},
		{&d.plan, `SELECT CAST(launch_json AS TEXT) FROM team_queue_entries WHERE id=?`, f.frozen.ID},
		{&d.assignment, `SELECT handler_id||'/'||handler_run_id||'/'||lease_generation FROM handler_arm_assignments WHERE entry_id=?`, f.running.ID}} {
		if err := s.db.QueryRow(q.query, q.arg).Scan(q.to); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func noticesTo(t *testing.T, s *Store, task, agent string) []api.Message {
	t.Helper()
	messages, err := s.ListMessages(context.Background(), task, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range messages {
		if m.Envelope != nil && m.Envelope.Subject == deadPrimaryNoticeSubject && m.To == agent {
			out = append(out, m)
		}
	}
	return out
}

// rotateDeadPrimary runs the whole accepted path: 11 minutes of silence,
// prepare with evidence, a registered successor and a commit with a second,
// later observation.
func rotateDeadPrimary(t *testing.T) (*deadPrimaryFixture, api.HandlerRotation, api.Agent) {
	t.Helper()
	f := newDeadPrimaryFixture(t)
	f.advance(11 * time.Minute)
	r, err := f.prepareDead(t, "dead-prepare", nil)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	successor := f.succeed(t, r)
	f.advance(30 * time.Second)
	committed, err := f.commitDead(r, "rotation-commit-"+r.ID, f.evidence())
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return f, committed, successor
}

// T1: a dead, silent, busy primary is rotated through prepare and commit, and
// everything it held is on the successor after the one commit.
func TestHandlerRotationDeadSilentPrimaryRotates(t *testing.T) {
	f, r, successor := rotateDeadPrimary(t)
	ctx := context.Background()
	s := f.s
	if r.State != api.HandlerRotationCommitted || r.Reason != api.HandlerRotationReasonDeadPrimary || r.Trigger != api.HandlerRotationTriggerRunner ||
		r.DeathEvidence == nil || r.CommitDeathEvidence == nil || !r.CommitDeathEvidence.ObservedAt.After(r.DeathEvidence.ObservedAt) || r.LastActivityAt == nil {
		t.Fatalf("rotation record: %+v", r)
	}
	task, _ := s.GetTask(ctx, f.task.ID)
	old, _ := s.GetAgent(ctx, f.old.ID)
	if task.PrimaryHandlerID != successor.ID || task.HandlerRevision != 2 || old.Status != api.AgentClosed {
		t.Fatalf("primary %s revision %d old %s", task.PrimaryHandlerID, task.HandlerRevision, old.Status)
	}
	// Leases: both entries name the successor's exact run with new generations.
	running, _ := s.GetTeamQueueEntry(ctx, f.task.ID, f.running.ID)
	frozen, _ := s.GetTeamQueueEntry(ctx, f.task.ID, f.frozen.ID)
	for i, e := range []api.TeamQueueEntry{running, frozen} {
		before := []api.TeamQueueEntry{f.running, f.frozen}[i]
		if e.HandlerID != successor.ID || e.HandlerRunID != successor.RunID || e.HandlerLeaseGeneration != int64(i+1) || e.Revision != before.Revision+1 || e.State != before.State || e.ReleasedAt != "" {
			t.Fatalf("lease %d not moved: %+v", i, e)
		}
	}
	if n := countRows(t, s, `SELECT count(*) FROM team_queue_entries WHERE handler_id=?`, f.old.ID); n != 0 {
		t.Fatalf("leases left on the dead handler: %d", n)
	}
	// The stored launch plan names the successor and is otherwise byte-identical.
	want := strings.Replace(f.plan, `"handlerId":"`+f.old.ID+`"`, `"handlerId":"`+successor.ID+`"`, 1)
	want = strings.Replace(want, `"handlerRunId": "`+f.old.RunID+`"`, `"handlerRunId": "`+successor.RunID+`"`, 1)
	want = strings.Replace(want, `"handlerLeaseGeneration":5,`, `"handlerLeaseGeneration":2,`, 1) // the successor's second lease
	if string(frozen.LaunchJSON) != want || want == f.plan {
		t.Fatalf("launch plan:\n got %s\nwant %s", frozen.LaunchJSON, want)
	}
	var plan struct {
		HandlerID              string `json:"handlerId"`
		HandlerRunID           string `json:"handlerRunId"`
		HandlerLeaseGeneration int64  `json:"handlerLeaseGeneration"`
	}
	if err := json.Unmarshal(frozen.LaunchJSON, &plan); err != nil || plan.HandlerID != frozen.HandlerID || plan.HandlerRunID != frozen.HandlerRunID || plan.HandlerLeaseGeneration != frozen.HandlerLeaseGeneration {
		t.Fatalf("launch plan does not match the entry: %+v %v", plan, err)
	}
	// The arm assignment follows the lease and keeps what was leased.
	if running.HandlerArm == nil || running.HandlerArm.HandlerID != successor.ID || running.HandlerArm.HandlerRunID != successor.RunID || running.HandlerArm.LeaseGeneration != 1 ||
		running.HandlerArm.HandlerDigest != "digest-as-leased" || running.HandlerArm.Draw != "0a" || running.HandlerArm.Arm != "S" {
		t.Fatalf("arm assignment: %+v", running.HandlerArm)
	}
	// Obligations: each of the two requests reissued once to the successor.
	h := r.Handoff
	if h == nil || len(h.Reissued) != 2 || countRows(t, s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>'closed'`, f.old.ID) != 0 {
		t.Fatalf("handoff reissued: %+v", h)
	}
	if n := countRows(t, s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>'closed' AND source_kind='request'`, successor.ID); n != 2 {
		t.Fatalf("requests on the successor: %d", n)
	}
	if len(h.LiveLeases) != 2 || h.LiveLeases[0].EntryID != f.running.ID || h.LiveLeases[0].LeaseGeneration != 1 || h.LiveLeases[0].NewLeaseGeneration != 1 || h.LiveLeases[0].LaunchPlanRewritten ||
		h.LiveLeases[1].EntryID != f.frozen.ID || h.LiveLeases[1].LeaseGeneration != 5 || h.LiveLeases[1].NewLeaseGeneration != 2 || !h.LiveLeases[1].LaunchPlanRewritten {
		t.Fatalf("handoff leases: %+v", h.LiveLeases)
	}
	// In flight: reported, not recovered.
	in := h.InFlight
	if in == nil || in.ActivityState != "working" || in.PendingTool != "Bash" || in.SilenceMinutes != 10 || !in.LastActivityAt.Equal(*r.LastActivityAt) || in.WritesOmitted != 0 ||
		len(in.RecentWrites) != 1 || in.RecentWrites[0].ItemID != f.revision.ItemID || in.RecentWrites[0].Revision != f.revision.Revision || !reflect.DeepEqual(in.RecentWrites[0].ChangedFields, []string{"title"}) {
		t.Fatalf("in flight: %+v", in)
	}
	// One notice each to the successor and the owner helper, with the same text.
	toSuccessor, toHelper := noticesTo(t, s, f.task.ID, successor.ID), noticesTo(t, s, f.task.ID, f.helper.ID)
	if len(toSuccessor) != 1 || len(toHelper) != 1 || toSuccessor[0].Seq != r.Receipt.NoticeSeq || toHelper[0].Seq != r.Receipt.OwnerNoticeSeq || r.Receipt.LeasesMoved != 2 || r.Receipt.Reissued != 2 {
		t.Fatalf("notices: successor %d helper %d receipt %+v", len(toSuccessor), len(toHelper), r.Receipt)
	}
	text := toSuccessor[0].Envelope.Body.Text
	if text != toHelper[0].Envelope.Body.Text || toSuccessor[0].Envelope.Refs["handlerRotation"] != r.ID || toHelper[0].Envelope.Refs["handlerRotation"] != r.ID {
		t.Fatalf("the two copies differ:\n%s\n%s", text, toHelper[0].Envelope.Body.Text)
	}
	for _, part := range []string{
		"Primary database handler " + f.old.Name + " (" + f.old.ID + " / " + f.old.RunID + ")",
		"Evidence from host mini: tmux session tt-handler-fixture is absent and runtime process 4242 (started " + deadPrimaryStarted + ") is absent, as is pane process 4200",
		"the silence applied was 10 minute(s)",
		"2 open obligation(s) and 2 team lease(s) (" + f.running.ID + ", " + f.frozen.ID + ")",
		"last activity working, pending tool Bash, 1 recent work-item save(s)",
		"tt handler rotation get " + r.ID,
		"owner order #28057",
		"To turn it off for this project: tt handler policy set --task " + f.task.ID + " --revision 0 --dead-silence-minutes 0",
	} {
		if !strings.Contains(text, part) {
			t.Fatalf("notice lacks %q:\n%s", part, text)
		}
	}
	if len(text) > 2000 {
		t.Fatalf("notice is %d bytes", len(text))
	}
	if n := countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=? AND to_agent=? AND text LIKE '%Handler rotation handoff%'`, f.task.ID, successor.ID); n != 0 {
		t.Fatalf("the ordinary handoff notice was posted too: %d", n)
	}
	// A replayed commit, even with a newer observation, adds nothing.
	messages := countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID)
	f.advance(20 * time.Second)
	again, err := f.commitDead(r, "rotation-commit-"+r.ID, f.evidence())
	if err != nil || again.State != api.HandlerRotationCommitted || again.Receipt.NoticeSeq != r.Receipt.NoticeSeq || !again.CommitDeathEvidence.ObservedAt.Equal(r.CommitDeathEvidence.ObservedAt) {
		t.Fatalf("replayed commit: %+v %v", again, err)
	}
	if countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID) != messages || len(noticesTo(t, s, f.task.ID, successor.ID)) != 1 || len(noticesTo(t, s, f.task.ID, f.helper.ID)) != 1 {
		t.Fatal("a replayed commit posted again")
	}
	// The record survives a read back.
	read, err := s.GetHandlerRotation(ctx, f.task.ID, r.ID)
	if err != nil || read.Handoff.InFlight == nil || read.DeathEvidence.PID != 4242 || read.CommitDeathEvidence == nil || len(read.Handoff.LiveLeases) != 2 {
		t.Fatalf("read back: %+v %v", read, err)
	}
}

// T1, no owner helper: the owner's copy of the notice is posted board-wide.
func TestHandlerRotationDeadPrimaryNoticeIsBoardWideWithoutOwnerHelper(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	if _, err := f.s.db.Exec(`UPDATE agents SET status='closed' WHERE id=?`, f.helper.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(11 * time.Minute)
	r, err := f.prepareDead(t, "dead-prepare", nil)
	if err != nil {
		t.Fatal(err)
	}
	successor := f.succeed(t, r)
	f.advance(time.Second)
	if r, err = f.commitDead(r, "dead-commit", f.evidence()); err != nil {
		t.Fatal(err)
	}
	if len(noticesTo(t, f.s, f.task.ID, successor.ID)) != 1 || len(noticesTo(t, f.s, f.task.ID, "")) != 1 || len(noticesTo(t, f.s, f.task.ID, f.helper.ID)) != 0 || r.Receipt.OwnerNoticeSeq == 0 {
		t.Fatalf("notices without an owner helper: receipt %+v", r.Receipt)
	}
}

// T2: a dead primary that was active within the silence waits. Any of the
// run's own records counts, by the store clock.
func TestHandlerRotationDeadButRecentlyActiveWaits(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	base := f.state(t)
	f.advance(9 * time.Minute)
	if _, err := f.prepareDead(t, "nine-minutes", nil); refusalCode(err) != api.HandlerRotationRefusedNotSilent {
		t.Fatalf("9 minutes of silence: %v", err)
	}
	if f.state(t) != base {
		t.Fatal("a refused prepare changed state")
	}
	// A message from the run at minute 8 keeps it not silent at minute 15.
	if _, err := f.s.db.Exec(`UPDATE messages SET created_at=? WHERE task_id=? AND from_agent=? AND from_run_id=?`, ts(f.clock.Add(-time.Minute)), f.task.ID, f.old.ID, f.old.RunID); err != nil {
		t.Fatal(err)
	}
	f.advance(6 * time.Minute)
	if _, err := f.prepareDead(t, "message-at-eight", nil); refusalCode(err) != api.HandlerRotationRefusedNotSilent {
		t.Fatalf("a message 7 minutes ago: %v", err)
	}
	// So does a work-item revision the run saved.
	if _, err := f.s.db.Exec(`UPDATE messages SET created_at=? WHERE task_id=? AND from_agent=?`, ts(f.clock.Add(-time.Hour)), f.task.ID, f.old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE work_item_revisions SET updated_at=? WHERE updated_agent=? AND updated_run_id=?`, ts(f.clock.Add(-2*time.Minute)), f.old.ID, f.old.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.prepareDead(t, "revision-two-minutes-ago", nil); refusalCode(err) != api.HandlerRotationRefusedNotSilent {
		t.Fatalf("a revision 2 minutes ago: %v", err)
	}
	if f.state(t) != base {
		t.Fatal("a refused prepare changed state")
	}
	// A run with no recorded activity at all is not silent either.
	if _, err := f.s.db.Exec(`UPDATE agents SET last_seen_at='' WHERE id=?`, f.old.ID); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DELETE FROM agent_activity WHERE agent_id=?`, `UPDATE messages SET from_run_id='' WHERE from_agent=?`, `UPDATE work_item_revisions SET updated_run_id='' WHERE updated_agent=?`} {
		if _, err := f.s.db.Exec(q, f.old.ID); err != nil {
			t.Fatal(err)
		}
	}
	f.advance(time.Hour)
	if _, err := f.prepareDead(t, "no-record", nil); refusalCode(err) != api.HandlerRotationRefusedNotSilent {
		t.Fatalf("no recorded activity: %v", err)
	}
}

// T3: an alive, busy primary is never rotated: without evidence the request
// is invalid, with a recent heartbeat the death is unconfirmed, and the owner
// trigger cannot carry evidence.
func TestHandlerRotationAliveBusyPrimaryNeverRotates(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	base := f.state(t)
	f.advance(11 * time.Minute)
	if _, err := f.prepareDead(t, "no-evidence", func(r *api.HandlerRotationRequest) { r.DeathEvidence = nil }); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("dead_primary without evidence: %v", err)
	}
	for name, change := range map[string]func(*api.HandlerRotationRequest){
		"owner-trigger":    func(r *api.HandlerRotationRequest) { r.Trigger = api.HandlerRotationTriggerOwner },
		"owner-authorized": func(r *api.HandlerRotationRequest) { r.AuthorizedBy, r.AuthorizationReason = "owner", "fixture" },
		"manual-reason":    func(r *api.HandlerRotationRequest) { r.Reason = api.HandlerRotationReasonManual },
		"limit-reason":     func(r *api.HandlerRotationRequest) { r.Reason = api.HandlerRotationReasonTokens },
		"control-chars":    func(r *api.HandlerRotationRequest) { r.DeathEvidence.SessionName = "bad\nname" },
	} {
		if _, err := f.prepareDead(t, "invalid-"+name, change); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The hub saw a heartbeat 30 seconds ago: whatever the host says, not dead.
	f.advance(-30 * time.Second)
	f.heartbeat(t)
	f.advance(30 * time.Second)
	if _, err := f.prepareDead(t, "heartbeat", nil); refusalCode(err) != api.HandlerRotationRefusedDeathUnconfirmed {
		t.Fatalf("heartbeat 30 seconds ago: %v", err)
	}
	// Without evidence the busy primary is refused exactly as before.
	if _, err := f.prepare(t, "owner-busy", api.Agent{}, ""); refusalCode(err) != api.HandlerRotationRefusedLiveLease {
		t.Fatalf("owner rotation of a leased primary: %v", err)
	}
	task, _ := f.s.GetTask(context.Background(), f.task.ID)
	runner := api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: "runner-busy", ExpectedHandlerRevision: task.HandlerRevision, OldAgentID: f.old.ID, OldRunID: f.old.RunID,
		SuccessorAgentID: api.NewID("agt"), SuccessorName: api.HandlerSuccessorName(f.old.Name, task.HandlerRevision), Reason: api.HandlerRotationReasonItems, Trigger: api.HandlerRotationTriggerRunner}
	if _, err := f.s.HandlerRotationAction(context.Background(), f.task.ID, runner, f.by); refusalCode(err) != api.HandlerRotationRefusedLiveLease {
		t.Fatalf("runner limit rotation of a leased primary: %v", err)
	}
	if f.state(t) != base {
		t.Fatal("a refused request changed state")
	}
}

// T4: an unknown or unreadable host state is never confirmation.
func TestHandlerRotationUnknownHostStateNeverRotates(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	base := f.state(t)
	f.advance(11 * time.Minute)
	for name, change := range map[string]func(*api.HandlerDeathEvidence){
		"session-unknown":   func(e *api.HandlerDeathEvidence) { e.SessionState = "unknown" },
		"session-alive":     func(e *api.HandlerDeathEvidence) { e.SessionState = "alive" },
		"process-unknown":   func(e *api.HandlerDeathEvidence) { e.ProcessState = "unknown" },
		"process-empty":     func(e *api.HandlerDeathEvidence) { e.ProcessState = "" },
		"no-session-name":   func(e *api.HandlerDeathEvidence) { e.SessionName = "" },
		"no-pid":            func(e *api.HandlerDeathEvidence) { e.PID = 0 },
		"no-start-identity": func(e *api.HandlerDeathEvidence) { e.ProcessStarted = "" },
		"another-host":      func(e *api.HandlerDeathEvidence) { e.Host = "air" },
		"no-host":           func(e *api.HandlerDeathEvidence) { e.Host = "" },
		"another-run":       func(e *api.HandlerDeathEvidence) { e.RunID = "run_0000000000000009" },
		"another-agent":     func(e *api.HandlerDeathEvidence) { e.AgentID = f.worker.ID },
		"three-minutes-old": func(e *api.HandlerDeathEvidence) { e.ObservedAt = f.clock.Add(-3 * time.Minute) },
		"one-minute-ahead":  func(e *api.HandlerDeathEvidence) { e.ObservedAt = f.clock.Add(time.Minute) },
		"undated":           func(e *api.HandlerDeathEvidence) { e.ObservedAt = time.Time{} },
	} {
		_, err := f.prepareDead(t, "unknown-"+name, func(r *api.HandlerRotationRequest) { change(r.DeathEvidence) })
		if refusalCode(err) != api.HandlerRotationRefusedDeathUnconfirmed {
			t.Fatalf("%s: %v", name, err)
		}
		if f.state(t) != base {
			t.Fatalf("%s changed state", name)
		}
	}
	// The same evidence, unchanged, is accepted: the cases above failed on
	// the one field each changed.
	if _, err := f.prepareDead(t, "valid", nil); err != nil {
		t.Fatalf("valid evidence: %v", err)
	}
}

// T5: activity recorded between prepare and commit refuses the commit and
// moves nothing; the abort leaves the old handler primary with its leases.
func TestHandlerRotationDeadPrimaryActivityBeforeCommitMovesNothing(t *testing.T) {
	for _, activity := range []string{"heartbeat", "message", "activity"} {
		t.Run(activity, func(t *testing.T) {
			f := newDeadPrimaryFixture(t)
			f.advance(11 * time.Minute)
			r, err := f.prepareDead(t, "prepare", nil)
			if err != nil {
				t.Fatal(err)
			}
			successor := f.succeed(t, r)
			prepared := f.state(t)
			f.advance(100 * time.Second)
			want := api.HandlerRotationRefusedNotSilent
			switch activity {
			case "heartbeat":
				// Seen within the online window as well, so the death is unconfirmed.
				f.heartbeat(t)
				want = api.HandlerRotationRefusedDeathUnconfirmed
				prepared = f.state(t)
			case "message":
				if _, err := f.s.db.Exec(`INSERT INTO messages (task_id,from_agent,from_node,from_user,to_agent,text,created_at,reply_to,broadcast,from_run_id,envelope) VALUES (?,?,'fixture','owner','','late write',?,0,1,?,'')`,
					f.task.ID, f.old.ID, ts(f.clock), f.old.RunID); err != nil {
					t.Fatal(err)
				}
				prepared = f.state(t)
			case "activity":
				if _, err := f.s.db.Exec(`UPDATE agent_activity SET observed_at=? WHERE agent_id=?`, ts(f.clock), f.old.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.advance(10 * time.Second)
			if _, err := f.commitDead(r, "commit", f.evidence()); refusalCode(err) != want {
				t.Fatalf("commit after %s: %v", activity, err)
			}
			still, _ := f.s.GetHandlerRotation(context.Background(), f.task.ID, r.ID)
			if f.state(t) != prepared || still.State != api.HandlerRotationPrepared || still.CommitDeathEvidence != nil ||
				countRows(t, f.s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>'closed'`, successor.ID) != 0 || len(noticesTo(t, f.s, f.task.ID, successor.ID)) != 0 {
				t.Fatalf("a refused commit moved something: %+v", still)
			}
			aborted, err := f.s.HandlerRotationAction(context.Background(), f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationAbort, RequestID: "abort", RotationID: r.ID}, f.by)
			if err != nil || aborted.State != api.HandlerRotationAborted {
				t.Fatalf("abort: %+v %v", aborted, err)
			}
			after := f.state(t)
			old, _ := f.s.GetAgent(context.Background(), f.old.ID)
			gone, _ := f.s.GetAgent(context.Background(), successor.ID)
			if after.runningLease != prepared.runningLease || after.frozenLease != prepared.frozenLease || after.plan != f.plan || after.assignment != prepared.assignment ||
				after.openOld != 2 || after.handlerRevision != 1 || old.Status == api.AgentClosed || gone.Status != api.AgentClosed {
				t.Fatalf("abort changed the old handler's holdings: %+v old %s successor %s", after, old.Status, gone.Status)
			}
		})
	}
}

// T6: a commit needs a second, fresh observation.
func TestHandlerRotationDeadPrimaryCommitNeedsFreshEvidence(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	f.advance(11 * time.Minute)
	first := f.evidence()
	r, err := f.prepareDead(t, "prepare", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.succeed(t, r)
	prepared := f.state(t)
	f.advance(30 * time.Second)
	stale := f.evidence()
	stale.ObservedAt = f.clock.Add(-3 * time.Minute)
	alive := f.evidence()
	alive.ProcessState = "alive"
	unnamed := f.evidence()
	unnamed.SessionName = ""
	for name, ev := range map[string]*api.HandlerDeathEvidence{"none": nil, "the-prepare-observation": first, "stale": stale, "alive": alive, "no-session-name": unnamed} {
		if _, err := f.commitDead(r, "commit-"+name, ev); refusalCode(err) != api.HandlerRotationRefusedDeathUnconfirmed {
			t.Fatalf("commit with %s evidence: %v", name, err)
		}
		if f.state(t) != prepared {
			t.Fatalf("commit with %s evidence changed state", name)
		}
	}
	// Turned off after prepare: the commit is refused too.
	policy, _ := f.s.HandlerRotationPolicy(context.Background(), f.task.ID)
	off := int64(0)
	if _, err := f.s.SetHandlerRotationPolicy(context.Background(), f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: policy.Revision, Enabled: policy.Enabled, DeadSilenceMinutes: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.commitDead(r, "commit-off", f.evidence()); refusalCode(err) != api.HandlerRotationRefusedDeathUnconfirmed {
		t.Fatalf("commit after the replacement was turned off: %v", err)
	}
	// Evidence is refused on a rotation that is not a dead primary one.
	if _, err := f.s.HandlerRotationAction(context.Background(), f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationAbort, RequestID: "abort", RotationID: r.ID}, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE team_queue_entries SET state='finished',released_at=? WHERE task_id=?`, ts(f.clock), f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.busy(t, "idle", "")
	ordinary, err := f.prepare(t, "ordinary", api.Agent{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.commitDead(ordinary, "ordinary-commit", f.evidence()); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("evidence on an ordinary commit: %v", err)
	}
}

// T7: the silence is a policy field: default 10, 0 is off, 1 to 1440 sets it,
// an omitted field keeps it, and it is measured by the store clock.
func TestHandlerRotationDeadSilencePolicy(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	ctx := context.Background()
	s := f.s
	p, err := s.HandlerRotationPolicy(ctx, f.task.ID)
	if err != nil || p.DeadSilenceMinutes != 10 || p.Revision != 0 {
		t.Fatalf("default policy: %+v %v", p, err)
	}
	set := func(minutes *int64) (api.HandlerRotationPolicy, error) {
		current, _ := s.HandlerRotationPolicy(ctx, f.task.ID)
		return s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: current.Revision, Enabled: true, MaxItems: 10, DeadSilenceMinutes: minutes})
	}
	minutes := func(v int64) *int64 { return &v }
	for _, bad := range []int64{-1, 1441} {
		if _, err := set(minutes(bad)); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("silence %d: %v", bad, err)
		}
	}
	// Off: even a dead, long-silent primary is not rotated.
	if p, err = set(minutes(0)); err != nil || p.DeadSilenceMinutes != 0 {
		t.Fatalf("off: %+v %v", p, err)
	}
	base := f.state(t)
	f.advance(time.Hour)
	if _, err := f.prepareDead(t, "off", nil); refusalCode(err) != api.HandlerRotationRefusedDeathUnconfirmed {
		t.Fatalf("prepare with the replacement off: %v", err)
	}
	if f.state(t) != base {
		t.Fatal("a refused prepare changed state")
	}
	// An omitted field keeps the saved value, off included.
	if p, err = set(nil); err != nil || p.DeadSilenceMinutes != 0 {
		t.Fatalf("omitted keeps off: %+v %v", p, err)
	}
	if p, err = set(minutes(3)); err != nil || p.DeadSilenceMinutes != 3 {
		t.Fatalf("three minutes: %+v %v", p, err)
	}
	if p, err = set(nil); err != nil || p.DeadSilenceMinutes != 3 {
		t.Fatalf("omitted keeps three: %+v %v", p, err)
	}
	if read, _ := s.HandlerRotationPolicy(ctx, f.task.ID); read.DeadSilenceMinutes != 3 {
		t.Fatalf("read back: %+v", read)
	}
	// The store clock decides: 2m59s is not silent, 4 minutes is.
	f.heartbeat(t)
	f.advance(3*time.Minute - time.Second)
	if _, err := f.prepareDead(t, "under-three", nil); refusalCode(err) != api.HandlerRotationRefusedNotSilent {
		t.Fatalf("2m59s of silence under a 3 minute policy: %v", err)
	}
	f.advance(time.Minute + time.Second)
	r, err := f.prepareDead(t, "four-minutes", nil)
	if err != nil {
		t.Fatalf("4 minutes of silence under a 3 minute policy: %v", err)
	}
	f.succeed(t, r)
	f.advance(time.Second)
	if r, err = f.commitDead(r, "commit", f.evidence()); err != nil || r.Handoff.InFlight.SilenceMinutes != 3 {
		t.Fatalf("commit: %+v %v", r, err)
	}
}

// T7: a hub that had the policy table before this change gets the silence
// with its default, and its rotations the three new columns.
func TestHandlerRotationDeadSilenceColumnsMigrate(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	if _, err := f.s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{Enabled: true, MaxItems: 4}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`ALTER TABLE handler_rotation_policy DROP COLUMN dead_silence_minutes`, `ALTER TABLE handler_rotations DROP COLUMN evidence_json`,
		`ALTER TABLE handler_rotations DROP COLUMN commit_evidence_json`, `ALTER TABLE handler_rotations DROP COLUMN last_activity_at`} {
		if _, err := f.s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ { // the migration is idempotent
		if err := migrateHandlerRotation(f.s.db); err != nil {
			t.Fatal(err)
		}
	}
	p, err := f.s.HandlerRotationPolicy(ctx, f.task.ID)
	if err != nil || p.DeadSilenceMinutes != api.DefaultHandlerDeadSilenceMinutes || p.MaxItems != 4 || p.Revision != 1 {
		t.Fatalf("migrated policy: %+v %v", p, err)
	}
	if r, err := f.prepare(t, "after-migration", api.Agent{}, ""); err != nil || r.DeathEvidence != nil || r.LastActivityAt != nil {
		t.Fatalf("rotation after migration: %+v %v", r, err)
	}
}

// T8: the due list says, by the store clock, whether the primary is online, a
// dead candidate and silent long enough; a project whose limit policy is off
// is listed only for the dead case.
func TestHandlerRotationDueListDeadCandidate(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	ctx := context.Background()
	s := f.s
	due := func() []api.HandlerRotationDue {
		t.Helper()
		list, err := s.HandlerRotationsDue(ctx, "mini", "")
		if err != nil {
			t.Fatal(err)
		}
		return list.Entries
	}
	// Online and busy: listed for its enabled limit policy, not a candidate.
	d := due()
	if len(d) != 1 || !d[0].Online || d[0].DeadCandidate || d[0].SilenceMet || d[0].LastActivityAt != nil || d[0].Idle || d[0].LiveLeases != 2 {
		t.Fatalf("online busy: %+v", d)
	}
	// Offline and busy, not silent yet.
	f.advance(5 * time.Minute)
	last := f.clock.Add(-5 * time.Minute)
	if d = due(); len(d) != 1 || d[0].Online || !d[0].DeadCandidate || d[0].SilenceMet || d[0].LastActivityAt == nil || !d[0].LastActivityAt.Equal(last) {
		t.Fatalf("offline busy, 5 minutes: %+v", d)
	}
	// Offline, busy and silent for the policy's minutes.
	f.advance(5 * time.Minute)
	if d = due(); len(d) != 1 || d[0].Online || !d[0].DeadCandidate || !d[0].SilenceMet || !d[0].LastActivityAt.Equal(last) {
		t.Fatalf("offline busy, 10 minutes: %+v", d)
	}
	// With the limit policy off the project is still listed for the dead case,
	// and no limit is reported due.
	if _, err := s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{Enabled: false, MaxItems: 1, MaxTotalTokens: 1, OnTemplateChange: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE team_queue_entries SET state='finished',released_at=? WHERE id=?`, ts(f.clock), f.running.ID); err != nil {
		t.Fatal(err)
	}
	if d = due(); len(d) != 1 || !d[0].DeadCandidate || !d[0].SilenceMet || len(d[0].DueReasons) != 0 || d[0].FinishedItems != 1 || d[0].Policy.Enabled {
		t.Fatalf("limit policy off, dead candidate: %+v", d)
	}
	// Dead rotation off as well: not listed at all.
	off := int64(0)
	if _, err := s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: 1, Enabled: false, DeadSilenceMinutes: &off}); err != nil {
		t.Fatal(err)
	}
	if d = due(); len(d) != 0 {
		t.Fatalf("both off: %+v", d)
	}
	// Limit policy on, dead rotation off: listed, never a candidate.
	if _, err := s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: 2, Enabled: true, MaxItems: 1}); err != nil {
		t.Fatal(err)
	}
	if d = due(); len(d) != 1 || d[0].DeadCandidate || d[0].SilenceMet || d[0].Online || !reflect.DeepEqual(d[0].DueReasons, []string{"items"}) {
		t.Fatalf("dead rotation off: %+v", d)
	}
	// Offline and idle: the ordinary rotation applies, so not a candidate; with
	// the limit policy off it is not listed.
	ten := int64(10)
	if _, err := s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: 3, Enabled: false, DeadSilenceMinutes: &ten}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE team_queue_entries SET state='finished',released_at=? WHERE id=?`, ts(f.clock), f.frozen.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent_activity SET state='idle',payload='{}' WHERE agent_id=?`, f.old.ID); err != nil {
		t.Fatal(err)
	}
	if d = due(); len(d) != 0 {
		t.Fatalf("offline idle with the limit policy off: %+v", d)
	}
	if _, err := s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: 4, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if d = due(); len(d) != 1 || d[0].Online || d[0].DeadCandidate || d[0].SilenceMet || !d[0].Idle || d[0].LastActivityAt == nil {
		t.Fatalf("offline idle: %+v", d)
	}
}

// T9: an offline primary that is not busy is not a dead primary rotation; the
// ordinary rotation already accepts it.
func TestHandlerRotationDeadPrimaryNotBusyIsRefused(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	if _, err := f.s.db.Exec(`UPDATE team_queue_entries SET state='finished',released_at=? WHERE task_id=?`, ts(f.clock), f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.busy(t, "idle", "")
	f.advance(11 * time.Minute)
	_, err := f.prepareDead(t, "idle", nil)
	var refusal *api.HandlerRotationRefusal
	if !errors.As(err, &refusal) || refusal.Code != api.HandlerRotationRefusedDeathUnconfirmed || refusal.Detail != "the handler is not busy; the ordinary rotation applies" {
		t.Fatalf("not busy: %v", err)
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM handler_rotations`); n != 0 {
		t.Fatalf("rotations: %d", n)
	}
	if _, err := f.prepare(t, "ordinary", api.Agent{}, ""); err != nil {
		t.Fatalf("the ordinary rotation of an idle offline primary: %v", err)
	}
}

// The launch plan rewrite replaces the three handler fields and nothing else,
// and refuses a plan it cannot hand over exactly.
func TestHandlerRotationLaunchPlanRewrite(t *testing.T) {
	for _, c := range []struct{ name, plan, want string }{
		{"all three", `{"a":1,"handlerId":"agt_old","handlerRunId":"run_old","handlerLeaseGeneration":7,"z":[{"handlerId":"agt_old"}]}`,
			`{"a":1,"handlerId":"agt_new","handlerRunId":"run_new","handlerLeaseGeneration":3,"z":[{"handlerId":"agt_old"}]}`},
		{"spacing and order kept", "{ \"handlerLeaseGeneration\" : 12 ,\n\t\"handlerRunId\":\"run_old\", \"x\":\"handlerId\",\"handlerId\" :  \"agt_old\" }",
			"{ \"handlerLeaseGeneration\" : 3 ,\n\t\"handlerRunId\":\"run_new\", \"x\":\"handlerId\",\"handlerId\" :  \"agt_new\" }"},
		{"omitted fields added", `{"handlerId":"agt_old","context":{}}`, `{"handlerId":"agt_new","handlerRunId":"run_new","handlerLeaseGeneration":3,"context":{}}`},
	} {
		got, changed, err := rewriteLaunchHandler([]byte(c.plan), "agt_old", "agt_new", "run_new", 3)
		if err != nil || !changed || string(got) != c.want {
			t.Fatalf("%s: %q %v %v", c.name, got, changed, err)
		}
	}
	for _, plan := range []string{`{"handlerId":"","members":[]}`, `{"members":[]}`} {
		got, changed, err := rewriteLaunchHandler([]byte(plan), "agt_old", "agt_new", "run_new", 3)
		if err != nil || changed || string(got) != plan {
			t.Fatalf("a plan naming no handler: %q %v %v", got, changed, err)
		}
	}
	for name, plan := range map[string]string{"third handler": `{"handlerId":"agt_other"}`, "not json": `{"handlerId":`, "not an object": `["agt_old"]`,
		"repeated": `{"handlerId":"agt_old","handlerId":"agt_old"}`, "trailing": `{"handlerId":"agt_old"} {}`, "wrong type": `{"handlerId":7}`} {
		if _, _, err := rewriteLaunchHandler([]byte(plan), "agt_old", "agt_new", "run_new", 3); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// A lease whose stored plan names a third handler fails the whole commit:
// nothing moves, and the rotation stays prepared.
func TestHandlerRotationDeadPrimaryUnmovableLeaseFailsCommit(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	bad := strings.Replace(f.plan, f.old.ID, f.worker.ID, 1)
	if _, err := f.s.db.Exec(`UPDATE team_queue_entries SET launch_json=? WHERE id=?`, bad, f.frozen.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(11 * time.Minute)
	r, err := f.prepareDead(t, "prepare", nil)
	if err != nil {
		t.Fatal(err)
	}
	successor := f.succeed(t, r)
	prepared := f.state(t)
	f.advance(time.Second)
	if _, err = f.commitDead(r, "commit", f.evidence()); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), f.frozen.ID) {
		t.Fatalf("commit with an unmovable lease: %v", err)
	}
	still, _ := f.s.GetHandlerRotation(context.Background(), f.task.ID, r.ID)
	if f.state(t) != prepared || still.State != api.HandlerRotationPrepared || countRows(t, f.s, `SELECT count(*) FROM team_queue_entries WHERE handler_id=?`, successor.ID) != 0 {
		t.Fatal("a failed commit moved the first lease")
	}
}

// Exited primary rotation (wi_1d987e7f296b6a2e, owner order #28426): the
// runtime ended, its wrapper survived and reported exited. The wrapper's exit
// event, the store clock, the broker's closure and the host's evidence are
// injected; no process, tmux server or handler is read, started, stopped or
// rotated.

// exit posts the old run's exit report as tt wrap does, at the fixture clock.
func (f *deadPrimaryFixture) exit(t *testing.T, code int) api.Event {
	t.Helper()
	e, err := f.s.PostEvent(context.Background(), f.task.ID, api.PostEventRequest{AgentID: f.old.ID, RunID: f.old.RunID, Kind: api.EventExited, Text: fmt.Sprintf("Process exited (%d)", code)}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if a, err := f.s.GetAgent(context.Background(), f.old.ID); err != nil || a.Status != api.AgentExited || a.RunID != f.old.RunID {
		t.Fatalf("the exit report left the handler %+v %v", a, err)
	}
	return e
}

// idleEvidence is what a host sends for an exited handler whose session
// remains: the runtime process gone, the receipt's exit, and a pane holding
// only the wrapper and its idle shell.
func (f *deadPrimaryFixture) idleEvidence(exit api.Event) *api.HandlerDeathEvidence {
	ev := f.evidence()
	at := exit.CreatedAt
	ev.SessionState, ev.PanePID, ev.PaneRootPID, ev.ExitedAt = api.HandlerDeathStateIdleShell, 0, 4200, &at
	return ev
}

func (f *deadPrimaryFixture) prepareExited(t *testing.T, key string, ev *api.HandlerDeathEvidence) (api.HandlerRotation, error) {
	t.Helper()
	return f.prepareDead(t, key, func(req *api.HandlerRotationRequest) { req.DeathEvidence = ev })
}

// brokerCloses is the broker's tick for an exited recipient: it closes the
// handler's open obligation on the given message recipient_gone.
func (f *deadPrimaryFixture) brokerCloses(t *testing.T, messageSeq int64) {
	t.Helper()
	open, err := f.s.BrokerOpenObligations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range open {
		if o.AgentID == f.old.ID && o.MessageSeq == messageSeq {
			if o.AgentStatus != api.AgentExited {
				t.Fatalf("the broker sees the handler as %q", o.AgentStatus)
			}
			if err := f.s.BrokerCloseRecipientGone(context.Background(), o, f.clock); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("no open obligation of the handler on message %d", messageSeq)
}

// a1, a7: an exited, silent, busy primary whose session still holds the idle
// shell is prepared and committed; leases, the frozen plan, the obligations
// the broker already closed and the one still open, and both notices are on
// the successor after the one commit.
func TestHandlerRotationExitedSilentPrimaryRotates(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	ctx := context.Background()
	s := f.s
	// A request the handler declined before it exited, and one the broker
	// closed for a closed recipient: neither is handed over.
	declined := f.send(t, f.worker, api.EnvelopeKindRequest, f.old.ID, "Fixture request declined before the exit", false)
	other := f.send(t, f.worker, api.EnvelopeKindRequest, f.old.ID, "Fixture request closed for another reason", false)
	early := f.send(t, f.worker, api.EnvelopeKindRequest, f.old.ID, "Fixture request closed before the exit", false)
	for _, c := range []struct {
		seq             int64
		outcome, reason string
	}{{declined.Seq, api.OutcomeDeclined, "fixture decline"}, {other.Seq, api.OutcomeRecipientGone, "recipient agent is closed"}, {early.Seq, api.OutcomeRecipientGone, exitedRecipientGoneReason}} {
		if _, err := s.db.Exec(`UPDATE obligations SET state='closed',outcome=?,reason=?,closed_at=?,changed_at=? WHERE message_seq=? AND agent_id=?`, c.outcome, c.reason, ts(f.clock), ts(f.clock), c.seq, f.old.ID); err != nil {
			t.Fatal(err)
		}
	}
	f.advance(time.Minute)
	exit := f.exit(t, 3)
	// The broker's next tick closes the first request; the second is still open.
	f.advance(30 * time.Second)
	f.brokerCloses(t, f.requests[0].Seq)
	if n := countRows(t, s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>'closed'`, f.old.ID); n != 1 {
		t.Fatalf("open obligations on the exited handler after the broker tick: %d", n)
	}
	f.advance(11 * time.Minute)
	r, err := f.prepareExited(t, "exit-prepare", f.idleEvidence(exit))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if r.Exit == nil || r.Exit.Code != 3 || !r.Exit.ReportedAt.Equal(exit.CreatedAt) || r.Exit.EventSeq != exit.Seq || r.DeathEvidence.SessionState != api.HandlerDeathStateIdleShell ||
		r.LastActivityAt == nil || r.LastActivityAt.Before(exit.CreatedAt) {
		t.Fatalf("prepared record: %+v exit %+v", r, r.Exit)
	}
	successor := f.succeed(t, r)
	f.advance(30 * time.Second)
	r, err = f.commitDead(r, "rotation-commit-"+r.ID, f.idleEvidence(exit))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	task, _ := s.GetTask(ctx, f.task.ID)
	old, _ := s.GetAgent(ctx, f.old.ID)
	if r.State != api.HandlerRotationCommitted || task.PrimaryHandlerID != successor.ID || task.HandlerRevision != 2 || old.Status != api.AgentClosed {
		t.Fatalf("state %s primary %s revision %d old %s", r.State, task.PrimaryHandlerID, task.HandlerRevision, old.Status)
	}
	// Leases and the frozen launch plan name the successor's exact run.
	running, _ := s.GetTeamQueueEntry(ctx, f.task.ID, f.running.ID)
	frozen, _ := s.GetTeamQueueEntry(ctx, f.task.ID, f.frozen.ID)
	for i, e := range []api.TeamQueueEntry{running, frozen} {
		if e.HandlerID != successor.ID || e.HandlerRunID != successor.RunID || e.HandlerLeaseGeneration != int64(i+1) {
			t.Fatalf("lease %d not moved: %+v", i, e)
		}
	}
	var plan struct {
		HandlerID    string `json:"handlerId"`
		HandlerRunID string `json:"handlerRunId"`
	}
	if err := json.Unmarshal(frozen.LaunchJSON, &plan); err != nil || plan.HandlerID != successor.ID || plan.HandlerRunID != successor.RunID || r.Receipt.LeasesMoved != 2 {
		t.Fatalf("launch plan %+v %v receipt %+v", plan, err, r.Receipt)
	}
	if running.HandlerArm == nil || running.HandlerArm.HandlerID != successor.ID {
		t.Fatalf("arm assignment: %+v", running.HandlerArm)
	}
	// Obligations: the one the broker closed after the exit and the one still
	// open are each open on the successor exactly once.
	if r.Handoff == nil || len(r.Handoff.Reissued) != 2 || r.Receipt.Reissued != 2 {
		t.Fatalf("handoff reissued: %+v", r.Handoff)
	}
	for i, m := range f.requests {
		var pairs []api.HandlerRotationReissue
		for _, p := range r.Handoff.Reissued {
			if p.OldMessageSeq == m.Seq {
				pairs = append(pairs, p)
			}
		}
		if len(pairs) != 1 || pairs[0].NewObligationID == "" || (i == 0) != (pairs[0].OldState == api.ObligationClosed) {
			t.Fatalf("request #%d reissued: %+v", m.Seq, pairs)
		}
		if n := countRows(t, s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>'closed' AND message_seq=? AND id=?`, successor.ID, pairs[0].NewMessageSeq, pairs[0].NewObligationID); n != 1 {
			t.Fatalf("request #%d is open on the successor %d times", m.Seq, n)
		}
		if n := countRows(t, s, `SELECT count(*) FROM obligations WHERE agent_id=? AND message_seq=? AND state='closed' AND outcome=?`, f.old.ID, m.Seq, api.OutcomeSuperseded); n != 1 {
			t.Fatalf("request #%d on the old handler is not superseded", m.Seq)
		}
	}
	if n := countRows(t, s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>'closed' AND source_kind='request'`, successor.ID); n != 2 {
		t.Fatalf("requests on the successor: %d", n)
	}
	// What was closed for another reason, or before the exit, stays as it was.
	for _, c := range []struct {
		seq             int64
		outcome, reason string
	}{{declined.Seq, api.OutcomeDeclined, "fixture decline"}, {other.Seq, api.OutcomeRecipientGone, "recipient agent is closed"}, {early.Seq, api.OutcomeRecipientGone, exitedRecipientGoneReason}} {
		if n := countRows(t, s, `SELECT count(*) FROM obligations WHERE message_seq=? AND agent_id=? AND state='closed' AND outcome=? AND reason=?`, c.seq, f.old.ID, c.outcome, c.reason); n != 1 {
			t.Fatalf("obligation on #%d did not stay closed %s (%s)", c.seq, c.outcome, c.reason)
		}
	}
	if n := countRows(t, s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>'closed'`, f.old.ID); n != 0 {
		t.Fatalf("open obligations left on the exited handler: %d", n)
	}
	// One notice each, same text, naming the exit status and time.
	toSuccessor, toHelper := noticesTo(t, s, f.task.ID, successor.ID), noticesTo(t, s, f.task.ID, f.helper.ID)
	if len(toSuccessor) != 1 || len(toHelper) != 1 || toSuccessor[0].Seq != r.Receipt.NoticeSeq || toHelper[0].Seq != r.Receipt.OwnerNoticeSeq {
		t.Fatalf("notices: successor %d helper %d receipt %+v", len(toSuccessor), len(toHelper), r.Receipt)
	}
	text := toSuccessor[0].Envelope.Body.Text
	if text != toHelper[0].Envelope.Body.Text {
		t.Fatalf("the two copies differ:\n%s\n%s", text, toHelper[0].Envelope.Body.Text)
	}
	for _, part := range []string{
		"Primary database handler " + f.old.Name + " (" + f.old.ID + " / " + f.old.RunID + ")",
		"Its runtime exited with status 3 at " + exit.CreatedAt.UTC().Format(time.RFC3339) + ", as reported by its wrapper.",
		"Evidence from host mini: runtime process 4242 (started " + deadPrimaryStarted + ") is absent and tmux session tt-handler-fixture holds only the wrapper (process 4200) and its idle shell",
		"2 open obligation(s) and 2 team lease(s) (" + f.running.ID + ", " + f.frozen.ID + ")",
		"the silence applied was 10 minute(s)",
		"tt handler rotation get " + r.ID,
	} {
		if !strings.Contains(text, part) {
			t.Fatalf("notice lacks %q:\n%s", part, text)
		}
	}
	if len(text) > 2000 {
		t.Fatalf("notice is %d bytes", len(text))
	}
	// A replayed commit, even with a newer observation, writes nothing.
	messages := countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID)
	obligations := countRows(t, s, `SELECT count(*) FROM obligations WHERE task_id=?`, f.task.ID)
	events := countRows(t, s, `SELECT count(*) FROM events WHERE task_id=?`, f.task.ID)
	f.advance(20 * time.Second)
	again, err := f.commitDead(r, "rotation-commit-"+r.ID, f.idleEvidence(exit))
	if err != nil || again.State != api.HandlerRotationCommitted || again.Receipt.NoticeSeq != r.Receipt.NoticeSeq || again.Exit == nil || again.Exit.EventSeq != exit.Seq {
		t.Fatalf("replayed commit: %+v %v", again, err)
	}
	if countRows(t, s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID) != messages || countRows(t, s, `SELECT count(*) FROM obligations WHERE task_id=?`, f.task.ID) != obligations ||
		countRows(t, s, `SELECT count(*) FROM events WHERE task_id=?`, f.task.ID) != events {
		t.Fatal("a replayed commit wrote again")
	}
	read, err := s.GetHandlerRotation(ctx, f.task.ID, r.ID)
	if err != nil || read.Exit == nil || read.Exit.Code != 3 || !read.Exit.ReportedAt.Equal(exit.CreatedAt) || read.CommitDeathEvidence == nil || read.CommitDeathEvidence.PaneRootPID != 4200 {
		t.Fatalf("read back: %+v %v", read, err)
	}
}

// a2: every missing condition refuses the prepare and leaves the primary,
// its leases and its obligations as they were.
func TestHandlerRotationExitedPrimaryRefusals(t *testing.T) {
	cases := []struct {
		name string
		// before runs ahead of the exit report, after once the silence passed.
		before, after func(*testing.T, *deadPrimaryFixture)
		wait          time.Duration
		evidence      func(*api.HandlerDeathEvidence, *deadPrimaryFixture)
		want          string
	}{
		{name: "silence not met", wait: 9 * time.Minute, want: api.HandlerRotationRefusedNotSilent},
		{name: "hub heartbeat within 90 s", after: func(t *testing.T, f *deadPrimaryFixture) {
			if _, err := f.s.db.Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(f.clock.Add(-30*time.Second)), f.old.ID); err != nil {
				t.Fatal(err)
			}
		}, want: api.HandlerRotationRefusedDeathUnconfirmed},
		{name: "stale evidence", evidence: func(ev *api.HandlerDeathEvidence, f *deadPrimaryFixture) {
			ev.ObservedAt = f.clock.Add(-3 * time.Minute)
		}, want: api.HandlerRotationRefusedDeathUnconfirmed},
		{name: "no exit in the host receipt", evidence: func(ev *api.HandlerDeathEvidence, _ *deadPrimaryFixture) { ev.ExitedAt = nil }, want: api.HandlerRotationRefusedDeathUnconfirmed},
		{name: "idle shell without the pane process", evidence: func(ev *api.HandlerDeathEvidence, _ *deadPrimaryFixture) { ev.PaneRootPID = 0 }, want: api.HandlerRotationRefusedDeathUnconfirmed},
		{name: "process not gone", evidence: func(ev *api.HandlerDeathEvidence, _ *deadPrimaryFixture) { ev.ProcessState = "alive" }, want: api.HandlerRotationRefusedDeathUnconfirmed},
		{name: "session alive", evidence: func(ev *api.HandlerDeathEvidence, _ *deadPrimaryFixture) { ev.SessionState = "alive" }, want: api.HandlerRotationRefusedDeathUnconfirmed},
		{name: "replacement off", after: func(t *testing.T, f *deadPrimaryFixture) {
			off := int64(0)
			if _, err := f.s.SetHandlerRotationPolicy(context.Background(), f.task.ID, api.HandlerRotationPolicyRequest{Enabled: true, DeadSilenceMinutes: &off}); err != nil {
				t.Fatal(err)
			}
		}, want: api.HandlerRotationRefusedDeathUnconfirmed},
		{name: "handler not busy", before: func(t *testing.T, f *deadPrimaryFixture) {
			if _, err := f.s.db.Exec(`UPDATE team_queue_entries SET state='finished',released_at=? WHERE task_id=?`, ts(f.clock), f.task.ID); err != nil {
				t.Fatal(err)
			}
			f.busy(t, "idle", "")
		}, want: api.HandlerRotationRefusedDeathUnconfirmed},
		{name: "no wrapper exit event", after: func(t *testing.T, f *deadPrimaryFixture) {
			if _, err := f.s.db.Exec(`DELETE FROM events WHERE agent_id=? AND kind=?`, f.old.ID, api.EventExited); err != nil {
				t.Fatal(err)
			}
		}, want: api.HandlerRotationRefusedDeathUnconfirmed},
		{name: "exit posted by hand", after: func(t *testing.T, f *deadPrimaryFixture) {
			if _, err := f.s.db.Exec(`UPDATE events SET text='' WHERE agent_id=? AND kind=?`, f.old.ID, api.EventExited); err != nil {
				t.Fatal(err)
			}
		}, want: api.HandlerRotationRefusedDeathUnconfirmed},
		{name: "exit text not the wrapper's", after: func(t *testing.T, f *deadPrimaryFixture) {
			if _, err := f.s.db.Exec(`UPDATE events SET text='Process exited (0) by hand' WHERE agent_id=? AND kind=?`, f.old.ID, api.EventExited); err != nil {
				t.Fatal(err)
			}
		}, want: api.HandlerRotationRefusedDeathUnconfirmed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newDeadPrimaryFixture(t)
			if c.before != nil {
				c.before(t, f)
			}
			exit := f.exit(t, 1)
			wait := c.wait
			if wait == 0 {
				wait = 11 * time.Minute
			}
			f.advance(wait)
			if c.after != nil {
				c.after(t, f)
			}
			ev := f.idleEvidence(exit)
			if c.evidence != nil {
				c.evidence(ev, f)
			}
			base := f.state(t)
			if _, err := f.prepareExited(t, "refused", ev); refusalCode(err) != c.want {
				t.Fatalf("prepare: %v, want %s", err, c.want)
			}
			if f.state(t) != base {
				t.Fatal("a refused prepare changed state")
			}
			if a, _ := f.s.GetAgent(context.Background(), f.old.ID); a.Status != api.AgentExited {
				t.Fatalf("the handler is %s", a.Status)
			}
		})
	}
}

// a3: an idle shell confirms nothing about a primary that is not exited, and
// an exited primary whose session is gone needs no idle shell.
func TestHandlerRotationIdleShellNeedsReportedExit(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	f.advance(11 * time.Minute)
	base := f.state(t)
	ev := f.idleEvidence(api.Event{CreatedAt: f.clock.Add(-11 * time.Minute)})
	if _, err := f.prepareExited(t, "not-exited", ev); refusalCode(err) != api.HandlerRotationRefusedDeathUnconfirmed || !strings.Contains(err.Error(), `session "idle_shell"`) {
		t.Fatalf("idle shell evidence for a handler that is not exited: %v", err)
	}
	if f.state(t) != base {
		t.Fatal("a refused prepare changed state")
	}
	// The parent's evidence still prepares it.
	if r, err := f.prepareDead(t, "gone", nil); err != nil || r.Exit != nil {
		t.Fatalf("parent path: %+v %v", r, err)
	}

	g := newDeadPrimaryFixture(t)
	exit := g.exit(t, 0)
	g.advance(11 * time.Minute)
	gone := g.evidence()
	gone.ExitedAt = &exit.CreatedAt
	if r, err := g.prepareExited(t, "exited-session-gone", gone); err != nil || r.Exit == nil || r.Exit.Code != 0 || r.DeathEvidence.SessionState != api.HandlerDeathStateGone {
		t.Fatalf("exited primary with its session gone: %+v %v", r, err)
	}
}

// a4: an exited primary that restarted, or whose run recorded anything after
// prepare, is never rotated, and neither is one with a later wrapper event.
func TestHandlerRotationExitedThenRestartedNeverRotates(t *testing.T) {
	ctx := context.Background()
	restart := func(t *testing.T, f *deadPrimaryFixture) api.Agent {
		t.Helper()
		a, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: f.old.ID, Role: api.AgentRoleDatabaseHandler, Name: f.old.Name, Host: f.old.Host, Session: f.old.Session, Runtime: f.old.Runtime, Cwd: f.old.Cwd, ExpectedRunID: f.old.RunID}, f.by)
		if err != nil || a.RunID == f.old.RunID || a.Status == api.AgentExited {
			t.Fatalf("restart: %+v %v", a, err)
		}
		return a
	}
	t.Run("restart before prepare", func(t *testing.T) {
		f := newDeadPrimaryFixture(t)
		exit := f.exit(t, 1)
		f.advance(11 * time.Minute)
		restarted := restart(t, f)
		if _, err := f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: restarted.ID, RunID: restarted.RunID, Kind: api.EventStarted, Text: "Process started"}, f.by); err != nil {
			t.Fatal(err)
		}
		base := f.state(t)
		if _, err := f.prepareExited(t, "restarted", f.idleEvidence(exit)); refusalCode(err) != api.HandlerRotationRefusedNotPrimary {
			t.Fatalf("prepare for the old run: %v", err)
		}
		// Even naming the new run, the old run's evidence confirms nothing.
		if _, err := f.prepareDead(t, "restarted-new-run", func(req *api.HandlerRotationRequest) {
			req.OldRunID, req.DeathEvidence = restarted.RunID, f.idleEvidence(exit)
		}); refusalCode(err) != api.HandlerRotationRefusedDeathUnconfirmed {
			t.Fatalf("prepare for the new run with the old evidence: %v", err)
		}
		if f.state(t) != base {
			t.Fatal("a refused prepare changed state")
		}
		list, err := f.s.HandlerRotationsDue(ctx, "mini", "")
		if err != nil || len(list.Entries) != 1 || list.Entries[0].Agent.RunID != restarted.RunID || list.Entries[0].DeadCandidate || list.Entries[0].SilenceMet {
			t.Fatalf("due list after a restart: %+v %v", list.Entries, err)
		}
	})
	t.Run("restart between prepare and commit", func(t *testing.T) {
		f := newDeadPrimaryFixture(t)
		exit := f.exit(t, 1)
		f.advance(11 * time.Minute)
		r, err := f.prepareExited(t, "prepare", f.idleEvidence(exit))
		if err != nil {
			t.Fatal(err)
		}
		f.succeed(t, r)
		restart(t, f)
		prepared := f.state(t)
		f.advance(time.Second)
		if _, err = f.commitDead(r, "commit", f.idleEvidence(exit)); refusalCode(err) != api.HandlerRotationRefusedStaleHandler {
			t.Fatalf("commit after a restart: %v", err)
		}
		if still, _ := f.s.GetHandlerRotation(ctx, f.task.ID, r.ID); f.state(t) != prepared || still.State != api.HandlerRotationPrepared {
			t.Fatal("a refused commit moved something")
		}
	})
	t.Run("activity between prepare and commit", func(t *testing.T) {
		f := newDeadPrimaryFixture(t)
		exit := f.exit(t, 1)
		f.advance(11 * time.Minute)
		r, err := f.prepareExited(t, "prepare", f.idleEvidence(exit))
		if err != nil {
			t.Fatal(err)
		}
		f.succeed(t, r)
		f.advance(time.Second)
		if _, err := f.s.db.Exec(`UPDATE messages SET created_at=? WHERE task_id=? AND from_agent=? AND from_run_id=?`, ts(f.clock), f.task.ID, f.old.ID, f.old.RunID); err != nil {
			t.Fatal(err)
		}
		prepared := f.state(t)
		f.advance(time.Second)
		if _, err = f.commitDead(r, "commit", f.idleEvidence(exit)); refusalCode(err) != api.HandlerRotationRefusedNotSilent {
			t.Fatalf("commit after activity: %v", err)
		}
		if still, _ := f.s.GetHandlerRotation(ctx, f.task.ID, r.ID); f.state(t) != prepared || still.State != api.HandlerRotationPrepared {
			t.Fatal("a refused commit moved something")
		}
	})
	// A rotation prepared without an exit report never replaces a handler
	// that exited afterwards: only a saved exit report lifts that refusal.
	t.Run("exit reported after a prepare without one", func(t *testing.T) {
		f := newDeadPrimaryFixture(t)
		f.advance(11 * time.Minute)
		r, err := f.prepareDead(t, "prepare", nil)
		if err != nil || r.Exit != nil {
			t.Fatalf("prepare: %+v %v", r, err)
		}
		f.succeed(t, r)
		exit := f.exit(t, 1)
		prepared := f.state(t)
		f.advance(time.Second)
		for _, ev := range []*api.HandlerDeathEvidence{f.evidence(), f.idleEvidence(exit)} {
			if _, err = f.commitDead(r, "commit", ev); refusalCode(err) != api.HandlerRotationRefusedStaleHandler {
				t.Fatalf("commit after a late exit report: %v", err)
			}
		}
		if still, _ := f.s.GetHandlerRotation(ctx, f.task.ID, r.ID); f.state(t) != prepared || still.State != api.HandlerRotationPrepared {
			t.Fatal("a refused commit moved something")
		}
	})
	// The hub records no event from an exited agent today, so a later row is
	// injected: the guard holds if that ever changes.
	for _, kind := range []string{api.EventStarted, api.EventHeartbeat} {
		inject := func(t *testing.T, f *deadPrimaryFixture) {
			t.Helper()
			if _, err := f.s.db.Exec(`INSERT INTO events (task_id,kind,agent_id,text,data,by_node,by_user,created_at) VALUES (?,?,?,'','','workspace','owner',?)`, f.task.ID, kind, f.old.ID, ts(f.clock)); err != nil {
				t.Fatal(err)
			}
		}
		t.Run("later "+kind+" row before prepare", func(t *testing.T) {
			f := newDeadPrimaryFixture(t)
			exit := f.exit(t, 1)
			f.advance(11 * time.Minute)
			inject(t, f)
			base := f.state(t)
			if _, err := f.prepareExited(t, "prepare", f.idleEvidence(exit)); refusalCode(err) != api.HandlerRotationRefusedDeathUnconfirmed {
				t.Fatalf("prepare after a later %s row: %v", kind, err)
			}
			if f.state(t) != base {
				t.Fatal("a refused prepare changed state")
			}
		})
		t.Run("later "+kind+" row before commit", func(t *testing.T) {
			f := newDeadPrimaryFixture(t)
			exit := f.exit(t, 1)
			f.advance(11 * time.Minute)
			r, err := f.prepareExited(t, "prepare", f.idleEvidence(exit))
			if err != nil {
				t.Fatal(err)
			}
			f.succeed(t, r)
			inject(t, f)
			prepared := f.state(t)
			f.advance(time.Second)
			if _, err = f.commitDead(r, "commit", f.idleEvidence(exit)); refusalCode(err) != api.HandlerRotationRefusedDeathUnconfirmed {
				t.Fatalf("commit after a later %s row: %v", kind, err)
			}
			if still, _ := f.s.GetHandlerRotation(ctx, f.task.ID, r.ID); f.state(t) != prepared || still.State != api.HandlerRotationPrepared {
				t.Fatal("a refused commit moved something")
			}
		})
	}
}

// a7: a commit of an exit rotation that fails part-way changes nothing: the
// first lease, and the obligation the broker closed, stay as they were.
func TestHandlerRotationExitedPrimaryCommitIsAtomic(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	bad := strings.Replace(f.plan, f.old.ID, f.worker.ID, 1)
	if _, err := f.s.db.Exec(`UPDATE team_queue_entries SET launch_json=? WHERE id=?`, bad, f.frozen.ID); err != nil {
		t.Fatal(err)
	}
	exit := f.exit(t, 2)
	f.advance(30 * time.Second)
	f.brokerCloses(t, f.requests[0].Seq)
	f.advance(11 * time.Minute)
	r, err := f.prepareExited(t, "prepare", f.idleEvidence(exit))
	if err != nil {
		t.Fatal(err)
	}
	successor := f.succeed(t, r)
	prepared := f.state(t)
	obligations := countRows(t, f.s, `SELECT count(*) FROM obligations WHERE task_id=?`, f.task.ID)
	f.advance(time.Second)
	if _, err = f.commitDead(r, "commit", f.idleEvidence(exit)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), f.frozen.ID) {
		t.Fatalf("commit with an unmovable lease: %v", err)
	}
	still, _ := f.s.GetHandlerRotation(context.Background(), f.task.ID, r.ID)
	old, _ := f.s.GetAgent(context.Background(), f.old.ID)
	if f.state(t) != prepared || still.State != api.HandlerRotationPrepared || old.Status != api.AgentExited ||
		countRows(t, f.s, `SELECT count(*) FROM team_queue_entries WHERE handler_id=?`, successor.ID) != 0 ||
		countRows(t, f.s, `SELECT count(*) FROM obligations WHERE task_id=?`, f.task.ID) != obligations ||
		countRows(t, f.s, `SELECT count(*) FROM obligations WHERE agent_id=? AND message_seq=? AND state='closed' AND outcome=? AND reason=?`, f.old.ID, f.requests[0].Seq, api.OutcomeRecipientGone, exitedRecipientGoneReason) != 1 ||
		countRows(t, f.s, `SELECT count(*) FROM obligations WHERE agent_id=?`, successor.ID) != 0 {
		t.Fatal("a failed commit changed something")
	}
}

// a8: a database from before the exit column gains it, and old rotations and
// an exit rotation both read back.
func TestHandlerRotationExitColumnMigrates(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	if _, err := f.s.db.Exec(`ALTER TABLE handler_rotations DROP COLUMN exit_json`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // the migration is idempotent
		if err := migrateHandlerRotation(f.s.db); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM pragma_table_info('handler_rotations') WHERE name='exit_json'`); n != 1 {
		t.Fatalf("exit_json columns after migrating: %d", n)
	}
	exit := f.exit(t, 137)
	f.advance(11 * time.Minute)
	r, err := f.prepareExited(t, "after-migration", f.idleEvidence(exit))
	if err != nil || r.Exit == nil || r.Exit.Code != 137 {
		t.Fatalf("exit rotation after migration: %+v %v", r, err)
	}
	read, err := f.s.GetHandlerRotation(context.Background(), f.task.ID, r.ID)
	if err != nil || read.Exit == nil || read.Exit.Code != 137 || read.Exit.EventSeq != exit.Seq || !read.Exit.ReportedAt.Equal(exit.CreatedAt) {
		t.Fatalf("read back: %+v %v", read.Exit, err)
	}
	list, err := f.s.ListHandlerRotations(context.Background(), f.task.ID)
	if err != nil || len(list) != 1 || list[0].Exit == nil {
		t.Fatalf("list: %+v %v", list, err)
	}
}

// a9: the due list marks an exited busy primary a dead candidate, with its
// silence measured from the exit report by the store clock, and does not
// list an exited idle one as one.
func TestHandlerRotationDueListExitedCandidate(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	ctx := context.Background()
	s := f.s
	due := func() []api.HandlerRotationDue {
		t.Helper()
		list, err := s.HandlerRotationsDue(ctx, "mini", "")
		if err != nil {
			t.Fatal(err)
		}
		return list.Entries
	}
	f.advance(2 * time.Minute)
	exit := f.exit(t, 1)
	// Just exited: the exit report was its last heartbeat, so not a candidate yet.
	d := due()
	if len(d) != 1 || d[0].Agent.ID != f.old.ID || d[0].Agent.Status != api.AgentExited || !d[0].Online || d[0].DeadCandidate || d[0].SilenceMet {
		t.Fatalf("just exited: %+v", d)
	}
	f.advance(5 * time.Minute)
	if d = due(); len(d) != 1 || d[0].Agent.Status != api.AgentExited || d[0].Online || !d[0].DeadCandidate || d[0].SilenceMet || d[0].Idle || d[0].LiveLeases != 2 ||
		d[0].LastActivityAt == nil || !d[0].LastActivityAt.Equal(exit.CreatedAt) {
		t.Fatalf("exited busy, 5 minutes: %+v", d)
	}
	f.advance(5 * time.Minute)
	if d = due(); len(d) != 1 || !d[0].DeadCandidate || !d[0].SilenceMet || !d[0].LastActivityAt.Equal(exit.CreatedAt) {
		t.Fatalf("exited busy, 10 minutes: %+v", d)
	}
	// With the limit policy off it is still listed for the dead case.
	if _, err := s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if d = due(); len(d) != 1 || !d[0].DeadCandidate || !d[0].SilenceMet || len(d[0].DueReasons) != 0 {
		t.Fatalf("limit policy off: %+v", d)
	}
	// With the dead primary replacement off it is not a candidate, and an
	// exited handler is not listed for any limit either.
	off, ten := int64(0), int64(10)
	if _, err := s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: 1, Enabled: true, MaxItems: 1, DeadSilenceMinutes: &off}); err != nil {
		t.Fatal(err)
	}
	if d = due(); len(d) != 0 {
		t.Fatalf("replacement off: %+v", d)
	}
	// Exited and idle: out of this rule's scope, so not listed as a candidate.
	if _, err := s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: 2, Enabled: true, MaxItems: 1, DeadSilenceMinutes: &ten}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE team_queue_entries SET state='finished',released_at=? WHERE task_id=?`, ts(f.clock), f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agent_activity SET state='idle',payload='{}' WHERE agent_id=?`, f.old.ID); err != nil {
		t.Fatal(err)
	}
	for _, e := range due() {
		if e.DeadCandidate || e.Agent.ID == f.old.ID {
			t.Fatalf("exited idle: %+v", e)
		}
	}
}

// Review b1: an idle exited primary beside a dead, busy acting handler does
// not hide that handler. The due list names the acting handler and the
// parent's rotation replaces it; the exited primary is untouched.
func TestHandlerRotationIdleExitedPrimaryLeavesActingHandlerToParentPath(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		name := "legacy oldest handler"
		if explicit {
			name = "explicit primary"
		}
		t.Run(name, func(t *testing.T) {
			f := newDeadPrimaryFixture(t)
			ctx := context.Background()
			s := f.s
			p, err := s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Role: api.AgentRoleDatabaseHandler, Name: "db-handler-first", Host: f.old.Host, Session: "tt-handler-first", Runtime: f.old.Runtime, Cwd: f.old.Cwd}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			if explicit {
				_, err = s.db.Exec(`UPDATE tasks SET primary_handler_id=? WHERE id=?`, p.ID, f.task.ID)
			} else {
				_, err = s.db.Exec(`UPDATE agents SET created_at=? WHERE id=?`, ts(f.clock.Add(-24*time.Hour)), p.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			idle := api.AgentActivity{State: "idle", ObservedAt: f.clock, LastEventAt: f.clock}
			if _, err = s.ReportActivity(ctx, f.task.ID, p.ID, api.ActivityReport{RequestID: api.NewID("act"), RunID: p.RunID, Activity: idle}); err != nil {
				t.Fatal(err)
			}
			if _, err = s.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: p.ID, RunID: p.RunID, Kind: api.EventExited, Text: "Process exited (1)"}, f.by); err != nil {
				t.Fatal(err)
			}
			task, _ := s.GetTask(ctx, f.task.ID)
			if exited, ok, err := exitedPrimaryHandler(ctx, s.db, task); err != nil || !ok || exited.ID != p.ID {
				t.Fatalf("the exited primary is %+v %t %v", exited, ok, err)
			}
			f.advance(11 * time.Minute)
			// The due list names the acting handler as the dead candidate.
			list, err := s.HandlerRotationsDue(ctx, "mini", "")
			if err != nil || len(list.Entries) != 1 || list.Entries[0].Agent.ID != f.old.ID || !list.Entries[0].DeadCandidate || !list.Entries[0].SilenceMet {
				t.Fatalf("due list: %+v %v", list.Entries, err)
			}
			// Prepare agrees: the acting handler rotates by the parent path.
			r, err := f.prepareDead(t, "acting-prepare", nil)
			if err != nil || r.OldAgentID != f.old.ID || r.Exit != nil {
				t.Fatalf("prepare for the acting handler: %+v %v", r, err)
			}
			successor := f.succeed(t, r)
			f.advance(30 * time.Second)
			if r, err = f.commitDead(r, "acting-commit", f.evidence()); err != nil || r.State != api.HandlerRotationCommitted || r.Receipt.LeasesMoved != 2 {
				t.Fatalf("commit for the acting handler: %+v %v", r, err)
			}
			task, _ = s.GetTask(ctx, f.task.ID)
			old, _ := s.GetAgent(ctx, f.old.ID)
			first, _ := s.GetAgent(ctx, p.ID)
			if task.PrimaryHandlerID != successor.ID || old.Status != api.AgentClosed || first.Status != api.AgentExited || first.RunID != p.RunID {
				t.Fatalf("primary %s acting %s exited primary %s", task.PrimaryHandlerID, old.Status, first.Status)
			}
		})
	}
	// Named by the request, an idle exited primary is still refused as not
	// busy, and a busy one is the primary for prepare as it is for the due list.
	f := newDeadPrimaryFixture(t)
	exit := f.exit(t, 1)
	f.advance(11 * time.Minute)
	other := api.NewID("agt")
	if _, err := f.prepareDead(t, "another-handler", func(req *api.HandlerRotationRequest) { req.OldAgentID = other }); refusalCode(err) != api.HandlerRotationRefusedNotPrimary {
		t.Fatalf("a request naming another handler beside a busy exited primary: %v", err)
	}
	if r, err := f.prepareExited(t, "busy-exited", f.idleEvidence(exit)); err != nil || r.OldAgentID != f.old.ID || r.Exit == nil {
		t.Fatalf("busy exited primary: %+v %v", r, err)
	}
}

// wi_c39902fd7463c26d (order #41851): one primary's unreadable activity
// snapshot does not fail the due list for every project on the host. That
// project is listed as busy and named; rotation prepare and commit still fail
// on the same snapshot.
func TestHandlerRotationDueListUnreadableActivityOfOnePrimary(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	s := f.s
	healthy, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Healthy rotation fixture"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.AddAgent(ctx, healthy.ID, api.AddAgentRequest{Name: "healthy-database", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: "healthy"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostEvent(ctx, healthy.ID, api.PostEventRequest{AgentID: h.ID, RunID: h.RunID, Kind: api.EventStarted}, f.by); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = s.ReportActivity(ctx, healthy.ID, h.ID, api.ActivityReport{RequestID: "healthy-idle", RunID: h.RunID, Activity: api.AgentActivity{State: "idle", ObservedAt: now, LastEventAt: now}}); err != nil {
		t.Fatal(err)
	}
	for _, task := range []string{healthy.ID, f.task.ID} {
		if _, err = s.SetHandlerRotationPolicy(ctx, task, api.HandlerRotationPolicyRequest{Enabled: true, OnTemplateChange: true}); err != nil {
			t.Fatal(err)
		}
	}
	// The stored state reads idle; only the payload does not parse.
	if _, err = s.db.Exec(`UPDATE agent_activity SET state='idle',payload='{not json' WHERE agent_id=? AND run_id=?`, f.old.ID, f.old.RunID); err != nil {
		t.Fatal(err)
	}
	list, err := s.HandlerRotationsDue(ctx, "mini", "bbbb")
	if err != nil || len(list.Entries) != 2 {
		t.Fatalf("due with one unreadable primary: %+v %v", list, err)
	}
	byTask := map[string]api.HandlerRotationDue{}
	for _, d := range list.Entries {
		byTask[d.TaskID] = d
	}
	if d := byTask[healthy.ID]; d.Agent.ID != h.ID || !d.Idle || d.ActivityState != "idle" || !reflect.DeepEqual(d.DueReasons, []string{"template"}) {
		t.Fatalf("healthy project: %+v", d)
	}
	d := byTask[f.task.ID]
	if d.Agent.ID != f.old.ID || d.Idle || d.DeadCandidate || d.SilenceMet || d.DueReasons == nil || len(d.DueReasons) != 0 ||
		!strings.HasPrefix(d.ActivityState, "unreadable: activity snapshot: invalid character 'n'") {
		t.Fatalf("unreadable project: %+v", d)
	}
	// Prepare is unchanged: it reads the snapshot, fails and saves nothing.
	if _, err = f.prepare(t, "unreadable-prepare", api.Agent{}, ""); err == nil || !strings.HasPrefix(err.Error(), "activity snapshot: ") {
		t.Fatalf("prepare on an unreadable snapshot: %v", err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM handler_rotations WHERE task_id=?`, f.task.ID); n != 0 {
		t.Fatalf("an unreadable primary was rotated: %d rotations", n)
	}
	// Commit is unchanged: prepared while readable, it fails once the
	// snapshot is not, and the rotation stays prepared.
	if _, err = s.db.Exec(`UPDATE agent_activity SET payload='{}' WHERE agent_id=?`, f.old.ID); err != nil {
		t.Fatal(err)
	}
	r, err := f.prepare(t, "unreadable-commit-prepare", api.Agent{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE agent_activity SET payload='{not json' WHERE agent_id=?`, f.old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.commit(r, "unreadable-commit"); err == nil || !strings.HasPrefix(err.Error(), "activity snapshot: ") {
		t.Fatalf("commit on an unreadable snapshot: %v", err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM handler_rotations WHERE task_id=? AND state='prepared'`, f.task.ID); n != 1 {
		t.Fatalf("commit on an unreadable snapshot changed the rotation: %d prepared", n)
	}
	// Any other error still fails the list.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.HandlerRotationsDue(canceled, "mini", "bbbb"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a database error was swallowed: %v", err)
	}
}

// The same holds for an offline busy primary and an exited one: unreadable,
// it is listed and named, never a dead primary candidate, even with the limit
// policy off.
func TestHandlerRotationDueListUnreadableDeadAndExitedPrimary(t *testing.T) {
	for _, exited := range []bool{false, true} {
		f := newDeadPrimaryFixture(t)
		ctx := context.Background()
		s := f.s
		status := api.AgentRunning
		if exited {
			// The wrapper reports the exit while the run is still heard from.
			f.advance(2 * time.Minute)
			f.exit(t, 1)
			status = api.AgentExited
		}
		if _, err := s.db.Exec(`UPDATE agent_activity SET payload='{"observedAt":5}' WHERE agent_id=?`, f.old.ID); err != nil {
			t.Fatal(err)
		}
		check := func(what string) {
			t.Helper()
			list, err := s.HandlerRotationsDue(ctx, "mini", "")
			if err != nil || len(list.Entries) != 1 {
				t.Fatalf("exited=%v %s: %+v %v", exited, what, list, err)
			}
			d := list.Entries[0]
			if d.Agent.ID != f.old.ID || d.Agent.Status != status || d.Online || d.Idle || d.DeadCandidate || d.SilenceMet || len(d.DueReasons) != 0 ||
				!strings.HasPrefix(d.ActivityState, "unreadable: activity snapshot: ") {
				t.Fatalf("exited=%v %s: %+v", exited, what, d)
			}
		}
		f.advance(11 * time.Minute)
		check("silent for the policy's minutes")
		if _, err := s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{Enabled: false}); err != nil {
			t.Fatal(err)
		}
		check("limit policy off")
	}
}

// Reissue dedupe (wi_d9e5d645bf6a466d, order #43382): an obligation the
// broker closed after the exit is not moved when its sender already re-sent
// it to the successor or the role after the close; the skip is on the
// handoff. An obligation still open at commit moves as before.

// exitedRotation exits the fixture handler, lets the broker close each listed
// request, then prepares and commits the rotation. beforeClose runs after the
// exit, afterClose before prepare and afterSucceed once the successor is
// registered; each may be nil.
func (f *deadPrimaryFixture) exitedRotation(t *testing.T, closes []int64, beforeClose, afterClose func(), afterSucceed func(successor api.Agent)) (api.HandlerRotation, api.Agent) {
	t.Helper()
	f.advance(time.Minute)
	exit := f.exit(t, 3)
	if beforeClose != nil {
		beforeClose()
	}
	f.advance(30 * time.Second)
	for _, seq := range closes {
		f.brokerCloses(t, seq)
	}
	f.advance(time.Second)
	if afterClose != nil {
		afterClose()
	}
	f.advance(11 * time.Minute)
	r, err := f.prepareExited(t, "resend-prepare", f.idleEvidence(exit))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	successor := f.succeed(t, r)
	if afterSucceed != nil {
		afterSucceed(successor)
	}
	f.advance(30 * time.Second)
	if r, err = f.commitDead(r, "rotation-commit-"+r.ID, f.idleEvidence(exit)); err != nil || r.Handoff == nil {
		t.Fatalf("commit: %+v %v", r, err)
	}
	return r, successor
}

// sendRole sends an unlinked request to role:database_handler, which only a
// running handler other than the exited one can hold.
func (f *deadPrimaryFixture) sendRole(t *testing.T, subject string) api.Message {
	t.Helper()
	if countRows(t, f.s, `SELECT count(*) FROM agents WHERE task_id=? AND name='db-handler-other'`, f.task.ID) == 0 {
		f.agent(t, "db-handler-other", api.AgentRoleDatabaseHandler)
	}
	m := f.send(t, f.worker, api.EnvelopeKindRequest, "role:database_handler", subject, false)
	if n := countRows(t, f.s, `SELECT count(*) FROM obligations WHERE message_seq=? AND via_role=?`, m.Seq, api.RoleDatabaseHandler); n != 1 {
		t.Fatalf("role request #%d has %d role obligation(s)", m.Seq, n)
	}
	return m
}

func reissuedFrom(r api.HandlerRotation, seq int64) []api.HandlerRotationReissue {
	var out []api.HandlerRotationReissue
	for _, p := range r.Handoff.Reissued {
		if p.OldMessageSeq == seq {
			out = append(out, p)
		}
	}
	return out
}

// a1, a3: re-sent by name or by role after the close is skipped and recorded;
// not re-sent, re-sent before the close, or re-sent as something else (another
// subject, item or sender) is reissued as before.
func TestHandlerRotationExitedPrimarySkipsResentRequest(t *testing.T) {
	const first = "First fixture request held by the handler"
	const linkedSubject = "Linked fixture request held by the handler"
	const beforeClose, afterClose, afterSucceed = "before the close", "after the close", "after the successor registers"
	for _, c := range []struct {
		name string
		// linked closes an item-linked request sent to the handler before the
		// exit, in place of the first fixture request.
		linked bool
		at     string
		send   func(t *testing.T, f *deadPrimaryFixture, successor api.Agent) api.Message
		reason string
	}{
		{name: "re-sent to the successor", at: afterSucceed, reason: api.HandlerRotationSkipSubjectItem,
			send: func(t *testing.T, f *deadPrimaryFixture, s api.Agent) api.Message {
				return f.send(t, f.worker, api.EnvelopeKindRequest, s.ID, first, false)
			}},
		{name: "re-sent to the role", at: afterClose, reason: api.HandlerRotationSkipSubjectItem,
			send: func(t *testing.T, f *deadPrimaryFixture, _ api.Agent) api.Message { return f.sendRole(t, first) }},
		{name: "linked and re-sent with the same item", linked: true, at: afterSucceed, reason: api.HandlerRotationSkipSubjectItem,
			send: func(t *testing.T, f *deadPrimaryFixture, s api.Agent) api.Message {
				return f.send(t, f.worker, api.EnvelopeKindRequest, s.ID, linkedSubject, true)
			}},
		{name: "not re-sent"},
		{name: "re-sent before the close", at: beforeClose,
			send: func(t *testing.T, f *deadPrimaryFixture, _ api.Agent) api.Message { return f.sendRole(t, first) }},
		{name: "re-sent with another subject", at: afterSucceed,
			send: func(t *testing.T, f *deadPrimaryFixture, s api.Agent) api.Message {
				return f.send(t, f.worker, api.EnvelopeKindRequest, s.ID, "Another fixture request", false)
			}},
		{name: "re-sent linked to an item", at: afterSucceed,
			send: func(t *testing.T, f *deadPrimaryFixture, s api.Agent) api.Message {
				return f.send(t, f.worker, api.EnvelopeKindRequest, s.ID, first, true)
			}},
		{name: "sent by someone else", at: afterSucceed,
			send: func(t *testing.T, f *deadPrimaryFixture, s api.Agent) api.Message {
				return f.send(t, f.agent(t, "reviewer", ""), api.EnvelopeKindRequest, s.ID, first, false)
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newDeadPrimaryFixture(t)
			ctx := context.Background()
			s := f.s
			target, subject := f.requests[0], first
			if c.linked {
				target, subject = f.send(t, f.worker, api.EnvelopeKindRequest, f.old.ID, linkedSubject, true), linkedSubject
			}
			var resent api.Message
			at := func(phase string) func(api.Agent) {
				return func(successor api.Agent) {
					if c.at == phase {
						resent = c.send(t, f, successor)
					}
				}
			}
			r, successor := f.exitedRotation(t, []int64{target.Seq}, func() { at(beforeClose)(api.Agent{}) }, func() { at(afterClose)(api.Agent{}) }, at(afterSucceed))
			// The request still open at commit always moves.
			if p := reissuedFrom(r, f.requests[1].Seq); len(p) != 1 || p[0].OldState == api.ObligationClosed {
				t.Fatalf("open request #%d reissued: %+v", f.requests[1].Seq, p)
			}
			moved := reissuedFrom(r, target.Seq)
			if c.reason == "" {
				if len(moved) != 1 || moved[0].OldState != api.ObligationClosed || len(r.Handoff.Skipped) != 0 {
					t.Fatalf("request #%d: reissued %+v skipped %+v", target.Seq, moved, r.Handoff.Skipped)
				}
				if data, _ := json.Marshal(r.Handoff); strings.Contains(string(data), `"skipped"`) {
					t.Fatalf("a handoff with no skip names the list: %s", data)
				}
				return
			}
			if resent.Seq == 0 || len(moved) != 0 || len(r.Handoff.Skipped) != 1 {
				t.Fatalf("request #%d: reissued %+v skipped %+v", target.Seq, moved, r.Handoff.Skipped)
			}
			var oldObligation string
			if err := s.db.QueryRow(`SELECT id FROM obligations WHERE message_seq=? AND agent_id=? AND state='closed' AND outcome=? AND reason=?`,
				target.Seq, f.old.ID, api.OutcomeRecipientGone, exitedRecipientGoneReason).Scan(&oldObligation); err != nil {
				t.Fatalf("the skipped obligation did not stay closed recipient_gone: %v", err)
			}
			want := []api.HandlerRotationSkip{{OldObligationID: oldObligation, OldMessageSeq: target.Seq, NewMessageSeq: resent.Seq, Reason: c.reason, Subject: subject}}
			if !reflect.DeepEqual(r.Handoff.Skipped, want) {
				t.Fatalf("skipped %+v, want %+v", r.Handoff.Skipped, want)
			}
			// The successor, or the role holder, has the request once: the re-send.
			if n := countRows(t, s, `SELECT count(*) FROM obligations WHERE state<>'closed' AND source_kind='request' AND subject LIKE ?`, "%"+subject); n != 1 {
				t.Fatalf("the request is open %d times", n)
			}
			if r.Receipt.Reissued != len(r.Handoff.Reissued) {
				t.Fatalf("receipt %d moved, handoff %d", r.Receipt.Reissued, len(r.Handoff.Reissued))
			}
			// The skip is saved with the handoff and counted on the event.
			saved, err := s.GetHandlerRotation(ctx, f.task.ID, r.ID)
			if err != nil || saved.Handoff == nil || !reflect.DeepEqual(saved.Handoff.Skipped, want) {
				t.Fatalf("saved handoff: %+v %v", saved.Handoff, err)
			}
			if n := countRows(t, s, `SELECT count(*) FROM events WHERE task_id=? AND agent_id=? AND text='Handler rotation committed' AND data LIKE '%"skipped":1%'`, f.task.ID, successor.ID); n != 1 {
				t.Fatalf("committed events counting the skip: %d", n)
			}
		})
	}
}

// a2: a request still open at commit is reissued even though its sender also
// re-sent it to the successor; only a closed one is checked.
func TestHandlerRotationExitedPrimaryMovesOpenResentRequest(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	held := f.requests[1]
	const subject = "Second fixture request held by role"
	var resent api.Message
	r, successor := f.exitedRotation(t, []int64{f.requests[0].Seq}, nil, nil, func(a api.Agent) {
		resent = f.send(t, f.worker, api.EnvelopeKindRequest, a.ID, subject, false)
	})
	p := reissuedFrom(r, held.Seq)
	if len(p) != 1 || p[0].OldState == api.ObligationClosed || p[0].ViaRole != api.RoleDatabaseHandler || len(r.Handoff.Skipped) != 0 || len(reissuedFrom(r, f.requests[0].Seq)) != 1 {
		t.Fatalf("reissued %+v skipped %+v", r.Handoff.Reissued, r.Handoff.Skipped)
	}
	for _, seq := range []int64{resent.Seq, p[0].NewMessageSeq} {
		if n := countRows(t, f.s, `SELECT count(*) FROM obligations WHERE agent_id=? AND message_seq=? AND state<>'closed'`, successor.ID, seq); n != 1 {
			t.Fatalf("request #%d open on the successor %d times", seq, n)
		}
	}
}

// a1: the match itself. A request id is the sender's own retry key; otherwise
// the subject and the linked items must both match. The kind always must.
func TestHandlerRotationResentReason(t *testing.T) {
	msg := func(kind, subject, request string, items ...string) api.Message {
		m := api.Message{Envelope: &api.Envelope{Kind: kind, Subject: subject}}
		if request != "" {
			m.PostReceipt = &api.MessagePostReceipt{RequestID: request}
		}
		for _, id := range items {
			m.WorkItems = append(m.WorkItems, api.MessageWorkItem{ItemTaskID: "tsk_fixture", ItemID: id, ItemRevision: int64(len(m.WorkItems) + 1)})
		}
		return m
	}
	original := msg(api.EnvelopeKindRequest, "Fixture request", "req-1", "wi_a", "wi_b")
	for _, c := range []struct {
		name     string
		original api.Message
		resend   api.Message
		want     string
	}{
		{"same request id, other subject", original, msg(api.EnvelopeKindRequest, "Reworded fixture request", "req-1"), api.HandlerRotationSkipRequestID},
		{"same subject and items, other request id and revisions", original, msg(api.EnvelopeKindRequest, " Fixture request ", "req-2", "wi_b", "wi_a"), api.HandlerRotationSkipSubjectItem},
		{"both unlinked, same subject", msg(api.EnvelopeKindRequest, "Fixture request", ""), msg(api.EnvelopeKindRequest, "Fixture request", ""), api.HandlerRotationSkipSubjectItem},
		{"same subject, one item fewer", original, msg(api.EnvelopeKindRequest, "Fixture request", "req-2", "wi_a"), ""},
		{"same subject, another item", original, msg(api.EnvelopeKindRequest, "Fixture request", "req-2", "wi_a", "wi_c"), ""},
		{"same subject and items, another kind", original, msg(api.EnvelopeKindQuestion, "Fixture request", "req-2", "wi_a", "wi_b"), ""},
		{"same request id, another kind", original, msg(api.EnvelopeKindNotice, "Fixture request", "req-1", "wi_a", "wi_b"), ""},
		{"another subject", original, msg(api.EnvelopeKindRequest, "Another fixture request", "req-2", "wi_a", "wi_b"), ""},
		{"no request ids, another subject", msg(api.EnvelopeKindRequest, "Fixture request", ""), msg(api.EnvelopeKindRequest, "Another fixture request", ""), ""},
		{"empty subjects", msg(api.EnvelopeKindRequest, "", ""), msg(api.EnvelopeKindRequest, "", ""), ""},
		{"untyped original", api.Message{Text: "Fixture request"}, msg(api.EnvelopeKindRequest, "Fixture request", ""), ""},
	} {
		if got := resentReason(c.original, c.resend); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// Stored times drop trailing zeros, so a handler created on a whole
// millisecond sorts after a later one in that millisecond as text. With no
// recorded primary both fallbacks must still pick the older handler
// (wi_fcdb3c8517ade6c3, order #44030, FINDING #32673).
func TestPrimaryHandlerFallbackOrdersByParsedTime(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	aux := f.agent(t, "aux", api.AgentRoleDatabaseHandler)
	created := func(a api.Agent, at string) {
		t.Helper()
		if _, err := f.s.db.Exec(`UPDATE agents SET created_at=? WHERE id=?`, at, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	pick := func(wantPrimary, wantExited string, wantOK bool) {
		t.Helper()
		task, err := f.s.GetTask(ctx, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if p, err := primaryHandler(ctx, f.s.db, task); err != nil || p.ID != wantPrimary {
			t.Errorf("primaryHandler = %s %v, want %s", p.ID, err, wantPrimary)
		}
		if e, ok, err := exitedPrimaryHandler(ctx, f.s.db, task); err != nil || e.ID != wantExited || ok != wantOK {
			t.Errorf("exitedPrimaryHandler = %s %t %v, want %s %t", e.ID, ok, err, wantExited, wantOK)
		}
	}
	created(f.old, "2026-10-13T01:44:15.994Z")
	created(aux, "2026-10-13T01:44:15.994296Z")
	pick(f.old.ID, f.old.ID, false)

	// Equal times break by id.
	created(aux, "2026-10-13T01:44:15.994Z")
	first := f.old
	if aux.ID < f.old.ID {
		first = aux
	}
	pick(first.ID, first.ID, false)

	// An exited older handler is the exited fallback but not the primary.
	created(aux, "2026-10-13T01:44:15.994296Z")
	if _, err := f.s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentExited, f.old.ID); err != nil {
		t.Fatal(err)
	}
	pick(aux.ID, f.old.ID, true)

	// A recorded primary still wins over the older handler.
	if _, err := f.s.db.Exec(`UPDATE agents SET status='running' WHERE id=?`, f.old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE tasks SET primary_handler_id=? WHERE id=?`, aux.ID, f.task.ID); err != nil {
		t.Fatal(err)
	}
	pick(aux.ID, aux.ID, false)
}

// Successor readiness at commit (bug wi_ce7310979bc45479, order #46760,
// criterion a4): nothing moves until the successor's exact run has completed
// a turn with a model reply since the prepare.
func TestHandlerRotationSuccessorReadiness(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		f.send(t, f.worker, api.EnvelopeKindRequest, f.old.ID, fmt.Sprintf("Record synthetic readiness item %d", i), false)
	}
	r, err := f.prepare(t, "ready-prepare", api.Agent{}, "")
	if err != nil {
		t.Fatal(err)
	}
	successor, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: r.SuccessorAgentID, Role: api.AgentRoleDatabaseHandler, Name: r.SuccessorName, Host: "mini", Session: "tt-handler-ready", Runtime: f.old.Runtime, Cwd: f.old.Cwd}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: successor.ID, RunID: successor.RunID, Kind: api.EventStarted}, f.by); err != nil {
		t.Fatal(err)
	}
	before := openObligations(t, f.s, f.task.ID)
	n := 0
	report := func(a api.Agent, activity api.AgentActivity) {
		t.Helper()
		n++
		if activity.ObservedAt.IsZero() {
			activity.ObservedAt = time.Now().UTC()
		}
		if _, err := f.s.ReportActivity(ctx, f.task.ID, a.ID, api.ActivityReport{RequestID: fmt.Sprintf("ready-report-%d", n), RunID: a.RunID, Activity: activity}); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(what string) {
		t.Helper()
		if _, err := f.commitAsIs(r, "ready-commit-"+strings.ReplaceAll(what, " ", "-")); refusalCode(err) != api.HandlerRotationRefusedSuccessorNotReady {
			t.Fatalf("%s: commit %v, want %s", what, err, api.HandlerRotationRefusedSuccessorNotReady)
		}
		f.assertNothingMoved(t, before)
		if c := countRows(t, f.s, `SELECT count(*) FROM obligations WHERE agent_id=?`, successor.ID); c != 0 {
			t.Fatalf("%s: successor holds %d obligations", what, c)
		}
	}
	refused("no activity")
	// The predecessor's own completed turns are not the successor's.
	report(f.old, api.AgentActivity{State: "working", Tokens: api.TokenTotals{Output: 9}})
	report(f.old, api.AgentActivity{State: "idle", Tokens: api.TokenTotals{Output: 10}})
	refused("predecessor activity")
	// An earlier run of the successor, and this run before the prepare.
	ready, _ := json.Marshal(api.AgentActivity{State: "idle", ObservedAt: time.Now().UTC(), Tokens: api.TokenTotals{Output: 5}})
	if _, err := f.s.db.Exec(`INSERT INTO agent_activity_receipts(task_id,request_id,agent_id,run_id,payload,request_payload) VALUES(?,?,?,?,?,'{}')`, f.task.ID, "ready-earlier-run", successor.ID, "run_fixtureearlier01", string(ready)); err != nil {
		t.Fatal(err)
	}
	report(successor, api.AgentActivity{State: "working", ObservedAt: r.CreatedAt.Add(-time.Second), Tokens: api.TokenTotals{Output: 4}})
	report(successor, api.AgentActivity{State: "idle", ObservedAt: r.CreatedAt.Add(-time.Second), Tokens: api.TokenTotals{Output: 5}})
	refused("earlier run or before the prepare")
	// Turns of this run that are not a completed model reply.
	report(successor, api.AgentActivity{State: "provider_blocked", Tokens: api.TokenTotals{Output: 5}, Provider: &api.ProviderBlock{Provider: "openai", Runtime: "codex", Model: "gpt-6-astra", Class: api.ProviderBlockRateLimited, Code: "response_too_many_failed_attempts", Status: 429, Since: time.Now().UTC()}})
	refused("provider blocked")
	report(successor, api.AgentActivity{State: "finished_silent", Reason: api.ProviderErrorTurnReasonPrefix + " (rate_limited)", Tokens: api.TokenTotals{Output: 5}})
	refused("provider error turn end")
	report(successor, api.AgentActivity{State: "idle", Tokens: api.TokenTotals{}})
	refused("no output")
	report(successor, api.AgentActivity{State: "working", Tokens: api.TokenTotals{Output: 6}})
	refused("turn still running")
	// One completed turn of the exact run with a model reply.
	report(successor, api.AgentActivity{State: "idle", Tokens: api.TokenTotals{Output: 6}})
	committed, err := f.commitAsIs(r, "ready-commit")
	if err != nil || committed.State != api.HandlerRotationCommitted || committed.Receipt == nil || committed.Receipt.Reissued != 3 {
		t.Fatalf("ready commit %+v %v", committed, err)
	}
	// Each moved request is one open obligation of the successor, for the
	// re-issue the handoff names.
	moved := func() int {
		c := 0
		for _, p := range committed.Handoff.Reissued {
			c += countRows(t, f.s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>? AND message_seq=?`, successor.ID, api.ObligationClosed, p.NewMessageSeq)
		}
		return c + countRows(t, f.s, `SELECT count(*) FROM obligations WHERE agent_id=? AND needs<>? AND message_seq NOT IN (SELECT value FROM json_each(?))`, successor.ID, api.ObligationNeedsDelivery, reissuedSeqs(committed))
	}
	if m := moved(); m != 3 || countRows(t, f.s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>?`, f.old.ID, api.ObligationClosed) != 0 {
		t.Fatalf("moved %d", m)
	}
	// A replayed commit, by its key or another, returns the saved record and
	// moves nothing again.
	for _, key := range []string{"ready-commit", "ready-commit-replay"} {
		again, err := f.commitAsIs(r, key)
		if err != nil || again.ID != committed.ID || again.Receipt == nil || again.Receipt.NoticeSeq != committed.Receipt.NoticeSeq {
			t.Fatalf("replay %s: %+v %v", key, again, err)
		}
	}
	if m := moved(); m != 3 {
		t.Fatalf("replay moved %d", m)
	}
}

func reissuedSeqs(r api.HandlerRotation) string {
	var seqs []int64
	if r.Handoff != nil {
		for _, p := range r.Handoff.Reissued {
			seqs = append(seqs, p.NewMessageSeq)
		}
	}
	out, _ := json.Marshal(seqs)
	return string(out)
}

// A provider-blocked primary handler (bug wi_ce7310979bc45479, order #46760,
// criterion a6). blockedFixture is a rotation fixture on a controllable clock
// with the limit policy off and the dead primary replacement at silence, an
// open request held by the primary, and the primary's run blocked by class
// since the clock's start.
type blockedFixture struct {
	*rotationFixture
	clock, opened time.Time
	request       api.Message
}

func newBlockedFixture(t *testing.T, class string, silence int64) *blockedFixture {
	t.Helper()
	// The clock starts so that the 20-minute bound falls just before real
	// time: a successor's online status is read by the real clock.
	f := &blockedFixture{rotationFixture: newRotationFixture(t), clock: time.Now().UTC().Add(-api.HandlerProviderBlockedAfter - 5*time.Second)}
	f.s.now = func() time.Time { return f.clock }
	ctx := context.Background()
	policy, err := f.s.HandlerRotationPolicy(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: policy.Revision, Enabled: false, DeadSilenceMinutes: &silence}); err != nil {
		t.Fatal(err)
	}
	f.request = f.send(t, f.worker, api.EnvelopeKindRequest, f.old.ID, "Record the blocked handler fixture item", false)
	f.opened = f.clock
	p := &api.ProviderBlock{Provider: "openai", Runtime: "codex", Model: "gpt-6-astra", Class: class, Code: "response_too_many_failed_attempts", Status: 429, Since: f.clock}
	if class == api.ProviderBlockAuth {
		p.Code, p.Status = "other", 401
	}
	if _, err = f.s.ReportActivity(ctx, f.task.ID, f.old.ID, api.ActivityReport{RequestID: api.NewID("act"), RunID: f.old.RunID, Activity: api.AgentActivity{State: "provider_blocked", ObservedAt: f.clock, Provider: p}}); err != nil {
		t.Fatal(err)
	}
	return f
}

// at moves the clock to opened+d and refreshes the primary's heartbeat.
func (f *blockedFixture) at(t *testing.T, d time.Duration) {
	t.Helper()
	f.clock = f.opened.Add(d)
	if _, err := f.s.db.Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(f.clock), f.old.ID); err != nil {
		t.Fatal(err)
	}
}

// due reports whether the host's due list names the primary provider_blocked.
func (f *blockedFixture) due(t *testing.T) bool {
	t.Helper()
	list, err := f.s.HandlerRotationsDue(context.Background(), "mini", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range list.Entries {
		if d.TaskID == f.task.ID {
			return slices.Contains(d.DueReasons, api.HandlerRotationReasonProviderBlocked)
		}
	}
	return false
}

func (f *blockedFixture) prepareBlocked(t *testing.T, key string, change func(*api.HandlerRotationRequest)) (api.HandlerRotation, error) {
	t.Helper()
	task, err := f.s.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: key, ExpectedHandlerRevision: task.HandlerRevision, OldAgentID: f.old.ID, OldRunID: f.old.RunID,
		SuccessorAgentID: api.NewID("agt"), SuccessorName: api.HandlerSuccessorName(f.old.Name, task.HandlerRevision), Reason: api.HandlerRotationReasonProviderBlocked, Trigger: api.HandlerRotationTriggerRunner}
	if change != nil {
		change(&req)
	}
	return f.s.HandlerRotationAction(context.Background(), f.task.ID, req, f.by)
}

func TestHandlerRotationDueProviderBlocked(t *testing.T) {
	for _, class := range []string{api.ProviderBlockRateLimited, api.ProviderBlockServerError} {
		f := newBlockedFixture(t, class, api.DefaultHandlerDeadSilenceMinutes)
		f.at(t, api.HandlerProviderBlockedAfter-time.Second)
		if f.due(t) {
			t.Fatalf("%s listed at 19m59s", class)
		}
		// Due at 20 minutes, with the limit policy off.
		f.at(t, api.HandlerProviderBlockedAfter)
		if !f.due(t) {
			t.Fatalf("%s not listed at 20m", class)
		}
		// Offline is not the blocked case.
		f.clock = f.clock.Add(time.Hour)
		if f.due(t) {
			t.Fatalf("%s listed while offline", class)
		}
	}
	// Not with the replacement off, an auth block, or no open obligation.
	f := newBlockedFixture(t, api.ProviderBlockRateLimited, 0)
	f.at(t, time.Hour)
	if f.due(t) {
		t.Fatal("listed with dead silence 0")
	}
	f = newBlockedFixture(t, api.ProviderBlockAuth, api.DefaultHandlerDeadSilenceMinutes)
	f.at(t, time.Hour)
	if f.due(t) {
		t.Fatal("listed for an auth block")
	}
	f = newBlockedFixture(t, api.ProviderBlockRateLimited, api.DefaultHandlerDeadSilenceMinutes)
	if _, err := f.s.db.Exec(`UPDATE obligations SET state=? WHERE agent_id=?`, api.ObligationClosed, f.old.ID); err != nil {
		t.Fatal(err)
	}
	f.at(t, time.Hour)
	if f.due(t) {
		t.Fatal("listed with no open obligation")
	}
	// Recovery closes the episode: no longer due.
	f = newBlockedFixture(t, api.ProviderBlockRateLimited, api.DefaultHandlerDeadSilenceMinutes)
	f.at(t, time.Hour)
	f.idle(t, f.old)
	if f.due(t) {
		t.Fatal("listed after recovery")
	}
}

func TestHandlerRotationProviderBlockedPrepare(t *testing.T) {
	f := newBlockedFixture(t, api.ProviderBlockRateLimited, api.DefaultHandlerDeadSilenceMinutes)
	f.at(t, api.HandlerProviderBlockedAfter)
	for name, change := range map[string]func(*api.HandlerRotationRequest){
		"owner trigger":  func(r *api.HandlerRotationRequest) { r.Trigger = api.HandlerRotationTriggerOwner },
		"authorization":  func(r *api.HandlerRotationRequest) { r.AuthorizedBy, r.AuthorizationReason = "owner", "fixture" },
		"death evidence": func(r *api.HandlerRotationRequest) { r.DeathEvidence = &api.HandlerDeathEvidence{Host: "mini"} },
	} {
		if _, err := f.prepareBlocked(t, "blocked-"+strings.ReplaceAll(name, " ", "-"), change); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	f.at(t, api.HandlerProviderBlockedAfter-time.Second)
	if _, err := f.prepareBlocked(t, "blocked-early", nil); refusalCode(err) != api.HandlerRotationRefusedNotBlocked {
		t.Fatalf("early prepare: %v", err)
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM handler_rotations WHERE task_id=?`, f.task.ID); n != 0 {
		t.Fatalf("refused prepares left %d rotations", n)
	}
	f.at(t, api.HandlerProviderBlockedAfter)
	r, err := f.prepareBlocked(t, "blocked-due", nil)
	if err != nil || r.State != api.HandlerRotationPrepared || r.Reason != api.HandlerRotationReasonProviderBlocked || r.Trigger != api.HandlerRotationTriggerRunner {
		t.Fatalf("due prepare %+v %v", r, err)
	}
	// Prepared, it stays listed for the runner to resume even with the limit
	// policy off, also after the block clears.
	f.idle(t, f.old)
	list, err := f.s.HandlerRotationsDue(context.Background(), "mini", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) != 1 || list.Entries[0].OpenRotation == nil || list.Entries[0].OpenRotation.ID != r.ID {
		t.Fatalf("prepared rotation not listed: %+v", list.Entries)
	}
	// The other conditions refuse it too.
	for _, setup := range []func(g *blockedFixture){
		func(g *blockedFixture) { g.clock = g.clock.Add(time.Hour) }, // offline
		func(g *blockedFixture) {
			if _, err := g.s.db.Exec(`UPDATE obligations SET state=? WHERE agent_id=?`, api.ObligationClosed, g.old.ID); err != nil {
				t.Fatal(err)
			}
		},
	} {
		g := newBlockedFixture(t, api.ProviderBlockRateLimited, api.DefaultHandlerDeadSilenceMinutes)
		g.at(t, time.Hour)
		setup(g)
		if _, err := g.prepareBlocked(t, "blocked-refused", nil); refusalCode(err) != api.HandlerRotationRefusedNotBlocked {
			t.Fatalf("prepare: %v", err)
		}
	}
	g := newBlockedFixture(t, api.ProviderBlockAuth, api.DefaultHandlerDeadSilenceMinutes)
	g.at(t, time.Hour)
	if _, err := g.prepareBlocked(t, "blocked-auth", nil); refusalCode(err) != api.HandlerRotationRefusedNotBlocked {
		t.Fatalf("auth prepare: %v", err)
	}
}

func TestHandlerRotationProviderBlockedOwnerNoticeOnce(t *testing.T) {
	ownerNotices := func(f *blockedFixture) []api.Message {
		t.Helper()
		messages, err := f.s.ListMessages(context.Background(), f.task.ID, 0, "", 500)
		if err != nil {
			t.Fatal(err)
		}
		var out []api.Message
		for _, m := range messages {
			if m.Envelope != nil && m.Envelope.Subject == providerBlockedReplacedSubject {
				out = append(out, m)
			}
		}
		return out
	}
	for _, withHelper := range []bool{true, false} {
		f := newBlockedFixture(t, api.ProviderBlockRateLimited, api.DefaultHandlerDeadSilenceMinutes)
		var helper api.Agent
		if withHelper {
			helper = f.agent(t, "owner-helper", "")
			if _, err := f.s.db.Exec(`UPDATE agents SET role=? WHERE id=?`, api.AgentRoleOwnerHelper, helper.ID); err != nil {
				t.Fatal(err)
			}
		}
		f.at(t, api.HandlerProviderBlockedAfter)
		r, err := f.prepareBlocked(t, "blocked-prepare", nil)
		if err != nil {
			t.Fatal(err)
		}
		f.successor(t, r, api.HandlerArm{Runtime: f.old.Runtime})
		committed, err := f.commit(r, "rotation-commit-"+r.ID)
		if err != nil || committed.State != api.HandlerRotationCommitted || committed.Receipt.Reissued != 1 {
			t.Fatalf("commit %+v %v", committed, err)
		}
		notices := ownerNotices(f)
		if len(notices) != 1 || notices[0].Seq != committed.Receipt.OwnerNoticeSeq || notices[0].To != helper.ID {
			t.Fatalf("helper=%v notices %+v receipt %+v", withHelper, notices, committed.Receipt)
		}
		text := notices[0].Envelope.Body.Text
		for _, want := range []string{f.old.Name, r.SuccessorName, "rate_limited", f.opened.Format(time.RFC3339), "1 open obligation(s) moved", "wi_ce7310979bc45479"} {
			if !strings.Contains(text, want) {
				t.Fatalf("notice lacks %q: %s", want, text)
			}
		}
		for _, key := range []string{"rotation-commit-" + r.ID, "rotation-commit-replay"} {
			if again, err := f.commitAsIs(r, key); err != nil || again.ID != committed.ID {
				t.Fatalf("replay %s: %+v %v", key, again, err)
			}
		}
		if n := len(ownerNotices(f)); n != 1 {
			t.Fatalf("replays posted %d notices", n)
		}
	}
	// A block that clears before the commit: the rotation completes as an
	// ordinary one once the successor is ready, and the notice says so.
	f := newBlockedFixture(t, api.ProviderBlockRateLimited, api.DefaultHandlerDeadSilenceMinutes)
	f.at(t, api.HandlerProviderBlockedAfter)
	r, err := f.prepareBlocked(t, "blocked-prepare", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.idle(t, f.old)
	f.successor(t, r, api.HandlerArm{Runtime: f.old.Runtime})
	if committed, err := f.commit(r, "rotation-commit-"+r.ID); err != nil || committed.State != api.HandlerRotationCommitted {
		t.Fatalf("commit after recovery %+v %v", committed, err)
	}
	if notices := ownerNotices(f); len(notices) != 1 || !strings.Contains(notices[0].Envelope.Body.Text, "it cleared before the commit") {
		t.Fatalf("notices %+v", notices)
	}
}

// fillAgentCap sets the store's cap so that nothing more fits beside what
// already stands against it, and returns the open and reserved counts.
func (f *rotationFixture) fillAgentCap(t *testing.T) (open, reserved int) {
	t.Helper()
	open, reserved, err := projectAgentCapUsage(context.Background(), f.s.db, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.s.MaxAgents = open + reserved
	return open, reserved
}

// A full project refuses the prepare with no_agent_slot and saves no
// rotation, so queue dispatch still leases the old handler; one free slot
// prepares (bug wi_caa61d47ad431bf5). This manual refusal also records a
// successor hold (wi_bbea3ea3ad6068a1), which a team that fits beside it
// does not notice.
func TestHandlerRotationNoAgentSlotRefusesAndSavesNoRotation(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	s := f.s
	q, err := s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "no-slot-add", Operation: "add", ItemID: f.items[0].ID, OrderMessageSeq: f.orders[0].Seq, Host: "mini", Cwd: "/no-slot"})
	if err != nil {
		t.Fatal(err)
	}
	open, _ := f.fillAgentCap(t)
	_, err = f.prepare(t, "no-slot-prepare", api.Agent{}, "")
	if code := refusalCode(err); code != api.HandlerRotationRefusedNoAgentSlot {
		t.Fatalf("full project prepare: code %q, %v", code, err)
	}
	if want := fmt.Sprintf("project agent cap: %d open + 1 seats > %d", open, open); !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal detail %q lacks %q", err, want)
	}
	if n := countRows(t, s, `SELECT count(*) FROM handler_rotations WHERE task_id=?`, f.task.ID); n != 0 {
		t.Fatalf("refused prepare saved %d rotation(s)", n)
	}
	if n := countRows(t, s, `SELECT count(*) FROM handler_rotation_requests WHERE task_id=?`, f.task.ID); n != 0 {
		t.Fatalf("refused prepare saved %d request receipt(s)", n)
	}
	if n := countRows(t, s, `SELECT count(*) FROM events WHERE task_id=? AND text='Handler rotation prepared'`, f.task.ID); n != 0 {
		t.Fatalf("refused prepare recorded %d event(s)", n)
	}
	// Admission has its own cap check for the team's seats; with room for the
	// team, no prepared rotation holds the handler back from the lease.
	s.MaxAgents = 64
	claimed, err := s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "no-slot-claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
	if err != nil || claimed.HandlerID != f.old.ID {
		t.Fatalf("dispatch after a refused prepare: %+v %v", claimed, err)
	}
	// The lease would make the handler busy; end it as a finished team does.
	if _, err = s.db.Exec(`UPDATE team_queue_entries SET state='finished',released_at=? WHERE id=?`, ts(s.now()), q.ID); err != nil {
		t.Fatal(err)
	}
	s.MaxAgents = open + 1
	r, err := f.prepare(t, "no-slot-prepare-freed", api.Agent{}, "")
	if err != nil || r.State != api.HandlerRotationPrepared {
		t.Fatalf("prepare with one free slot: %+v %v", r, err)
	}
}

// Seats an admitted launch still has to register count against the cap.
func TestHandlerRotationNoAgentSlotCountsReservedSeats(t *testing.T) {
	f := newRotationFixture(t)
	if _, err := f.s.db.Exec(`INSERT INTO team_launch_reservations(task_id,entry_id,item_id,token,pause_generation,state,created_at) VALUES(?,'',?,'token',0,'reserved',?)`, f.task.ID, f.items[0].ID, ts(f.s.now())); err != nil {
		t.Fatal(err)
	}
	open, reserved := f.fillAgentCap(t)
	if reserved == 0 {
		t.Fatal("the manual reservation reserved no seats")
	}
	_, err := f.prepare(t, "reserved-prepare", api.Agent{}, "")
	if code := refusalCode(err); code != api.HandlerRotationRefusedNoAgentSlot {
		t.Fatalf("reserved seats fill the cap: code %q, %v", code, err)
	}
	if want := fmt.Sprintf("%d open + %d reserved + 1 seats > %d", open, reserved, open+reserved); !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal detail %q lacks %q", err, want)
	}
	if _, err = f.s.db.Exec(`DELETE FROM team_launch_reservations WHERE task_id=?`, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if r, err := f.prepare(t, "reserved-prepare-gone", api.Agent{}, ""); err != nil || r.State != api.HandlerRotationPrepared {
		t.Fatalf("prepare once the reservation is gone: %+v %v", r, err)
	}
}

// A busy handler keeps its silent busy refusal on a full project.
func TestHandlerRotationBusyRefusalPrecedesNoAgentSlot(t *testing.T) {
	f := newRotationFixture(t)
	f.activity(t, f.old, "working", "")
	f.fillAgentCap(t)
	if _, err := f.prepare(t, "busy-full", api.Agent{}, ""); refusalCode(err) != api.HandlerRotationRefusedWorking {
		t.Fatalf("busy full project: %v", err)
	}
}

// After a manual abort on a full project, a new prepare is refused and only
// the aborted rotation exists: nothing holds dispatch.
func TestHandlerRotationManualAbortThenFullProjectPreparesNothing(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	r, err := f.prepare(t, "abort-first", api.Agent{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.HandlerRotationAction(ctx, f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationAbort, RequestID: "abort-manual", RotationID: r.ID}, f.by); err != nil {
		t.Fatal(err)
	}
	f.fillAgentCap(t)
	if _, err = f.prepare(t, "abort-again", api.Agent{}, ""); refusalCode(err) != api.HandlerRotationRefusedNoAgentSlot {
		t.Fatalf("prepare after the manual abort on a full project: %v", err)
	}
	rotations, err := f.s.ListHandlerRotations(ctx, f.task.ID)
	if err != nil || len(rotations) != 1 || rotations[0].ID != r.ID || rotations[0].State != api.HandlerRotationAborted {
		t.Fatalf("rotations after the refused prepare: %+v %v", rotations, err)
	}
}

// Successor holds (bug wi_bbea3ea3ad6068a1, order #49564): a template or
// manual prepare refused no_agent_slot holds the next free agent seat.

// prepareFor prepares the fixture's primary for a reason and trigger.
func (f *rotationFixture) prepareFor(t *testing.T, key, reason, trigger string) (api.HandlerRotation, error) {
	t.Helper()
	task, err := f.s.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f.s.HandlerRotationAction(context.Background(), f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationPrepare, RequestID: key,
		ExpectedHandlerRevision: task.HandlerRevision, OldAgentID: f.old.ID, OldRunID: f.old.RunID, SuccessorAgentID: api.NewID("agt"),
		SuccessorName: api.HandlerSuccessorName(f.old.Name, task.HandlerRevision), Reason: reason, Trigger: trigger}, f.by)
}

// releaseHold sends release_hold for an exact run.
func (f *rotationFixture) releaseHold(key string, a api.Agent) (api.HandlerRotation, error) {
	return f.s.HandlerRotationAction(context.Background(), f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationReleaseHold, RequestID: key,
		OldAgentID: a.ID, OldRunID: a.RunID}, f.by)
}

func (f *rotationFixture) holdRows(t *testing.T) int {
	t.Helper()
	return countRows(t, f.s, `SELECT count(*) FROM handler_successor_holds WHERE task_id=?`, f.task.ID)
}

// heldOnFullProject fills the cap and has the hub refuse a prepare for
// reason, so the refusal records a hold; it returns the open count.
func (f *rotationFixture) heldOnFullProject(t *testing.T, reason, trigger string) int {
	t.Helper()
	open, _ := f.fillAgentCap(t)
	_, err := f.prepareFor(t, "hold-"+reason, reason, trigger)
	if refusalCode(err) != api.HandlerRotationRefusedNoAgentSlot || !strings.Contains(err.Error(), "; the next free agent seat is held for the successor") {
		t.Fatalf("%s prepare on a full project: %v", reason, err)
	}
	if n := f.holdRows(t); n != 1 {
		t.Fatalf("%s refusal saved %d hold(s)", reason, n)
	}
	return open
}

// a1: a runner template prepare refused no_agent_slot saves one hold and no
// rotation, receipt or prepared event; the freed seats go to the successor,
// not to a queue claim, and the template prepare takes the one seat it needs.
func TestHandlerRotationTemplateHoldHoldsSuccessorSeat(t *testing.T) {
	f := newRotationFixture(t)
	s := f.s
	ctx := context.Background()
	q, err := s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "hold-add", Operation: "add", ItemID: f.items[0].ID, OrderMessageSeq: f.orders[0].Seq, Host: "mini", Cwd: "/hold"})
	if err != nil {
		t.Fatal(err)
	}
	if held := successorSeat(t, s, f.task.ID); held != 0 {
		t.Fatalf("seat held before any refusal: %d", held)
	}
	open := f.heldOnFullProject(t, api.HandlerRotationReasonTemplate, api.HandlerRotationTriggerRunner)
	// The runner retries every tick with a new request ID: the hold stays one
	// row with one event.
	if _, err = f.prepareFor(t, "hold-template-retry", api.HandlerRotationReasonTemplate, api.HandlerRotationTriggerRunner); refusalCode(err) != api.HandlerRotationRefusedNoAgentSlot {
		t.Fatalf("retried template prepare: %v", err)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM handler_successor_holds WHERE task_id=? AND reason='template' AND trigger_kind='runner'`: 1,
		`SELECT count(*) FROM handler_rotations WHERE task_id=?`:                                                       0,
		`SELECT count(*) FROM handler_rotation_requests WHERE task_id=?`:                                               0,
		`SELECT count(*) FROM events WHERE task_id=? AND text='Handler rotation prepared'`:                             0,
		`SELECT count(*) FROM events WHERE task_id=? AND text='Handler successor seat held'`:                           1,
	} {
		if n := countRows(t, s, query, f.task.ID); n != want {
			t.Fatalf("%s: %d, want %d", query, n, want)
		}
	}
	if held := successorSeat(t, s, f.task.ID); held != 1 {
		t.Fatalf("seat held after the template refusal: %d", held)
	}
	s.MaxAgents = open + 6
	reason := fmt.Sprintf("project agent cap: %d open + 1 seat held for the handler successor + 6 seats > %d", open, open+6)
	claim := api.TeamQueueRequest{RequestID: "hold-claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"}
	if _, err := s.TeamQueueAction(ctx, f.task.ID, claim); !errors.Is(err, api.ErrConflict) || err.Error() != "conflict: "+reason {
		t.Fatalf("claim beside a template hold: %v", err)
	}
	if list, err := s.ListTeamQueue(ctx, f.task.ID); err != nil || list.Entries[0].BlockReason != reason {
		t.Fatalf("list reason beside a template hold %+v %v", list.Entries, err)
	}
	// The prepare's own check ignores the hold: one free slot prepares, and
	// the prepared rotation holds the seat from then on.
	s.MaxAgents = open + 1
	r, err := f.prepareFor(t, "hold-template-freed", api.HandlerRotationReasonTemplate, api.HandlerRotationTriggerRunner)
	if err != nil || r.State != api.HandlerRotationPrepared {
		t.Fatalf("template prepare with one free slot: %+v %v", r, err)
	}
	if n := f.holdRows(t); n != 0 {
		t.Fatalf("hold left after the prepare: %d", n)
	}
	if held := successorSeat(t, s, f.task.ID); held != 1 {
		t.Fatalf("seat held while the successor has not registered: %d", held)
	}
}

// a2: with no hold, admission and its text are today's.
func TestHandlerRotationNoHoldLeavesAdmissionUnchanged(t *testing.T) {
	f := newRotationFixture(t)
	s := f.s
	ctx := context.Background()
	q, err := s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "nohold-add", Operation: "add", ItemID: f.items[0].ID, OrderMessageSeq: f.orders[0].Seq, Host: "mini", Cwd: "/nohold"})
	if err != nil {
		t.Fatal(err)
	}
	if held := successorSeat(t, s, f.task.ID); held != 0 || f.holdRows(t) != 0 {
		t.Fatalf("seat held with no hold: %d", held)
	}
	open, _ := queueCapUsage(t, s, f.task.ID)
	s.MaxAgents = open + 5
	reason := fmt.Sprintf("project agent cap: %d open + 6 seats > %d", open, open+5)
	if list, err := s.ListTeamQueue(ctx, f.task.ID); err != nil || list.Entries[0].BlockReason != reason {
		t.Fatalf("list reason %+v %v", list.Entries, err)
	}
	s.MaxAgents = open + 6
	claim := api.TeamQueueRequest{RequestID: "nohold-claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"}
	if claimed, err := s.TeamQueueAction(ctx, f.task.ID, claim); err != nil || claimed.State != "launching" {
		t.Fatalf("claim at exact fit %+v %v", claimed, err)
	}
}

// a3: a template hold counts only while the limit policy and its template
// check are on.
func TestHandlerRotationTemplateHoldNeedsPolicy(t *testing.T) {
	f := newRotationFixture(t)
	s := f.s
	ctx := context.Background()
	f.heldOnFullProject(t, api.HandlerRotationReasonTemplate, api.HandlerRotationTriggerRunner)
	setPolicy := func(enabled, onTemplate bool) {
		t.Helper()
		policy, err := s.HandlerRotationPolicy(ctx, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: policy.Revision, Enabled: enabled, MaxItems: 10, MaxTotalTokens: 1 << 40, OnTemplateChange: onTemplate}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		enabled, onTemplate bool
		want                int
	}{{false, true, 0}, {true, false, 0}, {false, false, 0}, {true, true, 1}} {
		setPolicy(c.enabled, c.onTemplate)
		if held := successorSeat(t, s, f.task.ID); held != c.want {
			t.Fatalf("enabled=%v onTemplateChange=%v: held %d, want %d", c.enabled, c.onTemplate, held, c.want)
		}
	}
	// The hold row stays: the policy only decides whether it counts.
	if n := f.holdRows(t); n != 1 {
		t.Fatalf("hold rows %d", n)
	}
}

// a3: a hold names the exact old run; once the primary runs a new run, it no
// longer counts or appears in the due list.
func TestHandlerRotationHoldForOldRunDoesNotCount(t *testing.T) {
	f := newRotationFixture(t)
	s := f.s
	f.heldOnFullProject(t, api.HandlerRotationReasonManual, api.HandlerRotationTriggerOwner)
	if held := successorSeat(t, s, f.task.ID); held != 1 {
		t.Fatalf("seat held for the current run: %d", held)
	}
	// A relaunch of the same handler starts a new run.
	if _, err := s.db.Exec(`UPDATE agents SET run_id=? WHERE id=?`, api.NewID("run"), f.old.ID); err != nil {
		t.Fatal(err)
	}
	if held := successorSeat(t, s, f.task.ID); held != 0 {
		t.Fatalf("seat held for an earlier run: %d", held)
	}
	list, err := s.HandlerRotationsDue(context.Background(), "mini", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range list.Entries {
		if d.TaskID == f.task.ID && d.SuccessorHold != nil {
			t.Fatalf("due list names an earlier run's hold %+v", d.SuccessorHold)
		}
	}
}

// a4: a manual hold counts from its refusal, with the limit policy off too,
// until release_hold for the exact run ends it; a release with no hold is
// NotFound and writes nothing.
func TestHandlerRotationManualHoldUntilReleased(t *testing.T) {
	f := newRotationFixture(t)
	s := f.s
	ctx := context.Background()
	q, err := s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "manual-add", Operation: "add", ItemID: f.items[0].ID, OrderMessageSeq: f.orders[0].Seq, Host: "mini", Cwd: "/manual"})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := s.HandlerRotationPolicy(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if _, err = s.SetHandlerRotationPolicy(ctx, f.task.ID, api.HandlerRotationPolicyRequest{ExpectedRevision: policy.Revision, Enabled: false, DeadSilenceMinutes: &zero}); err != nil {
		t.Fatal(err)
	}
	open := f.heldOnFullProject(t, api.HandlerRotationReasonManual, api.HandlerRotationTriggerOwner)
	s.MaxAgents = open + 6
	claim := api.TeamQueueRequest{RequestID: "manual-claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"}
	reason := fmt.Sprintf("project agent cap: %d open + 1 seat held for the handler successor + 6 seats > %d", open, open+6)
	if _, err := s.TeamQueueAction(ctx, f.task.ID, claim); err == nil || err.Error() != "conflict: "+reason {
		t.Fatalf("claim beside a manual hold: %v", err)
	}
	wrongRun := f.old
	wrongRun.RunID = "run_0000000000000009"
	for name, a := range map[string]api.Agent{"wrong run": wrongRun, "wrong agent": f.worker} {
		if _, err := f.releaseHold("release-"+strings.ReplaceAll(name, " ", "-"), a); !errors.Is(err, api.ErrNotFound) {
			t.Fatalf("release for the %s: %v", name, err)
		}
	}
	if n := f.holdRows(t); n != 1 {
		t.Fatalf("a refused release removed the hold: %d", n)
	}
	r, err := f.releaseHold("release-exact", f.old)
	if err != nil || r.ID != "" || r.State != api.HandlerRotationAborted || r.Reason != api.HandlerRotationReasonManual || r.OldAgentID != f.old.ID || r.OldRunID != f.old.RunID || r.OldName != f.old.Name {
		t.Fatalf("release %+v %v", r, err)
	}
	if n := f.holdRows(t); n != 0 {
		t.Fatalf("hold rows after the release: %d", n)
	}
	if n := countRows(t, s, `SELECT count(*) FROM events WHERE task_id=? AND text='Handler successor seat released'`, f.task.ID); n != 1 {
		t.Fatalf("released events %d", n)
	}
	if n := countRows(t, s, `SELECT count(*) FROM handler_rotation_requests WHERE task_id=?`, f.task.ID); n != 0 {
		t.Fatalf("release saved %d receipt(s)", n)
	}
	if _, err := f.releaseHold("release-exact", f.old); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("second release: %v", err)
	}
	if claimed, err := s.TeamQueueAction(ctx, f.task.ID, claim); err != nil || claimed.State != "launching" {
		t.Fatalf("claim after the release %+v %v", claimed, err)
	}
	// A release takes only the run it names.
	for name, change := range map[string]func(*api.HandlerRotationRequest){
		"reason":    func(r *api.HandlerRotationRequest) { r.Reason = api.HandlerRotationReasonManual },
		"rotation":  func(r *api.HandlerRotationRequest) { r.RotationID = "hrot_0000000000000001" },
		"successor": func(r *api.HandlerRotationRequest) { r.SuccessorAgentID = api.NewID("agt") },
		"no run":    func(r *api.HandlerRotationRequest) { r.OldRunID = "" },
	} {
		req := api.HandlerRotationRequest{Operation: api.HandlerRotationReleaseHold, RequestID: "release-invalid", OldAgentID: f.old.ID, OldRunID: f.old.RunID}
		change(&req)
		if _, err := s.HandlerRotationAction(ctx, f.task.ID, req, f.by); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("release with %s: %v", name, err)
		}
	}
	agentReq := api.HandlerRotationRequest{Operation: api.HandlerRotationReleaseHold, RequestID: "release-agent", OldAgentID: f.old.ID, OldRunID: f.old.RunID, ActorAgentID: f.worker.ID}
	if _, err := s.HandlerRotationAction(ctx, f.task.ID, agentReq, f.by); refusalCode(err) != api.HandlerRotationRefusedAgentCaller {
		t.Fatalf("release by an agent session: %v", err)
	}
}

// The due list carries the hold of the exact primary run.
func TestHandlerRotationDueListCarriesHold(t *testing.T) {
	f := newRotationFixture(t)
	f.heldOnFullProject(t, api.HandlerRotationReasonTemplate, api.HandlerRotationTriggerRunner)
	list, err := f.s.HandlerRotationsDue(context.Background(), "mini", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range list.Entries {
		if d.TaskID == f.task.ID {
			if h := d.SuccessorHold; h == nil || h.Reason != api.HandlerRotationReasonTemplate || h.Trigger != api.HandlerRotationTriggerRunner || h.OldAgentID != f.old.ID || h.OldRunID != f.old.RunID {
				t.Fatalf("due entry hold %+v", d.SuccessorHold)
			}
			return
		}
	}
	t.Fatal("the project is not listed")
}

// a6: items, tokens, provider_blocked and dead_primary prepares refused
// no_agent_slot record no hold (the hub's due rule holds their seat), and
// their refusal text is today's.
func TestHandlerRotationOtherReasonsRecordNoHold(t *testing.T) {
	check := func(t *testing.T, f *rotationFixture, name string, err error) {
		t.Helper()
		if refusalCode(err) != api.HandlerRotationRefusedNoAgentSlot || strings.Contains(err.Error(), "held for the successor") {
			t.Fatalf("%s prepare on a full project: %v", name, err)
		}
		if n := f.holdRows(t); n != 0 {
			t.Fatalf("%s refusal saved %d hold(s)", name, n)
		}
	}
	for _, reason := range []string{api.HandlerRotationReasonItems, api.HandlerRotationReasonTokens} {
		f := newRotationFixture(t)
		f.fillAgentCap(t)
		_, err := f.prepareFor(t, "other-"+reason, reason, api.HandlerRotationTriggerRunner)
		check(t, f, reason, err)
	}
	blocked := newBlockedFixture(t, api.ProviderBlockRateLimited, api.DefaultHandlerDeadSilenceMinutes)
	blocked.at(t, api.HandlerProviderBlockedAfter)
	blocked.fillAgentCap(t)
	_, err := blocked.prepareBlocked(t, "other-blocked", nil)
	check(t, blocked.rotationFixture, "provider_blocked", err)
	dead := newDeadPrimaryFixture(t)
	dead.advance(11 * time.Minute)
	dead.fillAgentCap(t)
	_, err = dead.prepareDead(t, "other-dead", nil)
	check(t, dead.rotationFixture, "dead_primary", err)
}

// a5 (g3): automatic provisioning leaves the held seat free, in the listed
// need and in the reserve, for a seat held by the due rule and by a hold.
func TestHandlerRotationProvisionCountsHeldSeat(t *testing.T) {
	for _, held := range []string{"tokens due", "manual hold"} {
		f := newProvisionFixture(t, 4, 3, 3)
		ctx := context.Background()
		primary := provisionPrimary(t, f)
		open, reserved, err := projectAgentCapUsage(ctx, f.s.db, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Room for the team and the new handler: today's rule provisions.
		f.s.MaxAgents = open + reserved + featureTeamSlots + 1
		if got := listedEntry(t, f.s, f.task.ID, f.entries[3].ID); got.HandlerNeed == nil || !got.HandlerNeed.Provision {
			t.Fatalf("%s: before the seat is held %+v", held, got.HandlerNeed)
		}
		switch held {
		case "tokens due":
			rotationDueByTokens(t, f.s, f.task.ID, primary)
		case "manual hold":
			// This fixture's primary holds a live lease, which refuses a
			// prepare before its cap check; the row is the one a refused
			// manual prepare writes (TestHandlerRotationManualHoldUntilReleased).
			if _, err = f.s.db.Exec(`INSERT INTO handler_successor_holds(task_id,reason,trigger_kind,old_agent_id,old_run_id,handler_revision,created_at) VALUES(?,'manual','owner',?,?,?,?)`,
				f.task.ID, primary.ID, primary.RunID, f.task.HandlerRevision, ts(f.s.now())); err != nil {
				t.Fatal(err)
			}
		}
		if n := successorSeat(t, f.s, f.task.ID); n != 1 {
			t.Fatalf("%s: seat held %d", held, n)
		}
		want := fmt.Sprintf("cannot add one: project agent cap: %d open", open)
		if reserved > 0 {
			want += fmt.Sprintf(" + %d reserved", reserved)
		}
		want += fmt.Sprintf(" + 1 seat held for the handler successor + %d seats + 1 handler > %d", featureTeamSlots, f.s.MaxAgents)
		got := listedEntry(t, f.s, f.task.ID, f.entries[3].ID)
		if n := got.HandlerNeed; n == nil || n.Provision || !strings.Contains(got.BlockReason, want) {
			t.Fatalf("%s: need %+v reason %q lacks %q", held, n, got.BlockReason, want)
		}
		if _, err = f.provision(got, "held-"+strings.ReplaceAll(held, " ", "-"), api.NewID("agt"), armS, digestP); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: provision beside the held seat: %v", held, err)
		}
		if f.allRows(t) != 0 || f.notices(t, provisionNotice) != 0 {
			t.Fatalf("%s: refused provision left a row or notice", held)
		}
		f.s.MaxAgents++
		if got := listedEntry(t, f.s, f.task.ID, f.entries[3].ID); got.HandlerNeed == nil || !got.HandlerNeed.Provision {
			t.Fatalf("%s: one more seat %+v", held, got.HandlerNeed)
		}
	}
}

// provisionPrimary is the provision fixture's primary database handler.
func provisionPrimary(t *testing.T, f *provisionFixture) api.Agent {
	t.Helper()
	task, err := f.s.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	primary, err := primaryHandler(context.Background(), f.s.db, task)
	if err != nil {
		t.Fatal(err)
	}
	return primary
}
