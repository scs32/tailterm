package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

// CloseItemTeam atomically records exact-run closure intents and clears the
// project's lead. Physical session cleanup remains a separate, retryable host
// operation. A receipt makes an uncertain HTTP response safe to replay.
func (s *Store) CloseItemTeam(ctx context.Context, taskID string, req api.TeamCloseRequest, by api.Caller) (api.TeamCloseResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var zero api.TeamCloseResult
	if !api.ValidID(taskID, "tsk") || !validRequestID(req.RequestID) || !api.ValidID(req.LeadAgentID, "agt") || !validRunID(req.LeadRunID) ||
		!api.ValidID(req.ItemID, "wi") || req.ItemRevision < 1 || req.LeadRevision < 0 || len(req.Members) == 0 || len(req.Members) > 500 ||
		(req.ActorAgentID == "") != (req.ActorRunID == "") || (req.ActorAgentID != "" && (!api.ValidID(req.ActorAgentID, "agt") || !validRunID(req.ActorRunID))) {
		return zero, api.ErrInvalid
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return zero, err
	}
	h := sha256.Sum256(encoded)
	hash := hex.EncodeToString(h[:])
	var oldHash, oldResult string
	err = s.db.QueryRowContext(ctx, `SELECT payload_hash,result_json FROM team_close_receipts WHERE task_id=? AND request_id=?`, taskID, req.RequestID).Scan(&oldHash, &oldResult)
	if err == nil {
		if oldHash != hash {
			return zero, fmt.Errorf("%w: team close request identity reused with different snapshot", api.ErrConflict)
		}
		var result api.TeamCloseResult
		if err := json.Unmarshal([]byte(oldResult), &result); err != nil {
			return zero, err
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	task, err := s.GetTask(ctx, taskID)
	if err != nil {
		return zero, err
	}
	if task.Status != api.TaskOpen || task.PauseState != api.ProjectPauseActive || task.Orchestrator == "" || task.LeadRevision != req.LeadRevision {
		return zero, &api.TeamCloseWaitError{Code: "team-close-snapshot", Text: "project or lead changed; refresh before team close"}
	}
	item, err := s.GetWorkItem(ctx, taskID, req.ItemID)
	if err != nil {
		return zero, err
	}
	if item.Revision < req.ItemRevision || (item.Status != "done" && item.Status != "dismissed") {
		return zero, fmt.Errorf("%w: item is not terminal at the selected revision", api.ErrConflict)
	}
	agents, err := s.ListAgents(ctx, taskID)
	if err != nil {
		return zero, err
	}
	var lead api.Agent
	actual := make([]api.TeamCloseMember, 0)
	for _, a := range agents {
		if a.ID == req.LeadAgentID {
			lead = a
		}
		if a.Role == api.AgentRoleDatabaseHandler || a.WorkItem == nil || a.WorkItem.ItemTaskID != taskID || a.WorkItem.ItemID != req.ItemID {
			continue
		}
		if a.Status != api.AgentClosed || !a.CleanupDone {
			actual = append(actual, api.TeamCloseMember{AgentID: a.ID, RunID: a.RunID, Host: a.Host, Status: a.Status})
		}
	}
	if lead.ID == "" || lead.Role == api.AgentRoleDatabaseHandler || lead.Status == api.AgentClosed || (lead.Status == api.AgentExited && req.ActorAgentID != "") ||
		!strings.EqualFold(lead.Name, task.Orchestrator) || lead.RunID != req.LeadRunID || lead.WorkItem == nil || lead.WorkItem.ItemTaskID != taskID || lead.WorkItem.ItemID != req.ItemID || lead.WorkItem.ItemRevision != req.ItemRevision {
		return zero, &api.TeamCloseWaitError{Code: "team-close-snapshot", Text: "selected lead is not the current item-scoped orchestrator"}
	}
	if req.ActorAgentID != "" && (req.ActorAgentID != lead.ID || req.ActorRunID != lead.RunID) {
		return zero, fmt.Errorf("%w: only the current item lead or an owner without agent identity may close the team", api.ErrConflict)
	}
	slices.SortFunc(actual, func(a, b api.TeamCloseMember) int { return strings.Compare(a.AgentID, b.AgentID) })
	if !slices.Equal(actual, req.Members) {
		return zero, &api.TeamCloseWaitError{Code: "team-close-snapshot", Text: "item team snapshot changed; refresh before team close"}
	}
	// Substantive work held or sent by a member must be resolved before close.
	// Delivery-only obligations held by members are closed with the recipients
	// in the same transaction below, regardless of who sent the notice.
	{
		team := `SELECT b.agent_id FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id WHERE b.item_task_id=? AND b.item_id=? AND a.role<>?`
		rows, err := s.db.QueryContext(ctx, `SELECT o.message_seq,o.state,o.agent_id,m.from_agent FROM obligations o JOIN messages m ON m.seq=o.message_seq WHERE o.task_id=? AND o.state<>? AND o.needs<>? AND (o.agent_id IN (`+team+`) OR m.from_agent IN (`+team+`)) ORDER BY o.message_seq`,
			taskID, api.ObligationClosed, api.ObligationNeedsDelivery, taskID, req.ItemID, api.AgentRoleDatabaseHandler, taskID, req.ItemID, api.AgentRoleDatabaseHandler)
		if err != nil {
			return zero, err
		}
		var open []string
		for rows.Next() {
			var seq int64
			var state, holder, sender string
			if err := rows.Scan(&seq, &state, &holder, &sender); err != nil {
				rows.Close()
				return zero, err
			}
			open = append(open, fmt.Sprintf("#%d %s holder=%s sender=%s", seq, state, holder, sender))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return zero, err
		}
		if len(open) > 0 {
			return zero, &api.TeamCloseWaitError{Code: "team-close-obligations", Text: "open team obligations: " + strings.Join(open, "; ")}
		}
	}
	result := api.TeamCloseResult{TaskID: taskID, ItemID: item.ID, LeadAgentID: lead.ID}
	for _, m := range actual {
		if m.AgentID != lead.ID {
			result.Members = append(result.Members, m)
		}
	}
	for _, m := range actual {
		if m.AgentID == lead.ID {
			result.Members = append(result.Members, m)
		}
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return zero, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	now := ts(s.now())
	for _, m := range result.Members {
		if _, err := tx.ExecContext(ctx, `UPDATE obligations SET state=?,outcome=?,reason=?,closed_at=?,changed_at=? WHERE task_id=? AND agent_id=? AND needs=? AND state<>?`,
			api.ObligationClosed, api.OutcomeRecipientGone, "recipient agent is closed", now, now,
			taskID, m.AgentID, api.ObligationNeedsDelivery, api.ObligationClosed); err != nil {
			return zero, err
		}
		if m.Status == api.AgentClosed {
			continue
		}
		r, err := tx.ExecContext(ctx, `UPDATE agents SET status=?,last_event_at=?,blocked_reason='',blocked_text='' WHERE id=? AND run_id=? AND status=?`, api.AgentClosed, now, m.AgentID, m.RunID, m.Status)
		if err != nil {
			return zero, err
		}
		n, err := r.RowsAffected()
		if err != nil || n != 1 {
			return zero, &api.TeamCloseWaitError{Code: "team-close-snapshot", Text: "team member changed during close"}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (task_id,kind,agent_id,text,data,by_node,by_user,created_at) VALUES (?,?,?,?,?,?,?,?)`, taskID, api.EventClosed, m.AgentID, "", "", by.Node, by.User, now); err != nil {
			return zero, err
		}
	}
	r, err := tx.ExecContext(ctx, `UPDATE tasks SET orchestrator='',lead_revision=lead_revision+1 WHERE id=? AND orchestrator=? AND lead_revision=? AND status=?`, taskID, task.Orchestrator, task.LeadRevision, api.TaskOpen)
	if err != nil {
		return zero, err
	}
	n, err := r.RowsAffected()
	if err != nil || n != 1 {
		return zero, &api.TeamCloseWaitError{Code: "team-close-snapshot", Text: "lead changed during close"}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (task_id,kind,agent_id,text,data,by_node,by_user,created_at) VALUES (?,?,?,?,?,?,?,?)`, taskID, "task_updated", "", task.Name, "", by.Node, by.User, now); err != nil {
		return zero, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO team_close_receipts(task_id,request_id,payload_hash,result_json,created_at) VALUES (?,?,?,?,?)`, taskID, req.RequestID, hash, string(resultJSON), now); err != nil {
		return zero, err
	}
	// Queue reservations stay until the runner confirms cleanup receipts.
	// A manual launch reservation is released by the exact team close.
	if _, err := tx.ExecContext(ctx, `DELETE FROM team_launch_reservations WHERE task_id=? AND entry_id='' AND item_id=?`, taskID, req.ItemID); err != nil {
		return zero, err
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(taskID)
	return result, nil
}

func (s *Store) GetTeamCloseReceipt(ctx context.Context, taskID, requestID string) (api.TeamCloseResult, error) {
	var out api.TeamCloseResult
	if !api.ValidID(taskID, "tsk") || !validRequestID(requestID) {
		return out, api.ErrInvalid
	}
	var encoded string
	err := s.db.QueryRowContext(ctx, `SELECT result_json FROM team_close_receipts WHERE task_id=? AND request_id=?`, taskID, requestID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return out, api.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal([]byte(encoded), &out)
	return out, err
}
