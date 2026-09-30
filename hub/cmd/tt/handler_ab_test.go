package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Handler arm CLI tests (wi_fc1396aef8a72a06, order #14869): an isolated hub
// behind httptest, temporary HOME and relay state, synthetic agents only.

func TestHandlerArmCommandParseArm(t *testing.T) {
	for v, want := range map[string]api.HandlerArm{
		"S=claude-sonnet-5-5/claude/high:1": {ID: "S", Model: "claude-sonnet-5-5", Runtime: "claude", Reasoning: "high", Weight: 1},
		"O=gpt-6.1-sol/codex/high:3":        {ID: "O", Model: "gpt-6.1-sol", Runtime: "codex", Reasoning: "high", Weight: 3},
		"C=custom/model/codex/low:2":        {ID: "C", Model: "custom/model", Runtime: "codex", Reasoning: "low", Weight: 2},
	} {
		got, err := parseArmFlag(v)
		if err != nil || got != want {
			t.Fatalf("%s: %+v %v", v, got, err)
		}
		if formatArm(got) != v {
			t.Fatalf("format %s", formatArm(got))
		}
	}
	for _, v := range []string{"S", "=m/r/x:1", "S=m/r:1", "S=m/r/x", "S=m/r/x:one"} {
		if _, err := parseArmFlag(v); err == nil {
			t.Fatalf("%s parsed", v)
		}
	}
}

func armCLIFixture(t *testing.T) (teamFixture, api.Agent) {
	t.Helper()
	f := newTeamFixture(t, false)
	ctx := context.Background()
	spec, err := newHandlerSpec(f.e.hub, f.task.ID, []string{"--run", "claude", "--prompt", "Planned database role", "--model", "claude-sonnet-5-5", "--reasoning", "high"})
	if err != nil {
		t.Fatal(err)
	}
	if err = saveHandlerSpec(spec); err != nil {
		t.Fatal(err)
	}
	h, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler-s", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "fixture-s",
		Runtime: "claude", TemplateDigest: handlerTemplateDigest("Planned database role"), HandlerModel: "claude-sonnet-5-5", HandlerReasoning: "high"})
	if err != nil {
		t.Fatal(err)
	}
	return f, h
}

func TestHandlerArmCommandSetGetReplayAndRefusals(t *testing.T) {
	f, h := armCLIFixture(t)
	set := []string{"set", "--task", f.task.ID, "--revision", "0", "--arm", "S=claude-sonnet-5-5/claude/high:1", "--arm", "O=gpt-6.1-sol/codex/high:1", "--seed", "K", "--fallback=false", "--enabled"}
	agent := f.e
	agent.agent = h.ID
	if err := cmdHandlerArms(agent, set); err == nil || !strings.Contains(err.Error(), "owner command") {
		t.Fatalf("agent session: %v", err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdHandlerArms(f.e, set) })
	if err != nil {
		t.Fatal(err)
	}
	digest := handlerTemplateDigest("Planned database role")
	for _, want := range []string{"Handler arms: enabled, revision 1", "arm O=gpt-6.1-sol/codex/high:1", "arm S=claude-sonnet-5-5/claude/high:1", "Template digest: " + digest, "digestMatches=true arm S"} {
		if !strings.Contains(out, want) {
			t.Fatalf("set output lacks %q:\n%s", want, out)
		}
	}
	// The same command replays its derived request ID.
	if _, err = captureCLIOutput(t, func() error { return cmdHandlerArms(f.e, set) }); err != nil {
		t.Fatalf("replay: %v", err)
	}
	out, err = captureCLIOutput(t, func() error { return cmdHandlerArms(f.e, []string{"get", "--task", f.task.ID, "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	var view api.HandlerArmPolicyView
	if err = json.Unmarshal([]byte(out), &view); err != nil || view.Policy.Revision != 1 || !view.Policy.Enabled || view.Policy.Fallback || view.Policy.Seed != "K" ||
		view.Policy.LimitHoldMinutes != 60 || view.Policy.TemplateDigest != digest || len(view.Policy.Arms) != 2 || len(view.Handlers) != 1 || !view.Handlers[0].DigestMatches {
		t.Fatalf("get %+v %v", view, err)
	}
	// Stale revision and validation refusals write nothing.
	if err = cmdHandlerArms(f.e, []string{"set", "--task", f.task.ID, "--revision", "0", "--seed", "other"}); err == nil || !strings.Contains(err.Error(), "stale_revision") {
		t.Fatalf("stale: %v", err)
	}
	if err = cmdHandlerArms(f.e, []string{"set", "--task", f.task.ID, "--revision", "1", "--hold-minutes", "2"}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("invalid hold: %v", err)
	}
	// A prompt file with another template is refused while handlers differ.
	prompt := filepath.Join(t.TempDir(), "prompt.txt")
	if err = os.WriteFile(prompt, []byte("Another database role"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = cmdHandlerArms(f.e, []string{"set", "--task", f.task.ID, "--revision", "1", "--prompt-file", prompt}); err == nil || !strings.Contains(err.Error(), "template_mismatch") || !strings.Contains(err.Error(), h.ID) {
		t.Fatalf("template mismatch: %v", err)
	}
	after, err := f.c.HandlerArmPolicy(context.Background(), f.task.ID)
	if err != nil || after.Policy.Revision != 1 || after.Policy.Seed != "K" {
		t.Fatalf("refusals changed the policy %+v %v", after.Policy, err)
	}
	if _, err = captureCLIOutput(t, func() error {
		return cmdHandlerArms(f.e, []string{"set", "--task", f.task.ID, "--revision", "1", "--enabled=false", "--prompt-file", prompt})
	}); err != nil {
		t.Fatalf("disabled save with another template: %v", err)
	}
}

// Disabling, or saving a disabled policy, needs no saved spec or digest.
func TestHandlerArmCommandDisableNeedsNoSpec(t *testing.T) {
	f, _ := armCLIFixture(t)
	if err := os.Remove(handlerSpecPath(f.e.hub, f.task.ID)); err != nil {
		t.Fatal(err)
	}
	arms := []string{"--arm", "S=claude-sonnet-5-5/claude/high:1", "--arm", "O=gpt-6.1-sol/codex/high:1", "--seed", "K"}
	if _, err := captureCLIOutput(t, func() error {
		return cmdHandlerArms(f.e, append([]string{"set", "--task", f.task.ID, "--revision", "0", "--enabled=false"}, arms...))
	}); err != nil {
		t.Fatalf("disabled save without a spec: %v", err)
	}
	if err := cmdHandlerArms(f.e, []string{"set", "--task", f.task.ID, "--revision", "1", "--enabled"}); err == nil || !strings.Contains(err.Error(), "no handler launch spec") {
		t.Fatalf("enabling without a spec: %v", err)
	}
	view, err := f.c.HandlerArmPolicy(context.Background(), f.task.ID)
	if err != nil || view.Policy.Revision != 1 || view.Policy.Enabled || view.Policy.TemplateDigest != "" {
		t.Fatalf("policy %+v %v", view.Policy, err)
	}
}

func TestHandlerABReportCommandPrintsArms(t *testing.T) {
	f, _ := armCLIFixture(t)
	if _, err := captureCLIOutput(t, func() error {
		return cmdHandlerArms(f.e, []string{"set", "--task", f.task.ID, "--revision", "0", "--arm", "S=claude-sonnet-5-5/claude/high:1", "--arm", "O=gpt-6.1-sol/codex/high:1", "--seed", "K"})
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdHandlerABReport(f.e, []string{"--task", f.task.ID}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Handler A/B report for " + f.task.ID, "Arm O gpt-6.1-sol/codex/high: n=0", "Arm S claude-sonnet-5-5/claude/high: n=0", "Compare O vs S responseMillis: insufficient", "not a significance test"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report lacks %q:\n%s", want, out)
		}
	}
	out, err = captureCLIOutput(t, func() error { return cmdHandlerABReport(f.e, []string{"--task", f.task.ID, "--json"}) })
	var report api.HandlerABReport
	if err != nil || json.Unmarshal([]byte(out), &report) != nil || len(report.Arms) != 2 || report.Items == nil {
		t.Fatalf("json report %s %v", out, err)
	}
	if err = cmdHandler(f.e, []string{"ab-report", "--task", "bad"}); err == nil {
		t.Fatal("bad task accepted")
	}
}
