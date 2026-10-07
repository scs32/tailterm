package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Bug wi_03ce50892a559767, order #27407, assignment #27506: stalled Claude
// turns. Nothing here touches a live pane: transcripts are the sanitized
// captures in testdata/claude-transcript, pane captures are the files in
// testdata/claude-pane, and every key or text "sent" lands in a slice.

// stallLastOutput is the timestamp of the last record of the open turn in the
// interrupt-after-tool fixture, a tool result: after it the turn is thinking.
var stallLastOutput = time.Date(2026, 10, 7, 6, 54, 26, 285000000, time.UTC)

// stallOpenTurn returns the interrupt-after-tool fixture without its last
// record, and that record: the one Claude Code wrote when Escape was pressed.
func stallOpenTurn(t *testing.T) (open string, interrupt string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "claude-transcript", "interrupt-after-tool.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	return strings.Join(lines[:len(lines)-1], "\n") + "\n", lines[len(lines)-1]
}

// stallFixture is one stalled agent: a real transcript the activity parser
// reads, a pane that shows fixture captures, and recorders for every key,
// typed text and notice.
type stallFixture struct {
	t          *testing.T
	b          runtimeBinding
	transcript string
	interrupt  string
	now        time.Time
	state      activityCursor
	agent      api.Agent
	agentReads int

	raw      string
	cx, cy   int
	pane     string
	captures int
	// beforeCapture runs before the nth capture (1-based).
	beforeCapture func(n int)
	captureErr    error

	// escapeEnds: Claude Code answers Escape by writing the interrupt record.
	escapeEnds   bool
	interruptErr error
	keys         []string
	sent         []string
	notices      []claudeStall
	notifyErrs   []error
}

func newStallFixture(t *testing.T) *stallFixture {
	t.Helper()
	b, transcript := claudeTranscriptFixture(t, "interrupt-after-tool")
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	open, interrupt := stallOpenTurn(t)
	if err := os.WriteFile(transcript, []byte(open), 0600); err != nil {
		t.Fatal(err)
	}
	f := &stallFixture{t: t, b: b, transcript: transcript, interrupt: interrupt, now: stallLastOutput.Add(16 * time.Minute), pane: "%7", escapeEnds: true,
		agent: api.Agent{ID: b.Agent, RunID: b.Run, Status: api.AgentRunning, Online: true, Runtime: "claude", Session: b.Session}}
	f.screen("spinner-stalled-1")
	return f
}

func (f *stallFixture) screen(name string) {
	f.t.Helper()
	f.raw, f.cx, f.cy = readClaudePaneFixture(f.t, name)
}

// setAction writes the host setting file as given; "" removes it.
func (f *stallFixture) setAction(content string) {
	f.t.Helper()
	path := filepath.Join(os.Getenv("HOME"), ".config", "tailterm", "relay.json")
	if content == "" {
		_ = os.Remove(path)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *stallFixture) cursor(runtimeBinding) (activityCursor, bool) {
	for {
		before := f.state.Offset
		if err := readActivityAppend(f.transcript, &f.state, parseClaudeActivity); err != nil {
			f.t.Fatal(err)
		}
		if f.state.Ready || f.state.Offset == before {
			break
		}
	}
	c := f.state
	c.Run, c.Thread, c.LastCheck = f.b.Run, f.b.Thread, f.now
	return c, true
}

func (f *stallFixture) wakeOps() claudeWakeOps {
	return transcriptWakeOps(f.t, f.transcript, &f.now, &f.sent)
}

func (f *stallFixture) ops() claudeStallOps {
	return claudeStallOps{
		now:    func() time.Time { return f.now },
		sleep:  func(d time.Duration) { f.now = f.now.Add(d) },
		cursor: f.cursor,
		agent: func(context.Context, runtimeBinding) (api.Agent, error) {
			f.agentReads++
			return f.agent, nil
		},
		capture: func(context.Context, runtimeBinding) (runtimePane, error) {
			f.captures++
			if f.beforeCapture != nil {
				f.beforeCapture(f.captures)
			}
			if f.captureErr != nil {
				return runtimePane{}, f.captureErr
			}
			return runtimePane{Pane: f.pane, Raw: f.raw, CursorX: f.cx, CursorY: f.cy}, nil
		},
		interrupt: func(_ context.Context, pane string) error {
			f.keys = append(f.keys, pane)
			if f.interruptErr != nil {
				return f.interruptErr
			}
			if f.escapeEnds {
				appendClaudeRecords(f.t, f.transcript, f.interrupt)
			}
			return nil
		},
		ended: func(_ context.Context, b runtimeBinding) bool {
			_, err := claudeTranscriptSnapshot(b, f.now)
			return err == nil
		},
		wake: func(ctx context.Context, b runtimeBinding, prompt string) error {
			return claudeWakeWith(ctx, b, prompt, f.wakeOps())
		},
		notify: func(_ context.Context, _ runtimeBinding, s claudeStall) error {
			f.notices = append(f.notices, s)
			if len(f.notifyErrs) > 0 {
				err := f.notifyErrs[0]
				f.notifyErrs = f.notifyErrs[1:]
				return err
			}
			return nil
		},
		action: relayStallAction,
	}
}

// pass runs one stall pass and returns what it logged.
func (f *stallFixture) pass() string {
	f.t.Helper()
	out, _ := captureRelayOutput(f.t, true, func() error {
		claudeStallPass(context.Background(), f.b, f.ops())
		return nil
	})
	return out
}

func (f *stallFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

// detect runs the two passes a stall needs: a first reading, and a second one
// from the named capture just over a minute later.
func (f *stallFixture) detect(second string) string {
	f.t.Helper()
	out := f.pass()
	f.advance(claudeStallProbeGap + time.Second)
	if second != "" {
		f.screen(second)
	}
	return out + f.pass()
}

func (f *stallFixture) record() claudeStall {
	f.t.Helper()
	return loadClaudeStall(f.b)
}

// untouched fails the test when any key or text reached the pane.
func (f *stallFixture) untouched(t *testing.T, what string) {
	t.Helper()
	if len(f.keys) != 0 || len(f.sent) != 0 {
		t.Fatalf("%s: the pane was touched: keys=%q text=%q", what, f.keys, f.sent)
	}
}

// appendAssistant adds a completed thinking block at the given time: output
// from a turn that is still running.
func (f *stallFixture) appendAssistant(at time.Time) {
	f.t.Helper()
	line, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": at.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"id": "msg_fixture_progress", "role": "assistant", "model": "claude-opus-5-5", "stop_reason": nil, "content": []map[string]any{{"type": "thinking", "thinking": "", "signature": ""}}}})
	appendClaudeRecords(f.t, f.transcript, string(line))
}

// The status line is read only from a busy pane that passes the wake's own
// checks, and only when it shows a token counter.
func TestClaudeStallCounterFixtures(t *testing.T) {
	for _, tc := range []struct{ fixture, counter, refusal string }{
		{"spinner-progress-1", "14", ""},
		{"spinner-progress-2", "339", ""},
		{"spinner-after-tool", "75", ""},
		{"spinner-stalled-1", "75", ""},
		{"spinner-stalled-2", "75", ""},
		{"spinner-no-counter", "", "status line shows no token counter"},
		{"streaming-no-status", "", "no status line above the input box"},
		{"interrupted", "", "footer does not say esc to interrupt"},
		{"interrupted-prompt-restored", "", "Claude cursor is not at an empty input"},
		{"permission-dialog", "", "permission or selection prompt"},
		{"selection-dialog", "", "permission or selection prompt"},
		{"typed", "", "Claude cursor is not at an empty input"},
		{"scrollback-dialog-words", "", "footer does not say esc to interrupt"},
	} {
		raw, x, y := readClaudePaneFixture(t, tc.fixture)
		counter, err := claudeStallCounter(raw, x, y)
		if counter != tc.counter || (tc.refusal == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.refusal)) {
			t.Errorf("%s: counter %q err %v, want %q %q", tc.fixture, counter, err, tc.counter, tc.refusal)
		}
	}
	for line, want := range map[string]string{
		"✻ Deliberating… (17m 30s · ↓ 9.5k tokens · esc to interrupt)": "9.5k",
		"· Thinking… (1m 2s · ↑ 1.2M tokens)":                          "1.2M",
		"✶ Schlepping… (3s · thinking with high effort)":               "",
		"✻ Baked for 10s · done 11:52 PM":                              "",
	} {
		raw := "answer\n" + line + "\n\n" + fakeClaudeRule + "\n\x1b[39m❯ \n" + fakeClaudeRule + "\n\x1b[39m  ⏵⏵ bypass permissions on · esc to interrupt\n"
		if counter, _ := claudeStallCounter(raw, 2, 4); counter != want {
			t.Errorf("%q: counter %q, want %q", line, counter, want)
		}
	}
	// The same status line in the scrollback of an idle pane is not read.
	idle := "✻ Deliberating… (17m 30s · ↓ 9.5k tokens)\n\n" + fakeClaudeRule + "\n\x1b[39m❯ \n" + fakeClaudeRule + "\n\x1b[39m  ⏵⏵ bypass permissions on\n"
	if counter, err := claudeStallCounter(idle, 2, 3); counter != "" || err == nil {
		t.Fatalf("idle pane read as busy: %q %v", counter, err)
	}
}

// h1 (s1, s5): an open turn with no pending tool call and nothing new for a
// minute past the threshold, on a host in report mode, gives one stall record
// and one log line, and the real activity tick reports stuck with the reason.
func TestClaudeStallDetected(t *testing.T) {
	cols, rows := 200, 50
	stubPaneSize(t, &cols, &rows)
	b, hub, client, tick := stuckFixture(t, "claude")
	open, _ := stallOpenTurn(t)
	transcript := filepath.Join(os.Getenv("HOME"), ".claude", "projects", "p", b.Thread+".jsonl")
	if err := os.WriteFile(transcript, []byte(open), 0600); err != nil {
		t.Fatal(err)
	}
	now := stallLastOutput.Add(16 * time.Minute)
	raw, cx, cy := readClaudePaneFixture(t, "spinner-stalled-1")
	var keys, typed []string
	var notices []claudeStall
	// The native ops, with only the clock, the pane and the outputs replaced:
	// the cursor is the file the activity tick wrote, the agent comes from the
	// hub and the setting from this home's (absent) relay.json.
	ops := nativeClaudeStallOps(client)
	ops.now = func() time.Time { return now }
	ops.sleep = func(time.Duration) {}
	ops.capture = func(context.Context, runtimeBinding) (runtimePane, error) {
		return runtimePane{Pane: "%7", Raw: raw, CursorX: cx, CursorY: cy}, nil
	}
	ops.interrupt = func(_ context.Context, pane string) error { keys = append(keys, pane); return nil }
	ops.wake = func(_ context.Context, _ runtimeBinding, prompt string) error {
		typed = append(typed, prompt)
		return nil
	}
	ops.notify = func(_ context.Context, _ runtimeBinding, s claudeStall) error {
		notices = append(notices, s)
		return nil
	}
	pass := func() string {
		out, _ := captureRelayOutput(t, true, func() error { claudeStallPass(context.Background(), b, ops); return nil })
		return out
	}

	if err := tick(now); err != nil {
		t.Fatal(err)
	}
	if len(hub.reports) != 1 || hub.reports[0].State != "unknown" || hub.reports[0].Reason != "no recent execution evidence" {
		t.Fatalf("before detection the quiet turn reports %+v", hub.reports)
	}
	if out := pass(); out != "" || loadClaudeStall(b).active() || loadClaudeStall(b).Probe == nil {
		t.Fatalf("first reading must only be recorded: log %q record %+v", out, loadClaudeStall(b))
	}
	// Inside the minute between readings nothing is captured or decided.
	now = now.Add(30 * time.Second)
	if err := tick(now); err != nil {
		t.Fatal(err)
	}
	if out := pass(); out != "" || loadClaudeStall(b).active() {
		t.Fatalf("second reading taken too early: %q", out)
	}
	now = now.Add(31 * time.Second)
	if err := tick(now); err != nil {
		t.Fatal(err)
	}
	raw, cx, cy = readClaudePaneFixture(t, "spinner-stalled-2")
	out := pass()
	s := loadClaudeStall(b)
	if !s.active() || s.Count != 1 || s.Mode != claudeStallReport || s.Counter != "75" || !s.Since.Equal(stallLastOutput) || !s.DetectedAt.Equal(now) || !s.InterruptedAt.IsZero() {
		t.Fatalf("stall record %+v", s)
	}
	want := fmt.Sprintf("[tt relay] %s %s Claude turn stalled since 2026-10-07T06:54:26Z (stall 1 of run %s, mode report): no transcript record or worktree change for 17m, no tool call pending, token counter 75 unchanged from ", now.Format(time.RFC3339), b.Agent, b.Run)
	if strings.Count(out, "turn stalled since") != 1 || !strings.HasPrefix(out, want) {
		t.Fatalf("detection log:\n%s\nwant one line starting\n%s", out, want)
	}
	if lines := strings.Count(out, "\n"); lines != 2 || !strings.Contains(out, "stall notice posted once (stall 1 of the run)") {
		t.Fatalf("want the detection line and the notice line, got %d lines:\n%s", lines, out)
	}
	if len(notices) != 1 || len(keys) != 0 || len(typed) != 0 {
		t.Fatalf("report mode: notices=%d keys=%q typed=%q", len(notices), keys, typed)
	}
	// The activity tick now reports the stall as stuck, with its reason.
	now = now.Add(20 * time.Second)
	if err := tick(now); err != nil {
		t.Fatal(err)
	}
	if last := hub.reports[len(hub.reports)-1]; len(hub.reports) != 2 || last.State != "stuck" || last.Reason != "turn stalled: no output for 17m" {
		t.Fatalf("stalled turn reports %+v", hub.reports)
	}
	// Later passes add no record, no line, no notice and no report.
	for i := 0; i < 4; i++ {
		now = now.Add(40 * time.Second)
		if err := tick(now); err != nil {
			t.Fatal(err)
		}
		if out := pass(); out != "" {
			t.Fatalf("steady stall logged again: %q", out)
		}
	}
	if again := loadClaudeStall(b); again.Count != 1 || !again.DetectedAt.Equal(s.DetectedAt) || len(notices) != 1 || len(hub.reports) != 2 || len(keys)+len(typed) != 0 {
		t.Fatalf("steady stall changed: %+v notices=%d reports=%d", again, len(notices), len(hub.reports))
	}
	// Output ends the stall: the record is cleared and the state leaves stuck.
	line, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": now.Format(time.RFC3339Nano), "message": map[string]any{"id": "m", "stop_reason": "end_turn", "content": []any{}}})
	appendClaudeRecords(t, transcript, string(line))
	now = now.Add(20 * time.Second)
	if err := tick(now); err != nil {
		t.Fatal(err)
	}
	if out := pass(); !strings.Contains(out, "turn stall of 2026-10-07T06:54:26Z ended: the turn ended") || loadClaudeStall(b).active() {
		t.Fatalf("stall not cleared by output: %q %+v", out, loadClaudeStall(b))
	}
	if last := hub.reports[len(hub.reports)-1]; last.State != "idle" {
		t.Fatalf("after the turn ended the agent reports %+v", last)
	}
}

// h2 (s1, s5): a slow turn is never a stall. A pending tool call, however
// old, belongs to the hung-tool rule; a transcript that grew inside the
// threshold and a token counter that grew between captures are progress.
func TestClaudeStallIgnoresProgress(t *testing.T) {
	t.Run("pending tool call", func(t *testing.T) {
		f := newStallFixture(t)
		f.setAction(`{"claudeStallAction":"interrupt"}`)
		// Drop the tool result: the Bash call has been pending for 20 minutes.
		open, _ := stallOpenTurn(t)
		lines := strings.Split(strings.TrimRight(open, "\n"), "\n")
		if err := os.WriteFile(f.transcript, []byte(strings.Join(lines[:len(lines)-1], "\n")+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		f.now = stallLastOutput.Add(20 * time.Minute)
		if out := f.detect("spinner-stalled-2"); out != "" {
			t.Fatalf("pending tool logged a stall: %q", out)
		}
		c, _ := f.cursor(f.b)
		if state := activityState(&c, f.agent, 0, true, true, nil, f.now, activityDefaults()); state.State != "hung_tool" || state.PendingTool != "Bash" {
			t.Fatalf("pending call should stay with the hung-tool rule: %+v", state)
		}
		if s := f.record(); s.Count != 0 || s.Probe != nil || f.captures != 0 || len(f.notices) != 0 {
			t.Fatalf("pending tool made a stall record %+v captures=%d", s, f.captures)
		}
		if _, err := os.Stat(claudeStallPath(f.b)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a record file was left behind: %v", err)
		}
		f.untouched(t, "pending tool")
	})
	t.Run("transcript grows", func(t *testing.T) {
		f := newStallFixture(t)
		f.setAction(`{"claudeStallAction":"interrupt"}`)
		if out := f.pass(); out != "" || f.record().Probe == nil {
			t.Fatalf("first reading: %q %+v", out, f.record())
		}
		f.advance(claudeStallProbeGap + time.Second)
		f.appendAssistant(f.now.Add(-10 * time.Second))
		f.screen("spinner-stalled-2")
		if out := f.pass(); out != "" {
			t.Fatalf("a turn that wrote a record was called stalled: %q", out)
		}
		if s := f.record(); s.Count != 0 || s.Probe != nil || len(f.notices) != 0 {
			t.Fatalf("progress left a stall record %+v", s)
		}
		// The threshold now runs from the new record.
		f.advance(14 * time.Minute)
		if out := f.detect("spinner-stalled-1"); out != "" || f.record().Count != 0 {
			t.Fatalf("stalled before the threshold passed again: %q", out)
		}
		f.untouched(t, "growing transcript")
	})
	t.Run("token counter grows", func(t *testing.T) {
		f := newStallFixture(t)
		f.setAction(`{"claudeStallAction":"interrupt"}`)
		f.screen("spinner-progress-1")
		if out := f.detect("spinner-progress-2"); out != "" {
			t.Fatalf("a growing token counter was called stalled: %q", out)
		}
		s := f.record()
		if s.Count != 0 || s.active() || s.Probe == nil || s.Probe.Counter != "339" || len(f.notices) != 0 {
			t.Fatalf("growing counter: record %+v", s)
		}
		f.untouched(t, "growing counter")
	})
	t.Run("worktree changes", func(t *testing.T) {
		f := newStallFixture(t)
		c, _ := f.cursor(f.b)
		c.WorktreeChangedAt = f.now.Add(-time.Minute)
		first := claudeStallReading{At: f.now.Add(-2 * time.Minute), Pane: "%7", Counter: "75"}
		second := claudeStallReading{At: f.now, Pane: "%7", Counter: "75"}
		if d := claudeStallDecide("claude", api.AgentRunning, c, &first, &second, f.now, 15*time.Minute); d.Quiet || d.Stalled {
			t.Fatalf("a worktree change a minute ago is progress: %+v", d)
		}
		c.WorktreeChangedAt = time.Time{}
		if d := claudeStallDecide("claude", api.AgentRunning, c, &first, &second, f.now, 15*time.Minute); !d.Stalled || !d.Interruptible || d.Counter != "75" || !d.Since.Equal(stallLastOutput) {
			t.Fatalf("the same readings without the change are a stall: %+v", d)
		}
		for _, status := range []string{api.AgentNeedsInput, api.AgentDone, api.AgentRetired} {
			if d := claudeStallDecide("claude", status, c, &first, &second, f.now, 15*time.Minute); d.Quiet || d.Stalled {
				t.Fatalf("status %s is not a running turn: %+v", status, d)
			}
		}
		if d := claudeStallDecide("codex", api.AgentRunning, c, &first, &second, f.now, 15*time.Minute); d.Quiet || d.Stalled {
			t.Fatalf("a Codex agent has no stall rule: %+v", d)
		}
		close := second
		close.At = first.At.Add(59 * time.Second)
		if d := claudeStallDecide("claude", api.AgentRunning, c, &first, &close, f.now, 15*time.Minute); !d.Quiet || d.Stalled {
			t.Fatalf("readings 59 s apart decided a stall: %+v", d)
		}
	})
}

// h3 (s1): TAILTERM_ACTIVITY_STALLED_SECONDS moves the threshold; values
// outside 300-86400 fall back to 900.
func TestClaudeStallThreshold(t *testing.T) {
	for value, want := range map[string]time.Duration{"": 900 * time.Second, "300": 300 * time.Second, "86400": 86400 * time.Second, "1200": 1200 * time.Second,
		"299": 900 * time.Second, "86401": 900 * time.Second, "0": 900 * time.Second, "-5": 900 * time.Second, "abc": 900 * time.Second, "600s": 900 * time.Second} {
		t.Setenv("TAILTERM_ACTIVITY_STALLED_SECONDS", value)
		if got := activityDefaults().Stalled; got != want {
			t.Errorf("TAILTERM_ACTIVITY_STALLED_SECONDS=%q gives %s, want %s", value, got, want)
		}
	}
	quietFor := func(env string, quiet time.Duration) (claudeStall, string) {
		t.Setenv("TAILTERM_ACTIVITY_STALLED_SECONDS", env)
		f := newStallFixture(t)
		f.now = stallLastOutput.Add(quiet)
		out := f.detect("spinner-stalled-2")
		return f.record(), out
	}
	if s, out := quietFor("", 10*time.Minute); s.Count != 0 || s.Probe != nil || out != "" {
		t.Fatalf("ten quiet minutes stalled under the 15-minute default: %+v %q", s, out)
	}
	if s, _ := quietFor("300", 6*time.Minute); !s.active() || s.Count != 1 {
		t.Fatalf("six quiet minutes did not stall under a 300 s threshold: %+v", s)
	}
	if s, _ := quietFor("1800", 20*time.Minute); s.Count != 0 {
		t.Fatalf("twenty quiet minutes stalled under an 1800 s threshold: %+v", s)
	}
	if s, _ := quietFor("100", 10*time.Minute); s.Count != 0 {
		t.Fatalf("an out-of-range threshold was used instead of the default: %+v", s)
	}
	if s, _ := quietFor("", 16*time.Minute); !s.active() {
		t.Fatalf("sixteen quiet minutes did not stall under the default: %+v", s)
	}
}

// h4 (s2, s3, s5): on a host set to interrupt, a stalled turn that is holding
// back a nudge gets exactly one interrupt key, and then the relay's normal
// inbox pass types that nudge once and the transcript confirms it. The record
// keeps both times.
func TestClaudeStallInterruptsAndRewakes(t *testing.T) {
	f := newStallFixture(t)
	hub := &needsInputHub{t: t, agent: f.agent}
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	f.b.Hub = srv.URL
	c, err := api.NewClient(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	const handler = "agt_00000000000000aa"
	hub.update(func(h *needsInputHub) {
		h.agent.ReadUpTo, h.agent.Unread = 40, 1
		h.messages = []api.Message{{Seq: 41, To: f.b.Agent, From: api.Sender{AgentID: handler}, Text: "Nudge: your result is overdue"}}
	})
	f.agent.Unread = 1
	f.setAction(`{"claudeStallAction":"interrupt"}`)
	queue := claudeStallWake(func(ctx context.Context, b runtimeBinding, prompt string) error {
		return claudeWakeWith(ctx, b, prompt, f.wakeOps())
	}, func() time.Time { return f.now })
	var p relayProgress
	saveProgress := func() {
		t.Helper()
		if err := writePrivateJSON(filepath.Join(relayDir(), bindingKey(f.b)+".progress.json"), p); err != nil {
			t.Fatal(err)
		}
	}
	// The nudge is held: the turn is in progress, and nothing is typed.
	if err := relayPass(f.b, &p, c, f.now, queue); err == nil || !strings.Contains(err.Error(), "turn in progress") {
		t.Fatalf("busy turn should hold the nudge: %v", err)
	}
	saveProgress()
	if p.Skip == nil || fmt.Sprint(p.Skip.MessageSeqs) != "[41]" {
		t.Fatalf("held nudge not recorded: %+v", p.Skip)
	}
	f.untouched(t, "held nudge")

	out := f.detect("spinner-stalled-2")
	s := f.record()
	if len(f.keys) != 1 || f.keys[0] != "%7" {
		t.Fatalf("want exactly one interrupt key to the stalled pane, got %q\n%s", f.keys, out)
	}
	if s.Count != 1 || s.Mode != claudeStallInterrupt || s.InterruptedAt.IsZero() || s.EndedAt.IsZero() || !s.RewakeAt.IsZero() || s.Unread != 1 || fmt.Sprint(s.WakeSeqs) != "[41]" {
		t.Fatalf("record after the interrupt: %+v", s)
	}
	if len(f.sent) != 0 {
		t.Fatalf("the stall pass typed text: %q", f.sent)
	}
	for _, want := range []string{"stalled turn interrupted once (pane %7)", "interrupted turn ended; its wake can now be delivered"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q:\n%s", want, out)
		}
	}
	if len(f.notices) != 1 || !strings.Contains(f.notices[0].Notice, "The relay interrupted the turn once at ") || !strings.Contains(f.notices[0].Notice, "let the normal wake deliver its 1 unread (held wake sequences [41])") {
		t.Fatalf("notice: %+v", f.notices)
	}

	// The same relay pass that follows delivers the held nudge, once.
	f.advance(time.Second)
	if err := relayPass(f.b, &p, c, f.now, queue); err != nil {
		t.Fatalf("the nudge was not delivered after the interrupt: %v", err)
	}
	saveProgress()
	prompt := "Tailterm messages #41. Run tt inbox --unread --mark-read."
	if fmt.Sprint(f.sent) != fmt.Sprint([]string{"text:" + prompt, "Enter"}) || p.Wake == nil || p.Wake.Status != "confirmed" {
		t.Fatalf("want the nudge typed and confirmed once, got sent=%q wake=%+v", f.sent, p.Wake)
	}
	s = f.record()
	if s.RewakeAt.IsZero() || s.RewakeAt.Before(s.InterruptedAt) || !s.ResumeAt.IsZero() {
		t.Fatalf("record does not hold the interrupt and the re-wake: %+v", s)
	}
	// Nothing more happens: no second key, no resume line, no second wake.
	hub.update(func(h *needsInputHub) { h.agent.ReadUpTo, h.agent.Unread = 41, 0 })
	f.agent.Unread = 0
	for i := 0; i < 6; i++ {
		f.advance(20 * time.Second)
		f.pass()
		if err := relayPass(f.b, &p, c, f.now, queue); err != nil {
			t.Fatal(err)
		}
	}
	s = f.record()
	if len(f.keys) != 1 || len(f.sent) != 2 || s.active() || !s.ResumeAt.IsZero() || len(f.notices) != 1 {
		t.Fatalf("after delivery: keys=%q sent=%q notices=%d record=%+v", f.keys, f.sent, len(f.notices), s)
	}
}

// stallAgain makes the fixture's run stall once more: whatever was typed last
// started a turn, and it has now written nothing for 16 minutes.
func (f *stallFixture) stallAgain(t *testing.T) string {
	t.Helper()
	f.advance(16 * time.Minute)
	f.screen("spinner-stalled-1")
	return f.detect("spinner-stalled-2")
}

// h5 (s2): a second stall in the same run is never interrupted. It posts one
// notice marked as a repeat; a failed post is retried under the same text, and
// neither a retry nor a restarted relay posts it twice. A third stall posts
// its own repeat notice, once, and is not interrupted either.
func TestClaudeStallRepeatEscalatesOnce(t *testing.T) {
	f := newStallFixture(t)
	f.setAction(`{"claudeStallAction":"interrupt"}`)
	f.detect("spinner-stalled-2")
	f.advance(claudeStallResumeGrace + time.Second)
	f.pass() // nothing unread: the resume line starts a new turn
	if s := f.record(); len(f.keys) != 1 || s.active() || s.ResumeConfirmedAt.IsZero() || s.Count != 1 || len(f.notices) != 1 {
		t.Fatalf("first stall should be interrupted and resumed: keys=%q %+v", f.keys, s)
	}
	keys, sent := len(f.keys), len(f.sent)

	f.notifyErrs = []error{errors.New("hub unavailable")}
	out := f.stallAgain(t)
	s := f.record()
	if !s.active() || s.Count != 2 || s.Mode != claudeStallInterrupt || !s.InterruptedAt.IsZero() || !strings.Contains(s.NotInterrupted, "stall 2 of this run") {
		t.Fatalf("second stall record: %+v\n%s", s, out)
	}
	if len(f.keys) != keys || len(f.sent) != sent {
		t.Fatalf("second stall touched the pane: keys=%q sent=%q", f.keys, f.sent)
	}
	if len(f.notices) != 2 || !s.NoticedAt.IsZero() || !s.EscalatedAt.IsZero() || !strings.Contains(out, "stall notice failed: hub unavailable") {
		t.Fatalf("failed repeat notice: notices=%d record=%+v\n%s", len(f.notices), s, out)
	}
	repeat := f.notices[1]
	for _, want := range []string{"This is stall 2 of the run.", "This is a repeat, so the agent needs a person.", "it never interrupts a run after its first stall"} {
		if !strings.Contains(repeat.Notice, want) {
			t.Fatalf("repeat notice lacks %q: %s", want, repeat.Notice)
		}
	}
	// Inside a minute the failed post is not tried again.
	f.advance(30 * time.Second)
	f.pass()
	if len(f.notices) != 2 {
		t.Fatalf("notice retried inside a minute: %d", len(f.notices))
	}
	f.advance(31 * time.Second)
	out = f.pass()
	s = f.record()
	if len(f.notices) != 3 || s.NoticedAt.IsZero() || !s.EscalatedAt.Equal(s.NoticedAt) || !strings.Contains(out, "stall notice posted once (stall 2 of the run)") {
		t.Fatalf("retried repeat notice: notices=%d record=%+v\n%s", len(f.notices), s, out)
	}
	// The retry replays the same request: same identity, same text.
	if f.notices[2].Notice != repeat.Notice || claudeStallRequestID(f.b, f.notices[2]) != claudeStallRequestID(f.b, repeat) {
		t.Fatalf("retry changed the notice:\n%s\n%s", repeat.Notice, f.notices[2].Notice)
	}
	if claudeStallRequestID(f.b, repeat) == claudeStallRequestID(f.b, f.notices[0]) {
		t.Fatal("the repeat reused the first stall's request identity")
	}
	// A restarted relay has only the record on disk; it posts nothing more.
	restarted := *f
	restarted.notices = nil
	for i := 0; i < 5; i++ {
		restarted.advance(70 * time.Second)
		restarted.pass()
	}
	if len(restarted.notices) != 0 || len(restarted.keys) != keys || len(restarted.sent) != sent {
		t.Fatalf("restarted relay acted again: notices=%d keys=%q sent=%q", len(restarted.notices), restarted.keys, restarted.sent)
	}
	// A third stall of the run: recorded and stuck, no key, and one notice of
	// its own, under its own identity, marked as a repeat.
	f.now = restarted.now
	f.appendAssistant(f.now)
	f.advance(20 * time.Second)
	if out := f.pass(); !strings.Contains(out, "ended: the turn produced output") {
		t.Fatalf("output did not end the second stall: %q", out)
	}
	posted := len(f.notices)
	f.stallAgain(t)
	third := f.record()
	if !third.active() || third.Count != 3 || !third.InterruptedAt.IsZero() || len(f.keys) != keys || len(f.sent) != sent {
		t.Fatalf("third stall: %+v keys=%q", third, f.keys)
	}
	if len(f.notices) != posted+1 || third.NoticedAt.IsZero() || !third.EscalatedAt.Equal(third.NoticedAt) {
		t.Fatalf("third stall posted %d notices, want one: %+v", len(f.notices)-posted, third)
	}
	last := f.notices[len(f.notices)-1]
	for _, want := range []string{"This is stall 3 of the run.", "This is a repeat, so the agent needs a person."} {
		if !strings.Contains(last.Notice, want) {
			t.Fatalf("third stall notice lacks %q: %s", want, last.Notice)
		}
	}
	if id := claudeStallRequestID(f.b, last); id == claudeStallRequestID(f.b, repeat) || id == claudeStallRequestID(f.b, f.notices[0]) {
		t.Fatalf("the third stall reused an earlier request identity %s", id)
	}
	for i := 0; i < 5; i++ {
		f.advance(70 * time.Second)
		f.pass()
	}
	if len(f.notices) != posted+1 || len(f.keys) != keys || len(f.sent) != sent {
		t.Fatalf("third stall reported again or touched the pane: notices=%d keys=%q", len(f.notices)-posted, f.keys)
	}
	c, _ := f.cursor(f.b)
	if reason := claudeStallStuckReason(f.b, &c, f.now); !strings.HasPrefix(reason, "turn stalled: no output for ") {
		t.Fatalf("third stall is not shown as stuck: %q", reason)
	}
}

// h6 (s2): a host set to interrupt still fails closed. Each case is a real
// stall by the transcript; none may receive a key or any text.
func TestClaudeStallFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *stallFixture)
		// second is the capture shown at the second reading.
		second string
		// stalled: the stall is still recorded and reported.
		stalled bool
		why     string
	}{
		{"status line without a counter", func(f *stallFixture) { f.screen("spinner-no-counter") }, "spinner-no-counter", true, "status line shows no token counter"},
		{"no status line", func(f *stallFixture) { f.screen("streaming-no-status") }, "streaming-no-status", true, "no status line above the input box"},
		{"counter unreadable at the second reading", nil, "spinner-no-counter", true, "status line shows no token counter"},
		{"permission dialog", nil, "permission-dialog", true, "permission or selection prompt"},
		{"selection dialog", nil, "selection-dialog", true, "permission or selection prompt"},
		{"a draft in the input", nil, "typed", true, "Claude cursor is not at an empty input"},
		{"idle pane after a person interrupted", nil, "interrupted-prompt-restored", true, "Claude cursor is not at an empty input"},
		{"pane identity lost", func(f *stallFixture) {
			f.beforeCapture = func(n int) {
				if n == 2 {
					f.captureErr = errors.New("Claude pane identity changed or pane is in a mode")
				}
			}
		}, "spinner-stalled-2", true, "Claude pane identity changed or pane is in a mode"},
		{"pane changed between the readings", func(f *stallFixture) {
			f.beforeCapture = func(n int) {
				if n == 2 {
					f.pane = "%9"
				}
			}
		}, "spinner-stalled-2", true, "pane changed between the two readings"},
		{"pane changed at the interrupt", func(f *stallFixture) {
			f.beforeCapture = func(n int) {
				if n == 3 {
					f.pane = "%9"
				}
			}
		}, "spinner-stalled-2", true, "pane or token counter changed before the interrupt"},
		{"counter moved at the interrupt", func(f *stallFixture) {
			f.beforeCapture = func(n int) {
				if n == 3 {
					f.screen("spinner-progress-2")
				}
			}
		}, "spinner-stalled-2", true, "pane or token counter changed before the interrupt"},
		{"dialog opened at the interrupt", func(f *stallFixture) {
			f.beforeCapture = func(n int) {
				if n == 3 {
					f.screen("permission-dialog")
				}
			}
		}, "spinner-stalled-2", true, "pane not safe at the interrupt"},
		{"transcript grew at the interrupt", func(f *stallFixture) {
			f.beforeCapture = func(n int) {
				if n == 3 {
					f.appendAssistant(f.now)
				}
			}
		}, "spinner-stalled-2", true, "transcript changed before the interrupt"},
		{"owner helper session", func(f *stallFixture) { f.b.Role = api.AgentRoleOwnerHelper }, "spinner-stalled-2", true, "the owner helper's session is never interrupted"},
		{"owner helper by hub role", func(f *stallFixture) { f.agent.Role = api.AgentRoleOwnerHelper }, "spinner-stalled-2", true, "the owner helper's session is never interrupted"},
		{"needs_input", func(f *stallFixture) { f.agent.Status = api.AgentNeedsInput }, "spinner-stalled-2", false, ""},
		{"done", func(f *stallFixture) { f.agent.Status = api.AgentDone }, "spinner-stalled-2", false, ""},
		{"superseded run", func(f *stallFixture) { f.agent.RunID = "run_00000000000000ff" }, "spinner-stalled-2", false, ""},
		{"non-Claude runtime", func(f *stallFixture) { f.b.Runtime, f.b.Codex = "codex", "/usr/bin/codex" }, "spinner-stalled-2", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStallFixture(t)
			f.setAction(`{"claudeStallAction":"interrupt"}`)
			if tc.setup != nil {
				tc.setup(f)
			}
			out := f.detect(tc.second)
			s := f.record()
			// Keep going well past every wait: still nothing.
			for i := 0; i < 8; i++ {
				f.advance(40 * time.Second)
				f.pass()
			}
			f.untouched(t, tc.name)
			if !tc.stalled {
				if s.Count != 0 || s.active() || len(f.notices) != 0 || strings.Contains(out, "turn stalled") {
					t.Fatalf("not a stall, yet recorded: %+v\n%s", s, out)
				}
				return
			}
			if !s.active() || s.Count != 1 || s.Mode != claudeStallInterrupt || !s.InterruptedAt.IsZero() || !strings.Contains(s.NotInterrupted, tc.why) {
				t.Fatalf("record %+v, want an uninterrupted stall because %q\n%s", s, tc.why, out)
			}
			if !strings.Contains(out, "stalled turn not interrupted: ") || len(f.notices) != 1 || !strings.Contains(f.notices[0].Notice, "The relay sent no key and typed nothing, because ") || !strings.Contains(f.notices[0].Notice, tc.why) {
				t.Fatalf("refusal not logged and reported: notices=%+v\n%s", f.notices, out)
			}
		})
	}
	// A key that cannot be sent is not sent again, and nothing is typed.
	f := newStallFixture(t)
	f.setAction(`{"claudeStallAction":"interrupt"}`)
	f.interruptErr = errors.New("tmux: no such pane")
	f.detect("spinner-stalled-2")
	for i := 0; i < 8; i++ {
		f.advance(40 * time.Second)
		f.pass()
	}
	if s := f.record(); len(f.keys) != 1 || len(f.sent) != 0 || !s.active() || !strings.HasPrefix(s.Outcome, "interrupt key failed") || !strings.Contains(f.notices[0].Notice, "the key could not be sent") {
		t.Fatalf("failed key: keys=%q sent=%q record=%+v", f.keys, f.sent, s)
	}
	// Escape that Claude Code records nothing for (pressed before the turn's
	// first assistant record): the turn never ends in the transcript, so
	// nothing is typed, however long the relay waits.
	f = newStallFixture(t)
	f.setAction(`{"claudeStallAction":"interrupt"}`)
	f.escapeEnds = false
	out := f.detect("spinner-stalled-2")
	for i := 0; i < 12; i++ {
		f.advance(40 * time.Second)
		out += f.pass()
	}
	if s := f.record(); len(f.keys) != 1 || len(f.sent) != 0 || !s.active() || !s.EndedAt.IsZero() || !strings.Contains(s.Outcome, "never showed the turn ending") || !strings.Contains(out, "interrupted turn never ended in the transcript; nothing typed") {
		t.Fatalf("unconfirmed interrupt: keys=%q sent=%q record=%+v\n%s", f.keys, f.sent, s, out)
	}
}

// h7 (s2): the record Claude Code writes on Escape ends the turn, so the wake
// that follows an interrupt is accepted. Captured from Claude Code 2.1.292.
func TestClaudeInterruptedTurnEnds(t *testing.T) {
	for _, fixture := range []string{"interrupt-after-tool", "interrupt-streaming"} {
		now := time.Date(2026, 10, 7, 7, 0, 0, 0, time.UTC)
		b, transcript := claudeTranscriptFixture(t, fixture)
		t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
		snap, err := claudeTranscriptSnapshot(b, now)
		if err != nil {
			t.Fatalf("%s: an interrupted turn is still busy: %v", fixture, err)
		}
		if c := snap.Cursor; !c.TurnComplete || c.TurnEndReason != claudeInterruptReason || len(c.Pending) != 0 {
			t.Fatalf("%s: cursor %+v", fixture, c)
		}
		state := activityState(&snap.Cursor, api.Agent{Status: api.AgentRunning}, 0, true, true, nil, now, activityDefaults())
		if state.State != "idle" || state.Reason != "turn interrupted" {
			t.Fatalf("%s: activity %+v", fixture, state)
		}
		var sent []string
		if err := claudeWakeWith(context.Background(), b, "Tailterm messages #7. Run tt inbox --unread --mark-read.", transcriptWakeOps(t, transcript, &now, &sent)); err != nil || len(sent) != 2 {
			t.Fatalf("%s: wake after the interrupt: err=%v sent=%q", fixture, err, sent)
		}
	}
	// The same turn without the interrupt record is in progress.
	b, transcript := claudeTranscriptFixture(t, "interrupt-after-tool")
	open, interrupt := stallOpenTurn(t)
	if err := os.WriteFile(transcript, []byte(open), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := claudeTranscriptSnapshot(b, time.Now()); err == nil || !strings.Contains(err.Error(), "turn in progress") {
		t.Fatalf("open turn: %v", err)
	}
	// Both forms of the record end a turn, and a pending call ends with it.
	for _, text := range []string{"[Request interrupted by user]", "[Request interrupted by user for tool use]"} {
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]string{{"type": "text", "text": text}}}})
		if ended, reason := claudeTurnEnd(line); !ended || reason != claudeInterruptReason {
			t.Fatalf("%q does not end a turn", text)
		}
		c := activityCursor{SeenTurn: true, Pending: map[string]pendingActivityCall{"toolu_1": {Name: "Bash"}}}
		if err := parseClaudeActivity(line, &c); err != nil || !c.TurnComplete || len(c.Pending) != 0 {
			t.Fatalf("%q: cursor %+v err %v", text, c, err)
		}
	}
	if ended, _ := claudeTurnEnd([]byte(interrupt)); !ended {
		t.Fatal("the captured interrupt record does not end a turn")
	}
	// A person typing the same words starts a turn: a typed prompt is a string.
	for _, content := range []any{"[Request interrupted by user]", []map[string]string{{"type": "text", "text": "[Request interrupted by user] and more"}},
		[]map[string]string{{"type": "text", "text": "[Request interrupted by user]"}, {"type": "text", "text": "x"}}} {
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
		c := activityCursor{SeenTurn: true, TurnComplete: true}
		if ended, _ := claudeTurnEnd(line); ended {
			t.Fatalf("%v ends a turn", content)
		}
		if err := parseClaudeActivity(line, &c); err != nil || c.TurnComplete {
			t.Fatalf("%v did not start a turn: %+v %v", content, c, err)
		}
	}
	// Escape before a turn's first assistant record writes nothing (the third
	// capture): the transcript still says the turn is in progress.
	b, _ = claudeTranscriptFixture(t, "interrupt-no-record")
	if _, err := claudeTranscriptSnapshot(b, time.Now()); err == nil || !strings.Contains(err.Error(), "turn in progress") {
		t.Fatalf("interrupt with no record: %v", err)
	}
}

// h10 (s1, owner answer #27465): the default is detect and report. With no
// relay.json, a malformed one, one without the key and one with any other
// value, a stall is recorded, shown as stuck and notified once, and the pane
// receives no key and no text, with and without a wake waiting.
func TestClaudeStallDefaultSendsNothing(t *testing.T) {
	for name, content := range map[string]string{
		"no file":                             "",
		"malformed":                           `{"claudeStallAction":"interrupt"`,
		"not an object":                       `"interrupt"`,
		"no key":                              `{"other":true}`,
		"empty value":                         `{"claudeStallAction":""}`,
		"report":                              `{"claudeStallAction":"report"}`,
		"unknown value":                       `{"claudeStallAction":"cancel"}`,
		"wrong case":                          `{"claudeStallAction":"Interrupt"}`,
		"padded":                              `{"claudeStallAction":" interrupt "}`,
		"wrong type":                          `{"claudeStallAction":true}`,
		"wrong key case":                      `{"claudestallaction":"interrupt"}`,
		"upper key case":                      `{"CLAUDESTALLACTION":"interrupt"}`,
		"lower-case duplicate after the key":  `{"claudeStallAction":"report","claudestallaction":"interrupt"}`,
		"lower-case duplicate before the key": `{"claudestallaction":"interrupt","claudeStallAction":"report"}`,
		"null value":                          `{"claudeStallAction":null}`,
	} {
		for _, unread := range []int{0, 2} {
			t.Run(fmt.Sprintf("%s unread %d", name, unread), func(t *testing.T) {
				f := newStallFixture(t)
				f.setAction(content)
				if got := relayStallAction(); got != claudeStallReport {
					t.Fatalf("setting %q read as %q", content, got)
				}
				f.agent.Unread = unread
				f.detect("spinner-stalled-2")
				for i := 0; i < 10; i++ {
					f.advance(40 * time.Second)
					f.pass()
					if unread > 0 {
						// The normal wake path keeps trying and keeps refusing.
						err := claudeWakeWith(context.Background(), f.b, "Tailterm messages #41. Run tt inbox --unread --mark-read.", f.wakeOps())
						if !errors.Is(err, errClaudeWakeUnsafe) || !strings.Contains(err.Error(), "turn in progress") {
							t.Fatalf("wake into the stalled turn: %v", err)
						}
					}
				}
				f.untouched(t, name)
				s := f.record()
				if !s.active() || s.Count != 1 || s.Mode != claudeStallReport || !s.InterruptedAt.IsZero() || !s.ResumeAt.IsZero() || !s.RewakeAt.IsZero() {
					t.Fatalf("record %+v", s)
				}
				c, _ := f.cursor(f.b)
				if reason := claudeStallStuckReason(f.b, &c, f.now); !strings.HasPrefix(reason, "turn stalled: no output for ") {
					t.Fatalf("stall not shown as stuck: %q", reason)
				}
				if len(f.notices) != 1 || !strings.Contains(f.notices[0].Notice, "Host setting claudeStallAction is report. The relay sent no key and typed nothing.") {
					t.Fatalf("want one report notice, got %+v", f.notices)
				}
				would := "it would have interrupted the turn once and then would type the resume line once."
				if unread > 0 {
					would = "it would have interrupted the turn once and then would let the normal wake deliver its 2 unread."
				}
				if !strings.Contains(f.notices[0].Notice, would) {
					t.Fatalf("notice lacks the would-have action %q: %s", would, f.notices[0].Notice)
				}
			})
		}
	}
	// The exact key decides, whatever other keys say.
	f := newStallFixture(t)
	for content, want := range map[string]string{
		`{"claudeStallAction":"interrupt"}`:                              claudeStallInterrupt,
		`{"claudeStallAction":"interrupt","claudestallaction":"report"}`: claudeStallInterrupt,
		`{"claudestallaction":"report","claudeStallAction":"interrupt"}`: claudeStallInterrupt,
		`{"hub":"x","claudeStallAction":"interrupt","n":[1,{"a":2}]}`:    claudeStallInterrupt,
	} {
		f.setAction(content)
		if got := relayStallAction(); got != want {
			t.Fatalf("setting %s read as %q, want %q", content, got, want)
		}
	}
	// A setting file that cannot be read at all also means report.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "tailterm", "relay.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := relayStallAction(); got != claudeStallReport {
		t.Fatalf("unreadable setting read as %q", got)
	}
}

// stallHub is a real hub store behind the real server, for notice posts.
type stallHub struct {
	c      *api.Client
	task   api.Task
	st     *store.Store
	by     api.Caller
	srvURL string
}

func newStallHub(t *testing.T) *stallHub {
	t.Helper()
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
	token := strings.Repeat("t", 40)
	identity, err := server.TokenIdentity(token, by)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, identity))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	data, _ := json.Marshal(map[string]string{"url": srv.URL, "token": token})
	if err := os.MkdirAll(filepath.Join(home, ".config", "tailterm"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "tailterm", "hub.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := api.NewClient(srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.Token = token
	return &stallHub{c: c, task: task, st: st, by: by, srvURL: srv.URL}
}

func (h *stallHub) add(t *testing.T, name, role string) api.Agent {
	t.Helper()
	a, err := h.st.AddAgent(context.Background(), h.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "h", Session: name, Runtime: "claude", Role: role}, h.by)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (h *stallHub) helper(t *testing.T, request string) api.Agent {
	t.Helper()
	ctx := context.Background()
	if _, err := h.st.RegisterOwnerHelper(ctx, h.task.ID, api.RegisterOwnerHelperRequest{Host: "h", Session: "owner", Runtime: "claude", RequestID: request}, h.by); err != nil {
		t.Fatal(err)
	}
	agents, err := h.c.ListAgents(ctx, h.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if a.Role == api.AgentRoleOwnerHelper && a.Status != api.AgentClosed {
			return a
		}
	}
	t.Fatal("no owner helper registered")
	return api.Agent{}
}

func (h *stallHub) binding(a api.Agent) runtimeBinding {
	b := testClaudeBinding()
	b.Hub, b.Task, b.Agent, b.Session = h.srvURL, h.task.ID, a.ID, a.Session
	return b
}

func (h *stallHub) notices(t *testing.T) []api.Message {
	t.Helper()
	all, err := h.c.ListMessages(context.Background(), h.task.ID, 0, "", 200)
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

// stallRecord is a detected stall as the pass leaves it before the notice.
func stallRecord(b runtimeBinding, mode string, count int) claudeStall {
	s := claudeStall{Run: b.Run, Count: count, Since: stallLastOutput, DetectedAt: stallLastOutput.Add(17 * time.Minute), Mode: mode, Counter: "75"}
	s.Notice = claudeStallNoticeText(b, s)
	return s
}

// h11 (owner answer #27465): the notice goes to the live owner helper, to the
// Board when there is none, never to the stalled agent. It names the agent,
// run, session, signals, mode and what the relay would have done. A second
// pass, a retry and a restarted relay all use one request identity and post
// once.
func TestClaudeStallNotice(t *testing.T) {
	ctx := context.Background()
	h := newStallHub(t)
	worker := h.add(t, "builder", "")
	h.add(t, "database", api.AgentRoleDatabaseHandler)
	b := h.binding(worker)
	s := stallRecord(b, claudeStallReport, 1)

	// No owner helper: the Board, with no recipient; never the handler.
	if err := nativeClaudeStallNotify(ctx, b, s); err != nil {
		t.Fatal(err)
	}
	got := h.notices(t)
	if len(got) != 1 || got[0].To != "" || got[0].Envelope.To != "" {
		t.Fatalf("with no owner helper the notice should reach the Board: %+v", got)
	}
	helper := h.helper(t, "ohreg-stall-1")
	later := s
	later.Since = s.Since.Add(time.Hour)
	later.DetectedAt = later.Since.Add(17 * time.Minute)
	later.Unread, later.WakeSeqs = 3, []int64{41, 44}
	later.Notice = claudeStallNoticeText(b, later)
	for i := 0; i < 3; i++ { // a second pass, a retry, a restarted relay
		if err := nativeClaudeStallNotify(ctx, b, later); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	got = h.notices(t)
	if len(got) != 2 || got[1].To != helper.ID || got[1].Envelope.To != helper.Name || got[1].Envelope.Subject != claudeStallSubject {
		t.Fatalf("want exactly one more notice, to the owner helper: %+v", got)
	}
	if id := got[1].PostReceipt; id == nil || id.RequestID != fmt.Sprintf("claude-stall-%s-%d", strings.TrimPrefix(b.Run, "run_"), later.Since.Unix()) {
		t.Fatalf("request identity %+v", got[1].PostReceipt)
	}
	for _, want := range []string{worker.ID, b.Run, worker.Session, "no transcript record and no worktree change since 2026-10-07T07:54:26Z (17m)", "no tool call pending",
		"the pane's token counter read 75 in two captures at least 60s apart", "This is stall 1 of the run", "Host setting claudeStallAction is report",
		"The relay sent no key and typed nothing. With the setting on interrupt it would have interrupted the turn once and then would let the normal wake deliver its 3 unread (held wake sequences [41 44]).",
		"an earlier notice about a skipped wake may describe the same episode"} {
		if !strings.Contains(got[1].Text, want) {
			t.Fatalf("notice lacks %q:\n%s", want, got[1].Text)
		}
	}
	if refs := got[1].Envelope.Refs; refs["agent"] != worker.ID || refs["run"] != b.Run || refs["session"] != worker.Session || refs["mode"] != "report" || refs["stall"] != "1" || refs["repeat"] != "" {
		t.Fatalf("refs %+v", refs)
	}
	if from := got[1].From; from != (api.Sender{Node: server.RelayNode, User: server.RelayUser}) {
		t.Fatalf("notice author %+v, want the relay", from)
	}
	// A relay that lost its record detects the same stall at another time: the
	// same identity with other text is refused by the hub and counts as posted.
	relost := later
	relost.DetectedAt = later.DetectedAt.Add(3 * time.Minute)
	relost.Notice = claudeStallNoticeText(b, relost)
	if relost.Notice == later.Notice {
		t.Fatal("fixture: the re-detected notice should differ")
	}
	if err := nativeClaudeStallNotify(ctx, b, relost); err != nil || len(h.notices(t)) != 2 {
		t.Fatalf("re-detected stall posted again: err=%v notices=%d", err, len(h.notices(t)))
	}
	// The stalled agent is the owner helper: never addressed to itself.
	hb := h.binding(helper)
	hs := stallRecord(hb, claudeStallReport, 1)
	hs.Since = s.Since.Add(3 * time.Hour)
	hs.Notice = claudeStallNoticeText(hb, hs)
	if err := nativeClaudeStallNotify(ctx, hb, hs); err != nil {
		t.Fatal(err)
	}
	if got = h.notices(t); len(got) != 3 || got[2].To != "" || !strings.Contains(got[2].Text, helper.ID) {
		t.Fatalf("a stalled owner helper should be reported on the Board: %+v", got)
	}
	// A repeat is marked.
	repeat := stallRecord(b, claudeStallInterrupt, 2)
	repeat.Since = s.Since.Add(2 * time.Hour)
	repeat.Notice = claudeStallNoticeText(b, repeat)
	if err := nativeClaudeStallNotify(ctx, b, repeat); err != nil {
		t.Fatal(err)
	}
	if got = h.notices(t); len(got) != 4 || got[3].Envelope.Refs["repeat"] != "true" || got[3].Envelope.Refs["stall"] != "2" || !strings.Contains(got[3].Text, "This is a repeat, so the agent needs a person.") {
		t.Fatalf("repeat notice: %+v", got)
	}
	// A third stall posts too, under its own identity, still marked.
	third := stallRecord(b, claudeStallInterrupt, 3)
	third.Since = s.Since.Add(4 * time.Hour)
	third.Notice = claudeStallNoticeText(b, third)
	for i := 0; i < 2; i++ {
		if err := nativeClaudeStallNotify(ctx, b, third); err != nil {
			t.Fatal(err)
		}
	}
	if got = h.notices(t); len(got) != 5 || got[4].To != helper.ID || got[4].Envelope.Refs["repeat"] != "true" || got[4].Envelope.Refs["stall"] != "3" || !strings.Contains(got[4].Text, "This is stall 3 of the run.") || got[4].PostReceipt.RequestID == got[3].PostReceipt.RequestID {
		t.Fatalf("third stall notice: %+v", got)
	}
	// The recipient rule, on its own.
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	agents := []api.Agent{
		{ID: "agt_h", Role: api.AgentRoleDatabaseHandler, Status: api.AgentRunning, CreatedAt: old.Add(9 * time.Hour)},
		{ID: "agt_o1", Role: api.AgentRoleOwnerHelper, Status: api.AgentRunning, CreatedAt: old},
		{ID: "agt_o2", Role: api.AgentRoleOwnerHelper, Status: api.AgentDone, CreatedAt: old.Add(time.Hour)},
		{ID: "agt_o3", Role: api.AgentRoleOwnerHelper, Status: api.AgentClosed, CreatedAt: old.Add(2 * time.Hour)},
		{ID: "agt_o4", Role: api.AgentRoleOwnerHelper, Status: api.AgentRetired, CreatedAt: old.Add(3 * time.Hour)},
		{ID: "agt_w", Status: api.AgentRunning, CreatedAt: old.Add(4 * time.Hour)},
	}
	if to := claudeStallRecipient(agents, "agt_w"); to.ID != "agt_o2" {
		t.Fatalf("newest live owner helper: %+v", to)
	}
	if to := claudeStallRecipient(agents, "agt_o2"); to.ID != "agt_o1" {
		t.Fatalf("the stalled helper was not excluded: %+v", to)
	}
	if to := claudeStallRecipient(agents[:1], "agt_w"); to.ID != "" {
		t.Fatalf("a handler was chosen: %+v", to)
	}
}

// h12 (s2, owner answer #27466): on a host set to interrupt, a stalled turn
// with nothing unread gets one interrupt key and then the fixed resume line,
// typed once through the wake path with a separate Enter and confirmed by the
// transcript. The line is the one the doc gives.
func TestClaudeStallResumeLine(t *testing.T) {
	f := newStallFixture(t)
	f.setAction(`{"claudeStallAction":"interrupt"}`)
	f.detect("spinner-stalled-2")
	s := f.record()
	if len(f.keys) != 1 || s.EndedAt.IsZero() || len(f.sent) != 0 {
		t.Fatalf("after detection: keys=%q sent=%q record=%+v", f.keys, f.sent, s)
	}
	if len(f.notices) != 1 || !strings.Contains(f.notices[0].Notice, "the transcript shows it ended. It will now type the resume line once.") {
		t.Fatalf("notice: %+v", f.notices)
	}
	// The normal wake path gets the first turn: nothing is typed yet.
	f.advance(claudeStallResumeGrace - 10*time.Second)
	f.pass()
	if len(f.sent) != 0 {
		t.Fatalf("resume line typed inside the grace period: %q", f.sent)
	}
	f.advance(11 * time.Second)
	out := f.pass()
	if fmt.Sprint(f.sent) != fmt.Sprint([]string{"text:" + claudeStallResumeLine, "Enter"}) {
		t.Fatalf("want the resume line then a separate Enter, got %q", f.sent)
	}
	s = f.record()
	if s.ResumeAt.IsZero() || s.ResumeConfirmedAt.IsZero() || s.ResumeConfirmedAt.Before(s.InterruptedAt) || s.active() || !s.RewakeAt.IsZero() {
		t.Fatalf("record does not hold the resume line: %+v", s)
	}
	if !strings.Contains(out, "nothing unread after the interrupt; typing the resume line once") {
		t.Fatalf("resume not logged: %q", out)
	}
	var intent claudeWakeIntent
	data, err := os.ReadFile(claudeWakePath(f.b))
	if err != nil || json.Unmarshal(data, &intent) != nil || intent.Phase != "confirmed" || intent.Prompt != claudeStallResumeLine || intent.TextAt.IsZero() || intent.EnterAt.IsZero() {
		t.Fatalf("the resume line did not go through the wake path's intent: %+v %v", intent, err)
	}
	// Once only: later passes type nothing and send no key.
	for i := 0; i < 8; i++ {
		f.advance(40 * time.Second)
		f.pass()
	}
	if len(f.sent) != 2 || len(f.keys) != 1 || len(f.notices) != 1 {
		t.Fatalf("after the resume line: keys=%q sent=%q notices=%d", f.keys, f.sent, len(f.notices))
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "claude-wake.md"))
	if err != nil || !strings.Contains(string(doc), "`"+claudeStallResumeLine+"`") {
		t.Fatalf("docs/claude-wake.md does not give the resume line verbatim (%v)", err)
	}

	// With input unread, the resume line is never typed: that input's own
	// wake is the re-delivery.
	f = newStallFixture(t)
	f.setAction(`{"claudeStallAction":"interrupt"}`)
	f.agent.Unread = 1
	f.detect("spinner-stalled-2")
	for i := 0; i < 8; i++ {
		f.advance(40 * time.Second)
		f.pass()
	}
	if s := f.record(); len(f.keys) != 1 || len(f.sent) != 0 || !s.ResumeAt.IsZero() || !s.active() {
		t.Fatalf("resume line typed over unread input: keys=%q sent=%q record=%+v", f.keys, f.sent, s)
	}
}

// h13 (owner answer #27465): the setting is read at each detection, with no
// restart, and is fixed for that stall. A stall detected under report is never
// interrupted later; the next stall follows the new setting.
func TestClaudeStallActionSetting(t *testing.T) {
	f := newStallFixture(t)
	f.setAction(`{"claudeStallAction":"report"}`)
	f.detect("spinner-stalled-2")
	if s := f.record(); !s.active() || s.Mode != claudeStallReport {
		t.Fatalf("first stall: %+v", s)
	}
	f.setAction(`{"claudeStallAction":"interrupt"}`)
	if relayStallAction() != claudeStallInterrupt {
		t.Fatal("the changed setting is not read")
	}
	for i := 0; i < 10; i++ {
		f.advance(40 * time.Second)
		f.pass()
	}
	f.untouched(t, "a stall detected under report")
	if s := f.record(); !s.active() || s.Mode != claudeStallReport || s.Count != 1 {
		t.Fatalf("the recorded stall changed mode: %+v", s)
	}
	// The turn moves on and stalls again: the same process, the same ops, no
	// restart, and the new setting applies. A run is interrupted at most
	// once, and this run has not been.
	f.appendAssistant(f.now)
	f.advance(20 * time.Second)
	f.pass()
	out := f.stallAgain(t)
	s := f.record()
	if s.Count != 2 || s.Mode != claudeStallInterrupt {
		t.Fatalf("second stall did not read the new setting: %+v\n%s", s, out)
	}
	if len(f.keys) != 0 || !strings.Contains(s.NotInterrupted, "stall 2 of this run") {
		t.Fatalf("a second stall is never interrupted: keys=%q %+v", f.keys, s)
	}
	// A fresh run under the new setting is interrupted on its first stall,
	// and switching back to report leaves that stall's saved mode alone.
	g := newStallFixture(t)
	g.setAction(`{"claudeStallAction":"interrupt"}`)
	g.escapeEnds = false
	g.detect("spinner-stalled-2")
	g.setAction(`{"claudeStallAction":"report"}`)
	g.advance(40 * time.Second)
	g.pass()
	if s := g.record(); len(g.keys) != 1 || s.Mode != claudeStallInterrupt || s.Count != 1 {
		t.Fatalf("first stall under interrupt: keys=%q %+v", g.keys, s)
	}
}

// h14 (request #27474): a stalled agent with unread input can get both the
// ten-minute skip notice and the stall notice. They have different request
// identities and different recipients, and each posts once.
func TestClaudeStallNoticeBesideSkipNotice(t *testing.T) {
	ctx := context.Background()
	h := newStallHub(t)
	worker := h.add(t, "builder", "")
	handler := h.add(t, "database", api.AgentRoleDatabaseHandler)
	helper := h.helper(t, "ohreg-stall-2")
	b := h.binding(worker)
	skip := claudeWakeSkip{Run: b.Run, Reason: "Claude transcript busy, incomplete, or unknown: turn in progress", Since: stallLastOutput.Add(5 * time.Minute)}
	stall := stallRecord(b, claudeStallReport, 1)
	stall.Unread = 1
	stall.Notice = claudeStallNoticeText(b, stall)
	for i := 0; i < 2; i++ {
		if err := nativeClaudeEscalate(ctx, b, skip); err != nil {
			t.Fatal(err)
		}
		if err := nativeClaudeStallNotify(ctx, b, stall); err != nil {
			t.Fatal(err)
		}
	}
	got := h.notices(t)
	if len(got) != 2 {
		t.Fatalf("want one skip notice and one stall notice, got %d: %+v", len(got), got)
	}
	skipNotice, stallNotice := got[0], got[1]
	if skipNotice.To != handler.ID || skipNotice.Envelope.Subject != "The relay cannot wake a Claude agent that has unread input" {
		t.Fatalf("skip notice: %+v", skipNotice)
	}
	if stallNotice.To != helper.ID || stallNotice.Envelope.Subject != claudeStallSubject {
		t.Fatalf("stall notice: %+v", stallNotice)
	}
	a, c := skipNotice.PostReceipt, stallNotice.PostReceipt
	if a == nil || c == nil || a.RequestID == c.RequestID || !strings.HasPrefix(a.RequestID, "claude-wake-skip-") || !strings.HasPrefix(c.RequestID, "claude-stall-") {
		t.Fatalf("request identities %+v %+v", a, c)
	}
	if !strings.Contains(stallNotice.Text, "an earlier notice about a skipped wake may describe the same episode") {
		t.Fatalf("the stall notice does not mention the skip notice: %s", stallNotice.Text)
	}
}

// The stall rule runs on every relay pass, three seconds apart, but asks the
// hub about an agent at most once a minute, and not at all while the turn is
// not quiet or a recorded stall is simply standing.
func TestClaudeStallHubReadsAreSpaced(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *stallFixture)
		most  int
	}{
		{"quiet open turn of an agent that is not running", func(f *stallFixture) { f.agent.Status = api.AgentNeedsInput }, 5},
		{"stall in report mode", nil, 2},
		{"turn that is not quiet", func(f *stallFixture) { f.now = stallLastOutput.Add(5 * time.Minute) }, 0},
	} {
		f := newStallFixture(t)
		if tc.setup != nil {
			tc.setup(f)
		}
		for i := 0; i < 80; i++ { // four minutes of relay passes
			f.pass()
			f.advance(3 * time.Second)
		}
		if f.agentReads > tc.most {
			t.Errorf("%s: %d hub reads in four minutes, want at most %d", tc.name, f.agentReads, tc.most)
		}
		f.untouched(t, tc.name)
	}
}

// The interrupt is exactly one Escape to the named pane, on a private tmux
// socket: a raw `cat -v` shows what arrived.
func TestNativeClaudeInterruptPrivateSocket(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	socket := "tt-stall-" + strings.TrimPrefix(api.NewID("agt"), "agt_")
	t.Setenv("TT_TMUX_SOCKET", socket)
	ctx := context.Background()
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
		// tmux leaves the socket file behind on macOS.
		dir := os.Getenv("TMUX_TMPDIR")
		if dir == "" {
			dir = "/tmp"
		}
		_ = os.Remove(filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), socket))
	})
	if out, err := exec.Command("tmux", "-L", socket, "-f", "/dev/null", "new-session", "-d", "-s", "stall", "-x", "80", "-y", "24", "stty raw -echo; exec cat -v").CombinedOutput(); err != nil {
		t.Fatalf("tmux: %v %s", err, out)
	}
	raw, err := startupTmux(ctx, "list-panes", "-s", "-t", "stall", "-F", "#{pane_id}")
	if err != nil {
		t.Fatal(err)
	}
	pane := strings.TrimSpace(string(raw))
	time.Sleep(300 * time.Millisecond) // let cat own the raw terminal
	if err := nativeClaudeInterrupt(ctx, pane); err != nil {
		t.Fatal(err)
	}
	var got string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		out, err := startupTmux(ctx, "capture-pane", "-p", "-t", pane)
		if err != nil {
			t.Fatal(err)
		}
		if got = strings.TrimSpace(string(out)); got != "" {
			break
		}
	}
	// Give a second key time to show, were one sent.
	time.Sleep(300 * time.Millisecond)
	out, _ := startupTmux(ctx, "capture-pane", "-p", "-t", pane)
	if got = strings.TrimSpace(string(out)); got != "^[" {
		t.Fatalf("pane received %q, want one Escape (^[)", got)
	}
	if err := nativeClaudeInterrupt(ctx, "%999"); err == nil {
		t.Fatal("a key to a pane that does not exist reported success")
	}
}

// Live: run with TT_LIVE_CLAUDE=1. A disposable Claude session on a private
// tmux socket works through a tool call and then thinks. The native pane
// reading must find its token counter, one native Escape must end the turn in
// the transcript, and the resume line must then be typed and confirmed through
// the wake path. The 15-minute wait itself is not reproduced; the steps a host
// set to interrupt takes after it are. No working agent pane is touched.
func TestClaudeStallLivePrivateSession(t *testing.T) {
	lc := startLiveClaude(t, false)
	ctx := context.Background()
	ops := nativeClaudeStallOps(lc.client)
	lc.submit("Run the bash command: true. After it returns, think privately and at great length about ten proofs of the infinitude of primes, then write a 2000-word essay. Use no other tools.")
	var reading claudeStallReading
	seen := ""
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		if reading = claudeStallRead(ctx, lc.b, ops); reading.Counter != "" {
			break
		}
		if reading.Note != seen {
			seen = reading.Note
			t.Logf("live reading not usable yet: %s", seen)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no token counter read from the live pane: %q\n%s", reading.Note, lc.capture(false))
		}
	}
	t.Logf("live reading: pane=%s counter=%s at=%s", reading.Pane, reading.Counter, reading.At.Format(time.RFC3339Nano))
	if ops.ended(ctx, lc.b) {
		t.Fatal("the turn is already complete; nothing to interrupt")
	}
	sent := time.Now()
	if err := ops.interrupt(ctx, reading.Pane); err != nil {
		t.Fatal(err)
	}
	ended := false
	for deadline := time.Now().Add(claudeStallEndWait); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if ended = ops.ended(ctx, lc.b); ended {
			break
		}
	}
	if !ended {
		t.Fatalf("the transcript did not show the interrupted turn ending:\n%s", lc.capture(false))
	}
	t.Logf("live interrupt: Escape at %s, transcript showed the turn ended %s later", sent.UTC().Format(time.RFC3339Nano), time.Since(sent).Round(time.Millisecond))
	snap, err := claudeTranscriptSnapshot(lc.b, time.Now())
	if err != nil || snap.Cursor.TurnEndReason != claudeInterruptReason {
		t.Fatalf("turn end reason %q err %v", snap.Cursor.TurnEndReason, err)
	}
	if err := ops.wake(ctx, lc.b, claudeStallResumeLine); err != nil {
		t.Fatalf("resume line through the wake path: %v\n%s", err, lc.capture(false))
	}
	intent := lc.intent()
	if intent.Phase != "confirmed" || intent.Prompt != claudeStallResumeLine {
		t.Fatalf("resume intent %+v", intent)
	}
	t.Logf("live resume: text %s, Enter %s, confirmed %s", intent.TextAt.Format(time.RFC3339Nano), intent.EnterAt.Format(time.RFC3339Nano), intent.ConfirmedAt.Format(time.RFC3339Nano))
}

// The relay loop runs the stall pass for Claude bindings before delivery and
// wraps the Claude wake path; the activity tick shows a recorded stall.
func TestClaudeStallIsWiredIntoTheRelay(t *testing.T) {
	relay, err := os.ReadFile("relay.go")
	if err != nil {
		t.Fatal(err)
	}
	pass := strings.Index(string(relay), "claudeStallPass(ctx, b, nativeClaudeStallOps(c))")
	wrap := strings.Index(string(relay), "queue = claudeStallWake(claudeQueue, time.Now)")
	deliver := strings.Index(string(relay), "queued, brokerErr = relayWakeJob(ctx, b, &progress, c, now, queue)")
	if pass < 0 || wrap < pass || deliver < wrap {
		t.Fatalf("relay loop order: pass %d, wrap %d, delivery %d", pass, wrap, deliver)
	}
	monitor, err := os.ReadFile("activity_monitor.go")
	if err != nil || !strings.Contains(string(monitor), "claudeStallStuckReason(b, &c, now)") {
		t.Fatalf("activity tick does not read the stall record (%v)", err)
	}
	// The native interrupt is one Escape, sent with send-keys to one pane.
	ops := nativeClaudeStallOps(nil)
	if ops.interrupt == nil || ops.wake == nil || ops.notify == nil || ops.capture == nil || ops.ended == nil || ops.cursor == nil || ops.action == nil {
		t.Fatal("native stall ops are incomplete")
	}
}
