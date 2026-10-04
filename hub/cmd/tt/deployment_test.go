package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/scs32/tailterm/hub/internal/api"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
		if err := supersedeCoverage(dir, job("rel_plain"), picked); err != nil {
			t.Fatal(err)
		}
	})
	// Cherry-picked onto a moved tasks-hub: ancestry fails, all patches present.
	t.Run("cherry-picked release accepted", func(t *testing.T) {
		if exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", c2, picked).Run() == nil {
			t.Fatal("fixture: candidate must not be an ancestor of the hand release")
		}
		if err := supersedeCoverage(dir, job("rel_picked"), picked); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing patch refused and named", func(t *testing.T) {
		if err := supersedeCoverage(dir, job("rel_picked"), partial); err == nil || !strings.Contains(err.Error(), c2) || strings.Contains(err.Error(), c1) {
			t.Fatal(err)
		}
	})
	t.Run("released commit not on tasks-hub refused", func(t *testing.T) {
		if err := supersedeCoverage(dir, job("rel_picked"), c2); err == nil || !strings.Contains(err.Error(), "refs/heads/tasks-hub") {
			t.Fatal(err)
		}
	})
	// A merge in the range needs ancestry: git cherry skips merges.
	t.Run("merge in range without ancestry refused", func(t *testing.T) {
		if err := supersedeCoverage(dir, job("rel_merge"), pickedMerge); err == nil || !strings.Contains(err.Error(), "merge") {
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
		if err := supersedeCoverage(noRepo, job("rel_picked"), bad); err == nil || !strings.Contains(err.Error(), "40-hex") {
			t.Fatalf("%q: %v", bad, err)
		}
		if err := supersedeCoverage(noRepo, api.ReleaseJob{ID: "rel_bad", BaseCommit: bad, Commit: c2}, picked); err == nil || !strings.Contains(err.Error(), "full commit") {
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
