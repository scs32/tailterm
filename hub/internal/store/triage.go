package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	openP := make([]triage.Profile, len(open))
	for i, item := range open {
		openP[i] = triage.NewProfile(item.Title, item.Description)
	}
	doneP := make([]triage.Profile, len(done))
	for i, item := range done {
		doneP[i] = triage.NewProfile(item.Title, item.Description)
	}
	listed := map[[2]int]bool{}
	for _, m := range triage.Duplicates(openP) {
		listed[[2]int{m.A, m.B}] = true
		out.Duplicates = append(out.Duplicates, api.TriageDuplicate{Items: []api.TriageItem{ref(open[m.A]), ref(open[m.B])}, TitleSimilarity: m.Title, CriteriaSimilarity: m.Criteria})
	}
	index, err := loadReviewFollowUpIndex(ctx, s.db, task)
	if err != nil {
		return zero, err
	}
	for _, pair := range index.duplicates(open) {
		if listed[pair] {
			continue
		}
		listed[pair] = true
		a, b := open[pair[0]], open[pair[1]]
		out.Duplicates = append(out.Duplicates, api.TriageDuplicate{Items: []api.TriageItem{ref(a), ref(b)}, TitleSimilarity: triage.Similarity(a.Title, b.Title), CriteriaSimilarity: triage.CriteriaSimilarity(a.Description, b.Description)})
	}
	for _, m := range triage.Delivered(openP, doneP) {
		match := ref(done[m.B])
		out.AlreadyReleased = append(out.AlreadyReleased, api.TriageAlreadyDone{Item: ref(open[m.A]), Done: &match, TitleSimilarity: m.Title, CriteriaSimilarity: m.Criteria})
	}
	for _, item := range open {
		for _, fixed := range index.verifiedFixed(item.ID) {
			out.AlreadyReleased = append(out.AlreadyReleased, api.TriageAlreadyDone{Item: ref(item), Release: fixed, NamedInRelease: true})
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

// reviewFollowUpRecord is one finding a review-convergence ledger filed as a
// follow-up: a held stub the hub created, or a fold into an existing item.
// Stage is the review stage the finding's RESULT belongs to.
type reviewFollowUpRecord struct {
	parent     string
	messageSeq int64
	findingID  string
	itemID     string
	folded     bool
	stage      int
}

// reviewFixingResult is a saved RESULT that recorded findings of its item's
// review fixed on candidate: a round-two review's blockerIds, or a passing
// focused verification's blocker IDs.
type reviewFixingResult struct {
	how       string
	candidate string
	resultSeq int64
	stage     int
	ids       []string
}

// reviewFollowUpIndex is every follow-up the project's review ledgers saved,
// and the results that recorded findings fixed, by reviewed item.
type reviewFollowUpIndex struct {
	followUps []reviewFollowUpRecord
	fixes     map[string][]reviewFixingResult
}

// reviewStageOf is the index of the review stage seq falls in: the number of
// later stages whose opening ASSIGN is at or before seq, as CurrentStage splits
// rounds.
func reviewStageOf(stages []api.ReviewStage, seq int64) int {
	n := 0
	for i := 1; i < len(stages); i++ {
		if stages[i].AssignmentSeq <= seq {
			n++
		}
	}
	return n
}

// loadReviewFollowUpIndex reads every review ledger of task once. Round-two
// blockerIds are read from that RESULT's stored envelope; a RESULT stored
// without review metadata names nothing fixed.
func loadReviewFollowUpIndex(ctx context.Context, q queryRower, task string) (reviewFollowUpIndex, error) {
	out := reviewFollowUpIndex{fixes: map[string][]reviewFixingResult{}}
	rows, err := q.QueryContext(ctx, `SELECT item_id,state_json FROM review_convergence WHERE task_id=? ORDER BY item_id`, task)
	if err != nil {
		return out, err
	}
	type roundTwo struct {
		parent string
		fix    reviewFixingResult
	}
	var pending []roundTwo
	for rows.Next() {
		var parent, raw string
		if err = rows.Scan(&parent, &raw); err != nil {
			rows.Close()
			return out, err
		}
		var state api.ReviewConvergence
		if err = json.Unmarshal([]byte(raw), &state); err != nil {
			rows.Close()
			return out, err
		}
		for _, f := range state.FollowUps {
			out.followUps = append(out.followUps, reviewFollowUpRecord{parent: parent, messageSeq: f.MessageSeq, findingID: f.Finding.ID, itemID: f.ItemID, folded: f.Folded, stage: reviewStageOf(state.Stages, f.MessageSeq)})
		}
		for _, r := range state.Rounds {
			if r.Number == 2 && r.ResultSeq != 0 {
				pending = append(pending, roundTwo{parent, reviewFixingResult{how: "round-two review", candidate: r.Candidate, resultSeq: r.ResultSeq, stage: reviewStageOf(state.Stages, r.RequestSeq)}})
			}
		}
		for _, f := range state.Focused {
			if f.Passed && f.ResultSeq != 0 && len(f.BlockerIDs) > 0 {
				out.fixes[parent] = append(out.fixes[parent], reviewFixingResult{how: "focused verification", candidate: f.Candidate, resultSeq: f.ResultSeq, stage: reviewStageOf(state.Stages, f.RequestSeq), ids: f.BlockerIDs})
			}
		}
	}
	if err = closeRows(rows); err != nil {
		return out, err
	}
	if len(pending) > 0 {
		args := []any{task}
		for _, p := range pending {
			args = append(args, p.fix.resultSeq)
		}
		envelopes := map[int64]string{}
		rows, err = q.QueryContext(ctx, `SELECT seq,envelope FROM messages WHERE task_id=? AND `+inList("seq", len(pending)), args...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var seq int64
			var raw string
			if err = rows.Scan(&seq, &raw); err != nil {
				rows.Close()
				return out, err
			}
			envelopes[seq] = raw
		}
		if err = closeRows(rows); err != nil {
			return out, err
		}
		for _, p := range pending {
			var e api.Envelope
			if json.Unmarshal([]byte(envelopes[p.fix.resultSeq]), &e) != nil || e.Review == nil || len(e.Review.BlockerIDs) == 0 {
				continue
			}
			p.fix.ids = e.Review.BlockerIDs
			out.fixes[p.parent] = append(out.fixes[p.parent], p.fix)
		}
	}
	for parent := range out.fixes {
		fixes := out.fixes[parent]
		sort.Slice(fixes, func(i, j int) bool { return fixes[i].resultSeq < fixes[j].resultSeq })
	}
	return out, nil
}

// citedMessage finds "#N" written as a whole token.
var citedMessage = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])#([0-9]+)`)

// duplicates returns each pair of open items, as indexes i<j, that share a
// finding identity: a review message and a finding ID. An item holds the
// identity of each follow-up record that filed it, and of each recorded
// finding whose message is its source message or one its text cites as #N
// and whose ID its text names as a whole word. An unchanged held stub holds
// only its record (its text is the finding the hub wrote), and the reviewed
// item never holds its own review's findings.
func (x reviewFollowUpIndex) duplicates(open []api.WorkItem) [][2]int {
	type identity struct {
		seq int64
		id  string
	}
	bySeq := map[int64][]reviewFollowUpRecord{}
	byItem := map[string][]reviewFollowUpRecord{}
	for _, f := range x.followUps {
		bySeq[f.messageSeq] = append(bySeq[f.messageSeq], f)
		if !f.folded {
			byItem[f.itemID] = append(byItem[f.itemID], f)
		}
	}
	holders := map[identity][]int{}
	for i, item := range open {
		held := map[identity]bool{}
		for _, f := range byItem[item.ID] {
			held[identity{f.messageSeq, f.findingID}] = true
		}
		if !heldReviewFollowUp(item) {
			text := item.Title + "\n" + item.Description
			seqs := map[int64]bool{}
			if item.SourceMessageSeq > 0 {
				seqs[item.SourceMessageSeq] = true
			}
			for _, m := range citedMessage.FindAllStringSubmatch(text, -1) {
				if seq, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					seqs[seq] = true
				}
			}
			for seq := range seqs {
				for _, f := range bySeq[seq] {
					if f.parent != item.ID && f.findingID != "" && namesItem(text, f.findingID) {
						held[identity{f.messageSeq, f.findingID}] = true
					}
				}
			}
		}
		for id := range held {
			holders[id] = append(holders[id], i)
		}
	}
	keys := make([]identity, 0, len(holders))
	for id := range holders {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].seq != keys[j].seq {
			return keys[i].seq < keys[j].seq
		}
		return keys[i].id < keys[j].id
	})
	seen := map[[2]int]bool{}
	var out [][2]int
	for _, id := range keys {
		items := holders[id]
		sort.Ints(items)
		for a := 0; a < len(items); a++ {
			for b := a + 1; b < len(items); b++ {
				pair := [2]int{items[a], items[b]}
				if !seen[pair] {
					seen[pair] = true
					out = append(out, pair)
				}
			}
		}
	}
	return out
}

// verifiedFixed describes, for each follow-up record that filed item, the
// first later RESULT of the same review stage that recorded its finding fixed.
// It reads saved review records only, so it lists the item whatever its queue,
// order or run state is.
func (x reviewFollowUpIndex) verifiedFixed(item string) []string {
	var out []string
	for _, f := range x.followUps {
		if f.folded || f.itemID != item {
			continue
		}
		for _, fix := range x.fixes[f.parent] {
			if fix.stage != f.stage || fix.resultSeq <= f.messageSeq || !slices.Contains(fix.ids, f.findingID) {
				continue
			}
			out = append(out, fmt.Sprintf("verified fixed in %s at %s by #%d (finding %s of #%d)", f.parent, fix.candidate, fix.resultSeq, f.findingID, f.messageSeq))
			break
		}
	}
	return out
}
