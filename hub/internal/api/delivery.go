package api

import (
	"context"
	"net/url"
	"time"
)

const ReliableDeliveryCapabilityVersion = 1

const (
	DeliveryAssignment = "assignment"
	DeliveryAmendment  = "amendment"
	DeliveryReview     = "review"

	DeliveryUnacknowledged = "unacknowledged"
	DeliveryAcknowledged   = "acknowledged"
	DeliveryProgressing    = "progressing"
	DeliveryBlocked        = "blocked"
	DeliveryResult         = "result"
	DeliverySuperseded     = "superseded"
)

type RequiredDelivery struct {
	ID               string           `json:"id"`
	TaskID           string           `json:"taskId"`
	MessageSeq       int64            `json:"messageSeq"`
	Kind             string           `json:"kind"`
	AgentID          string           `json:"agentId"`
	RunID            string           `json:"runId"`
	ItemTaskID       string           `json:"itemTaskId"`
	ItemID           string           `json:"itemId"`
	ItemRevision     int64            `json:"itemRevision"`
	WorkOrderMessage MessageReference `json:"workOrderMessage"`
	ContextDigest    string           `json:"contextDigest"`
	Generation       int64            `json:"generation"`
	SupersedesID     string           `json:"supersedesDeliveryId,omitempty"`
	Current          bool             `json:"current"`
	Phase            string           `json:"phase"`
	ExecutionEpoch   int64            `json:"executionEpoch"`
	CurrentBlockID   string           `json:"currentBlockId,omitempty"`
	CurrentBlock     *DeliveryBlock   `json:"currentBlock,omitempty"`
	ResultText       string           `json:"resultText,omitempty"`
	Message          *Message         `json:"message,omitempty"`
	Events           []DeliveryEvent  `json:"events,omitempty"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
}

type DeliveryEvent struct {
	ID         string         `json:"id"`
	TaskID     string         `json:"taskId"`
	DeliveryID string         `json:"deliveryId"`
	Sequence   int64          `json:"sequence"`
	Kind       string         `json:"kind"`
	AgentID    string         `json:"agentId,omitempty"`
	RunID      string         `json:"runId,omitempty"`
	RequestID  string         `json:"requestId"`
	Data       map[string]any `json:"data,omitempty"`
	By         Caller         `json:"by"`
	CreatedAt  time.Time      `json:"createdAt"`
}

type DeliveryBlock struct {
	ID             string     `json:"id"`
	DeliveryID     string     `json:"deliveryId"`
	ExecutionEpoch int64      `json:"executionEpoch"`
	ReasonClass    string     `json:"reasonClass"`
	Text           string     `json:"text"`
	Resolved       bool       `json:"resolved"`
	ResolutionID   string     `json:"resolutionId,omitempty"`
	ResolutionText string     `json:"resolutionText,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	ResolvedAt     *time.Time `json:"resolvedAt,omitempty"`
}

type DeliveryReceipt struct {
	ID        string    `json:"id"`
	RequestID string    `json:"requestId"`
	Operation string    `json:"operation"`
	CreatedAt time.Time `json:"createdAt"`
}

type DeliveryMutation struct {
	Delivery RequiredDelivery `json:"delivery"`
	Event    DeliveryEvent    `json:"event"`
	Block    *DeliveryBlock   `json:"block,omitempty"`
	Receipt  DeliveryReceipt  `json:"receipt"`
	Replay   bool             `json:"replay"`
}

type CreateRequiredDeliveryRequest struct {
	RequestID                 string           `json:"requestId"`
	MessageSeq                int64            `json:"messageSeq"`
	Kind                      string           `json:"kind"`
	AgentID                   string           `json:"agentId"`
	RunID                     string           `json:"runId"`
	ItemTaskID                string           `json:"itemTaskId"`
	ItemID                    string           `json:"itemId"`
	ItemRevision              int64            `json:"itemRevision"`
	WorkOrderMessage          MessageReference `json:"workOrderMessage"`
	ExpectedCurrentGeneration int64            `json:"expectedCurrentGeneration"`
	ExpectedCurrentDeliveryID string           `json:"expectedCurrentDeliveryId,omitempty"`
	ProducerAgentID           string           `json:"producerAgentId,omitempty"`
	ProducerRunID             string           `json:"producerRunId,omitempty"`
}

type DeliveryActionRequest struct {
	RequestID     string `json:"requestId"`
	AgentID       string `json:"agentId"`
	RunID         string `json:"runId"`
	ExpectedEpoch int64  `json:"expectedEpoch,omitempty"`
	Text          string `json:"text,omitempty"`
}

type DeliveryBlockRequest struct {
	RequestID     string `json:"requestId"`
	AgentID       string `json:"agentId"`
	RunID         string `json:"runId"`
	ExpectedEpoch int64  `json:"expectedEpoch"`
	ReasonClass   string `json:"reasonClass"`
	Text          string `json:"text,omitempty"`
}

type DeliveryResolutionRequest struct {
	RequestID     string `json:"requestId"`
	AgentID       string `json:"agentId,omitempty"`
	RunID         string `json:"runId,omitempty"`
	ExpectedEpoch int64  `json:"expectedEpoch"`
	Text          string `json:"text"`
}

type DeliveryResumeRequest struct {
	RequestID     string `json:"requestId"`
	AgentID       string `json:"agentId"`
	RunID         string `json:"runId"`
	ExpectedEpoch int64  `json:"expectedEpoch"`
	ResolutionID  string `json:"resolutionId"`
}

func (c *Client) CreateRequiredDelivery(ctx context.Context, task string, req CreateRequiredDeliveryRequest) (DeliveryMutation, error) {
	var out DeliveryMutation
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/required-deliveries", req, &out)
	return out, err
}

func (c *Client) CurrentAssignment(ctx context.Context, task, agent, runID string) (RequiredDelivery, error) {
	var out RequiredDelivery
	q := url.Values{}
	q.Set("runId", runID)
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/agents/"+url.PathEscape(agent)+"/current-assignment?"+q.Encode(), nil, &out)
	return out, err
}

func (c *Client) DeliveryAction(ctx context.Context, task, delivery, operation string, req any) (DeliveryMutation, error) {
	var out DeliveryMutation
	path := "/v1/tasks/" + url.PathEscape(task) + "/deliveries/" + url.PathEscape(delivery) + "/" + url.PathEscape(operation)
	err := c.do(ctx, "POST", path, req, &out)
	return out, err
}

func (c *Client) ResolveDeliveryBlock(ctx context.Context, task, delivery, block string, req DeliveryResolutionRequest) (DeliveryMutation, error) {
	var out DeliveryMutation
	path := "/v1/tasks/" + url.PathEscape(task) + "/deliveries/" + url.PathEscape(delivery) + "/blocks/" + url.PathEscape(block) + "/resolutions"
	err := c.do(ctx, "POST", path, req, &out)
	return out, err
}
