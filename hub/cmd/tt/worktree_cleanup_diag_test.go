package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// Diagnosis tests for wi_970c5207b8e8d1de (order #42078): they assert the
// worktree cleanup's behaviour at tasks-hub f2043adc for claims c2, c4 and
// c5, so a later fix changes them on purpose; wi_c3dc2bb931d90710 (order
// #42633) changed the c2 rows for $HOME and the symlinked root. The findings
// are written up in docs/incidents/worktree-sweep-kept-970c/report.md. Every
// fixture is a temp tree, a fake temp and artifacts root and a fake hub; none
// applies to a host path.

// c2: a receipt in the item folder cites a file inside a released item's
// verifier checkout, in one spelling per case. The absolute, "~", $HOME and
// ${HOME} spellings keep the checkout, as do both the resolved and the
// unresolved spelling of a symlinked artifacts root; an item-folder-relative
// path does not (F2-3, out of scope).
func TestWorktreeCleanupDiagCitationSpellings(t *testing.T) {
	type spellingCase struct {
		name string
		// symlinkRoot makes <repo>-artifacts a symlink to the real tree.
		symlinkRoot bool
		cite        func(r cleanupRepo, checkout string) string
		action      string
		reason      string
	}
	cases := []spellingCase{
		{"absolute (control)", false, func(r cleanupRepo, checkout string) string {
			return checkout + "/test-results/out.txt"
		}, "kept", keepEvidence},
		{"tilde (control)", false, func(r cleanupRepo, checkout string) string {
			return "~" + strings.TrimPrefix(checkout, r.root) + "/test-results/out.txt"
		}, "kept", keepEvidence},
		{"item-folder-relative", false, func(r cleanupRepo, checkout string) string {
			return filepath.Base(checkout) + "/test-results/out.txt"
		}, "would-remove", ""},
		{"literal $HOME", false, func(r cleanupRepo, checkout string) string {
			return "$HOME" + strings.TrimPrefix(checkout, r.root) + "/test-results/out.txt"
		}, "kept", keepEvidence},
		{"literal ${HOME}", false, func(r cleanupRepo, checkout string) string {
			return "${HOME}" + strings.TrimPrefix(checkout, r.root) + "/test-results/out.txt"
		}, "kept", keepEvidence},
		{"unresolved symlinked artifacts root", true, func(r cleanupRepo, checkout string) string {
			return filepath.Join(r.artifacts(), artifactItemA, filepath.Base(checkout)) + "/test-results/out.txt"
		}, "kept", keepEvidence},
		{"resolved symlinked artifacts root", true, func(r cleanupRepo, checkout string) string {
			return checkout + "/test-results/out.txt"
		}, "kept", keepEvidence},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newCleanupRepo(t)
			r.ignoreBuildOutput(t)
			// The fixture root stands in for the home directory.
			t.Setenv("HOME", r.root)
			if tc.symlinkRoot {
				real := filepath.Join(r.root, "artifacts-real")
				if err := os.MkdirAll(real, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, r.artifacts()); err != nil {
					t.Fatal(err)
				}
			}
			checkout := canonicalPath(r.verifierCheckout(t, artifactItemA, "verifier-abc1234"))
			if tc.symlinkRoot && strings.HasPrefix(checkout, r.artifacts()+"/") {
				t.Fatalf("checkout %s is not under the resolved root", checkout)
			}
			writeFixtureFile(t, filepath.Join(checkout, "test-results", "out.txt"), "kept output\n")
			cite := tc.cite(r, checkout)
			writeFixtureFile(t, filepath.Join(filepath.Dir(checkout), "receipt.json"), `{"output":"`+cite+`"}`)
			if _, err := os.Stat(filepath.Join(filepath.Dir(checkout), cite)); tc.name == "item-folder-relative" && err != nil {
				t.Fatalf("the relative citation does not name the file from the item folder: %v", err)
			}

			got := r.artifactSweep(t, worktreeCleanupInputs{Items: artifactItemStates([]api.TeamQueueEntry{releasedArtifactEntry(artifactItemA)}, nil)})
			d := requireDecision(t, got, checkout, tc.action, tc.reason)
			if tc.reason == keepEvidence && !strings.Contains(d.Detail, "receipt.json: test-results/out.txt") {
				t.Fatalf("detail %q does not name the receipt and the cited file", d.Detail)
			}
			if tc.reason == "" && d.Detail != "item "+artifactItemA+" released" {
				t.Fatalf("detail %q, want the release basis", d.Detail)
			}
			requireExists(t, filepath.Join(checkout, "test-results", "out.txt"), true)
		})
	}
	// The spellings the rule produces: the path, its /private-less form, a
	// main-checkout-relative form and the "~", $HOME and ${HOME} forms; none
	// is relative to the item folder. The unresolved root spelling is tried
	// by artifactPass.cited, not added here.
	t.Setenv("HOME", "/Users/fixture")
	got := worktreeEvidencePatterns("/Users/fixture/repo-artifacts/"+artifactItemA+"/verifier-abc1234", "/Users/fixture/repo")
	rest := "/repo-artifacts/" + artifactItemA + "/verifier-abc1234"
	want := []string{"/Users/fixture" + rest, "~" + rest, "$HOME" + rest, "${HOME}" + rest}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("patterns %q, want %q", got, want)
	}
}

// wi_e904b9cac59b1873 (order #43115): a tracked docs/ file on tasks-hub cites
// a released item's verifier checkout under a symlinked artifacts root, in
// one spelling per case. A file inside the checkout keeps it through either
// the configured (unresolved) or the resolved spelling of the root; the bare
// configured-spelling directory, like no citation at all, does not.
func TestWorktreeCleanupDiagDocsConfiguredRootSpelling(t *testing.T) {
	type docsCase struct {
		name string
		// cite returns the doc's text, or "" for no doc.
		cite   func(r cleanupRepo, checkout string) string
		action string
		reason string
	}
	configured := func(r cleanupRepo, checkout string) string {
		return filepath.Join(r.artifacts(), artifactItemA, filepath.Base(checkout))
	}
	cases := []docsCase{
		{"configured spelling", func(r cleanupRepo, checkout string) string {
			return "Evidence: `" + configured(r, checkout) + "/test-results/out.txt`.\n"
		}, "kept", keepEvidence},
		{"resolved spelling", func(r cleanupRepo, checkout string) string {
			return "Evidence: `" + checkout + "/test-results/out.txt`.\n"
		}, "kept", keepEvidence},
		{"configured bare directory", func(r cleanupRepo, checkout string) string {
			return "The matrix ran in `" + configured(r, checkout) + "`.\n"
		}, "would-remove", ""},
		{"uncited", func(r cleanupRepo, checkout string) string {
			return ""
		}, "would-remove", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newCleanupRepo(t)
			r.ignoreBuildOutput(t)
			real := filepath.Join(r.root, "artifacts-real")
			if err := os.MkdirAll(real, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, r.artifacts()); err != nil {
				t.Fatal(err)
			}
			checkout := canonicalPath(r.verifierCheckout(t, artifactItemA, "verifier-abc1234"))
			if strings.HasPrefix(checkout, r.artifacts()+"/") {
				t.Fatalf("checkout %s is not under the resolved root", checkout)
			}
			writeFixtureFile(t, filepath.Join(checkout, "test-results", "out.txt"), "kept output\n")
			if text := tc.cite(r, checkout); text != "" {
				writeFixtureFile(t, filepath.Join(r.main, "docs", "release.md"), text)
				cleanupGit(t, r.main, "add", "docs/release.md")
				cleanupGit(t, r.main, "commit", "-q", "-m", "cite a checkout")
			}

			got := r.artifactSweep(t, worktreeCleanupInputs{Items: artifactItemStates([]api.TeamQueueEntry{releasedArtifactEntry(artifactItemA)}, nil)})
			d := requireDecision(t, got, checkout, tc.action, tc.reason)
			if tc.reason == keepEvidence && d.Detail != "cited by docs/release.md: test-results/out.txt" {
				t.Fatalf("detail %q does not name the doc and the cited file", d.Detail)
			}
			if tc.reason == "" && d.Detail != "item "+artifactItemA+" released" {
				t.Fatalf("detail %q, want the release basis", d.Detail)
			}
			requireExists(t, filepath.Join(checkout, "test-results", "out.txt"), true)
		})
	}
}

// diagCloseoutTick is one closeout runner tick's cost: the hub reads by
// kind and the Git calls.
type diagCloseoutTick struct {
	getTask, getEntry, history, git int
}

// c4: N finished, accepted entries whose queue worktrees are gone and whose
// verifier checkouts stay kept, at one level (<item>/verifier-x) and nested
// (<item>/verifier-x/checkout), for each keep reason. Each tick on the
// runner's clock is counted: the first, one a minute later (inside
// closeoutWorktreeInterval), one an interval after the first and one a
// minute after that. The tick's own reads (the host listing, the active view
// and one GetTask) are included in the counts.
func TestWorktreeCleanupDiagKeptEntryRecheckCost(t *testing.T) {
	const n = 3
	type layoutCase struct {
		name string
		dir  string
	}
	layouts := []layoutCase{{"one-level", "verifier-abc1234"}, {"nested", filepath.Join("verifier-abc1234", "checkout")}}
	reasons := []string{keepRetention, keepEvidence, keepDirty}
	// Measured at f2043adc. Per examined entry closeout costs one
	// GetTeamQueueEntry (history entries are summaries), one GetTask and a
	// fixed run of Git calls that depends on how far classification gets.
	full := map[string]diagCloseoutTick{
		keepRetention: {getTask: 1 + n, getEntry: n, history: 1, git: 4 * n},
		keepEvidence:  {getTask: 1 + n, getEntry: n, history: 1, git: 4 * n},
		keepDirty:     {getTask: 1 + n, getEntry: n, history: 1, git: 5 * n},
	}
	quiet := diagCloseoutTick{getTask: 1}
	items := []string{"wi_d1a90000000000a1", "wi_d1a90000000000a2", "wi_d1a90000000000a3"}
	for _, layout := range layouts {
		for _, reason := range reasons {
			t.Run(layout.name+" "+reason, func(t *testing.T) {
				r := newCleanupRepo(t)
				r.ignoreBuildOutput(t)
				t.Setenv("HOME", filepath.Join(r.root, "home"))
				if reason == keepRetention {
					t.Setenv("TAILTERM_ARTIFACT_ACCEPTED_AFTER", "24h")
				}
				const host = "fixture"
				const task = "tsk_d1a9d1a9d1a9d1a9"
				head := cleanupGit(t, r.main, "rev-parse", "HEAD")
				acceptedAt := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
				var entries []api.TeamQueueEntry
				var checkouts []string
				for i, item := range items[:n] {
					checkout := r.verifierCheckout(t, item, layout.dir)
					switch reason {
					case keepEvidence:
						writeFixtureFile(t, filepath.Join(checkout, "test-results", "out.txt"), "kept output\n")
						writeFixtureFile(t, filepath.Join(r.artifacts(), item, "receipt.json"), `{"output":"`+checkout+`/test-results/out.txt"}`)
					case keepDirty:
						writeFixtureFile(t, filepath.Join(checkout, "notes.txt"), "untracked\n")
					}
					// The pre-c3b7ce55 existence check looked one level down.
					if one := detachedCheckoutWithin(filepath.Join(r.artifacts(), item), 1); one != (layout.name == "one-level") {
						t.Fatalf("one-level check found %s: %v", checkout, one)
					}
					cwd := r.queuePath("queue-gone-" + item[len(item)-2:])
					entries = append(entries, api.TeamQueueEntry{ID: "tqe_diag_kept_" + item[len(item)-2:], TaskID: task, ItemID: item, State: "finished", Host: host, Cwd: cwd, Repository: r.common, Position: int64(i + 1),
						Acceptance: &api.TeamIntegrationAcceptance{Repository: r.common, Worktree: cwd, Commit: head, AcceptedAt: acceptedAt, Evidence: "verification receipt vr_diag"}})
					checkouts = append(checkouts, checkout)
				}
				// Closeout looks at a project's history while it has an
				// active entry; the paused project never advances it.
				runningCwd := r.queuePath("queue-diag-running")
				cleanupGit(t, r.main, "worktree", "add", "-q", "--detach", runningCwd, "tasks-hub")
				running := api.TeamQueueEntry{ID: "tqe_diag_running", TaskID: task, ItemID: "wi_d1a90000000000ff", State: "running", Host: host, Cwd: runningCwd}
				hub := &cleanupHub{
					byHost: api.TeamQueueList{Entries: []api.TeamQueueEntry{running}},
					queues: map[string]api.TeamQueueList{task: {Entries: append(append([]api.TeamQueueEntry(nil), entries...), running)}},
					details: map[string]api.TaskDetail{task: {Task: api.Task{ID: task, Status: api.TaskOpen, PauseState: api.ProjectPausePaused}, Agents: []api.Agent{
						{ID: "agt_diag_running", Host: host, Status: "running", Cwd: runningCwd, WorkItem: &api.AgentWorkItemBinding{ItemTaskID: task, ItemID: running.ItemID}},
					}}},
				}
				server := httptest.NewServer(hub)
				defer server.Close()
				c, err := api.NewClient(server.URL, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				calls := countWorktreeGit(t)
				t0 := time.Now()
				now := t0
				runner := teamRunner{worktrees: closeoutWorktrees, now: func() time.Time { return now }}
				tick := func(at time.Time) diagCloseoutTick {
					t.Helper()
					now = at
					before := len(hub.queryLog())
					calls.Store(0)
					var stderr strings.Builder
					restore := captureStderr(t, &stderr)
					err := runner.tick(context.Background(), env{hub: server.URL}, c, host)
					restore()
					if err != nil {
						t.Fatal(err)
					}
					var got diagCloseoutTick
					for _, l := range hub.queryLog()[before:] {
						switch {
						case l == "GET /v1/tasks/"+task:
							got.getTask++
						case strings.HasPrefix(l, "GET /v1/tasks/"+task+"/team-queue/"):
							got.getEntry++
						}
					}
					got.history = historyReads(hub.queryLog()[before:], task)
					got.git = int(calls.Load())
					return got
				}
				for _, step := range []struct {
					name string
					at   time.Time
					want diagCloseoutTick
				}{
					{"first", t0, full[reason]},
					{"inside the interval", t0.Add(time.Minute), quiet},
					{"one interval on", t0.Add(closeoutWorktreeInterval), full[reason]},
					{"inside the next interval", t0.Add(closeoutWorktreeInterval + time.Minute), quiet},
				} {
					if got := tick(step.at); got != step.want {
						t.Fatalf("%s tick: %+v, want %+v", step.name, got, step.want)
					}
				}
				for _, checkout := range checkouts {
					requireExists(t, checkout, true)
				}
				// The kept outcome is recorded once per process, not once per
				// look.
				kept := 0
				for _, rec := range readCleanupReceipts(t) {
					if rec.Action == "kept" && rec.Reason == reason {
						kept++
					}
				}
				if kept != n {
					t.Fatalf("%d kept %s receipts, want %d", kept, reason, n)
				}
				if _, writes := hub.snapshot(); len(writes) != 0 {
					t.Fatalf("closeout wrote to the hub: %v", writes)
				}
			})
		}
	}
}

// c5: the nested repository scan runs before the permission walk, both in a
// dry run's classification and in the removal re-check, so a directory with
// no owner read or execute fails the scan and keeps the worktree as
// remove-failed, whether or not a repository lies below it. Its permissions
// are untouched. The permission walk run first would make the directory
// readable, find no repository in one case and the repository in the
// other, and its restore puts the mode back.
func TestWorktreeCleanupDiagUnreadableDirectoryBeforeNestedScan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 directory")
	}
	type unreadableCase struct {
		name   string
		nested bool
	}
	for _, tc := range []unreadableCase{{"no nested repository", false}, {"nested repository below", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCleanupRepo(t)
			w := r.branchWorktree(t, r.queuePath("queue-d1a90005"), "bug/unreadable")
			cleanupGit(t, r.main, "merge", "-q", "--ff-only", "bug/unreadable")
			// .build/ is ignored, so git status never opens it.
			locked := filepath.Join(w, ".build", "cache", "locked")
			writeFixtureFile(t, filepath.Join(locked, "entry"), "cache\n")
			clone := filepath.Join(locked, "clone")
			if tc.nested {
				if err := os.MkdirAll(clone, 0755); err != nil {
					t.Fatal(err)
				}
				cleanupGit(t, clone, "init", "-q", "-b", "main")
			}
			if err := os.Chmod(locked, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0755) })
			requireMode := func(when string) {
				t.Helper()
				info, err := os.Lstat(locked)
				if err != nil || info.Mode().Perm() != 0 {
					t.Fatalf("%s: mode %v %v, want 0", when, info.Mode(), err)
				}
			}
			wantDetail := "scan for nested repositories: open " + locked + ": permission denied"

			for _, apply := range []bool{false, true} {
				got := r.sweep(t, apply, nil, nil)
				d := requireDecision(t, got, w, "kept", keepFailed)
				if d.Detail != wantDetail {
					t.Fatalf("apply=%v detail %q, want %q", apply, d.Detail, wantDetail)
				}
				requireExists(t, w, true)
				requireMode("after the pass")
			}

			// The permission walk first: it opens the directory, the scan then
			// sees what is below it, and restore puts mode 000 back.
			restore, err := makeWorktreeWritable(w)
			if err != nil {
				t.Fatal(err)
			}
			nested, err := nestedRepository(w, nil)
			restore()
			if err != nil {
				t.Fatal(err)
			}
			if want := map[bool]string{false: "", true: clone}[tc.nested]; nested != want {
				t.Fatalf("scan after the walk found %q, want %q", nested, want)
			}
			requireMode("after restore")
		})
	}

	// A verifier checkout fails earlier when applying: its manifest's
	// usage walk reads the directory before the removal re-check runs.
	t.Run("artifact checkout", func(t *testing.T) {
		r := newCleanupRepo(t)
		r.ignoreBuildOutput(t)
		checkout := r.verifierCheckout(t, artifactItemA, "verifier-abc1234")
		locked := filepath.Join(checkout, "node_modules", "locked")
		writeFixtureFile(t, filepath.Join(locked, "index.js"), "x\n")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0755) })
		items := artifactItemStates([]api.TeamQueueEntry{releasedArtifactEntry(artifactItemA)}, nil)
		for _, step := range []struct {
			apply  bool
			detail string
		}{
			{false, "scan for nested repositories: open " + locked + ": permission denied"},
			{true, "manifest: open " + locked + ": permission denied"},
		} {
			got := r.artifactSweep(t, worktreeCleanupInputs{Items: items, Apply: step.apply})
			if d := requireDecision(t, got, checkout, "kept", keepFailed); d.Detail != step.detail {
				t.Fatalf("apply=%v detail %q, want %q", step.apply, d.Detail, step.detail)
			}
			requireExists(t, checkout, true)
			requireExists(t, checkout+".removed.json", false)
			if info, err := os.Lstat(locked); err != nil || info.Mode().Perm() != 0 {
				t.Fatalf("apply=%v mode %v %v, want 0", step.apply, info.Mode(), err)
			}
		}
	})
}
