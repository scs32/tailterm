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
	// The host runner found the busy primary's process and session gone on its
	// host, and the hub has recorded nothing from that run for the policy's
	// silence (owner order #28057). Runner trigger only, with death evidence.
	HandlerRotationReasonDeadPrimary = "dead_primary"

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
	// A dead-primary rotation only: the evidence does not confirm the death,
	// or the run has not been silent for the policy's minutes.
	HandlerRotationRefusedDeathUnconfirmed = "death_unconfirmed"
	HandlerRotationRefusedNotSilent        = "not_silent"

	// Owner decision #14233, option A.
	DefaultHandlerRotationMaxItems       int64 = 10
	DefaultHandlerRotationMaxTotalTokens int64 = 300_000_000

	// Owner order #28057: a dead, busy primary is replaced after this many
	// minutes with no recorded activity. 0 turns the replacement off.
	DefaultHandlerDeadSilenceMinutes int64 = 10
	MaxHandlerDeadSilenceMinutes     int64 = 1440
	// HandlerDeathEvidenceMaxAge bounds how old a host's observation may be
	// when the hub reads it; HandlerDeathEvidenceMaxAhead bounds clock skew.
	HandlerDeathEvidenceMaxAge   = 2 * time.Minute
	HandlerDeathEvidenceMaxAhead = 30 * time.Second
	// HandlerOnlineWindow is how recent a heartbeat makes a run online.
	HandlerOnlineWindow = 90 * time.Second

	HandlerDeathStateGone = "gone"
)

// HandlerDeathEvidence is what the handler's own host observed: the named
// tmux session is absent from a readable listing, and the runtime process is
// absent by PID and start identity, as is the pane process when PanePID is set. Only "gone" is evidence; a
// host that could not read a fact sends nothing.
type HandlerDeathEvidence struct {
	Host           string    `json:"host"`
	AgentID        string    `json:"agentId"`
	RunID          string    `json:"runId"`
	ObservedAt     time.Time `json:"observedAt"`
	SessionName    string    `json:"sessionName"`
	SessionID      string    `json:"sessionId,omitempty"`
	SessionCreated string    `json:"sessionCreated,omitempty"`
	SessionState   string    `json:"sessionState"`
	PID            int       `json:"pid"`
	PanePID        int       `json:"panePid,omitempty"`
	ProcessStarted string    `json:"processStarted"`
	ProcessState   string    `json:"processState"`
	// ExitedAt is the wrapper's own exit record, when it wrote one.
	ExitedAt *time.Time `json:"exitedAt,omitempty"`
}

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
	TaskID           string `json:"taskId"`
	Enabled          bool   `json:"enabled"`
	MaxItems         int64  `json:"maxItems"`
	MaxTotalTokens   int64  `json:"maxTotalTokens"`
	OnTemplateChange bool   `json:"onTemplateChange"`
	Revision         int64  `json:"revision"`
	// DeadSilenceMinutes is how long a dead, busy primary must have been
	// silent before the runner replaces it; 0 is off. It does not depend on
	// Enabled, which governs the item, token and template limits.
	DeadSilenceMinutes int64      `json:"deadSilenceMinutes"`
	UpdatedAt          *time.Time `json:"updatedAt,omitempty"`
}

// A zero MaxItems or MaxTotalTokens turns that limit off.
type HandlerRotationPolicyRequest struct {
	ExpectedRevision int64 `json:"expectedRevision"`
	Enabled          bool  `json:"enabled"`
	MaxItems         int64 `json:"maxItems"`
	MaxTotalTokens   int64 `json:"maxTotalTokens"`
	OnTemplateChange bool  `json:"onTemplateChange"`
	// DeadSilenceMinutes: omitted keeps the saved value, 0 turns the dead
	// primary replacement off, 1 to 1440 sets it.
	DeadSilenceMinutes *int64 `json:"deadSilenceMinutes,omitempty"`
	ActorAgentID       string `json:"actorAgentId,omitempty"`
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
	// An owner-authorized change of the primary's runtime, model or arm: who
	// authorized it and why. Both or neither; owner trigger only.
	AuthorizedBy        string `json:"authorizedBy,omitempty"`
	AuthorizationReason string `json:"authorizationReason,omitempty"`
	// DeathEvidence is required by a dead_primary prepare and by its commit,
	// which needs a second, fresh observation; refused with any other reason.
	DeathEvidence *HandlerDeathEvidence `json:"deathEvidence,omitempty"`
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
	// Authorization is set only for an owner-authorized change.
	Authorization *HandlerRotationAuthorization `json:"authorization,omitempty"`
	// A dead_primary rotation only: the host's evidence at prepare and at
	// commit, and the old run's last recorded activity at prepare.
	DeathEvidence       *HandlerDeathEvidence `json:"deathEvidence,omitempty"`
	CommitDeathEvidence *HandlerDeathEvidence `json:"commitDeathEvidence,omitempty"`
	LastActivityAt      *time.Time            `json:"lastActivityAt,omitempty"`
	CreatedAt           time.Time             `json:"createdAt"`
	UpdatedAt           time.Time             `json:"updatedAt"`
}

// HandlerRotationAuthorization records who authorized a rotation that may
// change the primary's runtime, model or arm, and why. The runtimes are saved
// at commit.
type HandlerRotationAuthorization struct {
	AuthorizedBy     string `json:"authorizedBy"`
	Reason           string `json:"reason"`
	OldRuntime       string `json:"oldRuntime,omitempty"`
	SuccessorRuntime string `json:"successorRuntime,omitempty"`
}

// HandlerRotationHandoff is the durable snapshot the successor reads with
// tt handler rotation get. Reissued moved, and so did LiveLeases in a
// dead_primary rotation (any other rotation has none); the other lists are
// what the successor inherits as context.
type HandlerRotationHandoff struct {
	Reissued                  []HandlerRotationReissue    `json:"reissued"`
	PendingScopeConfirmations []HandlerRotationScope      `json:"pendingScopeConfirmations"`
	LiveLeases                []HandlerRotationLease      `json:"liveLeases"`
	AuthoredOpen              []HandlerRotationAuthored   `json:"authoredOpen"`
	AllocationIntents         []HandlerRotationIntent     `json:"allocationIntents"`
	QueueClaims               []HandlerRotationQueueClaim `json:"queueClaims"`
	RequiredDeliveries        []HandlerRotationDelivery   `json:"requiredDeliveries"`
	// InFlight is set by a dead_primary rotation: what the dead run was doing,
	// reported because nothing can be recovered from a dead process.
	InFlight *HandlerRotationInFlight `json:"inFlight,omitempty"`
}

// HandlerRotationInFlight reports a dead run's last state and its recent
// saves, so the successor can compare a moved request with them before
// repeating a save.
type HandlerRotationInFlight struct {
	ActivityState  string                 `json:"activityState"`
	PendingTool    string                 `json:"pendingTool,omitempty"`
	LastActivityAt time.Time              `json:"lastActivityAt"`
	SilenceMinutes int64                  `json:"silenceMinutes"`
	WritesSince    time.Time              `json:"writesSince"`
	RecentWrites   []HandlerRotationWrite `json:"recentWrites"`
	// WritesOmitted counts older saves in the window beyond the 50 listed.
	WritesOmitted int `json:"writesOmitted"`
}

type HandlerRotationWrite struct {
	ItemID        string    `json:"itemId"`
	Revision      int64     `json:"revision"`
	UpdatedAt     time.Time `json:"updatedAt"`
	ChangedFields []string  `json:"changedFields"`
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
	// Set when a dead_primary rotation moved the lease to the successor.
	NewLeaseGeneration  int64 `json:"newLeaseGeneration,omitempty"`
	LaunchPlanRewritten bool  `json:"launchPlanRewritten,omitempty"`
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
	RotationID       string `json:"rotationId"`
	RequestID        string `json:"requestId"`
	HandlerRevision  int64  `json:"handlerRevision"`
	PrimaryHandlerID string `json:"primaryHandlerId"`
	ClosedAgentID    string `json:"closedAgentId"`
	ClosedRunID      string `json:"closedRunId"`
	NoticeSeq        int64  `json:"noticeSeq"`
	Reissued         int    `json:"reissued"`
	// A dead_primary rotation only: leases moved, and the owner helper's copy
	// of the notice.
	LeasesMoved    int       `json:"leasesMoved,omitempty"`
	OwnerNoticeSeq int64     `json:"ownerNoticeSeq,omitempty"`
	CommittedAt    time.Time `json:"committedAt"`
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
	// Online is the hub's own view by its clock: a heartbeat within
	// HandlerOnlineWindow. DeadCandidate: not online, busy for the runner, and
	// the dead primary replacement is on. SilenceMet: no recorded activity for
	// the policy's minutes. The runner probes its host only when both hold.
	Online         bool       `json:"online"`
	LastActivityAt *time.Time `json:"lastActivityAt,omitempty"`
	DeadCandidate  bool       `json:"deadCandidate"`
	SilenceMet     bool       `json:"silenceMet"`
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
