package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

func normalizeAuditSources(sources []api.MessageReference) []api.MessageReference {
	out := append([]api.MessageReference(nil), sources...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].TaskID == out[j].TaskID {
			return out[i].Seq < out[j].Seq
		}
		return out[i].TaskID < out[j].TaskID
	})
	return out
}

func basicAuditMutationScope(taskID string, seq int64, requestID, agentID, runID string, expected int64) error {
	if !api.ValidID(taskID, "tsk") || seq < 1 || !validRequestID(requestID) || expected < 0 ||
		(agentID != "" && !api.ValidID(agentID, "agt")) || (runID != "" && !validRunID(runID)) {
		return api.ErrInvalid
	}
	return nil
}

func loadOpenAuditMessage(tx *sql.Tx, ctx context.Context, taskID string, seq int64) (api.Task, *api.MessageAuditProjection, error) {
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return task, nil, api.ErrNotFound
	}
	if err != nil {
		return task, nil, err
	}
	if task.Status != api.TaskOpen {
		return task, nil, api.ErrClosed
	}
	var found int64
	if err := tx.QueryRowContext(ctx, `SELECT seq FROM messages WHERE task_id=? AND seq=?`, taskID, seq).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return task, nil, api.ErrNotFound
		}
		return task, nil, err
	}
	current, err := loadAuditProjection(tx, ctx, taskID, seq)
	return task, current, err
}

func validateExpectedAuditRevision(current *api.MessageAuditProjection, expected int64) error {
	actual := int64(0)
	if current != nil {
		actual = current.Revision
	}
	if actual != expected {
		return workItemConflict("message audit revision changed; refresh before correcting")
	}
	return nil
}

// createResolvedWorkItemAt mirrors the native item-create transaction while
// allowing intake resolution to commit item creation and audit resolution in
// one SQLite transaction.
func (s *Store) createResolvedWorkItemAt(ctx context.Context, tx *sql.Tx, task api.Task, req api.CreateWorkItemRequest, by api.Caller, nowText string) (api.WorkItem, error) {
	if req.Priority == "" {
		req.Priority = "normal"
	}
	if !validWorkItemKind(req.Kind) || !validWorkItemTitle(req.Title) || !api.ValidText(req.Description, api.MaxTextLen) ||
		!validWorkItemPriority(req.Priority) || !validRequestID(req.RequestID) || req.SourceMessageSeq < 1 {
		return api.WorkItem{}, api.ErrInvalid
	}
	payload := requestHash(req)
	var priorHash, priorItem string
	err := tx.QueryRowContext(ctx, `SELECT payload_hash,item_id FROM work_item_requests
WHERE task_id=? AND operation='create' AND request_id=?`, task.ID, req.RequestID).Scan(&priorHash, &priorItem)
	if err == nil {
		if priorHash != payload {
			return api.WorkItem{}, workItemConflict("new-item request ID was already used with different create data")
		}
		return getWorkItem(tx, ctx, task.ID, priorItem)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return api.WorkItem{}, err
	}
	if err = validateWorkItemAgent(tx, ctx, task.ID, req.AgentID); err != nil {
		return api.WorkItem{}, err
	}
	if err = validateSourceMessage(tx, ctx, task.ID, req.SourceMessageSeq); err != nil {
		return api.WorkItem{}, err
	}
	now := parseTS(nowText)
	item := api.WorkItem{ID: api.NewID("wi"), TaskID: task.ID, Kind: req.Kind, Title: req.Title, Description: req.Description,
		Status: "open", Priority: req.Priority, Revision: 1, ScopeRevision: 1, SourceMessageSeq: req.SourceMessageSeq,
		CreatedBy: api.Sender{AgentID: req.AgentID, Node: by.Node, User: by.User}, UpdatedBy: api.Sender{AgentID: req.AgentID, Node: by.Node, User: by.User}, CreatedAt: now, UpdatedAt: now}
	result, err := tx.ExecContext(ctx, `INSERT INTO work_items
(id,task_id,kind,title,description,status,priority,revision,source_message_seq,created_agent,created_node,created_user,updated_agent,updated_node,updated_user,created_at,updated_at,narrative_scope_revision)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, item.ID, item.TaskID, item.Kind, item.Title, item.Description, item.Status, item.Priority,
		item.Revision, item.SourceMessageSeq, item.CreatedBy.AgentID, item.CreatedBy.Node, item.CreatedBy.User,
		item.UpdatedBy.AgentID, item.UpdatedBy.Node, item.UpdatedBy.User, nowText, nowText, item.ScopeRevision)
	if err != nil {
		return api.WorkItem{}, err
	}
	item.Seq, _ = result.LastInsertId()
	fields, _ := json.Marshal(map[string]any{"kind": item.Kind, "title": item.Title, "description": item.Description, "status": item.Status, "priority": item.Priority, "sourceMessageSeq": item.SourceMessageSeq})
	changeResult, err := tx.ExecContext(ctx, `INSERT INTO work_item_changes
(item_id,revision,kind,fields,agent_id,by_node,by_user,created_at) VALUES(?,1,'created',?,?,?,?,?)`, item.ID, string(fields), req.AgentID, by.Node, by.User, nowText)
	if err != nil {
		return api.WorkItem{}, err
	}
	changeSeq, err := changeResult.LastInsertId()
	if err != nil {
		return api.WorkItem{}, err
	}
	creationFields := []string{"description", "kind", "priority", "sourceMessageSeq", "status", "title"}
	if err = insertWorkItemRevision(ctx, tx, revisionFromItem(item, "", "created", "native", creationFields), changeSeq); err != nil {
		return api.WorkItem{}, err
	}
	if err = refreshWorkItemHistoryState(ctx, tx, item, nowText); err != nil {
		return api.WorkItem{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO work_item_requests
(task_id,operation,request_id,payload_hash,item_id,created_at) VALUES(?,'create',?,?,?,?)`, task.ID, req.RequestID, payload, item.ID, nowText); err != nil {
		return api.WorkItem{}, err
	}
	if _, err = s.insertEvent(ctx, tx, task.ID, "work_item_created", req.AgentID, item.Title,
		map[string]any{"itemId": item.ID, "kind": item.Kind, "revision": item.Revision}, by); err != nil {
		return api.WorkItem{}, err
	}
	return item, nil
}

func (s *Store) CorrectMessageAudit(ctx context.Context, taskID string, seq int64, req api.CorrectMessageAuditRequest, by api.Caller) (api.MessageAuditMutationResult, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := basicAuditMutationScope(taskID, seq, req.RequestID, req.AgentID, req.RunID, req.ExpectedRevision); err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	req.Desired.WorkItems = orderAuditLinks(req.Desired.WorkItems)
	req.Sources = normalizeAuditSources(req.Sources)
	payload := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	defer tx.Rollback()
	prior, priorHash, err := findMessageAuditReceipt(tx, ctx, taskID, "correct", req.RequestID, req.AgentID, by)
	if err == nil {
		if priorHash != payload {
			return api.MessageAuditMutationResult{}, false, workItemConflict("request ID was already used with different message-audit correction data")
		}
		prior.Replay = true
		return prior, true, nil
	}
	if !errors.Is(err, api.ErrNotFound) {
		return api.MessageAuditMutationResult{}, false, err
	}
	if !validMessageAuditCaller(by) {
		return api.MessageAuditMutationResult{}, false, api.ErrInvalid
	}
	if strings.TrimSpace(req.Reason) == "" || !api.ValidText(req.Reason, 2048) || !validAuditClassification(req.Desired.Classification, false) || validateAuditLinkShape(req.Desired.Classification, req.Desired.WorkItems) != nil {
		return api.MessageAuditMutationResult{}, false, api.ErrInvalid
	}
	_, current, err := loadOpenAuditMessage(tx, ctx, taskID, seq)
	if err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	if err = validateExpectedAuditRevision(current, req.ExpectedRevision); err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	if auditStatesEqual(current, req.Desired) {
		return api.MessageAuditMutationResult{}, false, api.ErrInvalid
	}
	if err = validateAuditSources(tx, ctx, req.Sources, true); err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	err = validateMessageAuditActor(tx, ctx, taskID, req.AgentID, req.RunID)
	if err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	_, err = validateAuditLinks(tx, ctx, taskID, req.Desired.Classification, req.Desired.WorkItems, false)
	if err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	revision := int64(1)
	if current != nil {
		revision = current.Revision + 1
	}
	now := s.now()
	actor := api.MessageAuditActor{AgentID: req.AgentID, RunID: req.RunID, Caller: by}
	event, _, err := insertAuditProjection(ctx, tx, taskID, seq, revision, req.Desired.Classification, "correct", "evidence_backed_correction",
		req.Reason, req.Sources, actor, current, req.Desired.WorkItems, ts(now))
	if err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	state, err := loadMessageAuditRecord(tx, ctx, taskID, seq)
	if err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	result := api.MessageAuditMutationResult{State: state, Event: event}
	if err = insertMessageAuditReceipt(ctx, tx, taskID, "correct", req.RequestID, payload, req.AgentID, by, &result); err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	s.notify(taskID)
	return result, false, nil
}

func (s *Store) ResolveMessageAudit(ctx context.Context, taskID string, seq int64, req api.ResolveMessageAuditRequest, by api.Caller) (api.MessageAuditMutationResult, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := basicAuditMutationScope(taskID, seq, req.RequestID, req.AgentID, req.RunID, req.ExpectedRevision); err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	if req.NewItem != nil && req.NewItem.Priority == "" {
		req.NewItem.Priority = "normal"
	}
	req.Sources = normalizeAuditSources(req.Sources)
	payload := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	defer tx.Rollback()
	prior, priorHash, err := findMessageAuditReceipt(tx, ctx, taskID, "resolve", req.RequestID, req.AgentID, by)
	if err == nil {
		if priorHash != payload {
			return api.MessageAuditMutationResult{}, false, workItemConflict("request ID was already used with different intake-resolution data")
		}
		prior.Replay = true
		return prior, true, nil
	}
	if !errors.Is(err, api.ErrNotFound) {
		return api.MessageAuditMutationResult{}, false, err
	}
	if !validMessageAuditCaller(by) {
		return api.MessageAuditMutationResult{}, false, api.ErrInvalid
	}
	if (req.ExistingItem == nil) == (req.NewItem == nil) || strings.TrimSpace(req.Reason) == "" || !api.ValidText(req.Reason, 2048) {
		return api.MessageAuditMutationResult{}, false, api.ErrInvalid
	}
	if req.NewItem != nil && req.NewItem.ExpectedRevision != 0 {
		return api.MessageAuditMutationResult{}, false, api.ErrInvalid
	}
	task, current, err := loadOpenAuditMessage(tx, ctx, taskID, seq)
	if err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	if current == nil || current.Classification != api.MessageAuditIntake {
		return api.MessageAuditMutationResult{}, false, workItemConflict("message is not current intake")
	}
	if err = validateExpectedAuditRevision(current, req.ExpectedRevision); err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	if err = validateAuditSources(tx, ctx, req.Sources, true); err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	err = validateMessageAuditActor(tx, ctx, taskID, req.AgentID, req.RunID)
	if err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	var item api.WorkItem
	provenance := "intake_resolution_existing"
	if req.ExistingItem != nil {
		link := *req.ExistingItem
		if link.Relationship != "primary" {
			return api.MessageAuditMutationResult{}, false, api.ErrInvalid
		}
		item, err = getWorkItem(tx, ctx, link.ItemTaskID, link.ItemID)
		if err != nil {
			return api.MessageAuditMutationResult{}, false, err
		}
		if item.Revision != link.ItemRevision {
			return api.MessageAuditMutationResult{}, false, workItemConflict("work item revision changed; refresh before resolving intake")
		}
	} else {
		provenance = "intake_resolution_new_item"
		create := api.CreateWorkItemRequest{Kind: req.NewItem.Kind, Title: req.NewItem.Title, Description: req.NewItem.Description,
			Priority: req.NewItem.Priority, AgentID: req.AgentID, SourceMessageSeq: seq, RequestID: req.NewItem.RequestID}
		item, err = s.createResolvedWorkItemAt(ctx, tx, task, create, by, ts(s.now()))
		if err != nil {
			return api.MessageAuditMutationResult{}, false, err
		}
	}
	links := []api.MessageWorkItem{{ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
	_, err = validateAuditLinks(tx, ctx, taskID, api.MessageAuditWork, links, false)
	if err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	now := s.now()
	actor := api.MessageAuditActor{AgentID: req.AgentID, RunID: req.RunID, Caller: by}
	event, _, err := insertAuditProjection(ctx, tx, taskID, seq, current.Revision+1, api.MessageAuditWork, "resolve", provenance,
		req.Reason, req.Sources, actor, current, links, ts(now))
	if err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	state, err := loadMessageAuditRecord(tx, ctx, taskID, seq)
	if err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	result := api.MessageAuditMutationResult{State: state, Event: event, Item: &item}
	if err = insertMessageAuditReceipt(ctx, tx, taskID, "resolve", req.RequestID, payload, req.AgentID, by, &result); err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return api.MessageAuditMutationResult{}, false, err
	}
	s.notify(taskID)
	return result, false, nil
}
