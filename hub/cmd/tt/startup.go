package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

const trustBlocker = "Permission blocked: Codex is waiting for directory trust. Open this agent's terminal and review the directory shown before choosing Yes or No."

// Inspect only visible output in Tailterm-owned Codex panes. This never sends
// input, accepts trust, reads scrollback, or forwards terminal contents.
func codexTrustPrompt(screen string) bool {
	return strings.Contains(screen, "Do you trust the contents of this directory?") &&
		strings.Contains(screen, "1. Yes, continue") && strings.Contains(screen, "2. No, quit") &&
		strings.Contains(screen, "Press enter to continue")
}

func startupTmux(ctx context.Context, args ...string) ([]byte, error) {
	if socket := os.Getenv("TT_TMUX_SOCKET"); socket != "" {
		args = append([]string{"-L", socket}, args...)
	}
	return exec.CommandContext(ctx, "tmux", args...).Output()
}

func reportTrustBlocker(ctx context.Context, c *api.Client, task, agent, run, session string) error {
	a, err := c.GetAgent(ctx, task, agent)
	if err != nil {
		return err
	}
	if a.RunID != run || a.Session != session || a.Runtime != "codex" || a.Status == api.AgentClosed || a.Status == api.AgentExited || a.Status == api.AgentRetired {
		return nil
	}
	if a.Status == api.AgentNeedsInput && a.BlockedText == trustBlocker {
		return nil
	}
	_, err = c.PostEvent(ctx, task, api.PostEventRequest{Kind: api.EventNeedsInput, AgentID: agent, RunID: run, Text: trustBlocker, Data: map[string]any{"reason": "permission", "startupPrompt": "directory-trust"}})
	return err
}

func inspectStartupPrompts() {
	e := readEnv()
	c, err := e.client(3 * time.Second)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// JSON escaping in the tmux format safely preserves unusual session names.
	fields := []string{"session_name", "pane_id", "TAILTERM_HUB", "TAILTERM_TASK", "TAILTERM_AGENT", "TAILTERM_RUN", "TAILTERM_PERMISSION_RUNTIME"}
	for i, f := range fields {
		fields[i] = `"#{q/e:` + f + `}"`
	}
	raw, err := startupTmux(ctx, "list-panes", "-a", "-F", "["+strings.Join(fields, ",")+"]")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		var p []string
		if json.Unmarshal([]byte(line), &p) != nil || len(p) != 7 {
			continue
		}
		if p[2] != e.hub || p[6] != "codex" || !api.ValidID(p[3], "tsk") || !api.ValidID(p[4], "agt") || !runIDPattern.MatchString(p[5]) {
			continue
		}
		screen, err := startupTmux(ctx, "capture-pane", "-p", "-t", p[1])
		if err != nil || !codexTrustPrompt(string(screen)) {
			continue
		}
		// Querying the hub also checks run identity and prevents duplicate events.
		_ = reportTrustBlocker(ctx, c, p[3], p[4], p[5], p[0])
	}
}
