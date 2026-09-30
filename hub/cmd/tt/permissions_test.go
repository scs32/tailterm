package main

import (
	"strings"
	"testing"
)

func TestPermissionModesPreserveDefaultsAndBoundExplicitModes(t *testing.T) {
	if got, e := permissionCommand("codex --search", "codex", "", "", nil); e != nil || got != "codex --search" {
		t.Fatal(got, e)
	}
	if _, e := permissionCommand("codex", "codex", "workspace-auto", "", nil); e == nil {
		t.Fatal("workspace mode without explicit directory")
	}
	got, e := permissionCommand("codex", "codex", "workspace-auto", "/work", nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, part := range []string{"'workspace-write'", "'never'", "sandbox_workspace_write.network_access=true", "'--add-dir'"} {
		if !strings.Contains(got, part) {
			t.Fatal(got)
		}
	}
	if _, e := permissionCommand("codex --sandbox danger-full-access", "codex", "workspace-auto", "/work", nil); e == nil {
		t.Fatal("conflicting permission flags accepted")
	}
	if _, e := permissionCommand("gemini", "gemini", "full-auto", "/work", nil); e == nil {
		t.Fatal("unsupported runtime accepted")
	}
	got, e = permissionCommand("claude", "claude", "dontAsk", "/work", []string{"Read", "Bash(tt *)"})
	if e != nil || !strings.Contains(got, "'--allowedTools' 'Bash(tt *)'") {
		t.Fatal(got, e)
	}
	if _, e := permissionCommand("codex", "codex", "", "/work", []string{"Read"}); e == nil {
		t.Fatal("Claude rules on Codex")
	}
}

func TestClaudeLaunchesDisallowInteractiveQuestions(t *testing.T) {
	const rule = "'--disallowedTools=AskUserQuestion'"
	for _, mode := range []string{"", "acceptEdits", "auto", "dontAsk", "bypassPermissions"} {
		for _, allowed := range [][]string{nil, {"Read", "Bash(tt *)"}} {
			got, e := permissionCommand("claude", "claude", mode, "/work", allowed)
			if e != nil || strings.Count(got, rule) != 1 {
				t.Fatalf("mode %q allowed %v: %q %v", mode, allowed, got, e)
			}
		}
	}
	for _, c := range []struct{ runtime, mode, cwd string }{{"codex", "", ""}, {"codex", "workspace-auto", "/work"}, {"aider", "", ""}} {
		got, e := permissionCommand(c.runtime, c.runtime, c.mode, c.cwd, nil)
		if e != nil || strings.Contains(got, "disallowedTools") {
			t.Fatalf("%s %q: %q %v", c.runtime, c.mode, got, e)
		}
	}
}
