package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Handler floor tests use an isolated SQLite hub per test and synthetic
// agents only (wi_96295d2375d72618, order #17042).

type floorFixture struct {
	s    *Store
	ctx  context.Context
	by   api.Caller
	task api.Task
}

func newFloorFixture(t *testing.T) floorFixture {
	t.Helper()
	s, ctx, by := workItemStore(t)
	task, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Synthetic floor project"}, by)
	if err != nil {
		t.Fatal(err)
	}
	return floorFixture{s: s, ctx: ctx, by: by, task: task}
}

// agent registers a started agent; a role makes it a database handler.
func (f floorFixture) agent(t *testing.T, name, role string) api.Agent {
	t.Helper()
	req := api.AddAgentRequest{Name: name, Host: "fixture", Session: name, Runtime: "codex"}
	if role != "" {
		req.Role, req.AgentID = role, api.NewID("agt")
	}
	a, err := f.s.AddAgent(f.ctx, f.task.ID, req, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted}, f.by); err != nil {
		t.Fatal(err)
	}
	return f.get(t, a.ID)
}

func (f floorFixture) handler(t *testing.T, name string) api.Agent {
	return f.agent(t, name, api.AgentRoleDatabaseHandler)
}

func (f floorFixture) get(t *testing.T, id string) api.Agent {
	t.Helper()
	a, err := f.s.GetAgent(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f floorFixture) status(t *testing.T, id, status string) (api.Agent, error) {
	t.Helper()
	return f.s.UpdateAgent(f.ctx, id, api.UpdateAgentRequest{Status: &status}, f.by)
}

func (f floorFixture) event(t *testing.T, a api.Agent, kind string) error {
	t.Helper()
	_, err := f.s.PostEvent(f.ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: kind}, f.by)
	return err
}

// floorNotices returns the project's no-handler escalations, oldest first.
func floorNotices(t *testing.T, s *Store, task string) []api.Message {
	t.Helper()
	rows, err := s.db.Query(`SELECT seq,to_agent,text,envelope FROM messages WHERE task_id=? AND envelope<>'' ORDER BY seq`, task)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []api.Message
	for rows.Next() {
		var m api.Message
		var raw string
		if err := rows.Scan(&m.Seq, &m.To, &m.Text, &raw); err != nil {
			t.Fatal(err)
		}
		var env api.Envelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Fatal(err)
		}
		if env.Refs["cause"] == "no-database-handler" {
			m.Envelope = &env
			out = append(out, m)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func closedEvents(t *testing.T, s *Store, task, agent string) int {
	return countRows(t, s, `SELECT count(*) FROM events WHERE task_id=? AND agent_id=? AND kind=?`, task, agent, api.EventClosed)
}

func wantFloorRefusal(t *testing.T, path string, err error, text string) {
	t.Helper()
	if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), text) {
		t.Fatalf("%s: want ErrConflict containing %q, got %v", path, text, err)
	}
}

// floorRotate commits a rotation away from f.old to a started successor and
// returns the successor, now the recorded primary.
func floorRotate(t *testing.T, f *rotationFixture, key string) api.Agent {
	t.Helper()
	ctx := context.Background()
	r, err := f.prepare(t, key+"-prepare", api.Agent{}, "")
	if err != nil {
		t.Fatal(err)
	}
	successor, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: r.SuccessorAgentID, Role: api.AgentRoleDatabaseHandler, Name: r.SuccessorName, Host: "mini", Session: "tt-handler-" + key, Runtime: f.old.Runtime, Cwd: f.old.Cwd}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: successor.ID, RunID: successor.RunID, Kind: api.EventStarted}, f.by); err != nil {
		t.Fatal(err)
	}
	if _, err = f.commit(r, key+"-commit"); err != nil {
		t.Fatal(err)
	}
	task, err := f.s.GetTask(ctx, f.task.ID)
	if err != nil || task.PrimaryHandlerID != successor.ID {
		t.Fatalf("primary after rotation: %+v %v", task, err)
	}
	successor, err = f.s.GetAgent(ctx, successor.ID)
	if err != nil {
		t.Fatal(err)
	}
	return successor
}

// a1: every hub path that retires or closes the last available handler is
// refused and changes nothing.
func TestHandlerFloorRefusesLastAvailableHandlerOnEveryPath(t *testing.T) {
	f := newFloorFixture(t)
	h := f.handler(t, "db-handler")
	worker := f.agent(t, "builder", "")
	events := countRows(t, f.s, `SELECT count(*) FROM events WHERE task_id=?`, f.task.ID)
	messages := countRows(t, f.s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID)
	const text = "last available database handler"
	_, err := f.status(t, h.ID, api.AgentRetired)
	wantFloorRefusal(t, "UpdateAgent retired", err, text)
	_, err = f.status(t, h.ID, api.AgentClosed)
	wantFloorRefusal(t, "UpdateAgent closed", err, text)
	_, err = f.s.CloseAgentRun(f.ctx, h.ID, h.RunID, f.by)
	wantFloorRefusal(t, "CloseAgentRun", err, text)
	wantFloorRefusal(t, "PostEvent closed", f.event(t, h, api.EventClosed), text)
	if got := f.get(t, h.ID); got.Status != api.AgentRunning {
		t.Fatalf("refused handler changed status: %s", got.Status)
	}
	if got := countRows(t, f.s, `SELECT count(*) FROM events WHERE task_id=?`, f.task.ID); got != events {
		t.Fatalf("refusals wrote events: %d -> %d", events, got)
	}
	if got := countRows(t, f.s, `SELECT count(*) FROM messages WHERE task_id=?`, f.task.ID); got != messages {
		t.Fatalf("refusals wrote messages: %d -> %d", messages, got)
	}
	// A handler still starting counts as available; it is the last one too.
	f2 := newFloorFixture(t)
	starting, err := f2.s.AddAgent(f2.ctx, f2.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Role: api.AgentRoleDatabaseHandler, Name: "db-starting", Host: "fixture", Session: "db-starting", Runtime: "codex"}, f2.by)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f2.s.CloseAgentRun(f2.ctx, starting.ID, starting.RunID, f2.by)
	wantFloorRefusal(t, "CloseAgentRun starting", err, text)
	// The floor is per project and per role: the worker still closes.
	if _, err = f.status(t, worker.ID, api.AgentClosed); err != nil {
		t.Fatalf("worker close: %v", err)
	}
}

// a2: the recorded primary is refused while another handler is available,
// unless a rotation away from it is prepared.
func TestHandlerFloorRefusesPrimaryUntilRotationIsPrepared(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	s := f.s
	primary := floorRotate(t, f, "floor")
	other := f.agent(t, "db-handler-other", api.AgentRoleDatabaseHandler)
	retired, closed := api.AgentRetired, api.AgentClosed
	const text = "is the project's primary database handler"
	_, err := s.UpdateAgent(ctx, primary.ID, api.UpdateAgentRequest{Status: &retired}, f.by)
	wantFloorRefusal(t, "UpdateAgent retired primary", err, primary.Name+" "+text)
	_, err = s.UpdateAgent(ctx, primary.ID, api.UpdateAgentRequest{Status: &closed}, f.by)
	wantFloorRefusal(t, "UpdateAgent closed primary", err, primary.Name+" "+text)
	_, err = s.CloseAgentRun(ctx, primary.ID, primary.RunID, f.by)
	wantFloorRefusal(t, "CloseAgentRun primary", err, primary.Name+" "+text)
	if got, _ := s.GetAgent(ctx, primary.ID); got.Status != primary.Status {
		t.Fatalf("refused primary changed status: %s -> %s", primary.Status, got.Status)
	}
	if _, err = s.UpdateAgent(ctx, other.ID, api.UpdateAgentRequest{Status: &retired}, f.by); err != nil {
		t.Fatalf("retire the non-primary handler: %v", err)
	}
	// A retired primary stays protected against close.
	f.old = primary
	f.idle(t, primary)
	if _, err = f.prepare(t, "floor-away", api.Agent{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpdateAgent(ctx, primary.ID, api.UpdateAgentRequest{Status: &retired}, f.by); err != nil {
		t.Fatalf("retire the primary with a prepared successor: %v", err)
	}
	if got, _ := s.GetAgent(ctx, primary.ID); got.Status != api.AgentRetired {
		t.Fatalf("primary status: %s", got.Status)
	}
}

// A retired primary without a ready successor cannot be closed either.
func TestHandlerFloorRefusesClosingRetiredPrimary(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	primary := floorRotate(t, f, "floor")
	f.agent(t, "db-handler-other", api.AgentRoleDatabaseHandler)
	if _, err := f.s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentRetired, primary.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.CloseAgentRun(ctx, primary.ID, primary.RunID, f.by)
	wantFloorRefusal(t, "CloseAgentRun retired primary", err, "primary database handler")
}

// a3: a retired non-primary handler closes while another is available, and
// non-handler agents are unaffected.
func TestHandlerFloorAllowsRetiredNonPrimaryClose(t *testing.T) {
	for _, path := range []string{"UpdateAgent", "CloseAgentRun"} {
		t.Run(path, func(t *testing.T) {
			f := newFloorFixture(t)
			keep := f.handler(t, "db-handler")
			r := f.handler(t, "db-handler-2")
			if _, err := f.status(t, r.ID, api.AgentRetired); err != nil {
				t.Fatalf("retire a non-primary handler while another is available: %v", err)
			}
			var err error
			if path == "UpdateAgent" {
				_, err = f.status(t, r.ID, api.AgentClosed)
			} else {
				_, err = f.s.CloseAgentRun(f.ctx, r.ID, r.RunID, f.by)
			}
			if err != nil {
				t.Fatalf("close the retired handler: %v", err)
			}
			if got := f.get(t, r.ID); got.Status != api.AgentClosed {
				t.Fatalf("retired handler status: %s", got.Status)
			}
			if n := closedEvents(t, f.s, f.task.ID, r.ID); n != 1 {
				t.Fatalf("closed events: %d", n)
			}
			if got := f.get(t, keep.ID); got.Status != api.AgentRunning {
				t.Fatalf("remaining handler status: %s", got.Status)
			}
			if n := len(floorNotices(t, f.s, f.task.ID)); n != 0 {
				t.Fatalf("notices: %d", n)
			}
		})
	}
	f := newFloorFixture(t)
	h := f.handler(t, "db-handler")
	retiring, closing, deleting, exiting := f.agent(t, "worker-1", ""), f.agent(t, "worker-2", ""), f.agent(t, "worker-3", ""), f.agent(t, "worker-4", "")
	if _, err := f.status(t, retiring.ID, api.AgentRetired); err != nil {
		t.Fatal(err)
	}
	if _, err := f.status(t, closing.ID, api.AgentClosed); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CloseAgentRun(f.ctx, deleting.ID, deleting.RunID, f.by); err != nil {
		t.Fatal(err)
	}
	if err := f.event(t, exiting, api.EventExited); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{retiring.ID: api.AgentRetired, closing.ID: api.AgentClosed, deleting.ID: api.AgentClosed, exiting.ID: api.AgentExited, h.ID: api.AgentRunning} {
		if got := f.get(t, id); got.Status != want {
			t.Fatalf("%s: status %s, want %s", got.Name, got.Status, want)
		}
	}
	if n := len(floorNotices(t, f.s, f.task.ID)); n != 0 {
		t.Fatalf("notices: %d", n)
	}
}

// a4: an exit is never refused, and each drop to zero available handlers
// posts exactly one board-wide owner notice.
func TestHandlerFloorEscalatesOncePerZeroCrossing(t *testing.T) {
	f := newFloorFixture(t)
	h := f.handler(t, "db-handler")
	spare := f.handler(t, "db-handler-spare")
	if _, err := f.status(t, spare.ID, api.AgentRetired); err != nil {
		t.Fatal(err)
	}
	if n := len(floorNotices(t, f.s, f.task.ID)); n != 0 {
		t.Fatalf("notices before the exit: %d", n)
	}
	if err := f.event(t, h, api.EventExited); err != nil {
		t.Fatalf("the last handler's exit must be recorded: %v", err)
	}
	if got := f.get(t, h.ID); got.Status != api.AgentExited {
		t.Fatalf("status after exit: %s", got.Status)
	}
	notices := floorNotices(t, f.s, f.task.ID)
	if len(notices) != 1 {
		t.Fatalf("notices after the exit: %d", len(notices))
	}
	n := notices[0]
	if n.To != "" || n.Envelope.Kind != api.EnvelopeKindNotice || n.Envelope.Refs["escalation"] != "owner" || n.Envelope.Refs["task"] != f.task.ID || n.Envelope.Refs["agent"] != h.ID {
		t.Fatalf("notice must be a board-wide owner escalation: to=%q %+v", n.To, n.Envelope)
	}
	if !strings.Contains(n.Text, f.task.Name) || !strings.Contains(n.Text, f.task.ID) || !strings.Contains(n.Text, h.Name) {
		t.Fatalf("notice text must name the project and handler: %s", n.Text)
	}
	// While at zero, further transitions add nothing.
	if _, err := f.status(t, h.ID, api.AgentClosed); err != nil {
		t.Fatalf("close the exited handler: %v", err)
	}
	if _, err := f.s.CloseAgentRun(f.ctx, spare.ID, spare.RunID, f.by); err != nil {
		t.Fatalf("close the retired spare: %v", err)
	}
	if err := f.event(t, h, api.EventExited); !errors.Is(err, api.ErrClosed) {
		t.Fatalf("replayed exit: %v", err)
	}
	if got := len(floorNotices(t, f.s, f.task.ID)); got != 1 {
		t.Fatalf("notices while at zero: %d", got)
	}
	// A new handler comes and goes: exactly one more notice.
	next := f.handler(t, "db-handler-next")
	if err := f.event(t, next, api.EventExited); err != nil {
		t.Fatal(err)
	}
	notices = floorNotices(t, f.s, f.task.ID)
	if len(notices) != 2 || notices[1].Envelope.Refs["agent"] != next.ID {
		t.Fatalf("second zero crossing: %+v", notices)
	}
}

// a5: project pause, task close and a committed rotation are exempt.
func TestHandlerFloorExemptsPauseTaskCloseAndRotation(t *testing.T) {
	t.Run("pause", func(t *testing.T) {
		f := newPauseFixture(t, false)
		if _, err := f.s.PauseProject(f.ctx, f.task.ID, f.pauseRequest("floor-pause"), f.by); err != nil {
			t.Fatalf("pause with one handler: %v", err)
		}
		if got, _ := f.s.GetAgent(f.ctx, f.agents[1].ID); got.Role != api.AgentRoleDatabaseHandler || got.Status != api.AgentClosed {
			t.Fatalf("paused handler: %+v", got)
		}
		if n := len(floorNotices(t, f.s, f.task.ID)); n != 0 {
			t.Fatalf("notices: %d", n)
		}
	})
	t.Run("task close", func(t *testing.T) {
		f := newFloorFixture(t)
		h := f.handler(t, "db-handler")
		w := f.agent(t, "builder", "")
		task, err := f.s.CloseTask(f.ctx, f.task.ID, f.by)
		if err != nil || task.Status != api.TaskClosed {
			t.Fatalf("close task with one handler: %+v %v", task, err)
		}
		for _, a := range []api.Agent{h, w} {
			if got := f.get(t, a.ID); got.Status != api.AgentClosed {
				t.Fatalf("%s status: %s", got.Name, got.Status)
			}
		}
		if n := len(floorNotices(t, f.s, f.task.ID)); n != 0 {
			t.Fatalf("notices: %d", n)
		}
	})
	t.Run("rotation", func(t *testing.T) {
		f := newRotationFixture(t)
		old := f.old
		floorRotate(t, f, "floor")
		if got, _ := f.s.GetAgent(context.Background(), old.ID); got.Status != api.AgentClosed {
			t.Fatalf("rotated handler status: %s", got.Status)
		}
		if n := len(floorNotices(t, f.s, f.task.ID)); n != 0 {
			t.Fatalf("notices: %d", n)
		}
	})
}
