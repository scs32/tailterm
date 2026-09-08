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
