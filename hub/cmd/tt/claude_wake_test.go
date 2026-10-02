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

// wakeRetryPane models one Claude pane and its transcript for wake tests:
// typed text stays in the input until an Enter that submits appends it to the
// transcript as a user turn. Inspect fails like the native one when the input
// does not match what the caller expects.
type wakeRetryPane struct {
	t          *testing.T
	transcript string
	input      string
	dialog     bool
	busy       bool
	submit     bool
	enterErr   error
	now        time.Time
	sends      []string
}

func newWakeRetryPane(t *testing.T) *wakeRetryPane {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	path := filepath.Join(t.TempDir(), "claude.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"assistant","message":{"stop_reason":"end_turn"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return &wakeRetryPane{t: t, transcript: path, now: time.Now()}
}

func (f *wakeRetryPane) appendUser(text string) {
	f.t.Helper()
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": text}})
	file, err := os.OpenFile(f.transcript, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		f.t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		f.t.Fatal(err)
	}
}

func (f *wakeRetryPane) ops() claudeWakeOps {
	return claudeWakeOps{
		inspect: func(_ context.Context, _ runtimeBinding, expected string) (claudeWakeSnapshot, error) {
			info, err := os.Stat(f.transcript)
			if err != nil {
				return claudeWakeSnapshot{}, err
			}
			if f.dialog {
				return claudeWakeSnapshot{}, errors.New("Claude pane has a permission or selection prompt")
			}
			if f.input != expected {
				return claudeWakeSnapshot{}, errors.New("Claude input is occupied, prompting, or unknown")
			}
			return claudeWakeSnapshot{Pane: "%1", SessionID: "$1", Created: "100", PanePID: 1001, Path: f.transcript, FileID: fileIdentity(info), Offset: info.Size(), Screen: "❯ " + expected + "\n", Cursor: activityCursor{Ready: true, SeenTurn: true, TurnComplete: !f.busy}}, nil
		},
		send: func(_ context.Context, _ string, value string, literal bool) error {
			if literal {
				f.sends = append(f.sends, "text:"+value)
				f.input += value
				return nil
			}
			f.sends = append(f.sends, "Enter")
			if f.enterErr != nil {
				return f.enterErr
			}
			if f.submit && f.input != "" {
				f.appendUser(f.input)
				f.input = ""
			}
			return nil
		},
		sleep: func(d time.Duration) { f.now = f.now.Add(d) },
		now:   func() time.Time { return f.now },
	}
}

func (f *wakeRetryPane) intent(b runtimeBinding) claudeWakeIntent {
	f.t.Helper()
	var saved claudeWakeIntent
	data, err := os.ReadFile(claudeWakePath(b))
	if err != nil || json.Unmarshal(data, &saved) != nil {
		f.t.Fatalf("wake intent unreadable: %v %s", err, data)
	}
	return saved
}

// An unconfirmed wake is never resent immediately, including after a relay
// restart (the guard is durable); only the backoff-gated retry may act.
func TestClaudeWakeTimeoutAndRestartGuard(t *testing.T) {
	f := newWakeRetryPane(t)
	b := testClaudeBinding()
	prompt := "Tailterm obligations #41. Run tt inbox --unread --mark-read."
	if err := claudeWakeWith(context.Background(), b, prompt, f.ops()); err == nil || !strings.Contains(err.Error(), "did not confirm") || len(f.sends) != 2 {
		t.Fatalf("timeout: sends=%v err=%v", f.sends, err)
	}
	for _, advance := range []time.Duration{0, 5 * time.Second, 9 * time.Second} {
		f.now = f.now.Add(advance)
		// A fresh ops value stands in for a restarted relay reading the guard.
		if err := claudeWakeWith(context.Background(), b, prompt, f.ops()); err == nil || !strings.Contains(err.Error(), "did not confirm") || len(f.sends) != 2 {
			t.Fatalf("uncertain wake resent before backoff: sends=%v err=%v", f.sends, err)
		}
	}
	if got := f.intent(b); got.Phase != "uncertain" || got.Attempts != 0 || got.FirstAt.IsZero() || !got.RetryAt.After(got.EnterAt) {
		t.Fatalf("guard after timeout: %+v", got)
	}
}

// A failed Enter leaves the wake text in the prompt. Nothing is retyped: after
// the backoff the retry presses Enter alone on that text.
func TestClaudeWakePartialSendNeverRetypes(t *testing.T) {
	f := newWakeRetryPane(t)
	f.enterErr = errors.New("synthetic Enter failure")
	b := testClaudeBinding()
	prompt := "Tailterm #42. Run tt inbox --unread --mark-read."
	if err := claudeWakeWith(context.Background(), b, prompt, f.ops()); err == nil || !strings.Contains(err.Error(), "did not confirm") || len(f.sends) != 2 {
		t.Fatalf("partial send %v %v", f.sends, err)
	}
	if err := claudeWakeWith(context.Background(), b, prompt, f.ops()); err == nil || len(f.sends) != 2 {
		t.Fatalf("ambiguous retry before backoff %v %v", f.sends, err)
	}
	f.enterErr, f.submit = nil, true
	f.now = f.now.Add(15 * time.Second)
	if err := claudeWakeWith(context.Background(), b, prompt, f.ops()); err != nil {
		t.Fatalf("retry after backoff: %v", err)
	}
	if want := []string{"text:" + prompt, "Enter", "Enter"}; !slices.Equal(f.sends, want) {
		t.Fatalf("sends %v, want %v (no retyped text)", f.sends, want)
	}
}

func TestClaudeWakeRetriesEnterWhenOwnTextRemains(t *testing.T) {
	f := newWakeRetryPane(t)
	b := testClaudeBinding()
	prompt := "Tailterm messages #50. Run tt inbox --unread --mark-read."
	// Enter reaches a pane too small to submit: the text stays typed.
	if err := claudeWakeWith(context.Background(), b, prompt, f.ops()); err == nil || !strings.Contains(err.Error(), "did not confirm") {
		t.Fatalf("first wake: %v", err)
	}
	if f.input != prompt {
		t.Fatalf("fixture input %q", f.input)
	}
	if err := claudeWakeWith(context.Background(), b, prompt, f.ops()); err == nil || len(f.sends) != 2 {
		t.Fatalf("immediate second call sent %v (%v)", f.sends, err)
	}
	f.submit = true
	f.now = f.now.Add(15 * time.Second)
	// A newer prompt does not replace the text already typed.
	newer := "Tailterm messages #50,#51. Run tt inbox --unread --mark-read."
	if err := claudeWakeWith(context.Background(), b, newer, f.ops()); err != nil {
		t.Fatalf("Enter retry: %v", err)
	}
	if want := []string{"text:" + prompt, "Enter", "Enter"}; !slices.Equal(f.sends, want) {
		t.Fatalf("sends %v, want %v", f.sends, want)
	}
	if got := f.intent(b); got.Phase != "confirmed" || got.Attempts != 1 || !strings.Contains(got.LastRetry, "Enter on own unsubmitted text") {
		t.Fatalf("intent after Enter retry: %+v", got)
	}
}

func TestClaudeWakeRetypesAfterLostText(t *testing.T) {
	f := newWakeRetryPane(t)
	b := testClaudeBinding()
	prompt := "Tailterm messages #60. Run tt inbox --unread --mark-read."
	if err := claudeWakeWith(context.Background(), b, prompt, f.ops()); err == nil {
		t.Fatal("first wake confirmed")
	}
	f.input = "" // the typed text was lost; nothing reached the transcript
	f.now = f.now.Add(10 * time.Second)
	current := "Tailterm messages #60,#61. Run tt inbox --unread --mark-read."
	if err := claudeWakeWith(context.Background(), b, current, f.ops()); err == nil || len(f.sends) != 2 {
		t.Fatalf("retype before backoff: %v %v", f.sends, err)
	}
	f.now = f.now.Add(5 * time.Second)
	f.submit = true
	if err := claudeWakeWith(context.Background(), b, current, f.ops()); err != nil {
		t.Fatalf("retype: %v", err)
	}
	if want := []string{"text:" + prompt, "Enter", "text:" + current, "Enter"}; !slices.Equal(f.sends, want) {
		t.Fatalf("sends %v, want %v", f.sends, want)
	}
	if got := f.intent(b); got.Phase != "confirmed" || got.Attempts != 1 || got.Prompt != current || !strings.Contains(got.LastRetry, "retyped") {
		t.Fatalf("intent after retype: %+v", got)
	}

	// The same shape where the first text did reach the transcript late
	// confirms without typing anything.
	g := newWakeRetryPane(t)
	if err := claudeWakeWith(context.Background(), b, prompt, g.ops()); err == nil {
		t.Fatal("first wake confirmed")
	}
	g.appendUser(g.input)
	g.input = ""
	g.now = g.now.Add(time.Minute)
	if err := claudeWakeWith(context.Background(), b, prompt, g.ops()); err != nil || len(g.sends) != 2 {
		t.Fatalf("late submission retyped: %v %v", g.sends, err)
	}
	if got := g.intent(b); got.Phase != "confirmed" {
		t.Fatalf("late submission not confirmed: %+v", got)
	}
}

func TestClaudeWakeRetryIsBoundedAndBacksOff(t *testing.T) {
	f := newWakeRetryPane(t)
	b := testClaudeBinding()
	prompt := "Tailterm messages #70. Run tt inbox --unread --mark-read."
	wake := func() error { return claudeWakeWith(context.Background(), b, prompt, f.ops()) }
	if err := wake(); err == nil {
		t.Fatal("first wake confirmed")
	}
	sent := len(f.sends)
	for i, backoff := range []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second} {
		last := f.intent(b)
		// Just before the backoff elapses: nothing is sent.
		f.now = last.RetryAt.Add(-time.Second)
		if err := wake(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("retry %d/4", i+1)) || len(f.sends) != sent {
			t.Fatalf("retry %d before backoff %s: %v %v", i+1, backoff, f.sends, err)
		}
		if got := last.RetryAt.Sub(last.EnterAt); got < backoff || got > backoff+6*time.Second {
			t.Fatalf("retry %d backoff %s, want %s", i+1, got, backoff)
		}
		f.now = last.RetryAt
		if err := wake(); err == nil || !strings.Contains(err.Error(), "did not confirm") {
			t.Fatalf("retry %d confirmed: %v", i+1, err)
		}
		if len(f.sends) != sent+1 || f.sends[len(f.sends)-1] != "Enter" {
			t.Fatalf("retry %d sends %v", i+1, f.sends)
		}
		sent++
	}
	f.now = f.intent(b).RetryAt
	if err := wake(); err == nil || !strings.Contains(err.Error(), "after 4 retries") {
		t.Fatalf("exhaustion: %v", err)
	}
	exhausted := f.intent(b)
	if exhausted.Phase != "exhausted" || exhausted.Attempts != 4 || !strings.Contains(exhausted.LastRetry, "exhausted after 4 retries") {
		t.Fatalf("exhausted intent: %+v", exhausted)
	}
	for _, advance := range []time.Duration{time.Minute, 10 * time.Minute, 3 * time.Minute} {
		f.now = f.now.Add(advance)
		if err := wake(); err == nil || !strings.Contains(err.Error(), "needs attention") || len(f.sends) != sent {
			t.Fatalf("typed during cool-down: %v %v", f.sends, err)
		}
	}
	if total := len(f.sends); total != 6 {
		t.Fatalf("total sends %d, want 2 + 4 retries", total)
	}
	// After the cool-down a new bounded cycle starts; the stuck age survives.
	f.now = exhausted.ExhaustedAt.Add(15 * time.Minute)
	f.submit = true
	if err := wake(); err != nil {
		t.Fatalf("new cycle: %v", err)
	}
	if got := f.intent(b); got.Phase != "confirmed" || !got.FirstAt.Equal(exhausted.FirstAt) || got.Attempts != 1 {
		t.Fatalf("new cycle intent: %+v", got)
	}
}

func TestClaudeWakeNeverPressesEnterOnForeignText(t *testing.T) {
	f := newWakeRetryPane(t)
	b := testClaudeBinding()
	prompt := "Tailterm messages #80. Run tt inbox --unread --mark-read."
	if err := claudeWakeWith(context.Background(), b, prompt, f.ops()); err == nil {
		t.Fatal("first wake confirmed")
	}
	sent := len(f.sends)
	for _, tc := range []struct {
		name  string
		setup func()
	}{
		{"owner draft", func() { f.input = "my own draft, not the wake" }},
		{"wake text plus more", func() { f.input = prompt + " and more" }},
		{"dialog", func() { f.input, f.dialog = prompt, true }},
		{"busy", func() { f.input, f.dialog, f.busy = "", false, true }},
	} {
		tc.setup()
		f.now = f.now.Add(10 * time.Minute)
		err := claudeWakeWith(context.Background(), b, prompt, f.ops())
		if !errors.Is(err, errClaudeWakeUnsafe) || len(f.sends) != sent {
			t.Fatalf("%s: sends %v err %v", tc.name, f.sends, err)
		}
		if got := f.intent(b); got.Attempts != 0 || got.Phase != "uncertain" || !strings.Contains(got.LastRetry, "not safe") {
			t.Fatalf("%s: unsafe pass counted as a retry: %+v", tc.name, got)
		}
	}
}

func TestClaudeWakeAbandonsOnTranscriptChange(t *testing.T) {
	f := newWakeRetryPane(t)
	b := testClaudeBinding()
	prompt := "Tailterm messages #90. Run tt inbox --unread --mark-read."
	if err := claudeWakeWith(context.Background(), b, prompt, f.ops()); err == nil {
		t.Fatal("first wake confirmed")
	}
	// The owner cleared the wake text and submitted a turn of their own.
	f.input = ""
	f.appendUser("owner turn")
	f.submit = true
	newer := "Tailterm messages #90,#91. Run tt inbox --unread --mark-read."
	if err := claudeWakeWith(context.Background(), b, newer, f.ops()); err != nil {
		t.Fatalf("fresh wake after abandonment: %v", err)
	}
	if want := []string{"text:" + prompt, "Enter", "text:" + newer, "Enter"}; !slices.Equal(f.sends, want) {
		t.Fatalf("sends %v, want %v", f.sends, want)
	}
	if got := f.intent(b); got.Phase != "confirmed" || got.Attempts != 0 || got.Prompt != newer {
		t.Fatalf("fresh intent: %+v", got)
	}
	// A replaced transcript file abandons the uncertain intent the same way,
	// before its backoff, and a fresh wake starts at once.
	g := newWakeRetryPane(t)
	if err := claudeWakeWith(context.Background(), b, prompt, g.ops()); err == nil || !strings.Contains(err.Error(), "did not confirm") {
		t.Fatalf("setup wake: %v", err)
	}
	before := g.intent(b)
	replacement := g.transcript + ".new"
	if err := os.WriteFile(replacement, []byte(`{"type":"assistant","message":{"stop_reason":"end_turn"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, g.transcript); err != nil {
		t.Fatal(err)
	}
	g.input, g.submit = "", true
	if err := claudeWakeWith(context.Background(), b, prompt, g.ops()); err != nil {
		t.Fatalf("fresh wake into the replaced transcript: %v", err)
	}
	if got := g.intent(b); got.FileID == before.FileID || got.Phase != "confirmed" || got.Attempts != 0 || len(g.sends) != 4 {
		t.Fatalf("replaced transcript was not abandoned for a fresh intent: %+v sends %v", got, g.sends)
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

func startLiveClaude(t *testing.T, suggestions bool, flags ...string) *liveClaude {
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
	for _, flag := range flags {
		command += " " + spawn.ShellQuote(flag)
	}
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
			if _, plain, err := claudePlainScreen(raw); err == nil && emptyClaudeInput(plain) {
				break
			} else if err == nil && len(flags) > 0 {
				// Outside bypass mode the footer (for example "⏸ manual mode
				// on") is not on the wake allowlist; typing only needs an empty
				// input box.
				if area, boxed := claudePromptArea(strings.Split(plain, "\n")); boxed && strings.TrimSpace(area[1]) == "❯" {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dedicated Claude prompt did not become idle: %q", screen)
		}
	}
	t.Logf("live session agent=%s run=%s session=%s thread=%s socket=%s cwd=%s suggestions=%v flags=%q", b.Agent, b.Run, session, thread, socket, cwd, suggestions, flags)
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

// Review f2: dialogs are detected on the full screen, before faint text is
// dropped from the input area.
func TestClaudeInputFaintDialogTextStillRefuses(t *testing.T) {
	rule := "\x1b[38;5;244m────────────────\n"
	for _, tc := range []struct {
		name, raw string
		cursorX   int
	}{
		{"faint Esc to cancel under a selection menu", "Allow Bash?\n\x1b[39m❯ 1. Yes\n  2. No\n\x1b[2mEsc to cancel\x1b[0m\n", 2},
		{"faint selection text on an otherwise empty input", rule + "\x1b[39m❯ \x1b[2mSelect an option (y/n)\x1b[0m\n" + rule, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := claudeInputScreen(tc.raw, tc.cursorX, 1, ""); err == nil || !strings.Contains(err.Error(), "permission or selection prompt") {
				t.Fatalf("faint dialog text was not refused as a prompt: %v", err)
			}
		})
	}
}

// scrollbackWake is the wake prompt typed into the scrollback-dialog-words-typed
// capture.
const scrollbackWake = "Tailterm messages #14142. Run tt inbox --unread --mark-read."

// k7/k8: live Claude Code 2.1.284 captures (testdata/claude-pane/README.md).
// Dialog words in the transcript above an idle, empty input box wake, on the
// empty check and on the pre-Enter check. A real permission, selection or
// trust dialog refuses both.
func TestClaudeInputDialogWordsInScrollback(t *testing.T) {
	raw, x, y := readClaudePaneFixture(t, "scrollback-dialog-words")
	full, plain, err := claudePlainScreen(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"permission dialog", "Esc to cancel", "Allow this tool?", "Do you want to proceed?", "Select an option", "(y/n)"} {
		if !strings.Contains(full, phrase) {
			t.Fatalf("fixture scrollback lacks %q", phrase)
		}
	}
	screen, err := claudeInputScreen(raw, x, y, "")
	if err != nil || !emptyClaudeInput(screen) {
		t.Fatalf("idle prompt under dialog words refused: %q %v", screen, err)
	}
	// Whole-screen callers (the live harness) read the same area.
	if !emptyClaudeInput(plain) {
		t.Fatal("whole idle screen refused")
	}
	raw, x, y = readClaudePaneFixture(t, "scrollback-dialog-words-typed.joined")
	if _, err := claudeInputScreen(raw, x, y, scrollbackWake); err != nil {
		t.Fatalf("pre-Enter check refused the typed wake prompt: %v", err)
	}
	raw, x, y = readClaudePaneFixture(t, "scrollback-dialog-words-typed")
	if _, err := claudeInputScreen(raw, x, y, ""); err == nil {
		t.Fatal("typed wake prompt passed the empty check")
	}
	for _, fixture := range []string{"permission-dialog", "permission-dialog.joined", "selection-dialog", "selection-dialog.joined", "trust-dialog"} {
		for _, expected := range []string{"", scrollbackWake} {
			t.Run(fixture+"/"+expected, func(t *testing.T) {
				raw, x, y := readClaudePaneFixture(t, fixture)
				if _, err := claudeInputScreen(raw, x, y, expected); err == nil || !strings.Contains(err.Error(), "Claude pane has a permission or selection prompt: ") {
					t.Fatalf("real dialog was not refused as a prompt: %v", err)
				}
			})
		}
	}
}

// k7: only the input box, from its top rule down, is the active prompt area.
// Anything the rule cannot place is checked on the whole capture.
func TestClaudePromptArea(t *testing.T) {
	rule := "────────────────"
	footer := "  ⏵⏵ bypass permissions on (shift+tab to cycle)"
	words := "⏺ The permission dialog said Allow this tool? Do you want to proceed?\n  Select an option (y/n), Esc to cancel.\n"
	for _, tc := range []struct {
		name, raw, reason string
		wake              bool
	}{
		{"words directly above the top rule", words + rule + "\n❯ \n" + rule + "\n" + footer + "\n", "", true},
		{"words above a hint row", words + "  Ctrl+Y to paste deleted text\n" + rule + "\n❯ \n" + rule + "\n" + footer + "\n", "", true},
		{"phrase on the input row", rule + "\n❯ Esc to cancel\n" + rule + "\n" + footer + "\n", `"esc to cancel" in prompt area row 2`, false},
		{"phrase in the footer", words + rule + "\n❯ \n" + rule + "\n  Esc to cancel · Tab to amend\n", `"esc to cancel" in prompt area row 4`, false},
		{"faint phrase on the input row", rule + "\n\x1b[39m❯ \x1b[2mSelect an option\x1b[0m\n" + rule + "\n" + footer + "\n", `"select an option" in prompt area row 2`, false},
		{"unbordered prompt", words + "❯ \n" + rule + "\n" + footer + "\n", "whole capture checked", false},
		{"rule not directly above the prompt", words + rule + "\nlater answer\n❯ \n" + rule + "\n" + footer + "\n", "whole capture checked", false},
		{"dialog selection row", words + rule + "\n Do you want to proceed?\n❯ 1. Yes\n  2. No\n Esc to cancel · Tab to amend\n", `"esc to cancel" in capture row 7`, false},
		{"no input marker", words + rule + "\n" + footer + "\n", "whole capture checked", false},
		{"two prompt rows below the rule", rule + "\n❯ \n❯ \n" + rule + "\n" + footer + "\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			y := strings.Count(tc.raw[:max(strings.Index(tc.raw, "❯"), 0)], "\n")
			screen, err := claudeInputScreen(tc.raw, 2, y, "")
			if (err == nil) != tc.wake || err == nil && !emptyClaudeInput(screen) {
				t.Fatalf("wake=%v screen=%q err=%v", err == nil, screen, err)
			}
			if tc.reason != "" && (err == nil || !strings.Contains(err.Error(), "Claude pane has a permission or selection prompt: ") || !strings.Contains(err.Error(), tc.reason)) {
				t.Fatalf("refusal %v does not name %q", err, tc.reason)
			}
			// Whole-screen callers see faint text too.
			full, _, _ := claudePlainScreen(tc.raw)
			if emptyClaudeInput(full) != tc.wake {
				t.Fatalf("emptyClaudeInput(whole screen) = %v", !tc.wake)
			}
		})
	}
	prompt := "Tailterm messages #7. Run tt inbox."
	if !exactClaudeInput(words+rule+"\n❯ "+prompt+"\n"+rule+"\n"+footer+"\n", prompt) {
		t.Fatal("typed prompt under dialog words was not recognized")
	}
	if exactClaudeInput(words+rule+"\n❯ "+prompt+"\n"+rule+"\n  Esc to cancel\n", prompt) {
		t.Fatal("typed prompt above a dialog footer was recognized")
	}
	if exactClaudeInput(rule+"\n Do you want to proceed?\n❯ "+prompt+"\n", prompt) {
		t.Fatal("typed prompt inside an unbordered dialog was recognized")
	}
}

// fixtureClaudeOps inspects live captures through claudeInputScreen, as
// nativeClaudeInspect does: idle for the empty checks, typed before Enter.
func fixtureClaudeOps(t *testing.T, idle, typed string, sent *[]string) claudeWakeOps {
	transcript := filepath.Join(t.TempDir(), "claude.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"assistant","message":{"stop_reason":"end_turn","content":[]}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var pending string
	return claudeWakeOps{inspect: func(_ context.Context, _ runtimeBinding, expected string) (claudeWakeSnapshot, error) {
		fixture := idle
		if expected != "" {
			fixture = typed
		}
		raw, x, y := readClaudePaneFixture(t, fixture)
		screen, err := claudeInputScreen(raw, x, y, expected)
		if err != nil {
			return claudeWakeSnapshot{}, err
		}
		info, err := os.Stat(transcript)
		if err != nil {
			return claudeWakeSnapshot{}, err
		}
		return claudeWakeSnapshot{Pane: "%1", SessionID: "$1", Created: "100", PanePID: 1001, Path: transcript, FileID: fileIdentity(info), Offset: info.Size(), Screen: screen, Cursor: activityCursor{Ready: true, SeenTurn: true, TurnComplete: true}}, nil
	}, send: func(_ context.Context, _ string, value string, literal bool) error {
		if literal {
			pending = value
			*sent = append(*sent, "text:"+value)
			return nil
		}
		*sent = append(*sent, "Enter")
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": pending}})
		f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.Write(append(line, '\n'))
		return err
	}, sleep: func(time.Duration) {}, now: time.Now}
}

// k7: a wake under dialog words in the transcript types once, passes the
// pre-Enter check and confirms; a real dialog gets no input at all.
func TestClaudeWakeScrollbackDialogWordsFixtures(t *testing.T) {
	t.Run("scrollback words", func(t *testing.T) {
		t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
		var sent []string
		if err := claudeWakeWith(context.Background(), testClaudeBinding(), scrollbackWake, fixtureClaudeOps(t, "scrollback-dialog-words", "scrollback-dialog-words-typed.joined", &sent)); err != nil {
			t.Fatalf("wake: %v sent=%q", err, sent)
		}
		if fmt.Sprint(sent) != fmt.Sprint([]string{"text:" + scrollbackWake, "Enter"}) {
			t.Fatalf("sends %q", sent)
		}
	})
	for _, fixture := range []string{"permission-dialog", "selection-dialog", "trust-dialog"} {
		t.Run(fixture, func(t *testing.T) {
			t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
			var sent []string
			err := claudeWakeWith(context.Background(), testClaudeBinding(), scrollbackWake, fixtureClaudeOps(t, fixture, fixture, &sent))
			if !errors.Is(err, errClaudeWakeUnsafe) || !strings.Contains(err.Error(), "permission or selection prompt") || len(sent) != 0 {
				t.Fatalf("dialog wake: %v sent=%q", err, sent)
			}
		})
	}
}

// k3 for #14113: a dialog refusal names the phrase and its row, logs once while
// the screen is unchanged, and the pane gets no input; the transcript quoting
// the same words then wakes.
func TestClaudeDialogRefusalLogsPhraseOnce(t *testing.T) {
	b, hub, c, pane := needsInputFixture(t)
	pane.raw, pane.cursorX, pane.cursorY = readClaudePaneFixture(t, "permission-dialog")
	now := time.Date(2026, 9, 28, 22, 30, 0, 0, time.UTC)
	hub.update(func(h *needsInputHub) {
		h.agent.ReadUpTo, h.agent.Unread = 40, 2
		h.messages = []api.Message{{Seq: 41, To: b.Agent, From: api.Sender{AgentID: needsInputHandler}}, {Seq: 42, To: b.Agent, From: api.Sender{Node: "workspace", User: "owner"}}}
	})
	var p relayProgress
	want := `Claude pane has a permission or selection prompt: "esc to cancel" in capture row 30 (no input box, whole capture checked)`
	out, _ := captureRelayOutput(t, true, func() error { return relayOne(context.Background(), b, &p, c, now, pane.queue) })
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, " "+b.Agent+" wake skipped: ") || !strings.Contains(out, want+" seqs=[41 42]") {
		t.Fatalf("dialog skip log %q, want %q", out, want)
	}
	if p.Skip == nil || !strings.Contains(p.Skip.Reason, want) || len(pane.sent) != 0 {
		t.Fatalf("dialog skip %+v sent=%q", p.Skip, pane.sent)
	}
	out, _ = captureRelayOutput(t, true, func() error { return relayOne(context.Background(), b, &p, c, now.Add(20*time.Second), pane.queue) })
	if out != "" || len(pane.sent) != 0 || p.LastAttempt != now.Add(20*time.Second) {
		t.Fatalf("steady dialog refusal logged again %q sent=%q", out, pane.sent)
	}
	pane.raw, pane.cursorX, pane.cursorY = readClaudePaneFixture(t, "scrollback-dialog-words")
	if err := relayOne(context.Background(), b, &p, c, now.Add(40*time.Second), pane.queue); err != nil {
		t.Fatal(err)
	}
	if p.Skip != nil || len(pane.sent) != 1 || p.Wake == nil || p.Wake.Status != "confirmed" || fmt.Sprint(p.Wake.MessageSeqs) != "[41 42]" {
		t.Fatalf("scrollback words did not wake: skip=%+v wake=%+v sent=%q", p.Skip, p.Wake, pane.sent)
	}
}

// k7 live: run with TT_LIVE_CLAUDE=1. (a) An answer that quotes dialog words
// leaves them in the transcript above an idle prompt; a directed message wakes
// the session. (b) A real Bash permission prompt (this process only runs with
// --permission-mode default) refuses with a logged reason naming the dialog,
// and the pane gets no input: the command is never approved.
func TestClaudeWakeLiveScrollbackDialogWords(t *testing.T) {
	t.Run("scrollback words wake", func(t *testing.T) {
		lc := startLiveClaude(t, false)
		lc.submit("Do not use any tools. Reply with exactly these six lines, then stop: 1. The permission dialog appeared. 2. Its footer said Esc to cancel. 3. It asked Allow this tool? 4. Then: Do you want to proceed? 5. A menu said Select an option. 6. The shell asked (y/n).")
		snap := lc.idle(2 * time.Minute)
		if screen := lc.capture(false); !strings.Contains(screen, "Allow this tool?") || !strings.Contains(screen, "Esc to cancel") {
			t.Fatalf("transcript does not show the dialog words:\n%s", lc.capture(false))
		}
		m := lc.post("Relay scrollback check: reply OK only.", 0)
		posted := time.Now().UTC()
		var p relayProgress
		woken := func() bool {
			return p.Wake != nil && p.Wake.Status == "confirmed" && slices.Contains(p.Wake.MessageSeqs, m.Seq)
		}
		lc.relay(&p, time.Minute, woken)
		if !woken() {
			t.Fatalf("scrollback dialog words blocked the wake: %s\n%s", lc.status(p), lc.capture(false))
		}
		intent := lc.intent()
		if intent.Phase != "confirmed" || !strings.Contains(intent.Prompt, fmt.Sprintf("#%d", m.Seq)) {
			t.Fatalf("confirmed prompt: %+v", intent)
		}
		t.Logf("k7 live wake: message=#%d pane-offset=%d posted=%s text=%s enter=%s confirmed=%s posted-to-confirmed=%s prompt=%q transcript=%s offset=%d",
			m.Seq, snap.Offset, posted.Format(time.RFC3339Nano), intent.TextAt.Format(time.RFC3339Nano), intent.EnterAt.Format(time.RFC3339Nano),
			intent.ConfirmedAt.Format(time.RFC3339Nano), intent.ConfirmedAt.Sub(posted), intent.Prompt, intent.Path, intent.Offset)
		t.Logf("relay status: %s", lc.status(p))
	})
	t.Run("real dialog refuses", func(t *testing.T) {
		lc := startLiveClaude(t, false, "--permission-mode", "default")
		target := filepath.Join(t.TempDir(), "approved-by-relay")
		lc.submit("Run this exact Bash command: touch " + target)
		var dialog string
		for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
			if dialog = lc.capture(false); strings.Contains(dialog, "Do you want to proceed?") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no permission prompt appeared:\n%s", dialog)
			}
		}
		m := lc.post("Relay dialog check: reply OK only.", 0)
		var p relayProgress
		refused := func() bool {
			return p.Skip != nil && slices.Contains(p.Skip.MessageSeqs, m.Seq) && strings.Contains(p.Skip.Reason, "Claude pane has a permission or selection prompt: ")
		}
		out, _ := captureRelayOutput(t, true, func() error {
			lc.relay(&p, 40*time.Second, refused)
			return nil
		})
		if !refused() || !strings.Contains(out, "Claude is not safely idle: Claude pane has a permission or selection prompt: ") || strings.Count(out, "wake skipped") != 1 {
			t.Fatalf("real dialog did not refuse once with a logged reason: %s\nlog=%q\n%s", lc.status(p), out, lc.capture(false))
		}
		// The transcript's ⏺ glyph blinks; the dialog from its top rule down
		// must be exactly as it was, with nothing typed or selected.
		after := lc.capture(false)
		box := func(screen string) string { return screen[max(strings.LastIndex(screen, "────"), 0):] }
		if _, err := os.Stat(claudeWakePath(lc.b)); !errors.Is(err, os.ErrNotExist) || !strings.Contains(box(after), "Do you want to proceed?") || box(after) != box(dialog) {
			t.Fatalf("dialog pane was disturbed (wake intent %v):\nbefore:\n%s\nafter:\n%s", err, dialog, after)
		}
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the Bash command ran: %v", err)
		}
		t.Logf("k7 live dialog: message=#%d %s", m.Seq, lc.status(p))
		t.Logf("relay log: %s", strings.TrimSpace(out))
		lc.tmux("send-keys", "-t", lc.session, "Escape")
	})
}

// helperWakeBinding registers the owner helper through the CLI in a private
// tmux session running a fake claude process, and returns the binding the
// relay would read.
func helperWakeBinding(t *testing.T) (helperFixture, runtimeBinding) {
	t.Helper()
	f := newHelperFixture(t)
	a := *f.mustRegister(t).Agent
	b, ok := readBinding(t, f.owner.hub, a.ID)
	if !ok {
		t.Fatal("no helper binding")
	}
	return f, b
}

func TestOwnerHelperWakePrivateSession(t *testing.T) {
	f, b := helperWakeBinding(t)
	ctx := context.Background()
	if b.Thread != f.thread || b.Thread == mustClaudeSessionID(t, b.Agent) {
		t.Fatalf("binding thread %q", b.Thread)
	}
	receipt, err := nativeRuntimeDiscovery(ctx, b, api.Agent{Runtime: "claude"})
	if err != nil || receipt.PID < 1 || receipt.Session != "owner" {
		t.Fatalf("discovery %+v %v", receipt, err)
	}
	if pane, err := inspectRuntimePane(ctx, b, "Claude", false); err != nil || pane.PanePID != receipt.PanePID {
		t.Fatalf("pane identity %+v %v", pane, err)
	}
	snapshot, err := claudeTranscriptSnapshot(b, time.Now())
	if err != nil || filepath.Base(snapshot.Path) != f.thread+".jsonl" {
		t.Fatalf("transcript %+v %v", snapshot, err)
	}
	// The verified pane and process keep the helper online through the relay heartbeat.
	var p relayProgress
	before, _ := f.c.GetAgent(ctx, f.task.ID, b.Agent)
	time.Sleep(1100 * time.Millisecond)
	if err := relayHelperHeartbeat(ctx, b, &p, f.c, time.Now(), activityProbeNative); err != nil {
		t.Fatal(err)
	}
	after, _ := f.c.GetAgent(ctx, f.task.ID, b.Agent)
	if !after.LastSeenAt.After(before.LastSeenAt) || !after.Online {
		t.Fatalf("heartbeat %s -> %s online=%v", before.LastSeenAt, after.LastSeenAt, after.Online)
	}
	// The helper prompt is typed once and confirmed by the transcript.
	pane := newWakeRetryPane(t)
	pane.submit = true
	prompt := claudeWakeFor(b, claudeBrokerPrompt("Tailterm obligations #73", 73, "wake_0123456789abcdef"))
	if err := claudeWakeWith(ctx, b, prompt, pane.ops()); err != nil {
		t.Fatal(err)
	}
	if len(pane.sends) != 2 || pane.sends[0] != "text:"+prompt || pane.sends[1] != "Enter" || pane.intent(b).Phase != "confirmed" {
		t.Fatalf("sends %v intent %+v", pane.sends, pane.intent(b))
	}
	if err := claudeWakeWith(ctx, b, prompt, pane.ops()); err != nil || len(pane.sends) != 2 {
		t.Fatalf("retyped a confirmed wake: %v %v", pane.sends, err)
	}
	// With the session gone there is no heartbeat.
	f.tmux(t, "kill-session", "-t", "owner")
	p = relayProgress{}
	seen := after.LastSeenAt
	time.Sleep(1100 * time.Millisecond)
	if err := relayHelperHeartbeat(ctx, b, &p, f.c, time.Now(), activityProbeNative); err != nil {
		t.Fatal(err)
	}
	if gone, _ := f.c.GetAgent(ctx, f.task.ID, b.Agent); !gone.LastSeenAt.Equal(seen) {
		t.Fatal("heartbeat without the owner session")
	}
}

func TestOwnerHelperWakeMultiPaneRefused(t *testing.T) {
	f, b := helperWakeBinding(t)
	ctx := context.Background()
	f.tmux(t, "split-window", "-t", "owner:", "sleep 300")
	if _, err := nativeRuntimeDiscovery(ctx, b, api.Agent{Runtime: "claude"}); err == nil || err.Error() != "ambiguous owned runtime pane" {
		t.Fatalf("two panes: %v", err)
	}
	prompt := claudeWakeFor(b, claudeWakePrompt([]api.Message{{Seq: 9}}, b.Agent))
	if err := claudeQueue(ctx, b, prompt); err == nil || !strings.Contains(err.Error(), "ambiguous owned runtime pane") {
		t.Fatalf("queue into two panes: %v", err)
	}
	for _, pane := range strings.Fields(f.tmux(t, "list-panes", "-s", "-t", "owner", "-F", "#{pane_id}")) {
		if screen := f.tmux(t, "capture-pane", "-p", "-t", pane); strings.Contains(screen, "Tailterm") {
			t.Fatalf("typed into %s: %q", pane, screen)
		}
	}
	if _, err := os.Stat(claudeWakePath(b)); !os.IsNotExist(err) {
		t.Fatalf("wake intent written: %v", err)
	}
}

func TestOwnerHelperWakePromptWraps(t *testing.T) {
	b := runtimeBinding{Task: "tsk_e7af3c28a444b09a", Role: api.AgentRoleOwnerHelper}
	prompts := []string{
		claudeWakeFor(b, claudeBrokerPrompt("Tailterm obligations #15073", 15073, "wake_c7370d00057feac6")),
		claudeWakeFor(b, "Tailterm messages #13260,#13261,#13262,#13263,#13264. Run tt inbox --unread --mark-read."),
	}
	for _, prompt := range prompts {
		if !strings.Contains(prompt, "tt helper inbox --task "+b.Task) {
			t.Fatalf("prompt %q", prompt)
		}
		for _, width := range []int{80, 40} {
			var lines []string
			line := "❯ "
			for _, word := range strings.Fields(prompt) {
				if len([]rune(line))+len([]rune(word))+1 > width && line != "❯ " {
					lines = append(lines, line)
					line = "  " + word
				} else {
					if line != "❯ " && line != "  " {
						line += " "
					}
					line += word
				}
			}
			lines = append(lines, line, strings.Repeat("─", width), "  ⏵⏵ bypass permissions on")
			if !exactClaudeInput(strings.Join(lines, "\n"), prompt) {
				t.Fatalf("wrapped %d-column helper input not recognized: %q", width, lines)
			}
		}
	}
}

// p2/p4: a transcript that ends on Claude Code's API-error record, with or
// without the following turn_duration, is idle; a busy turn still refuses.
func TestClaudeWakeAPIErrorTurnSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 25, 19, 1, 0, 0, time.UTC)
	for name, extra := range map[string][]string{
		"with turn_duration":    {claudeAPIErrorPrompt, claudeAPIErrorEndTurn, claudeAPIErrorRecord, claudeAPIErrorTurnDuration},
		"API-error record only": {claudeAPIErrorPrompt, claudeAPIErrorEndTurn, claudeAPIErrorRecord},
	} {
		t.Run(name, func(t *testing.T) {
			b, _ := claudeTranscriptFixture(t, "queue-idle", extra...)
			snap, err := claudeTranscriptSnapshot(b, now)
			if err != nil {
				t.Fatalf("API-error turn refused: %v", err)
			}
			if !snap.Cursor.TurnComplete || snap.Cursor.TurnEndReason != "turn ended by API error (server_error)" || !claudeWakeIdle(snap, now) {
				t.Fatalf("snapshot not idle: complete=%v reason=%q", snap.Cursor.TurnComplete, snap.Cursor.TurnEndReason)
			}
		})
	}
	t.Run("queue-busy", func(t *testing.T) {
		b, _ := claudeTranscriptFixture(t, "queue-busy")
		if _, err := claudeTranscriptSnapshot(b, now); err == nil || !strings.HasSuffix(err.Error(), ": turn in progress") {
			t.Fatalf("busy turn not refused: %v", err)
		}
	})
}

// p2: the relay's wake reaches an agent stopped by an API error: exactly
// text then Enter, confirmed by the new user record. A busy turn gets nothing.
func TestClaudeWakeAPIErrorTurnDelivers(t *testing.T) {
	now := time.Date(2026, 9, 25, 19, 1, 0, 0, time.UTC)
	prompt := "Tailterm messages #15583. Run tt inbox --unread --mark-read."
	wake := func(t *testing.T, fixture string, extra ...string) ([]string, error) {
		t.Helper()
		b, transcript := claudeTranscriptFixture(t, fixture, extra...)
		t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
		var sent []string
		inspect := func(_ context.Context, b runtimeBinding, expected string) (claudeWakeSnapshot, error) {
			snap, err := claudeTranscriptSnapshot(b, now)
			if err != nil {
				return claudeWakeSnapshot{}, err
			}
			snap.Pane, snap.SessionID, snap.Created, snap.PanePID = "%1", "$1", "100", 1001
			if expected == "" {
				snap.Screen = "Answer\n❯ \n? for shortcuts\n"
			} else {
				snap.Screen = "❯ " + expected + "\n"
			}
			return snap, nil
		}
		send := func(_ context.Context, pane, value string, literal bool) error {
			if pane != "%1" {
				t.Fatalf("wrong pane %q", pane)
			}
			if literal {
				sent = append(sent, "text:"+value)
				return nil
			}
			sent = append(sent, "Enter")
			line, _ := json.Marshal(map[string]any{"type": "user", "timestamp": "2026-09-25T19:01:00.500Z", "message": map[string]any{"role": "user", "content": prompt}})
			appendClaudeRecords(t, transcript, string(line))
			return nil
		}
		err := claudeWakeWith(context.Background(), b, prompt, claudeWakeOps{inspect: inspect, send: send, sleep: func(d time.Duration) { now = now.Add(d) }, now: func() time.Time { return now }})
		if err == nil {
			var saved claudeWakeIntent
			data, _ := os.ReadFile(claudeWakePath(b))
			if json.Unmarshal(data, &saved) != nil || saved.Phase != "confirmed" {
				t.Fatalf("wake not confirmed: %s", data)
			}
		}
		return sent, err
	}
	sent, err := wake(t, "queue-idle", claudeAPIErrorPrompt, claudeAPIErrorEndTurn, claudeAPIErrorRecord, claudeAPIErrorTurnDuration)
	if err != nil || len(sent) != 2 || sent[0] != "text:"+prompt || sent[1] != "Enter" {
		t.Fatalf("API-error turn wake: sent=%q err=%v", sent, err)
	}
	sent, err = wake(t, "queue-busy")
	if err == nil || !strings.Contains(err.Error(), "turn in progress") || len(sent) != 0 {
		t.Fatalf("busy turn wake: sent=%q err=%v", sent, err)
	}
}

// transcriptWakeOps drives claudeWakeWith over the real transcript check with
// a fake pane: Enter appends the typed prompt as the new user record.
func transcriptWakeOps(t *testing.T, transcript string, now *time.Time, sent *[]string) claudeWakeOps {
	t.Helper()
	typed := ""
	return claudeWakeOps{
		inspect: func(_ context.Context, b runtimeBinding, expected string) (claudeWakeSnapshot, error) {
			snap, err := claudeTranscriptSnapshot(b, *now)
			if err != nil {
				return claudeWakeSnapshot{}, err
			}
			snap.Pane, snap.SessionID, snap.Created, snap.PanePID = "%1", "$1", "100", 1001
			snap.Screen = "❯ " + expected + "\n"
			if expected == "" {
				snap.Screen = "Answer\n❯ \n? for shortcuts\n"
			}
			return snap, nil
		},
		send: func(_ context.Context, _, value string, literal bool) error {
			if literal {
				*sent, typed = append(*sent, "text:"+value), value
				return nil
			}
			*sent = append(*sent, "Enter")
			line, _ := json.Marshal(map[string]any{"type": "user", "timestamp": now.UTC().Format(time.RFC3339Nano), "message": map[string]any{"role": "user", "content": typed}})
			appendClaudeRecords(t, transcript, string(line))
			return nil
		},
		sleep: func(d time.Duration) { *now = now.Add(d) },
		now:   func() time.Time { return *now },
	}
}

// Bug wi_85b4b3b61a6d8655, a1: a transcript longer than the old 32768-record
// read cap, ending in a completed turn, is woken exactly once and the intent
// records why that was safe. The owner helper's transcript passed 32768
// records at 16:49:06Z on 2026-10-01 and was skipped from then on.
func TestClaudeWakeLongTranscriptWokenOnce(t *testing.T) {
	now := time.Date(2026, 10, 1, 17, 0, 0, 0, time.UTC)
	b, transcript := claudeTranscriptFixture(t, "queue-idle")
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	// Bookkeeping records Claude Code writes after a turn, as in the incident.
	filler := strings.Repeat(`{"type":"mode","mode":"default","sessionId":"11111111-1111-4111-8111-111111111111"}`+"\n", 33000)
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(filler); err != nil {
		t.Fatal(err)
	}
	f.Close()
	snap, err := claudeTranscriptSnapshot(b, now)
	if err != nil || !claudeWakeIdle(snap, now) {
		t.Fatalf("long transcript refused: %v", err)
	}
	info, _ := os.Stat(transcript)
	if snap.Offset != info.Size() || !strings.Contains(snap.Safe, "transcript read to its end (33") || !strings.Contains(snap.Safe, fmt.Sprintf("%d bytes", info.Size())) {
		t.Fatalf("snapshot offset=%d size=%d safe=%q", snap.Offset, info.Size(), snap.Safe)
	}
	var sent []string
	prompt := "Tailterm messages #19684. Run tt inbox --unread --mark-read."
	for pass := 0; pass < 3; pass++ {
		if err := claudeWakeWith(context.Background(), b, prompt, transcriptWakeOps(t, transcript, &now, &sent)); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		now = now.Add(15 * time.Second)
	}
	if len(sent) != 2 || sent[0] != "text:"+prompt || sent[1] != "Enter" {
		t.Fatalf("long transcript not woken exactly once: %q", sent)
	}
	var saved claudeWakeIntent
	data, _ := os.ReadFile(claudeWakePath(b))
	if json.Unmarshal(data, &saved) != nil || saved.Phase != "confirmed" || !strings.HasPrefix(saved.Safe, "turn complete, no pending tool call, no fresh queued input; transcript read to its end (33") || !strings.HasSuffix(saved.Safe, "; pane input empty") {
		t.Fatalf("intent does not record why the wake was safe: %s", data)
	}
	if _, err := os.Stat(claudeWakeSkipPath(b)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed wake left a skip record: %v", err)
	}
}

// a1: the read is bounded by bytes, with its own reason, and a partial final
// record still refuses as incomplete.
func TestClaudeTranscriptReadBound(t *testing.T) {
	now := time.Date(2026, 10, 1, 17, 0, 0, 0, time.UTC)
	t.Run("a check stops at the bound and the next continues", func(t *testing.T) {
		b, transcript := claudeTranscriptFixture(t, "queue-idle")
		old := claudeTranscriptReadMax
		claudeTranscriptReadMax = 1 << 20
		t.Cleanup(func() { claudeTranscriptReadMax = old })
		appendClaudeRecords(t, transcript, strings.TrimSuffix(strings.Repeat(`{"type":"mode","mode":"default"}`+"\n", (3<<19)/33), "\n"))
		var sent []string
		t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
		prompt := "Tailterm messages #1. Run tt inbox --unread --mark-read."
		err := claudeWakeWith(context.Background(), b, prompt, transcriptWakeOps(t, transcript, &now, &sent))
		if !errors.Is(err, errClaudeWakeUnsafe) || !strings.HasSuffix(err.Error(), ": transcript read reached the 1 MiB bound for one check; continues on the next") || len(sent) != 0 {
			t.Fatalf("first check: sent=%q err=%v", sent, err)
		}
		info, _ := os.Stat(transcript)
		snap, err := claudeTranscriptSnapshot(b, now)
		if err != nil || snap.Offset != info.Size() || snap.ReadBytes <= 0 || snap.ReadBytes >= info.Size()-(1<<20)+(64<<10) {
			t.Fatalf("second check did not continue from the first: read=%d size=%d err=%v", snap.ReadBytes, info.Size(), err)
		}
	})
	t.Run("partial final record", func(t *testing.T) {
		b, transcript := claudeTranscriptFixture(t, "queue-idle")
		f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(`{"type":"user","message":{"content":"half a rec`); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if _, err := claudeTranscriptSnapshot(b, now); err == nil || !strings.HasSuffix(err.Error(), ": transcript incomplete") {
			t.Fatalf("partial record: %v", err)
		}
	})
}

// The idle check keeps its place per run: a later check reads only what was
// appended, and turn state, pending tool calls and queued input stay correct
// across checks. A replaced transcript starts again from zero.
func TestClaudeTranscriptCachedCursor(t *testing.T) {
	now := time.Date(2026, 10, 1, 16, 50, 0, 0, time.UTC)
	b, transcript := claudeTranscriptFixture(t, "queue-idle")
	size := func() int64 {
		info, err := os.Stat(transcript)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	check := func(step, wantSuffix string, wantRead int64) claudeWakeSnapshot {
		t.Helper()
		snap, err := claudeTranscriptSnapshot(b, now)
		switch {
		case wantSuffix == "" && err != nil:
			t.Fatalf("%s: refused: %v", step, err)
		case wantSuffix != "" && (err == nil || !strings.HasSuffix(err.Error(), ": "+wantSuffix)):
			t.Fatalf("%s: want %q, got %v", step, wantSuffix, err)
		case err == nil && (snap.ReadBytes != wantRead || snap.Offset != size()):
			t.Fatalf("%s: read %d bytes to offset %d, want %d to %d", step, snap.ReadBytes, snap.Offset, wantRead, size())
		}
		return snap
	}
	whole := size()
	check("first check reads the whole file", "", whole)
	check("unchanged file reads nothing", "", 0)

	appendClaudeRecords(t, transcript, claudeBgLaunch)
	check("tool call starts a turn", "turn in progress", 0)
	appendClaudeRecords(t, transcript, claudeBgEndTurn)
	check("tool call kept across checks", "tool call pending: Bash", 0)
	before := size()
	appendClaudeRecords(t, transcript, claudeBgLaunchResult)
	if snap := check("tool result and turn end", "", size()-before); len(snap.Cursor.Pending) != 0 {
		t.Fatalf("pending after result: %v", snap.Cursor.Pending)
	}

	appendClaudeRecords(t, transcript, strings.Replace(claudeBgEnqueue, "2026-10-01T16:41:00.000Z", "2026-10-01T16:49:55.000Z", 1))
	check("queued input across checks", "queued input pending (1)", 0)
	before = size()
	appendClaudeRecords(t, transcript, claudeBgDequeue, claudeBgNotification)
	check("notification turn", "turn in progress", 0)
	appendClaudeRecords(t, transcript, claudeBgEndTurn, claudeBgTurnDuration)
	if snap := check("notification turn ended", "", size()-before-int64(len(claudeBgDequeue)+len(claudeBgNotification)+2)); snap.Cursor.ClaudeQueued != 0 {
		t.Fatalf("queued count %d", snap.Cursor.ClaudeQueued)
	}

	// A record written in two parts refuses until its newline arrives.
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	half := len(claudeBgTaskStatus) / 2
	if _, err := f.WriteString(claudeBgTaskStatus[:half]); err != nil {
		t.Fatal(err)
	}
	check("partial final record", "transcript incomplete", 0)
	check("partial final record again", "transcript incomplete", 0)
	if _, err := f.WriteString(claudeBgTaskStatus[half:] + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	check("record completed", "", int64(len(claudeBgTaskStatus)-half+1))

	// A malformed record after the last completed turn refuses on every
	// check until a later completed turn supersedes it.
	appendClaudeRecords(t, transcript, `{"type":"user","message":`)
	_, malformed := claudeTranscriptSnapshot(b, now)
	_, again := claudeTranscriptSnapshot(b, now)
	if malformed == nil || again == nil || malformed.Error() != again.Error() || strings.Contains(malformed.Error(), "turn in progress") {
		t.Fatalf("malformed record not carried across checks: %v / %v", malformed, again)
	}
	before = size()
	appendClaudeRecords(t, transcript, claudeBgNotification, claudeBgEndTurn, claudeBgTurnDuration)
	check("completed turn after a malformed record", "", size()-before)

	// A restarted relay has no cache: it reads from zero to the same state.
	records := check("before restart", "", 0).Safe
	claudeTranscriptCache.Lock()
	claudeTranscriptCache.runs = nil
	claudeTranscriptCache.Unlock()
	if snap := check("after restart", "", size()); snap.Safe != records {
		t.Fatalf("restart read %q, cached read %q", snap.Safe, records)
	}

	// An ended context stops a long read between passes and keeps its place.
	appendClaudeRecords(t, transcript, strings.TrimSuffix(strings.Repeat(`{"type":"mode","mode":"default"}`+"\n", 600), "\n"))
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := claudeTranscriptSnapshotContext(ended, b, now); err == nil || !strings.HasSuffix(err.Error(), ": transcript read interrupted; continues on the next check") {
		t.Fatalf("ended context: %v", err)
	}
	check("continues after the interruption", "", 344*33)

	// The caller's copy does not alias the cached state.
	snap := check("copy", "", 0)
	snap.Cursor.Pending["toolu_alias"] = pendingActivityCall{Name: "Bash"}
	snap.Cursor.ClaudeUsage["msg_alias"] = api.TokenTotals{Total: 1}
	check("after mutating the copy", "", 0)

	// A different file at the same path, longer than the old one, is read
	// from zero: it ends mid-turn although the old file ended idle.
	data, err := os.ReadFile(filepath.Join("testdata", "claude-transcript", "queue-busy.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	padded := strings.Repeat(`{"type":"mode","mode":"default"}`+"\n", int(size())/33+1) + string(data)
	swap := transcript + ".new"
	if err := os.WriteFile(swap, []byte(padded), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(swap, transcript); err != nil {
		t.Fatal(err)
	}
	check("replaced by a longer file", "turn in progress", 0)
	appendClaudeRecords(t, transcript, claudeBgEndTurn, claudeBgTurnDuration)
	check("replaced file completes", "", int64(len(claudeBgEndTurn)+len(claudeBgTurnDuration)+2))

	// A shorter file at the same path is a different transcript.
	if int64(len(data)) >= size() {
		t.Fatalf("queue-busy fixture unusable: %d bytes, %v", len(data), err)
	}
	if err := os.WriteFile(transcript, data, 0600); err != nil {
		t.Fatal(err)
	}
	check("replaced transcript", "turn in progress", 0)
}

// Record shapes Claude Code 2.1.x writes around a tracked background task,
// taken from the owner helper transcript with content removed: the launch, the
// queued <task-notification> prompt, and the bookkeeping after a turn.
const (
	claudeBgLaunch       = `{"type":"assistant","timestamp":"2026-10-01T16:40:00.000Z","message":{"id":"msg_bg0001","role":"assistant","content":[{"type":"tool_use","id":"toolu_bg0001","name":"Bash","input":{"command":"fixture watcher","run_in_background":true}}],"stop_reason":"tool_use"}}`
	claudeBgLaunchResult = `{"type":"user","timestamp":"2026-10-01T16:40:00.200Z","toolUseResult":{"backgroundTaskId":"bg_fixture"},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_bg0001","content":"Command running in background with ID: bg_fixture"}]}}`
	claudeBgEndTurn      = `{"type":"assistant","timestamp":"2026-10-01T16:40:01.000Z","message":{"id":"msg_bg0002","role":"assistant","content":[{"type":"text","text":"Fixture answer."}],"stop_reason":"end_turn"}}`
	claudeBgTurnDuration = `{"type":"system","timestamp":"2026-10-01T16:40:01.100Z","subtype":"turn_duration","durationMs":1100}`
	claudeBgEnqueue      = `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-01T16:41:00.000Z","content":"fixture"}`
	claudeBgDequeue      = `{"type":"queue-operation","operation":"dequeue","timestamp":"2026-10-01T16:41:00.010Z"}`
	claudeBgNotification = `{"type":"user","timestamp":"2026-10-01T16:41:00.020Z","origin":{"kind":"task-notification"},"isSidechain":false,"message":{"role":"user","content":"<task-notification>\n<task-id>bg_fixture</task-id>\n<status>running</status>\n</task-notification>"}}`
	claudeBgTaskStatus   = `{"type":"attachment","timestamp":"2026-10-01T16:41:02.000Z","attachment":{"type":"task_status","taskId":"bg_fixture","status":"running"}}`
	claudeBgFrameLink    = `{"type":"frame-link","timestamp":"2026-10-01T16:41:02.100Z"}`
	claudeBgStopHook     = `{"type":"system","timestamp":"2026-10-01T16:41:01.050Z","subtype":"stop_hook_summary","hookCount":1}`
)

// a1/a3: background-task records after a completed turn do not block a wake;
// a notification turn still in progress does.
func TestClaudeWakeBackgroundTaskRecords(t *testing.T) {
	prompt := "Tailterm messages #19700. Run tt inbox --unread --mark-read."
	running := []string{claudeBgLaunch, claudeBgLaunchResult, claudeBgEndTurn, claudeBgStopHook, claudeBgTurnDuration, claudeBgTaskStatus, claudeBgFrameLink}
	notified := append(slices.Clone(running), claudeBgEnqueue, claudeBgDequeue, claudeBgNotification, claudeBgEndTurn, claudeBgStopHook, claudeBgTurnDuration,
		`{"type":"last-prompt"}`, `{"type":"ai-title"}`, `{"type":"mode"}`, `{"type":"atis-latch"}`, claudeBgFrameLink, claudeBgTaskStatus)
	for name, extra := range map[string][]string{"task running after a completed turn": running, "after a task notification turn": notified} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 1, 16, 50, 0, 0, time.UTC)
			b, transcript := claudeTranscriptFixture(t, "queue-idle", extra...)
			t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
			var sent []string
			for pass := 0; pass < 3; pass++ {
				if err := claudeWakeWith(context.Background(), b, prompt, transcriptWakeOps(t, transcript, &now, &sent)); err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
				now = now.Add(15 * time.Second)
			}
			if len(sent) != 2 || sent[0] != "text:"+prompt || sent[1] != "Enter" {
				t.Fatalf("not woken exactly once: %q", sent)
			}
		})
	}
	t.Run("notification turn in progress", func(t *testing.T) {
		now := time.Date(2026, 10, 1, 16, 50, 0, 0, time.UTC)
		b, transcript := claudeTranscriptFixture(t, "queue-idle", append(slices.Clone(running), claudeBgEnqueue, claudeBgDequeue, claudeBgNotification)...)
		t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
		var sent []string
		err := claudeWakeWith(context.Background(), b, prompt, transcriptWakeOps(t, transcript, &now, &sent))
		if !errors.Is(err, errClaudeWakeUnsafe) || !strings.HasSuffix(err.Error(), ": turn in progress") || len(sent) != 0 {
			t.Fatalf("busy notification turn: sent=%q err=%v", sent, err)
		}
	})
}

// a2/a3: a truly busy session stays skipped, and a skip that keeps one reason
// past the bound escalates exactly once, naming the session and the reason.
func TestClaudeWakeSkipEscalatesOnce(t *testing.T) {
	prompt := "Tailterm messages #19684. Run tt inbox --unread --mark-read."
	type call struct {
		b    runtimeBinding
		skip claudeWakeSkip
	}
	tries := 0
	setup := func(t *testing.T) (runtimeBinding, *time.Time, *[]string, *[]call, *error, func() error) {
		tries = 0
		now := time.Date(2026, 10, 1, 16, 50, 0, 0, time.UTC)
		b, transcript := claudeTranscriptFixture(t, "queue-busy")
		t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
		var sent []string
		var calls []call
		var fail error
		attempt := func() error {
			ops := transcriptWakeOps(t, transcript, &now, &sent)
			ops.escalate = func(_ context.Context, b runtimeBinding, skip claudeWakeSkip) error {
				tries++
				if fail != nil {
					return fail
				}
				calls = append(calls, call{b, skip})
				return nil
			}
			return claudeWakeWith(context.Background(), b, prompt, ops)
		}
		return b, &now, &sent, &calls, &fail, attempt
	}
	t.Run("busy past the bound", func(t *testing.T) {
		b, now, sent, calls, _, attempt := setup(t)
		start := *now
		for now.Sub(start) < 40*time.Minute {
			if err := attempt(); !errors.Is(err, errClaudeWakeUnsafe) || !strings.HasSuffix(err.Error(), ": turn in progress") {
				t.Fatalf("busy session not skipped: %v", err)
			}
			if now.Sub(start) < claudeWakeSkipBound && len(*calls) != 0 {
				t.Fatalf("escalated after %s, before the bound", now.Sub(start))
			}
			*now = now.Add(15 * time.Second)
		}
		if len(*sent) != 0 {
			t.Fatalf("busy session received input: %q", *sent)
		}
		if len(*calls) != 1 {
			t.Fatalf("escalations = %d, want exactly 1", len(*calls))
		}
		got := (*calls)[0]
		if got.b.Agent != b.Agent || got.b.Session != b.Session || got.b.Run != b.Run || !got.skip.Since.Equal(start) || got.skip.Reason != "Claude transcript busy, incomplete, or unknown: turn in progress" {
			t.Fatalf("escalation %+v", got)
		}
	})
	t.Run("failed report is retried, then sent once", func(t *testing.T) {
		_, now, _, calls, fail, attempt := setup(t)
		*fail = errors.New("hub: 403 identity unavailable: invalid hub token")
		for i := 0; i < 60; i++ { // 15 minutes of skips, 5 past the bound
			_ = attempt()
			*now = now.Add(15 * time.Second)
		}
		// 20 skips fall past the bound; a failed report is tried once a minute.
		if tries != 5 {
			t.Fatalf("failed report tried %d times in five minutes, want 5", tries)
		}
		*fail = nil
		for i := 0; i < 20; i++ {
			_ = attempt()
			*now = now.Add(15 * time.Second)
		}
		if len(*calls) != 1 {
			t.Fatalf("escalations after a failed report = %d, want 1", len(*calls))
		}
	})
	t.Run("a changed reason or a quiet gap starts a new episode", func(t *testing.T) {
		b, now, _, calls, _, attempt := setup(t)
		for i := 0; i < 36; i++ { // 9 minutes
			_ = attempt()
			*now = now.Add(15 * time.Second)
		}
		// The agent read its inbox; the relay had nothing to wake it for.
		*now = now.Add(claudeWakeSkipGap + time.Second)
		gapStart := *now
		for i := 0; i < 36; i++ {
			_ = attempt()
			*now = now.Add(15 * time.Second)
		}
		if len(*calls) != 0 {
			t.Fatalf("two short episodes escalated: %+v", *calls)
		}
		var skip claudeWakeSkip
		data, _ := os.ReadFile(claudeWakeSkipPath(b))
		if json.Unmarshal(data, &skip) != nil || !skip.Since.Equal(gapStart) {
			t.Fatalf("episode did not restart after the gap: %s", data)
		}
		claudeWakeTrackSkip(context.Background(), b, claudeWakeOps{now: func() time.Time { return *now }}, fmt.Errorf("%w: Claude pane has a permission or selection prompt", errClaudeWakeUnsafe))
		data, _ = os.ReadFile(claudeWakeSkipPath(b))
		if json.Unmarshal(data, &skip) != nil || !skip.Since.Equal(*now) || skip.Reason != "Claude pane has a permission or selection prompt" {
			t.Fatalf("episode did not restart on a new reason: %s", data)
		}
		// An unconfirmed wake is not an unsafe skip and leaves the episode alone.
		*now = now.Add(time.Minute)
		claudeWakeTrackSkip(context.Background(), b, claudeWakeOps{now: func() time.Time { return *now }}, errors.New("Claude new user turn did not confirm within five seconds"))
		after, _ := os.ReadFile(claudeWakeSkipPath(b))
		if string(after) != string(data) {
			t.Fatalf("unconfirmed wake changed the skip record: %s", after)
		}
	})
}

// a2: against an isolated hub that requires its token, as production does,
// the escalation is exactly one notice to the database handler. When the
// skipped agent is that handler it goes to the owner helper, and with neither
// to the Board. A wrong token fails and posts nothing.
func TestClaudeWakeSkipEscalationNotice(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "cli-test", User: "owner"}
	task, err := st.CreateTask(ctx, api.CreateTaskRequest{Name: "p", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "database", Host: "h", Session: "database", Runtime: "claude", Role: api.AgentRoleDatabaseHandler}, by)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "builder", Host: "h", Session: "builder", Runtime: "claude"}, by)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("t", 40)
	identity, err := server.TokenIdentity(token, by)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, identity))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	config := func(token string) {
		t.Helper()
		data, _ := json.Marshal(map[string]string{"url": srv.URL, "token": token})
		if err := os.MkdirAll(filepath.Join(home, ".config", "tailterm"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".config", "tailterm", "hub.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := api.NewClient(srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.Token = token
	notices := func() []api.Message {
		t.Helper()
		all, err := c.ListMessages(ctx, task.ID, 0, "", 200)
		if err != nil {
			t.Fatal(err)
		}
		var out []api.Message
		for _, m := range all {
			if m.Envelope != nil && m.Envelope.Kind == api.EnvelopeKindNotice {
				out = append(out, m)
			}
		}
		return out
	}
	binding := func(a api.Agent) runtimeBinding {
		b := testClaudeBinding()
		b.Hub, b.Task, b.Agent, b.Session = srv.URL, task.ID, a.ID, a.Session
		return b
	}
	since := time.Date(2026, 10, 1, 16, 50, 0, 0, time.UTC)
	skip := claudeWakeSkip{Run: testClaudeBinding().Run, Reason: "Claude transcript busy, incomplete, or unknown: turn in progress", Since: since}

	config(strings.Repeat("x", 40))
	if err := nativeClaudeEscalate(ctx, binding(worker), skip); err == nil || !strings.Contains(err.Error(), "403") || len(notices()) != 0 {
		t.Fatalf("wrong token: err=%v notices=%d", err, len(notices()))
	}
	config(token)
	for i := 0; i < 2; i++ { // the second stands in for a retry or a restarted relay
		if err := nativeClaudeEscalate(ctx, binding(worker), skip); err != nil {
			t.Fatalf("escalation %d: %v", i, err)
		}
	}
	got := notices()
	if len(got) != 1 || got[0].To != handler.ID || got[0].Envelope.To != handler.Name {
		t.Fatalf("want exactly one notice to the handler, got %d: %+v", len(got), got)
	}
	for _, want := range []string{worker.ID, skip.Run, worker.Session, skip.Reason, "2026-10-01T16:50:00Z"} {
		if !strings.Contains(got[0].Text, want) {
			t.Fatalf("notice lacks %q: %s", want, got[0].Text)
		}
	}

	// The handler itself is the agent that cannot be woken.
	skip.Since = since.Add(time.Hour)
	if err := nativeClaudeEscalate(ctx, binding(handler), skip); err != nil {
		t.Fatal(err)
	}
	if got = notices(); len(got) != 2 || got[1].To != "" || got[1].Envelope.To != "" || !strings.Contains(got[1].Text, handler.ID) {
		t.Fatalf("handler skip with no owner helper should reach the Board: %+v", got)
	}
}

// a2: the recipient is the newest live handler, else the newest live owner
// helper, never the skipped agent, else the Board.
func TestClaudeEscalationRecipient(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	agents := []api.Agent{
		{ID: "agt_old", Name: "db-handler", Role: api.AgentRoleDatabaseHandler, Status: api.AgentClosed, CreatedAt: at},
		{ID: "agt_h1", Name: "db-handler-s1", Role: api.AgentRoleDatabaseHandler, Status: "done", CreatedAt: at.Add(time.Hour)},
		{ID: "agt_h2", Name: "db-handler-s2", Role: api.AgentRoleDatabaseHandler, Status: "running", CreatedAt: at.Add(2 * time.Hour)},
		{ID: "agt_helper", Name: "owner-helper", Role: api.AgentRoleOwnerHelper, Status: "running", CreatedAt: at},
		{ID: "agt_retired", Name: "old-helper", Role: api.AgentRoleOwnerHelper, Status: api.AgentRetired, CreatedAt: at.Add(3 * time.Hour)},
		{ID: "agt_worker", Name: "builder", Status: "running", CreatedAt: at},
	}
	for skipped, want := range map[string]string{"agt_worker": "agt_h2", "agt_helper": "agt_h2", "agt_h2": "agt_h1"} {
		if got := claudeEscalationRecipient(agents, skipped); got.ID != want {
			t.Fatalf("skipped %s: recipient %q, want %q", skipped, got.ID, want)
		}
	}
	only := []api.Agent{agents[0], agents[2], agents[3], agents[4], agents[5]}
	if got := claudeEscalationRecipient(only, "agt_h2"); got.ID != "agt_helper" {
		t.Fatalf("handler skipped: recipient %q, want the owner helper", got.ID)
	}
	if got := claudeEscalationRecipient([]api.Agent{agents[0], agents[2], agents[4], agents[5]}, "agt_h2"); got.ID != "" {
		t.Fatalf("no one else live: recipient %q, want the Board", got.ID)
	}
}
