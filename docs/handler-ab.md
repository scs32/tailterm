# Handler arms: A/B trial of database handler models

Feature `wi_fc1396aef8a72a06`, work order #14869. A project can compare
database handler models by lending each queued team entry to a handler from
one of several **arms**, chosen at random per entry, then reading a report
that compares the arms on speed, cost and quality.

An arm is a runtime, a model and a reasoning level, for example Claude Sonnet
5.5 on Claude Code (`S`) and GPT-6.1 Sol on Codex (`O`). With no policy, or a
disabled one, the team queue leases the first free handler exactly as before.

## Commands (owner only)

Provision one handler per arm from the saved Planned database-role spec, with
the model and reasoning overridden per arm:

```sh
tt spawn --task T --role database_handler --name db-handler-sonnet \
  --run claude --model claude-sonnet-5-5 --reasoning high --prompt "$PROMPT" ...
tt spawn --task T --role database_handler --name db-handler-sol \
  --run codex --model gpt-6.1-sol --reasoning high --prompt "$PROMPT" ...
```

`tt spawn --role database_handler` records the run's template digest, model
and reasoning on the hub (`handler_runs`). The first record for a run wins, so
a replayed launch cannot rewrite it. `handlerModel` or `handlerReasoning` on any
other role is refused with 400.

Then save and enable the policy:

```sh
tt handler arms get --task T            # policy, handlers, digests, open limits
tt handler arms set --task T --revision 0 \
  --arm S=claude-sonnet-5-5/claude/high:1 --arm O=gpt-6.1-sol/codex/high:1 \
  --seed trial-2026-10 --fallback=false --enabled
tt handler ab-report --task T [--json]  # the comparison
```

`--arm` is `ID=MODEL/RUNTIME/REASONING:WEIGHT`; the model may contain slashes.
Giving any `--arm` replaces every arm; other flags keep their saved values when
omitted. Only enabling reads the template: disabling, or saving a disabled
policy, keeps the saved digest and needs no saved spec unless `--prompt-file`
is given. `--hold-minutes` sets the Claude limit hold. `--prompt-file` gives the
handler assignment prompt; by default it is the saved handler spec's
`--prompt` (`tt handler spec`), which must hold the Planned database-role text
so rotation successors match. The policy is revision-checked and keyed: the
same command replays its derived request ID, or pass `--request-id`.

The routes are `GET|PUT /v1/tasks/{id}/handler-ab/policy` and
`GET /v1/tasks/{id}/handler-ab/report`. An agent identity on `PUT` is refused
(409 `agent_caller`), as is a stale revision (409 `stale_revision`) and a
reused request ID with different input (409 `request_reused`).

## Policy validation

Each of these is a 400 and writes nothing:

- 2 to 8 arms; at least 2 when `fallback` is on;
- arm IDs match `[A-Za-z0-9_-]{1,16}` and are unique;
- weights are integers from 1 to 1000;
- each arm has a runtime, model and reasoning, and no two arms share all three;
- `limitHoldMinutes` is 5 to 1440 (0 means the default, 60);
- the seed is non-empty text of at most 128 characters;
- an enabled policy has a template digest of 64 lowercase hex characters (a
  disabled one may have none).

## Arm identity and template parity

A handler run matches an arm when its agent runtime and the model and reasoning
recorded at spawn are all equal to the arm's. A run with no recorded model
matches no arm and is never leased while a policy is enabled.

The policy stores a reference template digest. Enabling it, or saving it while
enabled, returns 409 `template_mismatch` when any open handler that matches an
arm has a missing or different recorded digest; the error names each one and
nothing is written. `tt handler arms get` shows every open handler's runtime,
model, reasoning, digest, `digestMatches` and arm, or why it has none.

The digest is not part of lease-time identity (lead amendment #15761): a
rotation successor started after a template change stays leasable. Each lease
records the handler run's digest and the policy's, and the report flags items
whose digest differs from the policy's or from every digest the other arms'
items used.

## Draw and lease

For a queue entry the claim computes `h = SHA-256(seed + "\x00" + entryID)` and
reads `u` as the big-endian uint64 of `h[0:8]`. With the arms sorted by ID and
`total` their weight sum, it picks the first arm whose cumulative weight `c`
satisfies `u < floor(2^64 · c / total)`, in integer arithmetic. The same seed
and entry ID always draw the same arm, and the order arms are listed in does
not matter.

Arms with an open provider-limit episode are left out of the draw and listed in
`skippedLimited`. The claim then leases a free handler of the drawn arm (online,
not leased by an active entry, not part of a prepared rotation). If none is
free:

- with `fallback` on, it leases a free handler of another non-limited arm, in
  arm ID order, recording `fallback=true, fallbackReason=busy`;
- otherwise it returns 409 `arm S: no free handler in the drawn arm`.

If every arm is limited it returns 409 `every handler arm is at a provider
limit, so there is no free handler in the drawn arm`. Both end in the fixed
suffix `no free handler in the drawn arm`, which the runner treats as an
ordinary wait. A refused claim writes nothing.

**Head-of-line waiting.** The runner claims the queue head. With
`fallback=false`, a head drawn to a busy arm holds every lane until a handler of
that arm is free, even when the other arm is idle. `fallback=true` avoids that
at the cost of arm purity; fallbacks are flagged in the report.

## Stored assignment and visibility

The claim stores the assignment in `handler_arm_assignments` in the same
transaction: policy revision, the 8-byte draw, drawn arm, leased arm, fallback
and reason, skipped arms, handler agent and run, the run's and the policy's
digests, and the lease time. Finishing the entry stamps `finishedAt`.

`tt team queue list` prints `arm=S drawn=S fallback=no|busy`, and the entry
JSON carries `handlerArm`. Under a policy a queued entry's reason starts with
`Waiting for a free handler of arm S` (continued as "Automatic provisioning"
describes) or is `Every handler arm is at a provider limit`. In
TailOS the Delivery row reads `Handler NAME · lease N · arm S`, plus
`(fallback from O)`.

## Automatic provisioning

Bug `wi_01b6d3afed81167c`, work order #27052. The queue limit admits teams,
but each team needs its own handler lease. Before this change nothing added a
handler when the limit had room and every handler was leased: on 2026-10-01
the limit was 4 with three handlers, an urgent entry waited for "a free handler
of arm S", and the owner helper started a fourth handler by hand.

**Rule.** When a queued entry is admissible except for a free database handler,
the runner on the entry's host adds one handler from that host's saved launch
spec (`tt handler spec`), if the hub allows it. Otherwise the entry's reason
names what is missing and the exact command that fixes it.

**Handler need.** Such an entry carries `handlerNeed` in the listing JSON:
`arm` (the drawn arm; empty without an enabled policy), the wanted `runtime`,
`model`, `reasoning` and `templateDigest`, `handlers` (the project's open,
non-retired handlers with that runtime, model and reasoning), `leased` (how
many of them active entries hold), `provision`, `reason`, `fix`, and the
bookkeeping fields `refused`, `attempt` and `agentId`. Under an enabled policy
the wanted settings are the drawn arm's and the policy's template digest.
Without one they are the recorded settings of the project's first open
handler; if that handler recorded no model or template at spawn, the entry has
no `handlerNeed`, keeps the reason `No free database handler`, and nothing is
added. A project with no handler at all is not covered here
(`wi_42be87739d7bbfc9`).

**Conditions.** `provision` is true only when all of these hold:

- automatic handler provisioning is on for the project (see the switch below);
- the queue has a free slot and the drawn arm is not at a provider limit;
- `handlers` is below the queue limit (with `--limit none`: below the active
  teams plus one), so handlers of the wanted settings never exceed the limit;
- the new handler and the waiting team both fit under the project agent cap:
  `open + reserved + team seats + 1 <= cap`, with the counts of
  [project-queue.md](project-queue.md), "Project agent cap". A provision is
  therefore never what pushes the team over the cap;
- no other provision of the project is pending;
- the host's saved spec was not refused for this entry.

**Exact match.** The runner sends the hub its saved spec's runtime, model,
reasoning and the template digest of its `--prompt`, with a preallocated
handler agent ID (queue operation `provision_handler`). The hub recomputes the
need in one transaction and decides the match itself: all four values must
equal the wanted ones. A single saved spec can therefore add handlers of one
arm only; for any other arm it is refused, never adapted.

**Reservation and notice.** On a match the hub stores one `reserved` row in
`handler_provisions` and posts one Board notice with the subject `Automatic
handler provision`, naming the entry, arm and handler agent. It is a notice
from the team queue, not an owner intervention. The runner then starts the
handler through the ordinary `tt spawn --role database_handler` path with the
spec's flags and the reserved agent ID; the next pass claims as usual. The row
becomes `registered` when that agent is an online handler and `abandoned` if
it is not after 10 minutes. The runner's request ID is
`queue-provision-ENTRY-REVISION-ATTEMPT` and the handler agent ID is derived
from it, so a retry with the same ID replays the same row and posts nothing.
Once the ten minutes have passed the listing reports the next `attempt` even
before anything has saved the outcome, so the runner's next request has a new
ID: it saves the old row as `abandoned`, reserves a new handler (a new agent
ID) and posts a new notice. This repeats every ten minutes while the handler
keeps failing to come online. A handler registers with the hub before its
session starts, so an abandoned attempt either never registered, and nothing
of it runs, or it is an open handler that counts toward `handlers`, and no
further one is reserved once the limit is reached. The runner makes one
provision attempt per project and pass. This work never closes a handler:
surplus handlers stay until the owner closes them.

**Reasons.** The entry's reason starts with `Waiting for a free handler of arm
S` (or `No free database handler` without a policy) and, when the limit has
room for another handler, continues with the counts and one of:

| Case | Reason after `…: 3 of 3 leased, limit 4` |
| --- | --- |
| Being added, or one is pending | `; the runner is adding one` |
| Switch off | `; automatic provisioning is off. Fix: tt team queue provision --task TSK --auto on` |
| Agent cap | `; cannot add one: project agent cap: 23 open + 6 seats + 1 handler > 29. Fix: tt team queue limit --task TSK --limit 3` (with `+ R reserved` after the open count when seats are reserved) |
| Spec differs | `; cannot add one: the saved launch spec on HOST is RUNTIME/MODEL/REASONING digest D1, arm S needs RUNTIME/MODEL/REASONING digest D2. Fix: PROMPT="$(cat PROMPT_FILE)" && tt handler spec --task TSK -- --run 'SAVED RUN' --cwd 'SAVED CWD' --runtime R --model M --reasoning E --prompt "$PROMPT" (PROMPT_FILE holds the handler prompt with that template digest), or: tt team queue limit --task TSK --limit 3` |
| Spec is for another runtime, or no spec saved | the same, with `the saved launch spec on HOST is missing` when none is saved, a command without `--run`, and the note `(…; add --run with the R launch command and the host's other launch flags, tt refuses the spec without --run)` |

`TSK` is the real project ID and the numbers are the real counts. The limit
command always names the current number of matching handlers, so it is an
exact alternative: the queue then admits only as many teams as it has
handlers.

The spec command is safe to paste as written. `tt handler spec` replaces the
whole saved spec, so the runner sends the saved spec's other launch flags
(`--run`, `--cwd`, the permission, approval, sandbox and allowed-tools flags;
never the prompt) and the command repeats them, shell-quoted, with the wanted
runtime, model and reasoning. The hub stores only the prompt's digest, so
`PROMPT_FILE` is the one placeholder; the command reads it first and stops
before saving anything if it cannot. When the saved run command starts
another runtime, or no spec is saved, the hub does not guess a run command:
the command has no `--run`, which `tt handler spec` refuses, and the note says
to add it. A saved value no handler could have, such as a runtime taken from
a run command given by path, is refused the same way with the value shown;
control characters are removed and long values cut. Without a policy the spec
reasons say `the handlers in use needs` instead of `arm S needs`.

**At the limit.** When the limit already has its handlers of the wanted
settings, nothing is added: another handler would not be within the limit, and
a provision request answers 409. What the entry says depends on whether one of
those handlers is offline (the hub has not heard from it for 90 seconds).

| Case | Entry reason |
| --- | --- |
| Every handler is online (busy, or held by an open rotation) | `Waiting for a free handler of arm S`, unchanged; the counts are in the entry's handler need only |
| One is offline | `Waiting for a free handler of arm S: 1 of 2 leased, limit 2; the limit already has its handlers, and NAME is offline. The hub does not restart a handler; an operator can retire NAME so that it stops counting and one can be added. Fix: tt retire --task TSK NAME` |
| The offline one is the primary | the same up to `…restart a handler;`, then ` an operator can rotate NAME, the primary, to a successor. Fix: tt handler rotate --task TSK` |
| The offline primary cannot be rotated now | the same up to `…restart a handler;`, then ` NAME is the primary, and its rotation (tt handler rotate --task TSK) is refused until this clears: DETAIL. The host runner on HOST replaces it automatically once its process and session are confirmed gone there and it has been silent for N minutes (silent M so far).` with no `Fix:`. DETAIL is the refusal `tt handler rotate` would give, such as `the handler run holds 1 live team lease(s), first ENTRY`; N is the project's silence and M the whole minutes since the run's last recorded activity. A run with no recorded activity ends `(no activity recorded from it yet).` instead |
| The same, with the automatic replacement off | the same up to `…until this clears: DETAIL.`, then ` Automatic replacement of a dead primary is off (tt handler policy set --task TSK --revision R --dead-silence-minutes N).` with no `Fix:`; R is the policy's current revision |
| The offline one is the project's last available handler (and not the primary) | the same up to `…restart a handler;`, then ` an operator can add another handler; NAME is the project's last available one and cannot be retired until then. Fix: set up a database handler (Projects → Set up database handler) or resume a retired one with tt resume NAME, in project TSK` |
| Several are offline | `…, and NAME1 and NAME2 are offline. …` (`NAME1, NAME2 and NAME3` for more), in lease order; the recovery and the fix are for one of them: a handler no active team leases before a leased one, and the primary last |

A busy handler frees itself, so that wait needs no action. An offline one does
not come back by itself, and until an operator acts the entry waits. The hub
only states the command: it never starts, stops, retires or rotates a handler
here, and the listing writes nothing. The operator's recovery is:

- **`tt retire --task TSK NAME`** for a handler that is not the primary. A
  retired handler no longer counts toward `handlers`, so the entry is below
  the limit again and the rules above apply: with the switch on and room under
  the agent cap the runner adds a replacement, otherwise the reason names that
  fix. `tt resume NAME` undoes it if the handler comes back. For a name that
  starts with `-` the command gives the agent ID instead.
- **`tt handler rotate --task TSK`** for the primary. The hub refuses to
  retire or close the primary; rotation, run on the old handler's host, moves
  the role to a successor and closes the old handler, which then no longer
  counts (see `docs/handler-rotation.md`). Rotation is refused while the
  primary's run holds a live team lease, has a pending tool call, or is
  recorded as working, hung on a tool or looping. The hub makes that same
  check here: when it would refuse, the reason gives the refusal and no fix,
  because no command the hub accepts frees the place yet. The lease clears
  when its team finishes or the owner releases its failed entry.

  A primary that died while busy never clears that refusal by itself. The
  host runner replaces it: once the handler's own host confirms its process
  and session gone and the hub has recorded nothing from the run for the
  project's silence (10 minutes by default), the runner rotates it and its
  leases move to the successor. See
  [A dead primary](handler-rotation.md#a-dead-primary). The reason says so,
  with the host, the minutes and how long the run has been silent. Turn that
  replacement off for a project with
  `tt handler policy set --task TSK --revision R --dead-silence-minutes 0`;
  the reason then names the command that turns it on, and the entry waits for
  the owner.

- **Another handler first** when the offline one is the project's last
  available handler (no other handler is open and not retired). The hub
  refuses to retire or close that one, so the retire is never stated. The fix
  is the one of the queue's no-handler stall: set up a database handler
  (Projects → Set up database handler) or resume a retired one with
  `tt resume NAME`. The waiting entry leases that handler on the next pass,
  and the offline one can be retired afterwards. With no limit set this
  project is stalled and the entry's reason is the stall's own text; the
  handler need then carries the same fix.

With several offline handlers the next listing names the remaining ones once
the first is dealt with. An offline handler below the limit changes nothing:
the entry keeps the provisioning reason above.

A refused spec is stored as one `refused` row per entry and is the entry's
standing reason. The runner keeps offering its spec each pass, so saving a
matching spec is noticed; the row is removed then, and when the entry leases a
handler. Offering the same spec again answers 409 and writes nothing: the row
is rewritten, and the project's listeners notified, only when the offered spec
or the wanted settings changed. A cap, switch or limit refusal answers 409 and
stores nothing.

**Switch.** Automatic handler provisioning is a project setting, default
**on**, stored as `team_queue_settings.handler_provision` (a project with no
settings row reads on):

```sh
tt team queue provision --task T --auto off
tt team queue provision --task T --auto on
```

It is an owner-side change with the same authorisation as `tt team queue
limit`. `tt team queue list` prints `handler provisioning=on|off` and the
listing JSON carries `handlerProvision`. Off stops reservations at once; a
handler that was already started stays.

**Raising the limit warns.** The filed choice was "warn or provision up to the
limit"; this change warns. `tt team queue limit` still saves a raised limit
that exceeds the available handlers (online, not retired, not in a prepared
rotation) and prints `warning: limit 4 exceeds 3 available database handlers;
the runner adds one per waiting team while the agent cap allows`, or, with the
switch off, `…; automatic provisioning is off, so teams will wait. Fix: tt
team queue provision --task TSK --auto on`. The JSON result carries it as
`warning`. A change to `--limit none` has no number to compare, so it always
reads `no fixed limit with 3 available database handlers; …`. Lowering or
keeping the limit prints no warning. Handlers are added one at a time, only
when a team actually waits.

**Release note.** The default is on, so after release the runner starts a
handler the first time a team waits with room under the limit and the cap,
provided the host's saved spec matches. To keep the old behaviour run `tt team
queue provision --task T --auto off`.

**Compatibility.** An older `tt` ignores `handlerNeed` and waits as before. A
newer `tt` against an older hub sees no `handlerNeed` and adds nothing.

## Rotation within an arm

Handler rotation stays primary-only and, under a saved arm policy, inside the
arm. When the project has a saved policy (enabled or not) and the old run
belongs to one of its arms, commit returns 409 `arm_changed`, and moves
nothing, if the successor is not in the same arm. `tt handler rotate` refuses
before spawning in the same case when the saved spec's `--model` or
`--reasoning` differs from the old run's recorded values. Without a policy, or
for a run of no arm, rotation is unchanged. An arm that does not rotate grows context; the
report shows rotations and mean input tokens per request per arm.

A [dead primary rotation](handler-rotation.md#a-dead-primary) moves the dead
handler's live leases to the successor, which the check above keeps in the same
arm. A moved lease keeps its arm assignment: the row takes the successor's
agent, run and new lease generation, and keeps its draw, arm and handler digest
as leased. The report then counts that item under the successor.

## Provider limits

An arm is limited exactly while it has an open episode
(`handler_arm_limit_episodes`, at most one open per arm). The draw, the list
reason and the report all read episodes and nothing else.

**Opening.** In the activity report transaction, a report for a handler run
that matches an arm opens the arm's episode, and posts one Board notice
("Handler arm O is at a provider limit; new items go to other arms."), when:

- Codex: `state=runtime_prompt` with prompt kind `codex_usage_limit` that no
  action has answered;
- Claude: `state` is `idle` or `finished_silent` with reason `turn ended by API
  error (rate_limit)`, or `state=provider_blocked` with class `usage_limit` or
  `rate_limited` ([provider-blocked.md](provider-blocked.md)); a newer relay
  sends the second form and the hold and clearing rules are the same for both.

A repeat signal updates the episode's last signal and run and, for Claude,
extends the hold to now plus `limitHoldMinutes`; it posts nothing.
`codex_rate_limit_switch` is only counted in the report.

**Clearing** follows one rule, applied by the activity hook and at every claim:

- Codex: a later report of that run no longer shows the usage-limit prompt (a
  gone or answered prompt counts), or the run is closed (`run_closed`). If the
  limit is real, Codex shows the prompt again and a new episode opens.
- Claude: the hold has expired **and** either a completed turn (`idle` or
  `finished_silent`) without the rate-limit reason was observed after the last
  signal (`clean_turn`), or no report of that run has arrived since it
  (`hold_expired`). A `working` report never clears a limit on its own. A closed
  run clears once the hold expires.

Clearing posts one "Handler arm O is available again." notice. The claim closes
expired episodes in its own transaction just before the draw, so a claim that
then waits does not undo the clearing. Replayed activity reports and replayed
claims return their stored results and post nothing.

The hub stores a report whose state is unchanged when its reason differs, for
every state. The relay sends one only for `idle` and `finished_silent`: its
report key includes the reason for those two states, so a turn that ends by the
rate limit reaches the hub, while stuck reasons, which change often, stay out
of the key. The new key carries a version prefix. A key an older relay saved
has no reason and is compared without one, then upgraded in place, so an
upgrade sends no extra report from idle bindings.

Entries already leased to a limited arm keep their handler. There is no
automatic handoff; the owner path `tt team queue fail` is unchanged.

## Report

`tt handler ab-report` and the TailOS **Handler A/B** disclosure (under
Interventions, closed by default, loaded only when opened, read-only) cover
entries that are `finished` and have an assignment for their final lease. Each
item counts toward the arm that handled it.

Per item:

- `handlerTokens`: the item's usage role group `database_handler` (all token
  classes); `handlerRequests`, input tokens (uncached plus cached) and allocated
  turns come from the same group.
- `responseMillis`: closed handler-request obligations (typed `request`, closed
  by result or decline) on the leased handler and its committed rotation
  successors, with a primary link to the item; creation to close.
- `launchToDoneMillis`: from the lease to the item's first revision with status
  `done` after the lease; null ("not done") until then. `entryFinishedMillis`
  (lease to entry finish) is secondary.
- `handlerBlocks`: typed `block` messages linked to the item and addressed to
  the handler chain; `handlerAuthoredBlocks` those it wrote.
- `refusedSaves`: recorded refused work-item writes by the handler chain within
  the lease window (see below).
- `incorrectSaves = ownerCorrections + gateFixes`. `ownerCorrections` counts
  each handler revision written within the lease window whose changed fields a
  later owner revision (no agent) of the same item changes again. `gateFixes`
  counts `gate-fix` owner interventions on the item. An owner-attributed
  revision is a native write by the owner's own helper session, or with no
  agent by a caller other than the hub's system writers (`system`,
  `team_queue`, `handler_arms`); reconstructed history and checkpoints never
  count.
- `linkCorrections`: message-audit `correct` events on handler-authored
  messages linked to the item. Context only, not a quality count.
- `interventions`: owner interventions on the item by kind.
- `limitEvents`: the arm's episodes overlapping the lease, plus the handler
  runs' `codex_usage_limit` and `codex_rate_limit_switch` escalations within it.
- `digestFlags`: `policy` and/or `other_arm`, as described above.

Per arm: `n` and fallbacks; response median and p90 over all the arm's
requests pooled; median launch-to-done over done items; median handler tokens
per item; mean input tokens per request (the sum of uncached plus cached input
divided by the sum of allocated turns); committed rotations whose old run
matched the arm; and each count's total and per-item rate. Rates and the mean
are exact rationals such as `1/3`.

Statistics: the median of an even count is the mean of the two middle values,
floored; p90 is the nearest-rank value at `ceil(0.9n)`. Each pair of arms gets
a flag per metric: `insufficient` when either arm has fewer than 5 items;
otherwise, for time and token metrics, `within_noise` when the medians differ by
less than the larger median absolute deviation, and for counts when the totals
differ by 2 or less; otherwise `difference`. This is a heuristic, not a
significance test.

### Refusal recording

`handler_write_refusals` is insert-only (triggers abort update and delete). A
row is written, in its own transaction after the refused one, when one of these
work-item writes by an open `database_handler` of the project fails with 400,
404 or 409:

- `PATCH /v1/tasks/{id}/work-items/{item}` (`update`; no request ID, so each
  retry counts);
- `POST .../updates` (`updates`; unique per agent, run and request ID, so a
  replay records once);
- `POST .../dispatch` (`dispatch`).

Only a fixed code is kept (`invalid`, `not_found`, `conflict`,
`stale_revision`), never request text. 403 and 429 responses, the owner and
other roles are not recorded. Order-scope confirmation, bookkeeping,
verification and other write routes are not recorded yet; that is a follow-up.

## Limits and decisions

- D1: rotation stays primary-only; auxiliary handlers do not rotate.
- D3: existing trial handlers are not backfilled. Spawn fresh arm handlers
  from the saved Planned database-role spec, check `tt handler arms get`, then
  enable the policy. Enabling refuses on a digest mismatch.
- The Claude error code for an account usage limit is unconfirmed; only
  `rate_limit` opens an episode. Adding another code is a one-line change.
- Token accounting counts every handler request attributed to the item; the
  item's usage models group shows whether only the arm's model served it.
- Stall notices (`team_queue_stall.go`) do not know about arms; the list reason
  names the arm and the wait.

## Rollback

The new tables (`handler_arm_policy`, `handler_arm_policy_requests`,
`handler_arm_assignments`, `handler_arm_limit_episodes`,
`handler_write_refusals`) and the `handler_runs.model` and `reasoning` columns
are additive. Disable the policy with `tt handler arms set --enabled=false` to
return to the first-free lease at once. An older hub binary ignores the new
tables and leases as before; an older CLI does not send model or reasoning, so
its handler runs match no arm.
