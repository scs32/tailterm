package broker

import (
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Owner delegation windows expire on the broker's clock, in paused projects
// too, exactly once across repeated ticks and a restart.
func TestDelegationWindowExpiresOnce(t *testing.T) {
	f := newFixture(t)
	item, err := f.st.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "feature", Title: "Delegation broker fixture", AgentID: f.lead.ID, RequestID: "item"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	out, err := f.st.OpenDelegationWindow(f.ctx, f.task.ID, api.OpenDelegationWindowRequest{Delegate: f.lead.Name, EndsAt: start.Add(time.Hour), Scope: api.DelegationScopeDecisions, RequestID: "open"}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.PostMessage(f.ctx, f.task.ID, api.PostMessageRequest{AgentID: f.builder.ID, RunID: f.builder.RunID, To: "owner", RequestID: "ask", Envelope: &api.Envelope{Kind: api.EnvelopeKindRequest, To: "owner", Subject: "Choose the broker fixture order", Body: api.EnvelopeBody{Ask: "Which?"}},
		WorkItems: []api.MessageWorkItem{{ItemTaskID: f.task.ID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}}}, f.by); err != nil {
		t.Fatal(err)
	}
	pause := api.PauseProjectRequest{Version: 1, RequestID: "pause", Targets: []api.ProjectPauseTargetRequest{
		{AgentID: f.lead.ID, RunID: f.lead.RunID, ServiceDisposition: api.PauseServiceNone},
		{AgentID: f.builder.ID, RunID: f.builder.RunID, ServiceDisposition: api.PauseServiceNone}}}
	if _, err := f.st.PauseProject(f.ctx, f.task.ID, pause, f.by); err != nil {
		t.Fatal(err)
	}
	expiries := func(steps []Step) int {
		n := 0
		for _, s := range steps {
			if s.Action == "delegation-expired" && s.WindowID == out.Window.ID {
				n++
			}
		}
		return n
	}
	b := &Broker{Store: f.st}
	if steps, err := b.Tick(f.ctx, start.Add(30*time.Minute)); err != nil || expiries(steps) != 0 {
		t.Fatalf("early tick %v %+v", err, steps)
	}
	if steps, err := b.Tick(f.ctx, start.Add(time.Hour+time.Second)); err != nil || expiries(steps) != 1 {
		t.Fatalf("expiry tick %v %+v", err, steps)
	}
	if steps, err := b.Tick(f.ctx, start.Add(time.Hour+time.Minute)); err != nil || expiries(steps) != 0 {
		t.Fatalf("repeat tick %v %+v", err, steps)
	}
	f.restart(t)
	b = &Broker{Store: f.st}
	if steps, err := b.Tick(f.ctx, start.Add(2*time.Hour)); err != nil || expiries(steps) != 0 {
		t.Fatalf("tick after restart %v %+v", err, steps)
	}
	list, err := f.st.ListDelegationWindows(f.ctx, f.task.ID)
	if err != nil || len(list.Windows) != 1 {
		t.Fatal(err)
	}
	w := list.Windows[0]
	if w.State != api.DelegationExpired || len(w.Routes) != 1 || w.Routes[0].ReturnedAt == nil {
		t.Fatalf("expired window %+v", w)
	}
}
