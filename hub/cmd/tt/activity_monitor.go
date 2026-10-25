package main

import (
	"context"
	"crypto/rand"
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

// activityWorktreeTimeout bounds both git calls of one worktree probe, so a
// slow git on a loaded host cannot hold the relay pass (wi_44e5b080b7e7d846).
var activityWorktreeTimeout = 2 * time.Second

func cachedActivityWorktree(ctx context.Context, cwd string) (string, error) {
	if cache, ok := ctx.Value(activityWorktreeContextKey{}).(activityWorktreeCache); ok {
		if result, found := cache[cwd]; found {
			return result.signature, result.err
		}
		// A timed-out probe is cached too: one wait per worktree per pass.
		signature, err := activityWorktree(ctx, cwd)
		cache[cwd] = activityWorktreeResult{signature, err}
		return signature, err
	}
	return activityWorktree(ctx, cwd)
}

func activityProbeNative(b runtimeBinding, a api.Agent) (bool, bool, error) {
	return probeRuntimeWithDiscovery(b, a, spawn.ProbeSession, nativeRuntimeSessionProbe, nativeRuntimePIDProbe, nativeRuntimeDiscovery)
}

func activityWorktree(ctx context.Context, cwd string) (string, error) {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, activityWorktreeTimeout)
	defer cancel()
	git := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-C", cwd}, args...)...)
		// A child that keeps stdout open cannot hold Output past the kill.
		cmd.WaitDelay = 500 * time.Millisecond
		return cmd.Output()
	}
	out, err := git("status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", err
	}
	h := sha256.New()
	h.Write(out)
	if head, headErr := git("rev-parse", "HEAD"); headErr == nil {
		h.Write(head)
	} else if ctx.Err() != nil {
		// Hashed without its head the signature would differ from the last
		// one and falsely mark the worktree changed; the caller keeps it.
		return "", ctx.Err()
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

// backgroundReasonPrefix starts every reason a running background task gives.
const backgroundReasonPrefix = "background "

// backgroundActivity is the state of a completed Claude turn whose session
// still runs a background task: working until the oldest task passes the
// bound, unknown after it. The reason names the tool, the task id and the
// launch time, never the command. ok is false when no task is running.
func backgroundActivity(c *activityCursor, now time.Time, threshold activityThresholds) (state, reason string, ok bool) {
	if len(c.Background) == 0 {
		return "", "", false
	}
	oldest := c.Background[0]
	for _, task := range c.Background[1:] {
		if task.Since.Before(oldest.Since) {
			oldest = task
		}
	}
	bound := threshold.Background
	if bound <= 0 {
		bound = activityBackgroundDefault * time.Second
	}
	reason = fmt.Sprintf("%s%s task %s running since %s", backgroundReasonPrefix, oldest.Tool, oldest.ID, oldest.Since.UTC().Format(time.RFC3339))
	if now.Sub(oldest.Since) > bound {
		return "unknown", fmt.Sprintf("%s, past the %dm bound", reason, int(bound/time.Minute)), true
	}
	if n := len(c.Background); n > 1 {
		reason += fmt.Sprintf(" (%d background tasks)", n)
	}
	return "working", reason, true
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
	if !tmuxAlive || !processAlive {
		// Background shells end with their runtime.
		c.Background = nil
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
	// A provider block ranks below crashed and above everything else while
	// the session can still make calls: its turns fail whatever its status
	// says. A done agent or a gone runtime makes no calls, so its stale block
	// is not reported; the cursor keeps it, and a resumed agent reports it
	// again. The exception is a done agent woken into the block, which
	// relayActivityTick reports (wokenDoneProviderBlock). A running session's
	// block has no time bound: only provider evidence clears it, because the
	// episode carries the owner ping and the login wake while the cause
	// remains (docs/provider-blocked.md).
	if block := reportableProviderBlock(c, threshold); block != nil && a.Status != api.AgentDone && tmuxAlive && processAlive {
		if applyProviderBlock(c, &result, block) {
			return result
		}
	}
	if a.Status == api.AgentNeedsInput {
		// The roster status may predate a turn that is still running. Only a
		// completed, unseen or unreadable turn is a planned wait; an open turn
		// is judged on its transcript below, without the hung or looping rules.
		if c.Unknown || !c.SeenTurn || c.TurnComplete {
			// A completed turn whose background task still runs is decided
			// as rule 10 decides it.
			if state, reason, ok := backgroundActivity(c, now, threshold); ok && !c.Unknown && c.SeenTurn {
				result.State, result.Reason = state, reason
				return result
			}
			result.Reason = "planned wait or input required"
			return result
		}
		// A pending tool call is working however long it runs.
		if pending := oldestPending(c); !pending.Since.IsZero() {
			result.State, result.PendingTool, result.PendingSince = "working", pending.Name, pending.Since
			return result
		}
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
		if state, reason, ok := backgroundActivity(c, now, threshold); ok {
			result.State, result.Reason = state, reason
			return result
		}
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

// applyProviderBlock sets result to the block's report and reports true when
// the state is decided. An older hub (ProviderUnsupported) gets the legacy
// form: Claude keeps its idle or finished_silent turn-end reason, decided by
// the caller, and Codex has unknown with a "provider_blocked:" reason.
func applyProviderBlock(c *activityCursor, result *api.AgentActivity, block *api.ProviderBlock) bool {
	if !c.ProviderUnsupported {
		result.State, result.Reason, result.Provider = "provider_blocked", "", block
		return true
	}
	if block.Runtime != "claude" {
		result.State, result.Reason = "unknown", "provider_blocked: "+block.Class
		return true
	}
	return false
}

// wokenDoneProviderBlock reports the block of a done agent that was woken into
// it. Codex agents are done between turns (only a Stop hook sets their status),
// so a wake whose turn fails would otherwise stay silent. The agent counts as
// woken while input waits for it (unread messages or open obligations) and
// either this check read a new failure of the block or the relay last reported
// it blocked. A message that arrived before the wake ran is not a failure, so a
// stale block is not reported for it. With no input waiting the agent reports
// its ordinary state again (docs/provider-blocked.md).
func wokenDoneProviderBlock(c *activityCursor, a api.Agent, state *api.AgentActivity, openObligations int, failed, tmuxAlive, processAlive bool, probeErr error, threshold activityThresholds) {
	if state.State == "provider_blocked" || a.Status != api.AgentDone || probeErr != nil || !tmuxAlive || !processAlive {
		return
	}
	if a.Unread == 0 && openObligations == 0 {
		return
	}
	if !failed && c.LastState != "provider_blocked" {
		return
	}
	if block := reportableProviderBlock(c, threshold); block != nil {
		applyProviderBlock(c, state, block)
	}
}

// queueMemberActivity is a team member's activity as `tt team queue list`
// prints it: the state and, for a provider block, its provider and class.
func queueMemberActivity(a *api.AgentActivity) string {
	if a == nil {
		return "activity=unknown"
	}
	if p := a.Provider; a.State == "provider_blocked" && p != nil {
		return fmt.Sprintf("activity=%s provider=%s class=%s", a.State, p.Provider, p.Class)
	}
	return "activity=" + a.State
}

// relayActivityState is the cursor file the relay saves. RequestNonce is
// random and made with each fresh cursor, so a cursor that restarts its
// transition count after the state directory is lost never reuses an earlier
// receipt's request ID for the same run (bug wi_15f305ae3eb57268). The stall
// watch reads the same file as a plain activityCursor.
type relayActivityState struct {
	activityCursor
	RequestNonce string `json:"requestNonce,omitempty"`
}

func activityRequestNonce() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func activityRequestID(b runtimeBinding, nonce string, n int64) string {
	digest := sha256.Sum256([]byte(b.Agent + "\x00" + b.Run + "\x00" + nonce + "\x00" + fmt.Sprint(n)))
	return "activity-" + hex.EncodeToString(digest[:12])
}

// relayActivityTick keeps private cursors and pending requests on the launch
// host. A pending report is persisted before HTTP so lost replies and restarts
// replay the same request ID. No transcript text is sent over HTTP.
func relayActivityTick(ctx context.Context, b runtimeBinding, client *api.Client, now time.Time, probe activityProbe) error {
	path := filepath.Join(relayDir(), bindingKey(b)+".activity.json")
	var saved relayActivityState
	fresh := true
	if data, err := os.ReadFile(path); err == nil {
		fresh = json.Unmarshal(data, &saved) != nil
	}
	c, nonce := saved.activityCursor, saved.RequestNonce
	if c.Run != b.Run || c.Thread != b.Thread {
		c, fresh = activityCursor{Run: b.Run, Thread: b.Thread}, true
	}
	// A cursor an older relay saved has no nonce; it gets one, and a pending
	// report keeps the request ID it was saved with.
	if fresh || nonce == "" {
		var err error
		if nonce, err = activityRequestNonce(); err != nil {
			return err
		}
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
		return writePrivateJSON(path, relayActivityState{activityCursor: c, RequestNonce: nonce})
	}
	// Metering failures never suppress activity transitions or alerts.
	if u != nil && u.hasPendingWork() {
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
		if retry, downgraded := stuckFallback(b, nonce, &c, err); downgraded {
			if err := save(); err != nil {
				return err
			}
			_, err = client.ReportActivity(ctx, b.Task, b.Agent, *retry)
		}
		if providerFallback(&c, err) {
			if err := save(); err != nil {
				return err
			}
		} else if err != nil {
			var httpErr *api.HTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != http.StatusConflict {
				return err
			}
			c.RejectedState = c.PendingReport.Activity.State
			c.RejectedWakeKey = activityReportKey(c.PendingReport.Activity)
			c.PendingReport = nil
			if err := save(); err != nil {
				return err
			}
		} else {
			c.LastState = c.PendingReport.Activity.State
			c.LastWakeKey = activityReportKey(c.PendingReport.Activity)
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
	// The block before this read, by value: noteProviderFailure changes it in
	// place. A new class, since or count is a failure this check read.
	hadBlock := c.ProviderBlock != nil
	var blockBefore providerBlockCursor
	if hadBlock {
		blockBefore = *c.ProviderBlock
	}
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
		failed := c.ProviderBlock != nil && (!hadBlock || c.ProviderBlock.Class != blockBefore.Class || !c.ProviderBlock.Since.Equal(blockBefore.Since) || c.ProviderBlock.Count > blockBefore.Count)
		wokenDoneProviderBlock(&c, a, &state, open, failed, tmuxAlive, processAlive, probeErr, activityDefaults())
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
			state.State, state.Prompt, state.Reason, state.Provider = "runtime_prompt", prompt, "", nil
		}
	}
	if b.Runtime == "claude" {
		var progress relayProgress
		if data, err := os.ReadFile(filepath.Join(relayDir(), bindingKey(b)+".progress.json")); err == nil && json.Unmarshal(data, &progress) == nil && progress.Run == b.Run && progress.Thread == b.Thread {
			state.Wake = progress.Wake
		}
	}
	if state.State != "runtime_prompt" && state.State != "provider_blocked" && probeErr == nil && tmuxAlive && processAlive {
		if reason := activityStuckReason(ctx, b, a, state.State, now, activityDefaults()); reason != "" {
			state.State, state.Reason = "stuck", reason
		} else if reason := claudeStallStuckReason(b, &c, now); observed && reason != "" {
			// A recorded stalled turn (claude_stall.go): open, nothing pending
			// and nothing written since the stall began.
			state.State, state.Reason = "stuck", reason
		}
	}
	if state.State == "stuck" && c.StuckUnsupported {
		downgradeStuck(&state)
	}
	// A verification run in this host's matrix lock is the agent's open work
	// even when its transcript is quiet (docs/agent-activity.md).
	state.MatrixWait = matrixWaitFor(a, now)
	key := activityReportKey(state)
	sameLast := state.State == c.LastState && sameActivityKey(c.LastWakeKey, state)
	if sameLast || (state.State == c.RejectedState && sameActivityKey(c.RejectedWakeKey, state)) {
		// A key an older relay saved upgrades in place, without a report, so
		// an upgrade sends no burst of reports from idle bindings.
		if sameLast {
			c.LastWakeKey = key
		} else {
			c.RejectedWakeKey = key
		}
		return save()
	}
	c.RejectedState, c.RejectedWakeKey = "", ""
	c.Transition++
	c.PendingReport = &api.ActivityReport{RequestID: activityRequestID(b, nonce, c.Transition), RunID: b.Run, Activity: state}
	c.LastReportAttempt = now
	if err := save(); err != nil {
		return err
	}
	_, err = client.ReportActivity(ctx, b.Task, b.Agent, *c.PendingReport)
	if retry, downgraded := stuckFallback(b, nonce, &c, err); downgraded {
		if err := save(); err != nil {
			return err
		}
		// A failure leaves the downgraded report pending; it replays.
		_, err = client.ReportActivity(ctx, b.Task, b.Agent, *retry)
		state = retry.Activity
	}
	if providerFallback(&c, err) {
		// The next tick observes again and reports the legacy form.
		return save()
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
//
// -u marks the client as UTF-8 capable. Without it a client whose LC_ALL,
// LC_CTYPE or LANG names no UTF-8 locale, and that is not inside tmux, gets
// every byte outside printable ASCII printed as "_", tabs included, so no row
// would split: the relay LaunchAgent sets only PATH.
func nativeActivityPaneSize(ctx context.Context, session string) (int, int, error) {
	raw, err := startupTmux(ctx, "-u", "list-panes", "-s", "-t", session, "-F", "#{window_name}\t#{pane_width}\t#{pane_height}")
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
func stuckFallback(b runtimeBinding, nonce string, c *activityCursor, err error) (*api.ActivityReport, bool) {
	var httpErr *api.HTTPError
	if c.PendingReport == nil || c.PendingReport.Activity.State != "stuck" || !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadRequest {
		return nil, false
	}
	c.StuckUnsupported = true
	report := *c.PendingReport
	downgradeStuck(&report.Activity)
	c.Transition++
	report.RequestID = activityRequestID(b, nonce, c.Transition)
	c.PendingReport = &report
	return c.PendingReport, true
}

// providerFallback handles a hub that rejects the provider_blocked state
// (HTTP 400). It remembers that, drops the rejected report and lets the next
// observation send the legacy form: for Claude the idle or finished_silent
// turn-end reason, for Codex unknown with a "provider_blocked:" reason. Later
// transitions are never blocked.
func providerFallback(c *activityCursor, err error) bool {
	var httpErr *api.HTTPError
	if c.PendingReport == nil || c.PendingReport.Activity.State != "provider_blocked" || !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadRequest {
		return false
	}
	c.ProviderUnsupported = true
	c.PendingReport = nil
	c.LastCheck = time.Time{}
	return true
}

// activityReportKey is what, besides the state, makes a new activity report.
// An idle or finished_silent reason is part of it, so a turn that ends by an
// API rate limit reaches the hub (docs/handler-ab.md); stuck reasons change
// too often and are left out. The version prefix tells it from the key an
// older relay saved, which had no reason.
func activityReportKey(a api.AgentActivity) string {
	key := activityKeyVersion + legacyActivityKey(a)
	// A background task's reason makes a new report too: a turn that closes
	// with a task running, or a new oldest task. Other working reports carry
	// no reason, so keys an older relay saved are unchanged.
	if a.State == "idle" || a.State == "finished_silent" || strings.HasPrefix(a.Reason, backgroundReasonPrefix) {
		key += "\x00reason=" + a.Reason
	}
	// A class, model, code or status change is a new report; a steady block
	// is not.
	if p := a.Provider; a.State == "provider_blocked" && p != nil {
		key += "\x00provider=" + p.Class + "\x00" + p.Model + "\x00" + p.Code + "\x00" + strconv.Itoa(p.Status) + "\x00" + p.Since.UTC().Format(time.RFC3339Nano)
	}
	return key + matrixWaitKey(a.MatrixWait)
}

const activityKeyVersion = "k2\x00"

func legacyActivityKey(a api.AgentActivity) string {
	return wakeKey(a.Wake) + runtimePromptKey(a.Prompt)
}

// sameActivityKey compares a saved key with an observation. An older relay's
// key cannot show the reason, so it is compared without one.
func sameActivityKey(saved string, a api.AgentActivity) bool {
	if strings.HasPrefix(saved, activityKeyVersion) {
		return saved == activityReportKey(a)
	}
	return saved == legacyActivityKey(a)
}

func wakeKey(w *api.WakeOutcome) string {
	if w == nil {
		return ""
	}
	data, _ := json.Marshal(w)
	return string(data)
}
