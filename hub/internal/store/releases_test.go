package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func releaseFixture(t *testing.T) (*Store, api.Task, api.Agent, api.Agent, string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "release fixture"}, by)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "handler", Host: "fixture", Session: "handler", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "deployer", Host: "fixture", Session: "deployer", Role: api.AgentRoleDeployment}, by)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`UPDATE agents SET status='running',last_seen_at=? WHERE id=?`, ts(time.Now()), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, task, h, d, releaseEntry(t, s, task, "release item", "item")
}

// releaseEntry adds a done item with a passing verification and its finished,
// handler-accepted queue entry, ready for a release enqueue.
func releaseEntry(t *testing.T, s *Store, task api.Task, title, requestID string) string {
	t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: title, RequestID: requestID}, by)
	if err != nil {
		t.Fatal(err)
	}
	seedPassingVerification(t, s, item, candidateB)
	_, err = s.db.Exec(`UPDATE work_items SET status='done' WHERE id=?`, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := api.TeamIntegrationAcceptance{Repository: "fixture", BaseCommit: candidateA, Commit: candidateB, ItemRevision: item.Revision}
	raw, _ := json.Marshal(a)
	entry := api.NewID("tqe")
	_, err = s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,created_at,updated_at,repository,base_commit,acceptance_json) VALUES(?,?,?,?,1,'planned',1,'finished',?,?, 'fixture',?,?)`, entry, task.ID, item.ID, item.Revision, ts(time.Now()), ts(time.Now()), candidateA, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return entry
}
func TestReleaseEligibilityFencingRetryAndReceipt(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	enqueue := api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry}
	j, err := s.ReleaseAction(ctx, task.ID, enqueue)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ReleaseAction(ctx, task.ID, enqueue)
	if err != nil || again.ID != j.ID {
		t.Fatal(again, err)
	}
	action := func(op, key string) api.ReleaseRequest {
		return api.ReleaseRequest{RequestID: key, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}
	}
	req := action("claim", "claim")
	j, err = s.ReleaseAction(ctx, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, action("claim", "takeover")); !errors.Is(err, api.ErrConflict) {
		t.Fatal("takeover", err)
	}
	req = action("merged", "wrong-sha")
	req.IntegratedCommit = candidateA
	if _, err = s.ReleaseAction(ctx, task.ID, req); !errors.Is(err, api.ErrConflict) {
		t.Fatal("integrated receipt gate", err)
	}
	req = action("merged", "merged")
	req.IntegratedCommit = j.Commit
	j, err = s.ReleaseAction(ctx, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	req = action("finish", "finish")
	req.Receipt = &api.ReleaseReceipt{Version: 1, JobID: j.ID, Commit: j.Commit, VerificationDigest: j.VerificationDigest, Outcome: "released", Targets: []api.ReleaseTargetReceipt{{Target: "tailos", Release: "fixture", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "released"}}}
	final, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err = s.ReleaseAction(ctx, task.ID, req)
	if err != nil || again.Generation != final.Generation {
		t.Fatal("receipt retry", err)
	}
	req.Receipt.Outcome = "blocked"
	if _, err = s.ReleaseAction(ctx, task.ID, req); !errors.Is(err, api.ErrConflict) {
		t.Fatal("receipt mutation", err)
	}
}
func TestDeploymentSingletonRotationAndUnavailableRun(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	if _, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "other", Host: "fixture", Session: "other", Role: api.AgentRoleDeployment}, by); err == nil {
		t.Fatal("second deployer admitted")
	}
	req := api.AddAgentRequest{AgentID: d.ID, Name: d.Name, Host: d.Host, Session: d.Session, Role: d.Role}
	same, err := s.AddAgent(ctx, task.ID, req, by)
	if err != nil || same.RunID != d.RunID {
		t.Fatal("retry", err)
	}
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"retired", "exited", "closed"} {
		_, err = s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, state, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "claim-" + state, Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}); !errors.Is(err, api.ErrConflict) {
			t.Fatal(state, err)
		}
	}
	_, err = s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedRunID = d.RunID
	rotated, err := s.AddAgent(ctx, task.ID, req, by)
	if err != nil || rotated.RunID == d.RunID {
		t.Fatal("rotate", err)
	}
}

func TestDeploymentItemClosePreservesProjectRole(t *testing.T) {
	s, task, item, _, _, _, req := teamCloseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	d, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "deployer", Host: "fixture", Session: "deployer", Role: api.AgentRoleDeployment}, by)
	if err != nil {
		t.Fatal(err)
	}
	teamCloseTerminal(t, s, task, item, "dismissed")
	if _, err = s.CloseItemTeam(ctx, task.ID, req, by); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetAgent(ctx, d.ID)
	if err != nil || after.Status == api.AgentClosed || after.RunID != d.RunID {
		t.Fatal(after, err)
	}
}
func TestReleaseUnavailableHeartbeatPauseAndImportedMatrix(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	req := api.ReleaseRequest{RequestID: "stale", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}
	s.db.Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(time.Now().Add(-2*time.Minute)), d.ID)
	if _, err = s.ReleaseAction(ctx, task.ID, req); !errors.Is(err, api.ErrConflict) {
		t.Fatal("stale", err)
	}
	s.db.Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(time.Now()), d.ID)
	s.db.Exec(`UPDATE tasks SET pause_state='paused' WHERE id=?`, task.ID)
	req.RequestID = "paused"
	if _, err = s.ReleaseAction(ctx, task.ID, req); !errors.Is(err, api.ErrConflict) {
		t.Fatal("paused", err)
	}
	s.db.Exec(`UPDATE tasks SET pause_state='active' WHERE id=?`, task.ID)
	req.RequestID = "claim"
	j, err = s.ReleaseAction(ctx, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	p := j.Plan
	p.Commit = candidateA
	p.VerifierAgentID = d.ID
	p.VerifierRunID = d.RunID
	p.ApprovedMatrixDigest = p.MatrixDigest
	r := passingVerification(p)
	imported, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "import", Operation: "verification", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: p.Commit, Plan: &p, Verification: &r})
	if err != nil || imported.IntegratedCoverage != nil {
		t.Fatal(imported.IntegratedCoverage, err)
	}
	req = api.ReleaseRequest{RequestID: "merged", Operation: "merged", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: imported.Generation, IntegratedCommit: p.Commit}
	if _, err = s.ReleaseAction(ctx, task.ID, req); err != nil {
		t.Fatal(err)
	}
}

// goRaceMatrix is a 72-check matrix like the approved one: 71 fixed checks
// and a go-race whose packages come from the changed paths.
func goRaceMatrix(race api.VerificationCheck) []api.VerificationCheck {
	checks := make([]api.VerificationCheck, 0, 72)
	for i := range 71 {
		script := fmt.Sprintf("tests/fixture-%03d-browser.mjs", i)
		checks = append(checks, api.VerificationCheck{ID: fmt.Sprintf("%03d-%s", i, script), Argv: []string{"node", script, "--profile=/tmp/tailterm-verification-fixture-matrix/profiles/" + script, "--output=/tmp/tailterm-verification-fixture-matrix/artifacts/" + script}, Cwd: ".", Environment: map[string]string{"VERIFICATION_BASE_COMMIT": candidateA, "VERIFICATION_TIMEOUT_MS": "600000"}})
	}
	return append(checks, race)
}
func goRace(flags []string, packages ...string) api.VerificationCheck {
	return api.VerificationCheck{ID: "go-race", Argv: append(append([]string{"go", "test"}, flags...), packages...), Cwd: "hub", Environment: map[string]string{"VERIFICATION_BASE_COMMIT": candidateA, "VERIFICATION_TIMEOUT_MS": "1800000"}}
}

var raceFlags = []string{"-race", "-timeout=25m"}

// claimGoRaceJob enqueues and claims the fixture job, then gives it an
// approved plan with the given checks (test-only rewrite of the snapshot).
func claimGoRaceJob(t *testing.T, s *Store, task api.Task, h, d api.Agent, entry string, approved []api.VerificationCheck) api.ReleaseJob {
	t.Helper()
	ctx := context.Background()
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "race-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "race-claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
	if err != nil {
		t.Fatal(err)
	}
	return approveChecks(t, s, j, approved)
}
func approveChecks(t *testing.T, s *Store, j api.ReleaseJob, approved []api.VerificationCheck) api.ReleaseJob {
	t.Helper()
	j.Plan.Checks = approved
	j.Plan.ChecksDigest = verificationDigest(approved)
	raw, _ := json.Marshal(j)
	if _, err := s.db.Exec(`UPDATE release_jobs SET record_json=? WHERE task_id=? AND id=?`, string(raw), j.TaskID, j.ID); err != nil {
		t.Fatal(err)
	}
	return j
}

// integratedImport is the handler's import of the deployer's integrated run.
func integratedImport(j api.ReleaseJob, h, d api.Agent, requestID string, checks []api.VerificationCheck) api.ReleaseRequest {
	p := j.Plan
	p.Commit = candidateA
	p.VerifierAgentID = d.ID
	p.VerifierRunID = d.RunID
	p.ApprovedMatrixDigest = p.MatrixDigest
	p.Checks = checks
	p.ChecksDigest = verificationDigest(checks)
	r := passingVerification(p)
	return api.ReleaseRequest{RequestID: requestID, Operation: "verification", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: p.Commit, Plan: &p, Verification: &r}
}

var widerPackages = []string{"./cmd/tt", "./internal/api", "./internal/server", "./internal/spawn", "./internal/store"}

// A cherry-pick onto a moved tasks-hub widens go-race to the intervening
// commits' packages; that import is accepted and recorded, not refused.
func TestReleaseImportAcceptsGoRacePackageSuperset(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	approved := goRaceMatrix(goRace(raceFlags, "./cmd/tt"))
	j := claimGoRaceJob(t, s, task, h, d, entry, approved)
	refuse := func(name, want string, checks []api.VerificationCheck) {
		t.Helper()
		_, err := s.ReleaseAction(ctx, task.ID, integratedImport(j, h, d, "race-"+name, checks))
		if !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "integrated matrix omitted approved check "+want) {
			t.Fatalf("%s: %v", name, err)
		}
		saved, err := releaseLoad(ctx, s.db, task.ID, j.ID)
		if err != nil || saved.Generation != j.Generation || saved.IntegratedPlan != nil || saved.IntegratedCoverage != nil {
			t.Fatal(name, "changed the job", saved.Generation, saved.IntegratedCoverage, err)
		}
	}
	refuse("missing", "go-race", goRaceMatrix(goRace(raceFlags, widerPackages[1:]...)))
	refuse("timeout", "go-race", goRaceMatrix(goRace([]string{"-race", "-timeout=14m"}, widerPackages...)))
	refuse("no-race", "go-race", goRaceMatrix(goRace([]string{"-timeout=25m"}, widerPackages...)))
	refuse("flag-after", "go-race", goRaceMatrix(goRace(raceFlags, append(slices.Clone(widerPackages), "-run", "TestX")...)))
	moved := goRace(raceFlags, widerPackages...)
	moved.Cwd = "."
	refuse("cwd", "go-race", goRaceMatrix(moved))
	env := goRace(raceFlags, widerPackages...)
	env.Environment["VERIFICATION_TIMEOUT_MS"] = "900000"
	refuse("environment", "go-race", goRaceMatrix(env))
	changed := goRaceMatrix(goRace(raceFlags, "./cmd/tt"))
	changed[7].Argv = append(slices.Clone(changed[7].Argv), "--headed")
	refuse("other-check", changed[7].ID, changed)
	// An approved "./..." is covered only by "./...", never by a list.
	j = approveChecks(t, s, j, goRaceMatrix(goRace(raceFlags, "./...")))
	refuse("all-vs-list", "go-race", goRaceMatrix(goRace(raceFlags, widerPackages...)))
	j = approveChecks(t, s, j, approved)

	integrated := goRaceMatrix(goRace(raceFlags, widerPackages...))
	req := integratedImport(j, h, d, "race-import", integrated)
	if raw, _ := json.Marshal(req); len(raw) <= api.MaxBody {
		t.Fatalf("import is %d bytes, not over the shared %d-byte limit", len(raw), api.MaxBody)
	}
	imported, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	want := []api.ReleaseCheckCoverage{{CheckID: "go-race", ApprovedDigest: verificationDigest(approved[71]), IntegratedDigest: verificationDigest(integrated[71]), Relation: "superset"}}
	if !slices.Equal(imported.IntegratedCoverage, want) || imported.Generation != j.Generation+1 || imported.IntegratedCommit != candidateA {
		t.Fatalf("coverage %+v generation %d", imported.IntegratedCoverage, imported.Generation)
	}
	retry, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil || retry.Generation != imported.Generation || !slices.Equal(retry.IntegratedCoverage, want) {
		t.Fatal("same request id did not replay the saved import", retry.Generation, err)
	}
	merged, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "race-merged", Operation: "merged", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: imported.Generation, IntegratedCommit: candidateA})
	if err != nil || merged.State != "merged" || !slices.Equal(merged.IntegratedCoverage, want) {
		t.Fatal(merged.State, err)
	}
}

// A requeued job starts its integrated verification from scratch, coverage
// included.
func TestReleaseRequeueClearsIntegratedCoverage(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	j := claimGoRaceJob(t, s, task, h, d, entry, goRaceMatrix(goRace(raceFlags, "./cmd/tt")))
	imported, err := s.ReleaseAction(ctx, task.ID, integratedImport(j, h, d, "race-import", goRaceMatrix(goRace(raceFlags, widerPackages...))))
	if err != nil || len(imported.IntegratedCoverage) != 1 {
		t.Fatal(imported.IntegratedCoverage, err)
	}
	s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, d.ID)
	evidence := recoveryEvidence(t, s, task, h, imported)
	requeued, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "race-requeue", Operation: "reconcile", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: imported.Generation, Reconciliation: &evidence})
	if err != nil || requeued.State != "verified" || requeued.IntegratedCoverage != nil || requeued.IntegratedPlan != nil || requeued.IntegratedVerification != nil {
		t.Fatal(requeued.State, requeued.IntegratedCoverage, requeued.IntegratedPlan != nil, requeued.IntegratedVerification != nil, err)
	}
}

func TestReleaseRehearsalRejectsForeignKeyDamage(t *testing.T) {
	s, _, _, _, _ := releaseFixture(t)
	ctx := context.Background()
	if err := s.ValidateReleaseDatabase(ctx); err != nil {
		t.Fatal(err)
	}
	// Synthetic orphan on a single connection models a structurally valid but
	// semantically damaged imported backup without printing any row contents.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, `UPDATE agents SET task_id='missing-fixture-task'`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if err = s.ValidateReleaseDatabase(ctx); err == nil {
		t.Fatal("damaged backup passed rehearsal")
	}
}

func recoveryEvidence(t *testing.T, s *Store, task api.Task, h api.Agent, j api.ReleaseJob) api.ReleaseReconciliation {
	t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Fixture incident and prevention", Description: "Inspect exact host execution; prevent automatic ambiguous replay", RequestID: api.NewID("req")}, by)
	if err != nil {
		t.Fatal(err)
	}
	order, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Bounded fixture prevention order; handler owns host inspection; verify no active execution and exact journal", RequestID: api.NewID("req"), WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: 1, Relationship: "primary"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: api.NewID("req"), AgentID: h.ID, RunID: h.RunID, ExpectedRevision: 1, ScopeRevision: 1, OrderMessageSeq: order.Seq, Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	return api.ReleaseReconciliation{LastActionAt: ts(time.Now()), StoppedAt: ts(time.Now()), ExpectedNextAction: "Retry after inspection", ContributingConditions: "Synthetic fixture exit", UnresolvedQuestions: "None after fixture inspection", JobID: j.ID, AgentID: j.AgentID, RunID: j.RunID, PauseGeneration: j.PauseGeneration, Disposition: "requeue", IncidentBugID: item.ID, IncidentDigest: strings.Repeat("c", 64), JournalDigest: strings.Repeat("d", 64), ObservedAt: ts(time.Now()), StopReason: "Synthetic predecessor exited", LastAction: "Detached integration", CausalEvidence: "Fixture host inspection and exact-run exit", PreventionOwner: h.Name, PreventionItemID: item.ID, PreventionOrderMessage: order.Seq, PreventionCriterion: "No process or effect may replay", JournalState: "no_effects", NoActiveExecution: true, NoPublication: true, RefResolved: true}
}
func TestReleaseCorrectionInputsAreHandlerOwnedAndWriteOnce(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "input-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "input-claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
	if err != nil {
		t.Fatal(err)
	}
	req := api.ReleaseRequest{RequestID: "input-native", Operation: "inputs", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit, InputsDigest: strings.Repeat("e", 64)}
	if _, err = s.ReleaseAction(ctx, task.ID, req); err == nil {
		t.Fatal("deployer self-imported input pin")
	}
	req.AgentID = h.ID
	req.RunID = h.RunID
	wrong := req
	wrong.RequestID = "input-wrong-sha"
	wrong.IntegratedCommit = candidateA
	if _, err = s.ReleaseAction(ctx, task.ID, wrong); err == nil {
		t.Fatal("wrong integrated input SHA")
	}
	pinned, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil || pinned.InputsDigest != req.InputsDigest || pinned.InputsCommit != j.Commit {
		t.Fatal(pinned, err)
	}
	retry, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil || retry.Generation != pinned.Generation {
		t.Fatal(retry, err)
	}
	req.RequestID = "input-overwrite"
	req.ExpectedGeneration = pinned.Generation
	req.InputsDigest = strings.Repeat("f", 64)
	if _, err = s.ReleaseAction(ctx, task.ID, req); err == nil {
		t.Fatal("input digest overwritten")
	}
}
func TestReleaseCorrectionRecoveryInspectsRotatedRunAndReleasesFence(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "recover-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "recover-claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
	if err != nil {
		t.Fatal(err)
	}
	evidence := recoveryEvidence(t, s, task, h, j)
	req := api.ReleaseRequest{RequestID: "recover", Operation: "reconcile", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Reconciliation: &evidence}
	if _, err = s.ReleaseAction(ctx, task.ID, req); err == nil {
		t.Fatal("active execution recovered")
	}
	s.db.Exec(`UPDATE agents SET status='retired' WHERE id=?`, d.ID)
	if _, err = s.ReleaseAction(ctx, task.ID, req); err == nil {
		t.Fatal("retirement treated as exit")
	}
	s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, d.ID)
	rotated, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: d.ID, ExpectedRunID: d.RunID, Name: d.Name, Role: d.Role, Host: d.Host, Session: d.Session}, by)
	if err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`UPDATE agents SET status='running',last_seen_at=? WHERE id=?`, ts(time.Now()), rotated.ID)
	ambiguous := evidence
	ambiguous.JournalState = "unknown"
	wrong := req
	wrong.Reconciliation = &ambiguous
	if _, err = s.ReleaseAction(ctx, task.ID, wrong); err == nil {
		t.Fatal("ambiguous journal recovered")
	}
	requeued, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil || requeued.State != "verified" || requeued.AgentID != "" || len(requeued.Reconciliations) != 1 {
		t.Fatal(requeued, err)
	}
	retry, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil || retry.Generation != requeued.Generation {
		t.Fatal(retry, err)
	}
	j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "rotated-claim", Operation: "claim", AgentID: rotated.ID, RunID: rotated.RunID, JobID: j.ID, ExpectedGeneration: requeued.Generation})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "old-run", Operation: "check", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}); err == nil {
		t.Fatal("old run executed")
	}
	// Refusal before merge is terminal and excludes no later job from the fence.
	j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "refuse", Operation: "refuse", AgentID: rotated.ID, RunID: rotated.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
	if err != nil || j.State != "refused" {
		t.Fatal(j, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM release_jobs WHERE task_id=? AND state IN ('claimed','merged','blocked')`, task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}
func TestReleaseCorrectionHandlerRefreshesUnclaimedJobAfterRealProjectPause(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "pause-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	evidence := recoveryEvidence(t, s, task, h, j)
	paused, err := s.PauseProject(ctx, task.ID, api.PauseProjectRequest{Version: 1, RequestID: "real-pause", Targets: []api.ProjectPauseTargetRequest{{AgentID: h.ID, RunID: h.RunID, ServiceDisposition: api.PauseServiceNone}, {AgentID: d.ID, RunID: d.RunID, ServiceDisposition: api.PauseServiceNone}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []api.Agent{h, d} {
		if _, err = s.ReportCleanup(ctx, a.ID, api.CleanupRequest{RunID: a.RunID}, by); err != nil {
			t.Fatal(err)
		}
	}
	status, err := s.ProjectPauseStatus(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	lead := api.ProjectResumeOrchestrator{AgentID: api.NewID("agt"), RunID: api.NewID("run"), Name: "fresh-lead"}
	resumed, err := s.ResumeProject(ctx, task.ID, api.ResumeProjectRequest{Version: 1, RequestID: "real-resume", ExpectedPauseGeneration: paused.PauseGeneration, ExpectedLifecycleGeneration: paused.LifecycleGeneration, RetainedHandoffDigest: status.RetainedHandoffDigest, SelectedTeamID: "fixture-team", Orchestrator: lead}, by)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: lead.AgentID, ExpectedRunID: lead.RunID, ResumeReceiptID: resumed.Receipt.ID, Name: lead.Name, Host: "fixture", Session: "fresh", ExpectedLifecycleGeneration: resumed.LifecycleGeneration}, by)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ConfirmProjectResume(ctx, task.ID, api.ConfirmProjectResumeRequest{Version: 1, RequestID: "real-confirm", ExpectedLifecycleGeneration: resumed.LifecycleGeneration, ResumeReceiptID: resumed.Receipt.ID, AgentID: fresh.ID, RunID: fresh.RunID}, by)
	if err != nil {
		t.Fatal(err)
	}
	h, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "fresh-handler", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "fresh-handler", ExpectedLifecycleGeneration: resumed.LifecycleGeneration}, by)
	if err != nil {
		t.Fatal(err)
	}
	d, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "fresh-deployer", Role: api.AgentRoleDeployment, Host: "fixture", Session: "fresh-deployer", ExpectedLifecycleGeneration: resumed.LifecycleGeneration}, by)
	if err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`UPDATE agents SET status='running',last_seen_at=? WHERE id=?`, ts(time.Now()), d.ID)
	if _, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "stale-pause-claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}); err == nil {
		t.Fatal("stale pause job claimed")
	}
	j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "pause-reconcile", Operation: "reconcile", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Reconciliation: &evidence})
	if err != nil || j.PauseGeneration != paused.PauseGeneration {
		t.Fatal(j, err)
	}
	_, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "fresh-pause-claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReleaseCorrectionHandlerRefusesResolvedHeldJobWithoutAmbiguousTakeover(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "held-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "held-claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
	if err != nil {
		t.Fatal(err)
	}
	j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "held-block", Operation: "block", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
	if err != nil {
		t.Fatal(err)
	}
	evidence := recoveryEvidence(t, s, task, h, j)
	evidence.NoPublication = false
	evidence.JournalState = "restored"
	s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, d.ID)
	req := api.ReleaseRequest{RequestID: "held-recovery", Operation: "reconcile", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Reconciliation: &evidence}
	if _, err = s.ReleaseAction(ctx, task.ID, req); err == nil {
		t.Fatal("published job requeued")
	}
	evidence.Disposition = "refuse"
	evidence.RefResolved = false
	if _, err = s.ReleaseAction(ctx, task.ID, req); err == nil {
		t.Fatal("unresolved release ref released")
	}
	evidence.RefResolved = true
	j, err = s.ReleaseAction(ctx, task.ID, req)
	if err != nil || j.State != "refused" {
		t.Fatal(j, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM release_jobs WHERE task_id=? AND state IN ('claimed','merged','blocked')`, task.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

// A verified team launches on the tasks-hub tip at launch, which is newer than
// the base recorded when the item was queued, and its plan names the builder
// worktree. Queue acceptance must still bind the verified base and repository.
func TestQueueAcceptanceUsesVerifiedBaseAndWorktree(t *testing.T) {
	verifiedAcceptance(t, "/fixture/worktrees/builder")
}

// The plan may also name the repository root, the directory holding .git
// (wi_0842a90ecb62e82a, profile-sync acceptance on 2026-09-28).
func TestQueueAcceptanceAcceptsVerifiedRepositoryRoot(t *testing.T) {
	verifiedAcceptance(t, "/fixture/repo")
}

func TestVerifiedRepositoryIdentity(t *testing.T) {
	a := api.TeamIntegrationAcceptance{Repository: "/fixture/repo/.git", Worktree: "/fixture/worktrees/builder"}
	for plan, want := range map[string]bool{
		"/fixture/repo/.git": true, "/fixture/repo": true, "/fixture/repo/": true, "/fixture/worktrees/builder": true,
		"/fixture/other": false, "/fixture": false, "/fixture/repo/sub": false, "/fixture/worktrees/other": false,
	} {
		if got := verifiedRepository(plan, a); got != want {
			t.Errorf("verifiedRepository(%q) = %v, want %v", plan, got, want)
		}
	}
	if verifiedRepository("/fixture", api.TeamIntegrationAcceptance{Repository: "/fixture/repo", Worktree: "/fixture/wt"}) {
		t.Error("a parent directory matched a repository that is not a .git directory")
	}
}

func verifiedAcceptance(t *testing.T, planRepository string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "verified acceptance"}, by)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "handler", Host: "fixture", Session: "handler", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "verified item", RequestID: "item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	const repository, worktree = "/fixture/repo/.git", "/fixture/worktrees/builder"
	seedPassingVerificationAt(t, s, item, candidateB, planRepository)
	now := ts(time.Now())
	if _, err = s.db.Exec(`UPDATE work_items SET status='done' WHERE id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO verification_enrollments(task_id,item_id,agent_id,run_id,required,provenance,created_at) VALUES(?,?,?,?,1,'fixture',?)`, task.ID, item.ID, api.NewID("agt"), api.NewID("run"), now); err != nil {
		t.Fatal(err)
	}
	entry := api.NewID("tqe")
	if _, err = s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,created_at,updated_at,repository,base_commit,handler_id,handler_run_id) VALUES(?,?,?,?,1,'planned',1,'running',1,?,?,?,?,?,?)`, entry, task.ID, item.ID, item.Revision, now, now, repository, candidateC, h.ID, h.RunID); err != nil {
		t.Fatal(err)
	}
	accept := func(key, planRepository, base string) (api.TeamQueueEntry, error) {
		current, err := s.GetTeamQueueEntry(ctx, task.ID, entry)
		if err != nil {
			t.Fatal(err)
		}
		return s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: key, Operation: "accept", EntryID: entry, ExpectedRevision: current.Revision, HandlerAgentID: h.ID, HandlerRunID: h.RunID,
			Acceptance: &api.TeamIntegrationAcceptance{Repository: repository, BaseCommit: base, Worktree: planRepository, Branch: "feature/fixture", Commit: candidateB, ItemRevision: item.Revision, Evidence: "handler terminal save"}})
	}
	if planRepository == worktree {
		if _, err = accept("other-worktree", "/fixture/worktrees/other", candidateC); !errors.Is(err, api.ErrConflict) {
			t.Fatal("a plan for a different worktree was accepted", err)
		}
	}
	if _, err = accept("wrong-base", worktree, strings.Repeat("d", 40)); !errors.Is(err, api.ErrConflict) {
		t.Fatal("an unrelated base was accepted", err)
	}
	got, err := accept("queued-base", worktree, candidateC)
	if err != nil {
		t.Fatal("verified acceptance", err)
	}
	if got.BaseCommit != candidateA || got.Acceptance == nil || got.Acceptance.BaseCommit != candidateA || got.Acceptance.Repository != repository {
		t.Fatalf("acceptance did not bind the verified base: %+v", got)
	}
	if got.Release == nil || got.Release.BaseCommit != candidateA || got.Release.Commit != candidateB {
		t.Fatalf("release job not bound to the verified candidate: %+v", got.Release)
	}
	reread, err := s.GetTeamQueueEntry(ctx, task.ID, entry)
	if err != nil || reread.BaseCommit != candidateA {
		t.Fatal("verified base not persisted", reread.BaseCommit, err)
	}
}

// f7: publication survives a later block so Delivery keeps showing merged.
func TestReleasePublishedMarkerSurvivesBlock(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"claim", "merged", "block"} {
		req := api.ReleaseRequest{RequestID: op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit}
		if j, err = s.ReleaseAction(ctx, task.ID, req); err != nil {
			t.Fatal(op, err)
		}
		if j.Published != (op != "claim") {
			t.Fatalf("%s published=%v", op, j.Published)
		}
	}
	if j.State != "blocked" {
		t.Fatal(j.State)
	}
}

// D1/D2: finish accepts a well-formed revert or push and rejects malformed ones.
func TestReleaseReceiptRevertAndPushValidation(t *testing.T) {
	sha := strings.Repeat("c", 40)
	receipt := func(outcome string) *api.ReleaseReceipt {
		return &api.ReleaseReceipt{Version: 1, Commit: candidateB, Outcome: outcome}
	}
	cases := []struct {
		name string
		r    *api.ReleaseReceipt
		ok   bool
	}{
		{"revert committed", func() *api.ReleaseReceipt {
			r := receipt("rolled_back")
			r.Revert = &api.ReleaseRevert{Commit: sha, Outcome: "committed", BugRequestID: "rel_x-rollback-bug"}
			return r
		}(), true},
		{"revert failed", func() *api.ReleaseReceipt {
			r := receipt("blocked")
			r.Revert = &api.ReleaseRevert{Outcome: "failed"}
			return r
		}(), true},
		{"revert on release", func() *api.ReleaseReceipt {
			r := receipt("released")
			r.Revert = &api.ReleaseRevert{Commit: sha, Outcome: "committed"}
			return r
		}(), false},
		{"revert committed without commit", func() *api.ReleaseReceipt {
			r := receipt("blocked")
			r.Revert = &api.ReleaseRevert{Outcome: "committed"}
			return r
		}(), false},
		{"revert newline", func() *api.ReleaseReceipt {
			r := receipt("blocked")
			r.Revert = &api.ReleaseRevert{Commit: sha, Outcome: "committed", BugRequestID: "a\nb"}
			return r
		}(), false},
		{"push pushed", func() *api.ReleaseReceipt {
			r := receipt("released")
			r.Push = &api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "pushed"}
			return r
		}(), true},
		{"push failed", func() *api.ReleaseReceipt {
			r := receipt("released")
			r.Push = &api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "failed"}
			return r
		}(), true},
		{"push after rollback", func() *api.ReleaseReceipt {
			r := receipt("rolled_back")
			r.Push = &api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "pushed"}
			return r
		}(), false},
		{"push other commit", func() *api.ReleaseReceipt {
			r := receipt("released")
			r.Push = &api.ReleasePush{Remote: "origin", Commit: sha, Outcome: "pushed"}
			return r
		}(), false},
		{"push remote url", func() *api.ReleaseReceipt {
			r := receipt("released")
			r.Push = &api.ReleasePush{Remote: "https://token@example/x", Commit: candidateB, Outcome: "pushed"}
			return r
		}(), false},
	}
	for _, c := range cases {
		if err := validateReleaseRefEffects(c.r); (err == nil) != c.ok {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	// Round trip through finish: the runner's rollback receipt shape is saved.
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"claim", "merged"} {
		if j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit}); err != nil {
			t.Fatal(op, err)
		}
	}
	bad := &api.ReleaseReceipt{Version: 1, JobID: j.ID, Commit: j.Commit, VerificationDigest: j.VerificationDigest, Outcome: "rolled_back", Targets: []api.ReleaseTargetReceipt{}, Revert: &api.ReleaseRevert{Outcome: "committed"}}
	if _, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "finish-bad", Operation: "finish", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Receipt: bad}); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("malformed revert", err)
	}
	good := *bad
	good.Revert = &api.ReleaseRevert{Commit: sha, Outcome: "committed", BugRequestID: j.ID + "-rollback-bug"}
	final, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "finish", Operation: "finish", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Receipt: &good})
	if err != nil || final.State != "rolled_back" || final.Receipt.Revert.Commit != sha {
		t.Fatal(final, err)
	}
}

// Q2: only the handler supersedes, only a verified never-claimed job, and a
// superseded job is terminal: never claimed and outside the project fence.
func TestReleaseSupersedeIsHandlerOnlyForUnclaimedJobs(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	record := &api.ReleaseSupersession{ReleasedCommit: candidateB, Release: "20260929-owner-helper-c6a8ec1"}
	req := func(key string, a api.Agent, gen int64, sup *api.ReleaseSupersession) api.ReleaseRequest {
		return api.ReleaseRequest{RequestID: key, Operation: "supersede", AgentID: a.ID, RunID: a.RunID, JobID: j.ID, ExpectedGeneration: gen, Supersession: sup}
	}
	if _, err = s.ReleaseAction(ctx, task.ID, req("by-deployer", d, j.Generation, record)); !errors.Is(err, api.ErrConflict) {
		t.Fatal("deployer superseded", err)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, req("no-record", h, j.Generation, &api.ReleaseSupersession{ReleasedCommit: "abc", Release: "x"})); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("malformed supersession", err)
	}
	done, err := s.ReleaseAction(ctx, task.ID, req("supersede", h, j.Generation, record))
	if err != nil || done.State != "superseded" || done.Supersession.ReleasedCommit != candidateB || done.Supersession.Release != record.Release || done.Supersession.AgentID != h.ID {
		t.Fatal(done, err)
	}
	if again, err := s.ReleaseAction(ctx, task.ID, req("supersede", h, j.Generation, record)); err != nil || again.Generation != done.Generation {
		t.Fatal("retry", err)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: done.Generation}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("superseded job claimed", err)
	}
	var fenced int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM release_jobs WHERE task_id=? AND state IN ('claimed','merged','blocked')`, task.ID).Scan(&fenced); err != nil || fenced != 0 {
		t.Fatal("fence", fenced, err)
	}
	// A claimed job is never superseded.
	s2, task2, h2, d2, entry2 := releaseFixture(t)
	k, err := s2.ReleaseAction(ctx, task2.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h2.ID, RunID: h2.RunID, EntryID: entry2})
	if err != nil {
		t.Fatal(err)
	}
	if k, err = s2.ReleaseAction(ctx, task2.ID, api.ReleaseRequest{RequestID: "claim", Operation: "claim", AgentID: d2.ID, RunID: d2.RunID, JobID: k.ID, ExpectedGeneration: k.Generation}); err != nil {
		t.Fatal(err)
	}
	if _, err = s2.ReleaseAction(ctx, task2.ID, api.ReleaseRequest{RequestID: "supersede", Operation: "supersede", AgentID: h2.ID, RunID: h2.RunID, JobID: k.ID, ExpectedGeneration: k.Generation, Supersession: record}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("claimed job superseded", err)
	}
}

// setAsideEvidence is the handler's typed record for setting aside claimed j.
func setAsideEvidence(t *testing.T, s *Store, task api.Task, h api.Agent, j api.ReleaseJob) api.ReleaseReconciliation {
	t.Helper()
	r := recoveryEvidence(t, s, task, h, j)
	r.Disposition = "set_aside"
	r.StopReason = "Integrated import refused; the fix is the next queued job"
	r.JournalState = ""
	r.JournalDigest = ""
	r.NoActiveExecution = false
	r.NoPublication = false
	r.RefResolved = false
	return r
}

// A claimed job whose integrated import is refused would wait forever; the
// fix for that refusal, queued behind it, claims once the handler sets the
// stuck job aside, and the stuck job is claimed afresh after the fix ships.
func TestReleaseSetAsideLetsQueuedJobClaimFence(t *testing.T) {
	s, task, h, d, entryA := releaseFixture(t)
	ctx := context.Background()
	a := claimGoRaceJob(t, s, task, h, d, entryA, goRaceMatrix(goRace(raceFlags, "./cmd/tt")))
	if _, err := s.ReleaseAction(ctx, task.ID, integratedImport(a, h, d, "refused-import", goRaceMatrix(goRace(raceFlags, widerPackages[1:]...)))); !errors.Is(err, api.ErrConflict) {
		t.Fatal("import", err)
	}
	if saved, err := releaseLoad(ctx, s.db, task.ID, a.ID); err != nil || saved.State != "claimed" || saved.Generation != a.Generation {
		t.Fatal("refused import changed the job", saved.State, err)
	}
	b, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "fix-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: releaseEntry(t, s, task, "fix for the refusal", "fix")})
	if err != nil {
		t.Fatal(err)
	}
	claimB := api.ReleaseRequest{RequestID: "fix-claim-fenced", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: b.ID, ExpectedGeneration: b.Generation}
	if _, err = s.ReleaseAction(ctx, task.ID, claimB); !errors.Is(err, api.ErrConflict) {
		t.Fatal("claimed behind a held fence", err)
	}
	evidence := setAsideEvidence(t, s, task, h, a)
	req := api.ReleaseRequest{RequestID: "set-aside", Operation: "set-aside", AgentID: h.ID, RunID: h.RunID, JobID: a.ID, ExpectedGeneration: a.Generation, Reconciliation: &evidence}
	aside, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if aside.State != "verified" || aside.Generation != a.Generation+1 || aside.AgentID != "" || aside.RunID != "" || aside.IntegratedCommit != "" || aside.IntegratedPlan != nil || aside.IntegratedCoverage != nil || aside.IntegratedVerification != nil || aside.InputsCommit != "" || aside.InputsDigest != "" {
		t.Fatalf("set aside %+v", aside)
	}
	if n := len(aside.Reconciliations); n != 1 || aside.Reconciliations[0].Disposition != "set_aside" || aside.Reconciliations[0].StopReason != evidence.StopReason || aside.Reconciliations[0].RunID != d.RunID {
		t.Fatalf("history %+v", aside.Reconciliations)
	}
	jobs, err := s.Releases(ctx, task.ID)
	if err != nil || len(jobs) != 2 || jobs[0].ID != b.ID || jobs[1].ID != a.ID {
		t.Fatal("queue order", jobs, err)
	}
	retry, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil || retry.Generation != aside.Generation || retry.State != "verified" {
		t.Fatal("replay", retry.Generation, err)
	}
	changed := evidence
	changed.StopReason = "A different account of the stop"
	mutated := req
	mutated.Reconciliation = &changed
	if _, err = s.ReleaseAction(ctx, task.ID, mutated); !errors.Is(err, api.ErrConflict) {
		t.Fatal("changed record replayed", err)
	}
	claimB.RequestID = "fix-claim"
	b, err = s.ReleaseAction(ctx, task.ID, claimB)
	if err != nil || b.State != "claimed" {
		t.Fatal("fix did not claim the fence", err)
	}

	// Re-planned on the new tip: claimed again only after the fix clears.
	claimA := api.ReleaseRequest{RequestID: "reclaim-fenced", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: a.ID, ExpectedGeneration: aside.Generation}
	if _, err = s.ReleaseAction(ctx, task.ID, claimA); !errors.Is(err, api.ErrConflict) {
		t.Fatal("set-aside job took the fence from the fix", err)
	}
	b, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "fix-merged", Operation: "merged", AgentID: d.ID, RunID: d.RunID, JobID: b.ID, ExpectedGeneration: b.Generation, IntegratedCommit: b.Commit})
	if err != nil {
		t.Fatal(err)
	}
	finish := api.ReleaseRequest{RequestID: "fix-finish", Operation: "finish", AgentID: d.ID, RunID: d.RunID, JobID: b.ID, ExpectedGeneration: b.Generation}
	finish.Receipt = &api.ReleaseReceipt{Version: 1, JobID: b.ID, Commit: b.Commit, VerificationDigest: b.VerificationDigest, Outcome: "released", Targets: []api.ReleaseTargetReceipt{{Target: "tailos", Release: "fixture", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "released"}}}
	if b, err = s.ReleaseAction(ctx, task.ID, finish); err != nil || b.State != "released" {
		t.Fatal(b.State, err)
	}
	claimA.RequestID = "reclaim"
	again, err := s.ReleaseAction(ctx, task.ID, claimA)
	if err != nil || again.State != "claimed" || again.RunID != d.RunID || again.IntegratedCommit != "" || again.IntegratedPlan != nil || again.InputsDigest != "" || again.Commit != a.Commit || again.VerificationDigest != a.VerificationDigest {
		t.Fatalf("reclaim %+v %v", again, err)
	}
}

// Only a claimed job with no effects moves aside, and only on an exact typed
// record from the handler while a later job waits; anything else keeps the
// fence and the job exactly as it was.
func TestReleaseSetAsideKeepsFenceForJobsWithEffects(t *testing.T) {
	ctx := context.Background()
	type fixture struct {
		s    *Store
		task api.Task
		h, d api.Agent
		a, b api.ReleaseJob
	}
	// setup claims job A with job B queued behind it; alone leaves B out.
	setup := func(t *testing.T, alone bool) fixture {
		s, task, h, d, entry := releaseFixture(t)
		a, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "a-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
		if err != nil {
			t.Fatal(err)
		}
		a, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "a-claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: a.ID, ExpectedGeneration: a.Generation})
		if err != nil {
			t.Fatal(err)
		}
		f := fixture{s: s, task: task, h: h, d: d, a: a}
		if !alone {
			if f.b, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "b-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: releaseEntry(t, s, task, "queued behind", "b")}); err != nil {
				t.Fatal(err)
			}
		}
		return f
	}
	deployer := func(t *testing.T, f fixture, op string, j api.ReleaseJob, change func(*api.ReleaseRequest)) api.ReleaseJob {
		t.Helper()
		req := api.ReleaseRequest{RequestID: "a-" + op, Operation: op, AgentID: f.d.ID, RunID: f.d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}
		if change != nil {
			change(&req)
		}
		out, err := f.s.ReleaseAction(ctx, f.task.ID, req)
		if err != nil {
			t.Fatal(op, err)
		}
		return out
	}
	// refused asserts a conflict that leaves the fence and the job unchanged.
	refused := func(t *testing.T, f fixture, j api.ReleaseJob, change func(*api.ReleaseRequest)) {
		t.Helper()
		evidence := setAsideEvidence(t, f.s, f.task, f.h, j)
		req := api.ReleaseRequest{RequestID: "set-aside", Operation: "set-aside", AgentID: f.h.ID, RunID: f.h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Reconciliation: &evidence}
		if change != nil {
			change(&req)
		}
		var fenced int
		f.s.db.QueryRow(`SELECT count(*) FROM release_jobs WHERE task_id=? AND state IN ('claimed','merged','blocked')`, f.task.ID).Scan(&fenced)
		if _, err := f.s.ReleaseAction(ctx, f.task.ID, req); !errors.Is(err, api.ErrConflict) {
			t.Fatal("set aside", err)
		}
		saved, err := releaseLoad(ctx, f.s.db, f.task.ID, j.ID)
		var after int
		f.s.db.QueryRow(`SELECT count(*) FROM release_jobs WHERE task_id=? AND state IN ('claimed','merged','blocked')`, f.task.ID).Scan(&after)
		if err != nil || saved.Generation != j.Generation || saved.State != j.State || len(saved.Reconciliations) != 0 || after != fenced {
			t.Fatal("refusal changed the ledger", saved.State, saved.Generation, after, fenced, err)
		}
	}
	t.Run("merged", func(t *testing.T) {
		f := setup(t, false)
		refused(t, f, deployer(t, f, "merged", f.a, func(r *api.ReleaseRequest) { r.IntegratedCommit = f.a.Commit }), nil)
	})
	t.Run("blocked", func(t *testing.T) {
		f := setup(t, false)
		refused(t, f, deployer(t, f, "block", f.a, nil), nil)
	})
	t.Run("inputs bound", func(t *testing.T) {
		f := setup(t, false)
		bound, err := f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: "a-inputs", Operation: "inputs", AgentID: f.h.ID, RunID: f.h.RunID, JobID: f.a.ID, ExpectedGeneration: f.a.Generation, IntegratedCommit: f.a.Commit, InputsDigest: strings.Repeat("e", 64)})
		if err != nil {
			t.Fatal(err)
		}
		refused(t, f, bound, nil)
	})
	t.Run("verified", func(t *testing.T) {
		f := setup(t, false)
		refused(t, f, f.b, nil)
	})
	t.Run("nothing waiting", func(t *testing.T) {
		f := setup(t, true)
		refused(t, f, f.a, nil)
	})
	for name, change := range map[string]func(*api.ReleaseRequest){
		"deployer caller":              func(r *api.ReleaseRequest) { r.AgentID, r.RunID = r.Reconciliation.AgentID, r.Reconciliation.RunID },
		"wrong generation":             func(r *api.ReleaseRequest) { r.ExpectedGeneration++ },
		"other agent":                  func(r *api.ReleaseRequest) { r.Reconciliation.AgentID = api.NewID("agt") },
		"other run":                    func(r *api.ReleaseRequest) { r.Reconciliation.RunID = api.NewID("run") },
		"other pause generation":       func(r *api.ReleaseRequest) { r.Reconciliation.PauseGeneration++ },
		"requeue disposition":          func(r *api.ReleaseRequest) { r.Reconciliation.Disposition = "requeue" },
		"missing incident bug":         func(r *api.ReleaseRequest) { r.Reconciliation.IncidentBugID = api.NewID("wi") },
		"lock digest":                  func(r *api.ReleaseRequest) { r.Reconciliation.LockDigest = strings.Repeat("f", 64) },
		"unconfirmed prevention order": func(r *api.ReleaseRequest) { r.Reconciliation.PreventionOrderMessage++ },
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, false)
			refused(t, f, f.a, change)
		})
	}
}
