package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The owner report tests run tt owner report against a fake hub that serves
// canned JSON for exactly the read routes the report uses. They never reach
// a live hub, live usage files, tailscale (--host is always given) or Discord.

const ownerReportTestTask = "tsk_0000000000000091"

type ownerReportFixture struct {
	settled       []api.ReleaseSummary
	active        []api.ReleaseSummary
	overviews     map[string]api.NarrativeOverview
	reports       map[string]api.NarrativeReportVersion // reportID/version
	queue         api.TeamQueueList
	interventions []api.Message
	usage         api.ProviderUsage
	tokens        map[string]string
	needs         []api.Obligation
	// status is the owner status call's answer; nil serves the zero status
	// of a project with nothing recorded.
	status *api.OwnerStatus
	wakes  api.OwnerHelperWakeList
	fail   map[string]bool // route name -> answer 500

	mu    sync.Mutex
	calls map[string]int
	// posts are the Board messages --post sent, in order; receipts maps a
	// request ID to its post, as the hub's post receipts do.
	posts    []api.PostMessageRequest
	receipts map[string]int
}

func (f *ownerReportFixture) count(route string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[route]++
}

func (f *ownerReportFixture) calledTimes(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[route]
}

func (f *ownerReportFixture) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(v any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
		route, payload := f.route(t, r)
		f.count(route)
		if route == "" {
			t.Errorf("owner report read an unexpected route: %s %s", r.Method, r.URL)
			http.Error(w, `{"error":"unexpected route"}`, http.StatusNotFound)
			return
		}
		if f.fail[route] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"synthetic failure"}`))
			return
		}
		if payload == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		if _, ok := payload.(ownerReportConflict); ok {
			http.Error(w, `{"error":"request ID was already used with different message data"}`, http.StatusConflict)
			return
		}
		write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ownerReportPostSeq numbers the fixture's posts from here.
const ownerReportPostSeq = 5000

func (f *ownerReportFixture) postedMessage(i int) api.Message {
	p := f.posts[i]
	seq := int64(ownerReportPostSeq + i)
	return api.Message{Seq: seq, TaskID: ownerReportTestTask, Text: p.Text,
		PostReceipt: &api.MessagePostReceipt{ID: fmt.Sprintf("mpr_%016d", i), RequestID: p.RequestID, TaskID: ownerReportTestTask, MessageSeq: seq}}
}

// post stores a Board message the way the hub does: the same request ID
// with the same text returns the first post, with other text it is refused.
func (f *ownerReportFixture) post(t *testing.T, r *http.Request) any {
	var req api.PostMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode post: %v", err)
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.receipts == nil {
		f.receipts = map[string]int{}
	}
	if i, ok := f.receipts[req.RequestID]; ok {
		if f.posts[i].Text != req.Text {
			return ownerReportConflict{}
		}
		return f.postedMessage(i)
	}
	f.posts = append(f.posts, req)
	f.receipts[req.RequestID] = len(f.posts) - 1
	return f.postedMessage(len(f.posts) - 1)
}

type ownerReportConflict struct{}

func (f *ownerReportFixture) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func (f *ownerReportFixture) route(t *testing.T, r *http.Request) (string, any) {
	q := r.URL.Query()
	prefix := "/v1/tasks/" + ownerReportTestTask
	path := r.URL.Path
	if r.Method == http.MethodPost && path == prefix+"/messages" {
		if f.fail["post"] {
			return "post", nil
		}
		return "post", f.post(t, r)
	}
	if r.Method != http.MethodGet {
		return "", nil
	}
	if id, ok := strings.CutPrefix(path, prefix+"/messages/receipts/"); ok {
		f.mu.Lock()
		defer f.mu.Unlock()
		i, found := f.receipts[id]
		if !found || f.posts[i].AgentID != q.Get("agentId") {
			return "receipt", nil
		}
		return "receipt", f.postedMessage(i)
	}
	switch {
	case path == "/v1/provider-usage":
		if q.Get("host") != f.usage.Host {
			t.Errorf("provider usage host = %q, want %q", q.Get("host"), f.usage.Host)
		}
		return "provider", f.usage
	case path == prefix+"/releases":
		view := q.Get("view")
		jobs := f.settled
		if view == "active" {
			jobs = f.active
		}
		return "releases-" + view, ownerReportReleasePage(view, q, jobs)
	case path == prefix+"/team-queue":
		if q.Get("view") != api.TeamQueueViewActive {
			t.Errorf("team queue view = %q, want active", q.Get("view"))
		}
		return "team-queue", f.queue
	case path == prefix+"/interventions":
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		out := api.InterventionList{Interventions: []api.Message{}}
		for _, m := range f.interventions {
			if m.Seq > after {
				out.Interventions = append(out.Interventions, m)
			}
		}
		return "interventions", out
	case path == prefix+"/usage":
		if q.Get("from") == "" || q.Get("to") == "" {
			t.Errorf("project usage read without a window: %s", r.URL.RawQuery)
		}
		return "usage", api.UsageReport{Version: 1, ProjectID: ownerReportTestTask, Summary: api.UsageSummary{State: "complete", Tokens: f.tokens}}
	case path == prefix+"/owner-status":
		if q.Get("from") == "" || q.Get("to") == "" || q.Get("tz") == "" {
			t.Errorf("owner status read without a window or zone: %s", r.URL.RawQuery)
		}
		if f.status == nil {
			return "owner-status", api.OwnerStatus{TaskID: ownerReportTestTask, Zone: q.Get("tz"), Teams: api.OwnerStatusTeams{Stalled: []api.OwnerStatusStall{}}}
		}
		return "owner-status", *f.status
	case path == prefix+"/owner-helper/wakes":
		return "wakes", f.wakes
	case path == prefix+"/obligations":
		if q.Get("owner") != "1" || q.Get("open") != "1" {
			t.Errorf("owner obligations query = %s", r.URL.RawQuery)
		}
		return "obligations", api.ObligationList{Obligations: f.needs}
	case strings.HasPrefix(path, prefix+"/work-items/"):
		rest := strings.TrimPrefix(path, prefix+"/work-items/")
		item, tail, _ := strings.Cut(rest, "/narrative")
		if tail == "" {
			o, ok := f.overviews[item]
			if !ok {
				return "narrative", nil
			}
			return "narrative", o
		}
		if strings.HasPrefix(tail, "/reports/") {
			key := strings.Replace(strings.TrimPrefix(tail, "/reports/"), "/versions/", "/", 1)
			rep, ok := f.reports[key]
			if !ok {
				return "report", nil
			}
			return "report", rep
		}
	}
	return "", nil
}

// ownerReportReleasePage pages jobs by rowId exactly as the hub's strict
// release page contract (api decodeReleasePage) requires.
func ownerReportReleasePage(view string, q map[string][]string, all []api.ReleaseSummary) map[string]any {
	limit := 50
	if v := first(q["limit"]); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	after, _ := strconv.ParseInt(first(q["after"]), 10, 64)
	jobs := []api.ReleaseSummary{}
	next := ""
	for _, j := range all {
		if j.RowID <= after {
			continue
		}
		if len(jobs) == limit {
			next = strconv.FormatInt(jobs[len(jobs)-1].RowID, 10)
			break
		}
		jobs = append(jobs, j)
	}
	return map[string]any{
		"version": 1,
		"jobs":    jobs,
		"page":    map[string]any{"view": view, "limit": limit, "snapshot": strings.Repeat("ab", 32), "nextAfter": next},
	}
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func ownerReportJob(row int64, id, item, state string, settled time.Time) api.ReleaseSummary {
	j := api.ReleaseSummary{Summary: true, RowID: row, ID: id, TaskID: ownerReportTestTask, ItemID: item, State: state}
	if !settled.IsZero() {
		j.SettledAt = settled.UTC().Format(time.RFC3339Nano)
	}
	if state == "released" {
		j.Receipt = &api.ReleaseSummaryReceipt{Outcome: "released", Targets: []api.ReleaseSummaryTarget{{Target: "hub", Outcome: "released"}}}
	}
	return j
}

func ownerReportOverview(item, title string, report string, version int64) api.NarrativeOverview {
	o := api.NarrativeOverview{Item: api.WorkItem{ID: item, TaskID: ownerReportTestTask, Kind: "bug", Title: title}}
	if report != "" {
		o.CompletionReport = &api.NarrativeReportPin{ReportID: report, Version: version}
	}
	return o
}

func numberRows(jobs []api.ReleaseSummary) []api.ReleaseSummary {
	for i := range jobs {
		jobs[i].RowID = int64(i + 1)
	}
	return jobs
}

// fullOwnerReportFixture fills every available section relative to now: one
// item shipped 30 minutes ago, one 3 hours ago (outside the hourly window
// only), one hand release 20 minutes ago without a completion report, and
// one long before every window.
func fullOwnerReportFixture(now time.Time) *ownerReportFixture {
	return &ownerReportFixture{
		settled: numberRows([]api.ReleaseSummary{
			ownerReportJob(0, "rel_0000000000000001", "wi_0000000000000000", "released", now.Add(-30*time.Hour)),
			ownerReportJob(0, "rel_0000000000000002", "wi_bbbbbbbbbbbbbbbb", "released", now.Add(-3*time.Hour)),
			ownerReportJob(0, "rel_0000000000000003", "wi_aaaaaaaaaaaaaaaa", "released", now.Add(-30*time.Minute)),
			ownerReportJob(0, "rel_0000000000000004", "wi_cccccccccccccccc", "superseded", now.Add(-20*time.Minute)),
		}),
		active: numberRows([]api.ReleaseSummary{
			ownerReportJob(0, "rel_0000000000000005", "wi_dddddddddddddddd", "verified", time.Time{}),
			ownerReportJob(0, "rel_0000000000000006", "wi_ffffffffffffffff", "claimed", time.Time{}),
		}),
		overviews: map[string]api.NarrativeOverview{
			"wi_0000000000000000": ownerReportOverview("wi_0000000000000000", "An old fix", "nar_0000000000000000", 1),
			"wi_aaaaaaaaaaaaaaaa": ownerReportOverview("wi_aaaaaaaaaaaaaaaa", "Owner report template", "nar_aaaaaaaaaaaaaaaa", 2),
			"wi_bbbbbbbbbbbbbbbb": ownerReportOverview("wi_bbbbbbbbbbbbbbbb", "Board pages load faster", "nar_bbbbbbbbbbbbbbbb", 1),
			"wi_cccccccccccccccc": ownerReportOverview("wi_cccccccccccccccc", "Retire the old relay flag", "", 0),
			"wi_dddddddddddddddd": ownerReportOverview("wi_dddddddddddddddd", "Queue shows up next", "", 0),
			"wi_eeeeeeeeeeeeeeee": ownerReportOverview("wi_eeeeeeeeeeeeeeee", "Host health figures", "", 0),
			"wi_ffffffffffffffff": ownerReportOverview("wi_ffffffffffffffff", "Retry partial launches", "", 0),
			"wi_gggggggggggggggg": ownerReportOverview("wi_gggggggggggggggg", "Shorter shipped lines", "", 0),
		},
		reports: map[string]api.NarrativeReportVersion{
			"nar_aaaaaaaaaaaaaaaa/2": {ReportID: "nar_aaaaaaaaaaaaaaaa", Version: 2, Sections: api.NarrativeReportSections{
				DeliveredWork: "The product renders the owner report\n  from one fixed template. Every model and person gets the same bytes, section by section.",
				Limitations:   "Host health waits for its data. Opened vs closed waits too.",
			}},
			"nar_bbbbbbbbbbbbbbbb/1": {ReportID: "nar_bbbbbbbbbbbbbbbb", Version: 1, Sections: api.NarrativeReportSections{
				DeliveredWork: "Board pages open in half the time",
				Limitations:   "",
			}},
		},
		queue: api.TeamQueueList{ConcurrencyLimit: 4, Entries: []api.TeamQueueEntry{
			{ID: "tqe_1", ItemID: "wi_dddddddddddddddd", Template: "planned", State: "running", Position: 5},
			{ID: "tqe_2", ItemID: "wi_eeeeeeeeeeeeeeee", Template: "small", State: "launching", Position: 3},
			{ID: "tqe_3", ItemID: "wi_ffffffffffffffff", Template: "small", State: "queued", Position: 7, UpNext: 2},
			{ID: "tqe_4", ItemID: "wi_gggggggggggggggg", Template: "planned", State: "queued", Position: 6, UpNext: 1},
		}},
		interventions: []api.Message{
			{Seq: 700, TaskID: ownerReportTestTask, CreatedAt: now.Add(-48 * time.Hour), Intervention: &api.Intervention{Kind: "redirect", ItemID: "wi_0000000000000000", Actor: "owner"}},
			{Seq: 900, TaskID: ownerReportTestTask, CreatedAt: now.Add(-10 * time.Minute), Intervention: &api.Intervention{Kind: "unblock", ItemID: "wi_dddddddddddddddd", Actor: "helper"}},
		},
		usage: api.ProviderUsage{Host: "test-host", Readings: []api.ProviderReading{
			{Runtime: "codex", Window: "seven_day", State: "missing", ReportedAt: now.Add(-time.Minute).Format(time.RFC3339)},
			{Runtime: "claude", Window: "seven_day", State: "ok", UsedPercent: "61.5", ResetsAt: now.Add(75 * time.Hour).Format(time.RFC3339), CapturedAt: now.Add(-7 * time.Minute).Format(time.RFC3339)},
			{Runtime: "claude", Window: "five_hour", State: "ok", UsedPercent: "42", ResetsAt: now.Add(2 * time.Hour).Format(time.RFC3339), CapturedAt: now.Add(-7 * time.Minute).Format(time.RFC3339)},
		}},
		tokens: map[string]string{"output": "1200", "input": "34000", "cached": "91000/2"},
		needs: []api.Obligation{
			{ID: "obl_0000000000000002", MessageSeq: 910, SourceKind: "question", Subject: "Approve the  bridge\nrestart", Needs: "answer", DueAt: now.Add(15 * time.Minute)},
			{ID: "obl_0000000000000001", MessageSeq: 880, SourceKind: "request", Needs: "result", DueAt: now.Add(-40 * time.Minute), Overdue: "outcome"},
		},
		status: ownerReportStatus(now),
	}
}

// ownerReportStatus is a full owner status: a stored baseline at 21:00 UTC
// on Oct 30, a host census read four minutes before now and one stalled
// entry. The fake hub serves it for every window.
func ownerReportStatus(now time.Time) *api.OwnerStatus {
	five, free, pressure, waiters := 5, int64(15360), 64, 0
	return &api.OwnerStatus{TaskID: ownerReportTestTask, Zone: "UTC",
		Releases: api.OwnerStatusReleases{ReleasedSinceBaseline: &five},
		Items: api.OwnerStatusItems{Opened: api.OwnerStatusCounts{Bugs: 2, Features: 1}, Closed: api.OwnerStatusCounts{Bugs: 3},
			OpenNow: api.OwnerStatusCounts{Bugs: 40, Features: 12}, NetSinceBaseline: &api.OwnerStatusCounts{Bugs: -1, Features: 1}},
		Baseline: &api.OwnerStatusBaseline{Day: "2026-10-30", At: time.Date(2026, 10, 30, 21, 0, 0, 0, time.UTC), Zone: "UTC", ReleasedTotal: 300, Open: api.OwnerStatusCounts{Bugs: 41, Features: 11}},
		Host: &api.OwnerStatusHost{Host: "mini", ObservedAt: now.Add(-4 * time.Minute).Format(time.RFC3339), FreeDiskMiB: &free, DiskReserveMiB: 8192,
			MemoryPressure: &pressure, StrainPressure: 70, MatrixWaiters: &waiters},
		Teams: api.OwnerStatusTeams{Running: 2, Limit: 4, Stalled: []api.OwnerStatusStall{{EntryID: "tqe_3", ItemID: "wi_ffffffffffffffff", Cause: "idle-entry"}}},
	}
}

func ownerReportEnv(srv *httptest.Server) env {
	return env{hub: srv.URL, task: ownerReportTestTask, token: "test-token"}
}

func runOwnerReport(t *testing.T, e env, args ...string) (string, error) {
	t.Helper()
	return captureCLIOutput(t, func() error { return cmdOwnerReport(e, args) })
}

func ownerReportArgs(kind string, now time.Time) []string {
	return []string{kind, "--now", now.Format(time.RFC3339), "--tz", "UTC", "--host", "test-host"}
}

func assertGolden(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("report differs from golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

const ownerReportGoldenHourly = `**Owner report: Hourly** 2026-10-31 13:00–14:00 UTC
**Shipped** (2)
- Owner report template: The product renders the owner report from one fixed template. Limits: Host health waits for its data.
- Retire the old relay flag: no saved completion report
**Releases**
- released 1, rolled back 0, refused 0, superseded 1 in this window; in line now: 2
- since the Oct 30 21:00 baseline: released 5
**Running teams** (2 of 4)
- wi_eeeeeeeeeeeeeeee Host health figures (small, launching)
- wi_dddddddddddddddd Queue shows up next (planned, running)
**Up next** (2)
- 1. wi_gggggggggggggggg Shorter shipped lines (planned)
- 2. wi_ffffffffffffffff Retry partial launches (small)
**What went wrong** (1)
- intervention #900 unblock on wi_dddddddddddddddd by helper
**Opened vs closed**
- opened 2 bugs, 1 features; closed 3 bugs, 0 features in this window
- open now: 40 bugs, 12 features (since the Oct 30 21:00 baseline: -1 bugs, +1 features)
**Usage**
- claude five_hour: 42% used, resets Sat 16:00 (reading 7m old)
- claude seven_day: 61.5% used, resets Tue 17:00 (reading 7m old)
- codex seven_day: missing
- project tokens in window: cached 91000/2, input 34000, output 1200
**Host health**
- mini: 15.0 GiB free (reserve 8.0 GiB), memory pressure 64, matrix waiters 0 (reading 4m old)
- queue: admitting
- stalled: wi_ffffffffffffffff idle-entry
**Needs you** (2)
- #880 request: result (due 13:20, overdue outcome)
- #910 question: Approve the bridge restart (due 14:15)
`

func TestOwnerReportGoldenHourly(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	f := fullOwnerReportFixture(now)
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("hourly", now)...)
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	assertGolden(t, out, ownerReportGoldenHourly)
	// One narrative read per distinct item across Shipped and Running.
	if got := f.calledTimes("narrative"); got != 6 {
		t.Fatalf("narrative overview reads = %d, want 6 (two shipped, two running, two up next)", got)
	}
}

const ownerReportGoldenMorning = `**Owner report: Morning** 2026-10-30 21:00–2026-10-31 07:00 UTC
**Shipped** (3)
- Board pages load faster: Board pages open in half the time. Limits: none recorded
- Owner report template: The product renders the owner report from one fixed template. Limits: Host health waits for its data.
- Retire the old relay flag: no saved completion report
**Releases**
- released 2, rolled back 0, refused 0, superseded 1 in this window; in line now: 2
- since the Oct 30 21:00 baseline: released 5
**Running teams** (2 of 4)
- wi_eeeeeeeeeeeeeeee Host health figures (small, launching)
- wi_dddddddddddddddd Queue shows up next (planned, running)
**Up next** (2)
- 1. wi_gggggggggggggggg Shorter shipped lines (planned)
- 2. wi_ffffffffffffffff Retry partial launches (small)
**What went wrong** (1)
- intervention #900 unblock on wi_dddddddddddddddd by helper
**Opened vs closed**
- opened 2 bugs, 1 features; closed 3 bugs, 0 features in this window
- open now: 40 bugs, 12 features (since the Oct 30 21:00 baseline: -1 bugs, +1 features)
**Usage**
- claude five_hour: 42% used, resets Sat 09:00 (reading 7m old)
- claude seven_day: 61.5% used, resets Tue 10:00 (reading 7m old)
- codex seven_day: missing
- project tokens in window: cached 91000/2, input 34000, output 1200
**Host health**
- mini: 15.0 GiB free (reserve 8.0 GiB), memory pressure 64, matrix waiters 0 (reading 4m old)
- queue: admitting
- stalled: wi_ffffffffffffffff idle-entry
**Needs you** (2)
- #880 request: result (due 06:20, overdue outcome)
- #910 question: Approve the bridge restart (due 07:15)
`

func TestOwnerReportGoldenMorning(t *testing.T) {
	now := time.Date(2026, 10, 31, 7, 0, 0, 0, time.UTC)
	f := fullOwnerReportFixture(now)
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("morning", now)...)
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	assertGolden(t, out, ownerReportGoldenMorning)
}

const ownerReportGoldenEvening = `**Owner report: Evening** 2026-10-31 07:00–21:00 UTC
**Shipped** (3)
- Board pages load faster: Board pages open in half the time. Limits: none recorded
- Owner report template: The product renders the owner report from one fixed template. Limits: Host health waits for its data.
- Retire the old relay flag: no saved completion report
**Releases**
- released 2, rolled back 0, refused 0, superseded 1 in this window; in line now: 2
- since the Oct 30 21:00 baseline: released 5
**Running teams** (2 of 4)
- wi_eeeeeeeeeeeeeeee Host health figures (small, launching)
- wi_dddddddddddddddd Queue shows up next (planned, running)
**Up next** (2)
- 1. wi_gggggggggggggggg Shorter shipped lines (planned)
- 2. wi_ffffffffffffffff Retry partial launches (small)
**What went wrong** (1)
- intervention #900 unblock on wi_dddddddddddddddd by helper
**Opened vs closed**
- opened 2 bugs, 1 features; closed 3 bugs, 0 features in this window
- open now: 40 bugs, 12 features (since the Oct 30 21:00 baseline: -1 bugs, +1 features)
**Usage**
- claude five_hour: 42% used, resets Sat 23:00 (reading 7m old)
- claude seven_day: 61.5% used, resets Wed 00:00 (reading 7m old)
- codex seven_day: missing
- project tokens in window: cached 91000/2, input 34000, output 1200
**Host health**
- mini: 15.0 GiB free (reserve 8.0 GiB), memory pressure 64, matrix waiters 0 (reading 4m old)
- queue: admitting
- stalled: wi_ffffffffffffffff idle-entry
**Needs you** (2)
- #880 request: result (due 20:20, overdue outcome)
- #910 question: Approve the bridge restart (due 21:15)
`

func TestOwnerReportGoldenEvening(t *testing.T) {
	now := time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC)
	f := fullOwnerReportFixture(now)
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("evening", now)...)
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	assertGolden(t, out, ownerReportGoldenEvening)
}

// ownerReportHeaders are the nine sections, in their fixed order.
var ownerReportHeaders = []string{"**Shipped**", "**Releases**", "**Running teams**", "**Up next**", "**What went wrong**", "**Opened vs closed**", "**Usage**", "**Host health**", "**Needs you**"}

func assertOwnerReportHeaders(t *testing.T, out string) {
	t.Helper()
	at := 0
	for _, h := range ownerReportHeaders {
		i := strings.Index(out[at:], "\n"+h)
		if i < 0 {
			t.Fatalf("header %s missing or out of order in\n%s", h, out)
		}
		at += i + 1
	}
}

func TestOwnerReportGoldensKeepEverySectionAndFixedLines(t *testing.T) {
	for name, golden := range map[string]string{"hourly": ownerReportGoldenHourly, "morning": ownerReportGoldenMorning, "evening": ownerReportGoldenEvening} {
		assertOwnerReportHeaders(t, golden)
		// Opened vs closed and Host health render the hub's figures; the
		// old placeholder is gone (wi_475a3df9b345e13e f4).
		if strings.Contains(golden, "not yet available") || !strings.Contains(golden, "**Opened vs closed**\n- opened ") || !strings.Contains(golden, "**Host health**\n- mini: ") {
			t.Fatalf("%s golden: Opened vs closed and Host health must show hub figures", name)
		}
		if !strings.Contains(golden, "**Up next** (2)\n- 1. ") {
			t.Fatalf("%s golden: Up next must list the queue", name)
		}
	}
}

const ownerReportGoldenEmpty = `**Owner report: Hourly** 2026-10-31 13:00–14:00 UTC
**Shipped** (0)
- none in this window
**Releases**
- released 0, rolled back 0, refused 0, superseded 0 in this window; in line now: 0
**Running teams** (0 of 4)
- none running
**Up next** (0)
- nothing queued
**What went wrong** (0)
- none recorded (sources: refused or rolled-back releases, owner interventions)
**Opened vs closed**
- opened 0 bugs, 0 features; closed 0 bugs, 0 features in this window
- open now: 0 bugs, 0 features (no 21:00 baseline yet)
**Usage**
- no provider reading for host test-host
- project tokens in window: none recorded
**Host health**
- no host reading
- queue: admitting
**Needs you** (0)
- nothing waiting on you
`

func TestOwnerReportNoShippedItems(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	f := &ownerReportFixture{
		// Settled long before the window, and a runner refusal with no
		// settled time: neither belongs to this window.
		settled: numberRows([]api.ReleaseSummary{
			ownerReportJob(0, "rel_0000000000000001", "wi_0000000000000000", "released", now.Add(-5*time.Hour)),
			ownerReportJob(0, "rel_0000000000000002", "wi_0000000000000000", "refused", time.Time{}),
		}),
		active: []api.ReleaseSummary{},
		queue: api.TeamQueueList{ConcurrencyLimit: 4, Entries: []api.TeamQueueEntry{
			{ID: "tqe_3", ItemID: "wi_ffffffffffffffff", Template: "small", State: "queued", Position: 7},
		}},
		interventions: []api.Message{
			{Seq: 700, TaskID: ownerReportTestTask, CreatedAt: now.Add(-2 * time.Hour), Intervention: &api.Intervention{Kind: "redirect", ItemID: "wi_0000000000000000"}},
		},
		usage: api.ProviderUsage{Host: "test-host", Readings: []api.ProviderReading{}},
	}
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("hourly", now)...)
	if err != nil {
		t.Fatalf("empty report must exit 0: %v\n%s", err, out)
	}
	assertGolden(t, out, ownerReportGoldenEmpty)
	assertOwnerReportHeaders(t, out)

	t.Run("released item without a completion report", func(t *testing.T) {
		f := &ownerReportFixture{
			settled: numberRows([]api.ReleaseSummary{
				ownerReportJob(0, "rel_0000000000000003", "wi_cccccccccccccccc", "released", now.Add(-10*time.Minute)),
			}),
			active:    []api.ReleaseSummary{},
			overviews: map[string]api.NarrativeOverview{"wi_cccccccccccccccc": ownerReportOverview("wi_cccccccccccccccc", "Retire the old relay flag", "", 0)},
			usage:     api.ProviderUsage{Host: "test-host"},
		}
		out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("hourly", now)...)
		if err != nil {
			t.Fatalf("report: %v\n%s", err, out)
		}
		if !strings.Contains(out, "**Shipped** (1)\n- Retire the old relay flag: no saved completion report\n") {
			t.Fatalf("missing the no-completion-report line:\n%s", out)
		}
		if f.calledTimes("report") != 0 {
			t.Fatal("read a report version for an item with no completion pin")
		}
	})
}

func ownerReportSection(t *testing.T, out, header, next string) string {
	t.Helper()
	i := strings.Index(out, "\n"+header)
	j := strings.Index(out, "\n"+next)
	if i < 0 || j < i {
		t.Fatalf("cannot find section %s in\n%s", header, out)
	}
	body := out[i+1 : j+1]
	_, rest, _ := strings.Cut(body, "\n")
	return rest
}

func TestOwnerReportWhatWentWrong(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	refused := func(row int64, id string, at time.Time) api.ReleaseSummary {
		j := ownerReportJob(row, id, "wi_rrrrrrrrrrrrrrrr", "refused", at)
		j.Refusal = &api.ReleaseRefusal{Step: "preflight", Reason: "candidate_unverified"}
		return j
	}
	rolledBack := func(row int64, id string, at time.Time) api.ReleaseSummary {
		j := ownerReportJob(row, id, "wi_bbbbbbbbbbbbbbbb", "rolled_back", at)
		j.Receipt = &api.ReleaseSummaryReceipt{Outcome: "rolled_back", Failure: &api.ReleaseFailure{Step: "live_check", Target: "hub", Reason: "live_commit_mismatch"}, Targets: []api.ReleaseSummaryTarget{}}
		return j
	}
	f := &ownerReportFixture{
		settled: numberRows([]api.ReleaseSummary{
			refused(0, "rel_0000000000000001", now.Add(-2*time.Hour)),
			rolledBack(0, "rel_0000000000000002", now.Add(-90*time.Minute)),
			rolledBack(0, "rel_0000000000000003", now.Add(-40*time.Minute)),
			refused(0, "rel_0000000000000004", now.Add(-15*time.Minute)),
			// Settled exactly at the window's end: outside [start, end).
			refused(0, "rel_0000000000000005", now),
		}),
		active: []api.ReleaseSummary{},
		interventions: []api.Message{
			{Seq: 600, TaskID: ownerReportTestTask, CreatedAt: now.Add(-3 * time.Hour), Intervention: &api.Intervention{Kind: "redirect", ItemID: "wi_0000000000000000", Actor: "owner"}},
			{Seq: 800, TaskID: ownerReportTestTask, CreatedAt: now.Add(-25 * time.Minute), Intervention: &api.Intervention{Kind: "restart", ItemID: "wi_dddddddddddddddd"}},
		},
		usage: api.ProviderUsage{Host: "test-host"},
	}
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("hourly", now)...)
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	want := `- release rel_0000000000000003 for wi_bbbbbbbbbbbbbbbb rolled back at live_check: live_commit_mismatch
- intervention #800 restart on wi_dddddddddddddddd by unknown
- release rel_0000000000000004 for wi_rrrrrrrrrrrrrrrr refused at preflight: candidate_unverified
`
	if got := ownerReportSection(t, out, "**What went wrong** (3)", "**Opened vs closed**"); got != want {
		t.Fatalf("what went wrong:\n%s\nwant:\n%s\nfull report:\n%s", got, want, out)
	}
	for _, outside := range []string{"rel_0000000000000001", "rel_0000000000000002", "rel_0000000000000005", "#600"} {
		if strings.Contains(out, outside) {
			t.Fatalf("%s is outside the window but appears:\n%s", outside, out)
		}
	}
	if !strings.Contains(out, "- released 0, rolled back 1, refused 1, superseded 0 in this window;") {
		t.Fatalf("release counts:\n%s", out)
	}
}

func TestOwnerReportSameOutputAnySession(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	srv := fullOwnerReportFixture(now).serve(t)
	cases := []struct {
		name, envName, envValue, agent string
	}{
		{"claude agent", "CLAUDE_CODE_SESSION_ID", "11111111-2222-3333-4444-555555555555", "agt_0000000000000001"},
		{"codex agent", "CODEX_THREAD_ID", "019a0000-0000-7000-8000-000000000000", "agt_0000000000000002"},
		{"owner terminal", "", "", ""},
		{"claude owner", "CLAUDE_CODE_SESSION_ID", "66666666-7777-8888-9999-000000000000", ""},
		{"codex owner", "CODEX_THREAD_ID", "019a1111-0000-7000-8000-000000000000", ""},
	}
	var outputs []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_SESSION_ID", "")
			t.Setenv("CODEX_THREAD_ID", "")
			if tc.envName != "" {
				t.Setenv(tc.envName, tc.envValue)
			}
			e := ownerReportEnv(srv)
			e.agent, e.runID = tc.agent, ""
			if tc.agent != "" {
				e.agentName, e.runID = "owner-helper", "run_0000000000000001"
			}
			out, err := runOwnerReport(t, e, ownerReportArgs("hourly", now)...)
			if err != nil {
				t.Fatalf("%s refused or failed: %v\n%s", tc.name, err, out)
			}
			outputs = append(outputs, out)
		})
	}
	if len(outputs) != len(cases) {
		t.Fatalf("ran %d of %d sessions", len(outputs), len(cases))
	}
	for i, out := range outputs {
		if out != outputs[0] {
			t.Fatalf("%s output differs from %s:\n%s\n---\n%s", cases[i].name, cases[0].name, out, outputs[0])
		}
	}
	assertGolden(t, outputs[0], ownerReportGoldenHourly)
}

func TestOwnerReportUnavailableSection(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	f := fullOwnerReportFixture(now)
	f.fail = map[string]bool{"provider": true}
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("hourly", now)...)
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("a failed read must return an error, got %v", err)
	}
	assertOwnerReportHeaders(t, out)
	readings := "- claude five_hour: 42% used, resets Sat 16:00 (reading 7m old)\n- claude seven_day: 61.5% used, resets Tue 17:00 (reading 7m old)\n- codex seven_day: missing\n"
	if !strings.Contains(ownerReportGoldenHourly, readings) {
		t.Fatal("test setup: readings are not in the hourly golden")
	}
	want := strings.Replace(ownerReportGoldenHourly, readings, "- unavailable: could not read provider usage\n", 1)
	assertGolden(t, out, want)

	t.Run("team queue", func(t *testing.T) {
		f := fullOwnerReportFixture(now)
		f.fail = map[string]bool{"team-queue": true}
		out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("hourly", now)...)
		if err == nil {
			t.Fatal("a failed team queue read must return an error")
		}
		want := "**Running teams**\n- unavailable: could not read the team queue\n**Up next**\n- unavailable: could not read the team queue\n**What went wrong**"
		if !strings.Contains(out, want) {
			t.Fatalf("running teams and up next must both read unavailable:\n%s", out)
		}
	})
}

func TestOwnerReportDispatch(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	e := ownerReportEnv(fullOwnerReportFixture(now).serve(t))
	e.agent, e.agentName, e.runID = "agt_0000000000000001", "owner-helper", "run_0000000000000001"
	out, err := captureCLIOutput(t, func() error {
		return dispatchOwner(e, append([]string{"report"}, ownerReportArgs("hourly", now)...))
	})
	if err != nil {
		t.Fatalf("agent session report: %v\n%s", err, out)
	}
	assertGolden(t, out, ownerReportGoldenHourly)

	err = dispatchOwner(e, []string{"extend", "obl_0000000000000001", "--for", "30m"})
	if err == nil || !strings.Contains(err.Error(), "tt owner is for the owner") {
		t.Fatalf("agent extend must still be refused, got %v", err)
	}
	synopsis := "  owner report hourly|morning|evening [--task ID] [--tz ZONE] [--host H] [--now T] [--since T] [--post]  the owner's progress report from one fixed template (docs/owner-report.md)\n"
	if !strings.Contains(usage, synopsis) {
		t.Fatalf("tt usage lacks the owner report synopsis line")
	}
}

func TestOwnerReportRejectsBadArguments(t *testing.T) {
	e := env{hub: "http://127.0.0.1:1", task: ownerReportTestTask}
	for _, args := range [][]string{
		{},
		{"weekly"},
		{"hourly", "extra"},
		{"hourly", "--now", "yesterday", "--host", "h"},
		{"hourly", "--tz", "Not/AZone", "--host", "h"},
		{"hourly", "--now", "2026-10-31T14:00:00Z", "--since", "2026-10-31T15:00:00Z", "--host", "h"},
	} {
		if _, err := runOwnerReport(t, e, args...); err == nil {
			t.Fatalf("args %q: want an error before any hub read", args)
		}
	}
}

func TestOwnerReportWindows(t *testing.T) {
	zone, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no zoneinfo for America/Los_Angeles")
	}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, zone)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct{ kind, now, start string }{
		{"hourly", "2026-10-31 14:30", "2026-10-31 13:30"},
		{"morning", "2026-10-31 07:00", "2026-10-30 21:00"},
		{"morning", "2026-10-31 21:00", "2026-10-30 21:00"}, // strictly before now
		{"morning", "2026-10-31 21:01", "2026-10-31 21:00"},
		{"evening", "2026-10-31 21:00", "2026-10-31 07:00"},
		{"evening", "2026-10-31 07:00", "2026-10-31 07:00"}, // at or before now
		{"evening", "2026-10-31 06:59", "2026-10-30 07:00"},
	} {
		if got := ownerReportWindow(tc.kind, at(tc.now), zone); !got.Equal(at(tc.start)) {
			t.Fatalf("%s at %s: start %s, want %s", tc.kind, tc.now, got.In(zone), tc.start)
		}
	}
}

func TestOwnerReportSinceOverridesWindow(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	f := fullOwnerReportFixture(now)
	args := append(ownerReportArgs("hourly", now), "--since", now.Add(-4*time.Hour).Format(time.RFC3339))
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), args...)
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "**Owner report: Hourly** 2026-10-31 10:00–14:00 UTC\n**Shipped** (3)\n") {
		t.Fatalf("--since window:\n%s", out)
	}
}

func TestOwnerReportTextIsNormalised(t *testing.T) {
	long := strings.Repeat("word ", 100)
	got := reportText(" a\n\tb  " + long)
	if !strings.HasPrefix(got, "a b word") || !strings.HasSuffix(got, "…") || len([]rune(got)) != ownerReportTextLimit+1 {
		t.Fatalf("normalised text %q (%d runes)", got, len([]rune(got)))
	}
}

// TestOwnerReportSettledPaging walks 600 settled rows over three pages of
// 200 (plan risk R3) and counts only the in-window ones on the last page.
func TestOwnerReportSettledPaging(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	var jobs []api.ReleaseSummary
	for i := 0; i < 598; i++ {
		jobs = append(jobs, ownerReportJob(0, fmt.Sprintf("rel_%016d", i+1), "wi_0000000000000000", "released", now.Add(-48*time.Hour)))
	}
	jobs = append(jobs,
		ownerReportJob(0, "rel_9000000000000001", "wi_aaaaaaaaaaaaaaaa", "released", now.Add(-5*time.Minute)),
		ownerReportJob(0, "rel_9000000000000002", "wi_aaaaaaaaaaaaaaaa", "released", now.Add(-4*time.Minute)),
	)
	f := fullOwnerReportFixture(now)
	f.settled = numberRows(jobs)
	started := time.Now()
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("hourly", now)...)
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	if got := f.calledTimes("releases-settled"); got != 3 {
		t.Fatalf("settled pages read = %d, want 3", got)
	}
	if !strings.Contains(out, "**Shipped** (1)\n- Owner report template:") || !strings.Contains(out, "- released 2, rolled back 0") {
		t.Fatalf("paged report:\n%s", out)
	}
	t.Logf("600 settled rows over 3 pages rendered in %s against the fake hub", time.Since(started).Round(time.Millisecond))
}

// TestOwnerReportDocMatchesTemplate keeps docs/owner-report.md in step with
// the renderer: the synopsis, the nine headers in order, every fixed line and
// every unavailable source.
func TestOwnerReportDocMatchesTemplate(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "owner-report.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	_, template, ok := strings.Cut(doc, "## Template")
	if !ok {
		t.Fatal("docs/owner-report.md has no Template section")
	}
	assertOwnerReportHeaders(t, template)
	for _, want := range []string{
		"tt owner report hourly|morning|evening [--task ID] [--tz ZONE] [--host H] [--now T] [--since T] [--post]",
		ownerReportNothingQueued, ownerReportNoHostReading, ownerReportNoBaseline, ownerReportSeeReport, ownerSourceOwnerStatus,
		"- opened <n> bugs, <n> features; closed <n> bugs, <n> features in this window", "- queue: admitting", "- queue: held: <hold>", "- stalled: <item id> <cause>",
		"- since the <Mon D> 21:00 baseline: released <n>", "GET /v1/tasks/{id}/owner-status", "## Owner status call", ownerReportNoShipped, ownerReportNoRunning, ownerReportNothingWrong, ownerReportNothingWaiting,
		ownerReportNoCompletion, "`" + ownerReportNoneRecorded + "`", "- no provider reading for host <host>",
		"- project tokens in window: none recorded", "- unavailable: could not read <source>",
		ownerSourceReleases, ownerSourceTeamQueue, ownerSourceInterventions, ownerSourceProvider,
		ownerSourceProjectUsage, ownerSourceObligations, ownerSourceNarrative,
		"## Windows", "## Exit status", "wi_35e9", "wi_475a", "wi_546279658420371a",
		"## Sending on the hub schedule", "tt owner report hourly --post", "tt owner report morning --post", "tt owner report evening --post",
		"--kind hourly-update --schedule '0 * * * *'", "--kind morning-summary --schedule '0 7 * * *'", "--kind evening-summary --schedule '0 21 * * *'",
		"wi_3b7daaaab7d0f188", "- <n>. <item id> <title> (<template>)",
		fmt.Sprintf("%d runes", ownerReportDeliveredLimit), fmt.Sprintf("%d runes", ownerReportLimitsLimit),
	} {
		if !strings.Contains(doc, want) {
			t.Fatalf("docs/owner-report.md lacks %q", want)
		}
	}
}

// TestOwnerReportUpNext lists only queued entries with an up-next number,
// in that order, whatever their queue position or listing order (f1a).
func TestOwnerReportUpNext(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	f := fullOwnerReportFixture(now)
	f.queue = api.TeamQueueList{ConcurrencyLimit: 3, Entries: []api.TeamQueueEntry{
		{ID: "tqe_9", ItemID: "wi_ffffffffffffffff", Template: "small", State: "queued", Position: 2, UpNext: 3},
		{ID: "tqe_1", ItemID: "wi_dddddddddddddddd", Template: "planned", State: "running", Position: 1},
		{ID: "tqe_7", ItemID: "wi_hhhhhhhhhhhhhhhh", Template: "small", State: "queued", Position: 9, UpNext: 2},
		{ID: "tqe_8", ItemID: "wi_gggggggggggggggg", Template: "planned", State: "queued", Position: 8, UpNext: 1},
	}}
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("hourly", now)...)
	// wi_hhhh has no narrative: listed without its title, and the read fails.
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("a failed narrative read must return an error, got %v", err)
	}
	want := `- 1. wi_gggggggggggggggg Shorter shipped lines (planned)
- 2. wi_hhhhhhhhhhhhhhhh (small)
- 3. wi_ffffffffffffffff Retry partial launches (small)
`
	if got := ownerReportSection(t, out, "**Up next** (3)", "**What went wrong**"); got != want {
		t.Fatalf("up next:\n%s\nwant:\n%s\nfull report:\n%s", got, want, out)
	}
	if got := ownerReportSection(t, out, "**Running teams** (1 of 3)", "**Up next**"); got != "- wi_dddddddddddddddd Queue shows up next (planned, running)\n" {
		t.Fatalf("running teams:\n%s", got)
	}
}

// TestOwnerReportShippedSummary prints the first sentence of each saved
// field, cut at its fixed limit, instead of the whole paragraph (f1c).
func TestOwnerReportShippedSummary(t *testing.T) {
	long := strings.Repeat("word ", 60)
	for _, tc := range []struct {
		in    string
		limit int
		want  string
	}{
		{"", 10, ""},
		{"One sentence", 40, "One sentence"},
		{"First one.  Second\none.", 40, "First one."},
		{"Is it done? Yes.", 40, "Is it done?"},
		{"Ships now! Later more.", 40, "Ships now!"},
		{"Edits owner_report.go and v1.2 docs. Then more.", 80, "Edits owner_report.go and v1.2 docs."},
		{long, 20, "word word word word…"},
		{"ééééé ééééé. x", 7, "ééééé é…"},
	} {
		if got := reportSummary(tc.in, tc.limit); got != tc.want {
			t.Fatalf("reportSummary(%q, %d) = %q, want %q", tc.in, tc.limit, got, tc.want)
		}
	}

	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	f := fullOwnerReportFixture(now)
	f.reports["nar_aaaaaaaaaaaaaaaa/2"] = api.NarrativeReportVersion{ReportID: "nar_aaaaaaaaaaaaaaaa", Version: 2, Sections: api.NarrativeReportSections{
		DeliveredWork: strings.Repeat("The renderer ", 20) + "works. A second sentence the owner never sees.",
		Limitations:   strings.Repeat("A limit ", 30) + ". Another limit.",
	}}
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("hourly", now)...)
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	delivered := strings.TrimRight(string([]rune(strings.Repeat("The renderer ", 20))[:ownerReportDeliveredLimit]), " ") + "…"
	limits := strings.TrimRight(string([]rune(strings.Repeat("A limit ", 30))[:ownerReportLimitsLimit]), " ") + "…"
	want := "- Owner report template: " + delivered + ". Limits: " + limits + "\n"
	if !strings.Contains(out, want) {
		t.Fatalf("shipped summary line missing\nwant: %s\nreport:\n%s", want, out)
	}
	if strings.Contains(out, "second sentence") || strings.Contains(out, "Another limit") {
		t.Fatalf("summary printed past the first sentence:\n%s", out)
	}
}

// TestOwnerReportPostSlots aligns the window end to the kind's slot at or
// before now, in the report's zone (f1b).
func TestOwnerReportPostSlots(t *testing.T) {
	for _, zoneName := range []string{"America/Los_Angeles", "Asia/Kolkata"} {
		zone, err := time.LoadLocation(zoneName)
		if err != nil {
			t.Skipf("no zoneinfo for %s", zoneName)
		}
		at := func(s string) time.Time {
			v, err := time.ParseInLocation("2006-01-02 15:04:05", s, zone)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		for _, tc := range []struct{ kind, now, slot string }{
			{"hourly", "2026-10-31 14:00:00", "2026-10-31 14:00:00"},
			{"hourly", "2026-10-31 14:59:59", "2026-10-31 14:00:00"},
			{"hourly", "2026-10-31 00:07:00", "2026-10-31 00:00:00"},
			{"morning", "2026-10-31 07:00:00", "2026-10-31 07:00:00"},
			{"morning", "2026-10-31 07:20:00", "2026-10-31 07:00:00"},
			{"morning", "2026-10-31 06:59:00", "2026-10-30 07:00:00"},
			{"evening", "2026-10-31 21:00:00", "2026-10-31 21:00:00"},
			{"evening", "2026-10-31 23:30:00", "2026-10-31 21:00:00"},
			{"evening", "2026-10-31 20:59:00", "2026-10-30 21:00:00"},
		} {
			if got := ownerReportSlot(tc.kind, at(tc.now), zone); !got.Equal(at(tc.slot)) {
				t.Fatalf("%s %s at %s: slot %s, want %s", zoneName, tc.kind, tc.now, got.In(zone), tc.slot)
			}
		}
	}
	// The fall-back hour in Pacific time: 01:30 PST (the second 01:30) is
	// in the hour that starts at 01:00 PST, one hour after 01:00 PDT.
	zone, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no zoneinfo for America/Los_Angeles")
	}
	pst := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC) // 01:30 PST
	if got, want := ownerReportSlot("hourly", pst, zone), time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("fall-back hourly slot %s, want %s", got.UTC(), want)
	}
}

func splitPostedReport(t *testing.T, out string) (report, posted string) {
	t.Helper()
	i := strings.LastIndex(strings.TrimRight(out, "\n"), "\n")
	if i < 0 {
		t.Fatalf("no post line in\n%s", out)
	}
	return out[:i+1], out[i+1:]
}

// TestOwnerReportPostOncePerSlot posts the printed bytes once per slot as
// the caller; a rerun of the slot, even with moved data, finds the first
// post, and the next slot posts again (f1b).
func TestOwnerReportPostOncePerSlot(t *testing.T) {
	slot := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	f := fullOwnerReportFixture(slot)
	e := ownerReportEnv(f.serve(t))
	e.agent, e.agentName, e.runID = "agt_0000000000000001", "owner-helper", "run_0000000000000001"
	postArgs := func(now time.Time) []string { return append(ownerReportArgs("hourly", now), "--post") }

	// Read 25 minutes late: the window is still exactly the 14:00 slot.
	out, err := runOwnerReport(t, e, postArgs(slot.Add(25*time.Minute))...)
	if err != nil {
		t.Fatalf("post: %v\n%s", err, out)
	}
	report, posted := splitPostedReport(t, out)
	assertGolden(t, report, ownerReportGoldenHourly)
	if posted != fmt.Sprintf("posted #%d\n", ownerReportPostSeq) {
		t.Fatalf("post line %q", posted)
	}
	if f.postCount() != 1 {
		t.Fatalf("posts = %d, want 1", f.postCount())
	}
	p := f.posts[0]
	if p.Text != ownerReportGoldenHourly || p.To != "" || p.ReplyTo != 0 || p.Envelope != nil {
		t.Fatalf("posted message is not the plain undirected report: %+v", p)
	}
	if p.AgentID != e.agent || p.RunID != e.runID || !strings.HasPrefix(p.RequestID, "owner-report-hourly-") {
		t.Fatalf("posted identity: agent %q run %q request %q", p.AgentID, p.RunID, p.RequestID)
	}

	// The same slot again, after the data moved: no second post.
	f.needs = nil
	out, err = runOwnerReport(t, e, postArgs(slot.Add(50*time.Minute))...)
	if err != nil {
		t.Fatalf("repost: %v\n%s", err, out)
	}
	if _, posted := splitPostedReport(t, out); posted != fmt.Sprintf("already posted #%d for this slot\n", ownerReportPostSeq) {
		t.Fatalf("rerun post line %q", posted)
	}
	if f.postCount() != 1 {
		t.Fatalf("a rerun of the slot posted again: %d posts", f.postCount())
	}

	// The next slot posts a new message.
	out, err = runOwnerReport(t, e, postArgs(slot.Add(65*time.Minute))...)
	if err != nil {
		t.Fatalf("next slot: %v\n%s", err, out)
	}
	report, posted = splitPostedReport(t, out)
	if !strings.HasPrefix(report, "**Owner report: Hourly** 2026-10-31 14:00–15:00 UTC\n") || posted != fmt.Sprintf("posted #%d\n", ownerReportPostSeq+1) {
		t.Fatalf("next slot:\n%s", out)
	}
	if f.postCount() != 2 || f.posts[1].Text != report || f.posts[1].RequestID == p.RequestID {
		t.Fatalf("next slot posts: %+v", f.posts)
	}

	// Without --post nothing is posted (the fixture refuses any other route).
	if _, err := runOwnerReport(t, e, ownerReportArgs("hourly", slot.Add(65*time.Minute))...); err != nil || f.postCount() != 2 {
		t.Fatalf("plain report posted or failed: %v, %d posts", err, f.postCount())
	}
}

// TestOwnerReportPostSourceFailure still posts a report with an
// unavailable section and exits 1; a refused post is an error.
func TestOwnerReportPostSourceFailure(t *testing.T) {
	slot := time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC)
	f := fullOwnerReportFixture(slot)
	f.fail = map[string]bool{"provider": true}
	e := ownerReportEnv(f.serve(t))
	out, err := runOwnerReport(t, e, append(ownerReportArgs("evening", slot.Add(3*time.Minute)), "--post")...)
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("a failed read must still exit 1, got %v", err)
	}
	report, posted := splitPostedReport(t, out)
	if f.postCount() != 1 || f.posts[0].Text != report || !strings.Contains(report, "- unavailable: could not read provider usage\n") || posted != fmt.Sprintf("posted #%d\n", ownerReportPostSeq) {
		t.Fatalf("report with an unavailable section was not posted as printed:\n%s", out)
	}
	if !strings.HasPrefix(report, "**Owner report: Evening** 2026-10-31 07:00–21:00 UTC\n") {
		t.Fatalf("evening slot window:\n%s", report)
	}

	f2 := fullOwnerReportFixture(slot)
	f2.fail = map[string]bool{"post": true}
	_, err = runOwnerReport(t, ownerReportEnv(f2.serve(t)), append(ownerReportArgs("morning", slot), "--post")...)
	if err == nil || !strings.Contains(err.Error(), "the report was not posted") {
		t.Fatalf("a refused post must fail, got %v", err)
	}
}

// TestOwnerReportHubFigures renders the owner status call's figures under
// Opened vs closed, Host health and Releases (wi_475a3df9b345e13e f4): no
// baseline yet, unobserved readings, a strained host, a lane hold and
// several stalled entries.
func TestOwnerReportHubFigures(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	f := fullOwnerReportFixture(now)
	f.status = ownerReportStatus(now)
	f.status.Baseline, f.status.Releases.ReleasedSinceBaseline, f.status.Items.NetSinceBaseline = nil, nil, nil
	f.status.Items = api.OwnerStatusItems{Opened: api.OwnerStatusCounts{Features: 4}, Closed: api.OwnerStatusCounts{Bugs: 1, Features: 2}, OpenNow: api.OwnerStatusCounts{Bugs: 7}}
	pressure := 83
	f.status.Host = &api.OwnerStatusHost{Host: "mini", DiskReserveMiB: 10240, MemoryPressure: &pressure, StrainPressure: 70, Strained: true}
	f.status.Teams.Hold = "a matrix run waits for the verification host"
	f.status.Teams.Stalled = append(f.status.Teams.Stalled, api.OwnerStatusStall{EntryID: "tqe_9", ItemID: "wi_9999999999999999", Cause: "failed-entry"})
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("hourly", now)...)
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	if got, want := ownerReportSection(t, out, "**Opened vs closed**", "**Usage**"),
		"- opened 0 bugs, 4 features; closed 1 bugs, 2 features in this window\n- open now: 7 bugs, 0 features (no 21:00 baseline yet)\n"; got != want {
		t.Fatalf("opened vs closed:\n%s\nwant:\n%s", got, want)
	}
	if got, want := ownerReportSection(t, out, "**Host health**", "**Needs you**"),
		"- mini: free disk not observed (reserve 10.0 GiB), memory pressure 83 (strained, over 70), matrix waiters not observed\n"+
			"- queue: held: a matrix run waits for the verification host\n- stalled: wi_ffffffffffffffff idle-entry\n- stalled: wi_9999999999999999 failed-entry\n"; got != want {
		t.Fatalf("host health:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(out, "since the") {
		t.Fatalf("a baseline line without a baseline:\n%s", out)
	}
	if got := f.calledTimes("owner-status"); got != 1 {
		t.Fatalf("owner status reads = %d, want one call", got)
	}
}

// TestOwnerReportStatusUnavailable: a failed owner status read shows the
// unavailable line under Opened vs closed and Host health, omits the
// baseline line and exits 1; every other section prints as usual.
func TestOwnerReportStatusUnavailable(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	f := fullOwnerReportFixture(now)
	f.fail = map[string]bool{"owner-status": true}
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("hourly", now)...)
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("a failed owner status read must exit 1, got %v", err)
	}
	want := ownerReportGoldenHourly
	for old, repl := range map[string]string{
		"- since the Oct 30 21:00 baseline: released 5\n": "",
		"**Opened vs closed**\n- opened 2 bugs, 1 features; closed 3 bugs, 0 features in this window\n- open now: 40 bugs, 12 features (since the Oct 30 21:00 baseline: -1 bugs, +1 features)\n": "**Opened vs closed**\n- unavailable: could not read the owner status\n",
		"**Host health**\n- mini: 15.0 GiB free (reserve 8.0 GiB), memory pressure 64, matrix waiters 0 (reading 4m old)\n- queue: admitting\n- stalled: wi_ffffffffffffffff idle-entry\n":        "**Host health**\n- unavailable: could not read the owner status\n",
	} {
		if !strings.Contains(want, old) {
			t.Fatalf("test setup: %q is not in the hourly golden", old)
		}
		want = strings.Replace(want, old, repl, 1)
	}
	assertGolden(t, out, want)
}

// TestOwnerReportPlainShipped: the Shipped line is the title and the first
// plain sentence of the delivered work; provenance (message numbers, commit
// SHAs, branch names, worktree paths) never prints (wi_475a3df9b345e13e f8).
func TestOwnerReportPlainShipped(t *testing.T) {
	for _, tc := range []struct {
		text  string
		plain bool
	}{
		{"Owners see the report in one place.", true},
		{"Edits owner_report.go and v1.2 docs in 2026.", true},
		{"Added three decades of deadbeef caches.", true},
		{"Follows order #55055.", false},
		{"Saved under REQUEST54571.", false},
		{"Landed 05f1a040 today.", false},
		{"Landed05f1a040 today.", false},
		{"Merged 298f54d0cedabf6be328c462107faa1ac8693b01.", false},
		{"Built in queue-475a3df9.", false},
		{"Item wi_475a3df9b345e13e.", false},
		{"Pushed to origin/main.", false},
		{"Rebased on Tasks-Hub.", false},
		{"From /Users/x/.build/worktrees/q.", false},
		{"Committed on branch wi_475a-owner-status.", false},
		{"Built on fix-wi_8237-pathless-wait.", false},
		{"Built under order 4335.", false},
		{"Answers request #812 and seq 2131.", false},
		{"Added 1245 tests in 2026 for 30 orders.", true},
	} {
		if got := reportPlain(tc.text); got != tc.plain {
			t.Fatalf("reportPlain(%q) = %v, want %v", tc.text, got, tc.plain)
		}
	}

	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	f := fullOwnerReportFixture(now)
	f.reports["nar_aaaaaaaaaaaaaaaa/2"] = api.NarrativeReportVersion{ReportID: "nar_aaaaaaaaaaaaaaaa", Version: 2, Sections: api.NarrativeReportSections{
		DeliveredWork: "Landed 05f1a040 on tasks-hub under order #55055 from /Users/x/.build/worktrees/queue-475a3df9. The owner report now shows open and closed counts. Verified by REQUEST54571.",
		Limitations:   "Receipt rel_ab12cd34ef56 is pending. Swap and load are not shown yet.",
	}}
	f.overviews["wi_cccccccccccccccc"] = ownerReportOverview("wi_cccccccccccccccc", "Retire the old relay flag", "nar_cccccccccccccccc", 1)
	f.reports["nar_cccccccccccccccc/1"] = api.NarrativeReportVersion{ReportID: "nar_cccccccccccccccc", Version: 1, Sections: api.NarrativeReportSections{
		DeliveredWork: "Merged 298f54d0 from branch wi_cccc-relay. See #54000.",
		Limitations:   "Reverted in 7310f81c.",
	}}
	out, err := runOwnerReport(t, ownerReportEnv(f.serve(t)), ownerReportArgs("hourly", now)...)
	if err != nil {
		t.Fatalf("report: %v\n%s", err, out)
	}
	want := "**Shipped** (2)\n" +
		"- Owner report template: The owner report now shows open and closed counts. Limits: Swap and load are not shown yet.\n" +
		"- Retire the old relay flag. Limits: see the completion report\n"
	if !strings.Contains(out, want) {
		t.Fatalf("shipped lines\nwant:\n%s\nreport:\n%s", want, out)
	}
	shipped := ownerReportSection(t, out, "**Shipped** (2)", "**Releases**")
	for _, banned := range []string{"#", "05f1a040", "298f54d0", "tasks-hub", "/", "queue-475a3df9", "wi_", "REQUEST"} {
		if strings.Contains(shipped, banned) {
			t.Fatalf("shipped section prints %q:\n%s", banned, shipped)
		}
	}
}

// TestOwnerReportPostRepliesToWake: an agent's --post replies to its own
// wake notice for the slot, which the hub's ack gate lets through; another
// kind's wake, another agent's firing, another slot and the owner post
// without a reply (wi_475a3df9b345e13e g1/g2).
func TestOwnerReportPostRepliesToWake(t *testing.T) {
	slot := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	const helper = "agt_0000000000000001"
	firing := func(due time.Time, notice int64, to string) api.OwnerHelperWakeFiring {
		return api.OwnerHelperWakeFiring{DueAt: due, Outcome: api.OwnerHelperWakePosted, NoticeSeq: notice, RecipientID: to}
	}
	wakes := api.OwnerHelperWakeList{TaskID: ownerReportTestTask, Wakes: []api.OwnerHelperWake{
		{ID: "ohw_0000000000000001", Kind: "hourly-update", Firings: []api.OwnerHelperWakeFiring{
			firing(slot.Add(time.Hour), 7300, helper), firing(slot, 7200, helper), firing(slot.Add(-time.Hour), 7100, helper)}},
		{ID: "ohw_0000000000000002", Kind: "queue-check", Firings: []api.OwnerHelperWakeFiring{firing(slot, 7250, helper)}},
		{ID: "ohw_0000000000000003", Kind: "morning-summary", Firings: []api.OwnerHelperWakeFiring{firing(slot, 7260, "agt_0000000000000009")}},
	}}
	run := func(t *testing.T, agent string, kind string, now time.Time, w api.OwnerHelperWakeList) api.PostMessageRequest {
		t.Helper()
		f := fullOwnerReportFixture(slot)
		f.wakes = w
		e := ownerReportEnv(f.serve(t))
		if agent != "" {
			e.agent, e.agentName, e.runID = agent, "owner-helper", "run_0000000000000001"
		}
		out, err := runOwnerReport(t, e, append(ownerReportArgs(kind, now), "--post")...)
		if err != nil || f.postCount() != 1 {
			t.Fatalf("post: %v (%d posts)\n%s", err, f.postCount(), out)
		}
		if agent == "" && f.calledTimes("wakes") != 0 {
			t.Fatal("the owner's post read the wakes")
		}
		return f.posts[0]
	}
	if p := run(t, helper, "hourly", slot.Add(25*time.Minute), wakes); p.ReplyTo != 7200 || p.Text != ownerReportGoldenHourly {
		t.Fatalf("the slot's report did not reply to its wake notice: reply %d", p.ReplyTo)
	}
	if p := run(t, helper, "hourly", slot.Add(-35*time.Minute), wakes); p.ReplyTo != 7100 {
		t.Fatalf("the earlier slot replied to %d, want 7100", p.ReplyTo)
	}
	if p := run(t, "agt_0000000000000002", "hourly", slot.Add(5*time.Minute), wakes); p.ReplyTo != 0 {
		t.Fatalf("another agent replied to the helper's wake: %d", p.ReplyTo)
	}
	if p := run(t, helper, "morning", time.Date(2026, 10, 31, 7, 10, 0, 0, time.UTC), wakes); p.ReplyTo != 0 {
		t.Fatalf("a slot with no own firing replied to %d", p.ReplyTo)
	}
	if p := run(t, "", "hourly", slot.Add(5*time.Minute), wakes); p.ReplyTo != 0 {
		t.Fatalf("the owner's post replied to %d", p.ReplyTo)
	}
	if p := run(t, helper, "hourly", slot.Add(5*time.Minute), api.OwnerHelperWakeList{}); p.ReplyTo != 0 {
		t.Fatalf("no wakes: replied to %d", p.ReplyTo)
	}
}
