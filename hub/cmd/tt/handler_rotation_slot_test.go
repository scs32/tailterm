package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Handler rotation with no free agent slot (bug wi_caa61d47ad431bf5, order
// #48740): the hub refuses the prepare with no_agent_slot, and the runner
// waits it out with one notice. Isolated fixture hub only (newRotationCLI).

// rotationFixtureCaller is the owner caller of the fixture hub.
var rotationFixtureCaller = api.Caller{Node: "fixture", User: "owner"}

// fillAgentSlots registers ordinary agents until the store refuses one at the
// project's agent cap, and returns them. They register through the store,
// past the hub's write rate limit; the cap is never assigned on the store,
// which the fixture's server goroutines read.
func (f *rotationCLI) fillAgentSlots(t *testing.T) []api.Agent {
	t.Helper()
	ctx := context.Background()
	var fillers []api.Agent
	for i := 0; ; i++ {
		name := "filler-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		a, err := f.st.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: name, Host: "fixture", Session: name, Runtime: "generic"}, rotationFixtureCaller)
		if errors.Is(err, api.ErrLimit) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if i >= api.MaxAgentsPerTask {
			t.Fatal("the hub registered more agents than its cap")
		}
		fillers = append(fillers, a)
	}
	if len(fillers) < 4 {
		t.Fatalf("only %d filler agents fit", len(fillers))
	}
	return fillers
}

// event moves an agent to the state an event kind records.
func (f *rotationCLI) event(t *testing.T, a api.Agent, kind string) {
	t.Helper()
	if _, err := f.st.PostEvent(context.Background(), f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: kind}, rotationFixtureCaller); err != nil {
		t.Fatal(err)
	}
}

// closeAgent frees one agent slot.
func (f *rotationCLI) closeAgent(t *testing.T, a api.Agent) {
	t.Helper()
	if _, err := f.st.CloseAgent(context.Background(), a.ID, rotationFixtureCaller); err != nil {
		t.Fatal(err)
	}
}

func (f *rotationCLI) noSlotNotices(t *testing.T) []api.Message {
	t.Helper()
	messages, err := f.c.ListMessages(context.Background(), f.task.ID, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Message
	for _, m := range messages {
		if m.Envelope != nil && m.Envelope.Subject == rotationNoSlotSubject {
			out = append(out, m)
		}
	}
	return out
}

// k1-k3: a due rotation on a full project prepares nothing, spawns nothing,
// keeps no journal and posts one notice, across ticks and a relay restart;
// once a slot frees, the next tick rotates.
func TestHandlerRotationRunnerWaitsForAnAgentSlot(t *testing.T) {
	f := newRotationCLI(t, "")
	ctx := context.Background()
	if err := saveHandlerSpec(f.spec); err != nil {
		t.Fatal(err)
	}
	setRotationPolicy(t, f, api.HandlerRotationPolicyRequest{Enabled: true, MaxItems: 1})
	f.request(t, f.old.Name, "Record the agent slot fixture item")
	f.finishedLeases(t, 1)
	// A done persistent handler is never offered for closing.
	spare, err := f.st.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Role: api.AgentRoleDatabaseHandler, Name: "spare-handler", Host: "elsewhere", Session: "spare-handler", Runtime: "generic"}, rotationFixtureCaller)
	if err != nil {
		t.Fatal(err)
	}
	f.event(t, spare, api.EventDone)
	fillers := f.fillAgentSlots(t)
	f.event(t, fillers[0], api.EventDone)
	// A retire is a lifecycle action, not a postable event; set its state.
	if _, err = f.db(t).Exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentRetired, fillers[1].ID); err != nil {
		t.Fatal(err)
	}
	r := f.runner()
	for i := 0; i < 2; i++ {
		f.tick(t, r)
	}
	f.tick(t, f.runner())
	if rotations := f.rotations(t); len(rotations) != 0 {
		t.Fatalf("a full project prepared: %+v", rotations)
	}
	if f.spawns != 0 || f.journalExists() {
		t.Fatalf("a full project launched or journaled: spawns=%d journal=%v", f.spawns, f.journalExists())
	}
	notices := f.noSlotNotices(t)
	if len(notices) != 1 {
		t.Fatalf("no-slot notices: %d", len(notices))
	}
	text := notices[0].Envelope.Body.Text
	for _, want := range []string{"no_agent_slot", "open + 1 seats > 32", fillers[0].Name + " (done)", fillers[1].Name + " (retired)", "Nothing was prepared", "This notice closes nothing", f.old.Name} {
		if !strings.Contains(text, want) {
			t.Fatalf("notice lacks %q: %s", want, text)
		}
	}
	for _, absent := range []string{spare.Name, fillers[2].Name, f.worker.Name + " ("} {
		if strings.Contains(text, absent) {
			t.Fatalf("notice names %q: %s", absent, text)
		}
	}
	f.closeAgent(t, fillers[0])
	f.tick(t, r)
	rotations := f.rotations(t)
	if len(rotations) != 1 {
		t.Fatalf("rotations once a slot freed: %+v", rotations)
	}
	got, err := f.c.GetHandlerRotation(ctx, f.task.ID, rotations[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != api.HandlerRotationCommitted || got.Reason != api.HandlerRotationReasonItems || got.Receipt == nil || got.Receipt.Reissued != 1 {
		t.Fatalf("rotation once a slot freed: %+v", got)
	}
	if len(f.noSlotNotices(t)) != 1 {
		t.Fatal("the rotation posted a second no-slot notice")
	}
}

// k5: a manual abort on a full project is not followed by a new prepared
// rotation; no back-off is recorded, and the runner rotates once a slot frees.
func TestHandlerRotationManualAbortWithNoSlotPreparesNothing(t *testing.T) {
	f := newRotationCLI(t, "")
	ctx := context.Background()
	if err := saveHandlerSpec(f.spec); err != nil {
		t.Fatal(err)
	}
	f.online = false
	f.deps.online = 200 * time.Millisecond
	if _, err := f.rotate(t); err == nil || !strings.Contains(err.Error(), "did not come online") {
		t.Fatalf("offline successor: %v", err)
	}
	aborted, err := abortHandlerRotation(ctx, f.deps, f.e, f.c, f.task.ID)
	if err != nil || aborted.State != api.HandlerRotationAborted {
		t.Fatalf("abort: %+v %v", aborted, err)
	}
	if loadRotationBackoff(f.c.Base, f.task.ID) != nil {
		t.Fatal("a manual abort recorded a back-off")
	}
	fillers := f.fillAgentSlots(t)
	setRotationPolicy(t, f, api.HandlerRotationPolicyRequest{Enabled: true, MaxItems: 1})
	f.finishedLeases(t, 1)
	spawns := f.spawns
	f.tick(t, f.runner())
	rotations := f.rotations(t)
	if len(rotations) != 1 || rotations[0].ID != aborted.ID || f.spawns != spawns || f.journalExists() {
		t.Fatalf("after the manual abort on a full project: rotations %+v spawns %d journal %v", rotations, f.spawns-spawns, f.journalExists())
	}
	if len(f.noSlotNotices(t)) != 1 {
		t.Fatal("want one no-slot notice")
	}
	f.closeAgent(t, fillers[0])
	f.online = true
	f.tick(t, f.runner())
	rotations = f.rotations(t)
	var committed *api.HandlerRotation
	for i := range rotations {
		if rotations[i].ID != aborted.ID {
			committed = &rotations[i]
		}
	}
	if len(rotations) != 2 || committed == nil || committed.State != api.HandlerRotationCommitted || committed.SuccessorAgentID == aborted.SuccessorAgentID {
		t.Fatalf("rotation once a slot freed: %+v", rotations)
	}
}

// An owner rotate on a full project gets the refusal code and keeps no journal.
func TestHandlerRotationOwnerRotateReportsNoAgentSlot(t *testing.T) {
	f := newRotationCLI(t, "")
	f.fillAgentSlots(t)
	_, err := f.rotate(t)
	if code, ok := rotationRefusal(err); !ok || code != api.HandlerRotationRefusedNoAgentSlot || !noAgentSlot(err) || !rotationWait(err) {
		t.Fatalf("owner rotate on a full project: %q %v", code, err)
	}
	if f.journalExists() || f.spawns != 0 || len(f.rotations(t)) != 0 {
		t.Fatalf("refused rotate left state: journal %v spawns %d", f.journalExists(), f.spawns)
	}
}
