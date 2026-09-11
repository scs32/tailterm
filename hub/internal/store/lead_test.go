package store

import (
	"context"
	"errors"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"testing"
)

func TestLeadAssignmentExactRunsRetryAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "lead recovery", Orchestrator: "old"}, by)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name string) api.Agent {
		a, e := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Host: "fixture", Session: name, Runtime: "generic"}, by)
		if e != nil {
			t.Fatal(e)
		}
		_, e = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted}, by)
		if e != nil {
			t.Fatal(e)
		}
		return a
	}
	old := add("old")
	next := add("replacement")
	other := add("other")
	req := api.AssignLeadRequest{RequestID: "recover-1", ExpectedName: "old", PreviousAgentID: old.ID, PreviousRunID: old.RunID, AgentID: next.ID, RunID: next.RunID}
	bad := req
	bad.RunID = api.NewID("run")
	if _, err = s.AssignLead(ctx, task.ID, bad, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("wrong target run: %v", err)
	}
	bad = req
	bad.PreviousRunID = api.NewID("run")
	if _, err = s.AssignLead(ctx, task.ID, bad, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("wrong previous run: %v", err)
	}
	result, err := s.AssignLead(ctx, task.ID, req, by)
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.Orchestrator != next.Name || result.Task.LeadRevision != 1 || result.MessageSeq < 1 {
		t.Fatalf("receipt: %+v", result)
	}

	current, err := currentQueueOrchestrator(s.db, ctx, result.Task)
	if err != nil || current.ID != next.ID || current.RunID != next.RunID {
		t.Fatalf("recipient: %+v %v", current, err)
	}
	if _, err = s.db.Exec(`UPDATE agents SET run_id=? WHERE id=?`, api.NewID("run"), next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = currentQueueOrchestrator(s.db, ctx, result.Task); err == nil {
		t.Fatal("dispatch accepted replacement run without explicit assignment")
	}
	if _, err = s.db.Exec(`UPDATE agents SET run_id=? WHERE id=?`, next.RunID, next.ID); err != nil {
		t.Fatal(err)
	}
	original, _ := s.GetAgent(ctx, old.ID)
	if original.RunID != old.RunID || original.Status != api.AgentRunning {
		t.Fatalf("old modified: %+v", original)
	}
	messages, err := s.ListMessages(ctx, task.ID, 0, "", 100)
	if err != nil || len(messages) != 1 || messages[0].To != next.ID {
		t.Fatalf("notice: %+v %v", messages, err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.AssignLead(ctx, task.ID, req, by)
	if err != nil || retry.MessageSeq != result.MessageSeq {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	bad = req
	bad.AgentID = other.ID
	bad.RunID = other.RunID
	if _, err = s.AssignLead(ctx, task.ID, bad, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("key reuse: %v", err)
	}
	bad.RequestID = "stale-other"
	if _, err = s.AssignLead(ctx, task.ID, bad, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale selection: %v", err)
	}
	name := "temporary"
	s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &name}, by)
	name = next.Name
	s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &name}, by)
	bad = api.AssignLeadRequest{RequestID: "stale-revision", ExpectedName: next.Name, ExpectedRevision: 1, PreviousAgentID: next.ID, PreviousRunID: next.RunID, AgentID: other.ID, RunID: other.RunID}
	if _, err = s.AssignLead(ctx, task.ID, bad, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("ABA: %v", err)
	}
	saved, _ := s.GetTask(ctx, task.ID)
	if saved.LeadRevision != 3 {
		t.Fatalf("revision: %+v", saved)
	}
	if _, err = s.CloseTask(ctx, task.ID, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AssignLead(ctx, task.ID, req, by); err != nil {
		t.Fatal(err)
	}
	messages, _ = s.ListMessages(ctx, task.ID, 0, "", 100)
	if len(messages) != 1 {
		t.Fatalf("duplicate notices: %d", len(messages))
	}
}

func TestLeadAssignmentEligibilityAndAtomicFailure(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "eligibility"}, by)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "candidate", Host: "fixture", Session: "candidate", Runtime: "generic"}, by)
	if err != nil {
		t.Fatal(err)
	}
	req := api.AssignLeadRequest{RequestID: "eligible", AgentID: candidate.ID, RunID: candidate.RunID}
	for _, status := range []string{"retired", "closed", "exited"} {
		if _, err = s.db.Exec(`UPDATE agents SET status=?,last_seen_at=? WHERE id=?`, status, ts(s.now()), candidate.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = s.AssignLead(ctx, task.ID, req, by); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("accepted %s: %v", status, err)
		}
	}
	if _, err = s.db.Exec(`UPDATE agents SET status='running',last_seen_at='',role='' WHERE id=?`, candidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AssignLead(ctx, task.ID, req, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("accepted offline: %v", err)
	}
	if _, err = s.db.Exec(`UPDATE agents SET last_seen_at=?,role='database_handler' WHERE id=?`, ts(s.now()), candidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AssignLead(ctx, task.ID, req, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("accepted handler: %v", err)
	}
	if _, err = s.db.Exec(`UPDATE agents SET role='' WHERE id=?`, candidate.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`CREATE TRIGGER fail_lead_notice BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT,'fixture notice failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AssignLead(ctx, task.ID, req, by); err == nil {
		t.Fatal("injected failure ignored")
	}
	saved, _ := s.GetTask(ctx, task.ID)
	if saved.Orchestrator != "" || saved.LeadRevision != 0 {
		t.Fatalf("partial assignment: %+v", saved)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM lead_assignments`).Scan(&count)
	if count != 0 {
		t.Fatal("partial receipt")
	}
	if _, err = s.db.Exec(`DROP TRIGGER fail_lead_notice`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AssignLead(ctx, task.ID, req, by); err != nil {
		t.Fatal(err)
	}
}
