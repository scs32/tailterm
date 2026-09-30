package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/adapters"
	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// tt doctor --role checks this machine against one role in
// docs/host-requirements.md. It is read-only: it runs version and login
// status commands, reads files and prints one line per requirement. Command
// output is discarded; only parsed versions, exit codes and fixed text reach
// the report, so no credential or private value is printed.

const doctorRoleUsage = `usage: tt doctor [--role client|agent|test|release] [--repo DIR] [--deploy-config PATH]

With no arguments, tt doctor runs the original tmux, runtime and hub checks.
With --role it checks this machine against docs/host-requirements.md, prints
one line per requirement (ok, missing, wrong version or warn) with a fix hint,
and exits 1 when any required piece is missing or the wrong version.

Roles:
  client   tt, hub configuration and reachability (TailOS or tt read-only)
  agent    client + tmux 3.2, ~/.local/bin/tt, claude and codex with logins, hooks, inbox relay
  test     client + Go, Node, npm, python3, Playwright browsers, the four fixtures, free disk
  release  client + gh login, Node, wrangler and its credential, the truenas SSH route, deployer config

Flags:
  --role ROLE           the role to check (required with any other flag)
  --repo DIR            the checkout for the test role (default: git top level of the working directory)
  --deploy-config PATH  the deployer's private config, checked by the release role (read-only)
`

var doctorRoles = []string{"client", "agent", "test", "release"}

const (
	doctorOK      = "ok"
	doctorMissing = "missing"
	doctorWrong   = "wrong version"
	doctorWarn    = "warn"
)

// Versions the repository requires. Tests pin both to package-lock.json.
const (
	// package-lock.json packages["node_modules/vite"].engines.node, the
	// strictest non-optional engine among the test dependencies.
	doctorTestNodeRange = "^20.19.0 || >=22.12.0"
	// vite (build:static) intersected with wrangler's engines ">=22.0.0".
	doctorReleaseNodeRange = ">=22.12.0"
	doctorMinFreeDiskMiB   = 8192
	doctorRelayLabel       = "com.tailterm.inbox-relay"
)

type doctorResult struct{ status, detail, fix string }

type doctorCheck struct {
	name     string
	required bool
	run      func(d *doctorRun) doctorResult
	// skip leaves the line out, for checks that only apply with a flag.
	skip func(d *doctorRun) bool
}

// hostProbe is everything doctor reads from the machine. Tests replace it.
type hostProbe struct {
	goos, home, repo, deployConfig string
	uid                            int
	env                            func(string) string
	lookPath                       func(string) (string, error)
	// run executes a command with extra environment and returns its exit
	// status and combined output. The output is parsed, never printed.
	run       func(ctx context.Context, dir string, extraEnv []string, name string, args ...string) (int, []byte, error)
	readFile  func(string) ([]byte, error)
	stat      func(string) (fs.FileInfo, error)
	glob      func(string) ([]string, error)
	statfsMiB func(string) (int64, error)
	whoami    func() error
	codexHook func() bool
	now       func() time.Time
	timeout   time.Duration
}

func cmdDoctorRole(e env, args []string) error {
	if len(args) == 0 {
		return cmdDoctor(e)
	}
	if args[0] == "help" {
		fmt.Print(doctorRoleUsage)
		return nil
	}
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	role := flags.String("role", "", "")
	repo := flags.String("repo", "", "")
	deployConfig := flags.String("deploy-config", "", "")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(doctorRoleUsage)
			return nil
		}
		return doctorUsageError(err.Error())
	}
	if flags.NArg() > 0 {
		return doctorUsageError("unexpected argument " + strconv.Quote(flags.Arg(0)))
	}
	if *role == "" {
		return doctorUsageError("--role is required with other flags")
	}
	if !doctorKnownRole(*role) {
		return doctorUsageError("unknown role " + strconv.Quote(*role))
	}
	p := newHostProbe(e)
	p.deployConfig = *deployConfig
	p.repo = *repo
	if p.repo == "" && *role == "test" {
		p.repo = gitTopLevel(p)
	}
	return runDoctorRole(os.Stdout, *role, p)
}

func doctorUsageError(msg string) error {
	return &exitError{code: 2, err: fmt.Errorf("doctor: %s; roles: %s (tt doctor --help)", msg, strings.Join(doctorRoles, ", "))}
}

func doctorKnownRole(role string) bool {
	for _, r := range doctorRoles {
		if r == role {
			return true
		}
	}
	return false
}

func newHostProbe(e env) hostProbe {
	home, _ := os.UserHomeDir()
	return hostProbe{
		goos:      runtime.GOOS,
		home:      home,
		uid:       os.Getuid(),
		env:       os.Getenv,
		lookPath:  exec.LookPath,
		run:       execDoctorCommand(nil, exec.LookPath, 2*time.Second),
		readFile:  os.ReadFile,
		stat:      os.Stat,
		glob:      filepath.Glob,
		statfsMiB: statfsFreeMiB,
		whoami: func() error {
			c, err := e.client(5 * time.Second)
			if err != nil {
				return err
			}
			ctx, cancel := ctxTimeout(5 * time.Second)
			defer cancel()
			_, err = c.Whoami(ctx)
			return err
		},
		codexHook: codexStopHookInstalled,
		now:       time.Now,
		timeout:   10 * time.Second,
	}
}

// execDoctorCommand runs name, resolved with lookPath, with base (nil: this
// process's environment) plus extra. stdin is empty and output is captured.
func execDoctorCommand(base []string, lookPath func(string) (string, error), waitDelay time.Duration) func(context.Context, string, []string, string, ...string) (int, []byte, error) {
	return func(ctx context.Context, dir string, extra []string, name string, args ...string) (int, []byte, error) {
		path, err := lookPath(name)
		if err != nil {
			return -1, nil, err
		}
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Dir = dir
		environ := base
		if environ == nil {
			environ = os.Environ()
		}
		cmd.Env = append(append([]string{}, environ...), extra...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		cmd.WaitDelay = waitDelay
		err = cmd.Run()
		if ctx.Err() != nil {
			return -1, out.Bytes(), ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), out.Bytes(), nil
		}
		if err != nil {
			return -1, out.Bytes(), err
		}
		return 0, out.Bytes(), nil
	}
}

func gitTopLevel(p hostProbe) string {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()
	code, out, err := p.run(ctx, "", nil, "git", "rev-parse", "--show-toplevel")
	if err != nil || code != 0 {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// doctorRun holds one role run: the probe and values several checks share.
type doctorRun struct {
	p    hostProbe
	memo map[string]any
}

type toolRun struct {
	found    bool
	code     int
	out      []byte
	timedOut bool
	err      error
}

// command runs a tool once per doctor run and remembers the outcome.
func (d *doctorRun) command(dir string, extra []string, name string, args ...string) toolRun {
	key := "cmd:" + dir + "\x00" + strings.Join(extra, "\x00") + "\x00" + name + "\x00" + strings.Join(args, "\x00")
	if v, ok := d.memo[key]; ok {
		return v.(toolRun)
	}
	var r toolRun
	if _, err := d.p.lookPath(name); err == nil {
		r.found = true
		ctx, cancel := context.WithTimeout(context.Background(), d.p.timeout)
		r.code, r.out, r.err = d.p.run(ctx, dir, extra, name, args...)
		r.timedOut = errors.Is(r.err, context.DeadlineExceeded) || ctx.Err() != nil
		cancel()
	}
	d.memo[key] = r
	return r
}

func (d *doctorRun) home(parts ...string) string {
	return filepath.Join(append([]string{d.p.home}, parts...)...)
}

func (d *doctorRun) inRepo(parts ...string) string {
	return filepath.Join(append([]string{d.p.repo}, parts...)...)
}

func runDoctorRole(w io.Writer, role string, p hostProbe) error {
	if !doctorKnownRole(role) {
		return doctorUsageError("unknown role " + strconv.Quote(role))
	}
	d := &doctorRun{p: p, memo: map[string]any{}}
	failed := 0
	for _, c := range doctorChecks(role) {
		if c.skip != nil && c.skip(d) {
			continue
		}
		r := c.run(d)
		// An optional check never fails the role; a required one warns only
		// for the SSH Match case.
		if !c.required && r.status != doctorOK {
			r.status = doctorWarn
		}
		if c.required && (r.status == doctorMissing || r.status == doctorWrong) {
			failed++
		}
		fmt.Fprintf(w, "%-13s  %-34s  %s\n", r.status, c.name, r.detail)
		if r.status != doctorOK && r.fix != "" {
			fmt.Fprintf(w, "%-13s  fix: %s\n", "", r.fix)
		}
	}
	if failed == 0 {
		fmt.Fprintf(w, "role %s: ok\n", role)
		return nil
	}
	fmt.Fprintf(w, "role %s: %d required missing\n", role, failed)
	return &exitError{code: 1, err: fmt.Errorf("doctor: role %s: %d required missing", role, failed)}
}

func doctorChecks(role string) []doctorCheck {
	checks := clientChecks()
	switch role {
	case "agent":
		checks = append(checks, agentChecks()...)
	case "test":
		checks = append(checks, testChecks()...)
	case "release":
		checks = append(checks, releaseChecks()...)
	}
	return checks
}

func checkOK(detail string) doctorResult { return doctorResult{status: doctorOK, detail: detail} }
func checkMissing(detail, fix string) doctorResult {
	return doctorResult{status: doctorMissing, detail: detail, fix: fix}
}
func checkWrong(detail, fix string) doctorResult {
	return doctorResult{status: doctorWrong, detail: detail, fix: fix}
}

func doctorOnPath(name, fix string) doctorCheck {
	return doctorCheck{name: name, required: true, run: func(d *doctorRun) doctorResult {
		if path, err := d.p.lookPath(name); err == nil {
			return checkOK(path)
		}
		return checkMissing("not on PATH", fix)
	}}
}

// ---- client ----

func clientChecks() []doctorCheck {
	hubJSON := func(d *doctorRun) string { return d.home(".config", "tailterm", "hub.json") }
	return []doctorCheck{
		doctorOnPath("tt", "install tt to ~/.local/bin/tt (docs/host-requirements.md)"),
		{name: "hub configured", required: true, run: func(d *doctorRun) doctorResult {
			if d.p.env(spawn.EnvHub) != "" {
				return checkOK("TAILTERM_HUB set")
			}
			b, err := d.p.readFile(hubJSON(d))
			if err != nil {
				return checkMissing("no TAILTERM_HUB and no ~/.config/tailterm/hub.json", "write ~/.config/tailterm/hub.json with url and token, mode 600")
			}
			var config struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(b, &config) != nil || config.URL == "" {
				return checkMissing("~/.config/tailterm/hub.json has no url", "write ~/.config/tailterm/hub.json with url and token, mode 600")
			}
			return checkOK("~/.config/tailterm/hub.json")
		}},
		{name: "hub.json private", required: true, run: func(d *doctorRun) doctorResult {
			info, err := d.p.stat(hubJSON(d))
			if err != nil {
				return checkOK("not present")
			}
			if info.Mode().Perm()&0o044 != 0 {
				return checkMissing(fmt.Sprintf("mode %04o is group/other readable", info.Mode().Perm()), "chmod 600 ~/.config/tailterm/hub.json")
			}
			return checkOK(fmt.Sprintf("mode %04o", info.Mode().Perm()))
		}},
		{name: "hub reachable", required: true, run: func(d *doctorRun) doctorResult {
			err := d.p.whoami()
			if err == nil {
				return checkOK("whoami answered")
			}
			var httpErr *api.HTTPError
			if errors.As(err, &httpErr) {
				return checkMissing(fmt.Sprintf("HTTP %d", httpErr.Status), "check the hub URL and token in ~/.config/tailterm/hub.json")
			}
			return checkMissing("unreachable", "check the hub URL and the tailnet route to the hub")
		}},
		{name: "tailscale CLI", required: false, run: func(d *doctorRun) doctorResult {
			if path, err := d.p.lookPath("tailscale"); err == nil {
				return checkOK(path)
			}
			return doctorResult{status: doctorWarn, detail: "not on PATH; optional (host name falls back to the OS host name)", fix: "install the Tailscale CLI"}
		}},
	}
}

// ---- agent ----

var tmuxVersionRE = regexp.MustCompile(`\d+\.\d+[a-z]?`)

// tmuxVersion reports tmux's version text and whether it is 3.2 or newer.
func (d *doctorRun) tmuxVersion() (text string, recent bool, res doctorResult, found bool) {
	r := d.command("", nil, spawn.Tmux, "-V")
	switch {
	case !r.found:
		return "", false, checkMissing("not on PATH", "install tmux 3.2 or newer (brew install tmux; apt install tmux)"), false
	case r.timedOut || r.code != 0:
		return "", false, checkMissing("tmux -V failed", "reinstall tmux 3.2 or newer"), false
	}
	v, err := spawn.ParseVersion(string(r.out))
	if err != nil {
		return "", false, checkMissing("unrecognized tmux version", "install tmux 3.2 or newer"), false
	}
	text = tmuxVersionRE.FindString(string(r.out))
	return text, v >= 3.02, doctorResult{}, true
}

func tmuxCheck() doctorCheck {
	return doctorCheck{name: "tmux >= 3.2", required: true, run: func(d *doctorRun) doctorResult {
		text, recent, res, found := d.tmuxVersion()
		if !found {
			return res
		}
		if !recent {
			return checkWrong("tmux "+text+" (need 3.2 or newer)", "upgrade tmux to 3.2 or newer")
		}
		return checkOK("tmux " + text)
	}}
}

func agentChecks() []doctorCheck {
	return []doctorCheck{
		tmuxCheck(),
		// docs/team-launch.md "Agent window size": tt spawn applies
		// default-size and window-size manual per session and TailOS attaches
		// with -f ignore-size. All need tmux 3.2; no global option is needed.
		{name: "tmux sizing policy", required: true, run: func(d *doctorRun) doctorResult {
			text, recent, res, found := d.tmuxVersion()
			if !found {
				return res
			}
			if !recent {
				return checkWrong("tmux "+text+" lacks default-size, window-size manual and attach -f ignore-size", "upgrade tmux to 3.2 or newer")
			}
			return checkOK("applied per session by tt spawn; no global option needed")
		}},
		{name: "tt in ~/.local/bin", required: true, run: func(d *doctorRun) doctorResult {
			info, err := d.p.stat(d.home(".local", "bin", "tt"))
			if err != nil || !info.Mode().IsRegular() {
				return checkMissing("~/.local/bin/tt not found", "install tt to ~/.local/bin/tt (docs/host-requirements.md)")
			}
			if info.Mode().Perm()&0o111 == 0 {
				return checkMissing("~/.local/bin/tt is not executable", "chmod +x ~/.local/bin/tt")
			}
			return checkOK("~/.local/bin/tt")
		}},
		doctorOnPath("git", "install git"),
		doctorOnPath("claude", "install Claude Code (docs/host-requirements.md)"),
		loginCheck("claude login", "claude", []string{"auth", "status"}, "run claude and sign in (/login)"),
		doctorOnPath("codex", "install the Codex CLI (docs/host-requirements.md)"),
		loginCheck("codex login", "codex", []string{"login", "status"}, "run codex login"),
		{name: "codex Stop hook", required: true, run: func(d *doctorRun) doctorResult {
			if d.p.codexHook() {
				return checkOK("hooks.json has tt " + adapters.CodexStopMarker)
			}
			return checkMissing("the Tailterm Stop hook is not in ~/.codex/hooks.json", "tt hooks codex --install")
		}},
		{name: "claude hooks", required: false, run: claudeHooksCheck},
		{name: "relay service installed", required: true, run: func(d *doctorRun) doctorResult {
			if d.p.goos != "darwin" {
				return checkMissing("no "+d.p.goos+" relay service yet (to fix, see docs/host-requirements.md)", "run tt relay under the host's process supervisor")
			}
			if _, err := d.p.stat(d.home("Library", "LaunchAgents", doctorRelayLabel+".plist")); err != nil {
				return checkMissing("no ~/Library/LaunchAgents/"+doctorRelayLabel+".plist", "python3 scripts/install-relay-macos.py")
			}
			return checkOK("LaunchAgent " + doctorRelayLabel)
		}},
		{name: "relay running", required: true, run: relayRunningCheck},
	}
}

func loginCheck(name, tool string, args []string, fix string) doctorCheck {
	return doctorCheck{name: name, required: true, run: func(d *doctorRun) doctorResult {
		r := d.command("", nil, tool, args...)
		switch {
		case !r.found:
			return checkMissing(tool+" not installed", "install "+tool+" first")
		case r.timedOut:
			return checkMissing(tool+" "+strings.Join(args, " ")+" timed out", fix)
		case r.err != nil:
			return checkMissing(tool+" "+strings.Join(args, " ")+" did not run", fix)
		case r.code != 0:
			return checkMissing(fmt.Sprintf("%s %s exited %d (not logged in)", tool, strings.Join(args, " "), r.code), fix)
		}
		return checkOK("logged in (" + tool + " " + strings.Join(args, " ") + ")")
	}}
}

// claudeHooksCheck looks for the four hooks tt hooks claude prints
// (internal/adapters ClaudeHooks) in Claude's user settings files.
func claudeHooksCheck(d *doctorRun) doctorResult {
	dir := d.p.env("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = d.home(".claude")
	}
	events := map[string]string{"SessionStart": "hook session-start", "UserPromptSubmit": "hook prompt", "Stop": "hook stop", "Notification": "hook notification"}
	found := map[string]bool{}
	for _, name := range []string{"settings.json", "settings.local.json"} {
		b, err := d.p.readFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var doc struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if json.Unmarshal(b, &doc) != nil {
			continue
		}
		for event, suffix := range events {
			for _, entry := range doc.Hooks[event] {
				for _, h := range entry.Hooks {
					if strings.HasSuffix(strings.TrimSpace(h.Command), suffix) {
						found[event] = true
					}
				}
			}
		}
	}
	if len(found) == len(events) {
		return checkOK("SessionStart, UserPromptSubmit, Stop and Notification")
	}
	return doctorResult{status: doctorWarn, detail: "not installed; optional (docs/tasks.md)", fix: "tt hooks claude (add the printed hooks to ~/.claude/settings.json)"}
}

var launchdStateRE = regexp.MustCompile(`(?m)^\tstate = (.+)$`)

var launchdStates = map[string]bool{"not running": true, "waiting": true, "spawn scheduled": true, "spawning": true, "exited": true, "killed": true}

func relayRunningCheck(d *doctorRun) doctorResult {
	if d.p.goos != "darwin" {
		return checkMissing("no "+d.p.goos+" relay service yet (to fix, see docs/host-requirements.md)", "run tt relay under the host's process supervisor")
	}
	target := fmt.Sprintf("gui/%d/%s", d.p.uid, doctorRelayLabel)
	fix := "launchctl kickstart -k " + target + " (or python3 scripts/install-relay-macos.py)"
	r := d.command("", nil, "launchctl", "print", target)
	switch {
	case !r.found:
		return checkMissing("launchctl not on PATH", fix)
	case r.timedOut || r.err != nil:
		return checkMissing("launchctl print did not answer", fix)
	case r.code != 0:
		return checkMissing("not loaded", fix)
	}
	m := launchdStateRE.FindSubmatch(r.out)
	if m == nil {
		return checkMissing("loaded, state unknown", fix)
	}
	if state := strings.TrimSpace(string(m[1])); state != "running" {
		// Name only states launchd is known to print; other text stays out.
		if !launchdStates[state] {
			state = "unrecognized"
		}
		return checkMissing("loaded, not running (state "+state+")", fix)
	}
	return checkOK("running")
}

// doctorWord keeps parsed tool text short and printable.
func doctorWord(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, s)
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// ---- test ----

func testChecks() []doctorCheck {
	checks := []doctorCheck{
		doctorOnPath("git", "install git"),
		tmuxCheck(),
		doctorOnPath("python3", "install python3"),
		doctorOnPath("npm", "install Node.js (npm comes with it)"),
		{name: "node_modules installed", required: true, run: func(d *doctorRun) doctorResult {
			if d.p.repo == "" {
				return checkMissing("no repository (pass --repo)", "run from the checkout or pass --repo DIR")
			}
			if _, err := d.p.stat(d.inRepo("node_modules", ".package-lock.json")); err != nil {
				return checkMissing("node_modules/.package-lock.json not found", "npm ci")
			}
			return checkOK("node_modules/.package-lock.json")
		}},
		nodeCheck("node "+doctorTestNodeRange, doctorTestNodeRange),
		{name: "go toolchain", required: true, run: goCheck},
	}
	for _, f := range []struct{ path, fix string }{
		{"wasm/tailserve.wasm", "npm run build:wasm -- --test"},
		{".build/test.wasm", "npm run build:wasm -- --test"},
		{".build/go-modules.txt", "npm run build:wasm -- --test"},
		{".build/speech-fixture.wav", "curl -fL https://huggingface.co/datasets/Xenova/transformers.js-docs/resolve/main/jfk.wav -o .build/speech-fixture.wav"},
	} {
		f := f
		checks = append(checks, doctorCheck{name: "fixture " + f.path, required: true, run: func(d *doctorRun) doctorResult {
			if d.p.repo == "" {
				return checkMissing("no repository (pass --repo)", "run from the checkout or pass --repo DIR")
			}
			info, err := d.p.stat(d.inRepo(filepath.FromSlash(f.path)))
			if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				return checkMissing(f.path+" not found", f.fix)
			}
			return checkOK(f.path)
		}})
	}
	for _, b := range []string{"chromium", "chromium-headless-shell", "webkit"} {
		b := b
		checks = append(checks, doctorCheck{name: "playwright " + b, required: true, run: func(d *doctorRun) doctorResult { return playwrightCheck(d, b) }})
	}
	checks = append(checks, doctorCheck{name: "free disk >= 8 GiB", required: true, run: func(d *doctorRun) doctorResult {
		dir := d.p.repo
		if dir == "" {
			dir = d.p.home
		}
		free, err := d.p.statfsMiB(dir)
		if err != nil {
			return checkMissing("free space not observed", "check the checkout's filesystem")
		}
		if free < doctorMinFreeDiskMiB {
			return checkMissing(fmt.Sprintf("%d MiB free (need %d)", free, doctorMinFreeDiskMiB), "free disk space (docs/go-build-cache.md)")
		}
		return checkOK(fmt.Sprintf("%d MiB free", free))
	}})
	return checks
}

func (d *doctorRun) nodeVersion() (string, doctorResult, bool) {
	r := d.command("", nil, "node", "--version")
	switch {
	case !r.found:
		return "", checkMissing("node not on PATH", "install Node.js 22.12 or newer"), false
	case r.timedOut || r.err != nil || r.code != 0:
		return "", checkMissing("node --version failed", "reinstall Node.js"), false
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(r.out)), "v")
	parts, ok := parseSemver(v)
	if !ok {
		return "", checkMissing("unrecognized node version", "reinstall Node.js"), false
	}
	return semverText(parts), doctorResult{}, true
}

func nodeCheck(name, rng string) doctorCheck {
	return doctorCheck{name: name, required: true, run: func(d *doctorRun) doctorResult {
		v, res, found := d.nodeVersion()
		if !found {
			return res
		}
		if !semverSatisfies(v, rng) {
			return checkWrong("node "+v+" (need "+rng+")", "install a Node.js version in "+rng)
		}
		return checkOK("node " + v)
	}}
}

var (
	goModLineRE       = regexp.MustCompile(`(?m)^go\s+(\S+)\s*$`)
	goVersionOutputRE = regexp.MustCompile(`go(\d+\.\d+(?:\.\d+)?)`)
	goVersionRE       = regexp.MustCompile(`^\d+\.\d+(\.\d+)?((rc|beta)\d+)?$`)
)

// goCheck compares the installed Go with hub/go.mod under the user's
// GOTOOLCHAIN setting. Every go command runs with GOTOOLCHAIN=local outside
// the module, so doctor never selects or downloads a toolchain.
func goCheck(d *doctorRun) doctorResult {
	if d.p.repo == "" {
		return checkMissing("no repository (pass --repo)", "run from the checkout or pass --repo DIR")
	}
	b, err := d.p.readFile(d.inRepo("hub", "go.mod"))
	if err != nil {
		return checkMissing("hub/go.mod not found", "run from the checkout or pass --repo DIR")
	}
	m := goModLineRE.FindSubmatch(b)
	if m == nil {
		return checkMissing("hub/go.mod has no go line", "check hub/go.mod")
	}
	required := string(m[1])
	local := []string{"GOTOOLCHAIN=local"}
	r := d.command(d.p.home, local, "go", "version")
	switch {
	case !r.found:
		return checkMissing("go not on PATH", "install Go "+required+" or newer")
	case r.timedOut || r.err != nil || r.code != 0:
		return checkMissing("go version failed", "reinstall Go")
	}
	vm := goVersionOutputRE.FindSubmatch(r.out)
	if vm == nil {
		return checkMissing("unrecognized go version", "reinstall Go")
	}
	installed := string(vm[1])
	setting := d.goToolchainSetting(local)
	return goToolchainOutcome(setting, installed, required, func(name string) bool {
		_, err := d.p.lookPath(name)
		return err == nil
	})
}

// goToolchainSetting follows go's own order: the environment, the user's
// GOENV file, then $GOROOT/go.env; unset means auto.
func (d *doctorRun) goToolchainSetting(local []string) string {
	if v := strings.TrimSpace(d.p.env("GOTOOLCHAIN")); v != "" {
		return v
	}
	read := func(path string) string {
		b, err := d.p.readFile(path)
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "GOTOOLCHAIN="); ok {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	if r := d.command(d.p.home, local, "go", "env", "GOENV"); r.found && r.code == 0 && r.err == nil {
		if path := strings.TrimSpace(string(r.out)); path != "" && path != "off" {
			if v := read(path); v != "" {
				return v
			}
		}
	}
	if r := d.command(d.p.home, local, "go", "env", "GOROOT"); r.found && r.code == 0 && r.err == nil {
		if root := strings.TrimSpace(string(r.out)); root != "" {
			if v := read(filepath.Join(root, "go.env")); v != "" {
				return v
			}
		}
	}
	return "auto"
}

func goToolchainOutcome(setting, installed, required string, onPath func(string) bool) doctorResult {
	have := "go " + installed + ", go.mod " + required + ", GOTOOLCHAIN=" + doctorWord(setting)
	fix := "install Go " + required + " or newer, or set GOTOOLCHAIN=auto"
	base, mode, _ := strings.Cut(setting, "+")
	if !strings.Contains(setting, "+") {
		switch setting {
		case "auto":
			base, mode = "local", "auto"
		case "path":
			base, mode = "local", "path"
		}
	}
	pinned := ""
	switch {
	case base == "local":
	case strings.HasPrefix(base, "go") && goVersionValid(strings.TrimPrefix(base, "go")):
		pinned = strings.TrimPrefix(base, "go")
	default:
		return checkWrong(have+": unrecognized GOTOOLCHAIN", "set GOTOOLCHAIN to local, auto, path or goX.Y.Z")
	}
	atLeast := func(v string) bool { return compareGoVersion(v, required) >= 0 }
	switch mode {
	case "":
		if pinned == "" {
			if atLeast(installed) {
				return checkOK(have)
			}
			return checkWrong(have, fix)
		}
		if atLeast(pinned) {
			return checkOK(have + " (pinned go" + pinned + ")")
		}
		return checkWrong(have+" (pinned go"+pinned+")", "set GOTOOLCHAIN to go"+required+" or newer")
	case "auto":
		// go selects the newer of its default (installed or pinned) and
		// go.mod's line, downloading go.mod's when both are older.
		if atLeast(installed) || (pinned != "" && atLeast(pinned)) {
			return checkOK(have)
		}
		return checkOK(have + " (will download go" + required + ")")
	case "path":
		start := installed
		if pinned != "" && compareGoVersion(pinned, start) > 0 {
			start = pinned
		}
		if atLeast(start) || onPath("go"+required) {
			return checkOK(have)
		}
		return checkWrong(have+" (no go"+required+" on PATH)", fix)
	}
	return checkWrong(have+": unrecognized GOTOOLCHAIN", "set GOTOOLCHAIN to local, auto, path or goX.Y.Z")
}

func goVersionValid(v string) bool {
	return goVersionRE.MatchString(v)
}

// compareGoVersion orders Go versions such as 1.26, 1.26rc1 and 1.26.6.
func compareGoVersion(a, b string) int {
	pa, pb := goVersionParts(a), goVersionParts(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// goVersionParts orders 1.26rc1 before 1.26, which equals 1.26.0.
func goVersionParts(v string) [4]int {
	var out [4]int
	pre := 1 << 20 // a release sorts after its pre-releases
	if i := strings.IndexAny(v, "rb"); i >= 0 {
		n, _ := strconv.Atoi(strings.TrimLeft(v[i:], "rcbeta"))
		pre, v = n, v[:i]
	}
	for i, part := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(part)
	}
	out[3] = pre
	return out
}

// playwrightCheck reads the browser revisions playwright-core expects and
// looks for their directories in Playwright's browser cache.
func playwrightCheck(d *doctorRun, browser string) doctorResult {
	fix := "npx playwright install chromium chromium-headless-shell webkit"
	if d.p.repo == "" {
		return checkMissing("no repository (pass --repo)", "run from the checkout or pass --repo DIR")
	}
	b, err := d.p.readFile(d.inRepo("node_modules", "playwright-core", "browsers.json"))
	if err != nil {
		return checkMissing("node_modules/playwright-core/browsers.json not found", "npm ci")
	}
	var doc struct {
		Browsers []struct {
			Name              string            `json:"name"`
			Revision          string            `json:"revision"`
			RevisionOverrides map[string]string `json:"revisionOverrides"`
		} `json:"browsers"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return checkMissing("browsers.json unreadable", "npm ci")
	}
	var accepted []string
	for _, entry := range doc.Browsers {
		if entry.Name != browser {
			continue
		}
		accepted = append(accepted, entry.Revision)
		if browser == "webkit" {
			for _, rev := range entry.RevisionOverrides {
				accepted = append(accepted, rev)
			}
		}
	}
	if len(accepted) == 0 {
		return checkMissing(browser+" is not in browsers.json", "npm ci")
	}
	dir := d.playwrightDir()
	prefix := strings.ReplaceAll(browser, "-", "_") + "-"
	for _, rev := range accepted {
		if info, err := d.p.stat(filepath.Join(dir, prefix+rev)); err == nil && info.IsDir() {
			return checkOK(prefix + rev)
		}
	}
	found, _ := d.p.glob(filepath.Join(dir, prefix+"*"))
	var others []string
	for _, f := range found {
		rest := strings.TrimPrefix(filepath.Base(f), prefix)
		if _, err := strconv.Atoi(rest); err == nil {
			others = append(others, filepath.Base(f))
		}
	}
	sort.Strings(others)
	if len(others) > 0 {
		return checkWrong("found "+strings.Join(others, ", ")+"; need "+prefix+accepted[0], fix)
	}
	return checkMissing(prefix+accepted[0]+" not installed", fix)
}

func (d *doctorRun) playwrightDir() string {
	if v := d.p.env("PLAYWRIGHT_BROWSERS_PATH"); v != "" {
		if v == "0" {
			return d.inRepo("node_modules", "playwright-core", ".local-browsers")
		}
		return v
	}
	if d.p.goos == "darwin" {
		return d.home("Library", "Caches", "ms-playwright")
	}
	if v := d.p.env("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "ms-playwright")
	}
	return d.home(".cache", "ms-playwright")
}

// ---- release ----

func releaseChecks() []doctorCheck {
	checks := []doctorCheck{
		doctorOnPath("gh", "install the GitHub CLI"),
		loginCheck("gh login", "gh", []string{"auth", "status"}, "gh auth login"),
		doctorOnPath("python3", "install python3"),
		nodeCheck("node "+doctorReleaseNodeRange, doctorReleaseNodeRange),
		{name: "wrangler", required: true, run: wranglerCheck},
		{name: "wrangler credential", required: true, run: wranglerCredentialCheck},
		{name: "ssh route truenas", required: true, run: sshRouteCheck},
	}
	return append(checks, deployConfigChecks()...)
}

// wranglerCheck finds wrangler on PATH or in npm's npx cache without running
// it or npx.
func wranglerCheck(d *doctorRun) doctorResult {
	if path, err := d.p.lookPath("wrangler"); err == nil {
		return checkOK(path + " (version not checked)")
	}
	cache := d.p.env("npm_config_cache")
	if cache == "" {
		cache = d.home(".npm")
	}
	matches, _ := d.p.glob(filepath.Join(cache, "_npx", "*", "node_modules", "wrangler", "package.json"))
	best, engines := "", ""
	for _, m := range matches {
		b, err := d.p.readFile(m)
		if err != nil {
			continue
		}
		var pkg struct {
			Version string `json:"version"`
			Engines struct {
				Node string `json:"node"`
			} `json:"engines"`
		}
		if json.Unmarshal(b, &pkg) != nil {
			continue
		}
		parsed, ok := parseSemver(pkg.Version)
		if !ok {
			continue
		}
		if best == "" || compareSemver(pkg.Version, best) > 0 {
			best, engines = semverText(parsed), pkg.Engines.Node
		}
	}
	if best == "" {
		return checkMissing("not on PATH or in the npx cache", "npx wrangler --version (downloads it)")
	}
	if engines != "" {
		node, _, found := d.nodeVersion()
		if found && !semverSatisfies(node, engines) {
			return checkWrong("wrangler "+best+" needs node "+doctorWord(engines)+", have "+node, "install a Node.js version in "+doctorWord(engines))
		}
	}
	return checkOK("wrangler " + best + " (npx cache)")
}

func wranglerCredentialCheck(d *doctorRun) doctorResult {
	fix := "npx wrangler login, or set CLOUDFLARE_API_TOKEN"
	if d.p.env("CLOUDFLARE_API_TOKEN") != "" {
		return checkOK("CLOUDFLARE_API_TOKEN set, not verified online")
	}
	var path string
	if d.p.goos == "darwin" {
		path = d.home("Library", "Preferences", ".wrangler", "config", "default.toml")
	} else {
		base := d.p.env("XDG_CONFIG_HOME")
		if base == "" {
			base = d.home(".config")
		}
		path = filepath.Join(base, ".wrangler", "config", "default.toml")
	}
	b, err := d.p.readFile(path)
	if err != nil {
		return checkMissing("no CLOUDFLARE_API_TOKEN and no wrangler login file", fix)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		if unq, err := strconv.Unquote(value); err == nil {
			value = unq
		}
		values[strings.TrimSpace(key)] = value
	}
	if values["oauth_token"] == "" {
		return checkMissing("wrangler login file has no oauth_token", fix)
	}
	expires, err := time.Parse(time.RFC3339Nano, values["expiration_time"])
	if err == nil && expires.After(d.p.now()) {
		return checkOK("credential present, not verified online")
	}
	if values["refresh_token"] != "" {
		return checkOK("credential present (refreshable), not verified online")
	}
	return checkMissing("wrangler login expired and has no refresh token", fix)
}

// sshRouteCheck reads ~/.ssh/config (and its Includes) for the truenas alias
// the release uses. It never runs ssh; Match blocks are noted, not evaluated.
func sshRouteCheck(d *doctorRun) doctorResult {
	const alias = "truenas"
	fix := "add Host truenas with HostName to ~/.ssh/config (scripts/deploy-truenas-hub.py)"
	path := d.home(".ssh", "config")
	if _, err := d.p.readFile(path); err != nil {
		return checkMissing("no ~/.ssh/config", fix)
	}
	s := &sshScan{d: d, alias: alias, seen: map[string]bool{}}
	s.file(path, 0)
	switch {
	case !s.aliasHostName && s.aliasBlock:
		return checkMissing("Host truenas has no HostName", fix)
	case !s.aliasHostName:
		return checkMissing("no Host truenas", fix)
	case s.match:
		return doctorResult{status: doctorWarn, detail: "Host truenas -> HostName set; Match rules not evaluated", fix: "confirm with ssh -G truenas"}
	}
	return checkOK("Host truenas -> HostName set")
}

type sshScan struct {
	d                                *doctorRun
	alias                            string
	seen                             map[string]bool
	inBlock                          bool
	aliasBlock, aliasHostName, match bool
}

func (s *sshScan) file(path string, depth int) {
	if depth > 16 || s.seen[path] {
		return
	}
	s.seen[path] = true
	defer delete(s.seen, path)
	b, err := s.d.p.readFile(path)
	if err != nil {
		return
	}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.IndexAny(line, " \t=")
		if i < 0 {
			continue
		}
		keyword := strings.ToLower(line[:i])
		args := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[i:]), "="))
		switch keyword {
		case "host":
			s.inBlock = sshHostMatches(strings.Fields(args), s.alias)
			if s.inBlock {
				s.aliasBlock = true
			}
		case "match":
			s.match, s.inBlock = true, false
		case "hostname":
			if s.inBlock && args != "" {
				s.aliasHostName = true
			}
		case "include":
			for _, arg := range strings.Fields(args) {
				arg = strings.Trim(arg, `"`)
				if strings.HasPrefix(arg, "~/") {
					arg = s.d.home(arg[2:])
				} else if !filepath.IsAbs(arg) {
					arg = s.d.home(".ssh", arg)
				}
				matches, _ := s.d.p.glob(arg)
				sort.Strings(matches)
				for _, m := range matches {
					s.file(m, depth+1)
				}
			}
		}
	}
}

// sshHostMatches applies ssh_config Host patterns: * and ? wildcards, and a
// matching !negation excludes the host.
func sshHostMatches(patterns []string, host string) bool {
	matched := false
	for _, p := range patterns {
		negated := strings.HasPrefix(p, "!")
		if sshWildcardMatch(strings.ToLower(strings.TrimPrefix(p, "!")), strings.ToLower(host)) {
			if negated {
				return false
			}
			matched = true
		}
	}
	return matched
}

func sshWildcardMatch(pattern, s string) bool {
	if pattern == "" {
		return s == ""
	}
	switch pattern[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if sshWildcardMatch(pattern[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && sshWildcardMatch(pattern[1:], s[1:])
	}
	return s != "" && s[0] == pattern[0] && sshWildcardMatch(pattern[1:], s[1:])
}

var doctorSHA40RE = regexp.MustCompile(`^[a-f0-9]{40}$`)

// deployConfigChecks checks the deployer's private config against the fields
// scripts/release-runner.mjs requires (serveDeployment, readBaselines). Lines
// name fields only, never values.
func deployConfigChecks() []doctorCheck {
	const fixPass = "pass --deploy-config PRIVATE_PATH"
	load := func(d *doctorRun) (map[string]any, doctorResult, bool) {
		if v, ok := d.memo["deploy-config"]; ok {
			c := v.(deployConfigLoad)
			return c.doc, c.res, c.ok
		}
		c := deployConfigLoad{}
		switch b, err := d.p.readFile(d.p.deployConfig); {
		case d.p.deployConfig == "":
			c.res = checkMissing("no --deploy-config", fixPass)
		case err != nil:
			c.res = checkMissing("deploy config not readable", "check the --deploy-config path")
		case json.Unmarshal(b, &c.doc) != nil:
			c.res = checkMissing("deploy config is not a JSON object", "fix the deployer config JSON")
		default:
			c.ok = true
		}
		d.memo["deploy-config"] = c
		return c.doc, c.res, c.ok
	}
	// Without --deploy-config the role prints one missing line, not eleven.
	noConfig := func(d *doctorRun) bool { return d.p.deployConfig == "" }
	field := func(name string, test func(doc map[string]any) bool, fix string) doctorCheck {
		return doctorCheck{name: "deploy config " + name, required: true, skip: noConfig, run: func(d *doctorRun) doctorResult {
			doc, res, loaded := load(d)
			if !loaded {
				return checkMissing("not checked: "+res.detail, res.fix)
			}
			if !test(doc) {
				return checkMissing(name+" missing or invalid", fix)
			}
			return checkOK(name + " set")
		}}
	}
	str := func(key string) func(map[string]any) bool {
		return func(doc map[string]any) bool { s, _ := doc[key].(string); return s != "" }
	}
	baseline := func(target string) func(map[string]any) bool {
		return func(doc map[string]any) bool {
			b, _ := doc["baselines"].(map[string]any)
			s, _ := b[target].(string)
			return doctorSHA40RE.MatchString(s)
		}
	}
	checks := []doctorCheck{
		{name: "deploy config", required: true, run: func(d *doctorRun) doctorResult {
			if _, res, loaded := load(d); !loaded {
				return res
			}
			return checkOK("read")
		}},
		{name: "deploy config private", required: true, skip: noConfig, run: func(d *doctorRun) doctorResult {
			info, err := d.p.stat(d.p.deployConfig)
			if err != nil {
				return checkMissing("not checked: deploy config not readable", "check the --deploy-config path")
			}
			if info.Mode().Perm()&0o044 != 0 {
				return checkMissing(fmt.Sprintf("mode %04o is group/other readable", info.Mode().Perm()), "chmod 600 the deployer config")
			}
			return checkOK(fmt.Sprintf("mode %04o", info.Mode().Perm()))
		}},
		field("version", func(doc map[string]any) bool { v, _ := doc["version"].(float64); return v == 1 }, "set version to 1"),
		field("enabled", func(doc map[string]any) bool { v, _ := doc["enabled"].(bool); return v }, "set enabled to true for an active deployer"),
		field("cwd", str("cwd"), "set cwd to the deployment checkout"),
		field("journalDirectory", str("journalDirectory"), "set journalDirectory"),
	}
	for _, t := range []string{"hub", "bridge", "mini", "tailos"} {
		checks = append(checks, field("baselines."+t, baseline(t), "set baselines."+t+" to the last successful 40-character commit"))
	}
	return checks
}

type deployConfigLoad struct {
	doc map[string]any
	res doctorResult
	ok  bool
}

// ---- semver ranges (package.json engines) ----

// parseSemver reads MAJOR[.MINOR[.PATCH]], ignoring a pre-release suffix.
func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "x" || p == "*" {
			break
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// semverText prints a parsed version, so no other text from a tool or file
// reaches the report.
func semverText(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

func compareSemver(a, b string) int {
	pa, _ := parseSemver(a)
	pb, _ := parseSemver(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// semverSatisfies evaluates the npm engines subset used here: ||, space
// separated comparators (>=, >, <=, <, =), ^ and ~ ranges and bare versions.
// An unparsable range is not satisfied.
func semverSatisfies(version, rng string) bool {
	for _, alt := range strings.Split(rng, "||") {
		comparators := strings.Fields(alt)
		if len(comparators) == 0 {
			continue
		}
		all := true
		for _, c := range comparators {
			if !semverComparator(version, c) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func semverComparator(version, c string) bool {
	if _, ok := parseSemver(version); !ok {
		return false
	}
	for _, op := range []string{">=", "<=", ">", "<", "=", "^", "~"} {
		if !strings.HasPrefix(c, op) {
			continue
		}
		bound := strings.TrimPrefix(c, op)
		b, ok := parseSemver(bound)
		if !ok {
			return false
		}
		cmp := compareSemver(version, bound)
		switch op {
		case ">=":
			return cmp >= 0
		case "<=":
			return cmp <= 0
		case ">":
			return cmp > 0
		case "<":
			return cmp < 0
		case "=":
			return cmp == 0
		case "^":
			upper := [3]int{b[0] + 1, 0, 0}
			if b[0] == 0 {
				upper = [3]int{0, b[1] + 1, 0}
			}
			return cmp >= 0 && compareSemver(version, fmt.Sprintf("%d.%d.%d", upper[0], upper[1], upper[2])) < 0
		case "~":
			return cmp >= 0 && compareSemver(version, fmt.Sprintf("%d.%d.0", b[0], b[1]+1)) < 0
		}
	}
	if _, ok := parseSemver(c); !ok {
		return false
	}
	return compareSemver(version, c) == 0
}
