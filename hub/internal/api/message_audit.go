package api

import (
	"context"
	"net/url"
	"time"
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
