package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// The delivery lifecycle test drives one synthetic item through the real
// store, the real HTTP server and the tt command entry points, from queue add
// to its release job (wi_a747a13aa62f4712, order #13868). Each variant walks
// the dead ends that stopped live teams: the base moved after queueing, the
// verification plan named the builder worktree or the repository root, the
// full matrix receipt was over the shared 64 KiB body limit, and the lead
// recorded a follow-ups or owner-decision disposition before verification.
//
// Agents are registered by a stub spawn: no runtime or tmux session starts.
// testdata/lifecycle/fails-before.md records the five fixes this test guards.

const (
	lifecycleMatrixChecks = 69
	lifecycleMaxAttempts  = 3
	lifecycleBranch       = "feature/item"
	lifecycleFix          = "src/a.txt line 1 now reads fixed"
)

var lifecycleCriteria = []string{
	"a1=the fix changes src/a.txt as ordered",
	"a2=the change is covered by a check",
}

type lifecycleOptions struct {
	name           string
	planRepository string // "worktree" or "root"
	disposition    string // "follow-ups" or "owner-decision"
	// ownerSavesDone: the owner saves the item done and the handler then runs
	// tt team queue accept. Otherwise the handler's done save carries the
	// acceptance and the later queue accept changes nothing.
	ownerSavesDone bool
}

type lifecycleRun struct {
	teamFixture
	ctx      context.Context
	opts     lifecycleOptions
	runner   teamRunner
	host     string
	dir      string // scratch files for --file arguments
	repo     string // repository root: the directory holding .git
	worktree string // the builder's worktree
	gitDir   string // the queue entry's repository: the .git directory
	b0, b1   string // tasks-hub at queue time and after it moved
	c1, c2   string // the candidate with blocker b1, and its fix
	order    int64
	entry    string
	owner    env
	handlerE env
	members  map[string]env // lead, builder, reviewer, verifier
	agents   map[string]api.Agent
	keys     int
	steps    int
}

func TestDeliveryLifecycle(t *testing.T) {
	for _, opts := range []lifecycleOptions{
		{name: "base moved after queueing, plan names the worktree, receipt over 64 KiB, follow-ups before verification", planRepository: "worktree", disposition: "follow-ups"},
		{name: "base moved after queueing, plan names the repository root, receipt over 64 KiB, owner-decision before verification", planRepository: "root", disposition: "owner-decision", ownerSavesDone: true},
	} {
		t.Run(opts.name, func(t *testing.T) {
			l := newLifecycleRun(t, opts)
			l.queueAndConfirmScope(t)
			l.moveBaseAndLaunch(t)
			l.restartHub(t)
			assign := l.assignAndReviewRoundOne(t)
			l.fixAndReviewRoundTwo(t, assign)
			l.dispositionBeforeVerification(t)
			l.restartHub(t)
			l.fullSizeVerification(t, assign)
			l.accept(t)
			l.queueAcceptOnNewerBase(t)
			l.releaseJob(t)
		})
	}
}

func newLifecycleRun(t *testing.T, opts lifecycleOptions) *lifecycleRun {
	t.Helper()
	l := &lifecycleRun{teamFixture: newTeamFixture(t, true), ctx: context.Background(), opts: opts, host: spawn.Host(), dir: t.TempDir(), members: map[string]env{}, agents: map[string]api.Agent{}}
	l.owner = l.e
	l.handlerE = l.e
	l.handlerE.agent, l.handlerE.runID, l.handlerE.agentName = l.handler.ID, l.handler.RunID, l.handler.Name
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l.repo, l.worktree = filepath.Join(root, "repo"), filepath.Join(root, "builder")
	if err := os.MkdirAll(l.repo, 0755); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, l.repo, "init", "-q", "-b", "tasks-hub")
	l.b0 = l.commit(t, l.repo, "a.txt", "base\n", "base")
	gitFixtureCommand(t, l.repo, "worktree", "add", "-q", "-b", lifecycleBranch, l.worktree)
	if l.gitDir, err = queueRepositoryScope(l.worktree, nil); err != nil {
		t.Fatal(err)
	}
	return l
}

// restartHub reopens the store behind a new server. The hub allows one
// caller a burst of 40 writes, which a team spreads over hours and this test
// sends in a second; each phase between restarts stays under that burst
// whatever the host's speed. It also shows every phase survives a hub restart.
func (l *lifecycleRun) restartHub(t *testing.T) {
	t.Helper()
	l.teamFixture.restartHub(t)
	l.owner.hub, l.handlerE.hub = l.e.hub, l.e.hub
	for role, member := range l.members {
		member.hub = l.e.hub
		l.members[role] = member
	}
}

func (l *lifecycleRun) step(t *testing.T, format string, args ...any) {
	t.Helper()
	l.steps++
	t.Logf("step %d: %s", l.steps, fmt.Sprintf(format, args...))
}

func (l *lifecycleRun) commit(t *testing.T, dir, name, content, message string) string {
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

func (l *lifecycleRun) key(prefix string) string {
	l.keys++
	return fmt.Sprintf("lifecycle-%s-%d", prefix, l.keys)
}

// file writes v as JSON for a tt --file or --review-file argument.
func (l *lifecycleRun) file(t *testing.T, name string, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(l.dir, name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// read runs a tt read command and decodes the JSON it prints into v.
func (l *lifecycleRun) read(t *testing.T, v any, what string, fn func() error) {
	t.Helper()
	out, err := lifecycleCLI(t, fn)
	if err != nil {
		t.Fatalf("%s: %v\n%s", what, err, out)
	}
	if err = json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("%s output: %v\n%s", what, err, out)
	}
}

func (l *lifecycleRun) current(t *testing.T) api.WorkItem {
	t.Helper()
	var item api.WorkItem
	l.read(t, &item, "tt work-items get", func() error { return cmdWorkItems(l.owner, []string{"get", "--json", l.item.ID}) })
	return item
}

// linked adds the item link and a fresh retry key to a tt post or tt send.
func (l *lifecycleRun) linked(t *testing.T, args []string) []string {
	t.Helper()
	item := l.current(t)
	return append(args, "--work-item", item.ID, "--work-item-revision", fmt.Sprint(item.Revision), "--work-order-message", fmt.Sprint(l.order), "--request-id", l.key("post"))
}

// lifecycleCLI runs a tt command entry point and returns what it printed. It
// drains the pipe while the command runs: a saved plan or receipt prints more
// than a pipe holds, which would block captureCLIOutput.
func lifecycleCLI(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	printed := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(r)
		printed <- data
	}()
	os.Stdout = w
	runErr := fn()
	os.Stdout = old
	_ = w.Close()
	data := <-printed
	_ = r.Close()
	return string(data), runErr
}

func lifecyclePosted(out string) int64 {
	_, number, _ := strings.Cut(strings.TrimSpace(out), "posted #")
	seq, _ := strconv.ParseInt(number, 10, 64)
	return seq
}

// send posts a typed message with tt send as e and returns its sequence.
func (l *lifecycleRun) send(t *testing.T, e env, args ...string) (int64, error) {
	t.Helper()
	args = l.linked(t, args)
	out, err := lifecycleCLI(t, func() error { return cmdSend(e, args) })
	if err != nil {
		return 0, err
	}
	seq := lifecyclePosted(out)
	if seq == 0 {
		t.Fatalf("tt send printed no sequence: %q", out)
	}
	return seq, nil
}

func (l *lifecycleRun) mustSend(t *testing.T, e env, args ...string) int64 {
	t.Helper()
	seq, err := l.send(t, e, args...)
	if err != nil {
		t.Fatalf("tt send %v: %v", args, err)
	}
	return seq
}

// refused expects err to be a refusal by tt. A wrong reason is reported without
// stopping, so the step that fails live still runs and reports its own error.
func (l *lifecycleRun) refused(t *testing.T, what, reason string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was not refused", what)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("%s was refused for another reason: %v (want %q)", what, err, reason)
	}
}

func (l *lifecycleRun) ack(t *testing.T, e env, seq int64) {
	t.Helper()
	if out, err := lifecycleCLI(t, func() error { return cmdObligationAction(e, "ack", []string{fmt.Sprint(seq)}) }); err != nil {
		t.Fatalf("tt ack %d: %v\n%s", seq, err, out)
	}
}

func (l *lifecycleRun) queueEntry(t *testing.T) api.TeamQueueEntry {
	t.Helper()
	var list api.TeamQueueList
	l.read(t, &list, "tt team queue list", func() error { return cmdTeamQueue(l.owner, []string{"list", "--item", l.item.ID, "--json"}) })
	for _, entry := range list.Entries {
		if entry.ID == l.entry {
			return entry
		}
	}
	t.Fatalf("tt team queue list does not show entry %s: %+v", l.entry, list.Entries)
	return api.TeamQueueEntry{}
}

// reviews reads the item's review state from the hub API: no tt command
// prints it (tt message-checks --summary prints only counts).
func (l *lifecycleRun) reviews(t *testing.T) api.ReviewConvergence {
	t.Helper()
	list, err := l.c.ListReviewConvergence(l.ctx, l.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range list {
		if state.ItemID == l.item.ID {
			return state
		}
	}
	t.Fatalf("no review state for %s", l.item.ID)
	return api.ReviewConvergence{}
}

func (l *lifecycleRun) disposition(kind, candidate string) map[string]string {
	return map[string]string{"mode": "disposition", "disposition": kind, "candidate": candidate}
}

// The owner files the order and queues the item on base B0. The hub
// takes the entry only once the handler has confirmed the scope of that exact
// revision and order.
func (l *lifecycleRun) queueAndConfirmScope(t *testing.T) {
	t.Helper()
	out, err := lifecycleCLI(t, func() error {
		return cmdPost(l.owner, l.linkedOrder(t, "Work order: change src/a.txt as the item says."))
	})
	if err != nil || lifecyclePosted(out) == 0 {
		t.Fatalf("tt post work order: %v\n%s", err, out)
	}
	l.order = lifecyclePosted(out)
	item := l.current(t)
	add := []string{"add", "--item", item.ID, "--order", fmt.Sprint(l.order), "--owns", "src", "--cwd", l.worktree, "--json"}
	_, err = lifecycleCLI(t, func() error { return cmdTeamQueue(l.owner, add) })
	l.refused(t, "queue add before the handler confirmed the scope", "scope is not confirmed for this exact item revision and order", err)
	l.step(t, "queue add: refused until the handler confirms the scope of order #%d", l.order)
	if out, err = lifecycleCLI(t, func() error {
		return cmdWorkItems(l.handlerE, []string{"scope", "confirm", "--complete", "--request-id", l.key("scope"), "--revision", fmt.Sprint(item.Revision), "--scope-revision", fmt.Sprint(item.ScopeRevision), "--order", fmt.Sprint(l.order), "--owns", "src", item.ID})
	}); err != nil {
		t.Fatalf("tt work-items scope confirm: %v\n%s", err, out)
	}
	l.step(t, "scope confirmed by the handler for revision %d and order #%d", item.Revision, l.order)

	if out, err = lifecycleCLI(t, func() error { return cmdTeamQueue(l.owner, add) }); err != nil {
		t.Fatalf("tt team queue add: %v\n%s", err, out)
	}
	var queued api.TeamQueueEntry
	if err = json.Unmarshal([]byte(out), &queued); err != nil {
		t.Fatalf("tt team queue add output: %v\n%s", err, out)
	}
	if queued.State != "queued" || queued.BaseCommit != l.b0 || queued.Repository != l.gitDir || queued.Cwd != l.worktree {
		t.Fatalf("entry is not queued on base %s in %s: %+v", l.b0, l.gitDir, queued)
	}
	l.entry = queued.ID
	l.step(t, "queue add: entry %s queued on base B0 %s", queued.ID, l.b0)
}

func (l *lifecycleRun) linkedOrder(t *testing.T, text string) []string {
	t.Helper()
	item := l.current(t)
	// tt post links a message to the item through a recorded order: the fixture order.
	return []string{"--work-item", item.ID, "--work-item-revision", fmt.Sprint(item.Revision), "--work-order-message", fmt.Sprint(l.teamFixture.order), "--request-id", l.key("order"), text}
}

// tasks-hub moves on to B1 as another release lands, then the runner
// launches the team. The stub spawn registers each member with its bound
// context; no runtime or tmux session starts.
func (l *lifecycleRun) moveBaseAndLaunch(t *testing.T) {
	t.Helper()
	l.b1 = l.commit(t, l.repo, "b.txt", "released meanwhile\n", "another release")
	l.runner = teamRunner{
		plan: func(ctx context.Context, in map[string]any, out *teamLaunchResolved) error {
			messages, err := l.c.ListMessages(ctx, l.task.ID, 0, "", 100)
			if err != nil {
				return err
			}
			for _, m := range messages {
				if m.Seq != l.order {
					continue
				}
				out.ItemRouting.WorkContextBundle = teamCloseCLIContext(t, l.item, m)
				for _, role := range []string{"lead", "builder", "reviewer", "verifier"} {
					out.Plan = append(out.Plan, teamLaunchEntry{Fields: teamLaunchFields{Name: role + "-e2e", Role: role, Runtime: "codex", Run: "codex", Cwd: in["cwd"].(string), Prompt: "fixture"}})
				}
				return nil
			}
			return fmt.Errorf("order #%d missing", l.order)
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
			_, err = l.c.AddAgent(l.ctx, l.task.ID, api.AddAgentRequest{AgentID: flags["--agent-id"], ExpectedRunID: flags["--expected-run-id"], Name: flags["--name"], Host: l.host, Session: flags["--name"], Runtime: "codex", Cwd: flags["--cwd"], WorkItem: &api.AgentWorkItemRequest{ItemTaskID: l.task.ID, ItemID: flags["--work-item"], ItemRevision: revision, WorkOrderMessage: api.MessageReference{TaskID: l.task.ID, Seq: order}, ContextBundle: data}})
			return err
		},
		owned: func(context.Context, env, api.Agent) error { return nil },
		cleanup: func(ctx context.Context, _ env, task, id string) error {
			agent, err := l.c.GetAgent(ctx, task, id)
			if err != nil {
				return err
			}
			_, err = l.c.ReportCleanup(ctx, task, id, api.CleanupRequest{RunID: agent.RunID})
			return err
		},
		integration: queueIntegrationSnapshot, // the real Git check, as in production
	}
	if err := l.runner.tick(l.ctx, l.e, l.c, l.host); err != nil {
		t.Fatal(err)
	}
	entry := l.queueEntry(t)
	if entry.State != "running" || entry.HandlerID != l.handler.ID || entry.HandlerRunID != l.handler.RunID || entry.BaseCommit != l.b0 {
		t.Fatalf("launch did not reach running with the handler leased: %+v", entry)
	}
	var roster []api.Agent
	l.read(t, &roster, "tt agents", func() error { return cmdAgents(l.owner, []string{"--json"}) })
	item := l.current(t)
	for _, a := range roster {
		role, ok := strings.CutSuffix(a.Name, "-e2e")
		if !ok {
			continue
		}
		if a.WorkItem == nil || a.WorkItem.ItemID != item.ID || a.WorkItem.ItemRevision != item.Revision || a.WorkItem.WorkOrderMessage.Seq != l.order {
			t.Fatalf("%s is not bound to revision %d and order #%d: %+v", a.Name, item.Revision, l.order, a.WorkItem)
		}
		l.agents[role] = a
		l.members[role] = env{hub: l.e.hub, task: l.task.ID, agent: a.ID, runID: a.RunID, agentName: a.Name}
	}
	if len(l.members) != 4 || !l.agents["lead"].ItemLead {
		t.Fatalf("launch bound %d members, lead=%v", len(l.members), l.agents["lead"].ItemLead)
	}
	l.step(t, "launch: tasks-hub moved to B1 %s, then lead, builder, reviewer and verifier bound to the item", l.b1)
}

// The lead freezes two criteria, the builder delivers C1 rebased on
// B1, and review round one raises blocker b1 against it.
func (l *lifecycleRun) assignAndReviewRoundOne(t *testing.T) int64 {
	t.Helper()
	lead, builder, reviewer := l.members["lead"], l.members["builder"], l.members["reviewer"]
	assign := l.mustSend(t, lead, "--kind", "assign", "--to", "builder-e2e", "--subject", "Change the source file as the order says", "--objective", "Make src/a.txt read fixed", "--owns", "src", "--acceptance", lifecycleCriteria[0], "--acceptance", lifecycleCriteria[1])
	l.ack(t, builder, assign)
	l.commit(t, l.worktree, "a.txt", "half fixed\n", "first attempt")
	gitFixtureCommand(t, l.worktree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "rebase", "-q", "tasks-hub")
	l.c1 = gitFixtureCommand(t, l.worktree, "rev-parse", "HEAD")
	l.mustSend(t, builder, "--kind", "result", "--to", "lead-e2e", "--reply-to", fmt.Sprint(assign), "--subject", "First candidate is ready for review", "--outcome", "done", "--status", "a1=pass", "--status", "a2=pass", "--evidence", "e1: git show --stat -> src/a.txt changed", "--ref", "commit="+l.c1)

	review := l.mustSend(t, lead, "--kind", "review", "--to", "reviewer-e2e", "--subject", "Review the first candidate against both criteria", "--candidate", l.c1, "--scope", "src/a.txt", "--acceptance", lifecycleCriteria[0], "--acceptance", lifecycleCriteria[1])
	l.ack(t, reviewer, review)
	roundOne := l.file(t, "review-1.json", api.ReviewMetadata{Mode: "general", Candidate: l.c1, Blockers: []api.ReviewFinding{{ID: "b1", Criterion: "a1", Title: "Fix is incomplete", File: "src/a.txt", Line: 1}}})
	result := l.mustSend(t, reviewer, "--kind", "result", "--to", "lead-e2e", "--reply-to", fmt.Sprint(review), "--subject", "First candidate has one blocker", "--outcome", "done", "--status", "a1=fail", "--status", "a2=pass", "--evidence", "e1: cat src/a.txt -> half fixed", "--review-file", roundOne)

	state := l.reviews(t)
	if len(state.Rounds) != 1 || state.Rounds[0].ResultSeq != result || state.Rounds[0].Candidate != l.c1 || len(state.Rounds[0].Blockers) != 1 || state.Rounds[0].Blockers[0].ID != "b1" {
		t.Fatalf("round one did not complete with blocker b1 on %s: %+v", l.c1, state.Rounds)
	}
	l.step(t, "review round 1: blocker b1 raised on C1 %s", l.c1)
	return assign
}

// C2 is the one-line fix for b1. Round two resolves b1 explicitly and
// leaves a2 failed with a finding, which the hub files as a follow-up.
func (l *lifecycleRun) fixAndReviewRoundTwo(t *testing.T, assign int64) {
	t.Helper()
	lead, builder, reviewer := l.members["lead"], l.members["builder"], l.members["reviewer"]
	request := l.mustSend(t, lead, "--kind", "request", "--to", "builder-e2e", "--subject", "Fix the one blocker from the first review", "--ask", "Fix blocker b1 only: src/a.txt line 1 must read fixed")
	l.ack(t, builder, request)
	l.c2 = l.commit(t, l.worktree, "a.txt", "fixed\n", "fix b1")
	if diff := gitFixtureCommand(t, l.worktree, "diff", "--numstat", l.c1, l.c2); diff != "1\t1\tsrc/a.txt" {
		t.Fatalf("C2 is not the one-line fix of C1: %q", diff)
	}
	l.mustSend(t, builder, "--kind", "result", "--to", "lead-e2e", "--reply-to", fmt.Sprint(request), "--subject", "The blocker is fixed in a new candidate", "--outcome", "done", "--status", "b1=pass", "--evidence", "e1: git diff --numstat -> 1 1 src/a.txt", "--ref", "commit="+l.c2)
	l.step(t, "fix: C2 %s is the one-line fix for b1", l.c2)

	review := l.mustSend(t, lead, "--kind", "review", "--to", "reviewer-e2e", "--subject", "Review the fixed candidate against both criteria", "--candidate", l.c2, "--scope", "src/a.txt", "--acceptance", lifecycleCriteria[0], "--acceptance", lifecycleCriteria[1])
	l.ack(t, reviewer, review)
	verdict := []string{"--kind", "result", "--to", "lead-e2e", "--reply-to", fmt.Sprint(review), "--subject", "The blocker is resolved and one gap remains", "--outcome", "done", "--status", "a1=pass", "--status", "a2=fail", "--evidence", "e1: cat src/a.txt -> fixed"}
	finding := []api.ReviewFinding{{ID: "n1", Criterion: "a2", Title: "Edge case is not covered", File: "src/a.txt", Line: 1}}

	silent := l.file(t, "review-2-silent.json", api.ReviewMetadata{Mode: "general", Candidate: l.c2, Findings: finding})
	_, err := l.send(t, reviewer, append(verdict, "--review-file", silent)...)
	l.refused(t, "a round-two result that neither retains nor resolves b1", "round two must retain or explicitly resolve b1", err)

	roundTwo := l.file(t, "review-2.json", api.ReviewMetadata{Mode: "general", Candidate: l.c2, BlockerIDs: []string{"b1"}, Fix: lifecycleFix, Findings: finding})
	result := l.mustSend(t, reviewer, append(verdict, "--review-file", roundTwo)...)
	l.step(t, "review round 2: b1 resolved on C2, a2 left failed with finding n1")

	state := l.reviews(t)
	if len(state.Rounds) != 2 || state.Rounds[1].ResultSeq != result || len(state.Rounds[1].Blockers) != 0 || state.Rounds[1].Verdicts["a1"] != "pass" || state.Rounds[1].Verdicts["a2"] != "fail" || len(state.Rounds[1].Verdicts) != 2 {
		t.Fatalf("round two is not complete with a1=pass, a2=fail and no blockers: %+v", state.Rounds)
	}
	if len(state.Focused) != 1 {
		t.Fatalf("the resolution of b1 is not persisted: %+v", state.Focused)
	}
	resolution := state.Focused[0]
	if len(resolution.BlockerIDs) != 1 || resolution.BlockerIDs[0] != "b1" || resolution.Candidate != l.c2 || !resolution.Passed || resolution.ResultSeq != result || !strings.Contains(resolution.Fix, lifecycleFix) {
		t.Fatalf("persisted resolution does not bind b1 to C2 %s and result #%d: %+v", l.c2, result, resolution)
	}
	if len(state.FollowUps) != 1 || state.FollowUps[0].Finding.ID != "n1" || !api.ValidID(state.FollowUps[0].ItemID, "wi") || state.FollowUps[0].MessageSeq != result {
		t.Fatalf("finding n1 was not filed as a follow-up item: %+v", state.FollowUps)
	}
	l.step(t, "persisted resolution checked: b1 on C2 by result #%d, follow-up %s filed for n1", result, state.FollowUps[0].ItemID)
}

// With a2 still failed the lead records its disposition while
// verification is pending.
func (l *lifecycleRun) dispositionBeforeVerification(t *testing.T) {
	t.Helper()
	lead := l.members["lead"]
	accept := l.file(t, "accept.json", l.disposition("accept", l.c2))
	notice := func(e env, subject, file string) (int64, error) {
		return l.send(t, e, "--kind", "notice", "--subject", subject, "--text", "Candidate "+l.c2, "--review-file", file)
	}
	_, err := notice(lead, "Accept the fixed candidate as it stands", accept)
	l.refused(t, "an ordinary accept with a2 failed and verification pending", "passing receipt for current scope and exact accepted candidate required", err)
	if _, err := notice(lead, "Record the lead disposition for the candidate", l.file(t, "disposition.json", l.disposition(l.opts.disposition, l.c2))); err != nil {
		t.Fatalf("%s disposition before verification: %v", l.opts.disposition, err)
	}
	state := l.reviews(t)
	if state.Disposition == nil || state.Disposition.Kind != l.opts.disposition || state.Disposition.Candidate != l.c2 || state.Disposition.AgentID != l.agents["lead"].ID {
		t.Fatalf("%s disposition is not recorded for C2: %+v", l.opts.disposition, state.Disposition)
	}
	_, err = notice(lead, "Accept the fixed candidate after the disposition", accept)
	l.refused(t, "an ordinary accept after the disposition", "disposition already recorded", err)
	ownerAccept := l.file(t, "owner-accept.json", l.disposition("owner-accept", l.c2))
	_, err = notice(lead, "Accept the candidate in the name of the owner", ownerAccept)
	l.refused(t, "an agent-authored owner-accept", "owner-accept requires an owner-authored notice", err)
	if l.opts.disposition == "follow-ups" {
		_, err = notice(l.owner, "Owner accepts the candidate before verification", ownerAccept)
		l.refused(t, "the owner's owner-accept before verification", "passing receipt for current scope and exact accepted candidate required", err)
	}
	if got := l.reviews(t).Disposition; got == nil || got.Kind != l.opts.disposition {
		t.Fatalf("a refused notice changed the disposition: %+v", got)
	}
	l.step(t, "disposition: %s recorded for C2 before verification", l.opts.disposition)
}

// lifecycleDigest is the hub's canonical digest of a plan or check list.
func lifecycleDigest(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	if err = json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err = encoder.Encode(generic); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(encoded.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

// The handler saves the plan for C2 on B1 and then a receipt the size
// of the approved matrix: 69 checks, each passing on its third attempt, with
// a real log file behind every attempt.
func (l *lifecycleRun) fullSizeVerification(t *testing.T, assign int64) {
	t.Helper()
	item := l.current(t)
	matrix := strings.Repeat("c", 64)
	out, err := lifecycleCLI(t, func() error {
		return cmdPost(l.owner, []string{"--request-id", l.key("matrix"), "verification-matrix-approval:" + matrix})
	})
	if err != nil || lifecyclePosted(out) == 0 {
		t.Fatalf("owner matrix approval: %v\n%s", err, out)
	}
	repository := l.worktree
	if l.opts.planRepository == "root" {
		repository = l.repo
	}
	checks := make([]api.VerificationCheck, lifecycleMatrixChecks)
	for i := range checks {
		suite := fmt.Sprintf("tests/fixture-%03d-browser.mjs", i)
		checks[i] = api.VerificationCheck{ID: fmt.Sprintf("%03d-%s", i, suite), Argv: []string{"node", "scripts/verify-matrix.mjs", "run", "--check", suite, "--engine", "chromium", "--engine", "webkit"}, Cwd: ".", Environment: map[string]string{
			"HOME":                     "/tmp/tailterm-verification-fixture/home",
			"NODE_OPTIONS":             "--max-old-space-size=4096",
			"PATH":                     "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/go/bin",
			"PLAYWRIGHT_BROWSERS_PATH": "/tmp/tailterm-verification-fixture/ms-playwright",
			"VERIFICATION_BASE_COMMIT": l.b1,
		}}
	}
	builder, verifier := l.agents["builder"], l.agents["verifier"]
	plan := api.VerificationPlan{MaxAttempts: lifecycleMaxAttempts, ApprovedMatrixDigest: matrix, MatrixApprovalMessageSeq: lifecyclePosted(out), ItemID: item.ID, ItemTaskID: l.task.ID, AssignmentOwnershipDigest: lifecycleDigest(t, []string{"src"}), Version: 1, OperationKey: l.key("verification"), Repository: repository, BaseCommit: l.b1, Commit: l.c2, ItemRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: l.order, AssignmentSeq: assign, BuilderAgentID: builder.ID, BuilderRunID: builder.RunID, VerifierAgentID: verifier.ID, VerifierRunID: verifier.RunID, MatrixDigest: matrix, ChecksDigest: lifecycleDigest(t, checks), Owned: []string{"src"}, Changed: []string{"src/a.txt"}, Checks: checks}
	planFile := l.file(t, "plan.json", plan)
	if out, err = lifecycleCLI(t, func() error {
		return cmdVerification(l.handlerE, []string{"plan", "--item", item.ID, "--file", planFile, "--request-id", l.key("plan"), "--generation", "0"})
	}); err != nil {
		t.Fatalf("tt verification plan naming the %s %s: %v\n%s", l.opts.planRepository, repository, err, out)
	}

	logs := filepath.Join(l.dir, "logs")
	if err = os.MkdirAll(logs, 0755); err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)
	receipt := api.VerificationReceipt{Worktree: filepath.Join(l.dir, "verifier"), Version: 1, OperationKey: plan.OperationKey, PlanDigest: lifecycleDigest(t, plan), Repository: plan.Repository, BaseCommit: plan.BaseCommit, Commit: plan.Commit, MatrixDigest: plan.MatrixDigest, ChecksDigest: plan.ChecksDigest, VerifierAgentID: verifier.ID, VerifierRunID: verifier.RunID, Detached: true, CleanBefore: true, CleanAfter: true, Environment: map[string]string{"HOME": "/tmp/tailterm-verification-fixture/home"}, Prerequisites: []api.VerificationPrerequisite{}, AIV: api.AIVBinding{State: "unsubmitted"}}
	attempts := 0
	for i, check := range checks {
		result := api.VerificationResult{VerificationCheck: check, Status: "flaky"}
		for n := 1; n <= lifecycleMaxAttempts; n++ {
			body := []byte(fmt.Sprintf("%s attempt %d\n", check.ID, n))
			path := filepath.Join(logs, fmt.Sprintf("%03d-attempt-%d.log", i, n))
			if err = os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			attempt := api.VerificationAttempt{Attempt: n, StartedAt: started.Format(time.RFC3339Nano), EndedAt: started.Add(time.Second).Format(time.RFC3339Nano), DurationMs: 1000, ExitCode: 1, FailureReason: "exit", LogURI: path, LogDigest: hex.EncodeToString(sum[:])}
			if n == lifecycleMaxAttempts {
				attempt.ExitCode, attempt.FailureReason = 0, ""
			}
			started = started.Add(2 * time.Second)
			result.Attempts = append(result.Attempts, attempt)
			attempts++
		}
		final := result.Attempts[lifecycleMaxAttempts-1]
		result.StartedAt, result.EndedAt, result.DurationMs, result.ExitCode, result.LogURI, result.LogDigest = final.StartedAt, final.EndedAt, final.DurationMs, final.ExitCode, final.LogURI, final.LogDigest
		receipt.Checks = append(receipt.Checks, result)
	}
	size, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	// The hub's shared request limit (api.MaxBody), which refused it live.
	limit := 64 * 1024
	if len(size) <= limit {
		t.Fatalf("the receipt is %d bytes, not over the shared %d-byte limit that refused it live", len(size), limit)
	}
	receiptFile := l.file(t, "receipt.json", receipt)
	if out, err = lifecycleCLI(t, func() error {
		return cmdVerification(l.handlerE, []string{"receipt", "--item", item.ID, "--file", receiptFile, "--request-id", l.key("receipt"), "--generation", "1"})
	}); err != nil {
		t.Fatalf("tt verification receipt of %d bytes (over the shared %d-byte limit): %v\n%s", len(size), limit, err, out)
	}
	var history []api.VerificationRecord
	l.read(t, &history, "tt verification history", func() error { return cmdVerification(l.handlerE, []string{"history", "--item", item.ID}) })
	if len(history) != 2 || history[1].Receipt == nil || len(history[1].Receipt.Checks) != lifecycleMatrixChecks || len(history[1].Receipt.Checks[lifecycleMatrixChecks-1].Attempts) != lifecycleMaxAttempts {
		t.Fatalf("the saved receipt lost checks or attempts: %d records", len(history))
	}
	l.step(t, "verification receipt: %d checks, %d attempts each (%d attempts), %d bytes (over %d), plan names the %s", lifecycleMatrixChecks, lifecycleMaxAttempts, attempts, len(size), limit, l.opts.planRepository)
}

// The owner resolves the disposition for exactly C2, the completion
// report is saved, and the item is saved done.
func (l *lifecycleRun) accept(t *testing.T) {
	t.Helper()
	if _, err := l.send(t, l.owner, "--kind", "notice", "--subject", "Owner accepts the verified candidate", "--text", "Candidate "+l.c2, "--review-file", l.file(t, "owner-accept.json", l.disposition("owner-accept", l.c2))); err != nil {
		t.Fatalf("the owner's owner-accept of a verified candidate after a %s disposition: %v", l.opts.disposition, err)
	}
	if got := l.reviews(t).Disposition; got == nil || got.Kind != "owner-accept" || got.Candidate != l.c2 || got.AgentID != "" {
		t.Fatalf("owner-accept is not the current disposition for C2: %+v", got)
	}
	item := l.current(t)
	reportFile := l.file(t, "report.json", api.PutNarrativeReportRequest{RequestID: l.key("report"), ScopeRevision: item.ScopeRevision,
		Sections:   api.NarrativeReportSections{RequestedOutcome: "Deliver the fix.", DeliveredWork: "Synthetic fix complete.", Verification: "Isolated fixture.", Limitations: "Fixture only.", RemainingWork: "Owner integration."},
		References: []api.NarrativeReference{{Kind: "work-item-revision", TaskID: l.task.ID, ItemID: item.ID, Revision: item.Revision, Label: "bounded scope"}}})
	out, err := lifecycleCLI(t, func() error {
		return cmdWorkItems(l.handlerE, []string{"narrative", "report-put", "--file", reportFile, item.ID})
	})
	if err != nil {
		t.Fatalf("tt work-items narrative report-put: %v\n%s", err, out)
	}
	var report api.NarrativeReportVersion
	if err = json.Unmarshal([]byte(out), &report); err != nil || report.ReportID == "" {
		t.Fatalf("completion report output: %v\n%s", err, out)
	}
	item = l.current(t)
	done := []string{"update", "--revision", fmt.Sprint(item.Revision), "--request-id", l.key("done"), "--status", "done",
		"--report-id", report.ReportID, "--report-version", fmt.Sprint(report.Version), "--report-digest", report.Digest, "--report-scope-revision", fmt.Sprint(report.ScopeRevision)}
	by := l.owner
	if !l.opts.ownerSavesDone {
		by = l.handlerE
		done = append(done, "--worktree", l.worktree, "--branch", lifecycleBranch, "--commit", l.c2)
	}
	if out, err = lifecycleCLI(t, func() error { return cmdWorkItems(by, append(done, item.ID)) }); err != nil {
		t.Fatalf("done save of C2 on the newer base B1 (queued on B0, plan names the %s): %v\n%s", l.opts.planRepository, err, out)
	}
	if got := l.current(t); got.Status != "done" {
		t.Fatalf("item is %s after the done save", got.Status)
	}
	if l.opts.ownerSavesDone {
		if entry := l.queueEntry(t); entry.Acceptance != nil {
			t.Fatalf("the owner's done save recorded queue acceptance: %+v", entry.Acceptance)
		}
		l.step(t, "acceptance: owner-accept for C2, completion report %s, item saved done by the owner", report.ReportID)
		return
	}
	l.step(t, "acceptance: owner-accept for C2, completion report %s, item saved done by the handler with the accepted worktree", report.ReportID)
}

// Queue acceptance from the builder worktree binds the verified base
// B1, not the queue-time base B0, and the runner reaches Ready to integrate.
func (l *lifecycleRun) queueAcceptOnNewerBase(t *testing.T) {
	t.Helper()
	before := l.queueEntry(t)
	out, err := lifecycleCLI(t, func() error {
		return cmdTeamQueue(l.handlerE, []string{"accept", "--entry", l.entry, "--worktree", l.worktree, "--branch", lifecycleBranch, "--commit", l.c2, "--evidence", "handler-saved acceptance receipt"})
	})
	if err != nil {
		t.Fatalf("tt team queue accept of C2 on the newer base B1 (queued on B0, plan names the %s): %v\n%s", l.opts.planRepository, err, out)
	}
	entry := l.queueEntry(t)
	if entry.Acceptance == nil || entry.Acceptance.Commit != l.c2 || entry.Acceptance.BaseCommit != l.b1 || entry.BaseCommit != l.b1 || entry.Acceptance.Worktree != l.worktree || entry.Acceptance.Repository != l.gitDir {
		t.Fatalf("acceptance is not bound to C2 %s on the verified base B1 %s: %+v", l.c2, l.b1, entry.Acceptance)
	}
	if !l.opts.ownerSavesDone && entry.Revision != before.Revision {
		t.Fatalf("re-accepting the saved tuple changed the entry: revision %d -> %d", before.Revision, entry.Revision)
	}
	for i := 0; i < 3 && entry.State != "finished"; i++ {
		if err = l.runner.tick(l.ctx, l.e, l.c, l.host); err != nil {
			t.Fatal(err)
		}
		entry = l.queueEntry(t)
	}
	if entry.State != "finished" || entry.Integration == nil || entry.Integration.Commit != l.c2 || entry.Integration.BaseCommit != l.b1 {
		t.Fatalf("runner did not reach Ready to integrate with C2 on B1: %+v", entry)
	}
	l.step(t, "queue accept on the newer base: C2 accepted on B1 %s (queued on B0), entry finished and ready to integrate", l.b1)
}

// Acceptance enqueued exactly one release job for C2 on B1; the
// handler's own enqueue is refused and adds none.
func (l *lifecycleRun) releaseJob(t *testing.T) {
	t.Helper()
	jobs := func() []api.ReleaseJob {
		t.Helper()
		var summaries []api.ReleaseSummary
		var snapshot string
		seen, seenJobs := map[string]bool{}, map[string]bool{}
		for _, view := range []string{"active", "settled"} {
			var after string
			var lastRow int64
			for {
				args := []string{"list", "--view", view}
				if after != "" {
					args = append(args, "--after", after)
				}
				if snapshot != "" {
					args = append(args, "--snapshot", snapshot)
				}
				out, err := lifecycleCLI(t, func() error { return cmdDeployment(l.handlerE, args) })
				if err != nil {
					t.Fatalf("tt deployment list: %v\n%s", err, out)
				}
				var page api.ReleasePage
				if err = json.Unmarshal([]byte(out), &page); err != nil {
					t.Fatalf("tt deployment list output: %v\n%s", err, out)
				}
				if page.Version != 1 || page.Page.View != view || page.Page.Limit < 1 || page.Page.Limit > 200 || len(page.Jobs) > page.Page.Limit || page.Page.Snapshot == "" || (snapshot != "" && page.Page.Snapshot != snapshot) || (page.Page.NextAfter != "" && len(page.Jobs) == 0) {
					t.Fatalf("tt deployment list returned an invalid page: %+v", page)
				}
				snapshot = page.Page.Snapshot
				for _, summary := range page.Jobs {
					if !summary.Summary || summary.ID == "" || seenJobs[summary.ID] || summary.RowID <= lastRow {
						t.Fatalf("tt deployment list returned an invalid summary: %+v", summary)
					}
					seenJobs[summary.ID], lastRow = true, summary.RowID
					summaries = append(summaries, summary)
				}
				if page.Page.NextAfter == "" {
					break
				}
				if seen[page.Page.NextAfter] {
					t.Fatalf("tt deployment list repeated cursor: %s", page.Page.NextAfter)
				}
				seen[page.Page.NextAfter] = true
				after = page.Page.NextAfter
			}
		}
		var list []api.ReleaseJob
		for _, summary := range summaries {
			out, err := lifecycleCLI(t, func() error {
				return cmdDeployment(l.handlerE, []string{"get", "--job", summary.ID})
			})
			if err != nil {
				t.Fatalf("tt deployment get: %v\n%s", err, out)
			}
			var job api.ReleaseJob
			if err = json.Unmarshal([]byte(out), &job); err != nil {
				t.Fatalf("tt deployment get output: %v\n%s", err, out)
			}
			if job.ID != summary.ID || job.TaskID != summary.TaskID || job.EntryID != summary.EntryID || job.ItemID != summary.ItemID || job.Generation != summary.Generation || job.State != summary.State {
				t.Fatalf("tt deployment get differs from listed summary: %+v versus %+v", job, summary)
			}
			list = append(list, job)
		}
		return list
	}
	list := jobs()
	if len(list) != 1 || list[0].EntryID != l.entry || list[0].ItemID != l.item.ID || list[0].Commit != l.c2 || list[0].BaseCommit != l.b1 || list[0].State != "verified" || len(list[0].Plan.Checks) != lifecycleMatrixChecks {
		t.Fatalf("no single verified release job for C2 on B1 bound to entry %s: %+v", l.entry, list)
	}
	_, err := lifecycleCLI(t, func() error {
		return cmdDeployment(l.handlerE, []string{"enqueue", "--entry", l.entry, "--request-id", l.key("enqueue")})
	})
	l.refused(t, "a second enqueue of the accepted entry", "entry already has release job ", err)
	l.refused(t, "a second enqueue of the accepted entry", " (verified); recover the original request receipt", err)
	if again := jobs(); len(again) != 1 || again[0].ID != list[0].ID || again[0].Generation != list[0].Generation {
		t.Fatalf("the refused enqueue changed the release ledger: %+v", again)
	}
	l.step(t, "release job enqueued: %s verified for C2 on B1, bound to entry %s", list[0].ID, l.entry)
}

// The matrix runs go test ./... in hub for every path under hub/, so a change
// to the store, the server or tt runs the lifecycle test with no check of its
// own (docs/objective-verification.md). This fails if that rule goes away.
func TestDeliveryLifecycleIsInTheVerificationMatrix(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "verification", "matrix.json"))
	if err != nil {
		t.Fatalf("the verification matrix is required: %v", err)
	}
	var matrix struct {
		Rules []struct {
			Prefixes []string `json:"prefixes"`
			Groups   []string `json:"groups"`
		} `json:"rules"`
	}
	if err = json.Unmarshal(raw, &matrix); err != nil {
		t.Fatal(err)
	}
	// The runner's rule match: a prefix ending in / matches by prefix.
	inGoGroup := func(path string) bool {
		for _, rule := range matrix.Rules {
			for _, prefix := range rule.Prefixes {
				if prefix != "hub/" || !strings.HasPrefix(path, prefix) {
					continue
				}
				for _, group := range rule.Groups {
					if group == "go" {
						return true
					}
				}
			}
		}
		return false
	}
	for _, path := range []string{"hub/internal/store/x.go", "hub/internal/server/x.go", "hub/cmd/tt/x.go"} {
		if !inGoGroup(path) {
			t.Errorf("%s no longer selects the go group, which runs go test ./... in hub and with it TestDeliveryLifecycle", path)
		}
	}
}
