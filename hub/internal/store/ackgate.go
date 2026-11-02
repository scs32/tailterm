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
// names a stale run. Every write but a Board post uses it.
func ackGate(ctx context.Context, q queryRower, agentID, runID string, replyTo int64, now time.Time) error {
	return ackGatePost(ctx, q, agentID, runID, replyTo, false, now)
}

// ackGatePost is ackGate for a Board post. reportShape says the post has the
// shape tt owner report --post sends: plain text, no envelope, no recipient
// and no work-item links. Only such a post can be the one scheduled owner
// report the gate also lets through: the first reply to its own latest wake
// notice, from the current run (ownerReportWakeReply).
func ackGatePost(ctx context.Context, q queryRower, agentID, runID string, replyTo int64, reportShape bool, now time.Time) error {
	if agentID == "" {
		return nil // people and the hub are never gated
	}
	// The deployment daemon cannot acknowledge messages, and its release
	// actions are fenced by exact run and job generation instead, so a
	// directed request must not stall its fence checks or escalations.
	var role string
	if err := q.QueryRowContext(ctx, `SELECT role FROM agents WHERE id=?`, agentID).Scan(&role); err != nil && err != sql.ErrNoRows {
		return err
	}
	if role == api.AgentRoleDeployment {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT o.message_seq,o.subject,COALESCE(a.name,''),m.from_node,m.from_user FROM obligations o
JOIN messages m ON m.seq=o.message_seq LEFT JOIN agents a ON a.id=m.from_agent
WHERE o.agent_id=? AND o.state IN (?,?) AND o.needs IN (?,?) AND o.created_at<=? ORDER BY o.message_seq`,
		agentID, api.ObligationQueued, api.ObligationDelivered, api.ObligationNeedsOutcome, api.ObligationNeedsAnswer, ts(now.Add(-api.ObligationAckGrace)))
	if err != nil {
		return err
	}
	defer rows.Close()
	var items []api.UnacknowledgedItem
	for rows.Next() {
		var it api.UnacknowledgedItem
		var name, node, user string
		if err := rows.Scan(&it.Seq, &it.Subject, &name, &node, &user); err != nil {
			return err
		}
		switch {
		case name != "":
			it.From = name
		case node == api.BrokerNode:
			it.From = "the broker"
		case user != "" && user != "owner":
			it.From = user
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
		if reportShape && runID != "" && runID == current {
			ok, err := ownerReportWakeReply(ctx, q, agentID, replyTo)
			if err != nil || ok {
				return err
			}
		}
	}
	return &api.UnacknowledgedError{Items: items}
}

// ownerReportWakeReply reports whether a post replying to replyTo is the one
// scheduled owner report that closes its wake notice (wi_475a3df9b345e13e g1;
// docs/owner-report.md, "Sending on the hub schedule"). It holds only when
// replyTo is the notice of the latest firing of an hourly-update,
// morning-summary or evening-summary wake, delivered to agentID, and agentID
// has not replied to that notice yet. The hub checks its own firing record,
// so no kind, text or request ID can claim it; an older slot's notice, and
// every other post, reply and write, stays gated.
func ownerReportWakeReply(ctx context.Context, q queryRower, agentID string, replyTo int64) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM owner_helper_wake_firings f JOIN owner_helper_wakes w ON w.id=f.wake_id AND w.task_id=f.task_id
WHERE f.notice_seq=? AND f.recipient_id=? AND w.kind IN (?,?,?)
AND NOT EXISTS (SELECT 1 FROM owner_helper_wake_firings later WHERE later.wake_id=f.wake_id AND later.due_at>f.due_at)
AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.task_id=f.task_id AND m.reply_to=f.notice_seq AND m.from_agent=?)`,
		replyTo, agentID, ownerReportWakeKinds[0], ownerReportWakeKinds[1], ownerReportWakeKinds[2], agentID).Scan(&n)
	return n > 0, err
}

// ownerReportWakeKinds are the wakes whose step is a scheduled owner report.
var ownerReportWakeKinds = [3]string{"hourly-update", "morning-summary", "evening-summary"}

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
	// The same reset as tt ack: activity clears nudges and escalation.
	if _, err := tx.ExecContext(ctx, `UPDATE obligations SET state=?,acked_at=?,delivered_at=CASE WHEN delivered_at='' THEN ? ELSE delivered_at END,last_progress_at=?,reason='',escalation=0,escalated_at='',nudges=0,nudged_at='',changed_at=?
WHERE task_id=? AND message_seq=? AND agent_id=? AND state IN (?,?) AND needs<>?`,
		api.ObligationAcknowledged, now, now, now, now, m.TaskID, req.ReplyTo, req.AgentID, api.ObligationQueued, api.ObligationDelivered, api.ObligationNeedsDelivery); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE obligations SET state=?,outcome=?,acked_at=?,delivered_at=CASE WHEN delivered_at='' THEN ? ELSE delivered_at END,closed_at=?,changed_at=?
WHERE task_id=? AND message_seq=? AND agent_id=? AND state IN (?,?) AND needs=?`,
		api.ObligationClosed, api.OutcomeDelivered, now, now, now, now, m.TaskID, req.ReplyTo, req.AgentID, api.ObligationQueued, api.ObligationDelivered, api.ObligationNeedsDelivery)
	return err
}
