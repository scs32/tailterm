package api

import (
	"context"
	"net/url"
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
