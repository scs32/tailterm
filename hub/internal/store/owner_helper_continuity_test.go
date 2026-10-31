package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The owner helper's continuity record (wi_32b39bab910066b2, order #52620):
// a hub-held, revisioned free-text record any session can read.

var continuityBy = api.Caller{Node: "fixture", User: "owner"}

func continuityStore(t *testing.T) (*Store, api.Task, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "continuity.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	fixed := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	task, err := s.CreateTask(context.Background(), api.CreateTaskRequest{Name: "Continuity fixture", Orchestrator: "lead"}, continuityBy)
	if err != nil {
		t.Fatal(err)
	}
	return s, task, path
}

func saveContinuity(s *Store, task, key string, expected int64, body string, who api.Agent) (api.OwnerHelperContinuity, error) {
	return s.SaveOwnerHelperContinuity(context.Background(), task, api.SaveOwnerHelperContinuityRequest{ExpectedRevision: expected, Body: body, RequestID: key, AgentID: who.ID, RunID: who.RunID}, continuityBy)
}

// a1: revisions are 1..n; the latest and revision N carry the body; the
// revision list carries none.
func TestOwnerHelperContinuitySaveAndRead(t *testing.T) {
	s, task, _ := continuityStore(t)
	ctx := context.Background()
	if _, err := s.OwnerHelperContinuity(ctx, task.ID, 0); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("read before any save: %v", err)
	}
	if list, err := s.OwnerHelperContinuityRevisions(ctx, task.ID); err != nil || len(list) != 0 {
		t.Fatalf("revisions before any save: %+v %v", list, err)
	}
	helper := registerHelper(t, s, task.ID, helperRequest("reg-1"), continuityBy).Agent
	bodies := []string{"## State\nsynthetic line one\n", "## State\nsynthetic line two\n", "## State\nsynthetic line three — ünïcode\n"}
	var saved []api.OwnerHelperContinuity
	for i, body := range bodies {
		who := *helper
		if i == 1 {
			who = api.Agent{} // the owner, by hand
		}
		r, err := saveContinuity(s, task.ID, "cont-"+string(rune('a'+i)), int64(i), body, who)
		if err != nil || r.Revision != int64(i+1) || r.Body != body || r.Bytes != len(body) || r.Digest != summaryDigest(body) || r.AgentID != who.ID || r.RunID != who.RunID || r.CreatedBy != continuityBy {
			t.Fatalf("save %d: %+v %v", i+1, r, err)
		}
		saved = append(saved, r)
	}
	latest, err := s.OwnerHelperContinuity(ctx, task.ID, 0)
	if err != nil || latest.Revision != 3 || latest.Body != bodies[2] || latest.AgentID != helper.ID || latest.RunID != helper.RunID || !latest.CreatedAt.Equal(saved[2].CreatedAt) {
		t.Fatalf("latest: %+v %v", latest, err)
	}
	for i, body := range bodies {
		got, err := s.OwnerHelperContinuity(ctx, task.ID, int64(i+1))
		if err != nil || got.Revision != int64(i+1) || got.Body != body || got.Digest != saved[i].Digest {
			t.Fatalf("revision %d: %+v %v", i+1, got, err)
		}
	}
	if _, err := s.OwnerHelperContinuity(ctx, task.ID, 4); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("revision past the latest: %v", err)
	}
	list, err := s.OwnerHelperContinuityRevisions(ctx, task.ID)
	if err != nil || len(list) != 3 {
		t.Fatalf("revisions: %+v %v", list, err)
	}
	for i, r := range list {
		if r.Revision != int64(i+1) || r.Body != "" || r.Bytes != len(bodies[i]) || r.Digest != saved[i].Digest {
			t.Fatalf("revision list entry %d: %+v", i, r)
		}
	}
	// Each save is one event, with no body in it.
	events, err := s.ListEvents(ctx, task.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Text == "Owner helper continuity saved" {
			n++
			if raw, _ := json.Marshal(e.Data); strings.Contains(string(raw), "synthetic line") {
				t.Fatalf("the event carries the body: %s", e.Data)
			}
		}
	}
	if n != 3 {
		t.Fatalf("%d continuity events; want 3", n)
	}
	// Another project's record is separate.
	other, err := s.CreateTask(ctx, api.CreateTaskRequest{Name: "Other"}, continuityBy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OwnerHelperContinuity(ctx, other.ID, 0); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("other project: %v", err)
	}
	if _, err := s.OwnerHelperContinuity(ctx, "tsk_0123456789abcdef", 0); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

// a1: a stale expected revision conflicts; the same request ID replays with
// the same input and conflicts with different input.
func TestOwnerHelperContinuityRevisionReplay(t *testing.T) {
	s, task, _ := continuityStore(t)
	owner := api.Agent{}
	r1, err := saveContinuity(s, task.ID, "cont-1", 0, "first", owner)
	if err != nil || r1.Revision != 1 {
		t.Fatalf("first: %+v %v", r1, err)
	}
	if _, err := saveContinuity(s, task.ID, "cont-stale", 0, "stale", owner); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "revision 1") {
		t.Fatalf("stale revision: %v", err)
	}
	if _, err := saveContinuity(s, task.ID, "cont-ahead", 5, "ahead", owner); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("revision ahead: %v", err)
	}
	if replay, err := saveContinuity(s, task.ID, "cont-1", 0, "first", owner); err != nil || replay.Revision != 1 || replay.Digest != r1.Digest || !replay.CreatedAt.Equal(r1.CreatedAt) {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	if _, err := saveContinuity(s, task.ID, "cont-1", 0, "changed", owner); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed body under a used request ID: %v", err)
	}
	if _, err := saveContinuity(s, task.ID, "cont-1", 1, "first", owner); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("changed revision under a used request ID: %v", err)
	}
	if list, err := s.OwnerHelperContinuityRevisions(context.Background(), task.ID); err != nil || len(list) != 1 {
		t.Fatalf("refusals saved something: %+v %v", list, err)
	}
	if r2, err := saveContinuity(s, task.ID, "cont-2", 1, "second", owner); err != nil || r2.Revision != 2 {
		t.Fatalf("second: %+v %v", r2, err)
	}
}

// a1: the owner and the current helper's exact run save; a replaced run,
// another agent, and a retired, exited or closed helper are refused.
func TestOwnerHelperContinuityWriter(t *testing.T) {
	s, task, _ := continuityStore(t)
	ctx := context.Background()
	rev := int64(0)
	mustSave := func(key string, who api.Agent) {
		t.Helper()
		r, err := saveContinuity(s, task.ID, key, rev, "body "+key, who)
		if err != nil || r.Revision != rev+1 {
			t.Fatalf("%s: %+v %v", key, r, err)
		}
		rev = r.Revision
	}
	refused := func(key string, who api.Agent, want string) {
		t.Helper()
		_, err := saveContinuity(s, task.ID, key, rev, "body "+key, who)
		if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v; want a conflict naming %q", key, err, want)
		}
	}
	// No helper yet: only the owner may save.
	refused("no-helper", api.Agent{ID: api.NewID("agt"), RunID: api.NewID("run")}, "is not the project's owner helper")
	mustSave("owner", api.Agent{})
	first := *registerHelper(t, s, task.ID, helperRequest("reg-1"), continuityBy).Agent
	mustSave("helper-first", first)
	refused("agent-only", api.Agent{ID: first.ID}, "give both the agent and its run")
	refused("run-only", api.Agent{RunID: first.RunID}, "give both the agent and its run")
	worker, err := s.AddAgent(ctx, task.ID, api.AddAgentRequest{Name: "worker", Host: "mini", Session: "worker", Runtime: "claude"}, continuityBy)
	if err != nil {
		t.Fatal(err)
	}
	refused("worker", worker, "is not the project's owner helper")
	// Registering again replaces the run: the previous run is refused.
	second := *registerHelper(t, s, task.ID, helperRequest("reg-2"), continuityBy).Agent
	if second.ID != first.ID || second.RunID == first.RunID {
		t.Fatalf("re-registration %+v", second)
	}
	refused("previous-run", first, "was replaced")
	mustSave("helper-second", second)
	setAgentStatus(t, s, second.ID, api.AgentRetired)
	refused("retired", second, "the owner helper is retired")
	setAgentStatus(t, s, second.ID, api.AgentRunning)
	if _, err := s.PostEvent(ctx, task.ID, api.PostEventRequest{Kind: api.EventExited, AgentID: second.ID, RunID: second.RunID}, continuityBy); err != nil {
		t.Fatal(err)
	}
	refused("exited", second, "the owner helper is exited")
	third := *registerHelper(t, s, task.ID, helperRequest("reg-3"), continuityBy).Agent
	mustSave("helper-third", third)
	if _, err := s.CloseAgentRun(ctx, third.ID, third.RunID, continuityBy); err != nil {
		t.Fatal(err)
	}
	refused("closed", third, "is not the project's owner helper")
	// The owner still may.
	mustSave("owner-after", api.Agent{})
	list, err := s.OwnerHelperContinuityRevisions(ctx, task.ID)
	if err != nil || int64(len(list)) != rev {
		t.Fatalf("revisions %+v %v; want %d", list, err, rev)
	}
	wantBy := []string{"", first.RunID, second.RunID, third.RunID, ""}
	for i, r := range list {
		if r.RunID != wantBy[i] {
			t.Fatalf("revision %d saved by run %q; want %q", r.Revision, r.RunID, wantBy[i])
		}
	}
}

// a1: body limits and the closed-project refusal.
func TestOwnerHelperContinuityLimits(t *testing.T) {
	s, task, _ := continuityStore(t)
	ctx := context.Background()
	owner := api.Agent{}
	for name, body := range map[string]string{
		"empty":     "",
		"blank":     " \n\t ",
		"oversize":  strings.Repeat("x", api.MaxOwnerHelperContinuityLen+1),
		"bad utf-8": "synthetic \xff line",
		"nul":       "synthetic \x00 line",
	} {
		if _, err := saveContinuity(s, task.ID, "cont-"+strings.ReplaceAll(name, " ", "-"), 0, body, owner); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s: %v; want invalid", name, err)
		}
	}
	for name, req := range map[string]api.SaveOwnerHelperContinuityRequest{
		"no request id":     {Body: "ok"},
		"negative revision": {ExpectedRevision: -1, Body: "ok", RequestID: "cont-neg"},
	} {
		if _, err := s.SaveOwnerHelperContinuity(ctx, task.ID, req, continuityBy); !errors.Is(err, api.ErrInvalid) {
			t.Fatalf("%s: %v; want invalid", name, err)
		}
	}
	if _, err := saveContinuity(s, "not-a-task", "cont-task", 0, "ok", owner); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("bad project id: %v", err)
	}
	if _, err := s.OwnerHelperContinuity(ctx, task.ID, -1); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("negative revision read: %v", err)
	}
	// Exactly the limit is accepted.
	max := strings.Repeat("y", api.MaxOwnerHelperContinuityLen)
	if r, err := saveContinuity(s, task.ID, "cont-max", 0, max, owner); err != nil || r.Bytes != api.MaxOwnerHelperContinuityLen {
		t.Fatalf("a body at the limit: %+v %v", r, err)
	}
	if list, err := s.OwnerHelperContinuityRevisions(ctx, task.ID); err != nil || len(list) != 1 {
		t.Fatalf("refused bodies were saved: %+v %v", list, err)
	}
	if _, err := s.CloseTask(ctx, task.ID, continuityBy); err != nil {
		t.Fatal(err)
	}
	if _, err := saveContinuity(s, task.ID, "cont-closed", 1, "after close", owner); !errors.Is(err, api.ErrClosed) {
		t.Fatalf("closed project: %v", err)
	}
	// A closed project's record can still be read.
	if got, err := s.OwnerHelperContinuity(ctx, task.ID, 0); err != nil || got.Revision != 1 {
		t.Fatalf("read after close: %+v %v", got, err)
	}
}

// a1: the migration runs again on an opened store, and a store without the
// table regains it on reopen with its existing rows intact.
func TestOwnerHelperContinuityMigrate(t *testing.T) {
	s, task, path := continuityStore(t)
	ctx := context.Background()
	if err := migrateOwnerHelperContinuity(s.db); err != nil {
		t.Fatalf("second run on an opened store: %v", err)
	}
	if err := migrate(s.db); err != nil {
		t.Fatalf("the whole migration again: %v", err)
	}
	summary, err := s.SaveBacklogSummary(ctx, task.ID, api.SaveBacklogSummaryRequest{Body: "synthetic summary", RequestID: "summary-1"}, continuityBy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := saveContinuity(s, task.ID, "cont-1", 0, "before the drop", api.Agent{}); err != nil {
		t.Fatal(err)
	}
	// A store from before this change has no table.
	if _, err := s.db.Exec(`DROP TABLE owner_helper_continuity`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	var n int
	if err := again.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='owner_helper_continuity'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("table after reopen: %d %v", n, err)
	}
	if got, err := again.GetTask(ctx, task.ID); err != nil || got.Name != task.Name {
		t.Fatalf("task after reopen: %+v %v", got, err)
	}
	if got, err := again.BacklogSummary(ctx, task.ID, 0); err != nil || got.Digest != summary.Digest {
		t.Fatalf("backlog summary after reopen: %+v %v", got, err)
	}
	if _, err := again.OwnerHelperContinuity(ctx, task.ID, 0); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("a regained table is empty: %v", err)
	}
	if r, err := saveContinuity(again, task.ID, "cont-2", 0, "after the reopen", api.Agent{}); err != nil || r.Revision != 1 {
		t.Fatalf("save after reopen: %+v %v", r, err)
	}
}
