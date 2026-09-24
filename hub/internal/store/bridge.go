package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Broker phase 2b (docs/broker-phase-2b.md): provenance for messages bridged
// from Discord, and the owner's immediate nudge of an obligation.
const bridgeSchema = `
CREATE TABLE IF NOT EXISTS message_sources (
  message_seq INTEGER PRIMARY KEY REFERENCES messages(seq),
  kind TEXT NOT NULL,
  source_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  UNIQUE (kind, source_id)
);
CREATE TABLE IF NOT EXISTS obligation_nudges (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  obligation_id TEXT NOT NULL REFERENCES obligations(id),
  wake_job_id TEXT NOT NULL,
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  created_at TEXT NOT NULL,
  request_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS obligation_nudges_obligation ON obligation_nudges(obligation_id, created_at);`

// migrateBridge creates the phase-2b tables and adds columns a newer build
// needs to tables an older build created.
func migrateBridge(db *sql.DB) error {
	if _, err := db.Exec(bridgeSchema); err != nil {
		return err
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('obligation_nudges') WHERE name='request_id'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE obligation_nudges ADD COLUMN request_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS obligation_nudges_request ON obligation_nudges(obligation_id, request_id) WHERE request_id<>''`)
	return err
}

func insertMessageSource(ctx context.Context, tx *sql.Tx, m *api.Message, src *api.MessageSource) error {
	if src == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_sources (message_seq,kind,source_id,user_id) VALUES (?,?,?,?)`,
		m.Seq, src.Kind, src.ID, src.UserID); err != nil {
		return fmt.Errorf("%w: this Discord message was already posted", api.ErrConflict)
	}
	copied := *src
	m.Source = &copied
	return nil
}

// NudgeObligation queues an immediate wake of an open obligation's recipient,
// outside the broker's retry schedule. Only the owner may nudge, at most once
// per obligation per api.ObligationNudgeInterval. The nudge is recorded with
// who asked; it does not acknowledge or change the obligation.
// A repeated request ID returns the original nudge, so a caller that lost
// the reply (or a bridge resuming after a crash) never wakes twice.
func (s *Store) NudgeObligation(ctx context.Context, taskID, obligationID, requestID string, by api.Caller) (api.ObligationNudgeResult, error) {
	if requestID != "" && !validRequestID(requestID) {
		return api.ObligationNudgeResult{}, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ObligationNudgeResult{}, err
	}
	defer tx.Rollback()
	o, err := scanObligation(tx.QueryRowContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE id=? AND task_id=?`, obligationID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return api.ObligationNudgeResult{}, api.ErrNotFound
	}
	if err != nil {
		return api.ObligationNudgeResult{}, err
	}
	if requestID != "" {
		var jobID string
		err := tx.QueryRowContext(ctx, `SELECT wake_job_id FROM obligation_nudges WHERE obligation_id=? AND request_id=?`, o.ID, requestID).Scan(&jobID)
		if err == nil {
			o.Overdue = ObligationOverdue(o, s.now())
			return api.ObligationNudgeResult{Obligation: o, WakeJobID: jobID}, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return api.ObligationNudgeResult{}, err
		}
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if err != nil {
		return api.ObligationNudgeResult{}, err
	}
	if task.Status != api.TaskOpen {
		return api.ObligationNudgeResult{}, api.ErrClosed
	}
	if task.PauseState != api.ProjectPauseActive {
		return api.ObligationNudgeResult{}, fmt.Errorf("%w: the project is paused", api.ErrConflict)
	}
	if o.State == api.ObligationClosed {
		return api.ObligationNudgeResult{}, fmt.Errorf("%w: the obligation is already closed", api.ErrConflict)
	}
	recipient, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, o.AgentID))
	if err != nil || recipient.Status == api.AgentClosed || recipient.Status == api.AgentExited {
		return api.ObligationNudgeResult{}, fmt.Errorf("%w: the recipient is no longer running; reassign the obligation instead", api.ErrConflict)
	}
	now := s.now()
	var last string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(created_at),'') FROM obligation_nudges WHERE obligation_id=?`, o.ID).Scan(&last); err != nil {
		return api.ObligationNudgeResult{}, err
	}
	if last != "" {
		if wait := parseTS(last).Add(api.ObligationNudgeInterval).Sub(now); wait > 0 {
			return api.ObligationNudgeResult{}, &api.ErrNudgeTooSoon{RetryAfter: wait}
		}
	}
	jobID := newObligationID("wake")
	if _, err := tx.ExecContext(ctx, `INSERT INTO wake_jobs (id,task_id,obligation_id,agent_id,due_at,state,created_at) VALUES (?,?,?,?,?,?,?)`,
		jobID, taskID, o.ID, o.AgentID, ts(now), wakePending, ts(now)); err != nil {
		return api.ObligationNudgeResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO obligation_nudges (id,task_id,obligation_id,wake_job_id,by_node,by_user,created_at,request_id) VALUES (?,?,?,?,?,?,?,?)`,
		newObligationID("nudge"), taskID, o.ID, jobID, by.Node, by.User, ts(now), requestID); err != nil {
		return api.ObligationNudgeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.ObligationNudgeResult{}, err
	}
	s.notify(taskID)
	o.Overdue = ObligationOverdue(o, now)
	return api.ObligationNudgeResult{Obligation: o, WakeJobID: jobID}, nil
}
