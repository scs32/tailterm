package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestReasoningLaunchArguments(t *testing.T) {
	command, err := reasoningCommand("printf '%s\\n'", "codex", "gpt-5.3-codex", "high", "codex")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", "-c", command+" briefing").Output()
	if err != nil || string(out) != "-c\nmodel_reasoning_effort=\"high\"\nbriefing\n" {
		t.Fatalf("argv = %q, err = %v", out, err)
	}
	if unchanged, err := reasoningCommand("codex", "codex", "", "", "node unknown-wrapper.js"); err != nil || unchanged != "codex" {
		t.Fatalf("inherit changed: %q %v", unchanged, err)
	}
	for _, tc := range []struct{ runtime, model, effort string }{
		{"claude", "", "high"}, {"codex", "", "high"}, {"codex", "custom/model", "high"}, {"codex", "gpt-5.3-codex", "max"}, {"codex", "gpt-5.3-codex", "$(id)"},
	} {
		if _, err := reasoningCommand("codex", tc.runtime, tc.model, tc.effort, "codex"); err == nil {
			t.Fatalf("accepted %#v", tc)
		}
	}
	if _, err := reasoningCommand("codex -c model_reasoning_effort=high", "codex", "gpt-5.3-codex", "low", "codex"); err == nil || !strings.Contains(err.Error(), "remove") {
		t.Fatal("accepted conflicting reasoning", err)
	}
	if _, err := reasoningCommand("node unknown-wrapper.js", "codex", "gpt-5.6-sol", "high", "node unknown-wrapper.js"); err == nil || !strings.Contains(err.Error(), "native codex") {
		t.Fatal("accepted custom wrapper reasoning", err)
	}
}
