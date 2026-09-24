package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Broker phase 3.1 (docs/broker-phase-3.1.md): an agent that has been handed
// work cannot report, claim or coordinate until it acknowledges or answers
// that work. The hub enforces it; prompts only explain it.

// ackGate refuses a write from agentID while it holds an ack_outcome or
// answer obligation still queued or delivered after the grace period. A
// reply to one of those messages is the way out and is refused only when it
// names a stale run.
func ackGate(ctx context.Context, q queryRower, agentID, runID string, replyTo int64, now time.Time) error {
	if agentID == "" {
		return nil // people and the hub are never gated
	}
	rows, err := q.QueryContext(ctx, `SELECT o.message_seq,o.subject,COALESCE(a.name,''),m.from_node,m.from_agent FROM obligations o
JOIN messages m ON m.seq=o.message_seq LEFT JOIN agents a ON a.id=m.from_agent
WHERE o.agent_id=? AND o.state IN (?,?) AND o.needs IN (?,?) AND o.created_at<=? ORDER BY o.message_seq LIMIT 10`,
		agentID, api.ObligationQueued, api.ObligationDelivered, api.ObligationNeedsOutcome, api.ObligationNeedsAnswer, ts(now.Add(-api.ObligationAckGrace)))
	if err != nil {
		return err
	}
	defer rows.Close()
	var items []api.UnacknowledgedItem
	for rows.Next() {
		var it api.UnacknowledgedItem
		var name, node, fromAgent string
		if err := rows.Scan(&it.Seq, &it.Subject, &name, &node, &fromAgent); err != nil {
			return err
		}
		switch {
		case name != "":
			it.From = name
		case node == api.BrokerNode:
			it.From = "the broker"
		default:
			it.From = "the owner"
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil || len(items) == 0 {
		return err
	}
	if replyTo > 0 {
		var current string
		if err := q.QueryRowContext(ctx, `SELECT run_id FROM agents WHERE id=?`, agentID).Scan(&current); err != nil && err != sql.ErrNoRows {
			return err
		}
		// Replying is the way out, so only a reply from a known-stale run is
		// refused; one without a run passes but acknowledges nothing.
		if runID == "" || runID == current {
			for _, it := range items {
				if it.Seq == replyTo {
					return nil // replying to the work is how it is acknowledged
				}
			}
		}
	}
	return &api.UnacknowledgedError{Items: items}
}

// acknowledgeByReply records a reply from the recipient's current run as the
// acknowledgement of the obligation it answers (in the reply's transaction).
// A reply to a delivery-only notice proves delivery and closes it.
func acknowledgeByReply(ctx context.Context, tx *sql.Tx, m api.Message, req api.PostMessageRequest) error {
	if req.AgentID == "" || req.ReplyTo <= 0 || req.RunID == "" {
		return nil
	}
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT run_id FROM agents WHERE id=?`, req.AgentID).Scan(&current); err != nil || current != req.RunID {
		return nil // a stale run's reply acknowledges nothing
	}
	now := ts(m.CreatedAt)
	if _, err := tx.ExecContext(ctx, `UPDATE obligations SET state=?,acked_at=?,delivered_at=CASE WHEN delivered_at='' THEN ? ELSE delivered_at END,changed_at=?
WHERE task_id=? AND message_seq=? AND agent_id=? AND state IN (?,?) AND needs<>?`,
		api.ObligationAcknowledged, now, now, now, m.TaskID, req.ReplyTo, req.AgentID, api.ObligationQueued, api.ObligationDelivered, api.ObligationNeedsDelivery); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE obligations SET state=?,outcome=?,acked_at=?,delivered_at=CASE WHEN delivered_at='' THEN ? ELSE delivered_at END,closed_at=?,changed_at=?
WHERE task_id=? AND message_seq=? AND agent_id=? AND state IN (?,?) AND needs=?`,
		api.ObligationClosed, api.OutcomeDelivered, now, now, now, now, m.TaskID, req.ReplyTo, req.AgentID, api.ObligationQueued, api.ObligationDelivered, api.ObligationNeedsDelivery)
	return err
}
