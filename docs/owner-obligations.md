# Owner obligations

Feature `wi_deb2ce3cda1968d0`, revision 1, owner intake #11941 and work order #11942; builder assignment #12043 and exact Start release #12046. Base: `4b5adb258528a5f788a6873bebe9f9b2c7b802d4`. Owner decisions #12007, #12008 and #12029 authorize a 30-minute default, explicit delegated sessions and the existing Board/TailOS plus Discord escalation path. Generator ownership supplement #12052/#12055 and withdrawal compatibility supplement #12059/#12062 preserve the same item scope.

## Request and answer

An agent's typed REQUEST to `owner`, or an effective board-wide typed REQUEST, creates one owner obligation in the message transaction. Other kinds and human board-wide posts keep their existing behavior. An exact primary item link is required, either supplied explicitly or inherited from the sender's exact admitted run. The original item revision, order, message and receipt remain attached. There is no fake owner agent. Existing rows migrate as agent obligations; older board messages are not backfilled.

```sh
tt send --kind request --to owner --subject "Approve the verification matrix" \
  --ask "Approve these exact matrix bytes" --expected-answer 'APPROVED exact-token' \
  --work-item ITEM_ID --work-item-revision REVISION --work-order-message ORDER_SEQ \
  --request-id STABLE_KEY
tt obligations --owner
tt owner answer OBLIGATION_ID --approve --request-id STABLE_ANSWER_KEY
```

`--due` overrides the default using the existing positive duration limit of seven days. `--expected-answer` retains up to 2000 bytes of exact request-only text. Approve reads those bytes from the stored request, rather than from button content. The answer envelope retains whitespace and Unicode exactly. A free-text owner answer or linked human reply is an answer, not inferred approval.

A linked human reply or the owner answer endpoint closes the request once and directs the answer to the requester when available, without creating another obligation or resuming a retired requester. Concurrent answers serialize; retrying an owner action with its unchanged request key returns its recorded result. A retry with changed bytes conflicts. Requester withdrawal uses its current run and original author checks, remains idempotent, and creates a linked delivery-only notice.

## Delegation and attribution

The owner can explicitly grant a session authority over one open owner request:

```sh
tt owner delegate OBLIGATION_ID --session SESSION_ID --authorization OWNER_REFERENCE \
  --agent AGENT_ID --run RUN_ID --request-id STABLE_GRANT_KEY
tt owner answer OBLIGATION_ID --session SESSION_ID --approve --request-id STABLE_ANSWER_KEY
```

Agent/run are optional together for a non-agent delegated session. An agent delegate must match the recorded current run. Ordinary agent replies, missing grants, differing identities and stale runs cannot settle an owner request. Answers retain the grant ID, session identity and on-behalf-of-owner refs, with the submitting caller and optional Discord source. The grant stores its owner authorization reference and issuer. These checks follow the existing shared-workspace caller model: they are audit controls, not a new independent authentication boundary for sessions sharing owner credentials.

## Surfaces and escalation

`tt obligations --owner` is read-only and never acknowledges or wakes an agent. Board fetches pending owner requests separately from its latest 200 messages, showing full request text and Answer/Approve controls. Queue and Delivery derive waiting counts and increasing age from exact primary task/item links. Resolved requests disappear on refresh, and answer mutations invalidate the cache. Age clocks update the DOM without repeated network reads.

Discord sends full owner request text in bounded multipart embeds, with one Approve button on the last part. Existing guild/channel/owner allowlist checks and the outbox reconciliation path remain in force. Successful approval records the interaction/user source. Duplicate, unauthorized, wrong-channel and resolved clicks cannot produce a second answer.

At the deadline the broker creates one durable, linked escalation on Board/TailOS and the existing bridge mentions the owner. Tick/restart retries do not repeat it. Owner requests bypass agent acknowledgement timeouts, wakes, recipient-gone closure and project-stall alerts. Extending after escalation moves the due time without rearming that escalation. Owner requests cannot be reassigned to an agent. Project pause handling remains inherited from the broker's existing task pause gate. No new notification transport is introduced.

## Verification

All tests use temporary hubs/databases, fake Discord or disposable loopback browser fixtures. No live hub, bridge, agent or profile is a fixture.

```sh
cd hub
go test -timeout 120s ./internal/api ./internal/store ./internal/server ./internal/broker ./internal/bridge ./cmd/tt
go test -race -timeout 120s ./internal/store ./internal/broker ./internal/bridge ./internal/server ./cmd/tt -run '^TestOwner' -count=1
cd ..
npm test
node tests/owner-obligations-browser.mjs
```

The browser fixture exercises real hub/CLI endpoints and real Board, Queue and Delivery modules in Chromium and WebKit, including more than 200 messages, two items with multiple requests, increasing ages, exact approval and resolution refresh. The fake bridge tests preserve full long Unicode text through retry reconciliation and assert one approval button and one overdue mention. The implementation candidate still requires two serial reviews and handler-saved acceptance. This work order excludes push, merge and deployment.
