package api

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// Backlog steward (docs/backlog-steward.md): one persistent agent per
// project whose context is the backlog. It takes intake, researches it and
// drafts items the database handler files; it proposes batches, queue order,
// scopes and triage outcomes as owner decisions and keeps a backlog summary
// that rotation hands to a fresh session. It holds no per-item records role.
const AgentRoleBacklogSteward = "backlog_steward"

const (
	// StewardRefusedActive: the project already has a steward. Starting,
	// running, done, retired and exited stewards all hold the slot; only a
	// close or a committed rotation frees it.
	StewardRefusedActive = "steward_active"
	// StewardRefusedWrite: the steward files through the database handler.
	StewardRefusedWrite = "steward_write"
)

// StewardRefusal is a 409 with a named reason. A refused call changes nothing.
type StewardRefusal struct {
	Code   string
	Detail string
}

func (e *StewardRefusal) Error() string {
	return fmt.Sprintf("backlog steward refused (%s): %s", e.Code, e.Detail)
}

func (e *StewardRefusal) Unwrap() error { return ErrConflict }

// PersistentAgentRole reports the project roles that are admitted without a
// parent or work item under a stable agent ID, and restart by expected run.
func PersistentAgentRole(role string) bool {
	return role == AgentRoleDatabaseHandler || role == AgentRoleDeployment || role == AgentRoleBacklogSteward
}

// BacklogStewardStatus is what launches and briefings need to know about a
// project's steward.
type BacklogStewardStatus struct {
	TaskID string `json:"taskId"`
	// Steward is the active steward, the one role:backlog_steward reaches.
	Steward *Agent `json:"steward,omitempty"`
	// Holder is the slot holder, which may be exited and awaiting a restart.
	Holder *Agent `json:"holder,omitempty"`
	// PendingSuccessorID is a prepared rotation's registered successor.
	PendingSuccessorID string `json:"pendingSuccessorId,omitempty"`
	// SummaryRevision and SummaryDigest name the latest backlog summary; 0
	// means none has been saved.
	SummaryRevision int64  `json:"summaryRevision"`
	SummaryDigest   string `json:"summaryDigest,omitempty"`
}

func (c *Client) BacklogSteward(ctx context.Context, task string) (BacklogStewardStatus, error) {
	var out BacklogStewardStatus
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/backlog-steward", nil, &out)
}

// The backlog summary is the steward's durable state: an append-only list of
// revisions that rotation hands to a fresh session instead of chat history.
const (
	// MaxBacklogSummaryLen bounds a summary body in bytes.
	MaxBacklogSummaryLen = 64 * 1024
	// MaxBacklogSummaryRequest bounds the POST body: a maximal summary of
	// characters JSON escapes as \uXXXX, plus the other fields.
	MaxBacklogSummaryRequest = 6*MaxBacklogSummaryLen + 4*1024
)

// BacklogSummary is one saved revision.
type BacklogSummary struct {
	TaskID    string    `json:"taskId"`
	Revision  int64     `json:"revision"`
	Body      string    `json:"body,omitempty"`
	Digest    string    `json:"digest"`
	Bytes     int       `json:"bytes"`
	AgentID   string    `json:"agentId,omitempty"`
	RunID     string    `json:"runId,omitempty"`
	RequestID string    `json:"requestId"`
	CreatedBy Caller    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

// SaveBacklogSummaryRequest appends revision ExpectedRevision+1. Only the
// exact run of the project's active steward, or the owner (no agent), may
// save. A replayed RequestID with the same input returns the saved row.
type SaveBacklogSummaryRequest struct {
	ExpectedRevision int64  `json:"expectedRevision"`
	Body             string `json:"body"`
	RequestID        string `json:"requestId"`
	AgentID          string `json:"agentId,omitempty"`
	RunID            string `json:"runId,omitempty"`
}

// BacklogSummary reads the latest revision, or revision rev when rev > 0.
func (c *Client) BacklogSummary(ctx context.Context, task string, rev int64) (BacklogSummary, error) {
	var out BacklogSummary
	path := "/v1/tasks/" + url.PathEscape(task) + "/backlog-summary"
	if rev > 0 {
		path += "?revision=" + strconv.FormatInt(rev, 10)
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// BacklogSummaryRevisions lists every revision without bodies.
func (c *Client) BacklogSummaryRevisions(ctx context.Context, task string) ([]BacklogSummary, error) {
	var out struct {
		Revisions []BacklogSummary `json:"revisions"`
	}
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/backlog-summary/revisions", nil, &out)
	return out.Revisions, err
}

func (c *Client) SaveBacklogSummary(ctx context.Context, task string, req SaveBacklogSummaryRequest) (BacklogSummary, error) {
	var out BacklogSummary
	return out, c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/backlog-summary", req, &out)
}

// Steward rotation replaces the project's steward with a fresh session that
// has the current template, handing over the backlog summary rather than
// chat history. It is two-phase like handler rotation: prepare admits the
// successor as a non-holding steward; commit, once the successor is online,
// closes the old steward and makes the successor the holder in one step.
const (
	StewardRotationPrepared  = "prepared"
	StewardRotationCommitted = "committed"
	StewardRotationAborted   = "aborted"

	StewardRotationPrepare = "prepare"
	StewardRotationCommit  = "commit"
	StewardRotationAbort   = "abort"

	StewardRotationReasonManual   = "manual"
	StewardRotationReasonTokens   = "tokens"
	StewardRotationReasonTemplate = "template"

	StewardRotationTriggerOwner  = "owner"
	StewardRotationTriggerRunner = "runner"

	StewardRefusedWorking        = "working"
	StewardRefusedPendingTool    = "pending_tool"
	StewardRefusedRotationOpen   = "rotation_open"
	StewardRefusedPaused         = "project_paused"
	StewardRefusedNotSteward     = "not_steward"
	StewardRefusedSummaryMissing = "summary_missing"
	StewardRefusedSuccessor      = "successor_unavailable"
	StewardRefusedNameTaken      = "name_taken"
	StewardRefusedAgentCaller    = "agent_caller"
	StewardRefusedStalePolicy    = "stale_revision"
	// StewardRefusedRotationStale: the successor of a prepared rotation whose
	// old steward has closed is never admitted.
	StewardRefusedRotationStale = "rotation_stale"
)

var stewardRotatedSuffix = regexp.MustCompile(`-r([0-9]+)$`)

// StewardSuccessorName names a steward's rotation successor: the base name
// plus -r2, or the next -rN after an earlier rotation.
func StewardSuccessorName(oldName string) string {
	next := 2
	if m := stewardRotatedSuffix.FindStringSubmatch(oldName); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			next = n + 1
		}
	}
	return stewardRotatedSuffix.ReplaceAllString(oldName, "") + fmt.Sprintf("-r%d", next)
}

// StewardRotationPolicy says when the host runner rotates a project's
// steward. Revision 0 means the owner has not saved one: new projects use
// the defaults with rotation enabled.
type StewardRotationPolicy struct {
	TaskID           string     `json:"taskId"`
	Enabled          bool       `json:"enabled"`
	MaxTotalTokens   int64      `json:"maxTotalTokens"`
	OnTemplateChange bool       `json:"onTemplateChange"`
	Revision         int64      `json:"revision"`
	UpdatedAt        *time.Time `json:"updatedAt,omitempty"`
}

// A zero MaxTotalTokens turns that limit off.
type StewardRotationPolicyRequest struct {
	ExpectedRevision int64  `json:"expectedRevision"`
	Enabled          bool   `json:"enabled"`
	MaxTotalTokens   int64  `json:"maxTotalTokens"`
	OnTemplateChange bool   `json:"onTemplateChange"`
	ActorAgentID     string `json:"actorAgentId,omitempty"`
}

type StewardRotationRequest struct {
	Operation string `json:"operation"`
	RequestID string `json:"requestId"`
	// prepare
	OldAgentID       string `json:"oldAgentId,omitempty"`
	OldRunID         string `json:"oldRunId,omitempty"`
	SuccessorAgentID string `json:"successorAgentId,omitempty"`
	SuccessorName    string `json:"successorName,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Trigger          string `json:"trigger,omitempty"`
	// commit and abort
	RotationID   string `json:"rotationId,omitempty"`
	ActorAgentID string `json:"actorAgentId,omitempty"`
}

type StewardRotation struct {
	ID               string                  `json:"id"`
	TaskID           string                  `json:"taskId"`
	RequestID        string                  `json:"requestId"`
	State            string                  `json:"state"`
	Reason           string                  `json:"reason"`
	Trigger          string                  `json:"trigger"`
	OldAgentID       string                  `json:"oldAgentId"`
	OldRunID         string                  `json:"oldRunId"`
	OldName          string                  `json:"oldName"`
	SuccessorAgentID string                  `json:"successorAgentId"`
	SuccessorName    string                  `json:"successorName"`
	SuccessorRunID   string                  `json:"successorRunId,omitempty"`
	SummaryRevision  int64                   `json:"summaryRevision"`
	SummaryDigest    string                  `json:"summaryDigest"`
	Handoff          *StewardRotationHandoff `json:"handoff,omitempty"`
	Receipt          *StewardRotationReceipt `json:"receipt,omitempty"`
	CreatedAt        time.Time               `json:"createdAt"`
	UpdatedAt        time.Time               `json:"updatedAt"`
}

// StewardRotationHandoff is what the successor reads first: the summary
// revision it inherits, the obligations that moved and the decisions the old
// steward proposed that are still unanswered.
type StewardRotationHandoff struct {
	SummaryRevision int64  `json:"summaryRevision"`
	SummaryDigest   string `json:"summaryDigest"`
	// SummaryFromOldRun says the old steward's exact run saved that revision.
	SummaryFromOldRun bool                      `json:"summaryFromOldRun"`
	Reissued          []HandlerRotationReissue  `json:"reissued"`
	OpenDecisions     []StewardRotationDecision `json:"openDecisions"`
}

type StewardRotationDecision struct {
	MessageSeq int64  `json:"messageSeq"`
	Question   string `json:"question"`
}

type StewardRotationReceipt struct {
	RotationID      string    `json:"rotationId"`
	RequestID       string    `json:"requestId"`
	StewardAgentID  string    `json:"stewardAgentId"`
	StewardRunID    string    `json:"stewardRunId"`
	ClosedAgentID   string    `json:"closedAgentId"`
	ClosedRunID     string    `json:"closedRunId"`
	SummaryRevision int64     `json:"summaryRevision"`
	NoticeSeq       int64     `json:"noticeSeq"`
	Reissued        int       `json:"reissued"`
	CommittedAt     time.Time `json:"committedAt"`
}

// StewardRotationDue is one project's steward on a host, with the counters
// the runner compares against the project's enabled policy.
type StewardRotationDue struct {
	TaskID          string                `json:"taskId"`
	Agent           Agent                 `json:"agent"`
	Policy          StewardRotationPolicy `json:"policy"`
	TotalTokens     int64                 `json:"totalTokens"`
	RecordedDigest  string                `json:"recordedDigest"`
	DigestMatches   bool                  `json:"digestMatches"`
	ActivityState   string                `json:"activityState"`
	PendingTool     string                `json:"pendingTool,omitempty"`
	Idle            bool                  `json:"idle"`
	SummaryRevision int64                 `json:"summaryRevision"`
	DueReasons      []string              `json:"dueReasons"`
	OpenRotation    *StewardRotation      `json:"openRotation,omitempty"`
}

type StewardRotationDueList struct {
	Entries []StewardRotationDue `json:"entries"`
}

func (c *Client) StewardRotationPolicy(ctx context.Context, task string) (StewardRotationPolicy, error) {
	var out StewardRotationPolicy
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/steward-rotation/policy", nil, &out)
}

func (c *Client) SetStewardRotationPolicy(ctx context.Context, task string, req StewardRotationPolicyRequest) (StewardRotationPolicy, error) {
	var out StewardRotationPolicy
	return out, c.do(ctx, "PUT", "/v1/tasks/"+url.PathEscape(task)+"/steward-rotation/policy", req, &out)
}

func (c *Client) StewardRotationAction(ctx context.Context, task string, req StewardRotationRequest) (StewardRotation, error) {
	var out StewardRotation
	return out, c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/steward-rotations", req, &out)
}

func (c *Client) GetStewardRotation(ctx context.Context, task, id string) (StewardRotation, error) {
	var out StewardRotation
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/steward-rotations/"+url.PathEscape(id), nil, &out)
}

func (c *Client) ListStewardRotations(ctx context.Context, task string) ([]StewardRotation, error) {
	var out struct {
		Rotations []StewardRotation `json:"rotations"`
	}
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/steward-rotations", nil, &out)
	return out.Rotations, err
}

func (c *Client) StewardRotationsDue(ctx context.Context, host, templateDigest string) (StewardRotationDueList, error) {
	var out StewardRotationDueList
	q := url.Values{"host": {host}, "templateDigest": {templateDigest}}
	return out, c.do(ctx, "GET", "/v1/steward-rotations/due?"+q.Encode(), nil, &out)
}
