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
