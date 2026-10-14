package store

import (
	"context"
	"encoding/json"
	"errors"
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
