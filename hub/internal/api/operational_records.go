package api

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

const OperationalRecordsCapabilityVersion = 1

type OperationalReference struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}
type OperationalArtifact struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Digest  string `json:"digest"`
}
type OperationalSource struct {
	Message    MessageReference `json:"message"`
	TextDigest string           `json:"textDigest"`
}
type OperationalSourceEvidence struct {
	OperationalSource
	Author Sender `json:"author"`
	Text   string `json:"text"`
}
type OperationalCriterion struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}
type OperationalInstruction struct {
	Objective string                 `json:"objective"`
	Scope     []string               `json:"scope"`
	Criteria  []OperationalCriterion `json:"criteria"`
}
type OperationalFinding struct {
	Instruction  OperationalReference `json:"instruction"`
	Observation  string               `json:"observation"`
	Reproduction []string             `json:"reproduction"`
}
type OperationalCandidate struct {
	Instruction OperationalReference   `json:"instruction"`
	Findings    []OperationalReference `json:"findings"`
	Commit      string                 `json:"commit"`
	Artifact    OperationalArtifact    `json:"artifact"`
}
type OperationalCandidateEvidence struct {
	Version int                        `json:"version"`
	Commit  string                     `json:"commit"`
	Files   []OperationalCandidateFile `json:"files"`
}
type OperationalCandidateFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}
type OperationalVerification struct {
	Candidate   OperationalReference `json:"candidate"`
	CriterionID string               `json:"criterionId"`
	TestID      string               `json:"testId"`
	Command     []string             `json:"command"`
	Outcome     string               `json:"outcome"`
	Evidence    OperationalArtifact  `json:"evidence"`
}

// OperationalTestEvidence is the schema of the immutable evidence artifact.
// Validation proves its association and claimed outcome, not external execution.
type OperationalTestEvidence struct {
	Version         int                  `json:"version"`
	Candidate       OperationalReference `json:"candidate"`
	Commit          string               `json:"commit"`
	CandidateDigest string               `json:"candidateDigest"`
	CriterionID     string               `json:"criterionId"`
	TestID          string               `json:"testId"`
	Command         []string             `json:"command"`
	Outcome         string               `json:"outcome"`
	AgentID         string               `json:"agentId"`
	RunID           string               `json:"runId"`
}
type OperationalResult struct {
	Candidate     OperationalReference   `json:"candidate"`
	Verifications []OperationalReference `json:"verifications"`
	Summary       string                 `json:"summary"`
}
type OperationalAcceptance struct {
	Result   OperationalReference `json:"result"`
	Decision string               `json:"decision"`
	Reason   string               `json:"reason"`
}

// Missing proposal fields remain zero/null. Only CommitOperationalRecord can
// grant authority after transactional reference/identity/state validation.
type OperationalData struct {
	Kind          string                   `json:"kind"`
	ItemTaskID    string                   `json:"itemTaskId"`
	ItemID        string                   `json:"itemId"`
	ItemRevision  int64                    `json:"itemRevision"`
	ScopeRevision int64                    `json:"scopeRevision"`
	DeliveryID    string                   `json:"deliveryId"`
	Generation    int64                    `json:"generation"`
	Epoch         int64                    `json:"epoch"`
	Sources       []OperationalSource      `json:"sources"`
	Instruction   *OperationalInstruction  `json:"instruction,omitempty"`
	Finding       *OperationalFinding      `json:"finding,omitempty"`
	Candidate     *OperationalCandidate    `json:"candidate,omitempty"`
	Verification  *OperationalVerification `json:"verification,omitempty"`
	Result        *OperationalResult       `json:"result,omitempty"`
	Acceptance    *OperationalAcceptance   `json:"acceptance,omitempty"`
}
type OperationalRecord struct {
	ID           string                      `json:"id"`
	TaskID       string                      `json:"taskId"`
	Version      int64                       `json:"version"`
	State        string                      `json:"state"`
	Data         OperationalData             `json:"data"`
	Sources      []OperationalSourceEvidence `json:"sources"`
	ActorAgentID string                      `json:"actorAgentId"`
	ActorRunID   string                      `json:"actorRunId"`
	By           Caller                      `json:"by"`
	CreatedAt    time.Time                   `json:"createdAt"`
}
type ProposeOperationalRecordRequest struct {
	RequestID       string          `json:"requestId"`
	RecordID        string          `json:"recordId,omitempty"`
	ExpectedVersion int64           `json:"expectedVersion"`
	AgentID         string          `json:"agentId,omitempty"`
	RunID           string          `json:"runId,omitempty"`
	Data            OperationalData `json:"data"`
}
type CommitOperationalRecordRequest struct {
	RequestID       string `json:"requestId"`
	ExpectedVersion int64  `json:"expectedVersion"`
	AgentID         string `json:"agentId,omitempty"`
	RunID           string `json:"runId,omitempty"`
}
type OperationalMutation struct {
	Record  OperationalRecord `json:"record"`
	Event   *DeliveryEvent    `json:"event,omitempty"`
	Receipt DeliveryReceipt   `json:"receipt"`
	Replay  bool              `json:"replay"`
}

func (c *Client) ProposeOperationalRecord(ctx context.Context, task string, req ProposeOperationalRecordRequest) (OperationalMutation, error) {
	var out OperationalMutation
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/operational-records", req, &out)
	return out, err
}
func (c *Client) CommitOperationalRecord(ctx context.Context, task, id string, req CommitOperationalRecordRequest) (OperationalMutation, error) {
	var out OperationalMutation
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/operational-records/"+url.PathEscape(id)+"/commit", req, &out)
	return out, err
}
func (c *Client) GetOperationalRecord(ctx context.Context, task, id string, version int64) (OperationalRecord, error) {
	var out OperationalRecord
	path := "/v1/tasks/" + url.PathEscape(task) + "/operational-records/" + url.PathEscape(id)
	if version > 0 {
		path += "?version=" + strconv.FormatInt(version, 10)
	}
	err := c.do(ctx, "GET", path, nil, &out)
	return out, err
}
