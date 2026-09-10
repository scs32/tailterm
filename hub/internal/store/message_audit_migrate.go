package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const messageAuditSchema = `
CREATE TABLE IF NOT EXISTS message_audit_originals (
  message_task_id TEXT NOT NULL,
  message_seq INTEGER NOT NULL,
  classification TEXT NOT NULL CHECK(classification IN ('intake','work')),
  work_items TEXT NOT NULL DEFAULT '[]',
  work_order_task_id TEXT,
  work_order_message_seq INTEGER,
  created_at TEXT NOT NULL,
  PRIMARY KEY(message_task_id,message_seq),
  CHECK((work_order_task_id IS NULL) = (work_order_message_seq IS NULL)),
  FOREIGN KEY(message_task_id,message_seq) REFERENCES messages(task_id,seq),
  FOREIGN KEY(work_order_task_id,work_order_message_seq) REFERENCES messages(task_id,seq)
);
CREATE TABLE IF NOT EXISTS message_audit_states (
  message_task_id TEXT NOT NULL,
  message_seq INTEGER NOT NULL,
  revision INTEGER NOT NULL CHECK(revision > 0),
  classification TEXT NOT NULL CHECK(classification IN ('intake','work')),
  current_event_seq INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(message_task_id,message_seq),
  FOREIGN KEY(message_task_id,message_seq) REFERENCES messages(task_id,seq)
);
CREATE INDEX IF NOT EXISTS message_audit_states_kind ON message_audit_states(message_task_id,classification,message_seq);
CREATE TABLE IF NOT EXISTS message_audit_links (
  message_task_id TEXT NOT NULL,
  message_seq INTEGER NOT NULL,
  ordinal INTEGER NOT NULL CHECK(ordinal >= 0 AND ordinal <= 15),
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  item_revision INTEGER NOT NULL CHECK(item_revision > 0),
  relationship TEXT NOT NULL CHECK(relationship IN ('primary','related')),
  PRIMARY KEY(message_task_id,message_seq,item_task_id,item_id),
  UNIQUE(message_task_id,message_seq,ordinal),
  FOREIGN KEY(message_task_id,message_seq) REFERENCES message_audit_states(message_task_id,message_seq),
  FOREIGN KEY(item_task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE UNIQUE INDEX IF NOT EXISTS message_audit_links_primary
ON message_audit_links(message_task_id,message_seq) WHERE relationship='primary';
CREATE INDEX IF NOT EXISTS message_audit_links_item ON message_audit_links(item_task_id,item_id,message_task_id,message_seq);
CREATE TABLE IF NOT EXISTS message_audit_events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  message_task_id TEXT NOT NULL,
  message_seq INTEGER NOT NULL,
  audit_revision INTEGER NOT NULL CHECK(audit_revision > 0),
  operation TEXT NOT NULL CHECK(operation IN ('post','baseline','correct','resolve')),
  provenance TEXT NOT NULL,
  before_state TEXT NOT NULL DEFAULT '',
  after_state TEXT NOT NULL,
  reason TEXT NOT NULL,
  sources TEXT NOT NULL DEFAULT '[]',
  agent_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(message_task_id,message_seq,audit_revision),
  FOREIGN KEY(message_task_id,message_seq) REFERENCES messages(task_id,seq)
);
CREATE INDEX IF NOT EXISTS message_audit_events_project ON message_audit_events(message_task_id,seq);
CREATE TABLE IF NOT EXISTS message_audit_receipts (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  operation TEXT NOT NULL CHECK(operation IN ('correct','resolve')),
  agent_id TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  request_id TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  message_seq INTEGER NOT NULL,
  audit_revision INTEGER NOT NULL,
  event_seq INTEGER NOT NULL REFERENCES message_audit_events(seq),
  result TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(task_id,operation,agent_id,by_node,by_user,request_id),
  FOREIGN KEY(task_id,message_seq) REFERENCES messages(task_id,seq)
);
CREATE TABLE IF NOT EXISTS message_audit_foreign_associations (
  id TEXT PRIMARY KEY,
  message_task_id TEXT NOT NULL REFERENCES tasks(id),
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  item_revision INTEGER NOT NULL CHECK(item_revision > 0),
  source_task_id TEXT NOT NULL,
  source_message_seq INTEGER NOT NULL,
  reason TEXT NOT NULL,
  agent_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(message_task_id,item_task_id,item_id,item_revision),
  FOREIGN KEY(item_task_id,item_id) REFERENCES work_items(task_id,id),
  FOREIGN KEY(source_task_id,source_message_seq) REFERENCES messages(task_id,seq)
);
CREATE TABLE IF NOT EXISTS message_audit_association_receipts (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  agent_id TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  request_id TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  association_id TEXT NOT NULL REFERENCES message_audit_foreign_associations(id),
  result TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(task_id,agent_id,by_node,by_user,request_id)
);`

// migrateMessageAudit adds A2 authority without changing immutable messages or
// A1 creation links. Valid A1 links become explicit work baselines; messages
// without an A1 link deliberately remain unclassified and receive no row.
func migrateMessageAudit(db *sql.DB) error {
	if _, err := db.Exec(messageAuditSchema); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT m.task_id,m.seq,m.created_at,
l.item_task_id,l.item_id,l.item_revision,l.relationship,l.work_order_task_id,l.work_order_message_seq,l.created_at
FROM messages m JOIN message_work_item_links l ON l.message_seq=m.seq
LEFT JOIN message_audit_states s ON s.message_task_id=m.task_id AND s.message_seq=m.seq
WHERE s.message_seq IS NULL ORDER BY m.seq`)
	if err != nil {
		return err
	}
	type baseline struct {
		task, messageCreated string
		seq                  int64
		link                 api.MessageWorkItem
		orderTask            sql.NullString
		orderSeq             sql.NullInt64
		linkCreated          string
	}
	var pending []baseline
	for rows.Next() {
		var b baseline
		if err := rows.Scan(&b.task, &b.seq, &b.messageCreated,
			&b.link.ItemTaskID, &b.link.ItemID, &b.link.ItemRevision, &b.link.Relationship,
			&b.orderTask, &b.orderSeq, &b.linkCreated); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	migratedAt := ts(time.Now().UTC())
	for _, b := range pending {
		links := []api.MessageWorkItem{b.link}
		linksJSON, _ := json.Marshal(links)
		var orderTask, orderSeq any
		if b.orderTask.Valid {
			orderTask, orderSeq = b.orderTask.String, b.orderSeq.Int64
		}
		if _, err := tx.Exec(`INSERT INTO message_audit_originals
(message_task_id,message_seq,classification,work_items,work_order_task_id,work_order_message_seq,created_at)
VALUES(?,?,'work',?,?,?,?)`, b.task, b.seq, string(linksJSON), orderTask, orderSeq, b.messageCreated); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO message_audit_states(message_task_id,message_seq,revision,classification,current_event_seq,updated_at)
VALUES(?,?,1,'work',0,?)`, b.task, b.seq, b.linkCreated); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO message_audit_links(message_task_id,message_seq,ordinal,item_task_id,item_id,item_revision,relationship)
VALUES(?,?,0,?,?,?,?)`, b.task, b.seq, b.link.ItemTaskID, b.link.ItemID, b.link.ItemRevision, b.link.Relationship); err != nil {
			return err
		}
		after := api.MessageAuditProjection{Revision: 1, Classification: api.MessageAuditWork, WorkItems: links, UpdatedAt: parseTS(b.linkCreated)}
		afterJSON, _ := json.Marshal(after)
		sourcesJSON, _ := json.Marshal([]api.MessageReference{{TaskID: b.task, Seq: b.seq}})
		eventID := api.NewID("mae")
		result, err := tx.Exec(`INSERT INTO message_audit_events
(id,message_task_id,message_seq,audit_revision,operation,provenance,before_state,after_state,reason,sources,agent_id,run_id,by_node,by_user,created_at)
VALUES(?,?,?,1,'baseline','validated_a1_migration','',?,'Validated A1 creation context baseline.',?,'','','','',?)`,
			eventID, b.task, b.seq, string(afterJSON), string(sourcesJSON), migratedAt)
		if err != nil {
			return err
		}
		eventSeq, err := result.LastInsertId()
		if err != nil {
			return err
		}
		after.EventSeq = eventSeq
		afterJSON, _ = json.Marshal(after)
		if _, err := tx.Exec(`UPDATE message_audit_events SET after_state=? WHERE seq=?`, string(afterJSON), eventSeq); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE message_audit_states SET current_event_seq=? WHERE message_task_id=? AND message_seq=?`, eventSeq, b.task, b.seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}
