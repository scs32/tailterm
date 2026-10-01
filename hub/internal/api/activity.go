package api

import (
	"context"
	"net/url"
	"regexp"
	"time"
)

// TokenTotals are cumulative for one runtime session. They are snapshots,
// never increments to add to a previous report.
type TokenTotals struct {
	Input      int64 `json:"input"`
	Cached     int64 `json:"cached"`
	CacheWrite int64 `json:"cacheWrite"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	Total      int64 `json:"total"`
}

type AgentActivity struct {
	State        string       `json:"state"`
	ObservedAt   time.Time    `json:"observedAt"`
	LastEventAt  time.Time    `json:"lastEventAt,omitempty"`
	PendingTool  string       `json:"pendingTool,omitempty"`
	PendingSince time.Time    `json:"pendingSince,omitempty"`
	Tokens       TokenTotals  `json:"tokens"`
	Reason       string       `json:"reason,omitempty"`
	Wake         *WakeOutcome `json:"wake,omitempty"`
	// Prompt is set only with state runtime_prompt: the runtime is waiting on
	// its own modal prompt rather than at its input.
	Prompt *RuntimePrompt `json:"prompt,omitempty"`
	// Provider is set only with state provider_blocked: the provider refused
	// the agent's requests (docs/provider-blocked.md).
	Provider *ProviderBlock `json:"provider,omitempty"`
}

// Provider block classes. Class, code and status together are the exact error
// class; no provider or transcript text is ever carried.
const (
	ProviderBlockUsageLimit  = "usage_limit"
	ProviderBlockAuth        = "auth"
	ProviderBlockRateLimited = "rate_limited"
	ProviderBlockServerError = "server_error"
)

// ProviderBlock describes a provider failure that stops an agent's turns.
type ProviderBlock struct {
	Provider string    `json:"provider"`         // anthropic | openai
	Runtime  string    `json:"runtime"`          // claude | codex
	Model    string    `json:"model"`            // or "unknown"
	Class    string    `json:"class"`            // usage_limit | auth | rate_limited | server_error
	Code     string    `json:"code,omitempty"`   // the runtime's short code
	Status   int       `json:"status,omitempty"` // 0 or 400..599
	Since    time.Time `json:"since"`
}

var (
	providerBlockModel = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	providerBlockCode  = regexp.MustCompile(`^[a-z_]{1,40}$`)
)

// ProviderForRuntime names the provider a runtime calls, or "".
func ProviderForRuntime(runtime string) string {
	switch runtime {
	case "claude":
		return "anthropic"
	case "codex":
		return "openai"
	}
	return ""
}

// Valid reports whether every field is an allowed enum, pattern or range.
func (p *ProviderBlock) Valid() bool {
	if p == nil || p.Provider == "" || p.Provider != ProviderForRuntime(p.Runtime) {
		return false
	}
	switch p.Class {
	case ProviderBlockUsageLimit, ProviderBlockAuth, ProviderBlockRateLimited, ProviderBlockServerError:
	default:
		return false
	}
	if !providerBlockModel.MatchString(p.Model) || (p.Code != "" && !providerBlockCode.MatchString(p.Code)) {
		return false
	}
	return (p.Status == 0 || (p.Status >= 400 && p.Status <= 599)) && !p.Since.IsZero()
}

// SameProviderBlock compares the parts of a block that make a new report.
func SameProviderBlock(a, b *ProviderBlock) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Provider == b.Provider && a.Runtime == b.Runtime && a.Model == b.Model && a.Class == b.Class && a.Code == b.Code && a.Status == b.Status && a.Since.Equal(b.Since)
}

// WakeOutcome is independent of execution state. A safe skip can be visible
// while the agent remains idle, and a confirmed delivery does not imply work.
type WakeOutcome struct {
	Status      string    `json:"status"`
	Reason      string    `json:"reason,omitempty"`
	MessageSeqs []int64   `json:"messageSeqs,omitempty"`
	At          time.Time `json:"at"`
}

type ActivityReport struct {
	RequestID string        `json:"requestId"`
	RunID     string        `json:"runId"`
	Activity  AgentActivity `json:"activity"`
}

func (c *Client) ReportActivity(ctx context.Context, task, agent string, report ActivityReport) (AgentActivity, error) {
	var out AgentActivity
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/agents/"+url.PathEscape(agent)+"/activity", report, &out)
	return out, err
}
