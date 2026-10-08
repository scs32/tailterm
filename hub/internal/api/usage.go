package api

import (
	"context"
	"net/url"
	"time"
)

const UsageVersion = 1

var UsageClasses = []string{"input", "cached", "cacheWrite", "output", "reasoning"}

// Absent classes are unavailable, not measured zero. Fully measured classes
// are disjoint. Partial Claude output can remain inclusive with unavailable
// reasoning, explicitly identified in Gap; never add missing reasoning as zero.
// Raw retains provider quantities before normalization, without transcript text.
type UsageTurn struct {
	ID           string           `json:"id"`
	Revision     int64            `json:"revision"`
	Runtime      string           `json:"runtime"`
	Session      string           `json:"session"`
	Model        string           `json:"model"`
	At           time.Time        `json:"at"`
	Tokens       map[string]int64 `json:"tokens"`
	Raw          map[string]int64 `json:"raw"`
	SourceDigest string           `json:"sourceDigest"`
	Activation   string           `json:"activation"`
	Complete     bool             `json:"complete"`
	Gap          string           `json:"gap,omitempty"`
	Handled      []UsageEvidence  `json:"handled,omitempty"`
}
type UsageEvidence struct {
	TaskID    string    `json:"taskId"`
	Seq       int64     `json:"seq"`
	Operation string    `json:"operation"`
	At        time.Time `json:"at"`
}

// UsageSpanSegment is the part of a turn that belongs to one metered model
// request. At is the timestamp the token ledger gives that request, so a
// request and the time in its segment are attributed alike. Poll marks a
// request that only re-checked the inbox.
type UsageSpanSegment struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	At   time.Time `json:"at"`
	Poll bool      `json:"poll,omitempty"`
}

// UsageSpan is one bounded chunk of a completed turn's time: half-open UTC
// intervals taken from transcript record timestamps. Tool, Wait (blocking
// inbox waits) and Mixed (a wait run together with other work, which no
// timestamp can split) are disjoint; the rest of the chunk is model time.
// Segments cover the chunk exactly. An unavailable chunk has a known wall
// time and no split. Command text never enters a span.
type UsageSpan struct {
	ID           string             `json:"id"`
	Turn         string             `json:"turn"`
	Chunk        int                `json:"chunk"`
	Last         bool               `json:"last,omitempty"`
	Start        time.Time          `json:"start"`
	End          time.Time          `json:"end"`
	Tool         [][2]time.Time     `json:"tool,omitempty"`
	Wait         [][2]time.Time     `json:"wait,omitempty"`
	Mixed        [][2]time.Time     `json:"mixed,omitempty"`
	Segments     []UsageSpanSegment `json:"segments"`
	Handled      []UsageEvidence    `json:"handled,omitempty"`
	Unavailable  bool               `json:"unavailable,omitempty"`
	Gap          string             `json:"gap,omitempty"`
	SourceDigest string             `json:"sourceDigest"`
}
type UsageBatch struct {
	Version   int         `json:"version"`
	RequestID string      `json:"requestId"`
	RunID     string      `json:"runId"`
	Session   string      `json:"session"`
	StartedAt time.Time   `json:"startedAt"`
	Coverage  string      `json:"coverage"`
	Turns     []UsageTurn `json:"turns"`
	// Spans are sent only to a hub that advertises usage.time: an older hub
	// would acknowledge the batch and drop them.
	Spans []UsageSpan `json:"spans,omitempty"`
}
type UsageReceipt struct {
	RequestID string `json:"requestId"`
	Turns     int    `json:"turns"`
	Spans     int    `json:"spans,omitempty"`
}
type UsageAttribution struct {
	TaskID      string `json:"taskId"`
	ItemID      string `json:"itemId,omitempty"`
	Denominator int64  `json:"denominator"`
	Reason      string `json:"reason"`
}
type UsageProjection struct {
	Turn        UsageTurn          `json:"turn"`
	AgentID     string             `json:"agentId"`
	RunID       string             `json:"runId"`
	Role        string             `json:"role"`
	RoleSource  string             `json:"roleSource"`
	Phase       string             `json:"phase"`
	PhaseReason string             `json:"phaseReason"`
	ReviewRound int                `json:"reviewRound,omitempty"`
	Shares      []UsageAttribution `json:"shares"`
	Evidence    []MessageReference `json:"evidence,omitempty"`
}

// Exact quantities use rational strings, e.g. "11/2". Null averages mean unknown.
type UsageSummary struct {
	State            string            `json:"state"`
	Requests         int               `json:"requests"`
	AllocatedTurns   string            `json:"allocatedTurns"`
	Tokens           map[string]string `json:"tokens"`
	MeasuredRequests map[string]int    `json:"measuredRequests"`
	AverageContext   *string           `json:"averageContext"`
	CachedShare      *string           `json:"cachedShare"`
	PricedSubtotal   map[string]string `json:"pricedSubtotal"`
	CostComplete     bool              `json:"costComplete"`
}
type UsageGroup struct {
	Key     string       `json:"key"`
	Label   string       `json:"label"`
	Summary UsageSummary `json:"summary"`
}
type UsageItemReport struct {
	TaskID     string       `json:"taskId"`
	ItemID     string       `json:"itemId,omitempty"`
	Title      string       `json:"title"`
	Summary    UsageSummary `json:"summary"`
	Phases     []UsageGroup `json:"phases"`
	Roles      []UsageGroup `json:"roles"`
	Models     []UsageGroup `json:"models"`
	PhaseRoles []UsageGroup `json:"phaseRoles"`
	// Time is absent when the item has no spans: its time was not measured.
	Time *UsageTime `json:"time,omitempty"`
	// Budget is the item's estimate against its lifetime actual. The From/To
	// filter never changes it. Project overhead has none.
	Budget *TokenBudget `json:"budget,omitempty"`
}

// UsageTime says where an item's wall time went. Every duration is exact
// milliseconds as a rational string, like token quantities: an integer, or
// "n/d" when a request served several items. The window runs from the
// earliest to the latest segment attributed to the item. For each agent,
// model + tool + waiting + unmeasured equals the window, so each of Phases,
// Roles and PhaseRoles sums to the same total as Agents.
type UsageTime struct {
	WallMs string    `json:"wallMs"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	// UnmeasuredMs is time with no model/tool/waiting split: a turn past the
	// interval bound, or a tool call that mixed an inbox wait with other work.
	UnmeasuredMs string           `json:"unmeasuredMs"`
	ModelMs      string           `json:"modelMs"`
	ToolMs       string           `json:"toolMs"`
	WaitingMs    string           `json:"waitingMs"`
	Agents       []UsageTimeAgent `json:"agents"`
	Phases       []UsageTimeSplit `json:"phases"`
	Roles        []UsageTimeSplit `json:"roles"`
	PhaseRoles   []UsageTimeSplit `json:"phaseRoles"`
	Timeline     UsageTimeline    `json:"timeline"`
	// Waits are the five longest waits by cause and awaited Board message.
	Waits  []UsageTimeWait `json:"waits"`
	Causes UsageTimeCauses `json:"causes"`
	// Polls counts model requests that only re-checked the inbox; PollMs is
	// the model time inside their segments.
	Polls  int    `json:"polls"`
	PollMs string `json:"pollMs"`
}
type UsageTimeAgent struct {
	AgentID      string `json:"agentId"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	ModelMs      string `json:"modelMs"`
	ToolMs       string `json:"toolMs"`
	WaitingMs    string `json:"waitingMs"`
	UnmeasuredMs string `json:"unmeasuredMs"`
	Polls        int    `json:"polls"`
	PollMs       string `json:"pollMs"`
}
type UsageTimeSplit struct {
	Key       string `json:"key"`
	ModelMs   string `json:"modelMs"`
	ToolMs    string `json:"toolMs"`
	WaitingMs string `json:"waitingMs"`
}

// UsageTimeline is the team's wall clock: some model working, only tools
// running, nobody active. With UnmeasuredMs the four sum to the window.
type UsageTimeline struct {
	ModelMs      string `json:"modelMs"`
	ToolsOnlyMs  string `json:"toolsOnlyMs"`
	IdleMs       string `json:"idleMs"`
	UnmeasuredMs string `json:"unmeasuredMs"`
}

// UsageTimeWait is waiting attributed to what was awaited. Cause is owner,
// handler, teammate or unknown; AwaitedBy names the agents that waited.
type UsageTimeWait struct {
	Cause      string `json:"cause"`
	MessageSeq int64  `json:"messageSeq,omitempty"`
	Subject    string `json:"subject,omitempty"`
	AwaitedBy  string `json:"awaitedBy"`
	Ms         string `json:"ms"`
}
type UsageTimeCauses struct {
	Owner    string `json:"owner"`
	Handler  string `json:"handler"`
	Teammate string `json:"teammate"`
	Unknown  string `json:"unknown"`
}
type UsageQuery struct {
	Item     string
	From, To time.Time
}
type UsageCoverage struct {
	AgentID   string `json:"agentId"`
	RunID     string `json:"runId"`
	StartedAt string `json:"startedAt"`
	State     string `json:"state"`
}
type UsageReport struct {
	Coverage      []UsageCoverage   `json:"coverage"`
	Version       int               `json:"version"`
	ProjectID     string            `json:"projectId"`
	From          *time.Time        `json:"from,omitempty"`
	To            *time.Time        `json:"to,omitempty"`
	PriceRevision int64             `json:"priceRevision"`
	Summary       UsageSummary      `json:"summary"`
	Items         []UsageItemReport `json:"items"`
	Overhead      UsageItemReport   `json:"overhead"`
	// TimeVersion is 1 when this hub reports time. It tells an item whose
	// time was not measured from a hub that does not report time at all.
	TimeVersion int `json:"timeVersion,omitempty"`
}
type UsagePrice struct {
	Runtime     string            `json:"runtime"`
	Model       string            `json:"model"`
	Currency    string            `json:"currency"`
	EffectiveAt time.Time         `json:"effectiveAt"`
	Rates       map[string]string `json:"rates"`
}
type UsagePrices struct {
	Revision int64        `json:"revision"`
	Rows     []UsagePrice `json:"rows"`
}
type UsagePriceRequest struct {
	RequestID        string       `json:"requestId"`
	ExpectedRevision int64        `json:"expectedRevision"`
	Rows             []UsagePrice `json:"rows"`
}

// DefaultUsageWarningThreshold is the multiple of an item's saved estimate
// past which the hub warns, unless the project sets its own.
const DefaultUsageWarningThreshold = "1.5"

// UsageWarningRequest sets a project's warning threshold: a decimal from 1 to
// 100 with at most three decimals. An agent caller names itself in AgentID
// and must be the project's owner helper.
type UsageWarningRequest struct {
	Threshold string `json:"threshold"`
	AgentID   string `json:"agentId,omitempty"`
}

// UsageWarning records the one warning an item got for one queue entry and
// one estimate value. Quantities are exact rational strings, as in
// TokenBudget. ActualTokens, ActualState and Ratio are the compared figure:
// with an EntryID, what that entry's team spent on the item, beside the
// item's lifetime figure at that moment; without one, a warning recorded
// before entries were compared, whose compared figure was the lifetime one. A
// message number is zero when that recipient was absent.
type UsageWarning struct {
	ItemID           string    `json:"itemId"`
	EntryID          string    `json:"entryId,omitempty"`
	EstimateTokens   int64     `json:"estimateTokens"`
	ActualTokens     string    `json:"actualTokens"`
	ActualState      string    `json:"actualState"`
	Ratio            string    `json:"ratio"`
	LifetimeTokens   string    `json:"lifetimeTokens,omitempty"`
	LifetimeState    string    `json:"lifetimeState,omitempty"`
	Threshold        string    `json:"threshold"`
	LeadAgent        string    `json:"leadAgent,omitempty"`
	LeadMessageSeq   int64     `json:"leadMessageSeq,omitempty"`
	HelperAgent      string    `json:"helperAgent,omitempty"`
	HelperMessageSeq int64     `json:"helperMessageSeq,omitempty"`
	At               time.Time `json:"at"`
}
type UsageWarnings struct {
	Threshold string         `json:"threshold"`
	Default   bool           `json:"default"`
	Warnings  []UsageWarning `json:"warnings"`
}

func (c *Client) ReportUsage(ctx context.Context, task, agent string, b UsageBatch) (UsageReceipt, error) {
	var out UsageReceipt
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/agents/"+url.PathEscape(agent)+"/usage", b, &out)
	return out, err
}
func (c *Client) Usage(ctx context.Context, task string, q UsageQuery) (UsageReport, error) {
	values := url.Values{}
	if q.Item != "" {
		values.Set("item", q.Item)
	}
	if !q.From.IsZero() {
		values.Set("from", q.From.UTC().Format(time.RFC3339Nano))
	}
	if !q.To.IsZero() {
		values.Set("to", q.To.UTC().Format(time.RFC3339Nano))
	}
	var out UsageReport
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/usage?"+values.Encode(), nil, &out)
	return out, err
}
func (c *Client) UsagePrices(ctx context.Context, task string) (UsagePrices, error) {
	var out UsagePrices
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/usage/prices", nil, &out)
	return out, err
}
func (c *Client) SetUsagePrices(ctx context.Context, task string, req UsagePriceRequest) (UsagePrices, error) {
	var out UsagePrices
	err := c.do(ctx, "PUT", "/v1/tasks/"+url.PathEscape(task)+"/usage/prices", req, &out)
	return out, err
}
func (c *Client) UsageWarnings(ctx context.Context, task string) (UsageWarnings, error) {
	var out UsageWarnings
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/usage/warning", nil, &out)
	return out, err
}
func (c *Client) SetUsageWarning(ctx context.Context, task string, req UsageWarningRequest) (UsageWarnings, error) {
	var out UsageWarnings
	err := c.do(ctx, "PUT", "/v1/tasks/"+url.PathEscape(task)+"/usage/warning", req, &out)
	return out, err
}

// Budget windows: the provider's rolling limits a budget row describes.
const (
	UsageWindowFiveHour = "five_hour"
	UsageWindowSevenDay = "seven_day"
)

// UsageWindowLength is the length of a budget window; zero for an unknown one.
func UsageWindowLength(window string) time.Duration {
	switch window {
	case UsageWindowFiveHour:
		return 5 * time.Hour
	case UsageWindowSevenDay:
		return 7 * 24 * time.Hour
	}
	return 0
}

// Budget sources, in the order admission tries them.
const (
	UsageBudgetSourceProvider  = "provider"
	UsageBudgetSourceAllowance = "allowance"
	UsageBudgetSourceNone      = "none"
)

// DefaultUsageBudgetStaleSeconds is how old a provider reading may be and
// still decide admission, unless the row sets its own bound.
const DefaultUsageBudgetStaleSeconds = 900

// UsageBudget is one owner-set budget row of a project: a runtime and one of
// its provider windows. AllowanceTokens is the tokens that 100 percent of the
// window represents; a provider percentage is converted with it and it is the
// fallback when no reading is usable. ReservePercent of the allowance is kept
// back from admission. ResetAt is an optional owner-entered reset instant,
// used only when no reading gives one. Status is computed on read for the
// host the read names.
type UsageBudget struct {
	Runtime         string             `json:"runtime"`
	Window          string             `json:"window"`
	AllowanceTokens int64              `json:"allowanceTokens"`
	ReservePercent  int                `json:"reservePercent"`
	ResetAt         string             `json:"resetAt,omitempty"`
	StaleSeconds    int                `json:"staleSeconds"`
	UpdatedAt       string             `json:"updatedAt,omitempty"`
	UpdatedBy       Sender             `json:"updatedBy"`
	Status          *UsageBudgetStatus `json:"status,omitempty"`
}

// UsageBudgetStatus is what admission would use for a budget row now: the
// source, the remaining tokens (an exact rational string) before the reserve
// is taken off, when the window resets, and, when the provider reading is not
// the source, why. Source "none" has no remaining figure and holds admission.
type UsageBudgetStatus struct {
	Host            string `json:"host,omitempty"`
	Source          string `json:"source"`
	RemainingTokens string `json:"remainingTokens,omitempty"`
	ResetAt         string `json:"resetAt,omitempty"`
	Reading         string `json:"reading,omitempty"`
}

// UsageBudgets is a project's budget rows. With none, admission ignores
// budgets altogether.
type UsageBudgets struct {
	Configured bool          `json:"configured"`
	Host       string        `json:"host,omitempty"`
	Budgets    []UsageBudget `json:"budgets"`
}

// UsageBudgetRequest sets one budget row, or with DELETE names the row to
// clear. ResetAt is RFC 3339 or empty; StaleSeconds 0 means the default. An
// agent caller names itself in AgentID and must be the project's owner helper
// or a database handler of the project.
type UsageBudgetRequest struct {
	Runtime         string `json:"runtime"`
	Window          string `json:"window"`
	AllowanceTokens int64  `json:"allowanceTokens,omitempty"`
	ReservePercent  int    `json:"reservePercent,omitempty"`
	ResetAt         string `json:"resetAt,omitempty"`
	StaleSeconds    int    `json:"staleSeconds,omitempty"`
	AgentID         string `json:"agentId,omitempty"`
}

// UsageEstimateDefaults is a project's lane default estimates: what admission
// compares with the budget for an item that has no saved estimate. They never
// warn and never pause a team. "Race" is the figure for an entry whose owned
// paths select the Go race check.
type UsageEstimateDefaults struct {
	Configured        bool   `json:"configured"`
	SmallTokens       int64  `json:"smallTokens"`
	SmallRaceTokens   int64  `json:"smallRaceTokens"`
	PlannedTokens     int64  `json:"plannedTokens"`
	PlannedRaceTokens int64  `json:"plannedRaceTokens"`
	UpdatedAt         string `json:"updatedAt,omitempty"`
	UpdatedBy         Sender `json:"updatedBy"`
}

// UsageEstimateDefaultsRequest sets all four lane defaults. AgentID is as in
// UsageBudgetRequest.
type UsageEstimateDefaultsRequest struct {
	SmallTokens       int64  `json:"smallTokens"`
	SmallRaceTokens   int64  `json:"smallRaceTokens"`
	PlannedTokens     int64  `json:"plannedTokens"`
	PlannedRaceTokens int64  `json:"plannedRaceTokens"`
	AgentID           string `json:"agentId,omitempty"`
}

// Provider reading states. Anything but ok carries no figure.
const (
	ProviderUsageOK         = "ok"
	ProviderUsageMissing    = "missing"
	ProviderUsageUnreadable = "unreadable"
	ProviderUsageMalformed  = "malformed"
)

// ProviderUsageWindow is one window of a provider reading: the percentage of
// the window used, 0 to 100, and when the window resets.
type ProviderUsageWindow struct {
	Window      string  `json:"window"`
	UsedPercent float64 `json:"usedPercent"`
	ResetsAt    string  `json:"resetsAt"`
}

// ProviderUsageReport is what a host's relay reports from the runtime's own
// usage capture. A state other than ok is an invalidation: the hub then keeps
// no figure of an earlier reading for that host and runtime.
type ProviderUsageReport struct {
	Host       string                `json:"host"`
	Runtime    string                `json:"runtime"`
	State      string                `json:"state"`
	CapturedAt string                `json:"capturedAt,omitempty"`
	Version    string                `json:"version,omitempty"`
	Windows    []ProviderUsageWindow `json:"windows,omitempty"`
}

// ProviderReading is one stored window of a host's provider reading.
type ProviderReading struct {
	Runtime     string `json:"runtime"`
	Window      string `json:"window"`
	State       string `json:"state"`
	UsedPercent string `json:"usedPercent,omitempty"`
	ResetsAt    string `json:"resetsAt,omitempty"`
	CapturedAt  string `json:"capturedAt,omitempty"`
	ReportedAt  string `json:"reportedAt"`
	Version     string `json:"version,omitempty"`
}

// ProviderUsage is every stored reading of one host.
type ProviderUsage struct {
	Host     string            `json:"host"`
	Readings []ProviderReading `json:"readings"`
}

// Budget hold states. A held team starts no new turns until the owner helper
// continues it or stops it by failing its queue entry.
const (
	BudgetHoldHeld      = "held"
	BudgetHoldContinued = "continued"
	BudgetHoldStopped   = "stopped"
)

// BudgetHoldMultiple is how many times its saved estimate a running entry's
// team may spend before it is held. The comparison is strictly greater.
const BudgetHoldMultiple = 3

// BudgetHoldRun is one exact run of a held team.
type BudgetHoldRun struct {
	AgentID string `json:"agentId"`
	RunID   string `json:"runId"`
}

// BudgetHold records that a running queue entry's team passed
// BudgetHoldMultiple times the item's saved estimate. There is one per entry
// and estimate value. TeamTokens is the exact rational figure compared. Runs
// lists the team's runs while the relay must not wake them: while the hold is
// held, and while it is stopped and the entry is failed and unreleased.
type BudgetHold struct {
	EntryID        string          `json:"entryId"`
	ItemID         string          `json:"itemId"`
	EstimateTokens int64           `json:"estimateTokens"`
	TeamTokens     string          `json:"teamTokens"`
	TeamState      string          `json:"teamState"`
	State          string          `json:"state"`
	AskAgent       string          `json:"askAgent,omitempty"`
	AskSeq         int64           `json:"askSeq,omitempty"`
	CreatedAt      string          `json:"createdAt"`
	ResolvedAt     string          `json:"resolvedAt,omitempty"`
	ResolvedBy     *Sender         `json:"resolvedBy,omitempty"`
	Runs           []BudgetHoldRun `json:"runs,omitempty"`
}

// UsageHolds is a project's budget holds that still keep runs from waking,
// and Held whether there is any. With an agent and run named in the read,
// Held says whether that exact run is one of the held runs, and Holds carries
// only its hold.
type UsageHolds struct {
	Held  bool         `json:"held"`
	Holds []BudgetHold `json:"holds"`
}

func (c *Client) UsageBudgets(ctx context.Context, task, host string) (UsageBudgets, error) {
	var out UsageBudgets
	query := ""
	if host != "" {
		query = "?host=" + url.QueryEscape(host)
	}
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/usage/budget"+query, nil, &out)
	return out, err
}
func (c *Client) SetUsageBudget(ctx context.Context, task string, req UsageBudgetRequest) (UsageBudgets, error) {
	var out UsageBudgets
	err := c.do(ctx, "PUT", "/v1/tasks/"+url.PathEscape(task)+"/usage/budget", req, &out)
	return out, err
}
func (c *Client) DeleteUsageBudget(ctx context.Context, task string, req UsageBudgetRequest) (UsageBudgets, error) {
	var out UsageBudgets
	err := c.do(ctx, "DELETE", "/v1/tasks/"+url.PathEscape(task)+"/usage/budget", req, &out)
	return out, err
}
func (c *Client) UsageEstimateDefaults(ctx context.Context, task string) (UsageEstimateDefaults, error) {
	var out UsageEstimateDefaults
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/usage/defaults", nil, &out)
	return out, err
}
func (c *Client) SetUsageEstimateDefaults(ctx context.Context, task string, req UsageEstimateDefaultsRequest) (UsageEstimateDefaults, error) {
	var out UsageEstimateDefaults
	err := c.do(ctx, "PUT", "/v1/tasks/"+url.PathEscape(task)+"/usage/defaults", req, &out)
	return out, err
}

// UsageHolds reads the project's holds. With agent and run it asks only
// whether that exact run is held. A hub without holds answers 404, which is
// returned as it is.
func (c *Client) UsageHolds(ctx context.Context, task, agent, run string) (UsageHolds, error) {
	var out UsageHolds
	query := ""
	if agent != "" || run != "" {
		query = "?" + url.Values{"agent": {agent}, "run": {run}}.Encode()
	}
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/usage/holds"+query, nil, &out)
	return out, err
}
func (c *Client) ReportProviderUsage(ctx context.Context, report ProviderUsageReport) (ProviderUsage, error) {
	var out ProviderUsage
	err := c.do(ctx, "PUT", "/v1/provider-usage", report, &out)
	return out, err
}
func (c *Client) ProviderUsage(ctx context.Context, host string) (ProviderUsage, error) {
	var out ProviderUsage
	err := c.do(ctx, "GET", "/v1/provider-usage?host="+url.QueryEscape(host), nil, &out)
	return out, err
}

// NormalizeUsageTokens version 1 preserves missing class availability.
func NormalizeUsageTokens(runtime string, raw map[string]int64) (map[string]int64, string) {
	out := map[string]int64{}
	gap := ""
	names := map[string]string{"input": "input_tokens", "cached": "cached_input_tokens", "cacheWrite": "cache_write_input_tokens", "output": "output_tokens", "reasoning": "reasoning_output_tokens"}
	if runtime == "claude" {
		names["cached"] = "cache_read_input_tokens"
		names["cacheWrite"] = "cache_creation_input_tokens"
		names["reasoning"] = "output_tokens_details.thinking_tokens"
	}
	for class, name := range names {
		if n, ok := raw[name]; ok {
			out[class] = n
		}
	}
	if runtime == "codex" {
		if input, ok := out["input"]; ok {
			cached, cok := out["cached"]
			if !cok {
				delete(out, "input")
				gap = "cached subset unavailable"
			} else if cached > input {
				delete(out, "input")
				gap = "cached exceeds inclusive input"
			} else if write, known := out["cacheWrite"]; known && write > 0 {
				// The local format reports this class but does not establish its
				// overlap with input. Preserve raw quantities; do not guess a split.
				delete(out, "input")
				gap = "nonzero cache-write overlap with input unavailable"
			} else {
				out["input"] = input - cached
			}
		}
	}
	if runtime == "codex" || runtime == "claude" {
		// Both runtime formats report thinking/reasoning inside generated output.
		// Preserve inclusive output in Raw; charge disjoint output.
		if output, ok := out["output"]; ok {
			reasoning, rok := out["reasoning"]
			if !rok {
				if runtime == "claude" {
					gap = "reasoning unavailable; output retained inclusive"
				} else {
					delete(out, "output")
					gap = "reasoning subset unavailable"
				}
			} else if reasoning > output {
				delete(out, "output")
				gap = "reasoning exceeds inclusive output"
			} else {
				out["output"] = output - reasoning
			}
		}
	}
	return out, gap
}
