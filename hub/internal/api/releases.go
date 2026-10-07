package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
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
	// RetryOf links a job the handler created by retry to the refused or
	// rolled-back job of the same entry it follows; that job is never rewritten.
	RetryOf *ReleaseRetry `json:"retryOf,omitempty"`
	// SettledAt orders released and superseded jobs for target baselines:
	// the hub time of the final receipt, or the hand release's record time.
	SettledAt string `json:"settledAt,omitempty"`
	// Hold is set only while the job is in state held (an owner hold);
	// HoldHistory keeps every hold and release of the job and is never rewritten.
	Hold        *ReleaseHold       `json:"hold,omitempty"`
	HoldHistory []ReleaseHoldEvent `json:"holdHistory,omitempty"`
	// MatrixApprovals is derived on exact detail reads and never saved: the
	// project's owner matrix approvals, for a claimed job only. It is a hint
	// for the runner; the verification import proves the one it cites.
	MatrixApprovals []ReleaseMatrixApproval `json:"matrixApprovals,omitempty"`
}

// ReleaseSummary is an explicit projection; heavy evidence is available only by ID.
type ReleaseSummary struct {
	Summary            bool                        `json:"summary"`
	RowID              int64                       `json:"rowId"`
	ID                 string                      `json:"id"`
	TaskID             string                      `json:"taskId"`
	EntryID            string                      `json:"entryId"`
	ItemID             string                      `json:"itemId"`
	ItemRevision       int64                       `json:"itemRevision"`
	ScopeRevision      int64                       `json:"scopeRevision"`
	OrderMessageSeq    int64                       `json:"orderMessageSeq"`
	BaseCommit         string                      `json:"baseCommit"`
	Commit             string                      `json:"commit"`
	VerificationDigest string                      `json:"verificationDigest"`
	State              string                      `json:"state"`
	Generation         int64                       `json:"generation"`
	AgentID            string                      `json:"agentId,omitempty"`
	RunID              string                      `json:"runId,omitempty"`
	PauseGeneration    int64                       `json:"pauseGeneration"`
	IntegratedCommit   string                      `json:"integratedCommit,omitempty"`
	InputsCommit       string                      `json:"inputsCommit,omitempty"`
	InputsDigest       string                      `json:"inputsDigest,omitempty"`
	Published          bool                        `json:"published,omitempty"`
	SettledAt          string                      `json:"settledAt,omitempty"`
	Receipt            *ReleaseSummaryReceipt      `json:"receipt,omitempty"`
	Supersession       *ReleaseSummarySupersession `json:"supersession,omitempty"`
	Hold               *ReleaseHold                `json:"hold,omitempty"`
}
type ReleaseSummaryReceipt struct {
	Outcome string                 `json:"outcome"`
	Commit  string                 `json:"commit"`
	Targets []ReleaseSummaryTarget `json:"targets"`
}
type ReleaseSummaryTarget struct {
	Target  string `json:"target"`
	Outcome string `json:"outcome"`
}
type ReleaseSummarySupersession struct {
	ReleasedCommit string   `json:"releasedCommit"`
	Targets        []string `json:"targets"`
}
type ReleaseListOptions struct {
	View     string
	Limit    int
	After    string
	Snapshot string
}
type ReleasePage struct {
	Version int              `json:"version"`
	Jobs    []ReleaseSummary `json:"jobs"`
	Page    ReleasePageInfo  `json:"page"`
}
type ReleasePageInfo struct {
	View      string `json:"view"`
	Limit     int    `json:"limit"`
	Snapshot  string `json:"snapshot"`
	NextAfter string `json:"nextAfter"`
}

func (c *Client) ReleasesPage(ctx context.Context, task string, opts ReleaseListOptions) (ReleasePage, error) {
	q := url.Values{}
	if opts.View != "" {
		q.Set("view", opts.View)
	}
	if opts.Limit != 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.After != "" {
		q.Set("after", opts.After)
	}
	if opts.Snapshot != "" {
		q.Set("snapshot", opts.Snapshot)
	}
	var raw json.RawMessage
	if err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/releases?"+q.Encode(), nil, &raw); err != nil {
		return ReleasePage{}, err
	}
	return decodeReleasePage(raw, task, opts)
}

func decodeReleasePage(raw json.RawMessage, task string, opts ReleaseListOptions) (ReleasePage, error) {
	var out ReleasePage
	var wire struct {
		Version int              `json:"version"`
		Jobs    []ReleaseSummary `json:"jobs"`
		Page    *struct {
			View      string  `json:"view"`
			Limit     int     `json:"limit"`
			Snapshot  string  `json:"snapshot"`
			NextAfter *string `json:"nextAfter"`
		} `json:"page"`
	}
	err := json.Unmarshal(raw, &wire)
	if err != nil {
		return out, err
	}
	if wire.Page == nil || wire.Page.NextAfter == nil {
		return out, fmt.Errorf("invalid release page metadata")
	}
	out = ReleasePage{Version: wire.Version, Jobs: wire.Jobs, Page: ReleasePageInfo{View: wire.Page.View, Limit: wire.Page.Limit, Snapshot: wire.Page.Snapshot, NextAfter: *wire.Page.NextAfter}}
	view := opts.View
	if view == "" {
		view = "active"
	}
	limit := opts.Limit
	if limit == 0 {
		limit = 50
	}
	token, tokenErr := hex.DecodeString(out.Page.Snapshot)
	if err == nil && (out.Version != 1 || out.Jobs == nil || out.Page.View != view || out.Page.Limit != limit || out.Page.Limit < 1 || out.Page.Limit > 200 || len(out.Jobs) > out.Page.Limit || tokenErr != nil || len(token) != 32 || (len(out.Jobs) == 0 && out.Page.NextAfter != "") || (opts.After != "" && out.Page.NextAfter == opts.After) || (opts.Snapshot != "" && out.Page.Snapshot != opts.Snapshot)) {
		err = fmt.Errorf("invalid release page")
	}
	previous := int64(0)
	seen := map[string]bool{}
	for _, j := range out.Jobs {
		settled := j.State == "released" || j.State == "rolled_back" || j.State == "refused" || j.State == "superseded"
		if !j.Summary || j.ID == "" || j.TaskID != task || j.RowID <= previous || seen[j.ID] || j.State == "" || settled != (view == "settled") {
			err = fmt.Errorf("invalid release summary")
		}
		previous = j.RowID
		seen[j.ID] = true
	}
	return out, err
}
func (c *Client) Release(ctx context.Context, task, id string) (ReleaseJob, error) {
	var wire struct {
		ReleaseJob
		Summary bool `json:"summary"`
	}
	err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/releases?job="+url.QueryEscape(id), nil, &wire)
	if err == nil && (wire.Summary || wire.ID != id || wire.TaskID != task) {
		err = fmt.Errorf("invalid release detail")
	}
	return wire.ReleaseJob, err
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

// ReleaseSupersession closes a job whose change the owner session already
// released by hand, so the deployer never replays it: a verified job no
// deployer holds (never claimed, or only set aside), or a refused or
// rolled-back job with no effects left live. It cites the recorded hand
// release; the hub copies that record's release name and targets. A receipt
// and reconciliations the job already carries stay in its record.
type ReleaseSupersession struct {
	ReleasedCommit string   `json:"releasedCommit"`
	Release        string   `json:"release"`
	HandReleaseID  string   `json:"handReleaseId,omitempty"`
	Targets        []string `json:"targets,omitempty"`
	AgentID        string   `json:"agentId"`
	RunID          string   `json:"runId"`
}

// ReleaseRestoredTarget is the handler's statement that a target the earlier
// job touched runs this release again.
type ReleaseRestoredTarget struct {
	Target  string `json:"target"`
	Release string `json:"release"`
}

// ReleaseRetry is the handler's retry of an accepted entry whose latest job
// ended refused or rolled back with nothing left live. A request supplies
// Reason and Restored (one entry per target in the earlier job's receipt);
// the hub fills the rest when it saves the new job's RetryOf: the earlier
// job, its generation and state, the retrying handler run and the attempt
// number among the entry's jobs.
type ReleaseRetry struct {
	JobID      string                  `json:"jobId,omitempty"`
	Generation int64                   `json:"generation,omitempty"`
	State      string                  `json:"state,omitempty"`
	Reason     string                  `json:"reason"`
	Restored   []ReleaseRestoredTarget `json:"restored,omitempty"`
	AgentID    string                  `json:"agentId,omitempty"`
	RunID      string                  `json:"runId,omitempty"`
	Attempt    int64                   `json:"attempt,omitempty"`
	CreatedAt  string                  `json:"createdAt,omitempty"`
}

// ReleaseHold is an owner hold on a verified job, saved by the database
// handler (operations hold and unhold). The job is in state held while it
// stands: no deployer can claim it or take it into a batch, and it keeps its
// place in the queue. A request supplies Reason and UntilItems; the hub fills
// the rest. UntilItems is the release condition: the hub lifts the hold once
// every named item has a released or superseded job. Empty means until unhold.
type ReleaseHold struct {
	Reason     string   `json:"reason"`
	UntilItems []string `json:"untilItems,omitempty"`
	AgentID    string   `json:"agentId,omitempty"`
	RunID      string   `json:"runId,omitempty"`
	HeldAt     string   `json:"heldAt,omitempty"`
}

// ReleaseHoldEvent is one hold or release in a job's hold history. Cause is
// set on a release: "handler" for an unhold, "items_released" when the hub
// lifted the hold because TriggerJobID settled the last named item.
// Generation is the job's generation after the event.
type ReleaseHoldEvent struct {
	Action       string   `json:"action"`
	Cause        string   `json:"cause,omitempty"`
	Reason       string   `json:"reason"`
	UntilItems   []string `json:"untilItems,omitempty"`
	AgentID      string   `json:"agentId,omitempty"`
	RunID        string   `json:"runId,omitempty"`
	TriggerJobID string   `json:"triggerJobId,omitempty"`
	Generation   int64    `json:"generation"`
	At           string   `json:"at"`
}

// HandRelease is the owner's immutable record of a release made by hand. It
// cites an owner release intervention and names the released tasks-hub
// commit, the targets it shipped and the accepted job commits it carries.
// The hub checks its structure only: the CLI proves the git facts locally.
type HandRelease struct {
	ID              string   `json:"id,omitempty"`
	TaskID          string   `json:"taskId,omitempty"`
	InterventionSeq int64    `json:"interventionSeq"`
	ReleasedCommit  string   `json:"releasedCommit"`
	Release         string   `json:"release"`
	Targets         []string `json:"targets"`
	Commits         []string `json:"commits"`
	RecordedBy      *Caller  `json:"recordedBy,omitempty"`
	CreatedAt       string   `json:"createdAt,omitempty"`
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
	HandRelease        *HandRelease           `json:"handRelease,omitempty"`
	Retry              *ReleaseRetry          `json:"retry,omitempty"`
	Hold               *ReleaseHold           `json:"hold,omitempty"`
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

// RecordHandRelease stores the owner's hand release record (operation
// hand_release); a request carrying an agent identity is refused.
func (c *Client) RecordHandRelease(ctx context.Context, task string, req ReleaseRequest) (HandRelease, error) {
	var out HandRelease
	req.Operation = "hand_release"
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/releases/actions", req, &out)
	return out, err
}

// HandReleases lists the project's recorded hand releases (read-only).
func (c *Client) HandReleases(ctx context.Context, task string) ([]HandRelease, error) {
	out := []HandRelease{}
	err := c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/releases/actions", ReleaseRequest{Operation: "hand_releases"}, &out)
	return out, err
}

// Releases is a compatibility reader for historical API fixtures. Production
// listing paths use strict ReleasesPage and exact Release instead.
func (c *Client) Releases(ctx context.Context, task string) ([]ReleaseJob, error) {
	out := []ReleaseJob{}
	opts := ReleaseListOptions{}
	seen := map[string]bool{}
	for {
		q := url.Values{}
		if opts.After != "" {
			q.Set("after", opts.After)
			q.Set("snapshot", opts.Snapshot)
		}
		var raw json.RawMessage
		if err := c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/releases?"+q.Encode(), nil, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 && raw[0] == '[' && opts.After == "" {
			err := json.Unmarshal(raw, &out)
			return out, err
		}
		var page struct {
			Version int             `json:"version"`
			Jobs    []ReleaseJob    `json:"jobs"`
			Page    ReleasePageInfo `json:"page"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		if page.Version != 1 || page.Jobs == nil || page.Page.View != "active" || len(page.Page.Snapshot) != 64 || (opts.Snapshot != "" && opts.Snapshot != page.Page.Snapshot) {
			return nil, fmt.Errorf("invalid release page")
		}
		out = append(out, page.Jobs...)
		if page.Page.NextAfter == "" {
			return out, nil
		}
		if seen[page.Page.NextAfter] {
			return nil, fmt.Errorf("repeating release cursor")
		}
		seen[page.Page.NextAfter] = true
		opts.After, opts.Snapshot = page.Page.NextAfter, page.Page.Snapshot
	}
}

// Deployment compatibility is deliberately separate from the ordinary strict
// page/detail protocol. It negotiates only a successful, validated JSON shape;
// transport, authentication, size and version failures are never retried as legacy.
type compatibilityReleaseCursor struct {
	Version  int    `json:"version"`
	Task     string `json:"task"`
	View     string `json:"view"`
	Snapshot string `json:"snapshot"`
	Row      int64  `json:"row"`
}

func releaseSettled(state string) bool {
	return state == "released" || state == "rolled_back" || state == "refused" || state == "superseded"
}

// Project full details onto exactly the native compact contract. Comparing
// this projection detects changes to every pin, not just state/generation.
func compatibilitySummary(j ReleaseJob, row int64) ReleaseSummary {
	s := ReleaseSummary{Summary: true, RowID: row, ID: j.ID, TaskID: j.TaskID, EntryID: j.EntryID, ItemID: j.ItemID, ItemRevision: j.ItemRevision, ScopeRevision: j.ScopeRevision, OrderMessageSeq: j.OrderMessageSeq, BaseCommit: j.BaseCommit, Commit: j.Commit, VerificationDigest: j.VerificationDigest, State: j.State, Generation: j.Generation, AgentID: j.AgentID, RunID: j.RunID, PauseGeneration: j.PauseGeneration, IntegratedCommit: j.IntegratedCommit, InputsCommit: j.InputsCommit, InputsDigest: j.InputsDigest, Published: j.Published, SettledAt: j.SettledAt}
	if j.Receipt != nil {
		s.Receipt = &ReleaseSummaryReceipt{Outcome: j.Receipt.Outcome, Commit: j.Receipt.Commit, Targets: []ReleaseSummaryTarget{}}
		for _, t := range j.Receipt.Targets {
			s.Receipt.Targets = append(s.Receipt.Targets, ReleaseSummaryTarget{Target: t.Target, Outcome: t.Outcome})
		}
	}
	if j.Supersession != nil {
		s.Supersession = &ReleaseSummarySupersession{ReleasedCommit: j.Supersession.ReleasedCommit, Targets: j.Supersession.Targets}
	}
	return s
}

func compatibilityLegacy(raw json.RawMessage, task string) ([]ReleaseJob, string, error) {
	var records []json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil || records == nil {
		return nil, "", fmt.Errorf("invalid legacy release ledger")
	}
	jobs := make([]ReleaseJob, 0, len(records))
	seen := map[string]bool{}
	for _, record := range records {
		j, err := decodeCompatibilityDetail(record, task, "")
		if err != nil || seen[j.ID] {
			return nil, "", fmt.Errorf("invalid legacy release detail")
		}
		seen[j.ID] = true
		jobs = append(jobs, j)
	}
	// Include unknown fields as well: dropping heavy evidence from the digest
	// would let a receipt/verification change escape the continuation bookend.
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(append([]byte("deployment-compat-v1\x00"+task+"\x00"), compact.Bytes()...))
	return jobs, hex.EncodeToString(sum[:]), nil
}

func (c *Client) compatibilityPage(ctx context.Context, task string, opts ReleaseListOptions) (ReleasePage, []ReleaseJob, error) {
	if opts.View == "" {
		opts.View = "active"
	}
	if opts.Limit == 0 {
		opts.Limit = 50
	}
	zero := ReleasePage{}
	if (opts.View != "active" && opts.View != "settled") || opts.Limit < 1 || opts.Limit > 200 || len(opts.After) > 1024 || (opts.After != "" && opts.Snapshot == "") {
		return zero, nil, fmt.Errorf("invalid compatibility list options")
	}
	if opts.Snapshot != "" {
		b, e := hex.DecodeString(opts.Snapshot)
		if e != nil || len(b) != 32 {
			return zero, nil, fmt.Errorf("invalid release snapshot")
		}
	}
	q := url.Values{"view": {opts.View}, "limit": {strconv.Itoa(opts.Limit)}}
	if opts.After != "" {
		q.Set("after", opts.After)
	}
	if opts.Snapshot != "" {
		q.Set("snapshot", opts.Snapshot)
	}
	var raw json.RawMessage
	if err := c.compatibilityRead(ctx, "/v1/tasks/"+url.PathEscape(task)+"/releases?"+q.Encode(), &raw); err != nil {
		return zero, nil, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return zero, nil, fmt.Errorf("empty release response")
	}
	if raw[0] != '[' {
		page, err := decodeReleasePage(raw, task, opts)
		if err == nil {
			for _, j := range page.Jobs {
				if !compatibilityReleaseID(j.ID) || j.Generation < 1 {
					err = fmt.Errorf("invalid compatibility release summary")
					break
				}
			}
		}
		if err != nil {
			return zero, nil, err
		}
		return page, nil, nil
	}
	jobs, token, err := compatibilityLegacy(raw, task)
	if err != nil {
		return zero, nil, err
	}
	if opts.Snapshot != "" && opts.Snapshot != token {
		return zero, nil, fmt.Errorf("legacy release ledger changed")
	}
	var cursor compatibilityReleaseCursor
	if opts.After != "" {
		b, err := base64.RawURLEncoding.DecodeString(opts.After)
		if err != nil || json.Unmarshal(b, &cursor) != nil || cursor.Version != 1 || cursor.Task != task || cursor.View != opts.View || cursor.Snapshot != token || cursor.Row < 1 || cursor.Row > int64(len(jobs)) {
			return zero, nil, fmt.Errorf("invalid compatibility cursor")
		}
		if releaseSettled(jobs[cursor.Row-1].State) != (opts.View == "settled") {
			return zero, nil, fmt.Errorf("invalid compatibility cursor view")
		}
	}
	page := ReleasePage{Version: 1, Jobs: []ReleaseSummary{}, Page: ReleasePageInfo{View: opts.View, Limit: opts.Limit, Snapshot: token}}
	for i, j := range jobs {
		row := int64(i + 1)
		if row <= cursor.Row || releaseSettled(j.State) != (opts.View == "settled") {
			continue
		}
		if len(page.Jobs) == opts.Limit {
			b, _ := json.Marshal(compatibilityReleaseCursor{1, task, opts.View, token, page.Jobs[len(page.Jobs)-1].RowID})
			page.Page.NextAfter = base64.RawURLEncoding.EncodeToString(b)
			break
		}
		page.Jobs = append(page.Jobs, compatibilitySummary(j, row))
	}
	return page, jobs, nil
}

func (c *Client) CompatibilityReleasesPage(ctx context.Context, task string, opts ReleaseListOptions) (ReleasePage, error) {
	page, _, err := c.compatibilityPage(ctx, task, opts)
	// A caller paging with its own snapshot sees the plain refusal, as before.
	var stale *staleReleaseSnapshotError
	if errors.As(err, &stale) {
		err = stale.HTTPError
	}
	return page, err
}

// staleReleaseSnapshotError marks the hub's 409 "stale list snapshot": the
// ledger changed after the token was issued. It reads as its HTTPError.
type staleReleaseSnapshotError struct{ *HTTPError }

func (e *staleReleaseSnapshotError) Unwrap() error { return e.HTTPError }

// compatibilityLedgerAttempts bounds whole-ledger reads under ledger churn.
const compatibilityLedgerAttempts = 3

// A stale snapshot means only that a job changed between two requests of one
// read, which is routine while releases move. Each attempt starts again from a
// first page with no token and shares nothing with the attempt before it, so
// the result is always one snapshot. No other failure is retried.
func (c *Client) compatibilityLedger(ctx context.Context, task, onlyID string) ([]ReleaseJob, error) {
	for attempt := 1; ; attempt++ {
		out, err := c.compatibilityLedgerOnce(ctx, task, onlyID)
		var stale *staleReleaseSnapshotError
		if !errors.As(err, &stale) {
			return out, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if attempt == compatibilityLedgerAttempts {
			return nil, fmt.Errorf("release list kept changing: stale list snapshot on each of %d attempts: %w", attempt, err)
		}
	}
}

// Traverse both views under one token, then hydrate full detail before exposing
// any flat output. The final same-token bookend also catches detail-time races.
func (c *Client) compatibilityLedgerOnce(ctx context.Context, task, onlyID string) ([]ReleaseJob, error) {
	summaries := []ReleaseSummary{}
	seenIDs := map[string]bool{}
	rows := map[int64]bool{}
	cursors := map[string]bool{}
	token := ""
	var legacy []ReleaseJob
	for _, view := range []string{"active", "settled"} {
		opts := ReleaseListOptions{View: view, Limit: 200, Snapshot: token}
		previous := int64(0)
		for {
			page, full, err := c.compatibilityPage(ctx, task, opts)
			if err != nil {
				return nil, err
			}
			if token == "" {
				token = page.Page.Snapshot
				legacy = full
			}
			if (legacy != nil) != (full != nil) {
				return nil, fmt.Errorf("release protocol changed during read")
			}
			for _, s := range page.Jobs {
				if s.RowID <= previous || seenIDs[s.ID] || rows[s.RowID] {
					return nil, fmt.Errorf("invalid release continuation")
				}
				previous = s.RowID
				seenIDs[s.ID] = true
				rows[s.RowID] = true
				summaries = append(summaries, s)
			}
			if page.Page.NextAfter == "" {
				break
			}
			if cursors[page.Page.NextAfter] {
				return nil, fmt.Errorf("repeating release cursor")
			}
			cursors[page.Page.NextAfter] = true
			opts.After = page.Page.NextAfter
			opts.Snapshot = token
		}
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].RowID < summaries[j].RowID })
	out := make([]ReleaseJob, 0, len(summaries))
	legacyByID := map[string]ReleaseJob{}
	for _, j := range legacy {
		legacyByID[j.ID] = j
	}
	for _, s := range summaries {
		if onlyID != "" && s.ID != onlyID {
			continue
		}
		var j ReleaseJob
		if legacy != nil {
			j = legacyByID[s.ID]
		} else {
			var err error
			j, err = c.compatibilityDetail(ctx, task, s.ID)
			if err != nil {
				return nil, err
			}
		}
		if !reflect.DeepEqual(compatibilitySummary(j, s.RowID), s) {
			return nil, fmt.Errorf("release detail changed or compact")
		}
		out = append(out, j)
	}
	// Revalidate both protocol and token after hydration; zero partial output.
	_, full, err := c.compatibilityPage(ctx, task, ReleaseListOptions{View: "active", Limit: 200, Snapshot: token})
	if err != nil {
		return nil, err
	}
	if (legacy != nil) != (full != nil) {
		return nil, fmt.Errorf("release protocol changed during bookend")
	}
	return out, nil
}
func (c *Client) CompatibilityReleases(ctx context.Context, task string) ([]ReleaseJob, error) {
	return c.compatibilityLedger(ctx, task, "")
}
func (c *Client) CompatibilityRelease(ctx context.Context, task, id string) (ReleaseJob, error) {
	if id == "" {
		return ReleaseJob{}, fmt.Errorf("exact release ID required")
	}
	jobs, err := c.compatibilityLedger(ctx, task, id)
	if err != nil {
		return ReleaseJob{}, err
	}
	if len(jobs) != 1 {
		return ReleaseJob{}, fmt.Errorf("release job not found")
	}
	return jobs[0], nil
}

// No successful-shape negotiation on redirects, partial bodies or HTTP errors.
func (c *Client) compatibilityRead(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+path, nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, defaultMaxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > defaultMaxResponseBytes {
		return fmt.Errorf("hub response exceeds %d bytes", defaultMaxResponseBytes)
	}
	if response.StatusCode != http.StatusOK {
		refused := &HTTPError{Status: response.StatusCode, Msg: "deployment compatibility read refused"}
		var body ErrorResponse
		if response.StatusCode == http.StatusConflict && json.Unmarshal(data, &body) == nil && strings.HasSuffix(body.Error, "release: stale list snapshot") {
			return &staleReleaseSnapshotError{refused}
		}
		return refused
	}
	return json.Unmarshal(data, out)
}
func compatibilityReleaseID(id string) bool {
	if len(id) != 20 || !strings.HasPrefix(id, "rel_") || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id[4:])
	return err == nil
}

func decodeCompatibilityDetail(raw json.RawMessage, task, id string) (ReleaseJob, error) {
	var fields map[string]json.RawMessage
	var wire struct {
		ReleaseJob
		Summary bool `json:"summary"`
	}
	if json.Unmarshal(raw, &fields) != nil || fields == nil || json.Unmarshal(raw, &wire) != nil || wire.Summary || !compatibilityReleaseID(wire.ID) || (id != "" && wire.ID != id) || wire.TaskID != task || wire.State == "" || wire.Generation < 1 {
		return ReleaseJob{}, fmt.Errorf("invalid full release detail")
	}
	// An unmarked compact object is also invalid: full records always contain
	// the approved plan, and full receipts carry their native schema version.
	var plan map[string]json.RawMessage
	if json.Unmarshal(fields["plan"], &plan) != nil || plan == nil {
		return ReleaseJob{}, fmt.Errorf("compact release detail")
	}
	if wire.Receipt != nil && (wire.Receipt.Version != 1 || wire.Receipt.JobID != wire.ID || wire.Receipt.Targets == nil) {
		return ReleaseJob{}, fmt.Errorf("compact or invalid release receipt")
	}
	return wire.ReleaseJob, nil
}
func (c *Client) compatibilityDetail(ctx context.Context, task, id string) (ReleaseJob, error) {
	var raw json.RawMessage
	if err := c.compatibilityRead(ctx, "/v1/tasks/"+url.PathEscape(task)+"/releases?job="+url.QueryEscape(id), &raw); err != nil {
		return ReleaseJob{}, err
	}
	return decodeCompatibilityDetail(raw, task, id)
}
