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
