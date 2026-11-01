package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/bridge"
	"github.com/scs32/tailterm/hub/internal/discord"
)

// wi_dc04ac6a9ccf7402: the bridge reads its saved settings, reports what it
// runs on, refuses a saved revision that cannot start, guards its guild and
// restarts on a new save. Fake hub and fake Discord only.

const (
	envGuild  = "100000000000000001"
	hubGuildB = "200000000000000002"
	hubGuildC = "300000000000000003"
	ownerID   = "900000000000000009"
	testBot   = "fixture-bot-token-not-real"
)

// fakeHub serves the bridge settings route and an empty project list.
type fakeHub struct {
	mu      sync.Mutex
	status  int // non-zero: answer the settings route with this status
	rec     api.BridgeSettingsRecord
	reports []api.BridgeReport
	bare    int
	queries []string
}

func (f *fakeHub) save(s api.BridgeSettings) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec.Revision++
	copied := s
	f.rec.Settings = &copied
	return f.rec.Revision
}

func (f *fakeHub) lastReport() (api.BridgeReport, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reports) == 0 {
		return api.BridgeReport{}, false
	}
	return f.reports[len(f.reports)-1], true
}

func (f *fakeHub) clearReports() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = nil
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/bridge/settings":
		f.queries = append(f.queries, r.URL.RawQuery)
		if f.status != 0 {
			w.WriteHeader(f.status)
			fmt.Fprint(w, `{"error":"the bridge credential cannot use this route"}`)
			return
		}
		report, has, err := api.ParseBridgeReport(r.URL.Query())
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":%q}`, err.Error())
			return
		}
		if has {
			f.reports = append(f.reports, report)
		} else {
			f.bare++
		}
		_ = json.NewEncoder(w).Encode(api.BridgeSettingsView{BridgeSettingsRecord: f.rec})
	case r.Method == "GET" && r.URL.Path == "/v1/tasks":
		fmt.Fprint(w, `{"tasks":[]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"not found"}`)
	}
}

// fakeDiscord answers the bridge's start-up calls; guild command
// registration fails for the guilds in fail.
type fakeDiscord struct {
	mu       sync.Mutex
	fail     map[string]bool
	requests []string
	commands map[string]int // guild → command registration attempts
}

func (f *fakeDiscord) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeDiscord) registrations(guild string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commands[guild]
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bot "+testBot {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"code":0,"message":"401: Unauthorized"}`)
		return
	}
	if r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/applications/app1/guilds/") {
		guild := strings.Split(strings.TrimPrefix(r.URL.Path, "/applications/app1/guilds/"), "/")[0]
		f.commands[guild]++
		if f.fail[guild] {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"code":50001,"message":"Missing Access"}`)
			return
		}
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/users/@me":
		fmt.Fprint(w, `{"id":"app1","username":"bot"}`)
	case r.Method == "GET", r.Method == "PUT":
		fmt.Fprint(w, `[]`)
	default:
		fmt.Fprint(w, `{}`)
	}
}

type harness struct {
	t       *testing.T
	hub     *fakeHub
	discord *fakeDiscord
	hubURL  string
	discURL string
	path    string
	env     api.BridgeSettings
	logMu   sync.Mutex
	logs    []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, hub: &fakeHub{}, discord: &fakeDiscord{fail: map[string]bool{}, commands: map[string]int{}},
		path: filepath.Join(t.TempDir(), "bridge.sqlite"),
		env:  api.BridgeSettings{GuildID: envGuild, OwnerIDs: []string{ownerID}}}
	hubSrv := httptest.NewServer(h.hub)
	t.Cleanup(hubSrv.Close)
	discSrv := httptest.NewServer(h.discord)
	t.Cleanup(discSrv.Close)
	h.hubURL, h.discURL = hubSrv.URL, discSrv.URL
	return h
}

func (h *harness) logf(format string, args ...any) {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	h.logs = append(h.logs, fmt.Sprintf(format, args...))
}

func (h *harness) logged(substr string) bool {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	for _, l := range h.logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func (h *harness) openState() *bridge.State {
	h.t.Helper()
	st, err := bridge.OpenState(h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func (h *harness) kv(key string) string {
	h.t.Helper()
	st := h.openState()
	defer st.Close()
	v, err := st.Get(context.Background(), key)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

func (h *harness) seedMapping(task string) {
	h.t.Helper()
	st := h.openState()
	defer st.Close()
	if err := st.SetChannel(context.Background(), task, "500000000000000005", 0); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) setKV(key, value string) {
	h.t.Helper()
	st := h.openState()
	defer st.Close()
	if err := st.Set(context.Background(), key, value); err != nil {
		h.t.Fatal(err)
	}
}

// run is one bridge process: its own state handle, stopped by stop().
type run struct {
	h       *harness
	state   *bridge.State
	cancel  context.CancelFunc
	bridges chan *bridge.Bridge
	done    chan struct{}
	restart bool
	err     error
}

func (h *harness) start() *run {
	h.t.Helper()
	hub, err := api.NewClient(h.hubURL, 5*time.Second)
	if err != nil {
		h.t.Fatal(err)
	}
	hub.Token = "fixture-bridge-token-not-real"
	st := h.openState()
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{h: h, state: st, cancel: cancel, bridges: make(chan *bridge.Bridge, 1), done: make(chan struct{})}
	runner := &settingsRunner{
		Hub: hub, State: st, Env: h.env,
		Base: bridge.Config{Hub: hub, AppID: "app1", State: st, Log: h.logf,
			Discord: &discord.Client{Token: testBot, Base: h.discURL, HTTP: &http.Client{Timeout: 5 * time.Second}, MaxWait: time.Second}},
		Poll: 20 * time.Millisecond, Log: h.logf,
		OnBridge: func(b *bridge.Bridge) { r.bridges <- b },
	}
	go func() {
		defer close(r.done)
		r.restart, r.err = runner.run(ctx)
	}()
	return r
}

// bridge waits for the bridge this run built.
func (r *run) bridge() *bridge.Bridge {
	r.h.t.Helper()
	select {
	case b := <-r.bridges:
		return b
	case <-r.done:
		r.h.t.Fatalf("the run ended before building a bridge: restart=%v err=%v", r.restart, r.err)
	case <-time.After(10 * time.Second):
		r.h.t.Fatal("no bridge was built")
	}
	return nil
}

// ready delivers the first Gateway READY once commands are registered.
func (r *run) ready(b *bridge.Bridge, guild string) {
	r.h.t.Helper()
	waitFor(r.h.t, "command registration", func() bool { return r.h.discord.registrations(guild) > 0 || r.h.kvOn(r.state, "commands") != "" })
	b.Dispatch(discord.Dispatch{Type: "READY", Data: json.RawMessage(`{}`)})
}

func (h *harness) kvOn(st *bridge.State, key string) string {
	v, _ := st.Get(context.Background(), key)
	return v
}

// stop is a clean SIGTERM.
func (r *run) stop() {
	r.h.t.Helper()
	r.cancel()
	r.wait()
	r.state.Close()
}

// kill ends the process without any clean-up: its state handle goes away
// first, so nothing it does afterwards is recorded.
func (r *run) kill() {
	r.h.t.Helper()
	r.state.Close()
	r.cancel()
	r.wait()
}

func (r *run) wait() {
	r.h.t.Helper()
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		r.h.t.Fatal("the run did not end")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) waitReport(want func(api.BridgeReport) bool) api.BridgeReport {
	h.t.Helper()
	var last api.BridgeReport
	waitFor(h.t, "a matching report", func() bool {
		r, ok := h.hub.lastReport()
		last = r
		return ok && want(r)
	})
	return last
}

func settingsFor(guild string) api.BridgeSettings {
	return api.BridgeSettings{GuildID: guild, OwnerIDs: []string{ownerID}, TailOSURL: "https://tailos.example"}
}

// a5: a valid saved revision is what the bridge runs on and reports; a new
// save makes it exit normally to restart.
func TestBridgeSettingsRunOnSavedRevisionAndRestartOnSave(t *testing.T) {
	h := newHarness(t)
	rev := h.hub.save(settingsFor(hubGuildB))
	r := h.start()
	b := r.bridge()
	rep := h.waitReport(func(r api.BridgeReport) bool { return true })
	if rep.Source != api.BridgeSourceHub || rep.Applied != rev || rep.Effective.GuildID != hubGuildB || rep.Effective.TailOSURL != "https://tailos.example" || rep.Refused != 0 {
		t.Fatalf("report = %+v", rep)
	}
	r.ready(b, hubGuildB)
	waitFor(t, "started marker", func() bool { return h.kvOn(r.state, kvStarted) == "1" })
	if h.discord.registrations(hubGuildB) != 1 || h.discord.registrations(envGuild) != 0 {
		t.Fatalf("registered commands in the wrong guild: %+v", h.discord.commands)
	}
	h.hub.save(settingsFor(hubGuildB))
	r.wait()
	if !r.restart || r.err != nil {
		t.Fatalf("after a new save: restart=%v err=%v", r.restart, r.err)
	}
	r.state.Close()
	if !h.logged("bridge settings changed to revision 2; restarting to apply") {
		t.Fatal("the restart was not logged")
	}
	// The same revision does not restart it again.
	r = h.start()
	r.bridge()
	h.waitReport(func(r api.BridgeReport) bool { return r.Applied == 2 })
	time.Sleep(100 * time.Millisecond)
	select {
	case <-r.done:
		t.Fatalf("restarted on an unchanged revision: %v %v", r.restart, r.err)
	default:
	}
	r.stop()
}

// a5: no saved record, or a hub without the route (403/404), runs on the
// deployment values exactly as before.
func TestBridgeSettingsDeploymentFallback(t *testing.T) {
	for _, status := range []int{0, http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newHarness(t)
			h.hub.status = status
			r := h.start()
			b := r.bridge()
			r.ready(b, envGuild)
			if status == 0 {
				rep := h.waitReport(func(api.BridgeReport) bool { return true })
				if rep.Source != api.BridgeSourceDeployment || rep.Applied != 0 || rep.Effective.GuildID != envGuild || rep.Refused != 0 {
					t.Fatalf("report = %+v", rep)
				}
			} else {
				waitFor(t, "a report attempt", func() bool {
					h.hub.mu.Lock()
					defer h.hub.mu.Unlock()
					return len(h.hub.queries) >= 2
				})
			}
			if h.discord.registrations(envGuild) != 1 {
				t.Fatalf("env guild registrations = %d", h.discord.registrations(envGuild))
			}
			if got := h.kvOn(r.state, kvAttempt); got != "" {
				t.Fatalf("deployment values wrote an attempt marker %q", got)
			}
			r.stop()
			if r.restart || r.err != nil {
				t.Fatalf("stop: restart=%v err=%v", r.restart, r.err)
			}
		})
	}
}

// a6: a start on revision N that fails before READY refuses N; the next start
// runs on the deployment values and reports the refusal; N is never tried
// again; a new revision is.
func TestBridgeSettingsStartupFailureRefusesRevision(t *testing.T) {
	h := newHarness(t)
	h.discord.fail[hubGuildB] = true
	rev := h.hub.save(settingsFor(hubGuildB))
	r := h.start()
	r.bridge()
	r.wait()
	r.state.Close()
	if r.err == nil || !strings.Contains(r.err.Error(), "register commands") {
		t.Fatalf("failed start = %v", r.err)
	}
	if got := h.kv(kvRefused); got != fmt.Sprintf("%d:%s", rev, api.BridgeRefusedStartupFailed) {
		t.Fatalf("refused = %q", got)
	}
	for i := 0; i < 2; i++ {
		h.hub.clearReports()
		r = h.start()
		b := r.bridge()
		rep := h.waitReport(func(api.BridgeReport) bool { return true })
		if rep.Source != api.BridgeSourceDeployment || rep.Effective.GuildID != envGuild || rep.Refused != rev || rep.Reason != api.BridgeRefusedStartupFailed {
			t.Fatalf("start %d report = %+v", i+2, rep)
		}
		r.ready(b, envGuild)
		time.Sleep(50 * time.Millisecond) // a few watcher polls: no restart on the refused revision
		r.stop()
		if r.restart {
			t.Fatal("restarted on the refused revision")
		}
	}
	if n := h.discord.registrations(hubGuildB); n != 1 {
		t.Fatalf("the refused revision was tried %d times", n)
	}
	// A new save is tried.
	h.discord.mu.Lock()
	h.discord.fail[hubGuildB] = false
	h.discord.mu.Unlock()
	rev2 := h.hub.save(settingsFor(hubGuildB))
	r = h.start()
	b := r.bridge()
	rep := h.waitReport(func(r api.BridgeReport) bool { return r.Source == api.BridgeSourceHub })
	if rep.Applied != rev2 || rep.Refused != 0 {
		t.Fatalf("new revision report = %+v", rep)
	}
	r.ready(b, hubGuildB)
	r.stop()
}

// a6 with lead decision L1: success-N, clean restart-N, kill before READY,
// restart-N → refused N stopped-during-start, runs on the fallback, N not
// retried. A SIGTERM before READY refuses nothing.
func TestBridgeSettingsAttemptIdentity(t *testing.T) {
	h := newHarness(t)
	rev := h.hub.save(settingsFor(hubGuildB))
	// success-N
	r := h.start()
	b := r.bridge()
	r.ready(b, hubGuildB)
	waitFor(t, "started", func() bool { return h.kvOn(r.state, kvStarted) == "1" })
	r.stop()
	// clean restart-N
	r = h.start()
	b = r.bridge()
	h.waitReport(func(r api.BridgeReport) bool { return r.Source == api.BridgeSourceHub && r.Applied == rev })
	r.ready(b, hubGuildB)
	waitFor(t, "started again", func() bool { return h.kvOn(r.state, kvStarted) == "1" })
	r.stop()
	// SIGTERM before READY: not a failure.
	r = h.start()
	r.bridge()
	waitFor(t, "attempt marker", func() bool { return h.kvOn(r.state, kvAttempt) == "1" && h.kvOn(r.state, kvStarted) == "" })
	r.stop()
	if got := h.kv(kvAttempt); got != "" {
		t.Fatalf("SIGTERM before READY left attempt %q", got)
	}
	if got := h.kv(kvRefused); got != "" {
		t.Fatalf("SIGTERM before READY refused %q", got)
	}
	// kill before READY
	r = h.start()
	r.bridge()
	waitFor(t, "attempt marker", func() bool { return h.kvOn(r.state, kvAttempt) == "1" && h.kvOn(r.state, kvStarted) == "" })
	r.kill()
	// restart-N: refused, runs on the fallback (last good, the only one
	// matching the bound guild), N not retried.
	for i := 0; i < 2; i++ {
		h.hub.clearReports()
		r = h.start()
		b = r.bridge()
		rep := h.waitReport(func(api.BridgeReport) bool { return true })
		if rep.Source == api.BridgeSourceHub || rep.Refused != rev || rep.Reason != api.BridgeRefusedStoppedDuringStart {
			t.Fatalf("restart %d report = %+v", i, rep)
		}
		if rep.Source != api.BridgeSourceLastGood || rep.Effective.GuildID != hubGuildB {
			t.Fatalf("restart %d fallback = %+v", i, rep)
		}
		if got := h.kvOn(r.state, kvAttempt); got != "1" {
			t.Fatalf("a fallback start rewrote the attempt marker: %q", got)
		}
		r.ready(b, hubGuildB)
		r.stop()
	}
}

// L2 test (a): env guild A, empty state, accept hub B, READY, B mappings,
// save C → refused guild-change, runs on last good (B), never env A.
func TestBridgeSettingsGuildChangeUsesLastGood(t *testing.T) {
	h := newHarness(t)
	h.hub.save(settingsFor(hubGuildB))
	r := h.start()
	b := r.bridge()
	r.ready(b, hubGuildB)
	waitFor(t, "last good", func() bool { return h.kvOn(r.state, kvLastGood) != "" })
	if err := r.state.SetChannel(context.Background(), "tsk_0123456789abcdef", "500000000000000005", 0); err != nil {
		t.Fatal(err)
	}
	revC := h.hub.save(settingsFor(hubGuildC))
	r.wait()
	r.state.Close()
	if !r.restart {
		t.Fatalf("no restart on the new save: %v", r.err)
	}
	h.hub.clearReports()
	r = h.start()
	b = r.bridge()
	rep := h.waitReport(func(api.BridgeReport) bool { return true })
	if rep.Source != api.BridgeSourceLastGood || rep.Applied != 1 || rep.Effective.GuildID != hubGuildB || rep.Refused != revC || rep.Reason != api.BridgeRefusedGuildChange {
		t.Fatalf("report = %+v", rep)
	}
	r.ready(b, hubGuildB)
	r.stop()
	if h.discord.registrations(hubGuildC) != 0 || h.discord.registrations(envGuild) != 0 {
		t.Fatalf("connected to another guild: %+v", h.discord.commands)
	}
	if h.kv(kvGuild) != hubGuildB {
		t.Fatalf("bound guild = %q", h.kv(kvGuild))
	}
}

// L2 test (b): mappings exist and no candidate matches the bound guild →
// parked: no Discord connection, report source=none, exit on a new save.
func TestBridgeSettingsParkedWithoutMatchingGuild(t *testing.T) {
	h := newHarness(t)
	h.seedMapping("tsk_0123456789abcdef")
	h.setKV(kvGuild, hubGuildB)
	rev := h.hub.save(settingsFor(hubGuildC))
	r := h.start()
	rep := h.waitReport(func(api.BridgeReport) bool { return true })
	if rep.Source != api.BridgeSourceNone || rep.Refused != rev || rep.Reason != api.BridgeRefusedGuildChange || rep.Effective.GuildID != "" {
		t.Fatalf("parked report = %+v", rep)
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case <-r.bridges:
		t.Fatal("a parked bridge built a bridge")
	case <-r.done:
		t.Fatalf("a parked bridge exited: %v %v", r.restart, r.err)
	default:
	}
	if n := h.discord.count(); n != 0 {
		t.Fatalf("a parked bridge called Discord %d times: %v", n, h.discord.requests)
	}
	// Saving the bound guild again restarts it onto that guild.
	h.hub.save(settingsFor(hubGuildB))
	r.wait()
	r.state.Close()
	if !r.restart || r.err != nil {
		t.Fatalf("parked exit = %v %v", r.restart, r.err)
	}
	r = h.start()
	b := r.bridge()
	rep = h.waitReport(func(r api.BridgeReport) bool { return r.Source == api.BridgeSourceHub })
	if rep.Effective.GuildID != hubGuildB {
		t.Fatalf("after saving the bound guild = %+v", rep)
	}
	r.ready(b, hubGuildB)
	r.stop()
}

// L2 test (c): the bound guild is written before Run starts its loops, so a
// mapping created before READY still matches on the next start; a state
// from before this change binds to the deployment guild.
func TestBridgeSettingsGuildBoundBeforeRun(t *testing.T) {
	h := newHarness(t)
	h.hub.save(settingsFor(hubGuildB))
	hub, _ := api.NewClient(h.hubURL, 5*time.Second)
	st := h.openState()
	runner := &settingsRunner{Hub: hub, State: st, Env: h.env, Log: h.logf, Poll: 20 * time.Millisecond,
		Base: bridge.Config{Hub: hub, AppID: "app1", State: st, Log: h.logf,
			Discord: &discord.Client{Token: testBot, Base: h.discURL, HTTP: &http.Client{Timeout: 5 * time.Second}, MaxWait: time.Second}}}
	ctx, cancel := context.WithCancel(context.Background())
	var boundAtBuild string
	runner.OnBridge = func(b *bridge.Bridge) {
		boundAtBuild, _ = st.Get(context.Background(), kvGuild)
		// A mapping created by a loop before READY.
		_ = st.SetChannel(context.Background(), "tsk_0123456789abcdef", "500000000000000005", 0)
		cancel()
	}
	if _, err := runner.run(ctx); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if boundAtBuild != hubGuildB {
		t.Fatalf("bound guild before Run = %q, want %s", boundAtBuild, hubGuildB)
	}
	r := h.start()
	b := r.bridge()
	rep := h.waitReport(func(api.BridgeReport) bool { return true })
	if rep.Source != api.BridgeSourceHub || rep.Effective.GuildID != hubGuildB {
		t.Fatalf("the mapping made before READY did not match: %+v", rep)
	}
	r.ready(b, hubGuildB)
	r.stop()

	// An older state: mappings, no bound guild → binds to the deployment
	// guild and refuses another saved guild.
	old := newHarness(t)
	old.seedMapping("tsk_0123456789abcdef")
	rev := old.hub.save(settingsFor(hubGuildC))
	r = old.start()
	b = r.bridge()
	rep = old.waitReport(func(api.BridgeReport) bool { return true })
	if rep.Source != api.BridgeSourceDeployment || rep.Effective.GuildID != envGuild || rep.Refused != rev || rep.Reason != api.BridgeRefusedGuildChange {
		t.Fatalf("older state report = %+v", rep)
	}
	r.ready(b, envGuild)
	r.stop()
	if old.kv(kvGuild) != envGuild {
		t.Fatalf("older state bound to %q", old.kv(kvGuild))
	}
}

// D4b step 4: a saved record the bridge cannot use is refused as invalid
// and the bridge runs on the deployment values in the same process.
func TestBridgeSettingsInvalidRecordFallsBack(t *testing.T) {
	h := newHarness(t)
	rev := h.hub.save(api.BridgeSettings{GuildID: "not-a-guild", OwnerIDs: []string{ownerID}})
	r := h.start()
	b := r.bridge()
	rep := h.waitReport(func(api.BridgeReport) bool { return true })
	if rep.Source != api.BridgeSourceDeployment || rep.Refused != rev || rep.Reason != api.BridgeRefusedInvalid {
		t.Fatalf("report = %+v", rep)
	}
	r.ready(b, envGuild)
	r.stop()
	h.logMu.Lock()
	defer h.logMu.Unlock()
	for _, l := range h.logs {
		if strings.Contains(l, testBot) || strings.Contains(l, "fixture-bridge-token") {
			t.Fatalf("a log line holds a token: %s", l)
		}
	}
}

// The report carries only the listed non-secret parameters.
func TestBridgeSettingsReportParameters(t *testing.T) {
	q := api.BridgeReport{Source: api.BridgeSourceHub, Applied: 3, Effective: settingsFor(hubGuildB)}.Query()
	var keys []string
	for k := range q {
		keys = append(keys, k)
	}
	want := map[string]bool{"source": true, "applied": true, "guild": true, "owners": true, "helperTask": true, "helperChannel": true, "tailosUrl": true, "refused": true, "reason": true}
	if len(keys) != len(want) {
		t.Fatalf("report parameters = %v", keys)
	}
	for _, k := range keys {
		if !want[k] {
			t.Fatalf("unexpected report parameter %q", k)
		}
	}
}
