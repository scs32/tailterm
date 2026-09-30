package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
