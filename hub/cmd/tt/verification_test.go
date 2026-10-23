package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestVerificationLogEvidenceRejectsTamperingAndMissingFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "check.log")
	content := []byte("isolated check passes\n")
	if err := os.WriteFile(p, content, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	r := api.VerificationReceipt{Checks: []api.VerificationResult{{LogURI: p, LogDigest: hex.EncodeToString(sum[:])}}}
	if err := verifyReceiptLogs(r); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("altered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyReceiptLogs(r); err == nil {
		t.Fatal("altered log accepted")
	}
	os.Remove(p)
	if err := verifyReceiptLogs(r); err == nil {
		t.Fatal("missing log accepted")
	}
}
func TestVerificationCLIRequiresExactHandlerInput(t *testing.T) {
	if err := cmdVerification(env{}, []string{"history", "--item", "wi_aaaaaaaaaaaaaaaa"}); err == nil {
		t.Fatal("unbound caller accepted")
	}
}

func TestVerificationTimeoutReceiptRoundTrip(t *testing.T) {
	raw := []byte(`{"checks":[{"id":"fixture","argv":["node","fixture.mjs"],"cwd":".","environment":{"VERIFICATION_TIMEOUT_MS":"1500"},"exitCode":124,"failureReason":"timeout"}]}`)
	var receipt api.VerificationReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Checks[0].FailureReason != "timeout" || receipt.Checks[0].ExitCode != 124 || receipt.Checks[0].Environment["VERIFICATION_TIMEOUT_MS"] != "1500" {
		t.Fatalf("timeout contract lost: %+v", receipt.Checks[0])
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var restored api.VerificationReceipt
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Checks[0].FailureReason != "timeout" {
		t.Fatal("native round trip lost timeout failure")
	}
	success, err := json.Marshal(api.VerificationResult{})
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(success, &result); err != nil {
		t.Fatal(err)
	}
	if _, exists := result["failureReason"]; exists {
		t.Fatal("successful result includes failure reason")
	}
}

func TestVerificationRetryChecksEveryAttemptLog(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "first.log"), filepath.Join(dir, "final.log")}
	attempts := []api.VerificationAttempt{}
	for i, p := range paths {
		data := []byte(string(rune('a' + i)))
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		attempts = append(attempts, api.VerificationAttempt{Attempt: i + 1, ExitCode: 1 - i, LogURI: p, LogDigest: hex.EncodeToString(hash[:])})
	}
	r := api.VerificationReceipt{Checks: []api.VerificationResult{{Attempts: attempts, Status: "flaky", LogURI: attempts[1].LogURI, LogDigest: attempts[1].LogDigest}}}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var restored api.VerificationReceipt
	if err = json.Unmarshal(raw, &restored); err != nil || len(restored.Checks[0].Attempts) != 2 || restored.Checks[0].Status != "flaky" {
		t.Fatal("retry roundtrip", err)
	}
	if err = verifyReceiptLogs(restored); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(paths[0], []byte("tampered first failure"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = verifyReceiptLogs(restored); err == nil {
		t.Fatal("first failed attempt log tampering accepted")
	}
}

// Validated verification operations run by the item lead and the plan's
// verifier through the CLI, against a loopback hub (wi_26c0698de7d3eef2 a7).

func cliDigest(v any) string {
	raw, _ := json.Marshal(v)
	var generic any
	_ = json.Unmarshal(raw, &generic)
	raw, _ = json.Marshal(generic)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// verificationCLI is a running team queue entry with an admitted lead,
// builder and verifier, the lead's ASSIGN, and an owner-approved plan and its
// passing receipt as files, with the receipt's log on disk.
type verificationCLI struct {
	teamFixture
	lead, builder, verifier api.Agent
	planFile, receiptFile   string
	logFile                 string
	entry                   api.TeamQueueEntry
}

func newVerificationCLI(t *testing.T) *verificationCLI {
	t.Helper()
	return newVerificationCLIWith(t, map[string]string{"a1": "works"}, nil)
}

// newVerificationCLIWith is newVerificationCLI with the ASSIGN's criteria and
// verification-owned criteria.
func newVerificationCLIWith(t *testing.T, acceptance map[string]string, owned []string) *verificationCLI {
	t.Helper()
	f := &verificationCLI{teamFixture: newTeamFixtureKind(t, true, "bug")}
	ctx := context.Background()
	messages, err := f.c.ListMessages(ctx, f.task.ID, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var order api.Message
	for _, m := range messages {
		if m.Seq == f.order {
			order = m
		}
	}
	action := func(req api.TeamQueueRequest) api.TeamQueueEntry {
		t.Helper()
		req.RequestID = api.NewID("req")
		q, err := f.c.TeamQueueAction(ctx, f.task.ID, req)
		if err != nil {
			t.Fatalf("%s: %v", req.Operation, err)
		}
		return q
	}
	entry := action(api.TeamQueueRequest{Operation: "add", ItemID: f.item.ID, OrderMessageSeq: f.order, Host: "fixture", Cwd: t.TempDir()})
	entry = action(api.TeamQueueRequest{Operation: "claim", EntryID: entry.ID, ExpectedRevision: entry.Revision, Host: "fixture"})
	member := func(name string) api.Agent {
		t.Helper()
		a, err := f.c.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "fixture", Session: name, Runtime: "codex",
			WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, WorkOrderMessage: api.MessageReference{TaskID: f.task.ID, Seq: f.order}, ContextBundle: teamCloseCLIContext(t, f.item, order)}})
		if err != nil {
			t.Fatalf("admit %s: %v", name, err)
		}
		if _, err = f.c.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventRunning}); err != nil {
			t.Fatal(err)
		}
		return a
	}
	f.lead, f.builder, f.verifier = member("lead"), member("builder"), member("verifier")
	launch, _ := json.Marshal(map[string]any{"task": f.task.ID, "item": f.item.ID, "revision": f.item.Revision, "order": f.order, "context": map[string]any{"version": 1},
		"members": []any{map[string]any{"state": "unstarted", "runId": f.lead.RunID, "fields": map[string]any{"agentId": f.lead.ID, "name": f.lead.Name, "cwd": entry.Cwd}}}})
	for _, step := range []api.TeamQueueRequest{{Operation: "freeze", LaunchJSON: launch}, {Operation: "attempt"}, {Operation: "started", MemberRunID: f.lead.RunID}, {Operation: "running"}} {
		step.EntryID, step.ExpectedRevision = entry.ID, entry.Revision
		entry = action(step)
	}
	f.entry = entry
	links := []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}
	assign, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{To: f.builder.ID, AgentID: f.lead.ID, RunID: f.lead.RunID, RequestID: "cli-assign", WorkItems: links,
		Envelope: &api.Envelope{Kind: "assign", Subject: "Implement frozen fixture criteria", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: acceptance, VerificationCriteria: owned}}})
	if err != nil {
		t.Fatal("assign", err)
	}
	checks := []api.VerificationCheck{{ID: "fixture-check", Argv: []string{"node", "fixture.js"}, Cwd: ".", Environment: map[string]string{}}}
	p := api.VerificationPlan{ItemID: f.item.ID, ItemTaskID: f.task.ID, AssignmentOwnershipDigest: cliDigest([]string{"fixture"}), Version: 1, OperationKey: "cli-operators", Repository: "fixture", BaseCommit: strings.Repeat("b", 40), Commit: strings.Repeat("a", 40),
		ItemRevision: f.item.Revision, ScopeRevision: f.item.ScopeRevision, OrderMessageSeq: f.order, AssignmentSeq: assign.Seq, BuilderAgentID: f.builder.ID, BuilderRunID: f.builder.RunID, VerifierAgentID: f.verifier.ID, VerifierRunID: f.verifier.RunID,
		MatrixDigest: strings.Repeat("a", 64), ChecksDigest: cliDigest(checks), Owned: []string{"fixture"}, Changed: []string{}, Checks: checks}
	approval, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "verification-matrix-approval:" + p.MatrixDigest})
	if err != nil {
		t.Fatal(err)
	}
	p.ApprovedMatrixDigest, p.MatrixApprovalMessageSeq = p.MatrixDigest, approval.Seq

	dir := t.TempDir()
	f.planFile, f.receiptFile, f.logFile = filepath.Join(dir, "plan.json"), filepath.Join(dir, "receipt.json"), filepath.Join(dir, "fixture-check.log")
	log := []byte("isolated check passes\n")
	sum := sha256.Sum256(log)
	r := api.VerificationReceipt{Worktree: dir, Version: 1, OperationKey: p.OperationKey, PlanDigest: cliDigest(p), Repository: p.Repository, BaseCommit: p.BaseCommit, Commit: p.Commit, MatrixDigest: p.MatrixDigest, ChecksDigest: p.ChecksDigest,
		VerifierAgentID: p.VerifierAgentID, VerifierRunID: p.VerifierRunID, Detached: true, CleanBefore: true, CleanAfter: true, Environment: map[string]string{"HOME": dir}, Prerequisites: []api.VerificationPrerequisite{}, AIV: api.AIVBinding{State: "unsubmitted"},
		Checks: []api.VerificationResult{{VerificationCheck: checks[0], StartedAt: "2026-09-26T00:00:00Z", EndedAt: "2026-09-26T00:00:01Z", DurationMs: 1000, LogURI: f.logFile, LogDigest: hex.EncodeToString(sum[:])}}}
	for path, value := range map[string]any{f.planFile: p, f.receiptFile: r} {
		data, _ := json.Marshal(value)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.logFile, log, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

// as is the fixture's CLI environment for one agent's exact run.
func (f *verificationCLI) as(a api.Agent) env {
	e := f.e
	e.agent, e.runID = a.ID, a.RunID
	return e
}

// run executes one tt verification command and decodes the record it prints.
func (f *verificationCLI) run(t *testing.T, e env, args ...string) (api.VerificationRecord, string, error) {
	t.Helper()
	out, err := captureCLIOutput(t, func() error { return cmdVerification(e, append(args, "--item", f.item.ID)) })
	var record api.VerificationRecord
	if err == nil {
		if decodeErr := json.Unmarshal([]byte(out), &record); decodeErr != nil {
			t.Fatalf("output %q: %v", out, decodeErr)
		}
	}
	return record, out, err
}

func (f *verificationCLI) plan(t *testing.T, e env) (api.VerificationRecord, string, error) {
	t.Helper()
	return f.run(t, e, "plan", "--file", f.planFile, "--request-id", "cli-plan")
}

func (f *verificationCLI) receipt(t *testing.T, e env) (api.VerificationRecord, string, error) {
	t.Helper()
	return f.run(t, e, "receipt", "--file", f.receiptFile, "--request-id", "cli-receipt", "--generation", "1")
}

func TestVerificationCLILeadFreezesPlan(t *testing.T) {
	f := newVerificationCLI(t)
	ctx := context.Background()
	lead := f.as(f.lead)
	bound, err := f.c.GetAgentWorkItemContext(ctx, f.task.ID, f.lead.ID, f.lead.RunID)
	if err != nil || bound.Binding.ContextDigest == "" {
		t.Fatalf("lead context %+v %v", bound.Binding, err)
	}
	sent, err := runContextDigest(ctx, f.c, lead)
	if err != nil || sent != bound.Binding.ContextDigest {
		t.Fatalf("digest the CLI sends = %q %v, want the lead's %q", sent, err, bound.Binding.ContextDigest)
	}
	record, _, err := f.plan(t, lead)
	if err != nil {
		t.Fatal("lead plan freeze", err)
	}
	if record.Generation != 1 || record.Kind != "plan" || record.AuthorRole != "lead" || record.HandlerAgentID != f.lead.ID || record.HandlerRunID != f.lead.RunID {
		t.Fatalf("lead plan record %+v", record)
	}
	// The hub requires that digest: the same call without it is refused.
	var p api.VerificationPlan
	data, _ := os.ReadFile(f.planFile)
	_ = json.Unmarshal(data, &p)
	if _, err = f.c.SaveVerification(ctx, f.task.ID, f.item.ID, api.VerificationRequest{RequestID: "no-digest", AgentID: f.lead.ID, RunID: f.lead.RunID, ExpectedGeneration: 1, Plan: &p}); err == nil || !strings.Contains(err.Error(), "context digest differs") {
		t.Fatalf("lead plan without a digest: %v", err)
	}
}

func TestVerificationCLIVerifierImportsReceipt(t *testing.T) {
	f := newVerificationCLI(t)
	if _, _, err := f.plan(t, f.as(f.lead)); err != nil {
		t.Fatal("lead plan freeze", err)
	}
	verifier := f.as(f.verifier)
	// Logs are checked from disk before any hub write.
	log, _ := os.ReadFile(f.logFile)
	if err := os.WriteFile(f.logFile, []byte("altered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.receipt(t, verifier); err == nil || !strings.Contains(err.Error(), "verification log digest mismatch") {
		t.Fatalf("altered log: %v", err)
	}
	if err := os.WriteFile(f.logFile, log, 0600); err != nil {
		t.Fatal(err)
	}
	record, out, err := f.receipt(t, verifier)
	if err != nil {
		t.Fatal("verifier receipt import", err)
	}
	if record.Generation != 2 || record.Kind != "receipt" || record.AuthorRole != "verifier" || record.HandlerAgentID != f.verifier.ID || len(record.Notices) != 2 {
		t.Fatalf("verifier receipt record %+v", record)
	}
	if n := record.Notices; n[0].Role != "lead" || n[0].AgentID != f.lead.ID || n[0].MessageSeq == 0 || n[1].Role != "handler" || n[1].AgentID != f.handler.ID || n[1].MessageSeq == 0 {
		t.Fatalf("notices %+v", n)
	}
	if !strings.Contains(out, `"notices"`) || !strings.Contains(out, `"messageSeq"`) {
		t.Fatalf("output does not print the notices: %s", out)
	}
	// The builder is neither the plan's verifier nor the lead.
	if _, _, err := f.run(t, f.as(f.builder), "receipt", "--file", f.receiptFile, "--request-id", "builder-receipt", "--generation", "1"); err == nil || !strings.Contains(err.Error(), "only the plan's verifier, the item lead or the database handler may import a receipt") {
		t.Fatalf("builder receipt: %v", err)
	}
}

func TestVerificationCLIHandlerWithoutBinding(t *testing.T) {
	f := newVerificationCLI(t)
	ctx := context.Background()
	handler := f.as(f.handler)
	if sent, err := runContextDigest(ctx, f.c, handler); err != nil || sent != "" {
		t.Fatalf("handler digest = %q %v, want none", sent, err)
	}
	plan, _, err := f.plan(t, handler)
	if err != nil || plan.Generation != 1 || plan.AuthorRole != "" || plan.HandlerAgentID != f.handler.ID {
		t.Fatalf("handler plan %+v %v", plan, err)
	}
	receipt, _, err := f.receipt(t, handler)
	if err != nil || receipt.Generation != 2 || receipt.AuthorRole != "" || len(receipt.Notices) != 1 || receipt.Notices[0].Role != "lead" {
		t.Fatalf("handler receipt %+v %v", receipt, err)
	}
	out, err := captureCLIOutput(t, func() error { return cmdVerification(handler, []string{"history", "--item", f.item.ID}) })
	var history []api.VerificationRecord
	if err != nil || json.Unmarshal([]byte(out), &history) != nil || len(history) != 2 {
		t.Fatalf("handler history %q %v", out, err)
	}
	if strings.Contains(out, "contextDigest") || strings.Contains(out, "authorRole") {
		t.Fatalf("handler records carry a digest or role: %s", out)
	}
}

func TestVerificationCLIReplay(t *testing.T) {
	f := newVerificationCLI(t)
	ctx := context.Background()
	lead, verifier := f.as(f.lead), f.as(f.verifier)
	first, firstOut, err := f.plan(t, lead)
	if err != nil {
		t.Fatal(err)
	}
	again, againOut, err := f.plan(t, lead)
	if err != nil || againOut != firstOut || again.Generation != first.Generation {
		t.Fatalf("plan replay %q vs %q: %v", againOut, firstOut, err)
	}
	_, receiptOut, err := f.receipt(t, verifier)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.c.ListMessages(ctx, f.task.ID, 0, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	_, replayOut, err := f.receipt(t, verifier)
	if err != nil || replayOut != receiptOut {
		t.Fatalf("receipt replay %q vs %q: %v", replayOut, receiptOut, err)
	}
	after, err := f.c.ListMessages(ctx, f.task.ID, 0, "", 1000)
	if err != nil || len(after) != len(before) {
		t.Fatalf("receipt replay posted: %d -> %d messages (%v)", len(before), len(after), err)
	}
	history, err := f.c.VerificationHistory(ctx, f.task.ID, f.item.ID, f.lead.ID, f.lead.RunID)
	if err != nil || len(history) != 2 {
		t.Fatalf("history after replays: %d records %v", len(history), err)
	}
}

// a2 (wi_9f6ca42e3acb9e22): the lead or the handler carries the receipt over
// an owner edit with tt verification carry-over against the real route, and
// tt verification history reads the record back. The plan's verifier is
// refused, and a file that sets a hub-filled field is refused before any
// write.
func TestVerificationCLICarryOver(t *testing.T) {
	for _, author := range []string{"lead", "handler"} {
		t.Run(author, func(t *testing.T) {
			f := newVerificationCLIWith(t, map[string]string{"a1": "works", "a2": "matrix passes"}, []string{"a2"})
			ctx := context.Background()
			if _, _, err := f.plan(t, f.as(f.lead)); err != nil {
				t.Fatal("plan", err)
			}
			if _, _, err := f.receipt(t, f.as(f.verifier)); err != nil {
				t.Fatal("receipt", err)
			}
			linked, err := f.c.CreateWorkItem(ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "matrix follow-up", RequestID: "linked"})
			if err != nil {
				t.Fatal(err)
			}
			description := "a2 moved to the follow-up"
			updated, err := f.c.UpdateWorkItem(ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Description: &description, ScopeChange: true})
			if err != nil {
				t.Fatal(err)
			}
			edit, err := f.c.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "owner moves a2", RequestID: "owner-edit", WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: updated.Revision, Relationship: "primary"}}})
			if err != nil {
				t.Fatal(err)
			}
			// The handler confirms the amended criteria and owned paths
			// through tt work-items scope confirm (wi_94e5cad697b5beb2 k1).
			criteriaFile := filepath.Join(t.TempDir(), "criteria.json")
			if err = os.WriteFile(criteriaFile, []byte(`{"a1":"works"}`), 0600); err != nil {
				t.Fatal(err)
			}
			revision, scope, order := strconv.FormatInt(updated.Revision, 10), strconv.FormatInt(updated.ScopeRevision, 10), strconv.FormatInt(f.order, 10)
			for name, body := range map[string]string{"empty object": `{}`, "invalid JSON": `{"a1":`, "missing": ""} {
				bad := filepath.Join(t.TempDir(), "criteria.json")
				if body != "" {
					if err = os.WriteFile(bad, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
				err = cmdWorkOrderScope(f.as(f.handler), []string{"confirm", "--complete", "--request-id", "bad-criteria", "--revision", revision, "--scope-revision", scope, "--order", order, "--owns", "fixture", "--criteria-file", bad, f.item.ID})
				if err == nil || !strings.HasPrefix(err.Error(), "criteria file: ") {
					t.Fatalf("%s criteria file: %v", name, err)
				}
			}
			out, err := captureCLIOutput(t, func() error {
				return cmdWorkOrderScope(f.as(f.handler), []string{"confirm", "--complete", "--request-id", "amended-scope", "--revision", revision, "--scope-revision", scope, "--order", order, "--owns", "fixture", "--criteria-file", criteriaFile, f.item.ID})
			})
			var confirmed api.WorkOrderScopeConfirmation
			if err != nil || json.Unmarshal([]byte(out), &confirmed) != nil || !reflect.DeepEqual(confirmed.Criteria, map[string]string{"a1": "works"}) || !reflect.DeepEqual(confirmed.Ownership, []string{"fixture"}) {
				t.Fatalf("scope confirm %q %v", out, err)
			}
			out, err = captureCLIOutput(t, func() error {
				return cmdWorkOrderScope(f.as(f.handler), []string{"get", "--revision", revision, "--order", order, f.item.ID})
			})
			var readBack api.WorkOrderScopeConfirmation
			if err != nil || json.Unmarshal([]byte(out), &readBack) != nil || !reflect.DeepEqual(readBack, confirmed) {
				t.Fatalf("scope get %q %v", out, err)
			}
			entry, err := f.c.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "rebind", Operation: "rebind", EntryID: f.entry.ID, ExpectedRevision: f.entry.Revision, ItemRevision: updated.Revision, SourceMessageSeq: edit.Seq})
			if err != nil || len(entry.Rebinds) != 1 {
				t.Fatal("rebind", entry.Rebinds, err)
			}
			history, err := f.c.VerificationHistory(ctx, f.task.ID, f.item.ID, f.handler.ID, f.handler.RunID)
			if err != nil || len(history) != 2 {
				t.Fatal(history, err)
			}
			p, r := history[0].Plan, history[1].Receipt
			c := api.VerificationCarryOver{ItemRevision: updated.Revision, SourceGeneration: 2, SourceReceiptDigest: cliDigest(r), SourcePlanDigest: cliDigest(p), Commit: p.Commit, MatrixDigest: p.MatrixDigest, MatrixApprovalMessageSeq: p.MatrixApprovalMessageSeq, ChecksDigest: p.ChecksDigest,
				FromScopeRevision: p.ScopeRevision, ToScopeRevision: updated.ScopeRevision, RebindID: entry.Rebinds[0].ID, OwnerEditMessageSeq: edit.Seq,
				Moved: []api.VerificationMovedCriterion{{ID: "a2", Text: "matrix passes", ItemTaskID: f.task.ID, ItemID: linked.ID, ItemRevision: linked.Revision}}}
			dir := t.TempDir()
			file := filepath.Join(dir, "carry-over.json")
			data, _ := json.Marshal(c)
			if err = os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			forged := c
			forged.Plan = p
			forgedFile := filepath.Join(dir, "forged.json")
			data, _ = json.Marshal(forged)
			if err = os.WriteFile(forgedFile, data, 0600); err != nil {
				t.Fatal(err)
			}
			carry := func(e env, file, key string) (api.VerificationRecord, string, error) {
				return f.run(t, e, "carry-over", "--file", file, "--request-id", key, "--generation", "2")
			}
			if _, _, err = carry(f.as(f.lead), forgedFile, "forged"); err == nil || !strings.Contains(err.Error(), "must not set plan, receipt or amendedBy") {
				t.Fatalf("forged file: %v", err)
			}
			if _, _, err = carry(f.as(f.verifier), file, "verifier-carry"); err == nil || !strings.Contains(err.Error(), "only the item lead or the database handler may carry a receipt over") {
				t.Fatalf("verifier carry-over: %v", err)
			}
			by, role := f.lead, "lead"
			if author == "handler" {
				by, role = f.handler, ""
			}
			record, out, err := carry(f.as(by), file, "carry")
			if err != nil {
				t.Fatal("carry-over", err)
			}
			if record.Kind != "carry-over" || record.Generation != 3 || record.AuthorRole != role || record.HandlerAgentID != by.ID || record.CarryOver == nil || record.CarryOver.Plan == nil || record.CarryOver.Plan.ScopeRevision != updated.ScopeRevision || record.CarryOver.AmendedBy == nil {
				t.Fatalf("carry-over record %+v", record)
			}
			if !strings.Contains(out, `"carryOver"`) || !strings.Contains(out, `"rebindId"`) {
				t.Fatalf("output does not print the carry-over: %s", out)
			}
			out, err = captureCLIOutput(t, func() error { return cmdVerification(f.as(f.handler), []string{"history", "--item", f.item.ID}) })
			var read []api.VerificationRecord
			if err != nil || json.Unmarshal([]byte(out), &read) != nil || len(read) != 3 || !reflect.DeepEqual(read[2], record) {
				t.Fatalf("history %q %v", out, err)
			}
		})
	}
}
