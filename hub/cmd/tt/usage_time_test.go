package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Fixtures under testdata/usage-time are synthetic; see its README.
func usageTimeFixture(t *testing.T, name string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "usage-time", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			lines = append(lines, scanner.Text())
		}
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func fixtureTime(hms string) time.Time {
	at, err := time.Parse(time.RFC3339, "2026-01-01T"+hms+"Z")
	if err != nil {
		panic(err)
	}
	return at
}

// usageTimeTotals is what a set of chunks says, in milliseconds.
type usageTimeTotals struct {
	wall, model, tool, wait, mixed, unavailable int64
	polls                                       int
	pollModel                                   int64
	toolIntervals                               [][2]time.Time
}

func intervalMs(list [][2]time.Time) (total int64) {
	for _, x := range list {
		total += x[1].Sub(x[0]).Milliseconds()
	}
	return total
}

// chunksOf returns the turn's chunks in order and checks the chunk contract:
// consecutive, covering the turn exactly, segments covering each chunk.
func chunksOf(t *testing.T, u *usageCursor, turn string) []api.UsageSpan {
	t.Helper()
	var out []api.UsageSpan
	for _, span := range u.Spans {
		if span.Turn == turn {
			out = append(out, span)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Chunk < out[j].Chunk })
	for i, span := range out {
		if span.Chunk != i || span.ID != fmt.Sprintf("%s#%d", turn, i) || span.Last != (i == len(out)-1) {
			t.Fatalf("chunk identity %d: %+v", i, span)
		}
		if i > 0 && !span.Start.Equal(out[i-1].End) {
			t.Fatalf("chunk %d does not continue chunk %d: %v after %v", i, i-1, span.Start, out[i-1].End)
		}
		if n := len(span.Tool) + len(span.Wait) + len(span.Mixed) + len(span.Segments); n > usageSpanChunkEntries {
			t.Fatalf("chunk %d holds %d entries", i, n)
		}
		at := span.Start
		for _, piece := range span.Segments {
			if !piece.From.Equal(at) || piece.To.Before(piece.From) {
				t.Fatalf("chunk %d segments do not cover it: %+v", i, span.Segments)
			}
			at = piece.To
		}
		if !at.Equal(span.End) {
			t.Fatalf("chunk %d segments end at %v, chunk at %v", i, at, span.End)
		}
		for _, list := range [][][2]time.Time{span.Tool, span.Wait, span.Mixed} {
			for _, x := range list {
				if x[0].Before(span.Start) || x[1].After(span.End) || !x[1].After(x[0]) {
					t.Fatalf("chunk %d interval outside it: %v", i, x)
				}
			}
		}
	}
	return out
}

func totalsOf(chunks []api.UsageSpan) usageTimeTotals {
	var out usageTimeTotals
	polls := map[string]bool{}
	for _, span := range chunks {
		length := span.End.Sub(span.Start).Milliseconds()
		out.wall += length
		if span.Unavailable {
			out.unavailable += length
			continue
		}
		busy := usageUnion(append(append(append([][2]time.Time(nil), span.Tool...), span.Wait...), span.Mixed...))
		out.tool += intervalMs(span.Tool)
		out.wait += intervalMs(span.Wait)
		out.mixed += intervalMs(span.Mixed)
		out.model += length - intervalMs(busy)
		out.toolIntervals = append(out.toolIntervals, span.Tool...)
		for _, piece := range span.Segments {
			if !piece.Poll {
				continue
			}
			polls[span.Turn+"/"+piece.At.String()] = true
			out.pollModel += intervalMs(usageSubtract([][2]time.Time{{piece.From, piece.To}}, busy))
		}
	}
	out.polls = len(polls)
	// Intervals split at a chunk cut are one interval again.
	out.toolIntervals = usageUnion(out.toolIntervals)
	return out
}

func turnsOf(u *usageCursor) []string {
	starts := map[string]time.Time{}
	for _, span := range u.Spans {
		if at, ok := starts[span.Turn]; !ok || span.Start.Before(at) {
			starts[span.Turn] = span.Start
		}
	}
	turns := make([]string, 0, len(starts))
	for turn := range starts {
		turns = append(turns, turn)
	}
	sort.Slice(turns, func(i, j int) bool { return starts[turns[i]].Before(starts[turns[j]]) })
	return turns
}

func stamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000Z") }
func jsonLine(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// syntheticToolTurn writes one turn whose tool calls are the given commands,
// each from calls[i][0] to calls[i][1], for either runtime.
func syntheticToolTurn(runtime string, start, end time.Time, commands []string, calls [][2]time.Time) []string {
	var lines []string
	if runtime == "claude" {
		lines = append(lines, jsonLine(map[string]any{"type": "user", "timestamp": stamp(start), "message": map[string]any{"content": "fixture prompt"}}))
		usage := map[string]any{"input_tokens": 3, "cache_read_input_tokens": 80, "cache_creation_input_tokens": 12, "output_tokens": 7, "output_tokens_details": map[string]any{"thinking_tokens": 2}}
		for i, call := range calls {
			id := fmt.Sprintf("tool-%d", i)
			// Content blocks of one message: a single metered request.
			lines = append(lines, jsonLine(map[string]any{"type": "assistant", "timestamp": stamp(call[0]), "message": map[string]any{"id": "msg_generated", "model": "fixture-claude-model", "usage": usage, "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": commands[i]}}}}}))
			lines = append(lines, jsonLine(map[string]any{"type": "user", "timestamp": stamp(call[1]), "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id}}}}))
		}
		lines = append(lines, jsonLine(map[string]any{"type": "assistant", "timestamp": stamp(end), "message": map[string]any{"id": "msg_generated", "model": "fixture-claude-model", "usage": usage, "stop_reason": "end_turn"}}))
		return lines
	}
	lines = append(lines, jsonLine(map[string]any{"type": "event_msg", "timestamp": stamp(start), "payload": map[string]any{"type": "task_started", "turn_id": "generated-turn"}}))
	for i, call := range calls {
		id := fmt.Sprintf("call-%d", i)
		literal, _ := json.Marshal(commands[i])
		lines = append(lines, jsonLine(map[string]any{"type": "response_item", "timestamp": stamp(call[0]), "payload": map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": id, "input": "text(await tools.exec_command({cmd:" + string(literal) + "}));"}}))
		lines = append(lines, jsonLine(map[string]any{"type": "response_item", "timestamp": stamp(call[1]), "payload": map[string]any{"type": "custom_tool_call_output", "call_id": id}}))
	}
	tokens := map[string]int64{"input_tokens": 100, "cached_input_tokens": 80, "cache_write_input_tokens": 0, "output_tokens": 10, "reasoning_output_tokens": 4}
	lines = append(lines, codexUsageEvent(stamp(end), tokens, tokens), jsonLine(map[string]any{"type": "event_msg", "timestamp": stamp(end), "payload": map[string]any{"type": "task_complete"}}))
	return lines
}

func TestUsageTimeCodexOverlappingTools(t *testing.T) {
	u := newSyntheticUsage(t, "codex")
	parseUsageLines(t, u, usageTimeFixture(t, "codex-overlap.jsonl")...)
	if u.Time != nil || len(u.Spans) != 1 {
		t.Fatalf("one completed turn expected: open=%v spans=%d", u.Time != nil, len(u.Spans))
	}
	chunks := chunksOf(t, u, "fixture-codex-overlap")
	got := totalsOf(chunks)
	// Calls 10:00:10-10:00:40 and 10:00:30-10:01:00 in a turn 10:00:00-10:02:00.
	if got.wall != 120000 || got.tool != 50000 || got.model != 70000 || got.wait != 0 || got.mixed != 0 {
		t.Fatalf("totals %+v", got)
	}
	if len(chunks[0].Tool) != 1 || !chunks[0].Tool[0][0].Equal(fixtureTime("10:00:10")) || !chunks[0].Tool[0][1].Equal(fixtureTime("10:01:00")) {
		t.Fatalf("overlapping calls were not merged into one exact interval: %v", chunks[0].Tool)
	}
	if chunks[0].Gap != "" || len(chunks[0].SourceDigest) != 64 {
		t.Fatalf("gap or digest: %+v", chunks[0])
	}
	// Two metered requests: the segment of each ends at its token_count, the
	// last one at the turn end.
	if s := chunks[0].Segments; len(s) != 2 || !s[0].At.Equal(fixtureTime("10:01:00")) || !s[1].At.Equal(fixtureTime("10:02:00")) || s[0].Poll || s[1].Poll {
		t.Fatalf("segments %+v", s)
	}
	// The turn carries the activation id of its token turns.
	for _, turn := range u.Turns {
		if turn.Activation != "fixture-codex-overlap" {
			t.Fatalf("token turn activation %q", turn.Activation)
		}
	}
}

func TestUsageTimeManyIntervalsExact(t *testing.T) {
	start := fixtureTime("12:00:00")
	disjoint := func(n int) ([]string, [][2]time.Time) {
		commands := make([]string, n)
		calls := make([][2]time.Time, n)
		for i := range calls {
			commands[i] = "go test ./..."
			from := start.Add(time.Duration(1+2*i) * time.Second)
			calls[i] = [2]time.Time{from, from.Add(time.Second)}
		}
		return commands, calls
	}
	for _, runtime := range []string{"codex", "claude"} {
		t.Run(runtime+"/33 intervals", func(t *testing.T) {
			u := newSyntheticUsage(t, runtime)
			commands, calls := disjoint(33)
			end := start.Add(70 * time.Second)
			parseUsageLines(t, u, syntheticToolTurn(runtime, start, end, commands, calls)...)
			turns := turnsOf(u)
			if len(turns) != 1 {
				t.Fatal(turns)
			}
			chunks := chunksOf(t, u, turns[0])
			got := totalsOf(chunks)
			if len(chunks) != 2 || got.tool != 33000 || got.model != 37000 || got.wall != 70000 || got.unavailable != 0 {
				t.Fatalf("chunks=%d totals=%+v", len(chunks), got)
			}
			if len(got.toolIntervals) != 33 {
				t.Fatalf("reassembled %d intervals", len(got.toolIntervals))
			}
			for i, x := range got.toolIntervals {
				if !x[0].Equal(calls[i][0]) || !x[1].Equal(calls[i][1]) {
					t.Fatalf("interval %d is %v, want %v", i, x, calls[i])
				}
			}
		})
		t.Run(runtime+"/2100 intervals", func(t *testing.T) {
			u := newSyntheticUsage(t, runtime)
			commands, calls := disjoint(2100)
			end := start.Add(4300 * time.Second)
			parseUsageLines(t, u, syntheticToolTurn(runtime, start, end, commands, calls)...)
			turns := turnsOf(u)
			if len(turns) != 1 {
				t.Fatal(turns)
			}
			chunks := chunksOf(t, u, turns[0])
			if len(chunks) != usageSpanMaxChunks+1 {
				t.Fatalf("chunks %d", len(chunks))
			}
			tail := chunks[len(chunks)-1]
			if !tail.Unavailable || len(tail.Tool)+len(tail.Wait)+len(tail.Mixed) != 0 || !tail.End.Equal(end) || !strings.Contains(tail.Gap, "interval bound") {
				t.Fatalf("tail %+v", tail)
			}
			for _, chunk := range chunks[:usageSpanMaxChunks] {
				if chunk.Unavailable {
					t.Fatal("measured chunk marked unavailable")
				}
			}
			got := totalsOf(chunks)
			// Every reported interval is one of the real ones: nothing invented,
			// nothing bridged, and the rest of the turn is not split at all.
			want := map[time.Time]time.Time{}
			for _, call := range calls {
				want[call[0]] = call[1]
			}
			for _, x := range got.toolIntervals {
				if to, ok := want[x[0]]; !ok || !to.Equal(x[1]) {
					t.Fatalf("invented interval %v", x)
				}
			}
			if got.tool != int64(len(got.toolIntervals))*1000 || got.tool >= 2100000 || got.wall != 4300000 {
				t.Fatalf("totals %+v", got)
			}
			if got.unavailable != end.Sub(tail.Start).Milliseconds() || got.model+got.tool+got.unavailable != got.wall {
				t.Fatalf("unavailable stretch %+v", got)
			}
		})
		t.Run(runtime+"/mixed call is unmeasured", func(t *testing.T) {
			u := newSyntheticUsage(t, runtime)
			from := start.Add(5 * time.Second)
			end := start.Add(11 * time.Minute)
			parseUsageLines(t, u, syntheticToolTurn(runtime, start, end, []string{"tt inbox --wait 9m && go test ./..."}, [][2]time.Time{{from, from.Add(10 * time.Minute)}})...)
			turns := turnsOf(u)
			if len(turns) != 1 {
				t.Fatal(turns)
			}
			got := totalsOf(chunksOf(t, u, turns[0]))
			if got.tool != 0 || got.wait != 0 || got.mixed != 600000 || got.model != 60000 || got.wall != 660000 {
				t.Fatalf("totals %+v", got)
			}
		})
	}
}

func TestUsageTimePollRequests(t *testing.T) {
	for _, runtime := range []string{"codex", "claude"} {
		t.Run(runtime, func(t *testing.T) {
			u := newSyntheticUsage(t, runtime)
			parseUsageLines(t, u, usageTimeFixture(t, runtime+"-poll.jsonl")...)
			turns := turnsOf(u)
			if len(turns) != 4 {
				t.Fatalf("turns %v", turns)
			}
			want := []struct {
				name                  string
				polls                 int
				pollModel, wait, tool int64
			}{
				// Three requests that only wait on the inbox, then one that runs tt ack.
				{"three waits then ack", 2, 3000, 27 * 60000, 1000},
				// One request that only waits, then a text-only request.
				{"one blocking wait", 1, 1000, 9 * 60000, 0},
				// An inbox check issued together with go test is never a poll.
				{"inbox check with go test", 0, 0, 0, 30000},
				// A non-blocking check alone, then a text-only request.
				{"non-blocking check", 1, 2000, 0, 1000},
			}
			for i, w := range want {
				got := totalsOf(chunksOf(t, u, turns[i]))
				if got.polls != w.polls || got.pollModel != w.pollModel || got.wait != w.wait || got.tool != w.tool || got.mixed != 0 {
					t.Errorf("%s: %+v", w.name, got)
				}
			}
		})
	}
}

func TestUsageTimeTurnBoundaries(t *testing.T) {
	t.Run("claude turn ended by turn_duration", func(t *testing.T) {
		u := newSyntheticUsage(t, "claude")
		parseUsageLines(t, u, usageTimeFixture(t, "claude-owner-wait.jsonl")...)
		turns := turnsOf(u)
		if u.Time != nil || len(turns) != 1 {
			t.Fatalf("open=%v turns=%v", u.Time != nil, turns)
		}
		chunks := chunksOf(t, u, turns[0])
		got := totalsOf(chunks)
		if !chunks[0].Start.Equal(fixtureTime("11:00:00")) || !chunks[0].End.Equal(fixtureTime("11:20:12")) || chunks[0].Gap != "" {
			t.Fatalf("turn %+v", chunks[0])
		}
		if got.wait != 20*60000 || got.tool != 0 || got.model != 12000 {
			t.Fatalf("totals %+v", got)
		}
	})
	t.Run("codex turn with no task_complete", func(t *testing.T) {
		u := newSyntheticUsage(t, "codex")
		one := map[string]int64{"input_tokens": 100, "cached_input_tokens": 80, "cache_write_input_tokens": 0, "output_tokens": 10, "reasoning_output_tokens": 4}
		two := map[string]int64{}
		for k, v := range one {
			two[k] = 2 * v
		}
		parseUsageLines(t, u,
			`{"timestamp":"2026-01-01T10:00:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-a"}}`,
			`{"timestamp":"2026-01-01T10:00:05.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"a1","input":"text(await tools.exec_command({cmd:\"ls\"}));"}}`,
			`{"timestamp":"2026-01-01T10:00:10.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"a1"}}`,
			codexUsageEvent("2026-01-01T10:00:10.000Z", one, one),
			// Record types the tracker does not know are ignored, not errors.
			`{"timestamp":"2026-01-01T10:00:15.000Z","type":"world_state","payload":{"note":"fixture"}}`,
			`{"timestamp":"2026-01-01T10:00:20.000Z","type":"compacted","payload":{"note":"fixture"}}`,
			`{"timestamp":"2026-01-01T10:00:20.000Z","type":"token_usage_record","payload":{"note":"fixture"}}`,
			`{"timestamp":"2026-01-01T10:05:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-b"}}`,
			// An output timestamped before its call clamps to zero.
			`{"timestamp":"2026-01-01T10:05:10.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"b1","input":"text(await tools.exec_command({cmd:\"ls\"}));"}}`,
			`{"timestamp":"2026-01-01T10:05:05.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"b1"}}`,
			codexUsageEvent("2026-01-01T10:05:20.000Z", one, two),
			`{"timestamp":"2026-01-01T10:05:30.000Z","type":"event_msg","payload":{"type":"task_complete"}}`,
		)
		a := chunksOf(t, u, "turn-a")
		if len(a) != 1 || !a[0].End.Equal(fixtureTime("10:00:20")) || !strings.Contains(a[0].Gap, "turn end not observed") {
			t.Fatalf("turn closed by the next start: %+v", a)
		}
		if got := totalsOf(a); got.tool != 5000 || got.model != 15000 {
			t.Fatalf("turn a %+v", got)
		}
		b := chunksOf(t, u, "turn-b")
		if len(b) != 1 || !strings.Contains(b[0].Gap, "out of order") {
			t.Fatalf("clamped turn: %+v", b)
		}
		if got := totalsOf(b); got.tool != 0 || got.model != 30000 || got.wall != 30000 {
			t.Fatalf("turn b %+v", got)
		}
		if strings.HasPrefix(u.Coverage, "partial: malformed") {
			t.Fatal(u.Coverage)
		}
	})
	t.Run("open turn is not emitted and survives a restart", func(t *testing.T) {
		u := newSyntheticUsage(t, "claude")
		lines := usageTimeFixture(t, "claude-owner-wait.jsonl")
		parseUsageLines(t, u, lines[:2]...)
		if len(u.Spans) != 0 || u.Time == nil {
			t.Fatal("open turn emitted")
		}
		if err := saveUsageCursor(u); err != nil {
			t.Fatal(err)
		}
		restarted, err := loadUsageCursor(u.Binding)
		if err != nil || restarted.Time == nil {
			t.Fatal(err)
		}
		parseUsageLines(t, restarted, lines[2:]...)
		if got := totalsOf(chunksOf(t, restarted, turnsOf(restarted)[0])); got.wait != 20*60000 || got.model != 12000 {
			t.Fatalf("after restart %+v", got)
		}
	})
}
