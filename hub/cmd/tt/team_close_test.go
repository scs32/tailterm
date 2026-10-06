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
	"github.com/scs32/tailterm/hub/internal/testverification"
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
	for _, tc := range []struct {
		terminal string
		exited   bool
	}{{"done", false}, {"dismissed", false}, {"done", true}, {"dismissed", true}} {
		name := tc.terminal
		if tc.exited {
			name += "-exited-lead"
		}
		t.Run(name, func(t *testing.T) {
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
			handlerAgent, err := c.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler", Session: "db-handler", Role: api.AgentRoleDatabaseHandler, Runtime: "generic", Host: "fixture", Cwd: "/fixture"})
			if err != nil {
				t.Fatal(err)
			}
			confirmCLIFixtureOrder(t, c, task.ID, item.ID, order.Seq, handlerAgent)
			add := func(name string) api.Agent {
				a, err := c.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Host: "remote-fixture", Session: name, Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: order.Seq}, ContextBundle: teamCloseCLIContext(t, item, order)}})
				if err != nil {
					t.Fatal(err)
				}
				return a
			}
			lead := add("lead")
			worker := add("worker")
			request, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{To: worker.ID, AgentID: lead.ID, RunID: lead.RunID,
				Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: worker.Name, Subject: "Fixture request", Body: api.EnvelopeBody{Ask: "Check the fixture."}}})
			if err != nil {
				t.Fatal(err)
			}
			obligations, err := c.ListObligationsFrom(ctx, task.ID, worker.ID, request.Seq, request.Seq)
			if err != nil || len(obligations) != 1 {
				t.Fatalf("fixture obligation %+v: %v", obligations, err)
			}
			if _, err := c.CancelObligation(ctx, task.ID, obligations[0].ID, api.ObligationCancelRequest{RequestID: "fixture-cancel", Reason: "fixture complete"}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{To: worker.ID, AgentID: lead.ID, RunID: lead.RunID,
				Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, To: worker.Name, Subject: "Fixture notice", Body: api.EnvelopeBody{Text: "Fixture complete."}}}); err != nil {
				t.Fatal(err)
			}
			if tc.exited {
				exited := api.AgentExited
				if _, err := c.UpdateAgent(ctx, task.ID, lead.ID, api.UpdateAgentRequest{Status: &exited}); err != nil {
					t.Fatal(err)
				}
			}
			status := tc.terminal
			if status == "done" {
				if err := testverification.Prepare(c, task.ID, item); err != nil {
					t.Fatal(err)
				}
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
			if tc.exited {
				actor := env{hub: srv.URL, task: task.ID, agent: lead.ID, runID: lead.RunID}
				if err := cmdClose(actor, []string{"--team"}); err == nil || !strings.Contains(err.Error(), "only the owner") {
					t.Fatalf("exited actor accepted: %v", err)
				}
			}
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
			finalObligations, err := c.ListObligationsFrom(ctx, task.ID, worker.ID, request.Seq, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(finalObligations) < 3 {
				t.Fatalf("missing owner and agent notices after close: %+v", finalObligations)
			}
			for _, obligation := range finalObligations {
				if obligation.Needs == api.ObligationNeedsDelivery && (obligation.State != api.ObligationClosed || obligation.Outcome != api.OutcomeRecipientGone) {
					t.Fatalf("delivery obligation survived close: %+v", obligation)
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

// a7: tt close --team --reason closes the team of an open item through a real
// fixture hub. Without a reason the refusal is unchanged, and a reason outside
// the supported two is refused before anything is sent.
func TestTeamCloseReasonClosesOpenItemTeam(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	hub := server.New(st, func(*http.Request) (api.Caller, error) { return by, nil })
	var closes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/team-close") {
			closes.Add(1)
		}
		hub.ServeHTTP(w, r)
	}))
	defer srv.Close()
	ctx := context.Background()
	c, err := api.NewClient(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	task, err := c.CreateTask(ctx, api.CreateTaskRequest{Name: "Open close", Orchestrator: "lead"})
	if err != nil {
		t.Fatal(err)
	}
	item, err := c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Diagnosis fixture", RequestID: "fixture-item"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Bounded diagnosis order", RequestID: "fixture-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	handlerAgent, err := c.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler", Session: "db-handler", Role: api.AgentRoleDatabaseHandler, Runtime: "generic", Host: "fixture", Cwd: "/fixture"})
	if err != nil {
		t.Fatal(err)
	}
	confirmCLIFixtureOrder(t, c, task.ID, item.ID, order.Seq, handlerAgent)
	add := func(name string) api.Agent {
		a, err := c.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Host: "remote-fixture", Session: name, Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: order.Seq}, ContextBundle: teamCloseCLIContext(t, item, order)}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	lead, worker := add("lead"), add("worker")
	e := env{hub: srv.URL}
	unchanged := func(step string) {
		t.Helper()
		detail, err := c.GetTask(ctx, task.ID)
		if err != nil || detail.Task.Orchestrator != "lead" {
			t.Fatalf("%s changed the project: %+v %v", step, detail.Task, err)
		}
		for _, id := range []string{lead.ID, worker.ID} {
			if a, err := c.GetAgent(ctx, task.ID, id); err != nil || a.Status == api.AgentClosed {
				t.Fatalf("%s closed %s: %v", step, a.Name, err)
			}
		}
	}

	if err := cmdClose(e, []string{"--team", "--task", task.ID, "--reason", "bogus"}); err == nil || err.Error() != "usage: tt close --team --reason owner-hold|findings-only" {
		t.Fatalf("unknown reason: %v", err)
	}
	if closes.Load() != 0 {
		t.Fatalf("an unknown reason sent %d team close requests", closes.Load())
	}
	unchanged("an unknown reason")
	if err := cmdClose(e, []string{"--reason", "findings-only", lead.ID}); err == nil || !strings.Contains(err.Error(), "--reason") || closes.Load() != 0 {
		t.Fatalf("reason without --team: %v after %d requests", err, closes.Load())
	}
	unchanged("a reason without --team")

	// Without a reason the hub refuses as it always has.
	if err := cmdClose(e, []string{"--team", "--task", task.ID}); err == nil || !strings.Contains(err.Error(), "item is not terminal at the selected revision") {
		t.Fatalf("open item without a reason: %v", err)
	}
	if closes.Load() != 1 {
		t.Fatalf("team close requests after the refusal: %d", closes.Load())
	}
	unchanged("a close without a reason")

	out, err := captureCLIOutput(t, func() error {
		return cmdClose(e, []string{"--team", "--task", task.ID, "--reason", "findings-only"})
	})
	if err != nil {
		t.Fatalf("close with a reason: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Recorded team close for "+item.ID+" (findings-only); the item stays open.") || !strings.Contains(out, "tt team queue release") || !strings.Contains(out, "tt team queue requeue") {
		t.Fatalf("output %q", out)
	}
	detail, err := c.GetTask(ctx, task.ID)
	if err != nil || detail.Task.Status != api.TaskOpen || detail.Task.Orchestrator != "" {
		t.Fatalf("task %+v %v", detail.Task, err)
	}
	for _, id := range []string{lead.ID, worker.ID} {
		if a, err := c.GetAgent(ctx, task.ID, id); err != nil || a.Status != api.AgentClosed {
			t.Fatalf("member %+v %v", a, err)
		}
	}
	after, err := c.GetWorkItem(ctx, task.ID, item.ID)
	if err != nil || after.Status != "open" || after.Revision != item.Revision {
		t.Fatalf("item %+v %v", after, err)
	}
	receipt, err := c.GetTeamCloseReceipt(ctx, task.ID, "team-close-"+task.ID+"-"+lead.RunID)
	if err != nil || receipt.Reason != "findings-only" || receipt.LeadAgentID != lead.ID {
		t.Fatalf("receipt %+v %v", receipt, err)
	}
}
