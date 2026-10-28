package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

type triageFixture struct {
	s       *Store
	task    api.Task
	by      api.Caller
	handler api.Agent
	now     time.Time
	items   map[string]api.WorkItem
}

// newTriageFixture files two near-duplicate open items, an open item that
// matches a done item, an item idle for ten days, and an idle item with a
// queued entry.
func newTriageFixture(t *testing.T) *triageFixture {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "triage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &triageFixture{s: s, by: api.Caller{Node: "fixture", User: "owner"}, items: map[string]api.WorkItem{}}
	if f.task, err = s.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Triage fixture"}, f.by); err != nil {
		t.Fatal(err)
	}
	task := f.task
	if f.handler, err = s.AddAgent(context.Background(), task.ID, api.AddAgentRequest{Name: "database", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: "database"}, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(s.now()), f.handler.ID); err != nil {
		t.Fatal(err)
	}
	f.now = s.now()
	s.SetClockForTest(func() time.Time { return f.now })
	ctx := context.Background()
	file := func(key, title, description string) api.WorkItem {
		item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: title, Description: description, Priority: "normal", RequestID: "triage-" + key}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		f.items[key] = item
		return item
	}
	file("idle", "Remove the unused legacy export button", "")
	queued := file("queued-idle", "Rotate the discord bridge credential", "")
	f.now = f.now.Add(10 * 24 * time.Hour)
	file("dup-a", "Board scroll jumps after a new message", "- c1: keep the scroll position when a message arrives")
	file("dup-b", "Board scroll jumps after new messages arrive", "")
	shipped := file("shipped", "Relay status keeps showing cleared errors", "")
	file("open-again", "Relay status keeps showing cleared errors", "")
	file("other", "Add dark mode to the usage view", "")
	done := "done"
	if _, _, err := s.CreateWorkItemUpdate(ctx, task.ID, shipped.ID, api.CreateWorkItemUpdate{ExpectedRevision: shipped.Revision, Status: &done, RequestID: "triage-done"}, f.by); err != nil {
		t.Fatal(err)
	}
	order := contextLinkedMessage(t, s, task, queued, "queued order", "triage-queued-order", nil)
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "triage-queue", Operation: "add", ItemID: queued.ID, OrderMessageSeq: order.Seq, Host: "mini", Cwd: "/queued"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *triageFixture) counts(t *testing.T) [4]int {
	t.Helper()
	var n [4]int
	for i, table := range []string{"work_items", "work_item_revisions", "messages", "team_queue_entries"} {
		if err := f.s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n[i]); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// a9 (c4): triage lists exactly the likely duplicates, the item a done item
// already delivered and the item idle past the stale period, not an idle item
// with a queued entry; it changes nothing; a non-handler agent is refused.
func TestWorkItemTriageSuggestsAndChangesNothing(t *testing.T) {
	f := newTriageFixture(t)
	ctx := context.Background()
	before := f.counts(t)
	got, err := f.s.WorkItemTriage(ctx, f.task.ID, 7, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Duplicates) != 1 || got.Duplicates[0].Items[0].ID != f.items["dup-a"].ID || got.Duplicates[0].Items[1].ID != f.items["dup-b"].ID || got.Duplicates[0].TitleSimilarity < 0.6 {
		t.Fatalf("duplicates %+v", got.Duplicates)
	}
	if len(got.AlreadyReleased) != 1 || got.AlreadyReleased[0].Item.ID != f.items["open-again"].ID || got.AlreadyReleased[0].Done == nil || got.AlreadyReleased[0].Done.ID != f.items["shipped"].ID {
		t.Fatalf("already released %+v", got.AlreadyReleased)
	}
	if len(got.Stale) != 1 || got.Stale[0].Item.ID != f.items["idle"].ID || got.Stale[0].IdleDays != 10 {
		t.Fatalf("stale %+v", got.Stale)
	}
	if shorter, err := f.s.WorkItemTriage(ctx, f.task.ID, 11, "", ""); err != nil || len(shorter.Stale) != 0 {
		t.Fatalf("11-day window %+v %v", shorter.Stale, err)
	}
	if after := f.counts(t); after != before {
		t.Fatalf("triage changed records: %v -> %v", before, after)
	}
	if _, err := f.s.WorkItemTriage(ctx, f.task.ID, 7, f.handler.ID, f.handler.RunID); err != nil {
		t.Fatalf("handler triage: %v", err)
	}
	worker, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "worker", AgentID: api.NewID("agt"), Host: "mini", Session: "worker"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.WorkItemTriage(ctx, f.task.ID, 7, worker.ID, worker.RunID); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "only the owner or a database handler") {
		t.Fatalf("worker triage: %v", err)
	}
}

// triageRows counts every table triage must leave alone, narrative tables
// included.
func triageRows(t *testing.T, s *Store) [7]int {
	t.Helper()
	var n [7]int
	for i, table := range []string{"work_items", "work_item_revisions", "messages", "team_queue_entries", "narrative_artifacts", "narrative_artifact_versions", "narrative_timeline"} {
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n[i]); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

func triageIDs(items []api.TriageItem) []string {
	out := []string{}
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}

// a4: each held follow-up carries its finding's file and exactly the open,
// held, unordered items that name that file; one with no file has none.
func TestTriageHeldListsFileAndFoldCandidates(t *testing.T) {
	f := newConvergenceFixture(t)
	targets := foldTargets(t, f.s, f.task)
	r := f.review(t, candidateA)
	findings := []api.ReviewFinding{
		{ID: "f1", Title: "Held finding with a file", File: foldFile, Line: 3},
		{ID: "f2", Title: "Held finding with no file", Command: "go test ./x", Output: "FAIL"},
	}
	if _, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general", Findings: findings}), "", r.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	st := f.state(t)
	before := triageRows(t, f.s)
	got, err := f.s.WorkItemTriage(f.ctx, f.task.ID, 7, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if after := triageRows(t, f.s); after != before {
		t.Fatalf("triage changed records: %v -> %v", before, after)
	}
	held := map[string]api.TriageHeld{}
	for _, h := range got.HeldForTriage {
		held[h.Item.ID] = h
	}
	withFile, noFile := held[st.FollowUps[0].ItemID], held[st.FollowUps[1].ItemID]
	if len(held) != 2 || withFile.File != foldFile || noFile.File != "" {
		t.Fatalf("held %+v", got.HeldForTriage)
	}
	if want := []string{targets["eligible"].ID, targets["second"].ID}; !reflect.DeepEqual(triageIDs(withFile.Candidates), want) {
		t.Fatalf("candidates %+v, want eligible and second only", withFile.Candidates)
	}
	if noFile.Candidates == nil || len(noFile.Candidates) != 0 {
		t.Fatalf("no-file candidates %#v", noFile.Candidates)
	}
}

// a5: reviewFolds lists every fold in the window whatever its target's status
// is now, and drops a fold older than staleDays.
func TestTriageFoldListsEveryFoldInWindow(t *testing.T) {
	f := newConvergenceFixture(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f.s.SetClockForTest(func() time.Time { return now })
	targets := foldTargets(t, f.s, f.task)
	empty, err := f.s.WorkItemTriage(f.ctx, f.task.ID, 7, "", "")
	if err != nil || empty.ReviewFolds == nil || len(empty.ReviewFolds) != 0 {
		t.Fatalf("no folds: %#v %v", empty.ReviewFolds, err)
	}
	r := f.review(t, candidateA)
	findings := []api.ReviewFinding{
		{ID: "f1", Title: "First folded finding", File: foldFile, Line: 3, Into: targets["eligible"].ID},
		{ID: "f2", Title: "Second folded finding", File: foldFile, Line: 4, Into: targets["second"].ID},
	}
	m, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general", Findings: findings}), "", r.Seq, f.reviewer)
	if err != nil {
		t.Fatal(err)
	}
	foldedAt := ts(now)
	now = now.Add(time.Hour)
	for key, status := range map[string]string{"eligible": "done", "second": "dismissed"} {
		item, err := f.s.GetWorkItem(f.ctx, f.task.ID, targets[key].ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, Status: ptr(status), RequestID: "close-" + key}, f.by); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.s.WorkItemTriage(f.ctx, f.task.ID, 7, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []api.TriageFold{
		{Item: api.TriageItem{ID: targets["eligible"].ID, Title: targets["eligible"].Title, Status: "done"}, ReviewParent: f.item.ID, MessageSeq: m.Seq, FindingID: "f1", Title: "First folded finding", FoldedAt: foldedAt},
		{Item: api.TriageItem{ID: targets["second"].ID, Title: targets["second"].Title, Status: "dismissed"}, ReviewParent: f.item.ID, MessageSeq: m.Seq, FindingID: "f2", Title: "Second folded finding", FoldedAt: foldedAt},
	}
	if !reflect.DeepEqual(got.ReviewFolds, want) {
		t.Fatalf("folds %+v, want %+v", got.ReviewFolds, want)
	}
	now = now.Add(8 * 24 * time.Hour)
	if old, err := f.s.WorkItemTriage(f.ctx, f.task.ID, 7, "", ""); err != nil || old.ReviewFolds == nil || len(old.ReviewFolds) != 0 {
		t.Fatalf("fold older than staleDays listed: %+v %v", old.ReviewFolds, err)
	}
	if wider, err := f.s.WorkItemTriage(f.ctx, f.task.ID, 9, "", ""); err != nil || !reflect.DeepEqual(wider.ReviewFolds, want) {
		t.Fatalf("wider window %+v %v", wider.ReviewFolds, err)
	}
}

// a12 (iv): the triage JSON carries candidates and reviewFolds as arrays even
// when empty, omits an empty file, and names each fold field.
func TestTriageHeldJSONShape(t *testing.T) {
	shape := func(v api.WorkItemTriage) map[string]any {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err = json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	empty := shape(api.WorkItemTriage{HeldForTriage: []api.TriageHeld{{Candidates: []api.TriageItem{}}}, ReviewFolds: []api.TriageFold{}})
	held := empty["heldForTriage"].([]any)[0].(map[string]any)
	if c, ok := held["candidates"].([]any); !ok || len(c) != 0 {
		t.Fatalf("empty candidates %#v", held)
	}
	if _, ok := held["file"]; ok {
		t.Fatalf("empty file present %#v", held)
	}
	if folds, ok := empty["reviewFolds"].([]any); !ok || len(folds) != 0 {
		t.Fatalf("empty reviewFolds %#v", empty)
	}
	full := shape(api.WorkItemTriage{
		HeldForTriage: []api.TriageHeld{{File: foldFile, Candidates: []api.TriageItem{{ID: "wi_aaaaaaaaaaaaaaaa"}}}},
		ReviewFolds:   []api.TriageFold{{Item: api.TriageItem{ID: "wi_aaaaaaaaaaaaaaaa"}, ReviewParent: "wi_bbbbbbbbbbbbbbbb", MessageSeq: 7, FindingID: "f1", Title: "t", FoldedAt: "now"}},
	})
	held = full["heldForTriage"].([]any)[0].(map[string]any)
	if held["file"] != foldFile || len(held["candidates"].([]any)) != 1 {
		t.Fatalf("held %#v", held)
	}
	fold := full["reviewFolds"].([]any)[0].(map[string]any)
	keys := []string{}
	for k := range fold {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"findingId", "foldedAt", "item", "messageSeq", "reviewParent", "title"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("fold keys %v, want %v", keys, want)
	}
}

// duplicatePairs lists triage's duplicate pairs as "A B" by item ID, and how
// many times each was listed.
func duplicatePairs(t *testing.T, f *convergenceFixture) map[string]int {
	t.Helper()
	out, err := f.s.WorkItemTriage(f.ctx, f.task.ID, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	pairs := map[string]int{}
	for _, d := range out.Duplicates {
		ids := []string{d.Items[0].ID, d.Items[1].ID}
		sort.Strings(ids)
		pairs[ids[0]+" "+ids[1]]++
	}
	return pairs
}

func pairKey(a, b api.WorkItem) string {
	ids := []string{a.ID, b.ID}
	sort.Strings(ids)
	return ids[0] + " " + ids[1]
}

// a1, a2 (d2, d3): two open items that share a review message and finding ID
// are listed as duplicates whatever their titles, once each, with or without
// the stub; f1 never matches f10, the same ID from another message does not
// match, and a dismissed copy is not listed.
func TestTriageDuplicateSameFinding(t *testing.T) {
	f := newConvergenceFixture(t)
	r1 := f.review(t, candidateA)
	findings := []api.ReviewFinding{{ID: "f1", Title: "Explain the fixture retry", Kind: "feature", File: "fixture.go", Line: 3}, {ID: "f10", Title: "Rename the receipt column", Kind: "feature", File: "receipt.go", Line: 9}}
	m, err := f.post(f.resultEnv(candidateA, passConvergence, api.ReviewMetadata{Mode: "general", Findings: findings}), "", r1.Seq, f.reviewer)
	if err != nil {
		t.Fatal(err)
	}
	stubs := map[string]api.WorkItem{}
	for _, follow := range f.state(t).FollowUps {
		stubs[follow.Finding.ID], _ = f.s.GetWorkItem(f.ctx, f.task.ID, follow.ItemID)
	}
	other := contextLinkedMessage(t, f.s, f.task, f.item, "unrelated board message", "triage-other-message", nil)
	file := func(key, title, description string, source int64) api.WorkItem {
		t.Helper()
		item, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: title, Description: description, SourceMessageSeq: source, RequestID: "triage-finding-" + key}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	review := fmt.Sprintf("#%d", m.Seq)
	cited := file("cited", "Document how retries resume", "Refined from review "+review+" finding f1.", 0)
	sourced := file("sourced", "Write retry notes for operators", "Covers f1 of that review.", m.Seq)
	similar := file("similar", "Explain the fixture retry", "Refined copy of "+review+" f1.", 0)
	otherMessage := file("other-message", "Speed up the board scroll", fmt.Sprintf("Finding f1 of #%d.", other.Seq), 0)
	tenOnly := file("ten", "Rename the receipt table column", "Covers "+review+" f10.", 0)
	dismissed := file("dismissed", "Retry wording for operators", "Also "+review+" f1.", 0)
	if _, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, dismissed.ID, api.UpdateWorkItemRequest{Revision: dismissed.Revision, Status: ptr("dismissed")}, f.by); err != nil {
		t.Fatal(err)
	}
	pairs := duplicatePairs(t, f)
	for _, want := range [][2]api.WorkItem{{stubs["f1"], cited}, {stubs["f1"], sourced}, {stubs["f1"], similar}, {cited, sourced}, {cited, similar}, {sourced, similar}, {stubs["f10"], tenOnly}} {
		if pairs[pairKey(want[0], want[1])] != 1 {
			t.Fatalf("pair %s %s listed %d times: %v", want[0].Title, want[1].Title, pairs[pairKey(want[0], want[1])], pairs)
		}
	}
	for _, item := range []api.WorkItem{otherMessage, dismissed} {
		for pair := range pairs {
			if strings.Contains(pair, item.ID) {
				t.Fatalf("%s listed: %s", item.Title, pair)
			}
		}
	}
	for _, unwanted := range [][2]api.WorkItem{{stubs["f1"], tenOnly}, {stubs["f10"], cited}, {stubs["f1"], stubs["f10"]}, {f.item, stubs["f1"]}} {
		if pairs[pairKey(unwanted[0], unwanted[1])] != 0 {
			t.Fatalf("pair %s %s listed: %v", unwanted[0].Title, unwanted[1].Title, pairs)
		}
	}
	// With the stub dismissed, two refined copies still match by finding.
	if _, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, stubs["f1"].ID, api.UpdateWorkItemRequest{Revision: 1, Status: ptr("dismissed")}, f.by); err != nil {
		t.Fatal(err)
	}
	pairs = duplicatePairs(t, f)
	if pairs[pairKey(cited, sourced)] != 1 || pairs[pairKey(cited, similar)] != 1 {
		t.Fatalf("refined copies without the stub: %v", pairs)
	}
	// The reviewed item never holds its own review's findings.
	f.rescope(t, "Retry fix; review "+review+" filed f1 as a follow-up.")
	if pairs = duplicatePairs(t, f); pairs[pairKey(f.item, cited)] != 0 {
		t.Fatalf("reviewed item listed with its follow-up: %v", pairs)
	}
}

// a6 (d4, d6): every open follow-up a saved round-two or focused RESULT
// recorded fixed is listed under alreadyReleased with its parent, candidate
// and fixing RESULT, even when it is queued or ordered; a closed stub and a
// legacy RESULT with no review metadata are not.
func TestTriageFollowUpVerifiedFixedListed(t *testing.T) {
	f := newConvergenceFixture(t)
	r1, r2, stubs := roundOneFollowUps(t, f)
	refine := func(id, text string) api.WorkItem {
		t.Helper()
		item, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, stubs[id].ID, api.UpdateWorkItemRequest{Revision: 1, Description: &text}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	queued := refine("f1", "Refined: explain the fixture retry.")
	order := contextLinkedMessage(t, f.s, f.task, queued, "listed follow-up order", "listed-order", nil)
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: "listed-queue", Operation: "add", ItemID: queued.ID, OrderMessageSeq: order.Seq, Host: "mini", Cwd: "/listed"}); err != nil {
		t.Fatal(err)
	}
	ordered := refine("f2", "Refined: keep the receipt on fixture retries.")
	contextLinkedMessage(t, f.s, f.task, ordered, "ordered follow-up", "listed-ordered", nil)
	if err := f.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1", "f1", "f2"}}, r2.Seq); err != nil {
		t.Fatal(err)
	}
	result := latestResultSeq(t, f)
	// A legacy ledger whose round-two RESULT has no review metadata names
	// nothing fixed.
	legacyParent, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Legacy reviewed item", RequestID: "legacy-parent"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	legacyStub, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Legacy follow-up", RequestID: "legacy-stub"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	plain := contextLinkedMessage(t, f.s, f.task, legacyParent, "legacy round two result", "legacy-result", nil)
	legacy := api.ReviewConvergence{ItemID: legacyParent.ID, History: "recorded", Rounds: []api.ReviewRound{{Number: 1, RequestSeq: 1, ResultSeq: 2, Candidate: candidateA}, {Number: 2, RequestSeq: 3, ResultSeq: plain.Seq, Candidate: candidateB}}, FollowUps: []api.ReviewFollowUp{{Finding: api.ReviewFinding{ID: "f1", Title: "Legacy follow-up"}, ItemID: legacyStub.ID, MessageSeq: 2}}}
	raw, _ := json.Marshal(legacy)
	if _, err = f.s.db.Exec(`INSERT INTO review_convergence(task_id,item_id,state_json) VALUES(?,?,?)`, f.task.ID, legacyParent.ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	out, err := f.s.WorkItemTriage(f.ctx, f.task.ID, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]api.TriageAlreadyDone{}
	for _, r := range out.AlreadyReleased {
		if strings.HasPrefix(r.Release, "verified fixed ") {
			if _, dup := listed[r.Item.ID]; dup {
				t.Fatalf("listed twice: %+v", r)
			}
			listed[r.Item.ID] = r
		}
	}
	for id, item := range map[string]api.WorkItem{"f1": queued, "f2": ordered} {
		want := fmt.Sprintf("verified fixed in %s at %s by #%d (finding %s of #%d)", f.item.ID, candidateB, result, id, r1.Seq)
		if r, ok := listed[item.ID]; !ok || r.Release != want || !r.NamedInRelease || r.Done != nil {
			t.Fatalf("%s: got %+v want %q", id, r, want)
		}
	}
	if len(listed) != 2 {
		t.Fatalf("listed %+v", listed)
	}
	// A closed stub is not open, so it is not listed.
	f2 := newConvergenceFixture(t)
	_, r2b, stubsB := roundOneFollowUps(t, f2)
	if err := f2.roundTwoResult(passConvergence, api.ReviewMetadata{BlockerIDs: []string{"b1", "f1"}}, r2b.Seq); err != nil {
		t.Fatal(err)
	}
	out, err = f2.s.WorkItemTriage(f2.ctx, f2.task.ID, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.AlreadyReleased {
		if r.Item.ID == stubsB["f1"].ID || r.Item.ID == stubsB["f2"].ID {
			t.Fatalf("closed or unfixed stub listed: %+v", r)
		}
	}
}
