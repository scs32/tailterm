package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Queue acceptance recorded by the handler's done save (wi_b4ec031a206d1326).
// The save and tt team queue accept share acceptTeamQueueEntry, so they refuse
// the same candidates; the save derives repository, base, item revision and
// completion report from the entry and the saved item.

const (
	autoRepository = "/fixture/repo/.git"
	autoWorktree   = "/fixture/worktrees/builder"
	autoBranch     = "feature/fixture"
)

type autoAcceptFixture struct {
	s       *Store
	ctx     context.Context
	by      api.Caller
	task    api.Task
	handler api.Agent
	item    api.WorkItem
	entry   string
}

// newAutoAcceptFixture seeds a running, repository-backed team queue entry
// leased to the handler. An enrolled item has a passing receipt for
// candidateB on the verified base candidateA; a legacy item has no review or
// verification history. The entry's queue-time base is candidateC.
func newAutoAcceptFixture(t *testing.T, enrolled bool) *autoAcceptFixture {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "hub.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &autoAcceptFixture{s: s, ctx: context.Background(), by: api.Caller{Node: "fixture", User: "owner"}}
	if f.task, err = s.CreateTask(f.ctx, api.CreateTaskRequest{Name: "auto acceptance"}, f.by); err != nil {
		t.Fatal(err)
	}
	if f.handler, err = s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "handler", Host: "fixture", Session: "handler", Role: api.AgentRoleDatabaseHandler}, f.by); err != nil {
		t.Fatal(err)
	}
	if f.item, err = s.CreateWorkItem(f.ctx, f.task.ID, api.CreateWorkItemRequest{Kind: "bug", Title: "queued item", RequestID: "item"}, f.by); err != nil {
		t.Fatal(err)
	}
	now := ts(time.Now())
	if enrolled {
		seedPassingVerificationAt(t, s, f.item, candidateB, autoWorktree)
		if _, err = s.db.Exec(`INSERT INTO verification_enrollments(task_id,item_id,agent_id,run_id,required,provenance,created_at) VALUES(?,?,?,?,1,'fixture',?)`, f.task.ID, f.item.ID, api.NewID("agt"), api.NewID("run"), now); err != nil {
			t.Fatal(err)
		}
	}
	f.entry = api.NewID("tqe")
	if _, err = s.db.Exec(`INSERT INTO team_queue_entries(id,task_id,item_id,item_revision,order_seq,template,position,state,revision,created_at,updated_at,repository,base_commit,handler_id,handler_run_id) VALUES(?,?,?,?,1,'planned',1,'running',1,?,?,?,?,?,?)`, f.entry, f.task.ID, f.item.ID, f.item.Revision, now, now, autoRepository, candidateC, f.handler.ID, f.handler.RunID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *autoAcceptFixture) tuple(commit string) *api.WorkItemQueueAcceptance {
	return &api.WorkItemQueueAcceptance{EntryID: f.entry, Worktree: autoWorktree, Branch: autoBranch, Commit: commit}
}

func (f *autoAcceptFixture) save(key string, acceptance *api.WorkItemQueueAcceptance) (api.WorkItemUpdateResult, error) {
	done := "done"
	res, _, err := f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, api.CreateWorkItemUpdate{ExpectedRevision: f.item.Revision, Status: &done, AgentID: f.handler.ID, RunID: f.handler.RunID, RequestID: key, QueueAcceptance: acceptance}, f.by)
	return res, err
}

func (f *autoAcceptFixture) accept(t *testing.T, key, worktree, commit string) (api.TeamQueueEntry, error) {
	t.Helper()
	q := f.queueEntry(t)
	item := f.current(t)
	return f.s.TeamQueueAction(f.ctx, f.task.ID, api.TeamQueueRequest{RequestID: key, Operation: "accept", EntryID: f.entry, ExpectedRevision: q.Revision, HandlerAgentID: f.handler.ID, HandlerRunID: f.handler.RunID,
		Acceptance: &api.TeamIntegrationAcceptance{Repository: autoRepository, BaseCommit: q.BaseCommit, Worktree: worktree, Branch: autoBranch, Commit: commit, ItemRevision: item.Revision, CompletionReport: item.CompletionReport, Evidence: "handler-saved acceptance receipt"}})
}

func (f *autoAcceptFixture) queueEntry(t *testing.T) api.TeamQueueEntry {
	t.Helper()
	q, err := f.s.GetTeamQueueEntry(f.ctx, f.task.ID, f.entry)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func (f *autoAcceptFixture) current(t *testing.T) api.WorkItem {
	t.Helper()
	item, err := f.s.GetWorkItem(f.ctx, f.task.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func (f *autoAcceptFixture) releaseJobs(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM release_jobs WHERE task_id=? AND entry_id=?`, f.task.ID, f.entry).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertUntouched checks that a refused save left the item open and the entry
// unaccepted, with no release job.
func (f *autoAcceptFixture) assertUntouched(t *testing.T) {
	t.Helper()
	if item := f.current(t); item.Status == "done" || item.Revision != f.item.Revision {
		t.Fatalf("refused save changed the item: status %s revision %d", item.Status, item.Revision)
	}
	if q := f.queueEntry(t); q.Acceptance != nil || q.Revision != 1 {
		t.Fatalf("refused save changed the entry: %+v", q)
	}
	if n := f.releaseJobs(t); n != 0 {
		t.Fatalf("refused save enqueued %d release jobs", n)
	}
}

// a1, a2, a3, a4 (enrolled item, retried save): one save marks the item done
// and records acceptance on the verified base with one release job; retries
// replay; tt team queue accept stays an idempotent recovery path.
func TestDoneSaveRecordsQueueAcceptanceForEnrolledItem(t *testing.T) {
	f := newAutoAcceptFixture(t, true)
	res, err := f.save("done-save", f.tuple(candidateB))
	if err != nil {
		t.Fatal("handler done save with acceptance", err)
	}
	item := f.current(t)
	if item.Status != "done" || item.Revision != res.Revision.Revision {
		t.Fatalf("item not saved done: %+v", item)
	}
	q := f.queueEntry(t)
	a := q.Acceptance
	if a == nil || q.State != "running" || q.Revision != 2 {
		t.Fatalf("entry not accepted by the save: %+v", q)
	}
	if a.Repository != autoRepository || a.Worktree != autoWorktree || a.Branch != autoBranch || a.Commit != candidateB || a.ItemRevision != res.Revision.Revision || a.AcceptedAt == "" {
		t.Fatalf("acceptance tuple: %+v", a)
	}
	if a.BaseCommit != candidateA || q.BaseCommit != candidateA {
		t.Fatalf("acceptance did not bind the verified base: %+v base %s", a, q.BaseCommit)
	}
	if !strings.Contains(a.Evidence, res.Receipt.ID) {
		t.Fatalf("default evidence does not cite the saved receipt %s: %q", res.Receipt.ID, a.Evidence)
	}
	if n := f.releaseJobs(t); n != 1 {
		t.Fatalf("release jobs = %d, want 1", n)
	}

	// a4 retried save: same key and payload replay the receipt, nothing new.
	again, err := f.save("done-save", f.tuple(candidateB))
	if err != nil || again.Receipt.ID != res.Receipt.ID || again.Revision.Revision != res.Revision.Revision {
		t.Fatal("retried save did not replay", again, err)
	}
	if q2 := f.queueEntry(t); q2.Revision != q.Revision || q2.Acceptance.AcceptedAt != a.AcceptedAt {
		t.Fatalf("retried save changed the entry: %+v", q2)
	}
	if n := f.releaseJobs(t); n != 1 {
		t.Fatalf("retry enqueued a second release job: %d", n)
	}
	changed := f.tuple(candidateB)
	changed.Branch = "feature/other"
	if _, err = f.save("done-save", changed); !errors.Is(err, api.ErrConflict) {
		t.Fatal("same request key with a different tuple was not refused", err)
	}

	// a3: an identical manual accept is a no-op; a different tuple is refused.
	same, err := f.accept(t, "manual-accept", autoWorktree, candidateB)
	if err != nil {
		t.Fatal("identical manual accept after the save", err)
	}
	if q3 := f.queueEntry(t); same.Revision != q.Revision || q3.Revision != q.Revision || f.releaseJobs(t) != 1 {
		t.Fatalf("identical manual accept changed the entry or release jobs: %+v", q3)
	}
	if _, err = f.accept(t, "manual-other-commit", autoWorktree, candidateC); !errors.Is(err, api.ErrConflict) {
		t.Fatal("manual accept of a different commit was not refused", err)
	}
	if _, err = f.accept(t, "manual-other-worktree", "/fixture/worktrees/other", candidateB); !errors.Is(err, api.ErrConflict) {
		t.Fatal("manual accept of a different worktree was not refused", err)
	}
}

// a1, a4 (legacy item): acceptance keeps the queue-time base and enqueues no
// release job.
func TestDoneSaveRecordsQueueAcceptanceForLegacyItem(t *testing.T) {
	f := newAutoAcceptFixture(t, false)
	evidence := f.tuple(candidateB)
	evidence.Evidence = "handler completion RESULT #1"
	res, err := f.save("legacy-save", evidence)
	if err != nil {
		t.Fatal("legacy handler done save", err)
	}
	q := f.queueEntry(t)
	if q.Acceptance == nil || q.Acceptance.BaseCommit != candidateC || q.BaseCommit != candidateC || q.Acceptance.ItemRevision != res.Revision.Revision || q.Acceptance.Evidence != "handler completion RESULT #1" {
		t.Fatalf("legacy acceptance: %+v", q)
	}
	if n := f.releaseJobs(t); n != 0 {
		t.Fatalf("legacy acceptance enqueued %d release jobs", n)
	}
}

// a2, a4 (mismatched candidate): a commit other than the accepted review
// candidate is refused and the whole save rolls back.
func TestDoneSaveRefusesMismatchedCandidate(t *testing.T) {
	f := newAutoAcceptFixture(t, true)
	if _, err := f.save("mismatch", f.tuple(candidateC)); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), "candidate") {
		t.Fatal("mismatched candidate was not refused", err)
	}
	f.assertUntouched(t)
	if _, err := f.s.GetWorkItemUpdateReceipt(f.ctx, f.task.ID, f.item.ID, "mismatch", f.handler.ID, f.by); !errors.Is(err, api.ErrNotFound) {
		t.Fatal("refused save left an update receipt", err)
	}
}

// a1 forcing function: while an entry waits, the handler cannot save done
// without the tuple on either update path. Owner saves are unchanged and the
// manual accept recovers them (a3).
func TestHandlerDoneSaveWithoutAcceptanceIsRefused(t *testing.T) {
	f := newAutoAcceptFixture(t, false)
	if _, err := f.save("no-tuple", nil); !errors.Is(err, api.ErrConflict) || !strings.Contains(err.Error(), f.entry) {
		t.Fatal("handler done save without acceptance was not refused", err)
	}
	f.assertUntouched(t)
	done := "done"
	if _, err := f.s.UpdateWorkItem(f.ctx, f.task.ID, f.item.ID, api.UpdateWorkItemRequest{Revision: f.item.Revision, Status: &done, AgentID: f.handler.ID}, f.by); !errors.Is(err, api.ErrConflict) {
		t.Fatal("legacy PATCH handler done save without acceptance was not refused", err)
	}
	f.assertUntouched(t)

	other := f.tuple(candidateB)
	other.EntryID = api.NewID("tqe")
	if _, err := f.save("wrong-entry", other); !errors.Is(err, api.ErrConflict) {
		t.Fatal("acceptance for another entry was not refused", err)
	}
	f.assertUntouched(t)

	if _, _, err := f.s.CreateWorkItemUpdate(f.ctx, f.task.ID, f.item.ID, api.CreateWorkItemUpdate{ExpectedRevision: f.item.Revision, Status: &done, RequestID: "owner-done"}, f.by); err != nil {
		t.Fatal("owner done save", err)
	}
	if q := f.queueEntry(t); q.Acceptance != nil {
		t.Fatal("owner save recorded acceptance")
	}
	got, err := f.accept(t, "owner-recovery", autoWorktree, candidateB)
	if err != nil || got.Acceptance == nil || got.Acceptance.BaseCommit != candidateC {
		t.Fatal("manual accept after an owner done save", got, err)
	}
	if _, err = f.accept(t, "owner-recovery", autoWorktree, candidateB); err != nil {
		t.Fatal("manual accept retry", err)
	}
}

// a2 parity: each input refused by tt team queue accept is refused by the done
// save with the same refusal, and the save leaves item and entry unchanged.
// The save takes repository and base from the entry, so a caller cannot submit
// an unrelated base; the verified-base binding is asserted above.
func TestDoneSaveRefusesWhatQueueAcceptRefuses(t *testing.T) {
	for _, row := range []struct {
		name     string
		enrolled bool
		setup    func(t *testing.T, f *autoAcceptFixture) (agent api.Agent)
		worktree string
		commit   string
		want     error
	}{
		{name: "other handler lease", enrolled: true, worktree: autoWorktree, commit: candidateB, want: api.ErrConflict, setup: func(t *testing.T, f *autoAcceptFixture) api.Agent {
			other, err := f.s.AddAgent(f.ctx, f.task.ID, api.AddAgentRequest{AgentID: api.NewID("agt"), Name: "other-handler", Host: "fixture", Session: "other", Role: api.AgentRoleDatabaseHandler}, f.by)
			if err != nil {
				t.Fatal(err)
			}
			return other
		}},
		{name: "retired handler", enrolled: true, worktree: autoWorktree, commit: candidateB, want: api.ErrConflict, setup: func(t *testing.T, f *autoAcceptFixture) api.Agent {
			if _, err := f.s.db.Exec(`UPDATE agents SET status=? WHERE id=?`, api.AgentRetired, f.handler.ID); err != nil {
				t.Fatal(err)
			}
			return f.handler
		}},
		{name: "other worktree under a worktree plan", enrolled: true, worktree: "/fixture/worktrees/other", commit: candidateB, want: api.ErrConflict},
		{name: "commit differs from the accepted candidate", enrolled: true, worktree: autoWorktree, commit: candidateC, want: api.ErrConflict},
		{name: "relative worktree", enrolled: false, worktree: "fixture/worktrees/builder", commit: candidateB, want: api.ErrInvalid},
		{name: "unclean worktree path", enrolled: false, worktree: "/fixture/worktrees/../builder", commit: candidateB, want: api.ErrInvalid},
		{name: "short commit", enrolled: false, worktree: autoWorktree, commit: "bbbb", want: api.ErrInvalid},
	} {
		t.Run(row.name, func(t *testing.T) {
			agentFor := func(f *autoAcceptFixture) api.Agent {
				if row.setup != nil {
					return row.setup(t, f)
				}
				return f.handler
			}
			// Save path.
			saved := newAutoAcceptFixture(t, row.enrolled)
			agent := agentFor(saved)
			done := "done"
			acceptance := &api.WorkItemQueueAcceptance{EntryID: saved.entry, Worktree: row.worktree, Branch: autoBranch, Commit: row.commit}
			_, _, saveErr := saved.s.CreateWorkItemUpdate(saved.ctx, saved.task.ID, saved.item.ID, api.CreateWorkItemUpdate{ExpectedRevision: saved.item.Revision, Status: &done, AgentID: agent.ID, RunID: agent.RunID, RequestID: "parity", QueueAcceptance: acceptance}, saved.by)
			if !errors.Is(saveErr, row.want) {
				t.Fatalf("save path: got %v, want %v", saveErr, row.want)
			}
			saved.assertUntouched(t)

			// Accept path, on an item already saved done as today.
			manual := newAutoAcceptFixture(t, row.enrolled)
			agent = agentFor(manual)
			if _, err := manual.s.db.Exec(`UPDATE work_items SET status='done' WHERE id=?`, manual.item.ID); err != nil {
				t.Fatal(err)
			}
			q := manual.queueEntry(t)
			_, acceptErr := manual.s.TeamQueueAction(manual.ctx, manual.task.ID, api.TeamQueueRequest{RequestID: "parity", Operation: "accept", EntryID: manual.entry, ExpectedRevision: q.Revision, HandlerAgentID: agent.ID, HandlerRunID: agent.RunID,
				Acceptance: &api.TeamIntegrationAcceptance{Repository: autoRepository, BaseCommit: q.BaseCommit, Worktree: row.worktree, Branch: autoBranch, Commit: row.commit, ItemRevision: manual.item.Revision, Evidence: "handler-saved acceptance receipt"}})
			if !errors.Is(acceptErr, row.want) {
				t.Fatalf("accept path: got %v, want %v", acceptErr, row.want)
			}
			if saveErr.Error() != acceptErr.Error() {
				t.Errorf("refusals differ: save %q, accept %q", saveErr, acceptErr)
			}
		})
	}
}
