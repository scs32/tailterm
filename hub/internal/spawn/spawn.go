// Package spawn creates and controls agent tmux sessions on the local host.
package spawn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Env names shared by tt and tailterm.
const (
	EnvHub       = "TAILTERM_HUB"
	EnvTask      = "TAILTERM_TASK"
	EnvAgent     = "TAILTERM_AGENT"
	EnvAgentName = "TAILTERM_AGENT_NAME"
	EnvSession   = "TAILTERM_SESSION"
	EnvGoFlags   = "GOFLAGS"
	EnvArtifacts = "TAILTERM_ARTIFACTS"
	AgentWindow  = "agent"
	WatchWindow  = "tt-watch"
)

// Agent windows keep a fixed size whatever attaches to them. With tmux's
// default `window-size latest`, a tiny viewer (a hidden browser tile) shrinks
// the agent's window until its UI cannot render or submit input.
const (
	AgentWindowCols = 200
	AgentWindowRows = 50
	// Below this a pane is too small for an agent UI to render.
	MinUsableCols = 80
	MinUsableRows = 24
)

// AgentDefaultSize is the tmux default-size value for agent sessions.
var AgentDefaultSize = fmt.Sprintf("%dx%d", AgentWindowCols, AgentWindowRows)

var Tmux = "tmux"

// tmux builds a tmux command, honouring TT_TMUX_SOCKET (a -L socket name) so
// tests and experiments can use a private tmux server.
func tmux(args ...string) *exec.Cmd {
	if sock := os.Getenv("TT_TMUX_SOCKET"); sock != "" {
		args = append([]string{"-L", sock}, args...)
	}
	return exec.Command(Tmux, args...)
}

// ShellQuote returns s as a single-quoted POSIX shell word.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Version returns the tmux major.minor version, e.g. 3.5, or an error.
func Version() (float64, error) {
	out, err := tmux("-V").Output()
	if err != nil {
		return 0, fmt.Errorf("tmux not available: %w", err)
	}
	return ParseVersion(string(out))
}

var versionRE = regexp.MustCompile(`(\d+)\.(\d+)`)

func ParseVersion(s string) (float64, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("unrecognized tmux version %q", strings.TrimSpace(s))
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return float64(major) + float64(minor)/100, nil
}

// HasSession reports whether a tmux session with exactly this name exists.
func HasSession(name string) bool {
	return tmux("has-session", "-t", "="+name).Run() == nil
}

// ProbeSession distinguishes a confirmed missing session from a tmux probe
// failure. Activity monitoring must not turn a permission or socket error into
// a crash report.
func ProbeSession(name string) (bool, error) {
	if name == "" {
		return false, errors.New("tmux session name is missing")
	}
	out, err := tmux("has-session", "-t", "="+name).CombinedOutput()
	if err == nil {
		return true, nil
	}
	message := strings.ToLower(string(out))
	if strings.Contains(message, "can't find session") || strings.Contains(message, "no server running") || (strings.Contains(message, "failed to connect to server") && strings.Contains(message, "no such file or directory")) {
		return false, nil
	}
	return false, fmt.Errorf("tmux session probe: %w", err)
}

// UniqueSession returns name, or name with a short suffix when taken.
func UniqueSession(name string) string {
	if !HasSession(name) {
		return name
	}
	for i := 2; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d", name, i)
		if !HasSession(candidate) {
			return candidate
		}
	}
	return fmt.Sprintf("%s-%d", name, time.Now().Unix())
}

// Options describe a new agent session.
type Options struct {
	Session string
	Cwd     string
	Env     map[string]string
	// Command runs in the agent window; it is passed to the user's shell.
	Command string
	// Self is the path of the tt binary used for wrap and watch.
	Self string
}

// Create starts a detached tmux session with an agent window running the
// command through `tt wrap`. Messages are retrieved from the inbox.
func Create(o Options) error {
	v, err := Version()
	if err != nil {
		return err
	}
	if v < 3.02 {
		return fmt.Errorf("tmux %.2f is too old; 3.2 or newer is required for per-session environment", v)
	}
	args := []string{"new-session", "-d", "-s", o.Session, "-n", AgentWindow, "-x", strconv.Itoa(AgentWindowCols), "-y", strconv.Itoa(AgentWindowRows)}
	if o.Cwd != "" {
		args = append(args, "-c", o.Cwd)
	}
	for k, val := range o.Env {
		if k == EnvGoFlags || k == EnvArtifacts {
			continue
		}
		// The full briefing is already present in the private one-shot command
		// file below. Repeating it as a tmux -e argument can exceed tmux's
		// command limit and is unnecessary after the model process starts.
		if !forwardSessionEnv(o.Command, k, val) {
			continue
		}
		args = append(args, "-e", k+"="+val)
	}
	// Agents build in many worktree paths. Without -trimpath, Go keys each
	// package's cache entries on its directory, so every path fills the shared
	// build cache again (docs/go-build-cache.md).
	args = append(args, "-e", EnvGoFlags+"="+sessionGoFlags(o.Env))
	// An agent working in a linked worktree cannot tell its own checkout from
	// the main one, so it is told the artifacts root the sweep will use.
	if root := sessionArtifactsRoot(o.Env, o.Cwd); root != "" {
		args = append(args, "-e", EnvArtifacts+"="+root)
	}
	launch, err := os.CreateTemp("", ".tailterm-agent-command-*")
	if err != nil {
		return fmt.Errorf("create private agent command: %w", err)
	}
	launchPath := launch.Name()
	removeLaunch := true
	defer func() {
		if removeLaunch {
			_ = os.Remove(launchPath)
		}
	}()
	if err = launch.Chmod(0600); err == nil {
		_, err = launch.WriteString(o.Command)
	}
	closeErr := launch.Close()
	if err != nil {
		return fmt.Errorf("write private agent command: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close private agent command: %w", closeErr)
	}
	agentCmd := ShellQuote(o.Self) + " wrap --shell-file " + ShellQuote(filepath.Clean(launchPath))
	args = append(args, agentCmd)
	// Set the size policy in the same tmux command so no viewer can attach
	// between creation and policy. The exact =S: target names the new session
	// and its window even when tt runs inside another tmux pane ($TMUX would
	// otherwise pick the caller's session); a bare =S fails inside the chain.
	// Global options stay untouched.
	target := "=" + o.Session + ":"
	args = append(args, ";", "set-option", "-t", target, "default-size", AgentDefaultSize, ";", "set-option", "-w", "-t", target, "window-size", "manual")
	if out, err := tmux(args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %s", strings.TrimSpace(string(out)))
	}
	// The wrapper owns deletion after tmux accepts the session. It may not have
	// opened the file yet when new-session returns.
	removeLaunch = false

	return nil
}

// sessionGoFlags returns the session's GOFLAGS: the caller's value (the host's
// when the caller sets none) plus -trimpath, unless it already names
// -trimpath; -trimpath=false opts out.
func sessionGoFlags(env map[string]string) string {
	base, ok := env[EnvGoFlags]
	if !ok {
		base = os.Getenv(EnvGoFlags)
	}
	base = strings.TrimSpace(base)
	for _, f := range strings.Fields(base) {
		name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(f, "-"), "-"), "=")
		if name == "trimpath" {
			return base
		}
	}
	return strings.TrimSpace(base + " -trimpath")
}

// sessionArtifactsRoot returns the session's TAILTERM_ARTIFACTS: the caller's
// value, else the host's, else <main checkout>-artifacts for the repository
// at cwd (the launcher's directory when cwd is empty), the rule the worktree
// sweep uses. A relative value names no tree to the sweep, so it is not
// exported and nothing replaces it. "" when there is nothing to export: no
// repository, a bare main, or a Git failure, which never fails the launch.
func sessionArtifactsRoot(env map[string]string, cwd string) string {
	dir, ok := env[EnvArtifacts]
	if !ok || dir == "" {
		dir = os.Getenv(EnvArtifacts)
	}
	if dir != "" {
		if filepath.IsAbs(dir) {
			return dir
		}
		return ""
	}
	if main := mainCheckout(cwd); main != "" {
		return main + "-artifacts"
	}
	return ""
}

// mainCheckout names the main checkout of the repository at dir: the first
// entry of git worktree list, symlinks resolved, or "" when dir is in no
// repository, the main is bare, or Git fails or takes too long.
func mainCheckout(dir string) string {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		dir = wd
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "worktree", "list", "--porcelain", "-z").Output()
	if err != nil {
		return ""
	}
	main := ""
	for i, field := range strings.Split(string(out), "\x00") {
		if field == "" {
			break
		}
		key, value, _ := strings.Cut(field, " ")
		if i == 0 {
			if key != "worktree" || !filepath.IsAbs(value) {
				return ""
			}
			main = value
		} else if key == "bare" {
			return ""
		}
	}
	if main == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(main); err == nil {
		main = real
	}
	return filepath.Clean(main)
}

func forwardSessionEnv(command, key, value string) bool {
	return key != "TAILTERM_BRIEFING" || value == "" || !strings.Contains(command, ShellQuote(value))
}

// Kill terminates a session.
func Kill(session string) error {
	out, err := tmux("kill-session", "-t", "="+session).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux kill-session: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Host returns this machine's tailnet name when Tailscale is present,
// otherwise the short hostname.
func Host() string {
	if out, err := exec.Command("tailscale", "status", "--self", "--json").Output(); err == nil {
		var st struct {
			Self struct {
				DNSName  string `json:"DNSName"`
				HostName string `json:"HostName"`
			} `json:"Self"`
		}
		if json.Unmarshal(out, &st) == nil {
			if st.Self.DNSName != "" {
				return strings.TrimSuffix(strings.SplitN(st.Self.DNSName, ".", 2)[0], ".")
			}
			if st.Self.HostName != "" {
				return st.Self.HostName
			}
		}
	}
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return strings.SplitN(h, ".", 2)[0]
}

// Runtimes lists known agent CLIs available on PATH.
func Runtimes() []string {
	known := []string{"claude", "codex", "aider", "gemini", "opencode", "goose", "cursor-agent", "amp", "copilot"}
	out := []string{}
	for _, name := range known {
		if _, err := exec.LookPath(name); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// wrapStopGrace bounds how long the command's process group may outlive a
// forwarded stop signal before the whole group gets SIGKILL.
var wrapStopGrace = 5 * time.Second

// wrapKillWait bounds the wait for the group to end after that SIGKILL.
var wrapKillWait = 2 * time.Second

// Wrap runs a shell command, reporting start and exit through the callbacks,
// then keeps the pane alive with an interactive shell so the session persists.
//
// The command leads its own process group (pgid = the onStart pid). When the
// wrapper's group is the terminal's foreground group, the command's group takes
// the foreground, so pane input and a pane Ctrl-C reach the command alone. A
// SIGTERM or SIGINT addressed to the wrapper is forwarded to the group once; a
// member still alive wrapStopGrace later gets SIGKILL with the whole group.
// onExit then runs after the group has ended, and the wrapper ends by the
// signal it received, with no login shell. SIGHUP keeps its default, so a
// closed pane ends the wrapper without onExit, as before. A command that exits
// on its own is reported at once, leaving any remaining children alone.
func Wrap(command string, onStart func(pid int), onExit func(code int)) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd, cleanup, err := privateShellCommand(shell, command)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	tty := int(os.Stdin.Fd())
	pgrp, err := ttyForegroundGroup(tty)
	foreground := err == nil && pgrp == syscall.Getpgrp()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: foreground, Ctty: tty}
	// Registered before Start, so no stop signal arrives while it would still
	// end the wrapper without reaching the command.
	stops := make(chan os.Signal, 4)
	signal.Notify(stops, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stops)
	err = cmd.Start()
	if err != nil {
		return err
	}
	pgid := cmd.Process.Pid
	onStart(pgid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var stop syscall.Signal
	select {
	case err = <-done:
		signal.Stop(stops)
		signal.Reset(syscall.SIGTERM, syscall.SIGINT)
	case sig := <-stops:
		stop = sig.(syscall.Signal)
		err = stopGroup(pgid, stop, done)
	}
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		code = 127
	}
	if foreground {
		reclaimForeground(tty)
	}
	onExit(code)
	if stop != 0 {
		cleanup()
		signal.Stop(stops)
		signal.Reset(stop)
		_ = syscall.Kill(os.Getpid(), stop)
		time.Sleep(time.Second)
		return fmt.Errorf("wrapper survived its own %v", stop)
	}
	fmt.Fprintf(os.Stderr, "\n[tt] agent command exited with status %d; this pane is now a shell.\n", code)
	login := exec.Command(shell, "-l")
	login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stdout, os.Stderr
	return login.Run()
}

// stopGroup forwards sig to the command's process group once and waits until
// the command's Wait has returned and the group has no members. A member still
// alive wrapStopGrace after the forward gets SIGKILL with the whole group, and
// the wait gives up wrapKillWait after that. Further stop signals stay queued
// on the wrapper's channel and are not forwarded again. It returns the
// command's Wait result.
func stopGroup(pgid int, sig syscall.Signal, done <-chan error) error {
	_ = syscall.Kill(-pgid, sig)
	var waitErr error
	exited, killed := false, false
	deadline := time.Now().Add(wrapStopGrace)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if !exited {
			select {
			case waitErr = <-done:
				exited = true
			default:
			}
		}
		if exited && !groupAlive(pgid) {
			return waitErr
		}
		if time.Now().After(deadline) {
			if killed {
				if !exited {
					waitErr = <-done
				}
				return waitErr
			}
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			killed = true
			deadline = time.Now().Add(wrapKillWait)
		}
		<-tick.C
	}
}

// groupAlive reports whether process group pgid still has a member; a member
// the wrapper may not signal still counts.
func groupAlive(pgid int) bool {
	return !errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

// reclaimForeground makes the wrapper's group the terminal's foreground group
// again, so the login shell it starts next can read the pane. A background
// group changing the foreground would be stopped by SIGTTOU, so it is ignored
// for the call. signal.Reset would keep it ignored, and an exec'd child would
// inherit that; Notify then Stop leaves the runtime's handler installed
// instead, which a child's exec resets to the default action.
func reclaimForeground(tty int) {
	signal.Ignore(syscall.SIGTTOU)
	_ = setTTYForegroundGroup(tty, syscall.Getpgrp())
	ttou := make(chan os.Signal, 1)
	signal.Notify(ttou, syscall.SIGTTOU)
	signal.Stop(ttou)
}

// ttyForegroundGroup returns the foreground process group of terminal fd.
func ttyForegroundGroup(fd int) (int, error) {
	var pgrp int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGPGRP), uintptr(unsafe.Pointer(&pgrp))); errno != 0 {
		return 0, errno
	}
	return int(pgrp), nil
}

// setTTYForegroundGroup makes pgrp the foreground process group of terminal fd.
func setTTYForegroundGroup(fd, pgrp int) error {
	group := int32(pgrp)
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCSPGRP), uintptr(unsafe.Pointer(&group))); errno != 0 {
		return errno
	}
	return nil
}

// ReadJSON reads a JSON object from r into a map, tolerating empty input.
func ReadJSON(b []byte) map[string]any {
	out := map[string]any{}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}

// Execute the complete command from a private file, not a shell -c argument.
// The shell opens it before its first instruction removes the directory entry;
// terminal stdin stays attached to the runtime, and failed starts also clean up.
func privateShellCommand(shell, command string) (*exec.Cmd, func(), error) {
	file, err := os.CreateTemp("", ".tailterm-runtime-command-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	if err = file.Chmod(0600); err == nil {
		_, err = file.WriteString("/bin/rm -f " + ShellQuote(file.Name()) + "\n" + command + "\n")
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return exec.Command(shell, file.Name()), cleanup, nil
}
