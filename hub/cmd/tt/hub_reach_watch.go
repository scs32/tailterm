package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/discord"
)

// The hub reachability watch (wi_e493b596a86e0e2f, docs/hub-reach-watch.md).
//
// Every alarm of the project lives in the hub, so a host that cannot reach
// the hub could tell nobody. The relay therefore asks the hub one small
// question every 30 seconds on its own timer. After 3 unbroken minutes of
// failures it opens an episode: it records the cause class, writes a marker
// file and one log line, and tells the owner by a direct Discord message from
// the bot credential already on the host, with no hub call on that path.
//
// The watch only reads. It changes no network setting, recovers no tunnel,
// and sends no key to any session.

const (
	hubReachInterval   = 30 * time.Second
	hubReachThreshold  = 3 * time.Minute
	hubReachGap        = 2 * time.Minute
	hubReachRetryEvery = 5 * time.Minute
	hubReachRetryFor   = 24 * time.Hour
	// hubReachAlertBurst is how many alert turns run on consecutive ticks
	// before they are spaced by hubReachRetryEvery.
	hubReachAlertBurst = 10
	hubReachMaxClosed  = 20
	hubReachMaxTasks   = 5

	hubReachStateFile  = "hub-reach.json"
	hubReachMarkerFile = "HUB-UNREACHABLE"
)

// The four cause classes of a failed probe.
const (
	hubReachHTTPError    = "http_error"
	hubReachNoRoute      = "no_tunnel_route"
	hubReachDNSMissing   = "tailnet_dns_missing"
	hubReachTCPNoAnswer  = "tcp_timeout_or_refused"
	hubReachTunnel       = "tunnel"
	hubReachNotTunnel    = "not-tunnel"
	hubReachDNSPresent   = "present"
	hubReachDNSAbsent    = "missing"
	hubReachUnknownProbe = "unknown"
	// hubReachTailnetResolver is the tailnet's own DNS resolver address.
	hubReachTailnetResolver = "100.100.100.100"
)

// The states of one delivery.
const (
	hubReachNotOwed    = "not-owed"
	hubReachOwed       = "owed"
	hubReachAttempting = "attempting"
	hubReachUnknown    = "unknown"
	hubReachDelivered  = "delivered"
	hubReachDropped    = "dropped"
	hubReachExpired    = "expired"
)

// hubReachOutcome is what one Discord send came to.
type hubReachOutcome int

const (
	// hubReachSent: Discord answered 2xx, the message exists.
	hubReachSent hubReachOutcome = iota
	// hubReachRejected: the message was not created.
	hubReachRejected
	// hubReachNoAnswer: the request may or may not have created the message.
	hubReachNoAnswer
)

// errHubReachNoRecipient means a task has neither a live owner helper nor a
// live primary database handler to direct the recovery notice to.
var errHubReachNoRecipient = errors.New("no live owner helper or primary database handler")

// hubReachCall is one hub call: the raw status, or the transport error.
type hubReachCall struct {
	Status int
	Err    error
}

func (c hubReachCall) ok() bool { return c.Err == nil && c.Status >= 200 && c.Status <= 299 }

type hubReachSettings struct {
	Off       bool
	Owner     string // the owner's Discord user ID; "" when none is configured
	TokenFile string
}

type hubReachDelivery struct {
	State           string    `json:"state"`
	FirstSendableAt time.Time `json:"firstSendableAt"`
	LastAttemptAt   time.Time `json:"lastAttemptAt"`
	Attempts        int       `json:"attempts"`
}

func (d hubReachDelivery) pending() bool {
	return d.State == hubReachOwed || d.State == hubReachAttempting || d.State == hubReachUnknown
}

type hubReachBoardDelivery struct {
	Task string `json:"task"`
	hubReachDelivery
}

type hubReachEpisode struct {
	ID           string    `json:"id"`
	Host         string    `json:"host"`
	Hub          string    `json:"hub"` // the hub address's host, never a credential
	FailingSince time.Time `json:"failingSince"`
	OpenedAt     time.Time `json:"openedAt"`
	ClosedAt     time.Time `json:"closedAt"`
	Class        string    `json:"class"`
	Detail       string    `json:"detail"`
	Route        string    `json:"route"`
	DNS          string    `json:"dns"`
	LatestClass  string    `json:"latestClass"`
	LatestDetail string    `json:"latestDetail"`
	LatestRoute  string    `json:"latestRoute"`
	LatestDNS    string    `json:"latestDns"`
	OwnerVisible bool      `json:"ownerVisible"`

	Alert    hubReachDelivery        `json:"alert"`
	Recovery hubReachDelivery        `json:"recovery"`
	Board    []hubReachBoardDelivery `json:"board,omitempty"`
}

func (e hubReachEpisode) owes() bool {
	if e.Alert.pending() || e.Recovery.pending() {
		return true
	}
	for _, b := range e.Board {
		if b.pending() {
			return true
		}
	}
	return false
}

// undelivered names the deliveries of an episode that were never made.
func (e hubReachEpisode) undelivered() string {
	var out []string
	if e.Recovery.pending() {
		out = append(out, "the Discord recovery line")
	}
	for _, b := range e.Board {
		if b.pending() {
			out = append(out, "the Board notice for "+b.Task)
		}
	}
	if len(out) == 0 {
		return "nothing"
	}
	return strings.Join(out, ", ")
}

type hubReachState struct {
	FailingSince time.Time         `json:"failingSince"`
	LastProbeAt  time.Time         `json:"lastProbeAt"`
	Open         *hubReachEpisode  `json:"open,omitempty"`
	Closed       []hubReachEpisode `json:"closed,omitempty"`
}

// hubReachMessage is one Discord message. Ref is its last line and its
// durable identity; Nonce covers a retry inside Discord's own short window.
type hubReachMessage struct {
	Kind  string // "alert" or "recovery"
	Nonce string
	Ref   string
	Text  string
	// After bounds the read that looks for Ref: one minute before the
	// episode opened.
	After time.Time
}

type hubReachDeps struct {
	now      func() time.Time
	hubURL   func() string // "" when no hub is configured
	hub      func(ctx context.Context) hubReachCall
	route    func(ctx context.Context, host string) string
	dns      func(ctx context.Context) string
	send     func(ctx context.Context, s hubReachSettings, m hubReachMessage) hubReachOutcome
	find     func(ctx context.Context, s hubReachSettings, m hubReachMessage) (bool, error)
	tasks    func() []string
	board    func(ctx context.Context, e hubReachEpisode, task string) error
	settings func() hubReachSettings
	log      io.Writer
	dir      string
	host     string
}

type hubReachWatch struct {
	d     hubReachDeps
	state hubReachState

	marker        string // the marker text last written
	idleLogged    bool
	noOwnerLogged string // the episode the missing owner was logged for
}

var hubReachPacific = func() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return time.FixedZone("PST", -8*60*60)
	}
	return loc
}()

// hubReachNight reports whether now falls in 22:00 to 07:00 Pacific, when no
// Discord call is made. It is the stall watch's rule (stallWatchNight in
// internal/store), copied because that one is not exported.
func hubReachNight(now time.Time) bool {
	hour := now.In(hubReachPacific).Hour()
	return hour >= 22 || hour < 7
}

func hubReachStamp(t time.Time) string {
	return t.In(hubReachPacific).Format("2006-01-02 15:04:05 MST")
}

// newHubReachWatch reads the saved state. A delivery found in attempting was
// cut off by a relay stop, so whether Discord has it is unknown.
func newHubReachWatch(d hubReachDeps) *hubReachWatch {
	w := &hubReachWatch{d: d}
	if data, err := os.ReadFile(filepath.Join(d.dir, hubReachStateFile)); err == nil {
		if json.Unmarshal(data, &w.state) != nil {
			w.state = hubReachState{}
			w.logf("the saved state could not be read; starting with none")
		}
	}
	interrupted := func(e *hubReachEpisode) {
		for _, d := range []*hubReachDelivery{&e.Alert, &e.Recovery} {
			if d.State == hubReachAttempting {
				d.State = hubReachUnknown
			}
		}
	}
	if w.state.Open != nil {
		interrupted(w.state.Open)
	}
	for i := range w.state.Closed {
		interrupted(&w.state.Closed[i])
	}
	return w
}

func (w *hubReachWatch) logf(format string, args ...any) {
	fmt.Fprintf(w.d.log, "[tt relay] hub reach: "+format+"\n", args...)
}

// save writes the state file and keeps the marker file present exactly while
// an episode is open.
func (w *hubReachWatch) save() {
	saved := w.state
	if saved.FailingSince.IsZero() && saved.Open == nil {
		// The time of the last probe matters only to a streak of failures,
		// so a healthy host does not rewrite the file on every tick.
		saved.LastProbeAt = time.Time{}
	}
	if err := writePrivateJSON(filepath.Join(w.d.dir, hubReachStateFile), saved); err != nil {
		w.logf("save state: %v", err)
	}
	path := filepath.Join(w.d.dir, hubReachMarkerFile)
	if w.state.Open == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			w.logf("remove marker: %v", err)
		}
		w.marker = ""
		return
	}
	text := hubReachMarkerText(*w.state.Open)
	if text == w.marker {
		return
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		w.logf("write marker: %v", err)
		return
	}
	w.marker = text
}

func hubReachMarkerText(e hubReachEpisode) string {
	var b strings.Builder
	fmt.Fprintf(&b, "HUB UNREACHABLE from host %s\n", e.Host)
	fmt.Fprintf(&b, "hub address: %s\n", e.Hub)
	fmt.Fprintf(&b, "failing since: %s\n", e.FailingSince.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "episode opened: %s\n", e.OpenedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "cause class at opening: %s (%s), route=%s dns=%s\n", e.Class, e.Detail, e.Route, e.DNS)
	fmt.Fprintf(&b, "cause class at the latest failed probe: %s (%s), route=%s dns=%s\n", e.LatestClass, e.LatestDetail, e.LatestRoute, e.LatestDNS)
	fmt.Fprintf(&b, "owner alert on Discord: %s\n", e.Alert.State)
	b.WriteString("The relay changed no network setting and recovered nothing.\n")
	return b.String()
}

// hubReachClassify names the cause class of a failed probe; the first
// matching row of the table in docs/hub-reach-watch.md wins.
func hubReachClassify(call hubReachCall, route, dns string) (class, detail string) {
	switch {
	case call.Err == nil:
		return hubReachHTTPError, strconv.Itoa(call.Status)
	case route == hubReachNotTunnel:
		return hubReachNoRoute, hubReachNoAnswerKind(call.Err)
	case dns == hubReachDNSAbsent:
		return hubReachDNSMissing, hubReachNoAnswerKind(call.Err)
	}
	return hubReachTCPNoAnswer, hubReachNoAnswerKind(call.Err)
}

func hubReachNoAnswerKind(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	}
	return "other"
}

// step is one tick of the watch: one hub call, the episode it opens,
// continues or closes, and the deliveries that are due.
func (w *hubReachWatch) step(ctx context.Context) {
	settings := w.d.settings()
	if settings.Off {
		return
	}
	hubURL := w.d.hubURL()
	if hubURL == "" {
		if !w.idleLogged {
			w.idleLogged = true
			w.logf("no hub is configured in hub.json; the watch is idle")
		}
		return
	}
	now := w.d.now()
	if !w.state.LastProbeAt.IsZero() && now.Sub(w.state.LastProbeAt) > hubReachGap && w.state.Open == nil {
		// The host slept or the relay was stopped: a gap never opens an
		// episode by itself.
		w.state.FailingSince = time.Time{}
	}
	call := w.d.hub(ctx)
	if ctx.Err() != nil {
		// The relay is stopping; this call says nothing about the hub.
		return
	}
	w.state.LastProbeAt = now
	if call.ok() {
		w.state.FailingSince = time.Time{}
		if w.state.Open != nil {
			w.closeEpisode(now)
		}
	} else {
		w.failed(ctx, now, hubURL, call)
	}
	w.save()
	if e := w.state.Open; e != nil {
		w.discord(ctx, settings, e, &e.Alert, "alert")
	}
	for i := range w.state.Closed {
		e := &w.state.Closed[i]
		w.discord(ctx, settings, e, &e.Recovery, "recovery")
		w.boardPosts(ctx, e, call.ok())
	}
	kept := w.state.Closed[:0]
	for _, e := range w.state.Closed {
		if e.owes() {
			kept = append(kept, e)
		}
	}
	w.state.Closed = kept
	w.save()
}

func (w *hubReachWatch) failed(ctx context.Context, now time.Time, hubURL string, call hubReachCall) {
	if w.state.FailingSince.IsZero() {
		w.state.FailingSince = now
	}
	hubHost := hubURL
	if u, err := url.Parse(hubURL); err == nil && u.Hostname() != "" {
		hubHost = u.Hostname()
	}
	route, dns := w.d.route(ctx, hubHost), w.d.dns(ctx)
	class, detail := hubReachClassify(call, route, dns)
	e := w.state.Open
	if e == nil {
		if now.Sub(w.state.FailingSince) < hubReachThreshold {
			return
		}
		since := w.state.FailingSince
		e = &hubReachEpisode{
			ID: fmt.Sprintf("%s-%d", w.d.host, since.Unix()), Host: w.d.host, Hub: hubHost,
			FailingSince: since, OpenedAt: now,
			Class: class, Detail: detail, Route: route, DNS: dns,
			Alert: hubReachDelivery{State: hubReachNotOwed}, Recovery: hubReachDelivery{State: hubReachNotOwed},
		}
		w.state.Open = e
		fmt.Fprintf(w.d.log, "[tt relay] %s HUB UNREACHABLE since %s: %s (route=%s dns=%s)\n",
			now.UTC().Format(time.RFC3339), since.UTC().Format(time.RFC3339), class, route, dns)
	}
	e.LatestClass, e.LatestDetail, e.LatestRoute, e.LatestDNS = class, detail, route, dns
	// An episode is the owner's from its first failed probe that falls in
	// the day: at once when it opens by day, at 07:00 when it opens at night
	// and the hub is still down then.
	if !e.OwnerVisible && !hubReachNight(now) {
		e.OwnerVisible = true
		e.Alert.State = hubReachOwed
	}
}

// closeEpisode closes the open episode on the first successful probe.
func (w *hubReachWatch) closeEpisode(now time.Time) {
	e := *w.state.Open
	w.state.Open = nil
	e.ClosedAt = now
	fmt.Fprintf(w.d.log, "[tt relay] %s HUB REACHABLE again: unreachable from %s to %s (%s), %s\n",
		now.UTC().Format(time.RFC3339), e.FailingSince.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339),
		hubReachDuration(e), e.Class)
	// Only the recovery line is sent after recovery; it says when the outage
	// began, so it stands without the alert.
	if e.Alert.State != hubReachDelivered && e.Alert.State != hubReachNotOwed {
		e.Alert.State = hubReachDropped
	}
	if e.OwnerVisible {
		e.Recovery.State = hubReachOwed
	}
	for _, task := range w.d.tasks() {
		e.Board = append(e.Board, hubReachBoardDelivery{Task: task, hubReachDelivery: hubReachDelivery{State: hubReachOwed, FirstSendableAt: now}})
	}
	if !e.owes() {
		return
	}
	w.state.Closed = append(w.state.Closed, e)
	for len(w.state.Closed) > hubReachMaxClosed {
		oldest := w.state.Closed[0]
		w.state.Closed = w.state.Closed[1:]
		w.logf("more than %d closed episodes have deliveries owed; dropped the oldest, which began %s; not delivered: %s",
			hubReachMaxClosed, oldest.FailingSince.UTC().Format(time.RFC3339), oldest.undelivered())
	}
}

func hubReachDuration(e hubReachEpisode) string {
	return e.ClosedAt.Sub(e.FailingSince).Round(time.Second).String()
}

// discord takes one turn at one Discord delivery: a send, or for a send
// whose answer was lost, a read of the channel first.
func (w *hubReachWatch) discord(ctx context.Context, settings hubReachSettings, e *hubReachEpisode, d *hubReachDelivery, kind string) {
	if d.State != hubReachOwed && d.State != hubReachUnknown {
		return
	}
	now := w.d.now()
	if hubReachNight(now) {
		return
	}
	if d.FirstSendableAt.IsZero() {
		d.FirstSendableAt = now
	}
	every := d.Attempts > 0
	if kind == "alert" {
		every = d.Attempts >= hubReachAlertBurst
	} else if now.Sub(d.FirstSendableAt) >= hubReachRetryFor {
		d.State = hubReachExpired
		w.logf("the Discord %s line of the episode that began %s was not delivered in %s; given up", kind, e.FailingSince.UTC().Format(time.RFC3339), hubReachRetryFor)
		return
	}
	if every && now.Sub(d.LastAttemptAt) < hubReachRetryEvery {
		return
	}
	if settings.Owner == "" {
		if w.noOwnerLogged != e.ID {
			w.noOwnerLogged = e.ID
			w.logf("no owner is configured (relay.json hubReachDiscordOwner); nothing is sent to Discord")
		}
		return
	}
	d.Attempts++
	d.LastAttemptAt = now
	m := hubReachMessageFor(*e, kind, filepath.Join(w.d.dir, hubReachMarkerFile))
	if d.State == hubReachUnknown {
		// Never sent again directly: Discord may already have it.
		found, err := w.d.find(ctx, settings, m)
		if err != nil {
			w.logf("the Discord %s line may or may not have arrived, and the channel could not be read; nothing is sent until it can", kind)
			return
		}
		if found {
			d.State = hubReachDelivered
			w.logf("the Discord %s line is in the channel; not sent again", kind)
			return
		}
		d.State = hubReachOwed
		if hubReachNight(w.d.now()) {
			return
		}
	}
	d.State = hubReachAttempting
	w.save()
	switch w.d.send(ctx, settings, m) {
	case hubReachSent:
		d.State = hubReachDelivered
		w.logf("the Discord %s line was sent", kind)
	case hubReachRejected:
		d.State = hubReachOwed
	default:
		d.State = hubReachUnknown
		w.logf("the Discord %s line got no answer; the channel is read before any repeat", kind)
	}
	w.save()
}

// boardPosts makes the due Board recovery posts of one closed episode. The
// hub stores each request ID, so a repeat after a lost answer is safe and
// needs no read first.
func (w *hubReachWatch) boardPosts(ctx context.Context, e *hubReachEpisode, hubAnswers bool) {
	now := w.d.now()
	for i := range e.Board {
		d := &e.Board[i]
		if d.State != hubReachOwed {
			continue
		}
		if now.Sub(d.FirstSendableAt) >= hubReachRetryFor {
			d.State = hubReachExpired
			w.logf("the Board recovery notice for %s of the episode that began %s was not posted in %s; given up", d.Task, e.FailingSince.UTC().Format(time.RFC3339), hubReachRetryFor)
			continue
		}
		if !hubAnswers || (d.Attempts > 0 && now.Sub(d.LastAttemptAt) < hubReachRetryEvery) {
			continue
		}
		d.Attempts++
		d.LastAttemptAt = now
		if err := w.d.board(ctx, *e, d.Task); err != nil {
			w.logf("Board recovery notice for %s: %v", d.Task, err)
			continue
		}
		d.State = hubReachDelivered
	}
}

// hubReachMessageFor builds one of the two Discord messages of an episode.
// The alert and the recovery line never share a nonce or a reference.
func hubReachMessageFor(e hubReachEpisode, kind, marker string) hubReachMessage {
	start := e.FailingSince.Unix()
	m := hubReachMessage{Kind: kind, After: e.OpenedAt.Add(-time.Minute)}
	var b strings.Builder
	if kind == "alert" {
		m.Nonce = fmt.Sprintf("hra-%d", start)
		m.Ref = "ref hub-reach alert " + e.ID
		fmt.Fprintf(&b, "Host %s has not reached the Tailterm hub at %s since %s. Cause class: %s (%s); route=%s dns=%s.\n",
			e.Host, e.Hub, hubReachStamp(e.FailingSince), e.Class, e.Detail, e.Route, e.DNS)
		b.WriteString("The relay changed no network setting and recovered nothing.\n")
		b.WriteString("Before recovering, record why it stopped (the recovery prime directive).\n")
		fmt.Fprintf(&b, "Marker file on the host: %s\n", marker)
	} else {
		m.Nonce = fmt.Sprintf("hrr-%d", start)
		m.Ref = "ref hub-reach recovery " + e.ID
		fmt.Fprintf(&b, "Host %s reaches the Tailterm hub at %s again. The outage began at %s; reachable again at %s; duration %s. Cause class at opening: %s (%s)",
			e.Host, e.Hub, hubReachStamp(e.FailingSince), hubReachStamp(e.ClosedAt), hubReachDuration(e), e.Class, e.Detail)
		if e.LatestClass != e.Class {
			fmt.Fprintf(&b, "; at the last failed probe: %s (%s)", e.LatestClass, e.LatestDetail)
		}
		b.WriteString(".\n")
		if e.Alert.State != hubReachDelivered {
			b.WriteString("No alert was delivered for this outage.\n")
		}
	}
	b.WriteString(m.Ref)
	m.Text = b.String()
	return m
}

// ---- native dependencies ----

// hubReachProbe asks the hub who the caller is and reports only the status.
// The body is not parsed: any 2xx means the hub address answered. A redirect
// is reported as itself, never followed.
func hubReachProbe(ctx context.Context, base, token string) hubReachCall {
	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/whoami", nil)
	if err != nil {
		return hubReachCall{Err: errors.New("the hub address is not usable")}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return hubReachCall{Err: err}
	}
	// The status line arrived; a body that cannot be read changes nothing.
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	res.Body.Close()
	return hubReachCall{Status: res.StatusCode}
}

var hubReachTailnetPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// hubReachTailnetHost reports whether the hub is reached through the tunnel,
// so that a route outside it is a finding and not the normal case.
func hubReachTailnetHost(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		for _, p := range hubReachTailnetPrefixes {
			if p.Contains(addr.Unmap()) {
				return true
			}
		}
		return false
	}
	return strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".ts.net")
}

var (
	hubReachRouteInterface = regexp.MustCompile(`(?m)^\s*interface:\s*(\S+)\s*$`)
	hubReachNameserver     = regexp.MustCompile(`(?m)^\s*nameserver\[\d+\]\s*:\s*(\S+)\s*$`)
	hubReachHostName       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:-]*$`)
)

// hubReachParseRoute reads the output of route -n get HOST.
func hubReachParseRoute(out string) string {
	if m := hubReachRouteInterface.FindStringSubmatch(out); m != nil {
		if strings.HasPrefix(m[1], "utun") {
			return hubReachTunnel
		}
		return hubReachNotTunnel
	}
	if strings.Contains(out, "not in table") {
		return hubReachNotTunnel
	}
	return hubReachUnknownProbe
}

// hubReachParseDNS reads the output of scutil --dns.
func hubReachParseDNS(out string) string {
	servers := hubReachNameserver.FindAllStringSubmatch(out, -1)
	for _, m := range servers {
		if m[1] == hubReachTailnetResolver {
			return hubReachDNSPresent
		}
	}
	if len(servers) > 0 || strings.Contains(out, "DNS configuration") {
		return hubReachDNSAbsent
	}
	return hubReachUnknownProbe
}

// nativeHubReachRoute runs the read-only route lookup. It applies only to a
// hub reached through the tunnel, and only on macOS.
func nativeHubReachRoute(ctx context.Context, host string) string {
	if runtime.GOOS != "darwin" || !hubReachTailnetHost(host) || !hubReachHostName.MatchString(host) {
		return hubReachUnknownProbe
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	args := []string{"-n", "get", host}
	if addr, err := netip.ParseAddr(host); err == nil && addr.Is6() && !addr.Is4In6() {
		args = []string{"-n", "get", "-inet6", host}
	}
	// A missing route exits non-zero and still says so in its output.
	out, _ := exec.CommandContext(ctx, "/sbin/route", args...).CombinedOutput()
	return hubReachParseRoute(string(out))
}

// nativeHubReachDNS runs the read-only resolver listing, only on macOS.
func nativeHubReachDNS(ctx context.Context) string {
	if runtime.GOOS != "darwin" {
		return hubReachUnknownProbe
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--dns").Output()
	if err != nil {
		return hubReachUnknownProbe
	}
	return hubReachParseDNS(string(out))
}

var hubReachOwnerID = regexp.MustCompile(`^[0-9]{17,20}$`)

// hubReachSettingsFrom reads the watch's three keys of relay.json, each by
// its exact name. home is the user's home directory.
func hubReachSettingsFrom(config map[string]json.RawMessage, home string) hubReachSettings {
	text := func(key string) string {
		var v string
		if json.Unmarshal(config[key], &v) != nil {
			return ""
		}
		return strings.TrimSpace(v)
	}
	s := hubReachSettings{Off: text("hubReachWatch") == "off", TokenFile: text("hubReachDiscordTokenFile")}
	if owner := text("hubReachDiscordOwner"); hubReachOwnerID.MatchString(owner) {
		s.Owner = owner
	}
	switch {
	case s.TokenFile == "":
		s.TokenFile = filepath.Join(home, ".config", "discord", "token")
	case strings.HasPrefix(s.TokenFile, "~/"):
		s.TokenFile = filepath.Join(home, s.TokenFile[2:])
	}
	return s
}

func nativeHubReachSettings() hubReachSettings {
	config, _ := readRelayConfig()
	home, _ := os.UserHomeDir()
	return hubReachSettingsFrom(config, home)
}

// hubReachDiscord posts to the owner's DM with the bot credential, over REST
// only: no Gateway session is opened, so the bridge's session is untouched.
// The token is read from its file for each call, kept in no state and passed
// to no formatter.
type hubReachDiscord struct {
	base   string // discord.DefaultAPI unless a test points it elsewhere
	log    io.Writer
	warned bool
}

func (s *hubReachDiscord) logf(format string, args ...any) {
	fmt.Fprintf(s.log, "[tt relay] hub reach: "+format+"\n", args...)
}

func (s *hubReachDiscord) client(settings hubReachSettings) (*discord.Client, error) {
	info, err := os.Stat(settings.TokenFile)
	if err != nil {
		return nil, errors.New("the bot token file cannot be read")
	}
	if info.Mode().Perm()&0o077 != 0 && !s.warned {
		s.warned = true
		s.logf("the bot token file %s has mode %04o; it should be readable by its owner only", settings.TokenFile, info.Mode().Perm())
	}
	data, err := os.ReadFile(settings.TokenFile)
	if err != nil || len(data) > 4<<10 {
		return nil, errors.New("the bot token file cannot be read")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, errors.New("the bot token file is empty")
	}
	return &discord.Client{Token: token, Base: s.base, HTTP: &http.Client{Timeout: 10 * time.Second}, MaxWait: 10 * time.Second}, nil
}

// hubReachDiscordReason describes a failed call without the error's own text unless Discord
// wrote it: a transport error may carry more than is safe to log.
func hubReachDiscordReason(err error) string {
	var httpErr *discord.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Error()
	}
	return "no answer (" + hubReachNoAnswerKind(err) + ")"
}

func (s *hubReachDiscord) send(ctx context.Context, settings hubReachSettings, m hubReachMessage) hubReachOutcome {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := s.client(settings)
	if err != nil {
		s.logf("Discord %s not sent: %v", m.Kind, err)
		return hubReachRejected
	}
	channel, err := c.CreateDM(ctx, settings.Owner)
	if err != nil || channel.ID == "" {
		// No message was asked for yet, so none can exist.
		if err == nil {
			err = errors.New("no channel")
		}
		s.logf("Discord %s not sent, the DM channel could not be opened: %s", m.Kind, hubReachDiscordReason(err))
		return hubReachRejected
	}
	_, err = c.CreateMessage(ctx, channel.ID, discord.MessageSend{
		Content:         m.Text,
		AllowedMentions: discord.AllowedMentions{Parse: []string{}, Users: []string{settings.Owner}},
		Nonce:           m.Nonce,
		EnforceNonce:    true,
	})
	var httpErr *discord.HTTPError
	switch {
	case err == nil:
		return hubReachSent
	case errors.As(err, &httpErr):
		s.logf("Discord refused the %s: %s", m.Kind, httpErr.Error())
		return hubReachRejected
	}
	return hubReachNoAnswer
}

// discordEpoch is the first millisecond of Discord's snowflake clock.
const discordEpoch = 1420070400000

func hubReachSnowflake(t time.Time) string {
	ms := t.UnixMilli() - discordEpoch
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatInt(ms<<22, 10)
}

// find reads the DM channel for a message of the bot's whose last line is
// the delivery's reference.
func (s *hubReachDiscord) find(ctx context.Context, settings hubReachSettings, m hubReachMessage) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := s.client(settings)
	if err != nil {
		return false, err
	}
	fail := func(err error) (bool, error) { return false, errors.New(hubReachDiscordReason(err)) }
	me, err := c.CurrentUser(ctx)
	if err != nil {
		return fail(err)
	}
	channel, err := c.CreateDM(ctx, settings.Owner)
	if err != nil {
		return fail(err)
	}
	if me.ID == "" || channel.ID == "" {
		return false, errors.New("no bot user or channel")
	}
	messages, err := c.ChannelMessages(ctx, channel.ID, hubReachSnowflake(m.After), 100)
	if err != nil {
		return fail(err)
	}
	for _, got := range messages {
		lines := strings.Split(strings.TrimRight(got.Content, "\n"), "\n")
		if got.Author.ID == me.ID && strings.TrimSpace(lines[len(lines)-1]) == m.Ref {
			return true, nil
		}
	}
	return false, nil
}

func hubReachBoardRequestID(e hubReachEpisode, task string) string {
	return fmt.Sprintf("hub-reach-%s-%d-%s", e.Host, e.FailingSince.Unix(), task)
}

const hubReachBoardSubject = "This host could not reach the hub for a while and reaches it again"

// hubReachBoardPost posts the recovery notice of one episode on one task,
// always directed: to the live owner helper, else to the live primary
// database handler. A post with no recipient would wake every agent of the
// task, so with neither live it is not posted. A conflict means this request
// ID is already stored, so it counts as posted.
func hubReachBoardPost(ctx context.Context, c *api.Client, e hubReachEpisode, task string) error {
	agents, err := c.ListAgents(ctx, task)
	if err != nil {
		return err
	}
	to := claudeStallRecipient(agents, "")
	if to.ID == "" {
		tasks, err := c.ListTasks(ctx)
		if err != nil {
			return err
		}
		primary := ""
		for _, t := range tasks {
			if t.ID == task {
				primary = t.PrimaryHandlerID
			}
		}
		for _, a := range agents {
			if primary != "" && a.ID == primary && a.Role == api.AgentRoleDatabaseHandler &&
				a.Status != api.AgentClosed && a.Status != api.AgentExited && a.Status != api.AgentRetired {
				to = a
			}
		}
	}
	if to.ID == "" {
		return errHubReachNoRecipient
	}
	seconds := int64(e.ClosedAt.Sub(e.FailingSince).Round(time.Second) / time.Second)
	text := fmt.Sprintf("Host %s could not reach the hub at %s from %s to %s (%s). Cause class at opening: %s (%s); route=%s dns=%s. The relay changed no network setting and recovered nothing. Before more recovery, record why it stopped (the recovery prime directive).",
		e.Host, e.Hub, hubReachStamp(e.FailingSince), hubReachStamp(e.ClosedAt), hubReachDuration(e), e.Class, e.Detail, e.Route, e.DNS)
	env := api.Envelope{
		Kind: api.EnvelopeKindNotice, To: to.Name, Subject: hubReachBoardSubject,
		Refs: map[string]string{"hubReach": "recovered", "host": e.Host, "cause": e.Class, "durationSeconds": strconv.FormatInt(seconds, 10), "episode": e.ID},
		Body: api.EnvelopeBody{Text: text},
	}
	_, err = c.PostMessage(ctx, task, api.PostMessageRequest{Envelope: &env, Text: api.RenderText(env), To: to.ID, RequestID: hubReachBoardRequestID(e, task)})
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict {
		return nil
	}
	return err
}

// nativeHubReachBoard posts as the relay, like the stall notice: the hub
// token of hub.json, the relay-author mark and the relay's request budget.
func nativeHubReachBoard(ctx context.Context, e hubReachEpisode, task string) error {
	var cfg env
	cfg.loadConfig()
	c, err := cfg.client(3 * time.Second)
	if err != nil {
		return err
	}
	c.HTTP.Transport = relayAuthorTransport{base: c.HTTP.Transport}
	attachRelayBudget(c, activeRelayBudget)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return hubReachBoardPost(ctx, c, e, task)
}

// hubReachTasks lists the tasks of the valid bindings on this host for one
// hub, at most hubReachMaxTasks.
func hubReachTasks(dir, hubURL string) []string {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.binding.json"))
	seen := map[string]bool{}
	for _, path := range paths {
		var b runtimeBinding
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &b) == nil && validBinding(b) && b.Hub == hubURL {
			seen[b.Task] = true
		}
	}
	tasks := make([]string, 0, len(seen))
	for task := range seen {
		tasks = append(tasks, task)
	}
	sort.Strings(tasks)
	if len(tasks) > hubReachMaxTasks {
		tasks = tasks[:hubReachMaxTasks]
	}
	return tasks
}

func nativeHubReachDeps(dir string) hubReachDeps {
	config := func() env {
		var cfg env
		cfg.loadConfig()
		return cfg
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "host"
	}
	sender := &hubReachDiscord{log: os.Stderr}
	return hubReachDeps{
		now:    time.Now,
		hubURL: func() string { return config().hub },
		hub: func(ctx context.Context) hubReachCall {
			cfg := config()
			return hubReachProbe(ctx, cfg.hub, cfg.token)
		},
		route:    nativeHubReachRoute,
		dns:      nativeHubReachDNS,
		send:     sender.send,
		find:     sender.find,
		tasks:    func() []string { return hubReachTasks(dir, config().hub) },
		board:    nativeHubReachBoard,
		settings: nativeHubReachSettings,
		log:      os.Stderr,
		dir:      dir,
		host:     host,
	}
}

// startHubReachWatch runs the watch on its own timer until stop is called.
// The relay loop slows down exactly when the hub is unreachable, so the
// watch does not run on it.
func startHubReachWatch(dir string) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		var w *hubReachWatch
		tick := func() {
			// A fault in the watch must not take the relay down with it.
			defer func() {
				if fault := recover(); fault != nil {
					fmt.Fprintf(os.Stderr, "[tt relay] hub reach: %v\n", fault)
				}
			}()
			if w == nil {
				w = newHubReachWatch(nativeHubReachDeps(dir))
			}
			w.step(ctx)
		}
		ticker := time.NewTicker(hubReachInterval)
		defer ticker.Stop()
		for {
			tick()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// hubReachStatusLines is the watch's part of tt relay --status, read from
// the state file.
func hubReachStatusLines(dir string) []string {
	var state hubReachState
	if data, err := os.ReadFile(filepath.Join(dir, hubReachStateFile)); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	lines := []string{"hub-reach: ok"}
	if e := state.Open; e != nil {
		visible := "no"
		if e.OwnerVisible {
			visible = "yes"
		}
		lines[0] = fmt.Sprintf("hub-reach: UNREACHABLE since %s cause=%s visible=%s alert=%s", e.FailingSince.UTC().Format(time.RFC3339), e.Class, visible, e.Alert.State)
	}
	if n := len(state.Closed); n != 0 {
		lines = append(lines, fmt.Sprintf("hub-reach: %d closed episodes with deliveries owed", n))
	}
	return lines
}
