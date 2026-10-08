package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
	"github.com/scs32/tailterm/hub/internal/testverification"
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
	// Read while fn runs: a pipe holds only one buffer (64 KiB on macOS), so a
	// command that prints more would otherwise block in write forever.
	type captured struct {
		data []byte
		err  error
	}
	done := make(chan captured, 1)
	go func() {
		defer r.Close()
		data, err := io.ReadAll(r)
		done <- captured{data, err}
	}()
	os.Stdout = w
	runErr := func() error {
		defer func() {
			os.Stdout = old
			_ = w.Close()
		}()
		return fn()
	}()
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	return string(out.data), runErr
}

func TestCaptureCLIOutputReturnsOutputLargerThanAPipeBuffer(t *testing.T) {
	var want bytes.Buffer
	for i := 0; want.Len() <= 1<<20; i++ {
		fmt.Fprintf(&want, "line %07d of output larger than one pipe buffer\n", i)
	}
	old := os.Stdout
	failure := fmt.Errorf("command failed after printing")
	got, err := captureCLIOutput(t, func() error {
		if _, err := os.Stdout.Write(want.Bytes()); err != nil {
			return err
		}
		return failure
	})
	if err != failure {
		t.Fatalf("error = %v, want the command's own error", err)
	}
	if os.Stdout != old {
		t.Fatal("os.Stdout was not restored")
	}
	if got != want.String() {
		t.Fatalf("captured %d bytes, want %d bytes returned intact", len(got), want.Len())
	}
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
	if err != nil || len(messages) != 1 || messages[0].From.Node != "system" || messages[0].From.User != "queue" || messages[0].To != lead.ID || messages[0].SystemNotice == nil || messages[0].SystemNotice.Kind == "" || !strings.Contains(messages[0].Text, "revision 2") || !strings.Contains(messages[0].Text, "Queued for deliberate review") {
		t.Fatalf("dispatch message: %+v %v", messages, err)
	}
	replay, err := c.DispatchWorkItem(context.Background(), task.ID, itemID, api.DispatchWorkItemRequest{Revision: 2, TargetTaskID: task.ID, AgentID: e.agent, RequestID: "cli-dispatch-1"})
	if err != nil || replay.Queue == nil || !replay.Queue.Replay || replay.Queue.Notification == nil || replay.Queue.Notification.CausalAuthor.Node != "cli-test" || replay.Queue.Notification.CausalAuthor.User != "owner" || replay.Queue.Notification.RecipientAgentID != lead.ID || replay.Queue.Notification.MessageSeq != messages[0].Seq || replay.Dispatch.MessageSeq != messages[0].Seq || replay.Queue.Notification.ID != messages[0].SystemNotice.ID {
		t.Fatalf("dispatch replay/provenance: %+v %v", replay, err)
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
	if err := testverification.Prepare(c, task.ID, item); err != nil {
		t.Fatal(err)
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
	for _, required := range []string{"durable Database handler", "--source-seq", "--request-id", "--body-file", "tt work-items evidence", "private --manifest", "first page/count", "Stay done and available"} {
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

// Token estimate and budget in the CLI (wi_899863352c81e3b0, plan r2).

// fixtureSQL runs statements against the fixture's isolated database.
func fixtureSQL(t *testing.T, f teamFixture, statement string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(statement, args...); err != nil {
		t.Fatal(statement, err)
	}
}

// fixtureShare records lifetime usage for an item as one share row.
func fixtureShare(t *testing.T, f teamFixture, item, request string, tokens, denominator int64, partial bool) {
	t.Helper()
	flag := 0
	if partial {
		flag = 1
	}
	fixtureSQL(t, f, `INSERT INTO usage_item_shares(task_id,agent_id,run_id,request_id,turn_revision,item_task_id,item_id,denominator,tokens,partial) VALUES(?,?,?,?,1,?,?,?,?,?)`,
		f.task.ID, f.handler.ID, f.handler.RunID, request, f.task.ID, item, denominator, tokens, flag)
}

func cliOK(t *testing.T, run func() error) string {
	t.Helper()
	out, err := captureCLIOutput(t, run)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return out
}

// a1, a3, a6: the handler saves an estimate by CLI; get shows it with an
// unchanged revision; refusals store nothing; the text forms name each state.
func TestWorkItemsCLIEstimateAndBudgetText(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	owner := f.e
	handler := f.e
	handler.agent, handler.agentName, handler.runID = f.handler.ID, f.handler.Name, f.handler.RunID
	worker, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "builder", Host: "fixture", Session: "fixture-builder", Runtime: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	builder := f.e
	builder.agent, builder.agentName, builder.runID = worker.ID, worker.Name, worker.RunID
	id := f.item.ID
	get := func() api.WorkItem {
		t.Helper()
		var item api.WorkItem
		if err := json.Unmarshal([]byte(cliOK(t, func() error { return cmdWorkItems(owner, []string{"get", "--json", id}) })), &item); err != nil {
			t.Fatal(err)
		}
		return item
	}
	text := func() string {
		t.Helper()
		return cliOK(t, func() error { return cmdWorkItems(owner, []string{"get", id}) })
	}
	revisions := func() string {
		t.Helper()
		return cliOK(t, func() error { return cmdWorkItems(owner, []string{"revisions", "--json", id}) })
	}
	before, history := get(), revisions()
	if before.Budget == nil || before.Budget.Estimate != nil {
		t.Fatalf("budget before an estimate: %+v", before.Budget)
	}
	if out := text(); !strings.Contains(out, "Budget: no estimate · lifetime actual not measured\n") || strings.Contains(out, "×") {
		t.Fatalf("get without estimate or usage:\n%s", out)
	}

	// Refusals, each leaving the item without an estimate.
	basis := "Small, 2 paths, median of 8 Small items"
	for name, c := range map[string]struct {
		e    env
		args []string
		want string
	}{
		"non-handler agent":    {builder, []string{"--revision", "1", "--request-id", "x1", "--estimate-tokens", "5", "--estimate-basis", basis}, "only the database handler sets an estimate"},
		"negative tokens":      {handler, []string{"--revision", "1", "--request-id", "x2", "--estimate-tokens", "-1", "--estimate-basis", basis}, "between 0 and"},
		"tokens above the cap": {handler, []string{"--revision", "1", "--request-id", "x3", "--estimate-tokens", "1000000000001", "--estimate-basis", basis}, "between 0 and"},
		"tokens without basis": {handler, []string{"--revision", "1", "--request-id", "x4", "--estimate-tokens", "5"}, "--estimate-basis is required"},
		"basis without tokens": {handler, []string{"--revision", "1", "--request-id", "x5", "--estimate-basis", basis}, "needs --estimate-tokens"},
		"clear with a basis":   {handler, []string{"--revision", "1", "--request-id", "x6", "--estimate-tokens", "0", "--estimate-basis", basis}, "takes no --estimate-basis"},
		"mixed with title":     {handler, []string{"--revision", "1", "--request-id", "x7", "--estimate-tokens", "5", "--estimate-basis", basis, "--title", "Other"}, "saved alone; drop --title"},
		"mixed with body":      {handler, []string{"--revision", "1", "--request-id", "x8", "--estimate-tokens", "5", "--estimate-basis", basis, "--body-file", "-"}, "saved alone; drop --body-file"},
		"mixed with status":    {handler, []string{"--revision", "1", "--request-id", "x9", "--estimate-tokens", "5", "--estimate-basis", basis, "--status", "blocked"}, "saved alone; drop --status"},
		"mixed with priority":  {handler, []string{"--revision", "1", "--request-id", "x10", "--estimate-tokens", "5", "--estimate-basis", basis, "--priority", "high"}, "saved alone; drop --priority"},
		"stale revision":       {handler, []string{"--revision", "7", "--request-id", "x11", "--estimate-tokens", "5", "--estimate-basis", basis}, "revision changed"},
		"missing request id":   {handler, []string{"--revision", "1", "--estimate-tokens", "5", "--estimate-basis", basis}, "--request-id is required"},
	} {
		out, err := captureCLIOutput(t, func() error { return cmdWorkItems(c.e, append(append([]string{"update"}, c.args...), id)) })
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v out %q, want %q", name, err, out, c.want)
		}
	}
	if got := get(); got.Budget.Estimate != nil || got.Revision != before.Revision {
		t.Fatalf("a refused estimate was stored: %+v", got)
	}

	// a1: the handler's save.
	saveA := []string{"update", "--revision", "1", "--request-id", "KA", "--estimate-tokens", "12000000", "--estimate-basis", basis, id}
	out := cliOK(t, func() error { return cmdWorkItems(handler, saveA) })
	if !strings.Contains(out, id+" revision 1 (unchanged) receipt wir_") || !strings.Contains(out, "estimate 12000000 tokens: "+basis) {
		t.Fatalf("estimate save output %q", out)
	}
	after := get()
	est := after.Budget.Estimate
	if est == nil || est.Tokens != 12000000 || est.Basis != basis || est.SetAt.IsZero() || est.SetBy.AgentID != f.handler.ID {
		t.Fatalf("saved estimate: %+v", est)
	}
	if after.Revision != before.Revision || after.ScopeRevision != before.ScopeRevision || !after.UpdatedAt.Equal(before.UpdatedAt) || after.UpdatedBy != before.UpdatedBy {
		t.Fatalf("the estimate changed the item: before %+v after %+v", before, after)
	}
	if got := revisions(); got != history {
		t.Fatalf("the estimate added a revision:\n%s\n%s", history, got)
	}
	if out = text(); !strings.Contains(out, "Budget: estimate 12.00M · lifetime actual not measured\n") || !strings.Contains(out, "Estimate basis: "+basis+" (set ") || !strings.Contains(out, " by "+f.handler.ID+")") || strings.Contains(out, "×") {
		t.Fatalf("get with an estimate and no usage:\n%s", out)
	}

	// a6: measured, then partial.
	fixtureShare(t, f, id, "turn-a", 15300000, 1, false)
	if out = text(); !strings.Contains(out, "Budget: estimate 12.00M · lifetime actual 15.30M · 1.28×\n") || strings.Contains(out, "at least") {
		t.Fatalf("get with measured usage:\n%s", out)
	}
	fixtureShare(t, f, id, "turn-b", 121000, 2, true)
	if out = text(); !strings.Contains(out, "Budget: estimate 12.00M · lifetime actual at least 15.36M (partial) · at least 1.28×\n") {
		t.Fatalf("get with partial usage:\n%s", out)
	}
	if got := get().Budget; got.ActualTokens != "30721000/2" && got.ActualTokens != "15360500" || got.ActualState != "partial" || got.Ratio == "" {
		t.Fatalf("partial budget JSON: %+v", got)
	}

	// a3: the owner saves B with no agent; A's delayed replay and receipt
	// read return A; zero clears.
	out = cliOK(t, func() error {
		return cmdWorkItems(owner, []string{"update", "--revision", "1", "--request-id", "KB", "--estimate-tokens", "30000000", "--estimate-basis", "Planned, 9 paths", id})
	})
	if !strings.Contains(out, "estimate 30000000 tokens: Planned, 9 paths") {
		t.Fatalf("owner save output %q", out)
	}
	replay := cliOK(t, func() error { return cmdWorkItems(handler, saveA) })
	receipt := cliOK(t, func() error { return cmdWorkItems(handler, []string{"receipt", "--request-id", "KA", id}) })
	if replay != receipt || !strings.Contains(replay, "estimate 12000000 tokens: "+basis) {
		t.Fatalf("delayed replay %q receipt %q", replay, receipt)
	}
	var saved api.WorkItemUpdateResult
	if err = json.Unmarshal([]byte(cliOK(t, func() error { return cmdWorkItems(handler, []string{"receipt", "--request-id", "KA", "--json", id}) })), &saved); err != nil || saved.Estimate == nil || saved.Estimate.Tokens != 12000000 || saved.Receipt.ResultRevision != 1 {
		t.Fatalf("receipt JSON: %+v %v", saved, err)
	}
	if got := get().Budget.Estimate; got == nil || got.Tokens != 30000000 || got.SetBy.AgentID != "" || got.SetBy.User != "owner" {
		t.Fatalf("item after the owner's save: %+v", got)
	}
	if out = text(); !strings.Contains(out, "Estimate basis: Planned, 9 paths (set ") || !strings.Contains(out, " by owner)") {
		t.Fatalf("get after the owner's save:\n%s", out)
	}
	out = cliOK(t, func() error {
		return cmdWorkItems(handler, []string{"update", "--revision", "1", "--request-id", "KC", "--estimate-tokens", "0", id})
	})
	if !strings.Contains(out, "estimate cleared") {
		t.Fatalf("clear output %q", out)
	}
	if out = text(); !strings.Contains(out, "Budget: no estimate · lifetime actual at least 15.36M (partial)\n") || strings.Contains(out, "×") || strings.Contains(out, "Estimate basis") {
		t.Fatalf("get after a clear:\n%s", out)
	}
	if got := get(); got.Revision != 1 || got.Budget.Estimate != nil || revisions() != history {
		t.Fatalf("item after the clear: %+v", got)
	}
}

// a7: the queue list carries the budget on every form, as JSON and text.
func TestTeamQueueCLIListCarriesBudget(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	cliOK(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--cwd", t.TempDir()})
	})
	cliOK(t, func() error {
		return cmdWorkItems(f.e, []string{"update", "--revision", "1", "--request-id", "estimate", "--estimate-tokens", "20000000", "--estimate-basis", "Planned, 3 paths", f.item.ID})
	})
	fixtureShare(t, f, f.item.ID, "turn-a", 25000000, 1, true)
	const history = api.DefaultTeamQueueHistoryLimit + 5
	finished := insertFinishedQueueRows(t, f, 10, history)
	fixtureShare(t, f, finished[0].ItemID, "turn-b", 4200000, 1, false)
	list := func(args ...string) api.TeamQueueList {
		t.Helper()
		var out api.TeamQueueList
		if err := json.Unmarshal([]byte(cliOK(t, func() error { return cmdTeamQueue(f.e, append(append([]string{"list"}, args...), "--json")) })), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	check := func(form string, entries []api.TeamQueueEntry, want int) {
		t.Helper()
		if len(entries) != want {
			t.Fatalf("%s: %d entries, want %d", form, len(entries), want)
		}
		for _, q := range entries {
			b := q.Budget
			if b == nil {
				t.Fatalf("%s: entry %s has no budget", form, q.ID)
			}
			switch q.ItemID {
			case f.item.ID:
				if b.Estimate == nil || b.Estimate.Tokens != 20000000 || b.ActualTokens != "25000000" || b.ActualState != "partial" || b.Ratio != "5/4" {
					t.Fatalf("%s: active budget %+v", form, b)
				}
			case finished[0].ItemID:
				if b.Estimate != nil || b.ActualTokens != "4200000" || b.ActualState != "measured" || b.Ratio != "" {
					t.Fatalf("%s: measured history budget %+v", form, b)
				}
			default:
				if b.Estimate != nil || b.ActualState != "not measured" || b.ActualTokens != "0" {
					t.Fatalf("%s: unused history budget %+v", form, b)
				}
			}
		}
	}
	first := list()
	check("default", first.Entries, 1+api.DefaultTeamQueueHistoryLimit)
	if first.History == nil || first.History.NextAfter == 0 {
		t.Fatalf("default history page: %+v", first.History)
	}
	// The oldest entry, with measured usage, is only on the second page.
	second := list("--after", fmt.Sprint(first.History.NextAfter))
	check("second history page", second.Entries, 1+5)
	found := false
	for _, q := range second.Entries {
		found = found || q.ItemID == finished[0].ItemID
	}
	if !found {
		t.Fatal("the second history page does not hold the oldest entry")
	}
	check("active", list("--active").Entries, 1)
	check("item attempt", list("--item", f.item.ID).Entries, 1)
	check("history item attempt", list("--item", finished[0].ItemID).Entries, 1)
	one, err := f.c.GetTeamQueueEntry(ctx, f.task.ID, first.Entries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	check("entry get", []api.TeamQueueEntry{one}, 1)
	hosted, err := f.c.TeamQueueByHost(ctx, first.Entries[0].Host)
	if err != nil {
		t.Fatal(err)
	}
	check("by host", hosted.Entries, 1)

	text := cliOK(t, func() error {
		return cmdTeamQueue(f.e, []string{"list", "--after", fmt.Sprint(first.History.NextAfter)})
	})
	for _, want := range []string{
		"  budget: estimate 20.00M · lifetime actual at least 25.00M (partial) · at least 1.25×\n",
		"  budget: no estimate · lifetime actual 4.20M\n",
		"  budget: no estimate · lifetime actual not measured\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("queue list text lacks %q:\n%s", want, text)
		}
	}
	if strings.Count(text, "  budget: ") != 1+5 {
		t.Fatalf("queue list text budget lines:\n%s", text)
	}
}

// a9: the calibration read lists every done item past both page limits, with
// its real lane even when its only entry is on a later history page.
func TestUsageCalibrationListsEveryDoneItem(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	by := api.Caller{Node: "team-fixture", User: "owner"}
	const done = api.MaxLimit + 7
	now := time.Now().UTC().Format(time.RFC3339Nano)
	planReview := `{"members":[{"fields":{"name":"lead","role":"lead"}},{"fields":{"name":"plan-reviewer","role":"Plan review"}}]}`
	planOnly := `{"members":[{"fields":{"name":"lead","role":"lead"}}]}`
	items := make([]api.WorkItem, 0, done)
	for i := 0; i < done; i++ {
		item, err := f.st.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: fmt.Sprintf("done fixture %03d", i), RequestID: api.NewID("req")}, by)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	fixtureSQL(t, f, `UPDATE work_items SET status='done' WHERE task_id=? AND id<>?`, f.task.ID, f.item.ID)
	entry := func(item api.WorkItem, position, attempt int, template, launch, ownership string) {
		t.Helper()
		fixtureSQL(t, f, `INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,attempt,state,revision,host,cwd,launch_json,ownership_json,created_at,updated_at) VALUES(?,?,?,?,1,?,?,?,'finished',3,'fixture','/tmp',?,?,?,?)`,
			api.NewID("tqe"), f.task.ID, item.ID, item.Revision, template, position, attempt, launch, ownership, now, now)
	}
	// items[0]: its only entry has the lowest position, so it is on the last
	// history page. items[1] was never queued. items[2] ran twice.
	entry(items[0], 1, 1, "small", planOnly, `["hub/cmd/tt/usage.go","docs/usage-accounting.md"]`)
	entry(items[2], 2, 1, "planned", planReview, `["a","b","c","d"]`)
	entry(items[2], 3, 2, "small", planOnly, `["a"]`)
	for i := 3; i < done; i++ {
		entry(items[i], 10+i, 1, "planned", planReview, `["hub/internal/store"]`)
	}
	calibrationTokens, calibrationBasis := int64(10000000), "Small, 2 paths"
	if _, err := f.c.CreateWorkItemUpdate(ctx, f.task.ID, items[0].ID, api.CreateWorkItemUpdate{ExpectedRevision: 1, RequestID: "estimate", EstimateTokens: &calibrationTokens, EstimateBasis: &calibrationBasis}); err != nil {
		t.Fatal(err)
	}
	fixtureShare(t, f, items[0].ID, "turn-a", 12500000, 1, false)
	fixtureShare(t, f, items[2].ID, "turn-b", 900, 2, true)

	e, _, queries := recordTeamQueueListings(t, f)
	var report usageCalibrationReport
	if err := json.Unmarshal([]byte(cliOK(t, func() error { return cmdUsage(e, []string{"--calibration", "--json"}) })), &report); err != nil {
		t.Fatal(err)
	}
	if got := queries(); len(got) != 2 || !strings.Contains(got[0], "limit=200") || !strings.Contains(got[1], "after=") {
		t.Fatalf("calibration did not read the history to its last page: %v", got)
	}
	if report.ProjectID != f.task.ID || len(report.Items) != done {
		t.Fatalf("calibration lists %d items, want %d", len(report.Items), done)
	}
	seen := map[string]usageCalibrationRow{}
	for i, row := range report.Items {
		if _, dup := seen[row.ItemID]; dup || row.ItemID == f.item.ID {
			t.Fatalf("calibration row %d repeats or is not done: %+v", i, row)
		}
		if i > 0 && row.Seq <= report.Items[i-1].Seq {
			t.Fatalf("calibration rows are not by item sequence at %d", i)
		}
		seen[row.ItemID] = row
	}
	for _, item := range items {
		if _, ok := seen[item.ID]; !ok {
			t.Fatalf("done item %s (%s) is missing", item.ID, item.Title)
		}
	}
	if row := seen[items[0].ID]; row.Lane != "small" || row.OwnedPaths != 2 || row.Template != "plan-only" || row.ActualTokens != "12500000" || row.ActualState != "measured" || row.EstimateTokens != 10000000 || row.EstimateBasis != "Small, 2 paths" || row.Ratio != "5/4" {
		t.Fatalf("item on the last history page: %+v", row)
	}
	if row := seen[items[1].ID]; row.Lane != "none" || row.OwnedPaths != 0 || row.Template != "" || row.ActualState != "not measured" || row.EstimateTokens != 0 || row.Ratio != "" {
		t.Fatalf("item never queued: %+v", row)
	}
	if row := seen[items[2].ID]; row.Lane != "small" || row.OwnedPaths != 1 || row.Template != "plan-only" || row.ActualTokens != "450" || row.ActualState != "partial" {
		t.Fatalf("item with two attempts takes its latest: %+v", row)
	}
	if row := seen[items[done-1].ID]; row.Lane != "planned" || row.OwnedPaths != 1 || row.Template != "plan-review" {
		t.Fatalf("planned item: %+v", row)
	}

	text := cliOK(t, func() error { return cmdUsage(f.e, []string{"--calibration"}) })
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) != done {
		t.Fatalf("calibration text has %d lines, want %d", len(lines), done)
	}
	for _, want := range []string{
		items[0].ID + " bug lane=small paths=2 template=plan-only · estimate 10.00M · lifetime actual 12.50M · 1.25× · done fixture 000",
		items[1].ID + " bug lane=none paths=0 template=none · no estimate · lifetime actual not measured · done fixture 001",
		items[2].ID + " bug lane=small paths=1 template=plan-only · no estimate · lifetime actual at least 450 (partial) · done fixture 002",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("calibration text lacks %q:\n%s", want, strings.Join(lines[:4], "\n"))
		}
	}
	if err := cmdUsage(f.e, []string{"--calibration", "--item", items[0].ID}); err == nil {
		t.Fatal("calibration accepted an item filter")
	}

	// tt usage prints the budget under each item and leaves overhead alone.
	usage := cliOK(t, func() error { return cmdUsage(f.e, []string{"--item", items[0].ID}) })
	if !strings.Contains(usage, "  budget: estimate 10.00M · lifetime actual 12.50M · 1.25×\n") || strings.Count(usage, "  budget: ") != 1 {
		t.Fatalf("usage text:\n%s", usage)
	}
}

// An older hub sends no budget: every text form prints nothing for it.
func TestTokenBudgetTextForms(t *testing.T) {
	if got := formatTokenBudget(nil); got != "" || workItemBudgetLines(nil) != nil {
		t.Fatalf("nil budget prints %q", got)
	}
	for want, b := range map[string]*api.TokenBudget{
		"no estimate · lifetime actual not measured":                                  {ActualTokens: "0", ActualState: "not measured"},
		"estimate 500 · lifetime actual not measured":                                 {ActualTokens: "0", ActualState: "not measured", Estimate: &api.WorkItemEstimate{Tokens: 500}},
		"estimate 12.00M · lifetime actual 15.30M · 1.28×":                            {ActualTokens: "15300000", ActualState: "measured", Ratio: "51/40", Estimate: &api.WorkItemEstimate{Tokens: 12000000}},
		"estimate 12.00M · lifetime actual at least 7.65M (partial) · at least 0.64×": {ActualTokens: "15300000/2", ActualState: "partial", Ratio: "51/80", Estimate: &api.WorkItemEstimate{Tokens: 12000000}},
		"no estimate · lifetime actual at least 1.2k (partial)":                       {ActualTokens: "1200", ActualState: "partial"},
	} {
		if got := formatTokenBudget(b); got != want {
			t.Fatalf("budget text %q, want %q", got, want)
		}
	}
}

// --scope-change reaches the hub as the request's marker and only rides a
// title or description change.
func TestWorkItemUpdateScopeChangeFlag(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "cli-test", User: "owner"}
	ctx := context.Background()
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "CLI project"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Scope marker", RequestID: "item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	hub := server.New(st, func(*http.Request) (api.Caller, error) { return by, nil })
	var updates []api.CreateWorkItemUpdate
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/updates") {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			var req api.CreateWorkItemUpdate
			if err = json.Unmarshal(raw, &req); err != nil {
				t.Error(err)
			}
			if req.ScopeChange != strings.Contains(string(raw), `"scopeChange":true`) {
				t.Errorf("scope change marker on the wire: %s", raw)
			}
			updates = append(updates, req)
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		hub.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e := env{hub: srv.URL, task: task.ID}
	bodyPath := filepath.Join(t.TempDir(), "description.txt")
	if err = os.WriteFile(bodyPath, []byte("Amended scope."), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"update", "--revision", "1", "--request-id", "marker-status", "--status", "in_progress", "--scope-change", item.ID},
		{"update", "--revision", "1", "--request-id", "marker-estimate", "--estimate-tokens", "100", "--estimate-basis", "Small, 2 paths", "--scope-change", item.ID},
	} {
		if _, err = captureCLIOutput(t, func() error { return cmdWorkItems(e, args) }); err == nil || !strings.Contains(err.Error(), "scope-change") {
			t.Fatalf("%v: err=%v, want a --scope-change usage error", args, err)
		}
	}
	if len(updates) != 0 {
		t.Fatalf("a refused flag reached the hub: %+v", updates)
	}
	out, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"update", "--revision", "1", "--request-id", "marked", "--body-file", bodyPath, "--scope-change", item.ID})
	})
	if err != nil || !strings.Contains(out, "revision 2") || len(updates) != 1 || !updates[0].ScopeChange || updates[0].Description == nil {
		t.Fatalf("marked update = %q %v %+v", out, err, updates)
	}
	out, err = captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"update", "--revision", "2", "--request-id", "unmarked", "--title", "Scope marker renamed", item.ID})
	})
	if err != nil || !strings.Contains(out, "revision 3") || len(updates) != 2 || updates[1].ScopeChange {
		t.Fatalf("unmarked update = %q %v %+v", out, err, updates)
	}
	got, err := st.GetWorkItem(ctx, task.ID, item.ID)
	if err != nil || got.Description != "Amended scope." || got.ScopeRevision != item.ScopeRevision+2 {
		t.Fatalf("saved item: %+v %v", got, err)
	}
}

// tt work-items note stores a narrative artifact on a bug and leaves the
// item's revision and scope revision alone.
func TestWorkItemNoteRecordsArtifact(t *testing.T) {
	e, c, task, _ := cliWorkItemFixture(t)
	ctx := context.Background()
	out, err := captureCLIOutput(t, func() error {
		return cmdWorkItems(e, []string{"create", "--kind", "bug", "--title", "Pane stays stale", "--request-id", "note-item"})
	})
	if err != nil {
		t.Fatal(err)
	}
	itemID := strings.TrimSpace(out)
	before, err := c.GetWorkItem(ctx, task.ID, itemID)
	if err != nil {
		t.Fatal(err)
	}
	bodyPath := filepath.Join(t.TempDir(), "note.txt")
	if err = os.WriteFile(bodyPath, []byte("Seen again at 17:12Z.\nLog: relay.log line 88."), 0600); err != nil {
		t.Fatal(err)
	}
	note := []string{"note", "--request-id", "recurrence-1", "--title", "Recurrence on the Mini", "--body-file", bodyPath, "--kind", "recurrence", itemID}
	out, err = captureCLIOutput(t, func() error { return cmdWorkItems(e, note) })
	if err != nil || !strings.Contains(out, itemID+" artifact nart_") || !strings.Contains(out, "version 1 narrative seq 1") {
		t.Fatalf("note = %q %v", out, err)
	}
	replay, err := captureCLIOutput(t, func() error { return cmdWorkItems(e, note) })
	if err != nil || replay != out {
		t.Fatalf("note replay = %q %v, want %q", replay, err, out)
	}
	artifacts, err := c.ListNarrativeArtifacts(ctx, task.ID, itemID, "", 10)
	if err != nil || len(artifacts.Artifacts) != 1 {
		t.Fatalf("artifacts: %+v %v", artifacts, err)
	}
	a := artifacts.Artifacts[0].Latest
	if a.Namespace != "note" || a.SourceID != "recurrence-1" || a.SourceVersion != "1" || a.Kind != "recurrence" || a.Title != "Recurrence on the Mini" || a.Provenance != "agent-note" || a.CaptureState != "stored-content" || a.Availability != "available" || a.IngestedBy.AgentID != e.agent {
		t.Fatalf("stored note: %+v", a)
	}
	after, err := c.GetWorkItem(ctx, task.ID, itemID)
	if err != nil || after.Revision != before.Revision || after.ScopeRevision != before.ScopeRevision || after.Description != before.Description {
		t.Fatalf("note changed the item: %+v -> %+v %v", before, after, err)
	}
	for _, bad := range [][]string{
		{"note", "--request-id", "k", "--title", "T", itemID},
		{"note", "--request-id", "k", "--body-file", bodyPath, itemID},
		{"note", "--title", "T", "--body-file", bodyPath, itemID},
		{"note", "--request-id", "k", "--title", "T", "--body-file", bodyPath, "--kind", "essay", itemID},
	} {
		if _, err = captureCLIOutput(t, func() error { return cmdWorkItems(e, bad) }); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
}
