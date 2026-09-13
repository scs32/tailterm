package server

import (
	"github.com/scs32/tailterm/hub/internal/api"
	"testing"
)

func TestOperationalHTTPStrictSchemaAndProposal(t *testing.T) {
	c := newClient(t)
	var task api.Task
	if code := c.do("POST", "/v1/tasks", api.CreateTaskRequest{Name: "Operational HTTP", Orchestrator: "lead"}, &task); code != 201 {
		t.Fatal(code)
	}
	lead := c.agent(task, "lead")
	path := "/v1/tasks/" + task.ID + "/operational-records"
	req := api.ProposeOperationalRecordRequest{RequestID: "op-http", AgentID: lead.ID, RunID: lead.RunID, Data: api.OperationalData{Kind: "instruction"}}
	var first, replay api.OperationalMutation
	if code := c.do("POST", path, req, &first); code != 201 || first.Record.State != "proposed" || first.Event != nil {
		t.Fatalf("proposal %d %+v", code, first)
	}
	if code := c.do("POST", path, req, &replay); code != 201 || !replay.Replay || replay.Receipt.ID != first.Receipt.ID {
		t.Fatalf("replay %d %+v", code, replay)
	}
	var current api.OperationalRecord
	if code := c.do("GET", path+"/"+first.Record.ID, nil, &current); code != 200 || current.Version != 1 {
		t.Fatalf("get %d %+v", code, current)
	}
	if code := c.do("POST", path+"/"+first.Record.ID+"/commit", api.CommitOperationalRecordRequest{RequestID: "op-incomplete", ExpectedVersion: 1, AgentID: lead.ID, RunID: lead.RunID}, nil); code != 400 {
		t.Fatalf("incomplete commit %d", code)
	}
	forged := map[string]any{"requestId": "forged", "agentId": lead.ID, "runId": lead.RunID, "data": map[string]any{"kind": "instruction", "accepted": true}}
	if code := c.do("POST", path, forged, nil); code != 400 {
		t.Fatalf("unknown authority field accepted: %d", code)
	}
	var caps api.Capabilities
	if code := c.do("GET", "/v1/capabilities", nil, &caps); code != 200 || !caps.OperationalRecords.Supported || len(caps.OperationalRecords.Versions) != 1 || caps.OperationalRecords.Versions[0] != 1 {
		t.Fatalf("capability %d %+v", code, caps)
	}
}
