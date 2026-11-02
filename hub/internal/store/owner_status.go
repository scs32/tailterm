package store

// The owner status call (bug wi_475a3df9b345e13e; docs/owner-report.md,
// "Owner status call"). The hub computes the owner report's figures from its
// own records: the release ledger, work item revisions and the active team
// queue with its host census. The one stored figure is the daily 21:00
// baseline, written once by the first read after its time and never changed.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// ownerStatusSchema is additive: an older hub ignores the table.
const ownerStatusSchema = `CREATE TABLE IF NOT EXISTS owner_status_baselines (
 task_id TEXT NOT NULL REFERENCES tasks(id), zone TEXT NOT NULL, day TEXT NOT NULL,
 at TEXT NOT NULL, released_total INTEGER NOT NULL, open_bugs INTEGER NOT NULL,
 open_features INTEGER NOT NULL, recorded_at TEXT NOT NULL,
 PRIMARY KEY(task_id,zone,day))`

// ownerStatusRelease is one release job's state and settled time.
type ownerStatusRelease struct {
	state   string
	settled time.Time
	ok      bool // settledAt was present and parsed
}

// ownerStatusRevision is one saved revision of a work item, without its text.
type ownerStatusRevision struct {
	revision         int64
	kind, status     string
	changeKind       string
	created, updated time.Time
}

func ownerStatusTerminal(status string) bool { return status == "done" || status == "dismissed" }

func ownerStatusCount(c *api.OwnerStatusCounts, kind string, n int) {
	switch kind {
	case "bug":
		c.Bugs += n
	case "feature":
		c.Features += n
	}
}

// ownerStatusBaselineAt is the latest 21:00 in zone strictly before t.
func ownerStatusBaselineAt(t time.Time, zone *time.Location) time.Time {
	local := t.In(zone)
	b := time.Date(local.Year(), local.Month(), local.Day(), api.OwnerStatusBaselineHour, 0, 0, 0, zone)
	if !b.Before(t) {
		b = time.Date(local.Year(), local.Month(), local.Day()-1, api.OwnerStatusBaselineHour, 0, 0, 0, zone)
	}
	return b
}

func validOwnerStatusQuery(task string, q api.OwnerStatusQuery) (*time.Location, error) {
	if !api.ValidID(task, "tsk") {
		return nil, api.ErrInvalid
	}
	if q.Zone == "" || q.Zone == "Local" {
		return nil, fmt.Errorf("%w: tz must name an IANA zone", api.ErrInvalid)
	}
	zone, err := time.LoadLocation(q.Zone)
	if err != nil {
		return nil, fmt.Errorf("%w: tz %q is not a known IANA zone", api.ErrInvalid, q.Zone)
	}
	if q.From.IsZero() || q.To.IsZero() || !q.From.Before(q.To) {
		return nil, fmt.Errorf("%w: from and to are required and from must be before to", api.ErrInvalid)
	}
	if q.To.Sub(q.From) > api.OwnerStatusMaxSpan {
		return nil, fmt.Errorf("%w: the window is longer than 31 days", api.ErrInvalid)
	}
	return zone, nil
}

// OwnerStatus computes the project's owner status for [q.From, q.To). It is
// a read for every caller: the baseline insert never runs the ack gate.
func (s *Store) OwnerStatus(ctx context.Context, task string, q api.OwnerStatusQuery) (api.OwnerStatus, error) {
	zone, err := validOwnerStatusQuery(task, q)
	if err != nil {
		return api.OwnerStatus{}, err
	}
	if _, err := s.GetTask(ctx, task); err != nil {
		return api.OwnerStatus{}, err
	}
	now := s.now().UTC()
	from, to := q.From.UTC(), q.To.UTC()
	out := api.OwnerStatus{TaskID: task, From: from, To: to, Zone: q.Zone, ComputedAt: now}

	releases, err := s.ownerStatusReleases(ctx, task)
	if err != nil {
		return out, err
	}
	for _, r := range releases {
		if !ownerStatusSettled(r.state) {
			out.Releases.InLine++
			continue
		}
		// A settled job without a settled time cannot be placed in a window.
		if !r.ok || r.settled.Before(from) || !r.settled.Before(to) {
			continue
		}
		switch r.state {
		case "released":
			out.Releases.Released++
		case "rolled_back":
			out.Releases.RolledBack++
		case "refused":
			out.Releases.Refused++
		case "superseded":
			out.Releases.Superseded++
		}
	}

	items, err := s.ownerStatusRevisions(ctx, task)
	if err != nil {
		return out, err
	}
	out.Items.Opened, out.Items.Closed = ownerStatusOpenedClosed(items, from, to)
	out.Items.OpenNow = ownerStatusOpenAt(items, to)

	if b := ownerStatusBaselineAt(to, zone); b.Before(now) {
		baseline, err := s.ownerStatusBaseline(ctx, task, q.Zone, b.Format("2006-01-02"), b, releases, items, now)
		if err != nil {
			return out, err
		}
		out.Baseline = &baseline
		since := 0
		for _, r := range releases {
			if r.ok && r.state == "released" && !r.settled.Before(baseline.At) && r.settled.Before(to) {
				since++
			}
		}
		out.Releases.ReleasedSinceBaseline = &since
		out.Items.NetSinceBaseline = &api.OwnerStatusCounts{Bugs: out.Items.OpenNow.Bugs - baseline.Open.Bugs, Features: out.Items.OpenNow.Features - baseline.Open.Features}
	}

	queue, err := s.TeamQueuePage(ctx, task, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
	if err != nil {
		return out, err
	}
	out.Teams = ownerStatusTeams(queue)
	host, err := s.ownerStatusHost(ctx, queue)
	if err != nil {
		return out, err
	}
	out.Host = host
	return out, nil
}

// ownerStatusTeams maps the active team queue: launching and running
// entries, the limit, the lanes' hold and every stalled entry in queue order.
func ownerStatusTeams(queue api.TeamQueueList) api.OwnerStatusTeams {
	t := api.OwnerStatusTeams{Limit: queue.ConcurrencyLimit, Stalled: []api.OwnerStatusStall{}}
	if queue.Lanes != nil {
		t.Hold = queue.Lanes.Hold
	}
	for _, e := range queue.Entries {
		if e.State == "launching" || e.State == "running" {
			t.Running++
		}
		if e.Stall != nil {
			t.Stalled = append(t.Stalled, api.OwnerStatusStall{EntryID: e.ID, ItemID: e.ItemID, Cause: e.Stall.Cause, Since: e.Stall.Since})
		}
	}
	return t
}

// ownerStatusHost reads the census of the host the queue's lane summary
// reads (the first queued entry's host, else the first launching or running
// entry's), else of the most recently observed host. nil: no census.
func (s *Store) ownerStatusHost(ctx context.Context, queue api.TeamQueueList) (*api.OwnerStatusHost, error) {
	name := ""
	for _, e := range queue.Entries {
		if e.State == "queued" {
			name = e.Host
			break
		}
	}
	for _, e := range queue.Entries {
		if name == "" && (e.State == "launching" || e.State == "running") {
			name = e.Host
		}
	}
	if name == "" {
		var latest time.Time
		rows, err := s.db.QueryContext(ctx, `SELECT host,observed_at FROM team_host_usage`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var h, observed string
			if err := rows.Scan(&h, &observed); err != nil {
				rows.Close()
				return nil, err
			}
			if t := parseTS(observed); name == "" || t.After(latest) || (t.Equal(latest) && h < name) {
				name, latest = h, t
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || name == "" {
			return nil, err
		}
	}
	u, err := readTeamHostUsage(ctx, s.db, name)
	if err != nil || u == nil {
		return nil, err
	}
	policy, err := readTeamHostPolicy(ctx, s.db, name)
	if err != nil {
		return nil, err
	}
	h := &api.OwnerStatusHost{Host: u.Host, ObservedAt: u.ObservedAt, FreeDiskMiB: u.FreeDiskMiB, MemoryPressure: u.MemoryPressure, MatrixWaiters: u.MatrixWaiters,
		DiskReserveMiB: api.DefaultMinFreeDiskMiB, StrainPressure: api.DefaultLaneStrainPressure}
	if policy != nil {
		h.DiskReserveMiB = policy.DiskReserveMiB()
	}
	if queue.Lanes != nil {
		h.StrainPressure = queue.Lanes.Policy.StrainPressure
	}
	h.Strained = h.MemoryPressure != nil && *h.MemoryPressure > h.StrainPressure
	return h, nil
}

func ownerStatusSettled(state string) bool {
	switch state {
	case "released", "rolled_back", "refused", "superseded":
		return true
	}
	return false
}

// ownerStatusReleases reads every release job's state and settled time. The
// rows are read whole before returning: the store holds one connection.
func (s *Store) ownerStatusReleases(ctx context.Context, task string) ([]ownerStatusRelease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state,record_json FROM release_jobs WHERE task_id=? ORDER BY rowid`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ownerStatusRelease
	for rows.Next() {
		var r ownerStatusRelease
		var raw string
		if err := rows.Scan(&r.state, &raw); err != nil {
			return nil, err
		}
		var rec struct {
			SettledAt string `json:"settledAt"`
		}
		if json.Unmarshal([]byte(raw), &rec) == nil && rec.SettledAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, rec.SettledAt); err == nil {
				r.settled, r.ok = t, true
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ownerStatusRevisions reads every saved revision of the project's work
// items (kind, status and times only), grouped by item in revision order.
func (s *Store) ownerStatusRevisions(ctx context.Context, task string) ([][]ownerStatusRevision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT item_id,revision,item_kind,status,change_kind,created_at,updated_at FROM work_item_revisions WHERE item_task_id=? ORDER BY item_id,revision`, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]ownerStatusRevision
	last := ""
	for rows.Next() {
		var id, created, updated string
		var r ownerStatusRevision
		if err := rows.Scan(&id, &r.revision, &r.kind, &r.status, &r.changeKind, &created, &updated); err != nil {
			return nil, err
		}
		r.created, r.updated = parseTS(created), parseTS(updated)
		if id != last || len(out) == 0 {
			out = append(out, nil)
			last = id
		}
		out[len(out)-1] = append(out[len(out)-1], r)
	}
	return out, rows.Err()
}

// ownerStatusOpenAt counts items open at t by kind: each item's latest saved
// revision updated before t, when its status is not done or dismissed.
func ownerStatusOpenAt(items [][]ownerStatusRevision, t time.Time) api.OwnerStatusCounts {
	var c api.OwnerStatusCounts
	for _, revs := range items {
		var at *ownerStatusRevision
		for i := range revs {
			if revs[i].updated.Before(t) {
				at = &revs[i]
			}
		}
		if at != nil && !ownerStatusTerminal(at.status) {
			ownerStatusCount(&c, at.kind, 1)
		}
	}
	return c
}

// ownerStatusOpenedClosed counts items created in [from, to) by their first
// saved kind, and closings in the window by the kind at closing. A closing
// is a revision whose status is done or dismissed while the item's previous
// saved revision's was not, or an item created closed. A checkpoint that
// starts an item's saved history is not a closing: when it closed is not
// known.
func ownerStatusOpenedClosed(items [][]ownerStatusRevision, from, to time.Time) (opened, closed api.OwnerStatusCounts) {
	in := func(t time.Time) bool { return !t.Before(from) && t.Before(to) }
	for _, revs := range items {
		if len(revs) == 0 {
			continue
		}
		if in(revs[0].created) {
			ownerStatusCount(&opened, revs[0].kind, 1)
		}
		for i, r := range revs {
			if !ownerStatusTerminal(r.status) || !in(r.updated) {
				continue
			}
			if i == 0 {
				if r.changeKind == "created" {
					ownerStatusCount(&closed, r.kind, 1)
				}
				continue
			}
			if !ownerStatusTerminal(revs[i-1].status) {
				ownerStatusCount(&closed, r.kind, 1)
			}
		}
	}
	return opened, closed
}

// ownerStatusBaseline returns the stored baseline of (task, zone, day),
// recording it first when it is missing: released jobs settled before at,
// and items open at at.
func (s *Store) ownerStatusBaseline(ctx context.Context, task, zoneName, day string, at time.Time, releases []ownerStatusRelease, items [][]ownerStatusRevision, now time.Time) (api.OwnerStatusBaseline, error) {
	if b, err := s.readOwnerStatusBaseline(ctx, task, zoneName, day); err == nil {
		return b, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return b, err
	}
	released := 0
	for _, r := range releases {
		if r.ok && r.state == "released" && r.settled.Before(at) {
			released++
		}
	}
	open := ownerStatusOpenAt(items, at)
	s.writeMu.Lock()
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO owner_status_baselines(task_id,zone,day,at,released_total,open_bugs,open_features,recorded_at) VALUES(?,?,?,?,?,?,?,?)`,
		task, zoneName, day, ts(at), released, open.Bugs, open.Features, ts(now))
	s.writeMu.Unlock()
	if err != nil {
		return api.OwnerStatusBaseline{}, err
	}
	return s.readOwnerStatusBaseline(ctx, task, zoneName, day)
}

func (s *Store) readOwnerStatusBaseline(ctx context.Context, task, zoneName, day string) (api.OwnerStatusBaseline, error) {
	b := api.OwnerStatusBaseline{Day: day, Zone: zoneName}
	var at, recorded string
	err := s.db.QueryRowContext(ctx, `SELECT at,released_total,open_bugs,open_features,recorded_at FROM owner_status_baselines WHERE task_id=? AND zone=? AND day=?`, task, zoneName, day).
		Scan(&at, &b.ReleasedTotal, &b.Open.Bugs, &b.Open.Features, &recorded)
	b.At, b.RecordedAt = parseTS(at), parseTS(recorded)
	return b, err
}
