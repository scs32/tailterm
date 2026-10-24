package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

func TestTeamQueueCLIAddAndListAgainstTestHub(t *testing.T) {
	f := newTeamFixture(t, true)
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--cwd", t.TempDir()})
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, f.item.ID) || !strings.Contains(out, "queued") {
		t.Fatalf("queue list: %s", out)
	}
	q, err := f.c.ListTeamQueue(context.Background(), f.task.ID)
	if err != nil || len(q.Entries) != 1 || q.Entries[0].OrderMessageSeq != f.order {
		t.Fatalf("saved queue %+v %v", q, err)
	}
	second, err := f.c.CreateWorkItem(context.Background(), f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "second", RequestID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.c.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{Text: "second bounded order", RequestID: "second-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: second.ID, ItemRevision: second.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	f.confirmOrder(t, second, order.Seq)
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", second.ID, "--order", fmt.Sprint(order.Seq), "--cwd", t.TempDir()})
	}); err != nil {
		t.Fatal(err)
	}
	q, err = f.c.ListTeamQueue(context.Background(), f.task.ID)
	if err != nil || len(q.Entries) != 2 {
		t.Fatalf("two entries %+v %v", q, err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"reorder", "--entry", q.Entries[1].ID, "--before", q.Entries[0].ID})
	}); err != nil {
		t.Fatal(err)
	}
	q, err = f.c.ListTeamQueue(context.Background(), f.task.ID)
	if err != nil || q.Entries[0].ItemID != second.ID {
		t.Fatalf("CLI reorder %+v %v", q, err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"remove", "--entry", q.Entries[0].ID}) }); err != nil {
		t.Fatal(err)
	}
	q, err = f.c.ListTeamQueue(context.Background(), f.task.ID)
	if err != nil || len(q.Entries) != 1 || q.Entries[0].ItemID != f.item.ID {
		t.Fatalf("CLI remove %+v %v", q, err)
	}
}

func TestTeamQueueCLIReleaseAndAbandonAgainstTestHub(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	q, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "fail", Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: "fixture failure"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"release", "--entry", q.ID}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"release", "--entry", q.ID}) }); err != nil {
		t.Fatalf("release receipt retry: %v", err)
	}
	list, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) })
	if err != nil || !strings.Contains(list, "failed (released)") {
		t.Fatalf("released list %q %v", list, err)
	}
	second, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "manual item", RequestID: "manual-item"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "manual order", RequestID: "manual-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: second.ID, ItemRevision: second.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	f.confirmOrder(t, second, order.Seq)
	token := fmt.Sprintf("manual-%s-%s-%d", f.task.ID, second.ID, order.Seq)
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: token, Operation: "manual", ItemID: second.ID, OrderMessageSeq: order.Seq, PauseGeneration: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"abandon", "--item", second.ID, "--order", fmt.Sprint(order.Seq)})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"abandon", "--item", second.ID, "--order", fmt.Sprint(order.Seq)})
	}); err != nil {
		t.Fatalf("abandon receipt retry: %v", err)
	}
}

// queueGitRepo makes a temporary repository with one commit and returns its
// real root and HEAD.
func queueGitRepo(t *testing.T) (string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture", "commit", "-q", "--allow-empty", "-m", "fixture"},
	} {
		if output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
	}
	head, err := queueGitCommit(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, head
}

// parallelCLIProject gives the fixture's project a host policy for this host
// with a 1 MiB disk reserve and sets its limit through the CLI.
func parallelCLIProject(t *testing.T, f teamFixture, limit string) string {
	t.Helper()
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"policy", "--policy-version", fmt.Sprint(time.Now().UnixNano()), "--expires", time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "--sessions", "100", "--polling", "10", "--bindings", "100", "--requests-per-minute", "100000", "--burst", "10000", "--headroom-percent", "20", "--min-free-disk-mib", "1"})
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"limit", "--limit", limit}) })
	if err != nil {
		t.Fatalf("limit %s: %v", limit, err)
	}
	return out
}

func queueFixtureItem(t *testing.T, f teamFixture, key string) (api.WorkItem, int64) {
	t.Helper()
	ctx := context.Background()
	item, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: key, RequestID: key})
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: key + " bounded order", RequestID: key + "-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	f.confirmOrder(t, item, order.Seq)
	return item, order.Seq
}

// a1 (CLI): --limit none and N are saved and printed; invalid values print usage.
func TestTeamQueueCLILimitNoneAndCeiling(t *testing.T) {
	f := newTeamFixture(t, true)
	repo, _ := queueGitRepo(t)
	if out := parallelCLIProject(t, f, "none"); !strings.Contains(out, "concurrency limit=none") {
		t.Fatalf("limit none output %q", out)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--cwd", repo, "--owns", "client"})
	}); err != nil {
		t.Fatal(err)
	}
	if out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) }); err != nil || !strings.Contains(out, "concurrency limit=none") {
		t.Fatalf("list %q %v", out, err)
	}
	if out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"limit", "--limit", "5"}) }); err != nil || !strings.Contains(out, "concurrency limit=5") {
		t.Fatalf("limit 5 %q %v", out, err)
	}
	if out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) }); err != nil || !strings.Contains(out, "concurrency limit=5") {
		t.Fatalf("list %q %v", out, err)
	}
	for _, bad := range []string{"-1", "0", "two"} {
		if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"limit", "--limit", bad}) }); err == nil || !strings.Contains(err.Error(), "usage: tt team queue limit --limit none|N") {
			t.Fatalf("limit %s: %v", bad, err)
		}
	}
}

// a5: a parallel project's add creates the entry's own detached worktree by
// default; --no-new-worktree keeps the checkout; a taken path is refused.
func TestTeamQueueCLINewWorktreeDefault(t *testing.T) {
	f := newTeamFixture(t, true)
	repo, head := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	t.Chdir(repo)
	want := filepath.Join(repo, ".build", "worktrees", "queue-"+strings.TrimPrefix(f.item.ID, "wi_")[:8])
	// Bad ownership, a hub refusal and an unreachable hub all leave no
	// worktree behind, so the add can be retried.
	offline := f.e
	offline.hub = "http://127.0.0.1:1"
	for _, attempt := range []struct {
		name string
		e    env
		args []string
	}{
		{"traversal refused before creation", f.e, []string{"--owns", "../escape"}},
		{"hub refuses the ownership", f.e, []string{"--owns", "client:x"}},
		{"hub unreachable", offline, []string{"--owns", "client", "--new-worktree"}},
	} {
		args := append([]string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order)}, attempt.args...)
		if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(attempt.e, args) }); err == nil {
			t.Fatalf("%s: add succeeded", attempt.name)
		}
		if _, err := os.Lstat(want); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: left worktree %s: %v", attempt.name, want, err)
		}
		if list, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output(); err != nil || strings.Contains(string(list), want) {
			t.Fatalf("%s: git still lists the worktree: %s %v", attempt.name, list, err)
		}
	}
	out, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--owns", "client"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "worktree "+want) {
		t.Fatalf("add output %q", out)
	}
	list, err := f.c.ListTeamQueue(context.Background(), f.task.ID)
	if err != nil || len(list.Entries) != 1 {
		t.Fatalf("queue %+v %v", list, err)
	}
	q := list.Entries[0]
	realCwd, _ := filepath.EvalSymlinks(q.Cwd)
	if realCwd != want || q.Repository != filepath.Join(repo, ".git") || q.BaseCommit != head {
		t.Fatalf("entry cwd=%s repository=%s base=%s", q.Cwd, q.Repository, q.BaseCommit)
	}
	if branch, err := exec.Command("git", "-C", want, "rev-parse", "--abbrev-ref", "HEAD").Output(); err != nil || strings.TrimSpace(string(branch)) != "HEAD" {
		t.Fatalf("worktree is not detached: %q %v", branch, err)
	}
	if sha, err := queueGitCommit(want); err != nil || sha != head {
		t.Fatalf("worktree HEAD %s %v", sha, err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--owns", "client"})
	}); err == nil || !strings.Contains(err.Error(), "queue worktree already exists") || !strings.Contains(err.Error(), "--cwd "+want) {
		t.Fatalf("second add for the same path: %v", err)
	}

	second, order := queueFixtureItem(t, f, "opt-out")
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", second.ID, "--order", fmt.Sprint(order), "--owns", "docs", "--no-new-worktree"})
	}); err != nil {
		t.Fatal(err)
	}
	list, _ = f.c.ListTeamQueue(context.Background(), f.task.ID)
	if got, _ := filepath.EvalSymlinks(list.Entries[1].Cwd); got != repo {
		t.Fatalf("opt-out cwd %s", list.Entries[1].Cwd)
	}
	if _, err := os.Stat(filepath.Join(repo, ".build", "worktrees", "queue-"+strings.TrimPrefix(second.ID, "wi_")[:8])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opt-out created a worktree: %v", err)
	}

	// A definite hub refusal removes the new worktree so the add can be retried.
	third, _ := queueFixtureItem(t, f, "refused")
	refused := filepath.Join(repo, ".build", "worktrees", "queue-"+strings.TrimPrefix(third.ID, "wi_")[:8])
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", third.ID, "--order", fmt.Sprint(f.order), "--owns", "tests"})
	}); err == nil {
		t.Fatal("hub accepted another item's order")
	}
	if _, err := os.Stat(refused); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused add left its worktree: %v", err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", third.ID, "--order", "1", "--new-worktree", "--cwd", repo})
	}); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("--new-worktree with --cwd: %v", err)
	}
}

// queueTasksHubRepo makes a temporary repository whose main checkout stays on
// its default branch at c1, with tracked.txt, while tasks-hub has moved on to
// c2. It returns the real root, c1 and c2.
func queueTasksHubRepo(t *testing.T) (string, string, string) {
	t.Helper()
	root, _ := queueGitRepo(t)
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("c1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	queueGit(t, root, "add", "tracked.txt")
	queueGit(t, root, "-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture", "commit", "-q", "-m", "c1")
	c1 := queueGit(t, root, "rev-parse", "HEAD")
	c2 := queueChildCommit(t, root, c1, "c2")
	queueGit(t, root, "update-ref", "refs/heads/tasks-hub", c2)
	return root, c1, c2
}

// queueChildCommit makes a commit with parent's tree on no branch.
func queueChildCommit(t *testing.T, root, parent, message string) string {
	t.Helper()
	return queueGit(t, root, "-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture", "commit-tree", parent+"^{tree}", "-p", parent, "-m", message)
}

func queueGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// w0, w4 (wi_6a36a19047db7f81): a new queue worktree starts at the tasks-hub
// tip, not the caller's HEAD, whether that HEAD is behind tasks-hub or a
// detached commit ahead of it; an explicit --base off tasks-hub is refused
// before any worktree or entry exists.
func TestTeamQueueCLINewWorktreeBasesOnTasksHub(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	repo, c1, c2 := queueTasksHubRepo(t)
	parallelCLIProject(t, f, "none")
	entryFor := func(item string) (api.TeamQueueEntry, bool) {
		t.Helper()
		list, err := f.c.ListTeamQueue(ctx, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range list.Entries {
			if q.ItemID == item {
				return q, true
			}
		}
		return api.TeamQueueEntry{}, false
	}
	requireBase := func(name, item, want string) {
		t.Helper()
		q, ok := entryFor(item)
		path := queueWorktreeFor(repo, item)
		if !ok || q.BaseCommit != want {
			t.Fatalf("%s: entry %+v base %s, want %s", name, q, q.BaseCommit, want)
		}
		if got, _ := filepath.EvalSymlinks(q.Cwd); got != path {
			t.Fatalf("%s: entry cwd %s, want %s", name, q.Cwd, path)
		}
		if head := queueGit(t, path, "rev-parse", "HEAD"); head != want {
			t.Fatalf("%s: worktree HEAD %s, want %s", name, head, want)
		}
		if branch := queueGit(t, path, "rev-parse", "--abbrev-ref", "HEAD"); branch != "HEAD" {
			t.Fatalf("%s: worktree is on %s, not detached", name, branch)
		}
	}

	// a. The main checkout is at c1, behind tasks-hub c2.
	t.Chdir(repo)
	out, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--owns", "client"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "base "+c2) {
		t.Fatalf("add output %q does not name the base %s", out, c2)
	}
	requireBase("behind", f.item.ID, c2)
	if c1 == c2 {
		t.Fatal("fixture: c1 equals c2")
	}

	// b. A detached checkout one commit ahead of tasks-hub.
	c3 := queueChildCommit(t, repo, c2, "c3")
	ahead := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-ahead")
	queueGit(t, repo, "worktree", "add", "--detach", ahead, c3)
	t.Cleanup(func() { _ = os.RemoveAll(ahead) })
	t.Chdir(ahead)
	second, secondOrder := queueFixtureItem(t, f, "detached-ahead")
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", second.ID, "--order", fmt.Sprint(secondOrder), "--owns", "docs"})
	}); err != nil {
		t.Fatal(err)
	}
	requireBase("detached ahead", second.ID, c2)

	// c. An explicit base off tasks-hub is refused, naming the commit and
	// the branch, with no worktree, git worktree record or entry.
	third, thirdOrder := queueFixtureItem(t, f, "explicit-off-branch")
	_, err = captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", third.ID, "--order", fmt.Sprint(thirdOrder), "--owns", "tests", "--base", c3})
	})
	if err == nil || !strings.Contains(err.Error(), c3) || !strings.Contains(err.Error(), "tasks-hub") || !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("--base off tasks-hub: %v", err)
	}
	requireNoQueueWorktree(t, repo, queueWorktreeFor(repo, third.ID), "--base off tasks-hub")
	if q, ok := entryFor(third.ID); ok {
		t.Fatalf("--base off tasks-hub created entry %+v", q)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", third.ID, "--order", fmt.Sprint(thirdOrder), "--owns", "tests", "--base", "no-such-rev"})
	}); err == nil || !strings.Contains(err.Error(), "no-such-rev") {
		t.Fatalf("--base unknown: %v", err)
	}
	requireNoQueueWorktree(t, repo, queueWorktreeFor(repo, third.ID), "--base unknown")
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", third.ID, "--order", fmt.Sprint(thirdOrder), "--owns", "tests", "--base=--orphan"})
	}); err == nil || !strings.Contains(err.Error(), "is not a commit") {
		t.Fatalf("--base option-like: %v", err)
	}
	requireNoQueueWorktree(t, repo, queueWorktreeFor(repo, third.ID), "--base option-like")

	// d. --base needs a new worktree.
	for _, extra := range [][]string{{"--cwd", repo}, {"--no-new-worktree"}} {
		args := append([]string{"add", "--item", third.ID, "--order", fmt.Sprint(thirdOrder), "--owns", "tests", "--base", c1}, extra...)
		if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, args) }); err == nil || !strings.Contains(err.Error(), "--base applies only") {
			t.Fatalf("--base with %v: %v", extra, err)
		}
		requireNoQueueWorktree(t, repo, queueWorktreeFor(repo, third.ID), fmt.Sprintf("--base with %v", extra))
	}
	if q, ok := entryFor(third.ID); ok {
		t.Fatalf("refused --base created entry %+v", q)
	}

	// An explicit base on tasks-hub is taken as given.
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", third.ID, "--order", fmt.Sprint(thirdOrder), "--owns", "tests", "--base", c1})
	}); err != nil {
		t.Fatal(err)
	}
	requireBase("explicit ancestor", third.ID, c1)
}

// a6 (CLI) and a9: the owner and an available handler scope entries; a plain
// agent may not; only the owner fails an entry.
func TestTeamQueueCLIScopeAndOwnerFail(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	repo, head := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "scope-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: spawn.Host(), Cwd: repo, Repository: filepath.Join(repo, ".git"), BaseCommit: head, Serial: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"scope", "--entry", q.ID}) }); err == nil || !strings.Contains(err.Error(), "usage: tt team queue scope") {
		t.Fatalf("scope without paths: %v", err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"scope", "--entry", q.ID, "--owns", "client", "--owns", "docs/team-launch.md"})
	}); err != nil {
		t.Fatalf("owner scope: %v", err)
	}
	if got, _ := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); strings.Join(got.Ownership, ",") != "client,docs/team-launch.md" || got.Revision != q.Revision+1 {
		t.Fatalf("owner scope saved %+v", got)
	}
	handlerEnv := f.e
	handlerEnv.agent, handlerEnv.runID = f.handler.ID, f.handler.RunID
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(handlerEnv, []string{"scope", "--entry", q.ID, "--owns", "client"})
	}); err != nil {
		t.Fatalf("handler scope of a queued entry: %v", err)
	}
	staleEnv := handlerEnv
	staleEnv.runID = api.NewID("run")
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(staleEnv, []string{"scope", "--entry", q.ID, "--owns", "hub"})
	}); err == nil {
		t.Fatal("mismatched handler run scoped an entry")
	}
	worker, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "worker", Host: "fixture", Session: "worker", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	workerEnv := f.e
	workerEnv.agent, workerEnv.runID = worker.ID, worker.RunID
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(workerEnv, []string{"scope", "--entry", q.ID, "--owns", "hub"})
	}); err == nil {
		t.Fatal("a plain agent scoped a queued entry")
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(handlerEnv, []string{"fail", "--entry", q.ID, "--reason", "owner integrated abc1234"})
	}); err == nil || !strings.Contains(err.Error(), "unbound CLI session") {
		t.Fatalf("agent-bound fail: %v", err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"fail", "--entry", q.ID}) }); err == nil || !strings.Contains(err.Error(), "usage: tt team queue fail") {
		t.Fatalf("fail without reason: %v", err)
	}

	// Drive the entry to running, then the owner fails it.
	running := runQueueEntry(t, f, q.ID)
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"fail", "--entry", running.ID, "--reason", "owner integrated abc1234"})
	}); err != nil {
		t.Fatalf("owner fail: %v", err)
	}
	failed, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, running.ID)
	if err != nil || failed.State != "failed" || failed.Failure != "Owner failed this entry: owner integrated abc1234" || failed.EscalationSeq == 0 || failed.ReleasedAt != "" {
		t.Fatalf("owner-failed entry %+v %v", failed, err)
	}
}

// runQueueEntry claims, freezes and starts a one-member launch through the
// hub so an entry reaches running without a real spawn.
func runQueueEntry(t *testing.T, f teamFixture, id string) api.TeamQueueEntry {
	t.Helper()
	ctx := context.Background()
	q, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	step := func(req api.TeamQueueRequest) {
		t.Helper()
		req.EntryID, req.ExpectedRevision = q.ID, q.Revision
		if q, err = f.c.TeamQueueAction(ctx, f.task.ID, req); err != nil {
			t.Fatalf("%s: %v", req.Operation, err)
		}
	}
	step(api.TeamQueueRequest{RequestID: "run-claim-" + id, Operation: "claim", Host: q.Host, PauseGeneration: detail.Task.PauseGeneration})
	run := api.NewID("run")
	plan, _ := json.Marshal(map[string]any{"task": f.task.ID, "item": q.ItemID, "revision": q.ItemRevision, "order": q.OrderMessageSeq, "handlerId": q.HandlerID, "handlerRunId": q.HandlerRunID, "handlerLeaseGeneration": q.HandlerLeaseGeneration, "context": map[string]any{"version": 1}, "members": []any{map[string]any{"state": "unstarted", "runId": run, "fields": map[string]any{"agentId": api.NewID("agt"), "name": "lead-" + id[4:12], "cwd": q.Cwd}}}})
	step(api.TeamQueueRequest{RequestID: "run-freeze-" + id, Operation: "freeze", LaunchJSON: plan})
	step(api.TeamQueueRequest{RequestID: "run-attempt-" + id, Operation: "attempt", MemberIndex: 0})
	step(api.TeamQueueRequest{RequestID: "run-started-" + id, Operation: "started", MemberIndex: 0, MemberRunID: run})
	step(api.TeamQueueRequest{RequestID: "run-running-" + id, Operation: "running"})
	return q
}

// a1 (c1): with no --owns, a parallel add takes the ownership the handler
// recorded at scope confirmation for the current revision and order.
func TestTeamQueueCLIAddUsesIntakeOwnership(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	repo, _ := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	item, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "intake owned", RequestID: "intake-owned"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "intake owned order", RequestID: "intake-owned-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.ConfirmWorkOrderScope(ctx, f.task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "intake-owned-scope", AgentID: f.handler.ID, RunID: f.handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true, Ownership: []string{"hub/x.go", "docs/y.md"}}); err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", item.ID, "--order", fmt.Sprint(order.Seq), "--cwd", repo})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "owns hub/x.go,docs/y.md (from the scope confirmation)") {
		t.Fatalf("add output %q", out)
	}
	list, err := f.c.ListTeamQueue(ctx, f.task.ID)
	if err != nil || len(list.Entries) != 1 || strings.Join(list.Entries[0].Ownership, ",") != "hub/x.go,docs/y.md" || list.Entries[0].Serial {
		t.Fatalf("saved entry %+v %v", list, err)
	}
	if printed, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) }); err != nil || !strings.Contains(printed, "owns=hub/x.go,docs/y.md") {
		t.Fatalf("list %q %v", printed, err)
	}
}

// a2 (c1): a parallel add with no ownership anywhere is refused before any
// worktree exists; --serial saves a serial entry that lists as running alone.
func TestTeamQueueCLIParallelAddNeedsOwnershipOrSerial(t *testing.T) {
	f := newTeamFixture(t, true)
	repo, _ := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	t.Chdir(repo)
	want := filepath.Join(repo, ".build", "worktrees", "queue-"+strings.TrimPrefix(f.item.ID, "wi_")[:8])
	_, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order)})
	})
	if err == nil || !strings.Contains(err.Error(), "--owns PATH") || !strings.Contains(err.Error(), "scope confirm --owns") || !strings.Contains(err.Error(), "--serial") {
		t.Fatalf("unscoped parallel add: %v", err)
	}
	if _, statErr := os.Lstat(want); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused add left a worktree: %v", statErr)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--serial", "--owns", "client"})
	}); err == nil || !strings.Contains(err.Error(), "either --owns or --serial") {
		t.Fatalf("--serial with --owns: %v", err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--serial"})
	}); err != nil {
		t.Fatal(err)
	}
	list, err := f.c.ListTeamQueue(context.Background(), f.task.ID)
	if err != nil || len(list.Entries) != 1 || !list.Entries[0].Serial || len(list.Entries[0].Ownership) != 0 {
		t.Fatalf("serial entry %+v %v", list, err)
	}
	if printed, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) }); err != nil || !strings.Contains(printed, "owns=serial (runs alone)") {
		t.Fatalf("list %q %v", printed, err)
	}
}

// The list shows a leased entry's handler arm (wi_fc1396aef8a72a06).
func TestTeamQueueListArmAssignment(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	digest := strings.Repeat("a", 64)
	arms := []api.HandlerArm{{ID: "S", Runtime: "claude", Model: "claude-sonnet-5-5", Reasoning: "high", Weight: 1}, {ID: "O", Runtime: "codex", Model: "gpt-6.1-sol", Reasoning: "high", Weight: 1}}
	for _, arm := range arms {
		h, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler-" + strings.ToLower(arm.ID), Role: api.AgentRoleDatabaseHandler, Host: "fixture",
			Session: "fixture-" + arm.ID, Runtime: arm.Runtime, TemplateDigest: digest, HandlerModel: arm.Model, HandlerReasoning: arm.Reasoning})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: h.ID, RunID: h.RunID, Kind: api.EventRunning}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.c.SetHandlerArmPolicy(ctx, f.task.ID, api.HandlerArmPolicyRequest{RequestID: "arms", Enabled: true, Seed: "K", TemplateDigest: digest, Arms: arms}); err != nil {
		t.Fatal(err)
	}
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "arm-list-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "arm-list-claim", Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: "fixture"})
	if err != nil || claimed.HandlerArm == nil {
		t.Fatalf("claim %+v %v", claimed, err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) })
	want := fmt.Sprintf("arm=%s drawn=%s fallback=no", claimed.HandlerArm.Arm, claimed.HandlerArm.Arm)
	if err != nil || !strings.Contains(out, want) {
		t.Fatalf("list lacks %q: %s %v", want, out, err)
	}
	if got := queueArmText(&api.TeamQueueHandlerArm{Arm: "O", DrawnArm: "S", Fallback: true, FallbackReason: "busy"}); got != " arm=O drawn=S fallback=busy" {
		t.Fatalf("fallback text %q", got)
	}
	if queueArmText(nil) != "" {
		t.Fatal("unassigned entry printed an arm")
	}
}

// queueMoveProxy fronts the fixture hub; move handles each scope request
// that moves an entry to a new worktree, with the real hub as forward.
func queueMoveProxy(t *testing.T, hub string, move func(w http.ResponseWriter, r *http.Request, forward http.Handler)) string {
	t.Helper()
	target, err := url.Parse(hub)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/team-queue/actions") {
			body, _ := io.ReadAll(r.Body)
			r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
			var req api.TeamQueueRequest
			if json.Unmarshal(body, &req) == nil && req.Operation == "scope" && req.Cwd != "" {
				move(w, r, forward)
				return
			}
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func dropQueueResponse(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		conn.Close()
	}
}

// liveQueueHandler adds a second available database handler, so two
// entries can hold leases at once.
func liveQueueHandler(t *testing.T, f teamFixture) {
	t.Helper()
	ctx := context.Background()
	h, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler-2", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "fixture-handler-2", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: h.ID, RunID: h.RunID, Kind: api.EventRunning}); err != nil {
		t.Fatal(err)
	}
}

func claimQueueEntry(t *testing.T, f teamFixture, q api.TeamQueueEntry, key string) (api.TeamQueueEntry, error) {
	t.Helper()
	detail, err := f.c.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f.c.TeamQueueAction(context.Background(), f.task.ID, api.TeamQueueRequest{RequestID: key, Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: q.Host, PauseGeneration: detail.Task.PauseGeneration})
}

func queueWorktreeFor(repo, item string) string {
	return filepath.Join(repo, ".build", "worktrees", "queue-"+strings.TrimPrefix(item, "wi_")[:8])
}

func requireNoQueueWorktree(t *testing.T, repo, path, why string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s: left worktree %s: %v", why, path, err)
	}
	if list, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output(); err != nil || strings.Contains(string(list), path) {
		t.Fatalf("%s: git still lists the worktree: %s %v", why, list, err)
	}
}

// q1-q3: a legacy entry on the main checkout, blocked only because a running
// entry shares that checkout, moves to its own worktree at its frozen base and
// launches beside it.
func TestTeamQueueCLIScopeNewWorktreeMovesSharedCheckoutEntry(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	repo, head := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	liveQueueHandler(t, f)
	second, secondOrder := queueFixtureItem(t, f, "shared-checkout")
	for _, args := range [][]string{
		{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--cwd", repo, "--owns", "client"},
		{"add", "--item", second.ID, "--order", fmt.Sprint(secondOrder), "--cwd", repo, "--owns", "docs"},
	} {
		if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, args) }); err != nil {
			t.Fatal(err)
		}
	}
	list, err := f.c.ListTeamQueue(ctx, f.task.ID)
	if err != nil || len(list.Entries) != 2 {
		t.Fatalf("queue %+v %v", list, err)
	}
	a, b := list.Entries[0], list.Entries[1]
	if a, err = claimQueueEntry(t, f, a, "claim-a"); err != nil {
		t.Fatal(err)
	}
	printed, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) })
	if err != nil || !strings.Contains(printed, "cwd="+b.Cwd+" owns=docs blocked-by="+a.ID+" reason=Shares checkout "+b.Cwd+" with active entry "+a.ID) || !strings.Contains(printed, "--entry "+b.ID+" --new-worktree") {
		t.Fatalf("shared checkout list: %s %v", printed, err)
	}
	if _, err := claimQueueEntry(t, f, b, "claim-b-shared"); err == nil {
		t.Fatal("B launched in A's checkout")
	}
	// The main checkout moves on; the new worktree still starts at B's base.
	if output, err := exec.Command("git", "-C", repo, "-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture", "commit", "-q", "--allow-empty", "-m", "newer").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, output)
	}
	if newer, _ := queueGitCommit(repo); newer == head {
		t.Fatal("main checkout did not advance")
	}
	want := queueWorktreeFor(repo, second.ID)
	out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"scope", "--entry", b.ID, "--new-worktree"}) })
	if err != nil || !strings.Contains(out, "worktree "+want) {
		t.Fatalf("move %q %v", out, err)
	}
	moved, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if real, _ := filepath.EvalSymlinks(moved.Cwd); real != want || moved.Position != b.Position || moved.Revision != b.Revision+1 || moved.State != "queued" || moved.BaseCommit != head || moved.Repository != b.Repository || moved.ItemID != b.ItemID || moved.OrderMessageSeq != b.OrderMessageSeq || strings.Join(moved.Ownership, ",") != "docs" {
		t.Fatalf("moved %+v, was %+v", moved, b)
	}
	if branch, err := exec.Command("git", "-C", want, "rev-parse", "--abbrev-ref", "HEAD").Output(); err != nil || strings.TrimSpace(string(branch)) != "HEAD" {
		t.Fatalf("worktree is not detached: %q %v", branch, err)
	}
	if sha, err := queueGitCommit(want); err != nil || sha != head {
		t.Fatalf("worktree HEAD %s %v, want base %s", sha, err, head)
	}
	if printed, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) }); err != nil || !strings.Contains(printed, "cwd="+moved.Cwd+" owns=docs blocked-by= reason= ") {
		t.Fatalf("moved list: %s %v", printed, err)
	}
	// A rerun changes nothing.
	out, err = captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"scope", "--entry", b.ID, "--new-worktree"}) })
	if err != nil || !strings.Contains(out, "entry already uses its own worktree "+moved.Cwd) {
		t.Fatalf("rerun %q %v", out, err)
	}
	if again, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, b.ID); err != nil || again.Revision != moved.Revision || again.Cwd != moved.Cwd {
		t.Fatalf("rerun changed the entry %+v %v", again, err)
	}
	claimed, err := claimQueueEntry(t, f, moved, "claim-b-own")
	if err != nil {
		t.Fatalf("moved B did not launch beside A: %v", err)
	}
	if still, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, a.ID); err != nil || still.State != "launching" || still.HandlerID == claimed.HandlerID {
		t.Fatalf("A beside B %+v %v (B handler %s)", still, err, claimed.HandlerID)
	}
}

// A legacy entry without a frozen repository, or one no longer queued, is
// refused before any worktree exists.
func TestTeamQueueCLIScopeNewWorktreeRefusesLegacyAndAdmittedEntries(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	repo, _ := queueGitRepo(t)
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "legacy", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: spawn.Host(), Cwd: repo, Ownership: []string{"client"}})
	if err != nil {
		t.Fatal(err)
	}
	want := queueWorktreeFor(repo, f.item.ID)
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"scope", "--entry", q.ID, "--new-worktree"}) }); err == nil || !strings.Contains(err.Error(), "no frozen repository and base") {
		t.Fatalf("legacy entry: %v", err)
	}
	requireNoQueueWorktree(t, repo, want, "legacy entry")
	if _, err := claimQueueEntry(t, f, q, "claim-legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"scope", "--entry", q.ID, "--new-worktree"}) }); err == nil || !strings.Contains(err.Error(), "only a queued entry") {
		t.Fatalf("launching entry: %v", err)
	}
	requireNoQueueWorktree(t, repo, want, "launching entry")
}

// Every refusal leaves no worktree, so the move can be retried; a saved
// move whose response was lost keeps its worktree.
func TestTeamQueueCLIScopeNewWorktreeRefusalsLeaveNoWorktree(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	repo, head := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	liveQueueHandler(t, f)
	serialItem, serialOrder := queueFixtureItem(t, f, "serial")
	serial, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "serial", Operation: "add", ItemID: serialItem.ID, OrderMessageSeq: serialOrder, Host: spawn.Host(), Cwd: repo, Repository: filepath.Join(repo, ".git"), BaseCommit: head, Serial: true})
	if err != nil {
		t.Fatal(err)
	}
	scopedItem, scopedOrder := queueFixtureItem(t, f, "scoped")
	scoped, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "scoped", Operation: "add", ItemID: scopedItem.ID, OrderMessageSeq: scopedOrder, Host: spawn.Host(), Cwd: repo, Repository: filepath.Join(repo, ".git"), BaseCommit: head, Ownership: []string{"docs"}})
	if err != nil {
		t.Fatal(err)
	}
	offline := f.e
	offline.hub = "http://127.0.0.1:1"
	stale := f.e
	stale.hub = queueMoveProxy(t, f.e.hub, func(w http.ResponseWriter, r *http.Request, forward http.Handler) {
		// Another writer changes the entry between the CLI's read and write.
		current, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, scoped.ID)
		if err == nil {
			_, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "scope", EntryID: current.ID, ExpectedRevision: current.Revision, Ownership: current.Ownership})
		}
		if err != nil {
			t.Errorf("concurrent scope: %v", err)
		}
		forward.ServeHTTP(w, r)
	})
	dropped := f.e
	dropped.hub = queueMoveProxy(t, f.e.hub, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) { dropQueueResponse(w) })
	for _, attempt := range []struct {
		name, want string
		e          env
		q          api.TeamQueueEntry
		args       []string
	}{
		{"--cwd", "usage:", f.e, scoped, []string{"--cwd", repo}},
		{"--no-new-worktree", "usage:", f.e, scoped, []string{"--no-new-worktree"}},
		{"no ownership", "declares no ownership", f.e, serial, nil},
		{"hub unreachable", "", offline, scoped, nil},
		{"stale revision", "revision changed", stale, scoped, nil},
		{"response dropped before the hub", "", dropped, scoped, nil},
	} {
		args := append([]string{"scope", "--entry", attempt.q.ID, "--new-worktree"}, attempt.args...)
		before, _ := f.c.GetTeamQueueEntry(ctx, f.task.ID, attempt.q.ID)
		out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(attempt.e, args) })
		if err == nil || !strings.Contains(err.Error(), attempt.want) {
			t.Fatalf("%s: %v", attempt.name, err)
		}
		path := queueWorktreeFor(repo, attempt.q.ItemID)
		if created := strings.Contains(out, "worktree "+path); created != (attempt.e.hub != f.e.hub && attempt.e.hub != offline.hub) {
			t.Fatalf("%s: worktree created=%v: %q", attempt.name, created, out)
		}
		requireNoQueueWorktree(t, repo, path, attempt.name)
		if after, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, attempt.q.ID); err != nil || after.Cwd != before.Cwd {
			t.Fatalf("%s: cwd changed %+v %v", attempt.name, after, err)
		}
	}
	// The hub saves the move but its response is lost: the CLI reads the
	// saved entry back and keeps the worktree it names.
	lost := f.e
	lost.hub = queueMoveProxy(t, f.e.hub, func(w http.ResponseWriter, r *http.Request, forward http.Handler) {
		forward.ServeHTTP(httptest.NewRecorder(), r)
		dropQueueResponse(w)
	})
	want := queueWorktreeFor(repo, scopedItem.ID)
	if out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(lost, []string{"scope", "--entry", scoped.ID, "--new-worktree"}) }); err != nil || !strings.Contains(out, "worktree "+want) {
		t.Fatalf("lost response %q %v", out, err)
	}
	saved, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, scoped.ID)
	if real, _ := filepath.EvalSymlinks(saved.Cwd); err != nil || real != want {
		t.Fatalf("saved %+v %v", saved, err)
	}
	if sha, err := queueGitCommit(want); err != nil || sha != head {
		t.Fatalf("kept worktree HEAD %s %v", sha, err)
	}
}

// When the hub saved a move but both its response and the read-back were
// lost, the CLI removed the worktree the entry names; a rerun recreates it
// at the entry's base without another hub write.
func TestTeamQueueCLIScopeNewWorktreeRepairsLostMove(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	repo, head := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "shared", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: spawn.Host(), Cwd: repo, Repository: filepath.Join(repo, ".git"), BaseCommit: head, Ownership: []string{"docs"}})
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(f.e.hub)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	var saved atomic.Bool
	lost := f.e
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/team-queue/actions"):
			forward.ServeHTTP(httptest.NewRecorder(), r)
			saved.Store(true)
			dropQueueResponse(w)
		case saved.Load() && r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/team-queue/"+q.ID):
			dropQueueResponse(w)
		default:
			forward.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(proxy.Close)
	lost.hub = proxy.URL
	want := queueWorktreeFor(repo, f.item.ID)
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(lost, []string{"scope", "--entry", q.ID, "--new-worktree"}) }); err == nil {
		t.Fatal("double loss reported success")
	}
	requireNoQueueWorktree(t, repo, want, "double loss")
	moved, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || moved.Cwd != want || moved.Revision != q.Revision+1 {
		t.Fatalf("hub did not save the move %+v %v", moved, err)
	}
	if output, err := exec.Command("git", "-C", repo, "-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture", "commit", "-q", "--allow-empty", "-m", "newer").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, output)
	}
	out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"scope", "--entry", q.ID, "--new-worktree"}) })
	if err != nil || !strings.Contains(out, "worktree "+want+" recreated at "+head) {
		t.Fatalf("repair %q %v", out, err)
	}
	if sha, err := queueGitCommit(want); err != nil || sha != head {
		t.Fatalf("recreated worktree HEAD %s %v, want base %s", sha, err, head)
	}
	if branch, err := exec.Command("git", "-C", want, "rev-parse", "--abbrev-ref", "HEAD").Output(); err != nil || strings.TrimSpace(string(branch)) != "HEAD" {
		t.Fatalf("recreated worktree is not detached: %q %v", branch, err)
	}
	if again, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); err != nil || again.Revision != moved.Revision || again.Cwd != want {
		t.Fatalf("repair wrote to the hub %+v %v", again, err)
	}
	if out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"scope", "--entry", q.ID, "--new-worktree"}) }); err != nil || !strings.Contains(out, "entry already uses its own worktree "+want) {
		t.Fatalf("rerun after repair %q %v", out, err)
	}
	// A missing checkout that is not the entry's queue worktree is refused.
	other, order := queueFixtureItem(t, f, "missing")
	gone := filepath.Join(t.TempDir(), "gone")
	missing, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "missing", Operation: "add", ItemID: other.ID, OrderMessageSeq: order, Host: spawn.Host(), Cwd: gone, Repository: filepath.Join(repo, ".git"), BaseCommit: head, Ownership: []string{"client"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"scope", "--entry", missing.ID, "--new-worktree"}) }); err == nil || !strings.Contains(err.Error(), "names a missing checkout") {
		t.Fatalf("missing unrelated checkout: %v", err)
	}
	requireNoQueueWorktree(t, repo, queueWorktreeFor(repo, other.ID), "missing unrelated checkout")
}

// recordTeamQueueListings fronts the fixture hub with a proxy that records
// the query of every team queue listing request (not entry reads or actions).
func recordTeamQueueListings(t *testing.T, f teamFixture) (env, *api.Client, func() []string) {
	t.Helper()
	target, err := url.Parse(f.e.hub)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	var mu sync.Mutex
	var queries []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/tasks/"+f.task.ID+"/team-queue" {
			mu.Lock()
			queries = append(queries, r.URL.RawQuery)
			mu.Unlock()
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	c, err := api.NewClient(proxy.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	e := f.e
	e.hub = proxy.URL
	return e, c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), queries...)
	}
}

// insertFinishedQueueRows files n items with finished queue entries at
// positions from..from+n-1, each carrying a 32 KiB launch.
func insertFinishedQueueRows(t *testing.T, f teamFixture, from, n int) []api.TeamQueueEntry {
	t.Helper()
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	launch := `{"members":[{"fields":{"name":"lead","role":"lead"}}],"pad":"` + strings.Repeat("x", 32<<10) + `"}`
	now := time.Now().UTC().Format(time.RFC3339Nano)
	out := make([]api.TeamQueueEntry, 0, n)
	for i := 0; i < n; i++ {
		item, err := f.st.CreateWorkItem(context.Background(), f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "finished fixture", RequestID: api.NewID("req")}, api.Caller{Node: "team-fixture", User: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		id := api.NewID("tqe")
		if _, err := db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,launch_json,created_at,updated_at) VALUES(?,?,?,?,1,'planned',?,'finished',3,'fixture','/tmp',?,?,?)`, id, f.task.ID, item.ID, item.Revision, from+i, launch, now, now); err != nil {
			t.Fatal(err)
		}
		out = append(out, api.TeamQueueEntry{ID: id, ItemID: item.ID, Position: int64(from + i)})
	}
	return out
}

// q2: tt team queue add reads only the active entries.
func TestTeamQueueCLIAddReadsActiveEntries(t *testing.T) {
	f := newTeamFixture(t, true)
	insertFinishedQueueRows(t, f, 1, 3)
	e, _, queries := recordTeamQueueListings(t, f)
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--cwd", t.TempDir()})
	}); err != nil {
		t.Fatal(err)
	}
	got := queries()
	if len(got) == 0 {
		t.Fatal("add read no listing")
	}
	for _, q := range got {
		if q != "view=active" {
			t.Fatalf("add listing query %q, want view=active (all %v)", q, got)
		}
	}
}

// q3: tt team queue list shows the newest history page and how to read
// older pages and one entry in full.
func TestTeamQueueCLIListPagesHistory(t *testing.T) {
	f := newTeamFixture(t, true)
	history := insertFinishedQueueRows(t, f, 1, 5)
	e, _, queries := recordTeamQueueListings(t, f)
	out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(e, []string{"list", "--limit", "2"}) })
	if err != nil {
		t.Fatal(err)
	}
	older := fmt.Sprintf("older: tt team queue list --task %s --limit 2 --after 4", f.task.ID)
	if !strings.Contains(out, "history: showing 2 of 5") || !strings.Contains(out, older) || !strings.Contains(out, history[4].ID) || strings.Contains(out, history[2].ID) {
		t.Fatalf("first page:\n%s", out)
	}
	out, err = captureCLIOutput(t, func() error { return cmdTeamQueue(e, []string{"list", "--limit", "2", "--after", "4"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "history: showing 2 of 5") || !strings.Contains(out, "--after 2") || !strings.Contains(out, history[2].ID) || strings.Contains(out, history[4].ID) {
		t.Fatalf("second page:\n%s", out)
	}
	out, err = captureCLIOutput(t, func() error { return cmdTeamQueue(e, []string{"list", "--limit", "2", "--after", "2"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "history: showing 1 of 5") || strings.Contains(out, "older:") || !strings.Contains(out, history[0].ID) {
		t.Fatalf("last page:\n%s", out)
	}
	out, err = captureCLIOutput(t, func() error { return cmdTeamQueue(e, []string{"list", "--item", history[0].ItemID, "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var one api.TeamQueueList
	if err := json.Unmarshal([]byte(out), &one); err != nil || len(one.Entries) != 1 || one.Entries[0].Summary || len(one.Entries[0].LaunchJSON) < 32<<10 {
		t.Fatalf("item listing %v: %.300s", err, out)
	}
	want := []string{"limit=2", "after=4&limit=2", "after=2&limit=2", "item=" + history[0].ItemID}
	if got := queries(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("queries %v, want %v", got, want)
	}
	for _, bad := range [][]string{{"list", "--limit", "0"}, {"list", "--limit", "201"}, {"list", "--active", "--after", "2"}, {"list", "--item", "tqe_x"}} {
		if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(e, bad) }); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("%v: %v", bad, err)
		}
	}
}

// q2: a retried done-save finds its accepted entry by item even after it
// finished and newer history pushed it off the default page.
func TestPendingQueueAcceptanceFindsFinishedEntryByItem(t *testing.T) {
	f := newTeamFixture(t, true)
	accepted := insertFinishedQueueRows(t, f, 1, 1)[0]
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	acceptance := `{"repository":"repo","baseCommit":"base","worktree":"/w","branch":"b","commit":"abc","itemRevision":2,"evidence":"saved","acceptedAt":"2026-09-30T00:00:00Z"}`
	if _, err := db.Exec(`UPDATE team_queue_entries SET repository='repo',handler_id=?,handler_run_id=?,acceptance_json=? WHERE id=?`, f.handler.ID, f.handler.RunID, acceptance, accepted.ID); err != nil {
		t.Fatal(err)
	}
	insertFinishedQueueRows(t, f, 2, api.DefaultTeamQueueHistoryLimit+5)
	if list, err := f.c.ListTeamQueue(context.Background(), f.task.ID); err != nil || len(list.Entries) != api.DefaultTeamQueueHistoryLimit {
		t.Fatalf("default page %d entries %v", len(list.Entries), err)
	} else {
		for _, q := range list.Entries {
			if q.ID == accepted.ID {
				t.Fatal("the fixture's accepted entry is still on the default page")
			}
		}
	}
	_, c, queries := recordTeamQueueListings(t, f)
	got, err := pendingQueueAcceptance(context.Background(), c, f.task.ID, accepted.ItemID, f.handler.ID, f.handler.RunID)
	if err != nil || got == nil || got.ID != accepted.ID || got.Acceptance == nil || got.Acceptance.Commit != "abc" || got.State != "finished" {
		t.Fatalf("pending acceptance %+v %v", got, err)
	}
	if q := queries(); len(q) != 1 || q[0] != "item="+accepted.ItemID {
		t.Fatalf("queries %v", q)
	}
}

// wi_f8d48780626165cc a4: the CLI queues an explicitly chosen small bug and
// shows each entry's template.
func TestTeamQueueCLISmallTemplateAddListAndUsage(t *testing.T) {
	f := newTeamFixtureKind(t, true, "bug")
	ctx := context.Background()
	repo, _ := queueGitRepo(t)
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--template", "bogus", "--cwd", t.TempDir()})
	}); err == nil || !strings.Contains(err.Error(), "usage: tt team queue add") || !strings.Contains(err.Error(), "[--template planned|small]") {
		t.Fatalf("bogus template: %v", err)
	}
	if q, err := f.c.ListTeamQueue(ctx, f.task.ID); err != nil || len(q.Entries) != 0 {
		t.Fatalf("usage error queued %+v %v", q, err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--template", "small", "--owns", "hub/cmd/tt/fix.go", "--owns", "hub/cmd/tt/fix_test.go", "--cwd", repo})
	}); err != nil {
		t.Fatal(err)
	}
	q, err := f.c.ListTeamQueue(ctx, f.task.ID)
	if err != nil || len(q.Entries) != 1 || q.Entries[0].Template != "small" || strings.Join(q.Entries[0].Ownership, ",") != "hub/cmd/tt/fix.go,hub/cmd/tt/fix_test.go" {
		t.Fatalf("saved small entry %+v %v", q, err)
	}
	planned, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "planned", RequestID: "planned"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "planned bounded order", RequestID: "planned-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: planned.ID, ItemRevision: planned.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	f.confirmOrder(t, planned, order.Seq)
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", planned.ID, "--order", fmt.Sprint(order.Seq), "--cwd", t.TempDir()})
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{f.item.ID + " order=#" + fmt.Sprint(f.order) + " template=small ", planned.ID + " order=#" + fmt.Sprint(order.Seq) + " template=planned "} {
		if !strings.Contains(out, want) {
			t.Fatalf("queue list lacks %q:\n%s", want, out)
		}
	}
}

// No test in this package may read the host's own verification lock file
// through an inherited override; fixtures set HOME to a temporary directory.
func init() { os.Unsetenv("TAILTERM_MATRIX_HOST_LOCK") }

// a6, a7 (wi_c5cb667695c3614c): tt team queue list shows each entry whose item
// waits for the verification host, from the host lock file.
func TestTeamQueueListMatrixWait(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	remote, remoteOrder := queueFixtureItem(t, f, "matrix-remote")
	idle := f.item
	add := func(key, item string, order int64, host string) {
		t.Helper()
		if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: key, Operation: "add", ItemID: item, OrderMessageSeq: order, Host: host, Cwd: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
	}
	add("matrix-add-idle", idle.ID, f.order, "Stephens-Mini")
	waitingItem, waitingSeq := queueFixtureItem(t, f, "matrix-waiting")
	add("matrix-add-waiting", waitingItem.ID, waitingSeq, "stephens-mini")
	add("matrix-add-remote", remote.ID, remoteOrder, "truenas")
	list := func() string {
		t.Helper()
		out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) })
		if err != nil {
			t.Fatalf("list must exit 0: %v\n%s", err, out)
		}
		for _, id := range []string{idle.ID, waitingItem.ID, remote.ID} {
			if !strings.Contains(out, id) {
				t.Fatalf("entries must stay intact, missing %s:\n%s", id, out)
			}
		}
		return out
	}
	listJSON := func() string {
		t.Helper()
		out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list", "--json"}) })
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	old := matrixLocalHost
	matrixLocalHost = func() (string, error) { return "Stephens-Mini.local", nil }
	t.Cleanup(func() { matrixLocalHost = old })

	// Absent file: today's output, in the default path under HOME and under an override.
	plain, plainJSON := list(), listJSON()
	if strings.Contains(plain, "matrix host") || strings.Contains(plain, "waiting-for-matrix") {
		t.Fatalf("an absent lock file must add nothing:\n%s", plain)
	}
	path := filepath.Join(t.TempDir(), "host.json")
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", path)
	if out := list(); out != plain {
		t.Fatalf("an absent override file must add nothing:\n%s", out)
	}
	write := func(host string, version int, holder any, waiters ...map[string]any) {
		t.Helper()
		if waiters == nil {
			waiters = []map[string]any{}
		}
		raw, err := json.Marshal(map[string]any{"version": version, "host": host, "requestSeq": 9, "grantSeq": 3, "holder": holder, "waiters": waiters})
		if err != nil || os.WriteFile(path, raw, 0o600) != nil {
			t.Fatal("write lock file")
		}
	}
	waiter := func(item, priority, kind, agent string) map[string]any {
		return map[string]any{"id": "w-" + item, "pid": 4242, "kind": kind, "item": item, "agent": agent, "priority": priority, "requestedAt": "2026-10-01T14:05:00.000Z"}
	}
	holder := map[string]any{"id": "h", "pid": 777, "kind": "run", "item": "wi_holder", "agent": "verifier-1", "priority": "high", "startedAt": "2026-10-01T14:00:00.000Z", "groups": []int{}}
	lines := func(out string) []string {
		var got []string
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "matrix host:") || strings.Contains(line, "waiting-for-matrix") {
				got = append(got, line)
			}
		}
		return got
	}

	// a6: one line under the waiting entry only, with position, holder and priority.
	write("Stephens-Mini.local", 1, holder, waiter("wi_someone_else", "urgent", "run", "verifier-2"), waiter(waitingItem.ID, "high", "run", "deployer"), waiter(remote.ID, "normal", "targeted", "verifier-3"))
	out := list()
	want := "  waiting-for-matrix position=2 of 3 holder=wi_holder/verifier-1/pid 777 priority=high kind=run agent=deployer since=2026-10-01T14:05:00.000Z"
	if got := lines(out); len(got) != 1 || got[0] != want {
		t.Fatalf("waiting lines %q, want only %q:\n%s", got, want, out)
	}
	entryAt := strings.Index(out, waitingItem.ID)
	if next := strings.Index(out[entryAt:], "\n"); !strings.Contains(out[entryAt+next:], want) || strings.Index(out, want) < entryAt {
		t.Fatalf("the line must follow its entry:\n%s", out)
	}
	if strings.Index(out, want) < strings.Index(out, idle.ID) && strings.Index(out, idle.ID) > entryAt {
		t.Fatalf("the line must be under the waiting entry, not another:\n%s", out)
	}
	if got := listJSON(); got != plainJSON {
		t.Fatalf("--json must be unchanged:\n%s", got)
	}
	// A free host, and two runs of one item.
	write("stephens-mini", 1, nil, waiter(waitingItem.ID, "high", "run", "deployer"), waiter(waitingItem.ID, "normal", "targeted", "verifier-9"))
	if got := lines(list()); len(got) != 2 || !strings.Contains(got[0], "position=1 of 2 holder=none priority=high kind=run agent=deployer") || !strings.Contains(got[1], "position=2 of 2 holder=none priority=normal kind=targeted agent=verifier-9") {
		t.Fatalf("free host lines %q", got)
	}
	// Only a holder: nothing waits.
	write("Stephens-Mini", 1, holder)
	if out := list(); out != plain {
		t.Fatalf("no waiter must add nothing:\n%s", out)
	}

	// q1, q2 (wi_dacc0b35ff1cec85): a version 2 lock names every holder, in
	// the file's order, on each waiting entry's line.
	writeV2 := func(holders []map[string]any, waiters ...map[string]any) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"version": 2, "host": "Stephens-Mini.local", "requestSeq": 9, "grantSeq": 3, "holders": holders, "waiters": waiters})
		if err != nil || os.WriteFile(path, raw, 0o600) != nil {
			t.Fatal("write lock file")
		}
	}
	second := map[string]any{"id": "h2", "pid": 778, "kind": "targeted", "item": "wi_second", "agent": "verifier-4", "priority": "normal", "startedAt": "2026-10-01T14:01:00.000Z", "groups": []int{}}
	writeV2([]map[string]any{holder, second}, waiter("wi_someone_else", "urgent", "run", "verifier-2"), waiter(waitingItem.ID, "high", "run", "deployer"), waiter(waitingItem.ID, "normal", "targeted", "verifier-9"))
	out = list()
	holders := "holder=wi_holder/verifier-1/pid 777,wi_second/verifier-4/pid 778"
	wantV2 := []string{
		"  waiting-for-matrix position=2 of 3 " + holders + " priority=high kind=run agent=deployer since=2026-10-01T14:05:00.000Z",
		"  waiting-for-matrix position=3 of 3 " + holders + " priority=normal kind=targeted agent=verifier-9 since=2026-10-01T14:05:00.000Z",
	}
	if got := lines(out); len(got) != 2 || got[0] != wantV2[0] || got[1] != wantV2[1] {
		t.Fatalf("version 2 lines %q, want %q:\n%s", got, wantV2, out)
	}
	if at := strings.Index(out, wantV2[0]+"\n"+wantV2[1]+"\n"); at < strings.Index(out, waitingItem.ID) || at > strings.Index(out, remote.ID) {
		t.Fatalf("the version 2 lines must follow their entry:\n%s", out)
	}
	if got := listJSON(); got != plainJSON {
		t.Fatalf("--json must be unchanged for version 2:\n%s", got)
	}
	// Version 2 with no holder is a free host, and with no waiter adds nothing.
	writeV2([]map[string]any{}, waiter(waitingItem.ID, "high", "run", "deployer"))
	if got := lines(list()); len(got) != 1 || !strings.Contains(got[0], "position=1 of 1 holder=none priority=high kind=run agent=deployer") {
		t.Fatalf("version 2 free host lines %q", got)
	}
	writeV2([]map[string]any{holder, second})
	if out := list(); out != plain {
		t.Fatalf("version 2 with no waiter must add nothing:\n%s", out)
	}

	// u1, u2 (wi_d6d7da393cea9c08): a live holder or waiter that names no
	// agent and no valid item is printed once after the entries, marked
	// unattributed. An agentless run with a valid item keeps its entry's line
	// only, and a named, dead or item-bearing run gets no unattributed line.
	oldAlive := matrixWaitPIDAlive
	matrixWaitPIDAlive = func(pid int) bool { return pid != 9999 }
	t.Cleanup(func() { matrixWaitPIDAlive = oldAlive })
	bare := func(pid int, agent, item, kind, priority string) map[string]any {
		return map[string]any{"id": fmt.Sprintf("u%d", pid), "pid": pid, "kind": kind, "item": item, "agent": agent, "priority": priority, "requestedAt": "2026-10-01T14:06:00.000Z"}
	}
	bareHolder := map[string]any{"id": "hb", "pid": 501, "kind": "exec", "item": "", "agent": "", "priority": "normal", "startedAt": "2026-10-01T14:02:00.000Z", "groups": []int{}}
	itemHolder := map[string]any{"id": "hi", "pid": 505, "kind": "run", "item": remote.ID, "agent": "unknown", "priority": "high", "startedAt": "2026-10-01T14:03:00.000Z", "groups": []int{}}
	writeV2([]map[string]any{bareHolder, holder, itemHolder},
		bare(502, "unknown", "", "exec", "high"),
		bare(4242, "", waitingItem.ID, "run", "high"),
		bare(9999, "", "", "exec", "normal"),
		bare(503, "verifier-5", "", "exec", "normal"),
		bare(504, "", "wi_not-an-item", "targeted", "low"))
	out = list()
	wantUnattributed := []string{
		"matrix unattributed holder pid=501 kind=exec priority=normal since=2026-10-01T14:02:00.000Z",
		"matrix unattributed waiter position=1 of 5 pid=502 kind=exec priority=high since=2026-10-01T14:06:00.000Z",
		"matrix unattributed waiter position=5 of 5 pid=504 kind=targeted priority=low since=2026-10-01T14:06:00.000Z",
	}
	var gotUnattributed []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "unattributed") {
			gotUnattributed = append(gotUnattributed, line)
		}
	}
	if strings.Join(gotUnattributed, "\n") != strings.Join(wantUnattributed, "\n") {
		t.Fatalf("unattributed lines %q, want %q:\n%s", gotUnattributed, wantUnattributed, out)
	}
	block := strings.Join(wantUnattributed, "\n") + "\n"
	if strings.Count(out, block) != 1 || strings.Index(out, block) < strings.Index(out, remote.ID) || !strings.HasPrefix(out[strings.Index(out, block)+len(block):], "history:") {
		t.Fatalf("the unattributed lines must follow every entry, once:\n%s", out)
	}
	bareHolders := "holder=unknown/unknown/pid 501,wi_holder/verifier-1/pid 777," + remote.ID + "/unknown/pid 505"
	itemLine := "  waiting-for-matrix position=2 of 5 " + bareHolders + " priority=high kind=run agent=unknown since=2026-10-01T14:06:00.000Z"
	if got := lines(out); len(got) != 1 || got[0] != itemLine {
		t.Fatalf("the item's waiter must keep only its entry's line %q, got %q:\n%s", itemLine, got, out)
	}
	if at := strings.Index(out, itemLine); at < strings.Index(out, waitingItem.ID) || at > strings.Index(out, remote.ID) {
		t.Fatalf("the item's line must follow its entry:\n%s", out)
	}
	if got := listJSON(); got != plainJSON {
		t.Fatalf("--json must be unchanged with unattributed runs:\n%s", got)
	}
	// Only dead, named or item-bearing runs: nothing unattributed.
	writeV2([]map[string]any{holder, itemHolder}, bare(9999, "", "", "exec", "normal"), bare(503, "verifier-5", "", "exec", "normal"))
	if out := list(); strings.Contains(out, "unattributed") {
		t.Fatalf("no unattributed run must add no unattributed line:\n%s", out)
	}
	// A version 1 holder with no agent and no item is unattributed too.
	write("Stephens-Mini.local", 1, bareHolder)
	if out := list(); !strings.Contains(out, "\n"+wantUnattributed[0]+"\nhistory:") || strings.Replace(out, wantUnattributed[0]+"\n", "", 1) != plain {
		t.Fatalf("version 1 bare holder:\n%s", out)
	}
	matrixWaitPIDAlive = oldAlive

	// a7: each no-waitlist case is one named line, exit 0, entries intact.
	oneLine := func(what, want string) {
		t.Helper()
		out := list()
		if got := lines(out); len(got) != 1 || got[0] != want {
			t.Fatalf("%s: lines %q, want %q", what, got, want)
		}
		if strings.Replace(out, want+"\n", "", 1) != plain {
			t.Fatalf("%s: the rest of the output must be unchanged:\n%s", what, out)
		}
		if got := listJSON(); got != plainJSON {
			t.Fatalf("%s: --json must be unchanged", what)
		}
	}
	write("TrueNAS.local", 1, holder, waiter(waitingItem.ID, "high", "run", "deployer"))
	oneLine("other host", "matrix host: lock file "+path+" belongs to host TrueNAS.local; waitlist not shown")
	write("Stephens-Mini.local", 3, holder, waiter(waitingItem.ID, "high", "run", "deployer"))
	oneLine("unknown version", "matrix host: lock file "+path+" unusable (unknown version); waitlist not shown")
	if os.WriteFile(path, []byte("{not json"), 0o600) != nil {
		t.Fatal("write")
	}
	oneLine("not JSON", "matrix host: lock file "+path+" unusable (not JSON); waitlist not shown")
	if os.Remove(path) != nil || os.Mkdir(path, 0o700) != nil {
		t.Fatal("directory in place of the file")
	}
	oneLine("unreadable", "matrix host: lock file "+path+" unusable (unreadable); waitlist not shown")
	// A relative override is named and the default path is not used instead.
	home, _ := os.UserHomeDir()
	fallback := filepath.Join(home, ".local/state/tailterm-matrix/host.json")
	if os.MkdirAll(filepath.Dir(fallback), 0o700) != nil {
		t.Fatal("default directory")
	}
	path = fallback
	write("Stephens-Mini.local", 1, holder, waiter(waitingItem.ID, "high", "run", "deployer"))
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", "relative/host.json")
	oneLine("relative override", "matrix host: TAILTERM_MATRIX_HOST_LOCK must be an absolute path; waitlist not shown")
	// With no override the default path under HOME is read.
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", "")
	if got := lines(list()); len(got) != 1 || !strings.HasPrefix(got[0], "  waiting-for-matrix position=1 of 1 holder=wi_holder/verifier-1/pid 777 priority=high") {
		t.Fatalf("default path lines %q", got)
	}

	// Host names: first DNS label, case-insensitive.
	for _, pair := range [][2]string{{"Stephens-Mini", "Stephens-Mini.local"}, {"stephens-mini", "STEPHENS-MINI.tail1234.ts.net"}} {
		if matrixHostLabel(pair[0]) != matrixHostLabel(pair[1]) {
			t.Fatalf("%q and %q must match", pair[0], pair[1])
		}
	}
	if matrixHostLabel("Stephens-Mini") == matrixHostLabel("Stephens-Mini-2.local") {
		t.Fatal("different machines must not match")
	}
	// A failed entry and an entry on another host get no line.
	m := &matrixWaitlist{Version: 1, Host: "Stephens-Mini.local", Waiters: []matrixHostEntry{{PID: 1, Item: "wi_x", Kind: "run", Agent: "a", Priority: "high", RequestedAt: "t"}}}
	if got := m.waitLines(api.TeamQueueEntry{ItemID: "wi_x", Host: "stephens-mini", State: "running"}); len(got) != 1 {
		t.Fatalf("matching entry lines %q", got)
	}
	for _, q := range []api.TeamQueueEntry{{ItemID: "wi_x", Host: "stephens-mini", State: "failed"}, {ItemID: "wi_x", Host: "truenas", State: "running"}, {ItemID: "wi_y", Host: "stephens-mini", State: "running"}} {
		if got := m.waitLines(q); got != nil {
			t.Fatalf("entry %+v printed %q", q, got)
		}
	}
	// A value from the file never breaks the line.
	m.Waiters[0].Agent = "two words\nnext"
	if got := m.waitLines(api.TeamQueueEntry{ItemID: "wi_x", Host: "stephens-mini", State: "running"}); len(got) != 1 || !strings.Contains(got[0], "agent=two_words_next ") {
		t.Fatalf("sanitized line %q", got)
	}
}

// wi_01b6d3afed81167c a9, a10 (CLI): the provisioning switch is set and
// listed, and a limit raised above the available handlers prints a warning.
func TestTeamQueueCLIHandlerProvisionSwitchAndLimitWarning(t *testing.T) {
	f := newTeamFixture(t, true)
	repo, _ := queueGitRepo(t)
	const adds = "warning: limit 2 exceeds 1 available database handler; the runner adds one per waiting team while the agent cap allows"
	if out := parallelCLIProject(t, f, "2"); !strings.Contains(out, "concurrency limit=2") || !strings.Contains(out, adds) {
		t.Fatalf("raised limit output %q", out)
	}
	if out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"limit", "--limit", "1"}) }); err != nil || strings.Contains(out, "warning") {
		t.Fatalf("lowered limit %q %v", out, err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--cwd", repo, "--owns", "client"})
	}); err != nil {
		t.Fatal(err)
	}
	if out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) }); err != nil || !strings.Contains(out, "handler provisioning=on\n") {
		t.Fatalf("default list %q %v", out, err)
	}
	if out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"provision", "--auto", "off"}) }); err != nil || out != "handler provisioning=off\n" {
		t.Fatalf("provision off %q %v", out, err)
	}
	if out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) }); err != nil || !strings.Contains(out, "handler provisioning=off\n") {
		t.Fatalf("off list %q %v", out, err)
	}
	off := "warning: limit 2 exceeds 1 available database handler; automatic provisioning is off, so teams will wait. Fix: tt team queue provision --task " + f.task.ID + " --auto on"
	if out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"limit", "--limit", "2"}) }); err != nil || !strings.Contains(out, off) {
		t.Fatalf("raised limit while off %q %v", out, err)
	}
	if out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"provision", "--auto", "on"}) }); err != nil || out != "handler provisioning=on\n" {
		t.Fatalf("provision on %q %v", out, err)
	}
	for _, bad := range [][]string{{"provision"}, {"provision", "--auto", "maybe"}} {
		if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, bad) }); err == nil || !strings.Contains(err.Error(), "usage: tt team queue provision --auto on|off") {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	// Like the limit, the switch is an owner-side change.
	bound := f.e
	bound.agent = f.handler.ID
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(bound, []string{"provision", "--auto", "off"}) }); err == nil || !strings.Contains(err.Error(), "owner-side team queue changes require an unbound CLI session") {
		t.Fatalf("bound session: %v", err)
	}
}

// wi_2430de4c12e43df4: the lane tt team queue add picks from the item kind,
// the submitted path count, --serial, --template and --planned-reason.
func TestQueueAddTemplate(t *testing.T) {
	for _, tc := range []struct {
		name               string
		kind               string
		owned              int
		serial             bool
		explicit, reason   string
		template, recorded string
		why, refusal       string
	}{
		{name: "bug with one path", kind: "bug", owned: 1, template: "small", why: "bug owning 1 path (default)"},
		{name: "bug with three paths", kind: "bug", owned: 3, template: "small", why: "bug owning 3 paths (default)"},
		{name: "bug with four paths", kind: "bug", owned: 4, template: "planned", recorded: "paths", why: "bug owning 4 paths, more than three (default)"},
		{name: "serial bug", kind: "bug", serial: true, template: "planned", recorded: "unscoped", why: "cannot admit"},
		{name: "unscoped bug", kind: "bug", template: "planned", recorded: "unscoped", why: "cannot admit"},
		{name: "feature", kind: "feature", owned: 2, template: "planned", why: "a feature stays on Planned delivery (default)"},
		{name: "explicit small bug", kind: "bug", owned: 2, explicit: "small", template: "small", why: "--template small"},
		{name: "explicit small is left to the hub", kind: "feature", owned: 9, explicit: "small", template: "small", why: "--template small"},
		{name: "explicit planned feature", kind: "feature", owned: 1, explicit: "planned", template: "planned", why: "--template planned"},
		{name: "explicit planned small bug without a reason", kind: "bug", owned: 2, explicit: "planned", refusal: "--planned-reason schema"},
		{name: "explicit planned small bug with paths", kind: "bug", owned: 3, explicit: "planned", reason: "paths", refusal: "--planned-reason risk:TEXT"},
		{name: "explicit planned small bug with schema", kind: "bug", owned: 2, explicit: "planned", reason: "schema", template: "planned", recorded: "schema", why: "--template planned"},
		{name: "explicit planned small bug with a risk", kind: "bug", owned: 2, explicit: "planned", reason: "risk: shared relay limiter ", template: "planned", recorded: "risk:shared relay limiter", why: "--template planned"},
		{name: "explicit planned wide bug", kind: "bug", owned: 5, explicit: "planned", template: "planned", recorded: "paths", why: "--template planned"},
		{name: "explicit planned wide bug with schema", kind: "bug", owned: 5, explicit: "planned", reason: "schema", template: "planned", recorded: "schema", why: "--template planned"},
		{name: "explicit planned serial bug", kind: "bug", serial: true, explicit: "planned", template: "planned", recorded: "unscoped", why: "--template planned"},
		{name: "paths on a serial bug", kind: "bug", serial: true, explicit: "planned", reason: "paths", refusal: "this entry declares none"},
		{name: "default wide bug with a named risk", kind: "bug", owned: 4, reason: "risk:x", template: "planned", recorded: "risk:x", why: "more than three (default)"},
		{name: "reason on a default small bug", kind: "bug", owned: 2, reason: "schema", refusal: "add --template planned"},
		{name: "reason with explicit small", kind: "bug", owned: 2, explicit: "small", reason: "schema", refusal: "add --template planned"},
		{name: "reason on a feature", kind: "feature", owned: 2, reason: "schema", refusal: "a feature is always Planned"},
		{name: "unknown reason", kind: "bug", owned: 4, reason: "big", refusal: "paths, schema or risk:TEXT"},
		{name: "empty risk", kind: "bug", owned: 2, explicit: "planned", reason: "risk: ", refusal: "paths, schema or risk:TEXT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template, why, recorded, err := queueAddTemplate(tc.kind, tc.owned, tc.serial, tc.explicit, tc.reason)
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) || template != "" || recorded != "" {
					t.Fatalf("got %q %q %q %v, want refusal %q", template, why, recorded, err, tc.refusal)
				}
				return
			}
			if err != nil || template != tc.template || recorded != tc.recorded || !strings.Contains(why, tc.why) {
				t.Fatalf("got %q %q %q %v, want %q %q %q", template, why, recorded, err, tc.template, tc.why, tc.recorded)
			}
		})
	}
}

// laneFixtureItem files one more item of a kind with its own confirmed order;
// owns, when given, is the ownership the handler recorded at intake.
func laneFixtureItem(t *testing.T, f teamFixture, kind, key string, owns ...string) (api.WorkItem, int64) {
	t.Helper()
	ctx := context.Background()
	item, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: kind, Title: key, RequestID: key})
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: key + " bounded order", RequestID: key + "-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.ConfirmWorkOrderScope(ctx, f.task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: key + "-scope", AgentID: f.handler.ID, RunID: f.handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true, Ownership: owns}); err != nil {
		t.Fatal(err)
	}
	return item, order.Seq
}

// laneAdd runs tt team queue add for an item in repo and returns its stdout
// and stderr.
func laneAdd(t *testing.T, e env, item api.WorkItem, order int64, repo string, extra ...string) (string, string, error) {
	t.Helper()
	args := append([]string{"add", "--item", item.ID, "--order", fmt.Sprint(order), "--cwd", repo}, extra...)
	oldErr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	out, runErr := captureCLIOutput(t, func() error { return cmdTeamQueue(e, args) })
	_ = w.Close()
	os.Stderr = oldErr
	stderr, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return out, string(stderr), runErr
}

func laneEntry(t *testing.T, f teamFixture, item string) api.TeamQueueEntry {
	t.Helper()
	list, err := f.c.ListTeamQueue(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found []api.TeamQueueEntry
	for _, q := range list.Entries {
		if q.ItemID == item {
			found = append(found, q)
		}
	}
	if len(found) != 1 {
		t.Fatalf("item %s has %d queue entries: %+v", item, len(found), list.Entries)
	}
	return found[0]
}

// laneNotices returns the lane notices linked to an item.
func laneNotices(t *testing.T, f teamFixture, item string) []api.Message {
	t.Helper()
	messages, err := f.c.ListMessages(context.Background(), f.task.ID, 0, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	var notices []api.Message
	for _, m := range messages {
		if m.Envelope == nil || m.Envelope.Kind != api.EnvelopeKindNotice || m.Envelope.Refs["entry"] == "" {
			continue
		}
		for _, link := range m.WorkItems {
			if link.ItemID == item {
				notices = append(notices, m)
			}
		}
	}
	return notices
}

// a1, a3, a7 and the intake case: a bug owning at most three paths defaults
// to the small-change lane, a feature to Planned delivery, neither leaves a
// notice, and --json keeps stdout to the entry alone.
func TestTeamQueueCLIDefaultLaneSmallBugAndFeature(t *testing.T) {
	f := newTeamFixtureKind(t, true, "bug")
	repo, _ := queueGitRepo(t)
	out, _, err := laneAdd(t, f.e, f.item, f.order, repo, "--owns", "hub/a.go", "--owns", "hub/a_test.go", "--owns", "docs/a.md")
	if err != nil || !strings.Contains(out, "template small: bug owning 3 paths (default)\n") || strings.Contains(out, "reason=") {
		t.Fatalf("three-path bug %q %v", out, err)
	}
	if q := laneEntry(t, f, f.item.ID); q.Template != "small" || len(q.Ownership) != 3 {
		t.Fatalf("three-path bug entry %+v", q)
	}

	intake, intakeOrder := laneFixtureItem(t, f, "bug", "intake-bug", "client/x.js", "tests/x.test.js")
	out, _, err = laneAdd(t, f.e, intake, intakeOrder, repo)
	if err != nil || !strings.Contains(out, "owns client/x.js,tests/x.test.js (from the scope confirmation)\n") || !strings.Contains(out, "template small: bug owning 2 paths (default)\n") {
		t.Fatalf("intake bug %q %v", out, err)
	}
	if q := laneEntry(t, f, intake.ID); q.Template != "small" {
		t.Fatalf("intake bug entry %+v", q)
	}

	feature, featureOrder := laneFixtureItem(t, f, "feature", "lane-feature")
	out, _, err = laneAdd(t, f.e, feature, featureOrder, repo, "--owns", "scripts/one.mjs")
	if err != nil || !strings.Contains(out, "template planned: a feature stays on Planned delivery (default)\n") || strings.Contains(out, "reason=") {
		t.Fatalf("feature %q %v", out, err)
	}
	if q := laneEntry(t, f, feature.ID); q.Template != "planned" {
		t.Fatalf("feature entry %+v", q)
	}

	jsonBug, jsonOrder := laneFixtureItem(t, f, "bug", "json-bug")
	out, stderr, err := laneAdd(t, f.e, jsonBug, jsonOrder, repo, "--json", "--owns", "web/a.js", "--owns", "web/b.js", "--owns", "web/c.js")
	var saved api.TeamQueueEntry
	if err != nil || json.Unmarshal([]byte(out), &saved) != nil || saved.ItemID != jsonBug.ID || saved.Template != "small" {
		t.Fatalf("json add stdout %q %v", out, err)
	}
	if strings.Contains(out, "template small:") || stderr != "template small: bug owning 3 paths (default)\n" {
		t.Fatalf("json add stdout %q stderr %q", out, stderr)
	}

	for _, item := range []string{f.item.ID, intake.ID, feature.ID, jsonBug.ID} {
		if notices := laneNotices(t, f, item); len(notices) != 0 {
			t.Fatalf("%s has lane notices %+v", item, notices)
		}
	}
}

// a2, a5: a bug the small-change lane cannot admit falls back to Planned
// delivery with the reason printed and recorded in one linked notice.
func TestTeamQueueCLIDefaultLanePlannedBugRecordsReason(t *testing.T) {
	f := newTeamFixtureKind(t, true, "bug")
	repo, _ := queueGitRepo(t)
	out, _, err := laneAdd(t, f.e, f.item, f.order, repo, "--owns", "hub/a.go", "--owns", "hub/a_test.go", "--owns", "docs/a.md", "--owns", "client/a.js")
	if err != nil || !strings.Contains(out, "template planned: bug owning 4 paths, more than three (default) reason=paths\n") {
		t.Fatalf("four-path bug %q %v", out, err)
	}
	wide := laneEntry(t, f, f.item.ID)
	serialBug, serialOrder := laneFixtureItem(t, f, "bug", "serial-bug")
	out, _, err = laneAdd(t, f.e, serialBug, serialOrder, repo, "--serial")
	if err != nil || !strings.Contains(out, "template planned: bug with no owned paths, which the small-change lane cannot admit (default) reason=unscoped\n") {
		t.Fatalf("serial bug %q %v", out, err)
	}
	serial := laneEntry(t, f, serialBug.ID)
	for _, want := range []struct {
		entry  api.TeamQueueEntry
		item   api.WorkItem
		order  int64
		reason string
	}{{wide, f.item, f.order, "reason=paths (more than three owned paths)"}, {serial, serialBug, serialOrder, "reason=unscoped (no owned paths"}} {
		if want.entry.Template != "planned" {
			t.Fatalf("entry %+v is not planned", want.entry)
		}
		notices := laneNotices(t, f, want.item.ID)
		if len(notices) != 1 {
			t.Fatalf("%s has %d lane notices: %+v", want.item.ID, len(notices), notices)
		}
		n := notices[0]
		if len(n.WorkItems) != 1 || n.WorkItems[0].ItemRevision != want.item.Revision || n.WorkItems[0].Relationship != "primary" || n.WorkOrderMessage == nil || n.WorkOrderMessage.Seq != want.order {
			t.Fatalf("notice links %+v order %+v", n.WorkItems, n.WorkOrderMessage)
		}
		if n.Envelope.Refs["entry"] != want.entry.ID || !strings.Contains(n.Text, want.entry.ID) || !strings.Contains(n.Text, "template planned") || !strings.Contains(n.Text, want.reason) {
			t.Fatalf("notice %q refs %v", n.Text, n.Envelope.Refs)
		}
	}
}

// a4: an explicit --template always wins, but Planned delivery for a bug the
// small-change lane would take needs a named reason, refused before any
// entry or worktree exists; --template small still meets the hub's refusals.
func TestTeamQueueCLIDefaultLaneExplicitTemplate(t *testing.T) {
	f := newTeamFixtureKind(t, true, "bug")
	repo, _ := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	t.Chdir(repo)
	worktree := filepath.Join(repo, ".build", "worktrees", "queue-"+strings.TrimPrefix(f.item.ID, "wi_")[:8])
	for _, refused := range [][]string{
		{"--template", "planned"},
		{"--template", "planned", "--planned-reason", "paths"},
		{"--template", "planned", "--planned-reason", "risk:"},
		{"--planned-reason", "schema"},
	} {
		args := append([]string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--owns", "hub/a.go", "--owns", "docs/a.md"}, refused...)
		if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, args) }); err == nil || !strings.Contains(err.Error(), "--planned-reason") {
			t.Fatalf("%v: %v", refused, err)
		}
		if list, err := f.c.ListTeamQueue(context.Background(), f.task.ID); err != nil || len(list.Entries) != 0 {
			t.Fatalf("%v queued %+v %v", refused, list, err)
		}
		if _, err := os.Lstat(worktree); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%v left worktree %s: %v", refused, worktree, err)
		}
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"requeue", "--entry", "tqe_0000000000000000", "--planned-reason", "schema"})
	}); err == nil || !strings.Contains(err.Error(), "--planned-reason applies only to tt team queue add") {
		t.Fatalf("requeue with a reason: %v", err)
	}
	out, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--owns", "hub/a.go", "--owns", "docs/a.md", "--template", "planned", "--planned-reason", "risk:touches every relay binding"})
	})
	if err != nil || !strings.Contains(out, "template planned: --template planned reason=risk:touches every relay binding\n") {
		t.Fatalf("planned with a risk %q %v", out, err)
	}
	q := laneEntry(t, f, f.item.ID)
	notices := laneNotices(t, f, f.item.ID)
	if q.Template != "planned" || len(notices) != 1 || !strings.Contains(notices[0].Text, "a cross-cutting risk: touches every relay binding") || notices[0].Envelope.Refs["entry"] != q.ID {
		t.Fatalf("entry %+v notices %+v", q, notices)
	}

	schema, schemaOrder := laneFixtureItem(t, f, "bug", "schema-bug")
	out, _, err = laneAdd(t, f.e, schema, schemaOrder, repo, "--owns", "hub/store.go", "--template", "planned", "--planned-reason", "schema")
	if err != nil || !strings.Contains(out, "template planned: --template planned reason=schema\n") {
		t.Fatalf("planned with schema %q %v", out, err)
	}
	if q, notices := laneEntry(t, f, schema.ID), laneNotices(t, f, schema.ID); q.Template != "planned" || len(notices) != 1 || !strings.Contains(notices[0].Text, "reason=schema (a schema or migration change)") {
		t.Fatalf("schema entry %+v notices %+v", q, notices)
	}

	// --template small is sent as given; the hub alone admits or refuses.
	feature, featureOrder := laneFixtureItem(t, f, "feature", "small-feature")
	if _, _, err := laneAdd(t, f.e, feature, featureOrder, repo, "--owns", "web/one.js", "--template", "small"); err == nil || !strings.Contains(err.Error(), "the small-change lane admits only bugs") {
		t.Fatalf("small feature: %v", err)
	}
	wide, wideOrder := laneFixtureItem(t, f, "bug", "wide-small-bug")
	if _, _, err := laneAdd(t, f.e, wide, wideOrder, repo, "--owns", "w/1", "--owns", "w/2", "--owns", "w/3", "--owns", "w/4", "--template", "small"); err == nil || !strings.Contains(err.Error(), "the small-change lane owns at most 3 paths") {
		t.Fatalf("wide small bug: %v", err)
	}
	small, smallOrder := laneFixtureItem(t, f, "bug", "explicit-small-bug")
	out, _, err = laneAdd(t, f.e, small, smallOrder, repo, "--owns", "s/1", "--template", "small")
	if err != nil || !strings.Contains(out, "template small: --template small\n") || laneEntry(t, f, small.ID).Template != "small" {
		t.Fatalf("explicit small bug %q %v", out, err)
	}
	for _, item := range []string{feature.ID, wide.ID, small.ID} {
		if notices := laneNotices(t, f, item); len(notices) != 0 {
			t.Fatalf("%s has lane notices %+v", item, notices)
		}
	}
}

// laneNoticeProxy fronts the fixture hub and answers every message post with
// status and body, recording each refused request.
func laneNoticeProxy(t *testing.T, hub string, status int, body string) (env, *[]api.PostMessageRequest) {
	t.Helper()
	target, err := url.Parse(hub)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	var refused []api.PostMessageRequest
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") {
			var req api.PostMessageRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("notice body: %v", err)
			}
			refused = append(refused, req)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	return env{hub: proxy.URL}, &refused
}

// laneSendArgs finds the one tt send command in text and splits it as the
// shell does, returning the arguments after "tt send".
func laneSendArgs(t *testing.T, text string) []string {
	t.Helper()
	var command string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "tt send ") {
			if command != "" {
				t.Fatalf("more than one tt send command:\n%s", text)
			}
			command = line
		}
	}
	if command == "" {
		t.Fatalf("no tt send command:\n%s", text)
	}
	out, err := exec.Command("sh", "-c", `eval "set -- $1"; shift 2; printf '%s\0' "$@"`, "sh", command).Output()
	if err != nil {
		t.Fatalf("shell cannot parse %q: %v", command, err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
}

// q2 (a2): when the hub refuses the notice the entry stays queued and add
// fails, naming the entry and one typed command. That command, parsed by the
// shell and tt send's own flags, posts the request add sent, so the hub
// stores the notice once however often it is run.
func TestTeamQueueCLIDefaultLaneNoticeFailureKeepsEntry(t *testing.T) {
	f := newTeamFixtureKind(t, true, "bug")
	repo, _ := queueGitRepo(t)
	broken, refused := laneNoticeProxy(t, f.e.hub, http.StatusServiceUnavailable, `{"error":"board unavailable"}`)
	broken.task = f.e.task
	_, _, err := laneAdd(t, broken, f.item, f.order, repo, "--owns", "a", "--owns", "b", "--owns", "c", "--owns", "d")
	q := laneEntry(t, f, f.item.ID)
	if q.Template != "planned" || q.State != "queued" {
		t.Fatalf("entry after a refused notice %+v", q)
	}
	if err == nil {
		t.Fatal("add succeeded without its reason record")
	}
	for _, want := range []string{"entry " + q.ID + " was added", "its Planned reason was not recorded", "board unavailable", fmt.Sprintf("order #%d", f.order)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "NOTICE:") {
		t.Fatalf("error still asks for the rendered text by hand:\n%v", err)
	}
	if notices := laneNotices(t, f, f.item.ID); len(notices) != 0 {
		t.Fatalf("refused notice was stored: %+v", notices)
	}
	if len(*refused) != 1 {
		t.Fatalf("add posted %d notices", len(*refused))
	}
	original := (*refused)[0]
	args := laneSendArgs(t, err.Error())
	for flag, want := range map[string]string{"--kind": "notice", "--request-id": "queue-lane-" + q.ID, "--work-item": f.item.ID, "--work-item-revision": fmt.Sprint(f.item.Revision), "--work-order-message": fmt.Sprint(f.order), "--task": f.task.ID} {
		if i := indexOf(args, flag); i < 0 || i+1 >= len(args) || args[i+1] != want {
			t.Fatalf("command %q: %s is not %q", args, flag, want)
		}
	}
	// The same request byte for byte: the hub keys a repost by request id
	// and refuses one whose data differs.
	repost, reposted := laneNoticeProxy(t, f.e.hub, http.StatusCreated, `{"seq":1}`)
	if _, err := captureCLIOutput(t, func() error { return cmdSend(repost, args) }); err != nil {
		t.Fatalf("tt send %q: %v", args, err)
	}
	want, _ := json.Marshal(original)
	got, _ := json.Marshal((*reposted)[0])
	if original.Envelope == nil || original.Envelope.Kind != api.EnvelopeKindNotice || original.AuditKind != api.MessageAuditWork || !bytes.Equal(got, want) {
		t.Fatalf("repost differs from add's post:\n got %s\nwant %s", got, want)
	}
	for range 2 {
		if out, err := captureCLIOutput(t, func() error { return cmdSend(f.e, args) }); err != nil || !strings.HasPrefix(out, "posted #") {
			t.Fatalf("tt send to the hub %q %v", out, err)
		}
	}
	notices := laneNotices(t, f, f.item.ID)
	if len(notices) != 1 {
		t.Fatalf("reposts stored %d notices: %+v", len(notices), notices)
	}
	n := notices[0]
	if n.Envelope.Kind != api.EnvelopeKindNotice || n.Envelope.Refs["entry"] != q.ID || len(n.WorkItems) != 1 || n.WorkItems[0].ItemRevision != f.item.Revision || n.WorkOrderMessage == nil || n.WorkOrderMessage.Seq != f.order {
		t.Fatalf("stored notice %+v", n)
	}
}

func indexOf(values []string, want string) int {
	for i, v := range values {
		if v == want {
			return i
		}
	}
	return -1
}

// q3 (a3): when the hub refuses only the notice, for work the caller has not
// acknowledged, the entry is queued: add warns on stderr naming the refused
// messages and the a2 command, prints the entry and succeeds.
func TestTeamQueueCLILaneNoticeUnacknowledgedRefusalWarns(t *testing.T) {
	f := newTeamFixtureKind(t, true, "bug")
	repo, _ := queueGitRepo(t)
	refusal := &api.UnacknowledgedError{Items: []api.UnacknowledgedItem{{Seq: 41, From: "lead", Subject: "Fix the add path"}, {Seq: 43, From: "the owner", Subject: "Answer #7 first"}}}
	body, _ := json.Marshal(api.ErrorResponse{Error: refusal.Error(), Code: "unacknowledged"})
	gated, _ := laneNoticeProxy(t, f.e.hub, http.StatusConflict, string(body))
	gated.task = f.e.task
	out, stderr, err := laneAdd(t, gated, f.item, f.order, repo, "--owns", "a", "--owns", "b", "--owns", "c", "--owns", "d")
	if err != nil {
		t.Fatalf("add failed although the entry is queued: %v", err)
	}
	q := laneEntry(t, f, f.item.ID)
	if q.State != "queued" || !strings.Contains(out, "add "+q.ID+" "+f.item.ID+" at ") {
		t.Fatalf("entry %+v, stdout %q", q, out)
	}
	for _, want := range []string{"warning: entry " + q.ID + " is queued", "its Planned reason was not recorded", "unacknowledged work #41, #43", "tt ack SEQ"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if args := laneSendArgs(t, stderr); args[indexOf(args, "--request-id")+1] != "queue-lane-"+q.ID {
		t.Fatalf("warning command %q", args)
	}
	if got := unacknowledgedSeqs(errors.New("hub: 409 unacknowledged")); got != nil {
		t.Fatalf("a plain error names seqs %q", got)
	}
}

// q3: an agent session cannot add an entry at all; it is refused before any
// worktree or hub write, so the hub's unacknowledged-work refusal never
// meets a saved entry.
func TestTeamQueueCLIAddFromAgentSessionRefusedBeforeWrites(t *testing.T) {
	f := newTeamFixture(t, true)
	repo, _ := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	t.Chdir(repo)
	agent := f.e
	agent.agent, agent.runID = f.handler.ID, f.handler.RunID
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(agent, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--owns", "client", "--new-worktree"})
	}); err == nil || !strings.Contains(err.Error(), "require an unbound CLI session") {
		t.Fatalf("agent add: %v", err)
	}
	requireNoQueueWorktree(t, repo, queueWorktreeFor(repo, f.item.ID), "agent add")
	if list, err := f.c.ListTeamQueue(context.Background(), f.task.ID); err != nil || len(list.Entries) != 0 {
		t.Fatalf("agent add saved %+v %v", list, err)
	}
}

// q1 (review b1): rerunning add for an item whose earlier entry failed, its
// worktree since removed and the checkout's HEAD unchanged, keeps the hub's
// refusal and removes the new worktree: only a queued first attempt is an
// entry this add could have saved.
func TestTeamQueueCLIRerunAddOverFailedEntryKeepsRefusal(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	repo, _ := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	t.Chdir(repo)
	item, order := queueFixtureItem(t, f, "rerun-failed")
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", item.ID, "--order", fmt.Sprint(order), "--owns", "client"})
	}); err != nil {
		t.Fatal(err)
	}
	list, err := f.c.ListTeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{Item: item.ID})
	if err != nil || len(list.Entries) != 1 || list.Entries[0].Attempt > 1 {
		t.Fatalf("first add %+v %v", list, err)
	}
	q := list.Entries[0]
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "rerun-fail", Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: "fixture failure"}); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", repo, "worktree", "remove", "--force", q.Cwd).Run(); err != nil {
		t.Fatal(err)
	}
	refusal, wantErr := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "add", ItemID: item.ID, OrderMessageSeq: order, Host: q.Host, Cwd: q.Cwd, Repository: q.Repository, BaseCommit: q.BaseCommit, Ownership: q.Ownership})
	if wantErr == nil {
		t.Fatalf("hub accepted a second add %+v", refusal)
	}
	out, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", item.ID, "--order", fmt.Sprint(order), "--owns", "client"})
	})
	if err == nil || err.Error() != wantErr.Error() || !strings.Contains(out, "worktree "+q.Cwd) {
		t.Fatalf("rerun over a failed entry: out=%q err=%v, want the hub's %v", out, err, wantErr)
	}
	requireNoQueueWorktree(t, repo, q.Cwd, "rerun over a failed entry")
	after, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID)
	if err != nil || after.State != "failed" {
		t.Fatalf("failed entry changed %+v %v", after, err)
	}
}

// queueAddProxy fronts the fixture hub; add handles each add request, with
// the real hub as forward, and lookup each read of one item's entry.
func queueAddProxy(t *testing.T, hub string, add, lookup func(w http.ResponseWriter, r *http.Request, forward http.Handler)) env {
	t.Helper()
	target, err := url.Parse(hub)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/team-queue/actions") {
			body, _ := io.ReadAll(r.Body)
			r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
			var req api.TeamQueueRequest
			if json.Unmarshal(body, &req) == nil && req.Operation == "add" {
				add(w, r, forward)
				return
			}
		}
		if r.Method == "GET" && r.URL.Query().Get("item") != "" && lookup != nil {
			lookup(w, r, forward)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return env{hub: srv.URL}
}

// q1 (a1): an add the hub saved but whose response was lost keeps the new
// worktree the saved entry names and reports the entry; with nothing saved,
// or when the read-back fails too, the worktree is removed for a retry.
func TestTeamQueueCLIAddKeepsWorktreeAfterLostSavedResponse(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	repo, head := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	t.Chdir(repo)
	saveThenDrop := func(w http.ResponseWriter, r *http.Request, forward http.Handler) {
		forward.ServeHTTP(httptest.NewRecorder(), r)
		dropQueueResponse(w)
	}
	for _, attempt := range []struct {
		name, key   string
		add, lookup func(http.ResponseWriter, *http.Request, http.Handler)
	}{
		{"response dropped before the hub", "dropped", func(w http.ResponseWriter, _ *http.Request, _ http.Handler) { dropQueueResponse(w) }, nil},
		{"saved, but the response and the read-back lost", "lookup-lost", saveThenDrop, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) { dropQueueResponse(w) }},
	} {
		item, order := queueFixtureItem(t, f, attempt.key)
		lost := queueAddProxy(t, f.e.hub, attempt.add, attempt.lookup)
		lost.task = f.e.task
		out, err := captureCLIOutput(t, func() error {
			return cmdTeamQueue(lost, []string{"add", "--item", item.ID, "--order", fmt.Sprint(order), "--owns", "client"})
		})
		path := queueWorktreeFor(repo, item.ID)
		if err == nil || !strings.Contains(out, "worktree "+path) {
			t.Fatalf("%s: add %q %v", attempt.name, out, err)
		}
		requireNoQueueWorktree(t, repo, path, attempt.name)
		if attempt.lookup == nil {
			continue
		}
		// The saved entry names the removed worktree. Rerunning the add
		// recreates it, the hub refuses the duplicate, and the read-back
		// keeps it.
		if _, err := captureCLIOutput(t, func() error {
			return cmdTeamQueue(f.e, []string{"add", "--item", item.ID, "--order", fmt.Sprint(order), "--owns", "client"})
		}); err != nil {
			t.Fatalf("%s: rerun: %v", attempt.name, err)
		}
		if sha, err := queueGitCommit(path); err != nil || sha != head {
			t.Fatalf("%s: rerun worktree HEAD %s %v", attempt.name, sha, err)
		}
	}
	item, order := queueFixtureItem(t, f, "saved-lost")
	lost := queueAddProxy(t, f.e.hub, saveThenDrop, nil)
	lost.task = f.e.task
	want := queueWorktreeFor(repo, item.ID)
	out, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(lost, []string{"add", "--item", item.ID, "--order", fmt.Sprint(order), "--owns", "client"})
	})
	if err != nil {
		t.Fatalf("saved add with a lost response failed: %v", err)
	}
	list, err := f.c.ListTeamQueuePage(ctx, f.task.ID, api.TeamQueueListOptions{Item: item.ID})
	if err != nil || len(list.Entries) != 1 {
		t.Fatalf("saved entries %+v %v", list, err)
	}
	q := list.Entries[0]
	if real, _ := filepath.EvalSymlinks(q.Cwd); real != want || !strings.Contains(out, "worktree "+want) || !strings.Contains(out, "add "+q.ID+" "+item.ID+" at ") {
		t.Fatalf("entry %+v, output %q", q, out)
	}
	if sha, err := queueGitCommit(want); err != nil || sha != head {
		t.Fatalf("kept worktree HEAD %s %v", sha, err)
	}
}

// a7 (review b1): with --json every line add prints itself, the intake owns
// line, the worktree line and the lane line, goes to stderr, so stdout is
// exactly the entry JSON.
func TestTeamQueueCLIDefaultLaneJSONStdoutIsOnlyTheEntry(t *testing.T) {
	f := newTeamFixtureKind(t, true, "bug")
	repo, _ := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	t.Chdir(repo)
	item, order := laneFixtureItem(t, f, "bug", "json-intake-bug", "client/x.js", "tests/x.test.js")
	worktree := filepath.Join(repo, ".build", "worktrees", "queue-"+strings.TrimPrefix(item.ID, "wi_")[:8])
	oldErr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	out, runErr := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", item.ID, "--order", fmt.Sprint(order), "--json"})
	})
	_ = w.Close()
	os.Stderr = oldErr
	stderr, readErr := io.ReadAll(r)
	_ = r.Close()
	if runErr != nil || readErr != nil {
		t.Fatalf("json add %v %v", runErr, readErr)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	var saved api.TeamQueueEntry
	if err := dec.Decode(&saved); err != nil || saved.ItemID != item.ID || saved.Template != "small" {
		t.Fatalf("stdout is not the entry: %q %v", out, err)
	}
	if realCwd, _ := filepath.EvalSymlinks(saved.Cwd); realCwd != worktree {
		t.Fatalf("entry cwd %s, want the new worktree %s", saved.Cwd, worktree)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout holds more than one JSON value: %q", out)
	}
	for _, line := range []string{"owns client/x.js,tests/x.test.js (from the scope confirmation)\n", "worktree ", "template small: bug owning 2 paths (default)\n"} {
		if strings.Contains(out, line) || !strings.Contains(string(stderr), line) {
			t.Fatalf("%q belongs on stderr only\nstdout %q\nstderr %q", line, out, stderr)
		}
	}
}

// a9: the text queue list labels an entry admitted on a lane default, shows
// nothing of the kind once the item has a saved estimate, and prints a held
// line for an entry whose team is held.
func TestTeamQueueListDefaultEstimateAndHold(t *testing.T) {
	f := newBudgetCLIFixture(t)
	list := func() string {
		t.Helper()
		out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) })
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := list(); strings.Contains(out, "default estimate") || strings.Contains(out, "held:") {
		t.Fatalf("no defaults set: %s", out)
	}
	if _, err := f.usage("defaults", "set", "--small", "18000000", "--small-race", "17000000", "--planned", "44000000", "--planned-race", "70000000"); err != nil {
		t.Fatal(err)
	}
	// The fixture entry is Planned and declares no ownership, so its
	// verification runs everything, the Go race check included.
	if out := list(); !strings.Contains(out, "\n  budget: no estimate · lifetime actual not measured · default estimate 70.00M (planned, Go race)\n") || strings.Contains(out, "held:") {
		t.Fatalf("lane default label: %s", out)
	}
	if got := formatEstimateDefault(&api.TeamQueueEstimateDefault{Tokens: 18_000_000, Lane: "small"}); got != " · default estimate 18.00M (small)" || formatEstimateDefault(nil) != "" {
		t.Fatalf("label %q", got)
	}
	f.estimate(2_000_000)
	if out := list(); strings.Contains(out, "default estimate") || !strings.Contains(out, "\n  budget: estimate 2.00M · lifetime actual not measured\n") {
		t.Fatalf("a saved estimate still shows a default: %s", out)
	}
	f.held()
	out := list()
	if strings.Contains(out, "default estimate") || !strings.Contains(out, "\n  held: past 3 times its token estimate since ") || !strings.Contains(out, "(team 3.0k, estimate 1.0k); no new turns start. Continue: tt usage hold continue --project "+f.task.ID+" --entry "+f.entry.ID+". Stop, from an unbound owner shell: tt team queue fail --task "+f.task.ID+" --entry "+f.entry.ID+" --reason TEXT\n") {
		t.Fatalf("held line: %s", out)
	}
	if !strings.Contains(out, "reason=Held past 3 times its token estimate") {
		t.Fatalf("held reason: %s", out)
	}
	var listed api.TeamQueueList
	text, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list", "--json"}) })
	if err != nil || json.Unmarshal([]byte(text), &listed) != nil || len(listed.Entries) != 1 || listed.Entries[0].BudgetHold == nil || listed.Entries[0].EstimateDefault != nil || listed.Entries[0].AdmittedAt == "" {
		t.Fatalf("json list %s %v", text, err)
	}
}

// wi_44cabd7e6233bc1b a1 and a2: tt team queue wait set and clear round trip
// against a test hub. The owner and a database handler may record a wait, a
// bound non-handler session may not, and the list prints the wait's sentence
// under the waiting entry and in its reason.
func TestTeamQueueCLIWaitSetListAndClear(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	repo, head := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	second, secondOrder := queueFixtureItem(t, f, "waits-behind")
	add := func(key, item string, order int64) api.TeamQueueEntry {
		t.Helper()
		q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: key, Operation: "add", ItemID: item, OrderMessageSeq: order, Host: spawn.Host(), Cwd: repo, Repository: filepath.Join(repo, ".git"), BaseCommit: head, Serial: true})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	pred, waiting := add("wait-pred", f.item.ID, f.order), add("wait-waiting", second.ID, secondOrder)
	run := func(e env, args ...string) (string, error) {
		t.Helper()
		return captureCLIOutput(t, func() error { return cmdTeamQueue(e, append([]string{"wait"}, args...)) })
	}
	for name, args := range map[string][]string{
		"no verb":              {},
		"an unknown verb":      {"show", "--entry", waiting.ID, "--on", pred.ID},
		"set without a path":   {"set", "--entry", waiting.ID, "--on", pred.ID, "--until", "accepted"},
		"set without until":    {"set", "--entry", waiting.ID, "--on", pred.ID, "--owns", "hub/a.go"},
		"set with a bad until": {"set", "--entry", waiting.ID, "--on", pred.ID, "--owns", "hub/a.go", "--until", "merged"},
		"set without --on":     {"set", "--entry", waiting.ID, "--owns", "hub/a.go", "--until", "accepted"},
		"set with a reason":    {"set", "--entry", waiting.ID, "--on", pred.ID, "--owns", "hub/a.go", "--until", "accepted", "--reason", "x"},
		"clear with a path":    {"clear", "--entry", waiting.ID, "--on", pred.ID, "--owns", "hub/a.go"},
		"clear with until":     {"clear", "--entry", waiting.ID, "--on", pred.ID, "--until", "accepted"},
		"clear without --on":   {"clear", "--entry", waiting.ID},
	} {
		if _, err := run(f.e, args...); err == nil || !strings.Contains(err.Error(), "usage: tt team queue wait set --entry tqe_WAITING --on tqe_PREDECESSOR") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"scope", "--entry", waiting.ID, "--owns", "hub", "--on", pred.ID})
	}); err == nil || !strings.Contains(err.Error(), "--on and --until apply only to tt team queue wait") {
		t.Fatalf("--on outside wait: %v", err)
	}
	if !strings.Contains(teamQueueUsage, "|wait|") {
		t.Fatalf("usage lacks wait: %s", teamQueueUsage)
	}

	sentence := fmt.Sprintf("waits on %s for hub/cmd/tt/tool_ledger_test.go, docs/project-queue.md until accepted", f.item.ID)
	out, err := run(f.e, "set", "--entry", waiting.ID, "--on", pred.ID, "--owns", "hub/cmd/tt/tool_ledger_test.go", "--owns", "docs/project-queue.md", "--until", "accepted")
	if err != nil || out != fmt.Sprintf("wait set %s %s: %s\n", waiting.ID, second.ID, sentence) {
		t.Fatalf("owner wait set: %q %v", out, err)
	}
	if got, _ := f.c.GetTeamQueueEntry(ctx, f.task.ID, waiting.ID); got.Revision != waiting.Revision {
		t.Fatalf("wait set moved the entry revision %d to %d", waiting.Revision, got.Revision)
	}
	list, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) })
	if err != nil {
		t.Fatal(err)
	}
	var entryLine string
	lines := strings.Split(list, "\n")
	for i, line := range lines {
		if strings.Contains(line, " "+waiting.ID+" ") {
			entryLine = line
			if i+1 >= len(lines) || lines[i+1] != "  "+sentence {
				t.Fatalf("list does not print the wait under its entry:\n%s", list)
			}
		}
	}
	if !strings.Contains(entryLine, "reason=") || !strings.Contains(entryLine, sentence) || strings.Count(list, sentence) != 2 {
		t.Fatalf("list reason lacks the wait, or another entry shows it:\n%s", list)
	}
	jsonList, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var decoded api.TeamQueueList
	if err := json.Unmarshal([]byte(jsonList), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, q := range decoded.Entries {
		if q.ID == pred.ID && len(q.Waits) != 0 {
			t.Fatalf("predecessor carries waits: %+v", q.Waits)
		}
		if q.ID != waiting.ID {
			continue
		}
		if len(q.Waits) != 1 || q.Waits[0].OnItemID != f.item.ID || q.Waits[0].OnEntryID != pred.ID || q.Waits[0].Until != "accepted" || q.Waits[0].State != "waiting" || strings.Join(q.Waits[0].Paths, ",") != "hub/cmd/tt/tool_ledger_test.go,docs/project-queue.md" {
			t.Fatalf("json waits %+v", q.Waits)
		}
	}
	for _, field := range []string{`"waits"`, `"onItemId"`, `"onEntryId"`, `"paths"`, `"until"`, `"state": "waiting"`} {
		if !strings.Contains(jsonList, field) {
			t.Fatalf("list json lacks %s", field)
		}
	}

	// A bound session that is not a database handler is refused before the hub is asked to write.
	worker, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "worker", Host: "fixture", Session: "worker", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	workerEnv := f.e
	workerEnv.agent, workerEnv.runID = worker.ID, worker.RunID
	if _, err := run(workerEnv, "clear", "--entry", waiting.ID, "--on", pred.ID); err == nil || !strings.Contains(err.Error(), "only the owner or a database handler may set or clear a shared-path wait") {
		t.Fatalf("worker wait clear: %v", err)
	}
	handlerEnv := f.e
	handlerEnv.agent, handlerEnv.runID = f.handler.ID, f.handler.RunID
	staleEnv := handlerEnv
	staleEnv.runID = api.NewID("run")
	if _, err := run(staleEnv, "clear", "--entry", waiting.ID, "--on", pred.ID); err == nil || !strings.Contains(err.Error(), "only the owner or a database handler") {
		t.Fatalf("stale handler run cleared a wait: %v", err)
	}
	out, err = run(handlerEnv, "clear", "--entry", waiting.ID, "--on", pred.ID, "--reason", "predecessor was requeued")
	if err != nil || out != fmt.Sprintf("wait clear %s %s: no longer waits on entry %s\n", waiting.ID, second.ID, pred.ID) {
		t.Fatalf("handler wait clear: %q %v", out, err)
	}
	if list, err = captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) }); err != nil || strings.Contains(list, "waits on") {
		t.Fatalf("cleared wait still listed: %v\n%s", err, list)
	}
	if _, err := run(f.e, "clear", "--entry", waiting.ID, "--on", pred.ID); err == nil {
		t.Fatal("clearing a cleared wait succeeded")
	}
	out, err = run(handlerEnv, "set", "--entry", waiting.ID, "--on", pred.ID, "--owns", "hub/a.go", "--until", "released")
	if err != nil || !strings.Contains(out, fmt.Sprintf("waits on %s for hub/a.go until released", f.item.ID)) {
		t.Fatalf("handler wait set: %q %v", out, err)
	}
	if _, err := run(f.e, "set", "--entry", waiting.ID, "--on", pred.ID, "--owns", "hub/b.go", "--until", "released"); err == nil || !strings.Contains(err.Error(), "clear it first") {
		t.Fatalf("conflicting wait set: %v", err)
	}
}

// q1, q2 (wi_09e8be006c508126): a requeue without --cwd copies the failed
// entry's checkout and base, so the CLI refuses a checkout that was cleaned, a
// checkout it cannot check from another host, and a base that is not the
// checkout's HEAD. Each refusal queues nothing; --cwd DIR is the fix it names.
func TestTeamQueueCLIRequeueChecksCopiedCheckoutAndBase(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	repo, base := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	git := func(dir string, args ...string) string {
		t.Helper()
		output, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	// failedEntry queues an item on host in its own worktree at the base
	// commit, then fails and releases the entry.
	failedEntry := func(key, item string, order int64, host string) api.TeamQueueEntry {
		t.Helper()
		worktree := filepath.Join(repo, ".build", "worktrees", key)
		git(repo, "worktree", "add", "-q", "--detach", worktree, base)
		q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: key + "-add", Operation: "add", ItemID: item, OrderMessageSeq: order, Host: host, Cwd: worktree, Repository: filepath.Join(repo, ".git"), BaseCommit: base, Serial: true})
		if err != nil {
			t.Fatal(err)
		}
		if q, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: key + "-fail", Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: "fixture failure"}); err != nil {
			t.Fatal(err)
		}
		if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"release", "--entry", q.ID}) }); err != nil {
			t.Fatal(err)
		}
		if q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); err != nil {
			t.Fatal(err)
		}
		return q
	}
	requeue := func(q api.TeamQueueEntry, extra ...string) (string, error) {
		t.Helper()
		return captureCLIOutput(t, func() error {
			return cmdTeamQueue(f.e, append([]string{"requeue", "--entry", q.ID}, extra...))
		})
	}
	attempts := func(item string) []api.TeamQueueEntry {
		t.Helper()
		list, err := f.c.ListTeamQueue(ctx, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		var found []api.TeamQueueEntry
		for _, e := range list.Entries {
			if e.ItemID == item {
				found = append(found, e)
			}
		}
		return found
	}
	refused := func(why string, q api.TeamQueueEntry, err error, wants ...string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: the requeue was accepted", why)
		}
		for _, want := range wants {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: %q lacks %q", why, err, want)
			}
		}
		if got := attempts(q.ItemID); len(got) != 1 || got[0].ID != q.ID {
			t.Fatalf("%s queued an attempt: %+v", why, got)
		}
	}

	// Another host cannot check the checkout, with or without it on disk.
	elsewhereItem, elsewhereOrder := queueFixtureItem(t, f, "requeue-elsewhere")
	elsewhere := failedEntry("queue-elsewhere", elsewhereItem.ID, elsewhereOrder, "another-host")
	_, err := requeue(elsewhere)
	refused("another host", elsewhere, err, "launches on another-host", "requeue it from another-host", "or pass --cwd DIR there")

	// The copied checkout exists, but its HEAD moved past the copied base.
	q := failedEntry("queue-main", f.item.ID, f.order, spawn.Host())
	git(q.Cwd, "commit", "-q", "--allow-empty", "-m", "moved")
	moved := git(q.Cwd, "rev-parse", "HEAD")
	_, err = requeue(q)
	refused("HEAD differs from the base", q, err, "base "+base, "HEAD "+moved, "may hold the failed attempt's unreviewed commits", "fresh worktree at the current tasks-hub tip", "pass --cwd DIR")
	if strings.Contains(err.Error(), "pass --cwd "+q.Cwd) {
		t.Fatalf("the stale base refusal offers the copied checkout's HEAD as the base: %v", err)
	}

	// The copied checkout was cleaned.
	git(repo, "worktree", "remove", "--force", q.Cwd)
	_, err = requeue(q)
	refused("checkout removed", q, err, q.Cwd+" is not a directory", "create a fresh worktree and pass --cwd DIR", "takes its HEAD as the base")

	// A directory at that path which is not a checkout has no HEAD to compare.
	if err := os.MkdirAll(q.Cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.Cwd, ".git"), []byte("gitdir: /nonexistent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = requeue(q)
	refused("checkout is not a worktree", q, err, "has no readable HEAD", "pass --cwd DIR")
	if err := os.RemoveAll(q.Cwd); err != nil {
		t.Fatal(err)
	}
	git(repo, "worktree", "prune")

	// Recreated at the copied base, the checkout and base are kept.
	git(repo, "worktree", "add", "-q", "--detach", q.Cwd, base)
	out, err := requeue(q)
	if err != nil || !strings.HasPrefix(out, "requeue ") {
		t.Fatalf("requeue with HEAD at the base: %q %v", out, err)
	}
	got := attempts(q.ItemID)
	if len(got) != 2 {
		t.Fatalf("attempts after the requeue: %+v", got)
	}
	for _, e := range got {
		if e.ID != q.ID && (e.RetryOf != q.ID || e.Attempt != 2 || e.Cwd != q.Cwd || e.BaseCommit != base) {
			t.Fatalf("requeued attempt %+v", e)
		}
	}

	// --cwd is the named fix: it replaces a cleaned checkout and takes the
	// new checkout's HEAD as the base.
	cleanedItem, cleanedOrder := queueFixtureItem(t, f, "requeue-cleaned")
	cleaned := failedEntry("queue-cleaned", cleanedItem.ID, cleanedOrder, spawn.Host())
	git(repo, "worktree", "remove", "--force", cleaned.Cwd)
	_, err = requeue(cleaned)
	refused("second checkout removed", cleaned, err, "is not a directory")
	fresh := filepath.Join(repo, ".build", "worktrees", "queue-fresh")
	git(repo, "worktree", "add", "-q", "--detach", fresh, moved)
	if out, err = requeue(cleaned, "--cwd", fresh); err != nil {
		t.Fatalf("requeue with --cwd: %q %v", out, err)
	}
	for _, e := range attempts(cleaned.ItemID) {
		if e.ID != cleaned.ID && (e.RetryOf != cleaned.ID || e.Cwd != fresh || e.BaseCommit != moved) {
			t.Fatalf("attempt requeued with --cwd %+v", e)
		}
	}
	if got := attempts(cleaned.ItemID); len(got) != 2 {
		t.Fatalf("attempts after --cwd: %+v", got)
	}

	// With a tasks-hub branch, --cwd refuses a HEAD with commits not on it
	// unless --unmerged-head says so; a HEAD on tasks-hub proceeds.
	git(repo, "branch", "tasks-hub", base)
	unmergedItem, unmergedOrder := queueFixtureItem(t, f, "requeue-unmerged")
	unmerged := failedEntry("queue-unmerged", unmergedItem.ID, unmergedOrder, spawn.Host())
	git(unmerged.Cwd, "commit", "-q", "--allow-empty", "-m", "failed attempt's work")
	unreviewed := git(unmerged.Cwd, "rev-parse", "HEAD")
	_, err = requeue(unmerged, "--cwd", unmerged.Cwd)
	refused("HEAD not on tasks-hub", unmerged, err, "HEAD "+unreviewed, "has commits not on tasks-hub", "fresh worktree at the current tasks-hub tip", "pass --cwd DIR", "--unmerged-head")
	_, err = requeue(unmerged, "--unmerged-head")
	refused("--unmerged-head without --cwd", unmerged, err, "--unmerged-head applies only to tt team queue requeue --cwd DIR")
	if out, err = requeue(unmerged, "--cwd", unmerged.Cwd, "--unmerged-head"); err != nil {
		t.Fatalf("requeue with --unmerged-head: %q %v", out, err)
	}
	if got := attempts(unmerged.ItemID); len(got) != 2 {
		t.Fatalf("attempts after --unmerged-head: %+v", got)
	}
	for _, e := range attempts(unmerged.ItemID) {
		if e.ID != unmerged.ID && (e.RetryOf != unmerged.ID || e.Cwd != unmerged.Cwd || e.BaseCommit != unreviewed) {
			t.Fatalf("attempt requeued with --unmerged-head %+v", e)
		}
	}
	// A HEAD at the tasks-hub tip, and one behind it, proceed unflagged.
	git(repo, "branch", "-f", "tasks-hub", moved)
	for _, c := range []struct{ key, at string }{{"tip", moved}, {"behind", base}} {
		item, order := queueFixtureItem(t, f, "requeue-on-hub-"+c.key)
		failed := failedEntry("queue-on-hub-"+c.key, item.ID, order, spawn.Host())
		dir := filepath.Join(repo, ".build", "worktrees", "queue-on-hub-"+c.key+"-fresh")
		git(repo, "worktree", "add", "-q", "--detach", dir, c.at)
		if out, err = requeue(failed, "--cwd", dir); err != nil {
			t.Fatalf("requeue --cwd at tasks-hub %s: %q %v", c.key, out, err)
		}
		for _, e := range attempts(item.ID) {
			if e.ID != failed.ID && (e.Cwd != dir || e.BaseCommit != c.at) {
				t.Fatalf("attempt requeued at tasks-hub %s %+v", c.key, e)
			}
		}
		if got := attempts(item.ID); len(got) != 2 {
			t.Fatalf("attempts after --cwd at tasks-hub %s: %+v", c.key, got)
		}
	}
}

func TestTeamQueueCLIRequeueChecksCopiedDirectoryWithoutRepository(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	// A serial project (the default) admits an entry with no repository.
	// failedEntry queues a serial item with no repository in dir on host,
	// then fails and releases the entry.
	failedEntry := func(key, item string, order int64, host, dir string) api.TeamQueueEntry {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: key + "-add", Operation: "add", ItemID: item, OrderMessageSeq: order, Host: host, Cwd: dir, Serial: true})
		if err != nil {
			t.Fatal(err)
		}
		if q.Repository != "" {
			t.Fatalf("fixture entry has a repository: %+v", q)
		}
		if q, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: key + "-fail", Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: "fixture failure"}); err != nil {
			t.Fatal(err)
		}
		if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"release", "--entry", q.ID}) }); err != nil {
			t.Fatal(err)
		}
		if q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); err != nil {
			t.Fatal(err)
		}
		return q
	}
	requeue := func(q api.TeamQueueEntry) (string, error) {
		t.Helper()
		return captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"requeue", "--entry", q.ID}) })
	}
	attempts := func(item string) int {
		t.Helper()
		list, err := f.c.ListTeamQueue(ctx, f.task.ID)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range list.Entries {
			if e.ItemID == item {
				n++
			}
		}
		return n
	}
	root := t.TempDir()

	// Another host cannot check the directory.
	elsewhereItem, elsewhereOrder := queueFixtureItem(t, f, "requeue-plain-elsewhere")
	elsewhere := failedEntry("plain-elsewhere", elsewhereItem.ID, elsewhereOrder, "another-host", filepath.Join(root, "elsewhere"))
	_, err := requeue(elsewhere)
	if err == nil || !strings.Contains(err.Error(), "launches on another-host") || !strings.Contains(err.Error(), "or pass --cwd DIR there") {
		t.Fatalf("requeue from another host: %v", err)
	}
	if n := attempts(elsewhereItem.ID); n != 1 {
		t.Fatalf("the refused requeue from another host queued an attempt: %d", n)
	}

	// The copied directory was removed: refused, nothing queued.
	dir := filepath.Join(root, "plain")
	q := failedEntry("plain", f.item.ID, f.order, spawn.Host(), dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	_, err = requeue(q)
	if err == nil || !strings.Contains(err.Error(), "working directory "+dir+" is not a directory") || !strings.Contains(err.Error(), "pass --cwd DIR") {
		t.Fatalf("requeue with the directory removed: %v", err)
	}
	if n := attempts(q.ItemID); n != 1 {
		t.Fatalf("the refused requeue queued an attempt: %d", n)
	}

	// Present again, the requeue proceeds and keeps it.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := requeue(q)
	if err != nil || !strings.HasPrefix(out, "requeue ") {
		t.Fatalf("requeue with the directory present: %q %v", out, err)
	}
	list, err := f.c.ListTeamQueue(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Entries {
		if e.ItemID == q.ItemID && e.ID != q.ID && (e.RetryOf != q.ID || e.Cwd != dir || e.Repository != "") {
			t.Fatalf("requeued attempt %+v", e)
		}
	}
	if n := attempts(q.ItemID); n != 2 {
		t.Fatalf("attempts after the requeue: %d", n)
	}
}
