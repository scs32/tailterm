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

// leadDoneSave is a launched team queue entry in a temporary Git repository,
// with a verified candidate commit and the feature's completion report, ready
// for the item lead's done save (wi_26c0698de7d3eef2 a7). The entry owns src;
// the candidate commit also adds any outside files given.
type leadDoneSave struct {
	teamFixture
	runner                         teamRunner
	lead                           api.Agent
	queued                         api.TeamQueueEntry
	current                        api.WorkItem
	report                         api.NarrativeReportVersion
	worktree, commit, base, branch string
}

func newLeadDoneSave(t *testing.T, outside ...string) *leadDoneSave {
	t.Helper()
	f := &leadDoneSave{teamFixture: newTeamFixture(t, true), branch: "feature/item"}
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
	f.base = gitFixtureCommand(t, repo, "rev-parse", "HEAD")
	gitFixtureCommand(t, repo, "worktree", "add", "-q", "-b", f.branch, worktree)
	if err := os.WriteFile(filepath.Join(worktree, "src", "a.txt"), []byte("fixed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, name := range outside {
		if err := os.WriteFile(filepath.Join(worktree, name), []byte("outside\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	gitFixtureCommand(t, worktree, "add", ".")
	gitFixtureCommand(t, worktree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "-m", "fix")
	f.commit = gitFixtureCommand(t, worktree, "rev-parse", "HEAD")
	var err error
	if f.worktree, err = filepath.EvalSymlinks(worktree); err != nil {
		t.Fatal(err)
	}
	repository, err := queueRepositoryScope(f.worktree, nil)
	if err != nil {
		t.Fatal(err)
	}

	if f.queued, err = f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "lead-accept-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: f.worktree, Repository: repository, BaseCommit: f.base, Ownership: []string{"src"}}); err != nil {
		t.Fatal(err)
	}
	f.runner = teamRunner{
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
	if err := f.runner.tick(ctx, f.e, f.c, "fixture"); err != nil {
		t.Fatal(err)
	}
	q, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, f.queued.ID)
	if err != nil || q.State != "running" {
		t.Fatalf("launch: %+v %v", q, err)
	}
	agents, err := f.c.ListAgents(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Name == "lead-auto" {
			f.lead = a
		}
	}
	if f.lead.ID == "" || f.lead.ID == f.handler.ID {
		t.Fatalf("the launched lead is missing: %+v", agents)
	}

	current, err := f.c.GetWorkItem(ctx, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := testverification.Prepare(f.c, f.task.ID, current, f.commit, repository, f.base); err != nil {
		t.Fatal(err)
	}
	if f.report, _, err = f.st.PutNarrativeReport(ctx, f.task.ID, current.ID, api.PutNarrativeReportRequest{RequestID: "lead-accept-report", ScopeRevision: current.ScopeRevision,
		Sections:   api.NarrativeReportSections{RequestedOutcome: "Deliver the fix.", DeliveredWork: "Synthetic fix complete.", Verification: "Isolated fixture.", Limitations: "Fixture only.", RemainingWork: "Owner integration."},
		References: []api.NarrativeReference{{Kind: "work-item-revision", TaskID: f.task.ID, ItemID: current.ID, Revision: current.Revision, Label: "bounded scope"}}}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	if f.current, err = f.c.GetWorkItem(ctx, f.task.ID, f.item.ID); err != nil {
		t.Fatal(err)
	}
	return f
}

// save runs tt work-items update --status done as the given agent's run, with
// the completion report pin and any extra flags.
func (f *leadDoneSave) save(t *testing.T, by api.Agent, extra ...string) (string, error) {
	t.Helper()
	e := f.e
	e.agent, e.runID = by.ID, by.RunID
	args := append([]string{"update", "--revision", fmt.Sprint(f.current.Revision), "--request-id", "lead-done", "--status", "done",
		"--report-id", f.report.ReportID, "--report-version", fmt.Sprint(f.report.Version), "--report-digest", f.report.Digest, "--report-scope-revision", fmt.Sprint(f.report.ScopeRevision)}, extra...)
	return captureCLIOutput(t, func() error { return cmdWorkItems(e, append(args, f.item.ID)) })
}

func (f *leadDoneSave) tuple() []string {
	return []string{"--worktree", f.worktree, "--branch", f.branch, "--commit", f.commit}
}

func (f *leadDoneSave) releaseJobs(t *testing.T) int {
	t.Helper()
	jobs, err := f.c.Releases(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, j := range jobs {
		if j.EntryID == f.queued.ID {
			n++
		}
	}
	return n
}

// The item lead's done save records queue acceptance with no handler call,
// and the runner reaches Ready to integrate.
func TestLeadDoneSaveRecordsQueueAcceptance(t *testing.T) {
	f := newLeadDoneSave(t)
	ctx := context.Background()

	// Forcing function: without the tuple the lead's save fails locally.
	if _, err := f.save(t, f.lead); err == nil || !strings.Contains(err.Error(), "waits on acceptance") {
		t.Fatalf("lead done save without the tuple: %v", err)
	}
	out, err := f.save(t, f.lead, f.tuple()...)
	if err != nil {
		t.Fatalf("lead done save with acceptance: %v\n%s", err, out)
	}
	if !strings.Contains(out, "queue acceptance recorded: entry "+f.queued.ID) || !strings.Contains(out, f.commit) {
		t.Fatalf("save output lacks the acceptance readback: %q", out)
	}
	item, err := f.c.GetWorkItem(ctx, f.task.ID, f.item.ID)
	if err != nil || item.Status != "done" || item.Revision != f.current.Revision+1 || item.UpdatedBy.AgentID != f.lead.ID {
		t.Fatalf("item after the lead's save: %+v %v", item, err)
	}
	accepted, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, f.queued.ID)
	if err != nil || accepted.Acceptance == nil || accepted.Acceptance.Commit != f.commit || accepted.Acceptance.Worktree != f.worktree || accepted.Acceptance.Branch != f.branch || !strings.Contains(accepted.Acceptance.Evidence, "lead-saved completion receipt") {
		t.Fatalf("entry acceptance after the save: %+v %v", accepted.Acceptance, err)
	}
	if accepted.Acceptance.BaseCommit != f.base || accepted.Acceptance.ItemRevision != f.current.Revision+1 || accepted.Acceptance.CompletionReport == nil || accepted.Acceptance.CompletionReport.ReportID != f.report.ReportID {
		t.Fatalf("acceptance not bound to the verified base and saved completion: %+v", accepted.Acceptance)
	}
	if accepted.HandlerID != f.handler.ID || f.lead.ID == accepted.HandlerID {
		t.Fatalf("entry lease changed: %+v", accepted)
	}

	// The runner closes the team and reaches Ready to integrate.
	q := accepted
	for i := 0; i < 3; i++ {
		if err = f.runner.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatal(err)
		}
		if q, err = f.c.GetTeamQueueEntry(ctx, f.task.ID, f.queued.ID); err != nil || q.State == "finished" {
			break
		}
	}
	if err != nil || q.State != "finished" || q.Integration == nil || q.Integration.Commit != f.commit || q.Integration.Worktree != f.worktree || q.Integration.Evidence != accepted.Acceptance.Evidence {
		t.Fatalf("runner did not reach Ready to integrate: %+v %v", q, err)
	}
}

// The same command again replays the saved receipt and enqueues nothing.
func TestLeadDoneSaveReplay(t *testing.T) {
	f := newLeadDoneSave(t)
	ctx := context.Background()
	out, err := f.save(t, f.lead, f.tuple()...)
	if err != nil {
		t.Fatalf("lead done save with acceptance: %v\n%s", err, out)
	}
	accepted, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, f.queued.ID)
	if err != nil || accepted.Acceptance == nil {
		t.Fatalf("entry %+v %v", accepted, err)
	}
	jobs := f.releaseJobs(t)
	if jobs != 1 {
		t.Fatalf("release jobs after the save = %d, want 1", jobs)
	}
	again, err := f.save(t, f.lead, f.tuple()...)
	if err != nil || again != out {
		t.Fatalf("retried save: %q vs %q: %v", again, out, err)
	}
	replay, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, f.queued.ID)
	if err != nil || replay.Revision != accepted.Revision || replay.Acceptance.AcceptedAt != accepted.Acceptance.AcceptedAt {
		t.Fatalf("retried save changed the entry: %+v %v", replay, err)
	}
	if n := f.releaseJobs(t); n != jobs {
		t.Fatalf("retried save enqueued a second release job: %d -> %d", jobs, n)
	}
}

// Changed files against ownership at queue acceptance (wi_3f48b6ccc57e95f2
// a1, a2, a4): tt lists the candidate's changed files, the hub refuses the
// one outside src until the leased handler gives a reason, and the saved
// acceptance records it with that file.
func TestDoneSaveRefusesChangedFilesOutsideOwnership(t *testing.T) {
	f := newLeadDoneSave(t, "outside.txt")
	ctx := context.Background()
	reason := "outside.txt is the fixture's release note"
	running, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, f.queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func(when string) {
		t.Helper()
		item, err := f.c.GetWorkItem(ctx, f.task.ID, f.item.ID)
		if err != nil || item.Status == "done" || item.Revision != f.current.Revision {
			t.Fatalf("%s changed the item: %+v %v", when, item, err)
		}
		entry, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, f.queued.ID)
		if err != nil || entry.Acceptance != nil || entry.Revision != running.Revision {
			t.Fatalf("%s changed the entry: acceptance %+v revision %d, want %d: %v", when, entry.Acceptance, entry.Revision, running.Revision, err)
		}
		if n := f.releaseJobs(t); n != 0 {
			t.Fatalf("%s enqueued %d release jobs", when, n)
		}
	}

	// a1: the leased handler's save without a reason is refused by the hub,
	// naming the outside file.
	if _, err := f.save(t, f.handler, f.tuple()...); err == nil || !strings.Contains(err.Error(), "outside the entry's owned paths: outside.txt") || !strings.Contains(err.Error(), "--outside-ownership-reason") || strings.Contains(err.Error(), "src/a.txt") {
		t.Fatalf("handler save of an outside file: %v", err)
	}
	unchanged("refused handler save")
	// a2: the lead may not give a reason.
	if _, err := f.save(t, f.lead, append(f.tuple(), "--outside-ownership-reason", reason)...); err == nil || !strings.Contains(err.Error(), "only the leased database handler may record an outside-ownership reason") {
		t.Fatalf("lead save with a reason: %v", err)
	}
	unchanged("refused lead save")

	// a2: the leased handler's reason accepts it and is saved with the file.
	withReason := append(f.tuple(), "--outside-ownership-reason", reason)
	out, err := f.save(t, f.handler, withReason...)
	if err != nil {
		t.Fatalf("handler save with a reason: %v\n%s", err, out)
	}
	accepted, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, f.queued.ID)
	if err != nil || accepted.Acceptance == nil || len(accepted.Acceptance.OutsideOwnership) != 1 || accepted.Acceptance.OutsideOwnership[0] != "outside.txt" || accepted.Acceptance.OutsideOwnershipReason != reason {
		t.Fatalf("acceptance after the handler's save: %+v %v", accepted.Acceptance, err)
	}

	// a4: the same command replays its receipt; the recovery accept of the
	// saved tuple changes nothing.
	again, err := f.save(t, f.handler, withReason...)
	if err != nil || again != out {
		t.Fatalf("retried save: %q vs %q: %v", again, out, err)
	}
	handler := f.e
	handler.agent, handler.runID = f.handler.ID, f.handler.RunID
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(handler, append([]string{"accept", "--entry", f.queued.ID, "--evidence", "handler-saved acceptance receipt", "--outside-ownership-reason", reason}, f.tuple()...))
	}); err != nil {
		t.Fatal("recovery accept of the saved tuple", err)
	}
	if after, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, f.queued.ID); err != nil || after.Revision != accepted.Revision || after.Acceptance.AcceptedAt != accepted.Acceptance.AcceptedAt || f.releaseJobs(t) != 1 {
		t.Fatalf("retry or recovery changed the entry: %+v %v jobs %d", after, err, f.releaseJobs(t))
	}
}

// correct commits a correction in the fixture worktree on a new branch from
// parent, saves its passing receipt as a new verification plan, which clears
// the acceptance of f.commit, and has the owner re-bind the acceptance to it
// with correctionOf (wi_c7a7b97e72fc151b a1-a3). It returns the correction.
func (f *leadDoneSave) correct(t *testing.T, branch, parent string) string {
	t.Helper()
	ctx := context.Background()
	gitFixtureCommand(t, f.worktree, "checkout", "-q", "-b", branch, parent)
	if err := os.WriteFile(filepath.Join(f.worktree, "src", "a.txt"), []byte("corrected from "+parent[:7]+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, f.worktree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "-am", "correction")
	correction := gitFixtureCommand(t, f.worktree, "rev-parse", "HEAD")
	history, err := f.c.VerificationHistory(ctx, f.task.ID, f.item.ID, f.handler.ID, f.handler.RunID)
	if err != nil || len(history) == 0 || history[0].Plan == nil {
		t.Fatalf("verification history: %+v %v", history, err)
	}
	p := *history[0].Plan
	p.OperationKey, p.Commit = "correction-"+strings.ReplaceAll(branch, "/", "-"), correction
	g := int64(len(history))
	if _, err = f.c.SaveVerification(ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: p.OperationKey + "-plan", AgentID: f.handler.ID, RunID: f.handler.RunID, ExpectedGeneration: g, Plan: &p}); err != nil {
		t.Fatal("correction plan", err)
	}
	r := *history[1].Receipt
	r.OperationKey, r.Commit, r.PlanDigest = p.OperationKey, correction, cliDigest(p)
	if _, err = f.c.SaveVerification(ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: p.OperationKey + "-receipt", AgentID: f.handler.ID, RunID: f.handler.RunID, ExpectedGeneration: g + 1, Receipt: &r}); err != nil {
		t.Fatal("correction receipt", err)
	}
	links := []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.current.Revision, Relationship: "primary"}}
	if _, err = f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{RequestID: "correction-accept-" + strings.ReplaceAll(branch, "/", "-"), WorkItems: links, Envelope: &api.Envelope{Kind: "notice", Subject: "Re-bind the acceptance to its verified correction", Body: api.EnvelopeBody{Text: "Fixture only"}, Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "owner-accept", Candidate: correction, CorrectionOf: f.commit}}}); err != nil {
		t.Fatal("owner-accept correction", err)
	}
	return correction
}

// a2, a3 (c1, c2): the lead's done save on a correction that descends from
// the accepted candidate records queue acceptance on the correction, with no
// re-verification of the original and no second acceptance.
func TestLeadDoneSaveCorrectionDescends(t *testing.T) {
	f := newLeadDoneSave(t)
	ctx := context.Background()
	correction := f.correct(t, "feature/correction", f.commit)
	out, err := f.save(t, f.lead, "--worktree", f.worktree, "--branch", "feature/correction", "--commit", correction)
	if err != nil {
		t.Fatalf("done save on the descending correction: %v\n%s", err, out)
	}
	item, err := f.c.GetWorkItem(ctx, f.task.ID, f.item.ID)
	if err != nil || item.Status != "done" {
		t.Fatalf("item after the correction's done save: %+v %v", item, err)
	}
	accepted, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, f.queued.ID)
	if err != nil || accepted.Acceptance == nil || accepted.Acceptance.Commit != correction || accepted.Acceptance.Branch != "feature/correction" {
		t.Fatalf("queue acceptance not on the correction: %+v %v", accepted.Acceptance, err)
	}
	history, err := f.c.VerificationHistory(ctx, f.task.ID, f.item.ID, f.handler.ID, f.handler.RunID)
	if err != nil || len(history) != 4 {
		t.Fatalf("the original was re-verified or the correction not recorded once: %d records %v", len(history), err)
	}
}

// a2: a correction that does not descend from the accepted candidate is
// refused locally, naming both commits. The item stays open and no release
// job exists.
func TestLeadDoneSaveCorrectionRefusesNonDescendant(t *testing.T) {
	f := newLeadDoneSave(t)
	ctx := context.Background()
	sibling := f.correct(t, "feature/sibling", f.base)
	out, err := f.save(t, f.lead, "--worktree", f.worktree, "--branch", "feature/sibling", "--commit", sibling)
	if err == nil || !strings.Contains(err.Error(), f.commit) || !strings.Contains(err.Error(), sibling) || !strings.Contains(err.Error(), "does not descend") {
		t.Fatalf("done save on a sibling correction: %v\n%s", err, out)
	}
	item, err := f.c.GetWorkItem(ctx, f.task.ID, f.item.ID)
	if err != nil || item.Status == "done" || item.Revision != f.current.Revision {
		t.Fatalf("item after the refused save: %+v %v", item, err)
	}
	if q, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, f.queued.ID); err != nil || q.Acceptance != nil {
		t.Fatalf("refused save recorded acceptance: %+v %v", q.Acceptance, err)
	}
	if n := f.releaseJobs(t); n != 0 {
		t.Fatalf("refused save created %d release jobs", n)
	}
}
