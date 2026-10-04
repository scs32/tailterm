package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReleasesStrictPageAndDetailProtocol(t *testing.T) {
	task := "tsk_0123456789abcdef"
	summary := ReleaseSummary{Summary: true, RowID: 1, ID: "rel_0123456789abcdef", TaskID: task, State: "verified", Generation: 1}
	page := ReleasePage{Version: 1, Jobs: []ReleaseSummary{summary}, Page: ReleasePageInfo{View: "active", Limit: 50, Snapshot: strings.Repeat("a", 64)}}
	for _, bad := range []any{[]ReleaseJob{}, map[string]any{"version": 1}, map[string]any{"version": 1, "jobs": []any{}, "page": map[string]any{"view": "active", "limit": 50, "snapshot": page.Page.Snapshot}}, ReleasePage{Version: 2, Jobs: page.Jobs, Page: page.Page}, ReleasePage{Version: 1, Jobs: page.Jobs, Page: ReleasePageInfo{View: "settled", Limit: 50, Snapshot: page.Page.Snapshot}}} {
		b, _ := json.Marshal(bad)
		c := newResponseCapClient(t, 200, b)
		if _, err := c.ReleasesPage(context.Background(), task, ReleaseListOptions{}); err == nil {
			t.Fatal("accepted malformed/legacy page", string(b))
		}
	}
	b, _ := json.Marshal(page)
	c := newResponseCapClient(t, 200, b)
	if got, err := c.ReleasesPage(context.Background(), task, ReleaseListOptions{}); err != nil || len(got.Jobs) != 1 {
		t.Fatal(got, err)
	}
	if _, err := c.ReleasesPage(context.Background(), task, ReleaseListOptions{Snapshot: strings.Repeat("b", 64)}); err == nil {
		t.Fatal("changed snapshot")
	}
	// Production detail reads do not accept arrays or another job's detail.
	for _, bad := range []any{[]ReleaseJob{{ID: summary.ID}}, ReleaseJob{ID: "rel_other", TaskID: task}, summary} {
		b, _ := json.Marshal(bad)
		c := newResponseCapClient(t, 200, b)
		if _, err := c.Release(context.Background(), task, summary.ID); err == nil {
			t.Fatal("invalid detail accepted")
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("job") != summary.ID {
			t.Error("not exact detail", r.URL.String())
		}
		json.NewEncoder(w).Encode(ReleaseJob{ID: summary.ID, TaskID: task, Plan: VerificationPlan{Commit: "full"}, Receipt: &ReleaseReceipt{Version: 1, Targets: []ReleaseTargetReceipt{{Target: "hub", Backup: "full-backup"}}}})
	}))
	defer srv.Close()
	c = &Client{Base: srv.URL, HTTP: srv.Client(), Token: "fixture"}
	got, err := c.Release(context.Background(), task, summary.ID)
	if err != nil || got.Plan.Commit != "full" || got.Receipt.Targets[0].Backup != "full-backup" {
		t.Fatal(got, err)
	}
}
