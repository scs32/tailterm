package main

import (
	"fmt"
	"regexp"

	"github.com/scs32/tailterm/hub/internal/spawn"
)

var modelNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,199}$`)

// An omitted model preserves host configuration. A supplied model is one
// quoted argument, never shell text. Custom wrappers manage their own flags.
func modelCommand(command, runtime, model string) (string, error) {
	if model == "" {
		return command, nil
	}
	if !modelNamePattern.MatchString(model) {
		return "", fmt.Errorf("invalid model name")
	}
	switch runtime {
	case "claude", "codex", "aider", "gemini":
		return command + " --model " + spawn.ShellQuote(model), nil
	default:
		return "", fmt.Errorf("--model is supported for claude, codex, aider and gemini; include custom model options in --run")
	}
}
