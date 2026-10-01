package api

import (
	"context"
	"encoding/json"
	"fmt"
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
	// Attempt numbers the item's entries from 1. A retry of a released failed
	// entry is a new entry; RetryOf names the entry it retries.
	Attempt int64  `json:"attempt,omitempty"`
	RetryOf string `json:"retryOf,omitempty"`
	// Serial marks an entry that runs alone: it conflicts with every other
	// team instead of declaring ownership.
	Serial bool `json:"serial,omitempty"`
	// OwnerIntegration is the owner's record that this entry's candidate was
	// integrated outside the handler acceptance path. It releases the entry's
	// slot, handler lease and ownership while its item stays open.
	OwnerIntegration *TeamQueueOwnerIntegration `json:"ownerIntegration,omitempty"`
	// Stall explains a queued entry that waits only on something nothing will
	// clear by itself, with the supported fix. It is computed on read.
	Stall     *TeamQueueStall `json:"stall,omitempty"`
	UpdatedAt string          `json:"updatedAt,omitempty"`

	// HandlerArm is the arm assignment of the current lease under a handler
	// arm policy (docs/handler-ab.md).
	HandlerArm *TeamQueueHandlerArm `json:"handlerArm,omitempty"`

	// Summary marks a trimmed history entry in a listing: it carries no
	// launch, close or activities and bounded reviews, verification and
	// release. GET .../team-queue/{entry} returns it in full.
	Summary bool `json:"summary,omitempty"`
	// TeamShape is "plan-review" or "plan-only", derived from the launch a
	// summary entry no longer carries.
	TeamShape string `json:"teamShape,omitempty"`
}

// TeamQueueOwnerIntegration is kept distinct from TeamIntegrationReady, the
// "Ready to integrate" snapshot of a handler-accepted team.
type TeamQueueOwnerIntegration struct {
	Commit       string   `json:"commit"`
	BaseCommit   string   `json:"baseCommit"`
	Evidence     string   `json:"evidence,omitempty"`
	ChangedFiles []string `json:"changedFiles"`
	At           string   `json:"at"`
}

// TeamQueueStall names the one entry (or missing handler) a queued entry is
// stuck behind, the supported fix command and since when it has held.
type TeamQueueStall struct {
	Cause           string `json:"cause"`
	BlockerEntryID  string `json:"blockerEntryId,omitempty"`
	BlockerRevision int64  `json:"blockerRevision,omitempty"`
	Fix             string `json:"fix"`
	Since           string `json:"since"`
}

// NoticeRequestID names one stall for its Board notice: its blocker (or,
// with none, the queued entry), its cause and the blocker's revision. The
// runner sends it as the notice's retry identity, so a stall is announced
// once; the hub refuses any other identity as stale.
func (s TeamQueueStall) NoticeRequestID(entryID string) string {
	subject := s.BlockerEntryID
	if subject == "" {
		subject = entryID
	}
	return fmt.Sprintf("queue-stall-%s-%s-%d", subject, s.Cause, s.BlockerRevision)
}

// Stall causes.
const (
	StallFailedEntry    = "failed-entry"
	StallNothingRunning = "nothing-running"
	StallIdleEntry      = "idle-entry"
	StallNoHandler      = "no-handler"
	StallSerialHalted   = "serial-halted"
)

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
	// History describes the page of history entries that follows the
	// active entries; it is absent for view=active and item listings.
	History *TeamQueueHistoryPage `json:"history,omitempty"`
}

// TeamQueueHistoryPage is one newest-first page of finished and released
// failed entries. NextAfter, when set, is the after cursor of the next page.
type TeamQueueHistoryPage struct {
	Total     int   `json:"total"`
	Limit     int   `json:"limit"`
	NextAfter int64 `json:"nextAfter,omitempty"`
}

// TeamQueueViewActive lists only the entries that are not history.
const TeamQueueViewActive = "active"

// DefaultTeamQueueHistoryLimit is the history page size when none is given.
const DefaultTeamQueueHistoryLimit = 50

// TeamQueueListOptions selects a team queue listing. The zero value is the
// default: active entries in full, then one page of history summaries.
type TeamQueueListOptions struct {
	// View is "" (default) or TeamQueueViewActive.
	View string
	// Item returns only that item's entry, in any state and in full.
	Item string
	// Limit is the history page size, 1..MaxLimit; 0 means the default.
	Limit int
	// After returns history entries with a lower position.
	After int64
}

// Query encodes the options, omitting zero values.
func (o TeamQueueListOptions) Query() string {
	v := url.Values{}
	if o.View != "" {
		v.Set("view", o.View)
	}
	if o.Item != "" {
		v.Set("item", o.Item)
	}
	if o.Limit != 0 {
		v.Set("limit", strconv.Itoa(o.Limit))
	}
	if o.After != 0 {
		v.Set("after", strconv.FormatInt(o.After, 10))
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
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
	Serial                   bool                       `json:"serial,omitempty"`
	OwnerIntegrationCommit   string                     `json:"ownerIntegrationCommit,omitempty"`
	OwnerIntegrationEvidence string                     `json:"ownerIntegrationEvidence,omitempty"`
	ChangedFiles             []string                   `json:"changedFiles,omitempty"`
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

// MaxTeamQueueListResponse bounds a team queue listing. The hub trims history
// entries to summaries and pages them, but an older hub returns every entry
// with its launch context, which outgrows the client's default 4 MiB response
// cap; a truncated body then fails to parse and stalls both tt team queue list
// and the queue runner. The larger cap stays as a backstop.
const MaxTeamQueueListResponse = 64 << 20

// ListTeamQueue reads the default listing: active entries in full, then the
// newest page of history summaries.
func (c *Client) ListTeamQueue(ctx context.Context, task string) (TeamQueueList, error) {
	return c.ListTeamQueuePage(ctx, task, TeamQueueListOptions{})
}

// ListTeamQueuePage reads the listing the options select. An older hub
// ignores them and returns every entry, so callers keep filtering.
func (c *Client) ListTeamQueuePage(ctx context.Context, task string, opts TeamQueueListOptions) (TeamQueueList, error) {
	var out TeamQueueList
	return out, c.doLimited(ctx, "GET", "/v1/tasks/"+url.PathEscape(task)+"/team-queue"+opts.Query(), nil, &out, MaxTeamQueueListResponse)
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
	return out, c.doLimited(ctx, "GET", "/v1/team-queues?host="+url.QueryEscape(host)+"&limit="+strconv.Itoa(MaxLimit), nil, &out, MaxTeamQueueListResponse)
}
