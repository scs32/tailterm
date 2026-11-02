package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The owner status call (wi_475a3df9b345e13e, order #55055). Isolated stores
// and a fake clock only; no live owner data.

var ownerStatusBy = api.Caller{Node: "fixture", User: "owner"}

type ownerStatusFixture struct {
	t    *testing.T
	path string
	s    *Store
	task api.Task
	now  time.Time
	la   *time.Location
	n    int
}

func newOwnerStatusFixture(t *testing.T, now time.Time) *ownerStatusFixture {
	t.Helper()
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	f := &ownerStatusFixture{t: t, path: filepath.Join(t.TempDir(), "owner-status.sqlite"), now: now, la: la}
	f.open()
	if f.task, err = f.s.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Owner status fixture"}, ownerStatusBy); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *ownerStatusFixture) open() {
	s, err := Open(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	s.now = func() time.Time { return f.now }
	f.s = s
	f.t.Cleanup(func() { s.Close() })
}

func (f *ownerStatusFixture) reopen() {
	f.s.Close()
	f.open()
}

// pst is a wall-clock time on 2026-11-day in Los Angeles (PST, UTC-8).
func (f *ownerStatusFixture) pst(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, f.la)
}

// job stores one release job as the ledger keeps it; settled "" leaves
// settledAt out, as on a job refused before wi_bd0310b7bbcd4a52.
func (f *ownerStatusFixture) job(state string, settled time.Time) {
	f.t.Helper()
	f.n++
	record := "{}"
	if !settled.IsZero() {
		record = fmt.Sprintf(`{"settledAt":%q}`, settled.UTC().Format(time.RFC3339Nano))
	}
	if _, err := f.s.db.Exec(`INSERT INTO release_jobs(task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,?,1,?)`,
		f.task.ID, fmt.Sprintf("rel_%016x", f.n), fmt.Sprintf("tqe_%016x", f.n), state, record); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ownerStatusFixture) item(kind string, at time.Time) api.WorkItem {
	f.t.Helper()
	f.now = at
	f.n++
	w, err := f.s.CreateWorkItem(context.Background(), f.task.ID, api.CreateWorkItemRequest{Kind: kind, Title: fmt.Sprintf("Fixture %s %d", kind, f.n), Priority: "normal", RequestID: api.NewID("req")}, ownerStatusBy)
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

func (f *ownerStatusFixture) status(w api.WorkItem, status string, at time.Time) api.WorkItem {
	f.t.Helper()
	f.now = at
	w, err := f.s.UpdateWorkItem(context.Background(), f.task.ID, w.ID, api.UpdateWorkItemRequest{Revision: w.Revision, Status: &status}, ownerStatusBy)
	if err != nil {
		f.t.Fatalf("%s -> %s: %v", w.ID, status, err)
	}
	return w
}

func (f *ownerStatusFixture) read(from, to time.Time, zone string) api.OwnerStatus {
	f.t.Helper()
	out, err := f.s.OwnerStatus(context.Background(), f.task.ID, api.OwnerStatusQuery{From: from, To: to, Zone: zone})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *ownerStatusFixture) baselineRows() int {
	f.t.Helper()
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM owner_status_baselines WHERE task_id=?`, f.task.ID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// a1: releases in the window by settled state, including a batch member's
// own released receipt and a deployer-refused job that carries settledAt;
// a refused job without settledAt is in no window; every unsettled job is in
// line.
func TestOwnerStatusReleaseCounts(t *testing.T) {
	f := newOwnerStatusFixture(t, time.Date(2026, 11, 2, 23, 0, 0, 0, time.UTC))
	from, to := f.pst(2, 13, 0), f.pst(2, 14, 0)
	f.job("released", f.pst(2, 13, 5))
	f.job("released", f.pst(2, 13, 5)) // batch member: same settled time as its lead
	f.job("released", f.pst(2, 14, 0)) // the end is outside [from, to)
	f.job("released", f.pst(2, 12, 59))
	f.job("rolled_back", f.pst(2, 13, 30))
	f.job("refused", f.pst(2, 13, 40)) // deployer refusal with its hub time
	f.job("refused", time.Time{})      // refused before settledAt was recorded
	f.job("superseded", f.pst(2, 13, 0))
	f.job("queued", time.Time{})
	f.job("claimed", time.Time{})
	f.job("held", time.Time{})
	got := f.read(from, to, "America/Los_Angeles").Releases
	want := api.OwnerStatusReleases{Released: 2, RolledBack: 1, Refused: 1, Superseded: 1, InLine: 3}
	got.ReleasedSinceBaseline = nil
	if got != want {
		t.Fatalf("releases %+v, want %+v", got, want)
	}
}

// a1: opened and closed in the window by kind, and open at the window end.
func TestOwnerStatusOpenedClosed(t *testing.T) {
	f := newOwnerStatusFixture(t, time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	from, to := f.pst(2, 13, 0), f.pst(2, 14, 0)
	before, inside, after := f.pst(2, 12, 0), f.pst(2, 13, 20), f.pst(2, 14, 10)

	closedBug := f.item("bug", before)
	f.status(closedBug, "done", inside) // closed in window
	dismissedFeature := f.item("feature", inside)
	dismissedFeature = f.status(dismissedFeature, "dismissed", inside.Add(5*time.Minute)) // opened and closed in window
	f.item("bug", inside.Add(time.Minute))                                                // opened, still open
	reopened := f.item("bug", before)
	reopened = f.status(reopened, "dismissed", before.Add(time.Minute))
	reopened = f.status(reopened, "open", inside) // reopening is not a closing
	working := f.item("feature", before)
	working = f.status(working, "in_progress", inside) // still open
	f.status(working, "blocked", inside.Add(time.Minute))
	edited := f.item("bug", before)
	edited = f.status(edited, "dismissed", before.Add(time.Minute))
	f.status(edited, "done", inside) // already closed: not a second closing
	late := f.item("feature", after)
	f.status(late, "dismissed", after.Add(time.Minute))
	f.status(dismissedFeature, "open", after) // reopened after the window

	f.now = after.Add(time.Hour)
	got := f.read(from, to, "America/Los_Angeles").Items
	if got.Opened != (api.OwnerStatusCounts{Bugs: 1, Features: 1}) {
		t.Fatalf("opened %+v", got.Opened)
	}
	if got.Closed != (api.OwnerStatusCounts{Bugs: 1, Features: 1}) {
		t.Fatalf("closed %+v", got.Closed)
	}
	// Open at 14:00: the new bug, the reopened bug and the working feature.
	if got.OpenNow != (api.OwnerStatusCounts{Bugs: 2, Features: 1}) {
		t.Fatalf("open now %+v", got.OpenNow)
	}
}

// a2: the first read after a day's 21:00 stores that day's baseline as of
// 21:00; later reads, concurrent reads and a reopened store return the same
// row after more releases and closures; another zone is its own row; a
// boundary not yet past on the hub stores nothing; a caller holding
// unacknowledged work still reads.
func TestOwnerStatusBaselineWriteOnce(t *testing.T) {
	f := newOwnerStatusFixture(t, time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	bugs := []api.WorkItem{f.item("bug", f.pst(2, 9, 0)), f.item("bug", f.pst(2, 10, 0))}
	feature := f.item("feature", f.pst(2, 11, 0))
	f.job("released", f.pst(2, 20, 0))
	f.job("released", f.pst(2, 21, 30))
	f.status(bugs[0], "dismissed", f.pst(2, 21, 30))
	f.now = f.pst(2, 22, 0)

	from, to := f.pst(2, 21, 0), f.pst(2, 22, 0)
	first := f.read(from, to, "America/Los_Angeles")
	b := first.Baseline
	if b == nil || b.Day != "2026-11-02" || !b.At.Equal(f.pst(2, 21, 0)) || b.Zone != "America/Los_Angeles" || b.ReleasedTotal != 1 ||
		b.Open != (api.OwnerStatusCounts{Bugs: 2, Features: 1}) || !b.RecordedAt.Equal(f.pst(2, 22, 0)) {
		t.Fatalf("baseline %+v", b)
	}
	if first.Releases.ReleasedSinceBaseline == nil || *first.Releases.ReleasedSinceBaseline != 1 {
		t.Fatalf("released since baseline %v", first.Releases.ReleasedSinceBaseline)
	}
	if n := first.Items.NetSinceBaseline; n == nil || *n != (api.OwnerStatusCounts{Bugs: -1, Features: 0}) {
		t.Fatalf("net since baseline %+v", n)
	}

	// More releases and closures later the same evening leave the row alone.
	f.job("released", f.pst(2, 22, 30))
	f.status(feature, "dismissed", f.pst(2, 22, 30))
	f.now = f.pst(2, 23, 0)
	later := f.read(f.pst(2, 22, 0), f.pst(2, 23, 0), "America/Los_Angeles")
	if later.Baseline == nil || *later.Baseline != *b {
		t.Fatalf("baseline changed: %+v, first %+v", later.Baseline, b)
	}
	if *later.Releases.ReleasedSinceBaseline != 2 || *later.Items.NetSinceBaseline != (api.OwnerStatusCounts{Bugs: -1, Features: -1}) {
		t.Fatalf("since baseline: released %d net %+v", *later.Releases.ReleasedSinceBaseline, *later.Items.NetSinceBaseline)
	}
	f.reopen()
	if again := f.read(from, to, "America/Los_Angeles"); again.Baseline == nil || *again.Baseline != *b {
		t.Fatalf("after reopen: %+v", again.Baseline)
	}

	// The same boundary in another zone is its own row.
	utc := f.read(from, to, "UTC")
	if utc.Baseline == nil || utc.Baseline.Zone != "UTC" || !utc.Baseline.At.Equal(time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)) || f.baselineRows() != 2 {
		t.Fatalf("UTC baseline %+v rows %d", utc.Baseline, f.baselineRows())
	}

	// A boundary after the hub's now stores nothing and returns none.
	future := f.read(f.pst(3, 21, 0), f.pst(3, 22, 0), "America/Los_Angeles")
	if future.Baseline != nil || future.Releases.ReleasedSinceBaseline != nil || future.Items.NetSinceBaseline != nil || f.baselineRows() != 2 {
		t.Fatalf("future boundary stored: %+v rows %d", future.Baseline, f.baselineRows())
	}

	// Concurrent first reads of a new day store one row and agree on it.
	f.now = f.pst(3, 22, 0)
	var wg sync.WaitGroup
	results := make([]*api.OwnerStatusBaseline, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := f.s.OwnerStatus(context.Background(), f.task.ID, api.OwnerStatusQuery{From: f.pst(3, 21, 0), To: f.pst(3, 22, 0), Zone: "America/Los_Angeles"})
			results[i], errs[i] = out.Baseline, err
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i] == nil || *results[i] != *results[0] {
			t.Fatalf("concurrent read %d: %+v %v, first %+v", i, results[i], errs[i], results[0])
		}
	}
	if f.baselineRows() != 3 || results[0].ReleasedTotal != 3 || results[0].Open != (api.OwnerStatusCounts{Bugs: 1}) {
		t.Fatalf("new day rows %d baseline %+v", f.baselineRows(), results[0])
	}

	// A caller holding unacknowledged work past the grace still reads: the
	// read and its baseline insert never pass the ack gate.
	ctx := context.Background()
	gated, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "gated", Host: "h", Session: "gated", Runtime: "codex"}, ownerStatusBy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostMessage(ctx, f.task.ID, api.PostMessageRequest{To: gated.ID, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: gated.Name, Subject: "Count the open bugs", Body: api.EnvelopeBody{Ask: "count"}}}, ownerStatusBy); err != nil {
		t.Fatal(err)
	}
	f.now = f.pst(4, 22, 0)
	var unacked *api.UnacknowledgedError
	if _, err = f.s.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Gated write", AgentID: gated.ID, RequestID: "gated"}, ownerStatusBy); !errors.As(err, &unacked) {
		t.Fatalf("fixture: the agent is not gated: %v", err)
	}
	if out := f.read(f.pst(4, 21, 0), f.pst(4, 22, 0), "America/Los_Angeles"); out.Baseline == nil || f.baselineRows() != 4 {
		t.Fatalf("read beside unacknowledged work: %+v rows %d", out.Baseline, f.baselineRows())
	}
}

// a1: host health is the census of the queue's host with its reserve and
// strain; teams count launching and running entries against the limit, and
// carry the lanes' hold and every stalled entry in queue order.
func TestOwnerStatusHostAndStalls(t *testing.T) {
	pressure, waiters := 81, 2
	list := api.TeamQueueList{ConcurrencyLimit: 4, Lanes: &api.TeamQueueLanes{Hold: "a matrix run waits for the verification host"}, Entries: []api.TeamQueueEntry{
		{ID: "tqe_a", ItemID: "wi_a", State: "running"},
		{ID: "tqe_b", ItemID: "wi_b", State: "launching"},
		{ID: "tqe_c", ItemID: "wi_c", State: "queued", Stall: &api.TeamQueueStall{Cause: api.StallIdleEntry, Since: "2026-11-02T13:00:00Z"}},
		{ID: "tqe_d", ItemID: "wi_d", State: "queued"},
		{ID: "tqe_e", ItemID: "wi_e", State: "queued", Stall: &api.TeamQueueStall{Cause: api.StallFailedEntry}},
	}}
	teams := ownerStatusTeams(list)
	if teams.Running != 2 || teams.Limit != 4 || teams.Hold != list.Lanes.Hold || len(teams.Stalled) != 2 ||
		teams.Stalled[0] != (api.OwnerStatusStall{EntryID: "tqe_c", ItemID: "wi_c", Cause: api.StallIdleEntry, Since: "2026-11-02T13:00:00Z"}) || teams.Stalled[1].EntryID != "tqe_e" {
		t.Fatalf("teams %+v", teams)
	}
	if none := ownerStatusTeams(api.TeamQueueList{}); none.Stalled == nil || len(none.Stalled) != 0 || none.Hold != "" {
		t.Fatalf("empty queue %+v", none)
	}

	// The real queue: one running team, one queued, the census of their host
	// (the fixture's runner census, replaced below), not the newer one of a
	// host the queue does not use.
	f := newChoresQueue(t, 2, 1, 3)
	f.run(t, f.add(t, 0, "src/a"))
	f.add(t, 1, "src/b")
	read := func() api.OwnerStatus {
		t.Helper()
		now := time.Now().UTC()
		out, err := f.s.OwnerStatus(context.Background(), f.task.ID, api.OwnerStatusQuery{From: now.Add(-time.Hour), To: now, Zone: "UTC"})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read(); got.Teams.Running != 1 || got.Teams.Limit != 3 {
		t.Fatalf("teams %+v", got.Teams)
	}
	observed := ts(time.Now().UTC().Add(-3 * time.Minute))
	if _, err := f.s.db.Exec(`INSERT OR REPLACE INTO team_host_usage(host,limiter_domain,policy_version,observed_at,relay_bindings,complete,source_digest,free_disk_mib,memory_pressure,matrix_waiters) VALUES('mini','http://hub',0,?,0,1,'digest',20480,?,?)`, observed, pressure, waiters); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO team_host_usage(host,limiter_domain,policy_version,observed_at,relay_bindings,complete,source_digest) VALUES('air','http://hub',0,?,0,1,'digest')`, ts(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	got := read()
	if got.Host == nil || got.Host.Host != "mini" || got.Host.ObservedAt != observed || *got.Host.FreeDiskMiB != 20480 || got.Host.DiskReserveMiB != api.DefaultMinFreeDiskMiB ||
		*got.Host.MemoryPressure != pressure || got.Host.StrainPressure != api.DefaultLaneStrainPressure || !got.Host.Strained || *got.Host.MatrixWaiters != waiters {
		t.Fatalf("census of the queue's host: %+v", got.Host)
	}
	// Unobserved readings stay nil and are never strained.
	if _, err := f.s.db.Exec(`UPDATE team_host_usage SET free_disk_mib=NULL,memory_pressure=NULL,matrix_waiters=NULL WHERE host='mini'`); err != nil {
		t.Fatal(err)
	}
	if got = read(); got.Host.FreeDiskMiB != nil || got.Host.MemoryPressure != nil || got.Host.MatrixWaiters != nil || got.Host.Strained {
		t.Fatalf("nil readings: %+v", got.Host)
	}
}

// a1: a bad window or zone is refused before any read; an unknown project is
// not found.
func TestOwnerStatusRejectsBadQueries(t *testing.T) {
	f := newOwnerStatusFixture(t, time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	from := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()
	for name, q := range map[string]api.OwnerStatusQuery{
		"no zone":        {From: from, To: from.Add(time.Hour)},
		"Local":          {From: from, To: from.Add(time.Hour), Zone: "Local"},
		"unknown zone":   {From: from, To: from.Add(time.Hour), Zone: "Mars/Olympus"},
		"no from":        {To: from, Zone: "UTC"},
		"no to":          {From: from, Zone: "UTC"},
		"equal bounds":   {From: from, To: from, Zone: "UTC"},
		"from after to":  {From: from.Add(time.Hour), To: from, Zone: "UTC"},
		"over 31 days":   {From: from, To: from.Add(api.OwnerStatusMaxSpan + time.Second), Zone: "UTC"},
		"bad project id": {From: from, To: from.Add(time.Hour), Zone: "UTC"},
	} {
		task := f.task.ID
		if name == "bad project id" {
			task = "not-a-task"
		}
		if _, err := f.s.OwnerStatus(ctx, task, q); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := f.s.OwnerStatus(ctx, f.task.ID, api.OwnerStatusQuery{From: from, To: from.Add(api.OwnerStatusMaxSpan), Zone: "UTC"}); err != nil {
		t.Fatalf("a 31-day window was refused: %v", err)
	}
	if _, err := f.s.OwnerStatus(ctx, "tsk_0000000000000000", api.OwnerStatusQuery{From: from, To: from.Add(time.Hour), Zone: "UTC"}); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	if f.baselineRows() != 0 {
		t.Fatalf("a refused read stored a baseline")
	}
}
