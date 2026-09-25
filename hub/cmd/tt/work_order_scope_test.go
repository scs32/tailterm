package main

import (
	"context"
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
