package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestWorkItemEvidenceClientUsesOnlySelectedNativeGetter(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"native\":true}\n"))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	reads := []WorkItemEvidenceRead{
		{Operation: WorkItemEvidenceCurrent, TaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002"},
		{Operation: WorkItemEvidenceRevisions, TaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002", After: 3, Limit: 7},
		{Operation: WorkItemEvidenceMessages, TaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002", After: 9, Limit: 7, Revision: 2},
		{Operation: WorkItemEvidenceReceipt, TaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002", RequestID: "key/with slash", AgentID: "agt_0000000000000003"},
	}
	for _, read := range reads {
		response, readErr := client.ReadWorkItemEvidence(context.Background(), read)
		if readErr != nil || response.StatusCode != http.StatusOK || string(response.Body) != "{\"native\":true}\n" {
			t.Fatalf("read %s = %+v %v", read.Operation, response, readErr)
		}
	}
	want := []string{
		"/v1/tasks/tsk_0000000000000001/work-items/wi_0000000000000002",
		"/v1/tasks/tsk_0000000000000001/work-items/wi_0000000000000002/revisions?after=3&limit=7",
		"/v1/tasks/tsk_0000000000000001/work-items/wi_0000000000000002/messages?after=9&limit=7&revision=2",
		"/v1/tasks/tsk_0000000000000001/work-items/wi_0000000000000002/updates/receipts/key%2Fwith%20slash?agentId=agt_0000000000000003",
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests=\n%q\nwant=\n%q", requests, want)
	}
}

func TestWorkItemEvidenceClientRejectsAmbiguousGetterParameters(t *testing.T) {
	client, err := NewClient("http://example.invalid", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, read := range []WorkItemEvidenceRead{
		{Operation: WorkItemEvidenceCurrent, TaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002", Revision: 1},
		{Operation: WorkItemEvidenceRevisions, TaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002", Limit: 0},
		{Operation: WorkItemEvidenceMessages, TaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002", Limit: 1, Revision: -1},
		{Operation: WorkItemEvidenceReceipt, TaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002"},
		{Operation: "overview", TaskID: "tsk_0000000000000001", ItemID: "wi_0000000000000002"},
	} {
		if _, readErr := client.ReadWorkItemEvidence(context.Background(), read); readErr == nil {
			t.Fatalf("ambiguous read accepted: %+v", read)
		}
	}
}
