package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"regexp"
	"strings"
)

var leadRunIDPattern = regexp.MustCompile(`^run_[a-f0-9]{16}$`)

func (s *Store) AssignLead(ctx context.Context, taskID string, req api.AssignLeadRequest, by api.Caller) (api.LeadAssignment, error) {
	var result api.LeadAssignment
	if len(req.RequestID) < 1 || len(req.RequestID) > 128 || req.ExpectedRevision < 0 || !api.ValidID(req.AgentID, "agt") || !leadRunIDPattern.MatchString(req.RunID) {
		return result, api.ErrInvalid
	}
	payload, _ := json.Marshal(struct {
		Request api.AssignLeadRequest
		Caller  api.Caller
	}{req, by})
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var savedPayload, savedResult string
	err = tx.QueryRowContext(ctx, `SELECT payload,result FROM lead_assignments WHERE task_id=? AND request_id=?`, taskID, req.RequestID).Scan(&savedPayload, &savedResult)
	if err == nil {
		if savedPayload != string(payload) {
			return result, fmt.Errorf("%w: lead retry key was reused with different input", api.ErrConflict)
		}
		err = json.Unmarshal([]byte(savedResult), &result)
		return result, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if err != nil {
		return result, err
	}
	if task.Status != api.TaskOpen {
		return result, api.ErrClosed
	}
	if task.LeadRevision != req.ExpectedRevision || task.Orchestrator != req.ExpectedName {
		return result, fmt.Errorf("%w: project lead changed; reload before choosing again", api.ErrConflict)
	}
	var previous api.Agent
	if task.Orchestrator != "" {
		previous, err = scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND name=? COLLATE NOCASE ORDER BY created_at DESC LIMIT 1`, taskID, task.Orchestrator))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, err
		}
	}
	if previous.ID != req.PreviousAgentID || previous.RunID != req.PreviousRunID {
		return result, fmt.Errorf("%w: previous lead run changed", api.ErrConflict)
	}
	agent, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=?`, taskID, req.AgentID))
	if err != nil {
		return result, err
	}
	var bound int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_work_item_bindings WHERE agent_id=? AND run_id=?`, agent.ID, agent.RunID).Scan(&bound); err != nil {
		return result, err
	}
	if agent.RunID != req.RunID || agent.Role != "" || bound > 0 || !agent.Online || (agent.Status != api.AgentRunning && agent.Status != api.AgentDone && agent.Status != api.AgentNeedsInput) || strings.EqualFold(agent.Name, task.Orchestrator) {
		return result, fmt.Errorf("%w: choose an online, available, unbound replacement run", api.ErrConflict)
	}
	task.Orchestrator = agent.Name
	task.LeadRevision++
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET orchestrator=?,lead_revision=? WHERE id=?`, task.Orchestrator, task.LeadRevision, task.ID); err != nil {
		return result, err
	}
	notice := fmt.Sprintf("Owner assigned %s (%s / %s) as project lead, replacing %s (%s / %s). Run tt brief to load your current orchestrator role and coordinate with the database handler for the verified project handoff and explicit Queue recipient reconciliation. Existing work-item assignments, historical messages and old sessions are preserved; this is not authorization to repeat or close existing work.", agent.Name, agent.ID, agent.RunID, req.ExpectedName, previous.ID, previous.RunID)
	message, err := s.insertMessage(ctx, tx, task, api.PostMessageRequest{To: agent.ID, Text: notice}, agent, by, false, false)
	if err != nil {
		return result, err
	}
	if _, err = s.insertEvent(ctx, tx, taskID, "task_updated", "", "Project lead assigned", map[string]any{"requestId": req.RequestID, "previousAgentId": previous.ID, "previousRunId": previous.RunID, "agentId": agent.ID, "runId": agent.RunID, "leadRevision": task.LeadRevision, "messageSeq": message.Seq}, by); err != nil {
		return result, err
	}
	result = api.LeadAssignment{Task: task, AgentID: agent.ID, RunID: agent.RunID, MessageSeq: message.Seq, RequestID: req.RequestID}
	encoded, _ := json.Marshal(result)
	if _, err = tx.ExecContext(ctx, `INSERT INTO lead_assignments(task_id,request_id,payload,result) VALUES(?,?,?,?)`, taskID, req.RequestID, string(payload), string(encoded)); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return api.LeadAssignment{}, err
	}
	s.notify(taskID)
	return result, nil
}

// Preserve legacy name-based projects, but do not let an assigned exact run
// silently become another run when downstream dispatch resolves its recipient.
func verifyAssignedLead(q queryRower, ctx context.Context, task api.Task, agent api.Agent) error {
	var encoded string
	err := q.QueryRowContext(ctx, `SELECT result FROM lead_assignments WHERE task_id=? ORDER BY rowid DESC LIMIT 1`, task.ID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved api.LeadAssignment
	if err = json.Unmarshal([]byte(encoded), &saved); err != nil {
		return err
	}
	if saved.Task.LeadRevision == task.LeadRevision && (saved.AgentID != agent.ID || saved.RunID != agent.RunID) {
		return workItemConflict("the assigned project lead run changed; explicitly replace the lead before dispatch")
	}
	return nil
}
