package store

import (
	"context"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"testing"
)

func TestBlockerReasonPersistsAndClearsOnResume(t *testing.T) {
	s, e := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	by := api.Caller{Node: "host", User: "owner"}
	task, e := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Blocked"}, by)
	if e != nil {
		t.Fatal(e)
	}
	a, e := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "agent", Host: "host", Session: "agent"}, by)
	if e != nil {
		t.Fatal(e)
	}
	for _, reason := range []string{"permission", "authentication", "tool"} {
		_, e = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventNeedsInput, Text: "Exact cause", Data: map[string]any{"reason": reason}}, by)
		if e != nil {
			t.Fatal(e)
		}
		got, e := s.GetAgent(ctx, a.ID)
		if e != nil || got.BlockedReason != reason || got.BlockedText != "Exact cause" || got.Status != api.AgentNeedsInput {
			t.Fatal(got, e)
		}
	}
	_, e = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventDone, Data: map[string]any{"runtimeStop": true}}, by)
	if e != nil {
		t.Fatal(e)
	}
	blocked, e := s.GetAgent(ctx, a.ID)
	if e != nil || blocked.Status != api.AgentNeedsInput || blocked.BlockedReason != "tool" {
		t.Fatal("runtime stop erased blocker", blocked, e)
	}
	_, e = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventRunning}, by)
	if e != nil {
		t.Fatal(e)
	}
	got, e := s.GetAgent(ctx, a.ID)
	if e != nil || got.BlockedReason != "" || got.BlockedText != "" {
		t.Fatal(got, e)
	}
	_, e = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventNeedsInput, Text: "Permission blocked: approval required"}, by)
	if e != nil {
		t.Fatal(e)
	}
	got, e = s.GetAgent(ctx, a.ID)
	if e != nil || got.BlockedReason != "permission" {
		t.Fatal(got, e)
	}
}
