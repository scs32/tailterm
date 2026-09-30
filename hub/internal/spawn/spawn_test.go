package spawn

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProbeSessionDistinguishesAbsenceFromProbeFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake-tmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$TT_PROBE_MESSAGE\" >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	previous := Tmux
	Tmux = path
	defer func() { Tmux = previous }()
	t.Setenv("TT_PROBE_MESSAGE", "can't find session: synthetic")
	if alive, err := ProbeSession("synthetic"); err != nil || alive {
		t.Fatalf("absent session alive=%v err=%v", alive, err)
	}
	t.Setenv("TT_PROBE_MESSAGE", "permission denied")
	if alive, err := ProbeSession("synthetic"); err == nil || alive {
		t.Fatalf("failed probe alive=%v err=%v", alive, err)
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]float64{"tmux 3.5a": 3.05, "tmux 3.2": 3.02, "tmux next-3.6": 3.06, "tmux 2.9a": 2.09} {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Errorf("%q: got %v %v, want %v", in, got, err, want)
		}
	}
	if _, err := ParseVersion("garbage"); err == nil {
		t.Error("expected error for garbage")
	}
}

func TestShellQuote(t *testing.T) {
	if got := ShellQuote(`it's "here"`); got != `'it'\''s "here"'` {
		t.Errorf("got %s", got)
	}
}

func TestReadJSON(t *testing.T) {
	if m := ReadJSON([]byte(`{"a":1}`)); m["a"] != float64(1) {
		t.Errorf("got %v", m)
	}
	if m := ReadJSON(nil); len(m) != 0 {
		t.Errorf("expected empty map, got %v", m)
	}
}

func TestBriefingEnvironmentIsKeptOnlyWhenItIsNotEmbedded(t *testing.T) {
	briefing := "exact task briefing with 'quotes'"
	if !forwardSessionEnv("generic-script", "TAILTERM_BRIEFING", briefing) {
		t.Fatal("generic runtime lost its briefing environment")
	}
	if forwardSessionEnv("codex "+ShellQuote(briefing), "TAILTERM_BRIEFING", briefing) {
		t.Fatal("embedded model briefing was duplicated in the tmux environment")
	}
	if !forwardSessionEnv("codex", "TAILTERM_WORK_ITEM", "wi_0123456789abcdef") {
		t.Fatal("unrelated session environment was suppressed")
	}
}

func TestPrivateShellCommandCompleteContextAndCleanup(t *testing.T) {
	// Synthetic quote amplification exceeded shell -c ARG_MAX before the fix.
	payload := strings.Repeat("'", 512*1024)
	command, cleanup, err := privateShellCommand("/bin/sh", "printf '%s' "+ShellQuote(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	script := command.Args[1]
	stat, err := os.Stat(script)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("private permissions: %v", err)
	}
	if len(command.Args) != 2 || !strings.HasPrefix(filepath.Base(script), ".tailterm-runtime-command-") {
		t.Fatal("large command remained in argv")
	}
	got, err := command.Output()
	if err != nil || string(got) != payload {
		t.Fatalf("complete context changed: %v", err)
	}
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Fatalf("script retained: %v", err)
	}
	failed, cleanup, err := privateShellCommand("/absent-synthetic-shell", "exit 0")
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.Run(); err == nil {
		t.Fatal("missing shell succeeded")
	}
	cleanup()
	if _, err := os.Stat(failed.Args[1]); !os.IsNotExist(err) {
		t.Fatal("failed-start script retained")
	}
}

func TestContextReleaseHostArgv(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, character := range []string{"'", "界"} {
		payload := strings.Repeat(character, 524288/len(character)) + strings.Repeat("x", 524288%len(character))
		command, cleanup, err := privateShellCommand("/bin/sh", ShellQuote(self)+" -test.run=^TestContextArgvFixture$ -- "+ShellQuote(payload))
		if err != nil {
			t.Fatal(err)
		}
		command.Env = []string{"PATH=/usr/bin:/bin", "TT_SYNTHETIC_CONTEXT_ARGV=1"}
		output, err := command.Output()
		cleanup()
		if err != nil {
			t.Fatalf("actual host runtime argv does not support complete512KiB: %v", err)
		}
		want := fmt.Sprintf("%x", sha256.Sum256([]byte(payload)))
		if string(output) != want {
			t.Fatal("external synthetic runtime received changed context")
		}
	}
}

func TestContextArgvFixture(t *testing.T) {
	if os.Getenv("TT_SYNTHETIC_CONTEXT_ARGV") != "1" {
		return
	}
	fmt.Printf("%x", sha256.Sum256([]byte(os.Args[len(os.Args)-1])))
	os.Exit(0)
}

// privateTmux points spawn at a private tmux server for one test and kills it
// afterwards. It skips when tmux is absent.
func privateTmux(t *testing.T) func(args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	sock := fmt.Sprintf("tt-size-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Setenv("TT_TMUX_SOCKET", sock)
	run := func(args ...string) string {
		out, err := tmux(args...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
		_ = exec.Command("tmux", "-L", sock+"-viewer", "kill-server").Run()
	})
	return run
}

// sleeperSelf stands in for the tt binary: `Self wrap --shell-file F` keeps
// the agent pane alive without running a model.
func sleeperSelf(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tt")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// attachTinyViewer attaches a real tmux client sized cols x rows to session,
// from a pane of that size on a second private server, like a hidden TailOS
// tile, and waits until the client is registered.
func attachTinyViewer(t *testing.T, run func(...string) string, session string, cols, rows int) {
	t.Helper()
	sock := os.Getenv("TT_TMUX_SOCKET")
	attach := fmt.Sprintf("env -u TMUX tmux -L %s attach-session -t %s", ShellQuote(sock), ShellQuote("="+session))
	out, err := exec.Command("tmux", "-L", sock+"-viewer", "-f", "/dev/null", "new-session", "-d", "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows), attach).CombinedOutput()
	if err != nil {
		t.Fatalf("viewer: %v: %s", err, out)
	}
	want := fmt.Sprintf("%dx%d %s", cols, rows, session)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		for _, line := range strings.Split(run("list-clients", "-F", "#{client_width}x#{client_height} #{session_name}"), "\n") {
			if line == want {
				time.Sleep(100 * time.Millisecond) // let tmux apply the client's size
				return
			}
		}
	}
	t.Fatalf("tiny viewer never attached to %s: %s", session, run("list-clients", "-F", "#{client_width}x#{client_height} #{session_name}"))
}

func windowSize(run func(...string) string, session string) string {
	return run("display-message", "-p", "-t", "="+session+":"+AgentWindow, "#{window_width}x#{window_height} #{window-size}")
}

func createAgent(t *testing.T, name string) {
	t.Helper()
	if err := Create(Options{Session: name, Cwd: t.TempDir(), Command: "true", Self: sleeperSelf(t)}); err != nil {
		t.Fatal(err)
	}
}

func TestSpawnCreateSetsManualWindowSize(t *testing.T) {
	run := privateTmux(t)
	createAgent(t, "agent-a")
	if got := windowSize(run, "agent-a"); got != "200x50 manual" {
		t.Fatalf("agent window = %q, want 200x50 manual", got)
	}
	if got := run("show-options", "-t", "agent-a", "default-size"); got != "default-size 200x50" {
		t.Fatalf("session default-size = %q", got)
	}
	// The policy is per session: globals and a later human session keep tmux's defaults.
	run("new-session", "-d", "-s", "human", "sleep 60")
	if got := run("show-options", "-gw", "window-size"); got == "window-size manual" {
		t.Fatalf("global window-size changed: %q", got)
	}
	if got := run("show-options", "-g", "default-size"); got == "default-size 200x50" {
		t.Fatalf("global default-size changed: %q", got)
	}
	if got := run("show-options", "-w", "-t", "human:", "window-size"); got != "" {
		t.Fatalf("human window inherited a local window-size: %q", got)
	}
}

func TestTinyClientAttachKeepsAgentWindowSize(t *testing.T) {
	run := privateTmux(t)
	createAgent(t, "agent-a")
	// Control: a session created the pre-fix way shrinks behind the same viewer.
	run("new-session", "-d", "-s", "prefix", "-n", AgentWindow, "-x", "200", "-y", "50", "sleep 60")
	run("set-option", "-w", "-t", "=prefix:", "window-size", "latest")
	attachTinyViewer(t, run, "prefix", 16, 2)
	if got := windowSize(run, "prefix"); got != "16x1 latest" {
		t.Fatalf("control window = %q, want the reproduced 16x1 latest", got)
	}
	attachTinyViewer(t, run, "agent-a", 16, 2)
	if got := windowSize(run, "agent-a"); got != "200x50 manual" {
		t.Fatalf("agent window behind a 16x2 viewer = %q, want 200x50 manual", got)
	}
}

func TestWindowSizePolicyAfterTmuxServerRestart(t *testing.T) {
	run := privateTmux(t)
	createAgent(t, "agent-a")
	if err := tmux("kill-server").Run(); err != nil {
		t.Fatal(err)
	}
	if HasSession("agent-a") {
		t.Fatal("tmux server survived kill-server")
	}
	// A fresh server has only tmux defaults; the policy comes from Create alone.
	createAgent(t, "agent-b")
	if got := run("show-options", "-gw", "window-size"); got == "window-size manual" {
		t.Fatalf("test relied on a global window-size: %q", got)
	}
	attachTinyViewer(t, run, "agent-b", 16, 2)
	if got := windowSize(run, "agent-b"); got != "200x50 manual" {
		t.Fatalf("agent window after server restart = %q, want 200x50 manual", got)
	}
}

// tt spawn usually runs inside another agent's tmux pane on the same server.
// The size policy must land on the new agent session only, never on the
// caller's session that $TMUX and $TMUX_PANE name.
func TestSpawnFromInsideTmuxTargetsOnlyTheAgent(t *testing.T) {
	run := privateTmux(t)
	run("new-session", "-d", "-s", "caller", "-x", "100", "-y", "30", "sleep 60")
	socket := run("display-message", "-p", "-t", "caller:", "#{socket_path}")
	pid := run("display-message", "-p", "-t", "caller:", "#{pid}")
	sessionID := strings.TrimPrefix(run("display-message", "-p", "-t", "caller:", "#{session_id}"), "$")
	t.Setenv("TMUX", socket+","+pid+","+sessionID)
	t.Setenv("TMUX_PANE", run("display-message", "-p", "-t", "caller:", "#{pane_id}"))
	callerSession, callerWindow := run("show-options", "-t", "caller"), run("show-options", "-w", "-t", "caller:")
	createAgent(t, "agent-a")
	if got := run("show-options", "-t", "caller"); got != callerSession {
		t.Fatalf("caller session options changed:\nbefore %q\nafter %q", callerSession, got)
	}
	if got := run("show-options", "-w", "-t", "caller:"); got != callerWindow || strings.Contains(got, "window-size") {
		t.Fatalf("caller window options changed:\nbefore %q\nafter %q", callerWindow, got)
	}
	if got := run("show-options", "-t", "agent-a", "default-size"); got != "default-size 200x50" {
		t.Fatalf("agent default-size = %q", got)
	}
	if got := windowSize(run, "agent-a"); got != "200x50 manual" {
		t.Fatalf("agent window = %q, want 200x50 manual", got)
	}
	attachTinyViewer(t, run, "agent-a", 16, 2)
	if got := windowSize(run, "agent-a"); got != "200x50 manual" {
		t.Fatalf("agent window behind a 16x2 viewer = %q, want 200x50 manual", got)
	}
}

func TestSessionGoFlagsAddsTrimpath(t *testing.T) {
	cases := []struct {
		name string
		host string
		env  map[string]string
		want string
	}{
		{"no key and empty host", "", nil, "-trimpath"},
		{"host value merged when key absent", "-mod=mod", map[string]string{}, "-mod=mod -trimpath"},
		{"key merged", "", map[string]string{"GOFLAGS": "-mod=mod"}, "-mod=mod -trimpath"},
		{"key wins over host", "-race", map[string]string{"GOFLAGS": "-mod=mod"}, "-mod=mod -trimpath"},
		{"empty key ignores host", "-race", map[string]string{"GOFLAGS": ""}, "-trimpath"},
		{"already set", "", map[string]string{"GOFLAGS": "-mod=mod -trimpath"}, "-mod=mod -trimpath"},
		{"double dash", "", map[string]string{"GOFLAGS": "--trimpath"}, "--trimpath"},
		{"explicit opt-out", "", map[string]string{"GOFLAGS": "-trimpath=false"}, "-trimpath=false"},
		{"host opt-out", "--trimpath=false", nil, "--trimpath=false"},
		{"host already set", "-trimpath", nil, "-trimpath"},
		{"extra spaces", "", map[string]string{"GOFLAGS": "  -mod=mod  "}, "-mod=mod -trimpath"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The test process may itself run under a spawned session's GOFLAGS.
			t.Setenv("GOFLAGS", c.host)
			if got := sessionGoFlags(c.env); got != c.want {
				t.Fatalf("sessionGoFlags(%v) with host %q = %q, want %q", c.env, c.host, got, c.want)
			}
		})
	}
}

func TestSpawnCreateSetsTrimpathGoFlags(t *testing.T) {
	run := privateTmux(t)
	t.Setenv("GOFLAGS", "")
	env := map[string]string{"GOFLAGS": "-mod=mod", EnvAgentName: "agent-a"}
	if err := Create(Options{Session: "agent-a", Cwd: t.TempDir(), Env: env, Command: "true", Self: sleeperSelf(t)}); err != nil {
		t.Fatal(err)
	}
	if got := run("show-environment", "-t", "=agent-a", "GOFLAGS"); got != "GOFLAGS=-mod=mod -trimpath" {
		t.Fatalf("session GOFLAGS = %q, want GOFLAGS=-mod=mod -trimpath", got)
	}
	if got := run("show-environment", "-t", "=agent-a", EnvAgentName); got != EnvAgentName+"=agent-a" {
		t.Fatalf("session %s = %q", EnvAgentName, got)
	}
	if env["GOFLAGS"] != "-mod=mod" {
		t.Fatalf("Create changed the caller's Env: GOFLAGS = %q", env["GOFLAGS"])
	}
	// A session with no GOFLAGS of its own still gets -trimpath.
	createAgent(t, "agent-b")
	if got := run("show-environment", "-t", "=agent-b", "GOFLAGS"); got != "GOFLAGS=-trimpath" {
		t.Fatalf("session GOFLAGS = %q, want GOFLAGS=-trimpath", got)
	}
}
