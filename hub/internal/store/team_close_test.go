package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func teamCloseFixture(t *testing.T) (*Store, api.Task, api.WorkItem, api.Agent, api.Agent, api.Agent, api.TeamCloseRequest) {
	t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Team close", Orchestrator: "lead"}, by)
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Close team", Priority: "normal", RequestID: "item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	order := contextLinkedMessage(t, s, task, item, "bounded order", "team-order", nil)
	ref := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	add := func(name, role string) api.Agent {
		bundle := syntheticPreparedContext(t, item, ref, syntheticHistory(item, order))
		a, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: name, Role: role, Host: "fixture", Session: name, Runtime: "codex",
			WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: ref, ContextBundle: bundle}}, by)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	lead := add("lead", "")
	worker := add("worker", "")
	other, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "unbound", Host: "fixture", Session: "unbound"}, by)
	if err != nil {
		t.Fatal(err)
	}
	// A handler role cannot be item-bound at admission, and must be untouched.
	handler, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "database", Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "fixture", Session: "database"}, by)
	if err != nil {
		t.Fatal(err)
	}
	_ = handler
	req := api.TeamCloseRequest{RequestID: "close-fixture", ActorAgentID: lead.ID, ActorRunID: lead.RunID,
		LeadAgentID: lead.ID, LeadRunID: lead.RunID, LeadRevision: task.LeadRevision, ItemID: item.ID, ItemRevision: item.Revision,
		Members: []api.TeamCloseMember{{AgentID: lead.ID, RunID: lead.RunID, Host: lead.Host, Status: lead.Status}, {AgentID: worker.ID, RunID: worker.RunID, Host: worker.Host, Status: worker.Status}}}
	slices.SortFunc(req.Members, func(a, b api.TeamCloseMember) int { return strings.Compare(a.AgentID, b.AgentID) })
	return s, task, item, lead, worker, other, req
}

func teamCloseTerminal(t *testing.T, s *Store, task api.Task, item api.WorkItem, status string) api.WorkItem {
	t.Helper()
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	if status == "done" {
		seedPassingVerification(t, s, item, candidateA)
		report, _, err := s.PutNarrativeReport(ctx, task.ID, item.ID, completeReportRequest(item, "close-report", 5), by)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = s.CreateWorkItemUpdate(ctx, task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, Status: &status, RequestID: "close-done", CompletionReport: &api.NarrativeReportPin{ReportID: report.ReportID, Version: report.Version, Digest: report.Digest, ScopeRevision: report.ScopeRevision}}, by)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := s.UpdateWorkItem(ctx, task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Status: &status}, by); err != nil {
			t.Fatal(err)
		}
	}
	updated, err := s.GetWorkItem(ctx, task.ID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestParallelItemClosePreservesOtherLead(t *testing.T) {
	s, task, item, lead, _, _, req := teamCloseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	other, err := s.CreateWorkItem(ctx, task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Other team", Priority: "normal", RequestID: "other-item"}, by)
	if err != nil {
		t.Fatal(err)
	}
	order := contextLinkedMessage(t, s, task, other, "other bounded order", "other-order", nil)
	ref := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	bundle := syntheticPreparedContext(t, other, ref, syntheticHistory(other, order))
	leadB, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "lead-b", Host: "fixture", Session: "lead-b", Runtime: "codex", WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: other.ID, ItemRevision: other.Revision, WorkOrderMessage: ref, ContextBundle: bundle}}, by)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ item, agent, run string }{{item.ID, lead.ID, lead.RunID}, {other.ID, leadB.ID, leadB.RunID}} {
		if _, err := s.db.Exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,state) VALUES(?,?,?,?,'running')`, task.ID, entry.item, entry.agent, entry.run); err != nil {
			t.Fatal(err)
		}
	}
	foreignLink := []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: other.ID, ItemRevision: other.Revision, Relationship: "primary"}}
	if _, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{AgentID: lead.ID, RunID: lead.RunID, To: leadB.ID, RequestID: "A-assigns-B", WorkItems: foreignLink, WorkOrderMessage: &ref, Envelope: &api.Envelope{Kind: api.EnvelopeKindAssign, To: leadB.Name, Subject: "Build the other item", Body: api.EnvelopeBody{Objective: "Build B", Owns: []string{"src/b"}, Acceptance: map[string]string{"a1": "passes"}}}}, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("A assigned B: %v", err)
	}
	foreignDecision := decisionRequest(lead, "A-decides-B")
	foreignDecision.WorkItems = foreignLink
	if _, err := s.CreateDecision(ctx, task.ID, foreignDecision, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("A requested B decision: %v", err)
	}
	teamCloseTerminal(t, s, task, item, "dismissed")
	req.LeadRevision = 1
	if _, err := s.CloseItemTeam(ctx, task.ID, req, by); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetAgent(ctx, leadB.ID)
	if err != nil || after.Status == api.AgentClosed || !after.ItemLead {
		t.Fatalf("B changed after closing A: %+v %v", after, err)
	}
	if _, err := s.db.Exec(`UPDATE work_items SET status='dismissed' WHERE id=?`, other.ID); err != nil {
		t.Fatal(err)
	}
	foreign := api.TeamCloseRequest{RequestID: "A-closes-B", ActorAgentID: lead.ID, ActorRunID: lead.RunID, LeadAgentID: lead.ID, LeadRunID: lead.RunID, LeadRevision: 1, ItemID: other.ID, ItemRevision: other.Revision, Members: []api.TeamCloseMember{{AgentID: leadB.ID, RunID: leadB.RunID, Host: leadB.Host, Status: leadB.Status}}}
	if _, err := s.CloseItemTeam(ctx, task.ID, foreign, by); err == nil {
		t.Fatal("A closed B")
	}
}

func TestCloseItemTeamTerminalAndReplay(t *testing.T) {
	for _, status := range []string{"done", "dismissed"} {
		t.Run(status, func(t *testing.T) {
			s, task, item, lead, worker, other, req := teamCloseFixture(t)
			ctx := context.Background()
			by := api.Caller{Node: "fixture", User: "owner"}
			// Simulate a historical item-bound handler row: the role still wins.
			agentsBefore, _ := s.ListAgents(ctx, task.ID)
			for _, a := range agentsBefore {
				if a.Role != api.AgentRoleDatabaseHandler {
					continue
				}
				_, err := s.db.ExecContext(ctx, `INSERT INTO agent_work_item_bindings(agent_id,run_id,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,replaces_agent_id,team_role,context_digest,context_json,created_at) SELECT ?,?,item_task_id,item_id,item_revision,work_order_task_id,work_order_message_seq,context_through_message_seq,replaces_agent_id,team_role,context_digest,context_json,created_at FROM agent_work_item_bindings WHERE agent_id=?`, a.ID, a.RunID, worker.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.CloseItemTeam(ctx, task.ID, req, by); !errors.Is(err, api.ErrConflict) {
				t.Fatalf("nonterminal = %v", err)
			}
			item = teamCloseTerminal(t, s, task, item, status)
			if item.Revision <= req.ItemRevision {
				t.Fatal("fixture did not advance terminal revision")
			}
			result, err := s.CloseItemTeam(ctx, task.ID, req, by)
			if err != nil || result.LeadAgentID != lead.ID || len(result.Members) != 2 || result.Members[0].AgentID != worker.ID || result.Members[1].AgentID != lead.ID {
				t.Fatalf("close result %+v %v", result, err)
			}
			if replay, err := s.CloseItemTeam(ctx, task.ID, req, by); err != nil || len(replay.Members) != 2 {
				t.Fatalf("replay %+v %v", replay, err)
			}
			current, _ := s.GetTask(ctx, task.ID)
			if current.Status != api.TaskOpen || current.Orchestrator != "" || current.LeadRevision != task.LeadRevision+1 {
				t.Fatalf("task %+v", current)
			}
			for _, a := range []api.Agent{lead, worker} {
				closed, _ := s.GetAgent(ctx, a.ID)
				if closed.Status != api.AgentClosed || closed.CleanupDone || closed.RunID != a.RunID {
					t.Fatalf("closure %+v", closed)
				}
			}
			unbound, _ := s.GetAgent(ctx, other.ID)
			if unbound.Status != other.Status {
				t.Fatalf("unbound changed %+v", unbound)
			}
			agents, _ := s.ListAgents(ctx, task.ID)
			for _, a := range agents {
				if a.Role == api.AgentRoleDatabaseHandler && a.Status == api.AgentClosed {
					t.Fatal("handler closed")
				}
			}
			changed := req
			changed.Members = append([]api.TeamCloseMember(nil), req.Members...)
			changed.Members[0].RunID = api.NewID("run")
			if _, err := s.CloseItemTeam(ctx, task.ID, changed, by); !errors.Is(err, api.ErrConflict) {
				t.Fatalf("changed replay = %v", err)
			}
		})
	}
}

func TestCloseItemTeamOwnerCanCloseExitedBoundLead(t *testing.T) {
	s, task, item, lead, worker, _, req := teamCloseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	exited := api.AgentExited
	if _, err := s.UpdateAgent(ctx, lead.ID, api.UpdateAgentRequest{Status: &exited}, by); err != nil {
		t.Fatal(err)
	}
	for i := range req.Members {
		if req.Members[i].AgentID == lead.ID {
			req.Members[i].Status = exited
		}
	}
	if _, err := s.CloseItemTeam(ctx, task.ID, req, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("exited actor accepted: %v", err)
	}
	req.ActorAgentID, req.ActorRunID = "", ""
	if _, err := s.CloseItemTeam(ctx, task.ID, req, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("nonterminal owner close accepted: %v", err)
	}
	teamCloseTerminal(t, s, task, item, "dismissed")
	stale := req
	stale.RequestID = "stale-exited"
	stale.Members = append([]api.TeamCloseMember(nil), req.Members...)
	stale.Members[0].RunID = api.NewID("run")
	if _, err := s.CloseItemTeam(ctx, task.ID, stale, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("stale owner snapshot accepted: %v", err)
	}
	current, _ := s.GetTask(ctx, task.ID)
	if current.Orchestrator != task.Orchestrator {
		t.Fatal("refusal cleared orchestrator")
	}
	result, err := s.CloseItemTeam(ctx, task.ID, req, by)
	if err != nil || result.LeadAgentID != lead.ID || len(result.Members) != 2 || result.Members[0].AgentID != worker.ID || result.Members[1].AgentID != lead.ID {
		t.Fatalf("owner close %+v: %v", result, err)
	}
	closed, _ := s.GetAgent(ctx, lead.ID)
	if closed.Status != api.AgentClosed {
		t.Fatalf("exited lead remained %s", closed.Status)
	}
}

func TestCloseItemTeamIgnoresWithdrawnRequest(t *testing.T) {
	s, task, item, lead, worker, _, req := teamCloseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	message, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: worker.ID, AgentID: lead.ID, RunID: lead.RunID,
		Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: worker.Name, Subject: "Check fixture", Body: api.EnvelopeBody{Ask: "Check the fixture."}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	var obligationID string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM obligations WHERE message_seq=?`, message.Seq).Scan(&obligationID); err != nil {
		t.Fatal(err)
	}
	teamCloseTerminal(t, s, task, item, "dismissed")
	if _, err := s.CloseItemTeam(ctx, task.ID, req, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("open request did not block close: %v", err)
	}
	withdrawn, err := s.WithdrawObligation(ctx, task.ID, obligationID, api.ObligationWithdrawRequest{
		AgentID: lead.ID, RunID: lead.RunID, Reason: "No longer needed", RequestID: "withdraw-close-fixture",
	})
	if err != nil || withdrawn.State != api.ObligationClosed || withdrawn.Outcome != api.OutcomeWithdrawn {
		t.Fatalf("withdrawal %+v: %v", withdrawn, err)
	}
	if _, err := s.CloseItemTeam(ctx, task.ID, req, by); err != nil {
		t.Fatalf("withdrawn request blocked team close: %v", err)
	}
}

func TestCloseItemTeamClosesHeldDeliveryNoticesAtomically(t *testing.T) {
	for _, sender := range []string{"owner cancellation", "agent notice"} {
		t.Run(sender, func(t *testing.T) {
			s, task, item, lead, worker, other, req := teamCloseFixture(t)
			ctx := context.Background()
			by := api.Caller{Node: "fixture", User: "owner"}
			teamCloseTerminal(t, s, task, item, "dismissed")
			var noticeSeq int64
			if sender == "owner cancellation" {
				m, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: worker.ID, AgentID: lead.ID, RunID: lead.RunID,
					Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: worker.Name, Subject: "Check fixture", Body: api.EnvelopeBody{Ask: "Check the fixture."}}}, by)
				if err != nil {
					t.Fatal(err)
				}
				var obligationID string
				if err := s.db.QueryRowContext(ctx, `SELECT id FROM obligations WHERE message_seq=?`, m.Seq).Scan(&obligationID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.CancelObligation(ctx, task.ID, obligationID, api.ObligationCancelRequest{RequestID: "cancel-fixture", Reason: "fixture complete"}, by); err != nil {
					t.Fatal(err)
				}
				if err := s.db.QueryRowContext(ctx, `SELECT max(message_seq) FROM obligations WHERE agent_id=? AND needs=? AND state<>?`, worker.ID, api.ObligationNeedsDelivery, api.ObligationClosed).Scan(&noticeSeq); err != nil {
					t.Fatal(err)
				}
			} else {
				m, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: worker.ID, AgentID: lead.ID, RunID: lead.RunID,
					Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, To: worker.Name, Subject: "Fixture notice", Body: api.EnvelopeBody{Text: "Fixture complete."}}}, by)
				if err != nil {
					t.Fatal(err)
				}
				noticeSeq = m.Seq
			}
			var state, needs string
			if err := s.db.QueryRowContext(ctx, `SELECT state,needs FROM obligations WHERE message_seq=?`, noticeSeq).Scan(&state, &needs); err != nil || state == api.ObligationClosed || needs != api.ObligationNeedsDelivery {
				t.Fatalf("notice setup %s %s: %v", state, needs, err)
			}
			outsideNotice, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: other.ID, AgentID: worker.ID, RunID: worker.RunID,
				Envelope: &api.Envelope{Kind: api.EnvelopeKindNotice, To: other.Name, Subject: "External fixture notice", Body: api.EnvelopeBody{Text: "External fixture remains."}}}, by)
			if err != nil {
				t.Fatal(err)
			}
			// A delivery notice cannot be consumed by a refusal.
			request, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: worker.ID, AgentID: lead.ID, RunID: lead.RunID,
				Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: worker.Name, Subject: "Substantive fixture", Body: api.EnvelopeBody{Ask: "Review the fixture."}}}, by)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CloseItemTeam(ctx, task.ID, req, by); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "#") {
				t.Fatalf("substantive request did not block: %v", err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT state FROM obligations WHERE message_seq=?`, noticeSeq).Scan(&state); err != nil || state == api.ObligationClosed {
				t.Fatalf("refusal mutated notice %s: %v", state, err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE obligations SET state='closed',outcome='cancelled' WHERE message_seq=?`, request.Seq); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CloseItemTeam(ctx, task.ID, req, by); err != nil {
				t.Fatalf("delivery notice blocked close: %v", err)
			}
			var outcome string
			if err := s.db.QueryRowContext(ctx, `SELECT state,outcome FROM obligations WHERE message_seq=?`, noticeSeq).Scan(&state, &outcome); err != nil || state != api.ObligationClosed || outcome != api.OutcomeRecipientGone {
				t.Fatalf("notice final %s/%s: %v", state, outcome, err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT state FROM obligations WHERE message_seq=?`, outsideNotice.Seq).Scan(&state); err != nil || state == api.ObligationClosed {
				t.Fatalf("outsider delivery obligation changed: %s %v", state, err)
			}
			var dangling int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM obligations WHERE agent_id IN (?,?) AND needs=? AND state<>?`, lead.ID, worker.ID, api.ObligationNeedsDelivery, api.ObligationClosed).Scan(&dangling); err != nil || dangling != 0 {
				t.Fatalf("dangling delivery obligations %d: %v", dangling, err)
			}
			unchanged, _ := s.GetAgent(ctx, other.ID)
			if unchanged.Status != other.Status {
				t.Fatal("outsider changed")
			}
		})
	}
}

func TestCloseItemTeamRefusesOpenHeldAndSentObligations(t *testing.T) {
	s, task, item, lead, worker, _, req := teamCloseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	seedPassingVerification(t, s, item, candidateA)
	status := "done"
	teamCloseTerminal(t, s, task, item, status)
	for _, state := range []string{api.ObligationQueued, api.ObligationDelivered, api.ObligationAcknowledged, api.ObligationWorking, api.ObligationBlocked} {
		m, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: worker.ID, AgentID: lead.ID, RunID: lead.RunID, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: worker.Name, Subject: "Check the synthetic fixture", Body: api.EnvelopeBody{Ask: "Please check the fixture."}}}, by)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE obligations SET state=? WHERE message_seq=?`, state, m.Seq); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CloseItemTeam(ctx, task.ID, req, by); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "#") || !strings.Contains(err.Error(), state) {
			t.Fatalf("%s refusal %v", state, err)
		}
		outcome := api.OutcomeCancelled
		if state == api.ObligationWorking {
			outcome = api.OutcomeResult
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE obligations SET state='closed',outcome=? WHERE message_seq=?`, outcome, m.Seq); err != nil {
			t.Fatal(err)
		}
	}
	// Sent by a member to an unbound agent is also a gate.
	agents, _ := s.ListAgents(ctx, task.ID)
	var unbound api.Agent
	for _, a := range agents {
		if a.Name == "unbound" {
			unbound = a
		}
	}
	m, err := s.PostMessage(ctx, task.ID, api.PostMessageRequest{To: unbound.ID, AgentID: worker.ID, RunID: worker.RunID, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: unbound.Name, Subject: "Check the external fixture", Body: api.EnvelopeBody{Ask: "Please check the fixture."}}}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseItemTeam(ctx, task.ID, req, by); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "#") {
		t.Fatalf("sent gate %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE obligations SET state='closed',outcome='cancelled' WHERE message_seq=?`, m.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseItemTeam(ctx, task.ID, req, by); err != nil {
		t.Fatalf("cancelled work should permit close: %v", err)
	}
}

func TestCloseItemTeamRefusalsLeaveProjectAndAgentsUntouched(t *testing.T) {
	s, task, item, lead, worker, _, req := teamCloseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	teamCloseTerminal(t, s, task, item, "dismissed")
	checks := []struct {
		name   string
		change func(*api.TeamCloseRequest)
	}{
		{"stale lead run", func(r *api.TeamCloseRequest) { r.LeadRunID = api.NewID("run") }},
		{"wrong actor", func(r *api.TeamCloseRequest) { r.ActorAgentID, r.ActorRunID = worker.ID, worker.RunID }},
		{"missing binding", func(r *api.TeamCloseRequest) {
			r.LeadAgentID = worker.ID
			r.LeadRunID = worker.RunID
			r.ActorAgentID = worker.ID
			r.ActorRunID = worker.RunID
		}},
		{"stale member", func(r *api.TeamCloseRequest) {
			r.Members = append([]api.TeamCloseMember(nil), r.Members...)
			r.Members[0].RunID = api.NewID("run")
		}},
		{"stale lead revision", func(r *api.TeamCloseRequest) { r.LeadRevision++ }},
	}
	for _, check := range checks {
		candidate := req
		candidate.RequestID = "refusal-" + strings.ReplaceAll(check.name, " ", "-")
		check.change(&candidate)
		if _, err := s.CloseItemTeam(ctx, task.ID, candidate, by); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("%s: %v", check.name, err)
		}
		current, _ := s.GetTask(ctx, task.ID)
		currentLead, _ := s.GetAgent(ctx, lead.ID)
		currentWorker, _ := s.GetAgent(ctx, worker.ID)
		if current.Orchestrator != task.Orchestrator || current.LeadRevision != task.LeadRevision || currentLead.Status != lead.Status || currentWorker.Status != worker.Status {
			t.Fatalf("%s mutated project or agents", check.name)
		}
	}
	newLead := "worker"
	if _, err := s.UpdateTask(ctx, task.ID, api.UpdateTaskRequest{Orchestrator: &newLead}, by); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseItemTeam(ctx, task.ID, req, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed lead: %v", err)
	}
	current, _ := s.GetTask(ctx, task.ID)
	if current.Orchestrator != newLead {
		t.Fatalf("changed lead overwritten: %+v", current)
	}
}

func TestCloseItemTeamRejectsStaleReplacementSnapshot(t *testing.T) {
	s, task, item, lead, worker, _, old := teamCloseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	exited := api.AgentExited
	if _, err := s.UpdateAgent(ctx, worker.ID, api.UpdateAgentRequest{Status: &exited}, by); err != nil {
		t.Fatal(err)
	}
	order := contextLinkedMessage(t, s, task, item, "replacement order", "replacement-order", nil)
	ref := api.MessageReference{TaskID: task.ID, Seq: order.Seq}
	replacement, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker-replacement", Host: "fixture", Session: "worker-new", Runtime: "codex", ParentAgentID: lead.ID,
		WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, WorkOrderMessage: ref, ReplacesAgentID: worker.ID, ContextBundle: syntheticPreparedContext(t, item, ref, syntheticHistory(item, order))}}, by)
	if err != nil {
		t.Fatal(err)
	}
	teamCloseTerminal(t, s, task, item, "dismissed")
	if _, err := s.CloseItemTeam(ctx, task.ID, old, by); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("old worker snapshot accepted: %v", err)
	}
	current, _ := s.GetTask(ctx, task.ID)
	currentLead, _ := s.GetAgent(ctx, lead.ID)
	currentReplacement, _ := s.GetAgent(ctx, replacement.ID)
	if current.Orchestrator != "lead" || currentLead.Status == api.AgentClosed || currentReplacement.Status == api.AgentClosed {
		t.Fatal("stale retry mutated active team")
	}
}

// openItemState is what a team close must leave alone on an open item.
type openItemState struct {
	status   string
	revision int64
	history  int
}

func readOpenItemState(t *testing.T, s *Store, task, item string) openItemState {
	t.Helper()
	var out openItemState
	if err := s.db.QueryRow(`SELECT status,revision FROM work_items WHERE task_id=? AND id=?`, task, item).Scan(&out.status, &out.revision); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM work_item_revisions WHERE item_task_id=? AND item_id=?`, task, item).Scan(&out.history); err != nil {
		t.Fatal(err)
	}
	return out
}

// a1: a team whose item is open closes with either reason, as its exact lead
// or as the owner, and the item is not touched.
func TestCloseItemTeamOpenItemWithReason(t *testing.T) {
	for _, reason := range []string{api.TeamCloseReasonOwnerHold, api.TeamCloseReasonFindingsOnly} {
		for _, actor := range []string{"lead", "owner"} {
			t.Run(reason+" as "+actor, func(t *testing.T) {
				s, task, item, lead, worker, other, req := teamCloseFixture(t)
				ctx := context.Background()
				by := api.Caller{Node: "fixture", User: "owner"}
				if _, err := s.db.Exec(`INSERT INTO item_team_leads(task_id,item_id,agent_id,run_id,state) VALUES(?,?,?,?,'running')`, task.ID, item.ID, lead.ID, lead.RunID); err != nil {
					t.Fatal(err)
				}
				req.LeadRevision = 1
				req.Reason = reason
				if actor == "owner" {
					req.ActorAgentID, req.ActorRunID = "", ""
				}
				before := readOpenItemState(t, s, task.ID, item.ID)
				if before.status != "open" {
					t.Fatalf("fixture item is %s", before.status)
				}
				result, err := s.CloseItemTeam(ctx, task.ID, req, by)
				if err != nil || result.ItemID != item.ID || result.LeadAgentID != lead.ID || result.Reason != reason || len(result.Members) != 2 || result.Members[0].AgentID != worker.ID || result.Members[1].AgentID != lead.ID {
					t.Fatalf("close result %+v %v", result, err)
				}
				if after := readOpenItemState(t, s, task.ID, item.ID); after != before {
					t.Fatalf("item changed: before %+v after %+v", before, after)
				}
				for _, a := range []api.Agent{lead, worker} {
					closed, _ := s.GetAgent(ctx, a.ID)
					if closed.Status != api.AgentClosed || closed.RunID != a.RunID {
						t.Fatalf("member not closed: %+v", closed)
					}
				}
				var leadState string
				var leadRevision int64
				if err := s.db.QueryRow(`SELECT state,revision FROM item_team_leads WHERE task_id=? AND item_id=?`, task.ID, item.ID).Scan(&leadState, &leadRevision); err != nil || leadState != "closed" || leadRevision != 2 {
					t.Fatalf("lead row %s revision=%d %v", leadState, leadRevision, err)
				}
				current, _ := s.GetTask(ctx, task.ID)
				if current.Status != api.TaskOpen || current.Orchestrator != "" || current.LeadRevision != task.LeadRevision+1 {
					t.Fatalf("task %+v", current)
				}
				if unbound, _ := s.GetAgent(ctx, other.ID); unbound.Status != other.Status {
					t.Fatalf("unbound agent changed: %+v", unbound)
				}
			})
		}
	}
}

// a2: a reason relaxes only the terminal-item check. Every snapshot refusal
// is the one the terminal path gives for the same stale request.
func TestCloseItemTeamOpenItemKeepsSnapshotChecks(t *testing.T) {
	type fixture struct {
		s            *Store
		task         api.Task
		item         api.WorkItem
		lead, worker api.Agent
		req          api.TeamCloseRequest
	}
	checks := []struct {
		name string
		code string
		// prepare changes the store or the request so the close is stale.
		prepare func(t *testing.T, f *fixture)
	}{
		{"stale lead revision", "team-close-snapshot", func(t *testing.T, f *fixture) { f.req.LeadRevision++ }},
		{"wrong run", "team-close-snapshot", func(t *testing.T, f *fixture) {
			f.req.LeadRunID = "run_0123456789abcdef"
			f.req.ActorRunID = f.req.LeadRunID
		}},
		{"wrong actor", "", func(t *testing.T, f *fixture) { f.req.ActorAgentID, f.req.ActorRunID = f.worker.ID, f.worker.RunID }},
		{"changed members", "team-close-snapshot", func(t *testing.T, f *fixture) {
			f.req.Members = append([]api.TeamCloseMember(nil), f.req.Members...)
			f.req.Members[0].RunID = "run_fedcba9876543210"
		}},
		{"paused project", "team-close-snapshot", func(t *testing.T, f *fixture) {
			if _, err := f.s.db.Exec(`UPDATE tasks SET pause_state=? WHERE id=?`, api.ProjectPausePaused, f.task.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"open obligation", "team-close-obligations", func(t *testing.T, f *fixture) {
			if _, err := f.s.PostMessage(context.Background(), f.task.ID, api.PostMessageRequest{To: f.worker.ID, AgentID: f.lead.ID, RunID: f.lead.RunID, Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: f.worker.Name, Subject: "Check the synthetic fixture", Body: api.EnvelopeBody{Ask: "Please check the fixture."}}}, api.Caller{Node: "fixture", User: "owner"}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	// refusal runs one stale close and reports its code, and for a snapshot
	// refusal its text, which names no identity. An obligation refusal lists
	// message numbers and agents, which differ between two stores.
	refusal := func(t *testing.T, prepare func(*testing.T, *fixture), terminal bool) (string, string) {
		t.Helper()
		f := &fixture{}
		var item api.WorkItem
		f.s, f.task, item, f.lead, f.worker, _, f.req = teamCloseFixture(t)
		f.item = item
		ctx := context.Background()
		by := api.Caller{Node: "fixture", User: "owner"}
		if terminal {
			teamCloseTerminal(t, f.s, f.task, item, "dismissed")
		} else {
			f.req.Reason = api.TeamCloseReasonFindingsOnly
		}
		prepare(t, f)
		before := readOpenItemState(t, f.s, f.task.ID, item.ID)
		_, err := f.s.CloseItemTeam(ctx, f.task.ID, f.req, by)
		if !errors.Is(err, api.ErrConflict) {
			t.Fatalf("terminal=%v accepted or wrong error: %v", terminal, err)
		}
		if after := readOpenItemState(t, f.s, f.task.ID, item.ID); after != before {
			t.Fatalf("terminal=%v refusal changed the item: %+v to %+v", terminal, before, after)
		}
		current, _ := f.s.GetTask(ctx, f.task.ID)
		lead, _ := f.s.GetAgent(ctx, f.lead.ID)
		worker, _ := f.s.GetAgent(ctx, f.worker.ID)
		var receipts int
		if err := f.s.db.QueryRow(`SELECT count(*) FROM team_close_receipts WHERE task_id=?`, f.task.ID).Scan(&receipts); err != nil {
			t.Fatal(err)
		}
		if current.Orchestrator != f.task.Orchestrator || current.LeadRevision != f.task.LeadRevision || lead.Status != f.lead.Status || worker.Status != f.worker.Status || receipts != 0 {
			t.Fatalf("terminal=%v refusal mutated project, agents or receipts", terminal)
		}
		var wait *api.TeamCloseWaitError
		if errors.As(err, &wait) {
			if wait.Code == "team-close-obligations" {
				return wait.Code, "open team obligations"
			}
			return wait.Code, wait.Text
		}
		return "", err.Error()
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			openCode, openText := refusal(t, check.prepare, false)
			terminalCode, terminalText := refusal(t, check.prepare, true)
			if openCode != check.code || openCode != terminalCode || openText != terminalText {
				t.Fatalf("open item %q %q; terminal item %q %q; want code %q", openCode, openText, terminalCode, terminalText, check.code)
			}
		})
	}
}

// a3: the reason is part of the receipt and of the request identity; a
// terminal close stores none.
func TestCloseItemTeamReasonOnReceiptAndReplay(t *testing.T) {
	s, task, item, _, _, _, req := teamCloseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
	req.Reason = api.TeamCloseReasonFindingsOnly
	result, err := s.CloseItemTeam(ctx, task.ID, req, by)
	if err != nil || result.Reason != api.TeamCloseReasonFindingsOnly {
		t.Fatalf("close %+v %v", result, err)
	}
	receipt, err := s.GetTeamCloseReceipt(ctx, task.ID, req.RequestID)
	if err != nil || receipt.Reason != api.TeamCloseReasonFindingsOnly || receipt.ItemID != item.ID || !slices.Equal(receipt.Members, result.Members) {
		t.Fatalf("receipt %+v %v", receipt, err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT result_json FROM team_close_receipts WHERE task_id=? AND request_id=?`, task.ID, req.RequestID).Scan(&stored); err != nil || !strings.Contains(stored, `"reason":"findings-only"`) {
		t.Fatalf("stored receipt %s %v", stored, err)
	}
	replay, err := s.CloseItemTeam(ctx, task.ID, req, by)
	if err != nil || replay.Reason != result.Reason || replay.LeadAgentID != result.LeadAgentID || !slices.Equal(replay.Members, result.Members) {
		t.Fatalf("replay %+v %v", replay, err)
	}
	other := req
	other.Reason = api.TeamCloseReasonOwnerHold
	if _, err := s.CloseItemTeam(ctx, task.ID, other, by); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "request identity reused") {
		t.Fatalf("same request with the other reason: %v", err)
	}
	if same, err := s.GetTeamCloseReceipt(ctx, task.ID, req.RequestID); err != nil || same.Reason != api.TeamCloseReasonFindingsOnly {
		t.Fatalf("receipt after conflict %+v %v", same, err)
	}

	s, task, item, _, _, _, req = teamCloseFixture(t)
	teamCloseTerminal(t, s, task, item, "dismissed")
	terminal, err := s.CloseItemTeam(ctx, task.ID, req, by)
	if err != nil || terminal.Reason != "" {
		t.Fatalf("terminal close %+v %v", terminal, err)
	}
	if err := s.db.QueryRow(`SELECT result_json FROM team_close_receipts WHERE task_id=? AND request_id=?`, task.ID, req.RequestID).Scan(&stored); err != nil || strings.Contains(stored, "reason") {
		t.Fatalf("terminal receipt %s %v", stored, err)
	}
}

// a6: without a reason an open item's team is refused exactly as before; an
// unknown reason is invalid; a reason does not apply to a terminal item.
func TestCloseItemTeamOpenItemWithoutReasonRefusedAsToday(t *testing.T) {
	f := newRebindFixture(t, true)
	untouched := func(t *testing.T, name string, item openItemState, entry api.TeamQueueEntry) {
		t.Helper()
		if after := readOpenItemState(t, f.s, f.task.ID, f.item.ID); after != item {
			t.Fatalf("%s changed the item: %+v to %+v", name, item, after)
		}
		for _, a := range []api.Agent{f.lead, f.worker} {
			current, _ := f.s.GetAgent(f.ctx, a.ID)
			if current.Status != api.AgentRunning {
				t.Fatalf("%s changed %s to %s", name, a.Name, current.Status)
			}
		}
		var leadState string
		var leadRevision, receipts int
		if err := f.s.db.QueryRow(`SELECT state,revision FROM item_team_leads WHERE task_id=? AND item_id=?`, f.task.ID, f.item.ID).Scan(&leadState, &leadRevision); err != nil || leadState != "running" || leadRevision != 1 {
			t.Fatalf("%s changed the lead row: %s revision=%d %v", name, leadState, leadRevision, err)
		}
		if err := f.s.db.QueryRow(`SELECT count(*) FROM team_close_receipts WHERE task_id=?`, f.task.ID).Scan(&receipts); err != nil || receipts != 0 {
			t.Fatalf("%s stored %d receipts %v", name, receipts, err)
		}
		project, _ := f.s.GetTask(f.ctx, f.task.ID)
		if project.Status != api.TaskOpen || project.LeadRevision != f.task.LeadRevision || project.Orchestrator != f.task.Orchestrator {
			t.Fatalf("%s changed the project: %+v", name, project)
		}
		current, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry.ID)
		if err != nil || current.State != entry.State || current.Revision != entry.Revision || current.Failure != entry.Failure || current.ReleasedAt != entry.ReleasedAt {
			t.Fatalf("%s changed the queue entry: %+v %v", name, current, err)
		}
	}
	open := readOpenItemState(t, f.s, f.task.ID, f.item.ID)
	req := queueTeamCloseRequest(t, f, "no-reason", "")
	_, err := f.s.CloseItemTeam(f.ctx, f.task.ID, req, f.by)
	if !errors.Is(err, api.ErrConflict) || err.Error() != api.ErrConflict.Error()+": item is not terminal at the selected revision" {
		t.Fatalf("open item without a reason: %v", err)
	}
	untouched(t, "no reason", open, f.entry)

	req = queueTeamCloseRequest(t, f, "unknown-reason", "bogus")
	if _, err := f.s.CloseItemTeam(f.ctx, f.task.ID, req, f.by); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("unknown reason: %v", err)
	}
	untouched(t, "unknown reason", open, f.entry)

	// A reason on a terminal item is refused; the entry stays running for the
	// runner's ordinary close and finish.
	if done := teamCloseTerminal(t, f.s, f.task, f.item, "done"); done.Status != "done" {
		t.Fatalf("fixture item is %s", done.Status)
	}
	terminal := readOpenItemState(t, f.s, f.task.ID, f.item.ID)
	req = queueTeamCloseRequest(t, f, "reason-on-terminal", api.TeamCloseReasonOwnerHold)
	if _, err := f.s.CloseItemTeam(f.ctx, f.task.ID, req, f.by); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "item is terminal; close its team without a reason") {
		t.Fatalf("reason on a terminal item: %v", err)
	}
	untouched(t, "reason on a terminal item", terminal, f.entry)
}

// queueTeamCloseRequest is the owner's close of the queue fixture's live
// team: its lead and worker at the exact lead row revision.
func queueTeamCloseRequest(t *testing.T, f *rebindFixture, key, reason string) api.TeamCloseRequest {
	t.Helper()
	var revision int64
	if err := f.s.db.QueryRow(`SELECT revision FROM item_team_leads WHERE task_id=? AND item_id=?`, f.task.ID, f.item.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	req := api.TeamCloseRequest{RequestID: key, LeadAgentID: f.lead.ID, LeadRunID: f.lead.RunID, LeadRevision: revision, ItemID: f.item.ID, ItemRevision: f.item.Revision, Reason: reason}
	for _, a := range []api.Agent{f.lead, f.worker} {
		current, err := f.s.GetAgent(f.ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		req.Members = append(req.Members, api.TeamCloseMember{AgentID: current.ID, RunID: current.RunID, Host: current.Host, Status: current.Status})
	}
	slices.SortFunc(req.Members, func(a, b api.TeamCloseMember) int { return strings.Compare(a.AgentID, b.AgentID) })
	return req
}

// closeOpenQueueTeam closes the queue fixture's team with a reason and
// returns the entry it left behind.
func closeOpenQueueTeam(t *testing.T, f *rebindFixture, reason string) api.TeamQueueEntry {
	t.Helper()
	if _, err := f.s.CloseItemTeam(f.ctx, f.task.ID, queueTeamCloseRequest(t, f, "open-close-"+reason, reason), f.by); err != nil {
		t.Fatalf("close with %s: %v", reason, err)
	}
	entry, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// a4: the close ends the item's running entry as failed with the reason. The
// ordinary release then frees its slot, handler lease and owned paths.
func TestOpenItemTeamCloseEndsQueueEntryFailedAndReleases(t *testing.T) {
	for _, reason := range []string{api.TeamCloseReasonOwnerHold, api.TeamCloseReasonFindingsOnly} {
		t.Run(reason, func(t *testing.T) {
			f := newRebindFixture(t, true)
			// A second item wants the same paths and waits behind the first.
			waiting := f.action(t, api.TeamQueueRequest{Operation: "add", ItemID: f.other.ID, OrderMessageSeq: f.otherOrder.Seq, Host: "mini", Cwd: "/worktrees/other", Repository: rebindRepository, BaseCommit: strings.Repeat("a", 40), Ownership: []string{"hub/internal/store"}})
			claim := func() (api.TeamQueueEntry, error) {
				return f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "claim", EntryID: waiting.ID, ExpectedRevision: waiting.Revision, Host: "mini"})
			}
			if _, err := claim(); err == nil {
				t.Fatal("the waiting entry claimed beside the running one")
			}
			item := readOpenItemState(t, f.s, f.task.ID, f.item.ID)
			var messages int
			if err := f.s.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID).Scan(&messages); err != nil {
				t.Fatal(err)
			}

			failed := closeOpenQueueTeam(t, f, reason)
			if failed.State != "failed" || failed.Failure != "Team closed with the item open: "+reason || failed.Revision != f.entry.Revision+1 || failed.ReleasedAt != "" || failed.EscalationSeq != 0 || failed.Integration != nil || failed.OwnerIntegration != nil {
				t.Fatalf("entry after close %+v", failed)
			}
			if after := readOpenItemState(t, f.s, f.task.ID, f.item.ID); after != item {
				t.Fatalf("item changed: %+v to %+v", item, after)
			}
			var after int
			if err := f.s.db.QueryRow(`SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID).Scan(&after); err != nil || after != messages {
				t.Fatalf("the close posted %d messages %v", after-messages, err)
			}
			var reserved int
			if err := f.s.db.QueryRow(`SELECT count(*) FROM team_launch_reservations WHERE task_id=? AND entry_id=?`, f.task.ID, f.entry.ID).Scan(&reserved); err != nil || reserved != 1 {
				t.Fatalf("reservation rows %d %v", reserved, err)
			}
			if _, err := claim(); err == nil {
				t.Fatal("the waiting entry claimed before the closed team was released")
			}

			// Closed members have no cleanup receipts yet.
			release := func() (api.TeamQueueEntry, error) {
				return f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("req"), Operation: "release", EntryID: failed.ID, ExpectedRevision: failed.Revision})
			}
			if _, err := release(); !errors.Is(err, api.ErrConflict) {
				t.Fatalf("release before cleanup receipts: %v", err)
			}
			if _, err := f.s.db.Exec(`UPDATE agents SET cleanup_done=1 WHERE id IN (?,?) AND status='closed'`, f.lead.ID, f.worker.ID); err != nil {
				t.Fatal(err)
			}
			released, err := release()
			if err != nil {
				t.Fatalf("release after cleanup receipts: %v", err)
			}
			if released.State != "failed" || released.ReleasedAt == "" || released.Failure != failed.Failure || released.Integration != nil || released.OwnerIntegration != nil || !queueEntryIsHistory(released) || queueEntryHoldsResources(released) {
				t.Fatalf("released entry %+v", released)
			}
			if err := f.s.db.QueryRow(`SELECT count(*) FROM team_launch_reservations WHERE task_id=? AND entry_id=?`, f.task.ID, f.entry.ID).Scan(&reserved); err != nil || reserved != 0 {
				t.Fatalf("reservation rows after release %d %v", reserved, err)
			}
			if after := readOpenItemState(t, f.s, f.task.ID, f.item.ID); after != item {
				t.Fatalf("release changed the item: %+v to %+v", item, after)
			}

			claimed, err := claim()
			if err != nil {
				t.Fatalf("overlapping entry after release: %v", err)
			}
			if claimed.State != "launching" || claimed.HandlerID != f.handler.ID || claimed.HandlerRunID != f.handler.RunID || claimed.HandlerLeaseGeneration == 0 {
				t.Fatalf("overlapping entry claim %+v", claimed)
			}
		})
	}

	// A terminal item's close does not touch its running entry: the runner
	// finishes it.
	t.Run("done item keeps its entry running", func(t *testing.T) {
		f := newRebindFixture(t, true)
		done := teamCloseTerminal(t, f.s, f.task, f.item, "done")
		if done.Status != "done" {
			t.Fatalf("fixture item is %s", done.Status)
		}
		before, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry.ID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := f.s.CloseItemTeam(f.ctx, f.task.ID, queueTeamCloseRequest(t, f, "terminal-close", ""), f.by)
		if err != nil || result.Reason != "" {
			t.Fatalf("terminal close %+v %v", result, err)
		}
		entry, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry.ID)
		if err != nil || entry.State != "running" || entry.Revision != before.Revision || entry.Failure != "" {
			t.Fatalf("entry after a terminal close %+v %v", entry, err)
		}
	})
}

// a5: the later order on the same item is a requeue: a new entry and a new
// team, with the closed team's entry kept as history.
func TestOpenItemTeamCloseThenRequeueStartsFreshTeam(t *testing.T) {
	f := newRebindFixture(t, true)
	closed := closeOpenQueueTeam(t, f, api.TeamCloseReasonFindingsOnly)
	if _, err := f.s.db.Exec(`UPDATE agents SET cleanup_done=1 WHERE id IN (?,?) AND status='closed'`, f.lead.ID, f.worker.ID); err != nil {
		t.Fatal(err)
	}
	f.entry = f.action(t, api.TeamQueueRequest{Operation: "release", EntryID: closed.ID, ExpectedRevision: closed.Revision})
	item, err := f.s.GetWorkItem(f.ctx, f.task.ID, f.item.ID)
	if err != nil || item.Status != "open" || item.Revision != f.item.Revision {
		t.Fatalf("item %+v %v", item, err)
	}

	retry := f.action(t, f.requeue())
	if retry.ID == f.entry.ID || !validTeamQueueID(retry.ID) || retry.ItemID != f.item.ID || retry.Attempt != 2 || retry.RetryOf != f.entry.ID || retry.State != "queued" || retry.ItemRevision != item.Revision || retry.HandlerID != "" || retry.Failure != "" {
		t.Fatalf("retry %+v", retry)
	}
	q := f.action(t, api.TeamQueueRequest{Operation: "claim", EntryID: retry.ID, ExpectedRevision: retry.Revision, Host: "mini"})
	lead, worker := f.member(t, "lead-two"), f.member(t, "worker-two")
	for _, old := range []api.Agent{f.lead, f.worker} {
		if lead.ID == old.ID || lead.RunID == old.RunID || worker.ID == old.ID || worker.RunID == old.RunID {
			t.Fatalf("the fresh team reused an identity: %+v %+v", lead, worker)
		}
	}
	plan, _ := json.Marshal(map[string]any{"task": f.task.ID, "item": f.item.ID, "revision": item.Revision, "order": f.order.Seq, "context": map[string]any{"version": 1}, "members": []any{map[string]any{"state": "unstarted", "runId": lead.RunID, "fields": map[string]any{"agentId": lead.ID, "name": lead.Name, "cwd": "/worktrees/rebind"}}}})
	for _, step := range []api.TeamQueueRequest{{Operation: "freeze", LaunchJSON: plan}, {Operation: "attempt"}, {Operation: "started", MemberRunID: lead.RunID}, {Operation: "running"}} {
		step.EntryID, step.ExpectedRevision = q.ID, q.Revision
		q = f.action(t, step)
	}
	if q.State != "running" || q.Attempt != 2 || q.ID != retry.ID {
		t.Fatalf("running retry %+v", q)
	}
	var leadAgent, leadRun, leadState string
	if err := f.s.db.QueryRow(`SELECT agent_id,run_id,state FROM item_team_leads WHERE task_id=? AND item_id=?`, f.task.ID, f.item.ID).Scan(&leadAgent, &leadRun, &leadState); err != nil || leadAgent != lead.ID || leadRun != lead.RunID || leadState != "running" {
		t.Fatalf("lead row %s/%s %s %v", leadAgent, leadRun, leadState, err)
	}
	for _, a := range []api.Agent{lead, worker} {
		current, err := f.s.GetAgent(f.ctx, a.ID)
		if err != nil || current.Status != api.AgentRunning || current.WorkItem == nil || current.WorkItem.ItemID != f.item.ID || current.WorkItem.ItemRevision != item.Revision {
			t.Fatalf("fresh member %+v %v", current, err)
		}
	}
	history, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry.ID)
	if err != nil || history.State != "failed" || history.ReleasedAt == "" || history.Attempt != 1 || history.Failure != "Team closed with the item open: findings-only" {
		t.Fatalf("closed team's entry %+v %v", history, err)
	}
}
