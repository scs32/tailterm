package server

// HTTP acceptance for wi_c97ba465a6f2a3ab-routing-1/order #814 and
// wi_649b1999c31d8fb9 r2/order #1240 and
// wi_dd57670ee65d974c r3/order #3622.
// Every task, item, message, agent and database is synthetic and isolated.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestAgentRegistrationBodyEnvelopeAndContextBoundaries(t *testing.T) {
	c := newMessageAuditFixture(t).c
	task := c.task("item-context-body")
	lead := c.agent(task, "context-body-lead")
	item := auditItem(c, task, "context-body-item")
	var order api.Message
	orderRequest := auditLinked(item, "context-body-order")
	orderRequest.AgentID = lead.ID
	orderRequest.Text = "Recorded bounded synthetic order"
	auditMust(c, http.StatusCreated, "POST", auditPostPath(task.ID), orderRequest, &order)
	confirmHTTPContextOrder(t, c, task, item, order)

	client, err := api.NewClient(c.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request := api.AddAgentRequest{
		Name: "large-context", Host: "fixture", Session: "large-context", Runtime: "codex",
		WorkItem: &api.AgentWorkItemRequest{
			ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision,
			WorkOrderMessage: api.MessageReference{TaskID: task.ID, Seq: order.Seq},
		},
	}
	request.WorkItem.ContextBundle = syntheticHTTPContextSized(t, item, order, 269315, "x")
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.WorkItem.ContextBundle) <= api.MaxBody || len(encoded) <= api.MaxBody {
		t.Fatalf("fixture did not reproduce the old 64 KiB request rejection: bundle=%d request=%d", len(request.WorkItem.ContextBundle), len(encoded))
	}
	agent, err := client.AddAgent(context.Background(), task.ID, request)
	if err != nil {
		t.Fatalf("supported context registration failed: bundle=%d request=%d: %v", len(request.WorkItem.ContextBundle), len(encoded), err)
	}
	if agent.WorkItem == nil || agent.WorkItem.RunID != agent.RunID || agent.WorkItem.AgentID != agent.ID {
		t.Fatalf("registration lost exact item/run identity: %+v", agent)
	}
	if _, err = client.GetAgentWorkItemContext(context.Background(), task.ID, agent.ID, api.NewID("run")); !httpErrorStatus(err, http.StatusConflict) {
		t.Fatalf("mismatched run context lookup = %v, want 409", err)
	}
	if _, err = client.AddAgent(context.Background(), task.ID, request); !httpErrorStatus(err, http.StatusConflict) {
		t.Fatalf("duplicate item registration = %v, want 409", err)
	}
	agents, err := client.ListAgents(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	registered := 0
	for _, candidate := range agents {
		if candidate.Name == request.Name {
			registered++
			if candidate.ID != agent.ID || candidate.RunID != agent.RunID {
				t.Fatalf("retry changed identity: first=%s/%s listed=%s/%s", agent.ID, agent.RunID, candidate.ID, candidate.RunID)
			}
		}
	}
	if registered != 1 {
		t.Fatalf("retry admitted %d agents named %q", registered, request.Name)
	}

	boundary := request
	boundary.Name, boundary.Session = "boundary-context", "boundary-context"
	boundary.WorkItem = cloneWorkItemRequest(request.WorkItem)
	boundary.WorkItem.ContextBundle = syntheticHTTPContextSized(t, item, order, api.MaxAgentWorkItemContextBytes, "<")
	boundaryBody := marshalContextNoHTMLEscape(t, boundary)
	if len(boundaryBody) <= api.MaxAgentWorkItemContextBytes || len(boundaryBody) > api.MaxAgentRegistrationBody {
		t.Fatalf("encoded boundary request size %d is outside (%d, %d]", len(boundaryBody), api.MaxAgentWorkItemContextBytes, api.MaxAgentRegistrationBody)
	}
	boundaryAgent, err := client.AddAgent(context.Background(), task.ID, boundary)
	if err != nil {
		t.Fatalf("exact %d-byte context registration failed with %d-byte encoded envelope: %v", api.MaxAgentWorkItemContextBytes, len(boundaryBody), err)
	}
	storedBoundary, err := client.GetAgentWorkItemContext(context.Background(), task.ID, boundaryAgent.ID, boundaryAgent.RunID)
	boundaryDigest := sha256.Sum256(boundary.WorkItem.ContextBundle)
	if err != nil || !bytes.Equal(storedBoundary.Bundle, boundary.WorkItem.ContextBundle) || storedBoundary.Binding.ContextDigest != hex.EncodeToString(boundaryDigest[:]) {
		t.Fatalf("boundary context digest changed: err=%v stored=%s want=%s", err, storedBoundary.Binding.ContextDigest, hex.EncodeToString(boundaryDigest[:]))
	}

	unicodeBoundary := request
	unicodeBoundary.Name, unicodeBoundary.Session = "unicode-context", "unicode-context"
	unicodeBoundary.WorkItem = cloneWorkItemRequest(request.WorkItem)
	unicodeBoundary.WorkItem.ContextBundle = syntheticHTTPContextSized(t, item, order, api.MaxAgentWorkItemContextBytes, "界")
	unicodeBoundary.WorkItem.ContextBundle = bytes.Replace(unicodeBoundary.WorkItem.ContextBundle, []byte("界界"), []byte("\u2028\u2029"), 1)
	unicodeBody := marshalContextNoHTMLEscape(t, unicodeBoundary)
	if len(unicodeBody) <= api.MaxAgentWorkItemContextBytes || len(unicodeBody) > api.MaxAgentRegistrationBody {
		t.Fatalf("encoded Unicode boundary request size %d is outside (%d, %d]", len(unicodeBody), api.MaxAgentWorkItemContextBytes, api.MaxAgentRegistrationBody)
	}
	unicodeAgent, err := client.AddAgent(context.Background(), task.ID, unicodeBoundary)
	if err != nil {
		t.Fatalf("exact %d-byte Unicode context registration failed with %d-byte encoded envelope: %v", api.MaxAgentWorkItemContextBytes, len(unicodeBody), err)
	}
	unicodeDigest := sha256.Sum256(unicodeBoundary.WorkItem.ContextBundle)
	if unicodeAgent.WorkItem == nil || unicodeAgent.WorkItem.ContextDigest != hex.EncodeToString(unicodeDigest[:]) {
		t.Fatalf("Unicode boundary context digest changed: agent=%+v want=%s", unicodeAgent, hex.EncodeToString(unicodeDigest[:]))
	}

	// Direct HTTP clients may preserve whitespace in RawMessage. Readback must
	// retain those exact admitted bytes, not just equivalent JSON values.
	spaced := request
	spaced.Name, spaced.Session = "spaced-context", "spaced-context"
	spaced.WorkItem = cloneWorkItemRequest(request.WorkItem)
	spacedRaw := bytes.Replace(spaced.WorkItem.ContextBundle, []byte(`"history":{`), []byte(`"history": { `), 1)
	spaced.WorkItem.ContextBundle = spacedRaw
	body := marshalContextNoHTMLEscape(t, spaced)
	compactRaw := marshalContextNoHTMLEscape(t, json.RawMessage(spacedRaw))
	body = bytes.Replace(body, compactRaw, spacedRaw, 1)
	var spacedAgent api.Agent
	if status := c.do("POST", "/v1/tasks/"+task.ID+"/agents", string(body), &spacedAgent); status != http.StatusCreated {
		t.Fatalf("direct spaced registration = %d", status)
	}
	spacedContext, err := client.GetAgentWorkItemContext(context.Background(), task.ID, spacedAgent.ID, spacedAgent.RunID)
	hash := sha256.Sum256(spacedRaw)
	if err != nil || !bytes.Equal(spacedContext.Bundle, spacedRaw) || spacedContext.Binding.ContextDigest != hex.EncodeToString(hash[:]) {
		t.Fatalf("direct HTTP exact bytes changed: %v", err)
	}

	overLimit := request
	overLimit.Name, overLimit.Session = "over-limit-context", "over-limit-context"
	overLimit.WorkItem = cloneWorkItemRequest(request.WorkItem)
	overLimit.WorkItem.ContextBundle = syntheticHTTPContextSized(t, item, order, api.MaxAgentWorkItemContextBytes+1, "x")
	if _, err = client.AddAgent(context.Background(), task.ID, overLimit); !httpError(err, http.StatusConflict, api.ErrContextLimit.Error()) {
		t.Fatalf("over-limit context registration = %v, want independent 409", err)
	}
	invalid := request
	invalid.Name, invalid.Session = "invalid-context", "invalid-context"
	invalid.WorkItem = cloneWorkItemRequest(request.WorkItem)
	invalid.WorkItem.ContextBundle = json.RawMessage(`{}`)
	if _, err = client.AddAgent(context.Background(), task.ID, invalid); !httpError(err, http.StatusBadRequest, "invalid request") {
		t.Fatalf("invalid context registration = %v, want independent 400", err)
	}

	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", "{not json", nil); code != http.StatusBadRequest {
		t.Fatalf("malformed agent registration = %d, want 400", code)
	}
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/agents", strings.Repeat(" ", api.MaxAgentRegistrationBody+1), nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("registration beyond safe envelope = %d, want 413", code)
	}
	big := strings.Repeat("x", api.MaxBody+1)
	if code := c.do("POST", "/v1/tasks", `{"name":"unchanged","goal":"`+big+`"}`, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("unrelated oversized request = %d, want 413", code)
	}
}

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
	confirmHTTPContextOrder(t, c, task, item, order)
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

func confirmHTTPContextOrder(t *testing.T, c *client, task api.Task, item api.WorkItem, order api.Message) api.Agent {
	t.Helper()
	var handler api.Agent
	auditMust(c, http.StatusCreated, "POST", "/v1/tasks/"+task.ID+"/agents", api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "context-handler", Role: api.AgentRoleDatabaseHandler, Host: "fixture", Session: "context-handler", Runtime: "codex", Cwd: "/tmp"}, &handler)
	auditMust(c, http.StatusCreated, "POST", "/v1/tasks/"+task.ID+"/work-items/"+item.ID+"/order-scope/confirm", api.ConfirmWorkOrderScopeRequest{RequestID: "context-intake", AgentID: handler.ID, RunID: handler.RunID, ExpectedRevision: item.Revision, ScopeRevision: item.ScopeRevision, OrderMessageSeq: order.Seq, Complete: true}, nil)
	return handler
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

func syntheticHTTPContextSized(t *testing.T, item api.WorkItem, order api.Message, size int, fill string) []byte {
	t.Helper()
	revision := api.WorkItemRevision{
		ItemID: item.ID, TaskID: item.TaskID, Kind: item.Kind, Title: item.Title, Description: item.Description,
		Status: item.Status, Priority: item.Priority, ItemSeq: item.Seq, Revision: item.Revision,
		CreatedBy: item.CreatedBy, UpdatedBy: item.UpdatedBy, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
		AttributionKind: "shared_workspace_claim", ChangeKind: "created", Provenance: "native",
	}
	messages := []api.WorkItemMessageLink{{
		ItemRevision: item.Revision, RevisionCoverage: "verified", Relationship: "primary", Message: order,
	}}
	bundle := map[string]any{
		"version": 1, "itemTaskId": item.TaskID, "itemId": item.ID, "itemRevision": item.Revision,
		"workOrderMessage": api.MessageReference{TaskID: item.TaskID, Seq: order.Seq},
		"history": map[string]any{
			"revision": revision, "revisions": []api.WorkItemRevision{revision},
			"messages": messages,
			"coverage": api.HistoryCoverage{Complete: true, ObservedCurrentRevision: item.Revision, LatestMaterialized: item.Revision, SnapshotCount: 1, ConversationLinks: "explicit_only"},
		},
	}
	for {
		data := marshalContextNoHTMLEscape(t, bundle)
		remaining := size - len(data)
		if remaining == 0 {
			return data
		}
		if remaining < 0 {
			t.Fatalf("context fixture overhead %d exceeds requested size %d", len(data), size)
		}
		textBytes := min(remaining, api.MaxTextLen)
		messages = append(messages, api.WorkItemMessageLink{
			ItemRevision: item.Revision, RevisionCoverage: "verified",
			Message: api.Message{TaskID: item.TaskID, Seq: order.Seq + int64(len(messages)), Text: fillBytes(fill, textBytes)},
		})
		bundle["history"].(map[string]any)["messages"] = messages
		withMessage := marshalContextNoHTMLEscape(t, bundle)
		if len(withMessage) > size {
			overhead := len(withMessage) - len(data) - textBytes
			messages[len(messages)-1].Message.Text = fillBytes(fill, remaining-overhead)
			bundle["history"].(map[string]any)["messages"] = messages
			final := marshalContextNoHTMLEscape(t, bundle)
			if len(final) != size {
				t.Fatalf("context fixture size = %d, want %d", len(final), size)
			}
			return final
		}
	}
}

func fillBytes(fill string, size int) string {
	return strings.Repeat(fill, size/len(fill)) + strings.Repeat("x", size%len(fill))
}

func marshalContextNoHTMLEscape(t *testing.T, value any) []byte {
	t.Helper()
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func cloneWorkItemRequest(request *api.AgentWorkItemRequest) *api.AgentWorkItemRequest {
	clone := *request
	clone.ContextBundle = append(json.RawMessage(nil), request.ContextBundle...)
	return &clone
}

func httpErrorStatus(err error, status int) bool {
	httpErr, ok := err.(*api.HTTPError)
	return ok && httpErr.Status == status
}

func httpError(err error, status int, message string) bool {
	httpErr, ok := err.(*api.HTTPError)
	return ok && httpErr.Status == status && httpErr.Msg == message
}
