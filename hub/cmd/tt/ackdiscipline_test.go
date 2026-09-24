package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// k8: the Codex Stop hook installs idempotently beside other hooks, and
// Codex agents get the trust bypass only when it is installed.
func TestCodexStopHookInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if got := codexHookCommand("codex", "codex", "codex"); got != "codex" {
		t.Fatalf("without the hook the command changed: %q", got)
	}
	existing := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"other-tool check"}]}]}}`
	if err := os.WriteFile(filepath.Join(home, "hooks.json"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := captureCLIOutput(t, func() error { return installCodexStopHook("/usr/local/bin/tt") }); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(home, "hooks.json"))
	var doc struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Hooks["Stop"]) != 1 || len(doc.Hooks["PreToolUse"]) != 1 || !strings.Contains(string(raw), "/usr/local/bin/tt hook stop") {
		t.Fatalf("hooks.json after two installs = %s (%v)", raw, err)
	}
	if got := codexHookCommand("codex '-m' 'gpt-6-sol'", "codex", "codex"); !strings.HasSuffix(got, "'--dangerously-bypass-hook-trust'") {
		t.Fatalf("with the hook installed the command is %q", got)
	}
	// Fable f8 and f10: a Codex executable by path gets the bypass too, and a
	// hooks.json that merely mentions "hook stop" is not the installed hook.
	if got := codexHookCommand("/opt/homebrew/bin/codex", "codex", "/opt/homebrew/bin/codex"); !strings.HasSuffix(got, "'--dangerously-bypass-hook-trust'") {
		t.Fatalf("a Codex path did not get the bypass: %q", got)
	}
	if got := codexHookCommand("codexx", "codex", "codexx"); got != "codexx" {
		t.Fatalf("a non-Codex executable got the bypass: %q", got)
	}
	other := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo hook stop now"}]}]}}`
	if err := os.WriteFile(filepath.Join(home, "hooks.json"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if codexStopHookInstalled() {
		t.Fatal("a look-alike command counted as the Tailterm hook")
	}
	if got := codexHookCommand("claude", "claude", "claude"); got != "claude" {
		t.Fatalf("a Claude command changed: %q", got)
	}
}

// k9: the summary reports acknowledgement latency and gating work.
func TestSummarizeAcks(t *testing.T) {
	now := time.Now()
	acked := func(seq int64, after time.Duration) api.Obligation {
		created := now.Add(-time.Hour)
		at := created.Add(after)
		return api.Obligation{MessageSeq: seq, AgentID: "agt_b", Needs: api.ObligationNeedsOutcome, State: api.ObligationAcknowledged, CreatedAt: created, AckedAt: &at}
	}
	list := []api.Obligation{acked(1, time.Minute), acked(2, 3*time.Minute), acked(3, 20*time.Minute),
		{MessageSeq: 4, AgentID: "agt_b", Needs: api.ObligationNeedsOutcome, State: api.ObligationDelivered, CreatedAt: now.Add(-10 * time.Minute)},
		{MessageSeq: 5, AgentID: "agt_b", Needs: api.ObligationNeedsDelivery, State: api.ObligationQueued, CreatedAt: now.Add(-10 * time.Minute)}}
	out := summarizeAcks(list, map[string]string{"agt_b": "builder"}, time.Time{}, now)
	for _, want := range []string{"3 acknowledged", "median 3m0s", "worst 20m0s (#3 by builder)", "Unacknowledged past the 2m0s grace: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

// Round-one R1-C3: latency runs from delivery, not creation, and an even
// sample takes the mean of the two middle values.
func TestSummarizeAcksFromDelivery(t *testing.T) {
	now := time.Now()
	acked := func(seq int64, queued, toAck time.Duration) api.Obligation {
		created := now.Add(-time.Hour)
		delivered := created.Add(queued)
		at := delivered.Add(toAck)
		return api.Obligation{MessageSeq: seq, AgentID: "agt_b", Needs: api.ObligationNeedsOutcome, State: api.ObligationAcknowledged, CreatedAt: created, DeliveredAt: &delivered, AckedAt: &at}
	}
	// #1 sat queued for ten minutes and was acknowledged a minute after delivery.
	list := []api.Obligation{acked(1, 10*time.Minute, time.Minute), acked(2, 0, 3*time.Minute), acked(3, 0, 5*time.Minute), acked(4, time.Minute, 7*time.Minute)}
	out := summarizeAcks(list, map[string]string{"agt_b": "builder"}, time.Time{}, now)
	for _, want := range []string{"4 acknowledged", "median 4m0s", "worst 7m0s (#4 by builder)"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}
