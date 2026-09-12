package api

import "time"

// AllocationIntent is a durable, handler/lead-authored record of intended
// member/extra classification for one preallocated agent identity bound to
// one exact work item, revision and work-order message. It exists BEFORE
// that agent is ever admitted (created through a supported pre-admission
// authoring call, never derived from execution Start/Queue state), and is
// consumed exactly once -- atomically, in the same transaction as the
// resulting admission -- so it authorizes exactly one fresh run, not an
// open-ended declaration. A fresh (non-replacement) parented member/extra
// admission for a preallocated --agent-id must have a matching, unconsumed
// intent recorded for that exact identity/item/revision/order/team role;
// admission is rejected when no such intent exists or when the requested
// classification does not match it (independent review #2300/#2771/#2840,
// finding 5, as clarified by #2844/#2850/#2867/#2870: functional recorded-
// allocation consistency, not a cryptographic authority boundary against a
// malicious shared-token holder -- that remains outside this bug's scope).
type AllocationIntent struct {
	AgentID          string           `json:"agentId"`
	ItemTaskID       string           `json:"itemTaskId"`
	ItemID           string           `json:"itemId"`
	ItemRevision     int64            `json:"itemRevision"`
	WorkOrderMessage MessageReference `json:"workOrderMessage"`
	TeamRole         string           `json:"teamRole"`
	CreatedBy        Caller           `json:"createdBy"`
	CreatedAt        time.Time        `json:"createdAt"`
	ConsumedAt       *time.Time       `json:"consumedAt,omitempty"`
	ConsumedByRunID  string           `json:"consumedByRunId,omitempty"`
}

// CreateAllocationIntentRequest authors one intent for one not-yet-admitted
// preallocated agent identity (--agent-id). It is a one-shot record: a
// second CreateAllocationIntent for the same AgentID is a conflict, not an
// update -- an intent is never mutated after authoring, only consumed.
type CreateAllocationIntentRequest struct {
	AgentID          string           `json:"agentId"`
	ItemTaskID       string           `json:"itemTaskId"`
	ItemID           string           `json:"itemId"`
	ItemRevision     int64            `json:"itemRevision"`
	WorkOrderMessage MessageReference `json:"workOrderMessage"`
	TeamRole         string           `json:"teamRole"`
}
