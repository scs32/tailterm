package store

import (
	"context"
	"fmt"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The handler floor keeps an open, active project from losing its last
// available database handler or its recorded primary by a retire or close
// (wi_96295d2375d72618, order #17042). An exit is never refused: the hub
// records it and escalates once to the owner when no handler is left.

// handlerFloorCheck decides whether a handler may move to next. It refuses a
// retire or close of the recorded primary, or of the last available handler,
// unless a rotation successor is ready, and reports whether the change takes
// the project's available handler count to zero.
func handlerFloorCheck(ctx context.Context, q queryRower, task api.Task, a api.Agent, next string) (crossesZero bool, err error) {
	if a.Role != api.AgentRoleDatabaseHandler || task.Status != api.TaskOpen || task.PauseState != api.ProjectPauseActive || a.Status == next {
		return false, nil
	}
	if next != api.AgentRetired && next != api.AgentClosed && next != api.AgentExited {
		return false, nil
	}
	refusable := next != api.AgentExited
	ready, err := handlerSuccessorReady(ctx, q, task.ID, a.ID)
	if err != nil {
		return false, err
	}
	if refusable && a.ID == task.PrimaryHandlerID && !ready {
		return false, fmt.Errorf("%w: %s is the project's primary database handler; rotate it with tt handler rotate before retiring or closing it", api.ErrConflict, a.Name)
	}
	if !handlerAvailable(a.Status) {
		return false, nil
	}
	var available int
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE task_id=? AND role=? AND status NOT IN (?,?,?)`,
		task.ID, api.AgentRoleDatabaseHandler, api.AgentClosed, api.AgentExited, api.AgentRetired).Scan(&available); err != nil {
		return false, err
	}
	if available > 1 {
		return false, nil
	}
	if refusable && !ready {
		return false, fmt.Errorf("%w: %s is the project's last available database handler; start or resume another handler first", api.ErrConflict, a.Name)
	}
	return true, nil
}

// handlerAvailable matches the deployer's and queue's notion of an available
// handler: any status but closed, exited or retired.
func handlerAvailable(status string) bool {
	return status != api.AgentClosed && status != api.AgentExited && status != api.AgentRetired
}

// handlerSuccessorReady reports a prepared rotation away from the handler or
// a committed rotation chain that ends at an open successor.
func handlerSuccessorReady(ctx context.Context, q queryRower, task, agentID string) (bool, error) {
	r, err := openHandlerRotation(ctx, q, task)
	if err != nil {
		return false, err
	}
	if r != nil && r.OldAgentID == agentID {
		return true, nil
	}
	successor, err := liveSuccessor(ctx, q, agentID)
	return successor != "", err
}

// escalateNoHandler posts one board-wide owner notice that the project has no
// available database handler left. The caller holds s.writeMu.
func (s *Store) escalateNoHandler(ctx context.Context, task api.Task, a api.Agent, next string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	text := fmt.Sprintf("Project %s (%s) has no available database handler: %s (%s, run %s) became %s. "+
		"Work-item requests, queue launches and deployment wait until a handler is resumed or provisioned (tt resume, or a new handler launch). "+
		"Automatic reprovisioning is follow-up wi_42be87739d7bbfc9.",
		task.Name, task.ID, a.Name, a.ID, a.RunID, next)
	refs := map[string]string{"escalation": "owner", "cause": "no-database-handler", "task": task.ID, "agent": a.ID, "run": a.RunID}
	if err = s.postBrokerNotice(ctx, tx, task, api.Agent{}, a.Name, "Owner attention: the project has no available database handler", "Owner attention: no available database handler", text, refs); err != nil {
		return err
	}
	return tx.Commit()
}
