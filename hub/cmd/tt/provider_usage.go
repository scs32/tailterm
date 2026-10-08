package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// The Claude provider reading for the token budget. Claude Code hands its
// status line command the account's rate limits; a capture-only status line
// (installed by tt host setup) saves them to a host file and prints nothing.
// The relay reads that file each pass and reports a change to the hub, which
// uses it to decide queue admission for this host. A file that is missing,
// unreadable or malformed is reported too, as an invalidation, so the hub
// never keeps a figure it can no longer stand behind. Unknown is never
// treated as plenty.

// claudeUsageCaptureName is the capture status line command's file name.
const claudeUsageCaptureName = "tt-claude-usage-capture"

// claudeUsageCaptureScript is the capture status line command. It reads the
// status line JSON on stdin, saves the rate limit fields atomically and
// prints nothing, so no status line shows. It never fails Claude Code.
const claudeUsageCaptureScript = `#!/bin/sh
# Tailterm capture-only Claude Code status line (installed by tt host setup).
# Reads the status line JSON on stdin and saves the account rate limit fields
# for the token budget. Prints nothing, so no status line shows.
# Remove with: tt host setup --remove-claude-usage
D="$HOME/.local/state/tailterm"; F="$D/claude-usage.json"
mkdir -p "$D" 2>/dev/null || exit 0
PY=$(command -v python3 2>/dev/null) || exit 0
T="$F.$$.tmp"
"$PY" -c '
import json,sys,time
try:
    j=json.load(sys.stdin)
except Exception:
    sys.exit(0)
if not isinstance(j,dict):
    sys.exit(0)
json.dump({"capturedAt":int(time.time()),"rate_limits":j.get("rate_limits"),"version":j.get("version")},sys.stdout)
' > "$T" 2>/dev/null && [ -s "$T" ] && mv -f "$T" "$F" 2>/dev/null
rm -f "$T" 2>/dev/null
exit 0
`

// claudeUsageHome is the home directory the capture lives under.
func claudeUsageHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// claudeUsageCapturePath is the file the capture status line writes.
func claudeUsageCapturePath() string {
	return filepath.Join(claudeUsageHome(), ".local", "state", "tailterm", "claude-usage.json")
}

// claudeUsageScriptPath is where tt host setup installs the capture command.
func claudeUsageScriptPath() string {
	return filepath.Join(claudeUsageHome(), ".local", "bin", claudeUsageCaptureName)
}

// claudeUsageReading is what the capture file says now.
type claudeUsageReading struct {
	State      string
	CapturedAt time.Time
	Version    string
	Windows    []api.ProviderUsageWindow
}

// readClaudeUsageCapture reads the capture file. It returns an ok reading, or
// the named state that makes the file unusable: missing, unreadable, or
// malformed (not JSON, no capture time, no rate_limits, or a window without a
// numeric used_percentage from 0 to 100 and a resets_at). At least one of the
// two windows must be present; a window that is present must be whole.
func readClaudeUsageCapture(path string) claudeUsageReading {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return claudeUsageReading{State: api.ProviderUsageMissing}
	}
	if err != nil {
		return claudeUsageReading{State: api.ProviderUsageUnreadable}
	}
	malformed := claudeUsageReading{State: api.ProviderUsageMalformed}
	var raw struct {
		CapturedAt *float64 `json:"capturedAt"`
		Version    string   `json:"version"`
		RateLimits map[string]*struct {
			UsedPercentage *float64 `json:"used_percentage"`
			ResetsAt       *float64 `json:"resets_at"`
		} `json:"rate_limits"`
	}
	if json.Unmarshal(data, &raw) != nil || raw.CapturedAt == nil || !epochSeconds(*raw.CapturedAt) || raw.RateLimits == nil {
		return malformed
	}
	out := claudeUsageReading{State: api.ProviderUsageOK, CapturedAt: time.Unix(int64(*raw.CapturedAt), 0).UTC(), Version: raw.Version}
	if len(out.Version) > 80 {
		out.Version = out.Version[:80]
	}
	for _, window := range []string{api.UsageWindowFiveHour, api.UsageWindowSevenDay} {
		w, present := raw.RateLimits[window]
		if !present {
			continue
		}
		if w == nil || w.UsedPercentage == nil || w.ResetsAt == nil || math.IsNaN(*w.UsedPercentage) || *w.UsedPercentage < 0 || *w.UsedPercentage > 100 || !epochSeconds(*w.ResetsAt) {
			return malformed
		}
		out.Windows = append(out.Windows, api.ProviderUsageWindow{Window: window, UsedPercent: *w.UsedPercentage, ResetsAt: time.Unix(int64(*w.ResetsAt), 0).UTC().Format(time.RFC3339)})
	}
	if len(out.Windows) == 0 {
		return malformed
	}
	return out
}

// epochSeconds accepts a plausible instant in whole or fractional seconds.
func epochSeconds(v float64) bool {
	return !math.IsNaN(v) && v > 0 && v < 1e11
}

// report is the reading as the hub takes it.
func (r claudeUsageReading) report(host string) api.ProviderUsageReport {
	out := api.ProviderUsageReport{Host: host, Runtime: "claude", State: r.State}
	if r.State == api.ProviderUsageOK {
		out.CapturedAt, out.Version, out.Windows = r.CapturedAt.Format(time.RFC3339), r.Version, r.Windows
	}
	return out
}

// key identifies what a reading says: its state and each window's percentage
// and reset. The capture time is not part of it: Claude Code rewrites the
// file every few seconds with the same figures, and that is not news.
func (r claudeUsageReading) key() string {
	key := r.State
	for _, w := range r.Windows {
		key += "|" + w.Window + "=" + strconv.FormatFloat(w.UsedPercent, 'f', -1, 64) + "@" + w.ResetsAt
	}
	return key
}

// providerUsageKeepFresh is how often an unchanged reading is reported again
// while its file is still being rewritten, so the hub's copy stays inside its
// staleness bound. providerUsageRetry spaces attempts after a failed report.
const (
	providerUsageKeepFresh = 5 * time.Minute
	providerUsageRetry     = time.Minute
)

// providerUsageState is the relay's record of what each hub was last told.
// The reports are kept in the relay state directory, so a restarted relay
// does not send them again; attempts and older hubs are remembered only for
// the life of the process.
type providerUsageState struct {
	loaded bool
	hubs   map[string]*providerUsageHub
}

type providerUsageHub struct {
	Reported string    `json:"reported"` // key of the last successful report
	Captured time.Time `json:"captured"` // capture time of that report
	Sent     time.Time `json:"sent"`     // when it was sent
	Good     bool      `json:"good"`     // an ok reading was reported at some time
	attempt  time.Time // when a report was last tried, successful or not
	missing  bool      // the hub has no provider usage route
}

// providerUsageStatePath is the file the last reports are kept in.
func providerUsageStatePath() string {
	return filepath.Join(relayDir(), "provider-usage.json")
}

func (s *providerUsageState) load() {
	if s.loaded {
		return
	}
	s.loaded, s.hubs = true, map[string]*providerUsageHub{}
	if data, err := os.ReadFile(providerUsageStatePath()); err == nil {
		_ = json.Unmarshal(data, &s.hubs)
	}
	for hub, record := range s.hubs {
		if record == nil {
			delete(s.hubs, hub)
		}
	}
}

func (s *providerUsageState) save() error {
	return writePrivateJSON(providerUsageStatePath(), s.hubs)
}

// providerUsageTick reports the host's Claude usage capture to one hub only:
//
//   - when its state, a percentage or a reset differs from the last
//     successful report, at once. That includes the invalidation when a
//     previously good file becomes missing, unreadable or malformed, so the
//     hub drops the old figures;
//   - or as a keep-fresh report, at most once every five minutes, when the
//     figures are unchanged but the file's capture time has advanced, so the
//     hub can tell a live reading from a stale one.
//
// It never reports because the capture time or the file time moved alone,
// and it reports nothing for a missing file when no good reading was ever
// reported. A failed report is tried again after a minute, and a hub without
// the route once an hour.
func providerUsageTick(ctx context.Context, c *api.Client, host string, now time.Time, state *providerUsageState) error {
	state.load()
	hub := state.hubs[c.Base]
	if hub == nil {
		hub = &providerUsageHub{}
		state.hubs[c.Base] = hub
	}
	reading := readClaudeUsageCapture(claudeUsageCapturePath())
	switch {
	case reading.State == api.ProviderUsageMissing && !hub.Good:
		return nil // nothing was ever there to invalidate
	case reading.key() != hub.Reported:
	case reading.State == api.ProviderUsageOK && reading.CapturedAt.After(hub.Captured) && now.Sub(hub.Sent) >= providerUsageKeepFresh:
	default:
		return nil
	}
	interval := providerUsageRetry
	if hub.missing {
		interval = time.Hour
	}
	if !hub.attempt.IsZero() && now.Sub(hub.attempt) < interval {
		return nil
	}
	_, err := c.ReportProviderUsage(ctx, reading.report(host))
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && (httpErr.Status == http.StatusNotFound || httpErr.Status == http.StatusMethodNotAllowed) {
		hub.missing, hub.attempt = true, now // an older hub
		return nil
	}
	if err != nil {
		hub.attempt = now
		return fmt.Errorf("report provider usage (%s): %w", reading.State, err)
	}
	hub.missing, hub.attempt = false, time.Time{}
	hub.Reported, hub.Captured, hub.Sent = reading.key(), reading.CapturedAt, now
	hub.Good = hub.Good || reading.State == api.ProviderUsageOK
	return state.save()
}

// relayProviderUsageState is the running relay's report memory, and
// relayProviderHost this host's name as the queue runner names it.
var (
	relayProviderUsageState providerUsageState
	relayProviderHost       string
	relayProviderHostAt     time.Time
)

// relayProviderUsage runs the provider usage report for each hub this host's
// bindings name, and the configured hub. It never delays a wake: it runs
// after delivery, with a short timeout, and only logs a failure.
func relayProviderUsage(hubs map[string]bool, now time.Time) {
	configured := env{hub: os.Getenv(spawn.EnvHub), token: os.Getenv("TAILTERM_TOKEN")}
	configured.loadConfig()
	if configured.hub != "" {
		hubs[configured.hub] = true
	}
	if len(hubs) == 0 {
		return
	}
	// Naming the host asks tailscale; the name is kept for ten minutes.
	if relayProviderHost == "" || now.Sub(relayProviderHostAt) >= 10*time.Minute {
		relayProviderHost, relayProviderHostAt = spawn.Host(), now
	}
	host := relayProviderHost
	for hub := range hubs {
		e := env{hub: hub}
		e.loadConfig()
		c, err := e.client(5 * time.Second)
		if err != nil {
			continue
		}
		attachRelayBudget(c, activeRelayBudget)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = providerUsageTick(ctx, c, host, now, &relayProviderUsageState)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "[tt relay] provider usage:", err)
		}
	}
}
