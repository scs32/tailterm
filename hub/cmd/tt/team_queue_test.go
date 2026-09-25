package main

import (
	"context"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"strings"
	"testing"
)

func TestTeamQueueCLIAddAndListAgainstTestHub(t *testing.T) {
	f := newTeamFixture(t, true)
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", f.item.ID, "--order", fmt.Sprint(f.order), "--cwd", t.TempDir()})
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"list"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, f.item.ID) || !strings.Contains(out, "queued") {
		t.Fatalf("queue list: %s", out)
	}
	q, err := f.c.ListTeamQueue(context.Background(), f.task.ID)
	if err != nil || len(q.Entries) != 1 || q.Entries[0].OrderMessageSeq != f.order {
		t.Fatalf("saved queue %+v %v", q, err)
	}
	second, err := f.c.CreateWorkItem(context.Background(), f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "second", RequestID: "second"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.c.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{Text: "second bounded order", RequestID: "second-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: second.ID, ItemRevision: second.Revision, Relationship: "primary"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"add", "--item", second.ID, "--order", fmt.Sprint(order.Seq), "--cwd", t.TempDir()})
	}); err != nil {
		t.Fatal(err)
	}
	q, err = f.c.ListTeamQueue(context.Background(), f.task.ID)
	if err != nil || len(q.Entries) != 2 {
		t.Fatalf("two entries %+v %v", q, err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdTeamQueue(f.e, []string{"reorder", "--entry", q.Entries[1].ID, "--before", q.Entries[0].ID})
	}); err != nil {
		t.Fatal(err)
	}
	q, err = f.c.ListTeamQueue(context.Background(), f.task.ID)
	if err != nil || q.Entries[0].ItemID != second.ID {
		t.Fatalf("CLI reorder %+v %v", q, err)
	}
	if _, err := captureCLIOutput(t, func() error { return cmdTeamQueue(f.e, []string{"remove", "--entry", q.Entries[0].ID}) }); err != nil {
		t.Fatal(err)
	}
	q, err = f.c.ListTeamQueue(context.Background(), f.task.ID)
	if err != nil || len(q.Entries) != 1 || q.Entries[0].ItemID != f.item.ID {
		t.Fatalf("CLI remove %+v %v", q, err)
	}
}
