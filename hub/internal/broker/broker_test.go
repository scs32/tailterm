package broker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/store"
)

type fixture struct {
	path          string
	st            *store.Store
	task          api.Task
	lead, builder api.Agent
	by            api.Caller
	ctx           context.Context
}

func newFixture(t *testing.T) *fixture {
	return newFixtureWithBuilder(t, "builder")
}

func newFixtureWithBuilder(t *testing.T, builderName string) *fixture {
	f := &fixture{path: filepath.Join(t.TempDir(), "hub.sqlite"), by: api.Caller{Node: "test", User: "owner"}, ctx: context.Background()}
	f.open(t)
	var err error
	if f.task, err = f.st.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Broker", Orchestrator: "lead"}, f.by); err != nil {
		t.Fatal(err)
	}
	add := func(name string) api.Agent {
		a, err := f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: name, Host: "h", Session: name, Runtime: "codex"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	f.lead, f.builder = add("lead"), add(builderName)
	return f
}

func (f *fixture) open(t *testing.T) {
	st, err := store.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.st = st
	t.Cleanup(func() { st.Close() })
}

// restart simulates a hub restart: a new Store and Broker over the same file.
func (f *fixture) restart(t *testing.T) {
	f.st.Close()
	f.open(t)
}

func (f *fixture) assign(t *testing.T, due string) api.Obligation {
	t.Helper()
	m, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: &api.Envelope{
		Kind: "assign", To: f.builder.Name, Subject: "Reject the empty recipient in tt post", Due: due,
		Body: api.EnvelopeBody{Objective: "fail fast", Owns: []string{"main.go"}, Acceptance: map[string]string{"a1": "exits 2"}}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	obls, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: f.builder.ID}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obls {
		if o.MessageSeq == m.Seq {
			return o
		}
	}
	t.Fatal("no obligation")
	return api.Obligation{}
}

// actions returns this obligation's steps (and project stalls) at now.
func (f *fixture) tick(t *testing.T, o api.Obligation, now time.Time) []string {
	t.Helper()
	steps, err := (&Broker{Store: f.st}).Tick(f.ctx, now)
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

func (f *fixture) brokerNotices(t *testing.T) (toLead, toOwner int) {
	t.Helper()
	msgs, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.From.Node != api.BrokerNode || m.Envelope == nil {
			continue
		}
		if m.To == f.lead.ID {
			toLead++
		} else if m.To == "" && m.Envelope.Subject != "Project stalled: overdue work and no progress" {
			toOwner++
		}
	}
	return
}

func want(t *testing.T, at string, got []string, expected ...string) {
	t.Helper()
	if len(got) != len(expected) {
		t.Fatalf("%s: steps %v, want %v", at, got, expected)
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Fatalf("%s: steps %v, want %v", at, got, expected)
		}
	}
}

// b5 and b10: wakes at 1/3/7 min, lead at 10 min, owner 15 min later, and
// one project-stall notice 15 quiet minutes after the owner escalation. Each
// fires exactly once, across restarts, and the stall never repeats without
// recipient activity (the broker's own escalations are not activity).
func TestUnacknowledgedAssignmentEscalatesOnce(t *testing.T) {
	f := newFixture(t)
	o := f.assign(t, "")
	c := o.CreatedAt
	want(t, "+30s", f.tick(t, o, c.Add(30*time.Second)))
	want(t, "+1m", f.tick(t, o, c.Add(time.Minute)), "wake")
	want(t, "+1m again", f.tick(t, o, c.Add(time.Minute)))
	want(t, "+3m", f.tick(t, o, c.Add(3*time.Minute)), "wake")
	f.restart(t)
	want(t, "+7m after restart", f.tick(t, o, c.Add(7*time.Minute)), "wake")
	want(t, "+8m", f.tick(t, o, c.Add(8*time.Minute)))
	want(t, "+10m1s", f.tick(t, o, c.Add(10*time.Minute+time.Second)), "escalate-lead")
	if lead, owner := f.brokerNotices(t); lead != 1 || owner != 0 {
		t.Fatalf("after lead escalation: lead %d owner %d", lead, owner)
	}
	f.restart(t)
	want(t, "+20m", f.tick(t, o, c.Add(20*time.Minute)))
	want(t, "+25m1s", f.tick(t, o, c.Add(25*time.Minute+time.Second)), "escalate-owner")
	want(t, "+40m", f.tick(t, o, c.Add(40*time.Minute)))
	want(t, "+40m2s", f.tick(t, o, c.Add(40*time.Minute+2*time.Second)), "project-stalled")
	want(t, "+41m", f.tick(t, o, c.Add(41*time.Minute)))
	want(t, "+70m no repeat", f.tick(t, o, c.Add(70*time.Minute)))
	if lead, owner := f.brokerNotices(t); lead != 1 || owner != 1 {
		t.Fatalf("after owner escalation: lead %d owner %d", lead, owner)
	}
	// Recipient activity clears the escalation; a later miss starts with a
	// reminder again, not a stall notice.
	if _, err := f.st.ObligationAction(f.ctx, f.task.ID, o.MessageSeq, "ack", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, c.Add(71*time.Minute)); err != nil {
		t.Fatal(err)
	}
	want(t, "+90m", f.tick(t, o, c.Add(90*time.Minute)))
	want(t, "+101m1s", f.tick(t, o, c.Add(101*time.Minute+time.Second)), "nudge")
}

func TestScopedAgentNamesSurviveBrokerSubjects(t *testing.T) {
	const scoped = "builder-41b1c632"
	for _, tc := range []struct {
		name  string
		step  time.Duration
		owner bool
	}{
		{"lead escalation", 10*time.Minute + time.Second, false},
		{"owner escalation", 25*time.Minute + time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtureWithBuilder(t, scoped)
			clock := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
			f.st.SetClockForTest(func() time.Time { return clock })
			o := f.assign(t, "")
			for _, at := range []time.Duration{time.Minute, 3 * time.Minute, 7 * time.Minute, 10*time.Minute + time.Second} {
				f.tick(t, o, clock.Add(at))
			}
			if tc.owner {
				f.tick(t, o, clock.Add(tc.step))
			}
			wantTo := f.lead.ID
			if tc.owner {
				wantTo = ""
			}
			msgs, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, m := range msgs {
				if m.From.Node == api.BrokerNode && m.To == wantTo && m.Envelope != nil && m.Envelope.Refs["escalation"] != "" {
					found = true
					if !strings.Contains(m.Envelope.Subject, scoped) || !strings.Contains(m.Text, scoped) {
						t.Fatalf("subject/text lost scoped name: %q / %q", m.Envelope.Subject, m.Text)
					}
				}
			}
			if !found {
				t.Fatal("escalation notice missing")
			}
		})
	}

	t.Run("nudge", func(t *testing.T) {
		f := newFixtureWithBuilder(t, scoped)
		clock := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
		f.st.SetClockForTest(func() time.Time { return clock })
		o := f.assign(t, "")
		if _, err := f.st.ObligationAction(f.ctx, f.task.ID, o.MessageSeq, "ack", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, clock.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		f.tick(t, o, clock.Add(32*time.Minute))
		msgs, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.From.Node == api.BrokerNode && m.To == f.builder.ID {
				if m.Envelope == nil || !strings.Contains(m.Envelope.Subject, scoped) || !strings.Contains(m.Text, scoped) {
					t.Fatalf("nudge lost scoped name: %+v", m)
				}
				return
			}
		}
		t.Fatal("nudge notice missing")
	})

	t.Run("reassignment", func(t *testing.T) {
		f := newFixture(t)
		clock := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
		f.st.SetClockForTest(func() time.Time { return clock })
		old := f.assign(t, "")
		target, err := f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: scoped, Host: "h", Session: scoped, Runtime: "codex"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		m, err := f.st.ReassignObligation(f.ctx, f.task.ID, old.ID, api.ObligationReassignRequest{ToAgentID: target.ID}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		if m.To != target.ID || m.Envelope == nil || !strings.Contains(m.Envelope.Subject, scoped) || !strings.Contains(m.Text, scoped) {
			t.Fatalf("reassignment lost scoped name or routing: %+v", m)
		}
		prior, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: f.builder.ID}, clock)
		if err != nil || len(prior) != 1 || prior[0].Outcome != api.OutcomeSuperseded {
			t.Fatalf("original obligation changed incorrectly: %v %+v", err, prior)
		}
		moved, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: target.ID}, clock)
		if err != nil || len(moved) != 1 || moved[0].MessageSeq != m.Seq || moved[0].Needs != api.ObligationNeedsOutcome {
			t.Fatalf("new obligation changed incorrectly: %v %+v", err, moved)
		}
	})
}

// b6: silence nudges twice at 30-minute spacing, then escalates; a missed due
// time escalates to the lead.
func TestSilenceAndDueTimeEscalate(t *testing.T) {
	f := newFixture(t)
	o := f.assign(t, "")
	ackAt := o.CreatedAt.Add(time.Minute)
	if _, err := f.st.ObligationAction(f.ctx, f.task.ID, o.MessageSeq, "ack", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, ackAt); err != nil {
		t.Fatal(err)
	}
	want(t, "+29m", f.tick(t, o, ackAt.Add(29*time.Minute)))
	want(t, "+31m", f.tick(t, o, ackAt.Add(31*time.Minute)), "nudge")
	want(t, "+45m", f.tick(t, o, ackAt.Add(45*time.Minute)))
	want(t, "+62m", f.tick(t, o, ackAt.Add(62*time.Minute)), "nudge")
	want(t, "+93m", f.tick(t, o, ackAt.Add(93*time.Minute)), "escalate-lead")

	g := newFixture(t)
	q := g.assign(t, "45m")
	if _, err := g.st.ObligationAction(g.ctx, g.task.ID, q.MessageSeq, "ack", api.ObligationActionRequest{AgentID: g.builder.ID, RunID: g.builder.RunID}, q.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := g.st.ObligationAction(g.ctx, g.task.ID, q.MessageSeq, "progress", api.ObligationActionRequest{AgentID: g.builder.ID, RunID: g.builder.RunID}, q.CreatedAt.Add(25*time.Minute)); err != nil {
		t.Fatal(err)
	}
	want(t, "due +44m", g.tick(t, q, q.CreatedAt.Add(44*time.Minute)))
	want(t, "due +46m", g.tick(t, q, q.CreatedAt.Add(46*time.Minute)), "escalate-lead")
}

// A closed recipient's obligations close instead of escalating.
func TestClosedRecipientClosesObligation(t *testing.T) {
	f := newFixture(t)
	o := f.assign(t, "")
	if _, err := f.st.CloseAgentRun(f.ctx, f.builder.ID, f.builder.RunID, f.by); err != nil {
		t.Fatal(err)
	}
	want(t, "closed", f.tick(t, o, o.CreatedAt.Add(time.Minute)), "recipient-gone")
	obls, _ := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: f.builder.ID}, time.Now())
	if obls[0].State != api.ObligationClosed || obls[0].Outcome != api.OutcomeRecipientGone {
		t.Fatalf("%+v", obls[0])
	}
}

// Round one f4/F9: silence waits a full window after the last reminder, and a
// missed explicit due time escalates even while reminders are still running.
func TestSilenceWindowAndDueTime(t *testing.T) {
	f := newFixture(t)
	o := f.assign(t, "")
	ackAt := o.CreatedAt.Add(time.Minute)
	if _, err := f.st.ObligationAction(f.ctx, f.task.ID, o.MessageSeq, "ack", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, ackAt); err != nil {
		t.Fatal(err)
	}
	want(t, "+31m", f.tick(t, o, ackAt.Add(31*time.Minute)), "nudge")
	want(t, "+62m", f.tick(t, o, ackAt.Add(62*time.Minute)), "nudge")
	want(t, "+62m30s", f.tick(t, o, ackAt.Add(62*time.Minute+30*time.Second)))

	g := newFixture(t)
	q := g.assign(t, "45m")
	if _, err := g.st.ObligationAction(g.ctx, g.task.ID, q.MessageSeq, "ack", api.ObligationActionRequest{AgentID: g.builder.ID, RunID: g.builder.RunID}, q.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	want(t, "due +32m", g.tick(t, q, q.CreatedAt.Add(32*time.Minute)), "nudge")
	want(t, "due +46m", g.tick(t, q, q.CreatedAt.Add(46*time.Minute)), "escalate-lead")
}

// Round one f5: activity clears escalation, so a later miss reaches the lead
// first rather than jumping to the owner.
func TestEscalationResetsAfterActivity(t *testing.T) {
	f := newFixture(t)
	o := f.assign(t, "")
	c := o.CreatedAt
	// A cold start past the wake times sends one catch-up wake; the escalation
	// follows on the next 30-second pass.
	want(t, "+10m1s", f.tick(t, o, c.Add(10*time.Minute+time.Second)), "wake")
	want(t, "+10m2s", f.tick(t, o, c.Add(10*time.Minute+2*time.Second)), "escalate-lead")
	act := func(action string, at time.Duration) {
		if _, err := f.st.ObligationAction(f.ctx, f.task.ID, o.MessageSeq, action, api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, c.Add(at)); err != nil {
			t.Fatal(err)
		}
	}
	act("ack", 12*time.Minute)
	for m := 30; m <= 110; m += 20 {
		act("progress", time.Duration(m)*time.Minute)
	}
	want(t, "+2h1s", f.tick(t, o, c.Add(2*time.Hour+time.Second)), "escalate-lead")
}

// Round one f3: the stall notice posts even when many obligations are overdue.
func TestStallNoticeFitsTheBodyLimit(t *testing.T) {
	f := newFixture(t)
	var first api.Obligation
	for i := 0; i < 45; i++ {
		m, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: &api.Envelope{
			Kind: "assign", To: "builder", Subject: "Long subject for a stall listing entry that keeps going to make the notice body large",
			Body: api.EnvelopeBody{Objective: "x", Owns: []string{"f"}, Acceptance: map[string]string{"a1": "y"}}}}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			obls, _ := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{}, time.Now())
			first = obls[0]
			_ = m
		}
	}
	c := first.CreatedAt
	for _, at := range []time.Duration{10*time.Minute + 5*time.Second, 10*time.Minute + 6*time.Second, 25*time.Minute + 10*time.Second, 40*time.Minute + 20*time.Second} {
		if _, err := (&Broker{Store: f.st}).Tick(f.ctx, c.Add(at)); err != nil {
			t.Fatalf("tick at %s: %v", at, err)
		}
	}
	msgs, _ := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 500)
	found := false
	for _, m := range msgs {
		if m.Envelope != nil && m.Envelope.Subject == "Project stalled: overdue work and no progress" {
			found = strings.Contains(m.Envelope.Body.Text, "and 35 more")
		}
	}
	if !found {
		t.Fatal("stall notice with a capped listing was not posted")
	}
}

// Round one F6/f10: a broker action on a stale snapshot is skipped.
func TestStaleSnapshotIsSkipped(t *testing.T) {
	f := newFixture(t)
	o := f.assign(t, "")
	snapshot, err := f.st.BrokerOpenObligations(f.ctx)
	if err != nil || len(snapshot) != 1 {
		t.Fatalf("snapshot %v %d", err, len(snapshot))
	}
	if _, err := f.st.ObligationAction(f.ctx, f.task.ID, o.MessageSeq, "ack", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.st.BrokerEscalate(f.ctx, snapshot[0], 1, "not acknowledged", o.CreatedAt.Add(11*time.Minute)); !errors.Is(err, store.ErrBrokerStale) {
		t.Fatalf("stale escalation: %v", err)
	}
	if lead, owner := f.brokerNotices(t); lead != 0 || owner != 0 {
		t.Fatalf("stale action posted notices: lead %d owner %d", lead, owner)
	}
}

// Round one F2/f8: a retired recipient is not woken, but its overdue work
// still escalates so the lead can reassign it.
func TestRetiredRecipientEscalatesWithoutWakes(t *testing.T) {
	f := newFixture(t)
	o := f.assign(t, "")
	if _, err := f.st.UpdateAgent(f.ctx, f.builder.ID, api.UpdateAgentRequest{Status: ptr(api.AgentRetired)}, f.by); err != nil {
		t.Fatal(err)
	}
	c := o.CreatedAt
	want(t, "retired +1m", f.tick(t, o, c.Add(time.Minute)))
	want(t, "retired +10m1s", f.tick(t, o, c.Add(10*time.Minute+time.Second)), "escalate-lead")
}

func ptr[T any](v T) *T { return &v }

// Round-two focused fix B2/N2: delivering the broker's own notice is not
// activity and never repeats the stall notice.
func TestStallDoesNotRepeatWhenNoticesAreDelivered(t *testing.T) {
	f := newFixture(t)
	o := f.assign(t, "")
	c := o.CreatedAt
	for _, at := range []time.Duration{time.Minute, 3 * time.Minute, 7 * time.Minute, 10*time.Minute + time.Second, 25*time.Minute + time.Second} {
		f.tick(t, o, c.Add(at))
	}
	want(t, "+40m2s", f.tick(t, o, c.Add(40*time.Minute+2*time.Second)), "project-stalled")
	if err := f.st.MarkObligationsDelivered(f.ctx, f.task.ID, f.lead.ID, f.lead.RunID, c.Add(45*time.Minute)); err != nil {
		t.Fatal(err)
	}
	want(t, "+45m1s after the lead read its notices", f.tick(t, o, c.Add(45*time.Minute+time.Second)))
	want(t, "+80m", f.tick(t, o, c.Add(80*time.Minute)))
}

// Round-two focused fix B3: the stall listing is bounded by bytes, not lines.
func TestStallNoticeFitsWithMultibyteSubjects(t *testing.T) {
	f := newFixture(t)
	subject := strings.Repeat("🚀", 117) + " ok"
	var first api.Obligation
	for i := 0; i < 12; i++ {
		if _, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: &api.Envelope{
			Kind: "assign", To: "builder", Subject: subject, Body: api.EnvelopeBody{Objective: "x", Owns: []string{"f"}, Acceptance: map[string]string{"a1": "y"}}}}, f.by); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			obls, _ := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{}, time.Now())
			first = obls[0]
		}
	}
	c := first.CreatedAt
	for _, at := range []time.Duration{10*time.Minute + 5*time.Second, 10*time.Minute + 6*time.Second, 25*time.Minute + 10*time.Second, 40*time.Minute + 20*time.Second} {
		if _, err := (&Broker{Store: f.st}).Tick(f.ctx, c.Add(at)); err != nil {
			t.Fatalf("tick at %s: %v", at, err)
		}
	}
	msgs, _ := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 500)
	for _, m := range msgs {
		if m.Envelope != nil && m.Envelope.Subject == "Project stalled: overdue work and no progress" {
			return
		}
	}
	t.Fatal("stall notice with multibyte subjects was not posted")
}
