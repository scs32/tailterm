package store

import (
	"context"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"testing"
)

func TestTaskCleanupReceiptsPersistAndAreRunScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	by := api.Caller{User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Cleanup"}, by)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "remote", Session: "worker"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportCleanup(ctx, a.ID, api.CleanupRequest{RunID: a.RunID}, by); err == nil {
		t.Fatal("acknowledged open task")
	}
	retired := api.AgentRetired
	if _, err = s.UpdateAgent(ctx, a.ID, api.UpdateAgentRequest{Status: &retired}, by); err != nil {
		t.Fatal(err)
	}
	closed, err := s.CloseTask(ctx, task.ID, by)
	if err != nil || closed.CleanupPending != 1 {
		t.Fatal(closed, err)
	}
	got, _ := s.GetAgent(ctx, a.ID)
	if got.Status != api.AgentClosed {
		t.Fatal("retired agent not closed")
	}
	if _, err = s.ReportCleanup(ctx, a.ID, api.CleanupRequest{RunID: api.NewID("run")}, by); err == nil {
		t.Fatal("accepted wrong run")
	}
	if _, err = s.ReportCleanup(ctx, a.ID, api.CleanupRequest{RunID: a.RunID, Error: "host retry"}, by); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, _ = s.GetAgent(ctx, a.ID)
	if got.CleanupDone || got.CleanupError != "host retry" {
		t.Fatal("error not durable", got)
	}
	if _, err = s.ReportCleanup(ctx, a.ID, api.CleanupRequest{RunID: a.RunID}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportCleanup(ctx, a.ID, api.CleanupRequest{RunID: a.RunID, Error: "late concurrent failure"}, by); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		closed, err = s.CloseTask(ctx, task.ID, by)
		if err != nil || closed.CleanupPending != 0 {
			t.Fatal("close erased receipt", closed, err)
		}
	}
}
