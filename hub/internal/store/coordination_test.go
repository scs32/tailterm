package store

import (
	"context"
	"errors"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"testing"
)

func TestRunsInboxRepliesAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	caller := api.Caller{Node: "host", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "coordination", Goal: "verify lifecycle"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	add := api.AddAgentRequest{Name: "reviewer", Host: "host", Session: "reviewer", Runtime: "generic"}
	a, err := s.AddAgent(ctx, task.ID, add, caller)
	if err != nil || a.RunID == "" {
		t.Fatalf("add: %v %v", a, err)
	}
	post := func(kind, run string) error {
		_, e := s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: a.ID, RunID: run, Kind: kind}, caller)
		return e
	}
	if err := post(api.EventStarted, a.RunID); err != nil {
		t.Fatal(err)
	}
	first, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Review this", To: a.ID}, caller)
	if err != nil {
		t.Fatal(err)
	}
	if err := post(api.EventExited, a.RunID); err != nil {
		t.Fatal(err)
	}
	if err := post(api.EventDone, a.RunID); !errors.Is(err, api.ErrClosed) {
		t.Fatalf("exit overwritten: %v", err)
	}
	next, err := s.AddAgent(ctx, task.ID, add, caller)
	if err != nil || next.ID != a.ID || next.RunID == a.RunID || next.Unread != 1 {
		t.Fatalf("reuse: %+v %v", next, err)
	}
	if err := post(api.EventStarted, a.RunID); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("stale run accepted: %v", err)
	}
	if err := post(api.EventStarted, next.RunID); err != nil {
		t.Fatal(err)
	}
	reply, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: a.ID, Text: "Reviewed", ReplyTo: first.Seq}, caller)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.ListMessages(ctx, task.ID, -1, "", 1)
	if err != nil || len(latest) != 1 || latest[0].Seq != reply.Seq || latest[0].ReplyTo != first.Seq {
		t.Fatalf("latest reply: %v %v", latest, err)
	}
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "other"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostMessage(ctx, other.ID, api.PostMessageRequest{Text: "wrong task", ReplyTo: first.Seq}, caller); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("cross-task reply accepted: %v", err)
	}
	if err := s.MarkRead(ctx, task.ID, api.MarkReadRequest{AgentID: a.ID, UpTo: first.Seq}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := s.GetAgent(ctx, a.ID)
	if err != nil || restored.RunID != next.RunID || restored.ReadUpTo != first.Seq || restored.Status != api.AgentRunning || restored.LastSeenAt.IsZero() {
		t.Fatalf("restart: %+v %v", restored, err)
	}
}
