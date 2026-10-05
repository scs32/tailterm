package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"path/filepath"
	"reflect"
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
	return api.UsageTurn{ID: id, Revision: 1, Runtime: "codex", Session: "synthetic-session", Model: "synthetic-model", At: time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC), Tokens: map[string]int64{"input": 21, "cached": 80, "cacheWrite": 0, "output": 7, "reasoning": 2}, Raw: map[string]int64{"input_tokens": 101, "cached_input_tokens": 80, "cache_write_input_tokens": 0, "output_tokens": 9, "reasoning_output_tokens": 2}, SourceDigest: strings.Repeat("a", 64), Activation: "activation-one", Complete: true}
}
func usageBatch(a api.Agent, key string, turns ...api.UsageTurn) api.UsageBatch {
	return api.UsageBatch{Version: 1, RequestID: key, RunID: a.RunID, Session: "synthetic-session", StartedAt: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC), Coverage: "synthetic complete", Turns: turns}
}

func TestUsageReportTurnsQueryUsesProjectIndex(t *testing.T) {
	s, task, _, _, _ := usageFixture(t)
	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+usageReportTurnsQuery, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		t.Log(detail)
		indexed = indexed || (strings.Contains(detail, "SEARCH usage_turns USING INDEX") && strings.Contains(detail, "(task_id=?)"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("usage report does not use an indexed task lookup")
	}
}

func TestUsageReportReadsOnlyProjectTurns(t *testing.T) {
	s, task, a, _, messages := usageFixture(t)
	ctx := context.Background()
	shared := syntheticUsageTurn("shared")
	for _, message := range messages {
		shared.Handled = append(shared.Handled, api.UsageEvidence{TaskID: task.ID, Seq: message.Seq, Operation: "ack", At: shared.At})
	}
	if _, err := s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "project-turns", shared, syntheticUsageTurn("overhead"))); err != nil {
		t.Fatal(err)
	}
	want, err := s.Usage(ctx, task.ID, api.UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if want.Summary.Requests != 2 || want.Summary.Tokens["input"] != "42" || want.Overhead.Summary.Tokens["input"] != "21" {
		t.Fatalf("project totals: %+v", want)
	}
	for _, item := range want.Items {
		if item.Summary.Requests != 1 || item.Summary.Tokens["input"] != "21/2" {
			t.Fatalf("shared item totals: %+v", item)
		}
	}
	if len(want.Coverage) != 1 || want.Coverage[0].AgentID != a.ID || want.Coverage[0].RunID != a.RunID || want.Coverage[0].State != "synthetic complete" {
		t.Fatalf("project coverage: %+v", want.Coverage)
	}
	by := api.Caller{Node: "fixture", User: "owner"}
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Unrelated synthetic project"}, by)
	if err != nil {
		t.Fatal(err)
	}
	otherAgent, err := s.AddAgent(ctx, other.ID, api.AddAgentRequest{Name: "other", AgentID: api.NewID("agt"), Runtime: "codex", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "other-synthetic"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReportUsage(ctx, other.ID, otherAgent.ID, usageBatch(otherAgent, "other-project-turn", syntheticUsageTurn("other"))); err != nil {
		t.Fatal(err)
	}
	otherReport, err := s.Usage(ctx, other.ID, api.UsageQuery{})
	if err != nil || otherReport.Summary.Requests != 1 || otherReport.Summary.Tokens["input"] != "21" || len(otherReport.Coverage) != 1 || otherReport.Coverage[0].AgentID != otherAgent.ID {
		t.Fatalf("other project report: %+v, %v", otherReport, err)
	}
	check := func() {
		t.Helper()
		got, err := s.Usage(ctx, task.ID, api.UsageQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("unrelated project changed report: got %+v, want %+v", got, want)
		}
	}
	check()
	// Poison only the unrelated project's isolated ledger row. Success proves
	// the report excludes that row before decoding, rather than filtering shares.
	if _, err := s.db.ExecContext(ctx, `UPDATE usage_turns SET projection='invalid JSON' WHERE task_id=?`, other.ID); err != nil {
		t.Fatal(err)
	}
	check()
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
	// A second handler keeps the handler floor satisfied while this one closes.
	if _, err = s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "shared-spare", AgentID: api.NewID("agt"), Runtime: "codex", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "synthetic-spare"}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		t.Fatal(err)
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
	if report.Summary.PricedSubtotal["USD"] != "59/500000" || !report.Summary.CostComplete {
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

func TestUsagePlannedDescriptiveRolesSelectPhases(t *testing.T) {
	for _, spec := range []struct{ role, phase, slug string }{{"Planning and acceptance criteria", "intake and planning", "planner"}, {"Independent matrix verification", "verification", "verifier"}} {
		t.Run(spec.slug, func(t *testing.T) {
			s, task, a, items, messages := usageFixture(t)
			ctx := context.Background()
			if _, err := s.db.Exec(`UPDATE agents SET role='' WHERE id=?`, a.ID); err != nil {
				t.Fatal(err)
			}
			plan, _ := json.Marshal(map[string]any{"members": []any{map[string]any{"fields": map[string]any{"agentId": a.ID, "name": spec.slug + "-fixture", "role": spec.role}, "runId": a.RunID}}})
			if _, err := s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,pause_generation,launch_json,created_at,updated_at) VALUES(?,?,?,1,1,'planned',1,'running',1,'fixture','/tmp',0,?,?,?)`, api.NewID("tqe"), task.ID, items[0].ID, string(plan), ts(s.now()), ts(s.now())); err != nil {
				t.Fatal(err)
			}
			turn := syntheticUsageTurn("planned-" + spec.slug)
			turn.At = messages[0].CreatedAt.Add(time.Second)
			turn.Handled = []api.UsageEvidence{{TaskID: task.ID, Seq: messages[0].Seq, Operation: "ack", At: turn.At}}
			if _, err := s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "planned-"+spec.slug, turn)); err != nil {
				t.Fatal(err)
			}
			report, err := s.Usage(ctx, task.ID, api.UsageQuery{Item: items[0].ID})
			if err != nil || len(report.Items) != 1 || report.Items[0].Phases[0].Key != spec.phase || report.Items[0].Roles[0].Key != spec.slug {
				t.Fatal(report, err)
			}
		})
	}
}
func TestUsageBoundedAncestorsAndBadEntryKeepValidItems(t *testing.T) {
	s, task, a, items, _ := usageFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	var head api.Message
	for i := 0; i < 11; i++ {
		req := api.PostMessageRequest{To: a.ID, ReplyTo: head.Seq, RequestID: fmt.Sprintf("ancestor-%d", i), Text: "Synthetic ancestor"}
		if i == 10 {
			req.WorkItems = []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: items[0].ID, ItemRevision: items[0].Revision, Relationship: "primary"}}
		}
		var err error
		head, err = s.PostMessage(ctx, task.ID, req, by)
		if err != nil {
			t.Fatal(err)
		}
	}
	turn := syntheticUsageTurn("bounded")
	turn.Handled = []api.UsageEvidence{{TaskID: task.ID, Seq: head.Seq, Operation: "ack", At: turn.At}, {TaskID: task.ID, Seq: 999999, Operation: "ack", At: turn.At}}
	if _, err := s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "bounded", turn)); err != nil {
		t.Fatal(err)
	}
	report, err := s.Usage(ctx, task.ID, api.UsageQuery{Item: items[0].ID})
	if err != nil || report.Summary.Requests != 1 || report.Summary.Tokens["input"] != "21" {
		t.Fatal("valid head lost to overhead", report, err)
	}
}
func TestUsageReassignedObligationAndBindingFallbackSurviveBadAck(t *testing.T) {
	s, task, old, items, messages := usageFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	target, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "correction-builder", Host: "fixture", Session: "fixture", Runtime: "codex"}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,?,0,'synthetic-binding','{}',?)`, target.ID, target.RunID, task.ID, items[0].ID, task.ID, messages[0].Seq, ts(s.now())); err != nil {
		t.Fatal(err)
	}
	var obligation string
	if err = s.db.QueryRow(`SELECT id FROM obligations WHERE message_seq=? AND agent_id=?`, messages[0].Seq, old.ID).Scan(&obligation); err != nil {
		t.Fatal(err)
	}
	moved, err := s.ReassignObligation(ctx, task.ID, obligation, api.ObligationReassignRequest{ToAgentID: target.ID, Reason: "Synthetic handoff"}, by)
	if err != nil {
		t.Fatal(err)
	}
	for i, seq := range []int64{moved.Seq, messages[0].Seq} {
		turn := syntheticUsageTurn(fmt.Sprintf("reassigned-%d", i))
		turn.Handled = []api.UsageEvidence{{TaskID: task.ID, Seq: seq, Operation: "ack", At: turn.At}}
		if _, err = s.ReportUsage(ctx, task.ID, target.ID, usageBatch(target, fmt.Sprintf("reassigned-%d", i), turn)); err != nil {
			t.Fatal(err)
		}
	}
	report, err := s.Usage(ctx, task.ID, api.UsageQuery{Item: items[0].ID})
	if err != nil || report.Summary.Requests != 2 || report.Summary.Tokens["input"] != "42" {
		t.Fatal("reassignment or exact binding discarded", report, err)
	}
}

func TestUsageClaudeInclusiveOutputReportAndPricing(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(fmt.Sprint(known), func(t *testing.T) {
			s, task, _, items, _ := usageFixture(t)
			ctx := context.Background()
			by := api.Caller{Node: "fixture", User: "owner"}
			a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "focused-claude", Host: "fixture", Session: "synthetic", Runtime: "claude", Role: api.AgentRoleDatabaseHandler}, by)
			if err != nil {
				t.Fatal(err)
			}
			m, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: a.ID, RequestID: "focused-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: items[0].ID, ItemRevision: 1, Relationship: "primary"}}, Envelope: &api.Envelope{Kind: "request", To: a.ID, Subject: "Handle focused synthetic evidence", Body: api.EnvelopeBody{Ask: "Record fixture"}}}, by)
			if err != nil {
				t.Fatal(err)
			}
			turn := syntheticUsageTurn("focused-output")
			turn.Runtime = "claude"
			turn.Model = "focused-model"
			turn.Raw = map[string]int64{"input_tokens": 2, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 4, "output_tokens": 7}
			if known {
				turn.Raw["output_tokens_details.thinking_tokens"] = 2
			}
			turn.Tokens, turn.Gap = api.NormalizeUsageTokens(turn.Runtime, turn.Raw)
			turn.Handled = []api.UsageEvidence{{TaskID: task.ID, Seq: m.Seq, Operation: "ack", At: turn.At}}
			if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "focused-output", turn)); err != nil {
				t.Fatal(err)
			}
			if _, err = s.SetUsagePrices(ctx, task.ID, api.UsagePriceRequest{RequestID: "focused-prices", Rows: []api.UsagePrice{{Runtime: "claude", Model: turn.Model, Currency: "USD", EffectiveAt: turn.At, Rates: map[string]string{"input": "1", "cached": "1", "cacheWrite": "1", "output": "2", "reasoning": "3"}}}}); err != nil {
				t.Fatal(err)
			}
			check := func(output, cost, state string, complete bool) {
				t.Helper()
				report, err := s.Usage(ctx, task.ID, api.UsageQuery{Item: items[0].ID})
				if err != nil {
					t.Fatal(err)
				}
				item := report.Items[0]
				summaries := []api.UsageSummary{report.Summary, item.Summary, item.Phases[0].Summary, item.Roles[0].Summary, item.Models[0].Summary, item.PhaseRoles[0].Summary}
				for _, summary := range summaries {
					if summary.Requests != 1 || summary.Tokens["output"] != output || summary.PricedSubtotal["USD"] != cost || summary.State != state || summary.CostComplete != complete {
						t.Fatal(summary)
					}
				}
				if !complete {
					if _, invented := report.Summary.Tokens["reasoning"]; invented || report.Summary.MeasuredRequests["reasoning"] != 0 {
						t.Fatal("missing reasoning fabricated", report.Summary)
					}
				} else if report.Summary.Tokens["reasoning"] != "2" {
					t.Fatal(report)
				}
			}
			if known {
				check("5", "1/40000", "measured", true)
			} else {
				check("7", "23/1000000", "partial", false)
				// A later known subset revises the same complete request: retain inclusive
				// provider output in Raw, replace its chargeable split, never add it twice.
				turn.Revision = 2
				turn.Raw["output_tokens_details.thinking_tokens"] = 2
				turn.Tokens, turn.Gap = api.NormalizeUsageTokens(turn.Runtime, turn.Raw)
				turn.SourceDigest = strings.Repeat("b", 64)
				if _, err = s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, "focused-output-late", turn)); err != nil {
					t.Fatal(err)
				}
				check("5", "1/40000", "measured", true)
			}
		})
	}
}

// usagePhaseFixture is a synthetic Planned team on one item: real typed
// messages (so real obligations), a controllable clock and bound members.
type usagePhaseFixture struct {
	t      *testing.T
	s      *Store
	ctx    context.Context
	by     api.Caller
	task   api.Task
	item   api.WorkItem
	order  api.Message
	base   time.Time
	clock  time.Time
	agents map[string]api.Agent
	serial int
}

var usagePhaseTeam = []struct{ name, role string }{{"lead", "lead"}, {"planner", "Planning and acceptance criteria"}, {"builder", "Implementation"}, {"reviewer", "Independent code review"}, {"verifier", "Independent matrix verification"}}

func newUsagePhaseFixture(t *testing.T) *usagePhaseFixture {
	t.Helper()
	f := &usagePhaseFixture{t: t, ctx: context.Background(), by: api.Caller{Node: "fixture", User: "owner"}, base: time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC), agents: map[string]api.Agent{}}
	f.clock = f.base.Add(-time.Hour)
	var err error
	if f.s, err = Open(filepath.Join(t.TempDir(), "usage-phase.sqlite")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.s.Close() })
	f.s.SetClockForTest(func() time.Time { return f.clock })
	if f.task, err = f.s.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Synthetic usage phases", Swarm: true}, f.by); err != nil {
		t.Fatal(err)
	}
	if f.item, err = f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Synthetic phase item", RequestID: "phase-item"}, f.by); err != nil {
		t.Fatal(err)
	}
	if f.order, err = f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "Synthetic work order", RequestID: "phase-order", WorkItems: f.links()}, f.by); err != nil {
		t.Fatal(err)
	}
	members := []any{}
	for _, m := range usagePhaseTeam {
		a, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: m.name, Host: "fixture", Session: m.name, Runtime: "codex"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		f.agents[m.name] = a
		members = append(members, map[string]any{"fields": map[string]any{"agentId": a.ID, "role": m.role}, "runId": a.RunID})
		if _, err = f.s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,?,0,'synthetic-binding','{}',?)`, a.ID, a.RunID, f.task.ID, f.item.ID, f.task.ID, f.order.Seq, ts(f.clock)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = f.s.db.Exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, f.task.ID, f.item.ID, f.agents["lead"].ID, f.agents["lead"].RunID); err != nil {
		t.Fatal(err)
	}
	plan, _ := json.Marshal(map[string]any{"members": members})
	if _, err = f.s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,pause_generation,launch_json,created_at,updated_at) VALUES(?,?,?,1,1,'planned',1,'running',1,'fixture','/tmp',0,?,?,?)`, api.NewID("tqe"), f.task.ID, f.item.ID, string(plan), ts(f.clock), ts(f.clock)); err != nil {
		t.Fatal(err)
	}
	if f.agents["handler"], err = f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "handler", AgentID: api.NewID("agt"), Runtime: "codex", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "handler"}, f.by); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *usagePhaseFixture) links() []api.MessageWorkItem {
	return []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}
}

// send posts a typed message at base+d, linked to the item.
func (f *usagePhaseFixture) send(d time.Duration, from, to string, replyTo int64, env api.Envelope) api.Message {
	f.t.Helper()
	f.clock = f.base.Add(d)
	f.serial++
	sender, recipient := f.agents[from], f.agents[to]
	env.To = recipient.Name
	m, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: sender.ID, RunID: sender.RunID, To: recipient.ID, ReplyTo: replyTo, Envelope: &env, WorkItems: f.links(), RequestID: fmt.Sprintf("phase-message-%d", f.serial)}, f.by)
	if err != nil {
		f.t.Fatal(env.Kind, from, to, err)
	}
	return m
}

// ack acknowledges an order at base+d, as tt ack does; the order stays open.
func (f *usagePhaseFixture) ack(d time.Duration, agent string, m api.Message) {
	f.t.Helper()
	a := f.agents[agent]
	if _, err := f.s.ObligationAction(f.ctx, f.task.ID, m.Seq, "ack", api.ObligationActionRequest{AgentID: a.ID, RunID: a.RunID}, f.base.Add(d)); err != nil {
		f.t.Fatal(agent, err)
	}
}

var usagePhaseCriteria = map[string]string{"a1": "works"}

func usagePhaseAssign() api.Envelope {
	return api.Envelope{Kind: "assign", Subject: "Implement the synthetic phase fixture", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: usagePhaseCriteria}}
}
func usagePhaseRequest() api.Envelope {
	return api.Envelope{Kind: "request", Subject: "Record the synthetic phase fixture", Body: api.EnvelopeBody{Ask: "Record synthetic data."}}
}
func usagePhaseReview() api.Envelope {
	return api.Envelope{Kind: "review", Subject: "Review the synthetic phase candidate", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "Fixture", Acceptance: usagePhaseCriteria}}
}
func usagePhaseResult(review *api.ReviewMetadata) api.Envelope {
	return api.Envelope{Kind: "result", Subject: "The synthetic phase fixture is recorded", Review: review, Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}}, Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "fixture check -> ok"}}}
}

// handled builds one activation's evidence: "ack" for a message addressed to
// the agent, "post" or "reply" for one it wrote.
func (f *usagePhaseFixture) handled(operation string, m api.Message) api.UsageEvidence {
	return api.UsageEvidence{TaskID: f.task.ID, Seq: m.Seq, Operation: operation, At: m.CreatedAt}
}

// requests reports n metered requests by agent at base+d, in batches.
func (f *usagePhaseFixture) requests(agent string, d time.Duration, n int, handled ...api.UsageEvidence) {
	f.t.Helper()
	a := f.agents[agent]
	for n > 0 {
		turns := []api.UsageTurn{}
		for ; n > 0 && len(turns) < 50; n-- {
			f.serial++
			turn := syntheticUsageTurn(fmt.Sprintf("%s-%d", agent, f.serial))
			turn.At = f.base.Add(d)
			turn.Handled = handled
			turns = append(turns, turn)
		}
		f.serial++
		if _, err := f.s.ReportUsage(f.ctx, f.task.ID, a.ID, usageBatch(a, fmt.Sprintf("%s-batch-%d", agent, f.serial), turns...)); err != nil {
			f.t.Fatal(agent, d, err)
		}
	}
}

// phaseRoles returns requests per "phase / role" over the item and overhead.
func usagePhaseRoles(report api.UsageReport) map[string]int {
	out := map[string]int{}
	for _, row := range append(append([]api.UsageItemReport{}, report.Items...), report.Overhead) {
		for _, g := range row.PhaseRoles {
			out[g.Key] += g.Summary.Requests
		}
	}
	return out
}

// at returns the single "phase / role" of the requests made at base+d.
func (f *usagePhaseFixture) at(d time.Duration) string {
	f.t.Helper()
	report, err := f.s.Usage(f.ctx, f.task.ID, api.UsageQuery{From: f.base.Add(d), To: f.base.Add(d + time.Nanosecond)})
	if err != nil {
		f.t.Fatal(err)
	}
	got := usagePhaseRoles(report)
	if len(got) != 1 {
		f.t.Fatalf("requests at +%s: want one phase/role, got %v", d, got)
	}
	for k := range got {
		return k
	}
	return ""
}
func (f *usagePhaseFixture) want(d time.Duration, phaseRole, why string) {
	f.t.Helper()
	if got := f.at(d); got != phaseRole {
		f.t.Fatalf("+%s (%s): got %q, want %q", d, why, got, phaseRole)
	}
}

func TestUsagePhaseFollowsOpenOrder(t *testing.T) {
	f := newUsagePhaseFixture(t)
	const m, sec = time.Minute, time.Second
	f.requests("builder", 1*m, 1)
	assign := f.send(2*m, "lead", "builder", 0, usagePhaseAssign())
	// A host clock 1 s behind the hub: the opening request precedes the ASSIGN it acknowledged.
	f.ack(2*m+sec, "builder", assign)
	f.requests("builder", 2*m-sec, 1, f.handled("ack", assign))
	// Three activations: own Start REQUEST, the handler's RESULT, nothing handled.
	start := f.send(3*m, "builder", "handler", 0, usagePhaseRequest())
	f.requests("builder", 3*m+sec, 2, f.handled("post", start))
	started := f.send(4*m, "handler", "builder", start.Seq, usagePhaseResult(nil))
	f.requests("builder", 4*m+sec, 2, f.handled("ack", started))
	f.requests("builder", 5*m, 2)
	// Two orders open at once: a plain REQUEST from lead during the build.
	side := f.send(6*m, "lead", "builder", 0, usagePhaseRequest())
	f.ack(6*m+sec, "builder", side)
	f.requests("builder", 6*m+sec, 1, f.handled("ack", side))
	sideDone := f.send(7*m, "builder", "lead", side.Seq, usagePhaseResult(nil))
	f.requests("builder", 7*m+sec, 1, f.handled("reply", sideDone))
	// The frozen candidate RESULT closes the ASSIGN.
	result := f.send(8*m, "builder", "lead", assign.Seq, usagePhaseResult(nil))
	f.requests("builder", 8*m+sec, 1, f.handled("reply", result))
	f.requests("builder", 9*m, 1)
	// A lone plain REQUEST after the build is coordination, not build.
	late := f.send(10*m, "lead", "builder", 0, usagePhaseRequest())
	f.requests("builder", 10*m+sec, 1, f.handled("ack", late))

	const build, handoffs = "build / builder", "hand-offs / builder"
	for _, c := range []struct {
		d         time.Duration
		want, why string
	}{
		{1 * m, handoffs, "before the ASSIGN, nothing handled"},
		{2*m - sec, build, "opening request 1 s before the ASSIGN it acknowledged"},
		{3*m + sec, build, "activation that posted its own Start REQUEST"},
		{4*m + sec, build, "activation that acknowledged the handler RESULT"},
		{5 * m, build, "activation that handled nothing"},
		{6*m + sec, build, "ASSIGN and a plain REQUEST both open"},
		{7*m + sec, build, "after the plain REQUEST closed, ASSIGN still open"},
		{8*m + sec, handoffs, "activation that posted the RESULT closing the ASSIGN"},
		{9 * m, handoffs, "after the RESULT, nothing handled"},
		{10*m + sec, handoffs, "lone plain REQUEST after the build"},
	} {
		f.want(c.d, c.want, c.why)
	}
	report, err := f.s.Usage(f.ctx, f.task.ID, api.UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got := usagePhaseRoles(report); got[build] != 9 || got[handoffs] != 4 || len(got) != 2 {
		t.Fatalf("builder phases: %v", got)
	}
}

func TestUsageVerifierAndReviewerWindows(t *testing.T) {
	f := newUsagePhaseFixture(t)
	const m, sec = time.Minute, time.Second
	f.send(0, "lead", "builder", 0, usagePhaseAssign()) // freezes the item's criteria for the review
	for _, c := range []struct {
		agent, phase string
		order        api.Envelope
		result       *api.ReviewMetadata
	}{
		{"verifier", "verification / verifier", usagePhaseRequest(), nil},
		{"reviewer", "review round 1 / reviewer", usagePhaseReview(), &api.ReviewMetadata{Mode: "general", Candidate: candidateA}},
	} {
		handoffs := "hand-offs / " + c.agent
		f.requests(c.agent, 1*m, 1)
		order := f.send(2*m, "lead", c.agent, 0, c.order)
		f.requests(c.agent, 2*m+sec, 1, f.handled("ack", order))
		own := f.send(3*m, c.agent, "handler", 0, usagePhaseRequest())
		f.requests(c.agent, 3*m+sec, 1, f.handled("post", own))
		answered := f.send(4*m, "handler", c.agent, own.Seq, usagePhaseResult(nil))
		f.requests(c.agent, 4*m+sec, 1, f.handled("ack", answered))
		f.requests(c.agent, 5*m, 1)
		result := f.send(6*m, c.agent, "lead", order.Seq, usagePhaseResult(c.result))
		f.requests(c.agent, 6*m+sec, 1, f.handled("reply", result))
		f.requests(c.agent, 7*m, 1)
		report, err := f.s.Usage(f.ctx, f.task.ID, api.UsageQuery{})
		if err != nil {
			t.Fatal(err)
		}
		got := usagePhaseRoles(report)
		if got[c.phase] != 4 || got[handoffs] != 3 {
			t.Fatalf("%s window: %v", c.agent, got)
		}
		// Reports of several agents share a request time; check this agent's requests by its own groups.
		for key, n := range got {
			if strings.HasSuffix(key, " / "+c.agent) && key != c.phase && key != handoffs {
				t.Fatalf("%s: unexpected group %q (%d)", c.agent, key, n)
			}
		}
	}
}

// usageReportedItemHistory records a history shaped like the reported item:
// per role, the request counts tt usage showed for it, with the builder's
// requests spread over many activations and its Start exchange with the handler.
func usageReportedItemHistory(f *usagePhaseFixture) {
	const m = time.Minute
	f.requests("lead", 0, 100)
	plan := f.send(1*m, "lead", "planner", 0, usagePhaseRequest())
	f.requests("planner", 2*m, 45, f.handled("ack", plan))
	f.send(3*m, "planner", "lead", plan.Seq, usagePhaseResult(nil))
	f.requests("builder", 4*m, 10)
	assign := f.send(5*m, "lead", "builder", 0, usagePhaseAssign())
	f.requests("builder", 5*m+time.Second, 20, f.handled("ack", assign))
	start := f.send(6*m, "builder", "handler", 0, usagePhaseRequest())
	f.requests("builder", 6*m+time.Second, 30, f.handled("post", start))
	started := f.send(7*m, "handler", "builder", start.Seq, usagePhaseResult(nil))
	f.requests("builder", 7*m+time.Second, 40, f.handled("ack", started))
	for i := 0; i < 26; i++ { // later activations that handled no Board message
		f.requests("builder", time.Duration(8+i)*m, 10)
	}
	f.requests("handler", 20*m, 147, f.handled("ack", start))
	result := f.send(40*m, "builder", "lead", assign.Seq, usagePhaseResult(nil))
	f.requests("builder", 40*m+time.Second, 27, f.handled("reply", result))
	review := f.send(41*m, "lead", "reviewer", 0, usagePhaseReview())
	f.requests("reviewer", 42*m, 30, f.handled("ack", review))
	verify := f.send(43*m, "lead", "verifier", 0, usagePhaseRequest())
	f.requests("verifier", 44*m, 13, f.handled("ack", verify))
}

func TestUsageBuilderDominantPhaseIsBuild(t *testing.T) {
	f := newUsagePhaseFixture(t)
	usageReportedItemHistory(f)
	report, err := f.s.Usage(f.ctx, f.task.ID, api.UsageQuery{Item: f.item.ID})
	if err != nil || len(report.Items) != 1 {
		t.Fatal(report, err)
	}
	item := report.Items[0]
	roles := map[string]int{}
	for _, g := range item.Roles {
		roles[g.Key] = g.Summary.Requests
	}
	if roles["builder"] != 387 || roles["database_handler"] != 147 || roles["lead"] != 100 || roles["planner"] != 45 || roles["reviewer"] != 30 || roles["verifier"] != 13 || item.Summary.Requests != 722 {
		t.Fatalf("role totals: %v, %d requests", roles, item.Summary.Requests)
	}
	builder, top, topKey := 0, 0, ""
	got := map[string]int{}
	for _, g := range item.PhaseRoles {
		got[g.Key] = g.Summary.Requests
		if strings.HasSuffix(g.Key, " / builder") {
			builder += g.Summary.Requests
			if g.Summary.Requests > top {
				top, topKey = g.Summary.Requests, g.Key
			}
		}
	}
	if topKey != "build / builder" || builder != 387 || top*100 < builder*80 {
		t.Fatalf("builder dominant phase %q with %d of %d: %v", topKey, top, builder, got)
	}
	if got["handler bookkeeping / database_handler"] != 147 || got["intake and planning / planner"] != 45 || got["review round 1 / reviewer"] != 30 || got["verification / verifier"] != 13 || got["hand-offs / lead"] != 100 {
		t.Fatalf("other roles: %v", got)
	}
}

// usageLedgerRows returns every stored usage row, in a stable order.
func usageLedgerRows(t *testing.T, s *Store) []string {
	t.Helper()
	out := []string{}
	for _, query := range []string{
		`SELECT task_id||'|'||agent_id||'|'||run_id||'|'||request_id||'|'||revision||'|'||at||'|'||payload||'|'||projection FROM usage_turns ORDER BY 1`,
		`SELECT task_id||'|'||agent_id||'|'||run_id||'|'||request_id||'|'||revision||'|'||payload||'|'||projection FROM usage_turn_revisions ORDER BY 1`,
	} {
		rows, err := s.db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var row string
			if err = rows.Scan(&row); err != nil {
				t.Fatal(err)
			}
			out = append(out, row)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return out
}

func TestUsageRecordedTurnsReclassifiedOnRead(t *testing.T) {
	f := newUsagePhaseFixture(t)
	const m = time.Minute
	f.requests("builder", 1*m, 3)
	assign := f.send(2*m, "lead", "builder", 0, usagePhaseAssign())
	start := f.send(3*m, "builder", "handler", 0, usagePhaseRequest())
	f.requests("builder", 3*m+time.Second, 20, f.handled("post", start))
	f.requests("builder", 4*m, 30)
	f.send(5*m, "builder", "lead", assign.Seq, usagePhaseResult(nil))
	f.requests("builder", 6*m, 2)
	// Rewrite every stored projection the way the previous classifier recorded
	// these requests: hand-offs, whatever the builder was doing.
	for _, table := range []string{"usage_turns", "usage_turn_revisions"} {
		rows, err := f.s.db.Query(`SELECT rowid,projection FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		stored := map[int64]string{}
		for rows.Next() {
			var id int64
			var raw string
			if err = rows.Scan(&id, &raw); err != nil {
				t.Fatal(err)
			}
			var p api.UsageProjection
			if err = json.Unmarshal([]byte(raw), &p); err != nil {
				t.Fatal(err)
			}
			p.Phase, p.PhaseReason, p.ReviewRound = "hand-offs", "handled request "+fmt.Sprint(start.Seq), 0
			old, _ := json.Marshal(p)
			stored[id] = string(old)
		}
		rows.Close()
		if len(stored) != 55 {
			t.Fatalf("%s: %d rows", table, len(stored))
		}
		for id, raw := range stored {
			if _, err = f.s.db.Exec(`UPDATE `+table+` SET projection=? WHERE rowid=?`, raw, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := usageLedgerRows(t, f.s)
	if len(before) != 110 || strings.Contains(strings.Join(before, "\n"), `"phase":"build"`) {
		t.Fatal("fixture rows must all be stored as hand-offs", len(before))
	}
	report, err := f.s.Usage(f.ctx, f.task.ID, api.UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	got := usagePhaseRoles(report)
	if got["build / builder"] != 50 || got["hand-offs / builder"] != 5 || len(got) != 2 {
		t.Fatalf("reclassified phases: %v", got)
	}
	// Nothing dropped: 55 requests of the synthetic turn (input 21, cached 80, cache write 0, output 7, reasoning 2).
	want := map[string]string{"input": "1155", "cached": "4400", "cacheWrite": "0", "output": "385", "reasoning": "110"}
	if report.Summary.Requests != 55 || report.Summary.AllocatedTurns != "55" || fmt.Sprint(report.Summary.Tokens) != fmt.Sprint(want) {
		t.Fatalf("totals not conserved: %+v", report.Summary)
	}
	after := usageLedgerRows(t, f.s)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatal("a report read rewrote stored usage rows")
	}
}

func TestUsageReportReadCostBounded(t *testing.T) {
	f := newUsagePhaseFixture(t)
	const m = time.Minute
	agents := []string{"lead", "planner", "builder", "reviewer", "verifier", "handler"}
	assign := f.send(1*m, "lead", "builder", 0, usagePhaseAssign())
	plan := f.send(1*m, "lead", "planner", 0, usagePhaseRequest())
	verify := f.send(1*m, "lead", "verifier", 0, usagePhaseRequest())
	start := f.send(2*m, "builder", "handler", 0, usagePhaseRequest())
	record := func(each int) {
		for _, agent := range agents {
			f.requests(agent, 3*m, each/2)
		}
		f.requests("lead", 4*m, each/2, f.handled("post", assign), f.handled("post", plan), f.handled("post", verify))
		f.requests("planner", 4*m, each/2, f.handled("ack", plan))
		f.requests("builder", 4*m, each/2, f.handled("ack", assign), f.handled("post", start))
		f.requests("reviewer", 4*m, each/2)
		f.requests("verifier", 4*m, each/2, f.handled("ack", verify))
		f.requests("handler", 4*m, each/2, f.handled("ack", start))
	}
	read := func(turns int) (int, time.Duration) {
		cache := newUsagePhaseCache()
		began := time.Now()
		report, err := f.s.usageReport(f.ctx, f.task.ID, api.UsageQuery{}, cache)
		took := time.Since(began)
		if err != nil || report.Summary.Requests != turns {
			t.Fatal(report.Summary, err)
		}
		if got := usagePhaseRoles(report); got["build / builder"] != turns/6 || got["hand-offs / lead"] != turns/6 {
			t.Fatalf("%d turns: %v", turns, got)
		}
		return cache.orderQueries, took
	}
	record(100)
	small, _ := read(600)
	record(400)
	large, took := read(3000)
	// One obligation query per non-handler agent, however many requests each made.
	if small != 5 || large != small {
		t.Fatalf("obligation queries grew with the turn count: %d for 600 turns, %d for 3000", small, large)
	}
	if took > 2*time.Second {
		t.Fatalf("report over 3000 turns took %s", took)
	}
	t.Logf("3000 turns: %s, %d obligation queries", took, large)
}
