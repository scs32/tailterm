# Projects, Bugs, Features, and database handler — proposed contract

September 8, 2026; design inspection for owner board #111 and lead assignment
#116. Owner asks for two tabs, Bugs and Features; project-scoped databases;
an automatically added database agent; agent-to-handler recording; and Send to
project. This is a proposal, not implementation. Inspection used `tasks-hub`
HEAD `3c3331909ab5607a61f3178059169f626f272e5e` plus concurrent working changes.
Line references identify the inspected version and may move during integration.

**Working assumption, awaiting owner decision:** Send to project dispatches to
an existing open project's orchestrator. It does not create another project or
launch a team. Default destination is the item's owning project; selecting
another project is explicit and does not silently move ownership.

## Smallest coherent product model

- Rename user-facing Tasks to **Projects**, including creation, Board scope,
  settings, history and close confirmations. Keep `tsk_` IDs, `/v1/tasks` routes,
  JSON `taskId`, environment names, internal mode key and existing CLI commands
  initially. Add `tt projects`/`tt new-project` aliases without removing old
  names. Preserve existing groups, history links and exports.
- Add **Bugs** and **Features** top-level tabs, each with an All projects/project
  filter. The same reusable work-item list can appear scoped from a Project.
  Two logical databases can share one typed SQLite table; avoid a separate
  physical database per project or per item kind.
- Every item has one required owning project. All projects is a view, not an
  unowned inbox or a new authorization boundary. List columns: title, project,
  state, priority, updated time, and dispatch state. Keep Send to project available
  from each row/detail with clear pending/accepted outcomes.
- Hub CRUD is authoritative and works while every agent is offline. The handler
  adds conversational intake, classification, deduplication suggestions and
  updates; its memory or filesystem never becomes the database.

## Local evidence and implications

| Evidence | Implication |
| --- | --- |
| [API types](../../hub/internal/api/types.go), `Task`, `Agent`, `Message`; [server routes](../../hub/internal/server/server.go) | Existing IDs/routes fit Projects without migration of identity. No work-item API exists. Agent records contain runtime/host/cwd but not model, permission preset or full launch configuration. |
| [SQLite store](../../hub/internal/store/store.go), schema and `notify`; [additive migrations](../../hub/internal/store/migrate.go) | Extend the hub DB and existing project/global change feeds. Do not put records in encrypted profile envelopes or alter profile migrations. |
| Store `PostMessage` (approximately line 521) | Validates same-project sender/recipient/reply; inserts message/event in a transaction. Current working change resumes an online retired agent only for human directed messages. Reuse these semantics, not a second delivery system. |
| Store `AddAgent` (approximately line 315) | Cap counts non-closed/non-exited agents, including retired agents. Ordinary helpers have a parent, require spawning enabled and consume a lifetime helper allowance. Exited identities can restart on the same host/parent with a new run; explicit ID registration returns an existing identity without validating a launch attempt. |
| [Team launch planning](../../client/teams.js), `teamLaunches`; [task launch](../../client/task-hub.js), `launchMembers` | Orchestrator first; inherited machine/project folder resolved before launch. Partial success survives in browser `progress`; duplicate open names are rejected. Current plan/progress is not a durable crash-recovery launch ledger. |
| [CLI](../../hub/cmd/tt/main.go), `cmdSpawn`; [spawn backend](../../hub/internal/spawn/spawn.go) | Browser launches through SSH; CLI registers then creates local tmux. Failure closes the registration. Helpers inherit cwd and compatible permission settings, but model is an explicit argument. Hub cannot launch remote sessions itself. |
| [Briefing](../../hub/cmd/tt/coordination.go); [relay](../../hub/cmd/tt/relay.go) | Standard worker retirement instructions and exact run/thread wake eligibility also affect a handler. A role label alone neither wakes it nor gives it stronger authority. |
| [Modes](../../client/modes.js), [Projects-to-be UI](../../client/tasks-view.js), [history export](../../client/task-history.js) | Add tabs without unmounting terminal sessions. Existing history format/version must remain readable when adding work items. Files stays hidden. |

## Proposed storage and HTTP/CLI surface

All names below are proposed. Reuse current text/body bounds, explicit validation,
shared workspace authentication and paginated response conventions.

| Record | Fields and constraints |
| --- | --- |
| `work_items` | `id` (`wi_` opaque ID), `task_id` FK, `kind` (`bug`/`feature`), `title`, `description`, `status` (`open`, `in_progress`, `blocked`, `done`, `dismissed`), `priority` (bounded enum), `revision`, creation/update timestamps and caller/agent attribution; optional validated `source_task_id` + `source_message_seq`, `resolution`, `archived_at`. |
| `work_item_changes` | Item ID, item revision, actor, timestamp, change kind and changed fields. Preserve accountable updates separately from the pruned activity feed. Agent attribution is provenance within the current trusted workspace, not cryptographic per-agent authentication. |
| `work_item_requests` | Stable client request ID, operation, payload hash and stored result; unique scoped key. Atomic with each create/update/dispatch to resolve response-loss retries; same key with changed payload returns conflict. |
| `work_item_dispatches` | Dispatch ID, item ID, item revision/snapshot, target task ID, resolved target agent ID, message sequence, requested-by/time, optional acknowledged/completed timestamps. Accepted means the board message committed; read/done/finished work are separate facts. |
| `project_agent_slots` | Unique `(task_id, role)` where role is `database_handler`; current agent ID, desired state, launch generation/attempt ID, claim/lease state and last error. Hold non-secret launch selection references/snapshot as needed; keep SSH credentials in the vault/host configuration. |

Index lists by project/kind/archive/status with stable ID tie-breakers. Use an
opaque cursor with deterministic ordering; refresh from current rows on feed
events rather than treating events as the entire database.

Suggested endpoints:

- `GET /v1/work-items?taskId=&kind=&status=&cursor=&limit=` for global or scoped
  lists; project scope is optional only for reads.
- `POST /v1/tasks/{id}/work-items`; `GET/PATCH /v1/tasks/{id}/work-items/{item}`.
  PATCH requires the expected revision; return 409 for stale edits. Separate
  disposition (`done`/`dismissed`) from soft archival. No permanent deletion in
  the first release; CRUD removal means an audited soft archive/restore.
- `POST /v1/tasks/{id}/work-items/{item}/dispatch`, containing target task ID,
  expected item revision and stable request ID. Return dispatch and board receipt.
- Add handler-slot reservation/status/claim operations under the existing task
  resource; final route shape belongs to lead's launch contract.

CLI: `tt work-items list|get|create|update|archive|dispatch`, defaulting to
`TAILTERM_TASK`, with explicit `--project` overrides, `--json`, `--request-id`,
`--revision` and `--body-file` for shell-safe descriptions. A create should print
the durable item ID only after commit. Add CLI aliases for Bugs/Features later
only if they reduce friction. Do not grant arbitrary SQL access to agents.

Keep closed projects' rows/history readable. Proposed first-release rule:
closed-project items are read-only; prevent new dispatches involving closed source
or destination projects. Closing a project neither marks its bugs fixed nor
deletes items. A later explicit transfer/reopen/archive policy can address long
lived backlogs; this needs a product decision.

## Intake and Send to project

Agent workflow: use `tt post --to <handler-name>` with kind/title/evidence and
source context. Handler creates the record through `tt work-items create` and
replies to that message with item ID and recorded status. Use a deterministic
request key per source message plus item ordinal so retries do not create
duplicates; allow several distinct items from one message. Similar titles are
not proof of duplication. Ask for missing critical detail without claiming a
record exists before commit.

If handler is unavailable, agents and UI can call the same create API directly
and optionally notify it afterward. Show handler availability and retry launch;
never require an LLM reply before saving. Board messages remain durable, but an
unprocessed request is not yet a structured item. Requesters must receive an
explicit receipt or know to use the direct write path.

Dispatch is a **hub transaction**: validate source item/revision, destination
openness and selected current orchestrator; insert the immutable dispatch
snapshot, a directed board message containing item ID/source project/summary and
a durable UI link, then corresponding events and request receipt. Commit before
notifying waiters. Extract a transaction-aware message insertion helper; calling
`PostMessage` recursively while holding the store lock/transaction would conflict
with its own locking and single SQLite connection.

The message belongs to the destination board; never use a source-project message
sequence as its `replyTo`. Keep source links in dispatch metadata. A missing,
closed or exited orchestrator returns an actionable conflict without claiming
dispatch. An offline existing orchestrator may receive a durable message, with
UI saying delivered to board / agent offline. Retired-agent behavior follows the
actual caller: preserve human direct-message resume rules; never forge a human
sender to wake an agent on an agent-authored dispatch. A dispatch must not
silently reassign ownership, launch processes or mark the item in progress.

The same request ID yields the same receipt. A deliberate resend uses a new
request ID and appears as a new dispatch in history. This distinguishes retry
from new work, and preserves which item revision was requested even after edits.

## Automatic handler launch and lifecycle

- Automatically add one **required project-owned launch member**, visibly
  identified as Database handler in the creation preview and resulting roster.
  Launch it for every new Project through the normal SSH/tmux flow; this is real
  automatic launch behavior, not an optional suggested agent or a placeholder.
  Report overall launch complete only when its launch succeeds, while retaining
  the saved project and records if it fails. It has a durable role slot, not an
  inferred name or ordinary
  helper parent. It consumes one of the 32 open-agent slots but no user-enabled
  helper allowance. Do not weaken helper permission checks to implement it.
- Resolve orchestrator first, then handler, then remaining members. Default
  handler host/runtime/model/cwd/permission/tool settings to the orchestrator's
  resolved launch selections, with a database-specific role/prompt. Do not copy
  unrelated custom command arguments or assignment text blindly. A generic
  runtime/custom launcher may need an explicit handler command. Persist enough
  non-secret launch configuration to retry after page reload; roster alone is
  insufficient to reconstruct model/settings.
- Preflight capacity for normal members **plus handler**, and reserve its slot
  atomically against simultaneous manual/helper additions. A 32-person existing
  template cannot silently become 33: preserve the template and require reducing
  this launch to 31 ordinary members. Existing full projects get a visible
  handler-pending-capacity state; do not kill or retire participants to make room.
- Reserve identity/attempt before launching and reconcile host receipts and
  roster before retry. A lost SSH response may mean the handler already started.
  Extend registration/launch idempotency deliberately: the existing explicit-ID
  shortcut must not start another tmux session for an already-running identity.
  Retry only failed members; retain successful members' folders and sessions.
- Ordinary process restarts retain handler agent identity/inbox, use a new run
  ID, and bind the exact new thread with normal cleanup receipts. Explicitly
  closed identities require a recorded replacement in the role slot rather than
  silent resurrection. Keep host changes explicit.
- Recommend handler stays **done/available** during an open project and ordinary
  work cycles. Update orchestrator briefing so routine worker closeout does not
  unintentionally retire this ongoing role. Explicit owner retirement remains
  respected; direct CRUD still works. Never add perpetual model polling.
- Project creation through a browser can launch using its selected SSH host.
  The new Project UI must collect handler-capable launch selections even when
  no ordinary team is selected; the old no-agent checkbox cannot imply full
  feature completion. Use those selections for the initial orchestrator/handler
  plan, with the exact no-team default settled by lead.
  `tt new-project` or API-only creation without launch inputs cannot promise a
  running handler: create a desired slot in pending setup and surface it. Do not
  invent a hub scheduler or read browser vault secrets. Existing projects should
  gain desired slots through an explicit reconciliation/backfill step, without
  launching unknown host processes merely on database migration.
- Project closure closes the handler's owned session through existing durable
  cleanup; stored work items survive. Handler has no implementation/file ownership
  by default, no implicit helper quota and no authority to close the project.

## Decisions, ownership, and acceptance

Lead should settle: existing-project dispatch (assumed) versus new-project/team;
cross-project dispatch versus same-project only; closed-project backlog policy;
existing-project handler backfill and headless creation behavior; generic-runtime
fallback; and whether priority/status defaults above match the desired workflow.
No new multi-tenant security model is implied by scoped views or handler roles.

Suggested implementation ownership: backend/CLI owns additive schema, validation,
revision/request receipts, atomic dispatch and role-slot contract; launch/UI owns
resolved handler planning, retry/reconciliation, naming changes and two compact
views; integration lead owns compatibility/auth/rollout and final lifecycle rules;
QA owns isolated cross-surface acceptance. No implementation assignments are made
by this document.

Acceptance should cover old task IDs/routes/exports; global/scoped filtering;
CRUD without a handler; replayed create/update/dispatch and stale revisions;
cross-project reply rejection; closed-project races; offline/retired recipients;
one handler after lost response/reload/concurrent launch; 31+1 and full-capacity
cases; helpers disabled/zero allowance; partial retry with preserved folders;
exact new run/thread identity; and project closure retaining items while durable
cleanup completes. Use isolated databases/browser contexts/tmux sockets only.

Verification: read owner #111 from the shared board and inspected the local files
linked above. No external research, tests, implementation edits, helper launches
or live database writes were performed for this design. Remaining dependencies:
lead's final contract/owner decisions and subsequent isolated implementation tests.

## Follow-up #144: minimum durable handler launch/retry recommendation

Read-only review of `client/task-hub.js`, `client/local-vault.js`,
`client/workspace-state.js`, `hub/cmd/tt/main.go`, `cleanup.go` and current
`Store.AddAgent`. Backend/CLI implementation belongs to lead and api. The
recommendation below narrows the broader role-slot design above; an always-on
launch scheduler is unnecessary for browser reload/SSH-response recovery.

**Current hazards:** `launchMembers` progress is an in-memory Set; roster has no
model or permission configuration. `cmdSpawn` chooses `UniqueSession` before
registration and always calls `spawn.Create` afterward. `AddAgent` with an
existing explicit ID returns that record without checking host/session/runtime;
the no-ID path restarts an exited same-name/host/parent record with a new run,
while rejecting another open same-name record. Neither path proves a session
should be created. A create error currently closes the registered agent without
distinguishing certain failure from an ambiguous command response.

1. **Persist browser intent before SSH.** Add a bounded, validated
   `handlerLaunches` map to the encrypted local vault, keyed by normalized hub
   URL and task ID. Store role, stable attempt ID, reserved name, server ID plus
   endpoint identity (host/port/SSH username/tmux target), and resolved
   runtime/model/base command/cwd/permission mode/allowed tools/prompt version.
   Copy the orchestrator's resolved selections, not a mutable team reference.
   Use an awaited `mutate` operation: it clones, encrypts and waits for IndexedDB
   transaction completion before publishing new contents. Do not piggyback on
   `scheduleWorkspaceSave` (150ms debounce), or put it into the tab snapshot that
   `normalizeWorkspace` filters. Expose validated plans through `localData`.
   Missing/changed server endpoints require review before executing the plan.

2. **Keep the first recovery scope local.** The new map need not enter
   `portableData`/profile sync for this MVP; `applyRemoteProfile` should preserve
   it like workspace state. Another browser can see/reconcile the existing
   handler through hub identity, but cannot reconstruct missing launch settings.
   Add explicit setup there. This avoids turning synced profiles into competing
   launch queues. The vault owner lock only coordinates tabs on this origin;
   it is not sufficient for host/process concurrency.

3. **Use one recoverable host operation.** Add handler-specific behavior to
   `tt spawn` (or a small `ensure-handler` command). Hold a process-releasing OS
   lock keyed by hub/task/role and actual tmux execution context. Persist a
   private journal before side effects, using the existing atomic private JSON
   writer pattern. Record attempt/config hash, agent/run IDs and phases such as
   registered, creating and started. Derive one fixed session name per attempt;
   do not find another free suffix on replay. On retry, look for exact
   hub/task/agent/run environment ownership using `localSessions` and existing
   cleanup receipts, including renamed sessions. Return an existing matching
   session; never create a second one just because the browser lost its receipt.

4. **Minimal additive registration semantics.** Keep `Role=database_handler`,
   one unique role per project, and ordinary `ParentAgentID` empty. Add a stable
   `launchId` and optional `expectedRunId` for explicit restart; persist the last
   accepted launch ID/config fingerprint with the role/agent. Replaying that
   launch ID returns the same run. Changed configuration under the same ID,
   another host, or a stale expected run conflicts. A restart of an exited run
   atomically retains agent/inbox identity and creates a new run only once.
   Existing open/retired roles are inspected/reused, never restarted implicitly.
   A closed identity needs explicit replacement policy. API owns slot uniqueness
   and capacity; host journal/lock owns the actual create decision. A response
   returning an Agent is not by itself permission to create a session.

5. **Fail safely in irreducible crash windows.** Write `creating` before tmux
   creation. If that journal exists but no exact session can be found, the command
   may have started and exited before recording success: report an ambiguous
   attempt, not permission to replay its prompt. A known pre-create failure can
   retry the same attempt; a confirmed exit can explicitly start a new run.
   Retain old receipts. Never close an existing registration on a timeout or
   duplicate-session error before reconciling ownership. Report malformed or
   multiple matching sessions as a blocker instead of choosing one arbitrarily.

The role still occupies one of 32 open-agent slots and does not consume helper
allowance. Add it to the real browser launch plan. Existing projects without a
saved plan get “Set up database handler”: prefill known host/runtime/cwd, ask for
missing model/settings, and label host-default model honestly if selected. Do
not inspect private runtime configuration or claim exact inheritance from fields
the roster never stored. API-only project creation can expose pending setup;
it cannot execute a browser-owned SSH plan.

Required isolated checks: vault write failure prevents SSH; reload before/after
registration and after tmux creation; lost SSH/API response; concurrent retries;
renamed/reused session names; same attempt with changed payload; explicit exited
restart replay with one new run; retired/closed role handling; old run receipts;
different SSH account/socket/host; missing vault plan; 31+handler capacity and
zero helper allowance. The host cleanup currently distinguishes older-run
receipts from current-run acknowledgement, so preserve that behavior.

No tests were run for this read-only recommendation. A role-only registration
change without the host journal/reconciliation path is insufficient to claim
safe durable launch retry.
