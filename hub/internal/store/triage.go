package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
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
	out := api.WorkItemTriage{TaskID: task, StaleDays: staleDays, GeneratedAt: ts(now), Duplicates: []api.TriageDuplicate{}, AlreadyReleased: []api.TriageAlreadyDone{}, Stale: []api.TriageStale{}, HeldForTriage: []api.TriageHeld{}, ReviewFolds: []api.TriageFold{}}
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
	ids := make([]string, 0, len(open))
	for _, item := range open {
		ids = append(ids, item.ID)
	}
	ineligible, err := foldIneligible(ctx, s.db, task, ids)
	if err != nil {
		return zero, err
	}
	for _, item := range open {
		if heldReviewFollowUp(item) {
			held := api.TriageHeld{Item: ref(item), SourceMessageSeq: item.SourceMessageSeq, File: heldFollowUpFile(item), Candidates: []api.TriageItem{}}
			if held.File != "" {
				for _, other := range open {
					if other.ID != item.ID && ineligible[other.ID] == "" && namesFile(other.Description, held.File) {
						held.Candidates = append(held.Candidates, ref(other))
					}
				}
			}
			out.HeldForTriage = append(out.HeldForTriage, held)
		}
	}
	cutoff := now.Add(-time.Duration(staleDays) * 24 * time.Hour)
	if out.ReviewFolds, err = reviewFolds(ctx, s.db, task, cutoff); err != nil {
		return zero, err
	}
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

// heldFollowUpFile is the file of the finding a held follow-up was filed from:
// the description's second line onward is the finding JSON.
func heldFollowUpFile(item api.WorkItem) string {
	_, raw, ok := strings.Cut(item.Description, "\n")
	if !ok {
		return ""
	}
	var f api.ReviewFinding
	if json.Unmarshal([]byte(raw), &f) != nil {
		return ""
	}
	return f.File
}

// reviewFolds lists the review findings folded at or after cutoff, whatever
// the target's status is now, ordered by fold time and then target.
func reviewFolds(ctx context.Context, q queryRower, task string, cutoff time.Time) ([]api.TriageFold, error) {
	rows, err := q.QueryContext(ctx, `SELECT w.id,w.title,w.status,v.title,v.content,v.ingested_at FROM narrative_artifacts a
 JOIN narrative_artifact_versions v ON v.artifact_id=a.id AND v.version=a.latest_version
 JOIN work_items w ON w.task_id=a.task_id AND w.id=a.item_id
 WHERE a.task_id=? AND a.namespace=? ORDER BY w.seq`, task, reviewFoldNamespace)
	if err != nil {
		return nil, err
	}
	type dated struct {
		fold api.TriageFold
		at   time.Time
	}
	var all []dated
	for rows.Next() {
		var fold api.TriageFold
		var content, ingested string
		if err = rows.Scan(&fold.Item.ID, &fold.Item.Title, &fold.Item.Status, &fold.Title, &content, &ingested); err != nil {
			rows.Close()
			return nil, err
		}
		at := parseTS(ingested)
		if at.Before(cutoff) {
			continue
		}
		fold.FoldedAt = ingested
		fold.ReviewParent, fold.MessageSeq, fold.FindingID = parseReviewFold(content)
		all = append(all, dated{fold, at})
	}
	if err = closeRows(rows); err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	out := make([]api.TriageFold, 0, len(all))
	for _, d := range all {
		out = append(out, d.fold)
	}
	return out, nil
}

// parseReviewFold reads the note foldReviewFinding writes: "Review finding ID
// from PARENT, message #SEQ, folded as evidence." and then the finding JSON.
func parseReviewFold(content string) (parent string, seq int64, finding string) {
	head, raw, _ := strings.Cut(content, "\n")
	var f api.ReviewFinding
	if json.Unmarshal([]byte(raw), &f) == nil {
		finding = f.ID
	}
	head = strings.TrimSuffix(head, ", folded as evidence.")
	if i := strings.LastIndex(head, " from "); i >= 0 {
		head = head[i+len(" from "):]
	}
	parent, rest, _ := strings.Cut(head, ", message #")
	seq, _ = strconv.ParseInt(rest, 10, 64)
	return parent, seq, finding
}
