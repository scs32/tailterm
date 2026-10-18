package broker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
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

func TestRepeatedReassignmentNamesCurrentTarget(t *testing.T) {
	for _, finalName := range []string{"reviewer-41b1c632", "reviewer"} {
		t.Run(finalName, func(t *testing.T) {
			f := newFixture(t)
			clock := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
			f.st.SetClockForTest(func() time.Time { return clock })
			first := f.assign(t, "")
			intermediate, err := f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "builder-41b1c632", Host: "h", Session: "intermediate", Runtime: "codex"}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			m1, err := f.st.ReassignObligation(f.ctx, f.task.ID, first.ID, api.ObligationReassignRequest{ToAgentID: intermediate.ID}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			middle, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: intermediate.ID}, clock)
			if err != nil || len(middle) != 1 {
				t.Fatalf("first move: %v %+v", err, middle)
			}
			final, err := f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: finalName, Host: "h", Session: "final", Runtime: "codex"}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			m2, err := f.st.ReassignObligation(f.ctx, f.task.ID, middle[0].ID, api.ObligationReassignRequest{ToAgentID: final.ID}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			if m2.To != final.ID || m2.Envelope == nil || m2.Envelope.To != final.Name || !strings.Contains(m2.Envelope.Subject, finalName) || strings.Contains(m2.Envelope.Subject, intermediate.Name) {
				t.Fatalf("second subject or routing: %+v", m2)
			}
			if m2.Envelope.Refs["reassignedFrom"] != fmt.Sprint(m1.Seq) || !reflect.DeepEqual(m2.Envelope.Body, m1.Envelope.Body) {
				t.Fatalf("source history or body changed: first %+v second %+v", m1.Envelope, m2.Envelope)
			}
			prior, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: intermediate.ID}, clock)
			if err != nil || len(prior) != 1 || prior[0].Outcome != api.OutcomeSuperseded {
				t.Fatalf("intermediate state: %v %+v", err, prior)
			}
			current, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: final.ID}, clock)
			if err != nil || len(current) != 1 || current[0].MessageSeq != m2.Seq || current[0].Needs != api.ObligationNeedsOutcome {
				t.Fatalf("final state: %v %+v", err, current)
			}
		})
	}
}

func TestReassignmentTruncationKeepsValidSubject(t *testing.T) {
	f := newFixture(t)
	target, err := f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "builder-41b1c632", Host: "h", Session: "target", Runtime: "codex"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	// The original token is valid; cutting inside it would create abc_123456.
	subject := strings.Repeat("w", 76) + " abc_123456xyz and more words"
	if _, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, To: f.builder.ID, Envelope: &api.Envelope{
		Kind: "assign", To: f.builder.Name, Subject: subject,
		Body: api.EnvelopeBody{Objective: "x", Owns: []string{"main.go"}, Acceptance: map[string]string{"a1": "y"}}}}, f.by); err != nil {
		t.Fatalf("original subject: %v", err)
	}
	old, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: f.builder.ID}, time.Now())
	if err != nil || len(old) != 1 {
		t.Fatalf("original obligation: %v %+v", err, old)
	}
	m, err := f.st.ReassignObligation(f.ctx, f.task.ID, old[0].ID, api.ObligationReassignRequest{ToAgentID: target.ID}, f.by)
	if err != nil {
		t.Fatalf("reassignment: %v", err)
	}
	if m.Envelope == nil || !strings.Contains(m.Envelope.Subject, target.Name) || len([]rune(m.Envelope.Subject)) > 120 {
		t.Fatalf("bounded subject lost target: %+v", m.Envelope)
	}
	if err := api.NormalizeBrokerPost(&api.PostMessageRequest{Envelope: m.Envelope}, target.Name); err != nil {
		t.Fatalf("shortened subject invalid: %v", err)
	}
}

func TestReassignmentKeepsBrokerOriginHistory(t *testing.T) {
	f := newFixtureWithBuilder(t, "builder-41b1c632")
	clock := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	f.st.SetClockForTest(func() time.Time { return clock })
	o := f.assign(t, "")
	for _, at := range []time.Duration{time.Minute, 3 * time.Minute, 7 * time.Minute, 10*time.Minute + time.Second} {
		f.tick(t, o, clock.Add(at))
	}
	leadNotices, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: f.lead.ID}, clock)
	if err != nil || len(leadNotices) != 1 {
		t.Fatalf("lead notice obligation: %v %+v", err, leadNotices)
	}
	target, err := f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: "reviewer-41b1c632", Host: "h", Session: "reviewer", Runtime: "codex"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	m, err := f.st.ReassignObligation(f.ctx, f.task.ID, leadNotices[0].ID, api.ObligationReassignRequest{ToAgentID: target.ID}, f.by)
	if err != nil {
		t.Fatalf("broker-origin reassignment: %v", err)
	}
	if m.Envelope == nil || !strings.Contains(m.Envelope.Subject, target.Name) || m.Envelope.Refs["reassignedFrom"] != fmt.Sprint(leadNotices[0].MessageSeq) || m.Envelope.Refs["escalation"] != "lead" {
		t.Fatalf("broker-origin history lost: %+v", m)
	}
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

// queuedFix files an item with a confirmed order and queues it, returning
// the item and its queue entry.
func (f *fixture) queuedFix(t *testing.T) (api.WorkItem, api.TeamQueueEntry) {
	t.Helper()
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
	return item, q
}

// blockOn has the holder acknowledge and then BLOCK the obligation, linking
// the item as primary or related.
func (f *fixture) blockOn(t *testing.T, o api.Obligation, holder, to api.Agent, item api.WorkItem, relationship string, at time.Time) {
	t.Helper()
	if _, err := f.st.ObligationAction(f.ctx, f.task.ID, o.MessageSeq, "ack", api.ObligationActionRequest{AgentID: holder.ID, RunID: holder.RunID}, at); err != nil {
		t.Fatalf("ack: %v", err)
	}
	links := []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
	if relationship == "related" {
		// A related link rides beside the message's own primary item.
		own, err := f.st.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "The blocked work itself", RequestID: "blocked-own"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		links = []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: own.ID, ItemRevision: own.Revision, Relationship: "primary"}, {ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "related"}}
	}
	if _, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: holder.ID, RunID: holder.RunID, To: to.ID, ReplyTo: o.MessageSeq,
		AuditKind: api.MessageAuditWork, RequestID: "block-" + o.ID, WorkItems: links,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindBlock, To: to.Name, Subject: "Blocked until the queued fix lands", Body: api.EnvelopeBody{Reason: "the fix is queued", Needs: "the queued fix", ResumeWhen: "the fix is released"}}}, f.by); err != nil {
		t.Fatalf("block: %v", err)
	}
}

// ownerEscalations counts owner-level escalation notices for one obligation.
// The obligation a BLOCK reply creates on its own recipient is a separate
// obligation (follow-up f4, wi_e76872a6abc6d78e) and is not counted.
func (f *fixture) ownerEscalations(t *testing.T, o api.Obligation) int {
	t.Helper()
	msgs, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if m.From.Node == api.BrokerNode && m.Envelope != nil && m.Envelope.Refs["obligation"] == o.ID && m.Envelope.Refs["escalation"] == "owner" {
			n++
		}
	}
	return n
}

// a8 (c3): an overdue obligation whose holder's latest BLOCK links an item
// with an unreleased queued, launching or running entry gets no owner
// escalation, whether a worker or the lead holds it; once the entry leaves
// the queue the next tick escalates normally.
func TestBlockOnQueuedFixIsNotEscalatedToOwner(t *testing.T) {
	for _, relationship := range []string{"primary", "related"} {
		t.Run("worker-"+relationship, func(t *testing.T) {
			f := newFixture(t)
			item, q := f.queuedFix(t)
			o := f.assign(t, "")
			c := o.CreatedAt
			f.blockOn(t, o, f.builder, f.lead, item, relationship, c.Add(time.Minute))
			var steps []string
			for _, at := range []time.Duration{time.Hour, 2 * time.Hour, 5 * time.Hour, 10 * time.Hour} {
				steps = append(steps, f.tick(t, o, c.Add(at))...)
			}
			for _, s := range steps {
				if s == "escalate-owner" {
					t.Fatalf("owner escalation for a block on a queued fix: %v", steps)
				}
			}
			if n := f.ownerEscalations(t, o); n != 0 {
				t.Fatalf("%d owner escalations", n)
			}
			if _, err := f.st.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: "remove-fix", Operation: "remove", EntryID: q.ID, ExpectedRevision: q.Revision}); err != nil {
				t.Fatal(err)
			}
			if got := f.tick(t, o, c.Add(11*time.Hour)); len(got) == 0 || got[0] != "escalate-owner" || f.ownerEscalations(t, o) != 1 {
				t.Fatalf("after the entry left the queue: %v", got)
			}
		})
	}
	t.Run("lead", func(t *testing.T) {
		f := newFixture(t)
		item, q := f.queuedFix(t)
		m, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: f.lead.ID, Envelope: &api.Envelope{
			Kind: api.EnvelopeKindRequest, To: f.lead.Name, Subject: "Decide how the release proceeds", Body: api.EnvelopeBody{Ask: "Decide the release."}}}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		obls, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: f.lead.ID}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var o api.Obligation
		for _, x := range obls {
			if x.MessageSeq == m.Seq {
				o = x
			}
		}
		if o.ID == "" {
			t.Fatal("no lead obligation")
		}
		c := o.CreatedAt
		f.blockOn(t, o, f.lead, f.builder, item, "primary", c.Add(time.Minute))
		for _, at := range []time.Duration{time.Hour, 3 * time.Hour, 10 * time.Hour} {
			for _, step := range f.tick(t, o, c.Add(at)) {
				if step == "escalate-lead" || step == "escalate-owner" {
					t.Fatalf("lead-held block on a queued fix at %v: %s", at, step)
				}
			}
		}
		if n := f.ownerEscalations(t, o); n != 0 {
			t.Fatalf("%d owner escalations", n)
		}
		if _, err := f.st.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: "remove-fix", Operation: "remove", EntryID: q.ID, ExpectedRevision: q.Revision}); err != nil {
			t.Fatal(err)
		}
		if got := f.tick(t, o, c.Add(11*time.Hour)); len(got) == 0 || got[0] != "escalate-lead" {
			t.Fatalf("after the entry left the queue: %v", got)
		}
		if n := f.ownerEscalations(t, o); n != 1 {
			t.Fatalf("lead-held escalation reached the owner %d times", n)
		}
	})
}

// Provider blocks (wi_72f41bd375032cf0): Tick runs the owner-notice sweep,
// in a paused project too, and a second tick posts nothing more.
func TestBrokerProviderBlockSweep(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
			f := newFixture(t)
			since := time.Now().UTC().Add(-time.Minute)
			block := api.AgentActivity{State: "provider_blocked", ObservedAt: time.Now().UTC(),
				Provider: &api.ProviderBlock{Provider: "openai", Runtime: "codex", Model: "gpt-6-astra", Class: "auth", Code: "other", Status: 401, Since: since}}
			if _, err := f.st.ReportActivity(f.ctx, f.task.ID, f.builder.ID, api.ActivityReport{RequestID: "blocked-1", RunID: f.builder.RunID, Activity: block}); err != nil {
				t.Fatal(err)
			}
			if paused {
				targets := []api.ProjectPauseTargetRequest{{AgentID: f.lead.ID, RunID: f.lead.RunID, ServiceDisposition: api.PauseServiceNone}, {AgentID: f.builder.ID, RunID: f.builder.RunID, ServiceDisposition: api.PauseServiceNone}}
				if _, err := f.st.PauseProject(f.ctx, f.task.ID, api.PauseProjectRequest{Version: 1, RequestID: "pause-for-sweep", Targets: targets}, f.by); err != nil {
					t.Fatal(err)
				}
			}
			b := &Broker{Store: f.st}
			owner := func() (n int) {
				t.Helper()
				msgs, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 500)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range msgs {
					if m.From.Node == api.BrokerNode && m.To == "" && m.Envelope != nil && m.Envelope.Subject == "An agent is blocked by its provider and needs the owner" {
						if !strings.Contains(m.Envelope.Body.Text, "openai auth") {
							t.Fatalf("notice text %q", m.Envelope.Body.Text)
						}
						// wi_1ef7c41da8711d4b: the notice is the owner's ping
						// and names the fix and the host.
						if m.Envelope.Refs["escalation"] != "owner" || !strings.Contains(m.Envelope.Body.Text, "run codex login on h") {
							t.Fatalf("notice refs %v text %q", m.Envelope.Refs, m.Envelope.Body.Text)
						}
						n++
					}
				}
				return n
			}
			if _, err := b.Tick(f.ctx, time.Now().Add(10*time.Second)); err != nil || owner() != 0 {
				t.Fatalf("inside the grace period: %v notices=%d", err, owner())
			}
			if _, err := b.Tick(f.ctx, time.Now().Add(2*time.Minute)); err != nil || owner() != 1 {
				t.Fatalf("after the grace period: %v notices=%d", err, owner())
			}
			if _, err := b.Tick(f.ctx, time.Now().Add(3*time.Minute)); err != nil || owner() != 1 {
				t.Fatalf("second tick: %v notices=%d", err, owner())
			}
		})
	}
}

// p3 (wi_1ef7c41da8711d4b): when one of two agents blocked by an expired
// login recovers, the other is woken through the ordinary wake job, with no
// one typing into its terminal, and the tick adds nothing to it.
func TestBrokerProviderRecoveryWake(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	report := func(a api.Agent, key string, activity api.AgentActivity) {
		t.Helper()
		activity.ObservedAt = time.Now().UTC()
		if _, err := f.st.ReportActivity(f.ctx, f.task.ID, a.ID, api.ActivityReport{RequestID: key, RunID: a.RunID, Activity: activity}); err != nil {
			t.Fatal(err)
		}
	}
	block := api.AgentActivity{State: "provider_blocked",
		Provider: &api.ProviderBlock{Provider: "openai", Runtime: "codex", Model: "gpt-6-astra", Class: "auth", Code: "other", Status: 401, Since: now.Add(-time.Minute)}}
	report(f.lead, "blocked-lead", block)
	report(f.builder, "blocked-builder", block)
	if _, err := f.st.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, Kind: api.EventHeartbeat}, f.by); err != nil {
		t.Fatal(err)
	}
	if job, err := f.st.LeaseWakeJob(f.ctx, f.task.ID, f.builder.ID, f.builder.RunID, time.Now()); err != nil || job != nil {
		t.Fatalf("wake job while both are blocked: %+v %v", job, err)
	}
	report(f.lead, "recovered-lead", api.AgentActivity{State: "working"})
	recovery := func() (seqs []int64) {
		t.Helper()
		msgs, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 500)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.From.Node == api.BrokerNode && m.To == f.builder.ID && m.Envelope != nil && m.Envelope.Subject == "Provider login restored; resume your work" {
				seqs = append(seqs, m.Seq)
			}
		}
		return seqs
	}
	seqs := recovery()
	if len(seqs) != 1 {
		t.Fatalf("recovery notices to the blocked agent: %v", seqs)
	}
	b := &Broker{Store: f.st}
	if _, err := b.Tick(f.ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	job, err := f.st.LeaseWakeJob(f.ctx, f.task.ID, f.builder.ID, f.builder.RunID, time.Now())
	if err != nil || job == nil {
		t.Fatalf("no wake job after the recovery: %+v %v", job, err)
	}
	if want := fmt.Sprintf("#%d notice: Provider login restored; resume your work", seqs[0]); job.MessageSeq != seqs[0] || !strings.Contains(job.Prompt, want) {
		t.Fatalf("wake job seq %d prompt %q, want %q", job.MessageSeq, job.Prompt, want)
	}
	// The woken agent's own recovery starts no second round.
	report(f.builder, "recovered-builder", api.AgentActivity{State: "working"})
	if got := recovery(); len(got) != 1 {
		t.Fatalf("recovery notices after the woken agent recovered: %v", got)
	}
}

// a10 (wi_d8ff05d1989ff279): the tick runs the deployer liveness sweep. The
// project has a silent deployer and a verified release job; the store's own
// tests cover the rule, this one that Tick reaches it.
func TestBrokerTickRunsDeployerLivenessSweep(t *testing.T) {
	f := newFixture(t)
	add := func(name, role string) api.Agent {
		t.Helper()
		a, err := f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Role: role, Host: "h", Session: name, Runtime: "codex"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	handler, deployer, helper := add("db-handler", api.AgentRoleDatabaseHandler), add("deployer", api.AgentRoleDeployment), add("owner-helper", "")
	// The wrapper's heartbeat, as tt wrap posts it.
	if _, err := f.st.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{AgentID: deployer.ID, RunID: deployer.RunID, Kind: api.EventHeartbeat}, f.by); err != nil {
		t.Fatal(err)
	}
	// A verified job and the owner helper's role have no short public path
	// from this package; the rows go into this test's own database file.
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`UPDATE agents SET role=? WHERE id=?`, []any{api.AgentRoleOwnerHelper, helper.ID}},
		{`INSERT INTO release_jobs(task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,'verified',1,'{}')`, []any{f.task.ID, api.NewID("rel"), api.NewID("tqe")}},
	} {
		if _, err := db.Exec(stmt.query, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	b := &Broker{Store: f.st}
	tick := func(at time.Time) (silent int) {
		t.Helper()
		steps, err := b.Tick(f.ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range steps {
			if s.Action == "deployer-silent" {
				if s.TaskID != f.task.ID || s.MessageSeq == 0 {
					t.Fatalf("step %+v", s)
				}
				silent++
			}
		}
		return silent
	}
	copies := func() (toHandler, toHelper int) {
		t.Helper()
		msgs, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 500)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.From.Node != api.BrokerNode || m.Envelope == nil || m.Envelope.Subject != "Deployer has gone silent while release jobs wait" {
				continue
			}
			switch m.To {
			case handler.ID:
				toHandler++
			case helper.ID:
				toHelper++
			default:
				t.Fatalf("liveness notice to %q", m.To)
			}
		}
		return
	}
	start := time.Now().UTC()
	// The first tick is the sweep's first sight of the job: it must stand
	// for the bound, seen by a tick every 30 s as the hub runs them, before
	// it counts as waiting.
	at := start
	for ; at.Before(start.Add(10 * time.Minute)); at = at.Add(30 * time.Second) {
		if n := tick(at); n != 0 {
			t.Fatalf("inside the bound, after %s: %d steps", at.Sub(start), n)
		}
	}
	past := at
	if n := tick(past); n != 2 {
		t.Fatalf("past the bound: %d deployer-silent steps, want 2", n)
	}
	if h, o := copies(); h != 1 || o != 1 {
		t.Fatalf("copies: handler %d, owner helper %d", h, o)
	}
	if n := tick(past); n != 0 {
		t.Fatalf("second tick: %d steps", n)
	}
	for at = past; at.Before(past.Add(30 * time.Minute)); at = at.Add(30 * time.Second) {
		if n := tick(at); n != 0 {
			t.Fatalf("later tick: %d steps", n)
		}
	}
	if h, o := copies(); h != 1 || o != 1 {
		t.Fatalf("copies after later ticks: handler %d, owner helper %d", h, o)
	}
}

// waitProject queues two entries in the fixture's project, the second
// sequenced behind the first, with a live database handler and an owner
// helper. db is this test's own handle on its database file, for the rows
// that have no short public path from this package.
type waitProject struct {
	handler, helper api.Agent
	// waitingLead is the waiting item's own lead once giveLead has run. The
	// fixture's project orchestrator, f.lead, leads neither item.
	waitingLead           api.Agent
	predItem, waitingItem api.WorkItem
	pred, waiting         api.TeamQueueEntry
	db                    *sql.DB
}

// giveLead gives the waiting item its own live team lead, as its launch does.
func (f *fixture) giveLead(t *testing.T, p *waitProject) {
	t.Helper()
	var err error
	if p.waitingLead, err = f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "waiting-lead", Host: "h", Session: "waiting-lead", Runtime: "codex"}, f.by); err != nil {
		t.Fatal(err)
	}
	p.exec(t, `INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,revision,state) VALUES(?,?,?,?,1,'running')`, f.task.ID, p.waitingItem.ID, p.waitingLead.ID, p.waitingLead.RunID)
}

func (f *fixture) waitProject(t *testing.T) *waitProject {
	t.Helper()
	p := &waitProject{}
	var err error
	if p.handler, err = f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "db-handler", Role: api.AgentRoleDatabaseHandler, Host: "h", Session: "db-handler", Runtime: "codex"}, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{AgentID: p.handler.ID, RunID: p.handler.RunID, Kind: api.EventRunning}, f.by); err != nil {
		t.Fatal(err)
	}
	if p.helper, err = f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "owner-helper", Host: "h", Session: "owner-helper", Runtime: "codex"}, f.by); err != nil {
		t.Fatal(err)
	}
	queue := func(key string) (api.WorkItem, api.TeamQueueEntry) {
		t.Helper()
		item, err := f.st.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Shares a path " + key, RequestID: key}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		order, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "bounded order " + key, RequestID: key + "-order", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.ConfirmWorkOrderScope(f.ctx, f.task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: key + "-scope", AgentID: p.handler.ID, RunID: p.handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}); err != nil {
			t.Fatal(err)
		}
		q, err := f.st.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: key + "-add", Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "h", Cwd: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return item, q
	}
	p.predItem, p.pred = queue("wait-pred")
	p.waitingItem, p.waiting = queue("wait-waiting")
	if p.db, err = sql.Open("sqlite", f.path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.db.Close() })
	p.exec(t, `UPDATE agents SET role=? WHERE id=?`, api.AgentRoleOwnerHelper, p.helper.ID)
	return p
}

func (p *waitProject) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := p.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) setWait(t *testing.T, p *waitProject, until string) {
	t.Helper()
	if _, err := f.st.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "wait_set", EntryID: p.waiting.ID, WaitOnEntryID: p.pred.ID, WaitPaths: []string{"hub/cmd/tt/tool_ledger_test.go"}, WaitUntil: until,
		HandlerAgentID: p.handler.ID, HandlerRunID: p.handler.RunID}); err != nil {
		t.Fatal(err)
	}
}

// waitSteps runs one Tick and returns its shared-path wait steps.
func (f *fixture) waitSteps(t *testing.T, now time.Time) []string {
	t.Helper()
	steps, err := (&Broker{Store: f.st}).Tick(f.ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range steps {
		if strings.HasPrefix(s.Action, "wait-") {
			if s.TaskID != f.task.ID || s.MessageSeq == 0 {
				t.Fatalf("step %+v", s)
			}
			out = append(out, s.Action)
		}
	}
	return out
}

// waitNotices counts the hub's notices with a subject, by recipient.
func (f *fixture) waitNotices(t *testing.T, subject string) map[string]int {
	t.Helper()
	msgs, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, m := range msgs {
		if m.From.Node == api.BrokerNode && m.Envelope != nil && m.Envelope.Subject == subject {
			out[m.To]++
		}
	}
	return out
}

const (
	waitMetSubject      = "A shared path this team waited on is now free"
	waitOverdueSubject  = "A team is idle after its shared path wait was met"
	waitOrphanedSubject = "A shared path wait needs a handler decision"
)

// wi_44cabd7e6233bc1b a3 and a4: the Tick tells the waiting lead once when
// the predecessor is accepted, across later ticks and a hub restart, and
// reports the team once when it stays idle for the bound.
func TestBrokerTickNotifiesMetWaitOnceAndReportsIdleTeamOnce(t *testing.T) {
	f := newFixture(t)
	p := f.waitProject(t)
	f.setWait(t, p, api.TeamQueueWaitAccepted)
	now := time.Now().UTC()
	want(t, "before the condition", f.waitSteps(t, now))
	p.exec(t, `UPDATE team_queue_entries SET acceptance_json=? WHERE id=?`, `{"commit":"`+strings.Repeat("b", 40)+`"}`, p.pred.ID)
	// The listing shows the recorded state only: still waiting before a tick.
	list, err := f.st.ListTeamQueue(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Entries {
		if e.ID == p.waiting.ID && (len(e.Waits) != 1 || e.Waits[0].State != api.TeamQueueWaitWaiting) {
			t.Fatalf("listing evaluated the wait: %+v", e.Waits)
		}
	}
	// The waiting entry is still queued and has no lead of its own: the
	// project orchestrator is not told in its place (review f1).
	want(t, "met with no lead yet", f.waitSteps(t, now.Add(10*time.Second)))
	if got := f.waitNotices(t, waitMetSubject); len(got) != 0 {
		t.Fatalf("told before the entry had a lead: %v", got)
	}
	f.giveLead(t, p)
	met := now.Add(30 * time.Second)
	want(t, "at the condition", f.waitSteps(t, met), "wait-met")
	for i := 1; i <= 5; i++ {
		want(t, "a later tick", f.waitSteps(t, met.Add(time.Duration(i)*30*time.Second)))
	}
	f.restart(t)
	for i := 6; i <= 10; i++ {
		want(t, "a tick after restart", f.waitSteps(t, met.Add(time.Duration(i)*30*time.Second)))
	}
	if got := f.waitNotices(t, waitMetSubject); len(got) != 1 || got[p.waitingLead.ID] != 1 {
		t.Fatalf("met notices by recipient %v, want one to the waiting entry's lead", got)
	}

	want(t, "one second before the bound", f.waitSteps(t, met.Add(15*time.Minute-time.Second)))
	want(t, "at the bound", f.waitSteps(t, met.Add(15*time.Minute)), "wait-overdue")
	f.restart(t)
	for i := 1; i <= 10; i++ {
		want(t, "after the report", f.waitSteps(t, met.Add(15*time.Minute+time.Duration(i)*time.Minute)))
	}
	if got := f.waitNotices(t, waitOverdueSubject); len(got) != 2 || got[p.helper.ID] != 1 || got[p.handler.ID] != 1 {
		t.Fatalf("overdue notices by recipient %v, want one each to the owner helper and the handler", got)
	}
	if got := f.waitNotices(t, waitMetSubject); len(got) != 1 || got[p.waitingLead.ID] != 1 {
		t.Fatalf("met notices %v", got)
	}
}

// a3: done and released each produce one wait-met step.
func TestBrokerTickNotifiesDoneAndReleasedWaits(t *testing.T) {
	for until, meet := range map[string]func(*testing.T, *fixture, *waitProject){
		api.TeamQueueWaitDone: func(t *testing.T, f *fixture, p *waitProject) {
			p.exec(t, `UPDATE work_items SET status='done' WHERE task_id=? AND id=?`, f.task.ID, p.predItem.ID)
		},
		api.TeamQueueWaitReleased: func(t *testing.T, f *fixture, p *waitProject) {
			p.exec(t, `INSERT INTO release_jobs(task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,'released',1,?)`, f.task.ID, api.NewID("rel"), p.pred.ID, `{"integratedCommit":"`+strings.Repeat("c", 40)+`"}`)
		},
	} {
		t.Run(until, func(t *testing.T) {
			f := newFixture(t)
			p := f.waitProject(t)
			f.giveLead(t, p)
			f.setWait(t, p, until)
			now := time.Now().UTC()
			want(t, "before the condition", f.waitSteps(t, now))
			meet(t, f, p)
			want(t, "at the condition", f.waitSteps(t, now.Add(30*time.Second)), "wait-met")
			f.restart(t)
			for i := 2; i <= 11; i++ {
				want(t, "a later tick", f.waitSteps(t, now.Add(time.Duration(i)*30*time.Second)))
			}
			if got := f.waitNotices(t, waitMetSubject); len(got) != 1 || got[p.waitingLead.ID] != 1 {
				t.Fatalf("met notices by recipient %v, want one to the waiting entry's lead", got)
			}
		})
	}
}

// a5: a predecessor removed from the queue is reported once to the primary
// handler; the waiting lead is told nothing.
func TestBrokerTickReportsOrphanedWaitToHandlerOnce(t *testing.T) {
	f := newFixture(t)
	p := f.waitProject(t)
	f.giveLead(t, p)
	f.setWait(t, p, api.TeamQueueWaitAccepted)
	if _, err := f.st.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: "remove-pred", Operation: "remove", EntryID: p.pred.ID, ExpectedRevision: p.pred.Revision}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	want(t, "after the removal", f.waitSteps(t, now), "wait-orphaned")
	f.restart(t)
	for i := 1; i <= 10; i++ {
		want(t, "a later tick", f.waitSteps(t, now.Add(time.Duration(i)*time.Minute)))
	}
	if got := f.waitNotices(t, waitOrphanedSubject); len(got) != 1 || got[p.handler.ID] != 1 {
		t.Fatalf("orphan notices by recipient %v, want one to the handler", got)
	}
	if got := f.waitNotices(t, waitMetSubject); len(got) != 0 {
		t.Fatalf("the lead was told of an unmet wait: %v", got)
	}
}

// a6: with no wait recorded, a Tick over a project whose entry is accepted,
// done and released takes no wait step and posts no wait notice.
func TestBrokerTickWithoutAWaitPostsNothing(t *testing.T) {
	f := newFixture(t)
	p := f.waitProject(t)
	p.exec(t, `UPDATE team_queue_entries SET acceptance_json=? WHERE id=?`, `{"commit":"`+strings.Repeat("b", 40)+`"}`, p.pred.ID)
	p.exec(t, `UPDATE work_items SET status='done' WHERE task_id=? AND id=?`, f.task.ID, p.predItem.ID)
	p.exec(t, `INSERT INTO release_jobs(task_id,id,entry_id,state,generation,record_json) VALUES(?,?,?,'released',1,'{}')`, f.task.ID, api.NewID("rel"), p.pred.ID)
	var before int
	if err := p.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		want(t, "a tick without a wait", f.waitSteps(t, now.Add(time.Duration(i)*20*time.Minute)))
	}
	var after, rows int
	if err := p.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := p.db.QueryRow(`SELECT count(*) FROM team_queue_waits`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if after != before || rows != 0 {
		t.Fatalf("%d new messages and %d wait rows without a wait", after-before, rows)
	}
}

// a8 (wi_fa5f10477f93c57d): a Tick runs the stall watch sweep once and
// returns one step per notice it posted; a sweep error is kept as the first
// error and the obligation pass still runs.
func TestBrokerTickRunsStallWatchSweep(t *testing.T) {
	// 22:30 Pacific, so the first notices are night lines and 07:00 pings.
	start := time.Date(2026, 10, 13, 5, 30, 0, 0, time.UTC)
	sevenAM := time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC)
	setup := func(t *testing.T, at time.Time) (*fixture, *sql.DB, api.Agent) {
		t.Helper()
		f := newFixture(t)
		f.st.SetClockForTest(func() time.Time { return at })
		out, err := f.st.RegisterOwnerHelper(f.ctx, f.task.ID, api.RegisterOwnerHelperRequest{Host: "owner-host", Session: "owner", Runtime: "claude", Cwd: "/work/tailterm", RequestID: "stall-helper"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		// The owner asks the helper for something; the helper stays silent.
		if _, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{To: out.Agent.ID, Text: "Owner asks the helper for a status"}, f.by); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", f.path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return f, db, *out.Agent
	}
	stall := func(t *testing.T, f *fixture, at time.Time) []Step {
		t.Helper()
		steps, err := (&Broker{Store: f.st}).Tick(f.ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		var out []Step
		for _, s := range steps {
			if strings.HasPrefix(s.Action, "helper-quiet") || strings.HasPrefix(s.Action, "team-quiet") {
				out = append(out, s)
			}
		}
		return out
	}
	actions := func(steps []Step) (out []string) {
		for _, s := range steps {
			out = append(out, s.Action)
		}
		return out
	}
	t.Run("one step per notice", func(t *testing.T) {
		f, db, _ := setup(t, start)
		// A running queue entry whose one member, the builder, is silent. The
		// rows go into this test's own database file.
		item, err := f.st.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Quiet team", RequestID: "stall-item"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		at := start.Format(time.RFC3339Nano)
		for _, stmt := range []struct {
			query string
			args  []any
		}{
			{`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,created_at,updated_at,admitted_at) VALUES('tqe_stall',?,?,1,1,'planned',1,'running',?,?,?)`, []any{f.task.ID, item.ID, at, at, at}},
			{`INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,1,0,'digest','{}',?)`,
				[]any{f.builder.ID, f.builder.RunID, f.task.ID, item.ID, f.task.ID, at}},
		} {
			if _, err := db.Exec(stmt.query, stmt.args...); err != nil {
				t.Fatal(err)
			}
		}
		want(t, "19 minutes", actions(stall(t, f, start.Add(19*time.Minute))))
		night := stall(t, f, start.Add(30*time.Minute))
		want(t, "23:00", actions(night), "helper-quiet", "team-quiet")
		want(t, "23:00 again", actions(stall(t, f, start.Add(30*time.Minute))))
		want(t, "06:59", actions(stall(t, f, sevenAM.Add(-time.Minute))))
		day := stall(t, f, sevenAM)
		want(t, "07:00", actions(day), "helper-quiet-ping", "team-quiet-ping")
		want(t, "07:00 again", actions(stall(t, f, sevenAM)))
		// Each step names the notice it posted.
		msgs, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 500)
		if err != nil {
			t.Fatal(err)
		}
		notices := map[int64]*api.Envelope{}
		for _, m := range msgs {
			if m.From.Node == api.BrokerNode && m.Envelope != nil && m.Envelope.Refs["stallWatch"] != "" {
				notices[m.Seq] = m.Envelope
			}
		}
		if len(notices) != 4 {
			t.Fatalf("%d stall watch notices, want 4", len(notices))
		}
		for i, s := range append(night, day...) {
			env := notices[s.MessageSeq]
			escalation := "lead"
			if i >= 2 {
				escalation = "owner"
			}
			if env == nil || s.TaskID != f.task.ID || env.Refs["escalation"] != escalation || !strings.HasPrefix(s.Action, env.Refs["stallWatch"]+"-quiet") {
				t.Fatalf("step %+v does not name its notice %+v", s, env)
			}
		}
	})
	t.Run("a paused project returns none", func(t *testing.T) {
		f, db, _ := setup(t, start.Add(14*time.Hour)) // 12:30 Pacific
		at := start.Add(14*time.Hour + 20*time.Minute)
		if _, err := db.Exec(`UPDATE tasks SET pause_state='paused' WHERE id=?`, f.task.ID); err != nil {
			t.Fatal(err)
		}
		want(t, "paused", actions(stall(t, f, at)))
		if _, err := db.Exec(`UPDATE tasks SET pause_state='active' WHERE id=?`, f.task.ID); err != nil {
			t.Fatal(err)
		}
		want(t, "active again", actions(stall(t, f, at)), "helper-quiet")
	})
	t.Run("a sweep error is the first error and obligations still run", func(t *testing.T) {
		f, db, _ := setup(t, start.Add(14*time.Hour))
		o := f.assign(t, "")
		if _, err := db.Exec(`ALTER TABLE stall_watch_episodes RENAME TO stall_watch_episodes_gone`); err != nil {
			t.Fatal(err)
		}
		steps, err := (&Broker{Store: f.st}).Tick(f.ctx, start.Add(14*time.Hour+2*time.Minute))
		if err == nil || !strings.Contains(err.Error(), "stall watch sweep") {
			t.Fatalf("tick error %v, want the stall watch sweep's", err)
		}
		var woke bool
		for _, s := range steps {
			woke = woke || (s.ObligationID == o.ID && s.Action == "wake")
		}
		if !woke {
			t.Fatalf("the obligation pass did not run: %+v", steps)
		}
	})
}

// ownerPingRun drives one or more assignments (due 45m, acknowledged at +1m)
// through the real Tick every 30 s for 4 h. progress maps a holder index to
// the times it records progress: a NOTICE reply to its assignment, or `tt
// progress` when tt is set. extendAt, if set, extends holder 0's obligation
// by 30 minutes as the owner. It returns, per holder, the times of owner
// escalations for its obligation and of stall notices that list it: every
// broker notice the bridge pings the owner for (wi_052f25d52d8b8f56).
func ownerPingRun(t *testing.T, holders int, tt bool, progress map[int][]time.Duration, extendAt time.Duration) (owner, stall [][]time.Duration) {
	t.Helper()
	f := newFixture(t)
	c := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	clock := c
	f.st.SetClockForTest(func() time.Time { return clock })
	agents := []api.Agent{f.builder}
	for i := 1; i < holders; i++ {
		a, err := f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: fmt.Sprintf("builder%d", i+1), Host: "h", Session: fmt.Sprintf("builder%d", i+1), Runtime: "codex"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, a)
	}
	msgs := make([]api.Message, holders)
	for i, a := range agents {
		m, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: a.ID, Envelope: &api.Envelope{
			Kind: "assign", To: a.Name, Subject: "Report progress on the owner notices", Due: "45m",
			Body: api.EnvelopeBody{Objective: "x", Owns: []string{"a.go"}, Acceptance: map[string]string{"a1": "x"}}}}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		msgs[i] = m
	}
	clock = c.Add(time.Minute)
	for i, a := range agents {
		if _, err := f.st.ObligationAction(f.ctx, f.task.ID, msgs[i].Seq, "ack", api.ObligationActionRequest{AgentID: a.ID, RunID: a.RunID}, clock); err != nil {
			t.Fatal(err)
		}
	}
	b := &Broker{Store: f.st}
	for d := 2 * time.Minute; d <= 4*time.Hour; d += 30 * time.Second {
		clock = c.Add(d)
		for i, a := range agents {
			for _, at := range progress[i] {
				if at != d {
					continue
				}
				if tt {
					if _, err := f.st.ObligationAction(f.ctx, f.task.ID, msgs[i].Seq, "progress", api.ObligationActionRequest{AgentID: a.ID, RunID: a.RunID}, clock); err != nil {
						t.Fatal(err)
					}
				} else if _, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: a.ID, RunID: a.RunID, ReplyTo: msgs[i].Seq, Envelope: &api.Envelope{
					Kind: api.EnvelopeKindNotice, Subject: "Progress on the assigned work", Body: api.EnvelopeBody{Text: "still going"}}}, f.by); err != nil {
					t.Fatal(err)
				}
			}
		}
		if extendAt != 0 && d == extendAt {
			obls, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{OpenOnly: true}, clock)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range obls {
				if o.MessageSeq == msgs[0].Seq {
					if _, err := f.st.ExtendObligation(f.ctx, f.task.ID, o.ID, api.ObligationExtendRequest{For: "30m", Reason: "more time", RequestID: "extend-1"}, f.by); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		if _, err := b.Tick(f.ctx, clock); err != nil {
			t.Fatal(err)
		}
	}
	all, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", 2000)
	if err != nil {
		t.Fatal(err)
	}
	owner, stall = make([][]time.Duration, holders), make([][]time.Duration, holders)
	for i := range msgs {
		owner[i], stall[i] = []time.Duration{}, []time.Duration{}
	}
	for _, x := range all {
		if x.From.Node != api.BrokerNode || x.Envelope == nil {
			continue
		}
		for i := range msgs {
			switch x.Envelope.Refs["escalation"] {
			case "owner":
				if x.Envelope.Refs["message"] == fmt.Sprint(msgs[i].Seq) && x.Envelope.Refs["obligation"] != "" {
					owner[i] = append(owner[i], x.CreatedAt.Sub(c))
				}
			case "stall":
				if strings.Contains(x.Envelope.Body.Text, fmt.Sprintf("#%d ", msgs[i].Seq)) {
					stall[i] = append(stall[i], x.CreatedAt.Sub(c))
				}
			}
		}
	}
	return owner, stall
}

func every(from, to, step time.Duration) []time.Duration {
	var out []time.Duration
	for d := from; d <= to; d += step {
		out = append(out, d)
	}
	return out
}

func durs(s ...string) []time.Duration {
	out := []time.Duration{}
	for _, v := range s {
		d, err := time.ParseDuration(v)
		if err != nil {
			panic(err)
		}
		out = append(out, d)
	}
	return out
}

// p1, p4 and p5 (wi_052f25d52d8b8f56): once an obligation reached the owner,
// progress from its holder, a message or `tt progress`, brings no further
// owner ping for it, counting owner escalations and the stall notices that
// list it. The assignment is due at 45m; the lead hears at 45m30s, the owner
// at 1h0m30s, and the cap ends at 2h45m. A holder that goes silent after its
// last progress is listed in a stall once, 2 h later. The base column is what
// d4bb545 (before the fix) sent for the same run; no row exceeds it.
func TestProgressAfterOwnerEscalationPingsTheOwnerOnce(t *testing.T) {
	m := time.Minute
	capEnd := 45*m + store.ObligationProgressCap
	cases := []struct {
		name         string
		tt           bool
		at           []time.Duration
		owner, stall []time.Duration
		base         int
	}{
		{"message every 30m", false, []time.Duration{65 * m, 95 * m, 125 * m, 155 * m}, durs("1h0m30s"), durs(), 3},
		{"message after each miss", false, []time.Duration{62 * m, 109 * m, 156 * m}, durs("1h0m30s"), durs(), 5},
		{"tt progress after each miss", true, []time.Duration{62 * m, 108 * m, 154 * m, 200 * m}, durs("1h0m30s"), durs(), 4},
		{"message every 20m", false, every(62*m, 162*m, 20*m), durs("1h0m30s"), durs(), 3},
		{"tt progress every 16m", true, every(62*m, 240*m, 16*m), durs("1h0m30s"), durs(), 1},
		{"message every 16m", false, every(62*m, 240*m, 16*m), durs("1h0m30s"), durs(), 5},
		{"tt progress every 30m", true, every(62*m, 240*m, 30*m), durs("1h0m30s"), durs(), 1},
		{"tt progress once", true, []time.Duration{62 * m}, durs("1h0m30s"), durs("3h2m"), 3},
		{"message once", false, []time.Duration{62 * m}, durs("1h0m30s"), durs("3h2m"), 3},
		{"no progress", false, nil, durs("1h0m30s"), durs("1h15m30s"), 2},
	}
	for _, k := range cases {
		t.Run(k.name, func(t *testing.T) {
			owner, stall := ownerPingRun(t, 1, k.tt, map[int][]time.Duration{0: k.at}, 0)
			t.Logf("%s: total=%d owner=%v stall=%v", k.name, len(owner[0])+len(stall[0]), owner[0], stall[0])
			if !reflect.DeepEqual(owner[0], k.owner) || !reflect.DeepEqual(stall[0], k.stall) {
				t.Errorf("owner %v stall %v, want owner %v stall %v", owner[0], stall[0], k.owner, k.stall)
			}
			if total := len(owner[0]) + len(stall[0]); total > k.base {
				t.Errorf("%d owner pings, more than the base's %d", total, k.base)
			}
			if len(k.at) == 0 {
				return
			}
			inCap := 0
			for _, d := range append(append([]time.Duration{}, owner[0]...), stall[0]...) {
				if d <= capEnd {
					inCap++
				}
			}
			if inCap != 1 {
				t.Errorf("%d owner pings inside the cap, want 1", inCap)
			}
			// A stall listing it comes only once the holder has been
			// silent for the cap.
			last := k.at[len(k.at)-1]
			for _, d := range stall[0] {
				if d-last < store.ObligationProgressCap {
					t.Errorf("stall at %v, %v after the last progress at %v", d, d-last, last)
				}
			}
		})
	}
}

// Progress before the owner escalation still clears the lead step, and an
// owner extension still re-bases an owner-level obligation: both send the
// owner exactly what d4bb545 did (wi_052f25d52d8b8f56 p1, p4).
func TestProgressBeforeOwnerAndExtensionKeepTheirPings(t *testing.T) {
	m := time.Minute
	for _, k := range []struct {
		tt           bool
		owner, stall string
	}{{false, "1h35m30s", "1h50m30s"}, {true, "1h35m", "1h50m"}} {
		t.Run(fmt.Sprintf("progress at 50m, tt=%v", k.tt), func(t *testing.T) {
			owner, stall := ownerPingRun(t, 1, k.tt, map[int][]time.Duration{0: {50 * m}}, 0)
			t.Logf("progress at 50m, tt=%v: owner=%v stall=%v", k.tt, owner[0], stall[0])
			if want := durs(k.owner); !reflect.DeepEqual(owner[0], want) {
				t.Errorf("owner escalations %v, want %v (none at 1h0m30s)", owner[0], want)
			}
			if want := durs(k.stall); !reflect.DeepEqual(stall[0], want) {
				t.Errorf("stalls %v, want %v", stall[0], want)
			}
		})
	}
	t.Run("owner extension at 2h6m, then silence", func(t *testing.T) {
		owner, stall := ownerPingRun(t, 1, false, map[int][]time.Duration{0: {65 * m, 95 * m, 125 * m}}, 126*m)
		t.Logf("owner extension: owner=%v stall=%v", owner[0], stall[0])
		if want := durs("1h0m30s", "2h51m30s"); !reflect.DeepEqual(owner[0], want) {
			t.Errorf("owner escalations %v, want %v", owner[0], want)
		}
		if want := durs("3h6m30s"); !reflect.DeepEqual(stall[0], want) {
			t.Errorf("stalls %v, want %v", stall[0], want)
		}
	})
}

// A stall still lists a silent owner-level obligation while it leaves out
// one in the same project whose holder keeps reporting (wi_052f25d52d8b8f56).
func TestStallListsTheSilentObligationOnly(t *testing.T) {
	owner, stall := ownerPingRun(t, 2, false, map[int][]time.Duration{0: every(65*time.Minute, 4*time.Hour, 30*time.Minute)}, 0)
	t.Logf("two holders: reporting owner=%v stall=%v; silent owner=%v stall=%v", owner[0], stall[0], owner[1], stall[1])
	if want := durs("1h0m30s"); !reflect.DeepEqual(owner[0], want) || len(stall[0]) != 0 {
		t.Errorf("reporting holder: owner %v stall %v, want owner %v and no stall", owner[0], stall[0], want)
	}
	if len(owner[1]) != 1 || len(stall[1]) == 0 {
		t.Errorf("silent holder: owner %v stall %v, want one owner escalation and a stall", owner[1], stall[1])
	}
}
