package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// doctorHost is a synthetic machine: a temp HOME and repo, and a bin dir of
// fake tools that is the only PATH the probe sees.
type doctorHost struct {
	t                *testing.T
	home, repo, bin  string
	log              string
	env              map[string]string
	goos             string
	whoamiErr        error
	codexHook        bool
	freeMiB          int64
	timeout          time.Duration
	deployConfigPath string
}

const doctorNow = "2026-09-30T19:00:00Z"

func newDoctorHost(t *testing.T) *doctorHost {
	t.Helper()
	root := t.TempDir()
	h := &doctorHost{t: t, home: filepath.Join(root, "home"), repo: filepath.Join(root, "repo"), bin: filepath.Join(root, "bin"),
		log: filepath.Join(root, "calls.log"), env: map[string]string{}, goos: "darwin", codexHook: true, freeMiB: 20000, timeout: 5 * time.Second}
	for _, dir := range []string{h.home, h.repo, h.bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Tools. Each fake logs its name, GOTOOLCHAIN and argv.
	h.fake("tmux", `echo "tmux 3.7b"`)
	h.fake("git", "exit 0")
	h.fake("claude", "exit 0")
	h.fake("codex", "exit 0")
	h.fake("launchctl", `printf 'gui/501/com.tailterm.inbox-relay = {\n\tactive count = 1\n\tstate = running\n\tendpoints = {\n\t\tstate = active\n\t}\n}\n'`)
	h.fake("python3", "exit 0")
	h.fake("npm", "exit 0")
	h.fake("node", "echo v26.5.1")
	h.fake("gh", "exit 0")
	h.fakeGo("1.26.6")
	h.fakeMac("25.5.0", "Apple M4")
	// Client files.
	h.file(".config/tailterm/hub.json", `{"url":"http://hub.invalid:1","token":"SECRET-TOKEN-123"}`, 0o600)
	// Agent files.
	h.file(".local/bin/tt", "#!/bin/sh\n", 0o755)
	h.file("Library/LaunchAgents/com.tailterm.inbox-relay.plist", "<plist/>", 0o644)
	// Test files.
	h.repoFile("node_modules/.package-lock.json", "{}")
	h.repoFile("hub/go.mod", "module example.test/hub\n\ngo 1.26.6\n")
	for _, f := range []string{"wasm/tailserve.wasm", ".build/test.wasm", ".build/go-modules.txt", ".build/speech-fixture.wav"} {
		h.repoFile(f, "fixture")
	}
	h.repoFile("node_modules/playwright-core/browsers.json", `{"browsers":[
{"name":"chromium","revision":"1243"},{"name":"chromium-headless-shell","revision":"1243"},
{"name":"firefox","revision":"1543"},
{"name":"webkit","revision":"2359","revisionOverrides":{"mac14":"2251","mac14-arm64":"2251"}}]}`)
	for _, b := range []string{"chromium-1243", "chromium_headless_shell-1243", "webkit-2359"} {
		h.dir("Library/Caches/ms-playwright/" + b)
	}
	// Release files.
	h.file(".npm/_npx/32026684e21afda6/node_modules/wrangler/package.json", `{"name":"wrangler","version":"4.145.0","engines":{"node":">=22.0.0"}}`, 0o644)
	h.file("Library/Preferences/.wrangler/config/default.toml", `oauth_token = "SECRET-OAUTH-321"
expiration_time = "2026-10-01T00:00:00.000Z"
refresh_token = "SECRET-OAUTH-321"
scopes = [ "account:read" ]
`, 0o600)
	h.file(".ssh/config", "Host *\n  ServerAliveInterval 30\n\nHost truenas\n  HostName SECRET-SSH-654\n  User root\n", 0o600)
	h.deployConfigPath = filepath.Join(root, "private", "deployer.json")
	h.writeDeployConfig(map[string]any{})
	return h
}

func (h *doctorHost) fake(name, body string) {
	h.t.Helper()
	script := "#!/bin/sh\nprintf '%s GOTOOLCHAIN=%s %s\\n' " + shellWord(name) + " \"${GOTOOLCHAIN-}\" \"$*\" >> " + shellWord(h.log) + "\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(h.bin, name), []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func (h *doctorHost) unfake(name string) {
	if err := os.Remove(filepath.Join(h.bin, name)); err != nil {
		h.t.Fatal(err)
	}
}

// fakeGo answers go version, go env GOENV (a file in HOME, absent unless the
// test writes it) and go env GOROOT (a root whose go.env says auto).
func (h *doctorHost) fakeGo(version string) {
	root := filepath.Join(h.home, "goroot")
	h.file("goroot/go.env", "GOPROXY=https://proxy.golang.org,direct\nGOTOOLCHAIN=auto\n", 0o644)
	h.fake("go", `case "$*" in
"version") echo "go version go`+version+` darwin/arm64" ;;
"env GOENV") echo `+shellWord(filepath.Join(h.home, "goenv"))+` ;;
"env GOROOT") echo `+shellWord(root)+` ;;
*) exit 2 ;;
esac`)
}

// fakeMac answers uname -r with a Darwin kernel release and sysctl with a
// CPU brand string; together they select Playwright's platform key.
func (h *doctorHost) fakeMac(release, cpu string) {
	h.fake("uname", "echo "+shellWord(release))
	h.fake("sysctl", "echo "+shellWord(cpu))
}

func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (h *doctorHost) file(rel, content string, mode os.FileMode) {
	h.t.Helper()
	path := filepath.Join(h.home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		h.t.Fatal(err)
	}
}

func (h *doctorHost) dir(rel string) {
	if err := os.MkdirAll(filepath.Join(h.home, filepath.FromSlash(rel)), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func (h *doctorHost) repoFile(rel, content string) {
	h.t.Helper()
	path := filepath.Join(h.repo, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// writeDeployConfig writes a valid deployer config with overrides applied.
func (h *doctorHost) writeDeployConfig(overrides map[string]any) {
	h.t.Helper()
	sha := strings.Repeat("a", 40)
	doc := map[string]any{"version": 1, "enabled": true, "cwd": "/SECRET-CFG-000/cwd", "journalDirectory": "/SECRET-CFG-000/journal",
		"hostAdapter": "SECRET-CFG-000", "baselines": map[string]any{"hub": sha, "bridge": sha, "mini": sha, "tailos": sha}}
	for k, v := range overrides {
		doc[k] = v
	}
	b, _ := json.Marshal(doc)
	if err := os.MkdirAll(filepath.Dir(h.deployConfigPath), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.deployConfigPath, b, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *doctorHost) probe() hostProbe {
	lookPath := func(name string) (string, error) {
		path := filepath.Join(h.bin, name)
		if info, err := os.Stat(path); err == nil && info.Mode()&0o111 != 0 {
			return path, nil
		}
		return "", exec.ErrNotFound
	}
	now, _ := time.Parse(time.RFC3339, doctorNow)
	return hostProbe{
		goos: h.goos, home: h.home, repo: h.repo, deployConfig: h.deployConfigPath, uid: 501,
		env:       func(k string) string { return h.env[k] },
		lookPath:  lookPath,
		run:       execDoctorCommand([]string{"PATH=" + h.bin, "HOME=" + h.home}, lookPath, 100*time.Millisecond),
		readFile:  os.ReadFile,
		stat:      os.Stat,
		glob:      filepath.Glob,
		statfsMiB: func(string) (int64, error) { return h.freeMiB, nil },
		whoami:    func() error { return h.whoamiErr },
		codexHook: func() bool { return h.codexHook },
		now:       func() time.Time { return now },
		timeout:   h.timeout,
	}
}

type doctorOutput struct {
	text string
	exit int
}

func (h *doctorHost) run(role string) doctorOutput {
	h.t.Helper()
	var out bytes.Buffer
	err := runDoctorRole(&out, role, h.probe())
	return doctorOutput{text: out.String(), exit: doctorExitCode(err)}
}

func doctorExitCode(err error) int {
	if err == nil {
		return 0
	}
	var coded *exitError
	if errors.As(err, &coded) {
		return coded.code
	}
	return -1
}

// line returns the status and detail printed for a named check.
func (o doctorOutput) line(t *testing.T, name string) (status, detail string) {
	t.Helper()
	for _, l := range strings.Split(o.text, "\n") {
		if len(l) < 15+34 || strings.HasPrefix(l, " ") {
			continue
		}
		if strings.TrimSpace(l[15:15+34]) == name {
			return strings.TrimSpace(l[:13]), strings.TrimSpace(l[15+34:])
		}
	}
	t.Fatalf("no line for %q in:\n%s", name, o.text)
	return "", ""
}

func (o doctorOutput) has(name string) bool {
	for _, l := range strings.Split(o.text, "\n") {
		if len(l) >= 15+34 && !strings.HasPrefix(l, " ") && strings.TrimSpace(l[15:15+34]) == name {
			return true
		}
	}
	return false
}

func expectLine(t *testing.T, o doctorOutput, name, status, detailPart string, exit int) {
	t.Helper()
	got, detail := o.line(t, name)
	if got != status || !strings.Contains(detail, detailPart) {
		t.Fatalf("%s = %q %q, want %q containing %q\n%s", name, got, detail, status, detailPart, o.text)
	}
	if o.exit != exit {
		t.Fatalf("exit = %d, want %d\n%s", o.exit, exit, o.text)
	}
}

func TestDoctorRoleClient(t *testing.T) {
	t.Run("pass", func(t *testing.T) {
		o := newDoctorHost(t).run("client")
		expectLine(t, o, "hub reachable", "ok", "whoami", 0)
		expectLine(t, o, "hub configured", "ok", "hub.json", 0)
		if !strings.HasSuffix(o.text, "role client: ok\n") {
			t.Fatalf("summary:\n%s", o.text)
		}
	})
	// tt adds its own directory to PATH, so a tt on-PATH line could never fail.
	t.Run("no tt line", func(t *testing.T) {
		for _, role := range []string{"client", "agent", "test", "release"} {
			if o := newDoctorHost(t).run(role); o.has("tt") {
				t.Fatalf("role %s prints a tt line:\n%s", role, o.text)
			}
		}
	})
	t.Run("hub unreachable", func(t *testing.T) {
		h := newDoctorHost(t)
		h.whoamiErr = errors.New("dial tcp http://hub.invalid:1 SECRET-TOKEN-123: refused")
		o := h.run("client")
		expectLine(t, o, "hub reachable", "missing", "unreachable", 1)
		if strings.Contains(o.text, "SECRET") || strings.Contains(o.text, "hub.invalid") {
			t.Fatalf("error text leaked:\n%s", o.text)
		}
	})
	t.Run("hub HTTP status", func(t *testing.T) {
		h := newDoctorHost(t)
		h.whoamiErr = &api.HTTPError{Status: 401, Msg: "SECRET-TOKEN-123 rejected"}
		expectLine(t, h.run("client"), "hub reachable", "missing", "HTTP 401", 1)
	})
	t.Run("hub.json mode 0644", func(t *testing.T) {
		h := newDoctorHost(t)
		h.file(".config/tailterm/hub.json", `{"url":"http://hub.invalid:1"}`, 0o644)
		o := h.run("client")
		expectLine(t, o, "hub.json private", "missing", "0644", 1)
		if !strings.Contains(o.text, "fix: chmod 600 ~/.config/tailterm/hub.json") {
			t.Fatalf("fix hint missing:\n%s", o.text)
		}
	})
	t.Run("no hub configured", func(t *testing.T) {
		h := newDoctorHost(t)
		os.Remove(filepath.Join(h.home, ".config", "tailterm", "hub.json"))
		expectLine(t, h.run("client"), "hub configured", "missing", "no TAILTERM_HUB", 1)
	})
	t.Run("tailscale optional", func(t *testing.T) {
		o := newDoctorHost(t).run("client")
		expectLine(t, o, "tailscale CLI", "warn", "optional", 0)
	})
}

func TestDoctorRoleAgent(t *testing.T) {
	t.Run("pass", func(t *testing.T) {
		o := newDoctorHost(t).run("agent")
		for _, name := range []string{"tmux >= 3.2", "tmux sizing policy", "tt in ~/.local/bin", "claude login", "codex login", "codex Stop hook", "relay service installed", "relay running"} {
			expectLine(t, o, name, "ok", "", 0)
		}
		// Client checks are part of every role.
		expectLine(t, o, "hub reachable", "ok", "", 0)
	})
	t.Run("tt not installed", func(t *testing.T) {
		h := newDoctorHost(t)
		os.Remove(filepath.Join(h.home, ".local", "bin", "tt"))
		expectLine(t, h.run("agent"), "tt in ~/.local/bin", "missing", "not found", 1)
	})
	t.Run("tt not executable", func(t *testing.T) {
		h := newDoctorHost(t)
		h.file(".local/bin/tt", "#!/bin/sh\n", 0o644)
		expectLine(t, h.run("agent"), "tt in ~/.local/bin", "missing", "not executable", 1)
	})
	t.Run("tmux 3.1", func(t *testing.T) {
		h := newDoctorHost(t)
		h.fake("tmux", `echo "tmux 3.1"`)
		o := h.run("agent")
		expectLine(t, o, "tmux >= 3.2", "wrong version", "tmux 3.1", 1)
		expectLine(t, o, "tmux sizing policy", "wrong version", "window-size manual", 1)
	})
	t.Run("claude logged out", func(t *testing.T) {
		h := newDoctorHost(t)
		h.fake("claude", `[ "$*" = "auth status" ] && exit 1; exit 0`)
		o := h.run("agent")
		expectLine(t, o, "claude login", "missing", "exited 1", 1)
		expectLine(t, o, "claude", "ok", "", 1)
	})
	t.Run("codex absent", func(t *testing.T) {
		h := newDoctorHost(t)
		h.unfake("codex")
		o := h.run("agent")
		expectLine(t, o, "codex", "missing", "not on PATH", 1)
		expectLine(t, o, "codex login", "missing", "not installed", 1)
	})
	t.Run("codex Stop hook absent", func(t *testing.T) {
		h := newDoctorHost(t)
		h.codexHook = false
		o := h.run("agent")
		expectLine(t, o, "codex Stop hook", "missing", "Stop hook", 1)
		if !strings.Contains(o.text, "fix: tt hooks codex --install") {
			t.Fatalf("fix hint missing:\n%s", o.text)
		}
	})
	t.Run("no relay plist", func(t *testing.T) {
		h := newDoctorHost(t)
		os.Remove(filepath.Join(h.home, "Library", "LaunchAgents", "com.tailterm.inbox-relay.plist"))
		expectLine(t, h.run("agent"), "relay service installed", "missing", "com.tailterm.inbox-relay.plist", 1)
	})
	t.Run("relay not loaded", func(t *testing.T) {
		h := newDoctorHost(t)
		h.fake("launchctl", `echo 'Could not find service "com.tailterm.inbox-relay" in domain for user gui: 501' >&2; exit 113`)
		expectLine(t, h.run("agent"), "relay running", "missing", "not loaded", 1)
	})
	t.Run("relay not running", func(t *testing.T) {
		h := newDoctorHost(t)
		h.fake("launchctl", `printf 'gui/501/com.tailterm.inbox-relay = {\n\tstate = not running\n\tendpoints = {\n\t\tstate = active\n\t}\n}\n'`)
		expectLine(t, h.run("agent"), "relay running", "missing", "state not running", 1)
	})
	t.Run("launchctl target", func(t *testing.T) {
		h := newDoctorHost(t)
		h.run("agent")
		if log, _ := os.ReadFile(h.log); !strings.Contains(string(log), "launchctl GOTOOLCHAIN= print gui/501/com.tailterm.inbox-relay") {
			t.Fatalf("launchctl call:\n%s", log)
		}
	})
	t.Run("linux relay", func(t *testing.T) {
		h := newDoctorHost(t)
		h.goos = "linux"
		o := h.run("agent")
		expectLine(t, o, "relay service installed", "missing", "no linux relay service yet", 1)
		expectLine(t, o, "relay running", "missing", "no linux relay service yet", 1)
		if log, _ := os.ReadFile(h.log); strings.Contains(string(log), "launchctl") {
			t.Fatalf("launchctl ran on linux:\n%s", log)
		}
	})
	t.Run("claude hooks present", func(t *testing.T) {
		h := newDoctorHost(t)
		h.file(".claude/settings.json", `{"hooks":{
"SessionStart":[{"hooks":[{"type":"command","command":"/Users/x/.local/bin/tt hook session-start"}]}],
"UserPromptSubmit":[{"hooks":[{"type":"command","command":"tt hook prompt"}]}],
"Notification":[{"hooks":[{"type":"command","command":"tt hook notification"}]}]}}`, 0o644)
		h.file(".claude/settings.local.json", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"tt hook stop"}]}]}}`, 0o644)
		expectLine(t, h.run("agent"), "claude hooks", "ok", "SessionStart", 0)
	})
	t.Run("claude hooks under CLAUDE_CONFIG_DIR", func(t *testing.T) {
		h := newDoctorHost(t)
		h.env["CLAUDE_CONFIG_DIR"] = filepath.Join(h.home, "alt")
		h.file("alt/settings.json", `{"hooks":{
"SessionStart":[{"hooks":[{"command":"tt hook session-start"}]}],"UserPromptSubmit":[{"hooks":[{"command":"tt hook prompt"}]}],
"Stop":[{"hooks":[{"command":"tt hook stop"}]}],"Notification":[{"hooks":[{"command":"tt hook notification"}]}]}}`, 0o644)
		expectLine(t, h.run("agent"), "claude hooks", "ok", "", 0)
	})
	t.Run("claude hooks absent", func(t *testing.T) {
		h := newDoctorHost(t)
		h.file(".claude/settings.json", `{"hooks":{"Stop":[{"hooks":[{"command":"tt hook stop"}]}]}}`, 0o644)
		o := h.run("agent")
		expectLine(t, o, "claude hooks", "warn", "optional", 0)
		if !strings.HasSuffix(o.text, "role agent: ok\n") {
			t.Fatalf("an optional warning failed the role:\n%s", o.text)
		}
	})
	t.Run("codex hook read from CODEX_HOME", func(t *testing.T) {
		h := newDoctorHost(t)
		codex := filepath.Join(h.home, "codex-home")
		t.Setenv("CODEX_HOME", codex)
		p := h.probe()
		p.codexHook = codexStopHookInstalled
		var out bytes.Buffer
		err := runDoctorRole(&out, "agent", p)
		o := doctorOutput{text: out.String(), exit: doctorExitCode(err)}
		expectLine(t, o, "codex Stop hook", "missing", "", 1)
		h.file("codex-home/hooks.json", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/Users/x/.local/bin/tt hook stop"}]}]}}`, 0o600)
		out.Reset()
		err = runDoctorRole(&out, "agent", p)
		expectLine(t, doctorOutput{text: out.String(), exit: doctorExitCode(err)}, "codex Stop hook", "ok", "", 0)
	})
}

func TestDoctorRoleTest(t *testing.T) {
	t.Run("pass", func(t *testing.T) {
		o := newDoctorHost(t).run("test")
		for _, name := range []string{"git", "tmux >= 3.2", "python3", "npm", "node_modules installed", "go toolchain",
			"fixture wasm/tailserve.wasm", "fixture .build/test.wasm", "fixture .build/go-modules.txt", "fixture .build/speech-fixture.wav",
			"playwright chromium", "playwright chromium-headless-shell", "playwright webkit", "free disk >= 8 GiB"} {
			expectLine(t, o, name, "ok", "", 0)
		}
		expectLine(t, o, "node "+doctorTestNodeRange, "ok", "26.5.1", 0)
	})
	t.Run("missing test.wasm", func(t *testing.T) {
		h := newDoctorHost(t)
		os.Remove(filepath.Join(h.repo, ".build", "test.wasm"))
		o := h.run("test")
		expectLine(t, o, "fixture .build/test.wasm", "missing", ".build/test.wasm not found", 1)
		if !strings.Contains(o.text, "fix: npm run build:wasm -- --test") || !strings.HasSuffix(o.text, "role test: 1 required missing\n") {
			t.Fatalf("fix or summary:\n%s", o.text)
		}
	})
	t.Run("python3 absent", func(t *testing.T) {
		h := newDoctorHost(t)
		h.unfake("python3")
		expectLine(t, h.run("test"), "python3", "missing", "not on PATH", 1)
	})
	t.Run("low disk", func(t *testing.T) {
		h := newDoctorHost(t)
		h.freeMiB = 8191
		expectLine(t, h.run("test"), "free disk >= 8 GiB", "missing", "8191 MiB free", 1)
	})
	t.Run("no node_modules", func(t *testing.T) {
		h := newDoctorHost(t)
		os.RemoveAll(filepath.Join(h.repo, "node_modules"))
		o := h.run("test")
		expectLine(t, o, "node_modules installed", "missing", "", 1)
		expectLine(t, o, "playwright chromium", "missing", "browsers.json not found", 1)
	})
	for _, c := range []struct {
		version string
		status  string
	}{{"20.18.9", "wrong version"}, {"20.19.0", "ok"}, {"21.7.0", "wrong version"}, {"22.11.9", "wrong version"}, {"22.12.0", "ok"}, {"26.5.1", "ok"}} {
		t.Run("node "+c.version, func(t *testing.T) {
			h := newDoctorHost(t)
			h.fake("node", "echo v"+c.version)
			exit := 0
			if c.status != "ok" {
				exit = 1
			}
			expectLine(t, h.run("test"), "node "+doctorTestNodeRange, c.status, c.version, exit)
		})
	}
	for _, c := range []struct {
		name, setting, installed, status, detail string
		extraOnPath                              string
	}{
		{"local older", "local", "1.26.5", "wrong version", "go 1.26.5, go.mod 1.26.6", ""},
		{"local equal", "local", "1.26.6", "ok", "GOTOOLCHAIN=local", ""},
		{"auto older", "auto", "1.26.5", "ok", "will download go1.26.6", ""},
		{"pinned older", "go1.26.5", "1.26.6", "wrong version", "pinned go1.26.5", ""},
		{"pinned equal", "go1.26.6", "1.26.5", "ok", "pinned go1.26.6", ""},
		{"pinned plus auto", "go1.26.5+auto", "1.26.5", "ok", "will download go1.26.6", ""},
		{"path older", "path", "1.26.5", "wrong version", "no go1.26.6 on PATH", ""},
		{"path with go1.26.6", "path", "1.26.5", "ok", "GOTOOLCHAIN=path", "go1.26.6"},
		{"local plus path", "local+path", "1.26.6", "ok", "", ""},
		{"bogus", "bogus", "1.26.6", "wrong version", "unrecognized GOTOOLCHAIN", ""},
	} {
		t.Run("go "+c.name, func(t *testing.T) {
			h := newDoctorHost(t)
			h.fakeGo(c.installed)
			h.env["GOTOOLCHAIN"] = c.setting
			if c.extraOnPath != "" {
				h.fake(c.extraOnPath, "exit 0")
			}
			exit := 0
			if c.status != "ok" {
				exit = 1
			}
			expectLine(t, h.run("test"), "go toolchain", c.status, c.detail, exit)
		})
	}
	t.Run("go toolchain from GOENV file", func(t *testing.T) {
		h := newDoctorHost(t)
		h.fakeGo("1.26.5")
		h.file("goenv", "GOTOOLCHAIN=local\n", 0o644)
		expectLine(t, h.run("test"), "go toolchain", "wrong version", "GOTOOLCHAIN=local", 1)
	})
	t.Run("go toolchain from GOROOT go.env", func(t *testing.T) {
		h := newDoctorHost(t)
		h.fakeGo("1.26.5")
		expectLine(t, h.run("test"), "go toolchain", "ok", "GOTOOLCHAIN=auto (will download go1.26.6)", 0)
	})
	t.Run("go runs with GOTOOLCHAIN=local", func(t *testing.T) {
		h := newDoctorHost(t)
		h.run("test")
		log, _ := os.ReadFile(h.log)
		calls := 0
		for _, l := range strings.Split(string(log), "\n") {
			if strings.HasPrefix(l, "go ") {
				calls++
				if !strings.HasPrefix(l, "go GOTOOLCHAIN=local ") {
					t.Fatalf("go call without GOTOOLCHAIN=local: %q", l)
				}
			}
		}
		if calls == 0 {
			t.Fatalf("no go calls logged:\n%s", log)
		}
	})
	t.Run("node range matches package-lock", func(t *testing.T) {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "package-lock.json"))
		if err != nil {
			t.Fatal(err)
		}
		var lock struct {
			Packages map[string]struct {
				Engines struct {
					Node string `json:"node"`
				} `json:"engines"`
			} `json:"packages"`
		}
		if err := json.Unmarshal(b, &lock); err != nil {
			t.Fatal(err)
		}
		if got := lock.Packages["node_modules/vite"].Engines.Node; got != doctorTestNodeRange {
			t.Fatalf("package-lock vite engines %q; update doctorTestNodeRange and docs/host-requirements.md", got)
		}
	})
	// The mac14 overrides in browsers.json are not for macOS 26.
	t.Run("webkit override for another platform", func(t *testing.T) {
		h := newDoctorHost(t)
		os.RemoveAll(filepath.Join(h.home, "Library", "Caches", "ms-playwright", "webkit-2359"))
		h.dir("Library/Caches/ms-playwright/webkit-2251")
		o := h.run("test")
		expectLine(t, o, "playwright webkit", "wrong version", "found webkit-2251; need webkit-2359", 1)
		expectLine(t, o, "playwright chromium", "ok", "chromium-1243", 1)
		expectLine(t, o, "playwright chromium-headless-shell", "ok", "chromium_headless_shell-1243", 1)
	})
	t.Run("webkit override for this platform", func(t *testing.T) {
		for _, cpu := range []string{"Apple M1", "Intel(R) Core(TM) i7-9750H CPU @ 2.60GHz"} {
			h := newDoctorHost(t)
			h.fakeMac("23.6.0", cpu)
			o := h.run("test")
			expectLine(t, o, "playwright webkit", "wrong version", "found webkit-2359; need webkit-2251", 1)
			expectLine(t, o, "playwright chromium", "ok", "chromium-1243", 1)
			expectLine(t, o, "playwright chromium-headless-shell", "ok", "chromium_headless_shell-1243", 1)
			os.RemoveAll(filepath.Join(h.home, "Library", "Caches", "ms-playwright", "webkit-2359"))
			h.dir("Library/Caches/ms-playwright/webkit-2251")
			expectLine(t, h.run("test"), "playwright webkit", "ok", "webkit-2251", 0)
		}
	})
	t.Run("webkit default revision without a platform key", func(t *testing.T) {
		unreadable := newDoctorHost(t)
		unreadable.fake("uname", "echo unknown")
		missing := newDoctorHost(t)
		missing.unfake("uname")
		failing := newDoctorHost(t)
		failing.fake("uname", "echo 23.6.0; exit 1")
		for name, h := range map[string]*doctorHost{"unreadable": unreadable, "missing": missing, "failing": failing} {
			expectLine(t, h.run("test"), "playwright webkit", "ok", "webkit-2359", 0)
			os.RemoveAll(filepath.Join(h.home, "Library", "Caches", "ms-playwright", "webkit-2359"))
			h.dir("Library/Caches/ms-playwright/webkit-2251")
			if status, detail := h.run("test").line(t, "playwright webkit"); status != "wrong version" || !strings.Contains(detail, "need webkit-2359") {
				t.Fatalf("%s uname: %q %q", name, status, detail)
			}
		}
	})
	t.Run("playwright platform key", func(t *testing.T) {
		for _, c := range []struct {
			release string
			apple   bool
			want    string
		}{
			{"23.6.0", false, "mac14"}, {"23.6.0", true, "mac14-arm64"},
			{"24.6.0", false, "mac15"}, {"24.6.0", true, "mac15-arm64"},
			{"25.5.0", false, "mac26"}, {"25.5.0", true, "mac26-arm64"},
			{"26.0.0", false, "mac26"}, {"26.0.0", true, "mac26-arm64"},
			{"20.1.0\n", true, "mac11-arm64"}, {"19.6.0", false, ""},
			{"", true, ""}, {"unknown", true, ""},
		} {
			if got := playwrightMacPlatform(c.release, c.apple); got != c.want {
				t.Errorf("playwrightMacPlatform(%q, %v) = %q, want %q", c.release, c.apple, got, c.want)
			}
		}
		// Linux never has a key, even where uname would answer like a mac14 host.
		h := newDoctorHost(t)
		h.goos = "linux"
		h.fakeMac("23.6.0", "Apple M1")
		d := &doctorRun{p: h.probe(), memo: map[string]any{}}
		if got := d.playwrightPlatform(); got != "" {
			t.Fatalf("linux platform key %q", got)
		}
	})
	t.Run("chromium other revision", func(t *testing.T) {
		h := newDoctorHost(t)
		os.RemoveAll(filepath.Join(h.home, "Library", "Caches", "ms-playwright", "chromium-1243"))
		h.dir("Library/Caches/ms-playwright/chromium-1234")
		o := h.run("test")
		expectLine(t, o, "playwright chromium", "wrong version", "found chromium-1234; need chromium-1243", 1)
		// chromium_headless_shell-1243 is a different browser, not a chromium revision.
		expectLine(t, o, "playwright chromium-headless-shell", "ok", "", 1)
	})
	t.Run("PLAYWRIGHT_BROWSERS_PATH", func(t *testing.T) {
		h := newDoctorHost(t)
		alt := filepath.Join(h.home, "pw")
		h.env["PLAYWRIGHT_BROWSERS_PATH"] = alt
		o := h.run("test")
		expectLine(t, o, "playwright chromium", "missing", "chromium-1243 not installed", 1)
	})
	t.Run("linux playwright cache", func(t *testing.T) {
		h := newDoctorHost(t)
		h.goos = "linux"
		for _, b := range []string{"chromium-1243", "chromium_headless_shell-1243", "webkit-2359"} {
			h.dir(".cache/ms-playwright/" + b)
		}
		h.dir(".cache/ms-playwright/webkit-2251")
		h.fakeMac("23.6.0", "Apple M1")
		expectLine(t, h.run("test"), "playwright webkit", "ok", "webkit-2359", 0)
	})
}

func TestDoctorRoleRelease(t *testing.T) {
	t.Run("pass", func(t *testing.T) {
		o := newDoctorHost(t).run("release")
		for _, name := range []string{"gh", "gh login", "python3", "node " + doctorReleaseNodeRange, "wrangler", "wrangler credential", "ssh route truenas",
			"deploy config", "deploy config private", "deploy config version", "deploy config enabled", "deploy config cwd", "deploy config journalDirectory",
			"deploy config baselines.hub", "deploy config baselines.bridge", "deploy config baselines.mini", "deploy config baselines.tailos"} {
			expectLine(t, o, name, "ok", "", 0)
		}
		expectLine(t, o, "wrangler", "ok", "4.145.0", 0)
	})
	t.Run("gh logged out", func(t *testing.T) {
		h := newDoctorHost(t)
		h.fake("gh", "exit 1")
		expectLine(t, h.run("release"), "gh login", "missing", "exited 1", 1)
	})
	t.Run("no deploy config flag", func(t *testing.T) {
		h := newDoctorHost(t)
		h.deployConfigPath = ""
		o := h.run("release")
		expectLine(t, o, "deploy config", "missing", "no --deploy-config", 1)
		if o.has("deploy config enabled") || !strings.HasSuffix(o.text, "role release: 1 required missing\n") {
			t.Fatalf("expected one deploy config line:\n%s", o.text)
		}
	})
	t.Run("deploy config disabled", func(t *testing.T) {
		h := newDoctorHost(t)
		h.writeDeployConfig(map[string]any{"enabled": false})
		expectLine(t, h.run("release"), "deploy config enabled", "missing", "enabled missing or invalid", 1)
	})
	t.Run("deploy config short baseline", func(t *testing.T) {
		h := newDoctorHost(t)
		sha := strings.Repeat("b", 40)
		h.writeDeployConfig(map[string]any{"baselines": map[string]any{"hub": sha, "bridge": sha[:39], "mini": sha, "tailos": sha}})
		o := h.run("release")
		expectLine(t, o, "deploy config baselines.bridge", "missing", "baselines.bridge", 1)
		expectLine(t, o, "deploy config baselines.hub", "ok", "", 1)
	})
	t.Run("deploy config mode 0644", func(t *testing.T) {
		h := newDoctorHost(t)
		os.Chmod(h.deployConfigPath, 0o644)
		expectLine(t, h.run("release"), "deploy config private", "missing", "0644", 1)
	})
	t.Run("deploy config not JSON", func(t *testing.T) {
		h := newDoctorHost(t)
		os.WriteFile(h.deployConfigPath, []byte("SECRET-CFG-000 not json"), 0o600)
		o := h.run("release")
		expectLine(t, o, "deploy config", "missing", "not a JSON object", 1)
		expectLine(t, o, "deploy config cwd", "missing", "not checked", 1)
	})
	t.Run("wrangler absent", func(t *testing.T) {
		h := newDoctorHost(t)
		os.RemoveAll(filepath.Join(h.home, ".npm"))
		o := h.run("release")
		expectLine(t, o, "wrangler", "missing", "not on PATH or in the npx cache", 1)
		if !strings.Contains(o.text, "fix: npx wrangler --version (downloads it)") {
			t.Fatalf("fix hint:\n%s", o.text)
		}
	})
	t.Run("wrangler on PATH", func(t *testing.T) {
		h := newDoctorHost(t)
		os.RemoveAll(filepath.Join(h.home, ".npm"))
		h.fake("wrangler", "exit 0")
		expectLine(t, h.run("release"), "wrangler", "ok", "version not checked", 0)
		if log, _ := os.ReadFile(h.log); strings.Contains(string(log), "wrangler ") {
			t.Fatalf("wrangler ran:\n%s", log)
		}
	})
	t.Run("wrangler highest npx version", func(t *testing.T) {
		h := newDoctorHost(t)
		h.env["npm_config_cache"] = filepath.Join(h.home, "npmcache")
		h.file("npmcache/_npx/one/node_modules/wrangler/package.json", `{"version":"4.9.0","engines":{"node":">=22.0.0"}}`, 0o644)
		h.file("npmcache/_npx/two/node_modules/wrangler/package.json", `{"version":"4.10.1","engines":{"node":">=22.0.0"}}`, 0o644)
		expectLine(t, h.run("release"), "wrangler", "ok", "wrangler 4.10.1", 0)
	})
	t.Run("wrangler engines exclude node", func(t *testing.T) {
		h := newDoctorHost(t)
		h.fake("node", "echo v20.19.0")
		o := h.run("release")
		expectLine(t, o, "wrangler", "wrong version", "needs node >=22.0.0, have 20.19.0", 1)
		expectLine(t, o, "node "+doctorReleaseNodeRange, "wrong version", "20.19.0", 1)
	})
	t.Run("wrangler token empty", func(t *testing.T) {
		h := newDoctorHost(t)
		h.file("Library/Preferences/.wrangler/config/default.toml", "oauth_token = \"\"\nrefresh_token = \"SECRET-OAUTH-321\"\n", 0o600)
		expectLine(t, h.run("release"), "wrangler credential", "missing", "no oauth_token", 1)
	})
	t.Run("wrangler expired without refresh", func(t *testing.T) {
		h := newDoctorHost(t)
		h.file("Library/Preferences/.wrangler/config/default.toml", "oauth_token = \"SECRET-OAUTH-321\"\nexpiration_time = \"2026-09-30T18:53:00.000Z\"\n", 0o600)
		expectLine(t, h.run("release"), "wrangler credential", "missing", "expired", 1)
	})
	t.Run("wrangler expired with refresh", func(t *testing.T) {
		h := newDoctorHost(t)
		h.file("Library/Preferences/.wrangler/config/default.toml", "oauth_token = \"SECRET-OAUTH-321\"\nexpiration_time = \"2026-09-30T18:53:00.000Z\"\nrefresh_token = \"SECRET-OAUTH-321\"\n", 0o600)
		expectLine(t, h.run("release"), "wrangler credential", "ok", "refreshable", 0)
	})
	t.Run("wrangler unexpired", func(t *testing.T) {
		h := newDoctorHost(t)
		h.file("Library/Preferences/.wrangler/config/default.toml", "oauth_token = \"SECRET-OAUTH-321\"\nexpiration_time = \"2026-10-01T00:00:00.000Z\"\n", 0o600)
		o := h.run("release")
		expectLine(t, o, "wrangler credential", "ok", "credential present, not verified online", 0)
	})
	t.Run("CLOUDFLARE_API_TOKEN", func(t *testing.T) {
		h := newDoctorHost(t)
		os.RemoveAll(filepath.Join(h.home, "Library", "Preferences", ".wrangler"))
		h.env["CLOUDFLARE_API_TOKEN"] = "SECRET-CF-456"
		expectLine(t, h.run("release"), "wrangler credential", "ok", "CLOUDFLARE_API_TOKEN set", 0)
	})
	t.Run("linux wrangler config", func(t *testing.T) {
		h := newDoctorHost(t)
		h.goos = "linux"
		h.env["XDG_CONFIG_HOME"] = filepath.Join(h.home, "xdg")
		h.file("xdg/.wrangler/config/default.toml", "oauth_token = \"SECRET-OAUTH-321\"\nexpiration_time = \"2026-10-01T00:00:00Z\"\n", 0o600)
		expectLine(t, h.run("release"), "wrangler credential", "ok", "", 0)
	})
	sshCases := []struct {
		name, config, status, detail string
		include                      map[string]string
	}{
		{name: "ssh alias", config: "Host truenas\n  HostName SECRET-SSH-654\n", status: "ok", detail: "HostName set"},
		{name: "ssh equals syntax", config: "HOST=nas truenas\n\tHostname=SECRET-SSH-654\n", status: "ok", detail: "HostName set"},
		{name: "ssh wildcard", config: "Host true?as\n  HostName SECRET-SSH-654\n", status: "ok"},
		{name: "ssh absent", config: "Host other\n  HostName SECRET-SSH-654\n", status: "missing", detail: "no Host truenas"},
		{name: "ssh no hostname", config: "Host truenas\n  User root\n", status: "missing", detail: "has no HostName"},
		{name: "ssh include", config: "Include config.d/*\n", include: map[string]string{".ssh/config.d/10-nas": "Host truenas\n  HostName SECRET-SSH-654\n"}, status: "ok"},
		{name: "ssh include home path", config: "Include ~/.ssh/extra\n", include: map[string]string{".ssh/extra": "Host truenas\n  HostName SECRET-SSH-654\n"}, status: "ok"},
		{name: "ssh include cycle", config: "Include config\nHost truenas\n  HostName SECRET-SSH-654\n", status: "ok"},
		{name: "ssh match", config: "Match exec \"false\"\n  User x\nHost truenas\n  HostName SECRET-SSH-654\n", status: "warn", detail: "Match rules not evaluated"},
		{name: "ssh negation", config: "Host * !truenas\n  HostName SECRET-SSH-654\n", status: "missing", detail: "no Host truenas"},
		{name: "ssh match without alias", config: "Match host truenas\n  HostName SECRET-SSH-654\n", status: "missing", detail: "no Host truenas"},
	}
	for _, c := range sshCases {
		t.Run(c.name, func(t *testing.T) {
			h := newDoctorHost(t)
			h.file(".ssh/config", c.config, 0o600)
			for rel, content := range c.include {
				h.file(rel, content, 0o600)
			}
			exit := 0
			if c.status == "missing" {
				exit = 1
			}
			o := h.run("release")
			expectLine(t, o, "ssh route truenas", c.status, c.detail, exit)
			if log, _ := os.ReadFile(h.log); strings.Contains(string(log), "\nssh ") || strings.HasPrefix(string(log), "ssh ") {
				t.Fatalf("ssh ran:\n%s", log)
			}
		})
	}
	t.Run("ssh no config", func(t *testing.T) {
		h := newDoctorHost(t)
		os.Remove(filepath.Join(h.home, ".ssh", "config"))
		expectLine(t, h.run("release"), "ssh route truenas", "missing", "no ~/.ssh/config", 1)
	})
}

func TestDoctorRoleTimeout(t *testing.T) {
	h := newDoctorHost(t)
	h.timeout = 200 * time.Millisecond
	h.fake("gh", "exec /bin/sleep 5")
	start := time.Now()
	o := h.run("release")
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("doctor waited %s for a hung gh", elapsed)
	}
	expectLine(t, o, "gh login", "missing", "timed out", 1)
}

func TestDoctorRoleNoSecrets(t *testing.T) {
	h := newDoctorHost(t)
	h.env["CLOUDFLARE_API_TOKEN"] = "SECRET-CF-456"
	for _, tool := range []string{"gh", "claude", "codex"} {
		h.fake(tool, "echo SECRET-OUT-789; echo SECRET-OUT-789 >&2; exit 0")
	}
	h.fake("launchctl", `echo SECRET-OUT-789 >&2; printf '\tstate = running SECRET-OUT-789\n'`)
	for _, role := range doctorRoles {
		o := h.run(role)
		if strings.Contains(o.text, "SECRET") {
			t.Fatalf("role %s printed a secret:\n%s", role, o.text)
		}
	}
	// Failure paths print no secret either.
	h.fake("gh", "echo SECRET-OUT-789; exit 1")
	h.writeDeployConfig(map[string]any{"enabled": "SECRET-CFG-000"})
	if o := h.run("release"); strings.Contains(o.text, "SECRET") || o.exit != 1 {
		t.Fatalf("release failure printed a secret or passed:\n%s", o.text)
	}
}

func TestDoctorRoleSemver(t *testing.T) {
	for _, c := range []struct {
		version, rng string
		want         bool
	}{
		{"22.0.0", ">=22.0.0", true}, {"21.9.9", ">=22.0.0", false},
		{"1.2.3", "^1.2.0", true}, {"2.0.0", "^1.2.0", false},
		{"0.3.1", "^0.3.0", true}, {"0.4.0", "^0.3.0", false},
		{"1.2.9", "~1.2.3", true}, {"1.3.0", "~1.2.3", false},
		{"18.0.0", ">=16 <20", true}, {"20.0.0", ">=16 <20", false},
		{"20.0.0", ">=20", true}, {"20.1.0", "nonsense", false},
	} {
		if got := semverSatisfies(c.version, c.rng); got != c.want {
			t.Errorf("semverSatisfies(%q, %q) = %v", c.version, c.rng, got)
		}
	}
}

// Wiring through the tt doctor entry point.
func TestDoctorRoleCommandLine(t *testing.T) {
	for _, args := range [][]string{{"--role", "nope"}, {"--repo", "/tmp"}, {"--role", "client", "extra"}, {"--bogus"}} {
		out, err := captureStdout(t, func() error { return cmdDoctorRole(env{}, args) })
		if code := doctorExitCode(err); code != 2 {
			t.Fatalf("%v: exit %d (%v)", args, code, err)
		}
		if !strings.Contains(err.Error(), "client, agent, test, release") || out != "" {
			t.Fatalf("%v: %v / %q", args, err, out)
		}
	}
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		out, err := captureStdout(t, func() error { return cmdDoctorRole(env{}, args) })
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, want := range []string{"--role", "--repo", "--deploy-config", "client", "agent", "test", "release"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%v help lacks %q:\n%s", args, want, out)
			}
		}
	}
}

func TestDoctorRoleUnknownRoleInRunner(t *testing.T) {
	var out bytes.Buffer
	if code := doctorExitCode(runDoctorRole(&out, "nope", newDoctorHost(t).probe())); code != 2 {
		t.Fatalf("exit %d", code)
	}
}
