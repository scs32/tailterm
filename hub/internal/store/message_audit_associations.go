package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

func findMessageAuditAssociationReceipt(q queryRower, ctx context.Context, taskID, requestID, agentID string, by api.Caller) (api.MessageAuditAssociationResult, string, error) {
	var result api.MessageAuditAssociationResult
	var payload, resultJSON string
	err := q.QueryRowContext(ctx, `SELECT payload_hash,result FROM message_audit_association_receipts
WHERE task_id=? AND agent_id=? AND by_node=? AND by_user=? AND request_id=?`,
		taskID, agentID, by.Node, by.User, requestID).Scan(&payload, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return result, payload, api.ErrNotFound
	}
	if err != nil {
		return result, payload, err
	}
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return result, payload, err
	}
	return result, payload, nil
}

func validateAssociationActorAndSource(q queryRower, ctx context.Context, taskID string, req api.CreateMessageAuditAssociationRequest, by api.Caller) error {
	if req.Source.TaskID != taskID {
		return api.ErrInvalid
	}
	var fromAgent, fromNode, fromUser string
	if err := q.QueryRowContext(ctx, `SELECT from_agent,from_node,from_user FROM messages WHERE task_id=? AND seq=?`,
		taskID, req.Source.Seq).Scan(&fromAgent, &fromNode, &fromUser); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return api.ErrInvalid
		}
		return err
	}
	if req.AgentID == "" {
		if req.RunID != "" || fromAgent != "" || fromNode != by.Node || fromUser != by.User {
			return api.ErrInvalid
		}
		return nil
	}
	if req.RunID == "" || !validRunID(req.RunID) || fromAgent != req.AgentID || fromNode != by.Node || fromUser != by.User {
		return api.ErrInvalid
	}
	var currentRun, role, status string
	if err := q.QueryRowContext(ctx, `SELECT run_id,role,status FROM agents WHERE task_id=? AND id=?`, taskID, req.AgentID).Scan(&currentRun, &role, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return api.ErrInvalid
		}
		return err
	}
	if role != api.AgentRoleDatabaseHandler {
		return api.ErrInvalid
	}
	if currentRun != req.RunID {
		return workItemConflict("database-handler run changed; refresh identity before creating a foreign association")
	}
	if !activeMessageAuditAgentStatus(status) {
		return workItemConflict("database-handler run is not active for foreign association")
	}
	return nil
}

func (s *Store) CreateMessageAuditAssociation(ctx context.Context, taskID string, req api.CreateMessageAuditAssociationRequest, by api.Caller) (api.MessageAuditAssociationResult, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !api.ValidID(taskID, "tsk") || !validRequestID(req.RequestID) || !api.ValidID(req.Item.ItemTaskID, "tsk") ||
		!api.ValidID(req.Item.ItemID, "wi") || req.Item.ItemRevision < 1 || req.Item.ItemTaskID == taskID ||
		!api.ValidID(req.Source.TaskID, "tsk") || req.Source.Seq < 1 || strings.TrimSpace(req.Reason) == "" ||
		!api.ValidText(req.Reason, 2048) || (req.AgentID != "" && !api.ValidID(req.AgentID, "agt")) ||
		(req.RunID != "" && !validRunID(req.RunID)) {
		return api.MessageAuditAssociationResult{}, false, api.ErrInvalid
	}
	payload := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.MessageAuditAssociationResult{}, false, err
	}
	defer tx.Rollback()
	prior, priorHash, err := findMessageAuditAssociationReceipt(tx, ctx, taskID, req.RequestID, req.AgentID, by)
	if err == nil {
		if priorHash != payload {
			return api.MessageAuditAssociationResult{}, false, workItemConflict("request ID was already used with different foreign-association data")
		}
		prior.Replay = true
		return prior, true, nil
	}
	if !errors.Is(err, api.ErrNotFound) {
		return api.MessageAuditAssociationResult{}, false, err
	}
	if !validMessageAuditCaller(by) {
		return api.MessageAuditAssociationResult{}, false, api.ErrInvalid
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return api.MessageAuditAssociationResult{}, false, api.ErrNotFound
	}
	if err != nil {
		return api.MessageAuditAssociationResult{}, false, err
	}
	if task.Status != api.TaskOpen {
		return api.MessageAuditAssociationResult{}, false, api.ErrClosed
	}
	if err = validateAssociationActorAndSource(tx, ctx, taskID, req, by); err != nil {
		return api.MessageAuditAssociationResult{}, false, err
	}
	item, err := getWorkItem(tx, ctx, req.Item.ItemTaskID, req.Item.ItemID)
	if err != nil {
		return api.MessageAuditAssociationResult{}, false, err
	}
	if item.Revision != req.Item.ItemRevision {
		var retained int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM work_item_revisions WHERE item_task_id=? AND item_id=? AND revision=?`,
			req.Item.ItemTaskID, req.Item.ItemID, req.Item.ItemRevision).Scan(&retained); err != nil {
			return api.MessageAuditAssociationResult{}, false, err
		}
		if retained == 0 {
			return api.MessageAuditAssociationResult{}, false, workItemConflict("foreign item revision is not retained")
		}
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM message_audit_foreign_associations
WHERE message_task_id=? AND item_task_id=? AND item_id=? AND item_revision=?`, taskID, req.Item.ItemTaskID, req.Item.ItemID, req.Item.ItemRevision).Scan(&existing)
	if err == nil {
		return api.MessageAuditAssociationResult{}, false, workItemConflict("foreign item association already exists")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return api.MessageAuditAssociationResult{}, false, err
	}
	now := s.now()
	association := api.MessageAuditAssociation{ID: api.NewID("maa"), TaskID: taskID, Item: req.Item, Source: req.Source, Reason: req.Reason,
		Actor: api.MessageAuditActor{AgentID: req.AgentID, RunID: req.RunID, Caller: by}, CreatedAt: now}
	if _, err = tx.ExecContext(ctx, `INSERT INTO message_audit_foreign_associations
(id,message_task_id,item_task_id,item_id,item_revision,source_task_id,source_message_seq,reason,agent_id,run_id,by_node,by_user,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, association.ID, taskID, req.Item.ItemTaskID, req.Item.ItemID, req.Item.ItemRevision,
		req.Source.TaskID, req.Source.Seq, req.Reason, req.AgentID, req.RunID, by.Node, by.User, ts(now)); err != nil {
		return api.MessageAuditAssociationResult{}, false, err
	}
	result := api.MessageAuditAssociationResult{Association: association,
		Receipt: api.MessageAuditAssociationReceipt{ID: api.NewID("mar"), TaskID: taskID, RequestID: req.RequestID, AssociationID: association.ID, CreatedAt: now}}
	resultJSON, _ := json.Marshal(result)
	if _, err = tx.ExecContext(ctx, `INSERT INTO message_audit_association_receipts
(id,task_id,agent_id,by_node,by_user,request_id,payload_hash,association_id,result,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?)`, result.Receipt.ID, taskID, req.AgentID, by.Node, by.User, req.RequestID, payload, association.ID, string(resultJSON), ts(now)); err != nil {
		return api.MessageAuditAssociationResult{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return api.MessageAuditAssociationResult{}, false, err
	}
	s.notify(taskID)
	return result, false, nil
}

func (s *Store) GetMessageAuditAssociationReceipt(ctx context.Context, taskID, requestID, agentID string, by api.Caller) (api.MessageAuditAssociationResult, error) {
	if !api.ValidID(taskID, "tsk") || !validRequestID(requestID) || (agentID != "" && !api.ValidID(agentID, "agt")) {
		return api.MessageAuditAssociationResult{}, api.ErrInvalid
	}
	result, _, err := findMessageAuditAssociationReceipt(s.db, ctx, taskID, requestID, agentID, by)
	if err == nil {
		result.Replay = true
	}
	return result, err
}
