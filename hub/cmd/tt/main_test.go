package main

import (
	"os"
	"strings"
	"testing"
)

// TestMain drops TAILTERM_* variables inherited from the shell, so the suite
// behaves the same when a Tailterm-launched agent runs it (a reviewer's
// TAILTERM_REASONING would otherwise leak into spawn defaults).
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "TAILTERM_") {
			os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}
