package main

import (
	"fmt"
	"regexp"

	"github.com/scs32/tailterm/hub/internal/spawn"
)

var reasoningFlag = regexp.MustCompile(`(^|\s)(-c|--config)(=|\s).*model_reasoning_effort|model_reasoning_effort\s*=`)

var codexReasoningModels = map[string]map[string]bool{
	"gpt-5.3-codex": {"low": true, "medium": true, "high": true, "xhigh": true},
	"gpt-6-astra":   {"low": true, "medium": true, "high": true, "xhigh": true},
	"gpt-5.6-sol":   {"low": true, "medium": true, "high": true, "xhigh": true},
	"gpt-5.6-terra": {"low": true, "medium": true, "high": true, "xhigh": true},
	"gpt-5.6-luna":  {"low": true, "medium": true, "high": true, "xhigh": true},
}

// Explicit reasoning is supported only for exact documented Codex model IDs.
// Empty reasoning preserves the runtime's host/model default without an argv
// override. Unknown/custom models intentionally remain inherit-only.
func reasoningCommand(command, runtime, model, reasoning string) (string, error) {
	if reasoning == "" {
		return command, nil
	}
	if runtime != "codex" {
		return "", fmt.Errorf("explicit reasoning is supported for Codex only")
	}
	if !codexReasoningModels[model][reasoning] {
		return "", fmt.Errorf("unsupported Codex model and reasoning combination; choose inherit")
	}
	if reasoningFlag.MatchString(command) {
		return "", fmt.Errorf("remove model_reasoning_effort from the command override or choose inherit")
	}
	return command + " " + spawn.ShellQuote("-c") + " " + spawn.ShellQuote("model_reasoning_effort=\""+reasoning+"\""), nil
}
