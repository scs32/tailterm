package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Broker phase 3 (docs/broker-phase-3.md): obligations become the only
// follow-through mechanism. This file holds the legacy close-out, role
// recipients, the lead-change handoff of role obligations, and the owner's
// extend, answer, cancel and resume actions.

const phase3Schema = `
CREATE TABLE IF NOT EXISTS owner_actions (
  task_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  action TEXT NOT NULL,
  target_id TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  result TEXT NOT NULL,
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (task_id, request_id)
);
CREATE TABLE IF NOT EXISTS broker_migrations (
  name TEXT PRIMARY KEY,
  applied_at TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT ''
);`

func migratePhase3(db *sql.DB) error {
	if _, err := db.Exec(phase3Schema); err != nil {
		return err
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('obligations') WHERE name='via_role'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE obligations ADD COLUMN via_role TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

// ---- Legacy close-out ----

// RetiredPhase3 marks deliveries and lead-disposition rows closed by the
// phase-3 retirement of the legacy follow-through path.
const RetiredPhase3 = "retired-phase3"

// retireLegacyFollowThrough closes, once, every current delivery that never
// reached a result, and any pending lead-disposition rows an older build
// left behind. Each project gets one board notice naming the work items
// those deliveries referenced, so nothing is silently dropped. It is
// idempotent: a recorded migration is never run again.
func (s *Store) retireLegacyFollowThrough(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM broker_migrations WHERE name=?`, RetiredPhase3).Scan(&done); err != nil || done > 0 {
		return err
	}
	now := s.now()
	rows, err := tx.QueryContext(ctx, `SELECT task_id,id FROM required_deliveries WHERE current=1 AND phase NOT IN (?,?) ORDER BY task_id,created_at`, api.DeliveryResult, api.DeliverySuperseded)
	if err != nil {
		return err
	}
	var open []ref
	for rows.Next() {
		var r ref
		if err := rows.Scan(&r.task, &r.id); err != nil {
			rows.Close()
			return err
		}
		open = append(open, r)
	}
	rows.Close()
	items := map[string]map[string]bool{}
	for _, r := range open {
		d, err := getRequiredDeliveryRow(tx, ctx, r.task, r.id)
		if err != nil {
			return err
		}
		recipient := "unknown"
		_ = tx.QueryRowContext(ctx, `SELECT status FROM agents WHERE id=?`, d.AgentID).Scan(&recipient)
		if _, err := tx.ExecContext(ctx, `UPDATE required_deliveries SET phase=?,current=0,updated_at=? WHERE id=?`, api.DeliverySuperseded, ts(now), d.ID); err != nil {
			return err
		}
		if _, err := insertDeliveryEvent(ctx, tx, d, RetiredPhase3, "retire-phase3-"+d.ID, "", "", map[string]any{
			"previousPhase": d.Phase, "recipientStatus": recipient,
			"reason": "broker phase 3 retired legacy follow-through; obligations are the only follow-through mechanism",
		}, BrokerCaller, now); err != nil {
			return err
		}
		if d.ItemID != "" {
			if items[d.TaskID] == nil {
				items[d.TaskID] = map[string]bool{}
			}
			items[d.TaskID][d.ItemID] = true
		}
	}
	// Lead-disposition rows exist only in databases an older side branch touched.
	var ldo int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='lead_disposition_obligations'`).Scan(&ldo); err != nil {
		return err
	}
	// Lead-disposition rows: each is listed by ID in the migration record,
	// and its project is told, even if it had no open delivery.
	ldoByTask := map[string][]string{}
	if ldo > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT id,task_id,item_id FROM lead_disposition_obligations WHERE state NOT IN ('closed','completed',?) ORDER BY id`, RetiredPhase3)
		if err != nil {
			return err
		}
		type row struct{ id, task, item string }
		var pending []row
		for rows.Next() {
			var r row
			var item sql.NullString
			if err := rows.Scan(&r.id, &r.task, &item); err != nil {
				rows.Close()
				return err
			}
			r.item = item.String
			pending = append(pending, r)
		}
		rows.Close()
		for _, r := range pending {
			if _, err := tx.ExecContext(ctx, `UPDATE lead_disposition_obligations SET state=?,updated_at=? WHERE id=?`, RetiredPhase3, ts(now), r.id); err != nil {
				return err
			}
			ldoByTask[r.task] = append(ldoByTask[r.task], r.id)
			if r.item != "" {
				if items[r.task] == nil {
					items[r.task] = map[string]bool{}
				}
				items[r.task][r.item] = true
			}
		}
	}
	tasks := make([]string, 0, len(items))
	for t := range items {
		tasks = append(tasks, t)
	}
	for t := range ldoByTask {
		if items[t] == nil {
			tasks = append(tasks, t)
		}
	}
	sort.Strings(tasks)
	detailItems := map[string][]string{}
	for _, taskID := range tasks {
		task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
		if errors.Is(err, sql.ErrNoRows) {
			continue // a row pointing at a missing project: recorded in detail only
		}
		if err != nil {
			return err
		}
		var ids []string
		for id := range items[taskID] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		detailItems[taskID] = ids
		// Bounded, so a project with many items can never exceed the envelope
		// body limit and stop the hub from starting; the full list is kept in
		// the broker_migrations record.
		shown := ids
		more := ""
		if len(shown) > maxRetiredItemsListed {
			shown, more = shown[:maxRetiredItemsListed], fmt.Sprintf(" and %d more (the full list is in the hub's broker_migrations record)", len(ids)-maxRetiredItemsListed)
		}
		text := fmt.Sprintf("Broker phase 3 retired the legacy directive follow-through in this project: %d open directive(s) and %d pending lead disposition(s) were closed with provenance.", countDeliveries(open, taskID), len(ldoByTask[taskID]))
		if len(shown) > 0 {
			text += fmt.Sprintf(" Work items they referenced, to re-dispatch when this project resumes: %s%s.", strings.Join(shown, ", "), more)
		}
		if err := s.postBrokerNotice(ctx, tx, task, api.Agent{}, "Legacy directives were retired for this project", "Legacy directives were retired for this project", text, map[string]string{"migration": RetiredPhase3}); err != nil {
			return err
		}
	}
	detail, _ := json.Marshal(map[string]any{"deliveries": deliveryIDs(open), "leadDispositions": ldoByTask, "items": detailItems})
	if _, err := tx.ExecContext(ctx, `INSERT INTO broker_migrations (name,applied_at,detail) VALUES (?,?,?)`, RetiredPhase3, ts(now), string(detail)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, t := range tasks {
		s.notify(t)
	}
	return nil
}

// maxRetiredItemsListed bounds the work items named in one close-out notice.
const maxRetiredItemsListed = 40

type ref struct{ task, id string }

func countDeliveries(open []ref, taskID string) int {
	n := 0
	for _, r := range open {
		if r.task == taskID {
			n++
		}
	}
	return n
}

func deliveryIDs(open []ref) []string {
	out := make([]string, 0, len(open))
	for _, r := range open {
		out = append(out, r.id)
	}
	return out
}

// ---- Role recipients ----

// resolveRole finds the agent a role recipient means right now.
func resolveRole(ctx context.Context, tx *sql.Tx, task api.Task, role string) (api.Agent, error) {
	var row *sql.Row
	switch role {
	case api.RoleLead:
		if task.Orchestrator == "" {
			return api.Agent{}, fmt.Errorf("%w: role:lead cannot be resolved: the project has no lead", api.ErrConflict)
		}
		row = tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND name=? COLLATE NOCASE AND status NOT IN (?,?) ORDER BY created_at DESC LIMIT 1`, task.ID, task.Orchestrator, api.AgentClosed, api.AgentExited)
	case api.RoleDatabaseHandler:
		row = tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status NOT IN (?,?) ORDER BY created_at DESC LIMIT 1`, task.ID, api.AgentRoleDatabaseHandler, api.AgentClosed, api.AgentExited)
	default:
		return api.Agent{}, fmt.Errorf("%w: unknown role %q; use role:lead or role:database_handler", api.ErrConflict, role)
	}
	a, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("%w: role:%s cannot be resolved: no running agent holds it", api.ErrConflict, role)
	}
	return a, err
}

// messageRole is the role a typed message addresses, or "".
func messageRole(req api.PostMessageRequest) string {
	if req.Envelope != nil {
		if role, ok := strings.CutPrefix(req.Envelope.To, "role:"); ok {
			return role
		}
	}
	return ""
}

// handOffRoleObligations moves the outgoing lead's open role:lead
// obligations to the new lead, in the caller's transaction. Obligations
// addressed to the old lead by name stay where they are.
func (s *Store) handOffRoleObligations(ctx context.Context, tx *sql.Tx, task api.Task, oldLead, newLead api.Agent) error {
	if oldLead.ID == "" || newLead.ID == "" || oldLead.ID == newLead.ID {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE task_id=? AND agent_id=? AND via_role=? AND state<>? ORDER BY message_seq`, task.ID, oldLead.ID, api.RoleLead, api.ObligationClosed)
	if err != nil {
		return err
	}
	var open []api.Obligation
	for rows.Next() {
		o, err := scanObligation(rows)
		if err != nil {
			rows.Close()
			return err
		}
		open = append(open, o)
	}
	rows.Close()
	for _, o := range open {
		if _, err := s.reissueObligation(ctx, tx, task, o, newLead, "lead change", "role:lead moved to "+newLead.Name, true); err != nil {
			return err
		}
	}
	return nil
}

// leadAgent is the current agent named by the task's orchestrator, if any.
func leadAgent(ctx context.Context, tx *sql.Tx, taskID, name string) (api.Agent, error) {
	if name == "" {
		return api.Agent{}, nil
	}
	// Same rule as resolveRole: an exited or closed agent never holds the role.
	a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND name=? COLLATE NOCASE AND status NOT IN (?,?) ORDER BY created_at DESC LIMIT 1`, taskID, name, api.AgentClosed, api.AgentExited))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Agent{}, nil
	}
	return a, err
}

// ---- Owner actions ----

// ownerAction runs an owner action once per request ID: a retry with the
// same request returns the stored result, a reused ID with other data
// conflicts.
func (s *Store) ownerAction(ctx context.Context, taskID, action, targetID, requestID string, req any, by api.Caller, fn func(*sql.Tx, api.Task, time.Time) (api.OwnerActionResult, error)) (api.OwnerActionResult, error) {
	if !api.ValidID(taskID, "tsk") || !validRequestID(requestID) {
		return api.OwnerActionResult{}, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	hash := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.OwnerActionResult{}, err
	}
	defer tx.Rollback()
	var savedAction, savedTarget, savedHash, saved string
	err = tx.QueryRowContext(ctx, `SELECT action,target_id,payload_hash,result FROM owner_actions WHERE task_id=? AND request_id=?`, taskID, requestID).Scan(&savedAction, &savedTarget, &savedHash, &saved)
	if err == nil {
		if savedAction != action || savedTarget != targetID || savedHash != hash {
			return api.OwnerActionResult{}, fmt.Errorf("%w: request ID was already used for a different owner action", api.ErrConflict)
		}
		var out api.OwnerActionResult
		if err := json.Unmarshal([]byte(saved), &out); err != nil {
			return out, err
		}
		out.Replay = true
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return api.OwnerActionResult{}, err
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return api.OwnerActionResult{}, api.ErrNotFound
	}
	if err != nil {
		return api.OwnerActionResult{}, err
	}
	if task.Status != api.TaskOpen {
		return api.OwnerActionResult{}, api.ErrClosed
	}
	now := s.now()
	out, err := fn(tx, task, now)
	if err != nil {
		return out, err
	}
	out.Action = action
	encoded, _ := json.Marshal(out)
	if _, err := tx.ExecContext(ctx, `INSERT INTO owner_actions (task_id,request_id,action,target_id,payload_hash,result,by_node,by_user,created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		taskID, requestID, action, targetID, hash, string(encoded), by.Node, by.User, ts(now)); err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	s.notify(taskID)
	return out, nil
}

func openObligation(ctx context.Context, tx *sql.Tx, taskID, obligationID string) (api.Obligation, error) {
	o, err := scanObligation(tx.QueryRowContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE id=? AND task_id=?`, obligationID, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return o, api.ErrNotFound
	}
	if err != nil {
		return o, err
	}
	if o.State == api.ObligationClosed {
		return o, fmt.Errorf("%w: the obligation is already closed (%s)", api.ErrConflict, o.Outcome)
	}
	return o, nil
}

// MaxExtension bounds one extension.
const MaxExtension = 7 * 24 * time.Hour

// ExtendObligation moves an open obligation's deadlines to now + For and
// resets its escalation, so the broker re-arms from the new time.
func (s *Store) ExtendObligation(ctx context.Context, taskID, obligationID string, req api.ObligationExtendRequest, by api.Caller) (api.OwnerActionResult, error) {
	d, err := time.ParseDuration(req.For)
	if err != nil || d < time.Minute || d > MaxExtension || !api.ValidText(req.Reason, 500) {
		return api.OwnerActionResult{}, api.ErrInvalid
	}
	return s.ownerAction(ctx, taskID, "extend", obligationID, req.RequestID, req, by, func(tx *sql.Tx, task api.Task, now time.Time) (api.OwnerActionResult, error) {
		o, err := openObligation(ctx, tx, taskID, obligationID)
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		until := now.Add(d)
		ackDue := o.AckDueAt
		if o.AckedAt == nil {
			ackDue = until
		}
		// last_progress_at restarts the silence timer too, so an extension is
		// never followed by an immediate "no progress" nudge.
		if _, err := tx.ExecContext(ctx, `UPDATE obligations SET due_at=?,ack_due_at=?,escalation=0,escalated_at='',nudges=0,nudged_at='',last_progress_at=CASE WHEN acked_at<>'' THEN ? ELSE last_progress_at END,changed_at=? WHERE id=?`, ts(until), ts(ackDue), ts(now), ts(now), o.ID); err != nil {
			return api.OwnerActionResult{}, err
		}
		text := fmt.Sprintf("The owner extended message #%d (%s) until %s.", o.MessageSeq, o.Subject, until.UTC().Format("Jan 2 15:04 UTC"))
		if req.Reason != "" {
			text += " Reason: " + req.Reason
		}
		if err := s.postBrokerNotice(ctx, tx, task, api.Agent{}, "The owner extended an obligation", "The owner extended an obligation", text, map[string]string{"obligation": o.ID, "message": fmt.Sprint(o.MessageSeq)}); err != nil {
			return api.OwnerActionResult{}, err
		}
		o, err = scanObligation(tx.QueryRowContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE id=?`, o.ID))
		return api.OwnerActionResult{Obligation: &o}, err
	})
}

// AnswerObligation lets the owner answer a question, or resolve a block, on
// the recipient's behalf: the answer goes to whoever asked (or is blocked),
// and the recipient's obligation closes as answered.
func (s *Store) AnswerObligation(ctx context.Context, taskID, obligationID string, req api.ObligationAnswerRequest, by api.Caller) (api.OwnerActionResult, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" || !api.ValidText(text, 4000) {
		return api.OwnerActionResult{}, api.ErrInvalid
	}
	return s.ownerAction(ctx, taskID, "answer", obligationID, req.RequestID, req, by, func(tx *sql.Tx, task api.Task, now time.Time) (api.OwnerActionResult, error) {
		o, err := openObligation(ctx, tx, taskID, obligationID)
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		if o.Needs != api.ObligationNeedsAnswer && o.SourceKind != api.EnvelopeKindBlock {
			return api.OwnerActionResult{}, fmt.Errorf("%w: only a question or a block can be answered; cancel or reassign this one", api.ErrConflict)
		}
		source, err := originalMessage(ctx, tx, taskID, o.MessageSeq)
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		subject := "The owner answered this question"
		if o.SourceKind == api.EnvelopeKindBlock {
			subject = "The owner resolved this block"
		}
		env := &api.Envelope{Kind: api.EnvelopeKindAnswer, Subject: subject, Body: api.EnvelopeBody{Answer: text},
			Refs: map[string]string{"obligation": o.ID, "answeredFor": o.AgentID}}
		// The answer keeps the question's item links, so an item-bound asker
		// sees it in its inbox (as decision answers do).
		reply := api.PostMessageRequest{Envelope: env, ReplyTo: o.MessageSeq, RequestID: "owner-answer-" + o.ID,
			WorkItems: source.WorkItems, WorkOrderMessage: source.WorkOrderMessage}
		var asker api.Agent
		if source.From.AgentID != "" {
			if a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, source.From.AgentID)); err == nil && a.Status != api.AgentClosed {
				asker = a
				reply.To, env.To = a.ID, a.Name
			}
		}
		if err := api.NormalizeEnvelopePost(&reply); err != nil {
			return api.OwnerActionResult{}, err
		}
		// An answer obliges nobody; it wakes the asker like any directed reply.
		m, err := s.insertMessageWithResume(ctx, tx, task, reply, asker, by, false, true, false)
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE obligations SET state=?,outcome=?,outcome_seq=?,reason=?,closed_at=?,changed_at=? WHERE id=?`,
			api.ObligationClosed, api.OutcomeAnswered, m.Seq, "answered by the owner", ts(now), ts(now), o.ID); err != nil {
			return api.OwnerActionResult{}, err
		}
		o, err = scanObligation(tx.QueryRowContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE id=?`, o.ID))
		return api.OwnerActionResult{Obligation: &o, Message: &m}, err
	})
}

// CancelObligation closes an open obligation as cancelled, and tells its
// recipient so it stops working on it.
func (s *Store) CancelObligation(ctx context.Context, taskID, obligationID string, req api.ObligationCancelRequest, by api.Caller) (api.OwnerActionResult, error) {
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || !api.ValidText(reason, 500) {
		return api.OwnerActionResult{}, api.ErrInvalid
	}
	return s.ownerAction(ctx, taskID, "cancel", obligationID, req.RequestID, req, by, func(tx *sql.Tx, task api.Task, now time.Time) (api.OwnerActionResult, error) {
		o, err := openObligation(ctx, tx, taskID, obligationID)
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE obligations SET state=?,outcome=?,reason=?,closed_at=?,changed_at=? WHERE id=?`,
			api.ObligationClosed, api.OutcomeCancelled, "cancelled by the owner: "+reason, ts(now), ts(now), o.ID); err != nil {
			return api.OwnerActionResult{}, err
		}
		recipient, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, o.AgentID))
		if err != nil || recipient.Status == api.AgentClosed || recipient.Status == api.AgentExited {
			recipient = api.Agent{} // tell the board instead
		}
		text := fmt.Sprintf("The owner cancelled message #%d (%s). Stop work on it. Reason: %s", o.MessageSeq, o.Subject, reason)
		source, err := loadMessage(tx, ctx, taskID, o.MessageSeq)
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		if err := s.postLinkedBrokerNotice(ctx, tx, task, recipient, "The owner cancelled an obligation", text, map[string]string{"obligation": o.ID, "message": fmt.Sprint(o.MessageSeq)}, source, "owner-cancel-"+o.ID); err != nil {
			return api.OwnerActionResult{}, err
		}
		o, err = scanObligation(tx.QueryRowContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE id=?`, o.ID))
		return api.OwnerActionResult{Obligation: &o}, err
	})
}

// ResumeAgent re-enables inbox wake-ups for a retired agent, as tt resume does.
func (s *Store) ResumeAgent(ctx context.Context, taskID, agentID string, req api.AgentResumeRequest, by api.Caller) (api.OwnerActionResult, error) {
	return s.ownerAction(ctx, taskID, "resume", agentID, req.RequestID, req, by, func(tx *sql.Tx, task api.Task, now time.Time) (api.OwnerActionResult, error) {
		a, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=? AND task_id=?`, agentID, taskID))
		if errors.Is(err, sql.ErrNoRows) {
			return api.OwnerActionResult{}, api.ErrNotFound
		}
		if err != nil {
			return api.OwnerActionResult{}, err
		}
		if a.Status != api.AgentRetired {
			return api.OwnerActionResult{}, fmt.Errorf("%w: only a retired agent can be resumed; %s is %s", api.ErrConflict, a.Name, a.Status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET status=?,last_event_at=?,blocked_reason='',blocked_text='' WHERE id=? AND status=?`, api.AgentDone, ts(now), a.ID, api.AgentRetired); err != nil {
			return api.OwnerActionResult{}, err
		}
		if _, err := s.insertEvent(ctx, tx, taskID, api.EventResumed, a.ID, "", nil, by); err != nil {
			return api.OwnerActionResult{}, err
		}
		a.Status = api.AgentDone
		return api.OwnerActionResult{Agent: &a}, nil
	})
}

// PostSystemTextForTest posts owner free text without creating an
// obligation, as AssignLead's notice does (tests in other packages only).
func (s *Store) PostSystemTextForTest(ctx context.Context, taskID, to, text string) error {
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
	target, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, to))
	if err != nil {
		return err
	}
	if _, err := s.insertMessage(ctx, tx, task, api.PostMessageRequest{To: to, Text: text}, target, api.Caller{Node: "workspace", User: "owner"}, false, false); err != nil {
		return err
	}
	return tx.Commit()
}

// originalMessage follows refs.reassignedFrom back to the message someone
// actually wrote, so an answer reaches the original asker even after the
// obligation was reissued by a reassignment or a lead hand-off.
func originalMessage(ctx context.Context, tx *sql.Tx, taskID string, seq int64) (api.Message, error) {
	m, err := loadMessage(tx, ctx, taskID, seq)
	for hops := 0; err == nil && hops < 16 && m.From.AgentID == "" && m.Envelope != nil; hops++ {
		prev, parseErr := strconv.ParseInt(m.Envelope.Refs["reassignedFrom"], 10, 64)
		if parseErr != nil || prev <= 0 || prev == m.Seq {
			break
		}
		var older api.Message
		if older, err = loadMessage(tx, ctx, taskID, prev); err != nil {
			return m, err
		}
		m = older
	}
	return m, err
}

// postLinkedBrokerNotice is postBrokerNotice keeping a source message's
// item links, so an item-bound recipient's inbox shows it.
func (s *Store) postLinkedBrokerNotice(ctx context.Context, tx *sql.Tx, task api.Task, to api.Agent, subject, text string, refs map[string]string, source api.Message, requestID string) error {
	env := &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: subject, Refs: refs, Body: api.EnvelopeBody{Text: text}}
	if to.ID != "" {
		env.To = to.Name
	}
	req := api.PostMessageRequest{Envelope: env, To: to.ID, WorkItems: source.WorkItems, WorkOrderMessage: source.WorkOrderMessage}
	if len(source.WorkItems) > 0 {
		req.RequestID = requestID
	}
	m, err := s.insertMessageWithResume(ctx, tx, task, req, to, BrokerCaller, false, true, false)
	if err != nil {
		return err
	}
	return s.createObligations(ctx, tx, m, req, false)
}
