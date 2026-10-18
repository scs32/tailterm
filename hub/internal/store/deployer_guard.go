package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The deployer guard keeps a deployment agent from being retired or closed
// while a claimed or merged release job names it as holder: every release
// action is refused for an unavailable deployer, so the job's next fence
// check fails and the release is left held (wi_42bdd9989b3c098c, order
// #20613). An exit is never refused: the hub records it. The refusal names
// the ways out (wi_27745813129a396b, order #38456): a clean stop of the
// runner, which unclaims a claimed job with no effects as it exits; the
// handler's set-aside; and, for a run already dead, its exit record and the
// handler's reconcile.

// deployerGuardCheck refuses a retire or close of a deployment agent that
// holds a claimed or merged release job in an open, active project.
func deployerGuardCheck(ctx context.Context, q queryRower, task api.Task, a api.Agent, next string) error {
	if a.Role != api.AgentRoleDeployment || task.Status != api.TaskOpen || task.PauseState != api.ProjectPauseActive || a.Status == next {
		return nil
	}
	if next != api.AgentRetired && next != api.AgentClosed {
		return nil
	}
	var job, state string
	err := q.QueryRowContext(ctx, `SELECT id,state FROM release_jobs WHERE task_id=? AND state IN ('claimed','merged') AND json_extract(record_json,'$.agentId')=? ORDER BY rowid LIMIT 1`,
		task.ID, a.ID).Scan(&job, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	action := "retiring"
	if next == api.AgentClosed {
		action = "closing"
	}
	return fmt.Errorf("%w: %s holds %s release job %s; %s it now fails the job's next fence check and leaves the release held. "+
		"Wait for the job to finish; or, while it is claimed with no effects, stop the deployer's runner cleanly (SIGTERM or SIGINT), "+
		"which returns the job to verified with an exact-run unclaim record and exits; or have the database handler set it aside "+
		"while it has no effects. If the holding run is already dead, record its exit (the hub never refuses an exit) and have the "+
		"database handler reconcile the job. Then retry (docs/project-deployment.md, \"Pause\")", api.ErrConflict, a.Name, state, job, action)
}
