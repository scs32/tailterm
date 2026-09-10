# Session lifecycle closeout

Work item `wi_af4cb8d286da4163` revision 3, bounded work order #1304
(`wi_af4cb8d286da4163-lifecycle-closeout-1`) owns this change. The implementation
candidate is `2b4c4f96fddabb524cdc1ebb4100c21952abf36e`, based on
`d102ab01b085677eea92c8b4411cb2f9119272c2`.

After lead acceptance, the implementation was replayed without conflict onto
released scrolling base `fb37de22235125e5d4dae96870c9688507c40657` as
`c18dfaf2ea60dee6372706cc92759778d2dcd8c8`. Release artifacts use this
integrated source line and therefore preserve the scrolling release.
The final revision-3 application commit is
`9c7bb254a590c073e9a1f6901757dc7448a7478c`.

## Behavior

An implementation worker session belongs to one bug or feature. After its result
is accepted and dependencies are resolved, `tt close NAME` records closure intent
for the exact current agent/run and uses the existing host cleanup path. The
project remains open; the orchestrator and active database handler are protected
by the CLI. A later item receives a fresh identity and context. Retirement remains
available only for intentional temporary retention of the same item session.

Closure intent is the durable `closed` agent state. Verified termination remains
the separate, retryable `cleanupDone`/`cleanupError` receipt. The close endpoint
now requires the expected run. Stale requests cannot close a replacement. A retry
by name refuses ambiguous current/pending identities and directs the caller to an
exact agent ID.

Before closing, the host records the tmux session's stable ID and creation time
with its hub/task/agent/run metadata. Cleanup checks those fields again inside
tmux. Renamed exact sessions close; reused names, changed creation identities,
mismatched runs and unrelated sessions stay open. A missing exact session can be
confirmed on the saved host. If a cleanup response is lost after the hub commits
it, the local receipt remains and an exact retry recovers the monotonic success.
A later task close preserves prior individual cleanup success and cleans only the
remaining agents.

Before closeout, the worker and orchestrator inventory useful long-lived services
descended from the worker's tmux session. A service that must continue is handed
off or detached and reverified first; exact tmux cleanup intentionally terminates
all remaining descendants.

Project settings compactly expose closed agents whose cleanup receipt is still
pending and offer the same exact-host retry. Confirmed closed agents leave the
active roster; immutable messages, item bindings, revisions, results and event
history remain stored. Files stays hidden.

## Candidate verification

All fixtures used temporary SQLite databases, private tmux sockets and disposable
browser contexts. No live task/profile data, runtime transcripts or the handler's
operational eligibility audit were used.

- `npm test`: 133/133 passed on the integrated scrolling baseline.
- `node tests/task-form-browser.mjs`: Chromium and WebKit passed. The isolated
  real hub/CLI path kept the parent open, displayed and retried one absent closed
  worker, saved its receipt, then preserved that receipt through later task
  closure while cleaning the remaining sessions.
- Focused Go store/server/CLI tests passed for exact and stale run intent,
  idempotency, open-parent cleanup, renamed sessions, reused identities, absent
  sessions, response loss after commit, mismatched old runs, late hooks, monotonic
  success, fresh same-name identity, protected roles and legacy task closure.
- The same focused store/server/CLI checks passed under `go test -race`; `go vet
  ./...` passed.
- The complete store and server packages passed. The broad CLI package is not
  reported as passing: pre-existing `TestPostHumanReplyAndLiteralHelp` failed its
  route expectation and `TestAskPreservesIdentityContextAndReplayPayload` blocked
  in its channel send until the five-minute suite timeout. Focused reruns reproduce
  those unrelated failures; the closeout tests finish in under one second (about
  two seconds under race).

## Release and operational cleanup

Lead accepted the candidate and assigned the release/cleanup slot in Board message
#1349. Handler-confirmed completion made `items-scroll` the seventeenth exact
eligible identity. During the first exact release, all 17 approved agent/run
records saved `closed` plus `cleanupDone=true`, their matching tmux sessions
disappeared, the project stayed open, and lead, db-handler, narrative-history and
lifecycle-closeout retained their exact sessions. No candidate mismatched.

The Mini preview initially survived exact-package activation and production
browser acceptance as PID 28664. Closing the approved `items-scroll` tmux also
terminated that preview and its parent, revealing that the supposedly shared
listener was still a descendant of a disposable worker session. The same exact
package was immediately restored on port 4318 as detached PID 60799 with PPID 1;
all 81 assets and the production browser acceptance passed again. Lead's
independent check #1366 confirmed both the 17 durable receipts/protected roster
and the detached replacement. The handler saved this evidence and the bounded
descendant-service guidance amendment as work-item revision 3 in #1368.

The final source, deployment identities, backup/rollback evidence, hashes and
per-agent receipt inventory are recorded in the release receipt. The revision-3
guidance was rebuilt and reverified on every changed component. The old PID is
not claimed as preserved.
See
[the release and cleanup receipt](releases/tailos-2026-09-09-lifecycle-closeout.json).
