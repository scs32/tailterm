package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

type activityProbe func(runtimeBinding, api.Agent) (tmuxAlive, processAlive bool, err error)

type activityWorktreeContextKey struct{}
type activityWorktreeResult struct {
	signature string
	err       error
}
type activityWorktreeCache map[string]activityWorktreeResult

func cachedActivityWorktree(ctx context.Context, cwd string) (string, error) {
	if cache, ok := ctx.Value(activityWorktreeContextKey{}).(activityWorktreeCache); ok {
		if result, found := cache[cwd]; found {
			return result.signature, result.err
		}
		signature, err := activityWorktree(cwd)
		cache[cwd] = activityWorktreeResult{signature, err}
		return signature, err
	}
	return activityWorktree(cwd)
}

func activityProbeNative(b runtimeBinding, a api.Agent) (bool, bool, error) {
	return probeRuntimeWithDiscovery(b, a, spawn.ProbeSession, nativeRuntimeSessionProbe, nativeRuntimePIDProbe, nativeRuntimeDiscovery)
}

func activityWorktree(cwd string) (string, error) {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return "", nil
	}
	out, err := exec.Command("git", "--no-optional-locks", "-C", cwd, "status", "--porcelain=v1", "--untracked-files=normal").Output()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(out)
	if head, headErr := exec.Command("git", "--no-optional-locks", "-C", cwd, "rev-parse", "HEAD").Output(); headErr == nil {
		h.Write(head)
	}
	// File metadata catches edits to an already modified file without retaining
	// source bytes or filenames in the durable checkpoint.
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if info, statErr := os.Stat(filepath.Join(cwd, path)); statErr == nil {
			fmt.Fprintf(h, "%s:%d:%d", path, info.Size(), info.ModTime().UnixNano())
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

func oldestPending(c *activityCursor) pendingActivityCall {
	var oldest pendingActivityCall
	for _, p := range c.Pending {
		if oldest.Since.IsZero() || p.Since.Before(oldest.Since) {
			oldest = p
		}
	}
	return oldest
}

func activityState(c *activityCursor, a api.Agent, openObligations int, tmuxAlive, processAlive bool, probeErr error, now time.Time, threshold activityThresholds) api.AgentActivity {
	result := api.AgentActivity{State: "unknown", ObservedAt: now, LastEventAt: c.LastEventAt, Tokens: c.Tokens}
	if a.Role == api.AgentRoleOwnerHelper && (probeErr != nil || !tmuxAlive || !processAlive) {
		// The owner's own session: unverified or absent means offline, never crashed.
		c.MissingSince = time.Time{}
		result.Reason = "owner session offline"
		return result
	}
	if probeErr != nil {
		c.MissingSince = time.Time{}
		result.Reason = "process probe unavailable"
		return result
	}
	if a.Status == api.AgentRunning {
		if !tmuxAlive || !processAlive {
			if c.MissingSince.IsZero() {
				c.MissingSince = now
			}
			if now.Sub(c.MissingSince) >= threshold.CrashProbe {
				result.State = "crashed"
				result.Reason = "runtime or tmux absent on two probes"
			}
			return result
		}
	}
	c.MissingSince = time.Time{}
	if a.Status == api.AgentNeedsInput {
		result.Reason = "planned wait or input required"
		return result
	}
	if c.Unknown {
		result.Reason = "unrecognized transcript format"
		return result
	}
	if !c.SeenTurn {
		result.Reason = "turn boundary unavailable"
		return result
	}
	if a.Status == api.AgentDone && !c.TurnComplete {
		result.Reason = "turn completion unavailable"
		return result
	}
	if a.Status == api.AgentRunning {
		pending := oldestPending(c)
		if !pending.Since.IsZero() {
			result.PendingTool, result.PendingSince = pending.Name, pending.Since
			deadline := pending.Since.Add(threshold.Hung)
			if pending.WaitUntil.After(deadline) {
				deadline = pending.WaitUntil
			}
			if !now.Before(deadline) {
				result.State = "hung_tool"
				return result
			}
		}
		if c.Worktree != "" && len(c.Completed) >= threshold.LoopCalls {
			recent := c.Completed[len(c.Completed)-threshold.LoopCalls:]
			loop := now.Sub(recent[0].At) >= threshold.Loop && recent[len(recent)-1].Tokens > recent[0].Tokens
			for _, call := range recent {
				if call.PlannedWait || call.Signature != recent[0].Signature || call.Worktree != recent[0].Worktree {
					loop = false
					break
				}
			}
			if loop && c.Worktree == recent[0].Worktree {
				result.State = "looping"
				return result
			}
		}
	}
	if c.TurnComplete {
		if openObligations > 0 {
			result.State = "finished_silent"
		} else {
			result.State = "idle"
		}
		result.Reason = c.TurnEndReason
		return result
	}
	if now.Sub(c.LastEventAt) <= threshold.Working || now.Sub(c.WorktreeChangedAt) <= threshold.Working {
		result.State = "working"
	} else {
		result.Reason = "no recent execution evidence"
	}
	return result
}

func activityRequestID(b runtimeBinding, n int64) string {
	digest := sha256.Sum256([]byte(b.Agent + "\x00" + b.Run + "\x00" + fmt.Sprint(n)))
	return "activity-" + hex.EncodeToString(digest[:12])
}

// relayActivityTick keeps private cursors and pending requests on the launch
// host. A pending report is persisted before HTTP so lost replies and restarts
// replay the same request ID. No transcript text is sent over HTTP.
func relayActivityTick(ctx context.Context, b runtimeBinding, client *api.Client, now time.Time, probe activityProbe) error {
	path := filepath.Join(relayDir(), bindingKey(b)+".activity.json")
	var c activityCursor
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	if c.Run != b.Run || c.Thread != b.Thread {
		c = activityCursor{Run: b.Run, Thread: b.Thread}
	}
	wakeKey := func(w *api.WakeOutcome) string {
		if w == nil {
			return ""
		}
		data, _ := json.Marshal(w)
		return string(data)
	}
	var u *usageCursor
	if enabled, _ := ctx.Value(usageEnabledContextKey{}).(bool); enabled {
		var usageErr error
		u, usageErr = loadUsageCursor(b)
		if usageErr != nil {
			fmt.Fprintln(os.Stderr, "[tt relay] usage cursor unavailable:", usageErr)
		}
	}
	save := func() error {
		if u != nil {
			if err := saveUsageCursor(u); err != nil {
				return err
			}
		}
		return writePrivateJSON(path, c)
	}
	// Metering failures never suppress activity transitions or alerts.
	if u != nil && (len(u.Dirty) > 0 || u.Pending != nil || !u.Enrolled || u.Coverage != u.UploadedCoverage) {
		if err := uploadUsage(ctx, u, client); err != nil {
			fmt.Fprintln(os.Stderr, "[tt relay] usage upload deferred:", err)
		}
	}
	if c.Ineligible {
		return nil
	}
	if c.PendingReport != nil {
		if !c.LastReportAttempt.IsZero() && now.Sub(c.LastReportAttempt) < 15*time.Second {
			return nil
		}
		c.LastReportAttempt = now
		if err := save(); err != nil {
			return err
		}
		_, err := client.ReportActivity(ctx, b.Task, b.Agent, *c.PendingReport)
		if retry, downgraded := stuckFallback(b, &c, err); downgraded {
			if err := save(); err != nil {
				return err
			}
			_, err = client.ReportActivity(ctx, b.Task, b.Agent, *retry)
		}
		if err != nil {
			var httpErr *api.HTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != http.StatusConflict {
				return err
			}
			c.RejectedState = c.PendingReport.Activity.State
			c.RejectedWakeKey = wakeKey(c.PendingReport.Activity.Wake) + runtimePromptKey(c.PendingReport.Activity.Prompt)
			c.PendingReport = nil
			if err := save(); err != nil {
				return err
			}
		} else {
			c.LastState = c.PendingReport.Activity.State
			c.LastWakeKey = wakeKey(c.PendingReport.Activity.Wake) + runtimePromptKey(c.PendingReport.Activity.Prompt)
			c.PendingReport = nil
			if err := save(); err != nil {
				return err
			}
		}
	}
	if !c.LastCheck.IsZero() && now.Sub(c.LastCheck) < 15*time.Second {
		return nil
	}
	c.LastCheck = now
	a, err := client.GetAgent(ctx, b.Task, b.Agent)
	if err != nil {
		_ = save()
		return err
	}
	if a.RunID != b.Run || a.Status == api.AgentClosed || a.Status == api.AgentExited {
		c.Ineligible = true
		return save()
	}
	if a.Status == api.AgentRetired {
		return save()
	}
	transcript := c.Path
	var transcriptErr error
	if transcript != "" {
		if _, statErr := os.Stat(transcript); statErr != nil {
			transcript = ""
		}
	}
	if transcript == "" || now.Sub(c.LastDiscovery) >= time.Minute {
		transcript, transcriptErr = activityTranscript(b)
		c.LastDiscovery = now
	}
	if transcriptErr != nil {
		c.Unknown = true
	}
	if transcript == "" && b.CreatedAt.IsZero() && !c.SeenTurn {
		// Runtime creation and transcript discovery are not simultaneous. Keep
		// legacy bindings' delivery untouched until an exact session file exists.
		return save()
	}
	if transcript == "" {
		c.Unknown = true
		c.MissingTranscript = true
	}
	var transcriptReadErr error
	if transcript != "" {
		if c.MissingTranscript {
			c.Unknown = false
			c.MissingTranscript = false
		}
		parse := parseCodexActivity
		if b.Runtime == "claude" {
			parse = parseClaudeActivity
		}
		transcriptReadErr = readActivityAppend(transcript, &c, func(line []byte, cursor *activityCursor) error {
			var usageParseErr error
			if u != nil {
				if len(u.Dirty) < 512 {
					usageParseErr = u.parse(line)
				} else {
					u.Coverage = "partial: usage outbox capacity exceeded"
				}
			}
			activityParseErr := parse(line, cursor)
			if usageParseErr != nil {
				u.Coverage = "partial: malformed usage record"
			}
			return activityParseErr
		})
		if u != nil && c.Skipping {
			u.Coverage = "partial: oversized transcript record"
		}
		if u != nil {
			if err := freezeUsageBatch(u); err != nil {
				fmt.Fprintln(os.Stderr, "[tt relay] usage batch freeze deferred:", err)
			}
		}
		if transcriptReadErr != nil {
			c.Unknown = true
			c.Ready = false
		}
	}
	tmuxAlive, processAlive, probeErr := probe(b, a)
	var state api.AgentActivity
	observed := false
	if transcript != "" && !c.Ready {
		if transcriptReadErr == nil && probeErr == nil && tmuxAlive && processAlive {
			// A healthy writer may still finish its tail. Continue bounded catch-up
			// without publishing partial transcript data, but reset crash continuity.
			c.MissingSince = time.Time{}
			return save()
		}
		// Runtime health does not depend on transcript readability. Preserve the
		// two-probe rule using a health-only cursor: no stale timestamps or totals
		// escape during bootstrap; established verified values remain independent
		// of partially accumulated parsing during subsequent health failures.
		health := activityCursor{Unknown: true, MissingSince: c.MissingSince}
		if v := c.Verified; v != nil {
			// On an open/read failure, still prove the file identity before retaining
			// its old verified fields. A replacement/truncation must bootstrap anew.
			info, err := os.Stat(transcript)
			if err == nil && v.Path == transcript && v.FileID == fileIdentity(info) && info.Size() >= c.Offset && info.Size() >= v.Offset {
				health.LastEventAt, health.Tokens = v.LastEventAt, v.Tokens
			} else {
				c.Verified = nil
			}
		}
		state = activityState(&health, a, 0, tmuxAlive, processAlive, probeErr, now, activityDefaults())
		c.MissingSince = health.MissingSince
		if transcriptReadErr != nil && state.Reason == "unrecognized transcript format" {
			state.Reason = "transcript read unavailable"
		}
	} else {
		if signature, err := cachedActivityWorktree(ctx, b.Cwd); err == nil && signature != "" {
			if c.Worktree != "" && c.Worktree != signature {
				c.WorktreeChangedAt = now
			}
			c.Worktree = signature
		}
		open := 0
		if c.TurnComplete {
			obligations, listErr := client.ListObligations(ctx, b.Task, b.Agent, "", true, false)
			if listErr != nil {
				_ = save()
				return listErr
			}
			open = len(obligations)
		}
		if transcript != "" && c.Ready && c.SeenTurn && !c.Unknown && c.TokensVerified {
			c.Verified = &verifiedActivitySnapshot{Path: c.Path, FileID: c.FileID, Offset: c.Offset, LastEventAt: c.LastEventAt, Tokens: c.Tokens}
		}
		state = activityState(&c, a, open, tmuxAlive, processAlive, probeErr, now, activityDefaults())
		observed = true
	}
	// A live runtime quiet on its own modal prompt is not idle at its input.
	// The agent's hub status (needs_input included) is left as it is.
	if deps, ok := ctx.Value(runtimePromptDepsKey{}).(*runtimePromptDeps); ok && observed && probeErr == nil && tmuxAlive && processAlive {
		prompt, promptErr := observeRuntimePrompt(ctx, deps, b, client, &c, now)
		if promptErr != nil {
			fmt.Fprintf(os.Stderr, "[tt relay] %s runtime prompt: %v\n", b.Agent, promptErr)
		}
		if prompt != nil {
			state.State, state.Prompt, state.Reason = "runtime_prompt", prompt, ""
		}
	}
	if b.Runtime == "claude" {
		var progress relayProgress
		if data, err := os.ReadFile(filepath.Join(relayDir(), bindingKey(b)+".progress.json")); err == nil && json.Unmarshal(data, &progress) == nil && progress.Run == b.Run && progress.Thread == b.Thread {
			state.Wake = progress.Wake
		}
	}
	if state.State != "runtime_prompt" && probeErr == nil && tmuxAlive && processAlive {
		if reason := activityStuckReason(ctx, b, a, state.State, now, activityDefaults()); reason != "" {
			state.State, state.Reason = "stuck", reason
		}
	}
	if state.State == "stuck" && c.StuckUnsupported {
		downgradeStuck(&state)
	}
	key := wakeKey(state.Wake) + runtimePromptKey(state.Prompt)
	if (state.State == c.LastState && key == c.LastWakeKey) || (state.State == c.RejectedState && key == c.RejectedWakeKey) {
		return save()
	}
	c.RejectedState, c.RejectedWakeKey = "", ""
	c.Transition++
	c.PendingReport = &api.ActivityReport{RequestID: activityRequestID(b, c.Transition), RunID: b.Run, Activity: state}
	c.LastReportAttempt = now
	if err := save(); err != nil {
		return err
	}
	_, err = client.ReportActivity(ctx, b.Task, b.Agent, *c.PendingReport)
	if retry, downgraded := stuckFallback(b, &c, err); downgraded {
		if err := save(); err != nil {
			return err
		}
		// A failure leaves the downgraded report pending; it replays.
		_, err = client.ReportActivity(ctx, b.Task, b.Agent, *retry)
		state = retry.Activity
	}
	if err != nil {
		var httpErr *api.HTTPError
		if !errors.As(err, &httpErr) || httpErr.Status != http.StatusConflict {
			return err
		}
		c.RejectedState, c.RejectedWakeKey = state.State, key
		c.PendingReport = nil
		return save()
	}
	c.LastState, c.LastWakeKey = state.State, key
	c.PendingReport = nil
	return save()
}

// activityPaneSize reads the agent pane's size; tests replace it.
var activityPaneSize = nativeActivityPaneSize

// nativeActivityPaneSize returns the size of the session's agent-window pane,
// or its first pane when no window is named for the agent.
func nativeActivityPaneSize(ctx context.Context, session string) (int, int, error) {
	raw, err := startupTmux(ctx, "list-panes", "-s", "-t", session, "-F", "#{window_name}\t#{pane_width}\t#{pane_height}")
	if err != nil {
		return 0, 0, err
	}
	var cols, rows int
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		w, werr := strconv.Atoi(f[1])
		h, herr := strconv.Atoi(f[2])
		if werr != nil || herr != nil {
			continue
		}
		if !found || f[0] == spawn.AgentWindow {
			cols, rows, found = w, h, true
		}
		if f[0] == spawn.AgentWindow {
			break
		}
	}
	if !found {
		return 0, 0, errors.New("agent pane size unavailable")
	}
	return cols, rows, nil
}

// activityStuckReason names why a live agent cannot make progress: its pane is
// too small for the runtime UI, or (Claude) a wake has stayed unconfirmed with
// unread input past the threshold. It returns "" when neither applies.
func activityStuckReason(ctx context.Context, b runtimeBinding, a api.Agent, state string, now time.Time, threshold activityThresholds) string {
	// The owner sizes the helper's terminal; its pane size is never a stuck reason.
	if b.Session != "" && b.Role != api.AgentRoleOwnerHelper && a.Role != api.AgentRoleOwnerHelper {
		if cols, rows, err := activityPaneSize(ctx, b.Session); err == nil && (cols < spawn.MinUsableCols || rows < spawn.MinUsableRows) {
			return fmt.Sprintf("pane %dx%d below minimum %dx%d", cols, rows, spawn.MinUsableCols, spawn.MinUsableRows)
		}
	}
	if b.Runtime != "claude" || a.Unread == 0 || (state != "idle" && state != "finished_silent" && state != "unknown") {
		return ""
	}
	data, err := os.ReadFile(claudeWakePath(b))
	if err != nil {
		return ""
	}
	var intent claudeWakeIntent
	if json.Unmarshal(data, &intent) != nil || intent.Run != b.Run || (intent.Phase != "uncertain" && intent.Phase != "exhausted") {
		return ""
	}
	since := intent.FirstAt
	if since.IsZero() {
		since = intent.At
	}
	age := now.Sub(since)
	if since.IsZero() || age < threshold.WakeStuck {
		return ""
	}
	reason := fmt.Sprintf("Claude wake unconfirmed for %dm (retry %d/%d)", int(age.Minutes()), intent.Attempts, claudeWakeMaxRetries)
	if intent.Phase == "exhausted" {
		reason += " exhausted"
	}
	if intent.LastRetry != "" {
		reason += ": " + intent.LastRetry
	}
	return claudeClip(strings.Join(strings.Fields(reason), " "), 240)
}

// downgradeStuck turns a stuck observation into the unknown state an older hub
// accepts, keeping the reason.
func downgradeStuck(state *api.AgentActivity) {
	state.State = "unknown"
	state.Reason = claudeClip("stuck: "+state.Reason, 240)
}

// stuckFallback handles a hub that rejects the stuck state (HTTP 400): it
// remembers the downgrade and replaces the pending report with an unknown one
// under a new request ID, so later transitions are never blocked.
func stuckFallback(b runtimeBinding, c *activityCursor, err error) (*api.ActivityReport, bool) {
	var httpErr *api.HTTPError
	if c.PendingReport == nil || c.PendingReport.Activity.State != "stuck" || !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadRequest {
		return nil, false
	}
	c.StuckUnsupported = true
	report := *c.PendingReport
	downgradeStuck(&report.Activity)
	c.Transition++
	report.RequestID = activityRequestID(b, c.Transition)
	c.PendingReport = &report
	return c.PendingReport, true
}
