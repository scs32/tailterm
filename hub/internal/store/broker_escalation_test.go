package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/broker"
	"github.com/scs32/tailterm/hub/internal/store"
)

// Tick-driven scenarios for progress-aware escalation (wi_e76872a6abc6d78e).
// This mirrors the fixture in internal/broker/broker_test.go, which lives in
// package broker and cannot be imported.

const stallSubject = "Project stalled: overdue work and no progress"

type escalationFixture struct {
	st                    *store.Store
	ctx                   context.Context
	by                    api.Caller
	task                  api.Task
	lead, builder, worker api.Agent
	c, clock              time.Time
}

// newEscalationFixture opens one store per test with a first project;
// subtests share the store and call fresh for a new project.
func newEscalationFixture(t *testing.T) *escalationFixture {
	t.Helper()
	f := &escalationFixture{ctx: context.Background(), by: api.Caller{Node: "test", User: "owner"}}
	f.c = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetClockForTest(func() time.Time { return f.clock })
	f.st = st
	return f.fresh(t)
}

// fresh starts a new project a day after the previous one, with a lead, a
// builder and a worker, and sets the clock to its start. Earlier projects
// stay in the store; tick only reports steps for the obligation it is given.
func (f *escalationFixture) fresh(t *testing.T) *escalationFixture {
	t.Helper()
	if f.task.ID != "" {
		f.c = f.c.Add(24 * time.Hour)
	}
	f.clock = f.c
	var err error
	if f.task, err = f.st.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Escalation", Orchestrator: "lead"}, f.by); err != nil {
		t.Fatal(err)
	}
	add := func(name string) api.Agent {
		a, err := f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: name, Host: "h", Session: name, Runtime: "codex"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	f.lead, f.builder, f.worker = add("lead"), add("builder"), add("worker")
	return f
}

func (f *escalationFixture) at(d time.Duration) { f.clock = f.c.Add(d) }

func (f *escalationFixture) assign(t *testing.T, to api.Agent, due string) api.Obligation {
	t.Helper()
	m, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: to.ID, Envelope: &api.Envelope{
		Kind: "assign", To: to.Name, Subject: "Carry out the execution request", Due: due,
		Body: api.EnvelopeBody{Objective: "run it", Owns: []string{"matrix"}, Acceptance: map[string]string{"a1": "passes"}}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return f.obligation(t, m.Seq, to)
}

func (f *escalationFixture) obligation(t *testing.T, seq int64, holder api.Agent) api.Obligation {
	t.Helper()
	obls, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: holder.ID}, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obls {
		if o.MessageSeq == seq {
			return o
		}
	}
	t.Fatalf("no obligation for #%d", seq)
	return api.Obligation{}
}

func (f *escalationFixture) act(t *testing.T, o api.Obligation, holder api.Agent, action string, d time.Duration) {
	t.Helper()
	if _, err := f.st.ObligationAction(f.ctx, f.task.ID, o.MessageSeq, action, api.ObligationActionRequest{AgentID: holder.ID, RunID: holder.RunID}, f.c.Add(d)); err != nil {
		t.Fatal(err)
	}
}

// reply posts a NOTICE (or RESULT) reply from the holder to o at +d.
func (f *escalationFixture) reply(t *testing.T, o api.Obligation, holder api.Agent, kind string, d time.Duration) {
	t.Helper()
	f.at(d)
	env := &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Still running the execution request", Body: api.EnvelopeBody{Text: "halfway through the matrix"}}
	if kind == api.EnvelopeKindResult {
		env = &api.Envelope{Kind: api.EnvelopeKindResult, Subject: "The execution request finished", Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}},
			Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "verify-matrix run", Outcome: "ok"}}}
	}
	if _, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: holder.ID, RunID: holder.RunID, ReplyTo: o.MessageSeq, Envelope: env}, f.by); err != nil {
		t.Fatal(err)
	}
}

// tick runs one broker pass at +d and returns o's steps and project stalls.
func (f *escalationFixture) tick(t *testing.T, o api.Obligation, d time.Duration) []string {
	t.Helper()
	steps, err := (&broker.Broker{Store: f.st}).Tick(f.ctx, f.c.Add(d))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range steps {
		if s.ObligationID == o.ID || (s.Action == "project-stalled" && s.TaskID == o.TaskID) {
			out = append(out, s.Action)
		}
	}
	return out
}

func wantSteps(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: steps %v, want %v", label, got, want)
	}
}

// notices returns the broker's notices about o at the given escalation level
// ("lead", "owner" or "stall").
func (f *escalationFixture) notices(t *testing.T, o api.Obligation, level string) []api.Message {
	t.Helper()
	msgs, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range msgs {
		if m.From.Node != api.BrokerNode || m.Envelope == nil || m.Envelope.Refs["escalation"] != level {
			continue
		}
		if level == "stall" || m.Envelope.Refs["obligation"] == o.ID {
			out = append(out, m)
		}
	}
	return out
}

// toOwnerLevel runs the unacknowledged obligation through its wakes and both
// escalations: lead at +10m1s, owner at +25m1s.
func (f *escalationFixture) toOwnerLevel(t *testing.T, o api.Obligation) {
	t.Helper()
	for _, d := range []time.Duration{time.Minute, 3 * time.Minute, 7 * time.Minute} {
		wantSteps(t, "wake", f.tick(t, o, d), "wake")
	}
	wantSteps(t, "+10m1s", f.tick(t, o, 10*time.Minute+time.Second), "escalate-lead")
	wantSteps(t, "+25m1s", f.tick(t, o, 25*time.Minute+time.Second), "escalate-owner")
}

// a1 (e1): the incident. A NOTICE reply 5 minutes before the due time moves
// it 30 minutes past the notice, and the lead hears only after that.
func TestNoticeReplyPostponesDueEscalation(t *testing.T) {
	f := newEscalationFixture(t)
	o := f.assign(t, f.builder, "45m")
	f.act(t, o, f.builder, "ack", time.Minute)
	f.reply(t, o, f.builder, api.EnvelopeKindNotice, 40*time.Minute)
	if got := f.obligation(t, o.MessageSeq, f.builder); !got.DueAt.Equal(f.c.Add(70 * time.Minute)) {
		t.Fatalf("due %s, want +70m", got.DueAt.Sub(f.c))
	}
	wantSteps(t, "+46m", f.tick(t, o, 46*time.Minute))
	wantSteps(t, "+69m", f.tick(t, o, 69*time.Minute))
	wantSteps(t, "+71m", f.tick(t, o, 71*time.Minute), "escalate-lead")
	if n := len(f.notices(t, o, "lead")); n != 1 {
		t.Fatalf("%d lead notices", n)
	}
}

// a4 (e1 cap): a NOTICE every 20 minutes cannot hold the due time past 2h
// beyond its original; the lead hears once, and later notices do not reset
// that escalation, so the owner hears 15 minutes after.
func TestProgressPostponementCap(t *testing.T) {
	f := newEscalationFixture(t)
	o := f.assign(t, f.builder, "45m")
	f.act(t, o, f.builder, "ack", time.Minute)
	limit := f.c.Add(45*time.Minute + store.ObligationProgressCap)
	var escalations []string
	for m := 2; m <= 200; m++ {
		d := time.Duration(m) * time.Minute
		if m%20 == 0 {
			f.reply(t, o, f.builder, api.EnvelopeKindNotice, d)
			if got := f.obligation(t, o.MessageSeq, f.builder); got.DueAt.After(limit) {
				t.Fatalf("+%dm: due %s is past the cap", m, got.DueAt.Sub(f.c))
			}
		}
		for _, s := range f.tick(t, o, d) {
			if strings.HasPrefix(s, "escalate-") {
				escalations = append(escalations, s+"@"+d.String())
			}
		}
	}
	if got := f.obligation(t, o.MessageSeq, f.builder); !got.DueAt.Equal(limit) {
		t.Fatalf("due %s, want the cap at +165m", got.DueAt.Sub(f.c))
	}
	wantSteps(t, "escalations", escalations, "escalate-lead@2h46m0s", "escalate-owner@3h1m0s")
	if lead, owner := len(f.notices(t, o, "lead")), len(f.notices(t, o, "owner")); lead != 1 || owner != 1 {
		t.Fatalf("lead %d owner %d", lead, owner)
	}
}

// a5 (e4): with no messages at all, the lead hears at the first tick after the
// due time and the owner 15 minutes later, as before.
func TestSilentRecipientStillEscalatesOnTime(t *testing.T) {
	f := newEscalationFixture(t)
	o := f.assign(t, f.builder, "45m")
	f.act(t, o, f.builder, "ack", time.Minute)
	var steps []string
	for m := 2; m <= 70; m++ {
		d := time.Duration(m) * time.Minute
		for _, s := range f.tick(t, o, d) {
			steps = append(steps, s+"@"+d.String())
		}
	}
	wantSteps(t, "silent", steps, "nudge@32m0s", "escalate-lead@46m0s", "escalate-owner@1h1m0s")
	if got := f.obligation(t, o.MessageSeq, f.builder); !got.DueAt.Equal(f.c.Add(45 * time.Minute)) {
		t.Fatalf("due moved without progress: %s", got.DueAt.Sub(f.c))
	}
}

// a6 (e2): another running worker's progress within the quiet window holds
// off the stall notice, which then fires once 15 minutes after that progress.
// A closed worker's progress does not count.
func TestStallWaitsForRecentTeamProgress(t *testing.T) {
	shared := newEscalationFixture(t)
	for _, variant := range []string{"notice", "progress", "result"} {
		t.Run(variant, func(t *testing.T) {
			f := shared.fresh(t)
			o := f.assign(t, f.builder, "")
			w := f.assign(t, f.worker, "")
			f.act(t, w, f.worker, "ack", 2*time.Minute)
			f.toOwnerLevel(t, o)
			switch variant {
			case "notice":
				f.reply(t, w, f.worker, api.EnvelopeKindNotice, 35*time.Minute)
			case "progress":
				f.act(t, w, f.worker, "progress", 35*time.Minute)
			case "result":
				f.reply(t, w, f.worker, api.EnvelopeKindResult, 35*time.Minute)
				if got := f.obligation(t, w.MessageSeq, f.worker); got.State != api.ObligationClosed {
					t.Fatalf("the result did not close the worker's obligation: %s", got.State)
				}
			}
			wantSteps(t, "+40m2s", f.tick(t, o, 40*time.Minute+2*time.Second))
			wantSteps(t, "+49m", f.tick(t, o, 49*time.Minute))
			wantSteps(t, "+50m", f.tick(t, o, 50*time.Minute), "project-stalled")
			wantSteps(t, "+51m", f.tick(t, o, 51*time.Minute))
			wantSteps(t, "+80m", f.tick(t, o, 80*time.Minute))
			if n := len(f.notices(t, o, "stall")); n != 1 {
				t.Fatalf("%d stall notices", n)
			}
		})
	}
	t.Run("closed worker", func(t *testing.T) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "")
		w := f.assign(t, f.worker, "")
		f.act(t, w, f.worker, "ack", 2*time.Minute)
		f.toOwnerLevel(t, o)
		f.reply(t, w, f.worker, api.EnvelopeKindNotice, 35*time.Minute)
		f.at(36 * time.Minute)
		if _, err := f.st.CloseAgentRun(f.ctx, f.worker.ID, f.worker.RunID, f.by); err != nil {
			t.Fatal(err)
		}
		wantSteps(t, "+40m2s", f.tick(t, o, 40*time.Minute+2*time.Second), "project-stalled")
		wantSteps(t, "+50m", f.tick(t, o, 50*time.Minute))
		if n := len(f.notices(t, o, "stall")); n != 1 {
			t.Fatalf("%d stall notices", n)
		}
	})
}

var progressAge = regexp.MustCompile(`last progress (\d+)m ago|no progress recorded`)

func wantProgressAge(t *testing.T, label string, msgs []api.Message, want string) {
	t.Helper()
	if len(msgs) != 1 {
		t.Fatalf("%s: %d notices", label, len(msgs))
	}
	text := msgs[0].Envelope.Body.Text
	if got := progressAge.FindString(text); got != want {
		t.Fatalf("%s: %q in %q, want %q", label, got, text, want)
	}
}

// a7 (e3): lead, owner and stall notices say how long since the last
// progress on the obligation, or that none was recorded.
func TestEscalationSaysTimeSinceProgress(t *testing.T) {
	shared := newEscalationFixture(t)
	t.Run("with progress", func(t *testing.T) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "45m")
		f.act(t, o, f.builder, "ack", time.Minute)
		f.reply(t, o, f.builder, api.EnvelopeKindNotice, 40*time.Minute)
		wantSteps(t, "+71m", f.tick(t, o, 71*time.Minute), "escalate-lead")
		wantProgressAge(t, "lead", f.notices(t, o, "lead"), "last progress 31m ago")
		wantSteps(t, "+86m", f.tick(t, o, 86*time.Minute), "escalate-owner")
		wantProgressAge(t, "owner", f.notices(t, o, "owner"), "last progress 46m ago")
		wantSteps(t, "+101m", f.tick(t, o, 101*time.Minute), "project-stalled")
		stall := f.notices(t, o, "stall")
		wantProgressAge(t, "stall", stall, "last progress 61m ago")
		if !strings.Contains(stall[0].Envelope.Body.Text, "(outcome; last progress 61m ago)") {
			t.Fatalf("stall line: %q", stall[0].Envelope.Body.Text)
		}
	})
	t.Run("without progress", func(t *testing.T) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "")
		f.toOwnerLevel(t, o)
		wantProgressAge(t, "lead", f.notices(t, o, "lead"), "no progress recorded")
		wantProgressAge(t, "owner", f.notices(t, o, "owner"), "no progress recorded")
		wantSteps(t, "+40m2s", f.tick(t, o, 40*time.Minute+2*time.Second), "project-stalled")
		stall := f.notices(t, o, "stall")
		wantProgressAge(t, "stall", stall, "no progress recorded")
		if !strings.Contains(stall[0].Envelope.Body.Text, "(ack; no progress recorded)") {
			t.Fatalf("stall line: %q", stall[0].Envelope.Body.Text)
		}
	})
}

// report records the holder's activity at +d: a matrix wait when w is set,
// or no wait at all.
func (f *escalationFixture) report(t *testing.T, a api.Agent, state string, w *api.MatrixWait, d time.Duration) {
	t.Helper()
	f.at(d)
	activity := api.AgentActivity{State: state, ObservedAt: f.c.Add(d), MatrixWait: w}
	if _, err := f.st.ReportActivity(f.ctx, f.task.ID, a.ID, api.ActivityReport{RequestID: fmt.Sprintf("matrix-%s-%s", a.RunID, d), RunID: a.RunID, Activity: activity}); err != nil {
		t.Fatal(err)
	}
}

// silentRun ticks once a minute from +from to +limit and appends o's steps,
// stamped with their time, to steps. Nothing happens before the +32m nudge,
// so callers start near it; each tick sweeps every project in the store.
// The project stall notice follows an owner escalation by 15 minutes, as it
// does today.
func (f *escalationFixture) silentRun(t *testing.T, o api.Obligation, from, limit time.Duration, steps []string) []string {
	t.Helper()
	for d := from; d <= limit; d += time.Minute {
		for _, s := range f.tick(t, o, d) {
			steps = append(steps, s+"@"+d.String())
		}
	}
	return steps
}

// wi_c7e55118ae8e7780 (o1-o3): a holder whose current run waits for or
// holds the host's matrix lock has a known reason and resume condition, so
// the owner escalation waits; the lead still hears on time. Once the wait
// ends, the owner hears a full lead window (15 minutes) after its end. An
// acknowledged obligation due at +45m with no progress is nudged at +32m,
// goes to the lead at +46m and, without a wait, to the owner at +61m.
func TestMatrixWaitHoldsOwnerEscalation(t *testing.T) {
	waiting := func(f *escalationFixture, since time.Duration) *api.MatrixWait {
		return &api.MatrixWait{Role: api.MatrixWaitWaiting, Position: 1, Length: 2, Since: f.c.Add(since)}
	}
	cases := []struct {
		name string
		// mid runs after the +40m tick; ticks resume at +41m.
		mid   func(t *testing.T, f *escalationFixture)
		steps []string
		owner int
	}{
		{name: "waiting for the host", mid: func(t *testing.T, f *escalationFixture) {
			f.report(t, f.builder, "idle", waiting(f, 0), 40*time.Minute)
		}, steps: []string{"nudge@32m0s", "escalate-lead@46m0s"}},
		{name: "holding the host", mid: func(t *testing.T, f *escalationFixture) {
			f.report(t, f.builder, "working", &api.MatrixWait{Role: api.MatrixWaitRunning, Since: f.c.Add(40 * time.Minute)}, 40*time.Minute)
		}, steps: []string{"nudge@32m0s", "escalate-lead@46m0s"}},
		{name: "wait ends while overdue", mid: func(t *testing.T, f *escalationFixture) {
			f.report(t, f.builder, "idle", waiting(f, 0), 40*time.Minute)
			f.report(t, f.builder, "idle", nil, 70*time.Minute+30*time.Second)
		}, steps: []string{"nudge@32m0s", "escalate-lead@46m0s", "escalate-owner@1h26m0s", "project-stalled@1h41m0s"}, owner: 1},
		{name: "no wait", mid: func(t *testing.T, f *escalationFixture) {
			f.report(t, f.builder, "working", nil, 40*time.Minute)
		}, steps: []string{"nudge@32m0s", "escalate-lead@46m0s", "escalate-owner@1h1m0s", "project-stalled@1h16m0s"}, owner: 1},
		{name: "wait on an earlier run", mid: func(t *testing.T, f *escalationFixture) {
			f.report(t, f.builder, "idle", waiting(f, 0), 40*time.Minute)
			exited := api.AgentExited
			if _, err := f.st.UpdateAgent(f.ctx, f.builder.ID, api.UpdateAgentRequest{Status: &exited}, f.by); err != nil {
				t.Fatal(err)
			}
			next, err := f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "builder", Host: "h", Session: "builder", Runtime: "codex"}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			if next.ID != f.builder.ID || next.RunID == f.builder.RunID {
				t.Fatalf("restart: agent %s run %s, was %s run %s", next.ID, next.RunID, f.builder.ID, f.builder.RunID)
			}
		}, steps: []string{"nudge@32m0s", "escalate-lead@46m0s", "escalate-owner@1h1m0s", "project-stalled@1h16m0s"}, owner: 1},
	}
	shared := newEscalationFixture(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := shared.fresh(t)
			o := f.assign(t, f.builder, "45m")
			f.act(t, o, f.builder, "ack", time.Minute)
			steps := f.silentRun(t, o, 30*time.Minute, 40*time.Minute, nil)
			tc.mid(t, f)
			steps = f.silentRun(t, o, 41*time.Minute, 2*time.Hour, steps)
			wantSteps(t, tc.name, steps, tc.steps...)
			if lead, owner := len(f.notices(t, o, "lead")), len(f.notices(t, o, "owner")); lead != 1 || owner != tc.owner {
				t.Fatalf("lead %d owner %d, want 1 and %d", lead, owner, tc.owner)
			}
		})
	}
	// A wait that ended more than a lead window before the owner escalation
	// is due does not delay it.
	t.Run("wait ended long before", func(t *testing.T) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "45m")
		f.act(t, o, f.builder, "ack", time.Minute)
		f.report(t, f.builder, "idle", waiting(f, 0), 5*time.Minute)
		f.report(t, f.builder, "idle", nil, 10*time.Minute)
		steps := f.silentRun(t, o, 30*time.Minute, 90*time.Minute, nil)
		wantSteps(t, "ended early", steps, "nudge@32m0s", "escalate-lead@46m0s", "escalate-owner@1h1m0s", "project-stalled@1h16m0s")
	})
	// The lead holds the obligation, so its escalation would go straight to
	// the owner: it waits out the matrix wait and its window too.
	t.Run("lead holds", func(t *testing.T) {
		f := shared.fresh(t)
		m, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.lead.ID, Envelope: &api.Envelope{
			Kind: api.EnvelopeKindRequest, To: f.lead.Name, Subject: "Decide how the release proceeds", Due: "45m", Body: api.EnvelopeBody{Ask: "Decide the release."}}}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		o := f.obligation(t, m.Seq, f.lead)
		f.act(t, o, f.lead, "ack", time.Minute)
		steps := f.silentRun(t, o, 30*time.Minute, 40*time.Minute, nil)
		f.report(t, f.lead, "idle", waiting(f, 0), 40*time.Minute)
		steps = f.silentRun(t, o, 41*time.Minute, 2*time.Hour, steps)
		f.report(t, f.lead, "idle", nil, 2*time.Hour)
		steps = f.silentRun(t, o, 2*time.Hour+time.Minute, 2*time.Hour+40*time.Minute, steps)
		wantSteps(t, "lead holds", steps, "nudge@32m0s", "escalate-lead@2h15m0s", "project-stalled@2h30m0s")
		if lead, owner := len(f.notices(t, o, "lead")), len(f.notices(t, o, "owner")); lead != 0 || owner != 1 {
			t.Fatalf("lead %d owner %d, want 0 and 1", lead, owner)
		}
	})
}

// wi_c7e55118ae8e7780 (o3): a block on a queued fix behaves as before the
// matrix-wait rule: no owner escalation while the entry is queued, and the
// next pass after it leaves the queue escalates to the owner.
func TestQueuedFixBlockUnchangedByMatrixWaitRule(t *testing.T) {
	f := newEscalationFixture(t)
	handler, err := f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler", Role: api.AgentRoleDatabaseHandler, Host: "h", Session: "db-handler", Runtime: "codex"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{AgentID: handler.ID, RunID: handler.RunID, Kind: api.EventRunning}, f.by); err != nil {
		t.Fatal(err)
	}
	item, err := f.st.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "The fix the block waits on", RequestID: "queued-fix"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	order, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "bounded fix order", RequestID: "queued-fix-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ConfirmWorkOrderScope(f.ctx, f.task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "queued-fix-scope", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}); err != nil {
		t.Fatal(err)
	}
	q, err := f.st.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: "queued-fix-add", Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "h", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	o := f.assign(t, f.builder, "45m")
	f.act(t, o, f.builder, "ack", time.Minute)
	f.at(2 * time.Minute)
	if _, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.lead.ID, ReplyTo: o.MessageSeq,
		AuditKind: api.MessageAuditWork, RequestID: "block-" + o.ID, WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}},
		Envelope: &api.Envelope{Kind: api.EnvelopeKindBlock, To: f.lead.Name, Subject: "Blocked until the queued fix lands", Body: api.EnvelopeBody{Reason: "the fix is queued", Needs: "the queued fix", ResumeWhen: "the fix is released"}}}, f.by); err != nil {
		t.Fatal(err)
	}
	for d := 10 * time.Minute; d <= 4*time.Hour; d += 10 * time.Minute {
		for _, s := range f.tick(t, o, d) {
			if s == "escalate-owner" {
				t.Fatalf("owner escalation at +%s for a block on a queued fix", d)
			}
		}
	}
	if n := len(f.notices(t, o, "owner")); n != 0 {
		t.Fatalf("%d owner notices", n)
	}
	if _, err := f.st.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: "remove-fix", Operation: "remove", EntryID: q.ID, ExpectedRevision: q.Revision}); err != nil {
		t.Fatal(err)
	}
	wantSteps(t, "after the entry left the queue", f.tick(t, o, 4*time.Hour+time.Minute), "escalate-owner")
	if n := len(f.notices(t, o, "owner")); n != 1 {
		t.Fatalf("%d owner notices", n)
	}
}
