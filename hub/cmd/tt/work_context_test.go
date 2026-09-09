package main

import (
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestFormatWorkItemContextIsExplicitlyScoped(t *testing.T) {
	context := api.AgentWorkItemContext{
		Version: 1,
		Binding: api.AgentWorkItemBinding{
			AgentID: "agt_0000000000000001", RunID: "run_0000000000000002",
			ItemTaskID: "tsk_0000000000000003", ItemID: "wi_0000000000000004", ItemRevision: 7,
			ContextThroughMessageSeq: 816,
		},
		Bundle: []byte(`{"version":1,"history":{"description":"Only relevant content"}}`),
	}
	formatted, err := formatWorkItemContext(context)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"exact agent run", "intentionally excluded", "contextThroughMessageSeq", "816", "Only relevant content"} {
		if !strings.Contains(formatted, required) {
			t.Fatalf("formatted context missing %q: %s", required, formatted)
		}
	}
}
