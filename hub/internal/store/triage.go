package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/triage"
)

// WorkItemTriage computes backlog suggestions for the owner to confirm:
// likely duplicate open items, open items a done item appears to have
// delivered, and open items with no activity for staleDays. It only reads.
// The owner's unbound session (no agent), an available database handler's
// exact run or the active backlog steward's exact run may read it; other
// agent sessions are refused.
func (s *Store) WorkItemTriage(ctx context.Context, task string, staleDays int, agentID, runID string) (api.WorkItemTriage, error) {
	var zero api.WorkItemTriage
	if !api.ValidID(task, "tsk") || staleDays < 0 || staleDays > 3650 {
		return zero, api.ErrInvalid
	}
	if staleDays == 0 {
		staleDays = triage.DefaultStaleDays
	}
	if agentID != "" || runID != "" {
		if err := requireScopeHandler(s.db, ctx, task, agentID, runID); err != nil {
			if stewardErr := requireActiveStewardRun(ctx, s.db, task, agentID, runID); stewardErr != nil {
				return zero, fmt.Errorf("%w: only the owner or a database handler (or the active backlog steward's exact run) may run triage", api.ErrConflict)
			}
		}
	}
	if _, err := s.GetTask(ctx, task); err != nil {
		return zero, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+workItemCols+` FROM work_items WHERE task_id=? ORDER BY seq`, task)
	if err != nil {
		return zero, err
	}
	var open, done []api.WorkItem
	for rows.Next() {
		item, err := scanWorkItem(rows)
		if err != nil {
			rows.Close()
			return zero, err
		}
		switch item.Status {
		case "done":
			done = append(done, item)
		case "dismissed":
		default:
			open = append(open, item)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return zero, err
	}
	now := s.now()
	out := api.WorkItemTriage{TaskID: task, StaleDays: staleDays, GeneratedAt: ts(now), Duplicates: []api.TriageDuplicate{}, AlreadyReleased: []api.TriageAlreadyDone{}, Stale: []api.TriageStale{}, HeldForTriage: []api.TriageHeld{}}
	ref := func(item api.WorkItem) api.TriageItem {
		return api.TriageItem{ID: item.ID, Title: item.Title, Status: item.Status}
	}
	for i := range open {
		for j := i + 1; j < len(open); j++ {
			title := triage.Similarity(open[i].Title, open[j].Title)
			criteria := triage.CriteriaSimilarity(open[i].Description, open[j].Description)
			if title >= triage.TitleThreshold || criteria >= triage.CriteriaThreshold {
				out.Duplicates = append(out.Duplicates, api.TriageDuplicate{Items: []api.TriageItem{ref(open[i]), ref(open[j])}, TitleSimilarity: title, CriteriaSimilarity: criteria})
			}
		}
	}
	for _, item := range open {
		for _, d := range done {
			title := triage.Similarity(item.Title, d.Title)
			criteria := triage.CriteriaSimilarity(item.Description, d.Description)
			if title >= triage.TitleThreshold || criteria >= triage.CriteriaThreshold {
				match := ref(d)
				out.AlreadyReleased = append(out.AlreadyReleased, api.TriageAlreadyDone{Item: ref(item), Done: &match, TitleSimilarity: title, CriteriaSimilarity: criteria})
			}
		}
	}
	for _, item := range open {
		if heldReviewFollowUp(item) {
			out.HeldForTriage = append(out.HeldForTriage, api.TriageHeld{Item: ref(item), SourceMessageSeq: item.SourceMessageSeq})
		}
	}
	cutoff := now.Add(-time.Duration(staleDays) * 24 * time.Hour)
	for _, item := range open {
		var queued int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM team_queue_entries WHERE task_id=? AND item_id=? AND (state='queued' OR `+queueHoldsSQL+`)`, task, item.ID).Scan(&queued); err != nil {
			return zero, err
		}
		if queued > 0 {
			continue // queued work is not idle
		}
		last, err := itemLastActivity(ctx, s.db, task, item)
		if err != nil {
			return zero, err
		}
		if last.Before(cutoff) {
			out.Stale = append(out.Stale, api.TriageStale{Item: ref(item), LastActivity: ts(last), IdleDays: int(now.Sub(last).Hours() / 24)})
		}
	}
	return out, nil
}

// itemLastActivity is the latest change to an item or message linked to it.
func itemLastActivity(ctx context.Context, q queryRower, task string, item api.WorkItem) (time.Time, error) {
	last := item.UpdatedAt
	var linked sql.NullString
	err := q.QueryRowContext(ctx, `SELECT MAX(m.created_at) FROM messages m WHERE m.task_id=? AND m.seq IN (
 SELECT message_seq FROM message_work_item_links WHERE item_task_id=? AND item_id=?
 UNION SELECT message_seq FROM message_audit_links WHERE item_task_id=? AND item_id=?)`, task, task, item.ID, task, item.ID).Scan(&linked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return last, err
	}
	if linked.Valid {
		if at := parseTS(linked.String); at.After(last) {
			last = at
		}
	}
	return last, nil
}

// heldReviewFollowUp matches the description fileReviewFollowUp writes. Once
// the handler refines the item its revision rises and it leaves the list.
func heldReviewFollowUp(item api.WorkItem) bool {
	return item.Revision == 1 && strings.HasPrefix(item.Description, "Review follow-up from ") && strings.Contains(item.Description, "Held for triage.")
}
