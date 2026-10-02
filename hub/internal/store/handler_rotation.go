package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
 task_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT PRIMARY KEY, template_digest TEXT NOT NULL, created_at TEXT NOT NULL);`); err != nil {
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
		MaxTotalTokens: api.DefaultHandlerRotationMaxTotalTokens, OnTemplateChange: true}
}

func loadHandlerRotationPolicy(ctx context.Context, q queryRower, task string) (api.HandlerRotationPolicy, error) {
	p := defaultHandlerRotationPolicy(task)
	var updated string
	err := q.QueryRowContext(ctx, `SELECT enabled,max_items,max_total_tokens,on_template_change,revision,updated_at FROM handler_rotation_policy WHERE task_id=?`, task).
		Scan(&p.Enabled, &p.MaxItems, &p.MaxTotalTokens, &p.OnTemplateChange, &p.Revision, &updated)
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
	if req.ExpectedRevision < 0 || req.MaxItems < 0 || req.MaxTotalTokens < 0 {
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO handler_rotation_policy(task_id,enabled,max_items,max_total_tokens,on_template_change,revision,updated_at) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(task_id) DO UPDATE SET enabled=excluded.enabled,max_items=excluded.max_items,max_total_tokens=excluded.max_total_tokens,on_template_change=excluded.on_template_change,revision=excluded.revision,updated_at=excluded.updated_at`,
		task, req.Enabled, req.MaxItems, req.MaxTotalTokens, req.OnTemplateChange, current.Revision+1, ts(now)); err != nil {
		return zero, err
	}
	out := api.HandlerRotationPolicy{TaskID: task, Enabled: req.Enabled, MaxItems: req.MaxItems, MaxTotalTokens: req.MaxTotalTokens, OnTemplateChange: req.OnTemplateChange, Revision: current.Revision + 1, UpdatedAt: &now}
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
	return scanAgent(q.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND role=? AND status NOT IN (?,?)
 AND id NOT IN (SELECT successor_agent_id FROM handler_rotations WHERE task_id=? AND state='prepared') ORDER BY created_at,id LIMIT 1`,
		task.ID, api.AgentRoleDatabaseHandler, api.AgentClosed, api.AgentExited, task.ID))
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

// recordHandlerRun keeps the prompt template digest, model and reasoning of
// an exact handler run. The first record for a run wins, so a retried launch
// cannot rewrite it.
func (s *Store) recordHandlerRun(ctx context.Context, a api.Agent, digest, model, reasoning string) error {
	if a.Role != api.AgentRoleDatabaseHandler || (digest == "" && model == "" && reasoning == "") || a.RunID == "" {
		return nil
	}
	if digest != "" && (len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "") {
		return api.ErrInvalid
	}
	if !validArmField(model) || !validArmField(reasoning) {
		return api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO handler_runs(task_id,agent_id,run_id,template_digest,model,reasoning,created_at) VALUES(?,?,?,?,?,?,?)`, a.TaskID, a.ID, a.RunID, digest, model, reasoning, ts(s.now()))
	return err
}

const handlerRotationCols = `id,task_id,request_id,state,reason,trigger_kind,handler_revision,old_agent_id,old_run_id,old_name,successor_agent_id,successor_name,successor_run_id,handoff_json,receipt_json,created_at,updated_at,authorized_by,authorization_reason,old_runtime,successor_runtime`

func scanHandlerRotation(row interface{ Scan(...any) error }) (api.HandlerRotation, error) {
	var r api.HandlerRotation
	var handoff, receipt, created, updated string
	var auth api.HandlerRotationAuthorization
	if err := row.Scan(&r.ID, &r.TaskID, &r.RequestID, &r.State, &r.Reason, &r.Trigger, &r.HandlerRevision, &r.OldAgentID, &r.OldRunID, &r.OldName,
		&r.SuccessorAgentID, &r.SuccessorName, &r.SuccessorRunID, &handoff, &receipt, &created, &updated,
		&auth.AuthorizedBy, &auth.Reason, &auth.OldRuntime, &auth.SuccessorRuntime); err != nil {
		return r, err
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
	case "idle", "finished_silent":
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
	hash := requestHash(req)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
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
	case api.HandlerRotationReasonManual, api.HandlerRotationReasonItems, api.HandlerRotationReasonTokens, api.HandlerRotationReasonTemplate:
	default:
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
	if refusal := busy.refusal(req.Trigger); refusal != nil {
		return zero, refusal
	}
	now := s.now()
	r := api.HandlerRotation{ID: api.NewID("hrot"), TaskID: t.ID, RequestID: req.RequestID, State: api.HandlerRotationPrepared, Reason: req.Reason, Trigger: req.Trigger,
		HandlerRevision: t.HandlerRevision, OldAgentID: old.ID, OldRunID: old.RunID, OldName: old.Name, SuccessorAgentID: req.SuccessorAgentID,
		SuccessorName: req.SuccessorName, CreatedAt: now, UpdatedAt: now}
	payload := map[string]any{"handlerRotationId": r.ID, "state": r.State, "reason": r.Reason,
		"trigger": r.Trigger, "oldAgentId": r.OldAgentID, "oldRunId": r.OldRunID, "successorAgentId": r.SuccessorAgentID, "successorName": r.SuccessorName}
	if authorized {
		r.Authorization = &api.HandlerRotationAuthorization{AuthorizedBy: req.AuthorizedBy, Reason: req.AuthorizationReason}
		payload["authorizedBy"], payload["authorizationReason"] = req.AuthorizedBy, req.AuthorizationReason
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO handler_rotations(id,task_id,request_id,payload_hash,state,reason,trigger_kind,handler_revision,old_agent_id,old_run_id,old_name,successor_agent_id,successor_name,created_at,updated_at,authorized_by,authorization_reason)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.TaskID, r.RequestID, requestHash(req), r.State, r.Reason, r.Trigger, r.HandlerRevision, r.OldAgentID, r.OldRunID, r.OldName,
		r.SuccessorAgentID, r.SuccessorName, ts(now), ts(now), req.AuthorizedBy, req.AuthorizationReason); err != nil {
		return zero, err
	}
	if _, err = s.insertEvent(ctx, tx, t.ID, "task_updated", old.ID, "Handler rotation prepared", payload, by); err != nil {
		return zero, err
	}
	return r, nil
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
	if t.PauseState != api.ProjectPauseActive {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedPaused, Detail: "the project is paused"}
	}
	old, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=?`, t.ID, r.OldAgentID))
	if err != nil {
		return zero, err
	}
	if old.RunID != r.OldRunID || old.Status == api.AgentClosed || old.Status == api.AgentExited || t.HandlerRevision != r.HandlerRevision {
		return zero, &api.HandlerRotationRefusal{Code: api.HandlerRotationRefusedStaleHandler, Detail: "the old handler run changed since prepare; abort this rotation"}
	}
	busy, err := loadHandlerBusy(ctx, tx, t.ID, old)
	if err != nil {
		return zero, err
	}
	if refusal := busy.refusal(r.Trigger); refusal != nil {
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
	for _, o := range open {
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
		text += fmt.Sprintf(" This change from runtime %s to %s was authorized by %s: %s", old.Runtime, successor.Runtime, r.Authorization.AuthorizedBy, r.Authorization.Reason)
	}
	if err = s.postBrokerNotice(ctx, tx, t, successor, successor.Name, "Handler rotation handoff to "+successor.Name, "Handler rotation handoff", text, map[string]string{"handlerRotation": r.ID}); err != nil {
		return zero, err
	}
	var noticeSeq int64
	if err = tx.QueryRowContext(ctx, `SELECT max(seq) FROM messages WHERE task_id=? AND to_agent=?`, t.ID, successor.ID).Scan(&noticeSeq); err != nil {
		return zero, err
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
	handoffJSON, _ := json.Marshal(handoff)
	receiptJSON, _ := json.Marshal(r.Receipt)
	if _, err = tx.ExecContext(ctx, `UPDATE handler_rotations SET state=?,successor_run_id=?,handoff_json=?,receipt_json=?,updated_at=?,old_runtime=?,successor_runtime=? WHERE id=? AND state='prepared'`,
		r.State, r.SuccessorRunID, string(handoffJSON), string(receiptJSON), ts(now), old.Runtime, successor.Runtime, r.ID); err != nil {
		return zero, err
	}
	payload := map[string]any{"handlerRotationId": r.ID, "state": r.State,
		"oldAgentId": old.ID, "oldRunId": old.RunID, "successorAgentId": successor.ID, "successorRunId": successor.RunID, "handlerRevision": t.HandlerRevision,
		"reissued": len(handoff.Reissued), "noticeSeq": noticeSeq, "oldRuntime": old.Runtime, "successorRuntime": successor.Runtime}
	if authorized {
		r.Authorization.OldRuntime, r.Authorization.SuccessorRuntime = old.Runtime, successor.Runtime
		payload["authorizedBy"], payload["authorizationReason"] = r.Authorization.AuthorizedBy, r.Authorization.Reason
	}
	if _, err = s.insertEvent(ctx, tx, t.ID, "task_updated", successor.ID, "Handler rotation committed", payload, by); err != nil {
		return zero, err
	}
	return r, nil
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

// HandlerRotationsDue lists, for each open project whose policy is enabled
// and whose primary handler runs on host, the counters the runner compares
// with the policy. templateDigest is the runner's current handler template.
func (s *Store) HandlerRotationsDue(ctx context.Context, host, templateDigest string) (api.HandlerRotationDueList, error) {
	out := api.HandlerRotationDueList{Entries: []api.HandlerRotationDue{}}
	if host == "" || !api.ValidText(host, 253) || len(templateDigest) > 64 {
		return out, api.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE status=? AND pause_state=? AND EXISTS (SELECT 1 FROM agents a WHERE a.task_id=tasks.id AND a.role=? AND a.host=? AND a.status NOT IN (?,?)) ORDER BY created_at,id`,
		api.TaskOpen, api.ProjectPauseActive, api.AgentRoleDatabaseHandler, host, api.AgentClosed, api.AgentExited)
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
		if !policy.Enabled {
			continue
		}
		primary, err := primaryHandler(ctx, s.db, t)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return out, err
		}
		if primary.Host != host {
			continue
		}
		d := api.HandlerRotationDue{TaskID: t.ID, HandlerRevision: t.HandlerRevision, Agent: primary, Policy: policy, DueReasons: []string{}}
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_entries WHERE task_id=? AND handler_id=? AND handler_run_id=? AND state='finished'`, t.ID, primary.ID, primary.RunID).Scan(&d.FinishedItems); err != nil {
			return out, err
		}
		busy, err := loadHandlerBusy(ctx, s.db, t.ID, primary)
		if err != nil {
			return out, err
		}
		d.ActivityState, d.PendingTool, d.LiveLeases, d.Idle = busy.state, busy.pendingTool, len(busy.leases), busy.idle()
		if err = s.loadActivity(ctx, &d.Agent); err != nil {
			return out, err
		}
		if d.Agent.Activity != nil {
			d.TotalTokens = d.Agent.Activity.Tokens.Total
		}
		err = s.db.QueryRowContext(ctx, `SELECT template_digest FROM handler_runs WHERE run_id=? AND agent_id=?`, primary.RunID, primary.ID).Scan(&d.RecordedDigest)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		d.DigestMatches = templateDigest != "" && d.RecordedDigest == templateDigest
		if policy.MaxItems > 0 && d.FinishedItems >= policy.MaxItems {
			d.DueReasons = append(d.DueReasons, api.HandlerRotationReasonItems)
		}
		if policy.MaxTotalTokens > 0 && d.TotalTokens >= policy.MaxTotalTokens {
			d.DueReasons = append(d.DueReasons, api.HandlerRotationReasonTokens)
		}
		if policy.OnTemplateChange && templateDigest != "" && !d.DigestMatches {
			d.DueReasons = append(d.DueReasons, api.HandlerRotationReasonTemplate)
		}
		if d.OpenRotation, err = openHandlerRotation(ctx, s.db, t.ID); err != nil {
			return out, err
		}
		out.Entries = append(out.Entries, d)
	}
	return out, nil
}
