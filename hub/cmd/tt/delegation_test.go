package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestOwnerDelegationCLI(t *testing.T) {
	e, c, task, lead := cliWorkItemFixture(t)
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
	owner := env{hub: e.hub, task: e.task}
	delegate := env{hub: e.hub, task: e.task, agent: lead.ID, agentName: lead.Name, runID: lead.RunID}

	if _, err := captureCLIOutput(t, func() error { return cmdOwner(e, []string{"delegation", "list"}) }); err == nil || !strings.Contains(err.Error(), "not agent sessions") {
		t.Fatalf("agent session listed windows: %v", err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdOwner(owner, []string{"delegation", "open", "--delegate", "lead", "--for", "2h", "--scope", "everything"})
	}); err == nil {
		t.Fatal("unknown scope accepted")
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdOwner(owner, []string{"delegation", "open", "--delegate", "lead", "--for", "30s", "--request-id", "short"})
	}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("short window: %v", err)
	}
	open := []string{"delegation", "open", "--delegate", "lead", "--for", "2h", "--scope", "decisions-merges-deploys", "--reason", "at the dentist", "--request-id", "open-1"}
	out, err := captureCLIOutput(t, func() error { return cmdOwner(owner, open) })
	if err != nil || !strings.Contains(out, "open  → lead  decisions-merges-deploys") {
		t.Fatalf("open %q %v", out, err)
	}
	again, err := captureCLIOutput(t, func() error { return cmdOwner(owner, open) })
	if err != nil || again != out {
		t.Fatalf("replay %q %v", again, err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdOwner(owner, []string{"delegation", "open", "--delegate", "lead", "--for", "1h", "--request-id", "open-2"})
	}); err == nil || !strings.Contains(err.Error(), "already open") {
		t.Fatalf("second open: %v", err)
	}

	item, err := c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Delegation CLI fixture", RequestID: "delegation-item"})
	if err != nil {
		t.Fatal(err)
	}
	order, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Bounded delegation fixture order"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdSend(e, []string{"--kind", "request", "--to", "owner", "--subject", "Approve the fixture deploy for tonight", "--ask", "Deploy tonight?", "--ref", "category=deploy",
			"--work-item", item.ID, "--work-item-revision", "1", "--work-order-message", fmt.Sprint(order.Seq), "--request-id", "deploy-ask"})
	}); err != nil {
		t.Fatal(err)
	}
	owed, err := c.ListOwnerObligations(ctx, task.ID, true)
	if err != nil || len(owed) != 1 {
		t.Fatalf("owner requests %v %+v", err, owed)
	}
	if _, err := captureCLIOutput(t, func() error {
		return cmdOwner(delegate, []string{"answer", owed[0].ID, "--text", "Deploy tonight"})
	}); err == nil || !strings.Contains(err.Error(), "--rationale") {
		t.Fatalf("missing rationale: %v", err)
	}
	if out, err := captureCLIOutput(t, func() error {
		return cmdOwner(delegate, []string{"answer", owed[0].ID, "--text", "Deploy tonight", "--rationale", "Tests passed and the window is quiet.", "--request-id", "delegate-answer"})
	}); err != nil || !strings.Contains(out, "closed (answered)") {
		t.Fatalf("delegate answer %q %v", out, err)
	}

	askFile := filepath.Join(t.TempDir(), "ask.json")
	if err := os.WriteFile(askFile, []byte(`{"question":"Which fixture rollout?","category":"decision","options":[{"id":"staged","label":"Staged","description":"A few first."},{"id":"all","label":"All","description":"Everyone."}],"recommendedOptionId":"staged","recommendationReason":"Safer."}`), 0600); err != nil {
		t.Fatal(err)
	}
	e.task = task.ID
	asked, err := captureCLIOutput(t, func() error { return cmdAsk(e, []string{"--request-id", "cli-ask", "--file", askFile, "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var decision api.Message
	if err := json.Unmarshal([]byte(asked), &decision); err != nil {
		t.Fatal(err)
	}
	seq := fmt.Sprint(decision.Seq)
	if _, err := captureCLIOutput(t, func() error { return cmdAsk(delegate, []string{"answer", seq, "--option", "staged"}) }); err == nil {
		t.Fatal("delegate answered without a rationale")
	}
	if out, err := captureCLIOutput(t, func() error {
		return cmdAsk(delegate, []string{"answer", seq, "--option", "staged", "--rationale", "It is the recommendation and reversible.", "--request-id", "cli-answer"})
	}); err != nil || !strings.Contains(out, "Decision #"+seq+" answered") {
		t.Fatalf("delegate decision %q %v", out, err)
	}

	listed, err := captureCLIOutput(t, func() error { return cmdOwner(owner, []string{"delegation", "list", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var list api.DelegationWindowList
	if err := json.Unmarshal([]byte(listed), &list); err != nil || len(list.Windows) != 1 {
		t.Fatalf("list %q %v", listed, err)
	}
	w := list.Windows[0]
	if w.Reason != "at the dentist" || w.OpenedSource == nil || w.OpenedSource.Kind != "tt" || len(w.Routes) != 2 {
		t.Fatalf("window %+v", w)
	}
	for _, r := range w.Routes {
		if r.AnswerSeq == 0 || r.Rationale == "" || r.AnsweredByName != "lead" {
			t.Fatalf("route %+v", r)
		}
	}
	if out, err := captureCLIOutput(t, func() error { return cmdOwner(owner, []string{"delegation", "close", w.ID, "--reason", "back"}) }); err != nil || !strings.Contains(out, "closed") {
		t.Fatalf("close %q %v", out, err)
	}
	text, err := captureCLIOutput(t, func() error { return cmdOwner(owner, []string{"delegation", "list"}) })
	if err != nil || !strings.Contains(text, "closed") || !strings.Contains(text, "rationale: Tests passed and the window is quiet.") || !strings.Contains(text, "rationale: It is the recommendation and reversible.") {
		t.Fatalf("list after close %q %v", text, err)
	}
}
