package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"sync"
	"testing"
)

func TestHelperLimitIncludesDescendantsAndFinishedAgents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	by := api.Caller{Node: "host", User: "owner"}
	limit := 2
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Limited helpers", AllowAgentSpawn: true, MaxNewAgents: &limit}, by)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name, parent string) (api.Agent, error) {
		return s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Session: name, Host: "host", ParentAgentID: parent}, by)
	}
	parent, err := add("parent", "")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := add("helper", parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := add("grandchild", helper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = add("extra", parent.ID); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("limit: %v", err)
	}
	if _, err = s.CloseAgent(ctx, grandchild.ID, by); err != nil {
		t.Fatal(err)
	}
	if _, err = add("replacement", parent.ID); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("closed helper freed lifetime allowance: %v", err)
	}
	if _, err = add("manual", ""); err != nil {
		t.Fatalf("manual member used quota: %v", err)
	}
	if _, err = s.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: helper.ID, RunID: helper.RunID, Kind: api.EventExited}, by); err != nil {
		t.Fatal(err)
	}
	resumed, err := add("helper", parent.ID)
	if err != nil || resumed.ID != helper.ID {
		t.Fatalf("existing helper should resume without new identity: %v", err)
	}
	zero := 0
	if _, err = s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{MaxNewAgents: &zero}, by); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.GetTask(ctx, task.ID)
	if err != nil || saved.MaxNewAgents != 0 {
		t.Fatalf("persisted limit: %+v %v", saved, err)
	}
	if _, err = add("blocked", parent.ID); !errors.Is(err, api.ErrAgentSpawnLimit) {
		t.Fatalf("zero limit: %v", err)
	}
	for _, bad := range []int{-1, 33} {
		if _, err = s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{MaxNewAgents: &bad}, by); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("invalid %d: %v", bad, err)
		}
	}
}
func TestConcurrentHelperRequestsShareOneTaskAllowance(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	by := api.Caller{Node: "host", User: "owner"}
	limit := 1
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "race", AllowAgentSpawn: true, MaxNewAgents: &limit}, by)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "parent", Session: "parent", Host: "host"}, by)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := fmt.Sprintf("helper%d", i)
			_, e := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: n, Session: n, Host: "host", ParentAgentID: parent.ID}, by)
			results <- e
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else if !errors.Is(e, api.ErrAgentSpawnLimit) {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatalf("allowed %d simultaneous helpers", success)
	}
}
