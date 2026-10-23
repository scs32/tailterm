package spawn

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain runs the package's tests with TMUX_TMPDIR pointing at a directory
// only this test process owns, and removes it afterwards, on pass and on
// failure. tmux leaves a socket file behind when its server stops, so every
// private -L server a test started used to leave one in the shared per-user
// directory (wi_3493188f14edd3e3). Nothing is removed by name or age, and the
// shared directory and its default server are never touched.
//
// The directory sits directly under /tmp rather than os.TempDir(): a unix
// socket path holds about 104 bytes, and the macOS per-user temporary
// directory would use half of that before the socket name.
//
// The TestContextArgvFixture and TestWrapFixture children start no tmux, so
// they get no directory to leave behind.
func TestMain(m *testing.M) {
	if os.Getenv("TT_SYNTHETIC_CONTEXT_ARGV") == "1" || os.Getenv("TT_WRAP_FIXTURE") == "1" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("/tmp", "tt-tmux-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "private tmux directory:", err)
		os.Exit(1)
	}
	os.Setenv("TMUX_TMPDIR", dir)
	code := m.Run()
	// Stop any server a test left running; it would be unreachable once its
	// socket is gone.
	sockets, _ := filepath.Glob(filepath.Join(dir, "tmux-*", "*"))
	for _, socket := range sockets {
		_ = exec.Command("tmux", "-S", socket, "kill-server").Run()
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "remove private tmux directory:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

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

// gitFixture runs git in dir with no user or system configuration.
func gitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// sessionEnv reads one variable of a session's own environment; ok is false
// when the session has none.
func sessionEnv(t *testing.T, session, key string) (string, bool) {
	t.Helper()
	out, err := tmux("show-environment", "-t", "="+session, key).CombinedOutput()
	got := strings.TrimSpace(string(out))
	if err != nil {
		if strings.Contains(got, "unknown variable") {
			return "", false
		}
		t.Fatalf("tmux show-environment %s: %v: %s", key, err, got)
	}
	value, ok := strings.CutPrefix(got, key+"=")
	if !ok {
		return "", false
	}
	return value, true
}

func TestSpawnCreateExportsArtifactsRoot(t *testing.T) {
	privateTmux(t)
	t.Setenv(EnvArtifacts, "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(root, "repo")
	if err := os.Mkdir(main, 0700); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, main, "init", "-q")
	gitFixture(t, main, "commit", "-q", "--allow-empty", "-m", "base")
	queue := filepath.Join(main, ".build", "worktrees", "queue-1")
	gitFixture(t, main, "worktree", "add", "-q", "--detach", queue)
	bare := filepath.Join(root, "bare.git")
	gitFixture(t, root, "clone", "-q", "--bare", main, bare)
	bareLinked := filepath.Join(root, "bare-linked")
	gitFixture(t, bare, "worktree", "add", "-q", "--detach", bareLinked)
	outside := t.TempDir()
	self := sleeperSelf(t)
	create := func(session, cwd string, env map[string]string) {
		t.Helper()
		if err := Create(Options{Session: session, Cwd: cwd, Env: env, Command: "true", Self: self}); err != nil {
			t.Fatalf("%s: %v", session, err)
		}
	}
	want := func(session, value string) {
		t.Helper()
		got, ok := sessionEnv(t, session, EnvArtifacts)
		if value == "" && ok {
			t.Fatalf("%s: %s = %q, want none", session, EnvArtifacts, got)
		}
		if value != "" && (!ok || got != value) {
			t.Fatalf("%s: %s = %q (set %v), want %q", session, EnvArtifacts, got, ok, value)
		}
	}

	// A queue worktree's agent gets its main checkout's sibling, not its own.
	create("queue", queue, nil)
	want("queue", main+"-artifacts")
	create("main", main, nil)
	want("main", main+"-artifacts")
	// Outside a repository, or with a bare main, there is nothing to name.
	create("outside", outside, nil)
	want("outside", "")
	create("bare", bareLinked, nil)
	want("bare", "")

	// An absolute value from the caller, else from the host, is kept.
	create("caller", queue, map[string]string{EnvArtifacts: "/srv/artifacts"})
	want("caller", "/srv/artifacts")
	t.Setenv(EnvArtifacts, "/host/artifacts")
	create("host", queue, nil)
	want("host", "/host/artifacts")
	create("caller-over-host", queue, map[string]string{EnvArtifacts: "/srv/artifacts"})
	want("caller-over-host", "/srv/artifacts")

	// A relative value names no tree: it is not exported and no computed root
	// replaces it.
	create("caller-relative", queue, map[string]string{EnvArtifacts: "relative/dir"})
	want("caller-relative", "")
	t.Setenv(EnvArtifacts, "relative/dir")
	create("host-relative", queue, nil)
	want("host-relative", "")
}

// TestWrapFixture is the wrapper under test when the test binary is re-run
// with TT_WRAP_FIXTURE=1: it runs Wrap on TT_WRAP_COMMAND with a 300 ms stop
// grace and appends what the wrapper saw to TT_WRAP_RECORD: each process
// table read as "snapshot", then the survivors and the exit onExit received.
func TestWrapFixture(t *testing.T) {
	if os.Getenv("TT_WRAP_FIXTURE") != "1" {
		return
	}
	wrapStopGrace = 300 * time.Millisecond
	record := func(format string, args ...any) {
		f, err := os.OpenFile(os.Getenv("TT_WRAP_RECORD"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err == nil {
			fmt.Fprintf(f, format+"\n", args...)
			f.Close()
		}
	}
	record("wrapper %d", os.Getpid())
	read := readProcesses
	readProcesses = func() (map[int]procInfo, error) {
		record("snapshot")
		return read()
	}
	pgid := 0
	err := Wrap(os.Getenv("TT_WRAP_COMMAND"), func(pid int) {
		pgid = pid
		record("start %d", pid)
	}, func(code int, survivors []Survivor) {
		names := []string{}
		for _, s := range survivors {
			names = append(names, fmt.Sprintf("%d:%s", s.PID, s.Command))
		}
		if len(names) == 0 {
			names = append(names, "none")
		}
		record("survivors %s", strings.Join(names, ","))
		record("exit %d alive=%v", code, groupAlive(pgid))
	})
	record("return %v", err)
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// wrapFixture writes the agent script and a fake $SHELL, and returns the
// record path and the fixture's environment, which runs the script in the
// command's own shell. Called as a login shell, the fake records the SIGTTOU
// disposition the login process inherited (perl run by exec, since sh resets
// it in the children it forks) and "login", then exits, or with interactive
// becomes /bin/sh -l.
func wrapFixture(t *testing.T, script string, interactive bool) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	rec := filepath.Join(dir, "record")
	agent := filepath.Join(dir, "agent.sh")
	if err := os.WriteFile(agent, []byte(strings.ReplaceAll(script, "REC", ShellQuote(rec))), 0600); err != nil {
		t.Fatal(err)
	}
	login, again := "exit 0", "''"
	if interactive {
		login, again = "exec /bin/sh -l", "1"
	}
	fake := `#!/bin/sh
if [ "$1" = -l ]; then
	if command -v perl >/dev/null; then
		exec perl -e 'open(my $f, ">>", $ARGV[0]) or die; print $f "ttou ", ($SIG{TTOU} || "DEFAULT"), "\nlogin\n"; close $f; exec("/bin/sh", "-l") if $ARGV[1]; exit 0' REC ` + again + `
	fi
	echo login >> REC
	` + login + `
fi
exec /bin/sh "$@"
`
	shell := filepath.Join(dir, "shell")
	if err := os.WriteFile(shell, []byte(strings.ReplaceAll(fake, "REC", ShellQuote(rec))), 0700); err != nil {
		t.Fatal(err)
	}
	return rec, []string{"TT_WRAP_FIXTURE=1", "SHELL=" + shell, "HOME=" + dir, "TT_WRAP_RECORD=" + rec, "TT_WRAP_COMMAND=. " + ShellQuote(agent)}
}

// loginTTOUDefault fails unless the login shell recorded the default SIGTTOU
// action: an ignored one would pass to every job the shell starts.
func loginTTOUDefault(t *testing.T, rec string) {
	t.Helper()
	if _, err := exec.LookPath("perl"); err != nil {
		t.Log("perl not available; login shell SIGTTOU not checked")
		return
	}
	if got := strings.Join(recordLine(t, rec, "ttou", 10*time.Second), " "); got != "DEFAULT" {
		t.Fatalf("login shell SIGTTOU disposition %q, want DEFAULT", got)
	}
}

// recordLine waits for the first record line starting with prefix and returns
// its remaining fields.
func recordLine(t *testing.T, rec, prefix string, wait time.Duration) []string {
	t.Helper()
	for deadline := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
		data, _ := os.ReadFile(rec)
		for _, line := range strings.Split(string(data), "\n") {
			if line == prefix || strings.HasPrefix(line, prefix+" ") {
				return strings.Fields(strings.TrimPrefix(line, prefix))
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %q record within %v:\n%s", prefix, wait, data)
		}
	}
}

func readRecord(rec string) string {
	data, _ := os.ReadFile(rec)
	return string(data)
}

func recordCount(t *testing.T, rec, line string) int {
	t.Helper()
	n := 0
	for _, l := range strings.Split(readRecord(rec), "\n") {
		if l == line {
			n++
		}
	}
	return n
}

func recordPID(t *testing.T, rec, prefix string) int {
	t.Helper()
	pid, err := strconv.Atoi(recordLine(t, rec, prefix, 10*time.Second)[0])
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// pidGone reports whether pid has ended: it no longer exists, or the pid now
// belongs to a process whose command does not contain want.
func pidGone(pid int, want string) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	out, err := exec.Command("ps", "-o", "stat=,command=", "-p", strconv.Itoa(pid)).Output()
	text := strings.TrimSpace(string(out))
	return err != nil || !strings.Contains(text, want) || strings.HasPrefix(text, "Z")
}

func waitGone(t *testing.T, pid int, want string, wait time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(wait); !pidGone(pid, want); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d (%s) still running after %v", pid, want, wait)
		}
	}
}

// killOnCleanup kills each recorded pid that still runs want when the test
// ends, so a failure leaves none of the fixture's processes behind.
func killOnCleanup(t *testing.T, rec string, prefixes map[string]string) {
	t.Cleanup(func() {
		data, _ := os.ReadFile(rec)
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if want, ok := prefixes[fields[0]]; ok {
				if pid, err := strconv.Atoi(fields[1]); err == nil && pid > 1 && !pidGone(pid, want) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
	})
}

func TestWrapForwardsStopSignalToCommandGroup(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sig  syscall.Signal
		name string
		code string
	}{{syscall.SIGTERM, "TERM", "143"}, {syscall.SIGINT, "INT", "130"}} {
		t.Run(tc.name, func(t *testing.T) {
			// The child ignores the signal (a background child also ignores
			// INT), so only the group SIGKILL after the grace ends it.
			rec, env := wrapFixture(t, `trap 'echo "trap TERM" >> REC; exit 143' TERM
trap 'echo "trap INT" >> REC; exit 130' INT
echo "agent $$" >> REC
sh -c 'trap "" TERM INT; echo "child $$" >> '"REC"'; while :; do sleep 1; done' &
wait
`, false)
			killOnCleanup(t, rec, map[string]string{"wrapper": "TestWrapFixture", "child": "sh -c"})
			cmd := exec.Command(self, "-test.run=^TestWrapFixture$")
			cmd.Env = append(os.Environ(), env...)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			agent := recordPID(t, rec, "agent")
			child := recordPID(t, rec, "child")
			if got := recordPID(t, rec, "start"); got != agent {
				t.Fatalf("onStart pid %d, agent shell %d", got, agent)
			}
			if pgid, err := syscall.Getpgid(agent); err != nil || pgid != agent {
				t.Fatalf("agent pgid %d (%v), want its own group %d", pgid, err, agent)
			}
			if pgid, err := syscall.Getpgid(child); err != nil || pgid != agent {
				t.Fatalf("child pgid %d (%v), want the agent group %d", pgid, err, agent)
			}
			sent := time.Now()
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("wrapper did not end after its stop signal")
			}
			elapsed := time.Since(sent)
			var exit *exec.ExitError
			status, ok := syscall.WaitStatus(0), false
			if errors.As(err, &exit) {
				status, ok = exit.Sys().(syscall.WaitStatus)
			}
			if !ok || !status.Signaled() || status.Signal() != tc.sig {
				t.Fatalf("wrapper ended %v, want killed by %v", err, tc.sig)
			}
			if n := recordCount(t, rec, "trap "+tc.name); n != 1 {
				t.Fatalf("agent received %s %d times, want once", tc.name, n)
			}
			if got := strings.Join(recordLine(t, rec, "exit", 0), " "); got != tc.code+" alive=false" {
				t.Fatalf("onExit record %q, want %s with the group ended", got, tc.code)
			}
			if !pidGone(child, "sh -c") {
				t.Fatalf("child %d outlived onExit", child)
			}
			if got := strings.Join(recordLine(t, rec, "survivors", 0), " "); got != "none" || recordCount(t, rec, "snapshot") != 2 {
				t.Fatalf("survivors %q after %d process table reads, want none after 2", got, recordCount(t, rec, "snapshot"))
			}
			if recordCount(t, rec, "login") != 0 || recordCount(t, rec, "return <nil>") != 0 {
				t.Fatal("stopped wrapper started the login shell")
			}
			// The child can only end by the SIGKILL that follows the grace.
			if limit := 300*time.Millisecond + wrapKillWait + 2*time.Second; elapsed < 300*time.Millisecond || elapsed > limit {
				t.Fatalf("wrapper ended %v after the signal, want between the grace and %v", elapsed, limit)
			}
		})
	}
}

func TestWrapNormalExitKeepsLoginShell(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rec, env := wrapFixture(t, `sleep 30 &
echo "child $!" >> REC
exit 7
`, false)
	killOnCleanup(t, rec, map[string]string{"child": "sleep 30"})
	cmd := exec.Command(self, "-test.run=^TestWrapFixture$")
	cmd.Env = append(os.Environ(), env...)
	// A file, not a pipe: the child keeps the wrapper's stderr open.
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	cmd.Stderr = stderr
	err = cmd.Run()
	notice, _ := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatalf("wrapper: %v\n%s", err, notice)
	}
	child := recordPID(t, rec, "child")
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(readRecord(rec)), "\n") {
		if !strings.HasPrefix(line, "ttou ") {
			lines = append(lines, line)
		}
	}
	want := []string{"exit 7 alive=true", "login", "return <nil>"}
	if len(lines) < 3 || strings.Join(lines[len(lines)-3:], "|") != strings.Join(want, "|") {
		t.Fatalf("record ends %q, want %q", lines, want)
	}
	loginTTOUDefault(t, rec)
	if pidGone(child, "sleep 30") {
		t.Fatal("normal exit killed the command's remaining child")
	}
	// A command that exits on its own reads no process table and reports no
	// survivors, even with its child still running.
	if got := strings.Join(recordLine(t, rec, "survivors", 0), " "); got != "none" || recordCount(t, rec, "snapshot") != 0 {
		t.Fatalf("normal exit reported survivors %q after %d process table reads, want none after 0", got, recordCount(t, rec, "snapshot"))
	}
	if !strings.Contains(string(notice), "[tt] agent command exited with status 7; this pane is now a shell.") {
		t.Fatalf("stderr notice missing: %q", notice)
	}
}

// TestWrapSessionChildFixture is an agent's child that leaves the command's
// group when the test binary is re-run with TT_WRAP_SESSION_CHILD=1: it starts
// its own session, records "session PID", records any stop or hangup signal
// it receives as "session got SIG", and ends by itself after a minute.
func TestWrapSessionChildFixture(t *testing.T) {
	if os.Getenv("TT_WRAP_SESSION_CHILD") != "1" {
		return
	}
	record := func(format string, args ...any) {
		f, err := os.OpenFile(os.Getenv("TT_WRAP_RECORD"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err == nil {
			fmt.Fprintf(f, format+"\n", args...)
			f.Close()
		}
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	if _, err := syscall.Setsid(); err != nil {
		record("session setsid %v", err)
		os.Exit(1)
	}
	record("session %d", os.Getpid())
	select {
	case sig := <-sigs:
		record("session got %v", sig)
	case <-time.After(time.Minute):
	}
	os.Exit(0)
}

// k4: a stop ends the command's group, and the agent's child in its own
// session outlives it unsignalled and is reported by pid and name.
func TestWrapStopReportsSessionChildWithoutSignallingIt(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The group child ignores TERM, so only the group SIGKILL ends it.
	rec, env := wrapFixture(t, `trap 'exit 143' TERM
echo "agent $$" >> REC
sh -c 'trap "" TERM INT; echo "child $$" >> '"REC"'; while :; do sleep 1; done' &
TT_WRAP_SESSION_CHILD=1 "$TT_WRAP_SELF" -test.run='^TestWrapSessionChildFixture$' &
wait
`, false)
	killOnCleanup(t, rec, map[string]string{"wrapper": "TestWrapFixture", "child": "sh -c", "session": "TestWrapSessionChildFixture"})
	cmd := exec.Command(self, "-test.run=^TestWrapFixture$")
	cmd.Env = append(os.Environ(), append(env, "TT_WRAP_SELF="+self)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	agent := recordPID(t, rec, "agent")
	child := recordPID(t, rec, "child")
	session := recordPID(t, rec, "session")
	if sid, err := getsid(session); err != nil || sid != session {
		t.Fatalf("session child sid %d (%v), want its own session %d", sid, err, session)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("wrapper did not end after its stop signal")
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
		t.Fatalf("wrapper ended %v, want killed by SIGTERM", err)
	}
	if got := strings.Join(recordLine(t, rec, "exit", 0), " "); got != "143 alive=false" {
		t.Fatalf("onExit record %q, want 143 with the group ended", got)
	}
	if !pidGone(agent, ".tailterm-runtime-command-") || !pidGone(child, "sh -c") {
		t.Fatalf("group member outlived the stop: agent gone %v, child gone %v", pidGone(agent, ".tailterm-runtime-command-"), pidGone(child, "sh -c"))
	}
	want := fmt.Sprintf("%d:%s", session, filepath.Base(self))
	if got := strings.Join(recordLine(t, rec, "survivors", 0), " "); got != want {
		t.Fatalf("survivors %q, want only the session child %q", got, want)
	}
	if n := recordCount(t, rec, "snapshot"); n != 2 {
		t.Fatalf("%d process table reads, want one before the stop and one after", n)
	}
	// Never signalled: still running after the wrapper ended, and nothing
	// recorded by its handler.
	time.Sleep(200 * time.Millisecond)
	if pidGone(session, "TestWrapSessionChildFixture") {
		t.Fatal("the stop ended the session child")
	}
	if strings.Contains(readRecord(rec), "session got") {
		t.Fatalf("the session child was signalled:\n%s", readRecord(rec))
	}
}

// getsid returns the session ID of pid.
func getsid(pid int) (int, error) {
	sid, _, errno := syscall.RawSyscall(syscall.SYS_GETSID, uintptr(pid), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(sid), nil
}

// k1: descendants follow parent links into other groups and sessions, and a
// survivor needs the same pid and start time after the stop.
func TestWrapSurvivorsMatchStartTimeAcrossSessions(t *testing.T) {
	at := func(s string) string { return "Thu Oct 23 14:06:" + s + " 2026" }
	table := map[int]procInfo{
		1:   {1, 0, at("00"), "/sbin/launchd"},
		100: {100, 1, at("01"), "/bin/sh"},           // the command, pgid 100
		101: {101, 100, at("02"), "/bin/sleep"},      // group member
		102: {102, 100, at("02"), "/usr/bin/claude"}, // runtime
		200: {200, 102, at("03"), "/bin/zsh"},        // tool shell, own session
		201: {201, 200, at("04"), "/usr/bin/git"},    // its child
		300: {300, 1, at("05"), "/bin/zsh"},          // unrelated
	}
	got := descendants(table, 100)
	if len(got) != 4 {
		t.Fatalf("descendants %v, want 101 102 200 201", got)
	}
	for _, pid := range []int{101, 102, 200, 201} {
		if _, ok := got[pid]; !ok {
			t.Fatalf("descendant %d missing from %v", pid, got)
		}
	}
	// After the stop: the group is gone, 200 survived reparented, 201's pid
	// was reused by a newer process, and 300 never was a descendant.
	after := map[int]procInfo{
		1:   table[1],
		200: {200, 1, at("03"), "/bin/zsh"},
		201: {201, 1, at("09"), "/bin/cat"},
		300: table[300],
	}
	if s := survivorsOf(got, after); len(s) != 1 || s[0] != (Survivor{PID: 200, Command: "zsh"}) {
		t.Fatalf("survivors %+v, want only pid 200 zsh", s)
	}
	if s := survivorsOf(got, nil); len(s) != 0 {
		t.Fatalf("survivors %+v with every process ended", s)
	}
	// The walk stops at maxDescendants, and names are base names only,
	// printable and bounded.
	wide := map[int]procInfo{10: {10, 1, at("00"), "sh"}}
	for pid := 11; pid < 11+maxDescendants+50; pid++ {
		wide[pid] = procInfo{pid, 10, at("00"), "sleep"}
	}
	if n := len(descendants(wide, 10)); n != maxDescendants {
		t.Fatalf("%d descendants, want the cap %d", n, maxDescendants)
	}
	if name := survivorName("/opt/x/" + strings.Repeat("é", 40) + "\x07"); len(name) > maxSurvivorName || strings.ContainsAny(name, "/\x07") {
		t.Fatalf("survivor name %q", name)
	}
}

func TestWrapTerminalForegroundCtrlCAndKillSession(t *testing.T) {
	run := privateTmux(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pane := func(session string, env []string) {
		words := []string{"exec", "env"}
		for _, kv := range env {
			words = append(words, ShellQuote(kv))
		}
		words = append(words, ShellQuote(self), "-test.run=^TestWrapFixture$")
		run("-f", "/dev/null", "new-session", "-d", "-x", "80", "-y", "24", "-s", session, strings.Join(words, " "))
	}

	// The agent reads the pane, counts INT and ends on HUP; its background
	// child is in the same group.
	rec, env := wrapFixture(t, `trap 'echo "trap INT" >> REC' INT
trap 'echo "trap HUP" >> REC; exit 129' HUP
sleep 60 &
echo "child $!" >> REC
set -- $(ps -o pgid=,tpgid= -p $$)
echo "agent $$ $1 $2" >> REC
read line
echo "read $line" >> REC
while :; do sleep 0.2; done
`, false)
	killOnCleanup(t, rec, map[string]string{"wrapper": "TestWrapFixture", "agent": ".tailterm-runtime-command-", "child": "sleep 60"})
	pane("stop", env)
	wrapper := recordPID(t, rec, "wrapper")
	child := recordPID(t, rec, "child")
	ids := recordLine(t, rec, "agent", 10*time.Second)
	if len(ids) != 3 || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("agent pid, pgid, tpgid = %q, want the command's group in the foreground", ids)
	}
	agent, _ := strconv.Atoi(ids[0])
	run("send-keys", "-t", "=stop:", "pane input", "Enter")
	recordLine(t, rec, "read pane input", 10*time.Second)

	run("send-keys", "-t", "=stop:", "C-c")
	recordLine(t, rec, "trap INT", 10*time.Second)
	// A forwarded copy would arrive at once and start the 300 ms stop.
	time.Sleep(time.Second)
	if n := recordCount(t, rec, "trap INT"); n != 1 {
		t.Fatalf("pane Ctrl-C reached the agent %d times, want once", n)
	}
	if pidGone(agent, ".tailterm-runtime-command-") || strings.Contains(readRecord(rec), "\nexit ") {
		t.Fatal("pane Ctrl-C stopped the wrapper's command")
	}

	run("kill-session", "-t", "=stop")
	waitGone(t, wrapper, "TestWrapFixture", 5*time.Second)
	waitGone(t, agent, ".tailterm-runtime-command-", 5*time.Second)
	waitGone(t, child, "sleep 60", 5*time.Second)
	for _, line := range strings.Split(readRecord(rec), "\n") {
		if strings.HasPrefix(line, "exit ") || strings.HasPrefix(line, "return ") {
			t.Fatalf("kill-session reached onExit or the login shell: %q", line)
		}
	}

	// A command that exits on its own hands the pane back to a login shell
	// that can read it.
	rec, env = wrapFixture(t, `exit 3
`, true)
	killOnCleanup(t, rec, map[string]string{"wrapper": "TestWrapFixture"})
	pane("login", env)
	recordLine(t, rec, "exit 3", 10*time.Second)
	loginTTOUDefault(t, rec)
	run("send-keys", "-t", "=login:", "echo login-read >> "+ShellQuote(rec), "Enter")
	recordLine(t, rec, "login-read", 10*time.Second)
	run("send-keys", "-t", "=login:", "exit", "Enter")
	recordLine(t, rec, "return <nil>", 10*time.Second)
}
