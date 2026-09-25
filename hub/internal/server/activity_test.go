package server

import (
	"context"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

func TestActivityEndpointPersistsExactRunTransition(t *testing.T) {
	fixture := newClient(t)
	ctx := context.Background()
	task, err := fixture.st.CreateTask(ctx, api.CreateTaskRequest{Name: "Activity HTTP fixture"}, fixture.who)
	if err != nil {
		t.Fatal(err)
	}
	a, err := fixture.st.AddAgent(ctx, task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "builder", Host: "mini", Session: "fake", Runtime: "codex", Cwd: t.TempDir()}, fixture.who)
	if err != nil {
		t.Fatal(err)
	}
	client, err := api.NewClient(fixture.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req := api.ActivityReport{RequestID: "activity-http-one", RunID: a.RunID, Activity: api.AgentActivity{State: "working", ObservedAt: time.Now().UTC(), Tokens: api.TokenTotals{Total: 42}}}
	if _, err := client.ReportActivity(ctx, task.ID, a.ID, req); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReportActivity(ctx, task.ID, a.ID, req); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	got, err := client.GetAgent(ctx, task.ID, a.ID)
	if err != nil || got.Activity == nil || got.Activity.State != "working" || got.Activity.Tokens.Total != 42 {
		t.Fatalf("HTTP readback %+v %v", got.Activity, err)
	}
	req.RunID = api.NewID("run")
	if _, err := client.ReportActivity(ctx, task.ID, a.ID, req); err == nil {
		t.Fatal("changed exact-run replay accepted")
	}
}
