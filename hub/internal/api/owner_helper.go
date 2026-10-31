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

// The owner helper's wakes on the hub (docs/session-handoff.md, "Wakes on the
// hub"): recurring wakes the hub stores and its broker delivers as a directed
// NOTICE to the project's current owner helper, recording when each slot was
// due, posted and read, and reporting a missed, late or unread slot to the
// owner. They replace the helper's session-only crons.

// OwnerHelperWakeKinds is what a hub wake is for. queue-check is the team
// watch. evening-summary exists only on the hub list; host wake notes keep
// their own list.
var OwnerHelperWakeKinds = []string{"hourly-update", "morning-summary", "evening-summary", "queue-check", "obligation-check", "authority-expiry", "other"}

// ValidOwnerHelperWakeKind reports whether kind is in OwnerHelperWakeKinds.
func ValidOwnerHelperWakeKind(kind string) bool {
	for _, k := range OwnerHelperWakeKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Wake states.
const (
	OwnerHelperWakeActive    = "active"
	OwnerHelperWakeCancelled = "cancelled"
	OwnerHelperWakeExpired   = "expired"
)

// Firing outcomes: what the hub did with one due slot.
const (
	OwnerHelperWakePosted        = "posted"         // a NOTICE went to the helper
	OwnerHelperWakeMissed        = "missed"         // an earlier slot of a catch-up; not delivered
	OwnerHelperWakeHelperAbsent  = "helper-absent"  // no running, done or needs-input helper
	OwnerHelperWakeSkippedPaused = "skipped-paused" // the project was paused
)

// Firing reports: what the owner was told about a slot. A report with no
// report message was folded into an earlier report of the same wake.
const (
	OwnerHelperWakeReportLate        = "late"
	OwnerHelperWakeReportMissed      = "missed"
	OwnerHelperWakeReportUndelivered = "undelivered"
	OwnerHelperWakeReportAbsent      = "helper-absent"
)

const (
	// MaxOwnerHelperWakes bounds the active wakes of one project.
	MaxOwnerHelperWakes = 20
	// OwnerHelperWakeLateAfter is how long after its due time a slot may be
	// posted, or read, before it is reported late.
	OwnerHelperWakeLateAfter = 5 * time.Minute
	// OwnerHelperWakeUnreadAfter is how long after its due time a posted
	// slot may stay unread, past the broker's 1, 3 and 7 minute re-wakes,
	// before it is reported undelivered.
	OwnerHelperWakeUnreadAfter = 15 * time.Minute
	// OwnerHelperWakeMissedRows bounds the rows one catch-up writes; more
	// slots are counted in the report, not stored.
	OwnerHelperWakeMissedRows = 48
	// OwnerHelperWakeReadWindow is how long after its due time the hub
	// keeps looking for a posted slot's read.
	OwnerHelperWakeReadWindow = 7 * 24 * time.Hour
	// DefaultOwnerHelperWakeZone is the zone a wake's schedule is read in
	// unless the request names one.
	DefaultOwnerHelperWakeZone = "America/Los_Angeles"
	// DefaultOwnerHelperWakeFirings and MaxOwnerHelperWakeFirings bound the
	// firings a list returns per wake.
	DefaultOwnerHelperWakeFirings = 5
	MaxOwnerHelperWakeFirings     = 50
	// MaxOwnerHelperWakeRequest bounds the POST body.
	MaxOwnerHelperWakeRequest = 8 * 1024
)

// OwnerHelperWake is one stored wake. NextDueAt is unset unless it is active.
type OwnerHelperWake struct {
	ID         string                  `json:"id"`
	TaskID     string                  `json:"taskId"`
	Kind       string                  `json:"kind"`
	Schedule   string                  `json:"schedule"`
	TimeZone   string                  `json:"timeZone"`
	MessageSeq int64                   `json:"messageSeq"`
	State      string                  `json:"state"`
	ExpiresAt  *time.Time              `json:"expiresAt,omitempty"`
	NextDueAt  *time.Time              `json:"nextDueAt,omitempty"`
	Revision   int64                   `json:"revision"`
	RequestID  string                  `json:"requestId"`
	AgentID    string                  `json:"agentId,omitempty"`
	RunID      string                  `json:"runId,omitempty"`
	CreatedBy  Caller                  `json:"createdBy"`
	CreatedAt  time.Time               `json:"createdAt"`
	UpdatedAt  time.Time               `json:"updatedAt"`
	Firings    []OwnerHelperWakeFiring `json:"firings,omitempty"`
}

// OwnerHelperWakeFiring is one due slot of a wake. LateMinutes is how many
// whole minutes after DueAt the slot was read, or posted when it is unread.
type OwnerHelperWakeFiring struct {
	WakeID         string     `json:"wakeId"`
	DueAt          time.Time  `json:"dueAt"`
	Outcome        string     `json:"outcome"`
	PostedAt       *time.Time `json:"postedAt,omitempty"`
	NoticeSeq      int64      `json:"noticeSeq,omitempty"`
	RecipientID    string     `json:"recipientId,omitempty"`
	RecipientRunID string     `json:"recipientRunId,omitempty"`
	DeliveredAt    *time.Time `json:"deliveredAt,omitempty"`
	LateMinutes    int64      `json:"lateMinutes"`
	Report         string     `json:"report,omitempty"`
	ReportSeq      int64      `json:"reportSeq,omitempty"`
}

// SetOwnerHelperWakeRequest creates a wake (WakeID empty) or replaces an
// existing one's fields at ExpectedRevision. State cancelled cancels it and
// takes no other field. Only the owner (no agent and no run) or the current
// owner helper's exact run may write. A replayed RequestID with the same input
// returns the saved row. ExpiresAt nil means the wake does not expire.
type SetOwnerHelperWakeRequest struct {
	WakeID           string     `json:"wakeId,omitempty"`
	Kind             string     `json:"kind,omitempty"`
	Schedule         string     `json:"schedule,omitempty"`
	TimeZone         string     `json:"timeZone,omitempty"`
	MessageSeq       int64      `json:"messageSeq,omitempty"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
	State            string     `json:"state,omitempty"`
	ExpectedRevision int64      `json:"expectedRevision,omitempty"`
	RequestID        string     `json:"requestId"`
	AgentID          string     `json:"agentId,omitempty"`
	RunID            string     `json:"runId,omitempty"`
}

// OwnerHelperWakeList is the GET route's body.
type OwnerHelperWakeList struct {
	TaskID string            `json:"taskId"`
	Wakes  []OwnerHelperWake `json:"wakes"`
}

// ListOwnerHelperWakes returns every wake of the project with its last
// firings (newest first); firings 0 means the default.
func (c *Client) ListOwnerHelperWakes(ctx context.Context, task string, firings int) (OwnerHelperWakeList, error) {
	var out OwnerHelperWakeList
	path := "/v1/tasks/" + url.PathEscape(task) + "/owner-helper/wakes"
	if firings > 0 {
		path += "?firings=" + strconv.Itoa(firings)
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}

// SetOwnerHelperWake creates, changes or cancels one wake.
func (c *Client) SetOwnerHelperWake(ctx context.Context, task string, req SetOwnerHelperWakeRequest) (OwnerHelperWake, error) {
	var out OwnerHelperWake
	return out, c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/owner-helper/wakes", req, &out)
}
