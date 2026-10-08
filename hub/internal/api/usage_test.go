package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestUsageNormalizedClassesPreserveRawAvailability(t *testing.T) {
	raw := map[string]int64{"input_tokens": 100, "cached_input_tokens": 80, "output_tokens": 10, "reasoning_output_tokens": 4}
	got, gap := NormalizeUsageTokens("codex", raw)
	want := map[string]int64{"input": 20, "cached": 80, "output": 6, "reasoning": 4}
	if gap != "" || !reflect.DeepEqual(got, want) || raw["input_tokens"] != 100 {
		t.Fatal(got, gap, raw)
	}
	if _, ok := got["cacheWrite"]; ok {
		t.Fatal("unavailable cache write inferred zero")
	}
	raw["cached_input_tokens"] = 120
	got, gap = NormalizeUsageTokens("codex", raw)
	if _, ok := got["input"]; ok || gap == "" {
		t.Fatal("invalid subset", got, gap)
	}
}

func TestUsageRealShapedClassesAndUncertainCodexWriteOverlap(t *testing.T) {
	codex := map[string]int64{"input_tokens": 100, "cached_input_tokens": 80, "cache_write_input_tokens": 0, "output_tokens": 10, "reasoning_output_tokens": 4, "total_tokens": 110}
	got, gap := NormalizeUsageTokens("codex", codex)
	if gap != "" || len(got) != 5 || got["input"] != 20 || got["cacheWrite"] != 0 || got["output"] != 6 {
		t.Fatal(got, gap)
	}
	codex["cache_write_input_tokens"] = 3
	got, gap = NormalizeUsageTokens("codex", codex)
	if _, known := got["input"]; known || got["cacheWrite"] != 3 || gap == "" {
		t.Fatal("unknown overlap guessed", got, gap)
	}
	claude := map[string]int64{"input_tokens": 3, "cache_read_input_tokens": 80, "cache_creation_input_tokens": 12, "output_tokens": 7, "output_tokens_details.thinking_tokens": 2}
	got, gap = NormalizeUsageTokens("claude", claude)
	if gap != "" || len(got) != 5 || got["input"] != 3 || got["output"] != 5 || got["reasoning"] != 2 {
		t.Fatal(got, gap)
	}
}

func TestUsageClaudeAbsentThinkingPreservesInclusiveOutput(t *testing.T) {
	raw := map[string]int64{"input_tokens": 2, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 4, "output_tokens": 7}
	got, gap := NormalizeUsageTokens("claude", raw)
	if got["output"] != 7 || gap == "" {
		t.Fatal("known output lost", got, gap)
	}
	if _, known := got["reasoning"]; known {
		t.Fatal("reasoning fabricated", got)
	}
	raw["output_tokens_details.thinking_tokens"] = 2
	got, gap = NormalizeUsageTokens("claude", raw)
	if got["output"] != 5 || got["reasoning"] != 2 || gap != "" {
		t.Fatal("thinking double counted", got, gap)
	}
}

func TestUsageWindowLengths(t *testing.T) {
	if UsageWindowLength(UsageWindowFiveHour) != 5*time.Hour || UsageWindowLength(UsageWindowSevenDay) != 7*24*time.Hour || UsageWindowLength("daily") != 0 || UsageWindowLength("") != 0 {
		t.Fatal("budget window lengths")
	}
	if BudgetHoldMultiple != 3 || DefaultUsageBudgetStaleSeconds != 900 {
		t.Fatal("budget constants")
	}
}

// The wire names of the budget types are what the hub, tt and the relay share.
func TestUsageBudgetWireShapes(t *testing.T) {
	for name, c := range map[string]struct {
		value any
		want  string
	}{
		"budget": {UsageBudgets{Configured: true, Host: "mini", Budgets: []UsageBudget{{Runtime: "claude", Window: UsageWindowFiveHour, AllowanceTokens: 5, ReservePercent: 10, ResetAt: "r", StaleSeconds: 900, UpdatedAt: "u", UpdatedBy: Sender{Node: "n", User: "o"},
			Status: &UsageBudgetStatus{Host: "mini", Source: UsageBudgetSourceProvider, RemainingTokens: "4", ResetAt: "t"}}}},
			`{"configured":true,"host":"mini","budgets":[{"runtime":"claude","window":"five_hour","allowanceTokens":5,"reservePercent":10,"resetAt":"r","staleSeconds":900,"updatedAt":"u","updatedBy":{"node":"n","user":"o"},"status":{"host":"mini","source":"provider","remainingTokens":"4","resetAt":"t"}}]}`},
		"not configured": {UsageBudgets{Budgets: []UsageBudget{}}, `{"configured":false,"budgets":[]}`},
		"defaults": {UsageEstimateDefaults{Configured: true, SmallTokens: 1, SmallRaceTokens: 2, PlannedTokens: 3, PlannedRaceTokens: 4},
			`{"configured":true,"smallTokens":1,"smallRaceTokens":2,"plannedTokens":3,"plannedRaceTokens":4,"updatedBy":{"node":"","user":""}}`},
		"report": {ProviderUsageReport{Host: "mini", Runtime: "claude", State: ProviderUsageOK, CapturedAt: "c", Version: "v", Windows: []ProviderUsageWindow{{Window: UsageWindowSevenDay, UsedPercent: 37.5, ResetsAt: "r"}}},
			`{"host":"mini","runtime":"claude","state":"ok","capturedAt":"c","version":"v","windows":[{"window":"seven_day","usedPercent":37.5,"resetsAt":"r"}]}`},
		"invalidation": {ProviderUsageReport{Host: "mini", Runtime: "claude", State: ProviderUsageMissing}, `{"host":"mini","runtime":"claude","state":"missing"}`},
		"holds": {UsageHolds{Held: true, Holds: []BudgetHold{{EntryID: "e", ItemID: "i", EstimateTokens: 5, TeamTokens: "16", TeamState: "measured", State: BudgetHoldHeld, AskAgent: "a", AskSeq: 7, CreatedAt: "c", Runs: []BudgetHoldRun{{AgentID: "a", RunID: "r"}}}}},
			`{"held":true,"holds":[{"entryId":"e","itemId":"i","estimateTokens":5,"teamTokens":"16","teamState":"measured","state":"held","askAgent":"a","askSeq":7,"createdAt":"c","runs":[{"agentId":"a","runId":"r"}]}]}`},
		"entry fields": {TeamQueueEntry{AdmittedAt: "a", EstimateDefault: &TeamQueueEstimateDefault{Tokens: 70, Lane: "planned", GoRace: true}}.budgetFields(),
			`{"admittedAt":"a","estimateDefault":{"tokens":70,"lane":"planned","goRace":true}}`},
		"continue request": {TeamQueueRequest{RequestID: "r", Operation: "budget_continue", EntryID: "e", ExpectedRevision: 2, AgentID: "a"},
			`{"requestId":"r","operation":"budget_continue","entryId":"e","expectedRevision":2,"agentId":"a"}`},
	} {
		got, err := json.Marshal(c.value)
		if err != nil || string(got) != c.want {
			t.Fatalf("%s:\n%s\nwant\n%s (%v)", name, got, c.want, err)
		}
	}
}

// budgetFields is the entry's budget fields alone, for the wire shape test.
func (e TeamQueueEntry) budgetFields() any {
	return struct {
		AdmittedAt      string                    `json:"admittedAt,omitempty"`
		EstimateDefault *TeamQueueEstimateDefault `json:"estimateDefault,omitempty"`
		BudgetHold      *BudgetHold               `json:"budgetHold,omitempty"`
	}{e.AdmittedAt, e.EstimateDefault, e.BudgetHold}
}

// Each client call uses its route and method, and a 404 from a hub without
// the route is returned as it is, for the relay to read as "not held".
func TestUsageBudgetClientRoutes(t *testing.T) {
	var seen []string
	missing := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		if missing {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	calls := []func() error{
		func() error { _, err := c.UsageBudgets(ctx, "tsk_1", "the mini"); return err },
		func() error { _, err := c.UsageBudgets(ctx, "tsk_1", ""); return err },
		func() error { _, err := c.SetUsageBudget(ctx, "tsk_1", UsageBudgetRequest{}); return err },
		func() error { _, err := c.DeleteUsageBudget(ctx, "tsk_1", UsageBudgetRequest{}); return err },
		func() error { _, err := c.UsageEstimateDefaults(ctx, "tsk_1"); return err },
		func() error {
			_, err := c.SetUsageEstimateDefaults(ctx, "tsk_1", UsageEstimateDefaultsRequest{})
			return err
		},
		func() error { _, err := c.UsageHolds(ctx, "tsk_1", "", ""); return err },
		func() error { _, err := c.UsageHolds(ctx, "tsk_1", "agt_1", "run_1"); return err },
		func() error { _, err := c.ReportProviderUsage(ctx, ProviderUsageReport{}); return err },
		func() error { _, err := c.ProviderUsage(ctx, "the mini"); return err },
	}
	for i, call := range calls {
		if err := call(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	want := []string{
		"GET /v1/tasks/tsk_1/usage/budget?host=the+mini", "GET /v1/tasks/tsk_1/usage/budget", "PUT /v1/tasks/tsk_1/usage/budget", "DELETE /v1/tasks/tsk_1/usage/budget",
		"GET /v1/tasks/tsk_1/usage/defaults", "PUT /v1/tasks/tsk_1/usage/defaults",
		"GET /v1/tasks/tsk_1/usage/holds", "GET /v1/tasks/tsk_1/usage/holds?agent=agt_1&run=run_1",
		"PUT /v1/provider-usage", "GET /v1/provider-usage?host=the+mini",
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("routes\n%v\nwant\n%v", seen, want)
	}
	missing = true
	for i, call := range calls {
		var httpErr *HTTPError
		if err := call(); !errors.As(err, &httpErr) || httpErr.Status != http.StatusNotFound {
			t.Fatalf("call %d against an older hub: %v", i, err)
		}
	}
}
