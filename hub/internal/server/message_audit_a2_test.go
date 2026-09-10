package server

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestMessageAuditA2HTTPWireAndGoClient(t *testing.T) {
	fixture := newMessageAuditFixture(t)
	c := fixture.c
	task := c.task("a2-http-wire")
	var source api.Message
	auditMust(c, http.StatusCreated, "POST", auditPostPath(task.ID), api.PostMessageRequest{Text: "Human A2 evidence"}, &source)
	item := auditItem(c, task, "a2-http-item")
	client, err := api.NewClient(c.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	intake, err := client.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Explicit intake", AuditKind: api.MessageAuditIntake, RequestID: "a2-http-intake"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := client.GetMessageAudit(ctx, task.ID, intake.Seq)
	if err != nil || record.Original.Classification != api.MessageAuditIntake || record.Current == nil || record.Current.Revision != 1 {
		t.Fatalf("intake audit: %+v err=%v", record, err)
	}
	resolve := api.ResolveMessageAuditRequest{RequestID: "a2-http-resolve", ExpectedRevision: 1, Reason: "The existing item is authoritative.",
		Sources:      []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}},
		ExistingItem: &api.MessageWorkItem{ItemTaskID: task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}
	resolved, err := client.ResolveMessageAudit(ctx, task.ID, intake.Seq, resolve)
	if err != nil || resolved.Item == nil || resolved.Item.ID != item.ID || resolved.State.Current.Revision != 2 || resolved.Receipt.ID == "" {
		t.Fatalf("resolve result: %+v err=%v", resolved, err)
	}
	receipt, err := client.GetMessageAuditReceipt(ctx, task.ID, resolve.RequestID, "resolve", "")
	if err != nil || !receipt.Replay || receipt.Receipt.ID != resolved.Receipt.ID {
		t.Fatalf("resolve receipt: %+v err=%v", receipt, err)
	}
	history, err := client.ListMessageAuditHistory(ctx, task.ID, intake.Seq, 0, "", 1)
	if err != nil || len(history.Events) != 1 || history.NextCursor == "" || history.Cutoff != 2 {
		t.Fatalf("history page: %+v err=%v", history, err)
	}
	history2, err := client.ListMessageAuditHistory(ctx, task.ID, intake.Seq, 0, history.NextCursor, 2)
	if err != nil || len(history2.Events) != 1 || history2.Events[0].Revision != 2 || history2.Cutoff != history.Cutoff {
		t.Fatalf("history continuation: %+v err=%v", history2, err)
	}
	if _, err := client.ListMessageAuditHistory(ctx, task.ID, source.Seq, 0, history.NextCursor, 1); err == nil {
		t.Fatal("cross-message history cursor accepted")
	}

	correct := api.CorrectMessageAuditRequest{RequestID: "a2-http-correct", ExpectedRevision: 2, Reason: "Return to intake with an append-only event.",
		Sources: []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}}, Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditIntake}}
	corrected, err := client.CorrectMessageAudit(ctx, task.ID, intake.Seq, correct)
	if err != nil || corrected.Event.Before == nil || corrected.Event.After.Classification != api.MessageAuditIntake {
		t.Fatalf("correction: %+v err=%v", corrected, err)
	}
	postReceipt, err := client.GetMessagePostReceipt(ctx, task.ID, "a2-http-intake", "")
	if err != nil || !reflect.DeepEqual(postReceipt, intake) {
		t.Fatalf("post receipt gained current audit state: %+v original=%+v err=%v", postReceipt, intake, err)
	}

	feed, err := client.ListMessageAuditChanges(ctx, task.ID, api.MessageAuditChangeQuery{Limit: 1})
	if err != nil || len(feed.Events) != 1 || feed.NextCursor == "" {
		t.Fatalf("change feed: %+v err=%v", feed, err)
	}
	code, body, exchangeErr := auditExchange(c, "GET", "/v1/tasks/"+task.ID+"/message-audit/changes?limit=1&kind=intake&cursor="+feed.NextCursor, nil, nil)
	if exchangeErr != nil || code != http.StatusBadRequest {
		t.Fatalf("filter-swapped cursor status=%d err=%v body=%s", code, exchangeErr, body)
	}
	code, body, exchangeErr = auditExchange(c, "GET", "/v1/tasks/"+task.ID+"/message-audit/changes?limit=1&cursor=not-a-cursor", nil, nil)
	if exchangeErr != nil || code != http.StatusBadRequest {
		t.Fatalf("malformed cursor status=%d err=%v body=%s", code, exchangeErr, body)
	}

	foreignTask := c.task("a2-http-foreign-source")
	foreignItem := auditItem(c, foreignTask, "a2-http-foreign-item")
	associationReq := api.CreateMessageAuditAssociationRequest{RequestID: "a2-http-association",
		Item:   api.MessageAuditItemReference{ItemTaskID: foreignTask.ID, ItemID: foreignItem.ID, ItemRevision: foreignItem.Revision},
		Source: api.MessageReference{TaskID: task.ID, Seq: source.Seq}, Reason: "The human source explicitly authorizes this project relationship."}
	association, err := client.CreateMessageAuditAssociation(ctx, task.ID, associationReq)
	if err != nil || association.Association.Source != associationReq.Source || association.Receipt.ID == "" {
		t.Fatalf("association: %+v err=%v", association, err)
	}
	associationReceipt, err := client.GetMessageAuditAssociationReceipt(ctx, task.ID, associationReq.RequestID, "")
	if err != nil || !associationReceipt.Replay || associationReceipt.Receipt.ID != association.Receipt.ID {
		t.Fatalf("association receipt: %+v err=%v", associationReceipt, err)
	}
	foreignIntake, err := client.PostMessage(ctx, task.ID, api.PostMessageRequest{Text: "Foreign-linked intake", AuditKind: api.MessageAuditIntake, RequestID: "a2-http-foreign-intake"})
	if err != nil {
		t.Fatal(err)
	}
	foreignCorrection := api.CorrectMessageAuditRequest{RequestID: "a2-http-foreign-correction", ExpectedRevision: 1, Reason: "Use the explicit human association.",
		Sources: []api.MessageReference{{TaskID: task.ID, Seq: source.Seq}}, Desired: api.MessageAuditDesiredState{Classification: api.MessageAuditWork,
			WorkItems: []api.MessageWorkItem{{ItemTaskID: foreignTask.ID, ItemID: foreignItem.ID, ItemRevision: foreignItem.Revision, Relationship: "primary"}}}}
	if _, err := client.CorrectMessageAudit(ctx, task.ID, foreignIntake.Seq, foreignCorrection); err != nil {
		t.Fatalf("explicit foreign correction: %v", err)
	}

	wrong := associationReq
	wrong.RequestID = associationReq.RequestID
	wrong.Reason = "Changed request payload"
	if _, err := client.CreateMessageAuditAssociation(ctx, task.ID, wrong); err == nil {
		t.Fatal("changed association retry accepted")
	} else {
		var httpErr *api.HTTPError
		if !errors.As(err, &httpErr) || httpErr.Status != http.StatusConflict {
			t.Fatalf("changed association retry error=%v", err)
		}
	}

	code, body, exchangeErr = auditExchange(c, "GET", "/v1/tasks/"+task.ID+"/message-audit/changes?limit=64", nil, nil)
	if exchangeErr != nil || code != http.StatusOK || len(body) > api.MaxMessageAuditResponseBytes {
		t.Fatalf("response bound status=%d bytes=%d err=%v", code, len(body), exchangeErr)
	}
}
