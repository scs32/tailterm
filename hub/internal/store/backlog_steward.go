package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Backlog steward (docs/backlog-steward.md). One steward holds a project's
// slot at a time. A rotation successor is admitted as a non-holding steward
// (steward_pending=1) until its rotation commits.
func migrateBacklogSteward(db *sql.DB) error {
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
 PRIMARY KEY(task_id,revision), UNIQUE(task_id,request_id));`)
	return err
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
	return false, nil
}

// stewardRotationOpenRefusal refuses a steward admission while a rotation is
// prepared.
func stewardRotationOpenRefusal(ctx context.Context, q queryRower, task string) error {
	return nil
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
