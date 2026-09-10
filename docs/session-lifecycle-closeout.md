# Session lifecycle closeout

Work item `wi_af4cb8d286da4163` revision 2, bounded work order #1304
(`wi_af4cb8d286da4163-lifecycle-closeout-1`) owns this change. The implementation
candidate is `2b4c4f96fddabb524cdc1ebb4100c21952abf36e`, based on
`d102ab01b085677eea92c8b4411cb2f9119272c2`.

After lead acceptance, the implementation was replayed without conflict onto
released scrolling base `fb37de22235125e5d4dae96870c9688507c40657` as
`c18dfaf2ea60dee6372706cc92759778d2dcd8c8`. Release artifacts use this
integrated source line and therefore preserve the scrolling release.

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

Project settings compactly expose closed agents whose cleanup receipt is still
pending and offer the same exact-host retry. Confirmed closed agents leave the
active roster; immutable messages, item bindings, revisions, results and event
history remain stored. Files stays hidden.

## Candidate verification

All fixtures used temporary SQLite databases, private tmux sockets and disposable
browser contexts. No live task/profile data, runtime transcripts or the handler's
operational eligibility audit were used.

- `npm test`: 132/132 passed.
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

No live component or agent session changed during candidate construction. Hub,
Mini/Air CLI, TailOS and Mini preview release identities, backup/rollback evidence,
and exact eligible-agent cleanup receipts will be appended only after lead accepts
the candidate and assigns the release slot. The parent project must remain open;
lead, db-handler, narrative-history, items-scroll and lifecycle-closeout remain
protected until separately released.
