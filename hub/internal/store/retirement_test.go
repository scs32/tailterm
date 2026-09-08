package store

import (
	"context"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"testing"
)

func TestRetirementPersistsAcrossHooksAndExplicitResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	by := api.Caller{Node: "host", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Retire", AllowAgentSpawn: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "remote", Session: "worker", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "peer", Host: "remote", Session: "peer", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	status := api.AgentRetired
	a, err = s.UpdateAgent(ctx, a.ID, api.UpdateAgentRequest{Status: &status}, by)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{api.EventStarted, api.EventRunning, api.EventDone, api.EventNeedsInput, api.EventHeartbeat} {
		_, err = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: kind, Text: "late hook", Data: map[string]any{"reason": "permission", "runtimeStop": true}}, by)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetAgent(ctx, a.ID)
		if got.Status != api.AgentRetired || got.BlockedReason != "" {
			t.Fatalf("%s erased retirement: %+v", kind, got)
		}
	}
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "helper", Host: "remote", Session: "helper", ParentAgentID: a.ID}, by); err == nil {
		t.Fatal("retired parent spawned helper")
	}
	// Agent-authored messages retain retirement while leaving the mailbox usable.
	if _, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: peer.ID, To: a.ID, Text: "Follow-up retained"}, by); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, _ := s.GetAgent(ctx, a.ID)
	if got.Status != api.AgentRetired || got.Session != "worker" {
		t.Fatal("retirement not durable", got)
	}
	unread, err := s.Unread(ctx, task.ID, a.ID)
	if err != nil || unread != 1 {
		t.Fatal("mailbox lost", unread, err)
	}
	status = api.AgentDone
	got, err = s.UpdateAgent(ctx, a.ID, api.UpdateAgentRequest{Status: &status}, by)
	if err != nil || got.Status != api.AgentDone {
		t.Fatal("resume failed", got, err)
	}
	if _, err = s.CloseAgent(ctx, a.ID, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateAgent(ctx, a.ID, api.UpdateAgentRequest{Status: &status}, by); err == nil {
		t.Fatal("resumed closed agent")
	}
}
