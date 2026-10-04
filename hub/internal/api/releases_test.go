package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
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

func TestReleaseDeploymentCompatibilityFullHistoryAndDetail(t *testing.T) {
	task := "tsk_0123456789abcdef"
	for _, legacy := range []bool{true, false} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			jobs := []ReleaseJob{}
			for i := 0; i < 1010; i++ {
				state := "released"
				if i < 205 {
					state = "verified"
				}
				jobs = append(jobs, ReleaseJob{ID: fmt.Sprintf("rel_%016x", i), TaskID: task, State: state, Generation: 2, Commit: strings.Repeat("b", 40), VerificationDigest: strings.Repeat("c", 64), Plan: VerificationPlan{Commit: "full-plan"}, Receipt: &ReleaseReceipt{Version: 1, JobID: fmt.Sprintf("rel_%016x", i), Outcome: "released", Commit: strings.Repeat("b", 40), Targets: []ReleaseTargetReceipt{{Target: "hub", Outcome: "released", Backup: "full-backup", ArtifactSHA256: strings.Repeat("d", 64)}, {Target: "bridge", Outcome: "released", Backup: "paired-backup"}, {Target: "mini", Outcome: "released", Rollback: "retained-mini"}, {Target: "tailos", Outcome: "released", Rollback: "retained-tailos"}}}})
			}
			reads, posts := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				if r.Method != "GET" {
					posts++
					t.Error("compatibility posted")
				}
				if r.Header.Get("Authorization") != "Bearer fixture" {
					t.Error("authentication lost")
				}
				q := r.URL.Query()
				if legacy {
					json.NewEncoder(w).Encode(jobs)
					return
				}
				if id := q.Get("job"); id != "" {
					for _, j := range jobs {
						if j.ID == id {
							json.NewEncoder(w).Encode(j)
							return
						}
					}
					w.WriteHeader(404)
					return
				}
				view := q.Get("view")
				limit, _ := strconv.Atoi(q.Get("limit"))
				after, _ := strconv.Atoi(q.Get("after"))
				if q.Get("snapshot") != "" && q.Get("snapshot") != strings.Repeat("a", 64) {
					w.WriteHeader(409)
					return
				}
				page := ReleasePage{Version: 1, Jobs: []ReleaseSummary{}, Page: ReleasePageInfo{View: view, Limit: limit, Snapshot: strings.Repeat("a", 64)}}
				for i, j := range jobs {
					row := i + 1
					if row <= after || releaseSettled(j.State) != (view == "settled") {
						continue
					}
					if len(page.Jobs) == limit {
						page.Page.NextAfter = strconv.FormatInt(page.Jobs[len(page.Jobs)-1].RowID, 10)
						break
					}
					page.Jobs = append(page.Jobs, compatibilitySummary(j, int64(row)))
				}
				json.NewEncoder(w).Encode(page)
			}))
			defer server.Close()
			c := &Client{Base: server.URL, HTTP: server.Client(), Token: "fixture"}
			full, err := c.CompatibilityReleases(context.Background(), task)
			if err != nil || !reflect.DeepEqual(full, jobs) {
				t.Fatal("full history/detail lost", len(full), err)
			}
			detail, err := c.CompatibilityRelease(context.Background(), task, jobs[800].ID)
			if err != nil || detail.Receipt.Targets[0].Backup != "full-backup" || detail.Plan.Commit != "full-plan" {
				t.Fatal(detail, err)
			}
			page, err := c.CompatibilityReleasesPage(context.Background(), task, ReleaseListOptions{View: "active", Limit: 200})
			if err != nil || len(page.Jobs) != 200 || page.Page.NextAfter == "" {
				t.Fatal(page, err)
			}
			next, err := c.CompatibilityReleasesPage(context.Background(), task, ReleaseListOptions{View: "active", Limit: 200, Snapshot: page.Page.Snapshot, After: page.Page.NextAfter})
			if err != nil || len(next.Jobs) != 5 || next.Page.NextAfter != "" {
				t.Fatal(next, err)
			}
			if legacy {
				// Full evidence (not just compact pins) invalidates a legacy continuation.
				jobs[999].Receipt.Targets[0].Backup = "changed-heavy-evidence"
				if _, err = c.CompatibilityReleasesPage(context.Background(), task, ReleaseListOptions{View: "active", Limit: 200, Snapshot: page.Page.Snapshot, After: page.Page.NextAfter}); err == nil {
					t.Fatal("legacy evidence change accepted")
				}
				if _, err = c.ReleasesPage(context.Background(), task, ReleaseListOptions{}); err == nil {
					t.Fatal("ordinary protocol became permissive")
				}
			}
			if posts != 0 || reads < 5 {
				t.Fatal(reads, posts)
			}
		})
	}
}

func TestReleaseDeploymentCompatibilityHoldsAllReadFailures(t *testing.T) {
	task := "tsk_0123456789abcdef"
	j := ReleaseJob{ID: "rel_0123456789abcdef", TaskID: task, State: "claimed", Generation: 2}
	good, _ := json.Marshal([]ReleaseJob{j})
	for _, tc := range []struct {
		name   string
		status int
		body   []byte
	}{
		{"unauthorized", 401, good}, {"forbidden", 403, good}, {"server", 500, good}, {"partial-http", 206, good}, {"redirect", 302, good}, {"null", 200, []byte("null")}, {"empty", 200, nil}, {"malformed", 200, []byte("[")}, {"version", 200, []byte(`{"version":2,"jobs":[],"page":{}}`)}, {"compact", 200, []byte(`[{"id":"rel_0123456789abcdef","taskId":"tsk_0123456789abcdef","state":"claimed","generation":2,"summary":true}]`)}, {"duplicate", 200, append(append([]byte("["), good[1:len(good)-1]...), append([]byte(","), good[1:]...)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(tc.status); w.Write(tc.body) }))
			defer server.Close()
			c := &Client{Base: server.URL, HTTP: server.Client()}
			out, err := c.CompatibilityReleases(context.Background(), task)
			if err == nil || out != nil || calls != 1 {
				t.Fatal(out, err, calls)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat(" ", defaultMaxResponseBytes+1)))
	}))
	defer server.Close()
	c := &Client{Base: server.URL, HTTP: server.Client()}
	if out, err := c.CompatibilityReleases(context.Background(), task); err == nil || out != nil {
		t.Fatal("oversize", out, err)
	}
}

func TestReleaseDeploymentCompatibilityModernDetailBookend(t *testing.T) {
	task := "tsk_0123456789abcdef"
	job := ReleaseJob{ID: "rel_0123456789abcdef", TaskID: task, State: "claimed", Generation: 2, RunID: "run_original"}
	for _, fault := range []string{"run", "pause", "digest", "input", "compact", "bookend", "repeat", "cross-task"} {
		t.Run(fault, func(t *testing.T) {
			detailRead := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("job") != "" {
					detailRead = true
					copy := job
					switch fault {
					case "run":
						copy.RunID = "run_other"
					case "pause":
						copy.PauseGeneration++
					case "digest":
						copy.VerificationDigest = "changed"
					case "input":
						copy.InputsDigest = "changed"
					case "cross-task":
						copy.TaskID = "tsk_other"
					}
					if fault == "compact" {
						json.NewEncoder(w).Encode(compatibilitySummary(copy, 1))
					} else {
						json.NewEncoder(w).Encode(copy)
					}
					return
				}
				token := strings.Repeat("a", 64)
				if fault == "bookend" && detailRead {
					token = strings.Repeat("b", 64)
				}
				page := ReleasePage{Version: 1, Jobs: []ReleaseSummary{}, Page: ReleasePageInfo{View: q.Get("view"), Limit: 200, Snapshot: token}}
				if q.Get("view") == "active" {
					page.Jobs = append(page.Jobs, compatibilitySummary(job, 1))
					if fault == "repeat" {
						page.Page.NextAfter = "same"
					}
				}
				json.NewEncoder(w).Encode(page)
			}))
			defer server.Close()
			c := &Client{Base: server.URL, HTTP: server.Client()}
			out, err := c.CompatibilityReleases(context.Background(), task)
			if err == nil || out != nil {
				t.Fatal(fault, out, err)
			}
		})
	}
}
