package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const deliveryCols = `id,task_id,message_seq,kind,recipient_kind,action_key,action_class,agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,governing_order_task_id,governing_order_message_seq,instruction_sha256,instruction_bytes,enrollment_version,context_digest,generation,COALESCE(supersedes_delivery_id,''),current,phase,execution_epoch,COALESCE(current_block_id,''),result_text,ack_deadline_seconds,progress_deadline_seconds,resume_deadline_seconds,confirmation_deadline_seconds,dispatch_report_seconds,active_tool_hard_limit_seconds,max_queue_attempts,last_substantive_progress_at,next_substantive_deadline_at,hard_deadline_at,followthrough_attempt_count,followthrough_pending_action,followthrough_pending_request_id,followthrough_pending_since,followthrough_transport_outcome,followthrough_confirmation_until,followthrough_escalation_message_seq,created_at,updated_at`

var defaultDeliveryFollowThroughPolicy = api.DeliveryFollowThroughPolicy{
	AckDeadlineSeconds:          120,
	ProgressDeadlineSeconds:     600,
	ResumeDeadlineSeconds:       120,
	ConfirmationDeadlineSeconds: 120,
	DispatchReportSeconds:       30,
	ActiveToolHardLimitSeconds:  1800,
	MaxQueueAttempts:            2,
}

func migrateDelivery(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS required_deliveries (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(id),
  message_seq INTEGER NOT NULL,
  kind TEXT NOT NULL,
  recipient_kind TEXT NOT NULL DEFAULT 'item_worker',
  action_key TEXT NOT NULL DEFAULT 'primary',
  action_class TEXT NOT NULL DEFAULT 'execution',
  agent_id TEXT NOT NULL REFERENCES agents(id),
  run_id TEXT NOT NULL,
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  item_revision INTEGER NOT NULL,
  work_order_task_id TEXT NOT NULL,
  work_order_message_seq INTEGER NOT NULL,
  governing_order_task_id TEXT NOT NULL DEFAULT '',
  governing_order_message_seq INTEGER NOT NULL DEFAULT 0,
  instruction_sha256 TEXT NOT NULL DEFAULT '',
  instruction_bytes INTEGER NOT NULL DEFAULT 0,
  enrollment_version INTEGER NOT NULL DEFAULT 1,
  context_digest TEXT NOT NULL,
  generation INTEGER NOT NULL,
  supersedes_delivery_id TEXT,
  current INTEGER NOT NULL DEFAULT 1,
  phase TEXT NOT NULL,
  execution_epoch INTEGER NOT NULL DEFAULT 1,
  current_block_id TEXT,
  result_text TEXT NOT NULL DEFAULT '',
  ack_deadline_seconds INTEGER NOT NULL DEFAULT 120,
  progress_deadline_seconds INTEGER NOT NULL DEFAULT 600,
  resume_deadline_seconds INTEGER NOT NULL DEFAULT 120,
  confirmation_deadline_seconds INTEGER NOT NULL DEFAULT 120,
  dispatch_report_seconds INTEGER NOT NULL DEFAULT 30,
  active_tool_hard_limit_seconds INTEGER NOT NULL DEFAULT 1800,
  max_queue_attempts INTEGER NOT NULL DEFAULT 2,
  last_substantive_progress_at TEXT NOT NULL DEFAULT '',
  next_substantive_deadline_at TEXT NOT NULL DEFAULT '',
  hard_deadline_at TEXT NOT NULL DEFAULT '',
  followthrough_attempt_count INTEGER NOT NULL DEFAULT 0,
  followthrough_pending_action TEXT NOT NULL DEFAULT '',
  followthrough_pending_request_id TEXT NOT NULL DEFAULT '',
  followthrough_pending_since TEXT NOT NULL DEFAULT '',
  followthrough_transport_outcome TEXT NOT NULL DEFAULT '',
  followthrough_confirmation_until TEXT NOT NULL DEFAULT '',
  followthrough_escalation_message_seq INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(task_id,item_task_id,item_id,agent_id,run_id,recipient_kind,action_key,generation)
);
CREATE INDEX IF NOT EXISTS required_deliveries_recipient
  ON required_deliveries(task_id,agent_id,run_id,current);
CREATE TABLE IF NOT EXISTS delivery_blocks (
  id TEXT PRIMARY KEY,
  delivery_id TEXT NOT NULL REFERENCES required_deliveries(id),
  execution_epoch INTEGER NOT NULL,
  reason_class TEXT NOT NULL,
  text TEXT NOT NULL DEFAULT '',
  resolved INTEGER NOT NULL DEFAULT 0,
  resolution_id TEXT NOT NULL DEFAULT '',
  resolution_text TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  resolved_at TEXT,
  UNIQUE(delivery_id,execution_epoch)
);
CREATE TABLE IF NOT EXISTS delivery_events (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  delivery_id TEXT NOT NULL REFERENCES required_deliveries(id),
  sequence INTEGER NOT NULL,
  kind TEXT NOT NULL,
  agent_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL,
  data TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(delivery_id,sequence)
);
CREATE INDEX IF NOT EXISTS delivery_events_delivery ON delivery_events(delivery_id,sequence);
CREATE TABLE IF NOT EXISTS delivery_operation_receipts (
  receipt_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  operation TEXT NOT NULL,
  subject_id TEXT NOT NULL,
  actor_agent_id TEXT NOT NULL DEFAULT '',
  actor_run_id TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  request_id TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  response_json BLOB NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(task_id,operation,actor_agent_id,actor_run_id,by_node,by_user,request_id)
);
CREATE TABLE IF NOT EXISTS delivery_recovery_incidents (
  id TEXT PRIMARY KEY,
  delivery_id TEXT NOT NULL REFERENCES required_deliveries(id),
  generation INTEGER NOT NULL,
  execution_epoch INTEGER NOT NULL,
  cause_status TEXT NOT NULL,
  last_substantive_action TEXT NOT NULL,
  last_substantive_at TEXT NOT NULL,
  expected_next_action TEXT NOT NULL,
  stop_reason TEXT NOT NULL,
  causal_evidence TEXT NOT NULL,
  contributing_conditions TEXT NOT NULL,
  unresolved_questions TEXT NOT NULL,
  prevention_owner_agent_id TEXT NOT NULL,
  prevention_owner_run_id TEXT NOT NULL,
  prevention_work_order_task_id TEXT NOT NULL,
  prevention_work_order_message_seq INTEGER NOT NULL,
  prevention_verification_criterion TEXT NOT NULL,
  prior_incident_id TEXT NOT NULL DEFAULT '',
  prior_control_failure TEXT NOT NULL DEFAULT '',
  recorded_by_agent_id TEXT NOT NULL,
  recorded_by_run_id TEXT NOT NULL,
  created_at TEXT NOT NULL
);`)
	return err
}

func validDeliveryID(id, prefix string) bool {
	if len(id) != len(prefix)+17 || !strings.HasPrefix(id, prefix+"_") {
		return false
	}
	for _, r := range id[len(prefix)+1:] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func validDeliveryKind(kind string) bool {
	return kind == api.DeliveryAssignment || kind == api.DeliveryAmendment || kind == api.DeliveryReview
}

func normalizeDeliveryResponsibility(req *api.CreateRequiredDeliveryRequest) bool {
	if req.RecipientKind == "" {
		req.RecipientKind = api.DeliveryRecipientItemWorker
	}
	if req.ActionKey == "" {
		req.ActionKey = "primary"
	}
	if req.ActionClass == "" {
		req.ActionClass = api.DeliveryActionExecution
	}
	if strings.TrimSpace(req.ActionKey) != req.ActionKey || !api.ValidText(req.ActionKey, 128) {
		return false
	}
	switch req.RecipientKind {
	case api.DeliveryRecipientItemWorker:
		return req.ActionClass == api.DeliveryActionExecution
	case api.DeliveryRecipientProjectLead:
		return req.EnrollmentVersion == api.ReliableMandatoryActionCapabilityVersion && (req.ActionClass == api.DeliveryActionExecution || req.ActionClass == api.DeliveryActionIndependentDispatch)
	case api.DeliveryRecipientDatabaseHandler:
		return req.EnrollmentVersion == api.ReliableMandatoryActionCapabilityVersion && (req.ActionClass == api.DeliveryActionExecution || req.ActionClass == api.DeliveryActionDatabaseOperation)
	default:
		return false
	}
}

func reliableFollowThroughEnrollment(version int) bool {
	return version == api.ReliableFollowThroughCapabilityVersion || version == api.ReliableMandatoryActionCapabilityVersion
}

func roleResponsibilityDigest(d api.RequiredDelivery) string {
	seed := fmt.Sprintf("%s\n%s\n%d\n%s\n%d\n%s\n%s\n%s\n%s", d.TaskID, d.ItemID, d.ItemRevision,
		d.WorkOrderMessage.TaskID, d.WorkOrderMessage.Seq, d.AgentID, d.RunID, d.RecipientKind, d.ActionKey)
	sum := sha256.Sum256([]byte(seed))
	return fmt.Sprintf("%x", sum)
}

func validateDeliveryRecipient(q queryRower, ctx context.Context, d api.RequiredDelivery) (string, error) {
	if d.RecipientKind == "" || d.RecipientKind == api.DeliveryRecipientItemWorker {
		binding, err := loadAgentWorkItemBinding(q, ctx, d.AgentID, d.RunID)
		if err != nil {
			return "", err
		}
		if binding == nil || binding.ItemTaskID != d.ItemTaskID || binding.ItemID != d.ItemID ||
			binding.ItemRevision != d.ItemRevision || binding.WorkOrderMessage != d.WorkOrderMessage {
			return "", workItemConflict("recipient item-worker binding does not match the directive item revision")
		}
		return binding.ContextDigest, nil
	}
	var currentRun, status, role, name, orchestrator string
	err := q.QueryRowContext(ctx, `SELECT a.run_id,a.status,a.role,a.name,t.orchestrator FROM agents a JOIN tasks t ON t.id=a.task_id WHERE a.task_id=? AND a.id=?`, d.TaskID, d.AgentID).Scan(&currentRun, &status, &role, &name, &orchestrator)
	if errors.Is(err, sql.ErrNoRows) {
		return "", api.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if currentRun != d.RunID || status == api.AgentClosed || status == api.AgentExited {
		return "", workItemConflict("shared-role recipient is not the exact current retained run")
	}
	if d.RecipientKind == api.DeliveryRecipientProjectLead && (role != "" || name != orchestrator) {
		return "", workItemConflict("recipient is not the exact current project lead run")
	}
	if d.RecipientKind == api.DeliveryRecipientDatabaseHandler && role != api.AgentRoleDatabaseHandler {
		return "", workItemConflict("recipient is not the exact current database handler run")
	}
	return roleResponsibilityDigest(d), nil
}

func scanRequiredDelivery(row interface{ Scan(...any) error }) (api.RequiredDelivery, error) {
	var d api.RequiredDelivery
	var current int
	var created, updated, lastProgress, nextDeadline, hardDeadline, pendingSince, confirmationUntil string
	err := row.Scan(&d.ID, &d.TaskID, &d.MessageSeq, &d.Kind, &d.RecipientKind, &d.ActionKey, &d.ActionClass, &d.AgentID, &d.RunID,
		&d.ItemTaskID, &d.ItemID, &d.ItemRevision, &d.WorkOrderMessage.TaskID,
		&d.WorkOrderMessage.Seq, &d.GoverningOrderMessage.TaskID, &d.GoverningOrderMessage.Seq,
		&d.InstructionSHA256, &d.InstructionBytes, &d.EnrollmentVersion, &d.ContextDigest, &d.Generation, &d.SupersedesID,
		&current, &d.Phase, &d.ExecutionEpoch, &d.CurrentBlockID, &d.ResultText,
		&d.FollowThrough.Policy.AckDeadlineSeconds, &d.FollowThrough.Policy.ProgressDeadlineSeconds,
		&d.FollowThrough.Policy.ResumeDeadlineSeconds, &d.FollowThrough.Policy.ConfirmationDeadlineSeconds,
		&d.FollowThrough.Policy.DispatchReportSeconds, &d.FollowThrough.Policy.ActiveToolHardLimitSeconds,
		&d.FollowThrough.Policy.MaxQueueAttempts, &lastProgress, &nextDeadline, &hardDeadline,
		&d.FollowThrough.AttemptCount, &d.FollowThrough.PendingAction, &d.FollowThrough.PendingRequestID,
		&pendingSince, &d.FollowThrough.TransportOutcome, &confirmationUntil, &d.FollowThrough.EscalationMessageSeq,
		&created, &updated)
	d.Current = current != 0
	d.CreatedAt, d.UpdatedAt = parseTS(created), parseTS(updated)
	d.FollowThrough.LastSubstantiveProgressAt = optionalTime(lastProgress)
	d.FollowThrough.NextDeadlineAt = optionalTime(nextDeadline)
	d.FollowThrough.HardDeadlineAt = optionalTime(hardDeadline)
	d.FollowThrough.PendingSince = optionalTime(pendingSince)
	d.FollowThrough.ConfirmationUntil = optionalTime(confirmationUntil)
	return d, err
}

func optionalTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	t := parseTS(value)
	if t.IsZero() {
		return nil
	}
	return &t
}

func optionalTS(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return ts(value)
}

func getRequiredDeliveryRow(q queryRower, ctx context.Context, taskID, deliveryID string) (api.RequiredDelivery, error) {
	d, err := scanRequiredDelivery(q.QueryRowContext(ctx, `SELECT `+deliveryCols+` FROM required_deliveries WHERE task_id=? AND id=?`, taskID, deliveryID))
	if errors.Is(err, sql.ErrNoRows) {
		return d, api.ErrNotFound
	}
	return d, err
}

func currentRequiredDeliveryRowForAction(q queryRower, ctx context.Context, taskID, itemTaskID, itemID, agentID, runID, recipientKind, actionKey string) (api.RequiredDelivery, error) {
	d, err := scanRequiredDelivery(q.QueryRowContext(ctx, `SELECT `+deliveryCols+` FROM required_deliveries WHERE task_id=? AND item_task_id=? AND item_id=? AND agent_id=? AND run_id=? AND recipient_kind=? AND action_key=? AND current=1`, taskID, itemTaskID, itemID, agentID, runID, recipientKind, actionKey))
	if errors.Is(err, sql.ErrNoRows) {
		return d, api.ErrNotFound
	}
	return d, err
}

func hydrateRequiredDelivery(q queryRower, ctx context.Context, d api.RequiredDelivery) (api.RequiredDelivery, error) {
	message, err := loadMessage(q, ctx, d.TaskID, d.MessageSeq)
	if err != nil {
		return d, err
	}
	d.Message = &message
	d.Events, err = loadDeliveryEvents(q, ctx, d.ID)
	if err != nil {
		return d, err
	}
	if d.CurrentBlockID != "" {
		block, blockErr := getDeliveryBlock(q, ctx, d.ID, d.CurrentBlockID)
		if blockErr != nil {
			return d, blockErr
		}
		d.CurrentBlock = &block
	}
	d.LatestRecoveryIncident, err = latestDeliveryRecoveryIncident(q, ctx, d.ID)
	if err != nil {
		return d, err
	}
	return d, nil
}

func currentItemWorkerDelivery(q queryRower, ctx context.Context, taskID, itemTaskID, itemID, agentID, runID, actionKey string) (api.RequiredDelivery, error) {
	query := `SELECT ` + deliveryCols + ` FROM required_deliveries WHERE task_id=? AND item_task_id=? AND item_id=? AND agent_id=? AND run_id=? AND current=1 AND recipient_kind=?`
	args := []any{taskID, itemTaskID, itemID, agentID, runID, api.DeliveryRecipientItemWorker}
	if actionKey != "" {
		query += ` AND action_key=?`
		args = append(args, actionKey)
	}
	query += ` ORDER BY created_at,id`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return api.RequiredDelivery{}, err
	}
	defer rows.Close()
	var selected api.RequiredDelivery
	count := 0
	for rows.Next() {
		selected, err = scanRequiredDelivery(rows)
		if err != nil {
			return selected, err
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return selected, err
	}
	if count == 0 {
		return selected, api.ErrNotFound
	}
	if count > 1 {
		return api.RequiredDelivery{}, workItemConflict("multiple current mandatory actions exist; select the exact item and action key")
	}
	return hydrateRequiredDelivery(q, ctx, selected)
}

func currentRoleRequiredDelivery(q queryRower, ctx context.Context, taskID, agentID, runID, itemID, actionKey string) (api.RequiredDelivery, error) {
	query := `SELECT ` + deliveryCols + ` FROM required_deliveries
WHERE task_id=? AND agent_id=? AND run_id=? AND current=1 AND recipient_kind<>?`
	args := []any{taskID, agentID, runID, api.DeliveryRecipientItemWorker}
	if itemID != "" {
		query += ` AND item_id=? AND action_key=?`
		args = append(args, itemID, actionKey)
	} else if actionKey != "" {
		query += ` AND action_key=?`
		args = append(args, actionKey)
	}
	query += ` ORDER BY created_at,id`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return api.RequiredDelivery{}, err
	}
	defer rows.Close()
	var selected api.RequiredDelivery
	count := 0
	for rows.Next() {
		selected, err = scanRequiredDelivery(rows)
		if err != nil {
			return selected, err
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return selected, err
	}
	if count == 0 {
		return selected, api.ErrNotFound
	}
	if count > 1 {
		return api.RequiredDelivery{}, workItemConflict("multiple current mandatory actions exist; select the exact item and action key")
	}
	d := selected
	return hydrateRequiredDelivery(q, ctx, d)
}

func loadDeliveryEvents(q queryRower, ctx context.Context, deliveryID string) ([]api.DeliveryEvent, error) {
	rows, err := q.QueryContext(ctx, `SELECT id,task_id,delivery_id,sequence,kind,agent_id,run_id,request_id,data,by_node,by_user,created_at FROM delivery_events WHERE delivery_id=? ORDER BY sequence`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []api.DeliveryEvent
	for rows.Next() {
		var event api.DeliveryEvent
		var raw, created string
		if err = rows.Scan(&event.ID, &event.TaskID, &event.DeliveryID, &event.Sequence, &event.Kind, &event.AgentID, &event.RunID, &event.RequestID, &raw, &event.By.Node, &event.By.User, &created); err != nil {
			return nil, err
		}
		event.CreatedAt = parseTS(created)
		if raw != "" {
			if err = json.Unmarshal([]byte(raw), &event.Data); err != nil {
				return nil, err
			}
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func scanDeliveryBlock(row interface{ Scan(...any) error }) (api.DeliveryBlock, error) {
	var b api.DeliveryBlock
	var resolved int
	var created string
	var resolvedAt sql.NullString
	err := row.Scan(&b.ID, &b.DeliveryID, &b.ExecutionEpoch, &b.ReasonClass, &b.Text,
		&resolved, &b.ResolutionID, &b.ResolutionText, &created, &resolvedAt)
	b.Resolved = resolved != 0
	b.CreatedAt = parseTS(created)
	if resolvedAt.Valid {
		t := parseTS(resolvedAt.String)
		b.ResolvedAt = &t
	}
	return b, err
}

func scanDeliveryRecoveryIncident(row interface{ Scan(...any) error }) (api.DeliveryRecoveryIncident, error) {
	var incident api.DeliveryRecoveryIncident
	var lastAt, createdAt, causal, conditions, questions string
	err := row.Scan(&incident.ID, &incident.DeliveryID, &incident.Generation, &incident.ExecutionEpoch,
		&incident.CauseStatus, &incident.LastSubstantiveAction, &lastAt, &incident.ExpectedNextAction,
		&incident.StopReason, &causal, &conditions, &questions, &incident.Prevention.OwnerAgentID,
		&incident.Prevention.OwnerRunID, &incident.Prevention.WorkOrderMessage.TaskID,
		&incident.Prevention.WorkOrderMessage.Seq, &incident.Prevention.VerificationCriterion,
		&incident.PriorIncidentID, &incident.PriorControlFailure, &incident.RecordedByAgentID,
		&incident.RecordedByRunID, &createdAt)
	if err != nil {
		return incident, err
	}
	incident.LastSubstantiveAt, incident.CreatedAt = parseTS(lastAt), parseTS(createdAt)
	for raw, target := range map[string]*[]string{causal: &incident.CausalEvidence, conditions: &incident.ContributingConditions, questions: &incident.UnresolvedQuestions} {
		if err = json.Unmarshal([]byte(raw), target); err != nil {
			return incident, err
		}
	}
	return incident, nil
}

func latestDeliveryRecoveryIncident(q queryRower, ctx context.Context, deliveryID string) (*api.DeliveryRecoveryIncident, error) {
	incident, err := scanDeliveryRecoveryIncident(q.QueryRowContext(ctx, `SELECT id,delivery_id,generation,execution_epoch,cause_status,last_substantive_action,last_substantive_at,expected_next_action,stop_reason,causal_evidence,contributing_conditions,unresolved_questions,prevention_owner_agent_id,prevention_owner_run_id,prevention_work_order_task_id,prevention_work_order_message_seq,prevention_verification_criterion,prior_incident_id,prior_control_failure,recorded_by_agent_id,recorded_by_run_id,created_at FROM delivery_recovery_incidents WHERE delivery_id=? ORDER BY rowid DESC LIMIT 1`, deliveryID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &incident, nil
}

func getDeliveryBlock(q queryRower, ctx context.Context, deliveryID, blockID string) (api.DeliveryBlock, error) {
	b, err := scanDeliveryBlock(q.QueryRowContext(ctx, `SELECT id,delivery_id,execution_epoch,reason_class,text,resolved,resolution_id,resolution_text,created_at,resolved_at FROM delivery_blocks WHERE delivery_id=? AND id=?`, deliveryID, blockID))
	if errors.Is(err, sql.ErrNoRows) {
		return b, api.ErrNotFound
	}
	return b, err
}

func hasDeliveryItemLink(message api.Message, taskID, itemID string, revision int64) bool {
	for _, link := range message.WorkItems {
		if link.ItemTaskID == taskID && link.ItemID == itemID && link.ItemRevision == revision && link.Relationship == "primary" {
			return true
		}
	}
	return false
}

func instructionEvidence(text string) (int64, string) {
	b := []byte(text)
	return int64(len(b)), fmt.Sprintf("%x", sha256.Sum256(b))
}

func validateDeliveryProducer(q queryRower, ctx context.Context, taskID, agentID, runID string) error {
	if agentID == "" && runID == "" {
		return nil // Human UI/API actor under the existing shared-workspace credential.
	}
	if !api.ValidID(agentID, "agt") || !validRunID(runID) {
		return api.ErrInvalid
	}
	var currentRun, status, role, name, orchestrator string
	err := q.QueryRowContext(ctx, `SELECT a.run_id,a.status,a.role,a.name,t.orchestrator FROM agents a JOIN tasks t ON t.id=a.task_id WHERE a.task_id=? AND a.id=?`, taskID, agentID).Scan(&currentRun, &status, &role, &name, &orchestrator)
	if errors.Is(err, sql.ErrNoRows) {
		return api.ErrNotFound
	}
	if err != nil {
		return err
	}
	if currentRun != runID || status == api.AgentRetired || status == api.AgentClosed || status == api.AgentExited || (role != "database_handler" && name != orchestrator) {
		return workItemConflict("producer is not the exact current project lead or database handler run")
	}
	return nil
}

func findDeliveryReplay(q queryRower, ctx context.Context, taskID, operation, subjectID, actorAgentID, actorRunID, requestID, payload string, by api.Caller) (api.DeliveryMutation, error) {
	var out api.DeliveryMutation
	var priorSubject, priorHash string
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT subject_id,payload_hash,response_json FROM delivery_operation_receipts WHERE task_id=? AND operation=? AND actor_agent_id=? AND actor_run_id=? AND by_node=? AND by_user=? AND request_id=?`,
		taskID, operation, actorAgentID, actorRunID, by.Node, by.User, requestID).Scan(&priorSubject, &priorHash, &raw)
	if err == nil {
		if priorSubject != subjectID || priorHash != payload {
			return out, workItemConflict("request ID was already used with different delivery data")
		}
		if err = json.Unmarshal(raw, &out); err != nil {
			return out, err
		}
		out.Replay = true
		return out, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return out, api.ErrNotFound
	}
	return out, err
}

func insertDeliveryReceipt(ctx context.Context, tx *sql.Tx, taskID, operation, subjectID, actorAgentID, actorRunID, requestID, payload string, by api.Caller, out *api.DeliveryMutation) error {
	out.Receipt = api.DeliveryReceipt{ID: api.NewID("drr"), RequestID: requestID, Operation: operation, CreatedAt: out.Event.CreatedAt}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO delivery_operation_receipts(receipt_id,task_id,operation,subject_id,actor_agent_id,actor_run_id,by_node,by_user,request_id,payload_hash,response_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		out.Receipt.ID, taskID, operation, subjectID, actorAgentID, actorRunID, by.Node, by.User, requestID, payload, raw, ts(out.Receipt.CreatedAt))
	return err
}

func insertDeliveryEvent(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, kind, requestID, actorAgentID, actorRunID string, data map[string]any, by api.Caller, now time.Time) (api.DeliveryEvent, error) {
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM delivery_events WHERE delivery_id=?`, d.ID).Scan(&sequence); err != nil {
		return api.DeliveryEvent{}, err
	}
	e := api.DeliveryEvent{ID: api.NewID("dlev"), TaskID: d.TaskID, DeliveryID: d.ID, Sequence: sequence, Kind: kind, AgentID: actorAgentID, RunID: actorRunID, RequestID: requestID, Data: data, By: by, CreatedAt: now}
	raw := ""
	if len(data) > 0 {
		b, err := json.Marshal(data)
		if err != nil {
			return e, err
		}
		raw = string(b)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO delivery_events(id,task_id,delivery_id,sequence,kind,agent_id,run_id,request_id,data,by_node,by_user,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.TaskID, e.DeliveryID, e.Sequence, e.Kind, e.AgentID, e.RunID, e.RequestID, raw, by.Node, by.User, ts(now))
	return e, err
}

// CreateRequiredDelivery attaches an immutable, already-stored message to one
// exact recipient/action responsibility. Message lookup, item/run validation,
// action-scoped generation CAS, supersession, event, and receipt commit atomically.
func (s *Store) CreateRequiredDelivery(ctx context.Context, taskID string, req api.CreateRequiredDeliveryRequest, by api.Caller) (api.DeliveryMutation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out api.DeliveryMutation
	// Hash the caller's wire payload before defaulting v1/v2 responsibility
	// fields so pre-v3 response-loss retries retain their historical identity.
	payload := requestHash(req)
	if !normalizeDeliveryResponsibility(&req) {
		return out, api.ErrInvalid
	}
	if !api.ValidID(taskID, "tsk") || !validRequestID(req.RequestID) || req.MessageSeq < 1 || !validDeliveryKind(req.Kind) ||
		!api.ValidID(req.AgentID, "agt") || !validRunID(req.RunID) || !api.ValidID(req.ItemTaskID, "tsk") ||
		!api.ValidID(req.ItemID, "wi") || req.ItemRevision < 1 || req.WorkOrderMessage.TaskID != req.ItemTaskID ||
		req.WorkOrderMessage.Seq < 1 || req.ExpectedCurrentGeneration < 0 ||
		((req.ProducerAgentID == "") != (req.ProducerRunID == "")) ||
		(req.EnrollmentVersion != 0 && req.EnrollmentVersion != 1 && !reliableFollowThroughEnrollment(req.EnrollmentVersion)) {
		return out, api.ErrInvalid
	}
	if reliableFollowThroughEnrollment(req.EnrollmentVersion) &&
		(req.GoverningOrderMessage.TaskID != req.ItemTaskID || req.GoverningOrderMessage.Seq < 1 ||
			req.InstructionBytes < 1 || len(req.InstructionSHA256) != sha256.Size*2) {
		return out, api.ErrInvalid
	}
	subject := req.ItemID + ":" + req.AgentID + ":" + req.RunID
	if req.RecipientKind != api.DeliveryRecipientItemWorker {
		subject = strings.Join([]string{subject, req.RecipientKind, req.ActionKey}, ":")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if replay, replayErr := findDeliveryReplay(tx, ctx, taskID, "create", subject, req.ProducerAgentID, req.ProducerRunID, req.RequestID, payload, by); replayErr == nil {
		return replay, nil
	} else if !errors.Is(replayErr, api.ErrNotFound) {
		return out, replayErr
	}
	if err = validateDeliveryProducer(tx, ctx, taskID, req.ProducerAgentID, req.ProducerRunID); err != nil {
		return out, err
	}
	var taskStatus, pauseState string
	if err = tx.QueryRowContext(ctx, `SELECT status,pause_state FROM tasks WHERE id=?`, taskID).Scan(&taskStatus, &pauseState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, api.ErrNotFound
		}
		return out, err
	}
	if taskStatus != api.TaskOpen {
		return out, api.ErrClosed
	}
	if pauseState != api.ProjectPauseActive {
		return out, workItemConflict("project is paused; directive creation is blocked")
	}
	message, err := loadMessage(tx, ctx, taskID, req.MessageSeq)
	if err != nil || !hasDeliveryItemLink(message, req.ItemTaskID, req.ItemID, req.ItemRevision) {
		if err != nil {
			return out, err
		}
		return out, workItemConflict("directive message is not immutably linked to the exact item revision")
	}
	enrollmentVersion := req.EnrollmentVersion
	if enrollmentVersion == 0 {
		enrollmentVersion = 1
	}
	var governingOrder api.MessageReference
	var instructionBytes int64
	var instructionSHA256 string
	if reliableFollowThroughEnrollment(enrollmentVersion) {
		governingOrder = req.GoverningOrderMessage
		if message.WorkOrderMessage == nil || *message.WorkOrderMessage != governingOrder {
			return out, workItemConflict("directive message is not linked to the exact governing work order")
		}
		governing, governingErr := loadMessage(tx, ctx, governingOrder.TaskID, governingOrder.Seq)
		if governingErr != nil {
			return out, governingErr
		}
		if !hasDeliveryItemLink(governing, req.ItemTaskID, req.ItemID, req.ItemRevision) {
			return out, workItemConflict("governing work order is not immutably linked to the exact item revision")
		}
		instructionBytes, instructionSHA256 = instructionEvidence(message.Text)
		if instructionBytes != req.InstructionBytes || instructionSHA256 != strings.ToLower(req.InstructionSHA256) {
			return out, workItemConflict("full directive instruction bytes or SHA256 do not match the immutable message")
		}
	}
	order, err := loadMessage(tx, ctx, req.WorkOrderMessage.TaskID, req.WorkOrderMessage.Seq)
	if err != nil || !hasDeliveryItemLink(order, req.ItemTaskID, req.ItemID, req.ItemRevision) {
		if err != nil {
			return out, err
		}
		return out, workItemConflict("work-order message is not immutably linked to the exact item revision")
	}
	item, err := getWorkItem(tx, ctx, req.ItemTaskID, req.ItemID)
	if err != nil {
		return out, err
	}
	if item.Revision != req.ItemRevision || item.Status == "done" || item.Status == "dismissed" {
		return out, workItemConflict("work item revision or status changed before directive creation")
	}
	var agentTask, currentRun, agentStatus string
	if err = tx.QueryRowContext(ctx, `SELECT task_id,run_id,status FROM agents WHERE id=?`, req.AgentID).Scan(&agentTask, &currentRun, &agentStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, api.ErrNotFound
		}
		return out, err
	}
	if agentTask != taskID || currentRun != req.RunID || agentStatus == api.AgentClosed || agentStatus == api.AgentExited {
		return out, workItemConflict("recipient is not the exact current actionable run")
	}
	if req.RecipientKind != api.DeliveryRecipientItemWorker && agentStatus == api.AgentRetired {
		return out, workItemConflict("retired shared-role runs cannot receive a new mandatory action")
	}
	anchor := api.RequiredDelivery{TaskID: taskID, AgentID: req.AgentID, RunID: req.RunID,
		ItemTaskID: req.ItemTaskID, ItemID: req.ItemID, ItemRevision: req.ItemRevision,
		WorkOrderMessage: req.WorkOrderMessage, RecipientKind: req.RecipientKind, ActionKey: req.ActionKey, ActionClass: req.ActionClass}
	contextDigest, err := validateDeliveryRecipient(tx, ctx, anchor)
	if err != nil {
		return out, err
	}
	prior, priorErr := currentRequiredDeliveryRowForAction(tx, ctx, taskID, req.ItemTaskID, req.ItemID, req.AgentID, req.RunID, req.RecipientKind, req.ActionKey)
	if errors.Is(priorErr, api.ErrNotFound) {
		if req.ExpectedCurrentGeneration != 0 || req.ExpectedCurrentDeliveryID != "" {
			return out, workItemConflict("directive generation changed; no current directive exists")
		}
	} else if priorErr != nil {
		return out, priorErr
	} else if req.ExpectedCurrentDeliveryID == "" {
		return out, workItemConflict("expected current delivery ID is required when superseding a directive")
	} else if prior.Generation != req.ExpectedCurrentGeneration || prior.ID != req.ExpectedCurrentDeliveryID {
		return out, workItemConflict(fmt.Sprintf("directive generation changed; current delivery is %s generation %d", prior.ID, prior.Generation))
	}
	now := s.now()
	// Old databases have a table-level uniqueness constraint that predates
	// action_key. Keep generation monotonic across sibling actions so the
	// additive migration can support them without rebuilding historical rows.
	var maxGeneration int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation),0) FROM required_deliveries WHERE task_id=? AND item_task_id=? AND item_id=? AND agent_id=? AND run_id=?`, taskID, req.ItemTaskID, req.ItemID, req.AgentID, req.RunID).Scan(&maxGeneration); err != nil {
		return out, err
	}
	generation := maxGeneration + 1
	policy := defaultDeliveryFollowThroughPolicy
	var nextDeadline, hardDeadline time.Time
	if reliableFollowThroughEnrollment(enrollmentVersion) {
		nextDeadline = now.Add(time.Duration(policy.AckDeadlineSeconds) * time.Second)
		hardDeadline = now.Add(time.Duration(policy.ActiveToolHardLimitSeconds) * time.Second)
	}
	d := api.RequiredDelivery{ID: api.NewID("dly"), TaskID: taskID, MessageSeq: req.MessageSeq, Kind: req.Kind,
		RecipientKind: req.RecipientKind, ActionKey: req.ActionKey, ActionClass: req.ActionClass,
		AgentID: req.AgentID, RunID: req.RunID, ItemTaskID: req.ItemTaskID, ItemID: req.ItemID, ItemRevision: req.ItemRevision,
		WorkOrderMessage: req.WorkOrderMessage, GoverningOrderMessage: governingOrder, InstructionSHA256: instructionSHA256,
		InstructionBytes: instructionBytes, EnrollmentVersion: enrollmentVersion, ContextDigest: contextDigest, Generation: generation, Current: true,
		Phase: api.DeliveryUnacknowledged, ExecutionEpoch: 1, CreatedAt: now, UpdatedAt: now,
		FollowThrough: api.DeliveryFollowThroughState{Policy: policy}}
	if reliableFollowThroughEnrollment(enrollmentVersion) {
		d.FollowThrough.NextDeadlineAt, d.FollowThrough.HardDeadlineAt = &nextDeadline, &hardDeadline
	}
	d.Message = &message
	if priorErr == nil {
		d.SupersedesID = prior.ID
		phase := prior.Phase
		if phase != api.DeliveryResult {
			phase = api.DeliverySuperseded
		}
		if _, err = tx.ExecContext(ctx, `UPDATE required_deliveries SET current=0,phase=?,next_substantive_deadline_at='',hard_deadline_at='',followthrough_pending_action='',followthrough_pending_request_id='',followthrough_pending_since='',updated_at=? WHERE id=? AND current=1 AND generation=?`, phase, ts(now), prior.ID, prior.Generation); err != nil {
			return out, err
		}
		if _, err = insertDeliveryEvent(ctx, tx, prior, "superseded", req.RequestID, req.ProducerAgentID, req.ProducerRunID, map[string]any{"byDeliveryId": d.ID, "generation": generation}, by, now); err != nil {
			return out, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO required_deliveries(id,task_id,message_seq,kind,recipient_kind,action_key,action_class,agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,governing_order_task_id,governing_order_message_seq,instruction_sha256,instruction_bytes,enrollment_version,context_digest,generation,supersedes_delivery_id,current,phase,execution_epoch,ack_deadline_seconds,progress_deadline_seconds,resume_deadline_seconds,confirmation_deadline_seconds,dispatch_report_seconds,active_tool_hard_limit_seconds,max_queue_attempts,next_substantive_deadline_at,hard_deadline_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.TaskID, d.MessageSeq, d.Kind, d.RecipientKind, d.ActionKey, d.ActionClass, d.AgentID, d.RunID, d.ItemTaskID, d.ItemID, d.ItemRevision,
		d.WorkOrderMessage.TaskID, d.WorkOrderMessage.Seq, d.GoverningOrderMessage.TaskID, d.GoverningOrderMessage.Seq,
		d.InstructionSHA256, d.InstructionBytes, d.EnrollmentVersion, d.ContextDigest, d.Generation, nullable(d.SupersedesID), 1, d.Phase, d.ExecutionEpoch,
		policy.AckDeadlineSeconds, policy.ProgressDeadlineSeconds, policy.ResumeDeadlineSeconds, policy.ConfirmationDeadlineSeconds,
		policy.DispatchReportSeconds, policy.ActiveToolHardLimitSeconds, policy.MaxQueueAttempts, optionalTS(nextDeadline), optionalTS(hardDeadline), ts(now), ts(now))
	if err != nil {
		return out, err
	}
	out.Delivery = d
	out.Event, err = insertDeliveryEvent(ctx, tx, d, "stored", req.RequestID, req.ProducerAgentID, req.ProducerRunID, map[string]any{
		"messageSeq": d.MessageSeq, "generation": d.Generation, "recipientKind": d.RecipientKind, "actionKey": d.ActionKey, "actionClass": d.ActionClass,
	}, by, now)
	if err != nil {
		return out, err
	}
	out.Delivery.Events = append(out.Delivery.Events, out.Event)
	if err = insertDeliveryReceipt(ctx, tx, taskID, "create", subject, req.ProducerAgentID, req.ProducerRunID, req.RequestID, payload, by, &out); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.notify(taskID)
	return out, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *Store) CurrentAssignment(ctx context.Context, taskID, agentID, runID string) (api.RequiredDelivery, error) {
	return s.CurrentAssignmentForResponsibility(ctx, taskID, agentID, runID, "", "")
}

func (s *Store) CurrentAssignmentForAction(ctx context.Context, taskID, agentID, runID, actionKey string) (api.RequiredDelivery, error) {
	return s.CurrentAssignmentForResponsibility(ctx, taskID, agentID, runID, "", actionKey)
}

func (s *Store) CurrentAssignmentForResponsibility(ctx context.Context, taskID, agentID, runID, itemID, actionKey string) (api.RequiredDelivery, error) {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(agentID, "agt") || !validRunID(runID) {
		return api.RequiredDelivery{}, api.ErrInvalid
	}
	if (itemID == "") != (actionKey == "") || (itemID != "" && !api.ValidID(itemID, "wi")) || (actionKey != "" && (strings.TrimSpace(actionKey) != actionKey || !api.ValidText(actionKey, 128))) {
		return api.RequiredDelivery{}, api.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.RequiredDelivery{}, err
	}
	defer tx.Rollback()
	var task, currentRun, status string
	if err = tx.QueryRowContext(ctx, `SELECT task_id,run_id,status FROM agents WHERE id=?`, agentID).Scan(&task, &currentRun, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return api.RequiredDelivery{}, api.ErrNotFound
		}
		return api.RequiredDelivery{}, err
	}
	if task != taskID {
		return api.RequiredDelivery{}, api.ErrNotFound
	}
	if currentRun != runID {
		return api.RequiredDelivery{}, workItemConflict("stale run; refresh the exact current assignment")
	}
	if status == api.AgentClosed || status == api.AgentExited {
		return api.RequiredDelivery{}, workItemConflict("closed or exited run has no actionable current assignment")
	}
	binding, err := loadAgentWorkItemBinding(tx, ctx, agentID, runID)
	if err != nil {
		return api.RequiredDelivery{}, err
	}
	var d api.RequiredDelivery
	if binding == nil {
		d, err = currentRoleRequiredDelivery(tx, ctx, taskID, agentID, runID, itemID, actionKey)
	} else {
		if itemID != "" && itemID != binding.ItemID {
			return api.RequiredDelivery{}, api.ErrNotFound
		}
		d, err = currentItemWorkerDelivery(tx, ctx, taskID, binding.ItemTaskID, binding.ItemID, agentID, runID, actionKey)
	}
	if err != nil {
		return api.RequiredDelivery{}, err
	}
	if err = validateDeliveryBinding(tx, ctx, d); err != nil {
		return api.RequiredDelivery{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.RequiredDelivery{}, err
	}
	return d, nil
}

// DeliveryCoverage exposes whether an exact run has a current, fully evidenced
// v2/v3 obligation. It never derives authority from
// Board prose, read cursors, Queue state, heartbeats, or roster labels.
func (s *Store) DeliveryCoverage(ctx context.Context, taskID, agentID, runID string) (api.DeliveryCoverage, error) {
	return s.DeliveryCoverageForResponsibility(ctx, taskID, agentID, runID, "", "")
}

func (s *Store) DeliveryCoverageForAction(ctx context.Context, taskID, agentID, runID, actionKey string) (api.DeliveryCoverage, error) {
	return s.DeliveryCoverageForResponsibility(ctx, taskID, agentID, runID, "", actionKey)
}

func (s *Store) DeliveryCoverageForResponsibility(ctx context.Context, taskID, agentID, runID, itemID, actionKey string) (api.DeliveryCoverage, error) {
	var out api.DeliveryCoverage
	if !api.ValidID(taskID, "tsk") || !api.ValidID(agentID, "agt") || !validRunID(runID) {
		return out, api.ErrInvalid
	}
	if (itemID == "") != (actionKey == "") || (itemID != "" && !api.ValidID(itemID, "wi")) || (actionKey != "" && (strings.TrimSpace(actionKey) != actionKey || !api.ValidText(actionKey, 128))) {
		return out, api.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var agentTask, currentRun, agentStatus string
	if err = tx.QueryRowContext(ctx, `SELECT task_id,run_id,status FROM agents WHERE id=?`, agentID).Scan(&agentTask, &currentRun, &agentStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, api.ErrNotFound
		}
		return out, err
	}
	if agentTask != taskID || currentRun != runID {
		return out, workItemConflict("stale run; refresh exact delivery coverage")
	}
	out.TaskID, out.AgentID, out.RunID = taskID, agentID, runID
	out.AgentStatus = agentStatus
	binding, err := loadAgentWorkItemBinding(tx, ctx, agentID, runID)
	if err != nil {
		return out, err
	}
	var d api.RequiredDelivery
	if binding == nil {
		d, err = currentRoleRequiredDelivery(tx, ctx, taskID, agentID, runID, itemID, actionKey)
		if errors.Is(err, api.ErrNotFound) {
			out.Status = api.DeliveryFollowThroughUncovered
			out.Reason = "the exact shared role run has no committed mandatory action; enrollment is required"
			return out, tx.Commit()
		}
		if err != nil {
			return out, err
		}
		out.ItemTaskID, out.ItemID, out.ItemRevision = d.ItemTaskID, d.ItemID, d.ItemRevision
		out.BindingWorkOrderMessage, out.ContextDigest = d.WorkOrderMessage, d.ContextDigest
	} else {
		if itemID != "" && itemID != binding.ItemID {
			return out, api.ErrNotFound
		}
		out.ItemTaskID, out.ItemID, out.ItemRevision = binding.ItemTaskID, binding.ItemID, binding.ItemRevision
		out.BindingWorkOrderMessage, out.ContextDigest = binding.WorkOrderMessage, binding.ContextDigest
	}
	item, err := getWorkItem(tx, ctx, out.ItemTaskID, out.ItemID)
	if err != nil {
		return out, err
	}
	out.CurrentItemRevision = item.Revision
	if item.Revision != out.ItemRevision || item.Status == "done" || item.Status == "dismissed" {
		out.Status = api.DeliveryFollowThroughUncovered
		out.Reason = "the admitted binding is not at the current actionable item revision; no follow-through action is authorized"
		return out, tx.Commit()
	}
	if binding != nil {
		d, err = currentItemWorkerDelivery(tx, ctx, taskID, binding.ItemTaskID, binding.ItemID, agentID, runID, actionKey)
		if errors.Is(err, api.ErrNotFound) {
			out.Status = api.DeliveryFollowThroughUncovered
			out.Reason = "the exact bound worker has no committed current obligation; enrollment is required"
			return out, tx.Commit()
		}
		if err != nil {
			return out, err
		}
	}
	out.Delivery = &d
	if agentStatus == api.AgentRetired {
		out.Status = api.DeliveryFollowThroughRetired
		out.Reason = "the exact run is intentionally retired and cannot receive automatic follow-through"
	} else if agentStatus == api.AgentClosed || agentStatus == api.AgentExited {
		out.Status = api.DeliveryFollowThroughClosed
		out.Reason = "the exact run is terminal and cannot receive automatic follow-through"
	} else if !reliableFollowThroughEnrollment(d.EnrollmentVersion) || d.GoverningOrderMessage.Seq < 1 || d.InstructionSHA256 == "" || d.InstructionBytes < 1 {
		out.Status = api.DeliveryFollowThroughLegacyEnrollment
		out.Reason = "the current directive predates full immutable governing-order enrollment and is not follow-through authority"
	} else {
		out.Status = "covered"
		out.Reason = fmt.Sprintf("the exact current directive has verified v%d governing-order and full-instruction evidence", d.EnrollmentVersion)
	}
	return out, tx.Commit()
}

// DeliveryCoverages enumerates each independently current responsibility for
// an exact run. Item plus action key is the selection identity; sibling actions
// never supersede or satisfy one another.
func (s *Store) DeliveryCoverages(ctx context.Context, taskID, agentID, runID string) (api.DeliveryCoverageList, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	out := api.DeliveryCoverageList{TaskID: taskID, AgentID: agentID, RunID: runID}
	if !api.ValidID(taskID, "tsk") || !api.ValidID(agentID, "agt") || !validRunID(runID) {
		return out, api.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT item_id,action_key FROM required_deliveries WHERE task_id=? AND agent_id=? AND run_id=? AND current=1 ORDER BY item_id,recipient_kind,action_key`, taskID, agentID, runID)
	if err != nil {
		return out, err
	}
	type actionIdentity struct{ itemID, actionKey string }
	var actions []actionIdentity
	for rows.Next() {
		var action actionIdentity
		if err = rows.Scan(&action.itemID, &action.actionKey); err != nil {
			rows.Close()
			return out, err
		}
		actions = append(actions, action)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	if len(actions) == 0 {
		coverage, coverageErr := s.DeliveryCoverageForAction(ctx, taskID, agentID, runID, "")
		if coverageErr != nil {
			return out, coverageErr
		}
		out.Actions = []api.DeliveryCoverage{coverage}
		return out, nil
	}
	for _, action := range actions {
		coverage, coverageErr := s.DeliveryCoverageForResponsibility(ctx, taskID, agentID, runID, action.itemID, action.actionKey)
		if coverageErr != nil {
			return out, coverageErr
		}
		out.Actions = append(out.Actions, coverage)
	}
	return out, nil
}

func validateDeliveryBinding(q queryRower, ctx context.Context, d api.RequiredDelivery) error {
	digest, err := validateDeliveryRecipient(q, ctx, d)
	if err != nil {
		return err
	}
	if digest != d.ContextDigest {
		return workItemConflict("delivery no longer matches the exact saved item/run/order/context binding")
	}
	return nil
}

type deliveryMutator func(context.Context, *sql.Tx, api.RequiredDelivery, time.Time) (string, map[string]any, *api.DeliveryBlock, error)

func (s *Store) mutateWorkerDelivery(ctx context.Context, taskID, deliveryID, operation, requestID, agentID, runID string, request any, by api.Caller, fn deliveryMutator) (api.DeliveryMutation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out api.DeliveryMutation
	if !api.ValidID(taskID, "tsk") || !validDeliveryID(deliveryID, "dly") || !validRequestID(requestID) || !api.ValidID(agentID, "agt") || !validRunID(runID) {
		return out, api.ErrInvalid
	}
	payload := requestHash(request)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if replay, replayErr := findDeliveryReplay(tx, ctx, taskID, operation, deliveryID, agentID, runID, requestID, payload, by); replayErr == nil {
		return replay, nil
	} else if !errors.Is(replayErr, api.ErrNotFound) {
		return out, replayErr
	}
	d, err := getRequiredDeliveryRow(tx, ctx, taskID, deliveryID)
	if err != nil {
		return out, err
	}
	var pauseState string
	if err = tx.QueryRowContext(ctx, `SELECT pause_state FROM tasks WHERE id=?`, taskID).Scan(&pauseState); err != nil {
		return out, err
	}
	if pauseState != api.ProjectPauseActive {
		return out, workItemConflict("project is paused; delivery follow-through is blocked")
	}
	if err = validateDeliveryBinding(tx, ctx, d); err != nil {
		return out, err
	}
	item, itemErr := getWorkItem(tx, ctx, d.ItemTaskID, d.ItemID)
	if itemErr != nil {
		return out, itemErr
	}
	if item.Revision != d.ItemRevision || item.Status == "done" || item.Status == "dismissed" {
		return out, workItemConflict("item revision or lifecycle changed; follow-through action is not authorized")
	}
	if d.AgentID != agentID || d.RunID != runID || !d.Current || d.Phase == api.DeliverySuperseded {
		current, _ := currentRequiredDeliveryRowForAction(tx, ctx, taskID, d.ItemTaskID, d.ItemID, d.AgentID, d.RunID, d.RecipientKind, d.ActionKey)
		return out, workItemConflict("superseded or wrong-run delivery; current delivery is " + current.ID)
	}
	var currentRun, status string
	if err = tx.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE id=? AND task_id=?`, agentID, taskID).Scan(&currentRun, &status); err != nil {
		return out, err
	}
	if currentRun != runID {
		return out, workItemConflict("stale run; refresh the exact current assignment")
	}
	if status == api.AgentRetired || status == api.AgentClosed || status == api.AgentExited {
		return out, workItemConflict("agent lifecycle state does not permit directive execution")
	}
	now := s.now()
	kind, data, block, err := fn(ctx, tx, d, now)
	if err != nil {
		return out, err
	}
	d, err = getRequiredDeliveryRow(tx, ctx, taskID, deliveryID)
	if err != nil {
		return out, err
	}
	out.Delivery, out.Block = d, block
	out.Event, err = insertDeliveryEvent(ctx, tx, d, kind, requestID, agentID, runID, data, by, now)
	if err != nil {
		return out, err
	}
	out.Delivery.Events = append(out.Delivery.Events, out.Event)
	if err = insertDeliveryReceipt(ctx, tx, taskID, operation, deliveryID, agentID, runID, requestID, payload, by, &out); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.notify(taskID)
	return out, nil
}

func (s *Store) AcknowledgeDelivery(ctx context.Context, taskID, deliveryID string, req api.DeliveryActionRequest, by api.Caller) (api.DeliveryMutation, error) {
	if req.ExpectedEpoch < 1 {
		return api.DeliveryMutation{}, api.ErrInvalid
	}
	return s.mutateWorkerDelivery(ctx, taskID, deliveryID, "ack", req.RequestID, req.AgentID, req.RunID, req, by, func(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, now time.Time) (string, map[string]any, *api.DeliveryBlock, error) {
		if req.ExpectedEpoch != d.ExecutionEpoch || d.Phase != api.DeliveryUnacknowledged {
			return "", nil, nil, workItemConflict("stale epoch or invalid directive transition to acknowledged from " + d.Phase)
		}
		next := now.Add(time.Duration(d.FollowThrough.Policy.ProgressDeadlineSeconds) * time.Second)
		hard := now.Add(time.Duration(d.FollowThrough.Policy.ActiveToolHardLimitSeconds) * time.Second)
		_, err := tx.ExecContext(ctx, `UPDATE required_deliveries SET phase=?,next_substantive_deadline_at=?,hard_deadline_at=?,followthrough_attempt_count=0,followthrough_pending_action='',followthrough_pending_request_id='',followthrough_pending_since='',followthrough_transport_outcome='',followthrough_confirmation_until='',followthrough_escalation_message_seq=0,updated_at=? WHERE id=?`, api.DeliveryAcknowledged, ts(next), ts(hard), ts(now), d.ID)
		return "acknowledged", map[string]any{"executionEpoch": d.ExecutionEpoch}, nil, err
	})
}

func (s *Store) ProgressDelivery(ctx context.Context, taskID, deliveryID string, req api.DeliveryActionRequest, by api.Caller) (api.DeliveryMutation, error) {
	if req.ExpectedEpoch < 1 || !api.ValidText(req.Text, api.MaxTextLen) {
		return api.DeliveryMutation{}, api.ErrInvalid
	}
	return s.mutateWorkerDelivery(ctx, taskID, deliveryID, "progress", req.RequestID, req.AgentID, req.RunID, req, by, func(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, now time.Time) (string, map[string]any, *api.DeliveryBlock, error) {
		if req.ExpectedEpoch != d.ExecutionEpoch || (d.Phase != api.DeliveryAcknowledged && d.Phase != api.DeliveryProgressing) {
			return "", nil, nil, workItemConflict("stale epoch or invalid directive progress transition from " + d.Phase)
		}
		next := now.Add(time.Duration(d.FollowThrough.Policy.ProgressDeadlineSeconds) * time.Second)
		hard := now.Add(time.Duration(d.FollowThrough.Policy.ActiveToolHardLimitSeconds) * time.Second)
		_, err := tx.ExecContext(ctx, `UPDATE required_deliveries SET phase=?,last_substantive_progress_at=?,next_substantive_deadline_at=?,hard_deadline_at=?,followthrough_attempt_count=0,followthrough_pending_action='',followthrough_pending_request_id='',followthrough_pending_since='',followthrough_transport_outcome='',followthrough_confirmation_until='',followthrough_escalation_message_seq=0,updated_at=? WHERE id=?`, api.DeliveryProgressing, ts(now), ts(next), ts(hard), ts(now), d.ID)
		return "progress", map[string]any{"text": req.Text, "executionEpoch": d.ExecutionEpoch}, nil, err
	})
}

func (s *Store) BlockDelivery(ctx context.Context, taskID, deliveryID string, req api.DeliveryBlockRequest, by api.Caller) (api.DeliveryMutation, error) {
	if req.ExpectedEpoch < 1 || !validBlockReason(req.ReasonClass) || !api.ValidText(req.Text, api.MaxTextLen) {
		return api.DeliveryMutation{}, api.ErrInvalid
	}
	return s.mutateWorkerDelivery(ctx, taskID, deliveryID, "block", req.RequestID, req.AgentID, req.RunID, req, by, func(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, now time.Time) (string, map[string]any, *api.DeliveryBlock, error) {
		if req.ExpectedEpoch != d.ExecutionEpoch || (d.Phase != api.DeliveryAcknowledged && d.Phase != api.DeliveryProgressing) {
			return "", nil, nil, workItemConflict("stale epoch or invalid directive block transition")
		}
		b := &api.DeliveryBlock{ID: api.NewID("dblk"), DeliveryID: d.ID, ExecutionEpoch: d.ExecutionEpoch, ReasonClass: req.ReasonClass, Text: req.Text, CreatedAt: now}
		if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_blocks(id,delivery_id,execution_epoch,reason_class,text,created_at) VALUES(?,?,?,?,?,?)`, b.ID, b.DeliveryID, b.ExecutionEpoch, b.ReasonClass, b.Text, ts(now)); err != nil {
			return "", nil, nil, err
		}
		_, err := tx.ExecContext(ctx, `UPDATE required_deliveries SET phase=?,current_block_id=?,last_substantive_progress_at=?,next_substantive_deadline_at='',hard_deadline_at='',followthrough_attempt_count=0,followthrough_pending_action='',followthrough_pending_request_id='',followthrough_pending_since='',followthrough_transport_outcome='',followthrough_confirmation_until='',followthrough_escalation_message_seq=0,updated_at=? WHERE id=?`, api.DeliveryBlocked, b.ID, ts(now), ts(now), d.ID)
		return "blocked", map[string]any{"blockId": b.ID, "reasonClass": b.ReasonClass, "executionEpoch": b.ExecutionEpoch}, b, err
	})
}

func validBlockReason(reason string) bool {
	switch reason {
	case "transient", "dependency", "permission", "authentication", "tool", "owner_input":
		return true
	}
	return false
}

func validIncidentTextList(values []string, required bool) bool {
	if required && len(values) == 0 || len(values) > 32 {
		return false
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || !api.ValidText(value, api.MaxTextLen) {
			return false
		}
	}
	return true
}

func (s *Store) RecordDeliveryRecoveryIncident(ctx context.Context, taskID, deliveryID string, req api.DeliveryRecoveryIncidentRequest, by api.Caller) (api.DeliveryRecoveryIncidentMutation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out api.DeliveryRecoveryIncidentMutation
	now := s.now()
	if !api.ValidID(taskID, "tsk") || !validDeliveryID(deliveryID, "dly") || !validRequestID(req.RequestID) ||
		!api.ValidID(req.AgentID, "agt") || !validRunID(req.RunID) || req.ExpectedGeneration < 1 || req.ExpectedEpoch < 1 ||
		(req.CauseStatus != api.DeliveryCauseUnknown && req.CauseStatus != api.DeliveryCauseEstablished) || req.LastSubstantiveAt.IsZero() || req.LastSubstantiveAt.After(now) ||
		strings.TrimSpace(req.LastSubstantiveAction) == "" || !api.ValidText(req.LastSubstantiveAction, api.MaxTextLen) ||
		strings.TrimSpace(req.ExpectedNextAction) == "" || !api.ValidText(req.ExpectedNextAction, api.MaxTextLen) ||
		strings.TrimSpace(req.StopReason) == "" || !api.ValidText(req.StopReason, api.MaxTextLen) ||
		!validIncidentTextList(req.CausalEvidence, req.CauseStatus == api.DeliveryCauseEstablished) ||
		!validIncidentTextList(req.ContributingConditions, false) || !validIncidentTextList(req.UnresolvedQuestions, req.CauseStatus == api.DeliveryCauseUnknown) ||
		!api.ValidID(req.Prevention.OwnerAgentID, "agt") || !validRunID(req.Prevention.OwnerRunID) ||
		!api.ValidID(req.Prevention.WorkOrderMessage.TaskID, "tsk") || req.Prevention.WorkOrderMessage.Seq < 1 ||
		strings.TrimSpace(req.Prevention.VerificationCriterion) == "" || !api.ValidText(req.Prevention.VerificationCriterion, api.MaxTextLen) ||
		(req.PriorIncidentID != "" && !validDeliveryID(req.PriorIncidentID, "dinc")) || !api.ValidText(req.PriorControlFailure, api.MaxTextLen) {
		return out, api.ErrInvalid
	}
	payload := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if replay, replayErr := findFollowThroughReplay(tx, ctx, taskID, "recovery_incident", deliveryID, req.AgentID, req.RunID, req.RequestID, payload, by, &out); replayErr != nil {
		return out, replayErr
	} else if replay {
		out.Replay = true
		return out, nil
	}
	d, err := getRequiredDeliveryRow(tx, ctx, taskID, deliveryID)
	if err != nil {
		return out, err
	}
	if err = validateDeliveryBinding(tx, ctx, d); err != nil {
		return out, err
	}
	if !d.Current || d.Generation != req.ExpectedGeneration || d.ExecutionEpoch != req.ExpectedEpoch || d.Phase == api.DeliveryResult || d.Phase == api.DeliverySuperseded {
		return out, workItemConflict("stale generation, epoch, or completed mandatory action")
	}
	if err = validateDeliveryProducer(tx, ctx, taskID, req.AgentID, req.RunID); err != nil {
		return out, err
	}
	var ownerRun, ownerStatus string
	if err = tx.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE id=? AND task_id=?`, req.Prevention.OwnerAgentID, taskID).Scan(&ownerRun, &ownerStatus); err != nil {
		return out, err
	}
	if ownerRun != req.Prevention.OwnerRunID || ownerStatus == api.AgentRetired || ownerStatus == api.AgentClosed || ownerStatus == api.AgentExited {
		return out, workItemConflict("prevention owner is not an exact current actionable run")
	}
	preventionOrder, err := loadMessage(tx, ctx, req.Prevention.WorkOrderMessage.TaskID, req.Prevention.WorkOrderMessage.Seq)
	if err != nil {
		return out, err
	}
	if !hasDeliveryItemLink(preventionOrder, d.ItemTaskID, d.ItemID, d.ItemRevision) {
		return out, workItemConflict("prevention work order is not linked to the exact action item revision")
	}
	prior, err := latestDeliveryRecoveryIncident(tx, ctx, d.ID)
	if err != nil {
		return out, err
	}
	if prior == nil {
		if req.PriorIncidentID != "" || strings.TrimSpace(req.PriorControlFailure) != "" {
			return out, workItemConflict("prior incident fields were supplied but no prior incident exists")
		}
	} else if req.PriorIncidentID != prior.ID {
		return out, workItemConflict("a subsequent causal record must link the latest prior incident")
	} else if prior.CauseStatus == api.DeliveryCauseEstablished && strings.TrimSpace(req.PriorControlFailure) == "" {
		return out, workItemConflict("recurrence must explain why the prior incident control failed")
	}
	causal, _ := json.Marshal(req.CausalEvidence)
	conditions, _ := json.Marshal(req.ContributingConditions)
	questions, _ := json.Marshal(req.UnresolvedQuestions)
	incident := api.DeliveryRecoveryIncident{
		ID: api.NewID("dinc"), DeliveryID: d.ID, Generation: d.Generation, ExecutionEpoch: d.ExecutionEpoch,
		CauseStatus: req.CauseStatus, LastSubstantiveAction: req.LastSubstantiveAction, LastSubstantiveAt: req.LastSubstantiveAt,
		ExpectedNextAction: req.ExpectedNextAction, StopReason: req.StopReason, CausalEvidence: req.CausalEvidence,
		ContributingConditions: req.ContributingConditions, UnresolvedQuestions: req.UnresolvedQuestions, Prevention: req.Prevention,
		PriorIncidentID: req.PriorIncidentID, PriorControlFailure: req.PriorControlFailure,
		RecordedByAgentID: req.AgentID, RecordedByRunID: req.RunID, CreatedAt: now,
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO delivery_recovery_incidents(id,delivery_id,generation,execution_epoch,cause_status,last_substantive_action,last_substantive_at,expected_next_action,stop_reason,causal_evidence,contributing_conditions,unresolved_questions,prevention_owner_agent_id,prevention_owner_run_id,prevention_work_order_task_id,prevention_work_order_message_seq,prevention_verification_criterion,prior_incident_id,prior_control_failure,recorded_by_agent_id,recorded_by_run_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		incident.ID, incident.DeliveryID, incident.Generation, incident.ExecutionEpoch, incident.CauseStatus,
		incident.LastSubstantiveAction, ts(incident.LastSubstantiveAt), incident.ExpectedNextAction, incident.StopReason,
		string(causal), string(conditions), string(questions), incident.Prevention.OwnerAgentID, incident.Prevention.OwnerRunID,
		incident.Prevention.WorkOrderMessage.TaskID, incident.Prevention.WorkOrderMessage.Seq, incident.Prevention.VerificationCriterion,
		incident.PriorIncidentID, incident.PriorControlFailure, incident.RecordedByAgentID, incident.RecordedByRunID, ts(now))
	if err != nil {
		return out, err
	}
	if req.CauseStatus == api.DeliveryCauseEstablished {
		if _, err = tx.ExecContext(ctx, `UPDATE required_deliveries SET followthrough_escalation_message_seq=0,updated_at=? WHERE id=?`, ts(now), d.ID); err != nil {
			return out, err
		}
	}
	out.Incident = incident
	out.Event, err = insertDeliveryEvent(ctx, tx, d, "recovery_incident_recorded", req.RequestID, req.AgentID, req.RunID, map[string]any{
		"incidentId": incident.ID, "causeStatus": incident.CauseStatus, "preventionOwnerAgentId": incident.Prevention.OwnerAgentID,
		"preventionWorkOrderMessageSeq": incident.Prevention.WorkOrderMessage.Seq,
	}, by, now)
	if err != nil {
		return out, err
	}
	d, err = getRequiredDeliveryRow(tx, ctx, taskID, deliveryID)
	if err != nil {
		return out, err
	}
	d.LatestRecoveryIncident = &incident
	d.Events, err = loadDeliveryEvents(tx, ctx, d.ID)
	if err != nil {
		return out, err
	}
	out.Delivery = d
	if err = insertFollowThroughReceipt(ctx, tx, taskID, "recovery_incident", deliveryID, req.AgentID, req.RunID, req.RequestID, payload, by, now, &out.Receipt, &out); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.notify(taskID)
	return out, nil
}

func (s *Store) ResultDelivery(ctx context.Context, taskID, deliveryID string, req api.DeliveryActionRequest, by api.Caller) (api.DeliveryMutation, error) {
	if req.ExpectedEpoch < 1 || strings.TrimSpace(req.Text) == "" || !api.ValidText(req.Text, api.MaxTextLen) {
		return api.DeliveryMutation{}, api.ErrInvalid
	}
	return s.mutateWorkerDelivery(ctx, taskID, deliveryID, "result", req.RequestID, req.AgentID, req.RunID, req, by, func(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, now time.Time) (string, map[string]any, *api.DeliveryBlock, error) {
		if req.ExpectedEpoch != d.ExecutionEpoch || (d.Phase != api.DeliveryAcknowledged && d.Phase != api.DeliveryProgressing) {
			return "", nil, nil, workItemConflict("stale epoch or invalid directive result transition from " + d.Phase)
		}
		_, err := tx.ExecContext(ctx, `UPDATE required_deliveries SET phase=?,result_text=?,last_substantive_progress_at=?,next_substantive_deadline_at='',hard_deadline_at='',followthrough_pending_action='',followthrough_pending_request_id='',followthrough_pending_since='',followthrough_transport_outcome='',followthrough_confirmation_until='',updated_at=? WHERE id=?`, api.DeliveryResult, req.Text, ts(now), ts(now), d.ID)
		return "result", map[string]any{"text": req.Text, "executionEpoch": d.ExecutionEpoch}, nil, err
	})
}

func (s *Store) ResolveDeliveryBlock(ctx context.Context, taskID, deliveryID, blockID string, req api.DeliveryResolutionRequest, by api.Caller) (api.DeliveryMutation, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out api.DeliveryMutation
	if !api.ValidID(taskID, "tsk") || !validDeliveryID(deliveryID, "dly") || !validDeliveryID(blockID, "dblk") || !validRequestID(req.RequestID) || req.ExpectedEpoch < 1 || strings.TrimSpace(req.Text) == "" || !api.ValidText(req.Text, api.MaxTextLen) || ((req.AgentID == "") != (req.RunID == "")) {
		return out, api.ErrInvalid
	}
	payload := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if replay, replayErr := findDeliveryReplay(tx, ctx, taskID, "resolve", blockID, req.AgentID, req.RunID, req.RequestID, payload, by); replayErr == nil {
		return replay, nil
	} else if !errors.Is(replayErr, api.ErrNotFound) {
		return out, replayErr
	}
	d, err := getRequiredDeliveryRow(tx, ctx, taskID, deliveryID)
	if err != nil {
		return out, err
	}
	if err = validateDeliveryBinding(tx, ctx, d); err != nil {
		return out, err
	}
	var recipientRun, recipientStatus string
	if err = tx.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE id=? AND task_id=?`, d.AgentID, taskID).Scan(&recipientRun, &recipientStatus); err != nil {
		return out, err
	}
	if recipientRun != d.RunID || recipientStatus == api.AgentClosed || recipientStatus == api.AgentExited {
		return out, workItemConflict("directive recipient is no longer the exact current retained run")
	}
	b, err := getDeliveryBlock(tx, ctx, deliveryID, blockID)
	if err != nil {
		return out, err
	}
	if !d.Current || d.Phase != api.DeliveryBlocked || d.CurrentBlockID != blockID || b.Resolved || b.ExecutionEpoch != req.ExpectedEpoch {
		return out, workItemConflict("block is not the unresolved current directive epoch")
	}
	if err = validateResolutionActor(tx, ctx, d, b, req.AgentID, req.RunID); err != nil {
		return out, err
	}
	now := s.now()
	b.Resolved, b.ResolutionID, b.ResolutionText = true, api.NewID("drsl"), req.Text
	b.ResolvedAt = &now
	if _, err = tx.ExecContext(ctx, `UPDATE delivery_blocks SET resolved=1,resolution_id=?,resolution_text=?,resolved_at=? WHERE id=? AND resolved=0`, b.ResolutionID, b.ResolutionText, ts(now), b.ID); err != nil {
		return out, err
	}
	next := now.Add(time.Duration(d.FollowThrough.Policy.ResumeDeadlineSeconds) * time.Second)
	hard := now.Add(time.Duration(d.FollowThrough.Policy.ActiveToolHardLimitSeconds) * time.Second)
	if _, err = tx.ExecContext(ctx, `UPDATE required_deliveries SET next_substantive_deadline_at=?,hard_deadline_at=?,followthrough_attempt_count=0,followthrough_pending_action='',followthrough_pending_request_id='',followthrough_pending_since='',followthrough_transport_outcome='',followthrough_confirmation_until='',followthrough_escalation_message_seq=0,updated_at=? WHERE id=?`, ts(next), ts(hard), ts(now), d.ID); err != nil {
		return out, err
	}
	out.Delivery, out.Block = d, &b
	out.Event, err = insertDeliveryEvent(ctx, tx, d, "block_resolved", req.RequestID, req.AgentID, req.RunID, map[string]any{"blockId": b.ID, "resolutionId": b.ResolutionID, "executionEpoch": b.ExecutionEpoch}, by, now)
	if err != nil {
		return out, err
	}
	out.Delivery.Events = append(out.Delivery.Events, out.Event)
	if err = insertDeliveryReceipt(ctx, tx, taskID, "resolve", blockID, req.AgentID, req.RunID, req.RequestID, payload, by, &out); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.notify(taskID)
	return out, nil
}

func validateResolutionActor(q queryRower, ctx context.Context, d api.RequiredDelivery, b api.DeliveryBlock, agentID, runID string) error {
	if agentID == "" && runID == "" {
		return nil
	}
	if !api.ValidID(agentID, "agt") || !validRunID(runID) {
		return api.ErrInvalid
	}
	var currentRun, status, role, name, orchestrator string
	if err := q.QueryRowContext(ctx, `SELECT a.run_id,a.status,a.role,a.name,t.orchestrator FROM agents a JOIN tasks t ON t.id=a.task_id WHERE a.task_id=? AND a.id=?`, d.TaskID, agentID).Scan(&currentRun, &status, &role, &name, &orchestrator); err != nil {
		return err
	}
	if currentRun != runID || status == api.AgentRetired || status == api.AgentClosed || status == api.AgentExited {
		return workItemConflict("resolution actor is not a current actionable run")
	}
	if agentID == d.AgentID && runID == d.RunID && (b.ReasonClass == "transient" || b.ReasonClass == "tool") {
		return nil
	}
	if role == api.AgentRoleDatabaseHandler || name == orchestrator {
		return nil
	}
	return workItemConflict("resolution actor is not authorized for this block class")
}

func (s *Store) ResumeDelivery(ctx context.Context, taskID, deliveryID string, req api.DeliveryResumeRequest, by api.Caller) (api.DeliveryMutation, error) {
	if req.ExpectedEpoch < 1 || !validDeliveryID(req.ResolutionID, "drsl") {
		return api.DeliveryMutation{}, api.ErrInvalid
	}
	return s.mutateWorkerDelivery(ctx, taskID, deliveryID, "resume", req.RequestID, req.AgentID, req.RunID, req, by, func(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, now time.Time) (string, map[string]any, *api.DeliveryBlock, error) {
		if d.Phase != api.DeliveryBlocked || d.ExecutionEpoch != req.ExpectedEpoch || d.CurrentBlockID == "" {
			return "", nil, nil, workItemConflict("stale epoch or directive is not blocked")
		}
		b, err := getDeliveryBlock(tx, ctx, d.ID, d.CurrentBlockID)
		if err != nil {
			return "", nil, nil, err
		}
		if !b.Resolved || b.ResolutionID != req.ResolutionID {
			return "", nil, nil, workItemConflict("current block has not been resolved with the supplied resolution")
		}
		requiresIncident := b.ReasonClass == "transient" || b.ReasonClass == "tool"
		incidentNotBefore := b.CreatedAt
		if d.FollowThrough.NextDeadlineAt != nil && !now.Before(*d.FollowThrough.NextDeadlineAt) {
			requiresIncident = true
			if b.ResolvedAt != nil {
				incidentNotBefore = *b.ResolvedAt
			}
		}
		if requiresIncident {
			incident, incidentErr := latestDeliveryRecoveryIncident(tx, ctx, d.ID)
			if incidentErr != nil {
				return "", nil, nil, incidentErr
			}
			if incident == nil || incident.Generation != d.Generation || incident.ExecutionEpoch != d.ExecutionEpoch || incident.CauseStatus != api.DeliveryCauseEstablished || incident.CreatedAt.Before(incidentNotBefore) {
				return "", nil, nil, workItemConflict("unexpected stop requires an evidence-backed causal incident and assigned prevention before resume")
			}
		}
		newEpoch := d.ExecutionEpoch + 1
		next := now.Add(time.Duration(d.FollowThrough.Policy.ProgressDeadlineSeconds) * time.Second)
		hard := now.Add(time.Duration(d.FollowThrough.Policy.ActiveToolHardLimitSeconds) * time.Second)
		_, err = tx.ExecContext(ctx, `UPDATE required_deliveries SET phase=?,execution_epoch=?,current_block_id=NULL,next_substantive_deadline_at=?,hard_deadline_at=?,followthrough_attempt_count=0,followthrough_pending_action='',followthrough_pending_request_id='',followthrough_pending_since='',followthrough_transport_outcome='',followthrough_confirmation_until='',followthrough_escalation_message_seq=0,updated_at=? WHERE id=?`, api.DeliveryAcknowledged, newEpoch, ts(next), ts(hard), ts(now), d.ID)
		return "resumed", map[string]any{"blockId": b.ID, "resolutionId": b.ResolutionID, "executionEpoch": newEpoch}, &b, err
	})
}

func validRuntimeObservation(observation api.DeliveryRuntimeObservation, now time.Time) bool {
	if observation.State != api.DeliveryObservationUnknown && observation.State != api.DeliveryObservationActiveTool && observation.State != api.DeliveryObservationIdlePrompt {
		return false
	}
	if strings.TrimSpace(observation.Source) == "" || !api.ValidText(observation.Source, 256) || observation.ObservedAt.IsZero() {
		return false
	}
	if observation.ObservedAt.After(now.Add(time.Minute)) || now.Sub(observation.ObservedAt) > 2*time.Minute {
		return false
	}
	return observation.ActivityID == "" || api.ValidText(observation.ActivityID, 256)
}

func findFollowThroughReplay(q queryRower, ctx context.Context, taskID, operation, deliveryID, agentID, runID, requestID, payload string, by api.Caller, out any) (bool, error) {
	var priorSubject, priorHash string
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT subject_id,payload_hash,response_json FROM delivery_operation_receipts WHERE task_id=? AND operation=? AND actor_agent_id=? AND actor_run_id=? AND by_node=? AND by_user=? AND request_id=?`,
		taskID, operation, agentID, runID, by.Node, by.User, requestID).Scan(&priorSubject, &priorHash, &raw)
	if err == nil {
		if priorSubject != deliveryID || priorHash != payload {
			return false, workItemConflict("request ID was already used with different follow-through data")
		}
		return true, json.Unmarshal(raw, out)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func insertFollowThroughReceipt(ctx context.Context, tx *sql.Tx, taskID, operation, deliveryID, agentID, runID, requestID, payload string, by api.Caller, createdAt time.Time, receipt *api.DeliveryReceipt, out any) error {
	*receipt = api.DeliveryReceipt{ID: api.NewID("drr"), RequestID: requestID, Operation: operation, CreatedAt: createdAt}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO delivery_operation_receipts(receipt_id,task_id,operation,subject_id,actor_agent_id,actor_run_id,by_node,by_user,request_id,payload_hash,response_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		receipt.ID, taskID, operation, deliveryID, agentID, runID, by.Node, by.User, requestID, payload, raw, ts(createdAt))
	return err
}

func initializeDeliveryDeadline(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, now time.Time) (api.RequiredDelivery, error) {
	if !reliableFollowThroughEnrollment(d.EnrollmentVersion) || d.FollowThrough.NextDeadlineAt != nil || d.Phase == api.DeliveryResult || d.Phase == api.DeliverySuperseded {
		return d, nil
	}
	base := d.UpdatedAt
	duration := time.Duration(d.FollowThrough.Policy.ProgressDeadlineSeconds) * time.Second
	switch d.Phase {
	case api.DeliveryUnacknowledged:
		base = d.CreatedAt
		duration = time.Duration(d.FollowThrough.Policy.AckDeadlineSeconds) * time.Second
	case api.DeliveryBlocked:
		if d.CurrentBlock == nil || !d.CurrentBlock.Resolved || d.CurrentBlock.ResolvedAt == nil {
			return d, nil
		}
		base = *d.CurrentBlock.ResolvedAt
		duration = time.Duration(d.FollowThrough.Policy.ResumeDeadlineSeconds) * time.Second
	}
	next := base.Add(duration)
	hard := base.Add(time.Duration(d.FollowThrough.Policy.ActiveToolHardLimitSeconds) * time.Second)
	if _, err := tx.ExecContext(ctx, `UPDATE required_deliveries SET next_substantive_deadline_at=?,hard_deadline_at=? WHERE id=? AND current=1`, ts(next), ts(hard), d.ID); err != nil {
		return d, err
	}
	d.FollowThrough.NextDeadlineAt, d.FollowThrough.HardDeadlineAt = &next, &hard
	return d, nil
}

func deliveryFollowThroughClassification(d api.RequiredDelivery, status string, observation api.DeliveryRuntimeObservation, now time.Time) (string, string, bool) {
	if !reliableFollowThroughEnrollment(d.EnrollmentVersion) {
		return api.DeliveryFollowThroughLegacyEnrollment, "the directive lacks full immutable v2 enrollment evidence", false
	}
	if status == api.AgentRetired {
		return api.DeliveryFollowThroughRetired, "retired runs are intentionally retained and are never auto-resumed", false
	}
	if status == api.AgentClosed || status == api.AgentExited || !d.Current || d.Phase == api.DeliverySuperseded {
		return api.DeliveryFollowThroughClosed, "terminal or superseded work has no executable follow-through", false
	}
	if d.Phase == api.DeliveryResult {
		return api.DeliveryFollowThroughIdleNoObligation, "the directive has a result; item acceptance remains separate", false
	}
	if d.Phase == api.DeliveryBlocked {
		if d.CurrentBlock == nil || !d.CurrentBlock.Resolved {
			return api.DeliveryFollowThroughKnownBlock, "the current exact epoch has an unresolved durable block", false
		}
		if d.FollowThrough.NextDeadlineAt != nil && now.Before(*d.FollowThrough.NextDeadlineAt) {
			return api.DeliveryFollowThroughResolvedAwaitingResume, "the resolved block is within its worker-resume deadline", false
		}
	}
	if d.FollowThrough.EscalationMessageSeq > 0 {
		return api.DeliveryFollowThroughEscalated, "the unresolved incident was already escalated", false
	}
	if d.FollowThrough.PendingRequestID != "" {
		if d.FollowThrough.TransportOutcome == "" {
			if d.FollowThrough.PendingSince != nil && now.Before(d.FollowThrough.PendingSince.Add(time.Duration(d.FollowThrough.Policy.DispatchReportSeconds)*time.Second)) {
				return api.DeliveryFollowThroughTransportUnconfirmed, "a leased exact-thread action is awaiting its durable transport report", false
			}
			return api.DeliveryFollowThroughDispatchAmbiguous, "the leased exact-thread action has no durable outcome and will not be repeated", true
		}
		if d.FollowThrough.TransportOutcome == api.DeliveryFollowThroughOutcomeAccepted && d.FollowThrough.ConfirmationUntil != nil && now.Before(*d.FollowThrough.ConfirmationUntil) {
			return api.DeliveryFollowThroughTransportUnconfirmed, "native queue acceptance is awaiting exact directive ack or substantive progress", false
		}
		if d.FollowThrough.TransportOutcome == api.DeliveryFollowThroughOutcomeFailed || d.FollowThrough.TransportOutcome == api.DeliveryFollowThroughOutcomeAmbiguous {
			return api.DeliveryFollowThroughDispatchAmbiguous, "the transport outcome is failed or ambiguous and will not be retried automatically", true
		}
	}
	if d.FollowThrough.NextDeadlineAt != nil && now.Before(*d.FollowThrough.NextDeadlineAt) {
		switch d.Phase {
		case api.DeliveryUnacknowledged:
			return api.DeliveryFollowThroughAwaitingAck, "the current directive is within its exact-run acknowledgment deadline", false
		case api.DeliveryProgressing:
			return api.DeliveryFollowThroughRecentProgress, "substantive progress is within its declared deadline", false
		case api.DeliveryBlocked:
			return api.DeliveryFollowThroughResolvedAwaitingResume, "the resolved block is within its worker-resume deadline", false
		default:
			return api.DeliveryFollowThroughAwaitingProgress, "acknowledged execution is within its substantive-progress deadline", false
		}
	}
	if observation.State == api.DeliveryObservationActiveTool && d.FollowThrough.HardDeadlineAt != nil && now.Before(*d.FollowThrough.HardDeadlineAt) {
		return api.DeliveryFollowThroughActiveTool, "a supported observer reports an active tool; silence alone does not interrupt it", false
	}
	if observation.State == api.DeliveryObservationActiveTool {
		return api.DeliveryFollowThroughActiveTool, "the active-tool hard limit elapsed; escalate without interrupting the tool", true
	}
	if d.EnrollmentVersion == api.ReliableMandatoryActionCapabilityVersion && (d.LatestRecoveryIncident == nil || d.LatestRecoveryIncident.Generation != d.Generation ||
		d.LatestRecoveryIncident.ExecutionEpoch != d.ExecutionEpoch || d.LatestRecoveryIncident.CauseStatus != api.DeliveryCauseEstablished || d.LatestRecoveryIncident.CreatedAt.Before(d.UpdatedAt)) {
		return api.DeliveryFollowThroughCauseRequired, "unexpected inactivity has no evidence-backed causal record and assigned bounded prevention; escalate without a bare restart", true
	}
	if d.FollowThrough.AttemptCount >= d.FollowThrough.Policy.MaxQueueAttempts {
		return api.DeliveryFollowThroughTransportUnconfirmed, "bounded exact-thread attempts produced no ack or substantive progress", true
	}
	if observation.State == api.DeliveryObservationIdlePrompt {
		return api.DeliveryFollowThroughOverdueIdlePrompt, "the exact current directive is overdue and the runtime prompt is observably idle", true
	}
	return api.DeliveryFollowThroughOverdueUnknown, "the exact current directive is overdue and runtime execution evidence is unknown", true
}

func (s *Store) escalateDeliveryFollowThrough(ctx context.Context, tx *sql.Tx, d api.RequiredDelivery, classification, requestID string, observation api.DeliveryRuntimeObservation, by api.Caller, now time.Time) (api.Message, api.DeliveryEvent, error) {
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, d.TaskID))
	if err != nil {
		return api.Message{}, api.DeliveryEvent{}, err
	}
	var target api.Agent
	var count int
	rows, err := tx.QueryContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND name=? AND role=''`, d.TaskID, task.Orchestrator)
	if err != nil {
		return api.Message{}, api.DeliveryEvent{}, err
	}
	for rows.Next() {
		target, err = scanAgent(rows)
		if err != nil {
			rows.Close()
			return api.Message{}, api.DeliveryEvent{}, err
		}
		count++
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return api.Message{}, api.DeliveryEvent{}, err
	}
	rows.Close()
	if count != 1 || target.Status == api.AgentRetired || target.Status == api.AgentClosed || target.Status == api.AgentExited {
		return api.Message{}, api.DeliveryEvent{}, workItemConflict("no unique actionable current orchestrator is available for follow-through escalation")
	}
	text := fmt.Sprintf("Reliable follow-through escalation: delivery %s generation %d epoch %d for item %s@%d, recipient %s %s/%s, action %s (%s) remains unresolved (%s). Runtime observation=%s source=%s; saved order, heartbeat, Working, inbox reads, turn end and queue acceptance are not substantive progress and cannot satisfy a sibling action. Review the exact current action before any recovery.", d.ID, d.Generation, d.ExecutionEpoch, d.ItemID, d.ItemRevision, d.RecipientKind, d.AgentID, d.RunID, d.ActionKey, d.ActionClass, classification, observation.State, observation.Source)
	escalationSeed := fmt.Sprintf("%s:%d:%d:%v:%s:%d", d.ID, d.Generation, d.ExecutionEpoch, d.FollowThrough.NextDeadlineAt, d.FollowThrough.PendingRequestID, d.FollowThrough.AttemptCount)
	messageRequestID := fmt.Sprintf("delivery-escalation-%x", sha256.Sum256([]byte(escalationSeed)))
	req := api.PostMessageRequest{To: target.ID, Text: text, RequestID: messageRequestID,
		WorkItems:        []api.MessageWorkItem{{ItemTaskID: d.ItemTaskID, ItemID: d.ItemID, ItemRevision: d.ItemRevision, Relationship: "primary"}},
		WorkOrderMessage: &d.WorkOrderMessage}
	message, err := s.insertMessageWithResume(ctx, tx, task, req, target, api.Caller{Node: "system", User: "delivery-follow-through"}, false, false, false)
	if err != nil {
		return api.Message{}, api.DeliveryEvent{}, err
	}
	if err = insertMessagePostReceipt(ctx, tx, &message, req.RequestID, requestHash(req), api.Caller{Node: "system", User: "delivery-follow-through"}); err != nil {
		return api.Message{}, api.DeliveryEvent{}, err
	}
	event, err := insertDeliveryEvent(ctx, tx, d, "followthrough_escalated", requestID, d.AgentID, d.RunID, map[string]any{"classification": classification, "messageSeq": message.Seq, "generation": d.Generation, "executionEpoch": d.ExecutionEpoch}, by, now)
	return message, event, err
}

// CheckDeliveryFollowThrough classifies one exact current directive and, only
// when due, transactionally owns either one queue lease or one escalation.
// Ordinary heartbeat/read/roster state is deliberately absent from the input.
func (s *Store) CheckDeliveryFollowThrough(ctx context.Context, taskID, deliveryID string, req api.DeliveryFollowThroughCheckRequest, by api.Caller) (api.DeliveryFollowThroughDecision, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out api.DeliveryFollowThroughDecision
	now := s.now()
	if !api.ValidID(taskID, "tsk") || !validDeliveryID(deliveryID, "dly") || !validRequestID(req.RequestID) || !api.ValidID(req.AgentID, "agt") || !validRunID(req.RunID) || req.ExpectedGeneration < 1 || req.ExpectedEpoch < 1 || !validRuntimeObservation(req.Observation, now) {
		return out, api.ErrInvalid
	}
	payload := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if replay, replayErr := findFollowThroughReplay(tx, ctx, taskID, "followthrough_check", deliveryID, req.AgentID, req.RunID, req.RequestID, payload, by, &out); replayErr != nil {
		return out, replayErr
	} else if replay {
		out.Replay, out.Execute = true, false
		return out, nil
	}
	d, err := getRequiredDeliveryRow(tx, ctx, taskID, deliveryID)
	if err != nil {
		return out, err
	}
	var pauseState string
	if err = tx.QueryRowContext(ctx, `SELECT pause_state FROM tasks WHERE id=?`, taskID).Scan(&pauseState); err != nil {
		return out, err
	}
	if pauseState != api.ProjectPauseActive {
		return out, workItemConflict("project is paused; delivery follow-through is blocked")
	}
	if err = validateDeliveryBinding(tx, ctx, d); err != nil {
		return out, err
	}
	item, err := getWorkItem(tx, ctx, d.ItemTaskID, d.ItemID)
	if err != nil {
		return out, err
	}
	if item.Revision != d.ItemRevision || item.Status == "done" || item.Status == "dismissed" {
		return out, workItemConflict("item revision or lifecycle changed; follow-through action is not authorized")
	}
	if d.AgentID != req.AgentID || d.RunID != req.RunID || d.Generation != req.ExpectedGeneration || d.ExecutionEpoch != req.ExpectedEpoch || !d.Current {
		return out, workItemConflict("stale run, generation, epoch, or superseded delivery")
	}
	var currentRun, status string
	if err = tx.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE id=? AND task_id=?`, req.AgentID, taskID).Scan(&currentRun, &status); err != nil {
		return out, err
	}
	if currentRun != req.RunID {
		return out, workItemConflict("stale run; refresh the exact current assignment")
	}
	d, err = hydrateRequiredDelivery(tx, ctx, d)
	if err != nil {
		return out, err
	}
	d, err = initializeDeliveryDeadline(ctx, tx, d, now)
	if err != nil {
		return out, err
	}
	classification, reason, due := deliveryFollowThroughClassification(d, status, req.Observation, now)
	out.Delivery, out.Classification, out.Reason, out.Action = d, classification, reason, api.DeliveryFollowThroughActionNone
	if !due || status == api.AgentRetired || status == api.AgentClosed || status == api.AgentExited || d.Phase == api.DeliveryResult || d.Phase == api.DeliverySuperseded {
		if err = tx.Commit(); err != nil {
			return out, err
		}
		return out, nil
	}
	mustEscalate := classification == api.DeliveryFollowThroughActiveTool || classification == api.DeliveryFollowThroughCauseRequired || classification == api.DeliveryFollowThroughDispatchAmbiguous || d.FollowThrough.AttemptCount >= d.FollowThrough.Policy.MaxQueueAttempts
	if mustEscalate {
		message, event, escalationErr := s.escalateDeliveryFollowThrough(ctx, tx, d, classification, req.RequestID, req.Observation, by, now)
		if escalationErr != nil {
			return out, escalationErr
		}
		if _, err = tx.ExecContext(ctx, `UPDATE required_deliveries SET followthrough_escalation_message_seq=?,next_substantive_deadline_at='',followthrough_pending_action='',followthrough_pending_request_id='',followthrough_pending_since='',updated_at=? WHERE id=?`, message.Seq, ts(now), d.ID); err != nil {
			return out, err
		}
		out.Classification, out.Reason, out.EscalationMessageSeq, out.Event = api.DeliveryFollowThroughEscalated, reason, message.Seq, &event
		out.Delivery.FollowThrough.EscalationMessageSeq = message.Seq
		out.Delivery.FollowThrough.NextDeadlineAt = nil
		var receipt api.DeliveryReceipt
		out.Receipt = &receipt
		if err = insertFollowThroughReceipt(ctx, tx, taskID, "followthrough_check", deliveryID, req.AgentID, req.RunID, req.RequestID, payload, by, now, &receipt, &out); err != nil {
			return out, err
		}
		if err = tx.Commit(); err != nil {
			return out, err
		}
		s.notify(taskID)
		return out, nil
	}
	attempt := d.FollowThrough.AttemptCount + 1
	event, err := insertDeliveryEvent(ctx, tx, d, "followthrough_queue_leased", req.RequestID, req.AgentID, req.RunID, map[string]any{"attempt": attempt, "classification": classification, "generation": d.Generation, "executionEpoch": d.ExecutionEpoch, "observation": req.Observation.State}, by, now)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE required_deliveries SET followthrough_attempt_count=?,followthrough_pending_action=?,followthrough_pending_request_id=?,followthrough_pending_since=?,followthrough_transport_outcome='',followthrough_confirmation_until='',updated_at=? WHERE id=?`, attempt, api.DeliveryFollowThroughActionQueue, req.RequestID, ts(now), ts(now), d.ID); err != nil {
		return out, err
	}
	out.Action, out.Attempt, out.Execute, out.Event = api.DeliveryFollowThroughActionQueue, attempt, true, &event
	out.Delivery.FollowThrough.AttemptCount = attempt
	out.Delivery.FollowThrough.PendingAction = out.Action
	out.Delivery.FollowThrough.PendingRequestID = req.RequestID
	out.Delivery.FollowThrough.PendingSince = &now
	var receipt api.DeliveryReceipt
	out.Receipt = &receipt
	if err = insertFollowThroughReceipt(ctx, tx, taskID, "followthrough_check", deliveryID, req.AgentID, req.RunID, req.RequestID, payload, by, now, &receipt, &out); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.notify(taskID)
	return out, nil
}

func validFollowThroughOutcome(outcome string) bool {
	return outcome == api.DeliveryFollowThroughOutcomeAccepted || outcome == api.DeliveryFollowThroughOutcomeFailed || outcome == api.DeliveryFollowThroughOutcomeAmbiguous || outcome == api.DeliveryFollowThroughOutcomeInvalidated
}

func hasFollowThroughLease(q queryRower, ctx context.Context, deliveryID, requestID string, generation, epoch int64) (bool, error) {
	events, err := loadDeliveryEvents(q, ctx, deliveryID)
	if err != nil {
		return false, err
	}
	for _, event := range events {
		if event.Kind != "followthrough_queue_leased" || event.RequestID != requestID {
			continue
		}
		gotGeneration, generationOK := event.Data["generation"].(float64)
		gotEpoch, epochOK := event.Data["executionEpoch"].(float64)
		if generationOK && epochOK && int64(gotGeneration) == generation && int64(gotEpoch) == epoch {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) ReportDeliveryFollowThrough(ctx context.Context, taskID, deliveryID string, req api.DeliveryFollowThroughReportRequest, by api.Caller) (api.DeliveryFollowThroughReport, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out api.DeliveryFollowThroughReport
	if !api.ValidID(taskID, "tsk") || !validDeliveryID(deliveryID, "dly") || !validRequestID(req.RequestID) || !validRequestID(req.LeaseRequestID) || !api.ValidID(req.AgentID, "agt") || !validRunID(req.RunID) || req.ExpectedGeneration < 1 || req.ExpectedEpoch < 1 || !validFollowThroughOutcome(req.Outcome) || !api.ValidText(req.Text, api.MaxTextLen) {
		return out, api.ErrInvalid
	}
	payload := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if replay, replayErr := findFollowThroughReplay(tx, ctx, taskID, "followthrough_report", deliveryID, req.AgentID, req.RunID, req.RequestID, payload, by, &out); replayErr != nil {
		return out, replayErr
	} else if replay {
		out.Replay = true
		return out, nil
	}
	d, err := getRequiredDeliveryRow(tx, ctx, taskID, deliveryID)
	if err != nil {
		return out, err
	}
	var pauseState string
	if err = tx.QueryRowContext(ctx, `SELECT pause_state FROM tasks WHERE id=?`, taskID).Scan(&pauseState); err != nil {
		return out, err
	}
	if pauseState != api.ProjectPauseActive && req.Outcome != api.DeliveryFollowThroughOutcomeInvalidated {
		return out, workItemConflict("project is paused; delivery follow-through is blocked")
	}
	if err = validateDeliveryBinding(tx, ctx, d); err != nil {
		return out, err
	}
	if d.AgentID != req.AgentID || d.RunID != req.RunID || d.Generation != req.ExpectedGeneration {
		return out, workItemConflict("stale or already-reported follow-through lease")
	}
	var currentRun, status string
	if err = tx.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE id=? AND task_id=?`, req.AgentID, taskID).Scan(&currentRun, &status); err != nil {
		return out, err
	}
	activeLease := d.Current && d.ExecutionEpoch == req.ExpectedEpoch && d.FollowThrough.PendingAction == api.DeliveryFollowThroughActionQueue && d.FollowThrough.PendingRequestID == req.LeaseRequestID && d.FollowThrough.TransportOutcome == ""
	if req.Outcome == api.DeliveryFollowThroughOutcomeInvalidated {
		recorded, leaseErr := hasFollowThroughLease(tx, ctx, deliveryID, req.LeaseRequestID, req.ExpectedGeneration, req.ExpectedEpoch)
		if leaseErr != nil {
			return out, leaseErr
		}
		item, itemErr := getWorkItem(tx, ctx, d.ItemTaskID, d.ItemID)
		if itemErr != nil {
			return out, itemErr
		}
		invalidated := !activeLease || currentRun != req.RunID || status == api.AgentRetired || status == api.AgentClosed || status == api.AgentExited || !d.Current || d.ExecutionEpoch != req.ExpectedEpoch || item.Revision != d.ItemRevision || item.Status == "done" || item.Status == "dismissed"
		if !recorded || !invalidated {
			return out, workItemConflict("follow-through lease is not durably invalidated")
		}
		now := s.now()
		if activeLease {
			if _, err = tx.ExecContext(ctx, `UPDATE required_deliveries SET followthrough_pending_action='',followthrough_pending_request_id='',followthrough_pending_since='',followthrough_transport_outcome=?,followthrough_confirmation_until='',updated_at=? WHERE id=?`, req.Outcome, ts(now), d.ID); err != nil {
				return out, err
			}
		}
		event, eventErr := insertDeliveryEvent(ctx, tx, d, "followthrough_queue_invalidated", req.RequestID, req.AgentID, req.RunID, map[string]any{"leaseRequestId": req.LeaseRequestID, "generation": req.ExpectedGeneration, "executionEpoch": req.ExpectedEpoch, "text": req.Text}, by, now)
		if eventErr != nil {
			return out, eventErr
		}
		d, err = getRequiredDeliveryRow(tx, ctx, taskID, deliveryID)
		if err != nil {
			return out, err
		}
		out.Delivery, out.Event = d, event
		if err = insertFollowThroughReceipt(ctx, tx, taskID, "followthrough_report", deliveryID, req.AgentID, req.RunID, req.RequestID, payload, by, now, &out.Receipt, &out); err != nil {
			return out, err
		}
		if err = tx.Commit(); err != nil {
			return out, err
		}
		s.notify(taskID)
		return out, nil
	}
	if !activeLease {
		return out, workItemConflict("stale or already-reported follow-through lease")
	}
	if currentRun != req.RunID || status == api.AgentRetired || status == api.AgentClosed || status == api.AgentExited {
		return out, workItemConflict("agent lifecycle no longer permits a follow-through report")
	}
	now := s.now()
	next := now
	confirmation := ""
	if req.Outcome == api.DeliveryFollowThroughOutcomeAccepted {
		next = now.Add(time.Duration(d.FollowThrough.Policy.ConfirmationDeadlineSeconds) * time.Second)
		confirmation = ts(next)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE required_deliveries SET followthrough_transport_outcome=?,followthrough_confirmation_until=?,next_substantive_deadline_at=?,updated_at=? WHERE id=?`, req.Outcome, confirmation, ts(next), ts(now), d.ID); err != nil {
		return out, err
	}
	event, err := insertDeliveryEvent(ctx, tx, d, "followthrough_queue_"+req.Outcome, req.RequestID, req.AgentID, req.RunID, map[string]any{"leaseRequestId": req.LeaseRequestID, "attempt": d.FollowThrough.AttemptCount, "outcome": req.Outcome, "text": req.Text}, by, now)
	if err != nil {
		return out, err
	}
	d, err = getRequiredDeliveryRow(tx, ctx, taskID, deliveryID)
	if err != nil {
		return out, err
	}
	out.Delivery, out.Event = d, event
	if err = insertFollowThroughReceipt(ctx, tx, taskID, "followthrough_report", deliveryID, req.AgentID, req.RunID, req.RequestID, payload, by, now, &out.Receipt, &out); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	s.notify(taskID)
	return out, nil
}
