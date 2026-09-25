package api

import (
	"context"
	"net/url"
	"strconv"
)

// WorkOrderScopeConfirmation is the handler's assertion that the owner-filed
// item at this exact revision already contains the scope of the linked order.
// It is deliberately separate from a work-item edit.
type WorkOrderScopeConfirmation struct {
	TaskID           string `json:"taskId"`
	ItemID           string `json:"itemId"`
	ItemRevision     int64  `json:"itemRevision"`
	ScopeRevision    int64  `json:"scopeRevision"`
	OrderSeq         int64  `json:"orderMessageSeq"`
	SourceMessageSeq int64  `json:"sourceMessageSeq,omitempty"`
	RequestID        string `json:"requestId"`
	AgentID          string `json:"agentId"`
	RunID            string `json:"runId"`
	CreatedAt        string `json:"createdAt"`
}

type ConfirmWorkOrderScopeRequest struct {
	RequestID        string `json:"requestId"`
	AgentID          string `json:"agentId"`
	RunID            string `json:"runId"`
	ExpectedRevision int64  `json:"expectedRevision"`
	ScopeRevision    int64  `json:"scopeRevision"`
	OrderMessageSeq  int64  `json:"orderMessageSeq"`
	// Complete is an explicit handler assertion after reading the owner filing.
	Complete bool `json:"complete"`
}

type WorkOrderBookkeepingRequest struct {
	RequestID        string               `json:"requestId"`
	AgentID          string               `json:"agentId"`
	RunID            string               `json:"runId"`
	ExpectedRevision int64                `json:"expectedRevision"`
	OrderMessageSeq  int64                `json:"orderMessageSeq"`
	SourceMessageSeq int64                `json:"sourceMessageSeq"`
	Kind             string               `json:"kind"` // order, sequencing_note, decision
	QueueEntryID     string               `json:"queueEntryId,omitempty"`
	Admissions       []WorkOrderAdmission `json:"admissions,omitempty"`
}

type WorkOrderAdmission struct {
	AgentID       string `json:"agentId"`
	RunID         string `json:"runId"`
	ContextDigest string `json:"contextDigest"`
}

type WorkOrderBookkeepingReceipt struct {
	ID               string               `json:"id"`
	TaskID           string               `json:"taskId"`
	ItemID           string               `json:"itemId"`
	ItemRevision     int64                `json:"itemRevision"`
	ScopeRevision    int64                `json:"scopeRevision"`
	OrderMessageSeq  int64                `json:"orderMessageSeq"`
	SourceMessageSeq int64                `json:"sourceMessageSeq"`
	Kind             string               `json:"kind"`
	QueueEntryID     string               `json:"queueEntryId,omitempty"`
	Admissions       []WorkOrderAdmission `json:"admissions"`
	RequestID        string               `json:"requestId"`
	AgentID          string               `json:"agentId"`
	RunID            string               `json:"runId"`
	CreatedAt        string               `json:"createdAt"`
}

func (c *Client) ConfirmWorkOrderScope(ctx context.Context, task, item string, req ConfirmWorkOrderScopeRequest) (WorkOrderScopeConfirmation, error) {
	var out WorkOrderScopeConfirmation
	return out, c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/work-items/"+url.PathEscape(item)+"/order-scope/confirm", req, &out)
}

func (c *Client) GetWorkOrderScopeConfirmation(ctx context.Context, task, item string, revision, order int64) (WorkOrderScopeConfirmation, error) {
	var out WorkOrderScopeConfirmation
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/work-items/"+url.PathEscape(item)+"/order-scope?revision="+strconv.FormatInt(revision, 10)+"&order="+strconv.FormatInt(order, 10), nil, &out)
}

func (c *Client) SaveWorkOrderBookkeeping(ctx context.Context, task, item string, req WorkOrderBookkeepingRequest) (WorkOrderBookkeepingReceipt, error) {
	var out WorkOrderBookkeepingReceipt
	return out, c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/work-items/"+url.PathEscape(item)+"/order-bookkeeping", req, &out)
}

func (c *Client) GetWorkOrderBookkeepingReceipt(ctx context.Context, task, item, requestID string) (WorkOrderBookkeepingReceipt, error) {
	var out WorkOrderBookkeepingReceipt
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/work-items/"+url.PathEscape(item)+"/order-bookkeeping/receipts/"+url.PathEscape(requestID), nil, &out)
}
