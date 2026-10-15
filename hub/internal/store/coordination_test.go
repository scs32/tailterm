package store

import (
	"context"
	"errors"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"strings"
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

func TestTaskTitlesAreNotShellIdentifiers(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Review API changes — phase 2"}, api.Caller{})
	if err != nil {
		t.Fatal(err)
	}
	name := "Plan the next release"
	if _, err = s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Name: &name}, api.Caller{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "  ", "bad\nname"} {
		if _, err = s.CreateTask(ctx, api.CreateTaskRequest{Name: name}, api.Caller{}); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("invalid title accepted: %q", name)
		}
	}
}

func TestTaskHelperPolicyPersistsAndIsEnforced(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "policy.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	by := api.Caller{Node: "owner", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Review"}, by)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "planner", Host: "host", Session: "planner"}, by)
	if err != nil {
		t.Fatal(err)
	}
	child := api.AddAgentRequest{Name: "helper", Host: "host", Session: "helper", ParentAgentID: parent.ID}
	if _, err = s.AddAgent(ctx, task.ID, child, by); !errors.Is(err, api.ErrAgentSpawnDisabled) {
		t.Fatalf("default policy: %v", err)
	}
	yes := true
	updated, err := s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{AllowAgentSpawn: &yes}, by)
	if err != nil || !updated.AllowAgentSpawn {
		t.Fatalf("enable: %+v %v", updated, err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	helper, err := s.AddAgent(ctx, task.ID, child, by)
	if err != nil || helper.ParentAgentID != parent.ID {
		t.Fatalf("helper: %+v %v", helper, err)
	}
	no := false
	if _, err = s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{AllowAgentSpawn: &no}, by); err != nil {
		t.Fatal(err)
	}
	child.Name = "another"
	child.Session = "another"
	if _, err = s.AddAgent(ctx, task.ID, child, by); !errors.Is(err, api.ErrAgentSpawnDisabled) {
		t.Fatalf("disabled policy: %v", err)
	}
	child.ParentAgentID = ""
	if _, err = s.AddAgent(ctx, task.ID, child, by); err != nil {
		t.Fatalf("owner must still be able to add agents: %v", err)
	}
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Other", AllowAgentSpawn: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	child.ParentAgentID = parent.ID
	if _, err = s.AddAgent(ctx, other.ID, child, by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("cross-task parent: %v", err)
	}
	agents, err := s.ListAgents(ctx, task.ID)
	if err != nil || len(agents) != 3 {
		t.Fatalf("existing agents affected: %v %v", agents, err)
	}
}

func TestProjectGoalWorkItemIDs(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	caller := api.Caller{Node: "host", User: "owner"}
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "other", Goal: "own work"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.CreateWorkItem(ctx, other.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Foreign item", Priority: "normal", RequestID: "goal-foreign"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	refused := func(err error, id string) {
		t.Helper()
		if !errors.Is(err, api.ErrInvalid) || !strings.Contains(err.Error(), "tsk_X/"+id) {
			t.Fatalf("unqualified foreign %s not refused with the qualified form: %v", id, err)
		}
	}
	_, err = s.CreateTask(ctx, api.CreateTaskRequest{Name: "unqualified", Goal: "Feature " + foreign.ID + "."}, caller)
	refused(err, foreign.ID)
	qualified := "Feature " + other.ID + "/" + foreign.ID + ", and tsk_0123456789abcdef/wi_0123456789abcdef elsewhere."
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "qualified", Goal: qualified}, caller)
	if err != nil || task.Goal != qualified {
		t.Fatalf("qualified goal at create: %+v %v", task, err)
	}
	local, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Local item", Priority: "normal", RequestID: "goal-local"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	for _, goal := range []string{"Fix " + foreign.ID, "Fix " + local.ID + " then wi_0123456789abcdef"} {
		_, err = s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Goal: &goal}, caller)
		refused(err, strings.Fields(goal)[len(strings.Fields(goal))-1])
	}
	if got, err := s.GetTask(ctx, task.ID); err != nil || got.Goal != qualified {
		t.Fatalf("refused update changed the goal: %q %v", got.Goal, err)
	}
	for _, goal := range []string{"Fix " + local.ID + ".", "Fix tsk_0123456789abcdef/wi_0123456789abcdef", "No ids, or wi_ shaped noise like wi_0123 and xwi_0123456789abcdef."} {
		got, err := s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Goal: &goal}, caller)
		if err != nil || got.Goal != goal {
			t.Fatalf("goal %q: %+v %v", goal, got, err)
		}
	}
	// A goal saved before this check, holding a foreign id, survives every update that leaves the goal alone.
	legacy := "Feature " + foreign.ID
	if _, err := s.db.ExecContext(ctx, `UPDATE tasks SET goal=? WHERE id=?`, legacy, task.ID); err != nil {
		t.Fatal(err)
	}
	name, lead, open, spawn, agents, swarm := "renamed", "lead", api.TaskOpen, true, 3, true
	for _, req := range []api.UpdateTaskRequest{{Name: &name}, {Orchestrator: &lead}, {Status: &open}, {AllowAgentSpawn: &spawn}, {MaxNewAgents: &agents}, {Swarm: &swarm}} {
		got, err := s.UpdateTask(ctx, task.ID, req, caller)
		if err != nil || got.Goal != legacy {
			t.Fatalf("unrelated update %+v: %+v %v", req, got, err)
		}
	}
}
