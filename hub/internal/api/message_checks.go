package api

import "time"

// Message check forms (docs/broker-phase-1.md). Phase 1 records them; nothing
// is rejected because of a form.
const (
	CheckFormTyped                 = "typed"
	CheckFormTextConvention        = "text_convention"
	CheckFormTextConventionInvalid = "text_convention_invalid"
	CheckFormFreeText              = "free_text"
	CheckFormHuman                 = "human"

	JevStatusPending     = "pending"
	JevStatusScored      = "scored"
	JevStatusUnavailable = "unavailable"
	JevStatusDisabled    = "disabled"
	JevStatusSkipped     = "skipped"
)

// MessageCheck is the shadow-mode verdict recorded for one board message.
type MessageCheck struct {
	Seq       int64      `json:"seq"`
	TaskID    string     `json:"taskId"`
	AgentID   string     `json:"agentId,omitempty"`
	Form      string     `json:"form"`
	Problems  []Problem  `json:"problems,omitempty"`
	JevStatus string     `json:"jevStatus"`
	Jev       *JevScores `json:"jev,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	ScoredAt  *time.Time `json:"scoredAt,omitempty"`
}

// JevScores holds one Jev evaluation: noul name to probability.
type JevScores struct {
	Model     string             `json:"model,omitempty"`
	LatencyMS int64              `json:"latencyMs,omitempty"`
	Nouls     map[string]float64 `json:"nouls,omitempty"`
	Error     string             `json:"error,omitempty"`
}

type MessageCheckList struct {
	Checks []MessageCheck `json:"checks"`
}
