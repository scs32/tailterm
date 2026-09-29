package api

// Owner delegation windows (docs/owner-delegation-windows.md): for a set
// time, the owner's decision requests in one project go to a named delegate
// agent, which answers with a required rationale. Matrix approvals never leave
// the owner.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Window scopes.
const (
	DelegationScopeDecisions              = "decisions"
	DelegationScopeDecisionsMergesDeploys = "decisions_merges_deploys"
)

// Window states.
const (
	DelegationOpen    = "open"
	DelegationExpired = "expired"
	DelegationClosed  = "closed"
)

// Request categories. An owner request declares one in refs.category, a
// decision in its category field; both default to decision.
const (
	RequestCategoryDecision = "decision"
	RequestCategoryMerge    = "merge"
	RequestCategoryDeploy   = "deploy"
	RequestCategoryMatrix   = "matrix"
)

// Route kinds: an owner request (obligation) or a Board decision (tt ask).
const (
	DelegationRouteObligation = "obligation"
	DelegationRouteDecision   = "decision"
)

// Where a window was opened or closed.
const (
	DelegationSourceTailOS  = "tailos"
	DelegationSourceTT      = "tt"
	DelegationSourceDiscord = "discord"
)

// MatrixApprovalToken marks a verification matrix approval. Any request that
// mentions it is a matrix approval, whatever category it declares.
const MatrixApprovalToken = "verification-matrix-approval:"

// Window length limits and the delegate rationale limit (bytes).
const (
	MinDelegationWindow    = time.Minute
	MaxDelegationWindow    = 7 * 24 * time.Hour
	MaxDelegationRationale = 2000
)

// ErrDelegationForbidden refuses a delegated answer from an agent that holds
// no open window for the request (HTTP 403).
var ErrDelegationForbidden = errors.New("forbidden")

// DelegationSource records the surface that opened or closed a window. A
// Discord source also carries the interaction and user IDs.
type DelegationSource struct {
	Kind   string `json:"kind"`
	ID     string `json:"id,omitempty"`
	UserID string `json:"userId,omitempty"`
}

// OpenDelegationWindowRequest opens a window. Delegate is an agent ID or name
// in the same project.
type OpenDelegationWindowRequest struct {
	Delegate  string            `json:"delegate"`
	EndsAt    time.Time         `json:"endsAt"`
	Scope     string            `json:"scope"`
	Reason    string            `json:"reason,omitempty"`
	Source    *DelegationSource `json:"source,omitempty"`
	RequestID string            `json:"requestId"`
}

// CloseDelegationWindowRequest ends an open window early.
type CloseDelegationWindowRequest struct {
	Reason    string            `json:"reason"`
	Source    *DelegationSource `json:"source,omitempty"`
	RequestID string            `json:"requestId"`
}

// DelegationRoute is one request routed to a window's delegate, and its
// delegated answer when there is one.
type DelegationRoute struct {
	WindowID               string     `json:"windowId"`
	RequestKind            string     `json:"requestKind"`
	RequestSeq             int64      `json:"requestSeq"`
	ObligationID           string     `json:"obligationId,omitempty"`
	Category               string     `json:"category"`
	Subject                string     `json:"subject,omitempty"`
	NoticeSeq              int64      `json:"noticeSeq,omitempty"`
	RoutedAt               time.Time  `json:"routedAt"`
	ReturnedAt             *time.Time `json:"returnedAt,omitempty"`
	AnswerSeq              int64      `json:"answerSeq,omitempty"`
	Answer                 string     `json:"answer,omitempty"`
	AnsweredByAgentID      string     `json:"answeredByAgentId,omitempty"`
	AnsweredByName         string     `json:"answeredByName,omitempty"`
	AnsweredByRunID        string     `json:"answeredByRunId,omitempty"`
	Rationale              string     `json:"rationale,omitempty"`
	FollowedRecommendation *bool      `json:"followedRecommendation,omitempty"`
	AnsweredAt             *time.Time `json:"answeredAt,omitempty"`
}

// DelegationWindow is one window and everything routed under it.
type DelegationWindow struct {
	ID              string            `json:"id"`
	TaskID          string            `json:"taskId"`
	DelegateAgentID string            `json:"delegateAgentId"`
	DelegateName    string            `json:"delegateName"`
	Scope           string            `json:"scope"`
	EndsAt          time.Time         `json:"endsAt"`
	Reason          string            `json:"reason,omitempty"`
	State           string            `json:"state"`
	EndedAt         *time.Time        `json:"endedAt,omitempty"`
	EndReason       string            `json:"endReason,omitempty"`
	OpenedBy        Sender            `json:"openedBy"`
	OpenedSource    *DelegationSource `json:"openedSource,omitempty"`
	ClosedSource    *DelegationSource `json:"closedSource,omitempty"`
	CreatedAt       time.Time         `json:"createdAt"`
	Routes          []DelegationRoute `json:"routes"`
}

// Active reports whether the window can still route and accept answers at now.
func (w DelegationWindow) Active(now time.Time) bool {
	return w.State == DelegationOpen && now.Before(w.EndsAt)
}

type DelegationWindowList struct {
	Windows []DelegationWindow `json:"windows"`
}

// ValidDelegationScope accepts the two window scopes.
func ValidDelegationScope(scope string) bool {
	return scope == DelegationScopeDecisions || scope == DelegationScopeDecisionsMergesDeploys
}

// NormalizeDelegationScope accepts the CLI's hyphenated spelling too.
func NormalizeDelegationScope(scope string) string {
	return strings.ReplaceAll(strings.TrimSpace(scope), "-", "_")
}

// ValidRequestCategory accepts the declared request categories.
func ValidRequestCategory(category string) bool {
	switch category {
	case RequestCategoryDecision, RequestCategoryMerge, RequestCategoryDeploy, RequestCategoryMatrix:
		return true
	}
	return false
}

// DelegationScopeCovers reports whether a window of scope may take a request
// of category. Matrix approvals are never covered.
func DelegationScopeCovers(scope, category string) bool {
	switch category {
	case RequestCategoryDecision:
		return ValidDelegationScope(scope)
	case RequestCategoryMerge, RequestCategoryDeploy:
		return scope == DelegationScopeDecisionsMergesDeploys
	}
	return false
}

// MentionsMatrixApproval reports whether any value carries the matrix
// approval token. A delegate may never write it.
func MentionsMatrixApproval(values ...string) bool { return mentionsMatrixApproval(values...) }

func mentionsMatrixApproval(values ...string) bool {
	for _, v := range values {
		if strings.Contains(v, MatrixApprovalToken) {
			return true
		}
	}
	return false
}

// OwnerRequestCategory classifies a typed owner request. Anything mentioning
// a matrix approval is matrix; otherwise refs.category, default decision. An
// unknown declared category is returned as is, so no scope covers it.
func OwnerRequestCategory(e *Envelope) string {
	if e == nil {
		return RequestCategoryDecision
	}
	matrix := false
	forEachText(*e, func(_, value string) { matrix = matrix || mentionsMatrixApproval(value) })
	declared := strings.TrimSpace(e.Refs["category"])
	if matrix || declared == RequestCategoryMatrix {
		return RequestCategoryMatrix
	}
	if declared == "" {
		return RequestCategoryDecision
	}
	return declared
}

// DecisionCategory classifies a Board decision the same way.
func DecisionCategory(req DecisionRequest) string {
	values := []string{req.Question, req.RecommendationReason}
	for _, o := range req.Options {
		values = append(values, o.ID, o.Label, o.Description)
	}
	if mentionsMatrixApproval(values...) || req.Category == RequestCategoryMatrix {
		return RequestCategoryMatrix
	}
	if req.Category == "" {
		return RequestCategoryDecision
	}
	return req.Category
}

// ValidateOpenDelegationWindow checks everything but the delegate, which the
// store resolves. now is the hub clock.
func ValidateOpenDelegationWindow(req OpenDelegationWindowRequest, now time.Time) error {
	if strings.TrimSpace(req.Delegate) == "" || !ValidText(req.Delegate, 200) {
		return fmt.Errorf("%w: name the delegate agent", ErrInvalid)
	}
	if !ValidDelegationScope(req.Scope) {
		return fmt.Errorf("%w: scope must be %s or %s", ErrInvalid, DelegationScopeDecisions, DelegationScopeDecisionsMergesDeploys)
	}
	if req.EndsAt.IsZero() || req.EndsAt.Before(now.Add(MinDelegationWindow)) || req.EndsAt.After(now.Add(MaxDelegationWindow)) {
		return fmt.Errorf("%w: the window must end between 1 minute and 7 days from now", ErrInvalid)
	}
	if !ValidText(req.Reason, 500) {
		return fmt.Errorf("%w: reason is at most 500 bytes", ErrInvalid)
	}
	return ValidateDelegationSource(req.Source)
}

// ValidateDelegationSource accepts tailos, tt or a Discord interaction.
func ValidateDelegationSource(src *DelegationSource) error {
	if src == nil {
		return nil
	}
	switch src.Kind {
	case DelegationSourceTailOS, DelegationSourceTT:
		if src.ID == "" && src.UserID == "" {
			return nil
		}
	case DelegationSourceDiscord:
		if ValidMessageSource(MessageSource{Kind: SourceDiscord, ID: src.ID, UserID: src.UserID}) {
			return nil
		}
	}
	return fmt.Errorf("%w: source must be tailos, tt or a Discord interaction", ErrInvalid)
}

// ValidDelegationRationale requires a nonblank rationale within its limit.
func ValidDelegationRationale(rationale string) bool {
	return strings.TrimSpace(rationale) != "" && ValidText(rationale, MaxDelegationRationale)
}

func (c *Client) OpenDelegationWindow(ctx context.Context, task string, req OpenDelegationWindowRequest) (OwnerActionResult, error) {
	return c.ownerAction(ctx, "/v1/tasks/"+url.PathEscape(task)+"/delegation-windows", req)
}

func (c *Client) CloseDelegationWindow(ctx context.Context, task, window string, req CloseDelegationWindowRequest) (OwnerActionResult, error) {
	return c.ownerAction(ctx, "/v1/tasks/"+url.PathEscape(task)+"/delegation-windows/"+url.PathEscape(window)+"/close", req)
}

func (c *Client) ListDelegationWindows(ctx context.Context, task string) (DelegationWindowList, error) {
	var out DelegationWindowList
	err := c.doLimited(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/delegation-windows", nil, &out, 16<<20)
	return out, err
}
