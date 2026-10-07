// Package adapters renders per-runtime integration snippets for tt.
package adapters

import (
	"encoding/json"
	"fmt"
	"strings"
)

// HookCommand is the shell command a runtime runs for one tt hook. A tt path
// made only of ordinary path characters stays bare; any other path (a home
// directory with a space, for example) is single-quoted so a shell reads it
// as one word.
func HookCommand(tt, name string) string {
	return quoteHookPath(tt) + " hook " + name
}

func plainHookPathByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("_/.+-", c) >= 0
}

func quoteHookPath(tt string) string {
	plain := tt != ""
	for i := 0; i < len(tt); i++ {
		if !plainHookPathByte(tt[i]) {
			plain = false
			break
		}
	}
	if plain {
		return tt
	}
	return "'" + strings.ReplaceAll(tt, "'", `'\''`) + "'"
}

// ParseHookCommand reads a command written by HookCommand: a first word that
// is bare or single-quoted, then exactly "hook <name>". It reports the
// executable and the hook name; any other command is not ok.
func ParseHookCommand(cmd string) (exe, name string, ok bool) {
	s := strings.TrimSpace(cmd)
	var word strings.Builder
	i := 0
	for i < len(s) && s[i] != ' ' && s[i] != '\t' {
		switch c := s[i]; {
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return "", "", false
			}
			word.WriteString(s[i+1 : i+1+end])
			i += end + 2
		case c == '\\':
			if i+1 >= len(s) {
				return "", "", false
			}
			word.WriteByte(s[i+1])
			i += 2
		case c == '"' || c == '$' || c == '`':
			return "", "", false
		default:
			word.WriteByte(c)
			i++
		}
	}
	rest := strings.Fields(s[i:])
	if word.Len() == 0 || len(rest) != 2 || rest[0] != "hook" {
		return "", "", false
	}
	for j := 0; j < len(rest[1]); j++ {
		if c := rest[1][j]; (c < 'a' || c > 'z') && c != '-' {
			return "", "", false
		}
	}
	return word.String(), rest[1], true
}

// ToolHookTimeoutSeconds is the timeout Claude Code is given for tt hook
// tool. The hook returns within 200 ms; if it ever hangs, Claude Code
// abandons it after this long and the tool call proceeds.
const ToolHookTimeoutSeconds = 5

// HandoffHookTimeoutSeconds is the timeout Claude Code is given for tt hook
// handoff. The hook returns within 500 ms; if it ever hangs, Claude Code
// abandons it after this long and the start, compaction or exit proceeds.
const HandoffHookTimeoutSeconds = 5

// HookTimeoutSeconds is the timeout a tt hook's settings entry carries, or
// zero for an entry written without one.
func HookTimeoutSeconds(name string) int {
	switch name {
	case "tool":
		return ToolHookTimeoutSeconds
	case "handoff":
		return HandoffHookTimeoutSeconds
	}
	return 0
}

// HookEntry is the settings entry that runs tt hook <name>.
func HookEntry(tt, name string) map[string]any {
	entry := map[string]any{"type": "command", "command": HookCommand(tt, name)}
	if seconds := HookTimeoutSeconds(name); seconds > 0 {
		entry["timeout"] = seconds
	}
	return entry
}

// ClaudeHooks returns a Claude Code settings.json fragment wiring tt hooks.
// The three tool events run the observe-only tool-call ledger (tt hook tool)
// with no matcher, so every tool call is seen. SessionStart, PreCompact and
// SessionEnd run the session handoff hook (tt hook handoff), which does
// nothing unless the host setting turns it on; on SessionStart it is its own
// entry, so its timeout never lands on tt hook session-start.
func ClaudeHooks(tt string) string {
	hook := func(name string) map[string]any {
		return map[string]any{"hooks": []map[string]any{HookEntry(tt, name)}}
	}
	settings := map[string]any{"hooks": map[string]any{
		"SessionStart":       []map[string]any{hook("session-start"), hook("handoff")},
		"PreCompact":         []map[string]any{hook("handoff")},
		"SessionEnd":         []map[string]any{hook("handoff")},
		"UserPromptSubmit":   []map[string]any{hook("prompt")},
		"Stop":               []map[string]any{hook("stop")},
		"Notification":       []map[string]any{hook("notification")},
		"PreToolUse":         []map[string]any{hook("tool")},
		"PostToolUse":        []map[string]any{hook("tool")},
		"PostToolUseFailure": []map[string]any{hook("tool")},
	}}
	b, _ := json.MarshalIndent(settings, "", "  ")
	return string(b) + "\n"
}

// CodexConfig returns the ~/.codex/config.toml lines wiring Codex notify,
// and the hooks.json that blocks a Tailterm agent's turn from ending while
// it holds unacknowledged work (broker phase 3.1).
func CodexConfig(tt string) string {
	return fmt.Sprintf("# Add to ~/.codex/config.toml\nnotify = [%q, \"hook\", \"codex\"]\n\n# ~/.codex/hooks.json (install with: tt hooks codex --install)\n%s", tt, CodexHooksJSON(tt))
}

// CodexStopMarker identifies the Tailterm Stop hook in hooks.json.
const CodexStopMarker = "hook stop"

// CodexStopHook is the Stop hook entry Tailterm adds to Codex's hooks.json.
// Outside a Tailterm agent session, tt hook stop does nothing.
func CodexStopHook(tt string) map[string]any {
	return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": HookCommand(tt, "stop")}}}
}

func CodexHooksJSON(tt string) string {
	b, _ := json.MarshalIndent(map[string]any{"hooks": map[string]any{"Stop": []any{CodexStopHook(tt)}}}, "", "  ")
	return string(b) + "\n"
}

// Generic explains the runtime-agnostic path.
func Generic(tt string) string {
	return fmt.Sprintf(`# Any command works as an agent: tt spawn wraps it so the hub learns when it
# starts and exits. Inside the session, the agent (or you) can run:
#   %[1]s inbox --unread --mark-read     read task messages
#   %[1]s post "text" [--to <agent>]     message the task or one agent
#   %[1]s event needs_input --text "..."  ask the humans for something
#   %[1]s spawn --name <n> --run "<cmd>"  add a sibling agent on this host
`, tt)
}
