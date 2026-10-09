package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
	"github.com/scs32/tailterm/hub/internal/teamplan"
)

// The supervised relay ticks this runner on the launch host. Hub rows are the
// journal: a local file may hold the frozen context only during one spawn call.
// Dependencies are injectable so tests never start a real runtime or tmux.
type teamRunner struct {
	plan        func(context.Context, map[string]any, *teamLaunchResolved) error
	spawn       func(env, []string) error
	owned       func(context.Context, env, api.Agent) error
	cleanup     func(context.Context, env, string, string) error
	integration func(context.Context, api.TeamQueueEntry, api.WorkItem, api.TeamCloseRequest) (*api.TeamIntegrationReady, error)
	census      func(ctx context.Context, c *api.Client, task, host string, policy api.TeamHostPolicy, prior *api.TeamHostUsage, cwds []string) error
	// changed lists the files a candidate changed; nil uses Git.
	changed func(ctx context.Context, repository, base, commit string) ([]string, error)
	// worktrees removes a finished entry's worktrees once their work is
	// integrated (docs/team-launch.md); nil leaves them in place.
	worktrees func(ctx context.Context, c *api.Client, host string, active, project api.TeamQueueList, q api.TeamQueueEntry) error
	// stallGrace is how long a stall holds before its Board notice; zero
	// uses the hub's five minutes.
	stallGrace time.Duration
	roundRobin bool
	// retries bounds and paces launch errors per entry; nil retries every
	// tick, as before.
	retries *launchRetryBook
	// now is the retry clock; nil uses time.Now.
	now func() time.Time
}

func (r teamRunner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Launch error bounds (docs/project-queue.md, "Team queue launch errors").
const (
	launchPermanentAttempts = 3
	launchRetryFirstDelay   = 15 * time.Second
	launchRetryMaxDelay     = 5 * time.Minute
	launchErrorNoticeRunes  = 300
)

// launchStepError is a refused hub write inside a launch: the operation and
// the entry revision it expected.
type launchStepError struct {
	op       string
	revision int64
	err      error
}

func (e *launchStepError) Error() string { return e.op + ": " + e.err.Error() }
func (e *launchStepError) Unwrap() error { return e.err }

// launchErrorPermanent reports a hub refusal that repeating the same write
// cannot change: a malformed, missing or oversized request, or the same
// retry identity with a different payload. A stale revision, a credential
// fault, rate limits, server errors and network faults can clear.
func launchErrorPermanent(err error) bool {
	var response *api.HTTPError
	if !errors.As(err, &response) {
		return false
	}
	switch response.Status {
	case 400, 404, 413, 422:
		return true
	case 409:
		return strings.HasSuffix(response.Msg, "team queue retry differs")
	}
	return false
}

// launchRetryDelay is the wait after the nth consecutive launch error.
func launchRetryDelay(n int) time.Duration {
	delay := launchRetryFirstDelay
	for i := 1; i < n && delay < launchRetryMaxDelay; i++ {
		delay *= 2
	}
	return min(delay, launchRetryMaxDelay)
}

// launchRetryBook holds each launching entry's consecutive errors on this
// relay. It lives only in memory: a restart allows at most one more bounded
// round, and the hub keeps the failure and its notice exactly once.
type launchRetryBook struct {
	mu      sync.Mutex
	entries map[string]*launchRetryState
}

type launchRetryState struct {
	signature string
	permanent int
	failures  int
	next      time.Time
	lastErr   string
}

var productionLaunchRetries = &launchRetryBook{}

func launchRetryKey(hub, task, entry string) string {
	return strings.TrimRight(hub, "/") + "\x00" + task + "\x00" + entry
}

// waiting reports whether the entry is inside its backoff window.
func (b *launchRetryBook) waiting(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.entries[key]
	return state != nil && now.Before(state.next)
}

// record notes one launch error and returns the permanent refusal once the
// same one has repeated launchPermanentAttempts times in a row.
func (b *launchRetryBook) record(key string, err error, now time.Time) *launchStepError {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entries == nil {
		b.entries = map[string]*launchRetryState{}
	}
	state := b.entries[key]
	if state == nil {
		state = &launchRetryState{}
		b.entries[key] = state
	}
	state.failures++
	state.next = now.Add(launchRetryDelay(state.failures))
	state.lastErr = err.Error()
	var step *launchStepError
	var response *api.HTTPError
	if !errors.As(err, &step) || !launchErrorPermanent(step.err) || !errors.As(step.err, &response) {
		state.signature, state.permanent = "", 0
		return nil
	}
	signature := fmt.Sprintf("%s\x00%d\x00%d", step.op, step.revision, response.Status)
	if signature != state.signature {
		state.signature, state.permanent = signature, 0
	}
	state.permanent++
	if state.permanent < launchPermanentAttempts {
		return nil
	}
	return step
}

// lastError is the entry's latest launch error on this relay, if any.
func (b *launchRetryBook) lastError(key string) string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if state := b.entries[key]; state != nil {
		return state.lastErr
	}
	return ""
}

func (b *launchRetryBook) drop(key string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, key)
}

// postedStallNotices remembers the stall notices this relay process has
// saved, so a persisting stall costs no hub write per tick. After a restart
// the hub replays the saved notice instead of posting another.
var postedStallNotices sync.Map

var teamQueueProjectCursor atomic.Uint64
var teamHostBudgetLastLog atomic.Int64

func reportTeamHostBudget(err error, now time.Time) {
	if err == nil {
		return
	}
	stamp := now.Unix()
	for {
		prior := teamHostBudgetLastLog.Load()
		if stamp-prior < 60 {
			return
		}
		if teamHostBudgetLastLog.CompareAndSwap(prior, stamp) {
			fmt.Fprintf(os.Stderr, "[tt relay] parallel queue held: %v\n", err)
			return
		}
	}
}

func rotateQueueProjects(projects []string, start int) []string {
	if len(projects) < 2 {
		return projects
	}
	start %= len(projects)
	return append(append([]string(nil), projects[start:]...), projects[:start]...)
}

// The runner and the owner's release command share this host lock. A dead
// process releases the flock; all durable decisions remain on the hub.
func queueLaunchLock(hub, task, entry string) (*os.File, error) {
	if err := os.MkdirAll(relayDir(), 0700); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(strings.TrimRight(hub, "/") + "\x00" + task + "\x00" + entry))
	file, err := os.OpenFile(filepath.Join(relayDir(), fmt.Sprintf("queue-launch-%x.lock", sum[:12])), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("queue launch is active on this host: %w", err)
	}
	return file, nil
}

func unlockQueueLaunch(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func productionTeamRunner() teamRunner {
	return teamRunner{
		plan: func(ctx context.Context, in map[string]any, out *teamLaunchResolved) error {
			return teamplan.Run(ctx, in, out)
		},
		spawn: cmdSpawn,
		owned: func(ctx context.Context, e env, a api.Agent) error {
			owned, err := handlerOwned(ctx, e.hub, a.TaskID, a.ID, a.RunID, a.Session, nil)
			if err != nil {
				return err
			}
			if owned == nil {
				return errors.New("exact owned session missing")
			}
			return nil
		},
		cleanup: func(ctx context.Context, e env, task, id string) error {
			result, err := cleanupSessions(ctx, e, task, []string{id})
			if err != nil {
				return err
			}
			if len(result.Errors) > 0 {
				return errors.New(result.Errors[0])
			}
			return nil
		},
		integration: queueIntegrationSnapshot,
		census: func(ctx context.Context, c *api.Client, task, host string, policy api.TeamHostPolicy, prior *api.TeamHostUsage, cwds []string) error {
			return saveHostRelayCensus(ctx, c, task, host, policy, prior, cwds, time.Now())
		},
		worktrees:  closeoutWorktrees,
		roundRobin: true,
		retries:    productionLaunchRetries,
	}
}

// Slow queue listings (docs/project-overview.md, "Reading Board history").
// A listing that times out starts an episode: the tick keeps acting on the
// last good listing for the entries it already knows, however old, tries a
// live listing again only after a growing delay, and tells the owner helper
// once. The episode ends after queueListingRecoverTicks ticks in a row whose
// listings all answered, so a hub that flaps stays in one episode.
const (
	queueListingRetryFirst   = 6 * time.Second
	queueListingRetryMax     = time.Minute
	queueListingRecoverTicks = 3
	queueListingSlowTitle    = "Queue listing slow"
)

type queueListingAt struct {
	list api.TeamQueueList
	at   time.Time
}

type slowListingEpisode struct {
	start, next time.Time
	delay       time.Duration
	cause       error
	// good counts the ticks in a row whose listings all answered.
	good int
	// unowned records that the relay log was told no owner helper is bound
	// here to notice for a host listing with no last good copy.
	unowned bool
	// noticed holds the projects whose owner helper was told in this episode.
	noticed map[string]bool
}

// hubQueueListings is one hub's last good listings and its slow episode.
type hubQueueListings struct {
	mu      sync.Mutex
	host    map[string]queueListingAt
	active  map[string]queueListingAt
	episode *slowListingEpisode
}

var (
	queueListingsMu sync.Mutex
	queueListings   = map[string]*hubQueueListings{}
)

func queueListingsFor(hub string) *hubQueueListings {
	queueListingsMu.Lock()
	defer queueListingsMu.Unlock()
	hub = strings.TrimRight(hub, "/")
	h := queueListings[hub]
	if h == nil {
		h = &hubQueueListings{host: map[string]queueListingAt{}, active: map[string]queueListingAt{}}
		queueListings[hub] = h
	}
	return h
}

// resetQueueListings forgets every hub's listings and episodes.
func resetQueueListings() {
	queueListingsMu.Lock()
	defer queueListingsMu.Unlock()
	queueListings = map[string]*hubQueueListings{}
}

// queueListingTimedOut reports a listing that got no answer in time. Any
// other failure is a hub answer and keeps its own handling.
func queueListingTimedOut(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout())
}

// liveDue reports whether this tick may ask the hub for a listing.
func (h *hubQueueListings) liveDue(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.episode == nil || !now.Before(h.episode.next)
}

// timedOut starts or continues the episode and pushes the next live listing
// out: six seconds first, doubling to a minute.
func (h *hubQueueListings) timedOut(now time.Time, cause error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.episode == nil {
		h.episode = &slowListingEpisode{start: now, noticed: map[string]bool{}}
	}
	ep := h.episode
	ep.delay = min(max(2*ep.delay, queueListingRetryFirst), queueListingRetryMax)
	ep.next, ep.cause, ep.good = now.Add(ep.delay), cause, 0
}

// recovered counts a tick whose listings all answered. The episode ends at
// queueListingRecoverTicks such ticks in a row; until then the next tick
// lists live again, and a timeout continues the same episode.
func (h *hubQueueListings) recovered(now time.Time) {
	h.mu.Lock()
	ep := h.episode
	if ep == nil {
		h.mu.Unlock()
		return
	}
	ep.good++
	if ep.good < queueListingRecoverTicks {
		ep.next = now
		h.mu.Unlock()
		return
	}
	h.episode = nil
	h.mu.Unlock()
	fmt.Fprintf(os.Stderr, "[tt relay] team queue listing answers again after %s slow\n", now.Sub(ep.start).Round(time.Second))
}

// slowErr is what a tick reports when it has no last good listing to use.
func (h *hubQueueListings) slowErr() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.episode == nil {
		return errors.New("team queue listing unavailable")
	}
	return fmt.Errorf("team queue listing timed out with no last good listing; next attempt in %s: %w", h.episode.delay, h.episode.cause)
}

func (h *hubQueueListings) save(kind map[string]queueListingAt, key string, list api.TeamQueueList, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	kind[key] = queueListingAt{list: list, at: now}
}

// lastGood returns the saved listing, whatever its age. It is read only
// inside a slow episode, where it names the entries already known: a stale
// pass reads each in-progress entry again by itself and starts nothing.
func (h *hubQueueListings) lastGood(kind map[string]queueListingAt, key string) (queueListingAt, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	saved, ok := kind[key]
	return saved, ok
}

// noticeNoHostListing handles a host listing that timed out with no last good
// copy, so no project is known from the hub. It tells the owner helper of
// each project that has an owner helper bound to this hub in the relay state
// directory, once per episode, and with no such binding says so once in the
// relay log. It runs only on a tick that tried the live listing.
func (r teamRunner) noticeNoHostListing(ctx context.Context, c *api.Client, h *hubQueueListings, host string, now time.Time) {
	hub := strings.TrimRight(c.Base, "/")
	paths, _ := filepath.Glob(filepath.Join(relayDir(), "*.binding.json"))
	seen := map[string]bool{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var b runtimeBinding
		if json.Unmarshal(data, &b) != nil || !validBinding(b) || b.Role != api.AgentRoleOwnerHelper || strings.TrimRight(b.Hub, "/") != hub || seen[b.Task] {
			continue
		}
		seen[b.Task] = true
		r.noticeSlowListing(ctx, c, h, b.Task, host, nil, now)
	}
	if len(seen) > 0 {
		return
	}
	h.mu.Lock()
	ep := h.episode
	logged := ep == nil || ep.unowned
	if ep != nil {
		ep.unowned = true
	}
	h.mu.Unlock()
	if !logged {
		fmt.Fprintf(os.Stderr, "[tt relay] team queue host listing timed out with no last good listing, and no owner helper is bound on this host to be told\n")
	}
}

// noticeSlowListing tells the project's owner helper, once per episode, that
// the relay is working from an old listing or from none. A failed post is
// tried again on a later tick under the same request identity.
func (r teamRunner) noticeSlowListing(ctx context.Context, c *api.Client, h *hubQueueListings, taskID, host string, used *queueListingAt, now time.Time) {
	h.mu.Lock()
	ep := h.episode
	if ep == nil || ep.noticed[taskID] {
		h.mu.Unlock()
		return
	}
	start, cause := ep.start, ep.cause
	h.mu.Unlock()
	var helper api.Agent
	if detail, err := c.GetTask(ctx, taskID); err == nil {
		for _, a := range detail.Agents {
			if a.Role == api.AgentRoleOwnerHelper && a.Status != api.AgentClosed && a.Status != api.AgentExited && a.Status != api.AgentRetired {
				helper = a
			}
		}
	}
	basis := "It has no last good listing, so this project's queue is not advanced until a listing answers."
	if used != nil {
		basis = fmt.Sprintf("It is acting on the last good listing, now %s old: running teams still advance, queued entries start only after a live listing, and a parallel launch continues only on a tick whose host listing answered.", now.Sub(used.at).Round(time.Second))
	}
	limit := "its time limit"
	if c.HTTP != nil && c.HTTP.Timeout > 0 {
		limit = "the " + c.HTTP.Timeout.String() + " limit"
	}
	notice := api.Envelope{Kind: api.EnvelopeKindNotice, To: helper.Name, Subject: queueListingSlowTitle,
		Refs: map[string]string{"host": host, "project": taskID},
		Body: api.EnvelopeBody{Text: fmt.Sprintf("The relay on host %s got no answer to the team queue listing for project %s within %s (%v). %s It tries a live listing again after a delay that grows from %s to %s, and sends this once per slow episode. Look for one caller looping hub reads.",
			host, taskID, limit, cause, basis, queueListingRetryFirst, queueListingRetryMax)}}
	post := *c
	if c.HTTP != nil {
		marked := *c.HTTP
		marked.Transport = relayAuthorTransport{base: c.HTTP.Transport}
		post.HTTP = &marked
	}
	_, err := post.PostMessage(ctx, taskID, api.PostMessageRequest{Envelope: &notice, Text: api.RenderText(notice), To: helper.ID,
		RequestID: fmt.Sprintf("queue-listing-slow-%s-%d", taskID, start.Unix())})
	var response *api.HTTPError
	if err != nil && (!errors.As(err, &response) || response.Status != 409) {
		fmt.Fprintf(os.Stderr, "[tt relay] team queue project %s: slow listing notice: %v\n", taskID, err)
		return
	}
	h.mu.Lock()
	if h.episode == ep {
		ep.noticed[taskID] = true
	}
	h.mu.Unlock()
}

func (r teamRunner) tick(ctx context.Context, e env, c *api.Client, host string) error {
	now := r.clock()
	listings := queueListingsFor(c.Base)
	// live is whether this tick still asks the hub for listings. It is false
	// between the attempts of a slow episode, and after the first timeout of
	// this tick, so one tick waits out at most one slow listing.
	live := listings.liveDue(now)
	// hostTimedOut is whether this tick's own host listing timed out.
	hostTimedOut := false
	var list api.TeamQueueList
	if live {
		var err error
		if list, err = c.TeamQueueByHost(ctx, host); err == nil {
			listings.save(listings.host, host, list, now)
		} else if !queueListingTimedOut(err) {
			return err
		} else {
			listings.timedOut(now, err)
			live, hostTimedOut = false, true
		}
	}
	hostListed := live
	if !hostListed {
		saved, ok := listings.lastGood(listings.host, host)
		if !ok {
			if hostTimedOut {
				r.noticeNoHostListing(ctx, c, listings, host, now)
			}
			return listings.slowErr()
		}
		list = saved.list
	}
	var hostBudgetErr error
	// The census is a hub write from the listing's usage, so an old listing
	// takes none; nothing new launches from one either.
	if hostListed && list.HostPolicy != nil && len(list.Entries) > 0 && r.census != nil {
		domain, domainErr := canonicalLimiterDomain(c.Base)
		if domainErr != nil || domain != list.HostPolicy.LimiterDomain {
			hostBudgetErr = errors.New("host policy limiter domain differs from relay hub")
		} else if err := activeRelayBudget.configure(list.HostPolicy, time.Now()); err != nil {
			hostBudgetErr = err
		} else if err := r.census(ctx, c, list.Entries[0].TaskID, host, *list.HostPolicy, list.HostUsage, queueCwds(list.Entries)); err != nil {
			hostBudgetErr = fmt.Errorf("host relay binding census: %w", err)
		}
	}
	seen := map[string]bool{}
	projects := make([]string, 0)
	for _, entry := range list.Entries {
		if !seen[entry.TaskID] {
			seen[entry.TaskID] = true
			projects = append(projects, entry.TaskID)
		}
	}
	if r.roundRobin && len(projects) > 1 {
		start := int((teamQueueProjectCursor.Add(1) - 1) % uint64(len(projects)))
		projects = rotateQueueProjects(projects, start)
	}
	var projectErrors []error
	for _, taskID := range projects {
		var queue api.TeamQueueList
		if live {
			var err error
			if queue, err = c.ListTeamQueuePage(ctx, taskID, api.TeamQueueListOptions{View: api.TeamQueueViewActive}); err == nil {
				listings.save(listings.active, taskID, queue, now)
			} else if !queueListingTimedOut(err) {
				projectErrors = append(projectErrors, fmt.Errorf("team queue project %s: %w", taskID, err))
				continue
			} else {
				listings.timedOut(now, err)
				live = false
			}
		}
		// stale marks a project served from its last good listing.
		stale := !live
		if stale {
			saved, ok := listings.lastGood(listings.active, taskID)
			if !ok {
				r.noticeSlowListing(ctx, c, listings, taskID, host, nil, now)
				projectErrors = append(projectErrors, fmt.Errorf("team queue project %s: %w", taskID, listings.slowErr()))
				continue
			}
			r.noticeSlowListing(ctx, c, listings, taskID, host, &saved, now)
			queue = saved.list
		}
		parallel := queueParallel(queue.ConcurrencyLimit)
		for _, q := range queue.Entries {
			if q.Host != host {
				continue
			}
			if stale {
				// An old listing starts no launch, and names which teams are
				// in progress, not their state: each is read again by itself,
				// so the tick never acts on an entry that has since moved on.
				if q.State != "launching" && q.State != "running" && q.State != "failed" {
					continue
				}
				fresh, err := c.GetTeamQueueEntry(ctx, taskID, q.ID)
				if err != nil {
					projectErrors = append(projectErrors, fmt.Errorf("team queue %s: %w", q.ID, err))
					continue
				}
				if fresh.State == "queued" {
					continue
				}
				q = fresh
			}
			if q.State != "launching" {
				r.retries.drop(launchRetryKey(c.Base, q.TaskID, q.ID))
			}
			// An unsafe host observation cannot start or continue a parallel
			// launch, and a cached host listing is no observation: it took
			// no census. Serial teams and already-running parallel teams
			// still advance through their own close and cleanup paths.
			if parallel && (!hostListed || hostBudgetErr != nil || list.HostPolicy == nil) && (q.State == "queued" || q.State == "launching") {
				continue
			}
			if q.State == "failed" && q.OwnerIntegration != nil {
				// The owner integrated it; the runner still closes and
				// cleans its team once the item is terminal. It holds no
				// slot, so it does not take a serial project's turn.
				if err := r.finish(ctx, e, c, q, host); err != nil {
					projectErrors = append(projectErrors, fmt.Errorf("team queue %s: %w", q.ID, err))
				}
				continue
			}
			if q.State == "failed" && q.ReleasedAt == "" {
				// A serial queue halts until the owner reconciles. A parallel
				// one frees the slot and handler lease once the failed team
				// is closed and cleaned.
				if !parallel {
					break
				}
				if err := r.releaseFailed(ctx, e, c, q, host); err != nil {
					projectErrors = append(projectErrors, fmt.Errorf("team queue %s: %w", q.ID, err))
				}
				continue
			}
			if q.State != "queued" && q.State != "launching" && q.State != "running" {
				continue
			}
			if !parallel && q.State == "queued" && strings.HasPrefix(q.BlockReason, tokenBudgetPrefix) {
				// The hub lists this entry as held by the token budget and
				// skips it at the head, so it does not take a serial
				// project's turn and costs no request: the next entry is
				// tried. The hub still decides that entry's claim.
				continue
			}
			err := r.advance(ctx, e, c, q, host)
			if errors.Is(err, errQueuedAwaitsRebind) {
				// Nothing was done for this entry, so it does not take a
				// serial project's turn: the next entry is tried.
				continue
			}
			if err != nil {
				projectErrors = append(projectErrors, fmt.Errorf("team queue %s: %w", q.ID, err))
			}
			if !parallel {
				break
			}
		}
		if stale {
			// Worktree cleanup, handler provisioning and stall notices all
			// read the listing itself, so they wait for a live one.
			continue
		}
		if r.worktrees != nil {
			// Finished teams' worktrees go once their release lands. The
			// cleanup is local: it logs and never fails the tick or entry.
			for _, q := range queue.Entries {
				if q.Host != host || q.State != "finished" || q.Acceptance == nil {
					continue
				}
				if err := r.worktrees(ctx, c, host, list, queue, q); err != nil {
					fmt.Fprintf(os.Stderr, "[tt relay] worktree cleanup %s: %v\n", q.ID, err)
				}
			}
		}
		if !parallel || (hostBudgetErr == nil && list.HostPolicy != nil) {
			// A new handler is new load, so an unsafe host observation holds
			// it like any parallel launch.
			r.provisionHandler(ctx, e, c, queue, host)
		}
		r.noticeStalls(ctx, c, queue, host)
	}
	if r.roundRobin && hostListed {
		// A host census fault is local to new parallel launch effects. Returning
		// it would activate the relay's global queue backoff and delay serial
		// projects even though their own progress was safe.
		reportTeamHostBudget(hostBudgetErr, time.Now())
	}
	if live {
		listings.recovered(now)
	}
	return errors.Join(projectErrors...)
}

// errQueuedAwaitsRebind is advance's report that a queued entry was left in
// place because its item was amended. It is not a failure.
var errQueuedAwaitsRebind = errors.New("queued entry waits for a rebind to its amended item")

func (r teamRunner) advance(ctx context.Context, e env, c *api.Client, q api.TeamQueueEntry, host string) error {
	if q.State == "launching" && r.retries != nil && r.retries.waiting(launchRetryKey(c.Base, q.TaskID, q.ID), r.clock()) {
		return nil
	}
	detail, err := c.GetTask(ctx, q.TaskID)
	if err != nil {
		return err
	}
	if detail.Task.Status != api.TaskOpen || detail.Task.PauseState != api.ProjectPauseActive {
		return nil
	}
	if q.State == "queued" {
		queue, err := c.ListTeamQueuePage(ctx, q.TaskID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
		if err != nil {
			return err
		}
		if !queueParallel(queue.ConcurrencyLimit) && detail.Task.Orchestrator != "" {
			return nil
		}
		item, err := c.GetWorkItem(ctx, q.TaskID, q.ItemID)
		if err != nil {
			return err
		}
		if item.Status == "done" || item.Status == "dismissed" {
			return r.fail(ctx, c, q, errors.New("queued item changed before launch"))
		}
		if item.Revision != q.ItemRevision {
			// An amended item leaves its entry queued for tt team queue
			// rebind. The hub skips it at the head and lists the reason, so
			// later entries still launch; failing it would strand the item.
			return errQueuedAwaitsRebind
		}
		q, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: "queue-claim-" + q.ID, Operation: "claim", EntryID: q.ID, ExpectedRevision: q.Revision, Host: host, PauseGeneration: detail.Task.PauseGeneration})
		if err != nil {
			var response *api.HTTPError
			if errors.As(err, &response) && response.Status == 409 && claimRaceConflict(response.Msg) {
				// A competing runner or manual launch can make this queued
				// snapshot obsolete. Other conflicts need to reach the operator.
				return nil
			}
			return err
		}
	}
	if q.State == "launching" {
		return r.boundedLaunch(ctx, e, c, q, host)
	}
	if q.State == "running" {
		q = r.narrow(ctx, c, q)
		return r.finish(ctx, e, c, q, host)
	}
	return nil
}

// provisionedHandlers remembers, per process, the handler provisions whose
// spawn call returned without error, so a reservation that is still waiting
// for its handler to register is not spawned again on every tick. After a
// relay restart one replay reaches the handler launch journal, which resumes
// the same agent instead of starting another.
var provisionedHandlers sync.Map

// handlerProvisionAgentID derives the preallocated handler identity from the
// provision's retry identity, so a replay offers the same agent.
func handlerProvisionAgentID(hub, task, requestID string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(hub, "/") + "\x00" + task + "\x00" + requestID))
	return "agt_" + hex.EncodeToString(sum[:8])
}

// provisionHandler adds one database handler for the first queued entry of
// this host that waits only for a handler the hub says may be added
// (docs/handler-ab.md, "Automatic provisioning"). It offers the host's saved
// launch spec to the hub, which decides the exact match and reserves the
// handler; only then is the handler started, through the same spawn path as
// tt spawn. One attempt per project and pass; an error is logged and the
// pass continues.
func (r teamRunner) provisionHandler(ctx context.Context, e env, c *api.Client, queue api.TeamQueueList, host string) {
	if r.spawn == nil {
		return
	}
	hub := strings.TrimRight(c.Base, "/")
	for _, q := range queue.Entries {
		n := q.HandlerNeed
		if q.Host != host || q.State != "queued" || n == nil {
			continue
		}
		requestID := fmt.Sprintf("queue-provision-%s-%d-%d", q.ID, q.Revision, n.Attempt)
		key := hub + "\x00" + requestID
		_, spawned := provisionedHandlers.Load(key)
		// A standing refusal is offered again so a corrected spec is seen; a
		// reservation of this entry is replayed until its spawn succeeded.
		if spawned || (!n.Provision && !n.Refused && n.AgentID == "") {
			continue
		}
		started, err := r.provisionOne(ctx, e, c, q, hub, host, requestID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[tt relay] handler provision %s: %v\n", q.ID, err)
		}
		if started {
			provisionedHandlers.Store(key, true)
		}
		return
	}
}

// provisionOne reports whether the reserved handler's launch returned
// without error. A 409 is the hub's refusal: nothing is started and the
// entry's listed reason says why.
func (r teamRunner) provisionOne(ctx context.Context, e env, c *api.Client, q api.TeamQueueEntry, hub, host, requestID string) (bool, error) {
	spec, specErr := loadHandlerSpec(hub, q.TaskID)
	if specErr != nil {
		// An unreadable spec is offered as a missing one, so the entry's
		// reason names the host and the command that saves it again.
		fmt.Fprintf(os.Stderr, "[tt relay] handler provision %s: %v\n", q.ID, specErr)
		spec = nil
	}
	agentID := handlerProvisionAgentID(hub, q.TaskID, requestID)
	req := api.TeamQueueRequest{RequestID: requestID, Operation: "provision_handler", EntryID: q.ID, ExpectedRevision: q.Revision, Host: host, HandlerAgentID: agentID}
	if spec != nil {
		req.HandlerSpecRuntime, req.HandlerSpecModel, req.HandlerSpecReasoning = spec.runtime(), spec.value("model"), spec.value("reasoning")
		req.HandlerSpecDigest = handlerTemplateDigest(spec.value("prompt"))
		// The other launch flags let a refusal print a complete command; the
		// prompt and the three compared settings are not repeated.
		for i := 0; i+1 < len(spec.Args); i += 2 {
			switch spec.Args[i] {
			case "--prompt", "--runtime", "--model", "--reasoning":
			default:
				req.HandlerSpecArgs = append(req.HandlerSpecArgs, spec.Args[i], spec.Args[i+1])
			}
		}
	}
	if _, err := c.TeamQueueAction(ctx, q.TaskID, req); err != nil {
		var response *api.HTTPError
		if errors.As(err, &response) && response.Status == 409 {
			return false, nil
		}
		return false, err
	}
	if spec == nil {
		return false, errors.New("the hub reserved a handler without a saved launch spec")
	}
	args := append(append([]string(nil), spec.Args...), "--role", api.AgentRoleDatabaseHandler, "--agent-id", agentID,
		"--name", "db-handler-auto-"+agentID[len(agentID)-6:], "--task", q.TaskID, "--hub", hub)
	launch := e
	launch.hub, launch.task, launch.agent, launch.agentName, launch.runID = hub, q.TaskID, "", "", ""
	if err := r.spawn(launch, args); err != nil {
		return false, fmt.Errorf("start reserved handler %s: %w", agentID, err)
	}
	return true, nil
}

// narrow shrinks an accepted running entry's ownership to the files its
// candidate actually changed, so queued work that overlapped only unchanged
// paths can start while the team closes. It is an optimization: a Git error,
// an empty result or a refused write leaves the entry as it was.
func (r teamRunner) narrow(ctx context.Context, c *api.Client, q api.TeamQueueEntry) api.TeamQueueEntry {
	if q.Acceptance == nil || q.Repository == "" || q.ReleasedAt != "" {
		return q
	}
	changed := r.changed
	if changed == nil {
		changed = queueChangedFiles
	}
	files, err := changed(ctx, q.Repository, q.Acceptance.BaseCommit, q.Acceptance.Commit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[tt relay] team queue %s: narrowing skipped: %v\n", q.ID, err)
		return q
	}
	owned := ownedChanges(files, q.Ownership)
	if len(owned) == 0 || len(owned) > 256 || sameOwnership(owned, q.Ownership) {
		return q
	}
	narrowed, err := c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: fmt.Sprintf("queue-narrow-%s-%d", q.ID, q.Revision), Operation: "scope", EntryID: q.ID, ExpectedRevision: q.Revision, Ownership: owned})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[tt relay] team queue %s: narrowing skipped: %v\n", q.ID, err)
		return q
	}
	return narrowed
}

// noticeStalls posts one Board notice for each stall the hub explains on this
// host's queued or launching entries once it has held past the grace period.
// The hub recomputes the stall and refuses a stale one; a refusal is not an
// error. A launch's notice carries this relay's last error for it.
func (r teamRunner) noticeStalls(ctx context.Context, c *api.Client, queue api.TeamQueueList, host string) {
	grace := r.stallGrace
	if grace <= 0 {
		grace = 5 * time.Minute
	}
	for _, q := range queue.Entries {
		if (q.State != "queued" && q.State != "launching") || q.Stall == nil || q.Host != host {
			continue
		}
		since, err := time.Parse(time.RFC3339Nano, q.Stall.Since)
		if err != nil || time.Since(since) < grace {
			continue
		}
		id := q.Stall.NoticeRequestID(q.ID)
		if _, posted := postedStallNotices.Load(id); posted {
			continue
		}
		var lastErr string
		if q.Stall.BlockerEntryID != "" {
			lastErr = r.retries.lastError(launchRetryKey(c.Base, q.TaskID, q.Stall.BlockerEntryID))
			if runes := []rune(lastErr); len(runes) > launchErrorNoticeRunes {
				lastErr = string(runes[:launchErrorNoticeRunes])
			}
		}
		_, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: id, Operation: "stall_notice", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: lastErr})
		var response *api.HTTPError
		if err == nil {
			postedStallNotices.Store(id, true)
		} else if !errors.As(err, &response) || response.Status != 409 {
			fmt.Fprintf(os.Stderr, "[tt relay] team queue %s: stall notice: %v\n", q.ID, err)
		}
	}
}

// queueParallel mirrors the hub: 1 is the serial queue, 0 has no fixed cap
// and N >= 2 is an owner ceiling.
func queueParallel(limit int) bool { return limit != 1 }

// queueCwds lists the launch folders of a host's queue entries for the disk
// census.
func queueCwds(entries []api.TeamQueueEntry) []string {
	var cwds []string
	for _, q := range entries {
		if q.Cwd != "" {
			cwds = append(cwds, q.Cwd)
		}
	}
	return cwds
}

// releaseFailed frees a failed parallel entry's slot and handler lease once
// every run bound to its item is closed and cleaned. It checks the roster
// first, so an unreleasable entry costs no hub write per tick; the hub
// repeats the exact release checks.
func (r teamRunner) releaseFailed(ctx context.Context, e env, c *api.Client, q api.TeamQueueEntry, host string) error {
	if q.Host != host {
		return nil
	}
	detail, err := c.GetTask(ctx, q.TaskID)
	if err != nil {
		return err
	}
	if detail.Task.Status != api.TaskOpen || detail.Task.PauseState != api.ProjectPauseActive {
		return nil
	}
	for _, a := range detail.Agents {
		if a.WorkItem != nil && a.WorkItem.ItemTaskID == q.TaskID && a.WorkItem.ItemID == q.ItemID && (a.Status != api.AgentClosed || !a.CleanupDone) {
			return nil
		}
	}
	lock, err := queueLaunchLock(e.hub, q.TaskID, q.ID)
	if err != nil {
		return nil // The owner's release or a launch holds this entry.
	}
	defer unlockQueueLaunch(lock)
	req, unlock, err := queueReleaseRequest(ctx, c, e.hub, q.TaskID, q, true)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = c.TeamQueueAction(ctx, q.TaskID, req)
	var response *api.HTTPError
	if errors.As(err, &response) && response.Status == 409 {
		return nil // The list names what still holds the entry.
	}
	return err
}

// projectAgentCapPrefix starts the hub's reason for holding a team that does
// not fit under the project's open-agent cap (docs/project-queue.md, "Project
// agent cap").
const projectAgentCapPrefix = "project agent cap: "

// tokenBudgetPrefix starts the hub's listed reason for a queued entry the
// token budget holds (docs/usage-accounting.md, "Admission reasons").
const tokenBudgetPrefix = "Token budget"

// registrationAtAgentCap reports a spawn whose agent registration the hub
// refused because the project is at its open-agent cap. Registration comes
// before any host session, so nothing was started.
func registrationAtAgentCap(err error) bool {
	var response *api.HTTPError
	return errors.As(err, &response) && response.Status == 409 && strings.HasSuffix(response.Msg, api.ErrLimit.Error())
}

func claimRaceConflict(message string) bool {
	// The team does not fit under the project agent cap yet: it waits queued
	// and the list names the counts.
	if strings.Contains(message, projectAgentCapPrefix) {
		return true
	}
	for _, cause := range []string{
		"entry revision changed",
		"project is not launchable",
		"not queue head",
		"launch is reserved",
		"all team slots are reserved",
		"no available database handler lease",
		"manual launch is reserved",
		// Handler arm waits (docs/handler-ab.md): the drawn arm is busy, or
		// every arm is at a provider limit.
		api.HandlerArmWaitSuffix,
	} {
		if strings.HasSuffix(message, cause) {
			return true
		}
	}
	return false
}

func (r teamRunner) fail(ctx context.Context, c *api.Client, q api.TeamQueueEntry, cause error) error {
	_, err := c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: "queue-fail-" + q.ID, Operation: "fail", EntryID: q.ID, ExpectedRevision: q.Revision, Failure: cause.Error()})
	if err != nil {
		return fmt.Errorf("%v; record failure: %w", cause, err)
	}
	return cause
}

func (r teamRunner) verifyMember(ctx context.Context, e env, c *api.Client, q api.TeamQueueEntry, m teamLaunchMember) (api.Agent, error) {
	a, err := c.GetAgent(ctx, q.TaskID, m.Fields.AgentID)
	if err != nil {
		return a, err
	}
	if a.Name != m.Fields.Name || a.Host != q.Host || a.WorkItem == nil || a.WorkItem.ItemID != q.ItemID || a.WorkItem.ItemRevision != q.ItemRevision || a.WorkItem.WorkOrderMessage.Seq != q.OrderMessageSeq || a.Status == api.AgentClosed || a.Status == api.AgentExited || (m.RunID != "" && a.RunID != m.RunID) {
		return a, errors.New("exact agent registration differs from frozen member")
	}
	if err := r.owned(ctx, e, a); err != nil {
		return a, err
	}
	return a, nil
}

// boundedLaunch paces a launch that keeps failing and fails the entry once
// the hub has refused the same write launchPermanentAttempts times.
func (r teamRunner) boundedLaunch(ctx context.Context, e env, c *api.Client, q api.TeamQueueEntry, host string) error {
	err := r.launch(ctx, e, c, q, host)
	if r.retries == nil {
		return err
	}
	key := launchRetryKey(c.Base, q.TaskID, q.ID)
	if err == nil {
		r.retries.drop(key)
		return nil
	}
	refused := r.retries.record(key, err, r.clock())
	if refused == nil {
		return err
	}
	// The text names the constant bound, so a retried fail sends the same
	// payload.
	q.Revision = refused.revision
	cause := fmt.Errorf("launch %s refused permanently by the hub after %d attempts: %v", refused.op, launchPermanentAttempts, refused.err)
	failErr := r.fail(ctx, c, q, cause)
	if failErr == cause {
		r.retries.drop(key)
	}
	return failErr
}

func (r teamRunner) launch(ctx context.Context, e env, c *api.Client, q api.TeamQueueEntry, host string) error {
	lock, lockErr := queueLaunchLock(e.hub, q.TaskID, q.ID)
	if lockErr != nil {
		return lockErr
	}
	defer unlockQueueLaunch(lock)
	detail, err := c.GetTask(ctx, q.TaskID)
	if err != nil {
		return err
	}
	if detail.Task.PauseGeneration != q.PauseGeneration {
		return r.fail(ctx, c, q, errors.New("project pause generation changed during launch"))
	}
	if q.Host != host {
		return nil
	}
	queueState, err := c.ListTeamQueuePage(ctx, q.TaskID, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
	if err != nil {
		return err
	}
	parallel := queueParallel(queueState.ConcurrencyLimit)
	var journal teamLaunchJournal
	if len(q.LaunchJSON) == 0 {
		item, err := c.GetWorkItem(ctx, q.TaskID, q.ItemID)
		if err != nil {
			return err
		}
		if item.Revision != q.ItemRevision || item.Status == "done" || item.Status == "dismissed" {
			return r.fail(ctx, c, q, errors.New("item changed before frozen launch"))
		}
		if q.Template == "small" && item.Kind != "bug" {
			return r.fail(ctx, c, q, fmt.Errorf("the small-change lane launches only bugs; this item is a %s: requeue it as Planned delivery", item.Kind))
		}
		var handler *api.Agent
		for i := range detail.Agents {
			a := &detail.Agents[i]
			if a.ID == q.HandlerID && a.RunID == q.HandlerRunID && a.Role == api.AgentRoleDatabaseHandler && a.Online && a.Status != api.AgentRetired && a.Status != api.AgentClosed && a.Status != api.AgentExited {
				handler = a
			}
		}
		if handler == nil {
			return nil
		}
		var resolved teamLaunchResolved
		input := map[string]any{"hub": e.hub, "token": e.token, "task": q.TaskID, "item": q.ItemID, "revision": q.ItemRevision, "order": q.OrderMessageSeq, "template": q.Template, "cwd": q.Cwd, "host": host, "handler": handler}
		if err := r.plan(ctx, input, &resolved); err != nil {
			return r.fail(ctx, c, q, fmt.Errorf("prepare team plan: %w", err))
		}
		if len(resolved.Plan) == 0 || !json.Valid(resolved.ItemRouting.WorkContextBundle) {
			return r.fail(ctx, c, q, errors.New("team plan is incomplete"))
		}
		journal = teamLaunchJournal{Version: 1, Hub: e.hub, Task: q.TaskID, Item: q.ItemID, Revision: q.ItemRevision, Order: q.OrderMessageSeq, HandlerID: handler.ID, HandlerRunID: handler.RunID, HandlerLeaseGeneration: q.HandlerLeaseGeneration, Context: resolved.ItemRouting.WorkContextBundle}
		for _, member := range resolved.Plan {
			f := member.Fields
			f.AgentID = api.NewID("agt")
			journal.Members = append(journal.Members, teamLaunchMember{Fields: f, State: "unstarted", RunID: api.NewID("run")})
		}
		frozen, _ := json.Marshal(journal)
		revision := q.Revision
		q, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: "queue-freeze-" + q.ID, Operation: "freeze", EntryID: q.ID, ExpectedRevision: q.Revision, LaunchJSON: frozen})
		if err != nil {
			return &launchStepError{op: "freeze", revision: revision, err: err}
		}
	} else if err := json.Unmarshal(q.LaunchJSON, &journal); err != nil {
		return r.fail(ctx, c, q, err)
	}
	if len(journal.Members) == 0 || journal.Task != q.TaskID || journal.Item != q.ItemID || journal.Revision != q.ItemRevision || journal.Order != q.OrderMessageSeq || ((parallel || journal.HandlerID != "") && (journal.HandlerID != q.HandlerID || journal.HandlerRunID != q.HandlerRunID || journal.HandlerLeaseGeneration != q.HandlerLeaseGeneration)) {
		return r.fail(ctx, c, q, errors.New("frozen team identity conflicts with queue entry"))
	}
	lead := journal.Members[0].Fields.Name
	if !parallel && detail.Task.Orchestrator == "" {
		if _, err := c.UpdateTask(ctx, q.TaskID, api.UpdateTaskRequest{Orchestrator: &lead, TeamLaunchToken: "queue-claim-" + q.ID}); err != nil {
			return r.fail(ctx, c, q, err)
		}
	}
	for i := range journal.Members {
		member := &journal.Members[i]
		fresh, err := c.GetTask(ctx, q.TaskID)
		if err != nil {
			return err
		}
		if fresh.Task.PauseState != api.ProjectPauseActive {
			return nil
		}
		if fresh.Task.PauseGeneration != q.PauseGeneration {
			return r.fail(ctx, c, q, errors.New("project pause generation changed during launch"))
		}
		available := false
		for _, a := range fresh.Agents {
			if a.ID == journal.HandlerID && a.RunID == q.HandlerRunID && a.Role == api.AgentRoleDatabaseHandler && a.Online && a.Status != api.AgentRetired && a.Status != api.AgentClosed && a.Status != api.AgentExited {
				available = true
			}
		}
		if !available {
			return nil
		}
		if !parallel && fresh.Task.Orchestrator != lead {
			return r.fail(ctx, c, q, errors.New("lead changed during frozen launch"))
		}
		current, err := c.GetWorkItem(ctx, q.TaskID, q.ItemID)
		if err != nil {
			return err
		}
		if current.Revision != q.ItemRevision || current.Status == "done" || current.Status == "dismissed" {
			return r.fail(ctx, c, q, errors.New("item changed during frozen launch"))
		}
		if member.State == "started" {
			if _, err := r.verifyMember(ctx, e, c, q, *member); err != nil {
				return r.fail(ctx, c, q, fmt.Errorf("started member %s changed: %w", member.Fields.AgentID, err))
			}
			continue
		}
		if member.State == "uncertain" {
			a, err := r.verifyMember(ctx, e, c, q, *member)
			if err != nil {
				return r.fail(ctx, c, q, fmt.Errorf("uncertain spawn %s has no exact registration; never respawn: %w", member.Fields.AgentID, err))
			}
			revision := q.Revision
			q, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: fmt.Sprintf("queue-started-%s-%d", q.ID, i), Operation: "started", EntryID: q.ID, ExpectedRevision: q.Revision, MemberIndex: i, MemberRunID: a.RunID})
			if err != nil {
				return &launchStepError{op: "started", revision: revision, err: err}
			}
			continue
		}
		if member.State != "unstarted" {
			return r.fail(ctx, c, q, errors.New("invalid frozen member state"))
		}
		revision := q.Revision
		q, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: fmt.Sprintf("queue-attempt-%s-%d-%d", q.ID, i, q.Revision), Operation: "attempt", EntryID: q.ID, ExpectedRevision: q.Revision, MemberIndex: i})
		if err != nil {
			return &launchStepError{op: "attempt", revision: revision, err: err}
		}
		contextFile, err := os.CreateTemp("", "tt-team-context-*.json")
		if err != nil {
			return r.fail(ctx, c, q, err)
		}
		if _, err = contextFile.Write(journal.Context); err != nil {
			contextFile.Close()
			os.Remove(contextFile.Name())
			return r.fail(ctx, c, q, err)
		}
		contextFile.Close()
		f := member.Fields
		allowed, _ := json.Marshal(f.AllowedTools)
		args := []string{"--name", f.Name, "--run", f.Run, "--runtime", f.Runtime, "--model", f.Model, "--reasoning", f.Reasoning, "--cwd", f.Cwd, "--prompt", f.Prompt, "--agent-id", f.AgentID, "--expected-run-id", member.RunID, "--task", q.TaskID, "--hub", e.hub, "--work-item", q.ItemID, "--work-item-revision", strconv.FormatInt(q.ItemRevision, 10), "--work-order-message", strconv.FormatInt(q.OrderMessageSeq, 10), "--work-context-file", contextFile.Name(), "--planned-team-members", strconv.Itoa(len(journal.Members)), "--team-lead-name", lead, "--team-handler-id", q.HandlerID, "--allowed-tools-json", string(allowed)}
		if f.PermissionMode != "" {
			args = append(args, "--permission-mode", f.PermissionMode)
		}
		if f.ApprovalMode != "" {
			args = append(args, "--approval-mode", f.ApprovalMode)
		}
		if f.SandboxMode != "" {
			args = append(args, "--sandbox-mode", f.SandboxMode)
		}
		spawnErr := r.spawn(e, args)
		os.Remove(contextFile.Name())
		if spawnErr != nil {
			if registrationAtAgentCap(spawnErr) {
				// The project filled between the attempt and registration.
				// The member returns to unstarted and the launch backs off;
				// the entry is not failed and started members stay.
				revision := q.Revision
				if _, err := c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: fmt.Sprintf("queue-unattempt-%s-%d-%d", q.ID, i, q.Revision), Operation: "unattempt", EntryID: q.ID, ExpectedRevision: q.Revision, MemberIndex: i}); err != nil {
					return &launchStepError{op: "unattempt", revision: revision, err: err}
				}
				roster, err := c.GetTask(ctx, q.TaskID)
				if err != nil {
					return err
				}
				open := 0
				for _, a := range roster.Agents {
					if a.Status != api.AgentClosed && a.Status != api.AgentExited {
						open++
					}
				}
				return &launchStepError{op: "spawn " + f.Name, revision: revision, err: fmt.Errorf("%s%d open + 1 seats > %d: %w", projectAgentCapPrefix, open, api.MaxAgentsPerTask, spawnErr)}
			}
			return r.fail(ctx, c, q, fmt.Errorf("spawn %s: %w", f.Name, spawnErr))
		}
		a, err := r.verifyMember(ctx, e, c, q, *member)
		if err != nil {
			return r.fail(ctx, c, q, fmt.Errorf("spawn registration %s: %w", f.Name, err))
		}
		revision = q.Revision
		q, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: fmt.Sprintf("queue-started-%s-%d", q.ID, i), Operation: "started", EntryID: q.ID, ExpectedRevision: q.Revision, MemberIndex: i, MemberRunID: a.RunID})
		if err != nil {
			return &launchStepError{op: "started", revision: revision, err: err}
		}
	}
	if _, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: "queue-running-" + q.ID, Operation: "running", EntryID: q.ID, ExpectedRevision: q.Revision}); err != nil {
		return &launchStepError{op: "running", revision: q.Revision, err: err}
	}
	return nil
}

func (r teamRunner) finish(ctx context.Context, e env, c *api.Client, q api.TeamQueueEntry, host string) error {
	item, err := c.GetWorkItem(ctx, q.TaskID, q.ItemID)
	if err != nil {
		return err
	}
	if item.Status != "done" && item.Status != "dismissed" {
		return nil
	}
	var closeReq api.TeamCloseRequest
	if len(q.CloseJSON) == 0 {
		detail, err := c.GetTask(ctx, q.TaskID)
		if err != nil {
			return err
		}
		itemLeadLive := false
		legacyLeadLive := false
		for _, a := range detail.Agents {
			if a.ItemLead && a.WorkItem != nil && a.WorkItem.ItemID == q.ItemID && a.Status != api.AgentClosed {
				itemLeadLive = true
			}
			if a.WorkItem != nil && a.WorkItem.ItemID == q.ItemID && strings.EqualFold(a.Name, detail.Task.Orchestrator) && a.Status != api.AgentClosed {
				legacyLeadLive = true
			}
		}
		if !itemLeadLive && !legacyLeadLive {
			var journal teamLaunchJournal
			if json.Unmarshal(q.LaunchJSON, &journal) != nil || len(journal.Members) == 0 || journal.Members[0].RunID == "" {
				return r.fail(ctx, c, q, errors.New("closed lead has no frozen exact run"))
			}
			// The owner may have handed the lead to another member of this item.
			// Locate the exact saved receipt by its lead run, including the
			// replacement, instead of assuming the originally spawned lead.
			for _, a := range detail.Agents {
				if a.WorkItem == nil || a.WorkItem.ItemID != q.ItemID || a.WorkItem.ItemTaskID != q.TaskID {
					continue
				}
				candidate := api.TeamCloseRequest{RequestID: "team-close-" + q.TaskID + "-" + a.RunID, ItemID: q.ItemID, ItemRevision: q.ItemRevision}
				result, lookupErr := c.GetTeamCloseReceipt(ctx, q.TaskID, candidate.RequestID)
				if lookupErr == nil && result.TaskID == q.TaskID && result.ItemID == q.ItemID && result.LeadAgentID == a.ID {
					closeReq = candidate
					break
				}
				if lookupErr != nil {
					var response *api.HTTPError
					if !errors.As(lookupErr, &response) || response.Status != 404 {
						return lookupErr
					}
				}
			}
			if closeReq.RequestID == "" {
				return r.fail(ctx, c, q, errors.New("closed lead has no exact team close receipt"))
			}
		} else {
			closeReq, err = closeTeamSnapshotForItem(detail.Task, detail.Agents, "", "", q.ItemID)
			if err != nil {
				return r.fail(ctx, c, q, fmt.Errorf("close gate: %w", err))
			}
			// The close request identity is frozen with its exact team snapshot.
			// A definite refusal can refresh the snapshot without reusing a key
			// that might already identify a different receipt.
			closeReq.RequestID = ""
			identity, _ := json.Marshal(closeReq)
			sum := sha256.Sum256(identity)
			closeReq.RequestID = fmt.Sprintf("queue-close-%s-%x", q.ID, sum[:12])
		}
		if closeReq.ItemID != q.ItemID {
			return r.fail(ctx, c, q, errors.New("close snapshot selected another item"))
		}
		data, _ := json.Marshal(closeReq)
		q, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: fmt.Sprintf("queue-close-freeze-%s-%d", q.ID, q.Revision), Operation: "close", EntryID: q.ID, ExpectedRevision: q.Revision, CloseJSON: data})
		if err != nil {
			return err
		}
	} else if err := json.Unmarshal(q.CloseJSON, &closeReq); err != nil {
		return r.fail(ctx, c, q, err)
	}
	result, err := c.GetTeamCloseReceipt(ctx, q.TaskID, closeReq.RequestID)
	if err != nil {
		var response *api.HTTPError
		if !errors.As(err, &response) || response.Status != 404 {
			return err
		}
		result, err = c.CloseItemTeam(ctx, q.TaskID, closeReq)
		if err != nil {
			var response *api.HTTPError
			if errors.As(err, &response) && response.Status == 409 && response.Code == "team-close-obligations" {
				// Nothing in the frozen team snapshot changed. Keep its exact
				// request identity and wait without another hub write.
				return nil
			}
			if errors.As(err, &response) && response.Status == 409 && response.Code == "team-close-snapshot" {
				_, refreshErr := c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: fmt.Sprintf("queue-close-refresh-%s-%d", q.ID, q.Revision), Operation: "close_refresh", EntryID: q.ID, ExpectedRevision: q.Revision, CloseRequestID: closeReq.RequestID})
				return refreshErr
			}
			return r.fail(ctx, c, q, fmt.Errorf("team close: %w", err))
		}
	}
	for _, m := range result.Members {
		if m.Host != host {
			return r.fail(ctx, c, q, fmt.Errorf("member %s cleanup is on another host", m.AgentID))
		}
		if err := r.cleanup(ctx, e, q.TaskID, m.AgentID); err != nil {
			return r.fail(ctx, c, q, fmt.Errorf("member %s cleanup: %w", m.AgentID, err))
		}
	}
	detail, err := c.GetTask(ctx, q.TaskID)
	if err != nil {
		return err
	}
	for _, m := range result.Members {
		found := false
		for _, a := range detail.Agents {
			if a.ID == m.AgentID && a.RunID == m.RunID && a.CleanupDone {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}
	var integration *api.TeamIntegrationReady
	if item.Status == "done" && q.Repository != "" && q.OwnerIntegration == nil {
		if q.Acceptance == nil {
			return nil // The assigned handler has not saved exact Git acceptance yet.
		}
		if r.integration == nil {
			return r.fail(ctx, c, q, errors.New("integration snapshot provider is unavailable"))
		}
		integration, err = r.integration(ctx, q, item, closeReq)
		if err != nil {
			return r.fail(ctx, c, q, fmt.Errorf("integration snapshot: %w", err))
		}
	}
	_, err = c.TeamQueueAction(ctx, q.TaskID, api.TeamQueueRequest{RequestID: "queue-finish-" + q.ID, Operation: "finish", EntryID: q.ID, ExpectedRevision: q.Revision, Integration: integration})
	return err
}

func relayTeamQueueTick(ctx context.Context) error {
	e := env{hub: os.Getenv(spawn.EnvHub), token: os.Getenv("TAILTERM_TOKEN")}
	e.loadConfig()
	if e.hub == "" {
		return nil
	}
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	attachRelayBudget(c, activeRelayBudget)
	return productionTeamRunner().tick(ctx, e, c, spawn.Host())
}
