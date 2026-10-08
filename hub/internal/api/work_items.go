package api

import (
	"encoding/json"
	"time"
)

const AgentRoleDatabaseHandler = "database_handler"

const (
	DefaultWorkItemHistoryPage = 32
	MaxWorkItemHistoryPage     = 64
	MaxWorkItemHistoryBytes    = 3 << 20
	DefaultNarrativePage       = 32
	MaxNarrativePage           = 64
	MaxNarrativeContentBytes   = 1 << 20
	MaxNarrativeReferenceBytes = 384 << 10
	MaxNarrativeResponseBytes  = 3 << 20
	MaxNarrativeMetadataBody   = 6*MaxNarrativeReferenceBytes + 256*1024
	MaxNarrativeRequestBody    = 6*(MaxNarrativeContentBytes+MaxNarrativeReferenceBytes) + 256*1024
	MaxNarrativeEncodedBytes   = MaxNarrativeRequestBody + 256*1024
)

type WorkItem struct {
	ID               string              `json:"id"`
	Seq              int64               `json:"seq"`
	TaskID           string              `json:"taskId"`
	Kind             string              `json:"kind"`
	Title            string              `json:"title"`
	Description      string              `json:"description"`
	Status           string              `json:"status"`
	Priority         string              `json:"priority"`
	Revision         int64               `json:"revision"`
	ScopeRevision    int64               `json:"scopeRevision"`
	SourceMessageSeq int64               `json:"sourceMessageSeq,omitempty"`
	CreatedBy        Sender              `json:"createdBy"`
	UpdatedBy        Sender              `json:"updatedBy"`
	CreatedAt        time.Time           `json:"createdAt"`
	UpdatedAt        time.Time           `json:"updatedAt"`
	LastDispatch     *WorkItemDispatch   `json:"lastDispatch,omitempty"`
	CompletionReport *NarrativeReportPin `json:"completionReport,omitempty"`
	// Budget is the token estimate beside the lifetime attributed usage. A
	// current hub always sends it on reads; a missing key means an older hub.
	Budget *TokenBudget `json:"budget,omitempty"`
}

// MaxEstimateTokens and MaxEstimateBasisBytes bound a saved estimate.
const (
	MaxEstimateTokens     = int64(1e12)
	MaxEstimateBasisBytes = 240
)

// WorkItemEstimate is one saved token estimate. Tokens 0 is a clear.
type WorkItemEstimate struct {
	Tokens int64     `json:"tokens"`
	Basis  string    `json:"basis,omitempty"`
	SetAt  time.Time `json:"setAt"`
	SetBy  Sender    `json:"setBy"`
}

// TokenBudget shows an item's estimate against its lifetime attributed
// tokens. ActualTokens is an exact rational string over every run, open and
// closed, and "0" when not measured. ActualState is "measured", "partial" or
// "not measured", the state the unfiltered usage report gives the item; a
// partial actual and its ratio are lower bounds. Ratio is actual/estimate,
// absent without an estimate or when not measured. Team is present only while
// the item has a running queue entry.
type TokenBudget struct {
	Estimate     *WorkItemEstimate `json:"estimate,omitempty"`
	ActualTokens string            `json:"actualTokens"`
	ActualState  string            `json:"actualState"`
	Ratio        string            `json:"ratio,omitempty"`
	Team         *TeamTokenBudget  `json:"team,omitempty"`
}

// TeamTokenBudget is what the team of an item's running queue entry has spent
// on the item: the figure the token warning compares with the estimate. Its
// fields read as TokenBudget's do, over that team's runs only.
type TeamTokenBudget struct {
	EntryID      string `json:"entryId"`
	ActualTokens string `json:"actualTokens"`
	ActualState  string `json:"actualState"`
	Ratio        string `json:"ratio,omitempty"`
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
	Revision         int64               `json:"revision"`
	Title            *string             `json:"title,omitempty"`
	Description      *string             `json:"description,omitempty"`
	Status           *string             `json:"status,omitempty"`
	Priority         *string             `json:"priority,omitempty"`
	AgentID          string              `json:"agentId"`
	CompletionReport *NarrativeReportPin `json:"completionReport,omitempty"`
	// ScopeChange marks a title or description change as a deliberate scope
	// amendment, which the hub otherwise refuses while the item's accepted
	// candidate awaits release.
	ScopeChange bool `json:"scopeChange,omitempty"`
}

// CreateWorkItemUpdate is the recoverable, request-keyed update contract. The
// legacy UpdateWorkItemRequest remains available to old clients over PATCH.
type CreateWorkItemUpdate struct {
	ExpectedRevision int64               `json:"expectedRevision"`
	Title            *string             `json:"title,omitempty"`
	Description      *string             `json:"description,omitempty"`
	Status           *string             `json:"status,omitempty"`
	Priority         *string             `json:"priority,omitempty"`
	AgentID          string              `json:"agentId,omitempty"`
	RunID            string              `json:"runId,omitempty"`
	RequestID        string              `json:"requestId"`
	CompletionReport *NarrativeReportPin `json:"completionReport,omitempty"`
	// QueueAcceptance records the leased handler's team queue acceptance in
	// the same transaction as a done save. The hub fills the repository, base,
	// item revision and completion report from the entry and the saved item.
	QueueAcceptance *WorkItemQueueAcceptance `json:"queueAcceptance,omitempty"`
	// EstimateTokens saves a token estimate with its basis and nothing else:
	// the item keeps its revision. 0 clears the estimate and takes no basis.
	// Only the database handler or the owner sets it.
	EstimateTokens *int64  `json:"estimateTokens,omitempty"`
	EstimateBasis  *string `json:"estimateBasis,omitempty"`
	// ScopeChange marks a title or description change as a deliberate scope
	// amendment, which the hub otherwise refuses while the item's accepted
	// candidate awaits release.
	ScopeChange bool `json:"scopeChange,omitempty"`
}

// WorkItemQueueAcceptance is the Git tuple of the accepted builder result for
// the item's running team queue entry.
type WorkItemQueueAcceptance struct {
	EntryID  string `json:"entryId"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
	Commit   string `json:"commit"`
	Evidence string `json:"evidence,omitempty"`
}

// NarrativeReportPin is verified atomically when a feature first transitions
// to Done. It identifies the exact immutable report bytes accepted for that
// feature scope; later report corrections do not rewrite this original pin.
type NarrativeReportPin struct {
	ReportID      string `json:"reportId"`
	Version       int64  `json:"version"`
	Digest        string `json:"digest"`
	ScopeRevision int64  `json:"scopeRevision"`
}

type NarrativeActor struct {
	AgentID string `json:"agentId,omitempty"`
	RunID   string `json:"runId,omitempty"`
	Caller  Caller `json:"caller"`
}

type NarrativeReference struct {
	Kind       string `json:"kind"`
	TaskID     string `json:"taskId,omitempty"`
	ItemID     string `json:"itemId,omitempty"`
	MessageSeq int64  `json:"messageSeq,omitempty"`
	Revision   int64  `json:"revision,omitempty"`
	ArtifactID string `json:"artifactId,omitempty"`
	Version    int64  `json:"version,omitempty"`
	SourceID   string `json:"sourceId,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Locator    string `json:"locator,omitempty"`
	Label      string `json:"label,omitempty"`
}

type NarrativeArtifactVersion struct {
	ArtifactID        string         `json:"artifactId"`
	TaskID            string         `json:"taskId"`
	ItemID            string         `json:"itemId"`
	Version           int64          `json:"version"`
	NarrativeSeq      int64          `json:"narrativeSeq"`
	Namespace         string         `json:"namespace"`
	SourceID          string         `json:"sourceId"`
	SourceVersion     string         `json:"sourceVersion"`
	Kind              string         `json:"kind"`
	Title             string         `json:"title"`
	OriginalAuthor    Sender         `json:"originalAuthor"`
	SourceTime        *time.Time     `json:"sourceTime,omitempty"`
	IngestedBy        NarrativeActor `json:"ingestedBy"`
	IngestedAt        time.Time      `json:"ingestedAt"`
	Provenance        string         `json:"provenance"`
	CaptureState      string         `json:"captureState"`
	Availability      string         `json:"availability"`
	Content           string         `json:"content,omitempty"`
	ContentDigest     string         `json:"contentDigest,omitempty"`
	SuppliedDigest    string         `json:"suppliedDigest,omitempty"`
	Locator           string         `json:"locator,omitempty"`
	Size              int64          `json:"size,omitempty"`
	SupersedesVersion int64          `json:"supersedesVersion,omitempty"`
}

type NarrativeArtifactSummary struct {
	ArtifactID string                   `json:"artifactId"`
	Namespace  string                   `json:"namespace"`
	SourceID   string                   `json:"sourceId"`
	Latest     NarrativeArtifactVersion `json:"latest"`
}

type PutNarrativeArtifactRequest struct {
	RequestID       string     `json:"requestId"`
	ArtifactID      string     `json:"artifactId,omitempty"`
	ExpectedVersion int64      `json:"expectedVersion"`
	Namespace       string     `json:"namespace"`
	SourceID        string     `json:"sourceId"`
	SourceVersion   string     `json:"sourceVersion"`
	Kind            string     `json:"kind"`
	Title           string     `json:"title"`
	OriginalAuthor  Sender     `json:"originalAuthor"`
	SourceTime      *time.Time `json:"sourceTime,omitempty"`
	AgentID         string     `json:"agentId,omitempty"`
	RunID           string     `json:"runId,omitempty"`
	Provenance      string     `json:"provenance"`
	CaptureState    string     `json:"captureState"`
	Availability    string     `json:"availability"`
	Content         string     `json:"content,omitempty"`
	SuppliedDigest  string     `json:"suppliedDigest,omitempty"`
	Locator         string     `json:"locator,omitempty"`
	Size            int64      `json:"size,omitempty"`
}

type NarrativeArtifactList struct {
	Artifacts  []NarrativeArtifactSummary `json:"artifacts"`
	NextCursor string                     `json:"nextCursor,omitempty"`
}

type NarrativeArtifactVersionList struct {
	Versions   []NarrativeArtifactVersion `json:"versions"`
	NextCursor string                     `json:"nextCursor,omitempty"`
}

type NarrativeLinkVersion struct {
	LinkID          string             `json:"linkId"`
	TaskID          string             `json:"taskId"`
	ItemID          string             `json:"itemId"`
	Revision        int64              `json:"revision"`
	NarrativeSeq    int64              `json:"narrativeSeq"`
	Action          string             `json:"action"`
	Relationship    string             `json:"relationship"`
	Target          NarrativeReference `json:"target"`
	FeatureRevision int64              `json:"featureRevision,omitempty"`
	Reason          string             `json:"reason,omitempty"`
	SourceTime      *time.Time         `json:"sourceTime,omitempty"`
	CreatedBy       NarrativeActor     `json:"createdBy"`
	CreatedAt       time.Time          `json:"createdAt"`
}

type PutNarrativeLinkRequest struct {
	RequestID        string             `json:"requestId"`
	LinkID           string             `json:"linkId,omitempty"`
	ExpectedRevision int64              `json:"expectedRevision"`
	Action           string             `json:"action"`
	Relationship     string             `json:"relationship"`
	Target           NarrativeReference `json:"target"`
	FeatureRevision  int64              `json:"featureRevision,omitempty"`
	Reason           string             `json:"reason,omitempty"`
	SourceTime       *time.Time         `json:"sourceTime,omitempty"`
	AgentID          string             `json:"agentId,omitempty"`
	RunID            string             `json:"runId,omitempty"`
}

type NarrativeLinkList struct {
	Links      []NarrativeLinkVersion `json:"links"`
	NextCursor string                 `json:"nextCursor,omitempty"`
}

type NarrativeCoverageVersion struct {
	CoverageID         string               `json:"coverageId"`
	TaskID             string               `json:"taskId"`
	ItemID             string               `json:"itemId"`
	Revision           int64                `json:"revision"`
	NarrativeSeq       int64                `json:"narrativeSeq"`
	Source             string               `json:"source"`
	Scope              string               `json:"scope"`
	CaptureState       string               `json:"captureState"`
	CapturedIDs        []string             `json:"capturedIds"`
	KnownGaps          []string             `json:"knownGaps"`
	UnknownExtent      bool                 `json:"unknownExtent"`
	AsOf               *time.Time           `json:"asOf,omitempty"`
	Assessment         string               `json:"assessment"`
	AssessmentText     string               `json:"assessmentText,omitempty"`
	EvidenceReferences []NarrativeReference `json:"evidenceReferences"`
	AssessmentBy       Sender               `json:"assessmentBy"`
	CreatedBy          NarrativeActor       `json:"createdBy"`
	CreatedAt          time.Time            `json:"createdAt"`
}

type PutNarrativeCoverageRequest struct {
	RequestID          string               `json:"requestId"`
	CoverageID         string               `json:"coverageId,omitempty"`
	ExpectedRevision   int64                `json:"expectedRevision"`
	Source             string               `json:"source"`
	Scope              string               `json:"scope"`
	CaptureState       string               `json:"captureState"`
	CapturedIDs        []string             `json:"capturedIds"`
	KnownGaps          []string             `json:"knownGaps"`
	UnknownExtent      bool                 `json:"unknownExtent"`
	AsOf               *time.Time           `json:"asOf,omitempty"`
	Assessment         string               `json:"assessment"`
	AssessmentText     string               `json:"assessmentText,omitempty"`
	EvidenceReferences []NarrativeReference `json:"evidenceReferences"`
	AssessedBy         Sender               `json:"assessedBy"`
	AgentID            string               `json:"agentId,omitempty"`
	RunID              string               `json:"runId,omitempty"`
}

type NarrativeCoverageList struct {
	Coverage   []NarrativeCoverageVersion `json:"coverage"`
	NextCursor string                     `json:"nextCursor,omitempty"`
}

type NarrativeReportSections struct {
	RequestedOutcome string `json:"requestedOutcome"`
	DeliveredWork    string `json:"deliveredWork"`
	Verification     string `json:"verification"`
	Limitations      string `json:"limitations"`
	RemainingWork    string `json:"remainingWork"`
}

type NarrativeReportVersion struct {
	ReportID      string                  `json:"reportId"`
	TaskID        string                  `json:"taskId"`
	ItemID        string                  `json:"itemId"`
	Version       int64                   `json:"version"`
	NarrativeSeq  int64                   `json:"narrativeSeq"`
	ScopeRevision int64                   `json:"scopeRevision"`
	Sections      NarrativeReportSections `json:"sections"`
	References    []NarrativeReference    `json:"references"`
	Digest        string                  `json:"digest"`
	CreatedBy     NarrativeActor          `json:"createdBy"`
	CreatedAt     time.Time               `json:"createdAt"`
}

type PutNarrativeReportRequest struct {
	RequestID       string                  `json:"requestId"`
	ReportID        string                  `json:"reportId,omitempty"`
	ExpectedVersion int64                   `json:"expectedVersion"`
	ScopeRevision   int64                   `json:"scopeRevision"`
	Sections        NarrativeReportSections `json:"sections"`
	References      []NarrativeReference    `json:"references"`
	AgentID         string                  `json:"agentId,omitempty"`
	RunID           string                  `json:"runId,omitempty"`
}

type NarrativeReportSummary struct {
	ReportID      string    `json:"reportId"`
	Version       int64     `json:"version"`
	ScopeRevision int64     `json:"scopeRevision"`
	Digest        string    `json:"digest"`
	CreatedAt     time.Time `json:"createdAt"`
}

type NarrativeReportList struct {
	Reports    []NarrativeReportSummary `json:"reports"`
	NextCursor string                   `json:"nextCursor,omitempty"`
}

type NarrativeTimelineEntry struct {
	Seq          int64      `json:"seq"`
	Kind         string     `json:"kind"`
	ObjectID     string     `json:"objectId"`
	Version      int64      `json:"version"`
	Source       string     `json:"source,omitempty"`
	Relationship string     `json:"relationship,omitempty"`
	CaptureState string     `json:"captureState,omitempty"`
	SourceTime   *time.Time `json:"sourceTime,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
}

type NarrativeTimelinePage struct {
	Entries       []NarrativeTimelineEntry `json:"entries"`
	Cursor        string                   `json:"cursor,omitempty"`
	HighWatermark int64                    `json:"highWatermark"`
}

type NarrativeTimelineQuery struct {
	Cursor       string
	Limit        int
	Kind         string
	Source       string
	Relationship string
	CaptureState string
	SourceFrom   string
	SourceTo     string
}

type NarrativeReceipt struct {
	ID        string          `json:"id"`
	RequestID string          `json:"requestId"`
	Operation string          `json:"operation"`
	TaskID    string          `json:"taskId"`
	ItemID    string          `json:"itemId"`
	Result    json.RawMessage `json:"result"`
	CreatedAt time.Time       `json:"createdAt"`
}

type NarrativeOverview struct {
	Item                WorkItem                   `json:"item"`
	History             HistoryCoverage            `json:"history"`
	LatestReport        *NarrativeReportSummary    `json:"latestReport,omitempty"`
	CompletionReport    *NarrativeReportPin        `json:"completionReport,omitempty"`
	LegacyReportMissing bool                       `json:"legacyReportMissing"`
	Coverage            []NarrativeCoverageVersion `json:"coverage"`
	CoverageNextCursor  string                     `json:"coverageNextCursor,omitempty"`
	DefaultGaps         []NarrativeCoverageVersion `json:"defaultGaps"`
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
	Item     WorkItem              `json:"item"`
	Dispatch WorkItemDispatch      `json:"dispatch"`
	Queue    *QueueDispatchReceipt `json:"queue,omitempty"`
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
	// Estimate is the estimate this request saved, on the first save, a
	// replay and a receipt read alike; later saves do not change it.
	Estimate *WorkItemEstimate `json:"estimate,omitempty"`
}

type WorkItemHistoryGapResponse struct {
	Error string     `json:"error"`
	Gap   HistoryGap `json:"gap"`
}

type WorkItemHistoryGapError struct {
	Response WorkItemHistoryGapResponse
}

func (e *WorkItemHistoryGapError) Error() string { return e.Response.Error }
