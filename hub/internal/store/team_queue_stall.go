package store

import (
	"context"
	"database/sql"
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
	if !queueEntryHoldsResources(e) {
		return nil, nil
	}
	shipped := fmt.Sprintf("tt team queue integrated --task %s --entry %s --commit SHA", e.TaskID, e.ID)
	if e.State == "failed" {
		live, err := liveItemRuns(ctx, q, e.TaskID, e.ItemID)
		if err != nil {
			return nil, err
		}
		if parallel && live == 0 {
			return nil, nil // the runner releases it on its next pass
		}
		fix := fmt.Sprintf("if its candidate shipped, %s; otherwise close its team once the item is terminal (tt close --team --task %s --item %s) so it can be released", shipped, e.TaskID, e.ItemID)
		if !parallel && live == 0 {
			fix = fmt.Sprintf("if its candidate shipped, %s; otherwise tt team queue release --task %s --entry %s", shipped, e.TaskID, e.ID)
		}
		return &stallBlocker{cause: api.StallFailedEntry, since: parseStallTime(e.UpdatedAt), fix: fix}, nil
	}
	var live int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id WHERE b.item_task_id=? AND b.item_id=? AND a.status NOT IN ('closed','exited')`, e.TaskID, e.ItemID).Scan(&live); err != nil {
		return nil, err
	}
	if live == 0 {
		var last sql.NullString
		if err := q.QueryRowContext(ctx, `SELECT MAX(a.last_event_at) FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id WHERE b.item_task_id=? AND b.item_id=? AND a.status IN ('closed','exited')`, e.TaskID, e.ItemID).Scan(&last); err != nil {
			return nil, err
		}
		// Held since the later of the item's last run and the entry's own
		// last write: a relaunch is not dated from an earlier team's exit.
		since := parseStallTime(last.String, e.UpdatedAt)
		// A launch in progress has no runs yet; only an entry that has had
		// none for the whole grace period is stuck.
		if s.now().Sub(since) < s.queueNoticeGrace() {
			return nil, nil
		}
		return &stallBlocker{cause: api.StallNothingRunning, since: since, fix: fmt.Sprintf("if its candidate shipped, %s; otherwise tt team queue fail --task %s --entry %s --reason TEXT, and the runner releases it", shipped, e.TaskID, e.ID)}, nil
	}
	if e.State != "running" {
		return nil, nil
	}
	// idle-entry: every live member idle or done, no open work between them
	// and anyone, for at least the idle threshold.
	rows, err := q.QueryContext(ctx, `SELECT a.status,COALESCE(x.state,''),COALESCE(x.observed_at,'') FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id
 LEFT JOIN agent_activity x ON x.agent_id=b.agent_id AND x.run_id=b.run_id
 WHERE b.item_task_id=? AND b.item_id=? AND a.status NOT IN ('closed','exited')`, e.TaskID, e.ItemID)
	if err != nil {
		return nil, err
	}
	since := parseStallTime(e.UpdatedAt)
	idle := true
	for rows.Next() {
		var status, state, observed string
		if err := rows.Scan(&status, &state, &observed); err != nil {
			rows.Close()
			return nil, err
		}
		if status != api.AgentDone && state != "idle" && state != "finished_silent" {
			idle = false
		}
		if at := parseStallTime(observed); at.After(since) {
			since = at
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || !idle {
		return nil, err
	}
	team := `SELECT b.agent_id FROM agent_work_item_bindings b JOIN agents a ON a.id=b.agent_id AND a.run_id=b.run_id WHERE b.item_task_id=? AND b.item_id=? AND a.role<>?`
	var open int
	var changed sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN o.state<>? AND o.needs<>? THEN 1 ELSE 0 END),0),MAX(o.changed_at) FROM obligations o JOIN messages m ON m.seq=o.message_seq WHERE o.task_id=? AND (o.agent_id IN (`+team+`) OR m.from_agent IN (`+team+`))`,
		api.ObligationClosed, api.ObligationNeedsDelivery, e.TaskID, e.TaskID, e.ItemID, api.AgentRoleDatabaseHandler, e.TaskID, e.ItemID, api.AgentRoleDatabaseHandler).Scan(&open, &changed); err != nil {
		return nil, err
	}
	if open > 0 {
		return nil, nil // someone owes this team, or it owes someone
	}
	if at := parseStallTime(changed.String); at.After(since) {
		since = at
	}
	if s.now().Sub(since) < s.queueIdleAfter() {
		return nil, nil
	}
	return &stallBlocker{cause: api.StallIdleEntry, since: since, fix: fmt.Sprintf("if its candidate shipped, %s; otherwise narrow it to what it changed: tt team queue scope --task %s --entry %s --owns PATH", shipped, e.TaskID, e.ID)}, nil
}

var stallCauseText = map[string]string{
	api.StallFailedEntry:    "a failed entry still holds its slot, handler lease and ownership",
	api.StallNothingRunning: "an entry holds its slot, handler lease and ownership with nothing running for it",
	api.StallIdleEntry:      "an entry's whole team has been idle with no open work",
	api.StallNoHandler:      "the project has no available database handler",
	api.StallSerialHalted:   "a failed entry halts this serial queue",
}

// explainQueueStalls sets Stall and its BlockReason on queued entries that
// wait only on stall blockers or a missing handler. q must see the same
// snapshot the list was built from.
func (s *Store) explainQueueStalls(ctx context.Context, q queryRower, capacity *sql.Tx, out *api.TeamQueueList) error {
	parallel := queueParallel(out.ConcurrencyLimit)
	if len(out.Entries) == 0 {
		return nil
	}
	// An owner-paused project launches nothing, so nothing in it is stalled.
	var pauseState string
	if err := q.QueryRowContext(ctx, `SELECT pause_state FROM tasks WHERE id=?`, out.Entries[0].TaskID).Scan(&pauseState); err != nil {
		return err
	}
	if pauseState != api.ProjectPauseActive {
		return nil
	}
	blockers := map[string]*stallBlocker{}
	var active []api.TeamQueueEntry
	for _, e := range out.Entries {
		if !queueEntryHoldsResources(e) {
			continue
		}
		active = append(active, e)
		b, err := s.classifyStallBlocker(ctx, q, e, parallel)
		if err != nil {
			return err
		}
		if b != nil {
			blockers[e.ID] = b
		}
	}
	// A launch with nothing running past the grace is a stall of its own,
	// shown even when no queued entry waits behind it.
	for i := range out.Entries {
		e := &out.Entries[i]
		if b := blockers[e.ID]; e.State == "launching" && b != nil {
			self := *e
			setQueueStall(e, &self, b.cause, b.fix, b.since)
		}
	}
	var working []api.TeamQueueEntry
	for _, e := range active {
		if blockers[e.ID] == nil {
			working = append(working, e)
		}
	}
	anyHandler, err := freeQueueHandler(ctx, q, out.Entries[0].TaskID, nil)
	if err != nil {
		return err
	}
	freeAll, err := freeQueueHandler(ctx, q, out.Entries[0].TaskID, active)
	if err != nil {
		return err
	}
	freeWorking, err := freeQueueHandler(ctx, q, out.Entries[0].TaskID, working)
	if err != nil {
		return err
	}
	for i := range out.Entries {
		e := &out.Entries[i]
		if e.State != "queued" {
			continue
		}
		var blocker *api.TeamQueueEntry
		cause, fix := "", ""
		var since time.Time
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
			blocked := len(overlapping) > 0 || queueSlotsFull(out.ConcurrencyLimit, len(active)) || freeAll.ID == ""
			if !blocked {
				continue
			}
			if freeWorking.ID == "" {
				if anyHandler.ID != "" {
					continue // every handler is leased by working teams
				}
				cause = api.StallNoHandler
				fix = fmt.Sprintf("set up a database handler (Projects → Set up database handler) or resume a retired one with tt resume NAME, in project %s", e.TaskID)
				since, err = s.lastHandlerChange(ctx, q, e.TaskID)
				if err != nil {
					return err
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
						if blockers[a.ID] != nil && (freeAll.ID != "" || a.HandlerID != "") {
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
		}
		setQueueStall(e, blocker, cause, fix, since)
	}
	return nil
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
