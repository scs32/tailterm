package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
)

type VerificationCheck struct {
	ID          string            `json:"id"`
	Argv        []string          `json:"argv"`
	Cwd         string            `json:"cwd"`
	Environment map[string]string `json:"environment"`
}
type VerificationKnownFailure struct {
	CheckID   string `json:"checkId"`
	BugTaskID string `json:"bugTaskId"`
	BugID     string `json:"bugId"`
}
type VerificationAttempt struct {
	Attempt       int    `json:"attempt"`
	StartedAt     string `json:"startedAt"`
	EndedAt       string `json:"endedAt"`
	DurationMs    int64  `json:"durationMs"`
	ExitCode      int    `json:"exitCode"`
	FailureReason string `json:"failureReason,omitempty"`
	LogURI        string `json:"logURI"`
	LogDigest     string `json:"logDigest"`
}
type VerificationPlan struct {
	MaxAttempts               int                        `json:"maxAttempts,omitempty"`
	KnownFailures             []VerificationKnownFailure `json:"knownFailures,omitempty"`
	ApprovedMatrixDigest      string                     `json:"approvedMatrixDigest"`
	MatrixApprovalMessageSeq  int64                      `json:"matrixApprovalMessageSeq"`
	ItemID                    string                     `json:"itemId"`
	ItemTaskID                string                     `json:"itemTaskId"`
	AssignmentOwnershipDigest string                     `json:"assignmentOwnershipDigest"`
	Version                   int                        `json:"version"`
	OperationKey              string                     `json:"operationKey"`
	Repository                string                     `json:"repository"`
	BaseCommit                string                     `json:"baseCommit"`
	Commit                    string                     `json:"commit"`
	ItemRevision              int64                      `json:"itemRevision"`
	ScopeRevision             int64                      `json:"scopeRevision"`
	OrderMessageSeq           int64                      `json:"orderMessageSeq"`
	AssignmentSeq             int64                      `json:"assignmentSeq"`
	BuilderAgentID            string                     `json:"builderAgentId"`
	BuilderRunID              string                     `json:"builderRunId"`
	VerifierAgentID           string                     `json:"verifierAgentId"`
	VerifierRunID             string                     `json:"verifierRunId"`
	MatrixDigest              string                     `json:"matrixDigest"`
	ChecksDigest              string                     `json:"checksDigest"`
	Owned                     []string                   `json:"owned"`
	Changed                   []string                   `json:"changed"`
	Checks                    []VerificationCheck        `json:"checks"`
}
type VerificationResult struct {
	Attempts     []VerificationAttempt `json:"attempts,omitempty"`
	Status       string                `json:"status,omitempty"`
	KnownFailure bool                  `json:"knownFailure,omitempty"`
	NowPassing   bool                  `json:"nowPassing,omitempty"`
	VerificationCheck
	StartedAt     string `json:"startedAt"`
	EndedAt       string `json:"endedAt"`
	DurationMs    int64  `json:"durationMs"`
	ExitCode      int    `json:"exitCode"`
	FailureReason string `json:"failureReason,omitempty"`
	LogURI        string `json:"logURI"`
	LogDigest     string `json:"logDigest"`
	// Wasm lists the WASM files a reader check loaded, hashed before it ran.
	Wasm []VerificationFileDigest `json:"wasm,omitempty"`
}
type VerificationFileDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// VerificationPrerequisite fields after SHA256 are provenance: a WASM file
// rebuilt by a check keeps its pre-run digest and names that check in Source;
// a test binary names the hub tree it was built from and its build.
type VerificationPrerequisite struct {
	Path             string                          `json:"path"`
	SHA256           string                          `json:"sha256"`
	PriorSHA256      string                          `json:"priorSha256,omitempty"`
	Source           *VerificationPrerequisiteSource `json:"source,omitempty"`
	Target           string                          `json:"target,omitempty"`
	HistoricalCommit string                          `json:"historicalCommit,omitempty"`
	BuildCommand     []string                        `json:"buildCommand,omitempty"`
	StartedAt        string                          `json:"startedAt,omitempty"`
	EndedAt          string                          `json:"endedAt,omitempty"`
}

// VerificationPrerequisiteSource is a JSON string naming the step that wrote
// the file (Step), or a JSON object {root, sha256} naming the source tree a
// binary was built from; root is null for a tree outside any checkout.
type VerificationPrerequisiteSource struct {
	Step   string
	Root   string
	SHA256 string
}

func (s VerificationPrerequisiteSource) MarshalJSON() ([]byte, error) {
	if s.Step != "" {
		return json.Marshal(s.Step)
	}
	tree := struct {
		Root   *string `json:"root"`
		SHA256 string  `json:"sha256"`
	}{SHA256: s.SHA256}
	if s.Root != "" {
		tree.Root = &s.Root
	}
	return json.Marshal(tree)
}

func (s *VerificationPrerequisiteSource) UnmarshalJSON(b []byte) error {
	*s = VerificationPrerequisiteSource{}
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &s.Step)
	}
	if len(b) == 0 || b[0] != '{' {
		return errors.New("verification prerequisite source must be a string or an object")
	}
	var tree struct {
		Root   *string `json:"root"`
		SHA256 string  `json:"sha256"`
	}
	if err := json.Unmarshal(b, &tree); err != nil {
		return err
	}
	if tree.Root != nil {
		s.Root = *tree.Root
	}
	s.SHA256 = tree.SHA256
	return nil
}

type AIVBinding struct {
	State      string `json:"state"`
	Service    string `json:"service,omitempty"`
	Repository string `json:"repository,omitempty"`
	Snapshot   string `json:"snapshot,omitempty"`
	Extractor  string `json:"extractor,omitempty"`
	Run        string `json:"run,omitempty"`
	Audit      string `json:"audit,omitempty"`
}
type VerificationReceipt struct {
	Worktree        string                     `json:"worktree"`
	Version         int                        `json:"version"`
	OperationKey    string                     `json:"operationKey"`
	PlanDigest      string                     `json:"planDigest"`
	Repository      string                     `json:"repository"`
	BaseCommit      string                     `json:"baseCommit"`
	Commit          string                     `json:"commit"`
	MatrixDigest    string                     `json:"matrixDigest"`
	ChecksDigest    string                     `json:"checksDigest"`
	VerifierAgentID string                     `json:"verifierAgentId"`
	VerifierRunID   string                     `json:"verifierRunId"`
	Detached        bool                       `json:"detached"`
	CleanBefore     bool                       `json:"cleanBefore"`
	CleanAfter      bool                       `json:"cleanAfter"`
	Environment     map[string]string          `json:"environment"`
	Prerequisites   []VerificationPrerequisite `json:"prerequisites"`
	Checks          []VerificationResult       `json:"checks"`
	AIV             AIVBinding                 `json:"aiv"`
}
type VerificationRequest struct {
	RequestID          string                 `json:"requestId"`
	AgentID            string                 `json:"agentId"`
	RunID              string                 `json:"runId"`
	ExpectedGeneration int64                  `json:"expectedGeneration"`
	Plan               *VerificationPlan      `json:"plan,omitempty"`
	Receipt            *VerificationReceipt   `json:"receipt,omitempty"`
	CarryOver          *VerificationCarryOver `json:"carryOver,omitempty"`
	// ContextDigest is the caller run's admitted context digest. An item
	// lead or verifier must send it; a database handler has no binding and
	// sends none.
	ContextDigest string `json:"contextDigest,omitempty"`
}

// VerificationCarryOver rebinds the current passing receipt to the scope an
// owner edit produced when that edit only moved verification-owned criteria
// to open linked items. The caller states what it saw: the source receipt,
// its exact commit, approved matrix and checks, both scope revisions, the
// queue rebind and owner message of the edit, and each moved criterion. The
// hub checks every field, and on the saved record fills AmendedBy from the
// rebind and the derived Plan and Receipt: the source pair with only the
// scope and item revisions and the receipt's plan digest moved. A request
// that sets them is refused.
type VerificationCarryOver struct {
	ItemRevision             int64                        `json:"itemRevision"`
	SourceGeneration         int64                        `json:"sourceGeneration"`
	SourceReceiptDigest      string                       `json:"sourceReceiptDigest"`
	SourcePlanDigest         string                       `json:"sourcePlanDigest"`
	Commit                   string                       `json:"commit"`
	MatrixDigest             string                       `json:"matrixDigest"`
	MatrixApprovalMessageSeq int64                        `json:"matrixApprovalMessageSeq"`
	ChecksDigest             string                       `json:"checksDigest"`
	FromScopeRevision        int64                        `json:"fromScopeRevision"`
	ToScopeRevision          int64                        `json:"toScopeRevision"`
	RebindID                 string                       `json:"rebindId"`
	OwnerEditMessageSeq      int64                        `json:"ownerEditMessageSeq"`
	Moved                    []VerificationMovedCriterion `json:"moved"`
	AmendedBy                *Sender                      `json:"amendedBy,omitempty"`
	Plan                     *VerificationPlan            `json:"plan,omitempty"`
	Receipt                  *VerificationReceipt         `json:"receipt,omitempty"`
}

// VerificationMovedCriterion is one verification-owned criterion the owner
// edit moved off the item: its ID and text in the source scope, and the open
// item that now carries it, at that item's revision when it was moved.
type VerificationMovedCriterion struct {
	ID           string `json:"id"`
	Text         string `json:"text"`
	ItemTaskID   string `json:"itemTaskId"`
	ItemID       string `json:"itemId"`
	ItemRevision int64  `json:"itemRevision"`
}

// VerificationNotice names one dependent actor a receipt import told, or the
// reason it was not told.
type VerificationNotice struct {
	Role       string `json:"role"`
	AgentID    string `json:"agentId"`
	MessageSeq int64  `json:"messageSeq,omitempty"`
	Skipped    string `json:"skipped,omitempty"`
}
type VerificationRecord struct {
	ItemID         string                 `json:"itemId"`
	ItemTaskID     string                 `json:"itemTaskId"`
	Generation     int64                  `json:"generation"`
	Kind           string                 `json:"kind"`
	RequestID      string                 `json:"requestId"`
	Digest         string                 `json:"digest"`
	HandlerAgentID string                 `json:"handlerAgentId"`
	HandlerRunID   string                 `json:"handlerRunId"`
	CreatedAt      string                 `json:"createdAt"`
	Plan           *VerificationPlan      `json:"plan,omitempty"`
	Receipt        *VerificationReceipt   `json:"receipt,omitempty"`
	CarryOver      *VerificationCarryOver `json:"carryOver,omitempty"`
	// HandlerAgentID and HandlerRunID hold the author. AuthorRole is lead or
	// verifier when the author is not a database handler. Notices lists who
	// a receipt import told.
	AuthorRole string               `json:"authorRole,omitempty"`
	Notices    []VerificationNotice `json:"notices,omitempty"`
}

func (c *Client) SaveVerification(ctx context.Context, task, item string, req VerificationRequest) (VerificationRecord, error) {
	var out VerificationRecord
	err := c.do(ctx, http.MethodPost, "/v1/tasks/"+task+"/work-items/"+item+"/verification", req, &out)
	return out, err
}
func (c *Client) VerificationHistory(ctx context.Context, task, item, agent, run string) ([]VerificationRecord, error) {
	var out []VerificationRecord
	err := c.do(ctx, http.MethodGet, "/v1/tasks/"+task+"/work-items/"+item+"/verification?agent="+url.QueryEscape(agent)+"&run="+url.QueryEscape(run), nil, &out)
	return out, err
}

type VerificationEnrollment struct {
	AgentID    string `json:"agentId,omitempty"`
	RunID      string `json:"runId,omitempty"`
	Required   bool   `json:"required"`
	Provenance string `json:"provenance"`
	CreatedAt  string `json:"createdAt,omitempty"`
}

func (c *Client) VerificationEnrollment(ctx context.Context, task, item, agent, run string) ([]VerificationEnrollment, error) {
	var out []VerificationEnrollment
	err := c.do(ctx, http.MethodGet, "/v1/tasks/"+task+"/work-items/"+item+"/verification/enrollment?agent="+url.QueryEscape(agent)+"&run="+url.QueryEscape(run), nil, &out)
	return out, err
}

// Summary is derived from the current plan; superseded evidence is never reused.
// CarryOverGeneration is set when the current pair was carried over an owner
// scope edit, and names that record.
type VerificationSummary struct {
	State               string                     `json:"state"`
	Commit              string                     `json:"commit"`
	Checks              []VerificationCheckSummary `json:"checks,omitempty"`
	CarryOverGeneration int64                      `json:"carryOverGeneration,omitempty"`
}
type VerificationCheckSummary struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	KnownFailure bool   `json:"knownFailure,omitempty"`
	NowPassing   bool   `json:"nowPassing,omitempty"`
}
