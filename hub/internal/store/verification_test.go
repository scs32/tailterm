package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	approval, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "verification-matrix-approval:" + p.MatrixDigest}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	p.ApprovedMatrixDigest = p.MatrixDigest
	p.MatrixApprovalMessageSeq = approval.Seq
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
	cases := []func(*api.VerificationReceipt){func(r *api.VerificationReceipt) { r.Checks = nil }, func(r *api.VerificationReceipt) { r.Checks = append(r.Checks, r.Checks[0]) }, func(r *api.VerificationReceipt) { r.CleanAfter = false }, func(r *api.VerificationReceipt) { r.Commit = candidateC }, func(r *api.VerificationReceipt) { r.VerifierRunID = api.NewID("run") }, func(r *api.VerificationReceipt) { r.Checks[0].Argv = []string{"true"} }, func(r *api.VerificationReceipt) { r.AIV.State = "submitted" }}
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
	approval, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "verification-matrix-approval:" + changed.MatrixDigest}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	changed.ApprovedMatrixDigest = changed.MatrixDigest
	changed.MatrixApprovalMessageSeq = approval.Seq
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
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	base := os.Getenv("VERIFICATION_BASE_COMMIT")
	if base == "" {
		// Ordinary unit invocation rehearses against its exact checked-out commit;
		// matrix invocation always supplies the approved plan's actual base SHA.
		cmd := exec.Command("git", "rev-parse", "HEAD")
		cmd.Dir = root
		b, e := cmd.Output()
		if e != nil {
			t.Fatalf("exact rehearsal base required: %v", e)
		}
		base = strings.TrimSpace(string(b))
	}
	if !validGitCommit(base) {
		t.Fatal("invalid rehearsal base")
	}
	temp := t.TempDir()
	path := filepath.Join(temp, "fixture.sqlite")
	archive := exec.Command("git", "archive", base, "hub")
	archive.Dir = root
	b, err := archive.Output()
	if err != nil {
		t.Fatalf("plan base unavailable %s: %v", base, err)
	}
	tar := exec.Command("tar", "-x", "-C", temp)
	tar.Stdin = bytes.NewReader(b)
	if out, e := tar.CombinedOutput(); e != nil {
		t.Fatalf("extract %s: %v", out, e)
	}
	helper := filepath.Join(temp, "hub", "fixture-reopen.go")
	program := `package main
import("os";"context";"database/sql";"time";"github.com/scs32/tailterm/hub/internal/store";"github.com/scs32/tailterm/hub/internal/api")
func main(){s,e:=store.Open(os.Args[1]);if e!=nil{panic(e)}
if os.Args[2]=="seed" {ctx:=context.Background();by:=api.Caller{Node:"fixture",User:"owner"};task,e:=s.CreateTask(ctx,api.CreateTaskRequest{Name:"base-created migration fixture"},by);if e!=nil{panic(e)};item,e:=s.CreateWorkItem(ctx,task.ID,api.CreateWorkItemRequest{Kind:"bug",Title:"base-created item",RequestID:"base-seed"},by);if e!=nil{panic(e)};a,e:=s.AddAgent(ctx,task.ID,api.AddAgentRequest{Name:"legacy",Host:"fixture",Session:"fixture",Runtime:"codex"},by);if e!=nil{panic(e)};m,e:=s.PostMessage(ctx,task.ID,api.PostMessageRequest{Text:"base fixture order"},by);if e!=nil{panic(e)};q,e:=sql.Open("sqlite",os.Args[1]);if e!=nil{panic(e)};_,e=q.Exec("INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,context_digest,context_json,created_at) VALUES(?,?,?,?,1,?,?,0,'fixture','{}',?)",a.ID,a.RunID,task.ID,item.ID,task.ID,m.Seq,time.Now().UTC().Format(time.RFC3339Nano));if e!=nil{panic(e)};
second,e:=s.CreateWorkItem(ctx,task.ID,api.CreateWorkItemRequest{Kind:"bug",Title:"base second item",RequestID:"base-second"},by);if e!=nil{panic(e)}
description:="synthetic second revision";_,e=s.UpdateWorkItem(ctx,task.ID,second.ID,api.UpdateWorkItemRequest{Revision:second.Revision,Description:&description},by);if e!=nil{panic(e)}
other,e:=s.CreateTask(ctx,api.CreateTaskRequest{Name:"base other project"},by);if e!=nil{panic(e)}
_,e=s.CreateWorkItem(ctx,other.ID,api.CreateWorkItemRequest{Kind:"bug",Title:"old not measured",RequestID:"base-unmeasured"},by);if e!=nil{panic(e)}
handler,e:=s.AddAgent(ctx,task.ID,api.AddAgentRequest{Name:"persistent-handler",AgentID:api.NewID("agt"),Role:api.AgentRoleDatabaseHandler,Host:"fixture",Session:"handler",Runtime:"codex"},by);if e!=nil{panic(e)}
_,e=s.PostMessage(ctx,task.ID,api.PostMessageRequest{To:handler.ID,RequestID:"base-shared-order",WorkItems:[]api.MessageWorkItem{{ItemTaskID:task.ID,ItemID:item.ID,ItemRevision:item.Revision,Relationship:"primary"},{ItemTaskID:task.ID,ItemID:second.ID,ItemRevision:2,Relationship:"related"}},Envelope:&api.Envelope{Kind:"request",To:handler.ID,Subject:"Synthetic handler bookkeeping",Body:api.EnvelopeBody{Ask:"Handle these two synthetic items"}}},by);if e!=nil{panic(e)}
_,e=s.ReportActivity(ctx,task.ID,a.ID,api.ActivityReport{RequestID:"base-activity",RunID:a.RunID,Activity:api.AgentActivity{State:"working",ObservedAt:time.Now().UTC(),Tokens:api.TokenTotals{Input:100,Cached:80,Output:3,Total:103}}});if e!=nil{panic(e)}
closed,e:=s.AddAgent(ctx,other.ID,api.AddAgentRequest{Name:"closed retained",Host:"fixture",Session:"closed",Runtime:"codex"},by);if e!=nil{panic(e)};_,e=s.CloseAgent(ctx,closed.ID,by);if e!=nil{panic(e)}
_,e=q.Exec("UPDATE agents SET cleanup_done=1 WHERE id=?",closed.ID);if e!=nil{panic(e)}
_,e=q.Exec("INSERT INTO profiles VALUES('synthetic-usage-profile','synthetic-hash',1,'2026-09-27T00:00:00Z',x'010203'); INSERT INTO profile_history VALUES('synthetic-usage-profile',1,'2026-09-27T00:00:00Z',x'010203')");if e!=nil{panic(e)}
q.Close()};if e=s.Close();e!=nil{panic(e)}}`
	if err = os.WriteFile(helper, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	runBase := func(mode string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "run", helper, path, mode)
		cmd.Dir = filepath.Join(temp, "hub")
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("base %s %s: %v", mode, out, e)
		}
	}
	t.Logf("plan base %s: create fixture database with base binary", base)
	runBase("seed")
	t.Log("open base-created fixture with candidate")
	current, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var task, item string
	if err = current.db.QueryRow(`SELECT task_id,id FROM work_items WHERE title='base-created item'`).Scan(&task, &item); err != nil {
		t.Fatal("base-created item missing", err)
	}
	// Add candidate-only fixture evidence and prove the previous binary preserves it.
	if _, err = current.db.Exec(`INSERT INTO verification_records VALUES(?,?,1,'fixture','migration-fixture','fixture','{"fixture":"candidate"}')`, task, item); err != nil {
		t.Fatal(err)
	}

	var metered api.Agent
	metered, err = current.GetAgent(context.Background(), func() string {
		var id string
		current.db.QueryRow(`SELECT id FROM agents WHERE name='legacy'`).Scan(&id)
		return id
	}())
	if err != nil {
		t.Fatal(err)
	}
	turn := syntheticUsageTurn("migration-usage")
	if _, err = current.ReportUsage(context.Background(), task, metered.ID, usageBatch(metered, "migration-metered", turn)); err != nil {
		t.Fatal(err)
	}
	if _, err = current.SetUsagePrices(context.Background(), task, api.UsagePriceRequest{RequestID: "migration-prices", ExpectedRevision: 0, Rows: []api.UsagePrice{{Runtime: "codex", Model: turn.Model, Currency: "USD", EffectiveAt: turn.At, Rates: map[string]string{"input": "2"}}}}); err != nil {
		t.Fatal(err)
	}
	current.Close()
	t.Log("reopen migrated fixture with base binary")
	runBase("reopen")
	t.Log("reopen fixture with candidate and check evidence/integrity/foreign keys")
	current, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	var count int
	if err = current.db.QueryRow(`SELECT count(*) FROM verification_records WHERE task_id=? AND item_id=?`, task, item).Scan(&count); err != nil || count != 1 {
		t.Fatal("candidate evidence lost", count, err)
	}

	report, readErr := current.Usage(context.Background(), task, api.UsageQuery{})
	if readErr != nil || report.Summary.Requests != 1 || report.Summary.Tokens["input"] != "21" || report.PriceRevision != 1 {
		t.Fatal("usage/prices lost across base reopen", report, readErr)
	}
	for query, want := range map[string]int{`SELECT count(*) FROM tasks`: 2, `SELECT count(*) FROM work_items`: 3, `SELECT count(*) FROM agents WHERE role='database_handler'`: 1, `SELECT count(*) FROM agents WHERE status='closed' AND cleanup_done=1`: 1, `SELECT count(*) FROM agent_activity`: 1, `SELECT count(*) FROM profiles WHERE username='synthetic-usage-profile' AND hex(envelope)='010203'`: 1, `SELECT count(*) FROM profile_history WHERE username='synthetic-usage-profile'`: 1, `SELECT count(*) FROM message_work_item_links`: 2, `SELECT count(*) FROM work_item_changes WHERE revision=2`: 1} {
		var got int
		if e := current.db.QueryRow(query).Scan(&got); e != nil || got != want {
			t.Fatal("retained fixture changed", query, got, want, e)
		}
	}
	var integrity string
	if err = current.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal(integrity, err)
	}
	rows, err := current.db.Query(`PRAGMA foreign_key_check`)
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
	seedPassingVerificationAt(t, s, item, candidate, "fixture")
}
func seedPassingVerificationAt(t *testing.T, s *Store, item api.WorkItem, candidate, repository string) {
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
	p := api.VerificationPlan{ItemID: item.ID, ItemTaskID: item.TaskID, AssignmentOwnershipDigest: verificationDigest([]string{"fixture"}), Version: 1, OperationKey: api.NewID("req"), Repository: repository, Commit: candidate, BaseCommit: candidateA, ItemRevision: item.Revision, ScopeRevision: item.ScopeRevision, AssignmentSeq: sc.AssignmentSeq, MatrixDigest: strings.Repeat("a", 64), ChecksDigest: verificationDigest(checks), VerifierAgentID: api.NewID("agt"), VerifierRunID: api.NewID("run"), Checks: checks}
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
	p.MaxAttempts = 3
	p.KnownFailures = []api.VerificationKnownFailure{{CheckID: p.Checks[0].ID, BugTaskID: p.ItemTaskID, BugID: p.ItemID}}
	for _, value := range []any{p, retryVerification(p, 1, 0), retryVerification(p, 1, 1, 1)} {
		raw, _ = json.Marshal(value)
		cmd = exec.Command("node", "--input-type=module", "-e", script)
		cmd.Dir = "../../.."
		cmd.Stdin = strings.NewReader(string(raw))
		out, err = cmd.Output()
		if err != nil || string(out) != verificationDigest(value) {
			t.Fatal("retry canonical digest mismatch", string(out), verificationDigest(value), err)
		}
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
	approval, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "verification-matrix-approval:" + strings.Repeat("c", 64)}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, key := range []string{"matrix-a", "matrix-b"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			newPlan := p
			newPlan.OperationKey = key
			newPlan.MatrixDigest = strings.Repeat("c", 64)
			newPlan.ApprovedMatrixDigest = newPlan.MatrixDigest
			newPlan.MatrixApprovalMessageSeq = approval.Seq
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

func TestVerificationRejectsUnapprovedCandidateMatrix(t *testing.T) {
	f, h, p := verificationFixture(t)
	p.MatrixDigest = strings.Repeat("c", 64)
	p.ApprovedMatrixDigest = p.MatrixDigest
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "weakened", AgentID: h.ID, RunID: h.RunID, Plan: &p}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("candidate self-approved matrix", err)
	}
	source, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: p.VerifierAgentID, RunID: p.VerifierRunID, Text: "verification-matrix-approval:" + p.MatrixDigest}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	p.MatrixApprovalMessageSeq = source.Seq
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "agent-approved", AgentID: h.ID, RunID: h.RunID, Plan: &p}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("agent masqueraded as owner approval", err)
	}
	rejection, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "Rejected: verification-matrix-approval:" + p.MatrixDigest}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	p.MatrixApprovalMessageSeq = rejection.Seq
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "quoted-owner-token", AgentID: h.ID, RunID: h.RunID, Plan: &p}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("owner quote mistaken for approval", err)
	}

}
func TestVerificationLegacyCompletionPreserved(t *testing.T) {
	f := newConvergenceFixture(t)
	request := f.review(t, candidateA)
	if _, err := f.post(f.resultEnv(candidateA, map[string]string{"a1": "pass", "a2": "pass"}, api.ReviewMetadata{Mode: "general"}), "", request.Seq, f.reviewer); err != nil {
		t.Fatal(err)
	}
	accept := api.Envelope{Kind: "notice", Subject: "Accept legacy fixture candidate", Body: api.EnvelopeBody{Text: "legacy review"}, Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: candidateA}}
	if _, err := f.post(accept, "", 0, api.Agent{}); err != nil {
		t.Fatal("legacy accept", err)
	}
	done := "done"
	if _, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, f.by); err != nil {
		t.Fatal("legacy completion", err)
	}
}

func retryVerification(p api.VerificationPlan, codes ...int) api.VerificationReceipt {
	r := passingVerification(p)
	for i := range r.Checks {
		c := &r.Checks[i]
		for j, code := range codes {
			start := time.Date(2026, 9, 26, 0, 0, j*2, 0, time.UTC)
			a := api.VerificationAttempt{Attempt: j + 1, StartedAt: start.Format(time.RFC3339), EndedAt: start.Add(time.Second).Format(time.RFC3339), DurationMs: 1000, ExitCode: code, LogURI: fmt.Sprintf("/tmp/check-%d-attempt-%d.log", i, j), LogDigest: strings.Repeat("b", 64)}
			if code != 0 {
				a.FailureReason = "exit"
			}
			c.Attempts = append(c.Attempts, a)
		}
		a := c.Attempts[len(c.Attempts)-1]
		c.StartedAt = a.StartedAt
		c.EndedAt = a.EndedAt
		c.DurationMs = a.DurationMs
		c.ExitCode = a.ExitCode
		c.FailureReason = a.FailureReason
		c.LogURI = a.LogURI
		c.LogDigest = a.LogDigest
		c.Status = "fail"
		if c.ExitCode == 0 {
			c.Status = "pass"
			if len(c.Attempts) > 1 {
				c.Status = "flaky"
			}
		}
		for _, e := range p.KnownFailures {
			if e.CheckID == c.ID {
				c.KnownFailure = true
				c.NowPassing = c.ExitCode == 0
			}
		}
	}
	return r
}
func knownVerificationFixture(t *testing.T) (*convergenceFixture, api.Agent, api.VerificationPlan, api.WorkItem) {
	f, h, p := verificationFixture(t)
	bug, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Known verification failure", RequestID: "known-bug"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	p.MaxAttempts = 3
	p.KnownFailures = []api.VerificationKnownFailure{{CheckID: p.Checks[0].ID, BugTaskID: f.task.ID, BugID: bug.ID}}
	return f, h, p, bug
}
func TestVerificationKnownFailureEvidenceAndAllCompletionGates(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, codes := range [][]int{{0}, {1, 0}, {1, 1, 0}, {1, 1, 1}} {
			t.Run(fmt.Sprintf("known=%v/codes=%v", known, codes), func(t *testing.T) {
				f, h, p, _ := knownVerificationFixture(t)
				if !known {
					p.KnownFailures = nil
				}
				request := f.review(t, p.Commit)
				if _, err := f.post(f.resultEnv(p.Commit, map[string]string{"a1": "pass", "a2": "pass"}, api.ReviewMetadata{Mode: "general"}), "", request.Seq, f.reviewer); err != nil {
					t.Fatal(err)
				}
				if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "new-plan", AgentID: h.ID, RunID: h.RunID, Plan: &p}); err != nil {
					t.Fatal(err)
				}
				r := retryVerification(p, codes...)
				if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "raw-receipt", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 1, Receipt: &r}); err != nil {
					t.Fatal("valid raw evidence rejected", err)
				}
				history, err := f.s.VerificationHistory(f.ctx, f.task.ID, f.item.ID, h.ID, h.RunID)
				if err != nil || !reflect.DeepEqual(history[1].Receipt, &r) {
					t.Fatal("roundtrip", err)
				}
				eligible := known || codes[len(codes)-1] == 0
				accept := api.Envelope{Kind: "notice", Subject: "Accept exact fixture candidate", Body: api.EnvelopeBody{Text: "accept"}, Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: p.Commit}}
				_, err = f.post(accept, "", 0, api.Agent{})
				if (err == nil) != eligible {
					t.Fatal("accept gate", err)
				}
				tx, _ := f.s.db.BeginTx(f.ctx, nil)
				err = reviewCompletion(f.ctx, tx, f.item, p.Commit)
				tx.Rollback()
				if (err == nil) != eligible {
					t.Fatal("queue gate", err)
				}
				done := "done"
				_, err = f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, f.by)
				if (err == nil) != eligible {
					t.Fatal("PATCH gate", err)
				}
				revision := int64(1)
				if eligible {
					revision = 2
				}
				_, _, err = f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, api.CreateWorkItemUpdate{RequestID: "keyed-done", ExpectedRevision: revision, Status: &done}, f.by)
				if (err == nil) != eligible {
					t.Fatal("keyed gate", err)
				}
			})
		}
	}
}
func TestVerificationMalformedAttemptsAndBugLinks(t *testing.T) {
	f, h, p, bug := knownVerificationFixture(t)
	feature, err := f.s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Not a bug", RequestID: "feature-link"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	for i, mutate := range []func(*api.VerificationPlan){
		func(p *api.VerificationPlan) { p.KnownFailures = append(p.KnownFailures, p.KnownFailures[0]) },
		func(p *api.VerificationPlan) { p.KnownFailures[0].CheckID = "unknown" },
		func(p *api.VerificationPlan) { p.KnownFailures[0].BugID = api.NewID("wi") },
		func(p *api.VerificationPlan) { p.KnownFailures[0].BugID = feature.ID },
		func(p *api.VerificationPlan) { p.MaxAttempts = 4 },
	} {
		b := p
		b.KnownFailures = append([]api.VerificationKnownFailure{}, p.KnownFailures...)
		mutate(&b)
		if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: fmt.Sprintf("bad-plan-%d", i), AgentID: h.ID, RunID: h.RunID, Plan: &b}); !errors.Is(err, api.ErrConflict) {
			t.Fatal(i, err)
		}
	}
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "plan", AgentID: h.ID, RunID: h.RunID, Plan: &p}); err != nil {
		t.Fatal(err)
	}
	for i, mutate := range []func(*api.VerificationReceipt){
		func(r *api.VerificationReceipt) { r.Checks[0].Attempts = nil },
		func(r *api.VerificationReceipt) { r.Checks[0].Attempts[0].Attempt = 2 },
		func(r *api.VerificationReceipt) {
			r.Checks[0].Attempts[0].ExitCode = 0
			r.Checks[0].Attempts[0].FailureReason = ""
		},
		func(r *api.VerificationReceipt) { r.Checks[0].Attempts[0].LogURI = r.Checks[0].Attempts[1].LogURI },
		func(r *api.VerificationReceipt) { r.Checks[0].Attempts[0].LogDigest = "bad" },
		func(r *api.VerificationReceipt) { r.Checks[0].Attempts[0].EndedAt = "bad" },
		func(r *api.VerificationReceipt) {
			r.Checks[0].Attempts[1].StartedAt = r.Checks[0].Attempts[0].StartedAt
		},
		func(r *api.VerificationReceipt) { r.Checks[0].ExitCode = 1 },
		func(r *api.VerificationReceipt) { r.Checks[0].Status = "pass" },
		func(r *api.VerificationReceipt) { r.Checks[0].KnownFailure = false },
		func(r *api.VerificationReceipt) { r.Checks[0].NowPassing = false },
		func(r *api.VerificationReceipt) {
			r.Checks[0].Attempts = append(r.Checks[0].Attempts, r.Checks[0].Attempts...)
		},
	} {
		r := retryVerification(p, 1, 0)
		mutate(&r)
		if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: fmt.Sprintf("bad-retry-%d", i), AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 1, Receipt: &r}); !errors.Is(err, api.ErrConflict) {
			t.Fatal(i, err)
		}
	}
	r := retryVerification(p, 1, 1)
	if err := validateVerificationReceipt(p, r); err == nil {
		t.Fatal("unexhausted failure accepted")
	}
	r = retryVerification(p, 1, 1, 1)
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "raw", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 1, Receipt: &r}); err != nil {
		t.Fatal(err)
	}
	done := "dismissed"
	if _, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, bug.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, f.by); err != nil {
		t.Fatal(err)
	}
	tx, _ := f.s.db.BeginTx(f.ctx, nil)
	err = verificationReady(f.ctx, tx, f.item, p.Commit)
	tx.Rollback()
	if !errors.Is(err, api.ErrConflict) {
		t.Fatal("closed bug still waived", err)
	}
	p.OperationKey = "closed-bug"
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "closed-bug", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 2, Plan: &p}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("closed bug plan", err)
	}
}
func TestVerificationKnownFailuresApprovalInvalidation(t *testing.T) {
	f, h, p, _ := knownVerificationFixture(t)
	rawBefore := `{"maxAttempts":3,"knownFailures":[]}`
	rawAfter := `{"maxAttempts":3,"knownFailures":[{"checkId":"fixture-check","bugId":"` + p.KnownFailures[0].BugID + `","bugTaskId":"` + p.ItemTaskID + `"}]}`
	p.MatrixDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(rawBefore)))
	p.ApprovedMatrixDigest = p.MatrixDigest
	approval, err := f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "verification-matrix-approval:" + p.MatrixDigest}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	p.MatrixApprovalMessageSeq = approval.Seq
	// Only knownFailures bytes changed; reusing the old owner's token must fail.
	p.MatrixDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(rawAfter)))
	p.ApprovedMatrixDigest = p.MatrixDigest
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "changed-list", AgentID: h.ID, RunID: h.RunID, Plan: &p}); !errors.Is(err, api.ErrConflict) {
		t.Fatal("old approval accepted changed knownFailures bytes", err)
	}
	approval, err = f.s.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{Text: "verification-matrix-approval:" + p.MatrixDigest}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	p.MatrixApprovalMessageSeq = approval.Seq
	if _, err = f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "new-approval", AgentID: h.ID, RunID: h.RunID, Plan: &p}); err != nil {
		t.Fatal("new approval rejected", err)
	}
}

func TestVerificationRetainsRawFailureAfterBugCloses(t *testing.T) {
	f, h, p, bug := knownVerificationFixture(t)
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "plan", AgentID: h.ID, RunID: h.RunID, Plan: &p}); err != nil {
		t.Fatal(err)
	}
	dismissed := "dismissed"
	if _, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, bug.ID, api.UpdateWorkItemRequest{Revision: 1, Status: &dismissed}, f.by); err != nil {
		t.Fatal(err)
	}
	r := retryVerification(p, 1, 1, 1)
	if _, err := f.s.SaveVerification(f.ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "raw-failure", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 1, Receipt: &r}); err != nil {
		t.Fatal("raw evidence lost after link closed", err)
	}
	tx, _ := f.s.db.BeginTx(f.ctx, nil)
	err := verificationReady(f.ctx, tx, f.item, p.Commit)
	tx.Rollback()
	if !errors.Is(err, api.ErrConflict) {
		t.Fatal("closed link allowed completion", err)
	}
}
