package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
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
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "release item", RequestID: "item"}, by)
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
	return s, task, h, d, entry
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
	if err != nil {
		t.Fatal(err)
	}
	req = api.ReleaseRequest{RequestID: "merged", Operation: "merged", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: imported.Generation, IntegratedCommit: p.Commit}
	if _, err = s.ReleaseAction(ctx, task.ID, req); err != nil {
		t.Fatal(err)
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
	seedPassingVerificationAt(t, s, item, candidateB, worktree)
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
	if _, err = accept("other-worktree", "/fixture/worktrees/other", candidateC); !errors.Is(err, api.ErrConflict) {
		t.Fatal("a plan for a different worktree was accepted", err)
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
