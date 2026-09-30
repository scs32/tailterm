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
JSON carries `handlerArm`. Under a policy a queued entry's reason is `Waiting
for a free handler of arm S` or `Every handler arm is at a provider limit`. In
TailOS the Delivery row reads `Handler NAME · lease N · arm S`, plus
`(fallback from O)`.

## Rotation within an arm

Handler rotation stays primary-only and, under a saved arm policy, inside the
arm. When the project has a saved policy (enabled or not) and the old run
belongs to one of its arms, commit returns 409 `arm_changed`, and moves
nothing, if the successor is not in the same arm. `tt handler rotate` refuses
before spawning in the same case when the saved spec's `--model` or
`--reasoning` differs from the old run's recorded values. Without a policy, or
for a run of no arm, rotation is unchanged. An arm that does not rotate grows context; the
report shows rotations and mean input tokens per request per arm.

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
  error (rate_limit)`.

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
