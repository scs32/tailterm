package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/adapters"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// tt host setup makes this machine an agent host in one idempotent command:
// it installs or updates ~/.local/bin/tt with a rollback copy, merges the
// Codex and Claude Code hooks, installs the inbox relay as a launchd service
// and runs doctor. --check reports the same steps without changing anything;
// --rollback restores the previous binary. See docs/host-setup.md.

const hostSetupUsage = `usage: tt host setup [--hub URL] [--from PATH] [--service agent|daemon] [--check | --rollback | --remove-claude-usage] [--json]

Installs or updates tt, the Codex and Claude Code hooks, the Claude usage
capture status line and the inbox relay service on this machine, then runs
tt doctor. Safe to run again.

  --hub URL         write ~/.config/tailterm/hub.json with this hub URL when absent
  --from PATH       the tt binary to install (default: the running executable)
  --service MODE    agent: a user LaunchAgent (default); daemon: a system LaunchDaemon (uses sudo)
  --check           report what is missing or out of date; change nothing; exit 1 if anything is
  --rollback        restore the previous tt binary and restart the relay
  --remove-claude-usage
                    remove the Claude usage capture status line and its command; change nothing else
  --json            print the steps and every path the run may write as JSON
`

const hostRelayLabel = "com.tailterm.inbox-relay"

// Seams. The environment defaults let a built binary run against fakes; the
// package tests point the commands at a path that does not exist.
var (
	hostSetupLaunchctl = or(os.Getenv("TAILTERM_LAUNCHCTL"), "launchctl")
	hostSetupSudo      = or(os.Getenv("TAILTERM_SUDO"), "sudo")
	hostSetupGOOS      = runtime.GOOS
	hostSetupRuntimes  = spawn.Runtimes
	hostSetupUID       = os.Getuid
	// How long doctor waits for the relay after a restart, and how long a
	// booted-out service may take to disappear.
	hostSetupRelayWait = 10 * time.Second
	hostSetupStopWait  = 5 * time.Second
	hostSetupPoll      = 100 * time.Millisecond
	// After a restart the relay lock is first probed after this delay, then
	// at this interval (see waitForRelay).
	hostSetupRelayProbe = 2 * time.Second
	relayRunningProbe   = relayRunning
)

const (
	hostCurrent   = "current"
	hostInstalled = "installed"
	hostUpdated   = "updated"
	hostMissing   = "missing"
	hostOutdated  = "outdated"
	hostConflict  = "conflict"
	hostFailed    = "failed"
	hostRemoved   = "removed"
)

type hostStep struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type hostReport struct {
	Mode    string            `json:"mode"`
	OK      bool              `json:"ok"`
	Service string            `json:"service,omitempty"`
	Steps   []hostStep        `json:"steps"`
	Doctor  []string          `json:"doctor,omitempty"`
	Paths   map[string]string `json:"paths"`
}

// hostPaths is every location host setup reads or writes.
type hostPaths struct {
	home, install, staged, rollback, rollbackTmp string
	codexHooks, claudeSettings, hubConfig        string
	// claudeUsageScript is the capture status line command.
	claudeUsageScript                          string
	agentPlist, daemonPlist, logDir, relayLock string
	// restartPending exists from the moment a new binary is installed until
	// the relay has been restarted on it, so a later run finishes the job.
	restartPending, daemonStaged string
}

func newHostPaths() (hostPaths, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return hostPaths{}, errors.New("no home directory: set HOME")
	}
	bin := filepath.Join(home, ".local", "bin")
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(home, ".claude")
	}
	root := or(os.Getenv("TAILTERM_HOST_SETUP_ROOT"), "/")
	return hostPaths{
		home:              home,
		install:           filepath.Join(bin, "tt"),
		staged:            filepath.Join(bin, "tt.new"),
		rollback:          filepath.Join(bin, "tt.previous"),
		rollbackTmp:       filepath.Join(bin, "tt.previous.tmp"),
		codexHooks:        filepath.Join(codexHome(), "hooks.json"),
		claudeSettings:    filepath.Join(claude, "settings.json"),
		claudeUsageScript: filepath.Join(bin, claudeUsageCaptureName),
		hubConfig:         filepath.Join(home, ".config", "tailterm", "hub.json"),
		agentPlist:        filepath.Join(home, "Library", "LaunchAgents", hostRelayLabel+".plist"),
		daemonPlist:       filepath.Join(root, "Library", "LaunchDaemons", hostRelayLabel+".plist"),
		logDir:            filepath.Join(home, "Library", "Logs", "Tailterm"),
		relayLock:         filepath.Join(relayDir(), "relay.lock"),
		restartPending:    filepath.Join(home, ".local", "state", "tailterm", "host-setup-restart-pending"),
		daemonStaged:      filepath.Join(home, ".local", "state", "tailterm", hostRelayLabel+".daemon.plist.staged"),
	}, nil
}

// resolvePath follows symlinks as far as the path exists, so a reported path
// is where a write would really land.
func resolvePath(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if target, err := os.Readlink(path); err == nil { // dangling link
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return resolvePath(target)
	}
	dir := filepath.Dir(path)
	if dir == path {
		return path
	}
	return filepath.Join(resolvePath(dir), filepath.Base(path))
}

// resolved lists every file a run may write, fully resolved.
func (p hostPaths) resolved() map[string]string {
	codex, claude := resolvePath(p.codexHooks), resolvePath(p.claudeSettings)
	return map[string]string{
		"install":              resolvePath(p.install),
		"staged":               resolvePath(p.staged),
		"rollback":             resolvePath(p.rollback),
		"rollbackStaged":       resolvePath(p.rollbackTmp),
		"codexHooks":           codex,
		"codexHooksBackup":     codex + hostBackupSuffix,
		"claudeSettings":       claude,
		"claudeSettingsBackup": claude + hostBackupSuffix,
		"claudeUsageScript":    resolvePath(p.claudeUsageScript),
		"hubConfig":            resolvePath(p.hubConfig),
		"agentPlist":           resolvePath(p.agentPlist),
		"daemonPlist":          resolvePath(p.daemonPlist),
		"daemonPlistStaged":    resolvePath(p.daemonStaged),
		"restartPending":       resolvePath(p.restartPending),
		"logDirectory":         resolvePath(p.logDir),
	}
}

type hostSetup struct {
	p             hostPaths
	apply         bool
	from, hub     string
	service       string // requested: "", agent or daemon
	binaryChanged bool
	// relayRestarted: this run started or restarted the relay, so doctor
	// waits for it to come up.
	relayRestarted bool
	steps          []hostStep
}

func (h *hostSetup) add(name, state, detail string) {
	h.steps = append(h.steps, hostStep{Name: name, State: state, Detail: detail})
}

func hostUsageError(msg string) error {
	return &exitError{code: 2, err: fmt.Errorf("host setup: %s\n%s", msg, strings.TrimSpace(hostSetupUsage))}
}

func cmdHost(args []string) error {
	if len(args) == 0 || args[0] != "setup" {
		return hostUsageError("unknown subcommand")
	}
	fs := flag.NewFlagSet("host setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	hub := fs.String("hub", "", "")
	from := fs.String("from", "", "")
	service := fs.String("service", "", "")
	check := fs.Bool("check", false, "")
	rollback := fs.Bool("rollback", false, "")
	removeUsage := fs.Bool("remove-claude-usage", false, "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(hostSetupUsage)
			return nil
		}
		return hostUsageError(err.Error())
	}
	switch {
	case fs.NArg() > 0:
		return hostUsageError("unexpected argument " + fs.Arg(0))
	case *check && *rollback:
		return hostUsageError("--check and --rollback cannot be combined")
	case *service != "" && *service != "agent" && *service != "daemon":
		return hostUsageError("--service must be agent or daemon")
	case *rollback && (*hub != "" || *from != "" || *service != ""):
		return hostUsageError("--rollback takes no --hub, --from or --service")
	case *removeUsage && (*check || *rollback || *hub != "" || *from != "" || *service != ""):
		return hostUsageError("--remove-claude-usage takes no other option but --json")
	}
	// As root every file written would be root-owned in the agent user's
	// home, so refuse before any step. Only the daemon's plist and launchctl
	// calls ever need privilege, and those go through sudo.
	if hostSetupUID() == 0 {
		return &exitError{code: 1, err: errors.New("host setup: run as the agent user, not root (do not use sudo); nothing was changed")}
	}
	p, err := newHostPaths()
	if err != nil {
		return err
	}
	h := &hostSetup{p: p, apply: !*check, from: *from, hub: *hub, service: *service}
	report := hostReport{Mode: "setup", Paths: p.resolved()}
	var doctor bytes.Buffer
	switch {
	case *rollback:
		report.Mode = "rollback"
		if err := h.rollbackBinary(); err != nil {
			return &exitError{code: 1, err: fmt.Errorf("host setup --rollback: %w", err)}
		}
		h.restartRelay()
		h.doctor(&doctor)
	case *removeUsage:
		report.Mode = "remove-claude-usage"
		h.removeClaudeUsage()
	default:
		if *check {
			report.Mode = "check"
		}
		h.binary()
		h.hubConfig()
		h.hooks("codex-hooks", "codex", p.codexHooks, codexHookEvents)
		h.hooks("claude-hooks", "claude", p.claudeSettings, claudeHookEvents)
		h.claudeUsage()
		report.Service = h.relayService()
		h.doctor(&doctor)
	}
	report.Steps = h.steps
	bad := 0
	for _, s := range h.steps {
		if s.State == hostFailed || s.State == hostMissing || s.State == hostOutdated || s.State == hostConflict {
			bad++
		}
	}
	report.OK = bad == 0
	for _, line := range strings.Split(strings.TrimRight(doctor.String(), "\n"), "\n") {
		if line != "" {
			report.Doctor = append(report.Doctor, line)
		}
	}
	if *asJSON {
		out, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(out))
	} else {
		for _, s := range h.steps {
			fmt.Printf("%-9s  %-13s  %s\n", s.State, s.Name, s.Detail)
			if s.Name == "doctor" {
				for _, line := range report.Doctor {
					fmt.Println("           " + line)
				}
			}
		}
	}
	if bad == 0 {
		if !*asJSON {
			fmt.Printf("host %s: ok\n", report.Mode)
		}
		return nil
	}
	if *check {
		return &exitError{code: 1, err: fmt.Errorf("host setup --check: %d step(s) missing, out of date or failed", bad)}
	}
	return &exitError{code: 1, err: fmt.Errorf("host %s: %d step(s) failed", report.Mode, bad)}
}

// ---- binary ----

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// copyFileSync copies src to dst with the given mode and syncs it, so a
// following rename publishes complete contents.
func copyFileSync(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Chmod(mode)
	}
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

func (h *hostSetup) binary() {
	const name = "binary"
	source := h.from
	if source == "" {
		source = selfPath()
	}
	want, err := fileSHA256(source)
	if err != nil {
		h.add(name, hostFailed, "cannot read the source binary: "+err.Error())
		return
	}
	have, err := fileSHA256(h.p.install)
	fresh := errors.Is(err, os.ErrNotExist)
	if err != nil && !fresh {
		h.add(name, hostFailed, "cannot read "+h.p.install+": "+err.Error())
		return
	}
	if have == want {
		h.add(name, hostCurrent, fmt.Sprintf("%s sha256 %s", h.p.install, want))
		return
	}
	if !h.apply {
		if fresh {
			h.add(name, hostMissing, fmt.Sprintf("%s is not installed; would install sha256 %s", h.p.install, want))
		} else {
			h.add(name, hostOutdated, fmt.Sprintf("%s is sha256 %s; would install %s", h.p.install, have, want))
		}
		return
	}
	if err := os.MkdirAll(filepath.Dir(h.p.install), 0o755); err != nil {
		h.add(name, hostFailed, err.Error())
		return
	}
	if !fresh {
		if err := copyFileSync(h.p.install, h.p.rollbackTmp, 0o755); err != nil {
			h.add(name, hostFailed, "cannot keep the rollback copy: "+err.Error())
			return
		}
		if err := os.Rename(h.p.rollbackTmp, h.p.rollback); err != nil {
			h.add(name, hostFailed, "cannot keep the rollback copy: "+err.Error())
			return
		}
	}
	if err := copyFileSync(source, h.p.staged, 0o755); err != nil {
		h.add(name, hostFailed, "cannot stage the new binary: "+err.Error())
		return
	}
	wasPending := h.restartIsPending()
	if err := h.markRestartPending(); err != nil {
		os.Remove(h.p.staged)
		h.add(name, hostFailed, err.Error())
		return
	}
	if err := os.Rename(h.p.staged, h.p.install); err != nil {
		os.Remove(h.p.staged)
		if !wasPending {
			os.Remove(h.p.restartPending)
		}
		h.add(name, hostFailed, "cannot install the new binary: "+err.Error())
		return
	}
	h.binaryChanged = true
	if fresh {
		h.add(name, hostInstalled, fmt.Sprintf("%s sha256 %s (no previous binary, so no rollback copy)", h.p.install, want))
		return
	}
	h.add(name, hostUpdated, fmt.Sprintf("%s sha256 %s; previous sha256 %s kept at %s", h.p.install, want, have, h.p.rollback))
}

// markRestartPending records, before the installed binary changes, that the
// relay still runs the old one. The record outlives a run whose restart
// failed, so the next run restarts the relay and --check reports it.
func (h *hostSetup) markRestartPending() error {
	if hostSetupGOOS != "darwin" {
		return nil // no relay service is managed here
	}
	if err := writeFileAtomic(h.p.restartPending, []byte("the relay must be restarted on the installed tt\n"), 0o600, 0o700); err != nil {
		return fmt.Errorf("cannot record the pending relay restart: %w", err)
	}
	return nil
}

func (h *hostSetup) restartIsPending() bool { return plistPresent(h.p.restartPending) }

// relayStarted clears the pending restart once the relay runs the installed
// binary.
func (h *hostSetup) relayStarted() error {
	h.relayRestarted = true
	if err := os.Remove(h.p.restartPending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the relay was restarted but the pending-restart record could not be removed: %w", err)
	}
	return nil
}

// rollbackBinary swaps tt and tt.previous, so a second rollback undoes the
// first. Nothing is changed when there is no rollback copy.
func (h *hostSetup) rollbackBinary() error {
	previous, err := fileSHA256(h.p.rollback)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no rollback copy at %s; nothing was changed", h.p.rollback)
	}
	if err != nil {
		return fmt.Errorf("cannot read the rollback copy %s: %w", h.p.rollback, err)
	}
	current, err := fileSHA256(h.p.install)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", h.p.install, err)
	}
	if err := copyFileSync(h.p.rollback, h.p.staged, 0o755); err != nil {
		return fmt.Errorf("cannot stage the rollback copy: %w", err)
	}
	if err := copyFileSync(h.p.install, h.p.rollbackTmp, 0o755); err != nil {
		os.Remove(h.p.staged)
		return fmt.Errorf("cannot keep the current binary: %w", err)
	}
	wasPending := h.restartIsPending()
	if err := h.markRestartPending(); err != nil {
		os.Remove(h.p.staged)
		os.Remove(h.p.rollbackTmp)
		return err
	}
	if err := os.Rename(h.p.staged, h.p.install); err != nil {
		os.Remove(h.p.staged)
		os.Remove(h.p.rollbackTmp)
		if !wasPending {
			os.Remove(h.p.restartPending)
		}
		return fmt.Errorf("cannot restore the previous binary: %w", err)
	}
	h.binaryChanged = true
	if err := os.Rename(h.p.rollbackTmp, h.p.rollback); err != nil {
		h.add("binary", hostFailed, fmt.Sprintf("restored sha256 %s but could not keep the replaced binary: %v", previous, err))
		return nil
	}
	h.add("binary", hostUpdated, fmt.Sprintf("%s restored to sha256 %s; replaced sha256 %s kept at %s", h.p.install, previous, current, h.p.rollback))
	return nil
}

// ---- hub config ----

func (h *hostSetup) hubConfig() {
	const name = "hub-config"
	data, err := os.ReadFile(h.p.hubConfig)
	absent := errors.Is(err, os.ErrNotExist)
	if err != nil && !absent {
		h.add(name, hostFailed, "cannot read "+h.p.hubConfig+": "+err.Error())
		return
	}
	if h.hub == "" {
		if absent {
			h.add(name, hostCurrent, "no --hub given and no hub.json; doctor reports whether a hub is configured")
		} else {
			h.add(name, hostCurrent, h.p.hubConfig+" left as it is")
		}
		return
	}
	if !absent {
		var config struct {
			URL string `json:"url"`
		}
		// The stored token belongs to the stored URL, so a different URL is
		// never written over it. Neither the file nor its URL is printed.
		if json.Unmarshal(data, &config) != nil {
			h.add(name, hostFailed, h.p.hubConfig+" is not valid JSON; fix or remove it, nothing was changed")
		} else if config.URL != h.hub {
			h.add(name, hostFailed, h.p.hubConfig+" already names a different hub; edit or remove it to change hubs, nothing was changed")
		} else {
			h.add(name, hostCurrent, h.p.hubConfig+" already names this hub")
		}
		return
	}
	if !h.apply {
		h.add(name, hostMissing, "would write "+h.p.hubConfig+" with the hub URL")
		return
	}
	out, _ := json.MarshalIndent(map[string]string{"url": h.hub}, "", "  ")
	if err := writeFileAtomic(h.p.hubConfig, append(out, '\n'), 0o600, 0o700); err != nil {
		h.add(name, hostFailed, err.Error())
		return
	}
	h.add(name, hostInstalled, h.p.hubConfig+" written with the hub URL (mode 600); add the token by hand, see docs/host-setup.md")
}

func writeFileAtomic(path string, data []byte, mode, dirMode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// ---- hooks ----

const hostBackupSuffix = ".before-tailterm"

// hookEvent is one runtime event tt manages and the tt hook it runs.
type hookEvent struct{ event, name string }

var (
	codexHookEvents  = []hookEvent{{"Stop", "stop"}}
	claudeHookEvents = []hookEvent{{"SessionStart", "session-start"}, {"UserPromptSubmit", "prompt"}, {"Stop", "stop"}, {"Notification", "notification"}, {"PreToolUse", "tool"}, {"PostToolUse", "tool"}, {"PostToolUseFailure", "tool"},
		{"SessionStart", "handoff"}, {"PreCompact", "handoff"}, {"SessionEnd", "handoff"}}
)

type mergeOutcome struct{ created, changed bool }

// mergeJSONFile validates the whole JSON document at path, lets mutate change
// it, and rewrites the file only when something changed. A symlink to a file
// is followed and stays a link. On any error nothing is written. With write
// false it only reports whether a write would happen.
func mergeJSONFile(path string, write bool, mutate func(doc map[string]any) (bool, error)) (mergeOutcome, error) {
	target := path
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return mergeOutcome{}, fmt.Errorf("%s is a symlink whose target does not exist; fix the link first", path)
		}
		target = resolved
	}
	doc := map[string]any{}
	mode := os.FileMode(0o600)
	raw, err := os.ReadFile(target)
	created := errors.Is(err, os.ErrNotExist)
	switch {
	case created:
	case err != nil:
		return mergeOutcome{}, err
	default:
		info, err := os.Stat(target)
		if err != nil {
			return mergeOutcome{}, err
		}
		if !info.Mode().IsRegular() {
			return mergeOutcome{}, fmt.Errorf("%s is not a regular file", target)
		}
		mode = info.Mode().Perm()
		if len(bytes.TrimSpace(raw)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			var value any
			if err := dec.Decode(&value); err != nil {
				return mergeOutcome{}, fmt.Errorf("%s is not valid JSON; fix it first: %w", path, err)
			}
			if _, err := dec.Token(); err != io.EOF {
				return mergeOutcome{}, fmt.Errorf("%s has trailing content after the JSON document; fix it first", path)
			}
			object, ok := value.(map[string]any)
			if !ok {
				return mergeOutcome{}, fmt.Errorf("%s: the top level is not a JSON object", path)
			}
			doc = object
		}
	}
	changed, err := mutate(doc)
	if err != nil {
		return mergeOutcome{}, fmt.Errorf("%s: %w", path, err)
	}
	out := mergeOutcome{created: created, changed: changed}
	if !changed || !write {
		return out, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return mergeOutcome{}, err
	}
	if !created {
		// One copy of the file as it was before Tailterm first changed it.
		backup := target + hostBackupSuffix
		if _, err := os.Lstat(backup); errors.Is(err, os.ErrNotExist) {
			if err := writeFileAtomic(backup, raw, mode, 0o700); err != nil {
				return mergeOutcome{}, fmt.Errorf("cannot keep %s: %w", backup, err)
			}
		}
	}
	if err := writeFileAtomic(target, buf.Bytes(), mode, 0o700); err != nil {
		return mergeOutcome{}, err
	}
	return out, nil
}

// checkHookShape rejects a document whose managed events are not the shape
// both runtimes define, naming the JSON path. Other events are not inspected.
func checkHookShape(doc map[string]any, events []hookEvent) error {
	value, ok := doc["hooks"]
	if !ok {
		return nil
	}
	hooks, ok := value.(map[string]any)
	if !ok {
		return errors.New("hooks is not an object")
	}
	for _, ev := range events {
		value, ok := hooks[ev.event]
		if !ok {
			continue
		}
		groups, ok := value.([]any)
		if !ok {
			return fmt.Errorf("hooks.%s is not an array", ev.event)
		}
		for i, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				return fmt.Errorf("hooks.%s[%d] is not an object", ev.event, i)
			}
			value, ok := group["hooks"]
			if !ok {
				continue
			}
			inner, ok := value.([]any)
			if !ok {
				return fmt.Errorf("hooks.%s[%d].hooks is not an array", ev.event, i)
			}
			for j, entry := range inner {
				hook, ok := entry.(map[string]any)
				if !ok {
					return fmt.Errorf("hooks.%s[%d].hooks[%d] is not an object", ev.event, i, j)
				}
				if command, ok := hook["command"]; ok {
					if _, ok := command.(string); !ok {
						return fmt.Errorf("hooks.%s[%d].hooks[%d].command is not a string", ev.event, i, j)
					}
				}
			}
		}
	}
	return nil
}

// isTTHook reports whether a hook command runs a tt binary's hook <name>.
func isTTHook(command, name string) bool {
	exe, got, ok := adapters.ParseHookCommand(command)
	return ok && filepath.Base(exe) == "tt" && got == name
}

// mergeTTHooks brings each managed event to exactly one tt entry, in a group
// with no matcher, and no other tt entry. Everything else in the document is
// kept. With keepExisting an unrestricted tt entry is left as written;
// otherwise its command becomes the canonical one for tt, and its timeout
// the one that hook carries (adapters.HookTimeoutSeconds), if it has one.
func mergeTTHooks(doc map[string]any, events []hookEvent, tt string, keepExisting bool) (bool, error) {
	if err := checkHookShape(doc, events); err != nil {
		return false, err
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	changed := false
	for _, ev := range events {
		canonical := adapters.HookCommand(tt, ev.name)
		timeout := adapters.HookTimeoutSeconds(ev.name)
		groups, _ := hooks[ev.event].([]any)
		kept := false
		out := make([]any, 0, len(groups)+1)
		for _, g := range groups {
			group := g.(map[string]any)
			inner, has := group["hooks"].([]any)
			matcher, restricted := group["matcher"]
			if restricted {
				restricted = matcher != nil && matcher != ""
			}
			remaining := make([]any, 0, len(inner))
			for _, entry := range inner {
				hook := entry.(map[string]any)
				command, _ := hook["command"].(string)
				switch {
				case !isTTHook(command, ev.name):
					remaining = append(remaining, hook)
				case !restricted && !kept:
					kept = true
					if command != canonical && !keepExisting {
						hook["command"] = canonical
						changed = true
					}
					if timeout > 0 && !keepExisting && fmt.Sprint(hook["timeout"]) != fmt.Sprint(timeout) {
						hook["timeout"] = timeout
						changed = true
					}
					remaining = append(remaining, hook)
				default: // a duplicate, or one a matcher would not always fire
					changed = true
				}
			}
			if has && len(remaining) == 0 && len(inner) > 0 {
				continue
			}
			if has {
				group["hooks"] = remaining
			}
			out = append(out, group)
		}
		if !kept {
			out = append(out, map[string]any{"hooks": []any{adapters.HookEntry(tt, ev.name)}})
			changed = true
		}
		hooks[ev.event] = out
	}
	doc["hooks"] = hooks
	return changed, nil
}

func (h *hostSetup) hooks(name, runtimeName, path string, events []hookEvent) {
	installed := false
	for _, r := range hostSetupRuntimes() {
		installed = installed || r == runtimeName
	}
	if !installed {
		h.add(name, hostCurrent, runtimeName+" runtime not installed; hooks not needed")
		return
	}
	out, err := mergeJSONFile(path, h.apply, func(doc map[string]any) (bool, error) {
		return mergeTTHooks(doc, events, h.p.install, false)
	})
	switch {
	case err != nil:
		h.add(name, hostFailed, err.Error()+"; nothing was changed")
	case !out.changed:
		h.add(name, hostCurrent, path)
	case !h.apply && out.created:
		h.add(name, hostMissing, "would create "+path)
	case !h.apply:
		h.add(name, hostOutdated, "would update the tt hooks in "+path)
	case out.created:
		h.add(name, hostInstalled, path)
	default:
		h.add(name, hostUpdated, path+" (the file as it was is kept at "+resolvePath(path)+hostBackupSuffix+")")
	}
}

// ---- Claude usage capture ----

// The capture status line is a Claude Code statusLine command that saves the
// account's rate limits for the token budget and prints nothing
// (provider_usage.go). Claude Code has one statusLine, so a different one the
// user set is never replaced: it is left as it is and reported.

// claudeUsageStatusLine is the settings entry host setup manages.
func claudeUsageStatusLine(script string) map[string]any {
	return map[string]any{"type": "command", "command": script}
}

// ownClaudeUsageStatusLine reports whether a statusLine value is the capture
// command: a command entry that runs the installed script and nothing else.
func ownClaudeUsageStatusLine(value any, script string) bool {
	entry, ok := value.(map[string]any)
	if !ok {
		return false
	}
	command, _ := entry["command"].(string)
	kind, _ := entry["type"].(string)
	return command == script && (kind == "command" || kind == "")
}

// statusLineText prints a statusLine value on one line for a report.
func statusLineText(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(raw)
}

// claudeUsageScriptState is missing, outdated or current for the installed
// capture command.
func claudeUsageScriptState(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return hostMissing, nil
	}
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if string(data) != claudeUsageCaptureScript || info.Mode().Perm()&0o111 == 0 {
		return hostOutdated, nil
	}
	return hostCurrent, nil
}

// claudeUsage installs or checks the capture status line: the command at
// ~/.local/bin/tt-claude-usage-capture and the statusLine entry in the Claude
// settings. A different existing statusLine is a conflict: nothing is
// written and the report shows its value.
func (h *hostSetup) claudeUsage() {
	const name = "claude-usage"
	installed := false
	for _, r := range hostSetupRuntimes() {
		installed = installed || r == "claude"
	}
	if !installed {
		h.add(name, hostCurrent, "claude runtime not installed; usage capture not needed")
		return
	}
	script, settings := h.p.claudeUsageScript, h.p.claudeSettings
	// Settings the hooks step refused are not touched here either.
	for _, step := range h.steps {
		if step.Name == "claude-hooks" && step.State == hostFailed {
			h.add(name, hostFailed, "not installed: "+settings+" was refused by claude-hooks; fix it first; nothing was changed")
			return
		}
	}
	// Look first, so a conflict writes nothing at all.
	var existing any
	probe, err := mergeJSONFile(settings, false, func(doc map[string]any) (bool, error) {
		existing = doc["statusLine"]
		return existing == nil, nil
	})
	if err != nil {
		h.add(name, hostFailed, err.Error()+"; nothing was changed")
		return
	}
	if existing != nil && !ownClaudeUsageStatusLine(existing, script) {
		h.add(name, hostConflict, fmt.Sprintf("%s already has a different statusLine, left unchanged: %s. Claude usage is not captured on this host; to capture it, have that status line command also run %s with the same input, or remove it and run tt host setup again", settings, statusLineText(existing), script))
		return
	}
	scriptState, err := claudeUsageScriptState(script)
	if err != nil {
		h.add(name, hostFailed, err.Error()+"; nothing was changed")
		return
	}
	if scriptState == hostCurrent && !probe.changed {
		h.add(name, hostCurrent, script+" is the statusLine in "+settings)
		return
	}
	if !h.apply {
		// Missing: the command or its statusLine entry is not there.
		// Outdated: both are, and the command is an older version.
		state, detail := hostOutdated, "would update "+script
		switch {
		case scriptState == hostMissing && probe.changed:
			state, detail = hostMissing, "would install "+script+" and set it as the statusLine in "+settings
		case probe.changed:
			state, detail = hostMissing, "would set "+script+" as the statusLine in "+settings
		case scriptState == hostMissing:
			state, detail = hostMissing, "would install "+script
		}
		h.add(name, state, detail)
		return
	}
	if scriptState != hostCurrent {
		if err := writeFileAtomic(script, []byte(claudeUsageCaptureScript), 0o755, 0o755); err != nil {
			h.add(name, hostFailed, err.Error())
			return
		}
	}
	out, err := mergeJSONFile(settings, true, func(doc map[string]any) (bool, error) {
		if current := doc["statusLine"]; current != nil {
			if !ownClaudeUsageStatusLine(current, script) {
				return false, errors.New("statusLine changed while host setup ran")
			}
			return false, nil
		}
		doc["statusLine"] = claudeUsageStatusLine(script)
		return true, nil
	})
	switch {
	case err != nil:
		h.add(name, hostFailed, err.Error())
	case scriptState == hostMissing && out.changed:
		h.add(name, hostInstalled, script+" set as the statusLine in "+settings+"; remove with tt host setup --remove-claude-usage")
	case out.changed:
		h.add(name, hostUpdated, script+" set as the statusLine in "+settings+" (the file as it was is kept at "+resolvePath(settings)+hostBackupSuffix+")")
	default:
		h.add(name, hostUpdated, script)
	}
}

// removeClaudeUsage is the one command that takes the capture status line
// out again: our statusLine entry and the command file, nothing else. A
// different statusLine is left alone and reported. The last capture file
// stays where it is; nothing refreshes it, so the hub stops using it once it
// is older than the budget's staleness bound.
func (h *hostSetup) removeClaudeUsage() {
	const name = "claude-usage"
	script, settings := h.p.claudeUsageScript, h.p.claudeSettings
	var other any
	out, err := mergeJSONFile(settings, true, func(doc map[string]any) (bool, error) {
		current, ok := doc["statusLine"]
		if !ok || current == nil {
			return false, nil
		}
		if !ownClaudeUsageStatusLine(current, script) {
			other = current
			return false, nil
		}
		delete(doc, "statusLine")
		return true, nil
	})
	if err != nil {
		h.add(name, hostFailed, err.Error()+"; nothing was changed")
		return
	}
	removed := out.changed
	if err := os.Remove(script); err == nil {
		removed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		h.add(name, hostFailed, err.Error())
		return
	}
	detail := "the capture status line was not installed"
	state := hostCurrent
	if removed {
		state, detail = hostRemoved, "removed "+script+" and its statusLine entry in "+settings
	}
	if other != nil {
		detail += "; a different statusLine is set and was left unchanged: " + statusLineText(other)
	}
	h.add(name, state, detail)
}

// ---- relay service ----

type relayDomain struct {
	mode, domain, plist string
	sudo                bool
}

func (d relayDomain) target() string { return d.domain + "/" + hostRelayLabel }

func (h *hostSetup) domains() (agent, daemon relayDomain) {
	return relayDomain{mode: "agent", domain: fmt.Sprintf("gui/%d", hostSetupUID()), plist: h.p.agentPlist},
		relayDomain{mode: "daemon", domain: "system", plist: h.p.daemonPlist, sudo: true}
}

// hostRun runs a launchd-related command, through sudo when asked, and
// returns its exit status. Output is discarded; sudo may still prompt.
func hostRun(sudo bool, name string, args ...string) (int, error) {
	if sudo {
		name, args = hostSetupSudo, append([]string{name}, args...)
	}
	cmd := exec.Command(name, args...)
	if sudo {
		cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	}
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

const (
	relayLoaded = "loaded"
	relayAbsent = "absent"
	// launchctl print exits 113 for "could not find service".
	launchctlNotFound = 113
)

func probeRelay(d relayDomain) (string, error) {
	code, err := hostRun(false, hostSetupLaunchctl, "print", d.target())
	switch {
	case err != nil:
		return "", fmt.Errorf("launchctl print %s did not run: %w", d.target(), err)
	case code == 0:
		return relayLoaded, nil
	case code == launchctlNotFound:
		return relayAbsent, nil
	}
	return "", fmt.Errorf("launchctl print %s exited %d; the service state is unknown", d.target(), code)
}

func plistPresent(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// stopRelay boots a service out and waits until launchd no longer has it,
// then removes its plist when asked. Any doubt is an error, so a second
// relay is never started beside one that may still be loaded.
func stopRelay(d relayDomain, state string, removePlist bool) error {
	if state == relayLoaded {
		code, err := hostRun(d.sudo, hostSetupLaunchctl, "bootout", d.target())
		if err != nil {
			return fmt.Errorf("launchctl bootout %s did not run: %w", d.target(), err)
		}
		deadline := time.Now().Add(hostSetupStopWait)
		for {
			now, err := probeRelay(d)
			if err != nil {
				return err
			}
			if now == relayAbsent {
				break
			}
			// launchctl can exit non-zero while the boot-out is still in
			// progress, so a failed bootout gets the same wait.
			if !time.Now().Before(deadline) {
				if code != 0 {
					return fmt.Errorf("launchctl bootout %s exited %d and the %s service is still loaded", d.target(), code, d.mode)
				}
				return fmt.Errorf("the %s service %s is still loaded after bootout", d.mode, d.target())
			}
			time.Sleep(hostSetupPoll)
		}
	}
	if !removePlist || !plistPresent(d.plist) {
		return nil
	}
	if d.sudo {
		if code, err := hostRun(true, "rm", "-f", d.plist); err != nil || code != 0 {
			return fmt.Errorf("could not remove %s through sudo (exit %d)", d.plist, code)
		}
	} else if err := os.Remove(d.plist); err != nil {
		return err
	}
	if plistPresent(d.plist) {
		return fmt.Errorf("%s is still present after removal", d.plist)
	}
	return nil
}

func relayPlist(d relayDomain, p hostPaths) ([]byte, error) {
	var b bytes.Buffer
	esc := func(s string) string {
		var out bytes.Buffer
		_ = xml.EscapeText(&out, []byte(s))
		return out.String()
	}
	str := func(indent, key, value string) {
		fmt.Fprintf(&b, "%s<key>%s</key>\n%s<string>%s</string>\n", indent, esc(key), indent, esc(value))
	}
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\">\n<dict>\n")
	str("\t", "Label", hostRelayLabel)
	fmt.Fprintf(&b, "\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>%s</string>\n\t\t<string>relay</string>\n\t</array>\n", esc(p.install))
	str("\t", "WorkingDirectory", p.home)
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	if d.mode == "daemon" {
		str("\t\t", "HOME", p.home)
	}
	str("\t\t", "PATH", filepath.Dir(p.install)+":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin")
	b.WriteString("\t</dict>\n")
	if d.mode == "daemon" {
		// The relay runs as the agent user, never as root.
		u, err := user.Current()
		if err != nil || u.Username == "" {
			return nil, errors.New("cannot name the current user for the daemon's UserName")
		}
		str("\t", "UserName", u.Username)
	}
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<true/>\n\t<key>ThrottleInterval</key>\n\t<integer>15</integer>\n")
	log := filepath.Join(p.logDir, "inbox-relay.log")
	str("\t", "StandardOutPath", log)
	str("\t", "StandardErrorPath", log)
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes(), nil
}

func writeRelayPlist(d relayDomain, data []byte, staged string) error {
	if !d.sudo {
		return writeFileAtomic(d.plist, data, 0o600, 0o755)
	}
	if err := writeFileAtomic(staged, data, 0o600, 0o700); err != nil {
		return err
	}
	defer os.Remove(staged)
	if _, err := os.Stat(filepath.Dir(d.plist)); err != nil {
		if code, err := hostRun(true, "mkdir", "-p", filepath.Dir(d.plist)); err != nil || code != 0 {
			return fmt.Errorf("could not create %s through sudo (exit %d)", filepath.Dir(d.plist), code)
		}
	}
	if code, err := hostRun(true, "install", "-o", "root", "-g", "wheel", "-m", "0644", staged, d.plist); err != nil || code != 0 {
		return fmt.Errorf("could not install %s through sudo (exit %d)", d.plist, code)
	}
	return nil
}

// relayService installs, updates or checks the launchd service and returns
// the mode it settled on.
func (h *hostSetup) relayService() string {
	const name = "relay-service"
	if hostSetupGOOS != "darwin" {
		h.add(name, hostCurrent, "not macOS: supervise `tt relay` with this host's service manager")
		return ""
	}
	agent, daemon := h.domains()
	agentState, err := probeRelay(agent)
	if err != nil {
		h.add(name, hostFailed, err.Error())
		return ""
	}
	daemonState, err := probeRelay(daemon)
	if err != nil {
		h.add(name, hostFailed, err.Error())
		return ""
	}
	agentHere := agentState == relayLoaded || plistPresent(agent.plist)
	daemonHere := daemonState == relayLoaded || plistPresent(daemon.plist)
	mode := h.service
	switch {
	case mode != "":
	case agentHere && daemonHere:
		state := hostFailed
		if !h.apply && agentState == relayLoaded && daemonState == relayLoaded {
			state = hostConflict
		}
		h.add(name, state, "both the agent and the daemon relay are installed; choose one with --service agent or --service daemon")
		return ""
	case daemonHere:
		mode = "daemon"
	default:
		mode = "agent"
	}
	sel, other, selState, otherState, otherHere := agent, daemon, agentState, daemonState, daemonHere
	if mode == "daemon" {
		sel, other, selState, otherState, otherHere = daemon, agent, daemonState, agentState, agentHere
	}
	want, err := relayPlist(sel, h.p)
	if err != nil {
		h.add(name, hostFailed, err.Error())
		return mode
	}
	have, err := os.ReadFile(sel.plist)
	fresh := errors.Is(err, os.ErrNotExist)
	if err != nil && !fresh {
		h.add(name, hostFailed, "cannot read "+sel.plist+": "+err.Error())
		return mode
	}
	same := !fresh && bytes.Equal(have, want)

	if !h.apply {
		switch {
		case agentState == relayLoaded && daemonState == relayLoaded:
			h.add(name, hostConflict, "both the agent and the daemon relay are loaded; a run with --service "+mode+" would remove the "+other.mode)
		case otherHere:
			h.add(name, hostOutdated, "would switch the relay from "+other.mode+" to "+mode)
		case fresh:
			h.add(name, hostMissing, "would install the "+mode+" relay service at "+sel.plist)
		case !same:
			h.add(name, hostOutdated, "would rewrite "+sel.plist+" and reload the "+mode+" relay")
		case selState != relayLoaded:
			h.add(name, hostOutdated, "the "+mode+" relay is installed but not loaded; would start it")
		case h.restartIsPending():
			h.add(name, hostOutdated, "the "+mode+" relay still runs the previous binary; would restart it")
		default:
			h.add(name, hostCurrent, mode+" relay "+sel.target())
		}
		return mode
	}

	fail := func(err error) string {
		h.add(name, hostFailed, err.Error()+"; the "+mode+" relay was not started")
		return mode
	}
	if otherHere {
		if err := stopRelay(other, otherState, true); err != nil {
			return fail(err)
		}
	}
	state, detail := hostCurrent, mode+" relay "+sel.target()
	bootstrap := func() error {
		if code, err := hostRun(sel.sudo, hostSetupLaunchctl, "bootstrap", sel.domain, sel.plist); err != nil || code != 0 {
			return fmt.Errorf("launchctl bootstrap %s %s failed (exit %d)", sel.domain, sel.plist, code)
		}
		if now, err := probeRelay(sel); err != nil {
			return err
		} else if now != relayLoaded {
			return fmt.Errorf("the %s relay is not loaded after bootstrap", mode)
		}
		return h.relayStarted()
	}
	switch {
	case !same:
		if err := stopRelay(sel, selState, false); err != nil {
			return fail(err)
		}
		if err := os.MkdirAll(h.p.logDir, 0o755); err != nil {
			return fail(err)
		}
		if err := writeRelayPlist(sel, want, h.p.daemonStaged); err != nil {
			return fail(err)
		}
		if err := bootstrap(); err != nil {
			return fail(err)
		}
		state, detail = hostUpdated, mode+" relay plist rewritten and reloaded: "+sel.plist
		if fresh {
			state, detail = hostInstalled, mode+" relay installed and started: "+sel.plist
		}
	case selState != relayLoaded:
		if err := bootstrap(); err != nil {
			return fail(err)
		}
		state, detail = hostUpdated, mode+" relay was not loaded; started "+sel.target()
	case h.binaryChanged || h.restartIsPending():
		if code, err := hostRun(sel.sudo, hostSetupLaunchctl, "kickstart", "-k", sel.target()); err != nil || code != 0 {
			return fail(fmt.Errorf("launchctl kickstart -k %s failed (exit %d)", sel.target(), code))
		}
		if err := h.relayStarted(); err != nil {
			return fail(err)
		}
		state, detail = hostUpdated, mode+" relay restarted on the new binary: "+sel.target()
	case otherHere:
		state, detail = hostUpdated, "removed the "+other.mode+" relay; "+mode+" relay "+sel.target()
	}
	// Final invariant: never report success with two relays loaded.
	for _, d := range []relayDomain{agent, daemon} {
		now, err := probeRelay(d)
		if err != nil {
			return fail(err)
		}
		if d.mode == other.mode && now == relayLoaded {
			h.add(name, hostFailed, "both the agent and the daemon relay are loaded; the "+other.mode+" relay must be removed")
			return mode
		}
	}
	h.add(name, state, detail)
	return mode
}

// restartRelay restarts whichever relay service is loaded, after a rollback.
func (h *hostSetup) restartRelay() {
	const name = "relay-service"
	if hostSetupGOOS != "darwin" {
		h.add(name, hostCurrent, "not macOS: restart `tt relay` with this host's service manager")
		return
	}
	agent, daemon := h.domains()
	restarted := []string{}
	for _, d := range []relayDomain{agent, daemon} {
		state, err := probeRelay(d)
		if err != nil {
			h.add(name, hostFailed, err.Error())
			return
		}
		if state != relayLoaded {
			continue
		}
		if code, err := hostRun(d.sudo, hostSetupLaunchctl, "kickstart", "-k", d.target()); err != nil || code != 0 {
			h.add(name, hostFailed, fmt.Sprintf("launchctl kickstart -k %s failed (exit %d)", d.target(), code))
			return
		}
		restarted = append(restarted, d.target())
	}
	if len(restarted) == 0 {
		h.add(name, hostFailed, "no relay service is loaded, so none was restarted; run tt host setup")
		return
	}
	if err := h.relayStarted(); err != nil {
		h.add(name, hostFailed, err.Error())
		return
	}
	h.add(name, hostUpdated, "restarted "+strings.Join(restarted, ", "))
}

// ---- doctor ----

// relayRunning reports whether a relay holds the exclusive lock tt relay
// keeps while it runs. It never creates the lock file.
func relayRunning() bool {
	lock, err := os.Open(filepath.Join(relayDir(), "relay.lock"))
	if err != nil {
		return false
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return true
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return false
}

// waitForRelay reports whether the relay is running, waiting up to wait for
// one that was just restarted. The probe takes the relay's own lock when it
// is free, and a relay that finds the lock taken exits as "already running"
// and is not retried by launchd for 15 seconds. So the probe never runs while
// a restarted relay is still starting: the first probe comes only after
// hostSetupRelayProbe, long after a starting relay has locked, and later
// probes are that far apart. A probe of a running relay takes no lock at all.
// launchctl's own state is not used because it says a process exists, not
// that the relay holds its lock, and it is absent off macOS.
func waitForRelay(wait time.Duration) bool {
	if wait <= 0 {
		return relayRunningProbe()
	}
	deadline := time.Now().Add(wait)
	for {
		pause := hostSetupRelayProbe
		if left := time.Until(deadline); left < pause {
			pause = left
		}
		time.Sleep(pause)
		if relayRunningProbe() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
}

func (h *hostSetup) doctor(w io.Writer) {
	// Only a relay this run restarted is given time to come up.
	relayWait := time.Duration(0)
	if h.relayRestarted {
		relayWait = hostSetupRelayWait
	}
	// The hub configuration may have been written by this run.
	if failed := runDoctor(w, readEnv(), relayWait); len(failed) > 0 {
		h.add("doctor", hostFailed, "failed: "+strings.Join(failed, ", "))
		return
	}
	h.add("doctor", hostCurrent, "tmux, runtimes, hub and relay")
}
