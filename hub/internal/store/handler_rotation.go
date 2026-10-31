package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Handler rotation (docs/handler-rotation.md). The project's primary handler
// is explicit once a rotation commits; before that the legacy rules apply.
func migrateHandlerRotation(db *sql.DB) error {
	var existed int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='handler_rotation_policy'`).Scan(&existed); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS handler_rotation_policy (
 task_id TEXT PRIMARY KEY REFERENCES tasks(id), enabled INTEGER NOT NULL, max_items INTEGER NOT NULL,
 max_total_tokens INTEGER NOT NULL, on_template_change INTEGER NOT NULL, revision INTEGER NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS handler_rotations (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), request_id TEXT NOT NULL, payload_hash TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('prepared','committed','aborted')), reason TEXT NOT NULL, trigger_kind TEXT NOT NULL,
 handler_revision INTEGER NOT NULL, old_agent_id TEXT NOT NULL, old_run_id TEXT NOT NULL, old_name TEXT NOT NULL,
 successor_agent_id TEXT NOT NULL, successor_name TEXT NOT NULL, successor_run_id TEXT NOT NULL DEFAULT '',
 handoff_json TEXT NOT NULL DEFAULT '', receipt_json TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 UNIQUE(task_id,request_id));
CREATE UNIQUE INDEX IF NOT EXISTS handler_rotations_open ON handler_rotations(task_id) WHERE state='prepared';
CREATE INDEX IF NOT EXISTS handler_rotations_old ON handler_rotations(old_agent_id,state);
CREATE TABLE IF NOT EXISTS handler_rotation_requests (
 task_id TEXT NOT NULL, request_id TEXT NOT NULL, operation TEXT NOT NULL, payload_hash TEXT NOT NULL,
 rotation_id TEXT NOT NULL, PRIMARY KEY(task_id,request_id));
CREATE TABLE IF NOT EXISTS handler_runs (
 task_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT PRIMARY KEY, template_digest TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS handler_successor_holds (
 task_id TEXT PRIMARY KEY REFERENCES tasks(id), reason TEXT NOT NULL CHECK(reason IN ('template','manual')), trigger_kind TEXT NOT NULL,
 old_agent_id TEXT NOT NULL, old_run_id TEXT NOT NULL, handler_revision INTEGER NOT NULL, created_at TEXT NOT NULL);`); err != nil {
		return err
	}
	// An owner-authorized change of runtime, model or arm (wi_f250a91c85367e6f).
	for _, column := range []string{"authorized_by", "authorization_reason", "old_runtime", "successor_runtime"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('handler_rotations') WHERE name=?`, column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec("ALTER TABLE handler_rotations ADD COLUMN " + column + " TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
	}
	// A dead primary (wi_b863e669073858f4, owner order #28057): the silence
	// applies to every project, existing and new, and the rotation keeps the
	// host's evidence and the old run's last recorded activity.
	for _, column := range []struct{ table, name, kind string }{
		{"handler_rotation_policy", "dead_silence_minutes", fmt.Sprintf("INTEGER NOT NULL DEFAULT %d", api.DefaultHandlerDeadSilenceMinutes)},
		{"handler_rotations", "evidence_json", "TEXT NOT NULL DEFAULT ''"},
		{"handler_rotations", "commit_evidence_json", "TEXT NOT NULL DEFAULT ''"},
		{"handler_rotations", "last_activity_at", "TEXT NOT NULL DEFAULT ''"},
		// An exited primary (wi_1d987e7f296b6a2e, owner order #28426): the
		// wrapper's exit report the rotation was prepared on.
		{"handler_rotations", "exit_json", "TEXT NOT NULL DEFAULT ''"},
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('`+column.table+`') WHERE name=?`, column.name).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec("ALTER TABLE " + column.table + " ADD COLUMN " + column.name + " " + column.kind); err != nil {
				return err
			}
		}
	}
	if existed == 0 {
		// Owner decision #14233: rotation is on by default for new projects; the
		// owner turns it on for projects that existed before this migration.
		if _, err := db.Exec(`INSERT OR IGNORE INTO handler_rotation_policy(task_id,enabled,max_items,max_total_tokens,on_template_change,revision,updated_at)
 SELECT id,0,?,?,1,1,? FROM tasks`, api.DefaultHandlerRotationMaxItems, api.DefaultHandlerRotationMaxTotalTokens, ts(time.Now().UTC())); err != nil {
			return err
		}
	}
	return nil
}

func defaultHandlerRotationPolicy(task string) api.HandlerRotationPolicy {
	return api.HandlerRotationPolicy{TaskID: task, Enabled: true, MaxItems: api.DefaultHandlerRotationMaxItems,
		MaxTotalTokens: api.DefaultHandlerRotationMaxTotalTokens, OnTemplateChange: true, DeadSilenceMinutes: api.DefaultHandlerDeadSilenceMinutes}
}

func loadHandlerRotationPolicy(ctx context.Context, q queryRower, task string) (api.HandlerRotationPolicy, error) {
	p := defaultHandlerRotationPolicy(task)
	var updated string
	err := q.QueryRowContext(ctx, `SELECT enabled,max_items,max_total_tokens,on_template_change,revision,updated_at,dead_silence_minutes FROM handler_rotation_policy WHERE task_id=?`, task).
		Scan(&p.Enabled, &p.MaxItems, &p.MaxTotalTokens, &p.OnTemplateChange, &p.Revision, &updated, &p.DeadSilenceMinutes)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	t := parseTS(updated)
	p.UpdatedAt = &t
	return p, nil
}

func (s *Store) HandlerRotationPolicy(ctx context.Context, task string) (api.HandlerRotationPolicy, error) {
	if _, err := s.GetTask(ctx, task); err != nil {
		return api.HandlerRotationPolicy{}, err
	}
	return loadHandlerRotationPolicy(ctx, s.db, task)
}

func (s *Store) SetHandlerRotationPolicy(ctx context.Context, task string, req api.HandlerRotationPolicyRequest) (api.HandlerRotationPolicy, error) {
	var zero api.HandlerRotationPolicy
	if req.ActorAgentID != "" {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedAgentCaller, Detail: "the rotation policy is the owner's; agent sessions cannot change it"}
	}
	if req.ExpectedRevision < 0 || req.MaxItems < 0 || req.MaxTotalTokens < 0 ||
		(req.DeadSilenceMinutes != nil && (*req.DeadSilenceMinutes < 0 || *req.DeadSilenceMinutes > api.MaxHandlerDeadSilenceMinutes)) {
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
	current, err := loadHandlerRotationPolicy(ctx, tx, task)
	if err != nil {
		return zero, err
	}
	if current.Revision != req.ExpectedRevision {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedStalePolicy, Detail: fmt.Sprintf("policy is at revision %d; reload before changing it", current.Revision)}
	}
	now := s.now()
	// An omitted silence keeps the saved one, so a caller that does not know
	// the field cannot turn the dead primary replacement off or on.
	silence := current.DeadSilenceMinutes
	if req.DeadSilenceMinutes != nil {
		silence = *req.DeadSilenceMinutes
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO handler_rotation_policy(task_id,enabled,max_items,max_total_tokens,on_template_change,revision,updated_at,dead_silence_minutes) VALUES(?,?,?,?,?,?,?,?)
 ON CONFLICT(task_id) DO UPDATE SET enabled=excluded.enabled,max_items=excluded.max_items,max_total_tokens=excluded.max_total_tokens,on_template_change=excluded.on_template_change,revision=excluded.revision,updated_at=excluded.updated_at,dead_silence_minutes=excluded.dead_silence_minutes`,
		task, req.Enabled, req.MaxItems, req.MaxTotalTokens, req.OnTemplateChange, current.Revision+1, ts(now), silence); err != nil {
		return zero, err
	}
	out := api.HandlerRotationPolicy{TaskID: task, Enabled: req.Enabled, MaxItems: req.MaxItems, MaxTotalTokens: req.MaxTotalTokens, OnTemplateChange: req.OnTemplateChange, DeadSilenceMinutes: silence, Revision: current.Revision + 1, UpdatedAt: &now}
	if _, err = s.insertEvent(ctx, tx, task, "task_updated", "", "Handler rotation policy saved", map[string]any{"handlerRotationPolicy": out}, api.Caller{Node: "workspace", User: "owner"}); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return out, nil
}

// primaryHandler is the project's primary database handler: the explicit
// primary when it is still open, otherwise the legacy oldest open handler.
// A successor of an open rotation is never the legacy primary.
func primaryHandler(ctx context.Context, q queryRower, task api.Task) (api.Agent, error) {
	if task.PrimaryHandlerID != "" {
		a, err := scanAgent(q.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=? AND role=? AND status NOT IN (?,?)`, task.ID, task.PrimaryHandlerID, api.AgentRoleDatabaseHandler, api.AgentClosed, api.AgentExited))
		if err == nil || !errors.Is(err, sql.ErrNoRows) {
			return a, err
		}
	}
	return oldestHandler(ctx, q, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status NOT IN (?,?)
 AND id NOT IN (SELECT successor_agent_id FROM handler_rotations WHERE task_id=? AND state='prepared')`,
		task.ID, api.AgentRoleDatabaseHandler, api.AgentClosed, api.AgentExited, task.ID)
}

// oldestHandler is the earliest-created handler the query returns, by parsed
// creation time and then id. Stored times drop trailing zeros, so as text a
// handler created on a whole millisecond sorts after a later one in that
// millisecond (FINDING #32673). The parsed time survives a VACUUM, which may
// renumber the rowid of agents.
func oldestHandler(ctx context.Context, q queryRower, query string, args ...any) (api.Agent, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return api.Agent{}, err
	}
	defer rows.Close()
	oldest, found := api.Agent{}, false
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return api.Agent{}, err
		}
		if !found || a.CreatedAt.Before(oldest.CreatedAt) || a.CreatedAt.Equal(oldest.CreatedAt) && a.ID < oldest.ID {
			oldest, found = a, true
		}
	}
	if err = rows.Err(); err != nil {
		return api.Agent{}, err
	}
	if !found {
		return api.Agent{}, sql.ErrNoRows
	}
	return oldest, nil
}

// exitedPrimaryHandler is the project's primary with the exited exclusion
// lifted: the explicit primary when it is not closed, otherwise the oldest
// handler that is not closed and not a prepared successor. ok is true only
// when that agent's wrapper reported exited. Only a dead_primary prepare and
// the due list use it; every other rotation keeps primaryHandler.
func exitedPrimaryHandler(ctx context.Context, q queryRower, task api.Task) (api.Agent, bool, error) {
	var a api.Agent
	err := sql.ErrNoRows
	if task.PrimaryHandlerID != "" {
		a, err = scanAgent(q.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=? AND role=? AND status<>?`, task.ID, task.PrimaryHandlerID, api.AgentRoleDatabaseHandler, api.AgentClosed))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return a, false, err
		}
	}
	if err != nil {
		a, err = oldestHandler(ctx, q, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status<>?
 AND id NOT IN (SELECT successor_agent_id FROM handler_rotations WHERE task_id=? AND state='prepared')`,
			task.ID, api.AgentRoleDatabaseHandler, api.AgentClosed, task.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return api.Agent{}, false, nil
		}
		if err != nil {
			return a, false, err
		}
	}
	return a, a.Status == api.AgentExited, nil
}

// wrapperExitText is the text tt wrap posts with its exited event.
var wrapperExitText = regexp.MustCompile(`^Process exited \((-?[0-9]{1,9})\)$`)

// wrapperExitReport is the exit an exited handler's own wrapper reported:
// the agent's newest started, heartbeat or exited event must be an exited
// event with the wrapper's text. A restart changes the run and the status, so
// together with the caller's exact-run check the event belongs to this run.
// A missing or pruned event, a later start or heartbeat, and an exit posted by
// hand are all refused: none of them is the wrapper's report.
func wrapperExitReport(ctx context.Context, q queryRower, task string, a api.Agent) (*api.HandlerRotationExit, error) {
	var seq int64
	var kind, text, created string
	err := q.QueryRowContext(ctx, `SELECT seq,kind,text,created_at FROM events WHERE task_id=? AND agent_id=? AND kind IN (?,?,?) ORDER BY seq DESC LIMIT 1`,
		task, a.ID, api.EventStarted, api.EventHeartbeat, api.EventExited).Scan(&seq, &kind, &text, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, deathUnconfirmed("the hub no longer has the exit event of the handler's wrapper")
	}
	if err != nil {
		return nil, err
	}
	if kind != api.EventExited {
		return nil, deathUnconfirmed("the handler's newest wrapper event is %s, not its exit", kind)
	}
	m := wrapperExitText.FindStringSubmatch(text)
	if m == nil {
		return nil, deathUnconfirmed("the handler's exit event was not reported by its wrapper")
	}
	code, _ := strconv.Atoi(m[1])
	return &api.HandlerRotationExit{Code: code, ReportedAt: parseTS(created), EventSeq: seq}, nil
}

// liveSuccessor follows committed rotations from a closed handler to the
// open end of its chain. It returns "" when the agent was not rotated.
func liveSuccessor(ctx context.Context, q queryRower, agentID string) (string, error) {
	current := agentID
	for range 64 {
		var next string
		err := q.QueryRowContext(ctx, `SELECT successor_agent_id FROM handler_rotations WHERE old_agent_id=? AND state='committed'`, current).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return "", err
		}
		current = next
	}
	if current == agentID {
		return "", nil
	}
	var status string
	if err := q.QueryRowContext(ctx, `SELECT status FROM agents WHERE id=?`, current).Scan(&status); err != nil {
		return "", err
	}
	if status == api.AgentClosed || status == api.AgentExited {
		return "", nil
	}
	return current, nil
}

func (s *Store) loadSuccessor(ctx context.Context, a *api.Agent) error {
	if a.Role != api.AgentRoleDatabaseHandler {
		return nil
	}
	err := s.db.QueryRowContext(ctx, `SELECT successor_agent_id FROM handler_rotations WHERE old_agent_id=? AND state='committed'`, a.ID).Scan(&a.SuccessorID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

// validRunRecord checks the template digest, model and reasoning a launch
// asks to record for the run it starts, before anything is written.
func validRunRecord(req api.AddAgentRequest) error {
	if req.TemplateDigest != "" && req.Role != api.AgentRoleDatabaseHandler && req.Role != api.AgentRoleBacklogSteward {
		return api.ErrInvalid
	}
	// Handler arms record a database handler run's model and reasoning.
	if (req.HandlerModel != "" || req.HandlerReasoning != "") && req.Role != api.AgentRoleDatabaseHandler {
		return api.ErrInvalid
	}
	if req.TemplateDigest != "" && (len(req.TemplateDigest) != 64 || strings.Trim(req.TemplateDigest, "0123456789abcdef") != "") {
		return api.ErrInvalid
	}
	if !validArmField(req.HandlerModel) || !validArmField(req.HandlerReasoning) {
		return api.ErrInvalid
	}
	return nil
}

// recordHandlerRun keeps the prompt template digest, model and reasoning of
// an exact handler run, in the transaction that starts the run, so a failed
// record fails the registration. The first record for a run wins, so a
// retried launch cannot rewrite it. The caller has checked the values with
// validRunRecord.
func recordHandlerRun(ctx context.Context, q execer, a api.Agent, req api.AddAgentRequest, now time.Time) error {
	if a.Role != api.AgentRoleDatabaseHandler || (req.TemplateDigest == "" && req.HandlerModel == "" && req.HandlerReasoning == "") || a.RunID == "" {
		return nil
	}
	_, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO handler_runs(task_id,agent_id,run_id,template_digest,model,reasoning,created_at) VALUES(?,?,?,?,?,?,?)`, a.TaskID, a.ID, a.RunID, req.TemplateDigest, req.HandlerModel, req.HandlerReasoning, ts(now))
	return err
}

const handlerRotationCols = `id,task_id,request_id,state,reason,trigger_kind,handler_revision,old_agent_id,old_run_id,old_name,successor_agent_id,successor_name,successor_run_id,handoff_json,receipt_json,created_at,updated_at,authorized_by,authorization_reason,old_runtime,successor_runtime,evidence_json,commit_evidence_json,last_activity_at,exit_json`

func scanHandlerRotation(row interface{ Scan(...any) error }) (api.HandlerRotation, error) {
	var r api.HandlerRotation
	var handoff, receipt, created, updated, evidence, commitEvidence, lastActivity, exit string
	var auth api.HandlerRotationAuthorization
	if err := row.Scan(&r.ID, &r.TaskID, &r.RequestID, &r.State, &r.Reason, &r.Trigger, &r.HandlerRevision, &r.OldAgentID, &r.OldRunID, &r.OldName,
		&r.SuccessorAgentID, &r.SuccessorName, &r.SuccessorRunID, &handoff, &receipt, &created, &updated,
		&auth.AuthorizedBy, &auth.Reason, &auth.OldRuntime, &auth.SuccessorRuntime, &evidence, &commitEvidence, &lastActivity, &exit); err != nil {
		return r, err
	}
	if exit != "" {
		r.Exit = &api.HandlerRotationExit{}
		if err := json.Unmarshal([]byte(exit), r.Exit); err != nil {
			return r, fmt.Errorf("handler rotation exit: %w", err)
		}
	}
	for _, saved := range []struct {
		raw string
		to  **api.HandlerDeathEvidence
	}{{evidence, &r.DeathEvidence}, {commitEvidence, &r.CommitDeathEvidence}} {
		if saved.raw == "" {
			continue
		}
		*saved.to = &api.HandlerDeathEvidence{}
		if err := json.Unmarshal([]byte(saved.raw), *saved.to); err != nil {
			return r, fmt.Errorf("handler rotation death evidence: %w", err)
		}
	}
	if lastActivity != "" {
		t := parseTS(lastActivity)
		r.LastActivityAt = &t
	}
	if auth.AuthorizedBy != "" {
		r.Authorization = &auth
	}
	r.CreatedAt, r.UpdatedAt = parseTS(created), parseTS(updated)
	if handoff != "" {
		r.Handoff = &api.HandlerRotationHandoff{}
		if err := json.Unmarshal([]byte(handoff), r.Handoff); err != nil {
			return r, fmt.Errorf("handler rotation handoff: %w", err)
		}
	}
	if receipt != "" {
		r.Receipt = &api.HandlerRotationReceipt{}
		if err := json.Unmarshal([]byte(receipt), r.Receipt); err != nil {
			return r, fmt.Errorf("handler rotation receipt: %w", err)
		}
	}
	return r, nil
}

func loadHandlerRotation(ctx context.Context, q queryRower, task, id string) (api.HandlerRotation, error) {
	r, err := scanHandlerRotation(q.QueryRowContext(ctx, `SELECT `+handlerRotationCols+` FROM handler_rotations WHERE task_id=? AND id=?`, task, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, api.ErrNotFound
	}
	return r, err
}

func openHandlerRotation(ctx context.Context, q queryRower, task string) (*api.HandlerRotation, error) {
	r, err := scanHandlerRotation(q.QueryRowContext(ctx, `SELECT `+handlerRotationCols+` FROM handler_rotations WHERE task_id=? AND state='prepared'`, task))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// successorHoldRow is the project's successor hold, whichever run it names,
// or nil.
func successorHoldRow(ctx context.Context, q queryRower, task string) (*api.HandlerSuccessorHold, error) {
	var h api.HandlerSuccessorHold
	var created string
	err := q.QueryRowContext(ctx, `SELECT reason,trigger_kind,old_agent_id,old_run_id,handler_revision,created_at FROM handler_successor_holds WHERE task_id=?`, task).
		Scan(&h.Reason, &h.Trigger, &h.OldAgentID, &h.OldRunID, &h.HandlerRevision, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h.CreatedAt = parseTS(created)
	return &h, nil
}

// loadSuccessorHold is the project's successor hold only while it names the
// current primary handler's exact agent and run; a hold for an earlier run
// is nil.
func loadSuccessorHold(ctx context.Context, q queryRower, t api.Task) (*api.HandlerSuccessorHold, error) {
	h, err := successorHoldRow(ctx, q, t.ID)
	if err != nil || h == nil {
		return nil, err
	}
	primary, err := primaryHandler(ctx, q, t)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if primary.ID != h.OldAgentID || primary.RunID != h.OldRunID {
		return nil, nil
	}
	return h, nil
}

// successorHoldCounts reports whether a current hold holds the seat: a
// manual hold always, a template hold only while the policy would rotate for
// a template change.
func successorHoldCounts(h api.HandlerSuccessorHold, policy api.HandlerRotationPolicy) bool {
	return h.Reason == api.HandlerRotationReasonManual || (h.Reason == api.HandlerRotationReasonTemplate && policy.Enabled && policy.OnTemplateChange)
}

// successorHeldRefusal is a no_agent_slot refusal whose prepare recorded a
// successor hold: HandlerRotationAction commits the hold and returns the bare
// refusal, with no rotation or request receipt.
type successorHeldRefusal struct{ refusal *api.HandlerRotationRefusal }

func (e successorHeldRefusal) Error() string { return e.refusal.Error() }
func (e successorHeldRefusal) Unwrap() error { return e.refusal }

// recordSuccessorHold writes the hold for a refused template or manual
// prepare of the exact old run. A hold for another run is replaced. One for
// the same run is kept, except that a manual refusal upgrades a template hold
// to manual, which counts whatever the policy says; a template refusal never
// downgrades a manual hold. One event marks a new hold; an upgrade adds none.
func (s *Store) recordSuccessorHold(ctx context.Context, tx *sql.Tx, t api.Task, old api.Agent, req api.HandlerRotationRequest, now time.Time, by api.Caller) error {
	h, err := successorHoldRow(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	if h != nil && h.OldAgentID == old.ID && h.OldRunID == old.RunID {
		if h.Reason == api.HandlerRotationReasonTemplate && req.Reason == api.HandlerRotationReasonManual {
			_, err = tx.ExecContext(ctx, `UPDATE handler_successor_holds SET reason=?,trigger_kind=? WHERE task_id=?`, req.Reason, req.Trigger, t.ID)
		}
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO handler_successor_holds(task_id,reason,trigger_kind,old_agent_id,old_run_id,handler_revision,created_at) VALUES(?,?,?,?,?,?,?)`,
		t.ID, req.Reason, req.Trigger, old.ID, old.RunID, t.HandlerRevision, ts(now)); err != nil {
		return err
	}
	_, err = s.insertEvent(ctx, tx, t.ID, "task_updated", old.ID, "Handler successor seat held", map[string]any{"reason": req.Reason, "trigger": req.Trigger,
		"oldAgentId": old.ID, "oldRunId": old.RunID, "handlerRevision": t.HandlerRevision}, by)
	return err
}

// releaseSuccessorHold is the release_hold operation: it deletes the hold of
// the exact old run, writes no request receipt, and is NotFound when that run
// holds none. The result is an aborted rotation with no ID that names the
// hold.
func (s *Store) releaseSuccessorHold(ctx context.Context, tx *sql.Tx, task string, req api.HandlerRotationRequest, by api.Caller) (api.HandlerRotation, error) {
	var zero api.HandlerRotation
	if !api.ValidID(req.OldAgentID, "agt") || !validRunID(req.OldRunID) || req.ExpectedHandlerRevision != 0 || req.SuccessorAgentID != "" || req.SuccessorName != "" ||
		req.Reason != "" || req.Trigger != "" || req.AuthorizedBy != "" || req.AuthorizationReason != "" || req.DeathEvidence != nil || req.RotationID != "" {
		return zero, api.ErrInvalid
	}
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, task))
	if errors.Is(err, sql.ErrNoRows) {
		return zero, api.ErrNotFound
	}
	if err != nil {
		return zero, err
	}
	h, err := successorHoldRow(ctx, tx, t.ID)
	if err != nil {
		return zero, err
	}
	if h == nil || h.OldAgentID != req.OldAgentID || h.OldRunID != req.OldRunID {
		return zero, api.ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM handler_successor_holds WHERE task_id=? AND old_agent_id=? AND old_run_id=?`, t.ID, h.OldAgentID, h.OldRunID); err != nil {
		return zero, err
	}
	var name string
	if err = tx.QueryRowContext(ctx, `SELECT name FROM agents WHERE task_id=? AND id=?`, t.ID, h.OldAgentID).Scan(&name); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	now := s.now()
	if _, err = s.insertEvent(ctx, tx, t.ID, "task_updated", h.OldAgentID, "Handler successor seat released", map[string]any{"reason": h.Reason, "trigger": h.Trigger,
		"oldAgentId": h.OldAgentID, "oldRunId": h.OldRunID}, by); err != nil {
		return zero, err
	}
	return api.HandlerRotation{TaskID: t.ID, RequestID: req.RequestID, State: api.HandlerRotationAborted, Reason: h.Reason, Trigger: h.Trigger, HandlerRevision: h.HandlerRevision,
		OldAgentID: h.OldAgentID, OldRunID: h.OldRunID, OldName: name, CreatedAt: h.CreatedAt, UpdatedAt: now}, nil
}

func (s *Store) GetHandlerRotation(ctx context.Context, task, id string) (api.HandlerRotation, error) {
	return loadHandlerRotation(ctx, s.db, task, id)
}

func (s *Store) ListHandlerRotations(ctx context.Context, task string) ([]api.HandlerRotation, error) {
	if _, err := s.GetTask(ctx, task); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+handlerRotationCols+` FROM handler_rotations WHERE task_id=? ORDER BY created_at,id`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.HandlerRotation{}
	for rows.Next() {
		r, err := scanHandlerRotation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type handlerBusy struct {
	leases      []api.HandlerRotationLease
	state       string
	pendingTool string
}

func loadHandlerBusy(ctx context.Context, q queryRower, task string, a api.Agent) (handlerBusy, error) {
	var b handlerBusy
	rows, err := q.QueryContext(ctx, `SELECT id,item_id,state,handler_lease_generation FROM team_queue_entries WHERE task_id=? AND handler_id=? AND handler_run_id=?
 AND `+queueHoldsSQL+` ORDER BY position,id`, task, a.ID, a.RunID)
	if err != nil {
		return b, err
	}
	b.leases = []api.HandlerRotationLease{}
	for rows.Next() {
		var l api.HandlerRotationLease
		if err := rows.Scan(&l.EntryID, &l.ItemID, &l.State, &l.LeaseGeneration); err != nil {
			rows.Close()
			return b, err
		}
		b.leases = append(b.leases, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return b, err
	}
	var payload string
	err = q.QueryRowContext(ctx, `SELECT state,payload FROM agent_activity WHERE agent_id=? AND run_id=?`, a.ID, a.RunID).Scan(&b.state, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		b.state = "unknown"
		return b, nil
	}
	if err != nil {
		return b, err
	}
	var activity api.AgentActivity
	if err := json.Unmarshal([]byte(payload), &activity); err != nil {
		return b, fmt.Errorf("activity snapshot: %w", err)
	}
	b.pendingTool = activity.PendingTool
	return b, nil
}

// refusal applies h2: never rotate a handler holding a live team lease or
// one that may be inside an atomic record. The owner command may treat an
// unobserved run as idle; the runner needs an observed idle state.
func (b handlerBusy) refusal(trigger string) *api.HandlerRotationRefusal {
	if len(b.leases) > 0 {
		return &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedLiveLease, Detail: fmt.Sprintf("the handler run holds %d live team lease(s), first %s", len(b.leases), b.leases[0].EntryID)}
	}
	if b.pendingTool != "" {
		return &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedPendingTool, Detail: "the handler run has a pending tool call: " + b.pendingTool}
	}
	switch b.state {
	case "working", "hung_tool", "looping":
		return &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedWorking, Detail: "the handler run's activity is " + b.state}
	case "idle", "finished_silent", "provider_blocked":
		// A provider-blocked handler makes no progress and holds no tool
		// call, so it is as safe to rotate as an idle one.
		return nil
	}
	if trigger == api.HandlerRotationTriggerRunner {
		return &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedWorking, Detail: "the runner needs an observed idle handler; activity is " + b.state}
	}
	return nil
}

func (b handlerBusy) idle() bool {
	return b.refusal(api.HandlerRotationTriggerRunner) == nil
}

// handlerSeenWithin reports whether the hub, by its own clock, has a
// heartbeat from the handler's current run within the online window.
func handlerSeenWithin(a api.Agent, now time.Time) bool {
	return !a.LastSeenAt.IsZero() && now.Sub(a.LastSeenAt) < api.HandlerOnlineWindow
}

// handlerLastActivity is the latest thing the hub recorded from the exact
// run: its wrapper's heartbeat, its observed activity, a message it sent or a
// work-item revision it saved. Events and broker reminders do not count,
// because other actors write those about the handler. ok is false when the
// run has none of the four: no record is not evidence of silence. Prepare,
// commit, the due list and the queue reason all use it, so they agree.
func handlerLastActivity(ctx context.Context, q queryRower, task string, a api.Agent) (time.Time, bool, error) {
	var last time.Time
	note := func(raw string) {
		if t := parseTS(raw); t.After(last) {
			last = t
		}
	}
	var raw string
	err := q.QueryRowContext(ctx, `SELECT last_seen_at FROM agents WHERE task_id=? AND id=? AND run_id=?`, task, a.ID, a.RunID).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return last, false, err
	}
	note(raw)
	raw = ""
	err = q.QueryRowContext(ctx, `SELECT observed_at FROM agent_activity WHERE agent_id=? AND run_id=?`, a.ID, a.RunID).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return last, false, err
	}
	note(raw)
	raw = ""
	err = q.QueryRowContext(ctx, `SELECT created_at FROM messages WHERE task_id=? AND from_agent=? AND from_run_id=? ORDER BY seq DESC LIMIT 1`, task, a.ID, a.RunID).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return last, false, err
	}
	note(raw)
	// Timestamps are not stored in a sortable form, so the newest revision is
	// found here rather than in SQL.
	rows, err := q.QueryContext(ctx, `SELECT updated_at FROM work_item_revisions WHERE item_task_id=? AND updated_agent=? AND updated_run_id=?`, task, a.ID, a.RunID)
	if err != nil {
		return last, false, err
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&raw); err != nil {
			return last, false, err
		}
		note(raw)
	}
	return last, !last.IsZero(), rows.Err()
}

func deathUnconfirmed(format string, args ...any) *api.HandlerRotationRefusal {
	return &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedDeathUnconfirmed, Detail: fmt.Sprintf(format, args...)}
}

// deathEvidenceRefusal checks a host's death evidence against what the hub
// itself knows: it must name the old handler's host and exact run, state both
// the session and the process gone, name that session, give a PID and start
// identity, and be fresh by the hub's clock. The hub must also see no heartbeat from the run.
// exit is the wrapper's exit report of an exited primary, nil otherwise. Only
// with it may the session be present as an idle shell, and then the host's
// own receipt must record the exit and the evidence must name the pane's
// process.
func deathEvidenceRefusal(ev *api.HandlerDeathEvidence, old api.Agent, exit *api.HandlerRotationExit, now time.Time) *api.HandlerRotationRefusal {
	switch {
	case ev == nil:
		return deathUnconfirmed("a dead primary rotation needs the host's death evidence")
	case ev.Host == "" || ev.Host != old.Host:
		return deathUnconfirmed("the evidence is from host %q, not the handler's host %q", ev.Host, old.Host)
	case ev.AgentID != old.ID || ev.RunID != old.RunID:
		return deathUnconfirmed("the evidence does not name the primary handler's exact agent and run")
	case exit == nil && (ev.SessionState != api.HandlerDeathStateGone || ev.ProcessState != api.HandlerDeathStateGone):
		return deathUnconfirmed("the evidence must state the session and the process gone; it states session %q, process %q", ev.SessionState, ev.ProcessState)
	case exit != nil && (ev.ProcessState != api.HandlerDeathStateGone || (ev.SessionState != api.HandlerDeathStateGone && ev.SessionState != api.HandlerDeathStateIdleShell)):
		return deathUnconfirmed("for a handler whose wrapper reported exited the evidence must state the process gone and the session gone or %s; it states session %q, process %q", api.HandlerDeathStateIdleShell, ev.SessionState, ev.ProcessState)
	case exit != nil && ev.ExitedAt == nil:
		return deathUnconfirmed("the host's process receipt records no exit, so the exit report is not confirmed on the handler's host")
	case ev.SessionState == api.HandlerDeathStateIdleShell && ev.PaneRootPID <= 0:
		return deathUnconfirmed("the evidence of an idle shell does not name the pane's process")
	case ev.SessionName == "":
		return deathUnconfirmed("the evidence does not name the tmux session it found absent")
	case ev.PID <= 0 || ev.ProcessStarted == "" || ev.PanePID < 0 || ev.PaneRootPID < 0:
		return deathUnconfirmed("the evidence has no runtime process ID and start identity")
	case ev.ObservedAt.IsZero() || now.Sub(ev.ObservedAt) > api.HandlerDeathEvidenceMaxAge:
		return deathUnconfirmed("the evidence was observed more than %s ago", api.HandlerDeathEvidenceMaxAge)
	case ev.ObservedAt.Sub(now) > api.HandlerDeathEvidenceMaxAhead:
		return deathUnconfirmed("the evidence is dated more than %s ahead of the hub clock", api.HandlerDeathEvidenceMaxAhead)
	case handlerSeenWithin(old, now):
		return deathUnconfirmed("the hub had a heartbeat from the handler run within the last %s", api.HandlerOnlineWindow)
	}
	return nil
}

func validDeathEvidenceText(ev *api.HandlerDeathEvidence) bool {
	for _, v := range []string{ev.Host, ev.AgentID, ev.RunID, ev.SessionName, ev.SessionID, ev.SessionCreated, ev.SessionState, ev.ProcessStarted, ev.ProcessState} {
		if !api.ValidText(v, 253) || strings.ContainsAny(v, "\r\n\t") {
			return false
		}
	}
	return true
}

// deadPrimaryPrepareCheck is every hub-side condition of a dead primary
// rotation besides the guards all rotations share. It returns the run's last
// recorded activity, which commit requires unchanged, and for a primary whose
// wrapper reported exited the exit report, which commit requires unchanged
// too.
func deadPrimaryPrepareCheck(ctx context.Context, q queryRower, t api.Task, old api.Agent, busy handlerBusy, ev *api.HandlerDeathEvidence, now time.Time) (time.Time, *api.HandlerRotationExit, error) {
	var zero time.Time
	policy, err := loadHandlerRotationPolicy(ctx, q, t.ID)
	if err != nil {
		return zero, nil, err
	}
	if policy.DeadSilenceMinutes <= 0 {
		return zero, nil, deathUnconfirmed("automatic replacement of a dead primary is off for this project")
	}
	var exit *api.HandlerRotationExit
	if old.Status == api.AgentExited {
		if exit, err = wrapperExitReport(ctx, q, t.ID, old); err != nil {
			return zero, nil, err
		}
	}
	if refusal := deathEvidenceRefusal(ev, old, exit, now); refusal != nil {
		return zero, nil, refusal
	}
	if busy.refusal(api.HandlerRotationTriggerRunner) == nil {
		return zero, nil, deathUnconfirmed("the handler is not busy; the ordinary rotation applies")
	}
	last, ok, err := handlerSilentSince(ctx, q, t.ID, old, exit)
	if err != nil {
		return zero, nil, err
	}
	if !ok {
		return zero, nil, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedNotSilent, Detail: "the hub has no recorded activity from the handler run, which is not evidence of silence"}
	}
	if silent := now.Sub(last); silent < time.Duration(policy.DeadSilenceMinutes)*time.Minute {
		return zero, nil, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedNotSilent, Detail: fmt.Sprintf("the handler run was last active %s ago; the policy needs %d minute(s) of silence", silent.Truncate(time.Second), policy.DeadSilenceMinutes)}
	}
	return last, exit, nil
}

// handlerSilentSince is handlerLastActivity, never earlier than the wrapper's
// exit report when there is one: the silence of an exited run starts at its
// exit or later.
func handlerSilentSince(ctx context.Context, q queryRower, task string, a api.Agent, exit *api.HandlerRotationExit) (time.Time, bool, error) {
	last, ok, err := handlerLastActivity(ctx, q, task, a)
	if err == nil && exit != nil && exit.ReportedAt.After(last) {
		last, ok = exit.ReportedAt, true
	}
	return last, ok, err
}

// deadPrimaryCommitCheck re-confirms a prepared dead primary rotation: a
// second observation later than the first, still no heartbeat, and no activity
// recorded since prepare. It returns the policy whose silence applied.
func deadPrimaryCommitCheck(ctx context.Context, q queryRower, t api.Task, old api.Agent, r api.HandlerRotation, ev *api.HandlerDeathEvidence, now time.Time) (api.HandlerRotationPolicy, error) {
	policy, err := loadHandlerRotationPolicy(ctx, q, t.ID)
	if err != nil {
		return policy, err
	}
	if policy.DeadSilenceMinutes <= 0 {
		return policy, deathUnconfirmed("automatic replacement of a dead primary was turned off for this project")
	}
	if ev == nil {
		return policy, deathUnconfirmed("the commit of a dead primary rotation needs a second observation from the host")
	}
	// An exit rotation needs the same exit report still to be the run's
	// newest start, heartbeat or exit.
	var exit *api.HandlerRotationExit
	if r.Exit != nil {
		if exit, err = wrapperExitReport(ctx, q, t.ID, old); err != nil {
			return policy, err
		}
		if exit.EventSeq != r.Exit.EventSeq {
			return policy, deathUnconfirmed("the handler's exit report is not the one saved at prepare")
		}
	}
	if refusal := deathEvidenceRefusal(ev, old, exit, now); refusal != nil {
		return policy, refusal
	}
	if r.DeathEvidence == nil || r.LastActivityAt == nil {
		return policy, workItemConflict("handler rotation " + r.ID + " has no saved death evidence")
	}
	if !ev.ObservedAt.After(r.DeathEvidence.ObservedAt) {
		return policy, deathUnconfirmed("the commit needs an observation later than the one saved at prepare")
	}
	last, ok, err := handlerSilentSince(ctx, q, t.ID, old, exit)
	if err != nil {
		return policy, err
	}
	if !ok || last.After(*r.LastActivityAt) {
		return policy, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedNotSilent, Detail: "the handler run recorded activity after the rotation was prepared"}
	}
	return policy, nil
}

// exitedRecipientGoneReason is the reason the broker saves when it closes an
// obligation whose recipient exited (BrokerCloseRecipientGone).
const exitedRecipientGoneReason = "recipient agent is " + api.AgentExited

// exitedHandlerClosedObligations lists the obligations of an exited handler
// that the broker closed recipient_gone, for that reason, at or after the
// wrapper's exit report. Nothing closed earlier or for any other reason is
// listed. Reissuing one marks it superseded, and one its sender already
// re-sent stays closed (resentAfterClose); the same commit closes the old
// handler, so none is listed twice.
func exitedHandlerClosedObligations(ctx context.Context, tx *sql.Tx, task string, old api.Agent, exit api.HandlerRotationExit) ([]api.Obligation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE task_id=? AND agent_id=? AND state=? AND outcome=? AND reason=? ORDER BY message_seq,id`,
		task, old.ID, api.ObligationClosed, api.OutcomeRecipientGone, exitedRecipientGoneReason)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Obligation
	for rows.Next() {
		o, err := scanObligation(rows)
		if err != nil {
			return nil, err
		}
		// Timestamps are not stored in a sortable form, so the bound is applied here.
		if o.ClosedAt != nil && !o.ClosedAt.Before(exit.ReportedAt) {
			out = append(out, o)
		}
	}
	return out, rows.Err()
}

// resentAfterClose finds the sender's re-send of a closed obligation: a later
// message from the author of its original message, to the successor by name
// or to the database handler role, created at or after the close. It returns
// the first match and why it matched, or a zero message.
func resentAfterClose(ctx context.Context, tx *sql.Tx, task string, o api.Obligation, successor api.Agent) (api.Message, string, error) {
	if o.ClosedAt == nil {
		return api.Message{}, "", nil
	}
	authored, err := originalMessage(ctx, tx, task, o.MessageSeq)
	if err != nil {
		return api.Message{}, "", err
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.seq FROM messages m WHERE m.task_id=? AND m.seq>? AND m.from_agent=? AND m.from_node=? AND m.from_user=?
 AND (m.to_agent=? OR EXISTS (SELECT 1 FROM obligations ob WHERE ob.message_seq=m.seq AND ob.via_role=?)) ORDER BY m.seq`,
		task, o.MessageSeq, authored.From.AgentID, authored.From.Node, authored.From.User, successor.ID, api.RoleDatabaseHandler)
	if err != nil {
		return api.Message{}, "", err
	}
	var seqs []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			rows.Close()
			return api.Message{}, "", err
		}
		seqs = append(seqs, seq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return api.Message{}, "", err
	}
	for _, seq := range seqs {
		c, err := loadMessage(tx, ctx, task, seq)
		if err != nil {
			return api.Message{}, "", err
		}
		// Timestamps are not stored in a sortable form, so the bound is applied here.
		if c.CreatedAt.Before(*o.ClosedAt) {
			continue
		}
		if reason := resentReason(authored, c); reason != "" {
			return c, reason, nil
		}
	}
	return api.Message{}, "", nil
}

// resentReason says why a later message c from the same sender re-sends
// original: the same kind, and either the same request id or the same subject
// and linked work items (two unlinked messages link the same items). It is ""
// when c is something else.
func resentReason(original, c api.Message) string {
	if original.Envelope == nil || c.Envelope == nil || original.Envelope.Kind != c.Envelope.Kind {
		return ""
	}
	if original.PostReceipt != nil && c.PostReceipt != nil && original.PostReceipt.RequestID != "" && original.PostReceipt.RequestID == c.PostReceipt.RequestID {
		return api.HandlerRotationSkipRequestID
	}
	subject := strings.TrimSpace(original.Envelope.Subject)
	if subject == "" || subject != strings.TrimSpace(c.Envelope.Subject) {
		return ""
	}
	items := func(m api.Message) map[string]bool {
		set := map[string]bool{}
		for _, w := range m.WorkItems {
			set[w.ItemTaskID+"/"+w.ItemID] = true
		}
		return set
	}
	a, b := items(original), items(c)
	if len(a) != len(b) {
		return ""
	}
	for k := range a {
		if !b[k] {
			return ""
		}
	}
	return api.HandlerRotationSkipSubjectItem
}

// deadPrimaryOffHint is how the feature is turned off; the docs, the queue
// reason and the notice quote it.
func deadPrimaryOffHint(task string, revision int64) string {
	return fmt.Sprintf("tt handler policy set --task %s --revision %d --dead-silence-minutes 0", task, revision)
}

// rewriteLaunchHandler replaces the handler identity inside an entry's frozen
// launch plan, leaving every other byte as stored: the team runner refuses a
// plan that names another handler than the entry's lease. A plan that names
// no handler is returned unchanged; one that names a third handler, or that
// cannot be read, is an error.
func rewriteLaunchHandler(plan []byte, oldID, newID, newRunID string, newGeneration int64) ([]byte, bool, error) {
	type span struct {
		start, end int
		found      bool
	}
	var id, run, generation span
	var named string
	dec := json.NewDecoder(bytes.NewReader(plan))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false, errors.New("the stored launch plan is not a JSON object")
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, false, err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false, err
		}
		end := int(dec.InputOffset())
		at := span{start: end - len(raw), end: end, found: true}
		var target *span
		switch key {
		case "handlerId":
			target = &id
			if err := json.Unmarshal(raw, &named); err != nil {
				return nil, false, err
			}
		case "handlerRunId":
			target = &run
		case "handlerLeaseGeneration":
			target = &generation
		default:
			continue
		}
		if target.found {
			return nil, false, fmt.Errorf("the stored launch plan repeats %s", key)
		}
		*target = at
	}
	if _, err := dec.Token(); err != nil {
		return nil, false, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false, errors.New("the stored launch plan has trailing data")
	}
	if named == "" {
		return plan, false, nil
	}
	if named != oldID {
		return nil, false, fmt.Errorf("the stored launch plan names handler %s", named)
	}
	newIDJSON, _ := json.Marshal(newID)
	newRunJSON, _ := json.Marshal(newRunID)
	newGenerationJSON := []byte(fmt.Sprint(newGeneration))
	// A field the plan omitted is added right after the handler ID.
	idText := append([]byte(nil), newIDJSON...)
	if !run.found {
		idText = append(append(idText, `,"handlerRunId":`...), newRunJSON...)
	}
	if !generation.found {
		idText = append(append(idText, `,"handlerLeaseGeneration":`...), newGenerationJSON...)
	}
	edits := []struct {
		at   span
		text []byte
	}{{id, idText}}
	if run.found {
		edits = append(edits, struct {
			at   span
			text []byte
		}{run, newRunJSON})
	}
	if generation.found {
		edits = append(edits, struct {
			at   span
			text []byte
		}{generation, newGenerationJSON})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].at.start > edits[j].at.start })
	out := append([]byte(nil), plan...)
	for _, e := range edits {
		out = append(out[:e.at.start], append(append([]byte(nil), e.text...), out[e.at.end:]...)...)
	}
	return out, true, nil
}

// moveDeadHandlerLeases hands every lease the dead run holds to the
// successor, in queue order: the entry's handler, run and a new generation,
// its frozen launch plan and its arm assignment. Leases are moved, never
// released: a released lease would strand a running team with no handler. Any
// entry that cannot be moved exactly fails the whole commit.
func (s *Store) moveDeadHandlerLeases(ctx context.Context, tx *sql.Tx, t api.Task, old, successor api.Agent, leases []api.HandlerRotationLease, now time.Time) ([]api.HandlerRotationLease, error) {
	moved := []api.HandlerRotationLease{}
	for _, l := range leases {
		var launch []byte
		if err := tx.QueryRowContext(ctx, `SELECT launch_json FROM team_queue_entries WHERE id=? AND task_id=?`, l.EntryID, t.ID).Scan(&launch); err != nil {
			return nil, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(handler_lease_generation),0)+1 FROM team_queue_entries WHERE handler_id=?`, successor.ID).Scan(&l.NewLeaseGeneration); err != nil {
			return nil, err
		}
		if len(launch) > 0 {
			rewritten, changed, err := rewriteLaunchHandler(launch, old.ID, successor.ID, successor.RunID, l.NewLeaseGeneration)
			if err != nil {
				return nil, workItemConflict(fmt.Sprintf("team queue entry %s cannot be handed to the successor: %v", l.EntryID, err))
			}
			launch, l.LaunchPlanRewritten = rewritten, changed
		}
		res, err := tx.ExecContext(ctx, `UPDATE team_queue_entries SET handler_id=?,handler_run_id=?,handler_lease_generation=?,launch_json=?,revision=revision+1,updated_at=?
 WHERE id=? AND task_id=? AND handler_id=? AND handler_run_id=? AND handler_lease_generation=? AND `+queueHoldsSQL,
			successor.ID, successor.RunID, l.NewLeaseGeneration, string(launch), ts(now), l.EntryID, t.ID, old.ID, old.RunID, l.LeaseGeneration)
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if n != 1 {
			return nil, workItemConflict(fmt.Sprintf("team queue entry %s changed while its lease was being handed to the successor", l.EntryID))
		}
		if err := moveHandlerArmAssignment(ctx, tx, l.EntryID, old.ID, l.LeaseGeneration, successor, l.NewLeaseGeneration); err != nil {
			return nil, err
		}
		moved = append(moved, l)
	}
	return moved, nil
}

const handlerRotationRecentWrites = 50

// snapshotDeadInFlight reports what the dead run was doing. Nothing can be
// recovered from a dead process, so its last state, its pending tool and the
// work-item revisions it saved recently are listed for the successor to
// compare with each moved request. The window starts at the oldest open
// obligation it held, or 30 minutes before its last activity with none.
func snapshotDeadInFlight(ctx context.Context, tx *sql.Tx, t api.Task, old api.Agent, busy handlerBusy, lastActivity time.Time, silence int64, open []api.Obligation) (*api.HandlerRotationInFlight, error) {
	in := &api.HandlerRotationInFlight{ActivityState: busy.state, PendingTool: busy.pendingTool, LastActivityAt: lastActivity, SilenceMinutes: silence,
		WritesSince: lastActivity.Add(-30 * time.Minute), RecentWrites: []api.HandlerRotationWrite{}}
	for i, o := range open {
		if i == 0 || o.CreatedAt.Before(in.WritesSince) {
			in.WritesSince = o.CreatedAt
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT item_id,revision,updated_at,COALESCE(changed_fields,'') FROM work_item_revisions WHERE item_task_id=? AND updated_agent=? AND updated_run_id=?`, t.ID, old.ID, old.RunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var w api.HandlerRotationWrite
		var updated, changed string
		if err := rows.Scan(&w.ItemID, &w.Revision, &updated, &changed); err != nil {
			return nil, err
		}
		if w.UpdatedAt = parseTS(updated); w.UpdatedAt.Before(in.WritesSince) {
			continue
		}
		w.ChangedFields = []string{}
		if changed != "" {
			if err := json.Unmarshal([]byte(changed), &w.ChangedFields); err != nil {
				return nil, fmt.Errorf("work item revision changed fields: %w", err)
			}
		}
		in.RecentWrites = append(in.RecentWrites, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(in.RecentWrites, func(i, j int) bool {
		a, b := in.RecentWrites[i], in.RecentWrites[j]
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		if a.ItemID != b.ItemID {
			return a.ItemID < b.ItemID
		}
		return a.Revision > b.Revision
	})
	if n := len(in.RecentWrites); n > handlerRotationRecentWrites {
		in.RecentWrites, in.WritesOmitted = in.RecentWrites[:handlerRotationRecentWrites], n-handlerRotationRecentWrites
	}
	return in, nil
}

const deadPrimaryNoticeSubject = "A dead primary database handler was replaced automatically"

// deadPrimaryNoticeText is the one notice of a dead primary rotation: the dead
// handler, the evidence, what was handed off, what the successor does next,
// and how the owner turns the replacement off.
func deadPrimaryNoticeText(t api.Task, r api.HandlerRotation, old, successor api.Agent, commit *api.HandlerDeathEvidence, h api.HandlerRotationHandoff, policyRevision int64) string {
	ev, in := r.DeathEvidence, h.InFlight
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	var b strings.Builder
	fmt.Fprintf(&b, "Primary database handler %s (%s / %s) was dead while the hub still recorded it as busy, so the host runner replaced it with %s by handler rotation %s.", old.Name, old.ID, old.RunID, successor.Name, r.ID)
	if r.Exit != nil {
		// The exited case: the session may remain, holding the wrapper's shell.
		session := "is absent"
		if ev.SessionState == api.HandlerDeathStateIdleShell {
			session = fmt.Sprintf("holds only the wrapper (process %d) and its idle shell", ev.PaneRootPID)
		}
		fmt.Fprintf(&b, " Its runtime exited with status %d at %s, as reported by its wrapper. Evidence from host %s: runtime process %d (started %s) is absent and tmux session %s %s",
			r.Exit.Code, stamp(r.Exit.ReportedAt), ev.Host, ev.PID, ev.ProcessStarted, ev.SessionName, session)
	} else {
		fmt.Fprintf(&b, " Evidence from host %s: tmux session %s is absent and runtime process %d (started %s) is absent", ev.Host, ev.SessionName, ev.PID, ev.ProcessStarted)
		if ev.PanePID > 0 {
			fmt.Fprintf(&b, ", as is pane process %d", ev.PanePID)
		}
	}
	fmt.Fprintf(&b, "; observed %s and again %s.", stamp(ev.ObservedAt), stamp(commit.ObservedAt))
	if ev.ExitedAt != nil && r.Exit == nil {
		fmt.Fprintf(&b, " Its wrapper recorded an exit at %s.", stamp(*ev.ExitedAt))
	}
	fmt.Fprintf(&b, " The hub's last recorded activity from that run was %s; the silence applied was %d minute(s).", stamp(in.LastActivityAt), in.SilenceMinutes)
	fmt.Fprintf(&b, " Handed to %s: %d open obligation(s) and %d team lease(s)", successor.Name, len(h.Reissued), len(h.LiveLeases))
	if len(h.LiveLeases) > 0 {
		ids := make([]string, 0, 5)
		for i, l := range h.LiveLeases {
			if i == 5 {
				break
			}
			ids = append(ids, l.EntryID)
		}
		b.WriteString(" (" + strings.Join(ids, ", "))
		if more := len(h.LiveLeases) - len(ids); more > 0 {
			fmt.Fprintf(&b, " and %d more", more)
		}
		b.WriteString(")")
	}
	b.WriteString(". Reported, not recovered: last activity " + in.ActivityState)
	if in.PendingTool != "" {
		b.WriteString(", pending tool " + provisionText(in.PendingTool))
	}
	fmt.Fprintf(&b, ", %d recent work-item save(s) by the dead run, %d allocation intent(s) and %d claimed Queue entr(ies) it authored.", len(in.RecentWrites)+in.WritesOmitted, len(h.AllocationIntents), len(h.QueueClaims))
	fmt.Fprintf(&b, " %s: run tt handler rotation get %s --task %s and compare each moved request with inFlight.recentWrites before repeating a save.", successor.Name, r.ID, t.ID)
	fmt.Fprintf(&b, " This was automatic under owner order #28057. To turn it off for this project: %s", deadPrimaryOffHint(t.ID, policyRevision))
	return b.String()
}

const providerBlockedReplacedSubject = "A provider-blocked primary database handler was replaced automatically"

// openOwnerHelper is the project's newest open owner helper, or the zero agent
// (a board-wide notice) when there is none.
func openOwnerHelper(ctx context.Context, tx *sql.Tx, task string) (api.Agent, error) {
	helper, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status NOT IN (?,?) ORDER BY created_at DESC LIMIT 1`, task, api.AgentRoleOwnerHelper, api.AgentClosed, api.AgentExited))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Agent{}, nil
	}
	return helper, err
}

// providerBlockedReplacedText is the owner helper's notice of a committed
// provider_blocked rotation: the old and new handler, the block's class and
// since from the old run's latest rate_limited or server_error episode, and
// the obligations moved.
func providerBlockedReplacedText(ctx context.Context, tx *sql.Tx, t api.Task, r api.HandlerRotation, old, successor api.Agent, h api.HandlerRotationHandoff) (string, error) {
	var class, since, cleared string
	err := tx.QueryRowContext(ctx, `SELECT class,since,cleared_at FROM provider_block_episodes WHERE task_id=? AND agent_id=? AND run_id=? AND class IN (?,?) ORDER BY julianday(opened_at) DESC LIMIT 1`,
		t.ID, old.ID, old.RunID, api.ProviderBlockRateLimited, api.ProviderBlockServerError).Scan(&class, &since, &cleared)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	block := "an unrecorded provider block"
	if class != "" {
		block = fmt.Sprintf("a %s provider block since %s", class, parseTS(since).UTC().Format(time.RFC3339))
		if cleared != "" {
			block += " (it cleared before the commit)"
		}
	}
	policy, err := loadHandlerRotationPolicy(ctx, tx, t.ID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Primary database handler %s (%s / %s) held open obligations while stopped by %s for at least %s, so the host runner replaced it with %s by handler rotation %s once %s had completed a turn. %d open obligation(s) moved to %s. No other action is needed. Read the handoff with tt handler rotation get %s --task %s. This replacement shares the dead primary switch; to turn both off for this project: %s (bug wi_ce7310979bc45479, order #46760).",
		old.Name, old.ID, old.RunID, block, api.HandlerProviderBlockedAfter, successor.Name, r.ID, successor.Name, len(h.Reissued), successor.Name, r.ID, t.ID, deadPrimaryOffHint(t.ID, policy.Revision)), nil
}

func handlerRotationReceipt(ctx context.Context, tx *sql.Tx, task, requestID, operation, hash string) (string, bool, error) {
	var priorOperation, priorHash, rotation string
	err := tx.QueryRowContext(ctx, `SELECT operation,payload_hash,rotation_id FROM handler_rotation_requests WHERE task_id=? AND request_id=?`, task, requestID).Scan(&priorOperation, &priorHash, &rotation)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if priorOperation != operation || priorHash != hash {
		return "", false, workItemConflict("handler rotation request ID was already used with different input")
	}
	return rotation, true, nil
}

func (s *Store) HandlerRotationAction(ctx context.Context, task string, req api.HandlerRotationRequest, by api.Caller) (api.HandlerRotation, error) {
	var zero api.HandlerRotation
	if !api.ValidID(task, "tsk") || !validRequestID(req.RequestID) {
		return zero, api.ErrInvalid
	}
	if req.ActorAgentID != "" {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedAgentCaller, Detail: "handler rotation is an owner or host-runner action; agent sessions cannot rotate handlers"}
	}
	if req.DeathEvidence != nil && !validDeathEvidenceText(req.DeathEvidence) {
		return zero, api.ErrInvalid
	}
	// A commit is keyed by its rotation. Its second observation is not part
	// of the key, so a retry of an unconfirmed commit with a newer observation
	// replays the saved record.
	keyed := req
	if req.Operation == api.HandlerRotationCommit {
		keyed.DeathEvidence = nil
	}
	hash := requestHash(keyed)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	// A release is idempotent by its exact run, so it keeps no receipt.
	if req.Operation == api.HandlerRotationReleaseHold {
		out, err := s.releaseSuccessorHold(ctx, tx, task, req, by)
		if err != nil {
			return zero, err
		}
		if err = tx.Commit(); err != nil {
			return zero, err
		}
		s.notify(task)
		return out, nil
	}
	if id, replay, err := handlerRotationReceipt(ctx, tx, task, req.RequestID, req.Operation, hash); err != nil {
		return zero, err
	} else if replay {
		return loadHandlerRotation(ctx, tx, task, id)
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
	var out api.HandlerRotation
	switch req.Operation {
	case api.HandlerRotationPrepare:
		out, err = s.prepareHandlerRotation(ctx, tx, t, req, by)
	case api.HandlerRotationCommit:
		out, err = s.commitHandlerRotation(ctx, tx, t, req, by)
	case api.HandlerRotationAbort:
		out, err = s.abortHandlerRotation(ctx, tx, t, req, by)
	default:
		return zero, api.ErrInvalid
	}
	var held successorHeldRefusal
	if errors.As(err, &held) {
		// The hold is the refusal's only save.
		if err = tx.Commit(); err != nil {
			return zero, err
		}
		s.notify(task)
		return zero, held.refusal
	}
	if err != nil {
		return zero, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO handler_rotation_requests(task_id,request_id,operation,payload_hash,rotation_id) VALUES(?,?,?,?,?)`, task, req.RequestID, req.Operation, hash, out.ID); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	s.notify(task)
	return out, nil
}

func (s *Store) prepareHandlerRotation(ctx context.Context, tx *sql.Tx, t api.Task, req api.HandlerRotationRequest, by api.Caller) (api.HandlerRotation, error) {
	var zero api.HandlerRotation
	switch req.Reason {
	case api.HandlerRotationReasonManual, api.HandlerRotationReasonItems, api.HandlerRotationReasonTokens, api.HandlerRotationReasonTemplate, api.HandlerRotationReasonDeadPrimary,
		api.HandlerRotationReasonProviderBlocked:
	default:
		return zero, api.ErrInvalid
	}
	// Only the host runner replaces a provider-blocked primary, never with an
	// owner authorization.
	blocked := req.Reason == api.HandlerRotationReasonProviderBlocked
	if blocked && (req.Trigger != api.HandlerRotationTriggerRunner || req.AuthorizedBy != "" || req.AuthorizationReason != "") {
		return zero, api.ErrInvalid
	}
	// Only the host runner replaces a dead primary, only with its host's
	// evidence, and never with an owner authorization; evidence means nothing
	// to any other rotation.
	dead := req.Reason == api.HandlerRotationReasonDeadPrimary
	if dead != (req.DeathEvidence != nil) || (dead && (req.Trigger != api.HandlerRotationTriggerRunner || req.AuthorizedBy != "" || req.AuthorizationReason != "")) {
		return zero, api.ErrInvalid
	}
	if (req.Trigger != api.HandlerRotationTriggerOwner && req.Trigger != api.HandlerRotationTriggerRunner) || req.RotationID != "" ||
		!api.ValidID(req.OldAgentID, "agt") || !validRunID(req.OldRunID) || !api.ValidID(req.SuccessorAgentID, "agt") || !api.ValidName(req.SuccessorName) || req.ExpectedHandlerRevision < 1 {
		return zero, api.ErrInvalid
	}
	// An authorization names who allowed a change of runtime, model or arm and
	// why: both fields or neither, and never from the runner.
	authorized := req.AuthorizedBy != "" || req.AuthorizationReason != ""
	if authorized && (req.Trigger != api.HandlerRotationTriggerOwner || strings.TrimSpace(req.AuthorizedBy) == "" || strings.TrimSpace(req.AuthorizationReason) == "" ||
		!api.ValidText(req.AuthorizedBy, 200) || !api.ValidText(req.AuthorizationReason, 1000)) {
		return zero, api.ErrInvalid
	}
	if t.PauseState != api.ProjectPauseActive {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedPaused, Detail: "the project is paused"}
	}
	if t.HandlerRevision != req.ExpectedHandlerRevision {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedStaleHandler, Detail: fmt.Sprintf("the project handler is at revision %d; reload before rotating", t.HandlerRevision)}
	}
	if open, err := openHandlerRotation(ctx, tx, t.ID); err != nil {
		return zero, err
	} else if open != nil {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedOpen, Detail: "rotation " + open.ID + " is still prepared; finish or abort it first"}
	}
	old, err := primaryHandler(ctx, tx, t)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	if dead {
		// Only this rotation sees a primary whose wrapper reported exited, and
		// only when the request names it or it is busy, as in the due list.
		// An idle exited primary leaves the acting handler above in place.
		if exited, ok, err := exitedPrimaryHandler(ctx, tx, t); err != nil {
			return zero, err
		} else if ok {
			use := exited.ID == req.OldAgentID && exited.RunID == req.OldRunID
			if !use {
				held, err := loadHandlerBusy(ctx, tx, t.ID, exited)
				if err != nil {
					return zero, err
				}
				use = !held.idle()
			}
			if use {
				old = exited
			}
		}
	}
	if old.ID != req.OldAgentID || old.RunID != req.OldRunID {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedNotPrimary, Detail: "the named run is not the project's current primary database handler"}
	}
	if want := api.HandlerSuccessorName(old.Name, t.HandlerRevision); req.SuccessorName != want {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedNameTaken, Detail: "the successor must be named " + want}
	}
	var taken int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE (task_id=? AND name=? COLLATE NOCASE AND status<>?) OR id=?`, t.ID, req.SuccessorName, api.AgentClosed, req.SuccessorAgentID).Scan(&taken); err != nil {
		return zero, err
	}
	if taken != 0 {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedNameTaken, Detail: "the successor name or agent ID is already in use"}
	}
	busy, err := loadHandlerBusy(ctx, tx, t.ID, old)
	if err != nil {
		return zero, err
	}
	now := s.now()
	var lastActivity time.Time
	var exit *api.HandlerRotationExit
	if dead {
		// The one exception to h2, by owner order #28057: the busy refusal is
		// lifted only when the host confirmed the death and the hub's own
		// records show the run silent.
		if lastActivity, exit, err = deadPrimaryPrepareCheck(ctx, tx, t, old, busy, req.DeathEvidence, now); err != nil {
			return zero, err
		}
	} else if refusal := busy.refusal(req.Trigger); refusal != nil {
		return zero, refusal
	}
	if blocked {
		// The hub checks the due condition itself, for the exact old run.
		policy, err := loadHandlerRotationPolicy(ctx, tx, t.ID)
		if err != nil {
			return zero, err
		}
		if _, ok, err := providerBlockedPrimary(ctx, tx, t.ID, policy, old, busy, now); err != nil {
			return zero, err
		} else if !ok {
			return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedNotBlocked, Detail: fmt.Sprintf("handler %s is not online and idle with a rate_limited or server_error block open for %s and an open obligation, or the dead primary replacement is off", old.Name, api.HandlerProviderBlockedAfter)}
		}
	}
	// The successor registers like any agent, after the prepare. With no slot
	// for it, a prepared rotation would only hold dispatch until it is
	// aborted (bug wi_caa61d47ad431bf5), so nothing is prepared.
	capOpen, capReserved, err := projectAgentCapUsage(ctx, tx, t.ID)
	if err != nil {
		return zero, err
	}
	if reason := projectAgentCapReason(capOpen, capReserved, 1, s.MaxAgents); reason != "" {
		refusal := &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedNoAgentSlot, Detail: reason + "; the successor cannot register until an agent is closed"}
		// The hub's due rule cannot see a template or manual rotation, so
		// the refusal itself holds the next free seat (bug
		// wi_bbea3ea3ad6068a1); every other reason's seat is held by that rule.
		if req.Reason != api.HandlerRotationReasonTemplate && req.Reason != api.HandlerRotationReasonManual {
			return zero, refusal
		}
		if err = s.recordSuccessorHold(ctx, tx, t, old, req, now, by); err != nil {
			return zero, err
		}
		refusal.Detail += "; the next free agent seat is held for the successor"
		return zero, successorHeldRefusal{refusal}
	}
	// The prepared rotation holds the seat from here until its successor
	// registers.
	if _, err = tx.ExecContext(ctx, `DELETE FROM handler_successor_holds WHERE task_id=?`, t.ID); err != nil {
		return zero, err
	}
	r := api.HandlerRotation{ID: api.NewID("hrot"), TaskID: t.ID, RequestID: req.RequestID, State: api.HandlerRotationPrepared, Reason: req.Reason, Trigger: req.Trigger,
		HandlerRevision: t.HandlerRevision, OldAgentID: old.ID, OldRunID: old.RunID, OldName: old.Name, SuccessorAgentID: req.SuccessorAgentID,
		SuccessorName: req.SuccessorName, CreatedAt: now, UpdatedAt: now}
	payload := map[string]any{"handlerRotationId": r.ID, "state": r.State, "reason": r.Reason,
		"trigger": r.Trigger, "oldAgentId": r.OldAgentID, "oldRunId": r.OldRunID, "successorAgentId": r.SuccessorAgentID, "successorName": r.SuccessorName}
	if authorized {
		r.Authorization = &api.HandlerRotationAuthorization{AuthorizedBy: req.AuthorizedBy, Reason: req.AuthorizationReason}
		payload["authorizedBy"], payload["authorizationReason"] = req.AuthorizedBy, req.AuthorizationReason
	}
	evidenceJSON, lastActivityText, exitJSON := "", "", ""
	if dead {
		data, _ := json.Marshal(req.DeathEvidence)
		evidenceJSON, lastActivityText = string(data), ts(lastActivity)
		r.DeathEvidence, r.LastActivityAt = req.DeathEvidence, &lastActivity
		payload["deathEvidence"], payload["lastActivityAt"] = req.DeathEvidence, lastActivityText
	}
	if exit != nil {
		data, _ := json.Marshal(exit)
		exitJSON, r.Exit, payload["exit"] = string(data), exit, exit
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO handler_rotations(id,task_id,request_id,payload_hash,state,reason,trigger_kind,handler_revision,old_agent_id,old_run_id,old_name,successor_agent_id,successor_name,created_at,updated_at,authorized_by,authorization_reason,evidence_json,last_activity_at,exit_json)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.TaskID, r.RequestID, requestHash(req), r.State, r.Reason, r.Trigger, r.HandlerRevision, r.OldAgentID, r.OldRunID, r.OldName,
		r.SuccessorAgentID, r.SuccessorName, ts(now), ts(now), req.AuthorizedBy, req.AuthorizationReason, evidenceJSON, lastActivityText, exitJSON); err != nil {
		return zero, err
	}
	if _, err = s.insertEvent(ctx, tx, t.ID, "task_updated", old.ID, "Handler rotation prepared", payload, by); err != nil {
		return zero, err
	}
	return r, nil
}

// authorizedRotationChange names what an authorized rotation changed, such as
// " from runtime codex to claude and model gpt-6.1-sol to claude-sonnet-5-5":
// each of runtime, model and reasoning only when it differs, and the model or
// reasoning only when both runs recorded it. Empty when none differs.
func authorizedRotationChange(ctx context.Context, tx *sql.Tx, old, successor api.Agent) (string, error) {
	oldRec, err := loadHandlerRunRecord(ctx, tx, old.ID, old.RunID)
	if err != nil {
		return "", err
	}
	next, err := loadHandlerRunRecord(ctx, tx, successor.ID, successor.RunID)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, f := range []struct{ name, from, to string }{{"runtime", old.Runtime, successor.Runtime}, {"model", oldRec.model, next.model}, {"reasoning", oldRec.reasoning, next.reasoning}} {
		if f.from != f.to && f.from != "" && f.to != "" {
			parts = append(parts, f.name+" "+f.from+" to "+f.to)
		}
	}
	switch len(parts) {
	case 0:
		return "", nil
	case 1:
		return " from " + parts[0], nil
	}
	return " from " + strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1], nil
}

func (s *Store) commitHandlerRotation(ctx context.Context, tx *sql.Tx, t api.Task, req api.HandlerRotationRequest, by api.Caller) (api.HandlerRotation, error) {
	var zero api.HandlerRotation
	if req.RotationID == "" {
		return zero, api.ErrInvalid
	}
	r, err := loadHandlerRotation(ctx, tx, t.ID, req.RotationID)
	if err != nil {
		return zero, err
	}
	if r.State == api.HandlerRotationCommitted {
		return r, nil
	}
	if r.State != api.HandlerRotationPrepared {
		return zero, workItemConflict("handler rotation " + r.ID + " was aborted")
	}
	dead := r.Reason == api.HandlerRotationReasonDeadPrimary
	if !dead && req.DeathEvidence != nil {
		return zero, api.ErrInvalid
	}
	if t.PauseState != api.ProjectPauseActive {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedPaused, Detail: "the project is paused"}
	}
	old, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=?`, t.ID, r.OldAgentID))
	if err != nil {
		return zero, err
	}
	// Only a rotation prepared on a saved exit report may replace an exited
	// handler, and it replaces nothing else: a restart changes the status.
	if old.RunID != r.OldRunID || old.Status == api.AgentClosed || (old.Status == api.AgentExited) != (r.Exit != nil) || t.HandlerRevision != r.HandlerRevision {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedStaleHandler, Detail: "the old handler run changed since prepare; abort this rotation"}
	}
	busy, err := loadHandlerBusy(ctx, tx, t.ID, old)
	if err != nil {
		return zero, err
	}
	var deadPolicy api.HandlerRotationPolicy
	if dead {
		// A second, later observation from the host, and nothing recorded from
		// the run since prepare; otherwise the rotation stays prepared and the
		// runner aborts it.
		if deadPolicy, err = deadPrimaryCommitCheck(ctx, tx, t, old, r, req.DeathEvidence, s.now()); err != nil {
			return zero, err
		}
	} else if refusal := busy.refusal(r.Trigger); refusal != nil {
		// A busy primary is a wait only for a successor that is ready. One
		// that is not is told so, so the host's readiness bound still counts
		// while the primary works (bug wi_e09e377485f7f21a).
		if successor, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=?`, t.ID, r.SuccessorAgentID)); err == nil {
			if ready, err := successorReady(ctx, tx, t.ID, successor, r.CreatedAt); err != nil {
				return zero, err
			} else if !ready {
				return zero, successorNotReadyRefusal(successor, old)
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return zero, err
		}
		return zero, refusal
	}
	successor, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=?`, t.ID, r.SuccessorAgentID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	// Only the authorization saved at prepare lets the runtime, model or arm
	// change; host and directory always match.
	authorized := r.Authorization != nil
	if err != nil || successor.Role != api.AgentRoleDatabaseHandler || successor.Name != r.SuccessorName || !successor.Online ||
		successor.Status == api.AgentRetired || successor.Host != old.Host || successor.Cwd != old.Cwd || (successor.Runtime != old.Runtime && !authorized) {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedSuccessor, Detail: "the successor must be a registered, online database handler on the old handler's host, runtime and directory; only an owner-authorized change (tt handler rotate --authorized-by NAME --authorization-reason TEXT) may change the runtime"}
	}
	if !authorized {
		if refusal, err := handlerRotationArmRefusal(ctx, tx, old, successor); err != nil {
			return zero, err
		} else if refusal != nil {
			return zero, refusal
		}
	}
	// Online is not readiness: a successor whose first turn fails at its
	// provider would hold every obligation, unbound and unwakeable (bug
	// wi_ce7310979bc45479). Nothing moves until its exact run has completed a
	// turn with a model reply, or posted a message, since the prepare; the old
	// handler keeps all.
	if ready, err := successorReady(ctx, tx, t.ID, successor, r.CreatedAt); err != nil {
		return zero, err
	} else if !ready {
		return zero, successorNotReadyRefusal(successor, old)
	}
	handoff, err := s.snapshotHandlerHandoff(ctx, tx, t, old, busy)
	if err != nil {
		return zero, err
	}
	// Move every open obligation the old handler holds, by role or by name,
	// before it closes: a closed recipient's obligations would otherwise be
	// closed recipient_gone by the broker.
	rows, err := tx.QueryContext(ctx, `SELECT `+obligationCols+` FROM obligations WHERE task_id=? AND agent_id=? AND state<>? ORDER BY message_seq,id`, t.ID, old.ID, api.ObligationClosed)
	if err != nil {
		return zero, err
	}
	var open []api.Obligation
	for rows.Next() {
		o, err := scanObligation(rows)
		if err != nil {
			rows.Close()
			return zero, err
		}
		open = append(open, o)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return zero, err
	}
	if r.Exit != nil {
		// The broker closes an exited recipient's obligations within one tick,
		// long before the silence is met, so those are handed over too.
		closed, err := exitedHandlerClosedObligations(ctx, tx, t.ID, old, *r.Exit)
		if err != nil {
			return zero, err
		}
		open = append(open, closed...)
		sort.SliceStable(open, func(i, j int) bool { return open[i].MessageSeq < open[j].MessageSeq })
	}
	if dead {
		if handoff.InFlight, err = snapshotDeadInFlight(ctx, tx, t, old, busy, *r.LastActivityAt, deadPolicy.DeadSilenceMinutes, open); err != nil {
			return zero, err
		}
		if handoff.LiveLeases, err = s.moveDeadHandlerLeases(ctx, tx, t, old, successor, busy.leases, s.now()); err != nil {
			return zero, err
		}
	}
	for _, o := range open {
		if o.State == api.ObligationClosed {
			// Closed by the broker after the exit: the sender may already
			// have re-sent it, and the successor would then hold it twice.
			resent, reason, err := resentAfterClose(ctx, tx, t.ID, o, successor)
			if err != nil {
				return zero, err
			}
			if reason != "" {
				handoff.Skipped = append(handoff.Skipped, api.HandlerRotationSkip{OldObligationID: o.ID, OldMessageSeq: o.MessageSeq, NewMessageSeq: resent.Seq, Reason: reason, Subject: o.Subject})
				continue
			}
		}
		var viaRole string
		if err = tx.QueryRowContext(ctx, `SELECT via_role FROM obligations WHERE id=?`, o.ID).Scan(&viaRole); err != nil {
			return zero, err
		}
		m, err := s.reissueObligation(ctx, tx, t, o, successor, "handler rotation", "rotation "+r.ID+" moved "+old.Name+" to "+successor.Name, true)
		if err != nil {
			return zero, err
		}
		pair := api.HandlerRotationReissue{OldObligationID: o.ID, OldMessageSeq: o.MessageSeq, OldState: o.State, NewMessageSeq: m.Seq, ViaRole: viaRole, Subject: o.Subject}
		if err = tx.QueryRowContext(ctx, `SELECT id FROM obligations WHERE message_seq=? AND agent_id=?`, m.Seq, successor.ID).Scan(&pair.NewObligationID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return zero, err
		}
		handoff.Reissued = append(handoff.Reissued, pair)
	}
	t.PrimaryHandlerID = successor.ID
	t.HandlerRevision++
	if _, err = tx.ExecContext(ctx, `UPDATE tasks SET primary_handler_id=?,handler_revision=? WHERE id=?`, t.PrimaryHandlerID, t.HandlerRevision, t.ID); err != nil {
		return zero, err
	}
	text := fmt.Sprintf("You are now this project's primary Database handler, replacing %s (%s / %s) by handler rotation %s. %d open obligation(s) moved to you. Run tt handler rotation get %s for the durable handoff: moved obligations, pending scope confirmations, requests and allocation intents the old handler authored, claimed Queue entries and legacy required deliveries. Continue from that record; do not repeat work it shows as done.",
		old.Name, old.ID, old.RunID, r.ID, len(handoff.Reissued), r.ID)
	if authorized {
		change, err := authorizedRotationChange(ctx, tx, old, successor)
		if err != nil {
			return zero, err
		}
		text += fmt.Sprintf(" This change%s was authorized by %s: %s", change, r.Authorization.AuthorizedBy, r.Authorization.Reason)
	}
	subject, fallback := "Handler rotation handoff to "+successor.Name, "Handler rotation handoff"
	if dead {
		// One notice, in place of the ordinary handoff: the successor's copy
		// here and the owner helper's below carry the same text.
		subject, fallback = deadPrimaryNoticeSubject, deadPrimaryNoticeSubject
		text = deadPrimaryNoticeText(t, r, old, successor, req.DeathEvidence, handoff, deadPolicy.Revision)
	}
	if err = s.postBrokerNotice(ctx, tx, t, successor, successor.Name, subject, fallback, text, map[string]string{"handlerRotation": r.ID}); err != nil {
		return zero, err
	}
	var noticeSeq, ownerNoticeSeq int64
	if err = tx.QueryRowContext(ctx, `SELECT max(seq) FROM messages WHERE task_id=? AND to_agent=?`, t.ID, successor.ID).Scan(&noticeSeq); err != nil {
		return zero, err
	}
	providerBlocked := r.Reason == api.HandlerRotationReasonProviderBlocked
	if providerBlocked {
		// The owner helper is told once, in this transaction: a replayed
		// commit returns the saved record above and posts nothing.
		helper, err := openOwnerHelper(ctx, tx, t.ID)
		if err != nil {
			return zero, err
		}
		text, err := providerBlockedReplacedText(ctx, tx, t, r, old, successor, handoff)
		if err != nil {
			return zero, err
		}
		refs := map[string]string{"handlerRotation": r.ID, "cause": "provider-blocked-handler"}
		if err = s.postBrokerNotice(ctx, tx, t, helper, successor.Name, providerBlockedReplacedSubject, providerBlockedReplacedSubject, text, refs); err != nil {
			return zero, err
		}
		if err = tx.QueryRowContext(ctx, `SELECT max(seq) FROM messages WHERE task_id=?`, t.ID).Scan(&ownerNoticeSeq); err != nil {
			return zero, err
		}
	}
	if dead {
		// The owner helper's copy; with no open owner helper it is board-wide.
		helper, err := openOwnerHelper(ctx, tx, t.ID)
		if err != nil {
			return zero, err
		}
		refs := map[string]string{"handlerRotation": r.ID, "escalation": "owner", "cause": "dead-primary-handler"}
		if err = s.postBrokerNotice(ctx, tx, t, helper, successor.Name, subject, fallback, text, refs); err != nil {
			return zero, err
		}
		if err = tx.QueryRowContext(ctx, `SELECT max(seq) FROM messages WHERE task_id=?`, t.ID).Scan(&ownerNoticeSeq); err != nil {
			return zero, err
		}
	}
	now := s.now()
	if _, err = tx.ExecContext(ctx, `UPDATE agents SET status=?,last_event_at=?,blocked_reason='',blocked_text='' WHERE id=?`, api.AgentClosed, ts(now), old.ID); err != nil {
		return zero, err
	}
	if _, err = s.insertEvent(ctx, tx, t.ID, api.EventClosed, old.ID, "", map[string]any{"handlerRotationId": r.ID, "successorAgentId": successor.ID}, by); err != nil {
		return zero, err
	}
	r.State, r.SuccessorRunID, r.UpdatedAt, r.Handoff = api.HandlerRotationCommitted, successor.RunID, now, &handoff
	r.Receipt = &api.HandlerRotationReceipt{RotationID: r.ID, RequestID: req.RequestID, HandlerRevision: t.HandlerRevision, PrimaryHandlerID: successor.ID,
		ClosedAgentID: old.ID, ClosedRunID: old.RunID, NoticeSeq: noticeSeq, Reissued: len(handoff.Reissued), CommittedAt: now}
	commitEvidenceJSON := ""
	if dead {
		data, _ := json.Marshal(req.DeathEvidence)
		commitEvidenceJSON, r.CommitDeathEvidence = string(data), req.DeathEvidence
		r.Receipt.LeasesMoved, r.Receipt.OwnerNoticeSeq = len(handoff.LiveLeases), ownerNoticeSeq
	}
	if providerBlocked {
		r.Receipt.OwnerNoticeSeq = ownerNoticeSeq
	}
	handoffJSON, _ := json.Marshal(handoff)
	receiptJSON, _ := json.Marshal(r.Receipt)
	if _, err = tx.ExecContext(ctx, `UPDATE handler_rotations SET state=?,successor_run_id=?,handoff_json=?,receipt_json=?,updated_at=?,old_runtime=?,successor_runtime=?,commit_evidence_json=? WHERE id=? AND state='prepared'`,
		r.State, r.SuccessorRunID, string(handoffJSON), string(receiptJSON), ts(now), old.Runtime, successor.Runtime, commitEvidenceJSON, r.ID); err != nil {
		return zero, err
	}
	payload := map[string]any{"handlerRotationId": r.ID, "state": r.State,
		"oldAgentId": old.ID, "oldRunId": old.RunID, "successorAgentId": successor.ID, "successorRunId": successor.RunID, "handlerRevision": t.HandlerRevision,
		"reissued": len(handoff.Reissued), "noticeSeq": noticeSeq, "oldRuntime": old.Runtime, "successorRuntime": successor.Runtime}
	if len(handoff.Skipped) > 0 {
		payload["skipped"] = len(handoff.Skipped)
	}
	if dead {
		payload["reason"], payload["leasesMoved"], payload["ownerNoticeSeq"], payload["commitDeathEvidence"] = r.Reason, len(handoff.LiveLeases), ownerNoticeSeq, req.DeathEvidence
	}
	if providerBlocked {
		payload["reason"], payload["ownerNoticeSeq"] = r.Reason, ownerNoticeSeq
	}
	if authorized {
		r.Authorization.OldRuntime, r.Authorization.SuccessorRuntime = old.Runtime, successor.Runtime
		payload["authorizedBy"], payload["authorizationReason"] = r.Authorization.AuthorizedBy, r.Authorization.Reason
	}
	if _, err = s.insertEvent(ctx, tx, t.ID, "task_updated", successor.ID, "Handler rotation committed", payload, by); err != nil {
		return zero, err
	}
	return r, nil
}

// blockedPrimary is a primary handler's provider block that makes it due for
// replacement (bug wi_ce7310979bc45479, order #46760, q2).
type blockedPrimary struct {
	class       string
	since       time.Time
	openedAt    time.Time
	obligations int
}

// providerBlockedPrimary reports whether primary a, on its current run, is due
// for a provider_blocked replacement: the policy's dead primary replacement is
// on, the run is online and idle for the runner, it has an open rate_limited
// or server_error episode that opened at least api.HandlerProviderBlockedAfter
// ago, and it holds an obligation that needs more than delivery.
func providerBlockedPrimary(ctx context.Context, q queryRower, task string, policy api.HandlerRotationPolicy, a api.Agent, busy handlerBusy, now time.Time) (blockedPrimary, bool, error) {
	var b blockedPrimary
	if policy.DeadSilenceMinutes <= 0 || !handlerSeenWithin(a, now) || !busy.idle() || a.Status == api.AgentClosed || a.Status == api.AgentExited {
		return b, false, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT class,since,opened_at FROM provider_block_episodes WHERE task_id=? AND agent_id=? AND run_id=? AND cleared_at='' AND class IN (?,?)`,
		task, a.ID, a.RunID, api.ProviderBlockRateLimited, api.ProviderBlockServerError)
	if err != nil {
		return b, false, err
	}
	found := false
	for rows.Next() {
		var class, since, opened string
		if err := rows.Scan(&class, &since, &opened); err != nil {
			rows.Close()
			return b, false, err
		}
		if at := parseTS(opened); !found || at.Before(b.openedAt) {
			b, found = blockedPrimary{class: class, since: parseTS(since), openedAt: at}, true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || !found || now.Sub(b.openedAt) < api.HandlerProviderBlockedAfter {
		return b, false, err
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM obligations WHERE task_id=? AND agent_id=? AND state<>? AND needs<>?`, task, a.ID, api.ObligationClosed, api.ObligationNeedsDelivery).Scan(&b.obligations); err != nil {
		return b, false, err
	}
	return b, b.obligations > 0, nil
}

// successorReady reports whether the successor's exact current run is ready
// to take the obligations. Either an activity report observed at or after
// since shows a completed turn with a model reply (api.HandlerSuccessorReady),
// or the run posted a message at or after since while its current activity is
// not provider_blocked. A post is a model reply even when the relay never
// samples the turn idle (bug wi_e09e377485f7f21a). Receipts and messages are
// read by agent and run, and broker messages never count, so the old
// handler's activity, another agent's message and an earlier run of the
// successor never count.
func successorReady(ctx context.Context, q queryRower, task string, successor api.Agent, since time.Time) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT payload FROM agent_activity_receipts WHERE task_id=? AND agent_id=? AND run_id=?`, task, successor.ID, successor.RunID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return false, err
		}
		var a api.AgentActivity
		if json.Unmarshal([]byte(payload), &a) != nil {
			continue
		}
		if !a.ObservedAt.Before(since) && api.HandlerSuccessorReady(a) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	rows.Close()
	return successorPosted(ctx, q, task, successor, since)
}

func successorNotReadyRefusal(successor, old api.Agent) *api.HandlerRotationRefusal {
	return &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedSuccessorNotReady, Detail: fmt.Sprintf("successor %s has not completed a turn with a model reply or posted a message since the prepare; %s keeps its obligations until it does", successor.Name, old.Name)}
}

// successorPosted reports whether the successor's exact run posted a message
// at or after since, and its current activity is neither provider_blocked nor
// a turn a provider error ended: a run its provider refuses cannot take the
// obligations whatever it said earlier. The hub stamps messages in seq order,
// so the task's messages are read newest first by the (task_id, seq) index
// and the scan stops at the first one before since; a clock step back can only
// end it early, which leaves the successor not ready.
func successorPosted(ctx context.Context, q queryRower, task string, successor api.Agent, since time.Time) (bool, error) {
	var payload string
	err := q.QueryRowContext(ctx, `SELECT payload FROM agent_activity WHERE task_id=? AND agent_id=? AND run_id=?`, task, successor.ID, successor.RunID).Scan(&payload)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if payload != "" {
		var a api.AgentActivity
		if json.Unmarshal([]byte(payload), &a) != nil || a.State == "provider_blocked" || a.Provider != nil || strings.HasPrefix(a.Reason, api.ProviderErrorTurnReasonPrefix) {
			return false, nil
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT from_agent,from_run_id,from_node,created_at FROM messages WHERE task_id=? ORDER BY seq DESC`, task)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var agent, run, node, raw string
		if err := rows.Scan(&agent, &run, &node, &raw); err != nil {
			return false, err
		}
		if parseTS(raw).Before(since) {
			break
		}
		if agent == successor.ID && run == successor.RunID && node != api.BrokerNode {
			return true, nil
		}
	}
	return false, rows.Err()
}

// snapshotHandlerHandoff lists what the successor inherits besides the
// obligations it receives. Legacy required deliveries stay pinned to the old
// run: they are listed here, not moved.
func (s *Store) snapshotHandlerHandoff(ctx context.Context, tx *sql.Tx, t api.Task, old api.Agent, busy handlerBusy) (api.HandlerRotationHandoff, error) {
	h := api.HandlerRotationHandoff{Reissued: []api.HandlerRotationReissue{}, LiveLeases: busy.leases, PendingScopeConfirmations: []api.HandlerRotationScope{},
		AuthoredOpen: []api.HandlerRotationAuthored{}, AllocationIntents: []api.HandlerRotationIntent{}, QueueClaims: []api.HandlerRotationQueueClaim{},
		RequiredDeliveries: []api.HandlerRotationDelivery{}}
	collect := func(query string, args []any, scan func(*sql.Rows) error) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := collect(`SELECT e.id,e.item_id,e.item_revision,e.order_seq FROM team_queue_entries e WHERE e.task_id=? AND e.state='queued'
 AND NOT EXISTS (SELECT 1 FROM work_order_scope_confirmations c WHERE c.task_id=e.task_id AND c.item_id=e.item_id AND c.item_revision=e.item_revision AND c.order_seq=e.order_seq)
 ORDER BY e.position,e.id`, []any{t.ID}, func(rows *sql.Rows) error {
		var v api.HandlerRotationScope
		err := rows.Scan(&v.EntryID, &v.ItemID, &v.ItemRevision, &v.OrderSeq)
		h.PendingScopeConfirmations = append(h.PendingScopeConfirmations, v)
		return err
	}); err != nil {
		return h, err
	}
	if err := collect(`SELECT o.id,o.message_seq,o.agent_id,o.state,o.subject FROM obligations o JOIN messages m ON m.seq=o.message_seq
 WHERE o.task_id=? AND m.from_agent=? AND o.agent_id<>? AND o.state<>? ORDER BY o.message_seq,o.id`, []any{t.ID, old.ID, old.ID, api.ObligationClosed}, func(rows *sql.Rows) error {
		var v api.HandlerRotationAuthored
		err := rows.Scan(&v.ObligationID, &v.MessageSeq, &v.AgentID, &v.State, &v.Subject)
		h.AuthoredOpen = append(h.AuthoredOpen, v)
		return err
	}); err != nil {
		return h, err
	}
	if err := collect(`SELECT agent_id,item_task_id,item_id,item_revision,team_role FROM agent_allocation_intents
 WHERE author_agent_id=? AND author_run_id=? AND consumed_at='' AND invalidated_at='' ORDER BY created_at,agent_id`, []any{old.ID, old.RunID}, func(rows *sql.Rows) error {
		var v api.HandlerRotationIntent
		err := rows.Scan(&v.AgentID, &v.ItemTaskID, &v.ItemID, &v.ItemRevision, &v.TeamRole)
		h.AllocationIntents = append(h.AllocationIntents, v)
		return err
	}); err != nil {
		return h, err
	}
	if err := collect(`SELECT id,source_task_id,item_id,cycle,state FROM queue_entries WHERE target_task_id=? AND claimant_agent_id=? AND claimant_run_id=?
 AND state IN (?,?) ORDER BY seq`, []any{t.ID, old.ID, old.RunID, api.QueueStateClaimed, api.QueueStateActive}, func(rows *sql.Rows) error {
		var v api.HandlerRotationQueueClaim
		err := rows.Scan(&v.EntryID, &v.SourceTaskID, &v.ItemID, &v.Cycle, &v.State)
		h.QueueClaims = append(h.QueueClaims, v)
		return err
	}); err != nil {
		return h, err
	}
	if err := collect(`SELECT id,message_seq,kind,item_id,phase FROM required_deliveries WHERE task_id=? AND agent_id=? AND run_id=? AND current=1 ORDER BY created_at,id`,
		[]any{t.ID, old.ID, old.RunID}, func(rows *sql.Rows) error {
			var v api.HandlerRotationDelivery
			err := rows.Scan(&v.ID, &v.MessageSeq, &v.Kind, &v.ItemID, &v.Phase)
			h.RequiredDeliveries = append(h.RequiredDeliveries, v)
			return err
		}); err != nil {
		return h, err
	}
	return h, nil
}

func (s *Store) abortHandlerRotation(ctx context.Context, tx *sql.Tx, t api.Task, req api.HandlerRotationRequest, by api.Caller) (api.HandlerRotation, error) {
	var zero api.HandlerRotation
	if req.RotationID == "" {
		return zero, api.ErrInvalid
	}
	r, err := loadHandlerRotation(ctx, tx, t.ID, req.RotationID)
	if err != nil {
		return zero, err
	}
	if r.State == api.HandlerRotationAborted {
		return r, nil
	}
	if r.State != api.HandlerRotationPrepared {
		return zero, workItemConflict("handler rotation " + r.ID + " is committed and cannot be aborted")
	}
	now := s.now()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM agents WHERE task_id=? AND id=?`, t.ID, r.SuccessorAgentID).Scan(&status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	if err == nil && status != api.AgentClosed {
		if _, err = tx.ExecContext(ctx, `UPDATE agents SET status=?,last_event_at=?,blocked_reason='',blocked_text='' WHERE id=?`, api.AgentClosed, ts(now), r.SuccessorAgentID); err != nil {
			return zero, err
		}
		if _, err = s.insertEvent(ctx, tx, t.ID, api.EventClosed, r.SuccessorAgentID, "", map[string]any{"handlerRotationId": r.ID, "aborted": true}, by); err != nil {
			return zero, err
		}
	}
	r.State, r.UpdatedAt = api.HandlerRotationAborted, now
	if _, err = tx.ExecContext(ctx, `UPDATE handler_rotations SET state=?,updated_at=? WHERE id=? AND state='prepared'`, r.State, ts(now), r.ID); err != nil {
		return zero, err
	}
	if _, err = s.insertEvent(ctx, tx, t.ID, "task_updated", r.OldAgentID, "Handler rotation aborted", map[string]any{"handlerRotationId": r.ID, "state": r.State,
		"successorAgentId": r.SuccessorAgentID}, by); err != nil {
		return zero, err
	}
	return r, nil
}

// HandlerRotationsDue lists, for each open project whose primary handler runs
// on host, the counters the runner compares with the policy: a project whose
// limit policy is enabled, and any project whose primary is a dead candidate.
// A busy primary whose wrapper reported exited is listed as the primary too.
// templateDigest is the runner's current handler template. templateDigest is the runner's current handler template.
func (s *Store) HandlerRotationsDue(ctx context.Context, host, templateDigest string) (api.HandlerRotationDueList, error) {
	out := api.HandlerRotationDueList{Entries: []api.HandlerRotationDue{}}
	if host == "" || !api.ValidText(host, 253) || len(templateDigest) > 64 {
		return out, api.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE status=? AND pause_state=? AND EXISTS (SELECT 1 FROM agents a WHERE a.task_id=tasks.id AND a.role=? AND a.host=? AND a.status<>?) ORDER BY created_at,id`,
		api.TaskOpen, api.ProjectPauseActive, api.AgentRoleDatabaseHandler, host, api.AgentClosed)
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
		policy, err := loadHandlerRotationPolicy(ctx, s.db, t.ID)
		if err != nil {
			return out, err
		}
		d, listed, err := primaryRotationDue(ctx, s.db, t, policy, host, templateDigest, s.now())
		if err != nil {
			return out, err
		}
		if listed {
			out.Entries = append(out.Entries, d)
		}
	}
	return out, nil
}

// primaryRotationDue is one project's entry in the due list: listed is false
// where HandlerRotationsDue skips the project. An empty host lists a primary
// on any host. It reads only q, so queue admission can call it inside its
// transaction (the store has one connection) and hold a seat on the same
// rule (handlerSuccessorSeat).
func primaryRotationDue(ctx context.Context, q queryRower, t api.Task, policy api.HandlerRotationPolicy, host, templateDigest string, now time.Time) (api.HandlerRotationDue, bool, error) {
	// Listed for its limits when the limit policy is enabled, and for a
	// dead primary whenever that replacement is on; decided below.
	if !policy.Enabled && policy.DeadSilenceMinutes <= 0 {
		return api.HandlerRotationDue{}, false, nil
	}
	// A primary whose wrapper reported exited is listed only while it is
	// busy and the dead primary replacement is on; otherwise the project
	// is listed for its open primary as before.
	primary, exited, err := exitedPrimaryHandler(ctx, q, t)
	if err != nil {
		return api.HandlerRotationDue{}, false, err
	}
	// One primary's unreadable activity snapshot must not fail the list
	// for every project on the host. That project is listed as busy, with
	// the error as its activity, so the runner neither rotates nor
	// replaces it; prepare and commit still fail on the same snapshot.
	var busy handlerBusy
	var unreadable error
	if exited {
		if busy, err = loadHandlerBusy(ctx, q, t.ID, primary); unreadableActivity(err) {
			unreadable = err
		} else if err != nil {
			return api.HandlerRotationDue{}, false, err
		}
		exited = policy.DeadSilenceMinutes > 0 && (unreadable != nil || !busy.idle())
	}
	if !exited {
		primary, err = primaryHandler(ctx, q, t)
		if errors.Is(err, sql.ErrNoRows) {
			return api.HandlerRotationDue{}, false, nil
		}
		if err != nil {
			return api.HandlerRotationDue{}, false, err
		}
		unreadable = nil
		if busy, err = loadHandlerBusy(ctx, q, t.ID, primary); unreadableActivity(err) {
			unreadable = err
		} else if err != nil {
			return api.HandlerRotationDue{}, false, err
		}
	}
	if host != "" && primary.Host != host {
		return api.HandlerRotationDue{}, false, nil
	}
	d := api.HandlerRotationDue{TaskID: t.ID, HandlerRevision: t.HandlerRevision, Agent: primary, Policy: policy, DueReasons: []string{}}
	if err = q.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_entries WHERE task_id=? AND handler_id=? AND handler_run_id=? AND state='finished'`, t.ID, primary.ID, primary.RunID).Scan(&d.FinishedItems); err != nil {
		return api.HandlerRotationDue{}, false, err
	}
	d.ActivityState, d.PendingTool, d.LiveLeases, d.Idle = busy.state, busy.pendingTool, len(busy.leases), unreadable == nil && busy.idle()
	d.Online = handlerSeenWithin(primary, now)
	d.DeadCandidate = policy.DeadSilenceMinutes > 0 && !d.Online && !d.Idle && unreadable == nil
	// A provider-blocked primary is replaced with the limit policy off
	// too, and its prepared rotation stays listed so the runner can
	// resume it after the block clears.
	blocked := false
	if !exited && unreadable == nil {
		if _, blocked, err = providerBlockedPrimary(ctx, q, t.ID, policy, primary, busy, now); err != nil {
			return api.HandlerRotationDue{}, false, err
		}
	}
	if d.OpenRotation, err = openHandlerRotation(ctx, q, t.ID); err != nil {
		return api.HandlerRotationDue{}, false, err
	}
	blockedRotation := d.OpenRotation != nil && d.OpenRotation.Reason == api.HandlerRotationReasonProviderBlocked
	if !policy.Enabled && !d.DeadCandidate && !blocked && !blockedRotation && unreadable == nil {
		return api.HandlerRotationDue{}, false, nil
	}
	if !d.Online {
		// An exited run's silence starts at its wrapper's exit report; a
		// report the hub cannot use leaves the decision to prepare.
		var exit *api.HandlerRotationExit
		if exited {
			var refusal *api.HandlerRotationRefusal
			if exit, err = wrapperExitReport(ctx, q, t.ID, primary); err != nil && !errors.As(err, &refusal) {
				return api.HandlerRotationDue{}, false, err
			}
		}
		last, ok, err := handlerSilentSince(ctx, q, t.ID, primary, exit)
		if err != nil {
			return api.HandlerRotationDue{}, false, err
		}
		if ok {
			d.LastActivityAt = &last
			d.SilenceMet = d.DeadCandidate && now.Sub(last) >= time.Duration(policy.DeadSilenceMinutes)*time.Minute
		}
	}
	if err = loadHandlerActivity(ctx, q, &d.Agent); unreadableActivity(err) {
		if unreadable == nil {
			unreadable = err
		}
	} else if err != nil {
		return api.HandlerRotationDue{}, false, err
	}
	if d.Agent.Activity != nil {
		d.TotalTokens = d.Agent.Activity.Tokens.Total
	}
	err = q.QueryRowContext(ctx, `SELECT template_digest FROM handler_runs WHERE run_id=? AND agent_id=?`, primary.RunID, primary.ID).Scan(&d.RecordedDigest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return api.HandlerRotationDue{}, false, err
	}
	d.DigestMatches = templateDigest != "" && d.RecordedDigest == templateDigest
	if hold, err := successorHoldRow(ctx, q, t.ID); err != nil {
		return api.HandlerRotationDue{}, false, err
	} else if hold != nil && hold.OldAgentID == primary.ID && hold.OldRunID == primary.RunID {
		d.SuccessorHold = hold
	}
	// The limits are due only under an enabled limit policy.
	if policy.Enabled && policy.MaxItems > 0 && d.FinishedItems >= policy.MaxItems {
		d.DueReasons = append(d.DueReasons, api.HandlerRotationReasonItems)
	}
	if policy.Enabled && policy.MaxTotalTokens > 0 && d.TotalTokens >= policy.MaxTotalTokens {
		d.DueReasons = append(d.DueReasons, api.HandlerRotationReasonTokens)
	}
	if policy.Enabled && policy.OnTemplateChange && templateDigest != "" && !d.DigestMatches {
		d.DueReasons = append(d.DueReasons, api.HandlerRotationReasonTemplate)
	}
	if blocked {
		d.DueReasons = append(d.DueReasons, api.HandlerRotationReasonProviderBlocked)
	}
	if unreadable != nil {
		d.ActivityState = "unreadable: " + unreadable.Error()
		d.Idle, d.DeadCandidate, d.SilenceMet, d.DueReasons = false, false, false, []string{}
	}
	return d, true, nil
}

// loadHandlerActivity is Store.loadActivity read through q.
func loadHandlerActivity(ctx context.Context, q queryRower, a *api.Agent) error {
	var payload string
	err := q.QueryRowContext(ctx, `SELECT payload FROM agent_activity WHERE agent_id=? AND run_id=?`, a.ID, a.RunID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var activity api.AgentActivity
	if err := json.Unmarshal([]byte(payload), &activity); err != nil {
		return fmt.Errorf("activity snapshot: %w", err)
	}
	a.Activity = &activity
	return nil
}
