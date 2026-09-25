package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// This opt-in fixture leaves a synthetic database for the previous-base hub
// executable to open during the rollback compatibility check.
func TestWithdrawRollbackFixture(t *testing.T) {
	path := os.Getenv("TAILTERM_WITHDRAW_ROLLBACK_DB")
	if path == "" {
		t.Skip("set a synthetic rollback database path")
	}
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, filepath.Clean(os.TempDir())+string(os.PathSeparator)) &&
		!strings.HasPrefix(clean, "/tmp/tailterm-withdraw-rollback-") &&
		!strings.HasPrefix(clean, "/private/tmp/tailterm-withdraw-rollback-") {
		t.Fatal("rollback fixture must use a temporary path")
	}
	if _, err := os.Stat(clean); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback fixture path must not exist: %v", err)
	}
	s, err := Open(clean)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	by := api.Caller{Node: "fixture", User: "owner"}
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Rollback fixture", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	lead, _ := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead", Host: "h", Session: "lead", Runtime: "codex"}, by)
	worker, _ := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "h", Session: "worker", Runtime: "codex"}, by)
	m, err := s.PostMessage(ctx, task.ID, withdrawSource(api.EnvelopeKindRequest, lead, worker), by)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListObligations(ctx, task.ID, ObligationFilter{FromSeq: m.Seq, ToSeq: m.Seq}, time.Now())
	if err != nil || len(rows) != 1 {
		t.Fatalf("source: %+v %v", rows, err)
	}
	if _, err := s.WithdrawObligation(ctx, task.ID, rows[0].ID, api.ObligationWithdrawRequest{AgentID: lead.ID, RunID: lead.RunID, Reason: "rollback compatibility fixture"}); err != nil {
		t.Fatal(err)
	}
	if rows, err = s.ListObligations(ctx, task.ID, ObligationFilter{FromSeq: m.Seq, ToSeq: m.Seq}, time.Now()); err != nil || rows[0].Outcome != api.OutcomeWithdrawn {
		t.Fatalf("retained withdrawal: %+v %v", rows, err)
	}
	t.Logf("task=%s source=%d obligation=%s", task.ID, m.Seq, rows[0].ID)
}

func withdrawSource(kind string, from, to api.Agent) api.PostMessageRequest {
	e := &api.Envelope{Kind: kind, To: to.Name, Subject: "Check the old request", Body: api.EnvelopeBody{Ask: "Please check it"}}
	switch kind {
	case api.EnvelopeKindAssign:
		e.Body = api.EnvelopeBody{Objective: "check", Owns: []string{"fixture.go"}, Acceptance: map[string]string{"a1": "done"}}
	case api.EnvelopeKindReview:
		e.Body = api.EnvelopeBody{Candidate: "commit abc123", Scope: "fixture", Acceptance: map[string]string{"a1": "done"}}
	case api.EnvelopeKindQuestion:
		e.Body = api.EnvelopeBody{Question: "Should this continue?"}
	}
	return api.PostMessageRequest{AgentID: from.ID, RunID: from.RunID, To: to.ID, Envelope: e}
}

func TestWithdrawFourKindsAndFiveOpenStates(t *testing.T) {
	for _, kind := range []string{api.EnvelopeKindAssign, api.EnvelopeKindRequest, api.EnvelopeKindReview, api.EnvelopeKindQuestion} {
		for _, state := range []string{api.ObligationQueued, api.ObligationDelivered, api.ObligationAcknowledged, api.ObligationWorking, api.ObligationBlocked} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				f := newPhase3Fixture(t)
				m, err := f.post(t, withdrawSource(kind, f.lead, f.builder))
				if err != nil {
					t.Fatal(err)
				}
				o := f.obligationFor(t, m.Seq)
				if state != api.ObligationQueued {
					_, err = f.s.db.Exec(`UPDATE obligations SET state=? WHERE id=?`, state, o.ID)
					if err != nil {
						t.Fatal(err)
					}
				}
				req := api.ObligationWithdrawRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, Reason: "new assignment supersedes it", RequestID: "withdraw-1"}
				got, err := f.s.WithdrawObligation(f.ctx, f.task.ID, o.ID, req)
				if err != nil || got.State != api.ObligationClosed || got.Outcome != api.OutcomeWithdrawn || got.Reason != req.Reason || got.ClosedAt == nil {
					t.Fatalf("withdraw: %+v %v", got, err)
				}
				messages, err := f.s.ListMessages(f.ctx, f.task.ID, 0, "", 100)
				if err != nil {
					t.Fatal(err)
				}
				var notices int
				for _, msg := range messages {
					if msg.Envelope != nil && msg.Envelope.Refs["obligation"] == o.ID {
						notices++
						if msg.To != f.builder.ID || msg.Envelope.Kind != api.EnvelopeKindNotice || !strings.Contains(msg.Text, fmt.Sprint(m.Seq)) || !strings.Contains(msg.Text, req.Reason) {
							t.Fatalf("bad withdrawal notice: %+v", msg)
						}
					}
				}
				if notices != 1 {
					t.Fatalf("got %d withdrawal notices", notices)
				}
				all, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{}, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				for _, notice := range all {
					if notice.SourceKind == api.EnvelopeKindNotice && notice.Needs != api.ObligationNeedsDelivery {
						t.Fatalf("notice obliges an outcome: %+v", notice)
					}
				}
				if again, err := f.s.WithdrawObligation(f.ctx, f.task.ID, o.ID, req); err != nil || again.Outcome != api.OutcomeWithdrawn {
					t.Fatalf("retry: %+v %v", again, err)
				}
			})
		}
	}
}

func TestWithdrawRefusesOtherRunsSourcesAndClosedRows(t *testing.T) {
	f := newPhase3Fixture(t)
	m, err := f.post(t, withdrawSource(api.EnvelopeKindRequest, f.lead, f.builder))
	if err != nil {
		t.Fatal(err)
	}
	o := f.obligationFor(t, m.Seq)
	good := api.ObligationWithdrawRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, Reason: "superseded"}
	bad := []api.ObligationWithdrawRequest{
		{AgentID: f.handler.ID, RunID: f.handler.RunID, Reason: good.Reason},
		{AgentID: f.lead.ID, RunID: "run_0000000000000000", Reason: good.Reason},
		{AgentID: f.lead.ID, Reason: good.Reason},
		{AgentID: f.lead.ID, RunID: f.lead.RunID, Reason: "  "},
	}
	for _, req := range bad {
		if _, err := f.s.WithdrawObligation(f.ctx, f.task.ID, o.ID, req); err == nil {
			t.Fatalf("accepted bad actor/reason: %+v", req)
		}
	}
	if _, err := f.s.WithdrawObligation(f.ctx, api.NewID("tsk"), o.ID, good); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("other task: %v", err)
	}
	if _, err := f.s.db.Exec(`UPDATE agents SET run_id=? WHERE id=?`, "run_1111111111111111", f.lead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.WithdrawObligation(f.ctx, f.task.ID, o.ID, good); err == nil {
		t.Fatal("stale sender run accepted")
	}
	good.RunID = "run_1111111111111111"
	if _, err := f.s.WithdrawObligation(f.ctx, f.task.ID, o.ID, good); err == nil {
		t.Fatal("new sender run withdrew previous-run source")
	}
	if _, err := f.s.db.Exec(`UPDATE agents SET run_id=? WHERE id=?`, f.lead.RunID, f.lead.ID); err != nil {
		t.Fatal(err)
	}
	good.RunID = f.lead.RunID
	if _, err := f.s.WithdrawObligation(f.ctx, f.task.ID, o.ID, good); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.WithdrawObligation(f.ctx, f.task.ID, o.ID, good); err == nil {
		t.Fatal("closed withdrawal accepted without matching retry receipt")
	}
	for _, kind := range []string{api.EnvelopeKindNotice, api.EnvelopeKindBlock} {
		e := &api.Envelope{Kind: kind, To: f.builder.Name, Subject: "Another message", Body: api.EnvelopeBody{Text: "delivery"}}
		if kind == api.EnvelopeKindBlock {
			e.Body = api.EnvelopeBody{Reason: "wait", Needs: "lead", ResumeWhen: "approved"}
		}
		msg, err := f.post(t, api.PostMessageRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, To: f.builder.ID, Envelope: e})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.WithdrawObligation(f.ctx, f.task.ID, f.obligationFor(t, msg.Seq).ID, good); err == nil {
			t.Fatalf("%s source withdrawn", kind)
		}
	}
	ownerMessage, err := f.post(t, withdrawSource(api.EnvelopeKindRequest, f.lead, f.builder))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := f.s.CancelObligation(f.ctx, f.task.ID, f.obligationFor(t, ownerMessage.Seq).ID, api.ObligationCancelRequest{Reason: "owner decision", RequestID: "owner-cancel-still-works"}, f.by)
	if err != nil || cancelled.Obligation == nil || cancelled.Obligation.Outcome != api.OutcomeCancelled {
		t.Fatalf("owner cancel changed: %+v %v", cancelled, err)
	}
}

func TestWithdrawRollsBackFailedNoticeAndConcurrentRetry(t *testing.T) {
	f := newPhase3Fixture(t)
	m, err := f.post(t, withdrawSource(api.EnvelopeKindRequest, f.lead, f.builder))
	if err != nil {
		t.Fatal(err)
	}
	o := f.obligationFor(t, m.Seq)
	req := api.ObligationWithdrawRequest{AgentID: f.lead.ID, RunID: f.lead.RunID, Reason: "new request", RequestID: "retry-once"}
	if _, err := f.s.db.Exec(`CREATE TRIGGER reject_withdraw_notice BEFORE INSERT ON messages BEGIN SELECT RAISE(FAIL,'notice failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.WithdrawObligation(f.ctx, f.task.ID, o.ID, req); err == nil {
		t.Fatal("notice failure did not abort withdrawal")
	}
	if got := f.obligationFor(t, m.Seq); got.State == api.ObligationClosed {
		t.Fatal("failed notice left the obligation closed")
	}
	if _, err := f.s.db.Exec(`DROP TRIGGER reject_withdraw_notice`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.s.WithdrawObligation(f.ctx, f.task.ID, o.ID, req)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.WithdrawObligation(f.ctx, f.task.ID, o.ID, api.ObligationWithdrawRequest{AgentID: req.AgentID, RunID: req.RunID, Reason: "changed", RequestID: req.RequestID}); err == nil {
		t.Fatal("changed retry accepted")
	}
	all, err := f.s.ListObligations(f.ctx, f.task.ID, ObligationFilter{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var notices int
	for _, one := range all {
		if one.SourceKind == api.EnvelopeKindNotice {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("got %d notice obligations after concurrent retry", notices)
	}
}

func TestWithdrawFollowsMultiHopReissueToOriginalSender(t *testing.T) {
	f := newPhase3Fixture(t)
	m, err := f.post(t, withdrawSource(api.EnvelopeKindRequest, f.builder, f.handler))
	if err != nil {
		t.Fatal(err)
	}
	first := f.obligationFor(t, m.Seq)
	secondMessage, err := f.s.ReassignObligation(f.ctx, f.task.ID, first.ID, api.ObligationReassignRequest{
		ToAgentID: f.lead2.ID, ActorAgentID: f.lead.ID, ActorRunID: f.lead.RunID, Reason: "better recipient",
	}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	second := f.obligationFor(t, secondMessage.Seq)
	thirdMessage, err := f.s.ReassignObligation(f.ctx, f.task.ID, second.ID, api.ObligationReassignRequest{
		ToAgentID: f.handler.ID, ActorAgentID: f.lead.ID, ActorRunID: f.lead.RunID, Reason: "final recipient",
	}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	third := f.obligationFor(t, thirdMessage.Seq)
	for _, bad := range []api.ObligationWithdrawRequest{
		{AgentID: f.lead.ID, RunID: f.lead.RunID, Reason: "I reassigned it"},
		{AgentID: f.builder.ID, RunID: "run_0000000000000000", Reason: "stale"},
		{AgentID: f.builder.ID, Reason: "missing run"},
	} {
		if _, err := f.s.WithdrawObligation(f.ctx, f.task.ID, third.ID, bad); err == nil {
			t.Fatalf("non-author or stale sender withdrew reissued request: %+v", bad)
		}
	}
	got, err := f.s.WithdrawObligation(f.ctx, f.task.ID, third.ID, api.ObligationWithdrawRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, Reason: "reissued request is superseded"})
	if err != nil || got.Outcome != api.OutcomeWithdrawn {
		t.Fatalf("original author could not withdraw third hop: %+v %v", got, err)
	}
	if f.obligationFor(t, m.Seq).Outcome != api.OutcomeSuperseded || f.obligationFor(t, secondMessage.Seq).Outcome != api.OutcomeSuperseded {
		t.Fatal("withdrawal changed the prior superseded hops")
	}
}

func TestRecentWithdrawnQueryUsesBoundedIndex(t *testing.T) {
	f := newPhase3Fixture(t)
	var id, parent, aux int
	var detail string
	err := f.s.db.QueryRow(`EXPLAIN QUERY PLAN SELECT `+obligationCols+` FROM obligations WHERE task_id=? AND outcome=? AND state=? ORDER BY message_seq DESC LIMIT 5`,
		f.task.ID, api.OutcomeWithdrawn, api.ObligationClosed).Scan(&id, &parent, &aux, &detail)
	if err != nil || !strings.Contains(detail, "obligations_task_outcome_seq") {
		t.Fatalf("recent query plan: %q %v", detail, err)
	}
}
