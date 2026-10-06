package api

// TeamCloseMember is an exact run in the item team. The client supplies the
// complete snapshot; the server compares it with current bindings before any
// closure intent is recorded.
type TeamCloseMember struct {
	AgentID string `json:"agentId"`
	RunID   string `json:"runId"`
	Host    string `json:"host"`
	Status  string `json:"status"`
}

// A team close with a reason ends a team whose item stays open: an owner hold,
// or a diagnosis order that produced findings and no commit. A close without
// a reason requires a done or dismissed item.
const (
	TeamCloseReasonOwnerHold    = "owner-hold"
	TeamCloseReasonFindingsOnly = "findings-only"
)

// ValidTeamCloseReason reports whether reason permits closing a team whose
// item is open.
func ValidTeamCloseReason(reason string) bool {
	return reason == TeamCloseReasonOwnerHold || reason == TeamCloseReasonFindingsOnly
}

type TeamCloseRequest struct {
	RequestID    string            `json:"requestId"`
	ActorAgentID string            `json:"actorAgentId,omitempty"`
	ActorRunID   string            `json:"actorRunId,omitempty"`
	LeadAgentID  string            `json:"leadAgentId"`
	LeadRunID    string            `json:"leadRunId"`
	LeadRevision int64             `json:"leadRevision"`
	ItemID       string            `json:"itemId"`
	ItemRevision int64             `json:"itemRevision"`
	Members      []TeamCloseMember `json:"members"`
	// Reason is empty for a terminal item's close, so existing requests keep
	// their payload hash.
	Reason string `json:"reason,omitempty"`
}

type TeamCloseResult struct {
	TaskID      string            `json:"taskId"`
	ItemID      string            `json:"itemId"`
	LeadAgentID string            `json:"leadAgentId"`
	Members     []TeamCloseMember `json:"members"`
	Reason      string            `json:"reason,omitempty"`
}
