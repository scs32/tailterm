package api

// The owner helper (docs/owner-helper.md): the owner's own Claude Code
// session, registered as a project agent so it can be a delegation-window
// delegate and be woken like other Claude agents. It is never leased, counted
// as a team member or handler, or closed with an item team.

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
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
type RegisterOwnerHelperRequest struct {
	Name      string `json:"name,omitempty"`
	Host      string `json:"host"`
	Session   string `json:"session"`
	Runtime   string `json:"runtime"`
	Cwd       string `json:"cwd,omitempty"`
	RequestID string `json:"requestId"`
}

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
	case req.Runtime != "claude":
		return fmt.Errorf("%w: the owner helper runtime is claude", ErrInvalid)
	case !ValidText(req.Cwd, 1024) || (req.Cwd != "" && !filepath.IsAbs(req.Cwd)):
		return fmt.Errorf("%w: cwd must be an absolute path", ErrInvalid)
	}
	return nil
}

func (c *Client) RegisterOwnerHelper(ctx context.Context, task string, req RegisterOwnerHelperRequest) (OwnerActionResult, error) {
	return c.ownerAction(ctx, "/v1/tasks/"+url.PathEscape(task)+"/owner-helper", req)
}
