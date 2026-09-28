package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/scs32/tailterm/hub/internal/api"
)

type ownershipFlags []string

func (f *ownershipFlags) String() string         { return strings.Join(*f, ",") }
func (f *ownershipFlags) Set(value string) error { *f = append(*f, value); return nil }

// A queue entry with declared ownership must name one canonical worktree and
// one logical repository. Generated descendants may be absent, but no existing
// ancestor may cross a symlink or use a case alias.
func queueRepositoryScope(cwd string, ownership []string) (string, error) {
	rootBytes, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		if len(ownership) == 0 {
			return "", nil // legacy serial queues can use a non-repository cwd
		}
		return "", fmt.Errorf("ownership requires a git worktree: %w", err)
	}
	root := strings.TrimSpace(string(rootBytes))
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", err
	}
	if realRoot != realCwd {
		return "", errors.New("queue cwd must be the worktree root when declaring ownership")
	}
	commonBytes, err := exec.Command("git", "-C", cwd, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", err
	}
	common := strings.TrimSpace(string(commonBytes))
	if !filepath.IsAbs(common) {
		common = filepath.Join(cwd, common)
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return "", err
	}
	for _, owned := range ownership {
		if owned == "" || filepath.IsAbs(owned) || strings.Contains(owned, "\\") {
			return "", errors.New("invalid repository-relative ownership")
		}
		parts := strings.Split(owned, "/")
		current := realRoot
		for _, part := range parts {
			if part == "" || part == "." || part == ".." {
				return "", errors.New("ownership path is not canonical")
			}
			entries, readErr := os.ReadDir(current)
			if os.IsNotExist(readErr) {
				break
			} // generated descendants
			if readErr != nil {
				return "", readErr
			}
			found := false
			for _, entry := range entries {
				if strings.EqualFold(entry.Name(), part) {
					if entry.Name() != part {
						return "", fmt.Errorf("ownership case alias at %s", owned)
					}
					if entry.Type()&os.ModeSymlink != 0 {
						return "", fmt.Errorf("ownership traverses symlink at %s", owned)
					}
					found = true
					break
				}
			}
			if !found {
				break
			}
			current = filepath.Join(current, part)
		}
	}
	return common, nil
}

func queueGitCommit(cwd string) (string, error) {
	output, err := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func queueIntegrationSnapshot(ctx context.Context, q api.TeamQueueEntry, item api.WorkItem, _ api.TeamCloseRequest) (*api.TeamIntegrationReady, error) {
	accepted := q.Acceptance
	if item.Status != "done" || accepted == nil || q.Repository == "" || q.BaseCommit == "" || accepted.Repository != q.Repository || accepted.BaseCommit != q.BaseCommit || accepted.ItemRevision != item.Revision || !reflect.DeepEqual(accepted.CompletionReport, item.CompletionReport) {
		return nil, errors.New("accepted item lacks an exact saved handler acceptance receipt")
	}
	if err := verifyAcceptedGit(ctx, q.Repository, q.BaseCommit, accepted.Worktree, accepted.Branch, accepted.Commit); err != nil {
		return nil, err
	}
	return &api.TeamIntegrationReady{Repository: accepted.Repository, BaseCommit: accepted.BaseCommit, Worktree: accepted.Worktree, Branch: accepted.Branch, Commit: accepted.Commit, Evidence: accepted.Evidence}, nil
}

// verifyAcceptedGit checks an accepted builder result on this host: the
// worktree is its canonical Git root in the frozen repository, sits on the
// accepted branch and commit, is clean and descends from the frozen base.
// tt team queue accept, the handler's done save and the runner share it.
func verifyAcceptedGit(ctx context.Context, repository, base, worktree, branch, commit string) error {
	git := func(args ...string) (string, error) {
		argv := append([]string{"-C", worktree}, args...)
		output, err := exec.CommandContext(ctx, "git", argv...).Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(output)), nil
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realAccepted, err := filepath.EvalSymlinks(worktree)
	if err != nil || realRoot != realAccepted {
		return errors.New("accepted worktree is not its canonical Git root")
	}
	common, err := git("rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(worktree, common)
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return err
	}
	if common != repository {
		return errors.New("repository identity changed before integration receipt")
	}
	head, err := git("symbolic-ref", "--short", "HEAD")
	if err != nil || head == "" {
		return errors.New("accepted worktree has no branch")
	}
	sha, err := git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != branch || sha != commit {
		return errors.New("accepted worktree moved from the saved branch and commit")
	}
	status, err := git("status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("accepted worktree is dirty")
	}
	if _, err := git("merge-base", "--is-ancestor", base, sha); err != nil {
		return errors.New("accepted commit is not descended from frozen base")
	}
	return nil
}

// acceptedWorktree resolves an accepted builder worktree and checks that it
// belongs to the entry's frozen repository.
func acceptedWorktree(worktree, repository string) (string, error) {
	realWorktree, err := filepath.EvalSymlinks(worktree)
	if err != nil || !filepath.IsAbs(realWorktree) {
		return "", errors.New("accepted worktree must be an existing absolute path")
	}
	scope, err := queueRepositoryScope(realWorktree, nil)
	if err != nil || scope != repository {
		return "", errors.New("accepted worktree is outside the frozen repository")
	}
	return realWorktree, nil
}
