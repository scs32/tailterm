package main

import (
	"fmt"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"path/filepath"
	"regexp"
	"strings"
)

var permissionFlag = regexp.MustCompile(`(^|\s)(--sandbox|--ask-for-approval|--permission-mode|--dangerously-bypass-approvals-and-sandbox|--dangerously-skip-permissions|-s|-a)(=|\s|$)`)

// The owner can still use host configuration or a custom wrapper. Explicit UI
// choices reject conflicting flags rather than relying on last-flag precedence.
func permissionCommand(command, runtime, mode, cwd string, allowed []string) (string, error) {
	if len(allowed) > 30 {
		return "", fmt.Errorf("too many allowed tool rules")
	}
	for _, rule := range allowed {
		if len(rule) > 200 || strings.TrimSpace(rule) == "" || strings.IndexFunc(rule, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return "", fmt.Errorf("invalid allowed tool rule")
		}
	}
	if len(allowed) > 0 && runtime != "claude" {
		return "", fmt.Errorf("allowed tool rules require Claude Code")
	}
	if mode != "" && permissionFlag.MatchString(command) {
		return "", fmt.Errorf("remove permission flags from the command override or choose Host settings")
	}
	args := []string{}
	switch runtime {
	case "codex":
		switch mode {
		case "":
		case "on-request":
			args = []string{"--ask-for-approval", "on-request"}
		case "workspace-auto":
			if !filepath.IsAbs(cwd) {
				return "", fmt.Errorf("workspace permissions require an explicit absolute working directory")
			}
			args = []string{"--sandbox", "workspace-write", "--ask-for-approval", "never", "-c", "sandbox_workspace_write.network_access=true", "--add-dir", relayDir()}
		case "full-auto":
			args = []string{"--sandbox", "danger-full-access", "--ask-for-approval", "never"}
		default:
			return "", fmt.Errorf("unsupported Codex permission mode")
		}
	case "claude":
		switch mode {
		case "":
		case "acceptEdits", "auto", "dontAsk", "bypassPermissions":
			args = []string{"--permission-mode", mode}
		default:
			return "", fmt.Errorf("unsupported Claude permission mode")
		}
		for _, rule := range allowed {
			args = append(args, "--allowedTools", rule)
		}
	default:
		if mode != "" {
			return "", fmt.Errorf("permission presets are supported for Codex and Claude Code only")
		}
	}
	for _, arg := range args {
		command += " " + spawn.ShellQuote(arg)
	}
	return command, nil
}
