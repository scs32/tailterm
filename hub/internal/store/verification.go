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
	if err := requireScopeHandler(s.db, ctx, task, agent, run); err != nil {
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
	return validateVerificationKnownFailures(ctx, tx, p)
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
func validateVerificationKnownFailures(ctx context.Context, q queryRower, p api.VerificationPlan) error {
	if p.MaxAttempts == 0 && len(p.KnownFailures) == 0 {
		return nil
	}
	if p.MaxAttempts != 3 {
		return verificationConflict("approved maximum must be three attempts")
	}
	ids := map[string]bool{}
	for _, c := range p.Checks {
		ids[c.ID] = true
	}
	seen := map[string]bool{}
	for _, e := range p.KnownFailures {
		if !ids[e.CheckID] || seen[e.CheckID] || !api.ValidID(e.BugTaskID, "tsk") || !api.ValidID(e.BugID, "wi") {
			return verificationConflict("invalid or duplicate known failure")
		}
		seen[e.CheckID] = true
		var kind, status string
		if err := q.QueryRowContext(ctx, `SELECT kind,status FROM work_items WHERE task_id=? AND id=?`, e.BugTaskID, e.BugID).Scan(&kind, &status); err != nil || kind != "bug" || (status != "open" && status != "in_progress" && status != "blocked") {
			return verificationConflict("known failure requires linked open bug")
		}
	}
	return nil
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
	if err = requireScopeHandler(tx, ctx, task, req.AgentID, req.RunID); err != nil {
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
	records, err := verificationRecords(ctx, tx, task, itemID)
	if err != nil {
		return zero, err
	}
	generation := int64(len(records))
	if req.ExpectedGeneration != generation {
		return zero, verificationConflict("generation changed")
	}
	kind := "plan"
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
	}
	record := api.VerificationRecord{ItemID: itemID, ItemTaskID: task, Generation: generation + 1, Kind: kind, RequestID: req.RequestID, Digest: hash, HandlerAgentID: req.AgentID, HandlerRunID: req.RunID, CreatedAt: ts(s.now()), Plan: req.Plan, Receipt: req.Receipt}
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
	return record, nil
}
func verificationReady(ctx context.Context, tx *sql.Tx, item api.WorkItem, candidate string) error {
	required, err := verificationRequired(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return err
	}
	state, err := reviewState(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return err
	}
	if sc := scopeFor(&state, item.ScopeRevision); sc != nil && len(sc.VerificationCriteria) > 0 {
		required = true
	}
	if !required {
		return nil
	}
	records, err := verificationRecords(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		return err
	}
	p, r := currentVerification(records)
	if p == nil || r == nil || p.ScopeRevision != item.ScopeRevision || candidate == "" || p.Commit != candidate {
		return verificationConflict("passing receipt for current scope and exact accepted candidate required")
	}
	sc := scopeFor(&state, item.ScopeRevision)
	if sc == nil || sc.AssignmentSeq != p.AssignmentSeq {
		return verificationConflict("assignment changed")
	}
	for _, round := range state.Rounds {
		if round.ReviewerID == r.VerifierAgentID {
			return verificationConflict("reviewer cannot verify")
		}
	}
	if err := validateVerificationKnownFailures(ctx, tx, *p); err != nil {
		return err
	}
	return verificationEligible(*p, *r)
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
	if err := requireScopeHandler(s.db, ctx, task, agent, run); err != nil {
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
	if independent && verificationEligible(*p, *r) == nil && validateVerificationKnownFailures(ctx, s.db, *p) == nil {
		summary.State = "passing"
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
