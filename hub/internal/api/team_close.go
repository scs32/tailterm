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
}

type TeamCloseResult struct {
	TaskID      string            `json:"taskId"`
	ItemID      string            `json:"itemId"`
	LeadAgentID string            `json:"leadAgentId"`
	Members     []TeamCloseMember `json:"members"`
}
