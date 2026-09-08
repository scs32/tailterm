package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestModelLaunchArguments(t *testing.T) {
	for _, runtime := range []string{"claude", "codex", "aider", "gemini"} {
		command, err := modelCommand("printf '%s\\n'", runtime, "provider/model-v2:latest")
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("/bin/sh", "-c", command+" 'task briefing'").Output()
		if err != nil || string(out) != "--model\nprovider/model-v2:latest\ntask briefing\n" {
			t.Fatalf("%s: %q %v", runtime, out, err)
		}
	}
	for _, runtime := range []string{"codex", "generic", "custom"} {
		command := "wrapper --existing-config 'two words'"
		got, err := modelCommand(command, runtime, "")
		if err != nil || got != command {
			t.Fatalf("default changed: %q %v", got, err)
		}
	}
	for _, model := range []string{"--help", "x;id", "$(id)", "a\nb", "two words", strings.Repeat("x", 201)} {
		if _, err := modelCommand("codex", "codex", model); err == nil {
			t.Fatalf("accepted %q", model)
		}
	}
	if _, err := modelCommand("custom", "generic", "model"); err == nil {
		t.Fatal("custom runtime accepted a model flag")
	}
}
