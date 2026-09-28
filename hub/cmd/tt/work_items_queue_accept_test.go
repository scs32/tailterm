package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/testverification"
)

// The leased handler's done save records queue acceptance, and the runner
// reaches Ready to integrate without tt team queue accept
// (wi_b4ec031a206d1326 a1, a3, a4).
func TestHandlerDoneSaveRecordsQueueAcceptanceAndRunnerFinishes(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()

	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "builder")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, repo, "init", "-q", "-b", "tasks-hub")
	if err := os.WriteFile(filepath.Join(repo, "src", "a.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, repo, "add", ".")
	gitFixtureCommand(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "-m", "base")
	base := gitFixtureCommand(t, repo, "rev-parse", "HEAD")
	gitFixtureCommand(t, repo, "worktree", "add", "-q", "-b", "feature/item", worktree)
	if err := os.WriteFile(filepath.Join(worktree, "src", "a.txt"), []byte("fixed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, worktree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "-am", "fix")
	commit := gitFixtureCommand(t, worktree, "rev-parse", "HEAD")
	worktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := queueRepositoryScope(worktree, nil)
	if err != nil {
		t.Fatal(err)
	}

	queued, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "auto-accept-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: worktree, Repository: repository, BaseCommit: base, Ownership: []string{"src"}})
	if err != nil {
		t.Fatal(err)
	}
	r := teamRunner{
		plan: func(ctx context.Context, in map[string]any, out *teamLaunchResolved) error {
			messages, err := f.c.ListMessages(ctx, f.task.ID, 0, "", 100)
			if err != nil {
				return err
			}
			for _, m := range messages {
				if m.Seq == f.order {
					out.ItemRouting.WorkContextBundle = teamCloseCLIContext(t, f.item, m)
					out.Plan = []teamLaunchEntry{{Fields: teamLaunchFields{Name: "lead-auto", Role: "lead", Runtime: "codex", Run: "codex", Cwd: in["cwd"].(string), Prompt: "fixture"}}}
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
			revision, err := strconv.ParseInt(flags["--work-item-revision"], 10, 64)
			if err != nil {
				return err
			}
			order, err := strconv.ParseInt(flags["--work-order-message"], 10, 64)
			if err != nil {
				return err
			}
			_, err = f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: flags["--agent-id"], ExpectedRunID: flags["--expected-run-id"], Name: flags["--name"], Host: "fixture", Session: flags["--name"], Runtime: "codex", Cwd: flags["--cwd"], WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: flags["--work-item"], ItemRevision: revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: order}, ContextBundle: data}})
			return err
		},
		owned: func(context.Context, env, api.Agent) error { return nil },
		cleanup: func(ctx context.Context, _ env, task, id string) error {
			agent, err := f.c.GetAgent(ctx, task, id)
			if err != nil {
				return err
			}
			_, err = f.c.ReportCleanup(ctx, task, id, api.CleanupRequest{RunID: agent.RunID})
			return err
		},
		integration: queueIntegrationSnapshot, // the real Git check, as in production
	}
	if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	q, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, queued.ID)
	if err != nil || q.State != "running" || q.HandlerID != f.handler.ID || q.HandlerRunID != f.handler.RunID {
		t.Fatalf("launch did not lease the handler: %+v %v", q, err)
	}

	current, err := f.c.GetWorkItem(ctx, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := testverification.Prepare(f.c, f.task.ID, current, commit, repository, base); err != nil {
		t.Fatal(err)
	}
	report, _, err := f.st.PutNarrativeReport(ctx, f.task.ID, current.ID, api.PutNarrativeReportRequest{RequestID: "auto-accept-report", ScopeRevision: current.ScopeRevision,
		Sections:   api.NarrativeReportSections{RequestedOutcome: "Deliver the fix.", DeliveredWork: "Synthetic fix complete.", Verification: "Isolated fixture.", Limitations: "Fixture only.", RemainingWork: "Owner integration."},
		References: []api.NarrativeReference{{Kind: "work-item-revision", TaskID: f.task.ID, ItemID: current.ID, Revision: current.Revision, Label: "bounded scope"}}}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	current, err = f.c.GetWorkItem(ctx, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}

	handler := f.e
	handler.agent, handler.runID = f.handler.ID, f.handler.RunID
	save := func(extra ...string) (string, error) {
		args := append([]string{"update", "--revision", fmt.Sprint(current.Revision), "--request-id", "handler-done", "--status", "done",
			"--report-id", report.ReportID, "--report-version", fmt.Sprint(report.Version), "--report-digest", report.Digest, "--report-scope-revision", fmt.Sprint(report.ScopeRevision)}, extra...)
		return captureCLIOutput(t, func() error { return cmdWorkItems(handler, append(args, f.item.ID)) })
	}
	unchanged := func(when string) {
		t.Helper()
		item, err := f.c.GetWorkItem(ctx, f.task.ID, f.item.ID)
		if err != nil || item.Status == "done" || item.Revision != current.Revision {
			t.Fatalf("%s changed the item: %+v %v", when, item, err)
		}
		entry, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, queued.ID)
		if err != nil || entry.Acceptance != nil {
			t.Fatalf("%s changed the entry: %+v %v", when, entry, err)
		}
	}

	// Forcing function: without the tuple the handler's save fails locally.
	if _, err := save(); err == nil || !strings.Contains(err.Error(), "waits on acceptance") {
		t.Fatalf("handler done save without the tuple: %v", err)
	}
	unchanged("save without tuple")
	// The local Git check refuses a dirty worktree before any hub write.
	if err := os.WriteFile(filepath.Join(worktree, "src", "a.txt"), []byte("dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := save("--worktree", worktree, "--branch", "feature/item", "--commit", commit); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("dirty worktree save: %v", err)
	}
	unchanged("dirty worktree save")
	gitFixtureCommand(t, worktree, "checkout", "--", ".")

	out, err := save("--worktree", worktree, "--branch", "feature/item", "--commit", commit)
	if err != nil {
		t.Fatalf("handler done save with acceptance: %v\n%s", err, out)
	}
	if !strings.Contains(out, "queue acceptance recorded: entry "+queued.ID) || !strings.Contains(out, commit) {
		t.Fatalf("save output lacks the acceptance readback: %q", out)
	}
	accepted, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, queued.ID)
	if err != nil || accepted.Acceptance == nil || accepted.Acceptance.Commit != commit || accepted.Acceptance.Worktree != worktree || accepted.Acceptance.Branch != "feature/item" || !strings.Contains(accepted.Acceptance.Evidence, "handler-saved completion receipt") {
		t.Fatalf("entry acceptance after the save: %+v %v", accepted.Acceptance, err)
	}
	if accepted.Acceptance.BaseCommit != base || accepted.Acceptance.ItemRevision != current.Revision+1 || accepted.Acceptance.CompletionReport == nil || accepted.Acceptance.CompletionReport.ReportID != report.ReportID {
		t.Fatalf("acceptance not bound to the verified base and saved completion: %+v", accepted.Acceptance)
	}

	// a4 retried save: the same command replays the receipt, nothing new.
	again, err := save("--worktree", worktree, "--branch", "feature/item", "--commit", commit)
	if err != nil || strings.SplitN(again, "\n", 2)[0] != strings.SplitN(out, "\n", 2)[0] {
		t.Fatalf("retried save: %q vs %q: %v", again, out, err)
	}
	if replay, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, queued.ID); err != nil || replay.Revision != accepted.Revision || replay.Acceptance.AcceptedAt != accepted.Acceptance.AcceptedAt {
		t.Fatalf("retried save changed the entry: %+v %v", replay, err)
	}

	// The runner closes the team and reaches Ready to integrate with no
	// separate accept.
	for i := 0; i < 3; i++ {
		if err := r.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatal(err)
		}
		if q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, queued.ID); err != nil || q.State == "finished" {
			break
		}
	}
	if err != nil || q.State != "finished" || q.Integration == nil || q.Integration.Commit != commit || q.Integration.Worktree != worktree || q.Integration.Evidence != accepted.Acceptance.Evidence {
		t.Fatalf("runner did not reach Ready to integrate: %+v %v", q, err)
	}

	// a3: the manual accept stays an idempotent recovery path.
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(handler, []string{"accept", "--entry", queued.ID, "--worktree", worktree, "--branch", "feature/item", "--commit", commit, "--evidence", "handler-saved acceptance receipt"})
	}); err != nil {
		t.Fatal("identical manual accept after automatic acceptance", err)
	}
	if after, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, queued.ID); err != nil || after.Revision != q.Revision || after.State != "finished" {
		t.Fatalf("identical manual accept changed the entry: %+v %v", after, err)
	}
}
