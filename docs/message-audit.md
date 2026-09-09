# Message audit foundation (A1)

Feature `wi_abc84eb23688d903`, order `wi_abc84eb23688d903-a1`, recorded #454.
The A1 source contract and isolated acceptance checks were accepted by lead in
#480; deployment is a separate recorded release. See [the plan](work-item-audit-implementation-plan.md)
for later intake, correction, client, enforcement and AIV slices.

Ordinary message posting remains compatible with existing clients. New callers
may attach exactly one same-project primary work-item reference and an optional
same-project recorded work-order message. The item revision must be current for
a new ordinary post. These are structured references; no text parsing attempts to
prove that an order message contains an approved assignment.

```json
{
  "text": "Candidate verification is ready.",
  "requestId": "candidate:check.1",
  "workItems": [{
    "itemTaskId": "tsk_0000000000000001",
    "itemId": "wi_0000000000000002",
    "itemRevision": 3,
    "relationship": "primary"
  }],
  "workOrderMessage": {"taskId": "tsk_0000000000000001", "seq": 12}
}
```

The route remains `POST /v1/tasks/{id}/messages`. Typed references require a valid
request ID (1–128 ASCII letters/digits or `-_.:`). An order reference requires a
primary item. A request ID without typed context also enables recoverable posting.
Unkeyed legacy messages retain the existing behavior and omit new receipt fields.

A successful post returns the message with `workItems`, optional
`workOrderMessage`, and `postReceipt` for keyed posting. The receipt contains
`id`, `requestId`, `taskId`, `messageSeq` and `createdAt`. It proves storage only.
Exact retries recover that message and receipt without another message/event or
agent resume. Reusing the key with different text, recipient, reply or context
conflicts. Later task closure or item edits do not change an existing receipt;
new writes still obey current task/validation rules. Message lists include the
same stored context and receipt metadata for keyed posts, so existing JSON history
exports retain these references.

Recover an uncertain response with
`GET /v1/tasks/{id}/messages/receipts/{requestID}?agentId={declaredAgentID}`;
omit `agentId` for a human-authored post. The Go client provides
`GetMessagePostReceipt(ctx, taskID, requestID, agentID)`. Recovery uses the same
caller-identity and rate-limit checks as posting, and
matches project, caller node/user and declared agent scope. The current server
has no separate read-only-versus-writer credential role. It returns
200 with the original message, 404 for no matching receipt and 400 for malformed
input; identity failure returns 403 and rate limiting returns 429. Exact POST replay retains 201;
a changed payload under the same key returns 409. Malformed, missing or foreign
ordinary item/order references fail with 400; a stale current item revision or a
new write to a closed project fails with 409.

Scope checks retain the existing shared-workspace trust boundary. Agent labels
are not per-agent credentials, and a receipt key is not an authorization token.
Board message visibility and recipient ownership remain unchanged. All agent
work-item database administration still goes through db-handler.

Existing work-item dispatch uses its validated source item to add a primary link
to the destination message, including existing human cross-project dispatch.
Source ownership stays unchanged. Arbitrary cross-project ordinary posts and
agent-authored cross-project dispatch remain rejected. Dispatch continues using
its own existing dispatch receipt; it does not require an ordinary posting key.

Storage must commit message, link, message event, any permitted human-directed
resume and posting receipt together. Validation or insertion failures leave no
partial effects. List/history responses retain creation-time context after the
item changes. A1 adds no correction API or typed order/assignment tables.

There are no new UI selectors, CLI posting flags, required-mode switch or AIV
adapter in this slice. Legacy posts can remain unlinked while compatible clients
are developed. Those later capabilities require their own recorded work orders.

Acceptance evidence: the full Go suite and vet passed; store/server race checks
passed. Independent QA exercised seven HTTP scenarios, including fourteen invalid
reference cases, concurrent duplicates, lost responses, lifecycle replay and
cross-project dispatch. Lead also verified all 101 JavaScript tests and the real
isolated-hub work-item browser flow in Chromium and WebKit. Store failure injection
covered links, message/resume events and receipts; the additive migration fixture
preserved legacy messages, work-item history, retirement/cleanup and encrypted
profiles. All validation used synthetic records.

The separate release order `wi_abc84eb23688d903-a1-release-1` (#483) targets only
the hub. An isolated downgrade check built the pre-A1 source `662ed72`, read and
wrote the migrated synthetic database with that old binary, then restarted A1:
the original typed receipt and the new legacy message survived, with SQLite
integrity and foreign keys valid. The initial check script assumed a bare message
array; correcting it to use the existing `messages` response envelope passed.
Application rollback retains the additive audit tables. Older clients/binaries
can still write unlinked messages; A1 does not claim strict enforcement.
