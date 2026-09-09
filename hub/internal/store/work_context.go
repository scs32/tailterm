package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Keep the complete prepared bundle and generated launch prompt below common
// exec argument limits. Nothing is truncated: the authorized preparer must
// consolidate an oversized item's durable context and create a new revision.
const maxAgentWorkItemContextBytes = 128 * 1024

// preparedContextEnvelope is the narrow integration boundary for the accepted
// immutable history reader. Its history payload is owned by that reader; routing
// verifies only the source coordinates and stores the payload byte-for-byte.
type preparedContextEnvelope struct {
	Version          int                  `json:"version"`
	ItemTaskID       string               `json:"itemTaskId"`
	ItemID           string               `json:"itemId"`
	ItemRevision     int64                `json:"itemRevision"`
	WorkOrderMessage api.MessageReference `json:"workOrderMessage"`
	History          json.RawMessage      `json:"history"`
}

func validateAgentWorkItemRequest(q queryRower, ctx context.Context, targetTaskID string, req *api.AgentWorkItemRequest) (int64, error) {
	if req == nil {
		return 0, nil
	}
	if !api.ValidID(req.ItemTaskID, "tsk") || req.ItemTaskID != targetTaskID || !api.ValidID(req.ItemID, "wi") || req.ItemRevision < 1 ||
		!api.ValidID(req.WorkOrderMessage.TaskID, "tsk") || req.WorkOrderMessage.Seq < 1 || req.WorkOrderMessage.TaskID != req.ItemTaskID ||
		(req.ReplacesAgentID != "" && !api.ValidID(req.ReplacesAgentID, "agt")) || len(req.ContextBundle) == 0 || len(req.ContextBundle) > maxAgentWorkItemContextBytes {
		return 0, api.ErrInvalid
	}
	var envelope preparedContextEnvelope
	if err := json.Unmarshal(req.ContextBundle, &envelope); err != nil || envelope.Version != 1 || len(envelope.History) == 0 || string(envelope.History) == "null" ||
		envelope.ItemTaskID != req.ItemTaskID || envelope.ItemID != req.ItemID || envelope.ItemRevision != req.ItemRevision || envelope.WorkOrderMessage != req.WorkOrderMessage {
		return 0, api.ErrInvalid
	}
	// Admission retains the current-row CAS and structured order relationship.
	// The bundle's immutable revision/history payload is prepared by the handler
	// or human launch flow through the accepted history interface, not rebuilt here.
	item, err := getWorkItem(q, ctx, req.ItemTaskID, req.ItemID)
	if err != nil {
		return 0, err
	}
	if item.Revision != req.ItemRevision {
		return 0, workItemConflict("work item revision changed; prepare a new context bundle before launching")
	}
	order, err := loadMessage(q, ctx, req.WorkOrderMessage.TaskID, req.WorkOrderMessage.Seq)
	if err != nil {
		return 0, err
	}
	linked := len(order.WorkItems) == 1 && order.WorkItems[0].ItemTaskID == req.ItemTaskID && order.WorkItems[0].ItemID == req.ItemID && order.WorkItems[0].Relationship == "primary"
	if !linked {
		return 0, workItemConflict("work-order message is not linked to the selected work item")
	}
	if req.ReplacesAgentID != "" {
		var priorTask, priorRun, priorRole string
		if err := q.QueryRowContext(ctx, `SELECT task_id,run_id,role FROM agents WHERE id=?`, req.ReplacesAgentID).Scan(&priorTask, &priorRun, &priorRole); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, api.ErrInvalid
			}
			return 0, err
		}
		if priorTask != targetTaskID || priorRole != "" {
			return 0, api.ErrInvalid
		}
		prior, err := loadAgentWorkItemBinding(q, ctx, req.ReplacesAgentID, priorRun)
		if err != nil || prior == nil || prior.ItemTaskID != req.ItemTaskID || prior.ItemID != req.ItemID {
			return 0, workItemConflict("replacement agent is not bound to the selected work item")
		}
	}
	var through int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM messages WHERE task_id=?`, targetTaskID).Scan(&through); err != nil {
		return 0, err
	}
	return through, nil
}

func insertAgentWorkItemBinding(ctx context.Context, tx *sql.Tx, agent api.Agent, req *api.AgentWorkItemRequest, through int64) (*api.AgentWorkItemBinding, error) {
	if req == nil {
		return nil, nil
	}
	digestBytes := sha256.Sum256(req.ContextBundle)
	binding := &api.AgentWorkItemBinding{
		AgentID: agent.ID, RunID: agent.RunID, ItemTaskID: req.ItemTaskID, ItemID: req.ItemID,
		ItemRevision: req.ItemRevision, WorkOrderMessage: req.WorkOrderMessage,
		ContextThroughMessageSeq: through, ReplacesAgentID: req.ReplacesAgentID,
		ContextDigest: hex.EncodeToString(digestBytes[:]), CreatedAt: agent.CreatedAt,
	}
	var replaces any
	if binding.ReplacesAgentID != "" {
		replaces = binding.ReplacesAgentID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_work_item_bindings
(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,replaces_agent_id,context_digest,context_json,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, binding.AgentID, binding.RunID, binding.ItemTaskID, binding.ItemID, binding.ItemRevision,
		binding.WorkOrderMessage.TaskID, binding.WorkOrderMessage.Seq, binding.ContextThroughMessageSeq, replaces,
		binding.ContextDigest, []byte(req.ContextBundle), ts(binding.CreatedAt))
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO read_cursors(task_id,agent_id,up_to) VALUES(?,?,?)
ON CONFLICT(task_id,agent_id) DO UPDATE SET up_to=MAX(up_to,excluded.up_to)`, agent.TaskID, agent.ID, through)
	return binding, err
}

func loadAgentWorkItemBinding(q queryRower, ctx context.Context, agentID, runID string) (*api.AgentWorkItemBinding, error) {
	var binding api.AgentWorkItemBinding
	var created string
	err := q.QueryRowContext(ctx, `SELECT agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,COALESCE(replaces_agent_id,''),context_digest,created_at
FROM agent_work_item_bindings WHERE agent_id=? AND run_id=?`, agentID, runID).Scan(
		&binding.AgentID, &binding.RunID, &binding.ItemTaskID, &binding.ItemID, &binding.ItemRevision,
		&binding.WorkOrderMessage.TaskID, &binding.WorkOrderMessage.Seq, &binding.ContextThroughMessageSeq,
		&binding.ReplacesAgentID, &binding.ContextDigest, &created,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	binding.CreatedAt = parseTS(created)
	return &binding, nil
}

func (s *Store) loadAgentWorkItem(ctx context.Context, agent *api.Agent) error {
	binding, err := loadAgentWorkItemBinding(s.db, ctx, agent.ID, agent.RunID)
	agent.WorkItem = binding
	return err
}

// GetAgentWorkItemContext requires the exact current run and reads only the
// immutable bundle captured during authorized admission. It never performs a
// fresh work-item/history read on behalf of the ordinary worker.
func (s *Store) GetAgentWorkItemContext(ctx context.Context, taskID, agentID, runID string) (api.AgentWorkItemContext, error) {
	var out api.AgentWorkItemContext
	if !api.ValidID(taskID, "tsk") || !api.ValidID(agentID, "agt") || !validRunID(runID) {
		return out, api.ErrInvalid
	}
	agent, err := s.GetAgent(ctx, agentID)
	if err != nil {
		return out, err
	}
	if agent.TaskID != taskID {
		return out, api.ErrNotFound
	}
	if agent.RunID != runID {
		return out, workItemConflict("agent run changed; refresh before restoring context")
	}
	if agent.WorkItem == nil {
		return out, api.ErrNotFound
	}
	out.Version, out.Binding = 1, *agent.WorkItem
	if err := s.db.QueryRowContext(ctx, `SELECT context_json FROM agent_work_item_bindings WHERE agent_id=? AND run_id=?`, agentID, runID).Scan(&out.Bundle); err != nil {
		return api.AgentWorkItemContext{}, err
	}
	digestBytes := sha256.Sum256(out.Bundle)
	if hex.EncodeToString(digestBytes[:]) != out.Binding.ContextDigest {
		return api.AgentWorkItemContext{}, errors.New("stored work-item context digest mismatch")
	}
	return out, nil
}
