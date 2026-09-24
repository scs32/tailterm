package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// BrokerObligation is an open obligation with the context the broker scheduler
// needs. Everything here is read from durable state, so a restarted hub makes
// the same decisions.
type BrokerObligation struct {
	api.Obligation
	Wakes        int
	EscalatedAt  time.Time
	NudgedAt     time.Time
	ChangedAt    time.Time
	AgentName    string
	AgentStatus  string
	Orchestrator string
	TaskPaused   bool
}

func (s *Store) BrokerOpenObligations(ctx context.Context) ([]BrokerObligation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.id,o.task_id,o.message_seq,o.agent_id,o.subject,o.source_kind,o.needs,o.state,o.outcome,o.outcome_seq,o.reason,
o.created_at,o.ack_due_at,o.due_at,o.delivered_at,o.acked_at,o.last_progress_at,o.closed_at,o.escalation,o.nudges,
o.wakes,o.escalated_at,o.nudged_at,o.changed_at,COALESCE(a.name,''),COALESCE(a.status,''),t.orchestrator,t.pause_state
FROM obligations o JOIN tasks t ON t.id=o.task_id LEFT JOIN agents a ON a.id=o.agent_id
WHERE o.state<>? AND t.status=? ORDER BY o.task_id,o.message_seq`, api.ObligationClosed, api.TaskOpen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BrokerObligation
	for rows.Next() {
		var b BrokerObligation
		var created, ackDue, due, delivered, acked, progress, closed, escalated, nudged, changed, pause string
		if err := rows.Scan(&b.ID, &b.TaskID, &b.MessageSeq, &b.AgentID, &b.Subject, &b.SourceKind, &b.Needs, &b.State, &b.Outcome, &b.OutcomeSeq, &b.Reason,
			&created, &ackDue, &due, &delivered, &acked, &progress, &closed, &b.Escalation, &b.Nudges,
			&b.Wakes, &escalated, &nudged, &changed, &b.AgentName, &b.AgentStatus, &b.Orchestrator, &pause); err != nil {
			return nil, err
		}
		b.CreatedAt, b.AckDueAt, b.DueAt = parseTS(created), parseTS(ackDue), parseTS(due)
		for _, f := range []struct {
			raw string
			dst **time.Time
		}{{delivered, &b.DeliveredAt}, {acked, &b.AckedAt}, {progress, &b.LastProgressAt}} {
			if f.raw != "" {
				t := parseTS(f.raw)
				*f.dst = &t
			}
		}
		if escalated != "" {
			b.EscalatedAt = parseTS(escalated)
		}
		if nudged != "" {
			b.NudgedAt = parseTS(nudged)
		}
		b.ChangedAt = parseTS(changed)
		b.TaskPaused = pause != api.ProjectPauseActive
		out = append(out, b)
	}
	return out, rows.Err()
}

// brokerTx runs fn in a write transaction and wakes task waiters afterwards.
func (s *Store) brokerTx(ctx context.Context, taskID string, fn func(*sql.Tx, api.Task) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if err != nil {
		return err
	}
	if err := fn(tx, task); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify(taskID)
	return nil
}

// BrokerWake queues one more wake job for an obligation.
func (s *Store) BrokerWake(ctx context.Context, o BrokerObligation, now time.Time) error {
	return s.brokerTx(ctx, o.TaskID, func(tx *sql.Tx, _ api.Task) error {
		if err := insertWakeJob(ctx, tx, o.TaskID, o.ID, o.AgentID, now, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE obligations SET wakes=wakes+1 WHERE id=? AND state<>?`, o.ID, api.ObligationClosed)
		return err
	})
}

// BrokerCloseRecipientGone closes an obligation whose recipient was closed or exited.
func (s *Store) BrokerCloseRecipientGone(ctx context.Context, o BrokerObligation, now time.Time) error {
	return s.brokerTx(ctx, o.TaskID, func(tx *sql.Tx, _ api.Task) error {
		_, err := tx.ExecContext(ctx, `UPDATE obligations SET state=?,outcome=?,reason=?,closed_at=?,changed_at=? WHERE id=? AND state<>?`,
			api.ObligationClosed, api.OutcomeRecipientGone, "recipient agent is "+o.AgentStatus, ts(now), ts(now), o.ID, api.ObligationClosed)
		return err
	})
}

func obligationNoun(kind string) string {
	switch kind {
	case api.EnvelopeKindAssign:
		return "an assignment"
	case api.EnvelopeKindReview:
		return "a review"
	case api.EnvelopeKindQuestion:
		return "a question"
	case api.EnvelopeKindBlock:
		return "a block"
	case "human":
		return "a message from the owner"
	}
	return "a request"
}

// postBrokerNotice inserts a hub-authored notice. When the subject names an
// agent that the envelope rules reject, it falls back to a generic subject.
func (s *Store) postBrokerNotice(ctx context.Context, tx *sql.Tx, task api.Task, to api.Agent, subject, fallback, text string, refs map[string]string) error {
	env := &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: subject, Refs: refs, Body: api.EnvelopeBody{Text: text}}
	if to.ID != "" {
		env.To = to.Name
	}
	if api.ValidateEnvelope(*env) != nil {
		env.Subject = fallback
	}
	_, err := s.insertMessageWithResume(ctx, tx, task, api.PostMessageRequest{Envelope: env, To: to.ID}, to, BrokerCaller, false, false, false)
	return err
}

func obligationRefs(o BrokerObligation) map[string]string {
	return map[string]string{"obligation": o.ID, "message": fmt.Sprint(o.MessageSeq)}
}

// BrokerNudge reminds a recipient that has gone quiet and wakes it again.
func (s *Store) BrokerNudge(ctx context.Context, o BrokerObligation, now time.Time) error {
	return s.brokerTx(ctx, o.TaskID, func(tx *sql.Tx, task api.Task) error {
		to, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, o.AgentID))
		if err != nil {
			return err
		}
		text := fmt.Sprintf("Message #%d (%s) has had no recorded progress for %s. Record progress with `tt progress %d --text ...`, "+
			"or reply with `tt send --reply-to %d` (result, answer, decline, or block with what you need).", o.MessageSeq, o.Subject, api.ObligationSilenceNudge, o.MessageSeq, o.MessageSeq)
		if err := s.postBrokerNotice(ctx, tx, task, to, "Reminder: no progress recorded on "+obligationNoun(o.SourceKind), "Reminder: no progress recorded on an open obligation", text, obligationRefs(o)); err != nil {
			return err
		}
		if err := insertWakeJob(ctx, tx, o.TaskID, o.ID, o.AgentID, now, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE obligations SET nudges=nudges+1,nudged_at=?,wakes=wakes+1 WHERE id=?`, ts(now), o.ID)
		return err
	})
}

// BrokerEscalate raises an overdue obligation to the project lead (level 1)
// or, when the lead is the recipient, missing or unresponsive, to the owner (level 2).
func (s *Store) BrokerEscalate(ctx context.Context, o BrokerObligation, level int, reason string, now time.Time) error {
	return s.brokerTx(ctx, o.TaskID, func(tx *sql.Tx, task api.Task) error {
		var lead api.Agent
		if level == 1 && task.Orchestrator != "" {
			l, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND name=? AND status<>? ORDER BY created_at DESC LIMIT 1`, o.TaskID, task.Orchestrator, api.AgentClosed))
			if err == nil && l.ID != o.AgentID {
				lead = l
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if lead.ID == "" {
			level = 2
		}
		who := o.AgentName
		if who == "" {
			who = "an agent"
		}
		text := fmt.Sprintf("Message #%d (%s) to %s is overdue: %s. Sent %s; state %s. "+
			"Options: nudge the recipient, reassign the obligation (tt obligations --overdue lists it), or answer for them.",
			o.MessageSeq, o.Subject, who, reason, o.CreatedAt.UTC().Format(time.RFC3339), o.State)
		subject := fmt.Sprintf("Overdue: %s on %s", who, obligationNoun(o.SourceKind))
		if level == 2 {
			subject = fmt.Sprintf("Owner attention: %s is overdue on %s", who, obligationNoun(o.SourceKind))
		}
		if err := s.postBrokerNotice(ctx, tx, task, lead, subject, "Overdue obligation needs attention", text, obligationRefs(o)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE obligations SET escalation=?,escalated_at=? WHERE id=?`, level, ts(now), o.ID)
		return err
	})
}

// BrokerProjectStall raises one board notice per stall: overdue obligations
// and no obligation change for the quiet period. A new change re-arms it.
func (s *Store) BrokerProjectStall(ctx context.Context, taskID string, lastChange time.Time, overdue []BrokerObligation, now time.Time) (bool, error) {
	notified := false
	err := s.brokerTx(ctx, taskID, func(tx *sql.Tx, task api.Task) error {
		var observed, notifiedAt string
		err := tx.QueryRowContext(ctx, `SELECT observed_change,notified_at FROM project_stalls WHERE task_id=?`, taskID).Scan(&observed, &notifiedAt)
		if errors.Is(err, sql.ErrNoRows) || observed != ts(lastChange) {
			_, err = tx.ExecContext(ctx, `INSERT INTO project_stalls (task_id,observed_change,notified_at) VALUES (?,?,'') ON CONFLICT(task_id) DO UPDATE SET observed_change=excluded.observed_change,notified_at=''`, taskID, ts(lastChange))
			if err != nil || len(overdue) == 0 {
				return err
			}
			notifiedAt = ""
		} else if err != nil {
			return err
		}
		if notifiedAt != "" || len(overdue) == 0 || now.Sub(lastChange) < api.ObligationProjectStallQuiet {
			return nil
		}
		var lines []string
		for _, o := range overdue {
			lines = append(lines, fmt.Sprintf("#%d %s → %s (%s)", o.MessageSeq, o.Subject, o.AgentName, ObligationOverdue(o.Obligation, now)))
		}
		text := fmt.Sprintf("No obligation in this project has changed for %s and %d are overdue: %s. Run `tt obligations --overdue` for details.",
			api.ObligationProjectStallQuiet, len(overdue), strings.Join(lines, "; "))
		if err := s.postBrokerNotice(ctx, tx, task, api.Agent{}, "Project stalled: overdue work and no progress", "Project stalled: overdue work and no progress", text, map[string]string{"overdue": fmt.Sprint(len(overdue))}); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE project_stalls SET notified_at=? WHERE task_id=?`, ts(now), taskID)
		notified = err == nil
		return err
	})
	return notified, err
}
