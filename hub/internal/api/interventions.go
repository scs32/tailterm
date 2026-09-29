package api

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Owner interventions are ordinary Board messages with a typed sidecar record.
// They count the times the owner had to step in, so each one can be traced to
// the product item expected to make it unnecessary. They are never edited.

// InterventionKinds is the fixed vocabulary shared by the CLI, hub and UI.
var InterventionKinds = []string{"release", "nudge", "decision-on-behalf", "gate-fix", "cleanup", "diagnosis", "status", "other"}

// MaxInterventionPage bounds one page of intervention messages.
const MaxInterventionPage = 100

// MaxInterventionText bounds the owner's description in bytes.
const MaxInterventionText = 4000

type Intervention struct {
	Kind          string `json:"kind"`
	ItemTaskID    string `json:"itemTaskId"`
	ItemID        string `json:"itemId"`
	ProductTaskID string `json:"productTaskId,omitempty"`
	ProductItemID string `json:"productItemId,omitempty"`
}

// CreateInterventionRequest is owner-only: a request that carries an agent
// identity is refused rather than recorded.
type CreateInterventionRequest struct {
	Kind          string `json:"kind"`
	ItemID        string `json:"itemId"`
	ProductItemID string `json:"productItemId,omitempty"`
	Text          string `json:"text"`
	RequestID     string `json:"requestId"`
	AgentID       string `json:"agentId,omitempty"`
}

// InterventionRef identifies one intervention without its full message.
type InterventionRef struct {
	Seq           int64  `json:"seq"`
	Kind          string `json:"kind"`
	ItemID        string `json:"itemId"`
	ProductItemID string `json:"productItemId,omitempty"`
	Day           string `json:"day"`
}

// InterventionDay counts one local calendar day in the summary's time zone.
type InterventionDay struct {
	Day    string         `json:"day"`
	Total  int            `json:"total"`
	Linked int            `json:"linked"`
	ByKind map[string]int `json:"byKind"`
}

// InterventionSummary always covers every intervention in the project, not
// only the returned page. Days are newest first. Unlinked lists the newest
// interventions without a product item, up to MaxInterventionPage.
type InterventionSummary struct {
	TimeZone string            `json:"timeZone"`
	Total    int               `json:"total"`
	Linked   int               `json:"linked"`
	ByKind   map[string]int    `json:"byKind"`
	Days     []InterventionDay `json:"days"`
	Unlinked []InterventionRef `json:"unlinked"`
}

type InterventionList struct {
	Interventions []Message           `json:"interventions"`
	Summary       InterventionSummary `json:"summary"`
	NextAfter     int64               `json:"nextAfter,omitempty"`
}

func ValidInterventionKind(kind string) bool {
	for _, known := range InterventionKinds {
		if kind == known {
			return true
		}
	}
	return false
}

// ValidateIntervention checks the request shape shared by CLI and hub. It does
// not check that the items exist; the store does that inside its transaction.
func ValidateIntervention(req CreateInterventionRequest) error {
	if !ValidInterventionKind(req.Kind) {
		return fmt.Errorf("%w: kind must be one of %s", ErrInvalid, strings.Join(InterventionKinds, ", "))
	}
	if !ValidID(req.ItemID, "wi") {
		return fmt.Errorf("%w: item must be a work item ID", ErrInvalid)
	}
	if req.ProductItemID != "" && !ValidID(req.ProductItemID, "wi") {
		return fmt.Errorf("%w: product item must be a work item ID", ErrInvalid)
	}
	if !utf8.ValidString(req.Text) || strings.TrimSpace(req.Text) == "" || !ValidText(req.Text, MaxInterventionText) {
		return fmt.Errorf("%w: describe the intervention in up to %d bytes", ErrInvalid, MaxInterventionText)
	}
	if !ValidText(FormatIntervention(req), MaxTextLen) {
		return fmt.Errorf("%w: rendered intervention exceeds the message limit", ErrInvalid)
	}
	return nil
}

// FormatIntervention renders the Board text of an intervention message.
func FormatIntervention(req CreateInterventionRequest) string {
	text := "Owner intervention (" + req.Kind + ") on " + req.ItemID
	if req.ProductItemID != "" {
		text += "; product fix " + req.ProductItemID
	}
	return text + ": " + req.Text
}

func (c *Client) CreateIntervention(ctx context.Context, taskID string, req CreateInterventionRequest) (Message, error) {
	var out Message
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(taskID)+"/interventions", req, &out)
	return out, err
}

// ListInterventions reads one page plus the whole-project summary bucketed by
// local day in timeZone (an IANA name; empty means UTC).
func (c *Client) ListInterventions(ctx context.Context, taskID string, after int64, limit int, timeZone string) (InterventionList, error) {
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(limit)}}
	if timeZone != "" {
		q.Set("tz", timeZone)
	}
	var out InterventionList
	err := c.doLimited(ctx, "GET", "/v1/tasks/"+url.PathEscape(taskID)+"/interventions?"+q.Encode(), nil, &out, 16<<20)
	return out, err
}
