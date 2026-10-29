package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/scs32/tailterm/hub/internal/api"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestDeploymentSetupUsesExactRetryJournal(t *testing.T) {
	f := newHandlerFixture(t)
	f.req.Role = api.AgentRoleDeployment
	f.req.Name = "deployer"
	a, err := f.launch()
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.launch()
	if err != nil || again.RunID != a.RunID {
		t.Fatal(again, err)
	}
	f.executions(t, 1)
	if !strings.HasPrefix(a.Session, "tt-deployer-") {
		t.Fatal(a.Session)
	}
}
func TestDeploymentBriefingPreservesHandlerBoundary(t *testing.T) {
	b := deploymentBriefing()
	for _, text := range []string{"Never read or mutate work-item APIs", "exact-SHA", "private host credential", "persistent", "handler"} {
		if !strings.Contains(strings.ToLower(b), strings.ToLower(text)) {
			t.Fatal(text)
		}
	}
}

// coverageFixture builds a repository whose tasks-hub moved on and then
// carried a candidate branch by cherry-pick, plus a range with a merge.
func coverageFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=F", "GIT_AUTHOR_EMAIL=f@example.invalid", "GIT_COMMITTER_NAME=F", "GIT_COMMITTER_EMAIL=f@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(args, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(file, text string) string {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", file)
		run("commit", "-m", file+" "+text)
		return run("rev-parse", "HEAD")
	}
	c := map[string]string{}
	run("init", "-b", "tasks-hub")
	c["base"] = commit("base.txt", "base")
	run("checkout", "-b", "feat")
	c["c1"] = commit("one.txt", "one")
	c["c2"] = commit("two.txt", "two")
	run("checkout", "-b", "side", c["base"])
	y1 := commit("side.txt", "side")
	run("checkout", "-b", "merged", c["base"])
	x1 := commit("three.txt", "three")
	run("merge", "--no-ff", "-m", "merge side", "side")
	c["merge"] = run("rev-parse", "HEAD")
	// tasks-hub moves on, then carries the candidate by cherry-pick.
	run("checkout", "tasks-hub")
	c["moved"] = commit("other.txt", "moved")
	run("cherry-pick", c["c1"])
	c["partial"] = run("rev-parse", "HEAD")
	run("cherry-pick", c["c2"])
	c["picked"] = run("rev-parse", "HEAD")
	run("cherry-pick", x1, y1)
	c["pickedMerge"] = run("rev-parse", "HEAD")
	return dir, c
}

// s4: supersede and hand-release prove every patch of the job's range is in
// the released commit on the local tasks-hub: by ancestry, or by patch
// equivalence for a cherry-picked hand release. Every commit changes a file.
func TestSupersedeCoverage(t *testing.T) {
	dir, c := coverageFixture(t)
	base, c1, c2, merge, moved, partial, picked, pickedMerge := c["base"], c["c1"], c["c2"], c["merge"], c["moved"], c["partial"], c["picked"], c["pickedMerge"]
	jobs := []api.ReleaseJob{
		{ID: "rel_plain", BaseCommit: base, Commit: moved},
		{ID: "rel_picked", BaseCommit: base, Commit: c2},
		{ID: "rel_merge", BaseCommit: base, Commit: merge},
	}
	job := func(id string) api.ReleaseJob {
		j, err := findReleaseJob(jobs, id)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	t.Run("ancestor accepted", func(t *testing.T) {
		if _, err := supersedeCoverage(dir, job("rel_plain"), picked); err != nil {
			t.Fatal(err)
		}
	})
	// Cherry-picked onto a moved tasks-hub: ancestry fails, all patches present.
	t.Run("cherry-picked release accepted", func(t *testing.T) {
		if exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", c2, picked).Run() == nil {
			t.Fatal("fixture: candidate must not be an ancestor of the hand release")
		}
		if _, err := supersedeCoverage(dir, job("rel_picked"), picked); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing patch refused and named", func(t *testing.T) {
		if _, err := supersedeCoverage(dir, job("rel_picked"), partial); err == nil || !strings.Contains(err.Error(), c2) || strings.Contains(err.Error(), c1) {
			t.Fatal(err)
		}
	})
	t.Run("released commit not on tasks-hub refused", func(t *testing.T) {
		if _, err := supersedeCoverage(dir, job("rel_picked"), c2); err == nil || !strings.Contains(err.Error(), "refs/heads/tasks-hub") {
			t.Fatal(err)
		}
	})
	// A merge in the range needs ancestry: git cherry skips merges.
	t.Run("merge in range without ancestry refused", func(t *testing.T) {
		if _, err := supersedeCoverage(dir, job("rel_merge"), pickedMerge); err == nil || !strings.Contains(err.Error(), "merge") {
			t.Fatal(err)
		}
	})
	if _, err := findReleaseJob(jobs, "rel_missing"); err == nil {
		t.Fatal("missing job accepted")
	}
	// f5: a malformed released or job commit is refused before git runs, so
	// an option-shaped value cannot write a file.
	marker := filepath.Join(t.TempDir(), "written")
	noRepo := filepath.Join(t.TempDir(), "no-repo")
	for _, bad := range []string{"--output=" + marker, picked[:12], strings.ToUpper(picked), ""} {
		if _, err := supersedeCoverage(noRepo, job("rel_picked"), bad); err == nil || !strings.Contains(err.Error(), "40-hex") {
			t.Fatalf("%q: %v", bad, err)
		}
		if _, err := supersedeCoverage(noRepo, api.ReleaseJob{ID: "rel_bad", BaseCommit: bad, Commit: c2}, picked); err == nil || !strings.Contains(err.Error(), "full commit") {
			t.Fatalf("base %q: %v", bad, err)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("git ran with an option-shaped commit", err)
	}
}

// s2: the CLI's coverage guard runs before any hub write, for supersede and
// for recording a hand release; a covered job reaches the hub citing the
// record, and a hand release sends the proven job commits and the session's
// agent identity so the hub can refuse agents.
func TestSupersedeAndHandReleaseGuardBeforeHub(t *testing.T) {
	dir, c := coverageFixture(t)
	task := "tsk_0123456789abcdef"
	jobs := []api.ReleaseJob{{ID: "rel_picked", BaseCommit: c["base"], Commit: c["c2"], State: "refused", Generation: 3}}
	var posts []api.ReleaseRequest
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if serveSettledPage(w, r, nil) {
			return
		}
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/releases") {
			if r.URL.Query().Get("job") != jobs[0].ID {
				t.Error("expected exact detail read", r.URL.String())
			}
			detail := jobs[0]
			detail.TaskID = task
			_ = json.NewEncoder(w).Encode(detail)
			return
		}
		var req api.ReleaseRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		posts = append(posts, req)
		if req.Operation == "hand_release" && req.AgentID != "" {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "conflict: release: only the owner records a hand release"})
			return
		}
		if req.Operation == "hand_release" {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.HandRelease{ID: "hrl_0123456789abcdef"})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(jobs[0])
	}))
	defer hub.Close()
	e := env{hub: hub.URL, task: task, agent: "agt_0123456789abcdef", runID: "run_0123456789abcdef"}
	supersede := func(released string, extra ...string) error {
		args := append([]string{"supersede", "--job", "rel_picked", "--generation", "3", "--request-id", "sup-" + released[:8], "--released-commit", released, "--repo", dir}, extra...)
		_, err := captureRelayOutput(t, false, func() error { return cmdDeployment(e, args) })
		return err
	}
	if err := supersede(c["picked"]); err == nil || !strings.Contains(err.Error(), "--hand-release") {
		t.Fatal("supersede without a record", err)
	}
	if err := supersede(c["partial"], "--hand-release", "hrl_0123456789abcdef"); err == nil || !strings.Contains(err.Error(), c["c2"]) {
		t.Fatal("partial supersede", err)
	}
	owner := env{hub: hub.URL, task: task}
	record := func(as env, released string) error {
		args := []string{"hand-release", "--intervention", "7", "--released-commit", released, "--release", "20260930-hand", "--target", "hub", "--job", "rel_picked", "--repo", dir, "--request-id", "hand-" + released[:8]}
		_, err := captureRelayOutput(t, false, func() error { return cmdDeployment(as, args) })
		return err
	}
	if err := record(owner, c["partial"]); err == nil || !strings.Contains(err.Error(), c["c2"]) {
		t.Fatal("partial hand release", err)
	}
	if len(posts) != 0 {
		t.Fatalf("refused commands reached the hub: %+v", posts)
	}
	// An agent shell sends its identity; the hub refuses and the CLI says how
	// to record it as the owner.
	if err := record(e, c["picked"]); err == nil || !strings.Contains(err.Error(), "env -u TAILTERM_AGENT -u TAILTERM_RUN") {
		t.Fatal("agent shell hand release", err)
	}
	if err := record(owner, c["picked"]); err != nil {
		t.Fatal(err)
	}
	if err := supersede(c["picked"], "--hand-release", "hrl_0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if len(posts) != 3 {
		t.Fatalf("posts %+v", posts)
	}
	if posts[0].AgentID != e.agent || posts[0].RunID != e.runID {
		t.Fatalf("agent identity not sent %+v", posts[0])
	}
	h := posts[1].HandRelease
	if posts[1].Operation != "hand_release" || posts[1].AgentID != "" || posts[1].RunID != "" || h == nil || h.InterventionSeq != 7 || h.ReleasedCommit != c["picked"] || len(h.Commits) != 1 || h.Commits[0] != c["c2"] || len(h.Targets) != 1 || h.Targets[0] != "hub" {
		t.Fatalf("hand release request %+v", posts[1])
	}
	sup := posts[2].Supersession
	if posts[2].Operation != "supersede" || sup == nil || sup.HandReleaseID != "hrl_0123456789abcdef" || sup.ReleasedCommit != c["picked"] {
		t.Fatalf("supersede request %+v", posts[2])
	}
}

// a1-a4: a hand release with a tailos target is recorded only with a retained
// rollback copy that matches the live release.json; --dist verifies a build
// and retains it as the runner does. Other targets need neither flag. The
// journal directory is a temp directory and release.json is served locally.
func TestHandReleaseRequiresRetainedTailOSCopy(t *testing.T) {
	dir, c := coverageFixture(t)
	task, released := "tsk_0123456789abcdef", c["picked"]
	jobs := []api.ReleaseJob{{ID: "rel_picked", BaseCommit: c["base"], Commit: c["c2"], State: "refused", Generation: 3}}
	assets := map[string]string{"index.html": "<html>home</html>", "assets/app.js": "console.log(1)"}
	manifest := func(commit string, files map[string]string) []byte {
		m := map[string]any{"schema": 1, "commit": commit, "files": map[string]any{}}
		for name, text := range files {
			sum := sha256.Sum256([]byte(text))
			m["files"].(map[string]any)[name] = map[string]any{"size": len(text), "sha256": hex.EncodeToString(sum[:])}
		}
		b, _ := json.Marshal(m)
		return b
	}
	liveCommit := released
	var posts []api.ReleaseRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/release.json":
			_, _ = w.Write(manifest(liveCommit, assets))
		case serveSettledPage(w, r, nil):
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/releases"):
			if r.URL.Query().Get("job") != jobs[0].ID {
				t.Error("expected exact detail read", r.URL.String())
			}
			detail := jobs[0]
			detail.TaskID = task
			_ = json.NewEncoder(w).Encode(detail)
		default:
			var req api.ReleaseRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			posts = append(posts, req)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.HandRelease{ID: "hrl_0123456789abcdef"})
		}
	}))
	defer server.Close()
	journal := filepath.Join(t.TempDir(), "journal")
	if err := os.Mkdir(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "deploy.json")
	doc, _ := json.Marshal(map[string]any{"version": 1, "journalDirectory": journal, "targets": map[string]any{"tailos": map[string]any{"url": server.URL + "/release.json"}}})
	if err := os.WriteFile(config, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(files map[string]string) string {
		root := t.TempDir()
		for name, text := range files {
			path := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	with := func(extra map[string]string) map[string]string {
		files := map[string]string{"release.json": string(manifest(released, assets))}
		for name, text := range assets {
			files[name] = text
		}
		for name, text := range extra {
			files[name] = text
		}
		return files
	}
	owner := env{hub: server.URL, task: task}
	record := func(target string, extra ...string) error {
		args := append([]string{"hand-release", "--intervention", "7", "--released-commit", released, "--release", "20261002-hand", "--target", target, "--job", "rel_picked", "--repo", dir, "--request-id", "hand-tailos"}, extra...)
		_, err := captureRelayOutput(t, false, func() error { return cmdDeployment(owner, args) })
		return err
	}
	retained := filepath.Join(journal, "tailos-dist-"+released)
	refused := func(label string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", label, err)
		}
		if strings.Contains(err.Error(), journal) {
			t.Fatalf("%s names the private journal path: %v", label, err)
		}
		if entries, _ := os.ReadDir(journal); len(entries) != 0 || len(posts) != 0 {
			t.Fatalf("%s retained %d entries or recorded %+v", label, len(entries), posts)
		}
	}
	refused("no config", record("tailos"), "--deploy-config")
	refused("missing directory", record("tailos", "--deploy-config", config), "--dist PATH")
	refused("different file", record("tailos", "--deploy-config", config, "--dist", build(with(map[string]string{"assets/app.js": "console.log(2)"}))), "different: assets/app.js")
	refused("extra file", record("tailos", "--deploy-config", config, "--dist", build(with(map[string]string{"extra.txt": "x"}))), "extra: extra.txt")
	short := with(nil)
	delete(short, "index.html")
	refused("missing file", record("tailos", "--deploy-config", config, "--dist", build(short)), "missing: index.html")
	refused("other build's release.json", record("tailos", "--deploy-config", config, "--dist", build(with(map[string]string{"release.json": string(manifest(c["base"], assets))}))), "does not name the released commit")
	liveCommit = c["base"]
	refused("live commit differs", record("tailos", "--deploy-config", config, "--dist", build(with(nil))), "TailOS serves "+c["base"])
	liveCommit = released
	refused("dist without tailos", record("hub", "--dist", build(with(nil))), "--target tailos")

	// A matching build is retained (0700, no .tmp left) and then recorded.
	if err := record("tailos", "--deploy-config", config, "--dist", build(with(nil))); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(retained)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("retained copy %v %v", info, err)
	}
	if b, err := os.ReadFile(filepath.Join(retained, "assets", "app.js")); err != nil || string(b) != assets["assets/app.js"] {
		t.Fatalf("retained file %q %v", b, err)
	}
	if entries, _ := os.ReadDir(journal); len(entries) != 1 {
		t.Fatalf("journal entries %v", entries)
	}
	if len(posts) != 1 || posts[0].Operation != "hand_release" || posts[0].HandRelease == nil || posts[0].HandRelease.ReleasedCommit != released || !reflect.DeepEqual(posts[0].HandRelease.Targets, []string{"tailos"}) {
		t.Fatalf("hand release request %+v", posts)
	}
	// The valid retained copy now satisfies the guard without --dist, and is
	// kept when --dist is passed again.
	before := info.ModTime()
	if err := record("tailos", "--deploy-config", config); err != nil {
		t.Fatal(err)
	}
	if err := record("tailos", "--deploy-config", config, "--dist", build(with(nil))); err != nil {
		t.Fatal(err)
	}
	if info, err = os.Stat(retained); err != nil || !info.ModTime().Equal(before) || len(posts) != 3 {
		t.Fatalf("existing copy not kept: %v %v posts=%d", info, err, len(posts))
	}
	// A retained copy that no longer matches is refused, with or without --dist.
	if err := os.WriteFile(filepath.Join(retained, "index.html"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{{"--deploy-config", config}, {"--deploy-config", config, "--dist", build(with(nil))}} {
		if err := record("tailos", extra...); err == nil || !strings.Contains(err.Error(), "different: index.html") || !strings.Contains(err.Error(), "--dist PATH") {
			t.Fatal("stale retained copy", err)
		}
	}
	// Other targets are unchanged: no config, no dist, no journal write.
	if err := record("hub"); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(journal); len(posts) != 4 || !reflect.DeepEqual(posts[3].HandRelease.Targets, []string{"hub"}) || len(entries) != 1 {
		t.Fatalf("non-tailos hand release %+v", posts)
	}
}

// serveSettledPage answers a settled-view release list with these summaries
// as one page, and reports whether the request was one.
func serveSettledPage(w http.ResponseWriter, r *http.Request, jobs []api.ReleaseSummary) bool {
	if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/releases") || r.URL.Query().Get("view") != "settled" {
		return false
	}
	if jobs == nil {
		jobs = []api.ReleaseSummary{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "jobs": jobs, "page": map[string]any{"view": "settled", "limit": 200, "snapshot": strings.Repeat("ab", 32), "nextAfter": ""}})
	return true
}

// handReleaseHub is a fake hub for tt deployment hand-release and supersede:
// exact job reads, the settled ledger in pages of two under one snapshot, the
// live TailOS release.json (after slow) and the record or supersede request.
type handReleaseHub struct {
	server  *httptest.Server
	task    string
	jobs    []api.ReleaseJob
	settled []api.ReleaseSummary
	live    []byte
	slow    time.Duration
	mu      sync.Mutex
	posts   []api.ReleaseRequest
	pages   []string
}

func newHandReleaseHub(t *testing.T, jobs ...api.ReleaseJob) *handReleaseHub {
	t.Helper()
	h := &handReleaseHub{task: "tsk_0123456789abcdef", jobs: jobs}
	snapshot := strings.Repeat("cd", 32)
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/release.json":
			time.Sleep(h.slow)
			_, _ = w.Write(h.live)
		case r.Method == "GET" && q.Get("view") == "settled":
			h.mu.Lock()
			h.pages = append(h.pages, r.URL.RawQuery)
			h.mu.Unlock()
			start := 0
			if q.Get("after") != "" {
				if q.Get("snapshot") != snapshot {
					t.Error("later settled page without the first page's snapshot", r.URL.RawQuery)
				}
				start, _ = strconv.Atoi(q.Get("after"))
			}
			end, next := min(start+2, len(h.settled)), ""
			if end < len(h.settled) {
				next = strconv.Itoa(end)
			}
			jobs := append([]api.ReleaseSummary{}, h.settled[start:end]...)
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "jobs": jobs, "page": map[string]any{"view": "settled", "limit": 200, "snapshot": snapshot, "nextAfter": next}})
		case r.Method == "GET":
			j, err := findReleaseJob(h.jobs, q.Get("job"))
			if err != nil {
				t.Error("unexpected read", r.URL.String())
			}
			j.TaskID = h.task
			_ = json.NewEncoder(w).Encode(j)
		default:
			var req api.ReleaseRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			h.mu.Lock()
			h.posts = append(h.posts, req)
			h.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			if req.Operation == "hand_release" {
				_ = json.NewEncoder(w).Encode(api.HandRelease{ID: "hrl_0123456789abcdef"})
				return
			}
			j := h.jobs[0]
			j.TaskID = h.task
			_ = json.NewEncoder(w).Encode(j)
		}
	}))
	t.Cleanup(h.server.Close)
	return h
}

func (h *handReleaseHub) recorded() []api.ReleaseRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]api.ReleaseRequest{}, h.posts...)
}

// settledRelease is a settled summary: a released receipt for its targets, or
// with state superseded a supersession naming them.
func settledRelease(task string, row int64, id, state, settledAt, commit string, targets ...string) api.ReleaseSummary {
	s := api.ReleaseSummary{Summary: true, RowID: row, ID: id, TaskID: task, State: state, SettledAt: settledAt}
	if state == "superseded" {
		s.Supersession = &api.ReleaseSummarySupersession{ReleasedCommit: commit, Targets: targets}
		return s
	}
	s.Receipt = &api.ReleaseSummaryReceipt{Outcome: "released", Commit: commit}
	for _, target := range targets {
		s.Receipt.Targets = append(s.Receipt.Targets, api.ReleaseSummaryTarget{Target: target, Outcome: "released"})
	}
	return s
}

// tailosHandRelease gives a deploy config whose journal is a temp directory
// and whose TailOS URL is the fake hub's release.json for released, plus a
// matching dist-static build.
func tailosHandRelease(t *testing.T, h *handReleaseHub, released string) (config, journal, dist string) {
	t.Helper()
	assets := map[string]string{"index.html": "<html>home</html>", "assets/app.js": "console.log(1)"}
	m := map[string]any{"schema": 1, "commit": released, "files": map[string]any{}}
	for name, text := range assets {
		sum := sha256.Sum256([]byte(text))
		m["files"].(map[string]any)[name] = map[string]any{"size": len(text), "sha256": hex.EncodeToString(sum[:])}
	}
	h.live, _ = json.Marshal(m)
	journal = filepath.Join(t.TempDir(), "journal")
	if err := os.Mkdir(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	config = filepath.Join(t.TempDir(), "deploy.json")
	doc, _ := json.Marshal(map[string]any{"version": 1, "journalDirectory": journal, "targets": map[string]any{"tailos": map[string]any{"url": h.server.URL + "/release.json"}}})
	if err := os.WriteFile(config, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	dist = t.TempDir()
	assets["release.json"] = string(h.live)
	for name, text := range assets {
		path := filepath.Join(dist, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return config, journal, dist
}

// wi_d305 (a5): a hand release recorded after the ledger already released a
// newer commit to a target would settle later and move that target's baseline
// back, so it is refused before any retain or record, naming target, job and
// commit. Equal, older and other-target ledger commits record as before.
func TestHandReleaseRefusesTargetWithNewerRelease(t *testing.T) {
	dir, c := coverageFixture(t)
	released := c["picked"] // pickedMerge descends from it, partial and moved are its ancestors
	h := newHandReleaseHub(t, api.ReleaseJob{ID: "rel_picked", BaseCommit: c["base"], Commit: c["c2"], State: "refused", Generation: 3})
	task := h.task
	h.settled = []api.ReleaseSummary{
		settledRelease(task, 1, "rel_legacy", "released", "", c["pickedMerge"], "bridge"),
		settledRelease(task, 2, "rel_newer", "released", "2026-10-28T10:00:00Z", c["pickedMerge"], "hub"),
		// Listed later but settled earlier: rel_newer stays the hub's latest.
		settledRelease(task, 3, "rel_early", "released", "2026-10-28T09:00:00.5Z", c["moved"], "hub"),
		settledRelease(task, 4, "rel_older", "released", "2026-10-28T11:00:00Z", c["partial"], "mini"),
		settledRelease(task, 5, "rel_same", "superseded", "2026-10-28T08:00:00Z", released, "tailos"),
		// A timed event is newer than every untimed one: bridge's latest is moved.
		settledRelease(task, 6, "rel_bridge", "released", "2026-10-28T07:00:00Z", c["moved"], "bridge"),
	}
	config, journal, dist := tailosHandRelease(t, h, released)
	owner := env{hub: h.server.URL, task: task}
	record := func(key string, targets ...string) error {
		args := []string{"hand-release", "--intervention", "7", "--released-commit", released, "--release", "20261028-hand", "--job", "rel_picked", "--repo", dir, "--request-id", key}
		for _, target := range targets {
			args = append(args, "--target", target)
			if target == "tailos" {
				args = append(args, "--deploy-config", config, "--dist", dist)
			}
		}
		_, err := captureRelayOutput(t, false, func() error { return cmdDeployment(owner, args) })
		return err
	}
	unchanged := func(label string, posts int) {
		t.Helper()
		if entries, _ := os.ReadDir(journal); len(entries) != 0 || len(h.recorded()) != posts {
			t.Fatalf("%s retained %d entries or recorded %+v", label, len(entries), h.recorded())
		}
	}
	err := record("hand-1", "hub", "tailos")
	if err == nil || !strings.Contains(err.Error(), "hub: release rel_newer shipped "+c["pickedMerge"]+" at 2026-10-28T10:00:00Z") || !strings.Contains(err.Error(), "Record it without --target hub;") || strings.Contains(err.Error(), "Every target") || strings.Contains(err.Error(), "tailos:") {
		t.Fatal("one target behind the ledger", err)
	}
	unchanged("hub behind", 0)
	if err = record("hand-2", "hub"); err == nil || !strings.Contains(err.Error(), "rel_newer") || !strings.Contains(err.Error(), "Every target is newer in the ledger") {
		t.Fatal("every target behind the ledger", err)
	}
	unchanged("only hub", 0)
	// The whole ledger was read, under the first page's snapshot.
	if pages := h.pages; len(pages) != 6 || strings.Contains(pages[0], "after=") || !strings.Contains(pages[1], "after=2") || !strings.Contains(pages[2], "after=4") {
		t.Fatal("settled pages", pages)
	}
	// An older ledger commit (mini), an equal one (tailos) and an untimed
	// newer one (bridge) are recorded as before.
	if err = record("hand-3", "mini", "tailos", "bridge"); err != nil {
		t.Fatal(err)
	}
	if posts := h.recorded(); len(posts) != 1 || posts[0].HandRelease == nil || !reflect.DeepEqual(posts[0].HandRelease.Targets, []string{"mini", "tailos", "bridge"}) {
		t.Fatalf("recorded %+v", posts)
	}
	if entries, _ := os.ReadDir(journal); len(entries) != 1 {
		t.Fatal("retained copy", entries)
	}
	// A ledger commit this checkout does not have cannot be compared.
	h.settled = append(h.settled, settledRelease(task, 7, "rel_unknown", "released", "2026-10-28T12:00:00Z", strings.Repeat("f", 40), "mini"))
	if err = record("hand-4", "mini"); err == nil || !strings.Contains(err.Error(), "cannot compare") || !strings.Contains(err.Error(), "rel_unknown") || len(h.recorded()) != 1 {
		t.Fatal("unknown ledger commit", err)
	}
}

// wi_01d8 (a6): an empty commit carries no patch, and cherry-picking a range
// drops it, so coverage ignores it and names it (short SHA and subject); a
// dropped commit that changes something is still missing.
func TestHandReleaseCoverageSkipsEmptyCommits(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=F", "GIT_AUTHOR_EMAIL=f@example.invalid", "GIT_COMMITTER_NAME=F", "GIT_COMMITTER_EMAIL=f@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatal(args, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(file, subject string) string {
		if file == "" {
			run("commit", "--allow-empty", "-m", subject)
		} else {
			if err := os.WriteFile(filepath.Join(dir, file), []byte(subject), 0o644); err != nil {
				t.Fatal(err)
			}
			run("add", file)
			run("commit", "-m", subject)
		}
		return run("rev-parse", "HEAD")
	}
	named := func(c string) string { return run("log", "-1", "--format=%h %s", c) }
	run("init", "-b", "tasks-hub")
	base := commit("base.txt", "base")
	run("checkout", "-b", "feat")
	e1 := commit("", "empty one")
	p := commit("p.txt", "change p")
	e2 := commit("", "empty two")
	run("checkout", "-b", "two", base)
	q1 := commit("q1.txt", "change q1")
	q2 := commit("q2.txt", "change q2")
	run("checkout", "-b", "blank", base)
	b1 := commit("", "blank one")
	b2 := commit("", "blank two")
	run("checkout", "tasks-hub")
	moved := commit("other.txt", "moved")
	run("cherry-pick", p)
	rel := run("rev-parse", "HEAD")
	run("cherry-pick", q1)
	relQ := run("rev-parse", "HEAD")
	if exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", e2, rel).Run() == nil {
		t.Fatal("fixture: the job must not be an ancestor of the hand release")
	}
	cases := []struct {
		name     string
		job      api.ReleaseJob
		released string
		ignored  []string
		missing  string
	}{
		{"cherry-pick dropped the range's empty commits", api.ReleaseJob{ID: "rel_feat", BaseCommit: base, Commit: e2}, rel, []string{named(e1), named(e2)}, ""},
		{"a range of only empty commits", api.ReleaseJob{ID: "rel_blank", BaseCommit: base, Commit: b2}, rel, []string{named(b1), named(b2)}, ""},
		{"an empty job range", api.ReleaseJob{ID: "rel_none", BaseCommit: base, Commit: base}, rel, nil, ""},
		{"an empty range off the hand release's line", api.ReleaseJob{ID: "rel_off", BaseCommit: b2, Commit: b2}, rel, nil, ""},
		{"a dropped non-empty commit is missing", api.ReleaseJob{ID: "rel_two", BaseCommit: base, Commit: q2}, relQ, nil, q2},
		{"empty commits are named even when a change is missing", api.ReleaseJob{ID: "rel_feat", BaseCommit: base, Commit: e2}, moved, []string{named(e1), named(e2)}, p},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ignored, err := supersedeCoverage(dir, tc.job, tc.released)
			if !reflect.DeepEqual(ignored, tc.ignored) {
				t.Fatalf("ignored %q, want %q", ignored, tc.ignored)
			}
			if tc.missing == "" && err != nil {
				t.Fatal(err)
			}
			if tc.missing != "" && (err == nil || !strings.Contains(err.Error(), "missing patches") || !strings.Contains(err.Error(), tc.missing) || strings.Contains(err.Error(), q1) || strings.Contains(err.Error(), e1)) {
				t.Fatal(err)
			}
		})
	}
	// Both commands that prove coverage name what they ignored on stderr.
	h := newHandReleaseHub(t, api.ReleaseJob{ID: "rel_feat", BaseCommit: base, Commit: e2, State: "refused", Generation: 3})
	owner := env{hub: h.server.URL, task: h.task}
	agent := env{hub: h.server.URL, task: h.task, agent: "agt_0123456789abcdef", runID: "run_0123456789abcdef"}
	want := []string{"coverage of rel_feat: ignored empty commit " + named(e1) + "\n", "coverage of rel_feat: ignored empty commit " + named(e2) + "\n"}
	for _, cmd := range []struct {
		as   env
		args []string
	}{
		{owner, []string{"hand-release", "--intervention", "7", "--released-commit", rel, "--release", "20261028-hand", "--target", "hub", "--job", "rel_feat", "--repo", dir, "--request-id", "hand-empty"}},
		{agent, []string{"supersede", "--job", "rel_feat", "--generation", "3", "--request-id", "sup-empty", "--released-commit", rel, "--hand-release", "hrl_0123456789abcdef", "--repo", dir}},
	} {
		stderr, err := captureRelayOutput(t, true, func() error {
			_, err := captureRelayOutput(t, false, func() error { return cmdDeployment(cmd.as, cmd.args) })
			return err
		})
		if err != nil || stderr != strings.Join(want, "") {
			t.Fatalf("%s: stderr %q, err %v", cmd.args[0], stderr, err)
		}
	}
	if posts := h.recorded(); len(posts) != 2 || posts[0].Operation != "hand_release" || posts[1].Operation != "supersede" {
		t.Fatalf("posts %+v", posts)
	}
}

// wi_4e59 (a8): a failed retain copy names the copy by its config key and the
// failed step, never the private journal path, and records nothing.
func TestHandReleaseRetainErrorHidesJournalPath(t *testing.T) {
	dir, c := coverageFixture(t)
	released := c["picked"]
	h := newHandReleaseHub(t, api.ReleaseJob{ID: "rel_picked", BaseCommit: c["base"], Commit: c["c2"], State: "refused", Generation: 3})
	config, journal, dist := tailosHandRelease(t, h, released)
	if err := os.Chmod(journal, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(journal, 0o700) })
	args := []string{"hand-release", "--intervention", "7", "--released-commit", released, "--release", "20261028-hand", "--target", "tailos", "--job", "rel_picked", "--repo", dir, "--request-id", "hand-ro", "--deploy-config", config, "--dist", dist}
	out, err := captureRelayOutput(t, false, func() error { return cmdDeployment(env{hub: h.server.URL, task: h.task}, args) })
	if err == nil || !strings.Contains(err.Error(), "retaining --dist as journalDirectory/tailos-dist-"+released+" failed at copy: permission denied; nothing was recorded") {
		t.Fatal(err)
	}
	if strings.Contains(err.Error()+out, journal) || strings.Contains(err.Error(), filepath.Dir(journal)) {
		t.Fatalf("the private journal path was printed: %v %q", err, out)
	}
	if len(h.recorded()) != 0 {
		t.Fatalf("recorded %+v", h.recorded())
	}
	// Each later step's path-carrying error keeps only its cause.
	tmp := filepath.Join(journal, "tailos-dist-"+released+".tmp")
	for _, tc := range []struct {
		step string
		err  error
		want string
	}{
		{"permissions", &fs.PathError{Op: "chmod", Path: tmp, Err: syscall.EPERM}, "operation not permitted"},
		{"rename", &os.LinkError{Op: "rename", Old: tmp, New: strings.TrimSuffix(tmp, ".tmp"), Err: syscall.ENOTEMPTY}, "directory not empty"},
		{"verify", fmt.Errorf("wrapped: %w", &fs.PathError{Op: "open", Path: tmp, Err: syscall.EACCES}), "permission denied"},
		{"verify", errors.New("missing: index.html"), "missing: index.html"},
	} {
		got := retainError(released, tc.step, tc.err).Error()
		if got != "retaining --dist as journalDirectory/tailos-dist-"+released+" failed at "+tc.step+": "+tc.want+"; nothing was recorded" || strings.Contains(got, journal) {
			t.Fatal(tc.step, got)
		}
	}
}

// wi_e7a1 (a9): the record request has its own deadline, so a slow
// release.json read and the copy before it cannot use up the record's time.
func TestHandReleaseRecordGetsItsOwnDeadline(t *testing.T) {
	dir, c := coverageFixture(t)
	released := c["picked"]
	h := newHandReleaseHub(t, api.ReleaseJob{ID: "rel_picked", BaseCommit: c["base"], Commit: c["c2"], State: "refused", Generation: 3})
	config, _, dist := tailosHandRelease(t, h, released)
	// TestMain raised the command timeout floor for the package; use the
	// shipped floor so the shortened deadline applies.
	previous, floor := handReleaseTimeout, minCommandTimeout
	handReleaseTimeout, minCommandTimeout = time.Second, productionMinCommandTimeout
	t.Cleanup(func() { handReleaseTimeout, minCommandTimeout = previous, floor })
	h.slow = 3 * handReleaseTimeout / 2
	args := []string{"hand-release", "--intervention", "7", "--released-commit", released, "--release", "20261028-hand", "--target", "tailos", "--job", "rel_picked", "--repo", dir, "--request-id", "hand-slow", "--deploy-config", config, "--dist", dist}
	started := time.Now()
	if _, err := captureRelayOutput(t, false, func() error { return cmdDeployment(env{hub: h.server.URL, task: h.task}, args) }); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took < h.slow {
		t.Fatalf("release.json was not slower than the deadline: %v", took)
	}
	if posts := h.recorded(); len(posts) != 1 || posts[0].Operation != "hand_release" {
		t.Fatalf("recorded %+v", posts)
	}
}

// wi_8afe404c4867dafc p5b: tt deployment finish sends the runner's receipt
// file to the hub with every push diagnostic as written (a zero is kept), and
// a receipt without them reaches the hub with the same keys as the file.
func TestDeploymentFinishPassesPushDiagnostics(t *testing.T) {
	var posts []api.ReleaseRequest
	var bodies [][]byte
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		var req api.ReleaseRequest
		_ = json.Unmarshal(b, &req)
		posts = append(posts, req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ReleaseJob{ID: "rel_fixture", State: "released", Generation: 4})
	}))
	defer hub.Close()
	e := env{hub: hub.URL, task: "tsk_0123456789abcdef", agent: "agt_0123456789abcdef", runID: "run_0123456789abcdef"}
	commit := strings.Repeat("c", 40)
	finish := func(name, push string) api.ReleaseRequest {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		receipt := `{"version":1,"jobId":"rel_fixture","commit":"` + commit + `","verificationDigest":"` + strings.Repeat("d", 64) + `","targets":[],"outcome":"released","push":` + push + `}`
		if err := os.WriteFile(path, []byte(receipt), 0o600); err != nil {
			t.Fatal(err)
		}
		before := len(posts)
		if _, err := captureRelayOutput(t, false, func() error {
			return cmdDeployment(e, []string{"finish", "--job", "rel_fixture", "--generation", "3", "--request-id", "finish-" + name, "--file", path})
		}); err != nil || len(posts) != before+1 {
			t.Fatalf("%s: %v %d", name, err, len(posts))
		}
		got := posts[len(posts)-1]
		if got.Operation != "finish" || got.Receipt == nil || got.Receipt.Push == nil {
			t.Fatalf("%s: %+v", name, got)
		}
		return got
	}
	full := `{"remote":"origin","commit":"` + commit + `","outcome":"failed","timedOut":true,"exitCode":128,"elapsedMs":0,"reason":"push-auth-failed","remoteCheck":"absent","stderrSHA256":"` + strings.Repeat("e", 64) + `"}`
	p := finish("full.json", full).Receipt.Push
	if p.Remote != "origin" || p.Commit != commit || p.Outcome != "failed" || !p.TimedOut || p.ExitCode == nil || *p.ExitCode != 128 || p.Signal != "" || p.ElapsedMs == nil || *p.ElapsedMs != 0 || p.Reason != "push-auth-failed" || p.RemoteCheck != "absent" || p.StderrSHA256 != strings.Repeat("e", 64) {
		t.Fatalf("full push %+v", p)
	}
	signal := `{"remote":"origin","commit":"` + commit + `","outcome":"failed","signal":"sigterm","elapsedMs":300001,"remoteCheck":"unknown"}`
	if p = finish("signal.json", signal).Receipt.Push; p.Signal != "sigterm" || p.ExitCode != nil || p.ElapsedMs == nil || *p.ElapsedMs != 300001 || p.RemoteCheck != "unknown" {
		t.Fatalf("signal push %+v", p)
	}
	bare := `{"remote":"origin","commit":"` + commit + `","outcome":"pushed"}`
	if p = finish("bare.json", bare).Receipt.Push; *p != (api.ReleasePush{Remote: "origin", Commit: commit, Outcome: "pushed"}) {
		t.Fatalf("bare push %+v", p)
	}
	// The pushes as they went over the wire: each equals the file's object.
	for i, want := range []string{full, signal, bare} {
		var wire struct {
			Receipt struct {
				Push json.RawMessage `json:"push"`
			} `json:"receipt"`
		}
		if err := json.Unmarshal(bodies[i], &wire); err != nil {
			t.Fatal(err)
		}
		var got, file map[string]any
		if json.Unmarshal(wire.Receipt.Push, &got) != nil || json.Unmarshal([]byte(want), &file) != nil || !reflect.DeepEqual(got, file) {
			t.Fatalf("push %d on the wire %s, file %s", i, wire.Receipt.Push, want)
		}
	}
}

// a13: tt deployment retry refuses a missing reason, entry or job and a
// malformed, unknown or repeated --restored before any hub call, then sends
// the handler's retry with the reason and the restored releases. The usage
// line lists retry.
func TestDeploymentRetryGuardsBeforeHub(t *testing.T) {
	var requests []string
	var posts []api.ReleaseRequest
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		var req api.ReleaseRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		posts = append(posts, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.ReleaseJob{ID: "rel_new", State: "verified", Generation: 1, RetryOf: &api.ReleaseRetry{JobID: "rel_refused"}})
	}))
	defer hub.Close()
	e := env{hub: hub.URL, task: "tsk_0123456789abcdef", agent: "agt_0123456789abcdef", runID: "run_0123456789abcdef"}
	retry := func(args ...string) error {
		_, err := captureRelayOutput(t, false, func() error { return cmdDeployment(e, append([]string{"retry"}, args...)) })
		return err
	}
	base := []string{"--entry", "tqe_0123456789abcdef", "--job", "rel_refused", "--generation", "4", "--request-id", "retry-1"}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no reason":         {base, "--reason"},
		"blank reason":      {with("--reason", "  "), "--reason"},
		"two line reason":   {with("--reason", "fixed\nreally"), "--reason"},
		"no entry":          {[]string{"--job", "rel_refused", "--generation", "4", "--request-id", "retry-1", "--reason", "fixed"}, "--entry and --job"},
		"no job":            {[]string{"--entry", "tqe_0123456789abcdef", "--generation", "4", "--request-id", "retry-1", "--reason", "fixed"}, "--entry and --job"},
		"restored no value": {with("--reason", "fixed", "--restored", "hub"), "TARGET=RELEASE"},
		"restored empty":    {with("--reason", "fixed", "--restored", "hub="), "TARGET=RELEASE"},
		"unknown target":    {with("--reason", "fixed", "--restored", "nas=20260930-old"), "unknown target"},
		"repeated target":   {with("--reason", "fixed", "--restored", "hub=20260930-old", "--restored", "hub=20260929-old"), "target hub twice"},
		"no request id":     {[]string{"--entry", "tqe_0123456789abcdef", "--job", "rel_refused", "--generation", "4", "--reason", "fixed"}, "request-id"},
	} {
		if err := retry(c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(requests) != 0 {
		t.Fatalf("refused retries reached the hub: %v", requests)
	}
	if err := retry(with("--reason", "Temp dir fixed; both targets restored", "--restored", "hub=20260930-old", "--restored", "bridge=20260929-old")...); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || len(posts) != 1 {
		t.Fatalf("requests %v", requests)
	}
	got := posts[0]
	if got.Operation != "retry" || got.RequestID != "retry-1" || got.AgentID != e.agent || got.RunID != e.runID || got.EntryID != "tqe_0123456789abcdef" || got.JobID != "rel_refused" || got.ExpectedGeneration != 4 || got.Retry == nil || got.Retry.Reason != "Temp dir fixed; both targets restored" {
		t.Fatalf("retry request %+v", got)
	}
	if r := got.Retry.Restored; len(r) != 2 || r[0] != (api.ReleaseRestoredTarget{Target: "hub", Release: "20260930-old"}) || r[1] != (api.ReleaseRestoredTarget{Target: "bridge", Release: "20260929-old"}) {
		t.Fatalf("restored %+v", got.Retry.Restored)
	}
	if err := cmdDeployment(e, nil); err == nil || !strings.Contains(err.Error(), "|supersede|retry|") {
		t.Fatal("usage does not list retry", err)
	}
	// Other operations never carry a retry record.
	if _, err := captureRelayOutput(t, false, func() error {
		return cmdDeployment(e, []string{"claim", "--job", "rel_new", "--generation", "1", "--request-id", "claim-1"})
	}); err != nil || len(posts) != 2 || posts[1].Operation != "claim" || posts[1].Retry != nil {
		t.Fatalf("claim %+v %v", posts, err)
	}
}

// wi_5b03fe47520b7c4f p1: setup provisions the matrix prerequisites in the
// deployer's checkout before it spawns the role, and spawns nothing otherwise.
func TestDeploymentSetupProvisionsPrerequisitesBeforeSpawn(t *testing.T) {
	var provisioned [][2]string
	var spawned [][]string
	provisionErr := error(nil)
	restoreProvision, restoreSpawn := provisionDeploymentPrerequisites, spawnDeploymentAgent
	t.Cleanup(func() { provisionDeploymentPrerequisites, spawnDeploymentAgent = restoreProvision, restoreSpawn })
	provisionDeploymentPrerequisites = func(checkout, source string) error {
		provisioned = append(provisioned, [2]string{checkout, source})
		return provisionErr
	}
	spawnDeploymentAgent = func(_ env, args []string) error {
		spawned = append(spawned, args)
		return nil
	}
	for _, args := range [][]string{
		{"--name", "deployer", "--run", "tt deployment serve --config /private/c.json"},
		{"--cwd", "/deployer", "--name", "deployer"},
		{"--prerequisites-from", "/root", "--name", "deployer"},
		{"--cwd", "/deployer", "--prerequisites-from"},
	} {
		if err := cmdDeployment(env{}, append([]string{"setup"}, args...)); err == nil {
			t.Fatalf("%q: setup accepted", args)
		}
	}
	if len(provisioned) != 0 || len(spawned) != 0 {
		t.Fatal("refused setup provisioned or spawned", provisioned, spawned)
	}
	provisionErr = errors.New("deployment prerequisites: Missing matrix prerequisites: .build/test.wasm")
	err := cmdDeployment(env{}, []string{"setup", "--cwd", "/deployer", "--prerequisites-from", "/root", "--name", "deployer"})
	if err == nil || !strings.Contains(err.Error(), "Missing matrix prerequisites: .build/test.wasm") || len(spawned) != 0 {
		t.Fatal("failed provisioning spawned or lost its reason", err, spawned)
	}
	provisionErr = nil
	provisioned = nil
	if err := cmdDeployment(env{}, []string{"setup", "--name", "deployer", "--cwd=/deployer", "-prerequisites-from=/root", "--run", "tt deployment serve --config /private/c.json"}); err != nil {
		t.Fatal(err)
	}
	if want := [][2]string{{"/deployer", "/root"}}; !reflect.DeepEqual(provisioned, want) {
		t.Fatal(provisioned)
	}
	want := []string{"--role", api.AgentRoleDeployment, "--name", "deployer", "--cwd", "/deployer", "--run", "tt deployment serve --config /private/c.json"}
	if len(spawned) != 1 || !reflect.DeepEqual(spawned[0], want) {
		t.Fatal(spawned)
	}
}

// The real seam runs the checkout's runner and returns only its named reason.
func TestDeploymentProvisioningReturnsTheRunnerReason(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := "if(process.argv.includes('--from')&&process.argv.at(-1)==='/good'){console.log('{\"version\":1}')}else{console.error('noise');console.error('Missing matrix prerequisites: .build/test.wasm');process.exitCode=1}"
	if err := os.WriteFile(filepath.Join(checkout, "scripts", "release-runner.mjs"), []byte(runner), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := provisionPrerequisites(checkout, "/good"); err != nil {
		t.Fatal(err)
	}
	err := provisionPrerequisites(checkout, "/bad")
	if err == nil || err.Error() != "deployment prerequisites: Missing matrix prerequisites: .build/test.wasm" {
		t.Fatal(err)
	}
}

// wi_31e817b4f98a1153 p1-p3: a successful run's stderr notes follow its JSON on
// tt's stderr, unchanged and bounded; a failed run still returns only the
// reason and echoes nothing.
func TestDeploymentProvisioningShowsRunnerStderrOnSuccess(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := `const from = process.argv.at(-1);
if (from === '/warn') {
  console.log('{"version":1}');
  process.stderr.write('go-licenses: copied none of the tree: key differs\n  texts come from module directories\n');
} else if (from === '/flood') {
  console.log('{"version":1}');
  process.stderr.write('x'.repeat(99) + '\n');
  process.stderr.write(('y'.repeat(99) + '\n').repeat(1000));
} else {
  console.log('{"partial":true}');
  console.error('noise');
  console.error('Missing matrix prerequisites: .build/test.wasm');
  process.exitCode = 1;
}
`
	if err := os.WriteFile(filepath.Join(checkout, "scripts", "release-runner.mjs"), []byte(runner), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(source string) (string, string, error) {
		var stdout string
		stderr, err := captureStream(t, &os.Stderr, func() error {
			var runErr error
			stdout, runErr = captureStream(t, &os.Stdout, func() error { return provisionPrerequisites(checkout, source) })
			return runErr
		})
		return stdout, stderr, err
	}

	stdout, stderr, err := run("/warn")
	if err != nil || stdout != "{\"version\":1}\n" {
		t.Fatalf("stdout JSON changed: %q %v", stdout, err)
	}
	if want := "go-licenses: copied none of the tree: key differs\n  texts come from module directories\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}

	stdout, stderr, err = run("/bad")
	if err == nil || err.Error() != "deployment prerequisites: Missing matrix prerequisites: .build/test.wasm" {
		t.Fatal(err)
	}
	if stdout != "{\"partial\":true}\n" || stderr != "" {
		t.Fatalf("failure echoed runner output: stdout %q stderr %q", stdout, stderr)
	}

	// 100 + 100000 bytes: whole lines up to the limit, then one count line.
	stdout, stderr, err = run("/flood")
	if err != nil || stdout != "{\"version\":1}\n" {
		t.Fatalf("stdout JSON changed: %q %v", stdout, err)
	}
	shown := 100 + (provisionStderrLimit-100)/100*100
	lines := strings.SplitAfter(stderr, "\n")
	if body := strings.Join(lines[:len(lines)-2], ""); len(body) != shown || !strings.HasPrefix(body, strings.Repeat("x", 99)+"\n") || !strings.HasSuffix(body, strings.Repeat("y", 99)+"\n") {
		t.Fatalf("bounded stderr: %d bytes, want %d whole lines", len(body), shown)
	}
	if want := fmt.Sprintf("deployment prerequisites: %d more bytes of runner stderr not shown (limit %d)\n", 100100-shown, provisionStderrLimit); lines[len(lines)-2] != want {
		t.Fatalf("limit line = %q, want %q", lines[len(lines)-2], want)
	}
}

func TestDeploymentBoundedListAndExactGet(t *testing.T) {
	task := "tsk_0123456789abcdef"
	id := "rel_0123456789abcdef"
	var paths []string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("job") != "" {
			json.NewEncoder(w).Encode(api.ReleaseJob{ID: id, TaskID: task, Plan: api.VerificationPlan{Commit: "full"}})
			return
		}
		json.NewEncoder(w).Encode(api.ReleasePage{Version: 1, Jobs: []api.ReleaseSummary{}, Page: api.ReleasePageInfo{View: r.URL.Query().Get("view"), Limit: 200, Snapshot: strings.Repeat("a", 64)}})
	}))
	defer hub.Close()
	e := env{hub: hub.URL, task: task, token: "fixture"}
	output, err := captureCLIOutput(t, func() error {
		return cmdDeployment(e, []string{"list", "--view", "settled", "--limit", "200", "--after", "cursor", "--snapshot", strings.Repeat("a", 64)})
	})
	if err != nil || !strings.Contains(output, `"version": 1`) {
		t.Fatal(output, err)
	}
	if !strings.Contains(paths[0], "after=cursor") || !strings.Contains(paths[0], "view=settled") || !strings.Contains(paths[0], "limit=200") {
		t.Fatal(paths)
	}
	output, err = captureCLIOutput(t, func() error { return cmdDeployment(e, []string{"get", "--job", id}) })
	if err != nil || !strings.Contains(output, `"commit": "full"`) || !strings.Contains(paths[1], "job="+id) {
		t.Fatal(output, err, paths)
	}
}

func TestDeploymentDedicatedCompatibilityCLILeavesOrdinaryReadsStrict(t *testing.T) {
	task, id := "tsk_0123456789abcdef", "rel_0123456789abcdef"
	job := api.ReleaseJob{ID: id, TaskID: task, State: "claimed", Generation: 2, Commit: strings.Repeat("c", 40), Plan: api.VerificationPlan{Commit: "full-plan"}, Receipt: &api.ReleaseReceipt{Version: 1, JobID: id, Targets: []api.ReleaseTargetReceipt{{Target: "hub", Backup: "retained-full-backup"}}}}
	status := 200
	posts := 0
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			posts++
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode([]api.ReleaseJob{job})
	}))
	defer hub.Close()
	e := env{hub: hub.URL, task: task, token: "fixture"}
	out, err := captureCLIOutput(t, func() error { return cmdDeployment(e, []string{"compat-list"}) })
	if err != nil || !strings.HasPrefix(strings.TrimSpace(out), "[") || !strings.Contains(out, "retained-full-backup") {
		t.Fatal(out, err)
	}
	out, err = captureCLIOutput(t, func() error { return cmdDeployment(e, []string{"compat-list", "--view", "active", "--limit", "200"}) })
	if err != nil || !strings.Contains(out, `"summary": true`) || !strings.Contains(out, `"version": 1`) {
		t.Fatal(out, err)
	}
	out, err = captureCLIOutput(t, func() error { return cmdDeployment(e, []string{"compat-get", "--job", id}) })
	if err != nil || !strings.Contains(out, "full-plan") || !strings.Contains(out, "retained-full-backup") {
		t.Fatal(out, err)
	}
	for _, args := range [][]string{{"list"}, {"get", "--job", id}, {"compat-get", "--job", id, "--snapshot", strings.Repeat("a", 64)}, {"compat-list", "--limit", "201"}, {"compat-list", "--after", "wrong"}} {
		out, err = captureCLIOutput(t, func() error { return cmdDeployment(e, args) })
		if err == nil || out != "" {
			t.Fatal(args, out, err)
		}
	}
	status = 401
	out, err = captureCLIOutput(t, func() error { return cmdDeployment(e, []string{"compat-list"}) })
	if err == nil || out != "" || posts != 0 {
		t.Fatal(out, err, posts)
	}
}

// holdHub is a hub that saves release actions, serves the job's item and
// records board posts; failPost makes the board refuse.
type holdHub struct {
	task     string
	job      api.ReleaseJob
	requests []string
	actions  []api.ReleaseRequest
	posts    []api.PostMessageRequest
	failPost bool
}

func newHoldHub(t *testing.T) (*holdHub, env) {
	t.Helper()
	h := &holdHub{task: "tsk_0123456789abcdef", job: api.ReleaseJob{ID: "rel_0123456789abcdef", ItemID: "wi_0123456789abcdef", ItemRevision: 2, OrderMessageSeq: 41, Commit: strings.Repeat("c", 40)}}
	h.job.TaskID = h.task
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests = append(h.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/work-items/"+h.job.ItemID):
			// The item moved on after the job's accepted revision.
			_ = json.NewEncoder(w).Encode(api.WorkItem{ID: h.job.ItemID, TaskID: h.task, Revision: 5})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/releases/actions"):
			var req api.ReleaseRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			h.actions = append(h.actions, req)
			out := h.job
			out.Generation = req.ExpectedGeneration + 1
			out.State = "verified"
			if req.Operation == "hold" {
				out.State = "held"
				out.Hold = &api.ReleaseHold{Reason: req.Hold.Reason, UntilItems: req.Hold.UntilItems, NotBefore: req.Hold.NotBefore, AgentID: req.AgentID, RunID: req.RunID, HeldAt: "2026-10-07T09:00:00Z"}
			}
			if req.JobID == "" && req.EntryID != "" {
				// An entry hold's response: no job yet.
				out = api.ReleaseJob{TaskID: h.task, EntryID: req.EntryID, ItemID: h.job.ItemID, OrderMessageSeq: h.job.OrderMessageSeq}
				if req.Operation == "hold" {
					out.Hold = &api.ReleaseHold{Reason: req.Hold.Reason, NotBefore: req.Hold.NotBefore, AgentID: req.AgentID, RunID: req.RunID, HeldAt: "2026-10-07T09:00:00Z"}
				}
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages"):
			var req api.PostMessageRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			h.posts = append(h.posts, req)
			if h.failPost {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "conflict: board unavailable"})
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.Message{Seq: 77})
		default:
			t.Error("unexpected request", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(hub.Close)
	return h, env{hub: hub.URL, task: h.task, agent: "agt_0123456789abcdef", runID: "run_0123456789abcdef"}
}

// wi_0c5c5add2242991b a8: hold sends the typed request, then posts one typed
// notice; bad input never reaches the hub.
func TestDeploymentHoldSendsRequestThenNotice(t *testing.T) {
	h, e := newHoldHub(t)
	hold := func(args ...string) (string, error) {
		return captureRelayOutput(t, false, func() error { return cmdDeployment(e, append([]string{"hold"}, args...)) })
	}
	base := []string{"--job", h.job.ID, "--generation", "1", "--request-id", "hold-1"}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
	many := make([]string, 17)
	for i := range many {
		many[i] = fmt.Sprintf("wi_%016x", i+1)
	}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no reason":       {base, "--reason"},
		"blank reason":    {with("--reason", "  "), "--reason"},
		"two line reason": {with("--reason", "wait\nreally"), "--reason"},
		"long reason":     {with("--reason", strings.Repeat("r", 513)), "--reason"},
		"no job":          {[]string{"--generation", "1", "--request-id", "hold-1", "--reason", "wait"}, "--job"},
		"malformed item":  {with("--reason", "wait", "--until-items", "wi_0123456789abcdee,item-two"), "work item IDs"},
		"empty item":      {with("--reason", "wait", "--until-items", "wi_0123456789abcdee,"), "work item IDs"},
		"repeated item":   {with("--reason", "wait", "--until-items", "wi_0123456789abcdee,wi_0123456789abcdee"), "twice"},
		"too many items":  {with("--reason", "wait", "--until-items", strings.Join(many, ",")), "at most 16"},
	} {
		if _, err := hold(c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(h.requests) != 0 {
		t.Fatalf("refused holds reached the hub: %v", h.requests)
	}
	if _, err := hold("--job", h.job.ID, "--generation", "1", "--reason", "wait"); err == nil || !strings.Contains(err.Error(), "request-id") || len(h.actions) != 0 || len(h.posts) != 0 {
		t.Fatal("hold without a request id", err, h.requests)
	}
	h.requests = nil
	out, err := hold(with("--reason", "Safety fixes must release first.", "--until-items", "wi_0123456789abcdee, wi_0123456789abcdef")...)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.actions) != 1 || len(h.posts) != 1 || h.requests[0] != "POST /v1/tasks/"+h.task+"/releases/actions" || !strings.HasSuffix(h.requests[len(h.requests)-1], "/messages") {
		t.Fatalf("requests %v", h.requests)
	}
	got := h.actions[0]
	if got.Operation != "hold" || got.RequestID != "hold-1" || got.AgentID != e.agent || got.RunID != e.runID || got.JobID != h.job.ID || got.ExpectedGeneration != 1 || got.Hold == nil || got.Hold.Reason != "Safety fixes must release first." || len(got.Hold.UntilItems) != 2 || got.Hold.UntilItems[0] != "wi_0123456789abcdee" || got.Hold.UntilItems[1] != "wi_0123456789abcdef" || got.Hold.AgentID != "" || got.Retry != nil {
		t.Fatalf("hold request %+v %+v", got, got.Hold)
	}
	var printed api.ReleaseJob
	if json.Unmarshal([]byte(out), &printed) != nil || printed.State != "held" || printed.Hold == nil {
		t.Fatal("output", out)
	}
	post := h.posts[0]
	if post.Envelope == nil || post.Envelope.Kind != api.EnvelopeKindNotice || post.Envelope.Subject != "A release job is on hold" || post.Envelope.To != "" || post.To != "" || post.Text != api.RenderText(*post.Envelope) {
		t.Fatalf("notice %+v", post)
	}
	if problems := api.ValidateEnvelope(*post.Envelope); len(problems) != 0 {
		t.Fatal(problems)
	}
	if post.Envelope.Refs["release-job"] != h.job.ID || post.Envelope.Refs["item"] != h.job.ItemID || post.RequestID != h.job.ID+"-hold-g2" || post.AgentID != e.agent || post.RunID != e.runID {
		t.Fatalf("notice identity %+v %v", post, post.Envelope.Refs)
	}
	text := post.Envelope.Body.Text
	for _, want := range []string{h.job.ID, h.job.ItemID, "cccccccccccc)", "Reason: Safety fixes must release first. Held by " + e.agent, "wi_0123456789abcdee, wi_0123456789abcdef", "will not claim it", "tt deployment unhold"} {
		if !strings.Contains(text, want) {
			t.Fatalf("notice text lacks %q: %s", want, text)
		}
	}
	if len(post.WorkItems) != 1 || post.WorkItems[0] != (api.MessageWorkItem{ItemTaskID: h.task, ItemID: h.job.ItemID, ItemRevision: 5, Relationship: "primary"}) || post.WorkOrderMessage == nil || *post.WorkOrderMessage != (api.MessageReference{TaskID: h.task, Seq: 41}) {
		t.Fatalf("notice links %+v %+v", post.WorkItems, post.WorkOrderMessage)
	}
	// A hold with no items says how it ends.
	if _, err = hold("--job", h.job.ID, "--generation", "3", "--request-id", "hold-2", "--reason", "Owner reads it first"); err != nil {
		t.Fatal(err)
	}
	if n := len(h.posts); n != 2 || h.actions[1].Hold.UntilItems != nil || !strings.Contains(h.posts[1].Envelope.Body.Text, "stays on hold until tt deployment unhold") || h.posts[1].RequestID != h.job.ID+"-hold-g4" {
		t.Fatalf("plain hold %+v", h.posts)
	}
	if err := cmdDeployment(e, nil); err == nil || !strings.Contains(err.Error(), "|set-aside|hold|unhold|refuse|") {
		t.Fatal("usage does not list hold and unhold", err)
	}
	// Other operations carry no hold record and post nothing.
	if _, err := captureRelayOutput(t, false, func() error {
		return cmdDeployment(e, []string{"claim", "--job", h.job.ID, "--generation", "1", "--request-id", "claim-1"})
	}); err != nil || len(h.actions) != 3 || h.actions[2].Operation != "claim" || h.actions[2].Hold != nil || len(h.posts) != 2 {
		t.Fatalf("claim %+v %v", h.actions, err)
	}
}

// wi_c09132e28129688d h1: hold --entry saves an entry hold before the
// entry's next job exists, then posts one notice linked to its item and
// order; bad combinations never reach the hub.
func TestDeploymentHoldEntrySendsEntryRequestThenNotice(t *testing.T) {
	h, e := newHoldHub(t)
	run := func(op string, args ...string) (string, error) {
		return captureRelayOutput(t, false, func() error { return cmdDeployment(e, append([]string{op}, args...)) })
	}
	const entry = "tqe_0123456789abcdef"
	const notBefore = "2026-10-29T07:00:00-07:00"
	for name, c := range map[string]struct {
		op   string
		args []string
		want string
	}{
		"entry and job":         {"hold", []string{"--entry", entry, "--job", h.job.ID, "--request-id", "e", "--reason", "wait"}, "exactly one of --job"},
		"neither":               {"hold", []string{"--request-id", "e", "--reason", "wait", "--not-before", notBefore}, "exactly one of --job"},
		"entry with items":      {"hold", []string{"--entry", entry, "--request-id", "e", "--reason", "wait", "--until-items", "wi_0123456789abcdee"}, "does not accept --until-items"},
		"entry with generation": {"hold", []string{"--entry", entry, "--generation", "1", "--request-id", "e", "--reason", "wait"}, "does not accept --generation"},
		"not-before and items":  {"hold", []string{"--job", h.job.ID, "--generation", "1", "--request-id", "e", "--reason", "wait", "--not-before", notBefore, "--until-items", "wi_0123456789abcdee"}, "not both"},
		"bad time":              {"hold", []string{"--entry", entry, "--request-id", "e", "--reason", "wait", "--not-before", "2026-10-29 07:00"}, "RFC 3339"},
		"unhold not-before":     {"unhold", []string{"--entry", entry, "--request-id", "e", "--reason", "go", "--not-before", notBefore}, "does not accept --not-before"},
		"unhold neither":        {"unhold", []string{"--request-id", "e", "--reason", "go"}, "exactly one of --job"},
	} {
		if _, err := run(c.op, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(h.requests) != 0 {
		t.Fatalf("refused entry holds reached the hub: %v", h.requests)
	}
	out, err := run("hold", "--entry", entry, "--request-id", "entry-hold-1", "--reason", "Owner: not before the measurement ends", "--not-before", notBefore)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.actions) != 1 || len(h.posts) != 1 {
		t.Fatalf("requests %v", h.requests)
	}
	got := h.actions[0]
	if got.Operation != "hold" || got.EntryID != entry || got.JobID != "" || got.ExpectedGeneration != 0 || got.Hold == nil || got.Hold.NotBefore != notBefore || got.Hold.UntilItems != nil || got.Hold.Reason != "Owner: not before the measurement ends" {
		t.Fatalf("entry hold request %+v %+v", got, got.Hold)
	}
	var printed api.ReleaseJob
	if json.Unmarshal([]byte(out), &printed) != nil || printed.EntryID != entry || printed.Hold == nil || printed.Hold.NotBefore != notBefore {
		t.Fatal("output", out)
	}
	post := h.posts[0]
	if post.Envelope == nil || post.Envelope.Kind != api.EnvelopeKindNotice || post.Envelope.Subject != "A release is on hold before acceptance" || post.Text != api.RenderText(*post.Envelope) || len(api.ValidateEnvelope(*post.Envelope)) != 0 {
		t.Fatalf("notice %+v", post)
	}
	if post.Envelope.Refs["entry"] != entry || post.Envelope.Refs["item"] != h.job.ItemID || post.RequestID != entry+"-hold-entry-hold-1" {
		t.Fatalf("notice identity %+v %v", post, post.Envelope.Refs)
	}
	for _, want := range []string{"entry " + entry, "created held", "Reason: Owner: not before the measurement ends. Held by " + e.agent, "lifts it at " + notBefore} {
		if !strings.Contains(post.Envelope.Body.Text, want) {
			t.Fatalf("notice text lacks %q: %s", want, post.Envelope.Body.Text)
		}
	}
	if len(post.WorkItems) != 1 || post.WorkItems[0].ItemRevision != 5 || post.WorkOrderMessage == nil || post.WorkOrderMessage.Seq != 41 {
		t.Fatalf("notice links %+v %+v", post.WorkItems, post.WorkOrderMessage)
	}
	if _, err = run("unhold", "--entry", entry, "--request-id", "entry-unhold-1", "--reason", "Owner go"); err != nil {
		t.Fatal(err)
	}
	if got := h.actions[1]; got.Operation != "unhold" || got.EntryID != entry || got.JobID != "" || got.Hold == nil || got.Hold.NotBefore != "" {
		t.Fatalf("entry unhold %+v", got)
	}
	if post := h.posts[1]; post.Envelope.Subject != "An entry release hold was released" || post.RequestID != entry+"-unhold-entry-unhold-1" || !strings.Contains(post.Envelope.Body.Text, "created verified") {
		t.Fatalf("unhold notice %+v", post)
	}
	// A failed notice still reports the saved entry hold and the notice.
	h.failPost = true
	_, err = run("hold", "--entry", entry, "--request-id", "entry-hold-2", "--reason", "Again")
	if err == nil || !strings.Contains(err.Error(), "entry "+entry+" has an entry hold") || !strings.Contains(err.Error(), "NOTICE: A release is on hold before acceptance") {
		t.Fatal("failed entry notice", err)
	}
}

// h2: hold --job with --not-before sends the time and the notice names it.
func TestDeploymentHoldNotBeforeOnJob(t *testing.T) {
	h, e := newHoldHub(t)
	if _, err := captureRelayOutput(t, false, func() error {
		return cmdDeployment(e, []string{"hold", "--job", h.job.ID, "--generation", "1", "--request-id", "hold-dated", "--reason", "Not before the boundary", "--not-before", "2026-10-29T14:00:00Z"})
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.actions[0]; got.JobID != h.job.ID || got.EntryID != "" || got.Hold == nil || got.Hold.NotBefore != "2026-10-29T14:00:00Z" {
		t.Fatalf("dated hold request %+v", got)
	}
	if text := h.posts[0].Envelope.Body.Text; !strings.Contains(text, "The hub lifts the hold at 2026-10-29T14:00:00Z; tt deployment unhold releases it earlier.") {
		t.Fatal("notice", text)
	}
}

// a8: a hold whose notice cannot be posted stands, and the command says so
// with the notice to post.
func TestDeploymentHoldReportsSavedHoldWhenNoticeFails(t *testing.T) {
	h, e := newHoldHub(t)
	h.failPost = true
	out, err := captureRelayOutput(t, false, func() error {
		return cmdDeployment(e, []string{"hold", "--job", h.job.ID, "--generation", "1", "--request-id", "hold-1", "--reason", "Owner reads it first"})
	})
	if err == nil || len(h.actions) != 1 || len(h.posts) != 1 {
		t.Fatal("failed post was not reported", err, h.requests)
	}
	for _, want := range []string{"job " + h.job.ID + " is on hold at generation 2", "board notice was not posted", "board unavailable", "linked to item " + h.job.ItemID, "NOTICE: A release job is on hold", "Reason: Owner reads it first."} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q: %v", want, err)
		}
	}
	if !strings.Contains(out, `"state": "held"`) && !strings.Contains(out, `"state":"held"`) {
		t.Fatal("the saved job was not printed", out)
	}
}

// a8: unhold sends the typed request, then posts one typed notice.
func TestDeploymentUnholdSendsRequestThenNotice(t *testing.T) {
	h, e := newHoldHub(t)
	unhold := func(args ...string) (string, error) {
		return captureRelayOutput(t, false, func() error { return cmdDeployment(e, append([]string{"unhold"}, args...)) })
	}
	base := []string{"--job", h.job.ID, "--generation", "2", "--request-id", "unhold-1"}
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no reason": {base, "--reason"},
		"no job":    {[]string{"--generation", "2", "--request-id", "unhold-1", "--reason", "go"}, "--job"},
		"items":     {append(append([]string{}, base...), "--reason", "go", "--until-items", "wi_0123456789abcdee"), "does not accept --until-items"},
	} {
		if _, err := unhold(c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(h.requests) != 0 {
		t.Fatalf("refused unholds reached the hub: %v", h.requests)
	}
	out, err := unhold(append(append([]string{}, base...), "--reason", "Owner go for the release")...)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.actions) != 1 || len(h.posts) != 1 {
		t.Fatalf("requests %v", h.requests)
	}
	got := h.actions[0]
	if got.Operation != "unhold" || got.RequestID != "unhold-1" || got.AgentID != e.agent || got.RunID != e.runID || got.JobID != h.job.ID || got.ExpectedGeneration != 2 || got.Hold == nil || got.Hold.Reason != "Owner go for the release" || got.Hold.UntilItems != nil {
		t.Fatalf("unhold request %+v %+v", got, got.Hold)
	}
	if !strings.Contains(out, "verified") {
		t.Fatal("output", out)
	}
	post := h.posts[0]
	if post.Envelope == nil || post.Envelope.Kind != api.EnvelopeKindNotice || post.Envelope.Subject != "A release job hold was released" || post.Text != api.RenderText(*post.Envelope) || len(api.ValidateEnvelope(*post.Envelope)) != 0 {
		t.Fatalf("notice %+v", post)
	}
	if post.Envelope.Refs["release-job"] != h.job.ID || post.RequestID != h.job.ID+"-unhold-g3" || !strings.Contains(post.Envelope.Body.Text, "Reason: Owner go for the release") || !strings.Contains(post.Envelope.Body.Text, "original queue position") || len(post.WorkItems) != 1 || post.WorkItems[0].ItemRevision != 5 {
		t.Fatalf("notice %+v %v", post, post.Envelope.Refs)
	}
	h.failPost = true
	if _, err = unhold("--job", h.job.ID, "--generation", "4", "--request-id", "unhold-2", "--reason", "Owner go again"); err == nil || !strings.Contains(err.Error(), "is released from its hold at generation 5") || !strings.Contains(err.Error(), "NOTICE: A release job hold was released") {
		t.Fatal("failed unhold notice", err)
	}
}

// wi_95b575f0454c7052 a3: tt deployment progress reports, withdraws and reads
// through operation progress, with the caller's exact agent and run.
func TestDeploymentProgress(t *testing.T) {
	task, job := "tsk_0123456789abcdef", "rel_0123456789abcdef"
	stored := api.ReleaseProgress{AgentID: "agt_0123456789abcdef", RunID: "run_0123456789abcdef", ReportedAt: "2026-10-13T18:00:00Z", JobID: job, Phase: "merged", Wait: api.ReleaseProgressWait{Kind: "matrix", PID: 7}}
	var requests []api.ReleaseRequest
	var bodies []string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/v1/tasks/"+task+"/releases/actions") {
			t.Error("unexpected request", r.Method, r.URL.String())
		}
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		var req api.ReleaseRequest
		_ = json.Unmarshal(raw, &req)
		requests = append(requests, req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stored)
	}))
	defer hub.Close()
	e := env{hub: hub.URL, task: task, agent: stored.AgentID, runID: stored.RunID}
	run := func(args ...string) (string, error) {
		return captureCLIOutput(t, func() error { return cmdDeployment(e, append([]string{"progress"}, args...)) })
	}
	last := func() api.ReleaseRequest { return requests[len(requests)-1] }
	identified := func(at string) {
		t.Helper()
		if r := last(); r.Operation != "progress" || r.AgentID != e.agent || r.RunID != e.runID {
			t.Fatalf("%s: %+v", at, r)
		}
	}

	if _, err := run("--phase", "merged", "--job", job, "--wait", "matrix", "--pid", "7"); err != nil {
		t.Fatal(err)
	}
	identified("report")
	if r := last(); r.RequestID != "" || r.Progress == nil || *r.Progress != (api.ReleaseProgress{JobID: job, Phase: "merged", Wait: api.ReleaseProgressWait{Kind: "matrix", PID: 7}}) {
		t.Fatalf("report: %+v %+v", r, r.Progress)
	}
	if _, err := run("--phase", "claimed", "--step", "prerequisites"); err != nil {
		t.Fatal(err)
	}
	if r := last(); r.Progress == nil || *r.Progress != (api.ReleaseProgress{Phase: "claimed", Step: "prerequisites"}) {
		t.Fatalf("report with a step: %+v", r.Progress)
	}

	if _, err := run("--withdraw"); err != nil {
		t.Fatal(err)
	}
	identified("withdraw")
	if r := last(); r.Progress != nil || r.RequestID != "progress-withdraw-"+e.runID || strings.Contains(bodies[len(bodies)-1], `"progress":`) {
		t.Fatalf("withdraw: %+v %s", r, bodies[len(bodies)-1])
	}
	// The form a tt from before this command sends: its generic path forwards
	// the operation, agent, run and request id. It withdraws here too.
	if _, err := run("--request-id", "progress-withdraw-old"); err != nil {
		t.Fatal(err)
	}
	identified("old form")
	if r := last(); r.Progress != nil || r.RequestID != "progress-withdraw-old" {
		t.Fatalf("old form: %+v", r)
	}

	output, err := run()
	if err != nil {
		t.Fatal(err)
	}
	identified("read")
	if r := last(); r.Progress != nil || r.RequestID != "" {
		t.Fatalf("read: %+v", r)
	}
	for _, part := range []string{`"agentId": "` + stored.AgentID + `"`, `"runId": "` + stored.RunID + `"`, `"reportedAt": "2026-10-13T18:00:00Z"`, `"phase": "merged"`, `"kind": "matrix"`, `"pid": 7`} {
		if !strings.Contains(output, part) {
			t.Fatalf("read output lacks %s: %s", part, output)
		}
	}

	sent := len(requests)
	for name, args := range map[string][]string{"both forms": {"--phase", "poll", "--withdraw"}, "wait without a phase": {"--wait", "matrix"}, "job without a phase": {"--job", job}} {
		if _, err := run(args...); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	anonymous := env{hub: hub.URL, task: task}
	for name, args := range map[string][]string{"report": {"--phase", "poll"}, "withdraw": {"--withdraw"}} {
		if _, err := captureCLIOutput(t, func() error { return cmdDeployment(anonymous, append([]string{"progress"}, args...)) }); err == nil || !strings.Contains(err.Error(), "agent/run") {
			t.Fatalf("%s without an agent run: %v", name, err)
		}
	}
	if len(requests) != sent {
		t.Fatalf("refused calls reached the hub: %d", len(requests)-sent)
	}
	// A read needs no agent identity: the handler and the owner helper read it.
	if _, err := captureCLIOutput(t, func() error { return cmdDeployment(anonymous, []string{"progress"}) }); err != nil || last().AgentID != "" {
		t.Fatal(err, last())
	}
}

// wi_63c71acaf4f4f7b5 s1: --code-* flags put exactly progress.code on a
// report, and are refused before any hub call without a code state, with a
// withdraw or without a phase.
func TestDeploymentProgressCode(t *testing.T) {
	task := "tsk_0123456789abcdef"
	var bodies []string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ReleaseProgress{})
	}))
	defer hub.Close()
	e := env{hub: hub.URL, task: task, agent: "agt_0123456789abcdef", runID: "run_0123456789abcdef"}
	run := func(args ...string) error {
		_, err := captureCLIOutput(t, func() error { return cmdDeployment(e, append([]string{"progress"}, args...)) })
		return err
	}
	if err := run("--phase", "poll", "--code-state", "refused", "--code-reason", "checkout-dirty", "--code-loaded", "0123456789ab", "--code-published", "ba9876543210", "--code-since", "2026-10-27T18:00:00.000Z"); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Progress map[string]json.RawMessage `json:"progress"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &req); err != nil {
		t.Fatal(err)
	}
	if got := string(req.Progress["code"]); got != `{"state":"refused","reason":"checkout-dirty","loaded":"0123456789ab","published":"ba9876543210","since":"2026-10-27T18:00:00.000Z"}` {
		t.Fatalf("code sent: %s", got)
	}
	if err := run("--phase", "poll", "--code-state", "refused", "--code-reason", "load-failed", "--code-checkout", "not-restored", "--code-since", "2026-10-27T18:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodies[1], `"code":{"state":"refused","reason":"load-failed","checkout":"not-restored","since":"2026-10-27T18:00:00Z"}`) {
		t.Fatalf("load-failed code sent: %s", bodies[1])
	}
	// No --code-* flag: no code on the report.
	if err := run("--phase", "poll"); err != nil || strings.Contains(bodies[2], `"code"`) {
		t.Fatal(err, bodies[2])
	}
	sent := len(bodies)
	for name, args := range map[string][]string{
		"code without a state":   {"--phase", "poll", "--code-reason", "checkout-dirty", "--code-since", "2026-10-27T18:00:00Z"},
		"since without a state":  {"--phase", "poll", "--code-since", "2026-10-27T18:00:00Z"},
		"code with a withdraw":   {"--withdraw", "--code-state", "current", "--code-since", "2026-10-27T18:00:00Z"},
		"code without a phase":   {"--code-state", "current", "--code-since", "2026-10-27T18:00:00Z"},
		"code with a request id": {"--request-id", "progress-withdraw-old", "--code-state", "current"},
	} {
		if err := run(args...); err == nil || !strings.Contains(err.Error(), "--code-") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(bodies) != sent {
		t.Fatalf("refused calls reached the hub: %d", len(bodies)-sent)
	}
}

// wi_63c71acaf4f4f7b5 s2: tt deployment list decodes and prints the page's
// deployer with its code and action, in both views, and prints none when the
// page has none.
func TestDeploymentListShowsDeployerCode(t *testing.T) {
	task := "tsk_0123456789abcdef"
	deployer := &api.ReleaseProgress{AgentID: "agt_0123456789abcdef", RunID: "run_0123456789abcdef", ReportedAt: "2026-10-27T18:00:30Z", Phase: "poll",
		Code: &api.ReleaseProgressCode{State: "refused", Reason: "checkout-dirty", Loaded: "0123456789ab", Published: "ba9876543210", Since: "2026-10-27T18:00:00.000Z", Action: "Run tt deployment setup for the deployer with its current run as predecessor (docs/project-deployment.md)."}}
	with := true
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		page := api.ReleasePage{Version: 1, Jobs: []api.ReleaseSummary{}, Page: api.ReleasePageInfo{View: r.URL.Query().Get("view"), Limit: 50, Snapshot: strings.Repeat("a", 64)}}
		if with {
			page.Deployer = deployer
		}
		json.NewEncoder(w).Encode(page)
	}))
	defer hub.Close()
	e := env{hub: hub.URL, task: task, token: "fixture"}
	for _, view := range []string{"active", "settled"} {
		output, err := captureCLIOutput(t, func() error { return cmdDeployment(e, []string{"list", "--view", view}) })
		if err != nil {
			t.Fatal(view, err)
		}
		var page api.ReleasePage
		if err := json.Unmarshal([]byte(output), &page); err != nil || page.Deployer == nil || page.Deployer.Code == nil || *page.Deployer.Code != *deployer.Code || page.Deployer.AgentID != deployer.AgentID || page.Deployer.RunID != deployer.RunID || page.Deployer.ReportedAt != deployer.ReportedAt {
			t.Fatalf("%s: %v %s", view, err, output)
		}
		for _, part := range []string{`"deployer": {`, `"state": "refused"`, `"reason": "checkout-dirty"`, `"loaded": "0123456789ab"`, `"published": "ba9876543210"`, `"since": "2026-10-27T18:00:00.000Z"`, `"action": "Run tt deployment setup`} {
			if !strings.Contains(output, part) {
				t.Fatalf("%s output lacks %s: %s", view, part, output)
			}
		}
	}
	with = false
	output, err := captureCLIOutput(t, func() error { return cmdDeployment(e, []string{"list"}) })
	if err != nil || strings.Contains(output, `"deployer"`) {
		t.Fatal(output, err)
	}
}

// TestDeploymentServeHelperProcess is the disposable tt process the serve
// test signals: re-exec'd with TT_SERVE_HELPER_DIR set, it serves the fake
// runner in that checkout and exits as die would. Otherwise it does nothing.
func TestDeploymentServeHelperProcess(t *testing.T) {
	checkout := os.Getenv("TT_SERVE_HELPER_DIR")
	if checkout == "" {
		return
	}
	if err := serveRunner(checkout, "cfg.json"); err != nil {
		die(err)
	}
	os.Exit(0)
}

// A SIGTERM or SIGINT to tt deployment serve reaches the runner, and serve
// exits with the runner's status (wi_3e40f1e689ecf44f). Only the re-exec'd
// helper and its fake runner child are signalled, never this test process.
func TestDeploymentServeForwardsStopSignals(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable")
	}
	// The fake runner writes its pid once both handlers are installed. SIGTERM
	// exits 43; SIGINT re-raises itself, so the runner dies by the signal.
	runner := `import {writeFileSync} from "node:fs";
if (process.argv.at(-2) !== "--config" || process.argv.at(-1) !== "cfg.json") process.exit(9);
process.on("SIGTERM", () => { writeFileSync("marker", "TERM"); process.exit(43); });
process.on("SIGINT", () => { writeFileSync("marker", "INT"); process.removeAllListeners("SIGINT"); process.kill(process.pid, "SIGINT"); });
writeFileSync("ready", String(process.pid));
if (process.env.FAKE_RUNNER_CLEAN) process.exit(0);
setTimeout(() => process.exit(7), 10000);
`
	for _, c := range []struct {
		name   string
		sig    os.Signal
		marker string
		code   int
	}{{"term", syscall.SIGTERM, "TERM", 43}, {"int", syscall.SIGINT, "INT", 130}, {"clean", nil, "", 0}} {
		t.Run(c.name, func(t *testing.T) {
			checkout, home := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Join(checkout, "scripts"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(checkout, "scripts", "release-runner.mjs"), []byte(runner), 0o644); err != nil {
				t.Fatal(err)
			}
			helper := exec.Command(os.Args[0], "-test.run=^TestDeploymentServeHelperProcess$", "-test.count=1")
			helper.Env = append(os.Environ(), "TT_SERVE_HELPER_DIR="+checkout, "HOME="+home)
			if c.sig == nil {
				helper.Env = append(helper.Env, "FAKE_RUNNER_CLEAN=1")
			}
			var out bytes.Buffer
			helper.Stdout, helper.Stderr = &out, &out
			if err := helper.Start(); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- helper.Wait() }()
			childPID, reaped := 0, false
			t.Cleanup(func() {
				if reaped {
					return
				}
				_ = helper.Process.Kill()
				if childPID > 0 {
					_ = syscall.Kill(childPID, syscall.SIGKILL)
				}
				<-result
			})
			deadline := time.Now().Add(5 * time.Second)
			for {
				if b, err := os.ReadFile(filepath.Join(checkout, "ready")); err == nil && len(b) > 0 {
					if childPID, err = strconv.Atoi(string(b)); err != nil {
						t.Fatalf("ready %q", b)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the fake runner never became ready; helper output:\n%s", out.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			if c.sig != nil {
				if err := helper.Process.Signal(c.sig); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			select {
			case err = <-result:
				reaped = true
			case <-time.After(15 * time.Second):
				t.Fatalf("serve did not exit; helper output:\n%s", out.String())
			}
			code := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			marker, _ := os.ReadFile(filepath.Join(checkout, "marker"))
			if code != c.code || string(marker) != c.marker {
				t.Fatalf("serve exited %d with runner marker %q, want %d and %q; helper output:\n%s", code, marker, c.code, c.marker, out.String())
			}
		})
	}
}

// wi_816228d539a80c48 a1/a2: tt deployment block sends the runner's revert
// flags as the request's revert, a block without them carries no revert key
// (the bare block of an older runner), and a revert flag on any other
// operation is refused before a request is sent.
func TestDeploymentBlockSendsRevert(t *testing.T) {
	var bodies [][]byte
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ReleaseJob{ID: "rel_fixture", State: "blocked", Generation: 4})
	}))
	defer hub.Close()
	e := env{hub: hub.URL, task: "tsk_0123456789abcdef", agent: "agt_0123456789abcdef", runID: "run_0123456789abcdef"}
	commit := strings.Repeat("c", 40)
	block := func(name string, flags ...string) map[string]any {
		t.Helper()
		before := len(bodies)
		args := append([]string{"block", "--job", "rel_fixture", "--generation", "3", "--request-id", "block-" + name}, flags...)
		if _, err := captureRelayOutput(t, false, func() error { return cmdDeployment(e, args) }); err != nil || len(bodies) != before+1 {
			t.Fatalf("%s: %v %d", name, err, len(bodies))
		}
		var wire map[string]any
		if err := json.Unmarshal(bodies[len(bodies)-1], &wire); err != nil {
			t.Fatal(err)
		}
		if wire["operation"] != "block" || wire["jobId"] != "rel_fixture" || wire["expectedGeneration"] != float64(3) || wire["requestId"] != "block-"+name {
			t.Fatalf("%s: %v", name, wire)
		}
		return wire
	}
	committed := block("committed", "--revert-outcome", "committed", "--revert-commit", commit, "--revert-bug-request", "rel_fixture-revert-bug")
	if want := map[string]any{"outcome": "committed", "commit": commit, "bugRequestId": "rel_fixture-revert-bug"}; !reflect.DeepEqual(committed["revert"], want) {
		t.Fatalf("committed revert %v", committed["revert"])
	}
	if failed := block("failed", "--revert-outcome", "failed"); !reflect.DeepEqual(failed["revert"], map[string]any{"outcome": "failed"}) {
		t.Fatalf("failed revert %v", failed["revert"])
	}
	if bare := block("bare"); bare["revert"] != nil {
		t.Fatalf("bare block carries a revert: %v", bare)
	}
	if _, ok := block("bare-key")["revert"]; ok {
		t.Fatal("bare block sends a revert key")
	}
	before := len(bodies)
	for _, op := range []string{"refuse", "finish", "merged", "claim"} {
		err := cmdDeployment(e, []string{op, "--job", "rel_fixture", "--generation", "3", "--request-id", "other-" + op, "--revert-outcome", "committed", "--revert-commit", commit})
		if err == nil || !strings.Contains(err.Error(), "block flags") {
			t.Fatalf("%s with a revert: %v", op, err)
		}
	}
	if len(bodies) != before {
		t.Fatalf("a revert on another operation reached the hub: %d", len(bodies)-before)
	}
}

// The usage line lists every operation the release runner calls.
func TestDeploymentUsageListsRunnerOperations(t *testing.T) {
	err := cmdDeployment(env{}, nil)
	for _, op := range []string{"|unclaim|", "|progress|", "|batch-open|batch-add|batch-drop|"} {
		if err == nil || !strings.Contains(err.Error(), op) {
			t.Fatalf("usage does not list %s: %v", op, err)
		}
	}
}
