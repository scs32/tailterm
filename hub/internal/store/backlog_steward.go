package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Backlog steward (docs/backlog-steward.md). One steward holds a project's
// slot at a time. A rotation successor is admitted as a non-holding steward
// (steward_pending=1) until its rotation commits.
func migrateBacklogSteward(db *sql.DB) error {
	var policyExisted int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='steward_rotation_policy'`).Scan(&policyExisted); err != nil {
		return err
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('agents') WHERE name='steward_pending'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE agents ADD COLUMN steward_pending INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	_, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS agents_one_backlog_steward ON agents(task_id) WHERE role='backlog_steward' AND status<>'closed' AND steward_pending=0;
CREATE TABLE IF NOT EXISTS steward_runs (
 task_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT PRIMARY KEY, template_digest TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS backlog_summaries (
 task_id TEXT NOT NULL REFERENCES tasks(id), revision INTEGER NOT NULL, body TEXT NOT NULL, digest TEXT NOT NULL,
 agent_id TEXT NOT NULL, run_id TEXT NOT NULL, request_id TEXT NOT NULL, payload_hash TEXT NOT NULL,
 created_node TEXT NOT NULL, created_user TEXT NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY(task_id,revision), UNIQUE(task_id,request_id));
CREATE TABLE IF NOT EXISTS steward_rotation_policy (
 task_id TEXT PRIMARY KEY REFERENCES tasks(id), enabled INTEGER NOT NULL, max_total_tokens INTEGER NOT NULL,
 on_template_change INTEGER NOT NULL, revision INTEGER NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS steward_rotations (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), request_id TEXT NOT NULL, payload_hash TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('prepared','committed','aborted')), reason TEXT NOT NULL, trigger_kind TEXT NOT NULL,
 old_agent_id TEXT NOT NULL, old_run_id TEXT NOT NULL, old_name TEXT NOT NULL,
 successor_agent_id TEXT NOT NULL, successor_name TEXT NOT NULL, successor_run_id TEXT NOT NULL DEFAULT '',
 summary_revision INTEGER NOT NULL, summary_digest TEXT NOT NULL,
 handoff_json TEXT NOT NULL DEFAULT '', receipt_json TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 UNIQUE(task_id,request_id));
CREATE UNIQUE INDEX IF NOT EXISTS steward_rotations_open ON steward_rotations(task_id) WHERE state='prepared';
CREATE TABLE IF NOT EXISTS steward_rotation_requests (
 task_id TEXT NOT NULL, request_id TEXT NOT NULL, operation TEXT NOT NULL, payload_hash TEXT NOT NULL,
 rotation_id TEXT NOT NULL, PRIMARY KEY(task_id,request_id));`)
	if err != nil {
		return err
	}
	if policyExisted == 0 {
		// Rotation is on by default for projects created after this
		// migration; the owner turns it on for projects that existed before.
		if _, err = db.Exec(`INSERT OR IGNORE INTO steward_rotation_policy(task_id,enabled,max_total_tokens,on_template_change,revision,updated_at)
 SELECT id,0,?,1,1,? FROM tasks`, api.DefaultHandlerRotationMaxTotalTokens, ts(time.Now().UTC())); err != nil {
			return err
		}
	}
	return nil
}

// stewardSlotHolder is the project's slot-holding steward: any status but
// closed, and not a pending rotation successor.
func stewardSlotHolder(ctx context.Context, q queryRower, task string) (api.Agent, bool, error) {
	a, err := scanAgent(q.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status<>? AND steward_pending=0 ORDER BY created_at,id LIMIT 1`,
		task, api.AgentRoleBacklogSteward, api.AgentClosed))
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	return a, err == nil, err
}

// activeSteward is the steward that receives role:backlog_steward messages
// and may run triage: the slot holder while it is neither closed nor exited.
func activeSteward(ctx context.Context, q queryRower, task string) (api.Agent, bool, error) {
	a, ok, err := stewardSlotHolder(ctx, q, task)
	if err != nil || !ok || a.Status == api.AgentExited {
		return api.Agent{}, false, err
	}
	return a, true, nil
}

func stewardActiveRefusal(holder api.Agent) error {
	return &api.StewardRefusal{Code: api.StewardRefusedActive, Detail: fmt.Sprintf("the project's backlog steward is %s (%s, %s); close or rotate it first", holder.Name, holder.ID, holder.Status)}
}

// admitStewardTx runs in the admission transaction just before a fresh
// steward row is inserted. It returns whether the new row is a pending
// rotation successor.
func admitStewardTx(ctx context.Context, tx *sql.Tx, task string, agentID string) (bool, error) {
	holder, held, err := stewardSlotHolder(ctx, tx, task)
	if err != nil {
		return false, err
	}
	pending, err := stewardPendingSuccessor(ctx, tx, task, agentID)
	if err != nil {
		return false, err
	}
	if pending {
		return true, nil
	}
	if held {
		return false, stewardActiveRefusal(holder)
	}
	return false, stewardRotationOpenRefusal(ctx, tx, task)
}

// stewardUniqueViolation maps the one-steward index to its named refusal, so
// no admission path returns a raw constraint error.
func stewardUniqueViolation(ctx context.Context, q queryRower, task string, err error) error {
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") || !strings.Contains(err.Error(), "agents.task_id") {
		return err
	}
	holder, held, lookupErr := stewardSlotHolder(ctx, q, task)
	if lookupErr != nil || !held {
		return &api.StewardRefusal{Code: api.StewardRefusedActive, Detail: "the project already has a backlog steward"}
	}
	return stewardActiveRefusal(holder)
}

// recordStewardRun keeps the template digest of an exact steward run. The
// first digest recorded for a run wins.
func (s *Store) recordStewardRun(ctx context.Context, a api.Agent, digest string) error {
	if a.Role != api.AgentRoleBacklogSteward || digest == "" || a.RunID == "" {
		return nil
	}
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO steward_runs(task_id,agent_id,run_id,template_digest,created_at) VALUES(?,?,?,?,?)`, a.TaskID, a.ID, a.RunID, digest, ts(s.now()))
	return err
}

// stewardPendingSuccessor reports whether agentID is the successor of the
// project's prepared steward rotation.
func stewardPendingSuccessor(ctx context.Context, q queryRower, task, agentID string) (bool, error) {
	r, err := openStewardRotation(ctx, q, task)
	if err != nil || r == nil {
		return false, err
	}
	return r.SuccessorAgentID == agentID, nil
}

// stewardRotationOpenRefusal refuses a steward admission while a rotation is
// prepared.
func stewardRotationOpenRefusal(ctx context.Context, q queryRower, task string) error {
	r, err := openStewardRotation(ctx, q, task)
	if err != nil || r == nil {
		return err
	}
	return &api.StewardRefusal{Code: api.StewardRefusedRotationOpen, Detail: "steward rotation " + r.ID + " is prepared; finish or abort it first"}
}

// BacklogStewardStatus reads the project's steward for launches and briefings.
func (s *Store) BacklogStewardStatus(ctx context.Context, task string) (api.BacklogStewardStatus, error) {
	out := api.BacklogStewardStatus{TaskID: task}
	if _, err := s.GetTask(ctx, task); err != nil {
		return out, err
	}
	holder, held, err := stewardSlotHolder(ctx, s.db, task)
	if err != nil {
		return out, err
	}
	if held {
		out.Holder = &holder
		if holder.Status != api.AgentExited {
			active := holder
			out.Steward = &active
		}
	}
	err = s.db.QueryRowContext(ctx, `SELECT id FROM agents WHERE task_id=? AND role=? AND status<>? AND steward_pending=1 ORDER BY created_at,id LIMIT 1`,
		task, api.AgentRoleBacklogSteward, api.AgentClosed).Scan(&out.PendingSuccessorID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT revision,digest FROM backlog_summaries WHERE task_id=? ORDER BY revision DESC LIMIT 1`, task).Scan(&out.SummaryRevision, &out.SummaryDigest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	return out, nil
}

// ---- Backlog summary ----

const backlogSummaryCols = `task_id,revision,body,digest,agent_id,run_id,request_id,created_node,created_user,created_at`

func scanBacklogSummary(row interface{ Scan(...any) error }) (api.BacklogSummary, error) {
	var b api.BacklogSummary
	var created string
	err := row.Scan(&b.TaskID, &b.Revision, &b.Body, &b.Digest, &b.AgentID, &b.RunID, &b.RequestID, &b.CreatedBy.Node, &b.CreatedBy.User, &created)
	b.CreatedAt, b.Bytes = parseTS(created), len(b.Body)
	return b, err
}

func summaryDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// BacklogSummary reads the latest revision, or rev when rev > 0. Any caller
// may read it.
func (s *Store) BacklogSummary(ctx context.Context, task string, rev int64) (api.BacklogSummary, error) {
	if !api.ValidID(task, "tsk") || rev < 0 {
		return api.BacklogSummary{}, api.ErrInvalid
	}
	if _, err := s.GetTask(ctx, task); err != nil {
		return api.BacklogSummary{}, err
	}
	var row *sql.Row
	if rev > 0 {
		row = s.db.QueryRowContext(ctx, `SELECT `+backlogSummaryCols+` FROM backlog_summaries WHERE task_id=? AND revision=?`, task, rev)
	} else {
		row = s.db.QueryRowContext(ctx, `SELECT `+backlogSummaryCols+` FROM backlog_summaries WHERE task_id=? ORDER BY revision DESC LIMIT 1`, task)
	}
	b, err := scanBacklogSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return b, api.ErrNotFound
	}
	return b, err
}

// BacklogSummaryRevisions lists every revision, oldest first, without bodies.
func (s *Store) BacklogSummaryRevisions(ctx context.Context, task string) ([]api.BacklogSummary, error) {
	if _, err := s.GetTask(ctx, task); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+backlogSummaryCols+` FROM backlog_summaries WHERE task_id=? ORDER BY revision`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.BacklogSummary{}
	for rows.Next() {
		b, err := scanBacklogSummary(rows)
		if err != nil {
			return nil, err
		}
		b.Body = ""
		out = append(out, b)
	}
	return out, rows.Err()
}

// SaveBacklogSummary appends the next revision. Only the exact run of the
// project's active steward, or the owner, may save; a stale expected
// revision is refused, and a replayed request ID returns the saved row.
func (s *Store) SaveBacklogSummary(ctx context.Context, task string, req api.SaveBacklogSummaryRequest, by api.Caller) (api.BacklogSummary, error) {
	var zero api.BacklogSummary
	if !api.ValidID(task, "tsk") || !validRequestID(req.RequestID) || req.ExpectedRevision < 0 || strings.TrimSpace(req.Body) == "" ||
		len(req.Body) > api.MaxBacklogSummaryLen || !utf8.ValidString(req.Body) || strings.ContainsRune(req.Body, 0) {
		return zero, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	hash := requestHash(req)
	prior, err := scanBacklogSummary(tx.QueryRowContext(ctx, `SELECT `+backlogSummaryCols+` FROM backlog_summaries WHERE task_id=? AND request_id=?`, task, req.RequestID))
	if err == nil {
		var priorHash string
		if err = tx.QueryRowContext(ctx, `SELECT payload_hash FROM backlog_summaries WHERE task_id=? AND request_id=?`, task, req.RequestID).Scan(&priorHash); err != nil {
			return zero, err
		}
		if priorHash != hash {
			return zero, workItemConflict("backlog summary request ID was already used with different input")
		}
		return prior, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, task))
	if errors.Is(err, sql.ErrNoRows) {
		return zero, api.ErrNotFound
	}
	if err != nil {
		return zero, err
	}
	if t.Status != api.TaskOpen {
		return zero, api.ErrClosed
	}
	if req.AgentID != "" || req.RunID != "" {
		if err = requireActiveStewardRun(ctx, tx, task, req.AgentID, req.RunID); err != nil {
			return zero, fmt.Errorf("%w: only the owner or the active backlog steward's exact run may save the backlog summary", api.ErrConflict)
		}
	}
	var current int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM backlog_summaries WHERE task_id=?`, task).Scan(&current); err != nil {
		return zero, err
	}
	if current != req.ExpectedRevision {
		return zero, workItemConflict(fmt.Sprintf("the backlog summary is at revision %d; read it and save against that revision", current))
	}
	now := s.now()
	out := api.BacklogSummary{TaskID: task, Revision: current + 1, Body: req.Body, Digest: summaryDigest(req.Body), Bytes: len(req.Body), AgentID: req.AgentID, RunID: req.RunID,
		RequestID: req.RequestID, CreatedBy: by, CreatedAt: now}
	if _, err = tx.ExecContext(ctx, `INSERT INTO backlog_summaries(`+backlogSummaryCols+`,payload_hash) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		out.TaskID, out.Revision, out.Body, out.Digest, out.AgentID, out.RunID, out.RequestID, by.Node, by.User, ts(now), hash); err != nil {
		return zero, err
	}
	if _, err = s.insertEvent(ctx, tx, task, "task_updated", req.AgentID, "Backlog summary saved", map[string]any{"backlogSummaryRevision": out.Revision, "digest": out.Digest, "bytes": out.Bytes}, by); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return out, nil
}

// requireActiveStewardRun admits only the exact run of the project's active
// steward: not pending, retired, closed or exited.
func requireActiveStewardRun(ctx context.Context, q queryRower, task, agentID, runID string) error {
	if agentID == "" || runID == "" {
		return api.ErrConflict
	}
	a, ok, err := activeSteward(ctx, q, task)
	if err != nil {
		return err
	}
	if !ok || a.ID != agentID || a.RunID != runID || a.Status == api.AgentRetired {
		return fmt.Errorf("%w: not the active backlog steward's exact run", api.ErrConflict)
	}
	return nil
}

// ---- Steward rotation ----

func defaultStewardRotationPolicy(task string) api.StewardRotationPolicy {
	return api.StewardRotationPolicy{TaskID: task, Enabled: true, MaxTotalTokens: api.DefaultHandlerRotationMaxTotalTokens, OnTemplateChange: true}
}

func loadStewardRotationPolicy(ctx context.Context, q queryRower, task string) (api.StewardRotationPolicy, error) {
	p := defaultStewardRotationPolicy(task)
	var updated string
	err := q.QueryRowContext(ctx, `SELECT enabled,max_total_tokens,on_template_change,revision,updated_at FROM steward_rotation_policy WHERE task_id=?`, task).
		Scan(&p.Enabled, &p.MaxTotalTokens, &p.OnTemplateChange, &p.Revision, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	at := parseTS(updated)
	p.UpdatedAt = &at
	return p, nil
}

func (s *Store) StewardRotationPolicy(ctx context.Context, task string) (api.StewardRotationPolicy, error) {
	if _, err := s.GetTask(ctx, task); err != nil {
		return api.StewardRotationPolicy{}, err
	}
	return loadStewardRotationPolicy(ctx, s.db, task)
}

func (s *Store) SetStewardRotationPolicy(ctx context.Context, task string, req api.StewardRotationPolicyRequest, by api.Caller) (api.StewardRotationPolicy, error) {
	var zero api.StewardRotationPolicy
	if req.ActorAgentID != "" {
		return zero, &api.StewardRefusal{Code: api.StewardRefusedAgentCaller, Detail: "the steward rotation policy is the owner's; agent sessions cannot change it"}
	}
	if req.ExpectedRevision < 0 || req.MaxTotalTokens < 0 {
		return zero, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, task))
	if errors.Is(err, sql.ErrNoRows) {
		return zero, api.ErrNotFound
	}
	if err != nil {
		return zero, err
	}
	if t.Status != api.TaskOpen {
		return zero, api.ErrClosed
	}
	current, err := loadStewardRotationPolicy(ctx, tx, task)
	if err != nil {
		return zero, err
	}
	if current.Revision != req.ExpectedRevision {
		return zero, &api.StewardRefusal{Code: api.StewardRefusedStalePolicy, Detail: fmt.Sprintf("policy is at revision %d; reload before changing it", current.Revision)}
	}
	now := s.now()
	if _, err = tx.ExecContext(ctx, `INSERT INTO steward_rotation_policy(task_id,enabled,max_total_tokens,on_template_change,revision,updated_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(task_id) DO UPDATE SET enabled=excluded.enabled,max_total_tokens=excluded.max_total_tokens,on_template_change=excluded.on_template_change,revision=excluded.revision,updated_at=excluded.updated_at`,
		task, req.Enabled, req.MaxTotalTokens, req.OnTemplateChange, current.Revision+1, ts(now)); err != nil {
		return zero, err
	}
	out := api.StewardRotationPolicy{TaskID: task, Enabled: req.Enabled, MaxTotalTokens: req.MaxTotalTokens, OnTemplateChange: req.OnTemplateChange, Revision: current.Revision + 1, UpdatedAt: &now}
	if _, err = s.insertEvent(ctx, tx, task, "task_updated", "", "Steward rotation policy saved", map[string]any{"stewardRotationPolicy": out}, by); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return out, nil
}

const stewardRotationCols = `id,task_id,request_id,state,reason,trigger_kind,old_agent_id,old_run_id,old_name,successor_agent_id,successor_name,successor_run_id,summary_revision,summary_digest,handoff_json,receipt_json,created_at,updated_at`

func scanStewardRotation(row interface{ Scan(...any) error }) (api.StewardRotation, error) {
	var r api.StewardRotation
	var handoff, receipt, created, updated string
	if err := row.Scan(&r.ID, &r.TaskID, &r.RequestID, &r.State, &r.Reason, &r.Trigger, &r.OldAgentID, &r.OldRunID, &r.OldName,
		&r.SuccessorAgentID, &r.SuccessorName, &r.SuccessorRunID, &r.SummaryRevision, &r.SummaryDigest, &handoff, &receipt, &created, &updated); err != nil {
		return r, err
	}
	r.CreatedAt, r.UpdatedAt = parseTS(created), parseTS(updated)
	if handoff != "" {
		r.Handoff = &api.StewardRotationHandoff{}
		if err := json.Unmarshal([]byte(handoff), r.Handoff); err != nil {
			return r, fmt.Errorf("steward rotation handoff: %w", err)
		}
	}
	if receipt != "" {
		r.Receipt = &api.StewardRotationReceipt{}
		if err := json.Unmarshal([]byte(receipt), r.Receipt); err != nil {
			return r, fmt.Errorf("steward rotation receipt: %w", err)
		}
	}
	return r, nil
}

func loadStewardRotation(ctx context.Context, q queryRower, task, id string) (api.StewardRotation, error) {
	r, err := scanStewardRotation(q.QueryRowContext(ctx, `SELECT `+stewardRotationCols+` FROM steward_rotations WHERE task_id=? AND id=?`, task, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, api.ErrNotFound
	}
	return r, err
}

// openStewardRotation is the project's prepared rotation. A prepared rotation
// whose old steward has since closed (a project pause, an owner close) no
// longer holds anything open: admissions and prepares ignore it, and the
// next write path aborts it.
func openStewardRotation(ctx context.Context, q queryRower, task string) (*api.StewardRotation, error) {
	r, err := scanStewardRotation(q.QueryRowContext(ctx, `SELECT `+stewardRotationCols+` FROM steward_rotations WHERE task_id=? AND state='prepared'`, task))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var status string
	err = q.QueryRowContext(ctx, `SELECT status FROM agents WHERE id=?`, r.OldAgentID).Scan(&status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err != nil || status == api.AgentClosed {
		return nil, nil
	}
	return &r, nil
}

func (s *Store) GetStewardRotation(ctx context.Context, task, id string) (api.StewardRotation, error) {
	return loadStewardRotation(ctx, s.db, task, id)
}

func (s *Store) ListStewardRotations(ctx context.Context, task string) ([]api.StewardRotation, error) {
	if _, err := s.GetTask(ctx, task); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+stewardRotationCols+` FROM steward_rotations WHERE task_id=? ORDER BY created_at,id`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.StewardRotation{}
	for rows.Next() {
		r, err := scanStewardRotation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// stewardBusy applies the handler idle rules to the steward: the owner may
// treat an unobserved run as idle; the runner needs an observed idle state.
func stewardBusy(ctx context.Context, q queryRower, task string, a api.Agent, trigger string) (handlerBusy, *api.StewardRefusal, error) {
	b, err := loadHandlerBusy(ctx, q, task, a)
	if err != nil {
		return b, nil, err
	}
	if b.pendingTool != "" {
		return b, &api.StewardRefusal{Code: api.StewardRefusedPendingTool, Detail: "the steward run has a pending tool call: " + b.pendingTool}, nil
	}
	switch b.state {
	case "working", "hung_tool", "looping":
		return b, &api.StewardRefusal{Code: api.StewardRefusedWorking, Detail: "the steward run's activity is " + b.state}, nil
	case "idle", "finished_silent":
		return b, nil, nil
	}
	if trigger == api.StewardRotationTriggerRunner {
		return b, &api.StewardRefusal{Code: api.StewardRefusedWorking, Detail: "the runner needs an observed idle steward; activity is " + b.state}, nil
	}
	return b, nil, nil
}

func stewardRotationReceipt(ctx context.Context, tx *sql.Tx, task, requestID, operation, hash string) (string, bool, error) {
	var priorOperation, priorHash, rotation string
	err := tx.QueryRowContext(ctx, `SELECT operation,payload_hash,rotation_id FROM steward_rotation_requests WHERE task_id=? AND request_id=?`, task, requestID).Scan(&priorOperation, &priorHash, &rotation)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if priorOperation != operation || priorHash != hash {
		return "", false, workItemConflict("steward rotation request ID was already used with different input")
	}
	return rotation, true, nil
}

// abortStaleStewardRotation aborts a prepared rotation whose old steward has
// closed, closing its registered successor, so a fresh setup or rotation can
// proceed. It runs inside the caller's write transaction.
func (s *Store) abortStaleStewardRotation(ctx context.Context, tx *sql.Tx, task string, by api.Caller) error {
	r, err := scanStewardRotation(tx.QueryRowContext(ctx, `SELECT `+stewardRotationCols+` FROM steward_rotations WHERE task_id=? AND state='prepared'`, task))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if open, err := openStewardRotation(ctx, tx, task); err != nil || open != nil {
		return err
	}
	return s.abortStewardRotationTx(ctx, tx, task, r, "the old steward closed before commit", by)
}

func (s *Store) abortStewardRotationTx(ctx context.Context, tx *sql.Tx, task string, r api.StewardRotation, why string, by api.Caller) error {
	now := s.now()
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM agents WHERE task_id=? AND id=?`, task, r.SuccessorAgentID).Scan(&status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && status != api.AgentClosed {
		if _, err = tx.ExecContext(ctx, `UPDATE agents SET status=?,last_event_at=?,blocked_reason='',blocked_text='' WHERE id=?`, api.AgentClosed, ts(now), r.SuccessorAgentID); err != nil {
			return err
		}
		if _, err = s.insertEvent(ctx, tx, task, api.EventClosed, r.SuccessorAgentID, "", map[string]any{"stewardRotationId": r.ID, "aborted": true}, by); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE steward_rotations SET state=?,updated_at=? WHERE id=? AND state='prepared'`, api.StewardRotationAborted, ts(now), r.ID); err != nil {
		return err
	}
	_, err = s.insertEvent(ctx, tx, task, "task_updated", r.OldAgentID, "Steward rotation aborted", map[string]any{"stewardRotationId": r.ID, "state": api.StewardRotationAborted,
		"successorAgentId": r.SuccessorAgentID, "why": why}, by)
	return err
}

// StewardRotationAction runs a keyed prepare, commit or abort. Only the owner
// or the host runner acting for it may rotate; an agent identity is refused.
func (s *Store) StewardRotationAction(ctx context.Context, task string, req api.StewardRotationRequest, by api.Caller) (api.StewardRotation, error) {
	var zero api.StewardRotation
	if !api.ValidID(task, "tsk") || !validRequestID(req.RequestID) {
		return zero, api.ErrInvalid
	}
	if req.ActorAgentID != "" {
		return zero, &api.StewardRefusal{Code: api.StewardRefusedAgentCaller, Detail: "steward rotation is an owner or host-runner action; agent sessions cannot rotate the steward"}
	}
	hash := requestHash(req)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	if id, replay, err := stewardRotationReceipt(ctx, tx, task, req.RequestID, req.Operation, hash); err != nil {
		return zero, err
	} else if replay {
		return loadStewardRotation(ctx, tx, task, id)
	}
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, task))
	if errors.Is(err, sql.ErrNoRows) {
		return zero, api.ErrNotFound
	}
	if err != nil {
		return zero, err
	}
	if t.Status != api.TaskOpen {
		return zero, api.ErrClosed
	}
	var out api.StewardRotation
	switch req.Operation {
	case api.StewardRotationPrepare:
		out, err = s.prepareStewardRotation(ctx, tx, t, req, hash, by)
	case api.StewardRotationCommit:
		out, err = s.commitStewardRotation(ctx, tx, t, req, by)
	case api.StewardRotationAbort:
		out, err = s.abortStewardRotation(ctx, tx, t, req, by)
	default:
		return zero, api.ErrInvalid
	}
	if err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO steward_rotation_requests(task_id,request_id,operation,payload_hash,rotation_id) VALUES(?,?,?,?,?)`, task, req.RequestID, req.Operation, hash, out.ID); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return out, nil
}

func (s *Store) prepareStewardRotation(ctx context.Context, tx *sql.Tx, t api.Task, req api.StewardRotationRequest, hash string, by api.Caller) (api.StewardRotation, error) {
	var zero api.StewardRotation
	switch req.Reason {
	case api.StewardRotationReasonManual, api.StewardRotationReasonTokens, api.StewardRotationReasonTemplate:
	default:
		return zero, api.ErrInvalid
	}
	if (req.Trigger != api.StewardRotationTriggerOwner && req.Trigger != api.StewardRotationTriggerRunner) || req.RotationID != "" ||
		!api.ValidID(req.OldAgentID, "agt") || !validRunID(req.OldRunID) || !api.ValidID(req.SuccessorAgentID, "agt") || !api.ValidName(req.SuccessorName) {
		return zero, api.ErrInvalid
	}
	if t.PauseState != api.ProjectPauseActive {
		return zero, &api.StewardRefusal{Code: api.StewardRefusedPaused, Detail: "the project is paused"}
	}
	if err := s.abortStaleStewardRotation(ctx, tx, t.ID, by); err != nil {
		return zero, err
	}
	if open, err := openStewardRotation(ctx, tx, t.ID); err != nil {
		return zero, err
	} else if open != nil {
		return zero, &api.StewardRefusal{Code: api.StewardRefusedRotationOpen, Detail: "steward rotation " + open.ID + " is still prepared; finish or abort it first"}
	}
	old, ok, err := activeSteward(ctx, tx, t.ID)
	if err != nil {
		return zero, err
	}
	if !ok || old.ID != req.OldAgentID || old.RunID != req.OldRunID {
		return zero, &api.StewardRefusal{Code: api.StewardRefusedNotSteward, Detail: "the named run is not the project's active backlog steward"}
	}
	if want := api.StewardSuccessorName(old.Name); req.SuccessorName != want {
		return zero, &api.StewardRefusal{Code: api.StewardRefusedNameTaken, Detail: "the successor must be named " + want}
	}
	var taken int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE (task_id=? AND name=? COLLATE NOCASE AND status<>?) OR id=?`, t.ID, req.SuccessorName, api.AgentClosed, req.SuccessorAgentID).Scan(&taken); err != nil {
		return zero, err
	}
	if taken != 0 {
		return zero, &api.StewardRefusal{Code: api.StewardRefusedNameTaken, Detail: "the successor name or agent ID is already in use"}
	}
	if _, refusal, err := stewardBusy(ctx, tx, t.ID, old, req.Trigger); err != nil {
		return zero, err
	} else if refusal != nil {
		return zero, refusal
	}
	now := s.now()
	r := api.StewardRotation{ID: api.NewID("srot"), TaskID: t.ID, RequestID: req.RequestID, State: api.StewardRotationPrepared, Reason: req.Reason, Trigger: req.Trigger,
		OldAgentID: old.ID, OldRunID: old.RunID, OldName: old.Name, SuccessorAgentID: req.SuccessorAgentID, SuccessorName: req.SuccessorName, CreatedAt: now, UpdatedAt: now}
	err = tx.QueryRowContext(ctx, `SELECT revision,digest FROM backlog_summaries WHERE task_id=? ORDER BY revision DESC LIMIT 1`, t.ID).Scan(&r.SummaryRevision, &r.SummaryDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, &api.StewardRefusal{Code: api.StewardRefusedSummaryMissing, Detail: "the steward has saved no backlog summary; a rotation hands over the summary, not chat history"}
	}
	if err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO steward_rotations(id,task_id,request_id,payload_hash,state,reason,trigger_kind,old_agent_id,old_run_id,old_name,successor_agent_id,successor_name,summary_revision,summary_digest,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.TaskID, r.RequestID, hash, r.State, r.Reason, r.Trigger, r.OldAgentID, r.OldRunID, r.OldName,
		r.SuccessorAgentID, r.SuccessorName, r.SummaryRevision, r.SummaryDigest, ts(now), ts(now)); err != nil {
		return zero, err
	}
	if _, err = s.insertEvent(ctx, tx, t.ID, "task_updated", old.ID, "Steward rotation prepared", map[string]any{"stewardRotationId": r.ID, "state": r.State, "reason": r.Reason,
		"trigger": r.Trigger, "oldAgentId": r.OldAgentID, "oldRunId": r.OldRunID, "successorAgentId": r.SuccessorAgentID, "successorName": r.SuccessorName, "summaryRevision": r.SummaryRevision}, by); err != nil {
		return zero, err
	}
	return r, nil
}

func (s *Store) commitStewardRotation(ctx context.Context, tx *sql.Tx, t api.Task, req api.StewardRotationRequest, by api.Caller) (api.StewardRotation, error) {
	var zero api.StewardRotation
	if req.RotationID == "" {
		return zero, api.ErrInvalid
	}
	r, err := loadStewardRotation(ctx, tx, t.ID, req.RotationID)
	if err != nil {
		return zero, err
	}
	if r.State == api.StewardRotationCommitted {
		return r, nil
	}
	if r.State != api.StewardRotationPrepared {
		return zero, workItemConflict("steward rotation " + r.ID + " was aborted")
	}
	if t.PauseState != api.ProjectPauseActive {
		return zero, &api.StewardRefusal{Code: api.StewardRefusedPaused, Detail: "the project is paused"}
	}
	old, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=?`, t.ID, r.OldAgentID))
	if err != nil {
		return zero, err
	}
	if old.RunID != r.OldRunID || old.Status == api.AgentClosed || old.Status == api.AgentExited {
		return zero, &api.StewardRefusal{Code: api.StewardRefusedNotSteward, Detail: "the old steward run changed since prepare; abort this rotation"}
	}
	if _, refusal, err := stewardBusy(ctx, tx, t.ID, old, r.Trigger); err != nil {
		return zero, err
	} else if refusal != nil {
		return zero, refusal
	}
	successor, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=?`, t.ID, r.SuccessorAgentID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	var pending int
	if err == nil {
		if err = tx.QueryRowContext(ctx, `SELECT steward_pending FROM agents WHERE id=?`, successor.ID).Scan(&pending); err != nil {
			return zero, err
		}
	}
	if successor.ID == "" || successor.Role != api.AgentRoleBacklogSteward || successor.Name != r.SuccessorName || !successor.Online || pending != 1 ||
		successor.Status == api.AgentRetired || successor.Host != old.Host || successor.Runtime != old.Runtime || successor.Cwd != old.Cwd {
		return zero, &api.StewardRefusal{Code: api.StewardRefusedSuccessor, Detail: "the successor must be a registered, online, pending backlog steward on the old steward's host, runtime and directory"}
	}
	handoff := api.StewardRotationHandoff{SummaryRevision: r.SummaryRevision, SummaryDigest: r.SummaryDigest, Reissued: []api.HandlerRotationReissue{}, OpenDecisions: []api.StewardRotationDecision{}}
	var savedBy, savedRun string
	if err = tx.QueryRowContext(ctx, `SELECT agent_id,run_id FROM backlog_summaries WHERE task_id=? AND revision=?`, t.ID, r.SummaryRevision).Scan(&savedBy, &savedRun); err != nil {
		return zero, err
	}
	handoff.SummaryFromOldRun = savedBy == old.ID && savedRun == old.RunID
	rows, err := tx.QueryContext(ctx, `SELECT d.message_seq,d.question FROM decision_requests d JOIN messages m ON m.seq=d.message_seq
 WHERE d.task_id=? AND m.from_agent=? AND NOT EXISTS (SELECT 1 FROM decision_answers a WHERE a.task_id=d.task_id AND a.request_seq=d.message_seq) ORDER BY d.message_seq`, t.ID, old.ID)
	if err != nil {
		return zero, err
	}
	for rows.Next() {
		var d api.StewardRotationDecision
		if err = rows.Scan(&d.MessageSeq, &d.Question); err != nil {
			rows.Close()
			return zero, err
		}
		handoff.OpenDecisions = append(handoff.OpenDecisions, d)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return zero, err
	}
	// Move every open obligation the old steward holds, by role or by name,
	// before it closes: a closed recipient's obligations would otherwise be
	// closed recipient_gone by the broker.
	orows, err := tx.QueryContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE task_id=? AND agent_id=? AND state<>? ORDER BY message_seq,id`, t.ID, old.ID, api.ObligationClosed)
	if err != nil {
		return zero, err
	}
	var open []api.Obligation
	for orows.Next() {
		o, err := scanObligation(orows)
		if err != nil {
			orows.Close()
			return zero, err
		}
		open = append(open, o)
	}
	orows.Close()
	if err = orows.Err(); err != nil {
		return zero, err
	}
	// Close the old steward first, then make the successor the holder: the
	// one-steward index never sees two holders.
	now := s.now()
	if _, err = tx.ExecContext(ctx, `UPDATE agents SET status=?,last_event_at=?,blocked_reason='',blocked_text='' WHERE id=?`, api.AgentClosed, ts(now), old.ID); err != nil {
		return zero, err
	}
	if _, err = s.insertEvent(ctx, tx, t.ID, api.EventClosed, old.ID, "", map[string]any{"stewardRotationId": r.ID, "successorAgentId": successor.ID}, by); err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agents SET steward_pending=0 WHERE id=? AND steward_pending=1`, successor.ID); err != nil {
		return zero, stewardUniqueViolation(ctx, tx, t.ID, err)
	}
	for _, o := range open {
		var viaRole string
		if err = tx.QueryRowContext(ctx, `SELECT via_role FROM obligations WHERE id=?`, o.ID).Scan(&viaRole); err != nil {
			return zero, err
		}
		m, err := s.reissueObligation(ctx, tx, t, o, successor, "steward rotation", "rotation "+r.ID+" moved "+old.Name+" to "+successor.Name, true)
		if err != nil {
			return zero, err
		}
		pair := api.HandlerRotationReissue{OldObligationID: o.ID, OldMessageSeq: o.MessageSeq, OldState: o.State, NewMessageSeq: m.Seq, ViaRole: viaRole, Subject: o.Subject}
		if err = tx.QueryRowContext(ctx, `SELECT id FROM obligations WHERE message_seq=? AND agent_id=?`, m.Seq, successor.ID).Scan(&pair.NewObligationID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return zero, err
		}
		handoff.Reissued = append(handoff.Reissued, pair)
	}
	text := fmt.Sprintf("You are now this project's backlog steward, replacing %s (%s / %s) by steward rotation %s. %d open obligation(s) moved to you. Read backlog summary revision %d first (tt steward summary get --revision %d), then the handoff (tt steward rotation get %s): moved obligations and the decisions %s proposed that are still unanswered. Continue from those records; do not rely on chat history.",
		old.Name, old.ID, old.RunID, r.ID, len(handoff.Reissued), r.SummaryRevision, r.SummaryRevision, r.ID, old.Name)
	if err = s.postBrokerNotice(ctx, tx, t, successor, successor.Name, "Steward rotation handoff to "+successor.Name, "Steward rotation handoff", text, map[string]string{"stewardRotation": r.ID}); err != nil {
		return zero, err
	}
	var noticeSeq int64
	if err = tx.QueryRowContext(ctx, `SELECT max(seq) FROM messages WHERE task_id=? AND to_agent=?`, t.ID, successor.ID).Scan(&noticeSeq); err != nil {
		return zero, err
	}
	r.State, r.SuccessorRunID, r.UpdatedAt, r.Handoff = api.StewardRotationCommitted, successor.RunID, now, &handoff
	r.Receipt = &api.StewardRotationReceipt{RotationID: r.ID, RequestID: req.RequestID, StewardAgentID: successor.ID, StewardRunID: successor.RunID, ClosedAgentID: old.ID, ClosedRunID: old.RunID,
		SummaryRevision: r.SummaryRevision, NoticeSeq: noticeSeq, Reissued: len(handoff.Reissued), CommittedAt: now}
	handoffJSON, _ := json.Marshal(handoff)
	receiptJSON, _ := json.Marshal(r.Receipt)
	if _, err = tx.ExecContext(ctx, `UPDATE steward_rotations SET state=?,successor_run_id=?,handoff_json=?,receipt_json=?,updated_at=? WHERE id=? AND state='prepared'`,
		r.State, r.SuccessorRunID, string(handoffJSON), string(receiptJSON), ts(now), r.ID); err != nil {
		return zero, err
	}
	if _, err = s.insertEvent(ctx, tx, t.ID, "task_updated", successor.ID, "Steward rotation committed", map[string]any{"stewardRotationId": r.ID, "state": r.State,
		"oldAgentId": old.ID, "oldRunId": old.RunID, "successorAgentId": successor.ID, "successorRunId": successor.RunID, "summaryRevision": r.SummaryRevision,
		"reissued": len(handoff.Reissued), "noticeSeq": noticeSeq}, by); err != nil {
		return zero, err
	}
	return r, nil
}

func (s *Store) abortStewardRotation(ctx context.Context, tx *sql.Tx, t api.Task, req api.StewardRotationRequest, by api.Caller) (api.StewardRotation, error) {
	var zero api.StewardRotation
	if req.RotationID == "" {
		return zero, api.ErrInvalid
	}
	r, err := loadStewardRotation(ctx, tx, t.ID, req.RotationID)
	if err != nil {
		return zero, err
	}
	if r.State == api.StewardRotationAborted {
		return r, nil
	}
	if r.State != api.StewardRotationPrepared {
		return zero, workItemConflict("steward rotation " + r.ID + " is committed and cannot be aborted")
	}
	if err = s.abortStewardRotationTx(ctx, tx, t.ID, r, "aborted by the owner or host runner", by); err != nil {
		return zero, err
	}
	return loadStewardRotation(ctx, tx, t.ID, r.ID)
}

// StewardRotationsDue lists, for each open project whose steward policy is
// enabled and whose active steward runs on host, the counters the runner
// compares with the policy. templateDigest is the runner's current template.
func (s *Store) StewardRotationsDue(ctx context.Context, host, templateDigest string) (api.StewardRotationDueList, error) {
	out := api.StewardRotationDueList{Entries: []api.StewardRotationDue{}}
	if host == "" || !api.ValidText(host, 253) || len(templateDigest) > 64 {
		return out, api.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE status=? AND pause_state=? AND EXISTS (SELECT 1 FROM agents a WHERE a.task_id=tasks.id AND a.role=? AND a.host=? AND a.status NOT IN (?,?)) ORDER BY created_at,id`,
		api.TaskOpen, api.ProjectPauseActive, api.AgentRoleBacklogSteward, host, api.AgentClosed, api.AgentExited)
	if err != nil {
		return out, err
	}
	var tasks []api.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		tasks = append(tasks, t)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return out, err
	}
	for _, t := range tasks {
		policy, err := loadStewardRotationPolicy(ctx, s.db, t.ID)
		if err != nil {
			return out, err
		}
		if !policy.Enabled {
			continue
		}
		steward, ok, err := activeSteward(ctx, s.db, t.ID)
		if err != nil {
			return out, err
		}
		if !ok || steward.Host != host {
			continue
		}
		d := api.StewardRotationDue{TaskID: t.ID, Agent: steward, Policy: policy, DueReasons: []string{}}
		busy, refusal, err := stewardBusy(ctx, s.db, t.ID, steward, api.StewardRotationTriggerRunner)
		if err != nil {
			return out, err
		}
		d.ActivityState, d.PendingTool, d.Idle = busy.state, busy.pendingTool, refusal == nil
		if err = s.loadActivity(ctx, &d.Agent); err != nil {
			return out, err
		}
		if d.Agent.Activity != nil {
			d.TotalTokens = d.Agent.Activity.Tokens.Total
		}
		err = s.db.QueryRowContext(ctx, `SELECT template_digest FROM steward_runs WHERE run_id=? AND agent_id=?`, steward.RunID, steward.ID).Scan(&d.RecordedDigest)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM backlog_summaries WHERE task_id=?`, t.ID).Scan(&d.SummaryRevision)
		if err != nil {
			return out, err
		}
		d.DigestMatches = templateDigest != "" && d.RecordedDigest == templateDigest
		if policy.MaxTotalTokens > 0 && d.TotalTokens >= policy.MaxTotalTokens {
			d.DueReasons = append(d.DueReasons, api.StewardRotationReasonTokens)
		}
		if policy.OnTemplateChange && templateDigest != "" && !d.DigestMatches {
			d.DueReasons = append(d.DueReasons, api.StewardRotationReasonTemplate)
		}
		if d.OpenRotation, err = openStewardRotation(ctx, s.db, t.ID); err != nil {
			return out, err
		}
		out.Entries = append(out.Entries, d)
	}
	return out, nil
}
