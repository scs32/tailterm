package store

import (
	"context"
	"fmt"

	"github.com/scs32/tailterm/hub/internal/api"
)

// CloseAgentRun records closure intent only for the exact current run. Session
// termination is confirmed separately through ReportCleanup.
func (s *Store) CloseAgentRun(ctx context.Context, id, runID string, by api.Caller) (api.Agent, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !validRunID(runID) {
		return api.Agent{}, api.ErrInvalid
	}
	a, err := s.GetAgent(ctx, id)
	if err != nil {
		return a, err
	}
	if a.RunID != runID {
		return a, fmt.Errorf("%w: agent run changed; refresh before closing", api.ErrConflict)
	}
	if a.Status == api.AgentClosed {
		return a, nil
	}
	task, err := s.GetTask(ctx, a.TaskID)
	if err != nil {
		return a, err
	}
	if task.Status != api.TaskOpen {
		return a, api.ErrClosed
	}
	return s.setAgentStatus(ctx, id, api.AgentClosed, by, true)
}
