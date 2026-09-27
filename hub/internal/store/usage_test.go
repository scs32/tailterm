package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func usageFixture(t *testing.T) (*Store, api.Task, api.Agent, []api.WorkItem, []api.Message) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Synthetic usage only", Swarm: true}, by)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "shared", AgentID: api.NewID("agt"), Runtime: "codex", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "synthetic"}, by)
	if err != nil {
		t.Fatal(err)
	}
	items := []api.WorkItem{}
	messages := []api.Message{}
	for i := 0; i < 2; i++ {
		item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: fmt.Sprintf("Item %d", i), RequestID: fmt.Sprintf("create-%d", i)}, by)
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
		m, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: a.ID, RequestID: fmt.Sprintf("order-%d", i), WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}, Envelope: &api.Envelope{Kind: "request", To: a.ID, Subject: "Handle synthetic evidence", Body: api.EnvelopeBody{Ask: "Record synthetic data"}}}, by)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, m)
	}
	return s, task, a, items, messages
}
func syntheticUsageTurn(id string) api.UsageTurn {
	return api.UsageTurn{ID: id, Revision: 1, Runtime: "codex", Session: "synthetic-session", Model: "synthetic-model", At: time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC), Tokens: map[string]int64{"input": 21, "cached": 80, "cacheWrite": 3, "output": 7, "reasoning": 2}, Raw: map[string]int64{"input_tokens": 101, "cached_input_tokens": 80, "cache_write_tokens": 3, "output_tokens": 9, "reasoning_output_tokens": 2}, SourceDigest: strings.Repeat("a", 64), Activation: "activation-one", Complete: true}
}
func usageBatch(a api.Agent, key string, turns ...api.UsageTurn) api.UsageBatch {
	return api.UsageBatch{Version: 1, RequestID: key, RunID: a.RunID, Session: "synthetic-session", StartedAt: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC), Coverage: "synthetic complete", Turns: turns}
}
func TestUsageSharedConservationOverheadReceiptsAndClosure(t *testing.T) {
	s, task, a, items, messages := usageFixture(t)
	ctx := context.Background()
	turn := syntheticUsageTurn("one")
	for _, m := range messages {
		turn.Handled = append(turn.Handled, api.UsageEvidence{TaskID: task.ID, Seq: m.Seq, Operation: "ack", At: turn.At})
	}
	batch := usageBatch(a, "batch-one", turn)
	receipt, err := s.ReportUsage(ctx, task.ID, a.ID, batch)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ReportUsage(ctx, task.ID, a.ID, batch)
	if err != nil || replay != receipt {
		t.Fatalf("retry %+v %v", replay, err)
	}
	bad := batch
	bad.Coverage = "changed"
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, bad); !errors.Is(err, api.ErrConflict) {
		t.Fatal("changed retry", err)
	}
	overhead := syntheticUsageTurn("two")
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "batch-two", overhead)); err != nil {
		t.Fatal(err)
	}
	report, err := s.Usage(ctx, task.ID, api.UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Tokens["input"] != "42" || report.Summary.Requests != 2 || report.Summary.AllocatedTurns != "2" {
		t.Fatalf("source totals %+v", report.Summary)
	}
	for _, item := range report.Items {
		if item.Summary.Tokens["input"] != "21/2" || item.Summary.AllocatedTurns != "1/2" || item.Summary.Requests != 1 || *item.Summary.AverageContext != "101" || item.Phases[0].Key != "handler bookkeeping" {
			t.Fatalf("half share %+v", item)
		}
	}
	if report.Overhead.Summary.Tokens["input"] != "21" {
		t.Fatal("overhead dropped", report.Overhead)
	}
	itemReport, err := s.Usage(ctx, task.ID, api.UsageQuery{Item: items[0].ID})
	if err != nil || len(itemReport.Items) != 1 || itemReport.Summary.Tokens["input"] != "21/2" {
		t.Fatal(itemReport, err)
	}
	if _, err = s.CloseAgent(ctx, a.ID, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
	}
	after := syntheticUsageTurn("three")
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "after-close", after)); err != nil {
		t.Fatal("enrolled frozen run rejected", err)
	}
	stale := a
	stale.RunID = api.NewID("run")
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(stale, "unknown-run", after)); !errors.Is(err, api.ErrConflict) {
		t.Fatal("unobserved stale run enrolled", err)
	}
	path := s.db
	_ = path
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM usage_turn_revisions`).Scan(&n); err != nil || n != 3 {
		t.Fatal(n, err)
	}
}
func TestUsagePricesExactDatesUnknownAndRevisionCAS(t *testing.T) {
	s, task, a, _, _ := usageFixture(t)
	ctx := context.Background()
	turn := syntheticUsageTurn("priced")
	if _, err := s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "priced-turn", turn)); err != nil {
		t.Fatal(err)
	}
	report, _ := s.Usage(ctx, task.ID, api.UsageQuery{})
	if len(report.Summary.PricedSubtotal) != 0 || report.Summary.CostComplete {
		t.Fatal("empty table implies free", report)
	}
	rows := []api.UsagePrice{{Runtime: "codex", Model: turn.Model, Currency: "USD", EffectiveAt: turn.At.Add(-time.Hour), Rates: map[string]string{"input": "2", "cached": "0.5", "cacheWrite": "3", "output": "4", "reasoning": "4"}}, {Runtime: "codex", Model: turn.Model, Currency: "USD", EffectiveAt: turn.At.Add(time.Hour), Rates: map[string]string{"input": "10"}}}
	req := api.UsagePriceRequest{RequestID: "prices-one", ExpectedRevision: 0, Rows: rows}
	prices, err := s.SetUsagePrices(ctx, task.ID, req)
	if err != nil || prices.Revision != 1 {
		t.Fatal(prices, err)
	}
	if _, err = s.SetUsagePrices(ctx, task.ID, req); err != nil {
		t.Fatal("prices replay", err)
	}
	report, _ = s.Usage(ctx, task.ID, api.UsageQuery{})
	if report.Summary.PricedSubtotal["USD"] != "127/1000000" || !report.Summary.CostComplete {
		t.Fatal("exact cost", report.Summary)
	}
	later := syntheticUsageTurn("later")
	later.At = turn.At.Add(time.Hour)
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "later-price", later)); err != nil {
		t.Fatal(err)
	}
	report, _ = s.Usage(ctx, task.ID, api.UsageQuery{From: later.At})
	if report.Summary.PricedSubtotal["USD"] != "21/100000" || report.Summary.CostComplete {
		t.Fatal("partial boundary", report.Summary)
	}
	report, _ = s.Usage(ctx, task.ID, api.UsageQuery{To: later.At})
	if report.Summary.Requests != 1 {
		t.Fatal("exclusive to", report.Summary)
	}
	req.RequestID = "prices-stale"
	if _, err = s.SetUsagePrices(ctx, task.ID, req); !errors.Is(err, api.ErrConflict) {
		t.Fatal("stale revision", err)
	}
	req.ExpectedRevision = 1
	req.Rows[0].Rates["input"] = "-1"
	if _, err = s.SetUsagePrices(ctx, task.ID, req); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("negative price", err)
	}
	if _, err = s.Usage(ctx, task.ID, api.UsageQuery{From: later.At, To: turn.At}); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("reversed range", err)
	}
}
func TestUsageStreamingFinalizationUnavailableAndNotMeasured(t *testing.T) {
	s, task, a, items, _ := usageFixture(t)
	ctx := context.Background()
	turn := syntheticUsageTurn("stream")
	turn.Complete = false
	delete(turn.Tokens, "cached")
	delete(turn.Tokens, "input")
	delete(turn.Raw, "cached_input_tokens")
	batch := usageBatch(a, "initial", turn)
	if _, err := s.ReportUsage(ctx, task.ID, a.ID, batch); err != nil {
		t.Fatal(err)
	}
	report, _ := s.Usage(ctx, task.ID, api.UsageQuery{})
	if report.Summary.AverageContext != nil || report.Summary.State != "partial" || report.Items[0].Summary.State != "not measured" {
		t.Fatal("unknown was zero", report)
	}
	next := syntheticUsageTurn("stream")
	next.Revision = 2
	if _, err := s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "final", next)); err != nil {
		t.Fatal(err)
	}
	report, _ = s.Usage(ctx, task.ID, api.UsageQuery{})
	if report.Summary.Requests != 1 || report.Summary.Tokens["input"] != "21" {
		t.Fatal("double counted update", report)
	}
	copy := next
	copy.Revision = 3
	copy.Handled = []api.UsageEvidence{{TaskID: task.ID, Seq: 1, Operation: "ack", At: copy.At}}
	if _, err := s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "post-final", copy)); !errors.Is(err, api.ErrConflict) {
		t.Fatal("final projection rewritten", err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT projection FROM usage_turns LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var projection api.UsageProjection
	if json.Unmarshal([]byte(raw), &projection) != nil || len(projection.Shares) != 1 || projection.Shares[0].ItemID != "" {
		t.Fatal(raw)
	}
	unmeasured, _ := s.Usage(ctx, task.ID, api.UsageQuery{Item: items[0].ID})
	if unmeasured.Summary.State != "not measured" {
		t.Fatal(unmeasured)
	}
}
func TestUsageRefsOnlyAndUnreadSwarmNeverAttributed(t *testing.T) {
	s, task, a, items, _ := usageFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	m, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: a.ID, Envelope: &api.Envelope{Kind: "request", To: a.ID, Subject: "Resolve references only", Refs: map[string]string{"item": items[0].ID}, Body: api.EnvelopeBody{Ask: "Handle reference"}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	turn := syntheticUsageTurn("refs")
	turn.Handled = []api.UsageEvidence{{TaskID: task.ID, Seq: m.Seq, Operation: "ack", At: turn.At}}
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "refs-only", turn)); err != nil {
		t.Fatal(err)
	}
	report, _ := s.Usage(ctx, task.ID, api.UsageQuery{})
	var linked api.UsageItemReport
	for _, x := range report.Items {
		if x.ItemID == items[0].ID {
			linked = x
		}
	}
	if linked.Summary.Requests != 1 {
		t.Fatal("refs not resolved", report)
	}
	turn.ID = "read"
	turn.Handled[0].Operation = "inbox"
	if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "read-only", turn)); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("read claimed handling", err)
	}
}

func TestUsageExactPhasesReviewRoundsAndWeightedContext(t *testing.T) {
	s, task, a, items, _ := usageFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agents SET role='' WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	plan, _ := json.Marshal(map[string]any{"members": []any{map[string]any{"fields": map[string]any{"agentId": a.ID, "role": "builder"}, "runId": a.RunID}}})
	if _, err := s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,pause_generation,launch_json,created_at,updated_at) VALUES(?,?,?,1,1,'planned',1,'running',1,'fixture','/tmp',0,?,?,?)`, api.NewID("tqe"), task.ID, items[0].ID, string(plan), ts(s.now()), ts(s.now())); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	specs := []struct {
		kind, phase string
		review      *api.ReviewMetadata
		want        string
		round       int
	}{{"request", "planning", nil, "intake and planning", 0}, {"assign", "", nil, "build", 0}, {"review", "", &api.ReviewMetadata{Mode: "general"}, "review round 1", 1}, {"assign", "", nil, "corrections", 0}, {"review", "", &api.ReviewMetadata{Mode: "general"}, "review round 2", 2}, {"review", "", &api.ReviewMetadata{Mode: "focused"}, "verification", 0}, {"request", "", nil, "hand-offs", 0}}
	messages := []api.Message{}
	for i, spec := range specs {
		e := api.Envelope{Kind: spec.kind, Subject: "Synthetic phase request", Refs: map[string]string{"item": items[0].ID, "phase": spec.phase}, Review: spec.review}
		raw, _ := json.Marshal(e)
		r, err := s.db.Exec(`INSERT INTO messages(task_id,from_agent,from_node,from_user,to_agent,text,created_at,envelope) VALUES(?,'','fixture','owner',?,'synthetic',?,?)`, task.ID, a.ID, ts(at.Add(time.Duration(i)*time.Minute)), string(raw))
		if err != nil {
			t.Fatal(err)
		}
		seq, _ := r.LastInsertId()
		messages = append(messages, api.Message{Seq: seq})
	}
	state := api.ReviewConvergence{ItemID: items[0].ID, Rounds: []api.ReviewRound{{Number: 1, RequestSeq: messages[2].Seq, ResultSeq: messages[2].Seq, Blockers: []api.ReviewFinding{{ID: "blocker", Title: "Synthetic blocker"}}}, {Number: 2, RequestSeq: messages[4].Seq}}}
	raw, _ := json.Marshal(state)
	if _, err := s.db.Exec(`INSERT INTO review_convergence VALUES(?,?,?)`, task.ID, items[0].ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	for i, m := range messages {
		turn := syntheticUsageTurn(fmt.Sprintf("phase-%d", i))
		turn.At = at.Add(time.Duration(i)*time.Minute + time.Second)
		turn.Handled = []api.UsageEvidence{{TaskID: task.ID, Seq: m.Seq, Operation: "ack", At: turn.At}}
		if _, err := s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, fmt.Sprintf("phase-batch-%d", i), turn)); err != nil {
			t.Fatal(err)
		}
	}
	report, err := s.Usage(ctx, task.ID, api.UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var phaseItem api.UsageItemReport
	for _, x := range report.Items {
		if x.ItemID == items[0].ID {
			phaseItem = x
		}
	}
	got := map[string]int{}
	for _, g := range phaseItem.Phases {
		got[g.Key] = g.Summary.Requests
		if *g.Summary.AverageContext != "101" {
			t.Fatal("average group", g)
		}
	}
	for _, spec := range specs {
		if got[spec.want] != 1 {
			t.Fatalf("phase %s: %+v", spec.want, got)
		}
	}
	if len(phaseItem.PhaseRoles) != 7 || phaseItem.Roles[0].Key != "builder" || report.Summary.Tokens["input"] != "147" {
		t.Fatal("phase/role conservation", report)
	}
}
