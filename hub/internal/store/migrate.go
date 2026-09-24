package store

import "database/sql"
import "github.com/scs32/tailterm/hub/internal/api"

// Additive migrations preserve existing task history and encrypted tailnet state.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(messageChecksSchema); err != nil {
		return err
	}
	if _, err := db.Exec(obligationsSchema); err != nil {
		return err
	}
	if err := migrateBridge(db); err != nil {
		return err
	}
	for _, c := range []struct{ table, name, definition string }{
		{"agents", "role", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "cleanup_done", "INTEGER NOT NULL DEFAULT 0"},
		{"agents", "cleanup_error", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "blocked_reason", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "blocked_text", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "run_id", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "last_seen_at", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "reply_to", "INTEGER NOT NULL DEFAULT 0"},
		{"tasks", "allow_agent_spawn", "INTEGER NOT NULL DEFAULT 0"},
		{"tasks", "max_new_agents", "INTEGER NOT NULL DEFAULT 2"},
		{"tasks", "swarm", "INTEGER NOT NULL DEFAULT 0"},
		{"tasks", "orchestrator", "TEXT NOT NULL DEFAULT ''"},
		{"tasks", "lead_revision", "INTEGER NOT NULL DEFAULT 0"},
		{"tasks", "pause_state", "TEXT NOT NULL DEFAULT 'active'"},
		{"tasks", "lifecycle_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"tasks", "pause_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"tasks", "paused_at", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "broadcast", "INTEGER NOT NULL DEFAULT 0"},
		{"messages", "envelope", "TEXT NOT NULL DEFAULT ''"},
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, c.table, c.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.name + " " + c.definition); err != nil {
				return err
			}
		}
	}

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS lead_assignments (
 task_id TEXT NOT NULL REFERENCES tasks(id), request_id TEXT NOT NULL,
 payload TEXT NOT NULL, result TEXT NOT NULL,
 PRIMARY KEY(task_id,request_id));
CREATE TABLE IF NOT EXISTS profile_meta (key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS profiles(username TEXT PRIMARY KEY,key_hash TEXT NOT NULL,revision INTEGER NOT NULL,updated_at TEXT NOT NULL,envelope BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS profile_history(username TEXT NOT NULL,revision INTEGER NOT NULL,updated_at TEXT NOT NULL,envelope BLOB NOT NULL,PRIMARY KEY(username,revision));`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS agents_database_handler
ON agents(task_id) WHERE role='database_handler' AND status<>'closed';
CREATE TABLE IF NOT EXISTS work_items (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  kind TEXT NOT NULL,
  title TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  priority TEXT NOT NULL,
  revision INTEGER NOT NULL,
  source_message_seq INTEGER NOT NULL DEFAULT 0,
  created_agent TEXT NOT NULL DEFAULT '',
  created_node TEXT NOT NULL,
  created_user TEXT NOT NULL,
  updated_agent TEXT NOT NULL DEFAULT '',
  updated_node TEXT NOT NULL,
  updated_user TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS work_items_list ON work_items(task_id,kind,status,seq);
CREATE TABLE IF NOT EXISTS work_item_changes (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  item_id TEXT NOT NULL REFERENCES work_items(id),
  revision INTEGER NOT NULL,
  kind TEXT NOT NULL,
  fields TEXT NOT NULL DEFAULT '',
  agent_id TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS work_item_changes_item ON work_item_changes(item_id,seq);
CREATE TABLE IF NOT EXISTS work_item_dispatches (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  item_id TEXT NOT NULL REFERENCES work_items(id),
  item_revision INTEGER NOT NULL,
  snapshot TEXT NOT NULL,
  target_task_id TEXT NOT NULL REFERENCES tasks(id),
  target_agent_id TEXT NOT NULL REFERENCES agents(id),
  message_seq INTEGER NOT NULL,
  agent_id TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS work_item_dispatches_item ON work_item_dispatches(item_id,seq);
CREATE TABLE IF NOT EXISTS work_item_requests (
  task_id TEXT NOT NULL REFERENCES tasks(id),
  operation TEXT NOT NULL,
  request_id TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  item_id TEXT NOT NULL REFERENCES work_items(id),
  dispatch_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  PRIMARY KEY(task_id,operation,request_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS messages_task_seq_unique ON messages(task_id,seq);
CREATE UNIQUE INDEX IF NOT EXISTS work_items_task_id_unique ON work_items(task_id,id);
CREATE TABLE IF NOT EXISTS message_work_item_links (
  message_seq INTEGER PRIMARY KEY,
  message_task_id TEXT NOT NULL,
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  item_revision INTEGER NOT NULL CHECK(item_revision > 0),
  relationship TEXT NOT NULL CHECK(relationship = 'primary'),
  work_order_task_id TEXT,
  work_order_message_seq INTEGER,
  created_at TEXT NOT NULL,
  CHECK((work_order_task_id IS NULL) = (work_order_message_seq IS NULL)),
  FOREIGN KEY(message_task_id,message_seq) REFERENCES messages(task_id,seq),
  FOREIGN KEY(item_task_id,item_id) REFERENCES work_items(task_id,id),
  FOREIGN KEY(work_order_task_id,work_order_message_seq) REFERENCES messages(task_id,seq)
);
CREATE INDEX IF NOT EXISTS message_work_item_links_item ON message_work_item_links(item_task_id,item_id,message_seq);
CREATE TABLE IF NOT EXISTS agent_work_item_bindings (
  agent_id TEXT NOT NULL REFERENCES agents(id),
  run_id TEXT NOT NULL,
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  item_revision INTEGER NOT NULL CHECK(item_revision > 0),
  work_order_task_id TEXT NOT NULL,
  work_order_message_seq INTEGER NOT NULL CHECK(work_order_message_seq > 0),
  context_through_message_seq INTEGER NOT NULL CHECK(context_through_message_seq >= 0),
  replaces_agent_id TEXT REFERENCES agents(id),
  team_role TEXT NOT NULL DEFAULT '',
  context_digest TEXT NOT NULL,
  context_json BLOB NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(agent_id,run_id),
  FOREIGN KEY(item_task_id,item_id) REFERENCES work_items(task_id,id),
  FOREIGN KEY(work_order_task_id,work_order_message_seq) REFERENCES messages(task_id,seq)
);
CREATE INDEX IF NOT EXISTS agent_work_item_bindings_item ON agent_work_item_bindings(item_task_id,item_id,created_at);
DROP INDEX IF EXISTS agent_work_item_bindings_replacement;
CREATE TABLE IF NOT EXISTS message_post_requests (
  receipt_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  agent_id TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  request_id TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  message_seq INTEGER NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  UNIQUE(task_id,agent_id,by_node,by_user,request_id),
  FOREIGN KEY(task_id,message_seq) REFERENCES messages(task_id,seq)
);
CREATE TABLE IF NOT EXISTS decision_requests (
  message_seq INTEGER PRIMARY KEY,
  task_id TEXT NOT NULL,
  question TEXT NOT NULL,
  options TEXT NOT NULL,
  recommended_option_id TEXT NOT NULL,
  recommendation_reason TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(task_id,message_seq),
  FOREIGN KEY(task_id,message_seq) REFERENCES messages(task_id,seq)
);
CREATE INDEX IF NOT EXISTS decision_requests_task ON decision_requests(task_id,message_seq);
CREATE TABLE IF NOT EXISTS decision_answers (
  message_seq INTEGER PRIMARY KEY,
  task_id TEXT NOT NULL,
  request_seq INTEGER NOT NULL UNIQUE,
  option_id TEXT,
  text TEXT,
  created_at TEXT NOT NULL,
  CHECK(option_id IS NOT NULL OR text IS NOT NULL),
  UNIQUE(task_id,message_seq),
  FOREIGN KEY(task_id,message_seq) REFERENCES messages(task_id,seq),
  FOREIGN KEY(task_id,request_seq) REFERENCES decision_requests(task_id,message_seq)
);`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS project_pause_cycles (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  pause_generation INTEGER NOT NULL CHECK(pause_generation > 0),
  lifecycle_generation INTEGER NOT NULL CHECK(lifecycle_generation > 0),
  state TEXT NOT NULL,
  retained_handoff_digest TEXT NOT NULL,
  previous_team_id TEXT NOT NULL DEFAULT '',
  previous_orchestrator TEXT NOT NULL DEFAULT '',
  selected_team_id TEXT NOT NULL DEFAULT '',
  resume_agent_id TEXT NOT NULL DEFAULT '',
  resume_run_id TEXT NOT NULL DEFAULT '',
  resume_name TEXT NOT NULL DEFAULT '',
  resume_receipt_id TEXT NOT NULL DEFAULT '',
  resume_admitted_at TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  resumed_at TEXT NOT NULL DEFAULT '',
  UNIQUE(task_id,pause_generation)
);
CREATE TABLE IF NOT EXISTS project_pause_targets (
  cycle_id TEXT NOT NULL REFERENCES project_pause_cycles(id),
  agent_id TEXT NOT NULL REFERENCES agents(id),
  run_id TEXT NOT NULL,
  snapshot_json BLOB NOT NULL,
  requested_service_disposition TEXT NOT NULL DEFAULT 'unresolved',
  service_disposition TEXT NOT NULL,
  handoff_note TEXT NOT NULL DEFAULT '',
  service_verified INTEGER NOT NULL DEFAULT 0,
  service_evidence_id TEXT NOT NULL DEFAULT '',
  service_evidence_version INTEGER NOT NULL DEFAULT 0,
  handoff_updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(cycle_id,agent_id,run_id)
);
CREATE INDEX IF NOT EXISTS project_pause_targets_agent_run ON project_pause_targets(agent_id,run_id);
CREATE TABLE IF NOT EXISTS project_pause_receipts (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  operation TEXT NOT NULL,
  request_id TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  cycle_id TEXT NOT NULL REFERENCES project_pause_cycles(id),
  created_at TEXT NOT NULL,
  UNIQUE(task_id,operation,request_id)
);`); err != nil {
		return err
	}
	for _, c := range []struct{ table, name, definition string }{
		{"project_pause_cycles", "previous_team_id", "TEXT NOT NULL DEFAULT ''"},
		{"project_pause_cycles", "previous_orchestrator", "TEXT NOT NULL DEFAULT ''"},
		{"project_pause_cycles", "selected_team_id", "TEXT NOT NULL DEFAULT ''"},
		{"project_pause_cycles", "resume_agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"project_pause_cycles", "resume_run_id", "TEXT NOT NULL DEFAULT ''"},
		{"project_pause_cycles", "resume_name", "TEXT NOT NULL DEFAULT ''"},
		{"project_pause_cycles", "resume_receipt_id", "TEXT NOT NULL DEFAULT ''"},
		{"project_pause_cycles", "resume_admitted_at", "TEXT NOT NULL DEFAULT ''"},
		{"project_pause_targets", "requested_service_disposition", "TEXT NOT NULL DEFAULT 'unresolved'"},
		{"project_pause_targets", "service_verified", "INTEGER NOT NULL DEFAULT 0"},
		{"project_pause_targets", "service_evidence_id", "TEXT NOT NULL DEFAULT ''"},
		{"project_pause_targets", "service_evidence_version", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, c.table, c.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.name + " " + c.definition); err != nil {
				return err
			}
		}
	}
	for _, c := range []struct{ name, definition string }{
		{"narrative_scope_revision", "INTEGER NOT NULL DEFAULT 0"},
		{"completion_report_id", "TEXT NOT NULL DEFAULT ''"},
		{"completion_report_version", "INTEGER NOT NULL DEFAULT 0"},
		{"completion_report_digest", "TEXT NOT NULL DEFAULT ''"},
		{"completion_scope_revision", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('work_items') WHERE name=?`, c.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec("ALTER TABLE work_items ADD COLUMN " + c.name + " " + c.definition); err != nil {
				return err
			}
		}
	}
	// agent_work_item_bindings predates team_role (item-extra-capacity
	// correction); the CREATE TABLE above only supplies it for a fresh
	// database, so an existing table needs the same targeted check.
	var teamRoleColumn int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('agent_work_item_bindings') WHERE name='team_role'`).Scan(&teamRoleColumn); err != nil {
		return err
	}
	if teamRoleColumn == 0 {
		if _, err := db.Exec(`ALTER TABLE agent_work_item_bindings ADD COLUMN team_role TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	// This index must not be created inside the CREATE TABLE IF NOT EXISTS
	// block above: on an existing database (table already present without
	// team_role, before the ALTER TABLE just above ran) an index on that
	// column would fail with "no such column: team_role" before this
	// migration ever reached the column repair. It is safe only here, after
	// the column is guaranteed to exist either way.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS agent_work_item_bindings_item_team_role ON agent_work_item_bindings(item_task_id,item_id,team_role,created_at)`); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE work_items SET narrative_scope_revision=revision WHERE narrative_scope_revision=0`); err != nil {
		return err
	}
	if err := reconcileWorkItemHistory(db); err != nil {
		return err
	}
	if err := migrateMessageAudit(db); err != nil {
		return err
	}
	if err := migrateNarrative(db); err != nil {
		return err
	}
	if err := migrateAuditExports(db); err != nil {
		return err
	}
	if err := migrateQueue(db); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS agent_allocation_intents (
  agent_id TEXT PRIMARY KEY,
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  item_revision INTEGER NOT NULL CHECK(item_revision > 0),
  work_order_task_id TEXT NOT NULL,
  work_order_message_seq INTEGER NOT NULL CHECK(work_order_message_seq > 0),
  team_role TEXT NOT NULL,
  target_task_id TEXT NOT NULL DEFAULT '',
  context_digest TEXT NOT NULL DEFAULT '',
  author_agent_id TEXT NOT NULL DEFAULT '',
  author_run_id TEXT NOT NULL DEFAULT '',
  expected_run_id TEXT NOT NULL DEFAULT '',
  expected_launcher_agent_id TEXT NOT NULL DEFAULT '',
  expected_launcher_run_id TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  launcher_agent_id TEXT NOT NULL DEFAULT '',
  launcher_run_id TEXT NOT NULL DEFAULT '',
  created_by_node TEXT NOT NULL,
  created_by_user TEXT NOT NULL,
  created_at TEXT NOT NULL,
  consumed_at TEXT NOT NULL DEFAULT '',
  consumed_by_run_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS agent_allocation_intents_item ON agent_allocation_intents(item_task_id,item_id,item_revision);`); err != nil {
		return err
	}
	// This table was itself introduced mid-correction (item-extra-capacity,
	// review #2300/#2771/#2840 finding 5); an already-upgraded existing
	// database can have the table without the fields added by review #2916
	// (expected-run binding, author-run/launcher provenance, idempotent
	// retry identity). CREATE TABLE IF NOT EXISTS is a no-op there, so
	// these columns need the same targeted ALTER-TABLE-after-check pattern
	// as team_role did -- and for the identical reason: an index or query
	// referencing a column that does not exist yet would abort before ever
	// reaching a later repair.
	for _, c := range []struct{ name, definition string }{
		{"target_task_id", "TEXT NOT NULL DEFAULT ''"},
		{"context_digest", "TEXT NOT NULL DEFAULT ''"},
		{"author_agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"author_run_id", "TEXT NOT NULL DEFAULT ''"},
		{"expected_run_id", "TEXT NOT NULL DEFAULT ''"},
		{"expected_launcher_agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"expected_launcher_run_id", "TEXT NOT NULL DEFAULT ''"},
		{"request_id", "TEXT NOT NULL DEFAULT ''"},
		{"launcher_agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"launcher_run_id", "TEXT NOT NULL DEFAULT ''"},
		{"invalidated_at", "TEXT NOT NULL DEFAULT ''"},
		{"invalidated_pause_generation", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('agent_allocation_intents') WHERE name=?`, c.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec("ALTER TABLE agent_allocation_intents ADD COLUMN " + c.name + " " + c.definition); err != nil {
				return err
			}
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS agent_allocation_intents_retry ON agent_allocation_intents(target_task_id,author_agent_id,author_run_id,request_id) WHERE request_id<>''`); err != nil {
		return err
	}
	// A legacy (pre-#2916) intent row has empty expected_run_id/
	// context_digest/author_*: it was authored under the weaker contract
	// and never bound to a preallocated run or a checked author. It must
	// never authorize an admission under the corrected rule -- an "upgraded"
	// empty row is not silently grandfathered in. Store.AddAgent enforces
	// this by requiring all four fields nonempty at consumption; no
	// migration step here attempts to backfill or guess them.
	if err := migrateScheduleMonitor(db); err != nil {
		return err
	}
	if err := migrateDelivery(db); err != nil {
		return err
	}
	if err := migrateOperationalRecords(db); err != nil {
		return err
	}
	if err := migrateDeliveryFollowThrough(db); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO profile_meta(key,value) VALUES('instance',?)`, api.NewID("profilehub"))
	return err
}

// migrateDeliveryFollowThrough upgrades existing directive ledgers. New
// databases receive these columns from migrateDelivery; this additive path is
// deliberately explicit so v1 rows remain readable and unverified rather than
// being backfilled with invented governing-order evidence.
func migrateDeliveryFollowThrough(db *sql.DB) error {
	for _, column := range []struct{ name, definition string }{
		{"governing_order_task_id", "TEXT NOT NULL DEFAULT ''"},
		{"recipient_kind", "TEXT NOT NULL DEFAULT 'item_worker'"},
		{"action_key", "TEXT NOT NULL DEFAULT 'primary'"},
		{"action_class", "TEXT NOT NULL DEFAULT 'execution'"},
		{"governing_order_message_seq", "INTEGER NOT NULL DEFAULT 0"},
		{"instruction_sha256", "TEXT NOT NULL DEFAULT ''"},
		{"instruction_bytes", "INTEGER NOT NULL DEFAULT 0"},
		{"enrollment_version", "INTEGER NOT NULL DEFAULT 1"},
		{"ack_deadline_seconds", "INTEGER NOT NULL DEFAULT 120"},
		{"progress_deadline_seconds", "INTEGER NOT NULL DEFAULT 600"},
		{"resume_deadline_seconds", "INTEGER NOT NULL DEFAULT 120"},
		{"confirmation_deadline_seconds", "INTEGER NOT NULL DEFAULT 120"},
		{"dispatch_report_seconds", "INTEGER NOT NULL DEFAULT 30"},
		{"active_tool_hard_limit_seconds", "INTEGER NOT NULL DEFAULT 1800"},
		{"max_queue_attempts", "INTEGER NOT NULL DEFAULT 2"},
		{"last_substantive_progress_at", "TEXT NOT NULL DEFAULT ''"},
		{"next_substantive_deadline_at", "TEXT NOT NULL DEFAULT ''"},
		{"hard_deadline_at", "TEXT NOT NULL DEFAULT ''"},
		{"followthrough_attempt_count", "INTEGER NOT NULL DEFAULT 0"},
		{"followthrough_pending_action", "TEXT NOT NULL DEFAULT ''"},
		{"followthrough_pending_request_id", "TEXT NOT NULL DEFAULT ''"},
		{"followthrough_pending_since", "TEXT NOT NULL DEFAULT ''"},
		{"followthrough_transport_outcome", "TEXT NOT NULL DEFAULT ''"},
		{"followthrough_confirmation_until", "TEXT NOT NULL DEFAULT ''"},
		{"followthrough_escalation_message_seq", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('required_deliveries') WHERE name=?`, column.name).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			if _, err := db.Exec("ALTER TABLE required_deliveries ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	_, err := db.Exec(`DROP INDEX IF EXISTS required_deliveries_one_current;
CREATE UNIQUE INDEX required_deliveries_one_current
  ON required_deliveries(task_id,item_task_id,item_id,agent_id,run_id,recipient_kind,action_key) WHERE current=1;
CREATE TABLE IF NOT EXISTS delivery_recovery_incidents (
  id TEXT PRIMARY KEY,
  delivery_id TEXT NOT NULL REFERENCES required_deliveries(id),
  generation INTEGER NOT NULL,
  execution_epoch INTEGER NOT NULL,
  cause_status TEXT NOT NULL,
  last_substantive_action TEXT NOT NULL,
  last_substantive_at TEXT NOT NULL,
  expected_next_action TEXT NOT NULL,
  stop_reason TEXT NOT NULL,
  causal_evidence TEXT NOT NULL,
  contributing_conditions TEXT NOT NULL,
  unresolved_questions TEXT NOT NULL,
  prevention_owner_agent_id TEXT NOT NULL,
  prevention_owner_run_id TEXT NOT NULL,
  prevention_work_order_task_id TEXT NOT NULL,
  prevention_work_order_message_seq INTEGER NOT NULL,
  prevention_verification_criterion TEXT NOT NULL,
  prior_incident_id TEXT NOT NULL DEFAULT '',
  prior_control_failure TEXT NOT NULL DEFAULT '',
  recorded_by_agent_id TEXT NOT NULL,
  recorded_by_run_id TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS delivery_recovery_incidents_delivery ON delivery_recovery_incidents(delivery_id,created_at,id);`)
	return err
}
