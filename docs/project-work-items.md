# Projects, Bugs, and Features

Projects is the user-facing name for the existing task hub. Existing `tsk_` IDs,
`taskId` fields, task environment variables, history exports and `/v1/tasks`
routes remain compatible. The Bugs and Features tabs store records in the hub,
independently of whether agents are online. Files remains hidden.

Projects, Teams, Bugs and Features use Board's full-height divider, boxed left
selectors and compact controls. Projects and Teams show the selected project or
template on the right, with their existing actions. Bugs/Features list open
projects on the left, All projects and a secondary closed-project list. On narrow
screens the rail scrolls across the top and a labelled project filter remains
available. The rail's + button creates a record for that view.

## Slow and offline connections

Board, Projects, Bugs and Features show previously saved reads immediately while
refreshing from the hub. The saved/offline label describes that local copy.
The cache is encrypted in this browser's vault, isolated by hub and credential,
and excluded from profile sync and portable exports. It retains up to 100 reads
or 4 MiB for at most seven days, evicting older entries. An unvisited view or a
new browser origin may need a connection before it has anything to show.

Board refreshes normally fetch only messages newer than the saved conversation.
Terminal reconciliation, launches and cleanup still require authoritative hub
reads. Changes and dispatches require a confirmed hub response: failures retain
the form draft and are never queued or replayed automatically. Teams already
live in the local vault and can be edited while the hub is unavailable.

This is a data cache, not an offline installation of the app: the page and its
runtime assets must still be available. A browser-origin change also does not
copy terminal layout. To reattach a project, use Board → project → Terminals,
or Projects → More → Open terminals; existing agent sessions are reused.

## Recording and assigning work

Each bug or feature belongs to one project. Both tabs can show all projects or
filter by owning project and status. New items start Open with Normal priority.
Items can be edited and marked In progress, Blocked, Done or Dismissed. Closed
projects and their items remain readable; they cannot be edited or dispatched.

Send to project sends the selected item revision to an existing open project's
orchestrator. The owning project is selected initially. Choosing another project
does not move the record, start a team or change its status. A successful receipt
means the directed board message was saved, not that an agent has read it or
finished the work. Missing or closed orchestrators produce a visible error.

Create and dispatch requests retain an idempotency key across response-loss
retries. Reusing a key with different input conflicts. Edits and dispatches
check the expected record revision so stale forms cannot overwrite newer work.
An agent-authored dispatch is limited to its own project; human UI dispatches
can target another project. Sender and source-message attribution are retained.

## Feature history and reports

Every feature has a reloadable **History & report** reader. It combines the
feature's immutable work-item revisions and explicitly retained messages with
narrative artifacts, links, coverage declarations and full report revisions.
It does not infer relationships from titles or prose. Missing PR or CI history
is shown as not ingested with unknown extent until an attributed declaration is
saved. External locations are references only: the hub neither crawls nor
embeds them.

Narrative writes append immutable versions and advance a sequence independent
of the work-item revision. Artifact corrections and link retractions preserve
the original version. Stored content has a server-computed digest; a submitted
digest remains separately labelled. Capture state, availability, declared
coverage and evidence assessment remain distinct, so successful storage is not
presented as successful execution or independent verification. Passed, failed
and independently verified assessments must cite exact evidence versions and
retain the verifier separately from the declaration author. AIV entries are
opaque evidence references and do not activate or duplicate that service.

List endpoints return metadata in frozen pages of 32 entries by default and at
most 64. Continue with the returned opaque cursor; entries appended after the
first page are excluded from that traversal. Fetch exact artifact and report
versions separately to read their full content. Stored narrative text is UTF-8,
limited to 1 MiB of decoded content per version and never truncated; JSON
escaping uses the larger transport bound. A report or coverage declaration can
carry at most 384 KiB of aggregate reference metadata; the HTTP envelopes cover
the worst-case escaping of both legal content and references. Metadata responses
stop below 3 MiB and return an advancing cursor instead of truncating a record
set. Closed projects retain reader access but reject new narrative writes.

A feature can transition to Done only after a complete structured report has
been stored, read back and pinned by report ID, exact version, server digest and
current scope revision. This check applies to keyed and legacy update routes.
Changing title or description advances the scope revision and makes an older
report stale; status and priority changes do not. Bugs are unaffected. Existing
Done features remain Done and show their retained legacy description together
with an explicit missing-dedicated-report warning until real source material is
deliberately imported. Later report corrections do not rewrite the report pin
that originally completed a feature. The reader shows the latest correction by
default while keeping the completion pin and every exact historical report
version selectable.

The API root is
`/v1/tasks/{task}/work-items/{item}/narrative`. Its overview, timeline,
artifacts, links, coverage, reports and receipts resources use request IDs,
expected-version checks and exact retry receipts. The CLI mirrors them under
`tt work-items narrative`; use `--file PATH` (or `--file -`) for artifact,
link, coverage and report writes. `tt work-items update` accepts
`--report-id`, `--report-version`, `--report-digest` and
`--report-scope-revision` when completing a feature. Agent database workflow
still routes these operations through the project's Database handler.

## Database handler

New projects launch their orchestrator, a visible Database handler, then the
remaining team members. The handler inherits the orchestrator's resolved host,
app, model, folder and permission settings. Supported apps use their normal
command with handler-specific instructions; custom apps retain their explicit
command. It occupies one open-agent slot but does not consume the additional
helper allowance. A new project can therefore launch at most 31 ordinary members
plus its handler, subject to the hub's per-project active-agent capacity.

All agent implementation, investigation, validation and deployment requires a
durable bug/feature ID and a bounded work order. Intake and board/inbox/roster
coordination can establish the record first. Agents send all work-item database
requests, including list/get/create/update/dispatch, to the handler's actual
roster name. An unavailable or missing handler requires authorized setup/resume
or an explicit dependency; agents must not bypass it through CLI, API or database
files. Human UI access remains available. This is an agent instruction and audit
workflow, not new API authorization enforcement.

The handler preserves source context, source-message sequences, stable retry keys,
body files and expected revisions. It reads back committed records before returning
IDs/revisions and work orders with owner, scope, owned files/artifacts, acceptance
checks and dependencies. Assignments and results cite the item and work-order
message. Scope changes go through the handler before additional work. The handler
retains assignment/result links and verification alongside the original owner
text, and confirms completion only after accepted verification and resolved
required dependencies. The handler stays available during ordinary project work
cycles; explicit retirement remains respected. Closing a project
includes the handler in the existing durable session-cleanup process and keeps
the saved records.

Each implementation worker is dedicated to one bounded bug or feature. After
the handler's recorded result and lead acceptance resolve its dependencies, the
worker is closed through its exact run and durable cleanup receipt while the
project stays open. A later item receives a fresh identity and context. Retirement
means intentional temporary retention for same-item follow-up, not completed-item
closeout.
Useful long-lived services descended from a worker's tmux session must be handed
off or detached and reverified before closeout; exact cleanup terminates the
remaining descendants.

The browser saves the handler's resolved launch plan and stable agent identity in
its encrypted local vault before SSH launch. Retry uses that identity. Host-side
recovery must verify the exact project, agent and run before reusing a session;
ambiguous interrupted launches require inspection instead of starting another
process. Successful members of a partial team launch retain their sessions.
An edited launch candidate retains the previous settings until SSH confirms its
launch. If an attempt fails, reopening handler setup offers Restore previous
settings. Recovery never discards the previous plan merely because a request was
sent or its response was lost.

Existing projects and projects created through the API without host selections
show Set up database handler. The hub cannot launch SSH processes itself. Missing
model or permission selections use visibly stated host defaults; missing saved
machines must be selected explicitly. Saved handler plans are local recovery data,
excluded from portable profile sync and exports. Moving browsers may therefore
require reviewing the handler's launch settings again.
For an existing handler, the roster's host, runtime and folder take precedence
over the orchestrator's settings when local launch selections are unavailable.

## Ownership and compatibility

The hub owns work-item records, revisions, dispatch receipts and board messages.
The browser owns forms and local launch selections; the handler's conversation is
not the database. Project identity, task-owned pane groups, guest boundaries,
exact runtime threads, retirement and cleanup receipts retain their existing
meaning. Exact terminal arrangements are encrypted local workspace data keyed by
project/agent identity, independent of transient pane IDs; they do not change or
resume native agent processes. Work-item storage is an additive migration; it does not change encrypted
profile contents or create a new multi-user authorization model.

See `tests/project-work-items-browser.mjs` for isolated browser acceptance and
`tests/project-handler-vault-browser.mjs` for encrypted launch-plan persistence.
Narrative storage/API acceptance is in `hub/internal/store/narrative_test.go` and
`hub/internal/server/narrative_test.go`; the safe reader and reload contract is
covered by `tests/narrative-history-browser.mjs`.
