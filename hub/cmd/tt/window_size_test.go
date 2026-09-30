package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

func TestRelayReconcileRestoresShrunkAgentWindowOnly(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	sock := fmt.Sprintf("tt-reconcile-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Setenv("TT_TMUX_SOCKET", sock)
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
		_ = exec.Command("tmux", "-L", sock+"-viewer", "kill-server").Run()
	})
	ctx := context.Background()
	run := func(args ...string) string {
		out, err := exec.Command("tmux", append([]string{"-L", sock, "-f", "/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	owned := func(name, agent, run_ string) []string {
		return []string{"new-session", "-d", "-s", name, "-n", spawn.AgentWindow, "-x", "200", "-y", "50",
			"-e", "TAILTERM_HUB=http://hub.test", "-e", "TAILTERM_TASK=tsk_0000000000000001",
			"-e", "TAILTERM_AGENT=" + agent, "-e", "TAILTERM_RUN=" + run_, "sleep 60"}
	}
	// A pre-fix agent session: window-size latest, shrunk by a 16x2 viewer.
	run(owned("old-agent", "agt_0000000000000001", "run_0000000000000001")...)
	run("set-option", "-w", "-t", "old-agent:", "window-size", "latest")
	// An owner's deliberate larger manual size.
	run(owned("big-agent", "agt_0000000000000002", "run_0000000000000002")...)
	run("set-option", "-w", "-t", "big-agent:", "window-size", "manual")
	run("resize-window", "-t", "big-agent:", "-x", "220", "-y", "60")
	// A human session behind the same kind of viewer.
	run("new-session", "-d", "-s", "human", "-x", "200", "-y", "50", "sleep 60")
	attachViewer(t, run, sock, "old-agent")
	attachViewer(t, run, sock, "human")
	size := func(s string) string {
		return run("display-message", "-p", "-t", s+":", "#{window_width}x#{window_height} #{window-size}")
	}
	if got := size("old-agent"); got != "16x1 latest" {
		t.Fatalf("fixture did not reproduce the shrink: %q", got)
	}
	globals := run("show-options", "-g") + run("show-options", "-gw")

	var writes []string
	var logs []string
	counting := func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] != "list-windows" {
			writes = append(writes, strings.Join(args, " "))
		}
		return startupTmux(ctx, args...)
	}
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	changed, err := reconcileAgentWindowSizes(ctx, counting, logf)
	if err != nil || changed != 1 {
		t.Fatalf("first pass changed=%d err=%v writes=%v", changed, err, writes)
	}
	if got := size("old-agent"); got != "200x50 manual" {
		t.Fatalf("shrunk agent window after reconcile = %q", got)
	}
	if got := run("show-options", "-t", "old-agent", "default-size"); got != "default-size 200x50" {
		t.Fatalf("agent default-size = %q", got)
	}
	if got := size("big-agent"); got != "220x60 manual" {
		t.Fatalf("deliberate manual size changed: %q", got)
	}
	if got := size("human"); got != "16x1 latest" {
		t.Fatalf("human session touched: %q", got)
	}
	if after := run("show-options", "-g") + run("show-options", "-gw"); after != globals {
		t.Fatalf("global options changed:\nbefore %s\nafter %s", globals, after)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "old-agent window resized 16x1 -> 200x50 (window-size was latest)") {
		t.Fatalf("log = %q", logs)
	}
	writes = nil
	if changed, err := reconcileAgentWindowSizes(ctx, counting, logf); err != nil || changed != 0 || len(writes) != 0 {
		t.Fatalf("second pass changed=%d err=%v writes=%v", changed, err, writes)
	}
}

// attachViewer attaches a 16x2 tmux client to session from a 16x2 pane on a
// second private server, like a hidden TailOS tile.
func attachViewer(t *testing.T, run func(...string) string, sock, session string, flags ...string) {
	t.Helper()
	attach := fmt.Sprintf("env -u TMUX tmux -L %s attach-session %s -t %s", spawn.ShellQuote(sock), strings.Join(flags, " "), spawn.ShellQuote("="+session))
	if out, err := exec.Command("tmux", "-L", sock+"-viewer", "-f", "/dev/null", "new-session", "-d", "-x", strconv.Itoa(16), "-y", "2", attach).CombinedOutput(); err != nil {
		t.Fatalf("viewer: %v: %s", err, out)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if strings.Contains(run("list-clients", "-F", "#{client_width}x#{client_height} #{session_name}"), "16x2 "+session) {
			time.Sleep(100 * time.Millisecond)
			return
		}
	}
	t.Fatalf("viewer never attached to %s", session)
}

// The TailOS agent attach (shared/tmux-command.js) passes -f ignore-size, so a
// tiny tile never overrides another client. It is not a guarantee on its own:
// on tmux 3.7b a lone ignore-size client, the only client on its server, still
// sized a window-size latest window to 16x1 (whether it does depends on the
// server's other clients). The fixed manual size from spawn and the relay
// reconcile is the control that protects agents.
func TestIgnoreSizeAttachSemantics(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	sock := fmt.Sprintf("tt-ignore-size-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
		_ = exec.Command("tmux", "-L", sock+"-viewer", "kill-server").Run()
	})
	run := func(args ...string) string {
		out, err := exec.Command("tmux", append([]string{"-L", sock, "-f", "/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	size := func(s string) string {
		return run("display-message", "-p", "-t", s+":", "#{window_width}x#{window_height} #{window-size}")
	}
	// A normal viewer is attached; the tiny ignore-size tile does not override it.
	run("new-session", "-d", "-s", "shared", "-x", "200", "-y", "50", "sleep 60")
	run("set-option", "-w", "-t", "shared:", "window-size", "latest")
	attachSized(t, run, sock, "shared", 120, 40)
	attachViewer(t, run, sock, "shared", "-f", "ignore-size")
	if got := size("shared"); got != "120x39 latest" { // the status line takes a row
		t.Fatalf("ignore-size tile overrode the normal client: %q", got)
	}
	// With the agent policy (manual), a lone ignore-size client changes nothing.
	run("new-session", "-d", "-s", "alone", "-x", "200", "-y", "50", "sleep 60")
	run("set-option", "-w", "-t", "alone:", "window-size", "manual")
	attachViewer(t, run, sock, "alone", "-f", "ignore-size")
	if got := size("alone"); got != "200x50 manual" {
		t.Fatalf("manual window behind a lone ignore-size client: %q", got)
	}
}

// attachSized attaches a normal client of the given size to session.
func attachSized(t *testing.T, run func(...string) string, sock, session string, cols, rows int) {
	t.Helper()
	attach := fmt.Sprintf("env -u TMUX tmux -L %s attach-session -t %s", spawn.ShellQuote(sock), spawn.ShellQuote("="+session))
	if out, err := exec.Command("tmux", "-L", sock+"-viewer", "-f", "/dev/null", "new-session", "-d", "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows), attach).CombinedOutput(); err != nil {
		t.Fatalf("viewer: %v: %s", err, out)
	}
	want := fmt.Sprintf("%dx%d %s", cols, rows, session)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if strings.Contains(run("list-clients", "-F", "#{client_width}x#{client_height} #{session_name}"), want) {
			time.Sleep(100 * time.Millisecond)
			return
		}
	}
	t.Fatalf("viewer never attached to %s", session)
}

func TestWindowSizeSkipsOwnerHelper(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	sock := fmt.Sprintf("tt-helper-size-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Setenv("TT_TMUX_SOCKET", sock)
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", sock, "kill-server").Run() })
	run := func(args ...string) string {
		out, err := exec.Command("tmux", append([]string{"-L", sock, "-f", "/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	session := func(name, agent, role string) {
		args := []string{"new-session", "-d", "-s", name, "-x", "100", "-y", "30", "-e", "TAILTERM_HUB=http://hub.test", "-e", "TAILTERM_TASK=tsk_0000000000000001",
			"-e", "TAILTERM_AGENT=" + agent, "-e", "TAILTERM_RUN=run_0000000000000001"}
		if role != "" {
			args = append(args, "-e", "TAILTERM_ROLE="+role)
		}
		run(append(args, "sleep 60")...)
		run("set-option", "-w", "-t", name+":", "window-size", "latest")
	}
	session("owner", "agt_0000000000000001", api.AgentRoleOwnerHelper)
	session("agent", "agt_0000000000000002", "")
	changed, err := reconcileAgentWindowSizes(context.Background(), startupTmux, func(string, ...any) {})
	if err != nil || changed != 1 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
	size := func(s string) string {
		return run("display-message", "-p", "-t", s+":", "#{window_width}x#{window_height} #{window-size}")
	}
	if got := size("owner"); got != "100x30 latest" {
		t.Fatalf("owner helper window changed: %q", got)
	}
	if got := size("agent"); got != "200x50 manual" {
		t.Fatalf("agent window not reconciled: %q", got)
	}
}
