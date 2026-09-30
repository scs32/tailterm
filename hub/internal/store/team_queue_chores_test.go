package store

import (
	"context"
	"database/sql"
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
	if _, err := f.s.db.Exec(`DELETE FROM release_jobs WHERE id='rel_fixture'`); err != nil {
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
