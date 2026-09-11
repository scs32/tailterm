package api

import (
	"encoding/json"
	"time"
)

// TeamRoleMember marks a work-item binding as this item's allocated regular
// team member (its builder), not subject to the per-item extra allowance.
// TeamRoleExtra marks it as an extra, on top of that allocation, checked
// against the task's per-item maxNewAgents. Classification is an explicit
// caller declaration, never inferred from ParentAgentID or binding order --
// see Store.AddAgent. A replacement (ReplacesAgentID set) always inherits
// the role of the binding it replaces; any caller-supplied value for a
// replacement is validated against that inherited role, not applied fresh.
const (
	TeamRoleMember = "member"
	TeamRoleExtra  = "extra"
)

// AgentWorkItemRequest binds a newly admitted ordinary agent to one durable
// work item. ItemRevision is a launch-time CAS. WorkOrderMessage identifies the
// bounded assignment that authorized the run. ReplacesAgentID is optional and
// links a new identity/run to the preserved session it supersedes. TeamRole
// is required (TeamRoleMember or TeamRoleExtra) for a fresh (non-replacement)
// binding admitted through a parented (agent-session) launch; it is ignored
// in favor of inherited classification for a replacement, and is not
// meaningful for a parentless (browser/manual) admission, which is always
// recorded as TeamRoleMember since it never consumes extra allowance.
type AgentWorkItemRequest struct {
	ItemTaskID       string               `json:"itemTaskId"`
	ItemID           string               `json:"itemId"`
	ItemRevision     int64                `json:"itemRevision"`
	WorkOrderMessage MessageReference     `json:"workOrderMessage"`
	ReplacesAgentID  string               `json:"replacesAgentId,omitempty"`
	TeamRole         string               `json:"teamRole,omitempty"`
	QueueClaim       *QueueAdmissionClaim `json:"queueClaim,omitempty"`
	ContextBundle    json.RawMessage      `json:"contextBundle"`
}

// QueueAdmissionClaim is required only when a receiving project admits a
// worker for a source-owned item. It binds admission to the exact deliberate
// Queue claim instead of treating a dispatch or prose as launch authority.
type QueueAdmissionClaim struct {
	EntryID          string `json:"entryId"`
	Cycle            int64  `json:"cycle"`
	ExpectedRevision int64  `json:"expectedRevision"`
	ClaimantAgentID  string `json:"claimantAgentId"`
	ClaimantRunID    string `json:"claimantRunId"`
}

// AgentWorkItemBinding is immutable for an agent run. ContextThroughMessageSeq
// is also the initial inbox cursor, so prior project conversation is restored
// only through the explicitly item-scoped context below.
type AgentWorkItemBinding struct {
	AgentID                  string           `json:"agentId"`
	RunID                    string           `json:"runId"`
	ItemTaskID               string           `json:"itemTaskId"`
	ItemID                   string           `json:"itemId"`
	ItemRevision             int64            `json:"itemRevision"`
	WorkOrderMessage         MessageReference `json:"workOrderMessage"`
	ContextThroughMessageSeq int64            `json:"contextThroughMessageSeq"`
	ReplacesAgentID          string           `json:"replacesAgentId,omitempty"`
	TeamRole                 string           `json:"teamRole,omitempty"`
	ContextDigest            string           `json:"contextDigest"`
	CreatedAt                time.Time        `json:"createdAt"`
}

// AgentWorkItemContext is the complete bounded durable input for one run. It
// intentionally excludes general Board history: Messages contains only the
// source, recorded order, explicitly primary-linked messages, and typed answers
// to linked decisions.
type AgentWorkItemContext struct {
	Version int                  `json:"version"`
	Binding AgentWorkItemBinding `json:"binding"`
	Bundle  json.RawMessage      `json:"bundle"`
}
