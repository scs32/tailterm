package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func testClaudeBinding() runtimeBinding {
	return runtimeBinding{Hub: "http://test.invalid", Task: "tsk_0000000000000001", Agent: "agt_0000000000000001", Run: "run_0000000000000001", Thread: "00000000-0000-0000-0000-000000000001", Runtime: "claude", Session: "test-claude"}
}

func TestClaudeInputAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name, screen string
		want         bool
	}{
		{"idle", "Some completed answer\n❯ \n? for shortcuts\n", true},
		{"placeholder", "❯ Try \"refactor server.go\"\n────────────────\n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n", true},
		{"empty", "Some completed answer\n", false},
		{"typing", "❯ pending text\n? for shortcuts\n", false},
		{"multiline", "❯ \ncontinuation\n? for shortcuts\n", false},
		{"permission", "Allow this tool?\n❯ \n", false},
		{"selection", "Select an option\n❯ \n", false},
		{"menu", "❯ \n1. Continue\n", false},
		{"two prompts", "❯ \n❯ \n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := emptyClaudeInput(tc.screen); got != tc.want {
				t.Fatalf("emptyClaudeInput = %v, want %v", got, tc.want)
			}
		})
	}
	prompt := "Tailterm messages #7. Run tt inbox."
	if !exactClaudeInput("❯ "+prompt+"\n────────────────\n  ⏵⏵ bypass permissions on\n", prompt) {
		t.Fatal("exact submitted text was not recognized")
	}
	if exactClaudeInput("❯ "+prompt+"\nold answer\n❯ different pending text\n", prompt) {
		t.Fatal("old matching prompt hid current pending text")
	}
}

func TestClaudeWrappedBrokerAndInboxInput(t *testing.T) {
	broker := claudeBrokerPrompt("Tailterm obligations #13260", 13260, "wake_0123456789abcdef")
	// The first screen is the editor region from a real 80x24 Claude pane.
	actual80 := "❯ Tailterm obligations #13260. Run tt inbox --unread --mark-read. Wake          \n  wake_0123456789abcdef.\n────────────────────────────────────────────────────────────────────────────────\n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"
	if !exactClaudeInput(actual80, broker) {
		t.Fatal("real 80-column broker editor was not recognized")
	}
	maxInbox := "Tailterm messages #13260,#13261,#13262,#13263,#13264. Run tt inbox --unread --mark-read."
	for _, tc := range []struct {
		name, prompt string
		width        int
	}{
		{"broker-40", broker, 40},
		{"inbox-80", maxInbox, 80},
		{"inbox-40", maxInbox, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A narrow editor word-wraps into indented continuation lines.
			var lines []string
			line := "❯ "
			for _, word := range strings.Fields(tc.prompt) {
				if len([]rune(line))+len([]rune(word))+1 > tc.width && line != "❯ " {
					lines = append(lines, line)
					line = "  " + word
				} else {
					if line != "❯ " && line != "  " {
						line += " "
					}
					line += word
				}
			}
			lines = append(lines, line, strings.Repeat("─", tc.width), "  ⏵⏵ bypass permissions on")
			if !exactClaudeInput(strings.Join(lines, "\n"), tc.prompt) {
				t.Fatalf("wrapped %d-column input was not recognized: %q", tc.width, lines)
			}
		})
	}
}

func TestClaudeWakeConfirmationAndNoResend(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	b := testClaudeBinding()
	transcript := filepath.Join(t.TempDir(), b.Thread+".jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"assistant","message":{"stop_reason":"end_turn","content":[]}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	var sent []string
	var snapshot claudeWakeSnapshot
	prompt := "Tailterm messages #12. Run tt inbox --unread --mark-read."
	inspect := func(_ context.Context, _ runtimeBinding, expected string) (claudeWakeSnapshot, error) {
		info, err := os.Stat(transcript)
		if err != nil {
			return claudeWakeSnapshot{}, err
		}
		snapshot = claudeWakeSnapshot{Pane: "%1", SessionID: "$1", Created: "100", PanePID: 1001, Path: transcript, FileID: fileIdentity(info), Offset: info.Size(), Cursor: activityCursor{Ready: true, SeenTurn: true, TurnComplete: true}}
		if expected == "" {
			snapshot.Screen = "Answer\n❯ \n? for shortcuts\n"
		} else {
			snapshot.Screen = "❯ " + expected + "\n"
		}
		return snapshot, nil
	}
	ops := claudeWakeOps{inspect: inspect, send: func(_ context.Context, pane, value string, literal bool) error {
		if pane != "%1" {
			t.Fatalf("wrong pane %q", pane)
		}
		if literal {
			sent = append(sent, "text:"+value)
		} else {
			sent = append(sent, "Enter")
			line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": prompt}})
			f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = f.Write(append(line, '\n'))
			f.Close()
			return err
		}
		return nil
	}, sleep: func(d time.Duration) { clock = clock.Add(d) }, now: func() time.Time { return clock }}
	if err := claudeWakeWith(context.Background(), b, prompt, ops); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[0] != "text:"+prompt || sent[1] != "Enter" {
		t.Fatalf("send order %v", sent)
	}
	if err := claudeWakeWith(context.Background(), b, prompt, ops); err != nil || len(sent) != 2 {
		t.Fatalf("confirmed retry retyped: %v %v", sent, err)
	}
	var saved claudeWakeIntent
	data, _ := os.ReadFile(claudeWakePath(b))
	if json.Unmarshal(data, &saved) != nil || saved.Phase != "confirmed" || saved.Nonce == "" || saved.Pane != "%1" || saved.EnterAt.Sub(saved.TextAt) < 100*time.Millisecond || saved.ConfirmedAt.Before(saved.EnterAt) {
		t.Fatalf("guard was not confirmed: %s", data)
	}
}

func TestClaudeWakeTimeoutAndRestartGuard(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	b := testClaudeBinding()
	transcript := filepath.Join(t.TempDir(), "claude.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"assistant","message":{"stop_reason":"end_turn"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sends := 0
	ops := claudeWakeOps{inspect: func(_ context.Context, _ runtimeBinding, expected string) (claudeWakeSnapshot, error) {
		info, _ := os.Stat(transcript)
		screen := "❯ \n"
		if expected != "" {
			screen = "❯ " + expected + "\n"
		}
		return claudeWakeSnapshot{Pane: "%1", SessionID: "$1", Created: "100", PanePID: 1001, Path: transcript, FileID: fileIdentity(info), Offset: info.Size(), Screen: screen, Cursor: activityCursor{Ready: true, SeenTurn: true, TurnComplete: true}}, nil
	}, send: func(context.Context, string, string, bool) error { sends++; return nil }, sleep: func(d time.Duration) { now = now.Add(d) }, now: func() time.Time { return now }}
	prompt := "Tailterm obligations #41. Run tt inbox --unread --mark-read."
	if err := claudeWakeWith(context.Background(), b, prompt, ops); err == nil || !strings.Contains(err.Error(), "did not confirm") || sends != 2 {
		t.Fatalf("timeout: sends=%d err=%v", sends, err)
	}
	if err := claudeWakeWith(context.Background(), b, prompt, ops); err == nil || sends != 2 {
		t.Fatalf("uncertain retry typed: sends=%d err=%v", sends, err)
	}
}

func TestClaudeWakePartialSendNeverRetypes(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	b := testClaudeBinding()
	path := filepath.Join(t.TempDir(), "claude.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"assistant","message":{"stop_reason":"end_turn"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sends := 0
	ops := claudeWakeOps{inspect: func(_ context.Context, _ runtimeBinding, expected string) (claudeWakeSnapshot, error) {
		info, _ := os.Stat(path)
		screen := "❯ \n"
		if expected != "" {
			screen = "❯ " + expected + "\n"
		}
		return claudeWakeSnapshot{Pane: "%1", SessionID: "$1", Created: "100", PanePID: 1001, Path: path, FileID: fileIdentity(info), Offset: info.Size(), Screen: screen, Cursor: activityCursor{Ready: true, SeenTurn: true, TurnComplete: true}}, nil
	}, send: func(_ context.Context, _ string, _ string, literal bool) error {
		sends++
		if !literal {
			return errors.New("synthetic Enter failure")
		}
		return nil
	}, sleep: func(time.Duration) {}, now: time.Now}
	prompt := "Tailterm #42. Run tt inbox --unread --mark-read."
	if err := claudeWakeWith(context.Background(), b, prompt, ops); err == nil || !strings.Contains(err.Error(), "did not confirm") || sends != 2 {
		t.Fatalf("partial send %d %v", sends, err)
	}
	if err := claudeWakeWith(context.Background(), b, prompt, ops); err == nil || sends != 2 {
		t.Fatalf("ambiguous retry %d %v", sends, err)
	}
}

func TestClaudeWakeIdentityRaceBeforeInput(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	b := testClaudeBinding()
	count := 0
	ops := claudeWakeOps{inspect: func(_ context.Context, _ runtimeBinding, _ string) (claudeWakeSnapshot, error) {
		count++
		pane := "%1"
		if count == 2 {
			pane = "%2"
		}
		return claudeWakeSnapshot{Pane: pane, Screen: "❯ \n", Cursor: activityCursor{Ready: true, TurnComplete: true}}, nil
	}, send: func(context.Context, string, string, bool) error { t.Fatal("unsafe send"); return nil }, now: time.Now, sleep: func(time.Duration) {}}
	if err := claudeWakeWith(context.Background(), b, "Tailterm #1 tt inbox", ops); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("identity race %v", err)
	}
}

func TestClaudeConfirmationRequiresNewCompleteUserText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	prompt := "Tailterm obligations #7. Run tt inbox --unread --mark-read."
	old, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": prompt}})
	if err := os.WriteFile(path, append(old, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	intent := claudeWakeIntent{Path: path, FileID: fileIdentity(info), Offset: info.Size(), Prompt: prompt}
	appendRecord := func(line []byte) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(line); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	for _, record := range []map[string]any{
		{"type": "assistant", "message": map[string]any{"content": []map[string]string{{"type": "text", "text": prompt}}}},
		{"type": "user", "message": map[string]any{"content": []map[string]string{{"type": "tool_result", "text": prompt}}}},
	} {
		line, _ := json.Marshal(record)
		appendRecord(append(line, '\n'))
	}
	if ok, err := claudeWakeConfirmed(intent); ok || err != nil {
		t.Fatalf("old, echo or tool result confirmed: %v %v", ok, err)
	}
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": prompt}})
	appendRecord(line)
	if ok, err := claudeWakeConfirmed(intent); ok || err != nil {
		t.Fatalf("partial tail confirmed: %v %v", ok, err)
	}
	appendRecord([]byte{'\n'})
	if ok, err := claudeWakeConfirmed(intent); !ok || err != nil {
		t.Fatalf("new user turn missed: %v %v", ok, err)
	}
}

func TestClaudeFakeTmuxRejectsWrongRunAndMultiplePanes(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is unavailable")
	}
	t.Setenv("TT_TMUX_SOCKET", "tt-claude-fake-"+api.NewID("agt"))
	b := testClaudeBinding()
	b.Session = "claude-fake-" + strings.TrimPrefix(api.NewID("agt"), "agt_")
	cmd := func(args ...string) error {
		c := exec.Command("tmux", append([]string{"-L", os.Getenv("TT_TMUX_SOCKET")}, args...)...)
		c.Env = append(os.Environ(), "TAILTERM_HUB="+b.Hub, "TAILTERM_TASK="+b.Task, "TAILTERM_AGENT="+b.Agent, "TAILTERM_RUN="+b.Run)
		out, err := c.CombinedOutput()
		if err != nil {
			return fmt.Errorf("tmux %v: %w: %s", args, err, out)
		}
		return nil
	}
	if err := cmd("new-session", "-d", "-s", b.Session, "sleep 30"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd("kill-session", "-t", b.Session) }()
	for key, value := range map[string]string{"TAILTERM_HUB": b.Hub, "TAILTERM_TASK": b.Task, "TAILTERM_AGENT": b.Agent, "TAILTERM_RUN": b.Run} {
		if err := cmd("set-environment", "-t", b.Session, key, value); err != nil {
			t.Fatal(err)
		}
	}
	wrong := b
	wrong.Run = "run_0000000000000002"
	if _, err := nativeClaudeInspect(context.Background(), wrong, ""); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("wrong run was accepted: %v", err)
	}
	if err := cmd("split-window", "-t", b.Session, "sleep 30"); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeClaudeInspect(context.Background(), b, ""); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("multiple panes were accepted: %v", err)
	}
}

// Run explicitly with TT_LIVE_CLAUDE=1. The hub is an isolated SQLite/HTTP
// fixture and tmux uses a private socket. This never attaches to a working
// agent's pane or to the live project hub.
func TestClaudeWakeLivePrivateSession(t *testing.T) {
	if os.Getenv("TT_LIVE_CLAUDE") != "1" {
		t.Skip("requires an explicit live Claude check")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(state, "relay"))
	t.Setenv("TT_TMUX_SOCKET", "tt-claude-wake-"+api.NewID("agt"))
	st, err := store.Open(filepath.Join(state, "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	localHub := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return api.Caller{Node: "fixture", User: "owner"}, nil }))
	defer localHub.Close()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := st.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Claude wake live fixture"}, by)
	if err != nil {
		t.Fatal(err)
	}
	session := "claude-wake-live-" + strings.TrimPrefix(api.NewID("agt"), "agt_")
	trustedCwd, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.AddAgent(context.Background(), task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "claude-live", Host: "mini", Session: session, Runtime: "claude", Cwd: trustedCwd}, by)
	if err != nil {
		t.Fatal(err)
	}
	thread := "00000000-0000-4000-8000-" + strings.TrimPrefix(api.NewID("agt"), "agt_")[:12]
	b := runtimeBinding{Hub: localHub.URL, Task: task.ID, Agent: agent.ID, Run: agent.RunID, Thread: thread, Runtime: "claude", Session: session}
	// The child runs in the exact owned session with a test-only hub identity.
	start := exec.Command("tmux", "-L", os.Getenv("TT_TMUX_SOCKET"), "new-session", "-d", "-x", "80", "-y", "24", "-s", session, "-c", trustedCwd, claude+" --session-id "+thread)
	start.Env = append(os.Environ(), "TAILTERM_HUB="+b.Hub, "TAILTERM_TASK="+b.Task, "TAILTERM_AGENT="+b.Agent, "TAILTERM_RUN="+b.Run)
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("private tmux start: %v %s", err, out)
	}
	defer func() {
		out, err := exec.Command("tmux", "-L", os.Getenv("TT_TMUX_SOCKET"), "kill-session", "-t", session).CombinedOutput()
		t.Logf("private tmux cleanup session=%s socket=%s: %v %s", session, os.Getenv("TT_TMUX_SOCKET"), err, strings.TrimSpace(string(out)))
	}()
	if out, err := exec.Command("tmux", "-L", os.Getenv("TT_TMUX_SOCKET"), "display-message", "-p", "-t", session, "#{window_width}x#{window_height}").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "80x24" {
		t.Fatalf("private tmux dimensions: %q %v", out, err)
	} else {
		t.Logf("private tmux dimensions: %s", strings.TrimSpace(string(out)))
	}
	for key, value := range map[string]string{"TAILTERM_HUB": b.Hub, "TAILTERM_TASK": b.Task, "TAILTERM_AGENT": b.Agent, "TAILTERM_RUN": b.Run} {
		if out, err := exec.Command("tmux", "-L", os.Getenv("TT_TMUX_SOCKET"), "set-environment", "-t", session, key, value).CombinedOutput(); err != nil {
			t.Fatalf("session identity: %v %s", err, out)
		}
	}
	// A short benign first turn creates a completed transcript; no work agent
	// or production data is touched. Its pane is inspected before each input.
	ready := false
	var lastScreen string
	for startDeadline := time.Now().Add(30 * time.Second); time.Now().Before(startDeadline); {
		screen, err := exec.Command("tmux", "-L", os.Getenv("TT_TMUX_SOCKET"), "capture-pane", "-p", "-t", session).Output()
		lastScreen = string(screen)
		if err == nil && emptyClaudeInput(string(screen)) {
			ready = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("dedicated Claude prompt did not become idle: %q", lastScreen)
	}
	first := "Reply with OK only."
	if out, err := exec.Command("tmux", "-L", os.Getenv("TT_TMUX_SOCKET"), "send-keys", "-t", session, "-l", "--", first).CombinedOutput(); err != nil {
		t.Fatalf("initial text: %v %s", err, out)
	}
	time.Sleep(100 * time.Millisecond)
	if out, err := exec.Command("tmux", "-L", os.Getenv("TT_TMUX_SOCKET"), "send-keys", "-t", session, "Enter").CombinedOutput(); err != nil {
		t.Fatalf("initial Enter: %v %s", err, out)
	}
	deadline := time.Now().Add(90 * time.Second)
	var idleErr error
	idle := false
	for time.Now().Before(deadline) {
		if snap, err := nativeClaudeInspect(context.Background(), b, ""); err == nil && snap.Cursor.TurnComplete {
			t.Logf("idle name=%s session=%s run=%s thread=%s pane=%s created=%s offset=%d", session, snap.SessionID, b.Run, b.Thread, snap.Pane, snap.Created, snap.Offset)
			idle = true
			break
		} else if err != nil {
			idleErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !idle {
		t.Fatalf("dedicated Claude pane did not become safe to wake: %v", idleErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	prompt := claudeBrokerPrompt("Tailterm obligations #13260", 13260, "wake_0123456789abcdef")
	if err := claudeQueue(ctx, b, prompt); err != nil {
		if screen, captureErr := exec.Command("tmux", "-L", os.Getenv("TT_TMUX_SOCKET"), "capture-pane", "-p", "-J", "-t", session).Output(); captureErr == nil {
			t.Logf("dedicated pane after attempted wake: %q", string(screen))
		}
		t.Fatal(err)
	}
	var intent claudeWakeIntent
	data, err := os.ReadFile(claudeWakePath(b))
	if err != nil || json.Unmarshal(data, &intent) != nil || intent.Phase != "confirmed" || intent.EnterAt.Sub(intent.TextAt) < 100*time.Millisecond || intent.ConfirmedAt.Before(intent.EnterAt) {
		t.Fatalf("confirmation guard: %v %s", err, data)
	}
	t.Logf("confirmed pane=%s attempt=%s text=%s enter=%s user-turn=%s transcript=%s offset=%d", intent.Pane, intent.At.Format(time.RFC3339Nano), intent.TextAt.Format(time.RFC3339Nano), intent.EnterAt.Format(time.RFC3339Nano), intent.ConfirmedAt.Format(time.RFC3339Nano), intent.Path, intent.Offset)
}
