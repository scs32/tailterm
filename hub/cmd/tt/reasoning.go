package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/scs32/tailterm/hub/internal/spawn"
)

var reasoningFlag = regexp.MustCompile(`(^|\s)(-c|--config)(=|\s).*model_reasoning_effort|model_reasoning_effort\s*=`)
var effortFlag = regexp.MustCompile(`(^|\s)--effort(=|\s|$)`)

var codexReasoningModels = map[string]map[string]bool{
	"gpt-5.3-codex": {"low": true, "medium": true, "high": true, "xhigh": true},
	"gpt-6-astra":   {"low": true, "medium": true, "high": true, "xhigh": true},
	"gpt-6-sol":     {"low": true, "medium": true, "high": true, "xhigh": true},
	"gpt-6-luna":    {"low": true, "medium": true, "high": true, "xhigh": true},
	"gpt-5.6-sol":   {"low": true, "medium": true, "high": true, "xhigh": true},
	"gpt-5.6-terra": {"low": true, "medium": true, "high": true, "xhigh": true},
	"gpt-5.6-luna":  {"low": true, "medium": true, "high": true, "xhigh": true},
}

// Claude Code --effort levels per exact model ID. Aliases (opus, sonnet)
// follow the server's configuration and stay inherit-only.
var claudeEffortModels = map[string]map[string]bool{
	"claude-opus-5-5":  {"low": true, "medium": true, "high": true, "xhigh": true, "max": true},
	"claude-fable-5-1": {"low": true, "medium": true, "high": true, "xhigh": true, "max": true},
	"claude-opus-5":    {"low": true, "medium": true, "high": true, "xhigh": true, "max": true},
	"claude-sonnet-5":  {"low": true, "medium": true, "high": true, "xhigh": true, "max": true},
}

// Explicit reasoning is supported only for exact documented Codex and Claude
// model IDs. Empty reasoning preserves the runtime's host/model default without
// an argv override. Unknown/custom models intentionally remain inherit-only.
func reasoningCommand(command, runtime, model, reasoning, run string) (string, error) {
	if reasoning == "" {
		return command, nil
	}
	models := map[string]map[string]map[string]bool{"codex": codexReasoningModels, "claude": claudeEffortModels}[runtime]
	if models == nil {
		return "", fmt.Errorf("explicit reasoning is supported for Codex and Claude only")
	}
	if !models[model][reasoning] {
		return "", fmt.Errorf("unsupported %s model and reasoning combination; choose inherit", runtime)
	}
	if strings.TrimSpace(run) != runtime {
		return "", fmt.Errorf("explicit reasoning requires the verified native %s command; custom command overrides must use inherit", runtime)
	}
	if runtime == "claude" {
		if effortFlag.MatchString(command) {
			return "", fmt.Errorf("remove --effort from the command override or choose inherit")
		}
		return command + " " + spawn.ShellQuote("--effort") + " " + spawn.ShellQuote(reasoning), nil
	}
	if reasoningFlag.MatchString(command) {
		return "", fmt.Errorf("remove model_reasoning_effort from the command override or choose inherit")
	}
	return command + " " + spawn.ShellQuote("-c") + " " + spawn.ShellQuote("model_reasoning_effort=\""+reasoning+"\""), nil
}
