package store

// The owner helper's wakes on the hub (docs/session-handoff.md, "Wakes on the
// hub"; bug wi_8d18e0fe8bdf2ad6). The hub stores the helper's recurring wakes
// (the hourly update, the morning and evening summaries, the team watch and
// the rest of api.OwnerHelperWakeKinds), and the broker's OwnerHelperWakeSweep
// delivers each due slot as a directed NOTICE to the project's current owner
// helper. Every slot is one firing row keyed by (wake, due time), so a slot
// fires at most once across ticks and restarts. The sweep records when the
// slot was due, posted and read (the notice's delivery-only obligation), and
// reports a missed, late, unread or helper-absent slot to the owner.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// migrateOwnerHelperWakes is additive only: new tables and indexes, no ALTER
// and no backfill, so it runs again on any store without change. The request
// table holds every write's request ID, so a replay is recognised after a
// later write to the same wake.
func migrateOwnerHelperWakes(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS owner_helper_wakes (
 id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id),
 kind TEXT NOT NULL, schedule TEXT NOT NULL, time_zone TEXT NOT NULL, message_seq INTEGER NOT NULL,
 state TEXT NOT NULL, expires_at TEXT NOT NULL DEFAULT '', next_due_at TEXT NOT NULL, revision INTEGER NOT NULL,
 request_id TEXT NOT NULL, payload_hash TEXT NOT NULL, agent_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '',
 created_node TEXT NOT NULL, created_user TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 UNIQUE(task_id,request_id));
CREATE INDEX IF NOT EXISTS owner_helper_wakes_due ON owner_helper_wakes(state,next_due_at);
CREATE TABLE IF NOT EXISTS owner_helper_wake_firings (
 wake_id TEXT NOT NULL REFERENCES owner_helper_wakes(id), task_id TEXT NOT NULL, due_at TEXT NOT NULL,
 outcome TEXT NOT NULL, posted_at TEXT NOT NULL DEFAULT '', notice_seq INTEGER NOT NULL DEFAULT 0,
 recipient_id TEXT NOT NULL DEFAULT '', recipient_run_id TEXT NOT NULL DEFAULT '', delivered_at TEXT NOT NULL DEFAULT '',
 report TEXT NOT NULL DEFAULT '', report_seq INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(wake_id,due_at));
CREATE INDEX IF NOT EXISTS owner_helper_wake_firings_open ON owner_helper_wake_firings(outcome,delivered_at);
CREATE TABLE IF NOT EXISTS owner_helper_wake_requests (
 task_id TEXT NOT NULL REFERENCES tasks(id), request_id TEXT NOT NULL, payload_hash TEXT NOT NULL,
 wake_id TEXT NOT NULL, revision INTEGER NOT NULL, created INTEGER NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY(task_id,request_id))`)
	return err
}

// ownerWakeLayout is the fixed-width UTC form of due times, so they compare
// as text. Slots are whole minutes.
const ownerWakeLayout = "2006-01-02T15:04:05Z"

func ownerWakeTS(t time.Time) string { return t.UTC().Format(ownerWakeLayout) }

var ownerWakeIDRE = regexp.MustCompile(`^ohw_[0-9a-f]{16}$`)

// ---- schedules ----

// ownerWakeSchedule is a parsed five-field cron schedule.
type ownerWakeSchedule struct {
	minutes, hours, days, months, weekdays [61]bool
	anyDay, anyWeekday                     bool
}

var errOwnerWakeSchedule = fmt.Errorf("%w: the schedule is not a five-field cron schedule of numbers, ranges, lists and steps within range, such as '14 7-20 * * *'", api.ErrInvalid)

// parseOwnerWakeSchedule accepts numbers, ranges, lists and steps inside each
// field's range, with Sunday as 0 or 7, and returns the schedule with single
// spaces. Names such as MON are refused.
func parseOwnerWakeSchedule(value string) (string, ownerWakeSchedule, error) {
	var s ownerWakeSchedule
	fields := strings.Fields(value)
	if len(value) > 64 || len(fields) != 5 {
		return "", s, errOwnerWakeSchedule
	}
	sets := []*[61]bool{&s.minutes, &s.hours, &s.days, &s.months, &s.weekdays}
	for i, r := range [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}} {
		for _, part := range strings.Split(fields[i], ",") {
			base, stepText, stepped := strings.Cut(part, "/")
			step := 1
			if stepped {
				n, ok := ownerWakeNumber(stepText)
				if !ok || n < 1 || n > r[1] {
					return "", s, errOwnerWakeSchedule
				}
				step = n
			}
			from, to := r[0], r[1]
			if base != "*" {
				low, high, ranged := strings.Cut(base, "-")
				a, ok := ownerWakeNumber(low)
				if !ok || a < r[0] || a > r[1] {
					return "", s, errOwnerWakeSchedule
				}
				from, to = a, a
				if ranged {
					b, ok := ownerWakeNumber(high)
					if !ok || b < a || b > r[1] {
						return "", s, errOwnerWakeSchedule
					}
					to = b
				} else if stepped {
					to = r[1]
				}
			}
			for v := from; v <= to; v += step {
				sets[i][v] = true
			}
		}
	}
	if s.weekdays[7] {
		s.weekdays[0] = true
	}
	s.anyDay, s.anyWeekday = fields[2] == "*", fields[4] == "*"
	return strings.Join(fields, " "), s, nil
}

func ownerWakeNumber(text string) (int, bool) {
	if len(text) == 0 || len(text) > 2 {
		return 0, false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(text)
	return n, err == nil
}

// matches reports whether the wall-clock time w fires. With both day fields
// restricted, cron fires on either one.
func (s *ownerWakeSchedule) matches(w time.Time) bool {
	if !s.minutes[w.Minute()] || !s.hours[w.Hour()] || !s.months[int(w.Month())] {
		return false
	}
	day, weekday := s.days[w.Day()], s.weekdays[int(w.Weekday())]
	if !s.anyDay && !s.anyWeekday {
		return day || weekday
	}
	return day && weekday
}

// ownerWakeRepeated reports whether t's wall-clock minute in loc already
// occurred earlier (the second pass of a fall-back hour). Zone shifts are
// multiples of 15 minutes and at most a few hours.
func ownerWakeRepeated(t time.Time, loc *time.Location) bool {
	w := t.In(loc)
	_, off := w.Zone()
	for k := 1; k <= 16; k++ {
		e := t.Add(-time.Duration(k) * 15 * time.Minute).In(loc)
		if _, eoff := e.Zone(); eoff != off && e.Year() == w.Year() && e.YearDay() == w.YearDay() && e.Hour() == w.Hour() && e.Minute() == w.Minute() {
			return true
		}
	}
	return false
}

// ownerWakeHorizon is how far ahead ownerWakeNext searches.
const ownerWakeHorizon = 366 * 24 * time.Hour

// ownerWakeNext returns the first minute strictly after after whose wall
// clock in loc matches, searching at most ownerWakeHorizon. A wall-clock
// minute that occurs twice fires once, at its first occurrence; a minute that
// spring-forward skips does not fire.
func ownerWakeNext(s *ownerWakeSchedule, loc *time.Location, after time.Time) (time.Time, bool) {
	t := after.UTC().Truncate(time.Minute)
	end := t.Add(ownerWakeHorizon)
	for t = t.Add(time.Minute); !t.After(end); t = t.Add(time.Minute) {
		if s.matches(t.In(loc)) && !ownerWakeRepeated(t, loc) {
			return t, true
		}
	}
	return time.Time{}, false
}

// ownerWakeZone loads a wake's zone: an IANA name, not "" or "Local".
func ownerWakeZone(name string) (*time.Location, error) {
	if name == "" || name == "Local" || len(name) > 64 {
		return nil, fmt.Errorf("%w: the zone must be an IANA time zone name such as America/Los_Angeles", api.ErrInvalid)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: the zone must be an IANA time zone name such as America/Los_Angeles", api.ErrInvalid)
	}
	return loc, nil
}

// ---- rows ----

const ownerWakeCols = `id,task_id,kind,schedule,time_zone,message_seq,state,expires_at,next_due_at,revision,request_id,agent_id,run_id,created_node,created_user,created_at,updated_at`

func scanOwnerWake(row interface{ Scan(...any) error }) (api.OwnerHelperWake, error) {
	var w api.OwnerHelperWake
	var expires, next, created, updated string
	err := row.Scan(&w.ID, &w.TaskID, &w.Kind, &w.Schedule, &w.TimeZone, &w.MessageSeq, &w.State, &expires, &next, &w.Revision, &w.RequestID,
		&w.AgentID, &w.RunID, &w.CreatedBy.Node, &w.CreatedBy.User, &created, &updated)
	w.ExpiresAt, w.NextDueAt = ownerWakeOptionalTS(expires), ownerWakeOptionalTS(next)
	w.CreatedAt, w.UpdatedAt = parseTS(created), parseTS(updated)
	return w, err
}

func ownerWakeOptionalTS(raw string) *time.Time {
	if raw == "" {
		return nil
	}
	t := parseTS(raw)
	return &t
}

const ownerWakeFiringCols = `wake_id,due_at,outcome,posted_at,notice_seq,recipient_id,recipient_run_id,delivered_at,report,report_seq`

func scanOwnerWakeFiring(row interface{ Scan(...any) error }) (api.OwnerHelperWakeFiring, error) {
	var f api.OwnerHelperWakeFiring
	var due, posted, delivered string
	err := row.Scan(&f.WakeID, &due, &f.Outcome, &posted, &f.NoticeSeq, &f.RecipientID, &f.RecipientRunID, &delivered, &f.Report, &f.ReportSeq)
	f.DueAt, f.PostedAt, f.DeliveredAt = parseTS(due), ownerWakeOptionalTS(posted), ownerWakeOptionalTS(delivered)
	switch {
	case f.DeliveredAt != nil:
		f.LateMinutes = int64(f.DeliveredAt.Sub(f.DueAt) / time.Minute)
	case f.PostedAt != nil:
		f.LateMinutes = int64(f.PostedAt.Sub(f.DueAt) / time.Minute)
	}
	if f.LateMinutes < 0 {
		f.LateMinutes = 0
	}
	return f, err
}

// ListOwnerHelperWakes returns every wake of the project in the order they
// were created, each with its last firings (newest first): firings 0 means
// the default. Any identified caller may read it.
func (s *Store) ListOwnerHelperWakes(ctx context.Context, task string, firings int) (api.OwnerHelperWakeList, error) {
	out := api.OwnerHelperWakeList{TaskID: task, Wakes: []api.OwnerHelperWake{}}
	if firings == 0 {
		firings = api.DefaultOwnerHelperWakeFirings
	}
	if !api.ValidID(task, "tsk") || firings < 0 || firings > api.MaxOwnerHelperWakeFirings {
		return out, api.ErrInvalid
	}
	if _, err := s.GetTask(ctx, task); err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+ownerWakeCols+` FROM owner_helper_wakes WHERE task_id=? ORDER BY created_at,rowid`, task)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		w, err := scanOwnerWake(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Wakes = append(out.Wakes, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for i := range out.Wakes {
		list, err := ownerWakeFirings(ctx, s.db, out.Wakes[i].ID, firings)
		if err != nil {
			return out, err
		}
		out.Wakes[i].Firings = list
	}
	return out, nil
}

func ownerWakeFirings(ctx context.Context, q queryRower, wake string, limit int) ([]api.OwnerHelperWakeFiring, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+ownerWakeFiringCols+` FROM owner_helper_wake_firings WHERE wake_id=? ORDER BY due_at DESC LIMIT ?`, wake, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.OwnerHelperWakeFiring{}
	for rows.Next() {
		f, err := scanOwnerWakeFiring(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---- writes ----

// SetOwnerHelperWake creates a wake (no WakeID), replaces an active wake's
// fields at ExpectedRevision, or cancels one (State cancelled, no other
// field). The owner (no agent and no run) or the current owner helper's exact
// run may write; a paused project may be written, a closed one may not. A
// replayed request ID with the same input returns the wake as saved now, with
// other input a conflict. created reports whether this call made a new wake.
func (s *Store) SetOwnerHelperWake(ctx context.Context, task string, req api.SetOwnerHelperWakeRequest, by api.Caller) (wake api.OwnerHelperWake, created bool, err error) {
	var zero api.OwnerHelperWake
	if !api.ValidID(task, "tsk") || !validRequestID(req.RequestID) || req.ExpectedRevision < 0 || req.MessageSeq < 0 {
		return zero, false, api.ErrInvalid
	}
	if req.WakeID != "" && !ownerWakeIDRE.MatchString(req.WakeID) {
		return zero, false, fmt.Errorf("%w: the wake ID must look like ohw_ and 16 hex digits", api.ErrInvalid)
	}
	cancel := req.State == api.OwnerHelperWakeCancelled
	var schedule ownerWakeSchedule
	var loc *time.Location
	switch {
	case req.State != "" && req.State != api.OwnerHelperWakeActive && !cancel:
		return zero, false, fmt.Errorf("%w: the state must be active or cancelled", api.ErrInvalid)
	case (req.WakeID == "") != (req.ExpectedRevision == 0):
		return zero, false, fmt.Errorf("%w: a new wake takes no revision; a change or cancel needs the wake's current revision", api.ErrInvalid)
	case cancel:
		if req.WakeID == "" || req.Kind != "" || req.Schedule != "" || req.TimeZone != "" || req.MessageSeq != 0 || req.ExpiresAt != nil {
			return zero, false, fmt.Errorf("%w: a cancel names the wake and its revision and nothing else", api.ErrInvalid)
		}
	default:
		if !api.ValidOwnerHelperWakeKind(req.Kind) {
			return zero, false, fmt.Errorf("%w: the kind must be one of %s", api.ErrInvalid, strings.Join(api.OwnerHelperWakeKinds, ", "))
		}
		if req.Schedule, schedule, err = parseOwnerWakeSchedule(req.Schedule); err != nil {
			return zero, false, err
		}
		if req.TimeZone == "" {
			req.TimeZone = api.DefaultOwnerHelperWakeZone
		}
		if loc, err = ownerWakeZone(req.TimeZone); err != nil {
			return zero, false, err
		}
		if req.MessageSeq < 1 {
			return zero, false, fmt.Errorf("%w: the wake needs the Board message that says what to do", api.ErrInvalid)
		}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, false, err
	}
	defer tx.Rollback()
	hash := requestHash(req)
	var priorHash, priorWake string
	var priorCreated bool
	err = tx.QueryRowContext(ctx, `SELECT payload_hash,wake_id,created FROM owner_helper_wake_requests WHERE task_id=? AND request_id=?`, task, req.RequestID).Scan(&priorHash, &priorWake, &priorCreated)
	if err == nil {
		if priorHash != hash {
			return zero, false, workItemConflict("owner helper wake request ID was already used with different input")
		}
		w, err := scanOwnerWake(tx.QueryRowContext(ctx, `SELECT `+ownerWakeCols+` FROM owner_helper_wakes WHERE id=?`, priorWake))
		return w, priorCreated, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, false, err
	}
	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, task))
	if errors.Is(err, sql.ErrNoRows) {
		return zero, false, api.ErrNotFound
	}
	if err != nil {
		return zero, false, err
	}
	if t.Status != api.TaskOpen {
		return zero, false, api.ErrClosed
	}
	if err = requireOwnerWakeWriter(ctx, tx, task, req.AgentID, req.RunID); err != nil {
		return zero, false, err
	}
	now := s.now()
	if !cancel {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE task_id=? AND seq=?`, task, req.MessageSeq).Scan(&n); err != nil {
			return zero, false, err
		}
		if n == 0 {
			return zero, false, fmt.Errorf("%w: Board message #%d is not in this project", api.ErrInvalid, req.MessageSeq)
		}
		if req.ExpiresAt != nil && !req.ExpiresAt.After(now) {
			return zero, false, fmt.Errorf("%w: the expiry must be in the future", api.ErrInvalid)
		}
	}
	var w api.OwnerHelperWake
	if req.WakeID == "" {
		var active int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM owner_helper_wakes WHERE task_id=? AND state=?`, task, api.OwnerHelperWakeActive).Scan(&active); err != nil {
			return zero, false, err
		}
		if active >= api.MaxOwnerHelperWakes {
			return zero, false, workItemConflict(fmt.Sprintf("the project already has %d active wakes; cancel one first", api.MaxOwnerHelperWakes))
		}
		w = api.OwnerHelperWake{ID: api.NewID("ohw"), TaskID: task, CreatedBy: by, CreatedAt: now}
		created = true
	} else {
		w, err = scanOwnerWake(tx.QueryRowContext(ctx, `SELECT `+ownerWakeCols+` FROM owner_helper_wakes WHERE id=? AND task_id=?`, req.WakeID, task))
		if errors.Is(err, sql.ErrNoRows) {
			return zero, false, api.ErrNotFound
		}
		if err != nil {
			return zero, false, err
		}
		if w.Revision != req.ExpectedRevision {
			return zero, false, workItemConflict(fmt.Sprintf("wake %s is at revision %d; list the wakes and write against that revision", w.ID, w.Revision))
		}
		if w.State != api.OwnerHelperWakeActive {
			return zero, false, workItemConflict(fmt.Sprintf("wake %s is %s; schedule a new wake instead", w.ID, w.State))
		}
	}
	if cancel {
		w.State, w.NextDueAt = api.OwnerHelperWakeCancelled, nil
	} else {
		reschedule := created || w.Schedule != req.Schedule || w.TimeZone != req.TimeZone
		w.Kind, w.Schedule, w.TimeZone, w.MessageSeq, w.ExpiresAt, w.State = req.Kind, req.Schedule, req.TimeZone, req.MessageSeq, req.ExpiresAt, api.OwnerHelperWakeActive
		if reschedule {
			// From the latest slot that already fired when the clock has
			// stepped back past it, so no slot is targeted twice.
			after := now
			if !created {
				var latest string
				if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(due_at),'') FROM owner_helper_wake_firings WHERE wake_id=?`, w.ID).Scan(&latest); err != nil {
					return zero, false, err
				}
				if t := parseTS(latest); latest != "" && t.After(after) {
					after = t
				}
			}
			next, ok := ownerWakeNext(&schedule, loc, after)
			if !ok {
				return zero, false, fmt.Errorf("%w: the schedule does not fire within a year", api.ErrInvalid)
			}
			w.NextDueAt = &next
		}
		if w.ExpiresAt != nil && w.NextDueAt != nil && !w.ExpiresAt.After(*w.NextDueAt) {
			return zero, false, fmt.Errorf("%w: the wake expires before its next slot at %s", api.ErrInvalid, w.NextDueAt.UTC().Format(time.RFC3339))
		}
	}
	w.Revision++
	w.RequestID, w.AgentID, w.RunID, w.UpdatedAt = req.RequestID, req.AgentID, req.RunID, now
	expires, next := "", ""
	if w.ExpiresAt != nil {
		expires = ts(*w.ExpiresAt)
	}
	if w.NextDueAt != nil {
		next = ownerWakeTS(*w.NextDueAt)
	}
	if created {
		_, err = tx.ExecContext(ctx, `INSERT INTO owner_helper_wakes(`+ownerWakeCols+`,payload_hash) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			w.ID, w.TaskID, w.Kind, w.Schedule, w.TimeZone, w.MessageSeq, w.State, expires, next, w.Revision, w.RequestID, w.AgentID, w.RunID,
			by.Node, by.User, ts(now), ts(now), hash)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE owner_helper_wakes SET kind=?,schedule=?,time_zone=?,message_seq=?,state=?,expires_at=?,next_due_at=?,revision=?,request_id=?,payload_hash=?,agent_id=?,run_id=?,updated_at=? WHERE id=?`,
			w.Kind, w.Schedule, w.TimeZone, w.MessageSeq, w.State, expires, next, w.Revision, w.RequestID, hash, w.AgentID, w.RunID, ts(now), w.ID)
	}
	if err != nil {
		return zero, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO owner_helper_wake_requests(task_id,request_id,payload_hash,wake_id,revision,created,created_at) VALUES(?,?,?,?,?,?,?)`,
		task, req.RequestID, hash, w.ID, w.Revision, created, ts(now)); err != nil {
		return zero, false, err
	}
	if _, err = s.insertEvent(ctx, tx, task, "task_updated", req.AgentID, "Owner helper wake saved", map[string]any{"ownerHelperWake": w.ID, "revision": w.Revision, "state": w.State, "kind": w.Kind}, by); err != nil {
		return zero, false, err
	}
	if err = tx.Commit(); err != nil {
		return zero, false, err
	}
	s.notify(task)
	return w, created, nil
}

// requireOwnerWakeWriter admits the owner (no agent and no run) or the exact
// current run of the project's owner helper while it is not exited, closed or
// retired.
func requireOwnerWakeWriter(ctx context.Context, tx *sql.Tx, task, agentID, runID string) error {
	if agentID == "" && runID == "" {
		return nil
	}
	const refused = "only the owner or the current owner helper's exact run may change the project's wakes"
	if agentID == "" || runID == "" {
		return fmt.Errorf("%w: %s; give both the agent and its run", api.ErrConflict, refused)
	}
	helper, found, err := currentOwnerHelper(ctx, tx, task)
	if err != nil {
		return err
	}
	switch {
	case !found || helper.ID != agentID:
		return fmt.Errorf("%w: %s; %s is not the project's owner helper", api.ErrConflict, refused, agentID)
	case helper.RunID != runID:
		return fmt.Errorf("%w: %s; run %s was replaced, so register this session again (tt helper register)", api.ErrConflict, refused, runID)
	case helper.Status == api.AgentExited || helper.Status == api.AgentClosed || helper.Status == api.AgentRetired:
		return fmt.Errorf("%w: %s; the owner helper is %s", api.ErrConflict, refused, helper.Status)
	}
	return nil
}

// ---- the sweep ----

// OwnerHelperWakeStep is one notice the sweep posted.
type OwnerHelperWakeStep struct {
	TaskID     string
	WakeID     string
	MessageSeq int64
	Action     string // owner-wake, owner-wake-missed, owner-wake-absent, owner-wake-late, owner-wake-undelivered
}

const (
	ownerWakeActionPosted      = "owner-wake"
	ownerWakeActionMissed      = "owner-wake-missed"
	ownerWakeActionAbsent      = "owner-wake-absent"
	ownerWakeActionLate        = "owner-wake-late"
	ownerWakeActionUndelivered = "owner-wake-undelivered"
)

// OwnerHelperWakeSweep delivers due slots and records reads, in two passes.
// Each write re-reads its row in its own transaction, so concurrent sweeps
// and a restarted hub post each slot and each report at most once. With no
// slot due and no unread notice to settle it makes two reads and no write.
func (s *Store) OwnerHelperWakeSweep(ctx context.Context, now time.Time) ([]OwnerHelperWakeStep, error) {
	var steps []OwnerHelperWakeStep
	var firstErr error
	note := func(what string, err error) {
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("owner helper wake sweep %s: %w", what, err)
		}
	}
	due, err := s.ownerWakesDue(ctx, now)
	if err != nil {
		return nil, fmt.Errorf("owner helper wake sweep: %w", err)
	}
	for _, id := range due {
		posted, err := s.ownerWakeFire(ctx, id, now)
		steps = append(steps, posted...)
		note(id, err)
	}
	unread, err := s.ownerWakeUnread(ctx, now)
	note("reads", err)
	for _, f := range unread {
		posted, err := s.ownerWakeSettle(ctx, f, now)
		steps = append(steps, posted...)
		note(f.wake, err)
	}
	return steps, firstErr
}

func (s *Store) ownerWakesDue(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.id FROM owner_helper_wakes w JOIN tasks t ON t.id=w.task_id
WHERE w.state=? AND w.next_due_at<>'' AND w.next_due_at<=? AND t.status=? ORDER BY w.next_due_at,w.id`, api.OwnerHelperWakeActive, ownerWakeTS(now.Truncate(time.Second)), api.TaskOpen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ownerWakeSlots lists the slots from first through now, stopping before
// expiry. It returns the number of slots, the first, and the last keep slots.
func ownerWakeSlots(s *ownerWakeSchedule, loc *time.Location, first, now time.Time, expires *time.Time, keep int) (count int, earliest time.Time, kept []time.Time, expired bool) {
	for t := first; !t.After(now); {
		if expires != nil && !expires.After(t) {
			return count, earliest, kept, true
		}
		if count == 0 {
			earliest = t
		}
		count++
		kept = append(kept, t)
		if len(kept) > keep {
			kept = kept[1:]
		}
		next, ok := ownerWakeNext(s, loc, t)
		if !ok {
			return count, earliest, kept, true
		}
		t = next
	}
	return count, earliest, kept, false
}

// ownerWakeFire handles one due wake: earlier slots of a catch-up are missed,
// the latest is posted to the helper (or recorded helper-absent), and a
// paused project records every slot skipped-paused. next_due_at moves past
// now in the same transaction.
func (s *Store) ownerWakeFire(ctx context.Context, id string, now time.Time) ([]OwnerHelperWakeStep, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	w, err := scanOwnerWake(tx.QueryRowContext(ctx, `SELECT `+ownerWakeCols+` FROM owner_helper_wakes WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if w.State != api.OwnerHelperWakeActive || w.NextDueAt == nil || w.NextDueAt.After(now) {
		return nil, nil // changed since the read
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, w.TaskID))
	if err != nil {
		return nil, err
	}
	if task.Status != api.TaskOpen {
		return nil, nil
	}
	_, schedule, err := parseOwnerWakeSchedule(w.Schedule)
	if err != nil {
		return nil, err
	}
	loc, err := ownerWakeZone(w.TimeZone)
	if err != nil {
		return nil, err
	}
	count, earliest, kept, expired := ownerWakeSlots(&schedule, loc, *w.NextDueAt, now, w.ExpiresAt, api.OwnerHelperWakeMissedRows+1)
	var steps []OwnerHelperWakeStep
	insert := func(due time.Time, outcome string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO owner_helper_wake_firings(wake_id,task_id,due_at,outcome) VALUES(?,?,?,?)`, w.ID, w.TaskID, ownerWakeTS(due), outcome)
		return err
	}
	if count > 0 {
		latest := kept[len(kept)-1]
		earlier := kept[:len(kept)-1]
		if len(earlier) > api.OwnerHelperWakeMissedRows {
			earlier = earlier[len(earlier)-api.OwnerHelperWakeMissedRows:]
		}
		paused := task.PauseState != api.ProjectPauseActive
		outcome := api.OwnerHelperWakeMissed
		if paused {
			outcome = api.OwnerHelperWakeSkippedPaused
		}
		for _, due := range earlier {
			if err := insert(due, outcome); err != nil {
				return nil, err
			}
		}
		if paused {
			if err := insert(latest, api.OwnerHelperWakeSkippedPaused); err != nil {
				return nil, err
			}
		} else {
			steps, err = s.ownerWakeDeliver(ctx, tx, task, w, latest, count-1, earliest, earlier, now)
			if err != nil {
				return nil, err
			}
		}
	}
	state, next := w.State, ""
	if !expired {
		n, ok := ownerWakeNext(&schedule, loc, now)
		if w.ExpiresAt != nil && ok && !w.ExpiresAt.After(n) || !ok {
			expired = true
		} else {
			next = ownerWakeTS(n)
		}
	}
	if expired {
		state = api.OwnerHelperWakeExpired
	}
	if _, err = tx.ExecContext(ctx, `UPDATE owner_helper_wakes SET state=?,next_due_at=? WHERE id=?`, state, next, w.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.notify(w.TaskID)
	return steps, nil
}

// ownerWakeDeliver posts the latest slot to the helper, or records it
// helper-absent, and posts the catch-up's one owner report. missed is the
// number of earlier slots; stored are the earlier slots written as rows.
func (s *Store) ownerWakeDeliver(ctx context.Context, tx *sql.Tx, task api.Task, w api.OwnerHelperWake, due time.Time, missed int, earliest time.Time, stored []time.Time, now time.Time) ([]OwnerHelperWakeStep, error) {
	var steps []OwnerHelperWakeStep
	helper, found, err := currentOwnerHelper(ctx, tx, task.ID)
	if err != nil {
		return nil, err
	}
	present := found && (helper.Status == api.AgentRunning || helper.Status == api.AgentDone || helper.Status == api.AgentNeedsInput)
	report, outcome := "", api.OwnerHelperWakeHelperAbsent
	late := now.Sub(due) > api.OwnerHelperWakeLateAfter
	var noticeSeq int64
	if present {
		outcome = api.OwnerHelperWakePosted
		m, err := s.ownerWakePost(ctx, tx, task, helper, ownerWakeSubject(w, due), ownerWakeText(w, due), ownerWakeRefs(w, due, false, false))
		if err != nil {
			return nil, err
		}
		noticeSeq = m.Seq
		steps = append(steps, OwnerHelperWakeStep{TaskID: task.ID, WakeID: w.ID, MessageSeq: m.Seq, Action: ownerWakeActionPosted})
		if late {
			report = api.OwnerHelperWakeReportLate
		}
	} else {
		report = api.OwnerHelperWakeReportAbsent
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO owner_helper_wake_firings(wake_id,task_id,due_at,outcome,posted_at,notice_seq,recipient_id,recipient_run_id) VALUES(?,?,?,?,?,?,?,?)`,
		w.ID, task.ID, ownerWakeTS(due), outcome, ownerWakePostedAt(present, now), noticeSeq, ownerWakeIf(present, helper.ID), ownerWakeIf(present, helper.RunID)); err != nil {
		return nil, err
	}
	helperState := "not registered"
	if found {
		helperState = helper.Status
	}
	var text, action string
	switch {
	case missed > 0:
		// One report per catch-up names the gap and what happened to the
		// latest slot; it is never folded.
		count := fmt.Sprintf("%d earlier slot(s)", missed)
		if missed > api.OwnerHelperWakeMissedRows {
			count = fmt.Sprintf("at least %d earlier slots (the firing list keeps the latest %d)", missed, api.OwnerHelperWakeMissedRows)
		}
		text = fmt.Sprintf("Wake %s (%s) missed %s, due from %s to %s, because the hub did not run them on time; they were not delivered. ",
			w.ID, w.Kind, count, ownerWakeWhen(earliest), ownerWakeWhen(stored[len(stored)-1]))
		switch {
		case !present:
			text += fmt.Sprintf("The latest slot, due %s, was not delivered either: the owner helper is %s.", ownerWakeWhen(due), helperState)
		case late:
			text += fmt.Sprintf("The latest slot, due %s, was posted to the owner helper %s late as message #%d.", ownerWakeWhen(due), stallDuration(now.Sub(due)), noticeSeq)
		default:
			text += fmt.Sprintf("The latest slot, due %s, was posted to the owner helper as message #%d.", ownerWakeWhen(due), noticeSeq)
		}
		seq, err := s.ownerWakeReport(ctx, tx, task, w, due, "Owner-helper wake slots were missed and not delivered", text, now)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE owner_helper_wake_firings SET report=?,report_seq=? WHERE wake_id=? AND due_at>=? AND due_at<? AND outcome=?`,
			api.OwnerHelperWakeReportMissed, seq, w.ID, ownerWakeTS(stored[0]), ownerWakeTS(due), api.OwnerHelperWakeMissed); err != nil {
			return nil, err
		}
		steps = append(steps, OwnerHelperWakeStep{TaskID: task.ID, WakeID: w.ID, MessageSeq: seq, Action: ownerWakeActionMissed})
		if report != "" {
			_, err = tx.ExecContext(ctx, `UPDATE owner_helper_wake_firings SET report=?,report_seq=? WHERE wake_id=? AND due_at=?`, report, seq, w.ID, ownerWakeTS(due))
		}
		return steps, err
	case report == api.OwnerHelperWakeReportAbsent:
		text = fmt.Sprintf("Wake %s (%s), due %s, was not delivered: the owner helper is %s. Register or resume the helper, or run the wake's Board message #%d by hand.",
			w.ID, w.Kind, ownerWakeWhen(due), helperState, w.MessageSeq)
		action = ownerWakeActionAbsent
	case report == api.OwnerHelperWakeReportLate:
		text = fmt.Sprintf("Wake %s (%s), due %s, was posted to the owner helper %s late as message #%d.", w.ID, w.Kind, ownerWakeWhen(due), stallDuration(now.Sub(due)), noticeSeq)
		action = ownerWakeActionLate
	default:
		return steps, nil
	}
	step, err := s.ownerWakeReportOnce(ctx, tx, task, w, due, report, text, now)
	if step != nil {
		step.Action = action
		steps = append(steps, *step)
	}
	return steps, err
}

func ownerWakePostedAt(present bool, now time.Time) string {
	if !present {
		return ""
	}
	return ts(now)
}

func ownerWakeIf(ok bool, v string) string {
	if !ok {
		return ""
	}
	return v
}

// ownerWakeUnreadFiring is a posted slot whose read is not recorded yet.
type ownerWakeUnreadFiring struct {
	wake, due string
}

// ownerWakeUnread nominates posted slots that need a write: read since the
// last sweep, or unread past OwnerHelperWakeUnreadAfter with no report. The
// read is unlocked; ownerWakeSettle decides again under the write lock.
func (s *Store) ownerWakeUnread(ctx context.Context, now time.Time) ([]ownerWakeUnreadFiring, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.wake_id,f.due_at FROM owner_helper_wake_firings f
JOIN obligations o ON o.message_seq=f.notice_seq AND o.agent_id=f.recipient_id
WHERE f.outcome=? AND f.delivered_at='' AND f.due_at>=? AND (o.delivered_at<>'' OR (f.report='' AND f.due_at<=?))
ORDER BY f.due_at,f.wake_id`, api.OwnerHelperWakePosted, ownerWakeTS(now.Add(-api.OwnerHelperWakeReadWindow)), ownerWakeTS(now.Add(-api.OwnerHelperWakeUnreadAfter)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ownerWakeUnreadFiring
	for rows.Next() {
		var f ownerWakeUnreadFiring
		if err := rows.Scan(&f.wake, &f.due); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ownerWakeSettle records a posted slot's read time, reporting it late when
// it was read more than OwnerHelperWakeLateAfter after due, or reports it
// undelivered once it has stayed unread OwnerHelperWakeUnreadAfter. A slot
// already reported gets no second report.
func (s *Store) ownerWakeSettle(ctx context.Context, f ownerWakeUnreadFiring, now time.Time) ([]OwnerHelperWakeStep, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	firing, err := scanOwnerWakeFiring(tx.QueryRowContext(ctx, `SELECT `+ownerWakeFiringCols+` FROM owner_helper_wake_firings WHERE wake_id=? AND due_at=?`, f.wake, f.due))
	if err != nil {
		return nil, err
	}
	if firing.Outcome != api.OwnerHelperWakePosted || firing.DeliveredAt != nil {
		return nil, nil
	}
	var read string
	if err = tx.QueryRowContext(ctx, `SELECT delivered_at FROM obligations WHERE message_seq=? AND agent_id=?`, firing.NoticeSeq, firing.RecipientID).Scan(&read); err != nil {
		return nil, err
	}
	w, err := scanOwnerWake(tx.QueryRowContext(ctx, `SELECT `+ownerWakeCols+` FROM owner_helper_wakes WHERE id=?`, f.wake))
	if err != nil {
		return nil, err
	}
	task, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id=?`, w.TaskID))
	if err != nil {
		return nil, err
	}
	var report, text, action string
	switch {
	case read != "":
		if _, err = tx.ExecContext(ctx, `UPDATE owner_helper_wake_firings SET delivered_at=? WHERE wake_id=? AND due_at=?`, read, f.wake, f.due); err != nil {
			return nil, err
		}
		if at := parseTS(read); firing.Report == "" && at.Sub(firing.DueAt) > api.OwnerHelperWakeLateAfter {
			report, action = api.OwnerHelperWakeReportLate, ownerWakeActionLate
			text = fmt.Sprintf("Wake %s (%s), due %s, was posted as message #%d but the owner helper read it %s late.", w.ID, w.Kind, ownerWakeWhen(firing.DueAt), firing.NoticeSeq, stallDuration(at.Sub(firing.DueAt)))
		}
	case firing.Report == "" && now.Sub(firing.DueAt) >= api.OwnerHelperWakeUnreadAfter:
		report, action = api.OwnerHelperWakeReportUndelivered, ownerWakeActionUndelivered
		text = fmt.Sprintf("Wake %s (%s), due %s, was posted to the owner helper as message #%d and is still unread after %s, past its re-wakes. Check the helper's session; the hub records the read when it happens.",
			w.ID, w.Kind, ownerWakeWhen(firing.DueAt), firing.NoticeSeq, stallDuration(now.Sub(firing.DueAt)))
	}
	var steps []OwnerHelperWakeStep
	if report != "" {
		step, err := s.ownerWakeReportOnce(ctx, tx, task, w, firing.DueAt, report, text, now)
		if err != nil {
			return nil, err
		}
		if step != nil {
			step.Action = action
			steps = append(steps, *step)
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.notify(w.TaskID)
	return steps, nil
}

// ownerWakeReportOnce records report on the firing and posts its owner
// notice, unless it folds into the report of the wake's previous slot: that
// slot carries the same report and the trouble is still going on. A
// helper-absent slot folds into a helper-absent previous slot. A late or
// undelivered slot folds only while the previous slot is still unread, so a
// dead helper session is reported once rather than every slot, while a wake
// read late on two days, or an unread slot after one that was read in the
// end, is reported each time. A folded slot is recorded with no report
// message. A slot without that report ends the episode.
func (s *Store) ownerWakeReportOnce(ctx context.Context, tx *sql.Tx, task api.Task, w api.OwnerHelperWake, due time.Time, report, text string, now time.Time) (*OwnerHelperWakeStep, error) {
	var previous, previousRead string
	err := tx.QueryRowContext(ctx, `SELECT report,delivered_at FROM owner_helper_wake_firings WHERE wake_id=? AND due_at<? ORDER BY due_at DESC LIMIT 1`, w.ID, ownerWakeTS(due)).Scan(&previous, &previousRead)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	fold := previous == report && (report == api.OwnerHelperWakeReportAbsent || previousRead == "")
	var seq int64
	if !fold {
		until := "until one of them is read"
		if report == api.OwnerHelperWakeReportAbsent {
			until = "until a slot reaches the helper"
		}
		text += fmt.Sprintf(" Later slots of this wake with the same trouble are recorded in `tt handoff wake list --task %s` without another notice %s.", task.ID, until)
		if seq, err = s.ownerWakeReport(ctx, tx, task, w, due, ownerWakeReportSubject[report], text, now); err != nil {
			return nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE owner_helper_wake_firings SET report=?,report_seq=? WHERE wake_id=? AND due_at=?`, report, seq, w.ID, ownerWakeTS(due)); err != nil {
		return nil, err
	}
	if seq == 0 {
		return nil, nil
	}
	return &OwnerHelperWakeStep{TaskID: task.ID, WakeID: w.ID, MessageSeq: seq}, nil
}

var ownerWakeReportSubject = map[string]string{
	api.OwnerHelperWakeReportAbsent:      "An owner-helper wake was not delivered: no helper is running",
	api.OwnerHelperWakeReportLate:        "An owner-helper wake reached the helper late",
	api.OwnerHelperWakeReportUndelivered: "An owner-helper wake is still unread by the helper",
}

// ownerWakeReport posts one undirected owner NOTICE: an owner ping by day and
// a plain channel line between 22:00 and 07:00 Pacific.
func (s *Store) ownerWakeReport(ctx context.Context, tx *sql.Tx, task api.Task, w api.OwnerHelperWake, due time.Time, subject, text string, now time.Time) (int64, error) {
	m, err := s.ownerWakePost(ctx, tx, task, api.Agent{}, subject, text, ownerWakeRefs(w, due, true, stallWatchNight(now)))
	return m.Seq, err
}

// ownerWakePost inserts a hub-authored NOTICE, directed to to when it has an
// ID (with its delivery-only obligation), and returns the message.
func (s *Store) ownerWakePost(ctx context.Context, tx *sql.Tx, task api.Task, to api.Agent, subject, text string, refs map[string]string) (api.Message, error) {
	env := &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: subject, Refs: refs, Body: api.EnvelopeBody{Text: text}}
	if to.ID != "" {
		env.To = to.Name
	}
	if err := api.NormalizeBrokerPost(&api.PostMessageRequest{Envelope: env}, to.Name); err != nil {
		env.Subject = "An owner-helper wake is due"
	}
	req := api.PostMessageRequest{Envelope: env, To: to.ID}
	m, err := s.insertBrokerMessage(ctx, tx, task, req, to, to.Name)
	if err != nil {
		return m, err
	}
	return m, s.createObligations(ctx, tx, m, req, false)
}

// ownerWakeKindWords is the kind as words, for the fixed subject.
func ownerWakeKindWords(kind string) string {
	if kind == "queue-check" {
		return "queue check (team watch)"
	}
	return strings.ReplaceAll(kind, "-", " ")
}

func ownerWakeSubject(w api.OwnerHelperWake, due time.Time) string {
	when := due.UTC().Format("15:04 UTC")
	if loc, err := ownerWakeZone(w.TimeZone); err == nil {
		when = due.In(loc).Format("15:04 MST")
	}
	return "Owner-helper wake: " + ownerWakeKindWords(w.Kind) + " due " + when
}

func ownerWakeText(w api.OwnerHelperWake, due time.Time) string {
	return fmt.Sprintf("Wake %s (%s) was due at %s. Do what Board message #%d says. This wake comes from the hub: delete any session cron for it.",
		w.ID, w.Kind, due.UTC().Format(time.RFC3339), w.MessageSeq)
}

func ownerWakeWhen(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ownerWakeRefs are a wake notice's refs; a report adds the owner escalation,
// which the bridge mirrors as a ping by day and a plain line at night.
func ownerWakeRefs(w api.OwnerHelperWake, due time.Time, report, night bool) map[string]string {
	refs := map[string]string{"ownerWake": w.Kind, "wake": w.ID, "due": due.UTC().Format(time.RFC3339)}
	if !report {
		refs["message"] = strconv.FormatInt(w.MessageSeq, 10)
		return refs
	}
	refs["recipientKind"], refs["escalation"] = "owner", "owner"
	if night {
		refs["escalation"] = "lead"
	}
	return refs
}
