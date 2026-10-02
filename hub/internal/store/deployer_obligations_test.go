package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/broker"
	"github.com/scs32/tailterm/hub/internal/store"
)

// A message addressed to the deployment agent creates no obligation
// (wi_1040ca9662cb4689): the deployer is an automated runner that cannot
// acknowledge, so an obligation on it could only go overdue.

type deployerFixture struct {
	st                              *store.Store
	db                              *sql.DB
	ctx                             context.Context
	by                              api.Caller
	task                            api.Task
	lead, handler, worker, deployer api.Agent
	c, clock                        time.Time
}

func newDeployerFixture(t *testing.T) *deployerFixture {
	t.Helper()
	f := &deployerFixture{ctx: context.Background(), by: api.Caller{Node: "test", User: "owner"}}
	f.c = time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	f.clock = f.c
	path := filepath.Join(t.TempDir(), "hub.sqlite")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetClockForTest(func() time.Time { return f.clock })
	f.st = st
	// A second, read-only handle counts rows the store API does not expose.
	if f.db, err = sql.Open("sqlite", path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	if f.task, err = st.CreateTask(f.ctx, api.CreateTaskRequest{Name: "Deployer", Orchestrator: "lead"}, f.by); err != nil {
		t.Fatal(err)
	}
	add := func(name, role string) api.Agent {
		a, err := st.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "h", Session: name, Runtime: "codex", Role: role}, f.by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	f.lead, f.handler, f.worker = add("lead", ""), add("handler", ""), add("worker", "")
	f.deployer = add("deployer", api.AgentRoleDeployment)
	return f
}

// owed returns agent's obligation and wake-job row counts.
func (f *deployerFixture) owed(t *testing.T, agent api.Agent) (obligations, wakes int) {
	t.Helper()
	if err := f.db.QueryRowContext(f.ctx, `SELECT (SELECT count(*) FROM obligations WHERE task_id=? AND agent_id=?),(SELECT count(*) FROM wake_jobs WHERE task_id=? AND agent_id=?)`,
		f.task.ID, agent.ID, f.task.ID, agent.ID).Scan(&obligations, &wakes); err != nil {
		t.Fatal(err)
	}
	return obligations, wakes
}

// deployerMessages is every directed kind that obliges an ordinary recipient,
// with what it needs from one. "human" is a person's directed post.
var deployerMessages = []struct {
	kind, needs string
	env         *api.Envelope
}{
	{api.EnvelopeKindBlock, api.ObligationNeedsDelivery, &api.Envelope{Kind: api.EnvelopeKindBlock, Subject: "Release input import waits for the rollback copy",
		Body: api.EnvelopeBody{Reason: "the rollback copy is missing", Needs: "the retained build", ResumeWhen: "the copy is saved"}}},
	{api.EnvelopeKindRequest, api.ObligationNeedsOutcome, &api.Envelope{Kind: api.EnvelopeKindRequest, Subject: "Retry the release input import", Body: api.EnvelopeBody{Ask: "retry the import"}}},
	{api.EnvelopeKindAssign, api.ObligationNeedsOutcome, &api.Envelope{Kind: api.EnvelopeKindAssign, Subject: "Carry out the release request",
		Body: api.EnvelopeBody{Objective: "run it", Owns: []string{"release"}, Acceptance: map[string]string{"a1": "passes"}}}},
	{api.EnvelopeKindReview, api.ObligationNeedsOutcome, &api.Envelope{Kind: api.EnvelopeKindReview, Subject: "Review the release candidate",
		Body: api.EnvelopeBody{Candidate: "abc1234", Scope: "the release diff", Acceptance: map[string]string{"a1": "passes"}}}},
	{api.EnvelopeKindQuestion, api.ObligationNeedsAnswer, &api.Envelope{Kind: api.EnvelopeKindQuestion, Subject: "Which release job is held", Body: api.EnvelopeBody{Question: "which job is held?"}}},
	{api.EnvelopeKindNotice, api.ObligationNeedsDelivery, &api.Envelope{Kind: api.EnvelopeKindNotice, Subject: "The release inputs are saved", Body: api.EnvelopeBody{Text: "inputs saved"}}},
	{api.EnvelopeKindFinding, api.ObligationNeedsDelivery, &api.Envelope{Kind: api.EnvelopeKindFinding, Subject: "The release job names a stale generation",
		Body:     api.EnvelopeBody{Severity: "medium", Summary: "stale generation"},
		Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "tt deployment list", Outcome: "generation 3, job names 2"}}}},
	{"human", api.ObligationNeedsOutcome, nil},
}

// send posts one deployerMessages entry to agent: typed kinds from the
// handler's current run, the human one as the owner.
func (f *deployerFixture) send(t *testing.T, to api.Agent, env *api.Envelope) api.Message {
	t.Helper()
	req := api.PostMessageRequest{To: to.ID, Text: "please look at the held release job"}
	if env != nil {
		e := *env
		e.To = to.Name
		req = api.PostMessageRequest{AgentID: f.handler.ID, RunID: f.handler.RunID, To: to.ID, Envelope: &e}
	}
	m, err := f.st.PostMessage(f.ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *deployerFixture) stored(t *testing.T, seq int64) api.Message {
	t.Helper()
	msgs, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", api.MaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Seq == seq {
			return m
		}
	}
	t.Fatalf("message #%d is not on the board", seq)
	return api.Message{}
}

// a1: every directed kind addressed to the deployer is stored, addressed to
// it, and creates neither an obligation row nor a wake job.
func TestDeployerAddressedMessageCreatesNoObligation(t *testing.T) {
	f := newDeployerFixture(t)
	for _, c := range deployerMessages {
		t.Run(c.kind, func(t *testing.T) {
			m := f.send(t, f.deployer, c.env)
			if got := f.stored(t, m.Seq); got.To != f.deployer.ID {
				t.Fatalf("stored message is addressed to %q, want the deployer", got.To)
			}
			if o, w := f.owed(t, f.deployer); o != 0 || w != 0 {
				t.Fatalf("deployer holds %d obligation(s) and %d wake job(s), want none", o, w)
			}
		})
	}
	open, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{}, f.clock)
	if err != nil || len(open) != 0 {
		t.Fatalf("project obligations %v err %v, want none", open, err)
	}
}

// a2: the same kinds addressed to an ordinary agent oblige it as before.
func TestOrdinaryAgentStillObliged(t *testing.T) {
	f := newDeployerFixture(t)
	for i, c := range deployerMessages {
		t.Run(c.kind, func(t *testing.T) {
			m := f.send(t, f.worker, c.env)
			if o, w := f.owed(t, f.worker); o != i+1 || w != i+1 {
				t.Fatalf("worker holds %d obligation(s) and %d wake job(s), want %d of each", o, w, i+1)
			}
			obls, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: f.worker.ID, FromSeq: m.Seq, ToSeq: m.Seq}, f.clock)
			if err != nil || len(obls) != 1 {
				t.Fatalf("obligations for #%d: %v err %v", m.Seq, obls, err)
			}
			if o := obls[0]; o.Needs != c.needs || o.SourceKind != c.kind || o.State != api.ObligationQueued {
				t.Fatalf("obligation needs=%s kind=%s state=%s, want needs=%s kind=%s queued", o.Needs, o.SourceKind, o.State, c.needs, c.kind)
			}
		})
	}
	// A BLOCK to the project lead still needs an outcome from it.
	m := f.send(t, f.lead, deployerMessages[0].env)
	obls, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: f.lead.ID, FromSeq: m.Seq, ToSeq: m.Seq}, f.clock)
	if err != nil || len(obls) != 1 || obls[0].Needs != api.ObligationNeedsOutcome {
		t.Fatalf("lead BLOCK obligations %v err %v, want one needing an outcome", obls, err)
	}
}

// brokerNotices returns every message the broker has posted in the project.
func (f *deployerFixture) brokerNotices(t *testing.T) []api.Message {
	t.Helper()
	msgs, err := f.st.ListMessages(f.ctx, f.task.ID, 0, "", api.MaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range msgs {
		if m.From.Node == api.BrokerNode {
			out = append(out, m)
		}
	}
	return out
}

// a3: the incident. A BLOCK to the deployer is the project's only directed
// message; broker passes past the acknowledgement deadline, the default due
// time and the stall quiet period take no step and post nothing.
func TestDeployerBlockNeverEscalatesOrStalls(t *testing.T) {
	f := newDeployerFixture(t)
	m := f.send(t, f.deployer, deployerMessages[0].env)
	passes := []time.Duration{time.Minute, api.ObligationAckDeadline + time.Second, api.ObligationAckDeadline + api.ObligationProjectStallQuiet + time.Second,
		api.ObligationDefaultDue + time.Second, api.ObligationDefaultDue + 2*api.ObligationProjectStallQuiet, 6 * time.Hour}
	for _, d := range passes {
		now := f.c.Add(d)
		f.clock = now
		steps, err := (&broker.Broker{Store: f.st}).Tick(f.ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range steps {
			if s.TaskID == f.task.ID {
				t.Fatalf("+%s: broker step %q for obligation %q, want none", d, s.Action, s.ObligationID)
			}
		}
		overdue, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{Overdue: true}, now)
		if err != nil || len(overdue) != 0 {
			t.Fatalf("+%s: overdue obligations %v err %v, want none", d, overdue, err)
		}
		if n := f.brokerNotices(t); len(n) != 0 {
			t.Fatalf("+%s: broker posted %q, want no lead, owner or stall notice", d, n[0].Text)
		}
	}
	if got := f.stored(t, m.Seq); got.To != f.deployer.ID || got.Envelope == nil || got.Envelope.Kind != api.EnvelopeKindBlock {
		t.Fatalf("the BLOCK is no longer on the board as sent: %+v", got)
	}
}

// The control for a3: the same BLOCK sent to the project lead, left
// unacknowledged, is escalated and stalls the project.
func TestUnacknowledgedLeadBlockStillEscalates(t *testing.T) {
	f := newDeployerFixture(t)
	f.send(t, f.lead, deployerMessages[0].env)
	seen := map[string]bool{}
	escalated := false
	for d := time.Minute; d <= api.ObligationDefaultDue+time.Hour; d += time.Minute {
		now := f.c.Add(d)
		f.clock = now
		steps, err := (&broker.Broker{Store: f.st}).Tick(f.ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range steps {
			seen[s.Action] = true
			escalated = escalated || strings.HasPrefix(s.Action, "escalate-")
		}
	}
	if !escalated || !seen["project-stalled"] {
		t.Fatalf("broker steps %v, want an escalation and a project stall", seen)
	}
	if len(f.brokerNotices(t)) == 0 {
		t.Fatal("the broker posted no notice")
	}
}

// An open obligation cannot be moved to the deployer, where it would be
// superseded with nothing to replace it.
func TestReassignToDeployerRefused(t *testing.T) {
	f := newDeployerFixture(t)
	m := f.send(t, f.worker, deployerMessages[1].env)
	obls, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: f.worker.ID}, f.clock)
	if err != nil || len(obls) != 1 {
		t.Fatalf("worker obligations %v err %v", obls, err)
	}
	_, err = f.st.ReassignObligation(f.ctx, f.task.ID, obls[0].ID, api.ObligationReassignRequest{ToAgentID: f.deployer.ID, Reason: "the deployer runs releases"}, f.by)
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("reassign to the deployer: err %v, want a conflict", err)
	}
	after, err := f.st.ListObligations(f.ctx, f.task.ID, store.ObligationFilter{AgentID: f.worker.ID, FromSeq: m.Seq, ToSeq: m.Seq}, f.clock)
	if err != nil || len(after) != 1 || after[0].State == api.ObligationClosed {
		t.Fatalf("the worker's obligation did not stay open: %v err %v", after, err)
	}
	if o, w := f.owed(t, f.deployer); o != 0 || w != 0 {
		t.Fatalf("deployer holds %d obligation(s) and %d wake job(s), want none", o, w)
	}
	// Moving it to an ordinary agent still works.
	if _, err := f.st.ReassignObligation(f.ctx, f.task.ID, obls[0].ID, api.ObligationReassignRequest{ToAgentID: f.handler.ID}, f.by); err != nil {
		t.Fatalf("reassign to the handler: %v", err)
	}
	if o, w := f.owed(t, f.handler); o != 1 || w != 1 {
		t.Fatalf("handler holds %d obligation(s) and %d wake job(s), want one of each", o, w)
	}
}
