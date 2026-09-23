package jevkit

import "time"
func ShouldWakeA(m Message, a Agent, p Policy, now time.Time) Decision {
	fromHuman := m.FromAgentID == ""
	if !fromHuman && m.FromAgentID == a.ID {
		return Decision{Action: "skip", Reason: "self"}
	}
	if a.Status == "closed" {
		return Decision{Action: "skip", Reason: "closed"}
	}
	if a.LastWokenSeq >= m.Seq {
		return Decision{Action: "skip", Reason: "duplicate"}
	}
	direct := m.To != "" && m.To == a.Name
	switch {
	case m.Swarm:
	case m.To != "" && !direct:
		return Decision{Action: "skip", Reason: "not_recipient"}
	case m.To == "" && !a.Orchestrator:
		return Decision{Action: "skip", Reason: "not_recipient"}
	}
	if m.IsAck && !fromHuman {
		return Decision{Action: "skip", Reason: "ack"}
	}
	if p.ProjectPaused && !fromHuman {
		return Decision{Action: "defer", Reason: "paused"}
	}
	if a.Status == "retired" {
		if direct && p.ResumeRetiredOnDirect {
			return Decision{Action: "resume", Reason: "direct_to_retired"}
		}
		return Decision{Action: "skip", Reason: "retired"}
	}
	if a.Busy {
		return Decision{Action: "defer", Reason: "busy"}
	}
	if !fromHuman && p.MaxWakes > 0 {
		var oldest time.Time
		n := 0
		for _, t := range a.RecentWakes {
			if now.Sub(t) < p.Window {
				n++
				if oldest.IsZero() || t.Before(oldest) {
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
