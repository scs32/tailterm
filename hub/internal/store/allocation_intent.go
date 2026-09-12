package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

// CreateAllocationIntent authors one durable, pre-admission member/extra
// intent for a preallocated agent identity, bound to the exact item,
// revision and work-order message. It is handler/lead-authored, before the
// agent is ever admitted -- never derived from execution Start/Queue state.
// A second intent for the same AgentID is a conflict: an intent is authored
// once, then consumed once by AddAgent, never mutated.
func (s *Store) CreateAllocationIntent(ctx context.Context, taskID string, req api.CreateAllocationIntentRequest, by api.Caller) (api.AllocationIntent, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !api.ValidID(taskID, "tsk") || !api.ValidID(req.AgentID, "agt") || !api.ValidID(req.ItemTaskID, "tsk") ||
		!api.ValidID(req.ItemID, "wi") || req.ItemRevision < 1 || !api.ValidID(req.WorkOrderMessage.TaskID, "tsk") ||
		req.WorkOrderMessage.Seq < 1 || (req.TeamRole != api.TeamRoleMember && req.TeamRole != api.TeamRoleExtra) {
		return api.AllocationIntent{}, api.ErrInvalid
	}
	item, err := getWorkItem(s.db, ctx, req.ItemTaskID, req.ItemID)
	if err != nil {
		return api.AllocationIntent{}, err
	}
	if item.Revision != req.ItemRevision {
		return api.AllocationIntent{}, workItemConflict("work item revision changed; author a fresh intent")
	}
	order, err := loadMessage(s.db, ctx, req.WorkOrderMessage.TaskID, req.WorkOrderMessage.Seq)
	if err != nil {
		return api.AllocationIntent{}, err
	}
	linked := len(order.WorkItems) == 1 && order.WorkItems[0].ItemTaskID == req.ItemTaskID && order.WorkItems[0].ItemID == req.ItemID && order.WorkItems[0].Relationship == "primary"
	if !linked {
		return api.AllocationIntent{}, workItemConflict("work-order message is not linked to the selected work item")
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_allocation_intents
(agent_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,team_role,created_by_node,created_by_user,created_at,consumed_at,consumed_by_run_id)
VALUES(?,?,?,?,?,?,?,?,?,?,'','')`,
		req.AgentID, req.ItemTaskID, req.ItemID, req.ItemRevision, req.WorkOrderMessage.TaskID, req.WorkOrderMessage.Seq, req.TeamRole, by.Node, by.User, ts(now))
	if err != nil {
		if isUniqueConstraintErr(err) {
			return api.AllocationIntent{}, fmt.Errorf("%w: an allocation intent already exists for this agent identity", api.ErrConflict)
		}
		return api.AllocationIntent{}, err
	}
	return api.AllocationIntent{
		AgentID: req.AgentID, ItemTaskID: req.ItemTaskID, ItemID: req.ItemID, ItemRevision: req.ItemRevision,
		WorkOrderMessage: req.WorkOrderMessage, TeamRole: req.TeamRole, CreatedBy: by, CreatedAt: now,
	}, nil
}

// loadAllocationIntent reads an unconsumed intent for exactly one agent
// identity, or nil if none exists (consumed or never authored).
func loadAllocationIntent(q queryRower, ctx context.Context, agentID string) (*api.AllocationIntent, error) {
	var in api.AllocationIntent
	var created, consumed string
	err := q.QueryRowContext(ctx, `SELECT agent_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,team_role,created_by_node,created_by_user,created_at,consumed_at,consumed_by_run_id
FROM agent_allocation_intents WHERE agent_id=?`, agentID).Scan(
		&in.AgentID, &in.ItemTaskID, &in.ItemID, &in.ItemRevision, &in.WorkOrderMessage.TaskID, &in.WorkOrderMessage.Seq,
		&in.TeamRole, &in.CreatedBy.Node, &in.CreatedBy.User, &created, &consumed, &in.ConsumedByRunID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	in.CreatedAt = parseTS(created)
	if consumed != "" {
		c := parseTS(consumed)
		in.ConsumedAt = &c
		return &in, nil // consumed: caller treats as unavailable for reuse
	}
	return &in, nil
}

func isUniqueConstraintErr(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "constraint failed"))
}
