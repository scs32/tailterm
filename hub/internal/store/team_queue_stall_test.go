package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// armStallQueue is a parallel chores queue whose only online handlers are
// hS (arm S) and hO (arm O); the fixture's plain handler is offline.
type armStallQueue struct {
	*choresQueue
	advance func(time.Duration)
	hS, hO  api.Agent
}

func newArmStallQueue(t *testing.T, n int) *armStallQueue {
	t.Helper()
	f := &armStallQueue{choresQueue: newChoresQueue(t, n, 1, 0)}
	f.advance = f.clock(t)
	if _, err := f.s.db.Exec(`UPDATE agents SET last_seen_at='' WHERE id=?`, f.handlers[0].ID); err != nil {
		t.Fatal(err)
	}
	f.hS = f.armHandler(t, "handler-s", armS)
	f.hO = f.armHandler(t, "handler-o", armO)
	return f
}

// armHandler registers an online database handler recorded with an arm's
// model; online uses the real clock, as scanAgent does.
func (f *armStallQueue) armHandler(t *testing.T, name string, arm api.HandlerArm) api.Agent {
	t.Helper()
	ctx := context.Background()
	a, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{Name: name, Role: api.AgentRoleDatabaseHandler, AgentID: api.NewID("agt"), Host: "mini", Session: name,
		Runtime: arm.Runtime, TemplateDigest: digestP, HandlerModel: arm.Model, HandlerReasoning: arm.Reasoning}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE agents SET last_seen_at=?,status='running' WHERE id=?`, ts(time.Now().UTC()), a.ID); err != nil {
		t.Fatal(err)
	}
	if a, err = f.s.GetAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	return a
}

// policy saves the arm policy with a seed that draws each entry's arm.
func (f *armStallQueue) policy(t *testing.T, enabled bool, draws map[string]string) {
	t.Helper()
	seed := ""
	for i := 0; i < 10000 && seed == ""; i++ {
		seed = fmt.Sprintf("seed-%d", i)
		for id, want := range draws {
			if arm, _ := handlerArmDraw(seed, id, []api.HandlerArm{armS, armO}); arm != want {
				seed = ""
				break
			}
		}
	}
	if seed == "" {
		t.Fatalf("no seed draws %v", draws)
	}
	view, err := f.s.HandlerArmPolicy(context.Background(), f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetHandlerArmPolicy(context.Background(), f.task.ID, api.HandlerArmPolicyRequest{RequestID: api.NewID("arms"), ExpectedRevision: view.Policy.Revision, Enabled: enabled,
		Seed: seed, TemplateDigest: digestP, Arms: []api.HandlerArm{armS, armO}}); err != nil {
		t.Fatal(err)
	}
}

func (f *armStallQueue) report(t *testing.T, a api.Agent, activity api.AgentActivity) {
	t.Helper()
	activity.ObservedAt = f.s.now()
	activity.LastEventAt = activity.ObservedAt
	if _, err := f.s.ReportActivity(context.Background(), f.task.ID, a.ID, api.ActivityReport{RequestID: api.NewID("act"), RunID: a.RunID, Activity: activity}); err != nil {
		t.Fatal(err)
	}
}

// rotatingS registers a second online arm-S handler held by an open rotation,
// so arm S already has the handlers the uncapped queue wants for one team and
// the one that waits: the drawn arm's need adds nothing to the plain wait.
func (f *armStallQueue) rotatingS(t *testing.T) {
	t.Helper()
	h := f.armHandler(t, "handler-s2", armS)
	now := ts(time.Now())
	if _, err := f.s.db.Exec(`INSERT INTO handler_rotations(id,task_id,request_id,payload_hash,state,reason,trigger_kind,handler_revision,old_agent_id,old_run_id,old_name,successor_agent_id,successor_name,created_at,updated_at) VALUES('hrot_fixture',?,'rotate','hash','prepared','fixture','owner',1,?,?,?,'agt_successor','successor',?,?)`,
		f.task.ID, h.ID, h.RunID, h.Name, now, now); err != nil {
		t.Fatal(err)
	}
}

// stallNoticeText posts the entry's stall notice and returns its text.
func stallNoticeText(t *testing.T, s *Store, task, id string) string {
	t.Helper()
	ctx := context.Background()
	got := listedEntry(t, s, task, id)
	if got.Stall == nil {
		t.Fatalf("no stall on %s: %q", id, got.BlockReason)
	}
	if _, err := s.TeamQueueAction(ctx, task, api.TeamQueueRequest{RequestID: got.Stall.NoticeRequestID(id), Operation: "stall_notice", EntryID: id}); err != nil {
		t.Fatal(err)
	}
	messages, err := s.ListMessages(ctx, task, 0, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if m := messages[i]; m.Envelope != nil && m.Envelope.Refs["entry"] == id && m.Envelope.Refs["cause"] == got.Stall.Cause {
			return m.Envelope.Body.Text
		}
	}
	t.Fatalf("no stall notice for %s", id)
	return ""
}

const armSWait = ". It waits for a free handler of arm S"

// wi_376dfb3ee857ffcd a1, a2: arm S's free handler is leased by an entry with
// nothing running, its other one is rotating, and arm O's handler is free. The
// queued entry drawn to S cannot lease O without a fallback, and no handler of
// S is added, so it is stalled behind the holder, and the list and the notice
// name the arm.
func TestQueueStallArmWaitBlockerHoldsDrawnArm(t *testing.T) {
	f := newArmStallQueue(t, 2)
	a, b := f.add(t, 0, "src/a"), f.add(t, 1, "src/b")
	f.policy(t, true, map[string]string{a.ID: "S", b.ID: "S"})
	f.rotatingS(t)
	a = f.run(t, a)
	if a.HandlerID != f.hS.ID {
		t.Fatalf("holder leased %s, want handler-s", a.HandlerID)
	}
	if got := listedEntry(t, f.s, f.task.ID, b.ID); got.Stall != nil || !strings.HasPrefix(got.BlockReason, "Waiting for a free handler of arm S") {
		t.Fatalf("inside the grace: %+v %q", got.Stall, got.BlockReason)
	}
	f.advance(6 * time.Minute)
	assertStall(t, f.choresQueue, b.ID, api.StallNothingRunning, a.ID, "tt team queue fail --task "+f.task.ID+" --entry "+a.ID)
	got := listedEntry(t, f.s, f.task.ID, b.ID)
	if !strings.HasPrefix(got.BlockReason, "Stalled: "+a.ID+" ("+a.ItemID+"): ") || !strings.HasSuffix(got.BlockReason, armSWait) {
		t.Fatalf("arm stall reason %q", got.BlockReason)
	}
	if text := stallNoticeText(t, f.s, f.task.ID, b.ID); !strings.Contains(text, "arm S") || !strings.Contains(text, "Stalled: "+a.ID) {
		t.Fatalf("notice text %q", text)
	}
}

// wi_376dfb3ee857ffcd a2: no arm stall when the drawn arm is free, when it is
// held by a working team, or when every arm is at a provider limit. Only a
// drawn arm whose handler need says the runner is adding one (offered or
// reserved) keeps that need's reason (verifier finding #50081,
// wi_dbb1529b84b724ae).
func TestQueueStallArmWaitNoStall(t *testing.T) {
	t.Run("drawn arm free", func(t *testing.T) {
		f := newArmStallQueue(t, 2)
		a, b := f.add(t, 0, "src/a"), f.add(t, 1, "src/b")
		f.policy(t, true, map[string]string{a.ID: "S", b.ID: "O"})
		f.run(t, a)
		f.advance(6 * time.Minute)
		if got := listedEntry(t, f.s, f.task.ID, b.ID); got.Stall != nil || got.BlockReason != "" {
			t.Fatalf("claimable entry: %+v %q", got.Stall, got.BlockReason)
		}
	})
	t.Run("drawn arm held by a working team", func(t *testing.T) {
		f := newArmStallQueue(t, 2)
		a, b := f.add(t, 0, "src/a"), f.add(t, 1, "src/b")
		f.policy(t, true, map[string]string{a.ID: "S", b.ID: "S"})
		f.run(t, a)
		f.member(t, 0, "lead-a")
		f.advance(6 * time.Minute)
		assertNoStall(t, f.choresQueue, b.ID, "the drawn arm is leased by a working team")
		if got := listedEntry(t, f.s, f.task.ID, b.ID); !strings.HasPrefix(got.BlockReason, "Waiting for a free handler of arm S") {
			t.Fatalf("ordinary arm wait %q", got.BlockReason)
		}
	})
	t.Run("drawn arm gets a handler from its need", func(t *testing.T) {
		f := newArmStallQueue(t, 2)
		a, b := f.add(t, 0, "src/a"), f.add(t, 1, "src/b")
		f.policy(t, true, map[string]string{a.ID: "S", b.ID: "S"})
		f.run(t, a)
		f.advance(6 * time.Minute)
		assertNoStall(t, f.choresQueue, b.ID, "the runner adds a handler of the drawn arm")
		got := listedEntry(t, f.s, f.task.ID, b.ID)
		if got.HandlerNeed == nil || !got.HandlerNeed.Provision || got.BlockReason != "Waiting for a free handler of arm S: 1 of 1 leased, limit none; the runner is adding one" {
			t.Fatalf("need %+v reason %q", got.HandlerNeed, got.BlockReason)
		}
	})
	t.Run("drawn arm's handler is being reserved", func(t *testing.T) {
		f := newArmStallQueue(t, 2)
		a, b := f.add(t, 0, "src/a"), f.add(t, 1, "src/b")
		f.policy(t, true, map[string]string{a.ID: "S", b.ID: "S"})
		f.run(t, a)
		f.advance(6 * time.Minute)
		listed := listedEntry(t, f.s, f.task.ID, b.ID)
		agentID := api.NewID("agt")
		if _, err := f.s.TeamQueueAction(context.Background(), f.task.ID, api.TeamQueueRequest{RequestID: "reserve-b", Operation: "provision_handler", EntryID: b.ID, ExpectedRevision: listed.Revision,
			Host: "mini", HandlerAgentID: agentID, HandlerSpecRuntime: armS.Runtime, HandlerSpecModel: armS.Model, HandlerSpecReasoning: armS.Reasoning, HandlerSpecDigest: digestP}); err != nil {
			t.Fatal(err)
		}
		assertNoStall(t, f.choresQueue, b.ID, "a handler of the drawn arm is reserved")
		got := listedEntry(t, f.s, f.task.ID, b.ID)
		if got.HandlerNeed == nil || got.HandlerNeed.Provision || got.HandlerNeed.AgentID != agentID || got.BlockReason != "Waiting for a free handler of arm S: 1 of 1 leased, limit none; the runner is adding one" {
			t.Fatalf("need %+v reason %q", got.HandlerNeed, got.BlockReason)
		}
	})
	t.Run("every arm limited", func(t *testing.T) {
		f := newArmStallQueue(t, 2)
		a, b := f.add(t, 0, "src/a"), f.add(t, 1, "src/b")
		f.policy(t, true, map[string]string{a.ID: "S", b.ID: "S"})
		f.rotatingS(t)
		f.run(t, a)
		f.advance(6 * time.Minute)
		assertStall(t, f.choresQueue, b.ID, api.StallNothingRunning, a.ID)
		f.report(t, f.hS, api.AgentActivity{State: "working"})
		f.report(t, f.hS, api.AgentActivity{State: "idle", Reason: api.ClaudeRateLimitReason})
		f.report(t, f.hO, api.AgentActivity{State: "runtime_prompt", Prompt: usageLimitPrompt(api.RuntimePromptEscalated, f.s.now())})
		assertNoStall(t, f.choresQueue, b.ID, "every arm is at a provider limit")
		if got := listedEntry(t, f.s, f.task.ID, b.ID); got.BlockReason != "Every handler arm is at a provider limit" {
			t.Fatalf("limited reason %q", got.BlockReason)
		}
	})
}

// wi_dbb1529b84b724ae f1: an arm wait whose handler need asks a person to act
// (provisioning off, an offline handler of the arm, the project agent cap)
// behind a stuck entry holding the drawn arm's handler is stalled behind that
// entry, names it, and keeps the need's reason and fix after the arm, in the
// list and in the notice.
func TestQueueStallArmWaitNeedAsksPersonNamesHolder(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		setup      func(t *testing.T, f *armStallQueue)
	}{
		{"provisioning off", "automatic provisioning is off. Fix: tt team queue provision", func(t *testing.T, f *armStallQueue) {
			if _, err := f.s.TeamQueueAction(context.Background(), f.task.ID, api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: "set_handler_provision", HandlerProvision: api.HandlerProvisionOff}); err != nil {
				t.Fatal(err)
			}
		}},
		{"offline handler of the arm", "the limit already has its handlers, and ", func(t *testing.T, f *armStallQueue) {
			h := f.armHandler(t, "handler-s2", armS)
			if _, err := f.s.db.Exec(`UPDATE agents SET last_seen_at='' WHERE id=?`, h.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"project agent cap", "cannot add one: " + projectAgentCapPrefix, func(t *testing.T, f *armStallQueue) {
			// Three handlers are open and B is a bug team of five: the team
			// fits, the team and one more handler do not.
			f.s.MaxAgents = 8
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newArmStallQueue(t, 2)
			a, b := f.add(t, 0, "src/a"), f.add(t, 1, "src/b")
			f.policy(t, true, map[string]string{a.ID: "S", b.ID: "S"})
			a = f.run(t, a)
			if a.HandlerID != f.hS.ID {
				t.Fatalf("holder leased %s, want handler-s", a.HandlerID)
			}
			tc.setup(t, f)
			inside := listedEntry(t, f.s, f.task.ID, b.ID)
			if inside.Stall != nil || inside.HandlerNeed == nil || inside.HandlerNeed.Provision || !strings.Contains(inside.HandlerNeed.Reason, tc.want) ||
				inside.BlockReason != armWaitReason("S")+": "+inside.HandlerNeed.Reason {
				t.Fatalf("inside the grace: %+v need %+v %q", inside.Stall, inside.HandlerNeed, inside.BlockReason)
			}
			f.advance(6 * time.Minute)
			assertStall(t, f.choresQueue, b.ID, api.StallNothingRunning, a.ID, "tt team queue fail --task "+f.task.ID+" --entry "+a.ID)
			got := listedEntry(t, f.s, f.task.ID, b.ID)
			if got.HandlerNeed == nil || !strings.Contains(got.HandlerNeed.Reason, tc.want) {
				t.Fatalf("need %+v", got.HandlerNeed)
			}
			tail := armSWait + ": " + got.HandlerNeed.Reason
			if !strings.HasPrefix(got.BlockReason, "Stalled: "+a.ID+" ("+a.ItemID+"): ") || !strings.HasSuffix(got.BlockReason, tail) {
				t.Fatalf("stall reason %q, want the tail %q", got.BlockReason, tail)
			}
			text := stallNoticeText(t, f.s, f.task.ID, b.ID)
			if !strings.Contains(text, "Stalled: "+a.ID) || !strings.Contains(text, tail) {
				t.Fatalf("notice text %q", text)
			}
			if got.HandlerNeed.Fix != "" && !strings.Contains(text, "Fix: "+got.HandlerNeed.Fix) {
				t.Fatalf("notice lacks the need's fix %q: %q", got.HandlerNeed.Fix, text)
			}
		})
	}
}

// wi_376dfb3ee857ffcd a3: with no arm policy, and with a disabled one, the
// arm layouts stall exactly as before: a free handler of either arm frees the
// entry, and only a holder of the last free lease or an overlapping holder is
// named, without any arm text.
func TestQueueStallArmPolicyOffUnchanged(t *testing.T) {
	for _, mode := range []string{"none", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			f := newArmStallQueue(t, 3)
			a, b, c := f.add(t, 0, "src/a"), f.add(t, 1, "src/b"), f.add(t, 2, "src/a/child")
			if mode == "disabled" {
				f.policy(t, false, map[string]string{a.ID: "S", b.ID: "S", c.ID: "S"})
			}
			a = f.run(t, a)
			if a.HandlerID != f.hS.ID && a.HandlerID != f.hO.ID {
				t.Fatalf("holder leased %s, want an arm handler", a.HandlerID)
			}
			f.advance(6 * time.Minute)
			// Another handler is free: B can be claimed.
			if got := listedEntry(t, f.s, f.task.ID, b.ID); got.Stall != nil || got.BlockReason != "" {
				t.Fatalf("%s: free other handler %+v %q", mode, got.Stall, got.BlockReason)
			}
			// C overlaps the stuck holder: it is stalled behind it.
			assertStall(t, f.choresQueue, c.ID, api.StallNothingRunning, a.ID)
			// The other handler goes offline: B now waits on the stuck
			// holder's lease, named with no arm.
			other := f.hO.ID
			if a.HandlerID == other {
				other = f.hS.ID
			}
			if _, err := f.s.db.Exec(`UPDATE agents SET last_seen_at='' WHERE id=?`, other); err != nil {
				t.Fatal(err)
			}
			assertStall(t, f.choresQueue, b.ID, api.StallNothingRunning, a.ID)
			for _, id := range []string{b.ID, c.ID} {
				if got := listedEntry(t, f.s, f.task.ID, id); strings.Contains(got.BlockReason, "arm ") {
					t.Fatalf("%s: arm text without an enabled policy %q", mode, got.BlockReason)
				}
			}
		})
	}
}

// wi_376dfb3ee857ffcd a4 (wi_d85481431355ef78): a launch whose member
// registration the project agent cap refuses is held, not stalled: neither it
// nor the work behind it shows the nothing-running stall's fail fix. A grace
// after the cap has room again the stall returns.
func TestQueueStallCapHeldLaunchIsNotStalled(t *testing.T) {
	f := newChoresQueue(t, 2, 1, 0)
	advance := f.clock(t)
	ctx := context.Background()
	a, err := claimEntry(f.s, f.task, f.add(t, 0, "src"))
	if err != nil {
		t.Fatal(err)
	}
	b := f.add(t, 1, "src/b")
	plan, _ := capLaunchPlan(f.task, f.items[0], f.orders[0], a, 1)
	if a, err = f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "freeze", Operation: "freeze", EntryID: a.ID, ExpectedRevision: a.Revision, LaunchJSON: plan}); err != nil {
		t.Fatal(err)
	}
	filler := capFiller(t, f.s, f.task, "late-worker")
	f.s.MaxAgents = 2
	if _, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "attempt", Operation: "attempt", EntryID: a.ID, ExpectedRevision: a.Revision}); !errors.Is(err, api.ErrConflict) || err.Error() != "conflict: project agent cap: 2 open + 1 seats > 2" {
		t.Fatalf("attempt at a full project: %v", err)
	}
	advance(6 * time.Minute)
	for _, id := range []string{a.ID, b.ID} {
		got := listedEntry(t, f.s, f.task.ID, id)
		if got.Stall != nil || strings.Contains(got.BlockReason, "tt team queue fail") {
			t.Fatalf("cap-held %s: %+v %q", id, got.Stall, got.BlockReason)
		}
	}
	if got := listedEntry(t, f.s, f.task.ID, a.ID); got.State != "launching" || got.BlockReason != "Launch held at the project agent cap: 2 open + 1 seats > 2" {
		t.Fatalf("held launch %s %q", got.State, got.BlockReason)
	}
	if _, err := f.s.CloseAgent(ctx, filler.ID, f.by); err != nil {
		t.Fatal(err)
	}
	advance(6 * time.Minute)
	fix := "tt team queue fail --task " + f.task.ID + " --entry " + a.ID
	assertStall(t, f, a.ID, api.StallNothingRunning, a.ID, fix)
	assertStall(t, f, b.ID, api.StallNothingRunning, a.ID, fix)
}

// wi_dbb1529b84b724ae h1: a launch the project agent cap held is dated from
// the seat freed at the full cap after its last write, not that old write, so
// neither it nor the work behind it shows the nothing-running stall (or can
// post its notice) until a grace after the seat frees, which the runner's
// backed-off retry needs. Later closes and exits do not defer it further.
func TestQueueStallFreedCapHeldLaunchDatedFromSeat(t *testing.T) {
	// held sets up launch A refused at the cap by the given fillers, with B
	// queued behind it, past the grace.
	held := func(t *testing.T, fillers, max int) (*choresQueue, func(time.Duration), api.TeamQueueEntry, api.TeamQueueEntry, []api.Agent) {
		t.Helper()
		f := newChoresQueue(t, 2, 1, 0)
		advance := f.clock(t)
		ctx := context.Background()
		a, err := claimEntry(f.s, f.task, f.add(t, 0, "src"))
		if err != nil {
			t.Fatal(err)
		}
		b := f.add(t, 1, "src/b")
		plan, _ := capLaunchPlan(f.task, f.items[0], f.orders[0], a, 1)
		if a, err = f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "freeze", Operation: "freeze", EntryID: a.ID, ExpectedRevision: a.Revision, LaunchJSON: plan}); err != nil {
			t.Fatal(err)
		}
		var filled []api.Agent
		for i := 0; i < fillers; i++ {
			filled = append(filled, capFiller(t, f.s, f.task, fmt.Sprintf("late-worker-%d", i)))
		}
		f.s.MaxAgents = max
		want := fmt.Sprintf("conflict: project agent cap: %d open + 1 seats > %d", max, max)
		if _, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "attempt", Operation: "attempt", EntryID: a.ID, ExpectedRevision: a.Revision}); !errors.Is(err, api.ErrConflict) || err.Error() != want {
			t.Fatalf("attempt at a full project: %v", err)
		}
		advance(6 * time.Minute)
		for _, id := range []string{a.ID, b.ID} {
			if got := listedEntry(t, f.s, f.task.ID, id); got.Stall != nil {
				t.Fatalf("cap-held %s: %+v %q", id, got.Stall, got.BlockReason)
			}
		}
		return f, advance, a, b, filled
	}
	closeAgent := func(t *testing.T, f *choresQueue, id string) {
		t.Helper()
		if _, err := f.s.CloseAgent(context.Background(), id, f.by); err != nil {
			t.Fatal(err)
		}
	}
	// freed asserts no stall and a refused notice at the seat-free instant.
	freed := func(t *testing.T, f *choresQueue, a, b api.TeamQueueEntry) {
		t.Helper()
		for _, id := range []string{a.ID, b.ID} {
			if got := listedEntry(t, f.s, f.task.ID, id); got.Stall != nil || strings.Contains(got.BlockReason, "tt team queue fail") {
				t.Fatalf("seat just freed, %s: %+v %q", id, got.Stall, got.BlockReason)
			}
		}
		notice := api.TeamQueueStall{Cause: api.StallNothingRunning, BlockerEntryID: a.ID, BlockerRevision: a.Revision}
		if _, err := f.s.TeamQueueAction(context.Background(), f.task.ID, api.TeamQueueRequest{RequestID: notice.NoticeRequestID(a.ID), Operation: "stall_notice", EntryID: a.ID}); !errors.Is(err, api.ErrConflict) {
			t.Fatalf("notice at the seat freeing: %v", err)
		}
	}
	// stalled asserts the stall after the grace, dated from the seat freeing.
	stalled := func(t *testing.T, f *choresQueue, a, b api.TeamQueueEntry, seat time.Time) {
		t.Helper()
		fix := "tt team queue fail --task " + f.task.ID + " --entry " + a.ID
		for _, id := range []string{a.ID, b.ID} {
			assertStall(t, f, id, api.StallNothingRunning, a.ID, fix)
			if since := listedEntry(t, f.s, f.task.ID, id).Stall.Since; since != ts(seat) {
				t.Fatalf("%s since %s, want the seat freeing %s", id, since, ts(seat))
			}
		}
	}
	t.Run("one seat", func(t *testing.T) {
		f, advance, a, b, filled := held(t, 1, 2)
		seat := f.s.now()
		closeAgent(t, f, filled[0].ID)
		freed(t, f, a, b)
		advance(6 * time.Minute)
		stalled(t, f, a, b, seat)
	})
	t.Run("two seats", func(t *testing.T) {
		f, advance, a, b, filled := held(t, 2, 3)
		seat := f.s.now()
		closeAgent(t, f, filled[0].ID)
		closeAgent(t, f, filled[1].ID)
		freed(t, f, a, b)
		advance(6 * time.Minute)
		stalled(t, f, a, b, seat)
	})
	t.Run("later events do not defer", func(t *testing.T) {
		f, advance, a, b, filled := held(t, 2, 3)
		seat := f.s.now()
		closeAgent(t, f, filled[0].ID)
		freed(t, f, a, b)
		advance(3 * time.Minute)
		closeAgent(t, f, filled[0].ID)
		closeAgent(t, f, filled[1].ID)
		advance(3 * time.Minute)
		stalled(t, f, a, b, seat)
	})
}

// wi_dbb1529b84b724ae review b1 (#51624): a stuck launch the cap never held
// keeps a stall already shown, with its Since, when an unrelated agent in the
// project closes.
func TestQueueStallShownLaunchStallKeptAfterUnrelatedClose(t *testing.T) {
	f := newChoresQueue(t, 2, 1, 0)
	advance := f.clock(t)
	a, err := claimEntry(f.s, f.task, f.add(t, 0, "src"))
	if err != nil {
		t.Fatal(err)
	}
	b := f.add(t, 1, "src/b")
	other := capFiller(t, f.s, f.task, "unrelated-worker")
	advance(6 * time.Minute)
	fix := "tt team queue fail --task " + f.task.ID + " --entry " + a.ID
	assertStall(t, f, a.ID, api.StallNothingRunning, a.ID, fix)
	assertStall(t, f, b.ID, api.StallNothingRunning, a.ID, fix)
	shown := listedEntry(t, f.s, f.task.ID, a.ID).Stall.Since
	advance(10 * time.Minute)
	if _, err := f.s.CloseAgent(context.Background(), other.ID, f.by); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, b.ID} {
		assertStall(t, f, id, api.StallNothingRunning, a.ID, fix)
		if got := listedEntry(t, f.s, f.task.ID, id).Stall.Since; got != shown {
			t.Fatalf("%s since %s after an unrelated close, was %s", id, got, shown)
		}
	}
}

const moveHint = "; move it: tt team queue scope --task "

// wi_376dfb3ee857ffcd a5 (wi_0806c5126e184b8c): a stall behind an entry that
// only shares the checkout carries the move only when moving would free the
// queued entry: not in a serial project, with full slots, with no free handler
// of its drawn arm, or when the team does not fit under the project agent cap.
func TestQueueStallMoveHintOnlyWhenMoveFrees(t *testing.T) {
	// stuck sets up a stuck holder A in /main and B in /main with disjoint
	// ownership, after the grace.
	stuck := func(t *testing.T, f *choresQueue, advance func(time.Duration)) (api.TeamQueueEntry, api.TeamQueueEntry) {
		t.Helper()
		a := addCheckoutEntry(t, f.s, f.task, f.items[0], f.orders[0], "/main", "src/a")
		b := addCheckoutEntry(t, f.s, f.task, f.items[1], f.orders[1], "/main", "src/b")
		a = f.run(t, a)
		advance(6 * time.Minute)
		return a, b
	}
	hint := func(t *testing.T, f *choresQueue, b api.TeamQueueEntry) bool {
		t.Helper()
		return strings.Contains(listedEntry(t, f.s, f.task.ID, b.ID).BlockReason, moveHint+f.task.ID+" --entry "+b.ID+" --new-worktree")
	}
	t.Run("moving frees it", func(t *testing.T) {
		f := newChoresQueue(t, 2, 2, 0)
		a, b := stuck(t, f, f.clock(t))
		assertStall(t, f, b.ID, api.StallNothingRunning, a.ID)
		if !hint(t, f, b) {
			t.Fatalf("hint missing: %q", listedEntry(t, f.s, f.task.ID, b.ID).BlockReason)
		}
	})
	t.Run("serial project", func(t *testing.T) {
		f := newChoresQueue(t, 2, 2, 1)
		advance := f.clock(t)
		a := addCheckoutEntry(t, f.s, f.task, f.items[0], f.orders[0], "/main", "src/a")
		b := addCheckoutEntry(t, f.s, f.task, f.items[1], f.orders[1], "/main", "src/b")
		a = f.fail(t, f.run(t, a))
		advance(time.Minute)
		assertStall(t, f, b.ID, api.StallSerialHalted, a.ID)
		if hint(t, f, b) {
			t.Fatal("serial: the move frees nothing")
		}
	})
	t.Run("full slots", func(t *testing.T) {
		f := newChoresQueue(t, 3, 3, 2)
		advance := f.clock(t)
		a, b := stuck(t, f, advance)
		f.run(t, addCheckoutEntry(t, f.s, f.task, f.items[2], f.orders[2], "/other", "docs"))
		f.member(t, 2, "lead-c")
		assertStall(t, f, b.ID, api.StallNothingRunning, a.ID)
		if hint(t, f, b) {
			t.Fatal("full slots: the move frees nothing")
		}
	})
	t.Run("no free handler", func(t *testing.T) {
		f := newChoresQueue(t, 2, 1, 0)
		a, b := stuck(t, f, f.clock(t))
		assertStall(t, f, b.ID, api.StallNothingRunning, a.ID)
		if hint(t, f, b) {
			t.Fatal("no free handler: the move frees nothing")
		}
	})
	t.Run("no free handler of the drawn arm", func(t *testing.T) {
		f := newArmStallQueue(t, 2)
		a := addCheckoutEntry(t, f.s, f.task, f.items[0], f.orders[0], "/main", "src/a")
		b := addCheckoutEntry(t, f.s, f.task, f.items[1], f.orders[1], "/main", "src/b")
		f.policy(t, true, map[string]string{a.ID: "S", b.ID: "S"})
		a = f.run(t, a)
		f.advance(6 * time.Minute)
		assertStall(t, f.choresQueue, b.ID, api.StallNothingRunning, a.ID)
		if got := listedEntry(t, f.s, f.task.ID, b.ID); !strings.Contains(got.BlockReason, armSWait) || hint(t, f.choresQueue, b) {
			t.Fatalf("arm-held: %q", got.BlockReason)
		}
	})
	t.Run("project agent cap", func(t *testing.T) {
		f := newChoresQueue(t, 2, 2, 0)
		a, b := stuck(t, f, f.clock(t))
		filler := capFiller(t, f.s, f.task, "late-worker")
		// Two handlers and the filler are open; B is a bug team of five.
		f.s.MaxAgents = 7
		got := listedEntry(t, f.s, f.task.ID, b.ID)
		if got.Stall == nil || got.Stall.BlockerEntryID != a.ID || hint(t, f, b) {
			t.Fatalf("cap-held: %+v %q", got.Stall, got.BlockReason)
		}
		setAgentStatus(t, f.s, filler.ID, "closed")
		if !hint(t, f, b) {
			t.Fatalf("hint after a seat frees: %q", listedEntry(t, f.s, f.task.ID, b.ID).BlockReason)
		}
	})
}
