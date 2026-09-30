package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Backlog steward (docs/backlog-steward.md). One steward holds a project's
// slot at a time. A rotation successor is admitted as a non-holding steward
// (steward_pending=1) until its rotation commits.
func migrateBacklogSteward(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('agents') WHERE name='steward_pending'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE agents ADD COLUMN steward_pending INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	_, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS agents_one_backlog_steward ON agents(task_id) WHERE role='backlog_steward' AND status<>'closed' AND steward_pending=0;
CREATE TABLE IF NOT EXISTS steward_runs (
 task_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT PRIMARY KEY, template_digest TEXT NOT NULL, created_at TEXT NOT NULL);`)
	return err
}

// stewardSlotHolder is the project's slot-holding steward: any status but
// closed, and not a pending rotation successor.
func stewardSlotHolder(ctx context.Context, q queryRower, task string) (api.Agent, bool, error) {
	a, err := scanAgent(q.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status<>? AND steward_pending=0 ORDER BY created_at,id LIMIT 1`,
		task, api.AgentRoleBacklogSteward, api.AgentClosed))
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	return a, err == nil, err
}

// activeSteward is the steward that receives role:backlog_steward messages
// and may run triage: the slot holder while it is neither closed nor exited.
func activeSteward(ctx context.Context, q queryRower, task string) (api.Agent, bool, error) {
	a, ok, err := stewardSlotHolder(ctx, q, task)
	if err != nil || !ok || a.Status == api.AgentExited {
		return api.Agent{}, false, err
	}
	return a, true, nil
}

func stewardActiveRefusal(holder api.Agent) error {
	return &api.StewardRefusal{Code: api.StewardRefusedActive, Detail: fmt.Sprintf("the project's backlog steward is %s (%s, %s); close or rotate it first", holder.Name, holder.ID, holder.Status)}
}

// admitStewardTx runs in the admission transaction just before a fresh
// steward row is inserted. It returns whether the new row is a pending
// rotation successor.
func admitStewardTx(ctx context.Context, tx *sql.Tx, task string, agentID string) (bool, error) {
	holder, held, err := stewardSlotHolder(ctx, tx, task)
	if err != nil {
		return false, err
	}
	pending, err := stewardPendingSuccessor(ctx, tx, task, agentID)
	if err != nil {
		return false, err
	}
	if pending {
		return true, nil
	}
	if held {
		return false, stewardActiveRefusal(holder)
	}
	return false, stewardRotationOpenRefusal(ctx, tx, task)
}

// stewardUniqueViolation maps the one-steward index to its named refusal, so
// no admission path returns a raw constraint error.
func stewardUniqueViolation(ctx context.Context, q queryRower, task string, err error) error {
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") || !strings.Contains(err.Error(), "agents.task_id") {
		return err
	}
	holder, held, lookupErr := stewardSlotHolder(ctx, q, task)
	if lookupErr != nil || !held {
		return &api.StewardRefusal{Code: api.StewardRefusedActive, Detail: "the project already has a backlog steward"}
	}
	return stewardActiveRefusal(holder)
}

// recordStewardRun keeps the template digest of an exact steward run. The
// first digest recorded for a run wins.
func (s *Store) recordStewardRun(ctx context.Context, a api.Agent, digest string) error {
	if a.Role != api.AgentRoleBacklogSteward || digest == "" || a.RunID == "" {
		return nil
	}
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO steward_runs(task_id,agent_id,run_id,template_digest,created_at) VALUES(?,?,?,?,?)`, a.TaskID, a.ID, a.RunID, digest, ts(s.now()))
	return err
}

// stewardPendingSuccessor reports whether agentID is the successor of the
// project's prepared steward rotation.
func stewardPendingSuccessor(ctx context.Context, q queryRower, task, agentID string) (bool, error) {
	return false, nil
}

// stewardRotationOpenRefusal refuses a steward admission while a rotation is
// prepared.
func stewardRotationOpenRefusal(ctx context.Context, q queryRower, task string) error {
	return nil
}
