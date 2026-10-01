package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const verificationSchema = `CREATE TABLE IF NOT EXISTS verification_records (
 task_id TEXT NOT NULL,item_id TEXT NOT NULL,generation INTEGER NOT NULL,kind TEXT NOT NULL,
 request_id TEXT NOT NULL,payload_hash TEXT NOT NULL,record_json TEXT NOT NULL,
 PRIMARY KEY(task_id,item_id,generation),UNIQUE(task_id,request_id),
 FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id));`

func verificationConflict(reason string) error {
	return fmt.Errorf("%w: verification: %s", api.ErrConflict, reason)
}

// JSON maps canonicalize key ordering on both native and host implementations.
func verificationDigest(v any) string {
	b, _ := json.Marshal(v)
	var generic any
	_ = json.Unmarshal(b, &generic)
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(generic)
	b = bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func verificationRecords(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, task, item string) ([]api.VerificationRecord, error) {
	rows, err := q.QueryContext(ctx, `SELECT record_json FROM verification_records WHERE task_id=? AND item_id=? ORDER BY generation`, task, item)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.VerificationRecord{}
	for rows.Next() {
		var raw string
		var r api.VerificationRecord
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func currentVerification(records []api.VerificationRecord) (*api.VerificationPlan, *api.VerificationReceipt) {
	var p *api.VerificationPlan
	var r *api.VerificationReceipt
	for _, v := range records {
		if v.Plan != nil {
			p = v.Plan
			r = nil
		}
		if v.Receipt != nil {
			r = v.Receipt
		}
	}
	return p, r
}
func (s *Store) VerificationHistory(ctx context.Context, task, item, agent, run string) ([]api.VerificationRecord, error) {
	if err := requireItemReader(ctx, s.db, task, item, agent, run); err != nil {
		return nil, err
	}
	return verificationRecords(ctx, s.db, task, item)
}
func availableVerificationAgent(ctx context.Context, q queryRower, task, id, run string) error {
	var actual, status string
	if err := q.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE task_id=? AND id=?`, task, id).Scan(&actual, &status); err != nil {
		return verificationConflict("exact verifier identity required")
	}
	if run == "" || actual != run || status == api.AgentRetired || status == api.AgentClosed || status == api.AgentExited {
		return verificationConflict("current available verifier run required")
	}
	return nil
}
func validateVerificationPlan(ctx context.Context, tx *sql.Tx, item api.WorkItem, p api.VerificationPlan) error {
	if p.ItemID != item.ID || p.ItemTaskID != item.TaskID || p.Version != 1 || !validRequestID(p.OperationKey) || p.ItemRevision != item.Revision || p.ScopeRevision != item.ScopeRevision || !validGitCommit(p.Commit) || !validGitCommit(p.BaseCommit) || strings.TrimSpace(p.Repository) == "" || !validContextDigest(p.MatrixDigest) || len(p.Checks) == 0 || len(p.Owned) == 0 || verificationDigest(p.Checks) != p.ChecksDigest {
		return verificationConflict("invalid current plan or check digest")
	}
	var approver, node, text string
	if p.ApprovedMatrixDigest != p.MatrixDigest || p.MatrixApprovalMessageSeq <= 0 {
		return verificationConflict("independently owner-approved matrix required")
	}
	if err := tx.QueryRowContext(ctx, `SELECT from_agent,from_node,text FROM messages WHERE task_id=? AND seq=?`, item.TaskID, p.MatrixApprovalMessageSeq).Scan(&approver, &node, &text); err != nil || approver != "" || node == "system" || strings.TrimSpace(text) != "verification-matrix-approval:"+p.MatrixDigest {
		return verificationConflict("owner matrix approval source does not bind exact digest")
	}
	if err := requireConfirmedTeamOrder(ctx, tx, item.TaskID, item.ID, item.Revision, p.OrderMessageSeq); err != nil {
		return err
	}
	state, err := reviewState(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return err
	}
	sc := scopeFor(&state, item.ScopeRevision)
	if sc == nil || sc.AssignmentSeq != p.AssignmentSeq {
		return verificationConflict("saved exact assignment required")
	}
	var target, envelope string
	if err = tx.QueryRowContext(ctx, `SELECT to_agent,envelope FROM messages WHERE task_id=? AND seq=?`, item.TaskID, p.AssignmentSeq).Scan(&target, &envelope); err != nil {
		return err
	}
	var e api.Envelope
	if json.Unmarshal([]byte(envelope), &e) != nil || e.Kind != "assign" || target != p.BuilderAgentID || (p.AssignmentOwnershipDigest != verificationDigest(e.Body.Owns)) {
		return verificationConflict("ownership and builder must match assignment")
	}
	if err = availableVerificationAgent(ctx, tx, item.TaskID, p.BuilderAgentID, p.BuilderRunID); err != nil {
		return err
	}
	if err = availableVerificationAgent(ctx, tx, item.TaskID, p.VerifierAgentID, p.VerifierRunID); err != nil {
		return err
	}
	if p.BuilderAgentID == p.VerifierAgentID {
		return verificationConflict("verifier must be distinct from builder")
	}
	for _, r := range state.Rounds {
		if r.ReviewerID == p.VerifierAgentID {
			return verificationConflict("verifier must be distinct from reviewer")
		}
	}
	// Exact item admission is required; an unrelated roster member cannot certify.
	var bound int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_work_item_bindings WHERE agent_id=? AND run_id=? AND item_task_id=? AND item_id=?`, p.VerifierAgentID, p.VerifierRunID, item.TaskID, item.ID).Scan(&bound); err != nil {
		return err
	}
	if bound != 1 {
		return verificationConflict("verifier must be admitted to this exact item")
	}
	seen := map[string]bool{}
	for _, c := range p.Checks {
		if c.ID == "" || seen[c.ID] || len(c.Argv) == 0 || c.Cwd == "" || filepath.IsAbs(c.Cwd) || strings.Contains(c.Cwd, "..") || c.Environment == nil {
			return verificationConflict("invalid or duplicate planned check")
		}
		seen[c.ID] = true
	}
	_, err = validateVerificationKnownFailures(ctx, tx, p, item, nil)
	return err
}
func validateVerificationReceipt(p api.VerificationPlan, r api.VerificationReceipt) error {
	if !filepath.IsAbs(r.Worktree) || r.Version != 1 || r.OperationKey != p.OperationKey || r.PlanDigest != verificationDigest(p) || r.Commit != p.Commit || r.BaseCommit != p.BaseCommit || r.Repository != p.Repository || r.MatrixDigest != p.MatrixDigest || r.ChecksDigest != p.ChecksDigest || r.VerifierAgentID != p.VerifierAgentID || r.VerifierRunID != p.VerifierRunID || !r.Detached || !r.CleanBefore || !r.CleanAfter || r.AIV.State != "unsubmitted" || len(r.Checks) != len(p.Checks) || len(r.Environment) == 0 {
		return verificationConflict("receipt binding, clean SHA or coverage mismatch")
	}
	logs := map[string]bool{}
	for i, c := range r.Checks {
		if !reflect.DeepEqual(c.VerificationCheck, p.Checks[i]) || !validContextDigest(c.LogDigest) || !filepath.IsAbs(c.LogURI) || c.DurationMs < 0 {
			return verificationConflict("missing, altered or duplicate check")
		}
		aggregate := api.VerificationAttempt{StartedAt: c.StartedAt, EndedAt: c.EndedAt, DurationMs: c.DurationMs, ExitCode: c.ExitCode, FailureReason: c.FailureReason, LogURI: c.LogURI, LogDigest: c.LogDigest}
		if err := validateVerificationAttempt(aggregate); err != nil {
			return err
		}
		if p.MaxAttempts == 0 {
			if len(c.Attempts) != 0 || c.Status != "" || c.KnownFailure || c.NowPassing {
				return verificationConflict("legacy result has unapproved retry policy")
			}
			continue
		}
		if p.MaxAttempts != 3 || len(c.Attempts) < 1 || len(c.Attempts) > p.MaxAttempts {
			return verificationConflict("invalid attempt count")
		}
		for j, attempt := range c.Attempts {
			if attempt.Attempt != j+1 || logs[attempt.LogURI] || (j > 0 && c.Attempts[j-1].ExitCode == 0) {
				return verificationConflict("invalid attempt order or log identity")
			}
			logs[attempt.LogURI] = true
			if err := validateVerificationAttempt(attempt); err != nil {
				return err
			}
			if j > 0 {
				priorEnd, _ := time.Parse(time.RFC3339Nano, c.Attempts[j-1].EndedAt)
				nextStart, _ := time.Parse(time.RFC3339Nano, attempt.StartedAt)
				if nextStart.Before(priorEnd) {
					return verificationConflict("overlapping attempts")
				}
			}
		}
		final := c.Attempts[len(c.Attempts)-1]
		final.Attempt = 0
		if !reflect.DeepEqual(aggregate, final) || (c.ExitCode != 0 && len(c.Attempts) != p.MaxAttempts) {
			return verificationConflict("aggregate or exhausted attempts mismatch")
		}
		status := "fail"
		if c.ExitCode == 0 {
			status = "pass"
			if len(c.Attempts) > 1 {
				status = "flaky"
			}
		}
		known := false
		for _, e := range p.KnownFailures {
			if e.CheckID == c.ID {
				known = true
			}
		}
		if c.Status != status || c.KnownFailure != known || c.NowPassing != (known && c.ExitCode == 0) {
			return verificationConflict("forged result status or known failure flags")
		}
	}
	return nil
}
func validateVerificationAttempt(a api.VerificationAttempt) error {
	start, e1 := time.Parse(time.RFC3339Nano, a.StartedAt)
	end, e2 := time.Parse(time.RFC3339Nano, a.EndedAt)
	if e1 != nil || e2 != nil || end.Before(start) || a.DurationMs < 0 || absVerificationDuration(end.Sub(start).Milliseconds()-a.DurationMs) > 2000 || !validContextDigest(a.LogDigest) || !filepath.IsAbs(a.LogURI) {
		return verificationConflict("invalid attempt evidence or times/duration")
	}
	if a.ExitCode == 0 && a.FailureReason != "" {
		return verificationConflict("successful attempt has failure reason")
	}
	switch a.FailureReason {
	case "", "exit", "timeout", "port-conflict", "port-inspection", "spawn", "output-limit":
	default:
		return verificationConflict("invalid failure reason")
	}
	return nil
}

// validateVerificationKnownFailures requires every known failure to name an
// open bug. The one exception is the closing item's own entries: self, the item
// whose completion is being checked, may be done when its current receipt r
// shows each of them passing with nowPassing. Those entries are returned as
// resolved. self's status comes from self rather than the database, so a done
// save can check the status it is about to write. Callers bind r to p and its
// exact commit before calling; a nil r resolves nothing.
func validateVerificationKnownFailures(ctx context.Context, q queryRower, p api.VerificationPlan, self api.WorkItem, r *api.VerificationReceipt) ([]api.VerificationKnownFailure, error) {
	if p.MaxAttempts == 0 && len(p.KnownFailures) == 0 {
		return nil, nil
	}
	if p.MaxAttempts != 3 {
		return nil, verificationConflict("approved maximum must be three attempts")
	}
	ids := map[string]bool{}
	for _, c := range p.Checks {
		ids[c.ID] = true
	}
	seen := map[string]bool{}
	var resolved []api.VerificationKnownFailure
	for _, e := range p.KnownFailures {
		if !ids[e.CheckID] || seen[e.CheckID] || !api.ValidID(e.BugTaskID, "tsk") || !api.ValidID(e.BugID, "wi") {
			return nil, verificationConflict("invalid or duplicate known failure")
		}
		seen[e.CheckID] = true
		var kind, status string
		if err := q.QueryRowContext(ctx, `SELECT kind,status FROM work_items WHERE task_id=? AND id=?`, e.BugTaskID, e.BugID).Scan(&kind, &status); err != nil || kind != "bug" {
			return nil, verificationConflict("known failure requires linked open bug")
		}
		isSelf := e.BugTaskID == self.TaskID && e.BugID == self.ID
		if isSelf {
			status = self.Status
		}
		if status == "open" || status == "in_progress" || status == "blocked" {
			continue
		}
		if !isSelf {
			return nil, verificationConflict("known failure requires linked open bug")
		}
		if status != "done" || !knownFailurePassing(r, e.CheckID) {
			return nil, verificationConflict("known failure for closing bug is not passing in its current receipt")
		}
		resolved = append(resolved, e)
	}
	return resolved, nil
}

// knownFailurePassing reports whether receipt r shows check id passing on its
// first attempt as a known failure that now passes. A flaky pass does not count.
func knownFailurePassing(r *api.VerificationReceipt, id string) bool {
	if r == nil {
		return false
	}
	for _, c := range r.Checks {
		if c.ID == id {
			return c.Status == "pass" && c.NowPassing && c.KnownFailure && c.ExitCode == 0
		}
	}
	return false
}
func verificationEligible(p api.VerificationPlan, r api.VerificationReceipt) error {
	if err := validateVerificationReceipt(p, r); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, e := range p.KnownFailures {
		known[e.CheckID] = true
	}
	for _, c := range r.Checks {
		if c.ExitCode != 0 && !known[c.ID] {
			return verificationConflict("unlisted exhausted failure blocks completion")
		}
	}
	return nil
}
func absVerificationDuration(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// verificationAuthor admits the author of a plan freeze or receipt import and
// returns the role saved on its record: empty for a database handler, lead
// for the item lead, verifier for the current plan's verifier. A handler is
// admitted as before. Only the lead may freeze a plan besides the handler;
// the lead or the current plan's exact verifier run may import its receipt.
func verificationAuthor(ctx context.Context, q queryRower, task, item string, req api.VerificationRequest, records []api.VerificationRecord) (string, error) {
	plan, _ := currentVerification(records)
	var order int64
	if req.Plan != nil {
		order = req.Plan.OrderMessageSeq
	} else if plan != nil {
		order = plan.OrderMessageSeq
	}
	role, _, err := requireItemOperator(ctx, q, task, item, req.AgentID, req.RunID, req.ContextDigest, order)
	if err != nil {
		return "", err
	}
	switch {
	case role == itemOperatorHandler:
		return "", nil
	case role == itemOperatorLead:
		return itemOperatorLead, nil
	case req.Plan != nil:
		return "", verificationConflict("only the item lead or the database handler may freeze a verification plan")
	case plan != nil && plan.VerifierAgentID == req.AgentID && plan.VerifierRunID == req.RunID:
		return "verifier", nil
	}
	return "", verificationConflict("only the plan's verifier, the item lead or the database handler may import a receipt")
}

// receiptNoticeTargets lists who depends on an item's receipt, in order: the
// item lead, each distinct reviewer of the item's current scope, then the
// handler: the running entry's leased handler, or, with no running entry or
// a leased handler that is closed, exited or retired, every available
// database handler in the project. The importing agent is left out and an
// agent is listed once, under its first role.
func receiptNoticeTargets(ctx context.Context, tx *sql.Tx, item api.WorkItem, author string) ([]api.VerificationNotice, error) {
	seen := map[string]bool{author: true, "": true}
	var out []api.VerificationNotice
	add := func(role, agent string) {
		if !seen[agent] {
			seen[agent] = true
			out = append(out, api.VerificationNotice{Role: role, AgentID: agent})
		}
	}
	var lead string
	err := tx.QueryRowContext(ctx, `SELECT agent_id FROM item_team_leads WHERE task_id=? AND item_id=? AND state<>'closed'`, item.TaskID, item.ID).Scan(&lead)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	add("lead", lead)
	state, err := reviewState(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return nil, err
	}
	for _, round := range state.Rounds {
		if round.ScopeRevision == item.ScopeRevision {
			add("reviewer", round.ReviewerID)
		}
	}
	var handler string
	err = tx.QueryRowContext(ctx, `SELECT handler_id FROM team_queue_entries WHERE task_id=? AND item_id=? AND state='running' ORDER BY attempt DESC LIMIT 1`, item.TaskID, item.ID).Scan(&handler)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && handler != "" {
		// The lease is listed even when its handler is gone, so the record
		// shows it skipped; an available lease is the only handler told.
		add("handler", handler)
		var status string
		err = tx.QueryRowContext(ctx, `SELECT status FROM agents WHERE task_id=? AND id=?`, item.TaskID, handler).Scan(&status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil && !unavailableAgentStatus(status) {
			return out, nil
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM agents WHERE task_id=? AND role=? AND status NOT IN (?,?,?) ORDER BY created_at,id`, item.TaskID, api.AgentRoleDatabaseHandler, api.AgentClosed, api.AgentExited, api.AgentRetired)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		if err = rows.Scan(&handler); err != nil {
			return nil, err
		}
		add("handler", handler)
	}
	return out, rows.Err()
}

// postReceiptNotices tells every dependent actor that a receipt was imported,
// inside the importing transaction and before the record is written, and
// returns what the record saves: each notice's message, or why a recipient
// was not told. A closed, exited or retired recipient is skipped. Any insert
// failure refuses the whole import, so no notice exists without its record.
func (s *Store) postReceiptNotices(ctx context.Context, tx *sql.Tx, item api.WorkItem, p api.VerificationPlan, r api.VerificationReceipt, generation int64, author string) ([]api.VerificationNotice, error) {
	notices, err := receiptNoticeTargets(ctx, tx, item, author)
	if err != nil || len(notices) == 0 {
		return nil, err
	}
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, item.TaskID))
	if err != nil {
		return nil, err
	}
	state := "blocked"
	if verificationEligible(p, r) == nil {
		if _, err := validateVerificationKnownFailures(ctx, tx, p, item, &r); err == nil {
			state = "passing"
		}
	}
	text := fmt.Sprintf("The verification receipt for commit %s was imported as generation %d and is %s.", p.Commit, generation, state)
	if state == "blocked" {
		var failed []string
		for _, c := range r.Checks {
			if c.ExitCode != 0 {
				failed = append(failed, c.ID)
			}
		}
		if len(failed) == 0 {
			text += " No check failed; a known failure's bug no longer covers it."
		} else {
			text += " Failed checks: " + strings.Join(failed, ", ") + "."
		}
	}
	text += " No reply is needed."
	for i := range notices {
		n := &notices[i]
		target, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=?`, item.TaskID, n.AgentID))
		if errors.Is(err, sql.ErrNoRows) {
			n.Skipped = api.AgentClosed
			continue
		}
		if err != nil {
			return nil, err
		}
		if unavailableAgentStatus(target.Status) {
			n.Skipped = target.Status
			continue
		}
		envelope := api.Envelope{Kind: api.EnvelopeKindNotice, To: target.Name, Subject: "Verification receipt imported: " + state,
			Refs: map[string]string{"item": item.ID, "commit": p.Commit, "generation": fmt.Sprint(generation), "state": state},
			Body: api.EnvelopeBody{Text: text}}
		post := api.PostMessageRequest{Envelope: &envelope, To: target.ID, RequestID: fmt.Sprintf("verification-receipt-%s-%d-%s", item.ID, generation, target.ID), WorkOrderMessage: &api.MessageReference{TaskID: item.TaskID, Seq: p.OrderMessageSeq},
			WorkItems: []api.MessageWorkItem{{ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}
		m, err := s.insertMessageWithResume(ctx, tx, t, post, target, api.Caller{Node: "verification", User: "hub"}, false, false, false)
		if err != nil {
			return nil, fmt.Errorf("%w: verification: receipt notice to %s %s failed: %v; nothing was saved, retry with the same request id", api.ErrConflict, n.Role, n.AgentID, err)
		}
		n.MessageSeq = m.Seq
	}
	return notices, nil
}

func (s *Store) SaveVerification(ctx context.Context, task, itemID string, req api.VerificationRequest) (api.VerificationRecord, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	zero := api.VerificationRecord{}
	if !api.ValidID(task, "tsk") || !api.ValidID(itemID, "wi") || !validRequestID(req.RequestID) || (req.Plan == nil) == (req.Receipt == nil) {
		return zero, api.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	hash := verificationDigest(req)
	var priorHash, raw string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash,record_json FROM verification_records WHERE task_id=? AND request_id=?`, task, req.RequestID).Scan(&priorHash, &raw)
	if err == nil {
		if hash != priorHash {
			return zero, verificationConflict("retry payload changed")
		}
		err = json.Unmarshal([]byte(raw), &zero)
		if err == nil && (zero.ItemID != itemID || zero.ItemTaskID != task) {
			return api.VerificationRecord{}, verificationConflict("retry belongs to a different item")
		}
		return zero, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	records, err := verificationRecords(ctx, tx, task, itemID)
	if err != nil {
		return zero, err
	}
	authorRole, err := verificationAuthor(ctx, tx, task, itemID, req, records)
	if err != nil {
		return zero, err
	}
	if err = ackGate(ctx, tx, req.AgentID, req.RunID, 0, s.now()); err != nil {
		return zero, err
	}
	item, err := getWorkItem(tx, ctx, task, itemID)
	if err != nil {
		return zero, err
	}
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id=?`, task).Scan(&status); err != nil {
		return zero, err
	}
	if status != api.TaskOpen {
		return zero, api.ErrClosed
	}
	generation := int64(len(records))
	if req.ExpectedGeneration != generation {
		return zero, verificationConflict("generation changed")
	}
	kind := "plan"
	var notices []api.VerificationNotice
	if req.Plan != nil {
		if err = validateVerificationPlan(ctx, tx, item, *req.Plan); err != nil {
			return zero, err
		}
	} else {
		kind = "receipt"
		p, r := currentVerification(records)
		if p == nil || r != nil {
			return zero, verificationConflict("unreceipted current plan required")
		}
		if item.ScopeRevision != p.ScopeRevision {
			return zero, verificationConflict("scope changed")
		}
		if err = availableVerificationAgent(ctx, tx, task, p.BuilderAgentID, p.BuilderRunID); err != nil {
			return zero, err
		}
		if err = availableVerificationAgent(ctx, tx, task, p.VerifierAgentID, p.VerifierRunID); err != nil {
			return zero, err
		}
		state, e := reviewState(ctx, tx, task, itemID)
		if e != nil {
			return zero, e
		}
		for _, round := range state.Rounds {
			if round.ReviewerID == p.VerifierAgentID {
				return zero, verificationConflict("reviewer cannot verify")
			}
		}
		if err = validateVerificationReceipt(*p, *req.Receipt); err != nil {
			return zero, err
		}
		if notices, err = s.postReceiptNotices(ctx, tx, item, *p, *req.Receipt, generation+1, req.AgentID); err != nil {
			return zero, err
		}
	}
	record := api.VerificationRecord{ItemID: itemID, ItemTaskID: task, Generation: generation + 1, Kind: kind, RequestID: req.RequestID, Digest: hash, HandlerAgentID: req.AgentID, HandlerRunID: req.RunID, CreatedAt: ts(s.now()), Plan: req.Plan, Receipt: req.Receipt, AuthorRole: authorRole, Notices: notices}
	b, err := json.Marshal(record)
	if err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO verification_records VALUES(?,?,?,?,?,?,?)`, task, itemID, record.Generation, kind, req.RequestID, hash, string(b)); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	if len(notices) > 0 {
		s.notify(task)
	}
	return record, nil
}

// verificationCompletion is the item's exact-candidate verification state for
// completion. It is nil when verification is not required. Resolved lists the
// item's own known failures that its current receipt shows now passing.
type verificationCompletion struct {
	Plan              api.VerificationPlan
	Receipt           api.VerificationReceipt
	ReceiptGeneration int64
	Resolved          []api.VerificationKnownFailure
}

func verificationReady(ctx context.Context, tx *sql.Tx, item api.WorkItem, candidate string) error {
	_, err := checkVerificationCompletion(ctx, tx, item, candidate)
	return err
}
func checkVerificationCompletion(ctx context.Context, tx *sql.Tx, item api.WorkItem, candidate string) (*verificationCompletion, error) {
	required, err := verificationRequired(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return nil, err
	}
	state, err := reviewState(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return nil, err
	}
	if sc := scopeFor(&state, item.ScopeRevision); sc != nil && len(sc.VerificationCriteria) > 0 {
		required = true
	}
	if !required {
		return nil, nil
	}
	records, err := verificationRecords(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return nil, err
	}
	p, r := currentVerification(records)
	if p == nil || r == nil || p.ScopeRevision != item.ScopeRevision || candidate == "" || p.Commit != candidate {
		return nil, verificationConflict("passing receipt for current scope and exact accepted candidate required")
	}
	sc := scopeFor(&state, item.ScopeRevision)
	if sc == nil || sc.AssignmentSeq != p.AssignmentSeq {
		return nil, verificationConflict("assignment changed")
	}
	for _, round := range state.Rounds {
		if round.ReviewerID == r.VerifierAgentID {
			return nil, verificationConflict("reviewer cannot verify")
		}
	}
	// The receipt is bound to the plan and candidate before any known
	// failure can be resolved by it.
	if err = verificationEligible(*p, *r); err != nil {
		return nil, err
	}
	resolved, err := validateVerificationKnownFailures(ctx, tx, *p, item, r)
	if err != nil {
		return nil, err
	}
	return &verificationCompletion{Plan: *p, Receipt: *r, ReceiptGeneration: currentReceiptGeneration(records), Resolved: resolved}, nil
}

// currentReceiptGeneration is the generation of the receipt currentVerification
// returns, or 0 when the current plan has none.
func currentReceiptGeneration(records []api.VerificationRecord) int64 {
	var generation int64
	for _, v := range records {
		if v.Plan != nil {
			generation = 0
		}
		if v.Receipt != nil {
			generation = v.Generation
		}
	}
	return generation
}

const verificationEnrollmentSchema = `CREATE TABLE IF NOT EXISTS verification_enrollments (
 task_id TEXT NOT NULL,item_id TEXT NOT NULL,agent_id TEXT NOT NULL,run_id TEXT NOT NULL,
 required INTEGER NOT NULL CHECK(required IN (0,1)),provenance TEXT NOT NULL,created_at TEXT NOT NULL,
 PRIMARY KEY(task_id,item_id,agent_id,run_id),FOREIGN KEY(task_id,item_id) REFERENCES work_items(task_id,id));
CREATE TRIGGER IF NOT EXISTS verification_enrollment_no_update BEFORE UPDATE ON verification_enrollments BEGIN SELECT RAISE(ABORT,'verification enrollment is immutable'); END;
CREATE TRIGGER IF NOT EXISTS verification_enrollment_no_delete BEFORE DELETE ON verification_enrollments BEGIN SELECT RAISE(ABORT,'verification enrollment is immutable'); END;`

func migrateVerificationEnrollment(db *sql.DB) error {
	var exists int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='verification_enrollments'`).Scan(&exists); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(verificationEnrollmentSchema); err != nil {
		return err
	}
	if exists == 0 {
		if _, err = tx.Exec(`INSERT OR IGNORE INTO verification_enrollments SELECT b.item_task_id,b.item_id,b.agent_id,b.run_id,0,'legacy-pre-rollout',b.created_at FROM agent_work_item_bindings b JOIN work_items w ON w.task_id=b.item_task_id AND w.id=b.item_id`); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func verificationRequired(ctx context.Context, q queryRower, task, item string) (bool, error) {
	var count int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM verification_enrollments WHERE task_id=? AND item_id=? AND required=1`, task, item).Scan(&count)
	return count > 0, err
}
func (s *Store) VerificationEnrollment(ctx context.Context, task, item, agent, run string) ([]api.VerificationEnrollment, error) {
	if err := requireItemReader(ctx, s.db, task, item, agent, run); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id,run_id,required,provenance,created_at FROM verification_enrollments WHERE task_id=? AND item_id=? ORDER BY created_at,agent_id,run_id`, task, item)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.VerificationEnrollment{}
	for rows.Next() {
		var entry api.VerificationEnrollment
		if err = rows.Scan(&entry.AgentID, &entry.RunID, &entry.Required, &entry.Provenance, &entry.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		out = append(out, api.VerificationEnrollment{Provenance: "legacy-no-team-admission"})
	}
	return out, nil
}

func (s *Store) loadTeamVerification(ctx context.Context, e *api.TeamQueueEntry) error {
	records, err := verificationRecords(ctx, s.db, e.TaskID, e.ItemID)
	if err != nil {
		return err
	}
	p, r := currentVerification(records)
	if p == nil {
		return nil
	}
	summary := &api.VerificationSummary{State: "pending", Commit: p.Commit}
	e.Verification = summary
	var scope int64
	if err = s.db.QueryRowContext(ctx, `SELECT narrative_scope_revision FROM work_items WHERE task_id=? AND id=?`, e.TaskID, e.ItemID).Scan(&scope); err != nil {
		return err
	}
	if scope != p.ScopeRevision {
		summary.State = "stale"
		return nil
	}
	state, err := reviewState(ctx, s.db, e.TaskID, e.ItemID)
	if err != nil {
		return err
	}
	sc := scopeFor(&state, scope)
	if sc == nil || sc.AssignmentSeq != p.AssignmentSeq {
		summary.State = "stale"
		return nil
	}
	if r == nil {
		return nil
	}
	if validateVerificationReceipt(*p, *r) != nil {
		summary.State = "invalid"
		return nil
	}
	summary.State = "blocked"
	independent := true
	for _, round := range state.Rounds {
		if round.ReviewerID == p.VerifierAgentID {
			independent = false
		}
	}
	if independent && verificationEligible(*p, *r) == nil {
		item, err := getWorkItem(s.db, ctx, e.TaskID, e.ItemID)
		if err != nil {
			return err
		}
		if _, err = validateVerificationKnownFailures(ctx, s.db, *p, item, r); err == nil {
			summary.State = "passing"
		}
	}
	for _, c := range r.Checks {
		status := c.Status
		if status == "" {
			status = "fail"
			if c.ExitCode == 0 {
				status = "pass"
			}
		}
		summary.Checks = append(summary.Checks, api.VerificationCheckSummary{ID: c.ID, Status: status, KnownFailure: c.KnownFailure, NowPassing: c.NowPassing})
	}
	return nil
}
