package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

func confirmCLIFixtureOrder(t *testing.T, c *api.Client, task, item string, order int64, handler api.Agent) {
	t.Helper()
	current, err := c.GetWorkItem(context.Background(), task, item)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ConfirmWorkOrderScope(context.Background(), task, item, api.ConfirmWorkOrderScopeRequest{
		RequestID:        "fixture-scope-" + item,
		AgentID:          handler.ID,
		RunID:            handler.RunID,
		ExpectedRevision: current.Revision,
		ScopeRevision:    current.ScopeRevision,
		OrderMessageSeq:  order,
		Complete:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkOrderScopeCLIHandlerIntakeAndReceipt(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	item, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "CLI intake", Description: "owner acceptance and files", RequestID: "cli-scope-item"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "CLI bounded order", RequestID: "cli-scope-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	handler := f.e
	handler.agent, handler.runID = f.handler.ID, f.handler.RunID
	confirm := []string{"confirm", "--revision", fmt.Sprint(item.Revision), "--scope-revision", fmt.Sprint(item.ScopeRevision), "--order", fmt.Sprint(order.Seq), "--request-id", "cli-intake", "--complete", item.ID}
	out, err := captureCLIOutput(t, func() error { return cmdWorkOrderScope(handler, confirm) })
	if err != nil || !strings.Contains(out, item.ID) {
		t.Fatalf("CLI confirm %q %v", out, err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdWorkOrderScope(handler, confirm) }); err != nil {
		t.Fatalf("CLI lost-response retry %v", err)
	}
	q, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "cli-scope-queue", Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "fixture", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	source, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "sequencing note", RequestID: "cli-scope-note", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	save := []string{"save", "--revision", fmt.Sprint(item.Revision), "--order", fmt.Sprint(order.Seq), "--source", fmt.Sprint(source.Seq), "--kind", "sequencing_note", "--queue-entry", q.ID, "--request-id", "cli-note", item.ID}
	out, err = captureCLIOutput(t, func() error { return cmdWorkOrderScope(handler, save) })
	if err != nil || !strings.Contains(out, q.ID) {
		t.Fatalf("CLI save %q %v", out, err)
	}
	out, err = captureCLIOutput(t, func() error {
		return cmdWorkOrderScope(handler, []string{"receipt", "--request-id", "cli-note", item.ID})
	})
	if err != nil || !strings.Contains(out, "cli-note") {
		t.Fatalf("CLI receipt %q %v", out, err)
	}
}

// a1 (c1): the handler records ownership at scope confirmation; get returns
// it, a retry with other owns is refused, and a queue add without --owns in a
// parallel project saves exactly those paths.
func TestWorkOrderScopeCLIConfirmOwnershipFeedsQueueAdd(t *testing.T) {
	f := newTeamFixture(t, true)
	ctx := context.Background()
	repo, _ := queueGitRepo(t)
	parallelCLIProject(t, f, "none")
	item, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "CLI owned intake", RequestID: "cli-owned-item"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "CLI owned order", RequestID: "cli-owned-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	handler := f.e
	handler.agent, handler.runID = f.handler.ID, f.handler.RunID
	confirm := func(owns ...string) error {
		args := []string{"confirm", "--revision", fmt.Sprint(item.Revision), "--scope-revision", fmt.Sprint(item.ScopeRevision), "--order", fmt.Sprint(order.Seq), "--request-id", "cli-owned-intake", "--complete"}
		for _, p := range owns {
			args = append(args, "--owns", p)
		}
		_, err := captureCLIOutput(t, func() error { return cmdWorkOrderScope(handler, append(args, item.ID)) })
		return err
	}
	if err := confirm("hub/x.go", "docs/y.md"); err != nil {
		t.Fatal(err)
	}
	if err := confirm("hub/x.go", "docs/y.md"); err != nil {
		t.Fatalf("unchanged retry: %v", err)
	}
	if err := confirm("hub/x.go"); err == nil || !strings.Contains(err.Error(), "retry payload changed") {
		t.Fatalf("changed-owns retry: %v", err)
	}
	out, err := captureCLIOutput(t, func() error {
		return cmdWorkOrderScope(f.e, []string{"get", "--revision", fmt.Sprint(item.Revision), "--order", fmt.Sprint(order.Seq), item.ID})
	})
	var got api.WorkOrderScopeConfirmation
	if err != nil || json.Unmarshal([]byte(out), &got) != nil || strings.Join(got.Ownership, ",") != "hub/x.go,docs/y.md" {
		t.Fatalf("confirmation get %q %v", out, err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", item.ID, "--order", fmt.Sprint(order.Seq), "--cwd", repo})
	}); err != nil {
		t.Fatal(err)
	}
	list, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) })
	if err != nil || !strings.Contains(list, "owns=hub/x.go,docs/y.md ") {
		t.Fatalf("list %q %v", list, err)
	}
}

// t4 through tt work-items scope confirm --trigger against the real route
// (bug wi_75d75d0b9693a9e9).
func TestWorkOrderScopeCLIConfirmCitesTrigger(t *testing.T) {
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
	lead, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "lead", Host: "h", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler", Host: "h", Session: "db-handler", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Scope trigger flag", RequestID: "item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	order, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "bounded owner order", RequestID: "order", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "second bounded owner order", RequestID: "order-2", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	srv := recordTriggerWire(t, server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }), "/order-scope/confirm", &bodies)
	var confirmOrder func(seq int64, key string, extra ...string) (string, error)
	e := env{hub: srv.URL, task: task.ID, agent: handler.ID, runID: handler.RunID}
	confirm := func(key string, extra ...string) (string, error) {
		return confirmOrder(order.Seq, key, extra...)
	}
	confirmOrder = func(seq int64, key string, extra ...string) (string, error) {
		args := append([]string{"confirm", "--revision", fmt.Sprint(item.Revision), "--scope-revision", fmt.Sprint(item.ScopeRevision), "--order", fmt.Sprint(seq), "--request-id", key, "--complete"}, extra...)
		return captureCLIOutput(t, func() error { return cmdWorkOrderScope(e, append(args, item.ID)) })
	}
	trigger := triggerCLIRequest(t, st, task.ID, lead, handler)
	cited := []string{"--trigger", fmt.Sprint(trigger)}
	first, err := confirm("cited", cited...)
	if err != nil || len(bodies) != 1 || !strings.Contains(bodies[0], fmt.Sprintf(`"triggerSeq":%d`, trigger)) {
		t.Fatalf("cited confirm %q %v, wire %v", first, err, bodies)
	}
	withdrawCLITrigger(t, st, task.ID, lead, trigger)
	if again, err := confirm("cited", cited...); err != nil || again != first {
		t.Fatalf("retry after the withdrawal %q vs %q: %v", again, first, err)
	}
	if _, err = confirm("cited-2", cited...); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("triggering request #%d was withdrawn", trigger)) {
		t.Fatalf("new confirm citing the withdrawn request: %v", err)
	}
	if _, err = confirm("negative", "--trigger", "-1"); err == nil || !strings.Contains(err.Error(), "--trigger SEQ") {
		t.Fatalf("negative trigger: %v", err)
	}
	bodies = nil
	// An uncited confirm of the second order saves beside the withdrawn request.
	if out, err := confirmOrder(second.Seq, "uncited"); err != nil || !strings.Contains(out, item.ID) || len(bodies) != 1 || strings.Contains(bodies[0], "triggerSeq") {
		t.Fatalf("uncited confirm %q %v, wire %v", out, err, bodies)
	}
}

// g2 through tt work-items scope save --trigger against the real route (bug
// wi_fa80fcbfc4430606).
func TestWorkOrderScopeCLISaveCitesTrigger(t *testing.T) {
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
	lead, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "lead", Host: "h", Session: "lead", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler", Host: "h", Session: "db-handler", Runtime: "codex", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := st.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Scope save trigger flag", RequestID: "item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	link := []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
	order, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "bounded owner order", RequestID: "order", WorkItems: link}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConfirmWorkOrderScope(ctx, task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "confirm", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}); err != nil {
		t.Fatal(err)
	}
	source, err := st.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "decision", RequestID: "decision", WorkItems: link}, by)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	srv := recordTriggerWire(t, server.New(st, func(*http.Request) (api.Caller, error) { return by, nil }), "/order-bookkeeping", &bodies)
	e := env{hub: srv.URL, task: task.ID, agent: handler.ID, runID: handler.RunID}
	save := func(key string, extra ...string) (string, error) {
		args := append([]string{"save", "--revision", fmt.Sprint(item.Revision), "--order", fmt.Sprint(order.Seq), "--source", fmt.Sprint(source.Seq), "--kind", "decision", "--request-id", key}, extra...)
		return captureCLIOutput(t, func() error { return cmdWorkOrderScope(e, append(args, item.ID)) })
	}
	trigger := triggerCLIRequest(t, st, task.ID, lead, handler)
	cited := []string{"--trigger", fmt.Sprint(trigger)}
	first, err := save("cited", cited...)
	if err != nil || len(bodies) != 1 || !strings.Contains(bodies[0], fmt.Sprintf(`"triggerSeq":%d`, trigger)) {
		t.Fatalf("cited save %q %v, wire %v", first, err, bodies)
	}
	withdrawCLITrigger(t, st, task.ID, lead, trigger)
	if again, err := save("cited", cited...); err != nil || again != first {
		t.Fatalf("retry after the withdrawal %q vs %q: %v", again, first, err)
	}
	if _, err = save("cited-2", cited...); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("triggering request #%d was withdrawn", trigger)) {
		t.Fatalf("new save citing the withdrawn request: %v", err)
	}
	if _, err = save("negative", "--trigger", "-1"); err == nil || !strings.Contains(err.Error(), "--trigger SEQ") {
		t.Fatalf("negative trigger: %v", err)
	}
	bodies = nil
	if out, err := save("uncited"); err != nil || !strings.Contains(out, item.ID) || len(bodies) != 1 || strings.Contains(bodies[0], "triggerSeq") {
		t.Fatalf("uncited save %q %v, wire %v", out, err, bodies)
	}
}
