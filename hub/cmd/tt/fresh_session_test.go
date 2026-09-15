package main

import (
	"strings"
	"testing"
)

func TestClaudeSessionIDIsStableAndUniquePerAgent(t *testing.T) {
	first, err := claudeSessionID("agt_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	again, err := claudeSessionID("agt_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	other, err := claudeSessionID("agt_fedcba9876543210")
	if err != nil {
		t.Fatal(err)
	}
	if first != again || first == other {
		t.Fatalf("session IDs must be stable per identity and distinct across identities: %q %q %q", first, again, other)
	}
	if len(first) != 36 || first[14] != '4' || !strings.Contains("89ab", string(first[19])) {
		t.Fatalf("session ID is not an RFC 4122 v4 UUID: %q", first)
	}
}

func TestFreshRuntimeCommandPinsNativeClaudeConversation(t *testing.T) {
	command, err := freshRuntimeCommand("claude --model 'sonnet'", "claude", "agt_0123456789abcdef", "fresh briefing")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(command, "--session-id '") || !strings.HasSuffix(command, " 'fresh briefing'") {
		t.Fatalf("Claude command did not pin a fresh conversation: %q", command)
	}
	generic, err := freshRuntimeCommand("codex", "codex", "", "briefing")
	if err != nil || generic != "codex 'briefing'" {
		t.Fatalf("non-Claude command changed unexpectedly: %q, %v", generic, err)
	}
}

func TestRequireNativeClaudeCommand(t *testing.T) {
	if err := requireNativeClaudeCommand("claude", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := requireNativeClaudeCommand("claude --continue", "claude"); err == nil {
		t.Fatal("custom Claude command unexpectedly bypassed fresh-session enforcement")
	}
}
