package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// unconfirmedOrder posts an item-linked order with no scope confirmation.
func unconfirmedOrder(t *testing.T, s *Store, task api.Task, item api.WorkItem, key string) api.Message {
	t.Helper()
	message, err := s.PostMessage(context.Background(), task.ID, api.PostMessageRequest{Text: key, RequestID: key, WorkItems: []api.MessageWorkItem{{ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// choresHandler returns the fixture's first online database handler.
func choresHandler(t *testing.T, s *Store, taskID string) api.Agent {
	t.Helper()
	agents, err := s.ListAgents(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Role == api.AgentRoleDatabaseHandler {
			return a
		}
	}
	t.Fatal("fixture has no database handler")
	return api.Agent{}
}

// a1 (c1): scope confirmation stores canonical ownership, returns it on the
// write, its replay and the GET, and refuses a retry with other paths.
func TestScopeConfirmationRecordsOwnership(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	handler := choresHandler(t, s, task.ID)
	item := items[0]
	order := unconfirmedOrder(t, s, task, item, "owned-order")
	req := api.ConfirmWorkOrderScopeRequest{RequestID: "owned-intake", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true, Ownership: []string{"hub/x.go", "docs/y.md"}}
	bad := req
	bad.RequestID, bad.Ownership = "bad-owned-intake", []string{"../escape"}
	if _, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, bad); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("non-canonical ownership: %v", err)
	}
	filed, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, req)
	if err != nil || strings.Join(filed.Ownership, ",") != "hub/x.go,docs/y.md" {
		t.Fatalf("confirm %+v %v", filed, err)
	}
	if replay, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, req); err != nil || !reflect.DeepEqual(replay, filed) {
		t.Fatalf("replay %+v %v", replay, err)
	}
	changed := req
	changed.Ownership = []string{"hub/x.go"}
	if _, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, changed); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed-owns retry: %v", err)
	}
	got, err := s.GetWorkOrderScopeConfirmation(ctx, task.ID, item.ID, item.Revision, order.Seq)
	if err != nil || strings.Join(got.Ownership, ",") != "hub/x.go,docs/y.md" {
		t.Fatalf("get %+v %v", got, err)
	}
	// A confirmation without ownership still works and returns none.
	plain, err := s.GetWorkOrderScopeConfirmation(ctx, task.ID, items[1].ID, items[1].Revision, orders[1].Seq)
	if err != nil || plain.Ownership != nil {
		t.Fatalf("plain confirmation %+v %v", plain, err)
	}
}

// a2 (c1): the hub refuses a parallel add with no ownership unless it is
// serial; a serial project's unscoped add is serial implicitly; scoping a
// serial entry makes it an ordinary scoped entry.
func TestQueueAddNeedsOwnershipOrSerial(t *testing.T) {
	s, task, items, orders, _ := uncappedFixture(t, 3, 2)
	ctx := context.Background()
	add := func(key string, i int, serial bool, owns ...string) (api.TeamQueueEntry, error) {
		return s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: key, Operation: "add", ItemID: items[i].ID, OrderMessageSeq: orders[i].Seq, Host: "mini", Cwd: "/worktrees/" + items[i].ID, Repository: "repo", BaseCommit: strings.Repeat("a", 40), Ownership: owns, Serial: serial})
	}
	// Serial project (default limit 1): unchanged, and implicitly serial.
	implicit, err := add("serial-project-add", 0, false)
	if err != nil || !implicit.Serial {
		t.Fatalf("serial project add %+v %v", implicit, err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "remove-implicit", Operation: "remove", EntryID: implicit.ID, ExpectedRevision: implicit.Revision}); err != nil {
		t.Fatal(err)
	}
	setQueueLimit(t, s, task.ID, 0)
	if _, err := add("unscoped-parallel", 0, false); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "--serial") {
		t.Fatalf("unscoped parallel add: %v", err)
	}
	if _, err := add("serial-with-owns", 0, true, "src/a"); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("serial entry with ownership: %v", err)
	}
	scoped, err := add("scoped-parallel", 1, false, "src/b")
	if err != nil || scoped.Serial {
		t.Fatalf("scoped add %+v %v", scoped, err)
	}
	if _, err := claimEntry(s, task, scoped); err != nil {
		t.Fatal(err)
	}
	serial, err := add("serial-parallel", 0, true)
	if err != nil || !serial.Serial || len(serial.Ownership) != 0 {
		t.Fatalf("serial add %+v %v", serial, err)
	}
	if got := listedEntry(t, s, task.ID, serial.ID); got.BlockReason != "Serial: runs alone once the active teams finish" || len(got.BlockedBy) != 1 {
		t.Fatalf("serial entry beside an active team: %q %v", got.BlockReason, got.BlockedBy)
	}
	rescoped, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "scope-serial", Operation: "scope", EntryID: serial.ID, ExpectedRevision: serial.Revision, Ownership: []string{"src/a"}})
	if err != nil || rescoped.Serial || strings.Join(rescoped.Ownership, ",") != "src/a" {
		t.Fatalf("scoped serial entry %+v %v", rescoped, err)
	}
	if got := listedEntry(t, s, task.ID, serial.ID); got.Serial || len(got.BlockedBy) != 0 {
		t.Fatalf("rescoped entry still serial %+v", got)
	}
}

// a12: a database created before the new columns migrates, twice, without
// loss; legacy entries read as not serial with no owner integration, and a
// legacy confirmation has no ownership.
func TestQueueChoresMigrationIsAdditiveAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Legacy queue"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Legacy", Priority: "normal", RequestID: "legacy-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	order := unconfirmedOrder(t, s, task, item, "legacy-order")
	handler, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "database", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: "database"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(s.now()), handler.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "legacy-intake", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}); err != nil {
		t.Fatal(err)
	}
	q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "legacy-add", Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "mini", Cwd: "/legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, drop := range []string{
		`ALTER TABLE team_queue_entries DROP COLUMN serial`,
		`ALTER TABLE team_queue_entries DROP COLUMN owner_integration_json`,
		`ALTER TABLE work_order_scope_confirmations DROP COLUMN ownership_json`,
	} {
		if _, err := db.Exec(drop); err != nil {
			t.Fatalf("%s: %v", drop, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := migrate(db); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	for _, c := range []struct{ table, column string }{
		{"team_queue_entries", "serial"},
		{"team_queue_entries", "owner_integration_json"},
		{"work_order_scope_confirmations", "ownership_json"},
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, c.table, c.column).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s.%s count=%d %v", c.table, c.column, n, err)
		}
	}
	db.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetTeamQueueEntry(ctx, task.ID, q.ID)
	if err != nil || got.Serial || got.OwnerIntegration != nil || got.State != "queued" || got.Revision != q.Revision {
		t.Fatalf("legacy entry %+v %v", got, err)
	}
	confirmation, err := s.GetWorkOrderScopeConfirmation(ctx, task.ID, item.ID, item.Revision, order.Seq)
	if err != nil || confirmation.Ownership != nil || confirmation.RequestID != "legacy-intake" {
		t.Fatalf("legacy confirmation %+v %v", confirmation, err)
	}
}

// choresQueue is a parallel project with bug items, one online handler per
// requested count, a fresh host census and a no-cap limit.
type choresQueue struct {
	s        *Store
	task     api.Task
	items    []api.WorkItem
	orders   []api.Message
	handlers []api.Agent
	by       api.Caller
}

func newChoresQueue(t *testing.T, n, h, limit int) *choresQueue {
	t.Helper()
	s, task, _, _, handlers := uncappedFixture(t, 0, h)
	f := &choresQueue{s: s, task: task, handlers: handlers, by: api.Caller{Node: "fixture", User: "owner"}}
	for i := 0; i < n; i++ {
		item, err := s.CreateWorkItem(context.Background(), task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Chores item", Priority: "normal", RequestID: api.NewID("req")}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		f.items = append(f.items, item)
		f.orders = append(f.orders, contextLinkedMessage(t, s, task, item, "bounded order", api.NewID("req"), nil))
	}
	setQueueLimit(t, s, task.ID, limit)
	return f
}

func (f *choresQueue) add(t *testing.T, i int, owns ...string) api.TeamQueueEntry {
	t.Helper()
	return addScopedEntry(t, f.s, f.task, f.items[i], f.orders[i], owns...)
}

// run drives an entry from queued to running through the hub's launch steps.
func (f *choresQueue) run(t *testing.T, q api.TeamQueueEntry) api.TeamQueueEntry {
	t.Helper()
	ctx := context.Background()
	q, err := claimEntry(f.s, f.task, q)
	if err != nil {
		t.Fatal(err)
	}
	step := func(req api.TeamQueueRequest) {
		t.Helper()
		req.RequestID, req.EntryID, req.ExpectedRevision = api.NewID("tqr"), q.ID, q.Revision
		if q, err = f.s.TeamQueueAction(ctx, f.task.ID, req); err != nil {
			t.Fatalf("%s: %v", req.Operation, err)
		}
	}
	run := api.NewID("run")
	plan := fmt.Sprintf(`{"task":%q,"item":%q,"revision":%d,"order":%d,"handlerId":%q,"handlerRunId":%q,"handlerLeaseGeneration":%d,"context":{"version":1},"members":[{"state":"unstarted","runId":%q,"fields":{"agentId":%q,"name":"lead-%s","cwd":%q}}]}`,
		f.task.ID, q.ItemID, q.ItemRevision, q.OrderMessageSeq, q.HandlerID, q.HandlerRunID, q.HandlerLeaseGeneration, run, api.NewID("agt"), q.ID[4:12], q.Cwd)
	step(api.TeamQueueRequest{Operation: "freeze", LaunchJSON: []byte(plan)})
	step(api.TeamQueueRequest{Operation: "attempt", MemberIndex: 0})
	step(api.TeamQueueRequest{Operation: "started", MemberIndex: 0, MemberRunID: run})
	step(api.TeamQueueRequest{Operation: "running"})
	return q
}

// member registers a live agent bound to item i.
func (f *choresQueue) member(t *testing.T, i int, name string) api.Agent {
	t.Helper()
	a, err := f.s.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{Name: name, AgentID: api.NewID("agt"), Host: "mini", Session: name}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,?,?,?,0,?,?,?)`, a.ID, a.RunID, f.task.ID, f.items[i].ID, f.items[i].Revision, f.task.ID, f.orders[i].Seq, "digest", []byte("{}"), ts(f.s.now())); err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *choresQueue) integrated(q api.TeamQueueEntry, key, commit string, changed ...string) (api.TeamQueueEntry, error) {
	return f.s.TeamQueueAction(context.Background(), f.task.ID, api.TeamQueueRequest{RequestID: key, Operation: "owner_integrated", EntryID: q.ID, ExpectedRevision: q.Revision, OwnerIntegrationCommit: commit, OwnerIntegrationEvidence: "owner release record", ChangedFiles: changed})
}

func (f *choresQueue) holding(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM team_queue_entries WHERE task_id=? AND `+queueHoldsSQL, f.task.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// a4 (c2): the owner's integration record releases a running entry's slot,
// handler lease and ownership while its item, bindings and lead stay; the
// item's role:database_handler messages still reach the handler that served
// it. Refusals cover every other state, agent callers and live release jobs.
func TestOwnerIntegratedReleasesRunningEntry(t *testing.T) {
	f := newChoresQueue(t, 3, 1, 0)
	ctx := context.Background()
	a := f.run(t, f.add(t, 0, "src"))
	f.member(t, 0, "member-a")
	b := f.add(t, 1, "src/b")
	c := f.add(t, 2, "docs")
	if got := listedEntry(t, f.s, f.task.ID, b.ID); len(got.BlockedBy) != 1 || got.BlockedBy[0] != a.ID {
		t.Fatalf("B before integration: %+v", got.BlockedBy)
	}
	if _, err := claimEntry(f.s, f.task, c); err == nil || !strings.Contains(err.Error(), "no available database handler lease") {
		t.Fatalf("C claimed without a free handler: %v", err)
	}
	commit := strings.Repeat("b", 40)

	// Refusals before the record: bad SHA, an agent caller, live release jobs.
	if _, err := f.integrated(a, "bad-sha", "abc1234"); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("short SHA: %v", err)
	}
	asHandler := api.TeamQueueRequest{RequestID: "as-handler", Operation: "owner_integrated", EntryID: a.ID, ExpectedRevision: a.Revision, OwnerIntegrationCommit: commit, HandlerAgentID: f.handlers[0].ID, HandlerRunID: f.handlers[0].RunID}
	if _, err := f.s.TeamQueueAction(ctx, f.task.ID, asHandler); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("agent caller: %v", err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO release_jobs VALUES(?,?,?,?,?,?)`, f.task.ID, "rel_fixture", a.ID, "verified", 1, "{}"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"verified", "claimed", "merged", "blocked"} {
		if _, err := f.s.db.Exec(`UPDATE release_jobs SET state=? WHERE id='rel_fixture'`, state); err != nil {
			t.Fatal(err)
		}
		if _, err := f.integrated(a, "live-job-"+state, commit); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "release job rel_fixture is "+state) {
			t.Fatalf("live %s release job: %v", state, err)
		}
	}
	// A finished job does not hold the candidate: refused here, superseded
	// on a second entry below.
	if _, err := f.s.db.Exec(`UPDATE release_jobs SET state='refused' WHERE id='rel_fixture'`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []api.TeamQueueEntry{b} {
		if _, err := f.integrated(q, "queued-"+q.ID, commit); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("queued entry: %v", err)
		}
	}

	itemBefore, err := f.s.GetWorkItem(ctx, f.task.ID, f.items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	released, err := f.integrated(a, "integrated-a", commit, "src/a.go", "src/b/z.go")
	if err != nil {
		t.Fatal(err)
	}
	if released.State != "running" || released.ReleasedAt == "" || released.OwnerIntegration == nil || released.OwnerIntegration.Commit != commit || strings.Join(released.OwnerIntegration.ChangedFiles, ",") != "src/a.go,src/b/z.go" || strings.Join(released.Ownership, ",") != "src/a.go,src/b/z.go" {
		t.Fatalf("integrated entry %+v", released)
	}
	if replay, err := f.integrated(a, "integrated-a", commit, "src/a.go", "src/b/z.go"); err != nil || replay.Revision != released.Revision {
		t.Fatalf("lost-response replay %+v %v", replay, err)
	}
	if _, err := f.integrated(released, "integrated-again", commit); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("already released: %v", err)
	}
	if n := f.holding(t); n != 0 {
		t.Fatalf("%d entries still hold slots", n)
	}
	var reservations int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM team_launch_reservations WHERE task_id=? AND entry_id=?`, f.task.ID, a.ID).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("reservation left %d %v", reservations, err)
	}
	if got := listedEntry(t, f.s, f.task.ID, b.ID); len(got.BlockedBy) != 0 || got.BlockReason != "" {
		t.Fatalf("B after integration: %q %v", got.BlockReason, got.BlockedBy)
	}
	if free, err := freeQueueHandler(ctx, f.s.db, f.task.ID, []api.TeamQueueEntry{}); err != nil || free.ID != a.HandlerID {
		t.Fatalf("lease not free: %+v %v", free, err)
	}
	itemAfter, err := f.s.GetWorkItem(ctx, f.task.ID, f.items[0].ID)
	if err != nil || itemAfter.Status != itemBefore.Status || itemAfter.Revision != itemBefore.Revision {
		t.Fatalf("item changed: %+v %v", itemAfter, err)
	}
	var bound, leads int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM agent_work_item_bindings WHERE item_task_id=? AND item_id=?`, f.task.ID, f.items[0].ID).Scan(&bound); err != nil || bound != 1 {
		t.Fatalf("bindings %d %v", bound, err)
	}
	if err := f.s.db.QueryRow(`SELECT count(*) FROM item_team_leads WHERE task_id=? AND item_id=? AND state<>'closed'`, f.task.ID, f.items[0].ID).Scan(&leads); err != nil || leads != 1 {
		t.Fatalf("item lead %d %v", leads, err)
	}
	message, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{RequestID: "post-release-a13", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.items[0].ID, ItemRevision: f.items[0].Revision, Relationship: "primary"}}, WorkOrderMessage: &api.MessageReference{TaskID: f.task.ID, Seq: f.orders[0].Seq}, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: "role:database_handler", Subject: "Record the post-release check", Body: api.EnvelopeBody{Ask: "Record a13."}}}, f.by)
	if err != nil || message.To != a.HandlerID {
		t.Fatalf("role routing after integration %q want %q: %v", message.To, a.HandlerID, err)
	}
	claimedB, err := claimEntry(f.s, f.task, b)
	if err != nil || claimedB.HandlerID != a.HandlerID {
		t.Fatalf("B did not take the released slot and lease: %+v %v", claimedB, err)
	}
	if _, err := f.integrated(claimedB, "launching-b", commit); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("launching entry: %v", err)
	}
	g := newChoresQueue(t, 1, 1, 0)
	d := g.run(t, g.add(t, 0, "src/d"))
	if _, err := g.s.db.Exec(`INSERT INTO release_jobs VALUES(?,?,?,?,?,?)`, g.task.ID, "rel_superseded", d.ID, "superseded", 1, "{}"); err != nil {
		t.Fatal(err)
	}
	if got, err := g.integrated(d, "integrated-superseded", commit); err != nil || got.ReleasedAt == "" {
		t.Fatalf("superseded release job: %+v %v", got, err)
	}
}

// a4 (c2): a failed, unreleased entry with live runs is released by the
// owner's record; one with an uncertain spawn is refused.
func TestOwnerIntegratedReleasesFailedEntry(t *testing.T) {
	f := newChoresQueue(t, 2, 2, 0)
	ctx := context.Background()
	a := f.run(t, f.add(t, 0, "src/a"))
	f.member(t, 0, "member-a")
	failed, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "fail-a", Operation: "fail", EntryID: a.ID, ExpectedRevision: a.Revision, Failure: "owner integrated abc1234"})
	if err != nil {
		t.Fatal(err)
	}
	released, err := f.integrated(failed, "integrated-failed-a", strings.Repeat("c", 40))
	if err != nil || released.State != "failed" || released.ReleasedAt == "" || strings.Join(released.Ownership, ",") != "src/a" {
		t.Fatalf("failed entry %+v %v", released, err)
	}
	b, err := claimEntry(f.s, f.task, f.add(t, 1, "src/b"))
	if err != nil {
		t.Fatal(err)
	}
	uncertain, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "freeze-b", Operation: "freeze", EntryID: b.ID, ExpectedRevision: b.Revision, LaunchJSON: []byte(fmt.Sprintf(`{"task":%q,"item":%q,"revision":%d,"order":%d,"handlerId":%q,"handlerRunId":%q,"handlerLeaseGeneration":%d,"context":{"version":1},"members":[{"state":"unstarted","runId":%q,"fields":{"agentId":%q,"name":"lead-b","cwd":"/b"}}]}`, f.task.ID, b.ItemID, b.ItemRevision, b.OrderMessageSeq, b.HandlerID, b.HandlerRunID, b.HandlerLeaseGeneration, api.NewID("run"), api.NewID("agt")))})
	if err != nil {
		t.Fatal(err)
	}
	if uncertain, err = f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "attempt-b", Operation: "attempt", EntryID: b.ID, ExpectedRevision: uncertain.Revision, MemberIndex: 0}); err != nil {
		t.Fatal(err)
	}
	if uncertain, err = f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "fail-b", Operation: "fail", EntryID: b.ID, ExpectedRevision: uncertain.Revision, Failure: "spawn crashed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.integrated(uncertain, "integrated-uncertain", strings.Repeat("c", 40)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "uncertain spawn") {
		t.Fatalf("uncertain spawn: %v", err)
	}
}

// a5 (c2): after the owner's record the handler saves a plain done; a save
// that carries an acceptance tuple is refused naming the integration. An
// owner scope narrows a failed, unreleased entry and refuses a widening.
func TestOwnerIntegratedDoneSaveAndFailedScope(t *testing.T) {
	f := newChoresQueue(t, 2, 2, 0)
	ctx := context.Background()
	a := f.run(t, f.add(t, 0, "src/a"))
	// The entry needs a repository for a pending acceptance; the fixture has
	// one, so a handler's plain done save is refused before the record.
	handler := f.handlers[1]
	for _, h := range f.handlers {
		if h.ID != a.HandlerID {
			handler = h
		}
	}
	done := "done"
	save := func(key string, accept *api.WorkItemQueueAcceptance) error {
		item, err := f.s.GetWorkItem(ctx, f.task.ID, f.items[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = f.s.CreateWorkItemUpdate(ctx, f.task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, Status: &done, AgentID: handler.ID, RunID: handler.RunID, RequestID: key, QueueAcceptance: accept}, f.by)
		return err
	}
	if err := save("plain-before", nil); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("plain save before the record: %v", err)
	}
	commit := strings.Repeat("d", 40)
	if _, err := f.integrated(a, "integrated-a", commit, "src/a/x.go"); err != nil {
		t.Fatal(err)
	}
	tuple := &api.WorkItemQueueAcceptance{EntryID: a.ID, Worktree: "/worktrees/a", Branch: "feature/a", Commit: commit}
	if err := save("tuple-after", tuple); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "integrated by the owner at "+commit) || !strings.Contains(err.Error(), "without --worktree/--branch/--commit") {
		t.Fatalf("tuple save after the record: %v", err)
	}
	if err := save("plain-after", nil); err != nil {
		t.Fatalf("plain save after the record: %v", err)
	}
	if item, _ := f.s.GetWorkItem(ctx, f.task.ID, f.items[0].ID); item.Status != "done" {
		t.Fatalf("item not done: %+v", item)
	}

	b := f.run(t, f.add(t, 1, "src/b", "docs/b"))
	failed, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "fail-b", Operation: "fail", EntryID: b.ID, ExpectedRevision: b.Revision, Failure: "stuck"})
	if err != nil {
		t.Fatal(err)
	}
	scope := func(key string, owns ...string) (api.TeamQueueEntry, error) {
		return f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: key, Operation: "scope", EntryID: failed.ID, ExpectedRevision: failed.Revision, Ownership: owns})
	}
	if _, err := scope("widen-b", "src/b", "hub"); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "only be narrowed") {
		t.Fatalf("widening a failed entry: %v", err)
	}
	narrowed, err := scope("narrow-b", "src/b/one.go")
	if err != nil || strings.Join(narrowed.Ownership, ",") != "src/b/one.go" || narrowed.State != "failed" || narrowed.ReleasedAt != "" {
		t.Fatalf("narrowed failed entry %+v %v", narrowed, err)
	}
	asHandler := api.TeamQueueRequest{RequestID: "handler-narrow-b", Operation: "scope", EntryID: narrowed.ID, ExpectedRevision: narrowed.Revision, Ownership: []string{"src/b/one.go"}, HandlerAgentID: narrowed.HandlerID, HandlerRunID: narrowed.HandlerRunID}
	if _, err := f.s.TeamQueueAction(ctx, f.task.ID, asHandler); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("handler scoping a failed entry: %v", err)
	}
}

// clock pins the store's time and returns a function that advances it; the
// host census is refreshed so admission stays fresh at the new time.
func (f *choresQueue) clock(t *testing.T) func(time.Duration) {
	t.Helper()
	now := f.s.now()
	f.s.now = func() time.Time { return now }
	return func(d time.Duration) {
		now = now.Add(d)
		observeFixtureHost(t, f.s, f.task.ID, 0, freeDiskMiB(1<<20))
	}
}

// activity records a member's reported activity state at the store's now.
func (f *choresQueue) activity(t *testing.T, a api.Agent, state string) {
	t.Helper()
	if _, err := f.s.db.Exec(`INSERT INTO agent_activity(task_id,agent_id,run_id,state,observed_at,payload,request_id) VALUES(?,?,?,?,?,?,?) ON CONFLICT(agent_id,run_id) DO UPDATE SET state=excluded.state,observed_at=excluded.observed_at,payload=excluded.payload`,
		f.task.ID, a.ID, a.RunID, state, ts(f.s.now()), fmt.Sprintf(`{"state":%q}`, state), api.NewID("req")); err != nil {
		t.Fatal(err)
	}
}

func (f *choresQueue) fail(t *testing.T, q api.TeamQueueEntry) api.TeamQueueEntry {
	t.Helper()
	failed, err := f.s.TeamQueueAction(context.Background(), f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: "fixture failure"})
	if err != nil {
		t.Fatal(err)
	}
	return failed
}

func (f *choresQueue) stall(t *testing.T, id string) *api.TeamQueueStall {
	t.Helper()
	return listedEntry(t, f.s, f.task.ID, id).Stall
}

func assertStall(t *testing.T, f *choresQueue, id, cause, blocker string, fix ...string) {
	t.Helper()
	got := listedEntry(t, f.s, f.task.ID, id)
	if got.Stall == nil || got.Stall.Cause != cause || got.Stall.BlockerEntryID != blocker || got.Stall.Since == "" {
		t.Fatalf("stall on %s: %+v reason %q", id, got.Stall, got.BlockReason)
	}
	if !strings.HasPrefix(got.BlockReason, "Stalled: ") || !strings.Contains(got.BlockReason, "Fix: "+got.Stall.Fix) {
		t.Fatalf("block reason %q", got.BlockReason)
	}
	for _, want := range fix {
		if !strings.Contains(got.Stall.Fix, want) {
			t.Fatalf("fix %q lacks %q", got.Stall.Fix, want)
		}
	}
}

func assertNoStall(t *testing.T, f *choresQueue, id, why string) {
	t.Helper()
	if got := listedEntry(t, f.s, f.task.ID, id); got.Stall != nil || strings.HasPrefix(got.BlockReason, "Stalled") {
		t.Fatalf("%s: unexpected stall %+v reason %q", why, got.Stall, got.BlockReason)
	}
}

// a6 (c3): failed-entry. A failed parallel entry with live runs stalls the
// work behind it at once; one with no live runs is released by the runner,
// so it is not a stall.
func TestQueueStallFailedEntry(t *testing.T) {
	f := newChoresQueue(t, 2, 2, 0)
	a := f.run(t, f.add(t, 0, "src"))
	b := f.add(t, 1, "src/b")
	failed := f.fail(t, a)
	assertNoStall(t, f, b.ID, "failed entry without live runs")
	f.member(t, 0, "member-a")
	assertStall(t, f, b.ID, api.StallFailedEntry, failed.ID, "tt team queue integrated --task "+f.task.ID+" --entry "+failed.ID, "tt close --team")
}

// a6 (c3): nothing-running, once the durable time has passed the grace.
func TestQueueStallNothingRunning(t *testing.T) {
	f := newChoresQueue(t, 2, 2, 0)
	advance := f.clock(t)
	a := f.run(t, f.add(t, 0, "src"))
	b := f.add(t, 1, "src/b")
	assertNoStall(t, f, b.ID, "launch just finished")
	advance(6 * time.Minute)
	assertStall(t, f, b.ID, api.StallNothingRunning, a.ID, "tt team queue fail --task "+f.task.ID+" --entry "+a.ID)
}

// a6 (c3): idle-entry needs every live member idle or done, no open work and
// the idle threshold; below it, or with an open obligation, it is not a
// stall.
func TestQueueStallIdleEntry(t *testing.T) {
	f := newChoresQueue(t, 2, 2, 0)
	f.s.queueIdleThreshold = 30 * time.Minute
	advance := f.clock(t)
	a := f.run(t, f.add(t, 0, "src"))
	lead := f.member(t, 0, "lead-a")
	worker := f.member(t, 0, "worker-a")
	b := f.add(t, 1, "src/b")
	f.activity(t, lead, "idle")
	f.activity(t, worker, "working")
	advance(40 * time.Minute)
	assertNoStall(t, f, b.ID, "a member is working")
	f.activity(t, worker, "finished_silent")
	advance(10 * time.Minute)
	assertNoStall(t, f, b.ID, "idle below the threshold")
	advance(25 * time.Minute)
	assertStall(t, f, b.ID, api.StallIdleEntry, a.ID, "tt team queue integrated", "tt team queue scope --task "+f.task.ID+" --entry "+a.ID)
	if _, err := f.s.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{RequestID: "open-work", AgentID: worker.ID, RunID: worker.RunID, To: "owner", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.items[0].ID, ItemRevision: f.items[0].Revision, Relationship: "primary"}}, WorkOrderMessage: &api.MessageReference{TaskID: f.task.ID, Seq: f.orders[0].Seq}, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: "owner", Subject: "Choose whether to check the release once more", Body: api.EnvelopeBody{Ask: "Check it?"}}}, f.by); err != nil {
		t.Fatal(err)
	}
	advance(40 * time.Minute)
	assertNoStall(t, f, b.ID, "an open obligation")
}

// report sends a member's idle activity through ReportActivity at the store's
// now, with or without a matrix wait.
func (f *choresQueue) report(t *testing.T, a api.Agent, wait *api.MatrixWait) (api.AgentActivity, error) {
	t.Helper()
	return f.s.ReportActivity(context.Background(), f.task.ID, a.ID, api.ActivityReport{RequestID: api.NewID("req"), RunID: a.RunID, Activity: api.AgentActivity{State: "idle", ObservedAt: f.s.now(), MatrixWait: wait}})
}

// idleTeam is a running entry whose lead and worker are idle, with a queued
// entry behind it that overlaps its ownership.
func idleTeam(t *testing.T) (f *choresQueue, advance func(time.Duration), a, b api.TeamQueueEntry, lead, worker api.Agent) {
	t.Helper()
	f = newChoresQueue(t, 2, 2, 0)
	f.s.queueIdleThreshold = 30 * time.Minute
	advance = f.clock(t)
	a = f.run(t, f.add(t, 0, "src"))
	lead, worker = f.member(t, 0, "lead-a"), f.member(t, 0, "worker-a")
	b = f.add(t, 1, "src/b")
	f.activity(t, lead, "idle")
	f.activity(t, worker, "idle")
	return
}

// s1: an unanswered owner decision (tt ask) from a member is open work; the
// answer restarts the idle time.
func TestQueueStallIdleEntryOwnerDecision(t *testing.T) {
	f, advance, a, b, lead, _ := idleTeam(t)
	ctx := context.Background()
	// In a parallel project only the item's lead may ask, with its item link.
	if _, err := f.s.db.Exec(`UPDATE item_team_leads SET agent_id=?,run_id=?,state='running' WHERE task_id=? AND item_id=?`, lead.ID, lead.RunID, f.task.ID, f.items[0].ID); err != nil {
		t.Fatal(err)
	}
	req := decisionRequest(lead, "stall-ask")
	req.WorkItems = []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.items[0].ID, ItemRevision: f.items[0].Revision, Relationship: "primary"}}
	req.WorkOrderMessage = &api.MessageReference{TaskID: f.task.ID, Seq: f.orders[0].Seq}
	ask, err := f.s.CreateDecision(ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	var obligations int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM obligations WHERE task_id=?`, f.task.ID).Scan(&obligations); err != nil || obligations != 0 {
		t.Fatalf("a decision request made %d obligations (%v); this test needs none", obligations, err)
	}
	advance(40 * time.Minute)
	assertNoStall(t, f, b.ID, "an unanswered owner decision")
	advance(5 * time.Hour)
	assertNoStall(t, f, b.ID, "an owner decision has no stall bound")
	if _, err := f.s.AnswerDecision(ctx, f.task.ID, ask.Seq, api.AnswerDecisionRequest{RequestID: "stall-answer", OptionID: "staged"}, f.by); err != nil {
		t.Fatal(err)
	}
	advance(10 * time.Minute)
	assertNoStall(t, f, b.ID, "idle below the threshold since the answer")
	advance(25 * time.Minute)
	assertStall(t, f, b.ID, api.StallIdleEntry, a.ID, "tt team queue scope --task "+f.task.ID+" --entry "+a.ID)
}

// s2, s4: a member's reported matrix wait, waiting or running, is open work
// though every member is idle; a report without it restarts the idle time.
func TestQueueStallIdleEntryMatrixWait(t *testing.T) {
	f, advance, a, b, lead, _ := idleTeam(t)
	if _, err := f.report(t, lead, &api.MatrixWait{Role: api.MatrixWaitWaiting, Item: f.items[0].ID, Position: 2, Length: 3, Since: f.s.now()}); err != nil {
		t.Fatal(err)
	}
	advance(40 * time.Minute)
	assertNoStall(t, f, b.ID, "a member waits for the matrix host")
	if _, err := f.report(t, lead, &api.MatrixWait{Role: api.MatrixWaitRunning, Item: f.items[0].ID, Since: f.s.now()}); err != nil {
		t.Fatal(err)
	}
	advance(40 * time.Minute)
	assertNoStall(t, f, b.ID, "a member's matrix run holds the host")
	if _, err := f.report(t, lead, nil); err != nil {
		t.Fatal(err)
	}
	advance(10 * time.Minute)
	assertNoStall(t, f, b.ID, "idle below the threshold since the wait ended")
	advance(25 * time.Minute)
	assertStall(t, f, b.ID, api.StallIdleEntry, a.ID, "tt team queue integrated", "tt team queue scope --task "+f.task.ID+" --entry "+a.ID)
}

// s2: a matrix wait the relay stopped updating counts only within the bound.
func TestQueueStallIdleEntryMatrixWaitBound(t *testing.T) {
	f, advance, a, b, lead, _ := idleTeam(t)
	if _, err := f.report(t, lead, &api.MatrixWait{Role: api.MatrixWaitWaiting, Position: 1, Length: 1, Since: f.s.now().Add(-(defaultQueueMatrixWaitBound - 40*time.Minute))}); err != nil {
		t.Fatal(err)
	}
	advance(35 * time.Minute)
	assertNoStall(t, f, b.ID, "a wait five minutes inside the bound")
	advance(10 * time.Minute)
	assertStall(t, f, b.ID, api.StallIdleEntry, a.ID)
}

// s4: the hub stores a matrix wait appearing, changing and ending as
// transitions, ignores a moved position, and refuses an invalid wait.
func TestQueueStallMatrixWaitReport(t *testing.T) {
	f, advance, _, _, _, _ := idleTeam(t)
	member := f.member(t, 0, "verifier-a") // no activity saved yet
	saved := func() (observed string, wait *api.MatrixWait) {
		t.Helper()
		var payload string
		err := f.s.db.QueryRow(`SELECT observed_at,payload FROM agent_activity WHERE agent_id=? AND run_id=?`, member.ID, member.RunID).Scan(&observed, &payload)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			t.Fatal(err)
		}
		var activity api.AgentActivity
		if err := json.Unmarshal([]byte(payload), &activity); err != nil {
			t.Fatal(err)
		}
		return observed, activity.MatrixWait
	}
	step := func(why string, wait *api.MatrixWait, stored bool) {
		t.Helper()
		advance(time.Minute)
		before, _ := saved()
		if _, err := f.report(t, member, wait); err != nil {
			t.Fatalf("%s: %v", why, err)
		}
		after, got := saved()
		if (after != before) != stored {
			t.Fatalf("%s: stored=%v, want %v", why, after != before, stored)
		}
		if stored && !api.SameMatrixWait(got, wait) {
			t.Fatalf("%s: saved wait %+v, want %+v", why, got, wait)
		}
	}
	since := f.s.now()
	step("idle", nil, true)
	step("idle with a wait", &api.MatrixWait{Role: api.MatrixWaitWaiting, Item: f.items[0].ID, Position: 3, Length: 3, Since: since}, true)
	step("only the position moved", &api.MatrixWait{Role: api.MatrixWaitWaiting, Item: f.items[0].ID, Position: 1, Length: 2, Since: since}, false)
	if _, got := saved(); got == nil || got.Position != 3 {
		t.Fatalf("a repeat replaced the saved wait: %+v", got)
	}
	step("the run got the host", &api.MatrixWait{Role: api.MatrixWaitRunning, Item: f.items[0].ID, Since: since.Add(time.Minute)}, true)
	step("idle again", nil, true)
	for name, wait := range map[string]*api.MatrixWait{
		"unknown role":          {Role: "queued", Position: 1, Length: 1, Since: since},
		"no since":              {Role: api.MatrixWaitWaiting, Position: 1, Length: 1},
		"waiting at position 0": {Role: api.MatrixWaitWaiting, Length: 1, Since: since},
		"position past length":  {Role: api.MatrixWaitWaiting, Position: 2, Length: 1, Since: since},
		"length over the cap":   {Role: api.MatrixWaitWaiting, Position: 1, Length: 1001, Since: since},
		"running with a place":  {Role: api.MatrixWaitRunning, Position: 1, Length: 1, Since: since},
		"item is not an id":     {Role: api.MatrixWaitRunning, Item: "release", Since: since},
	} {
		if _, err := f.report(t, member, wait); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// a6 (c3): no-handler only when no online, non-retired handler exists.
func TestQueueStallNoHandler(t *testing.T) {
	f := newChoresQueue(t, 2, 1, 0)
	a := f.add(t, 0, "src/a")
	assertNoStall(t, f, a.ID, "a handler is online")
	if _, err := f.s.db.Exec(`UPDATE agents SET status='retired' WHERE task_id=? AND role=?`, f.task.ID, api.AgentRoleDatabaseHandler); err != nil {
		t.Fatal(err)
	}
	assertStall(t, f, a.ID, api.StallNoHandler, "", "Set up database handler", "tt resume")
}

// a6 (c3): a failed entry halts a serial queue.
func TestQueueStallSerialHalted(t *testing.T) {
	f := newChoresQueue(t, 2, 1, 1)
	a := f.run(t, f.add(t, 0))
	b := f.add(t, 1)
	assertNoStall(t, f, b.ID, "serial queue with a working team")
	failed := f.fail(t, a)
	assertStall(t, f, b.ID, api.StallSerialHalted, failed.ID, "tt team queue release --task "+f.task.ID+" --entry "+failed.ID, "tt team queue integrated")
}

// a6 (c3): with limit 2 held by one failed and one idle entry, the queued
// entry shows a stall, not "All team slots are reserved"; slots full of
// working teams, an overlap with a working team, a host or disk block and
// handlers all leased by working teams are not stalls.
func TestQueueStallSlotsAndNonStalls(t *testing.T) {
	f := newChoresQueue(t, 3, 3, 2)
	f.s.queueIdleThreshold = 30 * time.Minute
	advance := f.clock(t)
	a := f.run(t, f.add(t, 0, "src/a"))
	b := f.run(t, f.add(t, 1, "src/b"))
	leadA, leadB := f.member(t, 0, "lead-a"), f.member(t, 1, "lead-b")
	f.activity(t, leadA, "working")
	f.activity(t, leadB, "working")
	c := f.add(t, 2, "src/c")
	advance(time.Hour)
	if got := listedEntry(t, f.s, f.task.ID, c.ID); got.Stall != nil || got.BlockReason != "All team slots are reserved" {
		t.Fatalf("slots full of working teams: %+v %q", got.Stall, got.BlockReason)
	}
	f.fail(t, a)
	f.activity(t, leadB, "idle")
	advance(time.Hour)
	got := listedEntry(t, f.s, f.task.ID, c.ID)
	if got.Stall == nil || got.Stall.BlockerEntryID != a.ID || got.Stall.Cause != api.StallFailedEntry || got.BlockReason == "All team slots are reserved" {
		t.Fatalf("slots held by stall blockers: %+v %q", got.Stall, got.BlockReason)
	}
	// A host or disk block is ordinary waiting.
	observeFixtureHost(t, f.s, f.task.ID, 1<<30, freeDiskMiB(1))
	assertNoStall(t, f, c.ID, "disk reserve")
	observeFixtureHost(t, f.s, f.task.ID, 0, freeDiskMiB(1<<20))
	assertStall(t, f, c.ID, api.StallFailedEntry, a.ID)
	_ = b
}

// a6 (c3): an overlap with a working team, and every handler leased by
// working teams, are not stalls.
func TestQueueStallNotForWorkingTeams(t *testing.T) {
	f := newChoresQueue(t, 3, 1, 0)
	advance := f.clock(t)
	a := f.run(t, f.add(t, 0, "src"))
	lead := f.member(t, 0, "lead-a")
	f.activity(t, lead, "working")
	b := f.add(t, 1, "src/b")
	c := f.add(t, 2, "docs")
	advance(time.Hour)
	assertNoStall(t, f, b.ID, "overlap with a working team")
	if got := listedEntry(t, f.s, f.task.ID, c.ID); got.Stall != nil || got.BlockReason != "No free database handler" {
		t.Fatalf("handlers leased by working teams: %+v %q", got.Stall, got.BlockReason)
	}
	_ = a
}

// a7 (c3): a stall posts exactly one Board NOTICE once its durable time has
// held past the grace period: no recipient, no escalation ref, no obligation.
// Retries, other entries behind the same blocker and a hub restart add
// nothing; a new blocker revision may post once more; a stale or cleared
// stall is refused.
func TestQueueStallNoticeOncePerStall(t *testing.T) {
	f := newChoresQueue(t, 3, 2, 0)
	ctx := context.Background()
	advance := f.clock(t)
	a := f.run(t, f.add(t, 0, "src"))
	f.member(t, 0, "member-a")
	b := f.add(t, 1, "src/b")
	c := f.add(t, 2, "src/c")
	failed := f.fail(t, a)
	messages := func() int {
		t.Helper()
		var n int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	notice := func(q api.TeamQueueEntry, id string) error {
		_, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: id, Operation: "stall_notice", EntryID: q.ID, ExpectedRevision: q.Revision})
		return err
	}
	stall := f.stall(t, b.ID)
	if stall == nil || stall.BlockerEntryID != failed.ID {
		t.Fatalf("stall %+v", stall)
	}
	id := stall.NoticeRequestID(b.ID)
	before := messages()
	advance(time.Minute)
	if err := notice(b, id); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "grace") {
		t.Fatalf("notice inside the grace period: %v", err)
	}
	if err := notice(b, "queue-stall-"+failed.ID+"-idle-entry-1"); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale notice: %v", err)
	}
	advance(5 * time.Minute)
	if err := notice(b, id); err != nil {
		t.Fatal(err)
	}
	if messages() != before+1 {
		t.Fatalf("notice posted %d messages", messages()-before)
	}
	var seq int64
	var from, to, raw string
	if err := f.s.db.QueryRow(`SELECT seq,from_node||'/'||from_user,to_agent,envelope FROM messages WHERE task_id=? ORDER BY seq DESC LIMIT 1`, f.task.ID).Scan(&seq, &from, &to, &raw); err != nil {
		t.Fatal(err)
	}
	if from != "team_queue/runner" || to != "" || !strings.Contains(raw, `"entry":"`+b.ID+`"`) || !strings.Contains(raw, `"blocker":"`+failed.ID+`"`) || !strings.Contains(raw, `"cause":"failed-entry"`) || !strings.Contains(raw, `"item":"`+b.ItemID+`"`) || strings.Contains(raw, "escalation") || !strings.Contains(raw, "Fix: ") {
		t.Fatalf("notice from=%s to=%q envelope=%s", from, to, raw)
	}
	var obligations int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM obligations WHERE message_seq=?`, seq).Scan(&obligations); err != nil || obligations != 0 {
		t.Fatalf("notice created %d obligations %v", obligations, err)
	}
	if err := notice(b, id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if cStall := f.stall(t, c.ID); cStall == nil || cStall.NoticeRequestID(c.ID) != id {
		t.Fatalf("C behind the same blocker %+v", cStall)
	}
	if err := notice(c, id); err != nil {
		t.Fatalf("second entry behind the same blocker: %v", err)
	}
	if messages() != before+1 {
		t.Fatalf("retries posted %d messages", messages()-before)
	}

	// A hub restart keeps the durable time and the saved notice.
	var path string
	if err := f.s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	now := f.s.now()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	reopened.now = func() time.Time { return now }
	f.s = reopened
	if again := f.stall(t, b.ID); again == nil || again.Since != stall.Since {
		t.Fatalf("restart moved the stall: %+v was %+v", again, stall)
	}
	if err := notice(b, id); err != nil || messages() != before+1 {
		t.Fatalf("notice after restart: %v, %d new messages", err, messages()-before)
	}
	advance = f.clock(t)

	// A new blocker revision is a new stall once it has held for the grace.
	narrowed, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "narrow-a", Operation: "scope", EntryID: failed.ID, ExpectedRevision: failed.Revision, Ownership: []string{"src/b"}})
	if err != nil {
		t.Fatal(err)
	}
	next := f.stall(t, b.ID)
	if next == nil || next.BlockerRevision != narrowed.Revision || next.NoticeRequestID(b.ID) == id {
		t.Fatalf("stall after the blocker changed %+v", next)
	}
	assertNoStall(t, f, c.ID, "C no longer overlaps the narrowed blocker")
	if err := notice(b, next.NoticeRequestID(b.ID)); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("new revision inside the grace period: %v", err)
	}
	advance(6 * time.Minute)
	if err := notice(b, next.NoticeRequestID(b.ID)); err != nil || messages() != before+2 {
		t.Fatalf("new revision notice: %v, %d new messages", err, messages()-before)
	}

	// Once the stall clears its notice is refused.
	if _, err := f.integrated(narrowed, "integrated-a", strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	assertNoStall(t, f, b.ID, "blocker integrated")
	if err := notice(b, next.NoticeRequestID(b.ID)+"-cleared"); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("cleared stall notice: %v", err)
	}
	if messages() != before+2 {
		t.Fatalf("cleared stall posted %d messages", messages()-before)
	}
}

// Review f0: after the owner's integration and the handler's plain done save,
// the recovery accept (and a done save carrying a tuple) is refused naming
// the integration, so no acceptance or second release job is recorded.
func TestOwnerIntegratedEntryTakesNoHandlerAcceptance(t *testing.T) {
	f := newChoresQueue(t, 1, 2, 0)
	ctx := context.Background()
	a := f.run(t, f.add(t, 0, "src/a"))
	commit := strings.Repeat("d", 40)
	a, err := f.integrated(a, "integrated-a", commit, "src/a/x.go")
	if err != nil {
		t.Fatal(err)
	}
	var leased api.Agent
	for _, h := range f.handlers {
		if h.ID == a.HandlerID {
			leased = h
		}
	}
	done := "done"
	item, err := f.s.GetWorkItem(ctx, f.task.ID, f.items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.CreateWorkItemUpdate(ctx, f.task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, Status: &done, AgentID: leased.ID, RunID: leased.RunID, RequestID: "plain"}, f.by); err != nil {
		t.Fatal(err)
	}
	if item, err = f.s.GetWorkItem(ctx, f.task.ID, f.items[0].ID); err != nil {
		t.Fatal(err)
	}
	acc := api.TeamIntegrationAcceptance{Repository: a.Repository, BaseCommit: a.BaseCommit, Worktree: "/worktrees/a", Branch: "feature/a", Commit: commit, ItemRevision: item.Revision, CompletionReport: item.CompletionReport, Evidence: "recovery accept"}
	_, err = f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "recover-accept", Operation: "accept", EntryID: a.ID, ExpectedRevision: a.Revision, HandlerAgentID: leased.ID, HandlerRunID: leased.RunID, Acceptance: &acc})
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "integrated by the owner at "+commit) {
		t.Fatalf("recovery accept on an owner-integrated entry: %v", err)
	}
	got, err := f.s.GetTeamQueueEntry(ctx, f.task.ID, a.ID)
	if err != nil || got.Acceptance != nil || got.Revision != a.Revision || got.OwnerIntegration == nil {
		t.Fatalf("entry after refused accept %+v %v", got, err)
	}
	var jobs int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM release_jobs WHERE task_id=? AND entry_id=?`, f.task.ID, a.ID).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("release jobs %d %v", jobs, err)
	}
}

// Review f1: an owner-paused project shows no stall and refuses a stall
// notice; resuming brings the stall back.
func TestQueueStallSkipsPausedProject(t *testing.T) {
	f := newChoresQueue(t, 2, 2, 0)
	ctx := context.Background()
	a := f.run(t, f.add(t, 0, "src"))
	f.member(t, 0, "member-a")
	b := f.add(t, 1, "src/b")
	failed := f.fail(t, a)
	stall := f.stall(t, b.ID)
	if stall == nil || stall.BlockerEntryID != failed.ID {
		t.Fatalf("active project stall %+v", stall)
	}
	id := stall.NoticeRequestID(b.ID)
	if _, err := f.s.db.Exec(`UPDATE tasks SET pause_state=? WHERE id=?`, api.ProjectPausePaused, f.task.ID); err != nil {
		t.Fatal(err)
	}
	assertNoStall(t, f, b.ID, "paused project")
	f.s.queueStallGrace = time.Nanosecond
	if _, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: id, Operation: "stall_notice", EntryID: b.ID, ExpectedRevision: b.Revision}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("stall notice in a paused project: %v", err)
	}
	var notices int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=? AND from_node='team_queue' AND from_user='runner' AND envelope LIKE '%"cause":%'`, f.task.ID).Scan(&notices); err != nil || notices != 0 {
		t.Fatalf("paused project posted %d stall notices %v", notices, err)
	}
	if _, err := f.s.db.Exec(`UPDATE tasks SET pause_state=? WHERE id=?`, api.ProjectPauseActive, f.task.ID); err != nil {
		t.Fatal(err)
	}
	if again := f.stall(t, b.ID); again == nil || again.NoticeRequestID(b.ID) != id {
		t.Fatalf("stall after resume %+v", again)
	}
}
