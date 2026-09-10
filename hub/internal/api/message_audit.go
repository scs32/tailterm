package api

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

const (
	MessageAuditUnclassified = "unclassified"
	MessageAuditIntake       = "intake"
	MessageAuditWork         = "work"

	MaxMessageAuditRelated       = 15
	MaxMessageAuditSources       = 16
	DefaultMessageAuditPage      = 32
	MaxMessageAuditPage          = 64
	MaxMessageAuditResponseBytes = 1 << 20
)

// MessageWorkItem pins the item revision considered when a message was written.
// A1 ordinary posts support one same-project primary link. Validated dispatches
// can carry their source item into a different destination project.
type MessageWorkItem struct {
	ItemTaskID   string `json:"itemTaskId"`
	ItemID       string `json:"itemId"`
	ItemRevision int64  `json:"itemRevision"`
	Relationship string `json:"relationship"`
}

// MessageReference identifies an existing recorded order message. It does not
// claim that the referenced text is a typed or machine-validated work order.
type MessageReference struct {
	TaskID string `json:"taskId"`
	Seq    int64  `json:"seq"`
}

// MessagePostReceipt proves storage of one keyed post, not retrieval/execution.
type MessagePostReceipt struct {
	ID         string    `json:"id"`
	RequestID  string    `json:"requestId"`
	TaskID     string    `json:"taskId"`
	MessageSeq int64     `json:"messageSeq"`
	CreatedAt  time.Time `json:"createdAt"`
}

// MessageAuditOriginal is immutable creation-time audit context. Unclassified
// means the message pre-dates explicit audit context or was posted by a legacy
// client without declaring one.
type MessageAuditOriginal struct {
	Classification   string            `json:"classification"`
	WorkItems        []MessageWorkItem `json:"workItems"`
	WorkOrderMessage *MessageReference `json:"workOrderMessage,omitempty"`
}

// MessageAuditProjection is the current classification and complete current
// link set. It is separate from Message.WorkItems, which remains original.
type MessageAuditProjection struct {
	Revision       int64             `json:"revision"`
	Classification string            `json:"classification"`
	WorkItems      []MessageWorkItem `json:"workItems"`
	EventSeq       int64             `json:"eventSeq"`
	UpdatedAt      time.Time         `json:"updatedAt"`
}

type MessageAuditActor struct {
	AgentID string `json:"agentId,omitempty"`
	RunID   string `json:"runId,omitempty"`
	Caller  Caller `json:"caller"`
}

type MessageAuditEvent struct {
	ID         string                  `json:"id"`
	Cursor     int64                   `json:"cursor"`
	Message    MessageReference        `json:"message"`
	Revision   int64                   `json:"revision"`
	Operation  string                  `json:"operation"`
	Provenance string                  `json:"provenance"`
	Before     *MessageAuditProjection `json:"before,omitempty"`
	After      MessageAuditProjection  `json:"after"`
	Reason     string                  `json:"reason"`
	Sources    []MessageReference      `json:"sources"`
	Actor      MessageAuditActor       `json:"actor"`
	CreatedAt  time.Time               `json:"createdAt"`
}

type MessageAuditRecord struct {
	Message  MessageReference        `json:"message"`
	Original MessageAuditOriginal    `json:"original"`
	Current  *MessageAuditProjection `json:"current,omitempty"`
}

type MessageAuditHistory struct {
	Message    MessageReference    `json:"message"`
	Events     []MessageAuditEvent `json:"events"`
	NextCursor string              `json:"nextCursor,omitempty"`
	Cutoff     int64               `json:"cutoff"`
}

type MessageAuditDesiredState struct {
	Classification string            `json:"classification"`
	WorkItems      []MessageWorkItem `json:"workItems"`
}

type CorrectMessageAuditRequest struct {
	RequestID        string                   `json:"requestId"`
	ExpectedRevision int64                    `json:"expectedRevision"`
	Desired          MessageAuditDesiredState `json:"desired"`
	Reason           string                   `json:"reason"`
	Sources          []MessageReference       `json:"sources"`
	AgentID          string                   `json:"agentId,omitempty"`
	RunID            string                   `json:"runId,omitempty"`
}

type ResolveMessageAuditNewItem struct {
	RequestID        string `json:"requestId"`
	ExpectedRevision int64  `json:"expectedRevision"`
	Kind             string `json:"kind"`
	Title            string `json:"title"`
	Description      string `json:"description,omitempty"`
	Priority         string `json:"priority,omitempty"`
}

type ResolveMessageAuditRequest struct {
	RequestID        string                      `json:"requestId"`
	ExpectedRevision int64                       `json:"expectedRevision"`
	ExistingItem     *MessageWorkItem            `json:"existingItem,omitempty"`
	NewItem          *ResolveMessageAuditNewItem `json:"newItem,omitempty"`
	Reason           string                      `json:"reason"`
	Sources          []MessageReference          `json:"sources"`
	AgentID          string                      `json:"agentId,omitempty"`
	RunID            string                      `json:"runId,omitempty"`
}

type MessageAuditReceipt struct {
	ID            string           `json:"id"`
	TaskID        string           `json:"taskId"`
	Operation     string           `json:"operation"`
	RequestID     string           `json:"requestId"`
	Message       MessageReference `json:"message"`
	AuditRevision int64            `json:"auditRevision"`
	EventSeq      int64            `json:"eventSeq"`
	CreatedAt     time.Time        `json:"createdAt"`
}

type MessageAuditMutationResult struct {
	State   MessageAuditRecord  `json:"state"`
	Event   MessageAuditEvent   `json:"event"`
	Receipt MessageAuditReceipt `json:"receipt"`
	Item    *WorkItem           `json:"item,omitempty"`
	Replay  bool                `json:"replay"`
}

type MessageAuditChangeQuery struct {
	Cursor string
	Limit  int
	Kind   string
}

type MessageAuditChangePage struct {
	Events     []MessageAuditEvent `json:"events"`
	NextCursor string              `json:"nextCursor,omitempty"`
	Cutoff     int64               `json:"cutoff"`
}

type MessageAuditItemReference struct {
	ItemTaskID   string `json:"itemTaskId"`
	ItemID       string `json:"itemId"`
	ItemRevision int64  `json:"itemRevision"`
}

// CreateMessageAuditAssociationRequest explicitly authorizes one foreign item
// for one destination project. The exact source message must be authored by the
// declaring human or active database-handler run in that destination project.
type CreateMessageAuditAssociationRequest struct {
	RequestID string                    `json:"requestId"`
	Item      MessageAuditItemReference `json:"item"`
	Source    MessageReference          `json:"source"`
	Reason    string                    `json:"reason"`
	AgentID   string                    `json:"agentId,omitempty"`
	RunID     string                    `json:"runId,omitempty"`
}

type MessageAuditAssociation struct {
	ID        string                    `json:"id"`
	TaskID    string                    `json:"taskId"`
	Item      MessageAuditItemReference `json:"item"`
	Source    MessageReference          `json:"source"`
	Reason    string                    `json:"reason"`
	Actor     MessageAuditActor         `json:"actor"`
	CreatedAt time.Time                 `json:"createdAt"`
}

type MessageAuditAssociationReceipt struct {
	ID            string    `json:"id"`
	TaskID        string    `json:"taskId"`
	RequestID     string    `json:"requestId"`
	AssociationID string    `json:"associationId"`
	CreatedAt     time.Time `json:"createdAt"`
}

type MessageAuditAssociationResult struct {
	Association MessageAuditAssociation        `json:"association"`
	Receipt     MessageAuditAssociationReceipt `json:"receipt"`
	Replay      bool                           `json:"replay"`
}

// GetMessagePostReceipt recovers the original committed message in the caller's
// workspace and declared agent scope. Agent labels are not per-agent credentials.
func (c *Client) GetMessagePostReceipt(ctx context.Context, taskID, requestID, agentID string) (Message, error) {
	q := url.Values{}
	if agentID != "" {
		q.Set("agentId", agentID)
	}
	path := "/v1/tasks/" + url.PathEscape(taskID) + "/messages/receipts/" + url.PathEscape(requestID)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out Message
	err := c.do(ctx, "GET", path, nil, &out)
	return out, err
}

func messageAuditPath(taskID string, seq int64, suffix string) string {
	return "/v1/tasks/" + url.PathEscape(taskID) + "/message-audit/messages/" + strconv.FormatInt(seq, 10) + suffix
}

func (c *Client) GetMessageAudit(ctx context.Context, taskID string, seq int64) (MessageAuditRecord, error) {
	var out MessageAuditRecord
	err := c.do(ctx, "GET", messageAuditPath(taskID, seq, ""), nil, &out)
	return out, err
}

func (c *Client) ListMessageAuditHistory(ctx context.Context, taskID string, seq, afterVersion int64, cursor string, limit int) (MessageAuditHistory, error) {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	} else {
		q.Set("afterVersion", strconv.FormatInt(afterVersion, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out MessageAuditHistory
	err := c.do(ctx, "GET", messageAuditPath(taskID, seq, "/history")+"?"+q.Encode(), nil, &out)
	return out, err
}

func (c *Client) CreateMessageAuditAssociation(ctx context.Context, taskID string, req CreateMessageAuditAssociationRequest) (MessageAuditAssociationResult, error) {
	var out MessageAuditAssociationResult
	path := "/v1/tasks/" + url.PathEscape(taskID) + "/message-audit/associations"
	err := c.do(ctx, "POST", path, req, &out)
	return out, err
}

func (c *Client) GetMessageAuditAssociationReceipt(ctx context.Context, taskID, requestID, agentID string) (MessageAuditAssociationResult, error) {
	q := url.Values{}
	if agentID != "" {
		q.Set("agentId", agentID)
	}
	path := "/v1/tasks/" + url.PathEscape(taskID) + "/message-audit/associations/receipts/" + url.PathEscape(requestID)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out MessageAuditAssociationResult
	err := c.do(ctx, "GET", path, nil, &out)
	return out, err
}

func (c *Client) CorrectMessageAudit(ctx context.Context, taskID string, seq int64, req CorrectMessageAuditRequest) (MessageAuditMutationResult, error) {
	var out MessageAuditMutationResult
	err := c.do(ctx, "POST", messageAuditPath(taskID, seq, "/corrections"), req, &out)
	return out, err
}

func (c *Client) ResolveMessageAudit(ctx context.Context, taskID string, seq int64, req ResolveMessageAuditRequest) (MessageAuditMutationResult, error) {
	var out MessageAuditMutationResult
	err := c.do(ctx, "POST", messageAuditPath(taskID, seq, "/resolve"), req, &out)
	return out, err
}

func (c *Client) GetMessageAuditReceipt(ctx context.Context, taskID, requestID, operation, agentID string) (MessageAuditMutationResult, error) {
	q := url.Values{}
	q.Set("operation", operation)
	if agentID != "" {
		q.Set("agentId", agentID)
	}
	var out MessageAuditMutationResult
	path := "/v1/tasks/" + url.PathEscape(taskID) + "/message-audit/receipts/" + url.PathEscape(requestID) + "?" + q.Encode()
	err := c.do(ctx, "GET", path, nil, &out)
	return out, err
}

func (c *Client) ListMessageAuditChanges(ctx context.Context, taskID string, query MessageAuditChangeQuery) (MessageAuditChangePage, error) {
	q := url.Values{}
	if query.Cursor != "" {
		q.Set("cursor", query.Cursor)
	}
	if query.Limit > 0 {
		q.Set("limit", strconv.Itoa(query.Limit))
	}
	if query.Kind != "" {
		q.Set("kind", query.Kind)
	}
	var out MessageAuditChangePage
	path := "/v1/tasks/" + url.PathEscape(taskID) + "/message-audit/changes"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	err := c.doLimited(ctx, "GET", path, nil, &out, MaxMessageAuditResponseBytes+64*1024)
	return out, err
}
