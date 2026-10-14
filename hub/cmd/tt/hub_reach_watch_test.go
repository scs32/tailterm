package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// No test of this package starts the real watch: it would call the hub of
// hub.json and write to stderr from its own goroutine.
func init() { relayHubReachStart = func(string) func() { return func() {} } }

const (
	hubReachTestOwner = "123456789012345678"
	hubReachTestToken = "FAKE-BOT-TOKEN-do-not-log-0123456789"
	hubReachTestBot   = "999999999999999999"
)

// hubReachAt is a wall-clock time in America/Los_Angeles on 2026-10-13 plus days.
func hubReachAt(day, hour, minute int) time.Time {
	return time.Date(2026, 10, 13+day, hour, minute, 0, 0, hubReachPacific)
}

type hubReachBoardCall struct {
	RequestID string
	Task      string
	At        time.Time
	Episode   hubReachEpisode
}

// hubReachFake holds every dependency of the watch as a fake.
type hubReachFake struct {
	t        *testing.T
	now      time.Time
	call     hubReachCall
	route    string
	dns      string
	hubCalls int
	probes   int
	settings hubReachSettings
	tasks    []string

	sends     []hubReachMessage
	sendAt    []time.Time
	finds     []hubReachMessage
	sendDoes  func(hubReachMessage) hubReachOutcome
	findDoes  func(hubReachMessage) (bool, error)
	boards    []hubReachBoardCall
	boardDoes func() error

	log bytes.Buffer
	dir string
	w   *hubReachWatch
	// reloadWith restarts the watch with the dependencies withDiscord set.
	reloadWith func()
}

var errHubReachRefused = fmt.Errorf("dial: %w", syscall.ECONNREFUSED)

func newHubReachFake(t *testing.T, start time.Time) *hubReachFake {
	t.Helper()
	f := &hubReachFake{
		t: t, now: start, call: hubReachCall{Status: 200}, route: hubReachTunnel, dns: hubReachDNSPresent,
		settings: hubReachSettings{Owner: hubReachTestOwner, TokenFile: filepath.Join(t.TempDir(), "token")},
		tasks:    []string{"tsk_one"}, dir: t.TempDir(),
	}
	f.reload()
	return f
}

func (f *hubReachFake) deps() hubReachDeps {
	return hubReachDeps{
		now:    func() time.Time { return f.now },
		hubURL: func() string { return "http://100.116.238.37:18765" },
		hub: func(context.Context) hubReachCall {
			f.hubCalls++
			return f.call
		},
		route: func(_ context.Context, host string) string {
			f.probes++
			if host != "100.116.238.37" {
				f.t.Errorf("route probe asked about %q", host)
			}
			return f.route
		},
		dns: func(context.Context) string { return f.dns },
		send: func(_ context.Context, s hubReachSettings, m hubReachMessage) hubReachOutcome {
			if s.Owner != hubReachTestOwner {
				f.t.Errorf("send to owner %q", s.Owner)
			}
			if hubReachNight(f.now) {
				f.t.Errorf("Discord send at night: %s", hubReachStamp(f.now))
			}
			f.sends = append(f.sends, m)
			f.sendAt = append(f.sendAt, f.now)
			if f.sendDoes != nil {
				return f.sendDoes(m)
			}
			return hubReachSent
		},
		find: func(_ context.Context, _ hubReachSettings, m hubReachMessage) (bool, error) {
			if hubReachNight(f.now) {
				f.t.Errorf("Discord read at night: %s", hubReachStamp(f.now))
			}
			f.finds = append(f.finds, m)
			if f.findDoes != nil {
				return f.findDoes(m)
			}
			return false, nil
		},
		tasks: func() []string { return f.tasks },
		board: func(_ context.Context, e hubReachEpisode, task string) error {
			f.boards = append(f.boards, hubReachBoardCall{RequestID: hubReachBoardRequestID(e, task), Task: task, At: f.now, Episode: e})
			if f.boardDoes != nil {
				return f.boardDoes()
			}
			return nil
		},
		settings: func() hubReachSettings { return f.settings },
		log:      &f.log,
		dir:      f.dir,
		host:     "testhost",
	}
}

// reload starts a new watch over the same state directory, as a relay
// restart does.
func (f *hubReachFake) reload() { f.w = newHubReachWatch(f.deps()) }

func (f *hubReachFake) step() { f.w.step(context.Background()) }

// at moves the clock to t and takes one step.
func (f *hubReachFake) at(t time.Time) {
	f.t.Helper()
	if t.Before(f.now) {
		f.t.Fatalf("clock moved back from %s to %s", f.now, t)
	}
	f.now = t
	f.step()
}

// ticks takes n steps 30 seconds apart, the first one 30 seconds from now.
func (f *hubReachFake) ticks(n int) {
	for i := 0; i < n; i++ {
		f.now = f.now.Add(hubReachInterval)
		f.step()
	}
}

func (f *hubReachFake) down(route string) {
	f.call = hubReachCall{Err: errHubReachRefused}
	f.route = route
}

func (f *hubReachFake) up() { f.call, f.route = hubReachCall{Status: 200}, hubReachTunnel }

// outage fails from the current time: a probe now, then every 30 seconds
// until the episode has just opened, 3 minutes later.
func (f *hubReachFake) outage(route string) {
	f.down(route)
	f.step()
	f.ticks(6)
}

func (f *hubReachFake) marker() string {
	data, err := os.ReadFile(filepath.Join(f.dir, hubReachMarkerFile))
	if err != nil {
		return ""
	}
	return string(data)
}

func (f *hubReachFake) kinds() string {
	var out []string
	for _, m := range f.sends {
		out = append(out, m.Kind)
	}
	return strings.Join(out, ",")
}

func (f *hubReachFake) logCount(part string) int { return strings.Count(f.log.String(), part) }

// T1: hub down with the route outside the tunnel gives one alert.
func TestHubReachOpensAfterThreeMinutesWithOneAlert(t *testing.T) {
	start := hubReachAt(0, 10, 0)
	f := newHubReachFake(t, start)
	f.down(hubReachNotTunnel)
	f.step()
	f.ticks(5) // 2 min 30 s of failures
	if f.w.state.Open != nil || len(f.sends) != 0 || f.marker() != "" {
		t.Fatalf("an episode opened before 3 minutes: %+v sends=%d", f.w.state.Open, len(f.sends))
	}
	f.ticks(1)
	e := f.w.state.Open
	if e == nil || e.Class != hubReachNoRoute || !e.FailingSince.Equal(start) || !e.OpenedAt.Equal(start.Add(3*time.Minute)) {
		t.Fatalf("episode %+v", e)
	}
	if e.Route != hubReachNotTunnel || e.DNS != hubReachDNSPresent || e.Alert.State != hubReachDelivered {
		t.Fatalf("episode readings %+v", e)
	}
	if f.kinds() != "alert" {
		t.Fatalf("sends %q", f.kinds())
	}
	alert := f.sends[0]
	if alert.Nonce != fmt.Sprintf("hra-%s-%d", hubReachHostHash("testhost"), start.Unix()) || alert.Ref != "ref hub-reach alert testhost-"+fmt.Sprint(start.Unix()) || !strings.HasSuffix(alert.Text, "\n"+alert.Ref) {
		t.Fatalf("alert identity %+v", alert)
	}
	for _, part := range []string{"testhost", "100.116.238.37", hubReachNoRoute, "route=not-tunnel dns=present", "changed no network setting and recovered nothing", "record why it stopped", filepath.Join(f.dir, hubReachMarkerFile)} {
		if !strings.Contains(alert.Text, part) {
			t.Fatalf("alert text lacks %q:\n%s", part, alert.Text)
		}
	}
	marker := f.marker()
	for _, part := range []string{"HUB UNREACHABLE", start.UTC().Format(time.RFC3339), hubReachNoRoute, "owner alert on Discord: delivered", "changed no network setting"} {
		if !strings.Contains(marker, part) {
			t.Fatalf("marker lacks %q:\n%s", part, marker)
		}
	}
	line := fmt.Sprintf("[tt relay] %s HUB UNREACHABLE since %s: no_tunnel_route (route=not-tunnel dns=present)\n", start.Add(3*time.Minute).UTC().Format(time.RFC3339), start.UTC().Format(time.RFC3339))
	if f.logCount("HUB UNREACHABLE") != 1 || !strings.Contains(f.log.String(), line) {
		t.Fatalf("log:\n%s", f.log.String())
	}
	if len(f.boards) != 0 {
		t.Fatalf("the alert path called the hub's Board: %+v", f.boards)
	}
}

// T2: hub down for less than 3 minutes gives nothing, and a success
// restarts the count.
func TestHubReachShortOutageGivesNoAlert(t *testing.T) {
	f := newHubReachFake(t, hubReachAt(0, 10, 0))
	for round := 0; round < 2; round++ {
		f.down(hubReachNotTunnel)
		f.ticks(6) // failing for 2 min 30 s
		f.up()
		f.ticks(1)
	}
	if len(f.sends) != 0 || len(f.boards) != 0 || f.marker() != "" || f.w.state.Open != nil || len(f.w.state.Closed) != 0 || f.logCount("HUB") != 0 {
		t.Fatalf("sends=%d boards=%d marker=%q log=%q", len(f.sends), len(f.boards), f.marker(), f.log.String())
	}
}

// T3: the class table.
func TestHubReachCauseClasses(t *testing.T) {
	timeout := fmt.Errorf("get: %w", context.DeadlineExceeded)
	for _, c := range []struct {
		name          string
		call          hubReachCall
		route, dns    string
		class, detail string
	}{
		{"http 503", hubReachCall{Status: 503}, hubReachNotTunnel, hubReachDNSAbsent, hubReachHTTPError, "503"},
		{"http 401", hubReachCall{Status: 401}, hubReachTunnel, hubReachDNSPresent, hubReachHTTPError, "401"},
		{"route outside the tunnel", hubReachCall{Err: timeout}, hubReachNotTunnel, hubReachDNSAbsent, hubReachNoRoute, "timeout"},
		{"resolver missing", hubReachCall{Err: timeout}, hubReachTunnel, hubReachDNSAbsent, hubReachDNSMissing, "timeout"},
		{"timeout", hubReachCall{Err: timeout}, hubReachTunnel, hubReachDNSPresent, hubReachTCPNoAnswer, "timeout"},
		{"refused", hubReachCall{Err: errHubReachRefused}, hubReachTunnel, hubReachDNSPresent, hubReachTCPNoAnswer, "refused"},
		{"unknown readings", hubReachCall{Err: errors.New("eof")}, hubReachUnknownProbe, hubReachUnknownProbe, hubReachTCPNoAnswer, "other"},
	} {
		if class, detail := hubReachClassify(c.call, c.route, c.dns); class != c.class || detail != c.detail {
			t.Errorf("%s: %s (%s), want %s (%s)", c.name, class, detail, c.class, c.detail)
		}
		// The same through the watch: the episode records it.
		f := newHubReachFake(t, hubReachAt(0, 10, 0))
		f.call, f.route, f.dns = c.call, c.route, c.dns
		f.step()
		f.ticks(6)
		if e := f.w.state.Open; e == nil || e.Class != c.class || e.Detail != c.detail || e.Route != c.route || e.DNS != c.dns {
			t.Errorf("%s: episode %+v", c.name, e)
		}
	}
}

// T4: repeated failures inside one episode give no second alert.
func TestHubReachOneAlertPerEpisode(t *testing.T) {
	f := newHubReachFake(t, hubReachAt(0, 10, 0))
	f.outage(hubReachNotTunnel)
	f.dns = hubReachDNSAbsent
	f.route = hubReachTunnel
	f.ticks(30)
	if f.kinds() != "alert" || f.logCount("HUB UNREACHABLE") != 1 || len(f.finds) != 0 {
		t.Fatalf("sends %q, log:\n%s", f.kinds(), f.log.String())
	}
	e := f.w.state.Open
	if e.Class != hubReachNoRoute || e.LatestClass != hubReachDNSMissing {
		t.Fatalf("classes %s then %s", e.Class, e.LatestClass)
	}
	if !strings.Contains(f.marker(), "latest failed probe: "+hubReachDNSMissing) {
		t.Fatalf("marker does not follow the latest class:\n%s", f.marker())
	}
}

// T5: recovery gives one recovery line and one Board post.
func TestHubReachRecoveryLine(t *testing.T) {
	start := hubReachAt(0, 10, 0)
	f := newHubReachFake(t, start)
	f.outage(hubReachNotTunnel)
	f.ticks(4) // down 5 minutes
	f.up()
	f.ticks(1)
	end := start.Add(5*time.Minute + 30*time.Second)
	if f.marker() != "" || f.w.state.Open != nil {
		t.Fatalf("marker or episode left after recovery: %q", f.marker())
	}
	if f.kinds() != "alert,recovery" {
		t.Fatalf("sends %q", f.kinds())
	}
	alert, rec := f.sends[0], f.sends[1]
	if rec.Nonce != fmt.Sprintf("hrr-%s-%d", hubReachHostHash("testhost"), start.Unix()) || rec.Nonce == alert.Nonce || rec.Ref == alert.Ref || !strings.HasPrefix(rec.Ref, "ref hub-reach recovery ") || !strings.HasSuffix(rec.Text, "\n"+rec.Ref) {
		t.Fatalf("recovery identity %+v", rec)
	}
	for _, part := range []string{"outage began at " + hubReachStamp(start), "reachable again at " + hubReachStamp(end), "duration 5m30s", hubReachNoRoute} {
		if !strings.Contains(rec.Text, part) {
			t.Fatalf("recovery text lacks %q:\n%s", part, rec.Text)
		}
	}
	if strings.Contains(rec.Text, "No alert was delivered") || strings.Contains(rec.Text, "may not have been delivered") {
		t.Fatalf("recovery text doubts a delivered alert:\n%s", rec.Text)
	}
	if len(f.boards) != 1 || f.boards[0].Task != "tsk_one" || !f.boards[0].At.Equal(end) || f.boards[0].Episode.ClosedAt.Sub(f.boards[0].Episode.FailingSince) != 5*time.Minute+30*time.Second {
		t.Fatalf("board posts %+v", f.boards)
	}
	if !strings.Contains(f.log.String(), "HUB REACHABLE again") || !strings.Contains(f.log.String(), "5m30s") || !strings.Contains(f.log.String(), hubReachNoRoute) {
		t.Fatalf("log:\n%s", f.log.String())
	}
	if len(f.w.state.Closed) != 0 {
		t.Fatalf("a fully delivered episode stayed in the list: %+v", f.w.state.Closed)
	}
	f.ticks(20)
	if f.kinds() != "alert,recovery" || len(f.boards) != 1 {
		t.Fatalf("later ticks repeated a delivery: %q, %d board posts", f.kinds(), len(f.boards))
	}
}

// T6: a failing Board post is retried at 5 minute spacing with one identity.
func TestHubReachBoardRetry(t *testing.T) {
	f := newHubReachFake(t, hubReachAt(0, 10, 0))
	f.outage(hubReachNotTunnel)
	fails := 2
	f.boardDoes = func() error {
		if fails > 0 {
			fails--
			return errors.New("hub: 503 busy")
		}
		return nil
	}
	f.up()
	f.ticks(1)
	closed := f.now
	f.ticks(40)
	if len(f.boards) != 3 {
		t.Fatalf("%d board posts", len(f.boards))
	}
	for i, b := range f.boards {
		if b.RequestID != f.boards[0].RequestID || !b.At.Equal(closed.Add(time.Duration(i)*hubReachRetryEvery)) {
			t.Fatalf("post %d at %s with %s", i, b.At.Sub(closed), b.RequestID)
		}
	}
	if want := fmt.Sprintf("hub-reach-testhost-%d-tsk_one", hubReachAt(0, 10, 0).Unix()); f.boards[0].RequestID != want {
		t.Fatalf("request ID %q, want %q", f.boards[0].RequestID, want)
	}
	if len(f.w.state.Closed) != 0 {
		t.Fatalf("still owed: %+v", f.w.state.Closed)
	}
}

// T7: a night outage still open at 07:00 pings once then.
func TestHubReachNightOutageStillOpenAtSeven(t *testing.T) {
	start := hubReachAt(0, 23, 10)
	f := newHubReachFake(t, start)
	f.outage(hubReachNotTunnel)
	if f.marker() == "" || !f.now.Equal(hubReachAt(0, 23, 13)) || f.logCount("HUB UNREACHABLE") != 1 {
		t.Fatalf("no marker at 23:13: now %s", hubReachStamp(f.now))
	}
	if e := f.w.state.Open; e.OwnerVisible || e.Alert.State != hubReachNotOwed {
		t.Fatalf("a night episode is the owner's at once: %+v", e)
	}
	for at := hubReachAt(0, 23, 20); at.Before(hubReachAt(1, 6, 59)); at = at.Add(10 * time.Minute) {
		f.at(at)
	}
	f.at(hubReachAt(1, 6, 59).Add(30 * time.Second))
	if len(f.sends) != 0 || len(f.finds) != 0 {
		t.Fatalf("Discord called before 07:00: %q", f.kinds())
	}
	f.at(hubReachAt(1, 7, 0))
	if f.kinds() != "alert" || !f.sendAt[0].Equal(hubReachAt(1, 7, 0)) || !f.w.state.Open.OwnerVisible {
		t.Fatalf("at 07:00: sends %q", f.kinds())
	}
	f.ticks(10)
	f.up()
	f.at(hubReachAt(1, 9, 0))
	f.ticks(20)
	if f.kinds() != "alert,recovery" || len(f.boards) != 1 {
		t.Fatalf("sends %q, %d board posts", f.kinds(), len(f.boards))
	}
	if !strings.Contains(f.sends[1].Text, "outage began at "+hubReachStamp(start)) {
		t.Fatalf("recovery text:\n%s", f.sends[1].Text)
	}
}

// T8: an outage inside one night never reaches Discord.
func TestHubReachOutageInsideOneNight(t *testing.T) {
	f := newHubReachFake(t, hubReachAt(0, 23, 10))
	f.outage(hubReachNotTunnel)
	for at := hubReachAt(0, 23, 20); at.Before(hubReachAt(1, 2, 0)); at = at.Add(10 * time.Minute) {
		f.at(at)
	}
	f.up()
	f.at(hubReachAt(1, 2, 0))
	if f.marker() != "" || len(f.boards) != 1 || !f.boards[0].At.Equal(hubReachAt(1, 2, 0)) {
		t.Fatalf("marker %q, board posts %+v", f.marker(), f.boards)
	}
	for at := hubReachAt(1, 2, 1); at.Before(hubReachAt(1, 8, 0)); at = at.Add(time.Minute) {
		f.at(at)
	}
	if len(f.sends) != 0 || len(f.finds) != 0 || len(f.boards) != 1 {
		t.Fatalf("Discord called for a night-only outage: %q", f.kinds())
	}
	if !strings.Contains(f.log.String(), "HUB REACHABLE again") {
		t.Fatalf("log:\n%s", f.log.String())
	}
}

// T9: a relay restart mid-episode sends nothing twice.
func TestHubReachRestartMidEpisode(t *testing.T) {
	start := hubReachAt(0, 10, 0)
	f := newHubReachFake(t, start)
	f.outage(hubReachNotTunnel)
	f.reload()
	f.ticks(4)
	if f.kinds() != "alert" || f.logCount("HUB UNREACHABLE") != 1 || f.marker() == "" {
		t.Fatalf("after restart: sends %q", f.kinds())
	}
	f.reload()
	f.up()
	f.at(start.Add(20 * time.Minute))
	if f.kinds() != "alert,recovery" || !strings.Contains(f.sends[1].Text, "duration 20m0s") || f.marker() != "" {
		t.Fatalf("sends %q", f.kinds())
	}
	if got := f.boards[0].Episode; !got.FailingSince.Equal(start) {
		t.Fatalf("episode start %s", got.FailingSince)
	}

	// A streak that had not opened restarts after a stop longer than the gap.
	g := newHubReachFake(t, start)
	g.down(hubReachNotTunnel)
	g.step()
	g.ticks(5)
	g.reload()
	g.at(g.now.Add(10 * time.Minute))
	if g.w.state.Open != nil || len(g.sends) != 0 {
		t.Fatalf("a stop opened an episode: %+v", g.w.state.Open)
	}
}

const hubReachRouteHealthy = `   route to: 100.116.238.37
destination: 100.116.238.37
  interface: utun8
      flags: <UP,HOST,DONE,WASCLONED,IFSCOPE,IFREF>
 recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire
       0         0         0         0         0         0      1280         0
`

const hubReachRouteIncident = `   route to: 100.116.238.37
destination: default
       mask: default
    gateway: 192.168.1.1
  interface: en0
      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,GLOBAL>
`

const hubReachDNSHealthy = `DNS configuration

resolver #1
  search domain[0] : tail1234.ts.net
  nameserver[0] : 100.100.100.100
  if_index : 24 (utun8)
  flags    : Supplemental, Request A records

resolver #2
  nameserver[0] : 192.168.1.1
  if_index : 11 (en0)
`

const hubReachDNSIncident = `DNS configuration

resolver #1
  nameserver[0] : 192.168.1.1
  if_index : 11 (en0)
  flags    : Request A records
`

// T10: the probe parsers.
func TestHubReachProbeParsers(t *testing.T) {
	for out, want := range map[string]string{
		hubReachRouteHealthy:                               hubReachTunnel,
		hubReachRouteIncident:                              hubReachNotTunnel,
		"route: writing to routing socket: not in table\n": hubReachNotTunnel,
		"garbage": hubReachUnknownProbe,
		"":        hubReachUnknownProbe,
	} {
		if got := hubReachParseRoute(out); got != want {
			t.Errorf("route %q: %s, want %s", out, got, want)
		}
	}
	for out, want := range map[string]string{
		hubReachDNSHealthy:  hubReachDNSPresent,
		hubReachDNSIncident: hubReachDNSAbsent,
		"DNS configuration\n\nNo DNS configuration available\n": hubReachDNSAbsent,
		"garbage": hubReachUnknownProbe,
		"":        hubReachUnknownProbe,
	} {
		if got := hubReachParseDNS(out); got != want {
			t.Errorf("dns %q: %s, want %s", out, got, want)
		}
	}
	for host, want := range map[string]bool{
		"100.116.238.37": true, "100.64.0.1": true, "100.128.0.1": false, "192.168.1.5": false,
		"fd7a:115c:a1e0::1": true, "fd00::1": false, "hub.tail1234.ts.net": true, "Hub.Tail1234.TS.net.": true,
		"tailos.example.com": false, "localhost": false,
	} {
		if got := hubReachTailnetHost(host); got != want {
			t.Errorf("tailnet host %q: %v", host, got)
		}
	}
	// A hub outside the tailnet is not asked about, on any system.
	if got := nativeHubReachRoute(context.Background(), "192.168.1.5"); got != hubReachUnknownProbe {
		t.Errorf("route for a host outside the tailnet: %s", got)
	}
}

// hubReachDiscordFake is a small Discord: the DM channel, message creation
// with the enforced nonce, and the channel read.
type hubReachDiscordFake struct {
	t  *testing.T
	mu sync.Mutex
	// messageDoes decides the next message creations, one entry each:
	// "", "401", "500", "drop" (no answer, not stored), "store-drop" (stored,
	// then no answer). After the list is used up every creation succeeds.
	messageDoes []string
	readFails   bool
	stored      []map[string]any
	requests    []string // "METHOD path", in order
	bodies      []map[string]any
	server      *httptest.Server
	nextID      int64
}

func newHubReachDiscordFake(t *testing.T) *hubReachDiscordFake {
	d := &hubReachDiscordFake{t: t, nextID: hubReachSnowflakeAt(hubReachAt(0, 12, 0))}
	d.server = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.server.Close)
	return d
}

func hubReachSnowflakeAt(at time.Time) int64 { return (at.UnixMilli() - discordEpoch) << 22 }

func hubReachDropConnection(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

func (d *hubReachDiscordFake) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.Header.Get("Authorization") != "Bot "+hubReachTestToken {
		d.t.Errorf("%s %s: authorization header is not the bot token", r.Method, r.URL.Path)
	}
	var body map[string]any
	if r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
	}
	d.requests = append(d.requests, r.Method+" "+r.URL.Path)
	d.bodies = append(d.bodies, body)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && r.URL.Path == "/users/@me":
		fmt.Fprintf(w, `{"id":%q,"username":"Tailterm Broker","bot":true}`, hubReachTestBot)
	case r.Method == "POST" && r.URL.Path == "/users/@me/channels":
		fmt.Fprint(w, `{"id":"555","type":1}`)
	case r.Method == "POST" && r.URL.Path == "/channels/555/messages":
		does := ""
		if len(d.messageDoes) > 0 {
			does, d.messageDoes = d.messageDoes[0], d.messageDoes[1:]
		}
		switch does {
		case "401":
			w.WriteHeader(401)
			fmt.Fprint(w, `{"code":0,"message":"401: Unauthorized"}`)
			return
		case "500":
			w.WriteHeader(500)
			fmt.Fprint(w, `{"code":0,"message":"500: Internal Server Error"}`)
			return
		case "drop":
			hubReachDropConnection(w)
			return
		}
		// Discord returns the earlier message for a repeated enforced nonce.
		if body["enforce_nonce"] == true {
			for _, m := range d.stored {
				if m["nonce"] == body["nonce"] {
					json.NewEncoder(w).Encode(m)
					return
				}
			}
		}
		d.nextID += 1 << 22
		m := map[string]any{"id": fmt.Sprint(d.nextID), "channel_id": "555", "content": body["content"], "nonce": body["nonce"], "author": map[string]any{"id": hubReachTestBot, "bot": true}}
		d.stored = append(d.stored, m)
		if does == "store-drop" {
			hubReachDropConnection(w)
			return
		}
		json.NewEncoder(w).Encode(m)
	case r.Method == "GET" && r.URL.Path == "/channels/555/messages":
		if d.readFails {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"code":0,"message":"500: Internal Server Error"}`)
			return
		}
		if r.URL.Query().Get("limit") != "100" || r.URL.Query().Get("after") == "" {
			d.t.Errorf("channel read query %q", r.URL.RawQuery)
		}
		// Newest first, with one message that is not the bot's.
		out := []map[string]any{{"id": "1", "channel_id": "555", "content": "ref hub-reach alert someone-else", "author": map[string]any{"id": hubReachTestOwner}}}
		for i := len(d.stored) - 1; i >= 0; i-- {
			out = append(out, d.stored[i])
		}
		json.NewEncoder(w).Encode(out)
	default:
		d.t.Errorf("unexpected Discord request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}
}

func (d *hubReachDiscordFake) count(request string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, r := range d.requests {
		if r == request {
			n++
		}
	}
	return n
}

func (d *hubReachDiscordFake) seen() (requests []string, bodies []map[string]any, stored []map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...), append([]map[string]any(nil), d.bodies...), append([]map[string]any(nil), d.stored...)
}

func (d *hubReachDiscordFake) does(next ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.messageDoes = next
}

func (d *hubReachDiscordFake) contents() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, m := range d.stored {
		out = append(out, fmt.Sprint(m["content"]))
	}
	return out
}

// withDiscord sends through the real sender to the fake Discord.
func (f *hubReachFake) withDiscord(d *hubReachDiscordFake) {
	if err := os.WriteFile(f.settings.TokenFile, []byte(hubReachTestToken+"\n"), 0600); err != nil {
		f.t.Fatal(err)
	}
	sender := &hubReachDiscord{base: d.server.URL, log: &f.log}
	deps := f.deps()
	fakeSend, fakeFind := deps.send, deps.find
	deps.send = func(ctx context.Context, s hubReachSettings, m hubReachMessage) hubReachOutcome {
		fakeSend(ctx, s, m)
		return sender.send(ctx, s, m)
	}
	deps.find = func(ctx context.Context, s hubReachSettings, m hubReachMessage) (bool, error) {
		fakeFind(ctx, s, m)
		return sender.find(ctx, s, m)
	}
	f.w = newHubReachWatch(deps)
	f.reloadWith = func() { f.w = newHubReachWatch(deps) }
}

// noToken fails the test if the fake token reached a log, the marker or a
// state file.
func (f *hubReachFake) noToken() {
	f.t.Helper()
	if strings.Contains(f.log.String(), hubReachTestToken) {
		f.t.Fatalf("the token is in the log:\n%s", f.log.String())
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, entry := range entries {
		data, _ := os.ReadFile(filepath.Join(f.dir, entry.Name()))
		if bytes.Contains(data, []byte(hubReachTestToken)) {
			f.t.Fatalf("the token is in %s", entry.Name())
		}
	}
}

// T11: the Discord sender's requests.
func TestHubReachDiscordSender(t *testing.T) {
	d := newHubReachDiscordFake(t)
	f := newHubReachFake(t, hubReachAt(0, 10, 0))
	f.withDiscord(d)
	f.outage(hubReachNotTunnel)
	requests, bodies, _ := d.seen()
	if got := strings.Join(requests, "; "); got != "POST /users/@me/channels; POST /channels/555/messages" {
		t.Fatalf("requests %q", got)
	}
	if bodies[0]["recipient_id"] != hubReachTestOwner {
		t.Fatalf("DM body %+v", bodies[0])
	}
	body := bodies[1]
	mentions, _ := json.Marshal(body["allowed_mentions"])
	if body["nonce"] != f.sends[0].Nonce || body["enforce_nonce"] != true || body["content"] != f.sends[0].Text || string(mentions) != fmt.Sprintf(`{"parse":[],"users":[%q]}`, hubReachTestOwner) {
		t.Fatalf("message body %+v", body)
	}
	if len(f.sends[0].Nonce) > 25 {
		t.Fatalf("nonce %q is longer than Discord allows", f.sends[0].Nonce)
	}
	if f.w.state.Open.Alert.State != hubReachDelivered {
		t.Fatalf("alert %+v", f.w.state.Open.Alert)
	}
	f.noToken()

	// A token file others can read is used, with one warning of path and mode.
	if err := os.Chmod(f.settings.TokenFile, 0644); err != nil {
		t.Fatal(err)
	}
	f.up()
	f.ticks(1)
	if f.kinds() != "alert,recovery" || f.logCount("has mode 0644") != 1 || !strings.Contains(f.log.String(), f.settings.TokenFile) {
		t.Fatalf("sends %q, log:\n%s", f.kinds(), f.log.String())
	}
	f.noToken()

	// The snowflake of a time, for the read's lower bound.
	if got := hubReachSnowflake(time.UnixMilli(discordEpoch + 1000)); got != fmt.Sprint(1000<<22) {
		t.Fatalf("snowflake %s", got)
	}
}

// T12: Discord errors and a lost answer.
func TestHubReachDiscordErrors(t *testing.T) {
	d := newHubReachDiscordFake(t)
	d.does("401", "500", "drop")
	f := newHubReachFake(t, hubReachAt(0, 10, 0))
	f.withDiscord(d)
	f.outage(hubReachNotTunnel) // 401
	if got := f.w.state.Open.Alert; got.State != hubReachOwed || got.Attempts != 1 {
		t.Fatalf("after 401: %+v", got)
	}
	f.ticks(1) // 500, on the next tick
	if got := f.w.state.Open.Alert; got.State != hubReachOwed || got.Attempts != 2 || d.count("POST /channels/555/messages") != 2 {
		t.Fatalf("after 500: %+v", got)
	}
	f.ticks(1) // no answer
	if got := f.w.state.Open.Alert; got.State != hubReachUnknown || d.count("GET /channels/555/messages") != 0 {
		t.Fatalf("after no answer: %+v", got)
	}
	if !strings.Contains(f.marker(), "owner alert on Discord: unknown") {
		t.Fatalf("marker:\n%s", f.marker())
	}
	f.ticks(1) // reads, finds nothing, sends
	if got := f.w.state.Open.Alert; got.State != hubReachDelivered {
		t.Fatalf("after the read: %+v", got)
	}
	seen, _, _ := d.seen()
	requests := strings.Join(seen, "; ")
	if !strings.HasSuffix(requests, "GET /users/@me; POST /users/@me/channels; GET /channels/555/messages; POST /users/@me/channels; POST /channels/555/messages") {
		t.Fatalf("the read did not come before the send: %s", requests)
	}
	if len(d.contents()) != 1 || d.count("POST /channels/555/messages") != 4 {
		t.Fatalf("stored %d messages", len(d.contents()))
	}
	for _, part := range []string{"discord: 401", "discord: 500"} {
		if !strings.Contains(f.log.String(), part) {
			t.Fatalf("log lacks %q:\n%s", part, f.log.String())
		}
	}
	f.noToken()

	// Discord not reachable at all: the DM channel cannot be opened, so
	// nothing was created and the alert stays owed.
	g := newHubReachFake(t, hubReachAt(0, 10, 0))
	g.withDiscord(d)
	d.server.Close()
	g.outage(hubReachNotTunnel)
	if got := g.w.state.Open.Alert; got.State != hubReachOwed || g.marker() == "" {
		t.Fatalf("with Discord down: %+v", got)
	}
	g.noToken()

	// No token file: nothing sent, and the path is not an error to hide.
	h := newHubReachFake(t, hubReachAt(0, 10, 0))
	h.w = newHubReachWatch(func() hubReachDeps {
		deps := h.deps()
		sender := &hubReachDiscord{base: "http://127.0.0.1:1", log: &h.log}
		deps.send, deps.find = sender.send, sender.find
		return deps
	}())
	h.outage(hubReachNotTunnel)
	if got := h.w.state.Open.Alert; got.State != hubReachOwed || !strings.Contains(h.log.String(), "token file cannot be read") {
		t.Fatalf("with no token file: %+v\n%s", got, h.log.String())
	}
}

type hubReachHubFake struct {
	mu       sync.Mutex
	agents   []api.Agent
	primary  string
	posts    []api.PostMessageRequest
	headers  []http.Header
	conflict bool
	server   *httptest.Server
}

func newHubReachHubFake(t *testing.T) (*hubReachHubFake, *api.Client) {
	h := &hubReachHubFake{}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/tasks/tsk_one/agents":
			json.NewEncoder(w).Encode(api.AgentList{Agents: h.agents})
		case r.Method == "GET" && r.URL.Path == "/v1/tasks":
			json.NewEncoder(w).Encode(api.TaskList{Tasks: []api.Task{{ID: "tsk_other", PrimaryHandlerID: "agt_wrong"}, {ID: "tsk_one", PrimaryHandlerID: h.primary}}})
		case r.Method == "POST" && r.URL.Path == "/v1/tasks/tsk_one/messages":
			var req api.PostMessageRequest
			json.NewDecoder(r.Body).Decode(&req)
			h.posts = append(h.posts, req)
			h.headers = append(h.headers, r.Header.Clone())
			if h.conflict {
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"error":"request ID stored with other data"}`)
				return
			}
			json.NewEncoder(w).Encode(api.Message{Seq: 7})
		default:
			t.Errorf("unexpected hub request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(h.server.Close)
	c, err := api.NewClient(h.server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return h, c
}

func (h *hubReachHubFake) set(agents []api.Agent, primary string, conflict bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agents, h.primary, h.conflict, h.posts = agents, primary, conflict, nil
}

func (h *hubReachHubFake) got() []api.PostMessageRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]api.PostMessageRequest(nil), h.posts...)
}

// T13: the Board recovery notice is always directed.
func TestHubReachBoardPost(t *testing.T) {
	start := hubReachAt(0, 10, 0)
	e := hubReachEpisode{ID: "testhost-1", Host: "testhost", Hub: "100.116.238.37", FailingSince: start, OpenedAt: start.Add(3 * time.Minute), ClosedAt: start.Add(13 * time.Minute), Class: hubReachNoRoute, Detail: "timeout", Route: hubReachNotTunnel, DNS: hubReachDNSAbsent}
	helper := api.Agent{ID: "agt_helper", Name: "owner-helper", Role: api.AgentRoleOwnerHelper, Status: "running"}
	handler := api.Agent{ID: "agt_handler", Name: "db-handler-o5", Role: api.AgentRoleDatabaseHandler, Status: "done"}
	other := api.Agent{ID: "agt_other", Name: "db-handler-item", Role: api.AgentRoleDatabaseHandler, Status: "running"}
	ctx := context.Background()

	h, c := newHubReachHubFake(t)
	h.set([]api.Agent{other, handler, helper}, handler.ID, false)
	if err := hubReachBoardPost(ctx, c, e, "tsk_one"); err != nil {
		t.Fatal(err)
	}
	post := h.got()[0]
	if post.To != helper.ID || post.Envelope == nil || post.Envelope.To != helper.Name || post.Envelope.Kind != api.EnvelopeKindNotice || post.RequestID != fmt.Sprintf("hub-reach-testhost-%d-tsk_one", start.Unix()) {
		t.Fatalf("post %+v", post)
	}
	refs := post.Envelope.Refs
	if refs["hubReach"] != "recovered" || refs["host"] != "testhost" || refs["cause"] != hubReachNoRoute || refs["durationSeconds"] != "780" || refs["episode"] != "testhost-1" || len(refs) != 5 {
		t.Fatalf("refs %+v", refs)
	}
	if !strings.Contains(post.Text, "13m0s") || !strings.Contains(post.Text, hubReachNoRoute) || len(post.Envelope.Subject) > 120 {
		t.Fatalf("text %q", post.Text)
	}

	// The helper is closed: the live primary handler, never another handler.
	helper.Status = api.AgentClosed
	h.set([]api.Agent{other, handler, helper}, handler.ID, false)
	if err := hubReachBoardPost(ctx, c, e, "tsk_one"); err != nil || h.got()[0].To != handler.ID || h.got()[0].Envelope.To != handler.Name {
		t.Fatalf("fallback: %v %+v", err, h.got())
	}

	// Neither is live: nothing is posted, and no post goes out undirected.
	handler.Status = api.AgentRetired
	h.set([]api.Agent{other, handler, helper}, handler.ID, false)
	if err := hubReachBoardPost(ctx, c, e, "tsk_one"); !errors.Is(err, errHubReachNoRecipient) || len(h.got()) != 0 {
		t.Fatalf("with no recipient: %v %+v", err, h.got())
	}
	h.set([]api.Agent{other, handler, helper}, "", false)
	if err := hubReachBoardPost(ctx, c, e, "tsk_one"); !errors.Is(err, errHubReachNoRecipient) || len(h.got()) != 0 {
		t.Fatalf("with no primary: %v %+v", err, h.got())
	}

	// A conflict means the request ID is already stored.
	helper.Status = "running"
	h.set([]api.Agent{helper}, "", true)
	if err := hubReachBoardPost(ctx, c, e, "tsk_one"); err != nil || len(h.got()) != 1 {
		t.Fatalf("conflict: %v", err)
	}
	h.server.Close()
	if err := hubReachBoardPost(ctx, c, e, "tsk_one"); err == nil {
		t.Fatal("an unreachable hub counted as posted")
	}

	// The watch keeps the post owed while there is no recipient.
	f := newHubReachFake(t, start)
	f.outage(hubReachNotTunnel)
	f.boardDoes = func() error { return errHubReachNoRecipient }
	f.up()
	f.ticks(1)
	if len(f.w.state.Closed) != 1 || f.w.state.Closed[0].Board[0].State != hubReachOwed {
		t.Fatalf("closed %+v", f.w.state.Closed)
	}

	// The tasks are those of the valid bindings for the watched hub.
	dir := t.TempDir()
	binding := func(hub string, task, n int) runtimeBinding {
		return runtimeBinding{Hub: hub, Task: fmt.Sprintf("tsk_%016d", task), Agent: fmt.Sprintf("agt_%016d", n), Run: fmt.Sprintf("run_%016d", n), Thread: "00000000-0000-4000-8000-000000000001", Codex: "/usr/local/bin/codex"}
	}
	for i, b := range []runtimeBinding{
		binding("http://hub", 2, 1), binding("http://hub", 1, 2), binding("http://hub", 1, 3), binding("http://elsewhere", 3, 4),
	} {
		if !validBinding(b) {
			t.Fatalf("test binding %d is not valid", i)
		}
		if err := writePrivateJSON(filepath.Join(dir, fmt.Sprintf("%d.binding.json", i)), b); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(hubReachTasks(dir, "http://hub"), ","); got != "tsk_0000000000000001,tsk_0000000000000002" {
		t.Fatalf("tasks %q", got)
	}
}

// T14: a gap in the probes restarts the streak.
func TestHubReachGapRestartsTheStreak(t *testing.T) {
	f := newHubReachFake(t, hubReachAt(0, 10, 0))
	f.down(hubReachNotTunnel)
	f.step()
	f.ticks(5)
	f.at(f.now.Add(10 * time.Minute))
	resumed := f.now
	if f.w.state.Open != nil || !f.w.state.FailingSince.Equal(resumed) {
		t.Fatalf("after the gap: failing since %s, episode %+v", f.w.state.FailingSince, f.w.state.Open)
	}
	f.ticks(5)
	if len(f.sends) != 0 || f.w.state.Open != nil {
		t.Fatalf("alert %s after the gap", f.now.Sub(resumed))
	}
	f.ticks(1)
	if f.kinds() != "alert" || !f.w.state.Open.FailingSince.Equal(resumed) {
		t.Fatalf("sends %q", f.kinds())
	}
}

// T15: the watch reads and reports; its source names nothing that would
// change a network setting or touch a session.
func TestHubReachSourceChangesNothing(t *testing.T) {
	data, err := os.ReadFile("hub_reach_watch.go")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ToLower(string(data))
	for _, word := range []string{"tailscale", "send-keys", "networksetup", "ifconfig", "truenas"} {
		if strings.Contains(source, word) {
			t.Errorf("hub_reach_watch.go contains %q", word)
		}
	}
	if n := strings.Count(source, "exec.command"); n != 2 {
		t.Errorf("%d commands are run, want the route lookup and the resolver listing only", n)
	}
	for _, command := range []string{`exec.commandcontext(ctx, "/sbin/route", args...)`, `exec.commandcontext(ctx, "/usr/sbin/scutil", "--dns")`, `args := []string{"-n", "get", host}`, `args = []string{"-n", "get", "-inet6", host}`} {
		if !strings.Contains(source, command) {
			t.Errorf("hub_reach_watch.go lacks %s", command)
		}
	}
}

// T16: no owner configured, and the watch switched off.
func TestHubReachSettings(t *testing.T) {
	f := newHubReachFake(t, hubReachAt(0, 10, 0))
	f.settings.Owner = ""
	f.outage(hubReachNotTunnel)
	f.ticks(10)
	if len(f.sends) != 0 || f.marker() == "" || f.logCount("HUB UNREACHABLE") != 1 || f.logCount("no owner is configured") != 1 {
		t.Fatalf("with no owner: sends %d, log:\n%s", len(f.sends), f.log.String())
	}
	if f.hubCalls != 17 || f.probes != 17 {
		t.Fatalf("%d hub calls and %d route probes in 17 ticks", f.hubCalls, f.probes)
	}
	// The owner is configured while the episode is open.
	f.settings.Owner = hubReachTestOwner
	f.ticks(1)
	if f.kinds() != "alert" {
		t.Fatalf("sends %q", f.kinds())
	}

	off := newHubReachFake(t, hubReachAt(0, 10, 0))
	off.settings.Off = true
	off.outage(hubReachNotTunnel)
	if off.hubCalls != 0 || off.probes != 0 || off.marker() != "" {
		t.Fatalf("a switched-off watch made %d hub calls", off.hubCalls)
	}

	// A healthy host runs no route or resolver probe.
	ok := newHubReachFake(t, hubReachAt(0, 10, 0))
	ok.ticks(10)
	if ok.hubCalls != 10 || ok.probes != 0 {
		t.Fatalf("healthy: %d hub calls, %d probes", ok.hubCalls, ok.probes)
	}

	// With no hub configured the watch idles and says so once.
	idle := newHubReachFake(t, hubReachAt(0, 10, 0))
	deps := idle.deps()
	deps.hubURL = func() string { return "" }
	idle.w = newHubReachWatch(deps)
	idle.ticks(3)
	if idle.hubCalls != 0 || idle.logCount("the watch is idle") != 1 {
		t.Fatalf("idle: %d hub calls, log %q", idle.hubCalls, idle.log.String())
	}

	// The three keys of relay.json, by exact name.
	raw := func(v string) json.RawMessage { data, _ := json.Marshal(v); return data }
	got := hubReachSettingsFrom(map[string]json.RawMessage{"hubReachDiscordOwner": raw(hubReachTestOwner), "hubReachDiscordTokenFile": raw("~/secrets/bot"), "hubReachWatch": raw("off")}, "/home/u")
	if got != (hubReachSettings{Off: true, Owner: hubReachTestOwner, TokenFile: "/home/u/secrets/bot"}) {
		t.Fatalf("settings %+v", got)
	}
	got = hubReachSettingsFrom(map[string]json.RawMessage{"hubreachdiscordowner": raw(hubReachTestOwner), "hubReachWatch": raw("OFF")}, "/home/u")
	if got != (hubReachSettings{TokenFile: "/home/u/.config/discord/token"}) {
		t.Fatalf("default settings %+v", got)
	}
	for _, owner := range []string{"1234", "12345678901234567x", "123456789012345678901"} {
		if got := hubReachSettingsFrom(map[string]json.RawMessage{"hubReachDiscordOwner": raw(owner)}, "/home/u"); got.Owner != "" {
			t.Fatalf("owner %q accepted", owner)
		}
	}
}

// T17: the night boundary follows America/Los_Angeles across a clock change.
func TestHubReachNightFollowsPacificTime(t *testing.T) {
	for _, c := range []struct {
		utc   string
		night bool
	}{
		{"2027-03-13T14:59:59Z", true},  // 06:59 PST, the day before spring forward
		{"2027-03-13T15:00:00Z", false}, // 07:00 PST
		{"2027-03-14T13:59:59Z", true},  // 06:59 PDT, the spring-forward date
		{"2027-03-14T14:00:00Z", false}, // 07:00 PDT
		{"2027-03-14T15:00:00Z", false}, // 08:00 PDT
		{"2027-03-15T04:59:59Z", false}, // 21:59 PDT
		{"2027-03-15T05:00:00Z", true},  // 22:00 PDT
		{"2026-11-01T14:59:59Z", true},  // 06:59 PST, the fall-back date
		{"2026-11-01T15:00:00Z", false}, // 07:00 PST
	} {
		at, err := time.Parse(time.RFC3339, c.utc)
		if err != nil {
			t.Fatal(err)
		}
		if got := hubReachNight(at); got != c.night {
			t.Errorf("%s (%s): night=%v", c.utc, hubReachStamp(at), got)
		}
	}
}

// T18: a recovery 30 seconds after the alert is its own message.
func TestHubReachRapidRecoveryIsASecondMessage(t *testing.T) {
	d := newHubReachDiscordFake(t)
	f := newHubReachFake(t, hubReachAt(0, 10, 0))
	f.withDiscord(d)
	f.outage(hubReachNotTunnel)
	f.up()
	f.ticks(1)
	got := d.contents()
	if len(got) != 2 || got[0] == got[1] || !strings.Contains(got[0], "ref hub-reach alert ") || !strings.Contains(got[1], "ref hub-reach recovery ") {
		t.Fatalf("stored messages %q", got)
	}
	_, _, stored := d.seen()
	if stored[0]["nonce"] == stored[1]["nonce"] || f.sends[0].Ref == f.sends[1].Ref {
		t.Fatalf("the alert and the recovery line share an identity: %v %v", stored[0]["nonce"], stored[1]["nonce"])
	}
	f.noToken()
}

// T19: an alert whose answer was lost is found by its reference, hours and a
// restart later, and not sent again.
func TestHubReachLostAnswerIsReadBack(t *testing.T) {
	d := newHubReachDiscordFake(t)
	d.does("store-drop")
	f := newHubReachFake(t, hubReachAt(0, 10, 0))
	f.withDiscord(d)
	f.outage(hubReachNotTunnel)
	if got := f.w.state.Open.Alert.State; got != hubReachUnknown {
		t.Fatalf("alert %s", got)
	}
	f.now = f.now.Add(2 * time.Hour)
	f.reloadWith()
	f.step()
	if got := f.w.state.Open.Alert.State; got != hubReachDelivered {
		t.Fatalf("alert %s after the read", got)
	}
	f.ticks(5)
	if len(d.contents()) != 1 || d.count("POST /channels/555/messages") != 1 || d.count("GET /channels/555/messages") != 1 {
		t.Fatalf("%d messages, %d sends", len(d.contents()), d.count("POST /channels/555/messages"))
	}

	// A relay stopped during a send finds the delivery in attempting.
	g := newHubReachFake(t, hubReachAt(0, 10, 0))
	g.outage(hubReachNotTunnel)
	g.w.state.Open.Alert.State = hubReachAttempting
	g.w.save()
	g.findDoes = func(hubReachMessage) (bool, error) { return true, nil }
	g.reload()
	if got := g.w.state.Open.Alert.State; got != hubReachUnknown {
		t.Fatalf("attempting read back as %s", got)
	}
	g.ticks(1)
	if g.kinds() != "alert" || len(g.finds) != 1 || g.w.state.Open.Alert.State != hubReachDelivered {
		t.Fatalf("sends %q finds %d", g.kinds(), len(g.finds))
	}
}

// T20: a recovery line that got no answer and was not stored is sent once
// more after a read; while the read fails nothing is sent.
func TestHubReachLostRecoveryIsSentAfterARead(t *testing.T) {
	d := newHubReachDiscordFake(t)
	d.does("", "drop")
	f := newHubReachFake(t, hubReachAt(0, 10, 0))
	f.withDiscord(d)
	f.outage(hubReachNotTunnel)
	f.up()
	f.ticks(1)
	if got := f.w.state.Closed[0].Recovery.State; got != hubReachUnknown {
		t.Fatalf("recovery %s", got)
	}
	d.mu.Lock()
	d.readFails = true
	d.mu.Unlock()
	f.ticks(30) // 15 minutes of failed reads, one per 5 minutes
	if got := f.w.state.Closed[0].Recovery; got.State != hubReachUnknown || d.count("POST /channels/555/messages") != 2 || d.count("GET /channels/555/messages") != 3 {
		t.Fatalf("with the read failing: %+v, %d sends, %d reads", got, d.count("POST /channels/555/messages"), d.count("GET /channels/555/messages"))
	}
	d.mu.Lock()
	d.readFails = false
	d.mu.Unlock()
	f.ticks(10)
	got := d.contents()
	if len(got) != 2 || !strings.Contains(got[1], "ref hub-reach recovery ") || d.count("POST /channels/555/messages") != 3 || len(f.w.state.Closed) != 0 {
		t.Fatalf("stored %q, %d sends", got, d.count("POST /channels/555/messages"))
	}
	f.noToken()
}

// T21: two closed episodes each keep their own owed deliveries.
func TestHubReachTwoClosedEpisodesKeepTheirDeliveries(t *testing.T) {
	first := hubReachAt(0, 10, 0)
	f := newHubReachFake(t, first)
	failing := true
	f.sendDoes = func(hubReachMessage) hubReachOutcome {
		if failing {
			return hubReachRejected
		}
		return hubReachSent
	}
	f.boardDoes = func() error {
		if failing {
			return errors.New("hub: 503 busy")
		}
		return nil
	}
	f.outage(hubReachNotTunnel)
	f.up()
	f.ticks(1)
	f.reload()
	second := f.now.Add(hubReachInterval)
	f.now = second
	f.outage(hubReachNotTunnel)
	f.up()
	f.ticks(1)
	if len(f.w.state.Closed) != 2 {
		t.Fatalf("%d closed episodes", len(f.w.state.Closed))
	}
	f.reload()
	failing = false
	f.sends, f.boards = nil, nil
	f.ticks(12)
	if f.kinds() != "recovery,recovery" || len(f.boards) != 2 || len(f.w.state.Closed) != 0 {
		t.Fatalf("sends %q, %d board posts, %d still closed", f.kinds(), len(f.boards), len(f.w.state.Closed))
	}
	if f.sends[0].Nonce != fmt.Sprintf("hrr-%s-%d", hubReachHostHash("testhost"), first.Unix()) || f.sends[1].Nonce != fmt.Sprintf("hrr-%s-%d", hubReachHostHash("testhost"), second.Unix()) || f.sends[0].Ref == f.sends[1].Ref {
		t.Fatalf("recovery identities %q %q", f.sends[0].Nonce, f.sends[1].Nonce)
	}
	if f.boards[0].RequestID == f.boards[1].RequestID || !strings.Contains(f.boards[0].RequestID, fmt.Sprint(first.Unix())) || !strings.Contains(f.boards[1].RequestID, fmt.Sprint(second.Unix())) {
		t.Fatalf("board identities %q %q", f.boards[0].RequestID, f.boards[1].RequestID)
	}
	for _, m := range f.sends {
		if !strings.Contains(m.Text, "No alert was delivered for this outage.") {
			t.Fatalf("recovery text:\n%s", m.Text)
		}
	}
	f.ticks(12)
	if len(f.sends) != 2 || len(f.boards) != 2 {
		t.Fatalf("a delivery was repeated: %d sends, %d board posts", len(f.sends), len(f.boards))
	}
}

// T22: at most 20 closed episodes are kept; the oldest is dropped aloud.
func TestHubReachClosedEpisodesAreBounded(t *testing.T) {
	start := hubReachAt(0, 10, 0)
	f := newHubReachFake(t, start)
	f.boardDoes = func() error { return errors.New("hub: 503 busy") }
	for i := 0; i < hubReachMaxClosed; i++ {
		since := start.Add(time.Duration(i-40) * time.Minute)
		f.w.state.Closed = append(f.w.state.Closed, hubReachEpisode{
			ID: fmt.Sprintf("testhost-%d", since.Unix()), Host: "testhost", FailingSince: since, OpenedAt: since.Add(3 * time.Minute), ClosedAt: since.Add(4 * time.Minute),
			Alert: hubReachDelivery{State: hubReachDropped}, Recovery: hubReachDelivery{State: hubReachNotOwed},
			Board: []hubReachBoardDelivery{{Task: "tsk_one", hubReachDelivery: hubReachDelivery{State: hubReachOwed, FirstSendableAt: start}}},
		})
	}
	oldest := f.w.state.Closed[0].FailingSince
	f.outage(hubReachNotTunnel)
	if f.logCount("dropped the oldest") != 0 {
		t.Fatalf("log:\n%s", f.log.String())
	}
	f.up()
	f.ticks(1)
	if len(f.w.state.Closed) != hubReachMaxClosed || !f.w.state.Closed[hubReachMaxClosed-1].FailingSince.Equal(start) {
		t.Fatalf("%d closed episodes", len(f.w.state.Closed))
	}
	want := "dropped the oldest, which began " + oldest.UTC().Format(time.RFC3339) + "; not delivered: the Board notice for tsk_one"
	if f.logCount("dropped the oldest") != 1 || !strings.Contains(f.log.String(), want) {
		t.Fatalf("log lacks %q:\n%s", want, f.log.String())
	}
	// Recovery lines and Board posts are given up after 24 hours, aloud.
	f.at(f.now.Add(25 * time.Hour))
	if len(f.w.state.Closed) != 0 || f.logCount("given up") != hubReachMaxClosed {
		t.Fatalf("%d still closed, %d given up", len(f.w.state.Closed), f.logCount("given up"))
	}
}

// T23: a recovery at night holds its Discord line until 07:00.
func TestHubReachNightRecoveryWaitsForMorning(t *testing.T) {
	f := newHubReachFake(t, hubReachAt(0, 20, 57))
	f.outage(hubReachNotTunnel)
	if f.kinds() != "alert" || !f.sendAt[0].Equal(hubReachAt(0, 21, 0)) {
		t.Fatalf("sends %q", f.kinds())
	}
	for at := hubReachAt(0, 21, 10); at.Before(hubReachAt(0, 23, 30)); at = at.Add(10 * time.Minute) {
		f.at(at)
	}
	f.up()
	f.at(hubReachAt(0, 23, 30))
	if f.kinds() != "alert" || len(f.boards) != 1 || !f.boards[0].At.Equal(hubReachAt(0, 23, 30)) || f.marker() != "" {
		t.Fatalf("at 23:30: sends %q, board %+v", f.kinds(), f.boards)
	}
	for at := hubReachAt(0, 23, 31); at.Before(hubReachAt(1, 7, 0)); at = at.Add(7 * time.Minute) {
		f.at(at)
	}
	f.at(hubReachAt(1, 6, 59).Add(30 * time.Second))
	if f.kinds() != "alert" {
		t.Fatalf("before 07:00: sends %q", f.kinds())
	}
	f.at(hubReachAt(1, 7, 0))
	f.ticks(20)
	if f.kinds() != "alert,recovery" || !f.sendAt[1].Equal(hubReachAt(1, 7, 0)) || len(f.finds) != 0 {
		t.Fatalf("sends %q at %v", f.kinds(), f.sendAt)
	}
	if !strings.Contains(f.sends[1].Text, "duration 2h33m0s") {
		t.Fatalf("recovery text:\n%s", f.sends[1].Text)
	}
}

// T24: a send that failed at 21:59 is not retried at 22:00.
func TestHubReachFailedSendIsHeldThroughTheNight(t *testing.T) {
	for _, recovers := range []bool{false, true} {
		f := newHubReachFake(t, hubReachAt(0, 21, 56))
		f.sendDoes = func(hubReachMessage) hubReachOutcome { return hubReachRejected }
		f.outage(hubReachNotTunnel) // opens at 21:59
		f.ticks(1)                  // 21:59:30
		if len(f.sends) != 2 || f.w.state.Open.Alert.State != hubReachOwed {
			t.Fatalf("before 22:00: %d sends", len(f.sends))
		}
		f.sendDoes = nil
		f.sends, f.sendAt = nil, nil
		f.ticks(4) // 22:00 to 22:01:30
		for at := hubReachAt(0, 22, 10); at.Before(hubReachAt(1, 3, 0)); at = at.Add(10 * time.Minute) {
			f.at(at)
		}
		if recovers {
			f.up()
		}
		for at := hubReachAt(1, 3, 0); at.Before(hubReachAt(1, 7, 0)); at = at.Add(10 * time.Minute) {
			f.at(at)
		}
		f.at(hubReachAt(1, 6, 59).Add(30 * time.Second))
		if len(f.sends) != 0 || len(f.finds) != 0 {
			t.Fatalf("recovers=%v: Discord called in the night: %q", recovers, f.kinds())
		}
		f.at(hubReachAt(1, 7, 0))
		f.ticks(20)
		if !recovers {
			if f.kinds() != "alert" || !f.sendAt[0].Equal(hubReachAt(1, 7, 0)) {
				t.Fatalf("still down: sends %q", f.kinds())
			}
			continue
		}
		if f.kinds() != "recovery" || !f.sendAt[0].Equal(hubReachAt(1, 7, 0)) || !strings.Contains(f.sends[0].Text, "No alert was delivered for this outage.") {
			t.Fatalf("recovered at 03:00: sends %q", f.kinds())
		}
		if len(f.boards) != 1 || !f.boards[0].At.Equal(hubReachAt(1, 3, 0)) {
			t.Fatalf("board posts %+v", f.boards)
		}
	}
}

// T25: an outage whose every alert attempt failed still gets its one
// recovery line, and no alert after recovery.
func TestHubReachRecoveryLineWithoutADeliveredAlert(t *testing.T) {
	start := hubReachAt(0, 10, 0)
	f := newHubReachFake(t, start)
	f.sendDoes = func(hubReachMessage) hubReachOutcome { return hubReachRejected }
	f.outage(hubReachNotTunnel)
	f.ticks(60) // down 33 minutes
	attempts := len(f.sends)
	// Ten on consecutive ticks, then one per 5 minutes.
	if attempts != 15 || f.kinds() != strings.TrimSuffix(strings.Repeat("alert,", attempts), ",") {
		t.Fatalf("%d alert attempts: %q", attempts, f.kinds())
	}
	if gap := f.sendAt[10].Sub(f.sendAt[9]); gap != hubReachRetryEvery {
		t.Fatalf("attempt 11 came %s after attempt 10", gap)
	}
	f.sendDoes = nil
	f.sends = nil
	f.up()
	f.ticks(1)
	f.ticks(30)
	if f.kinds() != "recovery" {
		t.Fatalf("after recovery: sends %q", f.kinds())
	}
	text := f.sends[0].Text
	if !strings.Contains(text, "outage began at "+hubReachStamp(start)) || !strings.Contains(text, "No alert was delivered for this outage.") || strings.Contains(text, "may not have been delivered") || !strings.Contains(text, "duration 33m30s") {
		t.Fatalf("recovery text:\n%s", text)
	}
}

// T26: the hub call reports the raw status and parses no body.
func TestHubReachProbeStatuses(t *testing.T) {
	followed := 0
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) { followed++ })
	status := 200
	body := ""
	mux.HandleFunc("/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if status == 302 {
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	ctx := context.Background()
	for _, c := range []struct {
		status int
		body   string
		ok     bool
	}{
		{401, `{"error":"no"}`, false},
		{503, "busy", false},
		{304, "", false},
		{302, "", false},
		{200, `{"node": not json`, true},
		{200, "", true},
		{204, "", true},
		{299, "x", true},
		{300, "", false},
	} {
		status, body = c.status, c.body
		call := hubReachProbe(ctx, server.URL+"/", "hub-token")
		if call.Err != nil || call.Status != c.status || call.ok() != c.ok {
			t.Errorf("status %d: %+v ok=%v", c.status, call, call.ok())
		}
		if !c.ok {
			if class, detail := hubReachClassify(call, hubReachTunnel, hubReachDNSPresent); class != hubReachHTTPError || detail != fmt.Sprint(c.status) {
				t.Errorf("status %d: class %s (%s)", c.status, class, detail)
			}
		}
	}
	if followed != 0 {
		t.Fatalf("the redirect was followed %d times", followed)
	}
	if gotAuth != "Bearer hub-token" {
		t.Fatalf("authorization %q", gotAuth)
	}
	// A closed port: no HTTP response.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + listener.Addr().String()
	listener.Close()
	call := hubReachProbe(ctx, closed, "hub-token")
	if call.Err == nil || call.ok() || strings.Contains(call.Err.Error(), "hub-token") {
		t.Fatalf("closed port: %+v", call)
	}
	if class, detail := hubReachClassify(call, hubReachTunnel, hubReachDNSPresent); class != hubReachTCPNoAnswer || detail != "refused" {
		t.Fatalf("closed port: class %s (%s)", class, detail)
	}
	if (hubReachCall{Status: 199}).ok() || (hubReachCall{Status: 200, Err: errors.New("x")}).ok() {
		t.Fatal("a status outside 200-299 or an error counted as a success")
	}
}

// T27: the relay starts the watch once for the long-running loop only, and
// reports it in --status.
func TestHubReachRelayHookAndStatus(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "relay")
	t.Setenv("TAILTERM_RELAY_STATE", dir)
	t.Setenv("TAILTERM_HUB", "")
	oldHandler, oldSteward, oldPause, oldStart := relayHandlerRotation, relayStewardRotation, relayLoopPause, relayHubReachStart
	t.Cleanup(func() {
		relayHandlerRotation, relayStewardRotation, relayLoopPause, relayHubReachStart = oldHandler, oldSteward, oldPause, oldStart
	})
	relayHandlerRotation = func(context.Context) error { return nil }
	relayStewardRotation = func(context.Context) error { return nil }
	var started, stopped int
	var startedDir string
	relayHubReachStart = func(dir string) func() {
		started++
		startedDir = dir
		return func() { stopped++ }
	}
	for _, flag := range []string{"--once", "--status"} {
		if _, err := captureRelayOutput(t, true, func() error { return cmdRelay([]string{flag}) }); err != nil {
			t.Fatalf("relay %s: %v", flag, err)
		}
	}
	if started != 0 {
		t.Fatalf("the watch started %d times under --once or --status", started)
	}
	passes := 0
	relayLoopPause = func() bool {
		passes++
		if started != 1 || stopped != 0 {
			t.Errorf("in pass %d: started %d, stopped %d", passes, started, stopped)
		}
		return passes < 3
	}
	if _, err := captureRelayOutput(t, true, func() error { return cmdRelay(nil) }); err != nil {
		t.Fatalf("relay loop: %v", err)
	}
	if started != 1 || stopped != 1 || startedDir != dir || passes != 3 {
		t.Fatalf("started %d, stopped %d, dir %q, passes %d", started, stopped, startedDir, passes)
	}

	out, err := captureRelayOutput(t, false, func() error { return cmdRelay([]string{"--status"}) })
	if err != nil || !strings.Contains(out, "hub-reach: ok\n") || strings.Contains(out, "closed episodes") {
		t.Fatalf("status with no state: %v\n%s", err, out)
	}
	since := hubReachAt(0, 10, 0)
	state := hubReachState{
		Open:   &hubReachEpisode{FailingSince: since, Class: hubReachNoRoute, OwnerVisible: true, Alert: hubReachDelivery{State: hubReachDelivered}},
		Closed: []hubReachEpisode{{ID: "a"}, {ID: "b"}},
	}
	if err := writePrivateJSON(filepath.Join(dir, hubReachStateFile), state); err != nil {
		t.Fatal(err)
	}
	out, err = captureRelayOutput(t, false, func() error { return cmdRelay([]string{"--status"}) })
	want := "hub-reach: UNREACHABLE since " + since.UTC().Format(time.RFC3339) + " cause=no_tunnel_route visible=yes alert=delivered\nhub-reach: 2 closed episodes with deliveries owed\n"
	if err != nil || !strings.Contains(out, want) || strings.Contains(out, "hub-reach: ok") {
		t.Fatalf("status with an open episode: %v\n%s", err, out)
	}
	state.Open.OwnerVisible, state.Open.Alert.State, state.Closed = false, hubReachNotOwed, nil
	if err := writePrivateJSON(filepath.Join(dir, hubReachStateFile), state); err != nil {
		t.Fatal(err)
	}
	if lines := hubReachStatusLines(dir); len(lines) != 1 || !strings.HasSuffix(lines[0], "visible=no alert=not-owed") {
		t.Fatalf("status lines %q", lines)
	}
}

// The real start and stop: the goroutine takes its first step at once and
// stop waits for it. With no hub.json under the test home it idles, so no
// connection is opened.
func TestHubReachStartAndStop(t *testing.T) {
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config", "tailterm", "hub.json")); err == nil {
		t.Skip("the test home has a hub.json")
	}
	dir := t.TempDir()
	stderr, err := captureStream(t, &os.Stderr, func() error {
		stop := startHubReachWatch(dir)
		stop()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "HUB UNREACHABLE") || strings.Count(stderr, "the watch is idle") != 1 {
		t.Fatalf("stderr:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, hubReachMarkerFile)); err == nil {
		t.Fatal("a marker was written")
	}
}

// An alert whose answer was lost and never settled by a read is unconfirmed
// at recovery, not dropped: the recovery line says it may not have been
// delivered, and the alert is neither sent nor read after the close.
func TestHubReachUnconfirmedAlertAtRecovery(t *testing.T) {
	f := newHubReachFake(t, hubReachAt(0, 10, 0))
	f.sendDoes = func(m hubReachMessage) hubReachOutcome {
		if m.Kind == "alert" {
			return hubReachNoAnswer
		}
		return hubReachSent
	}
	f.findDoes = func(hubReachMessage) (bool, error) { return false, errors.New("read: 503") }
	f.outage(hubReachNotTunnel)
	f.ticks(3)
	if got := f.w.state.Open.Alert.State; got != hubReachUnknown || len(f.finds) != 3 {
		t.Fatalf("alert %s, %d reads", got, len(f.finds))
	}
	sends, finds := len(f.sends), len(f.finds)
	f.up()
	f.ticks(1)
	if f.kinds() != strings.Repeat("alert,", sends)+"recovery" || len(f.finds) != finds {
		t.Fatalf("at recovery: sends %q, %d reads (was %d)", f.kinds(), len(f.finds), finds)
	}
	rec := f.sends[len(f.sends)-1].Text
	if !strings.Contains(rec, "may not have been delivered") || strings.Contains(rec, "No alert was delivered") {
		t.Fatalf("recovery text:\n%s", rec)
	}
	if len(f.w.state.Closed) != 0 {
		t.Fatalf("an unconfirmed alert kept the episode owed: %+v", f.w.state.Closed)
	}
	f.ticks(30)
	if len(f.sends) != sends+1 || len(f.finds) != finds {
		t.Fatalf("after the close: sends %q, %d reads", f.kinds(), len(f.finds))
	}

	// The recovery line is owed but not yet sent: the closed episode keeps
	// the alert unconfirmed, and only the recovery line is sent or read.
	g := newHubReachFake(t, hubReachAt(0, 10, 0))
	g.sendDoes = func(hubReachMessage) hubReachOutcome { return hubReachRejected }
	g.outage(hubReachNotTunnel)
	g.w.state.Open.Alert.State = hubReachAttempting // a relay stopped mid-send
	g.w.save()
	g.reload()
	g.findDoes = func(hubReachMessage) (bool, error) { return false, errors.New("read: 503") }
	g.up()
	g.ticks(1)
	if len(g.w.state.Closed) != 1 || g.w.state.Closed[0].Alert.State != hubReachUnconfirmed || g.w.state.Closed[0].Alert.pending() {
		t.Fatalf("closed episodes %+v", g.w.state.Closed)
	}
	g.reload()
	g.sends, g.finds = nil, nil
	g.sendDoes = nil
	g.ticks(12)
	if g.kinds() != "recovery" || len(g.finds) != 0 || len(g.w.state.Closed) != 0 {
		t.Fatalf("after the close: sends %q, %d reads, %d closed", g.kinds(), len(g.finds), len(g.w.state.Closed))
	}
	if !strings.Contains(g.sends[0].Text, "may not have been delivered") {
		t.Fatalf("recovery text:\n%s", g.sends[0].Text)
	}
}

// The recovery line has one wording per state of the alert at the close.
func TestHubReachRecoveryWordingByAlertState(t *testing.T) {
	base := hubReachEpisode{ID: "testhost-1", Host: "testhost", Hub: "hub", FailingSince: hubReachAt(0, 10, 0), ClosedAt: hubReachAt(0, 10, 10), Class: hubReachNoRoute}
	for _, c := range []struct{ state, want, not string }{
		{hubReachDelivered, "", "alert"},
		{hubReachUnconfirmed, "The alert for this outage got no answer from Discord and may not have been delivered.", "No alert was delivered"},
		{hubReachDropped, "No alert was delivered for this outage.", "may not have been delivered"},
	} {
		e := base
		e.Alert.State = c.state
		text := hubReachMessageFor(e, "recovery", "marker").Text
		body := strings.TrimSuffix(text, "ref hub-reach recovery testhost-1")
		if (c.want != "" && !strings.Contains(text, c.want)) || strings.Contains(body, c.not) {
			t.Fatalf("alert %s: recovery text:\n%s", c.state, text)
		}
	}
	// What each state at the open episode becomes at the close.
	for from, to := range map[string]string{
		hubReachOwed: hubReachDropped, hubReachUnknown: hubReachUnconfirmed, hubReachAttempting: hubReachUnconfirmed,
		hubReachDelivered: hubReachDelivered, hubReachNotOwed: hubReachNotOwed,
	} {
		f := newHubReachFake(t, hubReachAt(0, 10, 0))
		f.settings.Owner = ""
		f.outage(hubReachNotTunnel)
		f.w.state.Open.Alert.State = from
		f.w.closeEpisode(f.now)
		if got := f.w.state.Closed[0].Alert.State; got != to {
			t.Fatalf("alert %s closed as %s, want %s", from, got, to)
		}
	}
}

// With no owner configured, the no-owner line is logged once per episode,
// with two episodes owing a Discord line at once, and the set of logged
// episodes holds only episodes still kept.
func TestHubReachNoOwnerLoggedOncePerEpisode(t *testing.T) {
	const line = "no owner is configured"
	f := newHubReachFake(t, hubReachAt(0, 10, 0))
	f.settings.Owner = ""
	f.outage(hubReachNotTunnel)
	f.up()
	f.ticks(1)
	f.ticks(1)
	f.outage(hubReachNotTunnel) // one closed, one open
	f.ticks(10)
	if f.w.state.Open == nil || len(f.w.state.Closed) != 1 || f.logCount(line) != 2 {
		t.Fatalf("one closed and one open: %d closed, %d no-owner lines:\n%s", len(f.w.state.Closed), f.logCount(line), f.log.String())
	}
	f.up()
	f.ticks(1)
	f.ticks(10) // two closed
	if len(f.w.state.Closed) != 2 || f.logCount(line) != 2 || len(f.w.noOwnerLogged) != 2 {
		t.Fatalf("two closed: %d closed, %d no-owner lines, %d logged", len(f.w.state.Closed), f.logCount(line), len(f.w.noOwnerLogged))
	}
	if len(f.sends) != 0 {
		t.Fatalf("sent with no owner: %q", f.kinds())
	}
	// Both recovery lines give up after 24 hours; their IDs leave the set.
	f.at(f.now.Add(hubReachRetryFor + time.Minute))
	if len(f.w.state.Closed) != 0 || len(f.w.noOwnerLogged) != 0 || f.logCount(line) != 2 {
		t.Fatalf("after expiry: %d closed, %d logged, %d no-owner lines", len(f.w.state.Closed), len(f.w.noOwnerLogged), f.logCount(line))
	}
}

// The Discord nonces carry a short host hash: at most 25 characters, the
// same for one host and start across a restart, and different for two hosts
// whose outages start in the same second.
func TestHubReachNonceCarriesTheHost(t *testing.T) {
	start := hubReachAt(0, 10, 0)
	long := strings.Repeat("a-very-long-host-name.", 10) + "ts.net"
	for _, kind := range []string{"alert", "recovery"} {
		var nonces []string
		for _, host := range []string{"testhost", "otherhost", long} {
			e := hubReachEpisode{ID: fmt.Sprintf("%s-%d", host, start.Unix()), Host: host, FailingSince: start, ClosedAt: start.Add(time.Hour)}
			n := hubReachMessageFor(e, kind, "marker").Nonce
			if len(n) > 25 || n != hubReachMessageFor(e, kind, "marker").Nonce || !strings.HasSuffix(n, fmt.Sprintf("-%d", start.Unix())) {
				t.Fatalf("%s nonce %q (%d characters) for %s", kind, n, len(n), host)
			}
			nonces = append(nonces, n)
		}
		if nonces[0] == nonces[1] || nonces[0] == nonces[2] || nonces[1] == nonces[2] {
			t.Fatalf("two hosts share a %s nonce: %q", kind, nonces)
		}
	}

	// A send repeated after a restart keeps its nonce.
	f := newHubReachFake(t, start)
	rejected := true
	f.sendDoes = func(hubReachMessage) hubReachOutcome {
		if rejected {
			return hubReachRejected
		}
		return hubReachSent
	}
	f.outage(hubReachNotTunnel)
	rejected = false
	f.reload()
	f.ticks(1)
	if f.kinds() != "alert,alert" || f.sends[0].Nonce != f.sends[1].Nonce || f.w.state.Open.Alert.State != hubReachDelivered {
		t.Fatalf("sends %q, nonces %q %q", f.kinds(), f.sends[0].Nonce, f.sends[1].Nonce)
	}
}
