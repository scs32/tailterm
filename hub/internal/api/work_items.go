package api

import "time"

const AgentRoleDatabaseHandler = "database_handler"

type WorkItem struct {
	ID               string            `json:"id"`
	Seq              int64             `json:"seq"`
	TaskID           string            `json:"taskId"`
	Kind             string            `json:"kind"`
	Title            string            `json:"title"`
	Description      string            `json:"description"`
	Status           string            `json:"status"`
	Priority         string            `json:"priority"`
	Revision         int64             `json:"revision"`
	SourceMessageSeq int64             `json:"sourceMessageSeq,omitempty"`
	CreatedBy        Sender            `json:"createdBy"`
	UpdatedBy        Sender            `json:"updatedBy"`
	CreatedAt        time.Time         `json:"createdAt"`
	UpdatedAt        time.Time         `json:"updatedAt"`
	LastDispatch     *WorkItemDispatch `json:"lastDispatch,omitempty"`
}

type WorkItemDispatch struct {
	ID            string    `json:"id"`
	ItemID        string    `json:"itemId"`
	Revision      int64     `json:"revision"`
	TargetTaskID  string    `json:"targetTaskId"`
	TargetAgentID string    `json:"targetAgentId"`
	MessageSeq    int64     `json:"messageSeq"`
	CreatedAt     time.Time `json:"createdAt"`
}

type CreateWorkItemRequest struct {
	Kind             string `json:"kind"`
	Title            string `json:"title"`
	Description      string `json:"description"`
	Priority         string `json:"priority"`
	AgentID          string `json:"agentId"`
	SourceMessageSeq int64  `json:"sourceMessageSeq,omitempty"`
	RequestID        string `json:"requestId"`
}

type UpdateWorkItemRequest struct {
	Revision    int64   `json:"revision"`
	Title       *string `json:"title,omitempty"`
	Description *string `json:"description,omitempty"`
	Status      *string `json:"status,omitempty"`
	Priority    *string `json:"priority,omitempty"`
	AgentID     string  `json:"agentId"`
}

type DispatchWorkItemRequest struct {
	Revision     int64  `json:"revision"`
	TargetTaskID string `json:"targetTaskId"`
	AgentID      string `json:"agentId"`
	RequestID    string `json:"requestId"`
}

type WorkItemList struct {
	Items []WorkItem `json:"items"`
	Next  int64      `json:"next"`
}

type WorkItemDispatchResult struct {
	Item     WorkItem         `json:"item"`
	Dispatch WorkItemDispatch `json:"dispatch"`
}
