package api

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

const (
	QueueVersion        = 1
	DefaultQueuePage    = 32
	MaxQueuePage        = 64
	MaxQueuePageBytes   = 1 << 20
	MaxQueueReasonBytes = 1024
	QueueStateWaiting   = "waiting"
	QueueStateClaimed   = "claimed"
	QueueStateActive    = "active"
	QueueStateCompleted = "completed"
	QueueStateCancelled = "cancelled"
	QueueNoticeChanged  = "queue_changed"
)

type QueueItemSummary struct {
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Status      string `json:"status"`
	Priority    string `json:"priority"`
	Description string `json:"description,omitempty"`
}

type QueueSelection struct {
	TaskID     string `json:"taskId"`
	MessageSeq int64  `json:"messageSeq"`
}

type QueueEntry struct {
	ID                   string            `json:"id"`
	Seq                  int64             `json:"seq"`
	TargetTaskID         string            `json:"targetTaskId"`
	SourceTaskID         string            `json:"sourceTaskId"`
	ItemID               string            `json:"itemId"`
	Cycle                int64             `json:"cycle"`
	Revision             int64             `json:"revision"`
	State                string            `json:"state"`
	QueuePriority        string            `json:"queuePriority"`
	Eligible             bool              `json:"eligible"`
	Stale                bool              `json:"stale"`
	ReviewNeeded         bool              `json:"reviewNeeded"`
	PendingUpdate        bool              `json:"pendingUpdate"`
	OfferedItemRevision  int64             `json:"offeredItemRevision"`
	CurrentItemRevision  int64             `json:"currentItemRevision"`
	ClaimedItemRevision  int64             `json:"claimedItemRevision,omitempty"`
	ClaimantAgentID      string            `json:"claimantAgentId,omitempty"`
	ClaimantRunID        string            `json:"claimantRunId,omitempty"`
	WorkOrderMessage     *MessageReference `json:"workOrderMessage,omitempty"`
	Selection            *QueueSelection   `json:"selection,omitempty"`
	WorkerAgentID        string            `json:"workerAgentId,omitempty"`
	WorkerRunID          string            `json:"workerRunId,omitempty"`
	ContextDigest        string            `json:"contextDigest,omitempty"`
	ReconciliationNeeded bool              `json:"reconciliationNeeded"`
	TerminalReason       string            `json:"terminalReason,omitempty"`
	TerminalItemRevision int64             `json:"terminalItemRevision,omitempty"`
	LegacyObserved       bool              `json:"legacyObserved"`
	OrchestratorAgentID  string            `json:"orchestratorAgentId,omitempty"`
	OrchestratorRunID    string            `json:"orchestratorRunId,omitempty"`
	FirstEnqueuedAt      time.Time         `json:"firstEnqueuedAt"`
	LatestEnqueuedAt     time.Time         `json:"latestEnqueuedAt"`
	UpdatedAt            time.Time         `json:"updatedAt"`
	Item                 QueueItemSummary  `json:"item"`
}

type QueueEvent struct {
	Seq          int64      `json:"seq"`
	TargetTaskID string     `json:"targetTaskId"`
	EntryID      string     `json:"entryId"`
	Cycle        int64      `json:"cycle"`
	Revision     int64      `json:"revision"`
	Kind         string     `json:"kind"`
	Actor        Sender     `json:"actor"`
	ActorRunID   string     `json:"actorRunId,omitempty"`
	Reason       string     `json:"reason,omitempty"`
	DispatchID   string     `json:"dispatchId,omitempty"`
	Snapshot     QueueEntry `json:"snapshot"`
	ObservedAt   time.Time  `json:"observedAt"`
	EffectiveAt  time.Time  `json:"effectiveAt"`
}

type QueueNotification struct {
	ID                  string    `json:"id"`
	EventSeq            int64     `json:"eventSeq"`
	EntryID             string    `json:"entryId"`
	Cycle               int64     `json:"cycle"`
	Kind                string    `json:"kind"`
	CausalAuthor        Sender    `json:"causalAuthor"`
	RecipientAgentID    string    `json:"recipientAgentId,omitempty"`
	RecipientRunID      string    `json:"recipientRunId,omitempty"`
	RecipientGeneration int64     `json:"recipientGeneration"`
	Status              string    `json:"status"`
	MessageSeq          int64     `json:"messageSeq,omitempty"`
	UnavailableReason   string    `json:"unavailableReason,omitempty"`
	CreatedAt           time.Time `json:"createdAt"`
}

type QueueNoticeRef struct {
	EntryID  string `json:"entryId"`
	Cycle    int64  `json:"cycle"`
	EventSeq int64  `json:"eventSeq"`
}

type SystemNotice struct {
	Kind  string         `json:"kind"`
	ID    string         `json:"id"`
	Queue QueueNoticeRef `json:"queue"`
}

type QueueReceipt struct {
	ID        string    `json:"id"`
	RequestID string    `json:"requestId"`
	Operation string    `json:"operation"`
	TaskID    string    `json:"taskId"`
	EntryID   string    `json:"entryId"`
	Cycle     int64     `json:"cycle"`
	EventSeq  int64     `json:"eventSeq"`
	CreatedAt time.Time `json:"createdAt"`
}

type QueueActionRequest struct {
	Operation            string            `json:"operation"`
	RequestID            string            `json:"requestId"`
	ExpectedRevision     int64             `json:"expectedRevision"`
	Cycle                int64             `json:"cycle"`
	ExpectedItemRevision int64             `json:"expectedItemRevision,omitempty"`
	Priority             string            `json:"priority,omitempty"`
	Reason               string            `json:"reason,omitempty"`
	AgentID              string            `json:"agentId,omitempty"`
	RunID                string            `json:"runId,omitempty"`
	Selection            *QueueSelection   `json:"selection,omitempty"`
	ClaimantAgentID      string            `json:"claimantAgentId,omitempty"`
	ClaimantRunID        string            `json:"claimantRunId,omitempty"`
	WorkOrderMessage     *MessageReference `json:"workOrderMessage,omitempty"`
	WorkerAgentID        string            `json:"workerAgentId,omitempty"`
	WorkerRunID          string            `json:"workerRunId,omitempty"`
	ContextDigest        string            `json:"contextDigest,omitempty"`
	ReconciledRunID      string            `json:"reconciledRunId,omitempty"`
}

type QueueActionResult struct {
	Entry        QueueEntry         `json:"entry"`
	Event        QueueEvent         `json:"event"`
	Receipt      QueueReceipt       `json:"receipt"`
	Notification *QueueNotification `json:"notification,omitempty"`
	Replay       bool               `json:"replay"`
}

type QueueDispatchReceipt struct {
	Entry        QueueEntry         `json:"entry"`
	Event        QueueEvent         `json:"event"`
	Notification *QueueNotification `json:"notification,omitempty"`
	Replay       bool               `json:"replay"`
}

type QueueList struct {
	Entries  []QueueEntry `json:"entries"`
	Cursor   string       `json:"cursor,omitempty"`
	Cutoff   int64        `json:"cutoff"`
	Complete bool         `json:"complete"`
}

type QueueHistory struct {
	Entry    QueueEntry   `json:"entry"`
	Events   []QueueEvent `json:"events"`
	Cursor   string       `json:"cursor,omitempty"`
	Cutoff   int64        `json:"cutoff"`
	Complete bool         `json:"complete"`
}

type QueueChangePage struct {
	Events     []QueueEvent `json:"events"`
	Checkpoint int64        `json:"checkpoint"`
	Cutoff     int64        `json:"cutoff"`
	Complete   bool         `json:"complete"`
}

func (c *Client) ListQueue(ctx context.Context, taskID, cursor string, limit int, includeTerminal bool) (QueueList, error) {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if includeTerminal {
		q.Set("includeTerminal", "1")
	}
	var out QueueList
	err := c.doLimited(ctx, "GET", "/v1/tasks/"+taskID+"/queue?"+q.Encode(), nil, &out, 2*MaxQueuePageBytes)
	return out, err
}

func (c *Client) GetQueueEntry(ctx context.Context, taskID, entryID string) (QueueEntry, error) {
	var out QueueEntry
	err := c.doLimited(ctx, "GET", "/v1/tasks/"+taskID+"/queue/"+url.PathEscape(entryID), nil, &out, 2*MaxQueuePageBytes)
	return out, err
}

func (c *Client) ListQueueHistory(ctx context.Context, taskID, entryID, cursor string, limit int) (QueueHistory, error) {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out QueueHistory
	err := c.doLimited(ctx, "GET", "/v1/tasks/"+taskID+"/queue/"+url.PathEscape(entryID)+"/history?"+q.Encode(), nil, &out, 2*MaxQueuePageBytes)
	return out, err
}

func (c *Client) ListQueueChanges(ctx context.Context, taskID string, after, cutoff int64, limit int) (QueueChangePage, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	if cutoff > 0 {
		q.Set("cutoff", strconv.FormatInt(cutoff, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out QueueChangePage
	err := c.doLimited(ctx, "GET", "/v1/tasks/"+taskID+"/queue/changes?"+q.Encode(), nil, &out, 2*MaxQueuePageBytes)
	return out, err
}

func (c *Client) QueueAction(ctx context.Context, taskID, entryID string, req QueueActionRequest) (QueueActionResult, error) {
	var out QueueActionResult
	err := c.do(ctx, "POST", "/v1/tasks/"+taskID+"/queue/"+url.PathEscape(entryID)+"/actions", req, &out)
	return out, err
}

func (c *Client) GetQueueReceipt(ctx context.Context, taskID, requestID, agentID string) (QueueActionResult, error) {
	q := url.Values{}
	if agentID != "" {
		q.Set("agentId", agentID)
	}
	var out QueueActionResult
	err := c.do(ctx, "GET", "/v1/tasks/"+taskID+"/queue-receipts/"+url.PathEscape(requestID)+"?"+q.Encode(), nil, &out)
	return out, err
}
