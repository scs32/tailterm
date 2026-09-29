# Owner interventions

Feature `wi_d55e7d8c840a3739`, work order #13865.

An owner intervention is any time the owner had to step in to keep delivery
moving: releasing by hand, nudging a stalled agent, deciding on a team's behalf,
fixing a gate, cleaning up, diagnosing, or answering a status question. Each one
is a product gap. Recording them gives a daily count that should fall to zero,
and links each intervention to the product item expected to remove it.

## Record

```
tt owner intervene --task ID --kind KIND --item ID [--product-item ID] --text T [--request-id R] [--json]
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
- `--request-id` is the retry identity. Without it the CLI derives one from the
  fields and the current minute, so an identical command repeated in the same
  minute is treated as a retry. Give an explicit ID to record two identical
  interventions in one minute.

The route is `POST /v1/tasks/{id}/interventions` with body
`{kind, itemId, productItemId?, text, requestId}`. It returns `201` and the
Board message. An identical retry returns the original message; the same
`requestId` with different fields returns `409`. An invalid kind, blank text or
malformed ID returns `400`; an unknown item, an item from another project or an
unknown product item returns `404`; a closed project returns `409`. Nothing is
written when a request is refused.

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
  `{kind, itemTaskId, itemId, productTaskId?, productItemId?}`;
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

- `total`, `linked` (with a product item) and `byKind`;
- `days`, newest first, each with `day` (`YYYY-MM-DD`), `total`, `linked` and
  `byKind`, where the day is the local calendar day in `tz`;
- `unlinked`: the newest 100 interventions without a product item, each with
  `seq`, `kind`, `itemId` and `day`.

`tz` is an IANA zone name such as `America/Los_Angeles`; it defaults to `UTC`.
`Local` and unknown zones return `400`. The hub embeds its zone database, so the
result does not depend on the host's zoneinfo. The CLI uses this machine's zone
unless `--tz` is given; `--json` prints every intervention and the summary.

In TailOS, the Projects detail has a closed **Interventions** disclosure below
Usage. Opening it loads the summary in the browser's time zone: per-kind
totals, a row per day, "N of M linked to a product item", and each unlinked
intervention by message number, kind, item and day. Refresh reloads it. Offline
it keeps the last loaded data under the saved-data label, and an older hub
without the route shows that interventions are unsupported.

## Who may record

Only the owner records interventions. The hub refuses a request that carries an
agent identity (`agentId`) with `403`, and the CLI refuses inside an agent
session before sending anything; the Discord bridge credential cannot reach
either route. Under the shared-workspace identity model this is not a
cryptographic boundary: a session that removes its agent environment can still
post as the owner, the same limit that applies to decision answers.
