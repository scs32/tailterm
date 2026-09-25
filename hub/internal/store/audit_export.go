package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
)

const auditExportSchema = `
CREATE TABLE IF NOT EXISTS audit_exports (
  id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id),
  format_version INTEGER NOT NULL, request_hash TEXT NOT NULL,
  by_node TEXT NOT NULL, by_user TEXT NOT NULL,
  created_at TEXT NOT NULL, expires_at TEXT NOT NULL,
  byte_count INTEGER NOT NULL, sha256 TEXT NOT NULL, cutoffs TEXT NOT NULL,
  content BLOB
);
CREATE INDEX IF NOT EXISTS audit_exports_task_ready ON audit_exports(task_id,expires_at,id);
CREATE TABLE IF NOT EXISTS audit_export_receipts (
  id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id),
  by_node TEXT NOT NULL, by_user TEXT NOT NULL, request_id TEXT NOT NULL,
  request_hash TEXT NOT NULL, export_id TEXT NOT NULL REFERENCES audit_exports(id),
  created_at TEXT NOT NULL,
  UNIQUE(task_id,by_node,by_user,request_id)
);`

func migrateAuditExports(db *sql.DB) error {
	_, err := db.Exec(auditExportSchema)
	return err
}

type exportQuery struct {
	name, sql string
	args      func(string) []any
}

var exportQueries = []exportQuery{
	{"task", `SELECT id,name,goal,status,created_at,created_node,created_user,closed_at,allow_agent_spawn,max_new_agents,swarm,orchestrator FROM tasks WHERE id=?`, oneArg},
	{"agents", `SELECT id,task_id,name,host,session,runtime,parent_agent_id,status,title,created_at,last_event_at,role,cleanup_done,cleanup_error,blocked_reason,blocked_text,run_id,last_seen_at FROM agents WHERE task_id=? ORDER BY created_at,id`, oneArg},
	{"messages", `SELECT seq,task_id,from_agent,from_node,from_user,to_agent,text,created_at,reply_to,broadcast FROM messages WHERE task_id=? ORDER BY seq`, oneArg},
	{"events", `SELECT seq,task_id,kind,agent_id,text,data,by_node,by_user,created_at FROM events WHERE task_id=? ORDER BY seq`, oneArg},
	{"workItems", `SELECT * FROM work_items WHERE task_id=? ORDER BY seq`, oneArg},
	{"workItemChanges", `SELECT c.* FROM work_item_changes c JOIN work_items w ON w.id=c.item_id WHERE w.task_id=? ORDER BY c.seq`, oneArg},
	{"workItemDispatches", `SELECT d.* FROM work_item_dispatches d JOIN work_items w ON w.id=d.item_id WHERE w.task_id=? ORDER BY d.seq`, oneArg},
	{"workItemRequests", `SELECT * FROM work_item_requests WHERE task_id=? ORDER BY operation,request_id`, oneArg},
	{"messageWorkItemLinks", `SELECT * FROM message_work_item_links WHERE message_task_id=? ORDER BY message_seq`, oneArg},
	{"agentWorkItemBindings", `SELECT b.agent_id,b.run_id,b.item_task_id,b.item_id,b.item_revision,b.work_order_task_id,b.work_order_message_seq,b.context_through_message_seq,b.replaces_agent_id,b.team_role,b.context_digest,b.created_at FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id WHERE a.task_id=? ORDER BY b.created_at,b.agent_id`, oneArg},
	{"messagePostReceipts", `SELECT receipt_id,task_id,agent_id,by_node,by_user,request_id,payload_hash,message_seq,created_at FROM message_post_requests WHERE task_id=? ORDER BY created_at,receipt_id`, oneArg},
	{"decisionRequests", `SELECT * FROM decision_requests WHERE task_id=? ORDER BY message_seq`, oneArg},
	{"decisionAnswers", `SELECT * FROM decision_answers WHERE task_id=? ORDER BY message_seq`, oneArg},
	{"workItemRevisions", `SELECT * FROM work_item_revisions WHERE item_task_id=? ORDER BY item_seq,revision`, oneArg},
	{"workItemHistoryGaps", `SELECT * FROM work_item_history_gaps WHERE item_task_id=? ORDER BY seq`, oneArg},
	{"workItemHistoryState", `SELECT * FROM work_item_history_state WHERE item_task_id=? ORDER BY item_id`, oneArg},
	{"workItemUpdateReceipts", `SELECT * FROM work_item_update_requests WHERE task_id=? ORDER BY created_at,receipt_id`, oneArg},
	{"messageAuditOriginals", `SELECT * FROM message_audit_originals WHERE message_task_id=? ORDER BY message_seq`, oneArg},
	{"messageAuditStates", `SELECT * FROM message_audit_states WHERE message_task_id=? ORDER BY message_seq`, oneArg},
	{"messageAuditLinks", `SELECT * FROM message_audit_links WHERE message_task_id=? ORDER BY message_seq,ordinal`, oneArg},
	{"messageAuditEvents", `SELECT * FROM message_audit_events WHERE message_task_id=? ORDER BY seq`, oneArg},
	{"messageAuditReceipts", `SELECT * FROM message_audit_receipts WHERE task_id=? ORDER BY created_at,id`, oneArg},
	{"messageAuditForeignAssociations", `SELECT * FROM message_audit_foreign_associations WHERE message_task_id=? ORDER BY created_at,id`, oneArg},
	{"messageAuditAssociationReceipts", `SELECT * FROM message_audit_association_receipts WHERE task_id=? ORDER BY created_at,id`, oneArg},
	{"narrativeState", `SELECT * FROM narrative_state WHERE task_id=? ORDER BY item_id`, oneArg},
	{"narrativeArtifacts", `SELECT * FROM narrative_artifacts WHERE task_id=? ORDER BY item_id,id`, oneArg},
	{"narrativeArtifactVersions", `SELECT * FROM narrative_artifact_versions WHERE task_id=? ORDER BY item_id,narrative_seq`, oneArg},
	{"narrativeLinks", `SELECT * FROM narrative_links WHERE task_id=? ORDER BY item_id,id`, oneArg},
	{"narrativeLinkVersions", `SELECT * FROM narrative_link_versions WHERE task_id=? ORDER BY item_id,narrative_seq`, oneArg},
	{"narrativeCoverage", `SELECT * FROM narrative_coverage WHERE task_id=? ORDER BY item_id,id`, oneArg},
	{"narrativeCoverageVersions", `SELECT * FROM narrative_coverage_versions WHERE task_id=? ORDER BY item_id,narrative_seq`, oneArg},
	{"narrativeReports", `SELECT * FROM narrative_reports WHERE task_id=? ORDER BY item_id,id`, oneArg},
	{"narrativeReportVersions", `SELECT * FROM narrative_report_versions WHERE task_id=? ORDER BY item_id,narrative_seq`, oneArg},
	{"narrativeTimeline", `SELECT * FROM narrative_timeline WHERE task_id=? ORDER BY item_id,seq`, oneArg},
	{"narrativeReceipts", `SELECT * FROM narrative_receipts WHERE task_id=? ORDER BY item_id,created_at,id`, oneArg},
	{"narrativeCompletionPins", `SELECT * FROM narrative_completion_pins WHERE task_id=? ORDER BY item_id,item_revision`, oneArg},
}

var queueExportQueries = []exportQuery{
	{"queueEntries", `SELECT * FROM queue_entries WHERE target_task_id=? OR source_task_id=? ORDER BY seq`, twoArgs},
	{"queueCycles", `SELECT c.* FROM queue_cycles c JOIN queue_entries e ON e.id=c.entry_id WHERE e.target_task_id=? OR e.source_task_id=? ORDER BY e.seq,c.cycle`, twoArgs},
	{"queueEvents", `SELECT v.* FROM queue_events v JOIN queue_entries e ON e.id=v.entry_id WHERE e.target_task_id=? OR e.source_task_id=? ORDER BY v.seq`, twoArgs},
	{"queueDispatchLinks", `SELECT l.* FROM queue_dispatch_links l JOIN queue_entries e ON e.id=l.entry_id WHERE e.target_task_id=? OR e.source_task_id=? ORDER BY l.event_seq,l.dispatch_id`, twoArgs},
	{"queueReceipts", `SELECT r.* FROM queue_requests r JOIN queue_entries e ON e.id=r.entry_id WHERE e.target_task_id=? OR e.source_task_id=? ORDER BY r.created_at,r.receipt_id`, twoArgs},
	{"queueNotifications", `SELECT n.* FROM queue_notifications n JOIN queue_entries e ON e.id=n.entry_id WHERE e.target_task_id=? OR e.source_task_id=? ORDER BY n.event_seq,n.recipient_generation`, twoArgs},
}

// allocationIntentExportQueries is deliberately NOT part of the shared
// exportQueries list: the agent_allocation_intents table (and its
// expected-run/launcher/author columns) postdates legacy audit export
// format 2 (review #2893/#2916, gap flagged by review #3003/#3010). Format
// 2's schema must remain byte-for-byte unchanged for any existing consumer;
// the allocation-intent stream is included only in format 3, exactly like
// the Queue streams below.
var allocationIntentExportQueries = []exportQuery{
	{"agentAllocationIntents", `SELECT agent_id,target_task_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,team_role,context_digest,author_agent_id,author_run_id,expected_run_id,expected_launcher_agent_id,expected_launcher_run_id,request_id,created_by_node,created_by_user,created_at,consumed_at,consumed_by_run_id,launcher_agent_id,launcher_run_id,invalidated_at,invalidated_pause_generation FROM agent_allocation_intents WHERE target_task_id=? ORDER BY created_at,agent_id`, oneArg},
}

var projectPauseExportQueries = []exportQuery{
	{"projectPauseTaskState", `SELECT id,pause_state,lifecycle_generation,pause_generation,paused_at FROM tasks WHERE id=?`, oneArg},
	{"projectPauseCycles", `SELECT * FROM project_pause_cycles WHERE task_id=? ORDER BY pause_generation`, oneArg},
	{"projectPauseTargets", `SELECT pt.* FROM project_pause_targets pt JOIN project_pause_cycles pc ON pc.id=pt.cycle_id WHERE pc.task_id=? ORDER BY pc.pause_generation,pt.agent_id,pt.run_id`, oneArg},
	{"projectPauseReceipts", `SELECT * FROM project_pause_receipts WHERE task_id=? ORDER BY created_at,id`, oneArg},
}

// Phase-successor streams are format-3-only. Format 2 remains byte-for-byte
// compatible, while v2 operational acceptance exports its immutable record,
// obligation, accountable-lead transfer provenance, and exact delivery events.
var phaseSuccessorExportQueries = []exportQuery{
	{"operationalRecords", `SELECT * FROM operational_records WHERE task_id=? ORDER BY id`, oneArg},
	{"operationalRecordVersions", `SELECT v.* FROM operational_record_versions v JOIN operational_records r ON r.id=v.record_id WHERE r.task_id=? ORDER BY v.record_id,v.version`, oneArg},
	{"phaseSuccessorObligations", `SELECT * FROM phase_successor_obligations WHERE task_id=? ORDER BY created_at,id`, oneArg},
	{"phaseSuccessorLeadTransfers", `SELECT t.* FROM phase_successor_lead_transfers t JOIN phase_successor_obligations o ON o.id=t.obligation_id WHERE o.task_id=? ORDER BY t.obligation_id,t.sequence`, oneArg},
	{"phaseSuccessorDeliveries", `SELECT d.* FROM required_deliveries d WHERE d.id IN (SELECT lead_delivery_id FROM phase_successor_obligations WHERE task_id=? UNION SELECT successor_delivery_id FROM phase_successor_obligations WHERE task_id=? AND successor_delivery_id<>'' UNION SELECT t.from_delivery_id FROM phase_successor_lead_transfers t JOIN phase_successor_obligations o ON o.id=t.obligation_id WHERE o.task_id=? UNION SELECT t.to_delivery_id FROM phase_successor_lead_transfers t JOIN phase_successor_obligations o ON o.id=t.obligation_id WHERE o.task_id=?) ORDER BY d.created_at,d.id`, fourArgs},
	{"phaseSuccessorDeliveryEvents", `SELECT e.* FROM delivery_events e WHERE e.delivery_id IN (SELECT lead_delivery_id FROM phase_successor_obligations WHERE task_id=? UNION SELECT successor_delivery_id FROM phase_successor_obligations WHERE task_id=? AND successor_delivery_id<>'' UNION SELECT t.from_delivery_id FROM phase_successor_lead_transfers t JOIN phase_successor_obligations o ON o.id=t.obligation_id WHERE o.task_id=? UNION SELECT t.to_delivery_id FROM phase_successor_lead_transfers t JOIN phase_successor_obligations o ON o.id=t.obligation_id WHERE o.task_id=?) ORDER BY e.created_at,e.id`, fourArgs},
}

// Older databases can retain the side-branch successor tables. They were not
// part of the current schema, so export their history only when the pair exists.
// A partial pair is a broken schema, not an optional absence.
func hasLegacyPhaseSuccessorTables(ctx context.Context, tx *sql.Tx) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('phase_successor_obligations','phase_successor_lead_transfers')`).Scan(&count)
	if err != nil {
		return false, err
	}
	if count == 1 {
		return false, fmt.Errorf("incomplete legacy phase-successor schema")
	}
	return count == 2, nil
}

func oneArg(taskID string) []any   { return []any{taskID} }
func twoArgs(taskID string) []any  { return []any{taskID, taskID} }
func fourArgs(taskID string) []any { return []any{taskID, taskID, taskID, taskID} }

type auditExportWriter struct {
	ctx    context.Context
	tx     *sql.Tx
	id     string
	bytes  int64
	limit  int64
	hash   hash.Hash
	buffer []byte
}

func (w *auditExportWriter) flush() error {
	if len(w.buffer) == 0 {
		return nil
	}
	if _, err := w.tx.ExecContext(w.ctx, `UPDATE audit_exports SET content=CAST(content || ? AS BLOB) WHERE id=?`, w.buffer, w.id); err != nil {
		return err
	}
	w.buffer = w.buffer[:0]
	return nil
}

func (w *auditExportWriter) append(value []byte) error {
	if w.bytes+int64(len(value)) > w.limit {
		return api.ErrLimit
	}
	_, _ = w.hash.Write(value)
	w.bytes += int64(len(value))
	for len(value) > 0 {
		space := api.MaxAuditExportChunkBytes - len(w.buffer)
		if space > len(value) {
			space = len(value)
		}
		w.buffer = append(w.buffer, value[:space]...)
		value = value[space:]
		if len(w.buffer) == api.MaxAuditExportChunkBytes {
			if err := w.flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *auditExportWriter) appendJSON(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return w.append(encoded)
}

func (w *auditExportWriter) appendJSONString(value string) error {
	if err := w.append([]byte{'"'}); err != nil {
		return err
	}
	start := 0
	flush := func(end int) error {
		if end <= start {
			return nil
		}
		return w.append([]byte(value[start:end]))
	}
	for index := 0; index < len(value); {
		r, size := utf8.DecodeRuneInString(value[index:])
		escaped := ""
		switch r {
		case '\b':
			escaped = `\b`
		case '\f':
			escaped = `\f`
		case '\n':
			escaped = `\n`
		case '\r':
			escaped = `\r`
		case '\t':
			escaped = `\t`
		case '"':
			escaped = `\"`
		case '\\':
			escaped = `\\`
		case '<':
			escaped = `\u003c`
		case '>':
			escaped = `\u003e`
		case '&':
			escaped = `\u0026`
		case '\u2028':
			escaped = `\u2028`
		case '\u2029':
			escaped = `\u2029`
		case utf8.RuneError:
			if size == 1 {
				escaped = `\ufffd`
			}
		default:
			if r < 0x20 {
				escaped = fmt.Sprintf(`\u%04x`, r)
			}
		}
		if escaped != "" {
			if err := flush(index); err != nil {
				return err
			}
			if err := w.append([]byte(escaped)); err != nil {
				return err
			}
			start = index + size
		}
		index += size
		if index-start >= api.MaxAuditExportChunkBytes {
			if err := flush(index); err != nil {
				return err
			}
			start = index
		}
	}
	if err := flush(len(value)); err != nil {
		return err
	}
	return w.append([]byte{'"'})
}

func (w *auditExportWriter) appendRow(cols []string, values []any) error {
	if err := w.append([]byte("{")); err != nil {
		return err
	}
	indexes := make([]int, len(cols))
	for index := range indexes {
		indexes[index] = index
	}
	sort.Slice(indexes, func(i, j int) bool { return cols[indexes[i]] < cols[indexes[j]] })
	for position, index := range indexes {
		if position > 0 {
			if err := w.append([]byte(",")); err != nil {
				return err
			}
		}
		if err := w.appendJSONString(cols[index]); err != nil {
			return err
		}
		if err := w.append([]byte(":")); err != nil {
			return err
		}
		value := values[index]
		if raw, ok := value.([]byte); ok {
			value = string(raw)
		}
		if text, ok := value.(string); ok {
			if err := w.appendJSONString(text); err != nil {
				return err
			}
		} else if err := w.appendJSON(value); err != nil {
			return err
		}
	}
	return w.append([]byte("}"))
}

func writeExportRows(ctx context.Context, tx *sql.Tx, q exportQuery, taskID string, output *auditExportWriter) (int, error) {
	rows, err := tx.QueryContext(ctx, q.sql, q.args(taskID)...)
	if err != nil {
		return 0, fmt.Errorf("export %s: %w", q.name, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	if err := output.appendJSON(q.name); err != nil {
		return 0, err
	}
	if err := output.append([]byte(": [")); err != nil {
		return 0, err
	}
	count := 0
	for rows.Next() {
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return 0, err
		}
		if count > 0 {
			if err := output.append([]byte(",")); err != nil {
				return 0, err
			}
		}
		if err := output.appendRow(cols, values); err != nil {
			return 0, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := output.append([]byte("]")); err != nil {
		return 0, err
	}
	return count, nil
}

func requestDigest(req api.CreateAuditExportRequest) string {
	b, _ := json.Marshal(req)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func scanAuditExport(row interface{ Scan(...any) error }, requestID string, replay bool, now time.Time) (api.AuditExport, error) {
	var out api.AuditExport
	var created, expires, cutoffs string
	var available int
	var receiptID string
	err := row.Scan(&out.ID, &out.SourceProject, &out.FormatVersion, &out.RecordedBy.Node, &out.RecordedBy.User, &created, &expires, &out.ByteCount, &out.SHA256, &cutoffs, &available, &receiptID)
	if err != nil {
		return out, err
	}
	out.Schema = "tailterm-project-audit"
	out.CreatedAt = parseTS(created)
	out.ExpiresAt = parseTS(expires)
	out.Available = now.Before(out.ExpiresAt) && available != 0
	out.Replay = replay
	out.Receipt = api.AuditExportReceipt{ID: receiptID, RequestID: requestID, TaskID: out.SourceProject, ExportID: out.ID, CreatedAt: out.CreatedAt}
	if err := json.Unmarshal([]byte(cutoffs), &out.StreamCutoffs); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Store) expireAuditExports(ctx context.Context, tx *sql.Tx, taskID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE audit_exports SET content=NULL WHERE task_id=? AND content IS NOT NULL AND expires_at<=?`, taskID, ts(now))
	return err
}

func (s *Store) CreateAuditExport(ctx context.Context, taskID string, req api.CreateAuditExportRequest, by api.Caller) (api.AuditExport, error) {
	if !api.ValidID(taskID, "tsk") || strings.TrimSpace(req.RequestID) == "" || len(req.RequestID) > 128 || !api.ValidText(req.RequestID, 128) {
		return api.AuditExport{}, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := s.now()
	digest := requestDigest(req)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.AuditExport{}, err
	}
	defer tx.Rollback()
	if err = s.expireAuditExports(ctx, tx, taskID, now); err != nil {
		return api.AuditExport{}, err
	}
	row := tx.QueryRowContext(ctx, `SELECT e.id,e.task_id,e.format_version,e.by_node,e.by_user,e.created_at,e.expires_at,e.byte_count,e.sha256,e.cutoffs,e.content IS NOT NULL,r.id FROM audit_export_receipts r JOIN audit_exports e ON e.id=r.export_id WHERE r.task_id=? AND r.by_node=? AND r.by_user=? AND r.request_id=?`, taskID, by.Node, by.User, req.RequestID)
	existing, scanErr := scanAuditExport(row, req.RequestID, true, now)
	if scanErr == nil {
		var oldHash string
		if err := tx.QueryRowContext(ctx, `SELECT request_hash FROM audit_export_receipts WHERE id=?`, existing.Receipt.ID).Scan(&oldHash); err != nil {
			return api.AuditExport{}, err
		}
		if oldHash != digest {
			return api.AuditExport{}, api.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return api.AuditExport{}, err
		}
		return existing, nil
	}
	if !errors.Is(scanErr, sql.ErrNoRows) {
		return api.AuditExport{}, scanErr
	}
	if req.FormatVersion != api.AuditExportLegacyVersion && req.FormatVersion != api.AuditExportFormatVersion {
		return api.AuditExport{}, api.ErrInvalid
	}
	var taskExists int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE id=?`, taskID).Scan(&taskExists); err != nil {
		return api.AuditExport{}, err
	}
	if taskExists == 0 {
		return api.AuditExport{}, api.ErrNotFound
	}
	var readyCount int
	var readyBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(byte_count),0) FROM audit_exports WHERE task_id=? AND content IS NOT NULL AND expires_at>?`, taskID, ts(now)).Scan(&readyCount, &readyBytes); err != nil {
		return api.AuditExport{}, err
	}
	if readyCount >= api.MaxReadyAuditExports {
		return api.AuditExport{}, api.ErrLimit
	}
	cutoffs := map[string]any{}
	expires := now.Add(api.AuditExportRetention)
	exportID := api.NewID("aex")
	receiptID := api.NewID("aer")
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_exports(id,task_id,format_version,request_hash,by_node,by_user,created_at,expires_at,byte_count,sha256,cutoffs,content) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, exportID, taskID, req.FormatVersion, digest, by.Node, by.User, ts(now), ts(expires), 0, "", "{}", []byte{}); err != nil {
		return api.AuditExport{}, err
	}
	contentLimit := int64(api.MaxAuditExportBytes)
	if remaining := int64(api.MaxReadyAuditExportBytes) - readyBytes; remaining < contentLimit {
		contentLimit = remaining
	}
	if contentLimit < 1 {
		return api.AuditExport{}, api.ErrLimit
	}
	content := auditExportWriter{ctx: ctx, tx: tx, id: exportID, limit: contentLimit, hash: sha256.New()}
	if err := content.append([]byte(`{"schema":`)); err != nil {
		return api.AuditExport{}, err
	}
	for _, value := range []struct {
		prefix string
		value  any
	}{
		{"", "tailterm-project-audit"},
		{`,"formatVersion":`, req.FormatVersion},
		{`,"sourceProject":`, taskID},
		{`,"recordedBy":`, by},
		{`,"createdAt":`, now},
		{`,"expiresAt":`, expires},
	} {
		if err := content.append([]byte(value.prefix)); err != nil {
			return api.AuditExport{}, err
		}
		if err := content.appendJSON(value.value); err != nil {
			return api.AuditExport{}, err
		}
	}
	if err := content.append([]byte(`,"streams":{`)); err != nil {
		return api.AuditExport{}, err
	}
	queries := exportQueries
	if req.FormatVersion == api.AuditExportFormatVersion {
		queries = append(append(append(append([]exportQuery(nil), exportQueries...), allocationIntentExportQueries...), projectPauseExportQueries...), queueExportQueries...)
		queries = append(queries, phaseSuccessorExportQueries[:2]...)
		legacyPresent, err := hasLegacyPhaseSuccessorTables(ctx, tx)
		if err != nil {
			return api.AuditExport{}, err
		}
		if legacyPresent {
			queries = append(queries, phaseSuccessorExportQueries[2:]...)
		}
	}
	for index, q := range queries {
		if index > 0 {
			if err := content.append([]byte(",")); err != nil {
				return api.AuditExport{}, err
			}
		}
		count, err := writeExportRows(ctx, tx, q, taskID, &content)
		if err != nil {
			return api.AuditExport{}, err
		}
		cutoffs[q.name] = count
	}
	// Independent sequence cutoffs make the frozen boundary explicit without
	// pretending that unrelated streams share one sequence space.
	for key, query := range map[string]string{"messages": "SELECT coalesce(max(seq),0) FROM messages WHERE task_id=?", "events": "SELECT coalesce(max(seq),0) FROM events WHERE task_id=?", "workItemChanges": "SELECT coalesce(max(c.seq),0) FROM work_item_changes c JOIN work_items w ON w.id=c.item_id WHERE w.task_id=?", "messageAuditEvents": "SELECT coalesce(max(seq),0) FROM message_audit_events WHERE message_task_id=?"} {
		var high int64
		if err := tx.QueryRowContext(ctx, query, taskID).Scan(&high); err != nil {
			return api.AuditExport{}, err
		}
		cutoffs[key] = high
	}
	narrativeCutoffs := map[string]int64{}
	nr, err := tx.QueryContext(ctx, `SELECT item_id,current_seq FROM narrative_state WHERE task_id=? ORDER BY item_id`, taskID)
	if err != nil {
		return api.AuditExport{}, err
	}
	for nr.Next() {
		var id string
		var seq int64
		if err := nr.Scan(&id, &seq); err != nil {
			nr.Close()
			return api.AuditExport{}, err
		}
		narrativeCutoffs[id] = seq
	}
	if err := nr.Close(); err != nil {
		return api.AuditExport{}, err
	}
	cutoffs["narrativeByItem"] = narrativeCutoffs
	if req.FormatVersion == api.AuditExportFormatVersion {
		var queueCutoff int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(v.seq),0) FROM queue_events v JOIN queue_entries e ON e.id=v.entry_id WHERE e.target_task_id=? OR e.source_task_id=?`, taskID, taskID).Scan(&queueCutoff); err != nil {
			return api.AuditExport{}, err
		}
		cutoffs["queueEvents"] = queueCutoff
	}
	if err := content.append([]byte(`},"streamCutoffs":`)); err != nil {
		return api.AuditExport{}, err
	}
	if err := content.appendJSON(cutoffs); err != nil {
		return api.AuditExport{}, err
	}
	if err := content.append([]byte("}\n")); err != nil {
		return api.AuditExport{}, err
	}
	if err := content.flush(); err != nil {
		return api.AuditExport{}, err
	}
	if readyBytes+content.bytes > api.MaxReadyAuditExportBytes {
		return api.AuditExport{}, api.ErrLimit
	}
	hash := hex.EncodeToString(content.hash.Sum(nil))
	cutoffJSON, _ := json.Marshal(cutoffs)
	if _, err = tx.ExecContext(ctx, `UPDATE audit_exports SET byte_count=?,sha256=?,cutoffs=? WHERE id=?`, content.bytes, hash, string(cutoffJSON), exportID); err != nil {
		return api.AuditExport{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_export_receipts(id,task_id,by_node,by_user,request_id,request_hash,export_id,created_at) VALUES(?,?,?,?,?,?,?,?)`, receiptID, taskID, by.Node, by.User, req.RequestID, digest, exportID, ts(now)); err != nil {
		return api.AuditExport{}, err
	}
	if err = tx.Commit(); err != nil {
		return api.AuditExport{}, err
	}
	return api.AuditExport{ID: exportID, Schema: "tailterm-project-audit", FormatVersion: req.FormatVersion, SourceProject: taskID, RecordedBy: by, CreatedAt: now, ExpiresAt: expires, ByteCount: content.bytes, SHA256: hash, StreamCutoffs: cutoffs, Available: true, Receipt: api.AuditExportReceipt{ID: receiptID, RequestID: req.RequestID, TaskID: taskID, ExportID: exportID, CreatedAt: now}}, nil
}

func (s *Store) GetAuditExportChunk(ctx context.Context, taskID, exportID string, offset int64, limit int, by api.Caller) (api.AuditExportChunk, error) {
	if !api.ValidID(taskID, "tsk") || !strings.HasPrefix(exportID, "aex_") || offset < 0 || offset > api.MaxAuditExportBytes || limit < 1 || limit > api.MaxAuditExportChunkBytes {
		return api.AuditExportChunk{}, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.AuditExportChunk{}, err
	}
	defer tx.Rollback()
	if err = s.expireAuditExports(ctx, tx, taskID, now); err != nil {
		return api.AuditExportChunk{}, err
	}
	var content []byte
	var total int64
	var hash, expires string
	var available int
	err = tx.QueryRowContext(ctx, `SELECT substr(content,?,?),byte_count,sha256,expires_at,content IS NOT NULL FROM audit_exports WHERE id=? AND task_id=? AND by_node=? AND by_user=?`, offset+1, limit, exportID, taskID, by.Node, by.User).Scan(&content, &total, &hash, &expires, &available)
	if errors.Is(err, sql.ErrNoRows) {
		return api.AuditExportChunk{}, api.ErrNotFound
	}
	if err != nil {
		return api.AuditExportChunk{}, err
	}
	if available == 0 || !now.Before(parseTS(expires)) {
		if err = tx.Commit(); err != nil {
			return api.AuditExportChunk{}, err
		}
		return api.AuditExportChunk{}, api.ErrExpired
	}
	if offset > total {
		return api.AuditExportChunk{}, api.ErrInvalid
	}
	end := offset + int64(len(content))
	if end > total || len(content) > limit {
		return api.AuditExportChunk{}, errors.New("invalid stored audit export bounds")
	}
	data := append([]byte(nil), content...)
	if err = tx.Commit(); err != nil {
		return api.AuditExportChunk{}, err
	}
	return api.AuditExportChunk{ExportID: exportID, Offset: offset, NextOffset: end, Total: total, SHA256: hash, Data: data, Complete: end == total}, nil
}
