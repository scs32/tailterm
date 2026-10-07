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

Agent tmux sessions start at a manual 200x50 window (bug `wi_b6b79229c8fec99d`, order #14836). `tt spawn` sets the session's `default-size` and its window's `window-size manual` in the same tmux command that creates it. The relay restores that policy on older Tailterm sessions. TailOS also attaches agent tiles with `-f ignore-size` and hidden tiles never initiate sizing, while an eligible foreground focused ordinary agent pane may explicitly size the manual window with a minimum of 80x24 usable program dimensions (bug `wi_9e5d8194ed093cd4`, owner order #24075, implementation #24163). Most recently host-accepted focus wins; hidden/background panes cannot initiate claims, and stale resize/release tokens cannot override a newer viewer. Owner decision #24371, saved by handler #24387 at revision/scope 5, permits only an already-submitted eligible claim to arrive after hide/dispose, keeping at least 80x24 and releasing authority; this does not prove cancellation, and residual `wi_1b1a86c6be5b6bcc` remains held. Owner helpers and human shells are excluded. Global tmux options are not required or changed. Details are in [claude-wake.md](claude-wake.md).

What a launch host needs (tmux, `tt`, runtimes and logins, hooks, the relay) is
listed in [host-requirements.md](host-requirements.md); `tt doctor --role agent`
checks a machine against it read-only.

## Project team queue

On the launch host, the owner can save a sequence of recorded item orders:

```sh
tt team queue add --task tsk_... --item wi_... --order 123 --owns client   # lane by default: small for a bug owning at most three paths, else planned
tt team queue add --task tsk_... --item wi_... --order 123 --template planned --planned-reason schema   # a small bug on Planned delivery needs its reason
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
uncertain and pending members, extras and manual launches. Before its plan is
frozen, a reservation charges its item's team size: six slots for a feature and
five for a bug. The next admission has no item yet, so it charges six. Once a
plan freezes, it charges its actual unstarted members.

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
no integration record. Once the release lands, the runner removes the finished
entry's worktrees ([worktree cleanup](#worktree-cleanup)).

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
starts the non-database members of the Planned delivery template for an
existing project: six for a feature (lead, planner, plan-reviewer, builder,
verifier and reviewer) and five for a bug, without the plan reviewer. The kind
comes from the exact item revision in the prepared launch context, and the dry
run header names the shape, for example `Planned delivery (feature: plan
review)` or `Planned delivery (bug: plan only)`. `TAILTERM_HUB` and `TAILTERM_TASK` select the hub and project;
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

## Worktree cleanup

A finished team's Git worktrees, and the caches inside them such as the Go
module cache under `.build/go`, are removed on the launch host once the
accepted work is safe elsewhere. Team closeout and the sweep, run by hand or
by the relay on a schedule, use the same rules. A worktree is removed only when no keep reason applies; the first
matching reason is reported:

- `locked`: `git worktree lock` is set.
- `missing`: the directory is gone; applying runs `git worktree prune` and
  reports it as `pruned`. A gone worktree whose HEAD is not integrated (see
  `unpushed`) is kept `unpushed`, and while one exists nothing is pruned: Git
  prunes every gone worktree at once, and a worktree HEAD keeps its commit
  from garbage collection.
- `in-use`: the worktree holds, or a linked worktree holds it within, the cwd
  of a queued, launching, running or unreleased failed entry, that entry's
  accepted worktree, or the cwd of a non-closed agent on this host; or it lies
  in a Claude scratchpad (`<temp>/claude-<uid>/<cwd key>/<session>/scratchpad`)
  of a session started in one of those paths. The sweep reads every
  project's roster and queue. Closeout reads less: its own project's roster
  and queue, plus the host queue list, which holds other projects' queued,
  launching and running entries but not their failed ones or agents.
- `nested`: it contains a linked worktree, or any other Git repository (a
  `.git` entry below its root, such as a separate clone), that is not removed
  earlier in the same pass. `git worktree remove` would delete such a
  repository with everything in it when it sits under an ignored path.
  Deepest worktrees are handled first, so removable children go before their
  parent.
- `recent` (sweep only): its Git `HEAD`, `index` or `logs/HEAD` changed within
  `--min-idle`.
- `evidence`: its path, absolute, repository-relative or `~/`-relative, is
  cited by a tracked `docs/` file on `tasks-hub` or by a queue entry's
  acceptance or integration evidence.
- `operation`: a rebase, merge, cherry-pick, revert or bisect is in progress.
- `dirty`: `git status` shows tracked changes or untracked files that are not
  ignored. Ignored caches do not count.
- `unpushed`: HEAD is neither an ancestor of `tasks-hub` or
  `origin/tasks-hub`, nor contained in any remote-tracking ref, nor on a local
  branch whose commits `git cherry` finds patch-equivalent on `tasks-hub`
  (releases integrate by cherry-pick). A detached commit on no ref is always
  kept, so garbage collection cannot drop a receipt's SHA.

Removal re-checks the lock, operation, status, HEAD and nested repositories
immediately before it, adds owner write permission to read-only directories
inside the worktree, and then runs `git worktree remove` without `--force`.
A change found by the re-check keeps the worktree with that reason (or
`moved` for a new HEAD). A failed removal is kept as `remove-failed` and the
directory permissions are restored. Branches are never deleted: an accepted
branch and commit still resolve after their worktree is gone. Closeout and sweep share a host lock in
the relay state directory. While a sweep holds it, closeout skips its look
without a message and returns to the entry within 15 minutes.

**Closeout timing.** On each tick the runner looks at the project's finished
entries on its host that have a saved acceptance. If none of the entry's cwd,
accepted worktree or integration worktree exists, that costs only `os.Stat`.
Otherwise it reads the project's roster, at most once per 15 minutes per entry,
and considers the entry's recorded worktrees plus, when a recorded worktree or
a team cwd is a linked worktree, the worktrees nested in it and in scratchpads
of sessions started there. A shared main-checkout cwd attributes nothing
further; a worktree nested in a considered one still keeps it `nested`. The cleanup makes
no hub write. Because integration happens after the entry finishes, a worktree
is usually kept `unpushed` at first and removed on a later tick once its
release lands on `tasks-hub`. A finished entry is examined only while its
project still has an active entry on the host; dismissed items, owner-integrated
failed entries and anything left over go to the sweep.

**Receipts.** Every outcome is appended as one JSON line to
`worktree-cleanup.jsonl` in the relay state directory
(`~/.local/state/tailterm/relay`, or `TAILTERM_RELAY_STATE`):
`{at, source, taskId, entryId, itemId, path, branch, head, action, reason,
detail}` with `source` `closeout` or `sweep` (also for a scheduled run) and
`action` `removed`, `trimmed`, `kept` or `pruned`. A kept outcome is written once per path, reason and HEAD per process,
and closeout also logs new outcomes to the relay's stderr as
`[tt relay] worktree cleanup <entry>: ...`.

**Sweep.** For worktrees closeout does not reach:

```sh
tt team queue sweep-worktrees [--apply] [--json] [--min-idle 24h] [--accepted-after 24h] [--artifacts DIR] [--cwd DIR] [--hub URL]
tt team queue sweep-worktrees --journal [--limit 20] [--json]
```

- `--apply` removes the `would-remove` worktrees and session temp folders,
  trims the `would-trim` caches and prunes missing worktrees; without it the
  sweep is a dry run.
- `--journal` prints the scheduled sweep's journal instead (see "Scheduled
  sweep"). It reads only this host's file, and cannot be combined with
  `--apply`.
- `--json` prints the result as JSON instead of lines.
- `--min-idle` (default 24h) keeps worktrees whose Git state changed more
  recently, and session temp folders changed more recently.
- `--accepted-after` (default 24h) removes an accepted, unreleased item's
  verifier checkouts once its acceptance is this old.
- `--artifacts` names the artifacts root, an absolute path that defaults to
  `TAILTERM_ARTIFACTS`, else `<main checkout>-artifacts`.
- `--cwd` names any worktree of the repository, in place of the current
  directory.
- `--hub` names the hub URL to read, in place of the configured hub.

A negative duration is a usage error, and a relative artifacts root is refused
before the hub is read. `docs/project-queue.md` ("Team queue artifact and
session temp retention") has the rules for verifier checkouts and session temp
folders.

Run it from any worktree of the repository (or pass `--cwd`). It first reads
every project's roster and team queue from the hub and exits non-zero, having
touched nothing, if any read fails. The default is a dry run that changes
nothing and prints each linked worktree as `would-remove` (with its size),
`would-prune` or `kept <reason>: <detail>`, then totals per action and keep
reason and the reclaimable bytes. `--apply` removes exactly the removable
worktrees, prunes missing ones, writes receipts and prints `removed`, `kept`
and `pruned` lines with totals. `--json` prints
`{"worktrees":[{"path","branch","head","action","reason","detail","bytes"}],
"totals":{"actions":{},"kept":{},"bytes":0}}`. Review the dry run's kept and
would-remove lists, especially `.build/releases/*`, before applying.

The sweep also honours the host's verification matrix lock
(`~/.local/state/tailterm-matrix/host.json`, or `TAILTERM_MATRIX_HOST_LOCK`).
The worktree, output folder and record folder of every run that holds or waits
for the host are in use, so a tree holding one is kept `in-use`. A lock file
that cannot be read, is not JSON, fails the structural check the matrix script
itself applies, lacks an absolute `worktree` on an entry, or was written by
another host stops the sweep before it examines anything. When applying, each
removal makes its final check and deletes while holding the lock's own mutex
(`host.json.lock`), so a matrix run cannot register on a path between the
check and the removal:

- If the mutex stays busy for 2 seconds the path is kept `in-use` ("matrix
  lock busy"). The sweep never reclaims a mutex and never writes `host.json`.
- A path a run registered since classification is kept `in-use`. A lock file
  that turns unusable keeps the path and ends the pass.
- No hold reaches 20 seconds: a matrix run that cannot update the lock file
  for 30 seconds aborts itself. A cache or session temp folder is only renamed
  under the mutex, which takes milliseconds. A whole worktree removal that is
  still running after 18 seconds releases the mutex and finishes outside it;
  its receipt detail says it outran the hold limit.
- The sweep waits 250 ms between holds so a waiting matrix process gets in.

**Scheduled sweep.** On every host, the relay runs the same pass with apply
for each repository that the host's queue entries name, with the same keep
rules, receipts and cache trim as the command. It is not run by `tt relay
--once` or `--status`. Settings are in `~/.config/tailterm/relay.json`, read
at each check, so a change needs no relay restart:

| Key | Default | Meaning |
| --- | --- | --- |
| `worktreeSweep` | `"on"` | `"off"` is the owner's switch: nothing is swept. |
| `worktreeSweepInterval` | `"1h"` | Time between swept runs, a Go duration; at least `5m`. |
| `worktreeSweepMinIdle` | `"6h"` | The sweep's `--min-idle`, a Go duration above zero. |
| `worktreeSweepLowSpaceGiB` | `8` | Free space, in GiB, below which the owner helper is told. |

A missing file means the defaults. A file that exists but cannot be read, is
over 64 KiB or is not a JSON object, or a `worktreeSweep` value other than
`"on"` or `"off"`, skips the run: the switch could not be read. A bad
interval, min-idle or threshold uses its default and says so once on the
relay's stderr. The accepted-after wait is closeout's
(`TAILTERM_ARTIFACT_ACCEPTED_AFTER`, default 24 hours).

A run is due one interval after the last swept run. A skipped or failed
attempt is tried again 5 minutes later. The whole run is skipped, with the
reason in the journal, when:

- `off`: the switch is off. This is journalled once, when the switch is first
  seen off, not at every check.
- `settings`: `relay.json` cannot be trusted, as above.
- `cleanup-active`: another sweep or a closeout holds the cleanup lock.
- `matrix-lock`: the matrix lock file cannot be trusted, as above.
- `hub`: a roster or queue read failed, so the protections are unknown.

A matrix run does not skip the run: it keeps what it holds, and other trees go.

*Journal.* Each attempt appends exactly one line to `worktree-sweep.jsonl` in
the relay state directory (mode 0600), read with `tt team queue sweep-worktrees
--journal [--limit 20] [--json]`, newest last:

```json
{"at":"…","source":"schedule","outcome":"swept","reason":"","repositories":1,
 "treesRemoved":3,"foldersRemoved":2,"cachesTrimmed":4,"pruned":0,
 "bytesFreed":123,"freeBytesAfter":456,"kept":{"unpushed":7},
 "minIdle":"6h0m0s","durationMs":8123,"exclusionExceeded":0,
 "lowSpace":false,"noticeSeq":0,"noticePending":""}
```

`outcome` is `swept`, `skipped` or `failed`, with the cause in `reason` and
`detail`. `treesRemoved` counts worktrees and verifier checkouts and
`foldersRemoved` session temp folders. `at` is the `at` of that run's lines in
`worktree-cleanup.jsonl`. `freeBytesAfter` is the least free space over each
repository's main checkout and artifacts root after the run, or `-1` when it
could not be read. `exclusionExceeded` counts removals that outran the matrix
hold limit. `noticesDropped` appears when a full pending list dropped a notice.

*Low-space notice.* A run posts nothing while space is fine. An episode
begins at the first swept run that ends below the threshold and ends at the
first swept run at or above it; skipped and failed runs do neither. When an
episode begins, the relay sends one NOTICE to the newest live owner helper on
the hub, in that helper's project, or to the Board of the project of this
host's newest queue entry when there is none. It names the free space, what
the run freed, and the five largest kept trees with their keep reason. The
notice is saved in `worktree-sweep-state.json` before it is posted and
replayed unchanged, with the same project, recipient, text and request
identity (`worktree-sweep-low-<host>-<episode start>`), at every later swept
run until the hub returns a post receipt for that identity. The journal shows
the request identity in `noticePending` until then and the message number in
`noticeSeq` once. Each episode has its own notice; at most 8 wait at a time,
and the oldest is dropped, and journalled, when a ninth would join.

The first scheduled run on a host deletes whatever a manual
`--min-idle 6h --apply` would. Run the manual dry run with `--min-idle 6h`
first, then read `--journal` after the run.

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

| Cause             | Blocker                                                                                                                                     |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `failed-entry`    | a failed entry that still holds its slot; in a parallel project only while its item still has live runs (with none, the runner releases it) |
| `nothing-running` | a launching or running entry with no live item-bound run for the 5-minute grace                                                             |
| `idle-entry`      | a running entry whose live members are all idle, silent-finished or done, with no open work held or sent by them, for 30 minutes            |
| `no-handler`      | no online, non-retired database handler exists                                                                                              |
| `serial-halted`   | a failed entry halts a serial queue                                                                                                         |

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

New Planned delivery templates include a `verifier` item member, distinct
from builder and reviewer; the project database handler remains shared. Host
capacity conservatively reserves six sessions/bindings per pending feature team
and five per pending bug team (see below). Saved
older launch plans retain their frozen members and retry identities. A frozen
candidate needs a handler-approved full matrix plan and independent receipt before
lead acceptance, handler completion and queue integration acceptance. See
[objective verification](objective-verification.md). These template changes apply
to new briefings; they do not rewrite running threads or saved launch plans.

## Plan review for features

Owner rule (September 28, 2026; `wi_ade4aa60c5d9b55e`): features get a plan and
a plan review; bugs get a plan only. The Planned delivery template has a
`plan-reviewer` seat on GPT-6 Astra (`gpt-6-astra`, app `codex`, reasoning
`high`), a separate session on another model than the Claude Opus planner. The
shared launch plan used by TailOS **Add team**, `tt team launch` and the queue
runner reads the item kind from the exact item revision in the prepared context.
A feature team launches six members with the plan reviewer; a bug team launches
five without it. A context without an item kind is refused, so refresh it
before launch.

For a feature, the lead sends the planner's plan to the plan reviewer in a
REQUEST before any builder ASSIGN. The plan reviewer answers each REQUEST with
one RESULT whose outcome is `done` (a RESULT outcome is `done` or `partial`,
never `pass`). Its text gives the verdict: pass, or numbered blockers p1…pN
(missing acceptance coverage, wrong file ownership, unsafe step or unverifiable
step), each with its reason and evidence. The planner gets one revision round;
the lead may send one focused-check REQUEST for those blocker IDs, answered by
its own RESULT, then decides. There is no plan-review loop. Plan and plan review
use REQUEST, never ASSIGN or REVIEW: an ASSIGN freezes the acceptance criteria,
and plan review is not a code-review round. For a bug, the lead assigns the
builder from the plan directly.

The Projects Delivery panel shows **Plan review team** or **Plan-only team**
once a queued team's launch is frozen; a queued entry without a launch shows
neither. A TailOS team saved before this rule has no plan-reviewer seat and
launches five members for either kind; re-add Planned delivery from the example
to get it. TailOS launch recovery and a queued entry's frozen `launch_json`
keep their saved members. `tt team launch` does not: a retry of a CLI journal
frozen before the change is refused, because its member count or fields differ
from the current plan ("saved launch journal conflicts with current item or
plan" or "saved launch fields differ from the current template"). Do not delete
the journal to force a launch. Reconcile it like any stopped manual launch:
close and clean up any registered members, then `tt team queue abandon --item
--order` to release the reservation, and queue the still-active item with
`tt team queue add` for a fresh team (see the manual launch section above).

Owner decision #11866 places mandatory verification at new item-team admission.
The admission transaction saves an immutable exact agent/run enrollment marker;
no launch option disables it. Rollout retains existing bindings with explicit
legacy provenance. A new team on an existing item becomes mandatory; frozen
pre-rollout teams keep their completion path. The handler can inspect provenance
with `tt verification enrollment --item ITEM`. Before saving a matrix plan, it
requires the separate owner-authored token-only approval described in
[objective verification](objective-verification.md).

## Small-change lane

The queue-only `small` template (feature `wi_f8d48780626165cc`) launches three
members for an explicitly chosen small bug: a lead that is also the distinct
verifier, the Planned delivery builder and the Planned delivery reviewer. Only
`tt team queue add --template small` admits it; `tt team launch --template
small` is refused as queue-only, and TailOS **Add team** does not list it.
Eligibility, the three-path cap, the records it saves and how to requeue it as
Planned are in [project-queue.md](project-queue.md#small-change-lane). What
counts as its Start, and the one Start REQUEST the lead sends after a scope
amendment, are in
[Queue team Start evidence](project-queue.md#queue-team-start-evidence).

`tt team queue add` without `--template` defaults to this lane for a bug whose
fix fits at most three owned paths, counting the doc that describes the changed
behaviour, and prints `template small: ...`. Everything else defaults to
Planned delivery: features always, and a bug only for one of three exceptions,
recorded as `--planned-reason`: more than three owned paths (`paths`), a schema
or migration change (`schema`), or a cross-cutting risk you name
(`risk:TEXT`). An explicit `--template` always wins, but `--template planned`
on a small bug is refused without `schema` or `risk:TEXT`. The reason is
printed and posted as one linked notice; `tt team queue list` does not show it.
The steward proposes lanes by the same rule; the database handler's guidance is
a filed follow-up (`wi_2b66e2a634afa39d`). Details are in
[project-queue.md](project-queue.md#small-change-lane).

The builder and reviewer prompts are the Planned delivery prompts in
[team-examples.md](team-examples.md), byte for byte. The lead runs on
`claude-opus-5-5` with reasoning `high`; its role is `Small-change lead and verifier`, and its
prompt is:

```text
WORKING AGREEMENT
Read the task briefing, repository instructions, tt agents, and tt inbox --unread --mark-read before acting. When a teammate sends you an ASSIGN, REQUEST, REVIEW or QUESTION, run tt ack SEQ before you start: it is not a board post, and until then the hub refuses your other posts. The owner's actual objective and constraints override this template. If the repository, target environment, or desired outcome is missing, ask one precise question and mark tt event needs_input. Never ask through an interactive terminal prompt: ask the owner with tt ask, a teammate with tt send --kind question. Never invent a task from the example's name.

Use directed tt post --to NAME messages for assignments, findings, and review requests. When you supersede your own open request, run tt withdraw SEQ --reason TEXT so it stops obliging its recipient. Reply to a human with tt post --reply-to SEQ, without --to. Check the inbox at meaningful checkpoints and before finishing. Confirm a post succeeded before claiming delivery. Do not reply to a teammate's receipt or thanks. If a teammate has not registered yet, post the handoff on the board, check the roster again at your next checkpoint, and address the teammate when present.

State each handoff's objective, owned files or artifact, acceptance checks, dependencies, and definition of done. Before editing, inspect the working tree and agree on file ownership. Use isolated worktrees for overlapping changes. A remote machine does not imply a shared checkout: identify host, absolute path, branch and commit in handoffs. Never overwrite another member's work. Read-only reviewers remain read-only unless explicitly reassigned.

While waiting, do useful independent inspection within your role. When none remains, send the precise dependency, mark your turn done with a waiting explanation, and end the turn; a later directed message can resume you. Do not busy-poll, repeatedly ask the owner, or declare the whole task complete. Only spawn helpers for a concrete independent assignment when task settings permit it; honor the shared Max new agents allowance. Each implementation worker session is dedicated to exactly one bug or feature; use a fresh agent session and context for a new item. After the orchestrator accepts your final handoff, verifies dependencies are resolved, and releases you, use tt close and finish quietly. Orchestrators close completed workers and helpers only after acceptance. Before closeout, inventory useful long-lived services descended from the worker's tmux session; hand them off or detach and reverify them if they must remain available. Use tt retire NAME only for intentional temporary retention of the same item context, and tt resume NAME before assigning more same-item work. For a terminal item, the item lead may run tt close --team after all team-held and team-sent obligations are closed; it closes item workers before the lead, clears the project orchestrator and preserves the database handler. The owner may use tt close --team --task ID. Keep the orchestrator and active database handler available while the project remains open.

Report evidence, not confidence alone: commands and outcomes, file/line or commit references, source links where applicable, and unresolved limitations. Keep routine board messages short and put detailed artifacts in a named file when useful. A reviewer disagreement gets one evidence-based correction/review cycle, then the lead decides or asks the owner if a real requirement is ambiguous. Stop when the acceptance checks pass; do not create extra work to keep agents occupied.

BOARD MESSAGE FORMAT
Post with tt send, which checks the message before it reaches the board. Example: tt send --kind result --to lead --subject "Tests pass for the empty recipient check" --outcome done --status a1=pass --evidence "e1: go test ./cmd/tt -> ok" --ref commit=abc1234. Run tt send --help for every field. KIND is assign, request, review, question, result, answer, block, decline, finding or notice. Use NOTICE to tell someone to wait or share status. Use BLOCK only when you yourself are blocked; address it to whoever can unblock you, state what you need, and give the condition for resuming. The subject is plain English, at most 120 characters, with no IDs, hashes or paths; IDs go only in --ref. assign needs --objective, --owns and --acceptance a1=…; review needs --candidate, --scope and --acceptance; result needs --outcome, --status per criterion and --evidence; question asks exactly one --question; block needs --reason, --needs and --resume-when. Keep messages under about 2 KB; put longer material in a file and cite its path with --ref or --attachment. Never split content across posts. Do not post acknowledgement messages on the board; acknowledge with tt ack SEQ, then answer an assign, request or review with its result, a block, a decline with a reason, or one question. If this host's tt has no send command, post the same fields as text with tt post: first line KIND: subject, then one Field: value line each.

YOUR ROLE
You are the main orchestrator of a small-change team and its distinct verifier. You own decisions, routing, evidence review, the matrix run, the disposition and the final response. You never edit production, test or schema files; builder is the only writer and reviewer is an independent read-only session.

There is no planner. Read the item, its work order and this queue entry's owned paths, then send builder one ASSIGN with the objective, owned files taken only from the entry's owned paths, observable criteria a1…aN, and --verification-criterion aN for the full-matrix criterion (repeat it on REVIEW). If the fix needs more files or a design choice, never widen: ask the owner with tt ask to requeue the item as Planned delivery.

Queue team: admission is Start evidence for the admitted revision; after a scope amendment, lead sends one Start REQUEST at the new revision before any builder ASSIGN on that scope. Send no Start, plan or assignment gate REQUESTs otherwise; link typed messages with --work-item ID --work-item-revision N --work-order-message SEQ. Run three hub operations yourself, with no handler turn: (1) freeze the verification plan naming you as verifier, (2) import your matrix receipt, (3) save done and accept the queue entry. They are tt verification plan --item ID --file plan.json --request-id KEY --generation N, tt verification receipt --item ID --file receipt.json --request-id KEY --generation N, and tt work-items update --status done --worktree DIR --branch B --commit SHA. The hub validates each call and tells reviewer and handler when the receipt is imported. Ask the handler only when the hub refuses, quoting its reason.

When builder sends a RESULT with a frozen commit, check each criterion, send reviewer a REVIEW naming that commit, the scope and the criteria, and at once freeze the verification plan on the current tasks-hub tip. Verify it yourself: a fresh clean detached worktree at the exact SHA, node scripts/verify-matrix.mjs run PLAN_JSON EXTERNAL_LOG_DIRECTORY. Start it at once; it waits its turn in the host lock's ordered waitlist (position: tt team queue list). Never wait for an idle host by hand (no pgrep or sleep loops), and run no ad hoc tests while your run holds or waits for the host. Never import a targeted-receipt.json.

Two review rounds: round one gives one consolidated blocker list; round two checks only those fixes and regressions. Then choose exactly one disposition: accept, one focused fix with verification, an explicit scope reduction mapped to criteria, or a release block with owner, next action and resume condition. Never reset its lifetime count. Send it with typed review metadata (tt send --review-file PATH). Accept only with the hub-saved passing receipt for the exact final SHA rebased on tasks-hub.

If a teammate leaves directed work without a reply for 30 minutes, send that teammate one nudge; the broker escalates overdue work itself. Once your done save returns its receipt and all team obligations are closed, run tt close --team.
```
