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
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
)

const workItemCols = `seq,id,task_id,kind,title,description,status,priority,revision,source_message_seq,created_agent,created_node,created_user,updated_agent,updated_node,updated_user,created_at,updated_at,narrative_scope_revision,completion_report_id,completion_report_version,completion_report_digest,completion_scope_revision`

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validWorkItemKind(kind string) bool { return kind == "bug" || kind == "feature" }

func validWorkItemStatus(status string) bool {
	switch status {
	case "open", "in_progress", "blocked", "done", "dismissed":
		return true
	}
	return false
}

func validWorkItemPriority(priority string) bool {
	switch priority {
	case "low", "normal", "high", "urgent":
		return true
	}
	return false
}

func validWorkItemTitle(title string) bool {
	return api.ValidTaskName(title)
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == ':') {
			return false
		}
	}
	return true
}

func requestHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func workItemConflict(message string) error {
	return fmt.Errorf("%w: %s", api.ErrConflict, message)
}

func scanWorkItem(row interface{ Scan(...any) error }) (api.WorkItem, error) {
	var item api.WorkItem
	var created, updated string
	var completion api.NarrativeReportPin
	err := row.Scan(&item.Seq, &item.ID, &item.TaskID, &item.Kind, &item.Title, &item.Description, &item.Status, &item.Priority, &item.Revision, &item.SourceMessageSeq,
		&item.CreatedBy.AgentID, &item.CreatedBy.Node, &item.CreatedBy.User, &item.UpdatedBy.AgentID, &item.UpdatedBy.Node, &item.UpdatedBy.User, &created, &updated,
		&item.ScopeRevision, &completion.ReportID, &completion.Version, &completion.Digest, &completion.ScopeRevision)
	item.CreatedAt, item.UpdatedAt = parseTS(created), parseTS(updated)
	if completion.ReportID != "" {
		item.CompletionReport = &completion
	}
	return item, err
}

func scanWorkItemDispatch(row interface{ Scan(...any) error }) (api.WorkItemDispatch, error) {
	var dispatch api.WorkItemDispatch
	var created string
	err := row.Scan(&dispatch.ID, &dispatch.ItemID, &dispatch.Revision, &dispatch.TargetTaskID, &dispatch.TargetAgentID, &dispatch.MessageSeq, &created)
	dispatch.CreatedAt = parseTS(created)
	return dispatch, err
}

func getWorkItem(q queryRower, ctx context.Context, taskID, itemID string) (api.WorkItem, error) {
	item, err := scanWorkItem(q.QueryRowContext(ctx, `SELECT `+workItemCols+` FROM work_items WHERE task_id=? AND id=?`, taskID, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return item, api.ErrNotFound
	}
	return item, err
}

func loadLastDispatch(q queryRower, ctx context.Context, item *api.WorkItem) error {
	dispatch, err := scanWorkItemDispatch(q.QueryRowContext(ctx, `SELECT id,item_id,item_revision,target_task_id,target_agent_id,message_seq,created_at FROM work_item_dispatches WHERE item_id=? ORDER BY seq DESC LIMIT 1`, item.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	item.LastDispatch = &dispatch
	return nil
}

func validateWorkItemAgent(q queryRower, ctx context.Context, taskID, agentID string) error {
	if agentID == "" {
		return nil
	}
	if !api.ValidID(agentID, "agt") {
		return api.ErrInvalid
	}
	var found string
	if err := q.QueryRowContext(ctx, `SELECT id FROM agents WHERE id=? AND task_id=?`, agentID, taskID).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return api.ErrInvalid
		}
		return err
	}
	return nil
}

func validateSourceMessage(q queryRower, ctx context.Context, taskID string, seq int64) error {
	if seq == 0 {
		return nil
	}
	if seq < 0 {
		return api.ErrInvalid
	}
	var sender string
	if err := q.QueryRowContext(ctx, `SELECT from_agent FROM messages WHERE task_id=? AND seq=?`, taskID, seq).Scan(&sender); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return api.ErrInvalid
		}
		return err
	}
	return validateWorkItemAgent(q, ctx, taskID, sender)
}

func (s *Store) GetWorkItem(ctx context.Context, taskID, itemID string) (api.WorkItem, error) {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") {
		return api.WorkItem{}, api.ErrInvalid
	}
	item, err := getWorkItem(s.db, ctx, taskID, itemID)
	if err != nil {
		return item, err
	}
	err = loadLastDispatch(s.db, ctx, &item)
	return item, err
}

func (s *Store) ListWorkItems(ctx context.Context, taskID, kind, status string, after int64, limit int) (api.WorkItemList, error) {
	if after < 0 || (taskID != "" && !api.ValidID(taskID, "tsk")) || (kind != "" && !validWorkItemKind(kind)) || (status != "" && !validWorkItemStatus(status)) {
		return api.WorkItemList{}, api.ErrInvalid
	}
	if taskID != "" {
		if _, err := s.GetTask(ctx, taskID); err != nil {
			return api.WorkItemList{}, err
		}
	}
	if limit <= 0 || limit > api.MaxLimit {
		limit = api.MaxLimit
	}
	query := `SELECT ` + workItemCols + ` FROM work_items WHERE seq>?`
	args := []any{after}
	if taskID != "" {
		query += ` AND task_id=?`
		args = append(args, taskID)
	}
	if kind != "" {
		query += ` AND kind=?`
		args = append(args, kind)
	}
	if status != "" {
		query += ` AND status=?`
		args = append(args, status)
	}
	query += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return api.WorkItemList{}, err
	}
	items := []api.WorkItem{}
	for rows.Next() {
		item, err := scanWorkItem(rows)
		if err != nil {
			rows.Close()
			return api.WorkItemList{}, err
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return api.WorkItemList{}, err
	}
	rows.Close()
	for i := range items {
		if err = loadLastDispatch(s.db, ctx, &items[i]); err != nil {
			return api.WorkItemList{}, err
		}
	}
	list := api.WorkItemList{Items: items}
	if len(items) > 0 {
		list.Next = items[len(items)-1].Seq
	}
	return list, nil
}

func (s *Store) CreateWorkItem(ctx context.Context, taskID string, req api.CreateWorkItemRequest, by api.Caller) (api.WorkItem, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if req.Priority == "" {
		req.Priority = "normal"
	}
	if !api.ValidID(taskID, "tsk") || !validWorkItemKind(req.Kind) || !validWorkItemTitle(req.Title) || !api.ValidText(req.Description, api.MaxTextLen) || !validWorkItemPriority(req.Priority) || !validRequestID(req.RequestID) || req.SourceMessageSeq < 0 {
		return api.WorkItem{}, api.ErrInvalid
	}
	payload := requestHash(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.WorkItem{}, err
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return api.WorkItem{}, api.ErrNotFound
	}
	if err != nil {
		return api.WorkItem{}, err
	}
	var priorHash, priorItem string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash,item_id FROM work_item_requests WHERE task_id=? AND operation='create' AND request_id=?`, taskID, req.RequestID).Scan(&priorHash, &priorItem)
	if err == nil {
		if priorHash != payload {
			return api.WorkItem{}, workItemConflict("request ID was already used with different create data")
		}
		item, err := getWorkItem(tx, ctx, taskID, priorItem)
		if err == nil {
			err = loadLastDispatch(tx, ctx, &item)
		}
		return item, err
	}
	if err != sql.ErrNoRows {
		return api.WorkItem{}, err
	}
	if task.Status != api.TaskOpen {
		return api.WorkItem{}, api.ErrClosed
	}
	if err = validateWorkItemAgent(tx, ctx, taskID, req.AgentID); err != nil {
		return api.WorkItem{}, err
	}
	if err = validateSourceMessage(tx, ctx, taskID, req.SourceMessageSeq); err != nil {
		return api.WorkItem{}, err
	}
	now := s.now()
	item := api.WorkItem{ID: api.NewID("wi"), TaskID: taskID, Kind: req.Kind, Title: req.Title, Description: req.Description, Status: "open", Priority: req.Priority, Revision: 1, ScopeRevision: 1, SourceMessageSeq: req.SourceMessageSeq,
		CreatedBy: api.Sender{AgentID: req.AgentID, Node: by.Node, User: by.User}, UpdatedBy: api.Sender{AgentID: req.AgentID, Node: by.Node, User: by.User}, CreatedAt: now, UpdatedAt: now}
	result, err := tx.ExecContext(ctx, `INSERT INTO work_items(id,task_id,kind,title,description,status,priority,revision,source_message_seq,created_agent,created_node,created_user,updated_agent,updated_node,updated_user,created_at,updated_at,narrative_scope_revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		item.ID, item.TaskID, item.Kind, item.Title, item.Description, item.Status, item.Priority, item.Revision, item.SourceMessageSeq, item.CreatedBy.AgentID, item.CreatedBy.Node, item.CreatedBy.User, item.UpdatedBy.AgentID, item.UpdatedBy.Node, item.UpdatedBy.User, ts(now), ts(now), item.ScopeRevision)
	if err != nil {
		return api.WorkItem{}, err
	}
	item.Seq, _ = result.LastInsertId()
	fields, _ := json.Marshal(map[string]any{"kind": item.Kind, "title": item.Title, "description": item.Description, "status": item.Status, "priority": item.Priority, "sourceMessageSeq": item.SourceMessageSeq})
	changeResult, err := tx.ExecContext(ctx, `INSERT INTO work_item_changes(item_id,revision,kind,fields,agent_id,by_node,by_user,created_at) VALUES(?,1,'created',?,?,?,?,?)`, item.ID, string(fields), req.AgentID, by.Node, by.User, ts(now))
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
	if err = refreshWorkItemHistoryState(ctx, tx, item, ts(now)); err != nil {
		return api.WorkItem{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO work_item_requests(task_id,operation,request_id,payload_hash,item_id,created_at) VALUES(?,'create',?,?,?,?)`, taskID, req.RequestID, payload, item.ID, ts(now)); err != nil {
		return api.WorkItem{}, err
	}
	if _, err = s.insertEvent(ctx, tx, taskID, "work_item_created", req.AgentID, item.Title, map[string]any{"itemId": item.ID, "kind": item.Kind, "revision": item.Revision}, by); err != nil {
		return api.WorkItem{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.WorkItem{}, err
	}
	s.notify(taskID)
	return item, nil
}

func (s *Store) UpdateWorkItem(ctx context.Context, taskID, itemID string, req api.UpdateWorkItemRequest, by api.Caller) (api.WorkItem, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || req.Revision < 1 || (req.Title == nil && req.Description == nil && req.Status == nil && req.Priority == nil) {
		return api.WorkItem{}, api.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.WorkItem{}, err
	}
	defer tx.Rollback()
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return api.WorkItem{}, api.ErrNotFound
	}
	if err != nil {
		return api.WorkItem{}, err
	}
	if task.Status != api.TaskOpen {
		return api.WorkItem{}, api.ErrClosed
	}
	item, err := getWorkItem(tx, ctx, taskID, itemID)
	if err != nil {
		return api.WorkItem{}, err
	}
	if item.Revision != req.Revision {
		return api.WorkItem{}, workItemConflict("work item revision changed; refresh it before updating")
	}
	transitioningDone := item.Kind == "feature" && item.Status != "done" && req.Status != nil && *req.Status == "done"
	if transitioningDone {
		if req.Title != nil || req.Description != nil {
			return api.WorkItem{}, api.ErrNarrativeReportStale
		}
		if err = validateNarrativeCompletion(ctx, tx, item, req.CompletionReport); err != nil {
			return api.WorkItem{}, err
		}
	}
	if err = validateWorkItemAgent(tx, ctx, taskID, req.AgentID); err != nil {
		return api.WorkItem{}, err
	}
	changed := map[string]any{}
	changedFields := []string{}
	if req.Title != nil {
		if !validWorkItemTitle(*req.Title) {
			return api.WorkItem{}, api.ErrInvalid
		}
		item.Title = *req.Title
		changed["title"] = item.Title
		changedFields = append(changedFields, "title")
	}
	if req.Description != nil {
		if !api.ValidText(*req.Description, api.MaxTextLen) {
			return api.WorkItem{}, api.ErrInvalid
		}
		item.Description = *req.Description
		changed["description"] = item.Description
		changedFields = append(changedFields, "description")
	}
	if req.Title != nil || req.Description != nil {
		item.ScopeRevision++
	}
	if req.Status != nil {
		if !validWorkItemStatus(*req.Status) {
			return api.WorkItem{}, api.ErrInvalid
		}
		item.Status = *req.Status
		changed["status"] = item.Status
		changedFields = append(changedFields, "status")
	}
	if req.Priority != nil {
		if !validWorkItemPriority(*req.Priority) {
			return api.WorkItem{}, api.ErrInvalid
		}
		item.Priority = *req.Priority
		changed["priority"] = item.Priority
		changedFields = append(changedFields, "priority")
	}
	now := s.now()
	item.Revision++
	item.UpdatedAt = now
	item.UpdatedBy = api.Sender{AgentID: req.AgentID, Node: by.Node, User: by.User}
	if transitioningDone {
		item.CompletionReport = req.CompletionReport
	}
	completion := api.NarrativeReportPin{}
	if item.CompletionReport != nil {
		completion = *item.CompletionReport
	}
	result, err := tx.ExecContext(ctx, `UPDATE work_items SET title=?,description=?,status=?,priority=?,revision=?,updated_agent=?,updated_node=?,updated_user=?,updated_at=?,narrative_scope_revision=?,completion_report_id=?,completion_report_version=?,completion_report_digest=?,completion_scope_revision=? WHERE task_id=? AND id=? AND revision=?`,
		item.Title, item.Description, item.Status, item.Priority, item.Revision, item.UpdatedBy.AgentID, item.UpdatedBy.Node, item.UpdatedBy.User, ts(now), item.ScopeRevision, completion.ReportID, completion.Version, completion.Digest, completion.ScopeRevision, taskID, itemID, req.Revision)
	if err != nil {
		return api.WorkItem{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return api.WorkItem{}, err
	}
	if rows != 1 {
		return api.WorkItem{}, workItemConflict("work item revision changed; refresh it before updating")
	}
	fields, _ := json.Marshal(changed)
	changeResult, err := tx.ExecContext(ctx, `INSERT INTO work_item_changes(item_id,revision,kind,fields,agent_id,by_node,by_user,created_at) VALUES(?,?,'updated',?,?,?,?,?)`, item.ID, item.Revision, string(fields), req.AgentID, by.Node, by.User, ts(now))
	if err != nil {
		return api.WorkItem{}, err
	}
	changeSeq, err := changeResult.LastInsertId()
	if err != nil {
		return api.WorkItem{}, err
	}
	if err = insertWorkItemRevision(ctx, tx, revisionFromItem(item, "", "updated", "native", changedFields), changeSeq); err != nil {
		return api.WorkItem{}, err
	}
	if transitioningDone {
		if err = insertNarrativeCompletion(ctx, tx, item, completion, req.AgentID, "", by, now); err != nil {
			return api.WorkItem{}, err
		}
	}
	if err = refreshWorkItemHistoryState(ctx, tx, item, ts(now)); err != nil {
		return api.WorkItem{}, err
	}
	if _, err = s.insertEvent(ctx, tx, taskID, "work_item_updated", req.AgentID, item.Title, map[string]any{"itemId": item.ID, "kind": item.Kind, "revision": item.Revision, "fields": changedFields}, by); err != nil {
		return api.WorkItem{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.WorkItem{}, err
	}
	s.notify(taskID)
	return item, nil
}

func validateCreateWorkItemUpdate(taskID, itemID string, req api.CreateWorkItemUpdate) error {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || req.ExpectedRevision < 1 || !validRequestID(req.RequestID) ||
		(req.Title == nil && req.Description == nil && req.Status == nil && req.Priority == nil) ||
		(req.AgentID != "" && !api.ValidID(req.AgentID, "agt")) || (req.RunID != "" && (req.AgentID == "" || !validRunID(req.RunID))) {
		return api.ErrInvalid
	}
	if req.Title != nil && !validWorkItemTitle(*req.Title) {
		return api.ErrInvalid
	}
	if req.Description != nil && !api.ValidText(*req.Description, api.MaxTextLen) {
		return api.ErrInvalid
	}
	if req.Status != nil && !validWorkItemStatus(*req.Status) {
		return api.ErrInvalid
	}
	if req.Priority != nil && !validWorkItemPriority(*req.Priority) {
		return api.ErrInvalid
	}
	return nil
}

func scanWorkItemUpdateReceipt(row rowScanner) (api.WorkItemUpdateReceipt, string, error) {
	var receipt api.WorkItemUpdateReceipt
	var created, payload string
	err := row.Scan(&receipt.ID, &receipt.RequestID, &receipt.TaskID, &receipt.ItemID, &receipt.ResultRevision, &created, &payload)
	receipt.CreatedAt = parseTS(created)
	return receipt, payload, err
}

func findWorkItemUpdateReceipt(q queryRower, ctx context.Context, taskID, itemID, requestID, agentID string, by api.Caller) (api.WorkItemUpdateReceipt, string, error) {
	receipt, payload, err := scanWorkItemUpdateReceipt(q.QueryRowContext(ctx, `SELECT receipt_id,request_id,task_id,item_id,result_revision,created_at,payload_hash FROM work_item_update_requests WHERE task_id=? AND item_id=? AND agent_id=? AND by_node=? AND by_user=? AND request_id=?`, taskID, itemID, agentID, by.Node, by.User, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, payload, api.ErrNotFound
	}
	return receipt, payload, err
}

func findAnyWorkItemUpdateReceipt(q queryRower, ctx context.Context, taskID, requestID, agentID string, by api.Caller) (api.WorkItemUpdateReceipt, string, error) {
	receipt, payload, err := scanWorkItemUpdateReceipt(q.QueryRowContext(ctx, `SELECT receipt_id,request_id,task_id,item_id,result_revision,created_at,payload_hash FROM work_item_update_requests WHERE task_id=? AND agent_id=? AND by_node=? AND by_user=? AND request_id=?`, taskID, agentID, by.Node, by.User, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, payload, api.ErrNotFound
	}
	return receipt, payload, err
}

func loadWorkItemUpdateResult(q queryRower, ctx context.Context, receipt api.WorkItemUpdateReceipt) (api.WorkItemUpdateResult, error) {
	revision, err := scanWorkItemRevision(q.QueryRowContext(ctx, `SELECT `+workItemRevisionCols+` FROM work_item_revisions WHERE item_task_id=? AND item_id=? AND revision=?`, receipt.TaskID, receipt.ItemID, receipt.ResultRevision))
	return api.WorkItemUpdateResult{Revision: revision, Receipt: receipt}, err
}

// CreateWorkItemUpdate performs a recoverable CAS update. Receipt lookup occurs
// before mutable lifecycle checks so an exact retry remains valid after later
// edits, project closure, agent retirement, or hub restart.
func (s *Store) CreateWorkItemUpdate(ctx context.Context, taskID, itemID string, req api.CreateWorkItemUpdate, by api.Caller) (api.WorkItemUpdateResult, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := validateCreateWorkItemUpdate(taskID, itemID, req); err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	payload := requestHash(struct {
		ItemID  string                   `json:"itemId"`
		Request api.CreateWorkItemUpdate `json:"request"`
	}{itemID, req})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	defer tx.Rollback()
	receipt, priorHash, receiptErr := findAnyWorkItemUpdateReceipt(tx, ctx, taskID, req.RequestID, req.AgentID, by)
	if receiptErr == nil {
		if priorHash != payload {
			return api.WorkItemUpdateResult{}, false, workItemConflict("request ID was already used with different update data")
		}
		result, err := loadWorkItemUpdateResult(tx, ctx, receipt)
		return result, true, err
	}
	if !errors.Is(receiptErr, api.ErrNotFound) {
		return api.WorkItemUpdateResult{}, false, receiptErr
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return api.WorkItemUpdateResult{}, false, api.ErrNotFound
	}
	if err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	if task.Status != api.TaskOpen {
		return api.WorkItemUpdateResult{}, false, api.ErrClosed
	}
	item, err := getWorkItem(tx, ctx, taskID, itemID)
	if err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	if item.Revision != req.ExpectedRevision {
		return api.WorkItemUpdateResult{}, false, workItemConflict("work item revision changed; refresh it before updating")
	}
	transitioningDone := item.Kind == "feature" && item.Status != "done" && req.Status != nil && *req.Status == "done"
	if transitioningDone {
		if req.Title != nil || req.Description != nil {
			return api.WorkItemUpdateResult{}, false, api.ErrNarrativeReportStale
		}
		if err = validateNarrativeCompletion(ctx, tx, item, req.CompletionReport); err != nil {
			return api.WorkItemUpdateResult{}, false, err
		}
	}
	if err = validateWorkItemAgent(tx, ctx, taskID, req.AgentID); err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	if req.RunID != "" {
		var currentRun string
		if err := tx.QueryRowContext(ctx, `SELECT run_id FROM agents WHERE task_id=? AND id=?`, taskID, req.AgentID).Scan(&currentRun); err != nil {
			return api.WorkItemUpdateResult{}, false, err
		}
		if currentRun != req.RunID {
			return api.WorkItemUpdateResult{}, false, workItemConflict("agent run changed; refresh identity before updating")
		}
	}
	changed := map[string]any{}
	changedFields := []string{}
	if req.Title != nil {
		item.Title = *req.Title
		changed["title"] = item.Title
		changedFields = append(changedFields, "title")
	}
	if req.Description != nil {
		item.Description = *req.Description
		changed["description"] = item.Description
		changedFields = append(changedFields, "description")
	}
	if req.Status != nil {
		item.Status = *req.Status
		changed["status"] = item.Status
		changedFields = append(changedFields, "status")
	}
	if req.Priority != nil {
		item.Priority = *req.Priority
		changed["priority"] = item.Priority
		changedFields = append(changedFields, "priority")
	}
	if req.Title != nil || req.Description != nil {
		item.ScopeRevision++
	}
	now := s.now()
	item.Revision++
	item.UpdatedAt = now
	item.UpdatedBy = api.Sender{AgentID: req.AgentID, Node: by.Node, User: by.User}
	if transitioningDone {
		item.CompletionReport = req.CompletionReport
	}
	completion := api.NarrativeReportPin{}
	if item.CompletionReport != nil {
		completion = *item.CompletionReport
	}
	update, err := tx.ExecContext(ctx, `UPDATE work_items SET title=?,description=?,status=?,priority=?,revision=?,updated_agent=?,updated_node=?,updated_user=?,updated_at=?,narrative_scope_revision=?,completion_report_id=?,completion_report_version=?,completion_report_digest=?,completion_scope_revision=? WHERE task_id=? AND id=? AND revision=?`, item.Title, item.Description, item.Status, item.Priority, item.Revision, item.UpdatedBy.AgentID, item.UpdatedBy.Node, item.UpdatedBy.User, ts(now), item.ScopeRevision, completion.ReportID, completion.Version, completion.Digest, completion.ScopeRevision, taskID, itemID, req.ExpectedRevision)
	if err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	rows, err := update.RowsAffected()
	if err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	if rows != 1 {
		return api.WorkItemUpdateResult{}, false, workItemConflict("work item revision changed; refresh it before updating")
	}
	fields, _ := json.Marshal(changed)
	change, err := tx.ExecContext(ctx, `INSERT INTO work_item_changes(item_id,revision,kind,fields,agent_id,by_node,by_user,created_at) VALUES(?,?,'updated',?,?,?,?,?)`, item.ID, item.Revision, string(fields), req.AgentID, by.Node, by.User, ts(now))
	if err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	changeSeq, err := change.LastInsertId()
	if err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	revision := revisionFromItem(item, req.RunID, "updated", "native", changedFields)
	if err = insertWorkItemRevision(ctx, tx, revision, changeSeq); err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	if transitioningDone {
		if err = insertNarrativeCompletion(ctx, tx, item, completion, req.AgentID, req.RunID, by, now); err != nil {
			return api.WorkItemUpdateResult{}, false, err
		}
	}
	receipt = api.WorkItemUpdateReceipt{ID: api.NewID("wir"), RequestID: req.RequestID, TaskID: taskID, ItemID: itemID, ResultRevision: item.Revision, CreatedAt: now}
	if _, err = tx.ExecContext(ctx, `INSERT INTO work_item_update_requests(receipt_id,task_id,item_id,agent_id,run_id,by_node,by_user,request_id,payload_hash,result_revision,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, receipt.ID, taskID, itemID, req.AgentID, req.RunID, by.Node, by.User, req.RequestID, payload, item.Revision, ts(now)); err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	if err = refreshWorkItemHistoryState(ctx, tx, item, ts(now)); err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	if _, err = s.insertEvent(ctx, tx, taskID, "work_item_updated", req.AgentID, item.Title, map[string]any{"itemId": item.ID, "kind": item.Kind, "revision": item.Revision, "fields": changedFields, "requestId": req.RequestID}, by); err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return api.WorkItemUpdateResult{}, false, err
	}
	s.notify(taskID)
	return api.WorkItemUpdateResult{Revision: revision, Receipt: receipt}, false, nil
}

func (s *Store) GetWorkItemUpdateReceipt(ctx context.Context, taskID, itemID, requestID, agentID string, by api.Caller) (api.WorkItemUpdateResult, error) {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || !validRequestID(requestID) || (agentID != "" && !api.ValidID(agentID, "agt")) {
		return api.WorkItemUpdateResult{}, api.ErrInvalid
	}
	receipt, _, err := findWorkItemUpdateReceipt(s.db, ctx, taskID, itemID, requestID, agentID, by)
	if err != nil {
		return api.WorkItemUpdateResult{}, err
	}
	return loadWorkItemUpdateResult(s.db, ctx, receipt)
}

func (s *Store) DispatchWorkItem(ctx context.Context, taskID, itemID string, req api.DispatchWorkItemRequest, by api.Caller) (api.WorkItemDispatchResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if req.TargetTaskID == "" {
		req.TargetTaskID = taskID
	}
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || !api.ValidID(req.TargetTaskID, "tsk") || req.Revision < 1 || !validRequestID(req.RequestID) {
		return api.WorkItemDispatchResult{}, api.ErrInvalid
	}
	payload := requestHash(struct {
		ItemID string
		Req    api.DispatchWorkItemRequest
	}{itemID, req})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.WorkItemDispatchResult{}, err
	}
	defer tx.Rollback()
	item, err := getWorkItem(tx, ctx, taskID, itemID)
	if err != nil {
		return api.WorkItemDispatchResult{}, err
	}
	var priorHash, dispatchID string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash,dispatch_id FROM work_item_requests WHERE task_id=? AND operation='dispatch' AND request_id=?`, taskID, req.RequestID).Scan(&priorHash, &dispatchID)
	if err == nil {
		if priorHash != payload {
			return api.WorkItemDispatchResult{}, workItemConflict("request ID was already used with different dispatch data")
		}
		dispatch, err := scanWorkItemDispatch(tx.QueryRowContext(ctx, `SELECT id,item_id,item_revision,target_task_id,target_agent_id,message_seq,created_at FROM work_item_dispatches WHERE id=?`, dispatchID))
		if err != nil {
			return api.WorkItemDispatchResult{}, err
		}
		item.LastDispatch = &dispatch
		return api.WorkItemDispatchResult{Item: item, Dispatch: dispatch}, nil
	}
	if err != sql.ErrNoRows {
		return api.WorkItemDispatchResult{}, err
	}
	if item.Revision != req.Revision {
		return api.WorkItemDispatchResult{}, workItemConflict("work item revision changed; refresh it before dispatching")
	}
	sourceTask, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, taskID))
	if err != nil {
		return api.WorkItemDispatchResult{}, err
	}
	targetTask, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, req.TargetTaskID))
	if errors.Is(err, sql.ErrNoRows) {
		return api.WorkItemDispatchResult{}, api.ErrNotFound
	}
	if err != nil {
		return api.WorkItemDispatchResult{}, err
	}
	if sourceTask.Status != api.TaskOpen || targetTask.Status != api.TaskOpen {
		return api.WorkItemDispatchResult{}, api.ErrClosed
	}
	if err = validateWorkItemAgent(tx, ctx, taskID, req.AgentID); err != nil {
		return api.WorkItemDispatchResult{}, err
	}
	if req.AgentID != "" && req.TargetTaskID != taskID {
		return api.WorkItemDispatchResult{}, api.ErrInvalid
	}
	if targetTask.Orchestrator == "" {
		return api.WorkItemDispatchResult{}, workItemConflict("target project has no orchestrator")
	}
	targetAgent, err := scanAgent(tx.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE task_id=? AND name=? COLLATE NOCASE AND role='' AND status NOT IN ('closed','exited') ORDER BY created_at DESC LIMIT 1`, targetTask.ID, targetTask.Orchestrator))
	if errors.Is(err, sql.ErrNoRows) {
		var currentRole string
		roleErr := tx.QueryRowContext(ctx, `SELECT role FROM agents WHERE task_id=? AND name=? COLLATE NOCASE AND status NOT IN ('closed','exited') ORDER BY created_at DESC LIMIT 1`, targetTask.ID, targetTask.Orchestrator).Scan(&currentRole)
		if roleErr == nil {
			return api.WorkItemDispatchResult{}, workItemConflict("target project orchestrator name belongs to role " + currentRole)
		}
		if roleErr != sql.ErrNoRows {
			return api.WorkItemDispatchResult{}, roleErr
		}
		return api.WorkItemDispatchResult{}, workItemConflict("target project orchestrator has no open agent")
	}
	if err != nil {
		return api.WorkItemDispatchResult{}, err
	}
	description := item.Description
	if len(description) > 6000 {
		description = description[:6000]
		for !utf8.ValidString(description) {
			description = description[:len(description)-1]
		}
		description += "…"
	}
	messageText := fmt.Sprintf("Work item %s revision %d from project %q (%s)\n%s: %s\nStatus: %s · Priority: %s\nDescription: %s", item.ID, item.Revision, sourceTask.Name, sourceTask.ID, strings.ToUpper(item.Kind), item.Title, item.Status, item.Priority, description)
	message, err := s.insertMessage(ctx, tx, targetTask, api.PostMessageRequest{
		AgentID: req.AgentID,
		To:      targetAgent.ID,
		Text:    messageText,
		WorkItems: []api.MessageWorkItem{{
			ItemTaskID:   item.TaskID,
			ItemID:       item.ID,
			ItemRevision: item.Revision,
			Relationship: "primary",
		}},
	}, targetAgent, by, true, false)
	if err != nil {
		return api.WorkItemDispatchResult{}, err
	}
	now := s.now()
	dispatch := api.WorkItemDispatch{ID: api.NewID("wid"), ItemID: item.ID, Revision: item.Revision, TargetTaskID: targetTask.ID, TargetAgentID: targetAgent.ID, MessageSeq: message.Seq, CreatedAt: now}
	snapshot, _ := json.Marshal(item)
	if _, err = tx.ExecContext(ctx, `INSERT INTO work_item_dispatches(id,item_id,item_revision,snapshot,target_task_id,target_agent_id,message_seq,agent_id,by_node,by_user,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		dispatch.ID, dispatch.ItemID, dispatch.Revision, string(snapshot), dispatch.TargetTaskID, dispatch.TargetAgentID, dispatch.MessageSeq, req.AgentID, by.Node, by.User, ts(now)); err != nil {
		return api.WorkItemDispatchResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO work_item_requests(task_id,operation,request_id,payload_hash,item_id,dispatch_id,created_at) VALUES(?,'dispatch',?,?,?,?,?)`, taskID, req.RequestID, payload, item.ID, dispatch.ID, ts(now)); err != nil {
		return api.WorkItemDispatchResult{}, err
	}
	if _, err = s.insertEvent(ctx, tx, taskID, "work_item_dispatched", req.AgentID, item.Title, map[string]any{"itemId": item.ID, "revision": item.Revision, "dispatchId": dispatch.ID, "targetTaskId": targetTask.ID, "targetAgentId": targetAgent.ID, "messageSeq": message.Seq}, by); err != nil {
		return api.WorkItemDispatchResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.WorkItemDispatchResult{}, err
	}
	s.notify(taskID)
	if targetTask.ID != taskID {
		s.notify(targetTask.ID)
	}
	item.LastDispatch = &dispatch
	return api.WorkItemDispatchResult{Item: item, Dispatch: dispatch}, nil
}
