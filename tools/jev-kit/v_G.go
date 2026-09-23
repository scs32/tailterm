package jevkit

import "time"

// ShouldWake decides whether the relay should wake agent a for message m.
// Rules are applied in precedence order R1..R9; the first that matches wins.
func ShouldWakeG(m Message, a Agent, p Policy, now time.Time) Decision {
	human := m.FromAgentID == ""

	// R1 never wake the sender for its own message.
	if !human && m.FromAgentID == a.ID {
		return Decision{Action: "skip", Reason: "self"}
	}
	// R2 closed agents are never woken.
	if a.Status == "closed" {
		return Decision{Action: "skip", Reason: "closed"}
	}
	// R3 dedupe: this message (or a later one) already woke the agent.
	if a.LastWokenSeq >= m.Seq {
		return Decision{Action: "skip", Reason: "duplicate"}
	}
	// R4 scope: swarm reaches everyone; a directed post reaches only its
	// recipient; a board-wide non-swarm post reaches only the orchestrator.
	direct := m.To != "" && m.To == a.Name
	switch {
	case m.Swarm:
	case m.To != "" && !direct:
		return Decision{Action: "skip", Reason: "not_recipient"}
	case m.To == "" && !a.Orchestrator:
		return Decision{Action: "skip", Reason: "not_recipient"}
	}
	// R5 agent acknowledgements never wake anyone; human messages always may.
	if m.IsAck && !human {
		return Decision{Action: "skip", Reason: "ack"}
	}
	// R6 a paused project defers agent traffic but not human messages.
	if p.ProjectPaused && !human {
		return Decision{Action: "defer", Reason: "paused"}
	}
	// R7 retired agents resume only for a direct message when policy allows.
	if a.Status == "retired" {
		if direct && p.ResumeRetiredOnDirect {
			return Decision{Action: "resume", Reason: "direct_to_retired"}
		}
		return Decision{Action: "skip", Reason: "retired"}
	}
	// R8 a busy agent picks the message up at its next checkpoint.
	if a.Busy {
		return Decision{Action: "defer", Reason: "busy"}
	}
	// R9 rate limit: at most MaxWakes in the trailing Window. Humans bypass.
	if !human && p.MaxWakes > 0 {
		var oldest time.Time
		n := 0
		for _, t := range a.RecentWakes {
			if now.Sub(t) < p.Window {
				n++
				if oldest.IsZero() || t.After(oldest) {
					oldest = t
				}
			}
		}
		if n >= p.MaxWakes {
			return Decision{Action: "defer", Reason: "rate_limited", RetryAt: oldest.Add(p.Window)}
		}
	}
	return Decision{Action: "wake", Reason: "eligible"}
}
