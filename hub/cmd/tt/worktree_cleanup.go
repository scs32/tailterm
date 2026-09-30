package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// Worktree cleanup removes finished items' Git worktrees and the caches inside
// them. Team closeout and the one-time sweep share one classifier: a worktree
// is removed only when nothing keeps it, and every keep has a reason. Git's
// own `worktree remove` (never --force) refuses dirty work but deletes ignored
// content, nested repositories included, so those are checked first. Branches
// are never deleted, so an accepted commit stays reachable after its worktree
// goes.

// Keep reasons, in the order they are checked.
const (
	keepLocked    = "locked"
	keepMissing   = "missing"
	keepInUse     = "in-use"
	keepNested    = "nested"
	keepRecent    = "recent"
	keepEvidence  = "evidence"
	keepOperation = "operation"
	keepDirty     = "dirty"
	keepUnpushed  = "unpushed"
	keepMoved     = "moved"
	keepFailed    = "remove-failed"
)

// closeoutWorktreeInterval bounds how often closeout re-examines a finished
// entry whose worktree was kept, such as one waiting for its release.
const closeoutWorktreeInterval = 15 * time.Minute

type gitWorktree struct {
	Path     string
	Head     string
	Branch   string
	Detached bool
	Locked   bool
	Prunable bool
	Bare     bool
	Main     bool
}

type worktreeDecision struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Head   string `json:"head"`
	Action string `json:"action"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
	Bytes  int64  `json:"bytes"`
	// recorded marks a decision whose receipt this call wrote.
	recorded bool
}

type worktreeEvidence struct {
	Source string
	Text   string
}

type worktreeCleanupInputs struct {
	// Repo is any worktree of the repository, or its common Git directory.
	Repo string
	// Select limits the candidates; nil examines every linked worktree.
	Select   func(w gitWorktree, main string, linked []string) bool
	InUse    []string
	Evidence []worktreeEvidence
	// MinIdle keeps worktrees whose Git state changed recently (sweep only).
	MinIdle      time.Duration
	Now          time.Time
	Apply        bool
	MeasureBytes bool
	Receipt      worktreeCleanupReceipt
}

type worktreeCleanupReceipt struct {
	At      string `json:"at"`
	Source  string `json:"source"`
	TaskID  string `json:"taskId,omitempty"`
	EntryID string `json:"entryId,omitempty"`
	ItemID  string `json:"itemId,omitempty"`
	Path    string `json:"path"`
	Branch  string `json:"branch,omitempty"`
	Head    string `json:"head,omitempty"`
	Action  string `json:"action"`
	Reason  string `json:"reason,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// worktreeGit runs one Git command; tests wrap it to count calls. Optional
// locks are off so `git status` never rewrites an index: a dry run changes
// nothing, including the mtimes the recent rule reads.
var worktreeGit = func(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-C", dir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// gitExitCode reports a Git command's exit status, or -1 for any other error.
func gitExitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// keptWorktreeReceipts remembers the kept outcomes this process has recorded,
// so a worktree waiting for its release costs one receipt line, not one per
// look.
var keptWorktreeReceipts sync.Map

// closeoutWorktreeChecks remembers when closeout last examined each entry.
var closeoutWorktreeChecks sync.Map

func worktreeCleanupLock() (*os.File, error) {
	if err := os.MkdirAll(relayDir(), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(relayDir(), "worktree-cleanup.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("worktree cleanup is active on this host: %w", err)
	}
	return file, nil
}

func appendWorktreeReceipt(r worktreeCleanupReceipt) error {
	if err := os.MkdirAll(relayDir(), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(relayDir(), "worktree-cleanup.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(append(data, '\n'))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// recordWorktreeDecision appends the decision's receipt and reports whether
// it was new: kept outcomes are written once per path, reason and head.
func recordWorktreeDecision(base worktreeCleanupReceipt, d worktreeDecision, now time.Time) (bool, error) {
	if d.Action == "kept" {
		key := d.Path + "\x00" + d.Reason + "\x00" + d.Head
		if _, seen := keptWorktreeReceipts.LoadOrStore(key, true); seen {
			return false, nil
		}
	}
	r := base
	r.At, r.Path, r.Branch, r.Head, r.Action, r.Reason, r.Detail = now.UTC().Format(time.RFC3339Nano), d.Path, d.Branch, d.Head, d.Action, d.Reason, d.Detail
	return true, appendWorktreeReceipt(r)
}

func parseWorktreeList(data string) []gitWorktree {
	var out []gitWorktree
	var cur *gitWorktree
	for _, field := range strings.Split(data, "\x00") {
		if field == "" {
			cur = nil
			continue
		}
		key, value, _ := strings.Cut(field, " ")
		if key == "worktree" {
			out = append(out, gitWorktree{Path: value, Main: len(out) == 0})
			cur = &out[len(out)-1]
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "HEAD":
			cur.Head = value
		case "branch":
			cur.Branch = value
		case "detached":
			cur.Detached = true
		case "bare":
			cur.Bare = true
		case "locked":
			cur.Locked = true
		case "prunable":
			cur.Prunable = true
		}
	}
	return out
}

// resolveWorktreeRepo returns the canonical common Git directory and the
// repository's worktrees; the first is the main worktree.
func resolveWorktreeRepo(ctx context.Context, repo string) (string, []gitWorktree, error) {
	common, err := worktreeGit(ctx, repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", nil, err
	}
	common = strings.TrimSpace(common)
	if !filepath.IsAbs(common) {
		common = filepath.Join(repo, common)
	}
	if common, err = filepath.EvalSymlinks(common); err != nil {
		return "", nil, err
	}
	list, err := worktreeGit(ctx, common, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", nil, err
	}
	worktrees := parseWorktreeList(list)
	if len(worktrees) == 0 {
		return "", nil, errors.New("git listed no worktrees")
	}
	return common, worktrees, nil
}

// canonicalPath resolves symlinks in the longest existing prefix, so /tmp and
// /private/tmp compare equal even after the directory is gone.
func canonicalPath(p string) string {
	p = filepath.Clean(p)
	if !filepath.IsAbs(p) {
		return p
	}
	rest := ""
	for dir := p; ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		if dir == filepath.Dir(dir) {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

func pathWithin(child, parent string) bool {
	return child == parent || strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
}

// claudeScratchKey is the directory Claude Code names after a session's
// working directory under its temp root: every non-alphanumeric byte is "-".
func claudeScratchKey(cwd string) string {
	b := []byte(cwd)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// underClaudeScratch reports whether path lies in a scratchpad of a Claude
// session started in cwd: <temp>/claude-<uid>/<key>/<session>/scratchpad/...
func underClaudeScratch(path, cwd string) bool {
	key := claudeScratchKey(cwd)
	parts := strings.Split(path, "/")
	for i := 1; i+2 < len(parts); i++ {
		if parts[i] == key && strings.HasPrefix(parts[i-1], "claude") && parts[i+2] == "scratchpad" {
			return true
		}
	}
	return false
}

// linkedRoot returns the linked worktree containing p, or "" when p lies only
// in the main worktree or outside the repository.
func linkedRoot(p string, linked []string) string {
	best := ""
	for _, root := range linked {
		if pathWithin(p, root) && len(root) > len(best) {
			best = root
		}
	}
	return best
}

func worktreeAdminDir(path string) string {
	data, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		return ""
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return ""
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(path, dir)
	}
	return filepath.Clean(dir)
}

func worktreeOperation(admin string) string {
	for _, name := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG"} {
		if _, err := os.Lstat(filepath.Join(admin, name)); err == nil {
			return name
		}
	}
	return ""
}

func worktreeIdle(admin string, now time.Time) time.Duration {
	newest := time.Time{}
	for _, name := range []string{"HEAD", "index", filepath.Join("logs", "HEAD")} {
		if info, err := os.Stat(filepath.Join(admin, name)); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return 0
	}
	return now.Sub(newest)
}

func worktreeDirty(ctx context.Context, path string) (string, error) {
	status, err := worktreeGit(ctx, path, "status", "--porcelain", "--untracked-files=normal", "--ignore-submodules=none")
	if err != nil {
		return "", err
	}
	status = strings.TrimSpace(status)
	if status == "" {
		return "", nil
	}
	lines := strings.Split(status, "\n")
	return fmt.Sprintf("%d changed or untracked: %s", len(lines), strings.TrimSpace(lines[0])), nil
}

func gitRefExists(ctx context.Context, common, ref string) bool {
	_, err := worktreeGit(ctx, common, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// worktreeIntegrated reports whether head's work is safe without its worktree:
// on tasks-hub (ancestor or patch-equivalent) or contained in a remote ref. A
// commit reachable from no ref never is, since gc could drop a receipt SHA.
func worktreeIntegrated(ctx context.Context, common, head string) (bool, string, error) {
	if head == "" {
		return false, "no HEAD commit", nil
	}
	for _, ref := range []string{"refs/heads/tasks-hub", "refs/remotes/origin/tasks-hub"} {
		if !gitRefExists(ctx, common, ref) {
			continue
		}
		if _, err := worktreeGit(ctx, common, "merge-base", "--is-ancestor", head, ref); err == nil {
			return true, "on " + strings.TrimPrefix(ref, "refs/"), nil
		} else if gitExitCode(err) != 1 {
			return false, "", err
		}
	}
	remote, err := worktreeGit(ctx, common, "for-each-ref", "--count=1", "--format=%(refname)", "--contains", head, "refs/remotes/")
	if err != nil {
		return false, "", err
	}
	if remote = strings.TrimSpace(remote); remote != "" {
		return true, "pushed to " + strings.TrimPrefix(remote, "refs/remotes/"), nil
	}
	local, err := worktreeGit(ctx, common, "for-each-ref", "--count=1", "--format=%(refname)", "--contains", head, "refs/heads/")
	if err != nil {
		return false, "", err
	}
	if strings.TrimSpace(local) == "" {
		return false, "commit " + shortSHA(head) + " is on no branch or remote", nil
	}
	if !gitRefExists(ctx, common, "refs/heads/tasks-hub") {
		return false, "no tasks-hub branch to compare", nil
	}
	cherry, err := worktreeGit(ctx, common, "cherry", "refs/heads/tasks-hub", head)
	if err != nil {
		return false, "", err
	}
	missing := 0
	for _, line := range strings.Split(cherry, "\n") {
		if strings.HasPrefix(line, "+") {
			missing++
		}
	}
	if missing == 0 {
		return true, "patch-equivalent on heads/tasks-hub", nil
	}
	return false, fmt.Sprintf("%d commit(s) not on tasks-hub or any remote", missing), nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// worktreeEvidencePatterns are the spellings a document or receipt may use to
// cite the worktree: absolute, without the macOS /private prefix, relative to
// the main checkout and relative to the home directory.
func worktreeEvidencePatterns(path, main string) []string {
	patterns := []string{path}
	for _, prefix := range []string{"/private/tmp/", "/private/var/"} {
		if strings.HasPrefix(path, prefix) {
			patterns = append(patterns, strings.TrimPrefix(path, "/private"))
		}
	}
	if main != "" && pathWithin(path, main) && path != main {
		if rel, err := filepath.Rel(main, path); err == nil {
			patterns = append(patterns, rel)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && pathWithin(path, home) && path != home {
		patterns = append(patterns, "~"+strings.TrimPrefix(path, home))
	}
	return patterns
}

// docsCitations maps each candidate path to the first tracked docs/ file on
// tasks-hub that cites it, using one git grep over all candidates.
func docsCitations(ctx context.Context, common, main string, paths []string) (map[string]string, error) {
	out := map[string]string{}
	if len(paths) == 0 || !gitRefExists(ctx, common, "refs/heads/tasks-hub") {
		return out, nil
	}
	args := []string{"grep", "-F", "-l"}
	for _, p := range paths {
		for _, pattern := range worktreeEvidencePatterns(p, main) {
			args = append(args, "-e", pattern)
		}
	}
	args = append(args, "refs/heads/tasks-hub", "--", "docs/")
	files, err := worktreeGit(ctx, common, args...)
	if err != nil {
		if gitExitCode(err) == 1 {
			return out, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(files), "\n") {
		if line == "" {
			continue
		}
		content, err := worktreeGit(ctx, common, "show", line)
		if err != nil {
			return nil, err
		}
		file := strings.TrimPrefix(line, "refs/heads/tasks-hub:")
		for _, p := range paths {
			if out[p] != "" {
				continue
			}
			for _, pattern := range worktreeEvidencePatterns(p, main) {
				if strings.Contains(content, pattern) {
					out[p] = file
					break
				}
			}
		}
	}
	return out, nil
}

func worktreeBytes(root string, skip map[string]bool) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && p != root && skip[p] {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// makeWorktreeWritable adds owner rwx to directories under root that lack it,
// such as the Go module cache's read-only directories, so git can delete
// them. It never follows symlinks and never leaves root. The returned
// function restores the original modes, for a removal that then fails.
func makeWorktreeWritable(root string) (func(), error) {
	type change struct {
		path string
		perm fs.FileMode
	}
	var changed []change
	restore := func() {
		for i := len(changed) - 1; i >= 0; i-- {
			_ = os.Chmod(changed[i].path, changed[i].perm)
		}
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || !pathWithin(p, root) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if perm := info.Mode().Perm(); perm&0700 != 0700 {
			if err := os.Chmod(p, perm|0700); err != nil {
				return err
			}
			changed = append(changed, change{p, perm})
		}
		return nil
	})
	if err != nil {
		restore()
		return nil, err
	}
	return restore, nil
}

// nestedRepository returns the first Git repository or worktree below root:
// any `.git` entry other than root's own. Git's worktree remove deletes
// ignored content recursively, including such a repository and its unpushed
// work. Linked worktrees in cleared go earlier in the same pass and are
// skipped.
func nestedRepository(root string, cleared map[string]bool) (string, error) {
	found := ""
	own := filepath.Join(root, ".git")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		if d.IsDir() && cleared[p] {
			return filepath.SkipDir
		}
		if d.Name() == ".git" && p != own {
			found = filepath.Dir(p)
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}

// removeWorktree re-checks a removable worktree immediately before removing
// it and returns a keep reason when anything changed since classification.
func removeWorktree(ctx context.Context, common string, w gitWorktree, admin string, cleared map[string]bool) (string, string) {
	if admin == "" {
		return keepMoved, "worktree admin directory is unreadable"
	}
	if _, err := os.Lstat(filepath.Join(admin, "locked")); err == nil {
		return keepLocked, "locked since classification"
	}
	if op := worktreeOperation(admin); op != "" {
		return keepOperation, op + " in progress"
	}
	dirty, err := worktreeDirty(ctx, w.Path)
	if err != nil {
		return keepFailed, err.Error()
	}
	if dirty != "" {
		return keepDirty, dirty
	}
	head, err := worktreeGit(ctx, w.Path, "rev-parse", "HEAD")
	if err != nil {
		return keepFailed, err.Error()
	}
	if strings.TrimSpace(head) != w.Head {
		return keepMoved, "HEAD moved to " + shortSHA(strings.TrimSpace(head))
	}
	nested, err := nestedRepository(w.Path, cleared)
	if err != nil {
		return keepFailed, "scan for nested repositories: " + err.Error()
	}
	if nested != "" {
		return keepNested, "contains Git repository " + nested
	}
	restore, err := makeWorktreeWritable(w.Path)
	if err != nil {
		return keepFailed, "make caches removable: " + err.Error()
	}
	if _, err := worktreeGit(ctx, common, "worktree", "remove", w.Path); err != nil {
		restore()
		return keepFailed, err.Error()
	}
	return "", ""
}

// cleanupWorktrees classifies the selected worktrees deepest first and, when
// applying, removes the removable ones and prunes missing ones. Every
// decision is returned; applied decisions are also recorded as receipts.
func cleanupWorktrees(ctx context.Context, in worktreeCleanupInputs) ([]worktreeDecision, error) {
	common, worktrees, err := resolveWorktreeRepo(ctx, in.Repo)
	if err != nil {
		return nil, err
	}
	main := ""
	if !worktrees[0].Bare {
		main = canonicalPath(worktrees[0].Path)
	}
	var linked []string
	for i := range worktrees {
		worktrees[i].Path = canonicalPath(worktrees[i].Path)
		if !worktrees[i].Main && !worktrees[i].Bare {
			linked = append(linked, worktrees[i].Path)
		}
	}
	var candidates []gitWorktree
	for _, w := range worktrees {
		if w.Main || w.Bare {
			continue
		}
		if in.Select == nil || in.Select(w, main, linked) {
			candidates = append(candidates, w)
		}
	}
	// Deepest first, so a removable child goes before its parent and a kept
	// child keeps its parent.
	sort.SliceStable(candidates, func(i, j int) bool {
		di, dj := strings.Count(candidates[i].Path, "/"), strings.Count(candidates[j].Path, "/")
		if di != dj {
			return di > dj
		}
		return candidates[i].Path < candidates[j].Path
	})
	paths := make([]string, len(candidates))
	for i, w := range candidates {
		paths[i] = w.Path
	}
	cited, err := docsCitations(ctx, common, main, paths)
	if err != nil {
		return nil, fmt.Errorf("evidence lookup: %w", err)
	}
	inUse := make([]string, 0, len(in.InUse))
	for _, p := range in.InUse {
		if p != "" && filepath.IsAbs(p) {
			inUse = append(inUse, canonicalPath(p))
		}
	}
	linkedSet := map[string]bool{}
	for _, p := range linked {
		linkedSet[p] = true
	}
	var decisions []worktreeDecision
	// cleared holds worktrees removed or pruned (or, in a dry run, that
	// would be) earlier in this pass; only they may sit inside a removal.
	cleared := map[string]bool{}
	prune := false
	blockerChecked, blocker := false, ""
	for _, w := range candidates {
		d := worktreeDecision{Path: w.Path, Branch: strings.TrimPrefix(w.Branch, "refs/heads/"), Head: w.Head}
		admin := worktreeAdminDir(w.Path)
		reason, detail := classifyWorktree(ctx, common, main, w, admin, in, inUse, linked, cited, cleared)
		if reason == keepMissing && !blockerChecked {
			blockerChecked = true
			if blocker, err = pruneBlocker(ctx, common, worktrees); err != nil {
				return nil, fmt.Errorf("prune check: %w", err)
			}
		}
		switch {
		case reason == keepMissing && blocker != "":
			d.Action, d.Reason, d.Detail = "kept", keepMissing, "prune deferred: gone worktree "+blocker+" holds an unintegrated commit"
		case reason == keepMissing:
			d.Action = "would-prune"
			if in.Apply {
				d.Action = "pruned"
				prune = true
			}
			d.Reason, d.Detail = reason, detail
		case reason != "":
			d.Action, d.Reason, d.Detail = "kept", reason, detail
		default:
			if in.MeasureBytes {
				d.Bytes = worktreeBytes(w.Path, linkedSet)
			}
			d.Action, d.Detail = "would-remove", detail
			if in.Apply {
				if reason, why := removeWorktree(ctx, common, w, admin, cleared); reason != "" {
					d.Action, d.Reason, d.Detail, d.Bytes = "kept", reason, why, 0
				} else {
					d.Action = "removed"
				}
			}
		}
		if d.Action != "kept" {
			cleared[w.Path] = true
		}
		decisions = append(decisions, d)
	}
	if prune {
		if _, err := worktreeGit(ctx, common, "worktree", "prune"); err != nil {
			for i := range decisions {
				if decisions[i].Action == "pruned" {
					decisions[i].Action, decisions[i].Reason, decisions[i].Detail = "kept", keepFailed, err.Error()
				}
			}
		}
	}
	if in.Apply {
		now := in.Now
		if now.IsZero() {
			now = time.Now()
		}
		for i := range decisions {
			recorded, err := recordWorktreeDecision(in.Receipt, decisions[i], now)
			if err != nil {
				return decisions, fmt.Errorf("worktree cleanup receipt: %w", err)
			}
			decisions[i].recorded = recorded
		}
	}
	return decisions, nil
}

func worktreeGone(w gitWorktree) bool {
	info, err := os.Stat(w.Path)
	return w.Prunable || err != nil || !info.IsDir()
}

// pruneBlocker names a gone worktree whose HEAD is not integrated. Git prunes
// every gone worktree at once, so one such entry defers all pruning.
func pruneBlocker(ctx context.Context, common string, worktrees []gitWorktree) (string, error) {
	for _, w := range worktrees {
		if w.Main || w.Bare || w.Locked || !worktreeGone(w) {
			continue
		}
		ok, _, err := worktreeIntegrated(ctx, common, w.Head)
		if err != nil {
			return "", err
		}
		if !ok {
			return w.Path, nil
		}
	}
	return "", nil
}

// classifyWorktree returns the first keep reason, or "" with the integration
// detail when the worktree is removable.
func classifyWorktree(ctx context.Context, common, main string, w gitWorktree, admin string, in worktreeCleanupInputs, inUse, linked []string, cited map[string]string, cleared map[string]bool) (string, string) {
	if w.Locked {
		return keepLocked, "git worktree is locked"
	}
	if worktreeGone(w) {
		// Pruning drops the worktree's HEAD, a garbage-collection root.
		ok, detail, err := worktreeIntegrated(ctx, common, w.Head)
		if err != nil {
			return keepFailed, err.Error()
		}
		if !ok {
			return keepUnpushed, "directory is gone; " + detail
		}
		return keepMissing, "worktree directory is gone"
	}
	if admin == "" {
		return keepFailed, "worktree has no readable .git link"
	}
	for _, p := range inUse {
		// A path in use keeps the worktree holding it, and the worktrees
		// nested in it when it is itself inside a linked worktree.
		if pathWithin(p, w.Path) || (pathWithin(w.Path, p) && linkedRoot(p, linked) != "") {
			return keepInUse, "in use at " + p
		}
		// Sessions key scratchpads by their start directory, which may be
		// the worktree root above the path in use.
		for _, cwd := range []string{p, linkedRoot(p, linked)} {
			if cwd != "" && underClaudeScratch(w.Path, cwd) {
				return keepInUse, "in a scratchpad of a session in " + cwd
			}
		}
	}
	// Every linked worktree inside it counts, selected or not, unless it
	// goes first in this pass or is already gone.
	for _, child := range linked {
		if child == w.Path || !pathWithin(child, w.Path) || cleared[child] {
			continue
		}
		if info, err := os.Stat(child); err == nil && info.IsDir() {
			return keepNested, "contains worktree " + child
		}
	}
	if in.MinIdle > 0 {
		now := in.Now
		if now.IsZero() {
			now = time.Now()
		}
		if idle := worktreeIdle(admin, now); idle < in.MinIdle {
			return keepRecent, fmt.Sprintf("Git state changed %s ago", idle.Round(time.Minute))
		}
	}
	if file := cited[w.Path]; file != "" {
		return keepEvidence, "cited by " + file
	}
	for _, ev := range in.Evidence {
		for _, pattern := range worktreeEvidencePatterns(w.Path, main) {
			if strings.Contains(ev.Text, pattern) {
				return keepEvidence, "cited by " + ev.Source
			}
		}
	}
	if op := worktreeOperation(admin); op != "" {
		return keepOperation, op + " in progress"
	}
	dirty, err := worktreeDirty(ctx, w.Path)
	if err != nil {
		return keepFailed, err.Error()
	}
	if dirty != "" {
		return keepDirty, dirty
	}
	ok, detail, err := worktreeIntegrated(ctx, common, w.Head)
	if err != nil {
		return keepFailed, err.Error()
	}
	if !ok {
		return keepUnpushed, detail
	}
	if !in.Apply {
		// Applying repeats this scan as part of the removal re-check.
		nested, err := nestedRepository(w.Path, cleared)
		if err != nil {
			return keepFailed, "scan for nested repositories: " + err.Error()
		}
		if nested != "" {
			return keepNested, "contains Git repository " + nested
		}
	}
	return "", detail
}

// activeQueueEntry reports whether an entry still holds its worktree: it is
// queued, launching or running, or failed and not yet released.
func activeQueueEntry(q api.TeamQueueEntry) bool {
	switch q.State {
	case "queued", "launching", "running":
		return true
	case "failed":
		return q.ReleasedAt == ""
	}
	return false
}

// worktreeProtection collects in-use paths and cited evidence from queue
// entries and agents on this host.
func worktreeProtection(host string, entries []api.TeamQueueEntry, agents []api.Agent) ([]string, []worktreeEvidence) {
	var inUse []string
	var evidence []worktreeEvidence
	for _, q := range entries {
		if q.Host == host && activeQueueEntry(q) {
			inUse = append(inUse, q.Cwd)
			if q.Acceptance != nil {
				inUse = append(inUse, q.Acceptance.Worktree)
			}
		}
		if q.Acceptance != nil && q.Acceptance.Evidence != "" {
			evidence = append(evidence, worktreeEvidence{Source: "queue entry " + q.ID + " acceptance evidence", Text: q.Acceptance.Evidence})
		}
		if q.Integration != nil && q.Integration.Evidence != "" {
			evidence = append(evidence, worktreeEvidence{Source: "queue entry " + q.ID + " integration evidence", Text: q.Integration.Evidence})
		}
	}
	for _, a := range agents {
		if a.Host == host && a.Status != api.AgentClosed && a.Cwd != "" {
			inUse = append(inUse, a.Cwd)
		}
	}
	return inUse, evidence
}

// closeoutWorktrees removes a finished entry's worktrees once their work is
// on tasks-hub or pushed. It reads the hub only when one of the entry's
// worktrees still exists, at most once per closeoutWorktreeInterval, and
// never writes to the hub.
func closeoutWorktrees(ctx context.Context, c *api.Client, host string, active, project api.TeamQueueList, q api.TeamQueueEntry) error {
	direct := []string{q.Cwd, q.Acceptance.Worktree}
	if q.Integration != nil {
		direct = append(direct, q.Integration.Worktree)
	}
	exists := false
	for _, p := range direct {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			exists = true
		}
	}
	if !exists {
		return nil
	}
	now := time.Now()
	if last, ok := closeoutWorktreeChecks.Load(q.ID); ok && now.Sub(last.(time.Time)) < closeoutWorktreeInterval {
		return nil
	}
	closeoutWorktreeChecks.Store(q.ID, now)
	detail, err := c.GetTask(ctx, q.TaskID)
	if err != nil {
		return err
	}
	entries := append(append([]api.TeamQueueEntry(nil), active.Entries...), project.Entries...)
	inUse, evidence := worktreeProtection(host, entries, detail.Agents)
	roots := append([]string(nil), direct...)
	for _, a := range detail.Agents {
		if a.WorkItem != nil && a.WorkItem.ItemTaskID == q.TaskID && a.WorkItem.ItemID == q.ItemID && a.Host == host {
			roots = append(roots, a.Cwd)
		}
	}
	var directPaths []string
	for _, p := range direct {
		if p != "" {
			directPaths = append(directPaths, canonicalPath(p))
		}
	}
	for i := range roots {
		if roots[i] != "" {
			roots[i] = canonicalPath(roots[i])
		}
	}
	repo := q.Repository
	if repo == "" {
		repo = q.Acceptance.Repository
	}
	if repo == "" {
		for _, p := range direct {
			if _, err := os.Stat(p); p != "" && err == nil {
				repo = p
				break
			}
		}
	}
	lock, err := worktreeCleanupLock()
	if err != nil {
		return err
	}
	defer unlockQueueLaunch(lock)
	decisions, err := cleanupWorktrees(ctx, worktreeCleanupInputs{
		Repo: repo,
		// Only paths attributable to this item: its recorded worktrees and,
		// when a recorded worktree or team cwd is a linked worktree, what is
		// nested in it or in scratchpads of sessions started there. A
		// main-checkout cwd is shared, so it attributes nothing further.
		Select: func(w gitWorktree, main string, linked []string) bool {
			for _, p := range directPaths {
				if w.Path == p {
					return true
				}
			}
			for _, root := range roots {
				if root == "" {
					continue
				}
				owner := linkedRoot(root, linked)
				if owner == "" {
					continue
				}
				if pathWithin(w.Path, owner) || underClaudeScratch(w.Path, root) {
					return true
				}
			}
			return false
		},
		InUse:    inUse,
		Evidence: evidence,
		Now:      now,
		Apply:    true,
		Receipt:  worktreeCleanupReceipt{Source: "closeout", TaskID: q.TaskID, EntryID: q.ID, ItemID: q.ItemID},
	})
	for _, d := range decisions {
		if !d.recorded {
			continue
		}
		if d.Action == "kept" {
			fmt.Fprintf(os.Stderr, "[tt relay] worktree cleanup %s: kept %s %s: %s\n", q.ID, d.Reason, d.Path, d.Detail)
		} else {
			fmt.Fprintf(os.Stderr, "[tt relay] worktree cleanup %s: %s %s\n", q.ID, d.Action, d.Path)
		}
	}
	return err
}

type worktreeSweepTotals struct {
	Actions map[string]int `json:"actions"`
	Kept    map[string]int `json:"kept"`
	Bytes   int64          `json:"bytes"`
}

type worktreeSweepReport struct {
	Worktrees []worktreeDecision  `json:"worktrees"`
	Totals    worktreeSweepTotals `json:"totals"`
}

// cmdTeamQueueSweepWorktrees applies the closeout rules to every existing
// linked worktree of the repository. It is a dry run unless --apply is set,
// and reads the hub before touching anything.
func cmdTeamQueueSweepWorktrees(e env, args []string) error {
	fs := flag.NewFlagSet("team queue sweep-worktrees", flag.ContinueOnError)
	hub := fs.String("hub", e.hub, "hub URL")
	cwd := fs.String("cwd", "", "any worktree of the repository (default: current directory)")
	apply := fs.Bool("apply", false, "remove the would-remove worktrees and prune missing ones")
	jsonOut := fs.Bool("json", false, "print JSON")
	minIdle := fs.Duration("min-idle", 24*time.Hour, "keep worktrees whose Git state changed more recently")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *hub == "" || *minIdle < 0 {
		return errors.New("usage: tt team queue sweep-worktrees [--apply] [--json] [--min-idle 24h] [--cwd DIR] [--hub URL]")
	}
	repo := *cwd
	if repo == "" {
		var err error
		if repo, err = os.Getwd(); err != nil {
			return err
		}
	}
	e.hub = *hub
	c, err := e.client(20 * time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	host := spawn.Host()
	// Every protection input is read before any worktree is touched; a hub
	// failure stops the sweep.
	tasks, err := c.ListTasks(ctx)
	if err != nil {
		return fmt.Errorf("sweep needs the hub: %w", err)
	}
	var entries []api.TeamQueueEntry
	var agents []api.Agent
	for _, task := range tasks {
		detail, err := c.GetTask(ctx, task.ID)
		if err != nil {
			return fmt.Errorf("sweep needs the hub: project %s: %w", task.ID, err)
		}
		agents = append(agents, detail.Agents...)
		queue, err := c.ListTeamQueue(ctx, task.ID)
		if err != nil {
			return fmt.Errorf("sweep needs the hub: project %s queue: %w", task.ID, err)
		}
		entries = append(entries, queue.Entries...)
	}
	inUse, evidence := worktreeProtection(host, entries, agents)
	if *apply {
		lock, err := worktreeCleanupLock()
		if err != nil {
			return err
		}
		defer unlockQueueLaunch(lock)
	}
	decisions, err := cleanupWorktrees(ctx, worktreeCleanupInputs{
		Repo: repo, InUse: inUse, Evidence: evidence, MinIdle: *minIdle, Now: time.Now(),
		Apply: *apply, MeasureBytes: true, Receipt: worktreeCleanupReceipt{Source: "sweep"},
	})
	if decisions == nil && err != nil {
		return err
	}
	report := worktreeSweepReport{Worktrees: decisions, Totals: worktreeSweepTotals{Actions: map[string]int{}, Kept: map[string]int{}}}
	if report.Worktrees == nil {
		report.Worktrees = []worktreeDecision{}
	}
	for _, d := range decisions {
		report.Totals.Actions[d.Action]++
		if d.Action == "kept" {
			report.Totals.Kept[d.Reason]++
		}
		report.Totals.Bytes += d.Bytes
	}
	if *jsonOut {
		printJSON(report)
		return err
	}
	for _, d := range decisions {
		switch d.Action {
		case "kept":
			fmt.Printf("kept %s %s: %s\n", d.Reason, d.Path, d.Detail)
		case "removed", "would-remove":
			fmt.Printf("%s %s (%s, %s)\n", d.Action, d.Path, worktreeLabel(d), humanBytes(d.Bytes))
		default:
			fmt.Printf("%s %s\n", d.Action, d.Path)
		}
	}
	var parts []string
	for _, action := range []string{"removed", "would-remove", "pruned", "would-prune", "kept"} {
		if n := report.Totals.Actions[action]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", action, n))
		}
	}
	reasons := make([]string, 0, len(report.Totals.Kept))
	for reason, n := range report.Totals.Kept {
		reasons = append(reasons, fmt.Sprintf("%s=%d", reason, n))
	}
	sort.Strings(reasons)
	fmt.Printf("totals: %s; kept by reason: %s; %s %s\n", strings.Join(parts, " "), strings.Join(reasons, " "), map[bool]string{true: "removed", false: "reclaimable"}[*apply], humanBytes(report.Totals.Bytes))
	if !*apply {
		fmt.Println("dry run: nothing changed; rerun with --apply to remove the would-remove worktrees")
	}
	return err
}

func worktreeLabel(d worktreeDecision) string {
	label := "detached " + shortSHA(d.Head)
	if d.Branch != "" {
		label = "branch " + d.Branch
	}
	if d.Detail != "" {
		label += "; " + d.Detail
	}
	return label
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
