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
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/spawn"
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

func readClaudePaneFixture(t *testing.T, name string) (string, int, int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "claude-pane", name+".ansi"))
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := os.ReadFile(filepath.Join("testdata", "claude-pane", strings.SplitN(name, ".", 2)[0]+".cursor"))
	if err != nil {
		t.Fatal(err)
	}
	var x, y int
	if _, err := fmt.Sscanf(string(cursor), "%d %d", &x, &y); err != nil {
		t.Fatal(err)
	}
	return string(raw), x, y
}

// k5/k6: live Claude Code 2.1.284 captures (testdata/claude-pane/README.md).
// The prompt suggestion and the Try placeholder are faint and leave the input
// empty; typed text, including text that equals the suggestion, refuses.
func TestClaudeInputPromptSuggestionFixtures(t *testing.T) {
	const wake = "Tailterm messages #14056. Run tt inbox --unread --mark-read."
	for _, tc := range []struct {
		fixture, expected string
		wake              bool
	}{
		{"suggestion", "", true},
		{"try-placeholder", "", true},
		{"typed", "", false},
		{"typed-prefix", "", false},
		{"typed-full-suggestion", "", false},
		{"wake-prompt-typed", "", false},
		// Pre-Enter checks read the joined capture of the typed wake prompt.
		{"wake-prompt-typed.joined", wake, true},
		{"typed.joined", wake, false},
		{"suggestion.joined", wake, false},
		{"typed-full-suggestion.joined", "only test files", true},
		{"suggestion.joined", "only test files", false},
	} {
		t.Run(tc.fixture+"/"+tc.expected, func(t *testing.T) {
			raw, x, y := readClaudePaneFixture(t, tc.fixture)
			screen, err := claudeInputScreen(raw, x, y, tc.expected)
			if (err == nil) != tc.wake {
				t.Fatalf("claudeInputScreen = %q, %v; want wake=%v", screen, err, tc.wake)
			}
			if tc.wake && tc.expected == "" && !emptyClaudeInput(screen) {
				t.Fatalf("idle screen not empty for the wake guard: %q", screen)
			}
		})
	}
	// Without attributes the suggestion reads as typed text, so a plain
	// capture keeps refusing: the rule depends on the attribute, not the words.
	raw, x, y := readClaudePaneFixture(t, "suggestion")
	plain := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(raw, "")
	if _, err := claudeInputScreen(plain, x, y, ""); err == nil {
		t.Fatal("unattributed suggestion was treated as empty input")
	}
}

// k5: attribute layouts the rule does not know fail closed.
func TestClaudeInputFaintTextFailsClosed(t *testing.T) {
	rule := "\x1b[38;5;244m────────────────\n"
	footer := "\x1b[39m  ⏵⏵ bypass permissions on\n"
	for _, tc := range []struct {
		name, input string
		cursorX     int
		wake        bool
	}{
		{"suggestion", "\x1b[39m❯ \x1b[2mRun tt inbox --unread --mark-read.\x1b[0m\n", 2, true},
		{"suggestion reset by 22", "\x1b[39m❯ \x1b[2mRun tt inbox\x1b[22m\n", 2, true},
		{"faint then typed", "\x1b[39m❯ \x1b[2mRun\x1b[0m typed\n", 2, false},
		{"typed then faint", "\x1b[39m❯ typed \x1b[2mRun tt inbox\x1b[0m\n", 2, false},
		{"colour index 2 is not faint", "\x1b[39m❯ \x1b[38;5;2mRun tt inbox\x1b[39m\n", 2, false},
		{"colon subparameter", "\x1b[39m❯ \x1b[38:5:2mRun tt inbox\x1b[0m\n", 2, false},
		{"hyperlink in input", "\x1b[39m❯ \x1b]8;;https://example.invalid\x07\x1b[2mRun\x1b[0m\x1b]8;;\x07\n", 2, false},
		{"cursor mode escape in input", "\x1b[39m❯ \x1b[?25l\x1b[2mRun\x1b[0m\n", 2, false},
		{"suggestion with cursor after text", "\x1b[39m❯ \x1b[2mRun tt inbox\x1b[0m\n", 14, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A hyperlink in the transcript above the input is tolerated.
			raw := "\x1b]8;;file:///tmp/x\x07relay.go\x1b]8;;\x07 done\n" + rule + tc.input + rule + footer
			_, err := claudeInputScreen(raw, tc.cursorX, 2, "")
			if (err == nil) != tc.wake {
				t.Fatalf("wake=%v err=%v", err == nil, err)
			}
		})
	}
}

// liveClaude is one disposable Claude Code session for the opt-in live checks:
// an isolated SQLite/HTTP hub, a private tmux socket, and a tt built from this
// checkout on the session's PATH. It never touches the live hub, the owner's
// Claude settings or a working agent pane.
type liveClaude struct {
	t       *testing.T
	st      *store.Store
	b       runtimeBinding
	client  *api.Client
	owner   api.Caller
	session string
}

func startLiveClaude(t *testing.T, suggestions bool) *liveClaude {
	t.Helper()
	if os.Getenv("TT_LIVE_CLAUDE") != "1" {
		t.Skip("requires an explicit live Claude check")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	bin := filepath.Join(state, "bin")
	if out, err := exec.Command("go", "build", "-o", filepath.Join(bin, "tt"), ".").CombinedOutput(); err != nil {
		t.Fatalf("build tt: %v %s", err, out)
	}
	t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(state, "relay"))
	socket := "tt-claude-live-" + strings.TrimPrefix(api.NewID("agt"), "agt_")
	t.Setenv("TT_TMUX_SOCKET", socket)
	st, err := store.Open(filepath.Join(state, "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	owner := api.Caller{Node: "fixture", User: "owner"}
	localHub := httptest.NewServer(server.New(st, func(*http.Request) (api.Caller, error) { return owner, nil }))
	t.Cleanup(localHub.Close)
	task, err := st.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Claude wake live fixture"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	cwd := os.Getenv("TT_LIVE_CLAUDE_CWD")
	if cwd == "" {
		cwd = "../../.."
	}
	if cwd, err = filepath.Abs(cwd); err != nil {
		t.Fatal(err)
	}
	session := "claude-live-" + strings.TrimPrefix(api.NewID("agt"), "agt_")
	agent, err := st.AddAgent(context.Background(), task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "claude-live", Host: "mini", Session: session, Runtime: "claude", Cwd: cwd}, owner)
	if err != nil {
		t.Fatal(err)
	}
	thread := "00000000-0000-4000-8000-" + strings.TrimPrefix(api.NewID("agt"), "agt_")[:12]
	b := runtimeBinding{Hub: localHub.URL, Task: task.ID, Agent: agent.ID, Run: agent.RunID, Thread: thread, Runtime: "claude", Session: session}
	identity := map[string]string{"TAILTERM_HUB": b.Hub, "TAILTERM_TASK": b.Task, "TAILTERM_AGENT": b.Agent, "TAILTERM_RUN": b.Run, "TAILTERM_AGENT_NAME": agent.Name}
	var childEnv []string
	for _, kv := range os.Environ() {
		// No identity or nesting markers from the session running the test.
		if !strings.HasPrefix(kv, "TAILTERM_") && !strings.HasPrefix(kv, "CLAUDECODE=") && !strings.HasPrefix(kv, "CLAUDE_CODE_") && !strings.HasPrefix(kv, "PATH=") {
			childEnv = append(childEnv, kv)
		}
	}
	childEnv = append(childEnv, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for key, value := range identity {
		childEnv = append(childEnv, key+"="+value)
	}
	command := claude + " --session-id " + thread
	if suggestions {
		// Per process only; the owner's user settings stay as they are.
		childEnv = append(childEnv, "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=1")
		command += ` --settings '{"promptSuggestionEnabled":true}'`
	} else {
		childEnv = append(childEnv, "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=0")
	}
	lc := &liveClaude{t: t, st: st, b: b, owner: owner, session: session}
	// As in tt spawn, tt wrap runs the runtime and posts started and 30-second
	// heartbeat events, which keep the hub agent online for the relay.
	wrapped := spawn.ShellQuote(filepath.Join(bin, "tt")) + " wrap --shell " + spawn.ShellQuote(command)
	start := exec.Command("tmux", "-L", socket, "-f", "/dev/null", "new-session", "-d", "-x", "100", "-y", "30", "-s", session, "-c", cwd, wrapped)
	start.Env = childEnv
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("private tmux start: %v %s", err, out)
	}
	t.Cleanup(func() {
		out, err := exec.Command("tmux", "-L", socket, "kill-server").CombinedOutput()
		t.Logf("private tmux cleanup session=%s socket=%s: %v %s", session, socket, err, strings.TrimSpace(string(out)))
	})
	for key, value := range identity {
		if out, err := exec.Command("tmux", "-L", socket, "set-environment", "-t", session, key, value).CombinedOutput(); err != nil {
			t.Fatalf("session identity: %v %s", err, out)
		}
	}
	if lc.client, err = api.NewClient(b.Hub, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	var screen string
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		screen = lc.capture(false)
		if raw := lc.capture(true); strings.Contains(raw, "❯") {
			if plain, err := claudePlainScreen(raw); err == nil && emptyClaudeInput(plain) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dedicated Claude prompt did not become idle: %q", screen)
		}
	}
	t.Logf("live session agent=%s run=%s session=%s thread=%s socket=%s cwd=%s suggestions=%v", b.Agent, b.Run, session, thread, socket, cwd, suggestions)
	return lc
}

func (lc *liveClaude) tmux(args ...string) string {
	out, err := exec.Command("tmux", append([]string{"-L", os.Getenv("TT_TMUX_SOCKET")}, args...)...).CombinedOutput()
	if err != nil {
		lc.t.Fatalf("tmux %v: %v %s", args, err, out)
	}
	return string(out)
}

func (lc *liveClaude) capture(attributes bool) string {
	if attributes {
		return lc.tmux("capture-pane", "-p", "-e", "-t", lc.session)
	}
	return lc.tmux("capture-pane", "-p", "-t", lc.session)
}

func (lc *liveClaude) submit(text string) {
	lc.tmux("send-keys", "-t", lc.session, "-l", "--", text)
	time.Sleep(150 * time.Millisecond)
	lc.tmux("send-keys", "-t", lc.session, "Enter")
}

// idle waits for a completed turn that the relay's own inspection accepts.
func (lc *liveClaude) idle(timeout time.Duration) claudeWakeSnapshot {
	var last error
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(time.Second) {
		snap, err := nativeClaudeInspect(context.Background(), lc.b, "")
		if err == nil && snap.Cursor.TurnComplete {
			return snap
		}
		last = err
	}
	lc.t.Fatalf("Claude did not become safe to wake: %v\n%s", last, lc.capture(false))
	return claudeWakeSnapshot{}
}

var liveSuggestion = regexp.MustCompile("❯[ \u00a0]\x1b\\[2m(.*?)\x1b\\[0m")

// suggestion returns the faint text after the input marker, if any.
func (lc *liveClaude) suggestion(timeout time.Duration) string {
	for deadline := time.Now().Add(timeout); ; time.Sleep(time.Second) {
		// Claude Code 2.1.284 follows the marker with a no-break space.
		for _, line := range strings.Split(lc.capture(true), "\n") {
			if match := liveSuggestion.FindStringSubmatch(line); match != nil {
				return match[1]
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
	}
}

func (lc *liveClaude) post(text string, replyTo int64) api.Message {
	m, err := lc.st.PostMessage(context.Background(), lc.b.Task, api.PostMessageRequest{To: lc.b.Agent, Text: text, ReplyTo: replyTo}, lc.owner)
	if err != nil {
		lc.t.Fatal(err)
	}
	return m
}

// relay runs the host relay's broker and inbox passes against the isolated hub
// at the relay's 3-second cadence until done reports true or the time is up.
func (lc *liveClaude) relay(p *relayProgress, timeout time.Duration, done func() bool) {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline) && !done(); time.Sleep(3 * time.Second) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		now := time.Now().UTC()
		queued, brokerErr := relayWakeJob(ctx, lc.b, p, lc.client, now, claudeQueue)
		if brokerErr != nil {
			lc.t.Logf("broker wake: %v", brokerErr)
		}
		if !queued {
			if err := relayOne(ctx, lc.b, p, lc.client, now, claudeQueue); err != nil {
				lc.t.Logf("relay pass: %v", err)
			}
		}
		cancel()
	}
}

func (lc *liveClaude) status(p relayProgress) string {
	line := fmt.Sprintf("queued-through=%d broker-wakes=%v", p.Through, p.BrokerWakes)
	if p.Wake != nil {
		line += fmt.Sprintf(" wake=%s seqs=%v at=%s", p.Wake.Status, p.Wake.MessageSeqs, p.Wake.At.Format(time.RFC3339Nano))
	}
	if p.Skip != nil {
		line += fmt.Sprintf(" skip=%q skip-seqs=%v skip-at=%s", p.Skip.Reason, p.Skip.MessageSeqs, p.Skip.At.Format(time.RFC3339))
	}
	return line
}

func (lc *liveClaude) intent() claudeWakeIntent {
	var intent claudeWakeIntent
	data, err := os.ReadFile(claudeWakePath(lc.b))
	if err != nil || json.Unmarshal(data, &intent) != nil {
		lc.t.Fatalf("wake intent: %v %s", err, data)
	}
	return intent
}

// k4: run with TT_LIVE_CLAUDE=1. The disposable session asks the owner a
// decision with tt ask, runs tt event needs_input and ends its turn; the
// owner's answer must wake it through the real relay passes and tmux input.
func TestClaudeWakeLiveNeedsInput(t *testing.T) {
	lc := startLiveClaude(t, false)
	lc.submit(`This is a disposable Tailterm relay test session on an isolated local test hub. Run exactly this one shell command line, then reply "waiting" and end your turn: printf '%s' '{"question":"Relay test: pick A or B?","options":[{"id":"a","label":"A","description":"Option A"},{"id":"b","label":"B","description":"Option B"}],"recommendedOptionId":"a","recommendationReason":"Fixture default"}' | tt ask --request-id live-decision --file - && tt event needs_input --text "waiting for the owner's decision"`)
	var agent api.Agent
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
		var err error
		if agent, err = lc.st.GetAgent(context.Background(), lc.b.Agent); err == nil && agent.Status == api.AgentNeedsInput {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not set needs_input: %+v\n%s", agent, lc.capture(false))
		}
	}
	lc.idle(2 * time.Minute)
	messages, err := lc.st.ListMessages(context.Background(), lc.b.Task, 0, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	var decision api.Message
	for _, m := range messages {
		if m.From.AgentID == lc.b.Agent && m.DecisionRequest != nil {
			decision = m
		}
	}
	if decision.Seq == 0 {
		t.Fatalf("no decision request from the session: %+v", messages)
	}
	if agent, err = lc.st.GetAgent(context.Background(), lc.b.Agent); err != nil || agent.Status != api.AgentNeedsInput {
		t.Fatalf("status before the answer: %+v %v", agent, err)
	}
	idleAt := time.Now().UTC()
	answer, err := lc.st.AnswerDecision(context.Background(), lc.b.Task, decision.Seq, api.AnswerDecisionRequest{RequestID: "live-answer", OptionID: "a"}, lc.owner)
	if err != nil {
		t.Fatal(err)
	}
	answeredAt := time.Now().UTC()
	var p relayProgress
	lc.relay(&p, time.Minute, func() bool { return p.Wake != nil && p.Wake.Status == "confirmed" })
	if p.Wake == nil || p.Wake.Status != "confirmed" || fmt.Sprint(p.Wake.MessageSeqs) != fmt.Sprint([]int64{answer.Seq}) {
		t.Fatalf("needs_input session was not woken by the answer: %s\n%s", lc.status(p), lc.capture(false))
	}
	intent := lc.intent()
	if !strings.Contains(intent.Prompt, fmt.Sprintf("#%d", answer.Seq)) || intent.Phase != "confirmed" {
		t.Fatalf("confirmed prompt: %+v", intent)
	}
	t.Logf("k4 live: agent=%s run=%s status=%s decision=#%d answer=#%d idle=%s answered=%s text=%s enter=%s confirmed=%s answer-to-confirmed=%s",
		lc.b.Agent, lc.b.Run, agent.Status, decision.Seq, answer.Seq, idleAt.Format(time.RFC3339Nano), answeredAt.Format(time.RFC3339Nano),
		intent.TextAt.Format(time.RFC3339Nano), intent.EnterAt.Format(time.RFC3339Nano), intent.ConfirmedAt.Format(time.RFC3339Nano), intent.ConfirmedAt.Sub(answeredAt))
	t.Logf("relay status: %s", lc.status(p))
	path := "inbox"
	if strings.Contains(intent.Prompt, " Wake wake_") {
		path = "broker"
	}
	t.Logf("user turn prompt (%s path): %q transcript=%s offset=%d", path, intent.Prompt, intent.Path, intent.Offset)
}

// k5 live: with suggestions enabled for this process only, a wake confirms
// while the faint suggestion shows, and real typed text refuses with a logged
// reason.
func TestClaudeWakeLivePromptSuggestion(t *testing.T) {
	lc := startLiveClaude(t, true)
	var p relayProgress
	shown := ""
	for i, seed := range []string{
		"Run ls | head -3, show the output, then ask whether I want the full list or only directories.",
		"Run ls hub | head -3, show the output, then ask whether I want the Go files or the other files.",
	} {
		lc.submit(seed)
		lc.idle(2 * time.Minute)
		if shown = lc.suggestion(30 * time.Second); shown != "" {
			t.Logf("seed %d produced suggestion %q", i+1, shown)
			break
		}
	}
	if shown == "" {
		var input []string
		for _, line := range strings.Split(lc.capture(true), "\n") {
			if strings.Contains(line, "❯") {
				input = append(input, fmt.Sprintf("%q", line))
			}
		}
		t.Fatalf("Claude Code showed no prompt suggestion to test against: %s\n%s", strings.Join(input, " "), lc.capture(false))
	}
	for round := 1; round <= 2; round++ {
		snap := lc.idle(time.Minute)
		shown = lc.suggestion(0)
		if shown == "" {
			if round == 1 {
				t.Fatal("suggestion vanished before the first wake")
			}
			t.Logf("round %d: no suggestion after the previous wake; that round is a plain wake", round)
		}
		m := lc.post(fmt.Sprintf("Relay suggestion check %d: reply OK only.", round), 0)
		posted := time.Now().UTC()
		// A broker wake also names obligations the session read but has not
		// acknowledged, so the confirmed prompt must include this message.
		woken := func() bool {
			return p.Wake != nil && p.Wake.Status == "confirmed" && slices.Contains(p.Wake.MessageSeqs, m.Seq)
		}
		lc.relay(&p, time.Minute, woken)
		if !woken() {
			t.Fatalf("round %d wake with suggestion %q did not confirm: %s\n%s", round, shown, lc.status(p), lc.capture(false))
		}
		intent := lc.intent()
		t.Logf("k5 live round %d: suggestion=%q message=#%d pane-offset=%d posted=%s confirmed=%s posted-to-confirmed=%s prompt=%q", round, shown, m.Seq, snap.Offset,
			posted.Format(time.RFC3339Nano), intent.ConfirmedAt.Format(time.RFC3339Nano), intent.ConfirmedAt.Sub(posted), intent.Prompt)
		lc.idle(2 * time.Minute)
		if round == 1 {
			// The incident's suggestion came from the previous wake prompt.
			if lc.suggestion(30*time.Second) == "" {
				t.Log("no suggestion appeared after the first wake")
			}
		}
	}
	// Real typed text, not a suggestion, must still refuse.
	lc.idle(time.Minute)
	lc.tmux("send-keys", "-t", lc.session, "-l", "--", "hello there")
	time.Sleep(500 * time.Millisecond)
	typed := lc.post("Relay typed-text check: reply OK only.", 0)
	refused := func() bool {
		return p.Skip != nil && slices.Contains(p.Skip.MessageSeqs, typed.Seq) && strings.Contains(p.Skip.Reason, "not safely idle")
	}
	out, _ := captureRelayOutput(t, true, func() error {
		lc.relay(&p, 40*time.Second, refused)
		return nil
	})
	if !refused() || !strings.Contains(out, "not safely idle") {
		t.Fatalf("typed text did not refuse with a logged reason: %s\nlog=%q\n%s", lc.status(p), out, lc.capture(false))
	}
	if !strings.Contains(strings.ReplaceAll(lc.capture(false), "\u00a0", " "), "❯ hello there") || strings.Contains(lc.intent().Prompt, fmt.Sprintf("#%d", typed.Seq)) {
		t.Fatalf("typed input was disturbed:\n%s", lc.capture(false))
	}
	t.Logf("k5 live typed: message=#%d %s", typed.Seq, lc.status(p))
	t.Logf("relay log: %s", strings.TrimSpace(out))
	lc.tmux("send-keys", "-t", lc.session, "C-u")
}
