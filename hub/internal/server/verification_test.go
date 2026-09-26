package server

import (
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/testverification"
)

func prepareHTTPVerification(t *testing.T, c *client, task string, item api.WorkItem) {
	t.Helper()
	native, err := api.NewClient(c.srv.URL, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = testverification.Prepare(native, task, item); err != nil {
		t.Fatal(err)
	}
}
func TestVerificationHTTPRefusesNonHandlerAndMissingReceipt(t *testing.T) {
	c := newClient(t)
	task := c.task("verification-http")
	var item api.WorkItem
	if code := c.do("POST", "/v1/tasks/"+task.ID+"/work-items", api.CreateWorkItemRequest{Kind: "bug", Title: "Verification fixture", RequestID: "item"}, &item); code != 201 {
		t.Fatal(code)
	}
	path := "/v1/tasks/" + task.ID + "/work-items/" + item.ID
	done := "done"
	if code := c.do("PATCH", path, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, nil); code != 409 {
		t.Fatal("missing receipt", code)
	}
	if code := c.do("GET", path+"/verification?agent=agt_0000000000000000&run=run_0000000000000000", nil, nil); code != 409 {
		t.Fatal("nonhandler", code)
	}
	prepareHTTPVerification(t, c, task.ID, item)
	if code := c.do("PATCH", path, api.UpdateWorkItemRequest{Revision: 1, Status: &done}, nil); code != 200 {
		t.Fatal("passing receipt", code)
	}
}
