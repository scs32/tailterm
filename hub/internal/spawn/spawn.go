// Package spawn creates and controls agent tmux sessions on the local host.
package spawn

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Env names shared by tt and tailterm.
const (
	EnvHub       = "TAILTERM_HUB"
	EnvTask      = "TAILTERM_TASK"
	EnvAgent     = "TAILTERM_AGENT"
	EnvAgentName = "TAILTERM_AGENT_NAME"
	EnvSession   = "TAILTERM_SESSION"
	AgentWindow  = "agent"
	WatchWindow  = "tt-watch"
)

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
	args := []string{"new-session", "-d", "-s", o.Session, "-n", AgentWindow}
	if o.Cwd != "" {
		args = append(args, "-c", o.Cwd)
	}
	for k, val := range o.Env {
		args = append(args, "-e", k+"="+val)
	}
	agentCmd := ShellQuote(o.Self) + " wrap --shell " + ShellQuote(o.Command)
	args = append(args, agentCmd)
	if out, err := tmux(args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %s", strings.TrimSpace(string(out)))
	}

	return nil
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

// Wrap runs a shell command, reporting start and exit through the callbacks,
// then keeps the pane alive with an interactive shell so the session persists.
func Wrap(command string, onStart func(), onExit func(code int)) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	onStart()
	cmd := exec.Command(shell, "-c", command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		code = 127
	}
	onExit(code)
	fmt.Fprintf(os.Stderr, "\n[tt] agent command exited with status %d; this pane is now a shell.\n", code)
	login := exec.Command(shell, "-l")
	login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stdout, os.Stderr
	return login.Run()
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
