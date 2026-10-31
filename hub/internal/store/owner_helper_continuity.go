package store

// The owner helper's continuity record (docs/session-handoff.md, "The
// continuity record on the hub"): one append-only list of revisions per
// project that holds the free-text continuity log the owner helper keeps
// between sessions. Any identified caller may read it, from any host. Only
// the owner, or the exact run of the project's current owner helper, may
// save a revision. The host `tt handoff` record stays on its host and stores
// no words; this record is the continuity source.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
)

// migrateOwnerHelperContinuity is additive only: one new table, no ALTER and
// no backfill, so it runs again on any store without change.
func migrateOwnerHelperContinuity(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS owner_helper_continuity (
 task_id TEXT NOT NULL REFERENCES tasks(id), revision INTEGER NOT NULL, body TEXT NOT NULL, digest TEXT NOT NULL,
 agent_id TEXT NOT NULL, run_id TEXT NOT NULL, request_id TEXT NOT NULL, payload_hash TEXT NOT NULL,
 created_node TEXT NOT NULL, created_user TEXT NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY(task_id,revision), UNIQUE(task_id,request_id))`)
	return err
}

const ownerHelperContinuityCols = `task_id,revision,body,digest,agent_id,run_id,request_id,created_node,created_user,created_at`

func scanOwnerHelperContinuity(row interface{ Scan(...any) error }) (api.OwnerHelperContinuity, error) {
	var c api.OwnerHelperContinuity
	var created string
	err := row.Scan(&c.TaskID, &c.Revision, &c.Body, &c.Digest, &c.AgentID, &c.RunID, &c.RequestID, &c.CreatedBy.Node, &c.CreatedBy.User, &created)
	c.CreatedAt, c.Bytes = parseTS(created), len(c.Body)
	return c, err
}

// OwnerHelperContinuity reads the latest revision, or rev when rev > 0. Any
// caller may read it.
func (s *Store) OwnerHelperContinuity(ctx context.Context, task string, rev int64) (api.OwnerHelperContinuity, error) {
	if !api.ValidID(task, "tsk") || rev < 0 {
		return api.OwnerHelperContinuity{}, api.ErrInvalid
	}
	if _, err := s.GetTask(ctx, task); err != nil {
		return api.OwnerHelperContinuity{}, err
	}
	var row *sql.Row
	if rev > 0 {
		row = s.db.QueryRowContext(ctx, `SELECT `+ownerHelperContinuityCols+` FROM owner_helper_continuity WHERE task_id=? AND revision=?`, task, rev)
	} else {
		row = s.db.QueryRowContext(ctx, `SELECT `+ownerHelperContinuityCols+` FROM owner_helper_continuity WHERE task_id=? ORDER BY revision DESC LIMIT 1`, task)
	}
	c, err := scanOwnerHelperContinuity(row)
	if errors.Is(err, sql.ErrNoRows) {
		return c, api.ErrNotFound
	}
	return c, err
}

// OwnerHelperContinuityRevisions lists every revision, oldest first, without
// bodies.
func (s *Store) OwnerHelperContinuityRevisions(ctx context.Context, task string) ([]api.OwnerHelperContinuity, error) {
	if !api.ValidID(task, "tsk") {
		return nil, api.ErrInvalid
	}
	if _, err := s.GetTask(ctx, task); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+ownerHelperContinuityCols+` FROM owner_helper_continuity WHERE task_id=? ORDER BY revision`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.OwnerHelperContinuity{}
	for rows.Next() {
		c, err := scanOwnerHelperContinuity(rows)
		if err != nil {
			return nil, err
		}
		c.Body = ""
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveOwnerHelperContinuity appends the next revision. The owner (no agent
// and no run) or the current owner helper's exact run may save; a stale
// expected revision is refused, and a replayed request ID returns the saved
// row. A paused project is not refused, so continuity can be written during
// a pause; a closed project is.
func (s *Store) SaveOwnerHelperContinuity(ctx context.Context, task string, req api.SaveOwnerHelperContinuityRequest, by api.Caller) (api.OwnerHelperContinuity, error) {
	var zero api.OwnerHelperContinuity
	if !api.ValidID(task, "tsk") || !validRequestID(req.RequestID) || req.ExpectedRevision < 0 || strings.TrimSpace(req.Body) == "" ||
		len(req.Body) > api.MaxOwnerHelperContinuityLen || !utf8.ValidString(req.Body) || strings.ContainsRune(req.Body, 0) {
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
	prior, err := scanOwnerHelperContinuity(tx.QueryRowContext(ctx, `SELECT `+ownerHelperContinuityCols+` FROM owner_helper_continuity WHERE task_id=? AND request_id=?`, task, req.RequestID))
	if err == nil {
		var priorHash string
		if err = tx.QueryRowContext(ctx, `SELECT payload_hash FROM owner_helper_continuity WHERE task_id=? AND request_id=?`, task, req.RequestID).Scan(&priorHash); err != nil {
			return zero, err
		}
		if priorHash != hash {
			return zero, workItemConflict("owner helper continuity request ID was already used with different input")
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
		if err = requireCurrentOwnerHelperRun(ctx, tx, task, req.AgentID, req.RunID); err != nil {
			return zero, err
		}
	}
	var current int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM owner_helper_continuity WHERE task_id=?`, task).Scan(&current); err != nil {
		return zero, err
	}
	if current != req.ExpectedRevision {
		return zero, workItemConflict(fmt.Sprintf("the owner helper continuity record is at revision %d; read it and save against that revision", current))
	}
	now := s.now()
	out := api.OwnerHelperContinuity{TaskID: task, Revision: current + 1, Body: req.Body, Digest: summaryDigest(req.Body), Bytes: len(req.Body), AgentID: req.AgentID, RunID: req.RunID,
		RequestID: req.RequestID, CreatedBy: by, CreatedAt: now}
	if _, err = tx.ExecContext(ctx, `INSERT INTO owner_helper_continuity(`+ownerHelperContinuityCols+`,payload_hash) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		out.TaskID, out.Revision, out.Body, out.Digest, out.AgentID, out.RunID, out.RequestID, by.Node, by.User, ts(now), hash); err != nil {
		return zero, err
	}
	if _, err = s.insertEvent(ctx, tx, task, "task_updated", req.AgentID, "Owner helper continuity saved", map[string]any{"ownerHelperContinuityRevision": out.Revision, "digest": out.Digest, "bytes": out.Bytes}, by); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return out, nil
}

// requireCurrentOwnerHelperRun admits only the exact current run of the
// project's owner helper, and only while that helper is not exited, closed
// or retired. A run replaced by a later registration is refused.
func requireCurrentOwnerHelperRun(ctx context.Context, tx *sql.Tx, task, agentID, runID string) error {
	const refused = "only the owner or the current owner helper's exact run may save the continuity record"
	if agentID == "" || runID == "" {
		return fmt.Errorf("%w: %s; give both the agent and its run", api.ErrConflict, refused)
	}
	helper, found, err := currentOwnerHelper(ctx, tx, task)
	if err != nil {
		return err
	}
	switch {
	case !found || helper.ID != agentID:
		return fmt.Errorf("%w: %s; %s is not the project's owner helper", api.ErrConflict, refused, agentID)
	case helper.RunID != runID:
		return fmt.Errorf("%w: %s; run %s was replaced, so register this session again (tt helper register)", api.ErrConflict, refused, runID)
	case helper.Status == api.AgentExited || helper.Status == api.AgentClosed || helper.Status == api.AgentRetired:
		return fmt.Errorf("%w: %s; the owner helper is %s", api.ErrConflict, refused, helper.Status)
	}
	return nil
}
