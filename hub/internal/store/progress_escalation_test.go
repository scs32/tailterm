package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Progress-aware escalation (wi_e76872a6abc6d78e): a NOTICE or RESULT from an
// obligation's holder that references it postpones its due time.

type progressFixture struct {
	s                    *Store
	path                 string
	ctx                  context.Context
	by                   api.Caller
	task                 api.Task
	lead, builder, other api.Agent
	c, clock             time.Time
}

// newProgressFixture opens one store per test; opening dominates the race
// suite's time, so subtests share it and each takes a fresh project.
func newProgressFixture(t *testing.T) *progressFixture {
	t.Helper()
	f := &progressFixture{path: filepath.Join(t.TempDir(), "hub.sqlite"), ctx: context.Background(), by: api.Caller{Node: "test", User: "owner"}}
	f.c = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	f.open(t)
	t.Cleanup(func() { f.s.Close() })
	return f
}

func (f *progressFixture) open(t *testing.T) {
	t.Helper()
	s, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetClockForTest(func() time.Time { return f.clock })
	f.s = s
}

// fresh starts a new project a day after the previous one, with a lead, a
// builder and another agent, and sets the clock to its start.
func (f *progressFixture) fresh(t *testing.T) *progressFixture {
	t.Helper()
	if f.task.ID != "" {
		f.c = f.c.Add(24 * time.Hour)
	}
	f.clock = f.c
	var err error
	if f.task, err = f.s.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Progress", Orchestrator: "lead"}, f.by); err != nil {
		t.Fatal(err)
	}
	add := func(name string) api.Agent {
		a, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: name, Host: "h", Session: name, Runtime: "codex"}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	f.lead, f.builder, f.other = add("lead"), add("builder"), add("other")
	return f
}

func (f *progressFixture) at(d time.Duration) { f.clock = f.c.Add(d) }

// assign posts a lead assignment to the agent at the current clock.
func (f *progressFixture) assign(t *testing.T, to api.Agent, due string) api.Obligation {
	t.Helper()
	m, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: to.ID, Envelope: &api.Envelope{
		Kind: "assign", To: to.Name, Subject: "Make progress postpone the escalation", Due: due,
		Body: api.EnvelopeBody{Objective: "postpone", Owns: []string{"broker.go"}, Acceptance: map[string]string{"a1": "postpones"}}}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return f.obligation(t, m.Seq, to)
}

func (f *progressFixture) obligation(t *testing.T, seq int64, holder api.Agent) api.Obligation {
	t.Helper()
	o, err := scanObligation(f.s.db.QueryRow(`SELECT `+obligationCols+` FROM obligations WHERE task_id=? AND message_seq=? AND agent_id=?`, f.task.ID, seq, holder.ID))
	if err != nil {
		t.Fatalf("obligation for #%d: %v", seq, err)
	}
	return o
}

func (f *progressFixture) ack(t *testing.T, o api.Obligation, holder api.Agent, d time.Duration) {
	t.Helper()
	if _, err := f.s.ObligationAction(f.ctx, f.task.ID, o.MessageSeq, "ack", api.ObligationActionRequest{AgentID: holder.ID, RunID: holder.RunID}, f.c.Add(d)); err != nil {
		t.Fatal(err)
	}
}

func progressNotice(text string) *api.Envelope {
	return &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "Progress on the assigned work", Body: api.EnvelopeBody{Text: text}}
}

func progressResult() *api.Envelope {
	return &api.Envelope{Kind: api.EnvelopeKindResult, Subject: "Result for the assigned work", Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}},
		Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "go test ./internal/store", Outcome: "ok"}}}
}

// post sends env from one agent at the current clock.
func (f *progressFixture) post(t *testing.T, from api.Agent, to api.Agent, replyTo int64, refs map[string]string, env *api.Envelope) api.Message {
	t.Helper()
	env.Refs = refs
	req := api.PostMessageRequest{AgentID: from.ID, RunID: from.RunID, ReplyTo: replyTo, Envelope: env}
	if to.ID != "" {
		req.To, env.To = to.ID, to.Name
	}
	m, err := f.s.PostMessage(f.ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatalf("post %s: %v", env.Kind, err)
	}
	return m
}

type postponement struct {
	base, upTo time.Time
	count      int
}

func (f *progressFixture) postponement(t *testing.T, o api.Obligation) (postponement, bool) {
	t.Helper()
	var base, upTo string
	var p postponement
	err := f.s.db.QueryRow(`SELECT base_due_at,postponed_to,count FROM obligation_postponements WHERE obligation_id=?`, o.ID).Scan(&base, &upTo, &p.count)
	if err != nil {
		return p, false
	}
	p.base, p.upTo = parseTS(base), parseTS(upTo)
	return p, true
}

func wantDue(t *testing.T, label string, o api.Obligation, want time.Time) {
	t.Helper()
	if !o.DueAt.Equal(want) {
		t.Fatalf("%s: due %s, want %s", label, o.DueAt.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// a2 (e1): a NOTICE that is not a reply but names the request in refs, and a
// RESULT that names the obligation, both postpone; a RESULT reply still
// closes the obligation with its outcome.
func TestReferencedProgressPostpones(t *testing.T) {
	shared := newProgressFixture(t)
	for _, tc := range []struct {
		name string
		refs func(o api.Obligation) map[string]string
		env  func() *api.Envelope
	}{
		{"notice refs request=#SEQ", func(o api.Obligation) map[string]string {
			return map[string]string{"request": fmt.Sprintf("#%d", o.MessageSeq)}
		}, func() *api.Envelope { return progressNotice("halfway") }},
		{"notice refs bare SEQ", func(o api.Obligation) map[string]string {
			return map[string]string{"message": fmt.Sprint(o.MessageSeq)}
		}, func() *api.Envelope { return progressNotice("halfway") }},
		{"result refs obligation=ID", func(o api.Obligation) map[string]string { return map[string]string{"obligation": o.ID} }, progressResult},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shared.fresh(t)
			o := f.assign(t, f.builder, "45m")
			f.ack(t, o, f.builder, time.Minute)
			f.at(40 * time.Minute)
			f.post(t, f.builder, f.lead, 0, tc.refs(o), tc.env())
			got := f.obligation(t, o.MessageSeq, f.builder)
			wantDue(t, tc.name, got, f.c.Add(70*time.Minute))
			if got.State != api.ObligationWorking || got.LastProgressAt == nil || !got.LastProgressAt.Equal(f.c.Add(40*time.Minute)) {
				t.Fatalf("state %s progress %v", got.State, got.LastProgressAt)
			}
			if p, ok := f.postponement(t, o); !ok || !p.base.Equal(f.c.Add(45*time.Minute)) || !p.upTo.Equal(got.DueAt) || p.count != 1 {
				t.Fatalf("side row %+v %v", p, ok)
			}
		})
	}
	t.Run("result reply still closes", func(t *testing.T) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "45m")
		f.ack(t, o, f.builder, time.Minute)
		f.at(40 * time.Minute)
		m := f.post(t, f.builder, api.Agent{}, o.MessageSeq, nil, progressResult())
		got := f.obligation(t, o.MessageSeq, f.builder)
		if got.State != api.ObligationClosed || got.Outcome != api.OutcomeResult || got.OutcomeSeq != m.Seq {
			t.Fatalf("reply did not close: %+v", got)
		}
	})
}

// a3 (e1 negative): only the holder's current run, a matching reference, a
// NOTICE or RESULT, and an open non-delivery obligation count.
func TestProgressMessageMatching(t *testing.T) {
	shared := newProgressFixture(t)
	for _, tc := range []struct {
		name string
		send func(t *testing.T, f *progressFixture, o api.Obligation)
	}{
		{"another agent's reply", func(t *testing.T, f *progressFixture, o api.Obligation) {
			f.post(t, f.other, api.Agent{}, o.MessageSeq, nil, progressNotice("not mine"))
		}},
		{"another agent's refs", func(t *testing.T, f *progressFixture, o api.Obligation) {
			f.post(t, f.other, f.lead, 0, map[string]string{"obligation": o.ID, "request": fmt.Sprint(o.MessageSeq)}, progressNotice("not mine"))
		}},
		{"missing run", func(t *testing.T, f *progressFixture, o api.Obligation) {
			env := progressNotice("no run")
			if _, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.builder.ID, ReplyTo: o.MessageSeq, Envelope: env}, f.by); err != nil {
				t.Fatal(err)
			}
		}},
		{"stale run", func(t *testing.T, f *progressFixture, o api.Obligation) {
			stale := f.builder
			stale.RunID = "run_0000000000000000"
			f.post(t, stale, api.Agent{}, o.MessageSeq, nil, progressNotice("old run"))
		}},
		{"different seq", func(t *testing.T, f *progressFixture, o api.Obligation) {
			elsewhere := f.assign(t, f.other, "")
			f.post(t, f.builder, api.Agent{}, elsewhere.MessageSeq, map[string]string{"request": fmt.Sprintf("#%d", elsewhere.MessageSeq)}, progressNotice("about something else"))
		}},
		{"non-numeric ref", func(t *testing.T, f *progressFixture, o api.Obligation) {
			f.post(t, f.builder, f.lead, 0, map[string]string{"request": fmt.Sprintf("#%dx", o.MessageSeq), "note": fmt.Sprintf("message %d", o.MessageSeq), "commit": fmt.Sprintf("%da", o.MessageSeq)}, progressNotice("loose reference"))
		}},
		{"finding", func(t *testing.T, f *progressFixture, o api.Obligation) {
			f.post(t, f.builder, api.Agent{}, o.MessageSeq, nil, &api.Envelope{Kind: api.EnvelopeKindFinding, Subject: "A finding about the work", Body: api.EnvelopeBody{Severity: "low", Summary: "seen"},
				Evidence: map[string]api.Evidence{"e1": {Type: "file", Value: "broker.go"}}})
		}},
		{"question", func(t *testing.T, f *progressFixture, o api.Obligation) {
			f.post(t, f.builder, api.Agent{}, o.MessageSeq, nil, &api.Envelope{Kind: api.EnvelopeKindQuestion, Subject: "A question about the work", Body: api.EnvelopeBody{Question: "Which grace applies?"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := shared.fresh(t)
			o := f.assign(t, f.builder, "45m")
			f.ack(t, o, f.builder, time.Minute)
			f.at(40 * time.Minute)
			tc.send(t, f, o)
			got := f.obligation(t, o.MessageSeq, f.builder)
			wantDue(t, tc.name, got, f.c.Add(45*time.Minute))
			if got.State != api.ObligationAcknowledged || !got.LastProgressAt.Equal(f.c.Add(time.Minute)) {
				t.Fatalf("%s: state %s progress %v", tc.name, got.State, got.LastProgressAt)
			}
			if _, ok := f.postponement(t, o); ok {
				t.Fatalf("%s: side row written", tc.name)
			}
		})
	}
	t.Run("closed obligation", func(t *testing.T) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "45m")
		f.ack(t, o, f.builder, time.Minute)
		f.at(20 * time.Minute)
		f.post(t, f.builder, api.Agent{}, o.MessageSeq, nil, progressResult())
		closed := f.obligation(t, o.MessageSeq, f.builder)
		f.at(40 * time.Minute)
		f.post(t, f.builder, f.lead, o.MessageSeq, map[string]string{"obligation": o.ID}, progressNotice("after the result"))
		got := f.obligation(t, o.MessageSeq, f.builder)
		if !got.DueAt.Equal(closed.DueAt) || !got.LastProgressAt.Equal(*closed.LastProgressAt) || got.State != api.ObligationClosed {
			t.Fatalf("closed obligation changed: %+v", got)
		}
	})
	t.Run("delivery-only obligation", func(t *testing.T) {
		f := shared.fresh(t)
		m := f.post(t, f.lead, f.builder, 0, nil, progressNotice("for your information"))
		before := f.obligation(t, m.Seq, f.builder)
		if before.Needs != api.ObligationNeedsDelivery {
			t.Fatalf("needs %s", before.Needs)
		}
		f.at(5 * time.Minute)
		f.post(t, f.builder, f.lead, 0, map[string]string{"request": fmt.Sprint(m.Seq), "obligation": before.ID}, progressNotice("noted"))
		got := f.obligation(t, m.Seq, f.builder)
		if !got.DueAt.Equal(before.DueAt) || got.LastProgressAt != nil || got.State != before.State {
			t.Fatalf("delivery-only obligation changed: %+v", got)
		}
		if _, ok := f.postponement(t, before); ok {
			t.Fatal("side row written for a delivery-only obligation")
		}
	})
}

// a8: the cap re-bases after an owner extension and never shortens it; a
// retried post postpones once; the state survives a restart; an existing
// database gains the side table.
func TestPostponement(t *testing.T) {
	shared := newProgressFixture(t)
	t.Run("owner extension re-bases the cap", func(t *testing.T) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "45m")
		f.ack(t, o, f.builder, time.Minute)
		f.at(40 * time.Minute)
		f.post(t, f.builder, api.Agent{}, o.MessageSeq, nil, progressNotice("halfway"))
		wantDue(t, "postponed", f.obligation(t, o.MessageSeq, f.builder), f.c.Add(70*time.Minute))
		f.at(50 * time.Minute)
		if _, err := f.s.ExtendObligation(f.ctx, f.task.ID, o.ID, api.ObligationExtendRequest{For: "3h", Reason: "bigger than planned", RequestID: "extend-1"}, f.by); err != nil {
			t.Fatal(err)
		}
		extended := f.c.Add(230 * time.Minute)
		wantDue(t, "extended", f.obligation(t, o.MessageSeq, f.builder), extended)
		f.at(60 * time.Minute)
		f.post(t, f.builder, api.Agent{}, o.MessageSeq, nil, progressNotice("still going"))
		wantDue(t, "never shortened", f.obligation(t, o.MessageSeq, f.builder), extended)
		// Against the old base the cap would be +165m; the extension re-based it.
		f.at(220 * time.Minute)
		f.post(t, f.builder, api.Agent{}, o.MessageSeq, nil, progressNotice("nearly there"))
		got := f.obligation(t, o.MessageSeq, f.builder)
		wantDue(t, "re-based", got, f.c.Add(250*time.Minute))
		if p, ok := f.postponement(t, o); !ok || !p.base.Equal(extended) || p.count != 1 {
			t.Fatalf("side row %+v %v", p, ok)
		}
	})
	t.Run("a retried post postpones once", func(t *testing.T) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "45m")
		f.ack(t, o, f.builder, time.Minute)
		send := func() api.Message {
			m, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, ReplyTo: o.MessageSeq, RequestID: "progress-1", Envelope: progressNotice("halfway")}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			return m
		}
		f.at(40 * time.Minute)
		first := send()
		f.at(50 * time.Minute)
		if again := send(); again.Seq != first.Seq {
			t.Fatalf("retry posted a new message: %d vs %d", again.Seq, first.Seq)
		}
		got := f.obligation(t, o.MessageSeq, f.builder)
		wantDue(t, "retry", got, f.c.Add(70*time.Minute))
		if !got.LastProgressAt.Equal(f.c.Add(40 * time.Minute)) {
			t.Fatalf("retry recorded progress again: %v", got.LastProgressAt)
		}
		if p, ok := f.postponement(t, o); !ok || p.count != 1 {
			t.Fatalf("side row %+v %v", p, ok)
		}
	})
	t.Run("a restart keeps the state", func(t *testing.T) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "45m")
		f.ack(t, o, f.builder, time.Minute)
		f.at(40 * time.Minute)
		f.post(t, f.builder, api.Agent{}, o.MessageSeq, nil, progressNotice("halfway"))
		f.s.Close()
		f.open(t)
		wantDue(t, "reopened", f.obligation(t, o.MessageSeq, f.builder), f.c.Add(70*time.Minute))
		// The cap still counts from the original due time.
		f.at(150 * time.Minute)
		f.post(t, f.builder, api.Agent{}, o.MessageSeq, nil, progressNotice("late"))
		wantDue(t, "capped after restart", f.obligation(t, o.MessageSeq, f.builder), f.c.Add(165*time.Minute))
		if p, ok := f.postponement(t, o); !ok || !p.base.Equal(f.c.Add(45*time.Minute)) || p.count != 2 {
			t.Fatalf("side row %+v %v", p, ok)
		}
	})
	t.Run("an existing database gains the side table", func(t *testing.T) {
		f := shared.fresh(t)
		if _, err := f.s.db.Exec(`DROP TABLE obligation_postponements`); err != nil {
			t.Fatal(err)
		}
		f.s.Close()
		f.open(t)
		var n int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='obligation_postponements'`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("side table after reopen: %d %v", n, err)
		}
		o := f.assign(t, f.builder, "45m")
		f.ack(t, o, f.builder, time.Minute)
		f.at(40 * time.Minute)
		f.post(t, f.builder, api.Agent{}, o.MessageSeq, nil, progressNotice("halfway"))
		wantDue(t, "migrated", f.obligation(t, o.MessageSeq, f.builder), f.c.Add(70*time.Minute))
	})
}

// ladder runs one escalation pass at the current clock for o, through the
// same store calls and order as the broker's lead and owner steps (package
// broker imports store, so its Tick cannot run here). It returns the step.
// It counts owner escalations only; the Tick tests in internal/broker also
// count the stall notices that list the obligation.
func (f *progressFixture) ladder(t *testing.T, o api.Obligation) string {
	t.Helper()
	open, err := f.s.BrokerOpenObligations(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range open {
		if b.ID != o.ID {
			continue
		}
		overdue := ObligationOverdue(b.Obligation, f.clock)
		if overdue == "" || overdue == "silence" {
			return ""
		}
		level, step := 0, ""
		switch {
		case b.Escalation == 0:
			level, step = 1, "escalate-lead"
		case b.Escalation == 1 && f.clock.Sub(b.EscalatedAt) >= api.ObligationOwnerAfterLead:
			level, step = 2, "escalate-owner"
		default:
			return ""
		}
		err := f.s.BrokerEscalate(f.ctx, b, level, "past its due time", f.clock)
		if errors.Is(err, ErrBrokerProgressFresh) || errors.Is(err, ErrBrokerStale) {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		return step
	}
	return ""
}

// escalationNotices counts the broker's notices about o at a level ("lead"
// or "owner").
func (f *progressFixture) escalationNotices(t *testing.T, o api.Obligation, level string) int {
	t.Helper()
	msgs, err := f.s.ListMessages(f.ctx, f.task.ID, 0, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if m.From.Node == api.BrokerNode && m.Envelope != nil && m.Envelope.Refs["escalation"] == level && m.Envelope.Refs["obligation"] == o.ID {
			n++
		}
	}
	return n
}

// run ticks the ladder every minute from +from to +to, posting holder
// progress replies at the given offsets.
func (f *progressFixture) run(t *testing.T, o api.Obligation, from, to time.Duration, progressAt ...time.Duration) {
	t.Helper()
	f.tick(t, o, from, to, func() { f.post(t, f.builder, api.Agent{}, o.MessageSeq, nil, progressNotice("still going")) }, progressAt)
}

// runTT is run with tt progress (ObligationAction progress) in place of
// progress messages.
func (f *progressFixture) runTT(t *testing.T, o api.Obligation, from, to time.Duration, progressAt ...time.Duration) {
	t.Helper()
	f.tick(t, o, from, to, func() {
		if _, err := f.s.ObligationAction(f.ctx, f.task.ID, o.MessageSeq, "progress", api.ObligationActionRequest{AgentID: f.builder.ID, RunID: f.builder.RunID}, f.clock); err != nil {
			t.Fatal(err)
		}
	}, progressAt)
}

func (f *progressFixture) tick(t *testing.T, o api.Obligation, from, to time.Duration, progress func(), progressAt []time.Duration) {
	t.Helper()
	at := map[time.Duration]bool{}
	for _, d := range progressAt {
		at[d] = true
	}
	for d := from; d <= to; d += time.Minute {
		f.at(d)
		if at[d] {
			progress()
		}
		f.ladder(t, o)
	}
}

// p1 (wi_052f25d52d8b8f56): once an obligation's escalation reached the
// owner, progress inside the 2 h cap no longer re-arms the owner step.
func TestProgressAfterOwnerEscalation(t *testing.T) {
	shared := newProgressFixture(t)
	// Due at +45m: the lead hears at +46m, the owner at +61m.
	start := func(t *testing.T) (*progressFixture, api.Obligation) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "45m")
		f.ack(t, o, f.builder, time.Minute)
		f.run(t, o, 2*time.Minute, 61*time.Minute)
		if n := f.escalationNotices(t, o, "owner"); n != 1 {
			t.Fatalf("owner notices at +61m: %d, want 1", n)
		}
		return f, o
	}
	t.Run("progress every 30 minutes", func(t *testing.T) {
		f, o := start(t)
		f.run(t, o, 62*time.Minute, 4*time.Hour, 65*time.Minute, 95*time.Minute, 125*time.Minute, 155*time.Minute)
		if n := f.escalationNotices(t, o, "owner"); n != 1 {
			t.Fatalf("owner notices: %d, want 1", n)
		}
		got := f.obligation(t, o.MessageSeq, f.builder)
		wantDue(t, "capped", got, f.c.Add(165*time.Minute))
		if got.Escalation != 2 {
			t.Fatalf("escalation %d, want 2", got.Escalation)
		}
	})
	t.Run("progress after each later miss", func(t *testing.T) {
		f, o := start(t)
		// Before the fix each of these restarted the ladder, so the owner heard
		// again at +108m, +155m and +181m.
		f.run(t, o, 62*time.Minute, 4*time.Hour, 62*time.Minute, 109*time.Minute, 156*time.Minute)
		if n := f.escalationNotices(t, o, "owner"); n != 1 {
			t.Fatalf("owner notices: %d, want 1", n)
		}
		if n := f.escalationNotices(t, o, "lead"); n != 1 {
			t.Fatalf("lead notices: %d, want 1", n)
		}
	})
	t.Run("progress still clears the lead step", func(t *testing.T) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "45m")
		f.ack(t, o, f.builder, time.Minute)
		f.run(t, o, 2*time.Minute, 50*time.Minute, 50*time.Minute)
		got := f.obligation(t, o.MessageSeq, f.builder)
		if got.Escalation != 0 || f.escalationNotices(t, o, "lead") != 1 {
			t.Fatalf("after lead step and progress: escalation %d, lead notices %d", got.Escalation, f.escalationNotices(t, o, "lead"))
		}
		// The next miss (due +80m) starts again with the lead.
		f.run(t, o, 51*time.Minute, 81*time.Minute)
		if n := f.escalationNotices(t, o, "lead"); n != 2 {
			t.Fatalf("lead notices after the next miss: %d, want 2", n)
		}
		if n := f.escalationNotices(t, o, "owner"); n != 0 {
			t.Fatalf("owner notices: %d, want 0", n)
		}
	})
	t.Run("an owner extension re-bases it", func(t *testing.T) {
		f, o := start(t)
		f.run(t, o, 62*time.Minute, 70*time.Minute, 65*time.Minute)
		if _, err := f.s.ExtendObligation(f.ctx, f.task.ID, o.ID, api.ObligationExtendRequest{For: "1h", Reason: "more time", RequestID: "extend-owner"}, f.by); err != nil {
			t.Fatal(err)
		}
		// Extended to +130m; progress inside it re-arms the ladder again.
		f.run(t, o, 71*time.Minute, 4*time.Hour, 100*time.Minute)
		if n := f.escalationNotices(t, o, "owner"); n != 2 {
			t.Fatalf("owner notices after the extension: %d, want 2", n)
		}
	})
}

// p4 (wi_052f25d52d8b8f56): tt progress, which never moves the due time,
// no longer re-arms the owner step after an owner escalation either.
func TestTTProgressAfterOwnerEscalation(t *testing.T) {
	shared := newProgressFixture(t)
	start := func(t *testing.T) (*progressFixture, api.Obligation) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "45m")
		f.ack(t, o, f.builder, time.Minute)
		f.runTT(t, o, 2*time.Minute, 61*time.Minute)
		if n := f.escalationNotices(t, o, "owner"); n != 1 {
			t.Fatalf("owner notices at +61m: %d, want 1", n)
		}
		return f, o
	}
	t.Run("across later misses", func(t *testing.T) {
		f, o := start(t)
		// Before the fix each of these restarted the ladder, so the owner heard
		// again at +107m, +153m and +199m.
		f.runTT(t, o, 62*time.Minute, 4*time.Hour, 62*time.Minute, 108*time.Minute, 154*time.Minute, 200*time.Minute)
		if n := f.escalationNotices(t, o, "owner"); n != 1 {
			t.Fatalf("owner notices: %d, want 1", n)
		}
		if got := f.obligation(t, o.MessageSeq, f.builder); got.Escalation != 2 || got.State != api.ObligationWorking {
			t.Fatalf("escalation %d state %s, want 2 working", got.Escalation, got.State)
		}
	})
	t.Run("before the owner step it clears the lead step", func(t *testing.T) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "45m")
		f.ack(t, o, f.builder, time.Minute)
		f.runTT(t, o, 2*time.Minute, 50*time.Minute, 50*time.Minute)
		if got := f.obligation(t, o.MessageSeq, f.builder); got.Escalation != 0 {
			t.Fatalf("escalation %d, want 0", got.Escalation)
		}
		// Still past due: the lead hears again once the progress is 30m old.
		f.runTT(t, o, 51*time.Minute, 81*time.Minute)
		if n := f.escalationNotices(t, o, "lead"); n != 2 {
			t.Fatalf("lead notices: %d, want 2", n)
		}
		if n := f.escalationNotices(t, o, "owner"); n != 0 {
			t.Fatalf("owner notices: %d, want 0", n)
		}
	})
	t.Run("an owner extension re-bases it", func(t *testing.T) {
		f, o := start(t)
		f.at(62 * time.Minute)
		if _, err := f.s.ExtendObligation(f.ctx, f.task.ID, o.ID, api.ObligationExtendRequest{For: "1h", Reason: "more time", RequestID: "extend-tt"}, f.by); err != nil {
			t.Fatal(err)
		}
		f.runTT(t, o, 63*time.Minute, 4*time.Hour, 70*time.Minute)
		if n := f.escalationNotices(t, o, "owner"); n != 2 {
			t.Fatalf("owner notices after the extension: %d, want 2", n)
		}
	})
}

// p2 (wi_052f25d52d8b8f56): postponement rows go when their obligation
// closes, by any close path, and rows left by earlier closes are pruned.
func TestPostponementRowsGoOnClose(t *testing.T) {
	shared := newProgressFixture(t)
	postponed := func(t *testing.T) (*progressFixture, api.Obligation) {
		f := shared.fresh(t)
		o := f.assign(t, f.builder, "45m")
		f.ack(t, o, f.builder, time.Minute)
		f.at(40 * time.Minute)
		f.post(t, f.builder, api.Agent{}, o.MessageSeq, nil, progressNotice("halfway"))
		if _, ok := f.postponement(t, o); !ok {
			t.Fatal("no postponement row")
		}
		return f, o
	}
	wantGone := func(t *testing.T, f *progressFixture, o api.Obligation) {
		t.Helper()
		if got := f.obligation(t, o.MessageSeq, f.builder); got.State != api.ObligationClosed {
			t.Fatalf("state %s, want closed", got.State)
		}
		if p, ok := f.postponement(t, o); ok {
			t.Fatalf("postponement row remains: %+v", p)
		}
	}
	t.Run("reply outcome", func(t *testing.T) {
		f, o := postponed(t)
		f.at(50 * time.Minute)
		f.post(t, f.builder, f.lead, o.MessageSeq, nil, progressResult())
		wantGone(t, f, o)
	})
	t.Run("withdraw", func(t *testing.T) {
		f, o := postponed(t)
		f.at(50 * time.Minute)
		if _, err := f.s.WithdrawObligation(f.ctx, f.task.ID, o.ID, api.ObligationWithdrawRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, Reason: "superseded by a new plan"}); err != nil {
			t.Fatal(err)
		}
		wantGone(t, f, o)
	})
	t.Run("recipient gone", func(t *testing.T) {
		f, o := postponed(t)
		f.at(50 * time.Minute)
		open, err := f.s.BrokerOpenObligations(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		closed := false
		for _, b := range open {
			if b.ID == o.ID {
				b.AgentStatus = api.AgentClosed
				if err := f.s.BrokerCloseRecipientGone(f.ctx, b, f.clock); err != nil {
					t.Fatal(err)
				}
				closed = true
			}
		}
		if !closed {
			t.Fatal("obligation not open")
		}
		wantGone(t, f, o)
	})
	t.Run("rows from earlier closes are pruned on open", func(t *testing.T) {
		f, o := postponed(t)
		// A database from before the trigger: the close leaves the row behind.
		if _, err := f.s.db.Exec(`DROP TRIGGER obligation_postponements_close`); err != nil {
			t.Fatal(err)
		}
		f.at(50 * time.Minute)
		f.post(t, f.builder, f.lead, o.MessageSeq, nil, progressResult())
		if _, ok := f.postponement(t, o); !ok {
			t.Fatal("row already gone without the trigger")
		}
		f.s.Close()
		f.open(t)
		wantGone(t, f, o)
		var n int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='obligation_postponements_close'`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("trigger after reopen: %d %v", n, err)
		}
	})
}
