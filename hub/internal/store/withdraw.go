package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

// WithdrawObligation closes the original sender's open request and delivers a
// notice to its recipient in the same transaction. No owner privilege is used.
func (s *Store) WithdrawObligation(ctx context.Context, taskID, obligationID string, req api.ObligationWithdrawRequest) (api.Obligation, error) {
	reason := strings.TrimSpace(req.Reason)
	if !api.ValidID(taskID, "tsk") || !api.ValidObligationID(obligationID) || !api.ValidID(req.AgentID, "agt") || !validRunID(req.RunID) || reason == "" || !api.ValidText(reason, 500) || (req.RequestID != "" && !validRequestID(req.RequestID)) {
		return api.Obligation{}, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Obligation{}, err
	}
	defer tx.Rollback()
	var current, status string
	err = tx.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE id=? AND task_id=?`, req.AgentID, taskID).Scan(&current, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Obligation{}, api.ErrNotFound
	}
	if err != nil {
		return api.Obligation{}, err
	}
	if current != req.RunID || status == api.AgentClosed {
		return api.Obligation{}, fmt.Errorf("%w: only the sender's current run may withdraw", api.ErrConflict)
	}
	o, err := scanObligation(tx.QueryRowContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE id=? AND task_id=?`, obligationID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return o, api.ErrNotFound
	}
	if err != nil {
		return o, err
	}
	source, err := originalMessage(ctx, tx, taskID, o.MessageSeq)
	if err != nil {
		return o, err
	}
	var sourceAgent, sourceRun string
	err = tx.QueryRowContext(ctx, `SELECT from_agent,from_run_id FROM messages WHERE seq=? AND task_id=?`, source.Seq, taskID).Scan(&sourceAgent, &sourceRun)
	if err != nil {
		return o, err
	}
	if sourceAgent != req.AgentID || sourceRun == "" || sourceRun != req.RunID {
		return o, fmt.Errorf("%w: only the original sending run may withdraw", api.ErrConflict)
	}
	if o.SourceKind != api.EnvelopeKindAssign && o.SourceKind != api.EnvelopeKindRequest && o.SourceKind != api.EnvelopeKindReview && o.SourceKind != api.EnvelopeKindQuestion || o.Needs == api.ObligationNeedsDelivery {
		return o, fmt.Errorf("%w: this message is not a withdrawable request", api.ErrConflict)
	}
	fingerprint := fmt.Sprintf("withdraw|%s|%s|%s", req.RunID, obligationID, reason)
	if req.RequestID != "" {
		var prior, target string
		err = tx.QueryRowContext(ctx, `SELECT fingerprint,obligation_id FROM obligation_receipts WHERE task_id=? AND agent_id=? AND request_id=?`, taskID, req.AgentID, req.RequestID).Scan(&prior, &target)
		if err == nil {
			if prior != fingerprint || target != obligationID {
				return o, fmt.Errorf("%w: request ID was already used for a different obligation action", api.ErrConflict)
			}
			if o.State != api.ObligationClosed || o.Outcome != api.OutcomeWithdrawn {
				return o, fmt.Errorf("%w: withdrawal receipt disagrees with obligation", api.ErrConflict)
			}
			return o, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return o, err
		}
	}
	if o.State == api.ObligationClosed {
		return o, fmt.Errorf("%w: obligation is already closed (%s)", api.ErrConflict, o.Outcome)
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if err != nil {
		return o, err
	}
	to, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=? AND task_id=?`, o.AgentID, taskID))
	if err != nil {
		return o, err
	}
	now := ts(s.now())
	if _, err = tx.ExecContext(ctx, `UPDATE obligations SET state=?,outcome=?,reason=?,closed_at=?,changed_at=? WHERE id=? AND state<>?`, api.ObligationClosed, api.OutcomeWithdrawn, reason, now, now, o.ID, api.ObligationClosed); err != nil {
		return o, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE wake_jobs SET state=?,detail=?,lease_token='',lease_expires_at='',reported_at=? WHERE obligation_id=? AND state IN (?,?)`, wakeFailed, "source obligation withdrawn", now, o.ID, wakePending, wakeLeased); err != nil {
		return o, err
	}
	text := fmt.Sprintf("The sender withdrew message #%d (%s). Stop work on it. Reason: %s", o.MessageSeq, o.Subject, reason)
	if err = s.postLinkedBrokerNotice(ctx, tx, task, to, "The sender withdrew a request", text, map[string]string{"obligation": o.ID, "message": fmt.Sprint(o.MessageSeq)}, source, "sender-withdraw-"+o.ID); err != nil {
		return o, err
	}
	if req.RequestID != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO obligation_receipts (task_id,agent_id,request_id,fingerprint,obligation_id,created_at) VALUES (?,?,?,?,?,?)`, taskID, req.AgentID, req.RequestID, fingerprint, o.ID, now); err != nil {
			return o, err
		}
	}
	if err = tx.Commit(); err != nil {
		return o, err
	}
	s.notify(taskID)
	return s.getObligation(ctx, o.ID, s.now())
}
