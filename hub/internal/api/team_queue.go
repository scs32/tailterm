package api

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
)

// TeamCloseWaitError is a definite close refusal. A queue runner may refresh
// its snapshot and wait; transport errors must retain the exact prior request.
type TeamCloseWaitError struct {
	Code string
	Text string
}

func (e *TeamCloseWaitError) Error() string { return e.Text }
func (e *TeamCloseWaitError) Unwrap() error { return ErrConflict }

// TeamQueueEntry is the hub-owned delivery queue. It is unrelated to the
// deliberate, agent-claimed Queue API.
type TeamQueueEntry struct {
	Release                *ReleaseJob                `json:"release,omitempty"`
	Verification           *VerificationSummary       `json:"verification,omitempty"`
	Reviews                *ReviewConvergence         `json:"reviews,omitempty"`
	Activities             []TeamAgentActivity        `json:"activities,omitempty"`
	Tokens                 TokenTotals                `json:"tokens"`
	ID                     string                     `json:"id"`
	TaskID                 string                     `json:"taskId"`
	ItemID                 string                     `json:"itemId"`
	ItemRevision           int64                      `json:"itemRevision"`
	OrderMessageSeq        int64                      `json:"orderMessageSeq"`
	Template               string                     `json:"template"`
	Position               int64                      `json:"position"`
	State                  string                     `json:"state"`
	Revision               int64                      `json:"revision"`
	Host                   string                     `json:"host"`
	Cwd                    string                     `json:"cwd"`
	HandlerID              string                     `json:"handlerId,omitempty"`
	HandlerRunID           string                     `json:"handlerRunId,omitempty"`
	HandlerLeaseGeneration int64                      `json:"handlerLeaseGeneration,omitempty"`
	BaseCommit             string                     `json:"baseCommit,omitempty"`
	Acceptance             *TeamIntegrationAcceptance `json:"acceptance,omitempty"`
	Integration            *TeamIntegrationReady      `json:"integration,omitempty"`
	Repository             string                     `json:"repository,omitempty"`
	Ownership              []string                   `json:"ownership,omitempty"`
	BlockedBy              []string                   `json:"blockedBy,omitempty"`
	BlockReason            string                     `json:"blockReason,omitempty"`
	PauseGeneration        int64                      `json:"pauseGeneration"`
	LaunchJSON             json.RawMessage            `json:"launch,omitempty"`
	CloseJSON              json.RawMessage            `json:"close,omitempty"`
	Failure                string                     `json:"failure,omitempty"`
	EscalationSeq          int64                      `json:"escalationSeq,omitempty"`
	ReleasedAt             string                     `json:"releasedAt,omitempty"`
}

type TeamAgentActivity struct {
	AgentID  string         `json:"agentId"`
	RunID    string         `json:"runId"`
	Name     string         `json:"name"`
	Activity *AgentActivity `json:"activity,omitempty"`
}

type TeamQueueList struct {
	Entries          []TeamQueueEntry `json:"entries"`
	ConcurrencyLimit int              `json:"concurrencyLimit"`
	HostPolicy       *TeamHostPolicy  `json:"hostPolicy,omitempty"`
	HostUsage        *TeamHostUsage   `json:"hostUsage,omitempty"`
}

type TeamHostPolicy struct {
	Host                 string `json:"host"`
	LimiterDomain        string `json:"limiterDomain"`
	Version              int64  `json:"version"`
	ExpiresAt            string `json:"expiresAt"`
	MaxSessions          int    `json:"maxSessions"`
	MaxPolling           int    `json:"maxPolling"`
	MaxRelayBindings     int    `json:"maxRelayBindings"`
	MaxRequestsPerMinute int    `json:"maxRequestsPerMinute"`
	MaxBurst             int    `json:"maxBurst"`
	HeadroomPercent      int    `json:"headroomPercent"`
	// MinFreeDiskMiB is the free-disk reserve for new parallel admission;
	// 0 means the built-in default.
	MinFreeDiskMiB int64 `json:"minFreeDiskMiB,omitempty"`
}

// DefaultMinFreeDiskMiB is the free-disk reserve when a host policy leaves it
// unset. Each team adds builds and browser runs to the launch host.
const DefaultMinFreeDiskMiB = 8192

// DiskReserveMiB is the free-disk reserve that gates new parallel admission.
func (p TeamHostPolicy) DiskReserveMiB() int64 {
	if p.MinFreeDiskMiB > 0 {
		return p.MinFreeDiskMiB
	}
	return DefaultMinFreeDiskMiB
}

type TeamHostUsage struct {
	Host          string `json:"host"`
	LimiterDomain string `json:"limiterDomain"`
	PolicyVersion int64  `json:"policyVersion"`
	ObservedAt    string `json:"observedAt"`
	RelayBindings int    `json:"relayBindings"`
	Complete      bool   `json:"complete"`
	SourceDigest  string `json:"sourceDigest"`
	// FreeDiskMiB is the least free space across the host's queue worktrees;
	// nil means the runner did not observe it.
	FreeDiskMiB *int64 `json:"freeDiskMiB,omitempty"`
}

type TeamQueueRequest struct {
	RequestID                string                     `json:"requestId"`
	Operation                string                     `json:"operation"`
	ItemID                   string                     `json:"itemId,omitempty"`
	OrderMessageSeq          int64                      `json:"orderMessageSeq,omitempty"`
	Template                 string                     `json:"template,omitempty"`
	EntryID                  string                     `json:"entryId,omitempty"`
	BeforeID                 string                     `json:"beforeId,omitempty"`
	ExpectedRevision         int64                      `json:"expectedRevision,omitempty"`
	Host                     string                     `json:"host,omitempty"`
	Cwd                      string                     `json:"cwd,omitempty"`
	Repository               string                     `json:"repository,omitempty"`
	Ownership                []string                   `json:"ownership,omitempty"`
	ConcurrencyLimit         int                        `json:"concurrencyLimit,omitempty"`
	HostPolicyVersion        int64                      `json:"hostPolicyVersion,omitempty"`
	HostPolicyExpires        string                     `json:"hostPolicyExpires,omitempty"`
	HostMaxSessions          int                        `json:"hostMaxSessions,omitempty"`
	HostMaxPolling           int                        `json:"hostMaxPolling,omitempty"`
	HostMaxRelayBindings     int                        `json:"hostMaxRelayBindings,omitempty"`
	HostMaxRequestsPerMinute int                        `json:"hostMaxRequestsPerMinute,omitempty"`
	HostMaxBurst             int                        `json:"hostMaxBurst,omitempty"`
	HostHeadroomPercent      int                        `json:"hostHeadroomPercent,omitempty"`
	HostMinFreeDiskMiB       int64                      `json:"hostMinFreeDiskMiB,omitempty"`
	LimiterDomain            string                     `json:"limiterDomain,omitempty"`
	HostUsage                *TeamHostUsage             `json:"hostUsage,omitempty"`
	BaseCommit               string                     `json:"baseCommit,omitempty"`
	Integration              *TeamIntegrationReady      `json:"integration,omitempty"`
	Acceptance               *TeamIntegrationAcceptance `json:"acceptance,omitempty"`
	HandlerAgentID           string                     `json:"handlerAgentId,omitempty"`
	HandlerRunID             string                     `json:"handlerRunId,omitempty"`
	PauseGeneration          int64                      `json:"pauseGeneration,omitempty"`
	LaunchJSON               json.RawMessage            `json:"launch,omitempty"`
	CloseJSON                json.RawMessage            `json:"close,omitempty"`
	CloseRequestID           string                     `json:"closeRequestId,omitempty"`
	ReservationToken         string                     `json:"reservationToken,omitempty"`
	ManualJournal            json.RawMessage            `json:"manualJournal,omitempty"`
	SessionsChecked          bool                       `json:"sessionsChecked,omitempty"`
	ReleaseProof             *TeamQueueReleaseProof     `json:"releaseProof,omitempty"`
	Failure                  string                     `json:"failure,omitempty"`
	MemberIndex              int                        `json:"memberIndex,omitempty"`
	MemberRunID              string                     `json:"memberRunId,omitempty"`
	LeadAgentID              string                     `json:"leadAgentId,omitempty"`
	LeadRunID                string                     `json:"leadRunId,omitempty"`
	ExpectedLeadRevision     int64                      `json:"expectedLeadRevision,omitempty"`
}

type TeamIntegrationReady struct {
	Repository string `json:"repository"`
	BaseCommit string `json:"baseCommit"`
	Worktree   string `json:"worktree,omitempty"`
	Branch     string `json:"branch"`
	Commit     string `json:"commit"`
	Evidence   string `json:"evidence"`
	ReadyAt    string `json:"readyAt"`
}

// TeamIntegrationAcceptance is the handler's saved acceptance of the exact
// builder worktree and commit, pinned to the terminal item report/revision.
type TeamIntegrationAcceptance struct {
	Repository       string              `json:"repository"`
	BaseCommit       string              `json:"baseCommit"`
	Worktree         string              `json:"worktree"`
	Branch           string              `json:"branch"`
	Commit           string              `json:"commit"`
	ItemRevision     int64               `json:"itemRevision"`
	CompletionReport *NarrativeReportPin `json:"completionReport,omitempty"`
	Evidence         string              `json:"evidence"`
	AcceptedAt       string              `json:"acceptedAt"`
	// ResolvedKnownFailures is server-derived: the item's own known failures
	// that its current receipt shows now passing, recorded so a later matrix
	// cleanup has an exact source. It is absent when nothing was resolved.
	ResolvedKnownFailures *ResolvedKnownFailures `json:"resolvedKnownFailures,omitempty"`
}

// ResolvedKnownFailures names the exact receipt that let a bug close with
// known failures that name it still listed in its approved matrix.
type ResolvedKnownFailures struct {
	ReceiptGeneration int64                      `json:"receiptGeneration"`
	ReceiptDigest     string                     `json:"receiptDigest"`
	MatrixDigest      string                     `json:"matrixDigest"`
	Entries           []VerificationKnownFailure `json:"entries"`
}

type TeamQueueReleaseProof struct {
	TaskID       string                   `json:"taskId"`
	EntryID      string                   `json:"entryId"`
	ItemID       string                   `json:"itemId"`
	Host         string                   `json:"host"`
	LaunchDigest string                   `json:"launchDigest"`
	Members      []TeamQueueReleaseMember `json:"members"`
}

type TeamQueueReleaseMember struct {
	AgentID string `json:"agentId"`
	RunID   string `json:"runId"`
	Name    string `json:"name"`
}

func (c *Client) ListTeamQueue(ctx context.Context, task string) (TeamQueueList, error) {
	var out TeamQueueList
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/team-queue", nil, &out)
}

func (c *Client) TeamQueueAction(ctx context.Context, task string, req TeamQueueRequest) (TeamQueueEntry, error) {
	var out TeamQueueEntry
	return out, c.do(ctx, "POST", "/v1/tasks/"+url.PathEscape(task)+"/team-queue/actions", req, &out)
}

func (c *Client) GetTeamQueueEntry(ctx context.Context, task, entry string) (TeamQueueEntry, error) {
	var out TeamQueueEntry
	return out, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/team-queue/"+url.PathEscape(entry), nil, &out)
}

func (c *Client) TeamQueueByHost(ctx context.Context, host string) (TeamQueueList, error) {
	var out TeamQueueList
	return out, c.do(ctx, "GET", "/v1/team-queues?host="+url.QueryEscape(host)+"&limit="+strconv.Itoa(MaxLimit), nil, &out)
}
