package store

import (
	"context"
	"database/sql"

	"github.com/scs32/tailterm/hub/internal/api"
)

// handlerRequest is derived from the immutable sender run and native primary
// item link. A post may cite a newer item revision than the frozen admission
// binding; item identity and sender run must still match. It remains true after
// a team closes, so response history survives closeout. live is deliberately
// recomputed for scheduling only.
type handlerRequest struct{ live bool }

type handlerPriorityQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func handlerRequests(ctx context.Context, q handlerPriorityQuerier, taskID, recipient string) (map[int64]handlerRequest, error) {
	query := `SELECT o.message_seq,
EXISTS (SELECT 1 FROM agent_work_item_bindings team
 JOIN agents member ON member.id=team.agent_id AND member.run_id=team.run_id
 WHERE team.item_task_id=source.item_task_id AND team.item_id=source.item_id
 AND team.item_revision=source.item_revision
 AND member.status IN (?,?,?) AND member.role<>?)
FROM obligations o
JOIN agents handler ON handler.id=o.agent_id AND handler.role=?
JOIN messages m ON m.task_id=o.task_id AND m.seq=o.message_seq
JOIN agent_work_item_bindings source ON source.agent_id=m.from_agent AND source.run_id=m.from_run_id
JOIN message_work_item_links link ON link.message_task_id=m.task_id AND link.message_seq=m.seq
 AND link.relationship='primary' AND link.item_task_id=source.item_task_id
 AND link.item_id=source.item_id AND link.item_revision>=source.item_revision
WHERE o.task_id=? AND o.source_kind=? AND o.needs=?`
	args := []any{api.AgentRunning, api.AgentDone, api.AgentNeedsInput, api.AgentRoleDatabaseHandler,
		api.AgentRoleDatabaseHandler, taskID, api.EnvelopeKindRequest, api.ObligationNeedsOutcome}
	if recipient != "" {
		query += ` AND o.agent_id=?`
		args = append(args, recipient)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]handlerRequest)
	for rows.Next() {
		var seq int64
		var live bool
		if err := rows.Scan(&seq, &live); err != nil {
			return nil, err
		}
		out[seq] = handlerRequest{live: live}
	}
	return out, rows.Err()
}
