package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

type pauseFixture struct {
	s      *Store
	ctx    context.Context
	by     api.Caller
	task   api.Task
	agents []api.Agent
	item   api.WorkItem
}

func newPauseFixture(t *testing.T, withWorker bool) pauseFixture {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pause.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := pauseFixture{s: s, ctx: context.Background(), by: api.Caller{Node: "fixture", User: "owner"}}
	f.task, err = s.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Synthetic pause project", Orchestrator: "lead"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "lead", Host: "fixture", Session: "lead", Runtime: "codex"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler", Host: "fixture", Session: "handler", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	f.agents = []api.Agent{lead, handler}
	if withWorker {
		worker, addErr := s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "worker", Host: "fixture", Session: "worker", Runtime: "codex"}, f.by)
		if addErr != nil {
			t.Fatal(addErr)
		}
		f.agents = append(f.agents, worker)
	}
	f.item, err = s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Retained unfinished work", Description: "synthetic history", Priority: "high", RequestID: "pause-fixture-item"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f pauseFixture) pauseRequest(key string) api.PauseProjectRequest {
	targets := make([]api.ProjectPauseTargetRequest, 0, len(f.agents))
	for _, agent := range f.agents {
		targets = append(targets, api.ProjectPauseTargetRequest{AgentID: agent.ID, RunID: agent.RunID, ServiceDisposition: api.PauseServiceNone})
	}
	return api.PauseProjectRequest{Version: 1, RequestID: key, ExpectedLifecycleGeneration: 0, Targets: targets}
}

func TestPauseProjectPreservesHistoryAndResumesFreshGeneration(t *testing.T) {
	f := newPauseFixture(t, true)
	req := f.pauseRequest("pause-project-lost-response")
	paused, err := f.s.PauseProject(f.ctx, f.task.ID, req, f.by)
	if err != nil || paused.State != api.ProjectPauseCleanupPending || paused.PauseGeneration != 1 || paused.LifecycleGeneration != 1 || paused.CleanupPending != len(f.agents) || paused.Receipt == nil {
		t.Fatalf("pause: %+v err=%v", paused, err)
	}
	firstReceipt := paused.Receipt.ID
	replay, err := f.s.PauseProject(f.ctx, f.task.ID, req, f.by)
	if err != nil || !replay.Replay || replay.Receipt == nil || replay.Receipt.ID != firstReceipt {
		t.Fatalf("lost-response replay: %+v err=%v", replay, err)
	}
	changed := req
	changed.Targets = append([]api.ProjectPauseTargetRequest(nil), req.Targets...)
	changed.Targets[0].HandoffNote = "changed"
	if _, err = f.s.PauseProject(f.ctx, f.task.ID, changed, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed replay = %v", err)
	}
	for _, agent := range f.agents {
		current, getErr := f.s.GetAgent(f.ctx, agent.ID)
		if getErr != nil || current.Status != api.AgentClosed || current.RunID != agent.RunID {
			t.Fatalf("target not exact-run closed: %+v err=%v", current, getErr)
		}
	}
	if item, getErr := f.s.GetWorkItem(f.ctx, f.task.ID, f.item.ID); getErr != nil || item.Status != "open" || item.Revision != f.item.Revision || item.Description != f.item.Description {
		t.Fatalf("unfinished history changed: %+v err=%v", item, getErr)
	}
	if _, err = f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "blocked", Host: "fixture", Session: "blocked", ExpectedLifecycleGeneration: 1}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("paused admission = %v", err)
	}
	for index, agent := range f.agents {
		if _, err = f.s.ReportCleanup(f.ctx, agent.ID, api.CleanupRequest{RunID: agent.RunID}, f.by); err != nil {
			t.Fatal(err)
		}
		state, statusErr := f.s.ProjectPauseStatus(f.ctx, f.task.ID)
		want := api.ProjectPauseCleanupPending
		if index == len(f.agents)-1 {
			want = api.ProjectPausePaused
		}
		if statusErr != nil || state.State != want || state.CleanupPending != len(f.agents)-index-1 {
			t.Fatalf("cleanup %d: %+v err=%v", index, state, statusErr)
		}
	}
	status, err := f.s.ProjectPauseStatus(f.ctx, f.task.ID)
	if err != nil || status.RetainedHandoffDigest == "" {
		t.Fatalf("pause status: %+v err=%v", status, err)
	}
	plannedLead := api.ProjectResumeOrchestrator{AgentID: api.NewID("agt"), RunID: api.NewID("run"), Name: "fresh-lead"}
	wrongResume := api.ResumeProjectRequest{Version: 1, RequestID: "resume-wrong-digest", ExpectedPauseGeneration: 1, ExpectedLifecycleGeneration: 1, RetainedHandoffDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SelectedTeamID: "synthetic-team", Orchestrator: plannedLead}
	if _, err = f.s.ResumeProject(f.ctx, f.task.ID, wrongResume, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("wrong digest resume = %v", err)
	}
	oldIdentity := wrongResume
	oldIdentity.RequestID = "resume-old-identity"
	oldIdentity.RetainedHandoffDigest = status.RetainedHandoffDigest
	oldIdentity.Orchestrator = api.ProjectResumeOrchestrator{AgentID: f.agents[0].ID, RunID: f.agents[0].RunID, Name: f.agents[0].Name}
	if _, err = f.s.ResumeProject(f.ctx, f.task.ID, oldIdentity, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("old exact identity resume = %v", err)
	}
	resumeReq := wrongResume
	resumeReq.RequestID = "resume-project-lost-response"
	resumeReq.RetainedHandoffDigest = status.RetainedHandoffDigest
	resumed, err := f.s.ResumeProject(f.ctx, f.task.ID, resumeReq, f.by)
	if err != nil || resumed.State != api.ProjectPauseResuming || resumed.LifecycleGeneration != 2 || resumed.Receipt == nil || resumed.ResumeAdmission == nil || !resumed.ResumeAdmission.Pending || len(resumed.Targets) != len(f.agents) {
		t.Fatalf("resume: %+v err=%v", resumed, err)
	}
	resumeReceipt := resumed.Receipt.ID
	replayedResume, err := f.s.ResumeProject(f.ctx, f.task.ID, resumeReq, f.by)
	if err != nil || !replayedResume.Replay || replayedResume.Receipt == nil || replayedResume.Receipt.ID != resumeReceipt {
		t.Fatalf("resume replay: %+v err=%v", replayedResume, err)
	}
	if _, err = f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "other", Host: "fixture", Session: "other", ExpectedLifecycleGeneration: 2}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("non-plan admission while resuming = %v", err)
	}
	if _, err = f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "stale", Host: "fixture", Session: "stale", ExpectedLifecycleGeneration: 1}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale generation admission = %v", err)
	}
	freshRequest := api.AddAgentRequest{AgentID: plannedLead.AgentID, ExpectedRunID: plannedLead.RunID, ResumeReceiptID: resumeReceipt, Name: plannedLead.Name, Host: "fixture", Session: "fresh-lead", Runtime: "codex", ExpectedLifecycleGeneration: 2}
	fresh, err := f.s.AddAgent(f.ctx, f.task.ID, freshRequest, f.by)
	if err != nil || fresh.RunID == "" {
		t.Fatalf("fresh generation admission: %+v err=%v", fresh, err)
	}
	if retry, retryErr := f.s.AddAgent(f.ctx, f.task.ID, freshRequest, f.by); retryErr != nil || retry.ID != fresh.ID || retry.RunID != fresh.RunID {
		t.Fatalf("fresh lead admission response-loss retry: %+v err=%v", retry, retryErr)
	}
	resumed, err = f.s.ProjectPauseStatus(f.ctx, f.task.ID)
	if err != nil || resumed.State != api.ProjectPauseResuming || resumed.ResumeAdmission == nil || !resumed.ResumeAdmission.Pending {
		t.Fatalf("unconfirmed host launch cleared resume barrier: %+v err=%v", resumed, err)
	}
	confirm := api.ConfirmProjectResumeRequest{Version: 1, RequestID: "confirm-resume-launch", ExpectedLifecycleGeneration: 2,
		ResumeReceiptID: resumeReceipt, AgentID: fresh.ID, RunID: fresh.RunID}
	wrongConfirm := confirm
	wrongConfirm.RequestID = "confirm-wrong-receipt"
	wrongConfirm.ResumeReceiptID = api.NewID("ppr")
	if _, err = f.s.ConfirmProjectResume(f.ctx, f.task.ID, wrongConfirm, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("wrong resume confirmation = %v", err)
	}
	resumed, err = f.s.ConfirmProjectResume(f.ctx, f.task.ID, confirm, f.by)
	if err != nil || resumed.State != api.ProjectPauseActive || resumed.ResumeAdmission == nil || resumed.ResumeAdmission.Pending || resumed.Receipt == nil {
		t.Fatalf("exact host launch confirmation did not clear barrier: %+v err=%v", resumed, err)
	}
	confirmReceipt := resumed.Receipt.ID
	confirmedReplay, err := f.s.ConfirmProjectResume(f.ctx, f.task.ID, confirm, f.by)
	if err != nil || !confirmedReplay.Replay || confirmedReplay.Receipt == nil || confirmedReplay.Receipt.ID != confirmReceipt {
		t.Fatalf("resume confirmation replay: %+v err=%v", confirmedReplay, err)
	}
	if retry, retryErr := f.s.AddAgent(f.ctx, f.task.ID, freshRequest, f.by); retryErr != nil || retry.ID != fresh.ID || retry.RunID != fresh.RunID {
		t.Fatalf("confirmed fresh lead response-loss retry: %+v err=%v", retry, retryErr)
	}
	currentTask, err := f.s.GetTask(f.ctx, f.task.ID)
	if err != nil || currentTask.Orchestrator != plannedLead.Name {
		t.Fatalf("fresh exact lead not atomically selected: %+v err=%v", currentTask, err)
	}
	for _, old := range f.agents {
		current, _ := f.s.GetAgent(f.ctx, old.ID)
		if current.Status != api.AgentClosed || current.RunID != old.RunID {
			t.Fatalf("resume resurrected old run: %+v", current)
		}
	}
}

func TestPauseProjectUnresolvedServiceBlocksFullyPausedUntilDurableHandoff(t *testing.T) {
	f := newPauseFixture(t, false)
	req := f.pauseRequest("pause-with-service")
	req.Targets[0].ServiceDisposition = api.PauseServiceUnresolved
	req.Targets[0].HandoffNote = "synthetic listener needs an owner"
	status, err := f.s.PauseProject(f.ctx, f.task.ID, req, f.by)
	if err != nil || status.HandoffPending != len(f.agents) {
		t.Fatalf("pause unresolved: %+v err=%v", status, err)
	}
	for _, agent := range f.agents {
		if _, err = f.s.ReportCleanup(f.ctx, agent.ID, api.CleanupRequest{RunID: agent.RunID}, f.by); err != nil {
			t.Fatal(err)
		}
	}
	status, _ = f.s.ProjectPauseStatus(f.ctx, f.task.ID)
	if status.State != api.ProjectPauseCleanupPending || status.CleanupPending != 0 || status.HandoffPending != 1 {
		t.Fatalf("unresolved service disappeared: %+v", status)
	}
	resolve := api.ResolvePauseHandoffRequest{Version: 1, RequestID: "resolve-service", PauseGeneration: 1, AgentID: f.agents[0].ID, RunID: f.agents[0].RunID, ServiceDisposition: api.PauseServiceNone}
	resolved, err := f.s.ResolvePauseHandoff(f.ctx, f.task.ID, resolve, f.by)
	if err != nil || resolved.State != api.ProjectPausePaused || resolved.HandoffPending != 0 || resolved.Receipt == nil {
		t.Fatalf("resolve: %+v err=%v", resolved, err)
	}
	replay, err := f.s.ResolvePauseHandoff(f.ctx, f.task.ID, resolve, f.by)
	if err != nil || !replay.Replay || replay.Receipt == nil || replay.Receipt.ID != resolved.Receipt.ID {
		t.Fatalf("resolve replay: %+v err=%v", replay, err)
	}
}

func TestPauseProjectRejectsUnverifiedBrowserServiceTransfer(t *testing.T) {
	f := newPauseFixture(t, false)
	req := f.pauseRequest("pause-false-transfer")
	req.Targets[0].ServiceDisposition = api.PauseServiceTransferred
	req.Targets[0].HandoffNote = "browser claims a transfer"
	if _, err := f.s.PauseProject(f.ctx, f.task.ID, req, f.by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("transfer without exact evidence = %v", err)
	}
	req.Targets[0].ServiceEvidence = &api.OperationalReference{ID: api.NewID("opr"), Version: 1}
	if _, err := f.s.PauseProject(f.ctx, f.task.ID, req, f.by); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown service evidence = %v", err)
	}
	status, err := f.s.ProjectPauseStatus(f.ctx, f.task.ID)
	if err != nil || status.State != api.ProjectPauseActive || status.PauseGeneration != 0 {
		t.Fatalf("failed false transfer changed state: %+v err=%v", status, err)
	}
}

func TestPauseProjectVerifiesExactRunServiceEvidenceAndRejectsStaleOwnership(t *testing.T) {
	f := opFixtureNew(t)
	f.start(t)
	candidate := f.candidate(t)
	verification := f.commit(t, f.verificationData(t, candidate, "pass"), f.worker)
	note := "synthetic service detached and independently reverified"
	data := f.data("result")
	data.Result = &api.OperationalResult{Candidate: opRef(candidate), Verifications: []api.OperationalReference{opRef(verification)}, Summary: note}
	evidence := f.commit(t, data, f.worker)
	ref := opRef(evidence)
	stale := api.PauseProjectRequest{Version: 1, RequestID: "pause-stale-service-owner", ExpectedLifecycleGeneration: 0,
		Targets: []api.ProjectPauseTargetRequest{
			{AgentID: f.lead.ID, RunID: f.lead.RunID, ServiceDisposition: api.PauseServiceTransferred, HandoffNote: note, ServiceEvidence: &ref},
			{AgentID: f.worker.ID, RunID: f.worker.RunID, ServiceDisposition: api.PauseServiceNone},
		}}
	if _, err := f.s.PauseProject(context.Background(), f.task.ID, stale, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("service evidence from a different exact run = %v", err)
	}
	request := stale
	request.RequestID = "pause-verified-service-owner"
	request.Targets[0] = api.ProjectPauseTargetRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, ServiceDisposition: api.PauseServiceNone}
	request.Targets[1] = api.ProjectPauseTargetRequest{AgentID: f.worker.ID, RunID: f.worker.RunID, ServiceDisposition: api.PauseServiceDetached, HandoffNote: note, ServiceEvidence: &ref}
	status, err := f.s.PauseProject(context.Background(), f.task.ID, request, f.by)
	if err != nil || status.State != api.ProjectPauseCleanupPending || status.HandoffPending != 1 {
		t.Fatalf("verified service pause: %+v err=%v", status, err)
	}
	found := false
	for _, target := range status.Targets {
		if target.AgentID == f.worker.ID {
			found = target.RunID == f.worker.RunID && target.ServiceVerified && target.ServiceDisposition == api.PauseServiceDetached && target.ServiceEvidence != nil && *target.ServiceEvidence == ref
		}
	}
	if !found {
		t.Fatalf("verified exact-run service evidence missing: %+v", status.Targets)
	}
	for _, agent := range []api.Agent{f.lead, f.worker} {
		if _, err = f.s.ReportCleanup(context.Background(), agent.ID, api.CleanupRequest{RunID: agent.RunID}, f.by); err != nil {
			t.Fatal(err)
		}
	}
	status, err = f.s.ProjectPauseStatus(context.Background(), f.task.ID)
	if err != nil || status.State != api.ProjectPausePaused || status.CleanupPending != 0 || status.HandoffPending != 0 {
		t.Fatalf("verified service cleanup completion: %+v err=%v", status, err)
	}
}

func TestPauseProjectIncludesExitedRunWithUnconfirmedCleanup(t *testing.T) {
	f := newPauseFixture(t, false)
	exited := api.AgentExited
	if _, err := f.s.UpdateAgent(f.ctx, f.agents[0].ID, api.UpdateAgentRequest{Status: &exited}, f.by); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "replacement-lead", Host: "fixture", Session: "replacement-lead", Runtime: "codex"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	f.agents = append(f.agents, replacement)
	status, err := f.s.PauseProject(f.ctx, f.task.ID, f.pauseRequest("pause-exited-unconfirmed"), f.by)
	if err != nil || status.CleanupPending != len(f.agents) || len(status.Targets) != len(f.agents) {
		t.Fatalf("pause did not retain exited unsettled run: %+v err=%v", status, err)
	}
	foundExited := false
	for _, target := range status.Targets {
		if target.AgentID == f.agents[0].ID && target.RunID == f.agents[0].RunID {
			foundExited = target.OriginalStatus == api.AgentExited && !target.CleanupDone
		}
	}
	if !foundExited {
		t.Fatalf("exited exact run missing from retained targets: %+v", status.Targets)
	}
}

func TestPauseProjectBlocksLeadChangesAndQueueDispatch(t *testing.T) {
	f := newPauseFixture(t, true)
	other, err := f.s.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Unrelated active project", Orchestrator: "other-lead"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	otherLead, err := f.s.AddAgent(f.ctx, other.ID, api.AddAgentRequest{Name: "other-lead", Host: "fixture", Session: "other-lead", Runtime: "codex"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PauseProject(f.ctx, f.task.ID, f.pauseRequest("pause-lifecycle-gates"), f.by); err != nil {
		t.Fatal(err)
	}
	running := api.AgentRunning
	if _, err = f.s.UpdateAgent(f.ctx, f.agents[0].ID, api.UpdateAgentRequest{Status: &running}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("exact paused run reactivation = %v", err)
	}
	name := "changed-while-paused"
	if _, err = f.s.UpdateTask(f.ctx, f.task.ID, api.UpdateTaskRequest{Orchestrator: &name}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("orchestrator update while paused = %v", err)
	}
	leadReq := api.AssignLeadRequest{
		RequestID: "assign-while-paused", ExpectedName: f.task.Orchestrator, ExpectedRevision: f.task.LeadRevision,
		PreviousAgentID: f.agents[0].ID, PreviousRunID: f.agents[0].RunID,
		AgentID: otherLead.ID, RunID: otherLead.RunID,
	}
	if _, err = f.s.AssignLead(f.ctx, f.task.ID, leadReq, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("lead assignment while paused = %v", err)
	}
	dispatch := api.DispatchWorkItemRequest{RequestID: "dispatch-from-paused", Revision: f.item.Revision, TargetTaskID: other.ID}
	if _, err = f.s.DispatchWorkItem(f.ctx, f.task.ID, f.item.ID, dispatch, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("Queue dispatch from paused project = %v", err)
	}
	otherItem, err := f.s.CreateWorkItem(f.ctx, other.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Target pause gate", RequestID: "target-pause-item"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	dispatch = api.DispatchWorkItemRequest{RequestID: "dispatch-to-paused", Revision: otherItem.Revision, TargetTaskID: f.task.ID}
	if _, err = f.s.DispatchWorkItem(f.ctx, other.ID, otherItem.ID, dispatch, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("Queue dispatch to paused project = %v", err)
	}
	current, err := f.s.GetTask(f.ctx, f.task.ID)
	if err != nil || current.Orchestrator != f.task.Orchestrator || current.LeadRevision != f.task.LeadRevision {
		t.Fatalf("blocked mutations changed paused project: %+v err=%v", current, err)
	}
}

func TestPauseProjectInvalidatesAllocationIntentsAndBlocksNewAuthoring(t *testing.T) {
	f := newPauseFixture(t, false)
	order := contextLinkedMessage(t, f.s, f.task, f.item, "Bounded synthetic pause order", "pause-intent-order", nil)
	request := api.CreateAllocationIntentRequest{
		AgentID: api.NewID("agt"), TargetTaskID: f.task.ID, ItemTaskID: f.task.ID, ItemID: f.item.ID,
		ItemRevision: f.item.Revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: order.Seq},
		ContextDigest: strings.Repeat("a", 64), TeamRole: api.TeamRoleMember,
		AuthorAgentID: f.agents[0].ID, AuthorRunID: f.agents[0].RunID,
		ExpectedRunID: api.NewID("run"), RequestID: "pause-existing-intent",
	}
	if _, err := f.s.CreateAllocationIntent(f.ctx, f.task.ID, request, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PauseProject(f.ctx, f.task.ID, f.pauseRequest("pause-invalidates-intent"), f.by); err != nil {
		t.Fatal(err)
	}
	intent, err := f.s.GetAllocationIntent(f.ctx, f.task.ID, request.AgentID)
	if err != nil || intent.InvalidatedAt == nil || intent.InvalidatedPauseGeneration != 1 || intent.ConsumedAt != nil {
		t.Fatalf("allocation intent not durably invalidated: %+v err=%v", intent, err)
	}
	request.AgentID, request.ExpectedRunID, request.RequestID = api.NewID("agt"), api.NewID("run"), "pause-new-intent"
	if _, err = f.s.CreateAllocationIntent(f.ctx, f.task.ID, request, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("allocation intent authored while paused = %v", err)
	}
}

func TestPauseProjectBlocksQueueSchedulingForPausedSourceOrTarget(t *testing.T) {
	for _, pausedSide := range []string{"source", "target"} {
		t.Run(pausedSide, func(t *testing.T) {
			s, ctx, by := workItemStore(t)
			source, sourceLead := workItemProject(t, s, ctx, by, "Pause Queue source", "source-lead")
			target, targetLead := workItemProject(t, s, ctx, by, "Pause Queue target", "target-lead")
			item := createWorkItem(t, s, ctx, by, source, "pause-queue-item")
			dispatch, err := s.DispatchWorkItem(ctx, source.ID, item.ID, api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "pause-queue-dispatch"}, by)
			if err != nil {
				t.Fatal(err)
			}
			pausedTask, pausedLead := source, sourceLead
			if pausedSide == "target" {
				pausedTask, pausedLead = target, targetLead
			}
			pause := api.PauseProjectRequest{Version: 1, RequestID: "pause-queue-" + pausedSide, ExpectedLifecycleGeneration: 0,
				Targets: []api.ProjectPauseTargetRequest{{AgentID: pausedLead.ID, RunID: pausedLead.RunID, ServiceDisposition: api.PauseServiceNone}}}
			if _, err = s.PauseProject(ctx, pausedTask.ID, pause, by); err != nil {
				t.Fatal(err)
			}
			entry := dispatch.Queue.Entry
			action := api.QueueActionRequest{Operation: "claim", RequestID: "claim-with-paused-" + pausedSide, ExpectedRevision: entry.Revision, Cycle: entry.Cycle}
			if _, err = s.QueueAction(ctx, target.ID, entry.ID, action, by); !errors.Is(err, api.ErrConflict) {
				t.Fatalf("Queue scheduling with paused %s = %v", pausedSide, err)
			}
			current, err := s.GetQueueEntry(ctx, target.ID, entry.ID)
			if err != nil || current.State != api.QueueStateWaiting || current.Revision != entry.Revision {
				t.Fatalf("blocked Queue action mutated entry: %+v err=%v", current, err)
			}
		})
	}
}

func TestPauseProjectConcurrentAdmissionCannotEscapeSnapshot(t *testing.T) {
	for iteration := 0; iteration < 12; iteration++ {
		f := newPauseFixture(t, false)
		req := f.pauseRequest("pause-race")
		var wg sync.WaitGroup
		wg.Add(2)
		var pauseStatus api.ProjectPauseStatus
		var pauseErr, addErr error
		var admitted api.Agent
		go func() {
			defer wg.Done()
			pauseStatus, pauseErr = f.s.PauseProject(f.ctx, f.task.ID, req, f.by)
		}()
		go func() {
			defer wg.Done()
			admitted, addErr = f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "racer", Host: "fixture", Session: "racer"}, f.by)
		}()
		wg.Wait()
		if pauseErr == nil {
			if addErr == nil {
				t.Fatalf("iteration %d admitted run %s outside successful pause snapshot %+v", iteration, admitted.RunID, pauseStatus)
			}
			if !errors.Is(addErr, api.ErrConflict) {
				t.Fatalf("iteration %d unexpected admission error %v", iteration, addErr)
			}
		} else {
			if addErr != nil || !errors.Is(pauseErr, api.ErrConflict) {
				t.Fatalf("iteration %d pause=%v admission=%v", iteration, pauseErr, addErr)
			}
			current, getErr := f.s.GetAgent(f.ctx, admitted.ID)
			if getErr != nil || current.Status == api.AgentClosed {
				t.Fatalf("iteration %d successful concurrent admission was closed: %+v err=%v", iteration, current, getErr)
			}
		}
	}
}

func TestPauseProjectStaleExactRunCannotCloseReplacementOrOtherProject(t *testing.T) {
	f := newPauseFixture(t, false)
	old := f.agents[0]
	exited := api.AgentExited
	if _, err := f.s.UpdateAgent(f.ctx, old.ID, api.UpdateAgentRequest{Status: &exited}, f.by); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: old.Name, Host: old.Host, Session: "replacement", Runtime: old.Runtime}, f.by)
	if err != nil || replacement.RunID == old.RunID {
		t.Fatalf("replacement: %+v err=%v", replacement, err)
	}
	req := f.pauseRequest("stale-exact-run")
	if _, err = f.s.PauseProject(f.ctx, f.task.ID, req, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale snapshot pause = %v", err)
	}
	current, _ := f.s.GetAgent(f.ctx, replacement.ID)
	if current.RunID != replacement.RunID || current.Status == api.AgentClosed {
		t.Fatalf("stale pause killed replacement: %+v", current)
	}
	other, err := f.s.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Unrelated synthetic project"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	otherAgent, err := f.s.AddAgent(f.ctx, other.ID, api.AddAgentRequest{Name: "unrelated", Host: "fixture", Session: "unrelated"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	currentOther, _ := f.s.GetAgent(f.ctx, otherAgent.ID)
	if currentOther.Status == api.AgentClosed {
		t.Fatalf("unrelated project touched: %+v", currentOther)
	}
}
