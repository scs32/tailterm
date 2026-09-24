package api

// Broker phase 3 (docs/broker-phase-3.md): role recipients and the owner's
// remaining controls over obligations.

// Role recipients an envelope may address: "role:lead" is the project's
// orchestrator, "role:database_handler" its database handler.
const (
	RoleLead            = "lead"
	RoleDatabaseHandler = "database_handler"
)

// ObligationExtendRequest moves an open obligation's deadlines to now + For.
type ObligationExtendRequest struct {
	For       string `json:"for"` // a Go duration, 1m to 7 days
	Reason    string `json:"reason,omitempty"`
	RequestID string `json:"requestId"`
}

// ObligationAnswerRequest lets the owner answer a question, or resolve a
// block, on the recipient's behalf.
type ObligationAnswerRequest struct {
	Text      string `json:"text"`
	RequestID string `json:"requestId"`
}

// ObligationCancelRequest closes an open obligation as cancelled.
type ObligationCancelRequest struct {
	Reason    string `json:"reason"`
	RequestID string `json:"requestId"`
}

// AgentResumeRequest re-enables inbox wake-ups for a retired agent.
type AgentResumeRequest struct {
	RequestID string `json:"requestId"`
}

// OwnerActionResult is what an owner action changed. A retry with the same
// request ID returns the original result.
type OwnerActionResult struct {
	Action     string      `json:"action"`
	Obligation *Obligation `json:"obligation,omitempty"`
	Message    *Message    `json:"message,omitempty"`
	Agent      *Agent      `json:"agent,omitempty"`
	Replay     bool        `json:"replay,omitempty"`
}
