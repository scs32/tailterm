package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/scs32/tailterm/hub/internal/api"
)

// TestMigrateAddsTeamRoleToExistingBindingsTable is the exact reproduction
// independent review #2300/#2771 finding 1 described: on a database whose
// agent_work_item_bindings table predates the team_role column, the
// CREATE INDEX referencing that column must not run before the column is
// added, or SQLite aborts the whole migration with "no such column:
// team_role" before ever reaching the ALTER TABLE repair.
func TestMigrateAddsTeamRoleToExistingBindingsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	// The exact pre-team_role shape of agent_work_item_bindings, as it
	// existed in production before this correction.
	if _, err := db.Exec(`
CREATE TABLE agent_work_item_bindings (
  agent_id TEXT NOT NULL REFERENCES agents(id),
  run_id TEXT NOT NULL,
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  item_revision INTEGER NOT NULL CHECK(item_revision > 0),
  work_order_task_id TEXT NOT NULL,
  work_order_message_seq INTEGER NOT NULL CHECK(work_order_message_seq > 0),
  context_through_message_seq INTEGER NOT NULL CHECK(context_through_message_seq >= 0),
  replaces_agent_id TEXT REFERENCES agents(id),
  context_digest TEXT NOT NULL,
  context_json BLOB NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(agent_id,run_id)
);
CREATE INDEX agent_work_item_bindings_item ON agent_work_item_bindings(item_task_id,item_id,created_at);
`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migration must succeed against a pre-team_role bindings table, not abort: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('agent_work_item_bindings') WHERE name='team_role'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("team_role column was not added by migration")
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='agent_work_item_bindings_item_team_role'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("agent_work_item_bindings_item_team_role index was not created")
	}
	// The migrated database must actually be usable afterward: a fresh
	// Store.Open (base schema is a no-op here, migrate runs again
	// idempotently) can admit a real team-role-classified binding.
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopening the migrated legacy database failed: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Post-migration", Orchestrator: "lead", AllowAgentSpawn: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Post-migration item", AgentID: lead.ID, RequestID: "post-migration-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	order, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{
		Text: "order", RequestID: "post-migration-order",
		WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}},
	}, by)
	if err != nil {
		t.Fatal(err)
	}
	orderRef := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	if _, err := s.db.Exec(`INSERT INTO work_order_scope_confirmations(task_id,item_id,item_revision,scope_revision,order_seq,request_id,payload_hash,agent_id,run_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		task.ID, item.ID, item.Revision, item.ScopeRevision, order.Seq, api.NewID("req"), "fixture", "fixture", "fixture", ts(s.now())); err != nil {
		t.Fatal(err)
	}
	bundle := syntheticPreparedContext(t, item, orderRef, syntheticHistory(item, order))
	digestBytes := sha256.Sum256(bundle)
	digest := hex.EncodeToString(digestBytes[:])
	agentID := api.NewID("agt")
	expectedRunID := api.NewID("run")
	if _, err = s.CreateAllocationIntent(ctx, task.ID, api.CreateAllocationIntentRequest{
		AgentID: agentID, TargetTaskID: task.ID, ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef,
		ContextDigest: digest, TeamRole: api.TeamRoleMember, AuthorAgentID: lead.ID, AuthorRunID: lead.RunID, ExpectedRunID: expectedRunID,
	}, by); err != nil {
		t.Fatal(err)
	}
	member, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{
		AgentID: agentID, Name: "builder", Host: "fixture", Session: "builder", Runtime: "codex", ParentAgentID: lead.ID,
		WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: orderRef, ContextBundle: bundle, TeamRole: api.TeamRoleMember},
	}, by)
	if err != nil || member.WorkItem == nil || member.WorkItem.TeamRole != api.TeamRoleMember {
		t.Fatalf("team-role-classified admission failed on a migrated legacy database: %+v %v", member, err)
	}
}

func TestMigrateAddsFollowThroughColumnsWithoutInventingEnrollment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delivery-v1.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE required_deliveries (
  id TEXT PRIMARY KEY, task_id TEXT NOT NULL, message_seq INTEGER NOT NULL,
  kind TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL,
  item_task_id TEXT NOT NULL, item_id TEXT NOT NULL, item_revision INTEGER NOT NULL,
  work_order_task_id TEXT NOT NULL, work_order_message_seq INTEGER NOT NULL,
  context_digest TEXT NOT NULL, generation INTEGER NOT NULL,
  supersedes_delivery_id TEXT, current INTEGER NOT NULL DEFAULT 1,
  phase TEXT NOT NULL, execution_epoch INTEGER NOT NULL DEFAULT 1,
	  current_block_id TEXT, result_text TEXT NOT NULL DEFAULT '',
	  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
	);
	CREATE UNIQUE INDEX required_deliveries_one_current
	  ON required_deliveries(task_id,item_task_id,item_id,agent_id,run_id) WHERE current=1;
	INSERT INTO required_deliveries(id,task_id,message_seq,kind,agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_digest,generation,current,phase,execution_epoch,created_at,updated_at)
VALUES('dly_0000000000000001','tsk_0000000000000001',1,'assignment','agt_0000000000000001','run_0000000000000001','tsk_0000000000000001','wi_0000000000000001',1,'tsk_0000000000000001',1,'digest',1,1,'unacknowledged',1,'2026-09-13T00:00:00Z','2026-09-13T00:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	if err = migrate(db); err != nil {
		t.Fatalf("v1 directive ledger migration: %v", err)
	}
	var version, bytes, attempts int
	var governing, digest, deadline, recipientKind, actionKey, actionClass string
	if err = db.QueryRow(`SELECT enrollment_version,instruction_bytes,followthrough_attempt_count,governing_order_task_id,instruction_sha256,next_substantive_deadline_at,recipient_kind,action_key,action_class FROM required_deliveries WHERE id='dly_0000000000000001'`).Scan(&version, &bytes, &attempts, &governing, &digest, &deadline, &recipientKind, &actionKey, &actionClass); err != nil {
		t.Fatal(err)
	}
	if version != 1 || bytes != 0 || attempts != 0 || governing != "" || digest != "" || deadline != "" || recipientKind != api.DeliveryRecipientItemWorker || actionKey != "primary" || actionClass != api.DeliveryActionExecution {
		t.Fatalf("legacy row was backfilled with invented enrollment: version=%d bytes=%d attempts=%d governing=%q digest=%q deadline=%q recipient=%q action=%q class=%q", version, bytes, attempts, governing, digest, deadline, recipientKind, actionKey, actionClass)
	}
	var incidentsTable, actionIndex int
	if err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='delivery_recovery_incidents'`).Scan(&incidentsTable); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM pragma_index_info('required_deliveries_one_current') WHERE name IN ('recipient_kind','action_key')`).Scan(&actionIndex); err != nil {
		t.Fatal(err)
	}
	if incidentsTable != 1 || actionIndex != 2 {
		t.Fatalf("mandatory-action migration missing incident table or action-scoped current index: table=%d indexColumns=%d", incidentsTable, actionIndex)
	}
}
