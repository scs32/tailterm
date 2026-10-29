# Owner interventions

Feature `wi_d55e7d8c840a3739`, work order #13865. Actor: feature
`wi_3e13db5102b48ffa`, work order #50508.

An owner intervention is any time the owner had to step in to keep delivery
moving: releasing by hand, nudging a stalled agent, deciding on a team's behalf,
fixing a gate, cleaning up, diagnosing, or answering a status question. Each one
is a product gap. Recording them gives a daily count that should fall to zero,
and links each intervention to the product item expected to remove it.

Each intervention also records its **actor**: who performed the act. The
owner's target is zero interventions by the owner in person (owner direction
#19451); acts by the owner helper or another session working for the owner are
acceptable plumbing, so the counts are split by actor.

## Record

```
tt owner intervene --task ID --kind KIND --item ID [--product-item ID] --text T [--actor owner|helper] [--request-id R] [--json]
```

- `--task` is the project ID. It is required outside agent sessions, where
  `TAILTERM_TASK` is normally unset; when that variable is set it is the
  default. Agent sessions are refused regardless (see below).
- `--kind` is one of `release`, `nudge`, `decision-on-behalf`, `gate-fix`,
  `cleanup`, `diagnosis`, `status`, `other`.
- `--item` is the work item in this project the intervention concerns. Any
  status is accepted, including done items. It is required: for a project-wide
  intervention, cite the item being worked at the time or the project's own
  feature item.
- `--product-item` is optional: the bug or feature expected to make this
  intervention unnecessary. It may live in any project, usually the product's
  home project. Leaving it out marks the intervention as unlinked.
- `--text` is what the owner did and why (up to 4000 bytes).
- `--actor` is who performed the act being recorded, not who typed the
  command (see [Actor](#actor)).
- `--request-id` is the retry identity. Without it the CLI derives one from the
  fields and the current minute, so an identical command repeated in the same
  minute is treated as a retry. The actor is part of that derivation, so the
  same text recorded once as `owner` and once as `helper` is two records. Give
  an explicit ID to record two identical interventions in one minute.

The route is `POST /v1/tasks/{id}/interventions` with body
`{kind, itemId, productItemId?, text, requestId, actor?}`. It returns `201` and
the Board message. An identical retry returns the original message; the same
`requestId` with different fields, including a different actor, returns `409`.
An invalid kind, blank text, malformed ID or an actor other than `owner` or
`helper` returns `400`; an unknown item, an item from another project or an
unknown product item returns `404`; a closed project returns `409`. Nothing is
written when a request is refused.

## Actor

An actor is `owner` (the owner in person), `helper` (the owner helper or any
other Claude Code or Codex session acting for the owner), or `unknown`.

The CLI takes the actor from the session, checked against `--actor`:

- **Plain terminal** (neither `CLAUDE_CODE_SESSION_ID` nor `CODEX_THREAD_ID`
  is set): the actor is `owner`. `--actor owner` is accepted; `--actor helper`
  is refused, because a plain terminal is the owner in person.
- **Claude Code or Codex session**: `--actor` is required. Pass
  `--actor owner` when recording something the owner did in person, such as an
  order the owner gave, and `--actor helper` for what the session itself did.
  The helper records both, so the session alone cannot tell them apart, and
  defaulting to `helper` would undercount the owner.
- Any other `--actor` value is refused. Every refusal happens before a request
  is sent.

The hub accepts `owner` or `helper` and records `unknown` when a request names
no actor (an older client). It refuses `unknown` and any other value with
`400`. The hub cannot see the caller's session, so it checks the vocabulary
only. The actor is a self-declared label checked against the session class, not
proof of identity: a session that clears its runtime variables can record as
`owner`, the same limit described under [Who may record](#who-may-record).

Interventions recorded before the actor existed read `unknown`. The column was
added with a default of `unknown`; no row was rewritten, and the immutability
triggers still apply.

## What is stored

An intervention is an ordinary Board message, so it is ordered, auditable and
visible on the Board:

- the message text reads
  `Owner intervention (KIND) on ITEM[; product fix PRODUCT]: TEXT`, from the
  owner (no agent identity), not addressed to anyone;
- the message links the concerned item as `primary` at the item's current
  revision, so it appears in that item's message history;
- the typed fields live in the insert-only `owner_interventions` table keyed by
  message sequence, returned on every message read as `intervention`
  `{kind, itemTaskId, itemId, productTaskId?, productItemId?, actor}`;
- the post receipt makes retries safe.

Interventions are never edited or removed. There is no update or delete route,
and database triggers abort any `UPDATE` or `DELETE` on the table. A correction
is a new intervention of kind `other` that cites the earlier message number.
The project audit export includes the table as the `ownerInterventions` stream.

Interventions are human posts, so `tt message-checks` counts them in the human
form and they never affect the typed-agent adoption rate.

## Read

```
tt owner interventions --task ID [--tz ZONE] [--json]
```

`--task` is required here too unless `TAILTERM_TASK` is set.

`GET /v1/tasks/{id}/interventions?after=&limit=&tz=` returns one page of
intervention messages (at most 100, by message sequence, with `nextAfter` for
the next page) and a `summary` of **every** intervention in the project:

- `total`, `linked` (with a product item), `byKind` and `byActor`;
- `windows`: the last 6 h and the last 24 h, each with `hours`, `total` and
  `byActor`. A window is measured back from the hub's clock at read time and
  holds the interventions recorded strictly after its start, so one exactly
  6 h old is outside the 6 h window;
- `days`, newest first, each with `day` (`YYYY-MM-DD`), `total`, `linked`,
  `byKind` and `byActor`, where the day is the local calendar day in `tz`;
- `unlinked`: the newest 100 interventions without a product item, each with
  `seq`, `kind`, `itemId`, `actor` and `day`.

`byActor` keys are `owner`, `helper` and `unknown`; an actor with no
interventions is absent from the map.

`tz` is an IANA zone name such as `America/Los_Angeles`; it defaults to `UTC`.
`Local` and unknown zones return `400`. The hub embeds its zone database, so the
result does not depend on the host's zoneinfo. The CLI uses this machine's zone
unless `--tz` is given; `--json` prints every intervention and the summary.

The text output adds the actor split after the kind totals:

```
by actor: owner O, helper H, unknown U
last 6 h: N (owner O, helper H, unknown U)
last 24 h: N (owner O, helper H, unknown U)
```

Each day line shows `owner O  helper H  unknown U` before its kinds, and each
unlinked line shows the actor before the kind. A hub older than the actor sends
no `byActor` or `windows`; the CLI then prints `by actor: unavailable` and
leaves the actor columns out rather than showing zeros. The TailOS
Interventions view does not show the actor split yet.

In TailOS, the Projects detail has a closed **Interventions** disclosure below
Usage. Opening it loads the summary in the browser's time zone: per-kind
totals, a row per day, "N of M linked to a product item", and each unlinked
intervention by message number, kind, item and day. Refresh reloads it. Offline
it keeps the last loaded data under the saved-data label, and an older hub
without the route shows that interventions are unsupported.

## Who may record

Only the owner records interventions, in person or through a session acting
for the owner (see [Actor](#actor)). The hub refuses a request that carries an
agent identity (`agentId`) with `403`, and the CLI refuses inside an agent
session before sending anything; the Discord bridge credential cannot reach
either route. Under the shared-workspace identity model this is not a
cryptographic boundary: a session that removes its agent environment can still
post as the owner, the same limit that applies to decision answers.
