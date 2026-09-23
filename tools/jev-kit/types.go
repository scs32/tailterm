package jevkit

import "time"

type Message struct {
	Seq          int64
	FromAgentID  string // "" when a human posted it
	To           string // agent name, or "" for a board-wide post
	Swarm        bool   // task was in swarm mode when the message was stamped
	IsAck        bool   // pure acknowledgement ("thanks", "on it")
}

type Agent struct {
	ID           string
	Name         string
	Status       string // "active", "retired", "closed"
	Busy         bool   // mid-turn
	Orchestrator bool
	LastWokenSeq int64
	RecentWakes  []time.Time // wake times, any order
}

type Policy struct {
	ProjectPaused         bool
	ResumeRetiredOnDirect bool
	MaxWakes              int           // per agent per Window
	Window                time.Duration
}

type Decision struct {
	Action  string    // "wake", "resume", "defer", "skip"
	Reason  string
	RetryAt time.Time // set only for rate_limited defers
}
