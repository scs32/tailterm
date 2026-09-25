package spawn

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
