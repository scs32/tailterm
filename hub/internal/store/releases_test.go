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
