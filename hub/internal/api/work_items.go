package api

import "time"

const AgentRoleDatabaseHandler = "database_handler"

const (
	DefaultWorkItemHistoryPage = 32
	MaxWorkItemHistoryPage     = 64
	MaxWorkItemHistoryBytes    = 3 << 20
)

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

// CreateWorkItemUpdate is the recoverable, request-keyed update contract. The
// legacy UpdateWorkItemRequest remains available to old clients over PATCH.
type CreateWorkItemUpdate struct {
	ExpectedRevision int64   `json:"expectedRevision"`
	Title            *string `json:"title,omitempty"`
	Description      *string `json:"description,omitempty"`
	Status           *string `json:"status,omitempty"`
	Priority         *string `json:"priority,omitempty"`
	AgentID          string  `json:"agentId,omitempty"`
	RunID            string  `json:"runId,omitempty"`
	RequestID        string  `json:"requestId"`
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

type WorkItemRevision struct {
	ItemID           string    `json:"itemId"`
	TaskID           string    `json:"taskId"`
	Kind             string    `json:"kind"`
	Title            string    `json:"title"`
	Description      string    `json:"description"`
	Status           string    `json:"status"`
	Priority         string    `json:"priority"`
	ItemSeq          int64     `json:"itemSeq"`
	Revision         int64     `json:"revision"`
	SourceMessageSeq int64     `json:"sourceMessageSeq,omitempty"`
	CreatedBy        Sender    `json:"createdBy"`
	UpdatedBy        Sender    `json:"updatedBy"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	UpdatedRunID     string    `json:"updatedRunId,omitempty"`
	AttributionKind  string    `json:"attributionKind"`
	ChangeKind       string    `json:"changeKind"`
	Provenance       string    `json:"provenance"`
	ChangedFields    []string  `json:"changedFields,omitempty"`
}

type HistoryGap struct {
	Seq           int64     `json:"seq"`
	FirstRevision int64     `json:"firstRevision"`
	LastRevision  int64     `json:"lastRevision"`
	ReasonCode    string    `json:"reasonCode"`
	Detail        string    `json:"detail,omitempty"`
	DetectedAt    time.Time `json:"detectedAt"`
}

type HistoryCoverage struct {
	Complete                bool        `json:"complete"`
	ObservedCurrentRevision int64       `json:"observedCurrentRevision"`
	LatestMaterialized      int64       `json:"latestMaterializedRevision"`
	SnapshotCount           int64       `json:"snapshotCount"`
	GapCount                int64       `json:"gapCount"`
	FirstGap                *HistoryGap `json:"firstGap,omitempty"`
	ConversationLinks       string      `json:"conversationLinks"`
	ExplicitMessageCount    int64       `json:"explicitMessageCount"`
	SourceMessageCount      int64       `json:"sourceMessageCount"`
	DispatchCount           int64       `json:"dispatchCount"`
}

type WorkItemRevisionList struct {
	Revisions []WorkItemRevision `json:"revisions"`
	NextAfter int64              `json:"nextAfter,omitempty"`
	Coverage  HistoryCoverage    `json:"coverage"`
}

type HistoryGapList struct {
	Gaps      []HistoryGap `json:"gaps"`
	NextAfter int64        `json:"nextAfter,omitempty"`
}

type WorkItemMessageLink struct {
	ItemRevision     int64   `json:"itemRevision"`
	RevisionCoverage string  `json:"revisionCoverage"`
	Source           bool    `json:"source,omitempty"`
	Relationship     string  `json:"relationship,omitempty"`
	Message          Message `json:"message"`
}

type WorkItemMessageList struct {
	Links     []WorkItemMessageLink `json:"links"`
	NextAfter int64                 `json:"nextAfter,omitempty"`
	Coverage  HistoryCoverage       `json:"coverage"`
}

type WorkItemUpdateReceipt struct {
	ID             string    `json:"id"`
	RequestID      string    `json:"requestId"`
	TaskID         string    `json:"taskId"`
	ItemID         string    `json:"itemId"`
	ResultRevision int64     `json:"resultRevision"`
	CreatedAt      time.Time `json:"createdAt"`
}

type WorkItemUpdateResult struct {
	Revision WorkItemRevision      `json:"revision"`
	Receipt  WorkItemUpdateReceipt `json:"receipt"`
}

type WorkItemHistoryGapResponse struct {
	Error string     `json:"error"`
	Gap   HistoryGap `json:"gap"`
}
