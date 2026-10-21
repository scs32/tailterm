# Worktree sweep follow-ups: diagnosis of c2, c4 and c5

Bug `wi_970c5207b8e8d1de` revision 2, work order #42078 (lead ASSIGN #42092).
Claims carried from review #19468 f3 (c2), #16358 f1 (c4, with #41690 f3
folded in) and #16366 f5 (c5). Diagnosis only: no product code changed.

All line numbers are `hub/cmd/tt/worktree_cleanup.go` at tasks-hub `f2043adc`
unless another file is named. The tests are in
`hub/cmd/tt/worktree_cleanup_diag_test.go`. They assert today's behaviour, so
a later fix changes them on purpose. Every fixture is a temp Git repository
with a fake artifacts root, a fake temp root and a fake hub (`httptest`).
`HOME` is set only with `t.Setenv`. No test reads the live hub or runs a sweep
on a host path.

| Claim | Reproduces | Test | Direction | Recommendation |
| --- | --- | --- | --- | --- |
| c2: citation spellings | Yes, all three spellings | `TestWorktreeCleanupDiagCitationSpellings` | Unsafe: a cited file can be deleted | Small pattern fix (F2-1, F2-2); F2-3 optional |
| c4: re-check cost of kept entries | Yes, bounded: once per interval per entry; nested now costs the same as one-level | `TestWorktreeCleanupDiagKeptEntryRecheckCost` | Cost only | F4-1 (one GetTask per project); the others are optional |
| c5: nested scan before the permission walk | Yes | `TestWorktreeCleanupDiagUnreadableDirectoryBeforeNestedScan` | Safe: keeps, never deletes | Dismiss: no case in 20 days of receipts |

## c2: citation spellings

**Reproduces.** A released item's verifier checkout is removed even though a
receipt in the item folder cites a file inside it. This happens with each of
the three spellings:

| Spelling in `<item>/receipt.json` | Outcome |
| --- | --- |
| `<checkout>/test-results/out.txt` (absolute, control) | kept `evidence` |
| `~/…/<checkout>/test-results/out.txt` (control) | kept `evidence` |
| `verifier-abc1234/test-results/out.txt` (relative to the item folder) | would-remove |
| `$HOME/…/test-results/out.txt` | would-remove |
| `<repo>-artifacts/<item>/…/test-results/out.txt`, where `<repo>-artifacts` is a symlink | would-remove |

Why:

- `worktreeEvidencePatterns` (:643-662) produces four spellings: the path,
  the path without `/private`, the path relative to the main checkout, and
  the `~` form. A verifier checkout sits in the sibling `<repo>-artifacts`
  tree, outside the main checkout, so the main-relative form never applies.
  The test pins the list for an artifacts path: the absolute and `~`
  spellings only.
- `artifactCitation` (:1506-1532) matches only `pattern + "/"`. It does not
  resolve a relative path against the folder of the file that holds it.
  `cited` (:1595) passes it only the text, not `ev.Source`.
- Worktree paths are canonicalised (`canonicalPath`, :470, applied at
  :859), so the spelling through a symlinked root is never a pattern.

**Live exposure: none found.** This was a read-only check on the Mini on
2026-10-21. `/Users/stephenspeicher/projects/tailterm-artifacts` and its
parent are real directories, not symlinks. I scanned 4,355 receipt and plan
JSON files under the artifacts root, skipping checkouts. None held a
`verifier-<sha>/…` path without a leading `/`. Three held `$HOME`, all
in handler folders (`db-handler-o6/…`, `db-handler-r13/…`). `itemTexts`
(:1538) never reads those folders: it reads only `<root>/<wi_…>/`.

**Fix options** (a later bounded bug order). Owned paths:
`hub/cmd/tt/worktree_cleanup.go`, `hub/cmd/tt/worktree_cleanup_test.go`,
`hub/cmd/tt/worktree_cleanup_diag_test.go`.

- F2-1, `$HOME` spellings. `worktreeEvidencePatterns` adds `$HOME/…` and
  `${HOME}/…` next to `~/…`. Criterion: the diag case "literal $HOME" and
  a `${HOME}` case report kept `evidence`.
- F2-2, unresolved root. `artifactPass` keeps the root as configured (before
  `canonicalPath`). When it differs from the resolved root, `cited` also
  tries the checkout path with the resolved root swapped for the configured
  one. Criterion: the diag case "unresolved symlinked artifacts root" reports
  kept `evidence`. A symlinked root with an absolute resolved citation stays
  kept.
- F2-3, item-folder-relative (optional). `cited` also resolves a relative
  token against the directory of the receipt or plan file holding it, and
  keeps the checkout when that lands inside it. Criterion: the diag case
  "item-folder-relative" reports kept `evidence`. A relative token that
  resolves outside the checkout, such as `../x` or a sibling `-logs`, still
  does not keep it. No producer writes this spelling today, so it can be
  dismissed instead.

## c4: re-check cost of kept finished entries

**Reproduces, bounded.** The test sets up three (N) finished, accepted
entries. Each has a gone queue worktree and a kept verifier checkout. It
covers three keep reasons, retention (`TAILTERM_ARTIFACT_ACCEPTED_AFTER=24h`),
evidence (cited by a receipt) and dirty (an untracked file), at one level
(`<item>/verifier-x`) and nested (`<item>/verifier-x/checkout`). It counts
four runner ticks on the runner's clock. Counts include the tick's own reads:
the host listing, the active view and one GetTask.

| Tick | GetTask | GetTeamQueueEntry | History reads | Git calls |
| --- | ---: | ---: | ---: | ---: |
| first (t0) | 1 + N = 4 | N = 3 | 1 | 4N = 12 (5N = 15 dirty) |
| t0 + 1m (inside the interval) | 1 | 0 | 0 | 0 |
| t0 + 15m (one interval on) | 1 + N = 4 | N = 3 | 1 | 4N = 12 (5N = 15 dirty) |
| t0 + 16m | 1 | 0 | 0 | 0 |

The figures are the same for one-level and nested layouts and for all three
reasons, apart from the dirty case's extra `git status`.

Per examined entry, the cost is:

- One `GetTeamQueueEntry` (:2261), because history entries are summaries.
- One `GetTask` of the same project (:2273). Each entry reads the same detail
  again.
- Four Git calls from `cleanupWorktrees`, made before any keep rule runs:
  `rev-parse --git-common-dir` and `worktree list --porcelain`
  (`resolveWorktreeRepo`, :849), then `rev-parse --verify tasks-hub` and one
  `git grep` over `docs/` on tasks-hub (`docsCitations`, :899).
- A dirty checkout adds `git status` (`worktreeDirty`, from `classifyWorktree`).
  Retention returns at the item gate before that. Evidence returns at the
  receipt citation.

A kept receipt is written once per process (`recordWorktreeDecision`,
:389-396), so the receipts journal does not show the repeats. The test
asserts exactly N kept receipts after four ticks.

What bounds it:

- The interval gate (:2256, `closeoutWorktreeInterval` 15m at :89-91) and the
  per-project history read (`closeoutHistory`, :332-347).
- The runner examines only the newest history page (`team_runner.go:861`;
  `DefaultTeamQueueHistoryLimit` = 50, `hub/internal/api/team_queue.go:339`),
  and only while the project has an active entry.
- So "forever" lasts until the entry leaves the newest 50. After that only
  the hourly sweep looks at it (`defaultSweepInterval`,
  `sweep_schedule.go:36`). The sweep reads each project once per pass
  (`readSweepHub`, :2441), so the old per-entry GetTask claim does not apply
  to it.
- Worst case for one project: 50 GetTask + 50 GetTeamQueueEntry + 200-250 Git
  calls every 15 minutes. `worktree list` and `git grep` scale with the
  repository (the a9 dry run listed 183 linked worktrees). Their live timing
  was not measured.

**#41690 f3 is confirmed.** The test asserts that
`detachedCheckoutWithin(<item>, 1)`, the pre-c3b7ce55 existence check, does
not find the nested layout. Before c3b7ce55, then, a nested kept checkout of
an entry whose queue worktree is gone cost nothing, because closeout returned
at the existence check (:2241). Now it costs the same as one level.

Host scale, for context only: the receipts journal (read-only, 2026-10-01 to
2026-10-21) has closeout keeps on 182 distinct paths for `unpushed`, 35 for
`retention` and 6 for `nested`. That counts paths, not live entries.

**Fix options** (a later bounded bug order). Owned paths:
`hub/cmd/tt/worktree_cleanup.go`, `hub/cmd/tt/team_runner.go`,
`hub/cmd/tt/worktree_cleanup_test.go`,
`hub/cmd/tt/worktree_cleanup_diag_test.go`.

- F4-1, one GetTask per project per pass. The runner reads the project
  detail once for the closeout loop and passes it in. Closeout keeps its
  existing reads for direct callers. Criterion: in the diag test, a tick that
  examines N entries makes 1 + 1 GetTask (not 1 + N). The other counts are
  unchanged, and the existing closeout tests pass.
- F4-2 (optional), skip retention until it can end. An accepted entry whose
  checkouts are kept for retention is not examined again before
  `AcceptedAt + acceptedAfter`. A release seen on the history page ends the
  skip. Criterion: retention ticks before that time make no GetTask,
  GetTeamQueueEntry or Git call. The first tick after the time, or after a
  release appears, examines the entry. This matters only when
  `TAILTERM_ARTIFACT_ACCEPTED_AFTER` is set, since the default wait is 0.
- F4-3 (optional, larger), one cleanup pass per project. One
  `cleanupWorktrees` call covers every entry examined in a tick, so the four
  setup Git calls run once. Each decision keeps its own entry's receipt
  fields. Criterion: Git calls per beyond-interval tick are 4 + (dirty
  entries), and the receipts name each entry's `entryId`.

A persisted per-head backoff, as review #16358 suggested, is not
recommended. The interval and the 50-entry page already bound the cost, and
a backoff delays removal after a dirty checkout is cleaned.

## c5: nested repository scan before the permission walk

**Reproduces.** A clean, integrated worktree holding a mode-000 directory
under ignored `.build/` is kept `remove-failed`, with detail
`scan for nested repositories: open <dir>: permission denied`:

- in a dry run, from `classifyWorktree`'s scan (:1147-1156);
- when applying, from the removal re-check: `removeWorktree` scans at :826,
  before `makeWorktreeWritable` at :833.

The outcome is the same whether or not a repository lies below the
unreadable directory. The directory's mode is still 000 after both passes,
because the permission walk never ran and there is nothing to restore.

A verifier checkout fails earlier when applying, with
`manifest: open <dir>: permission denied`. `removeArtifactCheckout` builds the
manifest first (:1815), and its usage walk (`treeUsage` via
`buildArtifactManifest`, :1675) reads the directory. No manifest or pin is
left behind.

The test also probes the reverse order on today's functions.
`makeWorktreeWritable` opens the directory (WalkDir visits a directory
before reading it, and the walk adds owner rwx). `nestedRepository` then
finds nothing in one case and the repository in the other, and `restore()`
puts mode 000 back. So reversing the order would remove the first worktree
and keep the second as `nested`.

**Evidence to dismiss.** This is the safe direction: nothing is deleted, and
only reclaimable disk is held. The host receipts journal
(`~/.local/state/tailterm/relay/worktree-cleanup.jsonl`, read-only) covers
2026-10-01 04:01Z to 2026-10-21 15:11Z: 36,645 receipts, 0 of them
`remove-failed` with `permission denied`. The a9 dry run on 183 worktrees
also reported no `remove-failed` (handler refinement, 2026-09-30). The claim's
own condition, "act only on an evidenced dry-run case", is not met.

A fix is not a reorder alone. A dry run must not chmod, so it cannot see
below the directory. The verifier checkout path would also need the walk
before its manifest. If a case appears, a later order would own:

- paths: `hub/cmd/tt/worktree_cleanup.go`,
  `hub/cmd/tt/worktree_cleanup_test.go`,
  `hub/cmd/tt/worktree_cleanup_diag_test.go`;
- f5a: when applying, the permission walk runs before the nested scan and
  the manifest, and a worktree with no nested repository is removed;
- f5b: a nested repository under an unreadable directory is kept `nested`;
- f5c: every mode changed for the scan is restored when the worktree is
  kept or its removal fails;
- f5d: the dry run reports a distinct keep detail for an unreadable
  directory instead of a scan failure.

## Verification

- `go test ./hub/cmd/tt -run WorktreeCleanupDiag -count=3`: pass.
- `go vet ./hub/cmd/tt` and `gofmt -l`: clean.
- The lead runs the full verify-matrix as verifier (a7).
- No hub writes. Each closeout subtest asserts the fake hub saw none. The
  live checks above were read-only local file scans.
