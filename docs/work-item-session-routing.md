# Work-item session routing

Final implementation report for bug `wi_c97ba465a6f2a3ab`, work order
`wi_c97ba465a6f2a3ab-routing-1`, assigned on Board #814. The implementation was
accepted by lead on Board #1040 at candidate
`d495aa80847528c647729f7b704b068c5785f82f`. The database handler recorded the
final documentation scope on Board #1042. Release and deployment still require
the explicit main-slot handoff described below. The database handler committed
release order `wi_c97ba465a6f2a3ab-release-1` at item revision 18, dispatch #1045,
with exact readback on Board #1046.

## Source and dependency record

Routing is built on the accepted immutable work-item history source
`9ea86535e2e401e879d4fa1316483772de5e674e` and its integrated hardening. The
history reader remains the only authority for exact revisions, reconstructed
provenance, declared gaps, explicit source/A1 message links, dispatch snapshots
and coverage. Routing consumes that interface; it does not create a second
snapshot or revision authority.

The implementation sequence on this branch is:

- `e91a9b2` adds item-scoped admission, stored context and exact-run restore;
- `620cea7`, `9dac312` and `d4015a9` integrate and harden the accepted immutable
  history source;
- `93adeb6` makes routing consume complete accepted history pages;
- `e342130` rejects oversized complete context without truncation;
- `7472a70` preserves failed replacement attempts while allowing a new retry;
- `e99f784` preserves generic runtime briefings alongside routed context;
- `5f11207` freezes scoped partial-launch retries and preserves typed decision
  context through human answers;
- `d495aa8` permits exact launch-revision posts and asks after a handler edit,
  only for the validated binding of the author's current run.

## Durable routing contract

An ordinary agent may still be admitted without item metadata for compatibility.
The reusable-team item route instead binds each new worker to exactly one bug or
feature. The immutable agent/run binding records:

- item project, ID and exact launch revision;
- the structured, item-linked work-order message;
- the Board message sequence through which the new inbox starts;
- an optional prior agent identity that the new session replaces;
- the SHA-256 digest of the exact prepared context injected into the model.

The binding, initial read cutoff, agent row and `agent_added` event commit in one
transaction. Messages before the cutoff are represented by the prepared bundle,
not replayed through the live inbox. Later reads for a bound worker join only
messages with an explicit primary link to the same item. Recipient, reply chain,
timing and prose never infer relevance. Unbound agents retain the existing
Board/inbox behavior.

The model bundle is prepared before admission by the authorized human launch
flow using the database-handler-owned record and accepted history routes. It
contains the exact selected revision, every revision page, every gap page and
every explicit source/A1 message-link page with their original attribution and
coverage. It excludes unrelated Board conversation. The same bytes are stored
with their digest and passed to the new runtime.

`GET /v1/tasks/{task}/agents/{agent}/work-context?runId={exact-run}` returns only
that immutable stored bundle and binding. A stale run conflicts. `tt context`
uses the exact-run route and cannot select a different item. `tt spawn` accepts
the prepared bundle through `--work-context-file` or the browser-only JSON path,
injects it in the initial model briefing, preserves any compatible generic
runtime briefing, and exports the bound coordinates to the new tmux session.

## Messages, decisions and later item revisions

Bound `tt post` calls automatically attach the item, immutable considered launch
revision, recorded order and a content-derived retry identity. Bound `tt ask`
inherits the same coordinates when its decision file omits them and rejects
explicit mismatches. A human answer copies the typed request's immutable item and
order metadata. This makes progress, questions, decisions, artifacts, results
and acceptance evidence available to strict inbox reads and future replacement
bundles without classifying unlinked conversation.

The item can legitimately advance after a worker launches. A post or new ask may
retain its older considered revision only when all of these checks succeed:

1. the declared author joins to a binding through that agent's current run;
2. the message item, revision and order exactly equal that binding;
3. the stored context bytes still match their persisted SHA-256 digest; and
4. the stored bundle still passes the same item, revision, provenance, coverage,
   revision-order and work-order checks used at admission.

The old revision is not rewritten to the current row. A future revision, an
unbound agent's stale claim, a different item/order/revision, a stale agent run,
or damaged stored context is rejected. Human answers may retain the exact
historical revision considered by their already-valid typed request. This is a
shared-workspace integrity rule, not per-message cryptographic authentication of
the runtime process; the existing hub caller model still applies.

## Replacement and lifecycle

A model replacement receives a new name, agent ID, run ID, tmux session and
runtime thread. `--replaces-agent` links it to a prior agent bound to the same
item. A newly prepared bundle may bind the replacement at the item's later
current revision while retaining earlier explicitly linked messages at their
original considered revisions.

The original agent row, run, terminal session, messages and results are never
rewritten or automatically closed. An exited item-bound identity cannot be
restarted by an unscoped legacy client. Retirement remains distinct from closure,
task-owned terminal groups are preserved, and cleanup receipts remain run-scoped.
There is no claim that an existing model thread can be restarted with mixed item
context.

Parentless browser/team admission is a normal base-team addition and does not
consume `maxNewAgents`. A nonempty parent is an extra helper and continues to use
the existing spawn-enable, lifetime helper allowance and active-agent checks.
Owner-authorized reusable-team launch uses the parentless base-team path.

## Failure and retry behavior

- A malformed, stale, mismatched, incomplete or over-limit prepared bundle fails
  before admission. Agent, binding, cutoff and event cannot partially commit.
- Complete immutable context is capped at 128 KiB. It is never truncated. A team
  whose all-history bundle exceeds the limit is currently unsupported, and a new
  item revision cannot shrink the history already collected.
- An older hub without the required history/context routes returns an error before
  any reusable-team member is launched; the client does not fall back to a
  mutable row or prose-derived history.
- Stable post, ask, answer and work-item update identities return the original
  receipt on an exact retry and reject reuse with different payloads.
- A host-process failure closes only the newly registered failed agent under the
  existing cleanup flow. It does not remove its identity/history or alter the
  original agent being replaced. A later replacement retry uses another exact
  agent/run/session identity.
- After a partially successful browser team launch, completed members and their
  deterministic names stay fixed. The selected item, order and already prepared
  context stay frozen. Only project folders for unlaunched members become
  editable; correcting one rebuilds only that remaining launch command. History
  is not prepared again and completed members are not duplicated.
- Cleanup, closure and retirement failures keep their existing receipts and
  retry semantics. This change does not stop or clean up pre-existing sessions.

## Verification evidence

The following commands passed from a clean
`d495aa80847528c647729f7b704b068c5785f82f` worktree:

```text
cd hub && go test -count=1 ./...
cd hub && go vet ./...
cd hub && go test -count=1 -race ./internal/store ./internal/server
cd hub && go test -count=1 -run TestAgentWorkItemContextAdmissionRestorationAndReplacement -v ./internal/store
npm test
npm run build
node tests/task-form-browser.mjs
node tests/project-work-items-browser.mjs
node tests/work-item-draft-vault-browser.mjs
git diff --check
git status --short
```

Outcomes:

- all Go packages passed; vet was clean; the store/server race run passed;
- the focused synthetic database test passed the full `r1` admission, keyed
  `r2` edit, rejected unbound stale claim, bound post/ask, human answer, strict
  inbox/unread, exact attribution, failed replacement, successful fresh
  replacement and database-reopen sequence;
- all 125 JavaScript unit tests passed;
- the production Vite build passed with only its existing warning for chunks
  larger than 500 KiB;
- Chromium and WebKit passed team admission/partial retry, 65-link history
  pagination and encrypted work-item draft-vault scenarios;
- `git diff --check` passed and the implementation worktree was clean.

Browser/runtime fixtures use isolated databases, browser contexts and synthetic
provider commands. They validate admission, tmux process/session behavior,
filtering, retry, identity preservation and exact context restoration. They do
not validate a live provider-model conversation.

## Additive deployment and rollback boundary

The API and stored data are additive for existing clients. The routing and
immutable-history tables must remain in place during a normal binary rollback;
do not drop them or reinterpret their rows. An older binary will not provide the
new history/context capability, so item-scoped launch and restoration remain
unavailable until the compatible hub/CLI/client is restored. A pre-migration
database backup is disaster recovery only because restoring it would discard
writes made afterward.

Release order `wi_c97ba465a6f2a3ab-release-1` requires a clean exact integrated
commit and coordinates compatible hub, installed CLI and browser assets. It must
preserve the accepted immutable-history implementation, role and
planned-team-member flags, and the Mini preview fix; retain the prior binary and
additive database for rollback; and record deployed identities,
migration/backup integrity, receipts and served-asset verification. The active
history release owns main until lead explicitly hands over that slot, so this
report does not authorize early integration or target activation.

The implementation order did not perform live database/profile reads, migration,
installation, network changes, hub/CLI replacement, browser deployment, target
activation or existing-session shutdown. Those operations, if explicitly handed
off, must use isolated acceptance fixtures rather than live task/profile data and
must follow the saved release order and its sequencing dependency.
