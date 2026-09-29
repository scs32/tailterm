package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// promptRig is an isolated hub, one registered agent with a relay binding,
// its transcript under a private HOME and a fake pane.
type promptRig struct {
	t       *testing.T
	st      *store.Store
	task    api.Task
	agent   api.Agent
	b       runtimeBinding
	client  *api.Client
	bodies  []string
	screen  string
	inspect int
	keys    []string
	deps    *runtimePromptDeps
	clock   time.Time
	// override answers a request before the hub when it returns true.
	override func(http.ResponseWriter, *http.Request) bool
}

func newPromptRig(t *testing.T, runtime string) *promptRig {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(home, "relay"))
	resetRuntimePromptPolicies()
	st, _, task := runtimePromptHub(t)
	r := &promptRig{t: t, st: st, task: task}
	inner := server.New(st, func(*http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "owner"}, nil })
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Body != nil {
			body, _ := io.ReadAll(req.Body)
			r.bodies = append(r.bodies, string(body))
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		if r.override != nil && r.override(w, req) {
			return
		}
		inner.ServeHTTP(w, req)
	}))
	t.Cleanup(hub.Close)
	ctx := context.Background()
	owner := api.Caller{Node: "fixture", User: "owner"}
	a, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "builder", Host: "mini", Session: "prompt-rig", Runtime: runtime, Cwd: home}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventRunning}, owner); err != nil {
		t.Fatal(err)
	}
	r.agent = a
	r.client, _ = api.NewClient(hub.URL, 5*time.Second)
	thread := "12345678-1234-1234-1234-123456789abc"
	r.b = runtimeBinding{Hub: hub.URL, Task: task.ID, Agent: a.ID, Run: a.RunID, Thread: thread, Runtime: runtime, Session: "prompt-rig", Codex: filepath.Join(home, "codex")}
	r.clock = time.Date(2026, 9, 28, 20, 0, 0, 0, time.UTC)
	r.deps = &runtimePromptDeps{
		inspect: func(context.Context, runtimeBinding) (runtimePane, error) {
			r.inspect++
			return runtimePane{Pane: "%1", SessionID: "$1", Created: "1", PanePID: 42, Raw: r.screen}, nil
		},
		send:  func(_ context.Context, _ string, keys ...string) error { r.keys = append(r.keys, keys...); return nil },
		sleep: func(d time.Duration) { r.clock = r.clock.Add(d) },
		now:   func() time.Time { return r.clock },
	}
	return r
}

// transcript writes a runtime transcript whose last event is at.
func (r *promptRig) transcript(at time.Time, pendingTool bool) {
	r.t.Helper()
	stamp := at.Format(time.RFC3339)
	var dir, path, body string
	if r.b.Runtime == "claude" {
		dir = filepath.Join(os.Getenv("HOME"), ".claude", "projects", "rig")
		path = filepath.Join(dir, r.b.Thread+".jsonl")
		body = `{"type":"user","timestamp":"` + stamp + `","message":{"content":"run touch"}}` + "\n"
		if pendingTool {
			body += `{"type":"assistant","timestamp":"` + stamp + `","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"touch x"}}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
		}
	} else {
		dir = filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "09", "28")
		path = filepath.Join(dir, "rollout-rig-"+r.b.Thread+".jsonl")
		body = `{"type":"event_msg","timestamp":"` + stamp + `","payload":{"type":"task_started"}}` + "\n" + `{"type":"event_msg","timestamp":"` + stamp + `","payload":{"type":"task_complete"}}` + "\n"
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		r.t.Fatal(err)
	}
}

// tick runs one activity tick 16 s after the last and returns the snapshot.
func (r *promptRig) tick() api.AgentActivity {
	r.t.Helper()
	r.clock = r.clock.Add(16 * time.Second)
	ctx := context.WithValue(context.Background(), runtimePromptDepsKey{}, r.deps)
	probe := func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }
	if err := relayActivityTick(ctx, r.b, r.client, r.clock, probe); err != nil {
		r.t.Fatal(err)
	}
	a, err := r.st.GetAgent(context.Background(), r.agent.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	if a.Activity == nil {
		return api.AgentActivity{}
	}
	return *a.Activity
}

func (r *promptRig) fixture(path string) string {
	r.t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", path))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(raw)
}

func (r *promptRig) policy(kind, action string) {
	r.t.Helper()
	p, err := r.st.RuntimePromptPolicy(context.Background(), r.task.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.st.SetRuntimePromptPolicy(context.Background(), r.task.ID, api.RuntimePromptPolicyRequest{ExpectedRevision: p.Revision, Actions: map[string]string{kind: action}}); err != nil {
		r.t.Fatal(err)
	}
	resetRuntimePromptPolicies()
}

func (r *promptRig) leaked(words ...string) {
	r.t.Helper()
	for _, body := range r.bodies {
		for _, w := range words {
			if strings.Contains(body, w) {
				r.t.Fatalf("screen text %q reached the hub: %s", w, body)
			}
		}
	}
}

func TestRuntimePromptActivity(t *testing.T) {
	t.Run("quiet codex menu", func(t *testing.T) {
		r := newPromptRig(t, "codex")
		r.policy(api.RuntimePromptCodexRateLimit, api.RuntimePromptEscalate)
		r.transcript(r.clock.Add(-30*time.Second), false)
		r.screen = r.fixture("runtime-prompt/codex-rate-limit.reconstructed.ansi")
		got := r.tick()
		if got.State != "runtime_prompt" || got.Prompt == nil || got.Prompt.Kind != api.RuntimePromptCodexRateLimit || len(got.Prompt.Fingerprint) != 32 ||
			!got.Prompt.Since.Equal(r.clock) || got.Prompt.Outcome != api.RuntimePromptEscalated || got.Prompt.Label != "Codex rate-limit model menu" {
			t.Fatalf("menu activity %+v prompt %+v", got, got.Prompt)
		}
		since := got.Prompt.Since
		if again := r.tick(); again.Prompt == nil || !again.Prompt.Since.Equal(since) {
			t.Fatalf("steady prompt changed %+v", again.Prompt)
		}
		if len(r.keys) != 0 {
			t.Fatalf("escalate typed %v", r.keys)
		}
		r.screen = r.fixture("runtime-prompt/codex-idle-composer.ansi")
		if cleared := r.tick(); cleared.State != "idle" && cleared.State != "finished_silent" || cleared.Prompt != nil {
			t.Fatalf("cleared prompt %+v", cleared)
		}
		r.leaked("Approaching rate limits", "Keep current model", "GPT-6-Luna", "Ask Codex")
	})
	t.Run("active agent is not inspected", func(t *testing.T) {
		r := newPromptRig(t, "codex")
		r.screen = r.fixture("runtime-prompt/codex-rate-limit.reconstructed.ansi")
		r.transcript(r.clock.Add(10*time.Second), false) // 6 s before the tick
		if got := r.tick(); got.State == "runtime_prompt" || r.inspect != 0 {
			t.Fatalf("active agent inspected %d times: %+v", r.inspect, got)
		}
	})
	t.Run("needs_input claude permission", func(t *testing.T) {
		r := newPromptRig(t, "claude")
		ctx := context.Background()
		owner := api.Caller{Node: "fixture", User: "owner"}
		blocked := "Permission blocked: Claude needs approval"
		if _, err := r.st.PostEvent(ctx, r.task.ID, api.PostEventRequest{AgentID: r.agent.ID, RunID: r.agent.RunID, Kind: api.EventNeedsInput, Text: blocked}, owner); err != nil {
			t.Fatal(err)
		}
		r.transcript(r.clock.Add(-30*time.Second), true)
		r.screen = r.fixture("claude-pane/permission-dialog.ansi")
		got := r.tick()
		if got.State != "runtime_prompt" || got.Prompt == nil || got.Prompt.Kind != api.RuntimePromptClaudePermission || got.Prompt.Outcome != api.RuntimePromptEscalated {
			t.Fatalf("permission activity %+v prompt %+v", got, got.Prompt)
		}
		a, err := r.st.GetAgent(ctx, r.agent.ID)
		if err != nil || a.Status != api.AgentNeedsInput || a.BlockedText != blocked {
			t.Fatalf("status changed: %s %q %v", a.Status, a.BlockedText, err)
		}
		if len(r.keys) != 0 {
			t.Fatalf("permission dialog typed %v", r.keys)
		}
		r.leaked("Do you want to proceed", "touch /private", "Esc to cancel")
	})
}

// menuFrame redraws a plain menu capture with the marker on option at. The
// menu's options are its last numbered rows; numbered transcript rows above
// it are left alone.
func menuFrame(runtime string, lines []string, at int) string {
	m, _ := classifyRuntimePrompt(runtime, lines)
	var rows []int
	for i := len(lines) - 1; i >= 0 && len(rows) < len(m.Options); i-- {
		if runtimePromptOptionRow.MatchString(lines[i]) {
			rows = append([]int{i}, rows...)
		}
	}
	out := append([]string(nil), lines...)
	for n, i := range rows {
		rest := strings.TrimLeft(strings.TrimPrefix(strings.TrimLeft(lines[i], " "), "›"), " ")
		if n == at {
			out[i] = "› " + rest
		} else {
			out[i] = "  " + rest
		}
	}
	return strings.Join(out, "\n")
}

// fakeMenu is a pane that moves its marker on Up and Down and shows after on
// Enter, unless it ignores Enter. It records every key and whether the
// intent was saved before the first one.
type fakeMenu struct {
	lines       []string
	at, options int
	after       string
	ignoreEnter bool
	entered     bool
	intentFirst bool
	sawKey      bool
}

func (r *promptRig) menu(path, after string) *fakeMenu {
	r.t.Helper()
	lines := plainRuntimeScreen(r.fixture(path))
	m, ok := classifyRuntimePrompt(r.b.Runtime, lines)
	if !ok {
		r.t.Fatalf("%s is not a prompt", path)
	}
	menu := &fakeMenu{lines: lines, at: m.selected(), options: len(m.Options), after: r.fixture(after)}
	r.screen = menuFrame(r.b.Runtime, lines, menu.at)
	r.deps.send = func(_ context.Context, _ string, keys ...string) error {
		if !menu.sawKey {
			menu.sawKey = true
			l := loadRuntimePromptLocal(r.b)
			menu.intentFirst = l.Intent != nil && l.Intent.Phase == "uncertain"
		}
		for _, k := range keys {
			r.keys = append(r.keys, k)
			switch k {
			case "Down":
				menu.at = min(menu.at+1, menu.options-1)
			case "Up":
				menu.at = max(menu.at-1, 0)
			case "Enter":
				menu.entered = !menu.ignoreEnter
			}
		}
		if menu.entered {
			r.screen = menu.after
		} else {
			r.screen = menuFrame(r.b.Runtime, lines, menu.at)
		}
		return nil
	}
	return menu
}

func (r *promptRig) ownerNotices() int {
	r.t.Helper()
	var n int
	msgs, err := r.st.ListMessages(context.Background(), r.task.ID, 0, "", 500)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, m := range msgs {
		if m.From.Node == api.BrokerNode && m.To == "" && strings.Contains(m.Text, "waiting on a runtime prompt") {
			n++
		}
	}
	return n
}

func TestRuntimePromptAnswer(t *testing.T) {
	t.Run("rate-limit menu keeps the current model", func(t *testing.T) {
		r := newPromptRig(t, "codex")
		r.transcript(r.clock.Add(-30*time.Second), false)
		menu := r.menu("runtime-prompt/codex-rate-limit.reconstructed.ansi", "runtime-prompt/codex-idle-composer.ansi")
		got := r.tick()
		if strings.Join(r.keys, ",") != "Down,Down,Enter" || !menu.intentFirst || menu.at != 2 {
			t.Fatalf("keys %v intentFirst=%v at=%d", r.keys, menu.intentFirst, menu.at)
		}
		if got.State != "runtime_prompt" || got.Prompt.Action != api.RuntimePromptKeepCurrentNeverShow || got.Prompt.Outcome != api.RuntimePromptConfirmed {
			t.Fatalf("answer activity %+v %+v", got, got.Prompt)
		}
		l := loadRuntimePromptLocal(r.b)
		if l.Intent == nil || l.Intent.Phase != api.RuntimePromptConfirmed || l.Intent.Target != "Keep current model (never show again)" {
			t.Fatalf("intent %+v", l.Intent)
		}
		if next := r.tick(); next.State != "idle" && next.State != "finished_silent" {
			t.Fatalf("after answer %+v", next)
		}
		if r.ownerNotices() != 0 {
			t.Fatal("confirmed answer notified the owner")
		}
		r.leaked("Keep current model", "Approaching rate limits")
	})
	t.Run("model migration uses the existing model", func(t *testing.T) {
		r := newPromptRig(t, "codex")
		r.transcript(r.clock.Add(-30*time.Second), false)
		r.menu("runtime-prompt/codex-model-migration.ansi", "runtime-prompt/codex-idle-composer.ansi")
		if got := r.tick(); strings.Join(r.keys, ",") != "Down,Enter" || got.Prompt == nil || got.Prompt.Outcome != api.RuntimePromptConfirmed || got.Prompt.Action != api.RuntimePromptUseExisting {
			t.Fatalf("keys %v activity %+v", r.keys, got.Prompt)
		}
	})
	t.Run("marker checked before Enter", func(t *testing.T) {
		r := newPromptRig(t, "codex")
		r.transcript(r.clock.Add(-30*time.Second), false)
		menu := r.menu("runtime-prompt/codex-rate-limit.reconstructed.ansi", "runtime-prompt/codex-idle-composer.ansi")
		menu.options = 2 // the second Down does not move: the marker stops short
		got := r.tick()
		if strings.Join(r.keys, ",") != "Down,Down" || got.Prompt.Outcome != api.RuntimePromptFailed || r.ownerNotices() != 1 {
			t.Fatalf("keys %v prompt %+v notices %d", r.keys, got.Prompt, r.ownerNotices())
		}
	})
	t.Run("prompt still shown is ambiguous and never resent", func(t *testing.T) {
		r := newPromptRig(t, "codex")
		r.transcript(r.clock.Add(-30*time.Second), false)
		menu := r.menu("runtime-prompt/codex-rate-limit.reconstructed.ansi", "runtime-prompt/codex-idle-composer.ansi")
		menu.ignoreEnter = true
		got := r.tick()
		if got.Prompt.Outcome != api.RuntimePromptAmbiguous || strings.Join(r.keys, ",") != "Down,Down,Enter" {
			t.Fatalf("keys %v prompt %+v", r.keys, got.Prompt)
		}
		for i := 0; i < 3; i++ {
			r.tick()
		}
		// A new relay process has no policy cache; it must still not resend.
		resetRuntimePromptPolicies()
		r.tick()
		if len(r.keys) != 3 || r.ownerNotices() != 1 {
			t.Fatalf("resent keys %v notices %d", r.keys, r.ownerNotices())
		}
	})
	zero := func(t *testing.T, name string, setup func(*promptRig)) {
		t.Run(name, func(t *testing.T) {
			r := newPromptRig(t, "codex")
			r.transcript(r.clock.Add(-30*time.Second), false)
			r.menu("runtime-prompt/codex-rate-limit.reconstructed.ansi", "runtime-prompt/codex-idle-composer.ansi")
			setup(r)
			r.tick()
			r.tick()
			if len(r.keys) != 0 {
				t.Fatalf("%s sent %v", name, r.keys)
			}
		})
	}
	zero(t, "paused project", func(r *promptRig) {
		r.override = func(w http.ResponseWriter, req *http.Request) bool {
			if req.Method == "GET" && strings.HasSuffix(req.URL.Path, "/pause") {
				_ = json.NewEncoder(w).Encode(api.ProjectPauseStatus{TaskID: r.task.ID, State: api.ProjectPausePaused})
				return true
			}
			return false
		}
	})
	zero(t, "retired agent", func(r *promptRig) {
		r.override = func(w http.ResponseWriter, req *http.Request) bool {
			if req.Method == "GET" && strings.HasSuffix(req.URL.Path, "/agents/"+r.agent.ID) {
				a := r.agent
				a.Status, a.Online = api.AgentRetired, true
				_ = json.NewEncoder(w).Encode(a)
				return true
			}
			return false
		}
	})
	zero(t, "identity change between inspections", func(r *promptRig) {
		inspect := r.deps.inspect
		n := 0
		r.deps.inspect = func(ctx context.Context, b runtimeBinding) (runtimePane, error) {
			p, err := inspect(ctx, b)
			n++
			p.PanePID = 42 + n
			return p, err
		}
	})
	zero(t, "escalate action", func(r *promptRig) { r.policy(api.RuntimePromptCodexRateLimit, api.RuntimePromptEscalate) })
	t.Run("report action", func(t *testing.T) {
		r := newPromptRig(t, "codex")
		r.transcript(r.clock.Add(-30*time.Second), false)
		r.policy(api.RuntimePromptCodexTrust, api.RuntimePromptReport)
		r.menu("runtime-prompt/codex-trust.ansi", "runtime-prompt/codex-idle-composer.ansi")
		got := r.tick()
		if len(r.keys) != 0 || got.Prompt.Outcome != api.RuntimePromptReported || r.ownerNotices() != 0 {
			t.Fatalf("report keys %v prompt %+v notices %d", r.keys, got.Prompt, r.ownerNotices())
		}
	})
	t.Run("fourth answer inside five minutes", func(t *testing.T) {
		r := newPromptRig(t, "codex")
		r.transcript(r.clock.Add(-30*time.Second), false)
		l := loadRuntimePromptLocal(r.b)
		for i := 1; i <= 3; i++ {
			l.Answers = append(l.Answers, r.clock.Add(time.Duration(i)*time.Minute/2))
		}
		if err := writePrivateJSON(runtimePromptPath(r.b), l); err != nil {
			t.Fatal(err)
		}
		r.menu("runtime-prompt/codex-rate-limit.reconstructed.ansi", "runtime-prompt/codex-idle-composer.ansi")
		got := r.tick()
		if len(r.keys) != 0 || got.Prompt.Outcome != api.RuntimePromptSkipped || !strings.Contains(got.Prompt.Reason, "answer limit") {
			t.Fatalf("fourth answer keys %v prompt %+v", r.keys, got.Prompt)
		}
	})
}

// TestRuntimePromptTmuxReplay drives the native pane path on a private tmux
// socket: exact pane identity, process discovery, capture-pane and send-keys.
// The pane runs testdata/runtime-prompt/replay built under the runtime's
// name, which draws a fixture and logs every key it receives (plan a6).
func TestRuntimePromptTmuxReplay(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	for _, tc := range []struct {
		runtime, fixture, after, keys, outcome string
	}{
		{"codex", "runtime-prompt/codex-rate-limit.reconstructed.ansi", "runtime-prompt/codex-idle-composer.ansi", "Down,Down,Enter", api.RuntimePromptConfirmed},
		{"claude", "claude-pane/permission-dialog.ansi", "claude-pane/suggestion.ansi", "", api.RuntimePromptEscalated},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			r := newPromptRig(t, tc.runtime)
			t.Setenv("TT_TMUX_SOCKET", "tt-rtprompt-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
			bin := t.TempDir()
			if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, tc.runtime), "./testdata/runtime-prompt/replay").CombinedOutput(); err != nil {
				t.Fatalf("build replay: %v %s", err, out)
			}
			frames := t.TempDir()
			lines := plainRuntimeScreen(r.fixture(tc.fixture))
			m, ok := classifyRuntimePrompt(tc.runtime, lines)
			if !ok {
				t.Fatal("fixture is not a prompt")
			}
			for i := range max(1, len(m.Options)) {
				frame := menuFrame(tc.runtime, lines, i)
				if tc.runtime == "claude" {
					frame = strings.Join(lines, "\n") // drawn as captured
				}
				if err := os.WriteFile(filepath.Join(frames, fmt.Sprintf("frame-%d.txt", i)), []byte(frame), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(frames, "after.txt"), []byte(strings.Join(plainRuntimeScreen(r.fixture(tc.after)), "\n")), 0600); err != nil {
				t.Fatal(err)
			}
			keyLog := filepath.Join(frames, "keys.log")
			session := "rtprompt-" + strings.TrimPrefix(api.NewID("agt"), "agt_")
			tmux := func(args ...string) error {
				c := exec.Command("tmux", append([]string{"-L", os.Getenv("TT_TMUX_SOCKET"), "-f", "/dev/null"}, args...)...)
				out, err := c.CombinedOutput()
				if err != nil {
					return fmt.Errorf("tmux %v: %w: %s", args, err, out)
				}
				return nil
			}
			command := fmt.Sprintf("%s %s %s %d", filepath.Join(bin, tc.runtime), frames, keyLog, max(0, m.selected()))
			if err := tmux("new-session", "-d", "-x", "100", "-y", "30", "-s", session, command); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tmux("kill-server") })
			r.b.Session = session
			for key, value := range map[string]string{"TAILTERM_HUB": r.b.Hub, "TAILTERM_TASK": r.b.Task, "TAILTERM_AGENT": r.b.Agent, "TAILTERM_RUN": r.b.Run} {
				if err := tmux("set-environment", "-t", session, key, value); err != nil {
					t.Fatal(err)
				}
			}
			r.deps = nativeRuntimePromptDeps()
			r.transcript(r.clock.Add(-30*time.Second), tc.runtime == "claude")
			var got api.AgentActivity
			for deadline := time.Now().Add(5 * time.Second); ; {
				got = r.tick()
				if got.State == "runtime_prompt" || time.Now().After(deadline) {
					break
				}
				time.Sleep(200 * time.Millisecond) // the pane is still drawing
			}
			if got.Prompt == nil || got.Prompt.Outcome != tc.outcome {
				t.Fatalf("activity %+v prompt %+v", got, got.Prompt)
			}
			time.Sleep(300 * time.Millisecond)
			logged, _ := os.ReadFile(keyLog)
			if keys := strings.Join(strings.Fields(string(logged)), ","); keys != tc.keys {
				t.Fatalf("pane received %q, want %q", keys, tc.keys)
			}
		})
	}
}

func TestRuntimePromptLog(t *testing.T) {
	r := newPromptRig(t, "codex")
	r.policy(api.RuntimePromptCodexRateLimit, api.RuntimePromptEscalate)
	r.transcript(r.clock.Add(-30*time.Second), false)
	r.screen = r.fixture("runtime-prompt/codex-rate-limit.reconstructed.ansi")
	logged, err := captureRelayOutput(t, true, func() error {
		for i := 0; i < 6; i++ { // one change, then five steady ticks
			r.tick()
		}
		return nil
	})
	if err != nil || strings.Count(logged, "\n") != 1 || !strings.Contains(logged, "runtime prompt codex_rate_limit_switch: escalate escalated") || !strings.HasPrefix(logged, "[tt relay] ") {
		t.Fatalf("steady log %q %v", logged, err)
	}
	if err := writePrivateJSON(filepath.Join(relayDir(), bindingKey(r.b)+".binding.json"), r.b); err != nil {
		t.Fatal(err)
	}
	status, err := captureRelayOutput(t, false, func() error { return cmdRelay([]string{"--status"}) })
	if err != nil || !strings.Contains(status, "prompt=codex_rate_limit_switch action=escalate outcome=escalated prompt-at=") {
		t.Fatalf("status %q %v", status, err)
	}
	r.screen = r.fixture("runtime-prompt/codex-idle-composer.ansi")
	logged, _ = captureRelayOutput(t, true, func() error { r.tick(); r.tick(); return nil })
	if strings.Count(logged, "\n") != 1 || !strings.Contains(logged, "runtime prompt cleared") {
		t.Fatalf("clear log %q", logged)
	}
	r.screen = r.fixture("runtime-prompt/codex-update.ansi")
	logged, _ = captureRelayOutput(t, true, func() error { r.tick(); r.tick(); return nil })
	if strings.Count(logged, "\n") != 1 || !strings.Contains(logged, "runtime prompt unknown: escalate escalated") {
		t.Fatalf("new prompt log %q", logged)
	}
	r.leaked("Update available", "Skip until next version")
}

func TestAgentsRuntimePrompt(t *testing.T) {
	r := newPromptRig(t, "codex")
	r.policy(api.RuntimePromptCodexRateLimit, api.RuntimePromptEscalate)
	r.transcript(r.clock.Add(-30*time.Second), false)
	r.screen = r.fixture("runtime-prompt/codex-rate-limit.reconstructed.ansi")
	r.tick()
	out, err := captureRelayOutput(t, false, func() error { return cmdAgents(env{hub: r.b.Hub, task: r.task.ID}, nil) })
	if err != nil || !strings.Contains(out, "activity=runtime_prompt") ||
		!strings.Contains(out, "  runtime-prompt kind=codex_rate_limit_switch action=escalate outcome=escalated since=2026-09-28T20:00:16Z") {
		t.Fatalf("agents %q %v", out, err)
	}
}
