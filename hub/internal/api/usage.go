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
