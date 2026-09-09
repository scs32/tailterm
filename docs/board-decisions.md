# Board decisions

Feature `wi_65e8fd62e46a4eb8`, owner dispatch #494. Discovery order
`wi_65e8fd62e46a4eb8-plan-1` (#497), baseline `4e49888762ede18b46ae065dbbf5a0239101a318`
on `tasks-hub`, Stephens-Mini, `/Users/stephenspeicher/projects/tailterm`.
Implementation order `wi_65e8fd62e46a4eb8-build-1` (#500). This document defines
the implemented contract; release requires a separate recorded order.

## User flow

A worker uses `tt ask --request-id KEY --file request.json` (or `--file -` for
stdin) to post a decision request. Its question, two to five choices with distinct
IDs, labels and explanations, and one recommendation with a reason are durable.
An owner sees pending decisions in the Board even when their original messages
are older than the latest 200. The owner selects a choice or supplies a custom
answer, optionally explaining a selected choice. Nothing is submitted until the
owner presses Submit answer; a recommendation is never consent.

The submitted answer is an immutable human-authored reply directed to the
requesting worker and linked to the question. It appears in history and resolves
the pending card. Existing human-directed notification/resumption rules apply;
response storage does not claim retrieval or execution, and never starts a new
agent run. An answered question cannot be overwritten: discuss any subsequent
change in another message/request. Closed projects retain readable decisions but
reject new requests/answers. Ordinary text replies never silently resolve a card.

## Stable API contract

- `DecisionOption`: `id`, `label`, `description` (strings).
- `DecisionRequest`: `question`, `options`, `recommendedOptionId`,
  `recommendationReason`.
- `CreateDecisionRequest`: the above fields plus `agentId`, `requestId`, optional
  A1 `workItems` and `workOrderMessage` references.
- `AnswerDecisionRequest`: `requestId`, optional `optionId`, optional `text`,
  optional declared `agentId` (must be empty; only human authors answer).
- `DecisionAnswer`: `requestSeq`, optional `optionId`, optional `text`.
- Immutable `Message` adds optional `decisionRequest` and `decisionAnswer`.
- `DecisionRecord`: `request` (original Message), optional `answer` (reply Message).
- `DecisionList`: `decisions` array and optional `nextAfter` sequence cursor.

Routes under `/v1/tasks/{id}`:

- `POST /decisions` accepts CreateDecisionRequest, returns original request Message
  with A1-style postReceipt, status 201 including exact replay.
- `GET /decisions?after=0&limit=100` returns records in ascending request message
  sequence order, with a maximum page size of 100; nextAfter is present only when
  another page exists. The list includes pending and answered requests and remains
  readable after closure.
- `POST /decisions/{seq}/answer` accepts AnswerDecisionRequest, returns immutable
  answer Message with postReceipt, status 201 including exact replay.
- Existing receipt recovery also recovers these messages in their caller/project/
  declared-agent scope. Distinguish ordinary posts, decision creation and decision
  answers in receipt payload hashing so keys cannot replay a different operation.

Request keys use existing A1 validation. New requests require a current same-task
agent and no agent recipient; the message text includes the full question,
explained choices and recommendation so old clients/inboxes remain useful.
Text limits below are UTF-8 bytes, matching existing hub limits. Question length
1–2000, 2–5 options, each unique ASCII alphanumeric/underscore/hyphen ID 1–32,
label 1–120, description 1–1000, recommendation reason 1–1000.
Exactly one recommendedOptionId must match an option. Reject whitespace-only
required values; total rendered message text must fit existing message limits.
Selected answers require a valid option ID, with optional text up to 4000 bytes;
custom answers require nonblank text up to 4000. No automatic default answer.

Malformed/invalid references return 400, missing request/project returns 404,
closed project, already answered question or changed retry intent returns 409.
Agent-authored answer returns 403. Shared-workspace credentials still do not
provide authenticated per-agent or individual-owner roles; preserve honest
human-versus-declared-agent authorship without claiming stronger isolation.

The store owns resolution. Decision metadata, message, message event, allowed
resume, answer linkage and receipt commit atomically. First valid answer wins;
concurrent losers receive 409. Exact receipt replay precedes mutable closure,
item revision and agent lifecycle checks, and produces no extra events/resumes.
An answer links to the immutable question; do not revalidate its old work-item
revision as if the answer were a new ordinary A1 item reference.

Keep message projections immutable. Decision list is the separate mutable
projection and must refresh after answers and remote events; do not invalidate
A1's immutable-message incremental cache assumption. Decision reads may use the
existing encrypted bounded cache, with truthful saved/offline labels. Writes are
never queued. Preserve selected option, custom text and a stable key across a
failed/ambiguous submission and view refresh; exact retry must recover it. If a
user changes the payload after a failed attempt, use a new key and let the server
report whether an earlier answer already won. Show a concurrent winner after 409.
Decision-panel scroll and expanded answer history persist through redraws and
project navigation. History messages export all immutable request and answer
metadata.

## Ownership and acceptance

Lead owns `hub/internal/api/{types.go,decisions.go}`, CLI files
`hub/cmd/tt/{main.go,coordination.go,decisions.go,decisions_test.go}`, this document,
final integration and release artifacts. API owns only store/schema/server files
and its isolated Go tests. UI owns `client/board-view.js`, optional new
`client/board-decisions.js`, `client/{hub-client.js,cached-hub-client.js,style.css}`
and its new focused client tests. Existing workers may be explicitly resumed;
no new identities are required. QA receives behavior acceptance before reviewing
implementation and remains read-only except an explicitly owned fixture.

Acceptance: worker CLI creates the question and replay recovers it; Board displays
question, all explained choices and exactly one recommendation; selected/custom
answers persist after reload and produce one directed human reply; no auto-answer;
pending requests older than 200 messages remain reachable; concurrent answers and
lost-response retries are safe; closed projects are read-only; pending drafts
survive refresh/navigation/failure; remote answer replaces stale pending state;
keyboard and mobile controls fit the existing compact UI. Verify invalid payloads,
transaction failures, A1 receipts, unchanged legacy posts/history, migrations and
rollback using isolated databases/contexts only. Real Chromium/WebKit acceptance
must use an isolated real hub and the actual CLI before release.

Release targets are hub, both host CLIs, TailOS and both local previews. Use a
verified consistent SQLite backup and existing middleware/TCP listener, preserve
prior binaries/assets, and never change TrueNAS Tailscale. No live test tasks or
profiles, no unrelated roadmap implementation. Completion closes this feature
through db-handler after all required release acceptance succeeds.

## Worker command example

```json
{
  "question": "Which rollout should I implement?",
  "options": [
    {"id": "staged", "label": "Staged rollout", "description": "Verify the new behavior with a small group before enabling it broadly."},
    {"id": "all", "label": "Everyone at once", "description": "Enable it for everyone after testing; a problem would affect all users."}
  ],
  "recommendedOptionId": "staged",
  "recommendationReason": "It limits the impact of a problem while preserving a clear rollback path."
}
```

`tt ask --request-id rollout-1 --file decision.json` supplies task/agent identity
from the existing environment, not the file. Optional A1 item/order references
may be included in that file using their existing shapes. The command rejects
unknown fields, trailing JSON, missing identity/key and invalid content without
posting. `--json` returns the stored Message. Retrying uses the same key and file;
an ambiguous failure prints recovery/retry guidance rather than generating a new
key. A successful request does not itself mark the worker blocked: if dependent
work must pause, use the existing `tt event needs_input --text ...` after storage;
continue independent work under its existing order.
