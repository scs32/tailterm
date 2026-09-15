package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// claudeSessionID binds a Claude conversation to one Tailterm agent identity.
// A newly allocated Tailterm identity therefore cannot select an old Claude
// conversation, while a retry of that exact identity keeps its own session.
func claudeSessionID(agentID string) (string, error) {
	if !api.ValidID(agentID, "agt") {
		return "", fmt.Errorf("fresh Claude launch requires a stable agent identity")
	}
	digest := sha256.Sum256([]byte("tailterm-claude-session-v1\x00" + agentID))
	b := append([]byte(nil), digest[:16]...)
	// Make the generated identifier an RFC 4122 version-4 UUID. Claude
	// validates the UUID shape before it creates or resumes a conversation.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexID := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexID[:8], hexID[8:12], hexID[12:16], hexID[16:20], hexID[20:]), nil
}

// freshRuntimeCommand emits the complete native Claude invocation. Claude's
// positional prompt alone does not prove whether its host selected a new or
// persisted conversation; --session-id makes that selection explicit.
func freshRuntimeCommand(base, runtime, agentID, briefing string) (string, error) {
	if runtime != "claude" {
		return base + " " + spawn.ShellQuote(briefing), nil
	}
	id, err := claudeSessionID(agentID)
	if err != nil {
		return "", err
	}
	return base + " --session-id " + spawn.ShellQuote(id) + " " + spawn.ShellQuote(briefing), nil
}

func requireNativeClaudeCommand(run, runtime string) error {
	if runtime == "claude" && strings.TrimSpace(run) != "claude" {
		return fmt.Errorf("fresh Tailterm Claude launches require the native claude command; custom overrides cannot prove a new conversation")
	}
	return nil
}
