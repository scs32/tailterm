package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

func validContextDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// CreateAllocationIntent authors one durable, pre-admission member/extra
// intent for a preallocated agent identity, bound to the exact target task,
// item, revision, work-order message, prepared context, team role and a
// preallocated expected run ID. It is handler/lead-authored, before the
// agent is ever admitted -- never derived from execution Start/Queue state.
// The author's own current agent/run identity is verified against the live
// roster (matching run, not closed/exited, and holding database_handler
// role or the task Orchestrator name) before the intent is accepted; a
// stale or mismatched author run is rejected, not merely stored. A second
// intent for the same AgentID is a conflict unless RequestID scopes an
// exact, identical retry (see CreateAllocationIntentRequest).
func (s *Store) CreateAllocationIntent(ctx context.Context, taskID string, req api.CreateAllocationIntentRequest, by api.Caller) (api.AllocationIntent, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !api.ValidID(taskID, "tsk") || !api.ValidID(req.AgentID, "agt") || req.TargetTaskID != taskID ||
		!api.ValidID(req.ItemTaskID, "tsk") || !api.ValidID(req.ItemID, "wi") || req.ItemRevision < 1 ||
		!api.ValidID(req.WorkOrderMessage.TaskID, "tsk") || req.WorkOrderMessage.Seq < 1 ||
		!validContextDigest(req.ContextDigest) || (req.TeamRole != api.TeamRoleMember && req.TeamRole != api.TeamRoleExtra) ||
		!api.ValidID(req.AuthorAgentID, "agt") || !validRunID(req.AuthorRunID) || !validRunID(req.ExpectedRunID) {
		return api.AllocationIntent{}, api.ErrInvalid
	}
	// Independent review #3003/#3010 finding 1: the intended launcher is
	// part of the authorization tuple, not merely whichever ParentAgentID
	// happens to consume the intent afterward. When no explicit delegate is
	// named, the author is presumed the intended launcher -- the common
	// case where the same handler/lead session both authors the intent and
	// performs the launch.
	if req.ExpectedLauncherAgentID == "" && req.ExpectedLauncherRunID == "" {
		req.ExpectedLauncherAgentID = req.AuthorAgentID
		req.ExpectedLauncherRunID = req.AuthorRunID
	}
	if !api.ValidID(req.ExpectedLauncherAgentID, "agt") || !validRunID(req.ExpectedLauncherRunID) {
		return api.AllocationIntent{}, api.ErrInvalid
	}
	if req.RequestID != "" {
		existing, err := loadAllocationIntentByRetryKey(s.db, ctx, taskID, req.AuthorAgentID, req.AuthorRunID, req.RequestID)
		if err != nil {
			return api.AllocationIntent{}, err
		}
		if existing != nil {
			if existing.AgentID != req.AgentID || existing.ItemTaskID != req.ItemTaskID || existing.ItemID != req.ItemID ||
				existing.ItemRevision != req.ItemRevision || existing.WorkOrderMessage != req.WorkOrderMessage ||
				existing.ContextDigest != req.ContextDigest || existing.TeamRole != req.TeamRole || existing.ExpectedRunID != req.ExpectedRunID ||
				existing.ExpectedLauncherAgentID != req.ExpectedLauncherAgentID || existing.ExpectedLauncherRunID != req.ExpectedLauncherRunID {
				return api.AllocationIntent{}, fmt.Errorf("%w: retry request-id reused with a different allocation intent payload", api.ErrConflict)
			}
			// Identical retry: durable readback of the exact prior outcome,
			// including if it has since been consumed. Never a second
			// allowance/intent.
			return *existing, nil
		}
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
	// Author authority: a live current run of the database_handler or the
	// task's Orchestrator on this exact target task. Functional recorded-
	// allocation consistency (independent review #2844/#2845 framing), not
	// a claim that this is unforgeable on a single-shared-token hub.
	var authorTask, authorRun, authorName, authorRole, authorStatus string
	if err := s.db.QueryRowContext(ctx, `SELECT task_id,run_id,name,role,status FROM agents WHERE id=?`, req.AuthorAgentID).Scan(&authorTask, &authorRun, &authorName, &authorRole, &authorStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return api.AllocationIntent{}, fmt.Errorf("%w: unknown author agent identity", api.ErrInvalid)
		}
		return api.AllocationIntent{}, err
	}
	t, err := s.GetTask(ctx, taskID)
	if err != nil {
		return api.AllocationIntent{}, err
	}
	authorized := authorRole == api.AgentRoleDatabaseHandler || (t.Orchestrator != "" && strings.EqualFold(authorName, t.Orchestrator))
	// Independent review #3003/#3010 additional gap: a retired author was
	// previously accepted because only closed/exited status was checked.
	// Retirement disables inbox wake-ups but is not a live, currently
	// acting session either -- an authoring call from a retired identity is
	// rejected exactly like a closed/exited one.
	if authorTask != taskID || authorRun != req.AuthorRunID || authorStatus == api.AgentClosed || authorStatus == api.AgentExited || authorStatus == api.AgentRetired || !authorized {
		return api.AllocationIntent{}, fmt.Errorf("%w: author agent/run is not a current authorized database handler or orchestrator on this task", api.ErrConflict)
	}
	// The intended launcher, when an explicit delegate distinct from the
	// author, must itself be a live, non-closed/exited/retired agent on
	// this exact target task -- the same liveness bar as the author,
	// though it need not hold database_handler/Orchestrator authority
	// itself (a lead may delegate the actual spawn to any current team
	// member it names).
	if req.ExpectedLauncherAgentID != req.AuthorAgentID || req.ExpectedLauncherRunID != req.AuthorRunID {
		var launcherTask, launcherRun, launcherStatus string
		if err := s.db.QueryRowContext(ctx, `SELECT task_id,run_id,status FROM agents WHERE id=?`, req.ExpectedLauncherAgentID).Scan(&launcherTask, &launcherRun, &launcherStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return api.AllocationIntent{}, fmt.Errorf("%w: unknown intended launcher agent identity", api.ErrInvalid)
			}
			return api.AllocationIntent{}, err
		}
		if launcherTask != taskID || launcherRun != req.ExpectedLauncherRunID ||
			launcherStatus == api.AgentClosed || launcherStatus == api.AgentExited || launcherStatus == api.AgentRetired {
			return api.AllocationIntent{}, fmt.Errorf("%w: intended launcher agent/run is not a current live agent on this task", api.ErrConflict)
		}
	}
	// ExpectedRunID must be unique: not already an agent's run, and not
	// already recorded on another intent (consumed or not).
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE run_id=?`, req.ExpectedRunID).Scan(&count); err != nil {
		return api.AllocationIntent{}, err
	}
	if count > 0 {
		return api.AllocationIntent{}, fmt.Errorf("%w: expected run id is already in use", api.ErrConflict)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_allocation_intents WHERE expected_run_id=?`, req.ExpectedRunID).Scan(&count); err != nil {
		return api.AllocationIntent{}, err
	}
	if count > 0 {
		return api.AllocationIntent{}, fmt.Errorf("%w: expected run id is already reserved by another allocation intent", api.ErrConflict)
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_allocation_intents
(agent_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,team_role,target_task_id,context_digest,author_agent_id,author_run_id,expected_run_id,expected_launcher_agent_id,expected_launcher_run_id,request_id,created_by_node,created_by_user,created_at,consumed_at,consumed_by_run_id,launcher_agent_id,launcher_run_id)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'','','','')`,
		req.AgentID, req.ItemTaskID, req.ItemID, req.ItemRevision, req.WorkOrderMessage.TaskID, req.WorkOrderMessage.Seq, req.TeamRole,
		req.TargetTaskID, req.ContextDigest, req.AuthorAgentID, req.AuthorRunID, req.ExpectedRunID, req.ExpectedLauncherAgentID, req.ExpectedLauncherRunID, req.RequestID, by.Node, by.User, ts(now))
	if err != nil {
		if isUniqueConstraintErr(err) {
			return api.AllocationIntent{}, fmt.Errorf("%w: an allocation intent already exists for this agent identity", api.ErrConflict)
		}
		return api.AllocationIntent{}, err
	}
	return api.AllocationIntent{
		AgentID: req.AgentID, TargetTaskID: req.TargetTaskID, ItemTaskID: req.ItemTaskID, ItemID: req.ItemID, ItemRevision: req.ItemRevision,
		WorkOrderMessage: req.WorkOrderMessage, ContextDigest: req.ContextDigest, TeamRole: req.TeamRole,
		AuthorAgentID: req.AuthorAgentID, AuthorRunID: req.AuthorRunID, ExpectedRunID: req.ExpectedRunID,
		ExpectedLauncherAgentID: req.ExpectedLauncherAgentID, ExpectedLauncherRunID: req.ExpectedLauncherRunID, RequestID: req.RequestID,
		CreatedBy: by, CreatedAt: now,
	}, nil
}

const allocationIntentCols = `agent_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,team_role,target_task_id,context_digest,author_agent_id,author_run_id,expected_run_id,expected_launcher_agent_id,expected_launcher_run_id,request_id,created_by_node,created_by_user,created_at,consumed_at,consumed_by_run_id,launcher_agent_id,launcher_run_id`

func scanAllocationIntent(row interface{ Scan(...any) error }) (*api.AllocationIntent, error) {
	var in api.AllocationIntent
	var created, consumed string
	err := row.Scan(
		&in.AgentID, &in.ItemTaskID, &in.ItemID, &in.ItemRevision, &in.WorkOrderMessage.TaskID, &in.WorkOrderMessage.Seq,
		&in.TeamRole, &in.TargetTaskID, &in.ContextDigest, &in.AuthorAgentID, &in.AuthorRunID, &in.ExpectedRunID,
		&in.ExpectedLauncherAgentID, &in.ExpectedLauncherRunID, &in.RequestID,
		&in.CreatedBy.Node, &in.CreatedBy.User, &created, &consumed, &in.ConsumedByRunID, &in.LauncherAgentID, &in.LauncherRunID,
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
	}
	return &in, nil
}

// loadAllocationIntent reads an intent for exactly one agent identity
// (consumed or not), or nil if none was ever authored.
func loadAllocationIntent(q queryRower, ctx context.Context, agentID string) (*api.AllocationIntent, error) {
	return scanAllocationIntent(q.QueryRowContext(ctx, `SELECT `+allocationIntentCols+` FROM agent_allocation_intents WHERE agent_id=?`, agentID))
}

func loadAllocationIntentByRetryKey(q queryRower, ctx context.Context, targetTaskID, authorAgentID, authorRunID, requestID string) (*api.AllocationIntent, error) {
	return scanAllocationIntent(q.QueryRowContext(ctx, `SELECT `+allocationIntentCols+` FROM agent_allocation_intents WHERE target_task_id=? AND author_agent_id=? AND author_run_id=? AND request_id=?`,
		targetTaskID, authorAgentID, authorRunID, requestID))
}

// GetAllocationIntent is a plain readback by agent identity: it never
// consumes, authors, or mutates -- a durable receipt for an uncertain
// CreateAllocationIntent response, available even after consumption.
func (s *Store) GetAllocationIntent(ctx context.Context, taskID, agentID string) (api.AllocationIntent, error) {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(agentID, "agt") {
		return api.AllocationIntent{}, api.ErrInvalid
	}
	in, err := loadAllocationIntent(s.db, ctx, agentID)
	if err != nil {
		return api.AllocationIntent{}, err
	}
	if in == nil || in.TargetTaskID != taskID {
		return api.AllocationIntent{}, api.ErrNotFound
	}
	return *in, nil
}

func isUniqueConstraintErr(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "constraint failed"))
}
