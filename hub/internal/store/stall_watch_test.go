package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// stallNoon is 12:00 Pacific on a weekday: owner pings are allowed.
var stallNoon = time.Date(2026, 10, 12, 19, 0, 0, 0, time.UTC)

// stallFixture is a synthetic project on a controllable clock with two
// queued items. Agents listed in online heartbeat whenever the clock moves.
type stallFixture struct {
	t      *testing.T
	s      *Store
	task   api.Task
	items  []api.WorkItem
	orders []api.Message
	clk    *time.Time // shared by the projects of one store
	online map[string]bool
	n      int
}

var stallOwner = api.Caller{Node: "fixture", User: "owner"}

func newStallFixture(t *testing.T, start time.Time) *stallFixture {
	t.Helper()
	s, task, items, orders := queueFixture(t)
	f := &stallFixture{t: t, s: s, task: task, items: items, orders: orders, clk: &start, online: map[string]bool{}}
	s.now = f.now
	t.Cleanup(func() { stallWatchBeforeWrite = nil })
	return f
}

func (f *stallFixture) now() time.Time { return *f.clk }

// project adds a fresh project to the same store for one subtest and closes
// it when the subtest ends, so later sweeps post nothing for it. Opening a
// store per case would make the table tests slow under the race detector.
func (f *stallFixture) project(t *testing.T, start time.Time) *stallFixture {
	t.Helper()
	ctx := context.Background()
	f.n++
	task, err := f.s.CreateTask(ctx, api.CreateTaskRequest{Name: fmt.Sprintf("Stall fixture %d", f.n)}, stallOwner)
	if err != nil {
		t.Fatal(err)
	}
	p := &stallFixture{t: t, s: f.s, task: task, clk: f.clk, online: map[string]bool{}, n: f.n * 1000}
	*p.clk = start
	for i := 0; i < 2; i++ {
		item, err := f.s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Queued work", Priority: "normal", RequestID: api.NewID("req")}, stallOwner)
		if err != nil {
			t.Fatal(err)
		}
		p.items = append(p.items, item)
		p.orders = append(p.orders, contextLinkedMessage(t, f.s, task, item, "bounded order", api.NewID("req"), nil))
	}
	t.Cleanup(func() {
		stallWatchBeforeWrite = nil
		if _, err := f.s.db.Exec(`UPDATE tasks SET status='closed' WHERE id=?`, task.ID); err != nil {
			t.Error(err)
		}
	})
	return p
}

func (f *stallFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.s.db.Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *stallFixture) beat() {
	f.t.Helper()
	for id := range f.online {
		f.exec(`UPDATE agents SET last_seen_at=? WHERE id=?`, ts(f.now()), id)
	}
}

func (f *stallFixture) at(now time.Time) {
	f.t.Helper()
	*f.clk = now
	f.beat()
}

func (f *stallFixture) advance(d time.Duration) { f.t.Helper(); f.at(f.now().Add(d)) }

// helper registers the project's owner helper, online.
func (f *stallFixture) helper() api.Agent {
	f.t.Helper()
	f.n++
	out := registerHelper(f.t, f.s, f.task.ID, helperRequest(fmt.Sprintf("stall-helper-%d", f.n)), stallOwner)
	f.online[out.Agent.ID] = true
	f.beat()
	return *out.Agent
}

// owe posts a directed message from the owner, which the helper then owes.
func (f *stallFixture) owe(h api.Agent) api.Message {
	f.t.Helper()
	m, err := f.s.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{To: h.ID, Text: "Owner asks the helper for a status"}, stallOwner)
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

// post is a board post by the agent's exact run; replyTo passes the ack gate
// for an agent that owes an answer.
func (f *stallFixture) post(a api.Agent, replyTo int64) api.Message {
	f.t.Helper()
	m, err := f.s.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{AgentID: a.ID, RunID: a.RunID, Text: "Progress note from " + a.Name, ReplyTo: replyTo}, api.Caller{Node: "workspace", User: "owner"})
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *stallFixture) report(a api.Agent, activity api.AgentActivity) {
	f.t.Helper()
	f.n++
	if activity.ObservedAt.IsZero() {
		activity.ObservedAt = f.now()
	}
	if _, err := f.s.ReportActivity(context.Background(), f.task.ID, a.ID, api.ActivityReport{RequestID: fmt.Sprintf("stall-report-%d", f.n), RunID: a.RunID, Activity: activity}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *stallFixture) state(a api.Agent, state string) {
	f.t.Helper()
	f.report(a, api.AgentActivity{State: state})
}

// entry adds a running queue entry for item i, admitted now.
func (f *stallFixture) entry(i int) string {
	f.t.Helper()
	id := fmt.Sprintf("tqe_stall%d_%s", i, f.task.ID[len(f.task.ID)-6:])
	f.exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,created_at,updated_at,admitted_at) VALUES(?,?,?,1,?,'planned',?,'running',?,?,?)`,
		id, f.task.ID, f.items[i].ID, f.orders[i].Seq, i+1, ts(f.now().Add(-time.Hour)), ts(f.now()), ts(f.now()))
	return id
}

// member adds a running agent bound to item i now, online.
func (f *stallFixture) member(name string, i int) api.Agent {
	f.t.Helper()
	a, err := f.s.AddAgent(context.Background(), f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "mini", Session: name, Runtime: "codex", Cwd: f.t.TempDir()}, stallOwner)
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(`UPDATE agents SET status='running' WHERE id=?`, a.ID)
	f.exec(`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,?,0,?,?,?)`,
		a.ID, a.RunID, f.task.ID, f.items[i].ID, f.task.ID, f.orders[i].Seq, "digest", []byte("{}"), ts(f.now()))
	f.online[a.ID] = true
	f.beat()
	return a
}

func (f *stallFixture) sweep() []StallWatchStep {
	f.t.Helper()
	steps, err := f.s.StallWatchSweep(context.Background(), f.now())
	if err != nil {
		f.t.Fatal(err)
	}
	return steps
}

// one runs a sweep that must post exactly one notice and returns it.
func (f *stallFixture) one(action string) (StallWatchStep, api.Envelope, string) {
	f.t.Helper()
	steps := f.sweep()
	if len(steps) != 1 || steps[0].Action != action || steps[0].TaskID != f.task.ID {
		f.t.Fatalf("steps = %+v, want one %s", steps, action)
	}
	env, to := f.notice(steps[0].MessageSeq)
	return steps[0], env, to
}

// none runs a sweep that must post nothing.
func (f *stallFixture) none(what string) {
	f.t.Helper()
	const hub = `SELECT count(*) FROM messages WHERE from_node='hub-broker'`
	before := f.count(hub)
	if steps := f.sweep(); len(steps) != 0 {
		f.t.Fatalf("%s: steps = %+v, want none", what, steps)
	}
	if after := f.count(hub); after != before {
		f.t.Fatalf("%s: %d messages posted, want none", what, after-before)
	}
}

func (f *stallFixture) notice(seq int64) (api.Envelope, string) {
	f.t.Helper()
	var raw, to, node string
	if err := f.s.db.QueryRow(`SELECT envelope,to_agent,from_node FROM messages WHERE seq=?`, seq).Scan(&raw, &to, &node); err != nil {
		f.t.Fatal(err)
	}
	var env api.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil || node != api.BrokerNode || env.Kind != api.EnvelopeKindNotice {
		f.t.Fatalf("message %d is not a hub notice: %v %s %s", seq, err, node, raw)
	}
	if env.Refs["obligation"] != "" {
		f.t.Fatalf("notice %d carries an obligation ref", seq)
	}
	return env, to
}

func (f *stallFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// episodes counts the project's episodes, open or closed.
func (f *stallFixture) episodes() int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM stall_watch_episodes WHERE task_id=?`, f.task.ID)
}

func (f *stallFixture) open() int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM stall_watch_episodes WHERE closed_at='' AND task_id=?`, f.task.ID)
}

// closed asserts that no episode is open and the newest one closed for reason.
func (f *stallFixture) closed(reason string) {
	f.t.Helper()
	var got string
	if err := f.s.db.QueryRow(`SELECT close_reason FROM stall_watch_episodes WHERE task_id=? ORDER BY closed_at DESC,opened_at DESC LIMIT 1`, f.task.ID).Scan(&got); err != nil {
		f.t.Fatal(err)
	}
	if f.open() != 0 || got != reason {
		f.t.Fatalf("open episodes %d, close reason %q, want 0 and %q", f.open(), got, reason)
	}
}

var stallActionWords = regexp.MustCompile(`(?i)\b(restart|resume|prompt|escape)\b`)

// checkStallText asserts the notice text rule: the cause is not established,
// the record comes first, and action words follow the condition only.
func checkStallText(t *testing.T, text string) {
	t.Helper()
	for _, part := range []string{stallUnknownCause, stallRecordFirst, stallNextStep} {
		if !strings.Contains(text, part) {
			t.Fatalf("text lacks %q: %s", part, text)
		}
	}
	condition := strings.Index(text, stallNextStep)
	if !(strings.Index(text, stallUnknownCause) < strings.Index(text, stallRecordFirst) && strings.Index(text, stallRecordFirst) < condition) {
		t.Fatalf("parts out of order: %s", text)
	}
	if loc := stallActionWords.FindStringIndex(text); loc != nil && loc[0] < condition {
		t.Fatalf("action word %q before the condition: %s", text[loc[0]:loc[1]], text)
	}
}

func TestStallWatchHelperOnePingPerEpisode(t *testing.T) {
	f := newStallFixture(t, stallNoon)
	h := f.helper()
	f.state(h, "idle")
	f.advance(time.Hour)
	f.none("nothing owed")
	if f.episodes() != 0 {
		t.Fatal("an episode opened with nothing owed")
	}
	owed := f.owe(h)
	f.advance(20*time.Minute - time.Second)
	f.none("19 m 59 s")
	f.advance(time.Second)
	step, env, to := f.one(stallActionHelper)
	if to != "" || env.Subject != stallHelperSubject || env.Refs["escalation"] != "owner" || env.Refs["recipientKind"] != "owner" || env.Refs["stallWatch"] != "helper" ||
		env.Refs["agent"] != h.ID || env.Refs["run"] != h.RunID || env.Refs["activity"] != "idle" || env.Refs["episode"] == "" {
		t.Fatalf("helper notice to %q refs %+v", to, env.Refs)
	}
	for _, want := range []string{"owner-helper on owner-host", "quiet for 20 min", "activity state is idle", "1 open item(s)", fmt.Sprintf("message #%d", owed.Seq)} {
		if !strings.Contains(env.Body.Text, want) {
			t.Fatalf("text lacks %q: %s", want, env.Body.Text)
		}
	}
	checkStallText(t, env.Body.Text)
	var recipient, state string
	var noticeSeq int64
	if err := f.s.db.QueryRow(`SELECT recipient,owner_state,notice_seq FROM stall_watch_episodes WHERE id=?`, env.Refs["episode"]).Scan(&recipient, &state, &noticeSeq); err != nil {
		t.Fatal(err)
	}
	if recipient != "owner" || state != "pinged" || noticeSeq != step.MessageSeq {
		t.Fatalf("episode %s %s %d", recipient, state, noticeSeq)
	}
	for i := 0; i < 5; i++ {
		f.advance(13 * time.Minute)
		f.none("episode open")
	}
	// An exact-run post ends it; a new episode needs a fresh 20 minutes.
	f.post(h, owed.Seq)
	f.advance(time.Minute)
	f.none("after the post")
	f.closed("posted")
	f.advance(19*time.Minute - time.Second)
	f.none("19 m 59 s after the post")
	f.advance(time.Second)
	_, second, _ := f.one(stallActionHelper)
	if second.Refs["episode"] == env.Refs["episode"] {
		t.Fatal("the second episode reused the first")
	}
	// A working report ends it too.
	f.state(h, "working")
	f.none("working")
	f.closed("working")
	f.state(h, "idle")
	f.advance(20*time.Minute - time.Second)
	f.none("19 m 59 s after working")
	f.advance(time.Second)
	f.one(stallActionHelper)
	// Nothing owed any more ends it.
	f.exec(`UPDATE obligations SET state=? WHERE message_seq=?`, api.ObligationClosed, owed.Seq)
	f.none("nothing owed")
	f.closed("nothing-owed")
}

// p1: quiet time rests on the durable progress row, not on transcript or
// report times.
func TestStallWatchHelperProgress(t *testing.T) {
	root := newStallFixture(t, stallNoon)
	t.Run("working then unknown with an old transcript", func(t *testing.T) {
		f := root.project(t, stallNoon)
		h := f.helper()
		f.owe(h)
		f.state(h, "working")
		f.advance(time.Hour)
		f.none("working")
		// The report that ends working carries a transcript time hours old.
		f.report(h, api.AgentActivity{State: "unknown", LastEventAt: f.now().Add(-5 * time.Hour)})
		f.advance(20*time.Minute - time.Second)
		f.none("19 m 59 s after working ended")
		f.advance(time.Second)
		_, env, _ := f.one(stallActionHelper)
		if env.Refs["activity"] != "unknown" || !strings.Contains(env.Body.Text, "quiet for 20 min") {
			t.Fatalf("notice %+v %s", env.Refs, env.Body.Text)
		}
	})
	t.Run("wake-only reports do not move quiet-since", func(t *testing.T) {
		f := root.project(t, stallNoon)
		h := f.helper()
		f.owe(h)
		f.state(h, "idle")
		for i := 0; i < 3; i++ {
			f.advance(6 * time.Minute)
			f.report(h, api.AgentActivity{State: "idle", Wake: &api.WakeOutcome{Status: "failed", Reason: "pane busy", At: f.now()}})
		}
		f.advance(2 * time.Minute)
		_, env, _ := f.one(stallActionHelper)
		if !strings.Contains(env.Body.Text, "quiet for 20 min") {
			t.Fatalf("text %s", env.Body.Text)
		}
	})
	t.Run("working to idle between two sweeps", func(t *testing.T) {
		f := root.project(t, stallNoon)
		h := f.helper()
		f.owe(h)
		f.state(h, "idle")
		f.advance(15 * time.Minute)
		f.none("15 m")
		f.state(h, "working")
		f.advance(time.Minute)
		f.state(h, "idle")
		f.advance(9 * time.Minute)
		f.none("the working turn restarted the quiet time")
		f.advance(11 * time.Minute)
		f.one(stallActionHelper)
	})
	t.Run("no progress row rests on the owed time", func(t *testing.T) {
		f := root.project(t, stallNoon)
		h := f.helper()
		f.state(h, "idle")
		f.exec(`DELETE FROM stall_watch_progress WHERE task_id=?`, f.task.ID)
		f.owe(h)
		f.advance(20 * time.Minute)
		f.one(stallActionHelper)
	})
}

func TestStallWatchHelperOffline(t *testing.T) {
	f := newStallFixture(t, stallNoon)
	h := f.helper()
	f.owe(h)
	f.state(h, "working")
	// The session goes away: the working row stays, the heartbeat stops.
	delete(f.online, h.ID)
	f.advance(20 * time.Minute)
	_, env, _ := f.one(stallActionHelper)
	if env.Refs["activity"] != "offline" || !strings.Contains(env.Body.Text, "activity state is offline") || !strings.Contains(env.Body.Text, "tt doctor") {
		t.Fatalf("notice %+v %s", env.Refs, env.Body.Text)
	}
	checkStallText(t, env.Body.Text)
	f.advance(time.Hour)
	f.none("still offline")
}

// p3: an episode is closed from its own cursor whatever the downtime.
func TestStallWatchHelperDowntime(t *testing.T) {
	f := newStallFixture(t, stallNoon)
	h := f.helper()
	f.state(h, "idle")
	owed := f.owe(h)
	f.advance(20 * time.Minute)
	_, first, _ := f.one(stallActionHelper)
	// The sweeps stop; the helper posts; 31 minutes pass.
	f.advance(10 * time.Minute)
	f.post(h, owed.Seq)
	f.advance(31 * time.Minute)
	_, second, _ := f.one(stallActionHelper)
	if second.Refs["episode"] == first.Refs["episode"] {
		t.Fatal("the old episode was reused")
	}
	var reason string
	if err := f.s.db.QueryRow(`SELECT close_reason FROM stall_watch_episodes WHERE id=?`, first.Refs["episode"]).Scan(&reason); err != nil || reason != "posted" {
		t.Fatalf("old episode close reason %q %v", reason, err)
	}
	if f.open() != 1 {
		t.Fatalf("open episodes %d", f.open())
	}
	f.none("one notice for the new episode")
}

func TestStallWatchProgressHook(t *testing.T) {
	f := newStallFixture(t, stallNoon)
	h := f.helper()
	row := func() string {
		return tableDump(t, f.s, `SELECT agent_id,run_id,task_id,last_working_at FROM stall_watch_progress`)
	}
	wait := func() string {
		return tableDump(t, f.s, `SELECT matrix_wait,wait_ended_at FROM stall_watch_progress`)
	}
	others := func() string {
		return tableDump(t, f.s, `SELECT count(*) FROM messages`) + tableDump(t, f.s, `SELECT * FROM stall_watch_episodes`) + tableDump(t, f.s, `SELECT count(*) FROM obligations`)
	}
	f.state(h, "idle")
	if row() != "" {
		t.Fatalf("idle wrote progress: %s", row())
	}
	base := others()
	f.advance(time.Minute)
	entered := f.now()
	f.state(h, "working")
	want := fmt.Sprintf("agent_id=%s|run_id=%s|task_id=%s|last_working_at=%s|\n", h.ID, h.RunID, f.task.ID, ts(entered))
	if row() != want {
		t.Fatalf("entering working: %s", row())
	}
	// A repeated working report changes nothing.
	f.advance(time.Minute)
	f.state(h, "working")
	if row() != want {
		t.Fatalf("repeated working: %s", row())
	}
	f.advance(time.Minute)
	left := f.now()
	f.state(h, "idle")
	want = strings.Replace(want, ts(entered), ts(left), 1)
	if row() != want {
		t.Fatalf("leaving working: %s", row())
	}
	// Wake-only, reason-only and other state changes write nothing.
	f.advance(time.Minute)
	f.report(h, api.AgentActivity{State: "idle", Wake: &api.WakeOutcome{Status: "confirmed", At: f.now()}})
	f.report(h, api.AgentActivity{State: "idle", Reason: "waiting"})
	f.state(h, "unknown")
	f.state(h, "stuck")
	if row() != want {
		t.Fatalf("other reports wrote progress: %s", row())
	}
	// A matrix wait is marked while reported; the first report without it
	// stores when it ended. Neither moves the last working time.
	if wait() != "matrix_wait=0|wait_ended_at=|\n" {
		t.Fatalf("no wait yet: %s", wait())
	}
	f.advance(time.Minute)
	f.report(h, stallMatrixWait(f.now()))
	if wait() != "matrix_wait=1|wait_ended_at=|\n" || row() != want {
		t.Fatalf("entering a matrix wait: %s %s", wait(), row())
	}
	f.advance(time.Minute)
	ended := f.now()
	f.state(h, "idle")
	f.advance(time.Minute)
	f.state(h, "unknown")
	if wait() != "matrix_wait=0|wait_ended_at="+ts(ended)+"|\n" || row() != want {
		t.Fatalf("leaving a matrix wait: %s %s", wait(), row())
	}
	if others() != base {
		t.Fatal("the hook wrote outside stall_watch_progress")
	}
}

func TestStallWatchNoticeText(t *testing.T) {
	now := stallNoon
	for _, state := range []string{"idle", "finished_silent", "stuck", "hung_tool", "looping", "crashed", "provider_blocked", "runtime_prompt", "needs_input", "unknown", "offline"} {
		for _, class := range []string{api.ProviderBlockAuth, api.ProviderBlockUsageLimit, api.ProviderBlockRateLimited, "other"} {
			for _, runtime := range []string{"claude", "codex"} {
				text := stallHelperText("owner-helper", "mini", state, "running", now.Add(-25*time.Minute), now, 2, 41, stallHelperNextStep(state, runtime, class, "mini"))
				checkStallText(t, text)
				checkStallText(t, text+stallNightLine)
				if !strings.Contains(text, "activity state is "+state) || !strings.Contains(text, "quiet for 25 min") {
					t.Fatalf("%s: %s", state, text)
				}
			}
		}
	}
	team := &stallTeam{entry: "tqe_1", item: "wi_1", members: []stallAgent{{name: "lead-1", status: "running"}}}
	for _, helper := range []string{"", "offline", "quiet too", "not registered"} {
		for _, night := range []bool{false, true} {
			n := team.notice("swe_1", now.Add(-95*time.Minute), now, helper, night, helper != "", 0)
			checkStallText(t, n.text)
			if !strings.Contains(n.text, "quiet for 1 h 35 min") || !strings.Contains(n.text, "lead-1 (offline)") {
				t.Fatalf("team text %s", n.text)
			}
		}
	}
	checkStallText(t, team.notice("swe_1", now.Add(-time.Hour), now, "", false, true, 7).text)
}

func TestStallWatchNightRule(t *testing.T) {
	for _, tc := range []struct {
		at    time.Time
		night bool
	}{
		{time.Date(2026, 10, 13, 4, 59, 59, 0, time.UTC), false}, // 21:59:59 PDT
		{time.Date(2026, 10, 13, 5, 0, 0, 0, time.UTC), true},    // 22:00 PDT
		{time.Date(2026, 10, 13, 13, 59, 59, 0, time.UTC), true}, // 06:59:59 PDT
		{time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC), false},  // 07:00 PDT
		// Each side of the change to daylight time on 2026-03-08: the same
		// UTC time of day is 06:30 PST before it and 07:30 PDT after it.
		{time.Date(2026, 3, 7, 14, 30, 0, 0, time.UTC), true},
		{time.Date(2026, 3, 8, 14, 30, 0, 0, time.UTC), false},
		// And of the change back on 2026-11-01: 22:30 PDT, then 21:30 PST.
		{time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), true},
		{time.Date(2026, 11, 2, 5, 30, 0, 0, time.UTC), false},
	} {
		if got := stallWatchNight(tc.at); got != tc.night {
			t.Errorf("night(%s) = %v, want %v", tc.at.Format(time.RFC3339), got, tc.night)
		}
	}
}

// stallElevenPM is 23:00 Pacific; stallSevenAM is the next 07:00 Pacific.
var (
	stallElevenPM = time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	stallSevenAM  = time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC)
)

func TestStallWatchNightHelper(t *testing.T) {
	root := newStallFixture(t, stallNoon)
	deferred := func(t *testing.T) (*stallFixture, api.Agent, api.Message, api.Envelope) {
		f := root.project(t, stallElevenPM.Add(-20*time.Minute))
		h := f.helper()
		f.state(h, "idle")
		owed := f.owe(h)
		f.at(stallElevenPM)
		_, env, to := f.one(stallActionHelper)
		if to != "" || env.Refs["escalation"] != "lead" || !strings.HasSuffix(env.Body.Text, stallNightLine) {
			t.Fatalf("night line refs %+v text %s", env.Refs, env.Body.Text)
		}
		checkStallText(t, env.Body.Text)
		if f.count(`SELECT count(*) FROM stall_watch_episodes WHERE owner_state='deferred' AND closed_at='' AND task_id=?`, f.task.ID) != 1 {
			t.Fatal("the episode is not deferred")
		}
		return f, h, owed, env
	}
	t.Run("one ping at 07:00", func(t *testing.T) {
		f, _, _, line := deferred(t)
		for at := stallElevenPM.Add(time.Hour); at.Before(stallSevenAM); at = at.Add(time.Hour) {
			f.at(at)
			f.none("night")
		}
		f.at(stallSevenAM.Add(-time.Second))
		f.none("06:59:59")
		f.at(stallSevenAM)
		step, env, to := f.one(stallActionHelperPing)
		if to != "" || env.Refs["escalation"] != "owner" || env.Refs["episode"] != line.Refs["episode"] || !strings.HasPrefix(env.Body.Text, "Still open since the night line #") {
			t.Fatalf("deferred ping refs %+v text %s", env.Refs, env.Body.Text)
		}
		checkStallText(t, env.Body.Text)
		if f.count(`SELECT count(*) FROM stall_watch_episodes WHERE owner_state='pinged' AND ping_seq=? AND closed_at='' AND task_id=?`, step.MessageSeq, f.task.ID) != 1 {
			t.Fatal("the ping was not recorded on the episode")
		}
		f.none("after the ping")
		f.advance(time.Hour)
		f.none("an hour after the ping")
	})
	t.Run("ended at 06:00 never pings", func(t *testing.T) {
		f, h, owed, _ := deferred(t)
		f.at(stallSevenAM.Add(-time.Hour))
		f.post(h, owed.Seq)
		f.none("06:00")
		f.closed("posted")
		// Not owing any more, so no new episode either.
		f.exec(`UPDATE obligations SET state=? WHERE message_seq=?`, api.ObligationClosed, owed.Seq)
		f.at(stallSevenAM)
		f.none("07:00")
	})
	t.Run("ended while the hub was down never pings", func(t *testing.T) {
		f, h, owed, _ := deferred(t)
		f.at(stallSevenAM.Add(-time.Hour))
		f.post(h, owed.Seq)
		f.exec(`UPDATE obligations SET state=? WHERE message_seq=?`, api.ObligationClosed, owed.Seq)
		f.at(stallSevenAM.Add(2 * time.Hour))
		f.none("first daytime sweep")
		f.closed("nothing-owed")
	})
	t.Run("a hub down at 07:00 pings on its first daytime sweep", func(t *testing.T) {
		f, _, _, _ := deferred(t)
		f.at(stallSevenAM.Add(3 * time.Hour))
		f.one(stallActionHelperPing)
		f.none("after the ping")
	})
	for name, change := range map[string]func(f *stallFixture, h api.Agent){
		"working":     func(f *stallFixture, h api.Agent) { f.state(h, "working") },
		"matrix wait": func(f *stallFixture, h api.Agent) { f.report(h, stallMatrixWait(f.now())) },
		"own owner decision": func(f *stallFixture, h api.Agent) {
			f.decision(h, int64(f.count(`SELECT max(message_seq) FROM obligations WHERE agent_id=?`, h.ID)))
		},
		"pause": func(f *stallFixture, h api.Agent) {
			f.exec(`UPDATE tasks SET pause_state='paused' WHERE id=?`, f.task.ID)
		},
	} {
		t.Run("never quiet at 07:00 closes without a ping: "+name, func(t *testing.T) {
			f, h, _, _ := deferred(t)
			f.at(stallSevenAM)
			change(f, h)
			f.none("07:00")
			if f.open() != 0 {
				t.Fatal("the episode stayed open")
			}
		})
	}
}

// answer records the owner's answer, now, to the project's newest decision
// request.
func (f *stallFixture) answer() {
	f.t.Helper()
	m, err := f.s.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{Text: "Owner answers the decision"}, stallOwner)
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO decision_answers(message_seq,task_id,request_seq,option_id,created_at) VALUES(?,?,(SELECT max(message_seq) FROM decision_requests WHERE task_id=?),'yes',?)`,
		m.Seq, f.task.ID, f.task.ID, ts(f.now()))
}

// pause and resume move the project as the pause handler's rows do.
func (f *stallFixture) pause() {
	f.t.Helper()
	f.exec(`UPDATE tasks SET pause_state='paused' WHERE id=?`, f.task.ID)
}

func (f *stallFixture) resume() {
	f.t.Helper()
	f.n++
	f.exec(`UPDATE tasks SET pause_state='active' WHERE id=?`, f.task.ID)
	f.exec(`INSERT INTO project_pause_cycles(id,task_id,pause_generation,lifecycle_generation,state,retained_handoff_digest,created_at,resumed_at) VALUES(?,?,?,1,'resumed','digest',?,?)`,
		api.NewID("ppc"), f.task.ID, f.n, ts(f.now()), ts(f.now()))
}

// beforeWrite sets the sweep's test hook for the rest of the test.
func (f *stallFixture) beforeWrite(fn func()) {
	f.t.Helper()
	stallWatchBeforeWrite = fn
	f.t.Cleanup(func() { stallWatchBeforeWrite = nil })
}

func stallMatrixWait(since time.Time) api.AgentActivity {
	return api.AgentActivity{State: "idle", MatrixWait: &api.MatrixWait{Role: "waiting", Position: 1, Length: 1, Since: since}}
}

// decision records an unanswered owner decision asked by the agent's run.
func (f *stallFixture) decision(a api.Agent, replyTo int64) {
	f.t.Helper()
	m := f.post(a, replyTo)
	f.exec(`INSERT INTO decision_requests(message_seq,task_id,question,options,recommended_option_id,recommendation_reason,created_at) VALUES(?,?,?,?,?,?,?)`,
		m.Seq, f.task.ID, "Ship the candidate?", "[]", "yes", "synthetic", ts(f.now()))
}

func TestStallWatchNightTeam(t *testing.T) {
	// No helper is registered, so the team notice is the owner's.
	f := newStallFixture(t, stallElevenPM.Add(-30*time.Minute))
	entry := f.entry(0)
	f.member("lead-a", 0)
	f.at(stallElevenPM)
	_, line, to := f.one(stallActionTeam)
	if to != "" || line.Subject != stallTeamOwnerSubject || line.Refs["escalation"] != "lead" || line.Refs["entry"] != entry || !strings.HasSuffix(line.Body.Text, stallNightLine) {
		t.Fatalf("team night line to %q refs %+v", to, line.Refs)
	}
	f.at(stallSevenAM.Add(-time.Second))
	f.none("06:59:59")
	f.at(stallSevenAM)
	_, ping, to := f.one(stallActionTeamPing)
	// The 07:00 ping says why the owner hears it, as the night line did.
	const why = "The owner helper is not registered, so this goes to the owner."
	if to != "" || ping.Refs["escalation"] != "owner" || ping.Refs["episode"] != line.Refs["episode"] || ping.Refs["item"] != f.items[0].ID ||
		!strings.Contains(line.Body.Text, why) || !strings.Contains(ping.Body.Text, why) {
		t.Fatalf("team deferred ping refs %+v", ping.Refs)
	}
	checkStallText(t, ping.Body.Text)
	f.none("after the ping")

	// A team notice directed to the helper is sent at any hour.
	g := newStallFixture(t, stallElevenPM.Add(-30*time.Minute))
	h := g.helper()
	g.state(h, "idle")
	g.entry(0)
	g.member("lead-a", 0)
	g.at(stallElevenPM)
	_, env, to := g.one(stallActionTeam)
	if to != h.ID || env.Refs["escalation"] != "" || strings.Contains(env.Body.Text, "07:00") {
		t.Fatalf("directed night notice to %q refs %+v", to, env.Refs)
	}
	g.at(stallSevenAM)
	g.none("no deferred ping for a helper notice")

	// A deferred team episode that ended in the night never pings.
	e := newStallFixture(t, stallElevenPM.Add(-30*time.Minute))
	e.entry(0)
	lead := e.member("lead-a", 0)
	e.at(stallElevenPM)
	e.one(stallActionTeam)
	e.at(stallSevenAM.Add(-15 * time.Minute))
	e.post(lead, 0)
	e.at(stallSevenAM)
	e.none("07:00")
	e.closed("posted")

	// Nor does one whose team is in a never-quiet case at 07:00.
	w := newStallFixture(t, stallElevenPM.Add(-30*time.Minute))
	waiting := w.entry(0)
	w.member("lead-a", 0)
	w.at(stallElevenPM)
	w.one(stallActionTeam)
	w.at(stallSevenAM)
	w.wait(waiting, "waiting")
	w.none("07:00 during a shared-path wait")
	w.closed("shared-path-wait")
}

func TestStallWatchTeamNotice(t *testing.T) {
	f := newStallFixture(t, stallNoon)
	h := f.helper()
	f.state(h, "idle")
	entry := f.entry(0)
	lead := f.member("lead-a", 0)
	builder := f.member("builder-a", 0)
	f.state(lead, "idle")
	f.advance(30*time.Minute - time.Second)
	f.none("29 m 59 s")
	f.advance(time.Second)
	_, env, to := f.one(stallActionTeam)
	if to != h.ID || env.To != h.Name || env.Subject != stallTeamSubject || env.Refs["stallWatch"] != "team" || env.Refs["entry"] != entry || env.Refs["item"] != f.items[0].ID ||
		env.Refs["escalation"] != "" || env.Refs["recipientKind"] != "" {
		t.Fatalf("team notice to %q refs %+v", to, env.Refs)
	}
	for _, want := range []string{"queue entry " + entry, "quiet for 30 min", "2 member(s)", "builder-a (unknown), lead-a (idle)"} {
		if !strings.Contains(env.Body.Text, want) {
			t.Fatalf("text lacks %q: %s", want, env.Body.Text)
		}
	}
	checkStallText(t, env.Body.Text)
	if f.count(`SELECT count(*) FROM stall_watch_episodes WHERE kind='team' AND subject_id=? AND recipient='helper' AND owner_state='' AND closed_at=''`, entry) != 1 {
		t.Fatal("team episode row")
	}
	for i := 0; i < 4; i++ {
		f.advance(17 * time.Minute)
		f.none("episode open")
	}
	// A member post ends it; a fresh 30 minutes opens the next.
	f.post(builder, 0)
	f.none("after the post")
	f.closed("posted")
	f.advance(30*time.Minute - time.Second)
	f.none("29 m 59 s after the post")
	f.advance(time.Second)
	f.one(stallActionTeam)
	// A working report ends it.
	f.state(lead, "working")
	f.none("working")
	f.closed("working")
	f.state(lead, "idle")
	f.advance(30 * time.Minute)
	f.one(stallActionTeam)
	// So does one made while the sweeps were stopped for more than 30 m.
	f.advance(5 * time.Minute)
	f.state(lead, "working")
	f.advance(time.Minute)
	f.state(lead, "idle")
	f.advance(29 * time.Minute)
	f.none("worked during the downtime")
	f.closed("working")
	f.advance(time.Minute)
	f.one(stallActionTeam)
	f.advance(5 * time.Minute)
	f.post(lead, 0)
	f.advance(45 * time.Minute)
	steps := f.sweep()
	if len(steps) != 1 || steps[0].Action != stallActionTeam || f.open() != 1 {
		t.Fatalf("post during a long downtime: steps %+v open %d", steps, f.open())
	}
	if f.count(`SELECT count(*) FROM stall_watch_episodes WHERE close_reason='posted'`) != 2 {
		t.Fatal("the old episode did not close on the post")
	}
}

func TestStallWatchTeamRecipient(t *testing.T) {
	root := newStallFixture(t, stallNoon)
	team := func(f *stallFixture) {
		f.entry(0)
		f.member("lead-a", 0)
	}
	owner := func(t *testing.T, f *stallFixture, state string) {
		t.Helper()
		steps := f.sweep()
		var found bool
		for _, step := range steps {
			if step.Action != stallActionTeam {
				continue
			}
			found = true
			env, to := f.notice(step.MessageSeq)
			if to != "" || env.Subject != stallTeamOwnerSubject || env.Refs["escalation"] != "owner" || env.Refs["recipientKind"] != "owner" || !strings.Contains(env.Body.Text, "The owner helper is "+state+", so this goes to the owner.") {
				t.Fatalf("owner team notice to %q refs %+v text %s", to, env.Refs, env.Body.Text)
			}
			checkStallText(t, env.Body.Text)
		}
		if !found || f.count(`SELECT count(*) FROM stall_watch_episodes WHERE kind='team' AND recipient='owner' AND owner_state='pinged' AND task_id=?`, f.task.ID) != 1 {
			t.Fatalf("steps %+v", steps)
		}
		f.advance(10 * time.Minute)
		f.none("one notice")
	}
	t.Run("no helper", func(t *testing.T) {
		f := root.project(t, stallNoon)
		team(f)
		f.advance(30 * time.Minute)
		owner(t, f, "not registered")
	})
	t.Run("helper offline", func(t *testing.T) {
		f := root.project(t, stallNoon)
		h := f.helper()
		delete(f.online, h.ID)
		team(f)
		f.advance(30 * time.Minute)
		owner(t, f, "offline")
	})
	t.Run("helper quiet too", func(t *testing.T) {
		f := root.project(t, stallNoon)
		h := f.helper()
		f.state(h, "idle")
		f.owe(h)
		team(f)
		f.advance(20 * time.Minute)
		f.one(stallActionHelper)
		f.advance(10 * time.Minute)
		owner(t, f, "quiet too")
	})
	t.Run("retired helper", func(t *testing.T) {
		f := root.project(t, stallNoon)
		h := f.helper()
		f.owe(h)
		f.exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentRetired, h.ID)
		team(f)
		f.advance(30 * time.Minute)
		owner(t, f, "not registered")
		if f.count(`SELECT count(*) FROM stall_watch_episodes WHERE kind='helper' AND task_id=?`, f.task.ID) != 0 {
			t.Fatal("a retired helper was watched")
		}
	})
}

// stallNeverQuietCase applies one never-quiet condition to a fixture.
type stallNeverQuietCase struct {
	name  string
	apply func(f *stallFixture, entry string, lead api.Agent)
}

func (f *stallFixture) wait(entry, state string) {
	f.t.Helper()
	f.exec(`INSERT INTO team_queue_waits(task_id,entry_id,item_id,on_entry_id,on_item_id,paths_json,until,state,set_at) VALUES(?,?,?,?,?,'[]','released',?,?)`,
		f.task.ID, entry, f.items[0].ID, "tqe_other", f.items[1].ID, state, ts(f.now()))
}

func TestStallWatchNeverQuiet(t *testing.T) {
	root := newStallFixture(t, stallNoon)
	teamCases := []stallNeverQuietCase{
		{"owner pause", func(f *stallFixture, entry string, lead api.Agent) {
			f.exec(`UPDATE tasks SET pause_state='paused' WHERE id=?`, f.task.ID)
		}},
		{"project closed", func(f *stallFixture, entry string, lead api.Agent) {
			f.exec(`UPDATE tasks SET status='closed' WHERE id=?`, f.task.ID)
		}},
		{"shared-path wait waiting", func(f *stallFixture, entry string, lead api.Agent) { f.wait(entry, "waiting") }},
		{"shared-path wait met", func(f *stallFixture, entry string, lead api.Agent) { f.wait(entry, "met") }},
		{"shared-path wait orphaned", func(f *stallFixture, entry string, lead api.Agent) { f.wait(entry, "orphaned") }},
		{"held entry", func(f *stallFixture, entry string, lead api.Agent) {
			f.exec(`INSERT INTO team_queue_budget_holds(task_id,entry_id,item_id,estimate_tokens,team_tokens,team_state,state,created_at) VALUES(?,?,?,1,'2','running','held',?)`,
				f.task.ID, entry, f.items[0].ID, ts(f.now()))
		}},
		{"open owner decision", func(f *stallFixture, entry string, lead api.Agent) { f.decision(lead, 0) }},
		{"working", func(f *stallFixture, entry string, lead api.Agent) { f.state(lead, "working") }},
		{"matrix wait", func(f *stallFixture, entry string, lead api.Agent) { f.report(lead, stallMatrixWait(f.now())) }},
		{"entry released", func(f *stallFixture, entry string, lead api.Agent) {
			f.exec(`UPDATE team_queue_entries SET released_at=? WHERE id=?`, ts(f.now()), entry)
		}},
	}
	for _, state := range []string{"queued", "launching", "finished", "failed"} {
		teamCases = append(teamCases, stallNeverQuietCase{"entry " + state, func(f *stallFixture, entry string, lead api.Agent) {
			f.exec(`UPDATE team_queue_entries SET state=? WHERE id=?`, state, entry)
		}})
	}
	for _, tc := range teamCases {
		t.Run("team/"+tc.name, func(t *testing.T) {
			// No episode opens.
			f := root.project(t, stallNoon)
			h := f.helper()
			f.state(h, "idle")
			entry := f.entry(0)
			lead := f.member("lead-a", 0)
			f.member("builder-a", 0)
			tc.apply(f, entry, lead)
			for i := 0; i < 4; i++ {
				f.advance(30 * time.Minute)
				f.none("never quiet")
			}
			if f.episodes() != 0 {
				t.Fatal("an episode opened")
			}
			// An open one ends with no notice.
			g := root.project(t, stallNoon)
			h = g.helper()
			g.state(h, "idle")
			entry = g.entry(0)
			lead = g.member("lead-a", 0)
			g.advance(30 * time.Minute)
			g.one(stallActionTeam)
			g.advance(time.Minute)
			tc.apply(g, entry, lead)
			g.none("the open episode ends silently")
			if g.open() != 0 {
				t.Fatal("the episode stayed open")
			}
		})
	}
	helperCases := []stallNeverQuietCase{
		teamCases[0],
		{"working", func(f *stallFixture, _ string, h api.Agent) { f.state(h, "working") }},
		{"matrix wait", func(f *stallFixture, _ string, h api.Agent) { f.report(h, stallMatrixWait(f.now())) }},
		{"own unanswered owner decision", func(f *stallFixture, _ string, h api.Agent) {
			f.decision(h, int64(f.count(`SELECT max(message_seq) FROM obligations WHERE agent_id=?`, h.ID)))
		}},
	}
	for _, tc := range helperCases {
		t.Run("helper/"+tc.name, func(t *testing.T) {
			f := root.project(t, stallNoon)
			h := f.helper()
			f.state(h, "idle")
			f.owe(h)
			tc.apply(f, "", h)
			// The decision request is itself a post: wait it out as well.
			for i := 0; i < 4; i++ {
				f.advance(20 * time.Minute)
				f.none("never quiet")
			}
			if f.episodes() != 0 {
				t.Fatal("an episode opened")
			}
			g := root.project(t, stallNoon)
			h = g.helper()
			g.state(h, "idle")
			g.owe(h)
			g.advance(20 * time.Minute)
			g.one(stallActionHelper)
			g.advance(time.Minute)
			if tc.name == "own unanswered owner decision" {
				// Close on the decision, not on the post that carries it.
				m := g.post(h, int64(g.count(`SELECT max(message_seq) FROM obligations WHERE agent_id=?`, h.ID)))
				g.exec(`UPDATE stall_watch_episodes SET scanned_seq=? WHERE closed_at='' AND task_id=?`, m.Seq, g.task.ID)
				g.exec(`INSERT INTO decision_requests(message_seq,task_id,question,options,recommended_option_id,recommendation_reason,created_at) VALUES(?,?,?,?,?,?,?)`,
					m.Seq, g.task.ID, "Ship the candidate?", "[]", "yes", "synthetic", ts(g.now()))
				g.none("the open episode ends silently")
				g.closed("owner-decision")
				return
			}
			tc.apply(g, "", h)
			g.none("the open episode ends silently")
			if g.open() != 0 {
				t.Fatal("the episode stayed open")
			}
		})
	}
	// unknown, and needs_input without a decision request, are still pinged.
	for _, name := range []string{"unknown", "needs_input"} {
		t.Run("helper still pinged/"+name, func(t *testing.T) {
			f := root.project(t, stallNoon)
			h := f.helper()
			f.owe(h)
			if name == "needs_input" {
				f.exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentNeedsInput, h.ID)
			} else {
				f.state(h, "unknown")
			}
			f.advance(20 * time.Minute)
			_, env, _ := f.one(stallActionHelper)
			if env.Refs["activity"] != name {
				t.Fatalf("activity %q", env.Refs["activity"])
			}
		})
	}
	// An answered decision no longer exempts the helper, a full threshold
	// after the answer.
	t.Run("helper/answered decision", func(t *testing.T) {
		f := root.project(t, stallNoon)
		h := f.helper()
		f.state(h, "idle")
		owed := f.owe(h)
		f.decision(h, owed.Seq)
		f.advance(time.Hour)
		f.none("unanswered")
		f.answer()
		f.none("at the answer")
		f.advance(20*time.Minute - time.Second)
		f.none("19 m 59 s after the answer")
		f.advance(time.Second)
		f.one(stallActionHelper)
	})
	// Retired, closed, exited and role agents are not members.
	t.Run("team/non-members", func(t *testing.T) {
		f := root.project(t, stallNoon)
		h := f.helper()
		f.state(h, "idle")
		f.entry(0)
		f.member("lead-a", 0)
		for i, change := range []string{`status='retired'`, `status='closed'`, `status='exited'`, `role='database_handler'`, `role='deployment'`} {
			a := f.member(fmt.Sprintf("other-%d", i), 0)
			f.state(a, "working")
			f.exec(`UPDATE agents SET `+change+` WHERE id=?`, a.ID)
		}
		f.advance(30 * time.Minute)
		_, env, _ := f.one(stallActionTeam)
		if !strings.Contains(env.Body.Text, "1 member(s)") || strings.Contains(env.Body.Text, "other-") {
			t.Fatalf("text %s", env.Body.Text)
		}
	})
	// A team with no member is left to the queue's own stall.
	t.Run("team/no members", func(t *testing.T) {
		f := root.project(t, stallNoon)
		f.entry(0)
		f.advance(2 * time.Hour)
		f.none("no members")
	})
}

// p7: membership is bound to this admission.
func TestStallWatchTeamAdmission(t *testing.T) {
	f := newStallFixture(t, stallNoon.Add(-time.Hour))
	h := f.helper()
	f.state(h, "idle")
	// A run retained from an earlier admission of the same item.
	old := f.member("old-builder", 0)
	f.at(stallNoon)
	entry := f.entry(0)
	f.member("lead-a", 0)
	f.state(old, "working")
	f.advance(30*time.Minute - time.Second)
	f.post(old, 0)
	f.none("29 m 59 s")
	f.advance(time.Second)
	_, env, _ := f.one(stallActionTeam)
	if env.Refs["entry"] != entry || !strings.Contains(env.Body.Text, "1 member(s)") || strings.Contains(env.Body.Text, "old-builder") {
		t.Fatalf("notice %+v %s", env.Refs, env.Body.Text)
	}
	// Neither its post nor its working report ends the episode.
	f.advance(time.Minute)
	f.post(old, 0)
	f.state(old, "idle")
	f.state(old, "working")
	f.none("the earlier run's progress")
	if f.open() != 1 {
		t.Fatal("the earlier run ended the episode")
	}
	// An entry with no recorded admission falls back to its creation time.
	g := newStallFixture(t, stallNoon)
	g.entry(0)
	g.exec(`UPDATE team_queue_entries SET admitted_at='',created_at=? WHERE task_id=?`, ts(g.now()), g.task.ID)
	g.advance(time.Minute)
	g.member("lead-a", 0)
	g.advance(30*time.Minute - time.Second)
	g.none("29 m 59 s after the binding")
	g.advance(time.Second)
	g.one(stallActionTeam)
}

// stallDump is every table's rows, sorted, by table name.
func stallDump(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, name := range names {
		lines := strings.Split(tableDump(t, s, `SELECT * FROM "`+name+`"`), "\n")
		sort.Strings(lines)
		out[name] = lines
	}
	return out
}

// added returns the rows of after that before lacks; ok is false when a row
// of before is missing from after.
func stallAdded(before, after []string) (added []string, ok bool) {
	left := map[string]int{}
	for _, line := range before {
		left[line]++
	}
	for _, line := range after {
		if left[line] > 0 {
			left[line]--
			continue
		}
		added = append(added, line)
	}
	for _, n := range left {
		if n > 0 {
			return added, false
		}
	}
	return added, true
}

// a5: a sweep writes its episode rows and what posting a notice writes.
func TestStallWatchWritesOnlyNoticesAndEpisodes(t *testing.T) {
	noticeTables := map[string]bool{"messages": true, "message_checks": true, "events": true, "sqlite_sequence": true}
	check := func(t *testing.T, f *stallFixture, h *api.Agent, actions []string, directed int) {
		t.Helper()
		before := stallDump(t, f.s)
		counts := map[string]int{}
		for name := range noticeTables {
			if name != "sqlite_sequence" {
				counts[name] = f.count(`SELECT count(*) FROM "` + name + `"`)
			}
		}
		steps := f.sweep()
		var got []string
		for _, step := range steps {
			got = append(got, step.Action)
			f.notice(step.MessageSeq)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(actions, ",") {
			t.Fatalf("actions %v, want %v", got, actions)
		}
		after := stallDump(t, f.s)
		if len(after) != len(before) {
			t.Fatalf("tables changed: %d to %d", len(before), len(after))
		}
		for name, rows := range after {
			added, kept := stallAdded(before[name], rows)
			switch {
			case name == "stall_watch_episodes":
				continue
			case name == "sqlite_sequence":
				continue // SQLite's own counters for the notice tables
			case !kept:
				t.Fatalf("table %s: an existing row changed", name)
			case noticeTables[name]:
				// A notice's text spans lines, so its rows are counted.
				if n := f.count(`SELECT count(*) FROM "`+name+`"`) - counts[name]; n != len(steps) {
					t.Fatalf("table %s: %d new rows for %d notices", name, n, len(steps))
				}
			case name == "obligations" || name == "wake_jobs":
				if len(added) != directed {
					t.Fatalf("table %s: %d new rows, want %d", name, len(added), directed)
				}
				for _, line := range added {
					if h == nil || !strings.Contains(line, "agent_id="+h.ID+"|") || (name == "obligations" && !strings.Contains(line, "needs=delivery|")) {
						t.Fatalf("table %s: unexpected row %s", name, line)
					}
				}
			case len(added) != 0:
				t.Fatalf("table %s: %d rows written by the sweep: %v", name, len(added), added)
			}
		}
		// The sequence table changes only for the notice tables.
		added, _ := stallAdded(before["sqlite_sequence"], after["sqlite_sequence"])
		for _, line := range added {
			if !strings.Contains(line, "name=messages|") && !strings.Contains(line, "name=events|") && !strings.Contains(line, "name=message_checks|") {
				t.Fatalf("sqlite_sequence changed for %s", line)
			}
		}
	}
	t.Run("helper and team to the owner by day", func(t *testing.T) {
		f := newStallFixture(t, stallNoon)
		h := f.helper()
		f.state(h, "idle")
		f.owe(h)
		f.entry(0)
		f.member("lead-a", 0)
		f.advance(30 * time.Minute)
		check(t, f, &h, []string{stallActionHelper, stallActionTeam}, 0)
		check(t, f, &h, nil, 0)
	})
	t.Run("team to the helper", func(t *testing.T) {
		f := newStallFixture(t, stallNoon)
		h := f.helper()
		f.state(h, "idle")
		f.entry(0)
		f.member("lead-a", 0)
		f.advance(30 * time.Minute)
		check(t, f, &h, []string{stallActionTeam}, 1)
		check(t, f, &h, nil, 0)
	})
	t.Run("night lines, deferred pings and closes", func(t *testing.T) {
		f := newStallFixture(t, stallElevenPM.Add(-30*time.Minute))
		h := f.helper()
		f.state(h, "idle")
		owed := f.owe(h)
		f.entry(0)
		lead := f.member("lead-a", 0)
		f.at(stallElevenPM)
		check(t, f, &h, []string{stallActionHelper, stallActionTeam}, 0)
		f.at(stallSevenAM)
		check(t, f, &h, []string{stallActionHelperPing, stallActionTeamPing}, 0)
		f.post(h, owed.Seq)
		f.post(lead, 0)
		check(t, f, &h, nil, 0)
		if f.open() != 0 {
			t.Fatal("the episodes stayed open")
		}
	})
}

// a6: the whole decision is made again inside the write transaction.
func TestStallWatchRevalidatesInWrite(t *testing.T) {
	root := newStallFixture(t, stallNoon)
	type setup struct {
		f     *stallFixture
		h     api.Agent
		lead  api.Agent
		entry string
		owed  api.Message
	}
	changes := []struct {
		name   string
		team   bool // the change concerns the team watch
		change func(s setup)
	}{
		{"project paused", false, func(s setup) { s.f.exec(`UPDATE tasks SET pause_state='paused' WHERE id=?`, s.f.task.ID) }},
		{"helper posts", false, func(s setup) { s.f.post(s.h, s.owed.Seq) }},
		{"helper working report", false, func(s setup) { s.f.state(s.h, "working") }},
		{"helper run replaced", false, func(s setup) { s.f.helper() }},
		{"owed obligation closed", false, func(s setup) {
			s.f.exec(`UPDATE obligations SET state=? WHERE message_seq=?`, api.ObligationClosed, s.owed.Seq)
		}},
		{"project paused", true, func(s setup) { s.f.exec(`UPDATE tasks SET pause_state='paused' WHERE id=?`, s.f.task.ID) }},
		{"member posts", true, func(s setup) { s.f.post(s.lead, 0) }},
		{"member working report", true, func(s setup) { s.f.state(s.lead, "working") }},
		{"hold placed", true, func(s setup) {
			s.f.exec(`INSERT INTO team_queue_budget_holds(task_id,entry_id,item_id,estimate_tokens,team_tokens,team_state,state,created_at) VALUES(?,?,?,1,'2','running','held',?)`,
				s.f.task.ID, s.entry, s.f.items[0].ID, ts(s.f.now()))
		}},
	}
	build := func(t *testing.T, team bool, start time.Time) setup {
		s := setup{f: root.project(t, start)}
		if team {
			// No helper: the team notice is the owner's, so it can defer.
			s.entry = s.f.entry(0)
			s.lead = s.f.member("lead-a", 0)
			return s
		}
		s.h = s.f.helper()
		s.f.state(s.h, "idle")
		s.owed = s.f.owe(s.h)
		return s
	}
	for _, tc := range changes {
		kind := "helper"
		if tc.team {
			kind = "team"
		}
		t.Run(kind+" first notice/"+tc.name, func(t *testing.T) {
			s := build(t, tc.team, stallNoon)
			s.f.advance(30 * time.Minute)
			called := 0
			s.f.beforeWrite(func() { called++; tc.change(s) })
			s.f.none("changed between the read and the write")
			if called != 1 || s.f.episodes() != 0 {
				t.Fatalf("hook calls %d, episodes %d", called, s.f.episodes())
			}
		})
		t.Run(kind+" deferred ping/"+tc.name, func(t *testing.T) {
			s := build(t, tc.team, stallElevenPM.Add(-30*time.Minute))
			s.f.at(stallElevenPM)
			if steps := s.f.sweep(); len(steps) != 1 {
				t.Fatalf("night line steps %+v", steps)
			}
			s.f.at(stallSevenAM)
			called := 0
			s.f.beforeWrite(func() { called++; tc.change(s) })
			s.f.none("changed between the read and the write")
			if called != 1 || s.f.open() != 0 || s.f.count(`SELECT count(*) FROM stall_watch_episodes WHERE ping_seq<>0 AND task_id=?`, s.f.task.ID) != 0 {
				t.Fatalf("hook calls %d, open %d", called, s.f.open())
			}
			stallWatchBeforeWrite = nil
			s.f.none("the next sweep")
		})
	}
	// Without a change the same sweeps do post, so the cases above prove the
	// recheck and not a broken setup.
	for _, team := range []bool{false, true} {
		s := build(t, team, stallNoon)
		s.f.advance(30 * time.Minute)
		called := 0
		s.f.beforeWrite(func() { called++ })
		if steps := s.f.sweep(); len(steps) != 1 || called != 1 {
			t.Fatalf("unchanged sweep: steps %+v hook %d", steps, called)
		}
		stallWatchBeforeWrite = nil
	}
}

// a7: a quiet tick writes nothing.
func TestStallWatchQuietTickWritesNothing(t *testing.T) {
	f := newStallFixture(t, stallNoon)
	h := f.helper()
	f.state(h, "idle")
	owed := f.owe(h)
	f.entry(0)
	lead := f.member("lead-a", 0)
	f.state(lead, "working")
	f.advance(15 * time.Minute)
	f.post(h, owed.Seq)
	f.advance(15 * time.Minute)
	changes := func() int { return f.count(`SELECT total_changes()`) }
	before := changes()
	task, global := f.s.Changed(f.task.ID), f.s.Changed(Global)
	for i := 0; i < 3; i++ {
		f.none("quiet tick")
	}
	if after := changes(); after != before {
		t.Fatalf("total_changes moved by %d", after-before)
	}
	for _, ch := range []<-chan struct{}{task, global} {
		select {
		case <-ch:
			t.Fatal("a waiter was notified")
		default:
		}
	}
	// A posting sweep does notify, so the check above can fail.
	f.advance(20 * time.Minute)
	f.one(stallActionHelper)
	select {
	case <-task:
	default:
		t.Fatal("a posting sweep notified no waiter")
	}
}

// a7: both message reads are bounded.
func TestStallWatchBoundedMessageReads(t *testing.T) {
	ctx := context.Background()
	bulk := func(f *stallFixture, n int, at time.Time) {
		f.t.Helper()
		tx, err := f.s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(`INSERT INTO messages(task_id,from_agent,from_node,from_user,to_agent,text,created_at) VALUES(?,'agt_0000000000000000','workspace','owner','','filler',?)`, f.task.ID, ts(at)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("candidate read stops at the window", func(t *testing.T) {
		f := newStallFixture(t, stallNoon)
		h := f.helper()
		f.state(h, "idle")
		bulk(f, 5000, f.now())
		f.advance(31 * time.Minute)
		owed := f.owe(h)
		f.advance(10 * time.Minute)
		f.post(h, owed.Seq)
		bulk(f, 3, f.now())
		f.advance(19 * time.Minute)
		posts, read, err := stallWatchRecentPosts(ctx, f.s.db, f.task.ID, f.now().Add(-stallWatchPostWindow))
		if err != nil {
			t.Fatal(err)
		}
		// Inside the window: the owner's message, the helper's post, 3 fillers.
		if read > 5+1 || !posts[stallRun{h.ID, h.RunID}].Equal(f.now().Add(-19*time.Minute)) {
			t.Fatalf("read %d rows, posts %v", read, posts)
		}
		f.none("the helper posted 19 minutes ago")
		f.advance(time.Minute)
		f.one(stallActionHelper)
	})
	t.Run("episode read takes 500 and the cursor reaches the end", func(t *testing.T) {
		f := newStallFixture(t, stallNoon)
		h := f.helper()
		f.state(h, "idle")
		owed := f.owe(h)
		f.advance(20 * time.Minute)
		step, _, _ := f.one(stallActionHelper)
		bulk(f, 5000, f.now())
		last := int64(f.count(`SELECT max(seq) FROM messages`))
		cursor := func() int64 {
			return int64(f.count(`SELECT scanned_seq FROM stall_watch_episodes WHERE closed_at=''`))
		}
		if cursor() != step.MessageSeq {
			t.Fatalf("cursor starts at %d, want the notice %d", cursor(), step.MessageSeq)
		}
		sweeps := 0
		for cursor() < last {
			_, _, read, err := stallWatchEpisodePosts(ctx, f.s.db, f.task.ID, cursor())
			if err != nil || read > stallWatchScanLimit || read == 0 {
				t.Fatalf("episode read %d rows: %v", read, err)
			}
			before := cursor()
			f.none("filler posts")
			if sweeps++; sweeps > 11 {
				t.Fatalf("cursor at %d of %d after %d sweeps", cursor(), last, sweeps)
			}
			if cursor()-before > stallWatchScanLimit {
				t.Fatalf("cursor moved %d in one sweep", cursor()-before)
			}
		}
		if f.open() != 1 {
			t.Fatal("filler posts ended the episode")
		}
		// At the end the read is short and nothing is written.
		changes := f.count(`SELECT total_changes()`)
		f.none("at the end")
		if f.count(`SELECT total_changes()`) != changes {
			t.Fatal("a sweep at the end of the messages wrote")
		}
		// A post beyond 5 000 later messages still ends it.
		f.post(h, owed.Seq)
		f.none("the helper's post")
		f.closed("posted")
	})
}

// b1: time spent in a never-quiet case is not quiet time. No notice is posted
// when the case lifts, nor until a full threshold after it, and an episode
// that such a case closed does not post again when the case lifts.
func TestStallWatchQuietAfterWaitLifts(t *testing.T) {
	root := newStallFixture(t, stallNoon)
	hold := func(f *stallFixture, entry string) {
		f.exec(`INSERT INTO team_queue_budget_holds(task_id,entry_id,item_id,estimate_tokens,team_tokens,team_state,state,created_at) VALUES(?,?,?,1,'2','running','held',?)`,
			f.task.ID, entry, f.items[0].ID, ts(f.now()))
	}
	cases := []struct {
		name        string
		team        bool
		apply, lift func(f *stallFixture, entry string, a api.Agent, owed int64)
	}{
		{"helper/own decision answered", false,
			func(f *stallFixture, _ string, h api.Agent, owed int64) { f.decision(h, owed) },
			func(f *stallFixture, _ string, _ api.Agent, _ int64) { f.answer() }},
		{"helper/matrix wait cleared", false,
			func(f *stallFixture, _ string, h api.Agent, _ int64) { f.report(h, stallMatrixWait(f.now())) },
			func(f *stallFixture, _ string, h api.Agent, _ int64) { f.state(h, "idle") }},
		{"helper/pause resumed", false,
			func(f *stallFixture, _ string, _ api.Agent, _ int64) { f.pause() },
			func(f *stallFixture, _ string, _ api.Agent, _ int64) { f.resume() }},
		{"team/decision answered", true,
			func(f *stallFixture, _ string, lead api.Agent, _ int64) { f.decision(lead, 0) },
			func(f *stallFixture, _ string, _ api.Agent, _ int64) { f.answer() }},
		{"team/hold released", true,
			func(f *stallFixture, entry string, _ api.Agent, _ int64) { hold(f, entry) },
			func(f *stallFixture, entry string, _ api.Agent, _ int64) {
				f.exec(`UPDATE team_queue_budget_holds SET state='continued',resolved_at=? WHERE task_id=? AND entry_id=?`, ts(f.now()), f.task.ID, entry)
			}},
		{"team/shared-path wait cleared", true,
			func(f *stallFixture, entry string, _ api.Agent, _ int64) { f.wait(entry, "waiting") },
			func(f *stallFixture, entry string, _ api.Agent, _ int64) {
				f.exec(`UPDATE team_queue_waits SET state='cleared',closed_at=? WHERE task_id=? AND entry_id=?`, ts(f.now()), f.task.ID, entry)
			}},
		{"team/matrix wait cleared", true,
			func(f *stallFixture, _ string, lead api.Agent, _ int64) { f.report(lead, stallMatrixWait(f.now())) },
			func(f *stallFixture, _ string, lead api.Agent, _ int64) { f.state(lead, "idle") }},
		{"team/pause resumed", true,
			func(f *stallFixture, _ string, _ api.Agent, _ int64) { f.pause() },
			func(f *stallFixture, _ string, _ api.Agent, _ int64) { f.resume() }},
	}
	for _, tc := range cases {
		threshold, action := stallWatchHelperQuiet, stallActionHelper
		if tc.team {
			threshold, action = stallWatchTeamQuiet, stallActionTeam
		}
		// build returns the watched subject: the owing helper, or a team's lead
		// with an available helper that owes nothing.
		build := func(t *testing.T) (*stallFixture, string, api.Agent, int64) {
			f := root.project(t, stallNoon)
			h := f.helper()
			f.state(h, "idle")
			if !tc.team {
				return f, "", h, f.owe(h).Seq
			}
			entry := f.entry(0)
			lead := f.member("lead-a", 0)
			f.state(lead, "idle")
			return f, entry, lead, 0
		}
		after := func(t *testing.T, f *stallFixture) {
			t.Helper()
			f.none("when the case lifts")
			f.advance(threshold - time.Second)
			f.none("a second short of the threshold after the lift")
			f.advance(time.Second)
			f.one(action)
			f.advance(time.Minute)
			f.none("one notice")
		}
		t.Run(tc.name+"/held past the threshold", func(t *testing.T) {
			f, entry, a, owed := build(t)
			f.advance(time.Minute)
			tc.apply(f, entry, a, owed)
			for i := 0; i < 3; i++ {
				f.advance(threshold)
				f.none("during the case")
			}
			tc.lift(f, entry, a, owed)
			after(t, f)
			if f.episodes() != 1 {
				t.Fatalf("%d episodes, want one", f.episodes())
			}
		})
		t.Run(tc.name+"/closes an open episode", func(t *testing.T) {
			f, entry, a, owed := build(t)
			f.advance(threshold)
			f.one(action)
			f.advance(time.Minute)
			tc.apply(f, entry, a, owed)
			f.none("the case closes the episode")
			if f.open() != 0 {
				t.Fatal("the episode stayed open")
			}
			f.advance(time.Minute)
			tc.lift(f, entry, a, owed)
			after(t, f)
		})
	}
	// A matrix wait the relay stopped reporting ends at its bound.
	t.Run("team/matrix wait outlives its bound", func(t *testing.T) {
		f := root.project(t, stallNoon)
		f.entry(0)
		lead := f.member("lead-a", 0)
		f.report(lead, stallMatrixWait(f.now()))
		f.advance(defaultQueueMatrixWaitBound - time.Second)
		f.none("inside the bound")
		f.advance(stallWatchTeamQuiet)
		f.none("a second short of the threshold after the bound")
		f.advance(time.Second)
		f.one(stallActionTeam)
	})
}
