package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/scs32/tailterm/hub/internal/api"
)

func migrateWorkOrderScope(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS work_order_scope_confirmations (
		task_id TEXT NOT NULL, item_id TEXT NOT NULL, item_revision INTEGER NOT NULL,
		scope_revision INTEGER NOT NULL, order_seq INTEGER NOT NULL, source_message_seq INTEGER NOT NULL DEFAULT 0, request_id TEXT NOT NULL,
		payload_hash TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL, created_at TEXT NOT NULL,
		PRIMARY KEY(task_id,item_id,item_revision,order_seq), UNIQUE(task_id,request_id),
		FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id));
	CREATE TABLE IF NOT EXISTS work_order_bookkeeping (
		id TEXT PRIMARY KEY, task_id TEXT NOT NULL, item_id TEXT NOT NULL,
		request_id TEXT NOT NULL, payload_hash TEXT NOT NULL, receipt_json TEXT NOT NULL,
		created_at TEXT NOT NULL, UNIQUE(task_id,request_id),
		FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id));`)
	return err
}

func requireScopeHandler(q queryRower, ctx context.Context, task, agent, run string) error {
	if !api.ValidID(agent, "agt") || !validRunID(run) {
		return api.ErrInvalid
	}
	var actualRun, role, status string
	if err := q.QueryRowContext(ctx, `SELECT run_id,role,status FROM agents WHERE task_id=? AND id=?`, task, agent).Scan(&actualRun, &role, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workItemConflict("an available database handler is required")
		}
		return err
	}
	if actualRun != run || role != api.AgentRoleDatabaseHandler || status == api.AgentClosed || status == api.AgentExited || status == api.AgentRetired {
		return workItemConflict("the exact available database handler run must file scope evidence")
	}
	return nil
}

func requireConfirmedTeamOrder(ctx context.Context, q queryRower, task, item string, revision, order int64) error {
	var scope, currentScope int64
	err := q.QueryRowContext(ctx, `SELECT scope_revision FROM work_order_scope_confirmations WHERE task_id=? AND item_id=? AND item_revision=? AND order_seq=?`, task, item, revision, order).Scan(&scope)
	if errors.Is(err, sql.ErrNoRows) {
		return workItemConflict("scope is not confirmed for this exact item revision and order; ask the database handler to complete intake and confirm the owner-filed scope before queueing or launching")
	}
	if err != nil {
		return err
	}
	if err := q.QueryRowContext(ctx, `SELECT narrative_scope_revision FROM work_items WHERE task_id=? AND id=? AND revision=?`, task, item, revision).Scan(&currentScope); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return workItemConflict("scope confirmation is stale; ask the database handler to confirm the current item revision")
	}
	if currentScope != scope {
		return workItemConflict("scope confirmation is stale; ask the database handler to confirm the current item revision")
	}
	return nil
}

func requireCurrentConfirmedTeamOrder(ctx context.Context, q queryRower, task, item string, revision, order int64) error {
	var current int64
	if err := q.QueryRowContext(ctx, `SELECT revision FROM work_items WHERE task_id=? AND id=?`, task, item).Scan(&current); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return workItemConflict("item revision changed since queue admission; refresh the order before launch")
	}
	if current != revision {
		return workItemConflict("item revision changed since queue admission; refresh the order before launch")
	}
	return requireConfirmedTeamOrder(ctx, q, task, item, revision, order)
}

func (s *Store) ConfirmWorkOrderScope(ctx context.Context, task, itemID string, req api.ConfirmWorkOrderScopeRequest) (api.WorkOrderScopeConfirmation, error) {
	if !api.ValidID(task, "tsk") || !api.ValidID(itemID, "wi") || !validRequestID(req.RequestID) || req.ExpectedRevision < 1 || req.ScopeRevision < 1 || req.OrderMessageSeq < 1 {
		return api.WorkOrderScopeConfirmation{}, api.ErrInvalid
	}
	if !req.Complete {
		return api.WorkOrderScopeConfirmation{}, workItemConflict("owner filing is incomplete; record acceptance and owned files in the item before confirming scope")
	}
	hash := requestHash(struct {
		Task, Item string
		Request    api.ConfirmWorkOrderScopeRequest
	}{task, itemID, req})
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.WorkOrderScopeConfirmation{}, err
	}
	defer tx.Rollback()
	var prior api.WorkOrderScopeConfirmation
	var priorHash string
	err = tx.QueryRowContext(ctx, `SELECT item_id,item_revision,scope_revision,order_seq,source_message_seq,agent_id,run_id,created_at,payload_hash FROM work_order_scope_confirmations WHERE task_id=? AND request_id=?`, task, req.RequestID).Scan(&prior.ItemID, &prior.ItemRevision, &prior.ScopeRevision, &prior.OrderSeq, &prior.SourceMessageSeq, &prior.AgentID, &prior.RunID, &prior.CreatedAt, &priorHash)
	if err == nil {
		if priorHash != hash {
			return api.WorkOrderScopeConfirmation{}, workItemConflict("confirmation retry payload changed")
		}
		prior.TaskID, prior.RequestID = task, req.RequestID
		return prior, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return api.WorkOrderScopeConfirmation{}, err
	}
	if err := requireScopeHandler(tx, ctx, task, req.AgentID, req.RunID); err != nil {
		return api.WorkOrderScopeConfirmation{}, err
	}
	item, err := getWorkItem(tx, ctx, task, itemID)
	if err != nil {
		return api.WorkOrderScopeConfirmation{}, err
	}
	if item.Revision != req.ExpectedRevision || item.ScopeRevision != req.ScopeRevision || item.Status == "done" || item.Status == "dismissed" {
		return api.WorkOrderScopeConfirmation{}, workItemConflict("item or scope revision changed; complete intake against the current item")
	}
	if err := recordedTeamOrder(ctx, tx, task, itemID, item.Revision, req.OrderMessageSeq); err != nil {
		return api.WorkOrderScopeConfirmation{}, err
	}
	// A second request cannot quietly replace the handler's filed assertion.
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT request_id FROM work_order_scope_confirmations WHERE task_id=? AND item_id=? AND item_revision=? AND order_seq=?`, task, itemID, item.Revision, req.OrderMessageSeq).Scan(&existing)
	if err == nil {
		return api.WorkOrderScopeConfirmation{}, workItemConflict("this revision and order already have a different scope confirmation")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return api.WorkOrderScopeConfirmation{}, err
	}
	out := api.WorkOrderScopeConfirmation{TaskID: task, ItemID: itemID, ItemRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderSeq: req.OrderMessageSeq, SourceMessageSeq: item.SourceMessageSeq, RequestID: req.RequestID, AgentID: req.AgentID, RunID: req.RunID, CreatedAt: ts(s.now())}
	_, err = tx.ExecContext(ctx, `INSERT INTO work_order_scope_confirmations(task_id,item_id,item_revision,scope_revision,order_seq,source_message_seq,request_id,payload_hash,agent_id,run_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, task, itemID, out.ItemRevision, out.ScopeRevision, out.OrderSeq, out.SourceMessageSeq, out.RequestID, hash, out.AgentID, out.RunID, out.CreatedAt)
	if err != nil {
		return api.WorkOrderScopeConfirmation{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.WorkOrderScopeConfirmation{}, err
	}
	s.notify(task)
	return out, nil
}

func (s *Store) GetWorkOrderScopeConfirmation(ctx context.Context, task, item string, revision, order int64) (api.WorkOrderScopeConfirmation, error) {
	if !api.ValidID(task, "tsk") || !api.ValidID(item, "wi") || revision < 1 || order < 1 {
		return api.WorkOrderScopeConfirmation{}, api.ErrInvalid
	}
	var out api.WorkOrderScopeConfirmation
	err := s.db.QueryRowContext(ctx, `SELECT scope_revision,source_message_seq,request_id,agent_id,run_id,created_at FROM work_order_scope_confirmations WHERE task_id=? AND item_id=? AND item_revision=? AND order_seq=?`, task, item, revision, order).Scan(&out.ScopeRevision, &out.SourceMessageSeq, &out.RequestID, &out.AgentID, &out.RunID, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, api.ErrNotFound
	}
	out.TaskID, out.ItemID, out.ItemRevision, out.OrderSeq = task, item, revision, order
	return out, err
}

func sameAdmissions(a, b []api.WorkOrderAdmission) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Store) SaveWorkOrderBookkeeping(ctx context.Context, task, itemID string, req api.WorkOrderBookkeepingRequest) (api.WorkOrderBookkeepingReceipt, error) {
	if !api.ValidID(task, "tsk") || !api.ValidID(itemID, "wi") || !validRequestID(req.RequestID) || req.ExpectedRevision < 1 || req.OrderMessageSeq < 1 || req.SourceMessageSeq < 1 ||
		(req.Kind != "order" && req.Kind != "sequencing_note" && req.Kind != "decision") {
		return api.WorkOrderBookkeepingReceipt{}, api.ErrInvalid
	}
	hash := requestHash(struct {
		Task, Item string
		Request    api.WorkOrderBookkeepingRequest
	}{task, itemID, req})
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.WorkOrderBookkeepingReceipt{}, err
	}
	defer tx.Rollback()
	var oldHash, raw string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash,receipt_json FROM work_order_bookkeeping WHERE task_id=? AND request_id=?`, task, req.RequestID).Scan(&oldHash, &raw)
	if err == nil {
		if oldHash != hash {
			return api.WorkOrderBookkeepingReceipt{}, workItemConflict("bookkeeping retry payload changed")
		}
		var out api.WorkOrderBookkeepingReceipt
		err = json.Unmarshal([]byte(raw), &out)
		return out, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return api.WorkOrderBookkeepingReceipt{}, err
	}
	if err := requireScopeHandler(tx, ctx, task, req.AgentID, req.RunID); err != nil {
		return api.WorkOrderBookkeepingReceipt{}, err
	}
	item, err := getWorkItem(tx, ctx, task, itemID)
	if err != nil {
		return api.WorkOrderBookkeepingReceipt{}, err
	}
	if item.Revision != req.ExpectedRevision {
		return api.WorkOrderBookkeepingReceipt{}, workItemConflict("item revision changed; bookkeeping cannot edit item fields")
	}
	if err := requireConfirmedTeamOrder(ctx, tx, task, itemID, item.Revision, req.OrderMessageSeq); err != nil {
		return api.WorkOrderBookkeepingReceipt{}, err
	}
	var linked int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM message_work_item_links WHERE message_task_id=? AND message_seq=? AND item_task_id=? AND item_id=? AND item_revision<=? AND relationship='primary'`, task, req.SourceMessageSeq, task, itemID, item.Revision).Scan(&linked); err != nil {
		return api.WorkOrderBookkeepingReceipt{}, err
	}
	if linked != 1 {
		return api.WorkOrderBookkeepingReceipt{}, workItemConflict("bookkeeping source message is not linked to this item")
	}
	queueID := ""
	var queueRevision, queueOrder int64
	err = tx.QueryRowContext(ctx, `SELECT id,item_revision,order_seq FROM team_queue_entries WHERE task_id=? AND item_id=? AND state IN ('queued','launching','running')`, task, itemID).Scan(&queueID, &queueRevision, &queueOrder)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return api.WorkOrderBookkeepingReceipt{}, err
	}
	if queueID != "" && (queueRevision != item.Revision || queueOrder != req.OrderMessageSeq) {
		return api.WorkOrderBookkeepingReceipt{}, workItemConflict("queued order or revision differs from bookkeeping target")
	}
	if req.QueueEntryID != queueID {
		return api.WorkOrderBookkeepingReceipt{}, workItemConflict("bookkeeping queue identity differs from current item queue")
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.agent_id,b.run_id,b.context_digest FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id WHERE b.item_task_id=? AND b.item_id=? AND b.item_revision=? AND b.work_order_task_id=? AND b.work_order_message_seq=? AND a.status NOT IN ('closed','exited') ORDER BY b.agent_id`, task, itemID, item.Revision, task, req.OrderMessageSeq)
	if err != nil {
		return api.WorkOrderBookkeepingReceipt{}, err
	}
	admissions := []api.WorkOrderAdmission{}
	for rows.Next() {
		var a api.WorkOrderAdmission
		if err := rows.Scan(&a.AgentID, &a.RunID, &a.ContextDigest); err != nil {
			rows.Close()
			return api.WorkOrderBookkeepingReceipt{}, err
		}
		admissions = append(admissions, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return api.WorkOrderBookkeepingReceipt{}, err
	}
	provided := append([]api.WorkOrderAdmission(nil), req.Admissions...)
	sort.Slice(provided, func(i, j int) bool { return provided[i].AgentID < provided[j].AgentID })
	if !sameAdmissions(admissions, provided) {
		return api.WorkOrderBookkeepingReceipt{}, workItemConflict("bookkeeping admission runs or context digests differ from exact current bindings")
	}
	out := api.WorkOrderBookkeepingReceipt{ID: api.NewID("wob"), TaskID: task, ItemID: itemID, ItemRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: req.OrderMessageSeq, SourceMessageSeq: req.SourceMessageSeq, Kind: req.Kind, QueueEntryID: queueID, Admissions: admissions, RequestID: req.RequestID, AgentID: req.AgentID, RunID: req.RunID, CreatedAt: ts(s.now())}
	data, _ := json.Marshal(out)
	_, err = tx.ExecContext(ctx, `INSERT INTO work_order_bookkeeping(id,task_id,item_id,request_id,payload_hash,receipt_json,created_at) VALUES(?,?,?,?,?,?,?)`, out.ID, task, itemID, req.RequestID, hash, string(data), out.CreatedAt)
	if err != nil {
		return api.WorkOrderBookkeepingReceipt{}, fmt.Errorf("%w: cannot save bookkeeping receipt: %v", api.ErrConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return api.WorkOrderBookkeepingReceipt{}, err
	}
	s.notify(task)
	return out, nil
}

func (s *Store) GetWorkOrderBookkeepingReceipt(ctx context.Context, task, item, requestID string) (api.WorkOrderBookkeepingReceipt, error) {
	if !api.ValidID(task, "tsk") || !api.ValidID(item, "wi") || !validRequestID(requestID) {
		return api.WorkOrderBookkeepingReceipt{}, api.ErrInvalid
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT receipt_json FROM work_order_bookkeeping WHERE task_id=? AND item_id=? AND request_id=?`, task, item, requestID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return api.WorkOrderBookkeepingReceipt{}, api.ErrNotFound
	}
	if err != nil {
		return api.WorkOrderBookkeepingReceipt{}, err
	}
	var out api.WorkOrderBookkeepingReceipt
	err = json.Unmarshal([]byte(raw), &out)
	return out, err
}
