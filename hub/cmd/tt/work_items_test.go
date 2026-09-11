package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

func cliWorkItemFixture(t *testing.T) (env, *api.Client, api.Task, api.Agent) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "cli-test", User: "owner"}
	task, err := st.CreateTask(context.Background(), api.CreateTaskRequest{Name: "CLI project", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, err := st.AddAgent(context.Background(), task.ID, api.AddAgentRequest{Name: "lead", Host: "host", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := st.AddAgent(context.Background(), task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database", Host: "host", Session: "database", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }))
	t.Cleanup(srv.Close)
	c, err := api.NewClient(srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return env{hub: srv.URL, task: task.ID, agent: handler.ID, agentName: handler.Name}, c, task, lead
}

func captureCLIOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	data, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(data), runErr
}

func TestWorkItemsCLIUsesBodyFilesAndDurableReceipts(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
	bodyPath := filepath.Join(t.TempDir(), "description.txt")
	if err := os.WriteFile(bodyPath, []byte("Exact reproduction steps.\nNo shell interpolation."), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"create", "--kind", "bug", "--title", "CLI retry", "--body-file", bodyPath, "--priority", "urgent", "--request-id", "cli-create-1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	itemID := strings.TrimSpace(out)
	if !api.ValidID(itemID, "wi") {
		t.Fatalf("create output = %q", out)
	}
	item, err := c.GetWorkItem(context.Background(), task.ID, itemID)
	if err != nil || item.Description != "Exact reproduction steps.\nNo shell interpolation." || item.Priority != "urgent" || item.CreatedBy.AgentID != e.agent {
		t.Fatalf("created item: %+v %v", item, err)
	}
	replayOut, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"create", "--kind", "bug", "--title", "CLI retry", "--body-file", bodyPath, "--priority", "urgent", "--request-id", "cli-create-1"})
	})
	if err != nil || strings.TrimSpace(replayOut) != itemID {
		t.Fatalf("create replay = %q %v", replayOut, err)
	}

	updatedOut, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"update", "--revision", "1", "--request-id", "cli-update-1", "--status", "in_progress", itemID})
	})
	if err != nil || !strings.Contains(updatedOut, "revision 2") {
		t.Fatalf("update = %q %v", updatedOut, err)
	}
	receiptOut, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"receipt", "--request-id", "cli-update-1", itemID})
	})
	if err != nil || !strings.Contains(receiptOut, "revision 2") || !strings.Contains(receiptOut, "receipt wir_") {
		t.Fatalf("receipt = %q %v", receiptOut, err)
	}
	listOut, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"list", "--kind", "bug", "--status", "in_progress"})
	})
	if err != nil || !strings.Contains(listOut, itemID) || !strings.Contains(listOut, "CLI retry") {
		t.Fatalf("list = %q %v", listOut, err)
	}
	getOut, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"get", itemID})
	})
	if err != nil || !strings.Contains(getOut, "Exact reproduction steps") {
		t.Fatalf("get = %q %v", getOut, err)
	}
	dispatchOut, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"dispatch", "--revision", "2", "--request-id", "cli-dispatch-1", itemID})
	})
	if err != nil || !strings.Contains(dispatchOut, "message #") {
		t.Fatalf("dispatch = %q %v", dispatchOut, err)
	}
	messages, err := c.ListMessages(context.Background(), task.ID, 0, lead.ID, 10)
	if err != nil || len(messages) != 1 || messages[0].From.AgentID != e.agent || messages[0].To != lead.ID || !strings.Contains(messages[0].Text, "revision 2") {
		t.Fatalf("dispatch message: %+v %v", messages, err)
	}
}

func TestWorkItemsCLIRejectsUnsafeScopeAndOversizedBody(t *testing.T) {
	e, _, _, _ := cliWorkItemFixture(t)
	if err := cmdWorkItems(e, []string{"list", "--project", ""}); err == nil || !strings.Contains(err.Error(), "own project") {
		t.Fatalf("cross-scope list = %v", err)
	}
	large := filepath.Join(t.TempDir(), "large.txt")
	if err := os.WriteFile(large, bytes.Repeat([]byte("x"), api.MaxTextLen+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := bodyFile(large); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized body = %v", err)
	}
}

func TestWorkItemsCLINarrativeReportQueryAndDonePin(t *testing.T) {
	e, c, task, _ := cliWorkItemFixture(t)
	item, err := c.CreateWorkItem(context.Background(), task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "CLI narrative", AgentID: e.agent, RequestID: "cli-narrative-item"})
	if err != nil {
		t.Fatal(err)
	}
	req := api.PutNarrativeReportRequest{RequestID: "cli-report-1", ScopeRevision: item.ScopeRevision, Sections: api.NarrativeReportSections{RequestedOutcome: "Store a full report.", DeliveredWork: strings.Repeat("durable CLI content ", 500), Verification: "Isolated CLI and HTTP readback.", Limitations: "No release claim.", RemainingWork: "Release separately."}, References: []api.NarrativeReference{{Kind: "work-item-revision", TaskID: task.ID, ItemID: item.ID, Revision: 1, Label: "scope"}}}
	reportFile := filepath.Join(t.TempDir(), "report.json")
	raw, _ := json.Marshal(req)
	if err = os.WriteFile(reportFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"narrative", "report-put", "--file", reportFile, item.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	var report api.NarrativeReportVersion
	if err = json.Unmarshal([]byte(out), &report); err != nil || report.Digest == "" || len(report.Sections.DeliveredWork) <= 8192 {
		t.Fatalf("report output=%d %v", len(out), err)
	}
	overview, err := captureCLIOutput(t, func() error { return cmdWorkItems(e, []string{"narrative", "overview", item.ID}) })
	if err != nil || !strings.Contains(overview, report.ReportID) || !strings.Contains(overview, "not-ingested") {
		t.Fatalf("overview=%q %v", overview, err)
	}
	doneOut, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"update", "--revision", "1", "--request-id", "cli-done-1", "--status", "done", "--report-id", report.ReportID, "--report-version", "1", "--report-digest", report.Digest, "--report-scope-revision", "1", item.ID})
	})
	if err != nil || !strings.Contains(doneOut, "revision 2") {
		t.Fatalf("done=%q %v", doneOut, err)
	}
	receipt, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"narrative", "receipt", "--operation", "report", "--request-id", "cli-report-1", item.ID})
	})
	if err != nil || !strings.Contains(receipt, "cli-report-1") {
		t.Fatalf("receipt=%q %v", receipt, err)
	}
}

func TestAgentBriefingNamesDatabaseHandlerAndItsIntakeContract(t *testing.T) {
	task := api.Task{ID: "tsk_0000000000000001", Name: "Project", Goal: "Ship", Status: api.TaskOpen, Orchestrator: "lead"}
	handler := api.Agent{ID: "agt_0000000000000002", Name: "database", Role: api.AgentRoleDatabaseHandler, Status: api.AgentDone}
	worker := agentTaskBriefing(task, "worker", "", "", []api.Agent{handler})
	if !strings.Contains(worker, "Database handler is database") || !strings.Contains(worker, "tt work-items") {
		t.Fatal(worker)
	}
	handlerBrief := agentTaskBriefing(task, handler.Name, handler.Role, "", []api.Agent{handler})
	for _, required := range []string{"durable Database handler", "--source-seq", "--request-id", "--body-file", "Stay done and available"} {
		if !strings.Contains(handlerBrief, required) {
			t.Fatalf("handler briefing missing %q", required)
		}
	}
	leadBrief := agentTaskBriefing(task, "lead", "", "", []api.Agent{handler})
	if !strings.Contains(leadBrief, "database_handler is a continuing project role") || !strings.Contains(leadBrief, "active database_handler is the exception") {
		t.Fatal(leadBrief)
	}
}

// The audit workflow must remain intact for every role and handler lifecycle.
// In particular, losing a handler must never turn into direct database access.
func TestAgentWorkAuditAcrossRolesAndHandlerAvailability(t *testing.T) {
	task := api.Task{ID: "tsk_0000000000000001", Name: "Project", Goal: "Ship", Status: api.TaskOpen, Orchestrator: "lead", AllowAgentSpawn: true, MaxNewAgents: 2, Swarm: true}
	handler := api.Agent{ID: "agt_0000000000000002", Name: "records-custom", Role: api.AgentRoleDatabaseHandler, Status: api.AgentDone}
	for _, name := range []string{"lead", "worker"} {
		for _, state := range []string{api.AgentDone, api.AgentRunning, api.AgentRetired, api.AgentExited, api.AgentClosed, "missing"} {
			t.Run(name+"/"+state, func(t *testing.T) {
				h := handler
				h.Status = state
				agents := []api.Agent{h}
				if state == "missing" {
					agents = nil
				}
				got := agentTaskBriefing(task, name, "", "", agents)
				for _, required := range []string{"durable bug or feature", "recorded bounded work order", "Intake and board/inbox/roster coordination", "work-item ID and work-order message sequence", "list/get/create/update/dispatch", "Do not use tt work-items, direct API calls, or database files yourself", "even if the handler is unavailable", "human UI access remains available", "This task allows at most 2 active extras per bug or feature", "SWARM ENABLED"} {
					if !strings.Contains(got, required) {
						t.Fatalf("missing %q in %s", required, got)
					}
				}
				if state == api.AgentClosed || state == api.AgentExited || state == "missing" {
					if !strings.Contains(got, "No active Database handler") || !strings.Contains(got, "no direct database-access fallback") {
						t.Fatal(got)
					}
				} else {
					// LastSeenAt is absent: offline/unknown liveness must still name the handler,
					// while directing the caller to its current roster rather than assuming it works.
					for _, required := range []string{"Database handler is records-custom", "retired/offline/unavailable", "Respect explicit owner retirement"} {
						if !strings.Contains(got, required) {
							t.Fatalf("missing %q", required)
						}
					}
				}
				for _, forbidden := range []string{"use tt work-items directly", "Use tt work-items directly", "Then do independent inspection within your role"} {
					if strings.Contains(got, forbidden) {
						t.Fatalf("unsafe fallback: %q", forbidden)
					}
				}
				if name == "lead" {
					for _, required := range []string{"delivery ledger", "readUpTo", "active database_handler is the exception"} {
						if !strings.Contains(got, required) {
							t.Fatalf("lost lead safeguard %q", required)
						}
					}
				}
			})
		}
	}
	got := agentTaskBriefing(task, handler.Name, handler.Role, "", []api.Agent{handler})
	for _, required := range []string{"sole agent owner", "list/get/create/update/dispatch", "--source-seq", "--request-id", "--body-file", "--revision", "Read back the committed record", "assignment/result message links", "preserve owner text", "scope changes", "required dependencies are resolved", "Stay done and available", "respect explicit owner retirement"} {
		if !strings.Contains(got, required) {
			t.Fatalf("handler missing %q", required)
		}
	}
	if strings.Contains(got, "Do not use tt work-items") {
		t.Fatal("handler must retain its work-item tools")
	}
}
