# Overnight integration evidence

Feature `wi_90df5e8421f7f988`, revision/scope 2; owner work order #9951; lead continuation assignment #9991 (superseding #9981); builder `agt_1fde6dbfa7684098` / `run_e9931368417539fe`. Host: Stephens-Mini. Worktree: `/Users/stephenspeicher/projects/tailterm/.build/worktrees/overnight-integration-20260925`; branch: `integration/overnight-2026-09-25`; base: `414aae0f9a9b57379cfec81cd78e571765293969`.

The eleven cherry-picks preceded the bounded lead assignment. Handler reconciliation #9989 records the first Start as native event #303653 at 2026-09-25 08:11:35Z, the ordered commits at 08:11:55–08:12:07Z, and the sequencing gap without attributing them to a later assignment. The builder stopped when it read the lead hold, then consumed the handler's verified revision-2 snapshot and revised plan #9980. A separate `tt event started` and lead notice #9996 precede the continuation commit; handler #10002 verified that continuation as native event #303851 at 08:20:28Z. The full revision-2 supplement #9998 was delivered at 08:21:41Z, after that Start and initial continuation work. At the next checkpoint, the builder verified its SHA-256 `72955ff4766c3729571276530f089b4224af5b7c023082a32d33c7346bc67f62` and consumed both revisions, 46 linked messages, team bindings and saved audit. Lead #10003 directed no duplicate Start. The originally admitted run and context digest `144dbf6d999a1981ff7cc2fbe0d1e18e765d37258ee19c9cc985d63bc1c721bd` were retained.

## Source to integration mapping

All source commits were cherry-picked in owner order. There were **no Git conflicts** and no conflict hunks to resolve. Git auto-merged `hub/cmd/tt/main.go` while applying team launch; while applying team close it auto-merged `client/team-examples.js`, `docs/team-examples.md`, `hub/cmd/tt/coordination.go`, `hub/cmd/tt/main.go`, `hub/internal/api/client.go`, `hub/internal/server/server.go`, and `tests/team-examples.test.js`. The integrated diff and targeted tests retain the withdrawal, launch, and close additions in those files. Handler #9989 independently matched all eleven stable patch IDs.

| Candidate | Source | Integrated |
| --- | --- | --- |
| Projects UI | `93ee5d8` | `ab07df5` |
| Projects UI | `46e357e` | `3b1d243` |
| Projects UI | `ecee1c9` | `7534e54` |
| Projects UI | `d1c98df` | `013a582` |
| Reply wake | `1412042` | `b9bd251` |
| Reply wake | `4c43e77` | `24f987f` |
| Sender withdrawal | `f1263dd` | `5afa36c` |
| Sender withdrawal | `870f15e` | `0dfa0ed` |
| Team launch | `6bdb639` | `c094bc0` |
| Team close | `f5b60ee` | `6abc653` |
| Team close | `3d312a1` | `e0624d6` |

Post-assignment commit `31effac` adds two cross-feature store fixtures and regenerates `hub/internal/teamplan/plan.mjs` from the integrated client modules. The generated plan check failed before regeneration because withdrawal and close changed bundled `client/team-examples.js`; it passes after regeneration. The withdrawal fixture proves an open substantive request blocks team close, then a withdrawn request permits it. The reply fixture proves an unaddressed reply to a live author from the current run stamps `AckedAt` and releases the 3.1 gate; a stale run cannot do so. No production behavior was edited after the cherry-picks.

## Verification

Logs and test binaries are in `.build/integration-evidence/overnight-20260925/` in this worktree; this directory is ignored by Git. The test runs use isolated fixture databases, browser contexts, and private tmux sockets. Browser runs used the root checkout's ignored `wasm/tailserve.wasm` through an ignored worktree symlink; the candidates do not change WASM.

| Check | Result | Log |
| --- | --- | --- |
| `go vet ./...` from `hub/` | Pass | `go-vet.log` |
| `go test ./...` from `hub/` | Only `TestWorkItemsCLIUsesBodyFilesAndDurableReceipts` and the two audit-export tests fail | `go-test.log` |
| `npm test` | 210 pass; only `Board handles a second show while the first request is pending` fails | `npm-test.log` |
| Candidate-focused Go tests in cmd/tt, store, server, bridge, broker | All five packages pass | `candidate-go-tests.log` |
| Candidate-focused Node tests | 23 pass | `candidate-node-tests.log` |
| `npm run check:team-plan`; CLI/TailOS parity test | Pass | `npm-test.log`, `candidate-node-tests.log` |
| `node tests/task-form-browser.mjs` | Chromium and WebKit pass | `task-form-browser.log` |
| `node tests/project-compact-ui-browser.mjs` | 12 Chromium/WebKit viewport cases pass | `project-compact-ui-browser.log` |

The fixture `TestWithdrawRollbackFixture` created a fresh SQLite database in `/tmp/tailterm-withdraw-rollback-overnight-Vy3txJWB` using the integrated store (`rollback-fixture.log`). The integrated hub binary opened that database, then the `414aae0` hub binary opened the same migrated database. Both returned HTTP 200 for the fixture task and its withdrawn obligation on a loopback development listener (`rollback-check.log`, `rollback-new-hub.log`, `rollback-old-hub.log`). `PRAGMA integrity_check` returned `ok` and `PRAGMA foreign_key_check` returned no rows (`rollback-integrity.log`). Only those fixture processes were terminated. The old and new binaries in the evidence directory have SHA-256 `79d8c0c083fe78e0111d5a38a4f238529c6b62d4c1606f83b19a25826292f09f` and `af2e425c30f3dd4bbe489f7390a05e2e53d739752a5e4277f3752b7585c89f03`, respectively.

The independent integration-only review and handler-saved acceptance remain pending. No push, `tasks-hub` edit, deployment, or live database change was made.
