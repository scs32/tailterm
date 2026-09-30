package api

import (
	"context"
	"fmt"
	"net/url"
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
