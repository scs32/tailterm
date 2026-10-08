package main

import (
	"context"
	"encoding/json"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/server"
	"github.com/scs32/tailterm/hub/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestUsageCLIRealHTTPFiltersJSONAndPrices(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/v1/tasks/tsk_1111111111111111/usage":
			if r.URL.Query().Get("item") != "wi_1111111111111111" || r.URL.Query().Get("from") != "2026-09-27T00:00:00Z" || r.URL.Query().Get("to") != "2026-09-28T00:00:00Z" {
				t.Error(r.URL.String())
			}
			json.NewEncoder(w).Encode(api.UsageReport{Version: 1, ProjectID: "tsk_1111111111111111", Items: []api.UsageItemReport{}})
		case "/v1/tasks/tsk_1111111111111111/usage/prices":
			json.NewEncoder(w).Encode(api.UsagePrices{Rows: []api.UsagePrice{}})
		default:
			t.Error("unexpected endpoint", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	e := env{hub: srv.URL, task: "tsk_1111111111111111"}
	if err := cmdUsage(e, []string{"--item", "wi_1111111111111111", "--from", "2026-09-27T00:00:00Z", "--to", "2026-09-28T00:00:00Z", "--json"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdUsage(e, []string{"prices", "get"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdUsage(e, []string{"--from", "2026-09-28T00:00:00Z", "--to", "2026-09-27T00:00:00Z"}); err == nil {
		t.Fatal("bad range contacted hub")
	}
	if requests != 2 {
		t.Fatal(requests)
	}
}

func TestUsageTextReadable(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{formatTokenCount("0"), "0"},
		{formatTokenCount("950"), "950"},
		{formatTokenCount("11/2"), "5.5"},
		{formatTokenCount("12100"), "12.1k"},
		{formatTokenCount("174400"), "174.4k"},
		{formatTokenCount("999960"), "1.00M"},
		{formatTokenCount("6710000"), "6.71M"},
		{formatTokenCount("61500000"), "61.50M"},
		{formatTokenCount("1230000000"), "1.23B"},
		{formatTokenCount("4207361/31"), "135.7k"},
		{formatTokenCount("not a number"), "unavailable"},
		{formatRatDecimal("11/2", 1), "5.5"},
		{formatRatDecimal("52", 1), "52"},
		{formatRatDecimal("1/3", 1), "0.3"},
		{formatPercent("39/40"), "97.5%"},
		{formatPercent("1"), "100.0%"},
		{formatPercent("0"), "0.0%"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	average, share := "4207361/31", "39/40"
	build := api.UsageSummary{State: "measured", Requests: 52, AllocatedTurns: "103/2",
		Tokens:         map[string]string{"input": "174400", "cached": "6710000", "cacheWrite": "0", "output": "12100"},
		AverageContext: &average, CachedShare: &share, PricedSubtotal: map[string]string{"USD": "1234/100", "EUR": "7/2"}}
	handoffs := api.UsageSummary{State: "measured", Requests: 3, AllocatedTurns: "3", Tokens: map[string]string{"input": "2500", "output": "400"}}
	// The item has a builder hand-offs phase row and measured time, so the
	// text must carry readable tokens and the time lines together.
	spent := &api.UsageTime{WallMs: "600000", ModelMs: "300000", ToolMs: "150000", WaitingMs: "150000", UnmeasuredMs: "0", PollMs: "0",
		Timeline: api.UsageTimeline{ModelMs: "300000", ToolsOnlyMs: "150000", IdleMs: "150000", UnmeasuredMs: "0"},
		Phases:   []api.UsageTimeSplit{{Key: "build", ModelMs: "240000", ToolMs: "150000", WaitingMs: "0"}, {Key: "hand-offs", ModelMs: "60000", ToolMs: "0", WaitingMs: "150000"}}}
	report := api.UsageReport{Version: 1, TimeVersion: 1, ProjectID: "tsk_1111111111111111", PriceRevision: 3, Items: []api.UsageItemReport{{TaskID: "tsk_1111111111111111", ItemID: "wi_1111111111111111", Title: "Synthetic item", Summary: build, Time: spent,
		Phases: []api.UsageGroup{{Key: "build", Label: "build", Summary: build}, {Key: "hand-offs", Label: "hand-offs", Summary: handoffs}}, Roles: []api.UsageGroup{{Key: "builder", Label: "builder", Summary: api.UsageSummary{State: "unavailable", AllocatedTurns: "0", Tokens: map[string]string{}}}}}},
		Overhead: api.UsageItemReport{Title: "Project overhead", Summary: api.UsageSummary{State: "measured", Requests: 1, AllocatedTurns: "1", Tokens: map[string]string{"input": "950"}, CostComplete: true, PricedSubtotal: map[string]string{"USD": "1/8"}}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(report) }))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	err = cmdUsage(env{hub: srv.URL, task: "tsk_1111111111111111"}, nil)
	os.Stdout = original
	write.Close()
	raw, _ := io.ReadAll(read)
	text := string(raw)
	if err != nil {
		t.Fatal(err, text)
	}
	for _, want := range []string{
		"Synthetic item: measured · requests 52 · allocated 51.5 · tokens 6.90M (input 174.4k, cached 6.71M, cache write 0, output 12.1k, reasoning unavailable) · average context 135.7k tokens · cached-input share 97.5% · estimated subtotal EUR 3.50, USD 12.34 (partial)\n",
		"  build: measured · requests 52 · ",
		"  role builder: unavailable · requests 0 · allocated 0 · tokens unavailable (input unavailable, cached unavailable, cache write unavailable, output unavailable, reasoning unavailable) · average context unavailable\n",
		"Project overhead: measured · requests 1 · allocated 1 · tokens 950 (input 950, cached unavailable, cache write unavailable, output unavailable, reasoning unavailable) · average context unavailable · estimated subtotal USD 0.13\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text output lacks %q:\n%s", want, text)
		}
	}
	// Tokens and time together, in the order tt usage prints them.
	at := 0
	for _, want := range []string{
		"Synthetic item: measured · requests 52 · allocated 51.5 · tokens 6.90M (",
		"  time: wall 10m0s · model 50.0% · tool 25.0% · waiting 25.0% · polls 0 · ",
		"  timeline: some model working 50.0% · only tools running 25.0% · nobody active 25.0%\n",
		"  time phase build: model 4m0s · tool 2m30s · waiting 0s\n",
		"  time phase hand-offs: model 1m0s · tool 0s · waiting 2m30s\n",
		"  build: measured · requests 52 · allocated 51.5 · tokens 6.90M (",
		"  hand-offs: measured · requests 3 · allocated 3 · tokens 2.9k (input 2.5k, cached unavailable, cache write unavailable, output 400, reasoning unavailable) · average context unavailable\n",
		"Project overhead: measured · requests 1 · ",
		"  time: not measured\n",
	} {
		i := strings.Index(text[at:], want)
		if i < 0 {
			t.Fatalf("text output lacks %q after byte %d:\n%s", want, at, text)
		}
		at += i + len(want)
	}
	if strings.Contains(text, "map[") || regexp.MustCompile(`[0-9]/[0-9]`).MatchString(text) {
		t.Errorf("text output still prints a raw map or rational:\n%s", text)
	}
}

// tt usage warning against a real hub over HTTP (wi_3228104e700006c5, a9).
func TestUsageWarningCLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	by := api.Caller{Node: "fixture", User: "owner"}
	srv := httptest.NewServer(server.New(st, func(r *http.Request) (api.Caller, error) { return by, nil }))
	t.Cleanup(srv.Close)
	task, err := st.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Usage warning CLI synthetic"}, by)
	if err != nil {
		t.Fatal(err)
	}
	e := env{hub: srv.URL, task: task.ID}
	read := func(args ...string) api.UsageWarnings {
		t.Helper()
		text, err := captureStdout(t, func() error { return cmdUsage(e, append([]string{"warning"}, args...)) })
		if err != nil {
			t.Fatal(args, err)
		}
		var out api.UsageWarnings
		if err = json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatal(text, err)
		}
		return out
	}
	if out := read("get", "--json"); out.Threshold != "1.5" || !out.Default {
		t.Fatalf("default %+v", out)
	}
	text, err := captureStdout(t, func() error { return cmdUsage(e, []string{"warning", "set", "--threshold", "2"}) })
	if err != nil || text != "Warning level: 2× estimate\nNo warnings posted.\n" {
		t.Fatalf("set %q %v", text, err)
	}
	if out := read("get", "--json"); out.Threshold != "2" || out.Default || len(out.Warnings) != 0 {
		t.Fatalf("after set %+v", out)
	}
	if out := read("get", "--project", task.ID, "--json"); out.Threshold != "2" {
		t.Fatalf("explicit project %+v", out)
	}
	for _, bad := range [][]string{{"warning"}, {"warning", "show"}, {"warning", "set"}, {"warning", "get", "--threshold", "3"}, {"warning", "set", "--threshold", "abc"}, {"warning", "set", "--threshold", "0.5"}, {"warning", "get", "extra"}} {
		if _, err = captureStdout(t, func() error { return cmdUsage(e, bad) }); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
	}
	// An agent identity is named in the request, and only the owner helper's is accepted.
	worker, err := st.AddAgent(context.Background(), task.ID, api.AddAgentRequest{Name: "worker", Host: "fixture", Session: "synthetic", Runtime: "claude"}, by)
	if err != nil {
		t.Fatal(err)
	}
	asWorker := e
	asWorker.agent = worker.ID
	if _, err = captureStdout(t, func() error { return cmdUsage(asWorker, []string{"warning", "set", "--threshold", "3"}) }); err == nil || !strings.Contains(err.Error(), "only the owner helper sets the warning threshold") {
		t.Fatalf("a worker set the threshold: %v", err)
	}
	if out := read("get", "--json"); out.Threshold != "2" {
		t.Fatalf("a refused set changed the threshold: %+v", out)
	}
	at := time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)
	got := formatUsageWarnings(api.UsageWarnings{Threshold: "1.5", Default: true, Warnings: []api.UsageWarning{
		{ItemID: "wi_1111111111111111", EstimateTokens: 12000000, ActualTokens: "18360000", ActualState: "measured", Ratio: "153/100", Threshold: "1.5", LeadMessageSeq: 12, HelperMessageSeq: 13, At: at},
		{ItemID: "wi_2222222222222222", EstimateTokens: 1000, ActualTokens: "3001/2", ActualState: "partial", Ratio: "3001/2000", Threshold: "1.5", HelperMessageSeq: 14, At: at}}})
	want := "Warning level: 1.5× estimate (default)\n" +
		"  wi_1111111111111111: estimate 12.00M · lifetime actual 18.36M · 1.53× · level 1.5× · lead #12 · owner helper #13 · 2026-10-08T04:00:00Z\n" +
		"  wi_2222222222222222: estimate 1.0k · lifetime actual at least 1.5k · at least 1.50× · level 1.5× · no lead · owner helper #14 · 2026-10-08T04:00:00Z\n"
	if got != want {
		t.Fatalf("text\n%s\nwant\n%s", got, want)
	}
}
