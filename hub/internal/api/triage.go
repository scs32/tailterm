package api

import (
	"context"
	"net/url"
	"strconv"
)

// WorkItemTriage lists backlog suggestions for the owner to confirm. It is
// read-only: nothing is changed by computing or reading it.
type WorkItemTriage struct {
	TaskID          string              `json:"taskId"`
	StaleDays       int                 `json:"staleDays"`
	GeneratedAt     string              `json:"generatedAt"`
	Duplicates      []TriageDuplicate   `json:"duplicates"`
	AlreadyReleased []TriageAlreadyDone `json:"alreadyReleased"`
	Stale           []TriageStale       `json:"stale"`
}

type TriageItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// TriageDuplicate is a pair of open items with similar titles or criteria.
type TriageDuplicate struct {
	Items              []TriageItem `json:"items"`
	TitleSimilarity    float64      `json:"titleSimilarity"`
	CriteriaSimilarity float64      `json:"criteriaSimilarity"`
}

// TriageAlreadyDone is an open item that a done item, or a release-record
// row, appears to have delivered already.
type TriageAlreadyDone struct {
	Item               TriageItem  `json:"item"`
	Done               *TriageItem `json:"done,omitempty"`
	Release            string      `json:"release,omitempty"`
	TitleSimilarity    float64     `json:"titleSimilarity,omitempty"`
	CriteriaSimilarity float64     `json:"criteriaSimilarity,omitempty"`
	NamedInRelease     bool        `json:"namedInRelease,omitempty"`
}

// TriageStale is an open item with no activity for the stale period.
type TriageStale struct {
	Item         TriageItem `json:"item"`
	LastActivity string     `json:"lastActivity"`
	IdleDays     int        `json:"idleDays"`
}

// WorkItemTriage reads the suggestions. An agent session names its exact run;
// only the owner's unbound session or a database handler may read them.
func (c *Client) WorkItemTriage(ctx context.Context, task string, staleDays int, agentID, runID string) (WorkItemTriage, error) {
	q := url.Values{}
	if staleDays > 0 {
		q.Set("staleDays", strconv.Itoa(staleDays))
	}
	if agentID != "" {
		q.Set("agentId", agentID)
		q.Set("runId", runID)
	}
	var out WorkItemTriage
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/work-items/triage?"+q.Encode(), nil, &out)
}
