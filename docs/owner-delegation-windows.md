# Owner delegation windows

Feature `wi_2f6f24bc62b24a7a` revision 1, work order #13877, lead ASSIGN #14832,
builder Start #14841 (plan #14807). Base `tasks-hub` at `ef117cf`. Owner-question
defaults Q1–Q4 from the plan apply: matrix approvals are always excluded, owner-request
overdue escalation is unchanged during a window, the delegate is an exact agent, and
windows are per project.

## What it does

The owner opens a **delegation window** for one project: a delegate agent, an end
time and a scope. While the window is open, the owner's decision requests in that
project are **routed** to the delegate, which answers them with a required
**rationale**. Every delegated answer is marked as delegated and listed for the
owner. When the window ends (at its time or when the owner closes it), unanswered
routed requests are **handed back** to the owner and new requests are no longer
routed.

Two kinds of owner decision are routed:

- **Owner requests**: a typed REQUEST to `owner` (`tt send --kind request --to owner`),
  which creates an owner obligation.
- **Board decisions**: `tt ask`.

## Scope and categories

A request's category is `decision` (default), `merge`, `deploy` or `matrix`:

- owner request: `refs.category` (for example `tt send ... --ref category=deploy`);
- board decision: the `category` field of the `tt ask` file.

| Scope                      | Routes                        |
| -------------------------- | ----------------------------- |
| `decisions`                | `decision`                    |
| `decisions_merges_deploys` | `decision`, `merge`, `deploy` |

**Matrix approvals never leave the owner.** Any request that mentions
`verification-matrix-approval:` anywhere (expected answer, ask, question, options,
subject or refs) is `matrix` whatever it declares, and no scope covers `matrix`.
An unknown declared category is not covered either. A delegate's answer or
rationale containing that token is refused, and a delegated answer is authored by
the delegate agent, so it can never satisfy the verification plan's owner matrix
approval binding (`from_agent` must be empty).

Categories are declared by the requester; the shared-workspace model cannot
prove that a request declared `decision` is not really a deploy. The matrix token
check is the one hard exclusion.

## Routing

A request is routed when an open window covers it:

- in the same transaction that creates it, if the window is open and before its end;
- when a window opens, for every covered request still waiting on the owner.

Still waiting means, when a window opens:

- an owner request whose obligation is not closed. A withdrawn or superseded owner
  request is closed, so it is not routed;
- an unanswered decision whose requester is not closed or exited. A decision has no
  state of its own and cannot be withdrawn or superseded, so its requester's status
  is the only test. A decision left out stays with the owner, who can still answer it.

It is never routed when it is a matrix approval, outside the scope, **authored by
the delegate itself** (no self-approval), or when the delegate agent is closed or
exited. Routing records a route row and sends the delegate a directed NOTICE with
the request text and the exact answer command. The notice links the delegate's own
bound work item when it has one, so an item-bound delegate's inbox shows it. Opening
a window also sends the delegate one notice saying it is the owner's delegate.

The owner request stays an owner obligation: the owner can still answer it first,
it is never reassigned to an agent, and its overdue escalation still reaches the
owner.

**The owner's own session as delegate.** Register it as the project's owner helper
(`tt helper register --task T`, see [owner helper](owner-helper.md)) and open a window
with `--delegate owner-helper`. It answers through `eval "$(tt helper env --task T)"`
followed by the commands below; matrix approvals still stay with the owner. The window
names that exact helper agent. After a pause or a close, registering again creates a
new helper that the old window does not reach: end the old window and open a new one
(see [owner helper](owner-helper.md#opening-a-window-for-the-helper)).

## Answering as the delegate

```sh
tt owner answer OBLIGATION_ID --text "..." --rationale "..."     # or --approve --rationale "..."
tt ask answer SEQ --option ID --rationale "..."                  # or --text "..."
```

The hub checks at answer time, independent of the broker: the caller is the
window's delegate and its current run, a route exists, the window is open and
`now < endsAt`, the request is still open, not returned and inside the scope.

| Condition                                                 | Status |
| --------------------------------------------------------- | ------ |
| blank or missing rationale (over 2000 bytes)              | 400    |
| another agent, a stale run, or no window routes it to you | 403    |
| window ended, request returned, outside scope, matrix,    | 409    |
| already answered (by the owner or the delegate)           |        |

A stale run of the delegate gets 403 whether or not the request was routed to it.
The delegate's current run gets 409 for a request that is outside the scope or was
never routed.

An agent with no window keeps its previous behaviour: decisions answer 403, owner
requests need a per-request `tt owner delegate` session grant (unchanged). An owner
answer never takes a rationale.

The delegated answer is a typed ANSWER authored by the delegate, directed to the
asker, with `refs.delegated=true`, `refs.delegation=<window id>` and
`refs.onBehalfOf=owner`, and the rationale in `body.reason`. A delegated decision
answer also records whether it followed the recommendation.

## Ending and hand-back

- **Expiry**: the broker's first tick at or after `endsAt` marks the window
  `expired`, in paused projects too. Answers are already refused from `endsAt` on.
- **Owner close**: marks it `closed` at once.
- Opening a new window while an old one is open but past its end expires the old
  one first.

Ending posts one NOTICE to the delegate ("do not answer; returned to the owner") and
one board NOTICE listing the returned requests. Only an open window ends, so
repeated ticks and restarts post nothing more. Requests the owner already answered
are not handed back. At most one window is open per project; change one by closing
it and opening another.

## Reminder before the end

Bug `wi_9898f0ff7e8e1464` (work order #54280). The hub reminds before a delegation
window ends, so renewing it does not depend on the owner helper remembering.

- **When.** Opening a window saves one reminder at `endsAt` minus half the window,
  at most 24 hours, truncated to the whole UTC minute: a 7-day window is reminded
  24 hours before its end, a 3-hour window 90 minutes before. A window with no whole
  minute between the open and that slot (windows shorter than about two minutes) gets no
  reminder.
- **How.** In the open's transaction the hub posts one undirected board NOTICE,
  "A reminder is set before the delegation window ends", with refs
  `delegationReminder=<window id>`, `ownerWake=authority-expiry` and `wake=<wake id>`.
  It names the window, the delegate, the end and reminder times, and what to do: to
  keep the delegation, close this window and open a new one before it ends
  (`tt owner delegation close <id>`, then `tt owner delegation open ...`); otherwise
  it ends and unanswered routed requests return to the owner. The hub then saves an
  `authority-expiry` wake on the owner helper's hub wake schedule
  (`docs/session-handoff.md`, "Wakes on the hub"): zone `UTC`, a single-minute
  schedule due at the slot, `expiresAt = endsAt`, written as the owner (no agent or
  run), with that notice as its Board message and request ID
  `delegation-reminder-<window id>`.
- **Delivery.** The broker's existing wake sweep delivers it; there is no second
  sweep. At the slot the current owner helper (running, done or needs input) gets
  the standard directed notice "Owner-helper wake: authority expiry due HH:MM UTC",
  once across ticks and restarts, and the wake is then `expired`. With no live owner
  helper the slot is `helper-absent` and the owner gets one report naming the
  reminder's Board message. Late and undelivered slots are reported to the owner as
  for any wake. In a paused project the slot is recorded `skipped-paused` with no
  notice; the window still expires on time.
- **Cancellation.** Ending the window before the slot (owner close, broker expiry,
  or an open that expires a past-end window) cancels the reminder in the same
  transaction, even after the helper changed the wake, so no reminder is posted for
  an ended window. A renewal (close, then open) therefore leaves one active reminder,
  for the new window. A reminder that already fired stays `expired`. The broker
  expires windows before it sweeps wakes, so a hub down past both the slot and the
  end posts no reminder.
- **Cap.** The reminder is saved even when the project already has 20 active wakes
  (an open never fails for the cap); it counts toward the 20 for later wake writes.
- **Older windows.** A window opened before this release has no reminder; its next
  renewal creates one. The hub does not backfill.
- An owner helper that kept its own `authority-expiry` wake for a window should
  cancel it (`tt handoff wake cancel`), or it is reminded twice.

## Surfaces

**tt** (owner only; refused from agent sessions):

```sh
tt owner delegation open --delegate lead-x --for 3h --scope decisions-merges-deploys [--reason T] [--request-id K]
tt owner delegation open --delegate lead-x --until 2026-09-30T18:00:00-07:00
tt owner delegation close dlw_... [--reason T]
tt owner delegation list [--json]
```

A repeated `open` reuses its request ID: the one given with `--request-id`, or one
derived per minute from the delegate, `--for`/`--until`, scope and reason. The
hub's replay is the open-time snapshot, so `tt` checks the window's current state
and never prints a closed or expired window as open:

- **Matching open window**: printed as the first call printed it; nothing new
  opens. A repeated `--for` measures from a later moment, so its end time differs;
  `tt` then picks the newest window with the same delegate (name or agent ID),
  scope, reason and length and resends once with that window's end time.
- **Closed or expired window**: `tt` prints when it was closed or expired (or its
  end time, if the broker has not expired it yet), names the open window or says
  none is open, and exits non-zero. Open a new window with a new request ID.
- **Explicit `--request-id` with a different `--for`**: refused with the stored
  window's ID and length, not replayed. The hub stores no duration, so `tt`
  compares `--for` with the window's end minus its start: the stored length may
  be up to 3 s shorter (end-time truncation and request latency) or 1 s longer.
  A `--for` that differs by less than that cannot be told apart without a hub
  change (review #28131 f2). The hub stamps the start when it saves the open, so
  a first call that reached it more than about 3 s late stores a shorter window:
  a retry with the same explicit request ID and `--for` after such a slow first
  call can be refused this way, as a different duration. The window the refusal
  names is the one that call stored, and it is open: check it with
  `tt owner delegation list` instead of opening again. The exact fix (the hub
  storing the requested duration) is declined because the refusal fails safe. A
  derived key already holds `--for`, so it allows 15 s either way for latency and
  the two hosts' clocks differing.
- **Own window ended, a newer matching window open**: when the newest matching
  window was opened under another request ID, the resend is refused. Only one
  window is open per project, so this request's own window has ended; `tt` says
  so, names the open window with its delegate and end time, and exits non-zero.

**Discord** (allowlisted owner in the project's channel): `/delegate agent until scope
[reason]` (`until` is a duration such as `3h` or an RFC3339 time) and
`/delegate-end [reason]`. The interaction and user IDs are recorded as the source;
redelivered interactions change nothing.

**TailOS** (Board): a compact strip above Owner requests. With no window:
_Delegate decisions…_ (agent, end time, scope, Open). With a window: _Delegated to X
until T · scope_ and _End now_. Routed owner requests show _Delegated to X_.
_Delegated answers (n)_ lists every delegated answer with request, delegate, time,
answer and rationale, including after the window ends.

**API** (`/v1/tasks/{id}`; owner routes replay by request ID through `owner_actions`):

- `POST /delegation-windows` `{delegate, endsAt, scope, reason?, source?, requestId}` → 201
  (200 replay) `OwnerActionResult.window`. 400 invalid delegate/scope/end
  (1 minute to 7 days); 409 when a window is already open.
- `POST /delegation-windows/{wid}/close` `{reason?, source?, requestId}` → 201; 409 when
  it already ended.
- `GET /delegation-windows` → `{windows: [...]}`, newest first, each with its `routes`
  (request, category, notice, returned time, answer, delegate, rationale, time).
- `ObligationAnswerRequest.rationale`; `AnswerDecisionRequest.agentId/runId/rationale`.

The Discord bridge credential may call exactly these three routes in addition to its
existing ones.

## Storage

Additive tables `owner_delegation_windows` (partial unique index: one `open` per
project), `owner_delegation_routes` and `decision_request_categories`. The decision
category is kept beside the immutable request because `decision_requests` is created
after the phase-3 migration runs; the message projection does not carry it. Older
binaries ignore these tables.

## Verification

```sh
cd hub
go test ./internal/api ./internal/store ./internal/broker ./internal/server ./internal/bridge ./cmd/tt
go test ./internal/store ./internal/server ./internal/broker ./internal/bridge ./cmd/tt -run 'Delegat' -count=1
cd ..
node --test tests/owner-delegation.test.js
node tests/owner-obligations-browser.mjs
```

All tests use temporary hubs and databases, fake Discord and disposable browser
contexts. The browser suite drives the real Board in Chromium and WebKit against a
real loopback hub: it opens a window from the strip, survives a reload, shows the
routed tag, lists the delegated answer with its rationale, ends the window and keeps
the answer listed after another reload.
