package main

import (
	"context"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"strings"
	"testing"
)

func TestOwnerSendAndReadRealHub(t *testing.T) {
	e, c, task, _ := cliWorkItemFixture(t)
	ctx := context.Background()
	agents, err := c.ListAgents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.ID == e.agent {
			e.runID = a.RunID
		}
	}
	item, err := c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Owner CLI fixture", RequestID: "owner-item"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Bounded owner request fixture order"})
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"owner", ""} {
		args := []string{"--kind", "request", "--subject", "Approve the exact owner fixture text", "--ask", "Please approve", "--expected-answer", "APPROVE unicode 矩阵\n", "--work-item", item.ID, "--work-item-revision", "1", "--work-order-message", fmt.Sprint(order.Seq), "--request-id", "owner-ask-" + to}
		if to != "" {
			args = append(args, "--to", to)
		}
		first, err := captureCLIOutput(t, func() error { return cmdSend(e, args) })
		if err != nil {
			t.Fatal(err)
		}
		again, err := captureCLIOutput(t, func() error { return cmdSend(e, args) })
		if err != nil || first != again {
			t.Fatalf("retry %q %q %v", first, again, err)
		}
	}
	list, err := c.ListOwnerObligations(ctx, task.ID, true)
	if err != nil || len(list) != 2 {
		t.Fatalf("list %+v %v", list, err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdObligations(e, []string{"--owner"}) })
	if err != nil || !strings.Contains(out, "→ owner") || strings.Contains(out, "tt ack") {
		t.Fatalf("owner list %q %v", out, err)
	}
	if list[0].Request.Envelope.ExpectedAnswer != "APPROVE unicode 矩阵\n" {
		t.Fatal("exact bytes lost")
	}
}
