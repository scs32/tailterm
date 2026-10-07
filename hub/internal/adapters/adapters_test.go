package adapters

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeHooksIsValidJSON(t *testing.T) {
	var v struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal([]byte(ClaudeHooks("/usr/local/bin/tt")), &v); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"UserPromptSubmit", "Stop", "Notification"} {
		if len(v.Hooks[ev]) != 1 || !strings.HasPrefix(v.Hooks[ev][0].Hooks[0].Command, "/usr/local/bin/tt hook ") {
			t.Errorf("%s hook missing or wrong: %+v", ev, v.Hooks[ev])
		}
	}
	// The tool-call ledger: one unmatched group per tool event, so every
	// tool call runs tt hook tool.
	var raw struct {
		Hooks map[string][]map[string]json.RawMessage
	}
	if err := json.Unmarshal([]byte(ClaudeHooks("/usr/local/bin/tt")), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Hooks) != 9 {
		t.Errorf("fragment wires %d events; want 9", len(raw.Hooks))
	}
	for _, ev := range []string{"PreToolUse", "PostToolUse", "PostToolUseFailure"} {
		if len(v.Hooks[ev]) != 1 || len(v.Hooks[ev][0].Hooks) != 1 || v.Hooks[ev][0].Hooks[0].Type != "command" || v.Hooks[ev][0].Hooks[0].Command != "/usr/local/bin/tt hook tool" {
			t.Errorf("%s hook missing or wrong: %+v", ev, v.Hooks[ev])
			continue
		}
		if _, restricted := raw.Hooks[ev][0]["matcher"]; restricted || len(raw.Hooks[ev][0]) != 1 {
			t.Errorf("%s group is not a bare hooks list: %s", ev, raw.Hooks[ev][0])
		}
	}
	// Only the tool and handoff entries carry a timeout; the four older
	// entries are written exactly as before. On SessionStart the handoff hook
	// is a second group, so its timeout is not on tt hook session-start.
	handoff := `[{"command":"/usr/local/bin/tt hook handoff","timeout":5,"type":"command"}]`
	for ev, group := range raw.Hooks {
		want := `[{"command":"/usr/local/bin/tt hook tool","timeout":5,"type":"command"}]`
		groups := 1
		switch ev {
		case "SessionStart":
			want, groups = `[{"command":"/usr/local/bin/tt hook session-start","type":"command"}]`, 2
		case "PreCompact", "SessionEnd":
			want = handoff
		case "UserPromptSubmit":
			want = `[{"command":"/usr/local/bin/tt hook prompt","type":"command"}]`
		case "Stop":
			want = `[{"command":"/usr/local/bin/tt hook stop","type":"command"}]`
		case "Notification":
			want = `[{"command":"/usr/local/bin/tt hook notification","type":"command"}]`
		}
		if len(group) != groups {
			t.Errorf("%s has %d groups; want %d", ev, len(group), groups)
			continue
		}
		for i, want := range []string{want, handoff}[:groups] {
			var entries any
			if err := json.Unmarshal(group[i]["hooks"], &entries); err != nil {
				t.Fatal(err)
			}
			if got, _ := json.Marshal(entries); string(got) != want {
				t.Errorf("%s group %d entries = %s; want %s", ev, i, got, want)
			}
			if _, restricted := group[i]["matcher"]; restricted || len(group[i]) != 1 {
				t.Errorf("%s group %d is not a bare hooks list: %s", ev, i, group[i])
			}
		}
	}
	if ToolHookTimeoutSeconds != 5 || HookTimeoutSeconds("tool") != 5 || HookTimeoutSeconds("stop") != 0 {
		t.Errorf("timeouts: tool %d, stop %d", HookTimeoutSeconds("tool"), HookTimeoutSeconds("stop"))
	}
	if HandoffHookTimeoutSeconds != 5 || HookTimeoutSeconds("handoff") != 5 || HookTimeoutSeconds("session-start") != 0 {
		t.Errorf("timeouts: handoff %d, session-start %d", HookTimeoutSeconds("handoff"), HookTimeoutSeconds("session-start"))
	}
	if !strings.Contains(CodexConfig("tt"), `notify = ["tt", "hook", "codex"]`) {
		t.Error("codex config wrong")
	}
}

func TestHookCommandQuotesOnlyUnusualPaths(t *testing.T) {
	for _, c := range []struct{ tt, want string }{
		{"tt", "tt hook stop"},
		{"/usr/local/bin/tt", "/usr/local/bin/tt hook stop"},
		{"/opt/tt-1.2+x/_bin/tt", "/opt/tt-1.2+x/_bin/tt hook stop"},
		{"/Users/a b/.local/bin/tt", "'/Users/a b/.local/bin/tt' hook stop"},
		{"/Users/o'brien/tt", `'/Users/o'\''brien/tt' hook stop`},
		{"/tmp/$HOME/tt", "'/tmp/$HOME/tt' hook stop"},
	} {
		if got := HookCommand(c.tt, "stop"); got != c.want {
			t.Errorf("HookCommand(%q) = %q, want %q", c.tt, got, c.want)
		}
	}
	if got := CodexStopHook("/a b/tt")["hooks"].([]any)[0].(map[string]any)["command"]; got != "'/a b/tt' hook stop" {
		t.Errorf("CodexStopHook did not quote: %q", got)
	}
	if !strings.Contains(ClaudeHooks("/a b/tt"), `"'/a b/tt' hook session-start"`) {
		t.Errorf("ClaudeHooks did not quote: %s", ClaudeHooks("/a b/tt"))
	}
}

func TestParseHookCommandRoundTrip(t *testing.T) {
	for _, tt := range []string{"tt", "/usr/local/bin/tt", "/Users/a b/.local/bin/tt", "/Users/o'brien/tt", "/tmp/$HOME/tt", `/tmp/"q"/tt`, "/tmp/tab\there/tt"} {
		for _, name := range []string{"stop", "session-start", "prompt", "notification"} {
			exe, got, ok := ParseHookCommand(HookCommand(tt, name))
			if !ok || exe != tt || got != name {
				t.Errorf("round trip of %q %q = %q %q %v", tt, name, exe, got, ok)
			}
		}
	}
	if exe, name, ok := ParseHookCommand("  /opt/my\\ tools/tt   hook   stop "); !ok || exe != "/opt/my tools/tt" || name != "stop" {
		t.Errorf("backslash-escaped space = %q %q %v", exe, name, ok)
	}
	for _, cmd := range []string{"", "tt", "tt hook", "tt hook stop now", "echo hook stop now", "tt hooks stop", "'/a b/tt hook stop", `"/a b/tt" hook stop`, "$HOME/tt hook stop", "tt hook Stop", "tt hook stop;rm", "tt\\"} {
		if exe, name, ok := ParseHookCommand(cmd); ok {
			t.Errorf("ParseHookCommand(%q) accepted as %q %q", cmd, exe, name)
		}
	}
}
