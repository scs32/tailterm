package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
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

const usageTimeMarker = "FIXTURE-SECRET-ARG"

// setUsageTimeCapability writes the host's cached capability answer for a hub.
func setUsageTimeCapability(t *testing.T, hub string, timeSupported bool) {
	t.Helper()
	if err := writePrivateJSON(usageCapabilityPath(hub), usageCapabilityCache{At: time.Now().UTC(), Supported: true, Time: timeSupported}); err != nil {
		t.Fatal(err)
	}
}

// usageTimeHub is a real store behind the real HTTP server, with a wrapper
// that counts requests and can lose replies.
type usageTimeHub struct {
	t        *testing.T
	st       *store.Store
	dbPath   string
	srv      *httptest.Server
	client   *api.Client
	task     api.Task
	by       api.Caller
	mu       sync.Mutex
	requests []string
	batches  []api.UsageBatch
	// loseReplies makes the next n usage uploads reach the store and then
	// answer 503, as a reply lost on the way back.
	loseReplies int
}

func newUsageTimeHub(t *testing.T) *usageTimeHub {
	t.Helper()
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	h := &usageTimeHub{t: t, dbPath: filepath.Join(t.TempDir(), "time.sqlite"), by: api.Caller{Node: "fixture", User: "owner"}}
	st, err := store.Open(h.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.st = st
	if h.task, err = st.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Synthetic time accounting"}, h.by); err != nil {
		t.Fatal(err)
	}
	inner := server.New(st, server.StaticIdentity(h.by))
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := readAllBody(r)
		h.mu.Lock()
		h.requests = append(h.requests, r.Method+" "+r.URL.Path)
		lose := false
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/usage") {
			var b api.UsageBatch
			_ = json.Unmarshal(body, &b)
			h.batches = append(h.batches, b)
			if h.loseReplies > 0 {
				h.loseReplies--
				lose = true
			}
		}
		h.mu.Unlock()
		r.Body = readCloser(body)
		if lose {
			inner.ServeHTTP(httptest.NewRecorder(), r)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	if h.client, err = api.NewClient(h.srv.URL, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return h
}
func readAllBody(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

type bodyReader struct{ *bytes.Reader }

func (bodyReader) Close() error      { return nil }
func readCloser(b []byte) bodyReader { return bodyReader{bytes.NewReader(b)} }

func (h *usageTimeHub) agent(name, runtime, role string) (api.Agent, runtimeBinding) {
	h.t.Helper()
	a, err := h.st.AddAgent(context.Background(), h.task.ID, api.AddAgentRequest{Name: name, AgentID: api.NewID("agt"), Host: "fixture", Session: "fixture-" + name, Runtime: runtime, Role: role}, h.by)
	if err != nil {
		h.t.Fatal(err)
	}
	return a, runtimeBinding{Hub: h.srv.URL, Task: h.task.ID, Agent: a.ID, Run: a.RunID, Thread: "12345678-1234-1234-1234-123456789abc", Runtime: runtime}
}
func (h *usageTimeHub) cursor(b runtimeBinding) *usageCursor {
	h.t.Helper()
	u, err := loadUsageCursor(b)
	if err != nil {
		h.t.Fatal(err)
	}
	return u
}

// storedSpans counts the chunks in the database through a second handle.
func (h *usageTimeHub) storedSpans() (n int) {
	h.t.Helper()
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	if err = db.QueryRow(`SELECT count(*) FROM usage_spans`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}
func (h *usageTimeHub) usagePosts() (n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.batches)
}

// drain uploads until nothing is pending, ignoring the 15 second throttle.
func (h *usageTimeHub) drain(u *usageCursor) {
	h.t.Helper()
	for i := 0; u.hasPendingWork() && i < 20; i++ {
		u.LastUploadAttempt = time.Time{}
		if err := uploadUsage(context.Background(), u, h.client); err != nil {
			h.t.Fatal(err)
		}
	}
	if u.hasPendingWork() {
		h.t.Fatalf("outbox not drained: dirty=%d spans=%d pending=%v", len(u.Dirty), len(u.SpanDirty), u.Pending != nil)
	}
}

func assertNoUsageTimeMarker(t *testing.T, what string, raw []byte) {
	t.Helper()
	if bytes.Contains(raw, []byte(usageTimeMarker)) || bytes.Contains(raw, []byte("go test")) || bytes.Contains(raw, []byte("tt inbox")) {
		t.Fatalf("command text retained in %s", what)
	}
}

func TestUsageTimeOutboxRestart(t *testing.T) {
	h := newUsageTimeHub(t)
	_, b := h.agent("builder", "codex", "")
	setUsageTimeCapability(t, b.Hub, true)
	u := h.cursor(b)
	lines := usageTimeFixture(t, "codex-poll.jsonl")
	if !strings.Contains(strings.Join(lines, "\n"), usageTimeMarker) {
		t.Fatal("fixture lost its marker")
	}
	parseUsageLines(t, u, lines...)
	if len(u.SpanDirty) != 4 {
		t.Fatalf("chunks %d", len(u.SpanDirty))
	}
	// The hub stores the batch but its reply is lost.
	h.loseReplies = 1
	if err := uploadUsage(context.Background(), u, h.client); err == nil {
		t.Fatal("lost reply reported success")
	}
	if h.storedSpans() != 4 || len(u.SpanDirty) != 4 || u.Pending == nil {
		t.Fatalf("after lost reply: stored=%d dirty=%d", h.storedSpans(), len(u.SpanDirty))
	}
	key := u.Pending.RequestID
	// The relay restarts and replays the same frozen batch.
	restarted := h.cursor(b)
	if restarted.Pending == nil || restarted.Pending.RequestID != key || len(restarted.Pending.Spans) != 4 {
		t.Fatalf("pending batch after restart: %+v", restarted.Pending)
	}
	h.drain(restarted)
	if h.batches[0].RequestID != key || h.batches[1].RequestID != key {
		t.Fatalf("retry identity changed: %s %s", h.batches[0].RequestID, h.batches[1].RequestID)
	}
	if h.storedSpans() != 4 || len(restarted.Spans) != 0 {
		t.Fatalf("stored=%d kept=%d", h.storedSpans(), len(restarted.Spans))
	}
	// Re-reading the same transcript after the receipts emits nothing new.
	parseUsageLines(t, restarted, lines...)
	if len(restarted.SpanDirty) != 0 {
		t.Fatalf("re-read emitted %d chunks", len(restarted.SpanDirty))
	}

	// No command text in the cursor, any batch, or the database.
	state, err := os.ReadFile(usageStatePath(b))
	if err != nil {
		t.Fatal(err)
	}
	assertNoUsageTimeMarker(t, "cursor", state)
	sent, _ := json.Marshal(h.batches)
	assertNoUsageTimeMarker(t, "batch", sent)
	for _, path := range []string{h.dbPath, h.dbPath + "-wal"} {
		if raw, err := os.ReadFile(path); err == nil {
			assertNoUsageTimeMarker(t, filepath.Base(path), raw)
		}
	}

	// 40 chunks in one pass split across batches and all arrive.
	_, big := h.agent("verifier", "codex", "")
	v := h.cursor(big)
	start := fixtureTime("13:00:00")
	commands := make([]string, 1250)
	calls := make([][2]time.Time, 1250)
	for i := range calls {
		commands[i] = "go test ./..."
		from := start.Add(time.Duration(1+2*i) * time.Second)
		calls[i] = [2]time.Time{from, from.Add(time.Second)}
	}
	parseUsageLines(t, v, syntheticToolTurn("codex", start, start.Add(2600*time.Second), commands, calls)...)
	chunks := len(v.SpanDirty)
	if chunks < 40 {
		t.Fatalf("chunks %d", chunks)
	}
	before := h.usagePosts()
	h.drain(v)
	if posts := h.usagePosts() - before; posts < 2 {
		t.Fatalf("%d chunks went in %d batch", chunks, posts)
	}
	if h.storedSpans() != 4+chunks {
		t.Fatalf("stored %d of %d", h.storedSpans()-4, chunks)
	}
	for _, batch := range h.batches {
		raw, _ := json.Marshal(batch)
		if len(batch.Turns)+len(batch.Spans) > 32 || len(raw) > 48<<10 {
			t.Fatalf("batch over the caps: %d records, %d bytes", len(batch.Turns)+len(batch.Spans), len(raw))
		}
	}
}

func TestUsageTimeRetiredFreeze(t *testing.T) {
	h := newUsageTimeHub(t)
	_, b := h.agent("reviewer", "claude", "")
	setUsageTimeCapability(t, b.Hub, true)
	u := h.cursor(b)
	// The agent retires in the middle of its inbox wait: the turn is open.
	lines := usageTimeFixture(t, "claude-owner-wait.jsonl")
	parseUsageLines(t, u, lines[:2]...)
	if err := saveUsageCursor(u); err != nil {
		t.Fatal(err)
	}
	if err := freezeRetiredUsage(b); err != nil {
		t.Fatal(err)
	}
	frozen := h.cursor(b)
	if !frozen.Frozen || frozen.Time != nil || len(frozen.SpanDirty) != 1 {
		t.Fatalf("frozen=%v open=%v chunks=%d", frozen.Frozen, frozen.Time != nil, len(frozen.SpanDirty))
	}
	// No transcript exists in this test: the flush reads only the outbox.
	clientFor := func(runtimeBinding) (*api.Client, error) { return h.client, nil }
	for i := 0; i < 3 && h.storedSpans() == 0; i++ {
		if err := flushFrozenUsage(context.Background(), relayDir(), clientFor); err != nil {
			t.Fatal(err)
		}
	}
	if h.storedSpans() != 1 {
		t.Fatalf("stored %d", h.storedSpans())
	}
	var span api.UsageSpan
	for _, batch := range h.batches {
		for _, x := range batch.Spans {
			span = x
		}
	}
	// Closed at the last record seen, with the open wait running to that end.
	if !span.End.Equal(fixtureTime("11:00:05")) || !strings.Contains(span.Gap, "terminal boundary") || !span.Last {
		t.Fatalf("frozen span %+v", span)
	}
	if err := flushFrozenUsage(context.Background(), relayDir(), clientFor); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(usageStatePath(b)); !os.IsNotExist(err) {
		t.Fatal("drained frozen cursor retained", err)
	}
	for _, request := range h.requests {
		if !strings.HasSuffix(request, "/usage") {
			t.Fatalf("flush made another request: %s", request)
		}
	}
}

func TestUsageTimeCapabilityGate(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	timeSupported := false
	var batches []api.UsageBatch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			caps := api.CurrentCapabilities()
			caps.Usage.Time = timeSupported
			json.NewEncoder(w).Encode(caps)
			return
		}
		// An older hub decodes loosely: it would acknowledge and drop spans.
		var b api.UsageBatch
		json.NewDecoder(r.Body).Decode(&b)
		batches = append(batches, b)
		json.NewEncoder(w).Encode(api.UsageReceipt{RequestID: b.RequestID, Turns: len(b.Turns), Spans: len(b.Spans)})
	}))
	defer srv.Close()
	c, _ := api.NewClient(srv.URL, time.Second)
	b := runtimeBinding{Hub: srv.URL, Task: "tsk_1111111111111111", Agent: "agt_1111111111111111", Run: "run_1111111111111111", Thread: "12345678-1234-1234-1234-123456789abc", Runtime: "codex"}
	now := time.Now().UTC()
	prepareUsageContext(context.Background(), b, c, now)
	u, err := loadUsageCursor(b)
	if err != nil {
		t.Fatal(err)
	}
	parseUsageLines(t, u, usageTimeFixture(t, "codex-overlap.jsonl")...)
	if err = uploadUsage(context.Background(), u, c); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || len(batches[0].Turns) != 2 || len(batches[0].Spans) != 0 {
		t.Fatalf("hub without usage.time was sent spans: %+v", batches)
	}
	// Nothing is lost and nothing more is sent while the hub cannot store it.
	u.LastUploadAttempt = time.Time{}
	if err = uploadUsage(context.Background(), u, c); err != nil || len(batches) != 1 {
		t.Fatalf("empty upload sent: %d %v", len(batches), err)
	}
	restarted, err := loadUsageCursor(b)
	if err != nil || len(restarted.SpanDirty) != 1 || len(restarted.Spans) != 1 || !restarted.hasPendingWork() || restarted.hasSendableWork() {
		t.Fatalf("spans not kept: %+v %v", restarted.SpanDirty, err)
	}
	// The hub is upgraded; the cached answer expires after five minutes.
	timeSupported = true
	prepareUsageContext(context.Background(), b, c, now.Add(time.Minute))
	if usageTimeSupported(b) {
		t.Fatal("capability cache ignored")
	}
	prepareUsageContext(context.Background(), b, c, now.Add(6*time.Minute))
	restarted.LastUploadAttempt = time.Time{}
	if err = uploadUsage(context.Background(), restarted, c); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[1].Spans) != 1 || len(batches[1].Turns) != 0 || restarted.hasPendingWork() {
		t.Fatalf("spans not sent after upgrade: %+v", batches)
	}
}

// relayTicksWithOverlapFixture runs three relay ticks over the overlap
// transcript through the real append path and returns the requests made.
func relayTicksWithOverlapFixture(t *testing.T, timeSupported bool) (requests []string, usagePosts, stored int) {
	t.Helper()
	h := newUsageTimeHub(t)
	_, b := h.agent("handler", "codex", api.AgentRoleDatabaseHandler)
	b.CodexHome = t.TempDir()
	dir := filepath.Join(b.CodexHome, "sessions", "2026", "01", "01")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	content := strings.Join(usageTimeFixture(t, "codex-overlap.jsonl"), "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-fixture-"+b.Thread+".jsonl"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	setUsageTimeCapability(t, b.Hub, timeSupported)
	ctx := context.WithValue(context.Background(), usageEnabledContextKey{}, true)
	probe := func(runtimeBinding, api.Agent) (bool, bool, error) { return true, true, nil }
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if err := relayActivityTick(ctx, b, h.client, now.Add(time.Duration(16*i)*time.Second), probe); err != nil {
			t.Fatal(err)
		}
		u := h.cursor(b)
		u.LastUploadAttempt = time.Time{}
		if err := saveUsageCursor(u); err != nil {
			t.Fatal(err)
		}
	}
	return h.requests, h.usagePosts(), h.storedSpans()
}

// The spans ride the batch that already carries the token turns.
func TestUsageTimeCapabilityGateRequestCount(t *testing.T) {
	tokenOnly, tokenPosts, none := relayTicksWithOverlapFixture(t, false)
	withTime, timePosts, stored := relayTicksWithOverlapFixture(t, true)
	if none != 0 || stored != 1 {
		t.Fatalf("stored spans: without=%d with=%d", none, stored)
	}
	if len(withTime) != len(tokenOnly) || timePosts != tokenPosts || tokenPosts == 0 {
		t.Fatalf("requests changed: token-only %d (%d uploads), with time %d (%d uploads)", len(tokenOnly), tokenPosts, len(withTime), timePosts)
	}
}

func TestUsageTimeFrozenSpanOnlyDrain(t *testing.T) {
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	fail := false
	var batches []api.UsageBatch
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b api.UsageBatch
		json.NewDecoder(r.Body).Decode(&b)
		batches = append(batches, b)
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(api.UsageReceipt{RequestID: b.RequestID, Turns: len(b.Turns), Spans: len(b.Spans)})
	}))
	defer srv.Close()
	c, _ := api.NewClient(srv.URL, time.Second)
	clientFor := func(runtimeBinding) (*api.Client, error) { return c, nil }
	b := runtimeBinding{Hub: srv.URL, Task: "tsk_1111111111111111", Agent: "agt_1111111111111111", Run: "run_1111111111111111", Thread: "12345678-1234-1234-1234-123456789abc", Runtime: "codex"}
	setUsageTimeCapability(t, b.Hub, false)
	u, err := loadUsageCursor(b)
	if err != nil {
		t.Fatal(err)
	}
	parseUsageLines(t, u, usageTimeFixture(t, "codex-overlap.jsonl")...)
	// Token turns are uploaded; the spans cannot be yet.
	if err = uploadUsage(context.Background(), u, c); err != nil {
		t.Fatal(err)
	}
	if len(u.Dirty) != 0 || len(u.SpanDirty) != 1 || len(batches) != 1 {
		t.Fatalf("dirty=%d spans=%d batches=%d", len(u.Dirty), len(u.SpanDirty), len(batches))
	}
	if err = freezeRetiredUsage(b); err != nil {
		t.Fatal(err)
	}
	exists := func() bool { _, err := os.Stat(usageStatePath(b)); return err == nil }
	resetThrottle := func() {
		t.Helper()
		v, err := loadUsageCursor(b)
		if err != nil {
			t.Fatal(err)
		}
		v.LastUploadAttempt = time.Time{}
		if err = saveUsageCursor(v); err != nil {
			t.Fatal(err)
		}
	}
	// The terminal coverage note goes up; the spans stay and so does the file.
	resetThrottle()
	if err = flushFrozenUsage(context.Background(), relayDir(), clientFor); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 2 || len(batches[1].Spans) != 0 || !exists() {
		t.Fatalf("batches=%d file=%v", len(batches), exists())
	}
	// With only unsendable spans left no request is made and nothing is removed.
	resetThrottle()
	if err = flushFrozenUsage(context.Background(), relayDir(), clientFor); err != nil || len(batches) != 2 || !exists() {
		t.Fatalf("span-only run: batches=%d file=%v err=%v", len(batches), exists(), err)
	}
	// The hub now stores spans, but the first attempt fails: retried later.
	setUsageTimeCapability(t, b.Hub, true)
	fail = true
	if err = flushFrozenUsage(context.Background(), relayDir(), clientFor); err == nil || len(batches) != 3 || !exists() {
		t.Fatalf("failed attempt: batches=%d file=%v err=%v", len(batches), exists(), err)
	}
	fail = false
	resetThrottle()
	if err = flushFrozenUsage(context.Background(), relayDir(), clientFor); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 4 || len(batches[3].Spans) != 1 || len(batches[3].Turns) != 0 || batches[3].RequestID != batches[2].RequestID {
		t.Fatalf("span-only batch: %+v", batches[3])
	}
	// Removed only after the spans' receipt.
	if exists() {
		t.Fatal("drained frozen cursor retained")
	}
	if err = flushFrozenUsage(context.Background(), relayDir(), clientFor); err != nil || len(batches) != 4 {
		t.Fatalf("drained run polled: %d %v", len(batches), err)
	}
}

// boardRow writes one synthetic Board row through a second database handle,
// so the test controls its recorded time.
func (h *usageTimeHub) boardRow(query string, args ...any) int64 {
	h.t.Helper()
	db, err := sql.Open("sqlite", h.dbPath)
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		h.t.Fatal(err)
	}
	r, err := db.Exec(query, args...)
	if err != nil {
		h.t.Fatal(err)
	}
	id, _ := r.LastInsertId()
	return id
}
func (h *usageTimeHub) message(from, to, hms string, envelope api.Envelope, item string) int64 {
	h.t.Helper()
	if item != "" {
		envelope.Refs = map[string]string{"item": item}
	}
	raw, _ := json.Marshal(envelope)
	return h.boardRow(`INSERT INTO messages(task_id,from_agent,from_node,from_user,to_agent,text,created_at,envelope) VALUES(?,?,'fixture','owner',?,'synthetic',?,?)`, h.task.ID, from, to, fixtureTime(hms).Format(time.RFC3339Nano), string(raw))
}
func (h *usageTimeHub) item(title string) api.WorkItem {
	h.t.Helper()
	item, err := h.st.CreateWorkItem(context.Background(), h.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: title, RequestID: "item-" + api.NewID("req")}, h.by)
	if err != nil {
		h.t.Fatal(err)
	}
	return item
}

// journal records which Board messages the agent handled, and when.
func (h *usageTimeHub) journal(b runtimeBinding, entries ...api.UsageEvidence) {
	h.t.Helper()
	if err := writePrivateJSON(usageContextPath(env{agent: b.Agent, runID: b.Run}), entries); err != nil {
		h.t.Fatal(err)
	}
}

func TestUsageTimeCLIReport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	h := newUsageTimeHub(t)
	item := h.item("Synthetic time report")
	_, b := h.agent("builder", "claude", "")
	setUsageTimeCapability(t, b.Hub, true)
	assign := h.message("", b.Agent, "09:59:00", api.Envelope{Kind: "assign", Subject: "Build the synthetic item"}, item.ID)
	// The builder asked the owner before its first turn; the answer came at 11:15.
	question := h.message(b.Agent, "", "09:59:30", api.Envelope{}, "")
	answer := h.message("", "", "11:15:00", api.Envelope{}, "")
	h.boardRow(`INSERT INTO decision_requests(message_seq,task_id,question,options,recommended_option_id,recommendation_reason,created_at) VALUES(?,?,?,?,?,?,?)`, question, h.task.ID, "Approve the <matrix> plan?", "[]", "yes", "synthetic", fixtureTime("09:59:30").Format(time.RFC3339Nano))
	h.boardRow(`INSERT INTO decision_answers(message_seq,task_id,request_seq,option_id,created_at) VALUES(?,?,?,?,?)`, answer, h.task.ID, question, "yes", fixtureTime("11:15:00").Format(time.RFC3339Nano))
	var acks []api.UsageEvidence
	for _, at := range []string{"10:00:30", "10:30:30", "10:40:30", "10:50:05", "11:00:01"} {
		acks = append(acks, api.UsageEvidence{TaskID: h.task.ID, Seq: assign, Operation: "ack", At: fixtureTime(at)})
	}
	h.journal(b, acks...)
	u := h.cursor(b)
	parseUsageLines(t, u, usageTimeFixture(t, "claude-poll.jsonl")...)
	parseUsageLines(t, u, usageTimeFixture(t, "claude-owner-wait.jsonl")...)
	h.drain(u)

	e := env{hub: h.srv.URL, task: h.task.ID}
	raw, err := captureStdout(t, func() error {
		return cmdUsage(e, []string{"--project", h.task.ID, "--item", item.ID, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	// The JSON carries the hub's time section verbatim.
	var report struct {
		TimeVersion int `json:"timeVersion"`
		Items       []struct {
			Time map[string]json.RawMessage `json:"time"`
		} `json:"items"`
	}
	if err = json.Unmarshal([]byte(raw), &report); err != nil || report.TimeVersion != 1 || len(report.Items) != 1 {
		t.Fatalf("report %s %v", raw, err)
	}
	section := report.Items[0].Time
	text := func(key string) string {
		var v string
		if json.Unmarshal(section[key], &v) != nil {
			t.Fatalf("%s missing in %s", key, raw)
		}
		return v
	}
	// Five turns from 10:00:00 to 11:20:12: model 38 s, tool 32 s, the rest waiting.
	if text("wallMs") != "4812000" || text("modelMs") != "38000" || text("toolMs") != "32000" || text("waitingMs") != "4742000" || text("unmeasuredMs") != "0" || text("pollMs") != "11000" {
		t.Fatalf("time %s", raw)
	}
	var polls int
	var phases []api.UsageTimeSplit
	var waits []api.UsageTimeWait
	if json.Unmarshal(section["polls"], &polls) != nil || json.Unmarshal(section["phases"], &phases) != nil || json.Unmarshal(section["waits"], &waits) != nil {
		t.Fatal(raw)
	}
	// Polls: two of the three waits before the ack, the single blocking wait,
	// the non-blocking check, and the wait for the owner.
	if polls != 5 || len(phases) != 1 || phases[0].Key != "build" || phases[0].WaitingMs != "4742000" {
		t.Fatalf("polls %d phases %+v", polls, phases)
	}
	// Waiting while the owner decision was open is the owner's, with its message.
	if len(waits) != 2 || waits[0].Cause != "owner" || waits[0].MessageSeq != question || waits[0].Ms != "4437000" || waits[1].Cause != "unknown" || waits[1].Ms != "305000" {
		t.Fatalf("waits %+v", waits)
	}
	out, err := captureStdout(t, func() error { return cmdUsage(e, []string{"--project", h.task.ID, "--item", item.ID}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  time: wall 1h20m12s · model 0.8% · tool 0.7% · waiting 98.5% · polls 5 (11s)",
		"  timeline: some model working 0.8% · only tools running 0.7% · nobody active 98.5%",
		"  time agent builder (unknown): model 38s · tool 32s · waiting 1h19m2s · polls 5",
		"  time phase build: model 38s · tool 32s · waiting 1h19m2s",
		"  time role unknown: model 38s · tool 32s · waiting 1h19m2s",
		fmt.Sprintf("  wait owner: #%d \"Approve the <matrix> plan?\" · 1h13m57s · awaited by builder", question),
		"  wait unknown: nothing the Board shows · 5m5s · awaited by builder",
		"Project overhead: not measured",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}
	// An item with no spans says so; a hub that reports no time prints nothing.
	other := h.item("Synthetic unmeasured item")
	out, err = captureStdout(t, func() error { return cmdUsage(e, []string{"--project", h.task.ID, "--item", other.ID}) })
	if err != nil || !strings.Contains(out, "  time: not measured") {
		t.Fatalf("unmeasured item: %s %v", out, err)
	}
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.UsageReport{Version: 1, ProjectID: h.task.ID, Items: []api.UsageItemReport{{Title: "Older hub item"}}})
	}))
	defer old.Close()
	out, err = captureStdout(t, func() error { return cmdUsage(env{hub: old.URL, task: h.task.ID}, nil) })
	if err != nil || strings.Contains(out, "  time") || !strings.Contains(out, "Older hub item") {
		t.Fatalf("older hub output: %s %v", out, err)
	}
	// The browser test renders this real report.
	if path := os.Getenv("USAGE_TIME_REPORT_FIXTURE"); path != "" {
		if err = os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// The order-change turn, through the real parser, chunking, HTTP and store:
// the report is the same whether the turn went up whole or in chunks of two
// entries, several of which hold no request of their own.
func TestUsageTimeChunkSizeIndependence(t *testing.T) {
	for _, runtime := range []string{"codex", "claude"} {
		t.Run(runtime, func(t *testing.T) {
			run := func(size int) (*api.UsageTime, int) {
				usageSpanChunkSize = size
				defer func() { usageSpanChunkSize = usageSpanChunkEntries }()
				h := newUsageTimeHub(t)
				item := h.item("Synthetic order change")
				_, b := h.agent("builder", runtime, "")
				setUsageTimeCapability(t, b.Hub, true)
				assign := h.message("", b.Agent, "09:59:00", api.Envelope{Kind: "assign", Subject: "Build the synthetic item"}, item.ID)
				review := h.message("", b.Agent, "10:09:00", api.Envelope{Kind: "review", Subject: "Review a teammate's change", Review: &api.ReviewMetadata{Mode: "general"}}, item.ID)
				h.journal(b, api.UsageEvidence{TaskID: h.task.ID, Seq: assign, Operation: "ack", At: fixtureTime("09:59:55")}, api.UsageEvidence{TaskID: h.task.ID, Seq: review, Operation: "ack", At: fixtureTime("10:10:00")})
				u := h.cursor(b)
				parseUsageLines(t, u, usageTimeFixture(t, runtime+"-order-change.jsonl")...)
				chunks := len(u.SpanDirty)
				h.drain(u)
				report, err := h.client.Usage(context.Background(), h.task.ID, api.UsageQuery{Item: item.ID})
				if err != nil || len(report.Items) != 1 || report.Items[0].Time == nil {
					t.Fatal(report, err)
				}
				// Tokens and time of each request share a phase.
				tokens := map[string]int{}
				for _, phase := range report.Items[0].Phases {
					tokens[phase.Key] = phase.Summary.Requests
				}
				if tokens["build"] != 1 || tokens["review round unavailable"] != len(u.Turns)-1 {
					t.Fatalf("token phases %+v", tokens)
				}
				return report.Items[0].Time, chunks
			}
			whole, one := run(usageSpanChunkEntries)
			small, many := run(2)
			if one != 1 || many < 4 {
				t.Fatalf("chunks: whole %d, forced %d", one, many)
			}
			if len(whole.Phases) != 2 || whole.Phases[0].Key != "build" || whole.Phases[1].Key != "review round unavailable" {
				t.Fatalf("phases %+v", whole.Phases)
			}
			// The first segment, 09:59:50 to 10:00:00, is build; the rest is review.
			build, rest := whole.Phases[0], whole.Phases[1]
			if usageMillis(build.ModelMs)+usageMillis(build.ToolMs) != 10000 || usageMillis(rest.ModelMs)+usageMillis(rest.ToolMs) != 1230000 {
				t.Fatalf("phase split %+v", whole.Phases)
			}
			// The two runs are separate hubs; only the agent id may differ.
			whole.Agents[0].AgentID, small.Agents[0].AgentID = "", ""
			wholeJSON, _ := json.Marshal(whole)
			smallJSON, _ := json.Marshal(small)
			if string(wholeJSON) != string(smallJSON) {
				t.Fatalf("chunk size changed the report:\nwhole %s\nsmall %s", wholeJSON, smallJSON)
			}
		})
	}
}
