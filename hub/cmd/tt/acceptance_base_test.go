package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/testverification"
)

// Queue acceptance refuses a candidate that is not a fast-forward of the
// recorded base: the item's verification plan base, which is the tasks-hub
// tip the plan was frozen on, not the queue-time base
// (wi_82ed4c6924930bad a7, order #13844).
type acceptanceBaseFixture struct {
	teamFixture
	ctx                       context.Context
	runner                    teamRunner
	queued                    api.TeamQueueEntry
	worktree, repository      string
	queueBase, tip, oldCommit string
	handlerEnv                env
	report                    api.NarrativeReportVersion
	current                   api.WorkItem
}

// newAcceptanceBaseFixture queues an item at base B0, commits the builder's
// fix on B0, then moves tasks-hub on to B1 as other releases do.
func newAcceptanceBaseFixture(t *testing.T) *acceptanceBaseFixture {
	t.Helper()
	a := &acceptanceBaseFixture{teamFixture: newTeamFixture(t, true), ctx: context.Background()}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	a.worktree = filepath.Join(root, "builder")
	commitFile := func(dir, name, content, message string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "src"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "src", name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		gitFixtureCommand(t, dir, "add", ".")
		gitFixtureCommand(t, dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "-m", message)
		return gitFixtureCommand(t, dir, "rev-parse", "HEAD")
	}
	if err := os.MkdirAll(repo, 0755); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, repo, "init", "-q", "-b", "tasks-hub")
	a.queueBase = commitFile(repo, "a.txt", "base\n", "base")
	gitFixtureCommand(t, repo, "worktree", "add", "-q", "-b", "feature/item", a.worktree)
	a.oldCommit = commitFile(a.worktree, "a.txt", "fixed\n", "fix on the queue-time base")
	a.tip = commitFile(repo, "b.txt", "released meanwhile\n", "another release")
	var err error
	if a.worktree, err = filepath.EvalSymlinks(a.worktree); err != nil {
		t.Fatal(err)
	}
	if a.repository, err = queueRepositoryScope(a.worktree, nil); err != nil {
		t.Fatal(err)
	}
	if a.queued, err = a.c.TeamQueueAction(a.ctx, a.task.ID, api.TeamQueueRequest{RequestID: "acceptance-base-add", Operation: "add", ItemID: a.item.ID, OrderMessageSeq: a.order, Host: "fixture", Cwd: a.worktree, Repository: a.repository, BaseCommit: a.queueBase, Ownership: []string{"src"}}); err != nil {
		t.Fatal(err)
	}
	a.runner = teamRunner{
		plan: func(ctx context.Context, in map[string]any, out *teamLaunchResolved) error {
			messages, err := a.c.ListMessages(ctx, a.task.ID, 0, "", 100)
			if err != nil {
				return err
			}
			for _, m := range messages {
				if m.Seq == a.order {
					out.ItemRouting.WorkContextBundle = teamCloseCLIContext(t, a.item, m)
					out.Plan = []teamLaunchEntry{{Fields: teamLaunchFields{Name: "lead-base", Role: "lead", Runtime: "codex", Run: "codex", Cwd: in["cwd"].(string), Prompt: "fixture"}}}
					return nil
				}
			}
			return fmt.Errorf("order missing")
		},
		spawn: func(_ env, args []string) error {
			flags := map[string]string{}
			for i := 0; i+1 < len(args); i += 2 {
				flags[args[i]] = args[i+1]
			}
			data, err := os.ReadFile(flags["--work-context-file"])
			if err != nil {
				return err
			}
			var revision, order int64
			fmt.Sscan(flags["--work-item-revision"], &revision)
			fmt.Sscan(flags["--work-order-message"], &order)
			_, err = a.c.AddAgent(a.ctx, a.task.ID, api.AddAgentRequest{AgentID: flags["--agent-id"], ExpectedRunID: flags["--expected-run-id"], Name: flags["--name"], Host: "fixture", Session: flags["--name"], Runtime: "codex", Cwd: flags["--cwd"], WorkItem: &api.AgentWorkItemRequest{ItemTaskID: a.task.ID, ItemID: flags["--work-item"], ItemRevision: revision, WorkOrderMessage: api.MessageReference{TaskID: a.task.ID, Seq: order}, ContextBundle: data}})
			return err
		},
		owned: func(context.Context, env, api.Agent) error { return nil },
		cleanup: func(ctx context.Context, _ env, task, id string) error {
			agent, err := a.c.GetAgent(ctx, task, id)
			if err != nil {
				return err
			}
			_, err = a.c.ReportCleanup(ctx, task, id, api.CleanupRequest{RunID: agent.RunID})
			return err
		},
		integration: queueIntegrationSnapshot,
	}
	if err := a.runner.tick(a.ctx, a.e, a.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	a.handlerEnv = a.e
	a.handlerEnv.agent, a.handlerEnv.runID = a.handler.ID, a.handler.RunID
	return a
}

// freeze records the handler's verification plan for commit on the current
// tasks-hub tip and the completion report the done save pins.
func (a *acceptanceBaseFixture) freeze(t *testing.T, commit string) {
	t.Helper()
	current, err := a.c.GetWorkItem(a.ctx, a.task.ID, a.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := testverification.Prepare(a.c, a.task.ID, current, commit, a.repository, a.tip); err != nil {
		t.Fatal(err)
	}
	if a.report, _, err = a.st.PutNarrativeReport(a.ctx, a.task.ID, current.ID, api.PutNarrativeReportRequest{RequestID: "acceptance-base-report", ScopeRevision: current.ScopeRevision,
		Sections:   api.NarrativeReportSections{RequestedOutcome: "Deliver the fix.", DeliveredWork: "Synthetic fix complete.", Verification: "Isolated fixture.", Limitations: "Fixture only.", RemainingWork: "Owner integration."},
		References: []api.NarrativeReference{{Kind: "work-item-revision", TaskID: a.task.ID, ItemID: current.ID, Revision: current.Revision, Label: "bounded scope"}}}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	if a.current, err = a.c.GetWorkItem(a.ctx, a.task.ID, a.item.ID); err != nil {
		t.Fatal(err)
	}
}

func (a *acceptanceBaseFixture) doneSave(t *testing.T, commit string) (string, error) {
	args := []string{"update", "--revision", fmt.Sprint(a.current.Revision), "--request-id", "acceptance-base-done", "--status", "done",
		"--report-id", a.report.ReportID, "--report-version", fmt.Sprint(a.report.Version), "--report-digest", a.report.Digest, "--report-scope-revision", fmt.Sprint(a.report.ScopeRevision),
		"--worktree", a.worktree, "--branch", "feature/item", "--commit", commit, a.item.ID}
	return captureCLIOutput(t, func() error { return cmdWorkItems(a.handlerEnv, args) })
}

// ownerDone saves the item done without acceptance, leaving the running
// entry waiting on the handler's tt team queue accept.
func (a *acceptanceBaseFixture) ownerDone(t *testing.T) {
	t.Helper()
	done := "done"
	if _, _, err := a.st.CreateWorkItemUpdate(a.ctx, a.task.ID, a.item.ID, api.CreateWorkItemUpdate{ExpectedRevision: a.current.Revision, RequestID: "acceptance-base-owner-done", Status: &done, CompletionReport: &api.NarrativeReportPin{ReportID: a.report.ReportID, Version: a.report.Version, Digest: a.report.Digest, ScopeRevision: a.report.ScopeRevision}}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
}

func (a *acceptanceBaseFixture) queueAccept(t *testing.T, commit string) (string, error) {
	return captureCLIOutput(t, func() error {
		return cmdTeamQueue(a.handlerEnv, []string{"accept", "--entry", a.queued.ID, "--worktree", a.worktree, "--branch", "feature/item", "--commit", commit, "--evidence", "handler-saved acceptance receipt"})
	})
}

func (a *acceptanceBaseFixture) unaccepted(t *testing.T, when string) {
	t.Helper()
	entry, err := a.c.GetTeamQueueEntry(a.ctx, a.task.ID, a.queued.ID)
	if err != nil || entry.Acceptance != nil {
		t.Fatalf("%s recorded acceptance: %+v %v", when, entry.Acceptance, err)
	}
}

func (a *acceptanceBaseFixture) rebase(t *testing.T) string {
	t.Helper()
	gitFixtureCommand(t, a.worktree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "rebase", "-q", "tasks-hub")
	return gitFixtureCommand(t, a.worktree, "rev-parse", "HEAD")
}

func TestAcceptanceRefusesCandidateOutsideThePlanBase(t *testing.T) {
	a := newAcceptanceBaseFixture(t)
	// The plan was frozen on the tasks-hub tip, but the candidate was built
	// on the older queue-time base: it descends from B0 but not from B1.
	a.freeze(t, a.oldCommit)
	if _, err := a.doneSave(t, a.oldCommit); err == nil || !strings.Contains(err.Error(), "not a fast-forward of the recorded base") {
		t.Fatalf("done save of a candidate outside the plan base: %v", err)
	}
	a.unaccepted(t, "refused done save")
	if _, err := a.doneSave(t, a.tip); err == nil || !strings.Contains(err.Error(), "is not the verification plan commit "+a.oldCommit) {
		t.Fatalf("done save of a commit other than the plan's: %v", err)
	}
	a.unaccepted(t, "wrong-commit done save")

	a.ownerDone(t)
	if _, err := a.queueAccept(t, a.oldCommit); err == nil || !strings.Contains(err.Error(), "not a fast-forward of the recorded base") {
		t.Fatalf("queue accept of a candidate outside the plan base: %v", err)
	}
	if _, err := a.queueAccept(t, a.tip); err == nil || !strings.Contains(err.Error(), "is not the verification plan commit") {
		t.Fatalf("queue accept of a commit other than the plan's: %v", err)
	}
	a.unaccepted(t, "refused queue accept")
}

func TestDoneSaveAcceptsRebasedCandidateOnThePlanBase(t *testing.T) {
	a := newAcceptanceBaseFixture(t)
	rebased := a.rebase(t)
	a.freeze(t, rebased)
	if out, err := a.doneSave(t, rebased); err != nil {
		t.Fatalf("done save of the rebased candidate: %v\n%s", err, out)
	}
	entry, err := a.c.GetTeamQueueEntry(a.ctx, a.task.ID, a.queued.ID)
	if err != nil || entry.Acceptance == nil || entry.Acceptance.Commit != rebased || entry.Acceptance.BaseCommit != a.tip || entry.BaseCommit != a.tip {
		t.Fatalf("acceptance is not bound to the plan base %s: %+v %v", a.tip, entry.Acceptance, err)
	}
	// The integration snapshot checks the same recorded base, so the runner
	// reaches Ready to integrate with the exact verified commit.
	for i := 0; i < 3 && entry.State != "finished"; i++ {
		if err := a.runner.tick(a.ctx, a.e, a.c, "fixture"); err != nil {
			t.Fatal(err)
		}
		if entry, err = a.c.GetTeamQueueEntry(a.ctx, a.task.ID, a.queued.ID); err != nil {
			t.Fatal(err)
		}
	}
	if entry.State != "finished" || entry.Integration == nil || entry.Integration.Commit != rebased || entry.Integration.BaseCommit != a.tip {
		t.Fatalf("runner did not reach Ready to integrate on the plan base: %+v", entry)
	}
}

func TestQueueAcceptAcceptsRebasedCandidateOnThePlanBase(t *testing.T) {
	a := newAcceptanceBaseFixture(t)
	rebased := a.rebase(t)
	a.freeze(t, rebased)
	a.ownerDone(t)
	if out, err := a.queueAccept(t, rebased); err != nil {
		t.Fatalf("queue accept of the rebased candidate: %v\n%s", err, out)
	}
	entry, err := a.c.GetTeamQueueEntry(a.ctx, a.task.ID, a.queued.ID)
	if err != nil || entry.Acceptance == nil || entry.Acceptance.Commit != rebased || entry.Acceptance.BaseCommit != a.tip {
		t.Fatalf("acceptance is not bound to the plan base %s: %+v %v", a.tip, entry.Acceptance, err)
	}
}
