package spawn

import "testing"

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
