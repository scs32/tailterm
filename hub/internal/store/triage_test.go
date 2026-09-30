package store

import (
	"context"
	"errors"
	"path/filepath"
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
