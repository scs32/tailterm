package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"os"
	"path/filepath"
	"reflect"
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

	detail, detailErr := s.Release(ctx, task.ID, j.ID)
	if detailErr != nil || !slices.Equal(detail.MatrixApprovals, wantApprovals) {
		t.Fatal("detail approval hint", detailErr, detail.MatrixApprovals)
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

// wi_3aca3c7a3a869afb x1: an integrated plan whose matrix digests are not
// context digests is refused before any refusal text could echo them, even
// with an owner message that names the same text.
func TestReleaseImportRefusesInvalidMatrixDigestWithoutEchoingIt(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	approved := goRaceMatrix(goRace(raceFlags, "./cmd/tt"))
	j := claimGoRaceJob(t, s, task, h, d, entry, approved)
	const want = "release: integrated plan matrix digest is not a valid digest"
	for name, c := range map[string]struct{ digest, approved string }{
		"non-hex":          {strings.Repeat("Z", 64), strings.Repeat("Z", 64)},
		"upper-hex":        {strings.Repeat("F", 64), strings.Repeat("F", 64)},
		"markup":           {"<b>9f9f9f9f</b>", "<b>9f9f9f9f</b>"},
		"empty":            {"", ""},
		"approved-invalid": {matrixB, "QQQQQQQQ"},
		"plan-invalid":     {"QQQQQQQQ", matrixB},
	} {
		covering := ownerApproval(t, s, task, "verification-matrix-approval:"+c.digest)
		req := matrixImport(j, h, d, "bad-digest-"+name, c.digest, covering, approved)
		req.Plan.ApprovedMatrixDigest = c.approved
		_, err := s.ReleaseAction(ctx, task.ID, req)
		if !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("%s: %v", name, err)
		}
		for _, bad := range []string{c.digest, c.approved} {
			if bad != "" && bad != matrixB && strings.Contains(err.Error(), bad[:min(8, len(bad))]) {
				t.Fatalf("%s: refusal echoes %q: %v", name, bad, err)
			}
		}
		saved, err := releaseLoad(ctx, s.db, task.ID, j.ID)
		if err != nil || saved.Generation != j.Generation || saved.IntegratedPlan != nil || saved.IntegratedMatrix != nil {
			t.Fatal(name, "changed the job", saved.Generation, err)
		}
	}
	// A valid changed digest without an approval still names both digests.
	_, err := s.ReleaseAction(ctx, task.ID, matrixImport(j, h, d, "valid-unapproved", matrixB, 0, approved))
	if !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "matrix digest changed aaaaaaaa -> bbbbbbbb; no approval") {
		t.Fatal(err)
	}
}

// legacyMatrixApprovals is the approvals read before wi_3aca3c7a3a869afb
// x2: a leading-wildcard LIKE over every project message. It is the oracle
// the indexed read must match.
func legacyMatrixApprovals(t *testing.T, s *Store, task string) []api.ReleaseMatrixApproval {
	t.Helper()
	rows, err := s.db.Query(`SELECT seq,text FROM messages WHERE task_id=? AND from_agent='' AND from_node<>'system' AND text LIKE ? ORDER BY seq`, task, "%"+matrixApprovalPrefix+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []api.ReleaseMatrixApproval
	for rows.Next() {
		var seq int64
		var text string
		if err = rows.Scan(&seq, &text); err != nil {
			t.Fatal(err)
		}
		digest, ok := strings.CutPrefix(strings.TrimSpace(text), matrixApprovalPrefix)
		if !ok || !validContextDigest(digest) {
			continue
		}
		out = slices.DeleteFunc(out, func(a api.ReleaseMatrixApproval) bool { return a.Digest == digest })
		out = append(out, api.ReleaseMatrixApproval{Digest: digest, MessageSeq: seq})
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// wi_3aca3c7a3a869afb x2: while a job is claimed, list and detail reads find
// approvals through the partial index of token-bearing owner messages, not a
// scan of every project message, and return what the old scan returned.
func TestReleaseMatrixApprovalsUseIndexAndMatchScan(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	j := claimGoRaceJob(t, s, task, h, d, entry, goRaceMatrix(goRace(raceFlags, "./cmd/tt")))
	matrixC := strings.Repeat("c", 64)
	other := secondTask(t, s)
	ownerApproval(t, s, other, "verification-matrix-approval:"+matrixC)
	for i := range 50 {
		ownerApproval(t, s, task, fmt.Sprintf("ordinary owner message %d", i))
	}
	first := ownerApproval(t, s, task, "verification-matrix-approval:"+matrixA)
	ownerApproval(t, s, task, "I approve verification-matrix-approval:"+matrixB)
	ownerApproval(t, s, task, "VERIFICATION-MATRIX-APPROVAL:"+matrixB)
	ownerApproval(t, s, task, "verification-matrix-approval:"+strings.Repeat("Z", 64))
	ownerApproval(t, s, task, "verification-matrix-approval:"+matrixB+" and more")
	if _, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "verification-matrix-approval:" + matrixC, RequestID: "agent-approval", AgentID: h.ID}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	spaced := ownerApproval(t, s, task, "\t verification-matrix-approval:"+matrixB+"\n")
	newest := ownerApproval(t, s, task, "verification-matrix-approval:"+matrixA)
	want := []api.ReleaseMatrixApproval{{Digest: matrixB, MessageSeq: spaced}, {Digest: matrixA, MessageSeq: newest}}
	if newest <= first {
		t.Fatal("message order", first, newest)
	}
	if legacy := legacyMatrixApprovals(t, s, task.ID); !slices.Equal(legacy, want) {
		t.Fatalf("oracle %+v", legacy)
	}
	jobs, err := s.Releases(ctx, task.ID)
	if err != nil || len(jobs) != 1 || jobs[0].ID != j.ID || !slices.Equal(jobs[0].MatrixApprovals, want) {
		t.Fatalf("list approvals %+v %v", jobs, err)
	}
	detail, err := s.Release(ctx, task.ID, j.ID)
	if err != nil || !slices.Equal(detail.MatrixApprovals, want) {
		t.Fatalf("detail approvals %+v %v", detail.MatrixApprovals, err)
	}

	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+matrixApprovalsQuery, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	rows.Close()
	if len(plan) != 1 || !strings.Contains(plan[0], "USING INDEX messages_matrix_approvals") {
		t.Fatalf("approvals query plan %q", plan)
	}
	// The index holds only owner messages that carry the token.
	var indexed, tokens int
	if err = s.db.QueryRow(`SELECT count(*) FROM messages INDEXED BY messages_matrix_approvals WHERE task_id=? AND `+matrixApprovalCandidate, task.ID).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=? AND from_agent='' AND instr(text,?)>0`, task.ID, matrixApprovalPrefix).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if indexed != tokens || indexed != 6 {
		t.Fatal("indexed rows", indexed, tokens)
	}
}

// shardedRace is the go-race of a matrix with goRaceShards: the matrix
// runner's own command with the shard count in its prefix, then only packages
// (wi_d6b6387726c1f846). The coverage rule reads it like the go test form.
func shardedRace(k int, packages ...string) api.VerificationCheck {
	race := goRace(nil, packages...)
	race.Argv = append([]string{"node", "../scripts/verify-matrix.mjs", "go-race", "-timeout=45m", fmt.Sprintf("-shards=./internal/store=%d", k)}, packages...)
	return race
}

// Under one matrix a sharded go-race is covered by a sharded go-race with the
// same prefix and a package superset. Another shard count is another prefix.
func TestReleaseImportCoversShardedGoRaceBySuperset(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	approved := goRaceMatrix(shardedRace(4, "./internal/api"))
	j := claimGoRaceJob(t, s, task, h, d, entry, approved)
	refuse := func(name string, checks []api.VerificationCheck) {
		t.Helper()
		_, err := s.ReleaseAction(ctx, task.ID, integratedImport(j, h, d, "shard-"+name, checks))
		if !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "integrated matrix omitted approved check go-race") {
			t.Fatalf("%s: %v", name, err)
		}
		saved, err := releaseLoad(ctx, s.db, task.ID, j.ID)
		if err != nil || saved.Generation != j.Generation || saved.IntegratedPlan != nil || saved.IntegratedCoverage != nil {
			t.Fatal(name, "changed the job", saved.Generation, saved.IntegratedCoverage, err)
		}
	}
	refuse("other-k", goRaceMatrix(shardedRace(5, widerPackages...)))
	refuse("missing-package", goRaceMatrix(shardedRace(4, "./cmd/tt", "./internal/store")))
	refuse("go-test-form", goRaceMatrix(goRace([]string{"-race", "-timeout=45m"}, widerPackages...)))
	refuse("flag-after", goRaceMatrix(shardedRace(4, append(slices.Clone(widerPackages), "-shards=./internal/api=2")...)))
	// An approved store-only go-race and an approved "./..." follow the same rule.
	j = approveChecks(t, s, j, goRaceMatrix(shardedRace(4, "./internal/store")))
	refuse("store-missing", goRaceMatrix(shardedRace(4, "./internal/api")))
	j = approveChecks(t, s, j, goRaceMatrix(shardedRace(4, "./...")))
	refuse("all-vs-list", goRaceMatrix(shardedRace(4, widerPackages...)))
	j = approveChecks(t, s, j, approved)

	integrated := goRaceMatrix(shardedRace(4, widerPackages...))
	imported, err := s.ReleaseAction(ctx, task.ID, integratedImport(j, h, d, "shard-import", integrated))
	if err != nil {
		t.Fatal(err)
	}
	want := []api.ReleaseCheckCoverage{{CheckID: "go-race", ApprovedDigest: verificationDigest(approved[71]), IntegratedDigest: verificationDigest(integrated[71]), Relation: "superset"}}
	if !slices.Equal(imported.IntegratedCoverage, want) || imported.IntegratedMatrix != nil || imported.Generation != j.Generation+1 {
		t.Fatalf("coverage %+v matrix %+v", imported.IntegratedCoverage, imported.IntegratedMatrix)
	}
}

// A go-race approved in the go test form, before the matrix gained shards, is
// covered by a sharded one only as an owner-approved matrix change, and only
// while every approved package is still tested.
func TestReleaseImportCoversGoTestRaceByShardsOnlyAsMatrixChange(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	approved := goRaceMatrix(goRace([]string{"-race", "-timeout=45m"}, "./internal/api", "./internal/store"))
	j := claimGoRaceJob(t, s, task, h, d, entry, approved)
	integrated := goRaceMatrix(shardedRace(4, widerPackages...))
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
	const omitted = "integrated matrix omitted approved check go-race"
	refuse("same-matrix", omitted, integratedImport(j, h, d, "shards-same-matrix", integrated))
	refuse("no-approval", "release: matrix digest changed aaaaaaaa -> bbbbbbbb; no approval", matrixImport(j, h, d, "shards-no-approval", matrixB, 0, integrated))
	covering := ownerApproval(t, s, task, "verification-matrix-approval:"+matrixB)
	refuse("store-dropped", omitted, matrixImport(j, h, d, "shards-narrow", matrixB, covering, goRaceMatrix(shardedRace(4, "./cmd/tt", "./internal/api"))))

	imported, err := s.ReleaseAction(ctx, task.ID, matrixImport(j, h, d, "shards-import", matrixB, covering, integrated))
	if err != nil {
		t.Fatal(err)
	}
	wantMatrix := api.ReleaseMatrixChange{ApprovedDigest: matrixA, IntegratedDigest: matrixB, ApprovalMessageSeq: covering}
	wantCoverage := []api.ReleaseCheckCoverage{{CheckID: "go-race", ApprovedDigest: verificationDigest(approved[71]), IntegratedDigest: verificationDigest(integrated[71]), Relation: "matrix_changed"}}
	if imported.IntegratedMatrix == nil || *imported.IntegratedMatrix != wantMatrix || !slices.Equal(imported.IntegratedCoverage, wantCoverage) || imported.Generation != j.Generation+1 {
		t.Fatalf("matrix %+v coverage %+v", imported.IntegratedMatrix, imported.IntegratedCoverage)
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

// planAcceptance is a running entry queued on base C for a done item whose
// candidate is B. With plan, the item has a passing plan for B on base A;
// enrolment is "required" (enrolled), "legacy" (a required=0 rollout row) or
// "" (no enrolment row).
type planAcceptance struct {
	s     *Store
	task  api.Task
	entry string
}

func newPlanAcceptance(t *testing.T, enrolment string, plan bool) planAcceptance {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "plan acceptance"}, by)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "handler", Host: "fixture", Session: "handler", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "plan item", RequestID: "item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if plan {
		seedPassingVerificationAt(t, s, item, candidateB, "/fixture/repo/.git")
	}
	now := ts(time.Now())
	if _, err = s.db.Exec(`UPDATE work_items SET status='done' WHERE id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if enrolment != "" {
		required := map[string]int{"required": 1, "legacy": 0}[enrolment]
		if _, err = s.db.Exec(`INSERT INTO verification_enrollments(task_id,item_id,agent_id,run_id,required,provenance,created_at) VALUES(?,?,?,?,?,?,?)`, task.ID, item.ID, api.NewID("agt"), api.NewID("run"), required, enrolment, now); err != nil {
			t.Fatal(err)
		}
	}
	entry := api.NewID("tqe")
	if _, err = s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,created_at,updated_at,repository,base_commit,handler_id,handler_run_id) VALUES(?,?,?,?,1,'planned',1,'running',1,?,?,?,?,?,?)`, entry, task.ID, item.ID, item.Revision, now, now, "/fixture/repo/.git", candidateC, h.ID, h.RunID); err != nil {
		t.Fatal(err)
	}
	return planAcceptance{s: s, task: task, entry: entry}
}

func (p planAcceptance) accept(t *testing.T, key, base, commit string) (api.TeamQueueEntry, error) {
	t.Helper()
	ctx := context.Background()
	current, err := p.s.GetTeamQueueEntry(ctx, p.task.ID, p.entry)
	if err != nil {
		t.Fatal(err)
	}
	item, err := p.s.GetWorkItem(ctx, p.task.ID, current.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	return p.s.TeamQueueAction(ctx, p.task.ID, api.TeamQueueRequest{RequestID: key, Operation: "accept", EntryID: p.entry, ExpectedRevision: current.Revision, HandlerAgentID: current.HandlerID, HandlerRunID: current.HandlerRunID,
		Acceptance: &api.TeamIntegrationAcceptance{Repository: "/fixture/repo/.git", BaseCommit: base, Worktree: "/fixture/worktrees/builder", Branch: "feature/fixture", Commit: commit, ItemRevision: item.Revision, Evidence: "handler terminal save"}})
}

func (p planAcceptance) unaccepted(t *testing.T, when string) {
	t.Helper()
	got, err := p.s.GetTeamQueueEntry(context.Background(), p.task.ID, p.entry)
	if err != nil || got.Acceptance != nil || got.BaseCommit != candidateC {
		t.Fatalf("%s changed the entry: %+v %v", when, got, err)
	}
}

// A non-enrolled item with a passing plan is accepted on the plan base, the
// base its release job needs, as an enrolled one is (wi_e5b42a927a0455a7).
func TestQueueAcceptanceOfNonEnrolledItemUsesPlanBaseAndEnqueuesRelease(t *testing.T) {
	for _, enrolment := range []string{"", "legacy"} {
		t.Run("enrolment="+enrolment, func(t *testing.T) {
			p := newPlanAcceptance(t, enrolment, true)
			got, err := p.accept(t, "queued-base", candidateC, candidateB)
			if err != nil {
				t.Fatal("acceptance of a non-enrolled item with a plan", err)
			}
			if got.BaseCommit != candidateA || got.Acceptance == nil || got.Acceptance.BaseCommit != candidateA || got.Acceptance.Commit != candidateB {
				t.Fatalf("acceptance did not bind the plan base: %+v", got)
			}
			if got.Release == nil || got.Release.BaseCommit != candidateA || got.Release.Commit != candidateB {
				t.Fatalf("release job not bound to the plan base and commit: %+v", got.Release)
			}
			reread, err := p.s.GetTeamQueueEntry(context.Background(), p.task.ID, p.entry)
			if err != nil || reread.BaseCommit != candidateA || reread.Acceptance == nil || reread.Acceptance.BaseCommit != candidateA {
				t.Fatal("plan base not persisted", reread.BaseCommit, err)
			}
			refused := newPlanAcceptance(t, enrolment, true)
			if _, err := refused.accept(t, "wrong-base", strings.Repeat("d", 40), candidateB); !errors.Is(err, api.ErrConflict) {
				t.Fatal("an unrelated base was accepted", err)
			}
			refused.unaccepted(t, "an unrelated base")
		})
	}
}

// The plan pins the exact commit for a non-enrolled item, as tt does.
func TestQueueAcceptanceOfNonEnrolledItemRefusesOtherCommitThanPlan(t *testing.T) {
	p := newPlanAcceptance(t, "", true)
	if _, err := p.accept(t, "other-commit", candidateC, candidateC); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is not the verification plan commit") {
		t.Fatal("a commit other than the plan's was accepted", err)
	}
	p.unaccepted(t, "another commit")
}

// A current plan without a receipt still sets the base; no release job is
// enqueued without a receipt.
func TestQueueAcceptanceOfNonEnrolledItemWithUnreceiptedPlanUsesPlanBase(t *testing.T) {
	p := newPlanAcceptance(t, "", true)
	ctx := context.Background()
	current, err := p.s.GetTeamQueueEntry(ctx, p.task.ID, p.entry)
	if err != nil {
		t.Fatal(err)
	}
	records, err := verificationRecords(ctx, p.s.db, p.task.ID, current.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := currentVerification(records)
	next := *plan
	next.OperationKey = api.NewID("req")
	b, _ := json.Marshal(api.VerificationRecord{Generation: int64(len(records)) + 1, Kind: "plan", Plan: &next})
	if _, err = p.s.db.Exec(`INSERT INTO verification_records VALUES(?,?,?,?,?,?,?)`, p.task.ID, current.ItemID, len(records)+1, "plan", api.NewID("req"), "synthetic-fixture", string(b)); err != nil {
		t.Fatal(err)
	}
	got, err := p.accept(t, "unreceipted", candidateC, candidateB)
	if err != nil {
		t.Fatal("acceptance on an unreceipted plan", err)
	}
	if got.BaseCommit != candidateA || got.Acceptance == nil || got.Acceptance.BaseCommit != candidateA || got.Release != nil {
		t.Fatalf("unreceipted plan acceptance: base %s, %+v, release %+v", got.BaseCommit, got.Acceptance, got.Release)
	}
}

// With no plan, a legacy item keeps its queue entry's base and no release.
func TestQueueAcceptanceOfLegacyItemWithoutPlanKeepsEntryBase(t *testing.T) {
	p := newPlanAcceptance(t, "", false)
	got, err := p.accept(t, "legacy", candidateC, candidateB)
	if err != nil {
		t.Fatal("legacy acceptance", err)
	}
	if got.BaseCommit != candidateC || got.Acceptance == nil || got.Acceptance.BaseCommit != candidateC || got.Release != nil {
		t.Fatalf("legacy acceptance: base %s, %+v, release %+v", got.BaseCommit, got.Acceptance, got.Release)
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

// wi_8afe404c4867dafc p5a: a failed push may carry typed, bounded
// diagnostics and nothing else; a pushed one only its elapsed time; a push
// with only remote, commit and outcome keeps validating; a released finish
// saves the diagnostics as sent.
func TestReleasePushDiagnosticsValidation(t *testing.T) {
	code, zero, big, signal := 128, int64(0), int64(releaseProbeWaitMaxMs), "sigterm"
	full := func() *api.ReleasePush {
		return &api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "failed", TimedOut: true, ExitCode: &code, ElapsedMs: &big, Reason: "push-timed-out", RemoteCheck: "absent", StderrSHA256: strings.Repeat("e", 64)}
	}
	cases := []struct {
		name    string
		outcome string
		edit    func(p *api.ReleasePush)
		ok      bool
	}{
		{"pushed bare", "released", func(p *api.ReleasePush) {
			*p = api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "pushed"}
		}, true},
		{"failed bare", "released", func(p *api.ReleasePush) {
			*p = api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "failed"}
		}, true},
		{"all fields", "released", func(p *api.ReleasePush) {}, true},
		{"timed out alone", "released", func(p *api.ReleasePush) {
			*p = api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "failed", TimedOut: true}
		}, true},
		{"exit code alone", "released", func(p *api.ReleasePush) {
			*p = api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "failed", ExitCode: &code}
		}, true},
		{"signal alone", "released", func(p *api.ReleasePush) { p.ExitCode = nil; p.Signal = signal }, true},
		{"elapsed zero", "released", func(p *api.ReleasePush) { p.ElapsedMs = &zero }, true},
		{"each remote check", "released", func(p *api.ReleasePush) { p.RemoteCheck = "landed" }, true},
		{"remote check unknown", "released", func(p *api.ReleasePush) { p.RemoteCheck = "unknown" }, true},
		{"pushed with elapsed", "released", func(p *api.ReleasePush) {
			*p = api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "pushed", ElapsedMs: &zero}
		}, true},
		{"on rollback", "rolled_back", func(p *api.ReleasePush) {}, false},
		{"on block", "blocked", func(p *api.ReleasePush) {}, false},
		{"outcome timed_out", "released", func(p *api.ReleasePush) { p.Outcome = "timed_out" }, false},
		{"pushed with timeout", "released", func(p *api.ReleasePush) {
			*p = api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "pushed", TimedOut: true}
		}, false},
		{"pushed with reason", "released", func(p *api.ReleasePush) {
			*p = api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "pushed", Reason: "push-failed"}
		}, false},
		{"pushed with remote check", "released", func(p *api.ReleasePush) {
			*p = api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "pushed", RemoteCheck: "landed"}
		}, false},
		{"pushed with stderr hash", "released", func(p *api.ReleasePush) {
			*p = api.ReleasePush{Remote: "origin", Commit: candidateB, Outcome: "pushed", StderrSHA256: strings.Repeat("e", 64)}
		}, false},
		{"exit code zero", "released", func(p *api.ReleasePush) { c := 0; p.ExitCode = &c }, false},
		{"exit code 256", "released", func(p *api.ReleasePush) { c := 256; p.ExitCode = &c }, false},
		{"exit code and signal", "released", func(p *api.ReleasePush) { p.Signal = signal }, false},
		{"signal upper", "released", func(p *api.ReleasePush) { p.ExitCode = nil; p.Signal = "SIGTERM" }, false},
		{"signal multi-line", "released", func(p *api.ReleasePush) { p.ExitCode = nil; p.Signal = "sigterm\nfatal" }, false},
		{"elapsed negative", "released", func(p *api.ReleasePush) { v := int64(-1); p.ElapsedMs = &v }, false},
		{"elapsed above bound", "released", func(p *api.ReleasePush) { v := big + 1; p.ElapsedMs = &v }, false},
		{"reason secret word", "released", func(p *api.ReleasePush) { p.Reason = "push token=abc" }, false},
		{"reason outside pattern", "released", func(p *api.ReleasePush) { p.Reason = "push@failed" }, false},
		{"reason multi-line", "released", func(p *api.ReleasePush) { p.Reason = "push-failed\nfatal: unable to access" }, false},
		{"reason too long", "released", func(p *api.ReleasePush) { p.Reason = strings.Repeat("a", 161) }, false},
		{"remote check other", "released", func(p *api.ReleasePush) { p.RemoteCheck = "maybe" }, false},
		{"remote check multi-line", "released", func(p *api.ReleasePush) { p.RemoteCheck = "landed\n" }, false},
		{"stderr hash short", "released", func(p *api.ReleasePush) { p.StderrSHA256 = strings.Repeat("e", 63) }, false},
		{"stderr hash text", "released", func(p *api.ReleasePush) { p.StderrSHA256 = "fatal: unable to access\n" + strings.Repeat("e", 40) }, false},
	}
	for _, c := range cases {
		p := full()
		c.edit(p)
		r := &api.ReleaseReceipt{Version: 1, Commit: candidateB, Outcome: c.outcome, Push: p}
		if err := validateReleaseRefEffects(r); (err == nil) != c.ok {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	// Round trip through finish: a released receipt with every push field is
	// saved and read back as sent; a malformed one is refused.
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
	push := full()
	push.Commit = j.Commit
	receipt := &api.ReleaseReceipt{Version: 1, JobID: j.ID, Commit: j.Commit, VerificationDigest: j.VerificationDigest, Outcome: "released", Targets: []api.ReleaseTargetReceipt{{Target: "tailos", Release: "fixture", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "released"}}, Push: push}
	bad := *receipt
	badPush := *push
	badPush.Reason = "push-failed\nBearer abc"
	bad.Push = &badPush
	if _, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "finish-bad", Operation: "finish", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Receipt: &bad}); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("malformed push", err)
	}
	final, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "finish", Operation: "finish", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Receipt: receipt})
	if err != nil || final.State != "released" || final.Receipt == nil || !reflect.DeepEqual(final.Receipt.Push, push) {
		t.Fatal(final.Receipt, err)
	}
	got, err := s.Release(ctx, task.ID, j.ID)
	if err != nil || !reflect.DeepEqual(got.Receipt.Push, push) {
		t.Fatal(got.Receipt, err)
	}
}

// wi_d4e96831ab3a0038 r1-r4: a failed receipt keeps its failure, the target
// reason and the probe waits (0 kept); a summary shows them; secret-looking or
// malformed diagnostics are refused; an old-shape receipt keeps its exact
// request JSON and replays; changed diagnostics under one request id are a
// changed retry.
func TestReleaseReceiptFailureDiagnostics(t *testing.T) {
	ctx := context.Background()
	ms := func(v int64) *int64 { return &v }
	merged := func(t *testing.T) (*Store, string, api.Agent, api.ReleaseJob, func(string, *api.ReleaseReceipt) (api.ReleaseJob, error)) {
		s, task, h, d, entry := releaseFixture(t)
		j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range []string{"claim", "merged"} {
			if j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit}); err != nil {
				t.Fatal(op, err)
			}
		}
		generation := j.Generation
		finish := func(id string, r *api.ReleaseReceipt) (api.ReleaseJob, error) {
			return s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: id, Operation: "finish", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: generation, Receipt: r})
		}
		return s, task.ID, d, j, finish
	}
	s, task, _, j, finish := merged(t)
	tailos := func() api.ReleaseTargetReceipt {
		return api.ReleaseTargetReceipt{Target: "tailos", Release: "tailos-" + j.Commit[:12], ArtifactSHA256: strings.Repeat("a", 64), Outcome: "rolled_back", Rollback: "restored"}
	}
	valid := func() *api.ReleaseReceipt {
		target := tailos()
		target.Reason = "live verification failed"
		target.LiveCheck = &api.ReleaseProbeWait{LastCommit: strings.Repeat("c", 40), WaitedMs: ms(90000)}
		target.RollbackCheck = &api.ReleaseProbeWait{WaitedMs: ms(0)}
		return &api.ReleaseReceipt{Version: 1, JobID: j.ID, Commit: j.Commit, VerificationDigest: j.VerificationDigest, Outcome: "rolled_back", Targets: []api.ReleaseTargetReceipt{target},
			Revert: &api.ReleaseRevert{Outcome: "failed"}, Failure: &api.ReleaseFailure{Step: "live-check", Target: "tailos", Reason: "live verification failed"}}
	}
	// a3: each is refused and leaves the job merged and unreceipted. Fake values only.
	bad := map[string]func(*api.ReleaseReceipt){
		"token word": func(r *api.ReleaseReceipt) {
			r.Failure.Reason, r.Targets[0].Reason = "SYNTHETIC_PRIVATE_TOKEN", "SYNTHETIC_PRIVATE_TOKEN"
		},
		"bearer": func(r *api.ReleaseReceipt) {
			r.Failure.Reason, r.Targets[0].Reason = "Bearer SYNTHETIC_PRIVATE_VALUE", ""
		},
		"password": func(r *api.ReleaseReceipt) {
			r.Failure.Reason, r.Targets[0].Reason = "password: SYNTHETIC_PRIVATE_VALUE", ""
		},
		"equals sign":             func(r *api.ReleaseReceipt) { r.Failure.Reason, r.Targets[0].Reason = "key=SYNTHETIC_PRIVATE_VALUE", "" },
		"newline":                 func(r *api.ReleaseReceipt) { r.Failure.Reason, r.Targets[0].Reason = "live\nverification failed", "" },
		"161 characters":          func(r *api.ReleaseReceipt) { r.Failure.Reason, r.Targets[0].Reason = strings.Repeat("a", 161), "" },
		"target reason only":      func(r *api.ReleaseReceipt) { r.Failure = nil },
		"target reason differs":   func(r *api.ReleaseReceipt) { r.Targets[0].Reason = "live probe failed" },
		"target reason elsewhere": func(r *api.ReleaseReceipt) { r.Failure.Target = "" },
		"failure on release": func(r *api.ReleaseReceipt) {
			r.Outcome, r.Revert, r.Targets[0].Outcome, r.Targets[0].Rollback, r.Targets[0].Reason = "released", nil, "released", "", ""
		},
		"step Deploy!":             func(r *api.ReleaseReceipt) { r.Failure.Step = "Deploy!" },
		"target other":             func(r *api.ReleaseReceipt) { r.Failure.Target, r.Targets[0].Reason = "other", "" },
		"lastCommit secret":        func(r *api.ReleaseReceipt) { r.Targets[0].LiveCheck.LastCommit = "SYNTHETIC_PRIVATE_TOKEN" },
		"waitedMs -1":              func(r *api.ReleaseReceipt) { r.Targets[0].LiveCheck.WaitedMs = ms(-1) },
		"waitedMs 3600001":         func(r *api.ReleaseReceipt) { r.Targets[0].RollbackCheck.WaitedMs = ms(3600001) },
		"lastCommit uppercase hex": func(r *api.ReleaseReceipt) { r.Targets[0].LiveCheck.LastCommit = strings.Repeat("C", 40) },
	}
	for name, mutate := range bad {
		r := valid()
		mutate(r)
		if _, err := finish("finish-bad-"+strings.ReplaceAll(name, " ", "-"), r); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
		if now, err := s.Release(ctx, task, j.ID); err != nil || now.State != "merged" || now.Receipt != nil || now.Generation != j.Generation {
			t.Fatalf("%s left %s receipt=%v generation=%d: %v", name, now.State, now.Receipt, now.Generation, err)
		}
	}
	// a1: the valid receipt is saved exactly, 0 included, and replays.
	sent := valid()
	final, err := finish("finish", sent)
	if err != nil || final.State != "rolled_back" {
		t.Fatal(final.State, err)
	}
	saved, err := s.Release(ctx, task, j.ID)
	if err != nil || !reflect.DeepEqual(saved.Receipt, valid()) || saved.Receipt.Targets[0].RollbackCheck.WaitedMs == nil || *saved.Receipt.Targets[0].RollbackCheck.WaitedMs != 0 {
		t.Fatalf("saved receipt %+v: %v", saved.Receipt, err)
	}
	if again, err := finish("finish", valid()); err != nil || again.Generation != final.Generation || !reflect.DeepEqual(again.Receipt, valid()) {
		t.Fatal("replay", again.Generation, err)
	}
	// a7: the same request id with changed diagnostics is a changed retry.
	changed := valid()
	changed.Targets[0].LiveCheck.WaitedMs = ms(90001)
	if _, err := finish("finish", changed); err == nil || !strings.Contains(err.Error(), "retry changed") {
		t.Fatal("changed diagnostics replay", err)
	}
	// a2: the settled summary, what tt deployment list prints, carries them.
	summary := func(s *Store, task, id string) (api.ReleaseSummary, []byte) {
		page, err := s.ReleasesPage(ctx, task, api.ReleaseListOptions{View: "settled", Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range page.Jobs {
			if job.ID == id {
				b, err := json.Marshal(job)
				if err != nil {
					t.Fatal(err)
				}
				return job, b
			}
		}
		t.Fatal("no settled summary for", id)
		return api.ReleaseSummary{}, nil
	}
	got, _ := summary(s, task, j.ID)
	want := valid()
	if got.Receipt == nil || !reflect.DeepEqual(got.Receipt.Failure, want.Failure) || len(got.Receipt.Targets) != 1 || got.Receipt.Targets[0].Reason != want.Targets[0].Reason || !reflect.DeepEqual(got.Receipt.Targets[0].LiveCheck, want.Targets[0].LiveCheck) || !reflect.DeepEqual(got.Receipt.Targets[0].RollbackCheck, want.Targets[0].RollbackCheck) {
		t.Fatalf("settled summary %+v", got.Receipt)
	}
	// a5: an old-shape receipt (no new keys) finishes with unchanged request
	// JSON, so its request hash is the one an older hub stored, and replays.
	s2, task2, d2, j2, finish2 := merged(t)
	old := &api.ReleaseReceipt{Version: 1, JobID: j2.ID, Commit: j2.Commit, VerificationDigest: j2.VerificationDigest, Outcome: "rolled_back", Revert: &api.ReleaseRevert{Outcome: "failed"},
		Targets: []api.ReleaseTargetReceipt{{Target: "tailos", Release: "tailos-" + j2.Commit[:12], ArtifactSHA256: strings.Repeat("a", 64), Outcome: "rolled_back", Rollback: "restored"}}}
	req, err := json.Marshal(api.ReleaseRequest{RequestID: "finish", Operation: "finish", AgentID: d2.ID, RunID: d2.RunID, JobID: j2.ID, ExpectedGeneration: j2.Generation, Receipt: old})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"failure"`, `"reason"`, `"liveCheck"`, `"rollbackCheck"`, `"waitedMs"`, `"lastCommit"`} {
		if strings.Contains(string(req), key) {
			t.Fatalf("old-shape request carries %s: %s", key, req)
		}
	}
	first, err := finish2("finish", old)
	if err != nil || first.State != "rolled_back" {
		t.Fatal(first.State, err)
	}
	if again, err := finish2("finish", old); err != nil || again.Generation != first.Generation || !reflect.DeepEqual(again.Receipt, old) {
		t.Fatal("old-shape replay", err)
	}
	if _, b := summary(s2, task2, j2.ID); strings.Contains(string(b), `"failure"`) || strings.Contains(string(b), `"reason"`) || strings.Contains(string(b), `"liveCheck"`) || strings.Contains(string(b), `"rollbackCheck"`) {
		t.Fatalf("old-shape summary carries new keys: %s", b)
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
	return recordHandReleaseOf(t, s, task, key, released, commits, "hub")
}

// recordHandReleaseOf is recordHandRelease naming these targets.
func recordHandReleaseOf(t *testing.T, s *Store, task api.Task, key, released string, commits []string, targets ...string) api.HandRelease {
	t.Helper()
	seq := releaseIntervention(t, s, task, key, "release")
	h, err := s.RecordHandRelease(context.Background(), task.ID, api.ReleaseRequest{RequestID: "hand-release-" + key, HandRelease: &api.HandRelease{InterventionSeq: seq, ReleasedCommit: released, Release: "20260930-" + key, Targets: targets, Commits: commits}}, api.Caller{Node: "fixture", User: "owner"})
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
	t.Run("unclaimed only", func(t *testing.T) {
		f := setup(t)
		again, err := f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: "reclaim", Operation: "claim", AgentID: f.d.ID, RunID: f.d.RunID, JobID: f.aside.ID, ExpectedGeneration: f.aside.Generation})
		if err != nil {
			t.Fatal(err)
		}
		freed, err := f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: "unclaim", Operation: "unclaim", AgentID: f.d.ID, RunID: f.d.RunID, JobID: again.ID, ExpectedGeneration: again.Generation})
		if err != nil || freed.State != "verified" || len(freed.Reconciliations) != 2 || freed.Reconciliations[1].Disposition != "unclaimed" {
			t.Fatalf("%+v %v", freed, err)
		}
		done, err := supersede(t, f, freed)
		if err != nil || done.State != "superseded" {
			t.Fatalf("%+v %v", done, err)
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

func TestReleaseRecoveryPreventionScopeAfterSave(t *testing.T) {
	for _, operation := range []string{"set-aside", "reconcile"} {
		for _, state := range []string{"done", "changed scope", "dismissed"} {
			t.Run(operation+"/"+state, func(t *testing.T) {
				s, task, h, d, entry := releaseFixture(t)
				ctx := context.Background()
				j := claimGoRaceJob(t, s, task, h, d, entry, goRaceMatrix(goRace(raceFlags, "./cmd/tt")))
				evidence := recoveryEvidence(t, s, task, h, j)
				if operation == "set-aside" {
					if _, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "later-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: releaseEntry(t, s, task, "waiting fix", "later")}); err != nil {
						t.Fatal(err)
					}
					evidence.Disposition = "set_aside"
				} else if _, err := s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, d.ID); err != nil {
					t.Fatal(err)
				}
				item, err := s.GetWorkItem(ctx, task.ID, evidence.PreventionItemID)
				if err != nil {
					t.Fatal(err)
				}
				confirmation, err := s.GetWorkOrderScopeConfirmation(ctx, task.ID, item.ID, 1, evidence.PreventionOrderMessage)
				if err != nil {
					t.Fatal(err)
				}
				update := api.UpdateWorkItemRequest{Revision: item.Revision}
				if state == "changed scope" {
					description := item.Description + "; expanded prevention scope"
					update.Description = &description
				} else {
					update.Status = &state
					if state == "done" {
						seedPassingVerification(t, s, item, candidateB)
					}
				}
				updated, err := s.UpdateWorkItem(ctx, task.ID, item.ID, update, api.Caller{Node: "fixture", User: "owner"})
				if err != nil || updated.Revision != 2 {
					t.Fatal("prevention save", updated.Revision, err)
				}
				if (updated.ScopeRevision != confirmation.ScopeRevision) != (state == "changed scope") {
					t.Fatal("unexpected narrative scope", updated.ScopeRevision)
				}
				out, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "recover", Operation: operation, AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Reconciliation: &evidence})
				if state == "done" {
					if err != nil || out.State != "verified" || out.Generation != j.Generation+1 || len(out.Reconciliations) != 1 {
						t.Fatal("finished prevention refused", out.State, err)
					}
				} else {
					if !errors.Is(err, api.ErrConflict) || (state == "changed scope" && !strings.Contains(err.Error(), "scope confirmation is stale")) {
						t.Fatal("expected prevention refusal", err)
					}
					saved, loadErr := releaseLoad(ctx, s.db, task.ID, j.ID)
					if loadErr != nil || saved.State != j.State || saved.Generation != j.Generation || len(saved.Reconciliations) != 0 {
						t.Fatal("refusal changed the job", saved.State, loadErr)
					}
				}
				retained, err := s.GetWorkOrderScopeConfirmation(ctx, task.ID, item.ID, 1, evidence.PreventionOrderMessage)
				if err != nil || verificationDigest(retained) != verificationDigest(confirmation) {
					t.Fatal("original confirmation changed", err)
				}
				if _, err := s.GetWorkOrderScopeConfirmation(ctx, task.ID, item.ID, 2, evidence.PreventionOrderMessage); !errors.Is(err, api.ErrNotFound) {
					t.Fatal("recovery created a new confirmation", err)
				}
			})
		}
	}
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

// A clean stop gives back the exact run's own claim when it has no effects:
// the job is verified again in its queue place, with an unclaimed record and
// no incident or prevention item, and is claimed again first.
func TestReleaseUnclaimReturnsNoEffectClaimToVerified(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	a := claimGoRaceJob(t, s, task, h, d, entry, goRaceMatrix(goRace(raceFlags, "./cmd/tt")))
	b, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "b-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: releaseEntry(t, s, task, "queued behind", "b")})
	if err != nil {
		t.Fatal(err)
	}
	req := api.ReleaseRequest{RequestID: a.ID + "-unclaim-" + fmt.Sprint(a.Generation), Operation: "unclaim", AgentID: d.ID, RunID: d.RunID, JobID: a.ID, ExpectedGeneration: a.Generation}
	freed, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if freed.State != "verified" || freed.Generation != a.Generation+1 || freed.AgentID != "" || freed.RunID != "" || freed.IntegratedCommit != "" || freed.IntegratedPlan != nil || freed.IntegratedCoverage != nil || freed.IntegratedMatrix != nil || freed.IntegratedVerification != nil || freed.InputsCommit != "" || freed.InputsDigest != "" || freed.Commit != a.Commit || freed.VerificationDigest != a.VerificationDigest {
		t.Fatalf("unclaimed %+v", freed)
	}
	if n := len(freed.Reconciliations); n != 1 {
		t.Fatalf("history %+v", freed.Reconciliations)
	}
	r := freed.Reconciliations[0]
	if r.Disposition != "unclaimed" || r.JobID != a.ID || r.AgentID != d.ID || r.RunID != d.RunID || r.PauseGeneration != a.PauseGeneration || r.JournalState != "no_effects" || !r.NoPublication || r.StopReason == "" || r.ObservedAt == "" || r.StoppedAt == "" {
		t.Fatalf("record %+v", r)
	}
	if r.IncidentBugID != "" || r.IncidentDigest != "" || r.PreventionItemID != "" || r.PreventionOrderMessage != 0 || r.PreventionOwner != "" || r.PreventionCriterion != "" || r.JournalDigest != "" || r.LockDigest != "" {
		t.Fatalf("unclaim carries incident or prevention fields %+v", r)
	}
	jobs, err := s.Releases(ctx, task.ID)
	if err != nil || len(jobs) != 2 || jobs[0].ID != a.ID || jobs[1].ID != b.ID {
		t.Fatal("queue place", jobs, err)
	}
	again, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil || again.Generation != freed.Generation || again.State != "verified" || len(again.Reconciliations) != 1 {
		t.Fatal("replay", again.Generation, err)
	}
	mutated := req
	mutated.ExpectedGeneration = freed.Generation
	if _, err = s.ReleaseAction(ctx, task.ID, mutated); !errors.Is(err, api.ErrConflict) {
		t.Fatal("changed retry replayed", err)
	}
	if saved, err := releaseLoad(ctx, s.db, task.ID, a.ID); err != nil || saved.Generation != freed.Generation || saved.State != "verified" {
		t.Fatal("changed retry wrote the job", saved.State, err)
	}
	reclaim, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "reclaim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: a.ID, ExpectedGeneration: freed.Generation})
	if err != nil || reclaim.State != "claimed" || reclaim.RunID != d.RunID || len(reclaim.Reconciliations) != 1 {
		t.Fatalf("reclaim %+v %v", reclaim, err)
	}
}

// Only the exact run's claimed job with no effects, outside a batch and on a
// bare request, is unclaimed; every refusal leaves the job as it was.
func TestReleaseUnclaimKeepsFenceForJobsWithEffects(t *testing.T) {
	ctx := context.Background()
	type fixture struct {
		s    *Store
		task api.Task
		h, d api.Agent
		a, b api.ReleaseJob
	}
	setup := func(t *testing.T) fixture {
		s, task, h, d, entry := releaseFixture(t)
		a, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "a-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
		if err != nil {
			t.Fatal(err)
		}
		if a, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "a-claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: a.ID, ExpectedGeneration: a.Generation}); err != nil {
			t.Fatal(err)
		}
		b, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "b-enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: releaseEntry(t, s, task, "queued behind", "b")})
		if err != nil {
			t.Fatal(err)
		}
		return fixture{s, task, h, d, a, b}
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
	// refused asserts the unclaim is refused and the job's row is unchanged.
	refused := func(t *testing.T, s *Store, task api.Task, d api.Agent, j api.ReleaseJob, change func(*api.ReleaseRequest), want error) {
		t.Helper()
		req := api.ReleaseRequest{RequestID: "unclaim", Operation: "unclaim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}
		if change != nil {
			change(&req)
		}
		row := releaseRow(t, s, task, j.ID)
		if _, err := s.ReleaseAction(ctx, task.ID, req); !errors.Is(err, want) {
			t.Fatalf("unclaim: want %v, got %v", want, err)
		}
		if got := releaseRow(t, s, task, j.ID); got != row {
			t.Fatalf("refused unclaim changed the job:\n%s\n%s", row, got)
		}
	}
	t.Run("inputs bound", func(t *testing.T) {
		f := setup(t)
		bound, err := f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: "a-inputs", Operation: "inputs", AgentID: f.h.ID, RunID: f.h.RunID, JobID: f.a.ID, ExpectedGeneration: f.a.Generation, IntegratedCommit: f.a.Commit, InputsDigest: strings.Repeat("e", 64)})
		if err != nil {
			t.Fatal(err)
		}
		refused(t, f.s, f.task, f.d, bound, nil, api.ErrConflict)
	})
	t.Run("merged", func(t *testing.T) {
		f := setup(t)
		refused(t, f.s, f.task, f.d, deployer(t, f, "merged", f.a, func(r *api.ReleaseRequest) { r.IntegratedCommit = f.a.Commit }), nil, api.ErrConflict)
	})
	t.Run("blocked", func(t *testing.T) {
		f := setup(t)
		refused(t, f.s, f.task, f.d, deployer(t, f, "block", f.a, nil), nil, api.ErrConflict)
	})
	t.Run("verified", func(t *testing.T) {
		f := setup(t)
		refused(t, f.s, f.task, f.d, f.b, nil, api.ErrConflict)
	})
	t.Run("batch lead", func(t *testing.T) {
		bf := newBatchFixture(t, 1)
		bf.declare(t)
		lead := bf.load(t, bf.lead.ID)
		row := releaseRow(t, bf.s, bf.task, lead.ID)
		_, err := bf.s.ReleaseAction(ctx, bf.task.ID, api.ReleaseRequest{RequestID: "unclaim", Operation: "unclaim", AgentID: bf.d.ID, RunID: bf.d.RunID, JobID: lead.ID, ExpectedGeneration: lead.Generation})
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "leads release batch") {
			t.Fatal("batch lead unclaimed", err)
		}
		if got := releaseRow(t, bf.s, bf.task, lead.ID); got != row {
			t.Fatalf("refused unclaim changed the lead:\n%s\n%s", row, got)
		}
		if b := bf.batch(t, lead.ID); b.State != "open" {
			t.Fatal("batch state", b.State)
		}
	})
	t.Run("another deployer run", func(t *testing.T) {
		f := setup(t)
		if _, err := f.s.db.Exec(`UPDATE agents SET run_id=? WHERE id=?`, api.NewID("run"), f.d.ID); err != nil {
			t.Fatal(err)
		}
		other := f.d
		if err := f.s.db.QueryRow(`SELECT run_id FROM agents WHERE id=?`, f.d.ID).Scan(&other.RunID); err != nil {
			t.Fatal(err)
		}
		refused(t, f.s, f.task, other, f.a, nil, api.ErrConflict)
	})
	t.Run("exited run", func(t *testing.T) {
		f := setup(t)
		if _, err := f.s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, f.d.ID); err != nil {
			t.Fatal(err)
		}
		refused(t, f.s, f.task, f.d, f.a, nil, api.ErrConflict)
	})
	t.Run("stale pause generation", func(t *testing.T) {
		f := setup(t)
		if _, err := f.s.db.Exec(`UPDATE tasks SET pause_generation=pause_generation+1 WHERE id=?`, f.task.ID); err != nil {
			t.Fatal(err)
		}
		refused(t, f.s, f.task, f.d, f.a, nil, api.ErrConflict)
	})
	for name, c := range map[string]struct {
		change func(fixture, *api.ReleaseRequest)
		want   error
	}{
		"handler caller":   {func(f fixture, r *api.ReleaseRequest) { r.AgentID, r.RunID = f.h.ID, f.h.RunID }, api.ErrConflict},
		"wrong generation": {func(_ fixture, r *api.ReleaseRequest) { r.ExpectedGeneration++ }, api.ErrConflict},
		"with reconciliation": {func(_ fixture, r *api.ReleaseRequest) {
			r.Reconciliation = &api.ReleaseReconciliation{JobID: r.JobID, Disposition: "unclaimed"}
		}, api.ErrInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			refused(t, f.s, f.task, f.d, f.a, func(r *api.ReleaseRequest) { c.change(f, r) }, c.want)
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

// wi_3aca3c7a3a869afb x3: once a retry follows a refused job, only the
// entry's latest job may be superseded; the refusal names the newer job and
// leaves the earlier one as it was.
func TestReleaseSupersedeRefusesJobWithNewerRetry(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	first := refusedJob(t, s, task, h, d, entry)
	second, err := s.ReleaseAction(ctx, task.ID, retryRequest("retry", h, entry, first))
	if err != nil {
		t.Fatal(err)
	}
	before := releaseRow(t, s, task, first.ID)
	hand := recordHandRelease(t, s, task, "hand", candidateB, []string{first.Commit})
	supersede := func(key string, j api.ReleaseJob) (api.ReleaseJob, error) {
		return s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: key, Operation: "supersede", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Supersession: &api.ReleaseSupersession{ReleasedCommit: candidateB, HandReleaseID: hand.ID}})
	}
	_, err = supersede("supersede-earlier", first)
	if !errors.Is(err, api.ErrConflict) || !strings.HasSuffix(err.Error(), "job "+first.ID+" is not its entry's latest release job; the latest is "+second.ID+" (verified)") {
		t.Fatal(err)
	}
	if releaseRow(t, s, task, first.ID) != before {
		t.Fatal("refused job changed")
	}
	done, err := supersede("supersede-latest", second)
	if err != nil || done.State != "superseded" || done.Supersession.HandReleaseID != hand.ID {
		t.Fatal(done, err)
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

// Real isolated SQLite ledger: history size cannot expand default responses,
// and every mutation anywhere in the ledger invalidates a continuation.
func TestReleasesBoundedPagesAndExactDetail(t *testing.T) {
	s, task, _, _, _ := releaseFixture(t)
	ctx := context.Background()
	var saved, savedActive api.ReleaseJob
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1031; i++ {
		state := "released"
		if i >= 810 {
			state = "verified"
		}
		j := api.ReleaseJob{ID: fmt.Sprintf("rel_%016x", i+1), TaskID: task.ID, EntryID: api.NewID("tqe"), ItemID: api.NewID("wi"), State: state, Generation: 1, Commit: candidateB, BaseCommit: candidateA, Repository: strings.Repeat("heavy", 4096), Plan: api.VerificationPlan{Commit: candidateB}, SettledAt: "2026-10-03T00:00:00.1Z", Receipt: &api.ReleaseReceipt{Version: 1, Commit: candidateB, Outcome: "released", Targets: []api.ReleaseTargetReceipt{{Target: "hub", Outcome: "released", Backup: strings.Repeat("heavy", 4096)}}}, RetryOf: &api.ReleaseRetry{Reason: "preserved"}, Reconciliations: []api.ReleaseReconciliation{{StopReason: "preserved"}}}
		raw, _ := json.Marshal(j)
		if _, err = tx.Exec(`INSERT INTO release_jobs VALUES(?,?,?,?,?,?)`, task.ID, j.ID, j.EntryID, j.State, j.Generation, string(raw)); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			saved = j
		}
		if i == 810 {
			savedActive = j
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	page, err := s.ReleasesPage(ctx, task.ID, api.ReleaseListOptions{})
	raw, _ := json.Marshal(page)
	if err != nil || len(page.Jobs) != 50 || len(raw) >= 128<<10 || page.Page.NextAfter == "" {
		t.Fatal(err, len(page.Jobs), len(raw))
	}
	if strings.Contains(string(raw), "heavy") || strings.Contains(string(raw), `"plan"`) || strings.Contains(string(raw), "reconciliations") {
		t.Fatal("heavy fields leaked")
	}
	t.Logf("default: 810 settled heavy records, %d active summaries, %d bytes", len(page.Jobs), len(raw))
	token := page.Page.Snapshot
	for _, view := range []string{"active", "settled"} {
		after := ""
		ids := map[string]bool{}
		previous := int64(0)
		pages := 0
		for {
			p, e := s.ReleasesPage(ctx, task.ID, api.ReleaseListOptions{View: view, Limit: 200, After: after, Snapshot: token})
			if e != nil {
				t.Fatal(e)
			}
			b, _ := json.Marshal(p)
			t.Logf("%s page%d: %d summaries, %d bytes", view, pages+1, len(p.Jobs), len(b))
			if len(b) >= 512<<10 || len(p.Jobs) > 200 {
				t.Fatal("unbounded", len(b))
			}
			pages++
			for _, j := range p.Jobs {
				if ids[j.ID] || j.RowID <= previous {
					t.Fatal("duplicate/order", j.ID)
				}
				ids[j.ID] = true
				previous = j.RowID
			}
			if p.Page.NextAfter == "" {
				break
			}
			if p.Page.NextAfter == after {
				t.Fatal("repeating cursor")
			}
			after = p.Page.NextAfter
		}
		want := 810
		if view == "active" {
			want = 221
		}
		if len(ids) != want || (view == "settled" && pages < 3) {
			t.Fatal(view, len(ids), pages)
		}
	}
	detail, err := s.Release(ctx, task.ID, saved.ID)
	a, _ := json.Marshal(detail)
	b, _ := json.Marshal(saved)
	if err != nil || string(a) != string(b) {
		t.Fatal("detail changed", err)
	}

	activeDetail, activeErr := s.Release(ctx, task.ID, savedActive.ID)
	a, _ = json.Marshal(activeDetail)
	b, _ = json.Marshal(savedActive)
	if activeErr != nil || string(a) != string(b) {
		t.Fatal("active detail changed", activeErr)
	}
	// Task and view binding, malformed tokens, and limits fail closed.
	for _, opts := range []api.ReleaseListOptions{{Limit: 201}, {Limit: -1}, {View: "all"}, {After: "bad", Snapshot: token}, {View: "settled", After: page.Page.NextAfter, Snapshot: token}} {
		if _, e := s.ReleasesPage(ctx, task.ID, opts); e == nil {
			t.Fatal("accepted invalid options", opts)
		}
	}
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "other"}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReleasesPage(ctx, other.ID, api.ReleaseListOptions{After: page.Page.NextAfter, Snapshot: token}); err == nil {
		t.Fatal("cross-task cursor")
	}

	// A row behind the active continuation settles concurrently, without changing
	// the total row count: continuation must not silently omit it from history.
	if _, err = s.db.Exec(`UPDATE release_jobs SET state='released',generation=generation+1 WHERE task_id=? AND id=?`, task.ID, savedActive.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReleasesPage(ctx, task.ID, api.ReleaseListOptions{After: page.Page.NextAfter, Snapshot: token}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("concurrent old-row settlement", err)
	}
	// An old row settles or a set-aside moves rowid: both change the token.
	if _, err = s.db.Exec(`UPDATE release_jobs SET generation=generation+1 WHERE task_id=? AND id=?`, task.ID, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReleasesPage(ctx, task.ID, api.ReleaseListOptions{After: page.Page.NextAfter, Snapshot: token}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("stale continuation", err)
	}
	if _, err = s.ReleasesPage(ctx, task.ID, api.ReleaseListOptions{View: "settled", Snapshot: token}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("cross-view stale snapshot", err)
	}
	fresh, err := s.ReleasesPage(ctx, task.ID, api.ReleaseListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE release_jobs SET rowid=rowid+2000 WHERE task_id=? AND id=?`, task.ID, saved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReleasesPage(ctx, task.ID, api.ReleaseListOptions{After: fresh.Page.NextAfter, Snapshot: fresh.Page.Snapshot}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("set-aside move stale", err)
	}
}

// batchFixture is a claimed lead job with later verified jobs queued behind
// it, all from one base under one matrix approval.
type batchFixture struct {
	s       *Store
	task    api.Task
	h, d    api.Agent
	lead    api.ReleaseJob
	members []api.ReleaseJob
	keys    int
}

var batchBase = strings.Repeat("0", 40)

// batchTo is the integrated tip after the batch's i-th job; the last job of
// a declared batch ends on candidateA, the commit integratedImport verifies.
func batchTo(i int) string { return strings.Repeat(fmt.Sprint(i), 40) }

func newBatchFixture(t *testing.T, members int) *batchFixture {
	t.Helper()
	s, task, h, d, entry := releaseFixture(t)
	f := &batchFixture{s: s, task: task, h: h, d: d}
	ctx := context.Background()
	enqueue := func(entry string) api.ReleaseJob {
		j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: f.key("enqueue"), Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	f.lead = enqueue(entry)
	for i := range members {
		f.members = append(f.members, enqueue(releaseEntry(t, s, task, fmt.Sprintf("batch member %d", i+1), fmt.Sprintf("member-%d", i+1))))
	}
	if _, err := f.deployer("claim", "", ""); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *batchFixture) key(op string) string {
	f.keys++
	return fmt.Sprintf("batch-%s-%d", op, f.keys)
}

// deployer runs one operation of the deployer's exact run on the lead.
func (f *batchFixture) deployer(op, entry, commit string) (api.ReleaseJob, error) {
	j, err := f.s.ReleaseAction(context.Background(), f.task.ID, api.ReleaseRequest{RequestID: f.key(op), Operation: op, AgentID: f.d.ID, RunID: f.d.RunID, JobID: f.lead.ID, ExpectedGeneration: f.lead.Generation, EntryID: entry, IntegratedCommit: commit})
	if err == nil {
		f.lead = j
	}
	return j, err
}

// declare opens the batch and adds the lead and every member in queue order.
func (f *batchFixture) declare(t *testing.T) ReleaseBatch {
	t.Helper()
	if _, err := f.deployer("batch-open", "", batchBase); err != nil {
		t.Fatal(err)
	}
	jobs := append([]api.ReleaseJob{f.lead}, f.members...)
	for i, j := range jobs {
		to := batchTo(i + 1)
		if i == len(jobs)-1 {
			to = candidateA
		}
		if _, err := f.deployer("batch-add", j.EntryID, to); err != nil {
			t.Fatal(i, err)
		}
	}
	return f.batch(t, f.lead.ID)
}
func (f *batchFixture) batch(t *testing.T, job string) ReleaseBatch {
	t.Helper()
	b, err := f.s.ReleaseBatchOf(context.Background(), f.task.ID, job)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// verify imports the one integrated verification, as the handler does.
func (f *batchFixture) verify(checks []api.VerificationCheck) error {
	if checks == nil {
		checks = f.lead.Plan.Checks
	}
	j, err := f.s.ReleaseAction(context.Background(), f.task.ID, integratedImport(f.lead, f.h, f.d, f.key("import"), checks))
	if err == nil {
		f.lead = j
	}
	return err
}

// publish imports the verification and records the lead as merged.
func (f *batchFixture) publish(t *testing.T) {
	t.Helper()
	if err := f.verify(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.deployer("merged", "", candidateA); err != nil {
		t.Fatal(err)
	}
}
func (f *batchFixture) finish(outcome string, revert *api.ReleaseRevert) (api.ReleaseJob, error) {
	target := api.ReleaseTargetReceipt{Target: "tailos", Release: "fixture-batch", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "released"}
	switch outcome {
	case "rolled_back":
		target.Outcome, target.Rollback = "rolled_back", "restored"
	case "blocked":
		target.Outcome = "failed"
	}
	receipt := &api.ReleaseReceipt{Version: 1, JobID: f.lead.ID, Commit: candidateA, VerificationDigest: f.lead.VerificationDigest, Outcome: outcome, Targets: []api.ReleaseTargetReceipt{target}, Revert: revert}
	if outcome == "released" {
		receipt.Push = &api.ReleasePush{Remote: "origin", Commit: candidateA, Outcome: "pushed"}
	}
	j, err := f.s.ReleaseAction(context.Background(), f.task.ID, api.ReleaseRequest{RequestID: f.key("finish"), Operation: "finish", AgentID: f.d.ID, RunID: f.d.RunID, JobID: f.lead.ID, ExpectedGeneration: f.lead.Generation, Receipt: receipt})
	if err == nil {
		f.lead = j
	}
	return j, err
}
func (f *batchFixture) load(t *testing.T, id string) api.ReleaseJob {
	t.Helper()
	j, err := releaseLoad(context.Background(), f.s.db, f.task.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func (f *batchFixture) rows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM release_batch_jobs WHERE task_id=?`, f.task.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// claim is the deployer's claim of any job, lead or not.
func (f *batchFixture) claim(j api.ReleaseJob) (api.ReleaseJob, error) {
	return f.s.ReleaseAction(context.Background(), f.task.ID, api.ReleaseRequest{RequestID: f.key("claim"), Operation: "claim", AgentID: f.d.ID, RunID: f.d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
}

// memberHeld requires that no operation reaches the member while its batch
// holds it, and that the member's row is what it was.
func (f *batchFixture) memberHeld(t *testing.T, m api.ReleaseJob, row string) {
	t.Helper()
	ctx := context.Background()
	evidence := api.ReleaseReconciliation{JobID: m.ID, Disposition: "set_aside"}
	for _, req := range []api.ReleaseRequest{
		{Operation: "claim", AgentID: f.d.ID, RunID: f.d.RunID},
		{Operation: "supersede", AgentID: f.h.ID, RunID: f.h.RunID, Supersession: &api.ReleaseSupersession{ReleasedCommit: candidateB, HandReleaseID: "hrl_fixture"}},
		{Operation: "set-aside", AgentID: f.h.ID, RunID: f.h.RunID, Reconciliation: &evidence},
		{Operation: "reconcile", AgentID: f.h.ID, RunID: f.h.RunID, Reconciliation: &evidence},
	} {
		req.RequestID, req.JobID, req.ExpectedGeneration = f.key("member-"+req.Operation), m.ID, m.Generation
		if _, err := f.s.ReleaseAction(ctx, f.task.ID, req); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is in release batch") {
			t.Fatalf("%s reached batch member %s: %v", req.Operation, m.ID, err)
		}
	}
	if got := releaseRow(t, f.s, f.task, m.ID); got != row {
		t.Fatalf("batch member %s changed:\n%s\n%s", m.ID, row, got)
	}
}

// exitDeployer marks the deployer's run exited, as a reconcile requires;
// resumeDeployer brings the same run back for later deployer operations.
func (f *batchFixture) exitDeployer(t *testing.T) {
	t.Helper()
	if _, err := f.s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, f.d.ID); err != nil {
		t.Fatal(err)
	}
}
func (f *batchFixture) resumeDeployer(t *testing.T) {
	t.Helper()
	if _, err := f.s.db.Exec(`UPDATE agents SET status='running',last_seen_at=? WHERE id=?`, ts(time.Now()), f.d.ID); err != nil {
		t.Fatal(err)
	}
}
func (f *batchFixture) reconcile(t *testing.T, change func(*api.ReleaseReconciliation)) (api.ReleaseJob, error) {
	t.Helper()
	evidence := recoveryEvidence(t, f.s, f.task, f.h, f.lead)
	change(&evidence)
	j, err := f.s.ReleaseAction(context.Background(), f.task.ID, api.ReleaseRequest{RequestID: f.key("reconcile"), Operation: "reconcile", AgentID: f.h.ID, RunID: f.h.RunID, JobID: f.lead.ID, ExpectedGeneration: f.lead.Generation, Reconciliation: &evidence})
	if err == nil {
		f.lead = j
	}
	return j, err
}

// rewriteJob is a test-only rewrite of a saved job snapshot.
func rewriteJob(t *testing.T, s *Store, j api.ReleaseJob, change func(*api.ReleaseJob)) api.ReleaseJob {
	t.Helper()
	change(&j)
	raw, _ := json.Marshal(j)
	if _, err := s.db.Exec(`UPDATE release_jobs SET record_json=? WHERE task_id=? AND id=?`, string(raw), j.TaskID, j.ID); err != nil {
		t.Fatal(err)
	}
	return j
}

// One integrated verification, imported once on the lead, releases every job
// of the batch; each keeps its own receipt and its own commit range.
func TestReleaseBatchOneVerificationSettlesEveryJob(t *testing.T) {
	f := newBatchFixture(t, 2)
	declared := f.declare(t)
	if declared.ID != releaseBatchID(f.lead.ID, declared.LeadGeneration, batchBase) || declared.State != "open" || declared.IntegratedCommit != candidateA || len(declared.Jobs) != 3 {
		t.Fatalf("declared %+v", declared)
	}
	f.publish(t)
	if b := f.batch(t, f.lead.ID); b.State != "published" || !b.Published {
		t.Fatalf("published %+v", b)
	}
	lead, err := f.finish("released", nil)
	if err != nil || lead.State != "released" {
		t.Fatal(lead.State, err)
	}
	want := verificationDigest(lead.IntegratedVerification)
	from := batchBase
	for i, id := range []string{f.lead.ID, f.members[0].ID, f.members[1].ID} {
		j := f.load(t, id)
		if j.State != "released" || !j.Published || j.IntegratedCommit != candidateA || j.IntegratedVerification == nil || verificationDigest(j.IntegratedVerification) != want || j.SettledAt != lead.SettledAt || j.AgentID != f.d.ID || j.RunID != f.d.RunID {
			t.Fatalf("job %d %+v", i, j)
		}
		if j.Receipt == nil || j.Receipt.JobID != id || j.Receipt.VerificationDigest != j.VerificationDigest || j.Receipt.Commit != candidateA || j.Receipt.Outcome != "released" || len(j.Receipt.Targets) != 1 || j.Receipt.Targets[0].Outcome != "released" || j.Receipt.Push == nil || j.Receipt.Push.Commit != candidateA {
			t.Fatalf("job %d receipt %+v", i, j.Receipt)
		}
		b := f.batch(t, id)
		if b.ID != declared.ID || b.State != "released" || b.SettledAt == "" || b.LeadJobID != f.lead.ID || b.BaseCommit != batchBase || b.IntegratedCommit != candidateA {
			t.Fatalf("batch of job %d %+v", i, b)
		}
		row := b.Jobs[i]
		if row.JobID != id || row.Seq != int64(i+1) || row.FromCommit != from || row.ToCommit == from || row.EntryID != j.EntryID {
			t.Fatalf("range of job %d %+v", i, row)
		}
		from = row.ToCommit
	}
	if from != candidateA {
		t.Fatal("ranges do not end on the integrated commit", from)
	}
	if f.members[0].VerificationDigest == f.members[1].VerificationDigest || f.members[0].VerificationDigest == f.lead.VerificationDigest {
		t.Fatal("fixture jobs share a verification digest")
	}
	if _, err = f.s.ReleaseBatchOf(context.Background(), f.task.ID, "rel_absent"); !errors.Is(err, api.ErrNotFound) {
		t.Fatal("batch of an unknown job", err)
	}

	// An import that omits one member's approved check is refused by name.
	f = newBatchFixture(t, 2)
	extra := append(slices.Clone(f.members[1].Plan.Checks), api.VerificationCheck{ID: "member-only", Argv: []string{"fixture", "member"}, Cwd: ".", Environment: map[string]string{}})
	approveChecks(t, f.s, f.members[1], extra)
	f.declare(t)
	before := f.lead.Generation
	if err = f.verify(nil); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "release batch job "+f.members[1].ID+": integrated matrix omitted approved check member-only") {
		t.Fatal("uncovered member", err)
	}
	if saved := f.load(t, f.lead.ID); saved.Generation != before || saved.IntegratedVerification != nil || saved.IntegratedPlan != nil {
		t.Fatal("refused import changed the lead", saved.Generation)
	}
	if err = f.verify(extra); err != nil {
		t.Fatal("covering import", err)
	}
	// An import of another commit than the batch's is refused.
	f = newBatchFixture(t, 1)
	f.declare(t)
	other := integratedImport(f.lead, f.h, f.d, "batch-other-commit", f.lead.Plan.Checks)
	other.Plan.Commit, other.IntegratedCommit = candidateB, candidateB
	receipt := passingVerification(*other.Plan)
	other.Verification = &receipt
	if _, err = f.s.ReleaseAction(context.Background(), f.task.ID, other); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "requires the verification of its integrated commit") {
		t.Fatal("other commit", err)
	}
}

// A docs-only member needs every package and a code member needs its own:
// only the merged plan covers both, and each job records its own coverage.
func TestReleaseBatchHeterogeneousPlansNeedTheMergedPlan(t *testing.T) {
	f := newBatchFixture(t, 2)
	leadChecks := goRaceMatrix(goRace(raceFlags, "./cmd/tt"))
	docs := goRaceMatrix(goRace(raceFlags, "./..."))
	code := goRaceMatrix(goRace(raceFlags, "./internal/store"))
	f.lead = approveChecks(t, f.s, f.lead, leadChecks)
	approveChecks(t, f.s, f.members[0], docs)
	approveChecks(t, f.s, f.members[1], code)
	f.declare(t)
	if err := f.verify(leadChecks); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "release batch job "+f.members[0].ID+": integrated matrix omitted approved check go-race") {
		t.Fatal("lead plan alone", err)
	}
	if err := f.verify(goRaceMatrix(goRace(raceFlags, "./cmd/tt", "./internal/store"))); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), f.members[0].ID) {
		t.Fatal("package union without ./...", err)
	}
	if err := f.verify(docs); err != nil {
		t.Fatal("merged plan", err)
	}
	if _, err := f.deployer("merged", "", candidateA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.finish("released", nil); err != nil {
		t.Fatal(err)
	}
	superset := func(approved []api.VerificationCheck) []api.ReleaseCheckCoverage {
		return []api.ReleaseCheckCoverage{{CheckID: "go-race", ApprovedDigest: verificationDigest(approved[71]), IntegratedDigest: verificationDigest(docs[71]), Relation: "superset"}}
	}
	if got := f.load(t, f.lead.ID).IntegratedCoverage; !slices.Equal(got, superset(leadChecks)) {
		t.Fatalf("lead coverage %+v", got)
	}
	if got := f.load(t, f.members[0].ID); got.State != "released" || got.IntegratedCoverage != nil {
		t.Fatalf("docs member %s %+v", got.State, got.IntegratedCoverage)
	}
	if got := f.load(t, f.members[1].ID); got.State != "released" || !slices.Equal(got.IntegratedCoverage, superset(code)) {
		t.Fatalf("code member %s %+v", got.State, got.IntegratedCoverage)
	}
}

// Until the lead's finish a member is the row it was when it joined, no
// operation reaches it, and the lead alone holds the project fence.
func TestReleaseBatchMembersAdvanceOnlyAtFinish(t *testing.T) {
	f := newBatchFixture(t, 3)
	outside := f.members[2]
	f.members = f.members[:2]
	rows := []string{releaseRow(t, f.s, f.task, f.members[0].ID), releaseRow(t, f.s, f.task, f.members[1].ID)}
	held := func(stage string) {
		t.Helper()
		for i, m := range f.members {
			f.memberHeld(t, m, rows[i])
		}
		if _, err := f.claim(outside); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "project already fenced") {
			t.Fatal(stage, "the fence allowed a second claim", err)
		}
	}
	f.declare(t)
	held("declared")
	if err := f.verify(nil); err != nil {
		t.Fatal(err)
	}
	held("verified")
	if _, err := f.deployer("merged", "", candidateA); err != nil {
		t.Fatal(err)
	}
	held("merged")
	lead, err := f.finish("released", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range f.members {
		got := f.load(t, m.ID)
		if got.State != "released" || got.Generation != m.Generation+1 || got.SettledAt != lead.SettledAt || got.SettledAt == "" {
			t.Fatalf("member %d %s generation %d settled %q", i, got.State, got.Generation, got.SettledAt)
		}
		if _, err = f.claim(got); !errors.Is(err, api.ErrConflict) {
			t.Fatal("released member claimed", err)
		}
	}
	if got := f.load(t, outside.ID); got.State != "verified" || got.Generation != outside.Generation {
		t.Fatal("job outside the batch changed", got.State)
	}
	if claimed, err := f.claim(outside); err != nil || claimed.State != "claimed" {
		t.Fatal("next job after the batch", err)
	}
}

// A dropped batch and a rolled-back batch whose revert is committed leave
// every member the verified row it was, free for its own release.
func TestReleaseBatchDropAndRollbackLeaveMembersVerified(t *testing.T) {
	ctx := context.Background()
	untouched := func(f *batchFixture, rows []string) {
		t.Helper()
		for i, m := range f.members {
			if got := releaseRow(t, f.s, f.task, m.ID); got != rows[i] {
				t.Fatalf("member %d changed:\n%s\n%s", i, rows[i], got)
			}
		}
	}
	start := func(n int) (*batchFixture, []string) {
		t.Helper()
		f := newBatchFixture(t, n)
		var rows []string
		for _, m := range f.members {
			rows = append(rows, releaseRow(t, f.s, f.task, m.ID))
		}
		return f, rows
	}

	// batch-drop: dissolved, the lead clean and able to verify alone.
	f, rows := start(2)
	f.declare(t)
	dropped, err := f.deployer("batch-drop", "", "")
	if err != nil || dropped.State != "claimed" || dropped.IntegratedCommit != "" || dropped.IntegratedPlan != nil || dropped.IntegratedCoverage != nil || dropped.IntegratedMatrix != nil || dropped.IntegratedVerification != nil {
		t.Fatalf("drop %+v %v", dropped, err)
	}
	if b := f.batch(t, f.members[0].ID); b.State != "dissolved" || b.SettledAt == "" || len(b.Jobs) != 3 {
		t.Fatalf("dropped batch %+v", b)
	}
	untouched(f, rows)
	if _, err = f.deployer("batch-drop", "", ""); !errors.Is(err, api.ErrConflict) {
		t.Fatal("second drop", err)
	}
	if err = f.verify(nil); err != nil {
		t.Fatal("lead alone after the drop", err)
	}
	if _, err = f.deployer("merged", "", candidateA); err != nil {
		t.Fatal(err)
	}
	if _, err = f.finish("released", nil); err != nil {
		t.Fatal(err)
	}
	untouched(f, rows)
	if claimed, cerr := f.claim(f.members[0]); cerr != nil || claimed.State != "claimed" {
		t.Fatal("member after a dropped batch", cerr)
	}

	// A drop is refused once the import is saved: the batch continues.
	f, rows = start(1)
	f.declare(t)
	if err = f.verify(nil); err != nil {
		t.Fatal(err)
	}
	if _, err = f.deployer("batch-drop", "", ""); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "integrated verification is saved") {
		t.Fatal("drop after import", err)
	}
	if b := f.batch(t, f.lead.ID); b.State != "open" {
		t.Fatal(b.State)
	}

	// Rolled back with the tasks-hub revert committed: members are free.
	f, rows = start(2)
	f.declare(t)
	f.publish(t)
	lead, err := f.finish("rolled_back", &api.ReleaseRevert{Outcome: "committed", Commit: candidateB})
	if err != nil || lead.State != "rolled_back" {
		t.Fatal(lead.State, err)
	}
	if b := f.batch(t, f.lead.ID); b.State != "rolled_back" || b.SettledAt == "" {
		t.Fatalf("rolled back batch %+v", b)
	}
	untouched(f, rows)
	if claimed, cerr := f.claim(f.members[0]); cerr != nil || claimed.State != "claimed" {
		t.Fatal("member after a rolled-back batch", cerr)
	}

	// The lead leaving the fence unpublished dissolves the batch.
	f, rows = start(1)
	f.declare(t)
	if _, err = f.deployer("refuse", "", ""); err != nil {
		t.Fatal(err)
	}
	if b := f.batch(t, f.lead.ID); b.State != "dissolved" {
		t.Fatal("refuse", b.State)
	}
	untouched(f, rows)
	f, rows = start(1)
	f.declare(t)
	evidence := setAsideEvidence(t, f.s, f.task, f.h, f.lead)
	if _, err = f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: "batch-set-aside", Operation: "set-aside", AgentID: f.h.ID, RunID: f.h.RunID, JobID: f.lead.ID, ExpectedGeneration: f.lead.Generation, Reconciliation: &evidence}); err != nil {
		t.Fatal(err)
	}
	if b := f.batch(t, f.lead.ID); b.State != "dissolved" {
		t.Fatal("set-aside", b.State)
	}
	untouched(f, rows)
	if claimed, cerr := f.claim(f.members[0]); cerr != nil || claimed.State != "claimed" {
		t.Fatal("member after the lead was set aside", cerr)
	}

	// Blocked before publication holds the members until the handler's
	// inspected requeue, which dissolves the batch.
	f, rows = start(1)
	f.declare(t)
	if _, err = f.deployer("block", "", ""); err != nil {
		t.Fatal(err)
	}
	if b := f.batch(t, f.lead.ID); b.State != "held" || b.Published {
		t.Fatalf("blocked batch %+v", b)
	}
	f.memberHeld(t, f.members[0], rows[0])
	f.exitDeployer(t)
	if _, err = f.reconcile(t, func(*api.ReleaseReconciliation) {}); err != nil {
		t.Fatal("requeue", err)
	}
	if b := f.batch(t, f.lead.ID); b.State != "dissolved" {
		t.Fatal("requeue", b.State)
	}
	untouched(f, rows)
}

// The batch id is the runner's formula (the same literal is asserted in
// tests/release-runner.test.js), and a repeated batch request replays.
func TestReleaseBatchIdAndReplay(t *testing.T) {
	if got := releaseBatchID("rel_0123456789abcdef", 2, strings.Repeat("a", 40)); got != "bat_093efed8fd1877b6" {
		t.Fatal(got)
	}
	f := newBatchFixture(t, 1)
	ctx := context.Background()
	open := api.ReleaseRequest{RequestID: "replay-open", Operation: "batch-open", AgentID: f.d.ID, RunID: f.d.RunID, JobID: f.lead.ID, ExpectedGeneration: f.lead.Generation, IntegratedCommit: batchBase}
	opened, err := f.s.ReleaseAction(ctx, f.task.ID, open)
	if err != nil || opened.Generation != f.lead.Generation+1 || opened.State != "claimed" {
		t.Fatal(opened.Generation, err)
	}
	b := f.batch(t, f.lead.ID)
	if b.ID != releaseBatchID(f.lead.ID, f.lead.Generation, batchBase) || b.LeadGeneration != f.lead.Generation || len(b.Jobs) != 0 {
		t.Fatalf("opened %+v", b)
	}
	if again, err := f.s.ReleaseAction(ctx, f.task.ID, open); err != nil || again.Generation != opened.Generation {
		t.Fatal("open replay", err)
	}
	add := api.ReleaseRequest{RequestID: "replay-add", Operation: "batch-add", AgentID: f.d.ID, RunID: f.d.RunID, JobID: f.lead.ID, ExpectedGeneration: opened.Generation, EntryID: f.lead.EntryID, IntegratedCommit: batchTo(1)}
	added, err := f.s.ReleaseAction(ctx, f.task.ID, add)
	if err != nil || added.Generation != opened.Generation+1 {
		t.Fatal(added.Generation, err)
	}
	again, err := f.s.ReleaseAction(ctx, f.task.ID, add)
	if err != nil || again.Generation != added.Generation || f.rows(t) != 1 {
		t.Fatal("add replay", again.Generation, f.rows(t), err)
	}
	changed := add
	changed.IntegratedCommit = batchTo(2)
	if _, err = f.s.ReleaseAction(ctx, f.task.ID, changed); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "retry changed") || f.rows(t) != 1 {
		t.Fatal("changed add", err)
	}
	if got := f.batch(t, f.lead.ID); got.IntegratedCommit != batchTo(1) || got.Jobs[0].FromCommit != batchBase || got.Jobs[0].Generation != opened.Generation {
		t.Fatalf("after replay %+v", got)
	}
}

// Every refused batch-add leaves no row.
func TestReleaseBatchAddRefusals(t *testing.T) {
	ctx := context.Background()
	f := newBatchFixture(t, 9)
	refuse := func(name, want string, entry, commit string, rows int) {
		t.Helper()
		before := f.lead.Generation
		_, err := f.deployer("batch-add", entry, commit)
		if err == nil || (want == "" && !errors.Is(err, api.ErrInvalid)) || (want != "" && (!errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), want))) {
			t.Fatalf("%s: %v", name, err)
		}
		if f.rows(t) != rows || f.load(t, f.lead.ID).Generation != before {
			t.Fatalf("%s left a row or moved the lead: %d rows", name, f.rows(t))
		}
	}
	refuse("before open", "open release batch required", f.lead.EntryID, batchTo(1), 0)
	if _, err := f.deployer("batch-open", "", "not-a-commit"); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("open without a base", err)
	}
	if _, err := f.deployer("batch-open", "", batchBase); err != nil {
		t.Fatal(err)
	}
	if _, err := f.deployer("batch-open", "", batchBase); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is still open") {
		t.Fatal("second open", err)
	}
	refuse("member before the lead", "starts with its lead job", f.members[0].EntryID, batchTo(1), 0)
	refuse("no commit", "", f.lead.EntryID, "", 0)
	refuse("lead adds nothing", "adds no commit", f.lead.EntryID, batchBase, 0)
	if _, err := f.deployer("batch-add", f.lead.EntryID, batchTo(1)); err != nil {
		t.Fatal(err)
	}
	refuse("lead twice", "already in release batch", f.lead.EntryID, batchTo(2), 1)
	refuse("unknown entry", "entry has no release job", api.NewID("tqe"), batchTo(2), 1)
	refuse("no entry", "", "", batchTo(2), 1)

	m := f.members
	restore := func(j api.ReleaseJob) { rewriteJob(t, f.s, j, func(*api.ReleaseJob) {}) }
	rewriteJob(t, f.s, m[0], func(j *api.ReleaseJob) { j.AgentID, j.RunID = f.d.ID, f.d.RunID })
	refuse("claimed member", "is not an unclaimed verified job", m[0].EntryID, batchTo(2), 1)
	rewriteJob(t, f.s, m[0], func(j *api.ReleaseJob) { j.State = "refused" })
	refuse("settled member", "is not an unclaimed verified job", m[0].EntryID, batchTo(2), 1)
	rewriteJob(t, f.s, m[0], func(j *api.ReleaseJob) { j.BaseCommit = candidateB })
	refuse("other base", "has another base commit than the lead", m[0].EntryID, batchTo(2), 1)
	rewriteJob(t, f.s, m[0], func(j *api.ReleaseJob) { j.Plan.MatrixDigest = strings.Repeat("e", 64) })
	refuse("other matrix", "has another matrix approval than the lead", m[0].EntryID, batchTo(2), 1)
	rewriteJob(t, f.s, m[0], func(j *api.ReleaseJob) { j.Plan.MatrixApprovalMessageSeq = 77 })
	refuse("other approval", "has another matrix approval than the lead", m[0].EntryID, batchTo(2), 1)
	rewriteJob(t, f.s, m[0], func(j *api.ReleaseJob) { j.PauseGeneration++ })
	refuse("other project generation", "project generation changed for job", m[0].EntryID, batchTo(2), 1)
	restore(m[0])
	// The accepted candidate changed after enqueue.
	if _, err := f.s.db.Exec(`UPDATE work_items SET status='open' WHERE id=?`, m[0].ItemID); err != nil {
		t.Fatal(err)
	}
	refuse("changed candidate", "exact accepted SHA and current verification required", m[0].EntryID, batchTo(2), 1)
	if _, err := f.s.db.Exec(`UPDATE work_items SET status='done' WHERE id=?`, m[0].ItemID); err != nil {
		t.Fatal(err)
	}
	// Another run of the deployer, and the handler, cannot extend the batch.
	for name, who := range map[string][2]string{"wrong run": {f.d.ID, api.NewID("run")}, "handler": {f.h.ID, f.h.RunID}} {
		if _, err := f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: f.key("wrong"), Operation: "batch-add", AgentID: who[0], RunID: who[1], JobID: f.lead.ID, ExpectedGeneration: f.lead.Generation, EntryID: m[0].EntryID, IntegratedCommit: batchTo(2)}); !errors.Is(err, api.ErrConflict) || f.rows(t) != 1 {
			t.Fatal(name, err)
		}
	}
	if _, err := f.deployer("batch-add", m[1].EntryID, batchTo(2)); err != nil {
		t.Fatal(err)
	}
	refuse("earlier queue position", "in queue order", m[0].EntryID, batchTo(3), 2)
	// Eight jobs, then no ninth.
	for i := 2; i < 8; i++ {
		if _, err := f.deployer("batch-add", m[i].EntryID, batchTo(i+1)); err != nil {
			t.Fatal(i, err)
		}
	}
	refuse("ninth job", "already has 8 jobs", m[8].EntryID, strings.Repeat("9", 40), 8)

	// Nothing joins after the integrated verification is saved.
	f = newBatchFixture(t, 2)
	late := f.members[1]
	f.members = f.members[:1]
	f.declare(t)
	if err := f.verify(nil); err != nil {
		t.Fatal(err)
	}
	refuse("add after verification", "fixed once its integrated verification is saved", late.EntryID, candidateB, 2)
	if _, err := f.deployer("batch-open", "", batchBase); !errors.Is(err, api.ErrConflict) {
		t.Fatal("open after verification", err)
	}
	// An unclaimed job cannot lead a batch.
	if _, err := f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: "unclaimed-open", Operation: "batch-open", AgentID: f.d.ID, RunID: f.d.RunID, JobID: late.ID, ExpectedGeneration: late.Generation, IntegratedCommit: batchBase}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("unclaimed lead", err)
	}
}

// Once the lead is merged, tasks-hub carries every member: they stay held
// until the whole batch settles or an exact inspection records it restored.
func TestReleaseBatchPublishedMembersStayHeld(t *testing.T) {
	start := func() (*batchFixture, api.ReleaseJob, []string) {
		t.Helper()
		f := newBatchFixture(t, 3)
		outside := f.members[2]
		f.members = f.members[:2]
		rows := []string{releaseRow(t, f.s, f.task, f.members[0].ID), releaseRow(t, f.s, f.task, f.members[1].ID)}
		f.declare(t)
		f.publish(t)
		return f, outside, rows
	}
	held := func(f *batchFixture, rows []string, state string) {
		t.Helper()
		for i, m := range f.members {
			f.memberHeld(t, m, rows[i])
		}
		if b := f.batch(t, f.lead.ID); b.State != state || !b.Published {
			t.Fatalf("batch %+v, want %s", b, state)
		}
	}
	fenced := func(f *batchFixture, outside api.ReleaseJob) {
		t.Helper()
		if _, err := f.claim(outside); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "project already fenced") {
			t.Fatal("the fence allowed a second claim", err)
		}
	}
	free := func(f *batchFixture, rows []string) {
		t.Helper()
		if b := f.batch(t, f.lead.ID); b.State != "rolled_back" || b.SettledAt == "" {
			t.Fatalf("batch %+v", b)
		}
		for i, m := range f.members {
			if got := releaseRow(t, f.s, f.task, m.ID); got != rows[i] {
				t.Fatalf("member %d changed", i)
			}
		}
		f.resumeDeployer(t)
		if claimed, err := f.claim(f.members[0]); err != nil || claimed.State != "claimed" {
			t.Fatal("member after restoration", err)
		}
	}
	restored := func(r *api.ReleaseReconciliation) {
		r.Disposition, r.NoPublication, r.JournalState = "refuse", false, "restored"
	}

	// Merged, then the run stops: only exact restoration evidence frees them.
	f, outside, rows := start()
	held(f, rows, "published")
	fenced(f, outside)
	f.exitDeployer(t)
	if _, err := f.reconcile(t, func(r *api.ReleaseReconciliation) { restored(r); r.RefResolved = false }); !errors.Is(err, api.ErrConflict) {
		t.Fatal("reconcile without a resolved ref", err)
	}
	if _, err := f.reconcile(t, func(r *api.ReleaseReconciliation) { restored(r); r.JournalState = "unknown" }); !errors.Is(err, api.ErrConflict) {
		t.Fatal("reconcile without an inspected journal", err)
	}
	if _, err := f.reconcile(t, func(*api.ReleaseReconciliation) {}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is published; requeue cannot free its members") {
		t.Fatal("requeue of a published batch", err)
	}
	held(f, rows, "published")
	if lead, err := f.reconcile(t, restored); err != nil || lead.State != "refused" {
		t.Fatal(lead.State, err)
	}
	free(f, rows)

	// Finished blocked: held with the fence, until the same evidence.
	f, outside, rows = start()
	if lead, err := f.finish("blocked", nil); err != nil || lead.State != "blocked" {
		t.Fatal(lead.State, err)
	}
	held(f, rows, "held")
	fenced(f, outside)
	f.exitDeployer(t)
	if _, err := f.reconcile(t, func(r *api.ReleaseReconciliation) { restored(r); r.NoActiveExecution = false }); !errors.Is(err, api.ErrConflict) {
		t.Fatal("reconcile with a live execution", err)
	}
	held(f, rows, "held")
	if _, err := f.reconcile(t, restored); err != nil {
		t.Fatal(err)
	}
	free(f, rows)

	// The deployer's own block after publication holds them too.
	f, outside, rows = start()
	if _, err := f.deployer("block", "", ""); err != nil {
		t.Fatal(err)
	}
	held(f, rows, "held")
	fenced(f, outside)

	// Rolled back live, but the tasks-hub revert failed: the lead is settled
	// and the fence is free, yet tasks-hub still carries the members.
	for _, revert := range []*api.ReleaseRevert{{Outcome: "failed"}, nil} {
		f, outside, rows = start()
		lead, err := f.finish("rolled_back", revert)
		if err != nil || lead.State != "rolled_back" {
			t.Fatal(lead.State, err)
		}
		held(f, rows, "held")
		next, err := f.claim(outside)
		if err != nil {
			t.Fatal("job outside the batch", err)
		}
		// No new batch while one is held.
		if _, err = f.s.ReleaseAction(context.Background(), f.task.ID, api.ReleaseRequest{RequestID: f.key("open"), Operation: "batch-open", AgentID: f.d.ID, RunID: f.d.RunID, JobID: next.ID, ExpectedGeneration: next.Generation, IntegratedCommit: batchBase}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is still open, published or held") {
			t.Fatal("batch opened beside a held one", err)
		}
		if _, err = f.s.ReleaseAction(context.Background(), f.task.ID, api.ReleaseRequest{RequestID: f.key("refuse"), Operation: "refuse", AgentID: f.d.ID, RunID: f.d.RunID, JobID: next.ID, ExpectedGeneration: next.Generation}); err != nil {
			t.Fatal(err)
		}
		// A hand release cannot supersede the lead while it holds the batch:
		// that would leave the members held with no job to reconcile.
		hand := recordHandRelease(t, f.s, f.task, f.key("hand"), candidateB, []string{candidateA, candidateB})
		supersede := api.ReleaseRequest{RequestID: f.key("supersede"), Operation: "supersede", AgentID: f.h.ID, RunID: f.h.RunID, JobID: f.lead.ID, ExpectedGeneration: f.lead.Generation, Supersession: &api.ReleaseSupersession{ReleasedCommit: hand.ReleasedCommit, HandReleaseID: hand.ID}}
		if _, err = f.s.ReleaseAction(context.Background(), f.task.ID, supersede); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "leads release batch") || !strings.Contains(err.Error(), "(held)") {
			t.Fatal("lead of a held batch superseded", err)
		}
		if got := f.load(t, f.lead.ID); got.State != "rolled_back" || got.Generation != f.lead.Generation || got.Supersession != nil {
			t.Fatalf("refused supersede changed the lead: %s", got.State)
		}
		if _, err = f.reconcile(t, func(r *api.ReleaseReconciliation) { restored(r); r.JournalState = "no_effects" }); !errors.Is(err, api.ErrConflict) {
			t.Fatal("held batch freed without a restored journal", err)
		}
		if _, err = f.reconcile(t, func(*api.ReleaseReconciliation) {}); !errors.Is(err, api.ErrConflict) {
			t.Fatal("held batch freed by a requeue", err)
		}
		held(f, rows, "held")
		lead, err = f.reconcile(t, restored)
		if err != nil || lead.State != "rolled_back" || lead.Receipt == nil || len(lead.Reconciliations) != 1 || lead.Reconciliations[0].JournalState != "restored" {
			t.Fatalf("%+v %v", lead, err)
		}
		free(f, rows)
		// Freed, the same supersede is accepted: the refusal was the batch's.
		supersede.RequestID, supersede.ExpectedGeneration = f.key("supersede"), lead.Generation
		if done, serr := f.s.ReleaseAction(context.Background(), f.task.ID, supersede); serr != nil || done.State != "superseded" {
			t.Fatal("supersede after the batch was freed", serr)
		}
	}
}

// The batch tables are created beside an existing ledger, survive a second
// open unchanged, and keep their rows as history.
func TestReleaseBatchTablesSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "batch ledger"}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	// A ledger from before batches has neither table.
	if _, err = s.db.Exec(`DROP TABLE release_batch_jobs; DROP TABLE release_batches`); err != nil {
		t.Fatal(err)
	}
	shape := func(s *Store) string {
		t.Helper()
		var out []string
		rows, err := s.db.Query(`SELECT type||' '||name||' '||coalesce(sql,'') FROM sqlite_master WHERE tbl_name IN ('release_batches','release_batch_jobs') ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			if err = rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
	if shape(s) != "" {
		t.Fatal(shape(s))
	}
	reopen := func() {
		t.Helper()
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		if s, err = Open(path); err != nil {
			t.Fatal(err)
		}
	}
	reopen()
	created := shape(s)
	for _, want := range []string{"table release_batches", "table release_batch_jobs", "index release_batch_active", "trigger release_batch_no_delete", "trigger release_batch_job_no_update", "trigger release_batch_job_no_delete"} {
		if !strings.Contains(created, want) {
			t.Fatal("missing", want, created)
		}
	}
	id := releaseBatchID("rel_fixture", 2, batchBase)
	if _, err = s.db.Exec(`INSERT INTO release_batches(task_id,batch_id,lead_job_id,lead_generation,base_commit,state,created_at) VALUES(?,?,?,?,?,'open',?)`, task.ID, id, "rel_fixture", 2, batchBase, ts(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO release_batch_jobs VALUES(?,?,1,'rel_fixture','tqe_fixture',3,?,?)`, task.ID, id, batchBase, candidateA); err != nil {
		t.Fatal(err)
	}
	reopen()
	defer s.Close()
	if shape(s) != created {
		t.Fatal("a second open changed the batch tables")
	}
	b, err := s.ReleaseBatchOf(ctx, task.ID, "rel_fixture")
	if err != nil || b.ID != id || b.State != "open" || len(b.Jobs) != 1 || b.Jobs[0].ToCommit != candidateA || b.Jobs[0].Generation != 3 {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err = s.db.Exec(`INSERT INTO release_batches(task_id,batch_id,lead_job_id,lead_generation,base_commit,state,created_at) VALUES(?,?,?,?,?,'held',?)`, task.ID, "bat_second", "rel_other", 1, batchBase, ts(time.Now())); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatal("two active batches in one project", err)
	}
	if _, err = s.db.Exec(`DELETE FROM release_batches WHERE batch_id=?`, id); err == nil || !strings.Contains(err.Error(), "release batch history") {
		t.Fatal("batch deleted", err)
	}
	if _, err = s.db.Exec(`UPDATE release_batch_jobs SET to_commit=? WHERE batch_id=?`, candidateB, id); err == nil || !strings.Contains(err.Error(), "immutable release batch job") {
		t.Fatal("batch job rewritten", err)
	}
	if _, err = s.db.Exec(`DELETE FROM release_batch_jobs WHERE batch_id=?`, id); err == nil || !strings.Contains(err.Error(), "immutable release batch job") {
		t.Fatal("batch job deleted", err)
	}
}

// holdFixture is a project with n verified release jobs in queue order, each
// for its own item.
func holdFixture(t *testing.T, n int) (*Store, api.Task, api.Agent, api.Agent, []api.ReleaseJob) {
	t.Helper()
	s, task, h, d, entry := releaseFixture(t)
	jobs := make([]api.ReleaseJob, 0, n)
	for i := range n {
		if i > 0 {
			entry = releaseEntry(t, s, task, fmt.Sprintf("held line item %d", i+1), fmt.Sprintf("held-line-%d", i+1))
		}
		j, err := s.ReleaseAction(context.Background(), task.ID, api.ReleaseRequest{RequestID: fmt.Sprintf("enqueue-%d", i+1), Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	return s, task, h, d, jobs
}

func holdRequest(key string, h api.Agent, j api.ReleaseJob, reason string, items ...string) api.ReleaseRequest {
	return api.ReleaseRequest{RequestID: key, Operation: "hold", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Hold: &api.ReleaseHold{Reason: reason, UntilItems: items}}
}

func unholdRequest(key string, h api.Agent, j api.ReleaseJob, reason string) api.ReleaseRequest {
	return api.ReleaseRequest{RequestID: key, Operation: "unhold", AgentID: h.ID, RunID: h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Hold: &api.ReleaseHold{Reason: reason}}
}

func claimRequest(key string, d api.Agent, j api.ReleaseJob) api.ReleaseRequest {
	return api.ReleaseRequest{RequestID: key, Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}
}

// releaseThrough takes a verified job through claim, merged and a released
// finish as the deployer.
func releaseThrough(t *testing.T, s *Store, task api.Task, d api.Agent, j api.ReleaseJob) api.ReleaseJob {
	t.Helper()
	return releaseThroughPush(t, s, task, d, j, nil)
}

// releaseThroughPush is releaseThrough whose released receipt carries push,
// as the runner's finish after it selected and deployed targets does.
func releaseThroughPush(t *testing.T, s *Store, task api.Task, d api.Agent, j api.ReleaseJob, push *api.ReleasePush) api.ReleaseJob {
	t.Helper()
	ctx := context.Background()
	var err error
	if j, err = s.ReleaseAction(ctx, task.ID, claimRequest(j.ID+"-claim", d, j)); err != nil {
		t.Fatal(err)
	}
	if j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: j.ID + "-merged", Operation: "merged", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit}); err != nil {
		t.Fatal(err)
	}
	finish := api.ReleaseRequest{RequestID: j.ID + "-finish", Operation: "finish", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}
	finish.Receipt = &api.ReleaseReceipt{Version: 1, JobID: j.ID, Commit: j.Commit, VerificationDigest: j.VerificationDigest, Outcome: "released", Targets: []api.ReleaseTargetReceipt{{Target: "tailos", Release: "fixture", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "released"}}, Push: push}
	if j, err = s.ReleaseAction(ctx, task.ID, finish); err != nil || j.State != "released" {
		t.Fatal(j.State, err)
	}
	return j
}

// wi_0c5c5add2242991b a1: the handler holds a verified job; the deployer's
// claim of it is refused and its claim of the next verified job succeeds.
func TestReleaseHoldKeepsVerifiedJobFromClaim(t *testing.T) {
	s, task, h, d, jobs := holdFixture(t, 2)
	ctx := context.Background()
	a, b := jobs[0], jobs[1]
	before := time.Now().Add(-time.Second)
	held, err := s.ReleaseAction(ctx, task.ID, holdRequest("hold", h, a, "Safety fixes must release first", b.ItemID))
	if err != nil {
		t.Fatal(err)
	}
	if held.State != "held" || held.Generation != a.Generation+1 || held.AgentID != "" || held.RunID != "" {
		t.Fatalf("held job %+v", held)
	}
	hold := held.Hold
	if hold == nil || hold.Reason != "Safety fixes must release first" || !slices.Equal(hold.UntilItems, []string{b.ItemID}) || hold.AgentID != h.ID || hold.RunID != h.RunID {
		t.Fatalf("hold %+v", hold)
	}
	if at, perr := time.Parse(time.RFC3339Nano, hold.HeldAt); perr != nil || at.Before(before) {
		t.Fatal("heldAt", hold.HeldAt, perr)
	}
	if saved, err := releaseLoad(ctx, s.db, task.ID, a.ID); err != nil || saved.State != "held" || saved.Hold == nil || saved.Hold.Reason != hold.Reason {
		t.Fatal("saved", saved.State, err)
	}
	_, err = s.ReleaseAction(ctx, task.ID, claimRequest("claim-held", d, held))
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "tt deployment unhold") || !strings.Contains(err.Error(), "owner hold (Safety fixes must release first)") {
		t.Fatal("claim of a held job", err)
	}
	if saved, err := releaseLoad(ctx, s.db, task.ID, a.ID); err != nil || saved.State != "held" || saved.Generation != held.Generation || saved.AgentID != "" {
		t.Fatal("refused claim changed the held job", saved.State, err)
	}
	next, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-next", d, b))
	if err != nil || next.State != "claimed" || next.AgentID != d.ID || next.RunID != d.RunID {
		t.Fatal("the next verified job was not claimed", next.State, err)
	}
}

// a2: unhold returns the job to verified in its original queue position and
// the history keeps both transitions.
func TestReleaseUnholdMakesJobClaimable(t *testing.T) {
	s, task, h, d, jobs := holdFixture(t, 2)
	ctx := context.Background()
	a, b := jobs[0], jobs[1]
	held, err := s.ReleaseAction(ctx, task.ID, holdRequest("hold", h, a, "Owner wants to read the change first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, unholdRequest("unhold-verified", h, b, "nothing to release")); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is not on hold") {
		t.Fatal("unhold of a verified job", err)
	}
	free, err := s.ReleaseAction(ctx, task.ID, unholdRequest("unhold", h, held, "Owner go at message 100"))
	if err != nil {
		t.Fatal(err)
	}
	if free.State != "verified" || free.Hold != nil || free.Generation != held.Generation+1 || free.AgentID != "" {
		t.Fatalf("unheld job %+v", free)
	}
	history := free.HoldHistory
	if len(history) != 2 {
		t.Fatalf("history %+v", history)
	}
	if e := history[0]; e.Action != "hold" || e.Cause != "" || e.Reason != "Owner wants to read the change first" || e.AgentID != h.ID || e.RunID != h.RunID || e.Generation != held.Generation || e.At == "" {
		t.Fatalf("hold event %+v", e)
	}
	if e := history[1]; e.Action != "release" || e.Cause != "handler" || e.Reason != "Owner go at message 100" || e.AgentID != h.ID || e.RunID != h.RunID || e.Generation != free.Generation || e.At == "" || e.TriggerJobID != "" {
		t.Fatalf("release event %+v", e)
	}
	line, err := s.Releases(ctx, task.ID)
	if err != nil || len(line) != 2 || line[0].ID != a.ID || line[1].ID != b.ID || line[0].State != "verified" {
		t.Fatal("queue order", line, err)
	}
	claimed, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim", d, free))
	if err != nil || claimed.State != "claimed" || claimed.RunID != d.RunID || len(claimed.HoldHistory) != 2 {
		t.Fatal("unheld job was not claimable", claimed.State, err)
	}
}

// a3: a hold applies only before a claim; a claimed or terminal job is left
// exactly as it was and the refusal points to set-aside.
func TestReleaseHoldRefusesClaimedAndTerminalJobs(t *testing.T) {
	s, task, h, d, jobs := holdFixture(t, 3)
	ctx := context.Background()
	refuse := func(name string, j api.ReleaseJob) {
		t.Helper()
		row := releaseRow(t, s, task, j.ID)
		_, err := s.ReleaseAction(ctx, task.ID, holdRequest("hold-"+name, h, j, "Hold it"))
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "tt deployment set-aside") || !strings.Contains(err.Error(), j.State) {
			t.Fatalf("%s: %v", name, err)
		}
		// h3 (wi_c09132e28129688d): a claimed job's refusal names the
		// supported path, set-aside after the integrated run, then hold.
		if j.State == "claimed" && (!strings.Contains(err.Error(), "After its integrated verification run ends") || !strings.Contains(err.Error(), "and then held") || !strings.Contains(err.Error(), "tt deployment hold --entry")) {
			t.Fatalf("%s: %v", name, err)
		}
		if releaseRow(t, s, task, j.ID) != row {
			t.Fatalf("%s: refused hold changed the job", name)
		}
	}
	a, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-a", d, jobs[0]))
	if err != nil {
		t.Fatal(err)
	}
	refuse("claimed", a)
	if a, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "refuse-a", Operation: "refuse", AgentID: d.ID, RunID: d.RunID, JobID: a.ID, ExpectedGeneration: a.Generation}); err != nil {
		t.Fatal(err)
	}
	refuse("refused", a)
	refuse("released", releaseThrough(t, s, task, d, jobs[1]))
	hand := recordHandRelease(t, s, task, "hand", candidateB, []string{candidateB})
	c, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "supersede-c", Operation: "supersede", AgentID: h.ID, RunID: h.RunID, JobID: jobs[2].ID, ExpectedGeneration: jobs[2].Generation, Supersession: &api.ReleaseSupersession{ReleasedCommit: candidateB, HandReleaseID: hand.ID}})
	if err != nil {
		t.Fatal(err)
	}
	refuse("superseded", c)
}

// a4: hold and unhold are the handler's, at the exact generation, and replay
// by request ID.
func TestReleaseHoldRequiresHandlerAndGeneration(t *testing.T) {
	s, task, h, d, jobs := holdFixture(t, 1)
	ctx := context.Background()
	a := jobs[0]
	row := releaseRow(t, s, task, a.ID)
	if _, err := s.ReleaseAction(ctx, task.ID, holdRequest("by-deployer", d, a, "Hold it")); !errors.Is(err, api.ErrConflict) {
		t.Fatal("deployer held a job", err)
	}
	stale := holdRequest("stale", h, a, "Hold it")
	stale.ExpectedGeneration++
	if _, err := s.ReleaseAction(ctx, task.ID, stale); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "generation changed") {
		t.Fatal("stale generation", err)
	}
	if releaseRow(t, s, task, a.ID) != row {
		t.Fatal("refused hold changed the job")
	}
	req := holdRequest("hold", h, a, "Hold it")
	held, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.ReleaseAction(ctx, task.ID, req); err != nil || again.Generation != held.Generation || again.State != "held" || len(again.HoldHistory) != 1 {
		t.Fatal("replay", again.Generation, err)
	}
	changed := holdRequest("hold", h, a, "Another reason")
	if _, err = s.ReleaseAction(ctx, task.ID, changed); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "retry changed") {
		t.Fatal("changed hold replayed", err)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, holdRequest("hold-again", h, held, "Hold it twice")); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "already on hold (Hold it)") {
		t.Fatal("second hold", err)
	}
	row = releaseRow(t, s, task, a.ID)
	if _, err = s.ReleaseAction(ctx, task.ID, unholdRequest("unhold-by-deployer", d, held, "Go")); !errors.Is(err, api.ErrConflict) {
		t.Fatal("deployer released a hold", err)
	}
	stale = unholdRequest("unhold-stale", h, held, "Go")
	stale.ExpectedGeneration--
	if _, err = s.ReleaseAction(ctx, task.ID, stale); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "generation changed") {
		t.Fatal("stale unhold", err)
	}
	for name, bad := range map[string]*api.ReleaseHold{"no record": nil, "no reason": {}, "two lines": {Reason: "Go\nnow"}, "items": {Reason: "Go", UntilItems: []string{a.ItemID}}} {
		r := unholdRequest("unhold-bad-"+strings.ReplaceAll(name, " ", "-"), h, held, "")
		r.Hold = bad
		if _, err = s.ReleaseAction(ctx, task.ID, r); !errors.Is(err, api.ErrInvalid) {
			t.Fatal("unhold", name, err)
		}
	}
	if releaseRow(t, s, task, a.ID) != row {
		t.Fatal("refused unhold changed the job")
	}
	free := unholdRequest("unhold", h, held, "Go")
	done, err := s.ReleaseAction(ctx, task.ID, free)
	if err != nil || done.State != "verified" {
		t.Fatal(done.State, err)
	}
	if again, err := s.ReleaseAction(ctx, task.ID, free); err != nil || again.Generation != done.Generation || again.State != "verified" {
		t.Fatal("unhold replay", again.Generation, err)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, unholdRequest("unhold", h, held, "Go later")); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "retry changed") {
		t.Fatal("changed unhold replayed", err)
	}
}

// a4: the reason and the named items are checked before anything is saved.
func TestReleaseHoldValidation(t *testing.T) {
	s, task, h, d, jobs := holdFixture(t, 3)
	ctx := context.Background()
	a, b, c := jobs[0], jobs[1], jobs[2]
	row := releaseRow(t, s, task, a.ID)
	many := make([]string, 17)
	for i := range many {
		many[i] = api.NewID("wi")
	}
	keys := 0
	refuse := func(name string, want error, text string, hold *api.ReleaseHold) {
		t.Helper()
		keys++
		req := holdRequest(fmt.Sprintf("bad-%d", keys), h, a, "")
		req.Hold = hold
		_, err := s.ReleaseAction(ctx, task.ID, req)
		if !errors.Is(err, want) || !strings.Contains(err.Error(), text) {
			t.Fatalf("%s: %v", name, err)
		}
		if releaseRow(t, s, task, a.ID) != row {
			t.Fatalf("%s: refused hold changed the job", name)
		}
	}
	refuse("no record", api.ErrInvalid, "", nil)
	refuse("no reason", api.ErrInvalid, "", &api.ReleaseHold{})
	refuse("two line reason", api.ErrInvalid, "", &api.ReleaseHold{Reason: "Hold\nit"})
	refuse("long reason", api.ErrInvalid, "", &api.ReleaseHold{Reason: strings.Repeat("r", 513)})
	refuse("malformed item", api.ErrInvalid, "", &api.ReleaseHold{Reason: "Hold it", UntilItems: []string{"item-one"}})
	refuse("duplicate item", api.ErrInvalid, "", &api.ReleaseHold{Reason: "Hold it", UntilItems: []string{b.ItemID, b.ItemID}})
	refuse("seventeen items", api.ErrInvalid, "", &api.ReleaseHold{Reason: "Hold it", UntilItems: many})
	refuse("own item", api.ErrConflict, "its own item", &api.ReleaseHold{Reason: "Hold it", UntilItems: []string{a.ItemID}})
	unknown := api.NewID("wi")
	refuse("unknown item", api.ErrConflict, "named item "+unknown+" is not a work item of this project", &api.ReleaseHold{Reason: "Hold it", UntilItems: []string{b.ItemID, unknown}})
	other := secondTask(t, s)
	foreign, err := s.CreateWorkItem(ctx, other.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "another project's item", RequestID: "foreign"}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	refuse("another project's item", api.ErrConflict, "is not a work item of this project", &api.ReleaseHold{Reason: "Hold it", UntilItems: []string{foreign.ID}})
	releaseThrough(t, s, task, d, b)
	refuse("condition already met", api.ErrConflict, "the named items are already released", &api.ReleaseHold{Reason: "Hold it", UntilItems: []string{b.ItemID}})
	// One of two named items released is a condition still open.
	if held, err := s.ReleaseAction(ctx, task.ID, holdRequest("hold-open", h, a, "Hold it", b.ItemID, c.ItemID)); err != nil || held.State != "held" {
		t.Fatal(held.State, err)
	}
	// A requeue may leave the host release lock with the job.
	locked := rewriteJob(t, s, c, func(j *api.ReleaseJob) {
		j.Reconciliations = append(j.Reconciliations, api.ReleaseReconciliation{Disposition: "requeue", LockDigest: strings.Repeat("e", 64)})
	})
	if _, err = s.ReleaseAction(ctx, task.ID, holdRequest("hold-locked", h, locked, "Hold it")); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "host release lock") {
		t.Fatal("hold on a requeued job with a host lock", err)
	}
}

// a5: a held job is outside the release line: no batch takes it, set-aside
// does not count it as waiting, and reconcile and supersede wait for unhold.
func TestReleaseHeldJobIsOutsideTheLine(t *testing.T) {
	ctx := context.Background()
	f := newBatchFixture(t, 1)
	held, err := f.s.ReleaseAction(ctx, f.task.ID, holdRequest("hold", f.h, f.members[0], "Wait for the owner"))
	if err != nil || held.State != "held" {
		t.Fatal(held.State, err)
	}
	row := releaseRow(t, f.s, f.task, held.ID)
	// The lead is claimed with no effects, and the only job behind it is held.
	evidence := setAsideEvidence(t, f.s, f.task, f.h, f.lead)
	aside := api.ReleaseRequest{RequestID: "set-aside", Operation: "set-aside", AgentID: f.h.ID, RunID: f.h.RunID, JobID: f.lead.ID, ExpectedGeneration: f.lead.Generation, Reconciliation: &evidence}
	if _, err = f.s.ReleaseAction(ctx, f.task.ID, aside); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "no later verified job is waiting") {
		t.Fatal("set-aside counted a held job as waiting", err)
	}
	if _, err = f.deployer("batch-open", "", batchBase); err != nil {
		t.Fatal(err)
	}
	if _, err = f.deployer("batch-add", f.lead.EntryID, batchTo(1)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.deployer("batch-add", held.EntryID, candidateA); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is not an unclaimed verified job") {
		t.Fatal("a held job joined a batch", err)
	}
	if b := f.batch(t, f.lead.ID); len(b.Jobs) != 1 {
		t.Fatalf("batch %+v", b.Jobs)
	}
	recovery := recoveryEvidence(t, f.s, f.task, f.h, held)
	reconcile := api.ReleaseRequest{RequestID: "reconcile-held", Operation: "reconcile", AgentID: f.h.ID, RunID: f.h.RunID, JobID: held.ID, ExpectedGeneration: held.Generation, Reconciliation: &recovery}
	if _, err = f.s.ReleaseAction(ctx, f.task.ID, reconcile); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "tt deployment unhold") {
		t.Fatal("held job reconciled", err)
	}
	hand := recordHandRelease(t, f.s, f.task, "hand", candidateB, []string{candidateB})
	supersede := func(key string, j api.ReleaseJob) (api.ReleaseJob, error) {
		return f.s.ReleaseAction(ctx, f.task.ID, api.ReleaseRequest{RequestID: key, Operation: "supersede", AgentID: f.h.ID, RunID: f.h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Supersession: &api.ReleaseSupersession{ReleasedCommit: candidateB, HandReleaseID: hand.ID}})
	}
	if _, err = supersede("supersede-held", held); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "tt deployment unhold") {
		t.Fatal("held job superseded", err)
	}
	if releaseRow(t, f.s, f.task, held.ID) != row {
		t.Fatal("a refused operation changed the held job")
	}
	free, err := f.s.ReleaseAction(ctx, f.task.ID, unholdRequest("unhold", f.h, held, "Owner go"))
	if err != nil {
		t.Fatal(err)
	}
	if done, err := supersede("supersede", free); err != nil || done.State != "superseded" || len(done.HoldHistory) != 2 {
		t.Fatal("unheld job was not supersedable", done.State, err)
	}
}

// a6: the active page shows the held job with its hold; the settled page
// carries none.
func TestReleasesPageShowsHold(t *testing.T) {
	s, task, h, d, jobs := holdFixture(t, 3)
	ctx := context.Background()
	held, err := s.ReleaseAction(ctx, task.ID, holdRequest("hold", h, jobs[0], "Owner hold until the fix ships", jobs[2].ItemID))
	if err != nil {
		t.Fatal(err)
	}
	released := releaseThrough(t, s, task, d, jobs[1])
	active, err := s.ReleasesPage(ctx, task.ID, api.ReleaseListOptions{})
	if err != nil || len(active.Jobs) != 2 {
		t.Fatal(active.Jobs, err)
	}
	first := active.Jobs[0]
	if first.ID != held.ID || first.State != "held" || first.Generation != held.Generation || first.Hold == nil || first.Hold.Reason != "Owner hold until the fix ships" || !slices.Equal(first.Hold.UntilItems, []string{jobs[2].ItemID}) || first.Hold.AgentID != h.ID || first.Hold.HeldAt == "" {
		t.Fatalf("held summary %+v %+v", first, first.Hold)
	}
	if second := active.Jobs[1]; second.ID != jobs[2].ID || second.State != "verified" || second.Hold != nil {
		t.Fatalf("verified summary %+v", second)
	}
	settled, err := s.ReleasesPage(ctx, task.ID, api.ReleaseListOptions{View: "settled"})
	if err != nil || len(settled.Jobs) != 1 || settled.Jobs[0].ID != released.ID || settled.Jobs[0].Hold != nil {
		t.Fatal(settled.Jobs, err)
	}
	// The wire form the CLI and the runner read.
	raw, _ := json.Marshal(first)
	var wire map[string]json.RawMessage
	if json.Unmarshal(raw, &wire) != nil || !strings.Contains(string(wire["hold"]), `"reason":"Owner hold until the fix ships"`) || string(wire["state"]) != `"held"` {
		t.Fatal(string(raw))
	}
}

// a7: a hold that names items lifts itself when the last of them is released
// or superseded; a hold with no items is lifted only by unhold.
func TestReleaseHoldLiftsWhenNamedItemsRelease(t *testing.T) {
	s, task, h, d, jobs := holdFixture(t, 6)
	ctx := context.Background()
	a, b, c, plain, e, f := jobs[0], jobs[1], jobs[2], jobs[3], jobs[4], jobs[5]
	hold := func(key string, j api.ReleaseJob, items ...string) api.ReleaseJob {
		t.Helper()
		held, err := s.ReleaseAction(ctx, task.ID, holdRequest(key, h, j, "Wait for the safety fixes", items...))
		if err != nil || held.State != "held" {
			t.Fatal(key, held.State, err)
		}
		return held
	}
	load := func(j api.ReleaseJob) api.ReleaseJob {
		t.Helper()
		saved, err := releaseLoad(ctx, s.db, task.ID, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	a = hold("hold-a", a, b.ItemID, c.ItemID)
	plain = hold("hold-plain", plain)
	e = hold("hold-e", e, b.ItemID, f.ItemID)
	rows := map[string]string{a.ID: releaseRow(t, s, task, a.ID), plain.ID: releaseRow(t, s, task, plain.ID), e.ID: releaseRow(t, s, task, e.ID), f.ID: releaseRow(t, s, task, f.ID)}
	unchanged := func(when string, ids ...string) {
		t.Helper()
		for _, id := range ids {
			if releaseRow(t, s, task, id) != rows[id] {
				t.Fatalf("%s: job %s was rewritten", when, id)
			}
		}
	}
	// The first named item releases: both conditions are still open.
	releaseThrough(t, s, task, d, b)
	unchanged("after the first item", a.ID, plain.ID, e.ID, f.ID)
	// The second: a lifts, with the settling job as its trigger.
	released := releaseThrough(t, s, task, d, c)
	unchanged("after the second item", plain.ID, e.ID, f.ID)
	lifted := load(a)
	if lifted.State != "verified" || lifted.Hold != nil || lifted.Generation != a.Generation+1 || len(lifted.HoldHistory) != 2 {
		t.Fatalf("lifted job %+v", lifted)
	}
	if ev := lifted.HoldHistory[1]; ev.Action != "release" || ev.Cause != "items_released" || ev.TriggerJobID != released.ID || ev.AgentID != "" || ev.Generation != lifted.Generation || ev.At == "" || !slices.Equal(ev.UntilItems, []string{b.ItemID, c.ItemID}) {
		t.Fatalf("lift event %+v", ev)
	}
	// A hand release that supersedes the last named item's job lifts too.
	hand := recordHandRelease(t, s, task, "hand", candidateB, []string{candidateB})
	gone, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "supersede-f", Operation: "supersede", AgentID: h.ID, RunID: h.RunID, JobID: f.ID, ExpectedGeneration: f.Generation, Supersession: &api.ReleaseSupersession{ReleasedCommit: candidateB, HandReleaseID: hand.ID}})
	if err != nil || gone.State != "superseded" {
		t.Fatal(gone.State, err)
	}
	unchanged("after the supersession", plain.ID)
	lifted = load(e)
	if lifted.State != "verified" || lifted.Hold != nil || len(lifted.HoldHistory) != 2 || lifted.HoldHistory[1].Cause != "items_released" || lifted.HoldHistory[1].TriggerJobID != f.ID {
		t.Fatalf("job lifted by a supersession %+v", lifted)
	}
	// Queue order is the original one, and the lifted job is claimable.
	line, err := s.Releases(ctx, task.ID)
	if err != nil || len(line) != 6 || line[0].ID != a.ID || line[3].ID != plain.ID || line[4].ID != e.ID {
		t.Fatal("queue order", err)
	}
	if claimed, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-lifted", d, load(a))); err != nil || claimed.State != "claimed" {
		t.Fatal("lifted job was not claimable", claimed.State, err)
	}
	if still := load(plain); still.State != "held" || still.Hold == nil || len(still.HoldHistory) != 1 {
		t.Fatalf("a hold with no items lifted by itself: %+v", still)
	}
}

// a1: the 90 s claim freshness follows the store clock, not the wall clock.
func TestReleaseDeployerFreshnessUsesStoreClock(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	// Two days ahead of the wall clock: time.Since would call this heartbeat
	// fresh at any offset below, and a wall-clock heartbeat stale.
	beat := time.Now().UTC().Add(48 * time.Hour)
	if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(beat), d.ID); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return beat }
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	claim := api.ReleaseRequest{RequestID: "stale", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}
	s.now = func() time.Time { return beat.Add(91 * time.Second) }
	if _, err = s.ReleaseAction(ctx, task.ID, claim); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "deployment heartbeat stale") {
		t.Fatalf("91 s after the heartbeat on the store clock: %v", err)
	}
	if _, err = s.ReleaseHandler(ctx, task.ID, d.ID, d.RunID); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "deployment heartbeat stale") {
		t.Fatalf("handler lookup 91 s after the heartbeat: %v", err)
	}
	s.now = func() time.Time { return beat.Add(89 * time.Second) }
	claim.RequestID = "fresh"
	if j, err = s.ReleaseAction(ctx, task.ID, claim); err != nil || j.State != "claimed" {
		t.Fatalf("89 s after the heartbeat on the store clock: %v %s", err, j.State)
	}
	if _, err = s.ReleaseHandler(ctx, task.ID, d.ID, d.RunID); err != nil {
		t.Fatalf("handler lookup 89 s after the heartbeat: %v", err)
	}
}

// livenessFixture is a release fixture with an owner helper and an injected
// store clock; the deployer's heartbeat starts at that clock.
type livenessFixture struct {
	t         *testing.T
	s         *Store
	task      api.Task
	h, d, own api.Agent
	entry     string
	clock     time.Time
	ctx       context.Context
	keys      int
	// live keeps the deployer's heartbeat fresh at every tick of advance;
	// unseen counts notices that ticks of advance posted.
	live   bool
	unseen int
}

func newLivenessFixture(t *testing.T) *livenessFixture {
	t.Helper()
	s, task, h, d, entry := releaseFixture(t)
	f := &livenessFixture{t: t, s: s, task: task, h: h, d: d, entry: entry, clock: time.Now().UTC().Truncate(time.Second), ctx: context.Background()}
	s.now = func() time.Time { return f.clock }
	own, err := s.AddAgent(f.ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "owner-helper", Host: "fixture", Session: "owner-helper"}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	f.own = own
	f.exec(`UPDATE agents SET role=? WHERE id=?`, api.AgentRoleOwnerHelper, own.ID)
	f.beat()
	return f
}

func (f *livenessFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.s.db.Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *livenessFixture) key(op string) string {
	f.keys++
	return fmt.Sprintf("%s-%d", op, f.keys)
}

// beat records a deployer heartbeat at the fixture clock.
func (f *livenessFixture) beat() {
	f.t.Helper()
	f.exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(f.clock), f.d.ID)
}

// advance moves the clock in broker ticks of 30 s and sweeps at each, as the
// hub does; the notices those sweeps post are returned by the next sweep.
func (f *livenessFixture) advance(d time.Duration) {
	f.t.Helper()
	for d > 0 {
		step := min(d, 30*time.Second)
		d -= step
		f.clock = f.clock.Add(step)
		if f.live {
			f.beat()
		}
		posted, err := f.s.DeployerLivenessSweep(f.ctx, f.clock)
		if err != nil {
			f.t.Fatal(err)
		}
		f.unseen += len(posted)
	}
}

// gap moves the clock with no sweep: the hub is down.
func (f *livenessFixture) gap(d time.Duration) { f.clock = f.clock.Add(d) }

func (f *livenessFixture) enqueue() api.ReleaseJob {
	f.t.Helper()
	j, err := f.s.ReleaseAction(f.ctx, f.task.ID, api.ReleaseRequest{RequestID: f.key("enqueue"), Operation: "enqueue", AgentID: f.h.ID, RunID: f.h.RunID, EntryID: f.entry})
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

// act is a deployer action on a job; the heartbeat must be fresh.
func (f *livenessFixture) act(j api.ReleaseJob, op string) api.ReleaseJob {
	f.t.Helper()
	j, err := f.s.ReleaseAction(f.ctx, f.task.ID, api.ReleaseRequest{RequestID: f.key(op), Operation: op, AgentID: f.d.ID, RunID: f.d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit})
	if err != nil {
		f.t.Fatal(op, err)
	}
	return j
}

// say posts a notice as the deployer's current run, as the runner does.
func (f *livenessFixture) say(subject string) api.Message {
	f.t.Helper()
	m, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.d.ID, RunID: f.d.RunID, RequestID: f.key("say"), Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: subject, Refs: map[string]string{"wake": "routine"}, Body: api.EnvelopeBody{Text: "runner notice"}}}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

// sweep runs one liveness sweep at the fixture clock and returns how many
// notices were posted since the last call, by it and by the ticks of advance.
func (f *livenessFixture) sweep() int {
	f.t.Helper()
	posted, err := f.s.DeployerLivenessSweep(f.ctx, f.clock)
	if err != nil {
		f.t.Fatal(err)
	}
	n := f.unseen + len(posted)
	f.unseen = 0
	return n
}

// copies are the liveness notices addressed to one agent.
func (f *livenessFixture) copies(to api.Agent) []api.Envelope {
	f.t.Helper()
	rows, err := f.s.db.Query(`SELECT envelope FROM messages WHERE task_id=? AND to_agent=? AND from_node=? AND json_extract(envelope,'$.refs.activity')='deployer_liveness' ORDER BY seq`, f.task.ID, to.ID, api.BrokerNode)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []api.Envelope
	for rows.Next() {
		var raw string
		var e api.Envelope
		if err := rows.Scan(&raw); err != nil {
			f.t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// want checks the copies each recipient holds and that nothing else in the
// project carries a liveness notice.
func (f *livenessFixture) want(at string, handler, helper int) {
	f.t.Helper()
	if h, o := len(f.copies(f.h)), len(f.copies(f.own)); h != handler || o != helper {
		f.t.Fatalf("%s: handler copies %d (want %d), owner helper copies %d (want %d)", at, h, handler, o, helper)
	}
	if n := countRows(f.t, f.s, `SELECT count(*) FROM messages WHERE task_id=? AND json_extract(envelope,'$.refs.activity')='deployer_liveness'`, f.task.ID); n != handler+helper {
		f.t.Fatalf("%s: %d liveness notices in the project, want %d", at, n, handler+helper)
	}
}

const livenessN = 10 * time.Minute

// standing enqueues a job and lets the sweep see it stand for the bound plus
// a minute, keeping the deployer's heartbeat fresh when alive is set. The
// then functions run after the sweep's first sight of the job.
func (f *livenessFixture) standing(alive bool, then ...func()) api.ReleaseJob {
	f.t.Helper()
	j := f.enqueue()
	if n := f.sweep(); n != 0 {
		f.t.Fatalf("first sight posted %d", n)
	}
	for _, fn := range then {
		fn()
	}
	f.live = alive
	f.advance(livenessN + time.Minute)
	return j
}

// a2: a silent deployer with a waiting job posts one notice per recipient. A
// merged job (its prepare, deploy and live check) waits on the deployer too.
func TestDeployerLivenessSilentPostsOnePerRecipient(t *testing.T) {
	for _, state := range []string{"verified", "claimed", "merged"} {
		t.Run(state, func(t *testing.T) {
			f := newLivenessFixture(t)
			heartbeat := ts(f.clock)
			j := f.enqueue()
			if state != "verified" {
				j = f.act(j, "claim")
			}
			if state == "merged" {
				j = f.act(j, "merged")
			}
			lastNotice := f.say("A release job is waiting for the verification host")
			if n := f.sweep(); n != 0 {
				t.Fatalf("first sight posted %d", n)
			}
			f.advance(livenessN + time.Minute)
			if n := f.sweep(); n != 2 {
				t.Fatalf("posted %d, want 2", n)
			}
			f.want("silent", 1, 1)
			for _, to := range []api.Agent{f.h, f.own} {
				e := f.copies(to)[0]
				if e.Kind != api.EnvelopeKindNotice || e.To != to.Name || e.Subject != "Deployer has gone silent while release jobs wait" {
					t.Fatalf("%s copy: %+v", to.Name, e)
				}
				if e.Refs["reason"] != "silent" || e.Refs["deployer"] != f.d.ID || e.Refs["run"] != f.d.RunID {
					t.Fatalf("%s refs: %v", to.Name, e.Refs)
				}
				for _, part := range []string{f.d.Name, f.d.ID, f.d.RunID, "Last heartbeat: " + heartbeat, fmt.Sprintf("Last notice: #%d", lastNotice.Seq), "A release job is waiting for the verification host", "oldest " + j.ID, "Bound: 10 minutes",
					"Action: run tt deployment setup for the deployer with its current run as predecessor (docs/project-deployment.md).", "Nothing was restarted or signalled."} {
					if !strings.Contains(e.Body.Text, part) {
						t.Fatalf("%s text lacks %q: %s", to.Name, part, e.Body.Text)
					}
				}
				counts := livenessCounts[state]
				if !strings.Contains(e.Body.Text, counts) {
					t.Fatalf("%s text lacks %q: %s", to.Name, counts, e.Body.Text)
				}
				if n := countRows(t, f.s, `SELECT count(*) FROM wake_jobs WHERE task_id=? AND agent_id=?`, f.task.ID, to.ID); n != 1 {
					t.Fatalf("%s wake jobs %d, want 1", to.Name, n)
				}
			}
		})
	}
}

// livenessCounts is the notice's sentence for one waiting job in each state.
var livenessCounts = map[string]string{
	"verified": "Waiting: 1 verified, 0 claimed and 0 merged release jobs;",
	"claimed":  "Waiting: 0 verified, 1 claimed and 0 merged release jobs;",
	"merged":   "Waiting: 0 verified, 0 claimed and 1 merged release jobs;",
}

// a3: a restart announced and not completed is reported though the wrapper
// still heartbeats; a completed or refused restart is not.
func TestDeployerLivenessRestartNotCompleted(t *testing.T) {
	for _, subject := range []string{"Deployer is restarting itself onto the published scripts", "Deployer is restarting itself onto the published scripts with a held job"} {
		t.Run(subject, func(t *testing.T) {
			f := newLivenessFixture(t)
			restart := f.say(subject)
			posted := ts(f.clock.Add(livenessN)) // the tick that posts, with that tick's heartbeat
			f.standing(true)
			if n := f.sweep(); n != 2 {
				t.Fatalf("posted %d, want 2", n)
			}
			f.want("restart", 1, 1)
			for _, to := range []api.Agent{f.h, f.own} {
				e := f.copies(to)[0]
				if e.Subject != "Deployer announced a restart and did not come back" || e.Refs["reason"] != "restart" || !strings.Contains(e.Body.Text, fmt.Sprintf("Restart notice: #%d.", restart.Seq)) || !strings.Contains(e.Body.Text, "Last heartbeat: "+posted) {
					t.Fatalf("%s copy: %+v", to.Name, e)
				}
			}
		})
	}
	for _, later := range []string{"Deployer now runs the published scripts", "Deployer is not claiming releases: its scripts are out of date"} {
		t.Run(later, func(t *testing.T) {
			f := newLivenessFixture(t)
			f.say("Deployer is restarting itself onto the published scripts")
			f.advance(time.Minute)
			f.say(later)
			f.standing(true)
			if n := f.sweep(); n != 0 {
				t.Fatalf("posted %d, want 0", n)
			}
			f.want("answered restart", 0, 0)
		})
	}
}

// a4: one notice per episode; it re-arms only once the deployer is seen alive.
func TestDeployerLivenessOnePerEpisodeAndRearm(t *testing.T) {
	state := func(f *livenessFixture) (st string) {
		t.Helper()
		if err := f.s.db.QueryRow(`SELECT state FROM release_deployer_liveness WHERE task_id=?`, f.task.ID).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	t.Run("silent", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.standing(false)
		if n := f.sweep(); n != 2 {
			t.Fatalf("posted %d, want 2", n)
		}
		for i := 0; i < 4; i++ {
			f.advance(livenessN)
			if n := f.sweep(); n != 0 {
				t.Fatalf("repeat sweep %d posted %d", i, n)
			}
		}
		f.want("same episode", 1, 1)
		if state(f) != "open" {
			t.Fatal(state(f))
		}
		f.beat()
		if n := f.sweep(); n != 0 || state(f) != "armed" {
			t.Fatalf("alive sweep posted %d, state %s", n, state(f))
		}
		f.advance(livenessN - time.Minute) // the heartbeat stops again
		if n := f.sweep(); n != 0 {
			t.Fatalf("below the bound after re-arm posted %d", n)
		}
		f.advance(2 * time.Minute)
		if n := f.sweep(); n != 2 {
			t.Fatalf("second silence posted %d, want 2", n)
		}
		f.want("second episode", 2, 2)
	})
	t.Run("successor run", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.standing(false)
		if n := f.sweep(); n != 2 {
			t.Fatalf("posted %d, want 2", n)
		}
		// tt deployment setup rotates the same agent onto a new run.
		f.exec(`UPDATE agents SET run_id=?,last_seen_at=? WHERE id=?`, api.NewID("run"), ts(f.clock), f.d.ID)
		if n := f.sweep(); n != 0 || state(f) != "armed" {
			t.Fatalf("successor run: posted %d, state %s", n, state(f))
		}
		f.advance(livenessN + time.Minute)
		if n := f.sweep(); n != 2 {
			t.Fatalf("silent successor run posted %d, want 2", n)
		}
		f.want("successor episode", 2, 2)
	})
	t.Run("restart", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.say("Deployer is restarting itself onto the published scripts")
		f.standing(true)
		if n := f.sweep(); n != 2 {
			t.Fatalf("posted %d, want 2", n)
		}
		// A fresh heartbeat alone does not end a restart episode.
		f.advance(livenessN)
		if n := f.sweep(); n != 0 || state(f) != "open" {
			t.Fatalf("heartbeat only: posted %d, state %s", n, state(f))
		}
		f.say("Deployer now runs the published scripts")
		if n := f.sweep(); n != 0 || state(f) != "armed" {
			t.Fatalf("now runs: posted %d, state %s", n, state(f))
		}
		f.say("Deployer is restarting itself onto the published scripts")
		f.advance(livenessN + time.Minute)
		if n := f.sweep(); n != 2 {
			t.Fatalf("second restart posted %d, want 2", n)
		}
		f.want("second episode", 2, 2)
	})
}

// a5: nothing is posted when no job waits, when only an owner hold or a
// merged or blocked job stands, in a paused project, for a retired or closed
// deployer, or below the bound.
func TestDeployerLivenessQuietCases(t *testing.T) {
	quiet := func(t *testing.T, f *livenessFixture) {
		t.Helper()
		for i := 0; i < 3; i++ {
			if n := f.sweep(); n != 0 {
				t.Fatalf("sweep %d posted %d", i, n)
			}
			f.advance(livenessN)
		}
		f.want("quiet", 0, 0)
	}
	t.Run("no job", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.advance(2 * livenessN)
		quiet(t, f)
	})
	t.Run("only held jobs", func(t *testing.T) {
		f := newLivenessFixture(t)
		j := f.enqueue()
		if _, err := f.s.ReleaseAction(f.ctx, f.task.ID, api.ReleaseRequest{RequestID: "hold", Operation: "hold", AgentID: f.h.ID, RunID: f.h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Hold: &api.ReleaseHold{Reason: "owner hold"}}); err != nil {
			t.Fatal(err)
		}
		f.advance(2 * livenessN)
		quiet(t, f)
	})
	// A merged job waits on the deployer (TestDeployerLivenessSilentPostsOnePerRecipient);
	// a blocked one is handed to a person.
	t.Run("only block jobs", func(t *testing.T) {
		f := newLivenessFixture(t)
		j := f.act(f.act(f.enqueue(), "claim"), "merged")
		if j = f.act(j, "block"); j.State != "blocked" {
			t.Fatal(j.State)
		}
		f.advance(2 * livenessN)
		quiet(t, f)
	})
	t.Run("project paused", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.standing(false, func() { f.exec(`UPDATE tasks SET pause_state='paused' WHERE id=?`, f.task.ID) })
		quiet(t, f)
	})
	for _, status := range []string{api.AgentRetired, api.AgentClosed} {
		t.Run("deployer "+status, func(t *testing.T) {
			f := newLivenessFixture(t)
			f.standing(false, func() { f.exec(`UPDATE agents SET status=? WHERE id=?`, status, f.d.ID) })
			quiet(t, f)
		})
	}
	// One deployment agent row per project may be other than closed
	// (agents_deployment_agent), so a predecessor beside a successor row is
	// closed, and a retired one is the "deployer retired" case above.
	t.Run("closed predecessor with a live successor", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.standing(true, func() {
			f.exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentClosed, f.d.ID)
			f.gap(time.Second) // the successor is the newer row
			next, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "deployer-2", Host: "fixture", Session: "deployer-2", Role: api.AgentRoleDeployment}, api.Caller{Node: "fixture", User: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			f.exec(`UPDATE agents SET status='running' WHERE id=?`, next.ID)
			f.d = next // the heartbeats are the successor's from here
		})
		quiet(t, f)
	})
	t.Run("stale by less than the bound", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.standing(true) // the job has stood for the bound
		f.live = false
		f.advance(livenessN - time.Second)
		if n := f.sweep(); n != 0 {
			t.Fatalf("posted %d", n)
		}
		f.want("below the bound", 0, 0)
	})
}

// a6: a healthy deployer on a long verification run keeps its heartbeat and
// is not reported however long its claimed job stands.
func TestDeployerLivenessLongVerificationIsQuiet(t *testing.T) {
	f := newLivenessFixture(t)
	f.live = true
	f.act(f.enqueue(), "claim")
	for elapsed := time.Duration(0); elapsed <= 3*livenessN; elapsed += time.Minute {
		f.beat()
		if n := f.sweep(); n != 0 {
			t.Fatalf("posted %d after %s", n, elapsed)
		}
		f.advance(time.Minute)
	}
	f.want("long verification", 0, 0)
}

// a7: TAILTERM_DEPLOYER_LIVENESS_MINUTES moves the bound; an invalid value is 10.
func TestDeployerLivenessBoundSetting(t *testing.T) {
	for _, invalid := range []string{"", "abc", "0", "-3", "1441", "2.5"} {
		t.Setenv("TAILTERM_DEPLOYER_LIVENESS_MINUTES", invalid)
		if got := deployerLivenessBound(); got != 10*time.Minute {
			t.Fatalf("%q gave %s", invalid, got)
		}
	}
	t.Setenv("TAILTERM_DEPLOYER_LIVENESS_MINUTES", "abc")
	f := newLivenessFixture(t)
	f.enqueue()
	f.sweep()
	f.advance(3 * time.Minute)
	if n := f.sweep(); n != 0 {
		t.Fatalf("invalid setting, 3 minutes: posted %d", n)
	}
	t.Setenv("TAILTERM_DEPLOYER_LIVENESS_MINUTES", "2")
	if got := deployerLivenessBound(); got != 2*time.Minute {
		t.Fatal(got)
	}
	g := newLivenessFixture(t)
	g.enqueue()
	g.sweep()
	g.advance(time.Minute)
	if n := g.sweep(); n != 0 {
		t.Fatalf("bound 2, 1 minute: posted %d", n)
	}
	g.advance(time.Minute)
	if n := g.sweep(); n != 2 {
		t.Fatalf("bound 2, 2 minutes: posted %d, want 2", n)
	}
	if e := g.copies(g.h)[0]; !strings.Contains(e.Body.Text, "Bound: 2 minutes") {
		t.Fatal(e.Body.Text)
	}
}

// a8: the sweep detects and wakes only: the deployer and every release job
// are unchanged by a sweep that posts.
func TestDeployerLivenessChangesNothingElse(t *testing.T) {
	f := newLivenessFixture(t)
	f.act(f.enqueue(), "claim")
	f.entry = releaseEntry(t, f.s, f.task, "second release item", "item-2")
	f.enqueue()
	f.sweep()
	f.advance(livenessN - 30*time.Second) // one tick before the notice
	snapshot := func() string {
		t.Helper()
		var out []string
		for _, query := range []string{`SELECT ` + agentCols + ` FROM agents WHERE role='deployment_agent' ORDER BY id`, `SELECT rowid,task_id,id,entry_id,state,generation,record_json FROM release_jobs ORDER BY rowid`,
			`SELECT task_id,batch_id,state FROM release_batches ORDER BY batch_id`} {
			rows, err := f.s.db.Query(query)
			if err != nil {
				t.Fatal(err)
			}
			cols, _ := rows.Columns()
			for rows.Next() {
				vals := make([]any, len(cols))
				for i := range vals {
					vals[i] = new(any)
				}
				if err := rows.Scan(vals...); err != nil {
					t.Fatal(err)
				}
				for _, v := range vals {
					out = append(out, fmt.Sprint(*v.(*any)))
				}
			}
			rows.Close()
		}
		return strings.Join(out, "\x00")
	}
	before := snapshot()
	f.advance(30 * time.Second)
	if n := f.sweep(); n != 2 {
		t.Fatalf("posted %d, want 2", n)
	}
	if after := snapshot(); after != before {
		t.Fatalf("sweep changed the deployer or a release job:\nbefore %q\nafter  %q", before, after)
	}
	var status, run, seen string
	if err := f.s.db.QueryRow(`SELECT status,run_id,last_seen_at FROM agents WHERE id=?`, f.d.ID).Scan(&status, &run, &seen); err != nil || status != api.AgentRunning || run != f.d.RunID || seen == "" {
		t.Fatal(status, run, seen, err)
	}
}

// a9: the runner subjects the rule reads are the ones the runner posts, so a
// reworded runner notice fails here rather than silencing the rule.
func TestDeployerLivenessRunnerSubjects(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "release-runner.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	runner := string(raw)
	for _, literal := range []string{
		`subject:"` + deployerRestartSubject + `"`,
		`subject:"` + deployerRestartSubject + ` with a held job"`,
		`subject:"` + deployerRestartedSubject + `"`,
		`"` + deployerNotClaimingSubject + `: it cannot compare its scripts with the published ones"`,
		`"` + deployerNotClaimingSubject + `: its scripts are out of date"`,
	} {
		if !strings.Contains(runner, literal) {
			t.Fatalf("scripts/release-runner.mjs no longer contains %s", literal)
		}
	}
}

// a12: a job counts as waiting only after it has stood in its state for the
// bound, as the sweep saw it.
func TestDeployerLivenessJobMustStandForBound(t *testing.T) {
	t.Run("verified", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.advance(2 * livenessN) // deployer silent for twice the bound
		f.enqueue()
		f.sweep()
		f.advance(livenessN - time.Minute)
		if n := f.sweep(); n != 0 {
			t.Fatalf("job seen for the bound minus a minute: posted %d", n)
		}
		f.advance(2 * time.Minute)
		if n := f.sweep(); n != 2 {
			t.Fatalf("job seen for the bound plus a minute: posted %d, want 2", n)
		}
		f.want("stood", 1, 1)
	})
	t.Run("verified then claimed", func(t *testing.T) {
		f := newLivenessFixture(t)
		j := f.enqueue()
		f.sweep()
		f.live = true
		f.advance(3 * livenessN) // long in verified, deployer alive
		if n := f.sweep(); n != 0 {
			t.Fatalf("alive deployer: posted %d", n)
		}
		f.act(j, "claim")
		f.live = false
		// A claim needs a fresh heartbeat, so a deployer cannot be silent for
		// the bound a minute after it. The heartbeat is set back to isolate
		// the job rule: silence alone does not make the new state wait.
		f.exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(f.clock.Add(-livenessN)), f.d.ID)
		if n := f.sweep(); n != 0 { // first sight of claimed
			t.Fatalf("first sight of claimed: posted %d", n)
		}
		f.advance(time.Minute)
		if n := f.sweep(); n != 0 {
			t.Fatalf("claimed for a minute: posted %d", n)
		}
		f.want("claimed for a minute", 0, 0)
		f.advance(livenessN - 2*time.Minute)
		if n := f.sweep(); n != 0 {
			t.Fatalf("claimed for the bound minus a minute: posted %d", n)
		}
		f.advance(time.Minute)
		if n := f.sweep(); n != 2 {
			t.Fatalf("claimed for the bound: posted %d, want 2", n)
		}
		f.want("claimed for the bound", 1, 1)
	})
}

// a13: each recipient gets exactly one copy per episode; one absent when the
// episode opens gets its copy when it returns, and a retired handler none.
func TestDeployerLivenessLateRecipientGetsOneCopy(t *testing.T) {
	status := func(f *livenessFixture, a api.Agent, st string) {
		f.exec(`UPDATE agents SET status=? WHERE id=?`, st, a.ID)
	}
	t.Run("handler retired", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.standing(false, func() { status(f, f.h, api.AgentRetired) })
		if n := f.sweep(); n != 1 {
			t.Fatalf("posted %d, want 1", n)
		}
		f.want("handler retired", 0, 1)
		f.advance(time.Minute)
		if n := f.sweep(); n != 0 {
			t.Fatalf("still retired: posted %d", n)
		}
		status(f, f.h, api.AgentRunning)
		if n := f.sweep(); n != 1 {
			t.Fatalf("handler resumed: posted %d, want 1", n)
		}
		f.want("handler resumed", 1, 1)
		for i := 0; i < 3; i++ {
			f.advance(livenessN)
			if n := f.sweep(); n != 0 {
				t.Fatalf("later sweep %d posted %d", i, n)
			}
		}
		f.want("later sweeps", 1, 1)
	})
	t.Run("neither present then both", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.standing(false, func() {
			status(f, f.h, api.AgentRetired)
			status(f, f.own, api.AgentExited)
		})
		if n := f.sweep(); n != 0 {
			t.Fatalf("no recipient: posted %d", n)
		}
		f.want("no recipient", 0, 0)
		status(f, f.h, api.AgentRunning)
		status(f, f.own, api.AgentRunning)
		f.advance(time.Minute)
		if n := f.sweep(); n != 2 {
			t.Fatalf("both back: posted %d, want 2", n)
		}
		f.advance(time.Minute)
		if n := f.sweep(); n != 0 {
			t.Fatalf("after both copies: posted %d", n)
		}
		f.want("both back", 1, 1)
	})
}

// The fall-back for a runner that sends no progress reports (an older one):
// with no report on record a runner that hangs while its tt wrap wrapper
// still heartbeats, and that announced no restart, is not detected, and a
// healthy one is never reported (docs/project-deployment.md, "A hung runner").
func TestDeployerLivenessHungRunnerIsNotDetected(t *testing.T) {
	f := newLivenessFixture(t)
	f.live = true
	f.enqueue() // verified and never claimed, and no progress row
	for elapsed := time.Duration(0); elapsed <= 3*livenessN; elapsed += time.Minute {
		f.beat() // the wrapper lives
		if n := f.sweep(); n != 0 {
			t.Fatalf("posted %d after %s", n, elapsed)
		}
		f.advance(time.Minute)
	}
	f.want("hung runner, live wrapper", 0, 0)
}

// b1 of review round one (a11, a12): a job's wait does not carry across a
// gap in the sweeps. While the hub is down or the project paused no
// heartbeat is recorded either, so the first tick afterwards must not report
// a deployer for a job timed across the gap; the job stands the bound again.
func TestDeployerLivenessSweepGapRestartsJobWait(t *testing.T) {
	seen := func(f *livenessFixture) *livenessFixture {
		t.Helper()
		f.enqueue()
		f.sweep()
		f.advance(time.Minute)
		return f
	}
	again := func(f *livenessFixture) {
		t.Helper()
		if n := f.sweep(); n != 0 {
			t.Fatalf("first sweep after the gap posted %d", n)
		}
		f.want("first sweep after the gap", 0, 0)
		f.advance(livenessN - 30*time.Second)
		if n := f.sweep(); n != 0 {
			t.Fatalf("one tick short of the bound after the gap: posted %d", n)
		}
		f.advance(30 * time.Second)
		if n := f.sweep(); n != 2 {
			t.Fatalf("job stood the bound again: posted %d, want 2", n)
		}
		f.want("stood the bound again", 1, 1)
	}
	t.Run("project paused and resumed", func(t *testing.T) {
		f := seen(newLivenessFixture(t))
		f.exec(`UPDATE tasks SET pause_state='paused' WHERE id=?`, f.task.ID)
		f.advance(3 * livenessN) // the hub ticks; the paused project is not swept
		f.exec(`UPDATE tasks SET pause_state='active' WHERE id=?`, f.task.ID)
		again(f) // no heartbeat since before the pause
	})
	t.Run("no sweep for longer than the bound", func(t *testing.T) {
		f := seen(newLivenessFixture(t))
		f.gap(3 * livenessN) // the hub is down
		again(f)             // no heartbeat was recorded while it was down
	})
	t.Run("healthy deployer after a hub outage", func(t *testing.T) {
		f := seen(newLivenessFixture(t))
		f.gap(3 * livenessN)
		// Broker.Start ticks at once, before the wrapper's next heartbeat.
		if n := f.sweep(); n != 0 {
			t.Fatalf("first tick after the outage posted %d", n)
		}
		f.live = true
		f.advance(3 * livenessN)
		if n := f.sweep(); n != 0 {
			t.Fatalf("healthy deployer reported after an outage: %d", n)
		}
		f.want("healthy after outage", 0, 0)
	})
	t.Run("a gap under two minutes keeps the first sight", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.enqueue()
		f.sweep()
		f.advance(livenessN - 2*time.Minute)
		f.gap(90 * time.Second) // three missed ticks
		if n := f.sweep(); n != 0 {
			t.Fatalf("before the bound: posted %d", n)
		}
		f.advance(30 * time.Second)
		if n := f.sweep(); n != 2 {
			t.Fatalf("at the bound: posted %d, want 2", n)
		}
	})
	if got := deployerLivenessSweepGap(time.Minute); got != time.Minute {
		t.Fatalf("gap with a one minute bound: %s", got)
	}
}

// report records a runner progress report at the fixture clock; the
// deployer's heartbeat must be fresh.
func (f *livenessFixture) report(p api.ReleaseProgress) api.ReleaseProgress {
	f.t.Helper()
	out, err := f.s.RecordDeployerProgress(f.ctx, f.task.ID, f.d.ID, f.d.RunID, p)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// progressRow is the project's stored progress row, empty with none.
func (f *livenessFixture) progressRow() (row struct {
	agent, run, reported, record string
	scan, code                   int64
}) {
	f.t.Helper()
	err := f.s.db.QueryRow(`SELECT agent_id,run_id,reported_at,record_json,scan_seq,code_seq FROM release_deployer_progress WHERE task_id=?`, f.task.ID).Scan(&row.agent, &row.run, &row.reported, &row.record, &row.scan, &row.code)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		f.t.Fatal(err)
	}
	return row
}

// quiet sweeps with a fresh heartbeat for three bounds and wants no notice.
func (f *livenessFixture) quiet(at string) {
	f.t.Helper()
	f.live = true
	f.advance(3 * livenessN)
	if n := f.sweep(); n != 0 {
		f.t.Fatalf("%s: posted %d, want 0", at, n)
	}
	f.want(at, 0, 0)
}

var pollProgress = api.ReleaseProgress{Phase: "poll"}

// a1: a report is stored with the hub clock for the exact deployer run; a
// wrong run or an invalid field stores nothing; a heartbeat does not count.
func TestDeployerProgressRecord(t *testing.T) {
	f := newLivenessFixture(t)
	j := f.enqueue()
	if got, err := f.s.DeployerProgress(f.ctx, f.task.ID); err != nil || got != (api.ReleaseProgress{}) {
		t.Fatalf("read with nothing on record: %+v %v", got, err)
	}
	sent := api.ReleaseProgress{JobID: j.ID, Phase: "merged", Step: "matrix", Wait: api.ReleaseProgressWait{Kind: "matrix", PID: 4242}}
	want := sent
	want.AgentID, want.RunID, want.ReportedAt = f.d.ID, f.d.RunID, ts(f.clock)
	if got := f.report(sent); got != want {
		t.Fatalf("report answered %+v, want %+v", got, want)
	}
	if got, err := f.s.DeployerProgress(f.ctx, f.task.ID); err != nil || got != want {
		t.Fatalf("read %+v %v, want %+v", got, err, want)
	}
	before := f.progressRow()
	for name, call := range map[string]func() error{
		"handler agent": func() error {
			_, err := f.s.RecordDeployerProgress(f.ctx, f.task.ID, f.h.ID, f.h.RunID, pollProgress)
			return err
		},
		"another run": func() error {
			_, err := f.s.RecordDeployerProgress(f.ctx, f.task.ID, f.d.ID, api.NewID("run"), pollProgress)
			return err
		},
		"no run": func() error {
			_, err := f.s.RecordDeployerProgress(f.ctx, f.task.ID, f.d.ID, "", pollProgress)
			return err
		},
	} {
		if err := call(); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("%s: %v, want a conflict", name, err)
		}
	}
	for name, p := range map[string]api.ReleaseProgress{
		"no phase":        {},
		"phase case":      {Phase: "Poll"},
		"long phase":      {Phase: strings.Repeat("a", 49)},
		"step":            {Phase: "poll", Step: "has space"},
		"job":             {Phase: "poll", JobID: "job-1"},
		"wait kind":       {Phase: "poll", Wait: api.ReleaseProgressWait{Kind: "nap"}},
		"negative pid":    {Phase: "poll", Wait: api.ReleaseProgressWait{Kind: "matrix", PID: -1}},
		"pid too large":   {Phase: "poll", Wait: api.ReleaseProgressWait{Kind: "matrix", PID: 1 << 31}},
		"runner's time":   {Phase: "poll", ReportedAt: ts(f.clock)},
		"runner's agent":  {Phase: "poll", AgentID: f.d.ID},
		"runner's run id": {Phase: "poll", RunID: f.d.RunID},
	} {
		if _, err := f.s.RecordDeployerProgress(f.ctx, f.task.ID, f.d.ID, f.d.RunID, p); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s: %v, want invalid", name, err)
		}
	}
	if after := f.progressRow(); after != before {
		t.Fatalf("a refused report changed the row: %+v -> %+v", before, after)
	}
	// The wrapper heartbeat refreshes last_seen_at and nothing here.
	f.clock = f.clock.Add(time.Minute)
	f.beat()
	if after := f.progressRow(); after != before {
		t.Fatalf("a heartbeat changed the row: %+v -> %+v", before, after)
	}
	// A stale heartbeat refuses the report, as it refuses a release action.
	f.clock = f.clock.Add(91 * time.Second)
	if _, err := f.s.RecordDeployerProgress(f.ctx, f.task.ID, f.d.ID, f.d.RunID, pollProgress); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale heartbeat: %v", err)
	}
	f.beat()
	if got := f.report(pollProgress); got.ReportedAt != ts(f.clock) || got.Phase != "poll" || got.JobID != "" || got.Wait != (api.ReleaseProgressWait{}) {
		t.Fatalf("second report: %+v", got)
	}
}

// a2: report and withdraw are not release actions: no receipt, no job
// change, one row however many reports, and the row is durable.
func TestDeployerProgressWritesNothingElse(t *testing.T) {
	f := newLivenessFixture(t)
	j := f.act(f.enqueue(), "claim")
	snapshot := func() string {
		t.Helper()
		var out []string
		for _, query := range []string{`SELECT task_id,request_id,payload_hash,record_json FROM release_action_receipts ORDER BY request_id`, `SELECT rowid,task_id,id,entry_id,state,generation,record_json FROM release_jobs ORDER BY rowid`} {
			rows, err := f.s.db.Query(query)
			if err != nil {
				t.Fatal(err)
			}
			cols, _ := rows.Columns()
			for rows.Next() {
				vals := make([]any, len(cols))
				for i := range vals {
					vals[i] = new(any)
				}
				if err := rows.Scan(vals...); err != nil {
					t.Fatal(err)
				}
				for _, v := range vals {
					out = append(out, fmt.Sprint(*v.(*any)))
				}
			}
			rows.Close()
		}
		return strings.Join(out, "\x00")
	}
	before, messages := snapshot(), countRows(t, f.s, `SELECT count(*) FROM messages`)
	for i := 0; i < 100; i++ {
		f.report(api.ReleaseProgress{JobID: j.ID, Phase: "poll", Wait: api.ReleaseProgressWait{Kind: "matrix", PID: int64(i + 1)}})
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM release_deployer_progress`); n != 1 {
		t.Fatalf("%d progress rows after 100 reports, want 1", n)
	}
	if _, err := f.s.WithdrawDeployerProgress(f.ctx, f.task.ID, f.d.ID, f.d.RunID); err != nil {
		t.Fatal(err)
	}
	last := f.report(api.ReleaseProgress{JobID: j.ID, Phase: "merged", Step: "matrix"})
	if after := snapshot(); after != before {
		t.Fatalf("progress changed a receipt or a release job:\nbefore %q\nafter  %q", before, after)
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM messages`); n != messages {
		t.Fatalf("progress posted %d messages", n-messages)
	}
	var file string
	if err := f.s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&file); err != nil {
		t.Fatal(err)
	}
	again, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got, err := again.DeployerProgress(f.ctx, f.task.ID); err != nil || got != last {
		t.Fatalf("reopened store read %+v %v, want %+v", got, err, last)
	}
}

// a3 (store half) and a10: a withdraw by the exact run clears the report and
// keeps the cursors, so the run is judged as one that never reported; a
// withdraw by another agent or run is refused and changes nothing.
func TestDeployerProgressWithdraw(t *testing.T) {
	f := newLivenessFixture(t)
	f.report(api.ReleaseProgress{Phase: "poll", Wait: api.ReleaseProgressWait{Kind: "inputs"}})
	before := f.progressRow()
	for name, id := range map[string][2]string{"handler": {f.h.ID, f.h.RunID}, "another run": {f.d.ID, api.NewID("run")}} {
		if _, err := f.s.WithdrawDeployerProgress(f.ctx, f.task.ID, id[0], id[1]); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("%s: %v, want a conflict", name, err)
		}
	}
	if after := f.progressRow(); after != before {
		t.Fatalf("a refused withdraw changed the row: %+v -> %+v", before, after)
	}
	got, err := f.s.WithdrawDeployerProgress(f.ctx, f.task.ID, f.d.ID, f.d.RunID)
	if err != nil || got != (api.ReleaseProgress{AgentID: f.d.ID, RunID: f.d.RunID}) {
		t.Fatalf("withdraw answered %+v %v", got, err)
	}
	// A run with nothing on record may withdraw too: an older tt cannot know.
	if _, err := f.s.WithdrawDeployerProgress(f.ctx, f.task.ID, f.d.ID, f.d.RunID); err != nil {
		t.Fatal(err)
	}
}

// a10: a withdrawn report silences the hung rule and keeps the cursors.
func TestDeployerHungWithdrawnSameRun(t *testing.T) {
	f := newLivenessFixture(t)
	f.live = true
	f.report(pollProgress)
	said := f.say("A release job is waiting for the verification host")
	f.enqueue()
	f.advance(time.Minute)
	before := f.progressRow()
	if before.scan != said.Seq || before.reported == "" {
		t.Fatalf("row before the withdraw: %+v, newest message %d", before, said.Seq)
	}
	for name, id := range map[string][2]string{"handler": {f.h.ID, f.h.RunID}, "another run": {f.d.ID, api.NewID("run")}} {
		if _, err := f.s.WithdrawDeployerProgress(f.ctx, f.task.ID, id[0], id[1]); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("%s: %v, want a conflict", name, err)
		}
		if after := f.progressRow(); after != before {
			t.Fatalf("%s: refused withdraw changed the row: %+v -> %+v", name, before, after)
		}
	}
	if _, err := f.s.WithdrawDeployerProgress(f.ctx, f.task.ID, f.d.ID, f.d.RunID); err != nil {
		t.Fatal(err)
	}
	after := f.progressRow()
	if after.reported != "" || after.record != "" || after.scan != before.scan || after.code != before.code || after.agent != f.d.ID || after.run != f.d.RunID {
		t.Fatalf("row after the withdraw: %+v (before %+v)", after, before)
	}
	f.quiet("withdrawn report")
}

// a4: a fresh heartbeat with stale progress and a standing job posts one
// hung notice per recipient, once. A lone merged job is a runner hung in its
// prepare, deploy or live check.
func TestDeployerHungPostsOnePerRecipient(t *testing.T) {
	for _, state := range []string{"verified", "claimed", "merged"} {
		t.Run(state, func(t *testing.T) {
			f := newLivenessFixture(t)
			f.live = true
			j := f.enqueue()
			if state != "verified" {
				j = f.act(j, "claim")
			}
			step := "prerequisites"
			if state == "merged" {
				j, step = f.act(j, "merged"), "deploy"
			}
			reported := f.report(api.ReleaseProgress{JobID: j.ID, Phase: "merged", Step: step}).ReportedAt
			if n := f.sweep(); n != 0 {
				t.Fatalf("first sight posted %d", n)
			}
			f.advance(livenessN - 30*time.Second)
			if n := f.sweep(); n != 0 {
				t.Fatalf("one tick before the bound posted %d", n)
			}
			f.advance(30 * time.Second)
			heartbeat := ts(f.clock)
			if n := f.sweep(); n != 2 {
				t.Fatalf("posted %d, want 2", n)
			}
			f.want("hung", 1, 1)
			for _, to := range []api.Agent{f.h, f.own} {
				e := f.copies(to)[0]
				if e.Kind != api.EnvelopeKindNotice || e.To != to.Name || e.Subject != "Deployer runner has made no progress while release jobs wait" {
					t.Fatalf("%s copy: %+v", to.Name, e)
				}
				if e.Refs["reason"] != "hung" || e.Refs["deployer"] != f.d.ID || e.Refs["run"] != f.d.RunID {
					t.Fatalf("%s refs: %v", to.Name, e.Refs)
				}
				for _, part := range []string{f.d.Name, f.d.ID, f.d.RunID, "still heartbeats", "reported no progress for 10m0s or more", "Last progress: " + reported + ", phase merged, step " + step + ", job " + j.ID + ", no recorded wait.",
					"Last heartbeat: " + heartbeat, livenessCounts[state], "oldest " + j.ID, "Action: run tt deployment setup for the deployer with its current run as predecessor (docs/project-deployment.md).", "Nothing was restarted or signalled."} {
					if !strings.Contains(e.Body.Text, part) {
						t.Fatalf("%s text lacks %q: %s", to.Name, part, e.Body.Text)
					}
				}
				if n := countRows(t, f.s, `SELECT count(*) FROM wake_jobs WHERE task_id=? AND agent_id=?`, f.task.ID, to.ID); n != 1 {
					t.Fatalf("%s wake jobs %d, want 1", to.Name, n)
				}
			}
			for i := 0; i < 4; i++ {
				f.advance(livenessN)
				if n := f.sweep(); n != 0 {
					t.Fatalf("repeat sweep %d posted %d", i, n)
				}
			}
			f.want("same episode", 1, 1)
		})
	}
}

// a5: the wait a report names decides nothing. A matrix run whose runner
// keeps reporting is quiet; one whose reports stop is reported at the bound.
func TestDeployerHungMatrixRun(t *testing.T) {
	f := newLivenessFixture(t)
	f.live = true
	j := f.act(f.enqueue(), "claim")
	matrix := api.ReleaseProgress{JobID: j.ID, Phase: "poll", Wait: api.ReleaseProgressWait{Kind: "matrix", PID: 4242}}
	f.report(matrix)
	for elapsed := time.Duration(0); elapsed < 3*livenessN; elapsed += 30 * time.Second {
		f.advance(30 * time.Second)
		f.report(matrix)
	}
	if n := f.sweep(); n != 0 {
		t.Fatalf("matrix run with continuing reports posted %d", n)
	}
	f.want("reporting matrix run", 0, 0)
	f.advance(livenessN - 30*time.Second)
	if n := f.sweep(); n != 0 {
		t.Fatalf("one tick before the bound posted %d", n)
	}
	f.advance(30 * time.Second)
	if n := f.sweep(); n != 2 {
		t.Fatalf("reports stopped: posted %d, want 2", n)
	}
	f.want("matrix run, reports stopped", 1, 1)
	for _, to := range []api.Agent{f.h, f.own} {
		if e := f.copies(to)[0]; e.Refs["reason"] != "hung" || !strings.Contains(e.Body.Text, "waiting on matrix (pid 4242).") {
			t.Fatalf("%s copy: %+v", to.Name, e)
		}
	}
}

// a6: a new report ends the episode and re-arms; a second stale period posts
// one new notice per recipient.
func TestDeployerHungRearms(t *testing.T) {
	f := newLivenessFixture(t)
	f.live = true
	f.enqueue()
	f.report(pollProgress)
	f.sweep()
	f.advance(livenessN)
	if n := f.sweep(); n != 2 {
		t.Fatalf("posted %d, want 2", n)
	}
	state := func() (st string) {
		t.Helper()
		if err := f.s.db.QueryRow(`SELECT state FROM release_deployer_liveness WHERE task_id=?`, f.task.ID).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	if state() != "open" {
		t.Fatal(state())
	}
	f.report(pollProgress)
	if n := f.sweep(); n != 0 || state() != "armed" {
		t.Fatalf("sweep after a new report posted %d, state %s", n, state())
	}
	f.advance(livenessN - 30*time.Second)
	if n := f.sweep(); n != 0 {
		t.Fatalf("before the second bound posted %d", n)
	}
	f.want("re-armed", 1, 1)
	f.advance(30 * time.Second)
	if n := f.sweep(); n != 2 {
		t.Fatalf("second stale period posted %d, want 2", n)
	}
	f.want("second episode", 2, 2)
	if e := f.copies(f.own)[1]; e.Refs["reason"] != "hung" {
		t.Fatalf("second copy: %+v", e)
	}
}

// a7: stale progress alone is not reported: a job must be standing, and for
// the liveness bound.
func TestDeployerHungNoJobIsQuiet(t *testing.T) {
	t.Run("no job", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.report(pollProgress)
		f.quiet("no job")
	})
	t.Run("released job only", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.exec(`UPDATE release_jobs SET state='released' WHERE id=?`, f.enqueue().ID)
		f.report(pollProgress)
		f.quiet("no verified or claimed job")
	})
	t.Run("job younger than the bound", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.live = true
		f.report(pollProgress)
		f.advance(2 * livenessN) // progress is long stale before any job
		f.enqueue()
		f.sweep()
		f.advance(livenessN - 30*time.Second)
		if n := f.sweep(); n != 0 {
			t.Fatalf("job stood less than the bound: posted %d", n)
		}
		f.want("young job", 0, 0)
		f.advance(30 * time.Second)
		if n := f.sweep(); n != 2 {
			t.Fatalf("job stood the bound: posted %d, want 2", n)
		}
	})
}

// A blocked job is handed to a person, so a stale report or a silent deployer
// with only a blocked job posts nothing; a merged job that leaves claimed
// starts its bound again (the sweep's first sight is per state).
func TestDeployerHungBlockedAndStateChange(t *testing.T) {
	blocked := func(f *livenessFixture) {
		t.Helper()
		j := f.act(f.act(f.enqueue(), "claim"), "merged")
		if j = f.act(j, "block"); j.State != "blocked" {
			t.Fatal(j.State)
		}
	}
	t.Run("blocked, stale report", func(t *testing.T) {
		f := newLivenessFixture(t)
		blocked(f)
		f.report(api.ReleaseProgress{Phase: "merged", Step: "deploy"})
		f.quiet("blocked job, fresh heartbeat, stale report")
	})
	t.Run("blocked, silent deployer", func(t *testing.T) {
		f := newLivenessFixture(t)
		blocked(f)
		f.advance(3 * livenessN)
		if n := f.sweep(); n != 0 {
			t.Fatalf("blocked job, silent deployer: posted %d", n)
		}
		f.want("blocked job, silent deployer", 0, 0)
	})
	t.Run("claimed then merged", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.live = true
		j := f.act(f.enqueue(), "claim")
		f.report(api.ReleaseProgress{JobID: j.ID, Phase: "merged", Step: "prerequisites"})
		f.sweep()
		f.advance(livenessN - time.Minute)
		f.act(j, "merged")
		if n := f.sweep(); n != 0 { // the first sight of merged
			t.Fatalf("claimed less than the bound: posted %d", n)
		}
		f.advance(livenessN - 30*time.Second)
		if n := f.sweep(); n != 0 {
			t.Fatalf("merged less than the bound: posted %d", n)
		}
		f.want("merged less than the bound", 0, 0)
		f.advance(30 * time.Second)
		if n := f.sweep(); n != 2 {
			t.Fatalf("merged for the bound: posted %d, want 2", n)
		}
		f.want("merged for the bound", 1, 1)
		if e := f.copies(f.h)[0]; e.Refs["reason"] != "hung" || !strings.Contains(e.Body.Text, livenessCounts["merged"]) {
			t.Fatalf("handler copy: %+v", e)
		}
	})
}

// a8: a deployer with no report of its own run on record is judged by the
// parent's rules alone: no false alarm, and the silent rule still speaks.
func TestDeployerHungFallsBack(t *testing.T) {
	old := func(f *livenessFixture) string { return ts(f.clock.Add(-time.Hour)) }
	seeds := map[string]func(f *livenessFixture){
		"no row": func(f *livenessFixture) {},
		"row of another run": func(f *livenessFixture) {
			f.exec(`INSERT INTO release_deployer_progress(task_id,agent_id,run_id,reported_at,record_json) VALUES(?,?,?,?,?)`, f.task.ID, f.d.ID, api.NewID("run"), old(f), `{"phase":"poll"}`)
		},
		"row of another agent": func(f *livenessFixture) {
			f.exec(`INSERT INTO release_deployer_progress(task_id,agent_id,run_id,reported_at,record_json) VALUES(?,?,?,?,?)`, f.task.ID, api.NewID("agt"), f.d.RunID, old(f), `{"phase":"poll"}`)
		},
		"cursor-only row": func(f *livenessFixture) {
			f.exec(`INSERT INTO release_deployer_progress(task_id,agent_id,run_id,scan_seq,code_seq) VALUES(?,?,?,0,0)`, f.task.ID, f.d.ID, f.d.RunID)
		},
	}
	for name, seed := range seeds {
		t.Run(name, func(t *testing.T) {
			f := newLivenessFixture(t)
			seed(f)
			f.enqueue()
			f.sweep()
			f.quiet(name)
		})
		t.Run(name+", silent", func(t *testing.T) {
			f := newLivenessFixture(t)
			seed(f)
			f.standing(false)
			if n := f.sweep(); n != 2 {
				t.Fatalf("posted %d, want 2", n)
			}
			f.want("silent", 1, 1)
			if e := f.copies(f.h)[0]; e.Refs["reason"] != "silent" || e.Subject != "Deployer has gone silent while release jobs wait" {
				t.Fatalf("copy: %+v", e)
			}
		})
	}
	t.Run("stale heartbeat and stale progress open one silent episode", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.report(pollProgress)
		f.standing(false)
		f.advance(3 * livenessN)
		if n := f.sweep(); n != 2 {
			t.Fatalf("posted %d, want 2", n)
		}
		f.want("one episode", 1, 1)
		for _, to := range []api.Agent{f.h, f.own} {
			if e := f.copies(to)[0]; e.Refs["reason"] != "silent" {
				t.Fatalf("%s copy: %+v", to.Name, e)
			}
		}
	})
}

// a9: a report from before the run's newest code notice does not count: the
// scripts that sent it may have been rolled back. A report after it does.
func TestDeployerHungRollbackRunnerSameRun(t *testing.T) {
	for _, notice := range []string{"Deployer now runs the published scripts", "Deployer is restarting itself onto the published scripts"} {
		t.Run(notice, func(t *testing.T) {
			f := newLivenessFixture(t)
			f.exec(`INSERT INTO release_deployer_progress(task_id,agent_id,run_id,reported_at,record_json) VALUES(?,?,?,?,?)`, f.task.ID, f.d.ID, f.d.RunID, ts(f.clock.Add(-time.Hour)), `{"phase":"poll"}`)
			f.say(notice)
			if notice != "Deployer now runs the published scripts" {
				f.advance(time.Minute)
				f.say("Deployer now runs the published scripts") // the older runner came back
			}
			f.enqueue()
			f.sweep()
			f.quiet("report older than the code notice")
			f.report(pollProgress) // a runner that reports again counts again
			f.advance(livenessN - 30*time.Second)
			if n := f.sweep(); n != 0 {
				t.Fatalf("before the bound posted %d", n)
			}
			f.advance(30 * time.Second)
			if n := f.sweep(); n != 2 {
				t.Fatalf("report after the notice, then silence: posted %d, want 2", n)
			}
			f.want("hung after the notice", 1, 1)
			if e := f.copies(f.h)[0]; e.Refs["reason"] != "hung" {
				t.Fatalf("copy: %+v", e)
			}
		})
	}
}

// a11: the sweep that posts the hung notice changes no agent and no job.
func TestDeployerHungChangesNothingElse(t *testing.T) {
	f := newLivenessFixture(t)
	f.live = true
	f.act(f.enqueue(), "claim")
	f.entry = releaseEntry(t, f.s, f.task, "second release item", "item-2")
	f.enqueue()
	f.report(pollProgress)
	f.sweep()
	f.advance(livenessN - 30*time.Second) // one tick before the notice
	snapshot := func() string {
		t.Helper()
		var out []string
		for _, query := range []string{`SELECT ` + agentCols + ` FROM agents ORDER BY id`, `SELECT rowid,task_id,id,entry_id,state,generation,record_json FROM release_jobs ORDER BY rowid`,
			`SELECT task_id,batch_id,state FROM release_batches ORDER BY batch_id`, `SELECT task_id,agent_id,run_id,reported_at,record_json FROM release_deployer_progress`} {
			rows, err := f.s.db.Query(query)
			if err != nil {
				t.Fatal(err)
			}
			cols, _ := rows.Columns()
			for rows.Next() {
				vals := make([]any, len(cols))
				for i := range vals {
					vals[i] = new(any)
				}
				if err := rows.Scan(vals...); err != nil {
					t.Fatal(err)
				}
				for _, v := range vals {
					out = append(out, fmt.Sprint(*v.(*any)))
				}
			}
			rows.Close()
		}
		return strings.Join(out, "\x00")
	}
	f.clock = f.clock.Add(30 * time.Second)
	f.beat()
	before := snapshot()
	if n := f.sweep(); n != 2 {
		t.Fatalf("posted %d, want 2", n)
	}
	if after := snapshot(); after != before {
		t.Fatalf("the hung sweep changed an agent, a release job or the report:\nbefore %q\nafter  %q", before, after)
	}
	if e := f.copies(f.own)[0]; e.Refs["reason"] != "hung" {
		t.Fatalf("copy: %+v", e)
	}
}

// a11: TAILTERM_DEPLOYER_PROGRESS_MINUTES moves the bound; an invalid value is 10.
func TestDeployerProgressBoundSetting(t *testing.T) {
	for _, invalid := range []string{"", "abc", "0", "-3", "1441", "2.5"} {
		t.Setenv("TAILTERM_DEPLOYER_PROGRESS_MINUTES", invalid)
		if got := deployerProgressBound(); got != 10*time.Minute {
			t.Fatalf("%q gave %s", invalid, got)
		}
	}
	t.Setenv("TAILTERM_DEPLOYER_PROGRESS_MINUTES", "12")
	if got := deployerProgressBound(); got != 12*time.Minute {
		t.Fatal(got)
	}
	f := newLivenessFixture(t)
	f.live = true
	f.enqueue()
	f.report(pollProgress)
	f.sweep()
	f.advance(12*time.Minute - 30*time.Second) // the job has waited past its own 10 minute bound
	if n := f.sweep(); n != 0 {
		t.Fatalf("bound 12, 11.5 minutes: posted %d", n)
	}
	f.advance(30 * time.Second)
	if n := f.sweep(); n != 2 {
		t.Fatalf("bound 12, 12 minutes: posted %d, want 2", n)
	}
	if e := f.copies(f.h)[0]; !strings.Contains(e.Body.Text, "reported no progress for 12m0s or more") {
		t.Fatal(e.Body.Text)
	}
	t.Setenv("TAILTERM_DEPLOYER_PROGRESS_MINUTES", "2")
	g := newLivenessFixture(t)
	g.live = true
	g.enqueue()
	g.sweep()
	g.advance(9 * time.Minute)
	g.report(pollProgress)
	g.advance(90 * time.Second)
	if n := g.sweep(); n != 0 {
		t.Fatalf("bound 2, 1.5 minutes: posted %d", n)
	}
	g.advance(30 * time.Second)
	if n := g.sweep(); n != 2 {
		t.Fatalf("bound 2, 2 minutes: posted %d, want 2", n)
	}
}

// a12 (r5): the restart lookup reads only the run's messages newer than the
// last sweep's and keeps the code notice it found, in the sweep's transaction.
func TestDeployerLivenessRestartScanIsBounded(t *testing.T) {
	const restart, restarted = "Deployer is restarting itself onto the published scripts", "Deployer now runs the published scripts"
	t.Run("cursor follows the run and keeps the code notice", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.live = true
		f.enqueue()
		if f.sweep(); f.progressRow().agent != "" {
			t.Fatalf("a run with no message wrote a row: %+v", f.progressRow())
		}
		plain := f.say("A release job is waiting for the verification host")
		f.advance(30 * time.Second)
		if row := f.progressRow(); row.scan != plain.Seq || row.code != 0 || row.agent != f.d.ID || row.run != f.d.RunID || row.reported != "" {
			t.Fatalf("after a plain notice: %+v, newest %d", row, plain.Seq)
		}
		idle := f.progressRow()
		f.advance(time.Minute)
		if row := f.progressRow(); row != idle {
			t.Fatalf("an idle run's sweep changed the row: %+v -> %+v", idle, row)
		}
		// A restart subject at or below the cursor is not read again.
		f.exec(`UPDATE messages SET envelope=json_set(envelope,'$.subject',?) WHERE seq=?`, restart, plain.Seq)
		f.advance(livenessN + time.Minute)
		if n := f.sweep(); n != 0 || f.progressRow() != idle {
			t.Fatalf("a row below the cursor was read: posted %d, row %+v", n, f.progressRow())
		}
		f.exec(`UPDATE messages SET envelope=json_set(envelope,'$.subject',?) WHERE seq=?`, "A release job is waiting for the verification host", plain.Seq)
		notice := f.say(restart)
		f.advance(30 * time.Second)
		later := f.say("A release job is waiting for the verification host")
		f.advance(30 * time.Second)
		if row := f.progressRow(); row.scan != later.Seq || row.code != notice.Seq {
			t.Fatalf("after a restart notice and a later plain one: %+v, want scan %d code %d", row, later.Seq, notice.Seq)
		}
		f.advance(livenessN)
		if n := f.sweep(); n != 2 {
			t.Fatalf("restart found once: posted %d, want 2", n)
		}
		if e := f.copies(f.h)[0]; e.Refs["reason"] != "restart" || !strings.Contains(e.Body.Text, fmt.Sprintf("Restart notice: #%d.", notice.Seq)) {
			t.Fatalf("copy: %+v", e)
		}
		back := f.say(restarted)
		f.advance(30 * time.Second)
		var state string
		if err := f.s.db.QueryRow(`SELECT state FROM release_deployer_liveness WHERE task_id=?`, f.task.ID).Scan(&state); err != nil || state != "armed" {
			t.Fatalf("after the now-runs notice: %q %v", state, err)
		}
		if row := f.progressRow(); row.scan != back.Seq || row.code != back.Seq {
			t.Fatalf("after the now-runs notice: %+v, want %d", row, back.Seq)
		}
		f.advance(3 * livenessN)
		if n := f.sweep(); n != 0 {
			t.Fatalf("cleared restart posted %d", n)
		}
	})
	t.Run("run change resets the cursors", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.live = true
		f.enqueue()
		f.sweep()
		f.say(restart)
		f.report(pollProgress)
		f.advance(time.Minute)
		if row := f.progressRow(); row.code == 0 || row.reported == "" {
			t.Fatalf("old run: %+v", row)
		}
		f.d.RunID = api.NewID("run") // a successor run registers
		f.exec(`UPDATE agents SET run_id=?,last_seen_at=? WHERE id=?`, f.d.RunID, ts(f.clock), f.d.ID)
		f.advance(30 * time.Second)
		if row := f.progressRow(); row.run != f.d.RunID || row.scan != 0 || row.code != 0 || row.reported != "" || row.record != "" {
			t.Fatalf("first sweep of the new run: %+v", row)
		}
		// The old run's restart notice is not the new run's: no verdict from it.
		f.advance(3 * livenessN)
		if n := f.sweep(); n != 0 {
			t.Fatalf("old run's restart carried over: posted %d", n)
		}
		own := f.say(restart + " with a held job")
		f.advance(30 * time.Second)
		if row := f.progressRow(); row.scan != own.Seq || row.code != own.Seq {
			t.Fatalf("new run's own restart: %+v, want %d", row, own.Seq)
		}
		f.advance(livenessN)
		if n := f.sweep(); n != 2 {
			t.Fatalf("new run's restart: posted %d, want 2", n)
		}
		if e := f.copies(f.own)[0]; !strings.Contains(e.Body.Text, fmt.Sprintf("Restart notice: #%d.", own.Seq)) || e.Refs["run"] != f.d.RunID {
			t.Fatalf("copy: %+v", e)
		}
	})
	t.Run("a failed sweep leaves the cursors", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.enqueue()
		f.sweep()
		heard := f.clock
		f.advance(livenessN - 30*time.Second) // silent, one tick short of the notice
		before := f.progressRow()
		// A notice the run posted at its last heartbeat, not yet swept.
		now := f.clock
		f.clock = heard
		said := f.say("A release job is waiting for the verification host")
		f.clock = now.Add(30 * time.Second)
		f.exec(`CREATE TRIGGER fail_liveness_save BEFORE INSERT ON release_deployer_liveness BEGIN SELECT RAISE(ABORT,'fixture: episode save refused'); END`)
		if _, err := f.s.DeployerLivenessSweep(f.ctx, f.clock); err == nil || !strings.Contains(err.Error(), "episode save refused") {
			t.Fatalf("sweep with a failing save: %v", err)
		}
		if row := f.progressRow(); row != before {
			t.Fatalf("a failed sweep moved the cursors: %+v -> %+v", before, row)
		}
		f.want("failed sweep", 0, 0)
		f.exec(`DROP TRIGGER fail_liveness_save`)
		if n := f.sweep(); n != 2 {
			t.Fatalf("posted %d, want 2", n)
		}
		if row := f.progressRow(); row.scan != said.Seq {
			t.Fatalf("after the sweep that committed: %+v, want scan %d", row, said.Seq)
		}
	})
}

// Evidence notes on an accepted bug, before and after its release job is
// enqueued, leave the accepted candidate claimable.
func TestReleaseStaysClaimableAfterEvidenceNote(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	var itemID string
	if err := s.db.QueryRow(`SELECT item_id FROM team_queue_entries WHERE id=?`, entry).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetWorkItem(ctx, task.ID, itemID)
	if err != nil {
		t.Fatal(err)
	}
	note := func(key string) {
		t.Helper()
		req := api.PutNarrativeArtifactRequest{RequestID: key, Namespace: "note", SourceID: key, SourceVersion: "1", Kind: "recurrence", Title: "Recurrence " + key, Provenance: "agent-note", CaptureState: "stored-content", Availability: "available", Content: "seen again", AgentID: h.ID, RunID: h.RunID}
		if _, _, err := s.PutNarrativeArtifact(ctx, task.ID, itemID, req, by); err != nil {
			t.Fatal(key, err)
		}
	}
	note("before-enqueue")
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	note("after-enqueue")
	after, err := s.GetWorkItem(ctx, task.ID, itemID)
	if err != nil || after.Revision != before.Revision || after.ScopeRevision != before.ScopeRevision {
		t.Fatalf("notes moved the bug: revision %d->%d scope %d->%d err=%v", before.Revision, after.Revision, before.ScopeRevision, after.ScopeRevision, err)
	}
	claimed, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
	if err != nil || claimed.State != "claimed" {
		t.Fatalf("claim after evidence notes: state=%q err=%v", claimed.State, err)
	}
}

// A priority edit on an accepted bug, before and after its release job is
// enqueued, advances only the item revision and leaves the job claimable.
func TestReleaseStaysClaimableAfterPriorityEdit(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	before := releaseEntryItem(t, s, task, entry)
	priority := func(item api.WorkItem, value string) api.WorkItem {
		t.Helper()
		saved, err := s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Priority: &value}, by)
		if err != nil || saved.Priority != value {
			t.Fatalf("priority %s: %+v err=%v", value, saved, err)
		}
		return saved
	}
	item := priority(before, "high")
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal("enqueue after a priority edit:", err)
	}
	item = priority(item, "low")
	if item.Revision != before.Revision+2 || item.ScopeRevision != before.ScopeRevision {
		t.Fatalf("priority edits: revision %d->%d scope %d->%d", before.Revision, item.Revision, before.ScopeRevision, item.ScopeRevision)
	}
	claimed, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
	if err != nil || claimed.State != "claimed" {
		t.Fatalf("claim after priority edits: state=%q err=%v", claimed.State, err)
	}
}

// A status edit away from done holds an accepted bug's release until the bug
// is done again; it does not invalidate the release.
func TestReleaseStaysClaimableAfterStatusEdit(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	before := releaseEntryItem(t, s, task, entry)
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	status := func(item api.WorkItem, value string) api.WorkItem {
		t.Helper()
		saved, err := s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Status: &value}, by)
		if err != nil || saved.Status != value {
			t.Fatalf("status %s: %+v err=%v", value, saved, err)
		}
		return saved
	}
	claim := func(key string) (api.ReleaseJob, error) {
		return s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: key, Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
	}
	item := status(before, "in_progress")
	if _, err = claim("claim-in-progress"); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "exact accepted SHA and current verification required") {
		t.Fatalf("claim while in progress: %v", err)
	}
	item = status(item, "done")
	if item.Revision != before.Revision+2 || item.ScopeRevision != before.ScopeRevision {
		t.Fatalf("status edits: revision %d->%d scope %d->%d", before.Revision, item.Revision, before.ScopeRevision, item.ScopeRevision)
	}
	claimed, err := claim("claim")
	if err != nil || claimed.State != "claimed" {
		t.Fatalf("claim after the status round trip: state=%q err=%v", claimed.State, err)
	}
}

// A deliberate scope change still refuses the accepted candidate's claim, and
// its enqueue, even when a new plan and receipt are later recorded at the new
// scope.
func TestReleaseRefusesClaimAfterScopeEdit(t *testing.T) {
	for _, c := range []struct {
		name            string
		newPlan, before bool // before: the scope edit precedes the enqueue
	}{{"scope edit", false, false}, {"new plan after the scope edit", true, false}, {"new plan before the enqueue", true, true}} {
		t.Run(c.name, func(t *testing.T) {
			s, task, h, d, entry := releaseFixture(t)
			ctx := context.Background()
			by := api.Caller{Node: "fixture", User: "owner"}
			item := releaseEntryItem(t, s, task, entry)
			enqueue := func() (api.ReleaseJob, error) {
				return s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
			}
			var j api.ReleaseJob
			var err error
			if !c.before {
				if j, err = enqueue(); err != nil {
					t.Fatal(err)
				}
			}
			title := "Amended scope: also cover the second pane"
			edited, err := s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Title: &title, ScopeChange: true}, by)
			if err != nil || edited.ScopeRevision != item.ScopeRevision+1 {
				t.Fatalf("deliberate scope change: %+v err=%v", edited, err)
			}
			if c.newPlan {
				seedPassingVerification(t, s, edited, candidateB)
			}
			if c.before {
				_, err = enqueue()
			} else {
				_, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
			}
			if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "exact accepted SHA and current verification required") {
				t.Fatalf("release after the scope edit: %v", err)
			}
		})
	}
}

// claimedRelease enqueues the entry's job and has the deployer claim it.
func claimedRelease(t *testing.T, s *Store, task api.Task, h, d api.Agent, entry string) api.ReleaseJob {
	t.Helper()
	ctx := context.Background()
	j, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "enqueue", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	if j, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}); err != nil {
		t.Fatal(err)
	}
	return j
}

// settledSummary is the job's summary in the settled list.
func settledSummary(t *testing.T, s *Store, task api.Task, id string) api.ReleaseSummary {
	t.Helper()
	page, err := s.ReleasesPage(context.Background(), task.ID, api.ReleaseListOptions{View: "settled"})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range page.Jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("job %s is not in the settled list: %+v", id, page.Jobs)
	return api.ReleaseSummary{}
}

// a1 (f1): a refuse carrying a refusal keeps it on the job, in its detail and
// in its settled summary; the same request replays the same job, and the same
// request ID with another refusal is a conflict.
func TestReleaseRefuseKeepsRefusal(t *testing.T) {
	ctx := context.Background()
	for _, refusal := range []api.ReleaseRefusal{
		{Step: "select", Reason: "Unknown release path: tools/newthing/a.js"},
		{Step: "prepare", Target: "hub", Reason: "python3 exit 1"},
		{Reason: "integration-conflict: 2 paths"},
	} {
		t.Run(refusal.Reason, func(t *testing.T) {
			s, task, h, d, entry := releaseFixture(t)
			j := claimedRelease(t, s, task, h, d, entry)
			want := refusal
			req := api.ReleaseRequest{RequestID: "refuse", Operation: "refuse", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Refusal: &want}
			refused, err := s.ReleaseAction(ctx, task.ID, req)
			if err != nil || refused.State != "refused" || refused.Receipt != nil || refused.Refusal == nil || *refused.Refusal != refusal {
				t.Fatalf("%+v %v", refused, err)
			}
			detail, err := s.Release(ctx, task.ID, j.ID)
			if err != nil || detail.State != "refused" || detail.Refusal == nil || *detail.Refusal != refusal {
				t.Fatalf("detail %+v %v", detail, err)
			}
			if summary := settledSummary(t, s, task, j.ID); summary.State != "refused" || summary.Refusal == nil || *summary.Refusal != refusal {
				t.Fatalf("summary %+v", summary)
			}
			row := releaseRow(t, s, task, j.ID)
			again, err := s.ReleaseAction(ctx, task.ID, req)
			if err != nil || again.ID != j.ID || again.Generation != refused.Generation || again.Refusal == nil || *again.Refusal != refusal {
				t.Fatalf("replay %+v %v", again, err)
			}
			changed := refusal
			changed.Reason = "a different tag"
			req.Refusal = &changed
			if _, err = s.ReleaseAction(ctx, task.ID, req); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "retry changed") {
				t.Fatal("a changed refusal under the same request ID", err)
			}
			if releaseRow(t, s, task, j.ID) != row {
				t.Fatal("a replay or a refused change rewrote the job")
			}
		})
	}
}

// a2 (f1): a refusal that breaks the receipt failure rules is invalid and
// leaves the job claimed at its generation, with no refusal. Fake values only.
func TestReleaseRefuseRefusesBadRefusal(t *testing.T) {
	ctx := context.Background()
	s, task, h, d, entry := releaseFixture(t)
	j := claimedRelease(t, s, task, h, d, entry)
	row := releaseRow(t, s, task, j.ID)
	cases := map[string]struct {
		op      string
		refusal api.ReleaseRefusal
	}{
		"token":               {"refuse", api.ReleaseRefusal{Reason: "SYNTHETIC_PRIVATE_TOKEN missing"}},
		"bearer":              {"refuse", api.ReleaseRefusal{Reason: "Bearer SYNTHETIC_VALUE"}},
		"password":            {"refuse", api.ReleaseRefusal{Reason: "password SYNTHETIC"}},
		"api key":             {"refuse", api.ReleaseRefusal{Reason: "an API key SYNTHETIC"}},
		"private key":         {"refuse", api.ReleaseRefusal{Step: "prepare", Target: "hub", Reason: "PRIVATE KEY synthetic"}},
		"empty":               {"refuse", api.ReleaseRefusal{Step: "select"}},
		"long":                {"refuse", api.ReleaseRefusal{Reason: strings.Repeat("a", 161)}},
		"newline":             {"refuse", api.ReleaseRefusal{Reason: "python3 exit 1\nsecond line"}},
		"character":           {"refuse", api.ReleaseRefusal{Reason: "key=value"}},
		"step":                {"refuse", api.ReleaseRefusal{Step: "Bad Step", Reason: "python3 exit 1"}},
		"target":              {"refuse", api.ReleaseRefusal{Step: "prepare", Target: "nas", Reason: "python3 exit 1"}},
		"target without step": {"refuse", api.ReleaseRefusal{Target: "hub", Reason: "python3 exit 1"}},
		"on block":            {"block", api.ReleaseRefusal{Reason: "python3 exit 1"}},
		"on claim":            {"claim", api.ReleaseRefusal{Reason: "python3 exit 1"}},
	}
	for name, c := range cases {
		refusal := c.refusal
		_, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "bad-" + strings.ReplaceAll(name, " ", "-"), Operation: c.op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Refusal: &refusal})
		if !errors.Is(err, api.ErrInvalid) {
			t.Fatal(name, err)
		}
	}
	after, err := s.Release(ctx, task.ID, j.ID)
	if err != nil || after.State != "claimed" || after.Generation != j.Generation || after.Refusal != nil || releaseRow(t, s, task, j.ID) != row {
		t.Fatalf("%+v %v", after, err)
	}
}

// a7 (f2, f3): an older runner's bare refuse still refuses, and neither the
// request nor the saved job carries a refusal key, so a saved bare refuse
// replays under its old request hash.
func TestReleaseBareRefuseKeepsOldShape(t *testing.T) {
	ctx := context.Background()
	s, task, h, d, entry := releaseFixture(t)
	j := claimedRelease(t, s, task, h, d, entry)
	req := api.ReleaseRequest{RequestID: "refuse", Operation: "refuse", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation}
	body, err := json.Marshal(req)
	if err != nil || strings.Contains(string(body), "refusal") {
		t.Fatalf("%s %v", body, err)
	}
	refused, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil || refused.State != "refused" || refused.Refusal != nil {
		t.Fatalf("%+v %v", refused, err)
	}
	if row := releaseRow(t, s, task, j.ID); strings.Contains(row, "refusal") {
		t.Fatal(row)
	}
	if summary := settledSummary(t, s, task, j.ID); summary.State != "refused" || summary.Refusal != nil {
		t.Fatalf("%+v", summary)
	}
	if again, err := s.ReleaseAction(ctx, task.ID, req); err != nil || again.ID != j.ID || again.Generation != refused.Generation {
		t.Fatalf("replay %+v %v", again, err)
	}
}

// a8 (f3): a database with the release_jobs table from before retries and a
// job refused before refusals were kept opens with that row byte-identical;
// the job reads as refused with no refusal in its detail and in the settled
// list, and a second open changes nothing.
func TestRefusedJobsSurviveMigrationWithoutRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "old refusals"}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP TABLE release_jobs;` + releaseJobsSchemaBeforeRetry); err != nil {
		t.Fatal(err)
	}
	refusedID, claimedID := api.NewID("rel"), api.NewID("rel")
	// Jobs saved before this change: a refused one and one still claimed,
	// neither with a refusal key.
	for i, j := range []api.ReleaseJob{
		{ID: refusedID, TaskID: task.ID, EntryID: api.NewID("tqe"), ItemID: api.NewID("wi"), Repository: "fixture", BaseCommit: candidateA, Commit: candidateB, State: "refused", Generation: 4, AgentID: api.NewID("agt"), RunID: api.NewID("run"), PauseGeneration: 1},
		{ID: claimedID, TaskID: task.ID, EntryID: api.NewID("tqe"), Commit: candidateB, State: "claimed", Generation: 2, PauseGeneration: 1},
	} {
		raw, err := json.Marshal(j)
		if err != nil || strings.Contains(string(raw), "refusal") {
			t.Fatalf("%s %v", raw, err)
		}
		if _, err = s.db.Exec(`INSERT INTO release_jobs(rowid,task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,?,?,?,?)`, i+3, task.ID, j.ID, j.EntryID, j.State, j.Generation, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	before := releaseRow(t, s, task, refusedID) + "\n" + releaseRow(t, s, task, claimedID)
	check := func(s *Store) {
		t.Helper()
		if after := releaseRow(t, s, task, refusedID) + "\n" + releaseRow(t, s, task, claimedID); after != before {
			t.Fatalf("rows changed\nbefore\n%s\nafter\n%s", before, after)
		}
		var shape string
		if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='release_jobs'`).Scan(&shape); err != nil || strings.Contains(shape, "UNIQUE(task_id,entry_id)") {
			t.Fatal("the table was not migrated", shape, err)
		}
		detail, err := s.Release(ctx, task.ID, refusedID)
		if err != nil || detail.State != "refused" || detail.Refusal != nil || detail.Generation != 4 {
			t.Fatalf("detail %+v %v", detail, err)
		}
		if summary := settledSummary(t, s, task, refusedID); summary.State != "refused" || summary.Refusal != nil {
			t.Fatalf("summary %+v", summary)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	check(s)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(s)
}

// Release quiet window (wi_03dc6f7d365d6528). Times are America/Los_Angeles.
var windowZone = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		panic(err)
	}
	return loc
}()

func windowAt(s string) time.Time {
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return at
}

// setWindowClock moves the store clock and the deployer's heartbeat to at.
func setWindowClock(t *testing.T, s *Store, d api.Agent, at time.Time) {
	t.Helper()
	s.now = func() time.Time { return at }
	if _, err := s.db.Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(at), d.ID); err != nil {
		t.Fatal(err)
	}
}

func windowSet(key, start, end string, targets ...string) api.ReleaseRequest {
	return api.ReleaseRequest{RequestID: key, Operation: "window-set", Window: &api.ReleaseWindow{Zone: "America/Los_Angeles", Start: start, End: end, Targets: targets, Reason: "No releases overnight"}}
}

func setNightWindow(t *testing.T, s *Store, task api.Task, targets ...string) {
	t.Helper()
	if len(targets) == 0 {
		targets = []string{"hub", "bridge", "mini"}
	}
	if _, err := s.ReleaseAction(context.Background(), task.ID, windowSet("night-window", "22:00", "07:00", targets...)); err != nil {
		t.Fatal(err)
	}
}

// releaseEntryChanged is releaseEntry whose verification plan records these
// changed paths, with the receipt rebound to the changed plan.
func releaseEntryChanged(t *testing.T, s *Store, task api.Task, title, requestID string, changed ...string) string {
	t.Helper()
	return releaseEntryOn(t, s, task, title, requestID, candidateA, changed...)
}

// releaseEntryOn is releaseEntryChanged whose plan, acceptance and queue entry
// name base as the base commit.
func releaseEntryOn(t *testing.T, s *Store, task api.Task, title, requestID, base string, changed ...string) string {
	t.Helper()
	entry := releaseEntry(t, s, task, title, requestID)
	var raw string
	if err := s.db.QueryRow(`SELECT acceptance_json FROM team_queue_entries WHERE id=?`, entry).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var a api.TeamIntegrationAcceptance
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}
	a.BaseCommit = base
	b, _ := json.Marshal(a)
	if _, err := s.db.Exec(`UPDATE team_queue_entries SET base_commit=?,acceptance_json=? WHERE id=?`, base, string(b), entry); err != nil {
		t.Fatal(err)
	}
	var item string
	if err := s.db.QueryRow(`SELECT item_id FROM team_queue_entries WHERE id=?`, entry).Scan(&item); err != nil {
		t.Fatal(err)
	}
	var plan *api.VerificationPlan
	for _, kind := range []string{"plan", "receipt"} {
		var gen int64
		var raw string
		if err := s.db.QueryRow(`SELECT generation,record_json FROM verification_records WHERE task_id=? AND item_id=? AND kind=? ORDER BY generation DESC LIMIT 1`, task.ID, item, kind).Scan(&gen, &raw); err != nil {
			t.Fatal(err)
		}
		var record api.VerificationRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			t.Fatal(err)
		}
		if kind == "plan" {
			record.Plan.Changed = changed
			record.Plan.BaseCommit = base
			plan = record.Plan
		} else {
			record.Receipt.PlanDigest = verificationDigest(*plan)
			record.Receipt.BaseCommit = base
		}
		b, _ := json.Marshal(record)
		if _, err := s.db.Exec(`UPDATE verification_records SET record_json=? WHERE task_id=? AND item_id=? AND generation=?`, string(b), task.ID, item, gen); err != nil {
			t.Fatal(err)
		}
	}
	return entry
}

func windowEnqueue(t *testing.T, s *Store, task api.Task, h api.Agent, key, entry string) api.ReleaseJob {
	t.Helper()
	j, err := s.ReleaseAction(context.Background(), task.ID, api.ReleaseRequest{RequestID: key, Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func windowPage(t *testing.T, s *Store, task api.Task) api.ReleasePage {
	t.Helper()
	page, err := s.ReleasesPage(context.Background(), task.ID, api.ReleaseListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func pageJob(t *testing.T, page api.ReleasePage, id string) api.ReleaseSummary {
	t.Helper()
	for _, j := range page.Jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("job %s not on the page", id)
	return api.ReleaseSummary{}
}

// a5, a6: wall-clock membership, the end time across DST, and the fail-closed
// path classes (a4).
func TestReleaseWindowClockAndCoverage(t *testing.T) {
	night, err := parseReleaseWindow(api.ReleaseWindow{Zone: "America/Los_Angeles", Start: "22:00", End: "07:00", Targets: []string{"hub", "bridge", "mini"}, Reason: "night"})
	if err != nil {
		t.Fatal(err)
	}
	day, err := parseReleaseWindow(api.ReleaseWindow{Zone: "America/Los_Angeles", Start: "09:00", End: "17:00", Targets: []string{"tailos"}, Reason: "day"})
	if err != nil {
		t.Fatal(err)
	}
	spring, err := parseReleaseWindow(api.ReleaseWindow{Zone: "America/Los_Angeles", Start: "02:30", End: "04:00", Targets: []string{"tailos"}, Reason: "spring"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		w    releaseWindow
		at   string
		want bool
	}{
		{night, "2026-10-18T21:59:00-07:00", false},
		{night, "2026-10-18T22:00:00-07:00", true},
		{night, "2026-10-18T23:30:00-07:00", true},
		{night, "2026-10-19T06:59:00-07:00", true},
		{night, "2026-10-19T07:00:00-07:00", false},
		// Fall back: both 01:30s are inside.
		{night, "2026-11-01T01:30:00-07:00", true},
		{night, "2026-11-01T01:30:00-08:00", true},
		{night, "2026-11-01T07:00:00-08:00", false},
		{day, "2026-10-19T08:59:00-07:00", false},
		{day, "2026-10-19T09:00:00-07:00", true},
		{day, "2026-10-19T16:59:00-07:00", true},
		{day, "2026-10-19T17:00:00-07:00", false},
		// Spring forward: 02:30 never happens; 03:15 PDT is inside.
		{spring, "2027-03-14T03:15:00-07:00", true},
		{spring, "2027-03-14T01:59:00-08:00", false},
		{spring, "2027-03-14T04:00:00-07:00", false},
	} {
		if got := releaseWindowInside(c.w, windowAt(c.at)); got != c.want {
			t.Errorf("%s-%s at %s: inside %v, want %v", c.w.Start, c.w.End, c.at, got, c.want)
		}
	}
	for _, c := range []struct{ at, until string }{
		{"2026-10-18T23:30:00-07:00", "2026-10-19T07:00:00-07:00"},
		{"2026-10-19T06:59:00-07:00", "2026-10-19T07:00:00-07:00"},
		{"2026-10-31T23:00:00-07:00", "2026-11-01T07:00:00-08:00"},
		{"2026-11-01T01:30:00-07:00", "2026-11-01T07:00:00-08:00"},
		{"2026-11-01T01:30:00-08:00", "2026-11-01T07:00:00-08:00"},
		{"2027-03-13T23:00:00-08:00", "2027-03-14T07:00:00-07:00"},
	} {
		if got := releaseWindowUntil(night, windowAt(c.at)).Format(time.RFC3339); got != c.until {
			t.Errorf("until at %s: %s, want %s", c.at, got, c.until)
		}
	}
	// The fall-back night is 10 real hours long, the spring-forward night 8.
	if d := releaseWindowUntil(night, windowAt("2026-10-31T22:00:00-07:00")).Sub(windowAt("2026-10-31T22:00:00-07:00")); d != 10*time.Hour {
		t.Errorf("fall-back night %v", d)
	}
	if d := releaseWindowUntil(night, windowAt("2027-03-13T22:00:00-08:00")).Sub(windowAt("2027-03-13T22:00:00-08:00")); d != 8*time.Hour {
		t.Errorf("spring-forward night %v", d)
	}
	withTailOS := night
	withTailOS.Targets = []string{"hub", "bridge", "mini", "tailos"}
	tailosOnly := day
	for _, c := range []struct {
		name                     string
		changed                  []string
		service, all, tailosOnly bool
	}{
		{"empty is unknown", nil, true, true, true},
		{"new folder", []string{"newdir/x"}, true, true, true},
		{"runner", []string{"scripts/release-runner.mjs"}, true, true, true},
		{"store", []string{"hub/internal/store/x.go"}, true, true, true},
		{"tt", []string{"hub/cmd/tt/deployment.go"}, true, true, true},
		{"invalid path", []string{"../x"}, true, true, true},
		{"docs and tests", []string{"docs/a.md", "hub/internal/store/a_test.go", "tests/a.mjs", "verification/x.json", "client/AGENTS.md", "LICENSE", "README.md", "hub/README.md"}, false, false, false},
		{"client", []string{"client/a.js"}, false, true, true},
		{"tailos files", []string{"wasm/a", "public/a", "shared/a.js", "package.json", "package-lock.json", "index.html", "vite.config.js"}, false, true, true},
		{"nested readme", []string{"client/README.md"}, false, true, true},
		{"nested vite", []string{"tools/vite.config.js"}, true, true, true},
		{"docs then service", []string{"docs/a.md", "hub/go.mod"}, true, true, true},
	} {
		if got := releaseWindowCovers(night, c.changed); got != c.service {
			t.Errorf("%s: hub,bridge,mini covers %v, want %v", c.name, got, c.service)
		}
		if got := releaseWindowCovers(withTailOS, c.changed); got != c.all {
			t.Errorf("%s: all targets covers %v, want %v", c.name, got, c.all)
		}
		if got := releaseWindowCovers(tailosOnly, c.changed); got != c.tailosOnly {
			t.Errorf("%s: tailos covers %v, want %v", c.name, got, c.tailosOnly)
		}
	}
}

// a1: only the owner saves the window; a request ID replays; bad values are
// refused; a clear removes it.
func TestReleaseWindowSetReplayClearAndRefusals(t *testing.T) {
	s, task, h, d, _ := releaseFixture(t)
	ctx := context.Background()
	setWindowClock(t, s, d, windowAt("2026-10-18T12:00:00-07:00"))
	if page := windowPage(t, s, task); page.Window != nil {
		t.Fatalf("window before any set %+v", page.Window)
	}
	set := windowSet("window", "22:00", "07:00", "mini", "hub", "bridge")
	if _, err := s.ReleaseAction(ctx, task.ID, set); err != nil {
		t.Fatal(err)
	}
	w := windowPage(t, s, task).Window
	if w == nil || w.Zone != "America/Los_Angeles" || w.Start != "22:00" || w.End != "07:00" || !slices.Equal(w.Targets, []string{"hub", "bridge", "mini"}) || w.Reason != "No releases overnight" || w.SetAt != ts(s.now()) || w.RequestID != "window" || w.Open || w.Until != "" {
		t.Fatalf("saved window %+v", w)
	}
	if _, err := s.ReleaseAction(ctx, task.ID, set); err != nil {
		t.Fatal("replay", err)
	}
	changed := windowSet("window", "23:00", "07:00", "hub", "bridge", "mini")
	if _, err := s.ReleaseAction(ctx, task.ID, changed); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "retry changed") {
		t.Fatal("changed retry", err)
	}
	for name, who := range map[string]api.Agent{"handler": h, "deployer": d} {
		req := windowSet("window-"+name, "21:00", "07:00", "hub", "bridge", "mini")
		req.AgentID, req.RunID = who.ID, who.RunID
		if _, err := s.ReleaseAction(ctx, task.ID, req); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "only the owner sets the release quiet window") {
			t.Fatal(name, err)
		}
	}
	for name, edit := range map[string]func(*api.ReleaseWindow){
		"no zone":           func(w *api.ReleaseWindow) { w.Zone = "" },
		"Local":             func(w *api.ReleaseWindow) { w.Zone = "Local" },
		"unknown zone":      func(w *api.ReleaseWindow) { w.Zone = "Mars/Olympus" },
		"24:00":             func(w *api.ReleaseWindow) { w.End = "24:00" },
		"7:00":              func(w *api.ReleaseWindow) { w.End = "7:00" },
		"start equals end":  func(w *api.ReleaseWindow) { w.End = "22:00" },
		"hub alone":         func(w *api.ReleaseWindow) { w.Targets = []string{"hub"} },
		"duplicate target":  func(w *api.ReleaseWindow) { w.Targets = []string{"mini", "hub", "bridge", "hub"} },
		"unknown target":    func(w *api.ReleaseWindow) { w.Targets = []string{"hub", "bridge", "mini", "deployer"} },
		"no targets":        func(w *api.ReleaseWindow) { w.Targets = nil },
		"bridge and tailos": func(w *api.ReleaseWindow) { w.Targets = []string{"bridge", "tailos"} },
		"no reason":         func(w *api.ReleaseWindow) { w.Reason = "" },
		"hub fields":        func(w *api.ReleaseWindow) { w.SetAt = "2026-10-18T00:00:00Z" },
	} {
		req := windowSet("bad-"+strings.ReplaceAll(name, " ", "-"), "22:00", "07:00", "hub", "bridge", "mini")
		edit(req.Window)
		if _, err := s.ReleaseAction(ctx, task.ID, req); !errors.Is(err, api.ErrInvalid) {
			t.Error(name, err)
		}
	}
	if w = windowPage(t, s, task).Window; w == nil || w.RequestID != "window" {
		t.Fatalf("a refused request changed the window %+v", w)
	}
	clear := api.ReleaseRequest{RequestID: "clear", Operation: "window-clear", Window: &api.ReleaseWindow{Reason: "Owner releases tonight"}}
	bad := clear
	bad.RequestID = "clear-bad"
	bad.Window = &api.ReleaseWindow{Reason: "x", Start: "22:00"}
	if _, err := s.ReleaseAction(ctx, task.ID, bad); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("clear with bounds", err)
	}
	if _, err := s.ReleaseAction(ctx, task.ID, clear); err != nil {
		t.Fatal(err)
	}
	if w = windowPage(t, s, task).Window; w != nil {
		t.Fatalf("window after clear %+v", w)
	}
	if _, err := s.ReleaseAction(ctx, task.ID, clear); err != nil {
		t.Fatal("clear replay", err)
	}
}

// a2, a7: a covered job verified inside the window is saved held with the
// window and its end; claim is refused; list and detail show both.
func TestReleaseWindowHoldsJobVerifiedInside(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	setNightWindow(t, s, task)
	setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
	store := releaseEntryChanged(t, s, task, "store change", "store-change", "hub/internal/store/x.go")
	for _, c := range []struct{ name, entry string }{{"no-changed-paths", entry}, {"store-path", store}} {
		j := windowEnqueue(t, s, task, h, "enqueue-"+c.name, c.entry)
		hold := j.Hold
		if j.State != "held" || j.Generation != 1 || hold == nil || hold.Window == nil || hold.AgentID != "" || hold.RunID != "" || len(hold.UntilItems) > 0 || hold.HeldAt != ts(s.now()) {
			t.Fatalf("%s: job %+v hold %+v", c.name, j, hold)
		}
		if w := hold.Window; w.Zone != "America/Los_Angeles" || w.Start != "22:00" || w.End != "07:00" || !slices.Equal(w.Targets, []string{"hub", "bridge", "mini"}) || w.Until != "2026-10-19T07:00:00-07:00" {
			t.Fatalf("%s: hold window %+v", c.name, w)
		}
		for _, part := range []string{"22:00-07:00", "America/Los_Angeles", "2026-10-19T07:00:00-07:00", "explicit unhold"} {
			if !strings.Contains(hold.Reason, part) {
				t.Fatalf("%s: reason %q lacks %q", c.name, hold.Reason, part)
			}
		}
		if n := len(j.HoldHistory); n != 1 || j.HoldHistory[0].Action != "hold" || j.HoldHistory[0].Cause != "window" || j.HoldHistory[0].Reason != hold.Reason || j.HoldHistory[0].Generation != 1 {
			t.Fatalf("%s: history %+v", c.name, j.HoldHistory)
		}
		if _, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-"+c.name, d, j)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "owner hold (release quiet window 22:00-07:00") {
			t.Fatal(c.name, "claim", err)
		}
		page := windowPage(t, s, task)
		if page.Window == nil || !page.Window.Open || page.Window.Until != "2026-10-19T07:00:00-07:00" {
			t.Fatalf("page window %+v", page.Window)
		}
		if listed := pageJob(t, page, j.ID); listed.State != "held" || listed.Hold == nil || listed.Hold.Window == nil || listed.Hold.Window.Until != hold.Window.Until {
			t.Fatalf("%s: listed %+v", c.name, listed)
		}
		detail, err := s.Release(ctx, task.ID, j.ID)
		if err != nil || detail.Hold == nil || detail.Hold.Window == nil || detail.HoldHistory[len(detail.HoldHistory)-1].Cause != "window" {
			t.Fatal(c.name, "detail", detail.Hold, err)
		}
		// The JSON tt deployment list and get print carry the window.
		raw, _ := json.Marshal(page)
		for _, field := range []string{`"window":{"zone":"America/Los_Angeles","start":"22:00","end":"07:00","targets":["hub","bridge","mini"],"reason":"No releases overnight","setAt":`, `"open":true,"until":"2026-10-19T07:00:00-07:00"}`, `"hold":{"reason":"release quiet window`} {
			if !strings.Contains(string(raw), field) {
				t.Fatalf("page JSON lacks %s: %s", field, raw)
			}
		}
		raw, _ = json.Marshal(detail)
		if !strings.Contains(string(raw), `"window":{"zone":"America/Los_Angeles","start":"22:00","end":"07:00","targets":["hub","bridge","mini"],"until":"2026-10-19T07:00:00-07:00"}`) || !strings.Contains(string(raw), `"cause":"window"`) {
			t.Fatalf("detail JSON %s", raw)
		}
	}
}

// a3: a job verified before the window is refused at claim inside it and
// held by the next list; a batch cannot take a covered member inside it.
func TestReleaseWindowRefusesClaimOfJobVerifiedBefore(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	setNightWindow(t, s, task)
	setWindowClock(t, s, d, windowAt("2026-10-18T21:00:00-07:00"))
	j := windowEnqueue(t, s, task, h, "enqueue", entry)
	if j.State != "verified" || j.Hold != nil {
		t.Fatalf("job verified outside the window %+v", j)
	}
	if page := windowPage(t, s, task); page.Window == nil || page.Window.Open || pageJob(t, page, j.ID).State != "verified" {
		t.Fatalf("list outside the window %+v", page)
	}
	setWindowClock(t, s, d, windowAt("2026-10-18T22:30:00-07:00"))
	_, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim", d, j))
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "release quiet window 22:00-07:00 America/Los_Angeles") || !strings.Contains(err.Error(), "until 2026-10-19T07:00:00-07:00") {
		t.Fatal("claim inside the window", err)
	}
	if saved, err := releaseLoad(ctx, s.db, task.ID, j.ID); err != nil || saved.State != "verified" || saved.Generation != j.Generation || saved.AgentID != "" {
		t.Fatal("refused claim changed the job", saved, err)
	}
	listed := pageJob(t, windowPage(t, s, task), j.ID)
	if listed.State != "held" || listed.Generation != j.Generation+1 || listed.Hold == nil || listed.Hold.Window == nil || listed.Hold.Window.Until != "2026-10-19T07:00:00-07:00" {
		t.Fatalf("swept job %+v", listed)
	}
	saved, err := releaseLoad(ctx, s.db, task.ID, j.ID)
	if err != nil || len(saved.HoldHistory) != 1 || saved.HoldHistory[0].Cause != "window" || saved.HoldHistory[0].Generation != j.Generation+1 {
		t.Fatal("swept history", saved.HoldHistory, err)
	}
	if again := pageJob(t, windowPage(t, s, task), j.ID); again.Generation != listed.Generation {
		t.Fatal("a second list wrote the held job again", again.Generation)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, claimRequest("claim-held", d, saved)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is on an owner hold") {
		t.Fatal("claim of the swept job", err)
	}

	// The lead was claimed and its batch opened at 21:50, with one member;
	// inside the window another covered member is refused.
	f := newBatchFixture(t, 2)
	setNightWindow(t, f.s, f.task)
	setWindowClock(t, f.s, f.d, windowAt("2026-10-18T21:50:00-07:00"))
	if _, err = f.deployer("batch-open", "", batchBase); err != nil {
		t.Fatal(err)
	}
	if _, err = f.deployer("batch-add", f.lead.EntryID, batchTo(1)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.deployer("batch-add", f.members[0].EntryID, batchTo(2)); err != nil {
		t.Fatal(err)
	}
	setWindowClock(t, f.s, f.d, windowAt("2026-10-18T22:30:00-07:00"))
	if _, err = f.deployer("batch-add", f.members[1].EntryID, candidateA); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "release quiet window 22:00-07:00") || !strings.Contains(err.Error(), "until 2026-10-19T07:00:00-07:00") {
		t.Fatal("batch-add of a covered member", err)
	}
	// The open batch keeps its member out of the sweep; the refused one is held.
	page := windowPage(t, f.s, f.task)
	if m := pageJob(t, page, f.members[0].ID); m.State != "verified" {
		t.Fatalf("batch member %+v", m)
	}
	if m := pageJob(t, page, f.members[1].ID); m.State != "held" {
		t.Fatalf("refused member %+v", m)
	}
	if lead := pageJob(t, page, f.lead.ID); lead.State != "claimed" {
		t.Fatalf("lead %+v", lead)
	}
}

// a4: a job with no covered target passes the window. Its base is a sync
// commit (wi_d87f594a6ae13ec9): a hand release names every covered target.
func TestReleaseWindowPassesUncoveredJobs(t *testing.T) {
	s, task, h, d, _ := releaseFixture(t)
	ctx := context.Background()
	setNightWindow(t, s, task)
	setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
	recordHandReleaseOf(t, s, task, "sync", candidateA, []string{candidateA}, "hub", "bridge", "mini")
	docs := windowEnqueue(t, s, task, h, "enqueue-docs", releaseEntryChanged(t, s, task, "docs change", "docs-change", "docs/a.md", "hub/internal/store/a_test.go"))
	client := windowEnqueue(t, s, task, h, "enqueue-client", releaseEntryChanged(t, s, task, "client change", "client-change", "client/a.js"))
	for _, j := range []api.ReleaseJob{docs, client} {
		if j.State != "verified" || j.Hold != nil {
			t.Fatalf("uncovered job held %+v", j)
		}
		if listed := pageJob(t, windowPage(t, s, task), j.ID); listed.State != "verified" || listed.Generation != j.Generation {
			t.Fatalf("uncovered job swept %+v", listed)
		}
	}
	claimed, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-docs", d, docs))
	if err != nil || claimed.State != "claimed" {
		t.Fatal("claim of a docs job", err)
	}

	// Under a window that names tailos, the client job is held.
	s, task, h, d, _ = releaseFixture(t)
	setNightWindow(t, s, task, "hub", "bridge", "mini", "tailos")
	setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
	recordHandReleaseOf(t, s, task, "sync", candidateA, []string{candidateA}, "hub", "bridge", "mini", "tailos")
	docs = windowEnqueue(t, s, task, h, "enqueue-docs", releaseEntryChanged(t, s, task, "docs change", "docs-change", "docs/a.md"))
	client = windowEnqueue(t, s, task, h, "enqueue-client", releaseEntryChanged(t, s, task, "client change", "client-change", "client/a.js"))
	if docs.State != "verified" || client.State != "held" || !slices.Equal(client.Hold.Window.Targets, []string{"hub", "bridge", "mini", "tailos"}) {
		t.Fatalf("docs %s client %s", docs.State, client.State)
	}
}

// wi_d87f594a6ae13ec9 a1 (q1, example a): a published job refused while it
// waits for a hand release holds a docs-only job at night, at enqueue (W1),
// list (W2) and claim (W3), naming that job; its supersession clears it.
func TestReleaseWindowHoldsDocsJobBehindPublishedRefusal(t *testing.T) {
	s, task, h, d, _ := releaseFixture(t)
	ctx := context.Background()
	setNightWindow(t, s, task)
	setWindowClock(t, s, d, windowAt("2026-10-18T21:00:00-07:00"))
	recordHandReleaseOf(t, s, task, "sync", candidateA, []string{candidateA}, "hub", "bridge", "mini")
	p := windowEnqueue(t, s, task, h, "enqueue-store", releaseEntryChanged(t, s, task, "store change", "store-change", "hub/internal/store/x.go"))
	var err error
	for _, op := range []string{"claim", "merged", "block"} {
		if p, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: p.ID, ExpectedGeneration: p.Generation, IntegratedCommit: p.Commit}); err != nil {
			t.Fatal(op, err)
		}
	}
	evidence := recoveryEvidence(t, s, task, h, p)
	evidence.Disposition, evidence.NoPublication, evidence.JournalState = "refuse", false, "restored"
	if _, err = s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, d.ID); err != nil {
		t.Fatal(err)
	}
	if p, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "reconcile", Operation: "reconcile", AgentID: h.ID, RunID: h.RunID, JobID: p.ID, ExpectedGeneration: p.Generation, Reconciliation: &evidence}); err != nil || p.State != "refused" || !p.Published {
		t.Fatal("published refusal", p.State, err)
	}
	if _, err = s.db.Exec(`UPDATE agents SET status='running' WHERE id=?`, d.ID); err != nil {
		t.Fatal(err)
	}
	early := windowEnqueue(t, s, task, h, "enqueue-early", releaseEntryChanged(t, s, task, "early docs", "early-docs", "docs/a.md"))
	if early.State != "verified" {
		t.Fatalf("docs job outside the window %+v", early)
	}
	cause := "job " + p.ID + " was published and is not released"

	setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
	if _, err = s.ReleaseAction(ctx, task.ID, claimRequest("claim-early", d, early)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "release quiet window 22:00-07:00") || !strings.HasSuffix(err.Error(), "; "+cause) {
		t.Fatal("W3 claim of the docs job", err)
	}
	listed := pageJob(t, windowPage(t, s, task), early.ID)
	if listed.State != "held" || listed.Generation != early.Generation+1 || listed.Hold == nil || listed.Hold.Window == nil || !strings.HasSuffix(listed.Hold.Reason, "; "+cause) {
		t.Fatalf("W2 swept docs job %+v", listed)
	}
	late := windowEnqueue(t, s, task, h, "enqueue-late", releaseEntryChanged(t, s, task, "late tests", "late-tests", "tests/a.mjs", "hub/internal/store/a_test.go"))
	if late.State != "held" || late.Hold == nil || late.Hold.Reason != "release quiet window 22:00-07:00 America/Los_Angeles covers hub, bridge, mini; held until an explicit unhold after 2026-10-19T07:00:00-07:00; "+cause || late.HoldHistory[0].Cause != "window" {
		t.Fatalf("W1 docs job %+v", late.Hold)
	}
	if err = s.ValidateReleaseDatabase(ctx); err != nil {
		t.Fatal("database with a ledger hold", err)
	}

	// The owner's hand release names hub, bridge and mini and supersedes it.
	hand := recordHandReleaseOf(t, s, task, "settle", candidateC, []string{candidateB}, "hub", "bridge", "mini")
	if p, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "supersede", Operation: "supersede", AgentID: h.ID, RunID: h.RunID, JobID: p.ID, ExpectedGeneration: p.Generation, Supersession: &api.ReleaseSupersession{ReleasedCommit: candidateC, HandReleaseID: hand.ID}}); err != nil || p.State != "superseded" {
		t.Fatal("supersede", err)
	}
	next := windowEnqueue(t, s, task, h, "enqueue-next", releaseEntryOn(t, s, task, "next docs", "next-docs", candidateC, "docs/b.md"))
	if next.State != "verified" || next.Hold != nil {
		t.Fatalf("docs job after the supersession %+v", next)
	}
	if claimed, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-next", d, next)); err != nil || claimed.State != "claimed" {
		t.Fatal("claim inside the window", err)
	}
}

// wi_d87f594a6ae13ec9 a1 (review #40218 b1): a batch publishes its members'
// changes with its lead. A docs-only lead blocked with a hub member holds a
// docs-only job on the same sync base, naming the lead.
func TestReleaseWindowHoldsDocsJobBehindBlockedBatch(t *testing.T) {
	s, task, h, d, _ := releaseFixture(t)
	ctx := context.Background()
	setNightWindow(t, s, task)
	setWindowClock(t, s, d, windowAt("2026-10-18T21:00:00-07:00"))
	recordHandReleaseOf(t, s, task, "sync", candidateC, []string{candidateC}, "hub", "bridge", "mini")
	lead := windowEnqueue(t, s, task, h, "enqueue-lead", releaseEntryOn(t, s, task, "lead docs", "lead-docs", candidateC, "docs/lead.md"))
	member := windowEnqueue(t, s, task, h, "enqueue-member", releaseEntryOn(t, s, task, "member store", "member-store", candidateC, "hub/internal/store/m.go"))
	early := windowEnqueue(t, s, task, h, "enqueue-early", releaseEntryOn(t, s, task, "early docs", "early-docs", candidateC, "docs/a.md"))
	if early.State != "verified" {
		t.Fatalf("docs job outside the window %+v", early)
	}
	act := func(key, op string, j api.ReleaseJob, entry, commit string) api.ReleaseJob {
		t.Helper()
		out, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: key, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, EntryID: entry, IntegratedCommit: commit})
		if err != nil {
			t.Fatal(key, err)
		}
		return out
	}
	lead = act("claim", "claim", lead, "", "")
	lead = act("batch-open", "batch-open", lead, "", batchBase)
	lead = act("batch-add-lead", "batch-add", lead, lead.EntryID, batchTo(1))
	lead = act("batch-add-member", "batch-add", lead, member.EntryID, candidateA)
	imported := integratedImport(lead, h, d, "import", lead.Plan.Checks)
	imported.Plan.Changed = []string{"docs/lead.md", "hub/internal/store/m.go"}
	r := passingVerification(*imported.Plan)
	imported.Verification = &r
	var err error
	if lead, err = s.ReleaseAction(ctx, task.ID, imported); err != nil {
		t.Fatal("import", err)
	}
	lead = act("merged", "merged", lead, "", candidateA)
	finish := api.ReleaseRequest{RequestID: "finish", Operation: "finish", AgentID: d.ID, RunID: d.RunID, JobID: lead.ID, ExpectedGeneration: lead.Generation}
	finish.Receipt = &api.ReleaseReceipt{Version: 1, JobID: lead.ID, Commit: candidateA, VerificationDigest: lead.VerificationDigest, Outcome: "blocked", Targets: []api.ReleaseTargetReceipt{{Target: "tailos", Release: "fixture-batch", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "failed"}}}
	if lead, err = s.ReleaseAction(ctx, task.ID, finish); err != nil || lead.State != "blocked" || !lead.Published {
		t.Fatal("blocked lead", lead.State, err)
	}
	if m, err := releaseLoad(ctx, s.db, task.ID, member.ID); err != nil || m.Published {
		t.Fatal("member published", err)
	}
	cause := "job " + lead.ID + " was published and is not released"

	// The blocked batch fences claims until the handler refuses its lead with
	// restoration evidence; the lead stays published and unreleased.
	setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
	if _, err = s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, d.ID); err != nil {
		t.Fatal(err)
	}
	evidence := recoveryEvidence(t, s, task, h, lead)
	evidence.Disposition, evidence.NoPublication, evidence.JournalState = "refuse", false, "restored"
	if lead, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "reconcile", Operation: "reconcile", AgentID: h.ID, RunID: h.RunID, JobID: lead.ID, ExpectedGeneration: lead.Generation, Reconciliation: &evidence}); err != nil || lead.State != "refused" || !lead.Published {
		t.Fatal("refused lead", lead.State, err)
	}
	if _, err = s.db.Exec(`UPDATE agents SET status='running' WHERE id=?`, d.ID); err != nil {
		t.Fatal(err)
	}
	setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
	if _, err = s.ReleaseAction(ctx, task.ID, claimRequest("claim-early", d, early)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "release quiet window 22:00-07:00") || !strings.HasSuffix(err.Error(), "; "+cause) {
		t.Fatal("W3 claim of the docs job", err)
	}
	late := windowEnqueue(t, s, task, h, "enqueue-late", releaseEntryOn(t, s, task, "late docs", "late-docs", candidateC, "docs/b.md"))
	if late.State != "held" || late.Hold == nil || !strings.HasSuffix(late.Hold.Reason, "; "+cause) {
		t.Fatalf("W1 docs job %+v", late.Hold)
	}
	if listed := pageJob(t, windowPage(t, s, task), early.ID); listed.State != "held" || !strings.HasSuffix(listed.Hold.Reason, "; "+cause) {
		t.Fatalf("W2 swept docs job %+v", listed)
	}
}

// wi_fc763af18f58129f a2 (u3): the 10-09 shape. A published job refused after
// its rollback has no receipt, and its change re-lands under another entry, so
// its own entry never settles. It holds docs jobs only until an S1 job is
// claimed after it left the fence; an S1 job released before it does not
// clear it. A rolled-back job whose revert did not commit is cleared the same
// way, and a published job no later release passes stays a cause.
func TestReleaseWindowPassesDocsJobPastRefusalAndLaterRelease(t *testing.T) {
	ctx := context.Background()
	for _, shape := range []string{"refused", "rolled_back"} {
		t.Run(shape, func(t *testing.T) {
			s, task, h, d, _ := releaseFixture(t)
			setNightWindow(t, s, task)
			setWindowClock(t, s, d, windowAt("2026-10-18T21:00:00-07:00"))
			act := func(key, op string, j api.ReleaseJob, receipt *api.ReleaseReceipt) api.ReleaseJob {
				t.Helper()
				out, err := s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: key, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit, Receipt: receipt})
				if err != nil {
					t.Fatal(key, err)
				}
				return out
			}
			receipt := func(j api.ReleaseJob, outcome string) *api.ReleaseReceipt {
				target := api.ReleaseTargetReceipt{Target: "tailos", Release: "fixture", ArtifactSHA256: strings.Repeat("a", 64), Outcome: outcome}
				r := &api.ReleaseReceipt{Version: 1, JobID: j.ID, Commit: j.IntegratedCommit, VerificationDigest: j.VerificationDigest, Outcome: outcome, Targets: []api.ReleaseTargetReceipt{target}}
				switch outcome {
				case "released":
					r.Push = &api.ReleasePush{Remote: "origin", Commit: j.IntegratedCommit, Outcome: "pushed"}
				case "rolled_back":
					r.Targets[0].Rollback = "restored"
					r.Revert = &api.ReleaseRevert{Outcome: "failed"}
				}
				return r
			}
			release := func(key string) api.ReleaseJob {
				t.Helper()
				j := windowEnqueue(t, s, task, h, "enqueue-"+key, releaseEntryChanged(t, s, task, key, key, "hub/internal/store/x.go"))
				j = act("claim-"+key, "claim", j, nil)
				j = act("merged-"+key, "merged", j, nil)
				if j = act("finish-"+key, "finish", j, receipt(j, "released")); j.State != "released" {
					t.Fatalf("release %s %+v", key, j)
				}
				return j
			}
			// An S1 release before the job: its claim proves nothing about it.
			release("earlier")
			p := windowEnqueue(t, s, task, h, "enqueue-store", releaseEntryChanged(t, s, task, "store change", "store-change", "hub/internal/store/x.go"))
			p = act("claim", "claim", p, nil)
			p = act("merged", "merged", p, nil)
			if shape == "rolled_back" {
				p = act("finish", "finish", p, receipt(p, "rolled_back"))
			} else {
				p = act("block", "block", p, nil)
				evidence := recoveryEvidence(t, s, task, h, p)
				evidence.Disposition, evidence.NoPublication, evidence.JournalState = "refuse", false, "restored"
				if _, err := s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, d.ID); err != nil {
					t.Fatal(err)
				}
				var err error
				if p, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "reconcile", Operation: "reconcile", AgentID: h.ID, RunID: h.RunID, JobID: p.ID, ExpectedGeneration: p.Generation, Reconciliation: &evidence}); err != nil {
					t.Fatal("published refusal", err)
				}
				if _, err = s.db.Exec(`UPDATE agents SET status='running' WHERE id=?`, d.ID); err != nil {
					t.Fatal(err)
				}
			}
			if p.State != shape || !p.Published || (shape == "refused" && p.Receipt != nil) || (p.Receipt != nil && p.Receipt.Revert.Outcome == "committed") {
				t.Fatalf("published %s job %+v", shape, p)
			}
			cause := "job " + p.ID + " was published and is not released"
			setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
			if held := windowEnqueue(t, s, task, h, "enqueue-before", releaseEntryOn(t, s, task, "docs before", "docs-before", candidateB, "docs/a.md")); held.State != "held" || held.Hold == nil || !strings.HasSuffix(held.Hold.Reason, "; "+cause) {
				t.Fatalf("docs job behind the %s job %+v", shape, held.Hold)
			}

			// The change re-lands under another entry, outside the window.
			setWindowClock(t, s, d, windowAt("2026-10-19T08:00:00-07:00"))
			relanded := release("relanded")
			if p, err := releaseLoad(ctx, s.db, task.ID, p.ID); err != nil || p.State != shape || p.EntryID == relanded.EntryID {
				t.Fatal("the old job is untouched", p.State, err)
			}
			setWindowClock(t, s, d, windowAt("2026-10-19T23:30:00-07:00"))
			ledger, err := releaseWindowLedgerLoad(ctx, s.db, task.ID, releaseWindow{ReleaseWindow: api.ReleaseWindow{Targets: []string{"hub", "bridge", "mini"}}})
			if err != nil || ledger.cause != "" || !ledger.sync[candidateB] {
				t.Fatal("ledger after the later release", ledger, err)
			}
			docs := windowEnqueue(t, s, task, h, "enqueue-after", releaseEntryOn(t, s, task, "docs after", "docs-after", candidateB, "docs/b.md"))
			if docs.State != "verified" || docs.Hold != nil {
				t.Fatalf("docs job after the later release %+v", docs)
			}
			if claimed, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-after", d, docs)); err != nil || claimed.State != "claimed" {
				t.Fatal("claim inside the window", err)
			}
		})
	}
}

// windowRefusalFixture drives published jobs to the shapes the window ledger
// reads: released S1 jobs, a published block with or without a revert, and the
// handler's refuse reconcile, each at the store clock the caller sets.
type windowRefusalFixture struct {
	t    *testing.T
	s    *Store
	task api.Task
	h, d api.Agent
}

func newWindowRefusalFixture(t *testing.T) *windowRefusalFixture {
	s, task, h, d, _ := releaseFixture(t)
	setNightWindow(t, s, task)
	return &windowRefusalFixture{t: t, s: s, task: task, h: h, d: d}
}

func (f *windowRefusalFixture) clock(at string) { setWindowClock(f.t, f.s, f.d, windowAt(at)) }

func (f *windowRefusalFixture) do(key, op string, j api.ReleaseJob, set func(*api.ReleaseRequest)) (api.ReleaseJob, error) {
	req := api.ReleaseRequest{RequestID: key, Operation: op, AgentID: f.d.ID, RunID: f.d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: j.Commit}
	if set != nil {
		set(&req)
	}
	return f.s.ReleaseAction(context.Background(), f.task.ID, req)
}

func (f *windowRefusalFixture) act(key, op string, j api.ReleaseJob, set func(*api.ReleaseRequest)) api.ReleaseJob {
	f.t.Helper()
	out, err := f.do(key, op, j, set)
	if err != nil {
		f.t.Fatal(key, err)
	}
	return out
}

// published is a store change claimed and merged, so tasks-hub carries it.
func (f *windowRefusalFixture) published(key string) api.ReleaseJob {
	f.t.Helper()
	j := windowEnqueue(f.t, f.s, f.task, f.h, "enqueue-"+key, releaseEntryChanged(f.t, f.s, f.task, key, key, "hub/internal/store/x.go"))
	j = f.act("claim-"+key, "claim", j, nil)
	return f.act("merged-"+key, "merged", j, nil)
}

// release is an S1 job: released with its push at the current clock.
func (f *windowRefusalFixture) release(key string) api.ReleaseJob {
	f.t.Helper()
	j := f.published(key)
	target := api.ReleaseTargetReceipt{Target: "tailos", Release: "fixture", ArtifactSHA256: strings.Repeat("a", 64), Outcome: "released"}
	receipt := &api.ReleaseReceipt{Version: 1, JobID: j.ID, Commit: j.IntegratedCommit, VerificationDigest: j.VerificationDigest, Outcome: "released", Targets: []api.ReleaseTargetReceipt{target}, Push: &api.ReleasePush{Remote: "origin", Commit: j.IntegratedCommit, Outcome: "pushed"}}
	if j = f.act("finish-"+key, "finish", j, func(r *api.ReleaseRequest) { r.Receipt = receipt }); j.State != "released" {
		f.t.Fatalf("release %s %+v", key, j)
	}
	return j
}

// refuse is the handler's refuse reconcile of a blocked published job, with
// the incident observed at observed (RFC3339) or now when empty.
func (f *windowRefusalFixture) refuse(key string, j api.ReleaseJob, observed string) api.ReleaseJob {
	f.t.Helper()
	evidence := recoveryEvidence(f.t, f.s, f.task, f.h, j)
	evidence.Disposition, evidence.NoPublication, evidence.JournalState = "refuse", false, "restored"
	if observed != "" {
		evidence.ObservedAt = ts(windowAt(observed))
	}
	if _, err := f.s.db.Exec(`UPDATE agents SET status='exited' WHERE id=?`, f.d.ID); err != nil {
		f.t.Fatal(err)
	}
	out, err := f.s.ReleaseAction(context.Background(), f.task.ID, api.ReleaseRequest{RequestID: key, Operation: "reconcile", AgentID: f.h.ID, RunID: f.h.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, Reconciliation: &evidence})
	if err != nil || out.State != "refused" || !out.Published {
		f.t.Fatal("published refusal", out.State, err)
	}
	if _, err = f.s.db.Exec(`UPDATE agents SET status='running' WHERE id=?`, f.d.ID); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *windowRefusalFixture) cause() string {
	f.t.Helper()
	ledger, err := releaseWindowLedgerLoad(context.Background(), f.s.db, f.task.ID, releaseWindow{ReleaseWindow: api.ReleaseWindow{Targets: []string{"hub", "bridge", "mini"}}})
	if err != nil {
		f.t.Fatal(err)
	}
	return ledger.cause
}

func (f *windowRefusalFixture) raw(id string) string {
	f.t.Helper()
	var raw string
	if err := f.s.db.QueryRow(`SELECT record_json FROM release_jobs WHERE task_id=? AND id=?`, f.task.ID, id).Scan(&raw); err != nil {
		f.t.Fatal(err)
	}
	return raw
}

// wi_816228d539a80c48 a1/a2 (r1, r2): the runner's block of a published job
// with no receipt carries its tasks-hub revert; the hub saves it on the job,
// a later refuse reconcile keeps it and records the hub time, and the window
// ledger reads it first: a committed revert is no cause, a failed one is a
// cause until a later S1 release. A bare block, as an older runner sends it,
// is saved as before with no revert, which stays unknown. A revert is
// accepted on a block only, checked like a receipt's, and part of the
// request's retry identity.
func TestReleaseWindowReadsRevertRecordedOnBlock(t *testing.T) {
	commit := strings.Repeat("e", 40)
	for _, shape := range []string{"committed", "failed", "bare"} {
		t.Run(shape, func(t *testing.T) {
			f := newWindowRefusalFixture(t)
			f.clock("2026-10-18T20:00:00-07:00")
			f.release("earlier")
			f.clock("2026-10-18T21:00:00-07:00")
			p := f.published("store")
			var revert *api.ReleaseRevert
			switch shape {
			case "committed":
				revert = &api.ReleaseRevert{Outcome: "committed", Commit: commit, BugRequestID: p.ID + "-revert-bug"}
			case "failed":
				revert = &api.ReleaseRevert{Outcome: "failed"}
			}
			withRevert := func(v *api.ReleaseRevert) func(*api.ReleaseRequest) {
				return func(r *api.ReleaseRequest) { r.Revert = v }
			}
			if shape == "committed" {
				for name, bad := range map[string]api.ReleaseRevert{
					"outcome":          {Outcome: "reverted", Commit: commit},
					"committed bare":   {Outcome: "committed"},
					"commit":           {Outcome: "committed", Commit: "abc"},
					"failed commit":    {Outcome: "failed", Commit: "xyz"},
					"bug request":      {Outcome: "failed", BugRequestID: "two words"},
					"long bug request": {Outcome: "failed", BugRequestID: strings.Repeat("b", 129)},
				} {
					if _, err := f.do("block-bad-"+strings.ReplaceAll(name, " ", "-"), "block", p, withRevert(&bad)); !errors.Is(err, api.ErrInvalid) {
						t.Fatalf("invalid revert %s: %v", name, err)
					}
				}
				for _, op := range []string{"claim", "merged", "finish", "refuse", "unclaim"} {
					if _, err := f.do("revert-on-"+op, op, p, withRevert(revert)); !errors.Is(err, api.ErrInvalid) {
						t.Fatalf("revert on %s: %v", op, err)
					}
				}
			}
			p = f.act("block", "block", p, withRevert(revert))
			if p.State != "blocked" || !p.Published || p.Receipt != nil || !reflect.DeepEqual(p.Revert, revert) {
				t.Fatalf("blocked %s job %+v revert %+v", shape, p, p.Revert)
			}
			if shape == "bare" && strings.Contains(f.raw(p.ID), `"revert"`) {
				t.Fatalf("a bare block saved a revert: %s", f.raw(p.ID))
			}
			if shape == "committed" {
				if replay, err := f.do("block", "block", api.ReleaseJob{ID: p.ID, Commit: p.Commit, Generation: p.Generation - 1}, withRevert(revert)); err != nil || !reflect.DeepEqual(replay.Revert, revert) {
					t.Fatal("exact block replay", replay.Revert, err)
				}
				changed := *revert
				changed.Outcome, changed.Commit = "failed", ""
				if _, err := f.do("block", "block", api.ReleaseJob{ID: p.ID, Commit: p.Commit, Generation: p.Generation - 1}, withRevert(&changed)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "retry changed") {
					t.Fatal("block retried with another revert", err)
				}
			}
			cause := "job " + p.ID + " was published and is not released"
			want := cause
			if shape == "committed" {
				want = ""
			}
			if got := f.cause(); got != want {
				t.Fatalf("ledger cause of the blocked %s job %q, want %q", shape, got, want)
			}

			f.clock("2026-10-18T21:30:00-07:00")
			p = f.refuse("reconcile", p, "")
			if !reflect.DeepEqual(p.Revert, revert) || p.SettledAt != ts(windowAt("2026-10-18T21:30:00-07:00")) {
				t.Fatalf("refused %s job revert %+v settledAt %q", shape, p.Revert, p.SettledAt)
			}
			if saved, err := releaseLoad(context.Background(), f.s.db, f.task.ID, p.ID); err != nil || !reflect.DeepEqual(saved.Revert, revert) || saved.SettledAt != p.SettledAt {
				t.Fatal("saved refused job", saved.Revert, saved.SettledAt, err)
			}
			// The S1 release before the refusal clears nothing.
			if got := f.cause(); got != want {
				t.Fatalf("ledger cause of the refused %s job %q, want %q", shape, got, want)
			}
			f.clock("2026-10-19T08:00:00-07:00")
			f.release("relanded")
			if got := f.cause(); got != "" {
				t.Fatalf("ledger cause after a later release %q", got)
			}
		})
	}
}

// wi_e9b15b14264c30fe e1/e2: the runner revert-journals a block only after it
// published, so a block with a revert on a job the hub still holds as claimed
// (its merged never saved) marks the job published, and the window ledger
// reads it like any published job: a failed revert is a cause, a committed one
// is not. A bare block of a claimed job leaves it unpublished, as before.
func TestReleaseWindowSeesRevertOnBlockOfClaimedJob(t *testing.T) {
	commit := strings.Repeat("e", 40)
	for _, shape := range []string{"committed", "failed", "bare"} {
		t.Run(shape, func(t *testing.T) {
			f := newWindowRefusalFixture(t)
			f.clock("2026-10-18T21:00:00-07:00")
			j := windowEnqueue(t, f.s, f.task, f.h, "enqueue-store", releaseEntryChanged(t, f.s, f.task, "store", "store", "hub/internal/store/x.go"))
			j = f.act("claim-store", "claim", j, nil)
			if j.State != "claimed" || j.Published {
				t.Fatalf("claimed job %+v", j)
			}
			var revert *api.ReleaseRevert
			switch shape {
			case "committed":
				revert = &api.ReleaseRevert{Outcome: "committed", Commit: commit, BugRequestID: j.ID + "-revert-bug"}
			case "failed":
				revert = &api.ReleaseRevert{Outcome: "failed"}
			}
			j = f.act("block", "block", j, func(r *api.ReleaseRequest) { r.Revert = revert })
			if j.State != "blocked" || j.Published != (revert != nil) || !reflect.DeepEqual(j.Revert, revert) {
				t.Fatalf("blocked %s job published=%v %+v revert %+v", shape, j.Published, j, j.Revert)
			}
			if saved, err := releaseLoad(context.Background(), f.s.db, f.task.ID, j.ID); err != nil || saved.Published != j.Published {
				t.Fatal("saved blocked job", saved.Published, err)
			}
			want := ""
			if shape == "failed" {
				want = "job " + j.ID + " was published and is not released"
			}
			if got := f.cause(); got != want {
				t.Fatalf("ledger cause of the blocked %s job %q, want %q", shape, got, want)
			}
		})
	}
}

// wi_816228d539a80c48 a5 (r5): the later-release proof compares parsed times,
// never rows. A refusal and an S1 job's rows are reordered in the isolated
// database, as set-aside moves job rows and VACUUM may renumber the receipts:
// an S1 release settled after the refusal still clears the refused job, and
// one settled before it still leaves it a cause. A refusal saved before
// refused jobs carried settledAt is ordered by its refuse reconcile's
// observedAt; with neither time it stays a cause.
func TestReleaseWindowOrdersRefusalByTimeNotRowid(t *testing.T) {
	ctx := context.Background()
	// scramble moves the job's row before or after every other job and
	// reverses the action receipts' rows.
	scramble := func(f *windowRefusalFixture, id string, first bool) {
		t.Helper()
		move := `UPDATE release_jobs SET rowid=(SELECT MAX(rowid)+1 FROM release_jobs) WHERE task_id=? AND id=?`
		if first {
			move = `UPDATE release_jobs SET rowid=(SELECT MIN(rowid)-1 FROM release_jobs) WHERE task_id=? AND id=?`
		}
		for _, stmt := range []string{move, `DROP TRIGGER IF EXISTS release_receipt_no_update`} {
			if _, err := f.s.db.ExecContext(ctx, stmt, f.task.ID, id); err != nil {
				t.Fatal(stmt, err)
			}
		}
		for _, stmt := range []string{`UPDATE release_action_receipts SET rowid=-rowid`, `UPDATE release_action_receipts SET rowid=1000000+rowid`} {
			if _, err := f.s.db.ExecContext(ctx, stmt); err != nil {
				t.Fatal(stmt, err)
			}
		}
	}
	rowBefore := func(f *windowRefusalFixture, a, b string) bool {
		t.Helper()
		var before bool
		if err := f.s.db.QueryRowContext(ctx, `SELECT (SELECT rowid FROM release_jobs WHERE id=?)<(SELECT rowid FROM release_jobs WHERE id=?)`, a, b).Scan(&before); err != nil {
			t.Fatal(err)
		}
		return before
	}
	refused := func(f *windowRefusalFixture, observed string) api.ReleaseJob {
		t.Helper()
		f.clock("2026-10-18T21:00:00-07:00")
		p := f.published("store")
		p = f.act("block", "block", p, nil)
		f.clock("2026-10-18T21:30:00-07:00")
		return f.refuse("reconcile", p, observed)
	}

	t.Run("later release whose rows come first", func(t *testing.T) {
		f := newWindowRefusalFixture(t)
		p := refused(f, "")
		f.clock("2026-10-19T08:00:00-07:00")
		later := f.release("relanded")
		scramble(f, later.ID, true)
		if !rowBefore(f, later.ID, p.ID) {
			t.Fatal("the later release's row was not moved first")
		}
		if got := f.cause(); got != "" {
			t.Fatalf("a release settled after the refusal leaves cause %q", got)
		}
	})
	t.Run("earlier release whose rows come last", func(t *testing.T) {
		f := newWindowRefusalFixture(t)
		f.clock("2026-10-18T20:00:00-07:00")
		earlier := f.release("earlier")
		p := refused(f, "")
		scramble(f, earlier.ID, false)
		if !rowBefore(f, p.ID, earlier.ID) {
			t.Fatal("the earlier release's row was not moved last")
		}
		if got, want := f.cause(), "job "+p.ID+" was published and is not released"; got != want {
			t.Fatalf("a release settled before the refusal: cause %q, want %q", got, want)
		}
	})
	t.Run("refusal saved without settledAt", func(t *testing.T) {
		f := newWindowRefusalFixture(t)
		f.clock("2026-10-18T20:00:00-07:00")
		earlier := f.release("earlier")
		p := refused(f, "2026-10-18T21:15:00-07:00")
		if _, err := f.s.db.ExecContext(ctx, `UPDATE release_jobs SET record_json=json_remove(record_json,'$.settledAt') WHERE task_id=? AND id=?`, f.task.ID, p.ID); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(f.raw(p.ID), `"settledAt"`) {
			t.Fatal("legacy refusal still has settledAt")
		}
		scramble(f, earlier.ID, false)
		cause := "job " + p.ID + " was published and is not released"
		if got := f.cause(); got != cause {
			t.Fatalf("legacy refusal after an earlier release: cause %q", got)
		}
		f.clock("2026-10-19T08:00:00-07:00")
		later := f.release("relanded")
		scramble(f, later.ID, true)
		if got := f.cause(); got != "" {
			t.Fatalf("legacy refusal before a later release: cause %q", got)
		}
		// With no time at all the job stays a cause.
		if _, err := f.s.db.ExecContext(ctx, `UPDATE release_jobs SET record_json=json_set(record_json,'$.reconciliations[#-1].observedAt','') WHERE task_id=? AND id=?`, f.task.ID, p.ID); err != nil {
			t.Fatal(err)
		}
		if got := f.cause(); got != cause {
			t.Fatalf("refusal with no parsed time: cause %q", got)
		}
	})
}

// wi_d87f594a6ae13ec9 a2 (q1, example b): a docs-only job whose base is an
// owner commit with no hand-release record is held and its claim refused; the
// owner's record of that commit lets the next one release. The window hold is
// lifted by the usual unhold after the window ends.
func TestReleaseWindowHoldsDocsJobOnUnrecordedBase(t *testing.T) {
	s, task, h, d, _ := releaseFixture(t)
	ctx := context.Background()
	setNightWindow(t, s, task)
	setWindowClock(t, s, d, windowAt("2026-10-18T21:00:00-07:00"))
	early := windowEnqueue(t, s, task, h, "enqueue-early", releaseEntryOn(t, s, task, "early docs", "early-docs", candidateC, "docs/a.md"))
	if early.State != "verified" || pageJob(t, windowPage(t, s, task), early.ID).State != "verified" {
		t.Fatalf("docs job outside the window %+v", early)
	}
	cause := "base cccccccccccc is not a recorded release commit"

	setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
	_, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-early", d, early))
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "until 2026-10-19T07:00:00-07:00") || !strings.HasSuffix(err.Error(), "; "+cause) {
		t.Fatal("W3 claim of the docs job", err)
	}
	late := windowEnqueue(t, s, task, h, "enqueue-late", releaseEntryOn(t, s, task, "late docs", "late-docs", candidateC, "docs/b.md"))
	if late.State != "held" || late.Hold == nil || late.Hold.Window == nil || !strings.HasSuffix(late.Hold.Reason, "; "+cause) {
		t.Fatalf("W1 docs job %+v", late.Hold)
	}
	if listed := pageJob(t, windowPage(t, s, task), early.ID); listed.State != "held" || !strings.HasSuffix(listed.Hold.Reason, "; "+cause) {
		t.Fatalf("W2 swept docs job %+v", listed)
	}

	recordHandReleaseOf(t, s, task, "owner", candidateC, []string{candidateC}, "hub", "bridge", "mini")
	next := windowEnqueue(t, s, task, h, "enqueue-next", releaseEntryOn(t, s, task, "next docs", "next-docs", candidateC, "docs/c.md"))
	if next.State != "verified" {
		t.Fatalf("docs job on a recorded base %+v", next)
	}
	if claimed, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-next", d, next)); err != nil || claimed.State != "claimed" {
		t.Fatal("claim inside the window", err)
	}

	setWindowClock(t, s, d, windowAt("2026-10-19T07:00:00-07:00"))
	free, err := s.ReleaseAction(ctx, task.ID, unholdRequest("unhold", h, late, "Morning"))
	if err != nil || free.State != "verified" || free.Hold != nil || free.HoldHistory[len(free.HoldHistory)-1].Cause != "handler" {
		t.Fatal("unhold after the end", free, err)
	}
}

// wi_d87f594a6ae13ec9 a3 (U2): a hand release naming only hub, recorded after
// the newest sync record, holds a docs-only job; the next pushed release
// clears it. Times compare as times: 04:00:00.1Z sorts before 04:00:00Z as
// text.
func TestReleaseWindowHoldsDocsJobAfterPartialHandRelease(t *testing.T) {
	s, task, h, d, _ := releaseFixture(t)
	ctx := context.Background()
	push := func(j api.ReleaseJob) *api.ReleasePush {
		return &api.ReleasePush{Remote: "origin", Commit: j.Commit, Outcome: "pushed"}
	}
	setNightWindow(t, s, task)
	setWindowClock(t, s, d, windowAt("2026-10-18T21:00:00-07:00"))
	first := windowEnqueue(t, s, task, h, "enqueue-first", releaseEntryChanged(t, s, task, "first store", "first-store", "hub/internal/store/x.go"))
	if first = releaseThroughPush(t, s, task, d, first, push(first)); first.SettledAt != "2026-10-19T04:00:00Z" {
		t.Fatal("settledAt", first.SettledAt)
	}
	setWindowClock(t, s, d, windowAt("2026-10-18T21:00:00.1-07:00"))
	hand := recordHandRelease(t, s, task, "hub-only", candidateC, []string{candidateC})
	if hand.CreatedAt != "2026-10-19T04:00:00.1Z" || hand.CreatedAt > first.SettledAt {
		t.Fatal("fixture times", hand.CreatedAt, first.SettledAt)
	}

	setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
	docs := windowEnqueue(t, s, task, h, "enqueue-docs", releaseEntryOn(t, s, task, "docs", "docs", candidateB, "docs/a.md"))
	if docs.State != "held" || !strings.HasSuffix(docs.Hold.Reason, "; hand release "+hand.ID+" names only hub") {
		t.Fatalf("docs job after a partial hand release %+v", docs.Hold)
	}

	setWindowClock(t, s, d, windowAt("2026-10-19T07:30:00-07:00"))
	second := windowEnqueue(t, s, task, h, "enqueue-second", releaseEntryChanged(t, s, task, "second store", "second-store", "hub/internal/store/y.go"))
	releaseThroughPush(t, s, task, d, second, push(second))
	setWindowClock(t, s, d, windowAt("2026-10-19T23:30:00-07:00"))
	next := windowEnqueue(t, s, task, h, "enqueue-next", releaseEntryOn(t, s, task, "next docs", "next-docs", candidateB, "docs/b.md"))
	if next.State != "verified" {
		t.Fatalf("docs job after a pushed release %+v", next)
	}
	if claimed, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-next", d, next)); err != nil || claimed.State != "claimed" {
		t.Fatal("claim inside the window", err)
	}
}

// wi_d87f594a6ae13ec9 a4 (q2): a docs- or tests-only job based on a pushed
// release, or on a hand release naming every covered target, releases at
// night. A no-change finish has no push and makes no sync commit.
func TestReleaseWindowPassesDocsJobOnSyncCommit(t *testing.T) {
	for _, c := range []string{"pushed", "hand", "no-push"} {
		s, task, h, d, _ := releaseFixture(t)
		ctx := context.Background()
		setNightWindow(t, s, task)
		setWindowClock(t, s, d, windowAt("2026-10-18T21:00:00-07:00"))
		base := candidateB
		switch c {
		case "pushed", "no-push":
			j := windowEnqueue(t, s, task, h, "enqueue-store", releaseEntryChanged(t, s, task, "store change", "store-change", "hub/internal/store/x.go"))
			var push *api.ReleasePush
			if c == "pushed" {
				push = &api.ReleasePush{Remote: "origin", Commit: j.Commit, Outcome: "pushed"}
			}
			releaseThroughPush(t, s, task, d, j, push)
		case "hand":
			base = candidateC
			recordHandReleaseOf(t, s, task, "full", candidateC, []string{candidateC}, "hub", "bridge", "mini")
		}
		setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
		docs := windowEnqueue(t, s, task, h, "enqueue-docs", releaseEntryOn(t, s, task, "docs", "docs", base, "docs/a.md", "hub/internal/store/a_test.go"))
		if c == "no-push" {
			if docs.State != "held" || !strings.HasSuffix(docs.Hold.Reason, "; base bbbbbbbbbbbb is not a recorded release commit") {
				t.Fatalf("%s: docs job %+v", c, docs.Hold)
			}
			continue
		}
		if docs.State != "verified" || docs.Hold != nil {
			t.Fatalf("%s: docs job held %+v", c, docs.Hold)
		}
		if listed := pageJob(t, windowPage(t, s, task), docs.ID); listed.State != "verified" || listed.Generation != docs.Generation {
			t.Fatalf("%s: docs job swept %+v", c, listed)
		}
		if claimed, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-docs", d, docs)); err != nil || claimed.State != "claimed" {
			t.Fatal(c, "claim inside the window", err)
		}
	}
}

// wi_d87f594a6ae13ec9 a5: outside the window, and for a job whose own diff is
// covered, the ledger is never read; an unreadable record fails only the
// check that needs it.
func TestReleaseWindowReadsLedgerOnlyForUncoveredJobsInside(t *testing.T) {
	s, task, h, d, _ := releaseFixture(t)
	ctx := context.Background()
	setNightWindow(t, s, task)
	if _, err := s.db.Exec(`INSERT INTO release_hand_releases VALUES(?,?,?,?,?)`, task.ID, "hrl_unreadable", 999999, candidateC, "{"); err != nil {
		t.Fatal(err)
	}
	setWindowClock(t, s, d, windowAt("2026-10-18T21:00:00-07:00"))
	docs := windowEnqueue(t, s, task, h, "enqueue-docs", releaseEntryChanged(t, s, task, "docs", "docs", "docs/a.md"))
	if docs.State != "verified" || pageJob(t, windowPage(t, s, task), docs.ID).State != "verified" {
		t.Fatalf("docs job outside the window %+v", docs)
	}
	setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
	store := windowEnqueue(t, s, task, h, "enqueue-store", releaseEntryChanged(t, s, task, "store change", "store-change", "hub/internal/store/x.go"))
	if store.State != "held" || strings.Contains(store.Hold.Reason, "; base") {
		t.Fatalf("covered job %+v", store.Hold)
	}
	if _, err := s.ReleasesPage(ctx, task.ID, api.ReleaseListOptions{}); err == nil {
		t.Fatal("a list inside the window did not read the ledger for the docs job")
	}
}

// a5: the window's end lifts nothing; unhold waits for it and then returns
// the job in its queue order; a non-window hold unheld inside is held again.
func TestReleaseWindowEndNeedsExplicitUnhold(t *testing.T) {
	s, task, h, d, jobs := holdFixture(t, 2)
	ctx := context.Background()
	a, b := jobs[0], jobs[1]
	setNightWindow(t, s, task)
	setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
	page := windowPage(t, s, task)
	a, err := releaseLoad(ctx, s.db, task.ID, a.ID)
	if err != nil || a.State != "held" || pageJob(t, page, b.ID).State != "held" {
		t.Fatal("sweep", a.State, err)
	}
	setWindowClock(t, s, d, windowAt("2026-10-19T06:59:00-07:00"))
	if _, err = s.ReleaseAction(ctx, task.ID, unholdRequest("unhold-early", h, a, "Morning")); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "release quiet window hold until 2026-10-19T07:00:00-07:00") {
		t.Fatal("unhold inside the window", err)
	}
	setWindowClock(t, s, d, windowAt("2026-10-19T07:00:00-07:00"))
	page = windowPage(t, s, task)
	if page.Window == nil || page.Window.Open || page.Window.Until != "" || pageJob(t, page, a.ID).State != "held" {
		t.Fatalf("after the end %+v", page)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, claimRequest("claim-held", d, a)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is on an owner hold") {
		t.Fatal("claim after the end without unhold", err)
	}
	free, err := s.ReleaseAction(ctx, task.ID, unholdRequest("unhold", h, a, "Morning"))
	if err != nil || free.State != "verified" || free.Hold != nil || len(free.HoldHistory) != 2 || free.HoldHistory[1].Cause != "handler" {
		t.Fatal("unhold after the end", free, err)
	}
	line, err := s.Releases(ctx, task.ID)
	if err != nil || len(line) != 2 || line[0].ID != a.ID || line[1].ID != b.ID {
		t.Fatal("queue order", err)
	}
	if claimed, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim", d, free)); err != nil || claimed.State != "claimed" {
		t.Fatal("claim after unhold", err)
	}

	// A handler hold unheld inside the window ends held by the window.
	s, task, h, d, jobs = holdFixture(t, 1)
	setNightWindow(t, s, task)
	held, err := s.ReleaseAction(ctx, task.ID, holdRequest("hold", h, jobs[0], "Owner reads it first"))
	if err != nil {
		t.Fatal(err)
	}
	setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
	again, err := s.ReleaseAction(ctx, task.ID, unholdRequest("unhold", h, held, "Owner read it"))
	if err != nil || again.State != "held" || again.Hold == nil || again.Hold.Window == nil || again.Generation != held.Generation+1 {
		t.Fatal("unhold inside the window", again, err)
	}
	if hist := again.HoldHistory; len(hist) != 3 || hist[1].Action != "release" || hist[1].Cause != "handler" || hist[2].Action != "hold" || hist[2].Cause != "window" || hist[2].Generation != again.Generation {
		t.Fatalf("history %+v", hist)
	}

	// A clear releases nothing; the held job is unheld at night after it.
	if _, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "clear", Operation: "window-clear", Window: &api.ReleaseWindow{Reason: "Owner releases tonight"}}); err != nil {
		t.Fatal(err)
	}
	if saved, err := releaseLoad(ctx, s.db, task.ID, again.ID); err != nil || saved.State != "held" {
		t.Fatal("clear lifted a hold", saved.State, err)
	}
	if free, err := s.ReleaseAction(ctx, task.ID, unholdRequest("unhold-cleared", h, again, "Owner go")); err != nil || free.State != "verified" {
		t.Fatal("unhold after a clear", err)
	}
}

// A requeue that left the host release lock is never held: the deployer frees
// the lock only while the job is verified. Claim still waits for the end.
func TestReleaseWindowLeavesLockedRequeueVerified(t *testing.T) {
	s, task, _, d, jobs := holdFixture(t, 1)
	ctx := context.Background()
	locked := rewriteJob(t, s, jobs[0], func(j *api.ReleaseJob) {
		j.Reconciliations = append(j.Reconciliations, api.ReleaseReconciliation{Disposition: "requeue", LockDigest: strings.Repeat("e", 64)})
	})
	setNightWindow(t, s, task)
	setWindowClock(t, s, d, windowAt("2026-10-18T23:30:00-07:00"))
	if listed := pageJob(t, windowPage(t, s, task), locked.ID); listed.State != "verified" {
		t.Fatalf("locked requeue %+v", listed)
	}
	if _, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-night", d, locked)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "release quiet window") {
		t.Fatal("claim inside the window", err)
	}
	setWindowClock(t, s, d, windowAt("2026-10-19T07:00:00-07:00"))
	if claimed, err := s.ReleaseAction(ctx, task.ID, claimRequest("claim-morning", d, locked)); err != nil || claimed.State != "claimed" {
		t.Fatal("claim after the end", err)
	}
}

// A sweep is a ledger change: a list paged across the window's start is
// refused as stale, and a fresh first page shows the holds.
func TestReleaseWindowSweepStalesPagedList(t *testing.T) {
	s, task, _, d, jobs := holdFixture(t, 2)
	ctx := context.Background()
	setNightWindow(t, s, task)
	setWindowClock(t, s, d, windowAt("2026-10-18T21:59:00-07:00"))
	first, err := s.ReleasesPage(ctx, task.ID, api.ReleaseListOptions{Limit: 1})
	if err != nil || first.Page.NextAfter == "" || first.Jobs[0].State != "verified" {
		t.Fatal(first, err)
	}
	setWindowClock(t, s, d, windowAt("2026-10-18T22:00:00-07:00"))
	if _, err = s.ReleasesPage(ctx, task.ID, api.ReleaseListOptions{Limit: 1, After: first.Page.NextAfter, Snapshot: first.Page.Snapshot}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "stale list snapshot") {
		t.Fatal("continuation across the sweep", err)
	}
	page := windowPage(t, s, task)
	if pageJob(t, page, jobs[0].ID).State != "held" || pageJob(t, page, jobs[1].ID).State != "held" {
		t.Fatalf("fresh list %+v", page.Jobs)
	}
}

// Entry holds and dated holds (wi_c09132e28129688d): an owner hold saved on
// a queue entry before acceptance creates the job held, and a hold with a
// not-before time is lifted by the hub once the time passes.

func entryHoldRequest(key, op, agent, run, entry, reason, notBefore string) api.ReleaseRequest {
	return api.ReleaseRequest{RequestID: key, Operation: op, AgentID: agent, RunID: run, EntryID: entry, Hold: &api.ReleaseHold{Reason: reason, NotBefore: notBefore}}
}

// entryHoldAcceptance is a running entry not yet accepted, with a running
// deployer, its handler and the store clock set to a whole second.
func entryHoldAcceptance(t *testing.T) (planAcceptance, api.TeamQueueEntry, api.Agent, time.Time) {
	t.Helper()
	p := newPlanAcceptance(t, "", true)
	d, err := p.s.AddAgent(context.Background(), p.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "deployer", Host: "fixture", Session: "deployer", Role: api.AgentRoleDeployment}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.s.db.Exec(`UPDATE agents SET status='running' WHERE id=?`, d.ID); err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC().Truncate(time.Second)
	setWindowClock(t, p.s, d, clock)
	e, err := p.s.GetTeamQueueEntry(context.Background(), p.task.ID, p.entry)
	if err != nil {
		t.Fatal(err)
	}
	return p, e, d, clock
}

func entryHoldState(t *testing.T, s *Store, task api.Task, entry string) (state, job string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT state,COALESCE(json_extract(record_json,'$.jobId'),'') FROM release_entry_holds WHERE task_id=? AND entry_id=? ORDER BY rowid DESC LIMIT 1`, task.ID, entry).Scan(&state, &job); err != nil {
		t.Fatal(err)
	}
	return state, job
}

// h1/h4: a dated entry hold saved before acceptance; the accepting
// transaction creates the job held at its first generation and a claim of it
// is refused.
func TestReleaseEntryHoldCreatesAcceptedJobHeld(t *testing.T) {
	p, e, d, clock := entryHoldAcceptance(t)
	ctx := context.Background()
	notBefore := clock.Add(time.Hour).Format(time.RFC3339)
	reason := "Owner: no release before the measurement ends"
	saved, err := p.s.ReleaseAction(ctx, p.task.ID, entryHoldRequest("entry-hold", "hold", e.HandlerID, e.HandlerRunID, p.entry, reason, notBefore))
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != "" || saved.State != "" || saved.EntryID != p.entry || saved.ItemID != e.ItemID || saved.OrderMessageSeq != e.OrderMessageSeq || saved.Hold == nil || saved.Hold.Reason != reason || saved.Hold.NotBefore != notBefore || saved.Hold.AgentID != e.HandlerID || saved.Hold.RunID != e.HandlerRunID {
		t.Fatalf("entry hold response %+v %+v", saved, saved.Hold)
	}
	if page := windowPage(t, p.s, p.task); len(page.EntryHolds) != 1 || page.EntryHolds[0].EntryID != p.entry || page.EntryHolds[0].State != "standing" || page.EntryHolds[0].NotBefore != notBefore {
		t.Fatalf("page entry holds %+v", page.EntryHolds)
	}
	got, err := p.accept(t, "accept", candidateC, candidateB)
	if err != nil {
		t.Fatal(err)
	}
	j := got.Release
	if j == nil || j.State != "held" || j.Generation != 1 || j.AgentID != "" || j.Hold == nil || j.Hold.Reason != reason || j.Hold.NotBefore != notBefore || j.Hold.AgentID != e.HandlerID || j.Hold.HeldAt != ts(clock) {
		t.Fatalf("accepted job %+v", j)
	}
	if h := j.HoldHistory; len(h) != 1 || h[0].Action != "hold" || h[0].Cause != "entry" || h[0].NotBefore != notBefore || h[0].Generation != 1 || h[0].AgentID != e.HandlerID {
		t.Fatalf("history %+v", h)
	}
	if saved, err := releaseLoad(ctx, p.s.db, p.task.ID, j.ID); err != nil || saved.State != "held" || saved.Generation != 1 {
		t.Fatal("saved job", saved.State, saved.Generation, err)
	}
	if state, job := entryHoldState(t, p.s, p.task, p.entry); state != "applied" || job != j.ID {
		t.Fatal("entry hold", state, job)
	}
	row := releaseRow(t, p.s, p.task, j.ID)
	if _, err = p.s.ReleaseAction(ctx, p.task.ID, claimRequest("claim", d, *j)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "owner hold ("+reason+") until "+notBefore) {
		t.Fatal("claim of a job created held", err)
	}
	if releaseRow(t, p.s, p.task, j.ID) != row {
		t.Fatal("the refused claim changed the job")
	}
	if page := windowPage(t, p.s, p.task); len(page.EntryHolds) != 0 || pageJob(t, page, j.ID).State != "held" {
		t.Fatalf("page after acceptance %+v", page)
	}
}

// h2/h4: the job created held is lifted at the first list once its time
// has passed, compared as a time, not text, and is then claimable.
func TestReleaseEntryHoldLiftsAfterNotBefore(t *testing.T) {
	p, e, d, clock := entryHoldAcceptance(t)
	ctx := context.Background()
	notBefore := clock.Add(time.Hour)
	if _, err := p.s.ReleaseAction(ctx, p.task.ID, entryHoldRequest("entry-hold", "hold", e.HandlerID, e.HandlerRunID, p.entry, "Not before the boundary", notBefore.Format(time.RFC3339))); err != nil {
		t.Fatal(err)
	}
	got, err := p.accept(t, "accept", candidateC, candidateB)
	if err != nil || got.Release == nil || got.Release.State != "held" {
		t.Fatal("accept", got.Release, err)
	}
	id := got.Release.ID
	setWindowClock(t, p.s, d, notBefore.Add(-500*time.Millisecond))
	if listed := pageJob(t, windowPage(t, p.s, p.task), id); listed.State != "held" || listed.Generation != 1 {
		t.Fatalf("lifted before its time %+v", listed)
	}
	setWindowClock(t, p.s, d, notBefore.Add(250*time.Millisecond))
	if listed := pageJob(t, windowPage(t, p.s, p.task), id); listed.State != "verified" || listed.Generation != 2 || listed.Hold != nil {
		t.Fatalf("not lifted after its time %+v", listed)
	}
	j, err := p.s.Release(ctx, p.task.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	if h := j.HoldHistory; len(h) != 2 || h[0].Cause != "entry" || h[1].Action != "release" || h[1].Cause != "not_before" || h[1].NotBefore != notBefore.Format(time.RFC3339) || h[1].Generation != 2 || h[1].At != ts(notBefore.Add(250*time.Millisecond)) {
		t.Fatalf("history %+v", h)
	}
	// A second list writes nothing more.
	if listed := pageJob(t, windowPage(t, p.s, p.task), id); listed.Generation != 2 {
		t.Fatalf("relisted %+v", listed)
	}
	if claimed, err := p.s.ReleaseAction(ctx, p.task.ID, claimRequest("claim", d, j)); err != nil || claimed.State != "claimed" {
		t.Fatal("lifted job not claimable", claimed.State, err)
	}
}

// h4: without an entry hold acceptance behaves as before.
func TestReleaseAcceptanceWithoutEntryHoldStaysVerified(t *testing.T) {
	p, _, d, _ := entryHoldAcceptance(t)
	ctx := context.Background()
	got, err := p.accept(t, "accept", candidateC, candidateB)
	if err != nil {
		t.Fatal(err)
	}
	j := got.Release
	if j == nil || j.State != "verified" || j.Generation != 1 || j.Hold != nil || len(j.HoldHistory) != 0 {
		t.Fatalf("accepted job %+v", j)
	}
	if page := windowPage(t, p.s, p.task); len(page.EntryHolds) != 0 || pageJob(t, page, j.ID).State != "verified" {
		t.Fatalf("page %+v", page)
	}
	if n := countRows(t, p.s, `SELECT count(*) FROM release_entry_holds`); n != 0 {
		t.Fatal("entry holds", n)
	}
	if claimed, err := p.s.ReleaseAction(ctx, p.task.ID, claimRequest("claim", d, *j)); err != nil || claimed.State != "claimed" {
		t.Fatal("claim", claimed.State, err)
	}
}

// h1: an entry hold is the handler's, checked before it is saved, replays
// by request ID, applies only before the entry's next job, and an unhold
// releases it.
func TestReleaseEntryHoldValidationAndUnhold(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	future := s.now().Add(time.Hour).UTC().Format(time.RFC3339)
	hold := func(key, agent, run, entry, reason, notBefore string) (api.ReleaseJob, error) {
		return s.ReleaseAction(ctx, task.ID, entryHoldRequest(key, "hold", agent, run, entry, reason, notBefore))
	}
	if _, err := hold("by-deployer", d.ID, d.RunID, entry, "Hold it", future); !errors.Is(err, api.ErrConflict) {
		t.Fatal("deployer saved an entry hold", err)
	}
	generation := entryHoldRequest("generation", "hold", h.ID, h.RunID, entry, "Hold it", future)
	generation.ExpectedGeneration = 1
	items := entryHoldRequest("items", "hold", h.ID, h.RunID, entry, "Hold it", "")
	items.Hold.UntilItems = []string{api.NewID("wi")}
	for name, req := range map[string]api.ReleaseRequest{"generation": generation, "items": items, "reason": entryHoldRequest("reason", "hold", h.ID, h.RunID, entry, "", future), "unparseable": entryHoldRequest("unparseable", "hold", h.ID, h.RunID, entry, "Hold it", "tomorrow")} {
		if _, err := s.ReleaseAction(ctx, task.ID, req); !errors.Is(err, api.ErrInvalid) {
			t.Fatal(name, err)
		}
	}
	if _, err := hold("past", h.ID, h.RunID, entry, "Hold it", s.now().Add(-time.Minute).Format(time.RFC3339)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is not after the hub time") {
		t.Fatal("past not-before", err)
	}
	if _, err := hold("unknown", h.ID, h.RunID, api.NewID("tqe"), "Hold it", future); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is not in this project") {
		t.Fatal("unknown entry", err)
	}
	if n := countRows(t, s, `SELECT count(*) FROM release_entry_holds`); n != 0 {
		t.Fatal("a refused entry hold was saved", n)
	}
	saved, err := hold("hold", h.ID, h.RunID, entry, "Owner hold", future)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := hold("hold", h.ID, h.RunID, entry, "Owner hold", future); err != nil || again.Hold == nil || again.Hold.HeldAt != saved.Hold.HeldAt {
		t.Fatal("replay", again.Hold, err)
	}
	if _, err = hold("hold", h.ID, h.RunID, entry, "Another reason", future); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "retry changed") {
		t.Fatal("changed replay", err)
	}
	// A job operation reusing the entry hold's request ID is a changed retry.
	if _, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "hold", Operation: "enqueue", AgentID: h.ID, RunID: h.RunID, EntryID: entry}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "retry changed") {
		t.Fatal("job operation on an entry hold receipt", err)
	}
	if _, err = hold("second", h.ID, h.RunID, entry, "Again", ""); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "already has a standing entry hold") {
		t.Fatal("second standing hold", err)
	}
	page := windowPage(t, s, task)
	if len(page.EntryHolds) != 1 || page.EntryHolds[0].Reason != "Owner hold" || page.EntryHolds[0].AgentID != h.ID || page.EntryHolds[0].RequestID != "hold" || !strings.HasPrefix(page.EntryHolds[0].ID, "reh_") {
		t.Fatalf("page entry holds %+v", page.EntryHolds)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, entryHoldRequest("unhold-dated", "unhold", h.ID, h.RunID, entry, "Go", future)); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("unhold with a not-before time", err)
	}
	free, err := s.ReleaseAction(ctx, task.ID, entryHoldRequest("unhold", "unhold", h.ID, h.RunID, entry, "Owner go", ""))
	if err != nil || free.Hold != nil || free.EntryID != entry {
		t.Fatal("unhold", free, err)
	}
	if state, _ := entryHoldState(t, s, task, entry); state != "released" {
		t.Fatal("released entry hold", state)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, entryHoldRequest("unhold-again", "unhold", h.ID, h.RunID, entry, "Owner go", "")); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "has no standing entry hold") {
		t.Fatal("second unhold", err)
	}
	if page = windowPage(t, s, task); len(page.EntryHolds) != 0 {
		t.Fatalf("page after unhold %+v", page.EntryHolds)
	}
	j := windowEnqueue(t, s, task, h, "enqueue", entry)
	if j.State != "verified" || j.Hold != nil {
		t.Fatalf("job after a released entry hold %+v", j)
	}
	if _, err = hold("after-job", h.ID, h.RunID, entry, "Too late", future); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "tt deployment hold --job "+j.ID) {
		t.Fatal("entry hold over a verified job", err)
	}
}

// h1: an entry hold saved after a refusal creates the retry held; one whose
// time passed first expires and the retry stays verified.
func TestReleaseEntryHoldAppliesToRetry(t *testing.T) {
	s, task, h, d, entry := releaseFixture(t)
	ctx := context.Background()
	first := refusedJob(t, s, task, h, d, entry)
	notBefore := s.now().Add(time.Hour).UTC().Format(time.RFC3339)
	if _, err := s.ReleaseAction(ctx, task.ID, entryHoldRequest("entry-hold", "hold", h.ID, h.RunID, entry, "Retry after the boundary", notBefore)); err != nil {
		t.Fatal(err)
	}
	retried, err := s.ReleaseAction(ctx, task.ID, retryRequest("retry", h, entry, first))
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != "held" || retried.Generation != 1 || retried.Hold == nil || retried.Hold.NotBefore != notBefore || len(retried.HoldHistory) != 1 || retried.HoldHistory[0].Cause != "entry" {
		t.Fatalf("retry %+v", retried)
	}
	if state, job := entryHoldState(t, s, task, entry); state != "applied" || job != retried.ID {
		t.Fatal("entry hold", state, job)
	}
	other := releaseEntry(t, s, task, "expired hold item", "expired-hold")
	refused := windowEnqueue(t, s, task, h, "other-enqueue", other)
	for _, op := range []string{"claim", "refuse"} {
		if refused, err = s.ReleaseAction(ctx, task.ID, api.ReleaseRequest{RequestID: "other-" + op, Operation: op, AgentID: d.ID, RunID: d.RunID, JobID: refused.ID, ExpectedGeneration: refused.Generation}); err != nil {
			t.Fatal(op, err)
		}
	}
	soon := s.now().Add(time.Minute)
	if _, err = s.ReleaseAction(ctx, task.ID, entryHoldRequest("entry-hold-soon", "hold", h.ID, h.RunID, other, "Briefly", soon.UTC().Format(time.RFC3339))); err != nil {
		t.Fatal(err)
	}
	setWindowClock(t, s, d, soon.Add(time.Minute))
	again, err := s.ReleaseAction(ctx, task.ID, retryRequest("retry-other", h, other, refused))
	if err != nil || again.State != "verified" || again.Hold != nil || len(again.HoldHistory) != 0 {
		t.Fatal("retry after the entry hold's time", again, err)
	}
	if state, job := entryHoldState(t, s, task, other); state != "expired" || job != "" {
		t.Fatal("expired entry hold", state, job)
	}
}

// h2: a dated job hold refuses claim and batch-add until its time, is lifted
// by the first list after it, and is held again by an open quiet window.
func TestReleaseDatedHoldOnVerifiedJob(t *testing.T) {
	s, task, h, d, jobs := holdFixture(t, 2)
	ctx := context.Background()
	setWindowClock(t, s, d, windowAt("2026-10-18T20:00:00-07:00"))
	a, b := jobs[0], jobs[1]
	notBefore := "2026-10-18T21:00:00-07:00"
	both := holdRequest("both", h, a, "Hold it", b.ItemID)
	both.Hold.NotBefore = notBefore
	if _, err := s.ReleaseAction(ctx, task.ID, both); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("items and a not-before time", err)
	}
	past := holdRequest("past", h, a, "Hold it")
	past.Hold.NotBefore = "2026-10-18T19:00:00-07:00"
	if _, err := s.ReleaseAction(ctx, task.ID, past); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is not after the hub time") {
		t.Fatal("past not-before", err)
	}
	req := holdRequest("hold", h, a, "Owner: not before nine")
	req.Hold.NotBefore = notBefore
	held, err := s.ReleaseAction(ctx, task.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if held.State != "held" || held.Hold == nil || held.Hold.NotBefore != "2026-10-19T04:00:00Z" || held.HoldHistory[0].NotBefore != "2026-10-19T04:00:00Z" {
		t.Fatalf("dated hold %+v", held.Hold)
	}
	if _, err = s.ReleaseAction(ctx, task.ID, claimRequest("claim-held", d, held)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "until 2026-10-19T04:00:00Z") {
		t.Fatal("claim of a dated hold", err)
	}
	early := unholdRequest("unhold-dated", h, held, "Go")
	early.Hold.NotBefore = notBefore
	if _, err = s.ReleaseAction(ctx, task.ID, early); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("unhold with a not-before time", err)
	}
	setWindowClock(t, s, d, windowAt("2026-10-18T20:59:59-07:00"))
	if listed := pageJob(t, windowPage(t, s, task), a.ID); listed.State != "held" {
		t.Fatalf("lifted early %+v", listed)
	}
	// The quiet window opens at 22:00: a lift inside it is window-held in
	// the same list.
	setNightWindow(t, s, task)
	setWindowClock(t, s, d, windowAt("2026-10-18T22:30:00-07:00"))
	page := windowPage(t, s, task)
	listed := pageJob(t, page, a.ID)
	if listed.State != "held" || listed.Hold == nil || listed.Hold.Window == nil || listed.Hold.NotBefore != "" {
		t.Fatalf("dated hold lifted inside the window %+v %+v", listed, listed.Hold)
	}
	j, err := s.Release(ctx, task.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h := j.HoldHistory; len(h) != 3 || h[1].Cause != "not_before" || h[1].Generation != held.Generation+1 || h[2].Cause != "window" || h[2].Generation != held.Generation+2 {
		t.Fatalf("history %+v", h)
	}
}

// h2: batch-add refuses a member on a dated hold.
func TestReleaseDatedHoldRefusesBatchAdd(t *testing.T) {
	ctx := context.Background()
	f := newBatchFixture(t, 1)
	req := holdRequest("hold", f.h, f.members[0], "Not before the boundary")
	req.Hold.NotBefore = f.s.now().Add(time.Hour).UTC().Format(time.RFC3339)
	held, err := f.s.ReleaseAction(ctx, f.task.ID, req)
	if err != nil || held.State != "held" || held.Hold.NotBefore != req.Hold.NotBefore {
		t.Fatal(held.State, err)
	}
	if _, err = f.deployer("batch-open", "", batchBase); err != nil {
		t.Fatal(err)
	}
	if _, err = f.deployer("batch-add", f.lead.EntryID, batchTo(1)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.deployer("batch-add", held.EntryID, candidateA); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is not an unclaimed verified job") {
		t.Fatal("a dated held job joined a batch", err)
	}
}

// Review f1 (#47877): a requeue moves the failed entry's standing entry
// hold to the new attempt, whose acceptance then creates the job held; the
// failed entry keeps the moved row and has no standing hold.
func TestReleaseEntryHoldMovesWithRequeue(t *testing.T) {
	f := newRebindFixture(t, true)
	d, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "deployer", Host: "fixture", Session: "deployer", Role: api.AgentRoleDeployment}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec(`UPDATE agents SET status='running',last_seen_at=? WHERE id=?`, ts(f.s.now()), d.ID); err != nil {
		t.Fatal(err)
	}
	notBefore := f.s.now().Add(time.Hour).UTC().Format(time.RFC3339)
	first := f.entry.ID
	if _, err = f.s.ReleaseAction(f.ctx, f.task.ID, entryHoldRequest("entry-hold", "hold", f.handler.ID, f.handler.RunID, first, "Not before the measurement ends", notBefore)); err != nil {
		t.Fatal(err)
	}
	f.failAndRelease(t)
	retry, err := f.s.TeamQueueAction(f.ctx, f.task.ID, f.requeue())
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := entryHoldState(t, f.s, f.task, first); state != "moved" {
		t.Fatal("failed entry's hold", state)
	}
	page := windowPage(t, f.s, f.task)
	if len(page.EntryHolds) != 1 || page.EntryHolds[0].EntryID != retry.ID || page.EntryHolds[0].NotBefore != notBefore || page.EntryHolds[0].MovedFrom == "" || page.EntryHolds[0].OrderMessageSeq != retry.OrderMessageSeq {
		t.Fatalf("standing entry holds after the requeue %+v", page.EntryHolds)
	}
	var moved api.ReleaseEntryHold
	var raw string
	if err = f.s.db.QueryRow(`SELECT record_json FROM release_entry_holds WHERE task_id=? AND entry_id=?`, f.task.ID, first).Scan(&raw); err != nil || json.Unmarshal([]byte(raw), &moved) != nil {
		t.Fatal(err)
	}
	if moved.ID != page.EntryHolds[0].MovedFrom || moved.MovedTo != page.EntryHolds[0].ID || moved.ReleasedAt == "" || !strings.Contains(moved.ReleaseReason, retry.ID) {
		t.Fatalf("moved row %+v", moved)
	}
	// The retry runs and its done save accepts it before the hold's time.
	q := f.action(t, api.TeamQueueRequest{Operation: "claim", EntryID: retry.ID, ExpectedRevision: retry.Revision, Host: "mini"})
	lead := f.member(t, "lead-two")
	plan, _ := json.Marshal(map[string]any{"task": f.task.ID, "item": f.item.ID, "revision": f.item.Revision, "order": f.order.Seq, "context": map[string]any{"version": 1}, "members": []any{map[string]any{"state": "unstarted", "runId": lead.RunID, "fields": map[string]any{"agentId": lead.ID, "name": lead.Name, "cwd": "/worktrees/rebind"}}}})
	for _, step := range []api.TeamQueueRequest{{Operation: "freeze", LaunchJSON: plan}, {Operation: "attempt"}, {Operation: "started", MemberRunID: lead.RunID}, {Operation: "running"}} {
		step.EntryID, step.ExpectedRevision = q.ID, q.Revision
		q = f.action(t, step)
	}
	item, err := f.s.GetWorkItem(f.ctx, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedPassingVerificationAt(t, f.s, item, candidateB, autoWorktree)
	report, _, err := f.s.PutNarrativeReport(f.ctx, f.task.ID, f.item.ID, completeReportRequest(item, "requeued-report", 5), f.by)
	if err != nil {
		t.Fatal(err)
	}
	done := "done"
	if _, _, err = f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, Status: &done, AgentID: f.handler.ID, RunID: f.handler.RunID, RequestID: "done-save",
		CompletionReport: &api.NarrativeReportPin{ReportID: report.ReportID, Version: report.Version, Digest: report.Digest, ScopeRevision: report.ScopeRevision},
		QueueAcceptance:  &api.WorkItemQueueAcceptance{EntryID: retry.ID, Worktree: autoWorktree, Branch: autoBranch, Commit: candidateB}}, f.by); err != nil {
		t.Fatalf("done save with acceptance: %v", err)
	}
	accepted, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, retry.ID)
	if err != nil {
		t.Fatal(err)
	}
	j := accepted.Release
	if j == nil || j.EntryID != retry.ID || j.State != "held" || j.Generation != 1 || j.Hold == nil || j.Hold.NotBefore != notBefore || len(j.HoldHistory) != 1 || j.HoldHistory[0].Cause != "entry" {
		t.Fatalf("requeued entry's job %+v", j)
	}
	if state, job := entryHoldState(t, f.s, f.task, retry.ID); state != "applied" || job != j.ID {
		t.Fatal("moved hold", state, job)
	}
	if _, err = f.s.ReleaseAction(f.ctx, f.task.ID, claimRequest("claim", d, *j)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "until "+notBefore) {
		t.Fatal("claim of the requeued entry's held job", err)
	}
	if page = windowPage(t, f.s, f.task); len(page.EntryHolds) != 0 {
		t.Fatalf("standing entry holds after acceptance %+v", page.EntryHolds)
	}
}

// wi_63c71acaf4f4f7b5 s1-s4: the runner's code state on its progress report.
var (
	codeLoaded, codePublished = "0123456789ab", "ba9876543210"
	refusedCode               = api.ReleaseProgressCode{State: "refused", Reason: "checkout-dirty", Loaded: codeLoaded, Published: codePublished, Since: "2026-10-27T18:00:00.000Z"}
)

// codeProgress is a poll report carrying code.
func codeProgress(code api.ReleaseProgressCode) api.ReleaseProgress {
	return api.ReleaseProgress{Phase: "poll", Code: &code}
}

// s1: a report's code is stored and read back with the hub's action; every
// invalid shape is refused and writes nothing; a report without code is as
// before.
func TestDeployerProgressCodeRecord(t *testing.T) {
	f := newLivenessFixture(t)
	setup := "Run tt deployment setup for the deployer with its current run as predecessor (docs/project-deployment.md)."
	for _, c := range []struct {
		code   api.ReleaseProgressCode
		action string
	}{
		{api.ReleaseProgressCode{State: "current", Loaded: codeLoaded, Published: codeLoaded, Since: "2026-10-27T18:00:00Z"}, ""},
		{api.ReleaseProgressCode{State: "draining", Loaded: codeLoaded, Published: codePublished, Since: "2026-10-27T18:00:00.000Z"}, ""},
		{api.ReleaseProgressCode{State: "restart", Loaded: codeLoaded, Published: codePublished, Since: "2026-10-27T18:00:00-07:00"}, ""},
		{refusedCode, setup},
		{api.ReleaseProgressCode{State: "refused", Reason: "published-unreadable", Loaded: codeLoaded, Since: "2026-10-27T18:00:00Z"}, setup},
		{api.ReleaseProgressCode{State: "refused", Reason: "load-failed", Checkout: "restored", Loaded: codeLoaded, Published: codePublished, Since: "2026-10-27T18:00:00Z"},
			"Fix the published scripts on tasks-hub; the deployer restarts itself once a published runner loads (docs/project-deployment.md)."},
		{api.ReleaseProgressCode{State: "refused", Reason: "load-failed", Checkout: "not-restored", Loaded: codeLoaded, Published: codePublished, Since: "2026-10-27T18:00:00Z"},
			"Do not re-provision from this checkout: restore it to the loaded commit or fix the published scripts on tasks-hub (docs/project-deployment.md)."},
	} {
		want := c.code
		want.Action = c.action
		got := f.report(codeProgress(c.code))
		if got.Code == nil || *got.Code != want || got.Phase != "poll" || got.ReportedAt != ts(f.clock) {
			t.Fatalf("%+v: report answered %+v %+v", c.code, got, got.Code)
		}
		if read, err := f.s.DeployerProgress(f.ctx, f.task.ID); err != nil || read.Code == nil || *read.Code != want {
			t.Fatalf("%+v: read %+v %v", c.code, read.Code, err)
		}
		// The stored record keeps no action: the hub sets it on every read.
		if row := f.progressRow(); strings.Contains(row.record, "action") {
			t.Fatalf("stored record has an action: %s", row.record)
		}
	}
	before := f.progressRow()
	for name, code := range map[string]api.ReleaseProgressCode{
		"no state":                {Since: refusedCode.Since},
		"unknown state":           {State: "stopped", Since: refusedCode.Since},
		"refused without reason":  {State: "refused", Since: refusedCode.Since},
		"unknown reason":          {State: "refused", Reason: "unclassified", Since: refusedCode.Since},
		"reason on draining":      {State: "draining", Reason: "checkout-dirty", Since: refusedCode.Since},
		"checkout without reason": {State: "refused", Reason: "checkout-dirty", Checkout: "restored", Since: refusedCode.Since},
		"load-failed no checkout": {State: "refused", Reason: "load-failed", Since: refusedCode.Since},
		"unknown checkout":        {State: "refused", Reason: "load-failed", Checkout: "moved", Since: refusedCode.Since},
		"short loaded":            {State: "current", Loaded: "0123456789a", Since: refusedCode.Since},
		"full published digest":   {State: "current", Published: strings.Repeat("a", 64), Since: refusedCode.Since},
		"upper-case prefix":       {State: "current", Loaded: "0123456789AB", Since: refusedCode.Since},
		"no since":                {State: "current"},
		"since not RFC 3339":      {State: "current", Since: "2026-10-27 18:00:00"},
		"since too long":          {State: "current", Since: "2026-10-27T18:00:00." + strings.Repeat("0", 60) + "Z"},
		"runner's action":         {State: "refused", Reason: "checkout-dirty", Since: refusedCode.Since, Action: "anything"},
	} {
		if _, err := f.s.RecordDeployerProgress(f.ctx, f.task.ID, f.d.ID, f.d.RunID, codeProgress(code)); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s: %v, want invalid", name, err)
		}
	}
	if after := f.progressRow(); after != before {
		t.Fatalf("a refused report changed the row: %+v -> %+v", before, after)
	}
	// A report without code is stored and read exactly as before.
	if got := f.report(pollProgress); got.Code != nil || strings.Contains(f.progressRow().record, "code") {
		t.Fatalf("no-code report: %+v %s", got, f.progressRow().record)
	}
}

// s1: the hub's reasons are the runner's.
func TestDeployerProgressCodeReasonsMatchRunner(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "release-runner.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	literal := `export const CODE_REASONS=["` + strings.Join(deployerCodeReasons, `","`) + `"];`
	if !strings.Contains(string(raw), literal) {
		t.Fatalf("scripts/release-runner.mjs no longer contains %s", literal)
	}
}

// s2: the list page carries the live deployer run's last report with its
// code, in both views, and nothing for no report, a withdrawn one, or a run
// that is not the live deployer's; the jobs and snapshot are unchanged.
func TestReleasesPageShowsDeployerCode(t *testing.T) {
	f := newLivenessFixture(t)
	f.enqueue()
	page := func(view string) api.ReleasePage {
		t.Helper()
		p, err := f.s.ReleasesPage(f.ctx, f.task.ID, api.ReleaseListOptions{View: view})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	empty := page("active")
	if empty.Deployer != nil {
		t.Fatalf("no report: %+v", empty.Deployer)
	}
	f.report(codeProgress(refusedCode))
	for _, view := range []string{"active", "settled"} {
		p := page(view)
		if p.Deployer == nil || p.Deployer.AgentID != f.d.ID || p.Deployer.RunID != f.d.RunID || p.Deployer.ReportedAt != ts(f.clock) || p.Deployer.Code == nil {
			t.Fatalf("%s: %+v", view, p.Deployer)
		}
		want := refusedCode
		want.Action = "Run tt deployment setup for the deployer with its current run as predecessor (docs/project-deployment.md)."
		if *p.Deployer.Code != want {
			t.Fatalf("%s code: %+v", view, *p.Deployer.Code)
		}
		raw, _ := json.Marshal(p)
		if !strings.Contains(string(raw), `"deployer":{"agentId":"`+f.d.ID+`"`) || !strings.Contains(string(raw), `"code":{"state":"refused","reason":"checkout-dirty","loaded":"0123456789ab","published":"ba9876543210","since":"2026-10-27T18:00:00.000Z","action":"Run tt deployment setup`) {
			t.Fatalf("%s JSON: %s", view, raw)
		}
	}
	if p := page("active"); p.Page.Snapshot != empty.Page.Snapshot || !reflect.DeepEqual(p.Jobs, empty.Jobs) {
		t.Fatalf("the report changed the page: %+v -> %+v", empty, p)
	}
	if _, err := f.s.WithdrawDeployerProgress(f.ctx, f.task.ID, f.d.ID, f.d.RunID); err != nil {
		t.Fatal(err)
	}
	if p := page("active"); p.Deployer != nil {
		t.Fatalf("withdrawn: %+v", p.Deployer)
	}
	f.report(codeProgress(refusedCode))
	f.exec(`UPDATE agents SET run_id=? WHERE id=?`, api.NewID("run"), f.d.ID)
	if p := page("active"); p.Deployer != nil {
		t.Fatalf("another run of the deployer: %+v", p.Deployer)
	}
	f.exec(`UPDATE agents SET run_id=? WHERE id=?`, f.d.RunID, f.d.ID)
	for _, status := range []string{api.AgentRetired, api.AgentClosed} {
		f.exec(`UPDATE agents SET status=? WHERE id=?`, status, f.d.ID)
		if p := page("active"); p.Deployer != nil {
			t.Fatalf("%s deployer: %+v", status, p.Deployer)
		}
	}
	f.exec(`UPDATE agents SET status='running' WHERE id=?`, f.d.ID)
	if p := page("active"); p.Deployer == nil {
		t.Fatal("running deployer again: no deployer")
	}
}

// reporting advances the clock in broker ticks with a fresh heartbeat and
// a new report p after each tick's sweep, as a polling runner does.
func (f *livenessFixture) reporting(p api.ReleaseProgress, d time.Duration) {
	f.t.Helper()
	f.live = true
	for ; d > 0; d -= 30 * time.Second {
		f.advance(30 * time.Second)
		f.report(p)
	}
}

// s3: a live deployer whose runner reports refused code while a verified job
// waits gets one stale_code notice per recipient, with its reason, prefixes,
// since and action; a draining or current one, a claimed or merged job
// alone, a report from before the run's newest code notice and a job younger
// than the bound stay quiet; silent, restart and hung keep precedence; a
// current report re-arms.
func TestDeployerLivenessStaleCode(t *testing.T) {
	opened := func(f *livenessFixture, code api.ReleaseProgressCode) {
		f.t.Helper()
		f.report(codeProgress(code))
		f.enqueue()
		if n := f.sweep(); n != 0 {
			f.t.Fatalf("first sight posted %d", n)
		}
		f.reporting(codeProgress(code), livenessN)
	}
	for _, c := range []struct {
		name   string
		code   api.ReleaseProgressCode
		parts  []string
		action string
	}{
		{"checkout-dirty", refusedCode, []string{"Reason: checkout-dirty; loaded scripts 0123456789ab, published scripts ba9876543210; refused since 2026-10-27T18:00:00.000Z."},
			"Action: Run tt deployment setup for the deployer with its current run as predecessor (docs/project-deployment.md)."},
		{"load-failed not restored", api.ReleaseProgressCode{State: "refused", Reason: "load-failed", Checkout: "not-restored", Loaded: codeLoaded, Published: codePublished, Since: refusedCode.Since},
			[]string{"Reason: load-failed, checkout not-restored; loaded scripts 0123456789ab"},
			"Action: Do not re-provision from this checkout: restore it to the loaded commit or fix the published scripts on tasks-hub (docs/project-deployment.md)."},
		{"published unreadable", api.ReleaseProgressCode{State: "refused", Reason: "published-unreadable", Loaded: codeLoaded, Since: refusedCode.Since},
			[]string{"Reason: published-unreadable; loaded scripts 0123456789ab, published scripts none;"}, "Action: Run tt deployment setup"},
	} {
		t.Run("posts "+c.name, func(t *testing.T) {
			f := newLivenessFixture(t)
			opened(f, c.code)
			if n := f.sweep(); n != 2 {
				t.Fatalf("posted %d, want 2", n)
			}
			f.want("stale code", 1, 1)
			for _, to := range []api.Agent{f.h, f.own} {
				e := f.copies(to)[0]
				if e.Kind != api.EnvelopeKindNotice || e.To != to.Name || e.Subject != "Deployer refuses releases because its runner code is out of date" {
					t.Fatalf("%s copy: %+v", to.Name, e)
				}
				if e.Refs["activity"] != "deployer_liveness" || e.Refs["reason"] != "stale_code" || e.Refs["deployer"] != f.d.ID || e.Refs["run"] != f.d.RunID {
					t.Fatalf("%s refs: %v", to.Name, e.Refs)
				}
				for _, part := range append([]string{f.d.Name, f.d.ID, f.d.RunID, "is alive but refuses to claim releases because its runner code is out of date.", livenessCounts["verified"], c.action, "Nothing was restarted or signalled."}, c.parts...) {
					if !strings.Contains(e.Body.Text, part) {
						t.Fatalf("%s text lacks %q: %s", to.Name, part, e.Body.Text)
					}
				}
			}
			f.reporting(codeProgress(c.code), 3*livenessN)
			if n := f.sweep(); n != 0 {
				t.Fatalf("same episode posted %d", n)
			}
		})
	}
	for _, state := range []string{"current", "draining"} {
		t.Run(state+" is quiet", func(t *testing.T) {
			f := newLivenessFixture(t)
			opened(f, api.ReleaseProgressCode{State: state, Loaded: codeLoaded, Published: codePublished, Since: refusedCode.Since})
			f.reporting(codeProgress(api.ReleaseProgressCode{State: state, Loaded: codeLoaded, Published: codePublished, Since: refusedCode.Since}), 2*livenessN)
			if n := f.sweep(); n != 0 {
				t.Fatalf("posted %d", n)
			}
			f.want(state, 0, 0)
		})
	}
	for _, state := range []string{"claimed", "merged"} {
		t.Run(state+" job alone is quiet", func(t *testing.T) {
			f := newLivenessFixture(t)
			f.live = true
			j := f.act(f.enqueue(), "claim")
			if state == "merged" {
				f.act(j, "merged")
			}
			f.report(codeProgress(refusedCode))
			f.sweep()
			f.reporting(codeProgress(refusedCode), 3*livenessN)
			if n := f.sweep(); n != 0 {
				t.Fatalf("posted %d", n)
			}
			f.want(state, 0, 0)
		})
	}
	t.Run("report older than the newest code notice is quiet", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.live = true
		f.report(codeProgress(refusedCode))
		f.clock = f.clock.Add(time.Second)
		f.beat()
		f.say("Deployer now runs the published scripts")
		f.enqueue()
		f.sweep()
		f.advance(3 * livenessN)
		if n := f.sweep(); n != 0 {
			t.Fatalf("posted %d", n)
		}
		f.want("older report", 0, 0)
	})
	t.Run("job younger than the bound is quiet", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.report(codeProgress(refusedCode))
		f.enqueue()
		f.sweep()
		f.reporting(codeProgress(refusedCode), livenessN-30*time.Second)
		if n := f.sweep(); n != 0 {
			t.Fatalf("posted %d", n)
		}
		f.want("young job", 0, 0)
	})
	t.Run("silent and hung keep precedence", func(t *testing.T) {
		silent := newLivenessFixture(t)
		silent.report(codeProgress(refusedCode))
		silent.standing(false)
		if n := silent.sweep(); n != 2 {
			t.Fatalf("silent posted %d, want 2", n)
		}
		if e := silent.copies(silent.own)[0]; e.Refs["reason"] != "silent" || e.Subject != "Deployer has gone silent while release jobs wait" || !strings.Contains(e.Body.Text, "Action: run tt deployment setup for the deployer with its current run as predecessor (docs/project-deployment.md).") {
			t.Fatalf("silent copy: %+v", e)
		}
		hung := newLivenessFixture(t)
		hung.report(codeProgress(refusedCode))
		hung.standing(true)
		if n := hung.sweep(); n != 2 {
			t.Fatalf("hung posted %d, want 2", n)
		}
		if e := hung.copies(hung.own)[0]; e.Refs["reason"] != "hung" || e.Subject != "Deployer runner has made no progress while release jobs wait" {
			t.Fatalf("hung copy: %+v", e)
		}
	})
	t.Run("restart keeps precedence", func(t *testing.T) {
		f := newLivenessFixture(t)
		f.say("Deployer is restarting itself onto the published scripts")
		f.clock = f.clock.Add(time.Second)
		f.beat()
		f.report(codeProgress(refusedCode))
		f.enqueue()
		f.sweep()
		f.reporting(codeProgress(refusedCode), livenessN)
		if n := f.sweep(); n != 2 {
			t.Fatalf("posted %d, want 2", n)
		}
		if e := f.copies(f.own)[0]; e.Refs["reason"] != "restart" {
			t.Fatalf("restart copy: %+v", e)
		}
	})
	t.Run("a current report re-arms", func(t *testing.T) {
		f := newLivenessFixture(t)
		opened(f, refusedCode)
		if n := f.sweep(); n != 2 {
			t.Fatalf("posted %d, want 2", n)
		}
		state := func() (st string) {
			t.Helper()
			if err := f.s.db.QueryRow(`SELECT state FROM release_deployer_liveness WHERE task_id=?`, f.task.ID).Scan(&st); err != nil {
				t.Fatal(err)
			}
			return st
		}
		current := codeProgress(api.ReleaseProgressCode{State: "current", Loaded: codePublished, Published: codePublished, Since: "2026-10-27T18:30:00Z"})
		f.report(current)
		if n := f.sweep(); n != 0 || state() != "armed" {
			t.Fatalf("current report: posted %d, state %s", n, state())
		}
		f.reporting(current, livenessN)
		f.want("re-armed", 1, 1)
		f.report(codeProgress(refusedCode))
		if n := f.sweep(); n != 2 {
			t.Fatalf("second refusal posted %d, want 2", n)
		}
		f.want("second episode", 2, 2)
		if e := f.copies(f.h)[1]; e.Refs["reason"] != "stale_code" {
			t.Fatalf("second copy: %+v", e)
		}
	})
}
