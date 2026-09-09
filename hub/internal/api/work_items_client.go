package api

import (
	"context"
	"net/url"
	"strconv"
)

func (c *Client) ListWorkItems(ctx context.Context, taskID, kind, status string, after int64, limit int) (WorkItemList, error) {
	q := url.Values{}
	if taskID != "" {
		q.Set("taskId", taskID)
	}
	if kind != "" {
		q.Set("kind", kind)
	}
	if status != "" {
		q.Set("status", status)
	}
	q.Set("after", strconv.FormatInt(after, 10))
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out WorkItemList
	err := c.do(ctx, "GET", "/v1/work-items?"+q.Encode(), nil, &out)
	return out, err
}
func (c *Client) GetWorkItem(ctx context.Context, taskID, itemID string) (WorkItem, error) {
	var out WorkItem
	err := c.do(ctx, "GET", "/v1/tasks/"+taskID+"/work-items/"+itemID, nil, &out)
	return out, err
}
func (c *Client) CreateWorkItem(ctx context.Context, taskID string, req CreateWorkItemRequest) (WorkItem, error) {
	var out WorkItem
	err := c.do(ctx, "POST", "/v1/tasks/"+taskID+"/work-items", req, &out)
	return out, err
}
func (c *Client) UpdateWorkItem(ctx context.Context, taskID, itemID string, req UpdateWorkItemRequest) (WorkItem, error) {
	var out WorkItem
	err := c.do(ctx, "PATCH", "/v1/tasks/"+taskID+"/work-items/"+itemID, req, &out)
	return out, err
}
func (c *Client) DispatchWorkItem(ctx context.Context, taskID, itemID string, req DispatchWorkItemRequest) (WorkItemDispatchResult, error) {
	var out WorkItemDispatchResult
	err := c.do(ctx, "POST", "/v1/tasks/"+taskID+"/work-items/"+itemID+"/dispatch", req, &out)
	return out, err
}

func historyQuery(after int64, limit int) string {
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return q.Encode()
}

func (c *Client) ListWorkItemRevisions(ctx context.Context, taskID, itemID string, after int64, limit int) (WorkItemRevisionList, error) {
	var out WorkItemRevisionList
	err := c.do(ctx, "GET", "/v1/tasks/"+taskID+"/work-items/"+itemID+"/revisions?"+historyQuery(after, limit), nil, &out)
	return out, err
}

func (c *Client) GetWorkItemRevision(ctx context.Context, taskID, itemID string, revision int64) (WorkItemRevision, error) {
	var out WorkItemRevision
	err := c.do(ctx, "GET", "/v1/tasks/"+taskID+"/work-items/"+itemID+"/revisions/"+strconv.FormatInt(revision, 10), nil, &out)
	return out, err
}

func (c *Client) ListWorkItemHistoryGaps(ctx context.Context, taskID, itemID string, after int64, limit int) (HistoryGapList, error) {
	var out HistoryGapList
	err := c.do(ctx, "GET", "/v1/tasks/"+taskID+"/work-items/"+itemID+"/history-gaps?"+historyQuery(after, limit), nil, &out)
	return out, err
}

func (c *Client) ListWorkItemMessages(ctx context.Context, taskID, itemID string, revision, after int64, limit int) (WorkItemMessageList, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if revision > 0 {
		q.Set("revision", strconv.FormatInt(revision, 10))
	}
	var out WorkItemMessageList
	err := c.do(ctx, "GET", "/v1/tasks/"+taskID+"/work-items/"+itemID+"/messages?"+q.Encode(), nil, &out)
	return out, err
}

func (c *Client) CreateWorkItemUpdate(ctx context.Context, taskID, itemID string, req CreateWorkItemUpdate) (WorkItemUpdateResult, error) {
	var out WorkItemUpdateResult
	err := c.do(ctx, "POST", "/v1/tasks/"+taskID+"/work-items/"+itemID+"/updates", req, &out)
	return out, err
}

func (c *Client) GetWorkItemUpdateReceipt(ctx context.Context, taskID, itemID, requestID, agentID string) (WorkItemUpdateResult, error) {
	q := url.Values{}
	if agentID != "" {
		q.Set("agentId", agentID)
	}
	path := "/v1/tasks/" + taskID + "/work-items/" + itemID + "/updates/receipts/" + url.PathEscape(requestID)
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out WorkItemUpdateResult
	err := c.do(ctx, "GET", path, nil, &out)
	return out, err
}
