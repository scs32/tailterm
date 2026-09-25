package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestParallelQueueSkipsConflictAndLeasesDistinctHandlers(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	third, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Third item", Priority: "normal", RequestID: "third-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	items = append(items, third)
	orders = append(orders, contextLinkedMessage(t, s, task, third, "third bounded order", "third-order", nil))
	aux, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "aux-handler", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: "aux-handler"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(s.now()), aux.ID); err != nil {
		t.Fatal(err)
	}
	var entries []api.TeamQueueEntry
	for i, owned := range []string{"src/a", "src/a/child", "src/c"} {
		q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: []string{"add-a", "add-b", "add-c"}[i], Operation: "add", ItemID: items[i].ID, OrderMessageSeq: orders[i].Seq, Host: "mini", Cwd: []string{"/a", "/b", "/c"}[i], Repository: "repo", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ownership: []string{owned}})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, q)
	}
	claim := func(i int, key string) (api.TeamQueueEntry, error) {
		return s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: key, Operation: "claim", EntryID: entries[i].ID, ExpectedRevision: entries[i].Revision, Host: "mini"})
	}
	if _, err := claim(0, "serial-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := claim(2, "serial-c"); err == nil {
		t.Fatal("default limit 1 admitted concurrent team")
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "policy", Operation: "set_host_policy", Host: "mini", HostPolicyVersion: 1, HostPolicyExpires: s.now().Add(time.Hour).Format(time.RFC3339), HostMaxSessions: 100, HostMaxPolling: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "limit-two", Operation: "set_limit", Host: "mini", ConcurrencyLimit: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := claim(1, "conflicting-b"); err == nil {
		t.Fatal("conflicting entry crossed A")
	}
	var wg sync.WaitGroup
	results := make(chan api.TeamQueueEntry, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			q, err := claim(2, []string{"parallel-c-one", "parallel-c-two"}[n])
			if err == nil {
				results <- q
			}
		}(i)
	}
	wg.Wait()
	close(results)
	count := 0
	var c api.TeamQueueEntry
	for q := range results {
		count++
		c = q
	}
	if count != 1 {
		t.Fatalf("racing claims succeeded %d times", count)
	}
	a, err := s.GetTeamQueueEntry(ctx, task.ID, entries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.HandlerID == "" || c.HandlerID == "" || a.HandlerID == c.HandlerID || a.HandlerRunID == "" || c.HandlerRunID == "" {
		t.Fatalf("leases A=%s/%s C=%s/%s", a.HandlerID, a.HandlerRunID, c.HandlerID, c.HandlerRunID)
	}
	list, err := s.ListTeamQueue(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Entries[1].BlockedBy) != 1 || list.Entries[1].BlockedBy[0] != a.ID {
		t.Fatalf("B blocker %+v", list.Entries[1].BlockedBy)
	}
	failed, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "fail-a", Operation: "fail", EntryID: a.ID, ExpectedRevision: a.Revision, Failure: "synthetic A spawn failure"})
	if err != nil || failed.EscalationSeq == 0 {
		t.Fatalf("fail A: %+v %v", failed, err)
	}
	if _, err := claim(1, "b-before-release"); err == nil {
		t.Fatal("B crossed failed A reservation")
	}
	if still, err := s.GetTeamQueueEntry(ctx, task.ID, c.ID); err != nil || still.State != "launching" {
		t.Fatalf("C changed after A failure: %+v %v", still, err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "release-a", Operation: "release", EntryID: a.ID, ExpectedRevision: failed.Revision}); err != nil {
		t.Fatal(err)
	}
	b, err := claim(1, "b-after-release")
	if err != nil {
		t.Fatalf("B did not use released slot: %v", err)
	}
	if b.HandlerID != a.HandlerID || b.HandlerRunID != a.HandlerRunID || b.HandlerLeaseGeneration <= a.HandlerLeaseGeneration {
		t.Fatalf("handler lease generation did not advance: A=%+v B=%+v", a, b)
	}
	if _, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{RequestID: "stale-a-handler-role", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: items[0].ID, ItemRevision: items[0].Revision, Relationship: "primary"}}, WorkOrderMessage: &api.MessageReference{TaskID: task.ID, Seq: orders[0].Seq}, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: "role:database_handler", Subject: "Check stale item", Body: api.EnvelopeBody{Ask: "Check A after release."}}}, by); err == nil {
		t.Fatal("released A lease still routed a handler request")
	}
}

func TestParallelHandlerPoolReopens(t *testing.T) {
	s, task, _, _ := queueFixture(t)
	ctx := context.Background()
	if _, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "aux-handler", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: "aux"}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err := s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	agents, err := reopened.ListAgents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, agent := range agents {
		if agent.Role == api.AgentRoleDatabaseHandler {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("handlers after reopen=%d", count)
	}
}

func TestParallelQueueMigratesLegacySingletonReservations(t *testing.T) {
	s, task, _, _ := queueFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`DROP TABLE team_launch_reservations;
		CREATE TABLE team_launch_reservations(task_id TEXT PRIMARY KEY REFERENCES tasks(id),entry_id TEXT NOT NULL DEFAULT '',item_id TEXT NOT NULL,token TEXT NOT NULL,pause_generation INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN ('reserved','launching','running')),created_at TEXT NOT NULL);
		DROP INDEX team_queue_active;
		CREATE UNIQUE INDEX team_queue_active ON team_queue_entries(task_id) WHERE state IN ('launching','running');
		CREATE UNIQUE INDEX agents_database_handler ON agents(task_id) WHERE role='database_handler' AND status<>'closed';`); err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err := s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "aux-after-migration", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: "aux"}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatalf("legacy singleton index remained: %v", err)
	}
	rows, err := reopened.db.Query(`PRAGMA table_info('team_launch_reservations')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	entryPrimary := false
	for rows.Next() {
		var columnID, notNull, primary int
		var columnName, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&columnID, &columnName, &columnType, &notNull, &defaultValue, &primary); err != nil {
			t.Fatal(err)
		}
		if columnName == "entry_id" && primary > 0 {
			entryPrimary = true
		}
	}
	if err := rows.Err(); err != nil || !entryPrimary {
		t.Fatalf("legacy reservation key remained: entry primary=%v err=%v", entryPrimary, err)
	}
	list, err := reopened.ListTeamQueue(ctx, task.ID)
	if err != nil || list.ConcurrencyLimit != 1 {
		t.Fatalf("migration changed serial default: %+v %v", list, err)
	}
}

func TestParallelLimitRequiresFreshHostBudgetBeforeEffects(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	limit := func(key string, n int) error {
		_, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: key, Operation: "set_limit", ConcurrencyLimit: n, Host: "mini"})
		return err
	}
	if err := limit("missing-policy", 2); err == nil {
		t.Fatal("missing host policy admitted limit 2")
	}
	if err := limit("invalid-limit", 3); err == nil {
		t.Fatal("over ceiling admitted")
	}
	policy := func(key string, version int64, sessions, polling int, expiry time.Time) error {
		_, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: key, Operation: "set_host_policy", Host: "mini", HostPolicyVersion: version, HostPolicyExpires: expiry.Format(time.RFC3339), HostMaxSessions: sessions, HostMaxPolling: polling})
		return err
	}
	if err := policy("stale", 1, 20, 1, s.now().Add(-time.Minute)); err == nil {
		t.Fatal("stale policy stored")
	}
	if err := policy("small", 1, 4, 1, s.now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := limit("too-small", 2); err == nil {
		t.Fatal("session budget crossed")
	}
	if err := policy("enough", 2, 20, 1, s.now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "legacy-unscoped", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := limit("unverified-queue", 2); err == nil {
		t.Fatal("unverified legacy queue admitted parallel execution")
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "remove-legacy", Operation: "remove", EntryID: legacy.ID, ExpectedRevision: legacy.Revision}); err != nil {
		t.Fatal(err)
	}
	if err := limit("admitted", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "unverified-parallel", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/legacy"}); err == nil {
		t.Fatal("parallel mode accepted item without repository/base")
	}
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "other project"}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAgent(ctx, other.ID, api.AddAgentRequest{Name: "other-0", Host: "mini", Session: "other-0"}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := limit("other-project-polling", 2); err == nil {
		t.Fatal("other project polling budget was omitted")
	}
	if err := policy("two-polling-projects", 3, 20, 2, s.now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 16; i++ {
		if _, err := s.AddAgent(ctx, other.ID, api.AddAgentRequest{Name: fmt.Sprintf("other-%d", i), Host: "mini", Session: fmt.Sprintf("other-%d", i)}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := limit("other-project-capacity", 2); err == nil {
		t.Fatal("other project sessions were omitted from host budget")
	}
	list, err := s.ListTeamQueue(ctx, task.ID)
	if err != nil || list.ConcurrencyLimit != 2 {
		t.Fatalf("saved limit %+v %v", list, err)
	}
}

func TestParallelPolicyExpiryStopsFrozenMemberAttempt(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	now := s.now()
	s.now = func() time.Time { return now }
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "short-policy", Operation: "set_host_policy", Host: "mini", HostPolicyVersion: 1, HostPolicyExpires: now.Add(time.Hour).Format(time.RFC3339), HostMaxSessions: 20, HostMaxPolling: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "parallel-limit", Operation: "set_limit", Host: "mini", ConcurrencyLimit: 2}); err != nil {
		t.Fatal(err)
	}
	q, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "parallel-add", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/item-a", Repository: "repo", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Ownership: []string{"src/a"}})
	if err != nil {
		t.Fatal(err)
	}
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "parallel-claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := json.Marshal(map[string]any{"task": task.ID, "item": q.ItemID, "revision": q.ItemRevision, "order": q.OrderMessageSeq, "handlerId": q.HandlerID, "handlerRunId": q.HandlerRunID, "handlerLeaseGeneration": q.HandlerLeaseGeneration, "context": map[string]any{"version": 1}, "members": []any{map[string]any{"state": "unstarted", "runId": api.NewID("run"), "fields": map[string]any{"agentId": api.NewID("agt"), "name": "lead", "cwd": q.Cwd}}}})
	q, err = s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "parallel-freeze", Operation: "freeze", EntryID: q.ID, ExpectedRevision: q.Revision, LaunchJSON: plan})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "expired-attempt", Operation: "attempt", EntryID: q.ID, ExpectedRevision: q.Revision, MemberIndex: 0}); err == nil {
		t.Fatal("expired host policy allowed new spawn attempt")
	}
	current, err := s.GetTeamQueueEntry(ctx, task.ID, q.ID)
	if err != nil || current.Revision != q.Revision || string(current.LaunchJSON) != string(q.LaunchJSON) {
		t.Fatalf("refused attempt changed frozen entry: %+v %v", current, err)
	}
}
