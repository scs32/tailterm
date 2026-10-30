package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/scs32/tailterm/hub/internal/api"
)

// A stall is queued work that waits only on something nothing will clear by
// itself: a failed, dead or idle entry that still holds resources, or a
// project with no database handler. Waiting on working teams, host capacity
// or disk is ordinary queueing, never a stall.
const (
	defaultQueueIdleThreshold = 30 * time.Minute
	defaultQueueStallGrace    = 5 * time.Minute
	// A reported matrix wait stops counting this long after its since: the
	// host lock's 240-minute wait limit plus slack, and above its 122-minute
	// holder limit. It only matters when the relay stops reporting.
	defaultQueueMatrixWaitBound = 4*time.Hour + 30*time.Minute
)

// SetQueueStallTiming overrides the idle threshold and the notice grace;
// zero keeps a default. Isolated tests use it.
func (s *Store) SetQueueStallTiming(idle, grace time.Duration) {
	s.queueIdleThreshold, s.queueStallGrace = idle, grace
}

func (s *Store) queueIdleAfter() time.Duration {
	if s.queueIdleThreshold > 0 {
		return s.queueIdleThreshold
	}
	return defaultQueueIdleThreshold
}

func (s *Store) queueNoticeGrace() time.Duration {
	if s.queueStallGrace > 0 {
		return s.queueStallGrace
	}
	return defaultQueueStallGrace
}

// stallBlocker is a resource-holding entry that nothing is moving.
type stallBlocker struct {
	cause string
	since time.Time
	fix   string
}

func parseStallTime(values ...string) time.Time {
	var latest time.Time
	for _, v := range values {
		if v == "" {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, v); err == nil && at.After(latest) {
			latest = at
		}
	}
	return latest
}

// classifyStallBlocker reports whether a resource-holding entry is stuck and
// why, with the durable time it has been so.
func (s *Store) classifyStallBlocker(ctx context.Context, q queryRower, e api.TeamQueueEntry, parallel bool) (*stallBlocker, error) {
	b, _, err := s.classifyStallBlockerHeld(ctx, q, e, parallel)
	return b, err
}

// classifyStallBlockerHeld is classifyStallBlocker that also returns the
// project agent cap reason of a launch the cap holds, which is no stall.
func (s *Store) classifyStallBlockerHeld(ctx context.Context, q queryRower, e api.TeamQueueEntry, parallel bool) (*stallBlocker, string, error) {
	if !queueEntryHoldsResources(e) {
		return nil, "", nil
	}
	shipped := fmt.Sprintf("tt team queue integrated --task %s --entry %s --commit SHA", e.TaskID, e.ID)
	if e.State == "failed" {
		live, err := liveItemRuns(ctx, q, e.TaskID, e.ItemID)
		if err != nil {
			return nil, "", err
		}
		if parallel && live == 0 {
			return nil, "", nil // the runner releases it on its next pass
		}
		fix := fmt.Sprintf("if its candidate shipped, %s; otherwise close its team once the item is terminal (tt close --team --task %s --item %s) so it can be released", shipped, e.TaskID, e.ItemID)
		if !parallel && live == 0 {
			fix = fmt.Sprintf("if its candidate shipped, %s; otherwise tt team queue release --task %s --entry %s", shipped, e.TaskID, e.ID)
		}
		return &stallBlocker{cause: api.StallFailedEntry, since: parseStallTime(e.UpdatedAt), fix: fix}, "", nil
	}
	var live int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id WHERE b.item_task_id=? AND b.item_id=? AND a.status NOT IN ('closed','exited')`, e.TaskID, e.ItemID).Scan(&live); err != nil {
		return nil, "", err
	}
	if live == 0 {
		// A launch whose next member registration the project agent cap
		// refuses waits for a seat, as the attempt does: failing it frees
		// nothing the cap holds (wi_d85481431355ef78).
		if e.State == "launching" {
			var open int
			if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE task_id=? AND status NOT IN ('closed','exited')`, e.TaskID).Scan(&open); err != nil {
				return nil, "", err
			}
			if reason := projectAgentCapReason(open, 0, 1, s.MaxAgents); reason != "" {
				return nil, reason, nil
			}
		}
		var last sql.NullString
		if err := q.QueryRowContext(ctx, `SELECT MAX(a.last_event_at) FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id WHERE b.item_task_id=? AND b.item_id=? AND a.status IN ('closed','exited')`, e.TaskID, e.ItemID).Scan(&last); err != nil {
			return nil, "", err
		}
		// Held since the later of the item's last run and the entry's own
		// last write: a relaunch is not dated from an earlier team's exit.
		since := parseStallTime(last.String, e.UpdatedAt)
		if e.State == "launching" {
			// A launch the cap held retries on its backoff once a seat
			// frees, and refused attempts write nothing, so it is dated from
			// the latest seat freed at the full cap after its last write
			// (wi_dbb1529b84b724ae).
			freed, err := s.capSeatFreedAfter(ctx, q, e.TaskID, parseStallTime(e.UpdatedAt))
			if err != nil {
				return nil, "", err
			}
			if freed.After(since) {
				since = freed
			}
		}
		// A launch in progress has no runs yet; only an entry that has had
		// none for the whole grace period is stuck.
		if s.now().Sub(since) < s.queueNoticeGrace() {
			return nil, "", nil
		}
		return &stallBlocker{cause: api.StallNothingRunning, since: since, fix: fmt.Sprintf("if its candidate shipped, %s; otherwise tt team queue fail --task %s --entry %s --reason TEXT, and the runner releases it", shipped, e.TaskID, e.ID)}, "", nil
	}
	if e.State != "running" {
		return nil, "", nil
	}
	// idle-entry: every live member idle or done, no open work between them
	// and anyone, for at least the idle threshold. A provider-blocked member
	// makes no progress, so it counts as idle.
	rows, err := q.QueryContext(ctx, `SELECT a.status,COALESCE(x.state,''),COALESCE(x.observed_at,''),COALESCE(x.payload,'') FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id
 LEFT JOIN agent_activity x ON x.agent_id=b.agent_id AND x.run_id=b.run_id
 WHERE b.item_task_id=? AND b.item_id=? AND a.status NOT IN ('closed','exited')`, e.TaskID, e.ItemID)
	if err != nil {
		return nil, "", err
	}
	since := parseStallTime(e.UpdatedAt)
	idle := true
	for rows.Next() {
		var status, state, observed, payload string
		if err := rows.Scan(&status, &state, &observed, &payload); err != nil {
			rows.Close()
			return nil, "", err
		}
		if status != api.AgentDone && state != "idle" && state != "finished_silent" && state != "provider_blocked" {
			idle = false
		}
		// A member's verification run waiting for or holding the matrix
		// host is the team's open work, whatever the member's own state. A
		// retired member's saved wait is not: no relay keeps it current. The
		// bound runs from the earlier of since and the report's observedAt,
		// so a since saved in the future cannot outlive it.
		if status != api.AgentRetired && strings.Contains(payload, `"matrixWait"`) {
			var activity api.AgentActivity
			if json.Unmarshal([]byte(payload), &activity) == nil && activity.MatrixWait.Valid() {
				start := activity.MatrixWait.Since
				if at := parseStallTime(observed); !at.IsZero() && at.Before(start) {
					start = at
				}
				if s.now().Sub(start) < defaultQueueMatrixWaitBound {
					idle = false
				}
			}
		}
		if at := parseStallTime(observed); at.After(since) {
			since = at
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || !idle {
		return nil, "", err
	}
	team := `SELECT b.agent_id FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id WHERE b.item_task_id=? AND b.item_id=? AND a.role<>?`
	var open int
	var changed sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN o.state<>? AND o.needs<>? THEN 1 ELSE 0 END),0),MAX(o.changed_at) FROM obligations o JOIN messages m ON m.seq=o.message_seq WHERE o.task_id=? AND (o.agent_id IN (`+team+`) OR m.from_agent IN (`+team+`))`,
		api.ObligationClosed, api.ObligationNeedsDelivery, e.TaskID, e.TaskID, e.ItemID, api.AgentRoleDatabaseHandler, e.TaskID, e.ItemID, api.AgentRoleDatabaseHandler).Scan(&open, &changed); err != nil {
		return nil, "", err
	}
	if open > 0 {
		return nil, "", nil // someone owes this team, or it owes someone
	}
	if at := parseStallTime(changed.String); at.After(since) {
		since = at
	}
	// An owner decision (tt ask) is no obligation, so it is counted here: an
	// unanswered one from a member is open work, and an answer restarts the
	// idle time.
	var unanswered int
	var answered sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN d.message_seq IS NULL THEN 1 ELSE 0 END),0),MAX(d.created_at) FROM decision_requests r JOIN messages m ON m.seq=r.message_seq
 LEFT JOIN decision_answers d ON d.task_id=r.task_id AND d.request_seq=r.message_seq WHERE r.task_id=? AND m.from_agent IN (`+team+`)`,
		e.TaskID, e.TaskID, e.ItemID, api.AgentRoleDatabaseHandler).Scan(&unanswered, &answered); err != nil {
		return nil, "", err
	}
	if unanswered > 0 {
		return nil, "", nil // the owner owes this team an answer
	}
	if at := parseStallTime(answered.String); at.After(since) {
		since = at
	}
	if s.now().Sub(since) < s.queueIdleAfter() {
		return nil, "", nil
	}
	return &stallBlocker{cause: api.StallIdleEntry, since: since, fix: fmt.Sprintf("if its candidate shipped, %s; otherwise narrow it to what it changed: tt team queue scope --task %s --entry %s --owns PATH", shipped, e.TaskID, e.ID)}, "", nil
}

// capSeatFreedAfter is the time of the project's latest agent close or exit
// after after that freed a seat while the project was at its agent cap, so a
// launch's next member could not register just before it; zero when none did.
// The open count just before each close is rebuilt from the agents' creation
// times and the closed and exited events after after, which are never
// rewritten, so an unrelated close below the cap, a repeated close or a later
// exit does not date a launch. Times are compared parsed, because ts trims
// trailing zeros.
func (s *Store) capSeatFreedAfter(ctx context.Context, q queryRower, task string, after time.Time) (time.Time, error) {
	type exit struct {
		agent string
		at    time.Time
	}
	rows, err := q.QueryContext(ctx, `SELECT agent_id,created_at FROM events WHERE task_id=? AND kind IN (?,?) ORDER BY seq DESC`, task, api.EventClosed, api.EventExited)
	if err != nil {
		return time.Time{}, err
	}
	var exits []exit // newest first
	for rows.Next() {
		var x exit
		var created string
		if err := rows.Scan(&x.agent, &created); err != nil {
			rows.Close()
			return time.Time{}, err
		}
		if x.at = parseStallTime(created); !x.at.After(after) {
			break
		}
		exits = append(exits, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(exits) == 0 {
		return time.Time{}, err
	}
	closes := map[string][]time.Time{}
	for _, x := range exits {
		closes[x.agent] = append(closes[x.agent], x.at)
	}
	type agent struct {
		id      string
		created time.Time
		open    bool
	}
	var agents []agent
	rows, err = q.QueryContext(ctx, `SELECT id,created_at,status FROM agents WHERE task_id=?`, task)
	if err != nil {
		return time.Time{}, err
	}
	for rows.Next() {
		var a agent
		var created, status string
		if err := rows.Scan(&a.id, &created, &status); err != nil {
			rows.Close()
			return time.Time{}, err
		}
		a.created, a.open = parseStallTime(created), status != api.AgentClosed && status != api.AgentExited
		agents = append(agents, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return time.Time{}, err
	}
	for _, x := range exits {
		// Open just before x: created before it, not closed or exited since
		// after but before it, and still open or closed at or after it.
		open := 0
		for _, a := range agents {
			if !a.created.Before(x.at) {
				continue
			}
			earlier, later := false, false
			for _, at := range closes[a.id] {
				if at.Before(x.at) {
					earlier = true
				} else {
					later = true
				}
			}
			if !earlier && (a.open || later) {
				open++
			}
		}
		if projectAgentCapReason(open, 0, 1, s.MaxAgents) != "" {
			return x.at, nil
		}
	}
	return time.Time{}, nil
}

var stallCauseText = map[string]string{
	api.StallFailedEntry:    "a failed entry still holds its slot, handler lease and ownership",
	api.StallNothingRunning: "an entry holds its slot, handler lease and ownership with nothing running for it",
	api.StallIdleEntry:      "an entry's whole team has been idle with no open work",
	api.StallNoHandler:      "the project has no available database handler",
	api.StallSerialHalted:   "a failed entry halts this serial queue",
}

// explainQueueStalls sets Stall and its BlockReason on queued entries that
// wait only on stall blockers or a missing handler, and the cap reason on a
// launch the project agent cap holds. q must see the same snapshot the list
// was built from. It returns the stalled queued entries that moving to their
// own checkout would free: parallel, with a free slot and a free handler (of
// the drawn arm under an arm policy).
func (s *Store) explainQueueStalls(ctx context.Context, q queryRower, capacity *sql.Tx, out *api.TeamQueueList) (map[string]bool, error) {
	parallel := queueParallel(out.ConcurrencyLimit)
	if len(out.Entries) == 0 {
		return nil, nil
	}
	task := out.Entries[0].TaskID
	// An owner-paused project launches nothing, so nothing in it is stalled.
	var pauseState string
	if err := q.QueryRowContext(ctx, `SELECT pause_state FROM tasks WHERE id=?`, task).Scan(&pauseState); err != nil {
		return nil, err
	}
	if pauseState != api.ProjectPauseActive {
		return nil, nil
	}
	blockers := map[string]*stallBlocker{}
	capHeld := map[string]string{}
	var active []api.TeamQueueEntry
	for _, e := range out.Entries {
		if !queueEntryHoldsResources(e) {
			continue
		}
		active = append(active, e)
		b, held, err := s.classifyStallBlockerHeld(ctx, q, e, parallel)
		if err != nil {
			return nil, err
		}
		if b != nil {
			blockers[e.ID] = b
		}
		if held != "" {
			capHeld[e.ID] = held
		}
	}
	// A launch with nothing running past the grace is a stall of its own,
	// shown even when no queued entry waits behind it. One the cap holds
	// shows the cap instead.
	for i := range out.Entries {
		e := &out.Entries[i]
		if e.State != "launching" {
			continue
		}
		if b := blockers[e.ID]; b != nil {
			self := *e
			setQueueStall(e, &self, b.cause, b.fix, b.since)
		} else if held := capHeld[e.ID]; held != "" {
			e.BlockReason = "Launch held at the " + held
		}
	}
	var working []api.TeamQueueEntry
	for _, e := range active {
		if blockers[e.ID] == nil {
			working = append(working, e)
		}
	}
	anyHandler, err := freeQueueHandler(ctx, q, task, nil)
	if err != nil {
		return nil, err
	}
	freeAll, err := freeQueueHandler(ctx, q, task, active)
	if err != nil {
		return nil, err
	}
	freeWorking, err := freeQueueHandler(ctx, q, task, working)
	if err != nil {
		return nil, err
	}
	// Under an enabled arm policy an entry needs a free handler of its drawn
	// arm, as the claim's lease does; a free handler of another arm frees it
	// only when the policy falls back.
	policy, err := loadHandlerArmPolicy(ctx, q, task)
	if err != nil {
		return nil, err
	}
	armFree := func(e api.TeamQueueEntry, holders []api.TeamQueueEntry) (free bool, drawn string, limited bool, err error) {
		d, err := decideHandlerArm(ctx, q, policy, e.ID, holders, s.now())
		if err != nil {
			return false, "", false, err
		}
		return d.wait == "", d.drawn, d.wait == armAllLimitedMessage, nil
	}
	movable := map[string]bool{}
	for i := range out.Entries {
		e := &out.Entries[i]
		if e.State != "queued" {
			continue
		}
		var blocker *api.TeamQueueEntry
		cause, fix, armWait := "", "", ""
		var since time.Time
		// The arm wait's reason before any stall: a need's tail follows the
		// arm in the stall reason when a person acts on it.
		waitReason, runnerAdds := e.BlockReason, false
		if !parallel {
			for j := range active {
				if active[j].State == "failed" {
					blocker = &active[j]
					break
				}
			}
			if blocker == nil {
				continue
			}
			b := blockers[blocker.ID]
			cause, since = api.StallSerialHalted, parseStallTime(blocker.UpdatedAt)
			fix = fmt.Sprintf("if its candidate shipped, tt team queue integrated --task %s --entry %s --commit SHA; otherwise close its team and tt team queue release --task %s --entry %s", e.TaskID, blocker.ID, e.TaskID, blocker.ID)
			if b != nil {
				fix = b.fix
			}
		} else {
			if capacity == nil || checkTeamHostAdmission(ctx, capacity, e.Host, s.now()) != nil {
				continue // host capacity or disk is ordinary waiting
			}
			conflicts := func(other api.TeamQueueEntry) bool {
				return queueEntryConflicts(*e, other) || (e.Cwd != "" && e.Cwd == other.Cwd)
			}
			workingOverlap := false
			for _, w := range working {
				if conflicts(w) {
					workingOverlap = true
				}
			}
			if workingOverlap || queueSlotsFull(out.ConcurrencyLimit, len(working)) {
				continue
			}
			var overlapping []api.TeamQueueEntry
			for _, a := range active {
				if blockers[a.ID] != nil && conflicts(a) {
					overlapping = append(overlapping, a)
				}
			}
			freeAllE, freeWorkingE := freeAll.ID != "", freeWorking.ID != ""
			// freeWithout says whether releasing one blocker's lease frees
			// a handler for e.
			freeWithout := func(a api.TeamQueueEntry) (bool, error) { return a.HandlerID != "", nil }
			// A handler need where the runner is adding a handler (offered or
			// already reserved) owns the wait: it clears by itself. Any other
			// arm wait, plain or with a need a person acts on, is judged by its
			// draw here. need() in handler_ab.go writes the reserved text.
			runnerAdds = e.HandlerNeed != nil && (e.HandlerNeed.Provision || strings.HasSuffix(e.HandlerNeed.Reason, "; the runner is adding one"))
			if policy.Enabled && !runnerAdds {
				free, drawn, limited, err := armFree(*e, active)
				if err != nil {
					return nil, err
				}
				if limited {
					continue // every arm at a provider limit clears by itself
				}
				freeAllE = free
				if !free {
					armWait = drawn
				}
				if freeWorkingE, _, _, err = armFree(*e, working); err != nil {
					return nil, err
				}
				freeWithout = func(a api.TeamQueueEntry) (bool, error) {
					if a.HandlerID == "" {
						return false, nil
					}
					var rest []api.TeamQueueEntry
					for _, other := range active {
						if other.ID != a.ID {
							rest = append(rest, other)
						}
					}
					free, _, _, err := armFree(*e, rest)
					return free, err
				}
			}
			slotsFull := queueSlotsFull(out.ConcurrencyLimit, len(active))
			blocked := len(overlapping) > 0 || slotsFull || !freeAllE
			if !blocked {
				continue
			}
			if !freeWorkingE {
				if anyHandler.ID != "" {
					continue // every handler is leased by working teams
				}
				cause = api.StallNoHandler
				fix = fmt.Sprintf("set up a database handler (Projects → Set up database handler) or resume a retired one with tt resume NAME, in project %s", e.TaskID)
				since, err = s.lastHandlerChange(ctx, q, e.TaskID)
				if err != nil {
					return nil, err
				}
				if since.IsZero() {
					since = parseStallTime(e.UpdatedAt)
				}
			} else {
				candidates := overlapping
				if len(candidates) == 0 {
					// Full slots: any stall blocker holds one. No free
					// handler: only a blocker that holds a lease frees one.
					for _, a := range active {
						if blockers[a.ID] == nil {
							continue
						}
						frees := freeAllE
						if !frees {
							if frees, err = freeWithout(a); err != nil {
								return nil, err
							}
						}
						if frees {
							candidates = append(candidates, a)
						}
					}
				}
				if len(candidates) == 0 {
					continue
				}
				blocker = &candidates[0]
				b := blockers[blocker.ID]
				cause, since, fix = b.cause, b.since, b.fix
			}
			movable[e.ID] = !slotsFull && freeAllE
		}
		setQueueStall(e, blocker, cause, fix, since)
		if armWait != "" && blocker != nil {
			e.BlockReason += ". It waits for a free handler of arm " + armWait
			if e.HandlerNeed != nil && !runnerAdds && waitReason != armWaitReason(armWait) {
				e.BlockReason += ": " + e.HandlerNeed.Reason
			}
		}
	}
	return movable, nil
}

// setQueueStall names the entry's stall and its blocker (nil: the project).
func setQueueStall(e, blocker *api.TeamQueueEntry, cause, fix string, since time.Time) {
	stall := &api.TeamQueueStall{Cause: cause, Fix: fix, Since: since.UTC().Format(time.RFC3339Nano)}
	name := "the project"
	if blocker != nil {
		stall.BlockerEntryID, stall.BlockerRevision = blocker.ID, blocker.Revision
		name = blocker.ID + " (" + blocker.ItemID + ")"
	}
	e.Stall = stall
	e.BlockReason = fmt.Sprintf("Stalled: %s: %s. Fix: %s", name, stallCauseText[cause], fix)
}

// lastHandlerChange is the latest event time among the project's handlers,
// the durable start of a no-handler stall.
func (s *Store) lastHandlerChange(ctx context.Context, q queryRower, task string) (time.Time, error) {
	var last sql.NullString
	err := q.QueryRowContext(ctx, `SELECT MAX(last_event_at) FROM agents WHERE task_id=? AND role=?`, task, api.AgentRoleDatabaseHandler).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, err
	}
	return parseStallTime(last.String), nil
}

var stallNoticeSubjects = map[string]string{
	api.StallFailedEntry:    "Queued work waits behind a failed team queue entry",
	api.StallNothingRunning: "Queued work waits behind an entry with nothing running",
	api.StallIdleEntry:      "Queued work waits behind a team that has gone idle",
	api.StallNoHandler:      "Queued work waits because no database handler is available",
	api.StallSerialHalted:   "A failed entry has halted the serial team queue",
}

// stallNotice recomputes the queued entry's stall from a fresh list and,
// when it is the stall the request names and has held past the grace
// period, posts one Board NOTICE. It must run before the caller opens its
// write transaction, because the list reads through the single connection.
func (s *Store) stallNotice(ctx context.Context, task string, req api.TeamQueueRequest) (api.TeamQueueEntry, api.Envelope, error) {
	var zero api.TeamQueueEntry
	if !validTeamQueueID(req.EntryID) {
		return zero, api.Envelope{}, api.ErrInvalid
	}
	list, err := s.ListTeamQueue(ctx, task)
	if err != nil {
		return zero, api.Envelope{}, err
	}
	var e *api.TeamQueueEntry
	for i := range list.Entries {
		if list.Entries[i].ID == req.EntryID {
			e = &list.Entries[i]
		}
	}
	if e == nil {
		return zero, api.Envelope{}, api.ErrNotFound
	}
	if e.Stall == nil || e.Stall.NoticeRequestID(e.ID) != req.RequestID {
		return zero, api.Envelope{}, fmt.Errorf("%w: the stall is stale or has cleared", api.ErrConflict)
	}
	since, err := time.Parse(time.RFC3339Nano, e.Stall.Since)
	if err != nil {
		return zero, api.Envelope{}, err
	}
	if s.now().Sub(since) < s.queueNoticeGrace() {
		return zero, api.Envelope{}, fmt.Errorf("%w: the stall has not yet held for the notice grace period", api.ErrConflict)
	}
	refs := map[string]string{"entry": e.ID, "item": e.ItemID, "cause": e.Stall.Cause}
	if e.Stall.BlockerEntryID != "" {
		refs["blocker"] = e.Stall.BlockerEntryID
	}
	subject := stallNoticeSubjects[e.Stall.Cause]
	if e.Stall.BlockerEntryID == e.ID {
		subject = "A team queue launch has made no progress"
	}
	text := e.BlockReason
	if detail := stallNoticeDetail(req.Failure); detail != "" {
		// The runner's last launch error for the blocker. The notice's
		// identity ignores it, so a retry with other text replays the first.
		text += ". Last launch error on the runner: " + detail + "."
	}
	notice := api.Envelope{Kind: api.EnvelopeKindNotice, Subject: subject, Refs: refs, Body: api.EnvelopeBody{Text: text}}
	return *e, notice, nil
}

// stallNoticeMaxDetail caps the runner's error text in a stall notice.
const stallNoticeMaxDetail = 500

// stallNoticeDetail is the runner's error text on one line, without control
// characters (envelopes refuse them), capped at stallNoticeMaxDetail runes.
func stallNoticeDetail(text string) string {
	text = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)), " ")
	if runes := []rune(text); len(runes) > stallNoticeMaxDetail {
		text = string(runes[:stallNoticeMaxDetail])
	}
	return text
}
