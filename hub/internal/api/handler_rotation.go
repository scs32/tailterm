package api

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"time"
)

// Handler rotation (docs/handler-rotation.md) replaces a project's primary
// database handler with a fresh session that has the current prompt. A
// rotation is a two-phase, keyed record: prepare checks the old handler is
// idle and stops new leases to it; commit re-checks, moves every open
// obligation to the successor, snapshots the handoff and makes it primary.
const (
	HandlerRotationPrepared  = "prepared"
	HandlerRotationCommitted = "committed"
	HandlerRotationAborted   = "aborted"

	HandlerRotationPrepare = "prepare"
	HandlerRotationCommit  = "commit"
	HandlerRotationAbort   = "abort"

	HandlerRotationReasonManual   = "manual"
	HandlerRotationReasonItems    = "items"
	HandlerRotationReasonTokens   = "tokens"
	HandlerRotationReasonTemplate = "template"

	// The owner command may treat an unknown activity as idle; the runner
	// requires an observed idle or finished_silent state.
	HandlerRotationTriggerOwner  = "owner"
	HandlerRotationTriggerRunner = "runner"

	HandlerRotationRefusedLiveLease    = "live_lease"
	HandlerRotationRefusedWorking      = "working"
	HandlerRotationRefusedPendingTool  = "pending_tool"
	HandlerRotationRefusedOpen         = "rotation_open"
	HandlerRotationRefusedPaused       = "project_paused"
	HandlerRotationRefusedNotPrimary   = "not_primary"
	HandlerRotationRefusedSuccessor    = "successor_unavailable"
	HandlerRotationRefusedNameTaken    = "name_taken"
	HandlerRotationRefusedAgentCaller  = "agent_caller"
	HandlerRotationRefusedStalePolicy  = "stale_revision"
	HandlerRotationRefusedStaleHandler = "handler_changed"

	// Owner decision #14233, option A.
	DefaultHandlerRotationMaxItems       int64 = 10
	DefaultHandlerRotationMaxTotalTokens int64 = 300_000_000
)

var rotatedNameSuffix = regexp.MustCompile(`-r[0-9]+$`)

// HandlerSuccessorName names the successor of a handler at handler revision
// rev: the base name without any earlier -rN suffix, plus -r<rev+1>.
func HandlerSuccessorName(oldName string, rev int64) string {
	return rotatedNameSuffix.ReplaceAllString(oldName, "") + fmt.Sprintf("-r%d", rev+1)
}

// HandlerRotationRefusal is a 409 with a named reason. A refused prepare or
// commit changes nothing.
type HandlerRotationRefusal struct {
	Code   string
	Detail string
}

func (e *HandlerRotationRefusal) Error() string {
	return fmt.Sprintf("handler rotation refused (%s): %s", e.Code, e.Detail)
}

func (e *HandlerRotationRefusal) Unwrap() error { return ErrConflict }

// HandlerRotationPolicy says when the host runner rotates a project's
// primary handler. Revision 0 means the owner has not saved one: new projects
// use the owner-decided defaults with rotation enabled.
type HandlerRotationPolicy struct {
	TaskID           string     `json:"taskId"`
	Enabled          bool       `json:"enabled"`
	MaxItems         int64      `json:"maxItems"`
	MaxTotalTokens   int64      `json:"maxTotalTokens"`
	OnTemplateChange bool       `json:"onTemplateChange"`
	Revision         int64      `json:"revision"`
	UpdatedAt        *time.Time `json:"updatedAt,omitempty"`
}

// A zero MaxItems or MaxTotalTokens turns that limit off.
type HandlerRotationPolicyRequest struct {
	ExpectedRevision int64  `json:"expectedRevision"`
	Enabled          bool   `json:"enabled"`
	MaxItems         int64  `json:"maxItems"`
	MaxTotalTokens   int64  `json:"maxTotalTokens"`
	OnTemplateChange bool   `json:"onTemplateChange"`
	ActorAgentID     string `json:"actorAgentId,omitempty"`
}

type HandlerRotationRequest struct {
	Operation string `json:"operation"`
	RequestID string `json:"requestId"`
	// prepare
	ExpectedHandlerRevision int64  `json:"expectedHandlerRevision,omitempty"`
	OldAgentID              string `json:"oldAgentId,omitempty"`
	OldRunID                string `json:"oldRunId,omitempty"`
	SuccessorAgentID        string `json:"successorAgentId,omitempty"`
	SuccessorName           string `json:"successorName,omitempty"`
	Reason                  string `json:"reason,omitempty"`
	Trigger                 string `json:"trigger,omitempty"`
	// commit and abort
	RotationID   string `json:"rotationId,omitempty"`
	ActorAgentID string `json:"actorAgentId,omitempty"`
}

type HandlerRotation struct {
	ID               string                  `json:"id"`
	TaskID           string                  `json:"taskId"`
	RequestID        string                  `json:"requestId"`
	State            string                  `json:"state"`
	Reason           string                  `json:"reason"`
	Trigger          string                  `json:"trigger"`
	HandlerRevision  int64                   `json:"handlerRevision"`
	OldAgentID       string                  `json:"oldAgentId"`
	OldRunID         string                  `json:"oldRunId"`
	OldName          string                  `json:"oldName"`
	SuccessorAgentID string                  `json:"successorAgentId"`
	SuccessorName    string                  `json:"successorName"`
	SuccessorRunID   string                  `json:"successorRunId,omitempty"`
	Handoff          *HandlerRotationHandoff `json:"handoff,omitempty"`
	Receipt          *HandlerRotationReceipt `json:"receipt,omitempty"`
	CreatedAt        time.Time               `json:"createdAt"`
	UpdatedAt        time.Time               `json:"updatedAt"`
}

// HandlerRotationHandoff is the durable snapshot the successor reads with
// tt handler rotation get. Only Reissued moved; the other lists are what the
// successor inherits as context.
type HandlerRotationHandoff struct {
	Reissued                  []HandlerRotationReissue    `json:"reissued"`
	PendingScopeConfirmations []HandlerRotationScope      `json:"pendingScopeConfirmations"`
	LiveLeases                []HandlerRotationLease      `json:"liveLeases"`
	AuthoredOpen              []HandlerRotationAuthored   `json:"authoredOpen"`
	AllocationIntents         []HandlerRotationIntent     `json:"allocationIntents"`
	QueueClaims               []HandlerRotationQueueClaim `json:"queueClaims"`
	RequiredDeliveries        []HandlerRotationDelivery   `json:"requiredDeliveries"`
}

type HandlerRotationReissue struct {
	OldObligationID string `json:"oldObligationId"`
	OldMessageSeq   int64  `json:"oldMessageSeq"`
	OldState        string `json:"oldState"`
	NewObligationID string `json:"newObligationId"`
	NewMessageSeq   int64  `json:"newMessageSeq"`
	ViaRole         string `json:"viaRole,omitempty"`
	Subject         string `json:"subject"`
}

type HandlerRotationScope struct {
	EntryID      string `json:"entryId"`
	ItemID       string `json:"itemId"`
	ItemRevision int64  `json:"itemRevision"`
	OrderSeq     int64  `json:"orderSeq"`
}

type HandlerRotationLease struct {
	EntryID         string `json:"entryId"`
	ItemID          string `json:"itemId"`
	State           string `json:"state"`
	LeaseGeneration int64  `json:"leaseGeneration"`
}

type HandlerRotationAuthored struct {
	ObligationID string `json:"obligationId"`
	MessageSeq   int64  `json:"messageSeq"`
	AgentID      string `json:"agentId"`
	State        string `json:"state"`
	Subject      string `json:"subject"`
}

type HandlerRotationIntent struct {
	AgentID      string `json:"agentId"`
	ItemTaskID   string `json:"itemTaskId"`
	ItemID       string `json:"itemId"`
	ItemRevision int64  `json:"itemRevision"`
	TeamRole     string `json:"teamRole"`
}

type HandlerRotationQueueClaim struct {
	EntryID      string `json:"entryId"`
	SourceTaskID string `json:"sourceTaskId"`
	ItemID       string `json:"itemId"`
	Cycle        int64  `json:"cycle"`
	State        string `json:"state"`
}

type HandlerRotationDelivery struct {
	ID         string `json:"id"`
	MessageSeq int64  `json:"messageSeq"`
	Kind       string `json:"kind"`
	ItemID     string `json:"itemId"`
	Phase      string `json:"phase"`
}

type HandlerRotationReceipt struct {
	RotationID       string    `json:"rotationId"`
	RequestID        string    `json:"requestId"`
	HandlerRevision  int64     `json:"handlerRevision"`
	PrimaryHandlerID string    `json:"primaryHandlerId"`
	ClosedAgentID    string    `json:"closedAgentId"`
	ClosedRunID      string    `json:"closedRunId"`
	NoticeSeq        int64     `json:"noticeSeq"`
	Reissued         int       `json:"reissued"`
	CommittedAt      time.Time `json:"committedAt"`
}

// HandlerRotationDue is one project's primary handler on a host, with the
// counters the runner compares against the project's enabled policy.
type HandlerRotationDue struct {
	TaskID          string                `json:"taskId"`
	HandlerRevision int64                 `json:"handlerRevision"`
	Agent           Agent                 `json:"agent"`
	Policy          HandlerRotationPolicy `json:"policy"`
	FinishedItems   int64                 `json:"finishedItems"`
	TotalTokens     int64                 `json:"totalTokens"`
	RecordedDigest  string                `json:"recordedDigest"`
	DigestMatches   bool                  `json:"digestMatches"`
	ActivityState   string                `json:"activityState"`
	PendingTool     string                `json:"pendingTool,omitempty"`
	LiveLeases      int                   `json:"liveLeases"`
	Idle            bool                  `json:"idle"`
	DueReasons      []string              `json:"dueReasons"`
	OpenRotation    *HandlerRotation      `json:"openRotation,omitempty"`
}

type HandlerRotationDueList struct {
	Entries []HandlerRotationDue `json:"entries"`
}

func (c *Client) HandlerRotationPolicy(ctx context.Context, task string) (HandlerRotationPolicy, error) {
	var out HandlerRotationPolicy
	return out, c.do(ctx, "GET", "/v1/tasks/"+task+"/handler-rotation/policy", nil, &out)
}

func (c *Client) SetHandlerRotationPolicy(ctx context.Context, task string, req HandlerRotationPolicyRequest) (HandlerRotationPolicy, error) {
	var out HandlerRotationPolicy
	return out, c.do(ctx, "PUT", "/v1/tasks/"+task+"/handler-rotation/policy", req, &out)
}

func (c *Client) HandlerRotationAction(ctx context.Context, task string, req HandlerRotationRequest) (HandlerRotation, error) {
	var out HandlerRotation
	return out, c.do(ctx, "POST", "/v1/tasks/"+task+"/handler-rotations", req, &out)
}

func (c *Client) GetHandlerRotation(ctx context.Context, task, id string) (HandlerRotation, error) {
	var out HandlerRotation
	return out, c.do(ctx, "GET", "/v1/tasks/"+task+"/handler-rotations/"+url.PathEscape(id), nil, &out)
}

func (c *Client) ListHandlerRotations(ctx context.Context, task string) ([]HandlerRotation, error) {
	var out struct {
		Rotations []HandlerRotation `json:"rotations"`
	}
	err := c.do(ctx, "GET", "/v1/tasks/"+task+"/handler-rotations", nil, &out)
	return out.Rotations, err
}

func (c *Client) HandlerRotationsDue(ctx context.Context, host, templateDigest string) (HandlerRotationDueList, error) {
	var out HandlerRotationDueList
	q := url.Values{"host": {host}, "templateDigest": {templateDigest}}
	return out, c.do(ctx, "GET", "/v1/handler-rotations/due?"+q.Encode(), nil, &out)
}
