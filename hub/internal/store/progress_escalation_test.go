package store

import (
	"context"
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
