package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// staleLedgerHub is a modern-protocol hub whose snapshot token follows the
// ledger, as the store's does: a read carrying an outdated token is refused
// with the store's own 409 body. mutate runs before each request is served.
type staleLedgerHub struct {
	task                               string
	jobs                               []ReleaseJob
	mutate                             func(h *staleLedgerHub, r *http.Request)
	lists, first, stale, details, seen int
}

func (h *staleLedgerHub) token() string {
	sum := sha256.New()
	for i, j := range h.jobs {
		fmt.Fprintf(sum, "%d:%s:%d:%s\n", i+1, j.ID, j.Generation, j.State)
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func (h *staleLedgerHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.seen++
	if h.mutate != nil {
		h.mutate(h, r)
	}
	q := r.URL.Query()
	if id := q.Get("job"); id != "" {
		h.details++
		for _, j := range h.jobs {
			if j.ID == id {
				json.NewEncoder(w).Encode(j)
				return
			}
		}
		w.WriteHeader(404)
		return
	}
	h.lists++
	if q.Get("snapshot") == "" {
		h.first++
	} else if q.Get("snapshot") != h.token() {
		h.stale++
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(ErrorResponse{Error: "conflict: release: stale list snapshot"})
		return
	}
	view := q.Get("view")
	limit, _ := strconv.Atoi(q.Get("limit"))
	after, _ := strconv.Atoi(q.Get("after"))
	page := ReleasePage{Version: 1, Jobs: []ReleaseSummary{}, Page: ReleasePageInfo{View: view, Limit: limit, Snapshot: h.token()}}
	for i, j := range h.jobs {
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
}

func newStaleLedgerHub(t *testing.T, active, settled int) (*staleLedgerHub, *Client) {
	t.Helper()
	h := &staleLedgerHub{task: "tsk_0123456789abcdef"}
	for i := 0; i < active+settled; i++ {
		state := "verified"
		if i >= active {
			state = "released"
		}
		h.jobs = append(h.jobs, ReleaseJob{ID: fmt.Sprintf("rel_%016x", i), TaskID: h.task, State: state, Generation: 1, Plan: VerificationPlan{Commit: "full-plan"}})
	}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return h, &Client{Base: server.URL, HTTP: server.Client()}
}

// settle moves the first active job to the settled view, as a release does.
func (h *staleLedgerHub) settle() {
	for i := range h.jobs {
		if !releaseSettled(h.jobs[i].State) {
			h.jobs[i].State = "released"
			h.jobs[i].Generation++
			return
		}
	}
}

func TestReleaseDeploymentCompatibilityRetriesStaleSnapshot(t *testing.T) {
	// The ledger changes once, just before the numbered request of the first
	// attempt, and the read then succeeds on its second attempt.
	for _, tc := range []struct {
		name            string
		active, settled int
		changeBefore    int
		lists, details  int
	}{
		// 1 active page; stale on the settled view's first page.
		{"between-views", 3, 4, 2, 1 + 1 + 3, 7},
		// 2 active pages; stale on the active view's second page.
		{"later-page", 205, 4, 2, 1 + 1 + 4, 209},
		// stale on the bookend, after both views and every detail were read.
		{"bookend", 3, 4, 2 + 7 + 1, 3 + 3, 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, c := newStaleLedgerHub(t, tc.active, tc.settled)
			h.mutate = func(h *staleLedgerHub, _ *http.Request) {
				if h.seen == tc.changeBefore {
					h.settle()
				}
			}
			out, err := c.CompatibilityReleases(context.Background(), h.task)
			if err != nil {
				t.Fatal("stale snapshot not retried:", err)
			}
			// One consistent snapshot: exactly the ledger after the change, in
			// row order, so the moved job is neither duplicated nor missing.
			if !reflect.DeepEqual(out, h.jobs) {
				t.Fatalf("mixed snapshots: got %d jobs, want %d", len(out), len(h.jobs))
			}
			if out[0].State != "released" || out[0].Generation != 2 {
				t.Fatal("moved job kept its earlier state", out[0].State, out[0].Generation)
			}
			if h.first != 2 || h.stale != 1 || h.lists != tc.lists || h.details != tc.details {
				t.Fatalf("first=%d stale=%d lists=%d details=%d", h.first, h.stale, h.lists, h.details)
			}
		})
	}
	t.Run("persistent-churn", func(t *testing.T) {
		h, c := newStaleLedgerHub(t, 3, 4)
		h.mutate = func(h *staleLedgerHub, r *http.Request) {
			if r.URL.Query().Get("snapshot") != "" {
				h.jobs[0].Generation++
			}
		}
		out, err := c.CompatibilityReleases(context.Background(), h.task)
		if err == nil || out != nil {
			t.Fatal("persistent churn accepted", out, err)
		}
		if err.Error() != "release list kept changing: stale list snapshot on each of 3 attempts: hub: 409 deployment compatibility read refused" {
			t.Fatal("unclear error:", err)
		}
		var refused *HTTPError
		if !errors.As(err, &refused) || refused.Status != 409 {
			t.Fatal("hub refusal lost:", err)
		}
		if h.first != 3 || h.stale != 3 || h.lists != 6 || h.details != 0 {
			t.Fatalf("first=%d stale=%d lists=%d details=%d", h.first, h.stale, h.lists, h.details)
		}
	})
	t.Run("exact-release", func(t *testing.T) {
		h, c := newStaleLedgerHub(t, 3, 4)
		h.mutate = func(h *staleLedgerHub, _ *http.Request) {
			if h.seen == 2 {
				h.settle()
			}
		}
		got, err := c.CompatibilityRelease(context.Background(), h.task, h.jobs[0].ID)
		if err != nil || !reflect.DeepEqual(got, h.jobs[0]) || got.State != "released" || h.first != 2 || h.details != 1 {
			t.Fatal(got, err, h.first, h.details)
		}
	})
}

func TestReleaseDeploymentCompatibilityRetriesOnlyStaleSnapshot(t *testing.T) {
	refusal := "hub: 409 deployment compatibility read refused"
	want := map[string]string{"other-conflict": refusal, "bare-conflict": refusal, "server": "hub: 500", "repeat": "repeating release cursor", "continuation": "invalid release continuation", "detail": "release detail changed", "legacy-changed": "legacy release ledger changed", "token": "invalid release page"}
	for _, fault := range []string{"other-conflict", "bare-conflict", "server", "repeat", "continuation", "detail", "legacy-changed", "token"} {
		t.Run(fault, func(t *testing.T) {
			h, c := newStaleLedgerHub(t, 3, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				second := q.Get("snapshot") != ""
				switch {
				case fault == "other-conflict" && second:
					h.lists++
					w.WriteHeader(409)
					json.NewEncoder(w).Encode(ErrorResponse{Error: "conflict: release: another refusal"})
				case fault == "bare-conflict" && second:
					h.lists++
					w.WriteHeader(409)
				case fault == "server" && second:
					h.lists++
					w.WriteHeader(500)
					json.NewEncoder(w).Encode(ErrorResponse{Error: "conflict: release: stale list snapshot"})
				case fault == "legacy-changed" && second:
					h.lists++
					json.NewEncoder(w).Encode(h.jobs)
				case fault == "token" && second:
					h.lists++
					json.NewEncoder(w).Encode(ReleasePage{Version: 1, Jobs: []ReleaseSummary{}, Page: ReleasePageInfo{View: q.Get("view"), Limit: 200, Snapshot: strings.Repeat("b", 64)}})
				case fault == "repeat" && q.Get("job") == "" && q.Get("view") == "settled":
					h.lists++
					json.NewEncoder(w).Encode(ReleasePage{Version: 1, Jobs: []ReleaseSummary{compatibilitySummary(h.jobs[3+h.lists-2], int64(3+h.lists-1))}, Page: ReleasePageInfo{View: "settled", Limit: 200, Snapshot: h.token(), NextAfter: "same" + strconv.Itoa(h.lists%2)}})
				case fault == "continuation" && q.Get("job") == "" && q.Get("view") == "settled":
					h.lists++
					json.NewEncoder(w).Encode(ReleasePage{Version: 1, Jobs: []ReleaseSummary{compatibilitySummary(h.jobs[3], 4)}, Page: ReleasePageInfo{View: "settled", Limit: 200, Snapshot: h.token(), NextAfter: "next" + strconv.Itoa(h.lists)}})
				case fault == "detail" && q.Get("job") != "":
					h.details++
					copy := h.jobs[0]
					copy.Generation++
					json.NewEncoder(w).Encode(copy)
				default:
					h.ServeHTTP(w, r)
				}
			}))
			defer server.Close()
			c.Base, c.HTTP = server.URL, server.Client()
			out, err := c.CompatibilityReleases(context.Background(), h.task)
			if err == nil || out != nil {
				t.Fatal(fault, out, err)
			}
			if !strings.Contains(err.Error(), want[fault]) || strings.Contains(err.Error(), "attempts") || h.first != 1 {
				t.Fatalf("%s was retried: first=%d err=%v", fault, h.first, err)
			}
		})
	}
	t.Run("cancelled", func(t *testing.T) {
		h, c := newStaleLedgerHub(t, 3, 4)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h.mutate = func(h *staleLedgerHub, r *http.Request) {
			if r.URL.Query().Get("snapshot") != "" {
				h.jobs[0].Generation++
				cancel()
			}
		}
		out, err := c.CompatibilityReleases(ctx, h.task)
		if !errors.Is(err, context.Canceled) || out != nil || h.first != 1 {
			t.Fatal(out, err, h.first)
		}
	})
	// The explicit --snapshot page read reports the refusal exactly as before.
	t.Run("explicit-snapshot-page", func(t *testing.T) {
		h, c := newStaleLedgerHub(t, 3, 4)
		page, err := c.CompatibilityReleasesPage(context.Background(), h.task, ReleaseListOptions{View: "active", Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		h.settle()
		_, err = c.CompatibilityReleasesPage(context.Background(), h.task, ReleaseListOptions{View: "settled", Limit: 200, Snapshot: page.Page.Snapshot})
		refused, ok := err.(*HTTPError)
		if !ok || refused.Status != 409 || err.Error() != "hub: 409 deployment compatibility read refused" || h.lists != 2 || h.stale != 1 {
			t.Fatal(err, h.lists, h.stale)
		}
	})
}
