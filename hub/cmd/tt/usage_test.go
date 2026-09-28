package main

import (
	"encoding/json"
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http"
	"net/http/httptest"
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
