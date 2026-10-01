package main

import (
	"encoding/json"
	"github.com/scs32/tailterm/hub/internal/api"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
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
	report := api.UsageReport{Version: 1, ProjectID: "tsk_1111111111111111", PriceRevision: 3, Items: []api.UsageItemReport{{TaskID: "tsk_1111111111111111", ItemID: "wi_1111111111111111", Title: "Synthetic item", Summary: build,
		Phases: []api.UsageGroup{{Key: "build", Label: "build", Summary: build}}, Roles: []api.UsageGroup{{Key: "builder", Label: "builder", Summary: api.UsageSummary{State: "unavailable", AllocatedTurns: "0", Tokens: map[string]string{}}}}}},
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
	if strings.Contains(text, "map[") || regexp.MustCompile(`[0-9]/[0-9]`).MatchString(text) {
		t.Errorf("text output still prints a raw map or rational:\n%s", text)
	}
}
