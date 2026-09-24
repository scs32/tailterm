package api

import "time"

// Obligations (broker phase 2a, docs/broker-phase-2a.md): a typed message or a
// directed human message obliges its recipient until a typed outcome closes it.
const (
	// What the obligation requires.
	ObligationNeedsOutcome  = "ack_outcome" // ack, then result/decline (assign, request, review, block, directed human)
	ObligationNeedsAnswer   = "answer"      // answer/result/decline (question)
	ObligationNeedsDelivery = "delivery"    // reaching the recipient is enough (notice, finding)

	ObligationQueued       = "queued"
	ObligationDelivered    = "delivered"
	ObligationAcknowledged = "acknowledged"
	ObligationWorking      = "working"
	ObligationBlocked      = "blocked"
	ObligationClosed       = "closed"

	OutcomeResult        = "result"
	OutcomeAnswered      = "answered"
	OutcomeDeclined      = "declined"
	OutcomeDelivered     = "delivered"
	OutcomeSuperseded    = "superseded"
	OutcomeCancelled     = "cancelled"
	OutcomeRecipientGone = "recipient_gone"

	// BrokerNode marks hub-authored messages (escalations and reassignments).
	// They never create human obligations, which would loop.
	BrokerNode = "hub-broker"
)

// Broker timer defaults from docs/message-broker.md.
const (
	ObligationAckDeadline       = 10 * time.Minute
	ObligationSilenceNudge      = 30 * time.Minute
	ObligationMaxNudges         = 2
	ObligationOwnerAfterLead    = 15 * time.Minute
	ObligationDefaultDue        = 2 * time.Hour
	ObligationQuestionDue       = 30 * time.Minute
	ObligationProjectStallQuiet = 15 * time.Minute
)

// ObligationWakeOffsets are the re-wake times after creation while unacknowledged.
var ObligationWakeOffsets = []time.Duration{time.Minute, 3 * time.Minute, 7 * time.Minute}

type Obligation struct {
	ID             string     `json:"id"`
	TaskID         string     `json:"taskId"`
	MessageSeq     int64      `json:"messageSeq"`
	Subject        string     `json:"subject,omitempty"`
	SourceKind     string     `json:"sourceKind"`
	Needs          string     `json:"needs"`
	AgentID        string     `json:"agentId"`
	State          string     `json:"state"`
	Outcome        string     `json:"outcome,omitempty"`
	OutcomeSeq     int64      `json:"outcomeSeq,omitempty"`
	Reason         string     `json:"reason,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	AckDueAt       time.Time  `json:"ackDueAt"`
	DueAt          time.Time  `json:"dueAt"`
	DeliveredAt    *time.Time `json:"deliveredAt,omitempty"`
	AckedAt        *time.Time `json:"ackedAt,omitempty"`
	LastProgressAt *time.Time `json:"lastProgressAt,omitempty"`
	ClosedAt       *time.Time `json:"closedAt,omitempty"`
	Escalation     int        `json:"escalation"` // 0 none, 1 lead, 2 owner
	Nudges         int        `json:"nudges"`
	Overdue        string     `json:"overdue,omitempty"` // ack, silence, outcome
}

type ObligationList struct {
	Obligations []Obligation `json:"obligations"`
}

// ObligationActionRequest acknowledges, records progress on, or resumes an
// obligation. Only the recipient agent's current run may act.
type ObligationActionRequest struct {
	AgentID   string `json:"agentId"`
	RunID     string `json:"runId"`
	Text      string `json:"text,omitempty"`
	RequestID string `json:"requestId,omitempty"` // a retry returns the original result
}

// ObligationReassignRequest moves an open obligation. Without an actor it is
// the owner acting; an actor must be the project lead's current run.
type ObligationReassignRequest struct {
	ToAgentID    string `json:"toAgentId"`
	Reason       string `json:"reason"`
	ActorAgentID string `json:"actorAgentId,omitempty"`
	ActorRunID   string `json:"actorRunId,omitempty"`
}

// WakeJob is one broker-requested wake of an agent's runtime, leased by the
// host relay, which reports how the runtime answered.
type WakeJob struct {
	ID           string `json:"id"`
	LeaseToken   string `json:"leaseToken"`
	ObligationID string `json:"obligationId"`
	MessageSeq   int64  `json:"messageSeq"`
	AgentID      string `json:"agentId"`
	RunID        string `json:"runId"`
	Prompt       string `json:"prompt"`
}

type WakeJobReport struct {
	LeaseToken string `json:"leaseToken"`
	Status     string `json:"status"` // accepted, failed, ambiguous
	Detail     string `json:"detail,omitempty"`
}

// ObligationNudgeRequest optionally names the nudge so a retry is not a
// second nudge.
type ObligationNudgeRequest struct {
	RequestID string `json:"requestId,omitempty"`
}

// ObligationNudgeResult is the owner's immediate re-wake of an obligation's
// recipient, outside the broker's retry schedule (broker phase 2b).
type ObligationNudgeResult struct {
	Obligation Obligation `json:"obligation"`
	WakeJobID  string     `json:"wakeJobId"`
}

// ObligationNudgeInterval is the minimum time between owner nudges of one obligation.
const ObligationNudgeInterval = 2 * time.Minute

// ErrNudgeTooSoon means the obligation was nudged by the owner less than
// ObligationNudgeInterval ago; RetryAfter says when it may be nudged again.
type ErrNudgeTooSoon struct{ RetryAfter time.Duration }

func (e *ErrNudgeTooSoon) Error() string {
	return "this obligation was nudged recently; try again in " + e.RetryAfter.Round(time.Second).String()
}
