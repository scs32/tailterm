package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// captureHome gives the test its own HOME and returns the capture file path
// under it. It fails the test if that path, the capture command or the Claude
// settings would resolve inside the home of the user running the tests: no
// test here may open the real state file or the real ~/.claude/settings.json.
func captureHome(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	paths, err := newHostPaths()
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.SplitList(os.Getenv(testRealHomesEnv))
	for what, path := range map[string]string{"the capture file": claudeUsageCapturePath(), "the capture command": claudeUsageScriptPath(), "the Claude settings": paths.claudeSettings} {
		if !pathWithinAny(path, []string{home}) || (pathWithinAny(path, real) && !pathWithinAny(home, []string{os.TempDir()})) {
			t.Fatalf("%s resolves to %s, outside this test's home %s", what, path, home)
		}
	}
	return claudeUsageCapturePath()
}

// writeCapture writes the capture file as the status line command would.
func writeCapture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// captureJSON is a whole capture file with the given capture time, five-hour
// percentage and reset.
func captureJSON(captured int64, used float64, resets int64) string {
	return fmt.Sprintf(`{"capturedAt":%d,"rate_limits":{"five_hour":{"used_percentage":%v,"resets_at":%d},"seven_day":{"used_percentage":37,"resets_at":%d}},"version":"2.1.292"}`, captured, used, resets, resets+500000)
}

// a7: fresh, missing, unreadable and malformed capture files each return
// their named state, and a reset window is parsed.
func TestReadClaudeUsageCaptureStates(t *testing.T) {
	path := captureHome(t)
	if got := readClaudeUsageCapture(path); got.State != api.ProviderUsageMissing || len(got.Windows) != 0 {
		t.Fatalf("no file: %+v", got)
	}
	writeCapture(t, path, captureJSON(1791460000, 13, 1791470000))
	got := readClaudeUsageCapture(path)
	want := []api.ProviderUsageWindow{{Window: api.UsageWindowFiveHour, UsedPercent: 13, ResetsAt: time.Unix(1791470000, 0).UTC().Format(time.RFC3339)}, {Window: api.UsageWindowSevenDay, UsedPercent: 37, ResetsAt: time.Unix(1791970000, 0).UTC().Format(time.RFC3339)}}
	if got.State != api.ProviderUsageOK || !got.CapturedAt.Equal(time.Unix(1791460000, 0)) || got.Version != "2.1.292" || fmt.Sprint(got.Windows) != fmt.Sprint(want) {
		t.Fatalf("fresh file: %+v", got)
	}
	report := got.report("mini")
	if report.Host != "mini" || report.Runtime != "claude" || report.State != api.ProviderUsageOK || report.CapturedAt != time.Unix(1791460000, 0).UTC().Format(time.RFC3339) || len(report.Windows) != 2 {
		t.Fatalf("report %+v", report)
	}
	// A window that has just reset: nothing used and a new reset time.
	writeCapture(t, path, captureJSON(1791470100, 0, 1791488100))
	if got := readClaudeUsageCapture(path); got.State != api.ProviderUsageOK || got.Windows[0].UsedPercent != 0 || got.Windows[0].ResetsAt != time.Unix(1791488100, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("reset window: %+v", got)
	}
	// A fractional percentage and only one window are a whole reading.
	writeCapture(t, path, `{"capturedAt":1791460000,"rate_limits":{"five_hour":{"used_percentage":12.5,"resets_at":1791470000}}}`)
	if got := readClaudeUsageCapture(path); got.State != api.ProviderUsageOK || len(got.Windows) != 1 || got.Windows[0].UsedPercent != 12.5 {
		t.Fatalf("one window: %+v", got)
	}
	for name, content := range map[string]string{
		"not JSON":               `{"capturedAt":`,
		"empty":                  ``,
		"an array":               `[1,2]`,
		"no rate_limits":         `{"capturedAt":1791460000,"version":"2.1.292"}`,
		"null rate_limits":       `{"capturedAt":1791460000,"rate_limits":null}`,
		"no windows":             `{"capturedAt":1791460000,"rate_limits":{}}`,
		"no capture time":        `{"rate_limits":{"five_hour":{"used_percentage":13,"resets_at":1791470000}}}`,
		"no percentage":          `{"capturedAt":1791460000,"rate_limits":{"five_hour":{"resets_at":1791470000}}}`,
		"a text percentage":      `{"capturedAt":1791460000,"rate_limits":{"five_hour":{"used_percentage":"13","resets_at":1791470000}}}`,
		"percentage over 100":    `{"capturedAt":1791460000,"rate_limits":{"five_hour":{"used_percentage":101,"resets_at":1791470000}}}`,
		"a negative percentage":  `{"capturedAt":1791460000,"rate_limits":{"five_hour":{"used_percentage":-1,"resets_at":1791470000}}}`,
		"no reset time":          `{"capturedAt":1791460000,"rate_limits":{"five_hour":{"used_percentage":13}}}`,
		"a null window":          `{"capturedAt":1791460000,"rate_limits":{"five_hour":null}}`,
		"one good, one bad":      `{"capturedAt":1791460000,"rate_limits":{"five_hour":{"used_percentage":13,"resets_at":1791470000},"seven_day":{"used_percentage":37}}}`,
		"only an unknown window": `{"capturedAt":1791460000,"rate_limits":{"monthly":{"used_percentage":13,"resets_at":1791470000}}}`,
	} {
		writeCapture(t, path, content)
		if got := readClaudeUsageCapture(path); got.State != api.ProviderUsageMalformed || len(got.Windows) != 0 || !got.CapturedAt.IsZero() {
			t.Fatalf("%s: %+v", name, got)
		}
		if report := readClaudeUsageCapture(path).report("mini"); report.State != api.ProviderUsageMalformed || report.CapturedAt != "" || report.Version != "" || len(report.Windows) != 0 {
			t.Fatalf("%s: the report carries a figure: %+v", name, report)
		}
	}
	// Unreadable: the path cannot be read as a file.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if got := readClaudeUsageCapture(path); got.State != api.ProviderUsageUnreadable {
		t.Fatalf("unreadable: %+v", got)
	}
}

// providerHub records the provider usage reports a relay sends.
type providerHub struct {
	mu      sync.Mutex
	status  int
	reports []api.ProviderUsageReport
}

func (h *providerHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Method != http.MethodPut || r.URL.Path != "/v1/provider-usage" {
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
		return
	}
	var report api.ProviderUsageReport
	_ = json.NewDecoder(r.Body).Decode(&report)
	h.reports = append(h.reports, report)
	if h.status != 0 {
		http.Error(w, `{"error":"synthetic"}`, h.status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"host":"mini","readings":[]}`))
}

func (h *providerHub) take() []api.ProviderUsageReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.reports
	h.reports = nil
	return out
}

// a7: a fresh file is reported once. A file rewritten with the same figures
// and a later capture time is not a change: it is reported once as keep-fresh
// after five minutes, not before. A file whose time alone moved is never
// reported. A changed percentage or reset is reported at once. A missing
// file with no earlier good report sends nothing. A good file that becomes
// missing, malformed or unreadable sends exactly one invalidation. The last
// report survives a relay restart.
func TestProviderUsageTickReportRule(t *testing.T) {
	path := captureHome(t)
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	hub := &providerHub{}
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	c, err := api.NewClient(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	state := &providerUsageState{}
	tick := func(why string, wantStates ...string) []api.ProviderUsageReport {
		t.Helper()
		if err := providerUsageTick(context.Background(), c, "mini", now, state); err != nil {
			t.Fatalf("%s: %v", why, err)
		}
		got := hub.take()
		var states []string
		for _, r := range got {
			states = append(states, r.State)
		}
		if fmt.Sprint(states) != fmt.Sprint(wantStates) {
			t.Fatalf("%s: reported %v, want %v", why, states, wantStates)
		}
		return got
	}
	// No file and nothing ever reported: nothing is sent, however long.
	for i := 0; i < 3; i++ {
		tick("a missing file with no earlier good report")
		now = now.Add(10 * time.Minute)
	}
	writeCapture(t, path, captureJSON(1791460000, 13, 1791470000))
	first := tick("a fresh file", api.ProviderUsageOK)[0]
	if first.Host != "mini" || first.Runtime != "claude" || first.CapturedAt != time.Unix(1791460000, 0).UTC().Format(time.RFC3339) || first.Version != "2.1.292" || len(first.Windows) != 2 || first.Windows[0].UsedPercent != 13 {
		t.Fatalf("first report %+v", first)
	}
	for i := 0; i < 5; i++ {
		now = now.Add(3 * time.Minute)
		tick("an unchanged file")
	}
	// The file time moves and nothing else: never reported.
	touched := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, touched, touched); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	tick("a file whose time moved with nothing else changed")
	// Rewritten with the same figures and a later capture time: one
	// keep-fresh report, since more than five minutes have passed.
	writeCapture(t, path, captureJSON(1791460050, 13, 1791470000))
	if got := tick("rewritten with unchanged figures, long after the last report", api.ProviderUsageOK); got[0].CapturedAt != time.Unix(1791460050, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("keep-fresh report %+v", got[0])
	}
	for i := int64(1); i <= 4; i++ {
		now = now.Add(time.Minute)
		writeCapture(t, path, captureJSON(1791460050+i*60, 13, 1791470000))
		tick("rewritten with unchanged figures inside five minutes")
	}
	now = now.Add(time.Minute)
	writeCapture(t, path, captureJSON(1791460050+300, 13, 1791470000))
	tick("rewritten with unchanged figures after five minutes", api.ProviderUsageOK)
	// A file nobody rewrites any more is never reported again.
	now = now.Add(6 * time.Hour)
	tick("no longer rewritten")

	// A changed percentage is reported at once, and so is the next one.
	writeCapture(t, path, captureJSON(1791460500, 14, 1791470000))
	if got := tick("a changed percentage", api.ProviderUsageOK); got[0].Windows[0].UsedPercent != 14 {
		t.Fatalf("changed report %+v", got[0])
	}
	now = now.Add(2 * time.Second)
	writeCapture(t, path, captureJSON(1791460503, 15, 1791470000))
	if got := tick("a second changed percentage two seconds later", api.ProviderUsageOK); got[0].Windows[0].UsedPercent != 15 {
		t.Fatalf("second changed report %+v", got[0])
	}
	// A changed reset alone is a change too.
	now = now.Add(2 * time.Second)
	writeCapture(t, path, captureJSON(1791460503, 15, 1791488000))
	if got := tick("a changed reset", api.ProviderUsageOK); got[0].Windows[0].ResetsAt != time.Unix(1791488000, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("reset report %+v", got[0])
	}

	// The last report survives a relay restart: a new process with the same
	// state directory sends nothing for the same file.
	state = &providerUsageState{}
	now = now.Add(time.Minute)
	tick("after a restart with an unchanged file")
	if _, err := os.Stat(providerUsageStatePath()); err != nil {
		t.Fatalf("the last report is not kept in the relay state directory: %v", err)
	}

	invalidations := map[string]func(){
		api.ProviderUsageMissing: func() {
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
		},
		api.ProviderUsageMalformed: func() { writeCapture(t, path, `{"capturedAt":1791460200,"rate_limits":null}`) },
		api.ProviderUsageUnreadable: func() {
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		},
	}
	captured := int64(1791461000)
	for _, bad := range []string{api.ProviderUsageMissing, api.ProviderUsageMalformed, api.ProviderUsageUnreadable} {
		// Good again first.
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		captured += 1000
		writeCapture(t, path, captureJSON(captured, 20, 1791470000))
		now = now.Add(2 * time.Minute)
		tick("good before "+bad, api.ProviderUsageOK)
		invalidations[bad]()
		now = now.Add(time.Second)
		got := tick("good then "+bad, bad)[0]
		if got.Host != "mini" || got.Runtime != "claude" || got.CapturedAt != "" || got.Version != "" || len(got.Windows) != 0 {
			t.Fatalf("%s invalidation carries a figure: %+v", bad, got)
		}
		for i := 0; i < 3; i++ {
			now = now.Add(10 * time.Minute)
			tick("still " + bad)
		}
		// Nor after a restart.
		state = &providerUsageState{}
		tick("still " + bad + " after a restart")
	}

	// A failed report is tried again after a minute, not before.
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	writeCapture(t, path, captureJSON(captured+5000, 30, 1791470000))
	hub.mu.Lock()
	hub.status = http.StatusInternalServerError
	hub.mu.Unlock()
	now = now.Add(2 * time.Minute)
	if err := providerUsageTick(context.Background(), c, "mini", now, state); err == nil || !strings.Contains(err.Error(), "report provider usage (ok)") || len(hub.take()) != 1 {
		t.Fatalf("a failing hub: %v", err)
	}
	now = now.Add(10 * time.Second)
	tick("inside the minute after a failure")
	hub.mu.Lock()
	hub.status = 0
	hub.mu.Unlock()
	now = now.Add(time.Minute)
	tick("after the failure clears", api.ProviderUsageOK)

	// A hub without the route is not an error and is asked again in an hour.
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	state = &providerUsageState{}
	hub.mu.Lock()
	hub.status = http.StatusNotFound
	hub.mu.Unlock()
	tick("an older hub", api.ProviderUsageOK)
	now = now.Add(30 * time.Minute)
	tick("an older hub, half an hour later")
	now = now.Add(31 * time.Minute)
	tick("an older hub, an hour later", api.ProviderUsageOK)
}

// a7 against a real hub: after a good reading then an invalidation the hub
// holds no figure of the old reading, and the next admission uses the
// allowance source, or with no reset time the neither-source reason.
func TestProviderUsageInvalidationReachesAdmission(t *testing.T) {
	f := newBudgetCLIFixture(t)
	path := captureHome(t)
	ctx := context.Background()
	f.estimate(600_000)
	if _, err := f.c.SetUsageBudget(ctx, f.task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	var state providerUsageState
	tick := func() {
		t.Helper()
		now = now.Add(2 * time.Minute)
		if err := providerUsageTick(ctx, f.c, "fixture", now, &state); err != nil {
			t.Fatal(err)
		}
	}
	reason := func() string {
		t.Helper()
		list, err := f.c.ListTeamQueue(ctx, f.task.ID)
		if err != nil || len(list.Entries) != 1 {
			t.Fatalf("list %+v %v", list, err)
		}
		return list.Entries[0].BlockReason
	}
	status := func() api.UsageBudgetStatus {
		t.Helper()
		out, err := f.c.UsageBudgets(ctx, f.task.ID, "fixture")
		if err != nil || len(out.Budgets) != 1 || out.Budgets[0].Status == nil {
			t.Fatalf("budget %+v %v", out, err)
		}
		return *out.Budgets[0].Status
	}
	fresh := time.Now().UTC()
	resets := fresh.Add(2 * time.Hour).Unix()
	// 50 percent used: 500K remain, which does not cover 600K.
	writeCapture(t, path, captureJSON(fresh.Unix(), 50, resets))
	tick()
	if got := status(); got.Source != api.UsageBudgetSourceProvider || got.RemainingTokens != "500000" {
		t.Fatalf("good reading: %+v", got)
	}
	if got := reason(); !strings.Contains(got, "needs about 600.00K tokens; 500.00K remain") || !strings.HasSuffix(got, "(source: provider reading)") {
		t.Fatalf("held on the reading: %q", got)
	}
	for _, bad := range []struct {
		state string
		make  func()
	}{
		{api.ProviderUsageMissing, func() { _ = os.RemoveAll(path) }},
		{api.ProviderUsageMalformed, func() { writeCapture(t, path, `not json`) }},
		{api.ProviderUsageUnreadable, func() { _ = os.RemoveAll(path); _ = os.Mkdir(path, 0700) }},
	} {
		_ = os.RemoveAll(path)
		writeCapture(t, path, captureJSON(fresh.Unix()+int64(len(bad.state)), 50, resets))
		tick()
		if got := status(); got.Source != api.UsageBudgetSourceProvider {
			t.Fatalf("good before %s: %+v", bad.state, got)
		}
		bad.make()
		tick()
		readings, err := f.c.ProviderUsage(ctx, "fixture")
		if err != nil || len(readings.Readings) != 2 {
			t.Fatalf("readings after %s: %+v %v", bad.state, readings, err)
		}
		for _, r := range readings.Readings {
			if r.State != bad.state || r.UsedPercent != "" || r.ResetsAt != "" || r.CapturedAt != "" || r.Version != "" {
				t.Fatalf("after %s the hub kept a figure: %+v", bad.state, r)
			}
		}
		// No reset time is set: neither source.
		want := "Token budget (claude five_hour): no usable source. Provider reading for host fixture is " + bad.state + " and no reset time is set"
		if got := reason(); got != want {
			t.Fatalf("after %s: %q", bad.state, got)
		}
		if got := status(); got.Source != api.UsageBudgetSourceNone || got.RemainingTokens != "" {
			t.Fatalf("after %s: %+v", bad.state, got)
		}
	}
	// With an owner reset the same invalidated host uses the allowance source:
	// the full allowance covers the estimate, so the entry is no longer held.
	if _, err := f.c.SetUsageBudget(ctx, f.task.ID, api.UsageBudgetRequest{Runtime: "claude", Window: api.UsageWindowFiveHour, AllowanceTokens: 1_000_000, ResetAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if got := status(); got.Source != api.UsageBudgetSourceAllowance || got.RemainingTokens != "1000000" || got.Reading != "is unreadable" {
		t.Fatalf("invalidated with an owner reset: %+v", got)
	}
	if got := reason(); got != "" {
		t.Fatalf("allowance source admits: %q", got)
	}
}
