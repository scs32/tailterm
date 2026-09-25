package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
