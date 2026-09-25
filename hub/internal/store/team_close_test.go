package store

import (
	"context"
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

func TestCloseItemTeamRefusesOpenHeldAndSentObligations(t *testing.T) {
	s, task, item, lead, worker, _, req := teamCloseFixture(t)
	ctx := context.Background()
	by := api.Caller{Node: "fixture", User: "owner"}
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
