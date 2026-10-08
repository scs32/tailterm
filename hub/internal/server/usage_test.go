package server

import (
	"context"
	"errors"
	"github.com/scs32/tailterm/hub/internal/api"
	"strings"
	"testing"
	"time"
)

func TestUsageHTTPExactRequestReplayPricesAndFilters(t *testing.T) {
	f := newClient(t)
	ctx := context.Background()
	task, err := f.st.CreateTask(ctx, api.CreateTaskRequest{Name: "Usage HTTP synthetic"}, f.who)
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "metered", Host: "fixture", Session: "synthetic", Runtime: "claude"}, f.who)
	if err != nil {
		t.Fatal(err)
	}
	c, err := api.NewClient(f.srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	turn := api.UsageTurn{ID: "synthetic-one", Revision: 1, Runtime: "claude", Session: "synthetic", Model: "fixture-model", At: at, Tokens: map[string]int64{"input": 3, "cached": 2, "cacheWrite": 1, "output": 3, "reasoning": 1}, Raw: map[string]int64{"input_tokens": 3, "cache_read_input_tokens": 2, "cache_creation_input_tokens": 1, "output_tokens": 4, "output_tokens_details.thinking_tokens": 1}, SourceDigest: strings.Repeat("a", 64), Complete: true}
	b := api.UsageBatch{Version: 1, RequestID: "http-usage", RunID: a.RunID, Session: "synthetic", StartedAt: at, Turns: []api.UsageTurn{turn}}
	r, err := c.ReportUsage(ctx, task.ID, a.ID, b)
	if err != nil || r.Turns != 1 {
		t.Fatal(r, err)
	}
	if _, err = c.ReportUsage(ctx, task.ID, a.ID, b); err != nil {
		t.Fatal(err)
	}
	report, err := c.Usage(ctx, task.ID, api.UsageQuery{})
	if err != nil || report.Summary.Requests != 1 || report.Overhead.Summary.Requests != 1 {
		t.Fatal(report, err)
	}
	report, err = c.Usage(ctx, task.ID, api.UsageQuery{To: at})
	if err != nil || report.Summary.Requests != 0 {
		t.Fatal(report, err)
	}
	if _, err = c.Usage(ctx, task.ID, api.UsageQuery{From: at, To: at}); err == nil {
		t.Fatal("equal bounds accepted")
	}
	prices, err := c.UsagePrices(ctx, task.ID)
	if err != nil || len(prices.Rows) != 0 {
		t.Fatal(prices, err)
	}
	req := api.UsagePriceRequest{ExpectedRevision: 0, RequestID: "http-prices", Rows: []api.UsagePrice{{Runtime: "claude", Model: "fixture-model", Currency: "USD", EffectiveAt: at, Rates: map[string]string{"input": "1"}}}}
	prices, err = c.SetUsagePrices(ctx, task.ID, req)
	if err != nil || prices.Revision != 1 {
		t.Fatal(prices, err)
	}
	req.RequestID = "http-stale"
	_, err = c.SetUsagePrices(ctx, task.ID, req)
	var httpErr *api.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 409 {
		t.Fatal("CAS status", err)
	}
	b.Turns[0].Tokens["output"] = 5
	b.Turns[0].Raw["output_tokens"] = 6
	if _, err = c.ReportUsage(ctx, task.ID, a.ID, b); !errors.As(err, &httpErr) || httpErr.Status != 409 {
		t.Fatal("changed batch accepted", err)
	}
}

// Token estimate warning threshold over HTTP (wi_3228104e700006c5, a8).
func TestUsageWarningHTTP(t *testing.T) {
	f := newClient(t)
	ctx := context.Background()
	task, err := f.st.CreateTask(ctx, api.CreateTaskRequest{Name: "Usage warning HTTP synthetic"}, f.who)
	if err != nil {
		t.Fatal(err)
	}
	c, err := api.NewClient(f.srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.UsageWarnings(ctx, task.ID)
	if err != nil || api.DefaultUsageWarningThreshold != "1.5" || got.Threshold != "1.5" || !got.Default || got.Warnings == nil || len(got.Warnings) != 0 {
		t.Fatalf("default %+v %v", got, err)
	}
	got, err = c.SetUsageWarning(ctx, task.ID, api.UsageWarningRequest{Threshold: "2"})
	if err != nil || got.Threshold != "2" || got.Default {
		t.Fatalf("set %+v %v", got, err)
	}
	got, err = c.UsageWarnings(ctx, task.ID)
	if err != nil || got.Threshold != "2" || got.Default {
		t.Fatalf("get after set %+v %v", got, err)
	}
	var httpErr *api.HTTPError
	for _, bad := range []string{"0.9", "101", "abc", "1.2345", ""} {
		_, err = c.SetUsageWarning(ctx, task.ID, api.UsageWarningRequest{Threshold: bad})
		if !errors.As(err, &httpErr) || httpErr.Status != 400 {
			t.Fatalf("threshold %q: %v", bad, err)
		}
	}
	a, err := f.st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "fixture", Session: "synthetic", Runtime: "claude"}, f.who)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SetUsageWarning(ctx, task.ID, api.UsageWarningRequest{Threshold: "3", AgentID: a.ID})
	if !errors.As(err, &httpErr) || httpErr.Status != 409 {
		t.Fatalf("an agent that is not the owner helper: %v", err)
	}
	_, err = c.UsageWarnings(ctx, "tsk_0000000000000000")
	if !errors.As(err, &httpErr) || httpErr.Status != 404 {
		t.Fatalf("unknown project: %v", err)
	}
	if got, err = c.UsageWarnings(ctx, task.ID); err != nil || got.Threshold != "2" {
		t.Fatalf("a refused save changed the threshold: %+v %v", got, err)
	}
}

func httpStatus(err error) int {
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Status
	}
	return 0
}

// The budget, lane default and provider reading routes: each round-trips over
// HTTP, refuses bad values and unauthorised agents, and answers 404 for an
// unknown project.
func TestUsageBudgetDefaultsAndProviderHTTP(t *testing.T) {
	f := newClient(t)
	ctx := context.Background()
	task, err := f.st.CreateTask(ctx, api.CreateTaskRequest{Name: "Usage budget HTTP synthetic"}, f.who)
	if err != nil {
		t.Fatal(err)
	}
	c, err := api.NewClient(f.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	budgets, err := c.UsageBudgets(ctx, task.ID, "fixture")
	if err != nil || budgets.Configured || budgets.Budgets == nil || len(budgets.Budgets) != 0 || budgets.Host != "fixture" {
		t.Fatalf("no budget: %+v %v", budgets, err)
	}
	reset := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339)
	budgets, err = c.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 1000, ReservePercent: 10, ResetAt: reset, StaleSeconds: 300})
	if err != nil || !budgets.Configured || len(budgets.Budgets) != 1 {
		t.Fatalf("set budget: %+v %v", budgets, err)
	}
	budgets, err = c.UsageBudgets(ctx, task.ID, "fixture")
	if err != nil || len(budgets.Budgets) != 1 {
		t.Fatalf("get budget: %+v %v", budgets, err)
	}
	row := budgets.Budgets[0]
	if row.Runtime != "claude" || row.Window != api.UsageWindowFiveHour || row.AllowanceTokens != 1000 || row.ReservePercent != 10 || row.ResetAt != reset || row.StaleSeconds != 300 || row.UpdatedBy.User != f.who.User ||
		row.Status == nil || row.Status.Source != api.UsageBudgetSourceAllowance || row.Status.RemainingTokens != "1000" || row.Status.Host != "fixture" || row.Status.Reading != "is not reported" {
		t.Fatalf("budget row %+v status %+v", row, row.Status)
	}
	for name, bad := range map[string]api.UsageBudgetRequest{
		"missing allowance": {Runtime: "claude", Window: api.UsageWindowFiveHour},
		"unknown window":    {Runtime: "claude", Window: "monthly", AllowanceTokens: 5},
	} {
		// The refusal names the value that was wrong.
		if _, err = c.SetUsageBudget(ctx, task.ID, bad); httpStatus(err) != 400 || !strings.Contains(err.Error(), "budget") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	worker, err := f.st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "fixture", Session: "synthetic", Runtime: "claude"}, f.who)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 5, AgentID: worker.ID}); httpStatus(err) != 409 || !strings.Contains(err.Error(), "only the owner, the owner helper or a database handler") {
		t.Fatalf("budget set by an ordinary agent: %v", err)
	}
	if _, err = c.DeleteUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AgentID: worker.ID}); httpStatus(err) != 409 {
		t.Fatalf("budget clear by an ordinary agent: %v", err)
	}

	// A provider reading for the host becomes the source; an invalidation
	// removes its figures.
	captured := time.Now().UTC().Format(time.RFC3339Nano)
	resets := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second).Format(time.RFC3339)
	stored, err := c.ReportProviderUsage(ctx, api.ProviderUsageReport{Host: "fixture", Runtime: "claude", State: api.ProviderUsageOK, CapturedAt: captured, Version: "2.1.292",
		Windows: []api.ProviderUsageWindow{{Window: api.UsageWindowFiveHour, UsedPercent: 13, ResetsAt: resets}, {Window: api.UsageWindowSevenDay, UsedPercent: 37, ResetsAt: resets}}})
	if err != nil || stored.Host != "fixture" || len(stored.Readings) != 2 {
		t.Fatalf("report reading: %+v %v", stored, err)
	}
	read, err := c.ProviderUsage(ctx, "fixture")
	if err != nil || len(read.Readings) != 2 || read.Readings[0].Window != api.UsageWindowFiveHour || read.Readings[0].UsedPercent != "13" || read.Readings[0].ResetsAt != resets || read.Readings[0].State != api.ProviderUsageOK || read.Readings[1].UsedPercent != "37" {
		t.Fatalf("read reading: %+v %v", read, err)
	}
	if budgets, err = c.UsageBudgets(ctx, task.ID, "fixture"); err != nil || budgets.Budgets[0].Status.Source != api.UsageBudgetSourceProvider || budgets.Budgets[0].Status.RemainingTokens != "870" || budgets.Budgets[0].Status.ResetAt != resets {
		t.Fatalf("provider status: %+v %v", budgets.Budgets[0].Status, err)
	}
	if stored, err = c.ReportProviderUsage(ctx, api.ProviderUsageReport{Host: "fixture", Runtime: "claude", State: api.ProviderUsageMissing}); err != nil || len(stored.Readings) != 2 || stored.Readings[0].UsedPercent != "" || stored.Readings[0].ResetsAt != "" || stored.Readings[0].State != api.ProviderUsageMissing {
		t.Fatalf("invalidation: %+v %v", stored, err)
	}
	if budgets, err = c.UsageBudgets(ctx, task.ID, "fixture"); err != nil || budgets.Budgets[0].Status.Source != api.UsageBudgetSourceAllowance || budgets.Budgets[0].Status.Reading != "is missing" {
		t.Fatalf("status after the invalidation: %+v %v", budgets.Budgets[0].Status, err)
	}
	if _, err = c.ReportProviderUsage(ctx, api.ProviderUsageReport{Host: "fixture", Runtime: "claude", State: "fine"}); httpStatus(err) != 400 {
		t.Fatalf("unknown reading state: %v", err)
	}
	if _, err = c.ProviderUsage(ctx, ""); httpStatus(err) != 400 {
		t.Fatalf("reading with no host: %v", err)
	}
	if empty, err := c.ProviderUsage(ctx, "elsewhere"); err != nil || empty.Readings == nil || len(empty.Readings) != 0 {
		t.Fatalf("a host with no reading: %+v %v", empty, err)
	}

	// Lane defaults.
	defaults, err := c.UsageEstimateDefaults(ctx, task.ID)
	if err != nil || defaults.Configured {
		t.Fatalf("no defaults: %+v %v", defaults, err)
	}
	defaults, err = c.SetUsageEstimateDefaults(ctx, task.ID, api.UsageEstimateDefaultsRequest{SmallTokens: 18_000_000, SmallRaceTokens: 17_000_000, PlannedTokens: 44_000_000, PlannedRaceTokens: 70_000_000})
	if err != nil || !defaults.Configured {
		t.Fatalf("set defaults: %+v %v", defaults, err)
	}
	if defaults, err = c.UsageEstimateDefaults(ctx, task.ID); err != nil || defaults.SmallTokens != 18_000_000 || defaults.SmallRaceTokens != 17_000_000 || defaults.PlannedTokens != 44_000_000 || defaults.PlannedRaceTokens != 70_000_000 {
		t.Fatalf("get defaults: %+v %v", defaults, err)
	}
	if _, err = c.SetUsageEstimateDefaults(ctx, task.ID, api.UsageEstimateDefaultsRequest{SmallTokens: 1, SmallRaceTokens: 1, PlannedTokens: 1}); httpStatus(err) != 400 {
		t.Fatalf("a missing default: %v", err)
	}
	if _, err = c.SetUsageEstimateDefaults(ctx, task.ID, api.UsageEstimateDefaultsRequest{SmallTokens: 1, SmallRaceTokens: 1, PlannedTokens: 1, PlannedRaceTokens: 1, AgentID: worker.ID}); httpStatus(err) != 409 {
		t.Fatalf("defaults set by an ordinary agent: %v", err)
	}

	// Holds: none, and the exact-run form.
	holds, err := c.UsageHolds(ctx, task.ID, "", "")
	if err != nil || holds.Held || holds.Holds == nil || len(holds.Holds) != 0 {
		t.Fatalf("no holds: %+v %v", holds, err)
	}
	if holds, err = c.UsageHolds(ctx, task.ID, worker.ID, worker.RunID); err != nil || holds.Held {
		t.Fatalf("an unheld run: %+v %v", holds, err)
	}
	if _, err = c.UsageHolds(ctx, task.ID, worker.ID, ""); httpStatus(err) != 400 {
		t.Fatalf("an agent without its run: %v", err)
	}

	// Clearing the row switches the check off; an unknown project is 404.
	if budgets, err = c.DeleteUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour}); err != nil || budgets.Configured {
		t.Fatalf("clear budget: %+v %v", budgets, err)
	}
	if _, err = c.DeleteUsageBudget(ctx, task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour}); httpStatus(err) != 404 {
		t.Fatalf("clear a row that is not set: %v", err)
	}
	const unknown = "tsk_0000000000000000"
	if _, err = c.UsageBudgets(ctx, unknown, ""); httpStatus(err) != 404 {
		t.Fatalf("budget of an unknown project: %v", err)
	}
	if _, err = c.SetUsageBudget(ctx, unknown, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 5}); httpStatus(err) != 404 {
		t.Fatalf("budget set on an unknown project: %v", err)
	}
	if _, err = c.UsageEstimateDefaults(ctx, unknown); httpStatus(err) != 404 {
		t.Fatalf("defaults of an unknown project: %v", err)
	}
	if _, err = c.SetUsageEstimateDefaults(ctx, unknown, api.UsageEstimateDefaultsRequest{SmallTokens: 1, SmallRaceTokens: 1, PlannedTokens: 1, PlannedRaceTokens: 1}); httpStatus(err) != 404 {
		t.Fatalf("defaults set on an unknown project: %v", err)
	}
	if _, err = c.UsageHolds(ctx, unknown, "", ""); httpStatus(err) != 404 {
		t.Fatalf("holds of an unknown project: %v", err)
	}
}

// holdHTTP holds the operator fixture's running team past 3 times a saved
// estimate over HTTP, with a registered owner helper and a lead that owes an
// open obligation. It returns the hub client, the entry and the helper.
func holdHTTP(t *testing.T) (*operatorHTTP, api.TeamQueueEntry, api.Agent) {
	t.Helper()
	f := newOperatorHTTP(t)
	ctx := context.Background()
	registered, err := f.c.st.RegisterOwnerHelper(ctx, f.task.ID, api.RegisterOwnerHelperRequest{Host: "owner-host", Session: "owner", Runtime: "claude", Cwd: "/work/tailterm", RequestID: "hold-helper"}, f.c.who)
	if err != nil || registered.Agent == nil {
		t.Fatalf("register the owner helper: %+v %v", registered, err)
	}
	tokens, basis := int64(1000), "synthetic"
	if _, _, err := f.c.st.CreateWorkItemUpdate(ctx, f.task.ID, f.item.ID, api.CreateWorkItemUpdate{ExpectedRevision: f.item.Revision, RequestID: "hold-estimate", EstimateTokens: &tokens, EstimateBasis: &basis}, f.c.who); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.st.PostMessage(ctx, f.task.ID, api.PostMessageRequest{To: f.lead.ID, RequestID: "open-for-lead", Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: f.lead.Name, Subject: "Report the state of the candidate", Body: api.EnvelopeBody{Ask: "Report it"}}}, f.c.who); err != nil {
		t.Fatal(err)
	}
	order := f.builder.WorkItem.WorkOrderMessage
	at := time.Now().UTC()
	turn := api.UsageTurn{ID: "hold-turn", Revision: 1, Runtime: "codex", Session: "synthetic", Model: "fixture-model", At: at, Tokens: map[string]int64{"input": 3001, "cached": 0, "cacheWrite": 0, "output": 0, "reasoning": 0},
		Raw: map[string]int64{"input_tokens": 3001, "cached_input_tokens": 0, "cache_write_input_tokens": 0, "output_tokens": 0, "reasoning_output_tokens": 0}, SourceDigest: strings.Repeat("a", 64), Complete: true,
		Handled: []api.UsageEvidence{{TaskID: order.TaskID, Seq: order.Seq, Operation: "ack", At: at}}}
	if _, err := f.hub.ReportUsage(ctx, f.task.ID, f.builder.ID, api.UsageBatch{Version: 1, RequestID: "hold-usage", RunID: f.builder.RunID, Session: "synthetic", StartedAt: at, Turns: []api.UsageTurn{turn}}); err != nil {
		t.Fatal(err)
	}
	list, err := f.hub.ListTeamQueue(ctx, f.task.ID)
	if err != nil || len(list.Entries) != 1 || list.Entries[0].BudgetHold == nil || list.Entries[0].BudgetHold.State != api.BudgetHoldHeld || list.Entries[0].BudgetHold.TeamTokens != "3001" || list.Entries[0].AdmittedAt == "" {
		t.Fatalf("held entry over HTTP: %+v %v", list.Entries, err)
	}
	return f, list.Entries[0], *registered.Agent
}

// a13, a16 over HTTP: the holds route lists the team by exact run and never
// the database handler; continue is the owner helper's, through the existing
// queue action route.
func TestUsageHoldContinueHTTP(t *testing.T) {
	f, entry, helper := holdHTTP(t)
	ctx := context.Background()
	holds, err := f.hub.UsageHolds(ctx, f.task.ID, "", "")
	if err != nil || !holds.Held || len(holds.Holds) != 1 || holds.Holds[0].EntryID != entry.ID || holds.Holds[0].AskAgent != helper.ID || holds.Holds[0].AskSeq == 0 {
		t.Fatalf("holds: %+v %v", holds, err)
	}
	listed := map[string]string{}
	for _, run := range holds.Holds[0].Runs {
		listed[run.AgentID] = run.RunID
	}
	for _, a := range []api.Agent{f.lead, f.builder, f.verifier, f.review} {
		if listed[a.ID] != a.RunID {
			t.Fatalf("%s is not listed by its exact run: %v", a.Name, listed)
		}
		if one, err := f.hub.UsageHolds(ctx, f.task.ID, a.ID, a.RunID); err != nil || !one.Held || len(one.Holds) != 1 {
			t.Fatalf("exact run of %s: %+v %v", a.Name, one, err)
		}
	}
	for _, a := range []api.Agent{f.handler, helper} {
		if _, ok := listed[a.ID]; ok {
			t.Fatalf("%s is listed as held", a.Name)
		}
		if one, err := f.hub.UsageHolds(ctx, f.task.ID, a.ID, a.RunID); err != nil || one.Held {
			t.Fatalf("exact run of %s: %+v %v", a.Name, one, err)
		}
	}
	continued := func(agent string) error {
		_, err := f.hub.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "budget_continue", EntryID: entry.ID, ExpectedRevision: entry.Revision, AgentID: agent})
		return err
	}
	if err := continued(f.builder.ID); httpStatus(err) != 409 || !strings.Contains(err.Error(), "only the owner or the owner helper continues a held team") {
		t.Fatalf("continue by a team member: %v", err)
	}
	if err := continued(helper.ID); err != nil {
		t.Fatalf("continue by the owner helper: %v", err)
	}
	if holds, err = f.hub.UsageHolds(ctx, f.task.ID, f.builder.ID, f.builder.RunID); err != nil || holds.Held {
		t.Fatalf("after continue: %+v %v", holds, err)
	}
	got, err := f.hub.GetTeamQueueEntry(ctx, f.task.ID, entry.ID)
	if err != nil || got.State != "running" || got.BudgetHold != nil {
		t.Fatalf("entry after continue: %s %+v %v", got.State, got.BudgetHold, err)
	}
}

// a17 over HTTP: with the lead owing an open obligation, the existing fail
// operation stops the held team: the entry fails with the reason, the hold is
// stopped, and the team's runs stay listed while it is unreleased.
func TestUsageHoldStopByFailHTTP(t *testing.T) {
	f, entry, _ := holdHTTP(t)
	ctx := context.Background()
	reason := "Owner failed this entry: past three times the estimate"
	failed, err := f.hub.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "queue-owner-fail", Operation: "fail", EntryID: entry.ID, ExpectedRevision: entry.Revision, Failure: reason})
	if err != nil || failed.State != "failed" || failed.Failure != reason || failed.EscalationSeq == 0 || failed.ReleasedAt != "" {
		t.Fatalf("fail of a held entry: %+v %v", failed, err)
	}
	holds, err := f.hub.UsageHolds(ctx, f.task.ID, "", "")
	if err != nil || !holds.Held || len(holds.Holds) != 1 || holds.Holds[0].State != api.BudgetHoldStopped || holds.Holds[0].ResolvedAt == "" {
		t.Fatalf("holds after the stop: %+v %v", holds, err)
	}
	for _, a := range []api.Agent{f.lead, f.builder} {
		if one, err := f.hub.UsageHolds(ctx, f.task.ID, a.ID, a.RunID); err != nil || !one.Held {
			t.Fatalf("%s wakes after the stop: %+v %v", a.Name, one, err)
		}
	}
	if _, err := f.hub.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "budget_continue", EntryID: entry.ID, ExpectedRevision: failed.Revision}); httpStatus(err) != 409 {
		t.Fatalf("continue of a stopped hold: %v", err)
	}
}
