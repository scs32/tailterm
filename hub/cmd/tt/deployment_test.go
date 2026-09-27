package main

import (
	"github.com/scs32/tailterm/hub/internal/api"
	"strings"
	"testing"
)

func TestDeploymentSetupUsesExactRetryJournal(t *testing.T) {
	f := newHandlerFixture(t)
	f.req.Role = api.AgentRoleDeployment
	f.req.Name = "deployer"
	a, err := f.launch()
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.launch()
	if err != nil || again.RunID != a.RunID {
		t.Fatal(again, err)
	}
	f.executions(t, 1)
	if !strings.HasPrefix(a.Session, "tt-deployer-") {
		t.Fatal(a.Session)
	}
}
func TestDeploymentBriefingPreservesHandlerBoundary(t *testing.T) {
	b := deploymentBriefing()
	for _, text := range []string{"Never read or mutate work-item APIs", "exact-SHA", "private host credential", "persistent", "handler"} {
		if !strings.Contains(strings.ToLower(b), strings.ToLower(text)) {
			t.Fatal(text)
		}
	}
}
