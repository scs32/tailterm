package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestReasoningLaunchArguments(t *testing.T) {
	command, err := reasoningCommand("printf '%s\\n'", "codex", "gpt-5.3-codex", "high")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", "-c", command+" briefing").Output()
	if err != nil || string(out) != "-c\nmodel_reasoning_effort=\"high\"\nbriefing\n" {
		t.Fatalf("argv = %q, err = %v", out, err)
	}
	if unchanged, err := reasoningCommand("codex", "codex", "", ""); err != nil || unchanged != "codex" {
		t.Fatalf("inherit changed: %q %v", unchanged, err)
	}
	for _, tc := range []struct{ runtime, model, effort string }{
		{"claude", "", "high"}, {"codex", "", "high"}, {"codex", "custom/model", "high"}, {"codex", "gpt-5.3-codex", "max"}, {"codex", "gpt-5.3-codex", "$(id)"},
	} {
		if _, err := reasoningCommand("codex", tc.runtime, tc.model, tc.effort); err == nil {
			t.Fatalf("accepted %#v", tc)
		}
	}
	if _, err := reasoningCommand("codex -c model_reasoning_effort=high", "codex", "gpt-5.3-codex", "low"); err == nil || !strings.Contains(err.Error(), "remove") {
		t.Fatal("accepted conflicting reasoning", err)
	}
}
