package api

import (
	"context"
	"net/url"
)

const AgentRoleDeployment = "deployment_agent"

// ReleaseJob is a handler-owned snapshot. A deployer never reads work items.
type ReleaseJob struct {
	ID                     string                  `json:"id"`
	TaskID                 string                  `json:"taskId"`
	EntryID                string                  `json:"entryId"`
	ItemID                 string                  `json:"itemId"`
	ItemRevision           int64                   `json:"itemRevision"`
	ScopeRevision          int64                   `json:"scopeRevision"`
	OrderMessageSeq        int64                   `json:"orderMessageSeq"`
	Repository             string                  `json:"repository"`
	BaseCommit             string                  `json:"baseCommit"`
	Commit                 string                  `json:"commit"`
	VerificationDigest     string                  `json:"verificationDigest"`
	Plan                   VerificationPlan        `json:"plan"`
	State                  string                  `json:"state"`
	Generation             int64                   `json:"generation"`
	AgentID                string                  `json:"agentId,omitempty"`
	RunID                  string                  `json:"runId,omitempty"`
	PauseGeneration        int64                   `json:"pauseGeneration"`
	IntegratedCommit       string                  `json:"integratedCommit,omitempty"`
	IntegratedPlan         *VerificationPlan       `json:"integratedPlan,omitempty"`
	IntegratedCoverage     []ReleaseCheckCoverage  `json:"integratedCoverage,omitempty"`
	IntegratedMatrix       *ReleaseMatrixChange    `json:"integratedMatrix,omitempty"`
	IntegratedVerification *VerificationReceipt    `json:"integratedVerification,omitempty"`
	InputsCommit           string                  `json:"inputsCommit,omitempty"`
	InputsDigest           string                  `json:"inputsDigest,omitempty"`
	Reconciliations        []ReleaseReconciliation `json:"reconciliations,omitempty"`
	Receipt                *ReleaseReceipt         `json:"receipt,omitempty"`
	// Published survives a later block: tasks-hub already carries the release.
	Published    bool                 `json:"published,omitempty"`
	Supersession *ReleaseSupersession `json:"supersession,omitempty"`
	// MatrixApprovals is derived when jobs are listed and never saved: the
	// project's owner matrix approvals, for a claimed job only. It is a hint
	// for the runner; the verification import proves the one it cites.
	MatrixApprovals []ReleaseMatrixApproval `json:"matrixApprovals,omitempty"`
}

// ReleaseMatrixChange records an integrated verification bound to a matrix
// digest other than the job's approved one, and the owner approval message
// that covers it. The job's own plan is unchanged.
type ReleaseMatrixChange struct {
	ApprovedDigest     string `json:"approvedDigest"`
	IntegratedDigest   string `json:"integratedDigest"`
	ApprovalMessageSeq int64  `json:"approvalMessageSeq"`
}

// ReleaseMatrixApproval is one owner approval message of a matrix digest.
type ReleaseMatrixApproval struct {
	Digest     string `json:"digest"`
	MessageSeq int64  `json:"messageSeq"`
}

// ReleaseCheckCoverage records an approved check that the integrated plan
// covered with a wider check instead of the exact one (exact matches are not
// listed). Relation "superset": the integrated go-race tested every approved
// package and more. Relation "matrix_changed": under an owner-approved matrix
// change, the integrated check with the same ID (for go-race, still testing
// every approved package).
type ReleaseCheckCoverage struct {
	CheckID          string `json:"checkId"`
	ApprovedDigest   string `json:"approvedDigest"`
	IntegratedDigest string `json:"integratedDigest"`
	Relation         string `json:"relation"`
}

// ReleaseSupersession closes a verified, never-claimed job whose change the
// owner session already released by hand, so the deployer never replays it.
type ReleaseSupersession struct {
	ReleasedCommit string `json:"releasedCommit"`
	Release        string `json:"release"`
	AgentID        string `json:"agentId"`
	RunID          string `json:"runId"`
}

// Only nonsecret identities and hashes belong in receipts; arbitrary output
// and environment maps are deliberately absent.
type ReleaseTargetReceipt struct {
	Target                 string `json:"target"`
	Release                string `json:"release"`
	ArtifactSHA256         string `json:"artifactSHA256"`
	Backup                 string `json:"backup,omitempty"`
	PreflightReceiptSHA256 string `json:"preflightReceiptSHA256,omitempty"`
	BackupSHA256           string `json:"backupSHA256,omitempty"`
	Version                string `json:"version,omitempty"`
	Deployment             string `json:"deployment,omitempty"`
	Outcome                string `json:"outcome"`
	Rollback               string `json:"rollback,omitempty"`
}
type ReleaseReceipt struct {
	Version            int                    `json:"version"`
	JobID              string                 `json:"jobId"`
	Commit             string                 `json:"commit"`
	VerificationDigest string                 `json:"verificationDigest"`
	Targets            []ReleaseTargetReceipt `json:"targets"`
	Outcome            string                 `json:"outcome"`
	EscalationSeq      int64                  `json:"escalationSeq,omitempty"`
	Revert             *ReleaseRevert         `json:"revert,omitempty"`
	Push               *ReleasePush           `json:"push,omitempty"`
}

// ReleaseRevert records the compare-and-swap revert of a rolled-back release
// on tasks-hub and the handler request filing its bug.
type ReleaseRevert struct {
	Commit       string `json:"commit,omitempty"`
	Outcome      string `json:"outcome"`
	BugRequestID string `json:"bugRequestId,omitempty"`
}

// ReleasePush records the fast-forward push of a live-verified release.
type ReleasePush struct {
	Remote  string `json:"remote"`
	Commit  string `json:"commit"`
	Outcome string `json:"outcome"`
}

// Recovery is a handler inspection record, never a timeout-based takeover.
// Hashes reference private incident/journal evidence without exposing outputs.
type ReleaseReconciliation struct {
	LastActionAt           string `json:"lastActionAt"`
	StoppedAt              string `json:"stoppedAt"`
	ExpectedNextAction     string `json:"expectedNextAction"`
	ContributingConditions string `json:"contributingConditions"`
	UnresolvedQuestions    string `json:"unresolvedQuestions"`
	LockDigest             string `json:"lockDigest,omitempty"`

	JobID                  string `json:"jobId"`
	AgentID                string `json:"agentId"`
	RunID                  string `json:"runId"`
	PauseGeneration        int64  `json:"pauseGeneration"`
	Disposition            string `json:"disposition"`
	IncidentBugID          string `json:"incidentBugId"`
	IncidentDigest         string `json:"incidentDigest"`
	JournalDigest          string `json:"journalDigest"`
	ObservedAt             string `json:"observedAt"`
	StopReason             string `json:"stopReason"`
	LastAction             string `json:"lastAction"`
	CausalEvidence         string `json:"causalEvidence"`
	PreventionOwner        string `json:"preventionOwner"`
	PreventionItemID       string `json:"preventionItemId"`
	PreventionOrderMessage int64  `json:"preventionOrderMessage"`
	PreventionCriterion    string `json:"preventionCriterion"`
	JournalState           string `json:"journalState"`
	NoActiveExecution      bool   `json:"noActiveExecution"`
	NoPublication          bool   `json:"noPublication"`
	RefResolved            bool   `json:"refResolved"`
}
type ReleaseRequest struct {
	RequestID          string                 `json:"requestId"`
	Operation          string                 `json:"operation"`
	AgentID            string                 `json:"agentId"`
	RunID              string                 `json:"runId"`
	EntryID            string                 `json:"entryId,omitempty"`
	JobID              string                 `json:"jobId,omitempty"`
	ExpectedGeneration int64                  `json:"expectedGeneration"`
	IntegratedCommit   string                 `json:"integratedCommit,omitempty"`
	InputsDigest       string                 `json:"inputsDigest,omitempty"`
	Reconciliation     *ReleaseReconciliation `json:"reconciliation,omitempty"`
	Plan               *VerificationPlan      `json:"plan,omitempty"`
	Verification       *VerificationReceipt   `json:"verification,omitempty"`
	Receipt            *ReleaseReceipt        `json:"receipt,omitempty"`
	Supersession       *ReleaseSupersession   `json:"supersession,omitempty"`
}

func (c *Client) ReleaseAction(ctx context.Context, task string, req ReleaseRequest) (ReleaseJob, error) {
	var out ReleaseJob
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/releases/actions", req, &out)
	return out, err
}

// ReleaseHandler resolves the project database handler for the exact deployer run.
func (c *Client) ReleaseHandler(ctx context.Context, task, agent, run string) (Agent, error) {
	var out Agent
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/releases/actions", ReleaseRequest{Operation: "handler", AgentID: agent, RunID: run}, &out)
	return out, err
}
func (c *Client) Releases(ctx context.Context, task string) ([]ReleaseJob, error) {
	out := []ReleaseJob{}
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/releases", nil, &out)
	return out, err
}
