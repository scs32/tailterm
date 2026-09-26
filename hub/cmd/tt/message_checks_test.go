package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestMessageChecksSummarySeparatesWithdrawnFromUnacknowledged(t *testing.T) {
	now := time.Now().UTC()
	rows := []api.Obligation{{MessageSeq: 10, Needs: api.ObligationNeedsOutcome, State: api.ObligationClosed, Outcome: api.OutcomeWithdrawn, CreatedAt: now.Add(-time.Hour), AckDueAt: now.Add(-50 * time.Minute)}}
	out := summarizeAcks(rows, nil, now.Add(-2*time.Hour), now)
	if !strings.Contains(out, "Withdrawn: 1") || strings.Contains(out, "Unacknowledged past") {
		t.Fatal(out)
	}
	acked := now.Add(-55 * time.Minute)
	rows[0].AckedAt = &acked
	out = summarizeAcks(rows, nil, now.Add(-2*time.Hour), now)
	if !strings.Contains(out, "1 acknowledged") || !strings.Contains(out, "Withdrawn: 1") {
		t.Fatalf("withdrawal erased acknowledgement history: %s", out)
	}
}

// This test runs real native HTTP/CLI transitions on an isolated hub. The optional
// artifact is consumed by the two-engine browser fixture; no live data is read.
func TestReviewConvergenceNativeCLISummaryAndProjection(t *testing.T) {
	e, c, task, reviewer := cliWorkItemFixture(t)
	ctx := context.Background()
	agents, err := c.ListAgents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var handler api.Agent
	for _, a := range agents {
		if a.ID == e.agent {
			handler = a
		}
	}
	post := func(env api.Envelope, links []api.MessageWorkItem, to string, reply int64, from api.Agent) api.Message {
		t.Helper()
		m, err := c.PostMessage(ctx, task.ID, api.PostMessageRequest{Envelope: &env, WorkItems: links, To: to, ReplyTo: reply, AgentID: from.ID, RunID: from.RunID, RequestID: api.NewID("req")})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	var primary api.WorkItem
	for i := 0; i < 2; i++ {
		item, err := c.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "Synthetic review summary fixture", RequestID: api.NewID("req")})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			primary = item
		}
		links := []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: 1, Relationship: "primary"}}
		order := post(api.Envelope{Kind: "notice", Subject: "Owner bounded fixture work order", Body: api.EnvelopeBody{Text: "Fixture only"}}, links, "", 0, api.Agent{})
		if _, err = c.ConfirmWorkOrderScope(ctx, task.ID, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: api.NewID("req"), AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: 1, ScopeRevision: 1, OrderMessageSeq: order.Seq, Complete: true}); err != nil {
			t.Fatal(err)
		}
		if _, err = c.TeamQueueAction(ctx, task.ID, api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "add", ItemID: item.ID, OrderMessageSeq: order.Seq, Host: "fixture", Cwd: "/fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	links := []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: primary.ID, ItemRevision: 1, Relationship: "primary"}}
	criteria := map[string]string{"a1": "Fixture passes"}
	post(api.Envelope{Kind: "assign", Subject: "Implement frozen native fixture", Body: api.EnvelopeBody{Objective: "Fixture", Owns: []string{"fixture"}, Acceptance: criteria}}, links, "", 0, api.Agent{})
	candidate := strings.Repeat("a", 40)
	for round := 0; round < 2; round++ {
		request := post(api.Envelope{Kind: "review", Subject: "Review exact native fixture candidate", Body: api.EnvelopeBody{Candidate: candidate, Scope: "Fixture", Acceptance: criteria}}, links, reviewer.ID, 0, api.Agent{})
		metadata := api.ReviewMetadata{Mode: "general", Candidate: candidate}
		if round == 0 {
			metadata.Findings = []api.ReviewFinding{{ID: "f1", Title: "<script>fixture escape</script>", File: "fixture.go", Line: 2}}
		}
		post(api.Envelope{Kind: "result", Subject: "Native fixture review result recorded", Review: &metadata, Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}}, Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "fixture checks -> pass"}}}, links, "", request.Seq, reviewer)
	}
	summary, err := captureCLIOutput(t, func() error { return cmdMessageChecks(e, []string{"--summary", "--since", "0"}) })
	if err != nil {
		t.Fatal(err)
	}
	native, err := c.ListReviewConvergence(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := c.ListTeamQueue(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Entries) != 2 || queue.Entries[0].Reviews == nil || len(queue.Entries[0].Reviews.Rounds) != 2 || len(queue.Entries[0].Reviews.FollowUps) != 1 {
		t.Fatal("native projection", queue)
	}
	if !strings.Contains(summary, primary.ID+" reviews: 2/2; follow-ups: 1") || !strings.Contains(summary, "reviews: unknown; follow-ups: unknown") {
		t.Fatal(summary)
	}
	if queue.Entries[1].Reviews.History != "unknown" {
		t.Fatal("legacy inferred zero")
	}
	projected, _ := json.Marshal(queue.Entries[0].Reviews)
	recorded, _ := json.Marshal(native[0])
	if string(projected) != string(recorded) {
		t.Fatal("native summary and Delivery disagree")
	}
	reloaded, err := c.ListTeamQueue(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	bytes, _ := json.Marshal(reloaded)
	before, _ := json.Marshal(queue)
	if string(before) != string(bytes) {
		t.Fatal("reload changed history")
	}
	if path := os.Getenv("REVIEW_CONVERGENCE_FIXTURE_OUTPUT"); path != "" {
		data, _ := json.Marshal(map[string]any{"queue": queue, "summary": summary})
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
