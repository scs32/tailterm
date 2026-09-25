package server

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestTeamCloseHTTPGatesAndReplay(t *testing.T) {
	for _, terminal := range []string{"dismissed", "done"} {
		t.Run(terminal, func(t *testing.T) {
			c := newClient(t)
			task := c.task("fixture team")
			leadName := "lead"
			var updated api.Task
			if code := c.do("PATCH", "/v1/tasks/"+task.ID, api.UpdateTaskRequest{Orchestrator: &leadName}, &updated); code != 200 {
				t.Fatalf("lead assignment %d", code)
			}
			var item api.WorkItem
			if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "feature", Title: "Team close fixture", RequestID: "team-close-item"}, &item); code != 201 {
				t.Fatalf("item %d", code)
			}
			var order api.Message
			if code := c.do("POST", "/v1/tasks/"+task.ID+"/messages", auditLinked(item, "team-close-order"), &order); code != 201 {
				t.Fatalf("order %d", code)
			}
			add := func(name, role string) api.Agent {
				var a api.Agent
				req := api.AddAgentRequest{Name: name, Role: role, Host: "fixture", Session: name, Runtime: "codex",
					WorkItem: &api.AgentWorkItemRequest{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision,
						WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: order.Seq}, ContextBundle: syntheticHTTPContext(t, item, order)}}
				if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", req, &a); code != 201 {
					t.Fatalf("add %s: %d", name, code)
				}
				return a
			}
			lead := add("lead", "")
			worker := add("worker", "")
			var other api.Agent
			if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{Name: "outside", Host: "fixture", Session: "outside"}, &other); code != 201 {
				t.Fatal(code)
			}
			var handler api.Agent
			if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{Name: "database", AgentID: api.NewID("agt"), Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "database"}, &handler); code != 201 {
				t.Fatal(code)
			}
			req := api.TeamCloseRequest{RequestID: "http-close-" + terminal, ActorAgentID: lead.ID, ActorRunID: lead.RunID,
				LeadAgentID: lead.ID, LeadRunID: lead.RunID, LeadRevision: updated.LeadRevision, ItemID: item.ID, ItemRevision: item.Revision,
				Members: []api.TeamCloseMember{{AgentID: lead.ID, RunID: lead.RunID, Host: lead.Host, Status: lead.Status}, {AgentID: worker.ID, RunID: worker.RunID, Host: worker.Host, Status: worker.Status}}}
			slices.SortFunc(req.Members, func(a, b api.TeamCloseMember) int { return strings.Compare(a.AgentID, b.AgentID) })
			path := "/v1/tasks/" + task.ID + "/team-close"
			if code := c.do("POST", path, req, nil); code != http.StatusConflict {
				t.Fatalf("nonterminal %d", code)
			}
			if terminal == "done" {
				report, _, err := c.st.PutNarrativeReport(t.Context(), task.ID, item.ID, api.PutNarrativeReportRequest{RequestID: "report", ScopeRevision: item.ScopeRevision, Sections: api.NarrativeReportSections{RequestedOutcome: "Close the item team.", DeliveredWork: "Synthetic implementation.", Verification: "Fixture verification.", Limitations: "Fixture only.", RemainingWork: "None."}, References: []api.NarrativeReference{{Kind: "work-item-revision", TaskID: task.ID, ItemID: item.ID, Revision: item.Revision, Label: "scope"}}}, c.who)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := c.st.CreateWorkItemUpdate(t.Context(), task.ID, item.ID, api.CreateWorkItemUpdate{ExpectedRevision: item.Revision, RequestID: "done", Status: &terminal, CompletionReport: &api.NarrativeReportPin{ReportID: report.ReportID, Version: report.Version, Digest: report.Digest, ScopeRevision: report.ScopeRevision}}, c.who); err != nil {
					t.Fatal(err)
				}
			} else if _, err := c.st.UpdateWorkItem(t.Context(), task.ID, item.ID, api.UpdateWorkItemRequest{Revision: item.Revision, Status: &terminal}, c.who); err != nil {
				t.Fatal(err)
			}
			stale := req
			stale.ActorRunID = api.NewID("run")
			stale.RequestID = "stale-actor"
			if code := c.do("POST", path, stale, nil); code != http.StatusConflict {
				t.Fatalf("stale actor %d", code)
			}
			stale = req
			stale.Members = append([]api.TeamCloseMember(nil), req.Members...)
			stale.Members[0].RunID = api.NewID("run")
			stale.RequestID = "stale-team"
			if code := c.do("POST", path, stale, nil); code != http.StatusConflict {
				t.Fatalf("stale team %d", code)
			}
			var result api.TeamCloseResult
			if code := c.do("POST", path, req, &result); code != 200 || len(result.Members) != 2 || result.Members[0].AgentID != worker.ID {
				t.Fatalf("close %d %+v", code, result)
			}
			if code := c.do("POST", path, req, &result); code != 200 {
				t.Fatalf("replay %d", code)
			}
			var detail api.TaskDetail
			if code := c.do("GET", "/v1/tasks/"+task.ID, nil, &detail); code != 200 || detail.Task.Status != api.TaskOpen || detail.Task.Orchestrator != "" {
				t.Fatalf("task %d %+v", code, detail.Task)
			}
			for _, id := range []string{other.ID, handler.ID} {
				var a api.Agent
				if code := c.do("GET", "/v1/tasks/"+task.ID+"/agents/"+id, nil, &a); code != 200 || a.Status == api.AgentClosed {
					t.Fatalf("unrelated closed %d %+v", code, a)
				}
			}
		})
	}
}
