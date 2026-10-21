# Handler floor and rotation remainder: diagnosis

Item `wi_0f23db4849705da0` revision 2, work order #42409, ASSIGN #42429.
This is a diagnosis only, and no product code changed. Base: tasks-hub `79c0895e`.
That base includes both earlier holds: `wi_fc9b` at `8757a759` and `wi_c399` at `79c0895e`.

Tests: `hub/internal/store/handler_floor_diag_test.go`. Each test asserts what
the hub does today. A later fix order updates or replaces the test for its claim.
Each test uses an isolated SQLite store with synthetic agents and messages. No
live hub, handler or tmux session is touched.

```sh
cd hub && go test ./internal/store -run 'HandlerFloorDiag' -count=1 -v
```

Line numbers below are at `79c0895e`.

## Summary

| Claim | Item | Today at 79c0895e | Test |
|---|---|---|---|
| f1: closing the primary under a prepared rotation strands it | wi_25f41fb1831ff328 | Reproduces | `TestHandlerFloorDiagF1ClosingPrimaryStrandsPreparedRotation` |
| f3: the zero-handler notice commits before the status write | wi_bebcd76367a4f29f | Reproduces | `TestHandlerFloorDiagF3NoticeCommitsBeforeStatusWrite` |
| f4: an abort reaches zero handlers with no notice | wi_183bebdee30e07cb | Reproduces | `TestHandlerFloorDiagF4AbortReachesZeroWithoutNotice` |
| g3: an owner-integrated released entry routes to the dead handler | wi_70e2545bb8a643f0 | Reproduces, as a refusal | `TestHandlerFloorDiagG3OwnerIntegratedEntryRoutesToDeadHandler` |
| g2: a re-sent request reaches the successor twice | wi_c3267a9dd720279f | Reproduces | `TestHandlerFloorDiagG2ResentRequestReachesSuccessorTwice` |
| f5: the docs describe a launch-cleanup limit that cannot occur | wi_02b2d44b62965d78 | Still true: the bullet is still wrong | read only |
| Document that `tt close` of a retired primary is not refused in a paused project | wi_bb83658378b0abcf | Still undocumented | read only |

No claim has been fixed since it was filed, so there is no fixing commit.
`8757a759` (wi_fc9b) changed `handler_floor.go` at line 33 to allow closing an
exited primary. That is Part A's f2, which this item excludes, and it does not
touch the paths in f1 or f4.

## f1: closing the primary during a prepared rotation strands it (reproduces)

Evidence:
- `hub/internal/store/handler_floor.go:29` asks `handlerSuccessorReady` for both
  retire and close. Line 66 returns ready for any prepared rotation away from the
  handler, so the primary rule at line 33 lets a close through.
- Commit then refuses a closed old handler with `handler_changed`
  (`hub/internal/store/handler_rotation.go:1179`). The rotation stays prepared and
  can only be aborted.
- Abort closes the successor by raw SQL (`handler_rotation.go:1442`).
- The test makes P the recorded primary, prepares a rotation away from it and
  registers the successor S2. It then closes P with `CloseAgentRun`, which
  succeeds. Commit is refused with `handler_changed`, the rotation is still
  prepared, and the recorded primary is still the closed P. After abort, no
  handler is available and no owner notice was posted.
- Introduced by `072f44f9` (2026-09-30). Before that commit the close was also
  allowed (review #17294).

Fix options for a later order:
1. **Recommended.** A prepared rotation lifts only the retire refusal. A close
   needs a committed chain (`liveSuccessor`). Pass `next` into
   `handlerSuccessorReady`, or check `next == api.AgentRetired` at the call.
   - Paths: `hub/internal/store/handler_floor.go`,
     `hub/internal/store/handler_floor_test.go`,
     `hub/internal/store/handler_floor_diag_test.go` (replace F1), and
     `docs/handler-rotation.md:46-47`, the "A ready successor lifts both
     refusals" text.
   - Criteria:
     - With only a prepared rotation away from the recorded primary, closing it
       is refused with the primary refusal on `PATCH`, `DELETE` and a `closed`
       event, and nothing changes.
     - Retiring it is still allowed.
     - After commit the old primary is closed as today.
     - `TestHandlerFloorRefusesPrimaryUntilRotationIsPrepared` still passes.
2. Leave the floor alone and make commit accept a closed old handler. Not
   recommended: commit's stale-handler refusal protects the handoff of open
   obligations.

## f3: the zero-handler notice commits before the status write (reproduces)

Evidence:
- `setAgentStatus` calls `escalateNoHandler` (`hub/internal/store/store.go:1221`).
  That function opens and commits its own transaction
  (`hub/internal/store/handler_floor.go:76-89`).
- Only after that does `writeAgentStatus` update the agent, through `s.db`
  (`store.go:1238`), and add the event outside any transaction.
- The test adds a `BEFORE UPDATE OF status` trigger to the test database that
  aborts the handler's `exited` write. Two `exited` reports both fail with the
  injected error. The handler stays `running` with no `exited` event, and each
  attempt leaves a notice: 1, then 2. With the trigger dropped, the third report
  succeeds, so one real transition leaves three notices.
- Introduced by `072f44f9`.

Fix options:
1. **Recommended.** Write the notice, the status and the event in one
   transaction. `escalateNoHandler` takes the caller's `*sql.Tx`
   (`postBrokerNotice` already takes one). `setAgentStatus` and the status
   write run in that transaction, and the change notifies after commit.
   - Paths: `hub/internal/store/handler_floor.go`, `hub/internal/store/store.go`
     (`setAgentStatus` and `writeAgentStatus`, lines 1197-1255),
     `hub/internal/store/handler_floor_test.go` and
     `hub/internal/store/handler_floor_diag_test.go` (replace F3).
   - `CloseTask` also calls `writeAgentStatus`, so the order must keep it
     working.
   - Criteria:
     - When the status write fails (an injected trigger), no notice and no
       event exist, and a retry that fails again adds none.
     - When the write succeeds, there is exactly one notice.
     - `TestHandlerFloorEscalatesOncePerZeroCrossing` still passes.
2. Post the notice after the status write, in a second transaction. Not
   recommended: a crash between the two loses the notice.

## f4: an abort can reach zero available handlers with no notice (reproduces)

Evidence:
- The prepared rotation lets the primary retire (`handler_floor.go:47,66`).
  The successor keeps the available count at two, so no notice is due.
- Abort closes the successor by raw SQL (`handler_rotation.go:1442`) and never
  calls the floor.
- The test retires P under a prepared rotation with S2 registered, then aborts.
  S2 is closed, P is retired, no handler is available, and no owner notice was
  posted.
- `docs/handler-rotation.md:88-90` lists abort as exempt. That covers the
  refusal, but the notice paragraph at `:81-86` says each drop to zero posts
  one notice.
- f1's recommended fix does not remove this case, because a retire under a
  prepared rotation stays allowed.

Fix options:
1. **Recommended.** After the abort closes an available successor, count the
   available handlers in the same transaction. If none remain, post the owner
   notice there, using the transaction-scoped escalation from f3.
   - Paths: `hub/internal/store/handler_rotation.go` (`abortHandlerRotation`,
     lines 1420-1458), `hub/internal/store/handler_floor.go`,
     `hub/internal/store/handler_rotation_test.go` or `handler_floor_test.go`,
     `hub/internal/store/handler_floor_diag_test.go` (replace F4), and
     `docs/handler-rotation.md:88-90`, which would say abort is exempt from the
     refusal but not from the notice.
   - Criteria:
     - An abort that leaves no available handler posts exactly one board-wide
       owner notice, `cause=no-database-handler`.
     - An abort that leaves one or more posts none.
     - A replayed abort posts nothing.
   - This order depends on f3, or must be serialized after it, because both
     change the shared escalation helper.
2. Abort refuses to close the successor when doing so would reach zero. Not
   recommended: an aborted rotation must not keep a half-started successor
   alive.
3. Document the gap and dismiss. Not recommended: it conflicts with item a3's
   intent that a notice is posted whenever the count reaches zero.

## g3: an owner-integrated released entry still routes to the dead handler (reproduces, as a refusal)

Evidence:
- A released entry is not a lease (`hub/internal/store/team_queue_scope.go:68`,
  `released_at=''`). `loadHandlerBusy` (`handler_rotation.go:408-411`) and
  `moveDeadHandlerLeases` (`:804`) never see it, so no rotation rewrites its
  handler.
- Item role resolution still reads an owner-integrated released entry's
  `handler_id/handler_run_id` (`hub/internal/store/phase3.go:264`).
- After the rotation that run is closed. A closed agent is never `Online`
  (`store.go:503`), so `phase3.go:271` returns `exact item role run is
  unavailable`.
- The test gives `items[0]` an entry with state `running`, `released_at` set and
  `owner_integration_json` set, held by the old handler. After a committed
  rotation the entry still names the old handler, and an item-linked request to
  `role:database_handler` is refused with that conflict. It is not routed to the
  live successor.
- Control: the same setup with `owner_integration_json=''` falls through to the
  primary and reaches the successor.
- So the role does not deliver to the dead run. It is refused, and the sender
  cannot reach the handler for that item.
- Introduced by `d14821f8` (2026-09-29).

Fix options:
1. **Recommended.** In `resolveRole`, when the entry's handler is closed and
   was rotated, follow `liveSuccessor` (`handler_rotation.go:251`) and resolve
   to the open end of the chain. Keep the refusal when the handler closed with
   no committed rotation.
   - Paths: `hub/internal/store/phase3.go` and a store test (for example
     `hub/internal/store/phase3_test.go`), plus
     `hub/internal/store/handler_floor_diag_test.go` (replace G3), and
     `docs/handler-rotation.md:567`, a routing note.
   - Criteria:
     - After a committed rotation, a `role:database_handler` request linked to
       an item whose owner-integrated released entry names the closed old
       handler resolves to the live successor.
     - If that entry's handler closed without a rotation, the request is still
       refused.
     - Plainly released and live entries route as today.
2. Make every rotation commit rewrite the old handler's owner-integrated
   released entries to the successor. Not recommended: it touches queue rows
   and revisions outside the lease model, which the team runner's lease checks
   read.

## g2: a request re-sent after the broker closed it reaches the successor twice (reproduces)

Evidence:
- For an exited primary, commit appends every obligation the broker closed
  `recipient_gone` after the exit (`handler_rotation.go:679-698`, called at
  `:1242`). It reissues each one through `reissueObligation` (`:1262`) without
  checking what the sender sent since the close.
- `docs/handler-rotation.md:763-765` already lists this as a known limit.
- The test exits the primary, and the broker closes the first request. It then
  prepares a dead-primary rotation with idle-shell evidence and registers the
  successor.
- The worker first tries `role:database_handler`, which is refused while the
  rotation is prepared: the primary exited, and the prepared successor is
  excluded. The worker then re-sends the same subject to the successor by
  agent ID.
- After commit, the handoff lists the closed request as reissued with no skip
  record. The successor holds two open request obligations with that subject:
  the re-send and the reissue.
- Introduced by `ecd6bbdc` (2026-10-07).
- Note on the role branch of the criterion: during a prepared exited-primary
  rotation with no other handler, a role re-send is refused, as the test
  asserts. A role re-send matters when another handler holds the role in that
  window, or when the sender's message carries a request ID.

Fix:
- Saved criterion, carried verbatim from wi_fc9b r5: a reissued obligation is
  skipped (and the skip recorded) when the same sender already sent the
  successor or the role a request with the same request id, or the same subject
  and work item after the close. A store test covers re-sent and not re-sent.
- Option 1, **recommended.** In `commitHandlerRotation`, before reissuing a
  closed obligation taken from `exitedHandlerClosedObligations`, look for a
  later message from the same sender. That message must be addressed to the
  successor, or to `role:database_handler`, and carry the same request ID, or
  the same subject and work item. It must be sent at or after the obligation's
  `closed_at`. If one exists, leave the old obligation closed and record the
  skip in the handoff, for example a `skipped` list of old seq, new seq and
  reason.
  - Paths: `hub/internal/store/handler_rotation.go`,
    `hub/internal/api/handler_rotation.go` (the handoff type),
    `hub/internal/store/handler_rotation_test.go`,
    `hub/internal/store/handler_floor_diag_test.go` (replace G2), and
    `docs/handler-rotation.md:763-765` (remove the known limit) and `:545`
    (handoff contents).
  - If the CLI prints the handoff, also `hub/cmd/tt` for `tt handler rotation
    get`. The order should confirm this when planning.
  - Criteria: the saved criterion above. In addition, an obligation still open
    at commit is reissued as today.

## f5: the docs describe a launch-cleanup limit that cannot occur (still true)

Checked by reading. No doc was edited.
- The bullet is at `docs/handler-rotation.md:794-795`: "Team launch cleanup
  cannot close a failed launch that is the project's only handler; the handler
  stays starting and the owner sees it."
- The discarded `CloseAgent` calls are at `hub/cmd/tt/main.go:1496,1502,1507,1519,1545`.
  The review cited 1222-1265 at its base. All five are in the non-persistent
  `else` branch.
- A database handler takes `ensurePersistent` (`main.go:1485-1489`), and
  `api.PersistentAgentRole` includes `database_handler`
  (`hub/internal/api/backlog_steward.go:42-43`). That is `ensureHandler`
  (`hub/cmd/tt/handler_spawn.go:121,123`), which has no `CloseAgent` call.
- Team close skips database handlers (`hub/cmd/tt/team_close.go:97`).
- So no launch cleanup path closes a handler, and the limit cannot occur. The
  bullet is unchanged since `072f44f9`.

Fix:
- Path: `docs/handler-rotation.md` only.
- Criteria: remove the bullet at `:794-795`, or replace it with an accurate
  statement: a failed handler launch is kept by `ensureHandler` and never closed
  by launch cleanup. No other doc text claims that launch cleanup closes a
  handler.
- This can share an order with the wi_bb83 doc sentence below.

## wi_bb83: document that `tt close` of a retired primary is not refused in a paused project (still undocumented)

Checked by reading. No doc was edited.
- Code: `handlerFloorCheck` returns early unless the project is open and active
  (`hub/internal/store/handler_floor.go:22`). `UpdateAgent` allows `closed` and
  `exited` while paused (`hub/internal/store/store.go:1174`).
- So in a paused project the hub accepts the close of a retired primary.
  `tt close` sends it for a retired handler (`docs/handler-rotation.md:67`).
- Docs: `:33` scopes the floor to "While a project is open and active", which
  implies the exemption. But `:42`, "This holds even when the primary is
  already retired", and the `tt close` steps at `:65-71` say a retired primary
  is refused, and they do not mention the paused case. The pause exemption at
  `:88-89` covers the pause action itself, not a later close.
- Reachability, from reading only (untested): pause closes every live and
  retired run of the current team in its own transaction
  (`hub/internal/store/project_pause.go:259,342`). A retire is refused while
  the project is not active (`store.go:1174`). So a retired primary in a paused
  project should be rare.

Fix options:
1. **Recommended.** One sentence after `docs/handler-rotation.md:42`, or in the
   `tt close` steps: in a paused or resuming project the floor does not apply,
   so the hub accepts closing a retired primary. Fold it into the f5 doc order.
   - Path: `docs/handler-rotation.md`.
   - Criteria: the sentence is present and matches `handler_floor.go:22`.
2. Dismiss as implied by `:33`, with the reachability note above as the
   evidence.

## Verification and limits

- Run: `go test ./internal/store -run 'HandlerFloorDiag' -count=1 -v` (from
  `hub/`). All five tests pass, and g3 runs two subtests. The full store-race
  matrix is the lead's run.
- `gofmt -l` is clean.
- No live hub write was made, and no Board page was read for this diagnosis.
- Not run: the full matrix (lead), and any check of wi_bb83's reachability
  beyond reading.
