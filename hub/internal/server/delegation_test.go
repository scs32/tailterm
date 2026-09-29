package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestDelegationWindowHTTP(t *testing.T) {
	h := newTokenHub(t)
	task, builder, _ := h.project()
	ctx := context.Background()
	owner := api.Caller{Node: "workspace", User: "owner"}
	base := "/v1/tasks/" + task.ID
	agents, err := h.st.ListAgents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var lead api.Agent
	for _, a := range agents {
		if a.Name == "lead" {
			lead = a
		}
	}
	other, err := h.st.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "other", Host: "mini", Session: "tt-other", Runtime: "codex"}, owner)
	if err != nil {
		t.Fatal(err)
	}
	var item api.WorkItem
	if code, _ := h.do(ownerToken, "POST", base+"/work-items", api.CreateWorkItemRequest{Kind: "feature", Title: "Delegation HTTP fixture", RequestID: "item"}, &item); code != 201 {
		t.Fatal(code)
	}
	ask := func(key string) api.Obligation {
		t.Helper()
		var m api.Message
		req := api.PostMessageRequest{AgentID: builder.ID, RunID: builder.RunID, To: "owner", RequestID: key, Envelope: &api.Envelope{Kind: "request", To: "owner", Subject: "Choose the HTTP fixture rollout order", Body: api.EnvelopeBody{Ask: "Which?"}},
			WorkItems: []api.MessageWorkItem{{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}
		if code, _ := h.do(ownerToken, "POST", base+"/messages", req, &m); code != 201 {
			t.Fatalf("ask %d", code)
		}
		var list api.ObligationList
		h.do(ownerToken, "GET", base+"/obligations?owner=1&open=1", nil, &list)
		for _, o := range list.Obligations {
			if o.MessageSeq == m.Seq {
				return o
			}
		}
		t.Fatal("owner request missing")
		return api.Obligation{}
	}
	open := api.OpenDelegationWindowRequest{Delegate: "lead", EndsAt: time.Now().Add(2 * time.Hour), Scope: api.DelegationScopeDecisions, Source: &api.DelegationSource{Kind: "tailos"}, RequestID: "open"}
	for name, bad := range map[string]api.OpenDelegationWindowRequest{
		"scope":    {Delegate: "lead", EndsAt: open.EndsAt, Scope: "all", RequestID: "bad-scope"},
		"short":    {Delegate: "lead", EndsAt: time.Now().Add(10 * time.Second), Scope: "decisions", RequestID: "bad-short"},
		"long":     {Delegate: "lead", EndsAt: time.Now().Add(9 * 24 * time.Hour), Scope: "decisions", RequestID: "bad-long"},
		"delegate": {Delegate: "nobody", EndsAt: open.EndsAt, Scope: "decisions", RequestID: "bad-delegate"},
	} {
		if code, _ := h.do(ownerToken, "POST", base+"/delegation-windows", bad, nil); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", name, code)
		}
	}
	var opened api.OwnerActionResult
	if code, _ := h.do(ownerToken, "POST", base+"/delegation-windows", open, &opened); code != 201 || opened.Window == nil || opened.Window.DelegateAgentID != lead.ID {
		t.Fatalf("open %d %+v", code, opened)
	}
	var replay api.OwnerActionResult
	if code, _ := h.do(ownerToken, "POST", base+"/delegation-windows", open, &replay); code != 200 || !replay.Replay || replay.Window.ID != opened.Window.ID {
		t.Fatalf("replay %d", code)
	}
	second := open
	second.RequestID = "open-2"
	if code, _ := h.do(ownerToken, "POST", base+"/delegation-windows", second, nil); code != http.StatusConflict {
		t.Fatalf("second open %d", code)
	}
	o := ask("ask-1")
	answer := func(agent api.Agent, key, rationale string) (int, api.OwnerActionResult) {
		var out api.OwnerActionResult
		code, _ := h.do(ownerToken, "POST", base+"/obligations/"+o.ID+"/answer", api.ObligationAnswerRequest{AgentID: agent.ID, RunID: agent.RunID, Text: "Staged first", Rationale: rationale, RequestID: key}, &out)
		return code, out
	}
	if code, _ := answer(lead, "blank", " "); code != http.StatusBadRequest {
		t.Fatalf("blank rationale %d", code)
	}
	if code, _ := answer(other, "other", "because"); code != http.StatusForbidden {
		t.Fatalf("other agent %d", code)
	}
	if code, _ := answer(api.Agent{ID: lead.ID, RunID: "run_0000000000000000"}, "stale", "because"); code != http.StatusForbidden {
		t.Fatalf("stale run %d", code)
	}
	code, out := answer(lead, "ok", "Staged limits the blast radius.")
	if code != 201 || out.Message == nil || out.Message.From.AgentID != lead.ID || out.Message.Envelope.Refs["delegated"] != "true" || out.Message.Envelope.Body.Reason != "Staged limits the blast radius." {
		t.Fatalf("delegated answer %d %+v", code, out)
	}
	if code, again := answer(lead, "ok", "Staged limits the blast radius."); code != 200 || !again.Replay {
		t.Fatalf("answer replay %d", code)
	}
	if code, _ := answer(lead, "again", "because"); code != http.StatusConflict {
		t.Fatalf("second answer %d", code)
	}
	// Board decision answered by the delegate.
	var decision api.Message
	if code, _ := h.do(ownerToken, "POST", base+"/decisions", api.CreateDecisionRequest{AgentID: builder.ID, RequestID: "decision", DecisionRequest: api.DecisionRequest{Question: "Which HTTP rollout?", RecommendedOptionID: "a", RecommendationReason: "Safer.",
		Options: []api.DecisionOption{{ID: "a", Label: "A", Description: "a"}, {ID: "b", Label: "B", Description: "b"}}}}, &decision); code != 201 {
		t.Fatalf("decision %d", code)
	}
	path := base + "/decisions/" + itoa(decision.Seq) + "/answer"
	if code, _ := h.do(ownerToken, "POST", path, api.AnswerDecisionRequest{RequestID: "d-other", AgentID: other.ID, RunID: other.RunID, OptionID: "a", Rationale: "r"}, nil); code != http.StatusForbidden {
		t.Fatalf("decision other %d", code)
	}
	var reply api.Message
	if code, _ := h.do(ownerToken, "POST", path, api.AnswerDecisionRequest{RequestID: "d-ok", AgentID: lead.ID, RunID: lead.RunID, OptionID: "a", Rationale: "Recommended and safe."}, &reply); code != 201 || reply.From.AgentID != lead.ID {
		t.Fatalf("decision delegate %d %+v", code, reply)
	}
	// Close from the bridge credential with its Discord source; list shows the answers after.
	var closed api.OwnerActionResult
	closeReq := api.CloseDelegationWindowRequest{Reason: "back", Source: &api.DelegationSource{Kind: "discord", ID: "1234", UserID: "5678"}, RequestID: "close"}
	if code, _ := h.do(bridgeToken, "POST", base+"/delegation-windows/"+opened.Window.ID+"/close", closeReq, &closed); code != 201 || closed.Window.State != api.DelegationClosed || closed.Window.ClosedSource.UserID != "5678" {
		t.Fatalf("close %d %+v", code, closed)
	}
	closeReq.RequestID = "close-again"
	if code, _ := h.do(ownerToken, "POST", base+"/delegation-windows/"+opened.Window.ID+"/close", closeReq, nil); code != http.StatusConflict {
		t.Fatalf("close again %d", code)
	}
	if code, _ := h.do(ownerToken, "POST", base+"/delegation-windows/not-a-window/close", api.CloseDelegationWindowRequest{RequestID: "bad"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad window id %d", code)
	}
	var list api.DelegationWindowList
	if code, _ := h.do(bridgeToken, "GET", base+"/delegation-windows", nil, &list); code != 200 || len(list.Windows) != 1 || len(list.Windows[0].Routes) != 2 {
		t.Fatalf("list %d %+v", code, list)
	}
	for _, r := range list.Windows[0].Routes {
		if r.AnswerSeq == 0 || r.Rationale == "" || r.AnsweredByName != "lead" || r.AnsweredAt == nil {
			t.Fatalf("route %+v", r)
		}
	}
	// After the window: agents are refused again.
	o = ask("ask-2")
	if code, _ := answer(lead, "after", "because"); code != http.StatusForbidden {
		t.Fatalf("after close %d", code)
	}
}
