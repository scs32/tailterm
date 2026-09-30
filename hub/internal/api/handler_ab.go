package api

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// Handler arms (docs/handler-ab.md) compare database handler models. An
// enabled project policy lists the arms; each queued team entry draws one
// arm reproducibly from the policy seed and its entry ID and leases a free
// handler of that arm. The report compares the arms per finished item.
const (
	HandlerArmRefusedAgentCaller       = "agent_caller"
	HandlerArmRefusedStalePolicy       = "stale_revision"
	HandlerArmRefusedTemplate          = "template_mismatch"
	HandlerArmRefusedRequestReused     = "request_reused"
	HandlerRotationRefusedArmChanged   = "arm_changed"
	HandlerArmFallbackBusy             = "busy"
	HandlerArmClearCodexPromptGone     = "prompt_gone"
	HandlerArmClearRunClosed           = "run_closed"
	HandlerArmClearHoldExpiredClean    = "clean_turn"
	HandlerArmClearHoldExpiredNoReport = "hold_expired"
	HandlerArmSourceCodexUsageLimit    = "codex_usage_limit"
	HandlerArmSourceClaudeRateLimit    = "claude_rate_limit"

	// ClaudeRateLimitReason is the activity reason the relay reports when a
	// Claude turn ends with the rate_limit API error.
	ClaudeRateLimitReason = "turn ended by API error (rate_limit)"

	// Every arm-wait conflict ends in this suffix so the runner treats it as
	// an ordinary wait rather than a failure.
	HandlerArmWaitSuffix = "no free handler in the drawn arm"

	DefaultHandlerArmLimitHoldMinutes = 60
	HandlerArmNoiseMinItems           = 5
)

// HandlerArmRefusal is a 409 with a named reason. A refused change writes
// nothing.
type HandlerArmRefusal struct {
	Code   string
	Detail string
}

func (e *HandlerArmRefusal) Error() string {
	return fmt.Sprintf("handler arm policy refused (%s): %s", e.Code, e.Detail)
}

func (e *HandlerArmRefusal) Unwrap() error { return ErrConflict }

// HandlerArm is one arm: a handler run matches it when its agent runtime and
// the model and reasoning recorded at spawn are all equal.
type HandlerArm struct {
	ID        string `json:"id"`
	Runtime   string `json:"runtime"`
	Model     string `json:"model"`
	Reasoning string `json:"reasoning"`
	Weight    int    `json:"weight"`
}

// HandlerArmPolicy is a project's arm policy. Revision 0 means none is saved,
// which, like Enabled=false, keeps the ordinary handler lease.
type HandlerArmPolicy struct {
	TaskID           string       `json:"taskId"`
	Enabled          bool         `json:"enabled"`
	Seed             string       `json:"seed"`
	Fallback         bool         `json:"fallback"`
	LimitHoldMinutes int          `json:"limitHoldMinutes"`
	TemplateDigest   string       `json:"templateDigest"`
	Arms             []HandlerArm `json:"arms"`
	Revision         int64        `json:"revision"`
	UpdatedAt        *time.Time   `json:"updatedAt,omitempty"`
}

type HandlerArmPolicyRequest struct {
	RequestID        string       `json:"requestId"`
	ExpectedRevision int64        `json:"expectedRevision"`
	Enabled          bool         `json:"enabled"`
	Seed             string       `json:"seed"`
	Fallback         bool         `json:"fallback"`
	LimitHoldMinutes int          `json:"limitHoldMinutes"`
	TemplateDigest   string       `json:"templateDigest"`
	Arms             []HandlerArm `json:"arms"`
	ActorAgentID     string       `json:"actorAgentId,omitempty"`
}

// HandlerArmHandler is one open database handler as the policy sees it.
type HandlerArmHandler struct {
	AgentID        string `json:"agentId"`
	Name           string `json:"name"`
	RunID          string `json:"runId"`
	Status         string `json:"status"`
	Online         bool   `json:"online"`
	Runtime        string `json:"runtime"`
	Model          string `json:"model"`
	Reasoning      string `json:"reasoning"`
	TemplateDigest string `json:"templateDigest"`
	DigestMatches  bool   `json:"digestMatches"`
	Arm            string `json:"arm,omitempty"`
	NoArmReason    string `json:"noArmReason,omitempty"`
}

// HandlerArmLimit is one provider-limit episode. An arm is limited exactly
// while it has an open episode.
type HandlerArmLimit struct {
	ID             string     `json:"id"`
	Arm            string     `json:"arm"`
	AgentID        string     `json:"agentId"`
	RunID          string     `json:"runId"`
	Source         string     `json:"source"`
	StartedAt      time.Time  `json:"startedAt"`
	LastSignalAt   time.Time  `json:"lastSignalAt"`
	HoldUntil      *time.Time `json:"holdUntil,omitempty"`
	ClearedAt      *time.Time `json:"clearedAt,omitempty"`
	ClearReason    string     `json:"clearReason,omitempty"`
	StartNoticeSeq int64      `json:"startNoticeSeq"`
	ClearNoticeSeq int64      `json:"clearNoticeSeq,omitempty"`
}

type HandlerArmPolicyView struct {
	Policy   HandlerArmPolicy    `json:"policy"`
	Handlers []HandlerArmHandler `json:"handlers"`
	Limits   []HandlerArmLimit   `json:"limits"`
}

// TeamQueueHandlerArm is the arm assignment of an entry's current lease.
type TeamQueueHandlerArm struct {
	LeaseGeneration int64    `json:"leaseGeneration"`
	PolicyRevision  int64    `json:"policyRevision"`
	Draw            string   `json:"draw"`
	DrawnArm        string   `json:"drawnArm"`
	Arm             string   `json:"arm"`
	Fallback        bool     `json:"fallback"`
	FallbackReason  string   `json:"fallbackReason"`
	SkippedLimited  []string `json:"skippedLimited"`
	HandlerID       string   `json:"handlerId"`
	HandlerRunID    string   `json:"handlerRunId"`
	HandlerDigest   string   `json:"handlerDigest"`
	PolicyDigest    string   `json:"policyDigest"`
	LeasedAt        string   `json:"leasedAt"`
	FinishedAt      string   `json:"finishedAt"`
}

// HandlerWriteRefusal is one refused work-item write by a database handler.
// Only a fixed code is kept, never request text.
type HandlerWriteRefusal struct {
	ID        string    `json:"id"`
	ItemID    string    `json:"itemId"`
	AgentID   string    `json:"agentId"`
	RunID     string    `json:"runId"`
	Route     string    `json:"route"`
	Status    int       `json:"status"`
	Code      string    `json:"code"`
	RequestID string    `json:"requestId,omitempty"`
	At        time.Time `json:"at"`
}

// HandlerABItem is one finished, assigned entry in the report.
type HandlerABItem struct {
	EntryID               string           `json:"entryId"`
	ItemID                string           `json:"itemId"`
	Title                 string           `json:"title"`
	Arm                   string           `json:"arm"`
	DrawnArm              string           `json:"drawnArm"`
	Fallback              bool             `json:"fallback"`
	HandlerID             string           `json:"handlerId"`
	HandlerRunID          string           `json:"handlerRunId"`
	HandlerDigest         string           `json:"handlerDigest"`
	DigestFlags           []string         `json:"digestFlags"`
	LeasedAt              string           `json:"leasedAt"`
	FinishedAt            string           `json:"finishedAt"`
	HandlerTokens         int64            `json:"handlerTokens"`
	HandlerRequests       int              `json:"handlerRequests"`
	HandlerInputTokens    int64            `json:"handlerInputTokens"`
	HandlerAllocatedTurns string           `json:"handlerAllocatedTurns"`
	HandlerUsage          *UsageSummary    `json:"handlerUsage,omitempty"`
	ResponseMillis        []int64          `json:"responseMillis"`
	LaunchToDoneMillis    *int64           `json:"launchToDoneMillis"`
	EntryFinishedMillis   *int64           `json:"entryFinishedMillis"`
	HandlerBlocks         int              `json:"handlerBlocks"`
	HandlerAuthoredBlocks int              `json:"handlerAuthoredBlocks"`
	RefusedSaves          int              `json:"refusedSaves"`
	IncorrectSaves        int              `json:"incorrectSaves"`
	OwnerCorrections      int              `json:"ownerCorrections"`
	GateFixes             int              `json:"gateFixes"`
	LinkCorrections       int              `json:"linkCorrections"`
	Interventions         map[string]int   `json:"interventions"`
	LimitEvents           HandlerABLimitIn `json:"limitEvents"`
}

type HandlerABLimitIn struct {
	Episodes        int `json:"episodes"`
	CodexUsageLimit int `json:"codexUsageLimit"`
	CodexRateLimit  int `json:"codexRateLimitSwitch"`
	Total           int `json:"total"`
}

// HandlerABCount is a count total with its per-item rate and a noise flag.
type HandlerABCount struct {
	Total int    `json:"total"`
	Rate  string `json:"rate"`
}

type HandlerABArm struct {
	Arm                  string                    `json:"arm"`
	Runtime              string                    `json:"runtime,omitempty"`
	Model                string                    `json:"model,omitempty"`
	Reasoning            string                    `json:"reasoning,omitempty"`
	N                    int                       `json:"n"`
	Fallbacks            int                       `json:"fallbacks"`
	Requests             int                       `json:"requests"`
	ResponseMedianMillis *int64                    `json:"responseMedianMillis"`
	ResponseP90Millis    *int64                    `json:"responseP90Millis"`
	LaunchToDoneMedian   *int64                    `json:"launchToDoneMedianMillis"`
	LaunchToDoneDone     int                       `json:"launchToDoneItems"`
	HandlerTokensMedian  *int64                    `json:"handlerTokensMedian"`
	MeanInputPerRequest  *string                   `json:"meanInputTokensPerRequest"`
	Rotations            int                       `json:"rotations"`
	Counts               map[string]HandlerABCount `json:"counts"`
	DigestFlaggedItems   int                       `json:"digestFlaggedItems"`
}

// HandlerABComparison flags one metric's difference between two arms:
// insufficient, within_noise or difference. It is a heuristic, not a
// significance test.
type HandlerABComparison struct {
	Metric string `json:"metric"`
	A      string `json:"a"`
	B      string `json:"b"`
	Flag   string `json:"flag"`
}

type HandlerABReport struct {
	TaskID      string                `json:"taskId"`
	Policy      HandlerArmPolicy      `json:"policy"`
	GeneratedAt time.Time             `json:"generatedAt"`
	Arms        []HandlerABArm        `json:"arms"`
	Items       []HandlerABItem       `json:"items"`
	Comparisons []HandlerABComparison `json:"comparisons"`
	Limits      []HandlerArmLimit     `json:"limits"`
}

func (c *Client) HandlerArmPolicy(ctx context.Context, task string) (HandlerArmPolicyView, error) {
	var out HandlerArmPolicyView
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/handler-ab/policy", nil, &out)
}

func (c *Client) SetHandlerArmPolicy(ctx context.Context, task string, req HandlerArmPolicyRequest) (HandlerArmPolicy, error) {
	var out HandlerArmPolicy
	return out, c.do(ctx, "PUT", "/v1/tasks/"+url.PathEscape(task)+"/handler-ab/policy", req, &out)
}

func (c *Client) HandlerABReport(ctx context.Context, task string) (HandlerABReport, error) {
	var out HandlerABReport
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/handler-ab/report", nil, &out)
}
