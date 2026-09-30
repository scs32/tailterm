package main

import (
	"bytes"
	"context"
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
