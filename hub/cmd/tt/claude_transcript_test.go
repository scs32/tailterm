package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// claudeTranscriptFixture installs a sanitized transcript from
// testdata/claude-transcript, plus any extra records, where the real
// activityTranscript discovery finds it for testClaudeBinding.
func claudeTranscriptFixture(t *testing.T, fixture string, extra ...string) (runtimeBinding, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	b := testClaudeBinding()
	data, err := os.ReadFile(filepath.Join("testdata", "claude-transcript", fixture+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "projects", "fixture", b.Thread+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	appendClaudeRecords(t, path, extra...)
	return b, path
}

func appendClaudeRecords(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// resetClaudeRecordLog forgets logged types, so log-once checks hold under -count.
func resetClaudeRecordLog() {
	claudeRecordLog.Lock()
	claudeRecordLog.seen = nil
	claudeRecordLog.Unlock()
}

// Times from the fixtures' real records.
var (
	queueQueuedEnqueue = time.Date(2026, 9, 27, 14, 55, 52, 965000000, time.UTC)
	queueQueuedEnd     = time.Date(2026, 9, 27, 14, 55, 56, 270000000, time.UTC)
	queueIdleEnd       = time.Date(2026, 9, 25, 18, 57, 32, 0, time.UTC)
)

const (
	claudeDequeueRecord = `{"type":"queue-operation","operation":"dequeue","timestamp":"2026-09-27T14:55:56.300Z","sessionId":"11111111-1111-4111-8111-111111111111"}`
	claudeQueuedPrompt  = `{"type":"user","timestamp":"2026-09-27T14:55:56.310Z","message":{"role":"user","content":"Fixture background notification."}}`
	claudeEndTurn       = `{"type":"assistant","timestamp":"2026-09-27T14:56:01.000Z","message":{"id":"msg_fixture_end","role":"assistant","content":[{"type":"text","text":"Fixture answer."}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}}`
)

// a1: queue-operation records are understood and never change turn state.
func TestClaudeQueueOperationRecords(t *testing.T) {
	at := time.Date(2026, 9, 28, 17, 22, 4, 0, time.UTC)
	for _, complete := range []bool{true, false} {
		c := activityCursor{SeenTurn: true, TurnComplete: complete}
		for _, tc := range []struct {
			line  string
			depth int
		}{
			{`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-09-28T17:22:04.109Z","content":"x"}`, 1},
			{`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-09-28T17:22:04.110Z","content":"y"}`, 2},
			{`{"type":"queue-operation","operation":"dequeue","timestamp":"2026-09-28T17:22:04.161Z"}`, 1},
			{`{"type":"queue-operation","operation":"remove","timestamp":"2026-09-28T17:22:04.170Z","content":"y","reason":"absorbed_mid_turn"}`, 0},
			{`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-09-28T17:22:04.180Z","content":"z"}`, 1},
			{`{"type":"queue-operation","operation":"remove","timestamp":"2026-09-28T17:22:04.190Z","content":"z","reason":"delivered_to_agent"}`, 0},
			{`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-09-28T17:22:04.200Z","content":"w"}`, 1},
			{`{"type":"queue-operation","operation":"remove","timestamp":"2026-09-28T17:22:04.210Z","content":"w"}`, 0},
			{`{"type":"queue-operation","operation":"remove","timestamp":"2026-09-28T17:22:04.220Z"}`, 0},
		} {
			if err := parseClaudeActivity([]byte(tc.line), &c); err != nil {
				t.Fatalf("%s: %v", tc.line, err)
			}
			if c.TurnComplete != complete || !c.SeenTurn || c.ClaudeQueued != tc.depth {
				t.Fatalf("%s: complete=%v queued=%d, want complete=%v queued=%d", tc.line, c.TurnComplete, c.ClaudeQueued, complete, tc.depth)
			}
		}
		if !c.ClaudeQueuedAt.IsZero() {
			t.Fatalf("empty queue kept a time: %s", c.ClaudeQueuedAt)
		}
	}
	c := activityCursor{SeenTurn: true, TurnComplete: true}
	if err := parseClaudeActivity([]byte(`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-09-28T17:22:04Z","content":"x"}`), &c); err != nil || !c.ClaudeQueuedAt.Equal(at) {
		t.Fatalf("enqueue time %s err %v", c.ClaudeQueuedAt, err)
	}
	// A real prompt starts a turn and clears queue drift.
	if err := parseClaudeActivity([]byte(`{"type":"user","message":{"content":"next prompt"}}`), &c); err != nil || c.ClaudeQueued != 0 || !c.ClaudeQueuedAt.IsZero() || c.TurnComplete {
		t.Fatalf("prompt did not reset queue: %+v %v", c, err)
	}
	var unknown unknownClaudeRecordError
	err := parseClaudeActivity([]byte(`{"type":"queue-operation","operation":"flush"}`), &c)
	if !errors.As(err, &unknown) || unknown.Type != "queue-operation/flush" {
		t.Fatalf("unknown operation: %v", err)
	}
	err = parseClaudeActivity([]byte(`{"type":"future_record","message":{}}`), &c)
	if !errors.As(err, &unknown) || err.Error() != `unknown Claude record type "future_record"` {
		t.Fatalf("future type: %v", err)
	}
}

// a2: real queue-operation fixtures decide idleness from turn state.
func TestClaudeTranscriptQueueFixtures(t *testing.T) {
	for _, fixture := range []string{"queue-idle", "queue-remove-noreason"} {
		t.Run(fixture, func(t *testing.T) {
			b, _ := claudeTranscriptFixture(t, fixture)
			snap, err := claudeTranscriptSnapshot(b, queueIdleEnd)
			if err != nil || !snap.Cursor.TurnComplete || snap.Cursor.ClaudeQueued != 0 || len(snap.UnknownTypes) != 0 {
				t.Fatalf("idle fixture refused: %v %+v", err, snap)
			}
		})
	}
	t.Run("queue-busy", func(t *testing.T) {
		b, _ := claudeTranscriptFixture(t, "queue-busy")
		_, err := claudeTranscriptSnapshot(b, time.Date(2026, 9, 25, 15, 42, 10, 0, time.UTC))
		if err == nil || !strings.HasSuffix(err.Error(), ": turn in progress") || strings.Contains(err.Error(), "record type") {
			t.Fatalf("busy fixture: %v", err)
		}
	})
	t.Run("queue-queued", func(t *testing.T) {
		b, path := claudeTranscriptFixture(t, "queue-queued")
		now := queueQueuedEnd.Add(time.Second)
		_, err := claudeTranscriptSnapshot(b, now)
		if err == nil || !strings.HasSuffix(err.Error(), ": queued input pending (1)") {
			t.Fatalf("queued input at turn end: %v", err)
		}
		appendClaudeRecords(t, path, claudeDequeueRecord, claudeQueuedPrompt)
		if _, err := claudeTranscriptSnapshot(b, now); err == nil || !strings.HasSuffix(err.Error(), ": turn in progress") {
			t.Fatalf("dequeued prompt: %v", err)
		}
		appendClaudeRecords(t, path, claudeEndTurn)
		if snap, err := claudeTranscriptSnapshot(b, now.Add(5*time.Second)); err != nil || snap.Cursor.ClaudeQueued != 0 {
			t.Fatalf("completed queued turn: %v %+v", err, snap.Cursor)
		}
	})
}

// a3: an unknown record type after a completed turn is logged once, without
// content, and does not block.
func TestClaudeTranscriptUnknownTypeLogsOnce(t *testing.T) {
	resetClaudeRecordLog()
	const planted = "PLANTED-CONTENT-a3"
	unknown := `{"type":"tt-fixture-future-a3","timestamp":"2026-09-25T18:57:31.500Z","sessionId":"11111111-1111-4111-8111-111111111111","note":"` + planted + `","bad\nkey x":1,"message":{"content":"` + planted + `"}}`
	b, _ := claudeTranscriptFixture(t, "queue-idle", unknown)
	var snaps []claudeWakeSnapshot
	out, err := captureRelayOutput(t, true, func() error {
		for i := 0; i < 2; i++ {
			snap, err := claudeTranscriptSnapshot(b, queueIdleEnd)
			if err != nil {
				return err
			}
			snaps = append(snaps, snap)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unknown type blocked: %v", err)
	}
	if fmt.Sprint(snaps[0].UnknownTypes) != "[tt-fixture-future-a3]" || !snaps[0].Cursor.TurnComplete {
		t.Fatalf("snapshot %+v", snaps[0])
	}
	want := b.Agent + ` Claude transcript record type "tt-fixture-future-a3" not recognized; idle from turn state and pane check; sample=keys=[bad?key?x message note sessionId timestamp type] type="tt-fixture-future-a3"`
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, want) || strings.Contains(out, planted) {
		t.Fatalf("log %q, want one line with %q", out, want)
	}
}

// a3: a relay pass wakes an idle Claude through the real transcript check
// when an unknown record follows the completed turn.
func TestClaudeWakeUnknownRecordAfterTurn(t *testing.T) {
	b, hub, c, pane := needsInputFixture(t)
	unknown := `{"type":"tt-fixture-future-wake","timestamp":"2026-09-25T18:57:31.500Z"}`
	fb, path := claudeTranscriptFixture(t, "queue-idle", unknown)
	if fb.Thread != b.Thread {
		t.Fatal("fixture binding mismatch")
	}
	pane.transcript = path
	now := queueIdleEnd
	inspect := func(ctx context.Context, b runtimeBinding, expected string) (claudeWakeSnapshot, error) {
		snap, err := pane.inspect(ctx, b, expected)
		if err != nil {
			return claudeWakeSnapshot{}, err
		}
		transcript, err := claudeTranscriptSnapshot(b, now)
		if err != nil {
			return claudeWakeSnapshot{}, err
		}
		snap.Path, snap.FileID, snap.Offset, snap.Cursor = transcript.Path, transcript.FileID, transcript.Offset, transcript.Cursor
		return snap, nil
	}
	queue := func(ctx context.Context, b runtimeBinding, prompt string) error {
		return claudeWakeWith(ctx, b, prompt, claudeWakeOps{inspect: inspect, send: pane.send, sleep: func(time.Duration) {}, now: func() time.Time { return now }})
	}
	hub.update(func(h *needsInputHub) {
		h.agent.ReadUpTo, h.agent.Unread = 40, 1
		h.messages = []api.Message{{Seq: 41, To: b.Agent, From: api.Sender{AgentID: needsInputHandler}}}
	})
	var p relayProgress
	if _, err := captureRelayOutput(t, true, func() error { return relayOne(context.Background(), b, &p, c, now, queue) }); err != nil {
		t.Fatal(err)
	}
	if p.Skip != nil || len(pane.sent) != 1 || p.Wake == nil || p.Wake.Status != "confirmed" || fmt.Sprint(p.Wake.MessageSeqs) != "[41]" {
		t.Fatalf("unknown record blocked the wake: skip=%+v wake=%+v sent=%q", p.Skip, p.Wake, pane.sent)
	}
}

// a4: tolerance of unknown types does not hide a busy turn or malformed data.
func TestClaudeTranscriptStrictWhereItMatters(t *testing.T) {
	t.Run("unknown type in a busy turn", func(t *testing.T) {
		b, _ := claudeTranscriptFixture(t, "queue-busy", `{"type":"tt-fixture-future-a4","timestamp":"2026-09-25T15:42:10.000Z"}`)
		_, err := claudeTranscriptSnapshot(b, time.Date(2026, 9, 25, 15, 42, 11, 0, time.UTC))
		if err == nil || !strings.HasSuffix(err.Error(), ": turn in progress") || strings.Contains(err.Error(), "record type") {
			t.Fatalf("busy turn with unknown record: %v", err)
		}
	})
	t.Run("malformed record after the turn", func(t *testing.T) {
		b, _ := claudeTranscriptFixture(t, "queue-idle", `{"type":"assistant",broken}`)
		_, err := claudeTranscriptSnapshot(b, queueIdleEnd)
		if err == nil || !strings.Contains(err.Error(), "invalid character") {
			t.Fatalf("malformed record: %v", err)
		}
	})
}

// a5: queued input blocks only while fresh; an older unmatched enqueue after a
// completed turn is logged once and the pane decides.
func TestClaudeTranscriptQueuedInputStaleBound(t *testing.T) {
	resetClaudeRecordLog()
	b, _ := claudeTranscriptFixture(t, "queue-queued")
	if _, err := claudeTranscriptSnapshot(b, queueQueuedEnqueue.Add(claudeQueueStaleAfter-time.Second)); err == nil || !strings.HasSuffix(err.Error(), ": queued input pending (1)") {
		t.Fatalf("fresh queued input: %v", err)
	}
	out, err := captureRelayOutput(t, true, func() error {
		for _, age := range []time.Duration{claudeQueueStaleAfter + time.Second, 5 * time.Minute} {
			if _, err := claudeTranscriptSnapshot(b, queueQueuedEnqueue.Add(age)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stale queued input blocked: %v", err)
	}
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, b.Agent+" Claude transcript has 1 queued input older than 30s after a completed turn") {
		t.Fatalf("stale log %q", out)
	}
}

// The wake gate itself refuses fresh queued input.
func TestClaudeWakeRefusesFreshQueuedInput(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	now := time.Date(2026, 9, 28, 17, 22, 5, 0, time.UTC)
	var sent []string
	ops := claudeWakeOps{inspect: func(context.Context, runtimeBinding, string) (claudeWakeSnapshot, error) {
		return claudeWakeSnapshot{Pane: "%1", Screen: "Answer\n❯ \n? for shortcuts\n", Cursor: activityCursor{Ready: true, SeenTurn: true, TurnComplete: true, ClaudeQueued: 1, ClaudeQueuedAt: now.Add(-time.Second)}}, nil
	}, send: func(_ context.Context, _ string, value string, _ bool) error {
		sent = append(sent, value)
		return nil
	}, sleep: func(time.Duration) {}, now: func() time.Time { return now }}
	if err := claudeWakeWith(context.Background(), testClaudeBinding(), "Tailterm messages #1. Run tt inbox --unread --mark-read.", ops); !errors.Is(err, errClaudeWakeUnsafe) || len(sent) != 0 {
		t.Fatalf("fresh queued input woke: %v sent=%q", err, sent)
	}
}

// The persisted activity cursor stays compatible: an empty queue writes no
// count, and a saved cursor without the fields reads as an empty queue.
func TestClaudeQueueCursorCompatible(t *testing.T) {
	data, err := json.Marshal(activityCursor{})
	if err != nil || strings.Contains(string(data), `"claudeQueued":`) {
		t.Fatalf("empty cursor %s %v", data, err)
	}
	var old activityCursor
	if err := json.Unmarshal([]byte(`{"run":"run_1","turnComplete":true,"seenTurn":true}`), &old); err != nil || old.ClaudeQueued != 0 || !old.ClaudeQueuedAt.IsZero() {
		t.Fatalf("empty cursor %s %v", data, err)
	}
}
