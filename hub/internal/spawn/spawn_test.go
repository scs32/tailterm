package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	payload := strings.Repeat("'", 256*1024)
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
