package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// windowSizeInterval spaces reconcile passes; one list-windows per pass.
const windowSizeInterval = 15 * time.Second

// reconcileAgentWindowSizes restores the fixed size policy on Tailterm-owned
// agent sessions created before spawn set it, or shrunk anyway. A window whose
// window-size is not manual, or that is below the usable minimum, gets the
// session default-size, window-size manual and a resize to the agent default.
// A manual window at or above the minimum is left alone, so a deliberate larger
// size survives. Human sessions and global options are never touched. It
// returns the number of windows it changed.
func reconcileAgentWindowSizes(ctx context.Context, run func(context.Context, ...string) ([]byte, error), logf func(string, ...any)) (int, error) {
	sessions, err := localSessions(ctx)
	if err != nil || len(sessions) == 0 {
		return 0, err
	}
	owned := map[string]bool{}
	for _, s := range sessions {
		// The owner helper's session is the owner's terminal: never resized.
		if s.valid() && s.Role != api.AgentRoleOwnerHelper {
			owned[s.ID] = true
		}
	}
	if len(owned) == 0 {
		return 0, nil
	}
	raw, err := run(ctx, "list-windows", "-a", "-F", "#{session_id}\t#{session_name}\t#{window_id}\t#{window_width}\t#{window_height}\t#{window-size}")
	if err != nil {
		return 0, fmt.Errorf("list agent windows: %w", err)
	}
	changed := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 6 || !owned[f[0]] || !windowIDPattern(f[2]) {
			continue
		}
		cols, colsErr := strconv.Atoi(f[3])
		rows, rowsErr := strconv.Atoi(f[4])
		if colsErr != nil || rowsErr != nil {
			continue
		}
		if f[5] == "manual" && cols >= spawn.MinUsableCols && rows >= spawn.MinUsableRows {
			continue
		}
		// One tmux command: session default, window policy, then the size.
		if _, err := run(ctx, "set-option", "-t", f[0], "default-size", spawn.AgentDefaultSize, ";",
			"set-option", "-w", "-t", f[2], "window-size", "manual", ";",
			"resize-window", "-t", f[2], "-x", strconv.Itoa(spawn.AgentWindowCols), "-y", strconv.Itoa(spawn.AgentWindowRows)); err != nil {
			return changed, fmt.Errorf("resize %s window %s: %w", f[1], f[2], err)
		}
		changed++
		logf("[tt relay] %s %s window resized %dx%d -> %s (window-size was %s)\n", time.Now().UTC().Format(time.RFC3339), f[1], cols, rows, spawn.AgentDefaultSize, f[5])
	}
	return changed, nil
}

func windowIDPattern(id string) bool {
	if len(id) < 2 || id[0] != '@' {
		return false
	}
	_, err := strconv.Atoi(id[1:])
	return err == nil
}

func relayWindowSizeTick() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := reconcileAgentWindowSizes(ctx, startupTmux, func(format string, args ...any) { fmt.Fprintf(os.Stderr, format, args...) }); err != nil {
		fmt.Fprintf(os.Stderr, "[tt relay] window size: %v\n", err)
	}
}
