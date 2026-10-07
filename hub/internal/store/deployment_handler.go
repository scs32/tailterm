package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/scs32/tailterm/hub/internal/api"
)

// ReleaseHandler resolves the project database handler for the deployment
// agent's own requests (integrated matrix import, job inputs, rollback bug).
// A finished release entry holds no item lease, so the item-scoped role rule
// refuses in parallel mode; the deployer instead uses the project rule: the
// explicit primary, else the newest open handler that is not the prepared
// successor of an open rotation. Retired handlers never receive its work.
func (s *Store) ReleaseHandler(ctx context.Context, task, agent, run string) (api.Agent, error) {
	if !api.ValidID(task, "tsk") {
		return api.Agent{}, api.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.Agent{}, err
	}
	defer tx.Rollback()
	if err = releaseDeployer(ctx, tx, task, agent, run, s.now()); err != nil {
		return api.Agent{}, err
	}
	var primary string
	if err = tx.QueryRowContext(ctx, `SELECT primary_handler_id FROM tasks WHERE id=?`, task).Scan(&primary); err != nil {
		return api.Agent{}, err
	}
	closed := []any{api.AgentClosed, api.AgentExited, api.AgentRetired}
	if primary != "" {
		a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=? AND role=? AND status NOT IN (?,?,?)`, append([]any{task, primary, api.AgentRoleDatabaseHandler}, closed...)...))
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return a, err
		}
	}
	a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status NOT IN (?,?,?) AND id NOT IN (SELECT successor_agent_id FROM handler_rotations WHERE task_id=? AND state='prepared') ORDER BY created_at DESC,id DESC LIMIT 1`, append(append([]any{task, api.AgentRoleDatabaseHandler}, closed...), task)...))
	if errors.Is(err, sql.ErrNoRows) {
		return a, releaseConflict("no available project database handler; resume or provision one")
	}
	return a, err
}
