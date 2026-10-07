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
	"regexp"
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
	// Artifact checkouts only: the item still runs, or it is neither
	// released nor accepted long enough.
	keepItemActive = "item-active"
	keepRetention  = "retention"
	// Cache trim only: the directory is not provably a regenerable cache the
	// harness may delete, or it touches a path on the never-touch list.
	keepUnproven  = "unproven"
	keepProtected = "protected"
)

// Decision and receipt kinds; an ordinary worktree has none.
const (
	kindArtifactCheckout = "artifact-checkout"
	kindSessionTemp      = "session-temp"
	// A regenerable cache inside a kept checkout; its actions are
	// would-trim and trimmed.
	kindCache = "cache"
)

// defaultArtifactAcceptedAfter is how long an accepted, unreleased item keeps
// its verifier checkouts.
const defaultArtifactAcceptedAfter = 24 * time.Hour

// maxArtifactTextBytes bounds one receipt or plan file read for citations.
const maxArtifactTextBytes = 4 << 20

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
	// Kind is "artifact-checkout", "session-temp" or "cache"; an ordinary
	// worktree has none.
	Kind     string `json:"kind,omitempty"`
	Manifest string `json:"manifest,omitempty"`
	// recorded marks a decision whose receipt this call wrote.
	recorded bool
	// strayRoot is set on a checkout kept for lying outside the artifacts
	// root: the root in use, or "none".
	strayRoot string
}

type worktreeEvidence struct {
	Source string
	Text   string
	// artifactOnly evidence is read for artifact checkouts and session temp
	// only; ordinary worktrees keep the sources they always had.
	artifactOnly bool
}

// sessionTempCwd is a directory a team session may have started in, with the
// item whose receipt and plan files may cite its temp folder.
type sessionTempCwd struct {
	Path   string
	ItemID string
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
	// Items turns on the artifact checkout rules: the state of every item
	// with a queue record. Nil leaves every worktree under the ordinary rules.
	Items map[string]artifactItemState
	// Artifacts overrides the artifacts root (default: artifactsRootFor).
	Artifacts string
	// AcceptedAfter is how long an accepted, unreleased item keeps its
	// checkouts; zero means the default (see acceptedAfterInput).
	AcceptedAfter time.Duration
	// TempRoot is the Claude session temp root; "" skips session temp.
	TempRoot string
	TempCwds []sessionTempCwd
	// Exclusive, when set, runs each removal's final re-check and its
	// removal or detach while no matrix run can register: it calls act with
	// the paths matrix runs hold at that moment. It returns a keep reason
	// when it refuses, and then act has not run; with no reason, its second
	// value is a note for the receipt, such as a removal that outran the
	// hold limit. Nil runs the removal directly, as closeout does.
	Exclusive func(act func(live []string)) (string, string)
	// Trim turns on the cache trim for the finished checkouts it names
	// (sweep only); nil trims nothing.
	Trim *cacheTrimSet
	// Tombstones makes an applying pass detach session temp folders by a
	// rename before deleting them, and finish or drop what an interrupted
	// pass left behind (sweep only).
	Tombstones bool
}

// exclusive runs act under in.Exclusive, or directly without one. inUse is
// the in-use rule for what act removes: a path a matrix run registered since
// classification keeps it, and act does not run. It returns the keep reason
// and its detail, or "" and the exclusion's note about a removal that ran.
func (in worktreeCleanupInputs) exclusive(inUse func(p string) string, act func()) (string, string) {
	if in.Exclusive == nil {
		act()
		return "", ""
	}
	reason, detail := "", ""
	held, note := in.Exclusive(func(live []string) {
		for _, p := range live {
			if detail = inUse(p); detail != "" {
				reason, detail = keepInUse, detail+", registered by a matrix run since classification"
				return
			}
		}
		if exclusiveAfterCheck != nil {
			exclusiveAfterCheck()
		}
		act()
	})
	if held != "" {
		return held, note
	}
	if reason != "" {
		return reason, detail
	}
	return "", note
}

// worktreeInUseAt says how a path in use keeps the worktree at path, or "".
func worktreeInUseAt(path, p string, linked []string) string {
	// A path in use keeps the worktree holding it, and the worktrees
	// nested in it when it is itself inside a linked worktree.
	if pathWithin(p, path) || (pathWithin(path, p) && linkedRoot(p, linked) != "") {
		return "in use at " + p
	}
	// Sessions key scratchpads by their start directory, which may be
	// the worktree root above the path in use.
	for _, cwd := range []string{p, linkedRoot(p, linked)} {
		if cwd != "" && underClaudeScratch(path, cwd) {
			return "in a scratchpad of a session in " + cwd
		}
	}
	return ""
}

// sessionTempInUseAt says how a path in use keeps the session temp folder,
// or "".
func sessionTempInUseAt(folder, p string, linked []string) string {
	key := filepath.Base(folder)
	// The key is lossy: directories that differ only in punctuation share a
	// folder, so an equal key counts as in use.
	if claudeScratchKey(p) == key {
		return "a session directory in use has this key: " + p
	}
	// Sessions key their folder by their start directory, which may be
	// the worktree root above the path in use.
	if root := linkedRoot(p, linked); root != "" && claudeScratchKey(root) == key {
		return "a session started in " + root + " has a directory in use: " + p
	}
	if pathWithin(p, folder) {
		return "in use at " + p
	}
	return ""
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
	// Kind, Bytes and Manifest are set for artifact checkouts, session temp
	// folders and caches.
	Kind     string `json:"kind,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	Manifest string `json:"manifest,omitempty"`
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

// errWorktreeCleanupActive is the cleanup lock held by another closeout or
// sweep on this host.
var errWorktreeCleanupActive = errors.New("worktree cleanup is active on this host")

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
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %w", errWorktreeCleanupActive, err)
		}
		return nil, fmt.Errorf("worktree cleanup lock: %w", err)
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
	if d.Kind != "" {
		r.Kind, r.Bytes, r.Manifest = d.Kind, d.Bytes, d.Manifest
	}
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
// tasks-hub that cites it, using one git grep over all candidates. A strict
// path is an artifact checkout: only a citation of something inside it counts.
func docsCitations(ctx context.Context, common, main string, paths []string, strict map[string]bool) (map[string]string, error) {
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
			if strict[p] {
				if rel := artifactCitation(p, main, content); rel != "" {
					out[p] = file + ": " + rel
				}
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
// applying, removes the removable ones and prunes missing ones, then does the
// same for the session temp folders of in.TempCwds. Every decision is
// returned; applied decisions are also recorded as receipts.
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
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	art := newArtifactPass(in, main, linked)
	temp := sessionTempCandidates(in, main, linked)
	paths := make([]string, len(candidates), len(candidates)+len(temp))
	strict := map[string]bool{}
	for i, w := range candidates {
		paths[i] = w.Path
		if art.checkoutItem(w) != "" {
			strict[w.Path] = true
		}
	}
	for _, t := range temp {
		paths = append(paths, t.Folder)
	}
	cited, err := docsCitations(ctx, common, main, paths, strict)
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
		item := art.checkoutItem(w)
		if item != "" {
			d.Kind = kindArtifactCheckout
		}
		reason, detail := classifyWorktree(ctx, common, main, w, admin, in, inUse, linked, cited, cleared, art)
		if strayReason, strayDetail, root := strayCheckout(w, in, art); strayReason != "" && reason == strayReason && detail == strayDetail {
			d.strayRoot = root
		}
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
				var reason, why, manifest string
				bytes := d.Bytes
				held, heldWhy := in.exclusive(func(p string) string { return worktreeInUseAt(w.Path, p, linked) }, func() {
					if item != "" {
						// A verifier checkout leaves a manifest beside its receipts.
						reason, why, manifest, bytes = removeArtifactCheckout(ctx, common, w, admin, cleared, item, art.items[item], in.Receipt, now)
						return
					}
					reason, why = removeWorktree(ctx, common, w, admin, cleared)
				})
				if held != "" {
					reason, why = held, heldWhy
				}
				if reason != "" {
					d.Action, d.Reason, d.Detail, d.Bytes = "kept", reason, why, 0
				} else {
					d.Action, d.Manifest, d.Bytes = "removed", manifest, bytes
					if heldWhy != "" {
						d.Detail = strings.TrimPrefix(d.Detail+"; "+heldWhy, "; ")
					}
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
	if in.Apply && in.Tombstones {
		decisions = append(decisions, recoverDetachIntents(in, main, art)...)
	}
	decisions = append(decisions, trimCaches(ctx, in, common, main, candidates, decisions, linked, art, now)...)
	decisions = append(decisions, cleanupSessionTemp(in, main, temp, inUse, linked, cited, cleared, art, now)...)
	if in.Apply {
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
func classifyWorktree(ctx context.Context, common, main string, w gitWorktree, admin string, in worktreeCleanupInputs, inUse, linked []string, cited map[string]string, cleared map[string]bool, art *artifactPass) (string, string) {
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
		if detail := worktreeInUseAt(w.Path, p, linked); detail != "" {
			return keepInUse, detail
		}
	}
	// A verifier checkout under the artifacts tree waits for its item: it
	// goes only after the release, or once the acceptance is old enough.
	item := art.checkoutItem(w)
	eligible := ""
	if item != "" {
		reason, detail := art.gate(item)
		if reason != "" {
			return reason, detail
		}
		eligible = detail
	} else if reason, detail, _ := strayCheckout(w, in, art); reason != "" {
		return reason, detail
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
	if item != "" {
		// Its receipts, logs and plan files sit beside it, so only a
		// citation of something inside the checkout keeps it.
		if reason, detail := art.cited(w.Path, main, item, in.Evidence); reason != "" {
			return reason, detail
		}
	} else {
		for _, ev := range in.Evidence {
			if ev.artifactOnly {
				continue
			}
			for _, pattern := range worktreeEvidencePatterns(w.Path, main) {
				if strings.Contains(ev.Text, pattern) {
					return keepEvidence, "cited by " + ev.Source
				}
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
	// A verifier checkout is a clean detached copy of a candidate whose own
	// branch stays under the ordinary rules, so it need not be integrated;
	// removal pins a HEAD that no branch or remote holds.
	detail := eligible
	if item == "" {
		ok, why, err := worktreeIntegrated(ctx, common, w.Head)
		if err != nil {
			return keepFailed, err.Error()
		}
		if !ok {
			return keepUnpushed, why
		}
		detail = why
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

// Verifier checkouts and session temp.
//
// A verifier checkout is a detached linked worktree under
// <artifacts>/<itemId>/. Its receipts, logs and plan files sit beside it, so
// the checkout directory is the unit: once its item is released, or accepted
// long enough, a clean checkout is removed whole and a manifest of what went
// is written next to it. A session temp folder is Claude Code's per-directory
// folder under its temp root, removed once no session can still be using it.

var artifactItemIDPattern = regexp.MustCompile(`^wi_[0-9a-f]{16}$`)

// artifactsRootFor names the artifacts tree of the repository whose main
// checkout is main: TAILTERM_ARTIFACTS, else the sibling <main>-artifacts. A
// TAILTERM_ARTIFACTS that is not absolute names no tree: it says so once and
// returns "", so no verifier checkout is examined and session temp is kept.
// Tests replace it.
var artifactsRootFor = func(main string) string {
	if dir := os.Getenv("TAILTERM_ARTIFACTS"); dir != "" {
		if !filepath.IsAbs(dir) {
			artifactsRootWarning.Do(func() {
				fmt.Fprintf(os.Stderr, "[tt relay] TAILTERM_ARTIFACTS=%q is not an absolute path; verifier checkouts and session temp are left alone\n", dir)
			})
			return ""
		}
		return dir
	}
	if main == "" {
		return ""
	}
	return main + "-artifacts"
}

// claudeTempRoot is where Claude Code keeps one folder per session
// directory: <temp>/claude-<uid>, with /tmp (or CLAUDE_CODE_TMPDIR) as temp.
// Tests replace it.
var claudeTempRoot = func() string {
	base := os.Getenv("CLAUDE_CODE_TMPDIR")
	if base == "" {
		base = "/tmp"
	}
	return canonicalPath(filepath.Join(base, fmt.Sprintf("claude-%d", os.Getuid())))
}

// artifactsRootWarning limits the relative TAILTERM_ARTIFACTS notice to one
// line per process. Tests reset it.
var artifactsRootWarning sync.Once

var artifactAcceptedAfterWarning sync.Once

// artifactAcceptedAfter is closeout's acceptance age, from
// TAILTERM_ARTIFACT_ACCEPTED_AFTER (a Go duration) or the default.
func artifactAcceptedAfter() time.Duration {
	raw := os.Getenv("TAILTERM_ARTIFACT_ACCEPTED_AFTER")
	if raw == "" {
		return defaultArtifactAcceptedAfter
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		artifactAcceptedAfterWarning.Do(func() {
			fmt.Fprintf(os.Stderr, "[tt relay] TAILTERM_ARTIFACT_ACCEPTED_AFTER=%q is not a non-negative duration; using %s\n", raw, shortDuration(defaultArtifactAcceptedAfter))
		})
		return defaultArtifactAcceptedAfter
	}
	return acceptedAfterInput(d)
}

// acceptedAfterInput turns an explicit duration into the cleanup input, where
// an unset (zero) field means the default: an explicit zero, "no wait",
// becomes the smallest positive duration.
func acceptedAfterInput(d time.Duration) time.Duration {
	if d == 0 {
		return time.Nanosecond
	}
	return d
}

// shortDuration prints whole minutes without trailing zero units: 24h, 1h30m.
func shortDuration(d time.Duration) string {
	s := strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	if s == "" {
		return "0m"
	}
	return s
}

// Item states for artifact checkouts.
const (
	artifactItemRunning  = "running"
	artifactItemReleased = "released"
	artifactItemAccepted = "accepted"
)

// artifactItemState is what the queue records say about one item. An item
// with no record has no state and is never treated as released.
type artifactItemState struct {
	State string
	// At is the acceptance time of an accepted item.
	At time.Time
	// BasisAt is the recorded time of the release or acceptance.
	BasisAt string
	EntryID string
	Detail  string
	// position orders finished entries; the newest decides.
	position int64
}

func releasedQueueEntry(q api.TeamQueueEntry) bool {
	return q.OwnerIntegration != nil || (q.Release != nil && (q.Release.State == "released" || q.Release.Published))
}

// artifactItemStates derives each item's state from every queue entry and
// agent, on any host: running while an entry is active or a bound agent is
// not closed; otherwise what its newest finished entry says, released or
// accepted.
func artifactItemStates(entries []api.TeamQueueEntry, agents []api.Agent) map[string]artifactItemState {
	out := map[string]artifactItemState{}
	for _, q := range entries {
		if q.ItemID == "" {
			continue
		}
		cur, known := out[q.ItemID]
		if activeQueueEntry(q) {
			out[q.ItemID] = artifactItemState{State: artifactItemRunning, EntryID: q.ID, Detail: "queue entry " + q.ID + " is " + q.State}
			continue
		}
		if cur.State == artifactItemRunning || q.State != "finished" {
			continue
		}
		var next artifactItemState
		switch {
		case releasedQueueEntry(q):
			next = artifactItemState{State: artifactItemReleased, BasisAt: q.UpdatedAt, EntryID: q.ID, position: q.Position}
			if q.OwnerIntegration != nil && q.OwnerIntegration.At != "" {
				next.BasisAt = q.OwnerIntegration.At
			}
		case q.Acceptance != nil:
			at, err := time.Parse(time.RFC3339Nano, q.Acceptance.AcceptedAt)
			if err != nil {
				continue
			}
			next = artifactItemState{State: artifactItemAccepted, At: at, BasisAt: q.Acceptance.AcceptedAt, EntryID: q.ID, position: q.Position}
		default:
			continue
		}
		// The newest finished entry decides; on a tie the accepted one, which
		// waits longer.
		if !known || next.position > cur.position || (next.position == cur.position && next.State == artifactItemAccepted) {
			out[q.ItemID] = next
		}
	}
	for _, a := range agents {
		if a.WorkItem == nil || a.WorkItem.ItemID == "" || a.Status == api.AgentClosed {
			continue
		}
		name := a.Name
		if name == "" {
			name = a.ID
		}
		out[a.WorkItem.ItemID] = artifactItemState{State: artifactItemRunning, Detail: "agent " + name + " is " + string(a.Status)}
	}
	return out
}

// artifactPass holds one cleanup pass's artifact rules. A nil pass means the
// rules are off and every worktree is an ordinary one.
type artifactPass struct {
	root string
	// custom marks a root that is not the default <main>-artifacts: it came
	// from --artifacts or TAILTERM_ARTIFACTS and may be mistaken.
	custom        bool
	items         map[string]artifactItemState
	acceptedAfter time.Duration
	now           time.Time
	linked        map[string]bool
	texts         map[string][]worktreeEvidence
	textFault     map[string]string
}

func newArtifactPass(in worktreeCleanupInputs, main string, linked []string) *artifactPass {
	if in.Items == nil {
		return nil
	}
	root := in.Artifacts
	if root == "" {
		root = artifactsRootFor(main)
	}
	if root == "" || !filepath.IsAbs(root) {
		return nil
	}
	a := &artifactPass{root: canonicalPath(root), custom: main == "" || canonicalPath(root) != canonicalPath(main+"-artifacts"), items: in.Items, acceptedAfter: in.AcceptedAfter, now: in.Now, linked: map[string]bool{}, texts: map[string][]worktreeEvidence{}, textFault: map[string]string{}}
	if a.acceptedAfter <= 0 {
		a.acceptedAfter = defaultArtifactAcceptedAfter
	}
	if a.now.IsZero() {
		a.now = time.Now()
	}
	for _, p := range linked {
		a.linked[p] = true
	}
	return a
}

// checkoutItem returns the item whose verifier checkout w is, or "": a
// detached worktree below <artifacts>/<itemId>/. A worktree there with a
// branch checked out is a working checkout under the ordinary rules.
func (a *artifactPass) checkoutItem(w gitWorktree) string {
	if a == nil || !w.Detached || !pathWithin(w.Path, a.root) || w.Path == a.root {
		return ""
	}
	item, rest, nested := strings.Cut(strings.TrimPrefix(w.Path, strings.TrimSuffix(a.root, "/")+"/"), "/")
	if !nested || rest == "" || !artifactItemIDPattern.MatchString(item) {
		return ""
	}
	return item
}

// strayCheckout reports a detached worktree below a folder named for an item
// that is not a verifier checkout of the artifacts root in use, as happens
// when that root is wrong or unset. It returns the keep reason and detail and
// the root in use ("none" without one), or "" for any other worktree. Such a
// checkout is never judged under the ordinary rules: it is kept as
// item-active while an item it is filed under runs and as retention otherwise.
func strayCheckout(w gitWorktree, in worktreeCleanupInputs, art *artifactPass) (string, string, string) {
	if in.Items == nil || !w.Detached || art.checkoutItem(w) != "" {
		return "", "", ""
	}
	root := "none"
	if art != nil {
		root = art.root
	}
	filed := false
	for dir := filepath.Dir(w.Path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		item := filepath.Base(dir)
		if !artifactItemIDPattern.MatchString(item) {
			continue
		}
		filed = true
		if st, ok := in.Items[item]; ok && st.State == artifactItemRunning {
			return keepItemActive, st.Detail, root
		}
	}
	if !filed {
		return "", "", ""
	}
	return keepRetention, "outside the artifacts root " + root + "; check --artifacts or TAILTERM_ARTIFACTS", root
}

// unreadable says why the receipt and plan files of items cannot be read for
// citations, or "": there is no artifacts root, or the root is not the
// default and is not a directory or holds no folder for one of the items.
// Session temp is kept then, since a folder those files cite cannot be told
// from an uncited one. The default root is trusted: an item with no folder
// there has no receipt or plan file to cite anything.
func (a *artifactPass) unreadable(items []string) string {
	const check = "; check --artifacts or TAILTERM_ARTIFACTS"
	if a == nil {
		return "no artifacts root, so receipt citations cannot be read" + check
	}
	if !a.custom {
		return ""
	}
	if info, err := os.Stat(a.root); err != nil || !info.IsDir() {
		return "artifacts root " + a.root + " is not the default and is not a directory, so receipt citations cannot be read" + check
	}
	for _, item := range items {
		if !artifactItemIDPattern.MatchString(item) {
			continue
		}
		if info, err := os.Stat(filepath.Join(a.root, item)); err != nil || !info.IsDir() {
			return "artifacts root " + a.root + " is not the default and has no folder for " + item + ", so its receipt citations cannot be read" + check
		}
	}
	return ""
}

// gate returns the keep reason while the item holds its checkouts, or "" with
// the basis for removing them.
func (a *artifactPass) gate(item string) (string, string) {
	st, ok := a.items[item]
	switch {
	case !ok:
		return keepRetention, "no queue record for " + item
	case st.State == artifactItemRunning:
		return keepItemActive, st.Detail
	case st.State == artifactItemReleased:
		return "", "item " + item + " released"
	}
	age := a.now.Sub(st.At)
	if age < a.acceptedAfter {
		return keepRetention, fmt.Sprintf("accepted %s ago; eligible after %s", shortDuration(age), shortDuration(a.acceptedAfter))
	}
	return "", fmt.Sprintf("item %s accepted %s ago", item, shortDuration(age))
}

// artifactCitation returns the path inside the checkout that text refers to,
// or "". Only <checkout>/<rel> with <rel> present in the checkout counts: a
// mention of the checkout directory itself, or of a sibling such as
// <checkout>-logs, does not. A reference whose tail is absent or cut short
// still counts through its longest existing leading part, so a referenced
// file is never judged unreferenced by how the text spells or ends it.
func artifactCitation(path, main, text string) string {
	for _, pattern := range worktreeEvidencePatterns(path, main) {
		prefix := pattern + "/"
		for rest := text; ; {
			i := strings.Index(rest, prefix)
			if i < 0 {
				break
			}
			rest = rest[i+len(prefix):]
			token := rest
			if end := strings.IndexFunc(token, func(r rune) bool {
				return r <= ' ' || strings.ContainsRune("\"'`<>|*?\\", r)
			}); end >= 0 {
				token = token[:end]
			}
			rel := filepath.Clean(strings.TrimRight(token, ".,;:)]}"))
			for rel != "." && rel != "/" && rel != "" && !strings.HasPrefix(rel, "..") {
				if _, err := os.Lstat(filepath.Join(path, rel)); err == nil {
					return rel
				}
				rel = filepath.Dir(rel)
			}
		}
	}
	return ""
}

// itemTexts reads the receipt and plan files of an item's artifact folder
// that lie outside any checkout: JSON files whose name contains "receipt" or
// "plan". Logs are not read; they name every file a build touched. A file
// that cannot be read, or is larger than maxArtifactTextBytes, is a fault
// that keeps the item's checkouts.
func (a *artifactPass) itemTexts(item string) ([]worktreeEvidence, string) {
	if a == nil || !artifactItemIDPattern.MatchString(item) {
		return nil, ""
	}
	if texts, ok := a.texts[item]; ok {
		return texts, a.textFault[item]
	}
	root := filepath.Join(a.root, item)
	var texts []worktreeEvidence
	fault := ""
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if fault == "" && !os.IsNotExist(err) {
				fault = "cannot read " + p + ": " + err.Error()
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if _, err := os.Lstat(filepath.Join(p, ".git")); a.linked[p] || err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		name := strings.ToLower(d.Name())
		if !d.Type().IsRegular() || !strings.HasSuffix(name, ".json") || !(strings.Contains(name, "receipt") || strings.Contains(name, "plan")) {
			return nil
		}
		info, err := d.Info()
		if err == nil && info.Size() > maxArtifactTextBytes {
			err = fmt.Errorf("larger than %d bytes", maxArtifactTextBytes)
		}
		var data []byte
		if err == nil {
			data, err = os.ReadFile(p)
		}
		if err != nil {
			if fault == "" {
				fault = "cannot read " + p + ": " + err.Error()
			}
			return nil
		}
		texts = append(texts, worktreeEvidence{Source: p, Text: string(data)})
		return nil
	})
	a.texts[item], a.textFault[item] = texts, fault
	return texts, fault
}

// cited applies the referenced-file rule to a verifier checkout: queue
// evidence and the item's receipt and plan files (tracked docs are checked
// with the candidates). It names the source and the path inside the checkout.
func (a *artifactPass) cited(path, main, item string, evidence []worktreeEvidence) (string, string) {
	texts, fault := a.itemTexts(item)
	for _, ev := range append(append([]worktreeEvidence(nil), evidence...), texts...) {
		if rel := artifactCitation(path, main, ev.Text); rel != "" {
			return keepEvidence, "cited by " + ev.Source + ": " + rel
		}
	}
	if fault != "" {
		return keepFailed, "receipt scan: " + fault
	}
	return "", ""
}

// artifactManifest records what removing a verifier checkout deleted. It is
// written beside the checkout before the removal.
type artifactManifest struct {
	Version   int                     `json:"version"`
	At        string                  `json:"at"`
	Source    string                  `json:"source"`
	ItemID    string                  `json:"itemId"`
	EntryID   string                  `json:"entryId,omitempty"`
	Path      string                  `json:"path"`
	Head      string                  `json:"head"`
	PinnedRef string                  `json:"pinnedRef,omitempty"`
	Basis     string                  `json:"basis"`
	BasisAt   string                  `json:"basisAt,omitempty"`
	Bytes     int64                   `json:"bytes"`
	Files     int64                   `json:"files"`
	Entries   []artifactManifestEntry `json:"entries"`
}

// artifactManifestEntry is one top-level entry of the removed checkout.
type artifactManifestEntry struct {
	Name    string `json:"name"`
	Bytes   int64  `json:"bytes"`
	Files   int64  `json:"files"`
	Tracked bool   `json:"tracked"`
}

// treeUsage sums regular-file bytes and counts non-directory entries below
// root without following symlinks.
func treeUsage(root string) (int64, int64, error) {
	var bytes, files int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files++
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			bytes += info.Size()
		}
		return nil
	})
	return bytes, files, err
}

func buildArtifactManifest(ctx context.Context, w gitWorktree, item string, st artifactItemState, base worktreeCleanupReceipt, now time.Time) (artifactManifest, error) {
	m := artifactManifest{Version: 1, At: now.UTC().Format(time.RFC3339Nano), Source: base.Source, ItemID: item, EntryID: st.EntryID, Path: w.Path, Head: w.Head, Basis: st.State, BasisAt: st.BasisAt, Entries: []artifactManifestEntry{}}
	listed, err := worktreeGit(ctx, w.Path, "ls-tree", "--name-only", "-z", "HEAD")
	if err != nil {
		return m, err
	}
	tracked := map[string]bool{}
	for _, name := range strings.Split(listed, "\x00") {
		if name != "" {
			tracked[name] = true
		}
	}
	top, err := os.ReadDir(w.Path)
	if err != nil {
		return m, err
	}
	for _, d := range top {
		bytes, files, err := treeUsage(filepath.Join(w.Path, d.Name()))
		if err != nil {
			return m, err
		}
		m.Entries = append(m.Entries, artifactManifestEntry{Name: d.Name(), Bytes: bytes, Files: files, Tracked: tracked[d.Name()]})
		m.Bytes += bytes
		m.Files += files
	}
	return m, nil
}

// writeArtifactManifest creates <checkout>.removed.json, or a timestamped
// name when an earlier checkout of that name already left one. It never
// replaces a file.
func writeArtifactManifest(checkout string, m artifactManifest, now time.Time) (string, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	for _, path := range []string{checkout + ".removed.json", checkout + ".removed-" + now.UTC().Format("20060102T150405.000000000Z") + ".json"} {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = file.Write(append(data, '\n'))
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
			return "", err
		}
		return path, nil
	}
	return "", errors.New("a manifest already exists at " + checkout + ".removed.json")
}

var retiredRefUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// pinArtifactHead keeps a checkout's commit reachable once its worktree is
// gone: when no branch or remote holds head it points
// refs/tailterm/retired/<itemId>/<checkout name> at it. It returns the ref
// and whether this call created it.
func pinArtifactHead(ctx context.Context, common, item string, w gitWorktree) (string, bool, error) {
	held, err := worktreeGit(ctx, common, "for-each-ref", "--count=1", "--format=%(refname)", "--contains", w.Head, "refs/heads/", "refs/remotes/")
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(held) != "" {
		return "", false, nil
	}
	name := strings.Trim(retiredRefUnsafe.ReplaceAllString(filepath.Base(w.Path), "-"), "-")
	if name == "" {
		name = "checkout"
	}
	for _, ref := range []string{"refs/tailterm/retired/" + item + "/" + name, "refs/tailterm/retired/" + item + "/" + name + "-" + shortSHA(w.Head)} {
		at, err := worktreeGit(ctx, common, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err == nil {
			if strings.TrimSpace(at) == w.Head {
				return ref, false, nil
			}
			continue
		}
		if _, err := worktreeGit(ctx, common, "update-ref", ref, w.Head, ""); err != nil {
			return "", false, err
		}
		return ref, true, nil
	}
	return "", false, errors.New("retired refs for " + name + " hold other commits")
}

// removeArtifactCheckout pins the checkout's commit when nothing else holds
// it, writes the manifest, then removes the worktree through the ordinary
// re-checked path. A removal that keeps the checkout takes the manifest and
// a ref made here back, so a manifest always means a removed checkout.
func removeArtifactCheckout(ctx context.Context, common string, w gitWorktree, admin string, cleared map[string]bool, item string, st artifactItemState, base worktreeCleanupReceipt, now time.Time) (string, string, string, int64) {
	m, err := buildArtifactManifest(ctx, w, item, st, base, now)
	if err != nil {
		return keepFailed, "manifest: " + err.Error(), "", 0
	}
	ref, created, err := pinArtifactHead(ctx, common, item, w)
	if err != nil {
		return keepFailed, "pin " + shortSHA(w.Head) + ": " + err.Error(), "", 0
	}
	unpin := func() {
		if created {
			_, _ = worktreeGit(ctx, common, "update-ref", "-d", ref, w.Head)
		}
	}
	m.PinnedRef = ref
	manifest, err := writeArtifactManifest(w.Path, m, now)
	if err != nil {
		unpin()
		return keepFailed, "write manifest: " + err.Error(), "", 0
	}
	if reason, why := removeWorktree(ctx, common, w, admin, cleared); reason != "" {
		_ = os.Remove(manifest)
		unpin()
		return reason, why, "", 0
	}
	return "", "", manifest, m.Bytes
}

// mainCheckoutGuess names the main checkout from a repository path without
// asking Git: the common directory's parent, the path itself, or the main
// checkout a linked worktree points back to. "" when it cannot tell.
func mainCheckoutGuess(repo string) string {
	if repo == "" || !filepath.IsAbs(repo) {
		return ""
	}
	repo = canonicalPath(repo)
	if filepath.Base(repo) == ".git" {
		return filepath.Dir(repo)
	}
	info, err := os.Lstat(filepath.Join(repo, ".git"))
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return repo
	}
	admin := worktreeAdminDir(repo)
	if common := filepath.Dir(filepath.Dir(admin)); admin != "" && filepath.Base(filepath.Dir(admin)) == "worktrees" && filepath.Base(common) == ".git" {
		return canonicalPath(filepath.Dir(common))
	}
	return ""
}

// holdsDetachedCheckout reports whether a directory one level below dir is a
// detached linked worktree, reading only its .git link and HEAD.
func holdsDetachedCheckout(dir string) bool {
	children, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, d := range children {
		if !d.IsDir() {
			continue
		}
		admin := worktreeAdminDir(filepath.Join(dir, d.Name()))
		if admin == "" {
			continue
		}
		if head, err := os.ReadFile(filepath.Join(admin, "HEAD")); err == nil && len(head) > 0 && !strings.HasPrefix(string(head), "ref:") {
			return true
		}
	}
	return false
}

// sessionTempFolder returns the temp folder of sessions started in cwd, or ""
// when there is none to remove: no root, no such real directory directly
// under the root (a symlink is not one), or cwd is the shared main checkout
// or shares its key.
func sessionTempFolder(root, cwd, main string) string {
	if root == "" || cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	key := claudeScratchKey(cwd)
	if main != "" && (cwd == main || key == claudeScratchKey(main)) {
		return ""
	}
	folder := filepath.Join(root, key)
	if info, err := os.Lstat(folder); err != nil || !info.IsDir() || filepath.Dir(folder) != filepath.Clean(root) {
		return ""
	}
	return folder
}

type sessionTempCandidate struct {
	Folder string
	Cwd    string
	Items  []string
}

// sessionTempCandidates lists the existing temp folders of in.TempCwds, one
// per folder. A directory that still exists outside every linked worktree is
// shared (the main checkout's subdirectories, another project), so its
// sessions are not attributed to a team and its folder is not listed.
func sessionTempCandidates(in worktreeCleanupInputs, main string, linked []string) []sessionTempCandidate {
	if in.TempRoot == "" {
		return nil
	}
	root := canonicalPath(in.TempRoot)
	byFolder := map[string]*sessionTempCandidate{}
	var order []string
	for _, c := range in.TempCwds {
		if c.Path == "" || !filepath.IsAbs(c.Path) {
			continue
		}
		real := canonicalPath(c.Path)
		if _, err := os.Stat(real); err == nil && linkedRoot(real, linked) == "" {
			continue
		}
		for _, cwd := range []string{filepath.Clean(c.Path), real} {
			folder := sessionTempFolder(root, cwd, main)
			if folder == "" {
				continue
			}
			t := byFolder[folder]
			if t == nil {
				t = &sessionTempCandidate{Folder: folder, Cwd: cwd}
				byFolder[folder] = t
				order = append(order, folder)
			}
			known := c.ItemID == ""
			for _, item := range t.Items {
				known = known || item == c.ItemID
			}
			if !known {
				t.Items = append(t.Items, c.ItemID)
			}
		}
	}
	sort.Strings(order)
	out := make([]sessionTempCandidate, 0, len(order))
	for _, folder := range order {
		out = append(out, *byFolder[folder])
	}
	return out
}

// sessionTempIdle is the time since anything in the folder, at any depth,
// last changed. A folder that cannot be read through counts as changed now.
func sessionTempIdle(folder string, now time.Time) time.Duration {
	newest := time.Time{}
	err := filepath.WalkDir(folder, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	if err != nil || newest.IsZero() {
		return 0
	}
	return now.Sub(newest)
}

// classifySessionTemp returns the first keep reason for a temp folder, or "".
func classifySessionTemp(in worktreeCleanupInputs, main string, t sessionTempCandidate, inUse, linked []string, cited map[string]string, cleared map[string]bool, art *artifactPass, now time.Time) (string, string) {
	for _, p := range append(append([]string(nil), inUse...), in.InUse...) {
		if p == "" {
			continue
		}
		if detail := sessionTempInUseAt(t.Folder, p, linked); detail != "" {
			return keepInUse, detail
		}
	}
	if in.MinIdle > 0 {
		if idle := sessionTempIdle(t.Folder, now); idle < in.MinIdle {
			return keepRecent, fmt.Sprintf("changed %s ago", idle.Round(time.Minute))
		}
	}
	if file := cited[t.Folder]; file != "" {
		return keepEvidence, "cited by " + file
	}
	// Without its items' receipt and plan files a folder they cite would
	// look uncited, so an unusable artifacts root keeps every folder.
	if why := art.unreadable(t.Items); why != "" {
		return keepRetention, why
	}
	evidence := append([]worktreeEvidence(nil), in.Evidence...)
	for _, item := range t.Items {
		texts, fault := art.itemTexts(item)
		if fault != "" {
			return keepFailed, "receipt scan: " + fault
		}
		evidence = append(evidence, texts...)
	}
	for _, ev := range evidence {
		for _, pattern := range worktreeEvidencePatterns(t.Folder, main) {
			if strings.Contains(ev.Text, pattern) {
				return keepEvidence, "cited by " + ev.Source
			}
		}
	}
	nested, err := nestedRepository(t.Folder, cleared)
	if err != nil {
		return keepFailed, "scan for nested repositories: " + err.Error()
	}
	if nested != "" {
		return keepNested, "contains Git repository " + nested
	}
	return "", ""
}

// removeSessionTemp deletes one folder directly under the temp root. It
// refuses anything that is not a real directory there, so a symlink in the
// root is never followed out of it.
func removeSessionTemp(root, folder string) error {
	info, err := os.Lstat(folder)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return err
	}
	if !info.IsDir() || real != folder || filepath.Dir(folder) != root || folder == root {
		return errors.New("not a directory directly under " + root)
	}
	restore, err := makeWorktreeWritable(folder)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(folder); err != nil {
		restore()
		return err
	}
	return nil
}

// cleanupSessionTemp decides each candidate temp folder and, when applying,
// removes the removable ones. It runs after the worktree pass, so worktrees
// that pass removed from a scratchpad no longer hold their folder.
func cleanupSessionTemp(in worktreeCleanupInputs, main string, temp []sessionTempCandidate, inUse, linked []string, cited map[string]string, cleared map[string]bool, art *artifactPass, now time.Time) []worktreeDecision {
	if len(temp) == 0 {
		return nil
	}
	root := canonicalPath(in.TempRoot)
	decisions := make([]worktreeDecision, 0, len(temp))
	for _, t := range temp {
		d := worktreeDecision{Path: t.Folder, Kind: kindSessionTemp}
		reason, detail := classifySessionTemp(in, main, t, inUse, linked, cited, cleared, art, now)
		if reason != "" {
			d.Action, d.Reason, d.Detail = "kept", reason, detail
			decisions = append(decisions, d)
			continue
		}
		d.Action, d.Detail, d.Bytes = "would-remove", "sessions started in "+t.Cwd, worktreeBytes(t.Folder, nil)
		if in.Apply {
			reason, why := "", ""
			if in.Tombstones {
				reason, why = detachSessionTemp(in, root, t.Folder, linked)
			} else if err := removeSessionTemp(root, t.Folder); err != nil {
				reason, why = keepFailed, err.Error()
			}
			if reason != "" {
				d.Action, d.Reason, d.Detail, d.Bytes = "kept", reason, why, 0
			} else {
				d.Action = "removed"
			}
		}
		decisions = append(decisions, d)
	}
	return decisions
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
		if q.OwnerIntegration != nil && q.OwnerIntegration.Evidence != "" {
			evidence = append(evidence, worktreeEvidence{Source: "queue entry " + q.ID + " owner integration evidence", Text: q.OwnerIntegration.Evidence, artifactOnly: true})
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
// on tasks-hub or pushed, its item's verifier checkouts under the artifacts
// tree once the item is released or accepted long enough, and its closed
// team's session temp folders. It reads the hub only when one of the entry's
// worktrees, temp folders or verifier checkouts still exists, at most once
// per closeoutWorktreeInterval, and never writes to the hub.
func closeoutWorktrees(ctx context.Context, c *api.Client, host string, active, project api.TeamQueueList, q api.TeamQueueEntry) error {
	direct := []string{q.Cwd, q.Acceptance.Worktree}
	if q.Integration != nil {
		direct = append(direct, q.Integration.Worktree)
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
	if repo == "" {
		return nil
	}
	// This look runs every tick, so it only stats: the main checkout is
	// read from the repository path, not asked of Git.
	main := mainCheckoutGuess(repo)
	artifacts := artifactsRootFor(main)
	tempRoot := claudeTempRoot()
	exists := false
	for _, p := range direct {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			exists = true
		}
		if sessionTempFolder(tempRoot, p, main) != "" {
			exists = true
		}
	}
	if !exists && artifacts != "" && artifactItemIDPattern.MatchString(q.ItemID) {
		exists = holdsDetachedCheckout(filepath.Join(artifacts, q.ItemID))
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
	// Only this entry's own directories and its item's agents name session
	// temp folders here; other teams' folders go at their own closeout.
	var tempCwds []sessionTempCwd
	for _, p := range roots {
		if p != "" {
			tempCwds = append(tempCwds, sessionTempCwd{Path: p, ItemID: q.ItemID})
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
	lock, err := worktreeCleanupLock()
	if errors.Is(err, errWorktreeCleanupActive) {
		// A sweep holds the host; this entry is looked at again after
		// closeoutWorktreeInterval.
		return nil
	}
	if err != nil {
		return err
	}
	defer unlockQueueLaunch(lock)
	decisions, err := cleanupWorktrees(ctx, worktreeCleanupInputs{
		Repo: repo,
		// Only paths attributable to this item: its recorded worktrees and,
		// when a recorded worktree or team cwd is a linked worktree, what is
		// nested in it or in scratchpads of sessions started there. A
		// main-checkout cwd is shared, so it attributes nothing further. The
		// item's detached verifier checkouts under the artifacts tree are its
		// own too.
		Select: func(w gitWorktree, main string, linked []string) bool {
			for _, p := range directPaths {
				if w.Path == p {
					return true
				}
			}
			if root := artifactsRootFor(main); root != "" && w.Detached && artifactItemIDPattern.MatchString(q.ItemID) && w.Path != filepath.Join(canonicalPath(root), q.ItemID) && pathWithin(w.Path, filepath.Join(canonicalPath(root), q.ItemID)) {
				return true
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
		InUse:         inUse,
		Evidence:      evidence,
		Now:           now,
		Apply:         true,
		Receipt:       worktreeCleanupReceipt{Source: "closeout", TaskID: q.TaskID, EntryID: q.ID, ItemID: q.ItemID},
		Items:         artifactItemStates(entries, detail.Agents),
		AcceptedAfter: artifactAcceptedAfter(),
		TempRoot:      tempRoot,
		TempCwds:      tempCwds,
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

// sweepHubState is every queue entry and agent the hub knows: the sweep's
// protection inputs, read before any worktree is touched.
type sweepHubState struct {
	entries []api.TeamQueueEntry
	agents  []api.Agent
}

// readSweepHub reads every project's agents and every page of its queue.
func readSweepHub(ctx context.Context, c *api.Client) (sweepHubState, error) {
	var state sweepHubState
	tasks, err := c.ListTasks(ctx)
	if err != nil {
		return state, fmt.Errorf("sweep needs the hub: %w", err)
	}
	for _, task := range tasks {
		detail, err := c.GetTask(ctx, task.ID)
		if err != nil {
			return state, fmt.Errorf("sweep needs the hub: project %s: %w", task.ID, err)
		}
		state.agents = append(state.agents, detail.Agents...)
		// The listing pages its history; an item's state needs every page.
		for after, pages := int64(0), 0; ; pages++ {
			queue, err := c.ListTeamQueuePage(ctx, task.ID, api.TeamQueueListOptions{Limit: api.MaxLimit, After: after})
			if err != nil {
				return state, fmt.Errorf("sweep needs the hub: project %s queue: %w", task.ID, err)
			}
			if after == 0 {
				state.entries = append(state.entries, queue.Entries...)
			} else {
				// A later page repeats the active entries; keep its history.
				for _, q := range queue.Entries {
					if !activeQueueEntry(q) {
						state.entries = append(state.entries, q)
					}
				}
			}
			if queue.History == nil || queue.History.NextAfter == 0 || queue.History.NextAfter == after {
				break
			}
			if pages >= 1000 {
				return state, fmt.Errorf("sweep needs the hub: project %s queue history does not end", task.ID)
			}
			after = queue.History.NextAfter
		}
	}
	return state, nil
}

// sweepOptions is one sweep of one repository.
type sweepOptions struct {
	Repo  string
	Apply bool
	// MinIdle and AcceptedAfter are the cleanup inputs of the same name.
	MinIdle       time.Duration
	AcceptedAfter time.Duration
	Artifacts     string
	Now           time.Time
	// Matrix is the host's validated matrix lock: its entries' paths are in
	// use, and an applying sweep deletes under its mutex.
	Matrix *matrixExclusion
}

// runWorktreeSweep is the one sweep pass, for the command and the schedule:
// the same protection inputs, keep rules, receipts and cache trim. The caller
// holds the cleanup lock when applying.
func runWorktreeSweep(ctx context.Context, state sweepHubState, host string, o sweepOptions) ([]worktreeDecision, error) {
	inUse, evidence := worktreeProtection(host, state.entries, state.agents)
	// Session temp folders are named for the directories this host's teams
	// worked in: their entries' worktrees and their item-bound agents' cwds.
	var tempCwds []sessionTempCwd
	for _, q := range state.entries {
		if q.Host != host {
			continue
		}
		paths := []string{q.Cwd}
		if q.Acceptance != nil {
			paths = append(paths, q.Acceptance.Worktree)
		}
		if q.Integration != nil {
			paths = append(paths, q.Integration.Worktree)
		}
		for _, p := range paths {
			if p != "" {
				tempCwds = append(tempCwds, sessionTempCwd{Path: p, ItemID: q.ItemID})
			}
		}
	}
	for _, a := range state.agents {
		if a.Host == host && a.WorkItem != nil && a.Cwd != "" {
			tempCwds = append(tempCwds, sessionTempCwd{Path: a.Cwd, ItemID: a.WorkItem.ItemID})
		}
	}
	in := worktreeCleanupInputs{
		Repo: o.Repo, InUse: inUse, Evidence: evidence, MinIdle: o.MinIdle, Now: o.Now,
		Apply: o.Apply, MeasureBytes: true, Receipt: worktreeCleanupReceipt{Source: "sweep"},
		Items: artifactItemStates(state.entries, state.agents), Artifacts: o.Artifacts, AcceptedAfter: o.AcceptedAfter,
		TempRoot: claudeTempRoot(), TempCwds: tempCwds,
		Trim: finishedTrimSet(host, state.entries, state.agents), Tombstones: o.Apply,
	}
	if o.Matrix != nil {
		// A matrix run keeps the checkout, output and record folders it holds.
		in.InUse = append(in.InUse, o.Matrix.paths()...)
		if o.Apply {
			in.Exclusive = o.Matrix.run
		}
	}
	return cleanupWorktrees(ctx, in)
}

// sweepNow is the command's clock; tests replace it.
var sweepNow = time.Now

const sweepUsage = "usage: tt team queue sweep-worktrees [--apply] [--json] [--min-idle 24h] [--accepted-after 24h] [--artifacts DIR] [--cwd DIR] [--hub URL] | --journal [--limit 20] [--json]"

// cmdTeamQueueSweepWorktrees applies the closeout rules to every existing
// linked worktree of the repository, verifier checkouts under the artifacts
// tree included, and to the session temp folders of this host's teams. It is
// a dry run unless --apply is set, and reads the hub before touching anything.
// With --journal it prints the scheduled sweep's journal instead, from the
// local file only.
func cmdTeamQueueSweepWorktrees(e env, args []string) error {
	fs := flag.NewFlagSet("team queue sweep-worktrees", flag.ContinueOnError)
	hub := fs.String("hub", e.hub, "hub URL")
	cwd := fs.String("cwd", "", "any worktree of the repository (default: current directory)")
	apply := fs.Bool("apply", false, "remove the would-remove worktrees and session temp folders, trim the would-trim caches and prune missing worktrees")
	jsonOut := fs.Bool("json", false, "print JSON")
	minIdle := fs.Duration("min-idle", 24*time.Hour, "keep worktrees whose Git state changed more recently, and session temp folders changed more recently")
	acceptedAfter := fs.Duration("accepted-after", defaultArtifactAcceptedAfter, "remove an accepted, unreleased item's verifier checkouts once its acceptance is this old")
	artifacts := fs.String("artifacts", "", "artifacts root, an absolute path (default: TAILTERM_ARTIFACTS, else <main checkout>-artifacts)")
	journal := fs.Bool("journal", false, "print this host's scheduled sweep journal, newest last")
	limit := fs.Int("limit", 20, "with --journal: how many lines to print")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *journal {
		if fs.NArg() != 0 || *apply || *limit < 1 {
			return errors.New(sweepUsage)
		}
		return printSweepJournal(*limit, *jsonOut)
	}
	if fs.NArg() != 0 || *hub == "" || *minIdle < 0 || *acceptedAfter < 0 {
		return errors.New(sweepUsage)
	}
	// A relative artifacts root would name no tree and turn the item rules
	// off, so it is refused before anything is read. The flag overrides the
	// variable.
	if *artifacts != "" && !filepath.IsAbs(*artifacts) {
		return fmt.Errorf("--artifacts must be an absolute path, not %q", *artifacts)
	}
	if dir := os.Getenv("TAILTERM_ARTIFACTS"); *artifacts == "" && dir != "" && !filepath.IsAbs(dir) {
		return fmt.Errorf("TAILTERM_ARTIFACTS must be an absolute path, not %q; fix it or pass --artifacts", dir)
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
	// failure or an untrustworthy matrix lock file stops the sweep.
	state, err := readSweepHub(ctx, c)
	if err != nil {
		return err
	}
	matrix, why := loadMatrixExclusion()
	if matrix == nil {
		return fmt.Errorf("sweep cannot trust the matrix lock file (%s); nothing was examined", why)
	}
	if *apply {
		lock, err := worktreeCleanupLock()
		if err != nil {
			return err
		}
		defer unlockQueueLaunch(lock)
	}
	decisions, err := runWorktreeSweep(ctx, state, host, sweepOptions{Repo: repo, Apply: *apply, MinIdle: *minIdle, AcceptedAfter: acceptedAfterInput(*acceptedAfter), Artifacts: *artifacts, Now: sweepNow(), Matrix: matrix})
	if decisions == nil && err != nil {
		return err
	}
	report := worktreeSweepReport{Worktrees: decisions, Totals: worktreeSweepTotals{Actions: map[string]int{}, Kept: map[string]int{}}}
	if report.Worktrees == nil {
		report.Worktrees = []worktreeDecision{}
	}
	strays, strayRoot := 0, ""
	for _, d := range decisions {
		report.Totals.Actions[d.Action]++
		if d.Action == "kept" {
			report.Totals.Kept[d.Reason]++
		}
		report.Totals.Bytes += d.Bytes
		if d.strayRoot != "" {
			strays, strayRoot = strays+1, d.strayRoot
		}
	}
	if strays > 0 {
		noun := map[bool]string{true: "checkout is", false: "checkouts are"}[strays == 1]
		fmt.Fprintf(os.Stderr, "tt: %d detached %s under an item folder but outside the artifacts root %s; kept. Check --artifacts or TAILTERM_ARTIFACTS\n", strays, noun, strayRoot)
	}
	if *jsonOut {
		printJSON(report)
		return err
	}
	for _, d := range decisions {
		switch d.Action {
		case "kept":
			fmt.Printf("kept %s %s: %s\n", d.Reason, d.Path, d.Detail)
		case "removed", "would-remove", "trimmed", "would-trim":
			if d.Kind != "" {
				fmt.Printf("%s %s (%s; %s)\n", d.Action, d.Path, worktreeLabel(d), humanBytes(d.Bytes))
				continue
			}
			fmt.Printf("%s %s (%s, %s)\n", d.Action, d.Path, worktreeLabel(d), humanBytes(d.Bytes))
		default:
			fmt.Printf("%s %s\n", d.Action, d.Path)
		}
	}
	var parts []string
	for _, action := range []string{"removed", "would-remove", "trimmed", "would-trim", "pruned", "would-prune", "kept"} {
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
		fmt.Println("dry run: nothing changed; rerun with --apply to remove the would-remove worktrees and session temp folders and trim the would-trim caches")
	}
	return err
}

func worktreeLabel(d worktreeDecision) string {
	switch d.Kind {
	case kindArtifactCheckout:
		return "artifact checkout; " + d.Detail
	case kindSessionTemp:
		return "session temp"
	case kindCache:
		return "cache; " + d.Detail
	}
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

// Cache trim and tombstones.
//
// The sweep trims one regenerable cache, node_modules, out of a checkout it
// keeps, when the checkout belongs to a finished item. A trim, and the sweep's
// removal of a session temp folder, never deletes by pathname: the parent
// directory is opened as a pinned root, the entry is renamed to a tombstone
// beside it, and the tombstone is deleted through that root. The intent is
// saved before the rename, so a pass that died in between is finished or
// dropped by the next one, and nothing is ever deleted by name alone.

// tombstonePrefix starts every tombstone name; the rest is random.
const tombstonePrefix = ".tt-sweep-trash-"

// trimCacheName is the whole trim allowlist, and trimCacheMarker is the file
// npm writes inside it on every install.
const (
	trimCacheName   = "node_modules"
	trimCacheMarker = ".package-lock.json"
)

// trimKeptReasons are the keep reasons under which a checkout may still be
// trimmed; the lock, operation and idle rules are checked again directly,
// since the first reason reported can hide a later one.
var trimKeptReasons = map[string]bool{keepRetention: true, keepEvidence: true, keepDirty: true, keepUnpushed: true, keepNested: true}

// Test seams. trimBeforeDelete runs after every check and immediately before
// the rename; detachCrashAt returning true abandons the detach at that stage
// ("intent" or "renamed"), as a crash would.
var (
	trimBeforeDelete func(path string)
	detachCrashAt    func(stage string) bool
)

// cacheTrimSet names what may be trimmed: the finished items, and this
// host's recorded checkouts of those items by canonical path.
type cacheTrimSet struct {
	Items map[string]bool
	Paths map[string]string
}

// finishedTrimSet derives the trim set from every queue entry and agent, on
// any host. An item is finished when its newest entry by position, of any
// state, is finished and accepted or released, none of its entries is active
// and no agent bound to it is other than closed. It does not change
// artifactItemStates or any keep rule.
func finishedTrimSet(host string, entries []api.TeamQueueEntry, agents []api.Agent) *cacheTrimSet {
	type newest struct {
		position int64
		ok       bool
	}
	state := map[string]*newest{}
	vetoed := map[string]bool{}
	for _, q := range entries {
		if q.ItemID == "" {
			continue
		}
		if activeQueueEntry(q) {
			vetoed[q.ItemID] = true
		}
		ok := q.State == "finished" && (releasedQueueEntry(q) || q.Acceptance != nil)
		switch cur := state[q.ItemID]; {
		case cur == nil || q.Position > cur.position:
			state[q.ItemID] = &newest{position: q.Position, ok: ok}
		case q.Position == cur.position:
			cur.ok = cur.ok && ok
		}
	}
	for _, a := range agents {
		if a.WorkItem != nil && a.WorkItem.ItemID != "" && a.Status != api.AgentClosed {
			vetoed[a.WorkItem.ItemID] = true
		}
	}
	set := &cacheTrimSet{Items: map[string]bool{}, Paths: map[string]string{}}
	for item, n := range state {
		if n.ok && !vetoed[item] {
			set.Items[item] = true
		}
	}
	for _, q := range entries {
		if q.Host != host || q.State != "finished" || q.Acceptance == nil || !set.Items[q.ItemID] {
			continue
		}
		paths := []string{q.Cwd, q.Acceptance.Worktree}
		if q.Integration != nil {
			paths = append(paths, q.Integration.Worktree)
		}
		for _, p := range paths {
			if p != "" && filepath.IsAbs(p) {
				set.Paths[canonicalPath(p)] = q.ItemID
			}
		}
	}
	return set
}

// neverTouchPaths are the shared user caches no cleanup may delete, lie in or
// contain: the standing owner rule is that they are never cleared.
func neverTouchPaths() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		out = append(out, filepath.Join(home, "Library", "Caches"), filepath.Join(home, "go"), filepath.Join(home, ".npm"))
	}
	for _, name := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "npm_config_cache"} {
		for _, p := range filepath.SplitList(os.Getenv(name)) {
			if filepath.IsAbs(p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// neverTouch names the never-touch path that path lies in or contains, or
// that is the same file as one of infos; "" when there is none.
func neverTouch(path string, infos ...os.FileInfo) string {
	path = canonicalPath(path)
	for _, deny := range neverTouchPaths() {
		real := canonicalPath(deny)
		if pathWithin(path, real) || pathWithin(real, path) {
			return deny
		}
		st, err := os.Stat(deny)
		if err != nil {
			continue
		}
		for _, info := range infos {
			if info != nil && os.SameFile(st, info) {
				return deny
			}
		}
	}
	return ""
}

// trimCaches decides the cache of each kept, finished checkout and, when
// applying, trims it. candidates and decisions are the worktree pass's, in
// the same order. A checkout with no cache, or one that is not attributable
// to a finished item, gets no decision.
func trimCaches(ctx context.Context, in worktreeCleanupInputs, common, main string, candidates []gitWorktree, decisions []worktreeDecision, linked []string, art *artifactPass, now time.Time) []worktreeDecision {
	if in.Trim == nil {
		return nil
	}
	var roots []string
	if main != "" {
		roots = append(roots, filepath.Join(main, ".build", "worktrees"))
	}
	if art != nil {
		roots = append(roots, art.root)
	}
	var out []worktreeDecision
	for i, w := range candidates {
		if i >= len(decisions) || decisions[i].Path != w.Path || decisions[i].Action != "kept" || !trimKeptReasons[decisions[i].Reason] {
			continue
		}
		item := art.checkoutItem(w)
		if item != "" {
			if !in.Trim.Items[item] {
				continue
			}
		} else if item = in.Trim.Paths[w.Path]; item == "" {
			continue
		}
		allowed := ""
		for _, root := range roots {
			if w.Path != root && pathWithin(w.Path, root) {
				allowed = root
			}
		}
		cache := filepath.Join(w.Path, trimCacheName)
		cacheInfo, err := os.Lstat(cache)
		if allowed == "" || err != nil {
			continue
		}
		d := worktreeDecision{Path: cache, Branch: strings.TrimPrefix(w.Branch, "refs/heads/"), Head: w.Head, Kind: kindCache}
		admin := worktreeAdminDir(w.Path)
		checkout, reason, detail := classifyCache(ctx, in, common, main, w, admin, item, cacheInfo, art, now)
		switch {
		case reason != "":
			d.Action, d.Reason, d.Detail = "kept", reason, detail
		default:
			if in.MeasureBytes {
				d.Bytes = worktreeBytes(cache, nil)
			}
			d.Action, d.Detail = "would-trim", "regenerable cache in a finished checkout of "+item
			if in.Apply {
				if reason, detail := trimCheckoutCache(in, w, admin, checkout, allowed, linked); reason != "" {
					d.Action, d.Reason, d.Detail, d.Bytes = "kept", reason, detail, 0
				} else {
					d.Action = "trimmed"
				}
			}
		}
		out = append(out, d)
	}
	return out
}

// classifyCache returns the checkout's identity and the first reason its
// cache is kept, or "" when the cache may be trimmed.
func classifyCache(ctx context.Context, in worktreeCleanupInputs, common, main string, w gitWorktree, admin, item string, cacheInfo os.FileInfo, art *artifactPass, now time.Time) (os.FileInfo, string, string) {
	cache := filepath.Join(w.Path, trimCacheName)
	checkout, err := os.Lstat(w.Path)
	if err != nil || !checkout.IsDir() {
		return nil, keepMoved, "the checkout is not a real directory"
	}
	if !cacheInfo.IsDir() {
		return nil, keepUnproven, trimCacheName + " is not a real directory; left in place"
	}
	if admin == "" {
		return nil, keepFailed, "worktree has no readable .git link"
	}
	if _, err := os.Lstat(filepath.Join(admin, "locked")); w.Locked || err == nil {
		return nil, keepLocked, "git worktree is locked"
	}
	if op := worktreeOperation(admin); op != "" {
		return nil, keepOperation, op + " in progress"
	}
	if in.MinIdle > 0 {
		if idle := worktreeIdle(admin, now); idle < in.MinIdle {
			return nil, keepRecent, fmt.Sprintf("Git state changed %s ago", idle.Round(time.Minute))
		}
		if age := now.Sub(cacheInfo.ModTime()); age < in.MinIdle {
			return nil, keepRecent, fmt.Sprintf("%s changed %s ago", trimCacheName, age.Round(time.Minute))
		}
	}
	if marker, err := os.Lstat(filepath.Join(cache, trimCacheMarker)); err != nil || !marker.Mode().IsRegular() {
		return nil, keepUnproven, "no npm install marker " + filepath.Join(trimCacheName, trimCacheMarker)
	}
	tracked, err := worktreeGit(ctx, w.Path, "ls-files", "-z", "--", trimCacheName)
	if err != nil {
		return nil, keepFailed, err.Error()
	}
	if tracked != "" {
		return nil, keepUnproven, trimCacheName + " holds tracked files"
	}
	if _, err := worktreeGit(ctx, w.Path, "check-ignore", "-q", trimCacheName); err != nil {
		if gitExitCode(err) == 1 {
			return nil, keepUnproven, trimCacheName + " is not ignored by Git"
		}
		return nil, keepFailed, err.Error()
	}
	nested, err := nestedRepository(cache, nil)
	if err != nil {
		return nil, keepFailed, "scan for nested repositories: " + err.Error()
	}
	if nested != "" {
		return nil, keepNested, "contains Git repository " + nested
	}
	docs, err := docsCitations(ctx, common, main, []string{cache}, nil)
	if err != nil {
		return nil, keepFailed, "evidence lookup: " + err.Error()
	}
	if file := docs[cache]; file != "" {
		return nil, keepEvidence, "cited by " + file
	}
	evidence := append([]worktreeEvidence(nil), in.Evidence...)
	texts, fault := art.itemTexts(item)
	if fault != "" {
		return nil, keepFailed, "receipt scan: " + fault
	}
	for _, ev := range append(evidence, texts...) {
		for _, pattern := range worktreeEvidencePatterns(cache, main) {
			if strings.Contains(ev.Text, pattern) {
				return nil, keepEvidence, "cited by " + ev.Source
			}
		}
	}
	for _, target := range []struct {
		path string
		info os.FileInfo
	}{{w.Path, checkout}, {cache, cacheInfo}} {
		if deny := neverTouch(target.path, target.info); deny != "" {
			return nil, keepProtected, target.path + " touches the never-touch path " + deny
		}
	}
	return checkout, "", ""
}

// trimCheckoutCache is the only route to a trim. It pins the checkout as a
// root, proves the pinned directory is the one that was classified, and from
// then on resolves no pathname again: the rename and the delete are confined
// to that root, so a checkout or ancestor swapped meanwhile is never followed.
func trimCheckoutCache(in worktreeCleanupInputs, w gitWorktree, admin string, checkout os.FileInfo, allowed string, linked []string) (string, string) {
	root, err := os.OpenRoot(w.Path)
	if err != nil {
		return keepMoved, "the checkout cannot be opened: " + err.Error()
	}
	defer root.Close()
	pinned, err := root.Stat(".")
	if err != nil || !os.SameFile(checkout, pinned) {
		return keepMoved, "the checkout was replaced since it was classified"
	}
	link, err := root.ReadFile(".git")
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(link)), "gitdir: ")
	if err != nil || !ok || filepath.Clean(dir) != admin {
		return keepMoved, "the checkout's .git link no longer names " + admin
	}
	check := func() (os.FileInfo, string, string) {
		cache, err := root.Lstat(trimCacheName)
		if err != nil || !cache.IsDir() {
			return nil, keepUnproven, trimCacheName + " is not a real directory; left in place"
		}
		if marker, err := root.Lstat(filepath.Join(trimCacheName, trimCacheMarker)); err != nil || !marker.Mode().IsRegular() {
			return nil, keepUnproven, "no npm install marker " + filepath.Join(trimCacheName, trimCacheMarker)
		}
		return cache, "", ""
	}
	cache, reason, detail := check()
	if reason != "" {
		return reason, detail
	}
	for _, target := range []struct {
		path string
		info os.FileInfo
	}{{w.Path, pinned}, {filepath.Join(w.Path, trimCacheName), cache}} {
		if deny := neverTouch(target.path, target.info); deny != "" {
			return keepProtected, target.path + " touches the never-touch path " + deny
		}
	}
	inUse := func(p string) string { return worktreeInUseAt(w.Path, p, linked) }
	return detachAndDelete(in, root, pinned, w.Path, allowed, trimCacheName, kindCache, inUse, func() (string, string) {
		if _, err := os.Lstat(filepath.Join(admin, "locked")); err == nil {
			return keepLocked, "locked since classification"
		}
		if op := worktreeOperation(admin); op != "" {
			return keepOperation, op + " in progress"
		}
		_, reason, detail := check()
		return reason, detail
	})
}

// detachIntent is what a pass saves before it renames name to a tombstone in
// a pinned parent: enough for a later pass to prove it is deleting the same
// tombstone in the same directory, and nothing else.
type detachIntent struct {
	At string `json:"at"`
	// Kind is the decision kind: cache or session-temp.
	Kind string `json:"kind"`
	// Parent is the pinned directory's canonical path and Root the worktrees,
	// artifacts or session temp root it was recorded under.
	Parent string `json:"parent"`
	Root   string `json:"root"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Source string `json:"source"`
	// Tombstone is the generated name: the reserved prefix and a random
	// suffix.
	Tombstone string `json:"tombstone"`
}

// directoryIdentity is the device and inode of an opened or stat'ed file.
func directoryIdentity(info os.FileInfo) (uint64, uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}

func validTombstone(name string) bool {
	suffix, ok := strings.CutPrefix(name, tombstonePrefix)
	return ok && suffix != "" && !strings.ContainsAny(name, "/\\")
}

// rootMakeWritable adds owner rwx to the directories under name that lack
// it, through the root, so a read-only module cache in a session temp folder
// can be deleted. It never leaves the root.
func rootMakeWritable(root *os.Root, name string) {
	_ = fs.WalkDir(root.FS(), name, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().Perm()&0700 != 0700 {
			_ = root.Chmod(p, info.Mode().Perm()|0700)
		}
		return nil
	})
}

// detachAndDelete removes name from the pinned parent: it saves the intent,
// re-checks and renames name to a tombstone under the exclusion, then deletes
// the tombstone through the root and clears the intent. Nothing is renamed
// when the intent cannot be saved. It returns a keep reason, or "".
func detachAndDelete(in worktreeCleanupInputs, root *os.Root, parent os.FileInfo, parentPath, allowed, name, kind string, inUse func(p string) string, recheck func() (string, string)) (string, string) {
	device, inode, ok := directoryIdentity(parent)
	if !ok {
		return keepFailed, "the directory's identity cannot be read"
	}
	suffix, err := randomHex(8)
	if err != nil {
		return keepFailed, err.Error()
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	intent := detachIntent{At: now.UTC().Format(time.RFC3339Nano), Kind: kind, Parent: parentPath, Root: allowed, Device: device, Inode: inode, Source: name, Tombstone: tombstonePrefix + suffix}
	if err := saveDetachIntent(intent); err != nil {
		return keepFailed, "intent not saved, nothing renamed: " + err.Error()
	}
	if detachCrashAt != nil && detachCrashAt("intent") {
		return keepFailed, "interrupted after the intent"
	}
	var reason, why string
	held, heldWhy := in.exclusive(inUse, func() {
		if reason, why = recheck(); reason != "" {
			return
		}
		if trimBeforeDelete != nil {
			trimBeforeDelete(filepath.Join(parentPath, name))
		}
		if err := root.Rename(name, intent.Tombstone); err != nil {
			reason, why = keepFailed, err.Error()
		}
	})
	if held != "" {
		reason, why = held, heldWhy
	}
	if reason != "" {
		if err := clearDetachIntent(intent.Tombstone); err != nil {
			why += "; intent not cleared: " + err.Error()
		}
		return reason, why
	}
	if detachCrashAt != nil && detachCrashAt("renamed") {
		return keepFailed, "interrupted after the rename"
	}
	if kind == kindSessionTemp {
		rootMakeWritable(root, intent.Tombstone)
	}
	if err := root.RemoveAll(intent.Tombstone); err != nil {
		// The intent stays, so the next pass finishes the delete.
		return keepFailed, "detached as " + intent.Tombstone + " but not deleted: " + err.Error()
	}
	if err := clearDetachIntent(intent.Tombstone); err != nil {
		return keepFailed, "deleted, but the intent was not cleared: " + err.Error()
	}
	return "", ""
}

// detachSessionTemp is the sweep's removal of one folder directly under the
// temp root: the checks of removeSessionTemp, then a detach through the
// pinned root.
func detachSessionTemp(in worktreeCleanupInputs, root, folder string, linked []string) (string, string) {
	info, err := os.Lstat(folder)
	if err != nil {
		return keepFailed, err.Error()
	}
	real, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return keepFailed, err.Error()
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || !info.IsDir() || real != folder || filepath.Dir(folder) != root || folder == root {
		return keepFailed, "not a directory directly under " + root
	}
	pinnedRoot, err := os.OpenRoot(root)
	if err != nil {
		return keepFailed, err.Error()
	}
	defer pinnedRoot.Close()
	pinned, err := pinnedRoot.Stat(".")
	if err != nil || !os.SameFile(rootInfo, pinned) {
		return keepMoved, "the temp root was replaced"
	}
	name := filepath.Base(folder)
	check := func() (os.FileInfo, string, string) {
		entry, err := pinnedRoot.Lstat(name)
		if err != nil || !entry.IsDir() {
			return nil, keepMoved, "not a real directory directly under " + root
		}
		return entry, "", ""
	}
	entry, reason, detail := check()
	if reason != "" {
		return reason, detail
	}
	if deny := neverTouch(folder, pinned, entry); deny != "" {
		return keepProtected, folder + " touches the never-touch path " + deny
	}
	inUse := func(p string) string { return sessionTempInUseAt(folder, p, linked) }
	return detachAndDelete(in, pinnedRoot, pinned, root, root, name, kindSessionTemp, inUse, func() (string, string) {
		_, reason, detail := check()
		return reason, detail
	})
}

// recoverDetachIntents settles what an interrupted pass left under this
// pass's roots. A tombstone is deleted only when the recorded parent is still
// the same directory under the root it was recorded under, and the tombstone
// is a real directory with the reserved prefix; anything else deletes
// nothing, drops the intent and is recorded as kept with the cause.
func recoverDetachIntents(in worktreeCleanupInputs, main string, art *artifactPass) []worktreeDecision {
	roots := map[string]bool{}
	if main != "" {
		roots[filepath.Join(main, ".build", "worktrees")] = true
	}
	if art != nil {
		roots[art.root] = true
	}
	if in.TempRoot != "" {
		roots[canonicalPath(in.TempRoot)] = true
	}
	intents, err := loadDetachIntents()
	if err != nil {
		return nil
	}
	var out []worktreeDecision
	for _, intent := range intents {
		if !roots[intent.Root] {
			continue
		}
		d := worktreeDecision{Path: filepath.Join(intent.Parent, intent.Source), Kind: intent.Kind}
		cause, retry := finishDetach(intent)
		switch {
		case cause == "":
			d.Action, d.Detail = "removed", "finished an interrupted sweep: deleted "+intent.Tombstone
			if intent.Kind == kindCache {
				d.Action = "trimmed"
			}
		case retry:
			d.Action, d.Reason, d.Detail = "kept", keepFailed, cause
		default:
			d.Action, d.Reason, d.Detail = "kept", keepMoved, "interrupted sweep left "+intent.Tombstone+"; nothing deleted: "+cause
		}
		if !retry {
			if err := clearDetachIntent(intent.Tombstone); err != nil {
				d.Detail += "; intent not cleared: " + err.Error()
			}
		}
		out = append(out, d)
	}
	return out
}

// finishDetach deletes the intent's tombstone when every check holds. It
// returns the cause when it deleted nothing, and whether the intent should be
// kept for another try (the delete itself failed).
func finishDetach(intent detachIntent) (string, bool) {
	if !validTombstone(intent.Tombstone) || intent.Source == "" || strings.ContainsAny(intent.Source, "/\\") || !filepath.IsAbs(intent.Parent) || !filepath.IsAbs(intent.Root) {
		return "the intent is malformed", false
	}
	if intent.Kind != kindCache && intent.Kind != kindSessionTemp {
		return "the intent has an unknown kind", false
	}
	root, err := os.OpenRoot(intent.Parent)
	if err != nil {
		return "the parent directory cannot be opened: " + err.Error(), false
	}
	defer root.Close()
	pinned, err := root.Stat(".")
	if err != nil {
		return "the parent directory cannot be read: " + err.Error(), false
	}
	if device, inode, ok := directoryIdentity(pinned); !ok || device != intent.Device || inode != intent.Inode {
		return "the parent is not the directory the intent recorded", false
	}
	parent := canonicalPath(intent.Parent)
	if parent != intent.Parent || !pathWithin(parent, intent.Root) || (parent == intent.Root) != (intent.Kind == kindSessionTemp) {
		return "the parent no longer lies under " + intent.Root, false
	}
	tombstone, err := root.Lstat(intent.Tombstone)
	if err != nil {
		if _, sourceErr := root.Lstat(intent.Source); sourceErr == nil {
			return intent.Source + " was never detached", false
		}
		return "the tombstone is gone", false
	}
	if !tombstone.IsDir() {
		return "the tombstone is not a real directory", false
	}
	if deny := neverTouch(filepath.Join(parent, intent.Tombstone), pinned, tombstone); deny != "" {
		return "it touches the never-touch path " + deny, false
	}
	if intent.Kind == kindSessionTemp {
		rootMakeWritable(root, intent.Tombstone)
	}
	if err := root.RemoveAll(intent.Tombstone); err != nil {
		return "detached as " + intent.Tombstone + " but not deleted: " + err.Error(), true
	}
	return "", false
}
