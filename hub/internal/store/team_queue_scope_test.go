package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// commaOwnershipRefused reports whether err is the named comma refusal.
func commaOwnershipRefused(err error) bool {
	return errors.Is(err, api.ErrInvalid) && strings.Contains(err.Error(), "contains a comma; give each path separately")
}

func TestQueueOwnershipRefusesCommaPath(t *testing.T) {
	for _, bad := range []string{"a.go,b.go", "src/a.go,src/b.go", "src/a.go,", ",src/a.go", "src/a,b/c.go"} {
		if _, err := canonicalQueueOwnership([]string{"docs/ok.md", bad}); !commaOwnershipRefused(err) {
			t.Errorf("comma path %q: %v", bad, err)
		}
	}
	got, err := canonicalQueueOwnership([]string{"src/a.go", "src/b.go"})
	if err != nil || strings.Join(got, "|") != "src/a.go|src/b.go" {
		t.Fatalf("separate paths %v %v", got, err)
	}
}

// The hub refuses a comma path wherever ownership is declared, whatever the
// client sent: queue add, a queue scope change and order-scope confirmation.
func TestHubRefusesCommaOwnershipPath(t *testing.T) {
	s, task, items, orders := queueFixture(t)
	ctx := context.Background()
	joined := []string{"src/a.go,src/b.go"}
	add := api.TeamQueueRequest{RequestID: "add-comma", Operation: "add", ItemID: items[0].ID, OrderMessageSeq: orders[0].Seq, Host: "mini", Cwd: "/tmp", Ownership: joined}
	if _, err := s.TeamQueueAction(ctx, task.ID, add); !commaOwnershipRefused(err) {
		t.Fatalf("queue add: %v", err)
	}
	if list, err := s.ListTeamQueue(ctx, task.ID); err != nil || len(list.Entries) != 0 {
		t.Fatalf("refused add left entries %+v %v", list.Entries, err)
	}
	add.RequestID, add.Ownership = "add-separate", []string{"src/a.go", "src/b.go"}
	q, err := s.TeamQueueAction(ctx, task.ID, add)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: "scope-comma", Operation: "scope", EntryID: q.ID, ExpectedRevision: q.Revision, Ownership: joined}); !commaOwnershipRefused(err) {
		t.Fatalf("queue scope: %v", err)
	}
	if got, err := s.GetTeamQueueEntry(ctx, task.ID, q.ID); err != nil || got.Revision != q.Revision || strings.Join(got.Ownership, "|") != "src/a.go|src/b.go" {
		t.Fatalf("refused scope changed the entry %+v %v", got, err)
	}
	handler := choresHandler(t, s, task.ID)
	item := items[1]
	order := unconfirmedOrder(t, s, task, item, "comma-order")
	confirm := api.ConfirmWorkOrderScopeRequest{RequestID: "comma-intake", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true, Ownership: joined}
	if _, err := s.ConfirmWorkOrderScope(ctx, task.ID, item.ID, confirm); !commaOwnershipRefused(err) {
		t.Fatalf("order-scope confirmation: %v", err)
	}
	if _, err := s.GetWorkOrderScopeConfirmation(ctx, task.ID, item.ID, item.Revision, order.Seq); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("refused confirmation was saved: %v", err)
	}
}

func TestQueueOwnershipCanonicalAndOverlap(t *testing.T) {
	for _, bad := range []string{"", "/src", "../src", "src/../go", "./src", "src//go", "src/", "src\\go", "src:go", "src/./go"} {
		if _, err := canonicalQueueOwnership([]string{bad}); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := canonicalQueueOwnership([]string{"src/Foo", "src/foo"}); err == nil {
		t.Fatal("accepted case alias")
	}
	for _, tc := range []struct {
		a, b []string
		want bool
	}{
		{[]string{"src/a.go"}, []string{"src/a.go"}, true},
		{[]string{"src"}, []string{"src/a.go"}, true},
		{[]string{"src/a"}, []string{"src/ab"}, false},
		{[]string{"src/a.go"}, []string{"src/b.go"}, false},
		{nil, []string{"src/a.go"}, true},
	} {
		if got := queueScopesConflict(tc.a, tc.b); got != tc.want {
			t.Errorf("overlap %v %v = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
}
