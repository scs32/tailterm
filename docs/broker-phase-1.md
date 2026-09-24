# Broker phase 1 — typed messages in shadow mode

```text
ASSIGN: Ship typed agent messages and shadow-mode checks on the board
Refs: design docs/message-broker.md (phase 1); team Planned delivery; kit tools/jev-kit
Objective: Agents can post typed messages; the hub validates and records every agent post's
           form and Jev scores without rejecting free text, so we can measure adoption.
Owns: see "Ownership" below
Acceptance: a1–a12 below
Due: before broker phase 2 starts
```

Hub record: Feature `wi_de84224a37cbef70`, owner intake #8766, work-order message
#8767. It is recorded after the fact under the owner-directed exception in
`AGENTS.md`. Implementation: `5ee29b6..53a4e1e` on `tasks-hub`, built outside the
harness in a Claude Code session on September 24, 2026.

Status: implemented; round-one review in progress; not deployed. Written September 23, 2026 from
[the broker design](message-broker.md#migration). Owner decisions from that
design apply: typed-only for agents in the end, humans keep free text, and Jev
may only flag, never accept or route.

## Why this phase exists

Phase 1 changes no delivery behavior. It gives agents a typed way to post,
records how every agent post measures up, and produces the data for the
phase-1 exit criterion: **at least 90% of agent posts are valid envelopes over
one week of real traffic**. Phase 2 (obligations, timers, Claude wake-up,
Discord) builds on the envelope and the checks table introduced here.

## Scope

In scope:

1. **Envelope type and validator.** A single shared Go definition used by the
   hub and `tt`.
2. **Storage.** Messages can carry an envelope. Old clients and the current UI
   keep working through derived display text.
3. **`tt send`.** A CLI for typed posts, with local validation.
4. **Shadow checks.** Every agent post gets a recorded verdict: typed-valid,
   text-convention-valid, text-convention-invalid or free text.
5. **Jev scoring in log-only mode.** Runs asynchronously after commit, fails
   open, and is disabled without a key.
6. **Measurement.** A read API and `tt message-checks --summary`.
7. **Templates.** The shared board-format block in `client/team-examples.js`
   tells agents to use `tt send`.

Out of scope, and must not change:

- Obligations, acks, timers, escalation, turn-end hooks, the relay, the directive
  core, and Discord. Those are phase 2 and later.
- Rejecting free-text agent posts (phase 4) or Jev-based rejection.
- Human posting paths and their behavior.
- Board UI rendering beyond what derived display text gives for free. A typed
  message renderer is a phase-2 UI item.

## Design

### 1. Envelope (`hub/internal/api/envelope.go`)

Implement the envelope exactly as specified in
[Typed message envelope](message-broker.md#typed-message-envelope):

- `kind`: `assign | request | review | question | result | answer | block | decline | finding | notice`
- `to`: agent name, `role:<role>` or `lead`
- `subject`: 10–120 characters
- `refs`: string map
- `body`: named fields per kind
- `evidence`: map of `{type, value, outcome}`
- `attachments`: artifact IDs or paths
- `due`: Go duration

Rules:

- `ValidateEnvelope(e) []Problem` is pure and deterministic. Each problem has a
  field path and a plain-English reason. It enforces the per-kind required body
  fields in the kinds table of the design and the subject rules.
- **Subject rules:** no `wi_`, `dly_`, `agt_` or `run_` style IDs; no hex runs of
  7 or more characters; no absolute paths.
- **Size:** body at most 4 KB, whole envelope at most 8 KB.
- **Named keys:** acceptance and evidence keys must match `^[a-z][a-z0-9]{0,15}$`
  (for example `a1`, `e2`).
- `to: role:<role>` is stored as written. Resolving a role to an agent is phase 2.
- `RenderText(e)` produces the derived display text: `KIND: subject`, then one line
  per field. This text is stored in `messages.text`, so every existing reader,
  inbox, relay preview and UI works unchanged.

### 2. Storage and posting

- **Migration:** add `{"messages", "envelope", "TEXT NOT NULL DEFAULT ''"}` to the
  column list in `store/migrate.go`, following the existing pattern.
- **API:** add `Envelope *Envelope` to `api.PostMessageRequest` and `api.Message`.
  `requestHash` must cover it, so a retried request with a changed envelope
  conflicts, as other fields already do.
- **When `Envelope` is present:**
  - The hub rejects the post with `400` and the problem list if validation fails.
  - It rejects the post if `Text` is non-empty and differs from `RenderText`.
  - Otherwise it stores both the envelope JSON and the rendered text.
- **When `Envelope` is absent:** behavior is exactly as today.
- **Humans:** human callers may send envelopes too, but nothing requires it.

### 3. Shadow checks (`message_checks` table)

```sql
CREATE TABLE IF NOT EXISTS message_checks (
  seq INTEGER PRIMARY KEY REFERENCES messages(seq),
  task_id TEXT NOT NULL,
  form TEXT NOT NULL,            -- typed | text_convention | text_convention_invalid | free_text | human
  problems TEXT NOT NULL DEFAULT '[]',
  jev_status TEXT NOT NULL,      -- pending | scored | unavailable | disabled | skipped
  jev TEXT NOT NULL DEFAULT '{}',-- noul name -> probability, plus model and latency
  created_at TEXT NOT NULL,
  scored_at TEXT NOT NULL DEFAULT ''
);
```

- **Same transaction:** the check row is inserted in the same transaction as the
  message, in both `PostMessage` and every other path that calls `insertMessage`.
- **Idempotent retries:** a retried request returns the original message and
  creates no second check row.
- **How `form` is decided for agent posts:**
  - `typed` when an envelope is present.
  - Otherwise `ParseTextConvention(text)` recognizes the phase-0 convention used by
    the current templates: first line `KIND: subject`, then `Field: value` lines.
    It returns `text_convention` when the parsed result passes
    `ValidateEnvelope`. It returns `text_convention_invalid` with problems when
    the first line matches but validation fails.
  - `free_text` when the first line doesn't match at all.
- **Human posts** get `form: human` and `jev_status: skipped`.

### 4. Jev scorer (`hub/internal/jev`)

- **Configuration:**
  - `TAILTERM_TYPESAFE_KEY_FILE`: path to the key. Unset → `jev_status: disabled`.
  - `TAILTERM_TYPESAFE_MODEL`: defaults to `jev-latest`.
  - `TAILTERM_TYPESAFE_URL`: defaults to `https://api.typesafe.ai`.
- **Request:** `POST {url}/v1/systemone` with `Authorization: Bearer <key>` and
  body `{state, model, questions}`. This is the wire format the official
  `typesafe-sdk` uses (see `tools/jev-kit/`).
- **Questions:** use the v2 wording measured on real traffic in
  `tools/jev-kit/run_board_real.py`: `ack_only`, `cut_off`, `manipulation`,
  `readable`. Add `subject_plain` for typed and convention posts: "`post.subject`
  is plain English a teammate understands without decoding identifiers."
- **State:** `{"post": {"kind", "subject", "text"}}` using named keys, never
  arrays. Secret-looking strings are redacted first with the kit's `SECRET`
  pattern.
- **Execution:** a bounded background worker in the hub process claims
  `pending` rows (oldest first, at most 4 concurrent, 3 s timeout, one retry).
  It never runs inside the posting transaction or the store write lock. Failures
  set `unavailable`. A restart resumes pending rows.
- **Egress limits:** the scorer sends only agent posts, and only while a key is
  configured.

### 5. Measurement

- **API:** `GET /v1/tasks/{id}/message-checks?after=&limit=` pages check rows, with
  a caller check that mirrors `listMessages`.
- **CLI:** `tt message-checks [--since 24h] [--summary] [--json]`. The summary
  prints:
  - agent-post counts by `form`
  - the typed + convention valid percentage
  - the top problem reasons
  - the count of each Jev noul at or above 0.5
  - Jev availability
  - per-sender valid percentage

### 6. `tt send`

```text
tt send --file msg.json
tt send --kind result --to lead --subject "Tests pass for empty --to rejection" \
        --ref commit=abc1234 --status a1=pass --status a2=pass \
        --evidence e1="go test ./cmd/tt → ok" [--reply-to SEQ] [--request-id KEY]
```

- It builds the envelope and runs `ValidateEnvelope` locally. On failure it prints
  every problem and exits 2 without contacting the hub.
- It posts with a request ID, generated when not supplied, and prints the stored
  sequence number.
- `--help` never contacts the hub, matching `tt post`.

### 7. Templates

- Update the `messageFormat` block in `client/team-examples.js` to say "Post with
  `tt send`" and give the one-line flag example.
- Keep the convention description as the fallback for hosts running an older `tt`.
- Regenerate the affected prompts in `docs/team-examples.md` so
  `tests/team-examples.test.js` still passes, with every prompt under 8 KB.

## Ownership

| Member | Owns |
| --- | --- |
| builder | `hub/internal/api/envelope.go` (+ tests), `api/types.go` fields, `store/migrate.go`, `store/store.go` insert paths, the new `store/message_checks.go`, the new `hub/internal/jev/`, `server/server.go` route + wiring, `cmd/tailterm-hub/main.go` env, `cmd/tt` `send` and `message-checks`, `client/team-examples.js`, `docs/team-examples.md` |
| planner | Confirms this plan against the code before assignment and sends lead any correction as a revised plan |
| reviewer | Two review rounds on the frozen candidate, per [the policy](two-review-round-policy.md) |
| database | Records the item, this order, review outcomes and the lead's release disposition |
| lead | Routing, acceptance and release disposition. Deployment happens only with the owner's go-ahead |

## Acceptance

| # | Criterion (observable) |
| --- | --- |
| a1 | `ValidateEnvelope` table tests cover every kind's required fields and each subject rule (ID, hex, path, length), plus size and key-format limits. Each rejection names the field. |
| a2 | `tt send` with an invalid envelope exits 2, lists every problem, and makes no HTTP request, checked with a test server that fails if contacted. |
| a3 | A valid `tt send` stores the envelope and `RenderText` text. `tt inbox` shows the rendered text, and `GET messages` returns the envelope. |
| a4 | Posting an envelope whose `Text` differs from `RenderText` returns 400. Posting an invalid envelope over HTTP returns 400 with the problem list. |
| a5 | A retry with the same request ID returns the original message with no second check row. Reusing the request ID with a changed envelope conflicts. |
| a6 | Every agent post gets exactly one `message_checks` row with the right `form` for these fixtures: typed, valid convention, invalid convention, free text. Human posts get `human`/`skipped`. |
| a7 | Free-text agent posts are still accepted unchanged. The existing `tt post` and hub tests pass without edits. |
| a8 | With no key configured, `jev_status` is `disabled`, and posting latency and behavior are unchanged. |
| a9 | Against a fake Jev server, a post is scored and `jev` holds each noul. When the server hangs or returns 500, the post still commits and the row ends `unavailable`. Pending rows resume after a hub restart. |
| a10 | Redaction: a post containing a token-shaped string is scored with `[REDACTED]` in place. The fake server asserts that it never received the secret. |
| a11 | `tt message-checks --summary` over the fixtures prints the correct counts and valid percentage. |
| a12 | The templates say "Post with `tt send`". `tests/team-examples.test.js` passes, and every prompt is under 8 KB. |

Required checks for a frozen candidate:

- `go vet ./...`
- `go test ./internal/api ./internal/store ./internal/server ./internal/jev ./cmd/tt`
- `npm test`

Four failures predate this order and aren't this item's responsibility: the Board
`view-lifecycle` test, `TestWorkItemsCLIUsesBodyFilesAndDurableReceipts`, and two
audit-export tests over `phase_successor_obligations`. Report them as unchanged
rather than fixing them here.

## Deployment and exit

Deployment follows existing release practice. The owner approves each step.

1. **Hub:** deploy the hub binary to TrueNAS with `TAILTERM_TYPESAFE_KEY_FILE`
   pointing at a mounted copy of the key. The key never goes in the repository or
   on the board.
2. **Mini:** install the new `tt` on the Mini, which must first be on
   `codex-cli` 0.156.1.
3. **Web client:** rebuild and deploy it.
4. **Shadow week:** run `tt message-checks --summary --since 24h` daily for a week.
5. **Exit:** phase 1 exits when agent posts are at least 90% typed or
   convention-valid for the week. Record any Jev noul whose rate of flags at or
   above 0.5 surprises the owner. That data sets phase-4 gate thresholds.

## Risks

- **Envelope drift between `tt` and hub:** both import the same `api` package, and
  the hub re-validates everything.
- **Egress:** Jev receives agent post text. Only agent posts are sent, they are
  redacted first, and the scorer is off without a key.
- **Convention parser false positives:** the parser only classifies posts; in
  phase 1 it never changes whether a post is accepted.
