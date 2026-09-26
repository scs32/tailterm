// Package testverification supplies synthetic public-API evidence to unrelated
// lifecycle tests. It has no production callers and never connects itself to a hub.
package testverification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

const Commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func digest(v any) string {
	b, _ := json.Marshal(v)
	var x any
	_ = json.Unmarshal(b, &x)
	b, _ = json.Marshal(x)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func Prepare(c *api.Client, task string, item api.WorkItem, binding ...string) error {
	return prepare(c, task, item, false, binding...)
}

// PreparePending stops before receipt/import and acceptance for negative gate tests.
func PreparePending(c *api.Client, task string, item api.WorkItem) error {
	return prepare(c, task, item, true)
}
func prepare(c *api.Client, task string, item api.WorkItem, pending bool, binding ...string) error {
	commit, repository, base := Commit, "fixture", Commit
	if len(binding) == 3 {
		commit, repository, base = binding[0], binding[1], binding[2]
	}
	ctx := context.Background()
	key := api.NewID("req")
	agents, err := c.ListAgents(ctx, task)
	if err != nil {
		return err
	}
	var h, v api.Agent
	for _, a := range agents {
		if a.Role == api.AgentRoleDatabaseHandler {
			h = a
		}
		if a.WorkItem != nil && a.WorkItem.ItemID == item.ID && a.Status != api.AgentClosed && a.Status != api.AgentExited && a.Role == "" {
			v = a
		}
	}
	if h.ID == "" {
		h, err = c.AddAgent(ctx, task, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: key + "-handler", Host: "fixture", Session: key + "-handler", Role: api.AgentRoleDatabaseHandler})
		if err != nil {
			return err
		}
	}
	add := func(name string) (api.Agent, error) {
		return c.AddAgent(ctx, task, api.AddAgentRequest{Name: key + name, Host: "fixture", Session: key + name, Runtime: "codex"})
	}
	builder, err := add("-builder")
	if err != nil {
		return err
	}
	reviewer, err := add("-reviewer")
	if err != nil {
		return err
	}
	links := []api.MessageWorkItem{{ItemTaskID: task, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
	assign, err := c.PostMessage(ctx, task, api.PostMessageRequest{RequestID: key + "-assign", To: builder.ID, WorkItems: links, Envelope: &api.Envelope{Kind: "assign", Subject: "Assign synthetic fixture verification", Body: api.EnvelopeBody{Objective: "Fixture only", Owns: []string{"fixture"}, Acceptance: map[string]string{"a1": "synthetic fixture passes"}}}})
	if err != nil {
		return err
	}
	_, err = c.ConfirmWorkOrderScope(ctx, task, item.ID, api.ConfirmWorkOrderScopeRequest{RequestID: key + "-scope", AgentID: h.ID, RunID: h.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: assign.Seq, Complete: true})
	if err != nil {
		return err
	}
	if _, err = c.ObligationAction(ctx, task, assign.Seq, "ack", api.ObligationActionRequest{AgentID: builder.ID, RunID: builder.RunID}); err != nil {
		return err
	}
	_, err = c.PostMessage(ctx, task, api.PostMessageRequest{RequestID: key + "-built", AgentID: builder.ID, RunID: builder.RunID, ReplyTo: assign.Seq, WorkItems: links, Envelope: &api.Envelope{Kind: "result", Subject: "Synthetic fixture candidate prepared", Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}}, Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "synthetic fixture setup"}}}})
	if err != nil {
		return err
	}
	if v.ID == "" {
		exact, e := c.GetWorkItemRevision(ctx, task, item.ID, item.Revision)
		if e != nil {
			return e
		}
		revs, e := c.ListWorkItemRevisions(ctx, task, item.ID, 0, 64)
		if e != nil {
			return e
		}
		gaps, e := c.ListWorkItemHistoryGaps(ctx, task, item.ID, 0, 64)
		if e != nil {
			return e
		}
		messages, e := c.ListWorkItemMessages(ctx, task, item.ID, 0, 0, 64)
		if e != nil {
			return e
		}
		if revs.NextAfter != 0 || gaps.NextAfter != 0 || messages.NextAfter != 0 {
			return fmt.Errorf("fixture context needs complete history")
		}
		bundle, _ := json.Marshal(map[string]any{"version": 1, "itemTaskId": task, "itemId": item.ID, "itemRevision": item.Revision, "workOrderMessage": api.MessageReference{TaskID: task, Seq: assign.Seq}, "history": map[string]any{"revision": exact, "revisions": revs.Revisions, "gaps": gaps.Gaps, "messages": messages.Links, "coverage": messages.Coverage}})
		v, err = c.AddAgent(ctx, task, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: key + "-verifier", Host: "fixture", Session: key + "-verifier", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: api.MessageReference{TaskID: task, Seq: assign.Seq}, ContextBundle: bundle, TeamRole: api.TeamRoleMember}})
		if err != nil {
			return err
		}
	}
	review, err := c.PostMessage(ctx, task, api.PostMessageRequest{RequestID: key + "-review", To: reviewer.ID, WorkItems: links, Envelope: &api.Envelope{Kind: "review", Subject: "Review synthetic fixture candidate", Body: api.EnvelopeBody{Candidate: commit, Scope: "Fixture only", Acceptance: map[string]string{"a1": "synthetic fixture passes"}}}})
	if err != nil {
		return err
	}
	if _, err = c.ObligationAction(ctx, task, review.Seq, "ack", api.ObligationActionRequest{AgentID: reviewer.ID, RunID: reviewer.RunID}); err != nil {
		return err
	}
	_, err = c.PostMessage(ctx, task, api.PostMessageRequest{RequestID: key + "-review-result", AgentID: reviewer.ID, RunID: reviewer.RunID, ReplyTo: review.Seq, WorkItems: links, Envelope: &api.Envelope{Kind: "result", Subject: "Synthetic fixture review passed", Review: &api.ReviewMetadata{Mode: "general", Candidate: commit}, Body: api.EnvelopeBody{Outcome: "done", Status: map[string]string{"a1": "pass"}}, Evidence: map[string]api.Evidence{"e1": {Type: "command", Value: "synthetic fixture check"}}}})
	if err != nil {
		return err
	}
	checks := []api.VerificationCheck{{ID: "fixture", Argv: []string{"fixture"}, Cwd: ".", Environment: map[string]string{}}}
	p := api.VerificationPlan{ItemID: item.ID, ItemTaskID: task, AssignmentOwnershipDigest: digest([]string{"fixture"}), Version: 1, OperationKey: key, Repository: repository, BaseCommit: base, Commit: commit, ItemRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: assign.Seq, AssignmentSeq: assign.Seq, BuilderAgentID: builder.ID, BuilderRunID: builder.RunID, VerifierAgentID: v.ID, VerifierRunID: v.RunID, MatrixDigest: strings.Repeat("a", 64), ChecksDigest: digest(checks), Owned: []string{"fixture"}, Changed: []string{}, Checks: checks}
	approval, err := c.PostMessage(ctx, task, api.PostMessageRequest{Text: "verification-matrix-approval:" + p.MatrixDigest})
	if err != nil {
		return err
	}
	p.ApprovedMatrixDigest = p.MatrixDigest
	p.MatrixApprovalMessageSeq = approval.Seq
	if _, err = c.SaveVerification(ctx, task, item.ID, api.VerificationRequest{RequestID: key + "-plan", AgentID: h.ID, RunID: h.RunID, Plan: &p}); err != nil {
		return err
	}
	if pending {
		return nil
	}
	r := api.VerificationReceipt{Worktree: "/tmp/fixture", Version: 1, OperationKey: key, PlanDigest: digest(p), Repository: p.Repository, BaseCommit: base, Commit: commit, MatrixDigest: p.MatrixDigest, ChecksDigest: p.ChecksDigest, VerifierAgentID: v.ID, VerifierRunID: v.RunID, Detached: true, CleanBefore: true, CleanAfter: true, Environment: map[string]string{"HOME": "/tmp/fixture"}, Prerequisites: []api.VerificationPrerequisite{}, AIV: api.AIVBinding{State: "unsubmitted"}, Checks: []api.VerificationResult{{VerificationCheck: checks[0], StartedAt: "2026-09-26T00:00:00Z", EndedAt: "2026-09-26T00:00:01Z", DurationMs: 1000, LogURI: "/tmp/fixture.log", LogDigest: strings.Repeat("b", 64)}}}
	if _, err = c.SaveVerification(ctx, task, item.ID, api.VerificationRequest{RequestID: key + "-receipt", AgentID: h.ID, RunID: h.RunID, ExpectedGeneration: 1, Receipt: &r}); err != nil {
		return err
	}
	_, err = c.PostMessage(ctx, task, api.PostMessageRequest{RequestID: key + "-accept", WorkItems: links, Envelope: &api.Envelope{Kind: "notice", Subject: "Accept synthetic verified fixture", Body: api.EnvelopeBody{Text: "Fixture only"}, Review: &api.ReviewMetadata{Mode: "disposition", Disposition: "accept", Candidate: commit}}})
	return err
}
