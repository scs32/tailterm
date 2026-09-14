package store

import (
	"context"
	"database/sql"
	"github.com/scs32/tailterm/hub/internal/api"
)

// Closing is durable intent; only a host's matching-run receipt confirms cleanup.
func (s *Store) ReportCleanup(ctx context.Context, id string, req api.CleanupRequest, by api.Caller) (api.Agent, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	a, err := s.GetAgent(ctx, id)
	if err != nil {
		return a, err
	}
	if a.Status != api.AgentClosed || req.RunID == "" || req.RunID != a.RunID || !api.ValidText(req.Error, 500) {
		return a, api.ErrInvalid
	}
	done := req.Error == ""
	// Successful receipts are monotonic; a racing retry cannot undo confirmation.
	if a.CleanupDone || (a.CleanupDone == done && a.CleanupError == req.Error) {
		return a, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE agents SET cleanup_done=?,cleanup_error=? WHERE id=? AND run_id=? AND status=?`, done, req.Error, id, req.RunID, api.AgentClosed)
	if err != nil {
		return a, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return a, api.ErrConflict
	}
	a.CleanupDone, a.CleanupError = done, req.Error
	if _, err = s.insertEvent(ctx, tx, a.TaskID, "agent_cleanup", id, req.Error, map[string]any{"done": done, "runId": req.RunID}, by); err != nil {
		return a, err
	}
	if done {
		var generation int64
		err = tx.QueryRowContext(ctx, `SELECT pc.pause_generation FROM project_pause_cycles pc
JOIN project_pause_targets pt ON pt.cycle_id=pc.id
JOIN tasks t ON t.id=pc.task_id AND t.pause_generation=pc.pause_generation
WHERE pt.agent_id=? AND pt.run_id=? AND t.pause_state=?`, id, req.RunID, api.ProjectPauseCleanupPending).Scan(&generation)
		if err == nil {
			if err = reconcilePauseCycleTx(ctx, s, tx, a.TaskID, generation, by); err != nil {
				return a, err
			}
		} else if err != sql.ErrNoRows {
			return a, err
		}
	}
	if err = tx.Commit(); err != nil {
		return a, err
	}
	s.notify(a.TaskID)
	return a, nil
}
