package store

import "database/sql"
import "github.com/scs32/tailterm/hub/internal/api"

// Additive migrations preserve existing task history and encrypted tailnet state.
func migrate(db *sql.DB) error {
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
		{"messages", "broadcast", "INTEGER NOT NULL DEFAULT 0"},
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

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS profile_meta (key TEXT PRIMARY KEY,value TEXT NOT NULL);
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
	if _, err := db.Exec(`UPDATE work_items SET narrative_scope_revision=revision WHERE narrative_scope_revision=0`); err != nil {
		return err
	}
	if err := reconcileWorkItemHistory(db); err != nil {
		return err
	}
	if err := migrateNarrative(db); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT OR IGNORE INTO profile_meta(key,value) VALUES('instance',?)`, api.NewID("profilehub"))
	return err
}
