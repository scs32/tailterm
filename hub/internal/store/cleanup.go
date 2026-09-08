package store

import (
	"context"
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
	task, err := s.GetTask(ctx, a.TaskID)
	if err != nil {
		return a, err
	}
	if task.Status != api.TaskClosed || a.Status != api.AgentClosed || req.RunID == "" || req.RunID != a.RunID || !api.ValidText(req.Error, 500) {
		return a, api.ErrInvalid
	}
	done := req.Error == ""
	// Successful receipts are monotonic; a racing retry cannot undo confirmation.
	if a.CleanupDone || (a.CleanupDone == done && a.CleanupError == req.Error) {
		return a, nil
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE agents SET cleanup_done=?,cleanup_error=? WHERE id=?`, done, req.Error, id); err != nil {
		return a, err
	}
	a.CleanupDone, a.CleanupError = done, req.Error
	_, err = s.addEvent(ctx, a.TaskID, "agent_cleanup", id, req.Error, map[string]any{"done": done}, by)
	return a, err
}
