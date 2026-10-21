package main

import "os"

// homeDir is os.UserHomeDir for hub/cmd/tt. Every read of the home, and every
// setting that names a path in place of one under it (homeSetting), resolves
// here so the package's tests can see it (wi_49c93507dbce6cc2, see
// docs/test-isolation.md). The binary's resolution is os.UserHomeDir's.
func homeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err == nil {
		homeSeen(home)
	}
	return home, err
}

// homeSetting is os.Getenv for a variable naming a home path's replacement:
// TAILTERM_RELAY_STATE, TAILTERM_HANDOFF_CONFIG, TAILTERM_HANDOFF_DIR,
// TAILTERM_TOOL_LEDGER_DIR, TAILTERM_REDACT_SETTING_FILE, CODEX_HOME and
// CLAUDE_CONFIG_DIR.
func homeSetting(name string) string {
	return homeSettingFrom(os.Getenv, name)
}

// homeSettingFrom is homeSetting for code that reads its environment through
// an injected getenv, such as the doctor's probe.
func homeSettingFrom(getenv func(string) string, name string) string {
	value := getenv(name)
	if value != "" {
		homeSeen(value)
	}
	return value
}

// homeWatch sees each path homeDir and homeSetting return. The binary leaves
// it nil. Only TestMain sets it, once, before any test starts.
var homeWatch func(path string)

func homeSeen(path string) {
	if homeWatch != nil {
		homeWatch(path)
	}
}
