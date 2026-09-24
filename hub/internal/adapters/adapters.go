// Package adapters renders per-runtime integration snippets for tt.
package adapters

import (
	"encoding/json"
	"fmt"
)

// ClaudeHooks returns a Claude Code settings.json fragment wiring tt hooks.
func ClaudeHooks(tt string) string {
	hook := func(name string) map[string]any {
		return map[string]any{"hooks": []map[string]any{{"type": "command", "command": tt + " hook " + name}}}
	}
	settings := map[string]any{"hooks": map[string]any{
		"SessionStart":     []map[string]any{hook("session-start")},
		"UserPromptSubmit": []map[string]any{hook("prompt")},
		"Stop":             []map[string]any{hook("stop")},
		"Notification":     []map[string]any{hook("notification")},
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
	return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": tt + " " + CodexStopMarker}}}
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
