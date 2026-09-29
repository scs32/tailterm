package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
)

// runtimePromptHub is an isolated SQLite hub behind a local HTTP server.
func runtimePromptHub(t *testing.T) (*store.Store, *httptest.Server, api.Task) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hub := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "owner"}, nil }))
	t.Cleanup(hub.Close)
	task, err := st.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Runtime prompt fixture"}, api.Caller{Node: "fixture", User: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	return st, hub, task
}

func TestPromptPolicyCLI(t *testing.T) {
	_, hub, task := runtimePromptHub(t)
	owner := env{hub: hub.URL, task: task.ID}
	out, err := captureRelayOutput(t, false, func() error { return cmdPromptPolicy(owner, []string{"get"}) })
	if err != nil || !strings.Contains(out, "(revision 0)") || !strings.Contains(out, "codex_rate_limit_switch    keep_current_never_show") || !strings.Contains(out, "codex_trust                escalate") {
		t.Fatalf("get %q %v", out, err)
	}
	out, err = captureRelayOutput(t, false, func() error {
		return cmdPromptPolicy(owner, []string{"set", "--revision", "0", "--action", "codex_rate_limit_switch=keep_current", "--action", "codex_model_migration=escalate"})
	})
	if err != nil || !strings.Contains(out, "(revision 1)") || !strings.Contains(out, "codex_rate_limit_switch    keep_current ") || !strings.Contains(out, "codex_model_migration      escalate") {
		t.Fatalf("set %q %v", out, err)
	}
	if err := cmdPromptPolicy(owner, []string{"set", "--revision", "0", "--action", "codex_rate_limit_switch=escalate"}); err == nil || !strings.Contains(err.Error(), "409") && !strings.Contains(err.Error(), "revision") {
		t.Fatalf("stale revision: %v", err)
	}
	agent := env{hub: hub.URL, task: task.ID, agent: "agt_0123456789abcdef"}
	if err := cmdPromptPolicy(agent, []string{"set", "--revision", "1", "--action", "codex_rate_limit_switch=escalate"}); err == nil || !strings.Contains(err.Error(), "owner command") {
		t.Fatalf("agent session: %v", err)
	}
	for _, action := range []string{"claude_permission=approve", "codex_rate_limit_switch=switch", "unknown=report", "codex_trust=trust", "nonsense"} {
		if err := cmdPromptPolicy(owner, []string{"set", "--revision", "1", "--action", action}); err == nil || !strings.Contains(err.Error(), "not an allowed") {
			t.Fatalf("%s: %v", action, err)
		}
	}
	out, err = captureRelayOutput(t, false, func() error { return cmdPromptPolicy(owner, []string{"get", "--json"}) })
	if err != nil || !strings.Contains(out, `"revision": 1`) {
		t.Fatalf("json %q %v", out, err)
	}
}

func readRuntimePromptFixture(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", path))
	if err != nil {
		t.Fatal(err)
	}
	return plainRuntimeScreen(string(raw))
}

// TestRuntimePromptClassify maps every captured pane to its kind (plan a1).
// The Codex captures are live (testdata/runtime-prompt/README.md) except the
// rate-limit menu, which is reconstructed and labelled so.
func TestRuntimePromptClassify(t *testing.T) {
	for _, tc := range []struct {
		runtime, path, kind string
		selected, options   int
	}{
		{"codex", "runtime-prompt/codex-rate-limit.reconstructed.ansi", api.RuntimePromptCodexRateLimit, 0, 3},
		{"codex", "runtime-prompt/codex-model-migration.ansi", api.RuntimePromptCodexModelMigration, 0, 2},
		{"codex", "runtime-prompt/codex-model-migration-down.ansi", api.RuntimePromptCodexModelMigration, 1, 2},
		{"codex", "runtime-prompt/codex-trust.ansi", api.RuntimePromptCodexTrust, 0, 2},
		{"codex", "runtime-prompt/codex-update.ansi", api.RuntimePromptUnknown, 0, 3},
		{"codex", "runtime-prompt/codex-model-picker.ansi", api.RuntimePromptUnknown, 6, 7},
		{"claude", "claude-pane/permission-dialog.ansi", api.RuntimePromptClaudePermission, 0, 4},
		{"claude", "claude-pane/selection-dialog.ansi", api.RuntimePromptClaudeSelection, 0, 4},
		{"claude", "claude-pane/trust-dialog.ansi", api.RuntimePromptClaudeTrust, -1, 0}, // unnumbered choices
	} {
		m, ok := classifyRuntimePrompt(tc.runtime, readRuntimePromptFixture(t, tc.path))
		if !ok || m.Kind != tc.kind || len(m.Fingerprint) != 32 || m.selected() != tc.selected || len(m.Options) != tc.options {
			t.Errorf("%s: got ok=%v %+v, want %s selected=%d options=%d", tc.path, ok, m, tc.kind, tc.selected, tc.options)
		}
	}
	rate, _ := classifyRuntimePrompt("codex", readRuntimePromptFixture(t, "runtime-prompt/codex-rate-limit.reconstructed.ansi"))
	if rate.option("Keep current model (never show again)") != 2 || rate.option("Keep current model") != 1 {
		t.Fatalf("rate-limit options %+v", rate.Options)
	}
	// Moving the selection keeps the prompt's identity.
	up, _ := classifyRuntimePrompt("codex", readRuntimePromptFixture(t, "runtime-prompt/codex-model-migration.ansi"))
	down, _ := classifyRuntimePrompt("codex", readRuntimePromptFixture(t, "runtime-prompt/codex-model-migration-down.ansi"))
	if up.Fingerprint != down.Fingerprint {
		t.Fatal("selection changed the fingerprint")
	}
	for _, tc := range []struct{ runtime, path string }{
		{"claude", "claude-pane/suggestion.ansi"},
		{"claude", "claude-pane/try-placeholder.ansi"},
		{"claude", "claude-pane/scrollback-dialog-words.ansi"},
		{"claude", "claude-pane/scrollback-dialog-words-typed.ansi"},
		{"claude", "claude-pane/typed.ansi"},
		{"codex", "runtime-prompt/codex-idle-composer.ansi"},
		{"codex", "runtime-prompt/codex-scrollback-quote.ansi"},
	} {
		if m, ok := classifyRuntimePrompt(tc.runtime, readRuntimePromptFixture(t, tc.path)); ok {
			t.Errorf("%s: idle screen read as prompt %+v", tc.path, m)
		}
	}
}

// Synthetic screens cover layouts no capture has: a numbered menu with no
// known title is unknown, and a known title with drifted labels is unknown.
func TestRuntimePromptClassifySynthetic(t *testing.T) {
	for _, tc := range []struct{ runtime, screen, kind string }{
		{"codex", "  Pick a thing\n\n› 1. Alpha\n  2. Beta\n\n  enter select · esc back", api.RuntimePromptUnknown},
		{"codex", "  Approaching rate limits\n\n› 1. Switch to gpt-x\n  2. Keep model\n  3. Keep model (never show again)\n\n  enter select · esc back", api.RuntimePromptUnknown},
		{"codex", "  Usage limit reached\n  Request a limit increase from your owner to continue using codex. Request increase\n\n› 1. Yes\n  2. No\n\n  enter confirm · esc cancel", api.RuntimePromptCodexUsageLimit},
		{"codex", "Do you trust the contents of this directory?\n\n› 1. Yes, continue\n  2. No, quit\n\nPress enter to continue", api.RuntimePromptCodexTrust},
		{"claude", "some output\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. Maybe\n Esc to cancel", api.RuntimePromptClaudePermission},
		{"claude", "some output\n\n Choose\n ❯ 1. Red\n   2. Blue", api.RuntimePromptUnknown},
		{"claude", "some output\n\n Continue anyway? (y/n)", api.RuntimePromptUnknown},
	} {
		m, ok := classifyRuntimePrompt(tc.runtime, strings.Split(tc.screen, "\n"))
		if !ok || m.Kind != tc.kind {
			t.Errorf("%q: got ok=%v %+v, want %s", tc.screen, ok, m, tc.kind)
		}
	}
	// Codex transcript that ends with quoted option text and no key hint.
	if m, ok := classifyRuntimePrompt("codex", strings.Split("› 3. Keep current model (never show again)\n\n› Ask Codex to do anything\n\n  GPT-5.5 default · ~/x", "\n")); ok {
		t.Fatalf("quoted option read as prompt %+v", m)
	}
}
