package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// uncappedFixture extends queueFixture to n items and h online database
// handlers on host mini, with a fresh host policy and census.
func uncappedFixture(t *testing.T, n, h int) (*Store, api.Task, []api.WorkItem, []api.Message, []api.Agent) {
	t.Helper()
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	for len(items) < n {
		item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Queued work", Priority: "normal", RequestID: api.NewID("req")}, by)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
		orders = append(orders, contextLinkedMessage(t, s, task, item, "bounded order", api.NewID("req"), nil))
	}
	for i := 1; i < h; i++ {
		a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: fmt.Sprintf("handler-%d", i), Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: fmt.Sprintf("handler-%d", i)}, by)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(s.now()), a.ID); err != nil {
			t.Fatal(err)
		}
	}
	agents, err := s.ListAgents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var handlers []api.Agent
	for _, a := range agents {
		if a.Role == api.AgentRoleDatabaseHandler {
			handlers = append(handlers, a)
		}
	}
	observeFixtureHost(t, s, task.ID, 0, freeDiskMiB(1<<20))
	return s, task, items, orders, handlers
}

// observeFixtureHost saves a new host policy version with the given disk
// reserve, then a fresh census reporting free disk (nil: not observed).
func observeFixtureHost(t *testing.T, s *Store, taskID string, reserve int64, free *int64) {
	t.Helper()
	ctx := context.Background()
	var version int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version),0)+1 FROM team_host_policies WHERE host='mini'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, taskID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "set_host_policy", Host: "mini", HostPolicyVersion: version, HostPolicyExpires: s.now().Add(time.Hour).Format(time.RFC3339), HostMaxSessions: 100, HostMaxPolling: 10, LimiterDomain: "https://fixture.invalid", HostMaxRelayBindings: 100, HostMaxRequestsPerMinute: 100000, HostMaxBurst: 10000, HostHeadroomPercent: 20, HostMinFreeDiskMiB: reserve}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, taskID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "observe_host", Host: "mini", HostUsage: &api.TeamHostUsage{Host: "mini", LimiterDomain: "https://fixture.invalid", PolicyVersion: version, ObservedAt: s.now().UTC().Format(time.RFC3339Nano), RelayBindings: 1, Complete: true, SourceDigest: strings.Repeat("a", 64), FreeDiskMiB: free}}); err != nil {
		t.Fatal(err)
	}
}

func setQueueLimit(t *testing.T, s *Store, taskID string, limit int) {
	t.Helper()
	if _, err := s.TeamQueueAction(context.Background(), taskID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "set_limit", Host: "mini", ConcurrencyLimit: limit}); err != nil {
		t.Fatalf("set limit %d: %v", limit, err)
	}
}

func addScopedEntry(t *testing.T, s *Store, task api.Task, item api.WorkItem, order api.Message, owns ...string) api.TeamQueueEntry {
	t.Helper()
	q, err := s.TeamQueueAction(context.Background(), task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "mini", Cwd: "/worktrees/" + item.ID, Repository: "repo", BaseCommit: strings.Repeat("a", 40), Ownership: owns})
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func claimEntry(s *Store, task api.Task, q api.TeamQueueEntry) (api.TeamQueueEntry, error) {
	return s.TeamQueueAction(context.Background(), task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "mini"})
}

func listedEntry(t *testing.T, s *Store, taskID, id string) api.TeamQueueEntry {
	t.Helper()
	list, err := s.ListTeamQueue(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range list.Entries {
		if q.ID == id {
			return q
		}
	}
	t.Fatalf("entry %s not listed", id)
	return api.TeamQueueEntry{}
}

// a1: with no fixed cap, disjoint entries run until the handlers are all
// leased; a positive limit still caps slots.
func TestUncappedQueueAdmitsUntilHandlersRunOut(t *testing.T) {
	s, task, items, orders, handlers := uncappedFixture(t, 4, 3)
	ctx := context.Background()
	setQueueLimit(t, s, task.ID, 0)
	if list, err := s.ListTeamQueue(ctx, task.ID); err != nil || list.ConcurrencyLimit != 0 {
		t.Fatalf("no-cap limit %+v %v", list.ConcurrencyLimit, err)
	}
	var entries []api.TeamQueueEntry
	for i := range items {
		entries = append(entries, addScopedEntry(t, s, task, items[i], orders[i], fmt.Sprintf("src/%d", i)))
	}
	leased := map[string]bool{}
	for i := 0; i < 3; i++ {
		q, err := claimEntry(s, task, entries[i])
		if err != nil {
			t.Fatalf("claim %d under no cap: %v", i, err)
		}
		if leased[q.HandlerID] {
			t.Fatalf("handler %s leased twice", q.HandlerID)
		}
		leased[q.HandlerID] = true
	}
	if len(leased) != len(handlers) {
		t.Fatalf("leased %d of %d handlers", len(leased), len(handlers))
	}
	if got := listedEntry(t, s, task.ID, entries[3].ID); got.BlockReason != "No free database handler" || len(got.BlockedBy) != 0 {
		t.Fatalf("fourth entry reason %q blocked by %v", got.BlockReason, got.BlockedBy)
	}
	if _, err := claimEntry(s, task, entries[3]); err == nil || !strings.Contains(err.Error(), "no available database handler lease") {
		t.Fatalf("fourth claim: %v", err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "lower-below-active", Operation: "set_limit", Host: "mini", ConcurrencyLimit: 2}); err == nil || !strings.Contains(err.Error(), "exceed requested limit") {
		t.Fatalf("lowering below active entries: %v", err)
	}
	setQueueLimit(t, s, task.ID, 5)
	if list, err := s.ListTeamQueue(ctx, task.ID); err != nil || list.ConcurrencyLimit != 5 {
		t.Fatalf("ceiling %+v %v", list.ConcurrencyLimit, err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "negative-limit", Operation: "set_limit", Host: "mini", ConcurrencyLimit: -1}); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("negative limit: %v", err)
	}
}

func TestQueueLimitTwoCapsSlotsWithFreeHandlers(t *testing.T) {
	s, task, items, orders, _ := uncappedFixture(t, 3, 3)
	setQueueLimit(t, s, task.ID, 2)
	var entries []api.TeamQueueEntry
	for i := range items {
		entries = append(entries, addScopedEntry(t, s, task, items[i], orders[i], fmt.Sprintf("src/%d", i)))
	}
	for i := 0; i < 2; i++ {
		if _, err := claimEntry(s, task, entries[i]); err != nil {
			t.Fatal(err)
		}
	}
	if got := listedEntry(t, s, task.ID, entries[2].ID); got.BlockReason != "All team slots are reserved" {
		t.Fatalf("third entry reason %q", got.BlockReason)
	}
	if _, err := claimEntry(s, task, entries[2]); err == nil || !strings.Contains(err.Error(), "all team slots are reserved") {
		t.Fatalf("third claim under limit 2: %v", err)
	}
}

func TestUncappedQueueListNamesUnscopedEntry(t *testing.T) {
	s, task, items, orders, _ := uncappedFixture(t, 2, 2)
	setQueueLimit(t, s, task.ID, 0)
	a := addScopedEntry(t, s, task, items[0], orders[0], "src/a")
	// A parallel add must now declare ownership or be serial; seed the legacy
	// unscoped row this display covers.
	b, err := s.TeamQueueAction(context.Background(), task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "add", ItemID: items[1].ID, OrderMessageSeq: orders[1].Seq, Host: "mini", Cwd: "/worktrees/" + items[1].ID, Repository: "repo", BaseCommit: strings.Repeat("a", 40), Serial: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE team_queue_entries SET serial=0 WHERE id=?`, b.ID); err != nil {
		t.Fatal(err)
	}
	if got := listedEntry(t, s, task.ID, b.ID); got.BlockReason != "" {
		t.Fatalf("unscoped entry with nothing active: %q", got.BlockReason)
	}
	if _, err := claimEntry(s, task, a); err != nil {
		t.Fatal(err)
	}
	got := listedEntry(t, s, task.ID, b.ID)
	if got.BlockReason != "Unscoped: declare ownership to run beside other teams" || len(got.BlockedBy) != 1 || got.BlockedBy[0] != a.ID {
		t.Fatalf("unscoped reason %q blocked by %v", got.BlockReason, got.BlockedBy)
	}
	if _, err := claimEntry(s, task, b); err == nil {
		t.Fatal("unscoped entry ran beside an active team")
	}
}

// a3: the disk reserve gates new parallel admission, never a launch already
// under way, and never a serial project.
func TestQueueDiskReserveGatesAdmissionOnly(t *testing.T) {
	s, task, items, orders, _ := uncappedFixture(t, 2, 2)
	ctx := context.Background()
	setQueueLimit(t, s, task.ID, 0)
	a := addScopedEntry(t, s, task, items[0], orders[0], "src/a")
	b := addScopedEntry(t, s, task, items[1], orders[1], "src/b")

	observeFixtureHost(t, s, task.ID, 0, freeDiskMiB(4000))
	const defaultBelow = "host free disk 4000 MiB is below the 8192 MiB reserve"
	if _, err := claimEntry(s, task, a); err == nil || !strings.Contains(err.Error(), defaultBelow) {
		t.Fatalf("claim below default reserve: %v", err)
	}
	if got := listedEntry(t, s, task.ID, a.ID); !strings.Contains(got.BlockReason, defaultBelow) {
		t.Fatalf("list reason %q", got.BlockReason)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "raise-low-disk", Operation: "set_limit", Host: "mini", ConcurrencyLimit: 3}); err == nil || !strings.Contains(err.Error(), defaultBelow) {
		t.Fatalf("limit change below reserve: %v", err)
	}

	observeFixtureHost(t, s, task.ID, 5000, freeDiskMiB(4000))
	const policyBelow = "host free disk 4000 MiB is below the 5000 MiB reserve"
	if _, err := claimEntry(s, task, a); err == nil || !strings.Contains(err.Error(), policyBelow) {
		t.Fatalf("claim below policy reserve: %v", err)
	}

	observeFixtureHost(t, s, task.ID, 5000, nil)
	if _, err := claimEntry(s, task, a); err == nil || !strings.Contains(err.Error(), "host free disk is not observed") {
		t.Fatalf("claim without disk observation: %v", err)
	}
	if got := listedEntry(t, s, task.ID, a.ID); !strings.Contains(got.BlockReason, "host free disk is not observed") {
		t.Fatalf("list reason %q", got.BlockReason)
	}

	observeFixtureHost(t, s, task.ID, 1000, freeDiskMiB(4000))
	claimed, err := claimEntry(s, task, a)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := json.Marshal(map[string]any{"task": task.ID, "item": claimed.ItemID, "revision": claimed.ItemRevision, "order": claimed.OrderMessageSeq, "handlerId": claimed.HandlerID, "handlerRunId": claimed.HandlerRunID, "handlerLeaseGeneration": claimed.HandlerLeaseGeneration, "context": map[string]any{"version": 1}, "members": []any{map[string]any{"state": "unstarted", "runId": api.NewID("run"), "fields": map[string]any{"agentId": api.NewID("agt"), "name": "lead", "cwd": claimed.Cwd}}}})
	frozen, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "freeze-a", Operation: "freeze", EntryID: claimed.ID, ExpectedRevision: claimed.Revision, LaunchJSON: plan})
	if err != nil {
		t.Fatal(err)
	}
	observeFixtureHost(t, s, task.ID, 1000, freeDiskMiB(10))
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "attempt-a-low-disk", Operation: "attempt", EntryID: frozen.ID, ExpectedRevision: frozen.Revision, MemberIndex: 0}); err != nil {
		t.Fatalf("admitted launch stopped by low disk: %v", err)
	}
	if _, err := claimEntry(s, task, b); err == nil || !strings.Contains(err.Error(), "host free disk 10 MiB is below the 1000 MiB reserve") {
		t.Fatalf("second claim with low disk: %v", err)
	}

	// A serial project on the same low-disk host still launches.
	serial, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Serial project"}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	by := api.Caller{Node: "fixture", User: "owner"}
	item, err := s.CreateWorkItem(ctx, serial.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Serial work", Priority: "normal", RequestID: "serial-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	order := contextLinkedMessage(t, s, serial, item, "serial order", "serial-order", nil)
	handler, err := s.AddAgent(ctx, serial.ID, api.AddAgentRequest{Name: "serial-handler", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: "serial-handler"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(s.now()), handler.ID); err != nil {
		t.Fatal(err)
	}
	q, err := s.TeamQueueAction(ctx, serial.ID, api.TeamQueueRequest{RequestID: "serial-add", Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "mini", Cwd: "/serial"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimEntry(s, serial, q); err != nil {
		t.Fatalf("serial project stopped by the parallel disk reserve: %v", err)
	}
}

// a2: the old 1..2 settings table is rebuilt in place with its rows kept;
// a fresh database gets the wide check directly.
func TestTeamQueueSettingsMigrationWidensLimit(t *testing.T) {
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var tasks []api.Task
	for i := 0; i < 3; i++ {
		task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: fmt.Sprintf("Project %d", i)}, by)
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	if _, err := s.db.Exec(`DROP TABLE team_queue_settings;
		CREATE TABLE team_queue_settings(task_id TEXT PRIMARY KEY REFERENCES tasks(id), concurrency_limit INTEGER NOT NULL DEFAULT 1 CHECK(concurrency_limit BETWEEN 1 AND 2));
		INSERT INTO team_queue_settings(task_id,concurrency_limit) VALUES(?,1),(?,2);`, tasks[0].ID, tasks[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE team_queue_settings SET concurrency_limit=0 WHERE task_id=?`, tasks[0].ID); err == nil {
		t.Fatal("old table accepted 0")
	}
	s.Close()
	check := func(s *Store) string {
		t.Helper()
		var schema string
		if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='team_queue_settings'`).Scan(&schema); err != nil {
			t.Fatal(err)
		}
		var integrity string
		if err := s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("integrity %q %v", integrity, err)
		}
		rows, err := s.db.Query(`PRAGMA foreign_key_check`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if rows.Next() {
			t.Fatal("foreign key violation after migration")
		}
		return schema
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	schema := check(s)
	if strings.Contains(schema, "BETWEEN 1 AND 2") || !strings.Contains(schema, "concurrency_limit >= 0") {
		t.Fatalf("migrated schema %s", schema)
	}
	for i, want := range []int{1, 2, 1} {
		if got, err := queueConcurrencyLimit(ctx, s.db, tasks[i].ID); err != nil || got != want {
			t.Fatalf("project %d limit %d %v, want %d", i, got, err, want)
		}
	}
	if _, err := s.db.Exec(`UPDATE team_queue_settings SET concurrency_limit=0 WHERE task_id=?`, tasks[0].ID); err != nil {
		t.Fatalf("migrated table refused 0: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE team_queue_settings SET concurrency_limit=7 WHERE task_id=?`, tasks[1].ID); err != nil {
		t.Fatalf("migrated table refused 7: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE team_queue_settings SET concurrency_limit=-1 WHERE task_id=?`, tasks[1].ID); err == nil {
		t.Fatal("migrated table accepted -1")
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if again := check(s); again != schema {
		t.Fatalf("second migration changed schema:\n%s\n%s", schema, again)
	}
	for i, want := range []int{0, 7, 1} {
		if got, err := queueConcurrencyLimit(ctx, s.db, tasks[i].ID); err != nil || got != want {
			t.Fatalf("project %d limit %d %v after rerun, want %d", i, got, err, want)
		}
	}
	s.Close()

	fresh, err := Open(filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if schema := check(fresh); strings.Contains(schema, "BETWEEN") || !strings.Contains(schema, "concurrency_limit >= 0") {
		t.Fatalf("fresh schema %s", schema)
	}
	task, err := fresh.CreateTask(ctx, api.CreateTaskRequest{Name: "Fresh project"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.db.Exec(`INSERT INTO team_queue_settings(task_id,concurrency_limit) VALUES(?,0)`, task.ID); err != nil {
		t.Fatalf("fresh table refused 0: %v", err)
	}
}

// a6: scope authority by exact run, overlap refusal for admitted entries,
// unblocking by narrowing, and retry replay.
func TestQueueScopeAuthorityOverlapAndUnblock(t *testing.T) {
	s, task, items, orders, handlers := uncappedFixture(t, 3, 3)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	setQueueLimit(t, s, task.ID, 0)
	a := addScopedEntry(t, s, task, items[0], orders[0], "src/a")
	b := addScopedEntry(t, s, task, items[1], orders[1], "src/a/child")
	c, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "add", ItemID: items[2].ID, OrderMessageSeq: orders[2].Seq, Host: "mini", Cwd: "/worktrees/" + items[2].ID, Repository: "repo", BaseCommit: strings.Repeat("a", 40), Serial: true})
	if err != nil {
		t.Fatal(err)
	}
	scope := func(key string, q api.TeamQueueEntry, owns []string, mutate func(*api.TeamQueueRequest)) (api.TeamQueueEntry, error) {
		req := api.TeamQueueRequest{RequestID: key, Operation: "scope", EntryID: q.ID, ExpectedRevision: q.Revision, Ownership: owns}
		if mutate != nil {
			mutate(&req)
		}
		return s.TeamQueueAction(ctx, task.ID, req)
	}
	asHandler := func(h api.Agent) func(*api.TeamQueueRequest) {
		return func(r *api.TeamQueueRequest) { r.HandlerAgentID, r.HandlerRunID = h.ID, h.RunID }
	}

	// The owner scopes an unscoped queued entry; a retry replays it.
	scoped, err := scope("owner-scope-c", c, []string{"src/c"}, nil)
	if err != nil || scoped.Revision != c.Revision+1 || len(scoped.Ownership) != 1 || scoped.Ownership[0] != "src/c" {
		t.Fatalf("owner scope %+v %v", scoped, err)
	}
	if replay, err := scope("owner-scope-c", c, []string{"src/c"}, nil); err != nil || replay.Revision != scoped.Revision {
		t.Fatalf("retry %+v %v", replay, err)
	}
	c = scoped
	if _, err := scope("empty-scope", c, nil, nil); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("empty ownership: %v", err)
	}
	if _, err := scope("bad-path", c, []string{"../escape"}, nil); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("traversal: %v", err)
	}

	// Any available handler scopes a queued entry, but only by its exact run.
	c, err = scope("handler-scope-c", c, []string{"src/c", "docs/c.md"}, asHandler(handlers[2]))
	if err != nil {
		t.Fatalf("handler scoping a queued entry: %v", err)
	}
	stale := handlers[2]
	stale.RunID = api.NewID("run")
	if _, err := scope("stale-handler", c, []string{"src/c"}, asHandler(stale)); err == nil {
		t.Fatal("mismatched handler run scoped an entry")
	}

	a, err = claimEntry(s, task, a)
	if err != nil {
		t.Fatal(err)
	}
	c, err = claimEntry(s, task, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := listedEntry(t, s, task.ID, b.ID); len(got.BlockedBy) != 1 || got.BlockedBy[0] != a.ID {
		t.Fatalf("B blocked by %v", got.BlockedBy)
	}

	// An admitted entry takes only its leased handler or its item lead.
	var other api.Agent
	for _, h := range handlers {
		if h.ID != a.HandlerID {
			other = h
			break
		}
	}
	if _, err := scope("other-handler-a", a, []string{"src/a/other"}, asHandler(other)); err == nil {
		t.Fatal("unleased handler scoped an admitted entry")
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead-a", AgentID: api.NewID("agt"), Host: "mini", Session: "lead-a"}, by)
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "member-a", AgentID: api.NewID("agt"), Host: "mini", Session: "member-a"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'launching')`, task.ID, a.ItemID, lead.ID, lead.RunID); err != nil {
		t.Fatal(err)
	}
	asLead := func(agent api.Agent) func(*api.TeamQueueRequest) {
		return func(r *api.TeamQueueRequest) { r.LeadAgentID, r.LeadRunID = agent.ID, agent.RunID }
	}
	if _, err := scope("member-a", a, []string{"src/a/other"}, asLead(member)); err == nil {
		t.Fatal("non-lead member scoped its entry")
	}
	staleLead := lead
	staleLead.RunID = api.NewID("run")
	if _, err := scope("stale-lead", a, []string{"src/a/other"}, asLead(staleLead)); err == nil {
		t.Fatal("mismatched lead run scoped its entry")
	}
	if _, err := scope("both-roles", a, []string{"src/a/other"}, func(r *api.TeamQueueRequest) {
		asLead(lead)(r)
		asHandler(handlers[0])(r)
	}); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("handler and lead together: %v", err)
	}

	// Widening into another active entry's paths names that entry.
	if _, err := scope("widen-a", a, []string{"src/a", "src/c/deep"}, asLead(lead)); err == nil || !strings.Contains(err.Error(), "ownership overlaps active entry "+c.ID) {
		t.Fatalf("widening into C: %v", err)
	}
	var leased api.Agent
	for _, h := range handlers {
		if h.ID == a.HandlerID {
			leased = h
		}
	}
	if _, err := scope("widen-a-handler", a, []string{"src/c"}, asHandler(leased)); err == nil || !strings.Contains(err.Error(), c.ID) {
		t.Fatalf("leased handler widening into C: %v", err)
	}

	// Narrowing A unblocks B, which then claims the freed paths.
	a, err = scope("narrow-a", a, []string{"src/a/other"}, asLead(lead))
	if err != nil {
		t.Fatalf("lead narrowing its entry: %v", err)
	}
	if got := listedEntry(t, s, task.ID, b.ID); len(got.BlockedBy) != 0 {
		t.Fatalf("B still blocked by %v", got.BlockedBy)
	}
	if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE role=?`, ts(s.now()), api.AgentRoleDatabaseHandler); err != nil {
		t.Fatal(err)
	}
	extra, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "handler-extra", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: "handler-extra"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(s.now()), extra.ID); err != nil {
		t.Fatal(err)
	}
	b = listedEntry(t, s, task.ID, b.ID)
	if _, err := claimEntry(s, task, b); err != nil {
		t.Fatalf("B after A narrowed: %v", err)
	}
	if _, err := scope("leased-handler-a", a, []string{"src/a/other", "src/a/more"}, asHandler(leased)); err != nil {
		t.Fatalf("leased handler scoping its entry: %v", err)
	}
}

// a8 (hub side): a failed parallel entry names its live item-bound runs.
func TestParallelFailedEntryListsLiveRuns(t *testing.T) {
	s, task, items, orders, _ := uncappedFixture(t, 1, 1)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	setQueueLimit(t, s, task.ID, 0)
	q, err := claimEntry(s, task, addScopedEntry(t, s, task, items[0], orders[0], "src/a"))
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "fail-a", Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: "owner integrated abc1234"})
	if err != nil {
		t.Fatal(err)
	}
	if got := listedEntry(t, s, task.ID, failed.ID); got.BlockReason != "Failed; the runner releases it on its next pass" {
		t.Fatalf("releasable reason %q", got.BlockReason)
	}
	for i := 0; i < 2; i++ {
		a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: fmt.Sprintf("member-%d", i), AgentID: api.NewID("agt"), Host: "mini", Session: fmt.Sprintf("member-%d", i)}, by)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,?,0,?,?,?)`, a.ID, a.RunID, task.ID, items[0].ID, task.ID, orders[0].Seq, "digest", []byte("{}"), ts(s.now())); err != nil {
			t.Fatal(err)
		}
	}
	if got := listedEntry(t, s, task.ID, failed.ID); got.BlockReason != "Failed; 2 item-bound runs are still live or uncleaned" {
		t.Fatalf("live-run reason %q", got.BlockReason)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "release-live", Operation: "release", EntryID: failed.ID, ExpectedRevision: failed.Revision}); err == nil {
		t.Fatal("release crossed live item runs")
	}
}

// a4: limit 0 is parallel for item authority, role:lead resolution and
// escalation routing, exactly like limit 2; limit 1 keeps the project lead.
func TestNoCapLimitUsesItemScopedAuthority(t *testing.T) {
	for _, limit := range []int{1, 2, 0} {
		t.Run(fmt.Sprintf("limit-%d", limit), func(t *testing.T) {
			s, task, items, orders := queueFixture(t)
			ctx := context.Background()
			by := api.Caller{Node: "fixture", User: "owner"}
			if _, err := s.db.Exec(`INSERT INTO team_queue_settings(task_id,concurrency_limit) VALUES(?,?)`, task.ID, limit); err != nil {
				t.Fatal(err)
			}
			lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "project-lead", AgentID: api.NewID("agt"), Host: "mini", Session: "project-lead"}, by)
			if err != nil {
				t.Fatal(err)
			}
			name := lead.Name
			if _, err := s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &name}, by); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(s.now()), lead.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,?,0,?,?,?)`, lead.ID, lead.RunID, task.ID, items[0].ID, task.ID, orders[0].Seq, "digest", []byte("{}"), ts(s.now())); err != nil {
				t.Fatal(err)
			}
			parallel := limit != 1

			// Item authority: a bound non-lead may not assign in parallel.
			_, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{RequestID: "assign", AgentID: lead.ID, RunID: lead.RunID, WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: items[0].ID, ItemRevision: items[0].Revision, Relationship: "primary"}}, WorkOrderMessage: &api.MessageReference{TaskID: task.ID, Seq: orders[0].Seq}, Envelope: &api.Envelope{Kind: api.EnvelopeKindAssign, Subject: "Build the change for item A", Body: api.EnvelopeBody{Objective: "Build it.", Owns: []string{"src/a"}, Acceptance: map[string]string{"a1": "tests pass"}}}}, by)
			if refused := err != nil && strings.Contains(err.Error(), "only the exact current item lead may assign"); refused != parallel {
				t.Fatalf("item authority refusal=%v under limit %d: %v", refused, limit, err)
			}

			// role:lead needs an exact item link in parallel mode.
			current, err := s.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := resolveRole(ctx, tx, current, api.RoleLead, "")
			tx.Rollback()
			if parallel && (err == nil || !strings.Contains(err.Error(), "needs an exact item link in parallel mode")) {
				t.Fatalf("role:lead under limit %d resolved %s: %v", limit, resolved.ID, err)
			}
			if !parallel && (err != nil || resolved.ID != lead.ID) {
				t.Fatalf("serial role:lead %s: %v", resolved.ID, err)
			}

			// Escalation: an item-linked overdue obligation without an
			// item lead goes to the owner in parallel, to the lead serially.
			worker, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", AgentID: api.NewID("agt"), Host: "mini", Session: "worker"}, by)
			if err != nil {
				t.Fatal(err)
			}
			message, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{RequestID: "overdue", To: worker.ID, WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: items[1].ID, ItemRevision: items[1].Revision, Relationship: "primary"}}, WorkOrderMessage: &api.MessageReference{TaskID: task.ID, Seq: orders[1].Seq}, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: worker.Name, Subject: "Check item B", Body: api.EnvelopeBody{Ask: "Check B."}}}, by)
			if err != nil {
				t.Fatal(err)
			}
			obligations, err := s.BrokerOpenObligations(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, o := range obligations {
				if o.MessageSeq != message.Seq {
					continue
				}
				found = true
				if err := s.BrokerEscalate(ctx, o, 1, "synthetic overdue", s.now()); err != nil {
					t.Fatal(err)
				}
			}
			if !found {
				t.Fatal("overdue obligation was not recorded")
			}
			var escalation int
			if err := s.db.QueryRow(`SELECT escalation FROM obligations WHERE message_seq=? AND task_id=?`, message.Seq, task.ID).Scan(&escalation); err != nil && !errors.Is(err, sql.ErrNoRows) {
				t.Fatal(err)
			}
			if want := map[bool]int{true: 2, false: 1}[parallel]; escalation != want {
				t.Fatalf("escalation level %d under limit %d, want %d", escalation, limit, want)
			}
		})
	}
}
