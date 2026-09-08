package store

import (
	"context"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"testing"
)

func TestSwarmDeliveryPersistsScopeAndIncludesLaterHelpers(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	by := api.Caller{Node: "host", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Swarm", Swarm: true, Orchestrator: "lead", AllowAgentSpawn: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name, parent string) api.Agent {
		a, e := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Host: "host", Session: name, ParentAgentID: parent}, by)
		if e != nil {
			t.Fatal(e)
		}
		return a
	}
	lead := add("lead", "")
	worker := add("worker", "")
	peer := add("peer", "")
	m, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: worker.ID, Text: "Worker owns implementation"}, by)
	if err != nil || !m.Broadcast || m.To != worker.ID {
		t.Fatalf("broadcast lost addressee: %+v %v", m, err)
	}
	helper := add("helper", worker.ID)
	for _, a := range []api.Agent{worker, peer, helper} {
		msgs, e := s.ListMessages(ctx, task.ID, 0, a.ID, 100)
		if e != nil || len(msgs) != 1 || !msgs[0].Broadcast {
			t.Fatalf("missing broadcast: %s %+v %v", a.Name, msgs, e)
		}
		n, e := s.Unread(ctx, task.ID, a.ID)
		if e != nil || n != 1 {
			t.Fatalf("unread: %d %v", n, e)
		}
	}
	if n, _ := s.Unread(ctx, task.ID, lead.ID); n != 0 {
		t.Fatal("sender should not wake itself")
	}
	if err = s.MarkRead(ctx, task.ID, api.MarkReadRequest{AgentID: peer.ID, UpTo: m.Seq}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Unread(ctx, task.ID, peer.ID); n != 0 {
		t.Fatal("read receipt missing")
	}
	if n, _ := s.Unread(ctx, task.ID, worker.ID); n != 1 {
		t.Fatal("read receipt leaked across agents")
	}
	no := false
	if _, err = s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Swarm: &no}, by); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, To: worker.ID, Text: "Directed now"}, by); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ListMessages(ctx, task.ID, 0, helper.ID, 100)
	if err != nil || len(msgs) != 1 {
		t.Fatal("toggle changed old scope or leaked new directed message", msgs, err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.GetTask(ctx, task.ID)
	if err != nil || saved.Swarm || saved.Orchestrator != "lead" {
		t.Fatalf("settings not persisted %+v %v", saved, err)
	}
	yes := true
	if _, err = s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Swarm: &yes}, by); err != nil {
		t.Fatal(err)
	}
	msgs, err = s.ListMessages(ctx, task.ID, 0, helper.ID, 100)
	if err != nil || len(msgs) != 1 {
		t.Fatal("enabling swarm replayed old directed message", msgs, err)
	}
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Other", Swarm: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PostMessage(ctx, other.ID, api.PostMessageRequest{To: worker.ID, Text: "Wrong task"}, by); err == nil {
		t.Fatal("swarm bypassed task isolation")
	}
}
