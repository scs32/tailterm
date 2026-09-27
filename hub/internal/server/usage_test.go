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
