package server

// HTTP acceptance for wi_c97ba465a6f2a3ab-routing-1, work order #814.
// Every task, item, message, agent and database is synthetic and isolated.
import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestAgentWorkItemContextHTTPAcceptanceAndLegacyCompatibility(t *testing.T) {
	c := newMessageAuditFixture(t).c
	task := c.task("item-context-http")
	lead := c.agent(task, "lead")
	item := auditItem(c, task, "context-http-item")
	var order api.Message
	orderRequest := auditLinked(item, "context-http-order")
	orderRequest.AgentID = lead.ID
	orderRequest.Text = "Recorded bounded synthetic order"
	auditMust(c, http.StatusCreated, "POST", auditPostPath(task.ID), orderRequest, &order)
	var unrelated api.Message
	auditMust(c, http.StatusCreated, "POST", auditPostPath(task.ID), api.PostMessageRequest{Text: "UNRELATED HTTP HISTORY"}, &unrelated)

	request := api.AddAgentRequest{
		Name: "bound-http", Host: "fixture", Session: "bound-http", Runtime: "codex",
		WorkItem: &api.AgentWorkItemRequest{
			ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision,
			WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: order.Seq},
		},
	}
	request.WorkItem.ContextBundle = syntheticHTTPContext(t, item, order)
	var agent api.Agent
	auditMust(c, http.StatusCreated, "POST", "/v1/tasks/"+task.ID+"/agents", request, &agent)
	if agent.WorkItem == nil || agent.ReadUpTo != unrelated.Seq || agent.Unread != 0 {
		t.Fatalf("admission did not bind/cut off history: %+v", agent)
	}
	var context api.AgentWorkItemContext
	auditMust(c, http.StatusOK, "GET", "/v1/tasks/"+task.ID+"/agents/"+agent.ID+"/work-context?runId="+agent.RunID, nil, &context)
	if context.Binding.AgentID != agent.ID || context.Binding.RunID != agent.RunID || len(context.Bundle) == 0 {
		t.Fatalf("wrong exact-run context: %+v", context)
	}
	auditMust(c, http.StatusConflict, "GET", "/v1/tasks/"+task.ID+"/agents/"+agent.ID+"/work-context?runId="+api.NewID("run"), nil, nil)
	auditMust(c, http.StatusNotFound, "GET", "/v1/tasks/"+task.ID+"/agents/"+lead.ID+"/work-context?runId="+lead.RunID, nil, nil)

	// Old clients can still admit unbound ordinary agents and receive the old
	// top-level shape with no synthetic item binding.
	var legacy api.Agent
	auditMust(c, http.StatusCreated, "POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{Name: "legacy-http", Host: "fixture", Session: "legacy-http", Runtime: "generic"}, &legacy)
	if legacy.WorkItem != nil {
		t.Fatalf("legacy admission acquired context: %+v", legacy)
	}
}

func syntheticHTTPContext(t *testing.T, item api.WorkItem, order api.Message) []byte {
	t.Helper()
	revision := api.WorkItemRevision{
		ItemID: item.ID, TaskID: item.TaskID, Kind: item.Kind, Title: item.Title, Description: item.Description,
		Status: item.Status, Priority: item.Priority, ItemSeq: item.Seq, Revision: item.Revision,
		CreatedBy: item.CreatedBy, UpdatedBy: item.UpdatedBy, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		AttributionKind: "shared_workspace_claim", ChangeKind: "created", Provenance: "native",
	}
	data, err := json.Marshal(map[string]any{
		"version": 1, "itemTaskId": item.TaskID, "itemId": item.ID, "itemRevision": item.Revision,
		"workOrderMessage": api.MessageReference{TaskID: item.TaskID, Seq: order.Seq},
		"history": map[string]any{
			"revision": revision, "revisions": []api.WorkItemRevision{revision},
			"messages": []api.WorkItemMessageLink{{ItemRevision: item.Revision, RevisionCoverage: "verified", Relationship: "primary", Message: order}},
			"coverage": api.HistoryCoverage{Complete: true, ObservedCurrentRevision: item.Revision, LatestMaterialized: item.Revision, SnapshotCount: 1, ConversationLinks: "explicit_only"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
