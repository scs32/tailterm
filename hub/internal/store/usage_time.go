package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Time accounting (docs/usage-accounting.md, Time). The relay uploads each
// completed turn as immutable chunks; the hub projects every segment piece
// with the token ledger's own rule at ingest and assembles the report, with
// what each wait was for, at read time.
const usageSpansSchema = `CREATE TABLE IF NOT EXISTS usage_spans(
 task_id TEXT NOT NULL,agent_id TEXT NOT NULL,run_id TEXT NOT NULL,span_id TEXT NOT NULL,
 start_at TEXT NOT NULL,end_at TEXT NOT NULL,payload TEXT NOT NULL,projection TEXT NOT NULL,
 PRIMARY KEY(task_id,agent_id,run_id,span_id));
 CREATE INDEX IF NOT EXISTS usage_spans_time ON usage_spans(task_id,start_at);`

const usageSpanMaxEntries = 32

// usageSpanPiece is a stretch of one chunk with a single attribution.
type usageSpanPiece struct {
	From        time.Time              `json:"from"`
	To          time.Time              `json:"to"`
	Shares      []api.UsageAttribution `json:"shares"`
	Phase       string                 `json:"phase"`
	ReviewRound int                    `json:"reviewRound,omitempty"`
	Role        string                 `json:"role"`
}
type usageSpanProjection struct {
	AgentID string           `json:"agentId"`
	RunID   string           `json:"runId"`
	Pieces  []usageSpanPiece `json:"pieces"`
}

func validUsageSpan(s api.UsageSpan) bool {
	if s.Turn == "" || len(s.Turn) > 160 || s.Chunk < 0 || s.Chunk > 64 || s.ID != fmt.Sprintf("%s#%d", s.Turn, s.Chunk) {
		return false
	}
	for _, r := range s.Turn {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	if s.Start.IsZero() || s.End.Before(s.Start) || len(s.Gap) > 240 || len(s.SourceDigest) != 64 || len(s.Handled) > 64 {
		return false
	}
	if _, err := hex.DecodeString(s.SourceDigest); err != nil {
		return false
	}
	if len(s.Segments) == 0 || len(s.Tool)+len(s.Wait)+len(s.Mixed)+len(s.Segments) > usageSpanMaxEntries {
		return false
	}
	if s.Unavailable && len(s.Tool)+len(s.Wait)+len(s.Mixed) > 0 {
		return false
	}
	// Segments cover the chunk exactly.
	at := s.Start
	for _, piece := range s.Segments {
		if piece.At.IsZero() || !piece.From.Equal(at) || piece.To.Before(piece.From) {
			return false
		}
		at = piece.To
	}
	if !at.Equal(s.End) {
		return false
	}
	// Tool, wait and mixed intervals lie inside the chunk and never overlap.
	var all [][2]time.Time
	for _, list := range [][][2]time.Time{s.Tool, s.Wait, s.Mixed} {
		for _, x := range list {
			if x[0].Before(s.Start) || x[1].After(s.End) || !x[1].After(x[0]) {
				return false
			}
			all = append(all, x)
		}
	}
	if spanLength(spanUnion(all)) != spanLength(all) {
		return false
	}
	for _, e := range s.Handled {
		if !api.ValidID(e.TaskID, "tsk") || e.Seq < 1 || e.At.IsZero() || (e.Operation != "ack" && e.Operation != "progress" && e.Operation != "reply" && e.Operation != "post") {
			return false
		}
	}
	return true
}

// storeUsageSpans keeps each new chunk with its projection. A chunk is
// immutable: an identical replay is a no-op and a different payload under
// the same id is a conflict.
func storeUsageSpans(ctx context.Context, tx *sql.Tx, task, agent, run string, spans []api.UsageSpan, provenance usageRunProvenance) error {
	for _, span := range spans {
		payload, _ := json.Marshal(span)
		var old string
		err := tx.QueryRowContext(ctx, `SELECT payload FROM usage_spans WHERE task_id=? AND agent_id=? AND run_id=? AND span_id=?`, task, agent, run, span.ID).Scan(&old)
		if err == nil {
			if old != string(payload) {
				return api.ErrConflict
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		projection, err := projectUsageSpan(ctx, tx, task, agent, run, span, provenance)
		if err != nil {
			return err
		}
		encoded, _ := json.Marshal(projection)
		if _, err = tx.ExecContext(ctx, `INSERT INTO usage_spans VALUES(?,?,?,?,?,?,?,?)`, task, agent, run, span.ID, ts(span.Start), ts(span.End), string(payload), string(encoded)); err != nil {
			return err
		}
	}
	return nil
}

// projectUsageSpan resolves every segment piece at the timestamp of the
// request that owns it, exactly as that request's tokens are resolved, so a
// request and the time in its segment always get the same item, phase and
// role. Each piece names its own request, so the result does not depend on
// where the turn was cut into chunks.
func projectUsageSpan(ctx context.Context, tx *sql.Tx, task, agent, run string, span api.UsageSpan, provenance usageRunProvenance) (usageSpanProjection, error) {
	out := usageSpanProjection{AgentID: agent, RunID: run, Pieces: []usageSpanPiece{}}
	resolved := map[time.Time]api.UsageProjection{}
	for _, segment := range span.Segments {
		key := segment.At.UTC()
		p, ok := resolved[key]
		if !ok {
			var err error
			p, err = resolveUsage(ctx, tx, task, agent, run, api.UsageTurn{At: segment.At, Handled: span.Handled}, provenance)
			if err != nil {
				return out, err
			}
			resolved[key] = p
		}
		piece := usageSpanPiece{From: segment.From, To: segment.To, Shares: p.Shares, Phase: p.Phase, ReviewRound: p.ReviewRound, Role: p.Role}
		if n := len(out.Pieces); n > 0 {
			last := &out.Pieces[n-1]
			if last.To.Equal(piece.From) && last.Phase == piece.Phase && last.ReviewRound == piece.ReviewRound && last.Role == piece.Role && reflect.DeepEqual(last.Shares, piece.Shares) {
				last.To = piece.To
				continue
			}
		}
		out.Pieces = append(out.Pieces, piece)
	}
	return out, nil
}

// Interval arithmetic over half-open [from,to) stretches.

// spanUnion returns the exact union, sorted and disjoint: overlapping or
// touching stretches merge and a gap is never bridged.
func spanUnion(in [][2]time.Time) [][2]time.Time {
	sorted := append([][2]time.Time(nil), in...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i][0].Before(sorted[j][0]) })
	out := [][2]time.Time{}
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

// spanSubtract removes cut from in. Both are sorted and disjoint.
func spanSubtract(in, cut [][2]time.Time) [][2]time.Time {
	out := [][2]time.Time{}
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

// spanClip keeps the parts of in inside [from,to).
func spanClip(in [][2]time.Time, from, to time.Time) [][2]time.Time {
	out := [][2]time.Time{}
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
func spanLength(in [][2]time.Time) (total time.Duration) {
	for _, x := range in {
		total += x[1].Sub(x[0])
	}
	return total
}
