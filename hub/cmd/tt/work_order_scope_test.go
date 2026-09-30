package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
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
