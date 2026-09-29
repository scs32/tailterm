package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"time"
)

func migrateOwnerObligations(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('obligations') WHERE name='recipient_kind'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE obligations ADD COLUMN recipient_kind TEXT NOT NULL DEFAULT 'agent'`); err != nil {
			return err
		}
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS owner_obligation_delegations (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL, obligation_id TEXT NOT NULL REFERENCES obligations(id),
 session TEXT NOT NULL, agent_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '',
 authorization_ref TEXT NOT NULL, by_node TEXT NOT NULL, by_user TEXT NOT NULL, created_at TEXT NOT NULL,
 UNIQUE(obligation_id,session));`)
	return err
}

func isOwnerRequest(req api.PostMessageRequest) bool {
	return req.AgentID != "" && req.Envelope != nil && req.Envelope.Kind == api.EnvelopeKindRequest && (req.To == "owner" || req.Envelope.To == "owner" || (req.To == "" && req.Envelope.To == ""))
}

func (s *Store) createOwnerObligation(ctx context.Context, tx *sql.Tx, m api.Message, req api.PostMessageRequest) error {
	// The inserted message carries verified explicit links or the exact run's binding.
	linked := false
	for _, l := range m.WorkItems {
		if l.ItemTaskID == m.TaskID && l.Relationship == "primary" {
			linked = true
		}
	}
	if !linked {
		return fmt.Errorf("%w: owner requests require an exact primary work item", api.ErrInvalid)
	}
	if err := s.requireCurrentRun(ctx, tx, m.TaskID, req.AgentID, req.RunID); err != nil {
		return err
	}
	due := m.CreatedAt.Add(api.ObligationOwnerDefaultDue)
	if req.Envelope.Due != "" {
		d, err := time.ParseDuration(req.Envelope.Due)
		if err != nil {
			return err
		}
		due = m.CreatedAt.Add(d)
	}
	id := newObligationID("obl")
	if _, err := tx.ExecContext(ctx, `INSERT INTO obligations (id,task_id,message_seq,agent_id,subject,source_kind,needs,state,created_at,ack_due_at,due_at,changed_at,recipient_kind) VALUES (?,?,?,'',?,?,?,?,?,?,?,?,?)`, id, m.TaskID, m.Seq, req.Envelope.Subject, api.EnvelopeKindRequest, api.ObligationNeedsAnswer, api.ObligationDelivered, ts(m.CreatedAt), ts(due), ts(due), ts(m.CreatedAt), api.ObligationRecipientOwner); err != nil {
		return err
	}
	// An open delegation window takes covered requests in the same transaction.
	return s.routeIfDelegated(ctx, tx, m.TaskID, routable{kind: api.DelegationRouteObligation, seq: m.Seq, obligationID: id,
		subject: req.Envelope.Subject, category: api.OwnerRequestCategory(m.Envelope), author: req.AgentID, source: m})
}

// Owner requests never enter agent wake, acknowledgement or project-stall scheduling.
func (s *Store) BrokerEscalateOwnerRequest(ctx context.Context, o BrokerObligation, now time.Time) error {
	return s.brokerAct(ctx, o, func(tx *sql.Tx, task api.Task) error {
		if o.RecipientKind != api.ObligationRecipientOwner || o.Escalation != 0 || now.Before(o.DueAt) {
			return ErrBrokerStale
		}
		source, err := originalMessage(ctx, tx, o.TaskID, o.MessageSeq)
		if err != nil {
			return err
		}
		env := &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Owner request is past its due time", Refs: map[string]string{"obligation": o.ID, "message": fmt.Sprint(o.MessageSeq), "escalation": "owner", "recipientKind": "owner"}, Body: api.EnvelopeBody{Text: fmt.Sprintf("Waiting on owner for message #%d (%s), sent %s. Answer the linked request in TailOS or Discord.", o.MessageSeq, o.Subject, o.CreatedAt.UTC().Format(time.RFC3339))}}
		req := api.PostMessageRequest{Envelope: env, WorkItems: source.WorkItems, WorkOrderMessage: source.WorkOrderMessage, RequestID: "owner-overdue-" + o.ID}
		if _, err := s.insertBrokerMessage(ctx, tx, task, req, api.Agent{}, ""); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE obligations SET escalation=2,escalated_at=? WHERE id=?`, ts(now), o.ID)
		return err
	})
}

func (s *Store) applyOwnerReply(ctx context.Context, tx *sql.Tx, m *api.Message, req api.PostMessageRequest, by api.Caller) error {
	if req.ReplyTo <= 0 || req.AgentID != "" || by.Node == api.BrokerNode {
		return nil
	}
	if req.Envelope != nil && req.Envelope.Kind != api.EnvelopeKindAnswer {
		return nil
	}
	o, err := scanObligation(tx.QueryRowContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE task_id=? AND message_seq=? AND recipient_kind='owner' AND state<>?`, m.TaskID, req.ReplyTo, api.ObligationClosed))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE obligations SET state=?,outcome=?,outcome_seq=?,reason=?,closed_at=?,changed_at=? WHERE id=?`, api.ObligationClosed, api.OutcomeAnswered, m.Seq, "answered by the owner", ts(m.CreatedAt), ts(m.CreatedAt), o.ID)
	return err
}

func (s *Store) DelegateOwnerObligation(ctx context.Context, taskID, id string, req api.OwnerDelegationRequest, by api.Caller) (api.OwnerActionResult, error) {
	if !api.ValidText(req.Session, 200) || !api.ValidText(req.AuthorizationRef, 500) || req.Session == "" || req.AuthorizationRef == "" || (req.AgentID == "") != (req.RunID == "") {
		return api.OwnerActionResult{}, api.ErrInvalid
	}
	return s.ownerAction(ctx, taskID, "delegate", id, req.RequestID, req, by, func(tx *sql.Tx, task api.Task, now time.Time) (api.OwnerActionResult, error) {
		o, err := openObligation(ctx, tx, taskID, id)
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		if o.RecipientKind != api.ObligationRecipientOwner {
			return api.OwnerActionResult{}, api.ErrInvalid
		}
		if req.AgentID != "" {
			if err := s.requireCurrentRun(ctx, tx, taskID, req.AgentID, req.RunID); err != nil {
				return api.OwnerActionResult{}, err
			}
		}
		grant := newObligationID("odg")
		_, err = tx.ExecContext(ctx, `INSERT INTO owner_obligation_delegations (id,task_id,obligation_id,session,agent_id,run_id,authorization_ref,by_node,by_user,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, grant, taskID, id, req.Session, req.AgentID, req.RunID, req.AuthorizationRef, by.Node, by.User, ts(now))
		return api.OwnerActionResult{Obligation: &o, DelegationID: grant}, err
	})
}

func (s *Store) verifyOwnerDelegate(ctx context.Context, tx *sql.Tx, taskID, id string, req api.ObligationAnswerRequest) (string, error) {
	if req.DelegateSession == "" {
		if req.AgentID != "" || req.RunID != "" {
			return "", api.ErrInvalid
		}
		return "", nil
	}
	var grant, agent, run string
	err := tx.QueryRowContext(ctx, `SELECT id,agent_id,run_id FROM owner_obligation_delegations WHERE task_id=? AND obligation_id=? AND session=?`, taskID, id, req.DelegateSession).Scan(&grant, &agent, &run)
	if err != nil {
		return "", fmt.Errorf("%w: delegated session has no owner grant", api.ErrConflict)
	}
	if req.AgentID != agent || req.RunID != run {
		return "", fmt.Errorf("%w: delegate identity differs from grant", api.ErrConflict)
	}
	if agent != "" {
		if err := s.requireCurrentRun(ctx, tx, taskID, agent, run); err != nil {
			return "", err
		}
	}
	return grant, nil
}
