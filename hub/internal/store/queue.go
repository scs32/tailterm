package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const queueSchema = `
CREATE TABLE IF NOT EXISTS queue_entries (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  target_task_id TEXT NOT NULL REFERENCES tasks(id),
  source_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  cycle INTEGER NOT NULL,
  revision INTEGER NOT NULL,
  state TEXT NOT NULL,
  queue_priority TEXT NOT NULL,
  eligible INTEGER NOT NULL,
  stale INTEGER NOT NULL,
  review_needed INTEGER NOT NULL,
  pending_update INTEGER NOT NULL,
  offered_item_revision INTEGER NOT NULL,
  current_item_revision INTEGER NOT NULL,
  claimed_item_revision INTEGER NOT NULL DEFAULT 0,
  claimant_agent_id TEXT NOT NULL DEFAULT '',
  claimant_run_id TEXT NOT NULL DEFAULT '',
  work_order_task_id TEXT NOT NULL DEFAULT '',
  work_order_message_seq INTEGER NOT NULL DEFAULT 0,
  selection_task_id TEXT NOT NULL DEFAULT '',
  selection_message_seq INTEGER NOT NULL DEFAULT 0,
  worker_agent_id TEXT NOT NULL DEFAULT '',
  worker_run_id TEXT NOT NULL DEFAULT '',
  context_digest TEXT NOT NULL DEFAULT '',
  reconciliation_needed INTEGER NOT NULL DEFAULT 0,
  terminal_reason TEXT NOT NULL DEFAULT '',
  terminal_item_revision INTEGER NOT NULL DEFAULT 0,
  legacy_observed INTEGER NOT NULL DEFAULT 0,
  orchestrator_agent_id TEXT NOT NULL DEFAULT '',
  orchestrator_run_id TEXT NOT NULL DEFAULT '',
  item_kind TEXT NOT NULL,
  item_title TEXT NOT NULL,
  item_status TEXT NOT NULL,
  item_priority TEXT NOT NULL,
  item_description TEXT NOT NULL DEFAULT '',
  first_enqueued_at TEXT NOT NULL,
  latest_enqueued_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(target_task_id,source_task_id,item_id),
  FOREIGN KEY(source_task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE INDEX IF NOT EXISTS queue_entries_target ON queue_entries(target_task_id,state,queue_priority,seq);
CREATE INDEX IF NOT EXISTS queue_entries_source ON queue_entries(source_task_id,item_id,state);
CREATE TABLE IF NOT EXISTS queue_cycles (
  entry_id TEXT NOT NULL REFERENCES queue_entries(id),
  cycle INTEGER NOT NULL,
  initial_state TEXT NOT NULL,
  final_state TEXT NOT NULL DEFAULT '',
  opened_at TEXT NOT NULL,
  closed_at TEXT,
  terminal_reason TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(entry_id,cycle)
);
CREATE TABLE IF NOT EXISTS queue_events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  target_task_id TEXT NOT NULL REFERENCES tasks(id),
  entry_id TEXT NOT NULL REFERENCES queue_entries(id),
  cycle INTEGER NOT NULL,
  revision INTEGER NOT NULL,
  kind TEXT NOT NULL,
  actor_agent_id TEXT NOT NULL DEFAULT '',
  actor_node TEXT NOT NULL,
  actor_user TEXT NOT NULL,
  actor_run_id TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  dispatch_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT '',
  priority_rank INTEGER NOT NULL DEFAULT 3,
  first_enqueued_at TEXT NOT NULL DEFAULT '',
  entry_seq INTEGER NOT NULL DEFAULT 0,
  snapshot TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  effective_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS queue_events_target ON queue_events(target_task_id,seq);
CREATE INDEX IF NOT EXISTS queue_events_entry ON queue_events(entry_id,seq);
CREATE TABLE IF NOT EXISTS queue_dispatch_links (
  dispatch_id TEXT PRIMARY KEY,
  entry_id TEXT NOT NULL REFERENCES queue_entries(id),
  cycle INTEGER NOT NULL,
  event_seq INTEGER NOT NULL REFERENCES queue_events(seq),
  item_revision INTEGER NOT NULL,
  message_seq INTEGER NOT NULL DEFAULT 0,
  causal_agent_id TEXT NOT NULL DEFAULT '',
  causal_node TEXT NOT NULL,
  causal_user TEXT NOT NULL,
  dispatch_created_at TEXT NOT NULL,
  observed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS queue_dispatch_links_entry ON queue_dispatch_links(entry_id,cycle,event_seq);
CREATE TABLE IF NOT EXISTS queue_requests (
  receipt_id TEXT PRIMARY KEY,
  target_task_id TEXT NOT NULL REFERENCES tasks(id),
  operation TEXT NOT NULL,
  request_id TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  agent_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  entry_id TEXT NOT NULL REFERENCES queue_entries(id),
  cycle INTEGER NOT NULL,
  event_seq INTEGER NOT NULL REFERENCES queue_events(seq),
  result TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(target_task_id,agent_id,by_node,by_user,request_id)
);
CREATE INDEX IF NOT EXISTS queue_requests_entry ON queue_requests(entry_id,created_at,receipt_id);
CREATE TABLE IF NOT EXISTS queue_notifications (
  id TEXT PRIMARY KEY,
  event_seq INTEGER NOT NULL REFERENCES queue_events(seq),
  entry_id TEXT NOT NULL REFERENCES queue_entries(id),
  cycle INTEGER NOT NULL,
  kind TEXT NOT NULL,
  causal_agent_id TEXT NOT NULL DEFAULT '',
  causal_node TEXT NOT NULL,
  causal_user TEXT NOT NULL,
  recipient_agent_id TEXT NOT NULL DEFAULT '',
  recipient_run_id TEXT NOT NULL DEFAULT '',
  recipient_generation INTEGER NOT NULL,
  status TEXT NOT NULL,
  message_seq INTEGER NOT NULL DEFAULT 0,
  unavailable_reason TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(event_seq,recipient_generation)
);
CREATE INDEX IF NOT EXISTS queue_notifications_entry ON queue_notifications(entry_id,cycle,event_seq);
`

const queueEntryCols = `seq,id,target_task_id,source_task_id,item_id,cycle,revision,state,queue_priority,eligible,stale,review_needed,pending_update,offered_item_revision,current_item_revision,claimed_item_revision,claimant_agent_id,claimant_run_id,work_order_task_id,work_order_message_seq,selection_task_id,selection_message_seq,worker_agent_id,worker_run_id,context_digest,reconciliation_needed,terminal_reason,terminal_item_revision,legacy_observed,orchestrator_agent_id,orchestrator_run_id,item_kind,item_title,item_status,item_priority,item_description,first_enqueued_at,latest_enqueued_at,updated_at`

func migrateQueue(db *sql.DB) error {
	for _, column := range []struct{ name, definition string }{
		{"system_notice_kind", "TEXT NOT NULL DEFAULT ''"},
		{"system_notice_id", "TEXT NOT NULL DEFAULT ''"},
		{"from_run_id", "TEXT NOT NULL DEFAULT ''"},
	} {
		var found int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('messages') WHERE name=?`, column.name).Scan(&found); err != nil {
			return err
		}
		if found == 0 {
			if _, err := db.Exec("ALTER TABLE messages ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	if _, err := db.Exec(queueSchema); err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"state", "TEXT NOT NULL DEFAULT ''"},
		{"priority_rank", "INTEGER NOT NULL DEFAULT 3"},
		{"first_enqueued_at", "TEXT NOT NULL DEFAULT ''"},
		{"entry_seq", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var found int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('queue_events') WHERE name=?`, column.name).Scan(&found); err != nil {
			return err
		}
		if found == 0 {
			if _, err := db.Exec("ALTER TABLE queue_events ADD COLUMN " + column.name + " " + column.definition); err != nil {
				return err
			}
		}
	}
	if _, err := db.Exec(`UPDATE queue_events SET
		state=json_extract(snapshot,'$.state'),
		priority_rank=CASE json_extract(snapshot,'$.queuePriority') WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END,
		first_enqueued_at=json_extract(snapshot,'$.firstEnqueuedAt'),
		entry_seq=CAST(json_extract(snapshot,'$.seq') AS INTEGER)
		WHERE state='' OR first_enqueued_at='' OR entry_seq=0`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS queue_events_list ON queue_events(target_task_id,entry_id,seq,state,priority_rank,first_enqueued_at,entry_seq)`); err != nil {
		return err
	}
	return reconcileLegacyQueue(db, time.Now().UTC())
}

func validQueueEntryID(id string) bool {
	if len(id) != 20 || !strings.HasPrefix(id, "que_") {
		return false
	}
	for _, r := range id[4:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func terminalQueueState(state string) bool {
	return state == api.QueueStateCompleted || state == api.QueueStateCancelled
}

func scanQueueEntryFull(row rowScanner) (api.QueueEntry, error) {
	var entry api.QueueEntry
	var eligible, stale, review, pending, reconcile, legacy int
	var workOrderTask, selectionTask, first, latest, updated string
	var workOrderSeq, selectionSeq int64
	err := row.Scan(&entry.Seq, &entry.ID, &entry.TargetTaskID, &entry.SourceTaskID, &entry.ItemID,
		&entry.Cycle, &entry.Revision, &entry.State, &entry.QueuePriority, &eligible, &stale, &review, &pending,
		&entry.OfferedItemRevision, &entry.CurrentItemRevision, &entry.ClaimedItemRevision,
		&entry.ClaimantAgentID, &entry.ClaimantRunID, &workOrderTask, &workOrderSeq, &selectionTask, &selectionSeq,
		&entry.WorkerAgentID, &entry.WorkerRunID, &entry.ContextDigest, &reconcile, &entry.TerminalReason,
		&entry.TerminalItemRevision, &legacy, &entry.OrchestratorAgentID, &entry.OrchestratorRunID,
		&entry.Item.Kind, &entry.Item.Title, &entry.Item.Status, &entry.Item.Priority, &entry.Item.Description,
		&first, &latest, &updated)
	if err != nil {
		return entry, err
	}
	entry.Eligible, entry.Stale, entry.ReviewNeeded, entry.PendingUpdate = eligible != 0, stale != 0, review != 0, pending != 0
	entry.ReconciliationNeeded, entry.LegacyObserved = reconcile != 0, legacy != 0
	entry.FirstEnqueuedAt, entry.LatestEnqueuedAt, entry.UpdatedAt = parseTS(first), parseTS(latest), parseTS(updated)
	if workOrderTask != "" {
		entry.WorkOrderMessage = &api.MessageReference{TaskID: workOrderTask, Seq: workOrderSeq}
	}
	if selectionTask != "" {
		entry.Selection = &api.QueueSelection{TaskID: selectionTask, MessageSeq: selectionSeq}
	}
	return entry, nil
}

func getQueueEntry(q queryRower, ctx context.Context, targetTaskID, entryID string) (api.QueueEntry, error) {
	entry, err := scanQueueEntryFull(q.QueryRowContext(ctx, `SELECT `+queueEntryCols+` FROM queue_entries WHERE target_task_id=? AND id=?`, targetTaskID, entryID))
	if errors.Is(err, sql.ErrNoRows) {
		return entry, api.ErrNotFound
	}
	return entry, err
}

func getQueueEntryByItem(q queryRower, ctx context.Context, targetTaskID, sourceTaskID, itemID string) (api.QueueEntry, error) {
	entry, err := scanQueueEntryFull(q.QueryRowContext(ctx, `SELECT `+queueEntryCols+` FROM queue_entries WHERE target_task_id=? AND source_task_id=? AND item_id=?`, targetTaskID, sourceTaskID, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return entry, api.ErrNotFound
	}
	return entry, err
}

func queueEntryValues(entry api.QueueEntry) []any {
	workOrderTask, workOrderSeq, selectionTask, selectionSeq := "", int64(0), "", int64(0)
	if entry.WorkOrderMessage != nil {
		workOrderTask, workOrderSeq = entry.WorkOrderMessage.TaskID, entry.WorkOrderMessage.Seq
	}
	if entry.Selection != nil {
		selectionTask, selectionSeq = entry.Selection.TaskID, entry.Selection.MessageSeq
	}
	return []any{entry.Cycle, entry.Revision, entry.State, entry.QueuePriority, entry.Eligible, entry.Stale,
		entry.ReviewNeeded, entry.PendingUpdate, entry.OfferedItemRevision, entry.CurrentItemRevision,
		entry.ClaimedItemRevision, entry.ClaimantAgentID, entry.ClaimantRunID, workOrderTask, workOrderSeq,
		selectionTask, selectionSeq, entry.WorkerAgentID, entry.WorkerRunID, entry.ContextDigest,
		entry.ReconciliationNeeded, entry.TerminalReason, entry.TerminalItemRevision, entry.LegacyObserved,
		entry.OrchestratorAgentID, entry.OrchestratorRunID, entry.Item.Kind, entry.Item.Title, entry.Item.Status,
		entry.Item.Priority, entry.Item.Description, ts(entry.FirstEnqueuedAt), ts(entry.LatestEnqueuedAt), ts(entry.UpdatedAt), entry.ID}
}

func updateQueueEntry(ctx context.Context, tx *sql.Tx, entry api.QueueEntry) error {
	_, err := tx.ExecContext(ctx, `UPDATE queue_entries SET cycle=?,revision=?,state=?,queue_priority=?,eligible=?,stale=?,review_needed=?,pending_update=?,offered_item_revision=?,current_item_revision=?,claimed_item_revision=?,claimant_agent_id=?,claimant_run_id=?,work_order_task_id=?,work_order_message_seq=?,selection_task_id=?,selection_message_seq=?,worker_agent_id=?,worker_run_id=?,context_digest=?,reconciliation_needed=?,terminal_reason=?,terminal_item_revision=?,legacy_observed=?,orchestrator_agent_id=?,orchestrator_run_id=?,item_kind=?,item_title=?,item_status=?,item_priority=?,item_description=?,first_enqueued_at=?,latest_enqueued_at=?,updated_at=? WHERE id=?`, queueEntryValues(entry)...)
	return err
}

func insertQueueEntry(ctx context.Context, tx *sql.Tx, entry *api.QueueEntry) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO queue_entries(id,target_task_id,source_task_id,item_id,cycle,revision,state,queue_priority,eligible,stale,review_needed,pending_update,offered_item_revision,current_item_revision,claimed_item_revision,claimant_agent_id,claimant_run_id,work_order_task_id,work_order_message_seq,selection_task_id,selection_message_seq,worker_agent_id,worker_run_id,context_digest,reconciliation_needed,terminal_reason,terminal_item_revision,legacy_observed,orchestrator_agent_id,orchestrator_run_id,item_kind,item_title,item_status,item_priority,item_description,first_enqueued_at,latest_enqueued_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		entry.ID, entry.TargetTaskID, entry.SourceTaskID, entry.ItemID, entry.Cycle, entry.Revision, entry.State,
		entry.QueuePriority, entry.Eligible, entry.Stale, entry.ReviewNeeded, entry.PendingUpdate,
		entry.OfferedItemRevision, entry.CurrentItemRevision, entry.ClaimedItemRevision, entry.ClaimantAgentID,
		entry.ClaimantRunID, "", 0, "", 0, entry.WorkerAgentID, entry.WorkerRunID, entry.ContextDigest,
		entry.ReconciliationNeeded, entry.TerminalReason, entry.TerminalItemRevision, entry.LegacyObserved,
		entry.OrchestratorAgentID, entry.OrchestratorRunID, entry.Item.Kind, entry.Item.Title, entry.Item.Status,
		entry.Item.Priority, entry.Item.Description, ts(entry.FirstEnqueuedAt), ts(entry.LatestEnqueuedAt), ts(entry.UpdatedAt))
	if err != nil {
		return err
	}
	entry.Seq, _ = result.LastInsertId()
	_, err = tx.ExecContext(ctx, `INSERT INTO queue_cycles(entry_id,cycle,initial_state,opened_at) VALUES(?,?,?,?)`, entry.ID, entry.Cycle, entry.State, ts(entry.FirstEnqueuedAt))
	return err
}

func appendQueueEvent(ctx context.Context, tx *sql.Tx, entry api.QueueEntry, kind, reason, dispatchID, actorRunID string, actor api.Sender, observed, effective time.Time) (api.QueueEvent, error) {
	snapshot, err := json.Marshal(entry)
	if err != nil {
		return api.QueueEvent{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO queue_events(target_task_id,entry_id,cycle,revision,kind,actor_agent_id,actor_node,actor_user,actor_run_id,reason,dispatch_id,state,priority_rank,first_enqueued_at,entry_seq,snapshot,observed_at,effective_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		entry.TargetTaskID, entry.ID, entry.Cycle, entry.Revision, kind, actor.AgentID, actor.Node, actor.User, actorRunID, reason, dispatchID, entry.State, priorityRank(entry.QueuePriority), ts(entry.FirstEnqueuedAt), entry.Seq, string(snapshot), ts(observed), ts(effective))
	if err != nil {
		return api.QueueEvent{}, err
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return api.QueueEvent{}, err
	}
	return api.QueueEvent{Seq: seq, TargetTaskID: entry.TargetTaskID, EntryID: entry.ID, Cycle: entry.Cycle, Revision: entry.Revision, Kind: kind, Actor: actor, ActorRunID: actorRunID, Reason: reason, DispatchID: dispatchID, Snapshot: entry, ObservedAt: observed, EffectiveAt: effective}, nil
}

func scanQueueEvent(row rowScanner) (api.QueueEvent, error) {
	var event api.QueueEvent
	var snapshot, observed, effective string
	err := row.Scan(&event.Seq, &event.TargetTaskID, &event.EntryID, &event.Cycle, &event.Revision, &event.Kind,
		&event.Actor.AgentID, &event.Actor.Node, &event.Actor.User, &event.ActorRunID, &event.Reason, &event.DispatchID,
		&snapshot, &observed, &effective)
	if err != nil {
		return event, err
	}
	event.ObservedAt, event.EffectiveAt = parseTS(observed), parseTS(effective)
	err = json.Unmarshal([]byte(snapshot), &event.Snapshot)
	return event, err
}

const queueEventCols = `seq,target_task_id,entry_id,cycle,revision,kind,actor_agent_id,actor_node,actor_user,actor_run_id,reason,dispatch_id,snapshot,observed_at,effective_at`

func scanQueueNotification(row rowScanner) (api.QueueNotification, error) {
	var n api.QueueNotification
	var created string
	err := row.Scan(&n.ID, &n.EventSeq, &n.EntryID, &n.Cycle, &n.Kind, &n.CausalAuthor.AgentID,
		&n.CausalAuthor.Node, &n.CausalAuthor.User, &n.RecipientAgentID, &n.RecipientRunID,
		&n.RecipientGeneration, &n.Status, &n.MessageSeq, &n.UnavailableReason, &created)
	n.CreatedAt = parseTS(created)
	return n, err
}

const queueNotificationCols = `id,event_seq,entry_id,cycle,kind,causal_agent_id,causal_node,causal_user,recipient_agent_id,recipient_run_id,recipient_generation,status,message_seq,unavailable_reason,created_at`

func (s *Store) createQueueNotificationGeneration(ctx context.Context, tx *sql.Tx, entry api.QueueEntry, event api.QueueEvent, causal api.Sender, recipientAgentID, recipientRunID string, generation, existingMessageSeq int64) (*api.QueueNotification, error) {
	now := s.now()
	n := api.QueueNotification{ID: api.NewID("qnt"), EventSeq: event.Seq, EntryID: event.EntryID, Cycle: event.Cycle,
		Kind: api.QueueNoticeChanged, CausalAuthor: causal, RecipientAgentID: recipientAgentID,
		RecipientRunID: recipientRunID, RecipientGeneration: generation, Status: "pending", CreatedAt: now}
	if existingMessageSeq > 0 {
		result, updateErr := tx.ExecContext(ctx, `UPDATE messages SET from_agent='',from_run_id='',from_node='system',from_user='queue',system_notice_kind=?,system_notice_id=? WHERE task_id=? AND seq=? AND to_agent=?`, api.QueueNoticeChanged, n.ID, entry.TargetTaskID, existingMessageSeq, n.RecipientAgentID)
		if updateErr != nil {
			return nil, updateErr
		}
		changed, updateErr := result.RowsAffected()
		if updateErr != nil || changed != 1 {
			if updateErr != nil {
				return nil, updateErr
			}
			return nil, errors.New("queue notice provenance message missing")
		}
		n.MessageSeq, n.Status = existingMessageSeq, "stored"
	} else {
		var recipient api.Agent
		var err error
		if n.RecipientAgentID != "" {
			recipient, err = scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=? AND task_id=?`, n.RecipientAgentID, entry.TargetTaskID))
		}
		if n.RecipientAgentID == "" || errors.Is(err, sql.ErrNoRows) {
			n.Status, n.UnavailableReason = "unavailable", "project has no current orchestrator agent"
		} else if err != nil {
			return nil, err
		} else if recipient.RunID != n.RecipientRunID || recipient.Status == api.AgentRetired || recipient.Status == api.AgentClosed || recipient.Status == api.AgentExited {
			n.Status, n.UnavailableReason = "unavailable", "the pinned orchestrator run is not deliverable"
		} else {
			text := fmt.Sprintf("Queue updated: %s cycle %d is %s. Review and deliberately Pull if appropriate; this notice is not a work order and does not start an agent.", event.EntryID, event.Cycle, event.Snapshot.State)
			result, insertErr := tx.ExecContext(ctx, `INSERT INTO messages(task_id,from_agent,from_node,from_user,to_agent,text,created_at,reply_to,broadcast,system_notice_kind,system_notice_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				entry.TargetTaskID, "", "system", "queue", recipient.ID, text, ts(now), 0, false, api.QueueNoticeChanged, n.ID)
			if insertErr != nil {
				return nil, insertErr
			}
			n.MessageSeq, _ = result.LastInsertId()
			n.Status = "stored"
			if _, insertErr = s.insertEvent(ctx, tx, entry.TargetTaskID, api.EventMessage, "", text, map[string]any{"seq": n.MessageSeq, "to": recipient.ID, "systemNotice": api.QueueNoticeChanged, "queueEntryId": entry.ID, "queueEventSeq": event.Seq}, api.Caller{Node: "system", User: "queue"}); insertErr != nil {
				return nil, insertErr
			}
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO queue_notifications(id,event_seq,entry_id,cycle,kind,causal_agent_id,causal_node,causal_user,recipient_agent_id,recipient_run_id,recipient_generation,status,message_seq,unavailable_reason,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.ID, n.EventSeq, n.EntryID, n.Cycle, n.Kind, n.CausalAuthor.AgentID, n.CausalAuthor.Node, n.CausalAuthor.User,
		n.RecipientAgentID, n.RecipientRunID, n.RecipientGeneration, n.Status, n.MessageSeq, n.UnavailableReason, ts(n.CreatedAt))
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *Store) createQueueNotification(ctx context.Context, tx *sql.Tx, entry api.QueueEntry, event api.QueueEvent, causal api.Sender, existingMessageSeq int64) (*api.QueueNotification, error) {
	return s.createQueueNotificationGeneration(ctx, tx, entry, event, causal, entry.OrchestratorAgentID, entry.OrchestratorRunID, 1, existingMessageSeq)
}

// recoverQueueNotification deliberately creates a new recipient generation for
// one outstanding unavailable semantic event. The original unavailable row is
// retained, while the Queue reconciliation action has its own separate event
// and receipt. A read, restart, or ordinary Queue mutation never calls this.
func (s *Store) recoverQueueNotification(ctx context.Context, tx *sql.Tx, entry api.QueueEntry, recipient api.Agent) (*api.QueueNotification, error) {
	if recipient.Status == api.AgentRetired || recipient.Status == api.AgentClosed || recipient.Status == api.AgentExited || recipient.RunID == "" {
		return nil, workItemConflict("the current orchestrator run is not deliverable")
	}
	prior, err := scanQueueNotification(tx.QueryRowContext(ctx, `SELECT `+queueNotificationCols+` FROM queue_notifications n
WHERE n.entry_id=? AND n.cycle=? AND n.status='unavailable'
AND n.recipient_generation=(SELECT MAX(n2.recipient_generation) FROM queue_notifications n2 WHERE n2.event_seq=n.event_seq)
ORDER BY n.event_seq DESC LIMIT 1`, entry.ID, entry.Cycle))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, workItemConflict("Queue has no outstanding unavailable notification to reconcile")
	}
	if err != nil {
		return nil, err
	}
	event, err := scanQueueEvent(tx.QueryRowContext(ctx, `SELECT `+queueEventCols+` FROM queue_events WHERE target_task_id=? AND seq=? AND entry_id=?`, entry.TargetTaskID, prior.EventSeq, entry.ID))
	if err != nil {
		return nil, err
	}
	return s.createQueueNotificationGeneration(ctx, tx, entry, event, prior.CausalAuthor, recipient.ID, recipient.RunID, prior.RecipientGeneration+1, 0)
}

func currentQueueOrchestrator(q queryRower, ctx context.Context, task api.Task) (api.Agent, error) {
	if task.Orchestrator == "" {
		return api.Agent{}, workItemConflict("target project has no orchestrator")
	}
	agent, err := scanAgent(q.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND name=? COLLATE NOCASE AND role='' AND status NOT IN ('closed','exited') ORDER BY created_at DESC LIMIT 1`, task.ID, task.Orchestrator))
	if errors.Is(err, sql.ErrNoRows) {
		var role string
		roleErr := q.QueryRowContext(ctx, `SELECT role FROM agents WHERE task_id=? AND name=? COLLATE NOCASE AND status NOT IN ('closed','exited') ORDER BY created_at DESC LIMIT 1`, task.ID, task.Orchestrator).Scan(&role)
		if roleErr == nil {
			return api.Agent{}, workItemConflict("target project orchestrator name belongs to role " + role)
		}
		if !errors.Is(roleErr, sql.ErrNoRows) {
			return api.Agent{}, roleErr
		}
		return api.Agent{}, workItemConflict("target project orchestrator has no open agent")
	}
	return agent, err
}

func queueSummary(item api.WorkItem) api.QueueItemSummary {
	return api.QueueItemSummary{Kind: item.Kind, Title: item.Title, Status: item.Status, Priority: item.Priority, Description: item.Description}
}

// enqueueWorkItemDispatch is called inside the existing dispatch transaction.
// It creates exactly one stable entry, an immutable provenance event/link and,
// only for semantic changes, one typed notification intent.
func (s *Store) enqueueWorkItemDispatch(ctx context.Context, tx *sql.Tx, item api.WorkItem, targetTask api.Task, targetAgent api.Agent, dispatch api.WorkItemDispatch, causal api.Sender) (api.QueueDispatchReceipt, int64, error) {
	now := dispatch.CreatedAt
	entry, err := getQueueEntryByItem(tx, ctx, targetTask.ID, item.TaskID, item.ID)
	created := errors.Is(err, api.ErrNotFound)
	if err != nil && !created {
		return api.QueueDispatchReceipt{}, 0, err
	}
	semantic := true
	kind := "enqueued"
	if created {
		entry = api.QueueEntry{ID: api.NewID("que"), TargetTaskID: targetTask.ID, SourceTaskID: item.TaskID, ItemID: item.ID,
			Cycle: 1, Revision: 1, State: api.QueueStateWaiting, QueuePriority: item.Priority, Eligible: true,
			OfferedItemRevision: item.Revision, CurrentItemRevision: item.Revision, OrchestratorAgentID: targetAgent.ID,
			OrchestratorRunID: targetAgent.RunID, FirstEnqueuedAt: now, LatestEnqueuedAt: now, UpdatedAt: now, Item: queueSummary(item)}
		if err = insertQueueEntry(ctx, tx, &entry); err != nil {
			return api.QueueDispatchReceipt{}, 0, err
		}
	} else {
		if terminalQueueState(entry.State) {
			return api.QueueDispatchReceipt{}, 0, workItemConflict("queue cycle is terminal; explicitly requeue it before sending again")
		}
		if entry.OfferedItemRevision == item.Revision {
			semantic, kind = false, "dispatch_linked"
		} else {
			kind = "offer_updated"
			entry.Item, entry.CurrentItemRevision = queueSummary(item), item.Revision
			entry.LatestEnqueuedAt, entry.UpdatedAt = now, now
			entry.Revision++
			entry.OfferedItemRevision = item.Revision
			entry.Stale = false
			if entry.State == api.QueueStateClaimed || entry.State == api.QueueStateActive {
				entry.PendingUpdate = true
			} else {
				entry.PendingUpdate, entry.Stale, entry.ReviewNeeded, entry.Eligible = false, false, false, true
			}
			if err = updateQueueEntry(ctx, tx, entry); err != nil {
				return api.QueueDispatchReceipt{}, 0, err
			}
		}
	}
	event, err := appendQueueEvent(ctx, tx, entry, kind, "", dispatch.ID, "", causal, now, now)
	if err != nil {
		return api.QueueDispatchReceipt{}, 0, err
	}
	var notification *api.QueueNotification
	if semantic {
		notification, err = s.createQueueNotification(ctx, tx, entry, event, causal, dispatch.MessageSeq)
		if err != nil {
			return api.QueueDispatchReceipt{}, 0, err
		}
	}
	messageSeq := dispatch.MessageSeq
	if _, err = tx.ExecContext(ctx, `INSERT INTO queue_dispatch_links(dispatch_id,entry_id,cycle,event_seq,item_revision,message_seq,causal_agent_id,causal_node,causal_user,dispatch_created_at,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		dispatch.ID, entry.ID, entry.Cycle, event.Seq, item.Revision, messageSeq, causal.AgentID, causal.Node, causal.User, ts(dispatch.CreatedAt), ts(now)); err != nil {
		return api.QueueDispatchReceipt{}, 0, err
	}
	return api.QueueDispatchReceipt{Entry: entry, Event: event, Notification: notification}, messageSeq, nil
}

func loadQueueDispatchReceipt(q queryRower, ctx context.Context, dispatchID string) (api.QueueDispatchReceipt, error) {
	var eventSeq int64
	if err := q.QueryRowContext(ctx, `SELECT event_seq FROM queue_dispatch_links WHERE dispatch_id=?`, dispatchID).Scan(&eventSeq); err != nil {
		return api.QueueDispatchReceipt{}, err
	}
	event, err := scanQueueEvent(q.QueryRowContext(ctx, `SELECT `+queueEventCols+` FROM queue_events WHERE seq=?`, eventSeq))
	if err != nil {
		return api.QueueDispatchReceipt{}, err
	}
	out := api.QueueDispatchReceipt{Entry: event.Snapshot, Event: event}
	n, nerr := scanQueueNotification(q.QueryRowContext(ctx, `SELECT `+queueNotificationCols+` FROM queue_notifications WHERE event_seq=? ORDER BY recipient_generation LIMIT 1`, eventSeq))
	if nerr == nil {
		out.Notification = &n
	} else if !errors.Is(nerr, sql.ErrNoRows) {
		return out, nerr
	}
	return out, nil
}

func (s *Store) authorizeQueueAction(ctx context.Context, tx *sql.Tx, entry api.QueueEntry, req api.QueueActionRequest, by api.Caller) error {
	if req.AgentID == "" {
		if req.RunID != "" || req.Selection != nil {
			return api.ErrInvalid
		}
		return nil // Explicit human UI action in the trusted workspace.
	}
	if !api.ValidID(req.AgentID, "agt") || !validRunID(req.RunID) || req.Selection == nil {
		return api.ErrInvalid
	}
	var role, runID, status, taskID string
	if err := tx.QueryRowContext(ctx, `SELECT role,run_id,status,task_id FROM agents WHERE id=?`, req.AgentID).Scan(&role, &runID, &status, &taskID); err != nil {
		return api.ErrInvalid
	}
	if taskID != entry.TargetTaskID || role != api.AgentRoleDatabaseHandler || runID != req.RunID || status == api.AgentRetired || status == api.AgentClosed || status == api.AgentExited {
		return api.ErrInvalid
	}
	if req.Selection.TaskID != entry.TargetTaskID || req.Selection.MessageSeq < 1 {
		return api.ErrInvalid
	}
	var fromAgent, fromRun, systemNoticeKind, systemNoticeID string
	if err := tx.QueryRowContext(ctx, `SELECT from_agent,from_run_id,system_notice_kind,system_notice_id FROM messages WHERE task_id=? AND seq=?`, req.Selection.TaskID, req.Selection.MessageSeq).Scan(&fromAgent, &fromRun, &systemNoticeKind, &systemNoticeID); err != nil {
		return api.ErrInvalid
	}
	if fromAgent == "" {
		if systemNoticeKind != "" || systemNoticeID != "" {
			return api.ErrInvalid
		}
		return nil
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, entry.TargetTaskID))
	if err != nil {
		return err
	}
	orchestrator, err := currentQueueOrchestrator(tx, ctx, task)
	if err != nil || orchestrator.ID != fromAgent || orchestrator.RunID != fromRun {
		return api.ErrInvalid
	}
	return nil
}

func validateQueueClaimant(ctx context.Context, tx *sql.Tx, entry api.QueueEntry, agentID, runID string) (api.Agent, error) {
	if !api.ValidID(agentID, "agt") || !validRunID(runID) {
		return api.Agent{}, api.ErrInvalid
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, entry.TargetTaskID))
	if err != nil {
		return api.Agent{}, err
	}
	agent, err := currentQueueOrchestrator(tx, ctx, task)
	if err != nil {
		return api.Agent{}, err
	}
	if agent.ID != agentID || agent.RunID != runID || agent.Status == api.AgentRetired {
		return api.Agent{}, workItemConflict("claimant is not the current active project orchestrator run")
	}
	return agent, nil
}

func validateQueueWorkOrder(ctx context.Context, q queryRower, entry api.QueueEntry, ref *api.MessageReference, revision int64) error {
	if ref == nil || !api.ValidID(ref.TaskID, "tsk") || ref.Seq < 1 {
		return api.ErrInvalid
	}
	var noticeKind string
	if err := q.QueryRowContext(ctx, `SELECT system_notice_kind FROM messages WHERE task_id=? AND seq=?`, ref.TaskID, ref.Seq).Scan(&noticeKind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workItemConflict("work order message does not exist")
		}
		return err
	}
	if noticeKind != "" {
		return workItemConflict("automated system notices are not bounded work orders")
	}
	var found int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM message_audit_states s JOIN message_audit_links l ON l.message_task_id=s.message_task_id AND l.message_seq=s.message_seq
WHERE s.message_task_id=? AND s.message_seq=? AND s.classification='work' AND l.item_task_id=? AND l.item_id=? AND l.item_revision=? AND l.relationship='primary'`,
		ref.TaskID, ref.Seq, entry.SourceTaskID, entry.ItemID, revision).Scan(&found)
	if err != nil {
		return err
	}
	if found != 1 {
		return workItemConflict("work order is not linked to the exact offered item revision")
	}
	var dispatchOnly int
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM work_item_dispatches WHERE target_task_id=? AND message_seq=? AND item_id=? AND item_revision=?`,
		ref.TaskID, ref.Seq, entry.ItemID, revision).Scan(&dispatchOnly); err != nil {
		return err
	}
	if dispatchOnly != 0 {
		return workItemConflict("dispatch enqueue messages are not bounded work orders")
	}
	return nil
}

func queueActionPayload(entryID string, req api.QueueActionRequest) string {
	return requestHash(struct {
		Path string                 `json:"path"`
		Req  api.QueueActionRequest `json:"request"`
	}{entryID, req})
}

func loadQueueActionResult(q queryRower, ctx context.Context, taskID, requestID, agentID string, by api.Caller) (api.QueueActionResult, string, error) {
	var raw, hash string
	err := q.QueryRowContext(ctx, `SELECT result,payload_hash FROM queue_requests WHERE target_task_id=? AND agent_id=? AND by_node=? AND by_user=? AND request_id=?`, taskID, agentID, by.Node, by.User, requestID).Scan(&raw, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return api.QueueActionResult{}, "", api.ErrNotFound
	}
	var result api.QueueActionResult
	if err == nil {
		err = json.Unmarshal([]byte(raw), &result)
	}
	return result, hash, err
}

func validQueueReason(reason string, required bool) bool {
	if required && strings.TrimSpace(reason) == "" {
		return false
	}
	return api.ValidText(reason, api.MaxQueueReasonBytes)
}

func (s *Store) QueueAction(ctx context.Context, targetTaskID, entryID string, req api.QueueActionRequest, by api.Caller) (api.QueueActionResult, error) {
	if !api.ValidID(targetTaskID, "tsk") || !validQueueEntryID(entryID) || !validRequestID(req.RequestID) || req.ExpectedRevision < 1 || req.Cycle < 1 {
		return api.QueueActionResult{}, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	payload := queueActionPayload(entryID, req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.QueueActionResult{}, err
	}
	defer tx.Rollback()
	prior, priorHash, priorErr := loadQueueActionResult(tx, ctx, targetTaskID, req.RequestID, req.AgentID, by)
	if priorErr == nil {
		if priorHash != payload {
			return api.QueueActionResult{}, workItemConflict("request ID was already used with different Queue intent")
		}
		prior.Replay = true
		return prior, nil
	}
	if !errors.Is(priorErr, api.ErrNotFound) {
		return api.QueueActionResult{}, priorErr
	}
	entry, err := getQueueEntry(tx, ctx, targetTaskID, entryID)
	if err != nil {
		return api.QueueActionResult{}, err
	}
	if err = s.authorizeQueueAction(ctx, tx, entry, req, by); err != nil {
		return api.QueueActionResult{}, err
	}
	if entry.Revision != req.ExpectedRevision || entry.Cycle != req.Cycle {
		return api.QueueActionResult{}, workItemConflict("queue entry changed; refresh before retrying")
	}
	targetTask, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, targetTaskID))
	if err != nil {
		return api.QueueActionResult{}, err
	}
	if targetTask.Status != api.TaskOpen {
		return api.QueueActionResult{}, api.ErrClosed
	}
	item, err := getWorkItem(tx, ctx, entry.SourceTaskID, entry.ItemID)
	if err != nil {
		return api.QueueActionResult{}, err
	}
	var sourceTaskStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=?`, entry.SourceTaskID).Scan(&sourceTaskStatus); err != nil {
		return api.QueueActionResult{}, err
	}
	if sourceTaskStatus != api.TaskOpen && req.Operation != "reconcile_recipient" {
		return api.QueueActionResult{}, api.ErrClosed
	}
	now := s.now()
	kind, reason := req.Operation, req.Reason
	var recoveryRecipient *api.Agent
	switch req.Operation {
	case "priority":
		if terminalQueueState(entry.State) || !validWorkItemPriority(req.Priority) || req.Priority == entry.QueuePriority {
			return api.QueueActionResult{}, api.ErrInvalid
		}
		entry.QueuePriority = req.Priority
	case "adopt":
		if entry.State != api.QueueStateWaiting || !entry.ReviewNeeded || req.ExpectedItemRevision != item.Revision || item.Revision != entry.OfferedItemRevision {
			return api.QueueActionResult{}, workItemConflict("legacy entry cannot be adopted at this revision")
		}
		entry.ReviewNeeded, entry.Eligible, entry.Stale = false, true, false
	case "claim":
		if entry.State != api.QueueStateWaiting || !entry.Eligible || entry.Stale || entry.ReviewNeeded || req.ExpectedItemRevision != item.Revision || item.Revision != entry.OfferedItemRevision {
			return api.QueueActionResult{}, workItemConflict("queue entry is not eligible at the exact offered revision")
		}
		claimant, claimErr := validateQueueClaimant(ctx, tx, entry, req.ClaimantAgentID, req.ClaimantRunID)
		if claimErr != nil {
			return api.QueueActionResult{}, claimErr
		}
		if err = validateQueueWorkOrder(ctx, tx, entry, req.WorkOrderMessage, item.Revision); err != nil {
			return api.QueueActionResult{}, err
		}
		entry.State, entry.Eligible = api.QueueStateClaimed, false
		entry.ClaimedItemRevision = item.Revision
		entry.ClaimantAgentID, entry.ClaimantRunID = claimant.ID, claimant.RunID
		entry.WorkOrderMessage = req.WorkOrderMessage
		entry.Selection = req.Selection
		entry.ReconciliationNeeded = false
	case "start":
		if entry.State != api.QueueStateClaimed || entry.ReconciliationNeeded || req.WorkerAgentID == "" || !validRunID(req.WorkerRunID) || req.ContextDigest == "" {
			return api.QueueActionResult{}, workItemConflict("queue claim is not ready for an exact Start")
		}
		if entry.WorkerAgentID != "" || entry.WorkerRunID != "" || entry.ContextDigest != "" {
			if entry.WorkerAgentID != req.WorkerAgentID || entry.WorkerRunID != req.WorkerRunID || entry.ContextDigest != req.ContextDigest {
				return api.QueueActionResult{}, workItemConflict("Start does not match the worker pinned by Queue admission")
			}
		} else if entry.SourceTaskID != entry.TargetTaskID {
			return api.QueueActionResult{}, workItemConflict("cross-project Start has no exact admission pin")
		}
		var agentRun, itemTask, itemID, orderTask, digest, status string
		var revision, orderSeq int64
		err = tx.QueryRowContext(ctx, `SELECT a.run_id,b.item_task_id,b.item_id,b.item_revision,b.work_order_task_id,b.work_order_message_seq,b.context_digest,a.status FROM agents a JOIN agent_work_item_bindings b ON b.agent_id=a.id AND b.run_id=a.run_id WHERE a.id=? AND a.task_id=?`, req.WorkerAgentID, targetTaskID).Scan(&agentRun, &itemTask, &itemID, &revision, &orderTask, &orderSeq, &digest, &status)
		if err != nil || agentRun != req.WorkerRunID || itemTask != entry.SourceTaskID || itemID != entry.ItemID || revision != entry.ClaimedItemRevision || entry.WorkOrderMessage == nil || orderTask != entry.WorkOrderMessage.TaskID || orderSeq != entry.WorkOrderMessage.Seq || digest != req.ContextDigest || status == api.AgentClosed || status == api.AgentExited || status == api.AgentRetired {
			return api.QueueActionResult{}, workItemConflict("worker binding does not match the claimed item/order/context/run")
		}
		entry.State = api.QueueStateActive
		entry.WorkerAgentID, entry.WorkerRunID, entry.ContextDigest = req.WorkerAgentID, req.WorkerRunID, req.ContextDigest
	case "launch_unknown":
		if entry.State != api.QueueStateClaimed || !validQueueReason(reason, true) {
			return api.QueueActionResult{}, api.ErrInvalid
		}
		entry.ReconciliationNeeded = true
	case "release":
		if (entry.State != api.QueueStateClaimed && entry.State != api.QueueStateActive) || !validQueueReason(reason, true) {
			return api.QueueActionResult{}, api.ErrInvalid
		}
		if entry.WorkerRunID != "" && req.ReconciledRunID != entry.WorkerRunID {
			return api.QueueActionResult{}, workItemConflict("release requires exact current-run reconciliation")
		}
		if item.Status == "done" || item.Status == "dismissed" || targetTask.Status != api.TaskOpen {
			return api.QueueActionResult{}, workItemConflict("terminal work must remain terminal in Queue")
		}
		entry.State, entry.Eligible = api.QueueStateWaiting, true
		entry.ClaimedItemRevision, entry.ClaimantAgentID, entry.ClaimantRunID = 0, "", ""
		entry.WorkOrderMessage, entry.Selection = nil, nil
		entry.WorkerAgentID, entry.WorkerRunID, entry.ContextDigest = "", "", ""
		entry.ReconciliationNeeded = false
		entry.Stale = entry.OfferedItemRevision != item.Revision
		if entry.Stale {
			entry.Eligible = false
		}
	case "transfer":
		if entry.State != api.QueueStateClaimed || !validQueueReason(reason, true) {
			return api.QueueActionResult{}, api.ErrInvalid
		}
		if entry.WorkerRunID != "" && req.ReconciledRunID != entry.WorkerRunID {
			return api.QueueActionResult{}, workItemConflict("transfer requires exact admitted-run reconciliation")
		}
		claimant, claimErr := validateQueueClaimant(ctx, tx, entry, req.ClaimantAgentID, req.ClaimantRunID)
		if claimErr != nil {
			return api.QueueActionResult{}, claimErr
		}
		entry.ClaimantAgentID, entry.ClaimantRunID = claimant.ID, claimant.RunID
		entry.Selection = req.Selection
		entry.ReconciliationNeeded = false
	case "reconcile_recipient":
		if !validQueueReason(reason, true) {
			return api.QueueActionResult{}, api.ErrInvalid
		}
		orchestrator, reconcileErr := currentQueueOrchestrator(tx, ctx, targetTask)
		if reconcileErr != nil {
			return api.QueueActionResult{}, reconcileErr
		}
		if orchestrator.Status == api.AgentRetired || orchestrator.Status == api.AgentClosed || orchestrator.Status == api.AgentExited || orchestrator.RunID == "" {
			return api.QueueActionResult{}, workItemConflict("the current orchestrator run is not deliverable")
		}
		sameRecipient := orchestrator.ID == entry.OrchestratorAgentID && orchestrator.RunID == entry.OrchestratorRunID
		entry.OrchestratorAgentID, entry.OrchestratorRunID = orchestrator.ID, orchestrator.RunID
		if sameRecipient {
			recoveryRecipient = &orchestrator
		}
	case "withdraw":
		if terminalQueueState(entry.State) || !validQueueReason(reason, true) {
			return api.QueueActionResult{}, api.ErrInvalid
		}
		if entry.WorkerRunID != "" && req.ReconciledRunID != entry.WorkerRunID {
			return api.QueueActionResult{}, workItemConflict("withdrawal requires exact current-run reconciliation")
		}
		entry.State, entry.Eligible, entry.TerminalReason, entry.TerminalItemRevision = api.QueueStateCancelled, false, reason, entry.CurrentItemRevision
		entry.TerminalItemRevision = item.Revision
	case "requeue":
		if !terminalQueueState(entry.State) || !validQueueReason(reason, true) || item.Status == "done" || item.Status == "dismissed" || targetTask.Status != api.TaskOpen {
			return api.QueueActionResult{}, api.ErrInvalid
		}
		if entry.WorkerRunID != "" && req.ReconciledRunID != entry.WorkerRunID {
			return api.QueueActionResult{}, workItemConflict("requeue requires exact retained-run reconciliation")
		}
		entry.Cycle++
		entry.State, entry.Eligible, entry.ReviewNeeded = api.QueueStateWaiting, true, false
		entry.Stale, entry.PendingUpdate = false, false
		entry.OfferedItemRevision, entry.CurrentItemRevision = item.Revision, item.Revision
		entry.ClaimedItemRevision, entry.ClaimantAgentID, entry.ClaimantRunID = 0, "", ""
		entry.WorkOrderMessage, entry.Selection = nil, nil
		entry.WorkerAgentID, entry.WorkerRunID, entry.ContextDigest = "", "", ""
		entry.ReconciliationNeeded, entry.TerminalReason, entry.TerminalItemRevision = false, "", 0
		if _, err = tx.ExecContext(ctx, `INSERT INTO queue_cycles(entry_id,cycle,initial_state,opened_at) VALUES(?,?,?,?)`, entry.ID, entry.Cycle, entry.State, ts(now)); err != nil {
			return api.QueueActionResult{}, err
		}
	default:
		return api.QueueActionResult{}, api.ErrInvalid
	}
	if req.Operation != "launch_unknown" && !validQueueReason(reason, false) {
		return api.QueueActionResult{}, api.ErrInvalid
	}
	entry.Revision++
	entry.UpdatedAt, entry.CurrentItemRevision, entry.Item = now, item.Revision, queueSummary(item)
	if terminalQueueState(entry.State) {
		_, err = tx.ExecContext(ctx, `UPDATE queue_cycles SET final_state=?,closed_at=?,terminal_reason=? WHERE entry_id=? AND cycle=? AND final_state=''`, entry.State, ts(now), entry.TerminalReason, entry.ID, entry.Cycle)
		if err != nil {
			return api.QueueActionResult{}, err
		}
	}
	if err = updateQueueEntry(ctx, tx, entry); err != nil {
		return api.QueueActionResult{}, err
	}
	actor := api.Sender{AgentID: req.AgentID, Node: by.Node, User: by.User}
	event, err := appendQueueEvent(ctx, tx, entry, kind, reason, "", req.RunID, actor, now, now)
	if err != nil {
		return api.QueueActionResult{}, err
	}
	var notification *api.QueueNotification
	if recoveryRecipient != nil {
		notification, err = s.recoverQueueNotification(ctx, tx, entry, *recoveryRecipient)
	} else {
		notification, err = s.createQueueNotification(ctx, tx, entry, event, actor, 0)
	}
	if err != nil {
		return api.QueueActionResult{}, err
	}
	receipt := api.QueueReceipt{ID: api.NewID("qrr"), RequestID: req.RequestID, Operation: req.Operation, TaskID: targetTaskID, EntryID: entry.ID, Cycle: entry.Cycle, EventSeq: event.Seq, CreatedAt: now}
	result := api.QueueActionResult{Entry: entry, Event: event, Receipt: receipt, Notification: notification}
	raw, _ := json.Marshal(result)
	if _, err = tx.ExecContext(ctx, `INSERT INTO queue_requests(receipt_id,target_task_id,operation,request_id,payload_hash,agent_id,run_id,by_node,by_user,entry_id,cycle,event_seq,result,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		receipt.ID, targetTaskID, req.Operation, req.RequestID, payload, req.AgentID, req.RunID, by.Node, by.User, entry.ID, entry.Cycle, event.Seq, string(raw), ts(now)); err != nil {
		return api.QueueActionResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.QueueActionResult{}, err
	}
	s.notify(targetTaskID)
	return result, nil
}

func (s *Store) GetQueueReceipt(ctx context.Context, taskID, requestID, agentID string, by api.Caller) (api.QueueActionResult, error) {
	if !api.ValidID(taskID, "tsk") || !validRequestID(requestID) || (agentID != "" && !api.ValidID(agentID, "agt")) {
		return api.QueueActionResult{}, api.ErrInvalid
	}
	result, _, err := loadQueueActionResult(s.db, ctx, taskID, requestID, agentID, by)
	return result, err
}

type queueCursor struct {
	TaskID   string `json:"taskId"`
	Kind     string `json:"kind"`
	EntryID  string `json:"entryId,omitempty"`
	Cutoff   int64  `json:"cutoff"`
	Offset   int64  `json:"offset"`
	Terminal bool   `json:"terminal,omitempty"`
}

func encodeQueueCursor(cursor queueCursor) string {
	b, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeQueueCursor(raw string) (queueCursor, error) {
	var cursor queueCursor
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) > 256 || json.Unmarshal(b, &cursor) != nil || cursor.Cutoff < 0 || cursor.Offset < 0 {
		return cursor, api.ErrInvalid
	}
	return cursor, nil
}

func priorityRank(value string) int {
	switch value {
	case "urgent":
		return 0
	case "high":
		return 1
	case "normal":
		return 2
	default:
		return 3
	}
}

func (s *Store) ListQueue(ctx context.Context, taskID, rawCursor string, limit int, includeTerminal bool) (api.QueueList, error) {
	if !api.ValidID(taskID, "tsk") || limit < 1 || limit > api.MaxQueuePage {
		return api.QueueList{}, api.ErrInvalid
	}
	cursor := queueCursor{TaskID: taskID, Kind: "list", Terminal: includeTerminal}
	var err error
	if rawCursor != "" {
		cursor, err = decodeQueueCursor(rawCursor)
		if err != nil || cursor.TaskID != taskID || cursor.Kind != "list" || cursor.EntryID != "" || cursor.Terminal != includeTerminal {
			return api.QueueList{}, api.ErrInvalid
		}
	} else if err = s.db.QueryRowContext(ctx, `SELECT coalesce(max(seq),0) FROM queue_events WHERE target_task_id=?`, taskID).Scan(&cursor.Cutoff); err != nil {
		return api.QueueList{}, err
	}
	terminal := 0
	if includeTerminal {
		terminal = 1
	}
	const latestQueue = `WITH latest AS (
		SELECT entry_id,max(seq) seq FROM queue_events WHERE target_task_id=? AND seq<=? GROUP BY entry_id
	)`
	var total int64
	if err = s.db.QueryRowContext(ctx, latestQueue+`
		SELECT count(*) FROM queue_events e JOIN latest ON latest.seq=e.seq
		WHERE ?=1 OR e.state NOT IN ('completed','cancelled')`, taskID, cursor.Cutoff, terminal).Scan(&total); err != nil {
		return api.QueueList{}, err
	}
	if cursor.Offset > total {
		return api.QueueList{}, api.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, latestQueue+`, page AS (
		SELECT e.seq,e.priority_rank,e.first_enqueued_at,e.entry_seq
		FROM queue_events e JOIN latest ON latest.seq=e.seq
		WHERE ?=1 OR e.state NOT IN ('completed','cancelled')
		ORDER BY e.priority_rank,e.first_enqueued_at,e.entry_seq
		LIMIT ? OFFSET ?
	)
		SELECT e.snapshot FROM page JOIN queue_events e ON e.seq=page.seq
		ORDER BY page.priority_rank,page.first_enqueued_at,page.entry_seq`, taskID, cursor.Cutoff, terminal, limit+1, cursor.Offset)
	if err != nil {
		return api.QueueList{}, err
	}
	entries := []api.QueueEntry{}
	for rows.Next() {
		var raw string
		var entry api.QueueEntry
		if err = rows.Scan(&raw); err != nil || json.Unmarshal([]byte(raw), &entry) != nil {
			rows.Close()
			if err == nil {
				err = errors.New("invalid queue snapshot")
			}
			return api.QueueList{}, err
		}
		entries = append(entries, entry)
	}
	if err = rows.Close(); err != nil {
		return api.QueueList{}, err
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	out := api.QueueList{Entries: entries, Cutoff: cursor.Cutoff}
	setCursor := func() {
		next := cursor.Offset + int64(len(out.Entries))
		out.Complete = next == total
		out.Cursor = ""
		if !out.Complete {
			out.Cursor = encodeQueueCursor(queueCursor{TaskID: taskID, Kind: "list", Cutoff: cursor.Cutoff, Offset: next, Terminal: includeTerminal})
		}
	}
	setCursor()
	for {
		b, marshalErr := json.Marshal(out)
		if marshalErr != nil {
			return api.QueueList{}, marshalErr
		}
		if len(b)+1 <= api.MaxQueuePageBytes {
			break
		}
		if len(out.Entries) <= 1 {
			return api.QueueList{}, api.ErrLimit
		}
		out.Entries = out.Entries[:len(out.Entries)-1]
		setCursor()
	}
	return out, nil
}

func (s *Store) GetQueueEntry(ctx context.Context, taskID, entryID string) (api.QueueEntry, error) {
	if !api.ValidID(taskID, "tsk") || !validQueueEntryID(entryID) {
		return api.QueueEntry{}, api.ErrInvalid
	}
	return getQueueEntry(s.db, ctx, taskID, entryID)
}

func (s *Store) ListQueueHistory(ctx context.Context, taskID, entryID, rawCursor string, limit int) (api.QueueHistory, error) {
	if !api.ValidID(taskID, "tsk") || !validQueueEntryID(entryID) || limit < 1 || limit > api.MaxQueuePage {
		return api.QueueHistory{}, api.ErrInvalid
	}
	cursor := queueCursor{TaskID: taskID, Kind: "history", EntryID: entryID}
	var err error
	if rawCursor != "" {
		cursor, err = decodeQueueCursor(rawCursor)
		if err != nil || cursor.TaskID != taskID || cursor.Kind != "history" || cursor.EntryID != entryID || cursor.Terminal {
			if err == nil {
				err = api.ErrInvalid
			}
			return api.QueueHistory{}, err
		}
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT coalesce(max(seq),0) FROM queue_events WHERE entry_id=?`, entryID).Scan(&cursor.Cutoff)
		if err != nil {
			return api.QueueHistory{}, err
		}
	}
	var entryRaw string
	var entry api.QueueEntry
	if err = s.db.QueryRowContext(ctx, `SELECT snapshot FROM queue_events WHERE target_task_id=? AND entry_id=? AND seq<=? ORDER BY seq DESC LIMIT 1`, taskID, entryID, cursor.Cutoff).Scan(&entryRaw); errors.Is(err, sql.ErrNoRows) {
		return api.QueueHistory{}, api.ErrNotFound
	} else if err != nil || json.Unmarshal([]byte(entryRaw), &entry) != nil {
		if err == nil {
			err = errors.New("invalid frozen Queue entry snapshot")
		}
		return api.QueueHistory{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+queueEventCols+` FROM queue_events WHERE entry_id=? AND seq>? AND seq<=? ORDER BY seq LIMIT ?`, entryID, cursor.Offset, cursor.Cutoff, limit+1)
	if err != nil {
		return api.QueueHistory{}, err
	}
	events := []api.QueueEvent{}
	for rows.Next() {
		event, e := scanQueueEvent(rows)
		if e != nil {
			rows.Close()
			return api.QueueHistory{}, e
		}
		events = append(events, event)
	}
	if err = rows.Close(); err != nil {
		return api.QueueHistory{}, err
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	out := api.QueueHistory{Entry: entry, Events: events, Cutoff: cursor.Cutoff, Complete: !hasMore}
	if hasMore {
		out.Cursor = encodeQueueCursor(queueCursor{TaskID: taskID, Kind: "history", EntryID: entryID, Cutoff: cursor.Cutoff, Offset: events[len(events)-1].Seq})
	}
	for {
		if b, _ := json.Marshal(out); len(b)+1 <= api.MaxQueuePageBytes {
			break
		}
		if len(out.Events) <= 1 {
			return api.QueueHistory{}, api.ErrLimit
		}
		out.Events = out.Events[:len(out.Events)-1]
		out.Complete = false
		out.Cursor = encodeQueueCursor(queueCursor{TaskID: taskID, Kind: "history", EntryID: entryID, Cutoff: cursor.Cutoff, Offset: out.Events[len(out.Events)-1].Seq})
	}
	return out, nil
}

func (s *Store) ListQueueChanges(ctx context.Context, taskID string, after, cutoff int64, limit int) (api.QueueChangePage, error) {
	if !api.ValidID(taskID, "tsk") || after < 0 || cutoff < 0 || limit < 1 || limit > api.MaxQueuePage {
		return api.QueueChangePage{}, api.ErrInvalid
	}
	if cutoff == 0 {
		if err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(seq),0) FROM queue_events WHERE target_task_id=?`, taskID).Scan(&cutoff); err != nil {
			return api.QueueChangePage{}, err
		}
	}
	if after > cutoff {
		return api.QueueChangePage{}, api.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+queueEventCols+` FROM queue_events WHERE target_task_id=? AND seq>? AND seq<=? ORDER BY seq LIMIT ?`, taskID, after, cutoff, limit+1)
	if err != nil {
		return api.QueueChangePage{}, err
	}
	events := []api.QueueEvent{}
	for rows.Next() {
		e, e2 := scanQueueEvent(rows)
		if e2 != nil {
			rows.Close()
			return api.QueueChangePage{}, e2
		}
		events = append(events, e)
	}
	if err = rows.Close(); err != nil {
		return api.QueueChangePage{}, err
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	checkpoint := after
	if len(events) > 0 {
		checkpoint = events[len(events)-1].Seq
	}
	out := api.QueueChangePage{Events: events, Checkpoint: checkpoint, Cutoff: cutoff, Complete: !hasMore}
	for {
		if b, _ := json.Marshal(out); len(b)+1 <= api.MaxQueuePageBytes {
			break
		}
		if len(out.Events) <= 1 {
			return api.QueueChangePage{}, api.ErrLimit
		}
		out.Events = out.Events[:len(out.Events)-1]
		out.Checkpoint = out.Events[len(out.Events)-1].Seq
		out.Complete = false
	}
	return out, nil
}

func (s *Store) syncQueueForWorkItem(ctx context.Context, tx *sql.Tx, item api.WorkItem, actorRunID string, by api.Caller) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+queueEntryCols+` FROM queue_entries WHERE source_task_id=? AND item_id=? AND state IN ('waiting','claimed','active') ORDER BY seq`, item.TaskID, item.ID)
	if err != nil {
		return err
	}
	entries := []api.QueueEntry{}
	for rows.Next() {
		entry, e := scanQueueEntryFull(rows)
		if e != nil {
			rows.Close()
			return e
		}
		entries = append(entries, entry)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, entry := range entries {
		oldState, oldStale, oldCurrent := entry.State, entry.Stale, entry.CurrentItemRevision
		entry.CurrentItemRevision, entry.Item = item.Revision, queueSummary(item)
		kind, reason := "source_updated", ""
		switch item.Status {
		case "done":
			entry.State, entry.Eligible, entry.TerminalReason, entry.TerminalItemRevision = api.QueueStateCompleted, false, "source_done", item.Revision
			kind, reason = "completed", "source_done"
		case "dismissed":
			entry.State, entry.Eligible, entry.TerminalReason, entry.TerminalItemRevision = api.QueueStateCancelled, false, "source_dismissed", item.Revision
			kind, reason = "cancelled", "source_dismissed"
		default:
			entry.Stale = entry.OfferedItemRevision != item.Revision
			if entry.Stale {
				entry.Eligible = false
			}
		}
		if entry.State == oldState && entry.Stale == oldStale && entry.CurrentItemRevision == oldCurrent && entry.Item.Status == item.Status {
			continue
		}
		entry.Revision++
		entry.UpdatedAt = s.now()
		if err = updateQueueEntry(ctx, tx, entry); err != nil {
			return err
		}
		if terminalQueueState(entry.State) {
			if _, err = tx.ExecContext(ctx, `UPDATE queue_cycles SET final_state=?,closed_at=?,terminal_reason=? WHERE entry_id=? AND cycle=? AND final_state=''`, entry.State, ts(entry.UpdatedAt), entry.TerminalReason, entry.ID, entry.Cycle); err != nil {
				return err
			}
		}
		actor := api.Sender{AgentID: item.UpdatedBy.AgentID, Node: by.Node, User: by.User}
		event, e := appendQueueEvent(ctx, tx, entry, kind, reason, "", actorRunID, actor, entry.UpdatedAt, entry.UpdatedAt)
		if e != nil {
			return e
		}
		if _, e = s.createQueueNotification(ctx, tx, entry, event, actor, 0); e != nil {
			return e
		}
	}
	return nil
}

func (s *Store) cancelQueuesForProjectTx(ctx context.Context, tx *sql.Tx, taskID string, by api.Caller) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+queueEntryCols+` FROM queue_entries WHERE (target_task_id=? OR source_task_id=?) AND state IN ('waiting','claimed','active') ORDER BY seq`, taskID, taskID)
	if err != nil {
		return nil, err
	}
	entries := []api.QueueEntry{}
	for rows.Next() {
		entry, e := scanQueueEntryFull(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		entries = append(entries, entry)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	affected := map[string]bool{}
	for _, entry := range entries {
		affected[entry.TargetTaskID] = true
		reason := "source_project_closed"
		if entry.TargetTaskID == taskID {
			reason = "target_project_closed"
		}
		entry.State, entry.Eligible, entry.TerminalReason = api.QueueStateCancelled, false, reason
		entry.Revision++
		entry.UpdatedAt = s.now()
		if err = updateQueueEntry(ctx, tx, entry); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE queue_cycles SET final_state=?,closed_at=?,terminal_reason=? WHERE entry_id=? AND cycle=? AND final_state=''`, entry.State, ts(entry.UpdatedAt), reason, entry.ID, entry.Cycle); err != nil {
			return nil, err
		}
		actor := api.Sender{Node: by.Node, User: by.User}
		event, e := appendQueueEvent(ctx, tx, entry, "cancelled", reason, "", "", actor, entry.UpdatedAt, entry.UpdatedAt)
		if e != nil {
			return nil, e
		}
		if _, e = s.createQueueNotification(ctx, tx, entry, event, actor, 0); e != nil {
			return nil, e
		}
	}
	targets := make([]string, 0, len(affected))
	for targetID := range affected {
		targets = append(targets, targetID)
	}
	sort.Strings(targets)
	return targets, nil
}

// reconcileLegacyQueue imports only durable dispatch records. It emits no
// notifications, invents no claims and is safe to run on every store open.
func reconcileLegacyQueue(db *sql.DB, observed time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT d.id,d.item_id,d.item_revision,d.target_task_id,d.target_agent_id,d.message_seq,d.agent_id,d.by_node,d.by_user,d.created_at,w.task_id,w.kind,w.title,w.description,w.status,w.priority,w.revision,st.status,tt.status FROM work_item_dispatches d JOIN work_items w ON w.id=d.item_id JOIN tasks st ON st.id=w.task_id JOIN tasks tt ON tt.id=d.target_task_id LEFT JOIN queue_dispatch_links q ON q.dispatch_id=d.id WHERE q.dispatch_id IS NULL ORDER BY d.seq`)
	if err != nil {
		return err
	}
	type legacy struct {
		id, item, target, targetAgent, causalAgent, node, user, created, source, kind, title, description, status, priority, sourceStatus, targetStatus string
		revision, messageSeq, currentRevision                                                                                                           int64
	}
	all := []legacy{}
	for rows.Next() {
		var v legacy
		if err = rows.Scan(&v.id, &v.item, &v.revision, &v.target, &v.targetAgent, &v.messageSeq, &v.causalAgent, &v.node, &v.user, &v.created, &v.source, &v.kind, &v.title, &v.description, &v.status, &v.priority, &v.currentRevision, &v.sourceStatus, &v.targetStatus); err != nil {
			rows.Close()
			return err
		}
		all = append(all, v)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, v := range all {
		entry, e := getQueueEntryByItem(tx, context.Background(), v.target, v.source, v.item)
		created := errors.Is(e, api.ErrNotFound)
		if e != nil && !created {
			return e
		}
		at := parseTS(v.created)
		if created {
			state, terminal, reason := api.QueueStateWaiting, false, ""
			eligible, review := false, true
			if v.status == "done" {
				state, terminal, reason = api.QueueStateCompleted, true, "source_done"
			} else if v.status == "dismissed" {
				state, terminal, reason = api.QueueStateCancelled, true, "source_dismissed"
			} else if v.sourceStatus == api.TaskClosed || v.targetStatus == api.TaskClosed {
				state, terminal, reason = api.QueueStateCancelled, true, "project_closed"
			}
			entry = api.QueueEntry{ID: api.NewID("que"), TargetTaskID: v.target, SourceTaskID: v.source, ItemID: v.item, Cycle: 1, Revision: 1, State: state, QueuePriority: v.priority, Eligible: eligible, ReviewNeeded: review, OfferedItemRevision: v.revision, CurrentItemRevision: v.currentRevision, TerminalReason: reason, LegacyObserved: true, OrchestratorAgentID: v.targetAgent, FirstEnqueuedAt: at, LatestEnqueuedAt: at, UpdatedAt: observed, Item: api.QueueItemSummary{Kind: v.kind, Title: v.title, Description: v.description, Status: v.status, Priority: v.priority}}
			if terminal {
				entry.TerminalItemRevision = v.currentRevision
			}
			var agentID, runID, status string
			bindErr := tx.QueryRow(`SELECT a.id,a.run_id,a.status FROM agents a JOIN agent_work_item_bindings b ON b.agent_id=a.id AND b.run_id=a.run_id WHERE a.task_id=? AND b.item_task_id=? AND b.item_id=? AND b.item_revision=? AND a.status NOT IN ('closed','exited') ORDER BY b.created_at DESC LIMIT 1`, v.target, v.source, v.item, v.revision).Scan(&agentID, &runID, &status)
			if bindErr == nil && !terminal {
				entry.State, entry.Eligible, entry.ReviewNeeded, entry.WorkerAgentID, entry.WorkerRunID = api.QueueStateActive, false, false, agentID, runID
			}
			if err = insertQueueEntry(context.Background(), tx, &entry); err != nil {
				return err
			}
		} else {
			entry.LatestEnqueuedAt = at
			if v.revision > entry.OfferedItemRevision {
				entry.OfferedItemRevision = v.revision
				entry.Revision++
				entry.PendingUpdate = entry.State == api.QueueStateClaimed || entry.State == api.QueueStateActive
			}
			entry.UpdatedAt = observed
			entry.LegacyObserved = true
			if err = updateQueueEntry(context.Background(), tx, entry); err != nil {
				return err
			}
		}
		actor := api.Sender{AgentID: v.causalAgent, Node: v.node, User: v.user}
		event, e := appendQueueEvent(context.Background(), tx, entry, "legacy_dispatch_observed", "", v.id, "", actor, observed, at)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(`INSERT INTO queue_dispatch_links(dispatch_id,entry_id,cycle,event_seq,item_revision,message_seq,causal_agent_id,causal_node,causal_user,dispatch_created_at,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, v.id, entry.ID, entry.Cycle, event.Seq, v.revision, v.messageSeq, v.causalAgent, v.node, v.user, v.created, ts(observed)); e != nil {
			return e
		}
	}
	return tx.Commit()
}
