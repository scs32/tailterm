package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"
	// The hub container may not ship zoneinfo; per-day buckets need it.
	_ "time/tzdata"

	"github.com/scs32/tailterm/hub/internal/api"
)

// ErrInterventionForbidden marks an agent-authored intervention so the HTTP
// boundary can return 403: only the owner records owner interventions.
var ErrInterventionForbidden = errors.New("only the owner can record an owner intervention")

// Interventions are insert-only. The triggers keep them that way even for
// code that bypasses the store's methods.
const ownerInterventionsSchema = `
CREATE TABLE IF NOT EXISTS owner_interventions (
  message_seq INTEGER PRIMARY KEY,
  task_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('release','nudge','decision-on-behalf','gate-fix','cleanup','diagnosis','status','other')),
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  product_task_id TEXT NOT NULL DEFAULT '',
  product_item_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(task_id,message_seq),
  FOREIGN KEY(task_id,message_seq) REFERENCES messages(task_id,seq)
);
CREATE INDEX IF NOT EXISTS owner_interventions_task ON owner_interventions(task_id,created_at);
CREATE TRIGGER IF NOT EXISTS owner_interventions_no_update BEFORE UPDATE ON owner_interventions
BEGIN SELECT RAISE(ABORT,'owner interventions are immutable'); END;
CREATE TRIGGER IF NOT EXISTS owner_interventions_no_delete BEFORE DELETE ON owner_interventions
BEGIN SELECT RAISE(ABORT,'owner interventions are immutable'); END;`

func insertIntervention(ctx context.Context, tx *sql.Tx, message *api.Message, record api.Intervention) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO owner_interventions
(message_seq,task_id,kind,item_task_id,item_id,product_task_id,product_item_id,created_at)
VALUES(?,?,?,?,?,?,?,?)`, message.Seq, message.TaskID, record.Kind, record.ItemTaskID, record.ItemID,
		record.ProductTaskID, record.ProductItemID, ts(message.CreatedAt)); err != nil {
		return err
	}
	copy := record
	message.Intervention = &copy
	return nil
}

// CreateIntervention atomically stores an owner intervention as a Board
// message linked to the item it concerns, its typed record, the message event
// and the post receipt. An exact retry returns the original message.
func (s *Store) CreateIntervention(ctx context.Context, taskID string, req api.CreateInterventionRequest, by api.Caller) (api.Message, error) {
	if req.AgentID != "" {
		return api.Message{}, ErrInterventionForbidden
	}
	if !api.ValidID(taskID, "tsk") || !validRequestID(req.RequestID) {
		return api.Message{}, api.ErrInvalid
	}
	if err := api.ValidateIntervention(req); err != nil {
		return api.Message{}, err
	}
	payload := operationRequestHash("owner_intervention", req)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Message{}, err
	}
	defer tx.Rollback()
	if replay, found, err := replayMessageRequest(tx, ctx, taskID, req.RequestID, "", payload, by); found || err != nil {
		return replay, err
	}
	task, err := decisionTask(tx, ctx, taskID)
	if err != nil {
		return api.Message{}, err
	}
	if task.Status != api.TaskOpen {
		return api.Message{}, api.ErrClosed
	}
	// The concerned item must belong to this project; any status is allowed.
	item, err := getWorkItem(tx, ctx, taskID, req.ItemID)
	if err != nil {
		return api.Message{}, err
	}
	record := api.Intervention{Kind: req.Kind, ItemTaskID: item.TaskID, ItemID: item.ID}
	// The product item usually lives in the product's home project.
	if req.ProductItemID != "" {
		err = tx.QueryRowContext(ctx, `SELECT task_id FROM work_items WHERE id=?`, req.ProductItemID).Scan(&record.ProductTaskID)
		if errors.Is(err, sql.ErrNoRows) {
			return api.Message{}, api.ErrNotFound
		}
		if err != nil {
			return api.Message{}, err
		}
		record.ProductItemID = req.ProductItemID
	}
	messageReq := api.PostMessageRequest{
		RequestID: req.RequestID,
		Text:      api.FormatIntervention(req),
		WorkItems: []api.MessageWorkItem{{ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}},
	}
	message, err := s.insertMessage(ctx, tx, task, messageReq, api.Agent{}, by, false, false)
	if err != nil {
		return message, err
	}
	if err = insertIntervention(ctx, tx, &message, record); err != nil {
		return message, err
	}
	if err = insertMessagePostReceipt(ctx, tx, &message, req.RequestID, payload, by); err != nil {
		return message, err
	}
	if err = tx.Commit(); err != nil {
		return message, err
	}
	s.notify(taskID)
	return message, nil
}

// interventionLocation accepts IANA names only. "Local" would bucket by the
// hub host's zone, which the caller cannot see.
func interventionLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	if name == "Local" || !api.ValidText(name, 64) {
		return nil, api.ErrInvalid
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, api.ErrInvalid
	}
	return loc, nil
}

// ListInterventions returns one page of intervention messages in sequence
// order and a summary of every intervention in the project, bucketed by local
// calendar day in timeZone.
func (s *Store) ListInterventions(ctx context.Context, taskID string, after int64, limit int, timeZone string) (api.InterventionList, error) {
	if !api.ValidID(taskID, "tsk") || after < 0 {
		return api.InterventionList{}, api.ErrInvalid
	}
	if limit <= 0 {
		limit = api.MaxInterventionPage
	}
	if limit > api.MaxInterventionPage {
		return api.InterventionList{}, api.ErrInvalid
	}
	loc, err := interventionLocation(timeZone)
	if err != nil {
		return api.InterventionList{}, err
	}
	if _, err := decisionTask(s.db, ctx, taskID); err != nil {
		return api.InterventionList{}, err
	}
	summary, err := s.interventionSummary(ctx, taskID, loc)
	if err != nil {
		return api.InterventionList{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT message_seq FROM owner_interventions
WHERE task_id=? AND message_seq>? ORDER BY message_seq LIMIT ?`, taskID, after, limit+1)
	if err != nil {
		return api.InterventionList{}, err
	}
	seqs := make([]int64, 0, limit+1)
	for rows.Next() {
		var seq int64
		if err = rows.Scan(&seq); err != nil {
			rows.Close()
			return api.InterventionList{}, err
		}
		seqs = append(seqs, seq)
	}
	if err = rows.Close(); err != nil {
		return api.InterventionList{}, err
	}
	if err = rows.Err(); err != nil {
		return api.InterventionList{}, err
	}
	more := len(seqs) > limit
	if more {
		seqs = seqs[:limit]
	}
	out := api.InterventionList{Interventions: make([]api.Message, 0, len(seqs)), Summary: summary}
	for _, seq := range seqs {
		message, err := loadMessage(s.db, ctx, taskID, seq)
		if err != nil {
			return api.InterventionList{}, err
		}
		out.Interventions = append(out.Interventions, message)
	}
	if more && len(seqs) > 0 {
		out.NextAfter = seqs[len(seqs)-1]
	}
	return out, nil
}

func (s *Store) interventionSummary(ctx context.Context, taskID string, loc *time.Location) (api.InterventionSummary, error) {
	summary := api.InterventionSummary{TimeZone: loc.String(), ByKind: map[string]int{}, Days: []api.InterventionDay{}, Unlinked: []api.InterventionRef{}}
	rows, err := s.db.QueryContext(ctx, `SELECT message_seq,kind,item_id,product_item_id,created_at
FROM owner_interventions WHERE task_id=? ORDER BY message_seq DESC`, taskID)
	if err != nil {
		return summary, err
	}
	defer rows.Close()
	days := map[string]*api.InterventionDay{}
	for rows.Next() {
		var ref api.InterventionRef
		var created string
		if err := rows.Scan(&ref.Seq, &ref.Kind, &ref.ItemID, &ref.ProductItemID, &created); err != nil {
			return summary, err
		}
		ref.Day = parseTS(created).In(loc).Format("2006-01-02")
		day := days[ref.Day]
		if day == nil {
			day = &api.InterventionDay{Day: ref.Day, ByKind: map[string]int{}}
			days[ref.Day] = day
		}
		summary.Total++
		day.Total++
		summary.ByKind[ref.Kind]++
		day.ByKind[ref.Kind]++
		if ref.ProductItemID != "" {
			summary.Linked++
			day.Linked++
		} else if len(summary.Unlinked) < api.MaxInterventionPage {
			summary.Unlinked = append(summary.Unlinked, ref)
		}
	}
	if err := rows.Err(); err != nil {
		return summary, err
	}
	for _, day := range days {
		summary.Days = append(summary.Days, *day)
	}
	sort.Slice(summary.Days, func(i, j int) bool { return summary.Days[i].Day > summary.Days[j].Day })
	return summary, nil
}
