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
	// for the bound before it counts as waiting.
	if n := tick(start); n != 0 {
		t.Fatalf("first sight: %d steps", n)
	}
	if n := tick(start.Add(9 * time.Minute)); n != 0 {
		t.Fatalf("inside the bound: %d steps", n)
	}
	past := start.Add(11 * time.Minute)
	if n := tick(past); n != 2 {
		t.Fatalf("past the bound: %d deployer-silent steps, want 2", n)
	}
	if h, o := copies(); h != 1 || o != 1 {
		t.Fatalf("copies: handler %d, owner helper %d", h, o)
	}
	if n := tick(past); n != 0 {
		t.Fatalf("second tick: %d steps", n)
	}
	if n := tick(past.Add(30 * time.Minute)); n != 0 {
		t.Fatalf("later tick: %d steps", n)
	}
	if h, o := copies(); h != 1 || o != 1 {
		t.Fatalf("copies after later ticks: handler %d, owner helper %d", h, o)
	}
}
