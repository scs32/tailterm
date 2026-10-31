package api

// The owner helper (docs/owner-helper.md): the owner's own Claude Code or Codex
// session, registered as a project agent so it can be a delegation-window
// delegate and be woken through its exact runtime binding. It is never leased, counted
// as a team member or handler, or closed with an item team.

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// AgentRoleOwnerHelper is the owner helper's agent role. Only the register
// route creates it; spawn and addAgent refuse it.
const AgentRoleOwnerHelper = "owner_helper"

// DefaultOwnerHelperName is the helper's agent name unless the owner picks one.
const DefaultOwnerHelperName = "owner-helper"

// Registration modes.
const (
	OwnerHelperCreated    = "created"    // a new agent
	OwnerHelperReattached = "reattached" // an exited helper got a new run
	OwnerHelperReplaced   = "replaced"   // a live helper's run was replaced
)

// RegisterOwnerHelperRequest binds the project's helper to one owner session.
// Session is the tmux session name, or "terminal" outside tmux.
//
// ExpectedRunID, when set, makes the registration conditional: the hub replaces
// the run only if the helper's current run is this one and the helper is
// running, done or needs-input, and otherwise refuses with a conflict and
// writes nothing. It must stay omitempty: stored pending requests are replayed
// by the hash of this JSON.
type RegisterOwnerHelperRequest struct {
	Name          string `json:"name,omitempty"`
	Host          string `json:"host"`
	Session       string `json:"session"`
	Runtime       string `json:"runtime"`
	Cwd           string `json:"cwd,omitempty"`
	RequestID     string `json:"requestId"`
	ExpectedRunID string `json:"expectedRunId,omitempty"`
}

var ownerHelperRunRE = regexp.MustCompile(`^run_[0-9a-f]{16}$`)

// OwnerHelperRegistration is the receipt of one registration.
type OwnerHelperRegistration struct {
	ID            string    `json:"id"`
	TaskID        string    `json:"taskId"`
	AgentID       string    `json:"agentId"`
	RunID         string    `json:"runId"`
	PreviousRunID string    `json:"previousRunId,omitempty"`
	Mode          string    `json:"mode"`
	Host          string    `json:"host"`
	Session       string    `json:"session"`
	Runtime       string    `json:"runtime"`
	Cwd           string    `json:"cwd,omitempty"`
	RequestID     string    `json:"requestId"`
	By            Sender    `json:"by"`
	CreatedAt     time.Time `json:"createdAt"`
}

// ValidateRegisterOwnerHelper checks the request shape and fills the default
// name. The store checks the project and the existing helper.
func ValidateRegisterOwnerHelper(req *RegisterOwnerHelperRequest) error {
	if req.Name == "" {
		req.Name = DefaultOwnerHelperName
	}
	switch {
	case !ValidName(req.Name):
		return fmt.Errorf("%w: invalid helper name", ErrInvalid)
	case !ValidName(req.Session):
		return fmt.Errorf("%w: invalid session name", ErrInvalid)
	case req.Host == "" || !ValidText(req.Host, 253):
		return fmt.Errorf("%w: host is required", ErrInvalid)
	case req.Runtime != "claude" && req.Runtime != "codex":
		return fmt.Errorf("%w: the owner helper runtime must be claude or codex", ErrInvalid)
	case !ValidText(req.Cwd, 1024) || (req.Cwd != "" && !filepath.IsAbs(req.Cwd)):
		return fmt.Errorf("%w: cwd must be an absolute path", ErrInvalid)
	case req.ExpectedRunID != "" && !ownerHelperRunRE.MatchString(req.ExpectedRunID):
		return fmt.Errorf("%w: expectedRunId must be a run ID", ErrInvalid)
	}
	return nil
}

func (c *Client) RegisterOwnerHelper(ctx context.Context, task string, req RegisterOwnerHelperRequest) (OwnerActionResult, error) {
	return c.ownerAction(ctx, "/v1/tasks/"+url.PathEscape(task)+"/owner-helper", req)
}

// ValidOwnerHelperRun reports whether run has the shape of a run ID.
func ValidOwnerHelperRun(run string) bool { return ownerHelperRunRE.MatchString(run) }

// GetOwnerHelperRegistration returns the receipt of the registration that
// started the named owner-helper run.
func (c *Client) GetOwnerHelperRegistration(ctx context.Context, task, run string) (OwnerHelperRegistration, error) {
	var out OwnerHelperRegistration
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/owner-helper/registrations/"+url.PathEscape(run), nil, &out)
}

// The owner helper's continuity record (docs/session-handoff.md, "The
// continuity record on the hub") is an append-only list of revisions holding
// the helper's free-text continuity log. Any identified caller reads it from
// any host; the owner or the current helper's exact run saves it.
const (
	// MaxOwnerHelperContinuityLen bounds a continuity body in bytes.
	MaxOwnerHelperContinuityLen = 64 * 1024
	// MaxOwnerHelperContinuityRequest bounds the POST body: a maximal body
	// of characters JSON escapes as \uXXXX, plus the other fields.
	MaxOwnerHelperContinuityRequest = 6*MaxOwnerHelperContinuityLen + 4*1024
)

// OwnerHelperContinuity is one saved revision. Revision lists leave Body empty.
type OwnerHelperContinuity struct {
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

// SaveOwnerHelperContinuityRequest appends revision ExpectedRevision+1. Only
// the owner (no agent and no run) or the current owner helper's exact run may
// save. A replayed RequestID with the same input returns the saved row.
type SaveOwnerHelperContinuityRequest struct {
	ExpectedRevision int64  `json:"expectedRevision"`
	Body             string `json:"body"`
	RequestID        string `json:"requestId"`
	AgentID          string `json:"agentId,omitempty"`
	RunID            string `json:"runId,omitempty"`
}

// OwnerHelperContinuity reads the latest revision, or revision rev when rev > 0.
func (c *Client) OwnerHelperContinuity(ctx context.Context, task string, rev int64) (OwnerHelperContinuity, error) {
	var out OwnerHelperContinuity
	path := "/v1/tasks/" + url.PathEscape(task) + "/owner-helper/continuity"
	if rev > 0 {
		path += "?revision=" + strconv.FormatInt(rev, 10)
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// OwnerHelperContinuityRevisions lists every revision without bodies.
func (c *Client) OwnerHelperContinuityRevisions(ctx context.Context, task string) ([]OwnerHelperContinuity, error) {
	var out struct {
		Revisions []OwnerHelperContinuity `json:"revisions"`
	}
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/owner-helper/continuity/revisions", nil, &out)
	return out.Revisions, err
}

// SaveOwnerHelperContinuity saves the next revision.
func (c *Client) SaveOwnerHelperContinuity(ctx context.Context, task string, req SaveOwnerHelperContinuityRequest) (OwnerHelperContinuity, error) {
	var out OwnerHelperContinuity
	return out, c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/owner-helper/continuity", req, &out)
}
