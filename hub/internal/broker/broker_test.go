package broker

import (
	"context"
	"path/filepath"
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
	f.lead, f.builder = add("lead"), add("builder")
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
		Kind: "assign", To: "builder", Subject: "Reject the empty recipient in tt post", Due: due,
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

// b5 and b10: wakes at 1/3/7 min, lead at 10 min, owner 15 min later, one
// project-stall notice (only once the lead has been escalated and 15 quiet
// minutes have passed since), each exactly once, across a restart.
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
	want(t, "+25m1s", f.tick(t, o, c.Add(25*time.Minute+time.Second)), "escalate-owner", "project-stalled")
	want(t, "+40m", f.tick(t, o, c.Add(40*time.Minute)))
	if lead, owner := f.brokerNotices(t); lead != 1 || owner != 1 {
		t.Fatalf("after owner escalation: lead %d owner %d", lead, owner)
	}
	// Recipient activity re-arms the stall notice; it fires again only after a
	// new quiet period with overdue work.
	if _, err := f.st.ObligationAction(f.ctx, f.task.ID, o.MessageSeq, "ack", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, c.Add(41*time.Minute)); err != nil {
		t.Fatal(err)
	}
	want(t, "+50m", f.tick(t, o, c.Add(50*time.Minute)))
	want(t, "+71m1s", f.tick(t, o, c.Add(71*time.Minute+time.Second)), "nudge", "project-stalled")
	want(t, "+75m", f.tick(t, o, c.Add(75*time.Minute)))
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
