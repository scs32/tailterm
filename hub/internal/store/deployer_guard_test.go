package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Deployer guard tests use an isolated SQLite hub per test and synthetic
// agents and jobs only (wi_42bdd9989b3c098c, order #20613).

type guardFixture struct {
	s    *Store
	ctx  context.Context
	by   api.Caller
	task api.Task
	h, d api.Agent
	job  api.ReleaseJob
}

// newGuardFixture enqueues one release job and moves it to state through the
// real release actions: verified, claimed, merged or released.
func newGuardFixture(t *testing.T, state string) guardFixture {
	t.Helper()
	s, task, h, d, entry := releaseFixture(t)
	f := guardFixture{s: s, ctx: context.Background(), by: api.Caller{Node: "fixture", User: "owner"}, task: task, h: h, d: d}
	var err error
	f.job, err = s.ReleaseAction(f.ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"claim", "merged", "finish"} {
		if f.job.State == state {
			break
		}
		req := api.ReleaseRequest{RequestID: op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: f.job.ID, ExpectedGeneration: f.job.Generation}
		switch op {
		case "merged":
			req.IntegratedCommit = f.job.Commit
		case "finish":
			req.Receipt = &api.ReleaseReceipt{Version: 1, JobID: f.job.ID, Commit: f.job.Commit, VerificationDigest: f.job.VerificationDigest, Outcome: "released",
				Targets: []api.ReleaseTargetReceipt{{Target: "tailos", Release: "fixture", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "released"}}}
		}
		if f.job, err = s.ReleaseAction(f.ctx, task.ID, req); err != nil {
			t.Fatal(op, err)
		}
	}
	if f.job.State != state {
		t.Fatalf("fixture job is %s, want %s", f.job.State, state)
	}
	return f
}

func (f guardFixture) status(t *testing.T) string {
	t.Helper()
	a, err := f.s.GetAgent(f.ctx, f.d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a.Status
}

// moves are the ways a caller retires or closes an agent.
func (f guardFixture) moves() map[string]func() error {
	return map[string]func() error{
		"retire": func() error {
			status := api.AgentRetired
			_, err := f.s.UpdateAgent(f.ctx, f.d.ID, api.UpdateAgentRequest{Status: &status}, f.by)
			return err
		},
		"close": func() error {
			_, err := f.s.CloseAgent(f.ctx, f.d.ID, f.by)
			return err
		},
		"close run": func() error {
			_, err := f.s.CloseAgentRun(f.ctx, f.d.ID, f.d.RunID, f.by)
			return err
		},
		"closed event": func() error {
			_, err := f.s.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{AgentID: f.d.ID, RunID: f.d.RunID, Kind: api.EventClosed}, f.by)
			return err
		},
	}
}

// A deployer holding a claimed or merged job is neither retired nor closed;
// the refusal names the job, the runbook's pause step and the safe options.
func TestDeployerGuardRefusesRetireAndCloseWhileHoldingJob(t *testing.T) {
	for _, state := range []string{"claimed", "merged"} {
		f := newGuardFixture(t, state)
		for name, move := range f.moves() {
			err := move()
			if !errors.Is(err, api.ErrConflict) {
				t.Fatalf("%s %s: %v", state, name, err)
			}
			for _, want := range []string{f.job.ID, state, f.d.Name, `docs/project-deployment.md, "Pause"`, "Wait for the job to finish", "have the database handler set it aside while it has no effects"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("%s %s: refusal lacks %q: %v", state, name, want, err)
				}
			}
			if got := f.status(t); got != api.AgentRunning {
				t.Fatalf("%s %s: status became %s", state, name, got)
			}
		}
		// The fence still holds after the refusals.
		if _, err := f.s.ReleaseAction(f.ctx, f.task.ID, api.ReleaseRequest{RequestID: "check", Operation: "check", AgentID: f.d.ID, RunID: f.d.RunID, JobID: f.job.ID, ExpectedGeneration: f.job.Generation}); err != nil {
			t.Fatalf("%s: fence check after refusals: %v", state, err)
		}
	}
}

// With no claimed or merged job of its own the deployer retires and closes as
// before: a verified job, terminal jobs, and a fence held by another agent.
func TestDeployerGuardAllowsRetireAndCloseWithoutHeldJob(t *testing.T) {
	cases := map[string]func(t *testing.T) guardFixture{
		"verified": func(t *testing.T) guardFixture { return newGuardFixture(t, "verified") },
		"released": func(t *testing.T) guardFixture { return newGuardFixture(t, "released") },
	}
	for _, terminal := range []string{"refused", "superseded", "rolled_back"} {
		cases[terminal] = func(t *testing.T) guardFixture {
			f := newGuardFixture(t, "claimed")
			if _, err := f.s.db.Exec(`UPDATE release_jobs SET state=? WHERE task_id=? AND id=?`, terminal, f.task.ID, f.job.ID); err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	for _, state := range []string{"claimed", "merged"} {
		cases[state+" by another agent"] = func(t *testing.T) guardFixture {
			f := newGuardFixture(t, state)
			if _, err := f.s.db.Exec(`UPDATE release_jobs SET record_json=json_set(record_json,'$.agentId',?) WHERE task_id=? AND id=?`, f.h.ID, f.task.ID, f.job.ID); err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	for name, build := range cases {
		for move, want := range map[string]string{"retire": api.AgentRetired, "close": api.AgentClosed, "close run": api.AgentClosed, "closed event": api.AgentClosed} {
			f := build(t)
			if err := f.moves()[move](); err != nil {
				t.Fatalf("%s %s: %v", name, move, err)
			}
			if got := f.status(t); got != want {
				t.Fatalf("%s %s: status %s, want %s", name, move, got, want)
			}
		}
	}
}

// An exit is recorded even while the deployer holds a claimed or merged job.
func TestDeployerGuardAlwaysRecordsExit(t *testing.T) {
	for _, state := range []string{"claimed", "merged"} {
		exits := map[string]func(f guardFixture) error{
			"exited event": func(f guardFixture) error {
				_, err := f.s.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{AgentID: f.d.ID, RunID: f.d.RunID, Kind: api.EventExited}, f.by)
				return err
			},
			"exited status": func(f guardFixture) error {
				status := api.AgentExited
				_, err := f.s.UpdateAgent(f.ctx, f.d.ID, api.UpdateAgentRequest{Status: &status}, f.by)
				return err
			},
		}
		for name, exit := range exits {
			f := newGuardFixture(t, state)
			if err := exit(f); err != nil {
				t.Fatalf("%s %s: %v", state, name, err)
			}
			if got := f.status(t); got != api.AgentExited {
				t.Fatalf("%s %s: status %s", state, name, got)
			}
		}
	}
}
