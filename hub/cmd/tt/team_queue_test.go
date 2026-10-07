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

// a6: when the hub refuses the notice the entry stays queued and add fails
// naming the entry and the exact notice to post.
func TestTeamQueueCLIDefaultLaneNoticeFailureKeepsEntry(t *testing.T) {
	f := newTeamFixtureKind(t, true, "bug")
	repo, _ := queueGitRepo(t)
	target, err := url.Parse(f.e.hub)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(target)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") {
			http.Error(w, `{"error":"board unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	broken := f.e
	broken.hub = proxy.URL
	_, _, err = laneAdd(t, broken, f.item, f.order, repo, "--owns", "a", "--owns", "b", "--owns", "c", "--owns", "d")
	q := laneEntry(t, f, f.item.ID)
	if q.Template != "planned" || q.State != "queued" {
		t.Fatalf("entry after a refused notice %+v", q)
	}
	if err == nil {
		t.Fatal("add succeeded without its reason record")
	}
	for _, want := range []string{"entry " + q.ID + " was added", "its Planned reason was not recorded", "NOTICE: A bug was queued on Planned delivery with a recorded reason", "Queue entry " + q.ID + " puts bug " + f.item.ID + " on template planned", "reason=paths (more than three owned paths)", fmt.Sprintf("order #%d", f.order)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q:\n%v", want, err)
		}
	}
	if notices := laneNotices(t, f, f.item.ID); len(notices) != 0 {
		t.Fatalf("refused notice was stored: %+v", notices)
	}
}
