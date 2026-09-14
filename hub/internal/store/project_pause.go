package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

type pauseQuerier interface {
	queryRower
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type pendingResumeAdmission struct {
	CycleID      string
	ReceiptID    string
	AgentID      string
	RunID        string
	Name         string
	SelectedTeam string
}

func loadPendingResumeAdmission(ctx context.Context, q queryRower, taskID string, pauseGeneration int64) (pendingResumeAdmission, error) {
	var out pendingResumeAdmission
	err := q.QueryRowContext(ctx, `SELECT id,resume_receipt_id,resume_agent_id,resume_run_id,resume_name,selected_team_id FROM project_pause_cycles WHERE task_id=? AND pause_generation=? AND state=?`, taskID, pauseGeneration, api.ProjectPauseResuming).Scan(
		&out.CycleID, &out.ReceiptID, &out.AgentID, &out.RunID, &out.Name, &out.SelectedTeam)
	return out, err
}

func validServiceDisposition(value string) bool {
	switch value {
	case api.PauseServiceNone, api.PauseServiceTransferred, api.PauseServiceDetached, api.PauseServiceUnresolved:
		return true
	default:
		return false
	}
}

func validPauseRequest(req api.PauseProjectRequest) bool {
	if req.Version != api.ProjectPauseCapabilityVersion || !validRequestID(req.RequestID) || req.ExpectedLifecycleGeneration < 0 || !api.ValidText(req.PreviousTeamID, 128) || len(req.Targets) > api.MaxProjectPauseTargets {
		return false
	}
	seen := map[string]bool{}
	for _, target := range req.Targets {
		key := target.AgentID + ":" + target.RunID
		if !api.ValidID(target.AgentID, "agt") || !validRunID(target.RunID) || seen[key] || !validServiceDisposition(target.ServiceDisposition) || !api.ValidText(target.HandoffNote, api.MaxTextLen) {
			return false
		}
		if target.ServiceEvidence != nil && (!validDeliveryID(target.ServiceEvidence.ID, "opr") || target.ServiceEvidence.Version <= 0) {
			return false
		}
		if (target.ServiceDisposition == api.PauseServiceTransferred || target.ServiceDisposition == api.PauseServiceDetached) && (target.HandoffNote == "" || target.ServiceEvidence == nil) {
			return false
		}
		if (target.ServiceDisposition == api.PauseServiceNone || target.ServiceDisposition == api.PauseServiceUnresolved) && target.ServiceEvidence != nil {
			return false
		}
		seen[key] = true
	}
	return true
}

func pauseReceipt(ctx context.Context, q queryRower, taskID, operation, requestID string) (api.ProjectPauseReceipt, string, string, error) {
	var receipt api.ProjectPauseReceipt
	var created string
	var cycleID, payloadHash string
	err := q.QueryRowContext(ctx, `SELECT pr.id,pr.operation,pr.request_id,pr.task_id,pc.pause_generation,pr.created_at,pr.cycle_id,pr.payload_hash
FROM project_pause_receipts pr JOIN project_pause_cycles pc ON pc.id=pr.cycle_id
WHERE pr.task_id=? AND pr.operation=? AND pr.request_id=?`, taskID, operation, requestID).Scan(
		&receipt.ID, &receipt.Operation, &receipt.RequestID, &receipt.TaskID, &receipt.PauseGeneration, &created, &cycleID, &payloadHash)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, "", "", api.ErrNotFound
	}
	receipt.CreatedAt = parseTS(created)
	return receipt, cycleID, payloadHash, err
}

func insertPauseReceipt(ctx context.Context, tx *sql.Tx, taskID, operation, requestID, payloadHash, cycleID string, now time.Time) (api.ProjectPauseReceipt, error) {
	receipt := api.ProjectPauseReceipt{ID: api.NewID("ppr"), Operation: operation, RequestID: requestID, TaskID: taskID, CreatedAt: now}
	if err := tx.QueryRowContext(ctx, `SELECT pause_generation FROM project_pause_cycles WHERE id=?`, cycleID).Scan(&receipt.PauseGeneration); err != nil {
		return receipt, err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO project_pause_receipts(id,task_id,operation,request_id,payload_hash,cycle_id,created_at) VALUES(?,?,?,?,?,?,?)`,
		receipt.ID, taskID, operation, requestID, payloadHash, cycleID, ts(now))
	return receipt, err
}

func loadProjectPauseStatus(ctx context.Context, q pauseQuerier, taskID string) (api.ProjectPauseStatus, error) {
	var out api.ProjectPauseStatus
	var pausedAt, resumedAt, cycleID, previousTeamID, previousOrchestrator string
	var selectedTeamID, resumeAgentID, resumeRunID, resumeName, resumeReceiptID, resumeAdmittedAt string
	out.Version, out.TaskID, out.Targets = api.ProjectPauseCapabilityVersion, taskID, []api.ProjectPauseTarget{}
	err := q.QueryRowContext(ctx, `SELECT pause_state,lifecycle_generation,pause_generation,paused_at FROM tasks WHERE id=?`, taskID).Scan(
		&out.State, &out.LifecycleGeneration, &out.PauseGeneration, &pausedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, api.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if pausedAt != "" {
		value := parseTS(pausedAt)
		out.PausedAt = &value
	}
	if out.PauseGeneration == 0 {
		return out, nil
	}
	err = q.QueryRowContext(ctx, `SELECT id,retained_handoff_digest,resumed_at,previous_team_id,previous_orchestrator,selected_team_id,resume_agent_id,resume_run_id,resume_name,resume_receipt_id,resume_admitted_at FROM project_pause_cycles WHERE task_id=? AND pause_generation=?`, taskID, out.PauseGeneration).Scan(
		&cycleID, &out.RetainedHandoffDigest, &resumedAt, &previousTeamID, &previousOrchestrator, &selectedTeamID, &resumeAgentID, &resumeRunID, &resumeName, &resumeReceiptID, &resumeAdmittedAt)
	if err != nil {
		return out, err
	}
	if resumedAt != "" {
		value := parseTS(resumedAt)
		out.ResumedAt = &value
	}
	var pauseReceipt api.ProjectPauseReceipt
	var receiptCreated string
	err = q.QueryRowContext(ctx, `SELECT id,operation,request_id,task_id,created_at FROM project_pause_receipts WHERE cycle_id=? AND operation='pause' ORDER BY created_at,id LIMIT 1`, cycleID).Scan(
		&pauseReceipt.ID, &pauseReceipt.Operation, &pauseReceipt.RequestID, &pauseReceipt.TaskID, &receiptCreated)
	if err != nil {
		return out, err
	}
	pauseReceipt.PauseGeneration, pauseReceipt.CreatedAt = out.PauseGeneration, parseTS(receiptCreated)
	out.Receipt = &pauseReceipt
	rows, err := q.QueryContext(ctx, `SELECT pt.snapshot_json,pt.requested_service_disposition,pt.service_disposition,pt.handoff_note,pt.service_verified,pt.service_evidence_id,pt.service_evidence_version,a.cleanup_done,a.cleanup_error
FROM project_pause_targets pt JOIN agents a ON a.id=pt.agent_id AND a.run_id=pt.run_id
WHERE pt.cycle_id=? ORDER BY a.created_at,a.id`, cycleID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var target api.ProjectPauseTarget
		var raw []byte
		var requestedDisposition, serviceDisposition, handoffNote, evidenceID, cleanupError string
		var evidenceVersion int64
		var serviceVerified, cleanupDone bool
		if err = rows.Scan(&raw, &requestedDisposition, &serviceDisposition, &handoffNote, &serviceVerified, &evidenceID, &evidenceVersion, &cleanupDone, &cleanupError); err != nil {
			return out, err
		}
		if err = json.Unmarshal(raw, &target); err != nil {
			return out, fmt.Errorf("invalid retained project handoff: %w", err)
		}
		// Mutable resolution/receipt fields intentionally overlay the immutable
		// exact-run snapshot.
		target.RequestedServiceDisposition, target.ServiceDisposition, target.HandoffNote = requestedDisposition, serviceDisposition, handoffNote
		target.ServiceVerified = serviceVerified
		if evidenceID != "" {
			target.ServiceEvidence = &api.OperationalReference{ID: evidenceID, Version: evidenceVersion}
		}
		target.CleanupDone, target.CleanupError = cleanupDone, cleanupError
		if !target.CleanupDone {
			out.CleanupPending++
		}
		if !target.ServiceVerified || target.ServiceDisposition == api.PauseServiceUnresolved {
			out.HandoffPending++
		}
		out.Targets = append(out.Targets, target)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	out.PreviousTeam = &api.ProjectPausePreviousTeam{TeamID: previousTeamID, OrchestratorName: previousOrchestrator, Members: append([]api.ProjectPauseTarget(nil), out.Targets...)}
	if resumeReceiptID != "" {
		out.ResumeAdmission = &api.ProjectResumeAdmission{ReceiptID: resumeReceiptID, SelectedTeamID: selectedTeamID, Orchestrator: api.ProjectResumeOrchestrator{AgentID: resumeAgentID, RunID: resumeRunID, Name: resumeName}, Pending: resumeAdmittedAt == ""}
	}
	return out, nil
}

func verifiedServiceOperationalRecord(ctx context.Context, q queryRower, taskID string, target api.Agent, disposition, note string, ref *api.OperationalReference) error {
	if ref == nil || !validDeliveryID(ref.ID, "opr") || ref.Version <= 0 {
		return workItemConflict("verified exact-run service handoff evidence is required")
	}
	record, err := getOperationalRecord(q, ctx, taskID, ref.ID, ref.Version)
	if err != nil {
		return err
	}
	if record.State != "committed" || record.ActorAgentID != target.ID || record.ActorRunID != target.RunID || record.Data.Kind != "result" || record.Data.Result == nil || record.Data.Result.Summary != note {
		return workItemConflict("service handoff evidence does not match the exact target run and handoff note")
	}
	if disposition != api.PauseServiceTransferred && disposition != api.PauseServiceDetached {
		return workItemConflict("operational service evidence is valid only for transferred or detached services")
	}
	return nil
}

func projectPauseDigest(targets []api.ProjectPauseTarget) string {
	copyTargets := append([]api.ProjectPauseTarget(nil), targets...)
	for i := range copyTargets {
		copyTargets[i].CleanupDone = false
		copyTargets[i].CleanupError = ""
	}
	sort.Slice(copyTargets, func(i, j int) bool {
		if copyTargets[i].AgentID == copyTargets[j].AgentID {
			return copyTargets[i].RunID < copyTargets[j].RunID
		}
		return copyTargets[i].AgentID < copyTargets[j].AgentID
	})
	data, _ := json.Marshal(copyTargets)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (s *Store) ProjectPauseStatus(ctx context.Context, taskID string) (api.ProjectPauseStatus, error) {
	if !api.ValidID(taskID, "tsk") {
		return api.ProjectPauseStatus{}, api.ErrInvalid
	}
	return loadProjectPauseStatus(ctx, s.db, taskID)
}

func (s *Store) PauseProject(ctx context.Context, taskID string, req api.PauseProjectRequest, by api.Caller) (api.ProjectPauseStatus, error) {
	if !api.ValidID(taskID, "tsk") || !validPauseRequest(req) {
		return api.ProjectPauseStatus{}, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	payloadHash := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	defer tx.Rollback()
	if receipt, _, priorHash, receiptErr := pauseReceipt(ctx, tx, taskID, "pause", req.RequestID); receiptErr == nil {
		if priorHash != payloadHash {
			return api.ProjectPauseStatus{}, workItemConflict("request ID was already used with different pause data")
		}
		out, loadErr := loadProjectPauseStatus(ctx, tx, taskID)
		out.Replay, out.Receipt = true, &receipt
		return out, loadErr
	} else if !errors.Is(receiptErr, api.ErrNotFound) {
		return api.ProjectPauseStatus{}, receiptErr
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if task.Status != api.TaskOpen {
		return api.ProjectPauseStatus{}, api.ErrClosed
	}
	if task.PauseState != api.ProjectPauseActive {
		return api.ProjectPauseStatus{}, workItemConflict("project is already paused or cleanup is pending")
	}
	if task.LifecycleGeneration != req.ExpectedLifecycleGeneration {
		return api.ProjectPauseStatus{}, workItemConflict("project lifecycle generation changed; refresh before pausing")
	}
	// Current team includes every still-live/retired run and any exited or
	// already-closed exact run whose host cleanup has not been confirmed. A
	// pause cannot make an unsettled session disappear merely because its
	// runtime exited before the owner pressed Pause.
	rows, err := tx.QueryContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND (status NOT IN ('closed','exited') OR cleanup_done=0) ORDER BY created_at,id`, taskID)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	var agents []api.Agent
	for rows.Next() {
		agent, scanErr := scanAgent(rows)
		if scanErr != nil {
			rows.Close()
			return api.ProjectPauseStatus{}, scanErr
		}
		agents = append(agents, agent)
	}
	if err = rows.Close(); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	requested := map[string]api.ProjectPauseTargetRequest{}
	for _, target := range req.Targets {
		requested[target.AgentID+":"+target.RunID] = target
	}
	if len(requested) != len(agents) {
		return api.ProjectPauseStatus{}, workItemConflict("pause targets do not match the complete current team")
	}
	now := s.now()
	cycleID := api.NewID("ppc")
	pauseGeneration := task.PauseGeneration + 1
	lifecycleGeneration := task.LifecycleGeneration + 1
	targets := make([]api.ProjectPauseTarget, 0, len(agents))
	for i := range agents {
		agent := &agents[i]
		request, ok := requested[agent.ID+":"+agent.RunID]
		if !ok {
			return api.ProjectPauseStatus{}, workItemConflict("pause targets changed; refresh the exact current team")
		}
		binding, bindingErr := loadAgentWorkItemBinding(tx, ctx, agent.ID, agent.RunID)
		if bindingErr != nil {
			return api.ProjectPauseStatus{}, bindingErr
		}
		serviceDisposition, serviceVerified := api.PauseServiceUnresolved, false
		if request.ServiceDisposition == api.PauseServiceTransferred || request.ServiceDisposition == api.PauseServiceDetached {
			if evidenceErr := verifiedServiceOperationalRecord(ctx, tx, taskID, *agent, request.ServiceDisposition, request.HandoffNote, request.ServiceEvidence); evidenceErr != nil {
				return api.ProjectPauseStatus{}, evidenceErr
			}
			serviceDisposition, serviceVerified = request.ServiceDisposition, true
		}
		targets = append(targets, api.ProjectPauseTarget{
			AgentID: agent.ID, RunID: agent.RunID, Name: agent.Name, Host: agent.Host, Session: agent.Session,
			Runtime: agent.Runtime, Cwd: agent.Cwd, ParentAgentID: agent.ParentAgentID, Role: agent.Role, WorkItem: binding,
			OriginalStatus: agent.Status, RequestedServiceDisposition: request.ServiceDisposition,
			ServiceDisposition: serviceDisposition, HandoffNote: request.HandoffNote, ServiceVerified: serviceVerified,
			ServiceEvidence: request.ServiceEvidence, CleanupDone: agent.CleanupDone, CleanupError: agent.CleanupError,
		})
	}
	digest := projectPauseDigest(targets)
	if _, err = tx.ExecContext(ctx, `INSERT INTO project_pause_cycles(id,task_id,pause_generation,lifecycle_generation,state,retained_handoff_digest,previous_team_id,previous_orchestrator,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		cycleID, taskID, pauseGeneration, lifecycleGeneration, api.ProjectPauseCleanupPending, digest, req.PreviousTeamID, task.Orchestrator, ts(now)); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	// The barrier is persisted before any closure writes in this transaction.
	barrierResult, updateErr := tx.ExecContext(ctx, `UPDATE tasks SET pause_state=?,lifecycle_generation=?,pause_generation=?,paused_at=? WHERE id=? AND pause_state=? AND lifecycle_generation=?`,
		api.ProjectPauseCleanupPending, lifecycleGeneration, pauseGeneration, ts(now), taskID, api.ProjectPauseActive, task.LifecycleGeneration)
	if updateErr != nil {
		return api.ProjectPauseStatus{}, updateErr
	}
	if changed, rowsErr := barrierResult.RowsAffected(); rowsErr != nil {
		return api.ProjectPauseStatus{}, rowsErr
	} else if changed != 1 {
		return api.ProjectPauseStatus{}, workItemConflict("project lifecycle generation changed during pause")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_allocation_intents SET invalidated_at=?,invalidated_pause_generation=? WHERE target_task_id=? AND consumed_at='' AND invalidated_at=''`, ts(now), pauseGeneration, taskID); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	for _, target := range targets {
		raw, _ := json.Marshal(target)
		var evidenceID string
		var evidenceVersion int64
		if target.ServiceEvidence != nil {
			evidenceID, evidenceVersion = target.ServiceEvidence.ID, target.ServiceEvidence.Version
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO project_pause_targets(cycle_id,agent_id,run_id,snapshot_json,requested_service_disposition,service_disposition,handoff_note,service_verified,service_evidence_id,service_evidence_version) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			cycleID, target.AgentID, target.RunID, raw, target.RequestedServiceDisposition, target.ServiceDisposition, target.HandoffNote, target.ServiceVerified, evidenceID, evidenceVersion); err != nil {
			return api.ProjectPauseStatus{}, err
		}
		result, updateErr := tx.ExecContext(ctx, `UPDATE agents SET status=?,last_event_at=?,blocked_reason='',blocked_text='' WHERE id=? AND task_id=? AND run_id=? AND status<>'closed'`,
			api.AgentClosed, ts(now), target.AgentID, taskID, target.RunID)
		if updateErr != nil {
			return api.ProjectPauseStatus{}, updateErr
		}
		changed, _ := result.RowsAffected()
		if changed == 0 && target.OriginalStatus != api.AgentClosed {
			return api.ProjectPauseStatus{}, workItemConflict("team changed during exact-run pause")
		}
		if changed == 1 {
			if _, err = s.insertEvent(ctx, tx, taskID, api.EventClosed, target.AgentID, "", map[string]any{"runId": target.RunID, "pauseGeneration": pauseGeneration}, by); err != nil {
				return api.ProjectPauseStatus{}, err
			}
		}
	}
	receipt, err := insertPauseReceipt(ctx, tx, taskID, "pause", req.RequestID, payloadHash, cycleID, now)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if _, err = s.insertEvent(ctx, tx, taskID, api.EventTaskPauseRequested, "", task.Name, map[string]any{"pauseGeneration": pauseGeneration, "lifecycleGeneration": lifecycleGeneration, "retainedHandoffDigest": digest, "targetCount": len(targets)}, by); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if err = reconcilePauseCycleTx(ctx, s, tx, taskID, pauseGeneration, by); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	s.notify(taskID)
	out, err := s.ProjectPauseStatus(ctx, taskID)
	out.Receipt = &receipt
	return out, err
}

func reconcilePauseCycleTx(ctx context.Context, s *Store, tx *sql.Tx, taskID string, generation int64, by api.Caller) error {
	var cycleID, state string
	if err := tx.QueryRowContext(ctx, `SELECT id,state FROM project_pause_cycles WHERE task_id=? AND pause_generation=?`, taskID, generation).Scan(&cycleID, &state); err != nil {
		return err
	}
	// A browser's "none" selection is only a request. It becomes verified
	// after the existing exact-run host cleanup receipt confirms the owned
	// session is gone. Browser input alone can never complete this gate.
	if _, err := tx.ExecContext(ctx, `UPDATE project_pause_targets
SET service_disposition='none',service_verified=1,handoff_updated_at=?
WHERE cycle_id=? AND requested_service_disposition='none' AND service_verified=0
AND EXISTS (SELECT 1 FROM agents a WHERE a.id=project_pause_targets.agent_id AND a.run_id=project_pause_targets.run_id AND a.cleanup_done=1)`, ts(s.now()), cycleID); err != nil {
		return err
	}
	var cleanupPending, handoffPending sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT
SUM(CASE WHEN a.cleanup_done=0 THEN 1 ELSE 0 END),
SUM(CASE WHEN pt.service_verified=0 OR pt.service_disposition='unresolved' THEN 1 ELSE 0 END)
FROM project_pause_targets pt JOIN agents a ON a.id=pt.agent_id AND a.run_id=pt.run_id WHERE pt.cycle_id=?`, cycleID).Scan(&cleanupPending, &handoffPending); err != nil {
		return err
	}
	if cleanupPending.Int64 != 0 || handoffPending.Int64 != 0 || state == api.ProjectPausePaused || state == "resumed" {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET pause_state=? WHERE id=? AND pause_generation=? AND pause_state=?`, api.ProjectPausePaused, taskID, generation, api.ProjectPauseCleanupPending)
	if err != nil {
		return err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE project_pause_cycles SET state=? WHERE id=? AND state=?`, api.ProjectPausePaused, cycleID, api.ProjectPauseCleanupPending); err != nil {
		return err
	}
	_, err = s.insertEvent(ctx, tx, taskID, api.EventTaskPaused, "", "", map[string]any{"pauseGeneration": generation}, by)
	return err
}

func (s *Store) ResumeProject(ctx context.Context, taskID string, req api.ResumeProjectRequest, by api.Caller) (api.ProjectPauseStatus, error) {
	if !api.ValidID(taskID, "tsk") || req.Version != api.ProjectPauseCapabilityVersion || !validRequestID(req.RequestID) || req.ExpectedPauseGeneration <= 0 || req.ExpectedLifecycleGeneration <= 0 || len(req.RetainedHandoffDigest) != 64 || !api.ValidText(req.SelectedTeamID, 128) || req.SelectedTeamID == "" || !api.ValidID(req.Orchestrator.AgentID, "agt") || !validRunID(req.Orchestrator.RunID) || !api.ValidName(req.Orchestrator.Name) {
		return api.ProjectPauseStatus{}, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	payloadHash := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	defer tx.Rollback()
	if receipt, _, priorHash, receiptErr := pauseReceipt(ctx, tx, taskID, "resume", req.RequestID); receiptErr == nil {
		if priorHash != payloadHash {
			return api.ProjectPauseStatus{}, workItemConflict("request ID was already used with different resume data")
		}
		out, loadErr := loadProjectPauseStatus(ctx, tx, taskID)
		out.Replay, out.Receipt = true, &receipt
		return out, loadErr
	} else if !errors.Is(receiptErr, api.ErrNotFound) {
		return api.ProjectPauseStatus{}, receiptErr
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if task.Status != api.TaskOpen {
		return api.ProjectPauseStatus{}, api.ErrClosed
	}
	if task.PauseState != api.ProjectPausePaused || task.PauseGeneration != req.ExpectedPauseGeneration || task.LifecycleGeneration != req.ExpectedLifecycleGeneration {
		return api.ProjectPauseStatus{}, workItemConflict("project is not fully paused at the expected generation")
	}
	var cycleID, digest string
	if err = tx.QueryRowContext(ctx, `SELECT id,retained_handoff_digest FROM project_pause_cycles WHERE task_id=? AND pause_generation=? AND state=?`, taskID, task.PauseGeneration, api.ProjectPausePaused).Scan(&cycleID, &digest); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if digest != req.RetainedHandoffDigest {
		return api.ProjectPauseStatus{}, workItemConflict("retained project handoff changed; refresh before resuming")
	}
	var existingIdentity int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM agents WHERE id=? OR run_id=?`, req.Orchestrator.AgentID, req.Orchestrator.RunID).Scan(&existingIdentity); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if existingIdentity != 0 {
		return api.ProjectPauseStatus{}, workItemConflict("resume requires a fresh orchestrator identity and run")
	}
	now := s.now()
	newLifecycleGeneration := task.LifecycleGeneration + 1
	// Resume first persists a narrow admission barrier and the exact fresh lead
	// identity. Only that preallocated run may cross the barrier; normal wake,
	// schedule and admission remain blocked until AddAgent consumes it.
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET pause_state=?,lifecycle_generation=? WHERE id=? AND pause_state=? AND pause_generation=? AND lifecycle_generation=?`,
		api.ProjectPauseResuming, newLifecycleGeneration, taskID, api.ProjectPausePaused, task.PauseGeneration, task.LifecycleGeneration)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return api.ProjectPauseStatus{}, workItemConflict("project lifecycle generation changed during resume")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE project_pause_cycles SET state=?,selected_team_id=?,resume_agent_id=?,resume_run_id=?,resume_name=? WHERE id=? AND state=?`,
		api.ProjectPauseResuming, req.SelectedTeamID, req.Orchestrator.AgentID, req.Orchestrator.RunID, req.Orchestrator.Name, cycleID, api.ProjectPausePaused); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	receipt, err := insertPauseReceipt(ctx, tx, taskID, "resume", req.RequestID, payloadHash, cycleID, now)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE project_pause_cycles SET resume_receipt_id=? WHERE id=? AND state=?`, receipt.ID, cycleID, api.ProjectPauseResuming); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if _, err = s.insertEvent(ctx, tx, taskID, api.EventTaskResumeRequested, "", task.Name, map[string]any{"pauseGeneration": task.PauseGeneration, "lifecycleGeneration": newLifecycleGeneration, "retainedHandoffDigest": digest, "selectedTeamId": req.SelectedTeamID, "orchestratorAgentId": req.Orchestrator.AgentID, "orchestratorRunId": req.Orchestrator.RunID, "resumeReceiptId": receipt.ID}, by); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	s.notify(taskID)
	out, err := s.ProjectPauseStatus(ctx, taskID)
	out.Receipt = &receipt
	return out, err
}

// ConfirmProjectResume clears the admission barrier only after the host-side
// launcher has verified that the saved exact fresh orchestrator session exists.
// The confirmation is independently idempotent so response loss cannot launch
// another run or repeat the lifecycle transition.
func (s *Store) ConfirmProjectResume(ctx context.Context, taskID string, req api.ConfirmProjectResumeRequest, by api.Caller) (api.ProjectPauseStatus, error) {
	if !api.ValidID(taskID, "tsk") || req.Version != api.ProjectPauseCapabilityVersion || !validRequestID(req.RequestID) || req.ExpectedLifecycleGeneration <= 0 || !validDeliveryID(req.ResumeReceiptID, "ppr") || !api.ValidID(req.AgentID, "agt") || !validRunID(req.RunID) {
		return api.ProjectPauseStatus{}, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	payloadHash := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	defer tx.Rollback()
	if receipt, _, priorHash, receiptErr := pauseReceipt(ctx, tx, taskID, "resume_confirm", req.RequestID); receiptErr == nil {
		if priorHash != payloadHash {
			return api.ProjectPauseStatus{}, workItemConflict("request ID was already used with different resume confirmation data")
		}
		out, loadErr := loadProjectPauseStatus(ctx, tx, taskID)
		out.Replay, out.Receipt = true, &receipt
		return out, loadErr
	} else if !errors.Is(receiptErr, api.ErrNotFound) {
		return api.ProjectPauseStatus{}, receiptErr
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if task.Status != api.TaskOpen {
		return api.ProjectPauseStatus{}, api.ErrClosed
	}
	if task.PauseState != api.ProjectPauseResuming || task.LifecycleGeneration != req.ExpectedLifecycleGeneration {
		return api.ProjectPauseStatus{}, workItemConflict("project is not awaiting this resume confirmation")
	}
	plan, err := loadPendingResumeAdmission(ctx, tx, taskID, task.PauseGeneration)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if plan.ReceiptID != req.ResumeReceiptID || plan.AgentID != req.AgentID || plan.RunID != req.RunID {
		return api.ProjectPauseStatus{}, workItemConflict("resume confirmation does not match the saved exact admission")
	}
	agent, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=? AND run_id=?`, taskID, req.AgentID, req.RunID))
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if agent.Name != plan.Name || agent.Role != "" || agent.ParentAgentID != "" || agent.Status == api.AgentClosed || agent.Status == api.AgentExited || agent.Status == api.AgentRetired {
		return api.ProjectPauseStatus{}, workItemConflict("saved fresh orchestrator is not launchable")
	}
	now := s.now()
	result, err := tx.ExecContext(ctx, `UPDATE tasks SET pause_state=?,orchestrator=?,lead_revision=lead_revision+1 WHERE id=? AND pause_state=? AND pause_generation=? AND lifecycle_generation=?`,
		api.ProjectPauseActive, agent.Name, taskID, api.ProjectPauseResuming, task.PauseGeneration, task.LifecycleGeneration)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil {
		return api.ProjectPauseStatus{}, rowsErr
	} else if changed != 1 {
		return api.ProjectPauseStatus{}, workItemConflict("resume admission changed during exact-run confirmation")
	}
	result, err = tx.ExecContext(ctx, `UPDATE project_pause_cycles SET state='resumed',resumed_at=?,resume_admitted_at=? WHERE id=? AND state=? AND resume_receipt_id=? AND resume_agent_id=? AND resume_run_id=?`,
		ts(now), ts(now), plan.CycleID, api.ProjectPauseResuming, plan.ReceiptID, agent.ID, agent.RunID)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil {
		return api.ProjectPauseStatus{}, rowsErr
	} else if changed != 1 {
		return api.ProjectPauseStatus{}, workItemConflict("resume admission receipt was already consumed or changed")
	}
	receipt, err := insertPauseReceipt(ctx, tx, taskID, "resume_confirm", req.RequestID, payloadHash, plan.CycleID, now)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if _, err = s.insertEvent(ctx, tx, taskID, api.EventTaskResumed, agent.ID, agent.Name, map[string]any{"runId": agent.RunID, "pauseGeneration": task.PauseGeneration, "lifecycleGeneration": task.LifecycleGeneration, "resumeReceiptId": plan.ReceiptID, "selectedTeamId": plan.SelectedTeam}, by); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	s.notify(taskID)
	out, err := s.ProjectPauseStatus(ctx, taskID)
	out.Receipt = &receipt
	return out, err
}

func (s *Store) ResolvePauseHandoff(ctx context.Context, taskID string, req api.ResolvePauseHandoffRequest, by api.Caller) (api.ProjectPauseStatus, error) {
	if !api.ValidID(taskID, "tsk") || req.Version != api.ProjectPauseCapabilityVersion || !validRequestID(req.RequestID) || req.PauseGeneration <= 0 || !api.ValidID(req.AgentID, "agt") || !validRunID(req.RunID) || !validServiceDisposition(req.ServiceDisposition) || req.ServiceDisposition == api.PauseServiceUnresolved || !api.ValidText(req.HandoffNote, api.MaxTextLen) || (req.ServiceDisposition != api.PauseServiceNone && req.HandoffNote == "") || (req.ServiceEvidence != nil && (!validDeliveryID(req.ServiceEvidence.ID, "opr") || req.ServiceEvidence.Version <= 0)) {
		return api.ProjectPauseStatus{}, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	payloadHash := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	defer tx.Rollback()
	if receipt, _, priorHash, receiptErr := pauseReceipt(ctx, tx, taskID, "handoff", req.RequestID); receiptErr == nil {
		if priorHash != payloadHash {
			return api.ProjectPauseStatus{}, workItemConflict("request ID was already used with different handoff data")
		}
		out, loadErr := loadProjectPauseStatus(ctx, tx, taskID)
		out.Replay, out.Receipt = true, &receipt
		return out, loadErr
	} else if !errors.Is(receiptErr, api.ErrNotFound) {
		return api.ProjectPauseStatus{}, receiptErr
	}
	var cycleID, state string
	if err = tx.QueryRowContext(ctx, `SELECT id,state FROM project_pause_cycles WHERE task_id=? AND pause_generation=?`, taskID, req.PauseGeneration).Scan(&cycleID, &state); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if state != api.ProjectPauseCleanupPending {
		return api.ProjectPauseStatus{}, workItemConflict("pause handoff is not pending")
	}
	target, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND id=? AND run_id=?`, taskID, req.AgentID, req.RunID))
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	var evidenceID string
	var evidenceVersion int64
	if req.ServiceDisposition == api.PauseServiceNone {
		if !target.CleanupDone || req.ServiceEvidence != nil {
			return api.ProjectPauseStatus{}, workItemConflict("no-service resolution requires the successful exact-run host cleanup receipt")
		}
	} else {
		if err = verifiedServiceOperationalRecord(ctx, tx, taskID, target, req.ServiceDisposition, req.HandoffNote, req.ServiceEvidence); err != nil {
			return api.ProjectPauseStatus{}, err
		}
		evidenceID, evidenceVersion = req.ServiceEvidence.ID, req.ServiceEvidence.Version
	}
	result, err := tx.ExecContext(ctx, `UPDATE project_pause_targets SET requested_service_disposition=?,service_disposition=?,handoff_note=?,service_verified=1,service_evidence_id=?,service_evidence_version=?,handoff_updated_at=? WHERE cycle_id=? AND agent_id=? AND run_id=? AND (service_disposition='unresolved' OR service_verified=0)`,
		req.ServiceDisposition, req.ServiceDisposition, req.HandoffNote, evidenceID, evidenceVersion, ts(s.now()), cycleID, req.AgentID, req.RunID)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return api.ProjectPauseStatus{}, workItemConflict("exact pause target handoff is no longer unresolved")
	}
	current, err := loadProjectPauseStatus(ctx, tx, taskID)
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	digest := projectPauseDigest(current.Targets)
	if _, err = tx.ExecContext(ctx, `UPDATE project_pause_cycles SET retained_handoff_digest=? WHERE id=?`, digest, cycleID); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	receipt, err := insertPauseReceipt(ctx, tx, taskID, "handoff", req.RequestID, payloadHash, cycleID, s.now())
	if err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if err = reconcilePauseCycleTx(ctx, s, tx, taskID, req.PauseGeneration, by); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.ProjectPauseStatus{}, err
	}
	s.notify(taskID)
	out, err := s.ProjectPauseStatus(ctx, taskID)
	out.Receipt = &receipt
	return out, err
}
