package main

// Saved bridge settings (wi_dc04ac6a9ccf7402, docs/discord-helper-chat.md
// "## Configure"). The owner saves the bridge's non-secret settings in TailOS
// Settings; the bridge reads them at start through its one read-only route,
// reports what it runs on, and exits to be restarted when a new revision is
// saved. Both tokens stay token files on this host and never reach the hub.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/bridge"
	"github.com/scs32/tailterm/hub/internal/discord"
)

// Bridge state kv keys (bridge.sqlite; no schema change).
const (
	kvGuild    = "guild"             // the bound guild: the one the channel mappings belong to
	kvAttempt  = "settings:attempt"  // the hub revision the last start tried
	kvStarted  = "settings:started"  // the hub revision that reached Gateway READY on that start
	kvRefused  = "settings:refused"  // "N:code", the hub revision never to try again
	kvLastGood = "settings:lastGood" // the last hub settings that reached READY
)

// SettingsPoll is how often the bridge reads its saved settings.
const SettingsPoll = 30 * time.Second

// settingsChoice is what one start runs on.
type settingsChoice struct {
	values   api.BridgeSettings
	report   api.BridgeReport
	revision int64 // the hub revision when report.Source is hub
	parked   bool  // no candidate matches the bound guild: do not connect
}

type lastGood struct {
	Revision int64              `json:"revision"`
	Settings api.BridgeSettings `json:"settings"`
}

// envSettings reads the deployment values exactly as the bridge did before
// saved settings existed.
func envSettings(getenv func(string, string) string) api.BridgeSettings {
	s := api.BridgeSettings{
		GuildID:         getenv("DISCORD_GUILD_ID", ""),
		HelperTask:      getenv("DISCORD_HELPER_TASK", ""),
		HelperChannelID: getenv("DISCORD_HELPER_CHANNEL", ""),
		TailOSURL:       getenv("TAILOS_URL", ""),
	}
	for _, id := range strings.Split(getenv("DISCORD_OWNER_IDS", ""), ",") {
		if id = strings.TrimSpace(id); id != "" {
			s.OwnerIDs = append(s.OwnerIDs, id)
		}
	}
	return s
}

// settingsRunner runs the bridge on its saved settings, or on a fallback.
type settingsRunner struct {
	Hub   *api.Client
	State *bridge.State
	Env   api.BridgeSettings // deployment values
	// Base is the bridge configuration without the five settings, the
	// Gateway or Started; run fills them in.
	Base bridge.Config
	// Gateway builds the event connection for a configuration; nil in tests.
	Gateway func(helper bool) *discord.Gateway
	Poll    time.Duration
	Log     func(string, ...any)
	// OnBridge, when set, sees each bridge before it runs (tests).
	OnBridge func(*bridge.Bridge)
}

func (r *settingsRunner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

func parseRefused(v string) (int64, string) {
	rev, code, ok := strings.Cut(v, ":")
	if !ok {
		return 0, ""
	}
	n, err := strconv.ParseInt(rev, 10, 64)
	if err != nil {
		return 0, ""
	}
	return n, code
}

func (r *settingsRunner) refuse(ctx context.Context, rev int64, code string) error {
	r.logf("discord bridge: refusing saved settings revision %d (%s); it is not tried again until a new save", rev, code)
	return r.State.Set(ctx, kvRefused, strconv.FormatInt(rev, 10)+":"+code)
}

// readSaved reads the saved record. No record, or a hub too old to have the
// route (403/404), is no record; another error is logged and treated as no
// record too, and the watcher retries.
func (r *settingsRunner) readSaved(ctx context.Context) api.BridgeSettingsRecord {
	view, err := r.Hub.BridgeSettings(ctx, nil)
	if err != nil {
		var h *api.HTTPError
		if errors.As(err, &h) && (h.Status == http.StatusForbidden || h.Status == http.StatusNotFound) {
			r.logf("discord bridge: the hub has no bridge settings route (%d); using deployment values", h.Status)
		} else {
			r.logf("discord bridge: read saved settings: %v; using a fallback until the next read", err)
		}
		return api.BridgeSettingsRecord{}
	}
	return view.BridgeSettingsRecord
}

// resolve decides what this start runs on (plan D4b with lead decisions L1
// and L2): a usable saved revision; else the last good saved settings or the
// deployment values, whichever first matches the bound guild; else parked.
func (r *settingsRunner) resolve(ctx context.Context) (settingsChoice, error) {
	rec := r.readSaved(ctx)
	mappings, err := r.State.Mappings(ctx)
	if err != nil {
		return settingsChoice{}, err
	}
	hasMappings := len(mappings) > 0
	bound, err := r.State.Get(ctx, kvGuild)
	if err != nil {
		return settingsChoice{}, err
	}
	if bound == "" && hasMappings {
		// A state from before saved settings could only have used the
		// deployment guild.
		bound = r.Env.GuildID
		if err := r.State.Set(ctx, kvGuild, bound); err != nil {
			return settingsChoice{}, err
		}
	}
	var refused int64
	var reason string
	if rec.Settings != nil && rec.Revision > 0 {
		n := rec.Revision
		refusedRev, refusedCode := r.refusedKV(ctx)
		attempt, _ := r.State.Get(ctx, kvAttempt)
		started, _ := r.State.Get(ctx, kvStarted)
		rev := strconv.FormatInt(n, 10)
		switch {
		case refusedRev == n:
			refused, reason = n, refusedCode
		case attempt == rev && started != rev:
			// The previous start on n never reached READY and did not
			// record a failure: it was killed or stopped during start.
			refused, reason = n, api.BridgeRefusedStoppedDuringStart
		default:
			values, err := api.ValidateBridgeSettings(*rec.Settings)
			switch {
			case err != nil:
				refused, reason = n, api.BridgeRefusedInvalid
			case hasMappings && values.GuildID != bound:
				refused, reason = n, api.BridgeRefusedGuildChange
			default:
				return settingsChoice{values: values, revision: n,
					report: api.BridgeReport{Source: api.BridgeSourceHub, Applied: n, Effective: values}}, nil
			}
		}
		if refusedRev != n {
			if err := r.refuse(ctx, n, reason); err != nil {
				return settingsChoice{}, err
			}
		}
	}
	type candidate struct {
		source  string
		applied int64
		values  api.BridgeSettings
	}
	var candidates []candidate
	if raw, _ := r.State.Get(ctx, kvLastGood); raw != "" {
		var lg lastGood
		if json.Unmarshal([]byte(raw), &lg) == nil {
			if values, err := api.ValidateBridgeSettings(lg.Settings); err == nil {
				candidates = append(candidates, candidate{api.BridgeSourceLastGood, lg.Revision, values})
			}
		}
	}
	candidates = append(candidates, candidate{api.BridgeSourceDeployment, 0, r.Env})
	for _, c := range candidates {
		if !hasMappings || c.values.GuildID == bound {
			return settingsChoice{values: c.values, report: api.BridgeReport{Source: c.source, Applied: c.applied,
				Effective: c.values, Refused: refused, Reason: reason}}, nil
		}
	}
	r.logf("discord bridge: not connecting: the bridge has project channels in guild %s and no usable settings for it; save that guild again in Settings or re-set up the bridge (docs/discord-helper-chat.md)", bound)
	return settingsChoice{parked: true, report: api.BridgeReport{Source: api.BridgeSourceNone, Refused: refused, Reason: reason}}, nil
}

func (r *settingsRunner) refusedKV(ctx context.Context) (int64, string) {
	v, _ := r.State.Get(ctx, kvRefused)
	return parseRefused(v)
}

// run starts the bridge on its settings and returns when ctx ends (false,
// nil), when a newly saved revision needs a restart (true, nil), or when the
// bridge fails (false, err).
func (r *settingsRunner) run(ctx context.Context) (bool, error) {
	var (
		choice settingsChoice
		b      *bridge.Bridge
		cfg    bridge.Config
		ok     atomic.Bool // the first Gateway READY on this start
	)
	for attempt := 0; ; attempt++ {
		var err error
		if choice, err = r.resolve(ctx); err != nil {
			return false, err
		}
		if choice.parked {
			return r.watch(ctx, choice), nil
		}
		cfg = r.Base
		cfg.GuildID, cfg.Owners = choice.values.GuildID, choice.values.OwnerIDs
		cfg.HelperTask, cfg.HelperChannel = choice.values.HelperTask, choice.values.HelperChannelID
		cfg.TailOSURL = choice.values.TailOSURL
		if r.Gateway != nil {
			cfg.Gateway = r.Gateway(cfg.HelperTask != "")
		}
		choice := choice
		cfg.Started = func() {
			ok.Store(true)
			if choice.report.Source != api.BridgeSourceHub {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			raw, _ := json.Marshal(lastGood{Revision: choice.revision, Settings: choice.values})
			if err := r.State.Set(ctx, kvStarted, strconv.FormatInt(choice.revision, 10)); err != nil {
				r.logf("discord bridge: record settings revision %d started: %v", choice.revision, err)
			}
			if err := r.State.Set(ctx, kvLastGood, string(raw)); err != nil {
				r.logf("discord bridge: record last good settings: %v", err)
			}
		}
		b, err = bridge.New(cfg)
		if err == nil {
			break
		}
		if choice.report.Source != api.BridgeSourceHub || attempt > 0 {
			return false, err
		}
		r.logf("discord bridge: saved settings revision %d: %v", choice.revision, err)
		if err := r.refuse(ctx, choice.revision, api.BridgeRefusedInvalid); err != nil {
			return false, err
		}
	}
	hub := choice.report.Source == api.BridgeSourceHub
	if hub {
		// L1: a fresh attempt identity, so only READY on this start marks it.
		if err := r.State.Set(ctx, kvStarted, ""); err != nil {
			return false, err
		}
		if err := r.State.Set(ctx, kvAttempt, strconv.FormatInt(choice.revision, 10)); err != nil {
			return false, err
		}
	}
	// L2: bind the guild before any loop can create a channel mapping.
	if err := r.State.Set(ctx, kvGuild, cfg.GuildID); err != nil {
		return false, err
	}
	r.logf("discord bridge starting for guild %s with %d owner(s) on %s", cfg.GuildID, len(cfg.Owners), describeSource(choice.report))
	if cfg.HelperTask != "" {
		r.logf("discord bridge: owner helper conversation on for project %s", cfg.HelperTask)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var restart atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if r.watch(runCtx, choice) {
			restart.Store(true)
			cancel()
		}
	}()
	if r.OnBridge != nil {
		r.OnBridge(b)
	}
	err := b.Run(runCtx)
	cancel()
	wg.Wait()
	if restart.Load() || ctx.Err() != nil {
		if hub && !ok.Load() {
			// A deploy restart or a new save is not a failed start.
			clearCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			if err := r.State.Set(clearCtx, kvAttempt, ""); err != nil {
				r.logf("discord bridge: clear settings attempt: %v", err)
			}
		}
		return restart.Load(), nil
	}
	if hub && !ok.Load() {
		if rerr := r.refuse(context.Background(), choice.revision, api.BridgeRefusedStartupFailed); rerr != nil {
			r.logf("discord bridge: %v", rerr)
		}
	}
	return false, err
}

func describeSource(rep api.BridgeReport) string {
	switch rep.Source {
	case api.BridgeSourceHub:
		return fmt.Sprintf("saved settings revision %d", rep.Applied)
	case api.BridgeSourceLastGood:
		return fmt.Sprintf("the last good saved settings (revision %d)", rep.Applied)
	default:
		return "deployment values"
	}
}

// watch reads the saved settings at once and then every Poll, reporting
// what the bridge runs on. It returns true when a revision other than the
// applied and refused ones is saved, false when ctx ends. Read errors are
// logged (once per distinct error) and ignored.
func (r *settingsRunner) watch(ctx context.Context, choice settingsChoice) bool {
	report := &choice.report
	if _, err := api.ValidateBridgeReport(choice.report); err != nil {
		r.logf("discord bridge: the values in use do not pass the settings rules (%v); reading settings without a report", err)
		report = nil
	}
	poll := r.Poll
	if poll <= 0 {
		poll = SettingsPoll
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	lastErr := ""
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
		view, err := r.Hub.BridgeSettings(ctx, report)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return false
			}
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				r.logf("discord bridge: read saved settings: %v", err)
			}
		case view.Revision != choice.report.Applied && view.Revision != choice.report.Refused:
			r.logf("bridge settings changed to revision %d; restarting to apply", view.Revision)
			return true
		default:
			lastErr = ""
		}
		timer.Reset(poll)
	}
}
