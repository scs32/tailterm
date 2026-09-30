package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

func newCleanupRepo(t *testing.T) cleanupRepo {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("TAILTERM_RELAY_STATE", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
