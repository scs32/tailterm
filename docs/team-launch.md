# Local Planned delivery team launch

## File scope before admission

During intake, the database handler reads the owner-filed item and bounded
order. If the item already states the ordered acceptance and owned files, the
handler confirms that exact item revision, scope revision and order:

```sh
tt work-items scope confirm --project tsk_... --revision 2 --scope-revision 2 --order 123 --request-id intake-123 --complete wi_...
tt work-items scope get --project tsk_... --revision 2 --order 123 wi_...
```

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

## Project team queue

On the launch host, the owner can save a sequence of recorded item orders:

```sh
tt team queue add --task tsk_... --item wi_... --order 123 --template planned
tt team queue list --task tsk_...
tt team queue reorder --task tsk_... --entry tqe_... --before tqe_...
tt team queue remove --task tsk_... --entry tqe_...
tt team queue release --task tsk_... --entry tqe_...
tt team queue abandon --task tsk_... --item wi_... --order 123
```

The hub stores queue state and retry receipts. The existing supervised Mini
relay checks that state on each tick, including when no Codex thread is bound.
It waits for an active project, one available database handler, and no live
lead. It records a launch reservation and frozen plan before effects.
Uncertain member spawns are reconciled by exact identity and owned session;
unresolved attempts fail the entry and require owner action. Terminal items
advance only after the exact team close and host cleanup receipts. A failed
entry stops that project queue and posts one durable owner escalation. Pausing
the project stops launch ticks. The deliberate agent Queue is separate.

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
