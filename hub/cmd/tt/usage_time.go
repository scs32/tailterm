package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Time accounting rides the usage cursor: the same bounded transcript append
// pass that meters requests also records where a turn's wall time went. All
// times are transcript record timestamps, UTC, half-open intervals. A turn is
// emitted once, when it completes, as bounded chunks (api.UsageSpan). Only
// timestamps, classes and counts are kept; command text never is.
const (
	usageSpanChunkEntries = 32   // intervals and segment pieces in one chunk
	usageSpanMaxChunks    = 64   // measured chunks of one turn
	usageSpanTurnEntries  = 2048 // in-memory bound of an open turn
	usageSpanOutbox       = 256  // chunks kept locally awaiting upload
)

// usageSpanChunkSize is lowered only by tests, to force small chunks.
var usageSpanChunkSize = usageSpanChunkEntries

type usageTimeCall struct {
	Since time.Time `json:"since"`
	Class string    `json:"class"`
}

// usageTimeMark is one metered model request inside the open turn, at the
// timestamp the token ledger gives it, with the tool calls it issued.
type usageTimeMark struct {
	At      time.Time `json:"at"`
	ID      string    `json:"id,omitempty"`
	Actions int       `json:"actions,omitempty"`
	Other   bool      `json:"other,omitempty"` // some action was not inbox-only
}

type usageTimeTurn struct {
	Turn    string                   `json:"turn"`
	Start   time.Time                `json:"start"`
	Last    time.Time                `json:"last"`
	Calls   map[string]usageTimeCall `json:"calls,omitempty"`
	Tool    [][2]time.Time           `json:"tool,omitempty"`
	Wait    [][2]time.Time           `json:"wait,omitempty"`
	Mixed   [][2]time.Time           `json:"mixed,omitempty"`
	Marks   []usageTimeMark          `json:"marks,omitempty"`
	Actions int                      `json:"actions,omitempty"` // Codex: since the previous token_count
	Other   bool                     `json:"other,omitempty"`
	Gap     string                   `json:"gap,omitempty"`
	Digest  string                   `json:"digest"`
	// OverflowAt is set when the turn exceeded the in-memory bound; nothing
	// after it is recorded and that stretch is reported unavailable.
	OverflowAt time.Time `json:"overflowAt,omitempty"`
}

// usageTimeNote carries a new metered request from put to the tracker.
type usageTimeNote struct {
	ID, Activation string
	At             time.Time
}

func (t *usageTimeTurn) addGap(reason string) {
	if strings.Contains(t.Gap, reason) || len(t.Gap)+len(reason)+2 > 240 {
		return
	}
	if t.Gap != "" {
		t.Gap += "; "
	}
	t.Gap += reason
}
func (t *usageTimeTurn) entries() int {
	return len(t.Calls) + len(t.Tool) + len(t.Wait) + len(t.Mixed) + len(t.Marks)
}

// full reports that nothing more may be recorded, setting the overflow time
// on the first refusal.
func (t *usageTimeTurn) full(at time.Time) bool {
	if !t.OverflowAt.IsZero() {
		return true
	}
	if t.entries() >= usageSpanTurnEntries {
		t.OverflowAt = at
		t.addGap("turn exceeded the interval bound")
		return true
	}
	return false
}

// clock keeps record time monotonic inside a turn: an earlier timestamp is
// clamped to the last one seen and the turn says so.
func (t *usageTimeTurn) clock(at time.Time) time.Time {
	if at.Before(t.Last) {
		t.addGap("record timestamps out of order; clamped")
		return t.Last
	}
	t.Last = at
	return at
}
func (t *usageTimeTurn) open(id string, class usageToolClass, at time.Time) {
	if id == "" || t.full(at) {
		return
	}
	if t.Calls == nil {
		t.Calls = map[string]usageTimeCall{}
	}
	t.Calls[id] = usageTimeCall{Since: at, Class: class.Class}
}
func (t *usageTimeTurn) close(id string, at time.Time) {
	call, ok := t.Calls[id]
	if !ok {
		return
	}
	delete(t.Calls, id)
	if !t.OverflowAt.IsZero() && at.After(t.OverflowAt) {
		at = t.OverflowAt
	}
	if !at.After(call.Since) {
		return
	}
	interval := [2]time.Time{call.Since, at}
	switch call.Class {
	case usageToolWaiting:
		t.Wait = append(t.Wait, interval)
	case usageToolMixed:
		t.Mixed = append(t.Mixed, interval)
	default:
		t.Tool = append(t.Tool, interval)
	}
}

func (u *usageCursor) timeBegin(at time.Time, turn, gap string) {
	if u.Time != nil {
		u.timeClose(u.Time.Last, "turn end not observed")
	}
	if turn == "" {
		// The token ledger's own formula, so both name the turn alike.
		turn = "activation-" + fmt.Sprintf("%x", sha256.Sum256([]byte(u.Binding.Thread+at.String())))[:24]
		if u.Activation != "" && u.Start.Equal(at) {
			turn = u.Activation
		}
	}
	u.Time = &usageTimeTurn{Turn: turn, Start: at, Last: at, Gap: gap}
}

// timeMark records a request the ledger just metered. The first one also
// gives the turn the activation id its token turns carry.
func (u *usageCursor) timeMark(note *usageTimeNote, at time.Time) {
	t := u.Time
	if note == nil || t == nil || t.full(at) {
		return
	}
	if len(t.Marks) == 0 && note.Activation != "" && !u.SpanFinished[note.Activation] {
		t.Turn = note.Activation
	}
	t.Marks = append(t.Marks, usageTimeMark{At: at, ID: note.ID})
}

// parseTime runs after the token ledger has parsed the same line.
func (u *usageCursor) parseTime(line []byte) {
	note := u.note
	u.note = nil
	if u.WrongEpoch {
		return
	}
	var rec activityRecord
	if json.Unmarshal(line, &rec) != nil || rec.Type == "session_meta" {
		return
	}
	at, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return
	}
	at = at.UTC()
	if u.Binding.Runtime == "claude" {
		u.parseClaudeTime(line, rec, at, note)
	} else {
		u.parseCodexTime(rec, at, note)
	}
	if t := u.Time; t != nil {
		t.Digest = fmt.Sprintf("%x", sha256.Sum256(append([]byte(t.Digest), line...)))
	}
}

func (u *usageCursor) parseCodexTime(rec activityRecord, at time.Time, note *usageTimeNote) {
	var p struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		TurnID    string          `json:"turn_id"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
	}
	_ = json.Unmarshal(rec.Payload, &p)
	event := rec.Type
	if rec.Type == "event_msg" || rec.Type == "response_item" {
		event = p.Type
	}
	if event == "task_started" && rec.Type != "response_item" {
		u.timeBegin(at, p.TurnID, "")
		return
	}
	implicit := func() {
		if u.Time == nil {
			u.timeBegin(at, "", "turn start not observed")
		}
	}
	switch {
	case rec.Type != "response_item" && (event == "task_complete" || event == "result"):
		if u.Time != nil {
			u.timeClose(u.Time.clock(at), "")
		}
		return
	case rec.Type == "response_item" && (event == "function_call" || event == "custom_tool_call"):
		implicit()
		t := u.Time
		at = t.clock(at)
		args := p.Arguments
		if event == "custom_tool_call" {
			args = p.Input
		}
		class := classifyUsageTool("codex", event, p.Name, args)
		t.open(p.CallID, class, at)
		t.Actions++
		if !class.InboxOnly {
			t.Other = true
		}
	case rec.Type == "response_item" && (event == "function_call_output" || event == "custom_tool_call_output"):
		if t := u.Time; t != nil {
			t.close(p.CallID, t.clock(at))
		}
	case rec.Type == "event_msg" && event == "token_count":
		if note != nil {
			implicit()
		}
		t := u.Time
		if t == nil {
			return
		}
		at = t.clock(at)
		if note != nil {
			u.timeMark(note, at)
			if n := len(t.Marks); n > 0 && t.Marks[n-1].ID == note.ID {
				t.Marks[n-1].Actions, t.Marks[n-1].Other = t.Actions, t.Other
			}
		}
		// Calls belong to the request whose token_count follows them.
		t.Actions, t.Other = 0, false
	default:
		if t := u.Time; t != nil {
			t.clock(at)
		}
	}
}

func (u *usageCursor) parseClaudeTime(line []byte, rec activityRecord, at time.Time, note *usageTimeNote) {
	ended, _ := claudeTurnEnd(line)
	var msg struct {
		ID      string `json:"id"`
		Content []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
		} `json:"content"`
	}
	_ = json.Unmarshal(rec.Message, &msg)
	switch rec.Type {
	case "user":
		if claudeUserText(line) != "" {
			u.timeBegin(at, "", "")
			return
		}
		t := u.Time
		if t == nil {
			return
		}
		at = t.clock(at)
		for _, part := range msg.Content {
			if part.Type == "tool_result" {
				t.close(part.ToolUseID, at)
			}
		}
	case "assistant":
		if u.Time == nil {
			u.timeBegin(at, "", "turn start not observed")
		}
		t := u.Time
		at = t.clock(at)
		u.timeMark(note, at)
		for _, part := range msg.Content {
			if part.Type != "tool_use" || part.ID == "" {
				continue
			}
			class := classifyUsageTool("claude", "tool_use", part.Name, part.Input)
			t.open(part.ID, class, at)
			// A request's actions are the tool_use parts of its message id.
			for i := len(t.Marks) - 1; i >= 0; i-- {
				if t.Marks[i].ID == "claude-"+msg.ID {
					t.Marks[i].Actions++
					if !class.InboxOnly {
						t.Marks[i].Other = true
					}
					break
				}
			}
		}
		if ended {
			u.timeClose(at, "")
		}
	default:
		t := u.Time
		if t == nil {
			return
		}
		at = t.clock(at)
		if ended {
			u.timeClose(at, "")
		}
	}
}

// timeClose emits the open turn as chunks. A turn with no end record closes
// at the last record timestamp seen, with the reason in gap.
func (u *usageCursor) timeClose(end time.Time, gap string) {
	t := u.Time
	u.Time = nil
	if t == nil {
		return
	}
	if gap != "" {
		t.addGap(gap)
	}
	if !end.After(t.Start) || u.SpanFinished[t.Turn] {
		return
	}
	handled, partial := usageHandled(u.Binding, t.Start, end)
	if partial {
		u.Coverage = "partial: handled context retention exceeded"
	}
	chunks := usageSpanChunks(t, end, handled, usageSpanChunkSize)
	if len(u.Spans)+len(chunks) > usageSpanOutbox {
		u.Coverage = "partial: time span outbox capacity exceeded"
		return
	}
	if u.Spans == nil {
		u.Spans = map[string]api.UsageSpan{}
	}
	if u.SpanDirty == nil {
		u.SpanDirty = map[string]bool{}
	}
	if u.SpanFinished == nil {
		u.SpanFinished = map[string]bool{}
	}
	for _, chunk := range chunks {
		u.Spans[chunk.ID] = chunk
		u.SpanDirty[chunk.ID] = true
	}
	u.SpanFinished[t.Turn] = true
	if len(u.SpanFinished) > 4096 {
		u.Coverage = "partial: old turn deduplication retention exhausted"
		u.SpanFinished = map[string]bool{t.Turn: true}
	}
}

// usageUnion returns the exact union of intervals: overlapping or touching
// ones merge, and a gap is never bridged.
func usageUnion(in [][2]time.Time) [][2]time.Time {
	sorted := append([][2]time.Time(nil), in...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i][0].Before(sorted[j][0]) })
	var out [][2]time.Time
	for _, x := range sorted {
		if !x[1].After(x[0]) {
			continue
		}
		if n := len(out); n > 0 && !x[0].After(out[n-1][1]) {
			if x[1].After(out[n-1][1]) {
				out[n-1][1] = x[1]
			}
			continue
		}
		out = append(out, x)
	}
	return out
}

// usageSubtract removes cut from in. Both are sorted, disjoint unions.
func usageSubtract(in, cut [][2]time.Time) [][2]time.Time {
	var out [][2]time.Time
	for _, x := range in {
		from := x[0]
		for _, c := range cut {
			if !c[1].After(from) {
				continue
			}
			if !c[0].Before(x[1]) {
				break
			}
			if c[0].After(from) {
				out = append(out, [2]time.Time{from, c[0]})
			}
			from = c[1]
		}
		if from.Before(x[1]) {
			out = append(out, [2]time.Time{from, x[1]})
		}
	}
	return out
}
func usageClip(in [][2]time.Time, from, to time.Time) [][2]time.Time {
	var out [][2]time.Time
	for _, x := range in {
		if x[0].Before(from) {
			x[0] = from
		}
		if x[1].After(to) {
			x[1] = to
		}
		if x[1].After(x[0]) {
			out = append(out, x)
		}
	}
	return out
}

type usageSpanEntry struct {
	kind     int // 0 segment, 1 tool, 2 wait, 3 mixed
	from, to time.Time
	at       time.Time
	poll     bool
}

// usageSpanChunks cuts a completed turn into consecutive chunks that cover
// [start,end) exactly, each holding at most size entries; an interval or
// segment crossing a cut is split at the cut and nothing is merged to fit.
// Past the chunk limit or the in-memory bound, the remainder is one
// unavailable chunk with no intervals. size is a parameter for tests only.
func usageSpanChunks(t *usageTimeTurn, end time.Time, handled []api.UsageEvidence, size int) []api.UsageSpan {
	if size < 2 {
		size = 2
	}
	measured := end
	if !t.OverflowAt.IsZero() && t.OverflowAt.Before(end) {
		measured = t.OverflowAt
	}
	if measured.Before(t.Start) {
		measured = t.Start
	}
	raw := map[int][][2]time.Time{1: append([][2]time.Time(nil), t.Tool...), 2: append([][2]time.Time(nil), t.Wait...), 3: append([][2]time.Time(nil), t.Mixed...)}
	for _, call := range t.Calls {
		kind := 1
		if call.Class == usageToolWaiting {
			kind = 2
		} else if call.Class == usageToolMixed {
			kind = 3
		}
		// A call with no output closes at turn end.
		raw[kind] = append(raw[kind], [2]time.Time{call.Since, measured})
	}
	mixed := usageClip(usageUnion(raw[3]), t.Start, measured)
	wait := usageSubtract(usageClip(usageUnion(raw[2]), t.Start, measured), mixed)
	tool := usageSubtract(usageSubtract(usageClip(usageUnion(raw[1]), t.Start, measured), wait), mixed)

	// A request is a poll when it has actions, all of them inbox-only, and the
	// next request with actions in the turn, if any, is the same.
	inboxOnly := func(m usageTimeMark) bool { return m.Actions > 0 && !m.Other }
	polls := make([]bool, len(t.Marks))
	for i, m := range t.Marks {
		polls[i] = inboxOnly(m)
		for j := i + 1; j < len(t.Marks) && polls[i]; j++ {
			if t.Marks[j].Actions > 0 {
				polls[i] = inboxOnly(t.Marks[j])
				break
			}
		}
	}
	// Segment i runs from the previous mark (or the turn start) to its mark;
	// the last one extends to the turn end. No mark: one segment at the start.
	owner := func(at time.Time) (time.Time, bool) {
		for i, m := range t.Marks {
			if !m.At.Before(at) {
				return m.At, polls[i]
			}
		}
		if n := len(t.Marks); n > 0 {
			return t.Marks[n-1].At, polls[n-1]
		}
		return t.Start, false
	}
	var entries []usageSpanEntry
	if len(t.Marks) == 0 {
		entries = append(entries, usageSpanEntry{from: t.Start, to: measured, at: t.Start})
	}
	from := t.Start
	for i, m := range t.Marks {
		to := m.At
		if i == len(t.Marks)-1 || to.After(measured) {
			to = measured
		}
		if to.Before(from) {
			to = from
		}
		entries = append(entries, usageSpanEntry{from: from, to: to, at: m.At, poll: polls[i]})
		from = to
	}
	for kind, list := range map[int][][2]time.Time{1: tool, 2: wait, 3: mixed} {
		for _, x := range list {
			entries = append(entries, usageSpanEntry{kind: kind, from: x[0], to: x[1]})
		}
	}
	order := func() {
		sort.SliceStable(entries, func(i, j int) bool {
			a, b := entries[i], entries[j]
			if !a.from.Equal(b.from) {
				return a.from.Before(b.from)
			}
			// A zero-length segment is consumed before anything it touches.
			if za, zb := a.to.Equal(a.from), b.to.Equal(b.from); za != zb {
				return za
			}
			if a.kind != b.kind {
				return a.kind < b.kind
			}
			return a.at.Before(b.at)
		})
	}
	order()
	var out []api.UsageSpan
	add := func(span api.UsageSpan) {
		span.Turn, span.Chunk = t.Turn, len(out)
		span.ID = fmt.Sprintf("%s#%d", t.Turn, span.Chunk)
		span.Handled, span.Gap, span.SourceDigest = handled, t.Gap, t.Digest
		out = append(out, span)
	}
	cur := t.Start
	for len(entries) > 0 && len(out) < usageSpanMaxChunks {
		n := min(size, len(entries))
		cut := measured
		if n < len(entries) {
			cut = entries[n].from
		}
		span := api.UsageSpan{Start: cur, End: cut}
		rest := append([]usageSpanEntry(nil), entries[n:]...)
		for _, e := range entries[:n] {
			zero := e.to.Equal(e.from)
			if e.to.After(cut) {
				carried := e
				carried.from = cut
				rest = append(rest, carried)
				e.to = cut
			}
			if !zero && !e.to.After(e.from) {
				continue // the whole entry lies beyond the cut
			}
			switch e.kind {
			case 0:
				span.Segments = append(span.Segments, api.UsageSpanSegment{From: e.from, To: e.to, At: e.at, Poll: e.poll})
			case 1:
				span.Tool = append(span.Tool, [2]time.Time{e.from, e.to})
			case 2:
				span.Wait = append(span.Wait, [2]time.Time{e.from, e.to})
			case 3:
				span.Mixed = append(span.Mixed, [2]time.Time{e.from, e.to})
			}
		}
		entries = rest
		order()
		add(span)
		cur = cut
	}
	if cur.Before(end) {
		at, _ := owner(cur)
		add(api.UsageSpan{Start: cur, End: end, Unavailable: true, Segments: []api.UsageSpanSegment{{From: cur, To: end, At: at}}})
	}
	if n := len(out); n > 0 {
		out[n-1].Last = true
	}
	return out
}

// hasPendingWork covers dirty token turns, dirty spans, a pending batch and
// unsent coverage. A frozen run is drained only when none is left.
func (u *usageCursor) hasPendingWork() bool {
	return len(u.Dirty) > 0 || len(u.SpanDirty) > 0 || u.Pending != nil || !u.coverageSent()
}
func (u *usageCursor) coverageSent() bool {
	return (u.Enrolled && u.UploadedCoverage == u.Coverage) || u.RejectedCoverage == u.Coverage
}

// hasSendableWork leaves out spans a hub without usage.time cannot store.
func (u *usageCursor) hasSendableWork() bool {
	return len(u.Dirty) > 0 || u.Pending != nil || !u.coverageSent() || (len(u.SpanDirty) > 0 && usageTimeSupported(u.Binding))
}

func usageCapabilityPath(hub string) string {
	return filepath.Join(relayDir(), fmt.Sprintf("%x.usage-capability.json", sha256.Sum256([]byte(hub))))
}

type usageCapabilityCache struct {
	At        time.Time `json:"at"`
	Supported bool      `json:"supported"`
	Time      bool      `json:"time"`
}

// usageTimeSupported reads the host's cached capability answer for this hub.
// Unknown means no: spans then stay dirty locally and none is lost.
func usageTimeSupported(b runtimeBinding) bool {
	var cached usageCapabilityCache
	raw, _ := os.ReadFile(usageCapabilityPath(b.Hub))
	_ = json.Unmarshal(raw, &cached)
	return cached.Supported && cached.Time
}

// dirtySpans returns the unsent chunks in turn and chunk order.
func (u *usageCursor) dirtySpans() []api.UsageSpan {
	out := []api.UsageSpan{}
	for id, dirty := range u.SpanDirty {
		if span, ok := u.Spans[id]; ok && dirty {
			out = append(out, span)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		if out[i].Turn != out[j].Turn {
			return out[i].Turn < out[j].Turn
		}
		return out[i].Chunk < out[j].Chunk
	})
	return out
}
