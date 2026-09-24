package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/scs32/tailterm/hub/internal/adapters"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// Broker phase 3.1: Codex runs the same turn-end check as Claude. Codex reads
// hooks from $CODEX_HOME/hooks.json (default ~/.codex) and only runs hooks
// it trusts; session-level config and trust flags are ignored (verified with
// codex-cli 0.156.1), so Tailterm installs the hook file once and launches
// its Codex agents with --dangerously-bypass-hook-trust. The hook itself
// does nothing outside a Tailterm agent session.

func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// codexStopHookInstalled reports whether hooks.json has the Tailterm Stop hook.
func codexStopHookInstalled() bool {
	b, err := os.ReadFile(filepath.Join(codexHome(), "hooks.json"))
	return err == nil && strings.Contains(string(b), adapters.CodexStopMarker)
}

// codexHookCommand adds the trust bypass to a native Codex agent command, but
// only when the Tailterm Stop hook is installed: the bypass then covers a
// hook Tailterm put there.
func codexHookCommand(command, runtime, run string) string {
	if runtime != "codex" || strings.TrimSpace(run) != "codex" || !codexStopHookInstalled() {
		return command
	}
	return command + " " + spawn.ShellQuote("--dangerously-bypass-hook-trust")
}

// installCodexStopHook merges the Tailterm Stop hook into hooks.json,
// keeping every other hook, and is idempotent.
func installCodexStopHook(tt string) error {
	path := filepath.Join(codexHome(), "hooks.json")
	doc := map[string]any{}
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s is not valid JSON; fix it before installing: %w", path, err)
		}
	}
	if codexStopHookInstalled() {
		fmt.Println("The Tailterm Stop hook is already installed in", path)
		return nil
	}
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	stop, _ := hooks["Stop"].([]any)
	hooks["Stop"] = append(stop, adapters.CodexStopHook(tt))
	doc["hooks"] = hooks
	out, _ := json.MarshalIndent(doc, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	fmt.Println("Installed the Tailterm Stop hook in", path)
	return nil
}
