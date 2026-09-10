package server

import (
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestQueueHTTPFrozenListActionHistoryChangesAndReceipt(t *testing.T) {
	c := newClient(t)
	source := c.task("queue-http-source")
	target := c.task("queue-http-target")
	lead := c.agent(target, "queue-http-lead")
	orchestrator := lead.Name
	if code := c.do("PATCH", "/v1/tasks/"+target.ID, api.UpdateTaskRequest{Orchestrator: &orchestrator}, nil); code != 200 {
		t.Fatalf("orchestrator=%d", code)
	}
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+source.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "HTTP Queue fixture", Description: "Synthetic only", RequestID: "queue-http-item"}, &item); code != 201 {
		t.Fatalf("item=%d", code)
	}
	var sent api.WorkItemDispatchResult
	if code := c.do("POST", "/v1/tasks/"+source.ID+"/work-items/"+item.ID+"/dispatch", api.DispatchWorkItemRequest{Revision: item.Revision, TargetTaskID: target.ID, RequestID: "queue-http-send"}, &sent); code != 201 || sent.Queue == nil {
		t.Fatalf("send=%d %+v", code, sent)
	}
	var list api.QueueList
	if code := c.do("GET", "/v1/tasks/"+target.ID+"/queue?limit=1", nil, &list); code != 200 || len(list.Entries) != 1 || list.Entries[0].ID != sent.Queue.Entry.ID {
		t.Fatalf("list=%d %+v", code, list)
	}
	req := api.QueueActionRequest{Operation: "priority", RequestID: "queue-http-priority", ExpectedRevision: list.Entries[0].Revision, Cycle: list.Entries[0].Cycle, Priority: "urgent"}
	var changed api.QueueActionResult
	if code := c.do("POST", "/v1/tasks/"+target.ID+"/queue/"+sent.Queue.Entry.ID+"/actions", req, &changed); code != 201 || changed.Entry.QueuePriority != "urgent" {
		t.Fatalf("action=%d %+v", code, changed)
	}
	var replay api.QueueActionResult
	if code := c.do("POST", "/v1/tasks/"+target.ID+"/queue/"+sent.Queue.Entry.ID+"/actions", req, &replay); code != 200 || !replay.Replay || replay.Event.Seq != changed.Event.Seq {
		t.Fatalf("replay=%d %+v", code, replay)
	}
	var receipt api.QueueActionResult
	if code := c.do("GET", "/v1/tasks/"+target.ID+"/queue-receipts/"+req.RequestID, nil, &receipt); code != 200 || !receipt.Replay || receipt.Receipt.ID != changed.Receipt.ID {
		t.Fatalf("receipt=%d %+v", code, receipt)
	}
	var exact api.QueueEntry
	if code := c.do("GET", "/v1/tasks/"+target.ID+"/queue/"+sent.Queue.Entry.ID, nil, &exact); code != 200 || exact.Revision != changed.Entry.Revision {
		t.Fatalf("exact=%d %+v", code, exact)
	}
	var history api.QueueHistory
	if code := c.do("GET", "/v1/tasks/"+target.ID+"/queue/"+sent.Queue.Entry.ID+"/history?limit=64", nil, &history); code != 200 || len(history.Events) != 2 {
		t.Fatalf("history=%d %+v", code, history)
	}
	var changes api.QueueChangePage
	if code := c.do("GET", "/v1/tasks/"+target.ID+"/queue/changes?after=0&limit=1", nil, &changes); code != 200 || len(changes.Events) != 1 || changes.Complete {
		t.Fatalf("changes=%d %+v", code, changes)
	}
	if code := c.do("GET", "/v1/tasks/"+target.ID+"/queue?limit=65", nil, nil); code != 400 {
		t.Fatalf("oversized list limit=%d", code)
	}
}
