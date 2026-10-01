package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/testverification"
)

func prepareHTTPVerification(t *testing.T, c *client, task string, item api.WorkItem) {
	t.Helper()
	native, err := api.NewClient(c.srv.URL, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = testverification.Prepare(native, task, item); err != nil {
		t.Fatal(err)
	}
}
func TestVerificationHTTPPreservesLegacyAndRefusesEnrolledMissingReceipt(t *testing.T) {
	c := newClient(t)
	task := c.task("verification-http")
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "Verification fixture", RequestID: "item"}, &item); code != 201 {
		t.Fatal(code)
	}
	path := "/v1/tasks/" + task.ID + "/work-items/" + item.ID
	done := "done"
	if code := c.do("PATCH", path, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, nil); code != 200 {
		t.Fatal("legacy completion", code)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "enrolled fixture", RequestID: "enrolled"}, &item); code != 201 {
		t.Fatal(code)
	}
	path = "/v1/tasks/" + task.ID + "/work-items/" + item.ID
	native, err := api.NewClient(c.srv.URL, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = testverification.PreparePending(native, task.ID, item); err != nil {
		t.Fatal(err)
	}
	if code := c.do("PATCH", path, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, nil); code != 409 {
		t.Fatal("enrolled missing receipt", code)
	}
	if code := c.do("GET", path+"/verification?agent=agt_0000000000000000&run=run_0000000000000000", nil, nil); code != 409 {
		t.Fatal("nonhandler", code)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "passing fixture", RequestID: "passing"}, &item); code != 201 {
		t.Fatal(code)
	}
	path = "/v1/tasks/" + task.ID + "/work-items/" + item.ID
	prepareHTTPVerification(t, c, task.ID, item)
	if code := c.do("PATCH", path, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, nil); code != 200 {
		t.Fatal("passing receipt", code)
	}
}

func TestVerificationHTTPRetryEvidenceRoundTrip(t *testing.T) {
	c := newClient(t)
	task := c.task("verification-retry-http")
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "Retry contract", RequestID: "retry-item"}, &item); code != 201 {
		t.Fatal(code)
	}
	native, err := api.NewClient(c.srv.URL, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = testverification.PreparePending(native, task.ID, item); err != nil {
		t.Fatal(err)
	}
	agents, err := native.ListAgents(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var handler api.Agent
	for _, a := range agents {
		if a.Role == api.AgentRoleDatabaseHandler {
			handler = a
		}
	}
	history, err := native.VerificationHistory(context.Background(), task.ID, item.ID, handler.ID, handler.RunID)
	if err != nil {
		t.Fatal(err)
	}
	p := *history[0].Plan
	p.OperationKey = "http-retry"
	p.MaxAttempts = 3
	path := "/v1/tasks/" + task.ID + "/work-items/" + item.ID + "/verification"
	var saved api.VerificationRecord
	if code := c.do("POST", path, api.VerificationRequest{RequestID: "retry-plan", AgentID: handler.ID, RunID: handler.RunID, ExpectedGeneration: 1, Plan: &p}, &saved); code != 201 {
		t.Fatal("retry plan", code)
	}
	raw, _ := json.Marshal(p)
	var generic any
	_ = json.Unmarshal(raw, &generic)
	raw, _ = json.Marshal(generic)
	hash := sha256.Sum256(raw)
	attempts := []api.VerificationAttempt{
		{Attempt: 1, StartedAt: "2026-09-26T00:00:00Z", EndedAt: "2026-09-26T00:00:01Z", DurationMs: 1000, ExitCode: 1, FailureReason: "exit", LogURI: "/tmp/first.log", LogDigest: strings.Repeat("b", 64)},
		{Attempt: 2, StartedAt: "2026-09-26T00:00:02Z", EndedAt: "2026-09-26T00:00:03Z", DurationMs: 1000, LogURI: "/tmp/second.log", LogDigest: strings.Repeat("c", 64)},
	}
	last := attempts[1]
	r := api.VerificationReceipt{Version: 1, Worktree: "/tmp/fixture", OperationKey: p.OperationKey, PlanDigest: hex.EncodeToString(hash[:]), Repository: p.Repository, BaseCommit: p.BaseCommit, Commit: p.Commit, MatrixDigest: p.MatrixDigest, ChecksDigest: p.ChecksDigest, VerifierAgentID: p.VerifierAgentID, VerifierRunID: p.VerifierRunID, Detached: true, CleanBefore: true, CleanAfter: true, Environment: map[string]string{"HOME": "/tmp/isolated"}, AIV: api.AIVBinding{State: "unsubmitted"}, Checks: []api.VerificationResult{{VerificationCheck: p.Checks[0], Attempts: attempts, Status: "flaky", StartedAt: last.StartedAt, EndedAt: last.EndedAt, DurationMs: last.DurationMs, LogURI: last.LogURI, LogDigest: last.LogDigest}}}
	bad := r
	bad.Checks = append([]api.VerificationResult{}, r.Checks...)
	bad.Checks[0].Status = "pass"
	if code := c.do("POST", path, api.VerificationRequest{RequestID: "forged-status", AgentID: handler.ID, RunID: handler.RunID, ExpectedGeneration: 2, Receipt: &bad}, nil); code != 409 {
		t.Fatal("forged pass", code)
	}
	if code := c.do("POST", path, api.VerificationRequest{RequestID: "retry-receipt", AgentID: handler.ID, RunID: handler.RunID, ExpectedGeneration: 2, Receipt: &r}, &saved); code != 201 {
		t.Fatal("retry receipt", code)
	}
	history, err = native.VerificationHistory(context.Background(), task.ID, item.ID, handler.ID, handler.RunID)
	if err != nil || len(history) != 3 || history[2].Receipt.Checks[0].Status != "flaky" || len(history[2].Receipt.Checks[0].Attempts) != 2 || history[2].Receipt.Checks[0].Attempts[0].ExitCode != 1 {
		t.Fatal("HTTP evidence lost", err)
	}
}

func TestVerificationHTTPAcceptsFullMatrixReceiptOnly(t *testing.T) {
	c := newClient(t)
	task := c.task("verification-full-matrix")
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "Full matrix receipt", RequestID: "full-matrix"}, &item); code != 201 {
		t.Fatal(code)
	}
	native, err := api.NewClient(c.srv.URL, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = testverification.PrepareMatrix(native, task.ID, item, 69); err != nil {
		t.Fatal("full matrix import", err)
	}
	agents, err := native.ListAgents(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var handler api.Agent
	for _, a := range agents {
		if a.Role == api.AgentRoleDatabaseHandler {
			handler = a
		}
	}
	history, err := native.VerificationHistory(context.Background(), task.ID, item.ID, handler.ID, handler.RunID)
	if err != nil || len(history) != 2 || history[1].Receipt == nil || len(history[1].Receipt.Checks) != 69 {
		t.Fatal("full matrix receipt lost", err)
	}
	last := history[1].Receipt.Checks[68]
	if last.ID != "068-tests/fixture-068-browser.mjs" || last.LogURI != "/tmp/tailterm-verification-fixture-matrix/logs/068-tests-fixture-068-browser.mjs-attempt-1.log" || last.Environment["VERIFICATION_BASE_COMMIT"] != testverification.Commit {
		t.Fatalf("full matrix receipt changed: %+v", last)
	}
	raw, _ := json.Marshal(api.VerificationRequest{RequestID: "size", AgentID: handler.ID, RunID: handler.RunID, ExpectedGeneration: 2, Receipt: history[1].Receipt})
	if len(raw) <= api.MaxBody {
		t.Fatalf("fixture receipt is %d bytes, not over the shared %d-byte limit", len(raw), api.MaxBody)
	}

	path := "/v1/tasks/" + task.ID + "/work-items/" + item.ID
	post := func(route, body string) (int, api.ErrorResponse) {
		var out api.ErrorResponse
		status := c.do("POST", path+route, body, &out)
		return status, out
	}
	const invalid = "invalid scope metadata request"
	pad := func(n int) string {
		body := `{"requestId":"pad"}`
		return body + strings.Repeat(" ", n-len(body))
	}
	if _, got := post("/verification", pad(api.MaxVerificationBody)); strings.HasPrefix(got.Error, invalid) {
		t.Fatal("verification body at the limit was not decoded", got)
	}
	// Each refusal names its reason in the text the CLI prints, and its code.
	for _, tc := range []struct{ route, body, want, code string }{
		{"/verification", pad(api.MaxVerificationBody + 1), invalid + ": body exceeds 1048576 bytes", "body-too-large"},
		{"/verification", `{"requestId":"unknown","bogus":1}`, invalid + `: unknown field "bogus"`, "unknown-field"},
		{"/verification", `{"requestId":"trailing"} {}`, invalid + ": trailing data after the JSON object", "trailing-data"},
		{"/verification", `{"requestId":7}`, invalid + `: wrong type for field "requestId"`, "wrong-type"},
		{"/verification", `{"requestId":`, invalid + ": malformed JSON", "malformed-json"},
		{"/verification", `{"requestId":"long","` + strings.Repeat("x", 4096) + `":1}`, invalid + `: unknown field "` + strings.Repeat("x", 64) + `..."`, "unknown-field"},
		{"/order-scope/confirm", pad(api.MaxBody + 1), invalid + ": body exceeds 65536 bytes", "body-too-large"},
	} {
		if status, got := post(tc.route, tc.body); status != 400 || got.Error != tc.want || got.Code != tc.code {
			t.Fatalf("%s %.40q: %d %+v, want %q %q", tc.route, tc.body, status, got, tc.want, tc.code)
		}
	}
}

// Validated verification operations over HTTP for the item lead and the
// plan's verifier (wi_26c0698de7d3eef2 a7).

func httpDigest(v any) string {
	raw, _ := json.Marshal(v)
	var generic any
	_ = json.Unmarshal(raw, &generic)
	raw, _ = json.Marshal(generic)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// operatorHTTP is a running team queue entry with an admitted lead, builder,
// verifier and reviewer, the lead's ASSIGN and REVIEW, and an owner-approved
// plan. The handler leases the entry.
type operatorHTTP struct {
	c                                        *client
	hub                                      *api.Client
	task                                     api.Task
	item                                     api.WorkItem
	handler, lead, builder, verifier, review api.Agent
	plan                                     api.VerificationPlan
	path                                     string
}

func newOperatorHTTP(t *testing.T) *operatorHTTP {
	t.Helper()
	c := newClient(t)
	st, ctx := c.st, context.Background()
	f := &operatorHTTP{c: c, task: c.task("verification-operators")}
	var err error
	if f.hub, err = api.NewClient(c.srv.URL, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	live := func(a api.Agent) {
		t.Helper()
		if _, err := st.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventRunning}, c.who); err != nil {
			t.Fatal(err)
		}
	}
	if f.handler, err = st.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "handler", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "handler"}, c.who); err != nil {
		t.Fatal(err)
	}
	live(f.handler)
	if code := c.do("POST", "/v1/tasks/"+f.task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "Operator fixture", RequestID: "item"}, &f.item); code != 201 {
		t.Fatalf("item = %d", code)
	}
	links := []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, Relationship: "primary"}}
	order, err := st.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "bounded order", RequestID: "order", WorkItems: links}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConfirmWorkOrderScope(ctx, f.task.ID, f.item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: "scope", AgentID: f.handler.ID, RunID: f.handler.RunID, ExpectedRevision: f.item.Revision, ScopeRevision: f.item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}); err != nil {
		t.Fatal(err)
	}
	action := func(req api.TeamQueueRequest) api.TeamQueueEntry {
		t.Helper()
		req.RequestID = api.NewID("req")
		q, err := f.hub.TeamQueueAction(ctx, f.task.ID, req)
		if err != nil {
			t.Fatalf("%s: %v", req.Operation, err)
		}
		return q
	}
	entry := action(api.TeamQueueRequest{Operation: "add", ItemID: f.item.ID, OrderMessageSeq: order.Seq, Host: "fixture", Cwd: t.TempDir()})
	entry = action(api.TeamQueueRequest{Operation: "claim", EntryID: entry.ID, ExpectedRevision: entry.Revision, Host: "fixture"})
	ref := api.MessageReference{TaskID: f.task.ID, Seq: order.Seq}
	member := func(name string) api.Agent {
		t.Helper()
		a, err := st.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: name, Host: "fixture", Session: name,
			WorkItem: &api.AgentWorkItemRequest{ItemTaskID: f.task.ID, ItemID: f.item.ID, ItemRevision: f.item.Revision, WorkOrderMessage: ref, ContextBundle: syntheticServerTestContext(t, f.item, ref, order)}}, c.who)
		if err != nil {
			t.Fatalf("admit %s: %v", name, err)
		}
		live(a)
		return a
	}
	f.lead, f.builder, f.verifier, f.review = member("lead"), member("builder"), member("verifier"), member("reviewer")
	launch, _ := json.Marshal(map[string]any{"task": f.task.ID, "item": f.item.ID, "revision": f.item.Revision, "order": order.Seq, "context": map[string]any{"version": 1},
		"members": []any{map[string]any{"state": "unstarted", "runId": f.lead.RunID, "fields": map[string]any{"agentId": f.lead.ID, "name": f.lead.Name, "cwd": entry.Cwd}}}})
	for _, step := range []api.TeamQueueRequest{{Operation: "freeze", LaunchJSON: launch}, {Operation: "attempt"}, {Operation: "started", MemberRunID: f.lead.RunID}, {Operation: "running"}} {
		step.EntryID, step.ExpectedRevision = entry.ID, entry.Revision
		entry = action(step)
	}
	asLead := func(env api.Envelope, to api.Agent) api.Message {
		t.Helper()
		m, err := st.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Envelope: &env, To: to.ID, AgentID: f.lead.ID, RunID: f.lead.RunID, RequestID: api.NewID("req"), WorkItems: links}, c.who)
		if err != nil {
			t.Fatalf("%s: %v", env.Kind, err)
		}
		return m
	}
	criteria := map[string]string{"a1": "works"}
	assign := asLead(api.Envelope{Kind: "assign", Subject: "Implement frozen fixture criteria", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: criteria}}, f.builder)
	checks := []api.VerificationCheck{{ID: "fixture-check", Argv: []string{"node", "fixture.js"}, Cwd: ".", Environment: map[string]string{}}}
	commit, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	f.plan = api.VerificationPlan{ItemID: f.item.ID, ItemTaskID: f.task.ID, AssignmentOwnershipDigest: httpDigest([]string{"fixture"}), Version: 1, OperationKey: "http-operators", Repository: "fixture", BaseCommit: base, Commit: commit,
		ItemRevision: f.item.Revision, ScopeRevision: f.item.ScopeRevision, OrderMessageSeq: order.Seq, AssignmentSeq: assign.Seq, BuilderAgentID: f.builder.ID, BuilderRunID: f.builder.RunID, VerifierAgentID: f.verifier.ID, VerifierRunID: f.verifier.RunID,
		MatrixDigest: strings.Repeat("a", 64), ChecksDigest: httpDigest(checks), Owned: []string{"fixture"}, Changed: []string{}, Checks: checks}
	approval, err := st.PostMessage(ctx, f.task.ID, api.PostMessageRequest{Text: "verification-matrix-approval:" + f.plan.MatrixDigest}, c.who)
	if err != nil {
		t.Fatal(err)
	}
	f.plan.ApprovedMatrixDigest, f.plan.MatrixApprovalMessageSeq = f.plan.MatrixDigest, approval.Seq
	asLead(api.Envelope{Kind: "review", Subject: "Review frozen fixture candidate", Body: api.EnvelopeBody{Candidate: commit, Scope: "Fixture", Acceptance: criteria}}, f.review)
	f.path = "/v1/tasks/" + f.task.ID + "/work-items/" + f.item.ID + "/verification"
	return f
}

// digest is the context digest the agent's run was admitted with.
func (f *operatorHTTP) digest(t *testing.T, a api.Agent) string {
	t.Helper()
	bound, err := f.hub.GetAgentWorkItemContext(context.Background(), f.task.ID, a.ID, a.RunID)
	if err != nil || bound.Binding.ContextDigest == "" {
		t.Fatalf("context of %s: %+v %v", a.Name, bound.Binding, err)
	}
	return bound.Binding.ContextDigest
}

func (f *operatorHTTP) planRequest(t *testing.T, key string) api.VerificationRequest {
	t.Helper()
	p := f.plan
	return api.VerificationRequest{RequestID: key, AgentID: f.lead.ID, RunID: f.lead.RunID, ContextDigest: f.digest(t, f.lead), Plan: &p}
}

func (f *operatorHTTP) receipt() api.VerificationReceipt {
	p := f.plan
	return api.VerificationReceipt{Worktree: "/tmp/fixture", Version: 1, OperationKey: p.OperationKey, PlanDigest: httpDigest(p), Repository: p.Repository, BaseCommit: p.BaseCommit, Commit: p.Commit, MatrixDigest: p.MatrixDigest, ChecksDigest: p.ChecksDigest,
		VerifierAgentID: p.VerifierAgentID, VerifierRunID: p.VerifierRunID, Detached: true, CleanBefore: true, CleanAfter: true, Environment: map[string]string{"HOME": "/tmp/fixture"}, Prerequisites: []api.VerificationPrerequisite{}, AIV: api.AIVBinding{State: "unsubmitted"},
		Checks: []api.VerificationResult{{VerificationCheck: p.Checks[0], StartedAt: "2026-09-26T00:00:00Z", EndedAt: "2026-09-26T00:00:01Z", DurationMs: 1000, LogURI: "/tmp/fixture.log", LogDigest: strings.Repeat("b", 64)}}}
}

// counts reads the item's record count as the handler and the project's
// message count.
func (f *operatorHTTP) counts(t *testing.T) (records, messages int) {
	t.Helper()
	ctx := context.Background()
	history, err := f.hub.VerificationHistory(ctx, f.task.ID, f.item.ID, f.handler.ID, f.handler.RunID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.hub.ListMessages(ctx, f.task.ID, 0, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(history), len(list)
}

func TestVerificationHTTPLeadAndVerifier(t *testing.T) {
	f := newOperatorHTTP(t)
	var plan, receipt api.VerificationRecord
	if code := f.c.do("POST", f.path, f.planRequest(t, "lead-plan"), &plan); code != 201 || plan.Generation != 1 || plan.AuthorRole != "lead" || plan.HandlerAgentID != f.lead.ID || len(plan.Notices) != 0 {
		t.Fatalf("lead plan = %d %+v", code, plan)
	}
	r := f.receipt()
	req := api.VerificationRequest{RequestID: "verifier-receipt", AgentID: f.verifier.ID, RunID: f.verifier.RunID, ContextDigest: f.digest(t, f.verifier), ExpectedGeneration: 1, Receipt: &r}
	if code := f.c.do("POST", f.path, req, &receipt); code != 201 || receipt.Generation != 2 || receipt.AuthorRole != "verifier" || len(receipt.Notices) != 3 {
		t.Fatalf("verifier receipt = %d %+v", code, receipt)
	}
	for i, want := range []struct {
		role  string
		agent api.Agent
	}{{"lead", f.lead}, {"reviewer", f.review}, {"handler", f.handler}} {
		if n := receipt.Notices[i]; n.Role != want.role || n.AgentID != want.agent.ID || n.MessageSeq == 0 || n.Skipped != "" {
			t.Fatalf("notice %d = %+v, want %s to %s", i, n, want.role, want.agent.ID)
		}
	}
	var history []api.VerificationRecord
	if code := f.c.do("GET", f.path+"?agent="+f.lead.ID+"&run="+f.lead.RunID, nil, &history); code != 200 || len(history) != 2 || history[0].AuthorRole != "lead" || len(history[1].Notices) != 3 || history[1].Notices[0].MessageSeq != receipt.Notices[0].MessageSeq {
		t.Fatalf("lead history = %d %+v", code, history)
	}
	// A member without the lead's role cannot freeze a plan, and says why.
	var refused api.ErrorResponse
	builder := f.planRequest(t, "builder-plan")
	builder.AgentID, builder.RunID, builder.ContextDigest, builder.ExpectedGeneration = f.builder.ID, f.builder.RunID, f.digest(t, f.builder), 2
	if code := f.c.do("POST", f.path, builder, &refused); code != 409 || !strings.Contains(refused.Error, "only the item lead or the database handler may freeze a verification plan") {
		t.Fatalf("builder plan = %d %+v", code, refused)
	}
}

func TestVerificationHTTPChangedCallerRefused(t *testing.T) {
	f := newOperatorHTTP(t)
	stored := f.planRequest(t, "stored-plan")
	if code := f.c.do("POST", f.path, stored, nil); code != 201 {
		t.Fatalf("plan = %d", code)
	}
	records, messages := f.counts(t)
	otherAgent, otherRun := stored, stored
	otherAgent.AgentID, otherAgent.RunID, otherAgent.ContextDigest = f.handler.ID, f.handler.RunID, ""
	otherRun.RunID = api.NewID("run")
	for name, req := range map[string]api.VerificationRequest{"agentId": otherAgent, "runId": otherRun} {
		var refused api.ErrorResponse
		if code := f.c.do("POST", f.path, req, &refused); code != 409 || !strings.Contains(refused.Error, "retry payload changed") {
			t.Fatalf("changed %s = %d %+v", name, code, refused)
		}
		if r, m := f.counts(t); r != records || m != messages {
			t.Fatalf("changed %s wrote: records %d -> %d, messages %d -> %d", name, records, r, messages, m)
		}
	}
}

func TestVerificationHTTPRetiredRunExactReplay(t *testing.T) {
	f := newOperatorHTTP(t)
	stored := f.planRequest(t, "stored-plan")
	var first, replay api.VerificationRecord
	if code := f.c.do("POST", f.path, stored, &first); code != 201 {
		t.Fatalf("plan = %d", code)
	}
	retired := api.AgentRetired
	if _, err := f.c.st.UpdateAgent(context.Background(), f.lead.ID, api.UpdateAgentRequest{Status: &retired}, f.c.who); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stored)
	if code := f.c.do("POST", f.path, string(raw), &replay); code != 201 || replay.Generation != first.Generation || replay.Digest != first.Digest || replay.CreatedAt != first.CreatedAt || replay.AuthorRole != "lead" {
		t.Fatalf("exact replay after retirement = %d %+v, want %+v", code, replay, first)
	}
	fresh := f.planRequest(t, "new-plan-after-retirement")
	fresh.ExpectedGeneration = 1
	var refused api.ErrorResponse
	if code := f.c.do("POST", f.path, fresh, &refused); code != 409 || !strings.Contains(refused.Error, "agent run changed; refresh identity") {
		t.Fatalf("new request from the retired run = %d %+v", code, refused)
	}
	if records, _ := f.counts(t); records != 1 {
		t.Fatalf("records = %d, want 1", records)
	}
}
