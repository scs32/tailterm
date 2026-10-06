package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/adapters"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// No test in this package may reach the real launchctl or sudo: until a
// sandbox installs its fakes, both commands name a path that does not exist.
func init() {
	hostSetupLaunchctl = "/nonexistent/tailterm-test/launchctl"
	hostSetupSudo = "/nonexistent/tailterm-test/sudo"
}

const fakeLaunchctl = `#!/bin/sh
# Fake launchctl: logs its arguments and keeps loaded services as files.
dir="$(dirname "$0")/state"
echo "$*" >> "$(dirname "$0")/calls.log"
key() { printf '%s' "$1" | tr '/' '_'; }
case "$1" in
print)
	[ -f "$dir/print-exit" ] && exit "$(cat "$dir/print-exit")"
	# A boot-out still in progress: the service goes after a few more probes.
	if [ -f "$dir/leaving-$(key "$2")" ]; then
		n=$(cat "$dir/leaving-$(key "$2")")
		if [ "$n" -le 1 ]; then rm -f "$dir/leaving-$(key "$2")" "$dir/loaded-$(key "$2")"; else echo $((n - 1)) > "$dir/leaving-$(key "$2")"; fi
	fi
	[ -f "$dir/loaded-$(key "$2")" ] && exit 0
	exit 113;;
bootstrap)
	touch "$dir/loaded-$(key "$2/com.tailterm.inbox-relay")"
	[ -f "$dir/bootstrap-also-loads" ] && touch "$dir/loaded-$(key "$(cat "$dir/bootstrap-also-loads")")"
	exit 0;;
bootout)
	case "$(cat "$dir/bootout-mode" 2>/dev/null)" in
	fail-loaded) exit 5;;
	fail-absent) rm -f "$dir/loaded-$(key "$2")"; exit 5;;
	fail-slow) echo 3 > "$dir/leaving-$(key "$2")"; exit 5;;
	stuck) exit 0;;
	esac
	rm -f "$dir/loaded-$(key "$2")"
	exit 0;;
kickstart)
	[ -f "$dir/kickstart-exit" ] && exit "$(cat "$dir/kickstart-exit")"
	exit 0;;
esac
exit 64
`

const fakeSudo = `#!/bin/sh
# Fake sudo: logs, refuses the commands named in state/sudo-refuse, installs
# without changing ownership and otherwise runs its arguments.
dir="$(dirname "$0")/state"
echo "sudo $*" >> "$(dirname "$0")/calls.log"
if [ -f "$dir/sudo-refuse" ] && [ "$(cat "$dir/sudo-refuse")" = "$1" ]; then exit 1; fi
if [ "$1" = install ]; then cp "$8" "$9"; exit $?; fi
exec "$@"
`

type hostSandbox struct {
	t          *testing.T
	root, home string
	p          hostPaths
	hubURL     string
	hubMu      sync.Mutex
	hubPaths   []string
}

// newHostSandbox points every path host setup uses, the launchctl and sudo
// commands, tmux, the runtimes and the hub at fakes under t.TempDir(), and
// fails the test if any resolved path is outside it.
func newHostSandbox(t *testing.T, homeName string) *hostSandbox {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &hostSandbox{t: t, root: root, home: filepath.Join(root, homeName)}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hubMu.Lock()
		s.hubPaths = append(s.hubPaths, r.URL.Path)
		s.hubMu.Unlock()
		if r.URL.Path != "/v1/whoami" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"node":"sandbox","user":"tester"}`)
	}))
	t.Cleanup(hub.Close)
	s.hubURL = hub.URL
	for name, value := range map[string]string{
		"HOME": s.home, "CODEX_HOME": filepath.Join(s.home, ".codex"), "CLAUDE_CONFIG_DIR": filepath.Join(s.home, ".claude"),
		"TAILTERM_RELAY_STATE": filepath.Join(root, "relay-state"), "TAILTERM_HOST_SETUP_ROOT": filepath.Join(root, "sysroot"),
		spawn.EnvHub: hub.URL, "TAILTERM_TOKEN": "", "TAILTERM_RUN": "", spawn.EnvTask: "", spawn.EnvAgent: "", spawn.EnvAgentName: "", spawn.EnvSession: "",
	} {
		t.Setenv(name, value)
	}
	fake := filepath.Join(root, "fake")
	s.write(filepath.Join(fake, "state", ".keep"), "", 0o600)
	s.write(filepath.Join(fake, "launchctl"), fakeLaunchctl, 0o755)
	s.write(filepath.Join(fake, "sudo"), fakeSudo, 0o755)
	s.write(filepath.Join(fake, "tmux"), "#!/bin/sh\necho 'tmux 3.5a'\n", 0o755)
	old := struct {
		launchctl, sudo, goos, tmux string
		runtimes                    func() []string
		uid                         func() int
		probe                       func() bool
		relayWait, stopWait, poll   time.Duration
		relayProbe                  time.Duration
	}{hostSetupLaunchctl, hostSetupSudo, hostSetupGOOS, spawn.Tmux, hostSetupRuntimes, hostSetupUID, relayRunningProbe, hostSetupRelayWait, hostSetupStopWait, hostSetupPoll, hostSetupRelayProbe}
	t.Cleanup(func() {
		hostSetupLaunchctl, hostSetupSudo, hostSetupGOOS, spawn.Tmux, hostSetupRuntimes = old.launchctl, old.sudo, old.goos, old.tmux, old.runtimes
		hostSetupUID, relayRunningProbe = old.uid, old.probe
		hostSetupRelayWait, hostSetupStopWait, hostSetupPoll, hostSetupRelayProbe = old.relayWait, old.stopWait, old.poll, old.relayProbe
	})
	hostSetupLaunchctl, hostSetupSudo, hostSetupGOOS, spawn.Tmux = filepath.Join(fake, "launchctl"), filepath.Join(fake, "sudo"), "darwin", filepath.Join(fake, "tmux")
	hostSetupRuntimes = func() []string { return []string{"claude", "codex"} }
	hostSetupRelayWait, hostSetupStopWait, hostSetupPoll = 0, 300*time.Millisecond, 10*time.Millisecond
	s.p, err = newHostPaths()
	if err != nil {
		t.Fatal(err)
	}
	paths := s.p.resolved()
	paths["relayLock"] = resolvePath(s.p.relayLock)
	for name, path := range paths {
		if !strings.HasPrefix(path, root+string(filepath.Separator)) {
			t.Fatalf("host setup path %s = %s is outside the sandbox %s", name, path, root)
		}
	}
	return s
}

func (s *hostSandbox) write(path, content string, mode os.FileMode) string {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		s.t.Fatal(err)
	}
	return path
}

// artifact writes a distinct runnable stand-in for a tt binary.
func (s *hostSandbox) artifact(name string) string {
	return s.write(filepath.Join(s.root, "artifacts", name), "#!/bin/sh\n# "+name+"\nprintf '%s' \"$*\" > \"$TT_MARK\"\n", 0o755)
}

func (s *hostSandbox) state(name, value string) {
	s.write(filepath.Join(s.root, "fake", "state", name), value, 0o600)
}

func (s *hostSandbox) clearState(name string) {
	os.Remove(filepath.Join(s.root, "fake", "state", name))
}

// calls returns the launchctl and sudo calls logged so far and clears the log.
func (s *hostSandbox) calls() []string {
	path := filepath.Join(s.root, "fake", "calls.log")
	raw, _ := os.ReadFile(path)
	os.Remove(path)
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func onlyPrint(calls []string) bool {
	for _, c := range calls {
		if !strings.HasPrefix(c, "print ") {
			return false
		}
	}
	return true
}

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

func callIndex(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// holdRelayLock stands in for a running relay.
func (s *hostSandbox) holdRelayLock() func() {
	s.t.Helper()
	s.write(s.p.relayLock, "", 0o600)
	lock, err := os.OpenFile(s.p.relayLock, os.O_RDWR, 0o600)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		s.t.Fatal(err)
	}
	release := func() { lock.Close() }
	s.t.Cleanup(release)
	return release
}

// run runs tt host setup with args and returns stdout plus the error text,
// and the exit code main would use.
func (s *hostSandbox) run(args ...string) (string, int) {
	s.t.Helper()
	out, err := captureStdout(s.t, func() error { return cmdHost(append([]string{"setup"}, args...)) })
	if err == nil {
		return out, 0
	}
	code := 1
	var coded *exitError
	if errors.As(err, &coded) {
		code = coded.code
	}
	return out + "\n" + err.Error(), code
}

func (s *hostSandbox) mustRun(args ...string) string {
	s.t.Helper()
	out, code := s.run(args...)
	if code != 0 {
		s.t.Fatalf("tt host setup %v exited %d:\n%s", args, code, out)
	}
	return out
}

func (s *hostSandbox) hash(path string) string {
	s.t.Helper()
	sum, err := fileSHA256(path)
	if err != nil {
		s.t.Fatal(err)
	}
	return sum
}

// tree is every file host setup could have touched: content hash, mode and
// modification time.
func (s *hostSandbox) tree() map[string]string {
	s.t.Helper()
	out := map[string]string{}
	for _, dir := range []string{s.home, filepath.Join(s.root, "sysroot"), filepath.Join(s.root, "relay-state"), filepath.Join(s.root, "dotfiles")} {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			info, err := os.Lstat(path)
			if err != nil {
				s.t.Fatal(err)
			}
			entry := info.Mode().String()
			if info.Mode().IsRegular() {
				entry += " " + s.hash(path) + " " + info.ModTime().Format(time.RFC3339Nano)
			}
			out[path] = entry
			return nil
		})
	}
	return out
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (s *hostSandbox) agentTarget() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), hostRelayLabel)
}

// ttCommands returns the commands of the tt hook entries for one event.
func ttCommands(t *testing.T, path, event, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, raw)
	}
	out := []string{}
	for _, group := range doc.Hooks[event] {
		for _, h := range group.Hooks {
			if isTTHook(h.Command, name) {
				if group.Matcher != "" {
					t.Fatalf("%s: a tt %s entry remains inside matcher group %q", path, event, group.Matcher)
				}
				out = append(out, h.Command)
			}
		}
	}
	return out
}

func (s *hostSandbox) requireCanonicalHooks() {
	s.t.Helper()
	for _, ev := range claudeHookEvents {
		if got := ttCommands(s.t, s.p.claudeSettings, ev.event, ev.name); len(got) != 1 || got[0] != adapters.HookCommand(s.p.install, ev.name) {
			s.t.Fatalf("Claude %s tt entries = %q", ev.event, got)
		}
	}
	if got := ttCommands(s.t, s.p.codexHooks, "Stop", "stop"); len(got) != 1 || got[0] != adapters.HookCommand(s.p.install, "stop") {
		s.t.Fatalf("Codex Stop tt entries = %q", got)
	}
}

func decodeJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

// withoutTTHooks removes every tt hook entry, and what that leaves empty,
// from a decoded document, so the rest can be compared with the original.
func withoutTTHooks(doc map[string]any, events []hookEvent) map[string]any {
	hooks, _ := doc["hooks"].(map[string]any)
	for _, ev := range events {
		groups, _ := hooks[ev.event].([]any)
		kept := []any{}
		for _, g := range groups {
			group := g.(map[string]any)
			inner, _ := group["hooks"].([]any)
			rest := []any{}
			for _, h := range inner {
				if command, _ := h.(map[string]any)["command"].(string); !isTTHook(command, ev.name) {
					rest = append(rest, h)
				}
			}
			if len(rest) > 0 {
				group["hooks"] = rest
				kept = append(kept, group)
			}
		}
		if len(kept) > 0 {
			hooks[ev.event] = kept
		} else {
			delete(hooks, ev.event)
		}
	}
	return doc
}

func TestHostSetupFreshHost(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	source := s.artifact("tt-v1")
	out := s.mustRun("--from", source)
	info, err := os.Stat(s.p.install)
	if err != nil || info.Mode().Perm()&0o111 == 0 || s.hash(s.p.install) != s.hash(source) {
		t.Fatalf("installed tt is wrong: %v %v\n%s", info, err, out)
	}
	if exists(s.p.rollback) || exists(s.p.staged) || exists(s.p.rollbackTmp) {
		t.Fatal("a fresh host got a rollback or staging file")
	}
	s.requireCanonicalHooks()
	plist, err := os.ReadFile(s.p.agentPlist)
	if err != nil || !strings.Contains(string(plist), "<string>"+s.p.install+"</string>") || strings.Contains(string(plist), "UserName") {
		t.Fatalf("agent plist: %v\n%s", err, plist)
	}
	if info, _ := os.Stat(s.p.agentPlist); info.Mode().Perm() != 0o600 {
		t.Fatalf("agent plist mode %v", info.Mode())
	}
	calls := s.calls()
	if !hasCall(calls, fmt.Sprintf("bootstrap gui/%d %s", os.Getuid(), s.p.agentPlist)) {
		t.Fatalf("no agent bootstrap in %q", calls)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "sudo ") {
			t.Fatalf("agent mode used sudo: %q", calls)
		}
	}
	for _, want := range []string{"installed  binary", "installed  codex-hooks", "installed  claude-hooks", "installed  relay-service", "current    doctor", "OK relay running", "host setup: ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if len(s.hubPaths) == 0 {
		t.Fatal("doctor did not ask the hub")
	}
	for _, p := range s.hubPaths {
		if p != "/v1/whoami" {
			t.Fatalf("host setup asked the hub for %s", p)
		}
	}
}

func TestHostSetupUpdate(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	v1, v2 := s.artifact("tt-v1"), s.artifact("tt-v2")
	s.mustRun("--from", v1)
	s.calls()
	out := s.mustRun("--from", v2)
	if s.hash(s.p.install) != s.hash(v2) || s.hash(s.p.rollback) != s.hash(v1) {
		t.Fatalf("after the update tt or tt.previous has the wrong hash:\n%s", out)
	}
	if exists(s.p.staged) || exists(s.p.rollbackTmp) {
		t.Fatal("a staging file remains")
	}
	if !strings.Contains(out, s.hash(v1)) || !strings.Contains(out, s.hash(v2)) || !strings.Contains(out, s.p.rollback) {
		t.Fatalf("output does not name both hashes and the rollback path:\n%s", out)
	}
	if calls := s.calls(); !hasCall(calls, "kickstart -k "+s.agentTarget()) || callIndex(calls, "bootstrap") >= 0 || callIndex(calls, "bootout") >= 0 {
		t.Fatalf("update calls = %q", calls)
	}
}

func TestHostSetupAlreadyCurrent(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	v1, v2 := s.artifact("tt-v1"), s.artifact("tt-v2")
	s.mustRun("--from", v1)
	s.mustRun("--from", v2)
	s.calls()
	before := s.tree()
	out := s.mustRun("--from", v2)
	if after := s.tree(); !reflect.DeepEqual(before, after) {
		t.Fatalf("a re-run changed files:\nbefore %v\nafter  %v", before, after)
	}
	if s.hash(s.p.rollback) != s.hash(v1) {
		t.Fatal("a re-run replaced the rollback copy")
	}
	if calls := s.calls(); len(calls) == 0 || !onlyPrint(calls) {
		t.Fatalf("a re-run did more than print: %q", calls)
	}
	for _, step := range []string{"binary", "hub-config", "codex-hooks", "claude-hooks", "relay-service", "doctor"} {
		if !strings.Contains(out, fmt.Sprintf("%-9s  %s", "current", step)) {
			t.Fatalf("step %s is not current:\n%s", step, out)
		}
	}
	// Running the installed binary against itself is current too.
	if out := s.mustRun("--from", s.p.install); !strings.Contains(out, "current    binary") {
		t.Fatalf("installing tt from itself:\n%s", out)
	}
}

const (
	userClaudeSettings = `{
  "model": "opus",
  "big": 12345678901234567890,
  "permissions": {"allow": ["Bash(ls:*)"], "deny": []},
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "say <done> & exit", "timeout": 5}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}]
  }
}
`
	userCodexHooks = `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"notify-me"}]}],"PreToolUse":[{"hooks":[{"type":"command","command":"other-tool check"}]}]},"version":1}`
)

func TestHostSetupMergesHooks(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	s.write(s.p.claudeSettings, userClaudeSettings, 0o640)
	s.write(s.p.codexHooks, userCodexHooks, 0o600)
	wantClaude, wantCodex := decodeJSONFile(t, s.p.claudeSettings), decodeJSONFile(t, s.p.codexHooks)
	out := s.mustRun("--from", s.artifact("tt-v1"))
	s.requireCanonicalHooks()
	if got := withoutTTHooks(decodeJSONFile(t, s.p.claudeSettings), claudeHookEvents); !reflect.DeepEqual(got, wantClaude) {
		t.Fatalf("Claude settings lost or changed user content:\n got %v\nwant %v", got, wantClaude)
	}
	if got := withoutTTHooks(decodeJSONFile(t, s.p.codexHooks), codexHookEvents); !reflect.DeepEqual(got, wantCodex) {
		t.Fatalf("Codex hooks lost or changed user content:\n got %v\nwant %v", got, wantCodex)
	}
	raw, _ := os.ReadFile(s.p.claudeSettings)
	if !strings.Contains(string(raw), "say <done> & exit") || !strings.Contains(string(raw), "12345678901234567890") {
		t.Fatalf("Claude settings were re-encoded lossily:\n%s", raw)
	}
	if info, _ := os.Stat(s.p.claudeSettings); info.Mode().Perm() != 0o640 {
		t.Fatalf("Claude settings mode changed to %v", info.Mode())
	}
	for path, original := range map[string]string{s.p.claudeSettings: userClaudeSettings, s.p.codexHooks: userCodexHooks} {
		if backup, err := os.ReadFile(path + hostBackupSuffix); err != nil || string(backup) != original {
			t.Fatalf("%s backup: %v\n%s", path, err, backup)
		}
	}
	if !strings.Contains(out, "updated    claude-hooks") || !strings.Contains(out, "updated    codex-hooks") {
		t.Fatalf("hook steps not reported as updated:\n%s", out)
	}
	before := s.tree()
	s.mustRun("--from", s.artifact("tt-v1"))
	if after := s.tree(); !reflect.DeepEqual(before, after) {
		t.Fatalf("a second run changed files:\nbefore %v\nafter  %v", before, after)
	}
}

// The tool-call ledger events: each gets one unmatched tt hook tool group
// beside the user's own matched PreToolUse entry, a tt entry a matcher would
// restrict is moved out of it, every tool entry carries the timeout (one
// installed without it, or with another, is upgraded in place), the four
// older hooks get no timeout, and a second run changes nothing.
func TestHostSetupInstallsToolHooks(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	s.write(s.p.claudeSettings, `{"hooks":{
		"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"guard.sh"},{"type":"command","command":"/old/tt hook tool"}]}],
		"PostToolUse":[{"matcher":"Write","hooks":[{"type":"command","command":"tt hook tool"}]}],
		"PostToolUseFailure":[{"note":"mine","hooks":[{"type":"command","command":"`+adapters.HookCommand(s.p.install, "tool")+`","async":false},{"type":"command","command":"after.sh","timeout":60}]}],
		"Stop":[{"hooks":[{"type":"command","command":"`+adapters.HookCommand(s.p.install, "stop")+`"}]}]
	}}`, 0o600)
	out := s.mustRun("--from", s.artifact("tt-v1"))
	s.requireCanonicalHooks()
	tool := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": adapters.HookCommand(s.p.install, "tool"), "timeout": json.Number("5")}}}
	plain := func(name string) []any {
		return []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": adapters.HookCommand(s.p.install, name)}}}}
	}
	hooks := decodeJSONFile(t, s.p.claudeSettings)["hooks"].(map[string]any)
	for event, want := range map[string][]any{
		"PreToolUse":  {map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "guard.sh"}}}, tool},
		"PostToolUse": {tool},
		"PostToolUseFailure": {map[string]any{"note": "mine", "hooks": []any{
			map[string]any{"type": "command", "command": adapters.HookCommand(s.p.install, "tool"), "async": false, "timeout": json.Number("5")},
			map[string]any{"type": "command", "command": "after.sh", "timeout": json.Number("60")}}}},
		"SessionStart":     plain("session-start"),
		"UserPromptSubmit": plain("prompt"),
		"Stop":             plain("stop"),
		"Notification":     plain("notification"),
	} {
		if got := hooks[event]; !reflect.DeepEqual(got, want) {
			t.Errorf("%s after setup:\n got %v\nwant %v", event, got, want)
		}
	}
	if len(hooks) != len(claudeHookEvents) {
		t.Errorf("settings wire %d events; want %d", len(hooks), len(claudeHookEvents))
	}
	if !strings.Contains(out, "updated    claude-hooks") {
		t.Fatalf("tool hooks not reported as updated:\n%s", out)
	}
	before := s.tree()
	if out := s.mustRun("--from", s.artifact("tt-v1")); !strings.Contains(out, "current    claude-hooks") {
		t.Fatalf("a second run did not report the hooks current:\n%s", out)
	}
	if after := s.tree(); !reflect.DeepEqual(before, after) {
		t.Fatalf("a second run changed files:\nbefore %v\nafter  %v", before, after)
	}
}

func TestHostSetupHookStaleEntries(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	s.write(s.p.codexHooks, `{"hooks":{"Stop":[
		{"note":"keep me","hooks":[{"type":"command","command":"tt hook stop","timeout":7},{"type":"command","command":"user-sibling"}]},
		{"hooks":[{"type":"command","command":"/old/place/tt hook stop"}]},
		{"matcher":"x","hooks":[{"type":"command","command":"/elsewhere/tt hook stop"},{"type":"command","command":"matcher-sibling"}]}
	]}}`, 0o600)
	// Claude: the only tt Stop entry sits in a matcher group, so it does not count.
	s.write(s.p.claudeSettings, `{"hooks":{"Stop":[{"matcher":"m","hooks":[{"type":"command","command":"tt hook stop"}]}]}}`, 0o600)
	out := s.mustRun("--from", s.artifact("tt-v1"))
	s.requireCanonicalHooks()
	canonical := adapters.HookCommand(s.p.install, "stop")
	want := map[string]any{"hooks": map[string]any{"Stop": []any{
		map[string]any{"note": "keep me", "hooks": []any{
			map[string]any{"type": "command", "command": canonical, "timeout": json.Number("7")},
			map[string]any{"type": "command", "command": "user-sibling"}}},
		map[string]any{"matcher": "x", "hooks": []any{map[string]any{"type": "command", "command": "matcher-sibling"}}},
	}}}
	if got := decodeJSONFile(t, s.p.codexHooks); !reflect.DeepEqual(got, want) {
		t.Fatalf("Codex hooks after stale entries:\n got %v\nwant %v", got, want)
	}
	stop := decodeJSONFile(t, s.p.claudeSettings)["hooks"].(map[string]any)["Stop"].([]any)
	if len(stop) != 1 || !reflect.DeepEqual(stop[0], map[string]any{"hooks": []any{map[string]any{"type": "command", "command": canonical}}}) {
		t.Fatalf("Claude Stop after a matcher-only tt entry = %v", stop)
	}
	if !strings.Contains(out, "updated    codex-hooks") {
		t.Fatalf("stale Codex entries not reported as updated:\n%s", out)
	}
	if !codexStopHookInstalled() {
		t.Fatal("the canonical Codex hook is not recognised")
	}
}

func TestHostSetupHookRejects(t *testing.T) {
	for _, c := range []struct{ name, content, want string }{
		{"invalid JSON", `{"hooks": {`, "not valid JSON"},
		{"trailing content", `{"hooks": {}} {"more": 1}`, "trailing content"},
		{"top level array", `[{"hooks": {}}]`, "top level is not a JSON object"},
		{"hooks a string", `{"hooks": "none"}`, "hooks is not an object"},
		{"event an object", `{"hooks": {"Stop": {"hooks": []}}}`, "hooks.Stop is not an array"},
		{"group a string", `{"hooks": {"Stop": ["tt hook stop"]}}`, "hooks.Stop[0] is not an object"},
		{"inner hooks an object", `{"hooks": {"Stop": [{"hooks": {}}]}}`, "hooks.Stop[0].hooks is not an array"},
		{"command a number", `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": 7}]}]}}`, "hooks.Stop[0].hooks[0].command is not a string"},
	} {
		for _, runtime := range []string{"codex", "claude"} {
			t.Run(c.name+"/"+runtime, func(t *testing.T) {
				s := newHostSandbox(t, "home")
				s.holdRelayLock()
				path := s.p.codexHooks
				if runtime == "claude" {
					path = s.p.claudeSettings
				}
				s.write(path, c.content, 0o600)
				before := s.tree()[path]
				out, code := s.run("--from", s.artifact("tt-v1"))
				if code != 1 || !strings.Contains(out, c.want) || !strings.Contains(out, "failed     "+runtime+"-hooks") {
					t.Fatalf("exit %d:\n%s", code, out)
				}
				if raw, _ := os.ReadFile(path); string(raw) != c.content || s.tree()[path] != before {
					t.Fatalf("the rejected file was changed: %s", raw)
				}
				if exists(path+hostBackupSuffix) || exists(path+".tmp") {
					t.Fatal("a rejected file got a backup or temporary file")
				}
				if runtime == "codex" { // tt hooks codex --install is as strict
					if _, err := captureStdout(t, func() error { return installCodexStopHook("/usr/local/bin/tt") }); err == nil || !strings.Contains(err.Error(), c.want) {
						t.Fatalf("tt hooks codex --install accepted it: %v", err)
					}
					if raw, _ := os.ReadFile(path); string(raw) != c.content || exists(path+hostBackupSuffix) {
						t.Fatal("tt hooks codex --install changed a rejected file")
					}
				}
			})
		}
	}
}

func TestHostSetupHookSymlink(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	target := s.write(filepath.Join(s.root, "dotfiles", "claude-settings.json"), userClaudeSettings, 0o600)
	if err := os.MkdirAll(filepath.Dir(s.p.claudeSettings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, s.p.claudeSettings); err != nil {
		t.Fatal(err)
	}
	var report hostReport
	out, _ := s.run("--check", "--json", "--from", s.artifact("tt-v1"))
	if err := json.Unmarshal([]byte(out[:strings.LastIndex(out, "}")+1]), &report); err != nil || report.Paths["claudeSettings"] != target {
		t.Fatalf("--check --json does not report the resolved link target: %v\n%s", err, out)
	}
	s.mustRun("--from", s.artifact("tt-v1"))
	if info, err := os.Lstat(s.p.claudeSettings); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("settings.json is no longer a symlink: %v", err)
	}
	if got := ttCommands(t, target, "Stop", "stop"); len(got) != 1 {
		t.Fatalf("the link target was not updated: %q", got)
	}
	if backup, err := os.ReadFile(target + hostBackupSuffix); err != nil || string(backup) != userClaudeSettings {
		t.Fatalf("no backup beside the link target: %v", err)
	}

	// A dangling link fails without writing anything.
	if err := os.Remove(s.p.codexHooks); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(s.root, "dotfiles", "missing.json")
	if err := os.Symlink(missing, s.p.codexHooks); err != nil {
		t.Fatal(err)
	}
	before := s.tree()
	out, code := s.run("--from", s.artifact("tt-v1"))
	if code != 1 || !strings.Contains(out, "failed     codex-hooks") || !strings.Contains(out, "target does not exist") {
		t.Fatalf("dangling link: exit %d\n%s", code, out)
	}
	if exists(missing) || !reflect.DeepEqual(before, s.tree()) {
		t.Fatal("a dangling link was written through")
	}
}

func TestHostSetupHomeWithSpaces(t *testing.T) {
	s := newHostSandbox(t, "ho me's")
	s.holdRelayLock()
	s.mustRun("--from", s.artifact("tt-v1"))
	s.requireCanonicalHooks()
	for _, ev := range claudeHookEvents {
		command := ttCommands(t, s.p.claudeSettings, ev.event, ev.name)[0]
		if !strings.HasPrefix(command, "'") {
			t.Fatalf("command is not quoted: %s", command)
		}
		mark := filepath.Join(s.root, "mark-"+ev.name)
		cmd := exec.Command("/bin/sh", "-c", command)
		cmd.Env = []string{"TT_MARK=" + mark, "PATH=/usr/bin:/bin"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sh -c %s: %v\n%s", command, err, out)
		}
		if got, _ := os.ReadFile(mark); string(got) != "hook "+ev.name {
			t.Fatalf("sh -c %s ran the installed tt with %q", command, got)
		}
	}
	if !codexStopHookInstalled() {
		t.Fatal("a quoted Codex hook is not recognised")
	}
	plist, _ := os.ReadFile(s.p.agentPlist)
	if !strings.Contains(string(plist), "<string>"+strings.ReplaceAll(s.p.install, "'", "&#39;")+"</string>") {
		t.Fatalf("plist does not carry the escaped install path:\n%s", plist)
	}
	s.calls()
	before := s.tree()
	out := s.mustRun("--from", s.artifact("tt-v1"))
	if !reflect.DeepEqual(before, s.tree()) || !onlyPrint(s.calls()) {
		t.Fatalf("a re-run in a home with spaces was not a no-op:\n%s", out)
	}
}

func TestHostSetupCheck(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	v1, v2 := s.artifact("tt-v1"), s.artifact("tt-v2")
	before := s.tree()
	out, code := s.run("--check", "--from", v1)
	if code != 1 {
		t.Fatalf("--check on a fresh home exited %d:\n%s", code, out)
	}
	for _, step := range []string{"binary", "codex-hooks", "claude-hooks", "relay-service"} {
		if !strings.Contains(out, "missing    "+step) {
			t.Fatalf("fresh --check does not list %s as missing:\n%s", step, out)
		}
	}
	if !reflect.DeepEqual(before, s.tree()) || exists(s.home) {
		t.Fatal("--check changed a fresh home")
	}
	if calls := s.calls(); len(calls) == 0 || !onlyPrint(calls) {
		t.Fatalf("fresh --check calls = %q", calls)
	}

	s.mustRun("--from", v1)
	// An outdated host: a new binary and a hook that lost its absolute path.
	s.write(s.p.codexHooks, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"tt hook stop"}]}]}}`, 0o600)
	s.calls()
	before = s.tree()
	out, code = s.run("--check", "--from", v2)
	if code != 1 || !strings.Contains(out, "outdated   binary") || !strings.Contains(out, "outdated   codex-hooks") || !strings.Contains(out, "current    claude-hooks") || !strings.Contains(out, "current    relay-service") {
		t.Fatalf("--check on an outdated home exited %d:\n%s", code, out)
	}
	if !reflect.DeepEqual(before, s.tree()) {
		t.Fatal("--check changed an outdated home")
	}
	if calls := s.calls(); len(calls) == 0 || !onlyPrint(calls) {
		t.Fatalf("outdated --check calls = %q", calls)
	}

	s.mustRun("--from", v2)
	s.calls()
	before = s.tree()
	if out, code := s.run("--check", "--from", v2); code != 0 || !strings.Contains(out, "host check: ok") {
		t.Fatalf("--check on a current home exited %d:\n%s", code, out)
	}
	if !reflect.DeepEqual(before, s.tree()) || !onlyPrint(s.calls()) {
		t.Fatal("--check changed a current home")
	}

	for _, args := range [][]string{{"--check", "--rollback"}, {"--service", "cron"}, {"extra"}, {"--bogus"}, {"--rollback", "--from", v1}} {
		if out, code := s.run(args...); code != 2 {
			t.Fatalf("%v exited %d:\n%s", args, code, out)
		}
	}
	if err := cmdHost([]string{"teardown"}); err == nil || !strings.Contains(err.Error(), "usage: tt host setup") {
		t.Fatalf("tt host teardown: %v", err)
	}
}

func TestHostSetupService(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	v1 := s.artifact("tt-v1")
	s.mustRun("--from", v1)
	s.calls()
	launchctl, agent, daemon := hostSetupLaunchctl, s.agentTarget(), "system/"+hostRelayLabel

	out := s.mustRun("--from", v1, "--service", "daemon")
	calls := s.calls()
	bootout, install, bootstrap := callIndex(calls, "bootout "+agent), callIndex(calls, "sudo install -o root -g wheel -m 0644 "), callIndex(calls, "sudo "+launchctl+" bootstrap system "+s.p.daemonPlist)
	if bootout < 0 || install < bootout || bootstrap < install || callIndex(calls[bootout+1:], "print "+agent) != 0 {
		t.Fatalf("switch to daemon calls = %q", calls)
	}
	if exists(s.p.agentPlist) {
		t.Fatal("the agent plist remains after switching to the daemon")
	}
	plist, err := os.ReadFile(s.p.daemonPlist)
	if err != nil || !strings.Contains(string(plist), "<key>UserName</key>") || !strings.Contains(string(plist), "<key>HOME</key>") {
		t.Fatalf("daemon plist: %v\n%s", err, plist)
	}
	if !strings.Contains(out, "relay-service") || !strings.Contains(out, "daemon relay installed") {
		t.Fatalf("output:\n%s", out)
	}

	// With no flag the installed mode is kept.
	if out := s.mustRun("--from", v1); !strings.Contains(out, "current    relay-service  daemon relay") || !onlyPrint(s.calls()) {
		t.Fatalf("a re-run did not keep the daemon:\n%s", out)
	}
	// A daemon restart goes through sudo.
	s.mustRun("--from", s.artifact("tt-v2"))
	if calls := s.calls(); !hasCall(calls, "sudo "+launchctl+" kickstart -k "+daemon) {
		t.Fatalf("daemon update calls = %q", calls)
	}

	s.mustRun("--from", s.artifact("tt-v2"), "--service", "agent")
	calls = s.calls()
	bootout, remove, bootstrap := callIndex(calls, "sudo "+launchctl+" bootout "+daemon), callIndex(calls, "sudo rm -f "+s.p.daemonPlist), callIndex(calls, fmt.Sprintf("bootstrap gui/%d %s", os.Getuid(), s.p.agentPlist))
	if bootout < 0 || remove < bootout || bootstrap < remove {
		t.Fatalf("switch back to agent calls = %q", calls)
	}
	if exists(s.p.daemonPlist) || !exists(s.p.agentPlist) {
		t.Fatal("after switching back the plists are wrong")
	}
}

func TestHostSetupServiceFailures(t *testing.T) {
	type sandbox = *hostSandbox
	agentUp := func(t *testing.T) (sandbox, string) {
		s := newHostSandbox(t, "home")
		s.holdRelayLock()
		v1 := s.artifact("tt-v1")
		s.mustRun("--from", v1)
		s.calls()
		return s, v1
	}
	t.Run("bootout fails while the service is still leaving", func(t *testing.T) {
		s, v1 := agentUp(t)
		s.state("bootout-mode", "fail-slow")
		hostSetupStopWait = time.Hour // ends when the service is gone, however slow the host
		out := s.mustRun("--from", v1, "--service", "daemon")
		calls := s.calls()
		bootout, bootstrap := callIndex(calls, "bootout "+s.agentTarget()), callIndex(calls, "sudo "+hostSetupLaunchctl+" bootstrap system")
		if bootout < 0 || bootstrap < bootout+3 || !onlyPrint(calls[bootout+1:bootout+4]) || !exists(s.p.daemonPlist) || exists(s.p.agentPlist) {
			t.Fatalf("the switch did not wait for the boot-out to finish: %q\n%s", calls, out)
		}
	})
	t.Run("bootout fails but the service is already absent", func(t *testing.T) {
		s, v1 := agentUp(t)
		s.state("bootout-mode", "fail-absent")
		out := s.mustRun("--from", v1, "--service", "daemon")
		if !exists(s.p.daemonPlist) || exists(s.p.agentPlist) || callIndex(s.calls(), "sudo "+hostSetupLaunchctl+" bootstrap system") < 0 {
			t.Fatalf("the daemon was not installed:\n%s", out)
		}
	})
	for _, mode := range []string{"fail-loaded", "stuck"} {
		t.Run("bootout "+mode+" leaves the service loaded", func(t *testing.T) {
			s, v1 := agentUp(t)
			s.state("bootout-mode", mode)
			out, code := s.run("--from", v1, "--service", "daemon")
			calls := s.calls()
			if code != 1 || !strings.Contains(out, "failed     relay-service") || !strings.Contains(out, "still loaded") {
				t.Fatalf("exit %d:\n%s", code, out)
			}
			if exists(s.p.daemonPlist) || !exists(s.p.agentPlist) || callIndex(calls, "bootstrap") >= 0 || callIndex(calls, "sudo "+hostSetupLaunchctl+" bootstrap") >= 0 || callIndex(calls, "sudo install") >= 0 {
				t.Fatalf("a new service was written or started: %q", calls)
			}
		})
	}
	t.Run("sudo refuses to remove the daemon", func(t *testing.T) {
		s, v1 := agentUp(t)
		s.mustRun("--from", v1, "--service", "daemon")
		s.calls()
		s.state("sudo-refuse", "rm")
		out, code := s.run("--from", v1, "--service", "agent")
		calls := s.calls()
		if code != 1 || !strings.Contains(out, "failed     relay-service") || !strings.Contains(out, "agent relay was not started") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if exists(s.p.agentPlist) || callIndex(calls, "bootstrap") >= 0 {
			t.Fatalf("the agent was started: %q", calls)
		}
	})
	t.Run("sudo refuses to boot out the daemon", func(t *testing.T) {
		s, v1 := agentUp(t)
		s.mustRun("--from", v1, "--service", "daemon")
		s.calls()
		s.state("sudo-refuse", hostSetupLaunchctl)
		out, code := s.run("--from", v1, "--service", "agent")
		if calls := s.calls(); code != 1 || exists(s.p.agentPlist) || !exists(s.p.daemonPlist) || callIndex(calls, "bootstrap") >= 0 {
			t.Fatalf("exit %d, calls %q:\n%s", code, calls, out)
		}
	})
	t.Run("probe returns an unexpected code", func(t *testing.T) {
		s, v1 := agentUp(t)
		s.state("print-exit", "5")
		out, code := s.run("--from", v1)
		if calls := s.calls(); code != 1 || !strings.Contains(out, "state is unknown") || !onlyPrint(calls) {
			t.Fatalf("exit %d, calls %q:\n%s", code, calls, out)
		}
		if out, code := s.run("--check", "--from", v1); code != 1 || !strings.Contains(out, "failed     relay-service") {
			t.Fatalf("--check exit %d:\n%s", code, out)
		}
	})
	t.Run("launchctl cannot run", func(t *testing.T) {
		s, v1 := agentUp(t)
		hostSetupLaunchctl = filepath.Join(s.root, "fake", "absent-launchctl")
		if out, code := s.run("--from", v1); code != 1 || !strings.Contains(out, "did not run") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("both services loaded at the end", func(t *testing.T) {
		s, v1 := agentUp(t)
		s.state("bootstrap-also-loads", s.agentTarget())
		out, code := s.run("--from", v1, "--service", "daemon")
		if code != 1 || !strings.Contains(out, "failed     relay-service") || !strings.Contains(out, "both the agent and the daemon relay are loaded") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		s.calls()
		for _, args := range [][]string{{"--check", "--from", v1, "--service", "daemon"}, {"--check", "--from", v1}} {
			out, code = s.run(args...)
			if code != 1 || !strings.Contains(out, "conflict   relay-service") || !onlyPrint(s.calls()) {
				t.Fatalf("%v exit %d:\n%s", args, code, out)
			}
		}
	})
	t.Run("not macOS", func(t *testing.T) {
		s, v1 := agentUp(t)
		hostSetupGOOS = "linux"
		if out := s.mustRun("--from", v1); !strings.Contains(out, "current    relay-service  not macOS") || len(s.calls()) != 0 {
			t.Fatalf("output:\n%s", out)
		}
	})
}

func TestHostSetupRollback(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	v1, v2 := s.artifact("tt-v1"), s.artifact("tt-v2")
	s.mustRun("--from", v1)
	s.mustRun("--from", v2)
	hooks := s.hash(s.p.claudeSettings)
	s.calls()
	out := s.mustRun("--rollback")
	if s.hash(s.p.install) != s.hash(v1) || s.hash(s.p.rollback) != s.hash(v2) || exists(s.p.staged) || exists(s.p.rollbackTmp) {
		t.Fatalf("rollback left the wrong binaries:\n%s", out)
	}
	if info, _ := os.Stat(s.p.install); info.Mode().Perm()&0o111 == 0 {
		t.Fatal("the restored tt is not executable")
	}
	if calls := s.calls(); !hasCall(calls, "kickstart -k "+s.agentTarget()) || callIndex(calls, "bootstrap") >= 0 || callIndex(calls, "bootout") >= 0 {
		t.Fatalf("rollback calls = %q", calls)
	}
	if s.hash(s.p.claudeSettings) != hooks || !strings.Contains(out, "host rollback: ok") {
		t.Fatalf("rollback touched hooks or did not report ok:\n%s", out)
	}
	// A second rollback undoes the first.
	s.mustRun("--rollback")
	if s.hash(s.p.install) != s.hash(v2) || s.hash(s.p.rollback) != s.hash(v1) {
		t.Fatal("a second rollback did not undo the first")
	}

	// No rollback copy: nothing changes and nothing restarts.
	if err := os.Remove(s.p.rollback); err != nil {
		t.Fatal(err)
	}
	s.calls()
	before := s.tree()
	out, code := s.run("--rollback")
	if code != 1 || !strings.Contains(out, "no rollback copy at "+s.p.rollback) {
		t.Fatalf("rollback with no copy exited %d:\n%s", code, out)
	}
	if calls := s.calls(); !reflect.DeepEqual(before, s.tree()) || len(calls) != 0 {
		t.Fatalf("rollback with no copy changed something: %q", calls)
	}
}

func TestDoctorChecks(t *testing.T) {
	s := newHostSandbox(t, "home")
	release := s.holdRelayLock()
	e := readEnv()
	out, err := captureStdout(t, func() error { return cmdDoctor(e) })
	if err != nil {
		t.Fatalf("doctor with a hub and a running relay: %v\n%s", err, out)
	}
	for _, want := range []string{"OK tmux 3.05", "Installed runtimes: claude, codex", "OK hub reachable", "OK relay running"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output lacks %q:\n%s", want, out)
		}
	}
	release()
	out, err = captureStdout(t, func() error { return cmdDoctor(e) })
	if doctorExitCode(err) != 1 || !strings.Contains(err.Error(), "failed: relay") {
		t.Fatalf("doctor with the lock free: %v", err)
	}
	for _, want := range []string{"OK tmux 3.05", "Installed runtimes: claude, codex", "OK hub reachable", "FAIL relay"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output lacks %q:\n%s", want, out)
		}
	}
	// Every check still runs when the first ones fail.
	spawn.Tmux = filepath.Join(s.root, "fake", "no-tmux")
	e.hub = "http://127.0.0.1:1"
	out, err = captureStdout(t, func() error { return cmdDoctor(e) })
	if doctorExitCode(err) != 1 || !strings.Contains(err.Error(), "failed: tmux, hub, relay") {
		t.Fatalf("doctor with everything down: %v", err)
	}
	for _, want := range []string{"FAIL tmux", "Installed runtimes", "FAIL hub", "FAIL relay"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output lacks %q:\n%s", want, out)
		}
	}
	// Host setup reports the same failure and exits 1 with the steps current.
	spawn.Tmux = filepath.Join(s.root, "fake", "tmux")
	v1 := s.artifact("tt-v1")
	if out, code := s.run("--from", v1); code != 1 || !strings.Contains(out, "failed     doctor") || !strings.Contains(out, "failed: relay") || !strings.Contains(out, "installed  binary") {
		t.Fatalf("host setup with the relay down exited %d:\n%s", code, out)
	}
}

func TestHostSetupHubConfig(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	v1 := s.artifact("tt-v1")
	if out, code := s.run("--check", "--from", v1, "--hub", s.hubURL); code != 1 || !strings.Contains(out, "missing    hub-config") || exists(s.p.hubConfig) {
		t.Fatalf("--check with --hub exited %d:\n%s", code, out)
	}
	out := s.mustRun("--from", v1, "--hub", s.hubURL)
	info, err := os.Stat(s.p.hubConfig)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("hub.json: %v %v", info, err)
	}
	var config map[string]string
	raw, _ := os.ReadFile(s.p.hubConfig)
	if json.Unmarshal(raw, &config) != nil || !reflect.DeepEqual(config, map[string]string{"url": s.hubURL}) || !strings.Contains(out, "installed  hub-config") {
		t.Fatalf("hub.json = %s\n%s", raw, out)
	}
	before := s.tree()
	if out := s.mustRun("--from", v1, "--hub", s.hubURL); !strings.Contains(out, "current    hub-config") || !reflect.DeepEqual(before, s.tree()) {
		t.Fatalf("the same hub URL was not a no-op:\n%s", out)
	}

	const token = "SECRET-token-7f3a9c"
	stored := `{"url":"https://other-hub.example:8443","token":"` + token + `"}`
	s.write(s.p.hubConfig, stored, 0o600)
	before = s.tree()
	for _, args := range [][]string{{"--from", v1, "--hub", s.hubURL}, {"--from", v1, "--hub", s.hubURL, "--json"}, {"--check", "--from", v1, "--hub", s.hubURL}, {"--from", v1}} {
		out, code := s.run(args...)
		if strings.Contains(out, token) || strings.Contains(out, "other-hub.example") {
			t.Fatalf("%v printed the stored hub or token:\n%s", args, out)
		}
		if len(args) > 2 && (code != 1 || !strings.Contains(out, "different hub")) {
			t.Fatalf("%v did not refuse a different hub URL (exit %d):\n%s", args, code, out)
		}
	}
	if raw, _ := os.ReadFile(s.p.hubConfig); string(raw) != stored || !reflect.DeepEqual(before, s.tree()) {
		t.Fatalf("a refused hub URL changed hub.json: %s", raw)
	}
}

// Review f1: a relay restart that failed is still owed on the next run.
func TestHostSetupRestartPending(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	v1, v2 := s.artifact("tt-v1"), s.artifact("tt-v2")
	s.mustRun("--from", v1)
	if exists(s.p.restartPending) {
		t.Fatal("a successful first setup left a pending restart")
	}
	s.state("kickstart-exit", "1")
	s.calls()
	out, code := s.run("--from", v2)
	if code != 1 || !strings.Contains(out, "updated    binary") || !strings.Contains(out, "failed     relay-service") || s.hash(s.p.install) != s.hash(v2) {
		t.Fatalf("update with a failing kickstart exited %d:\n%s", code, out)
	}
	s.calls()
	before := s.tree()
	out, code = s.run("--check", "--from", v2)
	if code != 1 || !strings.Contains(out, "current    binary") || !strings.Contains(out, "outdated   relay-service") || !strings.Contains(out, "still runs the previous binary") {
		t.Fatalf("--check after a failed restart exited %d:\n%s", code, out)
	}
	if calls := s.calls(); !onlyPrint(calls) || !reflect.DeepEqual(before, s.tree()) {
		t.Fatalf("--check changed something: %q", calls)
	}
	// Still failing: the restart stays owed.
	if out, code := s.run("--from", v2); code != 1 || !strings.Contains(out, "failed     relay-service") {
		t.Fatalf("a second failing restart exited %d:\n%s", code, out)
	}
	s.clearState("kickstart-exit")
	s.calls()
	out = s.mustRun("--from", v2)
	if calls := s.calls(); !hasCall(calls, "kickstart -k "+s.agentTarget()) || !strings.Contains(out, "current    binary") || !strings.Contains(out, "updated    relay-service") {
		t.Fatalf("the re-run did not restart the relay: %q\n%s", calls, out)
	}
	if exists(s.p.restartPending) || s.hash(s.p.rollback) != s.hash(v1) {
		t.Fatal("the pending restart was not cleared, or the rollback copy changed")
	}
	if out := s.mustRun("--check", "--from", v2); !strings.Contains(out, "current    relay-service") || !onlyPrint(s.calls()) {
		t.Fatalf("after the restart --check is not current:\n%s", out)
	}

	// The same holds for a rollback whose restart failed.
	s.state("kickstart-exit", "1")
	if out, code := s.run("--rollback"); code != 1 || s.hash(s.p.install) != s.hash(v1) || !strings.Contains(out, "failed     relay-service") {
		t.Fatalf("rollback with a failing kickstart exited %d:\n%s", code, out)
	}
	s.clearState("kickstart-exit")
	s.calls()
	out = s.mustRun("--from", s.p.install)
	if calls := s.calls(); !hasCall(calls, "kickstart -k "+s.agentTarget()) || exists(s.p.restartPending) {
		t.Fatalf("setup after a rollback with a failed restart did not restart the relay: %q\n%s", calls, out)
	}
}

// Review f2: as root nothing is written, in any mode.
func TestHostSetupRefusesRoot(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	v1 := s.artifact("tt-v1")
	hostSetupUID = func() int { return 0 }
	before := s.tree()
	for _, args := range [][]string{{"--from", v1}, {"--from", v1, "--service", "agent"}, {"--from", v1, "--service", "daemon"}, {"--check", "--from", v1}, {"--rollback"}, {"--from", v1, "--hub", s.hubURL, "--json"}} {
		out, code := s.run(args...)
		if code != 1 || !strings.Contains(out, "not root") {
			t.Fatalf("%v as root exited %d:\n%s", args, code, out)
		}
		if calls := s.calls(); len(calls) != 0 || exists(s.home) || !reflect.DeepEqual(before, s.tree()) {
			t.Fatalf("%v as root wrote or called something: %q", args, calls)
		}
	}
}

// Review f3: the relay lock is not probed while a restarted relay starts.
func TestHostSetupRelayProbeSchedule(t *testing.T) {
	s := newHostSandbox(t, "home")
	s.holdRelayLock()
	// Nothing here depends on how fast the host is: the checks are probe
	// counts and lower bounds on pauses, and a sleep never returns early.
	const pause = 20 * time.Millisecond
	hostSetupRelayProbe = pause
	var probes []time.Time
	var start time.Time
	answer := func(nth int) bool { return false }
	relayRunningProbe = func() bool { probes = append(probes, time.Now()); return answer(len(probes)) }
	reset := func() { probes, start = nil, time.Now() }

	reset()
	if waitForRelay(0) || len(probes) != 1 {
		t.Fatalf("no wait: %d probes", len(probes))
	}
	reset()
	if waitForRelay(5*pause) || len(probes) == 0 || probes[0].Sub(start) < pause {
		t.Fatalf("a relay that never starts: %d probes, first after %v", len(probes), probes[0].Sub(start))
	}
	for i := 1; i < len(probes)-1; i++ { // the last pause may be cut short by the deadline
		if gap := probes[i].Sub(probes[i-1]); gap < pause {
			t.Fatalf("probes %d and %d are only %v apart", i-1, i, gap)
		}
	}
	answer = func(nth int) bool { return true }
	reset()
	if !waitForRelay(time.Hour) || len(probes) != 1 || probes[0].Sub(start) < pause {
		t.Fatalf("a relay that started: %d probes", len(probes))
	}

	// Through host setup. The fake relay is "running" only from the second
	// probe of a run, so a run that waits for a restarted relay makes two
	// probes and passes, while a run that restarts nothing makes exactly one
	// and reports the relay down. The wait is far longer than any run.
	hostSetupRelayWait = time.Hour
	answer = func(nth int) bool { return nth >= 2 }
	v1, v2 := s.artifact("tt-v1"), s.artifact("tt-v2")
	for _, c := range []struct {
		name  string
		args  []string
		waits bool
	}{
		{"fresh install", []string{"--from", v1}, true},
		{"current", []string{"--from", v1}, false},
		{"update", []string{"--from", v2}, true},
		{"current again", []string{"--from", v2}, false},
		{"rollback", []string{"--rollback"}, true},
		{"check", []string{"--check", "--from", v1}, false},
	} {
		reset()
		out, code := s.run(c.args...)
		if c.waits {
			if code != 0 || len(probes) != 2 || probes[0].Sub(start) < pause || probes[1].Sub(probes[0]) < pause {
				t.Fatalf("%s: exit %d, %d probes; want a wait and two spaced probes:\n%s", c.name, code, len(probes), out)
			}
		} else if code != 1 || len(probes) != 1 || !strings.Contains(out, "failed: relay") {
			t.Fatalf("%s: exit %d, %d probes; want one immediate probe and no wait:\n%s", c.name, code, len(probes), out)
		}
	}
}
