package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// hostScheduleFixture is a parallel queue with a lane policy, one live
// database handler, the owner helper and a pinned clock
// (wi_57c903a832b38b70).
type hostScheduleFixture struct {
	t       *testing.T
	s       *Store
	task    api.Task
	items   []api.WorkItem
	handler api.Agent
	helper  api.Agent
	clock   time.Time
}

func newHostScheduleFixture(t *testing.T, lanes bool) *hostScheduleFixture {
	t.Helper()
	s, task, items, _, handlers := uncappedFixture(t, 2, 1)
	if len(handlers) != 1 {
		t.Fatalf("fixture handlers %d", len(handlers))
	}
	f := &hostScheduleFixture{t: t, s: s, task: task, items: items, handler: handlers[0]}
	// The limit is admitted against the fixture's fresh host census, so the
	// clock is pinned only after it.
	setQueueLimit(t, s, task.ID, 4)
	if lanes {
		p := lanePolicy(3, 2)
		if err := setLanes(s, task.ID, &p); err != nil {
			t.Fatal(err)
		}
	}
	f.helper = *registerHelper(t, s, task.ID, helperRequest("schedule-helper"), api.Caller{Node: "fixture", User: "owner"}).Agent
	f.clock = s.now().UTC().Truncate(time.Second).Add(2 * time.Second)
	s.now = func() time.Time { return f.clock }
	return f
}

// act runs one schedule operation; who is "owner", "handler", "helper" or
// "lead".
func (f *hostScheduleFixture) act(who, op string, mutate func(*api.TeamQueueRequest)) error {
	req := api.TeamQueueRequest{RequestID: api.NewID("tqr"), Operation: op, Caller: api.Caller{Node: "fixture", User: "owner"}}
	switch who {
	case "handler":
		req.HandlerAgentID, req.HandlerRunID = f.handler.ID, f.handler.RunID
	case "helper":
		req.AgentID = f.helper.ID
	case "lead":
		req.LeadAgentID, req.LeadRunID = "agt_lead", "run_lead"
	}
	if mutate != nil {
		mutate(&req)
	}
	_, err := f.s.TeamQueueAction(context.Background(), f.task.ID, req)
	return err
}

func (f *hostScheduleFixture) set(who, reason string, minutes int) error {
	return f.act(who, "schedule_set", func(r *api.TeamQueueRequest) { r.ScheduleReason, r.ScheduleTTLMinutes = reason, minutes })
}

func (f *hostScheduleFixture) grant(who, op, item string) error {
	return f.act(who, op, func(r *api.TeamQueueRequest) { r.ScheduleItem, r.ScheduleReason = item, "turn for "+item })
}

func (f *hostScheduleFixture) lanes() api.TeamQueueLanes {
	f.t.Helper()
	list, _ := laneList(f.t, f.s, f.task.ID)
	if list.Lanes == nil {
		f.t.Fatal("no lanes listed")
	}
	return *list.Lanes
}

func TestHostScheduleNeedsALanePolicy(t *testing.T) {
	f := newHostScheduleFixture(t, false)
	if err := f.set("owner", "release turns", 60); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "a host schedule needs a lane policy") {
		t.Fatalf("schedule without lanes: %v", err)
	}
	if list, _ := laneList(t, f.s, f.task.ID); list.Lanes != nil {
		t.Fatalf("refusal saved a policy: %+v", list.Lanes)
	}
	// A first lanes save cannot bring its own schedule.
	p := lanePolicy(3, 2)
	p.HostSchedule = &api.TeamHostSchedule{Reason: "smuggled", SetBy: "agt_x", SetAt: ts(f.clock), ExpiresAt: ts(f.clock.Add(time.Hour))}
	if err := setLanes(f.s, f.task.ID, &p); err != nil {
		t.Fatal(err)
	}
	if l := f.lanes(); l.Policy.HostSchedule != nil || l.ScheduleState != "" {
		t.Fatalf("first lanes save stored a schedule: %q %+v", l.ScheduleState, l.Policy.HostSchedule)
	}
}

// Set computes the expiry from the hub clock and records who and why; the
// handler grants and the owner helper revokes; clear records who, when and
// why and drops the grants.
func TestHostScheduleSetGrantRevokeClearRecordsWhoAndWhy(t *testing.T) {
	f := newHostScheduleFixture(t, true)
	if err := f.set("handler", "release turns: team matrices start only on a handler grant", 90); err != nil {
		t.Fatal(err)
	}
	l := f.lanes()
	h := l.Policy.HostSchedule
	if l.ScheduleState != api.HostScheduleActive || h == nil {
		t.Fatalf("after set: state %q schedule %+v", l.ScheduleState, h)
	}
	if h.Reason != "release turns: team matrices start only on a handler grant" || h.SetBy != f.handler.ID || h.SetAt != ts(f.clock) || h.ExpiresAt != ts(f.clock.Add(90*time.Minute)) || len(h.Grants) != 0 {
		t.Fatalf("saved schedule %+v", h)
	}
	if l.Policy.UpdatedAt != ts(f.clock) {
		t.Fatalf("policy time %q", l.Policy.UpdatedAt)
	}
	f.clock = f.clock.Add(time.Minute)
	if err := f.grant("handler", "schedule_grant", f.items[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := f.grant("owner", "schedule_grant", f.items[1].ID); err != nil {
		t.Fatal(err)
	}
	h = f.lanes().Policy.HostSchedule
	if len(h.Grants) != 2 || h.Grants[0] != (api.TeamHostTurnGrant{ItemID: f.items[0].ID, By: f.handler.ID, At: ts(f.clock), Reason: "turn for " + f.items[0].ID}) || h.Grants[1].By != "owner" {
		t.Fatalf("grants %+v", h.Grants)
	}
	if h.Grant(f.items[0].ID) == nil || h.Grant("wi_other") != nil {
		t.Fatal("grant lookup")
	}
	// A repeated grant replaces the earlier one rather than adding a second.
	if err := f.grant("helper", "schedule_grant", f.items[0].ID); err != nil {
		t.Fatal(err)
	}
	if h = f.lanes().Policy.HostSchedule; len(h.Grants) != 2 || h.Grant(f.items[0].ID).By != f.helper.ID {
		t.Fatalf("regrant %+v", h.Grants)
	}
	if err := f.grant("helper", "schedule_revoke", f.items[0].ID); err != nil {
		t.Fatal(err)
	}
	if h = f.lanes().Policy.HostSchedule; len(h.Grants) != 1 || h.Grant(f.items[0].ID) != nil {
		t.Fatalf("after revoke %+v", h.Grants)
	}
	if err := f.grant("helper", "schedule_revoke", f.items[0].ID); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("revoking no grant: %v", err)
	}
	if err := f.grant("owner", "schedule_grant", "wi_"+strings.Repeat("0", 16)); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("granting an unknown item: %v", err)
	}
	f.clock = f.clock.Add(time.Minute)
	if err := f.act("helper", "schedule_clear", func(r *api.TeamQueueRequest) { r.ScheduleReason = "releases done" }); err != nil {
		t.Fatal(err)
	}
	l = f.lanes()
	h = l.Policy.HostSchedule
	if l.ScheduleState != api.HostScheduleCleared || h.ClearedBy != f.helper.ID || h.ClearedAt != ts(f.clock) || h.ClearReason != "releases done" || len(h.Grants) != 0 || h.SetBy != f.handler.ID {
		t.Fatalf("after clear: %q %+v", l.ScheduleState, h)
	}
	if err := f.grant("handler", "schedule_grant", f.items[0].ID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("grant after clear: %v", err)
	}
	if err := f.act("owner", "schedule_clear", func(r *api.TeamQueueRequest) { r.ScheduleReason = "again" }); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("clearing a cleared schedule: %v", err)
	}
	// Setting again replaces the cleared record.
	if err := f.set("owner", "night turns", 30); err != nil {
		t.Fatal(err)
	}
	if l = f.lanes(); l.ScheduleState != api.HostScheduleActive || l.Policy.HostSchedule.SetBy != "owner" || l.Policy.HostSchedule.ClearedAt != "" {
		t.Fatalf("set after clear: %q %+v", l.ScheduleState, l.Policy.HostSchedule)
	}
}

// An item lead, a closed handler and an agent that is not the owner helper
// are refused, and nothing is saved.
func TestHostScheduleRefusesLeadsClosedHandlersAndOtherAgents(t *testing.T) {
	f := newHostScheduleFixture(t, true)
	if err := f.set("lead", "lead's own schedule", 30); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "ask the database handler") {
		t.Fatalf("lead set: %v", err)
	}
	if err := f.act("", "schedule_set", func(r *api.TeamQueueRequest) {
		r.ScheduleReason, r.ScheduleTTLMinutes, r.AgentID = "impostor", 30, "agt_"+strings.Repeat("1", 16)
	}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("non-helper agent set: %v", err)
	}
	if err := f.act("handler", "schedule_set", func(r *api.TeamQueueRequest) {
		r.ScheduleReason, r.ScheduleTTLMinutes, r.AgentID = "both", 30, f.helper.ID
	}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("handler and helper together: %v", err)
	}
	if err := f.act("handler", "schedule_set", func(r *api.TeamQueueRequest) {
		r.ScheduleReason, r.ScheduleTTLMinutes, r.HandlerRunID = "stale run", 30, "run_stale"
	}); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("handler with another run: %v", err)
	}
	if _, err := f.s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentClosed, f.handler.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.set("handler", "closed handler", 30); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("closed handler set: %v", err)
	}
	if l := f.lanes(); l.Policy.HostSchedule != nil || l.ScheduleState != "" {
		t.Fatalf("refusals saved a schedule: %q %+v", l.ScheduleState, l.Policy.HostSchedule)
	}
	// The owner and the owner helper may still set it.
	if err := f.set("helper", "helper schedule", 30); err != nil {
		t.Fatal(err)
	}
	if l := f.lanes(); l.Policy.HostSchedule.SetBy != f.helper.ID {
		t.Fatalf("helper set by %q", l.Policy.HostSchedule.SetBy)
	}
	if err := f.grant("lead", "schedule_grant", f.items[0].ID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("lead grant: %v", err)
	}
	if err := f.act("lead", "schedule_clear", func(r *api.TeamQueueRequest) { r.ScheduleReason = "lead clears" }); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("lead clear: %v", err)
	}
}

func TestHostScheduleValidatesItsFields(t *testing.T) {
	f := newHostScheduleFixture(t, true)
	for name, req := range map[string]func(*api.TeamQueueRequest){
		"no reason":    func(r *api.TeamQueueRequest) { r.ScheduleTTLMinutes = 30 },
		"blank reason": func(r *api.TeamQueueRequest) { r.ScheduleReason, r.ScheduleTTLMinutes = "   ", 30 },
		"two lines":    func(r *api.TeamQueueRequest) { r.ScheduleReason, r.ScheduleTTLMinutes = "a\nb", 30 },
		"long reason": func(r *api.TeamQueueRequest) {
			r.ScheduleReason, r.ScheduleTTLMinutes = strings.Repeat("x", api.HostScheduleMaxReason+1), 30
		},
		"no expiry": func(r *api.TeamQueueRequest) { r.ScheduleReason = "turns" },
		"expiry over a day": func(r *api.TeamQueueRequest) {
			r.ScheduleReason, r.ScheduleTTLMinutes = "turns", api.HostScheduleMaxTTLMinutes+1
		},
		"an item": func(r *api.TeamQueueRequest) {
			r.ScheduleReason, r.ScheduleTTLMinutes, r.ScheduleItem = "turns", 30, f.items[0].ID
		},
	} {
		if err := f.act("owner", "schedule_set", req); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := f.grant("owner", "schedule_grant", f.items[0].ID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("grant with no schedule: %v", err)
	}
	if err := f.set("owner", "turns", api.HostScheduleMaxTTLMinutes); err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]func(*api.TeamQueueRequest){
		"no item":     func(r *api.TeamQueueRequest) {},
		"bad item":    func(r *api.TeamQueueRequest) { r.ScheduleItem = "tqe_x" },
		"with expiry": func(r *api.TeamQueueRequest) { r.ScheduleItem, r.ScheduleTTLMinutes = f.items[0].ID, 5 },
		"two lines":   func(r *api.TeamQueueRequest) { r.ScheduleItem, r.ScheduleReason = f.items[0].ID, "a\nb" },
	} {
		if err := f.act("owner", "schedule_grant", req); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("grant %s: %v", name, err)
		}
	}
	if err := f.act("owner", "schedule_clear", nil); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("clear without a reason: %v", err)
	}
	if err := f.act("owner", "schedule_clear", func(r *api.TeamQueueRequest) { r.ScheduleReason, r.ScheduleItem = "done", f.items[0].ID }); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("clear with an item: %v", err)
	}
}

// The schedule expires by the hub clock; an expired schedule takes no grant,
// may still be cleared, and no longer blocks lanes --off.
func TestHostScheduleExpiresByTheHubClock(t *testing.T) {
	f := newHostScheduleFixture(t, true)
	if err := f.set("owner", "turns", 10); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(10*time.Minute - time.Nanosecond)
	if l := f.lanes(); l.ScheduleState != api.HostScheduleActive {
		t.Fatalf("just before expiry: %q", l.ScheduleState)
	}
	f.clock = f.clock.Add(time.Nanosecond)
	if l := f.lanes(); l.ScheduleState != api.HostScheduleExpired {
		t.Fatalf("at expiry: %q", l.ScheduleState)
	}
	if err := f.grant("handler", "schedule_grant", f.items[0].ID); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("grant after expiry: %v", err)
	}
	if err := f.act("handler", "schedule_clear", func(r *api.TeamQueueRequest) { r.ScheduleReason = "tidy" }); err != nil {
		t.Fatalf("clearing an expired schedule: %v", err)
	}
	if l := f.lanes(); l.ScheduleState != api.HostScheduleCleared || l.Policy.HostSchedule.ClearedBy != f.handler.ID {
		t.Fatalf("after clearing: %q %+v", l.ScheduleState, l.Policy.HostSchedule)
	}
	if err := f.set("owner", "turns again", 10); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(11 * time.Minute)
	if err := setLanes(f.s, f.task.ID, nil); err != nil {
		t.Fatalf("lanes off over an expired schedule: %v", err)
	}
}

// A lanes save keeps the saved schedule, an identical resend keeps the
// policy time, and lanes --off is refused while the schedule is active.
func TestHostScheduleSurvivesLanesSavesAndBlocksLanesOff(t *testing.T) {
	f := newHostScheduleFixture(t, true)
	if err := f.set("owner", "turns", 60); err != nil {
		t.Fatal(err)
	}
	if err := f.grant("owner", "schedule_grant", f.items[0].ID); err != nil {
		t.Fatal(err)
	}
	before := f.lanes().Policy
	f.clock = f.clock.Add(time.Minute)
	p := lanePolicy(3, 2)
	if err := setLanes(f.s, f.task.ID, &p); err != nil {
		t.Fatal(err)
	}
	after := f.lanes().Policy
	if after.UpdatedAt != before.UpdatedAt || !after.HostSchedule.Equal(before.HostSchedule) {
		t.Fatalf("identical resend changed the policy: %+v to %+v", before, after)
	}
	// A changed lanes save keeps the schedule, grants included, and a
	// schedule sent inside the lanes policy is ignored.
	changed := lanePolicy(4, 2)
	changed.HostSchedule = &api.TeamHostSchedule{Reason: "smuggled", SetBy: "agt_x", SetAt: ts(f.clock), ExpiresAt: ts(f.clock.Add(time.Hour))}
	if err := setLanes(f.s, f.task.ID, &changed); err != nil {
		t.Fatal(err)
	}
	l := f.lanes()
	if l.Policy.Day != 4 || l.Policy.UpdatedAt != ts(f.clock) || !l.Policy.HostSchedule.Equal(before.HostSchedule) || l.ScheduleState != api.HostScheduleActive {
		t.Fatalf("changed lanes save: %+v state %q", l.Policy, l.ScheduleState)
	}
	err := setLanes(f.s, f.task.ID, nil)
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "tt team queue schedule clear --task "+f.task.ID) {
		t.Fatalf("lanes off under an active schedule: %v", err)
	}
	if l := f.lanes(); l.Policy.HostSchedule == nil {
		t.Fatal("refused lanes off dropped the schedule")
	}
	if err := f.act("owner", "schedule_clear", func(r *api.TeamQueueRequest) { r.ScheduleReason = "done" }); err != nil {
		t.Fatal(err)
	}
	if err := setLanes(f.s, f.task.ID, nil); err != nil {
		t.Fatalf("lanes off after clear: %v", err)
	}
	if list, _ := laneList(t, f.s, f.task.ID); list.Lanes != nil {
		t.Fatalf("lanes still listed: %+v", list.Lanes)
	}
	// The schedule needs the policy back before it can be set again.
	if err := f.set("owner", "turns", 60); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("set after lanes off: %v", err)
	}
}

// Without a schedule the lanes JSON is byte-identical to before: no
// hostSchedule or scheduleState key.
func TestHostScheduleAbsentKeepsLanesJSON(t *testing.T) {
	f := newHostScheduleFixture(t, true)
	var raw string
	if err := f.s.db.QueryRow(`SELECT lane_policy_json FROM team_queue_settings WHERE task_id=?`, f.task.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "hostSchedule") {
		t.Fatalf("stored policy names a schedule: %s", raw)
	}
	if l := f.lanes(); l.ScheduleState != "" || l.Policy.HostSchedule != nil {
		t.Fatalf("lanes without a schedule: %+v", l)
	}
}
