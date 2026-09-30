package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/testverification"
)

// choresGit runs git in a fixture repository and returns trimmed output.
func choresGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", root, "-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture"}, args...)
	output, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// choresCommit writes files and commits them, returning the new HEAD.
func choresCommit(t *testing.T, root, message string, files ...string) string {
	t.Helper()
	for _, name := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(message+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	choresGit(t, root, "add", "-A")
	choresGit(t, root, "commit", "-q", "-m", message)
	return choresGit(t, root, "rev-parse", "HEAD")
}

// choresParallelProject gives the fixture a fresh host policy and census for
// host fixture, a second online handler and the given limit.
func choresParallelProject(t *testing.T, f teamFixture, limit int) api.Agent {
	t.Helper()
	ctx := context.Background()
	second, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler-2", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "db-handler-2", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: second.ID, RunID: second.RunID, Kind: api.EventRunning}); err != nil {
		t.Fatal(err)
	}
	observeRunnerHost(t, f, "fixture", time.Now().UnixNano())
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "set_limit", Host: "fixture", ConcurrencyLimit: limit}); err != nil {
		t.Fatal(err)
	}
	return second
}

// choresAccept verifies, completes and handler-accepts a running entry's
// exact candidate through the hub's public paths.
func choresAccept(t *testing.T, f teamFixture, q api.TeamQueueEntry, worktree, branch, base, commit string) api.TeamQueueEntry {
	t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	current, err := f.c.GetWorkItem(ctx, f.task.ID, q.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if err := testverification.Prepare(f.c, f.task.ID, current, commit, q.Repository, base); err != nil {
		t.Fatal(err)
	}
	report, _, err := f.st.PutNarrativeReport(ctx, f.task.ID, current.ID, api.PutNarrativeReportRequest{RequestID: "chores-report-" + q.ID, ScopeRevision: current.ScopeRevision,
		Sections:   api.NarrativeReportSections{RequestedOutcome: "Deliver.", DeliveredWork: "Synthetic delivery.", Verification: "Fixture.", Limitations: "Fixture only.", RemainingWork: "None."},
		References: []api.NarrativeReference{{Kind: "work-item-revision", TaskID: f.task.ID, ItemID: current.ID, Revision: current.Revision, Label: "bounded scope"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	done := "done"
	if _, _, err := f.st.CreateWorkItemUpdate(ctx, f.task.ID, current.ID, api.CreateWorkItemUpdate{ExpectedRevision: current.Revision, RequestID: "chores-done-" + q.ID, Status: &done, CompletionReport: &api.NarrativeReportPin{ReportID: report.ReportID, Version: report.Version, Digest: report.Digest, ScopeRevision: report.ScopeRevision}}, by); err != nil {
		t.Fatal(err)
	}
	current, err = f.c.GetWorkItem(ctx, f.task.ID, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "chores-accept-" + q.ID, Operation: "accept", EntryID: q.ID, ExpectedRevision: q.Revision, HandlerAgentID: q.HandlerID, HandlerRunID: q.HandlerRunID, Acceptance: &api.TeamIntegrationAcceptance{Repository: q.Repository, BaseCommit: base, Worktree: worktree, Branch: branch, Commit: commit, ItemRevision: current.Revision, CompletionReport: current.CompletionReport, Evidence: "fixture acceptance"}})
	if err != nil {
		t.Fatal(err)
	}
	return accepted
}

// a3 (c2): once an entry's acceptance is saved, the runner narrows its
// ownership once to the merge-base changed files under that ownership, so an
// entry that overlapped only unchanged files can claim before the team
// closes. A Git error changes nothing and never fails the entry.
func TestTeamRunnerNarrowsAcceptedEntryToChangedFiles(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	root, base := queueGitRepo(t)
	start := choresGit(t, root, "symbolic-ref", "--short", "HEAD")
	choresGit(t, root, "checkout", "-q", "-b", "feature")
	commit := choresCommit(t, root, "feature work", "src/a/one.go", "docs/unowned.md")
	choresGit(t, root, "checkout", "-q", start)
	// tasks-hub moves on, touching an owned but unchanged file. The later
	// base is the acceptance base; the merge-base diff must not charge the
	// branch with it.
	later := choresCommit(t, root, "later base", "src/a/two.go")
	repository := filepath.Join(root, ".git")
	choresParallelProject(t, f, 0)
	a, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "narrow-add-a", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: root, Repository: repository, BaseCommit: base, Ownership: []string{"src/a", "src/c"}})
	if err != nil {
		t.Fatal(err)
	}
	a = runQueueEntry(t, f, a.ID)
	other, otherOrder := queueFixtureItem(t, f, "narrow-other")
	b, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "narrow-add-b", Operation: "add", ItemID: other.ID, OrderMessageSeq: otherOrder, Host: "fixture", Cwd: t.TempDir(), Repository: repository, BaseCommit: base, Ownership: []string{"src/a/two.go"}})
	if err != nil {
		t.Fatal(err)
	}
	listed := func(id string) api.TeamQueueEntry {
		t.Helper()
		list, err := f.c.ListTeamQueue(ctx, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range list.Entries {
			if q.ID == id {
				return q
			}
		}
		t.Fatalf("entry %s not listed", id)
		return api.TeamQueueEntry{}
	}
	if got := listed(b.ID); len(got.BlockedBy) != 1 || got.BlockedBy[0] != a.ID {
		t.Fatalf("B not blocked by A before narrowing: %+v", got.BlockedBy)
	}
	time.Sleep(2 * time.Second) // isolated test hub's write bucket refills
	a = choresAccept(t, f, a, root, "feature", later, commit)
	runner := teamRunner{}

	// A Git error leaves the entry as it was: no write, still running.
	broken := a
	broken.Acceptance = &api.TeamIntegrationAcceptance{BaseCommit: later, Commit: strings.Repeat("e", 40)}
	if got := runner.narrow(ctx, f.c, broken); got.Revision != a.Revision {
		t.Fatalf("git error changed the entry: %+v", got)
	}
	if got, _ := f.c.GetTeamQueueEntry(ctx, f.task.ID, a.ID); got.State != "running" || got.Revision != a.Revision || strings.Join(got.Ownership, ",") != "src/a,src/c" {
		t.Fatalf("git error entry %+v", got)
	}

	narrowed := runner.narrow(ctx, f.c, a)
	if strings.Join(narrowed.Ownership, ",") != "src/a/one.go" || narrowed.Revision != a.Revision+1 || narrowed.State != "running" {
		t.Fatalf("narrowed entry %+v", narrowed)
	}
	if again := runner.narrow(ctx, f.c, narrowed); again.Revision != narrowed.Revision {
		t.Fatalf("second narrowing wrote again: %+v", again)
	}
	if got := listed(b.ID); len(got.BlockedBy) != 0 {
		t.Fatalf("B still blocked after narrowing: %+v", got.BlockedBy)
	}
	detail, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "narrow-claim-b", Operation: "claim", EntryID: b.ID, ExpectedRevision: b.Revision, Host: "fixture", PauseGeneration: detail.Task.PauseGeneration}); err != nil || claimed.State != "launching" {
		t.Fatalf("B did not claim beside the closing team: %+v %v", claimed, err)
	}
	if got, _ := f.c.GetTeamQueueEntry(ctx, f.task.ID, a.ID); got.State != "running" {
		t.Fatalf("A changed state: %+v", got)
	}
}

func TestOwnedChangesKeepsFilesUnderOwnership(t *testing.T) {
	changed := []string{"src/a/one.go", "src/ab.go", "docs/x.md", "SRC/C/z.go", "src/c"}
	if got := strings.Join(ownedChanges(changed, []string{"src/a", "src/c"}), ","); got != "src/a/one.go,SRC/C/z.go,src/c" {
		t.Fatalf("owned changes %q", got)
	}
	if got := strings.Join(ownedChanges(changed, nil), ","); got != strings.Join(changed, ",") {
		t.Fatalf("serial keeps all %q", got)
	}
}
