package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const deliveryCols = `id,task_id,message_seq,kind,agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_digest,generation,COALESCE(supersedes_delivery_id,''),current,phase,execution_epoch,COALESCE(current_block_id,''),result_text,created_at,updated_at`

func migrateDelivery(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS required_deliveries (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  message_seq INTEGER NOT NULL,
  kind TEXT NOT NULL,
  agent_id TEXT NOT NULL REFERENCES agents(id),
  run_id TEXT NOT NULL,
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  item_revision INTEGER NOT NULL,
  work_order_task_id TEXT NOT NULL,
  work_order_message_seq INTEGER NOT NULL,
  context_digest TEXT NOT NULL,
  generation INTEGER NOT NULL,
  supersedes_delivery_id TEXT,
  current INTEGER NOT NULL DEFAULT 1,
  phase TEXT NOT NULL,
  execution_epoch INTEGER NOT NULL DEFAULT 1,
  current_block_id TEXT,
  result_text TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(task_id,item_task_id,item_id,agent_id,run_id,generation)
);
CREATE UNIQUE INDEX IF NOT EXISTS required_deliveries_one_current
  ON required_deliveries(task_id,item_task_id,item_id,agent_id,run_id) WHERE current=1;
CREATE INDEX IF NOT EXISTS required_deliveries_recipient
  ON required_deliveries(task_id,agent_id,run_id,current);
CREATE TABLE IF NOT EXISTS delivery_blocks (
  id TEXT PRIMARY KEY,
  delivery_id TEXT NOT NULL REFERENCES required_deliveries(id),
  execution_epoch INTEGER NOT NULL,
  reason_class TEXT NOT NULL,
  text TEXT NOT NULL DEFAULT '',
  resolved INTEGER NOT NULL DEFAULT 0,
  resolution_id TEXT NOT NULL DEFAULT '',
  resolution_text TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  resolved_at TEXT,
  UNIQUE(delivery_id,execution_epoch)
);
CREATE TABLE IF NOT EXISTS delivery_events (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  delivery_id TEXT NOT NULL REFERENCES required_deliveries(id),
  sequence INTEGER NOT NULL,
  kind TEXT NOT NULL,
  agent_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL,
  data TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(delivery_id,sequence)
);
CREATE INDEX IF NOT EXISTS delivery_events_delivery ON delivery_events(delivery_id,sequence);
CREATE TABLE IF NOT EXISTS delivery_operation_receipts (
  receipt_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  operation TEXT NOT NULL,
  subject_id TEXT NOT NULL,
  actor_agent_id TEXT NOT NULL DEFAULT '',
  actor_run_id TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  request_id TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  response_json BLOB NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(task_id,operation,actor_agent_id,actor_run_id,by_node,by_user,request_id)
);`)
	return err
}

func validDeliveryID(id, prefix string) bool {
	if len(id) != len(prefix)+17 || !strings.HasPrefix(id, prefix+"_") {
		return false
	}
	for _, r := range id[len(prefix)+1:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func validDeliveryKind(kind string) bool {
	return kind == api.DeliveryAssignment || kind == api.DeliveryAmendment || kind == api.DeliveryReview
}

func scanRequiredDelivery(row interface{ Scan(...any) error }) (api.RequiredDelivery, error) {
	var d api.RequiredDelivery
	var current int
	var created, updated string
	err := row.Scan(&d.ID, &d.TaskID, &d.MessageSeq, &d.Kind, &d.AgentID, &d.RunID,
		&d.ItemTaskID, &d.ItemID, &d.ItemRevision, &d.WorkOrderMessage.TaskID,
		&d.WorkOrderMessage.Seq, &d.ContextDigest, &d.Generation, &d.SupersedesID,
		&current, &d.Phase, &d.ExecutionEpoch, &d.CurrentBlockID, &d.ResultText,
		&created, &updated)
	d.Current = current != 0
	d.CreatedAt, d.UpdatedAt = parseTS(created), parseTS(updated)
	return d, err
}

func getRequiredDeliveryRow(q queryRower, ctx context.Context, taskID, deliveryID string) (api.RequiredDelivery, error) {
	d, err := scanRequiredDelivery(q.QueryRowContext(ctx, `SELECT `+deliveryCols+` FROM required_deliveries WHERE task_id=? AND id=?`, taskID, deliveryID))
	if errors.Is(err, sql.ErrNoRows) {
		return d, api.ErrNotFound
	}
	return d, err
}

func currentRequiredDeliveryRow(q queryRower, ctx context.Context, taskID, itemTaskID, itemID, agentID, runID string) (api.RequiredDelivery, error) {
	d, err := scanRequiredDelivery(q.QueryRowContext(ctx, `SELECT `+deliveryCols+` FROM required_deliveries WHERE task_id=? AND item_task_id=? AND item_id=? AND agent_id=? AND run_id=? AND current=1`, taskID, itemTaskID, itemID, agentID, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return d, api.ErrNotFound
	}
	return d, err
}

func hydrateRequiredDelivery(q queryRower, ctx context.Context, d api.RequiredDelivery) (api.RequiredDelivery, error) {
	message, err := loadMessage(q, ctx, d.TaskID, d.MessageSeq)
	if err != nil {
		return d, err
	}
	d.Message = &message
	d.Events, err = loadDeliveryEvents(q, ctx, d.ID)
	if err != nil {
		return d, err
	}
	if d.CurrentBlockID != "" {
		block, blockErr := getDeliveryBlock(q, ctx, d.ID, d.CurrentBlockID)
		if blockErr != nil {
			return d, blockErr
		}
		d.CurrentBlock = &block
	}
	return d, nil
}

func currentRequiredDelivery(q queryRower, ctx context.Context, taskID, itemTaskID, itemID, agentID, runID string) (api.RequiredDelivery, error) {
	d, err := currentRequiredDeliveryRow(q, ctx, taskID, itemTaskID, itemID, agentID, runID)
	if err != nil {
		return d, err
	}
	return hydrateRequiredDelivery(q, ctx, d)
}

func loadDeliveryEvents(q queryRower, ctx context.Context, deliveryID string) ([]api.DeliveryEvent, error) {
	rows, err := q.QueryContext(ctx, `SELECT id,task_id,delivery_id,sequence,kind,agent_id,run_id,request_id,data,by_node,by_user,created_at FROM delivery_events WHERE delivery_id=? ORDER BY sequence`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []api.DeliveryEvent
	for rows.Next() {
		var event api.DeliveryEvent
		var raw, created string
		if err = rows.Scan(&event.ID, &event.TaskID, &event.DeliveryID, &event.Sequence, &event.Kind, &event.AgentID, &event.RunID, &event.RequestID, &raw, &event.By.Node, &event.By.User, &created); err != nil {
			return nil, err
		}
		event.CreatedAt = parseTS(created)
		if raw != "" {
			if err = json.Unmarshal([]byte(raw), &event.Data); err != nil {
				return nil, err
			}
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func scanDeliveryBlock(row interface{ Scan(...any) error }) (api.DeliveryBlock, error) {
	var b api.DeliveryBlock
	var resolved int
	var created string
	var resolvedAt sql.NullString
	err := row.Scan(&b.ID, &b.DeliveryID, &b.ExecutionEpoch, &b.ReasonClass, &b.Text,
		&resolved, &b.ResolutionID, &b.ResolutionText, &created, &resolvedAt)
	b.Resolved = resolved != 0
	b.CreatedAt = parseTS(created)
	if resolvedAt.Valid {
		t := parseTS(resolvedAt.String)
		b.ResolvedAt = &t
	}
	return b, err
}

func getDeliveryBlock(q queryRower, ctx context.Context, deliveryID, blockID string) (api.DeliveryBlock, error) {
	b, err := scanDeliveryBlock(q.QueryRowContext(ctx, `SELECT id,delivery_id,execution_epoch,reason_class,text,resolved,resolution_id,resolution_text,created_at,resolved_at FROM delivery_blocks WHERE delivery_id=? AND id=?`, deliveryID, blockID))
	if errors.Is(err, sql.ErrNoRows) {
		return b, api.ErrNotFound
	}
	return b, err
}

func hasDeliveryItemLink(message api.Message, taskID, itemID string, revision int64) bool {
	for _, link := range message.WorkItems {
		if link.ItemTaskID == taskID && link.ItemID == itemID && link.ItemRevision == revision && link.Relationship == "primary" {
			return true
		}
	}
	return false
}

func validateDeliveryProducer(q queryRower, ctx context.Context, taskID, agentID, runID string) error {
	if agentID == "" && runID == "" {
		return nil // Human UI/API actor under the existing shared-workspace credential.
	}
	if !api.ValidID(agentID, "agt") || !validRunID(runID) {
		return api.ErrInvalid
	}
	var currentRun, status, role, name, orchestrator string
	err := q.QueryRowContext(ctx, `SELECT a.run_id,a.status,a.role,a.name,t.orchestrator FROM agents a JOIN tasks t ON t.id=a.task_id WHERE a.task_id=? AND a.id=?`, taskID, agentID).Scan(&currentRun, &status, &role, &name, &orchestrator)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ErrNotFound
	}
	if err != nil {
		return err
	}
	if currentRun != runID || status == api.AgentRetired || status == api.AgentClosed || status == api.AgentExited || (role != "database_handler" && name != orchestrator) {
		return workItemConflict("producer is not the exact current project lead or database handler run")
	}
	return nil
}

func findDeliveryReplay(q queryRower, ctx context.Context, taskID, operation, subjectID, actorAgentID, actorRunID, requestID, payload string, by api.Caller) (api.DeliveryMutation, error) {
	var out api.DeliveryMutation
	var priorSubject, priorHash string
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT subject_id,payload_hash,response_json FROM delivery_operation_receipts WHERE task_id=? AND operation=? AND actor_agent_id=? AND actor_run_id=? AND by_node=? AND by_user=? AND request_id=?`,
		taskID, operation, actorAgentID, actorRunID, by.Node, by.User, requestID).Scan(&priorSubject, &priorHash, &raw)
	if err == nil {
		if priorSubject != subjectID || priorHash != payload {
			return out, workItemConflict("request ID was already used with different delivery data")
		}
		if err = json.Unmarshal(raw, &out); err != nil {
			return out, err
		}
		out.Replay = true
		return out, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return out, api.ErrNotFound
	}
	return out, err
}

func insertDeliveryReceipt(ctx context.Context, tx *sql.Tx, taskID, operation, subjectID, actorAgentID, actorRunID, requestID, payload string, by api.Caller, out *api.DeliveryMutation) error {
	out.Receipt = api.DeliveryReceipt{ID: api.NewID("drr"), RequestID: requestID, Operation: operation, CreatedAt: out.Event.CreatedAt}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO delivery_operation_receipts(receipt_id,task_id,operation,subject_id,actor_agent_id,actor_run_id,by_node,by_user,request_id,payload_hash,response_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		out.Receipt.ID, taskID, operation, subjectID, actorAgentID, actorRunID, by.Node, by.User, requestID, payload, raw, ts(out.Receipt.CreatedAt))
	return err
}

func insertDeliveryEvent(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, kind, requestID, actorAgentID, actorRunID string, data map[string]any, by api.Caller, now time.Time) (api.DeliveryEvent, error) {
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM delivery_events WHERE delivery_id=?`, d.ID).Scan(&sequence); err != nil {
		return api.DeliveryEvent{}, err
	}
	e := api.DeliveryEvent{ID: api.NewID("dlev"), TaskID: d.TaskID, DeliveryID: d.ID, Sequence: sequence, Kind: kind, AgentID: actorAgentID, RunID: actorRunID, RequestID: requestID, Data: data, By: by, CreatedAt: now}
	raw := ""
	if len(data) > 0 {
		b, err := json.Marshal(data)
		if err != nil {
			return e, err
		}
		raw = string(b)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO delivery_events(id,task_id,delivery_id,sequence,kind,agent_id,run_id,request_id,data,by_node,by_user,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.TaskID, e.DeliveryID, e.Sequence, e.Kind, e.AgentID, e.RunID, e.RequestID, raw, by.Node, by.User, ts(now))
	return e, err
}

// CreateRequiredDelivery attaches an immutable, already-stored message to the
// exact current item-worker binding. Message lookup, item/run validation,
// generation CAS, supersession, event, and keyed receipt commit atomically.
func (s *Store) CreateRequiredDelivery(ctx context.Context, taskID string, req api.CreateRequiredDeliveryRequest, by api.Caller) (api.DeliveryMutation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out api.DeliveryMutation
	if !api.ValidID(taskID, "tsk") || !validRequestID(req.RequestID) || req.MessageSeq < 1 || !validDeliveryKind(req.Kind) ||
		!api.ValidID(req.AgentID, "agt") || !validRunID(req.RunID) || !api.ValidID(req.ItemTaskID, "tsk") ||
		!api.ValidID(req.ItemID, "wi") || req.ItemRevision < 1 || req.WorkOrderMessage.TaskID != req.ItemTaskID ||
		req.WorkOrderMessage.Seq < 1 || req.ExpectedCurrentGeneration < 0 ||
		((req.ProducerAgentID == "") != (req.ProducerRunID == "")) {
		return out, api.ErrInvalid
	}
	payload := requestHash(req)
	subject := req.ItemID + ":" + req.AgentID + ":" + req.RunID
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if replay, replayErr := findDeliveryReplay(tx, ctx, taskID, "create", subject, req.ProducerAgentID, req.ProducerRunID, req.RequestID, payload, by); replayErr == nil {
		return replay, nil
	} else if !errors.Is(replayErr, api.ErrNotFound) {
		return out, replayErr
	}
	if err = validateDeliveryProducer(tx, ctx, taskID, req.ProducerAgentID, req.ProducerRunID); err != nil {
		return out, err
	}
	var taskStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=?`, taskID).Scan(&taskStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, api.ErrNotFound
		}
		return out, err
	}
	if taskStatus != api.TaskOpen {
		return out, api.ErrClosed
	}
	message, err := loadMessage(tx, ctx, taskID, req.MessageSeq)
	if err != nil || !hasDeliveryItemLink(message, req.ItemTaskID, req.ItemID, req.ItemRevision) {
		if err != nil {
			return out, err
		}
		return out, workItemConflict("directive message is not immutably linked to the exact item revision")
	}
	order, err := loadMessage(tx, ctx, req.WorkOrderMessage.TaskID, req.WorkOrderMessage.Seq)
	if err != nil || !hasDeliveryItemLink(order, req.ItemTaskID, req.ItemID, req.ItemRevision) {
		if err != nil {
			return out, err
		}
		return out, workItemConflict("work-order message is not immutably linked to the exact item revision")
	}
	item, err := getWorkItem(tx, ctx, req.ItemTaskID, req.ItemID)
	if err != nil {
		return out, err
	}
	if item.Revision != req.ItemRevision || item.Status == "done" || item.Status == "dismissed" {
		return out, workItemConflict("work item revision or status changed before directive creation")
	}
	var agentTask, currentRun, agentStatus string
	if err = tx.QueryRowContext(ctx, `SELECT task_id,run_id,status FROM agents WHERE id=?`, req.AgentID).Scan(&agentTask, &currentRun, &agentStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, api.ErrNotFound
		}
		return out, err
	}
	if agentTask != taskID || currentRun != req.RunID || agentStatus == api.AgentClosed || agentStatus == api.AgentExited {
		return out, workItemConflict("recipient is not the exact current actionable run")
	}
	binding, err := loadAgentWorkItemBinding(tx, ctx, req.AgentID, req.RunID)
	if err != nil {
		return out, err
	}
	if binding == nil || binding.ItemTaskID != req.ItemTaskID || binding.ItemID != req.ItemID || binding.ItemRevision != req.ItemRevision || binding.WorkOrderMessage != req.WorkOrderMessage {
		return out, workItemConflict("recipient binding does not match the directive item revision")
	}
	prior, priorErr := currentRequiredDeliveryRow(tx, ctx, taskID, req.ItemTaskID, req.ItemID, req.AgentID, req.RunID)
	if errors.Is(priorErr, api.ErrNotFound) {
		if req.ExpectedCurrentGeneration != 0 || req.ExpectedCurrentDeliveryID != "" {
			return out, workItemConflict("directive generation changed; no current directive exists")
		}
	} else if priorErr != nil {
		return out, priorErr
	} else if req.ExpectedCurrentDeliveryID == "" {
		return out, workItemConflict("expected current delivery ID is required when superseding a directive")
	} else if prior.Generation != req.ExpectedCurrentGeneration || prior.ID != req.ExpectedCurrentDeliveryID {
		return out, workItemConflict(fmt.Sprintf("directive generation changed; current delivery is %s generation %d", prior.ID, prior.Generation))
	}
	now := s.now()
	generation := req.ExpectedCurrentGeneration + 1
	d := api.RequiredDelivery{ID: api.NewID("dly"), TaskID: taskID, MessageSeq: req.MessageSeq, Kind: req.Kind,
		AgentID: req.AgentID, RunID: req.RunID, ItemTaskID: req.ItemTaskID, ItemID: req.ItemID, ItemRevision: req.ItemRevision,
		WorkOrderMessage: req.WorkOrderMessage, ContextDigest: binding.ContextDigest, Generation: generation, Current: true,
		Phase: api.DeliveryUnacknowledged, ExecutionEpoch: 1, CreatedAt: now, UpdatedAt: now}
	d.Message = &message
	if priorErr == nil {
		d.SupersedesID = prior.ID
		phase := prior.Phase
		if phase != api.DeliveryResult {
			phase = api.DeliverySuperseded
		}
		if _, err = tx.ExecContext(ctx, `UPDATE required_deliveries SET current=0,phase=?,updated_at=? WHERE id=? AND current=1 AND generation=?`, phase, ts(now), prior.ID, prior.Generation); err != nil {
			return out, err
		}
		if _, err = insertDeliveryEvent(ctx, tx, prior, "superseded", req.RequestID, req.ProducerAgentID, req.ProducerRunID, map[string]any{"byDeliveryId": d.ID, "generation": generation}, by, now); err != nil {
			return out, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO required_deliveries(id,task_id,message_seq,kind,agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_digest,generation,supersedes_delivery_id,current,phase,execution_epoch,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.TaskID, d.MessageSeq, d.Kind, d.AgentID, d.RunID, d.ItemTaskID, d.ItemID, d.ItemRevision,
		d.WorkOrderMessage.TaskID, d.WorkOrderMessage.Seq, d.ContextDigest, d.Generation, nullable(d.SupersedesID), 1, d.Phase, d.ExecutionEpoch, ts(now), ts(now))
	if err != nil {
		return out, err
	}
	out.Delivery = d
	out.Event, err = insertDeliveryEvent(ctx, tx, d, "stored", req.RequestID, req.ProducerAgentID, req.ProducerRunID, map[string]any{"messageSeq": d.MessageSeq, "generation": d.Generation}, by, now)
	if err != nil {
		return out, err
	}
	out.Delivery.Events = append(out.Delivery.Events, out.Event)
	if err = insertDeliveryReceipt(ctx, tx, taskID, "create", subject, req.ProducerAgentID, req.ProducerRunID, req.RequestID, payload, by, &out); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.notify(taskID)
	return out, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Store) CurrentAssignment(ctx context.Context, taskID, agentID, runID string) (api.RequiredDelivery, error) {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(agentID, "agt") || !validRunID(runID) {
		return api.RequiredDelivery{}, api.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.RequiredDelivery{}, err
	}
	defer tx.Rollback()
	var task, currentRun, status string
	if err = tx.QueryRowContext(ctx, `SELECT task_id,run_id,status FROM agents WHERE id=?`, agentID).Scan(&task, &currentRun, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return api.RequiredDelivery{}, api.ErrNotFound
		}
		return api.RequiredDelivery{}, err
	}
	if task != taskID {
		return api.RequiredDelivery{}, api.ErrNotFound
	}
	if currentRun != runID {
		return api.RequiredDelivery{}, workItemConflict("stale run; refresh the exact current assignment")
	}
	if status == api.AgentClosed || status == api.AgentExited {
		return api.RequiredDelivery{}, workItemConflict("closed or exited run has no actionable current assignment")
	}
	binding, err := loadAgentWorkItemBinding(tx, ctx, agentID, runID)
	if err != nil {
		return api.RequiredDelivery{}, err
	}
	if binding == nil {
		return api.RequiredDelivery{}, api.ErrNotFound
	}
	d, err := currentRequiredDelivery(tx, ctx, taskID, binding.ItemTaskID, binding.ItemID, agentID, runID)
	if err != nil {
		return api.RequiredDelivery{}, err
	}
	if err = validateDeliveryBinding(tx, ctx, d); err != nil {
		return api.RequiredDelivery{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.RequiredDelivery{}, err
	}
	return d, nil
}

func validateDeliveryBinding(q queryRower, ctx context.Context, d api.RequiredDelivery) error {
	binding, err := loadAgentWorkItemBinding(q, ctx, d.AgentID, d.RunID)
	if err != nil {
		return err
	}
	if binding == nil || binding.ItemTaskID != d.ItemTaskID || binding.ItemID != d.ItemID ||
		binding.ItemRevision != d.ItemRevision || binding.WorkOrderMessage != d.WorkOrderMessage ||
		binding.ContextDigest != d.ContextDigest {
		return workItemConflict("delivery no longer matches the exact saved item/run/order/context binding")
	}
	return nil
}

type deliveryMutator func(context.Context, *sql.Tx, api.RequiredDelivery, time.Time) (string, map[string]any, *api.DeliveryBlock, error)

func (s *Store) mutateWorkerDelivery(ctx context.Context, taskID, deliveryID, operation, requestID, agentID, runID string, request any, by api.Caller, fn deliveryMutator) (api.DeliveryMutation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out api.DeliveryMutation
	if !api.ValidID(taskID, "tsk") || !validDeliveryID(deliveryID, "dly") || !validRequestID(requestID) || !api.ValidID(agentID, "agt") || !validRunID(runID) {
		return out, api.ErrInvalid
	}
	payload := requestHash(request)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if replay, replayErr := findDeliveryReplay(tx, ctx, taskID, operation, deliveryID, agentID, runID, requestID, payload, by); replayErr == nil {
		return replay, nil
	} else if !errors.Is(replayErr, api.ErrNotFound) {
		return out, replayErr
	}
	d, err := getRequiredDeliveryRow(tx, ctx, taskID, deliveryID)
	if err != nil {
		return out, err
	}
	if err = validateDeliveryBinding(tx, ctx, d); err != nil {
		return out, err
	}
	if d.AgentID != agentID || d.RunID != runID || !d.Current || d.Phase == api.DeliverySuperseded {
		current, _ := currentRequiredDeliveryRow(tx, ctx, taskID, d.ItemTaskID, d.ItemID, d.AgentID, d.RunID)
		return out, workItemConflict("superseded or wrong-run delivery; current delivery is " + current.ID)
	}
	var currentRun, status string
	if err = tx.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE id=? AND task_id=?`, agentID, taskID).Scan(&currentRun, &status); err != nil {
		return out, err
	}
	if currentRun != runID {
		return out, workItemConflict("stale run; refresh the exact current assignment")
	}
	if status == api.AgentRetired || status == api.AgentClosed || status == api.AgentExited {
		return out, workItemConflict("agent lifecycle state does not permit directive execution")
	}
	now := s.now()
	kind, data, block, err := fn(ctx, tx, d, now)
	if err != nil {
		return out, err
	}
	d, err = getRequiredDeliveryRow(tx, ctx, taskID, deliveryID)
	if err != nil {
		return out, err
	}
	out.Delivery, out.Block = d, block
	out.Event, err = insertDeliveryEvent(ctx, tx, d, kind, requestID, agentID, runID, data, by, now)
	if err != nil {
		return out, err
	}
	out.Delivery.Events = append(out.Delivery.Events, out.Event)
	if err = insertDeliveryReceipt(ctx, tx, taskID, operation, deliveryID, agentID, runID, requestID, payload, by, &out); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.notify(taskID)
	return out, nil
}

func (s *Store) AcknowledgeDelivery(ctx context.Context, taskID, deliveryID string, req api.DeliveryActionRequest, by api.Caller) (api.DeliveryMutation, error) {
	if req.ExpectedEpoch < 1 {
		return api.DeliveryMutation{}, api.ErrInvalid
	}
	return s.mutateWorkerDelivery(ctx, taskID, deliveryID, "ack", req.RequestID, req.AgentID, req.RunID, req, by, func(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, now time.Time) (string, map[string]any, *api.DeliveryBlock, error) {
		if req.ExpectedEpoch != d.ExecutionEpoch || d.Phase != api.DeliveryUnacknowledged {
			return "", nil, nil, workItemConflict("stale epoch or invalid directive transition to acknowledged from " + d.Phase)
		}
		_, err := tx.ExecContext(ctx, `UPDATE required_deliveries SET phase=?,updated_at=? WHERE id=?`, api.DeliveryAcknowledged, ts(now), d.ID)
		return "acknowledged", map[string]any{"executionEpoch": d.ExecutionEpoch}, nil, err
	})
}

func (s *Store) ProgressDelivery(ctx context.Context, taskID, deliveryID string, req api.DeliveryActionRequest, by api.Caller) (api.DeliveryMutation, error) {
	if req.ExpectedEpoch < 1 || !api.ValidText(req.Text, api.MaxTextLen) {
		return api.DeliveryMutation{}, api.ErrInvalid
	}
	return s.mutateWorkerDelivery(ctx, taskID, deliveryID, "progress", req.RequestID, req.AgentID, req.RunID, req, by, func(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, now time.Time) (string, map[string]any, *api.DeliveryBlock, error) {
		if req.ExpectedEpoch != d.ExecutionEpoch || (d.Phase != api.DeliveryAcknowledged && d.Phase != api.DeliveryProgressing) {
			return "", nil, nil, workItemConflict("stale epoch or invalid directive progress transition from " + d.Phase)
		}
		_, err := tx.ExecContext(ctx, `UPDATE required_deliveries SET phase=?,updated_at=? WHERE id=?`, api.DeliveryProgressing, ts(now), d.ID)
		return "progress", map[string]any{"text": req.Text, "executionEpoch": d.ExecutionEpoch}, nil, err
	})
}

func (s *Store) BlockDelivery(ctx context.Context, taskID, deliveryID string, req api.DeliveryBlockRequest, by api.Caller) (api.DeliveryMutation, error) {
	if req.ExpectedEpoch < 1 || !validBlockReason(req.ReasonClass) || !api.ValidText(req.Text, api.MaxTextLen) {
		return api.DeliveryMutation{}, api.ErrInvalid
	}
	return s.mutateWorkerDelivery(ctx, taskID, deliveryID, "block", req.RequestID, req.AgentID, req.RunID, req, by, func(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, now time.Time) (string, map[string]any, *api.DeliveryBlock, error) {
		if req.ExpectedEpoch != d.ExecutionEpoch || (d.Phase != api.DeliveryAcknowledged && d.Phase != api.DeliveryProgressing) {
			return "", nil, nil, workItemConflict("stale epoch or invalid directive block transition")
		}
		b := &api.DeliveryBlock{ID: api.NewID("dblk"), DeliveryID: d.ID, ExecutionEpoch: d.ExecutionEpoch, ReasonClass: req.ReasonClass, Text: req.Text, CreatedAt: now}
		if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_blocks(id,delivery_id,execution_epoch,reason_class,text,created_at) VALUES(?,?,?,?,?,?)`, b.ID, b.DeliveryID, b.ExecutionEpoch, b.ReasonClass, b.Text, ts(now)); err != nil {
			return "", nil, nil, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE required_deliveries SET phase=?,current_block_id=?,updated_at=? WHERE id=?`, api.DeliveryBlocked, b.ID, ts(now), d.ID)
		return "blocked", map[string]any{"blockId": b.ID, "reasonClass": b.ReasonClass, "executionEpoch": b.ExecutionEpoch}, b, err
	})
}

func validBlockReason(reason string) bool {
	switch reason {
	case "transient", "dependency", "permission", "authentication", "tool", "owner_input":
		return true
	}
	return false
}

func (s *Store) ResultDelivery(ctx context.Context, taskID, deliveryID string, req api.DeliveryActionRequest, by api.Caller) (api.DeliveryMutation, error) {
	if req.ExpectedEpoch < 1 || strings.TrimSpace(req.Text) == "" || !api.ValidText(req.Text, api.MaxTextLen) {
		return api.DeliveryMutation{}, api.ErrInvalid
	}
	return s.mutateWorkerDelivery(ctx, taskID, deliveryID, "result", req.RequestID, req.AgentID, req.RunID, req, by, func(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, now time.Time) (string, map[string]any, *api.DeliveryBlock, error) {
		if req.ExpectedEpoch != d.ExecutionEpoch || (d.Phase != api.DeliveryAcknowledged && d.Phase != api.DeliveryProgressing) {
			return "", nil, nil, workItemConflict("stale epoch or invalid directive result transition from " + d.Phase)
		}
		_, err := tx.ExecContext(ctx, `UPDATE required_deliveries SET phase=?,result_text=?,updated_at=? WHERE id=?`, api.DeliveryResult, req.Text, ts(now), d.ID)
		return "result", map[string]any{"text": req.Text, "executionEpoch": d.ExecutionEpoch}, nil, err
	})
}

func (s *Store) ResolveDeliveryBlock(ctx context.Context, taskID, deliveryID, blockID string, req api.DeliveryResolutionRequest, by api.Caller) (api.DeliveryMutation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out api.DeliveryMutation
	if !api.ValidID(taskID, "tsk") || !validDeliveryID(deliveryID, "dly") || !validDeliveryID(blockID, "dblk") || !validRequestID(req.RequestID) || req.ExpectedEpoch < 1 || strings.TrimSpace(req.Text) == "" || !api.ValidText(req.Text, api.MaxTextLen) || ((req.AgentID == "") != (req.RunID == "")) {
		return out, api.ErrInvalid
	}
	payload := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if replay, replayErr := findDeliveryReplay(tx, ctx, taskID, "resolve", blockID, req.AgentID, req.RunID, req.RequestID, payload, by); replayErr == nil {
		return replay, nil
	} else if !errors.Is(replayErr, api.ErrNotFound) {
		return out, replayErr
	}
	d, err := getRequiredDeliveryRow(tx, ctx, taskID, deliveryID)
	if err != nil {
		return out, err
	}
	if err = validateDeliveryBinding(tx, ctx, d); err != nil {
		return out, err
	}
	var recipientRun, recipientStatus string
	if err = tx.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE id=? AND task_id=?`, d.AgentID, taskID).Scan(&recipientRun, &recipientStatus); err != nil {
		return out, err
	}
	if recipientRun != d.RunID || recipientStatus == api.AgentClosed || recipientStatus == api.AgentExited {
		return out, workItemConflict("directive recipient is no longer the exact current retained run")
	}
	b, err := getDeliveryBlock(tx, ctx, deliveryID, blockID)
	if err != nil {
		return out, err
	}
	if !d.Current || d.Phase != api.DeliveryBlocked || d.CurrentBlockID != blockID || b.Resolved || b.ExecutionEpoch != req.ExpectedEpoch {
		return out, workItemConflict("block is not the unresolved current directive epoch")
	}
	if err = validateResolutionActor(tx, ctx, d, b, req.AgentID, req.RunID); err != nil {
		return out, err
	}
	now := s.now()
	b.Resolved, b.ResolutionID, b.ResolutionText = true, api.NewID("drsl"), req.Text
	b.ResolvedAt = &now
	if _, err = tx.ExecContext(ctx, `UPDATE delivery_blocks SET resolved=1,resolution_id=?,resolution_text=?,resolved_at=? WHERE id=? AND resolved=0`, b.ResolutionID, b.ResolutionText, ts(now), b.ID); err != nil {
		return out, err
	}
	out.Delivery, out.Block = d, &b
	out.Event, err = insertDeliveryEvent(ctx, tx, d, "block_resolved", req.RequestID, req.AgentID, req.RunID, map[string]any{"blockId": b.ID, "resolutionId": b.ResolutionID, "executionEpoch": b.ExecutionEpoch}, by, now)
	if err != nil {
		return out, err
	}
	out.Delivery.Events = append(out.Delivery.Events, out.Event)
	if err = insertDeliveryReceipt(ctx, tx, taskID, "resolve", blockID, req.AgentID, req.RunID, req.RequestID, payload, by, &out); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.notify(taskID)
	return out, nil
}

func validateResolutionActor(q queryRower, ctx context.Context, d api.RequiredDelivery, b api.DeliveryBlock, agentID, runID string) error {
	if agentID == "" && runID == "" {
		return nil
	}
	if !api.ValidID(agentID, "agt") || !validRunID(runID) {
		return api.ErrInvalid
	}
	var currentRun, status, role, name, orchestrator string
	if err := q.QueryRowContext(ctx, `SELECT a.run_id,a.status,a.role,a.name,t.orchestrator FROM agents a JOIN tasks t ON t.id=a.task_id WHERE a.task_id=? AND a.id=?`, d.TaskID, agentID).Scan(&currentRun, &status, &role, &name, &orchestrator); err != nil {
		return err
	}
	if currentRun != runID || status == api.AgentRetired || status == api.AgentClosed || status == api.AgentExited {
		return workItemConflict("resolution actor is not a current actionable run")
	}
	if agentID == d.AgentID && runID == d.RunID && (b.ReasonClass == "transient" || b.ReasonClass == "tool") {
		return nil
	}
	if role == "database_handler" || name == orchestrator {
		return nil
	}
	return workItemConflict("resolution actor is not authorized for this block class")
}

func (s *Store) ResumeDelivery(ctx context.Context, taskID, deliveryID string, req api.DeliveryResumeRequest, by api.Caller) (api.DeliveryMutation, error) {
	if req.ExpectedEpoch < 1 || !validDeliveryID(req.ResolutionID, "drsl") {
		return api.DeliveryMutation{}, api.ErrInvalid
	}
	return s.mutateWorkerDelivery(ctx, taskID, deliveryID, "resume", req.RequestID, req.AgentID, req.RunID, req, by, func(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, now time.Time) (string, map[string]any, *api.DeliveryBlock, error) {
		if d.Phase != api.DeliveryBlocked || d.ExecutionEpoch != req.ExpectedEpoch || d.CurrentBlockID == "" {
			return "", nil, nil, workItemConflict("stale epoch or directive is not blocked")
		}
		b, err := getDeliveryBlock(tx, ctx, d.ID, d.CurrentBlockID)
		if err != nil {
			return "", nil, nil, err
		}
		if !b.Resolved || b.ResolutionID != req.ResolutionID {
			return "", nil, nil, workItemConflict("current block has not been resolved with the supplied resolution")
		}
		newEpoch := d.ExecutionEpoch + 1
		_, err = tx.ExecContext(ctx, `UPDATE required_deliveries SET phase=?,execution_epoch=?,current_block_id=NULL,updated_at=? WHERE id=?`, api.DeliveryAcknowledged, newEpoch, ts(now), d.ID)
		return "resumed", map[string]any{"blockId": b.ID, "resolutionId": b.ResolutionID, "executionEpoch": newEpoch}, &b, err
	})
}
