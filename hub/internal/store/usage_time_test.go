package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"reflect"
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

// timeFixture is one synthetic project with two items and seeded Board rows.
type timeFixture struct {
	t     *testing.T
	s     *Store
	path  string
	task  api.Task
	items []api.WorkItem
	by    api.Caller
}

func newTimeFixture(t *testing.T) *timeFixture {
	t.Helper()
	f := &timeFixture{t: t, path: filepath.Join(t.TempDir(), "time.sqlite"), by: api.Caller{Node: "fixture", User: "owner"}}
	f.open()
	ctx := context.Background()
	var err error
	if f.task, err = f.s.CreateTask(ctx, api.CreateTaskRequest{Name: "Synthetic time accounting", Swarm: true}, f.by); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"Item X", "Item Y"} {
		item, err := f.s.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: title, RequestID: "create-" + strings.ToLower(title[5:])}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		f.items = append(f.items, item)
	}
	return f
}
func (f *timeFixture) open() {
	f.t.Helper()
	s, err := Open(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.s = s
	f.t.Cleanup(func() { s.Close() })
}

// team adds one agent per role. The handler has the authoritative agent
// role; the others get theirs from a frozen team slot, as a Planned team does.
func (f *timeFixture) team(roles ...string) map[string]api.Agent {
	f.t.Helper()
	out := map[string]api.Agent{}
	var members []any
	for _, role := range roles {
		req := api.AddAgentRequest{Name: role, AgentID: api.NewID("agt"), Runtime: "claude", Host: "fixture", Session: "synthetic-" + role}
		if role == "handler" {
			req.Role = api.AgentRoleDatabaseHandler
		}
		a, err := f.s.AddAgent(context.Background(), f.task.ID, req, f.by)
		if err != nil {
			f.t.Fatal(err)
		}
		out[role] = a
		if role != "handler" {
			members = append(members, map[string]any{"fields": map[string]any{"agentId": a.ID, "role": role}, "runId": a.RunID})
		}
	}
	if len(members) > 0 {
		plan, _ := json.Marshal(map[string]any{"members": members})
		if _, err := f.s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,pause_generation,launch_json,created_at,updated_at) VALUES(?,?,?,1,1,'planned',1,'running',1,'fixture','/tmp',0,?,?,?)`, api.NewID("tqe"), f.task.ID, f.items[0].ID, string(plan), ts(f.s.now()), ts(f.s.now())); err != nil {
			f.t.Fatal(err)
		}
	}
	return out
}

// message seeds one Board message created at the given time.
func (f *timeFixture) message(from, to string, at string, env api.Envelope, item string) int64 {
	f.t.Helper()
	if item != "" {
		if env.Refs == nil {
			env.Refs = map[string]string{}
		}
		env.Refs["item"] = item
	}
	raw, _ := json.Marshal(env)
	user := "owner"
	if from != "" {
		user = "agent"
	}
	r, err := f.s.db.Exec(`INSERT INTO messages(task_id,from_agent,from_node,from_user,to_agent,text,created_at,envelope) VALUES(?,?,'fixture',?,?,'synthetic',?,?)`, f.task.ID, from, user, to, ts(timeAt(at)), string(raw))
	if err != nil {
		f.t.Fatal(err)
	}
	seq, _ := r.LastInsertId()
	return seq
}

// obligation seeds what the message's recipient owed, open from created to closed.
func (f *timeFixture) obligation(seq int64, agent, subject, kind, created, closed string) {
	f.t.Helper()
	state := api.ObligationClosed
	at := ts(timeAt(created))
	if _, err := f.s.db.Exec(`INSERT INTO obligations (id,task_id,message_seq,agent_id,subject,source_kind,needs,state,created_at,ack_due_at,due_at,changed_at,closed_at,recipient_kind) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, api.NewID("obl"), f.task.ID, seq, agent, subject, api.EnvelopeKindRequest, api.ObligationNeedsAnswer, state, at, at, at, at, ts(timeAt(closed)), kind); err != nil {
		f.t.Fatal(err)
	}
}
func (f *timeFixture) link(seq int64, item string) {
	f.t.Helper()
	if _, err := f.s.db.Exec(`INSERT INTO message_work_item_links(message_seq,message_task_id,item_task_id,item_id,item_revision,relationship,created_at) VALUES(?,?,?,?,1,'primary',?)`, seq, f.task.ID, f.task.ID, item, ts(f.s.now())); err != nil {
		f.t.Fatal(err)
	}
}
func (f *timeFixture) handled(at string, seqs ...int64) []api.UsageEvidence {
	out := []api.UsageEvidence{}
	for _, seq := range seqs {
		out = append(out, api.UsageEvidence{TaskID: f.task.ID, Seq: seq, Operation: "ack", At: timeAt(at)})
	}
	return out
}
func (f *timeFixture) report(a api.Agent, spans ...api.UsageSpan) {
	f.t.Helper()
	for i := 0; i < len(spans); i += 32 {
		end := min(i+32, len(spans))
		if _, err := f.s.ReportUsage(context.Background(), f.task.ID, a.ID, spanBatch(a, "spans-"+api.NewID("req"), spans[i:end]...)); err != nil {
			f.t.Fatal(err)
		}
	}
}
func (f *timeFixture) time(item string) *api.UsageTime {
	f.t.Helper()
	report, err := f.s.Usage(context.Background(), f.task.ID, api.UsageQuery{Item: item})
	if err != nil || len(report.Items) != 1 {
		f.t.Fatal(report, err)
	}
	if report.Items[0].Time == nil {
		f.t.Fatal("time not measured")
	}
	return report.Items[0].Time
}

// cutSpan splits a whole-turn chunk at the given instants, as the relay's
// chunking does: intervals and segments crossing a cut are split there and
// every segment piece keeps the request it belongs to.
func cutSpan(whole api.UsageSpan, cuts ...string) []api.UsageSpan {
	edges := []time.Time{whole.Start}
	for _, cut := range cuts {
		edges = append(edges, timeAt(cut))
	}
	edges = append(edges, whole.End)
	var out []api.UsageSpan
	for i := 1; i < len(edges); i++ {
		from, to := edges[i-1], edges[i]
		chunk := whole
		chunk.Chunk, chunk.ID, chunk.Last = i-1, whole.Turn+"#"+string(rune('0'+i-1)), i == len(edges)-1
		chunk.Start, chunk.End = from, to
		chunk.Tool, chunk.Wait, chunk.Mixed = spanClip(whole.Tool, from, to), spanClip(whole.Wait, from, to), spanClip(whole.Mixed, from, to)
		chunk.Segments = nil
		for _, piece := range whole.Segments {
			if piece.From.Before(from) {
				piece.From = from
			}
			if piece.To.After(to) {
				piece.To = to
			}
			if piece.To.After(piece.From) {
				chunk.Segments = append(chunk.Segments, piece)
			}
		}
		out = append(out, chunk)
	}
	return out
}

const minute = int64(60000)

func msOf(t *testing.T, value string) int64 {
	t.Helper()
	r, ok := new(big.Rat).SetString(value)
	if !ok || !r.IsInt() {
		t.Fatalf("not whole milliseconds: %q", value)
	}
	return r.Num().Int64()
}

type wantSplit struct{ model, tool, waiting int64 }

func checkSplits(t *testing.T, name string, rows []api.UsageTimeSplit, want map[string]wantSplit) (total int64) {
	t.Helper()
	if len(rows) != len(want) {
		t.Fatalf("%s rows: %+v", name, rows)
	}
	for _, row := range rows {
		w, ok := want[row.Key]
		got := wantSplit{msOf(t, row.ModelMs), msOf(t, row.ToolMs), msOf(t, row.WaitingMs)}
		if !ok || got != w {
			t.Errorf("%s %q: got %+v, want %+v", name, row.Key, got, w)
		}
		total += got.model + got.tool + got.waiting
	}
	return total
}

func TestUsageTimeItemPhaseRoleSplit(t *testing.T) {
	f := newTimeFixture(t)
	team := f.team("planner", "builder", "reviewer", "handler")
	x, y := f.items[0].ID, f.items[1].ID
	plan := f.message("", team["planner"].ID, "09:59:00", api.Envelope{Kind: "request", Subject: "Plan the synthetic item", Refs: map[string]string{"phase": "planning"}}, x)
	assign := f.message("", team["builder"].ID, "10:14:00", api.Envelope{Kind: "assign", Subject: "Build the synthetic item"}, x)
	review := f.message("", team["reviewer"].ID, "10:39:00", api.Envelope{Kind: "review", Subject: "Review the synthetic item", Review: &api.ReviewMetadata{Mode: "general"}}, x)
	saveX := f.message("", team["handler"].ID, "10:35:30", api.Envelope{Kind: "request", Subject: "Save the result for X"}, x)
	saveY := f.message("", team["handler"].ID, "10:35:40", api.Envelope{Kind: "request", Subject: "Save the result for Y"}, y)
	state, _ := json.Marshal(api.ReviewConvergence{ItemID: x, Rounds: []api.ReviewRound{{Number: 1, RequestSeq: review}}})
	if _, err := f.s.db.Exec(`INSERT INTO review_convergence VALUES(?,?,?)`, f.task.ID, x, string(state)); err != nil {
		t.Fatal(err)
	}

	planner := testSpan("turn-plan", "10:00:00", "10:10:00")
	planner.Tool, planner.Handled = []span{between("10:02:00", "10:04:00")}, f.handled("10:00:01", plan)
	f.report(team["planner"], planner)
	builder := testSpan("turn-build", "10:15:00", "10:35:00")
	builder.Tool, builder.Wait, builder.Handled = []span{between("10:20:00", "10:30:00")}, []span{between("10:31:00", "10:33:00")}, f.handled("10:15:01", assign)
	f.report(team["builder"], builder)
	reviewer := testSpan("turn-review", "10:40:00", "11:00:00")
	reviewer.Tool, reviewer.Handled = []span{between("10:45:00", "10:50:00")}, f.handled("10:40:01", review)
	f.report(team["reviewer"], reviewer)
	// The shared handler's turn serves X and Y: each gets half of it.
	handler := testSpan("turn-save", "10:36:00", "10:38:00")
	handler.Handled = f.handled("10:36:01", saveX, saveY)
	f.report(team["handler"], handler)

	got := f.time(x)
	wall := 60 * minute
	if msOf(t, got.WallMs) != wall || !got.From.Equal(timeAt("10:00:00")) || !got.To.Equal(timeAt("11:00:00")) || got.UnmeasuredMs != "0" {
		t.Fatalf("window %+v", got)
	}
	// Each agent's model + tool + waiting is the window.
	agents := map[string]wantSplit{
		"planner":  {8 * minute, 2 * minute, 50 * minute},
		"builder":  {8 * minute, 10 * minute, 42 * minute},
		"reviewer": {15 * minute, 5 * minute, 40 * minute},
		"handler":  {1 * minute, 0, 59 * minute},
	}
	if len(got.Agents) != 4 {
		t.Fatalf("agents %+v", got.Agents)
	}
	for _, a := range got.Agents {
		w := agents[a.Name]
		if (wantSplit{msOf(t, a.ModelMs), msOf(t, a.ToolMs), msOf(t, a.WaitingMs)}) != w || w.model+w.tool+w.waiting != wall {
			t.Errorf("agent %s: %+v", a.Name, a)
		}
	}
	if msOf(t, got.ModelMs) != 32*minute || msOf(t, got.ToolMs) != 17*minute || msOf(t, got.WaitingMs) != 191*minute {
		t.Fatalf("item totals %+v", got)
	}
	// The builder's 15 minutes before its first segment, its 2 minute wait
	// inside it and its 25 minutes after it are all phase build, role builder.
	phases := map[string]wantSplit{"intake and planning": agents["planner"], "build": agents["builder"], "review round 1": agents["reviewer"], "handler bookkeeping": agents["handler"]}
	roles := map[string]wantSplit{"planner": agents["planner"], "builder": agents["builder"], "reviewer": agents["reviewer"], api.AgentRoleDatabaseHandler: agents["handler"]}
	phaseRoles := map[string]wantSplit{"intake and planning / planner": agents["planner"], "build / builder": agents["builder"], "review round 1 / reviewer": agents["reviewer"], "handler bookkeeping / " + api.AgentRoleDatabaseHandler: agents["handler"]}
	for name, check := range map[string]int64{
		"phases":     checkSplits(t, "phases", got.Phases, phases),
		"roles":      checkSplits(t, "roles", got.Roles, roles),
		"phaseRoles": checkSplits(t, "phaseRoles", got.PhaseRoles, phaseRoles),
	} {
		if check != 4*wall {
			t.Errorf("%s total %d, want 4 x wall %d", name, check, 4*wall)
		}
	}
	// Item Y sees only the handler's two minutes, half of them its own.
	other := f.time(y)
	if msOf(t, other.WallMs) != 2*minute || len(other.Agents) != 1 {
		t.Fatalf("item Y %+v", other)
	}
	checkSplits(t, "item Y phases", other.Phases, map[string]wantSplit{"handler bookkeeping": {1 * minute, 0, 1 * minute}})
	// The token ledger's own projection of a handler request agrees.
	turn := syntheticUsageTurn("handler-request")
	turn.Runtime, turn.At, turn.Handled = "codex", timeAt("10:36:00"), handler.Handled
	if _, err := f.s.ReportUsage(context.Background(), f.task.ID, team["handler"].ID, usageBatch(team["handler"], "handler-tokens", turn)); err != nil {
		t.Fatal(err)
	}
	report, _ := f.s.Usage(context.Background(), f.task.ID, api.UsageQuery{Item: y})
	if report.Items[0].Phases[0].Key != "handler bookkeeping" || report.Items[0].Summary.AllocatedTurns != "1/2" {
		t.Fatalf("token projection %+v", report.Items[0])
	}
	// A filter clips the window: 10:30 to 10:45 is 15 minutes of each agent.
	clipped, err := f.s.Usage(context.Background(), f.task.ID, api.UsageQuery{Item: x, From: timeAt("10:30:00"), To: timeAt("10:45:00")})
	if err != nil || clipped.Items[0].Time == nil || msOf(t, clipped.Items[0].Time.WallMs) != 15*minute {
		t.Fatalf("clipped %+v %v", clipped.Items[0].Time, err)
	}
	for _, a := range clipped.Items[0].Time.Agents {
		if msOf(t, a.ModelMs)+msOf(t, a.ToolMs)+msOf(t, a.WaitingMs) != 15*minute {
			t.Errorf("clipped agent %+v", a)
		}
	}
}

func TestUsageTimeMidActivationOrderChange(t *testing.T) {
	// One activation with requests at 10:00 and 10:20. A build ASSIGN is
	// handled before the first; a review request is handled at 10:10.
	shapes := map[string]func() api.UsageSpan{
		// Codex: a request's token_count follows its tool output.
		"codex": func() api.UsageSpan {
			s := testSpan("turn-order", "09:59:50", "10:20:30")
			s.Tool = []span{between("09:59:52", "09:59:58"), between("10:05:00", "10:15:00")}
			s.Segments = []api.UsageSpanSegment{{From: timeAt("09:59:50"), To: timeAt("10:00:00"), At: timeAt("10:00:00")}, {From: timeAt("10:00:00"), To: timeAt("10:20:30"), At: timeAt("10:20:00")}}
			return s
		},
		// Claude: a request is marked at its first assistant record.
		"claude": func() api.UsageSpan {
			s := testSpan("turn-order", "09:59:50", "10:20:30")
			s.Tool = []span{between("10:00:00", "10:00:06"), between("10:20:00", "10:20:20")}
			s.Segments = []api.UsageSpanSegment{{From: timeAt("09:59:50"), To: timeAt("10:00:00"), At: timeAt("10:00:00")}, {From: timeAt("10:00:00"), To: timeAt("10:20:00"), At: timeAt("10:20:00")}, {From: timeAt("10:20:00"), To: timeAt("10:20:30"), At: timeAt("10:20:30")}}
			return s
		},
	}
	want := map[string]map[string]wantSplit{
		"codex":  {"build": {4000, 6000, 0}, "review round 1": {630000, 600000, 0}},
		"claude": {"build": {10000, 0, 0}, "review round 1": {1204000, 26000, 0}},
	}
	for runtime, shape := range shapes {
		t.Run(runtime, func(t *testing.T) {
			ingest := func(chunks func(api.UsageSpan) []api.UsageSpan) (*timeFixture, *api.UsageTime, api.Agent) {
				f := newTimeFixture(t)
				builder := f.team("builder")["builder"]
				x := f.items[0].ID
				assign := f.message("", builder.ID, "09:59:00", api.Envelope{Kind: "assign", Subject: "Build the synthetic item"}, x)
				review := f.message("", builder.ID, "10:09:00", api.Envelope{Kind: "review", Subject: "Review a teammate's change", Review: &api.ReviewMetadata{Mode: "general"}}, x)
				state, _ := json.Marshal(api.ReviewConvergence{ItemID: x, Rounds: []api.ReviewRound{{Number: 1, RequestSeq: review}}})
				if _, err := f.s.db.Exec(`INSERT INTO review_convergence VALUES(?,?,?)`, f.task.ID, x, string(state)); err != nil {
					t.Fatal(err)
				}
				handled := []api.UsageEvidence{{TaskID: f.task.ID, Seq: assign, Operation: "ack", At: timeAt("09:59:55")}, {TaskID: f.task.ID, Seq: review, Operation: "ack", At: timeAt("10:10:00")}}
				whole := shape()
				whole.Handled = handled
				f.report(builder, chunks(whole)...)
				// The same requests in the token ledger.
				for i, segment := range whole.Segments {
					turn := syntheticUsageTurn("request-" + string(rune('a'+i)))
					turn.At, turn.Handled, turn.Activation = segment.At, handled, whole.Turn
					if _, err := f.s.ReportUsage(context.Background(), f.task.ID, builder.ID, usageBatch(builder, "tokens-"+turn.ID, turn)); err != nil {
						t.Fatal(err)
					}
				}
				return f, f.time(x), builder
			}
			f, whole, builder := ingest(func(s api.UsageSpan) []api.UsageSpan { return []api.UsageSpan{s} })
			checkSplits(t, "phases", whole.Phases, want[runtime])

			// For each request, the phase of its tokens is the phase of the
			// time in its segment.
			tokenPhase := map[string]string{}
			rows, err := f.s.db.Query(`SELECT projection FROM usage_turns WHERE agent_id=?`, builder.ID)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var raw string
				var p api.UsageProjection
				if rows.Scan(&raw) != nil || json.Unmarshal([]byte(raw), &p) != nil {
					t.Fatal(raw)
				}
				tokenPhase[ts(p.Turn.At)] = usagePhaseKey(p.Phase, p.ReviewRound)
			}
			rows.Close()
			var payload, encoded string
			if err = f.s.db.QueryRow(`SELECT payload,projection FROM usage_spans WHERE agent_id=?`, builder.ID).Scan(&payload, &encoded); err != nil {
				t.Fatal(err)
			}
			var stored api.UsageSpan
			var projection usageSpanProjection
			if json.Unmarshal([]byte(payload), &stored) != nil || json.Unmarshal([]byte(encoded), &projection) != nil {
				t.Fatal(payload, encoded)
			}
			if len(tokenPhase) != len(stored.Segments) || tokenPhase[ts(timeAt("10:00:00"))] != "build" || tokenPhase[ts(timeAt("10:20:00"))] != "review round 1" {
				t.Fatalf("token phases %+v", tokenPhase)
			}
			for _, segment := range stored.Segments {
				timePhase := ""
				for _, piece := range projection.Pieces {
					if !segment.From.Before(piece.From) && !segment.To.After(piece.To) {
						timePhase = usagePhaseKey(piece.Phase, piece.ReviewRound)
					}
				}
				if timePhase == "" || timePhase != tokenPhase[ts(segment.At)] {
					t.Errorf("request at %s: time phase %q, token phase %q", ts(segment.At), timePhase, tokenPhase[ts(segment.At)])
				}
			}

			// Chunk independence: the same turn cut into small chunks, several
			// of which hold no request of their own, gives the same report.
			_, chunked, _ := ingest(func(s api.UsageSpan) []api.UsageSpan {
				return cutSpan(s, "09:59:55", "10:00:03", "10:07:00", "10:12:00", "10:18:00", "10:20:10")
			})
			if !reflect.DeepEqual(whole.Phases, chunked.Phases) || !reflect.DeepEqual(whole.Roles, chunked.Roles) || !reflect.DeepEqual(whole.PhaseRoles, chunked.PhaseRoles) || whole.ModelMs != chunked.ModelMs || whole.ToolMs != chunked.ToolMs || whole.WaitingMs != chunked.WaitingMs || whole.WallMs != chunked.WallMs {
				t.Fatalf("chunked report differs:\nwhole   %+v\nchunked %+v", whole, chunked)
			}
		})
	}
}

func TestUsageTimeTeamTimeline(t *testing.T) {
	f := newTimeFixture(t)
	team := f.team("planner", "builder", "reviewer")
	x, y := f.items[0].ID, f.items[1].ID
	order := map[string]int64{}
	for role, a := range team {
		order[role] = f.message("", a.ID, "09:59:00", api.Envelope{Kind: "assign", Subject: "Work on the synthetic item"}, x)
	}
	one := testSpan("turn-a", "10:00:00", "10:10:00")
	one.Tool, one.Handled = []span{between("10:02:00", "10:08:00")}, f.handled("10:00:01", order["planner"])
	f.report(team["planner"], one)
	two := testSpan("turn-b", "10:05:00", "10:15:00")
	two.Tool, two.Handled = []span{between("10:06:00", "10:14:00")}, f.handled("10:05:01", order["builder"])
	f.report(team["builder"], two)
	three := testSpan("turn-c", "10:20:00", "10:30:00")
	three.Tool, three.Handled = []span{between("10:22:00", "10:30:00")}, f.handled("10:20:01", order["reviewer"])
	f.report(team["reviewer"], three)
	got := f.time(x)
	// Some model working 8 min, only tools running 17 min, nobody active 5 min.
	line := got.Timeline
	if msOf(t, line.ModelMs) != 8*minute || msOf(t, line.ToolsOnlyMs) != 17*minute || msOf(t, line.IdleMs) != 5*minute || line.UnmeasuredMs != "0" {
		t.Fatalf("timeline %+v", line)
	}
	if msOf(t, line.ModelMs)+msOf(t, line.ToolsOnlyMs)+msOf(t, line.IdleMs) != msOf(t, got.WallMs) || msOf(t, got.WallMs) != 30*minute {
		t.Fatalf("timeline does not sum to wall: %+v", got)
	}

	// The 33-interval turn, ingested as two chunks: nothing is invented.
	solo := f.message("", team["builder"].ID, "11:59:00", api.Envelope{Kind: "assign", Subject: "Work on the other synthetic item"}, y)
	many := testSpan("turn-many", "12:00:00", "12:01:10")
	for i := 0; i < 33; i++ {
		from := many.Start.Add(time.Duration(1+2*i) * time.Second)
		many.Tool = append(many.Tool, span{from, from.Add(time.Second)})
	}
	many.Handled = f.handled("12:00:00", solo)
	chunks := cutSpan(many, "12:01:02")
	if len(chunks) != 2 || len(chunks[0].Tool) != 31 || len(chunks[1].Tool) != 2 {
		t.Fatalf("chunks %d", len(chunks))
	}
	f.report(team["builder"], chunks...)
	got = f.time(y)
	if msOf(t, got.Timeline.ToolsOnlyMs) != 33000 || msOf(t, got.Timeline.ModelMs) != 37000 || got.Timeline.IdleMs != "0" || msOf(t, got.ToolMs) != 33000 || msOf(t, got.WallMs) != 70000 {
		t.Fatalf("33 intervals in two chunks: %+v", got)
	}

	// An unavailable chunk appears only as unmeasured time.
	tail := testSpan("turn-many", "12:01:10", "12:21:10")
	tail.Chunk, tail.ID, tail.Unavailable, tail.Handled = 2, "turn-many#2", true, many.Handled
	f.report(team["builder"], tail)
	got = f.time(y)
	if msOf(t, got.UnmeasuredMs) != 20*minute || msOf(t, got.Timeline.UnmeasuredMs) != 20*minute || msOf(t, got.WallMs) != 70000+20*minute {
		t.Fatalf("unavailable chunk: %+v", got)
	}
	if msOf(t, got.Timeline.ToolsOnlyMs) != 33000 || msOf(t, got.Timeline.ModelMs) != 37000 || got.Timeline.IdleMs != "0" || msOf(t, got.ModelMs) != 37000 || msOf(t, got.ToolMs) != 33000 || got.WaitingMs != "0" {
		t.Fatalf("unavailable stretch leaked into the split: %+v", got)
	}
	if a := got.Agents[0]; msOf(t, a.ModelMs)+msOf(t, a.ToolMs)+msOf(t, a.WaitingMs)+msOf(t, a.UnmeasuredMs) != msOf(t, got.WallMs) {
		t.Fatalf("agent identity %+v", a)
	}
	// A mixed interval is unmeasured too, never tool or waiting.
	mixed := testSpan("turn-mixed", "12:30:00", "12:41:00")
	mixed.Mixed, mixed.Handled = []span{between("12:30:05", "12:40:05")}, many.Handled
	f.report(team["builder"], mixed)
	only, err := f.s.Usage(context.Background(), f.task.ID, api.UsageQuery{Item: y, From: timeAt("12:30:00")})
	if err != nil {
		t.Fatal(err)
	}
	if m := only.Items[0].Time; msOf(t, m.UnmeasuredMs) != 10*minute || msOf(t, m.ModelMs) != minute || m.ToolMs != "0" || m.WaitingMs != "0" {
		t.Fatalf("mixed interval: %+v", m)
	}
}

func TestUsageTimeWaitCauses(t *testing.T) {
	f := newTimeFixture(t)
	team := f.team("builder", "reviewer", "handler")
	builder, reviewer, handler := team["builder"], team["reviewer"], team["handler"]
	x := f.items[0].ID
	assign := f.message("", builder.ID, "09:59:00", api.Envelope{Kind: "assign", Subject: "Build the synthetic item"}, x)
	review := f.message("", reviewer.ID, "09:59:30", api.Envelope{Kind: "review", Subject: "Review the synthetic item", Review: &api.ReviewMetadata{Mode: "general"}}, x)

	// What the Board shows. Everything is closed or answered before the read.
	ownerSeq := f.message(builder.ID, "", "10:05:00", api.Envelope{Kind: "request", Subject: "Approve the <matrix> plan"}, x)
	f.obligation(ownerSeq, "", "Approve the <matrix> plan", api.ObligationRecipientOwner, "10:05:00", "10:25:00")
	f.link(ownerSeq, x)
	handlerSeq := f.message(builder.ID, handler.ID, "10:20:00", api.Envelope{Kind: "request", Subject: "Save the build result"}, x)
	f.obligation(handlerSeq, handler.ID, "Save the build result", api.ObligationRecipientAgent, "10:20:00", "10:40:00")
	teammateSeq := f.message(builder.ID, reviewer.ID, "10:38:00", api.Envelope{Kind: "request", Subject: "Check the candidate"}, x)
	f.obligation(teammateSeq, reviewer.ID, "Check the candidate", api.ObligationRecipientAgent, "10:38:00", "10:50:00")
	decisionSeq := f.message(builder.ID, "", "11:04:00", api.Envelope{}, "")
	answerSeq := f.message("", "", "11:20:00", api.Envelope{}, "")
	if _, err := f.s.db.Exec(`INSERT INTO decision_requests(message_seq,task_id,question,options,recommended_option_id,recommendation_reason,created_at) VALUES(?,?,?,?,?,?,?)`, decisionSeq, f.task.ID, "Ship the candidate?", "[]", "yes", "synthetic", ts(timeAt("11:04:00"))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO decision_answers(message_seq,task_id,request_seq,option_id,created_at) VALUES(?,?,?,?,?)`, answerSeq, f.task.ID, decisionSeq, "yes", ts(timeAt("11:20:00"))); err != nil {
		t.Fatal(err)
	}
	f.link(decisionSeq, x)

	handled := f.handled("10:00:01", assign)
	first := testSpan("turn-1", "10:00:00", "10:05:00")
	second := testSpan("turn-2", "10:30:00", "10:35:00")
	// A turn spanning an owner wait: the inbox wait is inside the turn.
	third := testSpan("turn-3", "11:00:00", "11:30:00")
	third.Wait = []span{between("11:05:00", "11:25:00")}
	first.Handled, second.Handled, third.Handled = handled, handled, handled
	f.report(builder, first, second, third)
	glance := testSpan("turn-r", "10:00:00", "10:02:00")
	glance.Handled = f.handled("10:00:01", review)
	f.report(reviewer, glance)

	check := func() {
		t.Helper()
		got := f.time(x)
		if msOf(t, got.WallMs) != 90*minute {
			t.Fatalf("window %+v", got)
		}
		// Builder, 70 minutes waiting: owner 10:05-10:25 and 11:05-11:20;
		// handler 10:25-10:30 and 10:35-10:40; teammate 10:40-10:50; unknown
		// 10:50-11:00 and 11:20-11:25. Reviewer, 88 minutes: owner while the
		// item's owner request and decision were open, unknown otherwise.
		causes := got.Causes
		if msOf(t, causes.Owner) != (35+36)*minute || msOf(t, causes.Handler) != 10*minute || msOf(t, causes.Teammate) != 10*minute || msOf(t, causes.Unknown) != (15+52)*minute {
			t.Fatalf("causes %+v", causes)
		}
		if msOf(t, causes.Owner)+msOf(t, causes.Handler)+msOf(t, causes.Teammate)+msOf(t, causes.Unknown) != msOf(t, got.WaitingMs) || msOf(t, got.WaitingMs) != 158*minute {
			t.Fatalf("causes do not sum to waiting: %+v", got)
		}
		want := []api.UsageTimeWait{
			{Cause: "unknown", AwaitedBy: "builder, reviewer", Ms: "4020000"},
			{Cause: "owner", MessageSeq: ownerSeq, Subject: "Approve the <matrix> plan", AwaitedBy: "builder, reviewer", Ms: "2400000"},
			{Cause: "owner", MessageSeq: decisionSeq, Subject: "Ship the candidate?", AwaitedBy: "builder, reviewer", Ms: "1860000"},
			{Cause: "handler", MessageSeq: handlerSeq, Subject: "Save the build result", AwaitedBy: "builder", Ms: "600000"},
			{Cause: "teammate", MessageSeq: teammateSeq, Subject: "Check the candidate", AwaitedBy: "builder", Ms: "600000"},
		}
		if !reflect.DeepEqual(got.Waits, want) {
			t.Fatalf("waits:\n got %+v\nwant %+v", got.Waits, want)
		}
	}
	check()
	// The same after the hub closes and reopens the same database file.
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	check()

	// Report cost: obligations and decisions are loaded once per report.
	many := []api.UsageSpan{}
	base := timeAt("13:00:00")
	for i := 0; i < 5000; i++ {
		start := base.Add(time.Duration(i) * 2 * time.Second)
		s := api.UsageSpan{ID: fmt.Sprintf("bulk-%d#0", i), Turn: fmt.Sprintf("bulk-%d", i), Last: true, Start: start, End: start.Add(time.Second), Segments: []api.UsageSpanSegment{{From: start, To: start.Add(time.Second), At: start}}, SourceDigest: strings.Repeat("c", 64), Handled: handled}
		many = append(many, s)
	}
	f.report(builder, many...)
	started := time.Now()
	report, err := f.s.Usage(context.Background(), f.task.ID, api.UsageQuery{Item: x})
	if err != nil || report.Items[0].Time == nil {
		t.Fatal(err)
	}
	t.Logf("usage report over %d chunks took %v", 5000+4, time.Since(started))

	// The same report with 8,000 obligations another agent wrote to the
	// handler: they cannot explain this item's waits and are never walked.
	before := report.Items[0].Time.Causes
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8000; i++ {
		at := ts(base.Add(time.Duration(i) * time.Second))
		r, err := tx.Exec(`INSERT INTO messages(task_id,from_agent,from_node,from_user,to_agent,text,created_at,envelope) VALUES(?,?,'fixture','agent',?,'synthetic',?,'{}')`, f.task.ID, reviewer.ID, handler.ID, at)
		if err != nil {
			t.Fatal(err)
		}
		seq, _ := r.LastInsertId()
		if _, err = tx.Exec(`INSERT INTO obligations (id,task_id,message_seq,agent_id,subject,source_kind,needs,state,created_at,ack_due_at,due_at,changed_at,closed_at,recipient_kind) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, api.NewID("obl"), f.task.ID, seq, handler.ID, "Synthetic bulk request", api.EnvelopeKindRequest, api.ObligationNeedsAnswer, api.ObligationClosed, at, at, at, at, ts(base.Add(time.Duration(i+30)*time.Second)), api.ObligationRecipientAgent); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	report, err = f.s.Usage(context.Background(), f.task.ID, api.UsageQuery{Item: x})
	if err != nil || report.Items[0].Time == nil {
		t.Fatal(err)
	}
	t.Logf("usage report over %d chunks with 8,000 more obligations took %v", 5000+4, time.Since(started))
	// The reviewer wrote them, so only its own waits may change cause: the
	// builder's handler and teammate waits and the owner waits are as before.
	after := report.Items[0].Time.Causes
	if after.Owner != before.Owner || msOf(t, after.Handler)+msOf(t, after.Teammate)+msOf(t, after.Unknown) != msOf(t, before.Handler)+msOf(t, before.Teammate)+msOf(t, before.Unknown) || msOf(t, after.Handler) < msOf(t, before.Handler) {
		t.Fatalf("causes changed: before %+v after %+v", before, after)
	}

	// The span query reads only this project's rows, through the index.
	rows, err := f.s.db.Query(`EXPLAIN QUERY PLAN SELECT agent_id,run_id,payload,projection FROM usage_spans WHERE task_id=? ORDER BY start_at`, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan := ""
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	rows.Close()
	if !strings.Contains(plan, "usage_spans_time") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("span query does not use the index:\n%s", plan)
	}
	// Another project's spans never enter this project's report.
	otherTask, err := f.s.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Another synthetic project"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := f.s.AddAgent(context.Background(), otherTask.ID, api.AddAgentRequest{Name: "stranger", AgentID: api.NewID("agt"), Runtime: "claude", Host: "fixture", Session: "synthetic-stranger"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.ReportUsage(context.Background(), otherTask.ID, stranger.ID, spanBatch(stranger, "other-project", testSpan("turn-other", "09:00:00", "09:30:00"))); err != nil {
		t.Fatal(err)
	}
	again, err := f.s.Usage(context.Background(), f.task.ID, api.UsageQuery{})
	if err != nil || again.Overhead.Time != nil {
		t.Fatalf("another project's spans leaked into overhead: %+v %v", again.Overhead.Time, err)
	}
	elsewhere, err := f.s.Usage(context.Background(), otherTask.ID, api.UsageQuery{})
	if err != nil || elsewhere.Overhead.Time == nil || msOf(t, elsewhere.Overhead.Time.WallMs) != 30*minute {
		t.Fatalf("other project's own report: %+v %v", elsewhere.Overhead.Time, err)
	}
}
