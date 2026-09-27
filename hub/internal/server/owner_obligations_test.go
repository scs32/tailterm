package server

import (
	"github.com/scs32/tailterm/hub/internal/api"
	"testing"
)

func TestOwnerRequestHTTPReadAndAnswer(t *testing.T) {
	h := newTokenHub(t)
	task, builder, _ := h.project()
	base := "/v1/tasks/" + task.ID
	var item api.WorkItem
	if code, _ := h.do(ownerToken, "POST", base+"/work-items", api.CreateWorkItemRequest{Kind: "feature", Title: "Owner HTTP fixture", RequestID: "owner-item"}, &item); code != 201 {
		t.Fatal(code)
	}
	var m api.Message
	req := api.PostMessageRequest{AgentID: builder.ID, RunID: builder.RunID, To: "owner", RequestID: "owner-ask", Envelope: &api.Envelope{Kind: "request", To: "owner", Subject: "Approve the HTTP fixture request", ExpectedAnswer: "APPROVE exact 矩阵\n", Body: api.EnvelopeBody{Ask: "Please approve"}}, WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}
	if code, raw := h.do(ownerToken, "POST", base+"/messages", req, &m); code != 201 {
		t.Fatalf("post %d %s", code, raw)
	}
	var list api.ObligationList
	if code, _ := h.do(ownerToken, "GET", base+"/obligations?owner=1&open=1&agentId=invalid&runId=invalid", nil, &list); code != 200 || len(list.Obligations) != 1 {
		t.Fatalf("owner list %d %+v", code, list)
	}
	o := list.Obligations[0]
	if o.Request == nil || o.Request.Seq != m.Seq {
		t.Fatal("full request absent")
	}
	var out api.OwnerActionResult
	a := api.ObligationAnswerRequest{Approve: true, RequestID: "approval"}
	if code, raw := h.do(bridgeToken, "POST", base+"/obligations/"+o.ID+"/answer", a, &out); code != 201 {
		t.Fatalf("answer %d %s", code, raw)
	}
	if out.Message.Envelope.Body.Answer != m.Envelope.ExpectedAnswer {
		t.Fatal("changed bytes")
	}
	if code, _ := h.do(bridgeToken, "POST", base+"/obligations/"+o.ID+"/answer", a, &out); code != 200 || !out.Replay {
		t.Fatal("retry not recovered")
	}
}
