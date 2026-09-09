package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const historySchema = `
CREATE TABLE IF NOT EXISTS work_item_revisions (
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK(revision > 0),
  item_seq INTEGER NOT NULL,
  item_kind TEXT NOT NULL,
  title TEXT NOT NULL,
  description TEXT NOT NULL,
  status TEXT NOT NULL,
  priority TEXT NOT NULL,
  source_message_seq INTEGER NOT NULL DEFAULT 0,
  created_agent TEXT NOT NULL DEFAULT '',
  created_node TEXT NOT NULL,
  created_user TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_agent TEXT NOT NULL DEFAULT '',
  updated_run_id TEXT NOT NULL DEFAULT '',
  updated_node TEXT NOT NULL,
  updated_user TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  attribution_kind TEXT NOT NULL CHECK(attribution_kind='shared_workspace_claim'),
  change_kind TEXT NOT NULL CHECK(change_kind IN ('created','updated','checkpoint')),
  changed_fields TEXT,
  source_change_seq INTEGER UNIQUE,
  provenance TEXT NOT NULL CHECK(provenance IN ('native','reconstructed_change_log','current_row_checkpoint')),
  PRIMARY KEY(item_task_id,item_id,revision),
  FOREIGN KEY(item_task_id,item_id) REFERENCES work_items(task_id,id),
  FOREIGN KEY(source_change_seq) REFERENCES work_item_changes(seq),
  CHECK((provenance='current_row_checkpoint' AND source_change_seq IS NULL AND change_kind='checkpoint' AND changed_fields IS NULL)
     OR (provenance<>'current_row_checkpoint' AND source_change_seq IS NOT NULL AND change_kind IN ('created','updated') AND changed_fields IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS work_item_revisions_item ON work_item_revisions(item_task_id,item_id,revision);
CREATE TABLE IF NOT EXISTS work_item_history_gaps (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  first_revision INTEGER NOT NULL CHECK(first_revision > 0),
  last_revision INTEGER NOT NULL CHECK(last_revision >= first_revision),
  reason_code TEXT NOT NULL CHECK(reason_code IN ('missing_revision','duplicate_revision','invalid_change_json','final_projection_mismatch','immutable_snapshot_collision','unresolved_revision_link','dispatch_snapshot_mismatch')),
  reason_detail TEXT NOT NULL DEFAULT '' CHECK(length(reason_detail)<=512),
  detected_at TEXT NOT NULL,
  FOREIGN KEY(item_task_id,item_id) REFERENCES work_items(task_id,id),
  UNIQUE(item_task_id,item_id,first_revision,last_revision,reason_code)
);
CREATE INDEX IF NOT EXISTS work_item_history_gaps_item ON work_item_history_gaps(item_task_id,item_id,seq);
CREATE TABLE IF NOT EXISTS work_item_history_state (
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  observed_current_revision INTEGER NOT NULL,
  latest_materialized_revision INTEGER NOT NULL,
  complete INTEGER NOT NULL CHECK(complete IN (0,1)),
  checked_at TEXT NOT NULL,
  PRIMARY KEY(item_task_id,item_id),
  FOREIGN KEY(item_task_id,item_id) REFERENCES work_items(task_id,id)
);
CREATE TABLE IF NOT EXISTS work_item_update_requests (
  receipt_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  agent_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  request_id TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  result_revision INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(task_id,agent_id,by_node,by_user,request_id),
  FOREIGN KEY(task_id,item_id,result_revision) REFERENCES work_item_revisions(item_task_id,item_id,revision)
);`

const workItemRevisionCols = `item_id,item_task_id,item_kind,title,description,status,priority,item_seq,revision,source_message_seq,created_agent,created_node,created_user,created_at,updated_agent,updated_node,updated_user,updated_at,updated_run_id,attribution_kind,change_kind,provenance,changed_fields`

type historyChange struct {
	seq, revision                            int64
	kind, fields, agent, node, user, created string
}

// WorkItemHistoryGapError distinguishes a known unavailable revision from a
// revision which was never observed for the item.
type WorkItemHistoryGapError struct{ Gap api.HistoryGap }

func (e *WorkItemHistoryGapError) Error() string {
	return "work item revision is unavailable: " + e.Gap.ReasonCode
}
func (e *WorkItemHistoryGapError) Unwrap() error { return api.ErrConflict }

func scanWorkItemRevision(row rowScanner) (api.WorkItemRevision, error) {
	var r api.WorkItemRevision
	var created, updated string
	var changed sql.NullString
	err := row.Scan(&r.ItemID, &r.TaskID, &r.Kind, &r.Title, &r.Description, &r.Status, &r.Priority, &r.ItemSeq, &r.Revision, &r.SourceMessageSeq,
		&r.CreatedBy.AgentID, &r.CreatedBy.Node, &r.CreatedBy.User, &created,
		&r.UpdatedBy.AgentID, &r.UpdatedBy.Node, &r.UpdatedBy.User, &updated,
		&r.UpdatedRunID, &r.AttributionKind, &r.ChangeKind, &r.Provenance, &changed)
	r.CreatedAt, r.UpdatedAt = parseTS(created), parseTS(updated)
	if err == nil && changed.Valid {
		err = json.Unmarshal([]byte(changed.String), &r.ChangedFields)
	}
	return r, err
}

func revisionFromItem(item api.WorkItem, runID, changeKind, provenance string, changed []string) api.WorkItemRevision {
	return api.WorkItemRevision{
		ItemID: item.ID, TaskID: item.TaskID, Kind: item.Kind, Title: item.Title, Description: item.Description,
		Status: item.Status, Priority: item.Priority, ItemSeq: item.Seq, Revision: item.Revision,
		SourceMessageSeq: item.SourceMessageSeq, CreatedBy: item.CreatedBy, UpdatedBy: item.UpdatedBy,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, UpdatedRunID: runID,
		AttributionKind: "shared_workspace_claim", ChangeKind: changeKind, Provenance: provenance,
		ChangedFields: append([]string(nil), changed...),
	}
}

func insertWorkItemRevision(ctx context.Context, tx *sql.Tx, r api.WorkItemRevision, sourceChangeSeq any) error {
	var changed any
	if r.Provenance != "current_row_checkpoint" {
		b, _ := json.Marshal(r.ChangedFields)
		changed = string(b)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO work_item_revisions
(item_task_id,item_id,revision,item_seq,item_kind,title,description,status,priority,source_message_seq,created_agent,created_node,created_user,created_at,updated_agent,updated_run_id,updated_node,updated_user,updated_at,attribution_kind,change_kind,changed_fields,source_change_seq,provenance)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.TaskID, r.ItemID, r.Revision, r.ItemSeq, r.Kind, r.Title, r.Description, r.Status, r.Priority, r.SourceMessageSeq,
		r.CreatedBy.AgentID, r.CreatedBy.Node, r.CreatedBy.User, ts(r.CreatedAt), r.UpdatedBy.AgentID, r.UpdatedRunID,
		r.UpdatedBy.Node, r.UpdatedBy.User, ts(r.UpdatedAt), r.AttributionKind, r.ChangeKind, changed, sourceChangeSeq, r.Provenance)
	return err
}

func sameRevision(a, b api.WorkItemRevision) bool {
	ac, bc := append([]string(nil), a.ChangedFields...), append([]string(nil), b.ChangedFields...)
	sort.Strings(ac)
	sort.Strings(bc)
	return a.ItemID == b.ItemID && a.TaskID == b.TaskID && a.Kind == b.Kind && a.Title == b.Title && a.Description == b.Description &&
		a.Status == b.Status && a.Priority == b.Priority && a.ItemSeq == b.ItemSeq && a.Revision == b.Revision &&
		a.SourceMessageSeq == b.SourceMessageSeq && a.CreatedBy == b.CreatedBy && a.UpdatedBy == b.UpdatedBy &&
		a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt) && a.UpdatedRunID == b.UpdatedRunID &&
		a.AttributionKind == b.AttributionKind && a.ChangeKind == b.ChangeKind && a.Provenance == b.Provenance && strings.Join(ac, "\x00") == strings.Join(bc, "\x00")
}

func sameProjection(r api.WorkItemRevision, item api.WorkItem) bool {
	return r.ItemID == item.ID && r.TaskID == item.TaskID && r.Kind == item.Kind && r.Title == item.Title && r.Description == item.Description &&
		r.Status == item.Status && r.Priority == item.Priority && r.ItemSeq == item.Seq && r.Revision == item.Revision &&
		r.SourceMessageSeq == item.SourceMessageSeq && r.CreatedBy == item.CreatedBy && r.UpdatedBy == item.UpdatedBy &&
		r.CreatedAt.Equal(item.CreatedAt) && r.UpdatedAt.Equal(item.UpdatedAt)
}

func addHistoryGap(ctx context.Context, tx *sql.Tx, item api.WorkItem, first, last int64, code, detail, detected string) error {
	if len(detail) > 512 {
		detail = detail[:512]
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_item_history_gaps(item_task_id,item_id,first_revision,last_revision,reason_code,reason_detail,detected_at) VALUES(?,?,?,?,?,?,?)`, item.TaskID, item.ID, first, last, code, detail, detected)
	return err
}

func parseChange(item api.WorkItem, base *api.WorkItemRevision, c historyChange) (api.WorkItemRevision, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(c.fields), &fields); err != nil || fields == nil {
		return api.WorkItemRevision{}, fmt.Errorf("invalid JSON")
	}
	allowed := map[string]bool{"kind": true, "title": true, "description": true, "status": true, "priority": true, "sourceMessageSeq": true}
	for key := range fields {
		if !allowed[key] || (c.revision != 1 && (key == "kind" || key == "sourceMessageSeq")) {
			return api.WorkItemRevision{}, fmt.Errorf("invalid field %q", key)
		}
	}
	var r api.WorkItemRevision
	if c.revision == 1 {
		if base != nil || c.kind != "created" {
			return r, fmt.Errorf("revision 1 is not a creation")
		}
		r = revisionFromItem(item, "", "created", "reconstructed_change_log", nil)
		r.Title, r.Description, r.Status, r.Priority, r.Kind, r.SourceMessageSeq = "", "", "", "", "", 0
		r.CreatedBy = api.Sender{AgentID: c.agent, Node: c.node, User: c.user}
		r.CreatedAt = parseTS(c.created)
	} else {
		if base == nil || c.kind != "updated" {
			return r, fmt.Errorf("revision %d has no trusted predecessor", c.revision)
		}
		r = *base
		r.ChangeKind = "updated"
		r.Provenance = "reconstructed_change_log"
	}
	r.Revision = c.revision
	r.UpdatedBy = api.Sender{AgentID: c.agent, Node: c.node, User: c.user}
	r.UpdatedAt = parseTS(c.created)
	keys := make([]string, 0, len(fields))
	for key, raw := range fields {
		keys = append(keys, key)
		switch key {
		case "kind":
			if json.Unmarshal(raw, &r.Kind) != nil || !validWorkItemKind(r.Kind) {
				return api.WorkItemRevision{}, fmt.Errorf("invalid kind")
			}
		case "title":
			if json.Unmarshal(raw, &r.Title) != nil || !validWorkItemTitle(r.Title) {
				return api.WorkItemRevision{}, fmt.Errorf("invalid title")
			}
		case "description":
			if json.Unmarshal(raw, &r.Description) != nil || !api.ValidText(r.Description, api.MaxTextLen) {
				return api.WorkItemRevision{}, fmt.Errorf("invalid description")
			}
		case "status":
			if json.Unmarshal(raw, &r.Status) != nil || !validWorkItemStatus(r.Status) {
				return api.WorkItemRevision{}, fmt.Errorf("invalid status")
			}
		case "priority":
			if json.Unmarshal(raw, &r.Priority) != nil || !validWorkItemPriority(r.Priority) {
				return api.WorkItemRevision{}, fmt.Errorf("invalid priority")
			}
		case "sourceMessageSeq":
			if json.Unmarshal(raw, &r.SourceMessageSeq) != nil || r.SourceMessageSeq < 0 {
				return api.WorkItemRevision{}, fmt.Errorf("invalid source message")
			}
		}
	}
	if c.revision == 1 && (r.Kind == "" || r.Title == "" || r.Status == "" || r.Priority == "") {
		return api.WorkItemRevision{}, fmt.Errorf("incomplete creation")
	}
	sort.Strings(keys)
	r.ChangedFields = keys
	return r, nil
}

func reconcileWorkItemHistory(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, historySchema); err != nil {
		return fmt.Errorf("work item history schema: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+workItemCols+` FROM work_items ORDER BY seq`)
	if err != nil {
		return err
	}
	var items []api.WorkItem
	for rows.Next() {
		item, scanErr := scanWorkItem(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	detected := ts(time.Now().UTC())
	for _, item := range items {
		if err = reconcileOneWorkItem(ctx, tx, item, detected); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func reconcileOneWorkItem(ctx context.Context, tx *sql.Tx, item api.WorkItem, detected string) error {
	var base *api.WorkItemRevision
	maxRevision := int64(0)
	existing, err := scanWorkItemRevision(tx.QueryRowContext(ctx, `SELECT `+workItemRevisionCols+` FROM work_item_revisions WHERE item_task_id=? AND item_id=? ORDER BY revision DESC LIMIT 1`, item.TaskID, item.ID))
	if err == nil {
		base, maxRevision = &existing, existing.Revision
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Missing materialized revisions below the trusted base remain explicit.
	if maxRevision > 0 {
		var below int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM work_item_revisions WHERE item_task_id=? AND item_id=? AND revision<=?`, item.TaskID, item.ID, maxRevision).Scan(&below); err != nil {
			return err
		}
		if below != maxRevision {
			if err := addHistoryGap(ctx, tx, item, 1, maxRevision, "missing_revision", "materialized history below the latest snapshot is incomplete", detected); err != nil {
				return err
			}
		}
	}
	if maxRevision == item.Revision && base != nil && !sameProjection(*base, item) {
		if err := addHistoryGap(ctx, tx, item, item.Revision, item.Revision, "immutable_snapshot_collision", "current projection differs from the preserved immutable snapshot", detected); err != nil {
			return err
		}
	}

	rows, err := tx.QueryContext(ctx, `SELECT seq,revision,kind,fields,agent_id,by_node,by_user,created_at FROM work_item_changes WHERE item_id=? AND revision>? ORDER BY revision,seq`, item.ID, maxRevision)
	if err != nil {
		return err
	}
	changes := map[int64][]historyChange{}
	for rows.Next() {
		var c historyChange
		if err := rows.Scan(&c.seq, &c.revision, &c.kind, &c.fields, &c.agent, &c.node, &c.user, &c.created); err != nil {
			rows.Close()
			return err
		}
		changes[c.revision] = append(changes[c.revision], c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	type candidate struct {
		revision api.WorkItemRevision
		source   int64
	}
	var staged []candidate
	broken := false
	for revision := maxRevision + 1; revision <= item.Revision; revision++ {
		group := changes[revision]
		if len(group) == 0 {
			if err := addHistoryGap(ctx, tx, item, revision, revision, "missing_revision", "no change row exists for revision", detected); err != nil {
				return err
			}
			broken = true
			break
		}
		if len(group) != 1 {
			if err := addHistoryGap(ctx, tx, item, revision, revision, "duplicate_revision", fmt.Sprintf("%d change rows exist for revision", len(group)), detected); err != nil {
				return err
			}
			broken = true
			break
		}
		next, parseErr := parseChange(item, base, group[0])
		if parseErr != nil {
			if err := addHistoryGap(ctx, tx, item, revision, revision, "invalid_change_json", parseErr.Error(), detected); err != nil {
				return err
			}
			broken = true
			break
		}
		if revision == item.Revision && !sameProjection(next, item) {
			if err := addHistoryGap(ctx, tx, item, revision, revision, "final_projection_mismatch", "replayed final revision differs from the current projection", detected); err != nil {
				return err
			}
			broken = true
			break
		}
		copyNext := next
		base = &copyNext
		staged = append(staged, candidate{revision: next, source: group[0].seq})
	}
	for _, c := range staged {
		if err := insertWorkItemRevision(ctx, tx, c.revision, c.source); err != nil {
			preserved, getErr := scanWorkItemRevision(tx.QueryRowContext(ctx, `SELECT `+workItemRevisionCols+` FROM work_item_revisions WHERE item_task_id=? AND item_id=? AND revision=?`, item.TaskID, item.ID, c.revision.Revision))
			if getErr != nil || !sameRevision(preserved, c.revision) {
				if gapErr := addHistoryGap(ctx, tx, item, c.revision.Revision, c.revision.Revision, "immutable_snapshot_collision", "existing immutable snapshot differs from reconstructed change", detected); gapErr != nil {
					return gapErr
				}
				broken = true
				break
			}
		}
	}
	var hasCurrent int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM work_item_revisions WHERE item_task_id=? AND item_id=? AND revision=?`, item.TaskID, item.ID, item.Revision).Scan(&hasCurrent); err != nil {
		return err
	}
	if broken && hasCurrent == 0 {
		checkpoint := revisionFromItem(item, "", "checkpoint", "current_row_checkpoint", nil)
		if err := insertWorkItemRevision(ctx, tx, checkpoint, nil); err != nil {
			if gapErr := addHistoryGap(ctx, tx, item, item.Revision, item.Revision, "immutable_snapshot_collision", "current checkpoint collides with an immutable snapshot", detected); gapErr != nil {
				return gapErr
			}
		}
	}
	if err := validateHistoricalLinks(ctx, tx, item, detected); err != nil {
		return err
	}
	return refreshWorkItemHistoryState(ctx, tx, item, detected)
}

func validateHistoricalLinks(ctx context.Context, tx *sql.Tx, item api.WorkItem, detected string) error {
	rows, err := tx.QueryContext(ctx, `SELECT item_revision FROM message_work_item_links WHERE item_task_id=? AND item_id=? ORDER BY message_seq`, item.TaskID, item.ID)
	if err != nil {
		return err
	}
	var linked []int64
	for rows.Next() {
		var rev int64
		if err := rows.Scan(&rev); err != nil {
			rows.Close()
			return err
		}
		linked = append(linked, rev)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, rev := range linked {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM work_item_revisions WHERE item_task_id=? AND item_id=? AND revision=?`, item.TaskID, item.ID, rev).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if err := addHistoryGap(ctx, tx, item, rev, rev, "unresolved_revision_link", "explicit message link refers to an unavailable revision", detected); err != nil {
				return err
			}
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT item_revision,snapshot FROM work_item_dispatches WHERE item_id=? ORDER BY seq`, item.ID)
	if err != nil {
		return err
	}
	type dispatchCheck struct {
		rev int64
		raw string
	}
	var dispatches []dispatchCheck
	for rows.Next() {
		var d dispatchCheck
		if err := rows.Scan(&d.rev, &d.raw); err != nil {
			rows.Close()
			return err
		}
		dispatches = append(dispatches, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, d := range dispatches {
		r, getErr := scanWorkItemRevision(tx.QueryRowContext(ctx, `SELECT `+workItemRevisionCols+` FROM work_item_revisions WHERE item_task_id=? AND item_id=? AND revision=?`, item.TaskID, item.ID, d.rev))
		if errors.Is(getErr, sql.ErrNoRows) {
			if err := addHistoryGap(ctx, tx, item, d.rev, d.rev, "dispatch_snapshot_mismatch", "dispatch refers to an unavailable revision", detected); err != nil {
				return err
			}
			continue
		}
		if getErr != nil {
			return getErr
		}
		var snapshot api.WorkItem
		if json.Unmarshal([]byte(d.raw), &snapshot) != nil || !sameProjection(r, snapshot) {
			if err := addHistoryGap(ctx, tx, item, d.rev, d.rev, "dispatch_snapshot_mismatch", "stored dispatch snapshot differs from immutable revision", detected); err != nil {
				return err
			}
		}
	}
	return nil
}

func refreshWorkItemHistoryState(ctx context.Context, tx *sql.Tx, item api.WorkItem, checked string) error {
	var count, latest, gaps int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(max(revision),0) FROM work_item_revisions WHERE item_task_id=? AND item_id=?`, item.TaskID, item.ID).Scan(&count, &latest); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM work_item_history_gaps WHERE item_task_id=? AND item_id=?`, item.TaskID, item.ID).Scan(&gaps); err != nil {
		return err
	}
	complete := 0
	if count == item.Revision && latest == item.Revision && gaps == 0 {
		complete = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO work_item_history_state(item_task_id,item_id,observed_current_revision,latest_materialized_revision,complete,checked_at) VALUES(?,?,?,?,?,?) ON CONFLICT(item_task_id,item_id) DO UPDATE SET observed_current_revision=excluded.observed_current_revision,latest_materialized_revision=excluded.latest_materialized_revision,complete=excluded.complete,checked_at=excluded.checked_at`, item.TaskID, item.ID, item.Revision, latest, complete, checked)
	return err
}

func (s *Store) GetWorkItemRevision(ctx context.Context, taskID, itemID string, revision int64) (api.WorkItemRevision, error) {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || revision < 1 {
		return api.WorkItemRevision{}, api.ErrInvalid
	}
	r, err := scanWorkItemRevision(s.db.QueryRowContext(ctx, `SELECT `+workItemRevisionCols+` FROM work_item_revisions WHERE item_task_id=? AND item_id=? AND revision=?`, taskID, itemID, revision))
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	item, itemErr := getWorkItem(s.db, ctx, taskID, itemID)
	if itemErr != nil {
		return r, itemErr
	}
	if revision > item.Revision {
		return r, api.ErrNotFound
	}
	gap, gapErr := scanHistoryGap(s.db.QueryRowContext(ctx, `SELECT seq,first_revision,last_revision,reason_code,reason_detail,detected_at FROM work_item_history_gaps WHERE item_task_id=? AND item_id=? AND first_revision<=? AND last_revision>=? ORDER BY seq LIMIT 1`, taskID, itemID, revision, revision))
	if gapErr == nil {
		return r, &WorkItemHistoryGapError{Gap: gap}
	}
	if errors.Is(gapErr, sql.ErrNoRows) {
		return r, &WorkItemHistoryGapError{Gap: api.HistoryGap{FirstRevision: revision, LastRevision: revision, ReasonCode: "missing_revision"}}
	}
	return r, gapErr
}

func scanHistoryGap(row rowScanner) (api.HistoryGap, error) {
	var g api.HistoryGap
	var detected string
	err := row.Scan(&g.Seq, &g.FirstRevision, &g.LastRevision, &g.ReasonCode, &g.Detail, &detected)
	g.DetectedAt = parseTS(detected)
	return g, err
}

func historyCoverage(ctx context.Context, tx *sql.Tx, item api.WorkItem) (api.HistoryCoverage, error) {
	c := api.HistoryCoverage{ObservedCurrentRevision: item.Revision, ConversationLinks: "explicit_only"}
	var complete int
	err := tx.QueryRowContext(ctx, `SELECT observed_current_revision,latest_materialized_revision,complete FROM work_item_history_state WHERE item_task_id=? AND item_id=?`, item.TaskID, item.ID).Scan(&c.ObservedCurrentRevision, &c.LatestMaterialized, &complete)
	if err != nil {
		return c, err
	}
	c.Complete = complete == 1
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM work_item_revisions WHERE item_task_id=? AND item_id=?`, item.TaskID, item.ID).Scan(&c.SnapshotCount); err != nil {
		return c, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM work_item_history_gaps WHERE item_task_id=? AND item_id=?`, item.TaskID, item.ID).Scan(&c.GapCount); err != nil {
		return c, err
	}
	if c.GapCount > 0 {
		gap, gapErr := scanHistoryGap(tx.QueryRowContext(ctx, `SELECT seq,first_revision,last_revision,reason_code,reason_detail,detected_at FROM work_item_history_gaps WHERE item_task_id=? AND item_id=? ORDER BY seq LIMIT 1`, item.TaskID, item.ID))
		if gapErr != nil {
			return c, gapErr
		}
		c.FirstGap = &gap
	}
	if item.SourceMessageSeq > 0 {
		c.SourceMessageCount = 1
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM message_work_item_links WHERE item_task_id=? AND item_id=?`, item.TaskID, item.ID).Scan(&c.ExplicitMessageCount); err != nil {
		return c, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM work_item_dispatches WHERE item_id=?`, item.ID).Scan(&c.DispatchCount); err != nil {
		return c, err
	}
	return c, nil
}

func validateHistoryPage(taskID, itemID string, after int64, limit int) error {
	if !api.ValidID(taskID, "tsk") || !api.ValidID(itemID, "wi") || after < 0 || limit < 1 || limit > api.MaxWorkItemHistoryPage+1 {
		return api.ErrInvalid
	}
	return nil
}

func (s *Store) ListWorkItemRevisions(ctx context.Context, taskID, itemID string, after int64, limit int) (api.WorkItemRevisionList, error) {
	if err := validateHistoryPage(taskID, itemID, after, limit); err != nil {
		return api.WorkItemRevisionList{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.WorkItemRevisionList{}, err
	}
	defer tx.Rollback()
	item, err := getWorkItem(tx, ctx, taskID, itemID)
	if err != nil {
		return api.WorkItemRevisionList{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+workItemRevisionCols+` FROM work_item_revisions WHERE item_task_id=? AND item_id=? AND revision>? ORDER BY revision LIMIT ?`, taskID, itemID, after, limit)
	if err != nil {
		return api.WorkItemRevisionList{}, err
	}
	out := api.WorkItemRevisionList{Revisions: []api.WorkItemRevision{}}
	for rows.Next() {
		r, scanErr := scanWorkItemRevision(rows)
		if scanErr != nil {
			rows.Close()
			return out, scanErr
		}
		out.Revisions = append(out.Revisions, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	out.Coverage, err = historyCoverage(ctx, tx, item)
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Store) ListWorkItemHistoryGaps(ctx context.Context, taskID, itemID string, after int64, limit int) (api.HistoryGapList, error) {
	if err := validateHistoryPage(taskID, itemID, after, limit); err != nil {
		return api.HistoryGapList{}, err
	}
	if _, err := s.GetWorkItem(ctx, taskID, itemID); err != nil {
		return api.HistoryGapList{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq,first_revision,last_revision,reason_code,reason_detail,detected_at FROM work_item_history_gaps WHERE item_task_id=? AND item_id=? AND seq>? ORDER BY seq LIMIT ?`, taskID, itemID, after, limit)
	if err != nil {
		return api.HistoryGapList{}, err
	}
	out := api.HistoryGapList{Gaps: []api.HistoryGap{}}
	for rows.Next() {
		gap, scanErr := scanHistoryGap(rows)
		if scanErr != nil {
			rows.Close()
			return out, scanErr
		}
		out.Gaps = append(out.Gaps, gap)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	return out, nil
}

type messageLinkKey struct {
	seq          int64
	task         string
	revision     int64
	source       bool
	relationship string
}

func (s *Store) ListWorkItemMessages(ctx context.Context, taskID, itemID string, revision, after int64, limit int) (api.WorkItemMessageList, error) {
	if err := validateHistoryPage(taskID, itemID, after, limit); err != nil || revision < 0 {
		if err != nil {
			return api.WorkItemMessageList{}, err
		}
		return api.WorkItemMessageList{}, api.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.WorkItemMessageList{}, err
	}
	defer tx.Rollback()
	item, err := getWorkItem(tx, ctx, taskID, itemID)
	if err != nil {
		return api.WorkItemMessageList{}, err
	}
	query := `WITH candidates(message_seq,message_task_id,item_revision,is_source,relationship) AS (
SELECT source_message_seq,task_id,1,1,'' FROM work_items WHERE task_id=? AND id=? AND source_message_seq>0
UNION ALL
SELECT message_seq,message_task_id,item_revision,0,relationship FROM message_work_item_links WHERE item_task_id=? AND item_id=?
), combined(message_seq,message_task_id,item_revision,is_source,relationship) AS (
SELECT message_seq,message_task_id,CASE WHEN max(CASE WHEN relationship<>'' THEN item_revision ELSE 0 END)>0 THEN max(CASE WHEN relationship<>'' THEN item_revision ELSE 0 END) ELSE 1 END item_revision,max(is_source),max(relationship)
FROM candidates GROUP BY message_seq,message_task_id)
SELECT message_seq,message_task_id,item_revision,is_source,relationship FROM combined WHERE message_seq>?`
	args := []any{taskID, itemID, taskID, itemID, after}
	if revision > 0 {
		query += ` AND item_revision=?`
		args = append(args, revision)
	}
	query += ` ORDER BY message_seq LIMIT ?`
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return api.WorkItemMessageList{}, err
	}
	var keys []messageLinkKey
	for rows.Next() {
		var k messageLinkKey
		var source int
		if err := rows.Scan(&k.seq, &k.task, &k.revision, &source, &k.relationship); err != nil {
			rows.Close()
			return api.WorkItemMessageList{}, err
		}
		k.source = source == 1
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return api.WorkItemMessageList{}, err
	}
	rows.Close()
	out := api.WorkItemMessageList{Links: []api.WorkItemMessageLink{}}
	for _, k := range keys {
		message, loadErr := loadMessage(tx, ctx, k.task, k.seq)
		if loadErr != nil {
			return out, loadErr
		}
		coverage := "gap"
		var provenance string
		err = tx.QueryRowContext(ctx, `SELECT provenance FROM work_item_revisions WHERE item_task_id=? AND item_id=? AND revision=?`, taskID, itemID, k.revision).Scan(&provenance)
		if err == nil {
			if provenance == "current_row_checkpoint" {
				coverage = "checkpoint"
			} else {
				coverage = "verified"
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		out.Links = append(out.Links, api.WorkItemMessageLink{ItemRevision: k.revision, RevisionCoverage: coverage, Source: k.source, Relationship: k.relationship, Message: message})
	}
	out.Coverage, err = historyCoverage(ctx, tx, item)
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}
