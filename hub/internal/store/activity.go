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

func migrateActivity(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS agent_activity (
 task_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL,
 state TEXT NOT NULL, observed_at TEXT NOT NULL, payload TEXT NOT NULL,
 request_id TEXT NOT NULL, PRIMARY KEY(agent_id,run_id));
CREATE UNIQUE INDEX IF NOT EXISTS agent_activity_request ON agent_activity(task_id,request_id);
CREATE TABLE IF NOT EXISTS agent_activity_receipts (
 task_id TEXT NOT NULL, request_id TEXT NOT NULL, agent_id TEXT NOT NULL,
 run_id TEXT NOT NULL, payload TEXT NOT NULL, request_payload TEXT NOT NULL,
 PRIMARY KEY(task_id,request_id));`)
	return err
}

func validActivity(a api.AgentActivity) bool {
	switch a.State {
	case "working", "hung_tool", "finished_silent", "crashed", "looping", "idle", "unknown":
	default:
		return false
	}
	if a.ObservedAt.IsZero() || len(a.PendingTool) > 120 || len(a.Reason) > 240 || strings.ContainsAny(a.PendingTool+a.Reason, "\n\r") {
		return false
	}
	t := a.Tokens
	return t.Input >= 0 && t.Cached >= 0 && t.CacheWrite >= 0 && t.Output >= 0 && t.Reasoning >= 0 && t.Total >= 0
}

// ReportActivity records only a transition. The request receipt survives a lost
// HTTP reply, and a repeat state leaves the prior evidence/totals untouched.
func (s *Store) ReportActivity(ctx context.Context, task, agent string, report api.ActivityReport) (api.AgentActivity, error) {
	var zero api.AgentActivity
	if !api.ValidID(task, "tsk") || !api.ValidID(agent, "agt") || !validRequestID(report.RequestID) || !validActivity(report.Activity) || report.RunID == "" {
		return zero, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	requestBytes, _ := json.Marshal(report)
	var previous string
	var priorRequest, priorAgent, priorRun string
	err = tx.QueryRowContext(ctx, `SELECT payload,request_payload,agent_id,run_id FROM agent_activity_receipts WHERE task_id=? AND request_id=?`, task, report.RequestID).Scan(&previous, &priorRequest, &priorAgent, &priorRun)
	if err == nil {
		if priorRequest != string(requestBytes) || priorAgent != agent || priorRun != report.RunID {
			return zero, api.ErrConflict
		}
		var saved api.AgentActivity
		if json.Unmarshal([]byte(previous), &saved) != nil {
			return zero, api.ErrConflict
		}
		return saved, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	var run, status string
	if err = tx.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE id=? AND task_id=?`, agent, task).Scan(&run, &status); err != nil {
		return zero, err
	}
	if run != report.RunID || status == api.AgentClosed || status == api.AgentExited {
		return zero, api.ErrConflict
	}
	var oldState, oldPayload string
	err = tx.QueryRowContext(ctx, `SELECT state,payload FROM agent_activity WHERE agent_id=? AND run_id=?`, agent, run).Scan(&oldState, &oldPayload)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	if oldState == report.Activity.State {
		var saved api.AgentActivity
		if json.Unmarshal([]byte(oldPayload), &saved) != nil {
			return zero, api.ErrConflict
		}
		return saved, nil
	}
	payload, _ := json.Marshal(report.Activity)
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_activity(task_id,agent_id,run_id,state,observed_at,payload,request_id)
 VALUES(?,?,?,?,?,?,?) ON CONFLICT(agent_id,run_id) DO UPDATE SET state=excluded.state,observed_at=excluded.observed_at,payload=excluded.payload,request_id=excluded.request_id`, task, agent, run, report.Activity.State, report.Activity.ObservedAt.UTC().Format(time.RFC3339Nano), string(payload), report.RequestID); err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_activity_receipts(task_id,request_id,agent_id,run_id,payload,request_payload) VALUES(?,?,?,?,?,?)`, task, report.RequestID, agent, run, string(payload), string(requestBytes)); err != nil {
		return zero, err
	}
	if err := s.postActivityAlerts(ctx, tx, task, agent, run, report.Activity); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return report.Activity, nil
}

func (s *Store) postActivityAlerts(ctx context.Context, tx *sql.Tx, task, agent, run string, activity api.AgentActivity) error {
	if activity.State != "hung_tool" && activity.State != "crashed" && activity.State != "looping" {
		return nil
	}
	var item, leadID, leadRun string
	err := tx.QueryRowContext(ctx, `SELECT item_id FROM agent_work_item_bindings WHERE agent_id=? AND run_id=?`, agent, run).Scan(&item)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if item != "" {
		err = tx.QueryRowContext(ctx, `SELECT agent_id,run_id FROM item_team_leads WHERE task_id=? AND item_id=? AND state<>'closed'`, task, item).Scan(&leadID, &leadRun)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	project, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE tasks.id=?`, task))
	if err != nil {
		return err
	}
	refs := map[string]string{"agent": agent, "run": run, "activity": activity.State}
	if item != "" {
		refs["item"] = item
	}
	text := fmt.Sprintf("Agent activity changed to %s at %s. Last event %s.", activity.State, activity.ObservedAt.UTC().Format(time.RFC3339), activity.LastEventAt.UTC().Format(time.RFC3339))
	if activity.PendingTool != "" {
		text += fmt.Sprintf(" Pending tool %s since %s.", activity.PendingTool, activity.PendingSince.UTC().Format(time.RFC3339))
	}
	if leadID != "" && leadID != agent {
		lead, loadErr := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=? AND run_id=?`, leadID, leadRun))
		if loadErr == nil && lead.Status != api.AgentClosed && lead.Status != api.AgentExited {
			if err := s.postBrokerNotice(ctx, tx, project, lead, lead.Name, "Agent activity needs attention", "Agent activity needs attention", text, refs); err != nil {
				return err
			}
		}
	}
	if activity.State == "crashed" || (leadID == agent && (activity.State == "hung_tool" || activity.State == "looping")) {
		if err := s.postBrokerNotice(ctx, tx, project, api.Agent{}, "", "Agent activity needs owner attention", "Agent activity needs owner attention", text, refs); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) loadActivity(ctx context.Context, a *api.Agent) error {
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM agent_activity WHERE agent_id=? AND run_id=?`, a.ID, a.RunID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var activity api.AgentActivity
	if err := json.Unmarshal([]byte(payload), &activity); err != nil {
		return fmt.Errorf("activity snapshot: %w", err)
	}
	a.Activity = &activity
	return nil
}

func (s *Store) loadTeamActivities(ctx context.Context, entry *api.TeamQueueEntry) error {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,b.run_id,a.name,x.payload FROM agent_work_item_bindings b
 JOIN agents a ON a.id=b.agent_id
 LEFT JOIN agent_activity x ON x.agent_id=b.agent_id AND x.run_id=b.run_id
 WHERE b.item_task_id=? AND b.item_id=? ORDER BY a.created_at`, entry.TaskID, entry.ItemID)
	if err != nil {
		return err
	}
	defer rows.Close()
	entry.Activities = nil
	entry.Tokens = api.TokenTotals{}
	for rows.Next() {
		var member api.TeamAgentActivity
		var payload sql.NullString
		if err := rows.Scan(&member.AgentID, &member.RunID, &member.Name, &payload); err != nil {
			return err
		}
		if payload.Valid {
			var a api.AgentActivity
			if err := json.Unmarshal([]byte(payload.String), &a); err != nil {
				return err
			}
			member.Activity = &a
			entry.Tokens.Input += a.Tokens.Input
			entry.Tokens.Cached += a.Tokens.Cached
			entry.Tokens.CacheWrite += a.Tokens.CacheWrite
			entry.Tokens.Output += a.Tokens.Output
			entry.Tokens.Reasoning += a.Tokens.Reasoning
			entry.Tokens.Total += a.Tokens.Total
		}
		entry.Activities = append(entry.Activities, member)
	}
	return rows.Err()
}
