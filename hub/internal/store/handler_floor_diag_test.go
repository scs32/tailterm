package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Diagnosis for wi_0f23db4849705da0 (order #42409): each test asserts what
// the hub does today at tasks-hub 79c0895e for one of the handler floor and
// rotation follow-up claims, and passes while the behaviour is unchanged. They
// are not fixes: a later bounded order that changes the behaviour updates or
// replaces them. Each uses an isolated SQLite hub and synthetic agents only.
// See docs/incidents/handler-floor-remainder-0f23/report.md.

// diagSuccessor registers a prepared rotation's successor, started and online.
func diagSuccessor(t *testing.T, f *rotationFixture, r api.HandlerRotation, key string) api.Agent {
	t.Helper()
	ctx := context.Background()
	a, err := f.s.AddAgent(ctx, f.task.ID, api.AddAgentRequest{AgentID: r.SuccessorAgentID, Role: api.AgentRoleDatabaseHandler, Name: r.SuccessorName, Host: "mini", Session: "tt-handler-" + key, Runtime: f.old.Runtime, Cwd: f.old.Cwd}, f.by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.PostEvent(ctx, f.task.ID, api.PostEventRequest{AgentID: a.ID, RunID: a.RunID, Kind: api.EventStarted}, f.by); err != nil {
		t.Fatal(err)
	}
	if a, err = f.s.GetAgent(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	return a
}

func diagAbort(f *rotationFixture, r api.HandlerRotation, key string) (api.HandlerRotation, error) {
	return f.s.HandlerRotationAction(context.Background(), f.task.ID, api.HandlerRotationRequest{Operation: api.HandlerRotationAbort, RequestID: key, RotationID: r.ID}, f.by)
}

func availableHandlers(t *testing.T, s *Store, task string) int {
	return countRows(t, s, `SELECT count(*) FROM agents WHERE task_id=? AND role=? AND status NOT IN (?,?,?)`, task, api.AgentRoleDatabaseHandler, api.AgentClosed, api.AgentExited, api.AgentRetired)
}

// diagPreparedAway makes the recorded primary P (after one committed
// rotation), prepares a second rotation away from P and registers its
// successor S2. P and S2 are then the only available handlers.
func diagPreparedAway(t *testing.T) (*rotationFixture, api.Agent, api.HandlerRotation, api.Agent) {
	t.Helper()
	f := newRotationFixture(t)
	primary := floorRotate(t, f, "diag")
	f.old = primary
	f.idle(t, primary)
	r, err := f.prepare(t, "diag-away", api.Agent{}, "")
	if err != nil {
		t.Fatal(err)
	}
	successor := diagSuccessor(t, f, r, "diag-away")
	if n := availableHandlers(t, f.s, f.task.ID); n != 2 {
		t.Fatalf("available handlers before the change: %d", n)
	}
	return f, primary, r, successor
}

// f1 (wi_25f41fb1831ff328): reproduces. A prepared rotation counts as a ready
// successor for a close as well as a retire (handler_floor.go:29,33,66), so
// the primary closes; commit then refuses the closed old handler
// (handler_rotation.go:1179) and the rotation can only be aborted, which
// closes the successor too and leaves no available handler and no notice.
func TestHandlerFloorDiagF1ClosingPrimaryStrandsPreparedRotation(t *testing.T) {
	f, primary, r, successor := diagPreparedAway(t)
	ctx := context.Background()
	if _, err := f.s.CloseAgentRun(ctx, primary.ID, primary.RunID, f.by); err != nil {
		t.Fatalf("today the primary closes under a prepared rotation: %v", err)
	}
	if got, _ := f.s.GetAgent(ctx, primary.ID); got.Status != api.AgentClosed {
		t.Fatalf("primary status: %s", got.Status)
	}
	_, err := f.commit(r, "diag-away-commit")
	if code := refusalCode(err); code != api.HandlerRotationRefusedStaleHandler {
		t.Fatalf("commit after the close: want %s, got %q (%v)", api.HandlerRotationRefusedStaleHandler, code, err)
	}
	if got, err := f.s.GetHandlerRotation(ctx, f.task.ID, r.ID); err != nil || got.State != api.HandlerRotationPrepared {
		t.Fatalf("the rotation is stranded prepared: %+v %v", got, err)
	}
	if task, _ := f.s.GetTask(ctx, f.task.ID); task.PrimaryHandlerID != primary.ID {
		t.Fatalf("recorded primary: %s", task.PrimaryHandlerID)
	}
	if _, err = diagAbort(f, r, "diag-away-abort"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if got, _ := f.s.GetAgent(ctx, successor.ID); got.Status != api.AgentClosed {
		t.Fatalf("successor status after abort: %s", got.Status)
	}
	if n := availableHandlers(t, f.s, f.task.ID); n != 0 {
		t.Fatalf("available handlers after abort: %d", n)
	}
	if n := len(floorNotices(t, f.s, f.task.ID)); n != 0 {
		t.Fatalf("today no owner notice is posted: %d", n)
	}
}

// f3 (wi_bebcd76367a4f29f): reproduces. setAgentStatus commits the owner
// notice in its own transaction (store.go:1221, handler_floor.go:76-89)
// before writeAgentStatus updates the agent (store.go:1238). A status write
// that fails, injected here by a trigger in the test database, leaves a
// notice for a transition that never happened, and each retry adds another.
func TestHandlerFloorDiagF3NoticeCommitsBeforeStatusWrite(t *testing.T) {
	f := newFloorFixture(t)
	h := f.handler(t, "db-handler")
	if _, err := f.s.db.Exec(`CREATE TRIGGER diag_fail_status BEFORE UPDATE OF status ON agents WHEN NEW.id='` + h.ID + `' AND NEW.status='exited'
 BEGIN SELECT RAISE(ABORT, 'diag injected status write failure'); END`); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		err := f.event(t, h, api.EventExited)
		if err == nil || !strings.Contains(err.Error(), "diag injected status write failure") {
			t.Fatalf("attempt %d: want the injected failure, got %v", attempt, err)
		}
		if got := f.get(t, h.ID); got.Status != api.AgentRunning {
			t.Fatalf("attempt %d: status %s", attempt, got.Status)
		}
		if n := len(floorNotices(t, f.s, f.task.ID)); n != attempt {
			t.Fatalf("attempt %d: today each failed attempt leaves a notice, got %d", attempt, n)
		}
	}
	if n := countRows(t, f.s, `SELECT count(*) FROM events WHERE task_id=? AND agent_id=? AND kind=?`, f.task.ID, h.ID, api.EventExited); n != 0 {
		t.Fatalf("exited events: %d", n)
	}
	if _, err := f.s.db.Exec(`DROP TRIGGER diag_fail_status`); err != nil {
		t.Fatal(err)
	}
	if err := f.event(t, h, api.EventExited); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, h.ID); got.Status != api.AgentExited {
		t.Fatalf("status after the write succeeds: %s", got.Status)
	}
	if n := len(floorNotices(t, f.s, f.task.ID)); n != 3 {
		t.Fatalf("notices for one real transition: %d", n)
	}
}

// f4 (wi_183bebdee30e07cb): reproduces. A prepared rotation lets the primary
// retire (handler_floor.go:47,66) while the successor keeps the count at two,
// so no notice is due. Abort then closes the successor by raw SQL
// (handler_rotation.go:1442), which bypasses the floor: the count reaches
// zero and no owner notice is posted.
func TestHandlerFloorDiagF4AbortReachesZeroWithoutNotice(t *testing.T) {
	f, primary, r, successor := diagPreparedAway(t)
	ctx := context.Background()
	retired := api.AgentRetired
	if _, err := f.s.UpdateAgent(ctx, primary.ID, api.UpdateAgentRequest{Status: &retired}, f.by); err != nil {
		t.Fatalf("retire the primary under a prepared rotation: %v", err)
	}
	if n := len(floorNotices(t, f.s, f.task.ID)); n != 0 {
		t.Fatalf("notices after the retire: %d", n)
	}
	if _, err := diagAbort(f, r, "diag-abort"); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if got, _ := f.s.GetAgent(ctx, successor.ID); got.Status != api.AgentClosed {
		t.Fatalf("successor status after abort: %s", got.Status)
	}
	if got, _ := f.s.GetAgent(ctx, primary.ID); got.Status != api.AgentRetired {
		t.Fatalf("primary status after abort: %s", got.Status)
	}
	if n := availableHandlers(t, f.s, f.task.ID); n != 0 {
		t.Fatalf("available handlers after abort: %d", n)
	}
	if n := len(floorNotices(t, f.s, f.task.ID)); n != 0 {
		t.Fatalf("today the abort posts no owner notice: %d", n)
	}
}

// diagSendRole posts a request to role:database_handler linked to the
// fixture's first item and returns the message or the refusal.
func diagSendRole(f *rotationFixture, from api.Agent, subject string) (api.Message, error) {
	item := f.items[0]
	env := &api.Envelope{Kind: api.EnvelopeKindRequest, To: "role:database_handler", Subject: subject, Body: api.EnvelopeBody{Ask: "Please record this synthetic diagnosis item."}}
	req := api.PostMessageRequest{Envelope: env, AgentID: from.ID, RunID: from.RunID, RequestID: api.NewID("req"),
		WorkItems:        []api.MessageWorkItem{{ItemTaskID: item.TaskID, ItemID: item.ID, ItemRevision: item.Revision, Relationship: "primary"}},
		WorkOrderMessage: &api.MessageReference{TaskID: f.task.ID, Seq: f.orders[0].Seq}}
	req.Text = api.RenderText(*env)
	return f.s.PostMessage(context.Background(), f.task.ID, req, f.by)
}

// g3 (wi_70e2545bb8a643f0): reproduces as a refusal. A released entry is not
// a lease (team_queue_scope.go:68), so no rotation moves it, but item role
// resolution still reads an owner-integrated released entry's handler
// (phase3.go:264) and refuses its closed run (phase3.go:271) instead of
// routing role:database_handler to the live successor. A plainly released
// entry falls through to the primary.
func TestHandlerFloorDiagG3OwnerIntegratedEntryRoutesToDeadHandler(t *testing.T) {
	for _, integration := range []string{`{"diag":true}`, ""} {
		name := "owner-integrated"
		if integration == "" {
			name = "plainly released"
		}
		t.Run(name, func(t *testing.T) {
			f := newRotationFixture(t)
			ctx := context.Background()
			old := f.old
			q, err := f.s.TeamQueueAction(ctx, f.task.ID, api.TeamQueueRequest{RequestID: "diag-g3-add", Operation: "add", ItemID: f.items[0].ID, OrderMessageSeq: f.orders[0].Seq, Host: "mini", Cwd: "/diag-g3"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.db.Exec(`UPDATE team_queue_entries SET state='running',handler_id=?,handler_run_id=?,handler_lease_generation=1,released_at=?,owner_integration_json=? WHERE id=?`,
				old.ID, old.RunID, ts(time.Now().UTC()), integration, q.ID); err != nil {
				t.Fatal(err)
			}
			successor := floorRotate(t, f, "diag-g3")
			var handler string
			if err = f.s.db.QueryRow(`SELECT handler_id FROM team_queue_entries WHERE id=?`, q.ID).Scan(&handler); err != nil || handler != old.ID {
				t.Fatalf("released entry handler after rotation: %s %v", handler, err)
			}
			m, err := diagSendRole(f, f.worker, "Synthetic request after the rotation")
			if integration == "" {
				if err != nil || m.To != successor.ID {
					t.Fatalf("plainly released entry: want the successor %s, got to=%q %v", successor.ID, m.To, err)
				}
				return
			}
			if !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "exact item role run is unavailable") {
				t.Fatalf("owner-integrated entry: today the role resolves to the closed old run and is refused, got to=%q %v", m.To, err)
			}
		})
	}
}

// g2 (wi_c3267a9dd720279f): reproduces. Commit of an exited primary's
// rotation reissues every obligation the broker closed recipient_gone after
// the exit (handler_rotation.go:679-698,1242,1262) with no check for a
// request the sender already re-sent to the successor, so the successor
// holds the same request twice and the handoff records no skip.
func TestHandlerFloorDiagG2ResentRequestReachesSuccessorTwice(t *testing.T) {
	f := newDeadPrimaryFixture(t)
	s := f.s
	const subject = "First fixture request held by the handler"
	original := f.requests[0]
	f.advance(time.Minute)
	exit := f.exit(t, 3)
	f.advance(30 * time.Second)
	f.brokerCloses(t, original.Seq)
	f.advance(11 * time.Minute)
	r, err := f.prepareExited(t, "diag-g2-prepare", f.idleEvidence(exit))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	successor := f.succeed(t, r)
	// While the rotation is prepared the role names no holder (the primary
	// exited, a prepared successor is excluded), so the sender re-sends to
	// the successor by name.
	if _, err := diagSendRole(f.rotationFixture, f.worker, subject); !errors.Is(err, api.ErrConflict) {
		t.Fatalf("role:database_handler during the prepared rotation: %v", err)
	}
	resent := f.send(t, f.worker, api.EnvelopeKindRequest, successor.ID, subject, false)
	f.advance(30 * time.Second)
	r, err = f.commitDead(r, "rotation-commit-"+r.ID, f.idleEvidence(exit))
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	var reissue *api.HandlerRotationReissue
	for i, p := range r.Handoff.Reissued {
		if p.OldMessageSeq == original.Seq {
			reissue = &r.Handoff.Reissued[i]
		}
	}
	if reissue == nil || reissue.OldState != api.ObligationClosed || reissue.NewObligationID == "" {
		t.Fatalf("today the closed request is reissued despite the re-send: %+v", r.Handoff.Reissued)
	}
	for _, seq := range []int64{resent.Seq, reissue.NewMessageSeq} {
		if n := countRows(t, s, `SELECT count(*) FROM obligations WHERE agent_id=? AND message_seq=? AND state<>'closed' AND source_kind='request'`, successor.ID, seq); n != 1 {
			t.Fatalf("request #%d open on the successor %d times", seq, n)
		}
	}
	if n := countRows(t, s, `SELECT count(*) FROM obligations WHERE agent_id=? AND state<>'closed' AND source_kind='request' AND subject LIKE ?`, successor.ID, "%"+subject); n != 2 {
		t.Fatalf("today the successor holds the same request twice, got %d", n)
	}
}
