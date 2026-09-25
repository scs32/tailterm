package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
	"github.com/scs32/tailterm/hub/internal/teamplan"
)

type teamFixture struct {
	st                   *store.Store
	e                    env
	c                    *api.Client
	task                 api.Task
	item                 api.WorkItem
	order                int64
	handler              api.Agent
	dropFirstMemberReply *atomic.Bool
}

func newTeamFixture(t *testing.T, withHandler bool) teamFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	t.Setenv("TT_TMUX_SOCKET", "tt-team-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "team-fixture", User: "owner"}
	f := teamFixture{dropFirstMemberReply: &atomic.Bool{}}
	f.st = st
	handler := server.New(st, func(*http.Request) (api.Caller, error) { return by, nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/agents") && f.dropFirstMemberReply.CompareAndSwap(true, false) {
			handler.ServeHTTP(httptest.NewRecorder(), r)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := api.NewClient(srv.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	task, err := c.CreateTask(ctx, api.CreateTaskRequest{Name: "isolated team launch"})
	if err != nil {
		t.Fatal(err)
	}
	f.e, f.c, f.task = env{hub: srv.URL, task: task.ID}, c, task
	if withHandler {
		f.handler, err = c.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "fixture-handler", Runtime: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, Kind: api.EventRunning})
		if err != nil {
			t.Fatal(err)
		}
	}
	f.item, err = c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "fixture feature", RequestID: "team-item"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "bounded fixture order", RequestID: "team-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	f.order = message.Seq
	return f
}

func (f teamFixture) args(extra ...string) []string {
	a := []string{"launch", "--item", f.item.ID, "--order", fmt.Sprint(f.order)}
	return append(a, extra...)
}

func TestTeamLaunchDryRunPrintsFullPlanAndChangesNothing(t *testing.T) {
	f := newTeamFixture(t, true)
	before, err := f.c.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdTeam(f.e, f.args("--dry-run")) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"lead-", "planner-", "builder-", "reviewer-", "gpt-6-astra", "gpt-6-sol", "claude-fable-5-1", "reasoning=high", "promptBytes="} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry plan lacks %q: %s", want, out)
		}
	}
	if strings.Contains(out, "database-") {
		t.Fatal("template database seat was launched")
	}
	after, err := f.c.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Task.Orchestrator != before.Task.Orchestrator || len(after.Agents) != len(before.Agents) || after.LatestSeq != before.LatestSeq {
		t.Fatalf("dry run changed hub: before=%+v after=%+v", before.Task, after.Task)
	}
	path, err := teamJournalPath(f.e.hub, f.task.ID, f.item.ID, f.order)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote journal: %v", err)
	}
}

func TestTeamLaunchBinaryDryRunDoesNotBindAgentThread(t *testing.T) {
	f := newTeamFixture(t, true)
	bin := filepath.Join(t.TempDir(), "tt")
	build := exec.Command("go", "build", "-o", bin, "./cmd/tt")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v %s", err, output)
	}
	before, err := f.c.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "team", "launch", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--dry-run")
	cmd.Env = append(os.Environ(), "TAILTERM_HUB="+f.e.hub, "TAILTERM_TASK="+f.task.ID,
		"TAILTERM_AGENT="+f.handler.ID, "TAILTERM_RUN="+f.handler.RunID,
		"TAILTERM_AGENT_NAME="+f.handler.Name, "HOME="+os.Getenv("HOME"))
	if output, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(output), "promptBytes=") {
		t.Fatalf("binary dry run: %v %s", err, output)
	}
	after, err := f.c.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.LatestSeq != after.LatestSeq || before.Task.Orchestrator != after.Task.Orchestrator || len(before.Agents) != len(after.Agents) || before.Agents[0].ReadUpTo != after.Agents[0].ReadUpTo {
		t.Fatal("binary dry run changed hub state")
	}
}

func TestTeamLaunchRefusesMissingInputsHandlerAndOtherLead(t *testing.T) {
	f := newTeamFixture(t, false)
	if err := cmdTeam(f.e, []string{"launch", "--item", f.item.ID}); err == nil || !strings.Contains(err.Error(), "--order") {
		t.Fatal(err)
	}
	if err := cmdTeam(f.e, f.args("--dry-run")); err == nil || !strings.Contains(err.Error(), "Set up database handler") {
		t.Fatal(err)
	}
	g := newTeamFixture(t, true)
	if err := cmdTeam(g.e, []string{"launch", "--item", "wi_0000000000000000", "--order", fmt.Sprint(g.order), "--dry-run"}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatal(err)
	}
	if err := cmdTeam(g.e, []string{"launch", "--item", g.item.ID, "--order", "999999", "--dry-run"}); err == nil || !strings.Contains(err.Error(), "work-order") {
		t.Fatal(err)
	}
	_, err := g.c.AddAgent(context.Background(), g.task.ID, api.AddAgentRequest{Name: "other-lead", Host: "fixture", Session: "other-lead", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	// A live unbound lead is an existing other-item orchestrator.
	if err := setFixtureOrchestrator(g.c, g.task.ID, "other-lead"); err != nil {
		t.Fatal(err)
	}
	if err := cmdTeam(g.e, g.args("--dry-run")); err == nil || !strings.Contains(err.Error(), "live orchestrator") {
		t.Fatal(err)
	}
}

func setFixtureOrchestrator(c *api.Client, task, name string) error {
	// Use the existing shared client adapter for fixture task updates.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var updated api.Task
	return teamplan.Run(ctx, map[string]any{"action": "set-orchestrator", "hub": c.Base, "task": task, "orchestrator": name}, &updated)
}

func TestTeamLaunchStartsFourMembersOnPrivateTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	f := newTeamFixture(t, true)
	t.Cleanup(func() { _, _ = startupTmux(context.Background(), "kill-server") })
	bin := t.TempDir()
	for _, runtime := range []string{"codex", "claude"} {
		if err := os.WriteFile(filepath.Join(bin, runtime), []byte("#!/bin/sh\nexec sleep 300\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := captureCLIOutput(t, func() error { return cmdTeam(f.e, f.args()) })
	if err != nil {
		t.Fatal(err)
	}
	detail, err := f.c.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Agents) != 5 || detail.Task.Orchestrator != "lead-"+f.item.ID[len(f.item.ID)-8:] {
		t.Fatalf("launch state: %+v %s", detail.Task, out)
	}
	for _, a := range detail.Agents {
		if a.Role == api.AgentRoleDatabaseHandler {
			if a.ID != f.handler.ID {
				t.Fatal("handler replaced")
			}
			continue
		}
		if a.WorkItem == nil || a.WorkItem.ItemID != f.item.ID || a.RunID == "" || !strings.Contains(out, "agent="+a.ID+" run="+a.RunID) {
			t.Fatalf("member mismatch: %+v output=%s", a, out)
		}
	}
	events, err := f.c.Events(context.Background(), f.task.ID, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	updated, added := int64(0), int64(0)
	for _, event := range events.Events {
		if event.Kind == "task_updated" {
			updated = event.Seq
		}
		if event.Kind == api.EventAgentAdded && event.AgentID != f.handler.ID && added == 0 {
			added = event.Seq
		}
	}
	if updated == 0 || added == 0 || updated >= added {
		t.Fatalf("orchestrator was not set before member admission: update=%d add=%d", updated, added)
	}
	// A retry must report the same exact identities without new admissions.
	out, err = captureCLIOutput(t, func() error { return cmdTeam(f.e, f.args()) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "(reconciled)") != 4 {
		t.Fatal(out)
	}
}

func TestTeamLaunchLostAdmissionReplyKeepsFrozenIdentityAndStops(t *testing.T) {
	f := newTeamFixture(t, true)
	f.dropFirstMemberReply.Store(true)
	_, err := captureCLIOutput(t, func() error { return cmdTeam(f.e, f.args()) })
	if err == nil || !strings.Contains(err.Error(), "frozen for retry") {
		t.Fatal(err)
	}
	path, err := teamJournalPath(f.e.hub, f.task.ID, f.item.ID, f.order)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := loadTeamJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Members[0].State != "uncertain" || journal.Members[0].Fields.AgentID == "" {
		t.Fatalf("lost frozen identity: %+v", journal.Members[0])
	}
	detail, err := f.c.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Agents) != 2 || detail.Agents[1].ID != journal.Members[0].Fields.AgentID {
		t.Fatal("admission did not retain the saved identity")
	}
	_, err = captureCLIOutput(t, func() error { return cmdTeam(f.e, f.args()) })
	if err == nil || !strings.Contains(err.Error(), "without an owned tmux session") {
		t.Fatal(err)
	}
	detail, err = f.c.GetTask(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Agents) != 2 {
		t.Fatal("retry admitted a duplicate")
	}
}
