package api

import (
	"context"
	"net/http"
	"net/url"
)

type VerificationCheck struct {
	ID          string            `json:"id"`
	Argv        []string          `json:"argv"`
	Cwd         string            `json:"cwd"`
	Environment map[string]string `json:"environment"`
}
type VerificationPlan struct {
	ApprovedMatrixDigest      string              `json:"approvedMatrixDigest"`
	MatrixApprovalMessageSeq  int64               `json:"matrixApprovalMessageSeq"`
	ItemID                    string              `json:"itemId"`
	ItemTaskID                string              `json:"itemTaskId"`
	AssignmentOwnershipDigest string              `json:"assignmentOwnershipDigest"`
	Version                   int                 `json:"version"`
	OperationKey              string              `json:"operationKey"`
	Repository                string              `json:"repository"`
	BaseCommit                string              `json:"baseCommit"`
	Commit                    string              `json:"commit"`
	ItemRevision              int64               `json:"itemRevision"`
	ScopeRevision             int64               `json:"scopeRevision"`
	OrderMessageSeq           int64               `json:"orderMessageSeq"`
	AssignmentSeq             int64               `json:"assignmentSeq"`
	BuilderAgentID            string              `json:"builderAgentId"`
	BuilderRunID              string              `json:"builderRunId"`
	VerifierAgentID           string              `json:"verifierAgentId"`
	VerifierRunID             string              `json:"verifierRunId"`
	MatrixDigest              string              `json:"matrixDigest"`
	ChecksDigest              string              `json:"checksDigest"`
	Owned                     []string            `json:"owned"`
	Changed                   []string            `json:"changed"`
	Checks                    []VerificationCheck `json:"checks"`
}
type VerificationResult struct {
	VerificationCheck
	StartedAt     string `json:"startedAt"`
	EndedAt       string `json:"endedAt"`
	DurationMs    int64  `json:"durationMs"`
	ExitCode      int    `json:"exitCode"`
	FailureReason string `json:"failureReason,omitempty"`
	LogURI        string `json:"logURI"`
	LogDigest     string `json:"logDigest"`
}
type VerificationPrerequisite struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
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
	RequestID          string               `json:"requestId"`
	AgentID            string               `json:"agentId"`
	RunID              string               `json:"runId"`
	ExpectedGeneration int64                `json:"expectedGeneration"`
	Plan               *VerificationPlan    `json:"plan,omitempty"`
	Receipt            *VerificationReceipt `json:"receipt,omitempty"`
}
type VerificationRecord struct {
	ItemID         string               `json:"itemId"`
	ItemTaskID     string               `json:"itemTaskId"`
	Generation     int64                `json:"generation"`
	Kind           string               `json:"kind"`
	RequestID      string               `json:"requestId"`
	Digest         string               `json:"digest"`
	HandlerAgentID string               `json:"handlerAgentId"`
	HandlerRunID   string               `json:"handlerRunId"`
	CreatedAt      string               `json:"createdAt"`
	Plan           *VerificationPlan    `json:"plan,omitempty"`
	Receipt        *VerificationReceipt `json:"receipt,omitempty"`
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
