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

// OwnerDelegationRequest records an explicit owner grant for one request.
type OwnerDelegationRequest struct {
	Session          string `json:"session"`
	AgentID          string `json:"agentId,omitempty"`
	RunID            string `json:"runId,omitempty"`
	AuthorizationRef string `json:"authorizationRef"`
	RequestID        string `json:"requestId"`
}

// ObligationAnswerRequest lets the owner or a granted session answer a request,
// question or block on the recipient's behalf.
type ObligationAnswerRequest struct {
	DelegateSession string         `json:"delegateSession,omitempty"`
	AgentID         string         `json:"agentId,omitempty"`
	RunID           string         `json:"runId,omitempty"`
	Approve         bool           `json:"approve,omitempty"`
	Source          *MessageSource `json:"source,omitempty"`
	Text            string         `json:"text"`
	// Rationale is required from an agent answering under an owner delegation
	// window (AgentID and RunID set, no DelegateSession), and only then.
	Rationale string `json:"rationale,omitempty"`
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
	DelegationID string            `json:"delegationId,omitempty"`
	Action       string            `json:"action"`
	Obligation   *Obligation       `json:"obligation,omitempty"`
	Message      *Message          `json:"message,omitempty"`
	Agent        *Agent            `json:"agent,omitempty"`
	Window       *DelegationWindow `json:"window,omitempty"`
	// Registration is the owner helper register receipt.
	Registration *OwnerHelperRegistration `json:"registration,omitempty"`
	Replay       bool                     `json:"replay,omitempty"`
}
