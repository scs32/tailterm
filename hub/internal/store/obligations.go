package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Broker phase 2a (docs/broker-phase-2a.md). Obligations are created in the
// message's own transaction, closed only by typed outcomes from the recipient,
// and never by reads, heartbeats, wake acceptance or turn ends.
const obligationsSchema = `
CREATE TABLE IF NOT EXISTS obligations (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  message_seq INTEGER NOT NULL REFERENCES messages(seq),
  agent_id TEXT NOT NULL,
  subject TEXT NOT NULL DEFAULT '',
  source_kind TEXT NOT NULL,
  needs TEXT NOT NULL,
  state TEXT NOT NULL,
  outcome TEXT NOT NULL DEFAULT '',
  outcome_seq INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  ack_due_at TEXT NOT NULL,
  due_at TEXT NOT NULL,
  delivered_at TEXT NOT NULL DEFAULT '',
  acked_at TEXT NOT NULL DEFAULT '',
  last_progress_at TEXT NOT NULL DEFAULT '',
  closed_at TEXT NOT NULL DEFAULT '',
  escalation INTEGER NOT NULL DEFAULT 0,
  escalated_at TEXT NOT NULL DEFAULT '',
  nudges INTEGER NOT NULL DEFAULT 0,
  nudged_at TEXT NOT NULL DEFAULT '',
  wakes INTEGER NOT NULL DEFAULT 0,
  changed_at TEXT NOT NULL,
  UNIQUE(message_seq, agent_id)
);
CREATE INDEX IF NOT EXISTS obligations_task_state ON obligations(task_id, state);
CREATE INDEX IF NOT EXISTS obligations_agent_state ON obligations(agent_id, state);
CREATE TABLE IF NOT EXISTS wake_jobs (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  obligation_id TEXT NOT NULL REFERENCES obligations(id),
  agent_id TEXT NOT NULL,
  due_at TEXT NOT NULL,
  state TEXT NOT NULL,
  lease_token TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  reported_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS wake_jobs_agent ON wake_jobs(agent_id, state, due_at);
CREATE TABLE IF NOT EXISTS project_stalls (
  task_id TEXT PRIMARY KEY,
  observed_change TEXT NOT NULL,
  notified_at TEXT NOT NULL DEFAULT ''
);`

const (
	wakePending   = "pending"
	wakeLeased    = "leased"
	wakeAccepted  = "accepted"
	wakeFailed    = "failed"
	wakeAmbiguous = "ambiguous"
	wakeLease     = 2 * time.Minute
)

// BrokerCaller authors hub-generated escalations and reassignments.
var BrokerCaller = api.Caller{Node: api.BrokerNode, User: "broker"}

func newObligationID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// obligationNeeds maps a message to what it obliges its recipient to do.
func obligationNeeds(req api.PostMessageRequest, by api.Caller) string {
	if e := req.Envelope; e != nil {
		switch e.Kind {
		case api.EnvelopeKindAssign, api.EnvelopeKindRequest, api.EnvelopeKindReview, api.EnvelopeKindBlock:
			return api.ObligationNeedsOutcome
		case api.EnvelopeKindQuestion:
			return api.ObligationNeedsAnswer
		case api.EnvelopeKindNotice, api.EnvelopeKindFinding:
			return api.ObligationNeedsDelivery
		}
		return ""
	}
	if req.AgentID == "" && by.Node != api.BrokerNode {
		return api.ObligationNeedsOutcome // a directed human message
	}
	return ""
}

func obligationSubject(req api.PostMessageRequest) (kind, subject string) {
	if req.Envelope != nil {
		return req.Envelope.Kind, req.Envelope.Subject
	}
	text := strings.Join(strings.Fields(req.Text), " ")
	if len(text) > 100 {
		text = text[:100] + "…"
	}
	return "human", text
}

// createObligations runs inside the message transaction.
func (s *Store) createObligations(ctx context.Context, tx *sql.Tx, m api.Message, req api.PostMessageRequest, by api.Caller) error {
	if req.To == "" || req.To == req.AgentID {
		return nil // board-wide posts and self-addressed posts oblige no one
	}
	needs := obligationNeeds(req, by)
	if needs == "" {
		return nil
	}
	kind, subject := obligationSubject(req)
	created := m.CreatedAt
	ackDue := created.Add(api.ObligationAckDeadline)
	due := created.Add(api.ObligationDefaultDue)
	switch needs {
	case api.ObligationNeedsAnswer:
		due = created.Add(api.ObligationQuestionDue)
	case api.ObligationNeedsDelivery:
		due = ackDue
	}
	if req.Envelope != nil && req.Envelope.Due != "" {
		if d, err := time.ParseDuration(req.Envelope.Due); err == nil && d > 0 {
			due = created.Add(d)
		}
	}
	id := newObligationID("obl")
	if _, err := tx.ExecContext(ctx, `INSERT INTO obligations (id,task_id,message_seq,agent_id,subject,source_kind,needs,state,created_at,ack_due_at,due_at,changed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, m.TaskID, m.Seq, req.To, subject, kind, needs, api.ObligationQueued, ts(created), ts(ackDue), ts(due), ts(created)); err != nil {
		return err
	}
	return insertWakeJob(ctx, tx, m.TaskID, id, req.To, created, created)
}

func insertWakeJob(ctx context.Context, tx *sql.Tx, taskID, obligationID, agentID string, due, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO wake_jobs (id,task_id,obligation_id,agent_id,due_at,state,created_at) VALUES (?,?,?,?,?,?,?)`,
		newObligationID("wake"), taskID, obligationID, agentID, ts(due), wakePending, ts(now))
	return err
}

// applyReplyOutcome lets a typed reply from the recipient close or pause the
// obligation its reply-to message created. It runs in the reply's transaction.
func (s *Store) applyReplyOutcome(ctx context.Context, tx *sql.Tx, m api.Message, req api.PostMessageRequest) error {
	if req.ReplyTo <= 0 || req.AgentID == "" || req.Envelope == nil {
		return nil
	}
	now := ts(m.CreatedAt)
	switch req.Envelope.Kind {
	case api.EnvelopeKindResult, api.EnvelopeKindAnswer, api.EnvelopeKindDecline:
		outcome := map[string]string{api.EnvelopeKindResult: api.OutcomeResult, api.EnvelopeKindAnswer: api.OutcomeAnswered, api.EnvelopeKindDecline: api.OutcomeDeclined}[req.Envelope.Kind]
		_, err := tx.ExecContext(ctx, `UPDATE obligations SET state=?,outcome=?,outcome_seq=?,reason=?,closed_at=?,changed_at=? WHERE message_seq=? AND agent_id=? AND state<>?`,
			api.ObligationClosed, outcome, m.Seq, req.Envelope.Body.Reason, now, now, req.ReplyTo, req.AgentID, api.ObligationClosed)
		return err
	case api.EnvelopeKindBlock:
		_, err := tx.ExecContext(ctx, `UPDATE obligations SET state=?,reason=?,changed_at=? WHERE message_seq=? AND agent_id=? AND state<>?`,
			api.ObligationBlocked, req.Envelope.Body.Reason, now, req.ReplyTo, req.AgentID, api.ObligationClosed)
		return err
	}
	return nil
}

const obligationCols = `id,task_id,message_seq,agent_id,subject,source_kind,needs,state,outcome,outcome_seq,reason,created_at,ack_due_at,due_at,delivered_at,acked_at,last_progress_at,closed_at,escalation,nudges`

func scanObligation(row rowScanner) (api.Obligation, error) {
	var o api.Obligation
	var created, ackDue, due, delivered, acked, progress, closed string
	err := row.Scan(&o.ID, &o.TaskID, &o.MessageSeq, &o.AgentID, &o.Subject, &o.SourceKind, &o.Needs, &o.State, &o.Outcome, &o.OutcomeSeq, &o.Reason,
		&created, &ackDue, &due, &delivered, &acked, &progress, &closed, &o.Escalation, &o.Nudges)
	if err != nil {
		return o, err
	}
	o.CreatedAt, o.AckDueAt, o.DueAt = parseTS(created), parseTS(ackDue), parseTS(due)
	for _, f := range []struct {
		raw string
		dst **time.Time
	}{{delivered, &o.DeliveredAt}, {acked, &o.AckedAt}, {progress, &o.LastProgressAt}, {closed, &o.ClosedAt}} {
		if f.raw != "" {
			t := parseTS(f.raw)
			*f.dst = &t
		}
	}
	return o, nil
}

// ObligationOverdue says which deadline an open obligation has missed, if any.
func ObligationOverdue(o api.Obligation, now time.Time) string {
	if o.State == api.ObligationClosed || o.Needs == api.ObligationNeedsDelivery {
		return ""
	}
	switch o.State {
	case api.ObligationQueued, api.ObligationDelivered:
		if now.After(o.AckDueAt) {
			return "ack"
		}
	case api.ObligationAcknowledged, api.ObligationWorking:
		last := o.CreatedAt
		if o.AckedAt != nil {
			last = *o.AckedAt
		}
		if o.LastProgressAt != nil && o.LastProgressAt.After(last) {
			last = *o.LastProgressAt
		}
		if now.Sub(last) > api.ObligationSilenceNudge {
			return "silence"
		}
	}
	if now.After(o.DueAt) {
		return "outcome"
	}
	return ""
}

// ObligationFilter selects obligations for listing.
type ObligationFilter struct {
	AgentID  string
	OpenOnly bool
	Overdue  bool
}

func (s *Store) ListObligations(ctx context.Context, taskID string, f ObligationFilter, now time.Time) ([]api.Obligation, error) {
	q := `SELECT ` + obligationCols + ` FROM obligations WHERE task_id=?`
	args := []any{taskID}
	if f.AgentID != "" {
		q += ` AND agent_id=?`
		args = append(args, f.AgentID)
	}
	if f.OpenOnly || f.Overdue {
		q += ` AND state<>?`
		args = append(args, api.ObligationClosed)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY message_seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Obligation{}
	for rows.Next() {
		o, err := scanObligation(rows)
		if err != nil {
			return nil, err
		}
		o.Overdue = ObligationOverdue(o, now)
		if f.Overdue && o.Overdue == "" {
			continue
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MarkObligationsDelivered records that the recipient's current run fetched
// its obligations. Delivery is not acknowledgement; delivery-only obligations
// close here.
func (s *Store) MarkObligationsDelivered(ctx context.Context, taskID, agentID, runID string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.requireCurrentRun(ctx, s.db, taskID, agentID, runID); err != nil {
		return err
	}
	return s.markDelivered(ctx, s.db, `task_id=? AND agent_id=?`, []any{taskID, agentID}, now)
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) markDelivered(ctx context.Context, db execer, where string, args []any, now time.Time) error {
	t := ts(now)
	if _, err := db.ExecContext(ctx, `UPDATE obligations SET state=?,outcome=?,delivered_at=?,closed_at=?,changed_at=? WHERE `+where+` AND state=? AND needs=?`,
		append([]any{api.ObligationClosed, api.OutcomeDelivered, t, t, t}, append(args, api.ObligationQueued, api.ObligationNeedsDelivery)...)...); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE obligations SET state=?,delivered_at=?,changed_at=? WHERE `+where+` AND state=?`,
		append([]any{api.ObligationDelivered, t, t}, append(args, api.ObligationQueued)...)...)
	return err
}

func (s *Store) requireCurrentRun(ctx context.Context, db execer, taskID, agentID, runID string) error {
	var current, status string
	err := db.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE id=? AND task_id=?`, agentID, taskID).Scan(&current, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ErrNotFound
	}
	if err != nil {
		return err
	}
	if runID == "" || current != runID || status == api.AgentClosed {
		return fmt.Errorf("%w: only the recipient's current run may act on its obligations", api.ErrConflict)
	}
	return nil
}

// ObligationAction is ack, progress or resume by the recipient's current run.
func (s *Store) ObligationAction(ctx context.Context, taskID string, messageSeq int64, action string, req api.ObligationActionRequest, now time.Time) (api.Obligation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Obligation{}, err
	}
	defer tx.Rollback()
	if err := s.requireCurrentRun(ctx, tx, taskID, req.AgentID, req.RunID); err != nil {
		return api.Obligation{}, err
	}
	o, err := scanObligation(tx.QueryRowContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE task_id=? AND message_seq=? AND agent_id=?`, taskID, messageSeq, req.AgentID))
	if errors.Is(err, sql.ErrNoRows) {
		return o, api.ErrNotFound
	}
	if err != nil {
		return o, err
	}
	if o.State == api.ObligationClosed {
		return o, fmt.Errorf("%w: obligation is already closed (%s)", api.ErrConflict, o.Outcome)
	}
	t := ts(now)
	switch action {
	case "ack":
		if o.Needs == api.ObligationNeedsDelivery {
			_, err = tx.ExecContext(ctx, `UPDATE obligations SET state=?,outcome=?,acked_at=?,closed_at=?,changed_at=? WHERE id=?`, api.ObligationClosed, api.OutcomeDelivered, t, t, t, o.ID)
		} else if o.State == api.ObligationAcknowledged || o.State == api.ObligationWorking {
			// Already acknowledged: acknowledging again changes nothing.
		} else {
			// ack also resumes a blocked obligation.
			_, err = tx.ExecContext(ctx, `UPDATE obligations SET state=?,acked_at=CASE WHEN acked_at='' THEN ? ELSE acked_at END,delivered_at=CASE WHEN delivered_at='' THEN ? ELSE delivered_at END,last_progress_at=?,reason='',changed_at=? WHERE id=?`,
				api.ObligationAcknowledged, t, t, t, t, o.ID)
		}
	case "progress":
		if o.Needs == api.ObligationNeedsDelivery || o.State == api.ObligationQueued || o.State == api.ObligationDelivered || o.State == api.ObligationBlocked {
			return o, fmt.Errorf("%w: acknowledge the obligation before recording progress", api.ErrConflict)
		}
		_, err = tx.ExecContext(ctx, `UPDATE obligations SET state=?,last_progress_at=?,nudges=0,changed_at=? WHERE id=?`, api.ObligationWorking, t, t, o.ID)
	default:
		return o, api.ErrInvalid
	}
	if err != nil {
		return o, err
	}
	if err := tx.Commit(); err != nil {
		return o, err
	}
	s.notify(taskID)
	return s.getObligation(ctx, o.ID, now)
}

func (s *Store) getObligation(ctx context.Context, id string, now time.Time) (api.Obligation, error) {
	o, err := scanObligation(s.db.QueryRowContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return o, api.ErrNotFound
	}
	o.Overdue = ObligationOverdue(o, now)
	return o, err
}

// ReassignObligation supersedes an open obligation and re-issues its message
// to another agent in one transaction; the new message creates the new
// obligation through the normal insert path.
func (s *Store) ReassignObligation(ctx context.Context, taskID, obligationID string, req api.ObligationReassignRequest, by api.Caller) (api.Message, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Message{}, err
	}
	defer tx.Rollback()
	var old api.Obligation
	old, err = scanObligation(tx.QueryRowContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE id=? AND task_id=?`, obligationID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Message{}, api.ErrNotFound
	}
	if err != nil {
		return api.Message{}, err
	}
	if old.State == api.ObligationClosed || req.ToAgentID == old.AgentID {
		return api.Message{}, fmt.Errorf("%w: only an open obligation can move to a different agent", api.ErrConflict)
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if err != nil {
		return api.Message{}, err
	}
	target, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=? AND task_id=?`, req.ToAgentID, taskID))
	if err != nil || target.Status == api.AgentClosed {
		return api.Message{}, api.ErrInvalid
	}
	original, err := loadMessage(tx, ctx, taskID, old.MessageSeq)
	if err != nil {
		return api.Message{}, err
	}
	env := reassignedEnvelope(original, target.Name, old.MessageSeq)
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE obligations SET state=?,outcome=?,reason=?,closed_at=?,changed_at=? WHERE id=?`,
		api.ObligationClosed, api.OutcomeSuperseded, req.Reason, ts(now), ts(now), old.ID); err != nil {
		return api.Message{}, err
	}
	m, err := s.insertMessageWithResume(ctx, tx, task, api.PostMessageRequest{Envelope: env, To: target.ID}, target, BrokerCaller, false, false, false)
	if err != nil {
		return m, err
	}
	if err := tx.Commit(); err != nil {
		return m, err
	}
	s.notify(taskID)
	return m, nil
}

func reassignedEnvelope(original api.Message, toName string, seq int64) *api.Envelope {
	if original.Envelope != nil {
		e := *original.Envelope
		e.To = toName
		e.Refs = map[string]string{}
		for k, v := range original.Envelope.Refs {
			e.Refs[k] = v
		}
		e.Refs["reassignedFrom"] = fmt.Sprint(seq)
		if len(e.Subject) <= 108 {
			e.Subject = "Reassigned: " + e.Subject
		}
		return &e
	}
	ask := strings.Join(strings.Fields(original.Text), " ")
	if len(ask) > 1500 {
		ask = ask[:1500] + "…"
	}
	return &api.Envelope{Kind: api.EnvelopeKindRequest, To: toName, Subject: "Reassigned request from the owner",
		Refs: map[string]string{"reassignedFrom": fmt.Sprint(seq)}, Body: api.EnvelopeBody{Ask: ask}}
}
