package api

import (
	"context"
	"net/url"
	"time"
)

const (
	ReliableDeliveryCapabilityVersion        = 1
	ReliableFollowThroughCapabilityVersion   = 2
	ReliableMandatoryActionCapabilityVersion = 3
)

const (
	DeliveryAssignment = "assignment"
	DeliveryAmendment  = "amendment"
	DeliveryReview     = "review"

	DeliveryUnacknowledged = "unacknowledged"
	DeliveryAcknowledged   = "acknowledged"
	DeliveryProgressing    = "progressing"
	DeliveryBlocked        = "blocked"
	DeliveryResult         = "result"
	DeliverySuperseded     = "superseded"

	DeliveryObservationUnknown    = "unknown"
	DeliveryObservationActiveTool = "active_tool"
	DeliveryObservationIdlePrompt = "idle_prompt"

	DeliveryFollowThroughAwaitingAck            = "awaiting_ack"
	DeliveryFollowThroughAwaitingProgress       = "awaiting_progress"
	DeliveryFollowThroughRecentProgress         = "recent_progress"
	DeliveryFollowThroughActiveTool             = "active_tool"
	DeliveryFollowThroughKnownBlock             = "known_block"
	DeliveryFollowThroughResolvedAwaitingResume = "resolved_awaiting_resume"
	DeliveryFollowThroughTransportUnconfirmed   = "transport_unconfirmed"
	DeliveryFollowThroughOverdueUnknown         = "overdue_unknown"
	DeliveryFollowThroughOverdueIdlePrompt      = "overdue_idle_prompt"
	DeliveryFollowThroughDispatchAmbiguous      = "dispatch_ambiguous"
	DeliveryFollowThroughEscalated              = "escalated"
	DeliveryFollowThroughIdleNoObligation       = "idle_no_obligation"
	DeliveryFollowThroughRetired                = "retired"
	DeliveryFollowThroughClosed                 = "closed"
	DeliveryFollowThroughUncovered              = "uncovered_unverified"
	DeliveryFollowThroughLegacyEnrollment       = "legacy_enrollment_unverified"
	DeliveryFollowThroughCauseRequired          = "cause_required"

	DeliveryFollowThroughActionNone  = "none"
	DeliveryFollowThroughActionQueue = "queue_current_assignment"

	DeliveryFollowThroughOutcomeAccepted    = "accepted"
	DeliveryFollowThroughOutcomeFailed      = "failed"
	DeliveryFollowThroughOutcomeAmbiguous   = "ambiguous"
	DeliveryFollowThroughOutcomeInvalidated = "invalidated"
)

const (
	DeliveryRecipientItemWorker      = "item_worker"
	DeliveryRecipientProjectLead     = "project_lead"
	DeliveryRecipientDatabaseHandler = "database_handler"

	DeliveryActionExecution           = "execution"
	DeliveryActionIndependentDispatch = "independent_dispatch"
	DeliveryActionDatabaseOperation   = "database_operation"

	DeliveryCauseUnknown     = "unknown"
	DeliveryCauseEstablished = "established"
)

type DeliveryFollowThroughPolicy struct {
	AckDeadlineSeconds          int64 `json:"ackDeadlineSeconds"`
	ProgressDeadlineSeconds     int64 `json:"progressDeadlineSeconds"`
	ResumeDeadlineSeconds       int64 `json:"resumeDeadlineSeconds"`
	ConfirmationDeadlineSeconds int64 `json:"confirmationDeadlineSeconds"`
	DispatchReportSeconds       int64 `json:"dispatchReportSeconds"`
	ActiveToolHardLimitSeconds  int64 `json:"activeToolHardLimitSeconds"`
	MaxQueueAttempts            int   `json:"maxQueueAttempts"`
}

type DeliveryFollowThroughState struct {
	Policy                    DeliveryFollowThroughPolicy `json:"policy"`
	LastSubstantiveProgressAt *time.Time                  `json:"lastSubstantiveProgressAt,omitempty"`
	NextDeadlineAt            *time.Time                  `json:"nextDeadlineAt,omitempty"`
	HardDeadlineAt            *time.Time                  `json:"hardDeadlineAt,omitempty"`
	AttemptCount              int                         `json:"attemptCount"`
	PendingAction             string                      `json:"pendingAction,omitempty"`
	PendingRequestID          string                      `json:"pendingRequestId,omitempty"`
	PendingSince              *time.Time                  `json:"pendingSince,omitempty"`
	TransportOutcome          string                      `json:"transportOutcome,omitempty"`
	ConfirmationUntil         *time.Time                  `json:"confirmationUntil,omitempty"`
	EscalationMessageSeq      int64                       `json:"escalationMessageSeq,omitempty"`
}

type RequiredDelivery struct {
	ID                     string                     `json:"id"`
	TaskID                 string                     `json:"taskId"`
	MessageSeq             int64                      `json:"messageSeq"`
	Kind                   string                     `json:"kind"`
	RecipientKind          string                     `json:"recipientKind"`
	ActionKey              string                     `json:"actionKey"`
	ActionClass            string                     `json:"actionClass"`
	AgentID                string                     `json:"agentId"`
	RunID                  string                     `json:"runId"`
	ItemTaskID             string                     `json:"itemTaskId"`
	ItemID                 string                     `json:"itemId"`
	ItemRevision           int64                      `json:"itemRevision"`
	WorkOrderMessage       MessageReference           `json:"workOrderMessage"`
	GoverningOrderMessage  MessageReference           `json:"governingOrderMessage"`
	InstructionSHA256      string                     `json:"instructionSha256"`
	InstructionBytes       int64                      `json:"instructionBytes"`
	EnrollmentVersion      int                        `json:"enrollmentVersion"`
	ContextDigest          string                     `json:"contextDigest"`
	Generation             int64                      `json:"generation"`
	SupersedesID           string                     `json:"supersedesDeliveryId,omitempty"`
	Current                bool                       `json:"current"`
	Phase                  string                     `json:"phase"`
	ExecutionEpoch         int64                      `json:"executionEpoch"`
	CurrentBlockID         string                     `json:"currentBlockId,omitempty"`
	CurrentBlock           *DeliveryBlock             `json:"currentBlock,omitempty"`
	LatestRecoveryIncident *DeliveryRecoveryIncident  `json:"latestRecoveryIncident,omitempty"`
	ResultText             string                     `json:"resultText,omitempty"`
	Message                *Message                   `json:"message,omitempty"`
	Events                 []DeliveryEvent            `json:"events,omitempty"`
	FollowThrough          DeliveryFollowThroughState `json:"followThrough"`
	CreatedAt              time.Time                  `json:"createdAt"`
	UpdatedAt              time.Time                  `json:"updatedAt"`
}

type DeliveryEvent struct {
	ID         string         `json:"id"`
	TaskID     string         `json:"taskId"`
	DeliveryID string         `json:"deliveryId"`
	Sequence   int64          `json:"sequence"`
	Kind       string         `json:"kind"`
	AgentID    string         `json:"agentId,omitempty"`
	RunID      string         `json:"runId,omitempty"`
	RequestID  string         `json:"requestId"`
	Data       map[string]any `json:"data,omitempty"`
	By         Caller         `json:"by"`
	CreatedAt  time.Time      `json:"createdAt"`
}

type DeliveryBlock struct {
	ID             string     `json:"id"`
	DeliveryID     string     `json:"deliveryId"`
	ExecutionEpoch int64      `json:"executionEpoch"`
	ReasonClass    string     `json:"reasonClass"`
	Text           string     `json:"text"`
	Resolved       bool       `json:"resolved"`
	ResolutionID   string     `json:"resolutionId,omitempty"`
	ResolutionText string     `json:"resolutionText,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	ResolvedAt     *time.Time `json:"resolvedAt,omitempty"`
}

type DeliveryPreventionAction struct {
	OwnerAgentID          string           `json:"ownerAgentId"`
	OwnerRunID            string           `json:"ownerRunId"`
	WorkOrderMessage      MessageReference `json:"workOrderMessage"`
	VerificationCriterion string           `json:"verificationCriterion"`
}

type DeliveryRecoveryIncident struct {
	ID                     string                   `json:"id"`
	DeliveryID             string                   `json:"deliveryId"`
	Generation             int64                    `json:"generation"`
	ExecutionEpoch         int64                    `json:"executionEpoch"`
	CauseStatus            string                   `json:"causeStatus"`
	LastSubstantiveAction  string                   `json:"lastSubstantiveAction"`
	LastSubstantiveAt      time.Time                `json:"lastSubstantiveAt"`
	ExpectedNextAction     string                   `json:"expectedNextAction"`
	StopReason             string                   `json:"stopReason"`
	CausalEvidence         []string                 `json:"causalEvidence"`
	ContributingConditions []string                 `json:"contributingConditions"`
	UnresolvedQuestions    []string                 `json:"unresolvedQuestions"`
	Prevention             DeliveryPreventionAction `json:"prevention"`
	PriorIncidentID        string                   `json:"priorIncidentId,omitempty"`
	PriorControlFailure    string                   `json:"priorControlFailure,omitempty"`
	RecordedByAgentID      string                   `json:"recordedByAgentId"`
	RecordedByRunID        string                   `json:"recordedByRunId"`
	CreatedAt              time.Time                `json:"createdAt"`
}

type DeliveryRecoveryIncidentRequest struct {
	RequestID              string                   `json:"requestId"`
	AgentID                string                   `json:"agentId"`
	RunID                  string                   `json:"runId"`
	ExpectedGeneration     int64                    `json:"expectedGeneration"`
	ExpectedEpoch          int64                    `json:"expectedEpoch"`
	CauseStatus            string                   `json:"causeStatus"`
	LastSubstantiveAction  string                   `json:"lastSubstantiveAction"`
	LastSubstantiveAt      time.Time                `json:"lastSubstantiveAt"`
	ExpectedNextAction     string                   `json:"expectedNextAction"`
	StopReason             string                   `json:"stopReason"`
	CausalEvidence         []string                 `json:"causalEvidence"`
	ContributingConditions []string                 `json:"contributingConditions"`
	UnresolvedQuestions    []string                 `json:"unresolvedQuestions"`
	Prevention             DeliveryPreventionAction `json:"prevention"`
	PriorIncidentID        string                   `json:"priorIncidentId,omitempty"`
	PriorControlFailure    string                   `json:"priorControlFailure,omitempty"`
}

type DeliveryRecoveryIncidentMutation struct {
	Delivery RequiredDelivery         `json:"delivery"`
	Incident DeliveryRecoveryIncident `json:"incident"`
	Event    DeliveryEvent            `json:"event"`
	Receipt  DeliveryReceipt          `json:"receipt"`
	Replay   bool                     `json:"replay"`
}

type DeliveryReceipt struct {
	ID        string    `json:"id"`
	RequestID string    `json:"requestId"`
	Operation string    `json:"operation"`
	CreatedAt time.Time `json:"createdAt"`
}

type DeliveryMutation struct {
	Delivery RequiredDelivery `json:"delivery"`
	Event    DeliveryEvent    `json:"event"`
	Block    *DeliveryBlock   `json:"block,omitempty"`
	Receipt  DeliveryReceipt  `json:"receipt"`
	Replay   bool             `json:"replay"`
}

type CreateRequiredDeliveryRequest struct {
	RequestID                 string           `json:"requestId"`
	MessageSeq                int64            `json:"messageSeq"`
	Kind                      string           `json:"kind"`
	RecipientKind             string           `json:"recipientKind,omitempty"`
	ActionKey                 string           `json:"actionKey,omitempty"`
	ActionClass               string           `json:"actionClass,omitempty"`
	AgentID                   string           `json:"agentId"`
	RunID                     string           `json:"runId"`
	ItemTaskID                string           `json:"itemTaskId"`
	ItemID                    string           `json:"itemId"`
	ItemRevision              int64            `json:"itemRevision"`
	WorkOrderMessage          MessageReference `json:"workOrderMessage"`
	GoverningOrderMessage     MessageReference `json:"governingOrderMessage"`
	InstructionSHA256         string           `json:"instructionSha256"`
	InstructionBytes          int64            `json:"instructionBytes"`
	EnrollmentVersion         int              `json:"enrollmentVersion"`
	ExpectedCurrentGeneration int64            `json:"expectedCurrentGeneration"`
	ExpectedCurrentDeliveryID string           `json:"expectedCurrentDeliveryId,omitempty"`
	ProducerAgentID           string           `json:"producerAgentId,omitempty"`
	ProducerRunID             string           `json:"producerRunId,omitempty"`
}

type DeliveryCoverage struct {
	TaskID                  string            `json:"taskId"`
	AgentID                 string            `json:"agentId"`
	RunID                   string            `json:"runId"`
	ItemTaskID              string            `json:"itemTaskId"`
	ItemID                  string            `json:"itemId"`
	ItemRevision            int64             `json:"itemRevision"`
	CurrentItemRevision     int64             `json:"currentItemRevision"`
	BindingWorkOrderMessage MessageReference  `json:"bindingWorkOrderMessage"`
	ContextDigest           string            `json:"contextDigest"`
	AgentStatus             string            `json:"agentStatus"`
	Status                  string            `json:"status"`
	Reason                  string            `json:"reason"`
	Delivery                *RequiredDelivery `json:"delivery,omitempty"`
}

type DeliveryCoverageList struct {
	TaskID  string             `json:"taskId"`
	AgentID string             `json:"agentId"`
	RunID   string             `json:"runId"`
	Actions []DeliveryCoverage `json:"actions"`
}

type DeliveryActionRequest struct {
	RequestID     string `json:"requestId"`
	AgentID       string `json:"agentId"`
	RunID         string `json:"runId"`
	ExpectedEpoch int64  `json:"expectedEpoch,omitempty"`
	Text          string `json:"text,omitempty"`
}

type DeliveryBlockRequest struct {
	RequestID     string `json:"requestId"`
	AgentID       string `json:"agentId"`
	RunID         string `json:"runId"`
	ExpectedEpoch int64  `json:"expectedEpoch"`
	ReasonClass   string `json:"reasonClass"`
	Text          string `json:"text,omitempty"`
}

type DeliveryResolutionRequest struct {
	RequestID     string `json:"requestId"`
	AgentID       string `json:"agentId,omitempty"`
	RunID         string `json:"runId,omitempty"`
	ExpectedEpoch int64  `json:"expectedEpoch"`
	Text          string `json:"text"`
}

type DeliveryResumeRequest struct {
	RequestID     string `json:"requestId"`
	AgentID       string `json:"agentId"`
	RunID         string `json:"runId"`
	ExpectedEpoch int64  `json:"expectedEpoch"`
	ResolutionID  string `json:"resolutionId"`
}

type DeliveryRuntimeObservation struct {
	State      string    `json:"state"`
	Source     string    `json:"source"`
	ActivityID string    `json:"activityId,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

type DeliveryFollowThroughCheckRequest struct {
	RequestID          string                     `json:"requestId"`
	AgentID            string                     `json:"agentId"`
	RunID              string                     `json:"runId"`
	ExpectedGeneration int64                      `json:"expectedGeneration"`
	ExpectedEpoch      int64                      `json:"expectedEpoch"`
	Observation        DeliveryRuntimeObservation `json:"observation"`
}

type DeliveryFollowThroughDecision struct {
	Delivery             RequiredDelivery `json:"delivery"`
	Classification       string           `json:"classification"`
	Reason               string           `json:"reason"`
	Action               string           `json:"action"`
	Attempt              int              `json:"attempt,omitempty"`
	Execute              bool             `json:"execute"`
	EscalationMessageSeq int64            `json:"escalationMessageSeq,omitempty"`
	Event                *DeliveryEvent   `json:"event,omitempty"`
	Receipt              *DeliveryReceipt `json:"receipt,omitempty"`
	Replay               bool             `json:"replay"`
}

type DeliveryFollowThroughReportRequest struct {
	RequestID          string `json:"requestId"`
	AgentID            string `json:"agentId"`
	RunID              string `json:"runId"`
	ExpectedGeneration int64  `json:"expectedGeneration"`
	ExpectedEpoch      int64  `json:"expectedEpoch"`
	LeaseRequestID     string `json:"leaseRequestId"`
	Outcome            string `json:"outcome"`
	Text               string `json:"text,omitempty"`
}

type DeliveryFollowThroughReport struct {
	Delivery RequiredDelivery `json:"delivery"`
	Event    DeliveryEvent    `json:"event"`
	Receipt  DeliveryReceipt  `json:"receipt"`
	Replay   bool             `json:"replay"`
}

func (c *Client) CreateRequiredDelivery(ctx context.Context, task string, req CreateRequiredDeliveryRequest) (DeliveryMutation, error) {
	var out DeliveryMutation
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/required-deliveries", req, &out)
	return out, err
}

func (c *Client) CurrentAssignment(ctx context.Context, task, agent, runID string) (RequiredDelivery, error) {
	return c.CurrentAssignmentForAction(ctx, task, agent, runID, "")
}

func (c *Client) CurrentAssignmentForAction(ctx context.Context, task, agent, runID, actionKey string) (RequiredDelivery, error) {
	return c.CurrentAssignmentForResponsibility(ctx, task, agent, runID, "", actionKey)
}

func (c *Client) CurrentAssignmentForResponsibility(ctx context.Context, task, agent, runID, itemID, actionKey string) (RequiredDelivery, error) {
	var out RequiredDelivery
	q := url.Values{}
	q.Set("runId", runID)
	if itemID != "" {
		q.Set("itemId", itemID)
	}
	if actionKey != "" {
		q.Set("actionKey", actionKey)
	}
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/agents/"+url.PathEscape(agent)+"/current-assignment?"+q.Encode(), nil, &out)
	return out, err
}

func (c *Client) DeliveryCoverage(ctx context.Context, task, agent, runID string) (DeliveryCoverage, error) {
	return c.DeliveryCoverageForAction(ctx, task, agent, runID, "")
}

func (c *Client) DeliveryCoverageForAction(ctx context.Context, task, agent, runID, actionKey string) (DeliveryCoverage, error) {
	return c.DeliveryCoverageForResponsibility(ctx, task, agent, runID, "", actionKey)
}

func (c *Client) DeliveryCoverageForResponsibility(ctx context.Context, task, agent, runID, itemID, actionKey string) (DeliveryCoverage, error) {
	var out DeliveryCoverage
	q := url.Values{}
	q.Set("runId", runID)
	if itemID != "" {
		q.Set("itemId", itemID)
	}
	if actionKey != "" {
		q.Set("actionKey", actionKey)
	}
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/agents/"+url.PathEscape(agent)+"/delivery-coverage?"+q.Encode(), nil, &out)
	return out, err
}

func (c *Client) DeliveryCoverages(ctx context.Context, task, agent, runID string) (DeliveryCoverageList, error) {
	var out DeliveryCoverageList
	q := url.Values{}
	q.Set("runId", runID)
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/agents/"+url.PathEscape(agent)+"/delivery-coverages?"+q.Encode(), nil, &out)
	return out, err
}

func (c *Client) DeliveryAction(ctx context.Context, task, delivery, operation string, req any) (DeliveryMutation, error) {
	var out DeliveryMutation
	path := "/v1/tasks/" + url.PathEscape(task) + "/deliveries/" + url.PathEscape(delivery) + "/" + url.PathEscape(operation)
	err := c.do(ctx, "POST", path, req, &out)
	return out, err
}

func (c *Client) ResolveDeliveryBlock(ctx context.Context, task, delivery, block string, req DeliveryResolutionRequest) (DeliveryMutation, error) {
	var out DeliveryMutation
	path := "/v1/tasks/" + url.PathEscape(task) + "/deliveries/" + url.PathEscape(delivery) + "/blocks/" + url.PathEscape(block) + "/resolutions"
	err := c.do(ctx, "POST", path, req, &out)
	return out, err
}

func (c *Client) CheckDeliveryFollowThrough(ctx context.Context, task, delivery string, req DeliveryFollowThroughCheckRequest) (DeliveryFollowThroughDecision, error) {
	var out DeliveryFollowThroughDecision
	path := "/v1/tasks/" + url.PathEscape(task) + "/deliveries/" + url.PathEscape(delivery) + "/follow-through/check"
	err := c.do(ctx, "POST", path, req, &out)
	return out, err
}

func (c *Client) ReportDeliveryFollowThrough(ctx context.Context, task, delivery string, req DeliveryFollowThroughReportRequest) (DeliveryFollowThroughReport, error) {
	var out DeliveryFollowThroughReport
	path := "/v1/tasks/" + url.PathEscape(task) + "/deliveries/" + url.PathEscape(delivery) + "/follow-through/report"
	err := c.do(ctx, "POST", path, req, &out)
	return out, err
}

func (c *Client) RecordDeliveryRecoveryIncident(ctx context.Context, task, delivery string, req DeliveryRecoveryIncidentRequest) (DeliveryRecoveryIncidentMutation, error) {
	var out DeliveryRecoveryIncidentMutation
	path := "/v1/tasks/" + url.PathEscape(task) + "/deliveries/" + url.PathEscape(delivery) + "/recovery-incidents"
	err := c.do(ctx, "POST", path, req, &out)
	return out, err
}
