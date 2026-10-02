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
	if err != nil || final.SettledAt == "" {
		t.Fatal(final.SettledAt, err)
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

var (
	matrixA = strings.Repeat("a", 64)
	matrixB = strings.Repeat("b", 64)
	// A changed matrix rebuilds go-race: other flags and another time limit.
	changedRaceFlags = []string{"-race", "-timeout=40m", "-shuffle=on"}
)

func changedRace(packages ...string) api.VerificationCheck {
	race := goRace(changedRaceFlags, packages...)
	race.Environment["VERIFICATION_TIMEOUT_MS"] = "2700000"
	return race
}

// matrixImport is integratedImport for an integrated commit whose matrix has
// this digest, citing the approval message at seq.
func matrixImport(j api.ReleaseJob, h, d api.Agent, requestID, digest string, seq int64, checks []api.VerificationCheck) api.ReleaseRequest {
	req := integratedImport(j, h, d, requestID, checks)
	p := *req.Plan
	p.MatrixDigest = digest
	p.ApprovedMatrixDigest = digest
	p.MatrixApprovalMessageSeq = seq
	r := passingVerification(p)
	req.Plan = &p
	req.Verification = &r
	return req
}
func ownerApproval(t *testing.T, s *Store, task api.Task, text string) int64 {
	t.Helper()
	m, err := s.PostMessage(context.Background(), task.ID, api.PostMessageRequest{Text: text, RequestID: api.NewID("req")}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return m.Seq
}

// tasks-hub gained a matrix change after the job was approved, so the
// integrated commit has another matrix digest. The import binds that digest
// only under an owner approval of exactly it, records the change beside the
// unchanged approved plan, and otherwise names both digests.
func TestReleaseImportBindsOwnerApprovedMatrixChange(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	approved := goRaceMatrix(goRace(raceFlags, "./cmd/tt"))
	j := claimGoRaceJob(t, s, task, h, d, entry, approved)
	if j.Plan.MatrixDigest != matrixA {
		t.Fatal("fixture digest", j.Plan.MatrixDigest)
	}
	integrated := goRaceMatrix(changedRace(widerPackages...))
	refuse := func(name, want string, req api.ReleaseRequest) {
		t.Helper()
		_, err := s.ReleaseAction(ctx, task.ID, req)
		if !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("%s: %v", name, err)
		}
		saved, err := releaseLoad(ctx, s.db, task.ID, j.ID)
		if err != nil || saved.Generation != j.Generation || saved.IntegratedPlan != nil || saved.IntegratedMatrix != nil || saved.IntegratedCoverage != nil {
			t.Fatal(name, "changed the job", saved.Generation, saved.IntegratedMatrix, err)
		}
	}
	const uncovered = "release: matrix digest changed aaaaaaaa -> bbbbbbbb; no approval"
	old := ownerApproval(t, s, task, "verification-matrix-approval:"+matrixA)
	agent, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "verification-matrix-approval:" + matrixB, RequestID: "agent-token", AgentID: h.ID}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	wordy := ownerApproval(t, s, task, "I approve verification-matrix-approval:"+matrixB)
	refuse("no-seq", uncovered, matrixImport(j, h, d, "matrix-no-seq", matrixB, 0, integrated))
	refuse("old-approval", uncovered, matrixImport(j, h, d, "matrix-old", matrixB, old, integrated))
	refuse("agent-token", uncovered, matrixImport(j, h, d, "matrix-agent", matrixB, agent.Seq, integrated))
	refuse("extra-words", uncovered, matrixImport(j, h, d, "matrix-wordy", matrixB, wordy, integrated))
	refuse("missing-seq", uncovered, matrixImport(j, h, d, "matrix-missing", matrixB, wordy+1000, integrated))
	covering := ownerApproval(t, s, task, " verification-matrix-approval:"+matrixB+"\n")
	// The approved digest and the plan's own digest must still agree.
	split := matrixImport(j, h, d, "matrix-split", matrixB, covering, integrated)
	split.Plan.ApprovedMatrixDigest = matrixA
	refuse("split-digest", "integrated plan binding mismatch", split)
	// An approved matrix change still may not drop a check or narrow go-race.
	refuse("dropped-check", "integrated matrix omitted approved check "+approved[7].ID, matrixImport(j, h, d, "matrix-dropped", matrixB, covering, slices.Delete(slices.Clone(integrated), 7, 8)))
	refuse("narrower-race", "integrated matrix omitted approved check go-race", matrixImport(j, h, d, "matrix-narrow", matrixB, covering, goRaceMatrix(changedRace(widerPackages[1:]...))))
	// An unchanged digest keeps the job's own approval, whatever else exists.
	refuse("same-digest-other-seq", "integrated plan binding mismatch", matrixImport(j, h, d, "matrix-same", matrixA, old, approved))

	// The list gives the claimed job the owner approvals as a hint: newest
	// message per digest, nothing agent-authored or inexact, nothing saved.
	jobs, err := s.Releases(ctx, task.ID)
	wantApprovals := []api.ReleaseMatrixApproval{{Digest: matrixA, MessageSeq: old}, {Digest: matrixB, MessageSeq: covering}}
	if err != nil || len(jobs) != 1 || !slices.Equal(jobs[0].MatrixApprovals, wantApprovals) {
		t.Fatalf("approvals %+v %v", jobs, err)
	}

	req := matrixImport(j, h, d, "matrix-import", matrixB, covering, integrated)
	imported, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	wantMatrix := api.ReleaseMatrixChange{ApprovedDigest: matrixA, IntegratedDigest: matrixB, ApprovalMessageSeq: covering}
	wantCoverage := []api.ReleaseCheckCoverage{{CheckID: "go-race", ApprovedDigest: verificationDigest(approved[71]), IntegratedDigest: verificationDigest(integrated[71]), Relation: "matrix_changed"}}
	if imported.IntegratedMatrix == nil || *imported.IntegratedMatrix != wantMatrix || !slices.Equal(imported.IntegratedCoverage, wantCoverage) || imported.Generation != j.Generation+1 {
		t.Fatalf("matrix %+v coverage %+v", imported.IntegratedMatrix, imported.IntegratedCoverage)
	}
	if imported.Plan.MatrixDigest != matrixA || imported.Plan.MatrixApprovalMessageSeq != j.Plan.MatrixApprovalMessageSeq || verificationDigest(imported.Plan) != verificationDigest(j.Plan) || imported.IntegratedPlan.MatrixDigest != matrixB {
		t.Fatal("approved plan changed", imported.Plan.MatrixDigest)
	}
	if imported.MatrixApprovals != nil {
		t.Fatal("action returned the derived approvals")
	}
	var raw string
	if err = s.db.QueryRow(`SELECT record_json FROM release_jobs WHERE task_id=? AND id=?`, task.ID, j.ID).Scan(&raw); err != nil || strings.Contains(raw, "matrixApprovals") || !strings.Contains(raw, `"integratedMatrix"`) {
		t.Fatal("saved record", err, strings.Contains(raw, "matrixApprovals"))
	}
	merged, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "matrix-merged", Operation: "merged", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: imported.Generation, IntegratedCommit: candidateA})
	if err != nil || merged.State != "merged" || merged.IntegratedMatrix == nil || *merged.IntegratedMatrix != wantMatrix {
		t.Fatal(merged.State, err)
	}
	// Only a claimed job carries the hint.
	if jobs, err = s.Releases(ctx, task.ID); err != nil || len(jobs) != 1 || jobs[0].MatrixApprovals != nil {
		t.Fatalf("merged job approvals %+v %v", jobs, err)
	}
}

// A requeued or set-aside job starts its integrated verification from
// scratch, the recorded matrix change included.
func TestReleaseRequeueAndSetAsideClearIntegratedMatrix(t *testing.T) {
	for _, disposition := range []string{"requeue", "set_aside"} {
		t.Run(disposition, func(t *testing.T) {
			s, task, h, d, entry := releaseFixture(t)
			ctx := context.Background()
			j := claimGoRaceJob(t, s, task, h, d, entry, goRaceMatrix(goRace(raceFlags, "./cmd/tt")))
			seq := ownerApproval(t, s, task, "verification-matrix-approval:"+matrixB)
			imported, err := s.ReleaseAction(ctx, task.ID, matrixImport(j, h, d, "matrix-import", matrixB, seq, goRaceMatrix(changedRace("./cmd/tt"))))
			if err != nil || imported.IntegratedMatrix == nil || len(imported.IntegratedCoverage) != 1 {
				t.Fatal(imported.IntegratedMatrix, imported.IntegratedCoverage, err)
			}
			req := api.ReleaseRequest{RequestID: "matrix-" + disposition, Operation: "reconcile", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: imported.Generation}
			if disposition == "requeue" {
				s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, d.ID)
				evidence := recoveryEvidence(t, s, task, h, imported)
				req.Reconciliation = &evidence
			} else {
				if _, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "later-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: releaseEntry(t, s, task, "later job", "later")}); err != nil {
					t.Fatal(err)
				}
				evidence := setAsideEvidence(t, s, task, h, imported)
				req.Operation = "set-aside"
				req.Reconciliation = &evidence
			}
			cleared, err := s.ReleaseAction(ctx, task.ID, req)
			if err != nil || cleared.State != "verified" || cleared.IntegratedMatrix != nil || cleared.IntegratedCoverage != nil || cleared.IntegratedPlan != nil || cleared.Plan.MatrixDigest != matrixA {
				t.Fatal(cleared.State, cleared.IntegratedMatrix, cleared.IntegratedCoverage, err)
			}
		})
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
	hand := recordHandRelease(t, s, task, "hand", candidateB, []string{candidateB})
	record := &api.ReleaseSupersession{ReleasedCommit: candidateB, Release: hand.Release, HandReleaseID: hand.ID}
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
	record = &api.ReleaseSupersession{ReleasedCommit: candidateB, HandReleaseID: recordHandRelease(t, s2, task2, "hand", candidateB, []string{candidateB}).ID}
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

// releaseIntervention records an owner intervention of the given kind on a
// fresh item in the task and returns its message sequence.
func releaseIntervention(t *testing.T, s *Store, task api.Task, key, kind string) int64 {
	t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "hand release " + key, RequestID: "item-" + key}, by)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateIntervention(ctx, task.ID, api.CreateInterventionRequest{Kind: kind, ItemID: item.ID, Text: "released by hand", RequestID: "intervention-" + key}, by)
	if err != nil {
		t.Fatal(err)
	}
	return m.Seq
}

// secondTask is another project in the same store, so its records share the
// store's sequence and ID space with the fixture's.
func secondTask(t *testing.T, s *Store) api.Task {
	t.Helper()
	task, err := s.CreateTask(context.Background(), api.CreateTaskRequest{Name: "other project"}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// recordHandRelease records an owner hand release of the hub target that
// cites a fresh release intervention.
func recordHandRelease(t *testing.T, s *Store, task api.Task, key, released string, commits []string) api.HandRelease {
	t.Helper()
	seq := releaseIntervention(t, s, task, key, "release")
	h, err := s.RecordHandRelease(context.Background(), task.ID, api.ReleaseRequest{RequestID: "hand-release-" + key, HandRelease: &api.HandRelease{InterventionSeq: seq, ReleasedCommit: released, Release: "20260930-" + key, Targets: []string{"hub"}, Commits: commits}}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// a8: the hand release record is owner-only, cites a release intervention in
// its own project, validates its fields, replays exactly and is immutable.
func TestHandReleaseRecord(t *testing.T) {
	s, task, h, _, _ := releaseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	seq := releaseIntervention(t, s, task, "ok", "release")
	nudge := releaseIntervention(t, s, task, "nudge", "nudge")
	foreign := releaseIntervention(t, s, secondTask(t, s), "foreign", "release")
	valid := func() *api.HandRelease {
		return &api.HandRelease{InterventionSeq: seq, ReleasedCommit: candidateB, Release: "20260930-hand", Targets: []string{"tailos", "hub"}, Commits: []string{candidateA, candidateB}}
	}
	record := func(key string, mutate func(*api.ReleaseRequest)) (api.HandRelease, error) {
		req := api.ReleaseRequest{RequestID: key, HandRelease: valid()}
		if mutate != nil {
			mutate(&req)
		}
		return s.RecordHandRelease(ctx, task.ID, req, by)
	}
	refusals := map[string]struct {
		mutate func(*api.ReleaseRequest)
		want   error
	}{
		"agent":             {func(r *api.ReleaseRequest) { r.AgentID, r.RunID = h.ID, h.RunID }, api.ErrConflict},
		"run only":          {func(r *api.ReleaseRequest) { r.RunID = h.RunID }, api.ErrConflict},
		"no record":         {func(r *api.ReleaseRequest) { r.HandRelease = nil }, api.ErrInvalid},
		"no intervention":   {func(r *api.ReleaseRequest) { r.HandRelease.InterventionSeq = 999999 }, api.ErrConflict},
		"other kind":        {func(r *api.ReleaseRequest) { r.HandRelease.InterventionSeq = nudge }, api.ErrConflict},
		"other task":        {func(r *api.ReleaseRequest) { r.HandRelease.InterventionSeq = foreign }, api.ErrConflict},
		"short commit":      {func(r *api.ReleaseRequest) { r.HandRelease.ReleasedCommit = candidateB[:12] }, api.ErrInvalid},
		"upper commit":      {func(r *api.ReleaseRequest) { r.HandRelease.ReleasedCommit = strings.ToUpper(candidateB) }, api.ErrInvalid},
		"covered commit":    {func(r *api.ReleaseRequest) { r.HandRelease.Commits = []string{"--output=x"} }, api.ErrInvalid},
		"duplicate commit":  {func(r *api.ReleaseRequest) { r.HandRelease.Commits = []string{candidateA, candidateA} }, api.ErrInvalid},
		"no commits":        {func(r *api.ReleaseRequest) { r.HandRelease.Commits = nil }, api.ErrInvalid},
		"too many commits":  {func(r *api.ReleaseRequest) { r.HandRelease.Commits = make([]string, 65) }, api.ErrInvalid},
		"unknown target":    {func(r *api.ReleaseRequest) { r.HandRelease.Targets = []string{"nas"} }, api.ErrInvalid},
		"duplicate target":  {func(r *api.ReleaseRequest) { r.HandRelease.Targets = []string{"hub", "hub"} }, api.ErrInvalid},
		"no targets":        {func(r *api.ReleaseRequest) { r.HandRelease.Targets = nil }, api.ErrInvalid},
		"no release name":   {func(r *api.ReleaseRequest) { r.HandRelease.Release = "" }, api.ErrInvalid},
		"newline in name":   {func(r *api.ReleaseRequest) { r.HandRelease.Release = "a\nb" }, api.ErrInvalid},
		"no intervention 0": {func(r *api.ReleaseRequest) { r.HandRelease.InterventionSeq = 0 }, api.ErrInvalid},
	}
	for name, c := range refusals {
		if _, err := record("refused-"+strings.ReplaceAll(name, " ", "-"), c.mutate); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	got, err := record("record", nil)
	if err != nil || !strings.HasPrefix(got.ID, "hrl_") || got.TaskID != task.ID || got.CreatedAt == "" || got.RecordedBy == nil || *got.RecordedBy != by || strings.Join(got.Targets, ",") != "hub,tailos" {
		t.Fatal(got, err)
	}
	if again, err := record("record", nil); err != nil || again.ID != got.ID || again.CreatedAt != got.CreatedAt {
		t.Fatal("replay", again, err)
	}
	if _, err = record("record", func(r *api.ReleaseRequest) { r.HandRelease.Release = "changed" }); !errors.Is(err, api.ErrConflict) {
		t.Fatal("changed replay", err)
	}
	if _, err = record("second", nil); !errors.Is(err, api.ErrConflict) {
		t.Fatal("second record for one intervention", err)
	}
	list, err := s.HandReleases(ctx, task.ID)
	if err != nil || len(list) != 1 || list[0].ID != got.ID {
		t.Fatal(list, err)
	}
	if _, err = s.db.Exec(`UPDATE release_hand_releases SET released_commit=? WHERE id=?`, candidateA, got.ID); err == nil || !strings.Contains(err.Error(), "immutable hand release") {
		t.Fatal("update", err)
	}
	if _, err = s.db.Exec(`DELETE FROM release_hand_releases WHERE id=?`, got.ID); err == nil || !strings.Contains(err.Error(), "immutable hand release") {
		t.Fatal("delete", err)
	}
}

// a1 (s1): supersede must cite a recorded hand release in the project whose
// released commit matches and whose covered commits include the job's.
func TestSupersedeRequiresRecordedHandRelease(t *testing.T) {
	s, task, h, _, entry := releaseFixture(t)
	ctx := context.Background()
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	released := strings.Repeat("c", 40)
	covering := recordHandRelease(t, s, task, "covering", released, []string{candidateA, candidateB})
	uncovering := recordHandRelease(t, s, task, "uncovering", released, []string{candidateA})
	foreign := recordHandRelease(t, s, secondTask(t, s), "foreign", released, []string{candidateB})
	supersede := func(key string, sup *api.ReleaseSupersession) (api.ReleaseJob, error) {
		return s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: key, Operation: "supersede", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Supersession: sup})
	}
	refusals := map[string]struct {
		sup  *api.ReleaseSupersession
		want error
	}{
		"no record id":        {&api.ReleaseSupersession{ReleasedCommit: released}, api.ErrInvalid},
		"unknown record":      {&api.ReleaseSupersession{ReleasedCommit: released, HandReleaseID: "hrl_0123456789abcdef"}, api.ErrConflict},
		"other task record":   {&api.ReleaseSupersession{ReleasedCommit: released, HandReleaseID: foreign.ID}, api.ErrConflict},
		"commit differs":      {&api.ReleaseSupersession{ReleasedCommit: candidateB, HandReleaseID: covering.ID}, api.ErrConflict},
		"release differs":     {&api.ReleaseSupersession{ReleasedCommit: released, Release: "other", HandReleaseID: covering.ID}, api.ErrConflict},
		"does not cover":      {&api.ReleaseSupersession{ReleasedCommit: released, HandReleaseID: uncovering.ID}, api.ErrConflict},
		"caller sets targets": {&api.ReleaseSupersession{ReleasedCommit: released, HandReleaseID: covering.ID, Targets: []string{"mini"}}, api.ErrInvalid},
	}
	for name, c := range refusals {
		if _, err = supersede("refused-"+strings.ReplaceAll(name, " ", "-"), c.sup); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err = supersede("uncovered-message", &api.ReleaseSupersession{ReleasedCommit: released, HandReleaseID: uncovering.ID}); err == nil || !strings.Contains(err.Error(), "does not cover") {
		t.Fatal(err)
	}
	done, err := supersede("supersede", &api.ReleaseSupersession{ReleasedCommit: released, HandReleaseID: covering.ID})
	if err != nil || done.State != "superseded" || done.Supersession.HandReleaseID != covering.ID || done.Supersession.Release != covering.Release || strings.Join(done.Supersession.Targets, ",") != "hub" || done.SettledAt != covering.CreatedAt {
		t.Fatal(done, err)
	}
}

// a10 (D1): a refused job without a receipt may be superseded by a covering
// record; a claimed, merged or blocked job may not.
func TestSupersedeRefusedJob(t *testing.T) {
	ctx := context.Background()
	for _, final := range []string{"refused", "claimed", "merged", "blocked"} {
		s, task, h, d, entry := releaseFixture(t)
		j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
		if err != nil {
			t.Fatal(err)
		}
		step := func(op string) {
			req := api.ReleaseRequest{RequestID: op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit}
			if j, err = s.ReleaseAction(ctx, task.ID, req); err != nil {
				t.Fatal(final, op, err)
			}
		}
		step("claim")
		switch final {
		case "refused":
			step("refuse")
		case "merged":
			step("merged")
		case "blocked":
			step("block")
		}
		hand := recordHandRelease(t, s, task, "hand", candidateB, []string{candidateB})
		done, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "supersede", Operation: "supersede", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Supersession: &api.ReleaseSupersession{ReleasedCommit: candidateB, HandReleaseID: hand.ID}})
		if final == "refused" {
			if err != nil || done.State != "superseded" || done.Supersession.HandReleaseID != hand.ID {
				t.Fatal(final, done, err)
			}
		} else if !errors.Is(err, api.ErrConflict) {
			t.Fatal(final, "superseded", err)
		}
	}
}

// f2: a handler reconcile may refuse a published job; with no receipt it is
// supersedable once a hand release covers it.
func TestSupersedePublishedReconcileRefusedJob(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"claim", "merged", "block"} {
		if j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit}); err != nil {
			t.Fatal(op, err)
		}
	}
	evidence := recoveryEvidence(t, s, task, h, j)
	evidence.Disposition, evidence.NoPublication, evidence.JournalState = "refuse", false, "restored"
	if _, err = s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, d.ID); err != nil {
		t.Fatal(err)
	}
	j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "reconcile", Operation: "reconcile", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Reconciliation: &evidence})
	if err != nil || j.State != "refused" || !j.Published || j.Receipt != nil {
		t.Fatal(j, err)
	}
	hand := recordHandRelease(t, s, task, "hand", candidateB, []string{candidateB})
	done, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "supersede", Operation: "supersede", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Supersession: &api.ReleaseSupersession{ReleasedCommit: candidateB, HandReleaseID: hand.ID}})
	if err != nil || done.State != "superseded" || !done.Published || done.Supersession.HandReleaseID != hand.ID || done.SettledAt != hand.CreatedAt {
		t.Fatal(done, err)
	}
}

// a10/a11 (s1-s3): a job the handler set aside is verified and unclaimed again
// with only its set_aside reconciliation, which proves no release effects, so
// a covering hand release supersedes it and the reconciliation stays in its
// history. Claimed again, requeued after a claim, or published, it is refused
// with a reason naming the job.
func TestSupersedeSetAsideJob(t *testing.T) {
	ctx := context.Background()
	type fixture struct {
		s     *Store
		task  api.Task
		h, d  api.Agent
		aside api.ReleaseJob
	}
	setup := func(t *testing.T) fixture {
		s, task, h, d, entryA := releaseFixture(t)
		a := claimGoRaceJob(t, s, task, h, d, entryA, goRaceMatrix(goRace(raceFlags, "./cmd/tt")))
		if _, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "fix-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: releaseEntry(t, s, task, "fix for the refusal", "fix")}); err != nil {
			t.Fatal(err)
		}
		evidence := setAsideEvidence(t, s, task, h, a)
		aside, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "set-aside", Operation: "set-aside", AgentID: h.ID, RunID: h.RunID, JobID: a.ID, ExpectedGeneration: a.Generation, Reconciliation: &evidence})
		if err != nil || aside.State != "verified" || aside.AgentID != "" || len(aside.Reconciliations) != 1 {
			t.Fatalf("set aside %+v %v", aside, err)
		}
		return fixture{s, task, h, d, aside}
	}
	supersede := func(t *testing.T, f fixture, j api.ReleaseJob) (api.ReleaseJob, error) {
		t.Helper()
		hand := recordHandRelease(t, f.s, f.task, "hand", j.Commit, []string{j.Commit})
		return f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: "supersede", Operation: "supersede", AgentID: f.h.ID, RunID: f.h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Supersession: &api.ReleaseSupersession{ReleasedCommit: j.Commit, HandReleaseID: hand.ID}})
	}
	// refused asserts a conflict naming the job and the reason that leaves
	// the job as it was.
	refused := func(t *testing.T, f fixture, j api.ReleaseJob, want string) {
		t.Helper()
		_, err := supersede(t, f, j)
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), j.ID) || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q naming %s, got %v", want, j.ID, err)
		}
		if saved, err := releaseLoad(ctx, f.s.db, f.task.ID, j.ID); err != nil || saved.State != j.State || saved.Generation != j.Generation || saved.Supersession != nil {
			t.Fatal("refused supersede changed the job", saved.State, err)
		}
	}
	t.Run("set aside only", func(t *testing.T) {
		f := setup(t)
		uncovering := recordHandRelease(t, f.s, f.task, "uncovering", candidateC, []string{candidateC})
		if _, err := f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: "uncovered", Operation: "supersede", AgentID: f.h.ID, RunID: f.h.RunID, JobID: f.aside.ID, ExpectedGeneration: f.aside.Generation, Supersession: &api.ReleaseSupersession{ReleasedCommit: candidateC, HandReleaseID: uncovering.ID}}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "does not cover") {
			t.Fatal("uncovering record superseded a set-aside job", err)
		}
		done, err := supersede(t, f, f.aside)
		if err != nil || done.State != "superseded" || done.Supersession == nil || strings.Join(done.Supersession.Targets, ",") != "hub" || done.SettledAt == "" {
			t.Fatalf("%+v %v", done, err)
		}
		if len(done.Reconciliations) != 1 || done.Reconciliations[0] != f.aside.Reconciliations[0] {
			t.Fatalf("set-aside history changed %+v", done.Reconciliations)
		}
	})
	t.Run("claimed again", func(t *testing.T) {
		f := setup(t)
		again, err := f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: "reclaim", Operation: "claim", AgentID: f.d.ID, RunID: f.d.RunID, JobID: f.aside.ID, ExpectedGeneration: f.aside.Generation})
		if err != nil || again.State != "claimed" {
			t.Fatal(again.State, err)
		}
		refused(t, f, again, "is claimed; reconcile it first")
	})
	t.Run("requeued after a claim", func(t *testing.T) {
		f := setup(t)
		again, err := f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: "reclaim", Operation: "claim", AgentID: f.d.ID, RunID: f.d.RunID, JobID: f.aside.ID, ExpectedGeneration: f.aside.Generation})
		if err != nil {
			t.Fatal(err)
		}
		evidence := recoveryEvidence(t, f.s, f.task, f.h, again)
		if _, err = f.s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, f.d.ID); err != nil {
			t.Fatal(err)
		}
		requeued, err := f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: "requeue", Operation: "reconcile", AgentID: f.h.ID, RunID: f.h.RunID, JobID: again.ID, ExpectedGeneration: again.Generation, Reconciliation: &evidence})
		if err != nil || requeued.State != "verified" || len(requeued.Reconciliations) != 2 {
			t.Fatalf("%+v %v", requeued, err)
		}
		refused(t, f, requeued, "has deployer history (requeue reconciliation)")
	})
	t.Run("published", func(t *testing.T) {
		f := setup(t)
		j := f.aside
		for _, op := range []string{"claim", "merged"} {
			var err error
			if j, err = f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: "again-" + op, Operation: op, AgentID: f.d.ID, RunID: f.d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit}); err != nil {
				t.Fatal(op, err)
			}
		}
		refused(t, f, j, "is merged; reconcile it first")
	})
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

// refusedJob enqueues the entry's first job and has the deployer claim and
// refuse it before publication: refused, no receipt, never published.
func refusedJob(t *testing.T, s *Store, task api.Task, h, d api.Agent, entry string) api.ReleaseJob {
	t.Helper()
	ctx := context.Background()
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "first-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"claim", "refuse"} {
		if j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "first-" + op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}); err != nil {
			t.Fatal(op, err)
		}
	}
	return j
}

// failedReceiptJob publishes the entry's first job, finishes it blocked with
// the given target receipts, and has the handler refuse it by reconcile with
// the given journal state: refused, published, with a receipt. The deployer
// run is exited, as reconcile requires.
func failedReceiptJob(t *testing.T, s *Store, task api.Task, h, d api.Agent, entry, journalState string, targets []api.ReleaseTargetReceipt) api.ReleaseJob {
	t.Helper()
	ctx := context.Background()
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "first-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"claim", "merged", "finish"} {
		req := api.ReleaseRequest{RequestID: "first-" + op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit}
		if op == "finish" {
			req.IntegratedCommit = ""
			req.Receipt = &api.ReleaseReceipt{Version: 1, JobID: j.ID, Commit: j.Commit, VerificationDigest: j.VerificationDigest, Outcome: "blocked", Targets: targets}
		}
		if j, err = s.ReleaseAction(ctx, task.ID, req); err != nil {
			t.Fatal(op, err)
		}
	}
	evidence := recoveryEvidence(t, s, task, h, j)
	evidence.Disposition, evidence.NoPublication, evidence.JournalState = "refuse", false, journalState
	if _, err = s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, d.ID); err != nil {
		t.Fatal(err)
	}
	j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "first-reconcile", Operation: "reconcile", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Reconciliation: &evidence})
	if err != nil || j.State != "refused" || j.Receipt == nil || !j.Published {
		t.Fatalf("%+v %v", j, err)
	}
	return j
}

// restoredTargets is a blocked receipt whose two targets failed and were
// rolled back; unrestoredTargets leaves tailos released.
func restoredTargets() []api.ReleaseTargetReceipt {
	return []api.ReleaseTargetReceipt{
		{Target: "tailos", Release: "fixture-new", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "failed", Rollback: "restored"},
		{Target: "mini", Release: "fixture-new", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "rolled_back", Rollback: "restored"},
	}
}
func unrestoredTargets() []api.ReleaseTargetReceipt {
	return []api.ReleaseTargetReceipt{
		{Target: "mini", Release: "fixture-new", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "failed"},
		{Target: "tailos", Release: "fixture-new", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "released"},
	}
}

// releaseRow is a job's stored row exactly as saved, with its queue position.
func releaseRow(t *testing.T, s *Store, task api.Task, id string) string {
	t.Helper()
	var rowid, generation int64
	var entry, state, raw string
	if err := s.db.QueryRow(`SELECT rowid,entry_id,state,generation,record_json FROM release_jobs WHERE task_id=? AND id=?`, task.ID, id).Scan(&rowid, &entry, &state, &generation, &raw); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d|%s|%s|%d|%s", rowid, entry, state, generation, raw)
}

func retryRequest(key string, h api.Agent, entry string, j api.ReleaseJob, restored ...api.ReleaseRestoredTarget) api.ReleaseRequest {
	return api.ReleaseRequest{RequestID: key, Operation: "retry", AgentID: h.ID, RunID: h.RunID, EntryID: entry, JobID: j.ID, ExpectedGeneration: j.Generation, Retry: &api.ReleaseRetry{Reason: "Environment fixed; nothing of the first attempt is live", Restored: restored}}
}

// a1/a2 (r1, r3, r4, j1): the handler retries a job refused before
// publication. The entry gets a new verified job linked to the refused one,
// the refused job's row is byte-identical, both are listed in order, the
// entry shows the new job and a deployer can claim it. A second refusal can
// be retried again, keeping every prior job.
func TestReleaseRetryCreatesNewJobForRefusedEntry(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	first := refusedJob(t, s, task, h, d, entry)
	before := releaseRow(t, s, task, first.ID)
	var receipts int
	if err := s.db.QueryRow(`SELECT count(*) FROM release_action_receipts WHERE task_id=?`, task.ID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	second, err := s.ReleaseAction(ctx, task.ID, retryRequest("retry", h, entry, first))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || !strings.HasPrefix(second.ID, "rel_") || second.State != "verified" || second.Generation != 1 || second.EntryID != entry || second.ItemID != first.ItemID || second.ItemRevision != first.ItemRevision || second.Commit != first.Commit || second.BaseCommit != first.BaseCommit || second.VerificationDigest != first.VerificationDigest || second.PauseGeneration != first.PauseGeneration || second.AgentID != "" || second.Receipt != nil || len(second.Reconciliations) != 0 {
		t.Fatalf("new job %+v", second)
	}
	link := second.RetryOf
	if link == nil || link.JobID != first.ID || link.Generation != first.Generation || link.State != "refused" || link.Reason == "" || link.AgentID != h.ID || link.RunID != h.RunID || link.Attempt != 2 || link.CreatedAt == "" || len(link.Restored) != 0 {
		t.Fatalf("link %+v", link)
	}
	if after := releaseRow(t, s, task, first.ID); after != before {
		t.Fatalf("refused job changed\nbefore %s\nafter  %s", before, after)
	}
	var after int
	if err = s.db.QueryRow(`SELECT count(*) FROM release_action_receipts WHERE task_id=?`, task.ID).Scan(&after); err != nil || after != receipts+1 {
		t.Fatal("request receipts", receipts, after, err)
	}
	jobs, err := s.Releases(ctx, task.ID)
	if err != nil || len(jobs) != 2 || jobs[0].ID != first.ID || jobs[0].State != "refused" || jobs[0].RetryOf != nil || jobs[1].ID != second.ID {
		t.Fatal("history", jobs, err)
	}
	queued, err := s.GetTeamQueueEntry(ctx, task.ID, entry)
	if err != nil || queued.Release == nil || queued.Release.ID != second.ID {
		t.Fatalf("entry release %+v %v", queued.Release, err)
	}
	claimed, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "second-claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: second.ID, ExpectedGeneration: second.Generation})
	if err != nil || claimed.State != "claimed" || claimed.RetryOf == nil || claimed.RetryOf.JobID != first.ID {
		t.Fatalf("claim %+v %v", claimed, err)
	}
	refusedAgain, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "second-refuse", Operation: "refuse", AgentID: d.ID, RunID: d.RunID, JobID: second.ID, ExpectedGeneration: claimed.Generation})
	if err != nil {
		t.Fatal(err)
	}
	secondRow := releaseRow(t, s, task, second.ID)
	third, err := s.ReleaseAction(ctx, task.ID, retryRequest("retry-again", h, entry, refusedAgain))
	if err != nil || third.RetryOf == nil || third.RetryOf.JobID != second.ID || third.RetryOf.Attempt != 3 {
		t.Fatalf("third %+v %v", third, err)
	}
	if releaseRow(t, s, task, first.ID) != before || releaseRow(t, s, task, second.ID) != secondRow {
		t.Fatal("an earlier job changed on the second retry")
	}
	if jobs, err = s.Releases(ctx, task.ID); err != nil || len(jobs) != 3 || jobs[0].ID != first.ID || jobs[1].ID != second.ID || jobs[2].ID != third.ID {
		t.Fatal("history after second retry", jobs, err)
	}
}

// a3 (r4): a job the handler refused by reconcile with a receipt and a
// restored journal is retried only when the retry names the restored release
// on exactly the receipt's targets.
func TestReleaseRetryNeedsRestorationForEveryTarget(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	first := failedReceiptJob(t, s, task, h, d, entry, "restored", restoredTargets())
	before := releaseRow(t, s, task, first.ID)
	tailos := api.ReleaseRestoredTarget{Target: "tailos", Release: "fixture-old"}
	mini := api.ReleaseRestoredTarget{Target: "mini", Release: "fixture-old"}
	for name, c := range map[string]struct {
		restored []api.ReleaseRestoredTarget
		want     error
		text     string
	}{
		"none named":      {nil, api.ErrConflict, "has a receipt for target mini"},
		"one missing":     {[]api.ReleaseRestoredTarget{mini}, api.ErrConflict, "has a receipt for target tailos"},
		"extra target":    {[]api.ReleaseRestoredTarget{tailos, mini, {Target: "hub", Release: "fixture-old"}}, api.ErrConflict, "has no receipt for target hub"},
		"unknown target":  {[]api.ReleaseRestoredTarget{tailos, {Target: "nas", Release: "x"}}, api.ErrInvalid, ""},
		"duplicate":       {[]api.ReleaseRestoredTarget{tailos, tailos}, api.ErrInvalid, ""},
		"no release name": {[]api.ReleaseRestoredTarget{tailos, {Target: "mini"}}, api.ErrInvalid, ""},
		"newline in name": {[]api.ReleaseRestoredTarget{tailos, {Target: "mini", Release: "a\nb"}}, api.ErrInvalid, ""},
	} {
		_, err := s.ReleaseAction(ctx, task.ID, retryRequest("refused-"+strings.ReplaceAll(name, " ", "-"), h, entry, first, c.restored...))
		if !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.text) || (c.text != "" && !strings.Contains(err.Error(), first.ID)) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	noReason := retryRequest("no-reason", h, entry, first, tailos, mini)
	noReason.Retry.Reason = ""
	if _, err := s.ReleaseAction(ctx, task.ID, noReason); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("retry without a reason", err)
	}
	if jobs, err := s.Releases(ctx, task.ID); err != nil || len(jobs) != 1 {
		t.Fatal("a refused retry added a job", jobs, err)
	}
	second, err := s.ReleaseAction(ctx, task.ID, retryRequest("retry", h, entry, first, tailos, mini))
	if err != nil || second.State != "verified" || second.RetryOf == nil || second.RetryOf.JobID != first.ID || second.Receipt != nil || second.Published {
		t.Fatalf("%+v %v", second, err)
	}
	// Saved in the fixed target order, whatever order the request used.
	if got := second.RetryOf.Restored; len(got) != 2 || got[0] != mini || got[1] != tailos {
		t.Fatalf("restored %+v", got)
	}
	if releaseRow(t, s, task, first.ID) != before {
		t.Fatal("refused job changed")
	}
	// A job with no receipt has no target a restoration could be checked on.
	s2, task2, h2, d2, entry2 := releaseFixture(t)
	plain := refusedJob(t, s2, task2, h2, d2, entry2)
	if _, err = s2.ReleaseAction(ctx, task2.ID, retryRequest("unreceipted", h2, entry2, plain, tailos)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "has no receipt for target tailos") {
		t.Fatal("restoration named for an unreceipted job", err)
	}
}

// The effects classification retry and supersede share.
func TestReleaseEffectsClassification(t *testing.T) {
	refuse := func(journal string, resolved bool) []api.ReleaseReconciliation {
		return []api.ReleaseReconciliation{{Disposition: "requeue", JournalState: "no_effects", RefResolved: true}, {Disposition: "refuse", JournalState: journal, RefResolved: resolved}}
	}
	receipt := func(targets ...api.ReleaseTargetReceipt) *api.ReleaseReceipt {
		return &api.ReleaseReceipt{Outcome: "blocked", Targets: targets}
	}
	for name, c := range map[string]struct {
		j      api.ReleaseJob
		kind   string
		detail string
	}{
		"refused before publication":       {api.ReleaseJob{State: "refused"}, "none", ""},
		"requeued then refused":            {api.ReleaseJob{State: "refused", Reconciliations: refuse("no_effects", true)[:1]}, "none", ""},
		"rolled back":                      {api.ReleaseJob{State: "rolled_back", Published: true, Receipt: &api.ReleaseReceipt{Outcome: "rolled_back", Targets: restoredTargets()[1:]}}, "restored", ""},
		"published, journal restored":      {api.ReleaseJob{State: "refused", Published: true, Reconciliations: refuse("restored", true)}, "restored", ""},
		"receipt, journal restored":        {api.ReleaseJob{State: "refused", Published: true, Receipt: receipt(unrestoredTargets()...), Reconciliations: refuse("restored", true)}, "restored", ""},
		"failed targets, no effects":       {api.ReleaseJob{State: "refused", Published: true, Receipt: receipt(unrestoredTargets()[:1]...), Reconciliations: refuse("no_effects", true)}, "restored", ""},
		"released target, no effects":      {api.ReleaseJob{State: "refused", Published: true, Receipt: receipt(unrestoredTargets()...), Reconciliations: refuse("no_effects", true)}, "unrestored", "target tailos is released"},
		"blocked rollback, no effects":     {api.ReleaseJob{State: "refused", Published: true, Receipt: receipt(api.ReleaseTargetReceipt{Target: "hub", Outcome: "failed", Rollback: "blocked"}), Reconciliations: refuse("no_effects", true)}, "unrestored", "target hub rollback is blocked"},
		"restored but ref unresolved":      {api.ReleaseJob{State: "refused", Published: true, Reconciliations: refuse("restored", false)}, "unrestored", "published"},
		"published, no reconciliation":     {api.ReleaseJob{State: "refused", Published: true}, "unrestored", "published"},
		"published, last is a requeue":     {api.ReleaseJob{State: "refused", Published: true, Reconciliations: refuse("restored", true)[:1]}, "unrestored", "published"},
		"receipt, no reconciliation":       {api.ReleaseJob{State: "refused", Receipt: receipt(unrestoredTargets()[:1]...)}, "unrestored", "receipt outcome blocked"},
		"released target, unknown journal": {api.ReleaseJob{State: "refused", Receipt: receipt(unrestoredTargets()...), Reconciliations: refuse("unknown", true)}, "unrestored", "target tailos is released"},
	} {
		if kind, detail := releaseEffects(c.j); kind != c.kind || !strings.Contains(detail, c.detail) || (c.detail == "" && detail != "") {
			t.Fatalf("%s: %s %q", name, kind, detail)
		}
	}
}

// a4 (r2, j2): a retry is refused, naming the job and the reason, for a job
// with effects that are not restored, a job that is still held, and a job
// that released or was superseded. Nothing is added to the ledger.
func TestReleaseRetryRefusesEffectsAndHeldJobs(t *testing.T) {
	ctx := context.Background()
	refused := func(t *testing.T, s *Store, task api.Task, h api.Agent, entry string, j api.ReleaseJob, want string, restored ...api.ReleaseRestoredTarget) {
		t.Helper()
		before := releaseRow(t, s, task, j.ID)
		_, err := s.ReleaseAction(ctx, task.ID, retryRequest("retry", h, entry, j, restored...))
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), j.ID) || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q naming %s, got %v", want, j.ID, err)
		}
		if jobs, err := s.Releases(ctx, task.ID); err != nil || len(jobs) != 1 || releaseRow(t, s, task, j.ID) != before {
			t.Fatal("refused retry changed the ledger", jobs, err)
		}
	}
	both := []api.ReleaseRestoredTarget{{Target: "tailos", Release: "fixture-old"}, {Target: "mini", Release: "fixture-old"}}
	t.Run("released target", func(t *testing.T) {
		s, task, h, d, entry := releaseFixture(t)
		j := failedReceiptJob(t, s, task, h, d, entry, "no_effects", unrestoredTargets())
		refused(t, s, task, h, entry, j, "has release effects that are not restored (target tailos is released); roll back, reconcile, or supersede with a hand release", both...)
	})
	t.Run("blocked rollback", func(t *testing.T) {
		s, task, h, d, entry := releaseFixture(t)
		targets := restoredTargets()
		targets[1].Outcome, targets[1].Rollback = "failed", "blocked"
		j := failedReceiptJob(t, s, task, h, d, entry, "no_effects", targets)
		refused(t, s, task, h, entry, j, "has release effects that are not restored (target mini rollback is blocked)", both...)
	})
	step := func(t *testing.T, s *Store, task api.Task, d api.Agent, j api.ReleaseJob, ops ...string) api.ReleaseJob {
		t.Helper()
		for _, op := range ops {
			req := api.ReleaseRequest{RequestID: "held-" + op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}
			if op == "merged" {
				req.IntegratedCommit = j.Commit
			}
			if op == "finish" {
				req.Receipt = &api.ReleaseReceipt{Version: 1, JobID: j.ID, Commit: j.Commit, VerificationDigest: j.VerificationDigest, Outcome: "released", Targets: []api.ReleaseTargetReceipt{{Target: "tailos", Release: "fixture", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "released"}}}
			}
			var err error
			if j, err = s.ReleaseAction(ctx, task.ID, req); err != nil {
				t.Fatal(op, err)
			}
		}
		return j
	}
	for name, c := range map[string]struct {
		ops  []string
		want string
	}{
		"verified": {nil, "is verified; reconcile it first"},
		"claimed":  {[]string{"claim"}, "is claimed; reconcile it first"},
		"merged":   {[]string{"claim", "merged"}, "is merged; reconcile it first"},
		"blocked":  {[]string{"claim", "block"}, "is blocked; reconcile it first"},
		"released": {[]string{"claim", "merged", "finish"}, "already released"},
	} {
		t.Run(name, func(t *testing.T) {
			s, task, h, d, entry := releaseFixture(t)
			j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "first-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
			if err != nil {
				t.Fatal(err)
			}
			j = step(t, s, task, d, j, c.ops...)
			var restored []api.ReleaseRestoredTarget
			if j.Receipt != nil {
				restored = both[:1]
			}
			refused(t, s, task, h, entry, j, c.want, restored...)
		})
	}
	t.Run("superseded", func(t *testing.T) {
		s, task, h, d, entry := releaseFixture(t)
		j := refusedJob(t, s, task, h, d, entry)
		hand := recordHandRelease(t, s, task, "hand", candidateB, []string{candidateB})
		j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "supersede", Operation: "supersede", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Supersession: &api.ReleaseSupersession{ReleasedCommit: candidateB, HandReleaseID: hand.ID}})
		if err != nil {
			t.Fatal(err)
		}
		refused(t, s, task, h, entry, j, "already released (superseded by a hand release)")
	})
}

// a5 (r1, j1): the retry re-checks the candidate and the project pause
// generation; a new verification, another accepted commit or a pause since
// the refused job each refuse it by name.
func TestReleaseRetryRequiresUnchangedCandidate(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		change func(t *testing.T, s *Store, task api.Task, entry string, j api.ReleaseJob)
		want   string
	}{
		"new verification": {func(t *testing.T, s *Store, task api.Task, entry string, j api.ReleaseJob) {
			item, err := s.GetWorkItem(ctx, task.ID, j.ItemID)
			if err != nil {
				t.Fatal(err)
			}
			seedPassingVerification(t, s, item, candidateB)
		}, "candidate changed since "},
		"new accepted commit": {func(t *testing.T, s *Store, task api.Task, entry string, j api.ReleaseJob) {
			raw, _ := json.Marshal(api.TeamIntegrationAcceptance{Repository: "fixture", BaseCommit: candidateA, Commit: candidateC, ItemRevision: j.ItemRevision})
			if _, err := s.db.Exec(`UPDATE team_queue_entries SET acceptance_json=? WHERE id=?`, string(raw), entry); err != nil {
				t.Fatal(err)
			}
		}, "candidate changed since "},
		"item reopened": {func(t *testing.T, s *Store, task api.Task, entry string, j api.ReleaseJob) {
			if _, err := s.db.Exec(`UPDATE work_items SET status='open' WHERE id=?`, j.ItemID); err != nil {
				t.Fatal(err)
			}
		}, "candidate changed since "},
		"pause generation": {func(t *testing.T, s *Store, task api.Task, entry string, j api.ReleaseJob) {
			if _, err := s.db.Exec(`UPDATE tasks SET pause_generation=pause_generation+1 WHERE id=?`, task.ID); err != nil {
				t.Fatal(err)
			}
		}, "project generation changed since "},
	} {
		t.Run(name, func(t *testing.T) {
			s, task, h, d, entry := releaseFixture(t)
			j := refusedJob(t, s, task, h, d, entry)
			c.change(t, s, task, entry, j)
			_, err := s.ReleaseAction(ctx, task.ID, retryRequest("retry", h, entry, j))
			if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), c.want+j.ID) {
				t.Fatalf("want %q, got %v", c.want+j.ID, err)
			}
			if jobs, err := s.Releases(ctx, task.ID); err != nil || len(jobs) != 1 {
				t.Fatal("refused retry added a job", jobs, err)
			}
		})
	}
}

// a6 (r4): only the handler retries; the same request replays the same job,
// a changed one is refused, and a retry of a job that is no longer the
// entry's latest names the newer job.
func TestReleaseRetryIsHandlerOnlyAndIdempotent(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	first := refusedJob(t, s, task, h, d, entry)
	member, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "builder", Host: "fixture", Session: "builder"}, by)
	if err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]api.Agent{"deployer": d, "member": member, "stale handler run": {ID: h.ID, RunID: api.NewID("run")}} {
		if _, err = s.ReleaseAction(ctx, task.ID, retryRequest("by-"+strings.ReplaceAll(name, " ", "-"), a, entry, first)); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("%s retried: %v", name, err)
		}
	}
	// A request with no agent identity (the owner's shell) is refused too.
	if _, err = s.ReleaseAction(ctx, task.ID, retryRequest("by-owner", api.Agent{}, entry, first)); err == nil {
		t.Fatal("a request without an agent identity retried")
	}
	wrongGeneration := retryRequest("wrong-generation", h, entry, first)
	wrongGeneration.ExpectedGeneration++
	if _, err = s.ReleaseAction(ctx, task.ID, wrongGeneration); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "generation changed") {
		t.Fatal("wrong generation", err)
	}
	other := releaseEntry(t, s, task, "another entry", "other")
	if _, err = s.ReleaseAction(ctx, task.ID, retryRequest("wrong-entry", h, other, first)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "belongs to another entry") {
		t.Fatal("wrong entry", err)
	}
	unknown := retryRequest("unknown-job", h, entry, first)
	unknown.JobID = api.NewID("rel")
	if _, err = s.ReleaseAction(ctx, task.ID, unknown); !errors.Is(err, api.ErrNotFound) {
		t.Fatal("unknown job", err)
	}
	if jobs, err := s.Releases(ctx, task.ID); err != nil || len(jobs) != 1 {
		t.Fatal("a refused retry added a job", jobs, err)
	}
	req := retryRequest("retry", h, entry, first)
	second, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil || again.ID != second.ID || again.RetryOf == nil || again.RetryOf.CreatedAt != second.RetryOf.CreatedAt {
		t.Fatal("replay", again, err)
	}
	changed := retryRequest("retry", h, entry, first)
	changed.Retry.Reason = "A different reason"
	if _, err = s.ReleaseAction(ctx, task.ID, changed); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "retry changed") {
		t.Fatal("changed replay", err)
	}
	_, err = s.ReleaseAction(ctx, task.ID, retryRequest("second-retry", h, entry, first))
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "entry's latest release job is "+second.ID+" (verified)") {
		t.Fatal("second retry of the same refused job", err)
	}
	if jobs, err := s.Releases(ctx, task.ID); err != nil || len(jobs) != 2 {
		t.Fatal("job count", jobs, err)
	}
}

// a7 (r5): enqueue on an entry that already has a job names it; for a refused
// job it gives the retry command with the job's generation.
func TestReleaseEnqueueNamesRefusedJobAndRetry(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	enqueue := func(key string) error {
		_, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: key, Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
		return err
	}
	if err := enqueue("first-enqueue"); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.Releases(ctx, task.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	if err = enqueue("again-verified"); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "entry already has release job "+jobs[0].ID+" (verified); recover the original request receipt") || strings.Contains(err.Error(), "tt deployment retry") {
		t.Fatal("enqueue over a verified job", err)
	}
	j := jobs[0]
	for _, op := range []string{"claim", "refuse"} {
		if j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "first-" + op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}); err != nil {
			t.Fatal(op, err)
		}
	}
	want := fmt.Sprintf("entry already has release job %s (refused); retry it with tt deployment retry --entry %s --job %s --generation %d --reason TEXT", j.ID, entry, j.ID, j.Generation)
	if err = enqueue("again-refused"); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), want) {
		t.Fatalf("enqueue over a refused job: %v", err)
	}
	second, err := s.ReleaseAction(ctx, task.ID, retryRequest("retry", h, entry, j))
	if err != nil {
		t.Fatal(err)
	}
	if err = enqueue("again-retried"); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "entry already has release job "+second.ID+" (verified)") {
		t.Fatal("enqueue names the entry's latest job", err)
	}
}

// a8/a9 (r6): a refused job whose receipt shows a failed deploy that was
// restored is superseded by a covering hand release; its receipt and
// reconciliation stay, and the supersession carries the record's targets and
// time for the target baselines. Without a covering record, or with effects
// that are not restored, it is refused by name. A rolled-back job is
// supersedable the same way.
func TestSupersedeRefusedJobWithFailedReceipt(t *testing.T) {
	ctx := context.Background()
	supersede := func(s *Store, task api.Task, h api.Agent, j api.ReleaseJob, key, released, record string) (api.ReleaseJob, error) {
		return s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: key, Operation: "supersede", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Supersession: &api.ReleaseSupersession{ReleasedCommit: released, HandReleaseID: record}})
	}
	t.Run("restored", func(t *testing.T) {
		s, task, h, d, entry := releaseFixture(t)
		j := failedReceiptJob(t, s, task, h, d, entry, "restored", restoredTargets())
		before := releaseRow(t, s, task, j.ID)
		if _, err := supersede(s, task, h, j, "unknown", candidateC, "hrl_0123456789abcdef"); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "recorded hand release required") {
			t.Fatal("no record", err)
		}
		uncovering := recordHandRelease(t, s, task, "uncovering", candidateC, []string{candidateA})
		if _, err := supersede(s, task, h, j, "uncovered", candidateC, uncovering.ID); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "hand release does not cover the job's commit") {
			t.Fatal("uncovering record", err)
		}
		if releaseRow(t, s, task, j.ID) != before {
			t.Fatal("refused supersede changed the job")
		}
		hand := recordHandRelease(t, s, task, "hand", candidateC, []string{j.Commit})
		done, err := supersede(s, task, h, j, "supersede", candidateC, hand.ID)
		if err != nil || done.State != "superseded" || done.Supersession == nil || done.Supersession.HandReleaseID != hand.ID || strings.Join(done.Supersession.Targets, ",") != "hub" || done.SettledAt != hand.CreatedAt || !done.Published {
			t.Fatalf("%+v %v", done, err)
		}
		was, _ := json.Marshal(j.Receipt)
		kept, _ := json.Marshal(done.Receipt)
		if done.Receipt == nil || string(was) != string(kept) || len(done.Reconciliations) != 1 || done.Reconciliations[0] != j.Reconciliations[0] {
			t.Fatalf("history changed: receipt %s reconciliations %+v", kept, done.Reconciliations)
		}
	})
	t.Run("unrestored", func(t *testing.T) {
		s, task, h, d, entry := releaseFixture(t)
		j := failedReceiptJob(t, s, task, h, d, entry, "no_effects", unrestoredTargets())
		before := releaseRow(t, s, task, j.ID)
		hand := recordHandRelease(t, s, task, "hand", candidateC, []string{j.Commit})
		_, err := supersede(s, task, h, j, "supersede", candidateC, hand.ID)
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "job "+j.ID+" has release effects that are not restored (target tailos is released)") {
			t.Fatal("unrestored job superseded", err)
		}
		if releaseRow(t, s, task, j.ID) != before {
			t.Fatal("refused supersede changed the job")
		}
	})
	t.Run("rolled back", func(t *testing.T) {
		s, task, h, d, entry := releaseFixture(t)
		j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range []string{"claim", "merged", "finish"} {
			req := api.ReleaseRequest{RequestID: op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit}
			if op == "finish" {
				req.IntegratedCommit = ""
				req.Receipt = &api.ReleaseReceipt{Version: 1, JobID: j.ID, Commit: j.Commit, VerificationDigest: j.VerificationDigest, Outcome: "rolled_back", Targets: restoredTargets()[1:]}
			}
			if j, err = s.ReleaseAction(ctx, task.ID, req); err != nil {
				t.Fatal(op, err)
			}
		}
		hand := recordHandRelease(t, s, task, "hand", candidateC, []string{j.Commit})
		done, err := supersede(s, task, h, j, "supersede", candidateC, hand.ID)
		if err != nil || done.State != "superseded" || done.Receipt == nil || done.Receipt.Outcome != "rolled_back" || done.SettledAt != hand.CreatedAt {
			t.Fatalf("%+v %v", done, err)
		}
	})
}

const releaseJobsSchemaBeforeRetry = `CREATE TABLE release_jobs (
 task_id TEXT NOT NULL REFERENCES tasks(id),id TEXT NOT NULL,entry_id TEXT NOT NULL,
 state TEXT NOT NULL,generation INTEGER NOT NULL,record_json TEXT NOT NULL,
 PRIMARY KEY(task_id,id),UNIQUE(task_id,entry_id));
 CREATE UNIQUE INDEX release_project_fence ON release_jobs(task_id) WHERE state IN ('claimed','merged','blocked');`

// a12: a database from before a retry could add a job opens with every job
// row, its order and the one-holder fence kept, then accepts a second job for
// one entry; opening it again changes nothing.
func TestReleaseJobsMigrationAllowsSeveralJobsPerEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "old ledger"}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	// The table as shipped before, with rows whose rowids are out of
	// insertion order and have gaps, as set-aside leaves them.
	if _, err = s.db.Exec(`DROP TABLE release_jobs;` + releaseJobsSchemaBeforeRetry); err != nil {
		t.Fatal(err)
	}
	entries := []string{api.NewID("tqe"), api.NewID("tqe"), api.NewID("tqe")}
	for i, row := range []struct {
		rowid int64
		state string
	}{{7, "refused"}, {2, "claimed"}, {11, "verified"}} {
		j := api.ReleaseJob{ID: api.NewID("rel"), TaskID: task.ID, EntryID: entries[i], Commit: candidateB, State: row.state, Generation: int64(i + 3)}
		raw, _ := json.Marshal(j)
		if _, err = s.db.Exec(`INSERT INTO release_jobs(rowid,task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,?,?,?,?)`, row.rowid, task.ID, j.ID, j.EntryID, j.State, j.Generation, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec(`INSERT INTO release_jobs VALUES(?,?,?,'verified',1,'{}')`, task.ID, api.NewID("rel"), entries[0]); err == nil {
		t.Fatal("the old table accepted a second job for one entry")
	}
	dump := func(s *Store) string {
		t.Helper()
		rows, err := s.db.Query(`SELECT rowid,task_id,id,entry_id,state,generation,record_json FROM release_jobs ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var rowid, generation int64
			var taskID, id, entry, state, raw string
			if err = rows.Scan(&rowid, &taskID, &id, &entry, &state, &generation, &raw); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%d|%s|%s|%s|%s|%d|%s", rowid, taskID, id, entry, state, generation, raw))
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, "\n")
	}
	shape := func(s *Store) string {
		t.Helper()
		rows, err := s.db.Query(`SELECT type||' '||name||' '||coalesce(sql,'') FROM sqlite_master WHERE tbl_name LIKE 'release_jobs%' ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var line string
			if err = rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
	before := dump(s)
	if strings.Count(before, "\n") != 2 {
		t.Fatal(before)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if after := dump(s); after != before {
		t.Fatalf("rows changed\nbefore\n%s\nafter\n%s", before, after)
	}
	// The rebuild holds its temporary storage in memory and then restores
	// the connection's setting.
	var tempStore int
	if err = s.db.QueryRow(`PRAGMA temp_store`).Scan(&tempStore); err != nil || tempStore != 0 {
		t.Fatal("temp_store after the rebuild", tempStore, err)
	}
	migrated := shape(s)
	if strings.Contains(migrated, "UNIQUE(task_id,entry_id)") || strings.Contains(migrated, "release_jobs_v2") || !strings.Contains(migrated, "index release_project_fence") || !strings.Contains(migrated, "index release_jobs_entry") {
		t.Fatal(migrated)
	}
	jobs, err := s.Releases(ctx, task.ID)
	if err != nil || len(jobs) != 3 || jobs[0].State != "claimed" || jobs[1].State != "refused" || jobs[2].State != "verified" {
		t.Fatal("list order", jobs, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if dump(s) != before || shape(s) != migrated {
		t.Fatal("a second open changed the ledger")
	}
	second := api.NewID("rel")
	if _, err = s.db.Exec(`INSERT INTO release_jobs VALUES(?,?,?,'verified',1,'{}')`, task.ID, second, entries[0]); err != nil {
		t.Fatal("second job for one entry", err)
	}
	var rowid int64
	if err = s.db.QueryRow(`SELECT rowid FROM release_jobs WHERE id=?`, second).Scan(&rowid); err != nil || rowid != 12 {
		t.Fatal("a new job must queue behind every row", rowid, err)
	}
	if _, err = s.db.Exec(`UPDATE release_jobs SET state='claimed' WHERE id=?`, second); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatal("the fence allowed a second holder", err)
	}
}

// fenceCheck is the runner's check: one request ID per job and generation,
// repeated on every step and poll while the generation stands.
func fenceCheck(j api.ReleaseJob, d api.Agent, generation int64) api.ReleaseRequest {
	return api.ReleaseRequest{RequestID: fmt.Sprintf("%s-check-%d", j.ID, generation), Operation: "check", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: generation}
}

// The handler reads the job after the claim and imports a minute later; the
// runner's fence checks in between change nothing and must not cost it the
// generation it bound to.
func TestReleaseCheckKeepsGenerationForHandlerImport(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	approved := goRaceMatrix(goRace(raceFlags, "./cmd/tt"))
	j := claimGoRaceJob(t, s, task, h, d, entry, approved)
	for i := range 3 {
		checked, err := s.ReleaseAction(ctx, task.ID, fenceCheck(j, d, j.Generation))
		if err != nil || checked.Generation != j.Generation || checked.State != "claimed" {
			t.Fatal("check", i, checked.Generation, checked.State, err)
		}
	}
	saved, err := releaseLoad(ctx, s.db, task.ID, j.ID)
	if err != nil || saved.Generation != j.Generation {
		t.Fatal("checks moved the generation", saved.Generation, err)
	}
	var receipts int
	if err = s.db.QueryRow(`SELECT count(*) FROM release_action_receipts WHERE task_id=? AND request_id=?`, task.ID, fenceCheck(j, d, j.Generation).RequestID).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("check stored a receipt", receipts, err)
	}
	imported, err := s.ReleaseAction(ctx, task.ID, integratedImport(j, h, d, "import-after-checks", approved))
	if err != nil || imported.Generation != j.Generation+1 || imported.IntegratedVerification == nil {
		t.Fatal("import after checks", imported.Generation, err)
	}
	// The import is a substantive change: a second import still bound to the
	// earlier read, and the runner's check at that generation, both refuse.
	if _, err = s.ReleaseAction(ctx, task.ID, integratedImport(j, h, d, "import-stale", approved)); !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "generation changed") {
		t.Fatal("stale import", err)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, fenceCheck(j, d, j.Generation)); !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "generation changed") {
		t.Fatal("stale check", err)
	}
	if checked, err := s.ReleaseAction(ctx, task.ID, fenceCheck(j, d, imported.Generation)); err != nil || checked.Generation != imported.Generation {
		t.Fatal("check after import", checked.Generation, err)
	}
	// Another run: the same request ID that just passed is judged again, for
	// the replaced run and for a run that never held the claim.
	s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, d.ID)
	rotated, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: d.ID, ExpectedRunID: d.RunID, Name: d.Name, Role: d.Role, Host: d.Host, Session: d.Session}, by)
	if err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`UPDATE agents SET status='running',last_seen_at=? WHERE id=?`, ts(time.Now()), rotated.ID)
	if _, err = s.ReleaseAction(ctx, task.ID, fenceCheck(j, d, imported.Generation)); !errors.Is(err, api.ErrConflict) {
		t.Fatal("replaced run kept the fence", err)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, fenceCheck(j, rotated, imported.Generation)); !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "fence belongs to another exact run") {
		t.Fatal("another run passed the fence", err)
	}
	if saved, err = releaseLoad(ctx, s.db, task.ID, j.ID); err != nil || saved.Generation != imported.Generation {
		t.Fatal("refused checks moved the generation", saved.Generation, err)
	}
}

// A check that passed is not a receipt to replay: once the handler sets the
// job aside, the same request ID and generation no longer hold the fence.
func TestReleaseCheckAfterSetAsideIsRefused(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	j := claimGoRaceJob(t, s, task, h, d, entry, goRaceMatrix(goRace(raceFlags, "./cmd/tt")))
	if _, err := s.ReleaseAction(ctx, task.ID, fenceCheck(j, d, j.Generation)); err != nil {
		t.Fatal(err)
	}
	// Set-aside needs a later verified job waiting on the fence.
	if _, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "waiting-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: releaseEntry(t, s, task, "waiting item", "waiting")}); err != nil {
		t.Fatal(err)
	}
	evidence := setAsideEvidence(t, s, task, h, j)
	aside, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "set-aside", Operation: "set-aside", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Reconciliation: &evidence})
	if err != nil || aside.State != "verified" || aside.Generation != j.Generation+1 {
		t.Fatal(aside.State, aside.Generation, err)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, fenceCheck(j, d, j.Generation)); !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "generation changed") {
		t.Fatal("repeated check after set-aside", err)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, fenceCheck(j, d, aside.Generation)); !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "fence belongs to another exact run") {
		t.Fatal("check after set-aside", err)
	}
	if saved, err := releaseLoad(ctx, s.db, task.ID, j.ID); err != nil || saved.Generation != aside.Generation || saved.State != "verified" {
		t.Fatal("refused checks changed the job", saved.Generation, saved.State, err)
	}
}
