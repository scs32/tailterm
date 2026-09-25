package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/store"
)

func teamCloseCLIContext(t *testing.T, item api.WorkItem, order api.Message) []byte {
	t.Helper()
	revision := api.WorkItemRevision{ItemID: item.ID, TaskID: item.TaskID, Kind: item.Kind, Title: item.Title, Description: item.Description,
		Status: item.Status, Priority: item.Priority, ItemSeq: item.Seq, Revision: item.Revision, CreatedBy: item.CreatedBy, UpdatedBy: item.UpdatedBy,
		CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, AttributionKind: "shared_workspace_claim", ChangeKind: "created", Provenance: "native"}
	data, err := json.Marshal(map[string]any{"version": 1, "itemTaskId": item.TaskID, "itemId": item.ID, "itemRevision": item.Revision,
		"workOrderMessage": api.MessageReference{TaskID: item.TaskID, Seq: order.Seq},
		"history": map[string]any{"revision": revision, "revisions": []api.WorkItemRevision{revision},
			"messages": []api.WorkItemMessageLink{{ItemRevision: item.Revision, RevisionCoverage: "verified", Relationship: "primary", Message: order}},
			"coverage": api.HistoryCoverage{Complete: true, ObservedCurrentRevision: item.Revision, LatestMaterialized: item.Revision, SnapshotCount: 1, ConversationLinks: "explicit_only"}}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOwnerTeamCloseUsesRealFixtureHubWithoutAgentEnvironment(t *testing.T) {
	for _, terminal := range []string{"done", "dismissed"} {
		t.Run(terminal, func(t *testing.T) {
			t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
			st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			by := api.Caller{Node: "fixture", User: "owner"}
			hub := server.New(st, func(*http.Request) (api.Caller, error) { return by, nil })
			var dropFirstClose atomic.Bool
			dropFirstClose.Store(true)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/team-close") && dropFirstClose.CompareAndSwap(true, false) {
					recorded := httptest.NewRecorder()
					hub.ServeHTTP(recorded, r)
					if recorded.Code != 200 {
						w.WriteHeader(recorded.Code)
						_, _ = w.Write(recorded.Body.Bytes())
						return
					}
					http.Error(w, "fixture lost response after committed close", http.StatusServiceUnavailable)
					return
				}
				hub.ServeHTTP(w, r)
			}))
			defer srv.Close()
			ctx := context.Background()
			c, err := api.NewClient(srv.URL, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			task, err := c.CreateTask(ctx, api.CreateTaskRequest{Name: "Owner close", Orchestrator: "lead"})
			if err != nil {
				t.Fatal(err)
			}
			item, err := c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Owner close fixture", RequestID: "fixture-item"})
			if err != nil {
				t.Fatal(err)
			}
			order, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Bounded fixture order", RequestID: "fixture-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
			if err != nil {
				t.Fatal(err)
			}
			add := func(name string) api.Agent {
				a, err := c.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Host: "remote-fixture", Session: name, Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: order.Seq}, ContextBundle: teamCloseCLIContext(t, item, order)}})
				if err != nil {
					t.Fatal(err)
				}
				return a
			}
			lead := add("lead")
			worker := add("worker")
			status := terminal
			if status == "done" {
				report, _, err := st.PutNarrativeReport(ctx, task.ID, item.ID, api.PutNarrativeReportRequest{RequestID: "close-report", ScopeRevision: item.ScopeRevision,
					Sections:   api.NarrativeReportSections{RequestedOutcome: "Close the item team.", DeliveredWork: "The synthetic delivery is complete.", Verification: "Isolated CLI and hub fixture.", Limitations: "Fixture only.", RemainingWork: "No remaining fixture work."},
					References: []api.NarrativeReference{{Kind: "work-item-revision", TaskID: task.ID, ItemID: item.ID, Revision: item.Revision, Label: "bounded scope"}}}, by)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := st.CreateWorkItemUpdate(ctx, task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, RequestID: "close-done", Status: &status, CompletionReport: &api.NarrativeReportPin{ReportID: report.ReportID, Version: report.Version, Digest: report.Digest, ScopeRevision: report.ScopeRevision}}, by); err != nil {
					t.Fatal(err)
				}
			} else if _, err := st.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Status: &status}, by); err != nil {
				t.Fatal(err)
			}
			e := env{hub: srv.URL}
			if err := cmdClose(e, []string{"--team"}); err == nil || !strings.Contains(err.Error(), "owner must supply --task") {
				t.Fatalf("missing selector %v", err)
			}
			if err := cmdClose(e, []string{"--team", "--task", "bad"}); err == nil {
				t.Fatal("malformed selector accepted")
			}
			if err := cmdClose(e, []string{"--team", "--task", task.ID}); err == nil || !strings.Contains(err.Error(), "503") {
				t.Fatalf("fixture did not lose committed response: %v", err)
			}
			if err := cmdClose(e, []string{"--team", "--task", task.ID}); err != nil {
				t.Fatalf("lost-response replay: %v", err)
			}
			detail, err := c.GetTask(ctx, task.ID)
			if err != nil || detail.Task.Status != api.TaskOpen || detail.Task.Orchestrator != "" {
				t.Fatalf("task %+v %v", detail.Task, err)
			}
			for _, id := range []string{lead.ID, worker.ID} {
				a, err := c.GetAgent(ctx, task.ID, id)
				if err != nil || a.Status != api.AgentClosed || a.CleanupDone {
					t.Fatalf("remote cleanup %+v %v", a, err)
				}
			}
		})
	}
}

func TestTeamCloseSnapshotRejectsAmbiguousAndWrongActor(t *testing.T) {
	task := api.Task{ID: api.NewID("tsk"), Orchestrator: "lead"}
	lead := api.Agent{ID: api.NewID("agt"), Name: "lead", RunID: api.NewID("run"), Status: api.AgentDone,
		WorkItem: &api.AgentWorkItemBinding{ItemTaskID: task.ID, ItemID: api.NewID("wi"), ItemRevision: 1}}
	if _, err := closeTeamSnapshot(task, []api.Agent{lead, lead}, "", ""); err == nil {
		t.Fatal("ambiguous lead selected")
	}
	if _, err := closeTeamSnapshot(task, []api.Agent{lead}, api.NewID("agt"), api.NewID("run")); err == nil {
		t.Fatal("wrong actor selected")
	}
	lead.WorkItem = nil
	if _, err := closeTeamSnapshot(task, []api.Agent{lead}, "", ""); err == nil {
		t.Fatal("unbound orchestrator selected")
	}
}

func TestTeamCloseCleansLocalWorkerBeforeLead(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TT_TMUX_SOCKET", "tt-team-close-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	ctx := context.Background()
	defer startupTmux(ctx, "kill-server")
	task := api.Task{ID: api.NewID("tsk"), Status: api.TaskOpen, PauseState: api.ProjectPauseActive, Orchestrator: "lead"}
	item := api.NewID("wi")
	member := func(name string) api.Agent {
		return api.Agent{ID: api.NewID("agt"), TaskID: task.ID, Name: name, RunID: api.NewID("run"), Host: spawn.Host(), Session: name, Status: api.AgentDone, WorkItem: &api.AgentWorkItemBinding{ItemTaskID: task.ID, ItemID: item, ItemRevision: 1}}
	}
	lead, worker := member("lead"), member("worker")
	agents := []api.Agent{lead, worker}
	var cleaned []string
	var hubURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/tasks/"+task.ID:
			json.NewEncoder(w).Encode(api.TaskDetail{Task: task, Agents: agents})
		case r.Method == "POST" && r.URL.Path == "/v1/tasks/"+task.ID+"/team-close":
			for i := range agents {
				agents[i].Status = api.AgentClosed
			}
			task.Orchestrator = ""
			json.NewEncoder(w).Encode(api.TeamCloseResult{TaskID: task.ID, ItemID: item, LeadAgentID: lead.ID,
				Members: []api.TeamCloseMember{{AgentID: worker.ID, RunID: worker.RunID, Host: worker.Host, Status: worker.Status}, {AgentID: lead.ID, RunID: lead.RunID, Host: lead.Host, Status: lead.Status}}})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cleanup"):
			for i := range agents {
				if strings.Contains(r.URL.Path, agents[i].ID) {
					cleaned = append(cleaned, agents[i].ID)
					agents[i].CleanupDone = true
					json.NewEncoder(w).Encode(agents[i])
					return
				}
			}
			http.Error(w, "unknown cleanup", 404)
		default:
			http.Error(w, "unexpected path", 404)
		}
	}))
	defer srv.Close()
	hubURL = srv.URL
	for _, a := range agents {
		if _, err := startupTmux(ctx, "new-session", "-d", "-s", a.Name, "-e", "TAILTERM_HUB="+hubURL, "-e", "TAILTERM_TASK="+task.ID, "-e", "TAILTERM_AGENT="+a.ID, "-e", "TAILTERM_RUN="+a.RunID, "sleep 300"); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmdClose(env{hub: hubURL, task: task.ID, agent: lead.ID, runID: lead.RunID}, []string{"--team"}); err != nil {
		t.Fatal(err)
	}
	if len(cleaned) != 2 || cleaned[0] != worker.ID || cleaned[1] != lead.ID {
		t.Fatalf("cleanup order %v", cleaned)
	}
	for _, a := range agents {
		if _, err := startupTmux(ctx, "has-session", "-t", "="+a.Name); err == nil {
			t.Fatalf("session %s survived", a.Name)
		}
	}
}
