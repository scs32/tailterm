package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// Steward rotation CLI tests (wi_5b4b94dbc9a11e8b, order #14942), on the
// isolated hub, private tmux socket and fake tt of newStewardCLI.

// rotationDeps launch the successor through the real cmdSpawn and
// ensureHandler and, unless offline, mark it started and idle, as a live
// Claude session's hooks would. Cleanup calls are counted.
func (f *stewardCLI) rotationDeps(t *testing.T, offline bool, cleanups *int) stewardDeps {
	d := f.deps()
	d.spawn = func(e env, args []string) error {
		if err := cmdSpawn(e, args); err != nil {
			return err
		}
		if offline {
			return nil
		}
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--agent-id" {
				a, err := f.c.GetAgent(context.Background(), f.task.ID, args[i+1])
				if err != nil {
					return err
				}
				f.live(t, a)
			}
		}
		return nil
	}
	cleanup := d.cleanup
	d.cleanup = func(ctx context.Context, e env, task, agent string) error {
		*cleanups++
		return cleanup(ctx, e, task, agent)
	}
	return d
}

// rotationSetup provisions a steward with a saved summary and gives it one
// role request and one by-name question from a worker.
func (f *stewardCLI) rotationSetup(t *testing.T, summary bool) (api.Agent, api.Agent) {
	t.Helper()
	steward := f.steward(t)
	// Count the old steward's launch before a rotation can stop its session.
	f.executions(t, 1)
	worker := f.agent(t, "builder", "")
	if summary {
		f.saveSummary(t, steward, 0, "## Themes\nsynthetic backlog", "summary-r1")
	}
	e := env{hub: f.c.Base, task: f.task.ID, agent: worker.ID, agentName: worker.Name, runID: worker.RunID}
	if _, err := captureStdout(t, func() error {
		return cmdSend(e, []string{"--kind", "request", "--to", "role:backlog_steward", "--subject", "Draft this synthetic intake", "--ask", "Please research it."})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error {
		return cmdSend(e, []string{"--kind", "question", "--to", steward.Name, "--subject", "Which batch holds the scroll items", "--question", "Which batch?"})
	}); err != nil {
		t.Fatal(err)
	}
	return steward, worker
}

func (f *stewardCLI) rotate(t *testing.T, d stewardDeps) (api.StewardRotation, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return rotateSteward(ctx, d, f.e, f.c, f.task.ID, api.StewardRotationReasonManual, api.StewardRotationTriggerOwner)
}

func (f *stewardCLI) stewards(t *testing.T) (open, total int) {
	t.Helper()
	agents, err := f.c.ListAgents(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Role == api.AgentRoleBacklogSteward {
			total++
			if a.Status != api.AgentClosed {
				open++
			}
		}
	}
	return open, total
}

func TestStewardRotationOwnerRotationEndToEnd(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	cleanups := 0
	d := f.rotationDeps(t, false, &cleanups)
	steward, worker := f.rotationSetup(t, false)
	// Prepare refusals change nothing.
	if _, err := f.rotate(t, d); err == nil || !strings.Contains(err.Error(), api.StewardRefusedSummaryMissing) {
		t.Fatalf("rotation without a summary: %v", err)
	}
	f.saveSummary(t, steward, 0, "## Themes\nsynthetic backlog", "summary-r1")
	now := time.Now().UTC()
	for _, state := range []string{"working", "idle"} {
		if state == "idle" {
			break
		}
		if _, err := f.c.ReportActivity(ctx, f.task.ID, steward.ID, api.ActivityReport{RequestID: api.NewID("act"), RunID: steward.RunID, Activity: api.AgentActivity{State: state, ObservedAt: now, LastEventAt: now}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.rotate(t, d); !stewardRotationBusy(err) {
		t.Fatalf("rotation while working: %v", err)
	}
	f.live(t, steward)
	if _, err := os.Stat(stewardRotationJournalPath(f.c.Base, f.task.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused prepare left a journal: %v", err)
	}
	// Watch the pending phase: the old steward still holds the slot and role.
	d.after = func(phase string) error {
		if phase != rotationPhaseSpawned {
			return nil
		}
		status, err := f.c.BacklogSteward(ctx, f.task.ID)
		if err != nil || status.Steward == nil || status.Steward.ID != steward.ID || status.PendingSuccessorID == "" {
			t.Errorf("status while the successor is pending: %+v %v", status, err)
		}
		return nil
	}
	r, err := f.rotate(t, d)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != api.StewardRotationCommitted || r.SuccessorName != "backlog-steward-r2" || r.Handoff == nil || r.Handoff.SummaryRevision != 1 || r.Handoff.SummaryDigest == "" ||
		r.Receipt == nil || r.Receipt.Reissued != 2 || cleanups != 1 {
		t.Fatalf("rotation: %+v cleanups=%d", r, cleanups)
	}
	old, err := f.c.GetAgent(ctx, f.task.ID, steward.ID)
	if err != nil || old.Status != api.AgentClosed || !old.CleanupDone {
		t.Fatalf("old steward after rotation: %+v %v", old, err)
	}
	if open, _ := f.stewards(t); open != 1 {
		t.Fatalf("open stewards after rotation: %d", open)
	}
	successor, err := f.c.GetAgent(ctx, f.task.ID, r.SuccessorAgentID)
	if err != nil || successor.Status == api.AgentClosed {
		t.Fatal(err)
	}
	open, err := f.c.ListObligations(ctx, f.task.ID, successor.ID, "", true, false)
	notices := 0
	for _, o := range open {
		if o.SourceKind == api.EnvelopeKindNotice {
			notices++
		}
	}
	if err != nil || len(open) != 3 || notices != 1 {
		t.Fatalf("successor obligations: %+v %v", open, err)
	}
	msgs, err := f.c.ListMessages(ctx, f.task.ID, 0, successor.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	handoffNotice := false
	for _, m := range msgs {
		if m.Seq == r.Receipt.NoticeSeq && strings.Contains(m.Text, r.ID) && strings.Contains(m.Text, "summary revision 1") {
			handoffNotice = true
		}
	}
	if !handoffNotice {
		t.Fatal("no directed handoff notice names the rotation and summary revision")
	}
	saved, err := loadStewardIdentity(f.c.Base, f.task.ID)
	if err != nil || saved.AgentID != successor.ID || saved.Name != successor.Name || saved.RotationID != r.ID {
		t.Fatalf("identity file after rotation: %+v %v", saved, err)
	}
	if _, err = os.Stat(stewardRotationJournalPath(f.c.Base, f.task.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal left after rotation: %v", err)
	}
	// A later setup reuses the successor and admits nothing new.
	_, total := f.stewards(t)
	again, err := setupSteward(ctx, f.deps(), f.e, f.c, f.task.ID, "", "", "")
	if err != nil || again.ID != successor.ID {
		t.Fatalf("setup after rotation: %+v %v", again, err)
	}
	if _, after := f.stewards(t); after != total {
		t.Fatalf("setup after rotation admitted a steward: %d -> %d", total, after)
	}
	// Intake now reaches the successor.
	e := env{hub: f.c.Base, task: f.task.ID, agent: worker.ID, agentName: worker.Name, runID: worker.RunID}
	if _, err = captureStdout(t, func() error {
		return cmdSend(e, []string{"--kind", "request", "--to", "role:backlog_steward", "--subject", "Intake after the rotation", "--ask", "Please research it."})
	}); err != nil {
		t.Fatal(err)
	}
	if open, _ = f.c.ListObligations(ctx, f.task.ID, successor.ID, "", true, false); len(open) != 4 {
		t.Fatalf("successor obligations after new intake: %d", len(open))
	}
}

func TestStewardRotationResumeAfterEachJournalPhase(t *testing.T) {
	for _, stop := range []string{rotationPhasePreparing, rotationPhasePrepared, rotationPhaseSpawned, rotationPhaseCommitted, stewardPhaseCleaned} {
		t.Run(stop, func(t *testing.T) {
			f := newStewardCLI(t)
			ctx := context.Background()
			cleanups := 0
			d := f.rotationDeps(t, false, &cleanups)
			f.rotationSetup(t, true)
			d.after = func(phase string) error {
				if phase == stop {
					return errors.New("interrupted at " + phase)
				}
				return nil
			}
			if _, err := f.rotate(t, d); err == nil || !strings.Contains(err.Error(), "interrupted at "+stop) {
				t.Fatalf("interrupted rotation: %v", err)
			}
			d.after = nil
			r, err := f.rotate(t, d)
			if err != nil || r.State != api.StewardRotationCommitted {
				t.Fatalf("resumed rotation: %+v %v", r, err)
			}
			open, total := f.stewards(t)
			if open != 1 || total != 2 {
				t.Fatalf("stewards after resume: open %d total %d", open, total)
			}
			f.executions(t, 2) // the old steward and exactly one successor
			moved, err := f.c.ListObligations(ctx, f.task.ID, r.SuccessorAgentID, "", true, false)
			if err != nil || len(moved) != 3 || r.Receipt.Reissued != 2 {
				t.Fatalf("obligations after resume: %d, receipt %+v, %v", len(moved), r.Receipt, err)
			}
			if cleanups != 1 {
				t.Fatalf("old session cleanups: %d", cleanups)
			}
		})
	}
}

func TestStewardRotationLaunchFailureKeepsOldStewardThenAbort(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	cleanups := 0
	steward, _ := f.rotationSetup(t, true)
	before, err := f.c.ListObligations(ctx, f.task.ID, steward.ID, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	identityBefore, _ := os.ReadFile(stewardIdentityPath(f.c.Base, f.task.ID))
	// The successor spawn fails outright.
	d := f.rotationDeps(t, false, &cleanups)
	d.spawn = func(env, []string) error { return errors.New("synthetic launch failure") }
	if _, err = f.rotate(t, d); err == nil || !strings.Contains(err.Error(), "stays prepared") {
		t.Fatalf("failed launch: %v", err)
	}
	rotations, err := f.c.ListStewardRotations(ctx, f.task.ID)
	if err != nil || len(rotations) != 1 || rotations[0].State != api.StewardRotationPrepared {
		t.Fatalf("rotation after failed launch: %+v %v", rotations, err)
	}
	if _, err = f.c.StewardRotationAction(ctx, f.task.ID, api.StewardRotationRequest{Operation: api.StewardRotationCommit, RequestID: "commit-probe", RotationID: rotations[0].ID}); !stewardRotationCode(err, api.StewardRefusedSuccessor) {
		t.Fatalf("commit without a successor: %v", err)
	}
	status, err := f.c.BacklogSteward(ctx, f.task.ID)
	if err != nil || status.Steward == nil || status.Steward.ID != steward.ID {
		t.Fatalf("steward after failed launch: %+v %v", status, err)
	}
	after, _ := f.c.ListObligations(ctx, f.task.ID, steward.ID, "", true, false)
	if len(after) != len(before) {
		t.Fatalf("old steward's obligations changed: %d -> %d", len(before), len(after))
	}
	aborted, err := abortStewardRotation(ctx, d, f.e, f.c, f.task.ID)
	if err != nil || aborted.State != api.StewardRotationAborted {
		t.Fatalf("abort: %+v %v", aborted, err)
	}
	if identityAfter, _ := os.ReadFile(stewardIdentityPath(f.c.Base, f.task.ID)); string(identityAfter) != string(identityBefore) {
		t.Fatal("abort rewrote the setup identity file")
	}
	// The successor launches but never comes online.
	d = f.rotationDeps(t, true, &cleanups)
	d.online = 300 * time.Millisecond
	if _, err = f.rotate(t, d); err == nil || !strings.Contains(err.Error(), "did not come online") {
		t.Fatalf("offline successor: %v", err)
	}
	rotations, _ = f.c.ListStewardRotations(ctx, f.task.ID)
	pending := rotations[len(rotations)-1]
	if pending.State != api.StewardRotationPrepared {
		t.Fatalf("rotation with an offline successor: %+v", pending)
	}
	if _, err = f.c.StewardRotationAction(ctx, f.task.ID, api.StewardRotationRequest{Operation: api.StewardRotationCommit, RequestID: "commit-offline", RotationID: pending.ID}); !stewardRotationCode(err, api.StewardRefusedSuccessor) {
		t.Fatalf("commit with an offline successor: %v", err)
	}
	if status, _ = f.c.BacklogSteward(ctx, f.task.ID); status.Steward == nil || status.Steward.ID != steward.ID {
		t.Fatalf("the offline successor took the role: %+v", status)
	}
	if _, err = abortStewardRotation(ctx, d, f.e, f.c, f.task.ID); err != nil {
		t.Fatal(err)
	}
	gone, err := f.c.GetAgent(ctx, f.task.ID, pending.SuccessorAgentID)
	if err != nil || gone.Status != api.AgentClosed || !gone.CleanupDone || cleanups != 1 {
		t.Fatalf("pending successor after abort: %+v cleanups=%d %v", gone, cleanups, err)
	}
	// A new rotation succeeds; there was never more or fewer than one holder.
	d = f.rotationDeps(t, false, &cleanups)
	if r, err := f.rotate(t, d); err != nil || r.State != api.StewardRotationCommitted {
		t.Fatalf("rotation after abort: %+v %v", r, err)
	}
	if open, _ := f.stewards(t); open != 1 {
		t.Fatalf("open stewards: %d", open)
	}
}

func stewardRotationCode(err error, code string) bool {
	var httpErr *api.HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict && httpErr.Code == code
}

func TestStewardRotationAgentCallerRefused(t *testing.T) {
	f := newStewardCLI(t)
	ctx := context.Background()
	steward, worker := f.rotationSetup(t, true)
	e := env{hub: f.c.Base, task: f.task.ID, agent: worker.ID, agentName: worker.Name, runID: worker.RunID}
	for _, args := range [][]string{{"rotate", "--task", f.task.ID}, {"rotate", "--task", f.task.ID, "--abort"}, {"setup", "--task", f.task.ID, "--cwd", f.dir},
		{"policy", "set", "--task", f.task.ID, "--revision", "0", "--enabled=false"}} {
		if err := cmdSteward(e, args); err == nil || !strings.Contains(err.Error(), "owner command") {
			t.Fatalf("%v from an agent session: %v", args, err)
		}
	}
	if _, err := f.c.StewardRotationAction(ctx, f.task.ID, api.StewardRotationRequest{Operation: api.StewardRotationPrepare, RequestID: "agent-prepare", ActorAgentID: steward.ID}); !stewardRotationCode(err, api.StewardRefusedAgentCaller) {
		t.Fatalf("agent caller over HTTP: %v", err)
	}
	if rotations, _ := f.c.ListStewardRotations(ctx, f.task.ID); len(rotations) != 0 {
		t.Fatalf("an agent caller created a rotation: %+v", rotations)
	}
}

func TestStewardRotationPolicyCLI(t *testing.T) {
	f := newStewardCLI(t)
	out, err := captureStdout(t, func() error { return cmdSteward(env{hub: f.c.Base}, []string{"policy", "get", "--task", f.task.ID}) })
	if err != nil || !strings.Contains(out, "(revision 0): enabled=true max-total-tokens=300000000 on-template-change=true") {
		t.Fatalf("policy get: %q %v", out, err)
	}
	if err = cmdSteward(env{hub: f.c.Base}, []string{"policy", "set", "--task", f.task.ID, "--revision", "3", "--max-total-tokens", "5000"}); !stewardRotationCode(err, api.StewardRefusedStalePolicy) {
		t.Fatalf("stale policy revision: %v", err)
	}
	out, err = captureStdout(t, func() error {
		return cmdSteward(env{hub: f.c.Base}, []string{"policy", "set", "--task", f.task.ID, "--revision", "0", "--max-total-tokens", "5000"})
	})
	if err != nil || !strings.Contains(out, "(revision 1): enabled=true max-total-tokens=5000 on-template-change=true") {
		t.Fatalf("policy set: %q %v", out, err)
	}
}

func TestStewardRotationTickNeedsLocalStewardSessionAndSpacesRequests(t *testing.T) {
	f := newStewardCLI(t)
	cleanups := 0
	clock := time.Now()
	runner := &stewardRotationRunner{deps: f.rotationDeps(t, false, &cleanups), now: func() time.Time { return clock }, interval: time.Minute}
	client := func() (*api.Client, error) { return f.c, nil }
	ctx := context.Background()
	host := spawn.Host()
	// No tt-steward-* session for this hub runs here: no request at all.
	if err := runner.hostTick(ctx, f.e, client, host); err != nil || f.calls.due.Load() != 0 {
		t.Fatalf("tick without a steward session: %v due=%d", err, f.calls.due.Load())
	}
	f.rotationSetup(t, true)
	if err := runner.hostTick(ctx, f.e, client, host); err != nil || f.calls.due.Load() != 1 {
		t.Fatalf("first tick: %v due=%d", err, f.calls.due.Load())
	}
	clock = clock.Add(30 * time.Second)
	if err := runner.hostTick(ctx, f.e, client, host); err != nil || f.calls.due.Load() != 1 {
		t.Fatalf("second tick within a minute: %v due=%d", err, f.calls.due.Load())
	}
	if f.calls.actions.Load() != 0 {
		t.Fatal("the runner rotated a current, under-limit steward")
	}
	// A template change makes the idle steward due; the runner rotates it.
	runner.digest = strings.Repeat("e", 64)
	clock = clock.Add(time.Minute)
	if err := runner.hostTick(ctx, f.e, client, host); err != nil || f.calls.due.Load() != 2 {
		t.Fatalf("tick after the interval: %v due=%d", err, f.calls.due.Load())
	}
	rotations, err := f.c.ListStewardRotations(ctx, f.task.ID)
	if err != nil || len(rotations) != 1 || rotations[0].State != api.StewardRotationCommitted || rotations[0].Trigger != api.StewardRotationTriggerRunner || rotations[0].Reason != api.StewardRotationReasonTemplate {
		t.Fatalf("runner rotation: %+v %v", rotations, err)
	}
}
