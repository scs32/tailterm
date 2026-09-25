package api

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
)

// TeamQueueEntry is the hub-owned delivery queue. It is unrelated to the
// deliberate, agent-claimed Queue API.
type TeamQueueEntry struct {
	ID              string          `json:"id"`
	TaskID          string          `json:"taskId"`
	ItemID          string          `json:"itemId"`
	ItemRevision    int64           `json:"itemRevision"`
	OrderMessageSeq int64           `json:"orderMessageSeq"`
	Template        string          `json:"template"`
	Position        int64           `json:"position"`
	State           string          `json:"state"`
	Revision        int64           `json:"revision"`
	Host            string          `json:"host"`
	Cwd             string          `json:"cwd"`
	PauseGeneration int64           `json:"pauseGeneration"`
	LaunchJSON      json.RawMessage `json:"launch,omitempty"`
	CloseJSON       json.RawMessage `json:"close,omitempty"`
	Failure         string          `json:"failure,omitempty"`
	EscalationSeq   int64           `json:"escalationSeq,omitempty"`
}

type TeamQueueList struct {
	Entries []TeamQueueEntry `json:"entries"`
}

type TeamQueueRequest struct {
	RequestID        string          `json:"requestId"`
	Operation        string          `json:"operation"`
	ItemID           string          `json:"itemId,omitempty"`
	OrderMessageSeq  int64           `json:"orderMessageSeq,omitempty"`
	Template         string          `json:"template,omitempty"`
	EntryID          string          `json:"entryId,omitempty"`
	BeforeID         string          `json:"beforeId,omitempty"`
	ExpectedRevision int64           `json:"expectedRevision,omitempty"`
	Host             string          `json:"host,omitempty"`
	Cwd              string          `json:"cwd,omitempty"`
	PauseGeneration  int64           `json:"pauseGeneration,omitempty"`
	LaunchJSON       json.RawMessage `json:"launch,omitempty"`
	CloseJSON        json.RawMessage `json:"close,omitempty"`
	Failure          string          `json:"failure,omitempty"`
	MemberIndex      int             `json:"memberIndex,omitempty"`
	MemberRunID      string          `json:"memberRunId,omitempty"`
}

func (c *Client) ListTeamQueue(ctx context.Context, task string) (TeamQueueList, error) {
	var out TeamQueueList
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/team-queue", nil, &out)
}

func (c *Client) TeamQueueAction(ctx context.Context, task string, req TeamQueueRequest) (TeamQueueEntry, error) {
	var out TeamQueueEntry
	return out, c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/team-queue/actions", req, &out)
}

func (c *Client) GetTeamQueueEntry(ctx context.Context, task, entry string) (TeamQueueEntry, error) {
	var out TeamQueueEntry
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/team-queue/"+url.PathEscape(entry), nil, &out)
}

func (c *Client) TeamQueueByHost(ctx context.Context, host string) (TeamQueueList, error) {
	var out TeamQueueList
	return out, c.do(ctx, "GET", "/v1/team-queues?host="+url.QueryEscape(host)+"&limit="+strconv.Itoa(MaxLimit), nil, &out)
}
