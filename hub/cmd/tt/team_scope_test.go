package main

import (
	"context"
	"database/sql"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/scs32/tailterm/hub/internal/api"
)

func gitFixtureCommand(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", cwd}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestParallelQueueWorktreeScopeAndIntegrationSnapshot(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	worktree := filepath.Join(root, "item-c")
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0755); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, repo, "init", "-q", "-b", "tasks-hub")
	if err := os.WriteFile(filepath.Join(repo, "src", "a.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, repo, "add", ".")
	gitFixtureCommand(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "-m", "base")
	base := gitFixtureCommand(t, repo, "rev-parse", "HEAD")
	gitFixtureCommand(t, repo, "worktree", "add", "-q", "-b", "feature/item-c", worktree)
	commonA, err := queueRepositoryScope(repo, []string{"src/a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	commonC, err := queueRepositoryScope(worktree, []string{"src"})
	if err != nil || commonA != commonC {
		t.Fatalf("logical repository differs across worktrees: %q %q %v", commonA, commonC, err)
	}
	for _, alias := range []string{"../src/a.txt", "src/./a.txt", "src//a.txt", "src/A.txt", "/src/a.txt"} {
		if _, err := queueRepositoryScope(worktree, []string{alias}); err == nil {
			t.Fatalf("ownership alias %q was accepted", alias)
		}
	}
	if err := os.Symlink("src", filepath.Join(worktree, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := queueRepositoryScope(worktree, []string{"alias/a.txt"}); err == nil {
		t.Fatal("ownership traversed symlink")
	}
	if err := os.Remove(filepath.Join(worktree, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "c.txt"), []byte("item C\n"), 0644); err != nil {
		t.Fatal(err)
	}
	entry := api.TeamQueueEntry{Cwd: worktree, Repository: commonC, BaseCommit: base}
	item := api.WorkItem{ID: "wi_aaaaaaaaaaaaaaaa", Revision: 2, Status: "done"}
	closeReq := api.TeamCloseRequest{RequestID: "exact-close-receipt"}
	if _, err := queueIntegrationSnapshot(context.Background(), entry, item, closeReq); err == nil {
		t.Fatal("dirty worktree was marked ready")
	}
	gitFixtureCommand(t, worktree, "add", ".")
	gitFixtureCommand(t, worktree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "-m", "item C")
	commit := gitFixtureCommand(t, worktree, "rev-parse", "HEAD")
	entry.Cwd = repo // Launch may have used the main checkout, not the builder worktree.
	entry.Acceptance = &api.TeamIntegrationAcceptance{Repository: commonC, BaseCommit: base, Worktree: worktree, Branch: "feature/item-c", Commit: commit, ItemRevision: item.Revision, Evidence: "handler-saved completion receipt"}
	ready, err := queueIntegrationSnapshot(context.Background(), entry, item, closeReq)
	if err != nil || ready.Repository != commonC || ready.BaseCommit != base || ready.Worktree != worktree || ready.Branch != "feature/item-c" || ready.Commit != commit || ready.Evidence != "handler-saved completion receipt" {
		t.Fatalf("integration snapshot %+v %v", ready, err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "c.txt"), []byte("advanced\n"), 0644); err != nil {
		t.Fatal(err)
	}
	gitFixtureCommand(t, worktree, "add", ".")
	gitFixtureCommand(t, worktree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-q", "-m", "advance after acceptance")
	if _, err := queueIntegrationSnapshot(context.Background(), entry, item, closeReq); err == nil {
		t.Fatal("finish substituted advanced HEAD for accepted SHA")
	}
	item.Status = "dismissed"
	if _, err := queueIntegrationSnapshot(context.Background(), entry, item, closeReq); err == nil {
		t.Fatal("dismissed item was marked ready")
	}
}

// ownershipFlags backs --owns on tt team queue add/scope and tt work-items
// scope, so one flag set covers them all.
func TestOwnershipFlagRefusesCommaJoinedPaths(t *testing.T) {
	parse := func(args ...string) (ownershipFlags, error) {
		var owns ownershipFlags
		fs := flag.NewFlagSet("owns", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.Var(&owns, "owns", "repository-relative owned file or directory (repeatable)")
		return owns, fs.Parse(args)
	}
	owns, err := parse("--owns", "hub/a.go", "--owns", "docs", "--owns", "hub/b.go")
	if err != nil || !reflect.DeepEqual([]string(owns), []string{"hub/a.go", "docs", "hub/b.go"}) {
		t.Fatalf("repeated --owns = %q, %v", owns, err)
	}
	owns, err = parse("--owns", "hub/a.go", "--owns", "a,b")
	if err == nil {
		t.Fatalf("comma-joined --owns was accepted: %q", owns)
	}
	if !strings.Contains(err.Error(), `"a,b"`) || !strings.Contains(err.Error(), "repeat --owns once per path") {
		t.Fatalf("comma refusal lacks the value or the hint: %v", err)
	}
	if !reflect.DeepEqual([]string(owns), []string{"hub/a.go"}) {
		t.Fatalf("refused value was stored: %q", owns)
	}
}

// unenrol models a legacy item admitted before mandatory verification
// enrolment: this isolated database keeps the item's bindings but records each
// as the rollout migration does, a required=0 legacy enrolment.
func (a *acceptanceBaseFixture) unenrol(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", a.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`DROP TRIGGER verification_enrollment_no_delete`, nil},
		{`DELETE FROM verification_enrollments WHERE task_id=? AND item_id=?`, []any{a.task.ID, a.item.ID}},
		{`INSERT INTO verification_enrollments SELECT item_task_id,item_id,agent_id,run_id,0,'legacy-pre-rollout',created_at FROM agent_work_item_bindings WHERE item_task_id=? AND item_id=?`, []any{a.task.ID, a.item.ID}},
		{`CREATE TRIGGER verification_enrollment_no_delete BEFORE DELETE ON verification_enrollments BEGIN SELECT RAISE(ABORT,'verification enrollment is immutable'); END`, nil},
	} {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.query, err)
		}
	}
	enrolments, err := a.c.VerificationEnrollment(a.ctx, a.task.ID, a.item.ID, a.handler.ID, a.handler.RunID)
	if err != nil || len(enrolments) == 0 {
		t.Fatalf("legacy enrolment: %+v %v", enrolments, err)
	}
	for _, enrolment := range enrolments {
		if enrolment.Required {
			t.Fatalf("item is still enrolled: %+v", enrolments)
		}
	}
}

// accepted checks that the entry's acceptance and its release job are bound to
// the plan base and the exact candidate.
func (a *acceptanceBaseFixture) accepted(t *testing.T, commit string) {
	t.Helper()
	entry, err := a.c.GetTeamQueueEntry(a.ctx, a.task.ID, a.queued.ID)
	if err != nil || entry.Acceptance == nil || entry.Acceptance.Commit != commit || entry.Acceptance.BaseCommit != a.tip || entry.BaseCommit != a.tip {
		t.Fatalf("acceptance is not bound to the plan base %s: %+v %v", a.tip, entry.Acceptance, err)
	}
	if entry.Release == nil || entry.Release.BaseCommit != a.tip || entry.Release.Commit != commit {
		t.Fatalf("release job is not bound to the plan base %s and commit %s: %+v", a.tip, commit, entry.Release)
	}
}

// Enrolment decides only whether a plan is required: with a plan, the base is
// the plan's for enrolled and non-enrolled items alike, and the plan pins the
// commit, as the store's queue gate and release gate require
// (wi_e5b42a927a0455a7).
func TestAcceptanceBaseIgnoresEnrolmentForItemsWithAPlan(t *testing.T) {
	a := newAcceptanceBaseFixture(t)
	base := func(commit string) (string, error) {
		return acceptanceBase(a.ctx, a.c, a.task.ID, a.item.ID, a.handler.ID, a.handler.RunID, a.queueBase, commit)
	}
	if got, err := base(a.oldCommit); err != nil || got != a.queueBase {
		t.Fatalf("item without a plan: base %q, %v; want the entry base %s", got, err, a.queueBase)
	}
	rebased := a.rebase(t)
	a.freeze(t, rebased)
	for _, when := range []string{"enrolled", "non-enrolled"} {
		if when == "non-enrolled" {
			a.unenrol(t)
		}
		if got, err := base(rebased); err != nil || got != a.tip {
			t.Fatalf("%s item with a plan: base %q, %v; want the plan base %s", when, got, err, a.tip)
		}
		if _, err := base(a.oldCommit); err == nil || !strings.Contains(err.Error(), "is not the verification plan commit "+rebased) {
			t.Fatalf("%s item with a plan accepted another commit: %v", when, err)
		}
	}
}

// A non-enrolled item with a receipted plan on a later base than its queue
// entry is accepted on the plan base, which its release job requires.
func TestQueueAcceptOfNonEnrolledItemWithReceiptedPlan(t *testing.T) {
	a := newAcceptanceBaseFixture(t)
	rebased := a.rebase(t)
	a.freeze(t, rebased)
	a.unenrol(t)
	a.ownerDone(t)
	if out, err := a.queueAccept(t, rebased); err != nil {
		t.Fatalf("queue accept of a non-enrolled item with a receipted plan: %v\n%s", err, out)
	}
	a.accepted(t, rebased)
}

func TestDoneSaveOfNonEnrolledItemWithReceiptedPlan(t *testing.T) {
	a := newAcceptanceBaseFixture(t)
	rebased := a.rebase(t)
	a.freeze(t, rebased)
	a.unenrol(t)
	if out, err := a.doneSave(t, rebased); err != nil {
		t.Fatalf("done save of a non-enrolled item with a receipted plan: %v\n%s", err, out)
	}
	a.accepted(t, rebased)
}

// A non-enrolled candidate built on the queue-time base does not descend from
// the plan base, so it is refused as an enrolled one is.
func TestAcceptanceRefusesNonEnrolledCandidateOutsideThePlanBase(t *testing.T) {
	a := newAcceptanceBaseFixture(t)
	a.freeze(t, a.oldCommit)
	a.unenrol(t)
	if _, err := a.doneSave(t, a.oldCommit); err == nil || !strings.Contains(err.Error(), "not a fast-forward of the recorded base") {
		t.Fatalf("done save of a non-enrolled candidate outside the plan base: %v", err)
	}
	a.unaccepted(t, "refused non-enrolled done save")
}
