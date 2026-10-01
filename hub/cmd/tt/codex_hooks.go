package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// codexStopHookInstalled reports whether hooks.json has a Stop hook whose
// command is a tt binary running `hook stop`.
func codexStopHookInstalled() bool {
	b, err := os.ReadFile(filepath.Join(codexHome(), "hooks.json"))
	if err != nil {
		return false
	}
	var doc struct {
		Hooks struct {
			Stop []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return false
	}
	for _, entry := range doc.Hooks.Stop {
		for _, h := range entry.Hooks {
			if isTTHook(h.Command, "stop") {
				return true
			}
		}
	}
	return false
}

// codexHookCommand adds the trust bypass to a Codex agent command whose
// executable is codex (by name or path), but only when the Tailterm Stop hook
// is installed: the bypass then covers a hook Tailterm put there.
func codexHookCommand(command, runtime, run string) string {
	fields := strings.Fields(run)
	if runtime != "codex" || len(fields) == 0 || filepath.Base(fields[0]) != "codex" || !codexStopHookInstalled() {
		return command
	}
	return command + " " + spawn.ShellQuote("--dangerously-bypass-hook-trust")
}

// installCodexStopHook merges the Tailterm Stop hook into hooks.json with
// the same validation and merge rule as tt host setup, keeping every other
// hook, and is idempotent. A bare tt name leaves an installed hook as it is,
// so it never replaces the absolute path host setup wrote.
func installCodexStopHook(tt string) error {
	path := filepath.Join(codexHome(), "hooks.json")
	out, err := mergeJSONFile(path, true, func(doc map[string]any) (bool, error) {
		return mergeTTHooks(doc, codexHookEvents, tt, !filepath.IsAbs(tt))
	})
	if err != nil {
		return fmt.Errorf("%w; nothing was changed", err)
	}
	if !out.changed {
		fmt.Println("The Tailterm Stop hook is already installed in", path)
		return nil
	}
	fmt.Println("Installed the Tailterm Stop hook in", path)
	return nil
}
