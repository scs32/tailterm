package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// All data here is synthetic. Times are on 2026-01-01.
func timeAt(hms string) time.Time {
	at, err := time.Parse(time.RFC3339, "2026-01-01T"+hms+"Z")
	if err != nil {
		panic(err)
	}
	return at
}

type span = [2]time.Time

func between(from, to string) span { return span{timeAt(from), timeAt(to)} }

// testSpan is a whole turn in one chunk with a single request at its start.
func testSpan(turn, from, to string) api.UsageSpan {
	start, end := timeAt(from), timeAt(to)
	return api.UsageSpan{ID: turn + "#0", Turn: turn, Last: true, Start: start, End: end, Segments: []api.UsageSpanSegment{{From: start, To: end, At: start}}, SourceDigest: strings.Repeat("c", 64)}
}
func spanBatch(a api.Agent, key string, spans ...api.UsageSpan) api.UsageBatch {
	b := usageBatch(a, key)
	b.Spans = spans
	return b
}

func TestUsageTimeMigrationAndImmutability(t *testing.T) {
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	path := filepath.Join(t.TempDir(), "base.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Synthetic migration"}, by)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "shared", AgentID: api.NewID("agt"), Runtime: "codex", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "synthetic"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "before-migration", syntheticUsageTurn("kept"))); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// The database as tasks-hub 16c3648 created it: every usage table of the
	// token ledger and no usage_spans.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`DROP INDEX usage_spans_time; DROP TABLE usage_spans`); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err = raw.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name LIKE 'usage_spans%'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatal(tables, err)
	}
	raw.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var spans, indexes int
	if err = s.db.QueryRow(`SELECT count(*) FROM usage_spans`).Scan(&spans); err != nil || spans != 0 {
		t.Fatal("usage_spans not added", spans, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='usage_spans_time'`).Scan(&indexes); err != nil || indexes != 1 {
		t.Fatal("index not added", indexes, err)
	}
	report, err := s.Usage(ctx, task.ID, api.UsageQuery{})
	if err != nil || report.Summary.Requests != 1 || report.Summary.Tokens["input"] != "21" {
		t.Fatalf("existing usage rows not kept: %+v %v", report.Summary, err)
	}

	chunk := testSpan("turn-one", "10:00:00", "10:05:00")
	chunk.Tool = []span{between("10:01:00", "10:02:00")}
	batch := spanBatch(a, "spans-one", chunk)
	receipt, err := s.ReportUsage(ctx, task.ID, a.ID, batch)
	if err != nil || receipt.Spans != 1 || receipt.Turns != 0 {
		t.Fatalf("receipt %+v %v", receipt, err)
	}
	count := func() (n int) {
		t.Helper()
		if err := s.db.QueryRow(`SELECT count(*) FROM usage_spans`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// The same batch replays its receipt; the same chunk in another batch is a no-op.
	if replay, err := s.ReportUsage(ctx, task.ID, a.ID, batch); err != nil || replay != receipt {
		t.Fatalf("replay %+v %v", replay, err)
	}
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, spanBatch(a, "spans-again", chunk)); err != nil || count() != 1 {
		t.Fatalf("identical chunk: rows=%d err=%v", count(), err)
	}
	// A chunk never changes: another payload under the same id conflicts.
	changed := chunk
	changed.Tool = []span{between("10:01:00", "10:03:00")}
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, spanBatch(a, "spans-changed", changed)); !errors.Is(err, api.ErrConflict) {
		t.Fatal("changed chunk accepted", err)
	}
	var payload string
	if err = s.db.QueryRow(`SELECT payload FROM usage_spans`).Scan(&payload); err != nil || !strings.Contains(payload, "10:02:00") || count() != 1 {
		t.Fatal("stored chunk was rewritten", payload, err)
	}

	invalid := map[string]func(*api.UsageSpan){
		"segments do not cover the chunk": func(x *api.UsageSpan) { x.Segments[0].To = timeAt("10:04:00") },
		"overlapping tool and wait":       func(x *api.UsageSpan) { x.Wait = []span{between("10:01:30", "10:02:30")} },
		"interval outside the chunk":      func(x *api.UsageSpan) { x.Tool = []span{between("09:59:00", "10:01:00")} },
		"unavailable chunk with a split":  func(x *api.UsageSpan) { x.Unavailable = true },
		"id does not name its chunk":      func(x *api.UsageSpan) { x.ID = "turn-one#3" },
		"no source digest":                func(x *api.UsageSpan) { x.SourceDigest = "" },
		"inbox read as handled evidence": func(x *api.UsageSpan) {
			x.Handled = []api.UsageEvidence{{TaskID: task.ID, Seq: 1, Operation: "inbox", At: x.Start}}
		},
	}
	for name, mutate := range invalid {
		bad := testSpan("turn-bad", "10:00:00", "10:05:00")
		bad.Tool = []span{between("10:01:00", "10:02:00")}
		bad.Segments = append([]api.UsageSpanSegment(nil), bad.Segments...)
		mutate(&bad)
		if _, err = s.ReportUsage(ctx, task.ID, a.ID, spanBatch(a, "bad-"+strings.ReplaceAll(name, " ", "-"), bad)); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if count() != 1 {
		t.Fatal("invalid chunk stored", count())
	}
}
