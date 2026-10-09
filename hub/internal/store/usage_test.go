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
	// Three new turns and no revision of any: nothing is superseded.
	if err = s.db.QueryRow(`SELECT count(*) FROM usage_turn_revisions`).Scan(&n); err != nil || n != 0 {
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
	// No turn here was revised, so only usage_turns holds rows.
	for table, want := range map[string]int{"usage_turns": 55, "usage_turn_revisions": 0} {
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
		if len(stored) != want {
			t.Fatalf("%s: %d rows", table, len(stored))
		}
		for id, raw := range stored {
			if _, err = f.s.db.Exec(`UPDATE `+table+` SET projection=? WHERE rowid=?`, raw, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := usageLedgerRows(t, f.s)
	if len(before) != 55 || strings.Contains(strings.Join(before, "\n"), `"phase":"build"`) {
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

// Token estimate warning (wi_3228104e700006c5, plan r2; compared figure
// wi_12d5c1ef9a73ceae). When a new or revised attributed turn first takes the
// team of an item's running queue entry past the project's warning level
// times the item's saved estimate, the hub posts one directed notice to the
// item's running lead and one to the owner helper, once per item, queue entry
// and estimate value.

const usageWarningTestSubject = "An item has passed its token estimate warning level"

type usageWarningFixture struct {
	t    *testing.T
	s    *Store
	task api.Task
	// metered uploads the turns of use and turn, acknowledging orders. It is
	// the fixture's database handler until an entry runs, then that entry's
	// team member.
	metered api.Agent
	orders  []api.Message
	// handler is the fixture's database handler and source its orders, which
	// the bindings name as their work order.
	handler  api.Agent
	source   []api.Message
	items    []api.WorkItem
	lead     api.Agent
	helper   api.Agent
	entry    string
	admitted time.Time
	entries  int
	agents   int
	turns    int
	batches  int
	estimate int
}

// newUsageWarningFixture is the synthetic usage project, optionally with a
// running queue entry on item 0 that has a team member and a running lead,
// and a registered owner helper.
func newUsageWarningFixture(t *testing.T, withLead, withHelper bool) *usageWarningFixture {
	t.Helper()
	s, task, metered, items, orders := usageFixture(t)
	f := &usageWarningFixture{t: t, s: s, task: task, metered: metered, orders: orders, handler: metered, source: orders, items: items}
	if withLead {
		f.addEntry()
		f.addLead()
	}
	if withHelper {
		f.addHelper()
	}
	return f
}

// register adds an agent with the given project role.
func (f *usageWarningFixture) register(name, role string) api.Agent {
	f.t.Helper()
	a, _ := f.agent(name, role, false)
	return a
}

// agent registers an agent with the given project role and, when ordered,
// gives it an order for each item, so its turns can be attributed to them.
func (f *usageWarningFixture) agent(name, role string, ordered bool) (api.Agent, []api.Message) {
	f.t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	f.agents++
	name = fmt.Sprintf("%s-%d", name, f.agents)
	a, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: name, AgentID: api.NewID("agt"), Runtime: "codex", Host: "fixture", Session: name}, by)
	if err != nil {
		f.t.Fatal(err)
	}
	if role != "" {
		if _, err = f.s.db.Exec(`UPDATE agents SET role=? WHERE id=?`, role, a.ID); err != nil {
			f.t.Fatal(err)
		}
	}
	if !ordered {
		return a, nil
	}
	return a, f.order(a)
}

// order gives an agent an order for each item.
func (f *usageWarningFixture) order(a api.Agent) []api.Message {
	f.t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	orders := []api.Message{}
	for i, item := range f.items {
		m, err := f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{To: a.ID, RequestID: fmt.Sprintf("order-%s-%d", a.ID, i), WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}, Envelope: &api.Envelope{Kind: "request", To: a.ID, Subject: "Handle synthetic evidence", Body: api.EnvelopeBody{Ask: "Record synthetic data"}}}, by)
		if err != nil {
			f.t.Fatal(err)
		}
		orders = append(orders, m)
	}
	return orders
}

// bind records the agent's current run as bound to item 0 at an instant.
func (f *usageWarningFixture) bind(a api.Agent, at time.Time, teamRole string) {
	f.t.Helper()
	if _, err := f.s.db.Exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,team_role,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,?,0,?,?,'{}',?)`,
		a.ID, a.RunID, f.task.ID, f.items[0].ID, f.task.ID, f.source[0].Seq, teamRole, strings.Repeat("d", 64), ts(at)); err != nil {
		f.t.Fatal(err)
	}
}

// addEntry gives item 0 a running queue entry and a team member bound to the
// item, who becomes the metered agent. The first entry is admitted on a whole
// second and a later one two and a half seconds after the one before; a team
// is bound half a second after its entry. So entries and bindings share
// seconds and differ only in fractions, where the stored text does not sort.
func (f *usageWarningFixture) addEntry() {
	f.t.Helper()
	if f.entry == "" {
		f.admitted = time.Now().UTC().Truncate(time.Second)
	} else {
		f.admitted = f.admitted.Add(2500 * time.Millisecond)
	}
	f.entry = api.NewID("tqe")
	f.entries++
	if _, err := f.s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,attempt,created_at,updated_at,repository,base_commit) VALUES(?,?,?,?,1,'planned',1,'running',1,?,?,?,'fixture',?)`,
		f.entry, f.task.ID, f.items[0].ID, f.items[0].Revision, f.entries, ts(f.admitted), ts(f.admitted), strings.Repeat("c", 40)); err != nil {
		f.t.Fatal(err)
	}
	f.metered, f.orders = f.agent("member", "", true)
	f.bind(f.metered, f.admitted.Add(500*time.Millisecond), "member")
}

// addLead gives the running entry's team its running lead.
func (f *usageWarningFixture) addLead() {
	f.t.Helper()
	f.lead = f.register("lead", "")
	f.bind(f.lead, f.admitted.Add(500*time.Millisecond), "lead")
	if _, err := f.s.db.Exec(`INSERT OR REPLACE INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, f.task.ID, f.items[0].ID, f.lead.ID, f.lead.RunID); err != nil {
		f.t.Fatal(err)
	}
}

// release ends the running entry and its lead, as a finished team does.
func (f *usageWarningFixture) release() {
	f.t.Helper()
	if _, err := f.s.db.Exec(`UPDATE team_queue_entries SET state='finished',released_at=? WHERE id=?`, ts(f.admitted.Add(time.Second)), f.entry); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE item_team_leads SET state='closed' WHERE task_id=? AND item_id=?`, f.task.ID, f.items[0].ID); err != nil {
		f.t.Fatal(err)
	}
}

// as runs work with another agent as the metered one.
func (f *usageWarningFixture) as(a api.Agent, orders []api.Message, work func()) {
	f.t.Helper()
	metered, own := f.metered, f.orders
	f.metered, f.orders = a, orders
	work()
	f.metered, f.orders = metered, own
}
func (f *usageWarningFixture) addHelper() {
	f.t.Helper()
	f.helper = *registerHelper(f.t, f.s, f.task.ID, helperRequest("warning-helper"), api.Caller{Node: "fixture", User: "owner"}).Agent
}

// setEstimate saves an estimate on item i as the owner.
func (f *usageWarningFixture) setEstimate(i int, tokens int64) {
	f.t.Helper()
	f.estimate++
	basis := "synthetic"
	if tokens == 0 {
		basis = "" // a clear takes no basis
	}
	req := estimateRequest(f.items[i].Revision, fmt.Sprintf("warning-estimate-%d", f.estimate), tokens, basis, api.Agent{})
	if _, _, err := f.s.CreateWorkItemUpdate(context.Background(), f.task.ID, f.items[i].ID, req, api.Caller{Node: "fixture", User: "owner"}); err != nil {
		f.t.Fatal(err)
	}
}

// turn is a complete synthetic request of exactly tokens, attributed to item
// i by an acknowledgement of its order.
func (f *usageWarningFixture) turn(i int, tokens int64) api.UsageTurn {
	f.turns++
	turn := syntheticUsageTurn(fmt.Sprintf("warning-turn-%d", f.turns))
	turn.Tokens = map[string]int64{"input": tokens, "cached": 0, "cacheWrite": 0, "output": 0, "reasoning": 0}
	turn.Raw = map[string]int64{"input_tokens": tokens, "cached_input_tokens": 0, "cache_write_input_tokens": 0, "output_tokens": 0, "reasoning_output_tokens": 0}
	turn.Handled = []api.UsageEvidence{{TaskID: f.task.ID, Seq: f.orders[i].Seq, Operation: "ack", At: turn.At}}
	return turn
}

// upload stores the turns as one new batch and returns it for a replay.
func (f *usageWarningFixture) upload(turns ...api.UsageTurn) api.UsageBatch {
	f.t.Helper()
	f.batches++
	batch := usageBatch(f.metered, fmt.Sprintf("warning-batch-%d", f.batches), turns...)
	f.replay(batch)
	return batch
}
func (f *usageWarningFixture) replay(batch api.UsageBatch) {
	f.t.Helper()
	if receipt, err := f.s.ReportUsage(context.Background(), f.task.ID, f.metered.ID, batch); err != nil || receipt.Turns != len(batch.Turns) {
		f.t.Fatalf("usage upload %+v %v", receipt, err)
	}
}

// use uploads one new turn of tokens for item i.
func (f *usageWarningFixture) use(i int, tokens int64) api.UsageBatch {
	f.t.Helper()
	return f.upload(f.turn(i, tokens))
}

// notices are the warning notices addressed to one agent, oldest first.
func (f *usageWarningFixture) notices(agent api.Agent) []api.Message {
	f.t.Helper()
	if agent.ID == "" {
		return nil
	}
	messages, err := f.s.ListMessages(context.Background(), f.task.ID, 0, agent.ID, 500)
	if err != nil {
		f.t.Fatal(err)
	}
	out := []api.Message{}
	for _, m := range messages {
		if m.To == agent.ID && m.Envelope != nil && m.Envelope.Kind == api.EnvelopeKindNotice && m.Envelope.Subject == usageWarningTestSubject {
			out = append(out, m)
		}
	}
	return out
}
func (f *usageWarningFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(query, err)
	}
	return n
}

// want asserts the warning notices of the lead and the helper and the rows
// recorded for the project.
func (f *usageWarningFixture) want(why string, lead, helper, rows int) {
	f.t.Helper()
	if got := len(f.notices(f.lead)); got != lead {
		f.t.Fatalf("%s: lead has %d warning notices, want %d", why, got, lead)
	}
	if got := len(f.notices(f.helper)); got != helper {
		f.t.Fatalf("%s: owner helper has %d warning notices, want %d", why, got, helper)
	}
	if got := f.count(`SELECT count(*) FROM usage_entry_warnings WHERE task_id=?`, f.task.ID); got != rows {
		f.t.Fatalf("%s: %d warning rows, want %d", why, got, rows)
	}
}

// budget is item 0's budget as a work item read returns it.
func (f *usageWarningFixture) budget() *api.TokenBudget {
	f.t.Helper()
	item, err := f.s.GetWorkItem(context.Background(), f.task.ID, f.items[0].ID)
	if err != nil || item.Budget == nil {
		f.t.Fatalf("budget %+v %v", item.Budget, err)
	}
	return item.Budget
}

func TestUsageWarningOnCrossing(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	f.setEstimate(0, 1000)
	f.use(0, 900)
	f.use(0, 600)
	f.want("1500 of 1000 is at the level, not past it", 0, 0, 0)
	f.use(0, 1)
	f.want("1501 of 1000 is past 1.5", 1, 1, 1)
	item := f.items[0]
	for _, m := range []api.Message{f.notices(f.lead)[0], f.notices(f.helper)[0]} {
		e := m.Envelope
		want := map[string]string{"item": item.ID, "estimateTokens": "1000", "actualTokens": "1501", "actualState": "measured", "ratio": "1501/1000", "lifetimeTokens": "1501", "lifetimeState": "measured", "threshold": "1.5", "entry": f.entry, "lead": f.lead.Name}
		if !reflect.DeepEqual(e.Refs, want) {
			t.Fatalf("refs %v, want %v", e.Refs, want)
		}
		text := "Item " + item.ID + " (Item 0): the team of queue entry " + f.entry + " has used 1501 tokens on it against an estimate of 1000: 1.50 times, past the warning level of 1.5. The item's lifetime total is 1501 tokens, which includes work before this entry. Running team: lead " + f.lead.Name + ". Nothing is paused or held. No reply is needed."
		if e.Body.Text != text {
			t.Fatalf("text %q\nwant %q", e.Body.Text, text)
		}
		if m.From.AgentID != "" || m.From.Node != "system" || m.From.User != "usage-warning" || len(m.WorkItems) != 0 {
			t.Fatalf("sender or links %+v %+v", m.From, m.WorkItems)
		}
	}
	var estimate, leadSeq, helperSeq int64
	var entry, threshold, actual, state, lifetime, lifetimeState, leadAgent, helperAgent string
	if err := f.s.db.QueryRow(`SELECT entry_id,estimate_tokens,threshold,team_tokens,team_state,lifetime_tokens,lifetime_state,lead_agent,lead_message_seq,helper_agent,helper_message_seq FROM usage_entry_warnings WHERE task_id=? AND item_id=?`, f.task.ID, item.ID).
		Scan(&entry, &estimate, &threshold, &actual, &state, &lifetime, &lifetimeState, &leadAgent, &leadSeq, &helperAgent, &helperSeq); err != nil {
		t.Fatal(err)
	}
	if entry != f.entry || lifetime != "1501" || lifetimeState != "measured" || estimate != 1000 || threshold != "1.5" || actual != "1501" || state != "measured" || leadAgent != f.lead.ID || helperAgent != f.helper.ID || leadSeq != f.notices(f.lead)[0].Seq || helperSeq != f.notices(f.helper)[0].Seq {
		t.Fatal("warning row", estimate, threshold, actual, state, leadAgent, leadSeq, helperAgent, helperSeq)
	}
}

func TestUsageWarningNeverWithoutEstimate(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	f.use(0, 5_000_000)
	f.use(0, 5_000_000)
	f.want("no estimate", 0, 0, 0)
	item, err := f.s.GetWorkItem(context.Background(), f.task.ID, f.items[0].ID)
	if err != nil || item.Budget == nil || item.Budget.Estimate != nil || item.Budget.ActualTokens != "10000000" || item.Budget.Ratio != "" {
		t.Fatalf("budget %+v %v", item.Budget, err)
	}
	// Clearing an estimate returns the item to never warning.
	f.setEstimate(0, 1000)
	f.setEstimate(0, 0)
	f.use(0, 1)
	f.want("cleared estimate", 0, 0, 0)
}

func TestUsageWarningOncePerEstimateValue(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	f.setEstimate(0, 1000)
	crossing := f.use(0, 1501)
	f.want("crossing", 1, 1, 1)
	messages := f.count(`SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID)
	f.replay(crossing)
	f.use(0, 400)
	f.upload(f.turn(0, 10), f.turn(0, 20))
	f.replay(crossing)
	f.want("replay and new turns", 1, 1, 1)
	if got := f.count(`SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID); got != messages {
		t.Fatalf("messages grew from %d to %d", messages, got)
	}
}

func TestUsageWarningRaisedEstimateRearms(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	f.setEstimate(0, 1000)
	f.use(0, 1600)
	f.want("first estimate", 1, 1, 1)
	f.setEstimate(0, 2000)
	f.want("the estimate save posts nothing", 1, 1, 1)
	f.use(0, 1400)
	f.want("3000 of 2000 is at the level", 1, 1, 1)
	f.use(0, 1)
	f.want("3001 of 2000 is past 1.5", 2, 2, 2)
	f.use(0, 500)
	f.want("once for the raised value", 2, 2, 2)
	second := f.notices(f.lead)[1].Envelope
	if second.Refs["estimateTokens"] != "2000" || second.Refs["actualTokens"] != "3001" {
		t.Fatalf("second notice refs %v", second.Refs)
	}
	// Returning to a value that already warned stays silent.
	f.setEstimate(0, 1000)
	f.use(0, 1)
	f.want("returning to a warned value", 2, 2, 2)
}

func TestUsageWarningWithoutTeam(t *testing.T) {
	f := newUsageWarningFixture(t, false, true)
	f.setEstimate(0, 1000)
	// No entry is running, so there is no team figure to compare.
	f.use(0, 5_000_000)
	f.want("no running entry", 0, 0, 0)
	if b := f.budget(); b.Team != nil || b.ActualTokens != "5000000" || b.Ratio != "5000" {
		t.Fatalf("budget without a running entry %+v", b)
	}
	// A running entry without a running lead still warns the owner helper.
	f.addEntry()
	f.use(0, 1501)
	f.want("no lead", 0, 1, 1)
	e := f.notices(f.helper)[0].Envelope
	if !strings.Contains(e.Body.Text, " Queue entry "+f.entry+" is running without a running lead. Nothing is paused or held.") || e.Refs["lead"] != "" || e.Refs["entry"] != f.entry {
		t.Fatalf("notice %q %v", e.Body.Text, e.Refs)
	}
	if got := f.count(`SELECT count(*) FROM usage_entry_warnings WHERE task_id=? AND entry_id=? AND lead_agent='' AND lead_message_seq=0 AND helper_agent=?`, f.task.ID, f.entry, f.helper.ID); got != 1 {
		t.Fatal("row does not record the missing lead")
	}
	if got := f.count(`SELECT count(*) FROM messages WHERE task_id=? AND envelope LIKE ?`, f.task.ID, "%"+usageWarningTestSubject+"%"); got != 1 {
		t.Fatalf("%d warning messages, want 1", got)
	}
}

func TestUsageWarningWaitsForRecipient(t *testing.T) {
	f := newUsageWarningFixture(t, false, false)
	f.addEntry()
	f.setEstimate(0, 1000)
	warnings := func() int {
		return f.count(`SELECT count(*) FROM messages WHERE task_id=? AND envelope LIKE ?`, f.task.ID, "%"+usageWarningTestSubject+"%")
	}
	crossing := f.use(0, 1501)
	if got := f.count(`SELECT count(*) FROM usage_turns WHERE task_id=?`, f.task.ID); got != 1 {
		t.Fatalf("%d turns stored, want 1", got)
	}
	f.want("no recipient", 0, 0, 0)
	if warnings() != 0 {
		t.Fatal("a warning was posted with no recipient")
	}
	f.addHelper()
	f.replay(crossing)
	f.want("a replay checks nothing", 0, 0, 0)
	f.use(0, 1)
	f.want("the next new turn warns", 0, 1, 1)
	f.use(0, 1)
	f.want("then once only", 0, 1, 1)
	if warnings() != 1 {
		t.Fatalf("%d warning messages, want 1", warnings())
	}
}

// second adds another project with its own item, running queue entry, team
// member and owner helper to the same database.
func (f *usageWarningFixture) second() *usageWarningFixture {
	f.t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := f.s.CreateTask(ctx, api.CreateTaskRequest{Name: "Second synthetic usage", Swarm: true}, by)
	if err != nil {
		f.t.Fatal(err)
	}
	a, err := f.s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "shared-two", AgentID: api.NewID("agt"), Runtime: "codex", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "synthetic"}, by)
	if err != nil {
		f.t.Fatal(err)
	}
	item, err := f.s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Second item", RequestID: "create-second"}, by)
	if err != nil {
		f.t.Fatal(err)
	}
	m, err := f.s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: a.ID, RequestID: "order-second", WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}, Envelope: &api.Envelope{Kind: "request", To: a.ID, Subject: "Handle synthetic evidence", Body: api.EnvelopeBody{Ask: "Record synthetic data"}}}, by)
	if err != nil {
		f.t.Fatal(err)
	}
	g := &usageWarningFixture{t: f.t, s: f.s, task: task, metered: a, orders: []api.Message{m}, handler: a, source: []api.Message{m}, items: []api.WorkItem{item}, agents: 1000, turns: 1000, batches: 1000, estimate: 1000}
	g.addEntry()
	g.addHelper()
	return g
}

func TestUsageWarningThresholdPerProject(t *testing.T) {
	f := newUsageWarningFixture(t, false, true)
	f.addEntry()
	g := f.second()
	ctx := context.Background()
	owner := api.Caller{Node: "fixture", User: "owner"}
	got, err := f.s.UsageWarnings(ctx, f.task.ID)
	if err != nil || got.Threshold != "1.5" || !got.Default || len(got.Warnings) != 0 {
		t.Fatalf("default %+v %v", got, err)
	}
	got, err = f.s.SetUsageWarning(ctx, f.task.ID, api.UsageWarningRequest{Threshold: "2"}, owner)
	if err != nil || got.Threshold != "2" || got.Default {
		t.Fatalf("set %+v %v", got, err)
	}
	if other, err := f.s.UsageWarnings(ctx, g.task.ID); err != nil || other.Threshold != "1.5" || !other.Default {
		t.Fatalf("the second project changed: %+v %v", other, err)
	}
	for _, bad := range []string{"0.9", "101", "abc", "1.2345", "", "1.", ".5", "-2", "+2", "1e1", " 2", "02", "100.001"} {
		if _, err = f.s.SetUsageWarning(ctx, f.task.ID, api.UsageWarningRequest{Threshold: bad}, owner); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("threshold %q: %v", bad, err)
		}
	}
	if _, err = f.s.SetUsageWarning(ctx, f.task.ID, api.UsageWarningRequest{Threshold: "3", AgentID: f.metered.ID}, owner); !errors.Is(err, api.ErrConflict) {
		t.Fatal("an agent that is not the owner helper set the threshold", err)
	}
	if _, err = f.s.SetUsageWarning(ctx, f.task.ID, api.UsageWarningRequest{Threshold: "3", AgentID: g.helper.ID}, owner); !errors.Is(err, api.ErrConflict) {
		t.Fatal("another project's owner helper set the threshold", err)
	}
	if _, err = f.s.SetUsageWarning(ctx, "tsk_0000000000000000", api.UsageWarningRequest{Threshold: "3"}, owner); !errors.Is(err, api.ErrNotFound) {
		t.Fatal("unknown project", err)
	}
	if got, err = f.s.UsageWarnings(ctx, f.task.ID); err != nil || got.Threshold != "2" {
		t.Fatalf("a refused save changed the threshold: %+v %v", got, err)
	}
	for _, bounds := range []string{"1", "100", "1.001"} {
		if _, err = g.s.SetUsageWarning(ctx, g.task.ID, api.UsageWarningRequest{Threshold: bounds, AgentID: g.helper.ID}, owner); err != nil {
			t.Fatalf("threshold %q by the owner helper: %v", bounds, err)
		}
	}
	if _, err = g.s.SetUsageWarning(ctx, g.task.ID, api.UsageWarningRequest{Threshold: "1.5", AgentID: g.helper.ID}, owner); err != nil {
		t.Fatal(err)
	}
	f.setEstimate(0, 1000)
	g.setEstimate(0, 1000)
	f.use(0, 1600)
	g.use(0, 1600)
	f.want("1.6 times under a level of 2", 0, 0, 0)
	g.want("1.6 times under the 1.5 level of the second project", 0, 1, 1)
	f.use(0, 401)
	f.want("2001 of 1000 is past 2", 0, 1, 1)
	if e := f.notices(f.helper)[0].Envelope; e.Refs["threshold"] != "2" || !strings.Contains(e.Body.Text, "past the warning level of 2. ") {
		t.Fatalf("notice %q %v", e.Body.Text, e.Refs)
	}
	got, err = f.s.UsageWarnings(ctx, f.task.ID)
	if err != nil || len(got.Warnings) != 1 {
		t.Fatalf("warnings %+v %v", got, err)
	}
	w := got.Warnings[0]
	if w.EntryID != f.entry || w.LifetimeTokens != "2001" || w.LifetimeState != "measured" || w.ItemID != f.items[0].ID || w.EstimateTokens != 1000 || w.ActualTokens != "2001" || w.ActualState != "measured" || w.Ratio != "2001/1000" || w.Threshold != "2" || w.LeadAgent != "" || w.HelperAgent != f.helper.ID || w.HelperMessageSeq != f.notices(f.helper)[0].Seq || w.At.IsZero() {
		t.Fatalf("warning %+v", w)
	}
	// A changed level never re-warns an estimate value that already warned.
	if _, err = f.s.SetUsageWarning(ctx, f.task.ID, api.UsageWarningRequest{Threshold: "1.5"}, owner); err != nil {
		t.Fatal(err)
	}
	f.use(0, 1)
	f.want("changed level", 0, 1, 1)
}

func TestUsageWarningFailureKeepsUpload(t *testing.T) {
	stored := func(f *usageWarningFixture, turns int) {
		f.t.Helper()
		if got := f.count(`SELECT count(*) FROM usage_turns WHERE task_id=?`, f.task.ID); got != turns {
			f.t.Fatalf("%d turns stored, want %d", got, turns)
		}
		if got := f.count(`SELECT count(*) FROM usage_item_shares WHERE task_id=? AND item_id=?`, f.task.ID, f.items[0].ID); got != turns {
			f.t.Fatalf("%d share rows, want %d", got, turns)
		}
		if got := f.count(`SELECT count(*) FROM usage_receipts WHERE task_id=?`, f.task.ID); got != turns {
			f.t.Fatalf("%d usage receipts, want %d", got, turns)
		}
	}
	exec := func(f *usageWarningFixture, statement string) {
		f.t.Helper()
		if _, err := f.s.db.Exec(statement); err != nil {
			f.t.Fatal(statement, err)
		}
	}
	warnings := func(f *usageWarningFixture) int {
		return f.count(`SELECT count(*) FROM messages WHERE task_id=? AND envelope LIKE ?`, f.task.ID, "%"+usageWarningTestSubject+"%")
	}
	t.Run("dedupe read", func(t *testing.T) {
		f := newUsageWarningFixture(t, true, true)
		f.setEstimate(0, 1000)
		exec(f, `ALTER TABLE usage_entry_warnings RENAME TO usage_entry_warnings_away`)
		f.use(0, 1501)
		stored(f, 1)
		if warnings(f) != 0 {
			t.Fatal("a warning was posted without its once-only record")
		}
		exec(f, `ALTER TABLE usage_entry_warnings_away RENAME TO usage_entry_warnings`)
		f.use(0, 1)
		stored(f, 2)
		f.want("after the fault clears", 1, 1, 1)
	})
	t.Run("dedupe table dropped", func(t *testing.T) {
		f := newUsageWarningFixture(t, true, true)
		f.setEstimate(0, 1000)
		exec(f, `DROP TABLE usage_entry_warnings`)
		f.use(0, 1501)
		stored(f, 1)
		if warnings(f) != 0 {
			t.Fatal("a warning was posted without its once-only record")
		}
	})
	t.Run("settings read", func(t *testing.T) {
		f := newUsageWarningFixture(t, true, true)
		f.setEstimate(0, 1000)
		exec(f, `ALTER TABLE usage_warning_settings RENAME TO usage_warning_settings_away`)
		f.use(0, 1501)
		stored(f, 1)
		f.want("the threshold cannot be read", 0, 0, 0)
		exec(f, `ALTER TABLE usage_warning_settings_away RENAME TO usage_warning_settings`)
		f.use(0, 1)
		stored(f, 2)
		f.want("after the fault clears", 1, 1, 1)
	})
	t.Run("after the notices", func(t *testing.T) {
		f := newUsageWarningFixture(t, true, true)
		f.setEstimate(0, 1000)
		exec(f, `CREATE TRIGGER usage_warning_fault BEFORE INSERT ON usage_entry_warnings BEGIN SELECT RAISE(ABORT,'injected warning fault'); END`)
		tables := []string{"messages", "message_post_requests", "obligations", "wake_jobs", "usage_entry_warnings", "usage_budget_warnings", "events"}
		counts := func() []int {
			out := []int{}
			for _, table := range tables {
				out = append(out, f.count(`SELECT count(*) FROM `+table))
			}
			return out
		}
		before := counts()
		crossing := f.use(0, 1501)
		stored(f, 1)
		if after := counts(); !reflect.DeepEqual(before, after) {
			t.Fatalf("a failed warning left rows behind: %v before %v, after %v", tables, before, after)
		}
		f.want("the row insert fails", 0, 0, 0)
		exec(f, `DROP TRIGGER usage_warning_fault`)
		f.replay(crossing)
		f.want("a replay checks nothing", 0, 0, 0)
		f.use(0, 1)
		stored(f, 2)
		f.want("the next new turn warns", 1, 1, 1)
		f.use(0, 1)
		f.want("then once only", 1, 1, 1)
	})
	// A cancelled upload is not a warning failure: it stores nothing.
	t.Run("cancelled", func(t *testing.T) {
		f := newUsageWarningFixture(t, true, true)
		f.setEstimate(0, 1000)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := f.s.ReportUsage(ctx, f.task.ID, f.metered.ID, usageBatch(f.metered, "cancelled", f.turn(0, 1501))); err == nil {
			t.Fatal("a cancelled upload was stored")
		}
		stored(f, 0)
		f.want("cancelled", 0, 0, 0)
	})
}

func TestUsageWarningWakeAndDelivery(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	ctx := context.Background()
	f.setEstimate(0, 1000)
	f.use(0, 1501)
	f.want("crossing", 1, 1, 1)
	now := time.Now()
	overdue := func(why string) {
		t.Helper()
		late, err := f.s.ListObligations(ctx, f.task.ID, ObligationFilter{Overdue: true}, now.Add(48*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range late {
			if o.MessageSeq == f.notices(f.lead)[0].Seq || o.MessageSeq == f.notices(f.helper)[0].Seq {
				t.Fatalf("%s: warning notice %d is overdue: %+v", why, o.MessageSeq, o)
			}
		}
	}
	for _, to := range []api.Agent{f.lead, f.helper} {
		seq := f.notices(to)[0].Seq
		if got := f.count(`SELECT count(*) FROM obligations WHERE task_id=? AND message_seq=? AND agent_id=? AND needs=? AND source_kind='notice' AND state<>?`, f.task.ID, seq, to.ID, api.ObligationNeedsDelivery, api.ObligationClosed); got != 1 {
			t.Fatalf("%s: %d open delivery obligations for its notice, want 1", to.Name, got)
		}
		if got := f.count(`SELECT count(*) FROM obligations WHERE task_id=? AND message_seq=?`, f.task.ID, seq); got != 1 {
			t.Fatalf("%s: %d obligations for its notice, want 1", to.Name, got)
		}
		if got := f.count(`SELECT count(*) FROM wake_jobs w JOIN obligations o ON o.id=w.obligation_id WHERE o.message_seq=? AND w.agent_id=? AND w.state=?`, seq, to.ID, wakePending); got != 1 {
			t.Fatalf("%s: %d pending wake jobs for its notice, want 1", to.Name, got)
		}
	}
	overdue("unread")
	if _, err := f.s.db.Exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(now), f.lead.ID); err != nil {
		t.Fatal(err)
	}
	job, err := f.s.LeaseWakeJob(ctx, f.task.ID, f.lead.ID, f.lead.RunID, now.Add(time.Second))
	if err != nil || job == nil || job.MessageSeq != f.notices(f.lead)[0].Seq || job.AgentID != f.lead.ID {
		t.Fatalf("lead wake %+v %v", job, err)
	}
	for _, to := range []api.Agent{f.lead, f.helper} {
		seq := f.notices(to)[0].Seq
		// Reading alone settles nothing; a delivery-only obligation closes
		// when the recipient's current run fetches what it owes.
		if err = f.s.MarkRead(ctx, f.task.ID, api.MarkReadRequest{AgentID: to.ID, UpTo: seq}); err != nil {
			t.Fatal(err)
		}
		if err = f.s.MarkObligationsDelivered(ctx, f.task.ID, to.ID, to.RunID, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if got := f.count(`SELECT count(*) FROM obligations WHERE task_id=? AND message_seq=? AND state=? AND outcome=?`, f.task.ID, seq, api.ObligationClosed, api.OutcomeDelivered); got != 1 {
			t.Fatalf("%s: the notice obligation stayed open after delivery", to.Name)
		}
	}
	overdue("read")
}

func TestUsageWarningRetiredHelperNotResumed(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	ctx := context.Background()
	now := time.Now()
	if _, err := f.s.db.Exec(`UPDATE agents SET status=?,last_seen_at=? WHERE id=?`, api.AgentRetired, ts(now), f.helper.ID); err != nil {
		t.Fatal(err)
	}
	f.setEstimate(0, 1000)
	f.use(0, 1501)
	f.want("a retired helper still gets its notice", 1, 1, 1)
	helper, err := f.s.GetAgent(ctx, f.helper.ID)
	if err != nil || helper.Status != api.AgentRetired || helper.RunID != f.helper.RunID {
		t.Fatalf("helper %+v %v", helper, err)
	}
	if !agentOnlineAt(helper, now.Add(time.Second)) {
		t.Fatal("fixture: the retired helper must be online for the lease to be refused by status")
	}
	job, err := f.s.LeaseWakeJob(ctx, f.task.ID, f.helper.ID, f.helper.RunID, now.Add(time.Second))
	if err != nil || job != nil {
		t.Fatalf("a retired helper was leased a wake: %+v %v", job, err)
	}
	if got := f.count(`SELECT count(*) FROM wake_jobs WHERE agent_id=? AND state=?`, f.helper.ID, wakeLeased); got != 0 {
		t.Fatalf("%d leased wake jobs for the retired helper", got)
	}
	if got := f.count(`SELECT count(*) FROM wake_jobs WHERE agent_id=?`, f.helper.ID); got != 1 {
		t.Fatalf("%d wake jobs for the retired helper, want the 1 pending", got)
	}
}

func TestUsageWarningUnchangedTurnsDoNotCheck(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	f.setEstimate(0, 1000)
	turn := f.turn(0, 2000)
	f.upload(turn)
	f.want("first estimate", 1, 1, 1)
	f.setEstimate(0, 1200)
	f.upload(turn)
	f.want("a batch of unchanged turns checks nothing", 1, 1, 1)
	revised := turn
	revised.Revision = 2
	revised.Tokens = map[string]int64{"input": 2001, "cached": 0, "cacheWrite": 0, "output": 0, "reasoning": 0}
	revised.Raw = map[string]int64{"input_tokens": 2001, "cached_input_tokens": 0, "cache_write_input_tokens": 0, "output_tokens": 0, "reasoning_output_tokens": 0}
	f.upload(revised)
	f.want("a revised turn checks the new estimate value", 2, 2, 2)
	if refs := f.notices(f.helper)[1].Envelope.Refs; refs["estimateTokens"] != "1200" || refs["actualTokens"] != "2001" {
		t.Fatalf("refs %v", refs)
	}
}

// A request shared by two items and an incomplete request keep the warning
// exact: the actual is a rational, and a partial actual says "at least".
func TestUsageWarningSharedAndPartialActual(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	f.setEstimate(0, 1000)
	shared := f.turn(0, 3001)
	shared.Handled = append(shared.Handled, api.UsageEvidence{TaskID: f.task.ID, Seq: f.orders[1].Seq, Operation: "ack", At: shared.At})
	shared.Complete = false
	f.upload(shared)
	f.want("half of 3001 is past 1500", 1, 1, 1)
	e := f.notices(f.lead)[0].Envelope
	if e.Refs["actualTokens"] != "3001/2" || e.Refs["actualState"] != "partial" || e.Refs["ratio"] != "3001/2000" || e.Refs["lifetimeTokens"] != "3001/2" || e.Refs["lifetimeState"] != "partial" ||
		!strings.Contains(e.Body.Text, "has used at least 1501 tokens on it against an estimate of 1000: 1.50 times") || !strings.Contains(e.Body.Text, "lifetime total is at least 1501 tokens") {
		t.Fatalf("notice %q %v", e.Body.Text, e.Refs)
	}
}

// A closed project still takes late usage, and gets no new Board message.
func TestUsageWarningClosedProjectIsSilent(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	f.setEstimate(0, 1000)
	if _, err := f.s.db.Exec(`UPDATE tasks SET status=? WHERE id=?`, api.TaskClosed, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.use(0, 1501)
	f.want("closed project", 0, 0, 0)
	if _, err := f.s.db.Exec(`UPDATE tasks SET status=? WHERE id=?`, api.TaskOpen, f.task.ID); err != nil {
		t.Fatal(err)
	}
	f.use(0, 1)
	f.want("reopened project", 1, 1, 1)
}

// A committed warning wakes event waiters, as any post does (review f1). An
// upload that posts no warning leaves a waiter waiting, as before.
func TestUsageWarningNotifiesEventWaiters(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	ctx := context.Background()
	f.setEstimate(0, 1000)
	waiting := func() bool {
		f.s.mu.Lock()
		defer f.s.mu.Unlock()
		_, ok := f.s.waiters[f.task.ID]
		return ok
	}
	type waited struct {
		events []api.Event
		took   time.Duration
		err    error
	}
	// wait starts a long poll after the project's latest event and returns
	// once it is parked on the store's change channel.
	wait := func(limit time.Duration) chan waited {
		t.Helper()
		events, err := f.s.ListEvents(ctx, f.task.ID, 0, 500)
		if err != nil {
			t.Fatal(err)
		}
		after := int64(0)
		if len(events) > 0 {
			after = events[len(events)-1].Seq
		}
		done := make(chan waited, 1)
		go func() {
			start := time.Now()
			got, err := f.s.WaitEvents(ctx, f.task.ID, after, 10, limit)
			done <- waited{got, time.Since(start), err}
		}()
		for deadline := time.Now().Add(5 * time.Second); !waiting(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the event waiter never parked")
			}
		}
		return done
	}
	quiet := wait(500 * time.Millisecond)
	f.use(0, 1500)
	if !waiting() {
		t.Fatal("an upload that posted no warning woke the event waiter")
	}
	if got := <-quiet; got.err != nil || len(got.events) != 0 {
		t.Fatalf("quiet upload: %+v", got)
	}
	f.want("at the level", 0, 0, 0)

	woken := wait(30 * time.Second)
	f.use(0, 1)
	f.want("crossing", 1, 1, 1)
	select {
	case got := <-woken:
		if got.err != nil || len(got.events) == 0 || got.took > 10*time.Second {
			t.Fatalf("crossing upload: %d events after %s, %v", len(got.events), got.took, got.err)
		}
		messages := map[int64]bool{}
		for _, e := range got.events {
			if e.Kind == api.EventMessage {
				if seq, ok := e.Data["seq"].(float64); ok {
					messages[int64(seq)] = true
				}
			}
		}
		if lead, helper := f.notices(f.lead)[0].Seq, f.notices(f.helper)[0].Seq; !messages[lead] || !messages[helper] {
			t.Fatalf("message events %v do not name notices %d and %d", messages, lead, helper)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the crossing upload did not wake the event waiter")
	}
}

// The compared figure is the running entry's team's (wi_12d5c1ef9a73ceae):
// an item's earlier tokens are shown as its lifetime figure and never warn.
func TestUsageEntryWarningIgnoresEarlierTokens(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	f.setEstimate(0, 1000)
	f.as(f.handler, f.source, func() { f.use(0, 10000) })
	f.use(0, 1400)
	f.want("10000 earlier tokens and a team at 1400 of 1000", 0, 0, 0)
	if b := f.budget(); b.ActualTokens != "11400" || b.ActualState != "measured" || b.Ratio != "57/5" || b.Team == nil || *b.Team != (api.TeamTokenBudget{EntryID: f.entry, ActualTokens: "1400", ActualState: "measured", Ratio: "7/5"}) {
		t.Fatalf("budget %+v team %+v", b, b.Team)
	}
	f.use(0, 100)
	f.want("a team at 1500 of 1000 is at the level", 0, 0, 0)
	f.use(0, 1)
	f.want("a team at 1501 of 1000 is past 1.5", 1, 1, 1)
	for _, m := range []api.Message{f.notices(f.lead)[0], f.notices(f.helper)[0]} {
		e := m.Envelope
		if e.Refs["entry"] != f.entry || e.Refs["actualTokens"] != "1501" || e.Refs["actualState"] != "measured" || e.Refs["ratio"] != "1501/1000" || e.Refs["lifetimeTokens"] != "11501" || e.Refs["lifetimeState"] != "measured" {
			t.Fatalf("refs %v", e.Refs)
		}
		if !strings.Contains(e.Body.Text, ": the team of queue entry "+f.entry+" has used 1501 tokens on it against an estimate of 1000: 1.50 times, past the warning level of 1.5. The item's lifetime total is 11501 tokens, which includes work before this entry. ") {
			t.Fatalf("text %q", e.Body.Text)
		}
	}
	got, err := f.s.UsageWarnings(context.Background(), f.task.ID)
	if err != nil || len(got.Warnings) != 1 {
		t.Fatalf("warnings %+v %v", got, err)
	}
	if w := got.Warnings[0]; w.EntryID != f.entry || w.ActualTokens != "1501" || w.ActualState != "measured" || w.Ratio != "1501/1000" || w.LifetimeTokens != "11501" || w.LifetimeState != "measured" {
		t.Fatalf("warning %+v", w)
	}
	if b := f.budget(); b.ActualTokens != "11501" || b.Team == nil || b.Team.ActualTokens != "1501" || b.Team.Ratio != "1501/1000" {
		t.Fatalf("budget %+v team %+v", b, b.Team)
	}
}

// After admission, only turns of the entry's team count: not an agent without
// a binding, not a bound database handler, backlog steward or owner helper,
// and not an agent bound to the item before the entry.
func TestUsageEntryWarningCountsOnlyTheTeam(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	f.setEstimate(0, 1000)
	bound := f.admitted.Add(500 * time.Millisecond)
	unbound, unboundOrders := f.agent("unbound", "", true)
	f.as(unbound, unboundOrders, func() { f.use(0, 1_000_000) })
	f.want("an agent with no binding", 0, 0, 0)
	f.bind(f.handler, bound, "")
	f.as(f.handler, f.source, func() { f.use(0, 1_000_000) })
	f.want("a bound database handler", 0, 0, 0)
	steward, stewardOrders := f.agent("steward", api.AgentRoleBacklogSteward, true)
	f.bind(steward, bound, "member")
	f.as(steward, stewardOrders, func() { f.use(0, 1_000_000) })
	f.want("a bound backlog steward", 0, 0, 0)
	f.bind(f.helper, bound, "member")
	f.as(f.helper, f.order(f.helper), func() { f.use(0, 1_000_000) })
	f.want("the bound owner helper", 0, 0, 0)
	for _, a := range []api.Agent{f.handler, steward, f.helper} {
		if got := f.count(`SELECT count(*) FROM agents WHERE id=? AND role<>''`, a.ID); got != 1 {
			t.Fatalf("fixture: %s has no project role", a.Name)
		}
	}
	// Bound in the entry's own second, half a second before it.
	earlier, earlierOrders := f.agent("earlier", "", true)
	f.bind(earlier, f.admitted.Add(-500*time.Millisecond), "member")
	f.as(earlier, earlierOrders, func() { f.use(0, 1_000_000) })
	f.want("an agent bound before the entry", 0, 0, 0)
	if b := f.budget(); b.ActualTokens != "5000000" || b.Team == nil || b.Team.EntryID != f.entry || b.Team.ActualTokens != "0" || b.Team.ActualState != "not measured" || b.Team.Ratio != "" {
		t.Fatalf("budget %+v team %+v", b, b.Team)
	}
	// A second member bound at the entry's own instant counts with the first.
	extra, extraOrders := f.agent("extra", "", true)
	f.bind(extra, f.admitted, "member")
	f.as(extra, extraOrders, func() { f.use(0, 800) })
	f.use(0, 700)
	f.want("two members at 1500 of 1000", 0, 0, 0)
	f.use(0, 1)
	f.want("the team crosses", 1, 1, 1)
	if refs := f.notices(f.lead)[0].Envelope.Refs; refs["actualTokens"] != "1501" || refs["lifetimeTokens"] != "5001501" {
		t.Fatalf("refs %v", refs)
	}
}

// A new entry on the same item and estimate warns on its own team's tokens,
// once, whatever the earlier entry's team spent and whether it warned.
func TestUsageEntryWarningSecondEntry(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	ctx := context.Background()
	f.setEstimate(0, 1000)
	f.use(0, 1600)
	f.want("the first entry crosses", 1, 1, 1)
	first, firstLead, firstMember, firstOrders := f.entry, f.lead, f.metered, f.orders
	f.release()
	f.use(0, 1000)
	f.want("no entry is running", 1, 1, 1)
	// Bound on a whole second, half a second before the second entry: as
	// text that instant sorts after the entry's.
	between, betweenOrders := f.agent("between", "", true)
	f.bind(between, f.admitted.Add(2*time.Second), "member")
	f.addEntry()
	f.addLead()
	if f.entry == first || f.lead.ID == firstLead.ID || f.metered.ID == firstMember.ID {
		t.Fatal("fixture: the second entry reuses the first entry's team")
	}
	f.as(firstMember, firstOrders, func() { f.use(0, 5000) })
	f.as(between, betweenOrders, func() { f.use(0, 5000) })
	f.use(0, 1500)
	f.want("the second team is at the level", 0, 1, 1)
	if b := f.budget(); b.ActualTokens != "14100" || b.Team == nil || b.Team.EntryID != f.entry || b.Team.ActualTokens != "1500" {
		t.Fatalf("budget %+v team %+v", b, b.Team)
	}
	f.use(0, 1)
	f.want("the second team crosses", 1, 2, 2)
	if got := len(f.notices(firstLead)); got != 1 {
		t.Fatalf("the first entry's lead has %d notices, want 1", got)
	}
	for _, m := range []api.Message{f.notices(f.lead)[0], f.notices(f.helper)[1]} {
		if refs := m.Envelope.Refs; refs["entry"] != f.entry || refs["estimateTokens"] != "1000" || refs["actualTokens"] != "1501" || refs["lifetimeTokens"] != "14101" || refs["lead"] != f.lead.Name {
			t.Fatalf("second entry refs %v", refs)
		}
	}
	crossing := f.use(0, 400)
	f.replay(crossing)
	f.as(firstMember, firstOrders, func() { f.use(0, 5000) })
	f.want("once for the second entry", 1, 2, 2)
	for _, entry := range []string{first, f.entry} {
		if got := f.count(`SELECT count(*) FROM usage_entry_warnings WHERE task_id=? AND item_id=? AND entry_id=? AND estimate_tokens=1000`, f.task.ID, f.items[0].ID, entry); got != 1 {
			t.Fatalf("%d rows for entry %s, want 1", got, entry)
		}
	}
	got, err := f.s.UsageWarnings(ctx, f.task.ID)
	if err != nil || len(got.Warnings) != 2 || got.Warnings[0].EntryID != first || got.Warnings[0].ActualTokens != "1600" || got.Warnings[0].LifetimeTokens != "1600" || got.Warnings[1].EntryID != f.entry || got.Warnings[1].ActualTokens != "1501" || got.Warnings[1].LifetimeTokens != "14101" {
		t.Fatalf("warnings %+v %v", got, err)
	}
	// A raised estimate re-arms the second entry only.
	f.setEstimate(0, 2000)
	f.use(0, 1100)
	f.want("3001 of 2000 for the second team", 2, 3, 3)
}

// A warning recorded before entries were compared stays where it is and is
// listed with the new ones (wi_12d5c1ef9a73ceae, D6).
func TestUsageEntryWarningKeepsEarlierRows(t *testing.T) {
	f := newUsageWarningFixture(t, true, true)
	ctx := context.Background()
	row := func() string {
		t.Helper()
		var out string
		if err := f.s.db.QueryRow(`SELECT group_concat(task_id||'|'||item_id||'|'||estimate_tokens||'|'||threshold||'|'||actual_tokens||'|'||actual_state||'|'||lead_agent||'|'||lead_message_seq||'|'||helper_agent||'|'||helper_message_seq||'|'||created_at,';') FROM usage_budget_warnings`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// The same item and estimate value as the entry's warning below.
	if _, err := f.s.db.Exec(`INSERT INTO usage_budget_warnings(task_id,item_id,estimate_tokens,threshold,actual_tokens,actual_state,lead_agent,lead_message_seq,helper_agent,helper_message_seq,created_at) VALUES(?,?,1000,'1.5','28700000','partial','',0,?,7,?)`,
		f.task.ID, f.items[0].ID, f.helper.ID, ts(f.admitted.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	before := row()
	var seq int
	var name, path string
	if err := f.s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil || path == "" {
		t.Fatal("database path", path, err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal("a database with an earlier warning row did not open:", err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.s = reopened
	if after := row(); after != before {
		t.Fatalf("the earlier row changed on open:\n%s\nwas\n%s", after, before)
	}
	f.setEstimate(0, 1000)
	f.use(0, 1501)
	f.want("the entry warns although the item and estimate have an earlier row", 1, 1, 1)
	if after := row(); after != before {
		t.Fatalf("the earlier row changed on a warning:\n%s\nwas\n%s", after, before)
	}
	got, err := f.s.UsageWarnings(ctx, f.task.ID)
	if err != nil || len(got.Warnings) != 2 {
		t.Fatalf("warnings %+v %v", got, err)
	}
	old, entry := got.Warnings[0], got.Warnings[1]
	if old.EntryID != "" || old.LifetimeTokens != "" || old.LifetimeState != "" || old.ItemID != f.items[0].ID || old.EstimateTokens != 1000 || old.ActualTokens != "28700000" || old.ActualState != "partial" || old.Ratio != "28700" || old.HelperAgent != f.helper.ID || old.HelperMessageSeq != 7 || !old.At.Equal(f.admitted.Add(-time.Hour)) {
		t.Fatalf("earlier warning %+v", old)
	}
	if entry.EntryID != f.entry || entry.ActualTokens != "1501" || entry.LifetimeTokens != "1501" || entry.LifetimeState != "measured" {
		t.Fatalf("entry warning %+v", entry)
	}
}

// usageTurnFor is a complete synthetic request of exactly tokens, attributed
// to the item of an order by an acknowledgement of it.
func usageTurnFor(id string, order api.Message, tokens int64) api.UsageTurn {
	turn := syntheticUsageTurn(id)
	turn.Tokens = map[string]int64{"input": tokens, "cached": 0, "cacheWrite": 0, "output": 0, "reasoning": 0}
	turn.Raw = map[string]int64{"input_tokens": tokens, "cached_input_tokens": 0, "cache_write_input_tokens": 0, "output_tokens": 0, "reasoning_output_tokens": 0}
	turn.Handled = []api.UsageEvidence{{TaskID: order.TaskID, Seq: order.Seq, Operation: "ack", At: turn.At}}
	return turn
}

// The team of a running entry is counted from its admission, the claim, not
// from its enqueue. The entry, its admission, the bindings and the lead all
// come from the store's own queue and agent calls: no row is written by hand.
func TestUsageTeamCountsFromAdmission(t *testing.T) {
	f := newRebindFixture(t, false)
	clock := f.s.now().UTC().Truncate(time.Second).Add(time.Second)
	f.s.now = func() time.Time { return clock }
	// Bound while the entry waits in the queue.
	waiter := f.member(t, "waiter")
	clock = clock.Add(2 * time.Second)
	f.entry = f.action(t, api.TeamQueueRequest{Operation: "claim", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision, Host: "mini"})
	if f.entry.AdmittedAt != ts(clock) {
		t.Fatalf("claim admitted at %q, want %q", f.entry.AdmittedAt, ts(clock))
	}
	clock = clock.Add(2 * time.Second)
	lead, member := f.member(t, "lead"), f.member(t, "member")
	plan, _ := json.Marshal(map[string]any{"task": f.task.ID, "item": f.item.ID, "revision": f.item.Revision, "order": f.order.Seq, "context": map[string]any{"version": 1}, "members": []any{map[string]any{"state": "unstarted", "runId": lead.RunID, "fields": map[string]any{"agentId": lead.ID, "name": lead.Name, "cwd": "/worktrees/rebind"}}}})
	for _, step := range []api.TeamQueueRequest{{Operation: "freeze", LaunchJSON: plan}, {Operation: "attempt"}, {Operation: "started", MemberRunID: lead.RunID}, {Operation: "running"}} {
		step.EntryID, step.ExpectedRevision = f.entry.ID, f.entry.Revision
		f.entry = f.action(t, step)
	}
	if f.entry.State != "running" || f.entry.AdmittedAt != ts(clock.Add(-2*time.Second)) {
		t.Fatalf("running entry %s admitted at %q", f.entry.State, f.entry.AdmittedAt)
	}
	if _, _, err := f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, estimateRequest(f.item.Revision, "admission-estimate", 1000, "synthetic", api.Agent{}), f.by); err != nil {
		t.Fatal(err)
	}
	team := func(why, tokens, state string) {
		t.Helper()
		item, err := f.s.GetWorkItem(f.ctx, f.task.ID, f.item.ID)
		if err != nil || item.Budget == nil || item.Budget.Team == nil {
			t.Fatalf("%s: budget %+v %v", why, item.Budget, err)
		}
		if got := item.Budget.Team; got.EntryID != f.entry.ID || got.ActualTokens != tokens || got.ActualState != state {
			t.Fatalf("%s: team %+v, want %s %s", why, got, tokens, state)
		}
	}
	upload := func(a api.Agent, key string, tokens int64) {
		t.Helper()
		batch := usageBatch(a, key, usageTurnFor(key, f.order, tokens))
		if receipt, err := f.s.ReportUsage(f.ctx, f.task.ID, a.ID, batch); err != nil || receipt.Turns != 1 {
			t.Fatalf("upload %s: %+v %v", key, receipt, err)
		}
	}
	upload(waiter, "waiter-turn", 1_000_000)
	team("an agent bound between enqueue and admission", "0", "not measured")
	if n := queueBudgetHoldRows(t, f.s, f.task.ID); n != 0 {
		t.Fatalf("an agent bound before admission caused %d holds", n)
	}
	upload(member, "member-turn", 700)
	team("a member bound after admission", "700", "measured")
	upload(lead, "lead-turn", 300)
	team("the lead bound after admission", "1000", "measured")
}

// budgetTurn is a complete synthetic request of one runtime at an instant.
func budgetTurn(id, runtime string, at time.Time, tokens int64) api.UsageTurn {
	turn := syntheticUsageTurn(id)
	turn.Runtime, turn.At = runtime, at
	turn.Tokens = map[string]int64{"input": tokens, "cached": 0, "cacheWrite": 0, "output": 0, "reasoning": 0}
	turn.Raw = map[string]int64{"input_tokens": tokens, "cached_input_tokens": 0, "cache_write_input_tokens": 0, "output_tokens": 0, "reasoning_output_tokens": 0}
	if runtime == "claude" {
		turn.Raw = map[string]int64{"input_tokens": tokens, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 0, "output_tokens_details.thinking_tokens": 0}
	}
	return turn
}

// budgetStatus is the one budget row's status for a host.
func budgetStatus(t *testing.T, s *Store, task, host, runtime, window string) api.UsageBudgetStatus {
	t.Helper()
	out, err := s.UsageBudgets(context.Background(), task, host)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range out.Budgets {
		if b.Runtime == runtime && b.Window == window && b.Status != nil {
			return *b.Status
		}
	}
	t.Fatalf("no %s %s budget in %+v", runtime, window, out)
	return api.UsageBudgetStatus{}
}

// a1, a5: budget rows round-trip, refuse bad values and unauthorised agents,
// and a project without one is not configured.
func TestUsageBudgetSettings(t *testing.T) {
	s, task, handler, _, _ := usageFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	out, err := s.UsageBudgets(ctx, task.ID, "mini")
	if err != nil || out.Configured || len(out.Budgets) != 0 {
		t.Fatalf("a project with no budget row: %+v %v", out, err)
	}
	clock := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	reset := "2026-10-08T10:00:00Z"
	out, err = s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 200_000_000, ReservePercent: 10, ResetAt: reset, StaleSeconds: 600}, by)
	if err != nil || !out.Configured || len(out.Budgets) != 1 {
		t.Fatalf("set %+v %v", out, err)
	}
	want := api.UsageBudget{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 200_000_000, ReservePercent: 10, ResetAt: reset, StaleSeconds: 600, UpdatedAt: ts(clock), UpdatedBy: api.Sender{Node: "fixture", User: "owner"}}
	got := out.Budgets[0]
	got.Status = nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("saved row %+v, want %+v", got, want)
	}
	// A second window, with the default staleness bound, and an update in place.
	if _, err = s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowSevenDay, AllowanceTokens: 900_000_000}, by); err != nil {
		t.Fatal(err)
	}
	out, err = s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 250_000_000, ReservePercent: 20, AgentID: handler.ID}, by)
	if err != nil || len(out.Budgets) != 2 || out.Budgets[0].AllowanceTokens != 250_000_000 || out.Budgets[0].ReservePercent != 20 || out.Budgets[0].ResetAt != "" || out.Budgets[0].StaleSeconds != api.DefaultUsageBudgetStaleSeconds || out.Budgets[0].UpdatedBy.AgentID != handler.ID || out.Budgets[1].StaleSeconds != api.DefaultUsageBudgetStaleSeconds {
		t.Fatalf("update by a database handler %+v %v", out, err)
	}
	helper := *registerHelper(t, s, task.ID, helperRequest("budget-helper"), by).Agent
	if _, err = s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "codex", Window: api.UsageWindowFiveHour, AllowanceTokens: 1, AgentID: helper.ID}, by); err != nil {
		t.Fatalf("set by the owner helper: %v", err)
	}
	other, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "builder", AgentID: api.NewID("agt"), Host: "fixture", Session: "builder"}, by)
	if err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]api.UsageBudgetRequest{
		"missing allowance": {Runtime: "claude", Window: api.UsageWindowFiveHour},
		"zero allowance":    {Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 0, ReservePercent: 10},
		"unknown window":    {Runtime: "claude", Window: "daily", AllowanceTokens: 5},
		"unknown runtime":   {Runtime: "gemini", Window: api.UsageWindowFiveHour, AllowanceTokens: 5},
		"reserve too high":  {Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 5, ReservePercent: 91},
		"bad reset time":    {Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 5, ResetAt: "tomorrow"},
	} {
		if _, err := s.SetUsageBudget(ctx, task.ID, req, by); !errors.Is(err, api.ErrInvalid) || !strings.Contains(err.Error(), "budget") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err = s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 5, AgentID: other.ID}, by); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "only the owner, the owner helper or a database handler sets the token budget") {
		t.Fatalf("an ordinary agent: %v", err)
	}
	if _, err = s.DeleteUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AgentID: other.ID}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("clear by an ordinary agent: %v", err)
	}
	if _, err = s.SetUsageBudget(ctx, "tsk_0000000000000000", api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 5}, by); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	// The staleness bound has a floor the relay can keep fresh: a refused bound
	// names both figures and writes no row.
	budgetRows := func() string {
		return tableDump(t, s, `SELECT runtime,window,allowance_tokens,stale_seconds,updated_at FROM usage_budgets WHERE task_id=? ORDER BY runtime,window`, task.ID)
	}
	rowsBefore := budgetRows()
	for _, stale := range []int{1, 299, 300, 359} {
		_, err := s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "codex", Window: api.UsageWindowSevenDay, AllowanceTokens: 5, StaleSeconds: stale}, by)
		if !errors.Is(err, api.ErrInvalid) || !strings.Contains(err.Error(), "6 minutes") || !strings.Contains(err.Error(), "5 minutes") {
			t.Fatalf("staleness bound of %d seconds: %v", stale, err)
		}
	}
	if _, err = s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "codex", Window: api.UsageWindowSevenDay, AllowanceTokens: 5, StaleSeconds: 86401}, by); !errors.Is(err, api.ErrInvalid) || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("staleness bound over 24 hours: %v", err)
	}
	if got := budgetRows(); got != rowsBefore {
		t.Fatalf("a refused staleness bound wrote\n%s\nwas\n%s", got, rowsBefore)
	}
	for stale, want := range map[int]int{360: 360, 86400: 86400, 0: api.DefaultUsageBudgetStaleSeconds} {
		out, err = s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "codex", Window: api.UsageWindowSevenDay, AllowanceTokens: 5, StaleSeconds: stale}, by)
		if err != nil {
			t.Fatalf("staleness bound of %d seconds: %v", stale, err)
		}
		saved := -1
		for _, b := range out.Budgets {
			if b.Runtime == "codex" && b.Window == api.UsageWindowSevenDay {
				saved = b.StaleSeconds
			}
		}
		if saved != want {
			t.Fatalf("staleness bound of %d seconds saved %d, want %d", stale, saved, want)
		}
	}
	if api.DefaultUsageBudgetStaleSeconds != 900 {
		t.Fatalf("default staleness bound %d, want 900", api.DefaultUsageBudgetStaleSeconds)
	}
	if _, err = s.DeleteUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "codex", Window: api.UsageWindowSevenDay}, by); err != nil {
		t.Fatal(err)
	}
	for _, row := range [][2]string{{"claude", api.UsageWindowFiveHour}, {"claude", api.UsageWindowSevenDay}, {"codex", api.UsageWindowFiveHour}} {
		if out, err = s.DeleteUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: row[0], Window: row[1]}, by); err != nil {
			t.Fatalf("clear %v: %v", row, err)
		}
	}
	if out.Configured || len(out.Budgets) != 0 {
		t.Fatalf("after clearing every row: %+v", out)
	}
	if _, err = s.DeleteUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour}, by); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("clear a row that is not set: %v", err)
	}
}

// a1: with no reading, remaining is the allowance minus the project's tokens
// of that runtime since the last owner-entered reset. Earlier turns and other
// runtimes are not counted, and the reset rolls forward by whole windows.
func TestUsageBudgetAllowanceRemaining(t *testing.T) {
	s, task, agent, _, _ := usageFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	reset := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	clock := reset.Add(2 * time.Hour)
	s.now = func() time.Time { return clock }
	if _, err := s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "codex", Window: api.UsageWindowFiveHour, AllowanceTokens: 1000, ReservePercent: 10, ResetAt: ts(reset)}, by); err != nil {
		t.Fatal(err)
	}
	status := func() api.UsageBudgetStatus {
		return budgetStatus(t, s, task.ID, "mini", "codex", api.UsageWindowFiveHour)
	}
	if got := status(); got.Source != api.UsageBudgetSourceAllowance || got.RemainingTokens != "1000" || got.ResetAt != ts(reset.Add(5*time.Hour)) || got.Reading != "is not reported" || got.Host != "mini" {
		t.Fatalf("no usage: %+v", got)
	}
	batch := usageBatch(agent, "budget-batch",
		budgetTurn("before-reset", "codex", reset.Add(-time.Second), 400),
		// In the reset's own second, half a second before and at it.
		budgetTurn("just-before", "codex", reset.Add(time.Second).Add(-500*time.Millisecond), 30),
		budgetTurn("at-reset", "codex", reset.Add(time.Second), 70),
		budgetTurn("after-reset", "codex", reset.Add(time.Hour), 200),
		budgetTurn("other-runtime", "claude", reset.Add(time.Hour), 5000))
	if _, err := s.ReportUsage(ctx, task.ID, agent.ID, batch); err != nil {
		t.Fatal(err)
	}
	// Move the reset one second on, so the two turns in that second straddle it.
	if _, err := s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "codex", Window: api.UsageWindowFiveHour, AllowanceTokens: 1000, ReservePercent: 10, ResetAt: ts(reset.Add(time.Second))}, by); err != nil {
		t.Fatal(err)
	}
	if got := status(); got.RemainingTokens != "730" {
		t.Fatalf("since the reset, codex only: %+v", got)
	}
	// One window later the same row starts again from its second reset.
	clock = reset.Add(6 * time.Hour)
	if got := status(); got.RemainingTokens != "1000" || got.ResetAt != ts(reset.Add(time.Second).Add(10*time.Hour)) {
		t.Fatalf("after the next reset: %+v", got)
	}
	// A reset entered for the future counts back whole windows: the window
	// that ends at it holds every turn but the one at the reset itself.
	clock = reset.Add(-2 * time.Hour)
	if got := status(); got.ResetAt != ts(reset.Add(time.Second)) || got.RemainingTokens != "300" {
		t.Fatalf("before the entered reset: %+v", got)
	}
}

// usageTurnTotals is every totals row of the project.
func usageTurnTotals(t *testing.T, s *Store, task string) string {
	t.Helper()
	return tableDump(t, s, `SELECT agent_id,run_id,request_id,turn_revision,runtime,at_ns,tokens FROM usage_turn_totals WHERE task_id=? ORDER BY agent_id,run_id,request_id`, task)
}

// s1: the allowance read uses usage_turn_totals alone, through its covering
// index. With every stored projection of the project made unreadable, the
// remaining figure and an admission reason do not change.
func TestUsageBudgetAllowanceReadsTotalsOnly(t *testing.T) {
	s, task, agent, items, _ := usageFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	reset := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	clock := reset.Add(2 * time.Hour)
	s.now = func() time.Time { return clock }
	if _, err := s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "codex", Window: api.UsageWindowFiveHour, AllowanceTokens: 1000, ResetAt: ts(reset)}, by); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateWorkItemUpdate(ctx, task.ID, items[0].ID, estimateRequest(items[0].Revision, "totals-only-estimate", 900, "synthetic", api.Agent{}), by); err != nil {
		t.Fatal(err)
	}
	batch := usageBatch(agent, "totals-only",
		budgetTurn("before-reset", "codex", reset.Add(-time.Minute), 400),
		budgetTurn("first", "codex", reset.Add(time.Minute), 120),
		budgetTurn("second", "codex", reset.Add(time.Hour), 80),
		budgetTurn("other-runtime", "claude", reset.Add(time.Hour), 5000))
	if _, err := s.ReportUsage(ctx, task.ID, agent.ID, batch); err != nil {
		t.Fatal(err)
	}
	read := func() (string, string) {
		t.Helper()
		reason, err := queueBudgetAdmission(ctx, s.db, task.ID, api.TeamQueueEntry{TaskID: task.ID, ItemID: items[0].ID, Host: "mini"}, clock)
		if err != nil {
			t.Fatal(err)
		}
		return budgetStatus(t, s, task.ID, "mini", "codex", api.UsageWindowFiveHour).RemainingTokens, reason
	}
	remaining, reason := read()
	if remaining != "800" || !strings.Contains(reason, "needs about 900 tokens; 800 remain") {
		t.Fatalf("before: remaining %s reason %q", remaining, reason)
	}
	result, err := s.db.Exec(`UPDATE usage_turns SET projection='not json' WHERE task_id=?`, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := result.RowsAffected(); n != 4 {
		t.Fatalf("overwrote %d projections, want 4", n)
	}
	if gotRemaining, gotReason := read(); gotRemaining != remaining || gotReason != reason {
		t.Fatalf("with unreadable projections: remaining %s reason %q, want %s %q", gotRemaining, gotReason, remaining, reason)
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+usageRuntimeTokensSinceSQL, task.ID, "codex", reset.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "USING COVERING INDEX usage_turn_totals_window") || strings.Contains(plan, "usage_turns") {
		t.Fatalf("allowance read plan:\n%s", plan)
	}
}

// s1: a turn counts once at its current revision. A revision with more tokens
// moves the remaining figure by exactly the difference, and a replay of either
// upload changes neither the figure nor the totals rows.
func TestUsageTurnTotalsFollowRevisions(t *testing.T) {
	s, task, agent, _, _ := usageFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	reset := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	clock := reset.Add(2 * time.Hour)
	s.now = func() time.Time { return clock }
	if _, err := s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "codex", Window: api.UsageWindowFiveHour, AllowanceTokens: 1000, ResetAt: ts(reset)}, by); err != nil {
		t.Fatal(err)
	}
	remaining := func() string {
		return budgetStatus(t, s, task.ID, "mini", "codex", api.UsageWindowFiveHour).RemainingTokens
	}
	upload := func(why string, batch api.UsageBatch) {
		t.Helper()
		if _, err := s.ReportUsage(ctx, task.ID, agent.ID, batch); err != nil {
			t.Fatalf("%s: %v", why, err)
		}
	}
	one := budgetTurn("revised", "codex", reset.Add(time.Hour), 100)
	first := usageBatch(agent, "revision-one", one, budgetTurn("steady", "codex", reset.Add(time.Hour), 50))
	upload("revision 1", first)
	if got := remaining(); got != "850" {
		t.Fatalf("after revision 1: %s", got)
	}
	two := budgetTurn("revised", "codex", reset.Add(time.Hour), 160)
	two.Revision = 2
	second := usageBatch(agent, "revision-two", two)
	upload("revision 2", second)
	if got := remaining(); got != "790" {
		t.Fatalf("after revision 2 with 60 more tokens: %s", got)
	}
	totals := usageTurnTotals(t, s, task.ID)
	if strings.Count(totals, "\n") != 2 {
		t.Fatalf("totals rows:\n%s", totals)
	}
	upload("replay of revision 1", first)
	upload("replay of revision 2", second)
	// The same turn under a new upload key is unchanged and writes nothing.
	upload("revision 2 in a new upload", usageBatch(agent, "revision-two-again", two))
	if got := remaining(); got != "790" {
		t.Fatalf("after replays: %s", got)
	}
	if got := usageTurnTotals(t, s, task.ID); got != totals {
		t.Fatalf("replays changed the totals rows\n%s\nwas\n%s", got, totals)
	}
}

// s1: turns stored before the totals table existed are counted after the
// upgrade. With every totals row gone, one left at an older revision with a
// wrong figure and one projection unreadable, the backfill restores the
// figure less only the unreadable turn, and a second pass changes nothing.
func TestUsageTurnTotalsBackfill(t *testing.T) {
	s, task, agent, _, _ := usageFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	reset := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	clock := reset.Add(2 * time.Hour)
	s.now = func() time.Time { return clock }
	if _, err := s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "codex", Window: api.UsageWindowFiveHour, AllowanceTokens: 10_000_000, ResetAt: ts(reset)}, by); err != nil {
		t.Fatal(err)
	}
	const turns = 1200
	for start := 0; start < turns; start += 60 {
		batch := []api.UsageTurn{}
		for i := start; i < start+60; i++ {
			batch = append(batch, budgetTurn(fmt.Sprintf("turn-%04d", i), "codex", reset.Add(time.Duration(i+1)*time.Second), int64(i+1)))
		}
		if _, err := s.ReportUsage(ctx, task.ID, agent.ID, usageBatch(agent, fmt.Sprintf("backfill-%d", start), batch...)); err != nil {
			t.Fatal(err)
		}
	}
	remaining := func() int64 {
		t.Helper()
		var n int64
		if _, err := fmt.Sscan(budgetStatus(t, s, task.ID, "mini", "codex", api.UsageWindowFiveHour).RemainingTokens, &n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := remaining()
	if want := int64(10_000_000 - turns*(turns+1)/2); before != want {
		t.Fatalf("remaining after %d uploads %d, want %d", turns, before, want)
	}
	// turn-0006 (7 tokens) keeps a row from an older revision with a wrong
	// figure; turn-0700 (701 tokens) loses its projection.
	for _, statement := range []string{
		`DELETE FROM usage_turn_totals WHERE task_id=? AND request_id<>'turn-0006'`,
		`UPDATE usage_turn_totals SET turn_revision=0,tokens=999999 WHERE task_id=?`,
		`UPDATE usage_turns SET projection='not json' WHERE task_id=? AND request_id='turn-0700'`,
	} {
		if _, err := s.db.Exec(statement, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := remaining(); got != 10_000_000-999999 {
		t.Fatalf("remaining with the totals removed: %d", got)
	}
	if err := backfillUsageTurnTotals(s.db); err != nil {
		t.Fatal(err)
	}
	if got := remaining(); got != before+701 {
		t.Fatalf("remaining after the backfill %d, want %d", got, before+701)
	}
	totals := usageTurnTotals(t, s, task.ID)
	if n := strings.Count(totals, "\n"); n != turns-1 || strings.Contains(totals, "turn-0700") {
		t.Fatalf("%d totals rows after the backfill, want %d without the unreadable turn", n, turns-1)
	}
	if err := backfillUsageTurnTotals(s.db); err != nil {
		t.Fatal(err)
	}
	if got := usageTurnTotals(t, s, task.ID); got != totals {
		t.Fatal("a second backfill changed the totals rows")
	}
}

// s1, s4: the backfill runs at every open. On a file-backed store with 60,000
// stored turns and no totals row the first open fills them within its bound,
// and the next open, with nothing to fill, is fast and changes nothing.
func TestUsageTurnTotalsBackfillOpenTime(t *testing.T) {
	if raceBuilt() {
		t.Skip("open time bounds are not checked in race runs; TestUsageTurnTotalsBackfill covers the backfill there")
	}
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			s.Close()
		}
	}()
	const turns = 60000
	task, agent, run := "tsk_00000000000000aa", "agt_00000000000000aa", "run_00000000000000aa"
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	insert, err := tx.Prepare(`INSERT INTO usage_turns VALUES(?,?,?,?,1,?,'',?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < turns; i++ {
		at := now.Add(-time.Duration(i) * 10 * time.Second)
		projection := fmt.Sprintf(`{"turn":{"id":"t%d","revision":1,"runtime":"codex","session":"s","model":"m","at":%q,"tokens":{"input":100,"cached":900,"cacheWrite":0,"output":40,"reasoning":10},"raw":{"input_tokens":1000,"cached_input_tokens":900,"output_tokens":50,"reasoning_output_tokens":10},"sourceDigest":%q,"activation":"a","complete":true},"agentId":%q,"runId":%q,"role":"builder","roleSource":"name","phase":"build","phaseReason":"synthetic","shares":[{"taskId":%q,"denominator":1,"reason":"synthetic"}]}`,
			i, at.Format(time.RFC3339Nano), strings.Repeat("a", 64), agent, run, task)
		if _, err := insert.Exec(task, agent, run, fmt.Sprintf("t%d", i), ts(at), projection); err != nil {
			t.Fatal(err)
		}
	}
	insert.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var none int
	if err := s.db.QueryRow(`SELECT count(*) FROM usage_turn_totals`).Scan(&none); err != nil || none != 0 {
		t.Fatalf("%d totals rows before the first open %v", none, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopen := func() time.Duration {
		t.Helper()
		started := time.Now()
		if s, err = Open(path); err != nil {
			t.Fatal(err)
		}
		return time.Since(started)
	}
	first := reopen()
	var count, tokens, oldest, newest int64
	if err := s.db.QueryRow(`SELECT count(*),sum(tokens),min(at_ns),max(at_ns) FROM usage_turn_totals WHERE task_id=? AND runtime='codex'`, task).Scan(&count, &tokens, &oldest, &newest); err != nil {
		t.Fatal(err)
	}
	if count != turns || tokens != turns*1050 || oldest != now.Add(-time.Duration(turns-1)*10*time.Second).UnixNano() || newest != now.UnixNano() {
		t.Fatalf("totals after the first open: %d rows, %d tokens, from %d to %d", count, tokens, oldest, newest)
	}
	totals := usageTurnTotals(t, s, task)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	second := reopen()
	same := usageTurnTotals(t, s, task) == totals
	closed = true
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("open with %d turns to backfill took %s; the next open took %s", turns, first, second)
	if !same {
		t.Fatal("the second open changed the totals rows")
	}
	if first >= 30*time.Second {
		t.Fatalf("the first open with %d turns to backfill took %s, want under 30s", turns, first)
	}
	if second >= 3*time.Second {
		t.Fatalf("the second open with nothing to backfill took %s, want under 3s", second)
	}
}

// a6, a7 hub half: a fresh reading decides by percentage; a stale one and one
// whose reset has passed fall to the allowance minus usage, never zero use;
// an invalidation leaves no figure of the old reading behind.
func TestUsageBudgetProviderSources(t *testing.T) {
	s, task, agent, _, _ := usageFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	base := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	clock := base
	s.now = func() time.Time { return clock }
	if _, err := s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 1000, StaleSeconds: 600}, by); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowSevenDay, AllowanceTokens: 9000, StaleSeconds: 600}, by); err != nil {
		t.Fatal(err)
	}
	five := func(host string) api.UsageBudgetStatus {
		return budgetStatus(t, s, task.ID, host, "claude", api.UsageWindowFiveHour)
	}
	if got := five("mini"); got.Source != api.UsageBudgetSourceNone || got.RemainingTokens != "" || got.Reading != "is not reported" {
		t.Fatalf("no reading and no reset: %+v", got)
	}
	resets := base.Add(3 * time.Hour)
	report := api.ProviderUsageReport{Host: "mini", Runtime: "claude", State: api.ProviderUsageOK, CapturedAt: ts(base), Version: "2.1.292", Windows: []api.ProviderUsageWindow{
		{Window: api.UsageWindowFiveHour, UsedPercent: 13, ResetsAt: ts(resets)},
		{Window: api.UsageWindowSevenDay, UsedPercent: 37.5, ResetsAt: ts(base.Add(100 * time.Hour))}}}
	stored, err := s.ReportProviderUsage(ctx, report)
	if err != nil || len(stored.Readings) != 2 || stored.Readings[0].UsedPercent != "13" || stored.Readings[1].UsedPercent != "37.5" || stored.Readings[0].Version != "2.1.292" || stored.Readings[0].ReportedAt != ts(base) {
		t.Fatalf("stored reading %+v %v", stored, err)
	}
	if got := five("mini"); got.Source != api.UsageBudgetSourceProvider || got.RemainingTokens != "870" || got.ResetAt != ts(resets) || got.Reading != "" {
		t.Fatalf("fresh reading: %+v", got)
	}
	if got := budgetStatus(t, s, task.ID, "mini", "claude", api.UsageWindowSevenDay); got.Source != api.UsageBudgetSourceProvider || got.RemainingTokens != "5625" {
		t.Fatalf("fresh seven day reading: %+v", got)
	}
	// Another host has reported nothing.
	if got := five("air"); got.Source != api.UsageBudgetSourceNone {
		t.Fatalf("a host with no reading: %+v", got)
	}
	// Usage before and inside the reading's window (it began two hours ago),
	// and after its reset.
	batch := usageBatch(agent, "provider-batch",
		budgetTurn("before-window", "claude", base.Add(-3*time.Hour), 500),
		budgetTurn("in-window", "claude", base.Add(-time.Hour), 120),
		budgetTurn("after-old-reset", "claude", resets.Add(10*time.Minute), 45),
		budgetTurn("codex-turn", "codex", base.Add(-time.Hour), 9999))
	if _, err := s.ReportUsage(ctx, task.ID, agent.ID, batch); err != nil {
		t.Fatal(err)
	}
	// At the staleness bound the reading still decides; one second past it the
	// percentage is not used.
	clock = base.Add(600 * time.Second)
	if got := five("mini"); got.Source != api.UsageBudgetSourceProvider || got.RemainingTokens != "870" {
		t.Fatalf("at the staleness bound: %+v", got)
	}
	clock = base.Add(601 * time.Second)
	if got := five("mini"); got.Source != api.UsageBudgetSourceAllowance || got.RemainingTokens != "835" || got.ResetAt != ts(resets) || !strings.Contains(got.Reading, "is stale") {
		t.Fatalf("stale reading: %+v", got)
	}
	// The reading's reset has passed: usage since that reset, never zero use.
	clock = resets.Add(time.Hour)
	if got := five("mini"); got.Source != api.UsageBudgetSourceAllowance || got.RemainingTokens != "955" || got.ResetAt != ts(resets.Add(5*time.Hour)) || !strings.Contains(got.Reading, "reset that passed") {
		t.Fatalf("passed reset: %+v", got)
	}
	// Long after, the window is the last five hours.
	clock = resets.Add(9 * time.Hour)
	if got := five("mini"); got.Source != api.UsageBudgetSourceAllowance || got.RemainingTokens != "1000" || got.ResetAt != ts(clock) {
		t.Fatalf("long passed reset: %+v", got)
	}
	// Invalidation: no figure of the old reading remains, in either window.
	clock = base.Add(time.Minute)
	for _, state := range []string{api.ProviderUsageMissing, api.ProviderUsageUnreadable, api.ProviderUsageMalformed} {
		if _, err := s.ReportProviderUsage(ctx, report); err != nil {
			t.Fatal(err)
		}
		stored, err := s.ReportProviderUsage(ctx, api.ProviderUsageReport{Host: "mini", Runtime: "claude", State: state})
		if err != nil || len(stored.Readings) != 2 {
			t.Fatalf("%s: %+v %v", state, stored, err)
		}
		for _, r := range stored.Readings {
			if r.State != state || r.UsedPercent != "" || r.ResetsAt != "" || r.CapturedAt != "" || r.Version != "" {
				t.Fatalf("%s kept a figure: %+v", state, r)
			}
		}
		if got := five("mini"); got.Source != api.UsageBudgetSourceNone || got.RemainingTokens != "" || got.Reading != "is "+state {
			t.Fatalf("after %s: %+v", state, got)
		}
	}
	// With an owner reset the invalidated host falls to the allowance source.
	if _, err := s.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 1000, ResetAt: ts(base.Add(-2 * time.Hour))}, by); err != nil {
		t.Fatal(err)
	}
	if got := five("mini"); got.Source != api.UsageBudgetSourceAllowance || got.RemainingTokens != "835" || got.Reading != "is malformed" {
		t.Fatalf("invalidated with an owner reset: %+v", got)
	}
	for name, bad := range map[string]api.ProviderUsageReport{
		"no host":            {Runtime: "claude", State: api.ProviderUsageMissing},
		"unknown runtime":    {Host: "mini", Runtime: "gemini", State: api.ProviderUsageMissing},
		"unknown state":      {Host: "mini", Runtime: "claude", State: "fine"},
		"ok without windows": {Host: "mini", Runtime: "claude", State: api.ProviderUsageOK, CapturedAt: ts(base)},
		"percent over 100":   {Host: "mini", Runtime: "claude", State: api.ProviderUsageOK, CapturedAt: ts(base), Windows: []api.ProviderUsageWindow{{Window: api.UsageWindowFiveHour, UsedPercent: 101, ResetsAt: ts(resets)}}},
		"no reset":           {Host: "mini", Runtime: "claude", State: api.ProviderUsageOK, CapturedAt: ts(base), Windows: []api.ProviderUsageWindow{{Window: api.UsageWindowFiveHour, UsedPercent: 5}}},
		"figures in a miss":  {Host: "mini", Runtime: "claude", State: api.ProviderUsageMissing, Windows: []api.ProviderUsageWindow{{Window: api.UsageWindowFiveHour, UsedPercent: 5, ResetsAt: ts(resets)}}},
	} {
		if _, err := s.ReportProviderUsage(ctx, bad); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// a8: the four lane defaults round-trip; values and writers are checked.
func TestUsageEstimateDefaults(t *testing.T) {
	s, task, handler, _, _ := usageFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	out, err := s.UsageEstimateDefaults(ctx, task.ID)
	if err != nil || out.Configured {
		t.Fatalf("no defaults: %+v %v", out, err)
	}
	req := api.UsageEstimateDefaultsRequest{SmallTokens: 18_000_000, SmallRaceTokens: 17_000_000, PlannedTokens: 44_000_000, PlannedRaceTokens: 70_000_000, AgentID: handler.ID}
	if out, err = s.SetUsageEstimateDefaults(ctx, task.ID, req, by); err != nil {
		t.Fatal(err)
	}
	read, err := s.UsageEstimateDefaults(ctx, task.ID)
	if err != nil || !reflect.DeepEqual(read, out) || !read.Configured || read.SmallTokens != 18_000_000 || read.SmallRaceTokens != 17_000_000 || read.PlannedTokens != 44_000_000 || read.PlannedRaceTokens != 70_000_000 || read.UpdatedBy.AgentID != handler.ID || read.UpdatedBy.User != "owner" {
		t.Fatalf("defaults %+v %+v %v", read, out, err)
	}
	req.PlannedTokens, req.AgentID = 50_000_000, ""
	if out, err = s.SetUsageEstimateDefaults(ctx, task.ID, req, by); err != nil || out.PlannedTokens != 50_000_000 || out.UpdatedBy.AgentID != "" {
		t.Fatalf("recomputed defaults %+v %v", out, err)
	}
	req.SmallTokens = 0
	if _, err = s.SetUsageEstimateDefaults(ctx, task.ID, req, by); !errors.Is(err, api.ErrInvalid) || !strings.Contains(err.Error(), "lane default") {
		t.Fatalf("a zero default: %v", err)
	}
	other, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "builder", AgentID: api.NewID("agt"), Host: "fixture", Session: "builder"}, by)
	if err != nil {
		t.Fatal(err)
	}
	req.SmallTokens, req.AgentID = 1, other.ID
	if _, err = s.SetUsageEstimateDefaults(ctx, task.ID, req, by); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "the lane default estimates") {
		t.Fatalf("an ordinary agent: %v", err)
	}
	if _, err = s.UsageEstimateDefaults(ctx, "tsk_0000000000000000"); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

// wi_44b17e10c6450225 v3: the corrections label is judged inside the review
// stage that holds the ASSIGN. The first build ASSIGN after a findings stage
// that left blockers is build; an ASSIGN after a blocker round of its own
// stage is still corrections.
func TestUsagePhaseCorrectionsStayInsideReviewStage(t *testing.T) {
	s, task, a, items, _ := usageFixture(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`UPDATE agents SET role='' WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	plan, _ := json.Marshal(map[string]any{"members": []any{map[string]any{"fields": map[string]any{"agentId": a.ID, "role": "builder"}, "runId": a.RunID}}})
	if _, err := s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,host,cwd,pause_generation,launch_json,created_at,updated_at) VALUES(?,?,?,1,1,'planned',1,'running',1,'fixture','/tmp',0,?,?,?)`, api.NewID("tqe"), task.ID, items[0].ID, string(plan), ts(s.now()), ts(s.now())); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	specs := []struct {
		kind, want string
	}{{"assign", "build"}, {"review", "review round 1"}, {"assign", "corrections"}, {"assign", "build"}, {"review", "review round 1"}, {"assign", "corrections"}}
	messages := []api.Message{}
	for i, spec := range specs {
		e := api.Envelope{Kind: spec.kind, Subject: "Synthetic phase request", Refs: map[string]string{"item": items[0].ID}}
		if spec.kind == "review" {
			e.Review = &api.ReviewMetadata{Mode: "general"}
		}
		raw, _ := json.Marshal(e)
		r, err := s.db.Exec(`INSERT INTO messages(task_id,from_agent,from_node,from_user,to_agent,text,created_at,envelope) VALUES(?,'','fixture','owner',?,'synthetic',?,?)`, task.ID, a.ID, ts(at.Add(time.Duration(i)*time.Minute)), string(raw))
		if err != nil {
			t.Fatal(err)
		}
		seq, _ := r.LastInsertId()
		messages = append(messages, api.Message{Seq: seq})
	}
	blockers := []api.ReviewFinding{{ID: "blocker", Title: "Synthetic blocker"}}
	// Stage one is a findings stage that left a blocker; messages[3] opens stage two.
	state := api.ReviewConvergence{ItemID: items[0].ID,
		Stages: []api.ReviewStage{{Number: 1, ScopeRevision: 1, AssignmentSeq: messages[0].Seq}, {Number: 2, OrderSeq: 1, ScopeRevision: 2, AssignmentSeq: messages[3].Seq}},
		Rounds: []api.ReviewRound{{Number: 1, RequestSeq: messages[1].Seq, ResultSeq: messages[1].Seq, Blockers: blockers}, {Number: 1, RequestSeq: messages[4].Seq, ResultSeq: messages[4].Seq, Blockers: blockers}}}
	raw, _ := json.Marshal(state)
	if _, err := s.db.Exec(`INSERT INTO review_convergence VALUES(?,?,?)`, task.ID, items[0].ID, string(raw)); err != nil {
		t.Fatal(err)
	}
	for i, m := range messages {
		turn := syntheticUsageTurn(fmt.Sprintf("stage-phase-%d", i))
		turn.At = at.Add(time.Duration(i)*time.Minute + time.Second)
		turn.Handled = []api.UsageEvidence{{TaskID: task.ID, Seq: m.Seq, Operation: "ack", At: turn.At}}
		if _, err := s.ReportUsage(ctx, task.ID, a.ID, usageBatch(a, fmt.Sprintf("stage-phase-batch-%d", i), turn)); err != nil {
			t.Fatal(err)
		}
	}
	report, err := s.Usage(ctx, task.ID, api.UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, x := range report.Items {
		if x.ItemID == items[0].ID {
			for _, g := range x.Phases {
				got[g.Key] = g.Summary.Requests
			}
		}
	}
	if want := map[string]int{"build": 2, "corrections": 2, "review round 1": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("phases\n got %+v\nwant %+v", got, want)
	}
}
