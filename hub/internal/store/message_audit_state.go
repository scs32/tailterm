package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"

	"github.com/scs32/tailterm/hub/internal/api"
)

const messageAuditEventCols = `id,seq,message_task_id,message_seq,audit_revision,operation,provenance,before_state,after_state,reason,sources,agent_id,run_id,by_node,by_user,created_at`

type messageAuditCursor struct {
	Task  string `json:"task"`
	Kind  string `json:"kind,omitempty"`
	High  int64  `json:"high"`
	After int64  `json:"after"`
}

type messageAuditHistoryCursor struct {
	Task  string `json:"task"`
	Seq   int64  `json:"seq"`
	High  int64  `json:"high"`
	After int64  `json:"after"`
}

func encodeMessageAuditCursor(c messageAuditCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeMessageAuditCursor(raw string) (messageAuditCursor, error) {
	var c messageAuditCursor
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(b, &c) != nil || !api.ValidID(c.Task, "tsk") || c.High < 0 || c.After < 0 || c.After > c.High {
		return messageAuditCursor{}, api.ErrInvalid
	}
	return c, nil
}

func encodeMessageAuditHistoryCursor(c messageAuditHistoryCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeMessageAuditHistoryCursor(raw string) (messageAuditHistoryCursor, error) {
	var c messageAuditHistoryCursor
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || json.Unmarshal(b, &c) != nil || !api.ValidID(c.Task, "tsk") || c.Seq < 1 || c.High < 0 || c.After < 0 || c.After > c.High {
		return messageAuditHistoryCursor{}, api.ErrInvalid
	}
	return c, nil
}

func auditClassificationForPost(req api.PostMessageRequest) string {
	if req.AuditKind != "" {
		return req.AuditKind
	}
	if len(req.WorkItems) > 0 {
		return api.MessageAuditWork
	}
	return api.MessageAuditUnclassified
}

func validAuditClassification(kind string, allowUnclassified bool) bool {
	return kind == api.MessageAuditIntake || kind == api.MessageAuditWork || (allowUnclassified && kind == api.MessageAuditUnclassified)
}

func validMessageAuditCaller(by api.Caller) bool {
	return api.ValidText(by.Node, 512) && api.ValidText(by.User, 512)
}

func validateAuditLinkShape(classification string, links []api.MessageWorkItem) error {
	if classification == api.MessageAuditIntake {
		if len(links) != 0 {
			return api.ErrInvalid
		}
		return nil
	}
	if classification != api.MessageAuditWork || len(links) < 1 || len(links) > api.MaxMessageAuditRelated+1 {
		return api.ErrInvalid
	}
	seen := make(map[string]bool, len(links))
	primary := 0
	for _, link := range links {
		if !api.ValidID(link.ItemTaskID, "tsk") || !api.ValidID(link.ItemID, "wi") || link.ItemRevision < 1 || (link.Relationship != "primary" && link.Relationship != "related") {
			return api.ErrInvalid
		}
		key := link.ItemTaskID + "\x00" + link.ItemID
		if seen[key] {
			return api.ErrInvalid
		}
		seen[key] = true
		if link.Relationship == "primary" {
			primary++
		}
	}
	if primary != 1 {
		return api.ErrInvalid
	}
	return nil
}

func orderAuditLinks(links []api.MessageWorkItem) []api.MessageWorkItem {
	ordered := make([]api.MessageWorkItem, 0, len(links))
	related := make([]api.MessageWorkItem, 0, len(links))
	for _, link := range links {
		if link.Relationship == "primary" {
			ordered = append(ordered, link)
		} else {
			related = append(related, link)
		}
	}
	sort.Slice(related, func(i, j int) bool {
		if related[i].ItemTaskID == related[j].ItemTaskID {
			if related[i].ItemID == related[j].ItemID {
				return related[i].ItemRevision < related[j].ItemRevision
			}
			return related[i].ItemID < related[j].ItemID
		}
		return related[i].ItemTaskID < related[j].ItemTaskID
	})
	ordered = append(ordered, related...)
	return ordered
}

func validateAuditSources(q queryRower, ctx context.Context, sources []api.MessageReference, required bool) error {
	if (required && len(sources) == 0) || len(sources) > api.MaxMessageAuditSources {
		return api.ErrInvalid
	}
	seen := map[string]bool{}
	for _, source := range sources {
		if !api.ValidID(source.TaskID, "tsk") || source.Seq < 1 {
			return api.ErrInvalid
		}
		key := source.TaskID + "\x00" + strconv.FormatInt(source.Seq, 10)
		if seen[key] {
			return api.ErrInvalid
		}
		seen[key] = true
		var found int64
		if err := q.QueryRowContext(ctx, `SELECT seq FROM messages WHERE task_id=? AND seq=?`, source.TaskID, source.Seq).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return api.ErrInvalid
			}
			return err
		}
	}
	return nil
}

func validateMessageAuditActor(q queryRower, ctx context.Context, taskID, agentID, runID string) error {
	if agentID == "" {
		if runID != "" {
			return api.ErrInvalid
		}
		return nil
	}
	if err := validateWorkItemAgent(q, ctx, taskID, agentID); err != nil {
		return err
	}
	if runID == "" || !validRunID(runID) {
		return api.ErrInvalid
	}
	var currentRun, status string
	if err := q.QueryRowContext(ctx, `SELECT run_id,status FROM agents WHERE task_id=? AND id=?`, taskID, agentID).Scan(&currentRun, &status); err != nil {
		return err
	}
	if currentRun != runID {
		return workItemConflict("agent run changed; refresh identity before changing message audit state")
	}
	if !activeMessageAuditAgentStatus(status) {
		return workItemConflict("agent run is not active for message audit mutation")
	}
	return nil
}

func activeMessageAuditAgentStatus(status string) bool {
	switch status {
	case api.AgentStarting, api.AgentRunning, api.AgentDone, api.AgentNeedsInput:
		return true
	}
	return false
}

func hasForeignAuditAuthority(q queryRower, ctx context.Context, messageTaskID string, link api.MessageWorkItem) (bool, error) {
	if link.ItemTaskID == messageTaskID {
		return true, nil
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM work_item_dispatches d
JOIN work_items i ON i.id=d.item_id
WHERE i.task_id=? AND i.id=? AND d.item_revision=? AND d.target_task_id=?`, link.ItemTaskID, link.ItemID, link.ItemRevision, messageTaskID).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM message_audit_foreign_associations
WHERE message_task_id=? AND item_task_id=? AND item_id=? AND item_revision=?`, messageTaskID, link.ItemTaskID, link.ItemID, link.ItemRevision).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func validateAuditLinks(q queryRower, ctx context.Context, messageTaskID, classification string, links []api.MessageWorkItem, canEstablishForeign bool) ([]api.MessageWorkItem, error) {
	if err := validateAuditLinkShape(classification, links); err != nil {
		return nil, err
	}
	links = orderAuditLinks(links)
	var establish []api.MessageWorkItem
	for _, link := range links {
		item, err := getWorkItem(q, ctx, link.ItemTaskID, link.ItemID)
		if err != nil {
			if errors.Is(err, api.ErrNotFound) {
				return nil, api.ErrInvalid
			}
			return nil, err
		}
		if item.Revision != link.ItemRevision {
			var n int
			if err := q.QueryRowContext(ctx, `SELECT count(*) FROM work_item_revisions WHERE item_task_id=? AND item_id=? AND revision=?`, link.ItemTaskID, link.ItemID, link.ItemRevision).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				return nil, workItemConflict("work item revision is not retained; refresh exact history before linking")
			}
		}
		authorized, err := hasForeignAuditAuthority(q, ctx, messageTaskID, link)
		if err != nil {
			return nil, err
		}
		if !authorized {
			if !canEstablishForeign {
				return nil, api.ErrInvalid
			}
			establish = append(establish, link)
		}
	}
	return establish, nil
}

func loadAuditProjection(q queryRower, ctx context.Context, taskID string, seq int64) (*api.MessageAuditProjection, error) {
	var projection api.MessageAuditProjection
	var updated string
	err := q.QueryRowContext(ctx, `SELECT revision,classification,current_event_seq,updated_at
FROM message_audit_states WHERE message_task_id=? AND message_seq=?`, taskID, seq).Scan(
		&projection.Revision, &projection.Classification, &projection.EventSeq, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	projection.UpdatedAt = parseTS(updated)
	rows, err := q.QueryContext(ctx, `SELECT item_task_id,item_id,item_revision,relationship
FROM message_audit_links WHERE message_task_id=? AND message_seq=? ORDER BY ordinal`, taskID, seq)
	if err != nil {
		return nil, err
	}
	projection.WorkItems = []api.MessageWorkItem{}
	for rows.Next() {
		var link api.MessageWorkItem
		if err := rows.Scan(&link.ItemTaskID, &link.ItemID, &link.ItemRevision, &link.Relationship); err != nil {
			rows.Close()
			return nil, err
		}
		projection.WorkItems = append(projection.WorkItems, link)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return &projection, nil
}

func loadAuditOriginal(q queryRower, ctx context.Context, taskID string, seq int64) (api.MessageAuditOriginal, error) {
	original := api.MessageAuditOriginal{Classification: api.MessageAuditUnclassified, WorkItems: []api.MessageWorkItem{}}
	var linksJSON string
	var orderTask sql.NullString
	var orderSeq sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT classification,work_items,work_order_task_id,work_order_message_seq
FROM message_audit_originals WHERE message_task_id=? AND message_seq=?`, taskID, seq).Scan(
		&original.Classification, &linksJSON, &orderTask, &orderSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return original, nil
	}
	if err != nil {
		return original, err
	}
	if err := json.Unmarshal([]byte(linksJSON), &original.WorkItems); err != nil {
		return original, err
	}
	if orderTask.Valid {
		original.WorkOrderMessage = &api.MessageReference{TaskID: orderTask.String, Seq: orderSeq.Int64}
	}
	return original, nil
}

func loadMessageAuditRecord(q queryRower, ctx context.Context, taskID string, seq int64) (api.MessageAuditRecord, error) {
	var found int64
	if err := q.QueryRowContext(ctx, `SELECT seq FROM messages WHERE task_id=? AND seq=?`, taskID, seq).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return api.MessageAuditRecord{}, api.ErrNotFound
		}
		return api.MessageAuditRecord{}, err
	}
	original, err := loadAuditOriginal(q, ctx, taskID, seq)
	if err != nil {
		return api.MessageAuditRecord{}, err
	}
	current, err := loadAuditProjection(q, ctx, taskID, seq)
	if err != nil {
		return api.MessageAuditRecord{}, err
	}
	return api.MessageAuditRecord{Message: api.MessageReference{TaskID: taskID, Seq: seq}, Original: original, Current: current}, nil
}

func scanMessageAuditEvent(row rowScanner) (api.MessageAuditEvent, error) {
	var event api.MessageAuditEvent
	var beforeJSON, afterJSON, sourcesJSON, created string
	err := row.Scan(&event.ID, &event.Cursor, &event.Message.TaskID, &event.Message.Seq, &event.Revision,
		&event.Operation, &event.Provenance, &beforeJSON, &afterJSON, &event.Reason, &sourcesJSON,
		&event.Actor.AgentID, &event.Actor.RunID, &event.Actor.Caller.Node, &event.Actor.Caller.User, &created)
	if err != nil {
		return event, err
	}
	if beforeJSON != "" {
		var before api.MessageAuditProjection
		if err := json.Unmarshal([]byte(beforeJSON), &before); err != nil {
			return event, err
		}
		event.Before = &before
	}
	if err := json.Unmarshal([]byte(afterJSON), &event.After); err != nil {
		return event, err
	}
	if err := json.Unmarshal([]byte(sourcesJSON), &event.Sources); err != nil {
		return event, err
	}
	event.CreatedAt = parseTS(created)
	return event, nil
}

func insertAuditProjection(ctx context.Context, tx *sql.Tx, taskID string, seq int64, revision int64, classification, operation, provenance, reason string, sources []api.MessageReference, actor api.MessageAuditActor, before *api.MessageAuditProjection, links []api.MessageWorkItem, at string) (api.MessageAuditEvent, *api.MessageAuditProjection, error) {
	projection := api.MessageAuditProjection{Revision: revision, Classification: classification, WorkItems: orderAuditLinks(links), UpdatedAt: parseTS(at)}
	beforeJSON := ""
	if before != nil {
		b, _ := json.Marshal(before)
		beforeJSON = string(b)
	}
	afterJSON, _ := json.Marshal(projection)
	sourcesJSON, _ := json.Marshal(sources)
	event := api.MessageAuditEvent{ID: api.NewID("mae"), Message: api.MessageReference{TaskID: taskID, Seq: seq}, Revision: revision,
		Operation: operation, Provenance: provenance, Before: before, After: projection, Reason: reason, Sources: append([]api.MessageReference(nil), sources...), Actor: actor, CreatedAt: projection.UpdatedAt}
	result, err := tx.ExecContext(ctx, `INSERT INTO message_audit_events
(id,message_task_id,message_seq,audit_revision,operation,provenance,before_state,after_state,reason,sources,agent_id,run_id,by_node,by_user,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event.ID, taskID, seq, revision, operation, provenance, beforeJSON, string(afterJSON), reason, string(sourcesJSON),
		actor.AgentID, actor.RunID, actor.Caller.Node, actor.Caller.User, at)
	if err != nil {
		return event, nil, err
	}
	event.Cursor, err = result.LastInsertId()
	if err != nil {
		return event, nil, err
	}
	projection.EventSeq, event.After.EventSeq = event.Cursor, event.Cursor
	afterJSON, _ = json.Marshal(projection)
	if _, err := tx.ExecContext(ctx, `UPDATE message_audit_events SET after_state=? WHERE seq=?`, string(afterJSON), event.Cursor); err != nil {
		return event, nil, err
	}
	if before == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO message_audit_states(message_task_id,message_seq,revision,classification,current_event_seq,updated_at)
VALUES(?,?,?,?,?,?)`, taskID, seq, revision, classification, event.Cursor, at)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE message_audit_states SET revision=?,classification=?,current_event_seq=?,updated_at=?
WHERE message_task_id=? AND message_seq=? AND revision=?`, revision, classification, event.Cursor, at, taskID, seq, before.Revision)
		if err == nil {
			var changed int64
			changed, err = result.RowsAffected()
			if err == nil && changed != 1 {
				err = workItemConflict("message audit revision changed; refresh before correcting")
			}
		}
	}
	if err != nil {
		return event, nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_audit_links WHERE message_task_id=? AND message_seq=?`, taskID, seq); err != nil {
		return event, nil, err
	}
	for ordinal, link := range projection.WorkItems {
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_audit_links
(message_task_id,message_seq,ordinal,item_task_id,item_id,item_revision,relationship) VALUES(?,?,?,?,?,?,?)`,
			taskID, seq, ordinal, link.ItemTaskID, link.ItemID, link.ItemRevision, link.Relationship); err != nil {
			return event, nil, err
		}
	}
	return event, &projection, nil
}

func insertInitialMessageAudit(ctx context.Context, tx *sql.Tx, message *api.Message, req api.PostMessageRequest, dispatch bool) error {
	classification := auditClassificationForPost(req)
	if classification == api.MessageAuditUnclassified {
		return nil
	}
	if !validMessageAuditCaller(api.Caller{Node: message.From.Node, User: message.From.User}) {
		return api.ErrInvalid
	}
	links := orderAuditLinks(req.WorkItems)
	linksJSON, _ := json.Marshal(links)
	var orderTask, orderSeq any
	if req.WorkOrderMessage != nil {
		orderTask, orderSeq = req.WorkOrderMessage.TaskID, req.WorkOrderMessage.Seq
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_audit_originals
(message_task_id,message_seq,classification,work_items,work_order_task_id,work_order_message_seq,created_at)
VALUES(?,?,?,?,?,?,?)`, message.TaskID, message.Seq, classification, string(linksJSON), orderTask, orderSeq, ts(message.CreatedAt)); err != nil {
		return err
	}
	provenance := "native"
	if dispatch {
		provenance = "validated_dispatch"
	}
	actor := api.MessageAuditActor{AgentID: message.From.AgentID, Caller: api.Caller{Node: message.From.Node, User: message.From.User}}
	_, _, err := insertAuditProjection(ctx, tx, message.TaskID, message.Seq, 1, classification, "post", provenance,
		"Creation-time audit context.", nil, actor, nil, links, ts(message.CreatedAt))
	return err
}

func auditStatesEqual(a *api.MessageAuditProjection, desired api.MessageAuditDesiredState) bool {
	return a != nil && a.Classification == desired.Classification && reflect.DeepEqual(a.WorkItems, orderAuditLinks(desired.WorkItems))
}

func findMessageAuditReceipt(q queryRower, ctx context.Context, taskID, operation, requestID, agentID string, by api.Caller) (api.MessageAuditMutationResult, string, error) {
	var result api.MessageAuditMutationResult
	var payload, resultJSON string
	err := q.QueryRowContext(ctx, `SELECT payload_hash,result FROM message_audit_receipts
WHERE task_id=? AND operation=? AND agent_id=? AND by_node=? AND by_user=? AND request_id=?`,
		taskID, operation, agentID, by.Node, by.User, requestID).Scan(&payload, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return result, payload, api.ErrNotFound
	}
	if err != nil {
		return result, payload, err
	}
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return result, payload, err
	}
	return result, payload, nil
}

func insertMessageAuditReceipt(ctx context.Context, tx *sql.Tx, taskID, operation, requestID, payload, agentID string, by api.Caller, result *api.MessageAuditMutationResult) error {
	result.Receipt = api.MessageAuditReceipt{ID: api.NewID("mar"), TaskID: taskID, Operation: operation, RequestID: requestID,
		Message: result.Event.Message, AuditRevision: result.Event.Revision, EventSeq: result.Event.Cursor, CreatedAt: result.Event.CreatedAt}
	resultJSON, _ := json.Marshal(result)
	_, err := tx.ExecContext(ctx, `INSERT INTO message_audit_receipts
(id,task_id,operation,agent_id,by_node,by_user,request_id,payload_hash,message_seq,audit_revision,event_seq,result,created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, result.Receipt.ID, taskID, operation, agentID, by.Node, by.User, requestID, payload,
		result.Event.Message.Seq, result.Event.Revision, result.Event.Cursor, string(resultJSON), ts(result.Event.CreatedAt))
	return err
}

func (s *Store) GetMessageAudit(ctx context.Context, taskID string, seq int64) (api.MessageAuditRecord, error) {
	if !api.ValidID(taskID, "tsk") || seq < 1 {
		return api.MessageAuditRecord{}, api.ErrInvalid
	}
	return loadMessageAuditRecord(s.db, ctx, taskID, seq)
}

func (s *Store) ListMessageAuditHistory(ctx context.Context, taskID string, seq, afterVersion int64, cursor string, limit int) (api.MessageAuditHistory, error) {
	if !api.ValidID(taskID, "tsk") || seq < 1 || afterVersion < 0 || limit < 1 || limit > api.MaxMessageAuditPage {
		return api.MessageAuditHistory{}, api.ErrInvalid
	}
	if _, err := loadMessageAuditRecord(s.db, ctx, taskID, seq); err != nil {
		return api.MessageAuditHistory{}, err
	}
	var currentHigh int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(max(audit_revision),0) FROM message_audit_events
WHERE message_task_id=? AND message_seq=?`, taskID, seq).Scan(&currentHigh); err != nil {
		return api.MessageAuditHistory{}, err
	}
	c := messageAuditHistoryCursor{Task: taskID, Seq: seq, High: currentHigh, After: afterVersion}
	if cursor != "" {
		var err error
		c, err = decodeMessageAuditHistoryCursor(cursor)
		if err != nil || c.Task != taskID || c.Seq != seq || c.High > currentHigh || afterVersion != 0 {
			return api.MessageAuditHistory{}, api.ErrInvalid
		}
	} else if afterVersion > currentHigh {
		return api.MessageAuditHistory{}, api.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageAuditEventCols+` FROM message_audit_events
WHERE message_task_id=? AND message_seq=? AND audit_revision>? AND audit_revision<=? ORDER BY audit_revision LIMIT ?`, taskID, seq, c.After, c.High, limit+1)
	if err != nil {
		return api.MessageAuditHistory{}, err
	}
	out := api.MessageAuditHistory{Message: api.MessageReference{TaskID: taskID, Seq: seq}, Events: []api.MessageAuditEvent{}, Cutoff: c.High}
	for rows.Next() {
		event, err := scanMessageAuditEvent(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Events = append(out.Events, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	candidates := out.Events
	out.Events = []api.MessageAuditEvent{}
	more := len(candidates) > limit
	if more {
		candidates = candidates[:limit]
	}
	for _, event := range candidates {
		candidate := out
		candidate.Events = append(append([]api.MessageAuditEvent(nil), out.Events...), event)
		probe := c
		probe.After = event.Revision
		candidate.NextCursor = encodeMessageAuditHistoryCursor(probe)
		encoded, _ := json.Marshal(candidate)
		if len(encoded)+1 > api.MaxMessageAuditResponseBytes {
			if len(out.Events) == 0 {
				return api.MessageAuditHistory{}, workItemConflict("stored message audit event exceeds the history response bound")
			}
			more = true
			break
		}
		out.Events = append(out.Events, event)
	}
	if more && len(out.Events) > 0 {
		c.After = out.Events[len(out.Events)-1].Revision
		out.NextCursor = encodeMessageAuditHistoryCursor(c)
	}
	return out, nil
}

func (s *Store) GetMessageAuditReceipt(ctx context.Context, taskID, requestID, operation, agentID string, by api.Caller) (api.MessageAuditMutationResult, error) {
	if !api.ValidID(taskID, "tsk") || !validRequestID(requestID) || (operation != "correct" && operation != "resolve") || (agentID != "" && !api.ValidID(agentID, "agt")) {
		return api.MessageAuditMutationResult{}, api.ErrInvalid
	}
	result, _, err := findMessageAuditReceipt(s.db, ctx, taskID, operation, requestID, agentID, by)
	if err == nil {
		result.Replay = true
	}
	return result, err
}

func (s *Store) ListMessageAuditChanges(ctx context.Context, taskID, cursor string, limit int, kind string) (api.MessageAuditChangePage, error) {
	if !api.ValidID(taskID, "tsk") || limit < 1 || limit > api.MaxMessageAuditPage || (kind != "" && kind != api.MessageAuditIntake && kind != api.MessageAuditWork) {
		return api.MessageAuditChangePage{}, api.ErrInvalid
	}
	if _, err := s.GetTask(ctx, taskID); err != nil {
		return api.MessageAuditChangePage{}, err
	}
	var c messageAuditCursor
	var err error
	if cursor == "" {
		c = messageAuditCursor{Task: taskID, Kind: kind}
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(max(seq),0) FROM message_audit_events WHERE message_task_id=?`, taskID).Scan(&c.High); err != nil {
			return api.MessageAuditChangePage{}, err
		}
	} else {
		c, err = decodeMessageAuditCursor(cursor)
		var currentHigh int64
		if err == nil {
			err = s.db.QueryRowContext(ctx, `SELECT COALESCE(max(seq),0) FROM message_audit_events WHERE message_task_id=?`, taskID).Scan(&currentHigh)
		}
		if err != nil || c.Task != taskID || c.Kind != kind || c.High > currentHigh {
			return api.MessageAuditChangePage{}, api.ErrInvalid
		}
	}
	query := `SELECT ` + messageAuditEventCols + ` FROM message_audit_events WHERE message_task_id=? AND seq>? AND seq<=?`
	args := []any{taskID, c.After, c.High}
	if kind != "" {
		query += ` AND json_extract(after_state,'$.classification')=?`
		args = append(args, kind)
	}
	query += ` ORDER BY seq LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return api.MessageAuditChangePage{}, err
	}
	var candidates []api.MessageAuditEvent
	for rows.Next() {
		event, err := scanMessageAuditEvent(rows)
		if err != nil {
			rows.Close()
			return api.MessageAuditChangePage{}, err
		}
		candidates = append(candidates, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return api.MessageAuditChangePage{}, err
	}
	rows.Close()
	out := api.MessageAuditChangePage{Events: []api.MessageAuditEvent{}, Cutoff: c.High}
	more := len(candidates) > limit
	if more {
		candidates = candidates[:limit]
	}
	for _, event := range candidates {
		candidate := out
		candidate.Events = append(append([]api.MessageAuditEvent(nil), out.Events...), event)
		probe := c
		probe.After = event.Cursor
		candidate.NextCursor = encodeMessageAuditCursor(probe)
		encoded, _ := json.Marshal(candidate)
		if len(encoded)+1 > api.MaxMessageAuditResponseBytes {
			if len(out.Events) == 0 {
				return api.MessageAuditChangePage{}, workItemConflict("stored message audit event exceeds the change-feed response bound")
			}
			more = true
			break
		}
		out.Events = append(out.Events, event)
	}
	if more && len(out.Events) > 0 {
		c.After = out.Events[len(out.Events)-1].Cursor
		out.NextCursor = encodeMessageAuditCursor(c)
	}
	return out, nil
}
