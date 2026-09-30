package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// a9 (c4): tt work-items triage prints suggestions only, adds release-record
// matches by item ID and by title, changes nothing, and refuses an agent
// session that is not a database handler.
func TestWorkItemTriageCLI(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	now := time.Now().UTC()
	f.st.SetClockForTest(func() time.Time { return now })
	file := func(key, title string) api.WorkItem {
		item, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: title, RequestID: "cli-triage-" + key})
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	idle := file("idle", "Remove the unused legacy export button")
	now = now.Add(10 * 24 * time.Hour)
	a := file("dup-a", "Board scroll jumps after a new message")
	file("dup-b", "Board scroll jumps after new messages arrive")
	named := file("named", "Rename the quota dashboard")
	described := file("described", "Relay status clears errors after a successful pass")
	record := filepath.Join(t.TempDir(), "README.md")
	if err := os.WriteFile(record, []byte("# Release\n\n| Tasks-hub | Item | What it does |\n|---|---|---|\n| `abc1234` | Quota (`"+named.ID+"`) | New dashboard name |\n| `def5678` | Relay status | `tt relay --status` clears errors after a successful pass |\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := f.c.ListWorkItems(ctx, f.task.ID, "", "", 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(f.e, []string{"triage", "--project", f.task.ID, "--stale-days", "7", "--release-record", record, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var got api.WorkItemTriage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if len(got.Duplicates) != 1 || got.Duplicates[0].Items[0].ID != a.ID {
		t.Fatalf("duplicates %+v", got.Duplicates)
	}
	matched := map[string]api.TriageAlreadyDone{}
	for _, r := range got.AlreadyReleased {
		matched[r.Item.ID] = r
	}
	if len(got.AlreadyReleased) != 2 || !matched[named.ID].NamedInRelease || matched[described.ID].TitleSimilarity < 0.6 || !strings.Contains(matched[described.ID].Release, "def5678") {
		t.Fatalf("release matches %+v", got.AlreadyReleased)
	}
	stale := false
	for _, s := range got.Stale {
		stale = stale || s.Item.ID == idle.ID
	}
	if !stale {
		t.Fatalf("stale %+v", got.Stale)
	}
	text, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(f.e, []string{"triage", "--project", f.task.ID, "--release-record", record})
	})
	if err != nil || !strings.HasPrefix(text, "Suggestions only — nothing was changed; confirm with the owner.") || !strings.Contains(text, named.ID+" \"Rename the quota dashboard\" is named in release row") {
		t.Fatalf("text %q %v", text, err)
	}
	after, err := f.c.ListWorkItems(ctx, f.task.ID, "", "", 0, 200)
	if err != nil || len(after.Items) != len(before.Items) {
		t.Fatalf("items %d -> %d %v", len(before.Items), len(after.Items), err)
	}
	for i := range before.Items {
		if before.Items[i].Revision != after.Items[i].Revision || before.Items[i].Status != after.Items[i].Status {
			t.Fatalf("triage changed %s", before.Items[i].ID)
		}
	}
	handlerEnv := f.e
	handlerEnv.agent, handlerEnv.runID = f.handler.ID, f.handler.RunID
	if _, err := captureCLIOutput(t, func() error { return cmdWorkItems(handlerEnv, []string{"triage"}) }); err != nil {
		t.Fatalf("handler triage: %v", err)
	}
	worker, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "worker", Host: "fixture", Session: "worker", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	workerEnv := f.e
	workerEnv.agent, workerEnv.runID = worker.ID, worker.RunID
	if _, err := captureCLIOutput(t, func() error { return cmdWorkItems(workerEnv, []string{"triage"}) }); err == nil || !strings.Contains(err.Error(), "only the owner or a database handler") {
		t.Fatalf("worker triage: %v", err)
	}
}
