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
