package store

// The owner helper (docs/owner-helper.md): the project's one owner_helper
// agent, bound to the owner's own Claude Code session. Only the register route
// creates it. Registering again replaces its run; an exited helper gets a new
// run on the same agent; a closed helper is terminal, so a fresh agent is made.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// migrateOwnerHelper runs after the agents.role column exists (it is called
// from migrateActivity, after the column migrations).
func migrateOwnerHelper(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS owner_helper_registrations (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL,
 previous_run_id TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL, host TEXT NOT NULL, session TEXT NOT NULL,
 runtime TEXT NOT NULL, cwd TEXT NOT NULL DEFAULT '', request_id TEXT NOT NULL,
 by_node TEXT NOT NULL, by_user TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS owner_helper_registrations_agent ON owner_helper_registrations(task_id,agent_id,created_at);
CREATE UNIQUE INDEX IF NOT EXISTS agents_one_owner_helper ON agents(task_id) WHERE role='owner_helper' AND status NOT IN ('closed','exited');`)
	return err
}

// RegisterOwnerHelper creates or reattaches the project's owner helper, or
// replaces a live helper's run, and writes a receipt. It is an owner action:
// a retry with the same request ID replays the original result.
func (s *Store) RegisterOwnerHelper(ctx context.Context, taskID string, req api.RegisterOwnerHelperRequest, by api.Caller) (api.OwnerActionResult, error) {
	if err := api.ValidateRegisterOwnerHelper(&req); err != nil {
		return api.OwnerActionResult{}, err
	}
	return s.ownerAction(ctx, taskID, "owner-helper-register", api.AgentRoleOwnerHelper, req.RequestID, req, by, func(tx *sql.Tx, task api.Task, now time.Time) (api.OwnerActionResult, error) {
		// The pause barrier applies to create, reattach and replace alike.
		if task.PauseState != api.ProjectPauseActive {
			return api.OwnerActionResult{}, fmt.Errorf("%w: project is paused; register the owner helper after resume", api.ErrConflict)
		}
		helper, found, err := currentOwnerHelper(ctx, tx, taskID)
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		if found && helper.Name != req.Name {
			return api.OwnerActionResult{}, fmt.Errorf("%w: the owner helper is registered as %s; close it before choosing another name", api.ErrConflict, helper.Name)
		}
		var clash int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE task_id=? AND name=? COLLATE NOCASE AND status<>'closed' AND role<>?`, taskID, req.Name, api.AgentRoleOwnerHelper).Scan(&clash); err != nil {
			return api.OwnerActionResult{}, err
		}
		if clash > 0 {
			return api.OwnerActionResult{}, fmt.Errorf("%w: agent name %s is taken by another agent", api.ErrConflict, req.Name)
		}
		live := found && helper.Status != api.AgentExited
		if !live {
			// A new or reattached helper takes a live-agent slot.
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE task_id=? AND status NOT IN ('closed','exited')`, taskID).Scan(&count); err != nil {
				return api.OwnerActionResult{}, err
			}
			if count >= s.MaxAgents {
				return api.OwnerActionResult{}, api.ErrLimit
			}
		}
		r := api.OwnerHelperRegistration{ID: api.NewID("ohr"), TaskID: taskID, RunID: api.NewID("run"), Host: req.Host, Session: req.Session,
			Runtime: req.Runtime, Cwd: req.Cwd, RequestID: req.RequestID, By: api.Sender{Node: by.Node, User: by.User}, CreatedAt: now}
		switch {
		case !found:
			r.Mode, r.AgentID = api.OwnerHelperCreated, api.NewID("agt")
			// cleanup_done=1: the hub never owns the owner's terminal, so
			// closing the helper owes no host cleanup.
			if _, err := tx.ExecContext(ctx, `INSERT INTO agents (`+agentCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				r.AgentID, taskID, req.Name, req.Host, req.Session, req.Runtime, req.Cwd, "", api.AgentRoleOwnerHelper, api.AgentRunning, "", ts(now), ts(now), r.RunID, ts(now), "", "", true, ""); err != nil {
				return api.OwnerActionResult{}, err
			}
			// The helper reads from its registration on: earlier Board history
			// is not new input for it.
			if _, err := tx.ExecContext(ctx, `INSERT INTO read_cursors (task_id,agent_id,up_to) VALUES (?,?,(SELECT COALESCE(MAX(seq),0) FROM messages WHERE task_id=?))`, taskID, r.AgentID, taskID); err != nil {
				return api.OwnerActionResult{}, err
			}
		default:
			r.AgentID, r.PreviousRunID = helper.ID, helper.RunID
			r.Mode = api.OwnerHelperReplaced
			status := api.AgentRunning
			if helper.Status == api.AgentExited {
				r.Mode = api.OwnerHelperReattached
			} else if helper.Status == api.AgentRetired {
				// Retirement is the owner's choice; only tt resume undoes it.
				status = api.AgentRetired
			}
			result, err := tx.ExecContext(ctx, `UPDATE agents SET host=?,session=?,runtime=?,cwd=?,run_id=?,status=?,last_seen_at=?,last_event_at=?,cleanup_done=1,cleanup_error='',blocked_reason='',blocked_text='' WHERE id=? AND run_id=? AND status=?`,
				req.Host, req.Session, req.Runtime, req.Cwd, r.RunID, status, ts(now), ts(now), helper.ID, helper.RunID, helper.Status)
			if err != nil {
				return api.OwnerActionResult{}, err
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				return api.OwnerActionResult{}, fmt.Errorf("%w: the owner helper changed during registration", api.ErrConflict)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO owner_helper_registrations (id,task_id,agent_id,run_id,previous_run_id,mode,host,session,runtime,cwd,request_id,by_node,by_user,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.ID, taskID, r.AgentID, r.RunID, r.PreviousRunID, r.Mode, r.Host, r.Session, r.Runtime, r.Cwd, r.RequestID, by.Node, by.User, ts(now)); err != nil {
			return api.OwnerActionResult{}, err
		}
		data := map[string]any{"host": r.Host, "session": r.Session, "runtime": r.Runtime, "role": api.AgentRoleOwnerHelper, "runId": r.RunID, "registration": r.ID, "mode": r.Mode}
		if r.PreviousRunID != "" {
			data["previousRunId"] = r.PreviousRunID
		}
		if _, err := s.insertEvent(ctx, tx, taskID, api.EventAgentAdded, r.AgentID, req.Name, data, by); err != nil {
			return api.OwnerActionResult{}, err
		}
		a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, r.AgentID))
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		return api.OwnerActionResult{Agent: &a, Registration: &r}, nil
	})
}

// currentOwnerHelper is the project's helper that is not closed: live or exited.
func currentOwnerHelper(ctx context.Context, tx *sql.Tx, taskID string) (api.Agent, bool, error) {
	a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status<>? ORDER BY created_at DESC LIMIT 1`, taskID, api.AgentRoleOwnerHelper, api.AgentClosed))
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	return a, err == nil, err
}

// ListOwnerHelperRegistrations returns a project's registration receipts,
// newest first.
func (s *Store) ListOwnerHelperRegistrations(ctx context.Context, taskID string) ([]api.OwnerHelperRegistration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,task_id,agent_id,run_id,previous_run_id,mode,host,session,runtime,cwd,request_id,by_node,by_user,created_at FROM owner_helper_registrations WHERE task_id=? ORDER BY created_at DESC, rowid DESC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.OwnerHelperRegistration{}
	for rows.Next() {
		var r api.OwnerHelperRegistration
		var created string
		if err := rows.Scan(&r.ID, &r.TaskID, &r.AgentID, &r.RunID, &r.PreviousRunID, &r.Mode, &r.Host, &r.Session, &r.Runtime, &r.Cwd, &r.RequestID, &r.By.Node, &r.By.User, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTS(created)
		out = append(out, r)
	}
	return out, rows.Err()
}
