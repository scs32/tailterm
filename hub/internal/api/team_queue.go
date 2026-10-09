package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
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
	// AdmittedAt is when the entry was claimed for launch; the team's tokens
	// are counted from then. Empty on a queued entry and on one admitted by an
	// older hub.
	AdmittedAt string `json:"admittedAt,omitempty"`
	// Rebinds is the entry's history of moves to a later item revision,
	// oldest first. Summaries leave it out.
	Rebinds []TeamQueueRebind `json:"rebinds,omitempty"`
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

	// Waits are the entry's open shared-path waits on other entries
	// (docs/project-queue.md, "Team queue shared-path waits"). A listing
	// attaches them and appends each one's sentence to BlockReason; it never
	// evaluates their conditions.
	Waits []TeamQueueWait `json:"waits,omitempty"`

	// HandlerArm is the arm assignment of the current lease under a handler
	// arm policy (docs/handler-ab.md).
	HandlerArm *TeamQueueHandlerArm `json:"handlerArm,omitempty"`

	// Budget is the item's token estimate against its lifetime attributed
	// usage over every run, open and closed, unlike Tokens above.
	Budget *TokenBudget `json:"budget,omitempty"`

	// HandlerNeed explains a queued entry that waits only for a database
	// handler: what it needs and whether the runner may add one
	// (docs/handler-ab.md, "Automatic provisioning"). It is computed on read.
	HandlerNeed *TeamQueueHandlerNeed `json:"handlerNeed,omitempty"`
	// Warning is advice a saved settings change returns, such as a raised
	// limit that exceeds the available database handlers.
	Warning string `json:"warning,omitempty"`

	// EstimateDefault is the lane default admission compares with the budget
	// for an entry whose item has no saved estimate. It is computed on read
	// for queued, launching and running entries, and absent when the item has
	// a saved estimate or the project has no lane defaults.
	EstimateDefault *TeamQueueEstimateDefault `json:"estimateDefault,omitempty"`
	// BudgetHold is the entry's hold past BudgetHoldMultiple times its saved
	// estimate, while it keeps the team from starting turns.
	BudgetHold *BudgetHold `json:"budgetHold,omitempty"`

	// Summary marks a trimmed history entry in a listing: it carries no
	// launch, close or activities and bounded reviews, verification and
	// release. GET .../team-queue/{entry} returns it in full.
	Summary bool `json:"summary,omitempty"`
	// TeamShape is "plan-review" or "plan-only", derived from the launch a
	// summary entry no longer carries.
	TeamShape string `json:"teamShape,omitempty"`
}

// The conditions a shared-path wait resumes on, and its states.
const (
	TeamQueueWaitAccepted = "accepted"
	TeamQueueWaitDone     = "done"
	TeamQueueWaitReleased = "released"

	TeamQueueWaitWaiting  = "waiting"
	TeamQueueWaitMet      = "met"
	TeamQueueWaitResumed  = "resumed"
	TeamQueueWaitOverdue  = "overdue"
	TeamQueueWaitOrphaned = "orphaned"
	TeamQueueWaitCleared  = "cleared"
)

// ValidTeamQueueWaitUntil reports whether until is one of the three conditions.
func ValidTeamQueueWaitUntil(until string) bool {
	return until == TeamQueueWaitAccepted || until == TeamQueueWaitDone || until == TeamQueueWaitReleased
}

// TeamQueueWait is one entry's recorded wait for paths another entry owns.
// The broker tick moves it from waiting to met and tells the waiting lead
// once; orphaned means the predecessor can no longer meet the condition and
// a handler must decide.
type TeamQueueWait struct {
	OnItemID  string   `json:"onItemId"`
	OnEntryID string   `json:"onEntryId"`
	Paths     []string `json:"paths"`
	Until     string   `json:"until"`
	State     string   `json:"state"`
	SetAt     string   `json:"setAt,omitempty"`
	MetAt     string   `json:"metAt,omitempty"`
	MetCommit string   `json:"metCommit,omitempty"`
	NoticeSeq int64    `json:"noticeSeq,omitempty"`
	Reason    string   `json:"reason,omitempty"`
}

// TeamQueueWaitTextPathBytes caps the path list in a wait's sentence. The
// sentence is appended to BlockReason, which a stall notice carries in a body
// of at most MaxEnvelopeBodyBytes; Paths itself always lists every path.
const TeamQueueWaitTextPathBytes = 300

// TeamQueueWaitPaths lists paths in one line of about max bytes: the first
// path always, then whole paths while they fit, then "and N more".
func TeamQueueWaitPaths(paths []string, max int) string {
	var b strings.Builder
	for i, p := range paths {
		if i > 0 && b.Len()+len(p) > max {
			fmt.Fprintf(&b, " and %d more", len(paths)-i)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p)
	}
	return b.String()
}

// Text is the wait's one sentence in a listing and in BlockReason, with a
// long path list cut short.
func (w TeamQueueWait) Text() string {
	paths := TeamQueueWaitPaths(w.Paths, TeamQueueWaitTextPathBytes)
	switch w.State {
	case TeamQueueWaitMet:
		commit := w.MetCommit
		if commit == "" {
			commit = "none recorded"
		}
		return fmt.Sprintf("wait on %s met (%s, commit %s): rebase before editing %s", w.OnItemID, w.Until, commit, paths)
	case TeamQueueWaitOrphaned:
		return fmt.Sprintf("wait on %s needs a handler decision: %s", w.OnItemID, w.Reason)
	}
	return fmt.Sprintf("waits on %s for %s until %s", w.OnItemID, paths, w.Until)
}

// TeamQueueEstimateDefault is a lane default estimate: the tokens, the lane
// ("small" or "planned") and whether the entry's owned paths select the Go
// race check.
type TeamQueueEstimateDefault struct {
	Tokens int64  `json:"tokens"`
	Lane   string `json:"lane"`
	GoRace bool   `json:"goRace"`
}

// TeamQueueRebind records one move of a queued or running entry, and of a
// running team's live bindings, to a later revision of the same item after an
// amendment. AmendedBy saved the item's new revision; ApprovedBy asked for
// the rebind. Nothing a rebind records is rewritten later.
type TeamQueueRebind struct {
	ID                string                   `json:"id"`
	EntryID           string                   `json:"entryId"`
	FromItemRevision  int64                    `json:"fromItemRevision"`
	ToItemRevision    int64                    `json:"toItemRevision"`
	FromScopeRevision int64                    `json:"fromScopeRevision,omitempty"`
	ToScopeRevision   int64                    `json:"toScopeRevision"`
	OrderMessageSeq   int64                    `json:"orderMessageSeq"`
	SourceMessageSeq  int64                    `json:"sourceMessageSeq"`
	AmendedBy         Sender                   `json:"amendedBy"`
	ApprovedBy        Sender                   `json:"approvedBy"`
	ApprovedRunID     string                   `json:"approvedRunId,omitempty"`
	EntryState        string                   `json:"entryState"`
	CreatedAt         string                   `json:"createdAt"`
	Bindings          []TeamQueueRebindBinding `json:"bindings,omitempty"`
}

// TeamQueueRebindBinding is one live team binding a rebind moved. Its run and
// context digest are the admission's and do not change.
type TeamQueueRebindBinding struct {
	AgentID          string `json:"agentId"`
	RunID            string `json:"runId"`
	FromItemRevision int64  `json:"fromItemRevision"`
	ToItemRevision   int64  `json:"toItemRevision"`
	ContextDigest    string `json:"contextDigest"`
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

// TeamQueueHandlerNeed is the database handler a queued entry waits for.
// Handlers counts the project's open handlers with the wanted settings and
// Leased how many of them active entries hold. Provision is true when the
// runner may add one from its saved launch spec; otherwise Reason says why
// not and Fix is the exact command.
type TeamQueueHandlerNeed struct {
	Arm            string `json:"arm,omitempty"`
	Runtime        string `json:"runtime,omitempty"`
	Model          string `json:"model,omitempty"`
	Reasoning      string `json:"reasoning,omitempty"`
	TemplateDigest string `json:"templateDigest,omitempty"`
	Handlers       int    `json:"handlers"`
	Leased         int    `json:"leased"`
	Provision      bool   `json:"provision"`
	// Refused marks a standing refusal of this host's saved launch spec. The
	// runner keeps offering its spec, so a corrected one is noticed.
	Refused bool `json:"refused,omitempty"`
	// Attempt numbers the entry's provisions from 1; the runner's retry
	// identity includes it, so an abandoned reservation is not replayed.
	Attempt int    `json:"attempt,omitempty"`
	AgentID string `json:"agentId,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

// Automatic handler provisioning switch values.
const (
	HandlerProvisionOn  = "on"
	HandlerProvisionOff = "off"
)

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
	// HandlerProvision is the project's automatic handler provisioning
	// switch, "on" or "off"; an older hub leaves it empty.
	HandlerProvision string `json:"handlerProvision,omitempty"`
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
	// ItemRevision is the item revision a rebind moves the entry to; it must
	// be the item's current one. SourceMessageSeq is the amendment's message.
	ItemRevision     int64 `json:"itemRevision,omitempty"`
	SourceMessageSeq int64 `json:"sourceMessageSeq,omitempty"`
	// HandlerProvision is set_handler_provision's switch value, "on" or "off".
	HandlerProvision string `json:"handlerProvision,omitempty"`
	// The runner's saved handler launch spec for provision_handler: runtime,
	// model, reasoning and the prompt's template digest, all empty when the
	// host has no saved spec. HandlerAgentID is the preallocated handler.
	HandlerSpecRuntime   string `json:"handlerSpecRuntime,omitempty"`
	HandlerSpecModel     string `json:"handlerSpecModel,omitempty"`
	HandlerSpecReasoning string `json:"handlerSpecReasoning,omitempty"`
	HandlerSpecDigest    string `json:"handlerSpecDigest,omitempty"`
	// HandlerSpecArgs is the saved spec's other launch flags as flag, value
	// pairs (never the prompt), so a refusal can print a complete command
	// that saves the corrected spec.
	HandlerSpecArgs []string `json:"handlerSpecArgs,omitempty"`
	// wait_set and wait_clear: EntryID is the waiting entry, WaitOnEntryID
	// the predecessor entry. WaitPaths and WaitUntil (accepted, done or
	// released) belong to wait_set; WaitReason is wait_clear's optional why.
	WaitOnEntryID string   `json:"waitOnEntryId,omitempty"`
	WaitPaths     []string `json:"waitPaths,omitempty"`
	WaitUntil     string   `json:"waitUntil,omitempty"`
	WaitReason    string   `json:"waitReason,omitempty"`
	// AgentID is the agent a budget_continue caller names itself as; the hub
	// accepts only the project's owner helper. No other operation reads it.
	AgentID string `json:"agentId,omitempty"`
	// Caller is the authenticated caller, set by the hub's HTTP layer and
	// never read from the wire. A rebind records it as the approver.
	Caller Caller `json:"-"`
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
