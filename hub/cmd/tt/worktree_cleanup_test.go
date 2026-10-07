package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// Every fixture is a real Git repository in t.TempDir(); nothing here reads
// the live repository, hub or relay state.

type cleanupRepo struct {
	root   string // canonical temp root
	main   string // main checkout, on tasks-hub
	common string
}

func cleanupGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// resetCleanupProcessState clears what closeout remembers for the life of the
// process, now and when the test ends: which entries it examined recently and
// that it already warned about a bad TAILTERM_ARTIFACT_ACCEPTED_AFTER. Without
// it a repeat run (go test -count) starts where the last one stopped.
func resetCleanupProcessState(t *testing.T) {
	t.Helper()
	reset := func() {
		closeoutWorktreeChecks.Clear()
		keptWorktreeReceipts.Clear()
		artifactAcceptedAfterWarning = sync.Once{}
	}
	reset()
	t.Cleanup(reset)
}

func newCleanupRepo(t *testing.T) cleanupRepo {
	t.Helper()
	resetCleanupProcessState(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	t.Setenv("TAILTERM_ARTIFACTS", "")
	t.Setenv("TAILTERM_ARTIFACT_ACCEPTED_AFTER", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The matrix lock is a fake file too: a sweep takes its mutex, and the
	// host's real one is never read or linked.
	t.Setenv("TAILTERM_MATRIX_HOST_LOCK", filepath.Join(root, "matrix", "host.json"))
	// Session temp is a fake tree too; the host's real one is never read.
	realTempRoot := claudeTempRoot
	claudeTempRoot = func() string { return filepath.Join(root, "tmp", "claude-501") }
	t.Cleanup(func() { claudeTempRoot = realTempRoot })
	main := filepath.Join(root, "repo")
	if err := os.MkdirAll(main, 0755); err != nil {
		t.Fatal(err)
	}
	cleanupGit(t, main, "init", "-q", "-b", "tasks-hub")
	writeFixtureFile(t, filepath.Join(main, ".gitignore"), ".build/\n")
	writeFixtureFile(t, filepath.Join(main, "docs", "handoff.md"), "# Handoff\n")
	writeFixtureFile(t, filepath.Join(main, "app.txt"), "base\n")
	cleanupGit(t, main, "add", ".")
	cleanupGit(t, main, "commit", "-q", "-m", "base")
	return cleanupRepo{root: root, main: main, common: filepath.Join(main, ".git")}
}

// branchWorktree adds a worktree on a new branch from tasks-hub with one
// commit of its own.
func (r cleanupRepo) branchWorktree(t *testing.T, path, branch string) string {
	t.Helper()
	cleanupGit(t, r.main, "worktree", "add", "-q", "-b", branch, path, "tasks-hub")
	writeFixtureFile(t, filepath.Join(path, branch+".txt"), branch+"\n")
	cleanupGit(t, path, "add", ".")
	cleanupGit(t, path, "commit", "-q", "-m", "work on "+branch)
	return path
}

func (r cleanupRepo) queuePath(name string) string {
	return filepath.Join(r.main, ".build", "worktrees", name)
}

func (r cleanupRepo) sweep(t *testing.T, apply bool, inUse []string, evidence []worktreeEvidence) map[string]worktreeDecision {
	t.Helper()
	decisions, err := cleanupWorktrees(context.Background(), worktreeCleanupInputs{Repo: r.main, InUse: inUse, Evidence: evidence, Now: time.Now(), Apply: apply, Receipt: worktreeCleanupReceipt{Source: "sweep"}})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]worktreeDecision{}
	for _, d := range decisions {
		out[d.Path] = d
	}
	return out
}

func readCleanupReceipts(t *testing.T) []worktreeCleanupReceipt {
	t.Helper()
	file, err := os.Open(filepath.Join(relayDir(), "worktree-cleanup.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var out []worktreeCleanupReceipt
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var r worktreeCleanupReceipt
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			t.Fatalf("receipt line %q: %v", scanner.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

func receiptFor(receipts []worktreeCleanupReceipt, path string) (worktreeCleanupReceipt, bool) {
	for _, r := range receipts {
		if r.Path == path {
			return r, true
		}
	}
	return worktreeCleanupReceipt{}, false
}

func requireDecision(t *testing.T, got map[string]worktreeDecision, path, action, reason string) worktreeDecision {
	t.Helper()
	d, ok := got[path]
	if !ok {
		t.Fatalf("no decision for %s in %+v", path, got)
	}
	if d.Action != action || d.Reason != reason {
		t.Fatalf("%s: got %s %s (%s), want %s %s", path, d.Action, d.Reason, d.Detail, action, reason)
	}
	return d
}

func requireExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	if want && err != nil {
		t.Fatalf("%s should still exist: %v", path, err)
	}
	if !want && !os.IsNotExist(err) {
		t.Fatalf("%s should be gone: %v", path, err)
	}
}

// Case 1: a clean branch fast-forwarded into tasks-hub is removed with its
// read-only Go module cache; the branch and commit still resolve.
func TestWorktreeCleanupRemovesFastForwardedWorktreeWithReadOnlyCache(t *testing.T) {
	r := newCleanupRepo(t)
	w := r.branchWorktree(t, r.queuePath("queue-aaaa0001"), "bug/merged")
	head := cleanupGit(t, w, "rev-parse", "HEAD")
	cleanupGit(t, r.main, "merge", "-q", "--ff-only", "bug/merged")
	mod := filepath.Join(w, ".build", "go", "pkg", "mod", "example.com", "m@v1.0.0")
	writeFixtureFile(t, filepath.Join(mod, "go.mod"), "module example.com/m\n")
	if err := os.Chmod(filepath.Join(mod, "go.mod"), 0444); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{mod, filepath.Dir(mod)} {
		if err := os.Chmod(dir, 0555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = makeWorktreeWritable(w) })

	dry := r.sweep(t, false, nil, nil)
	requireDecision(t, dry, w, "would-remove", "")
	requireExists(t, w, true)
	if receipts := readCleanupReceipts(t); len(receipts) != 0 {
		t.Fatalf("dry run wrote receipts %+v", receipts)
	}

	got := r.sweep(t, true, nil, nil)
	d := requireDecision(t, got, w, "removed", "")
	if d.Branch != "bug/merged" || d.Head != head || !strings.Contains(d.Detail, "tasks-hub") {
		t.Fatalf("removed decision %+v", d)
	}
	requireExists(t, w, false)
	if sha := cleanupGit(t, r.main, "rev-parse", "refs/heads/bug/merged"); sha != head {
		t.Fatalf("branch moved or vanished: %s want %s", sha, head)
	}
	cleanupGit(t, r.main, "cat-file", "-e", head+"^{commit}")
	if strings.Contains(cleanupGit(t, r.main, "worktree", "list"), w) {
		t.Fatal("git still lists the removed worktree")
	}
	rec, ok := receiptFor(readCleanupReceipts(t), w)
	if !ok || rec.Action != "removed" || rec.Source != "sweep" || rec.Head != head || rec.Branch != "bug/merged" || rec.At == "" {
		t.Fatalf("removed receipt %+v %v", rec, ok)
	}
}

// Case 2: cherry-pick integration gives a different SHA; patch equivalence
// still makes the worktree removable, and its branch is kept.
func TestWorktreeCleanupRemovesCherryPickedWorktree(t *testing.T) {
	r := newCleanupRepo(t)
	w := r.branchWorktree(t, r.queuePath("queue-aaaa0002"), "bug/picked")
	head := cleanupGit(t, w, "rev-parse", "HEAD")
	// tasks-hub moves on first, so the pick lands with a different SHA.
	writeFixtureFile(t, filepath.Join(r.main, "other.txt"), "other\n")
	cleanupGit(t, r.main, "add", ".")
	cleanupGit(t, r.main, "commit", "-q", "-m", "other")
	cleanupGit(t, r.main, "cherry-pick", head)
	if cleanupGit(t, r.main, "rev-parse", "HEAD") == head {
		t.Fatal("fixture pick kept the same SHA")
	}
	got := r.sweep(t, true, nil, nil)
	d := requireDecision(t, got, w, "removed", "")
	if !strings.Contains(d.Detail, "patch-equivalent") {
		t.Fatalf("detail %q", d.Detail)
	}
	requireExists(t, w, false)
	if sha := cleanupGit(t, r.main, "rev-parse", "refs/heads/bug/picked"); sha != head {
		t.Fatalf("branch lost: %s", sha)
	}
}

// Case 3: tracked changes and untracked non-ignored files keep a worktree;
// ignored caches do not count as dirty.
func TestWorktreeCleanupKeepsDirtyWorktrees(t *testing.T) {
	r := newCleanupRepo(t)
	tracked := r.queuePath("queue-aaaa0003")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", tracked, "tasks-hub")
	writeFixtureFile(t, filepath.Join(tracked, "app.txt"), "edited\n")
	untracked := r.queuePath("queue-aaaa0004")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", untracked, "tasks-hub")
	writeFixtureFile(t, filepath.Join(untracked, "notes.txt"), "keep me\n")
	ignored := r.queuePath("queue-aaaa0005")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", ignored, "tasks-hub")
	writeFixtureFile(t, filepath.Join(ignored, ".build", "cache", "blob"), "cache\n")

	got := r.sweep(t, true, nil, nil)
	requireDecision(t, got, tracked, "kept", keepDirty)
	requireDecision(t, got, untracked, "kept", keepDirty)
	requireDecision(t, got, ignored, "removed", "")
	if data, err := os.ReadFile(filepath.Join(tracked, "app.txt")); err != nil || string(data) != "edited\n" {
		t.Fatalf("tracked edit lost: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(untracked, "notes.txt")); err != nil || string(data) != "keep me\n" {
		t.Fatalf("untracked file lost: %q %v", data, err)
	}
	receipts := readCleanupReceipts(t)
	for _, path := range []string{tracked, untracked} {
		if rec, ok := receiptFor(receipts, path); !ok || rec.Action != "kept" || rec.Reason != keepDirty || rec.Detail == "" {
			t.Fatalf("kept receipt for %s: %+v %v", path, rec, ok)
		}
	}
}

// Case 4: a branch commit on neither tasks-hub nor a remote is kept until it
// is pushed.
func TestWorktreeCleanupKeepsUnpushedBranchUntilPushed(t *testing.T) {
	r := newCleanupRepo(t)
	w := r.branchWorktree(t, r.queuePath("queue-aaaa0006"), "feat/unpushed")
	got := r.sweep(t, true, nil, nil)
	d := requireDecision(t, got, w, "kept", keepUnpushed)
	if !strings.Contains(d.Detail, "not on tasks-hub") {
		t.Fatalf("detail %q", d.Detail)
	}
	requireExists(t, w, true)

	remote := filepath.Join(r.root, "remote.git")
	cleanupGit(t, r.root, "init", "-q", "--bare", remote)
	cleanupGit(t, r.main, "remote", "add", "origin", remote)
	cleanupGit(t, w, "push", "-q", "origin", "feat/unpushed")
	got = r.sweep(t, true, nil, nil)
	d = requireDecision(t, got, w, "removed", "")
	if !strings.Contains(d.Detail, "pushed to origin/feat/unpushed") {
		t.Fatalf("detail %q", d.Detail)
	}
	requireExists(t, w, false)
	cleanupGit(t, r.main, "rev-parse", "--verify", "refs/heads/feat/unpushed")
}

// Case 5: a detached commit reachable from no ref is kept, even though the
// worktree is clean, so gc cannot drop a receipt SHA.
func TestWorktreeCleanupKeepsDetachedUnreachableCommit(t *testing.T) {
	r := newCleanupRepo(t)
	w := r.queuePath("queue-aaaa0007")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", w, "tasks-hub")
	writeFixtureFile(t, filepath.Join(w, "detached.txt"), "x\n")
	cleanupGit(t, w, "add", ".")
	cleanupGit(t, w, "commit", "-q", "-m", "detached work")
	got := r.sweep(t, true, nil, nil)
	d := requireDecision(t, got, w, "kept", keepUnpushed)
	if !strings.Contains(d.Detail, "on no branch or remote") {
		t.Fatalf("detail %q", d.Detail)
	}
	requireExists(t, w, true)
}

// Case 6: a running item's clean, integrated worktree is in use through the
// entry cwd, a live agent cwd, or a Claude scratchpad of a session there.
func TestWorktreeCleanupKeepsRunningItemWorktrees(t *testing.T) {
	r := newCleanupRepo(t)
	host := spawn.Host()
	entryCwd := r.queuePath("queue-aaaa0008")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", entryCwd, "tasks-hub")
	nested := filepath.Join(entryCwd, ".build", "verify-aaaa")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", nested, "tasks-hub")
	agentCwd := r.queuePath("queue-aaaa0009")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", agentCwd, "tasks-hub")
	agentSub := filepath.Join(agentCwd, "hub")
	if err := os.MkdirAll(agentSub, 0755); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(r.root, "tmp", "claude-501", claudeScratchKey(agentCwd), "0f6c3e0e-session", "scratchpad", "verify-bbbb")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", scratch, "tasks-hub")
	otherScratch := filepath.Join(r.root, "tmp", "claude-501", claudeScratchKey(r.queuePath("queue-gone")), "1a2b-session", "scratchpad", "verify-cccc")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", otherScratch, "tasks-hub")
	// A main-checkout cwd is shared and protects only scratchpads keyed by it.
	shared := r.queuePath("queue-aaaa0010")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", shared, "tasks-hub")

	entries := []api.TeamQueueEntry{
		{ID: "tqe_running", State: "running", Host: host, Cwd: entryCwd},
		{ID: "tqe_elsewhere", State: "running", Host: host + "-other", Cwd: shared},
	}
	agents := []api.Agent{
		{ID: "agt_live", Host: host, Status: "running", Cwd: agentSub},
		{ID: "agt_main", Host: host, Status: "running", Cwd: r.main},
		{ID: "agt_gone", Host: host, Status: api.AgentClosed, Cwd: shared},
	}
	inUse, _ := worktreeProtection(host, entries, agents)
	got := r.sweep(t, true, inUse, nil)
	requireDecision(t, got, entryCwd, "kept", keepInUse)
	requireDecision(t, got, nested, "kept", keepInUse)
	requireDecision(t, got, agentCwd, "kept", keepInUse)
	requireDecision(t, got, scratch, "kept", keepInUse)
	requireDecision(t, got, otherScratch, "removed", "")
	requireDecision(t, got, shared, "removed", "")
	for _, p := range []string{entryCwd, nested, agentCwd, scratch} {
		requireExists(t, p, true)
	}
}

// Case 7: locked, mid-operation, cited and nested-kept worktrees are kept
// with their reasons.
func TestWorktreeCleanupKeepsLockedCitedOperationAndNested(t *testing.T) {
	r := newCleanupRepo(t)
	locked := r.queuePath("queue-aaaa0011")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", locked, "tasks-hub")
	cleanupGit(t, r.main, "worktree", "lock", "--reason", "fixture", locked)
	t.Cleanup(func() { exec.Command("git", "-C", r.main, "worktree", "unlock", locked).Run() })

	release := filepath.Join(r.main, ".build", "releases", "release-aaaa")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", release, "tasks-hub")
	writeFixtureFile(t, filepath.Join(r.main, "docs", "release.md"), "Rollback package: `.build/releases/release-aaaa/.build/receipt.json`.\n")
	cleanupGit(t, r.main, "add", "docs/release.md")
	cleanupGit(t, r.main, "commit", "-q", "-m", "cite release")

	receiptCited := r.queuePath("queue-aaaa0012")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", receiptCited, "tasks-hub")

	operation := r.queuePath("queue-aaaa0013")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", operation, "tasks-hub")
	writeFixtureFile(t, filepath.Join(worktreeAdminDir(operation), "MERGE_HEAD"), cleanupGit(t, r.main, "rev-parse", "HEAD")+"\n")

	parent := r.queuePath("queue-aaaa0014")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", parent, "tasks-hub")
	child := filepath.Join(parent, ".build", "verify-dddd")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", child, "tasks-hub")
	writeFixtureFile(t, filepath.Join(child, "app.txt"), "child edit\n")

	removableParent := r.queuePath("queue-aaaa0015")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", removableParent, "tasks-hub")
	removableChild := filepath.Join(removableParent, ".build", "verify-eeee")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", removableChild, "tasks-hub")

	evidence := []worktreeEvidence{{Source: "queue entry tqe_cited acceptance evidence", Text: "matrix report at " + receiptCited + "/.build/verify/report.json"}}
	got := r.sweep(t, true, nil, evidence)
	requireDecision(t, got, locked, "kept", keepLocked)
	d := requireDecision(t, got, release, "kept", keepEvidence)
	if d.Detail != "cited by docs/release.md" {
		t.Fatalf("evidence detail %q", d.Detail)
	}
	d = requireDecision(t, got, receiptCited, "kept", keepEvidence)
	if !strings.Contains(d.Detail, "tqe_cited") {
		t.Fatalf("entry evidence detail %q", d.Detail)
	}
	requireDecision(t, got, operation, "kept", keepOperation)
	requireDecision(t, got, child, "kept", keepDirty)
	d = requireDecision(t, got, parent, "kept", keepNested)
	if !strings.Contains(d.Detail, child) {
		t.Fatalf("nested detail %q", d.Detail)
	}
	requireDecision(t, got, removableChild, "removed", "")
	requireDecision(t, got, removableParent, "removed", "")
	for _, p := range []string{locked, release, receiptCited, operation, parent, child} {
		requireExists(t, p, true)
	}
	receipts := readCleanupReceipts(t)
	for path, reason := range map[string]string{locked: keepLocked, release: keepEvidence, operation: keepOperation, parent: keepNested} {
		if rec, ok := receiptFor(receipts, path); !ok || rec.Action != "kept" || rec.Reason != reason {
			t.Fatalf("receipt for %s: %+v %v", path, rec, ok)
		}
	}
	// A kept outcome is recorded once per path, reason and head.
	before := len(readCleanupReceipts(t))
	r.sweep(t, true, nil, evidence)
	if after := len(readCleanupReceipts(t)); after != before {
		t.Fatalf("repeated kept receipts: %d -> %d", before, after)
	}
}

// The sweep's recent rule keeps a worktree whose Git state changed within
// --min-idle; closeout does not apply it.
func TestWorktreeCleanupSweepKeepsRecentWorktrees(t *testing.T) {
	r := newCleanupRepo(t)
	w := r.queuePath("queue-aaaa0016")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", w, "tasks-hub")
	decisions, err := cleanupWorktrees(context.Background(), worktreeCleanupInputs{Repo: r.main, MinIdle: time.Hour, Now: time.Now()})
	if err != nil || len(decisions) != 1 || decisions[0].Action != "kept" || decisions[0].Reason != keepRecent {
		t.Fatalf("recent decision %+v %v", decisions, err)
	}
	decisions, err = cleanupWorktrees(context.Background(), worktreeCleanupInputs{Repo: r.main, MinIdle: time.Hour, Now: time.Now().Add(2 * time.Hour)})
	if err != nil || len(decisions) != 1 || decisions[0].Action != "would-remove" {
		t.Fatalf("idle decision %+v %v", decisions, err)
	}
}

type cleanupHub struct {
	mu       sync.Mutex
	requests []string
	writes   []string
	tasks    []api.Task
	details  map[string]api.TaskDetail
	queues   map[string]api.TeamQueueList
	byHost   api.TeamQueueList
}

func (h *cleanupHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, req.Method+" "+req.URL.Path)
	if req.Method != http.MethodGet {
		h.writes = append(h.writes, req.Method+" "+req.URL.Path)
		http.Error(w, `{"error":"fixture hub is read-only"}`, http.StatusConflict)
		return
	}
	var body any
	switch path := req.URL.Path; {
	case path == "/v1/team-queues":
		body = h.byHost
	case path == "/v1/tasks":
		body = api.TaskList{Tasks: h.tasks}
	case strings.HasSuffix(path, "/team-queue"):
		body = h.queues[strings.TrimSuffix(strings.TrimPrefix(path, "/v1/tasks/"), "/team-queue")]
	case strings.HasPrefix(path, "/v1/tasks/"):
		detail, ok := h.details[strings.TrimPrefix(path, "/v1/tasks/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		body = detail
	default:
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (h *cleanupHub) snapshot() ([]string, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...), append([]string(nil), h.writes...)
}

// countWorktreeGit counts Git calls made by the cleanup for the test.
func countWorktreeGit(t *testing.T) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	original := worktreeGit
	worktreeGit = func(ctx context.Context, dir string, args ...string) (string, error) {
		calls.Add(1)
		return original(ctx, dir, args...)
	}
	t.Cleanup(func() { worktreeGit = original })
	return &calls
}

// Case 8: the runner removes a finished, accepted entry's integrated
// worktrees in one tick with no hub write; the next tick costs only stats,
// and a running entry's worktree in the same project is untouched.
func TestWorktreeCleanupRunnerRemovesFinishedEntryWorktrees(t *testing.T) {
	r := newCleanupRepo(t)
	const host = "fixture"
	const task = "tsk_c1ea0c1ea0c1ea00"
	finishedCwd := r.branchWorktree(t, r.queuePath("queue-bbbb0001"), "bug/finished")
	commit := cleanupGit(t, finishedCwd, "rev-parse", "HEAD")
	cleanupGit(t, r.main, "merge", "-q", "--ff-only", "bug/finished")
	cache := filepath.Join(finishedCwd, ".build", "go", "pkg", "mod", "cache")
	writeFixtureFile(t, filepath.Join(cache, "x.zip"), "zip\n")
	if err := os.Chmod(cache, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = makeWorktreeWritable(finishedCwd) })
	verifier := filepath.Join(r.root, "tmp", "claude-501", claudeScratchKey(finishedCwd), "5e55-session", "scratchpad", "verify-ffff")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", verifier, commit)
	runningCwd := r.queuePath("queue-bbbb0002")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", runningCwd, "tasks-hub")
	unrelated := r.queuePath("queue-bbbb0003")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", unrelated, "tasks-hub")

	finished := api.TeamQueueEntry{ID: "tqe_finished_case8", TaskID: task, ItemID: "wi_f1f1f1f1f1f1f1f1", State: "finished", Host: host, Cwd: finishedCwd, Repository: r.common,
		Acceptance:  &api.TeamIntegrationAcceptance{Repository: r.common, Worktree: finishedCwd, Branch: "bug/finished", Commit: commit, Evidence: "verification receipt vr_1"},
		Integration: &api.TeamIntegrationReady{Repository: r.common, Worktree: finishedCwd, Branch: "bug/finished", Commit: commit, Evidence: "verification receipt vr_1"}}
	running := api.TeamQueueEntry{ID: "tqe_running_case8", TaskID: task, ItemID: "wi_e2e2e2e2e2e2e2e2", State: "running", Host: host, Cwd: runningCwd}
	hub := &cleanupHub{
		byHost: api.TeamQueueList{Entries: []api.TeamQueueEntry{running}},
		queues: map[string]api.TeamQueueList{task: {Entries: []api.TeamQueueEntry{finished, running}}},
		details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen, PauseState: api.ProjectPausePaused}, Agents: []api.Agent{
			{ID: "agt_verifier", Host: host, Status: api.AgentClosed, Cwd: finishedCwd, WorkItem: &api.AgentWorkItemBinding{ItemTaskID: task, ItemID: finished.ItemID}},
			{ID: "agt_running", Host: host, Status: "running", Cwd: runningCwd, WorkItem: &api.AgentWorkItemBinding{ItemTaskID: task, ItemID: running.ItemID}},
		}}},
	}
	server := httptest.NewServer(hub)
	defer server.Close()
	c, err := api.NewClient(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	e := env{hub: server.URL}
	runner := teamRunner{worktrees: closeoutWorktrees}
	calls := countWorktreeGit(t)
	ctx := context.Background()

	if err := runner.tick(ctx, e, c, host); err != nil {
		t.Fatal(err)
	}
	requireExists(t, finishedCwd, false)
	requireExists(t, verifier, false)
	requireExists(t, runningCwd, true)
	requireExists(t, unrelated, true)
	if sha := cleanupGit(t, r.main, "rev-parse", "refs/heads/bug/finished"); sha != commit {
		t.Fatalf("accepted branch lost: %s", sha)
	}
	if calls.Load() == 0 {
		t.Fatal("first tick made no Git call")
	}
	receipts := readCleanupReceipts(t)
	for _, p := range []string{finishedCwd, verifier} {
		rec, ok := receiptFor(receipts, p)
		if !ok || rec.Action != "removed" || rec.Source != "closeout" || rec.EntryID != finished.ID || rec.ItemID != finished.ItemID || rec.TaskID != task {
			t.Fatalf("closeout receipt for %s: %+v %v", p, rec, ok)
		}
	}
	if _, ok := receiptFor(receipts, runningCwd); ok {
		t.Fatal("closeout examined the running entry's worktree")
	}
	first, writes := hub.snapshot()
	if len(writes) != 0 {
		t.Fatalf("cleanup wrote to the hub: %v", writes)
	}

	// Later ticks, even past the re-check interval, only stat the gone paths.
	closeoutWorktreeChecks.Delete(finished.ID)
	calls.Store(0)
	if err := runner.tick(ctx, e, c, host); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("second tick made %d Git calls", n)
	}
	all, writes := hub.snapshot()
	second := all[len(first):]
	baseline := []string{"GET /v1/team-queues", "GET /v1/tasks/" + task + "/team-queue", "GET /v1/tasks/" + task}
	if len(writes) != 0 || strings.Join(second, ",") != strings.Join(baseline, ",") {
		t.Fatalf("second tick requests %v writes %v, want only the tick's own reads %v", second, writes, baseline)
	}
	if n := len(first) - len(baseline); n != 1 || first[len(first)-1] != "GET /v1/tasks/"+task {
		t.Fatalf("first tick requests %v: cleanup should add one project read", first)
	}
}

// Closeout keeps an accepted worktree whose release has not landed and
// reports it once; a running item's worktree is never a candidate.
func TestWorktreeCleanupRunnerKeepsUnreleasedWorktree(t *testing.T) {
	r := newCleanupRepo(t)
	const host = "fixture"
	const task = "tsk_c1ea0c1ea0c1ea01"
	w := r.branchWorktree(t, r.queuePath("queue-bbbb0004"), "bug/waiting")
	commit := cleanupGit(t, w, "rev-parse", "HEAD")
	finished := api.TeamQueueEntry{ID: "tqe_waiting_case8b", TaskID: task, ItemID: "wi_a3a3a3a3a3a3a3a3", State: "finished", Host: host, Cwd: r.main, Repository: r.common,
		Acceptance: &api.TeamIntegrationAcceptance{Repository: r.common, Worktree: w, Branch: "bug/waiting", Commit: commit}}
	// A serial entry shares the main checkout; that attributes nothing more.
	shared := r.queuePath("queue-bbbb0005")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", shared, "tasks-hub")
	hub := &cleanupHub{
		queues:  map[string]api.TeamQueueList{task: {Entries: []api.TeamQueueEntry{finished}}},
		details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen}}},
	}
	server := httptest.NewServer(hub)
	defer server.Close()
	c, err := api.NewClient(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	restore := captureStderr(t, &stderr)
	err = closeoutWorktrees(context.Background(), c, host, api.TeamQueueList{}, hub.queues[task], finished)
	restore()
	if err != nil {
		t.Fatal(err)
	}
	requireExists(t, w, true)
	requireExists(t, shared, true)
	rec, ok := receiptFor(readCleanupReceipts(t), w)
	if !ok || rec.Action != "kept" || rec.Reason != keepUnpushed {
		t.Fatalf("kept receipt %+v %v", rec, ok)
	}
	if !strings.Contains(stderr.String(), "kept unpushed "+w) {
		t.Fatalf("stderr %q", stderr.String())
	}
	if _, ok := receiptFor(readCleanupReceipts(t), shared); ok {
		t.Fatal("closeout attributed a main-checkout worktree to the item")
	}
	// Within the interval the entry is not re-examined.
	calls := countWorktreeGit(t)
	if err := closeoutWorktrees(context.Background(), c, host, api.TeamQueueList{}, hub.queues[task], finished); err != nil || calls.Load() != 0 {
		t.Fatalf("re-examined within the interval: calls=%d err=%v", calls.Load(), err)
	}
	// After the release lands, the next look removes it.
	cleanupGit(t, r.main, "merge", "-q", "--ff-only", "bug/waiting")
	closeoutWorktreeChecks.Delete(finished.ID)
	if err := closeoutWorktrees(context.Background(), c, host, api.TeamQueueList{}, hub.queues[task], finished); err != nil {
		t.Fatal(err)
	}
	requireExists(t, w, false)
	if _, writes := hub.snapshot(); len(writes) != 0 {
		t.Fatalf("closeout wrote to the hub: %v", writes)
	}
}

func captureStderr(t *testing.T, into *strings.Builder) func() {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = write
	done := make(chan struct{})
	go func() {
		data, _ := io.ReadAll(read)
		into.Write(data)
		close(done)
	}()
	return func() {
		os.Stderr = original
		write.Close()
		<-done
		read.Close()
	}
}

func captureSweepStdout(t *testing.T, run func() error) (string, error) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(read)
		done <- string(data)
	}()
	runErr := run()
	os.Stdout = original
	write.Close()
	out := <-done
	read.Close()
	return out, runErr
}

// Case 9: the sweep command is a dry run by default, --apply removes exactly
// the would-remove set and records receipts, --json has the documented shape,
// and an unreachable hub stops it before anything changes.
func TestWorktreeCleanupSweepCommand(t *testing.T) {
	r := newCleanupRepo(t)
	host := spawn.Host()
	const task = "tsk_c1ea0c1ea0c1ea02"
	merged := r.branchWorktree(t, r.queuePath("queue-cccc0001"), "bug/sweep-merged")
	cleanupGit(t, r.main, "merge", "-q", "--ff-only", "bug/sweep-merged")
	stale := r.queuePath("queue-cccc0002")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", stale, "tasks-hub")
	dirty := r.queuePath("queue-cccc0003")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", dirty, "tasks-hub")
	writeFixtureFile(t, filepath.Join(dirty, "draft.txt"), "draft\n")
	unpushed := r.branchWorktree(t, r.queuePath("queue-cccc0004"), "feat/sweep-unpushed")
	running := r.queuePath("queue-cccc0005")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", running, "tasks-hub")
	missing := r.queuePath("queue-cccc0006")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", missing, "tasks-hub")
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}
	hub := &cleanupHub{
		tasks:   []api.Task{{ID: task, Status: api.TaskOpen}},
		details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen}}},
		queues:  map[string]api.TeamQueueList{task: {Entries: []api.TeamQueueEntry{{ID: "tqe_sweep_running", TaskID: task, State: "running", Host: host, Cwd: running}}}},
	}
	server := httptest.NewServer(hub)
	defer server.Close()
	e := env{hub: server.URL}
	listBefore := cleanupGit(t, r.main, "worktree", "list", "--porcelain")
	// A touched tracked file makes a plain git status rewrite the index.
	past := time.Now().Add(-48 * time.Hour)
	staleIndex := filepath.Join(worktreeAdminDir(stale), "index")
	if err := os.Chtimes(staleIndex, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(stale, "app.txt"), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	// An unreachable hub stops the sweep before anything is touched.
	_, err := captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(env{hub: "http://127.0.0.1:1"}, []string{"--cwd", r.main, "--min-idle", "0s", "--apply"})
	})
	if err == nil || !strings.Contains(err.Error(), "sweep needs the hub") {
		t.Fatalf("unreachable hub error %v", err)
	}
	if list := cleanupGit(t, r.main, "worktree", "list", "--porcelain"); list != listBefore {
		t.Fatalf("unreachable hub changed worktrees:\n%s", list)
	}

	var dry worktreeSweepReport
	out, err := captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(e, []string{"--cwd", r.main, "--min-idle", "0s", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var shape map[string]any
	if err := json.Unmarshal([]byte(out), &shape); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	for _, key := range []string{"worktrees", "totals"} {
		if _, ok := shape[key]; !ok {
			t.Fatalf("json lacks %s: %s", key, out)
		}
	}
	first := shape["worktrees"].([]any)[0].(map[string]any)
	for _, key := range []string{"path", "branch", "head", "action", "reason", "detail", "bytes"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("worktree json lacks %s: %v", key, first)
		}
	}
	if err := json.Unmarshal([]byte(out), &dry); err != nil {
		t.Fatal(err)
	}
	if list := cleanupGit(t, r.main, "worktree", "list", "--porcelain"); list != listBefore {
		t.Fatalf("dry run changed worktrees:\n%s", list)
	}
	if receipts := readCleanupReceipts(t); len(receipts) != 0 {
		t.Fatalf("dry run wrote receipts: %+v", receipts)
	}
	if info, err := os.Stat(staleIndex); err != nil || !info.ModTime().Equal(past) {
		t.Fatalf("dry run rewrote an index: %v %v", info.ModTime(), err)
	}
	byPath := map[string]worktreeDecision{}
	var wouldRemove []string
	for _, d := range dry.Worktrees {
		byPath[d.Path] = d
		if d.Action == "would-remove" {
			wouldRemove = append(wouldRemove, d.Path)
			if d.Bytes <= 0 {
				t.Fatalf("would-remove without size %+v", d)
			}
		}
	}
	sort.Strings(wouldRemove)
	if strings.Join(wouldRemove, ",") != strings.Join([]string{merged, stale}, ",") {
		t.Fatalf("would-remove %v", wouldRemove)
	}
	requireDecision(t, byPath, dirty, "kept", keepDirty)
	requireDecision(t, byPath, unpushed, "kept", keepUnpushed)
	requireDecision(t, byPath, running, "kept", keepInUse)
	requireDecision(t, byPath, missing, "would-prune", keepMissing)
	if dry.Totals.Actions["would-remove"] != 2 || dry.Totals.Kept[keepDirty] != 1 || dry.Totals.Kept[keepUnpushed] != 1 || dry.Totals.Kept[keepInUse] != 1 || dry.Totals.Bytes <= 0 {
		t.Fatalf("dry totals %+v", dry.Totals)
	}

	text, err := captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(e, []string{"--cwd", r.main, "--min-idle", "0s", "--apply"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"removed " + merged, "removed " + stale, "kept dirty " + dirty, "kept unpushed " + unpushed, "kept in-use " + running, "pruned " + missing, "totals: removed=2 pruned=1 kept=3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("apply output lacks %q:\n%s", want, text)
		}
	}
	for _, p := range []string{merged, stale} {
		requireExists(t, p, false)
	}
	for _, p := range []string{dirty, unpushed, running} {
		requireExists(t, p, true)
	}
	if list := cleanupGit(t, r.main, "worktree", "list", "--porcelain"); strings.Contains(list, missing) || strings.Contains(list, merged) || !strings.Contains(list, dirty) {
		t.Fatalf("worktree list after apply:\n%s", list)
	}
	receipts := readCleanupReceipts(t)
	for path, action := range map[string]string{merged: "removed", stale: "removed", missing: "pruned", dirty: "kept", unpushed: "kept", running: "kept"} {
		if rec, ok := receiptFor(receipts, path); !ok || rec.Action != action || rec.Source != "sweep" {
			t.Fatalf("receipt for %s: %+v %v", path, rec, ok)
		}
	}
	if _, writes := hub.snapshot(); len(writes) != 0 {
		t.Fatalf("sweep wrote to the hub: %v", writes)
	}
}

func TestWorktreeCleanupHelpers(t *testing.T) {
	if got := claudeScratchKey("/Users/me/projects/tailterm/.build/worktrees/queue-3d682859"); got != "-Users-me-projects-tailterm--build-worktrees-queue-3d682859" {
		t.Fatalf("key %q", got)
	}
	cwd := "/Users/me/projects/tailterm/.build/worktrees/queue-3d682859"
	if !underClaudeScratch("/private/tmp/claude-501/"+claudeScratchKey(cwd)+"/1002dc56/scratchpad/verify-dd46c24", cwd) {
		t.Fatal("scratchpad not matched")
	}
	if underClaudeScratch("/private/tmp/claude-501/"+claudeScratchKey(cwd)+"x/1002dc56/scratchpad/verify-dd46c24", cwd) {
		t.Fatal("key prefix matched")
	}
	if !pathWithin("/a/b/c", "/a/b") || pathWithin("/a/bc", "/a/b") || !pathWithin("/a/b", "/a/b") {
		t.Fatal("pathWithin")
	}
	list := parseWorktreeList("worktree /r\x00HEAD aaa\x00branch refs/heads/tasks-hub\x00\x00worktree /r/.build/w\x00HEAD bbb\x00detached\x00locked fixture\x00\x00worktree /tmp/x\x00HEAD ccc\x00detached\x00prunable gitdir file points to non-existent location\x00\x00")
	if len(list) != 3 || !list[0].Main || list[0].Branch != "refs/heads/tasks-hub" || !list[1].Locked || !list[1].Detached || list[1].Main || !list[2].Prunable {
		t.Fatalf("parsed %+v", list)
	}
}

// Closeout keeps a finished entry's clean, integrated worktree while a
// running entry or a live agent of another item still works in it.
func TestWorktreeCleanupCloseoutKeepsWorktreeInUse(t *testing.T) {
	r := newCleanupRepo(t)
	const host = "fixture"
	const task = "tsk_c1ea0c1ea0c1ea03"
	reused := r.queuePath("queue-dddd0001")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", reused, "tasks-hub")
	visited := r.queuePath("queue-dddd0002")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", visited, "tasks-hub")
	if err := os.MkdirAll(filepath.Join(visited, "hub"), 0755); err != nil {
		t.Fatal(err)
	}
	head := cleanupGit(t, r.main, "rev-parse", "HEAD")
	byRunning := api.TeamQueueEntry{ID: "tqe_inuse_running", TaskID: task, ItemID: "wi_b4b4b4b4b4b4b4b4", State: "finished", Host: host, Cwd: reused, Repository: r.common,
		Acceptance: &api.TeamIntegrationAcceptance{Repository: r.common, Worktree: reused, Commit: head}}
	byAgent := api.TeamQueueEntry{ID: "tqe_inuse_agent", TaskID: task, ItemID: "wi_c5c5c5c5c5c5c5c5", State: "finished", Host: host, Cwd: visited, Repository: r.common,
		Acceptance: &api.TeamIntegrationAcceptance{Repository: r.common, Worktree: visited, Commit: head}}
	active := api.TeamQueueList{Entries: []api.TeamQueueEntry{{ID: "tqe_inuse_now", TaskID: task, ItemID: "wi_d6d6d6d6d6d6d6d6", State: "running", Host: host, Cwd: reused}}}
	project := api.TeamQueueList{Entries: []api.TeamQueueEntry{byRunning, byAgent, active.Entries[0]}}
	hub := &cleanupHub{details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen}, Agents: []api.Agent{
		{ID: "agt_other_item", Host: host, Status: "running", Cwd: filepath.Join(visited, "hub"), WorkItem: &api.AgentWorkItemBinding{ItemTaskID: task, ItemID: "wi_e7e7e7e7e7e7e7e7"}},
	}}}}
	server := httptest.NewServer(hub)
	defer server.Close()
	c, err := api.NewClient(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	restore := captureStderr(t, &stderr)
	for _, q := range []api.TeamQueueEntry{byRunning, byAgent} {
		if err := closeoutWorktrees(context.Background(), c, host, active, project, q); err != nil {
			restore()
			t.Fatal(err)
		}
	}
	restore()
	requireExists(t, reused, true)
	requireExists(t, visited, true)
	receipts := readCleanupReceipts(t)
	for _, p := range []string{reused, visited} {
		if rec, ok := receiptFor(receipts, p); !ok || rec.Action != "kept" || rec.Reason != keepInUse || rec.Source != "closeout" {
			t.Fatalf("closeout receipt for %s: %+v %v", p, rec, ok)
		}
		if !strings.Contains(stderr.String(), "kept in-use "+p) {
			t.Fatalf("stderr lacks kept in-use %s: %q", p, stderr.String())
		}
	}
}

// Git prunes every gone worktree at once and a worktree HEAD is a gc root, so
// a gone worktree holding an unintegrated commit defers all pruning.
func TestWorktreeCleanupDefersPruneForUnintegratedGoneWorktree(t *testing.T) {
	r := newCleanupRepo(t)
	orphan := r.queuePath("queue-aaaa0017")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", orphan, "tasks-hub")
	writeFixtureFile(t, filepath.Join(orphan, "orphan.txt"), "x\n")
	cleanupGit(t, orphan, "add", ".")
	cleanupGit(t, orphan, "commit", "-q", "-m", "orphan work")
	orphanHead := cleanupGit(t, orphan, "rev-parse", "HEAD")
	gone := r.queuePath("queue-aaaa0018")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", gone, "tasks-hub")
	for _, p := range []string{orphan, gone} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	got := r.sweep(t, true, nil, nil)
	d := requireDecision(t, got, orphan, "kept", keepUnpushed)
	if !strings.Contains(d.Detail, "directory is gone") {
		t.Fatalf("detail %q", d.Detail)
	}
	d = requireDecision(t, got, gone, "kept", keepMissing)
	if !strings.Contains(d.Detail, "prune deferred") {
		t.Fatalf("detail %q", d.Detail)
	}
	if list := cleanupGit(t, r.main, "worktree", "list", "--porcelain"); !strings.Contains(list, orphanHead) || !strings.Contains(list, gone) {
		t.Fatalf("pruned despite an unintegrated gone worktree:\n%s", list)
	}
}

// Review round 1 b1: git worktree remove deletes whatever sits under an
// ignored path, including a nested worktree or clone with unpushed work. Both
// closeout (whose team cwd is the shared main checkout) and the sweep must
// keep the accepted worktree as nested and leave the nested work intact.
func TestWorktreeCleanupKeepsWorktreeHoldingNestedWork(t *testing.T) {
	type nestedCase struct {
		name  string
		setup func(t *testing.T, r cleanupRepo, parent string) (nested, marker, childReason string)
	}
	cases := []nestedCase{
		{"dirty nested worktree", func(t *testing.T, r cleanupRepo, parent string) (string, string, string) {
			nested := filepath.Join(parent, ".build", "verify-x")
			cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", nested, "tasks-hub")
			writeFixtureFile(t, filepath.Join(nested, "app.txt"), "uncommitted edit\n")
			writeFixtureFile(t, filepath.Join(nested, "notes.txt"), "untracked work\n")
			return nested, filepath.Join(nested, "notes.txt"), keepDirty
		}},
		{"detached unreachable nested worktree", func(t *testing.T, r cleanupRepo, parent string) (string, string, string) {
			nested := filepath.Join(parent, ".build", "verify-y")
			cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", nested, "tasks-hub")
			writeFixtureFile(t, filepath.Join(nested, "extra.txt"), "x\n")
			cleanupGit(t, nested, "add", "extra.txt")
			cleanupGit(t, nested, "commit", "-q", "-m", "unpushed detached commit")
			return nested, filepath.Join(nested, "extra.txt"), keepUnpushed
		}},
		{"nested separate clone", func(t *testing.T, r cleanupRepo, parent string) (string, string, string) {
			clone := filepath.Join(parent, ".build", "clone")
			if err := os.MkdirAll(clone, 0755); err != nil {
				t.Fatal(err)
			}
			cleanupGit(t, clone, "init", "-q", "-b", "main")
			writeFixtureFile(t, filepath.Join(clone, "work.txt"), "unpushed clone work\n")
			cleanupGit(t, clone, "add", ".")
			cleanupGit(t, clone, "commit", "-q", "-m", "clone work")
			return clone, filepath.Join(clone, "work.txt"), ""
		}},
	}
	for _, tc := range cases {
		for _, mode := range []string{"closeout", "sweep"} {
			t.Run(tc.name+" "+mode, func(t *testing.T) {
				r := newCleanupRepo(t)
				parent := r.branchWorktree(t, r.queuePath("builder-wt"), "bug/accepted")
				commit := cleanupGit(t, parent, "rev-parse", "HEAD")
				cleanupGit(t, r.main, "merge", "-q", "--ff-only", "bug/accepted")
				nested, marker, childReason := tc.setup(t, r, parent)
				var got map[string]worktreeDecision
				if mode == "closeout" {
					const host = "fixture"
					task := "tsk_c1ea0c1ea0c1ea09"
					q := api.TeamQueueEntry{ID: "tqe_nested_" + strings.ReplaceAll(tc.name, " ", "_"), TaskID: task, ItemID: "wi_0101010101010101", State: "finished", Host: host, Cwd: r.main, Repository: r.common,
						Acceptance: &api.TeamIntegrationAcceptance{Repository: r.common, Worktree: parent, Branch: "bug/accepted", Commit: commit, Evidence: "verification receipt vr_9"}}
					hub := &cleanupHub{details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen}, Agents: []api.Agent{
						{ID: "agt_builder", Host: host, Status: api.AgentClosed, Cwd: r.main, WorkItem: &api.AgentWorkItemBinding{ItemTaskID: task, ItemID: q.ItemID}},
					}}}}
					server := httptest.NewServer(hub)
					defer server.Close()
					c, err := api.NewClient(server.URL, 5*time.Second)
					if err != nil {
						t.Fatal(err)
					}
					var stderr strings.Builder
					restore := captureStderr(t, &stderr)
					err = closeoutWorktrees(context.Background(), c, host, api.TeamQueueList{}, api.TeamQueueList{Entries: []api.TeamQueueEntry{q}}, q)
					restore()
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(stderr.String(), "kept nested "+parent) {
						t.Fatalf("stderr lacks kept nested: %q", stderr.String())
					}
					got = map[string]worktreeDecision{}
					for _, rec := range readCleanupReceipts(t) {
						got[rec.Path] = worktreeDecision{Path: rec.Path, Action: rec.Action, Reason: rec.Reason, Detail: rec.Detail}
					}
				} else {
					got = r.sweep(t, true, nil, nil)
				}
				d := requireDecision(t, got, parent, "kept", keepNested)
				if !strings.Contains(d.Detail, nested) {
					t.Fatalf("nested detail %q lacks %s", d.Detail, nested)
				}
				if childReason != "" {
					requireDecision(t, got, nested, "kept", childReason)
				}
				requireExists(t, marker, true)
				requireExists(t, parent, true)
			})
		}
	}
}

// Review round 1 f4: a removal that fails after the permission walk puts the
// read-only directories back as they were.
func TestWorktreeCleanupRestoresPermissionsWhenRemovalFails(t *testing.T) {
	r := newCleanupRepo(t)
	w := r.branchWorktree(t, r.queuePath("queue-aaaa0019"), "bug/refused")
	cleanupGit(t, r.main, "merge", "-q", "--ff-only", "bug/refused")
	mod := filepath.Join(w, ".build", "go", "pkg", "mod", "example.com")
	writeFixtureFile(t, filepath.Join(mod, "go.mod"), "module example.com\n")
	if err := os.Chmod(mod, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = makeWorktreeWritable(w) })
	original := worktreeGit
	worktreeGit = func(ctx context.Context, dir string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
			return "", exec.Command("false").Run()
		}
		return original(ctx, dir, args...)
	}
	t.Cleanup(func() { worktreeGit = original })
	got := r.sweep(t, true, nil, nil)
	requireDecision(t, got, w, "kept", keepFailed)
	info, err := os.Stat(mod)
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatalf("permissions not restored: %v %v", info.Mode(), err)
	}
	requireExists(t, w, true)
}

// Verifier checkouts under the artifacts tree and session temp folders. The
// artifacts root is the fixture repository's sibling, <repo>-artifacts, and
// the temp root is <fixture>/tmp/claude-501.

const (
	artifactItemA = "wi_a1a1a1a1a1a1a1a1"
	artifactItemB = "wi_b2b2b2b2b2b2b2b2"
	artifactItemC = "wi_c3c3c3c3c3c3c3c3"
)

func (r cleanupRepo) artifacts() string { return r.main + "-artifacts" }

func (r cleanupRepo) tempRoot() string { return filepath.Join(r.root, "tmp", "claude-501") }

// ignoreBuildOutput makes node_modules, dist and test-results ignored on
// tasks-hub, as they are in the real repository.
func (r cleanupRepo) ignoreBuildOutput(t *testing.T) {
	t.Helper()
	writeFixtureFile(t, filepath.Join(r.main, ".gitignore"), ".build/\nnode_modules/\ndist/\ntest-results/\n")
	cleanupGit(t, r.main, "add", ".gitignore")
	cleanupGit(t, r.main, "commit", "-q", "-m", "ignore build output")
}

// verifierCheckout adds a detached checkout of tasks-hub under the item's
// artifact folder, with ignored build output inside it.
func (r cleanupRepo) verifierCheckout(t *testing.T, item, name string) string {
	t.Helper()
	path := filepath.Join(r.artifacts(), item, name)
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", path, "tasks-hub")
	writeFixtureFile(t, filepath.Join(path, "node_modules", "left-pad", "index.js"), "module.exports = 1\n")
	writeFixtureFile(t, filepath.Join(path, "dist", "app.js"), "built\n")
	return path
}

func releasedArtifactEntry(item string) api.TeamQueueEntry {
	return api.TeamQueueEntry{ID: "tqe_rel_" + item[3:7], ItemID: item, State: "finished", Position: 1, UpdatedAt: "2026-10-01T10:00:00Z",
		Acceptance: &api.TeamIntegrationAcceptance{AcceptedAt: "2026-10-01T09:00:00Z"}, Release: &api.ReleaseJob{State: "released"}}
}

func acceptedArtifactEntry(item string, at time.Time) api.TeamQueueEntry {
	return api.TeamQueueEntry{ID: "tqe_acc_" + item[3:7], ItemID: item, State: "finished", Position: 1,
		Acceptance: &api.TeamIntegrationAcceptance{AcceptedAt: at.UTC().Format(time.RFC3339Nano)}}
}

// artifactSweep runs one cleanup pass with the artifact rules on.
func (r cleanupRepo) artifactSweep(t *testing.T, in worktreeCleanupInputs) map[string]worktreeDecision {
	t.Helper()
	in.Repo = r.main
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	if in.Items == nil {
		in.Items = map[string]artifactItemState{}
	}
	in.MeasureBytes = true
	in.Receipt = worktreeCleanupReceipt{Source: "sweep"}
	decisions, err := cleanupWorktrees(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]worktreeDecision{}
	for _, d := range decisions {
		out[d.Path] = d
	}
	return out
}

func readFixtureFiles(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s should still exist: %v", p, err)
		}
		out[p] = string(data)
	}
	return out
}

func readArtifactManifest(t *testing.T, path string) artifactManifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m artifactManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest %q: %v", data, err)
	}
	return m
}

// fixtureTree lists every file below the roots with its size and mode, so a
// dry run can be shown to change nothing.
func fixtureTree(t *testing.T, roots ...string) string {
	t.Helper()
	var lines []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			info, err := os.Lstat(p)
			if err != nil {
				return nil
			}
			size := info.Size()
			if d.IsDir() {
				size = 0
			}
			lines = append(lines, p+" "+info.Mode().String()+" "+strconv.FormatInt(size, 10))
			return nil
		})
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// Item state table: running wins, then the newest finished entry decides,
// and an item with no usable record has no state.
func TestWorktreeCleanupArtifactItemStates(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	accepted := func(id, item string, pos int64) api.TeamQueueEntry {
		q := acceptedArtifactEntry(item, at)
		q.ID, q.Position = id, pos
		return q
	}
	entries := []api.TeamQueueEntry{
		{ID: "tqe_run", ItemID: "wi_0000000000000001", State: "running"},
		{ID: "tqe_fail", ItemID: "wi_0000000000000002", State: "failed"},
		{ID: "tqe_released", ItemID: "wi_0000000000000003", State: "finished", Release: &api.ReleaseJob{State: "released"}},
		{ID: "tqe_published", ItemID: "wi_0000000000000004", State: "finished", Release: &api.ReleaseJob{State: "blocked", Published: true}},
		{ID: "tqe_owner", ItemID: "wi_0000000000000005", State: "finished", OwnerIntegration: &api.TeamQueueOwnerIntegration{At: "2026-10-01T08:00:00Z"}},
		accepted("tqe_accepted", "wi_0000000000000006", 1),
		{ID: "tqe_verified", ItemID: "wi_0000000000000007", State: "finished", Release: &api.ReleaseJob{State: "verified"}},
		{ID: "tqe_dismissed", ItemID: "wi_0000000000000008", State: "failed", ReleasedAt: "2026-10-01T08:00:00Z"},
		// A retry: the first try was released, the second is still running.
		{ID: "tqe_retry_old", ItemID: "wi_0000000000000009", State: "finished", Position: 1, Release: &api.ReleaseJob{State: "released"}},
		{ID: "tqe_retry_new", ItemID: "wi_0000000000000009", State: "queued", Position: 2},
		// A retry whose newer try is accepted only: the newest decides, in
		// either listing order.
		accepted("tqe_again_new", "wi_000000000000000a", 2),
		{ID: "tqe_again_old", ItemID: "wi_000000000000000a", State: "finished", Position: 1, Release: &api.ReleaseJob{State: "released"}},
		{ID: "tqe_bad_time", ItemID: "wi_000000000000000b", State: "finished", Acceptance: &api.TeamIntegrationAcceptance{AcceptedAt: "yesterday"}},
		accepted("tqe_agent", "wi_000000000000000c", 1),
	}
	agents := []api.Agent{
		{ID: "agt_closed", Name: "verifier", Status: api.AgentClosed, WorkItem: &api.AgentWorkItemBinding{ItemID: "wi_0000000000000006"}},
		{ID: "agt_open", Name: "reviewer", Status: "done", WorkItem: &api.AgentWorkItemBinding{ItemID: "wi_000000000000000c"}},
		{ID: "agt_unbound", Name: "handler", Status: "running"},
	}
	got := artifactItemStates(entries, agents)
	want := map[string]string{
		"wi_0000000000000001": artifactItemRunning,
		"wi_0000000000000002": artifactItemRunning,
		"wi_0000000000000003": artifactItemReleased,
		"wi_0000000000000004": artifactItemReleased,
		"wi_0000000000000005": artifactItemReleased,
		"wi_0000000000000006": artifactItemAccepted,
		"wi_0000000000000009": artifactItemRunning,
		"wi_000000000000000a": artifactItemAccepted,
		"wi_000000000000000c": artifactItemRunning,
	}
	if len(got) != len(want) {
		t.Fatalf("states %+v", got)
	}
	for item, state := range want {
		if got[item].State != state {
			t.Fatalf("%s: got %+v, want %s", item, got[item], state)
		}
	}
	if st := got["wi_0000000000000006"]; !st.At.Equal(at) || st.EntryID != "tqe_accepted" {
		t.Fatalf("accepted state %+v", st)
	}
	if st := got["wi_0000000000000005"]; st.BasisAt != "2026-10-01T08:00:00Z" {
		t.Fatalf("owner-integrated state %+v", st)
	}
	if st := got["wi_000000000000000c"]; !strings.Contains(st.Detail, "agent reviewer is done") {
		t.Fatalf("agent detail %+v", st)
	}
}

// T1: a released item's detached checkout is deleted whole; the receipts,
// logs and plan file beside it are byte-identical and a manifest joins them.
func TestWorktreeCleanupArtifactCheckoutRemovedAfterRelease(t *testing.T) {
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	checkout := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
	head := cleanupGit(t, checkout, "rev-parse", "HEAD")
	itemDir := filepath.Dir(checkout)
	receipt := filepath.Join(itemDir, "verifier-abc1234-logs", "receipt.json")
	plan := filepath.Join(itemDir, "plan-abc1234.json")
	setup := filepath.Join(itemDir, "setup-abc1234.out")
	// A sibling's path and the bare checkout directory are not citations.
	writeFixtureFile(t, receipt, `{"log":"`+checkout+`-logs/matrix.log","checkout":"`+checkout+`"}`+"\n")
	writeFixtureFile(t, plan, `{"cwd":"`+checkout+`"}`+"\n")
	writeFixtureFile(t, setup, "npm ci in "+checkout+"/node_modules\n")
	before := readFixtureFiles(t, receipt, plan, setup)
	items := artifactItemStates([]api.TeamQueueEntry{releasedArtifactEntry(artifactItemA)}, nil)
	manifest := checkout + ".removed.json"

	dry := r.artifactSweep(t, worktreeCleanupInputs{Items: items})
	d := requireDecision(t, dry, checkout, "would-remove", "")
	if d.Kind != kindArtifactCheckout || d.Bytes <= 0 || d.Detail != "item "+artifactItemA+" released" {
		t.Fatalf("dry decision %+v", d)
	}
	requireExists(t, checkout, true)
	requireExists(t, manifest, false)
	if receipts := readCleanupReceipts(t); len(receipts) != 0 {
		t.Fatalf("dry run wrote receipts %+v", receipts)
	}

	got := r.artifactSweep(t, worktreeCleanupInputs{Items: items, Apply: true})
	d = requireDecision(t, got, checkout, "removed", "")
	if d.Kind != kindArtifactCheckout || d.Manifest != manifest || d.Bytes <= 0 {
		t.Fatalf("removed decision %+v", d)
	}
	requireExists(t, checkout, false)
	if strings.Contains(cleanupGit(t, r.main, "worktree", "list"), checkout) {
		t.Fatal("git still lists the removed checkout")
	}
	after := readFixtureFiles(t, receipt, plan, setup)
	for p, content := range before {
		if after[p] != content {
			t.Fatalf("%s changed: %q", p, after[p])
		}
	}
	info, err := os.Stat(manifest)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("manifest mode %v %v", info, err)
	}
	m := readArtifactManifest(t, manifest)
	if m.Version != 1 || m.At == "" || m.Source != "sweep" || m.ItemID != artifactItemA || m.EntryID != "tqe_rel_a1a1" || m.Path != checkout || m.Head != head ||
		m.PinnedRef != "" || m.Basis != artifactItemReleased || m.BasisAt != "2026-10-01T10:00:00Z" || m.Bytes != d.Bytes || m.Files < 4 {
		t.Fatalf("manifest %+v", m)
	}
	entries := map[string]artifactManifestEntry{}
	for _, e := range m.Entries {
		entries[e.Name] = e
	}
	if e := entries["node_modules"]; e.Tracked || e.Files != 1 || e.Bytes <= 0 {
		t.Fatalf("node_modules entry %+v", e)
	}
	if e := entries["dist"]; e.Tracked || e.Bytes <= 0 {
		t.Fatalf("dist entry %+v", e)
	}
	if e := entries["app.txt"]; !e.Tracked || e.Files != 1 {
		t.Fatalf("app.txt entry %+v", e)
	}
	var lines []worktreeCleanupReceipt
	for _, rec := range readCleanupReceipts(t) {
		if rec.Kind == kindArtifactCheckout {
			lines = append(lines, rec)
		}
	}
	if len(lines) != 1 || lines[0].Path != checkout || lines[0].Action != "removed" || lines[0].Manifest != manifest || lines[0].Bytes != d.Bytes || lines[0].Head != head {
		t.Fatalf("artifact receipts %+v", lines)
	}
	// The commit is on tasks-hub, so nothing was pinned.
	if refs := cleanupGit(t, r.main, "for-each-ref", "refs/tailterm/"); refs != "" {
		t.Fatalf("unexpected pinned refs: %s", refs)
	}
}

// T2: an accepted item keeps its checkout until the acceptance is N old.
func TestWorktreeCleanupArtifactCheckoutAcceptedWaitsForRetention(t *testing.T) {
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	checkout := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
	now := time.Now()
	items := artifactItemStates([]api.TeamQueueEntry{acceptedArtifactEntry(artifactItemA, now.Add(-time.Hour))}, nil)
	before := fixtureTree(t, r.artifacts())

	got := r.artifactSweep(t, worktreeCleanupInputs{Items: items, Apply: true, AcceptedAfter: 24 * time.Hour, Now: now})
	d := requireDecision(t, got, checkout, "kept", keepRetention)
	if d.Detail != "accepted 1h ago; eligible after 24h" || d.Kind != kindArtifactCheckout {
		t.Fatalf("retention decision %+v", d)
	}
	if after := fixtureTree(t, r.artifacts()); after != before {
		t.Fatalf("a kept checkout changed the artifacts tree:\n%s", after)
	}
	// A shorter N makes the same acceptance old enough.
	short := r.artifactSweep(t, worktreeCleanupInputs{Items: items, AcceptedAfter: 30 * time.Minute, Now: now})
	requireDecision(t, short, checkout, "would-remove", "")

	got = r.artifactSweep(t, worktreeCleanupInputs{Items: items, Apply: true, AcceptedAfter: 24 * time.Hour, Now: now.Add(25 * time.Hour)})
	requireDecision(t, got, checkout, "removed", "")
	requireExists(t, checkout, false)
	if m := readArtifactManifest(t, checkout+".removed.json"); m.Basis != artifactItemAccepted || m.BasisAt == "" || m.Bytes <= 0 {
		t.Fatalf("manifest %+v", m)
	}
}

// T3: a running item's checkout is kept although its commit is on tasks-hub,
// whether an entry is active or a bound agent is not closed.
func TestWorktreeCleanupArtifactCheckoutRunningItemKept(t *testing.T) {
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	byEntry := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
	byAgent := r.verifierCheckout(t, artifactItemB, "verifier-def5678")
	running := api.TeamQueueEntry{ID: "tqe_running_t3", ItemID: artifactItemA, State: "running", Position: 2}
	items := artifactItemStates(
		[]api.TeamQueueEntry{releasedArtifactEntry(artifactItemA), running, releasedArtifactEntry(artifactItemB)},
		[]api.Agent{{ID: "agt_t3", Name: "verifier-t3", Status: "running", Host: "elsewhere", WorkItem: &api.AgentWorkItemBinding{ItemID: artifactItemB}}})
	got := r.artifactSweep(t, worktreeCleanupInputs{Items: items, Apply: true})
	d := requireDecision(t, got, byEntry, "kept", keepItemActive)
	if !strings.Contains(d.Detail, "tqe_running_t3 is running") {
		t.Fatalf("entry detail %q", d.Detail)
	}
	d = requireDecision(t, got, byAgent, "kept", keepItemActive)
	if !strings.Contains(d.Detail, "agent verifier-t3 is running") {
		t.Fatalf("agent detail %q", d.Detail)
	}
	for _, p := range []string{byEntry, byAgent} {
		requireExists(t, filepath.Join(p, "node_modules", "left-pad", "index.js"), true)
		requireExists(t, p+".removed.json", false)
	}
}

// T4: a checkout holding a file that queue evidence, a receipt file, a plan
// file or a tracked document refers to is kept whole, in every spelling the
// evidence patterns produce; a mention of the directory or a sibling is not
// a reference.
func TestWorktreeCleanupArtifactCheckoutReferencedEvidenceKept(t *testing.T) {
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	// The fixture root stands in for the home directory, so "~" spellings
	// reach the artifacts tree.
	t.Setenv("HOME", r.root)
	type citedCase struct {
		name   string
		item   string
		source string
		cite   func(t *testing.T, checkout string) []api.TeamQueueEntry
	}
	home := func(p string) string { return "~" + strings.TrimPrefix(p, r.root) }
	short := func(p string) string { return strings.TrimPrefix(p, "/private") }
	cases := []citedCase{
		{"queue acceptance evidence, absolute", "wi_e000000000000001", "queue entry tqe_cite_1 acceptance evidence", func(t *testing.T, checkout string) []api.TeamQueueEntry {
			return []api.TeamQueueEntry{{ID: "tqe_cite_1", Acceptance: &api.TeamIntegrationAcceptance{Evidence: "report: " + checkout + "/test-results/out.txt."}}}
		}},
		{"owner integration evidence, home", "wi_e000000000000002", "queue entry tqe_cite_2 owner integration evidence", func(t *testing.T, checkout string) []api.TeamQueueEntry {
			return []api.TeamQueueEntry{{ID: "tqe_cite_2", OwnerIntegration: &api.TeamQueueOwnerIntegration{Evidence: "see " + home(checkout) + "/test-results/out.txt"}}}
		}},
		{"receipt file, without /private", "wi_e000000000000003", "receipt.json", func(t *testing.T, checkout string) []api.TeamQueueEntry {
			writeFixtureFile(t, filepath.Join(filepath.Dir(checkout), "verifier-abc1234-logs", "receipt.json"), `{"output":"`+short(checkout)+`/test-results/out.txt"}`)
			return nil
		}},
		{"plan file, home", "wi_e000000000000004", "plan-abc1234.json", func(t *testing.T, checkout string) []api.TeamQueueEntry {
			writeFixtureFile(t, filepath.Join(filepath.Dir(checkout), "plan-abc1234.json"), `{"checks":[{"output":"`+home(checkout)+`/test-results/out.txt"}]}`)
			return nil
		}},
		{"verification plan file, missing tail", "wi_e000000000000005", "verification-plan-abc1234.json", func(t *testing.T, checkout string) []api.TeamQueueEntry {
			writeFixtureFile(t, filepath.Join(filepath.Dir(checkout), "verification-plan-abc1234.json"), `{"dir":"`+checkout+`/test-results/later run/trace.zip"}`)
			return nil
		}},
		{"tracked document", "wi_e000000000000006", "docs/release.md", func(t *testing.T, checkout string) []api.TeamQueueEntry {
			writeFixtureFile(t, filepath.Join(r.main, "docs", "release.md"), "Evidence: `"+checkout+"/test-results/out.txt`.\n")
			cleanupGit(t, r.main, "add", "docs/release.md")
			cleanupGit(t, r.main, "commit", "-q", "-m", "cite a checkout file")
			return nil
		}},
	}
	var entries []api.TeamQueueEntry
	checkouts := map[string]string{}
	for _, tc := range cases {
		checkout := r.verifierCheckout(t, tc.item, "verifier-abc1234")
		writeFixtureFile(t, filepath.Join(checkout, "test-results", "out.txt"), "kept output\n")
		entries = append(entries, releasedArtifactEntry(tc.item))
		entries = append(entries, tc.cite(t, checkout)...)
		checkouts[tc.name] = checkout
	}
	// Cited only by a sibling's path, by the bare directory and by a file
	// that is not in the checkout: removed.
	uncited := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
	writeFixtureFile(t, filepath.Join(filepath.Dir(uncited), "verifier-abc1234-logs", "receipt.json"), `{"log":"`+uncited+`-logs/x.log","cwd":"`+uncited+`","gone":"`+uncited+`/coverage/none.txt"}`)
	entries = append(entries, releasedArtifactEntry(artifactItemA),
		api.TeamQueueEntry{ID: "tqe_cite_dir", Acceptance: &api.TeamIntegrationAcceptance{Evidence: "matrix ran in " + uncited + " with logs in " + uncited + "-logs/x.log"}})
	_, evidence := worktreeProtection("fixture", entries, nil)

	got := r.artifactSweep(t, worktreeCleanupInputs{Items: artifactItemStates(entries, nil), Evidence: evidence, Apply: true})
	for _, tc := range cases {
		checkout := checkouts[tc.name]
		d := requireDecision(t, got, checkout, "kept", keepEvidence)
		if !strings.Contains(d.Detail, tc.source) || !strings.Contains(d.Detail, "test-results") {
			t.Fatalf("%s: detail %q lacks source %q or the cited path", tc.name, d.Detail, tc.source)
		}
		if data, err := os.ReadFile(filepath.Join(checkout, "test-results", "out.txt")); err != nil || string(data) != "kept output\n" {
			t.Fatalf("%s: referenced file lost: %q %v", tc.name, data, err)
		}
		requireExists(t, filepath.Join(checkout, "node_modules", "left-pad", "index.js"), true)
		requireExists(t, checkout+".removed.json", false)
	}
	requireDecision(t, got, uncited, "removed", "")
	requireExists(t, uncited, false)
	requireExists(t, uncited+".removed.json", true)
}

// T5: an untracked, non-ignored file keeps a released item's checkout.
func TestWorktreeCleanupDirtyArtifactCheckoutKept(t *testing.T) {
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	checkout := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
	writeFixtureFile(t, filepath.Join(checkout, "notes.txt"), "verifier notes\n")
	items := artifactItemStates([]api.TeamQueueEntry{releasedArtifactEntry(artifactItemA)}, nil)
	got := r.artifactSweep(t, worktreeCleanupInputs{Items: items, Apply: true})
	requireDecision(t, got, checkout, "kept", keepDirty)
	requireExists(t, filepath.Join(checkout, "notes.txt"), true)
	requireExists(t, checkout+".removed.json", false)
}

// T6: a superseded candidate's checkout, whose commit no branch or remote
// holds, is removed and its commit pinned so no receipt SHA can be collected.
func TestWorktreeCleanupSupersededCandidatePinned(t *testing.T) {
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	checkout := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
	writeFixtureFile(t, filepath.Join(checkout, "candidate.txt"), "superseded\n")
	cleanupGit(t, checkout, "add", "candidate.txt")
	cleanupGit(t, checkout, "commit", "-q", "-m", "superseded candidate")
	head := cleanupGit(t, checkout, "rev-parse", "HEAD")
	items := artifactItemStates([]api.TeamQueueEntry{releasedArtifactEntry(artifactItemA)}, nil)
	ref := "refs/tailterm/retired/" + artifactItemA + "/verifier-abc1234"

	requireDecision(t, r.artifactSweep(t, worktreeCleanupInputs{Items: items}), checkout, "would-remove", "")
	if refs := cleanupGit(t, r.main, "for-each-ref", "refs/tailterm/"); refs != "" {
		t.Fatalf("dry run pinned a ref: %s", refs)
	}
	got := r.artifactSweep(t, worktreeCleanupInputs{Items: items, Apply: true})
	requireDecision(t, got, checkout, "removed", "")
	requireExists(t, checkout, false)
	if sha := cleanupGit(t, r.main, "rev-parse", ref); sha != head {
		t.Fatalf("pinned ref %s, want %s", sha, head)
	}
	if m := readArtifactManifest(t, checkout+".removed.json"); m.PinnedRef != ref || m.Head != head {
		t.Fatalf("manifest %+v", m)
	}
	cleanupGit(t, r.main, "worktree", "prune")
	cleanupGit(t, r.main, "gc", "-q", "--prune=now")
	cleanupGit(t, r.main, "cat-file", "-e", head+"^{commit}")

	// A removal that then fails takes its manifest and its ref back.
	second := r.verifierCheckout(t, artifactItemB, "verifier-def5678")
	writeFixtureFile(t, filepath.Join(second, "candidate.txt"), "second\n")
	cleanupGit(t, second, "add", "candidate.txt")
	cleanupGit(t, second, "commit", "-q", "-m", "second superseded candidate")
	original := worktreeGit
	worktreeGit = func(ctx context.Context, dir string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
			return "", exec.Command("false").Run()
		}
		return original(ctx, dir, args...)
	}
	t.Cleanup(func() { worktreeGit = original })
	got = r.artifactSweep(t, worktreeCleanupInputs{Items: artifactItemStates([]api.TeamQueueEntry{releasedArtifactEntry(artifactItemB)}, nil), Apply: true})
	requireDecision(t, got, second, "kept", keepFailed)
	requireExists(t, second, true)
	requireExists(t, second+".removed.json", false)
	if refs := cleanupGit(t, r.main, "for-each-ref", "--format=%(refname)", "refs/tailterm/"); refs != ref {
		t.Fatalf("refs after a failed removal: %q", refs)
	}
}

// T7: a worktree under the artifacts tree with a branch checked out is a
// working checkout: the ordinary rules decide, even for a released item.
func TestWorktreeCleanupBranchCheckoutUnderArtifactsUnchanged(t *testing.T) {
	r := newCleanupRepo(t)
	builder := r.branchWorktree(t, filepath.Join(r.artifacts(), artifactItemA, "builder"), "feat/artifact-builder")
	items := artifactItemStates([]api.TeamQueueEntry{releasedArtifactEntry(artifactItemA)}, nil)
	got := r.artifactSweep(t, worktreeCleanupInputs{Items: items, Apply: true})
	d := requireDecision(t, got, builder, "kept", keepUnpushed)
	if d.Kind != "" {
		t.Fatalf("branch checkout treated as %q", d.Kind)
	}
	requireExists(t, builder, true)
	// A detached worktree in a folder not named for an item is ordinary too.
	other := filepath.Join(r.artifacts(), "verifier-75d72618", "checkout")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", other, "tasks-hub")
	got = r.artifactSweep(t, worktreeCleanupInputs{Items: items})
	if d := requireDecision(t, got, other, "would-remove", ""); d.Kind != "" || !strings.Contains(d.Detail, "tasks-hub") {
		t.Fatalf("non-item folder decision %+v", d)
	}
}

// T8: an item with no queue record is never treated as released.
func TestWorktreeCleanupArtifactCheckoutUnknownItemKept(t *testing.T) {
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	checkout := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
	items := artifactItemStates([]api.TeamQueueEntry{releasedArtifactEntry(artifactItemB)}, nil)
	got := r.artifactSweep(t, worktreeCleanupInputs{Items: items, Apply: true})
	d := requireDecision(t, got, checkout, "kept", keepRetention)
	if !strings.Contains(d.Detail, "no queue record") {
		t.Fatalf("detail %q", d.Detail)
	}
	requireExists(t, checkout, true)
	// With the artifact rules off it is an ordinary worktree, as before.
	requireDecision(t, r.sweep(t, false, nil, nil), checkout, "would-remove", "")
}

// T9: when the manifest cannot be written nothing is removed.
func TestWorktreeCleanupArtifactCheckoutManifestFailureKeepsCheckout(t *testing.T) {
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	checkout := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
	writeFixtureFile(t, filepath.Join(checkout, "candidate.txt"), "superseded\n")
	cleanupGit(t, checkout, "add", "candidate.txt")
	cleanupGit(t, checkout, "commit", "-q", "-m", "superseded candidate")
	itemDir := filepath.Dir(checkout)
	if err := os.Chmod(itemDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(itemDir, 0755) })
	items := artifactItemStates([]api.TeamQueueEntry{releasedArtifactEntry(artifactItemA)}, nil)
	got := r.artifactSweep(t, worktreeCleanupInputs{Items: items, Apply: true})
	d := requireDecision(t, got, checkout, "kept", keepFailed)
	if !strings.Contains(d.Detail, "write manifest") {
		t.Fatalf("detail %q", d.Detail)
	}
	requireExists(t, filepath.Join(checkout, "node_modules", "left-pad", "index.js"), true)
	requireExists(t, filepath.Join(checkout, "app.txt"), true)
	if !strings.Contains(cleanupGit(t, r.main, "worktree", "list"), checkout) {
		t.Fatal("git no longer lists the kept checkout")
	}
	if refs := cleanupGit(t, r.main, "for-each-ref", "refs/tailterm/"); refs != "" {
		t.Fatalf("a kept checkout left a pinned ref: %s", refs)
	}
}

// T10: a closed team's session temp folder is removed with a receipt; a
// folder in use, one sharing a key with a directory in use, the main
// checkout's, one holding a kept worktree, a cited one, a recent one and a
// symlink in the temp root all survive.
func TestWorktreeCleanupSessionTemp(t *testing.T) {
	r := newCleanupRepo(t)
	root := r.tempRoot()
	folder := func(cwd string) string { return filepath.Join(root, claudeScratchKey(cwd)) }
	session := func(cwd string) string {
		dir := folder(cwd)
		writeFixtureFile(t, filepath.Join(dir, "0f6c-session", "scratchpad", "notes.md"), "scratch\n")
		writeFixtureFile(t, filepath.Join(dir, "0f6c-session", "tasks", "b1.output"), strings.Repeat("x", 4096))
		return dir
	}
	// The closed team's queue worktree is already gone.
	closed := r.queuePath("queue-eeee0001")
	closedTemp := session(closed)
	live := r.queuePath("queue-eeee0002")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", live, "tasks-hub")
	liveTemp := session(live)
	// "queue-eeee.0003" and "queue-eeee-0003" share one key.
	sameKey := r.queuePath("queue-eeee.0003")
	sameKeyInUse := r.queuePath("queue-eeee-0003")
	sameKeyTemp := session(sameKey)
	mainTemp := session(r.main)
	nestedCwd := r.queuePath("queue-eeee0004")
	nestedTemp := session(nestedCwd)
	dirty := filepath.Join(nestedTemp, "0f6c-session", "scratchpad", "verify-dirty")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", dirty, "tasks-hub")
	writeFixtureFile(t, filepath.Join(dirty, "notes.txt"), "unsaved\n")
	// A scratchpad worktree that goes in the same pass does not hold its folder.
	clearedCwd := r.queuePath("queue-eeee0005")
	clearedTemp := session(clearedCwd)
	clean := filepath.Join(clearedTemp, "0f6c-session", "scratchpad", "verify-clean")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", clean, "tasks-hub")
	citedCwd := r.queuePath("queue-eeee0006")
	citedTemp := session(citedCwd)
	receiptCitedCwd := r.queuePath("queue-eeee0007")
	receiptCitedTemp := session(receiptCitedCwd)
	writeFixtureFile(t, filepath.Join(r.artifacts(), artifactItemB, "receipt.json"), `{"report":"`+receiptCitedTemp+`/0f6c-session/scratchpad/notes.md"}`)
	// A symlink in the temp root that points outside it.
	outside := filepath.Join(r.root, "outside")
	writeFixtureFile(t, filepath.Join(outside, "keep.txt"), "outside the temp root\n")
	linkCwd := r.queuePath("queue-eeee0008")
	if err := os.Symlink(outside, folder(linkCwd)); err != nil {
		t.Fatal(err)
	}
	// A directory that still exists outside every linked worktree is shared.
	shared := filepath.Join(r.root, "other-project")
	if err := os.MkdirAll(shared, 0755); err != nil {
		t.Fatal(err)
	}
	sharedTemp := session(shared)
	unlisted := session(r.queuePath("queue-eeee0009"))
	// A gone directory whose key equals the main checkout's names the main
	// checkout's shared folder, which is never attributed to a team.
	mainAlias := filepath.Join(r.root, "repo.")
	if claudeScratchKey(mainAlias+"x") == claudeScratchKey(r.main+"x") || sessionTempFolder(root, r.main, r.main) != "" || sessionTempFolder(root, r.main, "") != mainTemp {
		t.Fatal("main checkout temp folder guard")
	}
	mainAlias = filepath.Join(filepath.Dir(r.root), filepath.Base(r.root)+".repo")
	if claudeScratchKey(mainAlias) != claudeScratchKey(r.main) || sessionTempFolder(root, mainAlias, r.main) != "" {
		t.Fatalf("a directory sharing the main checkout's key is not guarded: %s", mainAlias)
	}

	in := worktreeCleanupInputs{
		TempRoot: root,
		InUse:    []string{live, sameKeyInUse},
		Evidence: []worktreeEvidence{{Source: "queue entry tqe_temp acceptance evidence", Text: "report at " + citedTemp + "/0f6c-session/scratchpad/notes.md"}},
		TempCwds: []sessionTempCwd{{Path: closed, ItemID: artifactItemA}, {Path: closed, ItemID: artifactItemA}, {Path: live, ItemID: artifactItemA}, {Path: sameKey, ItemID: artifactItemA},
			{Path: r.main, ItemID: artifactItemA}, {Path: nestedCwd, ItemID: artifactItemA}, {Path: clearedCwd, ItemID: artifactItemA}, {Path: citedCwd, ItemID: artifactItemA},
			{Path: receiptCitedCwd, ItemID: artifactItemB}, {Path: linkCwd, ItemID: artifactItemA}, {Path: shared, ItemID: artifactItemA}, {Path: mainAlias, ItemID: artifactItemA}, {Path: "relative/cwd"}},
	}
	// The sweep's recent rule keeps a folder that changed within --min-idle.
	recent := in
	recent.MinIdle = time.Hour
	d := requireDecision(t, r.artifactSweep(t, recent), closedTemp, "kept", keepRecent)
	if d.Kind != kindSessionTemp {
		t.Fatalf("recent decision %+v", d)
	}

	before := fixtureTree(t, root, outside)
	dry := r.artifactSweep(t, in)
	if d := requireDecision(t, dry, closedTemp, "would-remove", ""); d.Kind != kindSessionTemp || d.Bytes < 4096 {
		t.Fatalf("dry decision %+v", d)
	}
	requireDecision(t, dry, clearedTemp, "would-remove", "")
	if after := fixtureTree(t, root, outside); after != before {
		t.Fatalf("dry run changed the temp tree:\n%s", after)
	}
	if receipts := readCleanupReceipts(t); len(receipts) != 0 {
		t.Fatalf("dry run wrote receipts %+v", receipts)
	}

	in.Apply = true
	got := r.artifactSweep(t, in)
	d = requireDecision(t, got, closedTemp, "removed", "")
	if d.Kind != kindSessionTemp || d.Bytes < 4096 {
		t.Fatalf("removed decision %+v", d)
	}
	requireExists(t, closedTemp, false)
	requireDecision(t, got, clean, "removed", "")
	requireDecision(t, got, clearedTemp, "removed", "")
	requireExists(t, clearedTemp, false)
	requireDecision(t, got, liveTemp, "kept", keepInUse)
	d = requireDecision(t, got, sameKeyTemp, "kept", keepInUse)
	if !strings.Contains(d.Detail, sameKeyInUse) {
		t.Fatalf("same-key detail %q", d.Detail)
	}
	requireDecision(t, got, dirty, "kept", keepDirty)
	d = requireDecision(t, got, nestedTemp, "kept", keepNested)
	if !strings.Contains(d.Detail, dirty) {
		t.Fatalf("nested detail %q", d.Detail)
	}
	d = requireDecision(t, got, citedTemp, "kept", keepEvidence)
	if !strings.Contains(d.Detail, "tqe_temp") {
		t.Fatalf("evidence detail %q", d.Detail)
	}
	d = requireDecision(t, got, receiptCitedTemp, "kept", keepEvidence)
	if !strings.Contains(d.Detail, "receipt.json") {
		t.Fatalf("receipt evidence detail %q", d.Detail)
	}
	for _, p := range []string{mainTemp, folder(linkCwd), sharedTemp, unlisted} {
		if d, ok := got[p]; ok {
			t.Fatalf("%s should not be listed: %+v", p, d)
		}
	}
	for _, p := range []string{liveTemp, sameKeyTemp, mainTemp, nestedTemp, citedTemp, receiptCitedTemp, sharedTemp, unlisted, filepath.Join(dirty, "notes.txt"), filepath.Join(outside, "keep.txt"), root} {
		requireExists(t, p, true)
	}
	if info, err := os.Lstat(folder(linkCwd)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink in the temp root changed: %v %v", info, err)
	}
	receipts := readCleanupReceipts(t)
	rec, ok := receiptFor(receipts, closedTemp)
	if !ok || rec.Kind != kindSessionTemp || rec.Action != "removed" || rec.Source != "sweep" || rec.Bytes < 4096 || rec.At == "" {
		t.Fatalf("session temp receipt %+v %v", rec, ok)
	}
	if rec, ok := receiptFor(receipts, liveTemp); !ok || rec.Kind != kindSessionTemp || rec.Action != "kept" || rec.Reason != keepInUse {
		t.Fatalf("kept session temp receipt %+v %v", rec, ok)
	}
	// The removal itself refuses anything that is not a real directory
	// directly under the root.
	for _, p := range []string{folder(linkCwd), outside, root, filepath.Join(liveTemp, "0f6c-session")} {
		if err := removeSessionTemp(root, p); err == nil {
			t.Fatalf("removeSessionTemp accepted %s", p)
		}
	}
	requireExists(t, filepath.Join(outside, "keep.txt"), true)
	requireExists(t, filepath.Join(liveTemp, "0f6c-session", "scratchpad", "notes.md"), true)
}

// T11: one runner tick removes a finished, released entry's verifier checkout
// and its closed team's session temp folder, although its queue worktree is
// already gone; an accepted, unreleased item keeps its checkout, a team with
// an open agent keeps both, and the hub is never written.
func TestWorktreeCleanupRunnerPrunesArtifactsAndTemp(t *testing.T) {
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	const host = "fixture"
	const task = "tsk_c1ea0c1ea0c1ea0a"
	head := cleanupGit(t, r.main, "rev-parse", "HEAD")
	session := func(cwd string) string {
		dir := filepath.Join(r.tempRoot(), claudeScratchKey(cwd))
		writeFixtureFile(t, filepath.Join(dir, "5e55-session", "scratchpad", "notes.md"), "scratch\n")
		return dir
	}
	entry := func(id, item, name string, acceptedAt time.Time) (api.TeamQueueEntry, string, string) {
		cwd := r.queuePath(name)
		q := api.TeamQueueEntry{ID: id, TaskID: task, ItemID: item, State: "finished", Host: host, Cwd: cwd, Repository: r.common, Position: 1,
			Acceptance: &api.TeamIntegrationAcceptance{Repository: r.common, Worktree: cwd, Commit: head, AcceptedAt: acceptedAt.UTC().Format(time.RFC3339Nano)}}
		return q, r.verifierCheckout(t, item, "verifier-abc1234"), session(cwd)
	}
	young := time.Now().Add(-time.Hour)
	released, releasedCheckout, releasedTemp := entry("tqe_t11_released", artifactItemA, "queue-ffff0001", young)
	released.Release = &api.ReleaseJob{State: "released"}
	accepted, acceptedCheckout, acceptedTemp := entry("tqe_t11_accepted", artifactItemB, "queue-ffff0002", young)
	open, openCheckout, openTemp := entry("tqe_t11_open", artifactItemC, "queue-ffff0003", young)
	open.Release = &api.ReleaseJob{State: "released"}
	runningCwd := r.queuePath("queue-ffff0004")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", runningCwd, "tasks-hub")
	running := api.TeamQueueEntry{ID: "tqe_t11_running", TaskID: task, ItemID: "wi_d4d4d4d4d4d4d4d4", State: "running", Host: host, Cwd: runningCwd}
	runningTemp := session(runningCwd)
	bound := func(id string, q api.TeamQueueEntry, status string) api.Agent {
		return api.Agent{ID: id, Name: id, Host: host, Status: status, Cwd: q.Cwd, WorkItem: &api.AgentWorkItemBinding{ItemTaskID: task, ItemID: q.ItemID}}
	}
	hub := &cleanupHub{
		byHost: api.TeamQueueList{Entries: []api.TeamQueueEntry{running}},
		queues: map[string]api.TeamQueueList{task: {Entries: []api.TeamQueueEntry{released, accepted, open, running}}},
		details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen, PauseState: api.ProjectPausePaused}, Agents: []api.Agent{
			bound("agt_t11_released", released, api.AgentClosed),
			bound("agt_t11_accepted", accepted, api.AgentClosed),
			bound("agt_t11_open", open, "done"),
			bound("agt_t11_running", running, "running"),
		}}},
	}
	server := httptest.NewServer(hub)
	defer server.Close()
	c, err := api.NewClient(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	restore := captureStderr(t, &stderr)
	runner := teamRunner{worktrees: closeoutWorktrees}
	err = runner.tick(context.Background(), env{hub: server.URL}, c, host)
	restore()
	if err != nil {
		t.Fatal(err)
	}
	requireExists(t, releasedCheckout, false)
	requireExists(t, releasedTemp, false)
	manifest := releasedCheckout + ".removed.json"
	if m := readArtifactManifest(t, manifest); m.Source != "closeout" || m.ItemID != artifactItemA || m.EntryID != released.ID || m.Basis != artifactItemReleased || m.Bytes <= 0 {
		t.Fatalf("closeout manifest %+v", m)
	}
	// Accepted an hour ago: the checkout waits; the closed team's temp goes.
	requireExists(t, acceptedCheckout, true)
	requireExists(t, acceptedTemp, false)
	// A bound agent is not closed: the item is active and its temp in use.
	requireExists(t, openCheckout, true)
	requireExists(t, openTemp, true)
	requireExists(t, runningCwd, true)
	requireExists(t, runningTemp, true)
	receipts := readCleanupReceipts(t)
	want := []struct {
		path, entry, kind, action, reason string
	}{
		{releasedCheckout, released.ID, kindArtifactCheckout, "removed", ""},
		{releasedTemp, released.ID, kindSessionTemp, "removed", ""},
		{acceptedCheckout, accepted.ID, kindArtifactCheckout, "kept", keepRetention},
		{acceptedTemp, accepted.ID, kindSessionTemp, "removed", ""},
		{openCheckout, open.ID, kindArtifactCheckout, "kept", keepItemActive},
		{openTemp, open.ID, kindSessionTemp, "kept", keepInUse},
	}
	for _, w := range want {
		rec, ok := receiptFor(receipts, w.path)
		if !ok || rec.Source != "closeout" || rec.EntryID != w.entry || rec.TaskID != task || rec.Kind != w.kind || rec.Action != w.action || rec.Reason != w.reason {
			t.Fatalf("closeout receipt for %s: %+v %v", w.path, rec, ok)
		}
	}
	if rec, _ := receiptFor(receipts, releasedCheckout); rec.Manifest != manifest || rec.Bytes <= 0 {
		t.Fatalf("checkout receipt %+v", rec)
	}
	if rec, _ := receiptFor(receipts, releasedTemp); rec.Bytes <= 0 {
		t.Fatalf("session temp receipt %+v", rec)
	}
	for _, p := range []string{runningCwd, runningTemp} {
		if _, ok := receiptFor(receipts, p); ok {
			t.Fatalf("closeout examined the running entry's %s", p)
		}
	}
	for _, line := range []string{"removed " + releasedCheckout, "removed " + releasedTemp, "kept retention " + acceptedCheckout, "kept item-active " + openCheckout} {
		if !strings.Contains(stderr.String(), line) {
			t.Fatalf("stderr lacks %q: %q", line, stderr.String())
		}
	}
	// Nothing of the released entry is left, so later looks cost no hub read
	// and no Git call, even past the re-check interval.
	closeoutWorktreeChecks.Delete(released.ID)
	calls := countWorktreeGit(t)
	requests, _ := hub.snapshot()
	if err := closeoutWorktrees(context.Background(), c, host, hub.byHost, hub.queues[task], released); err != nil {
		t.Fatal(err)
	}
	after, writes := hub.snapshot()
	if len(after) != len(requests) || calls.Load() != 0 || len(writes) != 0 {
		t.Fatalf("finished closeout still read: requests %v, git calls %d, writes %v", after[len(requests):], calls.Load(), writes)
	}
}

// T12: the sweep command reports artifact checkouts and session temp with
// sizes and changes nothing in a dry run; --apply removes exactly the dry
// run's would-remove set; --json rows carry kind; a negative
// --accepted-after is a usage error.
func TestWorktreeCleanupSweepCommandArtifactsAndTemp(t *testing.T) {
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	host := spawn.Host()
	const task = "tsk_c1ea0c1ea0c1ea0b"
	head := cleanupGit(t, r.main, "rev-parse", "HEAD")
	session := func(cwd string) string {
		dir := filepath.Join(r.tempRoot(), claudeScratchKey(cwd))
		writeFixtureFile(t, filepath.Join(dir, "5e55-session", "tasks", "b1.output"), strings.Repeat("y", 2048))
		return dir
	}
	entry := func(id, item, name string) api.TeamQueueEntry {
		cwd := r.queuePath(name)
		return api.TeamQueueEntry{ID: id, TaskID: task, ItemID: item, State: "finished", Host: host, Cwd: cwd, Position: 1,
			Acceptance: &api.TeamIntegrationAcceptance{Worktree: cwd, Commit: head, AcceptedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)}}
	}
	released := entry("tqe_t12_released", artifactItemA, "queue-9999aaa1")
	released.Release = &api.ReleaseJob{State: "blocked", Published: true}
	accepted := entry("tqe_t12_accepted", artifactItemB, "queue-9999aaa2")
	running := entry("tqe_t12_running", artifactItemC, "queue-9999aaa3")
	running.State, running.Acceptance = "running", nil
	releasedCheckout := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
	acceptedCheckout := r.verifierCheckout(t, artifactItemB, "verifier-abc1234")
	runningCheckout := r.verifierCheckout(t, artifactItemC, "verifier-abc1234")
	writeFixtureFile(t, filepath.Join(r.artifacts(), artifactItemA, "verifier-abc1234-logs", "receipt.json"), `{"ok":true}`)
	releasedTemp, acceptedTemp, runningTemp := session(released.Cwd), session(accepted.Cwd), session(running.Cwd)
	hub := &cleanupHub{
		tasks:   []api.Task{{ID: task, Status: api.TaskOpen}},
		details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen}}},
		queues:  map[string]api.TeamQueueList{task: {Entries: []api.TeamQueueEntry{released, accepted, running}}},
	}
	server := httptest.NewServer(hub)
	defer server.Close()
	e := env{hub: server.URL}
	state := func() string {
		return fixtureTree(t, r.artifacts(), r.tempRoot(), relayDir()) + "\n" + cleanupGit(t, r.main, "worktree", "list", "--porcelain") + "\n" + cleanupGit(t, r.main, "for-each-ref")
	}
	before := state()

	_, err := captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(e, []string{"--cwd", r.main, "--min-idle", "0s", "--accepted-after", "-1h"})
	})
	if err == nil || !strings.Contains(err.Error(), "usage: tt team queue sweep-worktrees") || !strings.Contains(err.Error(), "--accepted-after") || !strings.Contains(err.Error(), "--artifacts") {
		t.Fatalf("negative --accepted-after error %v", err)
	}

	text, err := captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(e, []string{"--cwd", r.main, "--min-idle", "0s"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"would-remove " + releasedCheckout + " (artifact checkout; item " + artifactItemA + " released; ",
		"would-remove " + releasedTemp + " (session temp; 2.0 KiB)",
		"would-remove " + acceptedTemp + " (session temp; 2.0 KiB)",
		"kept retention " + acceptedCheckout + ": accepted 2h ago; eligible after 24h",
		"kept item-active " + runningCheckout + ": queue entry tqe_t12_running is running",
		"kept in-use " + runningTemp,
		"dry run: nothing changed",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("dry run output lacks %q:\n%s", want, text)
		}
	}
	var dry worktreeSweepReport
	out, err := captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(e, []string{"--cwd", r.main, "--min-idle", "0s", "--json", "--artifacts", r.artifacts()})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &dry); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if after := state(); after != before {
		t.Fatalf("dry runs changed the fixture:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	kinds := map[string]string{}
	var wouldRemove []string
	for _, d := range dry.Worktrees {
		kinds[d.Path] = d.Kind
		if d.Action == "would-remove" {
			wouldRemove = append(wouldRemove, d.Path)
			if d.Bytes <= 0 {
				t.Fatalf("would-remove without size %+v", d)
			}
		}
	}
	for path, kind := range map[string]string{releasedCheckout: kindArtifactCheckout, acceptedCheckout: kindArtifactCheckout, runningCheckout: kindArtifactCheckout,
		releasedTemp: kindSessionTemp, acceptedTemp: kindSessionTemp, runningTemp: kindSessionTemp} {
		if kinds[path] != kind {
			t.Fatalf("%s: kind %q, want %q in %s", path, kinds[path], kind, out)
		}
	}
	sort.Strings(wouldRemove)
	wantRemove := []string{releasedCheckout, releasedTemp, acceptedTemp}
	sort.Strings(wantRemove)
	if strings.Join(wouldRemove, ",") != strings.Join(wantRemove, ",") {
		t.Fatalf("would-remove %v, want %v", wouldRemove, wantRemove)
	}
	if dry.Totals.Actions["would-remove"] != 3 || dry.Totals.Kept[keepRetention] != 1 || dry.Totals.Kept[keepItemActive] != 1 || dry.Totals.Kept[keepInUse] != 1 || dry.Totals.Bytes <= 4096 {
		t.Fatalf("dry totals %+v", dry.Totals)
	}
	// --accepted-after shortens the wait for an accepted item.
	out, err = captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(e, []string{"--cwd", r.main, "--min-idle", "0s", "--accepted-after", "1h"})
	})
	if err != nil || !strings.Contains(out, "would-remove "+acceptedCheckout+" (artifact checkout; item "+artifactItemB+" accepted 2h ago; ") {
		t.Fatalf("--accepted-after 1h output %v:\n%s", err, out)
	}
	// An explicit zero means no wait, not the default.
	out, err = captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(e, []string{"--cwd", r.main, "--min-idle", "0s", "--accepted-after", "0s"})
	})
	if err != nil || !strings.Contains(out, "would-remove "+acceptedCheckout+" (artifact checkout; ") {
		t.Fatalf("--accepted-after 0s output %v:\n%s", err, out)
	}
	// An artifacts root elsewhere frees none of these checkouts: outside the
	// root in use they are kept, the running item's as item-active.
	var elsewhere strings.Builder
	restore := captureStderr(t, &elsewhere)
	out, err = captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(e, []string{"--cwd", r.main, "--min-idle", "0s", "--artifacts", filepath.Join(r.root, "elsewhere")})
	})
	restore()
	if err != nil || strings.Contains(out, "artifact checkout") || strings.Contains(out, "would-remove "+runningCheckout) ||
		!strings.Contains(out, "kept item-active "+runningCheckout+": queue entry tqe_t12_running is running") ||
		!strings.Contains(out, "kept retention "+releasedCheckout+": outside the artifacts root "+filepath.Join(r.root, "elsewhere")) ||
		!strings.Contains(elsewhere.String(), "3 detached checkouts") {
		t.Fatalf("--artifacts elsewhere output %v, stderr %q:\n%s", err, elsewhere.String(), out)
	}
	if after := state(); after != before {
		t.Fatal("dry runs with flags changed the fixture")
	}

	text, err = captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(e, []string{"--cwd", r.main, "--min-idle", "0s", "--apply"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"removed " + releasedCheckout + " (artifact checkout; ", "removed " + releasedTemp + " (session temp; ", "removed " + acceptedTemp,
		// The accepted item is finished, so its kept checkout's node_modules is
		// judged for the cache trim: it has no install marker and is kept.
		"kept unproven " + filepath.Join(acceptedCheckout, "node_modules") + ": no npm install marker", "totals: removed=3 kept=4"} {
		if !strings.Contains(text, want) {
			t.Fatalf("apply output lacks %q:\n%s", want, text)
		}
	}
	for _, p := range wantRemove {
		requireExists(t, p, false)
	}
	for _, p := range []string{acceptedCheckout, runningCheckout, runningTemp, filepath.Join(r.artifacts(), artifactItemA, "verifier-abc1234-logs", "receipt.json")} {
		requireExists(t, p, true)
	}
	requireExists(t, releasedCheckout+".removed.json", true)
	if _, writes := hub.snapshot(); len(writes) != 0 {
		t.Fatalf("sweep wrote to the hub: %v", writes)
	}
}

// The sweep reads every page of a project's queue history, so an old item
// still has a queue record.
func TestWorktreeCleanupSweepCommandReadsQueueHistoryPages(t *testing.T) {
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	const task = "tsk_c1ea0c1ea0c1ea0c"
	checkout := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
	old := releasedArtifactEntry(artifactItemA)
	old.TaskID = task
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body any
		switch req.URL.Path {
		case "/v1/tasks":
			body = api.TaskList{Tasks: []api.Task{{ID: task, Status: api.TaskOpen}}}
		case "/v1/tasks/" + task:
			body = api.TaskDetail{Task: api.Task{ID: task, Status: api.TaskOpen}}
		case "/v1/tasks/" + task + "/team-queue":
			queries = append(queries, req.URL.RawQuery)
			page := api.TeamQueueList{Entries: []api.TeamQueueEntry{{ID: "tqe_newer", TaskID: task, ItemID: artifactItemB, State: "finished", Position: 9}}, History: &api.TeamQueueHistoryPage{Total: 2, Limit: 1, NextAfter: 9}}
			if req.URL.Query().Get("after") == "9" {
				page = api.TeamQueueList{Entries: []api.TeamQueueEntry{old}, History: &api.TeamQueueHistoryPage{Total: 2, Limit: 1}}
			}
			body = page
		default:
			http.NotFound(w, req)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	out, err := captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(env{hub: server.URL}, []string{"--cwd", r.main, "--min-idle", "0s"})
	})
	if err != nil || !strings.Contains(out, "would-remove "+checkout+" (artifact checkout; item "+artifactItemA+" released; ") {
		t.Fatalf("paged sweep %v:\n%s", err, out)
	}
	if len(queries) != 2 || !strings.Contains(queries[1], "after=9") {
		t.Fatalf("queue queries %v", queries)
	}
}

func TestWorktreeCleanupArtifactHelpers(t *testing.T) {
	r := newCleanupRepo(t)
	checkout := filepath.Join(r.artifacts(), artifactItemA, "verifier-abc1234")
	writeFixtureFile(t, filepath.Join(checkout, "test-results", "out.txt"), "x\n")
	for text, want := range map[string]string{
		"at " + checkout + "/test-results/out.txt":                          "test-results/out.txt",
		"(" + checkout + "/test-results/out.txt).":                          "test-results/out.txt",
		`{"p":"` + checkout + `/test-results/out.txt"}`:                     "test-results/out.txt",
		checkout + "/test-results/missing.txt":                              "test-results",
		checkout + "/test-results/":                                         "test-results",
		checkout + "/../verifier-abc1234-logs/x.log":                        "",
		checkout + "-logs/test-results/out.txt":                             "",
		checkout + "/coverage/out.txt":                                      "",
		checkout + " and " + checkout + "/":                                 "",
		"dir " + checkout:                                                   "",
		checkout + "/coverage/a then " + checkout + "/test-results/out.txt": "test-results/out.txt",
	} {
		if got := artifactCitation(checkout, r.main, text); got != want {
			t.Fatalf("citation in %q: got %q, want %q", text, got, want)
		}
	}
	for d, want := range map[time.Duration]string{24 * time.Hour: "24h", 90 * time.Minute: "1h30m", 25 * time.Minute: "25m", 10 * time.Second: "0m", 3*time.Hour + 20*time.Second: "3h"} {
		if got := shortDuration(d); got != want {
			t.Fatalf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
	linked := r.queuePath("queue-aaaa0020")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", linked, "tasks-hub")
	for repo, want := range map[string]string{r.common: r.main, r.main: r.main, linked: r.main, filepath.Join(r.root, "nowhere"): "", "": ""} {
		if got := mainCheckoutGuess(repo); got != want {
			t.Fatalf("mainCheckoutGuess(%q) = %q, want %q", repo, got, want)
		}
	}
	if got := artifactsRootFor(r.main); got != r.artifacts() {
		t.Fatalf("default artifacts root %q", got)
	}
	t.Setenv("TAILTERM_ARTIFACTS", filepath.Join(r.root, "elsewhere"))
	if got := artifactsRootFor(r.main); got != filepath.Join(r.root, "elsewhere") {
		t.Fatalf("overridden artifacts root %q", got)
	}
	t.Setenv("TAILTERM_ARTIFACT_ACCEPTED_AFTER", "90m")
	if got := artifactAcceptedAfter(); got != 90*time.Minute {
		t.Fatalf("accepted-after override %v", got)
	}
	t.Setenv("TAILTERM_ARTIFACT_ACCEPTED_AFTER", "0")
	if got := artifactAcceptedAfter(); got != time.Nanosecond {
		t.Fatalf("accepted-after zero %v", got)
	}
	var stderr strings.Builder
	restore := captureStderr(t, &stderr)
	t.Setenv("TAILTERM_ARTIFACT_ACCEPTED_AFTER", "-1h")
	negative := artifactAcceptedAfter()
	t.Setenv("TAILTERM_ARTIFACT_ACCEPTED_AFTER", "soon")
	unparseable := artifactAcceptedAfter()
	restore()
	if negative != defaultArtifactAcceptedAfter || unparseable != defaultArtifactAcceptedAfter || strings.Count(stderr.String(), "TAILTERM_ARTIFACT_ACCEPTED_AFTER") != 1 {
		t.Fatalf("bad accepted-after values gave %v %v, stderr %q", negative, unparseable, stderr.String())
	}
	if !holdsDetachedCheckout(filepath.Dir(linked)) || holdsDetachedCheckout(filepath.Join(r.artifacts(), artifactItemA)) {
		t.Fatal("holdsDetachedCheckout")
	}
}

// T13: sessions key their temp folder by their start directory, so a session
// started at a worktree's root keeps its folder while any directory below
// that root is in use, in the cleanup pass and at closeout.
func TestWorktreeCleanupSessionTempInUseBelowWorktreeRoot(t *testing.T) {
	fixture := func(t *testing.T, r cleanupRepo) (string, string, string) {
		t.Helper()
		w := r.queuePath("queue-eeee0010")
		cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", w, "tasks-hub")
		below := filepath.Join(w, "hub")
		if err := os.MkdirAll(below, 0755); err != nil {
			t.Fatal(err)
		}
		temp := filepath.Join(r.tempRoot(), claudeScratchKey(w))
		writeFixtureFile(t, filepath.Join(temp, "0f6c-session", "scratchpad", "notes.md"), "scratch\n")
		return w, below, temp
	}
	t.Run("pass", func(t *testing.T) {
		r := newCleanupRepo(t)
		w, below, temp := fixture(t, r)
		in := worktreeCleanupInputs{TempRoot: r.tempRoot(), TempCwds: []sessionTempCwd{{Path: w, ItemID: artifactItemA}}, InUse: []string{below}}
		for _, apply := range []bool{false, true} {
			in.Apply = apply
			got := r.artifactSweep(t, in)
			d := requireDecision(t, got, temp, "kept", keepInUse)
			if d.Kind != kindSessionTemp || !strings.Contains(d.Detail, "a session started in "+w) || !strings.Contains(d.Detail, below) {
				t.Fatalf("apply=%v: decision %+v", apply, d)
			}
			requireDecision(t, got, w, "kept", keepInUse)
			requireExists(t, filepath.Join(temp, "0f6c-session", "scratchpad", "notes.md"), true)
			requireExists(t, w, true)
		}
	})
	t.Run("closeout", func(t *testing.T) {
		r := newCleanupRepo(t)
		const host = "fixture"
		const task = "tsk_c1ea0c1ea0c1ea0d"
		w, below, temp := fixture(t, r)
		head := cleanupGit(t, r.main, "rev-parse", "HEAD")
		finished := api.TeamQueueEntry{ID: "tqe_t13_finished", TaskID: task, ItemID: artifactItemA, State: "finished", Host: host, Cwd: w, Repository: r.common, Position: 1,
			Acceptance: &api.TeamIntegrationAcceptance{Repository: r.common, Worktree: w, Commit: head, AcceptedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)},
			Release:    &api.ReleaseJob{State: "released"}}
		runningCwd := r.queuePath("queue-eeee0011")
		cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", runningCwd, "tasks-hub")
		running := api.TeamQueueEntry{ID: "tqe_t13_running", TaskID: task, ItemID: artifactItemB, State: "running", Host: host, Cwd: runningCwd}
		hub := &cleanupHub{
			byHost: api.TeamQueueList{Entries: []api.TeamQueueEntry{running}},
			queues: map[string]api.TeamQueueList{task: {Entries: []api.TeamQueueEntry{finished, running}}},
			details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen, PauseState: api.ProjectPausePaused}, Agents: []api.Agent{
				{ID: "agt_t13_closed", Name: "agt_t13_closed", Host: host, Status: api.AgentClosed, Cwd: w, WorkItem: &api.AgentWorkItemBinding{ItemTaskID: task, ItemID: finished.ItemID}},
				// Another item's agent works below the finished entry's worktree root.
				{ID: "agt_t13_below", Name: "agt_t13_below", Host: host, Status: "running", Cwd: below, WorkItem: &api.AgentWorkItemBinding{ItemTaskID: task, ItemID: running.ItemID}},
			}}},
		}
		server := httptest.NewServer(hub)
		defer server.Close()
		c, err := api.NewClient(server.URL, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		restore := captureStderr(t, &stderr)
		runner := teamRunner{worktrees: closeoutWorktrees}
		err = runner.tick(context.Background(), env{hub: server.URL}, c, host)
		restore()
		if err != nil {
			t.Fatal(err)
		}
		requireExists(t, w, true)
		requireExists(t, filepath.Join(temp, "0f6c-session", "scratchpad", "notes.md"), true)
		receipts := readCleanupReceipts(t)
		rec, ok := receiptFor(receipts, temp)
		if !ok || rec.Source != "closeout" || rec.EntryID != finished.ID || rec.Kind != kindSessionTemp || rec.Action != "kept" || rec.Reason != keepInUse || !strings.Contains(rec.Detail, below) {
			t.Fatalf("closeout receipt for %s: %+v %v", temp, rec, ok)
		}
		if rec, ok := receiptFor(receipts, w); !ok || rec.Action != "kept" || rec.Reason != keepInUse {
			t.Fatalf("closeout receipt for %s: %+v %v", w, rec, ok)
		}
		if _, writes := hub.snapshot(); len(writes) != 0 {
			t.Fatalf("closeout wrote to the hub: %v", writes)
		}
	})
}

// T14: the sweep's recent rule reads every level of a session temp folder. An
// old folder whose only recent change is a file below its top level is kept,
// as is one the sweep cannot read through; closeout has no idle rule.
func TestWorktreeCleanupSessionTempRecentBelowTopLevel(t *testing.T) {
	r := newCleanupRepo(t)
	root := r.tempRoot()
	old := time.Now().Add(-48 * time.Hour)
	age := func(paths ...string) {
		t.Helper()
		for _, p := range paths {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	// session builds an old folder for a gone cwd and returns it with its
	// notes file, two levels below the folder's direct child.
	session := func(name string) (string, string, sessionTempCwd) {
		cwd := r.queuePath(name)
		folder := filepath.Join(root, claudeScratchKey(cwd))
		notes := filepath.Join(folder, "0f6c-session", "scratchpad", "notes.md")
		writeFixtureFile(t, notes, "scratch\n")
		age(notes, filepath.Dir(notes), filepath.Dir(filepath.Dir(notes)), folder)
		return folder, notes, sessionTempCwd{Path: cwd, ItemID: artifactItemA}
	}
	active, activeNotes, activeCwd := session("queue-eeee0012")
	idle, _, idleCwd := session("queue-eeee0013")
	blocked, blockedNotes, blockedCwd := session("queue-eeee0014")
	// Only the file changes; every directory above it stays 48 hours old.
	writeFixtureFile(t, activeNotes, "written now\n")
	age(filepath.Dir(activeNotes), filepath.Dir(filepath.Dir(activeNotes)), active)
	unreadable := filepath.Dir(blockedNotes)
	if err := os.Chmod(unreadable, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0755) })

	in := worktreeCleanupInputs{TempRoot: root, TempCwds: []sessionTempCwd{activeCwd, idleCwd, blockedCwd}, MinIdle: 24 * time.Hour}
	got := r.artifactSweep(t, in)
	if d := requireDecision(t, got, active, "kept", keepRecent); d.Kind != kindSessionTemp {
		t.Fatalf("recent decision %+v", d)
	}
	requireDecision(t, got, idle, "would-remove", "")
	if os.Geteuid() != 0 {
		// Root reads through any mode, so the folder is plainly idle to it.
		requireDecision(t, got, blocked, "kept", keepRecent)
	}
	// Without --min-idle, as at closeout, the rule does not apply.
	in.MinIdle = 0
	got = r.artifactSweep(t, in)
	requireDecision(t, got, active, "would-remove", "")
	requireDecision(t, got, idle, "would-remove", "")
	for _, p := range []string{activeNotes, idle, blocked} {
		requireExists(t, p, true)
	}
}

// closeoutTick runs one runner tick over a finished, released entry of item
// with the given cwd, beside a running entry of runningItem in a paused
// project, and returns the entry and the tick's stderr.
func closeoutTick(t *testing.T, r cleanupRepo, item, cwd, runningItem string) (api.TeamQueueEntry, string) {
	t.Helper()
	const host = "fixture"
	const task = "tsk_c1ea0c1ea0c1ea0f"
	head := cleanupGit(t, r.main, "rev-parse", "HEAD")
	finished := api.TeamQueueEntry{ID: "tqe_t15_finished", TaskID: task, ItemID: item, State: "finished", Host: host, Cwd: cwd, Repository: r.common, Position: 1,
		Acceptance: &api.TeamIntegrationAcceptance{Repository: r.common, Worktree: cwd, Commit: head, AcceptedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)},
		Release:    &api.ReleaseJob{State: "released"}}
	runningCwd := r.queuePath("queue-9999bbb4")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", runningCwd, "tasks-hub")
	running := api.TeamQueueEntry{ID: "tqe_t15_other", TaskID: task, ItemID: runningItem, State: "running", Host: host, Cwd: runningCwd, Position: 2}
	hub := &cleanupHub{
		byHost:  api.TeamQueueList{Entries: []api.TeamQueueEntry{running}},
		queues:  map[string]api.TeamQueueList{task: {Entries: []api.TeamQueueEntry{finished, running}}},
		details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen, PauseState: api.ProjectPausePaused}}},
	}
	server := httptest.NewServer(hub)
	defer server.Close()
	c, err := api.NewClient(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Every caller uses this entry; each gets its own first look.
	closeoutWorktreeChecks.Delete(finished.ID)
	var stderr strings.Builder
	restore := captureStderr(t, &stderr)
	runner := teamRunner{worktrees: closeoutWorktrees}
	err = runner.tick(context.Background(), env{hub: server.URL}, c, host)
	restore()
	if err != nil {
		t.Fatal(err)
	}
	if _, writes := hub.snapshot(); len(writes) != 0 {
		t.Fatalf("closeout wrote to the hub: %v", writes)
	}
	return finished, stderr.String()
}

// T15: a wrong artifacts root never frees a verifier checkout. The sweep
// keeps the checkouts outside the root it used and warns, and refuses a
// relative root before it reads the hub; closeout leaves the artifacts tree
// alone when its root is relative, and keeps a running item's checkout
// outside the root.
func TestWorktreeCleanupWrongArtifactsRootKeepsRunningItem(t *testing.T) {
	const strayWarning = "outside the artifacts root"
	t.Run("sweep", func(t *testing.T) {
		r := newCleanupRepo(t)
		r.ignoreBuildOutput(t)
		host := spawn.Host()
		const task = "tsk_c1ea0c1ea0c1ea0e"
		released := releasedArtifactEntry(artifactItemA)
		released.ID, released.TaskID, released.Host, released.Cwd = "tqe_t15_released", task, host, r.queuePath("queue-9999bbb1")
		running := api.TeamQueueEntry{ID: "tqe_t15_running", TaskID: task, ItemID: artifactItemC, State: "running", Host: host, Cwd: r.queuePath("queue-9999bbb3"), Position: 2}
		releasedCheckout := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
		runningCheckout := r.verifierCheckout(t, artifactItemC, "verifier-abc1234")
		hub := &cleanupHub{
			tasks:   []api.Task{{ID: task, Status: api.TaskOpen}},
			details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen}}},
			queues:  map[string]api.TeamQueueList{task: {Entries: []api.TeamQueueEntry{released, running}}},
		}
		server := httptest.NewServer(hub)
		defer server.Close()
		e := env{hub: server.URL}
		state := func() string {
			return fixtureTree(t, r.artifacts(), r.tempRoot(), relayDir()) + "\n" + cleanupGit(t, r.main, "worktree", "list", "--porcelain") + "\n" + cleanupGit(t, r.main, "for-each-ref")
		}
		before := state()
		sweep := func(args ...string) (string, string, error) {
			var stderr strings.Builder
			restore := captureStderr(t, &stderr)
			out, err := captureSweepStdout(t, func() error {
				return cmdTeamQueueSweepWorktrees(e, append([]string{"--cwd", r.main, "--min-idle", "0s"}, args...))
			})
			restore()
			return out, stderr.String(), err
		}
		itemActive := "kept item-active " + runningCheckout + ": queue entry tqe_t15_running is running"

		// a. An absolute root that is not where the checkouts are.
		wrong := filepath.Join(r.root, "elsewhere")
		out, warned, err := sweep("--artifacts", wrong)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "would-remove "+runningCheckout) || strings.Contains(out, "would-remove "+releasedCheckout) {
			t.Fatalf("a wrong root made a verifier checkout removable:\n%s", out)
		}
		for _, want := range []string{itemActive, "kept retention " + releasedCheckout + ": " + strayWarning + " " + wrong + "; check --artifacts or TAILTERM_ARTIFACTS"} {
			if !strings.Contains(out, want) {
				t.Fatalf("wrong root output lacks %q:\n%s", want, out)
			}
		}
		if strings.Count(warned, strayWarning) != 1 || !strings.Contains(warned, "2 detached checkouts") || !strings.Contains(warned, wrong) || !strings.Contains(warned, "--artifacts") || !strings.Contains(warned, "TAILTERM_ARTIFACTS") {
			t.Fatalf("wrong root warning %q", warned)
		}

		// b. A relative --artifacts is refused before the hub is read.
		requests, _ := hub.snapshot()
		out, _, err = sweep("--artifacts", "relative/dir")
		if err == nil || !strings.Contains(err.Error(), "--artifacts") || !strings.Contains(err.Error(), "absolute") || out != "" {
			t.Fatalf("relative --artifacts: err %v, output %q", err, out)
		}
		// c. So is a relative TAILTERM_ARTIFACTS with no flag.
		t.Setenv("TAILTERM_ARTIFACTS", "relative/dir")
		out, _, err = sweep()
		if err == nil || !strings.Contains(err.Error(), "TAILTERM_ARTIFACTS") || !strings.Contains(err.Error(), "absolute") || out != "" {
			t.Fatalf("relative TAILTERM_ARTIFACTS: err %v, output %q", err, out)
		}
		if after, _ := hub.snapshot(); len(after) != len(requests) {
			t.Fatalf("a refused sweep read the hub: %v", after[len(requests):])
		}
		// d. An absolute --artifacts overrides the variable.
		out, warned, err = sweep("--artifacts", r.artifacts())
		if err != nil || !strings.Contains(out, itemActive) || !strings.Contains(out, "would-remove "+releasedCheckout+" (artifact checkout; item "+artifactItemA+" released; ") || strings.Contains(warned, "TAILTERM_ARTIFACTS") {
			t.Fatalf("flag over a relative variable: err %v, stderr %q, output:\n%s", err, warned, out)
		}
		if after := state(); after != before {
			t.Fatalf("dry runs changed the fixture:\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if _, writes := hub.snapshot(); len(writes) != 0 {
			t.Fatalf("sweep wrote to the hub: %v", writes)
		}
	})

	// e. A relative TAILTERM_ARTIFACTS: closeout says so once and leaves the
	// artifacts tree alone, and the closed team's session temp with it.
	t.Run("closeout relative root", func(t *testing.T) {
		r := newCleanupRepo(t)
		r.ignoreBuildOutput(t)
		checkout := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
		cwd := r.queuePath("queue-9999bbb5")
		temp := filepath.Join(r.tempRoot(), claudeScratchKey(cwd))
		writeFixtureFile(t, filepath.Join(temp, "5e55-session", "scratchpad", "notes.md"), "scratch\n")
		t.Setenv("TAILTERM_ARTIFACTS", "relative/dir")
		artifactsRootWarning = sync.Once{}
		_, stderr := closeoutTick(t, r, artifactItemA, cwd, artifactItemB)
		requireExists(t, filepath.Join(checkout, "dist", "app.js"), true)
		requireExists(t, checkout+".removed.json", false)
		requireExists(t, filepath.Join(temp, "5e55-session", "scratchpad", "notes.md"), true)
		if rec, ok := receiptFor(readCleanupReceipts(t), temp); !ok || rec.Action != "kept" || rec.Reason != keepRetention || !strings.Contains(rec.Detail, "no artifacts root") {
			t.Fatalf("closeout receipt for %s: %+v %v", temp, rec, ok)
		}
		if _, ok := receiptFor(readCleanupReceipts(t), checkout); ok {
			t.Fatal("closeout examined a verifier checkout with no artifacts root")
		}
		if strings.Count(stderr, "TAILTERM_ARTIFACTS=") != 1 || !strings.Contains(stderr, `TAILTERM_ARTIFACTS="relative/dir" is not an absolute path`) {
			t.Fatalf("relative root warning %q", stderr)
		}
	})

	// f. An absolute root elsewhere: the entry's recorded worktree is a
	// detached checkout under its item's folder and the item still runs.
	t.Run("closeout wrong root", func(t *testing.T) {
		r := newCleanupRepo(t)
		r.ignoreBuildOutput(t)
		checkout := r.verifierCheckout(t, artifactItemC, "verifier-abc1234")
		t.Setenv("TAILTERM_ARTIFACTS", filepath.Join(r.root, "elsewhere"))
		finished, _ := closeoutTick(t, r, artifactItemC, checkout, artifactItemC)
		requireExists(t, filepath.Join(checkout, "dist", "app.js"), true)
		requireExists(t, checkout+".removed.json", false)
		rec, ok := receiptFor(readCleanupReceipts(t), checkout)
		if !ok || rec.Source != "closeout" || rec.EntryID != finished.ID || rec.Action != "kept" || rec.Reason != keepItemActive || !strings.Contains(rec.Detail, "tqe_t15_other is running") {
			t.Fatalf("closeout receipt for %s: %+v %v", checkout, rec, ok)
		}
	})
}

// T16: session temp is removed only when its items' receipt and plan files
// can be read for citations. With no artifacts root, or one that is not the
// default and is not a directory or has no folder for the item, every folder
// is kept, in the pass, the sweep command and at closeout. The default root
// is trusted: an item with no folder there has no receipt to cite.
func TestWorktreeCleanupSessionTempKeptWithUnusableArtifactsRoot(t *testing.T) {
	// fixture has a gone cwd of item A whose temp folder only A's receipt
	// cites, and one of item B, which has no artifact folder and no citation.
	fixture := func(t *testing.T, r cleanupRepo) (cited, plain string, cwds []sessionTempCwd) {
		t.Helper()
		session := func(name, item string) string {
			cwd := r.queuePath(name)
			folder := filepath.Join(r.tempRoot(), claudeScratchKey(cwd))
			writeFixtureFile(t, filepath.Join(folder, "5e55-session", "scratchpad", "notes.md"), "scratch\n")
			cwds = append(cwds, sessionTempCwd{Path: cwd, ItemID: item})
			return folder
		}
		cited, plain = session("queue-7777aaa1", artifactItemA), session("queue-7777aaa2", artifactItemB)
		writeFixtureFile(t, filepath.Join(r.artifacts(), artifactItemA, "receipt.json"), `{"report":"`+cited+`/5e55-session/scratchpad/notes.md"}`)
		return cited, plain, cwds
	}
	const check = "check --artifacts or TAILTERM_ARTIFACTS"
	t.Run("pass", func(t *testing.T) {
		r := newCleanupRepo(t)
		cited, plain, cwds := fixture(t, r)
		in := worktreeCleanupInputs{TempRoot: r.tempRoot(), TempCwds: cwds}
		// The default root: the receipt is read, and item B's missing folder
		// means there is nothing to cite its temp folder.
		got := r.artifactSweep(t, in)
		if d := requireDecision(t, got, cited, "kept", keepEvidence); !strings.Contains(d.Detail, "receipt.json") {
			t.Fatalf("default root decision %+v", d)
		}
		requireDecision(t, got, plain, "would-remove", "")
		// A wrong root that does not exist, and one that exists.
		missing, other := filepath.Join(r.root, "elsewhere"), filepath.Join(r.root, "other-artifacts")
		if err := os.MkdirAll(filepath.Join(other, artifactItemC), 0755); err != nil {
			t.Fatal(err)
		}
		for root, why := range map[string]string{missing: "is not a directory", other: "has no folder for " + artifactItemA} {
			for _, apply := range []bool{false, true} {
				in.Artifacts, in.Apply = root, apply
				got := r.artifactSweep(t, in)
				d := requireDecision(t, got, cited, "kept", keepRetention)
				if d.Kind != kindSessionTemp || !strings.Contains(d.Detail, root) || !strings.Contains(d.Detail, "not the default") || !strings.Contains(d.Detail, why) || !strings.Contains(d.Detail, check) {
					t.Fatalf("root %s apply=%v: decision %+v", root, apply, d)
				}
				requireDecision(t, got, plain, "kept", keepRetention)
				for _, p := range []string{cited, plain} {
					requireExists(t, filepath.Join(p, "5e55-session", "scratchpad", "notes.md"), true)
				}
			}
		}
		// Back on the default root, applying removes only the uncited folder.
		in.Artifacts, in.Apply = "", true
		got = r.artifactSweep(t, in)
		requireDecision(t, got, cited, "kept", keepEvidence)
		requireDecision(t, got, plain, "removed", "")
		requireExists(t, plain, false)
		requireExists(t, cited, true)
	})
	t.Run("sweep command", func(t *testing.T) {
		r := newCleanupRepo(t)
		cited, plain, cwds := fixture(t, r)
		host := spawn.Host()
		const task = "tsk_c1ea0c1ea0c1ea1a"
		var entries []api.TeamQueueEntry
		for i, c := range cwds {
			q := releasedArtifactEntry(c.ItemID)
			q.ID, q.TaskID, q.Host, q.Cwd, q.Position = "tqe_t16_"+strconv.Itoa(i), task, host, c.Path, int64(i+1)
			entries = append(entries, q)
		}
		hub := &cleanupHub{
			tasks:   []api.Task{{ID: task, Status: api.TaskOpen}},
			details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen}}},
			queues:  map[string]api.TeamQueueList{task: {Entries: entries}},
		}
		server := httptest.NewServer(hub)
		defer server.Close()
		wrong := filepath.Join(r.root, "elsewhere")
		out, err := captureSweepStdout(t, func() error {
			return cmdTeamQueueSweepWorktrees(env{hub: server.URL}, []string{"--cwd", r.main, "--min-idle", "0s", "--artifacts", wrong})
		})
		if err != nil || strings.Contains(out, "would-remove /") {
			t.Fatalf("sweep with a wrong root: %v\n%s", err, out)
		}
		for _, p := range []string{cited, plain} {
			if !strings.Contains(out, "kept retention "+p+": artifacts root "+wrong+" is not the default and is not a directory") {
				t.Fatalf("sweep output lacks the kept row for %s:\n%s", p, out)
			}
		}
		out, err = captureSweepStdout(t, func() error {
			return cmdTeamQueueSweepWorktrees(env{hub: server.URL}, []string{"--cwd", r.main, "--min-idle", "0s"})
		})
		if err != nil || !strings.Contains(out, "kept evidence "+cited) || !strings.Contains(out, "would-remove "+plain+" (session temp; ") {
			t.Fatalf("sweep with the default root: %v\n%s", err, out)
		}
	})
	for name, root := range map[string]string{"closeout wrong root": "elsewhere", "closeout relative root": ""} {
		t.Run(name, func(t *testing.T) {
			r := newCleanupRepo(t)
			cited, _, cwds := fixture(t, r)
			want := "no artifacts root"
			if root == "" {
				t.Setenv("TAILTERM_ARTIFACTS", "relative/dir")
				artifactsRootWarning = sync.Once{}
			} else {
				t.Setenv("TAILTERM_ARTIFACTS", filepath.Join(r.root, root))
				want = "artifacts root " + filepath.Join(r.root, root) + " is not the default and is not a directory"
			}
			finished, stderr := closeoutTick(t, r, artifactItemA, cwds[0].Path, artifactItemC)
			requireExists(t, filepath.Join(cited, "5e55-session", "scratchpad", "notes.md"), true)
			rec, ok := receiptFor(readCleanupReceipts(t), cited)
			if !ok || rec.Source != "closeout" || rec.EntryID != finished.ID || rec.Kind != kindSessionTemp || rec.Action != "kept" || rec.Reason != keepRetention || !strings.Contains(rec.Detail, want) || !strings.Contains(rec.Detail, check) {
				t.Fatalf("closeout receipt for %s: %+v %v", cited, rec, ok)
			}
			if !strings.Contains(stderr, "kept retention "+cited) {
				t.Fatalf("stderr lacks the kept line: %q", stderr)
			}
		})
	}
	// The default root with no folder for the item: closeout still removes.
	t.Run("closeout default root", func(t *testing.T) {
		r := newCleanupRepo(t)
		_, plain, cwds := fixture(t, r)
		closeoutTick(t, r, artifactItemB, cwds[1].Path, artifactItemC)
		requireExists(t, plain, false)
	})
}

// Cache trim, tombstones and the scheduled pass.

const (
	trimHost     = "fixture-host"
	trimItemAcc  = "wi_00000000000000a1"
	trimItemRel  = "wi_00000000000000a2"
	trimItemCwd  = "wi_00000000000000a3"
	trimItemFar  = "wi_00000000000000a4"
	trimItemOut  = "wi_00000000000000a5"
	trimItemFail = "wi_00000000000000b1"
	trimItemLive = "wi_00000000000000b2"
	trimItemGone = "wi_00000000000000b3"
	trimItemRedo = "wi_00000000000000b4"
	trimItemBusy = "wi_00000000000000b5"
)

// installModules makes checkout's node_modules look like an npm install: the
// marker npm writes, and a package.
func installModules(t *testing.T, checkout string) string {
	t.Helper()
	cache := filepath.Join(checkout, "node_modules")
	writeFixtureFile(t, filepath.Join(cache, ".package-lock.json"), `{"name":"fixture","lockfileVersion":3}`+"\n")
	writeFixtureFile(t, filepath.Join(cache, "left-pad", "index.js"), "module.exports = 1\n")
	return cache
}

// treeContents is every entry below root with its mode and bytes (a link's
// target for a symlink), so a tree can be shown byte-identical.
func treeContents(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		line := p + " " + info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			line += " -> " + target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			line += " " + strconv.Quote(string(data))
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// trimFixture is a repository with ignored build output, a fake home holding
// sentinel shared caches, and a clock two days ahead so every tree is idle.
type trimFixture struct {
	cleanupRepo
	home string
	now  time.Time
}

func newTrimFixture(t *testing.T) trimFixture {
	t.Helper()
	r := newCleanupRepo(t)
	r.ignoreBuildOutput(t)
	return trimFixture{cleanupRepo: r, home: isolatedHome(t, r.root), now: time.Now().Add(48 * time.Hour)}
}

// accepted is a finished entry of item on this host, accepted two hours
// before the fixture's clock: its verifier checkouts are kept for retention.
func (f trimFixture) accepted(item string) api.TeamQueueEntry {
	q := acceptedArtifactEntry(item, f.now.Add(-2*time.Hour))
	q.Host, q.TaskID = trimHost, "tsk_c1ea0c1ea0c1ea10"
	return q
}

// pass runs the sweep's own pass over the fixture.
func (f trimFixture) pass(t *testing.T, state sweepHubState, apply bool) map[string]worktreeDecision {
	t.Helper()
	decisions, err := runWorktreeSweep(context.Background(), state, trimHost, sweepOptions{Repo: f.main, Apply: apply, MinIdle: 6 * time.Hour, AcceptedAfter: defaultArtifactAcceptedAfter, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]worktreeDecision{}
	for _, d := range decisions {
		if _, twice := out[d.Path]; twice {
			t.Fatalf("two decisions for %s: %+v", d.Path, decisions)
		}
		out[d.Path] = d
	}
	return out
}

func requireNoTombstones(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		left, _ := filepath.Glob(filepath.Join(dir, tombstonePrefix+"*"))
		if len(left) != 0 {
			t.Fatalf("tombstones left in %s: %v", dir, left)
		}
	}
	if intents, err := loadDetachIntents(); err != nil || len(intents) != 0 {
		t.Fatalf("intents left: %+v %v", intents, err)
	}
}

// t12 (a2): one fixture swept by the manual command and by the schedule
// gives the same decisions and the same cleanup receipts apart from the time.
func TestWorktreeCleanupManualAndScheduledSweepsAgree(t *testing.T) {
	f := newSweepFixture(t)
	r := f.r
	f.host = spawn.Host()
	at := f.now
	oldNow := sweepNow
	sweepNow = func() time.Time { return at }
	t.Cleanup(func() { sweepNow = oldNow })

	stale := f.stale(t, "queue-a2a20001")
	dirty := f.stale(t, "queue-a2a20002")
	writeFixtureFile(t, filepath.Join(dirty, "draft.txt"), "draft\n")
	unpushed := r.branchWorktree(t, r.queuePath("queue-a2a20003"), "feat/parity-unpushed")
	missing := f.stale(t, "queue-a2a20004")
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}
	releasedCheckout := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
	acceptedCheckout := r.verifierCheckout(t, artifactItemB, "verifier-abc1234")
	cache := installModules(t, acceptedCheckout)
	runningCheckout := r.verifierCheckout(t, artifactItemC, "verifier-abc1234")
	tempCwd := r.queuePath("queue-a2a20005")
	temp := filepath.Join(r.tempRoot(), claudeScratchKey(tempCwd))
	writeFixtureFile(t, filepath.Join(temp, "session", "scratchpad", "note.txt"), "scratch\n")
	released := releasedArtifactEntry(artifactItemA)
	released.TaskID, released.Host, released.Cwd, released.Repository = sweepTask, f.host, tempCwd, r.main
	accepted := acceptedArtifactEntry(artifactItemB, at.Add(-2*time.Hour))
	accepted.TaskID, accepted.Host = sweepTask, f.host
	running := api.TeamQueueEntry{ID: "tqe_parity_running", TaskID: sweepTask, ItemID: artifactItemC, State: "running", Host: f.host, Cwd: r.queuePath("queue-a2a20006"), Position: 2}
	f.setEntries(released, accepted, running)

	snapshot := t.TempDir()
	copyTree := func(from, to string) {
		t.Helper()
		if out, err := exec.Command("cp", "-a", from+"/.", to).CombinedOutput(); err != nil {
			t.Fatalf("copy %s: %v: %s", from, err, out)
		}
	}
	copyTree(r.root, snapshot)
	type result struct {
		decisions string
		receipts  []worktreeCleanupReceipt
	}
	collect := func(decisions []worktreeDecision) result {
		t.Helper()
		data, err := json.MarshalIndent(decisions, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		receipts := readCleanupReceipts(t)
		for i := range receipts {
			if receipts[i].At != at.UTC().Format(time.RFC3339Nano) {
				t.Fatalf("receipt time %s, want the pass's %s", receipts[i].At, at.UTC().Format(time.RFC3339Nano))
			}
			receipts[i].At = ""
		}
		return result{string(data), receipts}
	}
	check := func(who string) {
		t.Helper()
		for _, p := range []string{stale, releasedCheckout, temp, cache} {
			requireExists(t, p, false)
		}
		for _, p := range []string{dirty, unpushed, acceptedCheckout, runningCheckout, releasedCheckout + ".removed.json", filepath.Join(acceptedCheckout, "dist", "app.js")} {
			requireExists(t, p, true)
		}
		if list := cleanupGit(t, r.main, "worktree", "list", "--porcelain"); strings.Contains(list, missing) {
			t.Fatalf("%s did not prune %s", who, missing)
		}
	}

	out, err := captureSweepStdout(t, func() error {
		return cmdTeamQueueSweepWorktrees(env{hub: f.hubURL}, []string{"--cwd", r.main, "--min-idle", "6h", "--apply", "--json"})
	})
	if err != nil {
		t.Fatalf("manual sweep: %v\n%s", err, out)
	}
	var report worktreeSweepReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	check("the manual sweep")
	manual := collect(report.Worktrees)
	if report.Totals.Actions["removed"] != 3 || report.Totals.Actions["trimmed"] != 1 || report.Totals.Actions["pruned"] != 1 || len(manual.receipts) != len(report.Worktrees) {
		t.Fatalf("manual totals %+v with %d receipts", report.Totals, len(manual.receipts))
	}

	// Back to the same fixture, with nothing remembered from the first run.
	entries, err := os.ReadDir(r.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(r.root, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	copyTree(snapshot, r.root)
	for _, name := range []string{"worktree-cleanup.jsonl", "worktree-sweep-state.json"} {
		if err := os.Remove(filepath.Join(relayDir(), name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	keptWorktreeReceipts.Clear()
	for _, p := range []string{stale, releasedCheckout, temp, cache} {
		requireExists(t, p, true)
	}

	if !f.attempt() || f.sweeps != 1 {
		t.Fatalf("scheduled sweep: %d sweeps, %+v", f.sweeps, lastSweepLine(t))
	}
	check("the scheduled sweep")
	scheduled := collect(f.last)
	if manual.decisions != scheduled.decisions {
		t.Fatalf("decisions differ.\nmanual:\n%s\nscheduled:\n%s", manual.decisions, scheduled.decisions)
	}
	if !reflect.DeepEqual(manual.receipts, scheduled.receipts) {
		t.Fatalf("receipts differ.\nmanual:    %+v\nscheduled: %+v", manual.receipts, scheduled.receipts)
	}
	line := lastSweepLine(t)
	if line.Outcome != "swept" || line.TreesRemoved != 2 || line.FoldersRemoved != 1 || line.CachesTrimmed != 1 || line.Pruned != 1 || line.Kept[keepDirty] != 1 || line.Kept[keepUnpushed] != 1 || line.Kept[keepRetention] != 1 || line.Kept[keepItemActive] != 1 {
		t.Fatalf("scheduled line %+v", line)
	}
	var freed int64
	for _, d := range report.Worktrees {
		freed += d.Bytes
	}
	if line.BytesFreed != freed || freed != report.Totals.Bytes {
		t.Fatalf("bytes freed %d, manual %d, totals %d", line.BytesFreed, freed, report.Totals.Bytes)
	}
}

// t13 (a5): finished is positive evidence with an item-wide veto.
func TestWorktreeCleanupCacheTrimFinishedItems(t *testing.T) {
	f := newTrimFixture(t)
	checkout := func(item string) string {
		path := f.verifierCheckout(t, item, "verifier-abc1234")
		installModules(t, path)
		return path
	}
	acc := checkout(trimItemAcc)
	rel := checkout(trimItemRel)
	writeFixtureFile(t, filepath.Join(rel, "notes.txt"), "kept dirty\n")
	failed, live, gone, redo, busy := checkout(trimItemFail), checkout(trimItemLive), checkout(trimItemGone), checkout(trimItemRedo), checkout(trimItemBusy)
	branch := func(name, branch string) string {
		path := f.branchWorktree(t, f.queuePath(name), branch)
		installModules(t, path)
		return path
	}
	cwd := branch("queue-a5a50001", "feat/trim-cwd")
	far := branch("queue-a5a50002", "feat/trim-other-host")
	nobody := branch("queue-a5a50003", "feat/trim-unattributed")
	outside := f.branchWorktree(t, filepath.Join(f.root, "elsewhere", "queue-a5a50004"), "feat/trim-outside")
	installModules(t, outside)

	after := func(q api.TeamQueueEntry, state string) api.TeamQueueEntry {
		next := api.TeamQueueEntry{ID: q.ID + "_next", TaskID: q.TaskID, ItemID: q.ItemID, Host: q.Host, State: state, Position: q.Position + 1}
		return next
	}
	released := releasedArtifactEntry(trimItemRel)
	released.Host = trimHost
	withCwd := func(item, path, host string) api.TeamQueueEntry {
		q := f.accepted(item)
		q.Host, q.Cwd = host, path
		q.Acceptance.Worktree = path
		return q
	}
	failedReleased := after(f.accepted(trimItemFail), "failed")
	failedReleased.ReleasedAt = "2026-10-01T10:00:00Z"
	entries := []api.TeamQueueEntry{
		f.accepted(trimItemAcc), released,
		withCwd(trimItemCwd, cwd, trimHost), withCwd(trimItemFar, far, "another-host"), withCwd(trimItemOut, outside, trimHost),
		f.accepted(trimItemFail), failedReleased,
		f.accepted(trimItemLive), after(f.accepted(trimItemLive), "failed"),
		f.accepted(trimItemGone), after(f.accepted(trimItemGone), "abandoned"),
		f.accepted(trimItemRedo), after(f.accepted(trimItemRedo), "queued"),
		f.accepted(trimItemBusy),
	}
	agents := []api.Agent{{ID: "agt_00000000000000b5", Name: "late-reviewer", Host: "another-host", Status: api.AgentRunning, WorkItem: &api.AgentWorkItemBinding{ItemID: trimItemBusy}}}
	set := finishedTrimSet(trimHost, entries, agents)
	wantItems := map[string]bool{trimItemAcc: true, trimItemRel: true, trimItemCwd: true, trimItemFar: true, trimItemOut: true}
	wantPaths := map[string]string{cwd: trimItemCwd, outside: trimItemOut}
	if !reflect.DeepEqual(set.Items, wantItems) || !reflect.DeepEqual(set.Paths, wantPaths) {
		t.Fatalf("finished set %+v\nwant items %v, paths %v", set, wantItems, wantPaths)
	}
	// A closed agent does not veto, and an item whose only entry is active has none finished.
	closed := finishedTrimSet(trimHost, []api.TeamQueueEntry{f.accepted(trimItemBusy)}, []api.Agent{{Status: api.AgentClosed, WorkItem: &api.AgentWorkItemBinding{ItemID: trimItemBusy}}})
	if !closed.Items[trimItemBusy] || len(finishedTrimSet(trimHost, []api.TeamQueueEntry{{ItemID: trimItemRedo, State: "running", Position: 1}}, nil).Items) != 0 {
		t.Fatalf("closed agent or active entry: %+v", closed)
	}

	state := sweepHubState{entries: entries, agents: agents}
	trimmed := []string{acc, rel, cwd}
	untouched := []string{failed, live, gone, redo, busy, far, nobody, outside}
	before := fixtureTree(t, f.root)
	dry := f.pass(t, state, false)
	for _, p := range trimmed {
		if d := requireDecision(t, dry, filepath.Join(p, "node_modules"), "would-trim", ""); d.Kind != kindCache || d.Bytes <= 0 {
			t.Fatalf("dry decision %+v", d)
		}
	}
	if fixtureTree(t, f.root) != before {
		t.Fatal("the dry run changed the fixture")
	}
	got := f.pass(t, state, true)
	for _, p := range trimmed {
		d := requireDecision(t, got, filepath.Join(p, "node_modules"), "trimmed", "")
		if d.Kind != kindCache || d.Bytes <= 0 {
			t.Fatalf("trim decision %+v", d)
		}
		requireExists(t, filepath.Join(p, "node_modules"), false)
		requireExists(t, filepath.Join(p, "app.txt"), true)
		requireNoTombstones(t, p)
	}
	requireExists(t, filepath.Join(rel, "notes.txt"), true)
	requireExists(t, filepath.Join(acc, "dist", "app.js"), true)
	for _, p := range untouched {
		if d, ok := got[filepath.Join(p, "node_modules")]; ok {
			t.Fatalf("a cache decision for an unfinished or unattributed checkout: %+v", d)
		}
		requireExists(t, filepath.Join(p, "node_modules", "left-pad", "index.js"), true)
		requireExists(t, filepath.Join(p, "node_modules", ".package-lock.json"), true)
	}
	// The checkouts' own decisions are the keep rules', unchanged.
	requireDecision(t, got, acc, "kept", keepRetention)
	requireDecision(t, got, rel, "kept", keepDirty)
	requireDecision(t, got, cwd, "kept", keepUnpushed)
	requireDecision(t, got, live, "kept", keepItemActive)
	requireDecision(t, got, busy, "kept", keepItemActive)
	var caches int
	for _, receipt := range readCleanupReceipts(t) {
		if receipt.Kind == kindCache {
			caches++
			if receipt.Action != "trimmed" || receipt.Bytes <= 0 || receipt.Source != "sweep" {
				t.Fatalf("cache receipt %+v", receipt)
			}
		}
	}
	if caches != len(trimmed) {
		t.Fatalf("%d cache receipts for %d trims", caches, len(trimmed))
	}
	// Closeout, which sets no trim, never trims.
	again := installModules(t, acc)
	decisions, err := cleanupWorktrees(context.Background(), worktreeCleanupInputs{Repo: f.main, Now: f.now, Apply: true, Items: artifactItemStates(entries, agents), Receipt: worktreeCleanupReceipt{Source: "closeout"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range decisions {
		if d.Kind == kindCache {
			t.Fatalf("closeout made a cache decision: %+v", d)
		}
	}
	requireExists(t, again, true)
}

// t14 (a5): the trim touches only node_modules, only in an idle, unlocked
// checkout nobody uses, and only when the directory is provably the cache.
func TestWorktreeCleanupCacheTrimScope(t *testing.T) {
	f := newTrimFixture(t)
	checkout := func(name string) (string, string) {
		path := f.verifierCheckout(t, trimItemAcc, name)
		return path, installModules(t, path)
	}
	plain, plainCache := checkout("verifier-plain")
	writeFixtureFile(t, filepath.Join(plain, "home", "go-build", "trim.txt"), "1759000000\n")
	writeFixtureFile(t, filepath.Join(plain, "home", "go-build", "aa", "aa11-a"), "object\n")
	writeFixtureFile(t, filepath.Join(plain, "home", "go", "pkg", "mod", "cache", "download", "sumdb"), "sum\n")
	writeFixtureFile(t, filepath.Join(plain, ".build", "go", "pkg", "mod", "x"), "wasm cache\n")
	recent, recentCache := checkout("verifier-recent")
	stale, staleCache := checkout("verifier-recent-cache")
	used, usedCache := checkout("verifier-used")
	locked, lockedCache := checkout("verifier-locked")
	cleanupGit(t, f.main, "worktree", "lock", locked)
	tracked, trackedCache := checkout("verifier-tracked")
	cleanupGit(t, tracked, "add", "-f", "node_modules/left-pad/index.js")
	cleanupGit(t, tracked, "commit", "-q", "-m", "track a module")
	cited, citedCache := checkout("verifier-cited")
	writeFixtureFile(t, filepath.Join(f.artifacts(), trimItemAcc, "verifier-cited-logs", "receipt.json"), `{"module":"`+filepath.Join(cited, "node_modules", "left-pad", "index.js")+`"}`+"\n")
	unmarked := f.verifierCheckout(t, trimItemAcc, "verifier-unmarked")
	unmarkedCache := filepath.Join(unmarked, "node_modules")
	nested, nestedCache := checkout("verifier-nested")
	cleanupGit(t, nestedCache, "init", "-q", "left-pad")
	// Every checkout is two days idle by the fixture's clock except these.
	soon := f.now.Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(worktreeAdminDir(recent), "HEAD"), soon, soon); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleCache, soon, soon); err != nil {
		t.Fatal(err)
	}
	state := sweepHubState{entries: []api.TeamQueueEntry{f.accepted(trimItemAcc)}, agents: []api.Agent{{ID: "agt_00000000000000c1", Name: "someone", Host: trimHost, Status: api.AgentRunning, Cwd: filepath.Join(used, "hub")}}}
	before := fixtureTree(t, f.root)
	dry := f.pass(t, state, false)
	requireDecision(t, dry, plainCache, "would-trim", "")
	if fixtureTree(t, f.root) != before {
		t.Fatal("the dry run changed the fixture")
	}
	got := f.pass(t, state, true)
	requireDecision(t, got, plainCache, "trimmed", "")
	requireExists(t, plainCache, false)
	// Nothing but node_modules goes, a Go cache under a home directory least of all.
	for _, p := range []string{"home/go-build/trim.txt", "home/go-build/aa/aa11-a", "home/go/pkg/mod/cache/download/sumdb", ".build/go/pkg/mod/x", "dist/app.js", "app.txt"} {
		requireExists(t, filepath.Join(plain, p), true)
	}
	// Retention is reported first for a verifier checkout and hides that it
	// is recent, so the trim checks the idle rule itself.
	requireDecision(t, got, recent, "kept", keepRetention)
	requireDecision(t, got, used, "kept", keepInUse)
	requireDecision(t, got, locked, "kept", keepLocked)
	for _, cache := range []string{usedCache, lockedCache} {
		if d, ok := got[cache]; ok {
			t.Fatalf("a cache decision in an in-use or locked checkout: %+v", d)
		}
	}
	for cache, want := range map[string][2]string{
		recentCache:   {keepRecent, "Git state changed 1h0m0s ago"},
		staleCache:    {keepRecent, "node_modules changed 1h0m0s ago"},
		trackedCache:  {keepUnproven, "tracked files"},
		citedCache:    {keepEvidence, "receipt.json"},
		unmarkedCache: {keepUnproven, "no npm install marker"},
		nestedCache:   {keepNested, "contains Git repository"},
	} {
		if d := requireDecision(t, got, cache, "kept", want[0]); d.Kind != kindCache || !strings.Contains(d.Detail, want[1]) {
			t.Fatalf("%s: %+v, want detail with %q", cache, d, want[1])
		}
	}
	for _, cache := range []string{recentCache, staleCache, usedCache, lockedCache, trackedCache, citedCache, unmarkedCache, nestedCache} {
		requireExists(t, filepath.Join(cache, "left-pad", "index.js"), true)
	}
	requireNoTombstones(t, plain, stale, nested)
}

// sharedCaches is the fake home's shared caches, byte for byte.
func (f trimFixture) sharedCaches(t *testing.T) string {
	t.Helper()
	return treeContents(t, f.home)
}

// t15 (a6): the never-touch proof. The sentinel shared caches are
// byte-identical after an applying sweep in every case.
func TestWorktreeCleanupCacheTrimNeverTouchesSharedCaches(t *testing.T) {
	// trap is a shared cache laid out so that a followed link would find a
	// node_modules to delete in it.
	setup := func(t *testing.T) (trimFixture, string, string, sweepHubState, string) {
		f := newTrimFixture(t)
		checkout := f.verifierCheckout(t, trimItemAcc, "verifier-abc1234")
		trap := filepath.Join(f.home, "go", "trap")
		installModules(t, filepath.Join(trap, "verifier-abc1234"))
		state := sweepHubState{entries: []api.TeamQueueEntry{f.accepted(trimItemAcc)}}
		return f, checkout, trap, state, f.sharedCaches(t)
	}
	t.Run("node_modules is a symlink to a shared cache", func(t *testing.T) {
		f, checkout, _, state, shared := setup(t)
		cache := filepath.Join(checkout, "node_modules")
		if err := os.RemoveAll(cache); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(f.home, ".npm"), cache); err != nil {
			t.Fatal(err)
		}
		got := f.pass(t, state, true)
		if d := requireDecision(t, got, cache, "kept", keepUnproven); !strings.Contains(d.Detail, "not a real directory") {
			t.Fatalf("decision %+v", d)
		}
		if target, err := os.Readlink(cache); err != nil || target != filepath.Join(f.home, ".npm") {
			t.Fatalf("the link was not left in place: %q %v", target, err)
		}
		if f.sharedCaches(t) != shared {
			t.Fatal("a shared cache changed")
		}
	})
	t.Run("a symlink inside node_modules points to a shared cache", func(t *testing.T) {
		f, checkout, _, state, shared := setup(t)
		cache := installModules(t, checkout)
		for name, target := range map[string]string{"go-build": filepath.Join(f.home, "Library", "Caches", "go-build"), "mod": filepath.Join(f.home, "go", "pkg"), ".bin/npm-cache": filepath.Join(f.home, ".npm")} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(cache, name)), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(cache, name)); err != nil {
				t.Fatal(err)
			}
		}
		got := f.pass(t, state, true)
		requireDecision(t, got, cache, "trimmed", "")
		requireExists(t, cache, false)
		requireNoTombstones(t, checkout)
		if f.sharedCaches(t) != shared {
			t.Fatal("a shared cache changed")
		}
	})
	swap := func(t *testing.T, replace func(f trimFixture, checkout, trap string)) {
		f, checkout, trap, state, shared := setup(t)
		cache := installModules(t, checkout)
		moved := ""
		trimBeforeDelete = func(path string) {
			if path != cache {
				t.Errorf("trim of %s", path)
			}
			replace(f, checkout, trap)
			moved = "done"
		}
		t.Cleanup(func() { trimBeforeDelete = nil })
		got := f.pass(t, state, true)
		trimBeforeDelete = nil
		if moved == "" {
			t.Fatal("the swap never ran")
		}
		// The pinned directory was trimmed wherever it went; what took its
		// place was not followed.
		requireDecision(t, got, cache, "trimmed", "")
		if f.sharedCaches(t) != shared {
			t.Fatal("a shared cache changed")
		}
		requireExists(t, filepath.Join(checkout, "node_modules", "left-pad", "index.js"), true)
	}
	t.Run("the checkout is swapped for a symlink to a shared cache before the delete", func(t *testing.T) {
		swap(t, func(f trimFixture, checkout, trap string) {
			if err := os.Rename(checkout, checkout+".moved"); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(filepath.Join(trap, "verifier-abc1234"), checkout); err != nil {
				t.Error(err)
			}
		})
	})
	t.Run("the checkout is swapped for another directory before the delete", func(t *testing.T) {
		swap(t, func(f trimFixture, checkout, trap string) {
			if err := os.Rename(checkout, checkout+".moved"); err != nil {
				t.Error(err)
			}
			installModules(t, checkout)
		})
	})
	t.Run("an ancestor is swapped for a symlink to a shared cache before the delete", func(t *testing.T) {
		swap(t, func(f trimFixture, checkout, trap string) {
			item := filepath.Dir(checkout)
			if err := os.Rename(item, item+".moved"); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(trap, item); err != nil {
				t.Error(err)
			}
		})
	})
	t.Run("a checkout swapped before it is pinned is kept", func(t *testing.T) {
		for _, kind := range []string{"symlink", "directory"} {
			f, checkout, trap, _, shared := setup(t)
			installModules(t, checkout)
			admin := worktreeAdminDir(checkout)
			classified, err := os.Lstat(checkout)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(checkout, checkout+".moved"); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Symlink(filepath.Join(trap, "verifier-abc1234"), checkout); err != nil {
					t.Fatal(err)
				}
			} else {
				installModules(t, checkout)
				writeFixtureFile(t, filepath.Join(checkout, ".git"), "gitdir: "+admin+"\n")
			}
			reason, detail := trimCheckoutCache(worktreeCleanupInputs{Now: f.now}, gitWorktree{Path: checkout}, admin, classified, f.artifacts(), nil)
			if reason != keepMoved || !strings.Contains(detail, "replaced") {
				t.Fatalf("%s: %q %q", kind, reason, detail)
			}
			if f.sharedCaches(t) != shared {
				t.Fatalf("%s: a shared cache changed", kind)
			}
			requireExists(t, filepath.Join(checkout, "node_modules", "left-pad", "index.js"), true)
			requireExists(t, filepath.Join(checkout+".moved", "node_modules", "left-pad", "index.js"), true)
			requireNoTombstones(t, checkout+".moved")
		}
	})
	t.Run("a never-touch path inside the harness roots is kept", func(t *testing.T) {
		f, checkout, _, state, shared := setup(t)
		cache := installModules(t, checkout)
		for name, value := range map[string]string{"GOMODCACHE": cache, "GOCACHE": filepath.Join(checkout, ".build", "go-build"), "GOPATH": f.root, "npm_config_cache": filepath.Join(cache, "left-pad")} {
			t.Run(name, func(t *testing.T) {
				t.Setenv(name, value)
				for _, apply := range []bool{false, true} {
					got := f.pass(t, state, apply)
					if d := requireDecision(t, got, cache, "kept", keepProtected); !strings.Contains(d.Detail, "never-touch path "+value) {
						t.Fatalf("decision %+v", d)
					}
				}
				requireExists(t, filepath.Join(cache, "left-pad", "index.js"), true)
			})
		}
		// The apply-time check stands on its own: the same file under another name.
		alias := filepath.Join(f.root, "alias-cache")
		if err := os.Symlink(cache, alias); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOMODCACHE", alias)
		classified, err := os.Lstat(checkout)
		if err != nil {
			t.Fatal(err)
		}
		if reason, _ := trimCheckoutCache(worktreeCleanupInputs{Now: f.now}, gitWorktree{Path: checkout}, worktreeAdminDir(checkout), classified, f.artifacts(), nil); reason != keepProtected {
			t.Fatalf("aliased never-touch path: %q", reason)
		}
		requireExists(t, filepath.Join(cache, "left-pad", "index.js"), true)
		if f.sharedCaches(t) != shared {
			t.Fatal("a shared cache changed")
		}
	})
}

// t15 (a6, R1): what an interrupted pass left is finished only when the
// recorded parent is provably the same directory; anything else deletes
// nothing, and a tombstone with no intent is never deleted by name.
func TestWorktreeCleanupTombstoneRecoveryFailsClosed(t *testing.T) {
	setup := func(t *testing.T) (trimFixture, string, string, sweepHubState, string) {
		f := newTrimFixture(t)
		checkout := f.verifierCheckout(t, trimItemAcc, "verifier-abc1234")
		installModules(t, checkout)
		state := sweepHubState{entries: []api.TeamQueueEntry{f.accepted(trimItemAcc)}}
		return f, checkout, filepath.Join(checkout, "node_modules"), state, f.sharedCaches(t)
	}
	crash := func(t *testing.T, f trimFixture, state sweepHubState, stage string) detachIntent {
		t.Helper()
		detachCrashAt = func(at string) bool { return at == stage }
		t.Cleanup(func() { detachCrashAt = nil })
		f.pass(t, state, true)
		detachCrashAt = nil
		intents, err := loadDetachIntents()
		if err != nil || len(intents) != 1 || intents[0].Kind != kindCache || intents[0].Source != "node_modules" || !validTombstone(intents[0].Tombstone) {
			t.Fatalf("intents after a crash at %s: %+v %v", stage, intents, err)
		}
		return intents[0]
	}
	recovered := func(t *testing.T, got map[string]worktreeDecision, cache string) worktreeDecision {
		t.Helper()
		d, ok := got[cache]
		if !ok {
			t.Fatalf("no decision for %s: %+v", cache, got)
		}
		return d
	}
	t.Run("crash after the intent and before the rename", func(t *testing.T) {
		f, checkout, cache, state, shared := setup(t)
		intent := crash(t, f, state, "intent")
		requireExists(t, filepath.Join(cache, "left-pad", "index.js"), true)
		requireExists(t, filepath.Join(checkout, intent.Tombstone), false)
		// The next pass drops the intent, deleting nothing for it.
		decisions := recoverDetachIntents(worktreeCleanupInputs{}, f.main, &artifactPass{root: f.artifacts()})
		if len(decisions) != 1 || decisions[0].Action != "kept" || decisions[0].Path != cache || !strings.Contains(decisions[0].Detail, "was never detached") {
			t.Fatalf("recovery %+v", decisions)
		}
		requireExists(t, filepath.Join(cache, "left-pad", "index.js"), true)
		requireNoTombstones(t, checkout)
		if f.sharedCaches(t) != shared {
			t.Fatal("a shared cache changed")
		}
		// And a whole pass afterwards trims normally.
		requireDecision(t, f.pass(t, state, true), cache, "trimmed", "")
		requireExists(t, cache, false)
	})
	t.Run("crash after the rename and before the delete", func(t *testing.T) {
		f, checkout, cache, state, shared := setup(t)
		intent := crash(t, f, state, "renamed")
		requireExists(t, cache, false)
		requireExists(t, filepath.Join(checkout, intent.Tombstone, "left-pad", "index.js"), true)
		if intent.Parent != checkout || intent.Root != f.artifacts() || intent.Inode == 0 {
			t.Fatalf("intent %+v", intent)
		}
		// A stray tombstone with no intent is not the pass's to delete.
		stray := filepath.Join(checkout, tombstonePrefix+"stray")
		writeFixtureFile(t, filepath.Join(stray, "keep.txt"), "no intent names this\n")
		got := f.pass(t, state, true)
		d := recovered(t, got, cache)
		if d.Action != "trimmed" || d.Kind != kindCache || !strings.Contains(d.Detail, "finished an interrupted sweep") {
			t.Fatalf("recovery %+v", d)
		}
		requireExists(t, filepath.Join(checkout, intent.Tombstone), false)
		requireExists(t, filepath.Join(stray, "keep.txt"), true)
		if intents, _ := loadDetachIntents(); len(intents) != 0 {
			t.Fatalf("intents left %+v", intents)
		}
		if f.sharedCaches(t) != shared {
			t.Fatal("a shared cache changed")
		}
	})
	replaced := func(t *testing.T, name, cause string, replace func(f trimFixture, checkout string, intent detachIntent)) {
		t.Run(name, func(t *testing.T) {
			f, checkout, cache, state, _ := setup(t)
			// The trap holds a directory with the tombstone's own name.
			intent := crash(t, f, state, "renamed")
			trap := filepath.Join(f.home, "go", "trap")
			writeFixtureFile(t, filepath.Join(trap, intent.Tombstone, "shared.txt"), "shared cache\n")
			replace(f, checkout, intent)
			shared := f.sharedCaches(t)
			before := fixtureTree(t, f.root)
			decisions := recoverDetachIntents(worktreeCleanupInputs{}, f.main, &artifactPass{root: f.artifacts()})
			if len(decisions) != 1 || decisions[0].Action != "kept" || decisions[0].Path != cache || !strings.Contains(decisions[0].Detail, cause) || !strings.Contains(decisions[0].Detail, "nothing deleted") {
				t.Fatalf("recovery %+v, want cause %q", decisions, cause)
			}
			if f.sharedCaches(t) != shared {
				t.Fatal("a shared cache changed")
			}
			if fixtureTree(t, f.root) != before {
				t.Fatal("recovery deleted something")
			}
			if intents, _ := loadDetachIntents(); len(intents) != 0 {
				t.Fatalf("the intent was not dropped: %+v", intents)
			}
			// One kept decision: the next pass has nothing left to say about it.
			if again := recoverDetachIntents(worktreeCleanupInputs{}, f.main, &artifactPass{root: f.artifacts()}); len(again) != 0 {
				t.Fatalf("a second recovery decision: %+v", again)
			}
		})
	}
	replaced(t, "the parent is replaced by another directory", "not the directory the intent recorded", func(f trimFixture, checkout string, intent detachIntent) {
		if err := os.Rename(checkout, checkout+".moved"); err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, filepath.Join(checkout, intent.Tombstone, "other.txt"), "another directory's own\n")
	})
	replaced(t, "the parent is replaced by a symlink to a shared cache", "not the directory the intent recorded", func(f trimFixture, checkout string, intent detachIntent) {
		if err := os.Rename(checkout, checkout+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(f.home, "go", "trap"), checkout); err != nil {
			t.Fatal(err)
		}
	})
	replaced(t, "the tombstone is replaced by a symlink", "not a real directory", func(f trimFixture, checkout string, intent detachIntent) {
		tombstone := filepath.Join(checkout, intent.Tombstone)
		if err := os.Rename(tombstone, tombstone+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(f.home, "go", "trap", intent.Tombstone), tombstone); err != nil {
			t.Fatal(err)
		}
	})
	replaced(t, "the parent is gone", "cannot be opened", func(f trimFixture, checkout string, intent detachIntent) {
		if err := os.Rename(checkout, checkout+".moved"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("an intent that is malformed or under another root deletes nothing", func(t *testing.T) {
		f, checkout, _, state, shared := setup(t)
		intent := crash(t, f, state, "renamed")
		tombstone := filepath.Join(checkout, intent.Tombstone)
		for name, change := range map[string]func(*detachIntent){
			"a tombstone without the prefix": func(i *detachIntent) { i.Tombstone = "node_modules" },
			"a tombstone with a path":        func(i *detachIntent) { i.Tombstone = tombstonePrefix + "x/../../app.txt" },
			"a parent outside its root":      func(i *detachIntent) { i.Root = filepath.Join(f.artifacts(), "wi_ffffffffffffffff") },
			"an unknown kind":                func(i *detachIntent) { i.Kind = "worktree" },
			"a session temp kind in a tree":  func(i *detachIntent) { i.Kind = kindSessionTemp },
		} {
			bad := intent
			change(&bad)
			if cause, retry := finishDetach(bad); cause == "" || retry {
				t.Fatalf("%s: finished (%q, %v)", name, cause, retry)
			}
			requireExists(t, filepath.Join(tombstone, "left-pad", "index.js"), true)
		}
		// A pass over other roots leaves the intent for the pass that owns it.
		if decisions := recoverDetachIntents(worktreeCleanupInputs{}, filepath.Join(f.root, "another-repo"), nil); len(decisions) != 0 {
			t.Fatalf("another repository's pass decided %+v", decisions)
		}
		if intents, _ := loadDetachIntents(); len(intents) != 1 {
			t.Fatalf("intents %+v", intents)
		}
		if f.sharedCaches(t) != shared {
			t.Fatal("a shared cache changed")
		}
	})
	t.Run("nothing is renamed when the intent cannot be saved", func(t *testing.T) {
		f, checkout, cache, state, _ := setup(t)
		if err := os.MkdirAll(filepath.Join(sweepStatePath(), "blocked"), 0700); err != nil {
			t.Fatal(err)
		}
		got := f.pass(t, state, true)
		if d := requireDecision(t, got, cache, "kept", keepFailed); !strings.Contains(d.Detail, "nothing renamed") {
			t.Fatalf("decision %+v", d)
		}
		requireExists(t, filepath.Join(cache, "left-pad", "index.js"), true)
		if left, _ := filepath.Glob(filepath.Join(checkout, tombstonePrefix+"*")); len(left) != 0 {
			t.Fatalf("tombstones %v", left)
		}
	})
}

// The sweep removes a session temp folder through a tombstone in the pinned
// temp root, read-only directories included, and finishes an interrupted one.
func TestWorktreeCleanupSessionTempTombstone(t *testing.T) {
	f := newTrimFixture(t)
	cwd := f.queuePath("queue-a6a60001")
	folder := filepath.Join(f.tempRoot(), claudeScratchKey(cwd))
	fill := func() {
		writeFixtureFile(t, filepath.Join(folder, "session", "scratchpad", "mod", "pkg", "file.go"), "package pkg\n")
		if err := os.Chmod(filepath.Join(folder, "session", "scratchpad", "mod", "pkg"), 0555); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(folder, "session", "scratchpad", "mod"), 0555); err != nil {
			t.Fatal(err)
		}
	}
	fill()
	t.Cleanup(func() { _, _ = makeWorktreeWritable(f.tempRoot()) })
	stray := filepath.Join(f.tempRoot(), tombstonePrefix+"stray")
	writeFixtureFile(t, filepath.Join(stray, "keep.txt"), "no intent names this\n")
	released := releasedArtifactEntry(trimItemRel)
	released.Host, released.Cwd = trimHost, cwd
	state := sweepHubState{entries: []api.TeamQueueEntry{released}}
	shared := f.sharedCaches(t)
	if d := requireDecision(t, f.pass(t, state, true), folder, "removed", ""); d.Kind != kindSessionTemp || d.Bytes <= 0 {
		t.Fatalf("decision %+v", d)
	}
	requireExists(t, folder, false)
	requireExists(t, filepath.Join(stray, "keep.txt"), true)
	if intents, _ := loadDetachIntents(); len(intents) != 0 {
		t.Fatalf("intents %+v", intents)
	}
	// Interrupted after the rename: the next pass deletes the tombstone.
	fill()
	detachCrashAt = func(at string) bool { return at == "renamed" }
	t.Cleanup(func() { detachCrashAt = nil })
	f.pass(t, state, true)
	detachCrashAt = nil
	intents, _ := loadDetachIntents()
	if len(intents) != 1 || intents[0].Kind != kindSessionTemp || intents[0].Parent != f.tempRoot() || intents[0].Root != f.tempRoot() {
		t.Fatalf("intents %+v", intents)
	}
	tombstone := filepath.Join(f.tempRoot(), intents[0].Tombstone)
	requireExists(t, folder, false)
	requireExists(t, filepath.Join(tombstone, "session", "scratchpad", "mod", "pkg", "file.go"), true)
	got := f.pass(t, state, true)
	if d := requireDecision(t, got, folder, "removed", ""); d.Kind != kindSessionTemp || !strings.Contains(d.Detail, "finished an interrupted sweep") {
		t.Fatalf("recovery %+v", d)
	}
	requireExists(t, tombstone, false)
	requireExists(t, filepath.Join(stray, "keep.txt"), true)
	if f.sharedCaches(t) != shared {
		t.Fatal("a shared cache changed")
	}
	if intents, _ := loadDetachIntents(); len(intents) != 0 {
		t.Fatalf("intents %+v", intents)
	}
}

// t16: closeout steps aside quietly while a sweep holds the cleanup lock, and
// still returns every other error.
func TestWorktreeCleanupCloseoutSkipsQuietlyWhileASweepHoldsTheLock(t *testing.T) {
	r := newCleanupRepo(t)
	const host = "fixture"
	const task = "tsk_c1ea0c1ea0c1ea16"
	cwd := r.queuePath("queue-a1a60001")
	cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", cwd, "tasks-hub")
	head := cleanupGit(t, r.main, "rev-parse", "HEAD")
	finished := api.TeamQueueEntry{ID: "tqe_t16_finished", TaskID: task, ItemID: artifactItemA, State: "finished", Host: host, Cwd: cwd, Repository: r.common, Position: 1,
		Acceptance: &api.TeamIntegrationAcceptance{Repository: r.common, Worktree: cwd, Commit: head, AcceptedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)},
		Release:    &api.ReleaseJob{State: "released"}}
	hub := &cleanupHub{details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen}}}}
	server := httptest.NewServer(hub)
	defer server.Close()
	c, err := api.NewClient(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	list := api.TeamQueueList{Entries: []api.TeamQueueEntry{finished}}
	closeout := func() (string, error) {
		closeoutWorktreeChecks.Delete(finished.ID)
		var stderr strings.Builder
		restore := captureStderr(t, &stderr)
		err := closeoutWorktrees(context.Background(), c, host, api.TeamQueueList{}, list, finished)
		restore()
		return stderr.String(), err
	}
	lock, err := worktreeCleanupLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worktreeCleanupLock(); !errors.Is(err, errWorktreeCleanupActive) || !strings.Contains(err.Error(), "worktree cleanup is active on this host") {
		t.Fatalf("second lock: %v", err)
	}
	if said, err := closeout(); err != nil || said != "" {
		t.Fatalf("closeout under a held lock: %v, said %q", err, said)
	}
	requireExists(t, cwd, true)
	if receipts := readCleanupReceipts(t); len(receipts) != 0 {
		t.Fatalf("receipts %+v", receipts)
	}
	unlockQueueLaunch(lock)
	// Another error is still returned: here the project cannot be read.
	hub.mu.Lock()
	hub.details = map[string]api.TaskDetail{}
	hub.mu.Unlock()
	if _, err := closeout(); err == nil {
		t.Fatal("a hub failure was swallowed")
	}
	requireExists(t, cwd, true)
	hub.mu.Lock()
	hub.details = map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen}}}
	hub.mu.Unlock()
	if said, err := closeout(); err != nil || !strings.Contains(said, "removed "+cwd) {
		t.Fatalf("closeout with the lock free: %v, said %q", err, said)
	}
	requireExists(t, cwd, false)
}
