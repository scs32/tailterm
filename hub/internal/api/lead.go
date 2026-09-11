package api

// AssignLeadRequest binds a deliberate human choice to the displayed project
// revision and both exact runs. RequestID makes uncertain responses recoverable.
type AssignLeadRequest struct {
	RequestID        string `json:"requestId"`
	ExpectedRevision int64  `json:"expectedRevision"`
	ExpectedName     string `json:"expectedName"`
	PreviousAgentID  string `json:"previousAgentId"`
	PreviousRunID    string `json:"previousRunId"`
	AgentID          string `json:"agentId"`
	RunID            string `json:"runId"`
}
type LeadAssignment struct {
	Task       Task   `json:"task"`
	AgentID    string `json:"agentId"`
	RunID      string `json:"runId"`
	MessageSeq int64  `json:"messageSeq"`
	RequestID  string `json:"requestId"`
}
