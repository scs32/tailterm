package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// homeSettingNames are the variables that name a home path's replacement;
// product code reads them only through homeSetting.
var homeSettingNames = []string{"TAILTERM_RELAY_STATE", "TAILTERM_HANDOFF_CONFIG", "TAILTERM_HANDOFF_DIR", "TAILTERM_TOOL_LEDGER_DIR", "TAILTERM_REDACT_SETTING_FILE", "CODEX_HOME", "CLAUDE_CONFIG_DIR"}

// g1: a test points the seam at a temporary root through HOME, and the seam
// returns what os.UserHomeDir and os.Getenv return.
func TestHomeSeamFollowsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range homeSettingNames {
		t.Setenv(name, "")
	}
	got, err := homeDir()
	want, wantErr := os.UserHomeDir()
	if got != want || (err == nil) != (wantErr == nil) || got != home {
		t.Fatalf("homeDir() = %q, %v; os.UserHomeDir() = %q, %v; HOME %q", got, err, want, wantErr, home)
	}
	journal, err := teamJournalPath("http://hub", "tsk_1", "wi_1", 1)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := newHostPaths()
	if err != nil {
		t.Fatal(err)
	}
	for what, path := range map[string]string{
		"relayDir": relayDir(), "handoffConfigPath": handoffConfigPath(), "handoffRoot": handoffRoot(),
		"toolLedgerRoot": toolLedgerRoot(), "toolRedactSettingPath": toolRedactSettingPath(), "codexHome": codexHome(),
		"claudeUsageCapturePath": claudeUsageCapturePath(), "teamJournalPath": journal,
		"hostPaths.install": paths.install, "hostPaths.codexHooks": paths.codexHooks,
		"hostPaths.claudeSettings": paths.claudeSettings, "hostPaths.hubConfig": paths.hubConfig,
		"hostPaths.relayLock": paths.relayLock, "hostPaths.restartPending": paths.restartPending,
	} {
		if !pathWithinAny(path, []string{home}) || path == home {
			t.Errorf("%s = %q, not below the test's home %q", what, path, home)
		}
	}
	for _, value := range []string{"", filepath.Join(home, "elsewhere")} {
		for _, name := range homeSettingNames {
			t.Setenv(name, value)
			if got := homeSetting(name); got != os.Getenv(name) || got != value {
				t.Errorf("homeSetting(%s) = %q, os.Getenv %q, set %q", name, got, os.Getenv(name), value)
			}
		}
	}
}

// g1: product code reads the home and the home settings only through the
// seam in home.go.
func TestHomeSeamOnlyCaller(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	settings := map[string]bool{}
	for _, name := range homeSettingNames {
		settings[name] = true
	}
	fset := token.NewFileSet()
	checked := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "home.go" {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "os" {
				return true
			}
			switch sel.Sel.Name {
			case "UserHomeDir":
				t.Errorf("%s: os.UserHomeDir outside home.go; use homeDir", fset.Position(call.Pos()))
			case "Getenv", "LookupEnv":
				if len(call.Args) == 1 {
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if name, err := strconv.Unquote(lit.Value); err == nil && settings[name] {
							t.Errorf("%s: os.%s(%q) outside home.go; use homeSetting", fset.Position(call.Pos()), sel.Sel.Name, name)
						}
					}
				}
			}
			return true
		})
	}
	if checked < 20 {
		t.Fatalf("checked only %d files", checked)
	}
	// verificationCacheRoot takes HOME from an injected getenv, so it reports
	// the home it resolved to the seam itself.
	src, err := os.ReadFile("go_cache_trim.go")
	if err != nil {
		t.Fatal(err)
	}
	_, body, _ := strings.Cut(string(src), "func verificationCacheRoot(")
	body, _, _ = strings.Cut(body, "\n}\n")
	if !strings.Contains(body, `getenv("HOME")`) || !strings.Contains(body, "homeSeen(home)") {
		t.Fatal("verificationCacheRoot no longer passes the home it resolved to homeSeen")
	}
}

// homeProbeStart and homeProbeRead put two goroutines that are not tests
// between the probe and its read, so the guard has to follow the starters.
func homeProbeStart(done chan struct{}) {
	inner := make(chan struct{})
	go homeProbeRead(inner)
	<-inner
	close(done)
}

func homeProbeRead(done chan struct{}) {
	defer close(done)
	handoffRoot()
}

// homeGuardProbeEnv names the case TestHomeGuardProbe runs, and
// homeGuardProbeRealEnv the fake real home it reads.
const (
	homeGuardProbeEnv     = "TT_HOME_GUARD_PROBE"
	homeGuardProbeRealEnv = "TT_HOME_GUARD_PROBE_REAL"
)

// TestHomeGuardProbe is not a test on its own: TestHomeGuardNamesTest runs
// it in a child with a temporary directory declared as the real home, and
// each case resolves a path through the seam. Every override is a t.Setenv,
// restored before the child exits.
func TestHomeGuardProbe(t *testing.T) {
	probe := os.Getenv(homeGuardProbeEnv)
	if probe == "" {
		t.Skip("run by TestHomeGuardNamesTest")
	}
	real := os.Getenv(homeGuardProbeRealEnv)
	switch probe {
	case "home":
		t.Setenv("HOME", real)
		relayDir()
	case "setting":
		t.Setenv("TAILTERM_RELAY_STATE", filepath.Join(real, "state"))
		relayDir()
	case "goroutine":
		t.Setenv("HOME", real)
		done := make(chan struct{})
		go homeProbeStart(done)
		<-done
	case "swallowed":
		t.Setenv("HOME", real)
		func() {
			defer func() { _ = recover() }()
			relayDir()
		}()
	case "clean":
		relayDir()
		handoffRoot()
	default:
		t.Fatalf("unknown probe %q", probe)
	}
}

// g2: the guard fails the exact test that resolves a path inside the real
// home, naming the path, the caller and the test, in its own goroutine, in
// one it started, and when product code swallows the failure. The real home
// here is a temporary directory; nothing reads the user's.
func TestHomeGuardNamesTest(t *testing.T) {
	base := t.TempDir()
	real, home, tmp := filepath.Join(base, "real"), filepath.Join(base, "home"), filepath.Join(base, "tmp")
	for _, dir := range []string{real, home, tmp} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	drop := map[string]bool{"HOME": true, "TMPDIR": true, testRealHomesEnv: true, homeGuardProbeEnv: true, homeGuardProbeRealEnv: true, "TT_LIVE_CLAUDE": true, "TT_LIVE_RUNTIME_PROMPT": true, redactProbeHelperEnv: true}
	var env []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !drop[name] {
			env = append(env, kv)
		}
	}
	env = append(env, testRealHomesEnv+"="+real, "HOME="+home, "TMPDIR="+tmp, homeGuardProbeRealEnv+"="+real)
	cases := []struct {
		probe, extra string
		code         int // -1: any non-zero exit
		want         []string
	}{
		{"home", "", -1, []string{"--- FAIL: TestHomeGuardProbe", "real-home guard: TestHomeGuardProbe resolved " + real + " inside the real user home", "relayDir (relay.go:"}},
		{"setting", "", -1, []string{"--- FAIL: TestHomeGuardProbe", "real-home guard: TestHomeGuardProbe resolved " + filepath.Join(real, "state") + " inside the real user home", "relayDir (relay.go:"}},
		{"goroutine", "", -1, []string{"real-home guard: TestHomeGuardProbe resolved " + real + " inside the real user home", "handoffRoot (handoff.go:"}},
		{"swallowed", "", 1, []string{"--- PASS: TestHomeGuardProbe", "after the tests: real-home guard: TestHomeGuardProbe resolved " + real + " inside the real user home", "relayDir (relay.go:"}},
		{"clean", "", 0, nil},
		{"home", "TT_LIVE_CLAUDE=1", 0, nil},
		{"home", redactProbeHelperEnv + "=resolve", 0, nil},
	}
	for _, c := range cases {
		name := c.probe
		if c.extra != "" {
			name += " " + c.extra
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHomeGuardProbe$", "-test.count=1", "-test.v")
		cmd.Env = append(append([]string{}, env...), homeGuardProbeEnv+"="+c.probe)
		if c.extra != "" {
			cmd.Env = append(cmd.Env, c.extra)
		}
		out, err := cmd.CombinedOutput()
		cancel()
		code := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if (c.code < 0 && code == 0) || (c.code >= 0 && code != c.code) {
			t.Errorf("%s: exit %d, want %d\n%s", name, code, c.code, out)
			continue
		}
		for _, want := range c.want {
			if !strings.Contains(string(out), want) {
				t.Errorf("%s: output lacks %q\n%s", name, want, out)
			}
		}
		if c.want == nil && strings.Contains(string(out), "real-home guard") {
			t.Errorf("%s: unexpected guard line\n%s", name, out)
		}
	}
}
