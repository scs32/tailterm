package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// A bug may close while its approved matrix still lists known failures that
// name it, when its current receipt for the accepted SHA shows each of them
// passing (wi_6fb81e10c366d7b4). Every other known-failure case stays strict.

// seedKnownCloseVerification inserts a plan listing known failures and a
// receipt with one exit-code sequence per check, as a handler import would
// store them. mutate, when set, edits the receipt before it is stored.
func seedKnownCloseVerification(t *testing.T, s *Store, item api.WorkItem, candidate string, known []api.VerificationKnownFailure, codes [][]int, mutate func(*api.VerificationReceipt)) (api.VerificationPlan, api.VerificationReceipt) {
	t.Helper()
	ctx := t.Context()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	state, err := reviewState(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	state.History = "recorded"
	state.Scopes = append(state.Scopes, api.ReviewScope{ScopeRevision: item.ScopeRevision, ItemRevision: item.Revision, AssignmentSeq: 1, Criteria: map[string]string{"a1": "synthetic fixture"}})
	state.Rounds = append(state.Rounds, api.ReviewRound{Number: 1, ScopeRevision: item.ScopeRevision, RequestSeq: 1, ResultSeq: 2, Candidate: candidate, Criteria: map[string]string{"a1": "synthetic fixture"}, Verdicts: map[string]string{"a1": "pass"}})
	state.Disposition = &api.ReviewDisposition{Kind: "accept", Candidate: candidate, MessageSeq: 3}
	if err = saveReviewState(ctx, tx, item.TaskID, state); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var checks []api.VerificationCheck
	for i := range codes {
		checks = append(checks, api.VerificationCheck{ID: fmt.Sprintf("check-%d", i), Argv: []string{"fixture", fmt.Sprint(i)}, Cwd: ".", Environment: map[string]string{}})
	}
	p := api.VerificationPlan{MaxAttempts: 3, KnownFailures: known, ItemID: item.ID, ItemTaskID: item.TaskID, AssignmentOwnershipDigest: verificationDigest([]string{"fixture"}), Version: 1, OperationKey: api.NewID("req"), Repository: autoWorktree, Commit: candidate, BaseCommit: candidateA, ItemRevision: item.Revision, ScopeRevision: item.ScopeRevision, AssignmentSeq: 1, MatrixDigest: strings.Repeat("a", 64), ApprovedMatrixDigest: strings.Repeat("a", 64), ChecksDigest: verificationDigest(checks), VerifierAgentID: api.NewID("agt"), VerifierRunID: api.NewID("run"), Checks: checks}
	r := passingVerification(p)
	for i, c := range codes {
		r.Checks[i] = retryVerification(p, c...).Checks[i]
	}
	if mutate != nil {
		mutate(&r)
	}
	insertVerificationRecord(t, s, item, api.VerificationRecord{Kind: "plan", Plan: &p})
	insertVerificationRecord(t, s, item, api.VerificationRecord{Kind: "receipt", Receipt: &r})
	return p, r
}

func insertVerificationRecord(t *testing.T, s *Store, item api.WorkItem, record api.VerificationRecord) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT count(*)+1 FROM verification_records WHERE task_id=? AND item_id=?`, item.TaskID, item.ID).Scan(&record.Generation); err != nil {
		t.Fatal(err)
	}
	record.ItemID, record.ItemTaskID = item.ID, item.TaskID
	b, _ := json.Marshal(record)
	if _, err := s.db.Exec(`INSERT INTO verification_records VALUES(?,?,?,?,?,?,?)`, item.TaskID, item.ID, record.Generation, record.Kind, api.NewID("req"), "synthetic-fixture", string(b)); err != nil {
		t.Fatal(err)
	}
}

func enrollVerification(t *testing.T, s *Store, item api.WorkItem) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO verification_enrollments(task_id,item_id,agent_id,run_id,required,provenance,created_at) VALUES(?,?,?,?,1,'fixture',?)`, item.TaskID, item.ID, api.NewID("agt"), api.NewID("run"), ts(time.Now())); err != nil {
		t.Fatal(err)
	}
}

func selfKnown(item api.WorkItem, checks ...string) []api.VerificationKnownFailure {
	var out []api.VerificationKnownFailure
	for _, c := range checks {
		out = append(out, api.VerificationKnownFailure{CheckID: c, BugTaskID: item.TaskID, BugID: item.ID})
	}
	return out
}

// newKnownCloseFixture is an enrolled bug B with a running queue entry. Its
// plan lists check-0 and check-1 as known failures naming B, plus any extra
// entries; check-2 is an ordinary check.
func newKnownCloseFixture(t *testing.T, codes [][]int, extra []api.VerificationKnownFailure, mutate func(*api.VerificationReceipt)) (*autoAcceptFixture, api.VerificationPlan, api.VerificationReceipt) {
	t.Helper()
	f := newAutoAcceptFixture(t, false)
	known := append(selfKnown(f.item, "check-0", "check-1"), extra...)
	p, r := seedKnownCloseVerification(t, f.s, f.item, candidateB, known, codes, mutate)
	enrollVerification(t, f.s, f.item)
	return f, p, r
}

var knownClosePassing = [][]int{{0}, {0}, {0}}

const notPassing = "closing bug is not passing in its current receipt"

func isConflict(err error, reason string) bool {
	return errors.Is(err, api.ErrConflict) && strings.Contains(err.Error(), reason)
}

func (f *autoAcceptFixture) verificationBytes(t *testing.T) []string {
	t.Helper()
	rows, err := f.s.db.Query(`SELECT record_json FROM verification_records WHERE task_id=? AND item_id=? ORDER BY generation`, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		out = append(out, raw)
	}
	return out
}

func (f *autoAcceptFixture) rawAcceptance(t *testing.T) string {
	t.Helper()
	var raw string
	if err := f.s.db.QueryRow(`SELECT acceptance_json FROM team_queue_entries WHERE task_id=? AND id=?`, f.task.ID, f.entry).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *autoAcceptFixture) releaseJob(t *testing.T) api.ReleaseJob {
	t.Helper()
	var raw string
	if err := f.s.db.QueryRow(`SELECT record_json FROM release_jobs WHERE task_id=? AND entry_id=?`, f.task.ID, f.entry).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var j api.ReleaseJob
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		t.Fatal(err)
	}
	return j
}

func (f *autoAcceptFixture) ownerDone(key string) error {
	done := "done"
	_, _, err := f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, api.CreateWorkItemUpdate{ExpectedRevision: f.item.Revision, Status: &done, RequestID: key}, f.by)
	return err
}

func assertResolved(t *testing.T, got *api.ResolvedKnownFailures, f *autoAcceptFixture, p api.VerificationPlan, r api.VerificationReceipt) {
	t.Helper()
	want := &api.ResolvedKnownFailures{ReceiptGeneration: 2, ReceiptDigest: verificationDigest(r), MatrixDigest: p.MatrixDigest, Entries: selfKnown(f.item, "check-0", "check-1")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved known failures\n got %+v\nwant %+v", got, want)
	}
}

// a1, a6, a7: the handler's done save closes B and accepts its entry in one
// save, recording which known failures the receipt resolved.
func TestKnownFailureCloseSameSave(t *testing.T) {
	f, p, r := newKnownCloseFixture(t, knownClosePassing, nil, nil)
	before := f.verificationBytes(t)
	res, err := f.save("done-save", f.tuple(candidateB))
	if err != nil {
		t.Fatal("done save with every self entry passing", err)
	}
	if item := f.current(t); item.Status != "done" || item.Revision != res.Revision.Revision {
		t.Fatalf("item not saved done: %+v", item)
	}
	q := f.queueEntry(t)
	if q.Acceptance == nil || q.Revision != 2 {
		t.Fatalf("entry not accepted by the save: %+v", q)
	}
	if n := f.releaseJobs(t); n != 1 {
		t.Fatalf("release jobs = %d, want 1", n)
	}
	assertResolved(t, q.Acceptance.ResolvedKnownFailures, f, p, r)
	if j := f.releaseJob(t); q.Acceptance.ResolvedKnownFailures.ReceiptDigest != j.VerificationDigest {
		t.Fatalf("receipt digest %s does not match the release job %s", q.Acceptance.ResolvedKnownFailures.ReceiptDigest, j.VerificationDigest)
	}
	if !strings.Contains(f.rawAcceptance(t), `"resolvedKnownFailures"`) {
		t.Fatal("stored acceptance lacks the resolution record")
	}

	// a1 retry: same key replays; no second job.
	again, err := f.save("done-save", f.tuple(candidateB))
	if err != nil || again.Receipt.ID != res.Receipt.ID {
		t.Fatal("retried save did not replay", err)
	}
	if n := f.releaseJobs(t); n != 1 || f.queueEntry(t).Revision != 2 {
		t.Fatalf("retry changed the entry or enqueued again: %d jobs", n)
	}

	// a7: the save leaves verification records byte-identical.
	if after := f.verificationBytes(t); !reflect.DeepEqual(before, after) {
		t.Fatal("done save rewrote verification records")
	}

	// a6: downstream readers see the closed bug's entries as resolved.
	if v := f.queueEntry(t).Verification; v == nil || v.State != "passing" {
		t.Fatalf("queue summary after close: %+v", v)
	}
	d, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "deployer", Host: "fixture", Session: "deployer", Role: api.AgentRoleDeployment}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.db.Exec(`UPDATE agents SET status='running',last_seen_at=? WHERE id=?`, ts(time.Now()), d.ID); err != nil {
		t.Fatal(err)
	}
	j := f.releaseJob(t)
	j, err = f.s.ReleaseAction(f.ctx, f.task.ID, api.ReleaseRequest{RequestID: "claim", Operation: "claim", AgentID: d.ID, RunID: d.RunID, JobID: j.ID, ExpectedGeneration: j.Generation})
	if err != nil {
		t.Fatal("release claim after close", err)
	}
	integrated := j.Plan
	integrated.Commit = candidateC
	integrated.VerifierAgentID = d.ID
	integrated.VerifierRunID = d.RunID
	if integrated.ApprovedMatrixDigest != integrated.MatrixDigest {
		t.Fatal("seeded plan lacks its approved matrix digest")
	}
	importReceipt := func(codes [][]int) api.VerificationReceipt {
		out := passingVerification(integrated)
		for i, c := range codes {
			out.Checks[i] = retryVerification(integrated, c...).Checks[i]
		}
		return out
	}
	importAction := func(key string, receipt api.VerificationReceipt) (api.ReleaseJob, error) {
		return f.s.ReleaseAction(f.ctx, f.task.ID, api.ReleaseRequest{RequestID: key, Operation: "verification", AgentID: f.handler.ID, RunID: f.handler.RunID, JobID: j.ID, ExpectedGeneration: j.Generation, IntegratedCommit: integrated.Commit, Plan: &integrated, Verification: &receipt})
	}
	if _, err = importAction("import-failing", importReceipt([][]int{{1, 1, 1}, {0}, {0}})); !isConflict(err, notPassing) {
		t.Fatal("integrated receipt with a failing self entry was accepted", err)
	}
	if _, err = importAction("import-passing", importReceipt(knownClosePassing)); err != nil {
		t.Fatal("integrated receipt with every self entry passing", err)
	}

	// a4: another item still listing B, now done, is refused.
	other, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "lists the closed bug", RequestID: "item-d"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	seedKnownCloseVerification(t, f.s, other, candidateB, selfKnown(f.item, "check-0"), [][]int{{0}}, nil)
	enrollVerification(t, f.s, other)
	tx, _ := f.s.db.BeginTx(f.ctx, nil)
	err = verificationReady(f.ctx, tx, other, candidateB)
	tx.Rollback()
	if !isConflict(err, "linked open bug") {
		t.Fatal("another item's entry naming the closed bug was waived", err)
	}
}

// a2: a self entry that still fails, or only passed on retry, keeps B open on
// every done path.
func TestKnownFailureCloseStillFailing(t *testing.T) {
	for name, codes := range map[string][][]int{"exhausted": {{1, 1, 1}, {0}, {0}}, "flaky": {{0}, {1, 0}, {0}}} {
		t.Run(name, func(t *testing.T) {
			f, _, _ := newKnownCloseFixture(t, codes, nil, nil)
			if _, err := f.save("done-save", f.tuple(candidateB)); !isConflict(err, notPassing) {
				t.Fatal("handler done save", err)
			}
			f.assertUntouched(t)
			if err := f.ownerDone("owner-done"); !isConflict(err, notPassing) {
				t.Fatal("done save without a queue entry", err)
			}
			done := "done"
			if _, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &done}, f.by); !isConflict(err, notPassing) {
				t.Fatal("UpdateWorkItem to done", err)
			}
			f.assertUntouched(t)
		})
	}
}

// a3: nothing can be resolved by a receipt that is stale, for another commit,
// absent, or ineligible.
func TestKnownFailureCloseStaleOrMismatchedReceipt(t *testing.T) {
	t.Run("plan saved after receipt", func(t *testing.T) {
		f, p, _ := newKnownCloseFixture(t, knownClosePassing, nil, nil)
		p.OperationKey = api.NewID("req")
		insertVerificationRecord(t, f.s, f.item, api.VerificationRecord{Kind: "plan", Plan: &p})
		if _, err := f.save("done-save", f.tuple(candidateB)); !isConflict(err, "exact accepted candidate required") {
			t.Fatal(err)
		}
		f.assertUntouched(t)
	})
	t.Run("commit mismatch", func(t *testing.T) {
		f, _, _ := newKnownCloseFixture(t, knownClosePassing, nil, nil)
		if _, err := f.save("done-save", f.tuple(candidateC)); !isConflict(err, "exact accepted candidate required") {
			t.Fatal(err)
		}
		f.assertUntouched(t)
	})
	t.Run("nil receipt", func(t *testing.T) {
		f, p, r := newKnownCloseFixture(t, knownClosePassing, nil, nil)
		self := f.item
		self.Status = "done"
		if _, err := validateVerificationKnownFailures(f.ctx, f.s.db, p, self, nil); !isConflict(err, notPassing) {
			t.Fatal("self done with no receipt", err)
		}
		resolved, err := validateVerificationKnownFailures(f.ctx, f.s.db, p, self, &r)
		if err != nil || !reflect.DeepEqual(resolved, selfKnown(f.item, "check-0", "check-1")) {
			t.Fatal("control: passing receipt", resolved, err)
		}
		f.assertUntouched(t)
	})
	t.Run("forged pass with nonzero exit", func(t *testing.T) {
		forge := func(r *api.VerificationReceipt) {
			r.Checks[0].Status = "pass"
			r.Checks[0].NowPassing = true
		}
		f, _, _ := newKnownCloseFixture(t, [][]int{{1, 1, 1}, {0}, {0}}, nil, forge)
		if _, err := f.save("done-save", f.tuple(candidateB)); !isConflict(err, "forged result status") {
			t.Fatal(err)
		}
		f.assertUntouched(t)
	})
}

// a4: an entry naming a different closed bug is never waived, even when its
// check passes.
func TestKnownFailureCloseDifferentClosedBug(t *testing.T) {
	for _, status := range []string{"done", "dismissed"} {
		t.Run(status, func(t *testing.T) {
			f := newAutoAcceptFixture(t, false)
			c, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "another bug", RequestID: "item-c"}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, c.ID, api.UpdateWorkItemRequest{Revision: c.Revision, Status: &status}, f.by); err != nil {
				t.Fatal(err)
			}
			known := append(selfKnown(f.item, "check-0", "check-1"), api.VerificationKnownFailure{CheckID: "check-2", BugTaskID: c.TaskID, BugID: c.ID})
			seedKnownCloseVerification(t, f.s, f.item, candidateB, known, knownClosePassing, nil)
			enrollVerification(t, f.s, f.item)
			if _, err = f.save("done-save", f.tuple(candidateB)); !isConflict(err, "linked open bug") {
				t.Fatal("entry naming a closed other bug was waived", err)
			}
			f.assertUntouched(t)
		})
	}
}

// a5: a new plan still cannot list a closed bug.
func TestKnownFailureClosePlanSaveStaysStrict(t *testing.T) {
	f, h, p, bug := knownVerificationFixture(t)
	done := "done"
	if _, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, bug.ID, api.UpdateWorkItemRequest{Revision: bug.Revision, Status: &done}, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "plan", AgentID: h.ID, RunID: h.RunID, Plan: &p}); !isConflict(err, "linked open bug") {
		t.Fatal("plan listing a done bug was saved", err)
	}
}

// a7: nothing resolved records nothing, and a client cannot supply the record.
func TestKnownFailureCloseRecordIsServerDerived(t *testing.T) {
	plain := newAutoAcceptFixture(t, true)
	if _, err := plain.save("done-save", plain.tuple(candidateB)); err != nil {
		t.Fatal(err)
	}
	if raw := plain.rawAcceptance(t); raw == "" || strings.Contains(raw, "resolvedKnownFailures") {
		t.Fatalf("acceptance without known failures: %s", raw)
	}

	f, p, r := newKnownCloseFixture(t, knownClosePassing, nil, nil)
	if err := f.ownerDone("owner-done"); err != nil {
		t.Fatal(err)
	}
	q := f.queueEntry(t)
	item := f.current(t)
	forged := api.TeamIntegrationAcceptance{Repository: autoRepository, BaseCommit: q.BaseCommit, Worktree: autoWorktree, Branch: autoBranch, Commit: candidateB, ItemRevision: item.Revision, CompletionReport: item.CompletionReport, Evidence: "handler-saved acceptance receipt",
		ResolvedKnownFailures: &api.ResolvedKnownFailures{ReceiptGeneration: 2, ReceiptDigest: verificationDigest(r), MatrixDigest: p.MatrixDigest, Entries: p.KnownFailures}}
	if _, err := f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: "forged-accept", Operation: "accept", EntryID: f.entry, ExpectedRevision: q.Revision, HandlerAgentID: f.handler.ID, HandlerRunID: f.handler.RunID, Acceptance: &forged}); !errors.Is(err, api.ErrInvalid) {
		t.Fatal("client-supplied resolution record", err)
	}
	if q := f.queueEntry(t); q.Acceptance != nil || f.releaseJobs(t) != 0 {
		t.Fatalf("refused accept changed the entry: %+v", q)
	}
}

// a8: the manual accept after an owner done save records the same resolution,
// and an identical re-accept changes nothing.
func TestKnownFailureCloseManualAcceptParity(t *testing.T) {
	f, p, r := newKnownCloseFixture(t, knownClosePassing, nil, nil)
	if err := f.ownerDone("owner-done"); err != nil {
		t.Fatal("owner done save without the tuple", err)
	}
	if q := f.queueEntry(t); q.Acceptance != nil {
		t.Fatalf("owner save accepted the entry: %+v", q)
	}
	accepted, err := f.accept(t, "manual-accept", autoWorktree, candidateB)
	if err != nil {
		t.Fatal("manual accept", err)
	}
	assertResolved(t, accepted.Acceptance.ResolvedKnownFailures, f, p, r)
	if n := f.releaseJobs(t); n != 1 {
		t.Fatalf("release jobs = %d, want 1", n)
	}
	same, err := f.accept(t, "manual-accept-again", autoWorktree, candidateB)
	if err != nil || same.Revision != accepted.Revision || f.queueEntry(t).Revision != accepted.Revision || f.releaseJobs(t) != 1 {
		t.Fatal("identical re-accept was not a no-op", err)
	}
}

// a9: while B is still in progress its own failing entry is an ordinary open
// known failure, so the lead's accept disposition (the owner-release path)
// still records, and only the done save refuses.
func TestKnownFailureCloseDispositionUnchanged(t *testing.T) {
	f, h, p := verificationFixture(t)
	p.MaxAttempts = 3
	p.KnownFailures = []api.VerificationKnownFailure{{CheckID: p.Checks[0].ID, BugTaskID: f.task.ID, BugID: f.item.ID}}
	request := f.review(t, p.Commit)
	if _, err := f.post(f.resultEnv(p.Commit, map[string]string{"a1": "pass", "a2": "pass"}, api.ReviewMetadata{Mode: "general"}), "", request.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "plan", AgentID: h.ID, RunID: h.RunID, Plan: &p}); err != nil {
		t.Fatal(err)
	}
	r := retryVerification(p, 1, 1, 1)
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "receipt", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 1, Receipt: &r}); err != nil {
		t.Fatal(err)
	}
	accept := api.Envelope{Kind: "notice", Subject: "Accept exact fixture candidate", Body: api.EnvelopeBody{Text: "accept"}, Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: p.Commit}}
	if _, err := f.post(accept, "", 0, api.Agent{}); err != nil {
		t.Fatal("accept disposition with an open self entry failing", err)
	}
	done := "done"
	if _, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &done}, f.by); !isConflict(err, notPassing) {
		t.Fatal("done save with a failing self entry", err)
	}
}
