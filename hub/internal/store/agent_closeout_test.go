package store

import (
	"context"
	"errors"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestAgentCloseoutIsRunScopedAndKeepsOpenProject(t *testing.T) {
	s, err := Open(t.TempDir() + "/hub.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	by := api.Caller{Node: "test", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Open parent"}, by)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "host", Session: "worker"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportCleanup(ctx, agent.ID, api.CleanupRequest{RunID: agent.RunID}, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("non-closed agent cleanup = %v", err)
	}
	if _, err = s.CloseAgentRun(ctx, agent.ID, api.NewID("run"), by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale close = %v", err)
	}
	unchanged, _ := s.GetAgent(ctx, agent.ID)
	if unchanged.Status == api.AgentClosed {
		t.Fatal("stale close changed the current run")
	}
	closed, err := s.CloseAgentRun(ctx, agent.ID, agent.RunID, by)
	if err != nil || closed.Status != api.AgentClosed || closed.CleanupDone {
		t.Fatalf("close intent = %+v, %v", closed, err)
	}
	if _, err = s.CloseAgentRun(ctx, agent.ID, agent.RunID, by); err != nil {
		t.Fatalf("idempotent close = %v", err)
	}
	if _, err = s.ReportCleanup(ctx, agent.ID, api.CleanupRequest{RunID: agent.RunID, Error: "receipt delivery retry"}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportCleanup(ctx, agent.ID, api.CleanupRequest{RunID: agent.RunID}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportCleanup(ctx, agent.ID, api.CleanupRequest{RunID: agent.RunID, Error: "late failure"}, by); err != nil {
		t.Fatal(err)
	}
	confirmed, _ := s.GetAgent(ctx, agent.ID)
	if !confirmed.CleanupDone || confirmed.CleanupError != "" {
		t.Fatalf("late failure undid success: %+v", confirmed)
	}
	stillOpen, _ := s.GetTask(ctx, task.ID)
	if stillOpen.Status != api.TaskOpen {
		t.Fatalf("individual closeout closed parent: %+v", stillOpen)
	}
	if _, err = s.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventRunning, AgentID: agent.ID, RunID: agent.RunID}, by); !errors.Is(err, api.ErrClosed) {
		t.Fatalf("late hook revived closed run: %v", err)
	}

	// Reusing a display name after close creates a fresh identity and run.
	replacement, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: agent.Name, Host: "host", Session: agent.Session}, by)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == agent.ID || replacement.RunID == agent.RunID {
		t.Fatalf("replacement reused closed identity: old=%+v new=%+v", agent, replacement)
	}
	closedTask, err := s.CloseTask(ctx, task.ID, by)
	if err != nil || closedTask.CleanupPending != 1 {
		t.Fatalf("task close reset prior receipt or miscounted replacement: %+v, %v", closedTask, err)
	}
	preserved, _ := s.GetAgent(ctx, agent.ID)
	if !preserved.CleanupDone {
		t.Fatal("task close reset successful individual cleanup")
	}
}
