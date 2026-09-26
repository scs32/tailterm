package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func verificationFixture(t *testing.T) (*convergenceFixture, api.Agent, api.VerificationPlan) {
	t.Helper()
	f := newConvergenceFixture(t)
	h, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "handler", Host: "fixture", Session: "handler", Role: api.AgentRoleDatabaseHandler}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	order := f.state(t).Scopes[0].AssignmentSeq
	_, err = f.s.ConfirmWorkOrderScope(f.ctx, f.task.ID, f.item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "verify-intake", AgentID: h.ID, RunID: h.RunID, ExpectedRevision: 1, ScopeRevision: 1, OrderMessageSeq: order, Complete: true})
	if err != nil {
		t.Fatal(err)
	}
	bundle := preparedContextFromAcceptedHistory(t, f.s, f.item, api.MessageReference{TaskID: f.task.ID, Seq: order})
	v, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "verifier", Host: "fixture", Session: "verifier", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: 1, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: order}, ContextBundle: bundle, TeamRole: api.TeamRoleMember}}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	checks := []api.VerificationCheck{{ID: "fixture-check", Argv: []string{"node", "fixture.js"}, Cwd: ".", Environment: map[string]string{}}}
	p := api.VerificationPlan{ItemID: f.item.ID, ItemTaskID: f.task.ID, AssignmentOwnershipDigest: verificationDigest([]string{"fixture"}), Version: 1, OperationKey: "fixture-verification", Repository: "fixture", BaseCommit: candidateB, Commit: candidateA, ItemRevision: 1, ScopeRevision: 1, OrderMessageSeq: order, AssignmentSeq: order, BuilderAgentID: f.reviewer.ID, BuilderRunID: f.reviewer.RunID, VerifierAgentID: v.ID, VerifierRunID: v.RunID, MatrixDigest: strings.Repeat("a", 64), ChecksDigest: verificationDigest(checks), Owned: []string{"fixture"}, Changed: []string{}, Checks: checks}
	return f, h, p
}
func passingVerification(p api.VerificationPlan) api.VerificationReceipt {
	r := api.VerificationReceipt{Worktree: "/tmp/fixture", Version: 1, OperationKey: p.OperationKey, PlanDigest: verificationDigest(p), Repository: p.Repository, BaseCommit: p.BaseCommit, Commit: p.Commit, MatrixDigest: p.MatrixDigest, ChecksDigest: p.ChecksDigest, VerifierAgentID: p.VerifierAgentID, VerifierRunID: p.VerifierRunID, Detached: true, CleanBefore: true, CleanAfter: true, Environment: map[string]string{"HOME": "/tmp/isolated"}, Prerequisites: []api.VerificationPrerequisite{}, AIV: api.AIVBinding{State: "unsubmitted", Service: "future-service", Repository: "future-repository", Snapshot: "future-snapshot", Extractor: "future-extractor", Run: "future-run", Audit: "future-audit"}}
	for _, c := range p.Checks {
		r.Checks = append(r.Checks, api.VerificationResult{VerificationCheck: c, StartedAt: "2026-09-26T00:00:00Z", EndedAt: "2026-09-26T00:00:01Z", DurationMs: 1000, LogURI: "/tmp/fixture.log", LogDigest: strings.Repeat("b", 64)})
	}
	return r
}
func saveFixtureVerification(t *testing.T, f *convergenceFixture, h api.Agent, p api.VerificationPlan, g int64) {
	t.Helper()
	_, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: p.OperationKey + "-plan", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: g, Plan: &p})
	if err != nil {
		t.Fatal(err)
	}
	r := passingVerification(p)
	_, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: p.OperationKey + "-receipt", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: g + 1, Receipt: &r})
	if err != nil {
		t.Fatal(err)
	}
}
func TestVerificationPlanReceiptRetryRestartAndCAS(t *testing.T) {
	f, h, p := verificationFixture(t)
	req := api.VerificationRequest{RequestID: "plan", AgentID: h.ID, RunID: h.RunID, Plan: &p}
	first, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, req)
	if err != nil || first.Digest != again.Digest {
		t.Fatal(again, err)
	}
	altered := req
	altered.ExpectedGeneration = 1
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, altered); !errors.Is(err, api.ErrConflict) {
		t.Fatal("altered retry", err)
	}
	f.s.Close()
	f.s, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	again, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, req)
	if err != nil || again.Generation != 1 {
		t.Fatal("restart", err)
	}
	r := passingVerification(p)
	rreq := api.VerificationRequest{RequestID: "receipt", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 1, Receipt: &r}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, rreq); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	history, err := f.s.VerificationHistory(f.ctx, f.task.ID, f.item.ID, h.ID, h.RunID)
	if err != nil || len(history) != 2 || history[1].Receipt.AIV != r.AIV {
		t.Fatal(history, err)
	}
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, api.NewID("wi"), rreq); !errors.Is(err, api.ErrConflict) {
		t.Fatal("receipt retry crossed item boundary", err)
	}
	stale := req
	stale.RequestID = "stale"
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, stale); !errors.Is(err, api.ErrConflict) {
		t.Fatal("CAS", err)
	}
}
func TestVerificationRejectsInvalidReceiptAndIdentities(t *testing.T) {
	f, h, p := verificationFixture(t)
	bad := p
	bad.VerifierAgentID = p.BuilderAgentID
	bad.VerifierRunID = p.BuilderRunID
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "builder", AgentID: h.ID, RunID: h.RunID, Plan: &bad}); !errors.Is(err, api.ErrConflict) {
		t.Fatal(err)
	}
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "wrong-handler", AgentID: f.reviewer.ID, RunID: f.reviewer.RunID, Plan: &p}); !errors.Is(err, api.ErrConflict) {
		t.Fatal(err)
	}
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "plan", AgentID: h.ID, RunID: h.RunID, Plan: &p}); err != nil {
		t.Fatal(err)
	}
	cases := []func(*api.VerificationReceipt){func(r *api.VerificationReceipt) { r.Checks = nil }, func(r *api.VerificationReceipt) { r.Checks = append(r.Checks, r.Checks[0]) }, func(r *api.VerificationReceipt) { r.Checks[0].ExitCode = 1 }, func(r *api.VerificationReceipt) { r.CleanAfter = false }, func(r *api.VerificationReceipt) { r.Commit = candidateC }, func(r *api.VerificationReceipt) { r.VerifierRunID = api.NewID("run") }, func(r *api.VerificationReceipt) { r.Checks[0].Argv = []string{"true"} }, func(r *api.VerificationReceipt) { r.AIV.State = "submitted" }}
	for i, mutate := range cases {
		r := passingVerification(p)
		mutate(&r)
		_, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "bad-" + string(rune('a'+i)), AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 1, Receipt: &r})
		if !errors.Is(err, api.ErrConflict) {
			t.Fatalf("case%d %v", i, err)
		}
	}
	history, _ := f.s.VerificationHistory(f.ctx, f.task.ID, f.item.ID, h.ID, h.RunID)
	if len(history) != 1 {
		t.Fatal("failed import wrote record")
	}
}
func TestVerificationCompletionPathsAndInvalidation(t *testing.T) {
	f, h, p := verificationFixture(t)
	request := f.review(t, candidateA)
	if _, err := f.post(f.resultEnv(candidateA, map[string]string{"a1": "pass", "a2": "pass"}, api.ReviewMetadata{Mode: "general"}), "", request.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	accept := api.Envelope{Kind: "notice", Subject: "Accept exact fixture candidate", Body: api.EnvelopeBody{Text: "accept"}, Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: candidateA}}
	done := "done"
	if _, err := f.post(accept, "", 0, api.Agent{}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("lead missing receipt", err)
	}
	if _, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatal("PATCH done", err)
	}
	if _, _, err := f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, api.CreateWorkItemUpdate{RequestID: "done", ExpectedRevision: 1, Status: &done}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatal("keyed done", err)
	}
	tx, _ := f.s.db.BeginTx(f.ctx, nil)
	if err := reviewCompletion(f.ctx, tx, f.item, candidateA); !errors.Is(err, api.ErrConflict) {
		t.Fatal("queue gate", err)
	}
	tx.Rollback()
	saveFixtureVerification(t, f, h, p, 0)
	if _, err := f.post(accept, "", 0, api.Agent{}); err != nil {
		t.Fatal(err)
	}
	tx, _ = f.s.db.BeginTx(f.ctx, nil)
	if err := reviewCompletion(f.ctx, tx, f.item, candidateA); err != nil {
		t.Fatal(err)
	}
	if err := reviewCompletion(f.ctx, tx, f.item, candidateB); !errors.Is(err, api.ErrConflict) {
		t.Fatal("new commit", err)
	}
	tx.Rollback()
	priority := "low"
	var err error
	f.item, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: 1, Priority: &priority}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	tx, _ = f.s.db.BeginTx(f.ctx, nil)
	if err = verificationReady(f.ctx, tx, f.item, candidateA); err != nil {
		t.Fatal("administrative revision", err)
	}
	tx.Rollback()
	changed := p
	changed.OperationKey = "new-candidate"
	changed.Commit = candidateB
	changed.MatrixDigest = strings.Repeat("c", 64)
	changed.ItemRevision = f.item.Revision
	// Scope confirmation is revision-pinned for the new plan.
	if _, err = f.s.ConfirmWorkOrderScope(f.ctx, f.task.ID, f.item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "new-intake", AgentID: h.ID, RunID: h.RunID, ExpectedRevision: f.item.Revision, ScopeRevision: 1, OrderMessageSeq: p.OrderMessageSeq, Complete: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "new-plan", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 2, Plan: &changed}); err != nil {
		t.Fatal(err)
	}
	tx, _ = f.s.db.BeginTx(f.ctx, nil)
	if err = verificationReady(f.ctx, tx, f.item, candidateA); !errors.Is(err, api.ErrConflict) {
		t.Fatal("invalidated", err)
	}
	tx.Rollback()
	history, _ := f.s.VerificationHistory(f.ctx, f.task.ID, f.item.ID, h.ID, h.RunID)
	if len(history) != 3 {
		t.Fatal("history lost")
	}
}
func TestVerificationMigrationRehearsal(t *testing.T) {
	f, h, p := verificationFixture(t)
	saveFixtureVerification(t, f, h, p, 0)
	f.s.Close()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	archive := exec.Command("git", "archive", "7ab5100eb6c117fd08b4bb971b093eb2d429eefd", "hub")
	archive.Dir = root
	b, err := archive.Output()
	if err != nil {
		t.Fatal(err)
	}
	tar := exec.Command("tar", "-x", "-C", temp)
	tar.Stdin = strings.NewReader(string(b))
	if out, err := tar.CombinedOutput(); err != nil {
		t.Fatalf("extract %s: %v", out, err)
	}
	helper := filepath.Join(temp, "hub", "fixture-reopen.go")
	if err = os.WriteFile(helper, []byte(`package main
import("os";"github.com/scs32/tailterm/hub/internal/store")
func main(){s,e:=store.Open(os.Args[1]);if e!=nil{panic(e)};if e=s.Close();e!=nil{panic(e)}}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", helper, f.path)
	cmd.Dir = filepath.Join(temp, "hub")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("previous binary %s: %v", out, err)
	}
	f.s, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	history, err := f.s.VerificationHistory(f.ctx, f.task.ID, f.item.ID, h.ID, h.RunID)
	if err != nil || len(history) != 2 {
		t.Fatal(history, err)
	}
	var integrity string
	if err = f.s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal(integrity, err)
	}
	rows, err := f.s.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation")
	}
}

// seedPassingVerification is fixture setup for older tests of unrelated
// lifecycle/narrative/review contracts. Import and selection invariants are
// exercised through public operations by TestVerification* above.
func seedPassingVerification(t *testing.T, s *Store, item api.WorkItem, candidate string) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	state, err := reviewState(ctx, tx, item.TaskID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	sc := scopeFor(&state, item.ScopeRevision)
	if sc == nil {
		state.History = "recorded"
		state.Scopes = append(state.Scopes, api.ReviewScope{ScopeRevision: item.ScopeRevision, ItemRevision: item.Revision, AssignmentSeq: 1, Criteria: map[string]string{"a1": "synthetic fixture"}})
		state.Rounds = append(state.Rounds, api.ReviewRound{Number: 1, ScopeRevision: item.ScopeRevision, RequestSeq: 1, ResultSeq: 2, Candidate: candidate, Criteria: map[string]string{"a1": "synthetic fixture"}, Verdicts: map[string]string{"a1": "pass"}})
		state.Disposition = &api.ReviewDisposition{Kind: "accept", Candidate: candidate, MessageSeq: 3}
		if err = saveReviewState(ctx, tx, item.TaskID, state); err != nil {
			t.Fatal(err)
		}
		sc = scopeFor(&state, item.ScopeRevision)
	}
	checks := []api.VerificationCheck{{ID: "synthetic-fixture", Argv: []string{"fixture"}, Cwd: ".", Environment: map[string]string{}}}
	p := api.VerificationPlan{ItemID: item.ID, ItemTaskID: item.TaskID, AssignmentOwnershipDigest: verificationDigest([]string{"fixture"}), Version: 1, OperationKey: api.NewID("req"), Repository: "fixture", Commit: candidate, BaseCommit: candidateA, ItemRevision: item.Revision, ScopeRevision: item.ScopeRevision, AssignmentSeq: sc.AssignmentSeq, MatrixDigest: strings.Repeat("a", 64), ChecksDigest: verificationDigest(checks), VerifierAgentID: api.NewID("agt"), VerifierRunID: api.NewID("run"), Checks: checks}
	r := passingVerification(p)
	var n int64
	if err = tx.QueryRow(`SELECT count(*) FROM verification_records WHERE task_id=? AND item_id=?`, item.TaskID, item.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	for _, record := range []api.VerificationRecord{{Generation: n + 1, Kind: "plan", Plan: &p}, {Generation: n + 2, Kind: "receipt", Receipt: &r}} {
		b, _ := json.Marshal(record)
		if _, err = tx.Exec(`INSERT INTO verification_records VALUES(?,?,?,?,?,?,?)`, item.TaskID, item.ID, record.Generation, record.Kind, api.NewID("req"), "synthetic-fixture", string(b)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
func TestVerificationCrossRuntimeDigest(t *testing.T) {
	f, _, p := verificationFixture(t)
	p.Repository = "fixture <tag> & Unicode λ"
	raw, _ := json.Marshal(p)
	script := `import {digest} from './scripts/verify-matrix.mjs';let s='';for await(const x of process.stdin)s+=x;process.stdout.write(digest(JSON.parse(s)))`
	cmd := exec.Command("node", "--input-type=module", "-e", script)
	cmd.Dir = "../../.."
	cmd.Stdin = strings.NewReader(string(raw))
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != verificationDigest(p) {
		t.Fatalf("native/host mismatch %s %s", out, verificationDigest(p))
	}
	_ = f
}
func TestVerificationScopeInvalidationAndStaleVerifier(t *testing.T) {
	f, h, p := verificationFixture(t)
	saveFixtureVerification(t, f, h, p, 0)
	desc := "Changed scope"
	updated, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: 1, Description: &desc}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := f.s.db.BeginTx(f.ctx, nil)
	if err = verificationReady(f.ctx, tx, updated, p.Commit); !errors.Is(err, api.ErrConflict) {
		t.Fatal("scope carried old pass", err)
	}
	tx.Rollback()
	f2, h2, p2 := verificationFixture(t)
	if _, err = f2.s.SaveVerification(f2.ctx, f2.task.ID, f2.item.ID, api.VerificationRequest{RequestID: "plan", AgentID: h2.ID, RunID: h2.RunID, Plan: &p2}); err != nil {
		t.Fatal(err)
	}
	exited := api.AgentExited
	if _, err = f2.s.UpdateAgent(f2.ctx, p2.VerifierAgentID, api.UpdateAgentRequest{Status: &exited}, f2.by); err != nil {
		t.Fatal(err)
	}
	r := passingVerification(p2)
	if _, err = f2.s.SaveVerification(f2.ctx, f2.task.ID, f2.item.ID, api.VerificationRequest{RequestID: "receipt", AgentID: h2.ID, RunID: h2.RunID, ExpectedGeneration: 1, Receipt: &r}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("stale identity accepted", err)
	}
}
func TestVerificationDistinctReviewerCannotProduceReceipt(t *testing.T) {
	f, h, p := verificationFixture(t)
	_, err := f.post(api.Envelope{Kind: "review", Subject: "Review exact fixture candidate", Body: api.EnvelopeBody{Candidate: candidateA, Scope: "fixture", Acceptance: map[string]string{"a1": "works", "a2": "retries"}}}, p.VerifierAgentID, 0, api.Agent{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "reviewer-plan", AgentID: h.ID, RunID: h.RunID, Plan: &p}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("reviewer certified", err)
	}
}
func TestVerificationConcurrentNewPlansAndRollback(t *testing.T) {
	f, h, p := verificationFixture(t)
	saveFixtureVerification(t, f, h, p, 0)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, key := range []string{"matrix-a", "matrix-b"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			newPlan := p
			newPlan.OperationKey = key
			newPlan.MatrixDigest = strings.Repeat("c", 64)
			_, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: key, AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 2, Plan: &newPlan})
			results <- err
		}(key)
	}
	wg.Wait()
	close(results)
	pass, conflict := 0, 0
	for err := range results {
		if err == nil {
			pass++
		} else if errors.Is(err, api.ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if pass != 1 || conflict != 1 {
		t.Fatal("CAS", pass, conflict)
	}
	history, err := f.s.VerificationHistory(f.ctx, f.task.ID, f.item.ID, h.ID, h.RunID)
	if err != nil || len(history) != 3 {
		t.Fatal("rollback/history", history, err)
	}
	tx, _ := f.s.db.BeginTx(f.ctx, nil)
	if err = verificationReady(f.ctx, tx, f.item, p.Commit); !errors.Is(err, api.ErrConflict) {
		t.Fatal("matrix change carried receipt", err)
	}
	tx.Rollback()
}
