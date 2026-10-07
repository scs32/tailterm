package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	info, _ := os.Stat(path)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) + " mtime=" + info.ModTime().UTC().Format(time.RFC3339Nano)
}

// TestRuntimePromptLiveClaudePermission runs a real Claude Code session on a
// private tmux socket against an isolated SQLite/HTTP hub (plan a12). Claude
// runs with --permission-mode default for this process only and is asked to
// run one harmless Bash command in a disposable folder. The relay's activity
// tick, with the native pane inspection and send-keys, must report
// runtime_prompt/claude_permission, post exactly one owner notice and send no
// key: the dialog stays and the command never runs. Run explicitly with
// TT_LIVE_RUNTIME_PROMPT=1; it never touches a working agent, the live hub
// or user settings.
func TestRuntimePromptLiveClaudePermission(t *testing.T) {
	if os.Getenv("TT_LIVE_RUNTIME_PROMPT") != "1" {
		t.Skip("requires an explicit live runtime prompt check")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	before := fileDigest(t, settings)
	t.Logf("claude version: %s", strings.TrimSpace(func() string { out, _ := exec.Command(claude, "--version").Output(); return string(out) }()))
	t.Logf("~/.claude/settings.json before: %s", before)
	state := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(state, "relay"))
	t.Setenv("TT_TMUX_SOCKET", "tt-rtprompt-live-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
	t.Setenv("TAILTERM_ACTIVITY_PROMPT_SECONDS", "5")
	resetRuntimePromptPolicies()
	st, err := store.Open(filepath.Join(state, "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hub := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "owner"}, nil }))
	defer hub.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	ctx := context.Background()
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Runtime prompt live fixture"}, by)
	if err != nil {
		t.Fatal(err)
	}
	// The worktree root is a folder Claude already trusts; the command's
	// target is a disposable folder outside it.
	cwd, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "runtime-prompt-live-marker")
	session := "rtprompt-live-" + strings.TrimPrefix(api.NewID("agt"), "agt_")
	agent, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "claude-live", Host: "mini", Session: session, Runtime: "claude", Cwd: cwd}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: agent.ID, RunID: agent.RunID, Kind: api.EventRunning}, by); err != nil {
		t.Fatal(err)
	}
	thread := "00000000-0000-4000-8000-" + strings.TrimPrefix(api.NewID("agt"), "agt_")[:12]
	b := runtimeBinding{Hub: hub.URL, Task: task.ID, Agent: agent.ID, Run: agent.RunID, Thread: thread, Runtime: "claude", Session: session, Cwd: cwd}
	tmux := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", append([]string{"-L", os.Getenv("TT_TMUX_SOCKET")}, args...)...).CombinedOutput()
		return string(out), err
	}
	start := exec.Command("tmux", "-L", os.Getenv("TT_TMUX_SOCKET"), "-f", "/dev/null", "new-session", "-d", "-x", "100", "-y", "30", "-s", session, "-c", cwd,
		claude+" --session-id "+thread+" --permission-mode default")
	start.Env = append(os.Environ(), "TAILTERM_HUB="+b.Hub, "TAILTERM_TASK="+b.Task, "TAILTERM_AGENT="+b.Agent, "TAILTERM_RUN="+b.Run, "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=0")
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("private tmux start: %v %s", err, out)
	}
	defer func() {
		out, err := tmux("kill-server")
		t.Logf("private tmux cleanup socket=%s: %v %s", os.Getenv("TT_TMUX_SOCKET"), err, strings.TrimSpace(out))
		after := fileDigest(t, settings)
		t.Logf("~/.claude/settings.json after: %s", after)
		if after != before {
			t.Error("user settings changed")
		}
	}()
	for key, value := range map[string]string{"TAILTERM_HUB": b.Hub, "TAILTERM_TASK": b.Task, "TAILTERM_AGENT": b.Agent, "TAILTERM_RUN": b.Run} {
		if out, err := tmux("set-environment", "-t", session, key, value); err != nil {
			t.Fatalf("session identity: %v %s", err, out)
		}
	}
	capture := func() string { out, _ := tmux("capture-pane", "-p", "-t", session); return out }
	waitFor := func(what string, limit time.Duration, ok func(string) bool) {
		t.Helper()
		var screen string
		for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
			if screen = capture(); ok(screen) {
				return
			}
		}
		t.Fatalf("%s did not appear: %q", what, screen)
	}
	// The default-permission footer ("⏸ manual mode on") is not on the wake
	// check's idle allowlist, so readiness is an input box with no dialog.
	waitFor("idle Claude input box", 45*time.Second, func(screen string) bool {
		lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
		_, boxed := claudePromptArea(lines)
		_, prompt := classifyRuntimePrompt("claude", lines)
		return boxed && !prompt
	})
	ask := "Use the Bash tool to run exactly this command and nothing else: touch " + target
	if out, err := tmux("send-keys", "-t", session, "-l", "--", ask); err != nil {
		t.Fatalf("prompt text: %v %s", err, out)
	}
	time.Sleep(200 * time.Millisecond)
	if out, err := tmux("send-keys", "-t", session, "Enter"); err != nil {
		t.Fatalf("prompt Enter: %v %s", err, out)
	}
	waitFor("permission dialog", 120*time.Second, func(screen string) bool {
		m, ok := classifyRuntimePrompt("claude", strings.Split(screen, "\n"))
		return ok && m.Kind == api.RuntimePromptClaudePermission
	})
	t.Log("permission dialog is up")
	deps := nativeRuntimePromptDeps()
	send := deps.send
	keys := 0
	deps.send = func(ctx context.Context, pane string, k ...string) error {
		keys += len(k)
		return send(ctx, pane, k...)
	}
	c, _ := api.NewClient(hub.URL, 5*time.Second)
	var got api.AgentActivity
	offset := time.Duration(0)
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		offset += 16 * time.Second // keeps the tick's 15-second spacing without waiting for it
		tickCtx := context.WithValue(context.Background(), runtimePromptDepsKey{}, deps)
		if err := relayActivityTick(tickCtx, b, c, time.Now().Add(offset), activityProbeNative); err != nil {
			t.Logf("tick: %v", err)
		}
		a, err := st.GetAgent(ctx, agent.ID)
		if err == nil && a.Activity != nil {
			got = *a.Activity
			t.Logf("activity state=%s reason=%q", got.State, got.Reason)
			if got.State == "runtime_prompt" {
				break
			}
		}
	}
	if got.State != "runtime_prompt" || got.Prompt == nil || got.Prompt.Kind != api.RuntimePromptClaudePermission || got.Prompt.Outcome != api.RuntimePromptEscalated {
		t.Fatalf("live activity %+v prompt %+v", got, got.Prompt)
	}
	t.Logf("reported runtime_prompt kind=%s action=%s outcome=%s fingerprint=%s", got.Prompt.Kind, got.Prompt.Action, got.Prompt.Outcome, got.Prompt.Fingerprint)
	for i := 0; i < 3; i++ { // steady ticks post nothing more
		offset += 16 * time.Second
		tickCtx := context.WithValue(context.Background(), runtimePromptDepsKey{}, deps)
		_ = relayActivityTick(tickCtx, b, c, time.Now().Add(offset), activityProbeNative)
	}
	msgs, err := st.ListMessages(ctx, task.ID, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	notices := 0
	for _, m := range msgs {
		if m.From.Node == api.BrokerNode && m.To == "" && strings.Contains(m.Text, "waiting on a runtime prompt") {
			notices++
			t.Logf("owner notice #%d: %s", m.Seq, m.Text)
		}
	}
	if notices != 1 || keys != 0 {
		t.Fatalf("owner notices %d, keys sent %d", notices, keys)
	}
	if m, ok := classifyRuntimePrompt("claude", strings.Split(capture(), "\n")); !ok || m.Kind != api.RuntimePromptClaudePermission {
		t.Fatal("permission dialog did not stay up")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the command ran: %v", err)
	}
	t.Log("dialog still up, zero keys sent, command did not run")
}

// TestRuntimePromptLiveCodexMigration is the Codex leg of plan a12. Real
// Codex runs on a private tmux socket with a disposable CODEX_HOME holding a
// fake API key (no model request is made) and an older model, so it shows
// its model-migration menu at start. The relay must answer it under the
// default policy, use_existing: Down, a verified marker, Enter, confirmed,
// and the disposable config keeps the older model. The user's ~/.codex is
// never used.
func TestRuntimePromptLiveCodexMigration(t *testing.T) {
	if os.Getenv("TT_LIVE_RUNTIME_PROMPT") != "1" {
		t.Skip("requires an explicit live runtime prompt check")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	ownerConfig := filepath.Join(home, ".codex", "config.toml")
	before := fileDigest(t, ownerConfig)
	t.Logf("codex version: %s", strings.TrimSpace(func() string { out, _ := exec.Command(codex, "--version").Output(); return string(out) }()))
	t.Logf("~/.codex/config.toml before: %s", before)
	state := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(state, "relay"))
	t.Setenv("TT_TMUX_SOCKET", "tt-rtprompt-live-"+strings.TrimPrefix(api.NewID("agt"), "agt_"))
	t.Setenv("TAILTERM_ACTIVITY_PROMPT_SECONDS", "5")
	resetRuntimePromptPolicies()
	codexHome, cwd := t.TempDir(), t.TempDir()
	cwd, _ = filepath.EvalSymlinks(cwd)
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"OPENAI_API_KEY":"sk-fake-fixture-not-a-key"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := "model = \"gpt-5.5\"\ncheck_for_update_on_startup = false\n\n[projects.\"" + cwd + "\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(state, "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hub := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "owner"}, nil }))
	defer hub.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	ctx := context.Background()
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "Runtime prompt live Codex fixture"}, by)
	if err != nil {
		t.Fatal(err)
	}
	session := "rtprompt-live-" + strings.TrimPrefix(api.NewID("agt"), "agt_")
	agent, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "codex-live", Host: "mini", Session: session, Runtime: "codex", Cwd: cwd}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PostEvent(ctx, task.ID, api.PostEventRequest{AgentID: agent.ID, RunID: agent.RunID, Kind: api.EventRunning}, by); err != nil {
		t.Fatal(err)
	}
	thread := "00000000-0000-4000-8000-" + strings.TrimPrefix(api.NewID("agt"), "agt_")[:12]
	b := runtimeBinding{Hub: hub.URL, Task: task.ID, Agent: agent.ID, Run: agent.RunID, Thread: thread, Runtime: "codex", Codex: codex, CodexHome: codexHome, Session: session, Cwd: cwd, CreatedAt: time.Now().UTC()}
	tmux := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", append([]string{"-L", os.Getenv("TT_TMUX_SOCKET")}, args...)...).CombinedOutput()
		return string(out), err
	}
	start := exec.Command("tmux", "-L", os.Getenv("TT_TMUX_SOCKET"), "-f", "/dev/null", "new-session", "-d", "-x", "100", "-y", "30", "-s", session, "-c", cwd, codex)
	start.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "USER=" + os.Getenv("USER"), "LANG=en_US.UTF-8", "TERM=xterm-256color", "CODEX_HOME=" + codexHome,
		"TMUX_TMPDIR=" + os.Getenv("TMUX_TMPDIR"),
		"TAILTERM_HUB=" + b.Hub, "TAILTERM_TASK=" + b.Task, "TAILTERM_AGENT=" + b.Agent, "TAILTERM_RUN=" + b.Run}
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("private tmux start: %v %s", err, out)
	}
	defer func() {
		out, err := tmux("kill-server")
		t.Logf("private tmux cleanup socket=%s: %v %s", os.Getenv("TT_TMUX_SOCKET"), err, strings.TrimSpace(out))
		after := fileDigest(t, ownerConfig)
		t.Logf("~/.codex/config.toml after: %s", after)
		if after != before {
			t.Error("user Codex config changed")
		}
	}()
	for key, value := range map[string]string{"TAILTERM_HUB": b.Hub, "TAILTERM_TASK": b.Task, "TAILTERM_AGENT": b.Agent, "TAILTERM_RUN": b.Run} {
		if out, err := tmux("set-environment", "-t", session, key, value); err != nil {
			t.Fatalf("session identity: %v %s", err, out)
		}
	}
	capture := func() string { out, _ := tmux("capture-pane", "-p", "-t", session); return out }
	var screen string
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		screen = capture()
		if m, ok := classifyRuntimePrompt("codex", strings.Split(screen, "\n")); ok && m.Kind == api.RuntimePromptCodexModelMigration {
			break
		}
	}
	if m, ok := classifyRuntimePrompt("codex", strings.Split(screen, "\n")); !ok || m.Kind != api.RuntimePromptCodexModelMigration {
		t.Fatalf("migration menu did not appear: %q", screen)
	}
	t.Log("model migration menu is up")
	deps := nativeRuntimePromptDeps()
	send := deps.send
	var keys []string
	deps.send = func(ctx context.Context, pane string, k ...string) error {
		keys = append(keys, k...)
		return send(ctx, pane, k...)
	}
	c, _ := api.NewClient(hub.URL, 5*time.Second)
	var got api.AgentActivity
	offset := time.Duration(0)
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		offset += 16 * time.Second
		tickCtx := context.WithValue(context.Background(), runtimePromptDepsKey{}, deps)
		if err := relayActivityTick(tickCtx, b, c, time.Now().Add(offset), activityProbeNative); err != nil {
			t.Logf("tick: %v", err)
		}
		if a, err := st.GetAgent(ctx, agent.ID); err == nil && a.Activity != nil {
			got = *a.Activity
			t.Logf("activity state=%s reason=%q", got.State, got.Reason)
			if got.State == "runtime_prompt" {
				break
			}
		}
	}
	if got.Prompt == nil || got.Prompt.Kind != api.RuntimePromptCodexModelMigration || got.Prompt.Action != api.RuntimePromptUseExisting || got.Prompt.Outcome != api.RuntimePromptConfirmed {
		t.Fatalf("live activity %+v prompt %+v", got, got.Prompt)
	}
	t.Logf("reported runtime_prompt kind=%s action=%s outcome=%s reason=%q; keys sent %v", got.Prompt.Kind, got.Prompt.Action, got.Prompt.Outcome, got.Prompt.Reason, keys)
	if strings.Join(keys, ",") != "Down,Enter" {
		t.Fatalf("keys %v", keys)
	}
	time.Sleep(time.Second)
	after := capture()
	if _, ok := classifyRuntimePrompt("codex", strings.Split(after, "\n")); ok || !strings.Contains(after, "GPT-5.5") {
		t.Fatalf("pane after answer: %q", after)
	}
	written, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if !strings.Contains(string(written), `model = "gpt-5.5"`) {
		t.Fatalf("disposable config lost the model: %s", written)
	}
	t.Logf("pane shows the idle composer on GPT-5.5; disposable config:\n%s", written)
}
