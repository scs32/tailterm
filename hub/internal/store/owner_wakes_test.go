package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The owner helper's wakes on the hub (bug wi_8d18e0fe8bdf2ad6, order
// #52946). Isolated stores and a fake clock only.

var wakeBy = api.Caller{Node: "fixture", User: "owner"}

type wakeFixture struct {
	t      *testing.T
	s      *Store
	task   api.Task
	path   string
	now    time.Time
	msg    int64
	helper api.Agent
}

// wakeStore opens a store whose clock is 2026-11-02 06:50 Pacific (PST).
func wakeStore(t *testing.T, withHelper bool) *wakeFixture {
	t.Helper()
	f := &wakeFixture{t: t, path: filepath.Join(t.TempDir(), "wakes.sqlite"), now: time.Date(2026, 11, 2, 14, 50, 0, 0, time.UTC)}
	f.open()
	ctx := context.Background()
	task, err := f.s.CreateTask(ctx, api.CreateTaskRequest{Name: "Wake fixture", Orchestrator: "lead"}, wakeBy)
	if err != nil {
		t.Fatal(err)
	}
	f.task = task
	m, err := f.s.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "synthetic wake instructions"}, wakeBy)
	if err != nil {
		t.Fatal(err)
	}
	f.msg = m.Seq
	if withHelper {
		f.helper = *registerHelper(t, f.s, task.ID, helperRequest("wake-reg-1"), wakeBy).Agent
	}
	return f
}

func (f *wakeFixture) open() {
	s, err := Open(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	s.now = func() time.Time { return f.now }
	f.s = s
	f.t.Cleanup(func() { s.Close() })
}

// reopen is a hub restart over the same database.
func (f *wakeFixture) reopen() {
	f.s.Close()
	f.open()
}

func (f *wakeFixture) set(req api.SetOwnerHelperWakeRequest) (api.OwnerHelperWake, bool, error) {
	return f.s.SetOwnerHelperWake(context.Background(), f.task.ID, req, wakeBy)
}

func (f *wakeFixture) create(key, kind, schedule string) api.OwnerHelperWake {
	f.t.Helper()
	w, created, err := f.set(api.SetOwnerHelperWakeRequest{Kind: kind, Schedule: schedule, MessageSeq: f.msg, RequestID: key})
	if err != nil || !created || w.State != api.OwnerHelperWakeActive || w.NextDueAt == nil {
		f.t.Fatalf("create %s: %+v created=%v %v", key, w, created, err)
	}
	return w
}

func (f *wakeFixture) sweep(at time.Time) []OwnerHelperWakeStep {
	f.t.Helper()
	f.now = at
	steps, err := f.s.OwnerHelperWakeSweep(context.Background(), at)
	if err != nil {
		f.t.Fatalf("sweep at %s: %v", at, err)
	}
	return steps
}

func (f *wakeFixture) firings(wake string) []api.OwnerHelperWakeFiring {
	f.t.Helper()
	list, err := ownerWakeFirings(context.Background(), f.s.db, wake, 1000)
	if err != nil {
		f.t.Fatal(err)
	}
	return list
}

func (f *wakeFixture) wake(id string) api.OwnerHelperWake {
	f.t.Helper()
	w, err := scanOwnerWake(f.s.db.QueryRow(`SELECT `+ownerWakeCols+` FROM owner_helper_wakes WHERE id=?`, id))
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

// read is the helper reading its inbox at at: its cursor passes every
// message and its delivery-only obligations close.
func (f *wakeFixture) read(at time.Time) {
	f.t.Helper()
	ctx := context.Background()
	var top int64
	if err := f.s.db.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM messages WHERE task_id=?`, f.task.ID).Scan(&top); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.MarkRead(ctx, f.task.ID, api.MarkReadRequest{AgentID: f.helper.ID, UpTo: top}); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.MarkObligationsDelivered(ctx, f.task.ID, f.helper.ID, f.helper.RunID, at); err != nil {
		f.t.Fatal(err)
	}
}

type wakeNotice struct {
	seq int64
	to  string
	env api.Envelope
}

// notices lists the broker's notices after seq, oldest first.
func (f *wakeFixture) notices(after int64) []wakeNotice {
	f.t.Helper()
	rows, err := f.s.db.Query(`SELECT seq,to_agent,envelope FROM messages WHERE task_id=? AND from_node=? AND seq>? ORDER BY seq`, f.task.ID, api.BrokerNode, after)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []wakeNotice
	for rows.Next() {
		var n wakeNotice
		var raw string
		if err := rows.Scan(&n.seq, &n.to, &raw); err != nil {
			f.t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &n.env); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func pst(day, hour, minute, second int) time.Time {
	return time.Date(2026, 11, day, hour+8, minute, second, 0, time.UTC)
}

// a1: the parser and the next slot, in the wake's zone.
func TestOwnerHelperWakeSchedule(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		schedule string
		loc      *time.Location
		after    time.Time
		want     []time.Time
	}{
		{"14 7-20 * * *", la, pst(2, 7, 0, 0), []time.Time{pst(2, 7, 14, 0), pst(2, 8, 14, 0)}},              // number and range
		{"14 7-20 * * *", la, pst(2, 20, 14, 0), []time.Time{pst(3, 7, 14, 0)}},                              // the range ends
		{"8,28,48 * * * *", la, pst(2, 7, 30, 0), []time.Time{pst(2, 7, 48, 0), pst(2, 8, 8, 0)}},            // list
		{"*/15 * * * *", la, pst(2, 7, 1, 0), []time.Time{pst(2, 7, 15, 0), pst(2, 7, 30, 0)}},               // step
		{"0 9-17/4 * * *", la, pst(2, 7, 0, 0), []time.Time{pst(2, 9, 0, 0), pst(2, 13, 0, 0)}},              // stepped range
		{"0 8 * * 0", la, pst(2, 7, 0, 0), []time.Time{pst(8, 8, 0, 0)}},                                     // Sunday as 0
		{"0 8 * * 7", la, pst(2, 7, 0, 0), []time.Time{pst(8, 8, 0, 0)}},                                     // Sunday as 7
		{"0 8 * * 5-7", la, pst(2, 7, 0, 0), []time.Time{pst(6, 8, 0, 0), pst(7, 8, 0, 0), pst(8, 8, 0, 0)}}, // a range ending at 7
		{"0 8 15 * 1", la, pst(2, 7, 0, 0), []time.Time{pst(2, 8, 0, 0), pst(9, 8, 0, 0), pst(15, 8, 0, 0)}}, // either day field
		{"0 8 15 * *", la, pst(2, 7, 0, 0), []time.Time{pst(15, 8, 0, 0)}},                                   // day of month alone
		{"0 7 * * *", london, pst(2, 7, 0, 0), []time.Time{time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC)}},    // another zone
		{"  0   7 * * * ", la, pst(2, 6, 59, 59), []time.Time{pst(2, 7, 0, 0)}},                              // spaces normalised
	} {
		norm, s, err := parseOwnerWakeSchedule(c.schedule)
		if err != nil || norm != strings.Join(strings.Fields(c.schedule), " ") {
			t.Fatalf("%q: %q %v", c.schedule, norm, err)
		}
		after := c.after
		for i, want := range c.want {
			got, ok := ownerWakeNext(&s, c.loc, after)
			if !ok || !got.Equal(want) {
				t.Fatalf("%q slot %d after %s: %s %v; want %s", c.schedule, i+1, after, got, ok, want)
			}
			after = got
		}
	}
	for _, bad := range []string{"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "0 0 0 * *", "0 0 32 * *", "0 0 * 0 *", "0 0 * 13 *",
		"0 0 * * 8", "MON * * * *", "0 0 * * MON", "5-1 * * * *", "*/0 * * * *", "1,,2 * * * *", "-1 * * * *", "100 * * * *", "0 0 * * 1-", strings.Repeat("1,", 40) + "1 * * * *"} {
		if _, _, err := parseOwnerWakeSchedule(bad); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%q was accepted: %v", bad, err)
		}
	}
	// A schedule that never fires within a year has no next slot.
	_, never, _ := parseOwnerWakeSchedule("0 0 31 2 *")
	if got, ok := ownerWakeNext(&never, la, pst(2, 7, 0, 0)); ok {
		t.Fatalf("31 February fired at %s", got)
	}
}

// a1: the 2026-11-01 fall back fires a 01:30 wake once; the 2027-03-14 spring
// forward skips a 02:30 wake that day.
func TestOwnerHelperWakeDST(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	_, fall, _ := parseOwnerWakeSchedule("30 1 * * *")
	first, ok := ownerWakeNext(&fall, la, time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC))
	if !ok || !first.Equal(time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC)) { // 01:30 PDT
		t.Fatalf("fall back first slot %s", first)
	}
	second, ok := ownerWakeNext(&fall, la, first)
	if !ok || !second.Equal(time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)) { // 01:30 PST the next day, not 01:30 PST on 11-01
		t.Fatalf("fall back second slot %s", second)
	}
	_, spring, _ := parseOwnerWakeSchedule("30 2 * * *")
	got, ok := ownerWakeNext(&spring, la, time.Date(2027, 3, 13, 12, 0, 0, 0, time.UTC))
	if !ok || !got.Equal(time.Date(2027, 3, 15, 9, 30, 0, 0, time.UTC)) { // 02:30 PDT on 03-15
		t.Fatalf("spring forward slot %s", got)
	}
	// Through the sweep: the fall-back wake posts once that night.
	f := wakeStore(t, true)
	f.now = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	w := f.create("wake-fall", "other", "30 1 * * *")
	if steps := f.sweep(time.Date(2026, 11, 1, 8, 30, 20, 0, time.UTC)); len(steps) != 1 || steps[0].Action != ownerWakeActionPosted {
		t.Fatalf("01:30 PDT %+v", steps)
	}
	f.read(time.Date(2026, 11, 1, 8, 31, 0, 0, time.UTC))
	if steps := f.sweep(time.Date(2026, 11, 1, 9, 30, 20, 0, time.UTC)); len(steps) != 0 {
		t.Fatalf("01:30 PST posted again: %+v", steps)
	}
	if list := f.firings(w.ID); len(list) != 1 {
		t.Fatalf("fall-back firings %+v", list)
	}
}

// R2: the zone loads from the store's embedded zone data.
func TestOwnerHelperWakeZoneData(t *testing.T) {
	t.Setenv("ZONEINFO", filepath.Join(t.TempDir(), "none"))
	if _, err := ownerWakeZone("America/Los_Angeles"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "Local", "Mars/Olympus", "../etc", strings.Repeat("A", 65)} {
		if _, err := ownerWakeZone(bad); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("zone %q: %v", bad, err)
		}
	}
}

// a1: what a save refuses, and what it stores.
func TestOwnerHelperWakeSaveValidation(t *testing.T) {
	f := wakeStore(t, false)
	ctx := context.Background()
	other, err := f.s.CreateTask(ctx, api.CreateTaskRequest{Name: "Other fixture", Orchestrator: "lead"}, wakeBy)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := f.s.PostMessage(ctx, other.ID, api.PostMessageRequest{Text: "another project's message"}, wakeBy)
	if err != nil {
		t.Fatal(err)
	}
	past := f.now.Add(-time.Minute)
	soon := pst(2, 7, 10, 0) // before the first 07:14 slot
	ok := api.SetOwnerHelperWakeRequest{Kind: "hourly-update", Schedule: "14 7-20 * * *", MessageSeq: f.msg}
	for name, edit := range map[string]func(r *api.SetOwnerHelperWakeRequest){
		"no request id":      func(r *api.SetOwnerHelperWakeRequest) { r.RequestID = "" },
		"bad kind":           func(r *api.SetOwnerHelperWakeRequest) { r.Kind = "nightly" },
		"no kind":            func(r *api.SetOwnerHelperWakeRequest) { r.Kind = "" },
		"bad schedule":       func(r *api.SetOwnerHelperWakeRequest) { r.Schedule = "0 25 * * *" },
		"never fires":        func(r *api.SetOwnerHelperWakeRequest) { r.Schedule = "0 0 31 2 *" },
		"local zone":         func(r *api.SetOwnerHelperWakeRequest) { r.TimeZone = "Local" },
		"unknown zone":       func(r *api.SetOwnerHelperWakeRequest) { r.TimeZone = "Mars/Olympus" },
		"no message":         func(r *api.SetOwnerHelperWakeRequest) { r.MessageSeq = 0 },
		"missing message":    func(r *api.SetOwnerHelperWakeRequest) { r.MessageSeq = 999999 },
		"foreign message":    func(r *api.SetOwnerHelperWakeRequest) { r.MessageSeq = foreign.Seq },
		"past expiry":        func(r *api.SetOwnerHelperWakeRequest) { r.ExpiresAt = &past },
		"expires before due": func(r *api.SetOwnerHelperWakeRequest) { r.ExpiresAt = &soon },
		"bad state":          func(r *api.SetOwnerHelperWakeRequest) { r.State = "paused" },
		"new with revision":  func(r *api.SetOwnerHelperWakeRequest) { r.ExpectedRevision = 1 },
		"bad wake id":        func(r *api.SetOwnerHelperWakeRequest) { r.WakeID, r.ExpectedRevision = "ohw_nothex", 1 },
		"change no revision": func(r *api.SetOwnerHelperWakeRequest) { r.WakeID = "ohw_0123456789abcdef" },
		"cancel new":         func(r *api.SetOwnerHelperWakeRequest) { r.State = api.OwnerHelperWakeCancelled },
	} {
		req := ok
		req.RequestID = "wake-" + strings.ReplaceAll(name, " ", "-")
		edit(&req)
		if _, _, err := f.set(req); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s: %v; want invalid", name, err)
		}
	}
	if _, _, err := f.s.SetOwnerHelperWake(ctx, "not-a-task", api.SetOwnerHelperWakeRequest{Kind: "other", Schedule: "0 7 * * *", MessageSeq: f.msg, RequestID: "wake-x"}, wakeBy); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("bad project id: %v", err)
	}
	// Every kind is accepted; the zone defaults to Pacific.
	for i, kind := range api.OwnerHelperWakeKinds {
		w := f.create("wake-kind-"+kind, kind, "0 7 * * *")
		if w.TimeZone != api.DefaultOwnerHelperWakeZone || w.Kind != kind || w.Revision != 1 || w.MessageSeq != f.msg || !w.NextDueAt.Equal(pst(2, 7, 0, 0)) || w.CreatedBy != wakeBy {
			t.Fatalf("kind %d %s: %+v", i, kind, w)
		}
	}
	w, _, err := f.set(api.SetOwnerHelperWakeRequest{Kind: "other", Schedule: "0 7 * * *", TimeZone: "Europe/London", MessageSeq: f.msg, RequestID: "wake-london"})
	if err != nil || !w.NextDueAt.Equal(time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC)) {
		t.Fatalf("London wake %+v %v", w, err)
	}
	// A cancel takes no other field.
	if _, _, err := f.set(api.SetOwnerHelperWakeRequest{WakeID: w.ID, ExpectedRevision: 1, State: api.OwnerHelperWakeCancelled, Kind: "other", RequestID: "wake-cancel-extra"}); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("cancel with a kind: %v", err)
	}
	if list, err := f.s.ListOwnerHelperWakes(ctx, f.task.ID, 0); err != nil || len(list.Wakes) != len(api.OwnerHelperWakeKinds)+1 {
		t.Fatalf("refusals were saved: %d %v", len(list.Wakes), err)
	}
}

// a3: the owner and the current helper's exact run write; nobody else does.
func TestOwnerHelperWakeWriter(t *testing.T) {
	f := wakeStore(t, false)
	ctx := context.Background()
	write := func(key string, who api.Agent) (api.OwnerHelperWake, error) {
		w, _, err := f.set(api.SetOwnerHelperWakeRequest{Kind: "queue-check", Schedule: "8,28,48 * * * *", MessageSeq: f.msg, RequestID: key, AgentID: who.ID, RunID: who.RunID})
		return w, err
	}
	refused := func(key string, who api.Agent, want string) {
		t.Helper()
		if _, err := write(key, who); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v; want a conflict naming %q", key, err, want)
		}
	}
	refused("no-helper", api.Agent{ID: api.NewID("agt"), RunID: api.NewID("run")}, "is not the project's owner helper")
	if _, err := write("owner", api.Agent{}); err != nil {
		t.Fatal(err)
	}
	first := *registerHelper(t, f.s, f.task.ID, helperRequest("reg-1"), wakeBy).Agent
	w, err := write("helper-create", first)
	if err != nil || w.AgentID != first.ID || w.RunID != first.RunID {
		t.Fatalf("helper create %+v %v", w, err)
	}
	// The helper changes and cancels its own wake.
	changed, _, err := f.set(api.SetOwnerHelperWakeRequest{WakeID: w.ID, ExpectedRevision: 1, Kind: "queue-check", Schedule: "0,20,40 * * * *", MessageSeq: f.msg, RequestID: "helper-change", AgentID: first.ID, RunID: first.RunID})
	if err != nil || changed.Revision != 2 || changed.Schedule != "0,20,40 * * * *" || !changed.NextDueAt.Equal(pst(2, 7, 0, 0)) {
		t.Fatalf("helper change %+v %v", changed, err)
	}
	cancelled, _, err := f.set(api.SetOwnerHelperWakeRequest{WakeID: w.ID, ExpectedRevision: 2, State: api.OwnerHelperWakeCancelled, RequestID: "helper-cancel", AgentID: first.ID, RunID: first.RunID})
	if err != nil || cancelled.State != api.OwnerHelperWakeCancelled || cancelled.NextDueAt != nil || cancelled.Revision != 3 || cancelled.Schedule != "0,20,40 * * * *" {
		t.Fatalf("helper cancel %+v %v", cancelled, err)
	}
	refused("agent-only", api.Agent{ID: first.ID}, "give both the agent and its run")
	refused("run-only", api.Agent{RunID: first.RunID}, "give both the agent and its run")
	worker, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: "worker", Host: "mini", Session: "worker", Runtime: "claude"}, wakeBy)
	if err != nil {
		t.Fatal(err)
	}
	refused("worker", worker, "is not the project's owner helper")
	second := *registerHelper(t, f.s, f.task.ID, helperRequest("reg-2"), wakeBy).Agent
	refused("previous-run", first, "was replaced")
	setAgentStatus(t, f.s, second.ID, api.AgentRetired)
	refused("retired", second, "the owner helper is retired")
	setAgentStatus(t, f.s, second.ID, api.AgentRunning)
	if _, err := f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{Kind: api.EventExited, AgentID: second.ID, RunID: second.RunID}, wakeBy); err != nil {
		t.Fatal(err)
	}
	refused("exited", second, "the owner helper is exited")
	third := *registerHelper(t, f.s, f.task.ID, helperRequest("reg-3"), wakeBy).Agent
	if _, err := write("helper-third", third); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CloseAgentRun(ctx, third.ID, third.RunID, wakeBy); err != nil {
		t.Fatal(err)
	}
	refused("closed", third, "is not the project's owner helper")
	// Any identified caller lists: the store read needs no writer.
	if list, err := f.s.ListOwnerHelperWakes(ctx, f.task.ID, 0); err != nil || len(list.Wakes) != 3 {
		t.Fatalf("list %+v %v", list, err)
	}
}

// a3: replay, conflict, stale revision, the active cap and a closed project.
func TestOwnerHelperWakeReplayRevisionCap(t *testing.T) {
	f := wakeStore(t, false)
	ctx := context.Background()
	req := api.SetOwnerHelperWakeRequest{Kind: "hourly-update", Schedule: "14 7-20 * * *", MessageSeq: f.msg, RequestID: "wake-1"}
	w, created, err := f.set(req)
	if err != nil || !created {
		t.Fatalf("create %+v %v", w, err)
	}
	if again, created, err := f.set(req); err != nil || !created || again.ID != w.ID || again.Revision != 1 {
		t.Fatalf("replay %+v created=%v %v", again, created, err)
	}
	other := req
	other.Schedule = "15 7-20 * * *"
	if _, _, err := f.set(other); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("reused request id: %v", err)
	}
	change := api.SetOwnerHelperWakeRequest{WakeID: w.ID, ExpectedRevision: 1, Kind: "hourly-update", Schedule: "14 7-20 * * *", MessageSeq: f.msg, RequestID: "wake-1-change"}
	changed, created, err := f.set(change)
	if err != nil || created || changed.Revision != 2 || !changed.NextDueAt.Equal(*w.NextDueAt) {
		t.Fatalf("change %+v created=%v %v", changed, created, err)
	}
	// The create replayed after a later change returns the wake, not a new one.
	if again, created, err := f.set(req); err != nil || !created || again.ID != w.ID || again.Revision != 2 {
		t.Fatalf("create replay after a change %+v %v", again, err)
	}
	if again, created, err := f.set(change); err != nil || created || again.Revision != 2 {
		t.Fatalf("change replay %+v %v", again, err)
	}
	stale := change
	stale.RequestID = "wake-1-stale"
	if _, _, err := f.set(stale); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "revision 2") {
		t.Fatalf("stale revision: %v", err)
	}
	missing := change
	missing.WakeID, missing.RequestID = "ohw_0123456789abcdef", "wake-missing"
	if _, _, err := f.set(missing); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown wake: %v", err)
	}
	// Up to MaxOwnerHelperWakes active; a cancelled one frees a place.
	for i := 2; i <= api.MaxOwnerHelperWakes; i++ {
		f.create("wake-cap-"+strings.Repeat("x", i), "other", "0 7 * * *")
	}
	if _, _, err := f.set(api.SetOwnerHelperWakeRequest{Kind: "other", Schedule: "0 7 * * *", MessageSeq: f.msg, RequestID: "wake-21"}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "20 active wakes") {
		t.Fatalf("21st active wake: %v", err)
	}
	if _, _, err := f.set(api.SetOwnerHelperWakeRequest{WakeID: w.ID, ExpectedRevision: 2, State: api.OwnerHelperWakeCancelled, RequestID: "wake-1-cancel"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.set(api.SetOwnerHelperWakeRequest{WakeID: w.ID, ExpectedRevision: 3, Kind: "hourly-update", Schedule: "14 7-20 * * *", MessageSeq: f.msg, RequestID: "wake-1-revive"}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is cancelled") {
		t.Fatalf("change after cancel: %v", err)
	}
	f.create("wake-21-after-cancel", "other", "0 7 * * *")
	// A paused project may be written; a closed one may not.
	f.s.db.Exec(`UPDATE tasks SET pause_state='paused' WHERE id=?`, f.task.ID)
	if _, _, err := f.set(api.SetOwnerHelperWakeRequest{WakeID: w.ID, ExpectedRevision: 3, State: api.OwnerHelperWakeCancelled, RequestID: "wake-paused"}); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "is cancelled") {
		t.Fatalf("a paused project's write reached the wake check: %v", err)
	}
	if _, err := f.s.CloseTask(ctx, f.task.ID, wakeBy); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.set(api.SetOwnerHelperWakeRequest{Kind: "other", Schedule: "0 7 * * *", MessageSeq: f.msg, RequestID: "wake-closed"}); !errors.Is(err, api.ErrClosed) {
		t.Fatalf("closed project: %v", err)
	}
	if list, err := f.s.ListOwnerHelperWakes(ctx, f.task.ID, 0); err != nil || len(list.Wakes) != api.MaxOwnerHelperWakes+1 {
		t.Fatalf("a closed project's wakes are listed: %d %v", len(list.Wakes), err)
	}
}

// a2: each kind posts one directed notice per slot, never again on a second
// sweep or after a restart, and posts again at its next slot.
func TestOwnerHelperWakeDelivery(t *testing.T) {
	for _, c := range []struct {
		kind, schedule, words string
		first, second         time.Time
	}{
		{"hourly-update", "14 7-20 * * *", "hourly update", pst(2, 7, 14, 0), pst(2, 8, 14, 0)},
		{"morning-summary", "0 7 * * *", "morning summary", pst(2, 7, 0, 0), pst(3, 7, 0, 0)},
		{"evening-summary", "0 21 * * *", "evening summary", pst(2, 21, 0, 0), pst(3, 21, 0, 0)},
		{"queue-check", "8,28,48 * * * *", "queue check (team watch)", pst(2, 7, 8, 0), pst(2, 7, 28, 0)},
	} {
		t.Run(c.kind, func(t *testing.T) {
			f := wakeStore(t, true)
			w := f.create("wake-"+c.kind, c.kind, c.schedule)
			if !w.NextDueAt.Equal(c.first) {
				t.Fatalf("next due %s; want %s", w.NextDueAt, c.first)
			}
			if steps := f.sweep(c.first.Add(-time.Second)); len(steps) != 0 {
				t.Fatalf("posted before due: %+v", steps)
			}
			steps := f.sweep(c.first.Add(20 * time.Second))
			if len(steps) != 1 || steps[0].Action != ownerWakeActionPosted || steps[0].WakeID != w.ID || steps[0].TaskID != f.task.ID {
				t.Fatalf("first slot steps %+v", steps)
			}
			got := f.notices(0)
			if len(got) != 1 || got[0].seq != steps[0].MessageSeq || got[0].to != f.helper.ID {
				t.Fatalf("notices %+v", got)
			}
			env := got[0].env
			subject := "Owner-helper wake: " + c.words + " due " + c.first.In(time.FixedZone("PST", -8*3600)).Format("15:04") + " PST"
			text := "Wake " + w.ID + " (" + c.kind + ") was due at " + c.first.Format(time.RFC3339) + ". Do what Board message #" + strconv.FormatInt(f.msg, 10) + " says. This wake comes from the hub: delete any session cron for it."
			wantRefs := map[string]string{"ownerWake": c.kind, "wake": w.ID, "due": c.first.Format(time.RFC3339), "message": strconv.FormatInt(f.msg, 10)}
			if env.Kind != api.EnvelopeKindNotice || env.Subject != subject || env.Body.Text != text || env.To != f.helper.Name || !sameRefs(env.Refs, wantRefs) {
				t.Fatalf("notice %+v\nwant subject %q text %q refs %v", env, subject, text, wantRefs)
			}
			var needs, state string
			if err := f.s.db.QueryRow(`SELECT needs,state FROM obligations WHERE message_seq=? AND agent_id=?`, got[0].seq, f.helper.ID).Scan(&needs, &state); err != nil || needs != api.ObligationNeedsDelivery || state != api.ObligationQueued {
				t.Fatalf("obligation %s %s %v", needs, state, err)
			}
			if steps := f.sweep(c.first.Add(40 * time.Second)); len(steps) != 0 {
				t.Fatalf("second sweep %+v", steps)
			}
			f.reopen()
			if steps := f.sweep(c.first.Add(50 * time.Second)); len(steps) != 0 || len(f.notices(0)) != 1 {
				t.Fatalf("after a restart %+v", steps)
			}
			f.read(c.first.Add(time.Minute))
			if steps := f.sweep(c.second.Add(10 * time.Second)); len(steps) != 1 || steps[0].Action != ownerWakeActionPosted {
				t.Fatalf("next slot %+v", steps)
			}
			list := f.firings(w.ID)
			if len(list) != 2 || !list[0].DueAt.Equal(c.second) || !list[1].DueAt.Equal(c.first) || list[1].Outcome != api.OwnerHelperWakePosted || list[1].NoticeSeq != got[0].seq ||
				list[1].RecipientID != f.helper.ID || list[1].RecipientRunID != f.helper.RunID || list[1].PostedAt == nil || list[1].Report != "" {
				t.Fatalf("firings %+v", list)
			}
		})
	}
}

func sameRefs(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// a2: a cancelled or expired wake posts nothing.
func TestOwnerHelperWakeCancelledAndExpired(t *testing.T) {
	f := wakeStore(t, true)
	cancelled := f.create("wake-cancelled", "hourly-update", "14 7-20 * * *")
	if _, _, err := f.set(api.SetOwnerHelperWakeRequest{WakeID: cancelled.ID, ExpectedRevision: 1, State: api.OwnerHelperWakeCancelled, RequestID: "wake-cancel"}); err != nil {
		t.Fatal(err)
	}
	expires := pst(2, 7, 20, 0) // after the 07:08 slot, before 07:28
	expiring, _, err := f.set(api.SetOwnerHelperWakeRequest{Kind: "queue-check", Schedule: "8,28,48 * * * *", MessageSeq: f.msg, ExpiresAt: &expires, RequestID: "wake-expiring"})
	if err != nil {
		t.Fatal(err)
	}
	if steps := f.sweep(pst(2, 7, 8, 5)); len(steps) != 1 || steps[0].WakeID != expiring.ID {
		t.Fatalf("the slot before expiry %+v", steps)
	}
	f.read(pst(2, 7, 9, 0))
	if steps := f.sweep(pst(2, 7, 14, 5)); len(steps) != 0 {
		t.Fatalf("a cancelled wake posted %+v", steps)
	}
	if got := f.wake(expiring.ID); got.State != api.OwnerHelperWakeExpired || got.NextDueAt != nil {
		t.Fatalf("a wake whose next slot is past its expiry %+v", got)
	}
	if steps := f.sweep(pst(2, 7, 28, 5)); len(steps) != 0 {
		t.Fatalf("an expired wake posted %+v", steps)
	}
	if len(f.firings(cancelled.ID)) != 0 || len(f.firings(expiring.ID)) != 1 || len(f.notices(0)) != 1 {
		t.Fatalf("firings %+v %+v", f.firings(cancelled.ID), f.firings(expiring.ID))
	}
}

// a4: a catch-up records earlier slots missed, delivers the latest and posts
// one owner report.
func TestOwnerHelperWakeMissed(t *testing.T) {
	f := wakeStore(t, true)
	w := f.create("wake-hourly", "hourly-update", "14 7-20 * * *")
	steps := f.sweep(pst(2, 9, 15, 0)) // slots 07:14, 08:14 and 09:14
	if len(steps) != 2 || steps[0].Action != ownerWakeActionPosted || steps[1].Action != ownerWakeActionMissed {
		t.Fatalf("catch-up steps %+v", steps)
	}
	list := f.firings(w.ID)
	if len(list) != 3 || list[0].Outcome != api.OwnerHelperWakePosted || !list[0].DueAt.Equal(pst(2, 9, 14, 0)) || list[0].Report != "" ||
		list[1].Outcome != api.OwnerHelperWakeMissed || list[2].Outcome != api.OwnerHelperWakeMissed ||
		list[1].Report != api.OwnerHelperWakeReportMissed || list[1].ReportSeq != steps[1].MessageSeq || list[2].ReportSeq != steps[1].MessageSeq {
		t.Fatalf("firings %+v", list)
	}
	got := f.notices(0)
	if len(got) != 2 || got[1].to != "" {
		t.Fatalf("notices %+v", got)
	}
	report := got[1].env
	if !strings.Contains(report.Body.Text, "missed 2 earlier slot(s), due from "+pst(2, 7, 14, 0).Format(time.RFC3339)+" to "+pst(2, 8, 14, 0).Format(time.RFC3339)) ||
		!strings.Contains(report.Body.Text, "was posted to the owner helper as message #") ||
		report.Refs["recipientKind"] != "owner" || report.Refs["escalation"] != "owner" || report.Refs["wake"] != w.ID || report.Refs["ownerWake"] != "hourly-update" {
		t.Fatalf("missed report %+v", report)
	}
	if steps := f.sweep(pst(2, 9, 16, 0)); len(steps) != 0 {
		t.Fatalf("a second sweep %+v", steps)
	}
	if got := f.wake(w.ID); !got.NextDueAt.Equal(pst(2, 10, 14, 0)) {
		t.Fatalf("next due %s", got.NextDueAt)
	}
}

// a4: more than 48 skipped slots store 48 rows and say "at least"; a late
// latest slot is named in the same report.
func TestOwnerHelperWakeMissedCap(t *testing.T) {
	f := wakeStore(t, true)
	w := f.create("wake-minutely", "other", "* * * * *")
	steps := f.sweep(f.now.Add(100*time.Minute + 6*time.Minute)) // 105 earlier slots; the latest is under a minute late
	if len(steps) != 2 {
		t.Fatalf("steps %+v", steps)
	}
	list := f.firings(w.ID)
	missed := 0
	for _, x := range list {
		if x.Outcome == api.OwnerHelperWakeMissed {
			missed++
		}
	}
	if len(list) != api.OwnerHelperWakeMissedRows+1 || missed != api.OwnerHelperWakeMissedRows {
		t.Fatalf("%d rows, %d missed", len(list), missed)
	}
	report := f.notices(0)[1].env
	if !strings.Contains(report.Body.Text, "at least 105 earlier slots (the firing list keeps the latest 48)") {
		t.Fatalf("report %q", report.Body.Text)
	}
	// A catch-up whose latest slot is posted late names the lateness once.
	g := wakeStore(t, true)
	h := g.create("wake-hourly", "hourly-update", "14 7-20 * * *")
	steps = g.sweep(pst(2, 8, 24, 0)) // 07:14 missed, 08:14 posted 10 min late
	if len(steps) != 2 || steps[1].Action != ownerWakeActionMissed {
		t.Fatalf("late catch-up steps %+v", steps)
	}
	hl := g.firings(h.ID)
	if hl[0].Report != api.OwnerHelperWakeReportLate || hl[0].ReportSeq != steps[1].MessageSeq || hl[0].LateMinutes != 10 || len(g.notices(0)) != 2 ||
		!strings.Contains(g.notices(0)[1].env.Body.Text, "posted to the owner helper 10 min late") {
		t.Fatalf("late latest slot %+v %q", hl, g.notices(0)[1].env.Body.Text)
	}
}

// a4: no helper, or a retired, exited or closed one: helper-absent with one
// owner report.
func TestOwnerHelperWakeHelperAbsent(t *testing.T) {
	for _, state := range []string{"none", api.AgentRetired, api.AgentExited, api.AgentClosed} {
		t.Run(state, func(t *testing.T) {
			f := wakeStore(t, state != "none")
			ctx := context.Background()
			switch state {
			case api.AgentRetired:
				setAgentStatus(t, f.s, f.helper.ID, api.AgentRetired)
			case api.AgentExited:
				if _, err := f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{Kind: api.EventExited, AgentID: f.helper.ID, RunID: f.helper.RunID}, wakeBy); err != nil {
					t.Fatal(err)
				}
			case api.AgentClosed:
				if _, err := f.s.CloseAgentRun(ctx, f.helper.ID, f.helper.RunID, wakeBy); err != nil {
					t.Fatal(err)
				}
			}
			w := f.create("wake-morning", "morning-summary", "0 7 * * *")
			steps := f.sweep(pst(2, 7, 0, 30))
			if len(steps) != 1 || steps[0].Action != ownerWakeActionAbsent {
				t.Fatalf("steps %+v", steps)
			}
			list := f.firings(w.ID)
			if len(list) != 1 || list[0].Outcome != api.OwnerHelperWakeHelperAbsent || list[0].Report != api.OwnerHelperWakeReportAbsent || list[0].ReportSeq != steps[0].MessageSeq || list[0].PostedAt != nil {
				t.Fatalf("firings %+v", list)
			}
			n := f.notices(0)
			want := "the owner helper is " + map[string]string{"none": "not registered", api.AgentClosed: "not registered"}[state]
			if state == api.AgentRetired || state == api.AgentExited {
				want = "the owner helper is " + state
			}
			if len(n) != 1 || n[0].to != "" || n[0].env.Refs["recipientKind"] != "owner" || !strings.Contains(n[0].env.Body.Text, want) {
				t.Fatalf("report %+v; want %q", n, want)
			}
			if steps := f.sweep(pst(2, 7, 1, 0)); len(steps) != 0 {
				t.Fatalf("again %+v", steps)
			}
		})
	}
}

// a4: a paused project records skipped-paused, with no notice and no report.
func TestOwnerHelperWakePaused(t *testing.T) {
	f := wakeStore(t, true)
	w := f.create("wake-hourly", "hourly-update", "14 7-20 * * *")
	f.s.db.Exec(`UPDATE tasks SET pause_state='paused' WHERE id=?`, f.task.ID)
	if steps := f.sweep(pst(2, 7, 14, 5)); len(steps) != 0 {
		t.Fatalf("paused slot %+v", steps)
	}
	if steps := f.sweep(pst(2, 10, 20, 0)); len(steps) != 0 { // a paused catch-up
		t.Fatalf("paused catch-up %+v", steps)
	}
	list := f.firings(w.ID)
	if len(list) != 4 || len(f.notices(0)) != 0 {
		t.Fatalf("firings %+v", list)
	}
	for _, x := range list {
		if x.Outcome != api.OwnerHelperWakeSkippedPaused || x.Report != "" || x.ReportSeq != 0 {
			t.Fatalf("paused firing %+v", x)
		}
	}
	f.s.db.Exec(`UPDATE tasks SET pause_state='active' WHERE id=?`, f.task.ID)
	if steps := f.sweep(pst(2, 11, 14, 5)); len(steps) != 1 || steps[0].Action != ownerWakeActionPosted {
		t.Fatalf("after the pause %+v", steps)
	}
}

// a5: posted late, read late, unread, and read on time.
func TestOwnerHelperWakeLateAndUnread(t *testing.T) {
	t.Run("posted late", func(t *testing.T) {
		f := wakeStore(t, true)
		w := f.create("wake", "hourly-update", "14 7-20 * * *")
		steps := f.sweep(pst(2, 7, 20, 30))
		if len(steps) != 2 || steps[0].Action != ownerWakeActionPosted || steps[1].Action != ownerWakeActionLate {
			t.Fatalf("steps %+v", steps)
		}
		x := f.firings(w.ID)[0]
		if x.Report != api.OwnerHelperWakeReportLate || x.ReportSeq != steps[1].MessageSeq || x.LateMinutes != 6 {
			t.Fatalf("firing %+v", x)
		}
		if n := f.notices(0); len(n) != 2 || !strings.Contains(n[1].env.Body.Text, "posted to the owner helper 6 min late") || n[1].env.Refs["escalation"] != "owner" {
			t.Fatalf("late report %+v", n)
		}
		// Read later, it gets no second report, across a restart too.
		f.reopen()
		f.read(pst(2, 7, 21, 0))
		if steps := f.sweep(pst(2, 7, 21, 10)); len(steps) != 0 {
			t.Fatalf("second report %+v", steps)
		}
		if x := f.firings(w.ID)[0]; x.DeliveredAt == nil || x.LateMinutes != 7 || x.Report != api.OwnerHelperWakeReportLate {
			t.Fatalf("read recorded %+v", x)
		}
	})
	t.Run("read late", func(t *testing.T) {
		f := wakeStore(t, true)
		w := f.create("wake", "hourly-update", "14 7-20 * * *")
		f.sweep(pst(2, 7, 14, 10))
		f.read(pst(2, 7, 20, 30))
		steps := f.sweep(pst(2, 7, 21, 0))
		if len(steps) != 1 || steps[0].Action != ownerWakeActionLate {
			t.Fatalf("steps %+v", steps)
		}
		x := f.firings(w.ID)[0]
		if x.DeliveredAt == nil || !x.DeliveredAt.Equal(pst(2, 7, 20, 30)) || x.LateMinutes != 6 || x.Report != api.OwnerHelperWakeReportLate || x.ReportSeq != steps[0].MessageSeq {
			t.Fatalf("firing %+v", x)
		}
		if n := f.notices(0); len(n) != 2 || !strings.Contains(n[1].env.Body.Text, "the owner helper read it 6 min late") {
			t.Fatalf("report %+v", n)
		}
		f.reopen()
		if steps := f.sweep(pst(2, 7, 22, 0)); len(steps) != 0 {
			t.Fatalf("again after a restart %+v", steps)
		}
	})
	t.Run("unread", func(t *testing.T) {
		f := wakeStore(t, true)
		w := f.create("wake", "hourly-update", "14 7-20 * * *")
		f.sweep(pst(2, 7, 14, 10))
		if steps := f.sweep(pst(2, 7, 28, 59)); len(steps) != 0 {
			t.Fatalf("reported before 15 min %+v", steps)
		}
		steps := f.sweep(pst(2, 7, 29, 0))
		if len(steps) != 1 || steps[0].Action != ownerWakeActionUndelivered {
			t.Fatalf("steps %+v", steps)
		}
		if n := f.notices(0); len(n) != 2 || !strings.Contains(n[1].env.Body.Text, "is still unread after 15 min") {
			t.Fatalf("report %+v", n)
		}
		f.reopen()
		if steps := f.sweep(pst(2, 7, 30, 0)); len(steps) != 0 {
			t.Fatalf("again %+v", steps)
		}
		f.read(pst(2, 7, 40, 0))
		if steps := f.sweep(pst(2, 7, 40, 10)); len(steps) != 0 {
			t.Fatalf("read after the report %+v", steps)
		}
		x := f.firings(w.ID)[0]
		if x.DeliveredAt == nil || !x.DeliveredAt.Equal(pst(2, 7, 40, 0)) || x.Report != api.OwnerHelperWakeReportUndelivered || x.LateMinutes != 26 || len(f.notices(0)) != 2 {
			t.Fatalf("firing %+v", x)
		}
	})
	t.Run("on time", func(t *testing.T) {
		f := wakeStore(t, true)
		w := f.create("wake", "hourly-update", "14 7-20 * * *")
		f.sweep(pst(2, 7, 14, 10))
		f.read(pst(2, 7, 16, 0))
		if steps := f.sweep(pst(2, 7, 30, 0)); len(steps) != 0 {
			t.Fatalf("on-time read reported %+v", steps)
		}
		if x := f.firings(w.ID)[0]; x.DeliveredAt == nil || x.Report != "" || x.LateMinutes != 2 {
			t.Fatalf("firing %+v", x)
		}
	})
}

// a5: reports ping the owner by day and are a plain line between 22:00 and
// 07:00 Pacific.
func TestOwnerHelperWakeNightEscalation(t *testing.T) {
	for _, c := range []struct {
		schedule, escalation string
		due                  time.Time
	}{
		{"0 23 * * *", "lead", pst(2, 23, 0, 0)},
		{"30 6 * * *", "lead", pst(3, 6, 30, 0)},
		{"0 7 * * *", "owner", pst(2, 7, 0, 0)},
		{"0 21 * * *", "owner", pst(2, 21, 0, 0)},
	} {
		f := wakeStore(t, true)
		f.create("wake", "other", c.schedule)
		if steps := f.sweep(c.due.Add(6 * time.Minute)); len(steps) != 2 || steps[1].Action != ownerWakeActionLate {
			t.Fatalf("%s steps %+v", c.schedule, steps)
		}
		if n := f.notices(0); n[1].env.Refs["escalation"] != c.escalation || n[1].env.Refs["recipientKind"] != "owner" || n[0].env.Refs["escalation"] != "" {
			t.Fatalf("%s refs %v %v", c.schedule, n[0].env.Refs, n[1].env.Refs)
		}
	}
}

// A wake that stays in the same trouble is reported once per episode: later
// slots record the report with no report message until one goes cleanly.
func TestOwnerHelperWakeReportFold(t *testing.T) {
	f := wakeStore(t, true)
	w := f.create("wake", "hourly-update", "14 7-20 * * *")
	f.sweep(pst(2, 7, 14, 10))
	if steps := f.sweep(pst(2, 7, 29, 0)); len(steps) != 1 || steps[0].Action != ownerWakeActionUndelivered {
		t.Fatalf("first unread %+v", steps)
	}
	f.sweep(pst(2, 8, 14, 10))
	if steps := f.sweep(pst(2, 8, 29, 0)); len(steps) != 0 {
		t.Fatalf("second unread was reported again %+v", steps)
	}
	if x := f.firings(w.ID)[0]; x.Report != api.OwnerHelperWakeReportUndelivered || x.ReportSeq != 0 {
		t.Fatalf("folded firing %+v", x)
	}
	f.read(pst(2, 8, 40, 0))
	f.sweep(pst(2, 9, 14, 10))
	f.read(pst(2, 9, 15, 0))
	f.sweep(pst(2, 9, 16, 0)) // 09:14 went through cleanly
	f.sweep(pst(2, 10, 14, 10))
	if steps := f.sweep(pst(2, 10, 29, 0)); len(steps) != 1 || steps[0].Action != ownerWakeActionUndelivered {
		t.Fatalf("a new episode %+v", steps)
	}
	// A different trouble is not folded into the previous one.
	f.read(pst(2, 10, 40, 0))
	if steps := f.sweep(pst(2, 11, 20, 0)); len(steps) != 2 || steps[1].Action != ownerWakeActionLate {
		t.Fatalf("a late slot after an unread one %+v", steps)
	}
}

// R4: with nothing due and nothing to settle, a sweep writes nothing.
func TestOwnerHelperWakeQuietSweep(t *testing.T) {
	f := wakeStore(t, true)
	w := f.create("wake", "hourly-update", "14 7-20 * * *")
	f.sweep(pst(2, 7, 14, 10))
	f.read(pst(2, 7, 15, 0))
	f.sweep(pst(2, 7, 15, 10))
	var before, after int64
	changes := func() int64 {
		var n int64
		if err := f.s.db.QueryRow(`SELECT (SELECT count(*) FROM messages)+(SELECT count(*) FROM owner_helper_wake_firings)+(SELECT COALESCE(SUM(length(delivered_at)+length(report)),0) FROM owner_helper_wake_firings)+(SELECT COALESCE(SUM(length(next_due_at)+revision),0) FROM owner_helper_wakes)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before = changes()
	for _, at := range []time.Time{pst(2, 7, 30, 0), pst(2, 7, 45, 0), pst(2, 8, 13, 59)} {
		if steps := f.sweep(at); len(steps) != 0 {
			t.Fatalf("quiet sweep %+v", steps)
		}
	}
	if after = changes(); after != before || !f.wake(w.ID).NextDueAt.Equal(pst(2, 8, 14, 0)) {
		t.Fatalf("a quiet sweep wrote: %d -> %d", before, after)
	}
}

// R1: concurrent sweeps post each slot once.
func TestOwnerHelperWakeConcurrentSweeps(t *testing.T) {
	f := wakeStore(t, true)
	w := f.create("wake", "queue-check", "8,28,48 * * * *")
	at := pst(2, 7, 8, 10)
	f.now = at
	var wg sync.WaitGroup
	var mu sync.Mutex
	var all []OwnerHelperWakeStep
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			steps, err := f.s.OwnerHelperWakeSweep(context.Background(), at)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			all = append(all, steps...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(all) != 1 || len(f.notices(0)) != 1 || len(f.firings(w.ID)) != 1 {
		t.Fatalf("concurrent sweeps %+v", all)
	}
}

// a6: the list carries each wake's next due and its last firings, newest
// first, with lateness.
func TestOwnerHelperWakeList(t *testing.T) {
	f := wakeStore(t, true)
	ctx := context.Background()
	w := f.create("wake", "queue-check", "8,28,48 * * * *")
	for i, at := range []time.Time{pst(2, 7, 8, 10), pst(2, 7, 28, 10), pst(2, 7, 48, 10), pst(2, 8, 8, 10), pst(2, 8, 28, 10), pst(2, 8, 48, 10)} {
		f.sweep(at)
		if i < 5 {
			f.read(at.Add(time.Duration(i) * time.Minute))
		}
	}
	list, err := f.s.ListOwnerHelperWakes(ctx, f.task.ID, 0)
	if err != nil || len(list.Wakes) != 1 || list.TaskID != f.task.ID {
		t.Fatalf("list %+v %v", list, err)
	}
	got := list.Wakes[0]
	if got.ID != w.ID || !got.NextDueAt.Equal(pst(2, 9, 8, 0)) || len(got.Firings) != api.DefaultOwnerHelperWakeFirings || !got.Firings[0].DueAt.Equal(pst(2, 8, 48, 0)) || got.Firings[0].DeliveredAt != nil ||
		got.Firings[1].LateMinutes != 4 || got.Firings[1].DeliveredAt == nil || !got.Firings[4].DueAt.Equal(pst(2, 7, 28, 0)) {
		t.Fatalf("wake %+v", got)
	}
	if all, err := f.s.ListOwnerHelperWakes(ctx, f.task.ID, api.MaxOwnerHelperWakeFirings); err != nil || len(all.Wakes[0].Firings) != 6 {
		t.Fatalf("max firings %+v %v", all, err)
	}
	for _, n := range []int{-1, api.MaxOwnerHelperWakeFirings + 1} {
		if _, err := f.s.ListOwnerHelperWakes(ctx, f.task.ID, n); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("firings %d: %v", n, err)
		}
	}
	if _, err := f.s.ListOwnerHelperWakes(ctx, api.NewID("tsk"), 0); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

// a7: a store without the wake tables migrates, migrates again with no
// change, and keeps its existing rows.
func TestOwnerHelperWakeMigration(t *testing.T) {
	f := wakeStore(t, true)
	ctx := context.Background()
	if err := migrateOwnerHelperWakes(f.s.db); err != nil {
		t.Fatalf("second run on an opened store: %v", err)
	}
	if err := migrate(f.s.db); err != nil {
		t.Fatalf("the whole migration again: %v", err)
	}
	if _, err := saveContinuity(f.s, f.task.ID, "cont-1", 0, "before the drop", api.Agent{}); err != nil {
		t.Fatal(err)
	}
	w := f.create("wake-before", "hourly-update", "14 7-20 * * *")
	f.sweep(pst(2, 7, 14, 10))
	schema := func(s *Store) string {
		rows, err := s.db.Query(`SELECT type||' '||name||' '||COALESCE(sql,'') FROM sqlite_master WHERE name LIKE 'owner_helper_wake%' ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var line string
			rows.Scan(&line)
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
	want := schema(f.s)
	if strings.Count(want, "CREATE TABLE") != 3 || strings.Count(want, "CREATE INDEX") != 2 {
		t.Fatalf("schema %s", want)
	}
	// Reopening an up-to-date store changes nothing and keeps the rows.
	f.reopen()
	if got := schema(f.s); got != want {
		t.Fatalf("schema changed on reopen:\n%s\nwant\n%s", got, want)
	}
	if list := f.firings(w.ID); len(list) != 1 || f.wake(w.ID).Revision != 1 {
		t.Fatalf("rows after reopen %+v", list)
	}
	// A store from before this change has none of the tables.
	for _, stmt := range []string{`DROP TABLE owner_helper_wake_firings`, `DROP TABLE owner_helper_wake_requests`, `DROP TABLE owner_helper_wakes`} {
		if _, err := f.s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	f.reopen()
	if got := schema(f.s); got != want {
		t.Fatalf("schema after migrating an old store:\n%s\nwant\n%s", got, want)
	}
	if got, err := f.s.OwnerHelperContinuity(ctx, f.task.ID, 0); err != nil || got.Body != "before the drop" {
		t.Fatalf("continuity after migration %+v %v", got, err)
	}
	if got, err := f.s.GetTask(ctx, f.task.ID); err != nil || got.Name != f.task.Name {
		t.Fatalf("task after migration %+v %v", got, err)
	}
	if list, err := f.s.ListOwnerHelperWakes(ctx, f.task.ID, 0); err != nil || len(list.Wakes) != 0 {
		t.Fatalf("regained tables are empty: %+v %v", list, err)
	}
	f.create("wake-after", "hourly-update", "14 7-20 * * *")
	f.reopen()
	if list, err := f.s.ListOwnerHelperWakes(ctx, f.task.ID, 0); err != nil || len(list.Wakes) != 1 {
		t.Fatalf("a third open lost rows: %+v %v", list, err)
	}
}

// b1 (review round one): a late or undelivered episode ends once the earlier
// slot was read, so a repeat is reported again; helper-absent still folds.
func TestOwnerHelperWakeReportFoldEnds(t *testing.T) {
	t.Run("late two days running", func(t *testing.T) {
		f := wakeStore(t, true)
		w := f.create("wake", "morning-summary", "0 7 * * *")
		for _, day := range []int{2, 3} {
			f.sweep(pst(day, 7, 0, 10))
			f.read(pst(day, 7, 12, 0))
			steps := f.sweep(pst(day, 7, 12, 30))
			if len(steps) != 1 || steps[0].Action != ownerWakeActionLate {
				t.Fatalf("11-0%d late read %+v", day, steps)
			}
		}
		list := f.firings(w.ID)
		if len(list) != 2 || list[0].ReportSeq == 0 || list[1].ReportSeq == 0 || list[0].ReportSeq == list[1].ReportSeq {
			t.Fatalf("firings %+v", list)
		}
	})
	t.Run("unread after a slot read late", func(t *testing.T) {
		f := wakeStore(t, true)
		w := f.create("wake", "hourly-update", "14 7-20 * * *")
		f.sweep(pst(2, 7, 14, 10))
		if steps := f.sweep(pst(2, 7, 29, 0)); len(steps) != 1 || steps[0].Action != ownerWakeActionUndelivered {
			t.Fatalf("first unread %+v", steps)
		}
		f.read(pst(2, 7, 40, 0))
		f.sweep(pst(2, 8, 14, 10))
		steps := f.sweep(pst(2, 8, 30, 0))
		if len(steps) != 1 || steps[0].Action != ownerWakeActionUndelivered {
			t.Fatalf("an unread slot after a read one was folded %+v", steps)
		}
		if x := f.firings(w.ID)[0]; x.Report != api.OwnerHelperWakeReportUndelivered || x.ReportSeq != steps[0].MessageSeq {
			t.Fatalf("firing %+v", x)
		}
	})
	t.Run("helper absent folds", func(t *testing.T) {
		f := wakeStore(t, false)
		w := f.create("wake", "hourly-update", "14 7-20 * * *")
		if steps := f.sweep(pst(2, 7, 14, 10)); len(steps) != 1 || steps[0].Action != ownerWakeActionAbsent {
			t.Fatalf("first absent %+v", steps)
		}
		if steps := f.sweep(pst(2, 8, 14, 10)); len(steps) != 0 {
			t.Fatalf("second absent was reported again %+v", steps)
		}
		if x := f.firings(w.ID)[0]; x.Report != api.OwnerHelperWakeReportAbsent || x.ReportSeq != 0 {
			t.Fatalf("folded absent firing %+v", x)
		}
		f.helper = *registerHelper(t, f.s, f.task.ID, helperRequest("wake-reg-late"), wakeBy).Agent
		if steps := f.sweep(pst(2, 9, 14, 10)); len(steps) != 1 || steps[0].Action != ownerWakeActionPosted {
			t.Fatalf("after the helper registered %+v", steps)
		}
	})
}

// f1 (review round one): a change after the clock stepped back past a fired
// slot does not target that slot again, and later slots still fire.
func TestOwnerHelperWakeClockStepBack(t *testing.T) {
	f := wakeStore(t, true)
	w := f.create("wake", "hourly-update", "14 7-20 * * *")
	if steps := f.sweep(pst(2, 7, 14, 10)); len(steps) != 1 {
		t.Fatalf("first slot %+v", steps)
	}
	f.read(pst(2, 7, 15, 0))
	f.now = pst(2, 7, 13, 59) // the clock steps back past the fired slot
	changed, _, err := f.set(api.SetOwnerHelperWakeRequest{WakeID: w.ID, ExpectedRevision: 1, Kind: "hourly-update", Schedule: "14,44 * * * *", MessageSeq: f.msg, RequestID: "wake-step-back"})
	if err != nil || !changed.NextDueAt.Equal(pst(2, 7, 44, 0)) {
		t.Fatalf("change after a step back %+v %v", changed, err)
	}
	for _, at := range []time.Time{pst(2, 7, 14, 20), pst(2, 7, 14, 50)} {
		if steps := f.sweep(at); len(steps) != 0 {
			t.Fatalf("sweep at %s %+v", at, steps)
		}
	}
	steps := f.sweep(pst(2, 7, 44, 10))
	if len(steps) != 1 || steps[0].Action != ownerWakeActionPosted {
		t.Fatalf("the later slot %+v", steps)
	}
	if list := f.firings(w.ID); len(list) != 2 || !list[0].DueAt.Equal(pst(2, 7, 44, 0)) {
		t.Fatalf("firings %+v", list)
	}
}
