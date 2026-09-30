# Local Planned delivery team launch

## File scope before admission

During intake, the database handler reads the owner-filed item and bounded
order. If the item already states the ordered acceptance and owned files, the
handler confirms that exact item revision, scope revision and order:

```sh
tt work-items scope confirm --project tsk_... --revision 2 --scope-revision 2 --order 123 --request-id intake-123 --complete --owns hub/internal/store --owns docs/team-launch.md wi_...
tt work-items scope get --project tsk_... --revision 2 --order 123 wi_...
```

`--owns` (repeatable) records the repository-relative files and directories the
plan or intake says the item will change. The hub saves them canonically with
the confirmation and returns them from `scope get`; they are part of the retry
identity, so a retry with different paths is refused. `tt team queue add`
without `--owns` then takes exactly these paths, so nobody scopes the entry by
hand. `--owns` is optional for older handlers; without it the add needs its own
`--owns` or `--serial`.

The handler runs these commands from its own agent session. The `--complete`
flag is its explicit semantic assertion; the hub checks the handler's exact
run and the immutable order link. If the owner filing is incomplete, the
handler records the missing scope through the ordinary work-item update path
and confirms the new revision. No extra owner confirmation is needed for a
complete filing. Queue add, manual launch, TailOS planning and final agent
registration refuse missing or stale confirmation before admitting a team.
An actual scope edit advances the item revision and needs a new confirmation.

For an order, sequencing note or decision that changes no item field, the
handler saves a separate receipt. This leaves item and queue revisions, the
selected order, prepared contexts and Start evidence intact:

```sh
tt work-items scope save --project tsk_... --revision 2 --order 123 --source 124 --kind sequencing_note --queue-entry tqe_... --request-id note-124 wi_...
tt work-items scope receipt --project tsk_... --request-id note-124 wi_...
```

For an admitted team, pass `--admissions-file` containing a JSON array of the
exact current `{agentId,runId,contextDigest}` bindings. A queued item has no
admitted bindings and needs no file. Retrying an unchanged request ID returns
the same receipt, including after a lost response or hub restart. A changed
payload or mismatched source, queue, run or digest is refused. A decision that
changes acceptance or owned files uses the ordinary revision checked update;
the bookkeeping path cannot edit item fields.

## Agent window size

Agent tmux sessions keep a fixed 200x50 window whatever attaches to them (bug `wi_b6b79229c8fec99d`, order #14836). `tt spawn` sets the session's `default-size` and its window's `window-size manual` in the same tmux command that creates it. The relay restores that policy on older Tailterm sessions. TailOS also attaches agent tiles with `-f ignore-size` and never sizes a hidden tile, but the manual window size is what protects agents. Global tmux options are not required or changed. Details are in [claude-wake.md](claude-wake.md).

## Project team queue

On the launch host, the owner can save a sequence of recorded item orders:

```sh
tt team queue add --task tsk_... --item wi_... --order 123 --template planned
tt team queue add --task tsk_... --item wi_... --order 124 --cwd /absolute/worktree --owns client --owns hub/internal/store
tt team queue add --task tsk_... --item wi_... --order 125 --new-worktree --owns docs/team-launch.md
tt team queue add --task tsk_... --item wi_... --order 126   # ownership from the handler's scope confirmation
tt team queue add --task tsk_... --item wi_... --order 127 --serial   # declares nothing; runs alone
tt team queue policy --task tsk_... --policy-version 1 --expires RFC3339 --sessions N --polling N --bindings N --requests-per-minute N --burst N --headroom-percent N [--min-free-disk-mib N]
tt team queue limit --task tsk_... --limit none   # or --limit N; --limit 1 is serial
tt team queue scope --task tsk_... --entry tqe_... --owns client/team-delivery-view.js --owns tests/parallel-team-browser.mjs
tt team queue fail --task tsk_... --entry tqe_... --reason 'stuck launch'
tt team queue integrated --task tsk_... --entry tqe_... --commit FULL_SHA [--evidence 'release record']
tt team queue list --task tsk_...
tt team queue replace-lead --task tsk_... --entry tqe_... --lead-agent agt_...
tt team queue accept --task tsk_... --entry tqe_... --worktree /absolute/builder/worktree --branch feature/name --commit FULL_SHA --evidence 'handler-saved acceptance receipt'  # recovery only
tt team queue reorder --task tsk_... --entry tqe_... --before tqe_...
tt team queue remove --task tsk_... --entry tqe_...
tt team queue release --task tsk_... --entry tqe_...
tt team queue abandon --task tsk_... --item wi_... --order 123
```

The default project limit is one: the serial queue. `--limit none` removes the
fixed cap, and `--limit N` sets an optional owner ceiling; `list` prints
`concurrency limit=none` and the Delivery panel says **No fixed limit**. Any
limit other than one needs a fresh, versioned owner policy whose session, project polling, relay binding, request
rate, burst and headroom caps cover the host's hub limiter domain. The CLI
derives the domain from the configured hub origin. No safe production values
are inferred. The host CLI inventories every local relay binding file before
raising a limit, and the relay refreshes this census before parallel queue
effects. An unreadable binding, another unbudgeted domain, missing or stale
census, or incomplete policy stops new parallel launch effects; it does
not kill running teams. The relay rotates the first polled project and backs
off after a hub 429; unrelated inbox delivery continues. A shared transport
bucket charges actual queue and binding HTTP requests, with reserved shares
for each path so a queue burst cannot starve inbox checks. Admission projects
80 requests per minute and six burst tokens per binding, plus 20 requests per
minute for the host queue scan and up to six effects per polled project. This
bounded worst-case estimate must fit both the total cap and its reserved
queue/binding shares (one quarter and three quarters); the transport bucket
enforces actual rate and burst. Reservations include every uncleaned agent run,
uncertain and pending members, extras and manual launches (five slots before
a manual plan is known).

Without a fixed cap, admission is governed by real constraints only:
non-overlapping declared ownership and worktrees, a free database handler for
each running entry, the host policy's session, polling, binding and request
budgets, and a free-disk reserve. The runner reports the least free space
across the host's queue worktrees with each census (a worktree that no longer
exists is skipped; with none it reports the invoking checkout's filesystem)
and writes a census early when the reserve comparison flips. A new parallel
claim is refused with `host free disk N MiB is below the M MiB reserve`, or
`host free disk is not observed` when an older `tt` sent the census; the
reserve is `--min-free-disk-mib` or 8192 MiB when unset. The reserve gates new
admission only (claim, the queued entry's list reason and raising the limit);
a launch already under way still starts its members. Provider usage limits
are not consulted yet: no provider-limit signal exists, so that headroom
waits for usage-limit detection (`wi_72f41bd375032cf0`).

To lower the limit, run `tt team queue limit --limit 1` (serial) or
`--limit N`; it is refused while more entries than N are active, so wait for
them or release failed ones first. Before rolling the hub back to a build
older than the uncapped queue, set `--limit 2` (or 1): an older hub reads
`none` as every slot reserved and stops launching until it is changed.

The owner provisions
additional database handlers as ordinary continuing handler agents; the runner
never starts or resumes them. Each running item leases a distinct exact
handler ID and run. A retired or offline handler is unavailable for a new
lease. Existing one-at-a-time projects retain their primary handler.

Use one canonical worktree per parallel item. In a parallel project, `add`
without `--cwd` creates one: from the invoking Git worktree root it runs
`git worktree add --detach <repository root>/.build/worktrees/queue-<first 8
hex of the item ID> HEAD` and queues that folder, so the repository, base and
ownership checks apply unchanged. It checks ownership first, refuses an existing path
(queue from it with `--cwd` instead), and removes the new worktree on any
failure before the hub saves the entry, so the add can be retried. `--new-worktree` does the
same in a serial project; `--no-new-worktree` keeps the current checkout.
`--owns` accepts repository relative files and directories; an ancestor directory overlaps its descendants,
while sibling directories do not. An entry either declares ownership or is
serial: in a parallel project an add with no `--owns`, no ownership in the
handler's scope confirmation and no `--serial` is refused before any worktree
is created, and the hub refuses an unscoped add that is not serial. A serial
project's unscoped adds are serial implicitly, so they behave as before. A
serial entry conflicts with every other team; `list` shows it as
`owns=serial (runs alone)`, and an entry saved before serial existed as
`owns=unscoped (legacy)`.
The queue command rejects traversal, absolute or dot segments, symlink paths,
and case aliases. It records the shared repository identity and starting commit
across worktrees. `list` and the Projects Delivery panel show ownership,
blockers, item lead, handler, and limit. When slots are available, the
runner takes queued items in order and skips one blocked by active ownership
before trying the next. It keeps distinct worktree and handler leases. A
queued entry's reason names the first real limit it waits for: slots, host
capacity or disk, `No free database handler`, `Serial: runs alone once the
active teams finish`, or, for a legacy entry, `Unscoped: declare ownership to
run beside other teams`. When the wait is a stall, the reason starts with
`Stalled:` (see below).

`scope` replaces an entry's declared ownership with a non-empty canonical
list and increments its revision; a retried request returns the saved result.
The owner may scope any queued, launching or running entry from an unbound
CLI. From an agent session, any available database handler may scope a queued
entry, and the leased handler or the item's current lead may scope its own
launching or running entry; the hub checks the exact agent and run like
`accept`. An admitted entry may narrow its paths, but widening into another
active entry's paths is refused with `ownership overlaps active entry tqe_…`.
The planned lead narrows its entry to the plan's owned files once the plan
freezes, and the handler records intake ownership with `scope confirm --owns`.
Scoping a serial entry makes it an ordinary scoped entry. The owner may also
scope a failed entry that still holds its slot, but only to narrow it: every
new path must lie under a path it already holds.

The hub stores queue state and retry receipts. The supervised Mini relay checks
the queue on each tick, including when no Codex thread is bound. It records a
launch reservation and frozen plan before effects. Each queued lead is scoped
to its own item. The owner can transfer that lead to a live exact member of
the same item with `replace-lead`; the other item lead remains untouched.
Uncertain member spawns are reconciled by exact identity and owned session;
unresolved attempts fail the entry and require owner action. Terminal items
advance only after the exact team close and host cleanup receipts. A failed
entry keeps its slot and posts one durable owner escalation. Unrelated teams
continue; ownership blocked by the failed entry waits for an explicit verified
release. Pausing the project stops launch ticks. The deliberate agent Queue is
separate.

The exact assigned database handler records the accepted builder worktree,
branch and full SHA in the same save that marks the item done:

```sh
tt work-items update wi_... --revision N --request-id KEY --status done --worktree /absolute/builder/worktree --branch feature/name --commit FULL_SHA
```

tt checks the Git tuple on the host first; the hub then applies exactly the
`team queue accept` rules (exact leased handler run, verified base and
repository for an enrolled item, accepted review candidate, saved item
revision and completion report) in the save's own transaction, and enqueues the
release job for an exact-SHA verification receipt. Any refusal leaves the item
open and the entry unaccepted. The evidence defaults to the saved completion
receipt. When the receipt lets a bug close with known failures that name it now
passing, the acceptance records them as `resolvedKnownFailures`; the hub
derives that field and refuses a client-supplied one. While an entry waits, the hub refuses a handler done save without the
tuple, so acceptance is no longer a separate step. A retried save with the same
request key replays its receipt without a second acceptance or release job.

`team queue accept` stays as the idempotent recovery path, for example after
an owner saved the item done: re-running it with the saved tuple changes
nothing, and a different tuple is refused. Its retry key is stable and pins the
exact item revision and completion report. The runner waits for the acceptance
and exact team close and cleanup receipts. It verifies that the
accepted worktree still has the saved branch, commit, repository, base ancestry
and a clean tree; a later HEAD cannot replace the accepted SHA. Then the item
becomes **Ready to integrate**. The saved tuple and evidence appear in the
queue and Projects Delivery panel. The
owner performs any merge, push or deployment separately. Dismissed items have
no integration record.

Both primary and auxiliary handler briefings include the done save with its
acceptance tuple and the recovery command. A done repository-backed entry without its receipt remains running and
the queue list and Delivery panel say **Waiting for handler acceptance**. If
the host census or limiter domain fails, the runner reports that error and
holds new parallel launch effects while serial projects and already-running
teams continue their safe close and cleanup paths. The host's capacity checks
still refuse new parallel work until current matching evidence is available.

In a parallel project the runner releases a failed entry itself on its launch
host, under the same launch lock and checks as `release`, once every run bound
to its item is closed and cleaned; it checks the roster first, so an entry
that cannot be released costs no hub write. The failure and escalation stay.
Until then its list reason counts the item-bound runs still live or uncleaned.
A serial queue still halts until the owner reconciles. For an entry that is
otherwise stuck, `fail --entry --reason` (owner only, unbound CLI) fails a
queued, launching or running entry with the owner escalation; after its team is
closed, the release follows. For an entry whose candidate the owner integrated
or released outside the queue, use `integrated` instead (next section).

After inspecting a failed entry, use `release --entry` to clear its reservation
and let the next queued item run. The failed entry and escalation remain in
the list with a release timestamp. Release is refused while its team is live
or cleanup is pending. For an attempted spawn with no hub registration, run
`release` on the saved launch host: it locks out a concurrent launch, checks
that no matching owned session remains, and sends the exact frozen agent and
run identities for a final hub recheck. If it cannot verify absence, release
refuses. Release never restarts the failed item.
If a manual launch stops, `abandon --item --order` releases its exact reservation
with a retry receipt. After lead selection, first clear the lead and resolve
every member through the saved journal: registered runs need close and cleanup
receipts, and an uncertain unregistered attempt needs a verified absence of
its local session. `abandon` locks the journal, checks local sessions, and sends
the exact member identities to the hub. It refuses a live team or pending
cleanup. Both commands are owner-side operations.
An abandoned manual attempt cannot replay its old launch identity; queue the
still-active item with `team queue add` if it needs a fresh team.

`tt team launch --item wi_… --order N [--template planned] [--dry-run]`
starts the four non-database members of the Planned delivery template for an
existing project. `TAILTERM_HUB` and `TAILTERM_TASK` select the hub and project;
`--hub` and `--task` can supply them explicitly. The current directory is the
members' project folder, or pass `--cwd` for an absolute local directory.

The project must have an available database handler. In TailOS, use **Projects →
Set up database handler** first. The command reuses that handler and omits the
template's database member. It refuses a live orchestrator for another item,
and requires the exact active item revision and explicitly linked primary work
order. Run it from the owner's unbound CLI shell; an agent-session launch needs
handler-authored allocation intents and is refused by this command.

Use `--dry-run` first to see the resolved names, runtimes, models, reasoning,
UTF-8 prompt byte sizes and local folder. Dry-run reads the hub and writes no
project or launch state. Node.js is required locally to execute the generated
copy of the same planner used by TailOS. `npm run build:team-plan` regenerates
the embedded module; `npm run check:team-plan` rejects stale generated code.

For execution, the CLI freezes identities, full prompts and the prepared item
context in a private journal under `~/.local/state/tt/team-launch`. It sets the
item-scoped lead as project orchestrator before starting any member, then runs
the existing `tt spawn` path in order. Output gives each name, agent ID and run.
After an uncertain response, repeat the identical command. It reconciles exact
saved IDs before attempting an unstarted member and refuses conflicting or
closed identities. Do not delete a partial journal to force another launch;
inspect the project and reconcile the original identities first.

The command launches on the host where it runs. It does not select remote saved
servers or read an encrypted browser profile. Tests use local test hubs, isolated
home directories and private tmux sockets. This feature does not merge, deploy
or release the source.

## Queue chores the product handles

**Narrowing after acceptance.** Once the handler's acceptance is saved on a
running entry, the runner narrows the entry's ownership, in one `scope` write,
to the files the candidate changed that lie under its declared ownership (a
serial entry keeps them all). It uses the merge-base diff
`git diff --name-only <acceptance base>...<commit>`, so a branch is charged only
with its own files even when the base moved on. Queued work that overlapped only
unchanged files can then start while the team closes. A Git error or an empty
result is logged and skipped; it never fails the entry.

**Owner integration.** `tt team queue integrated --entry E --commit SHA
[--evidence TEXT]` records that the owner integrated or released the entry's
candidate. It runs from an unbound CLI only. The CLI checks that the commit is
in the entry's repository and descends from its base, and lists the files it
changed (merge-base diff, under the entry's ownership). The hub accepts a
running entry or a failed entry that still holds its slot, and in one
transaction saves the owner-integration record, sets the entry's ownership to
the changed files, marks it released and deletes its launch reservation. The
entry's slot, handler lease and ownership are free at once: queued work that
overlapped it claims on the next tick. The item, its revision, its bound
members and its lead stay as they are, so post-release checks continue and
`role:database_handler` messages linked to the item still reach the handler
that served it. `list` shows the entry as `(owner-integrated)` with the commit.
It is refused for queued, launching, finished or already released entries, for
agent sessions, for a failed entry with an uncertain spawn (use `release`), and
while a release job for the entry is `verified`, `claimed`, `merged` or
`blocked`: the deployment path owns that candidate.

After it, the handler saves the item done with a plain
`tt work-items update … --status done`; a save that carries
`--worktree/--branch/--commit` is refused and names the integration, and no
leased run is needed. Once the item is terminal the runner closes the team (or
uses the lead's own `tt close --team` receipt), cleans up every member on the
host and marks the entry `finished`, running or failed, with `releasedAt` and
`ownerIntegration` kept and no integration snapshot. After `integrated` the
entry no longer protects its files, so a post-release fix is filed as a new
item and queued, not made by the still-live team. In a serial project
`integrated` records the integration and frees the reservation and ownership,
but the next serial launch still waits for the project lead slot, which frees
at the lead's team close once the item is terminal.

**Stalls.** The queue list explains a queued entry that waits only on
something nothing will clear by itself. Its `stall` field names the cause, the
blocking entry, the supported fix command and since when it has held (a
durable time from saved records, so a hub restart does not reset it), and its
reason starts with `Stalled:`. Causes:

| Cause | Blocker |
|---|---|
| `failed-entry` | a failed entry that still holds its slot; in a parallel project only while its item still has live runs (with none, the runner releases it) |
| `nothing-running` | a launching or running entry with no live item-bound run for the 5-minute grace |
| `idle-entry` | a running entry whose live members are all idle, silent-finished or done, with no open work held or sent by them, for 30 minutes |
| `no-handler` | no online, non-retired database handler exists |
| `serial-halted` | a failed entry halts a serial queue |

A stall also covers slots or handler leases that are full only because stall
blockers hold them. Waiting on a working team (an ownership overlap, full slots
or every handler leased), host capacity or disk is ordinary queueing, never a
stall. After a stall has held for five minutes, the runner posts one Board
NOTICE from `team_queue/runner` naming the queued entry, its item, the blocker,
the cause and the fix. It has no recipient, no escalation ref and no
obligation. The hub recomputes the stall and refuses a stale or cleared one;
one notice covers every entry behind the same blocker, and retries, later ticks
and restarts add none. A new blocker revision may post once more if the stall
persists.

**Blocks on a queued fix.** The broker does not escalate to the owner an
obligation that is blocked on work already in the queue; see
[message-broker.md](message-broker.md).

**Rollback.** The hub adds three columns (`team_queue_entries.serial`,
`team_queue_entries.owner_integration_json`,
`work_order_scope_confirmations.ownership_json`); existing rows read as not
serial, with no owner integration and no intake ownership. An older hub
ignores the new columns. It counts an owner-integrated
running entry as active again (more conservative) and ignores an
owner-integrated failed entry, which stays failed and released until the new
hub returns. Unscoped parallel adds become possible again under the old hub.

## Independent verification

New Planned delivery templates include a fifth item member, `verifier`, distinct
from builder and reviewer; the project database handler remains shared. Host
capacity conservatively reserves five sessions/bindings per pending team. Saved
older launch plans retain their frozen members and retry identities. A frozen
candidate needs a handler-approved full matrix plan and independent receipt before
lead acceptance, handler completion and queue integration acceptance. See
[objective verification](objective-verification.md). These template changes apply
to new briefings; they do not rewrite running threads or saved launch plans.

Owner decision #11866 places mandatory verification at new item-team admission.
The admission transaction saves an immutable exact agent/run enrollment marker;
no launch option disables it. Rollout retains existing bindings with explicit
legacy provenance. A new team on an existing item becomes mandatory; frozen
pre-rollout teams keep their completion path. The handler can inspect provenance
with `tt verification enrollment --item ITEM`. Before saving a matrix plan, it
requires the separate owner-authored token-only approval described in
[objective verification](objective-verification.md).
