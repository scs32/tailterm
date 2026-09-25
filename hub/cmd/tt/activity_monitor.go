package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

type activityProbe func(api.Agent) (tmuxAlive, processAlive bool, err error)

func activityProbeNative(a api.Agent) (bool, bool, error) {
	if a.Session == "" {
		return false, false, errors.New("agent has no tmux session")
	}
	tmuxAlive, err := spawn.ProbeSession(a.Session)
	if err != nil {
		return false, false, err
	}
	// tt wrap sends a heartbeat every 30 seconds while its runtime child is
	// alive. The hub's 90-second online window is the process probe; tmux
	// presence alone would mistake the wrapper's post-exit shell for a runtime.
	return tmuxAlive, tmuxAlive && a.Online, nil
}

func activityWorktree(cwd string) (string, error) {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return "", nil
	}
	out, err := exec.Command("git", "-C", cwd, "status", "--porcelain=v1", "--untracked-files=normal").Output()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(out)
	if head, headErr := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").Output(); headErr == nil {
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
	if probeErr != nil {
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
			for _, call := range recent[1:] {
				if call.Signature != recent[0].Signature || call.Worktree != recent[0].Worktree {
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
	save := func() error { return writePrivateJSON(path, c) }
	if c.PendingReport != nil {
		if !c.LastReportAttempt.IsZero() && now.Sub(c.LastReportAttempt) < 15*time.Second {
			return nil
		}
		c.LastReportAttempt = now
		if err := save(); err != nil {
			return err
		}
		if _, err := client.ReportActivity(ctx, b.Task, b.Agent, *c.PendingReport); err != nil {
			return err
		}
		c.LastState = c.PendingReport.Activity.State
		c.PendingReport = nil
		if err := save(); err != nil {
			return err
		}
	}
	if !c.LastCheck.IsZero() && now.Sub(c.LastCheck) < 15*time.Second {
		return nil
	}
	c.LastCheck = now
	transcript, err := activityTranscript(b)
	if err != nil {
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
	if transcript != "" {
		if c.MissingTranscript {
			c.Unknown = false
			c.MissingTranscript = false
		}
		parse := parseCodexActivity
		if b.Runtime == "claude" {
			parse = parseClaudeActivity
		}
		if err := readActivityAppend(transcript, &c, parse); err != nil {
			c.Unknown = true
		}
	}
	if signature, err := activityWorktree(b.Cwd); err == nil && signature != "" {
		if c.Worktree != "" && c.Worktree != signature {
			c.WorktreeChangedAt = now
		}
		c.Worktree = signature
	}
	a, err := client.GetAgent(ctx, b.Task, b.Agent)
	if err != nil {
		_ = save()
		return err
	}
	if a.RunID != b.Run || a.Status == api.AgentClosed || a.Status == api.AgentExited || a.Status == api.AgentRetired {
		return save()
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
	tmuxAlive, processAlive, probeErr := probe(a)
	state := activityState(&c, a, open, tmuxAlive, processAlive, probeErr, now, activityDefaults())
	if state.State == c.LastState {
		return save()
	}
	c.Transition++
	c.PendingReport = &api.ActivityReport{RequestID: activityRequestID(b, c.Transition), RunID: b.Run, Activity: state}
	c.LastReportAttempt = now
	if err := save(); err != nil {
		return err
	}
	if _, err := client.ReportActivity(ctx, b.Task, b.Agent, *c.PendingReport); err != nil {
		return err
	}
	c.LastState = state.State
	c.PendingReport = nil
	return save()
}
