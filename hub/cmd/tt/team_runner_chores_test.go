package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// choresRunner launches fake members through the hub for the named items:
// members[item] lists member names, the first being the item lead.
func choresRunner(t *testing.T, f teamFixture, orders map[string]int64, members map[string][]string) teamRunner {
	t.Helper()
	ctx := context.Background()
	return teamRunner{
		plan: func(ctx context.Context, in map[string]any, out *teamLaunchResolved) error {
			itemID := in["item"].(string)
			item, err := f.c.GetWorkItem(ctx, f.task.ID, itemID)
			if err != nil {
				return err
			}
			out.ItemRouting.WorkContextBundle = teamCloseCLIContext(t, item, api.Message{TaskID: f.task.ID, Seq: orders[itemID], Text: "bounded order"})
			for i, name := range members[itemID] {
				role := "lead"
				if i > 0 {
					role = "builder"
				}
				out.Plan = append(out.Plan, teamLaunchEntry{Fields: teamLaunchFields{Name: name, Role: role, Runtime: "codex", Run: "codex", Cwd: in["cwd"].(string), Prompt: "fixture"}})
			}
			return nil
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
			revision, _ := strconv.ParseInt(flags["--work-item-revision"], 10, 64)
			order, _ := strconv.ParseInt(flags["--work-order-message"], 10, 64)
			a, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: flags["--agent-id"], ExpectedRunID: flags["--expected-run-id"], Name: flags["--name"], Host: "fixture", Session: flags["--name"], Runtime: "codex", Cwd: flags["--cwd"], WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: flags["--work-item"], ItemRevision: revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: order}, ContextBundle: data}})
			if err != nil {
				return err
			}
			_, err = f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventRunning})
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
	}
}

// choresAgentEnv is the CLI identity of one registered agent.
func choresAgentEnv(t *testing.T, f teamFixture, name string) (env, api.Agent) {
	t.Helper()
	detail, err := f.c.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range detail.Agents {
		if a.Name == name {
			e := f.e
			e.agent, e.runID = a.ID, a.RunID
			return e, a
		}
	}
	t.Fatalf("agent %s is not registered", name)
	return env{}, api.Agent{}
}

// choresPosted returns the sequence a tt send printed.
func choresPosted(t *testing.T, out string) int64 {
	t.Helper()
	var seq int64
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "posted #%d", &seq); err != nil {
		t.Fatalf("send output %q: %v", out, err)
	}
	return seq
}

// a11 (c6): the 2026-09-29 deadlock. A coarse entry A is running; the owner
// released its candidate out of band; a post-release a13 REQUEST to A's
// verifier is still open; the owner failed A "as integrated"; A's lead cannot
// close itself; queued B overlaps A. One supported owner command records the
// integration: B then claims, A's item keeps its record and A's verifier can
// still reach the handler. After a13's RESULT and a plain done save, the
// runner closes and cleans A's team and A ends finished. No dismissal, owner
// cancel, owner team close or queue release is needed at any point.
func TestTeamRunnerResolvesSeptember29DeadlockWithOneOwnerCommand(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	root, base := queueGitRepo(t)
	start := choresGit(t, root, "symbolic-ref", "--short", "HEAD")
	choresGit(t, root, "checkout", "-q", "-b", "feature-a")
	shipped := choresCommit(t, root, "A delivery", "src/a/one.go", "docs/a.md")
	choresGit(t, root, "checkout", "-q", start)
	repository := filepath.Join(root, ".git")
	observeRunnerHost(t, f, "fixture", 1)
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "deadlock-limit", Operation: "set_limit", Host: "fixture", ConcurrencyLimit: 3}); err != nil {
		t.Fatal(err)
	}
	itemB, orderB := queueFixtureItem(t, f, "deadlock-b")
	a, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "deadlock-add-a", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: root, Repository: repository, BaseCommit: base, Ownership: []string{"src", "docs"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "deadlock-add-b", Operation: "add", ItemID: itemB.ID, OrderMessageSeq: orderB, Host: "fixture", Cwd: t.TempDir(), Repository: repository, BaseCommit: base, Ownership: []string{"src/b"}})
	if err != nil {
		t.Fatal(err)
	}
	runner := choresRunner(t, f, map[string]int64{f.item.ID: f.order, itemB.ID: orderB}, map[string][]string{
		f.item.ID: {"lead-a", "verifier-a"},
		itemB.ID:  {"lead-b"},
	})
	entry := func(id string) api.TeamQueueEntry {
		t.Helper()
		q, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	tick := func() {
		t.Helper()
		time.Sleep(2 * time.Second) // isolated test hub's write bucket refills
		if err := runner.tick(ctx, f.e, f.c, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if got := entry(a.ID); got.State != "running" {
		t.Fatalf("A did not start: %+v", got)
	}
	if got := entry(b.ID); got.State != "queued" {
		t.Fatalf("B crossed coarse A: %+v", got)
	}
	leadEnv, lead := choresAgentEnv(t, f, "lead-a")
	verifierEnv, _ := choresAgentEnv(t, f, "verifier-a")
	handlerEnv := f.e
	handlerEnv.agent, handlerEnv.runID = f.handler.ID, f.handler.RunID
	links := []string{"--work-item", f.item.ID, "--work-item-revision", fmt.Sprint(f.item.Revision), "--work-order-message", fmt.Sprint(f.order)}
	send := func(e env, args ...string) int64 {
		t.Helper()
		out, err := captureCLIOutput(t, func() error { return cmdSend(e, append(args, links...)) })
		if err != nil {
			t.Fatalf("send %v: %v", args, err)
		}
		return choresPosted(t, out)
	}

	// The post-release a13 REQUEST is open; the owner fails A "as integrated".
	a13 := send(leadEnv, "--kind", "request", "--to", "verifier-a", "--subject", "Run the post-release check on the live site", "--ask", "Run a13 against the released candidate.")
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"fail", "--entry", a.ID, "--reason", "owner integrated " + shipped[:7]})
	}); err != nil {
		t.Fatal(err)
	}
	if err := cmdClose(leadEnv, nil); err == nil || !strings.Contains(err.Error(), "item lead remains available") {
		t.Fatalf("lead self-close: %v", err)
	}
	if err := cmdClose(leadEnv, []string{"--team"}); err == nil {
		t.Fatal("lead closed its team while the item is open")
	}
	tick()
	if got := entry(b.ID); got.State != "queued" {
		t.Fatalf("B crossed failed A before the owner record: %+v", got)
	}

	// One supported owner command.
	itemBefore, err := f.c.GetWorkItem(ctx, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"integrated", "--entry", a.ID, "--commit", shipped, "--evidence", "owner release record"})
	})
	if err != nil {
		t.Fatalf("integrated: %v", err)
	}
	if !strings.Contains(out, "integrated "+a.ID) {
		t.Fatalf("integrated output %q", out)
	}
	integrated := entry(a.ID)
	if integrated.ReleasedAt == "" || integrated.OwnerIntegration == nil || integrated.OwnerIntegration.Commit != shipped || strings.Join(integrated.OwnerIntegration.ChangedFiles, ",") != "docs/a.md,src/a/one.go" {
		t.Fatalf("owner integration record %+v", integrated)
	}
	if listed, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) }); err != nil || !strings.Contains(listed, "failed (owner-integrated) "+a.ID) {
		t.Fatalf("list %q %v", listed, err)
	}
	tick()
	if got := entry(b.ID); got.State != "running" || got.HandlerID != f.handler.ID {
		t.Fatalf("B did not claim A's slot and lease: %+v", got)
	}
	if item, err := f.c.GetWorkItem(ctx, f.task.ID, f.item.ID); err != nil || item.Status != itemBefore.Status || item.Revision != itemBefore.Revision {
		t.Fatalf("A's item changed: %+v %v", item, err)
	}

	// A's verifier still reaches the handler that served A.
	if _, err := captureCLIOutput(t, func() error { return cmdObligationAction(verifierEnv, "ack", []string{fmt.Sprint(a13)}) }); err != nil {
		t.Fatal(err)
	}
	record := send(verifierEnv, "--kind", "request", "--to", "role:database_handler", "--subject", "Record the post-release check result", "--ask", "Record a13 evidence on the item.")
	messages, err := f.c.ListMessages(ctx, f.task.ID, record-1, "", 5)
	if err != nil || len(messages) == 0 || messages[0].Seq != record || messages[0].To != f.handler.ID {
		t.Fatalf("role:database_handler routing %+v %v", messages, err)
	}
	send(verifierEnv, "--kind", "result", "--to", "lead-a", "--reply-to", fmt.Sprint(a13), "--subject", "The post-release check passes on the live site", "--outcome", "done", "--status", "a13=pass", "--evidence", "e1: live check -> ok")
	if _, err := captureCLIOutput(t, func() error { return cmdObligationAction(handlerEnv, "ack", []string{fmt.Sprint(record)}) }); err != nil {
		t.Fatal(err)
	}
	send(handlerEnv, "--kind", "result", "--to", "verifier-a", "--reply-to", fmt.Sprint(record), "--subject", "The post-release check result is recorded", "--outcome", "done", "--status", "a1=pass", "--evidence", "e1: recorded")

	// The handler saves a plain done; the runner closes, cleans and finishes A.
	by := api.Caller{Node: "fixture", User: "owner"}
	current, err := f.c.GetWorkItem(ctx, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A was verified and lead-accepted on the shipped candidate before the
	// owner released it, as on 09-29.
	if err := testverification.Prepare(f.c, f.task.ID, current, shipped, repository, base); err != nil {
		t.Fatal(err)
	}
	if current, err = f.c.GetWorkItem(ctx, f.task.ID, f.item.ID); err != nil {
		t.Fatal(err)
	}
	report, _, err := f.st.PutNarrativeReport(ctx, f.task.ID, current.ID, api.PutNarrativeReportRequest{RequestID: "deadlock-report", ScopeRevision: current.ScopeRevision,
		Sections:   api.NarrativeReportSections{RequestedOutcome: "Deliver A.", DeliveredWork: "A was released by the owner.", Verification: "a13 passed.", Limitations: "Fixture only.", RemainingWork: "None."},
		References: []api.NarrativeReference{{Kind: "work-item-revision", TaskID: f.task.ID, ItemID: current.ID, Revision: current.Revision, Label: "bounded scope"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	done := "done"
	if _, _, err := f.st.CreateWorkItemUpdate(ctx, f.task.ID, current.ID, api.CreateWorkItemUpdate{ExpectedRevision: current.Revision, Status: &done, AgentID: f.handler.ID, RunID: f.handler.RunID, RequestID: "deadlock-plain-done", CompletionReport: &api.NarrativeReportPin{ReportID: report.ReportID, Version: report.Version, Digest: report.Digest, ScopeRevision: report.ScopeRevision}}, by); err != nil {
		t.Fatalf("plain done save: %v", err)
	}
	for i := 0; i < 3 && entry(a.ID).State != "finished"; i++ {
		tick()
	}
	final := entry(a.ID)
	if final.State != "finished" || final.ReleasedAt == "" || final.OwnerIntegration == nil || final.Integration != nil {
		t.Fatalf("A end state %+v", final)
	}
	detail, err := f.c.GetTask(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	members := 0
	for _, agent := range detail.Agents {
		if agent.WorkItem != nil && agent.WorkItem.ItemID == f.item.ID {
			members++
			if agent.Status != api.AgentClosed || !agent.CleanupDone {
				t.Fatalf("A member %s not closed and cleaned: %+v", agent.Name, agent)
			}
		}
	}
	if members != 2 || lead.ID == "" {
		t.Fatalf("A had %d members", members)
	}
	if got := entry(b.ID); got.State != "running" {
		t.Fatalf("B changed: %+v", got)
	}
}

// a4 (CLI): tt team queue integrated runs only from an unbound session and
// only for a commit in the entry's repository that descends from its base.
func TestTeamQueueIntegratedCLIRefusals(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	root, base := queueGitRepo(t)
	start := choresGit(t, root, "symbolic-ref", "--short", "HEAD")
	choresGit(t, root, "checkout", "-q", "--orphan", "unrelated")
	unrelated := choresCommit(t, root, "unrelated history", "other.txt")
	choresGit(t, root, "checkout", "-q", start)
	good := choresCommit(t, root, "delivery", "src/a/one.go")
	observeRunnerHost(t, f, "fixture", 1)
	if _, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "cli-limit", Operation: "set_limit", Host: "fixture", ConcurrencyLimit: 0}); err != nil {
		t.Fatal(err)
	}
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "cli-add", Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: root, Repository: filepath.Join(root, ".git"), BaseCommit: base, Ownership: []string{"src"}})
	if err != nil {
		t.Fatal(err)
	}
	integrated := func(e env, commit string) error {
		_, err := captureCLIOutput(t, func() error {
			return cmdTeamQueue(e, []string{"integrated", "--entry", q.ID, "--commit", commit})
		})
		return err
	}
	if err := integrated(f.e, good); err == nil || !strings.Contains(err.Error(), "only a running or unreleased failed entry") {
		t.Fatalf("queued entry: %v", err)
	}
	q = runQueueEntry(t, f, q.ID)
	handlerEnv := f.e
	handlerEnv.agent, handlerEnv.runID = f.handler.ID, f.handler.RunID
	if err := integrated(handlerEnv, good); err == nil || !strings.Contains(err.Error(), "unbound CLI session") {
		t.Fatalf("agent session: %v", err)
	}
	if err := integrated(f.e, strings.Repeat("f", 40)); err == nil || !strings.Contains(err.Error(), "is not in the entry's repository") {
		t.Fatalf("missing commit: %v", err)
	}
	if err := integrated(f.e, unrelated); err == nil || !strings.Contains(err.Error(), "does not descend from the entry base") {
		t.Fatalf("unrelated commit: %v", err)
	}
	if err := integrated(f.e, "abc1234"); err == nil || !strings.Contains(err.Error(), "full commit SHA") {
		t.Fatalf("short SHA: %v", err)
	}
	if got, _ := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); got.ReleasedAt != "" || got.OwnerIntegration != nil {
		t.Fatalf("a refused command changed the entry: %+v", got)
	}
	if err := integrated(f.e, good); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.c.GetTeamQueueEntry(ctx, f.task.ID, q.ID); got.ReleasedAt == "" || got.OwnerIntegration == nil || strings.Join(got.OwnerIntegration.ChangedFiles, ",") != "src/a/one.go" || got.State != "running" {
		t.Fatalf("integrated entry: %+v", got)
	}
}
