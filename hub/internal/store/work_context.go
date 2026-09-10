package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Keep the complete prepared bundle and generated launch prompt bounded.
// Nothing is truncated; launching oversized immutable histories is unsupported.
const maxAgentWorkItemContextBytes = api.MaxAgentWorkItemContextBytes

// preparedContextEnvelope is the narrow integration boundary for the accepted
// immutable history reader. Its history payload is owned by that reader; routing
// verifies only the source coordinates and stores the payload byte-for-byte.
type preparedContextEnvelope struct {
	Version          int                    `json:"version"`
	ItemTaskID       string                 `json:"itemTaskId"`
	ItemID           string                 `json:"itemId"`
	ItemRevision     int64                  `json:"itemRevision"`
	WorkOrderMessage api.MessageReference   `json:"workOrderMessage"`
	History          preparedContextHistory `json:"history"`
}

type preparedContextHistory struct {
	Revision  api.WorkItemRevision      `json:"revision"`
	Revisions []api.WorkItemRevision    `json:"revisions"`
	Gaps      []api.HistoryGap          `json:"gaps"`
	Messages  []api.WorkItemMessageLink `json:"messages"`
	Coverage  api.HistoryCoverage       `json:"coverage"`
}

func validatePreparedContextBundle(req *api.AgentWorkItemRequest) error {
	if len(req.ContextBundle) > maxAgentWorkItemContextBytes {
		return api.ErrContextLimit
	}
	if !api.ValidID(req.ItemTaskID, "tsk") || !api.ValidID(req.ItemID, "wi") || req.ItemRevision < 1 ||
		!api.ValidID(req.WorkOrderMessage.TaskID, "tsk") || req.WorkOrderMessage.Seq < 1 || req.WorkOrderMessage.TaskID != req.ItemTaskID ||
		(req.ReplacesAgentID != "" && !api.ValidID(req.ReplacesAgentID, "agt")) || len(req.ContextBundle) == 0 {
		return api.ErrInvalid
	}
	var envelope preparedContextEnvelope
	if err := json.Unmarshal(req.ContextBundle, &envelope); err != nil || envelope.Version != 1 ||
		envelope.ItemTaskID != req.ItemTaskID || envelope.ItemID != req.ItemID || envelope.ItemRevision != req.ItemRevision || envelope.WorkOrderMessage != req.WorkOrderMessage {
		return api.ErrInvalid
	}
	exact := envelope.History.Revision
	if exact.TaskID != req.ItemTaskID || exact.ItemID != req.ItemID || exact.Revision != req.ItemRevision ||
		exact.AttributionKind != "shared_workspace_claim" ||
		(exact.Provenance != "native" && exact.Provenance != "reconstructed_change_log" && exact.Provenance != "current_row_checkpoint") ||
		envelope.History.Coverage.ObservedCurrentRevision != req.ItemRevision || envelope.History.Coverage.ConversationLinks != "explicit_only" {
		return api.ErrInvalid
	}
	seenExact := false
	previousRevision := int64(0)
	for _, revision := range envelope.History.Revisions {
		if revision.TaskID != req.ItemTaskID || revision.ItemID != req.ItemID || revision.Revision <= previousRevision || revision.Revision > req.ItemRevision {
			return api.ErrInvalid
		}
		previousRevision = revision.Revision
		seenExact = seenExact || revision.Revision == req.ItemRevision
	}
	if !seenExact {
		return api.ErrInvalid
	}
	orderInBundle := false
	for _, link := range envelope.History.Messages {
		if link.Message.TaskID == req.WorkOrderMessage.TaskID && link.Message.Seq == req.WorkOrderMessage.Seq && link.Relationship == "primary" {
			orderInBundle = true
		}
	}
	if !orderInBundle {
		return workItemConflict("work-order message is absent from the prepared explicit history")
	}
	return nil
}

func validateAgentWorkItemRequest(q queryRower, ctx context.Context, targetTaskID string, req *api.AgentWorkItemRequest) (int64, error) {
	if req == nil {
		return 0, nil
	}
	if err := validatePreparedContextBundle(req); err != nil {
		return 0, err
	}
	if req.ItemTaskID == targetTaskID {
		if req.QueueClaim != nil {
			return 0, api.ErrInvalid
		}
	} else if err := validateCrossProjectQueueAdmission(q, ctx, targetTaskID, req); err != nil {
		return 0, err
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

func validateCrossProjectQueueAdmission(q queryRower, ctx context.Context, targetTaskID string, req *api.AgentWorkItemRequest) error {
	claim := req.QueueClaim
	if claim == nil || !validQueueEntryID(claim.EntryID) || claim.Cycle < 1 || claim.ExpectedRevision < 1 ||
		!api.ValidID(claim.ClaimantAgentID, "agt") || !validRunID(claim.ClaimantRunID) {
		return api.ErrInvalid
	}
	entry, err := getQueueEntry(q, ctx, targetTaskID, claim.EntryID)
	if err != nil {
		return err
	}
	if entry.SourceTaskID != req.ItemTaskID || entry.ItemID != req.ItemID || entry.Cycle != claim.Cycle ||
		entry.Revision != claim.ExpectedRevision || entry.State != api.QueueStateClaimed || entry.Stale ||
		entry.ReviewNeeded || entry.ReconciliationNeeded || entry.ClaimedItemRevision != req.ItemRevision ||
		entry.OfferedItemRevision != req.ItemRevision || entry.CurrentItemRevision != req.ItemRevision ||
		entry.ClaimantAgentID != claim.ClaimantAgentID || entry.ClaimantRunID != claim.ClaimantRunID ||
		entry.WorkOrderMessage == nil || *entry.WorkOrderMessage != req.WorkOrderMessage ||
		entry.WorkerAgentID != "" || entry.WorkerRunID != "" || entry.ContextDigest != "" {
		return workItemConflict("cross-project admission does not match the exact current Queue claim")
	}
	var targetStatus, sourceStatus, itemStatus string
	if err = q.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=?`, targetTaskID).Scan(&targetStatus); err != nil {
		return err
	}
	if err = q.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=?`, req.ItemTaskID).Scan(&sourceStatus); err != nil {
		return err
	}
	if err = q.QueryRowContext(ctx, `SELECT status FROM work_items WHERE task_id=? AND id=? AND revision=?`, req.ItemTaskID, req.ItemID, req.ItemRevision).Scan(&itemStatus); err != nil {
		return err
	}
	if targetStatus != api.TaskOpen || sourceStatus != api.TaskOpen || itemStatus == "done" || itemStatus == "dismissed" {
		return workItemConflict("cross-project Queue claim is no longer eligible for admission")
	}
	if err = validateQueueWorkOrder(ctx, q, entry, &req.WorkOrderMessage, req.ItemRevision); err != nil {
		return err
	}
	var claimantTask, claimantRun, claimantStatus, claimantRole, claimantName, orchestratorName string
	if err = q.QueryRowContext(ctx, `SELECT a.task_id,a.run_id,a.status,a.role,a.name,t.orchestrator FROM agents a JOIN tasks t ON t.id=a.task_id WHERE a.id=?`, claim.ClaimantAgentID).Scan(
		&claimantTask, &claimantRun, &claimantStatus, &claimantRole, &claimantName, &orchestratorName); err != nil {
		return err
	}
	if claimantTask != targetTaskID || claimantRun != claim.ClaimantRunID || claimantRole != "" || claimantName != orchestratorName ||
		claimantStatus == api.AgentRetired || claimantStatus == api.AgentClosed || claimantStatus == api.AgentExited {
		return workItemConflict("Queue claimant is not the exact current project orchestrator run")
	}
	if entry.Selection != nil {
		var selectionFromAgent, selectionFromRun, selectionNoticeKind string
		if err = q.QueryRowContext(ctx, `SELECT from_agent,from_run_id,system_notice_kind FROM messages WHERE task_id=? AND seq=?`, entry.Selection.TaskID, entry.Selection.MessageSeq).Scan(&selectionFromAgent, &selectionFromRun, &selectionNoticeKind); err != nil {
			return err
		}
		if selectionNoticeKind != "" || (selectionFromAgent != "" && (selectionFromAgent != claim.ClaimantAgentID || selectionFromRun != claim.ClaimantRunID)) {
			return workItemConflict("Queue selection is not retained human/current-orchestrator authority")
		}
	} else {
		var actorAgent, actorNode, actorUser string
		if err = q.QueryRowContext(ctx, `SELECT actor_agent_id,actor_node,actor_user FROM queue_events WHERE entry_id=? AND revision=? AND kind='claim'`, entry.ID, entry.Revision).Scan(&actorAgent, &actorNode, &actorUser); err != nil {
			return err
		}
		if actorAgent != "" || actorNode == "" || actorUser == "" {
			return workItemConflict("Queue claim has no retained human selection")
		}
	}
	return nil
}

func (s *Store) pinCrossProjectQueueAdmission(ctx context.Context, tx *sql.Tx, targetTaskID string, req *api.AgentWorkItemRequest, agent api.Agent) error {
	if req == nil || req.ItemTaskID == targetTaskID {
		return nil
	}
	entry, err := getQueueEntry(tx, ctx, targetTaskID, req.QueueClaim.EntryID)
	if err != nil {
		return err
	}
	digestBytes := sha256.Sum256(req.ContextBundle)
	entry.WorkerAgentID, entry.WorkerRunID = agent.ID, agent.RunID
	entry.ContextDigest = hex.EncodeToString(digestBytes[:])
	entry.Revision++
	entry.UpdatedAt = agent.CreatedAt
	if err = updateQueueEntry(ctx, tx, entry); err != nil {
		return err
	}
	actor := api.Sender{AgentID: agent.ID}
	event, err := appendQueueEvent(ctx, tx, entry, "admitted", "", "", agent.RunID, actor, agent.CreatedAt, agent.CreatedAt)
	if err != nil {
		return err
	}
	_, err = s.createQueueNotification(ctx, tx, entry, event, actor, 0)
	return err
}

func exactExistingQueueAdmission(q queryRower, ctx context.Context, targetTaskID string, req api.AddAgentRequest, existing api.Agent) (bool, error) {
	work := req.WorkItem
	if work == nil || work.ItemTaskID == targetTaskID || work.QueueClaim == nil || existing.WorkItem == nil {
		return false, nil
	}
	if existing.Name != req.Name || existing.Host != req.Host || existing.Session != req.Session || existing.Runtime != req.Runtime ||
		existing.Cwd != req.Cwd || existing.ParentAgentID != req.ParentAgentID || existing.Role != req.Role {
		return false, workItemConflict("Queue admission retry changed agent launch settings")
	}
	digestBytes := sha256.Sum256(work.ContextBundle)
	digest := hex.EncodeToString(digestBytes[:])
	binding := existing.WorkItem
	if binding.ItemTaskID != work.ItemTaskID || binding.ItemID != work.ItemID || binding.ItemRevision != work.ItemRevision ||
		binding.WorkOrderMessage != work.WorkOrderMessage || binding.ReplacesAgentID != work.ReplacesAgentID || binding.ContextDigest != digest {
		return false, workItemConflict("Queue admission retry changed its exact item/order/context binding")
	}
	rows, err := q.QueryContext(ctx, `SELECT snapshot FROM queue_events WHERE target_task_id=? AND entry_id=? AND cycle=? AND kind='admitted' ORDER BY seq`, targetTaskID, work.QueueClaim.EntryID, work.QueueClaim.Cycle)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var entry api.QueueEntry
		if err = rows.Scan(&raw); err != nil || json.Unmarshal([]byte(raw), &entry) != nil {
			if err == nil {
				err = errors.New("invalid Queue admission snapshot")
			}
			return false, err
		}
		if entry.Revision == work.QueueClaim.ExpectedRevision+1 && entry.WorkerAgentID == existing.ID && entry.WorkerRunID == existing.RunID &&
			entry.ContextDigest == digest && entry.ClaimantAgentID == work.QueueClaim.ClaimantAgentID && entry.ClaimantRunID == work.QueueClaim.ClaimantRunID &&
			entry.SourceTaskID == work.ItemTaskID && entry.ItemID == work.ItemID && entry.ClaimedItemRevision == work.ItemRevision {
			return true, nil
		}
	}
	return false, rows.Err()
}

// validatedBoundHistoricalMessage reports whether a stale message revision is
// the immutable launch revision for the author's exact current run. Merely
// naming a bound agent or an old item revision is insufficient: the item/order
// coordinates must match the binding, and its stored prepared history must
// still pass the same validation and digest check used at admission.
func validatedBoundHistoricalMessage(q queryRower, ctx context.Context, messageTaskID string, req api.PostMessageRequest) (bool, error) {
	if req.AgentID == "" || len(req.WorkItems) != 1 || req.WorkOrderMessage == nil {
		return false, nil
	}
	var binding api.AgentWorkItemBinding
	var contextDigest string
	var contextBundle []byte
	err := q.QueryRowContext(ctx, `SELECT b.agent_id,b.run_id,b.item_task_id,b.item_id,b.item_revision,
b.work_order_task_id,b.work_order_message_seq,b.context_digest,b.context_json
FROM agents a
JOIN agent_work_item_bindings b ON b.agent_id=a.id AND b.run_id=a.run_id
WHERE a.id=? AND a.task_id=?`, req.AgentID, messageTaskID).Scan(
		&binding.AgentID, &binding.RunID, &binding.ItemTaskID, &binding.ItemID, &binding.ItemRevision,
		&binding.WorkOrderMessage.TaskID, &binding.WorkOrderMessage.Seq, &contextDigest, &contextBundle,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	link := req.WorkItems[0]
	if link.ItemTaskID != binding.ItemTaskID || link.ItemID != binding.ItemID || link.ItemRevision != binding.ItemRevision ||
		link.Relationship != "primary" || *req.WorkOrderMessage != binding.WorkOrderMessage {
		return false, nil
	}
	digestBytes := sha256.Sum256(contextBundle)
	if hex.EncodeToString(digestBytes[:]) != contextDigest {
		return false, errors.New("stored work-item context digest mismatch")
	}
	stored := &api.AgentWorkItemRequest{
		ItemTaskID: binding.ItemTaskID, ItemID: binding.ItemID, ItemRevision: binding.ItemRevision,
		WorkOrderMessage: binding.WorkOrderMessage, ContextBundle: json.RawMessage(contextBundle),
	}
	if err := validatePreparedContextBundle(stored); err != nil {
		return false, fmt.Errorf("stored work-item context is invalid: %w", err)
	}
	return true, nil
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
