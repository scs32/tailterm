package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
	// RelayBound is true when the holder's current run has an activity
	// report. The host relay reports activity only for a session it is
	// bound to, so a run without one is not bound (as far as the hub knows).
	RelayBound bool
}

func (s *Store) BrokerOpenObligations(ctx context.Context) ([]BrokerObligation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.id,o.task_id,o.message_seq,o.agent_id,o.subject,o.source_kind,o.needs,o.state,o.outcome,o.outcome_seq,o.reason,
o.created_at,o.ack_due_at,o.due_at,o.delivered_at,o.acked_at,o.last_progress_at,o.closed_at,o.escalation,o.nudges,
o.wakes,o.escalated_at,o.nudged_at,o.changed_at,COALESCE(a.name,''),COALESCE(a.status,''),t.orchestrator,t.pause_state,o.recipient_kind,
EXISTS(SELECT 1 FROM agent_activity aa WHERE aa.agent_id=o.agent_id AND aa.run_id=a.run_id)
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
			&b.Wakes, &escalated, &nudged, &changed, &b.AgentName, &b.AgentStatus, &b.Orchestrator, &pause, &b.RecipientKind, &b.RelayBound); err != nil {
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

// ErrBrokerStale means the obligation or its project changed after the
// scheduler read it; the action is skipped and decided afresh next tick.
var ErrBrokerStale = errors.New("obligation changed since the broker read it")

// ErrBrokerQueuedFix means an owner escalation was held back because the
// obligation's block waits on a fix that is already queued.
var ErrBrokerQueuedFix = errors.New("obligation is blocked on a queued fix")

// blockedOnQueuedFix reports whether a blocked obligation waits on queued
// work: the holder's latest BLOCK reply to it links an item (primary or
// related) whose team queue entry is queued, launching or running and not
// released. The owner has nothing to decide there, so no owner escalation
// is posted until that entry finishes, is removed, fails or is released.
// Progress messages (wi_e76872a6abc6d78e) postpone the due time instead;
// see applyMessageProgress.
func blockedOnQueuedFix(ctx context.Context, q queryRower, o BrokerObligation) (bool, error) {
	block, err := latestBlock(ctx, q, o)
	if block == 0 || err != nil {
		return false, err
	}
	var queued int
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_entries q WHERE q.released_at='' AND q.state IN ('queued','launching','running') AND EXISTS (
 SELECT 1 FROM (SELECT item_task_id,item_id FROM message_work_item_links WHERE message_task_id=? AND message_seq=?
  UNION SELECT item_task_id,item_id FROM message_audit_links WHERE message_task_id=? AND message_seq=?) l
 WHERE l.item_task_id=q.task_id AND l.item_id=q.item_id)`, o.TaskID, block, o.TaskID, block).Scan(&queued)
	return queued > 0, err
}

// latestBlock is the seq of the holder's latest BLOCK reply to a blocked
// obligation, or 0 when it is not blocked or has none.
func latestBlock(ctx context.Context, q queryRower, o BrokerObligation) (int64, error) {
	if o.State != api.ObligationBlocked {
		return 0, nil
	}
	var block int64
	err := q.QueryRowContext(ctx, `SELECT seq FROM messages WHERE task_id=? AND reply_to=? AND from_agent=? AND CASE WHEN json_valid(envelope) THEN json_extract(envelope,'$.kind') END=? ORDER BY seq DESC LIMIT 1`, o.TaskID, o.MessageSeq, o.AgentID, api.EnvelopeKindBlock).Scan(&block)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return block, err
}

// ErrBrokerPlannedHold means an owner escalation (and the project stall
// notice) was held back because the holder blocked the obligation on a
// release job that is on a recorded hold. It wraps ErrBrokerQueuedFix, so
// the scheduler skips it the same way.
var ErrBrokerPlannedHold = fmt.Errorf("obligation is blocked on a held release job: %w", ErrBrokerQueuedFix)

// blockedOnReleaseHold reports whether a blocked obligation waits on a
// planned release hold (wi_d1e42f0739e47139 p1): a ref of its request, or of
// the holder's latest BLOCK reply to it, names a release job of the project
// that is in state held. The hold is recorded with its resume condition (an
// unhold, or the named items releasing), so the owner has nothing to decide
// until it lifts; the next pass after that escalates normally.
func blockedOnReleaseHold(ctx context.Context, q queryRower, o BrokerObligation) (bool, error) {
	block, err := latestBlock(ctx, q, o)
	if block == 0 || err != nil {
		return false, err
	}
	var held int
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM release_jobs j WHERE j.task_id=? AND j.state='held' AND j.id IN (
 SELECT r.value FROM messages m, json_each(CASE WHEN json_valid(m.envelope) THEN m.envelope ELSE '{}' END,'$.refs') r
 WHERE m.task_id=? AND m.seq IN (?,?) AND r.type='text')`, o.TaskID, o.TaskID, o.MessageSeq, block).Scan(&held)
	return held > 0, err
}

// ErrBrokerProgressFresh means a lead re-escalation was held back because
// the holder recorded progress after the due time and that progress is
// fresher than the silence window. It wraps ErrBrokerQueuedFix, so the
// scheduler skips it the same way.
var ErrBrokerProgressFresh = fmt.Errorf("obligation holder recorded progress past the due time: %w", ErrBrokerQueuedFix)

// progressPastDue reports whether progress recorded after the due time still
// holds the outcome escalation (wi_d1e42f0739e47139 p6). Recording progress
// clears the escalation but keeps the due time, so without this every
// `tt progress` past due re-escalated to the lead on the next pass. Progress
// past the due time extends the outcome window to ObligationSilenceNudge
// after the latest progress: one lead notice per overdue episode, and a new
// one once progress has stopped for that window.
func progressPastDue(o BrokerObligation, now time.Time) bool {
	p := o.LastProgressAt
	return p != nil && p.After(o.DueAt) && now.Sub(*p) < api.ObligationSilenceNudge
}

// ErrBrokerMatrixWait means an owner escalation was held back because the
// holder is in a matrix wait. It wraps ErrBrokerQueuedFix, so the scheduler
// skips it the same way: the owner escalation is decided afresh next tick.
var ErrBrokerMatrixWait = fmt.Errorf("obligation holder is in a matrix wait: %w", ErrBrokerQueuedFix)

// inMatrixWait reports whether the holder's current run is waiting for or
// holding the host's matrix lock, or ended that wait less than
// ObligationOwnerAfterLead ago, as the activity report recorded it in
// stall_watch_progress. The holder has a known reason and a known resume
// condition, so the owner hears only after a full lead window from the
// wait's end. A wait recorded on an earlier run does not count. A wait whose
// ending report never arrives holds no longer than the stall watch counts
// it (wi_d1e42f0739e47139 p5): defaultQueueMatrixWaitBound from the wait's
// start in the run's latest activity report.
func inMatrixWait(ctx context.Context, q queryRower, o BrokerObligation, now time.Time) (bool, error) {
	var waiting bool
	var ended, payload string
	err := q.QueryRowContext(ctx, `SELECT p.matrix_wait,p.wait_ended_at,COALESCE(aa.payload,'') FROM agents a JOIN stall_watch_progress p ON p.agent_id=a.id AND p.run_id=a.run_id
 LEFT JOIN agent_activity aa ON aa.agent_id=a.id AND aa.run_id=a.run_id WHERE a.id=?`, o.AgentID).Scan(&waiting, &ended, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if waiting {
		var activity api.AgentActivity
		waiting = json.Unmarshal([]byte(payload), &activity) == nil && activity.MatrixWait.Valid() && now.Sub(activity.MatrixWait.Since) < defaultQueueMatrixWaitBound
	}
	return waiting || (ended != "" && now.Sub(parseTS(ended)) < api.ObligationOwnerAfterLead), nil
}

// brokerAct runs fn only if the obligation is exactly as the scheduler saw it
// and its project is still open and not paused.
func (s *Store) brokerAct(ctx context.Context, o BrokerObligation, fn func(*sql.Tx, api.Task) error) error {
	return s.brokerTx(ctx, o.TaskID, func(tx *sql.Tx, task api.Task) error {
		if task.Status != api.TaskOpen || task.PauseState != api.ProjectPauseActive {
			return ErrBrokerStale
		}
		var state, changed string
		var escalation, nudges, wakes int
		if err := tx.QueryRowContext(ctx, `SELECT state,escalation,nudges,wakes,changed_at FROM obligations WHERE id=?`, o.ID).Scan(&state, &escalation, &nudges, &wakes, &changed); err != nil {
			return err
		}
		if state != o.State || escalation != o.Escalation || nudges != o.Nudges || wakes != o.Wakes || !parseTS(changed).Equal(o.ChangedAt) {
			return ErrBrokerStale
		}
		return fn(tx, task)
	})
}

// BrokerWake queues one wake job and records that the wakes due by now (for
// example several missed during downtime) are covered by it.
func (s *Store) BrokerWake(ctx context.Context, o BrokerObligation, dueCount int, now time.Time) error {
	return s.brokerAct(ctx, o, func(tx *sql.Tx, _ api.Task) error {
		if err := insertWakeJob(ctx, tx, o.TaskID, o.ID, o.AgentID, now, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE obligations SET wakes=? WHERE id=? AND state<>?`, dueCount, o.ID, api.ObligationClosed)
		return err
	})
}

// BrokerCloseRecipientGone closes an obligation whose recipient was closed or exited.
func (s *Store) BrokerCloseRecipientGone(ctx context.Context, o BrokerObligation, now time.Time) error {
	return s.brokerAct(ctx, o, func(tx *sql.Tx, _ api.Task) error {
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

// postBrokerNotice inserts a hub-authored notice, allowing only the trusted
// agent name in its subject to pass the public hash rule.
func (s *Store) postBrokerNotice(ctx context.Context, tx *sql.Tx, task api.Task, to api.Agent, agentName, subject, fallback, text string, refs map[string]string) error {
	env := &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: subject, Refs: refs, Body: api.EnvelopeBody{Text: text}}
	if to.ID != "" {
		env.To = to.Name
	}
	if err := api.NormalizeBrokerPost(&api.PostMessageRequest{Envelope: env}, agentName); err != nil {
		env.Subject = fallback
	}
	req := api.PostMessageRequest{Envelope: env, To: to.ID}
	m, err := s.insertBrokerMessage(ctx, tx, task, req, to, agentName)
	if err != nil {
		return err
	}
	return s.createObligations(ctx, tx, m, req, false) // a directed notice needs delivery only
}

func obligationRefs(o BrokerObligation) map[string]string {
	return map[string]string{"obligation": o.ID, "message": fmt.Sprint(o.MessageSeq)}
}

// BrokerNudge reminds a recipient that has gone quiet and wakes it again.
func (s *Store) BrokerNudge(ctx context.Context, o BrokerObligation, now time.Time) error {
	return s.brokerAct(ctx, o, func(tx *sql.Tx, task api.Task) error {
		to, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, o.AgentID))
		if err != nil {
			return err
		}
		text := fmt.Sprintf("Message #%d (%s) has had no recorded progress for %s. Record progress with `tt progress %d --text ...`, "+
			"or reply with `tt send --reply-to %d` (result, answer, decline, or block with what you need).", o.MessageSeq, o.Subject, api.ObligationSilenceNudge, o.MessageSeq, o.MessageSeq)
		if err := s.postBrokerNotice(ctx, tx, task, to, to.Name, "Reminder: "+to.Name+" has no progress on "+obligationNoun(o.SourceKind), "Reminder: no progress recorded on an open obligation", text, obligationRefs(o)); err != nil {
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
	// Read from the scheduler's view only, so a held pass takes no write lock.
	if level == 1 && progressPastDue(o, now) {
		return ErrBrokerProgressFresh
	}
	return s.brokerAct(ctx, o, func(tx *sql.Tx, task api.Task) error {
		var lead api.Agent
		var linkedItem string
		if err := tx.QueryRowContext(ctx, `SELECT item_id FROM message_work_item_links WHERE message_task_id=? AND message_seq=? AND item_task_id=? AND relationship='primary' ORDER BY item_id LIMIT 1`, o.TaskID, o.MessageSeq, o.TaskID).Scan(&linkedItem); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if linkedItem == "" {
			if err := tx.QueryRowContext(ctx, `SELECT item_id FROM agent_work_item_bindings WHERE agent_id=? AND item_task_id=? ORDER BY created_at DESC LIMIT 1`, o.AgentID, o.TaskID).Scan(&linkedItem); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		var scopedRows int
		if linkedItem != "" {
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM item_team_leads WHERE task_id=? AND item_id=?`, o.TaskID, linkedItem).Scan(&scopedRows); err != nil {
				return err
			}
		}
		parallel, err := projectQueueParallel(ctx, tx, o.TaskID)
		if err != nil {
			return err
		}
		if level == 1 && linkedItem != "" && scopedRows != 0 {
			l, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND status NOT IN ('closed','exited','retired') AND EXISTS (SELECT 1 FROM item_team_leads l WHERE l.task_id=? AND l.item_id=? AND l.state='running' AND l.agent_id=agents.id AND l.run_id=agents.run_id)`, o.TaskID, o.TaskID, linkedItem))
			if err == nil && l.ID != o.AgentID {
				lead = l
			} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		} else if level == 1 && task.Orchestrator != "" && (linkedItem == "" || !parallel) {
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
		if level == 2 {
			held, err := blockedOnQueuedFix(ctx, tx, o)
			if err != nil {
				return err
			}
			if held {
				return ErrBrokerQueuedFix
			}
			if held, err = inMatrixWait(ctx, tx, o, now); err != nil {
				return err
			}
			if held {
				return ErrBrokerMatrixWait
			}
			if held, err = blockedOnReleaseHold(ctx, tx, o); err != nil {
				return err
			}
			if held {
				return ErrBrokerPlannedHold
			}
		}
		who := o.AgentName
		if who == "" {
			who = "an agent"
		}
		text := fmt.Sprintf("Message #%d (%s) to %s is overdue: %s. Sent %s; state %s; %s.%s "+
			"Options: nudge the recipient, reassign the obligation (tt obligations --overdue lists it), or answer for them.",
			o.MessageSeq, o.Subject, who, reason, o.CreatedAt.UTC().Format(time.RFC3339), o.State, progressAge(o.Obligation, now), unboundNote(o, who))
		subject := fmt.Sprintf("Overdue: %s on %s", who, obligationNoun(o.SourceKind))
		if level == 2 {
			subject = fmt.Sprintf("Owner attention: %s is overdue on %s", who, obligationNoun(o.SourceKind))
		}
		refs := obligationRefs(o)
		refs["escalation"] = "lead"
		if level == 2 {
			refs["escalation"] = "owner"
		}
		if err := s.postBrokerNotice(ctx, tx, task, lead, who, subject, "Overdue obligation needs attention", text, refs); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE obligations SET escalation=?,escalated_at=? WHERE id=?`, level, ts(now), o.ID)
		return err
	})
}

// unboundNote is the sentence an overdue notice adds when the holder's
// current run is not bound to the relay (wi_d1e42f0739e47139 b2); a bound
// holder's notice is unchanged.
func unboundNote(o BrokerObligation, who string) string {
	if o.RelayBound {
		return ""
	}
	return fmt.Sprintf(" %s is not bound to the relay: its current run has sent no activity report, so a directed message does not wake it.", who)
}

// progressAge says how long ago the obligation last recorded progress
// (acknowledgement, tt progress, a progress message or an owner extension).
func progressAge(o api.Obligation, now time.Time) string {
	if o.LastProgressAt == nil || o.LastProgressAt.IsZero() {
		return "no progress recorded"
	}
	return fmt.Sprintf("last progress %dm ago", int(max(0, now.Sub(*o.LastProgressAt)).Round(time.Minute)/time.Minute))
}

// latestTeamProgress is the latest progress in the project since the given
// time by an agent that is not closed or exited: recorded progress on any
// obligation it holds, or a result, answer or decline that closed one.
func (s *Store) latestTeamProgress(ctx context.Context, taskID string, since time.Time) (time.Time, error) {
	// Stored times trim trailing zeros, so they compare as strings only to
	// the second; the exact latest is taken after parsing.
	bound := ts(since.Truncate(time.Second))[:19]
	rows, err := s.db.QueryContext(ctx, `SELECT o.last_progress_at FROM obligations o JOIN agents a ON a.id=o.agent_id
 WHERE o.task_id=? AND o.last_progress_at<>'' AND substr(o.last_progress_at,1,19)>=? AND a.status NOT IN (?,?)
UNION ALL
SELECT o.closed_at FROM obligations o JOIN agents a ON a.id=o.agent_id
 WHERE o.task_id=? AND o.outcome IN (?,?,?) AND o.closed_at<>'' AND substr(o.closed_at,1,19)>=? AND a.status NOT IN (?,?)`,
		taskID, bound, api.AgentClosed, api.AgentExited,
		taskID, api.OutcomeResult, api.OutcomeAnswered, api.OutcomeDeclined, bound, api.AgentClosed, api.AgentExited)
	if err != nil {
		return time.Time{}, err
	}
	defer rows.Close()
	var latest time.Time
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return time.Time{}, err
		}
		if at := parseTS(raw); !at.Before(since) && at.After(latest) {
			latest = at
		}
	}
	return latest, rows.Err()
}

// maxStallLines keeps the stall notice under the envelope body limit.
const (
	maxStallLines     = 10
	stallListingBytes = 2500
)

// BrokerProjectStall raises one board notice per stall. lastChange is the
// latest recipient-driven change (never the broker's own escalations), so
// only real activity re-arms it; it also counts recent progress by any
// running agent in the project, including results that closed their
// obligations. quietFrom is when the quiet period starts, and moves with
// that progress. A pass with nothing to record takes no write lock and
// wakes no waiters.
func (s *Store) BrokerProjectStall(ctx context.Context, taskID string, lastChange, quietFrom time.Time, overdue []BrokerObligation, now time.Time) (bool, error) {
	// An obligation blocked on a held release job is a planned wait, even
	// one that reached the owner before its hold began: it is not listed,
	// and alone raises no stall.
	var listed []BrokerObligation
	for _, o := range overdue {
		held, err := blockedOnReleaseHold(ctx, s.db, o)
		if err != nil {
			return false, err
		}
		if !held {
			listed = append(listed, o)
		}
	}
	overdue = listed
	if len(overdue) > 0 {
		latest, err := s.latestTeamProgress(ctx, taskID, now.Add(-api.ObligationProjectStallQuiet))
		if err != nil {
			return false, err
		}
		if latest.After(lastChange) {
			lastChange = latest
		}
		if latest.After(quietFrom) {
			quietFrom = latest
		}
	}
	var observed, notifiedAt string
	err := s.db.QueryRowContext(ctx, `SELECT observed_change,notified_at FROM project_stalls WHERE task_id=?`, taskID).Scan(&observed, &notifiedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	// Re-arm only when activity moves forward, never when it appears to move back.
	rearm := errors.Is(err, sql.ErrNoRows) || lastChange.After(parseTS(observed))
	due := len(overdue) > 0 && now.Sub(quietFrom) >= api.ObligationProjectStallQuiet && (rearm || notifiedAt == "")
	if !rearm && !due {
		return false, nil
	}
	notified := false
	err = s.brokerTx(ctx, taskID, func(tx *sql.Tx, task api.Task) error {
		if rearm {
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_stalls (task_id,observed_change,notified_at) VALUES (?,?,'') ON CONFLICT(task_id) DO UPDATE SET observed_change=excluded.observed_change,notified_at=''`, taskID, ts(lastChange)); err != nil {
				return err
			}
		}
		if !due || task.PauseState != api.ProjectPauseActive {
			return nil
		}
		// Bound the listing by bytes so the body always fits its limit.
		var lines []string
		size := 0
		for i, o := range overdue {
			unbound := ""
			if !o.RelayBound {
				unbound = "; not bound to the relay"
			}
			line := fmt.Sprintf("#%d %s → %s (%s; %s%s)", o.MessageSeq, truncateRunes(o.Subject, 80), o.AgentName, ObligationOverdue(o.Obligation, now), progressAge(o.Obligation, now), unbound)
			if i == maxStallLines || size+len(line) > stallListingBytes {
				lines = append(lines, fmt.Sprintf("and %d more", len(overdue)-i))
				break
			}
			lines = append(lines, line)
			size += len(line) + 2
		}
		text := fmt.Sprintf("Overdue work has already gone to the owner and nothing has changed for %s. %d obligation(s) are overdue: %s. Run `tt obligations --overdue` for the full list.",
			api.ObligationProjectStallQuiet, len(overdue), strings.Join(lines, "; "))
		if err := s.postBrokerNotice(ctx, tx, task, api.Agent{}, "", "Project stalled: overdue work and no progress", "Project stalled: overdue work and no progress", text, map[string]string{"overdue": fmt.Sprint(len(overdue)), "escalation": "stall"}); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE project_stalls SET notified_at=? WHERE task_id=?`, ts(now), taskID)
		notified = err == nil
		return err
	})
	return notified, err
}
