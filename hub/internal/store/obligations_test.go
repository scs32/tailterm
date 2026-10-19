package store_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/broker"
	"github.com/scs32/tailterm/hub/internal/store"
)

// Item-scoped agent names end in a hex suffix, and a role name may start with
// a digit (wi_3cf8a80864f27f51). A broker reassignment names its target
// literally whether the name starts with a letter or a digit.
var scopedTargetNames = []string{"2nd-reviewer-41b1c632", "reviewer-41b1c632"}

type reassignFixture struct {
	ctx        context.Context
	by         api.Caller
	st         *store.Store
	task       api.Task
	lead, from api.Agent
	clock      time.Time
}

func newReassignFixture(t *testing.T, fromName string) *reassignFixture {
	t.Helper()
	f := &reassignFixture{ctx: context.Background(), by: api.Caller{Node: "test", User: "owner"}, clock: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetClockForTest(func() time.Time { return f.clock })
	f.st = st
	if f.task, err = st.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Reassign", Orchestrator: "lead"}, f.by); err != nil {
		t.Fatal(err)
	}
	f.lead, f.from = f.add(t, "lead"), f.add(t, fromName)
	return f
}

func (f *reassignFixture) add(t *testing.T, name string) api.Agent {
	t.Helper()
	a, err := f.st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{Name: name, Host: "h", Session: name, Runtime: "codex"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// only returns agent's one obligation, failing unless there is exactly one.
func (f *reassignFixture) only(t *testing.T, agent api.Agent) api.Obligation {
	t.Helper()
	obls, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: agent.ID}, f.clock)
	if err != nil || len(obls) != 1 {
		t.Fatalf("%s obligations: %v %+v", agent.Name, err, obls)
	}
	return obls[0]
}

// reassign moves old to target and checks the routing and obligation state
// every reassignment keeps: the old one is superseded, one stays open.
func (f *reassignFixture) reassign(t *testing.T, owner api.Agent, old api.Obligation, target api.Agent) api.Message {
	t.Helper()
	m, err := f.st.ReassignObligation(f.ctx, f.task.ID, old.ID, api.ObligationReassignRequest{ToAgentID: target.ID}, f.by)
	if err != nil {
		t.Fatalf("reassign to %s: %v", target.Name, err)
	}
	if m.To != target.ID || m.Envelope == nil || m.Envelope.To != target.Name {
		t.Fatalf("reassignment routing: %+v", m)
	}
	if prior := f.only(t, owner); prior.State != api.ObligationClosed || prior.Outcome != api.OutcomeSuperseded {
		t.Fatalf("original obligation: %+v", prior)
	}
	if moved := f.only(t, target); moved.State == api.ObligationClosed || moved.MessageSeq != m.Seq {
		t.Fatalf("reissued obligation: %+v", moved)
	}
	return m
}

func TestReassignmentNamesScopedTargetLiterally(t *testing.T) {
	const topic = "Reject the empty recipient in tt post"
	for _, name := range scopedTargetNames {
		t.Run(name, func(t *testing.T) {
			f := newReassignFixture(t, "builder")
			if _, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, To: f.from.ID, Envelope: &api.Envelope{
				Kind: "assign", To: f.from.Name, Subject: topic,
				Body: api.EnvelopeBody{Objective: "fail fast", Owns: []string{"main.go"}, Acceptance: map[string]string{"a1": "exits 2"}}}}, f.by); err != nil {
				t.Fatal(err)
			}
			target := f.add(t, name)
			m := f.reassign(t, f.from, f.only(t, f.from), target)
			if want := "Reassigned to " + name + ": " + topic; m.Envelope.Subject != want {
				t.Fatalf("subject %q, want %q", m.Envelope.Subject, want)
			}
			if !strings.Contains(m.Text, name) {
				t.Fatalf("text lost the target name: %q", m.Text)
			}
		})
	}
}

// A broker escalation names the overdue scoped agent. Moving it to another
// scoped agent keeps the topic, with the former name neutralized.
func TestReassignedBrokerSubjectNeutralizesFormerScopedName(t *testing.T) {
	for i, name := range scopedTargetNames {
		t.Run(name, func(t *testing.T) {
			f := newReassignFixture(t, name)
			if _, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.lead.ID, To: f.from.ID, Envelope: &api.Envelope{
				Kind: "assign", To: f.from.Name, Subject: "Reject the empty recipient in tt post",
				Body: api.EnvelopeBody{Objective: "fail fast", Owns: []string{"main.go"}, Acceptance: map[string]string{"a1": "exits 2"}}}}, f.by); err != nil {
				t.Fatal(err)
			}
			start := f.clock
			for _, at := range []time.Duration{time.Minute, 3 * time.Minute, 7 * time.Minute, 10*time.Minute + time.Second} {
				if _, err := (&broker.Broker{Store: f.st}).Tick(f.ctx, start.Add(at)); err != nil {
					t.Fatal(err)
				}
			}
			escalation := f.only(t, f.lead)
			if !strings.Contains(escalation.Subject, name) {
				t.Fatalf("escalation subject lost the overdue name: %q", escalation.Subject)
			}
			target := f.add(t, scopedTargetNames[1-i])
			m := f.reassign(t, f.lead, escalation, target)
			topic := strings.ReplaceAll(escalation.Subject, name, "agent")
			if want := "Reassigned to " + target.Name + ": " + topic; m.Envelope.Subject != want {
				t.Fatalf("subject %q, want %q", m.Envelope.Subject, want)
			}
		})
	}
}

// The broker exception masks only a trusted name's hex suffix. Public and
// agent-authored validation stay strict, and nothing beside the name is hidden.
func TestBrokerSubjectMasksScopedNames(t *testing.T) {
	notice := func(subject string) *api.PostMessageRequest {
		return &api.PostMessageRequest{Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: subject, Body: api.EnvelopeBody{Text: "reassigned"}}}
	}
	for _, name := range scopedTargetNames {
		subject := "Reassigned to " + name + ": open obligation"
		if err := api.NormalizeBrokerPost(notice(subject), name); err != nil {
			t.Fatalf("broker subject naming %s refused: %v", name, err)
		}
		if err := api.NormalizeEnvelopePost(notice(subject)); err == nil {
			t.Fatalf("public normalization accepted %q", subject)
		}
		for _, bad := range []string{subject + " and abc1234", subject + " in /tmp/report.json"} {
			if err := api.NormalizeBrokerPost(notice(bad), name); err == nil {
				t.Fatalf("broker normalization accepted unrelated invalid subject %q", bad)
			}
		}
		other := "Reassigned to " + name + ": see 3rd-reviewer-99c0ffee"
		if err := api.NormalizeBrokerPost(notice(other), name); err == nil {
			t.Fatalf("broker normalization masked a name it was not given: %q", other)
		}
	}
	for _, name := range []string{"74c050c73f7ff0b4-41b1c632", "wi_74c050c73f7ff0b4-41b1c632"} {
		if err := api.NormalizeBrokerPost(notice("Reassigned to "+name+": open obligation"), name); err == nil {
			t.Fatalf("broker normalization hid an ID in agent name %s", name)
		}
	}
}
