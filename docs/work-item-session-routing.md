# Work-item session routing

Implementation record: `wi_c97ba465a6f2a3ab`, order
`wi_c97ba465a6f2a3ab-routing-1`, Board assignment #814. This contract is a
candidate until it is accepted and released through the database handler.

## Contract

An ordinary agent may be admitted without item metadata for compatibility, but
the reusable-team item route binds every new worker to exactly one bug or feature.
The binding is immutable for that agent/run and records:

- item project, ID and exact launch revision;
- the structured, item-linked bounded work-order message;
- the Board message sequence through which the new inbox starts;
- an optional prior agent identity that this new session replaces;
- the SHA-256 digest of the exact prepared context injected into the model.

The inbox cutoff and binding are committed in the same transaction as the agent
and `agent_added` event. Messages before the cutoff are not replayed. Later inbox
reads for a bound worker include only messages with the same explicit primary
work-item link. Recipient, reply chain, timing and prose are never treated as
evidence of relevance.

The model context bundle is prepared before admission by the database handler or
the human browser launch flow. Preparation consumes the accepted immutable
history routes from `docs/work-item-revision-history-plan.md`: exact revision,
all revision pages, all gap pages and all explicit source/A1 message-link pages.
The bundle retains their source coordinates, provenance, coverage and gaps. It
does not create another revision/snapshot authority and does not include ordinary
Board history. The stored bundle is capped at 128 KiB and is never truncated; an
oversized bundle fails with instructions to consolidate the durable item record.

`GET /v1/tasks/{task}/agents/{agent}/work-context?runId={exact-run}` returns only
the immutable stored bundle and binding. A stale run conflicts. `tt context`
uses that route; it cannot ask the hub for a different item. `tt spawn` accepts
the prepared bundle through `--work-context-file` or the browser-only JSON path,
then injects it into the initial model briefing and exports the bound item
coordinates in the new tmux session.

Bound-agent `tt post` messages automatically carry the item, considered revision,
recorded order and a content-derived retry identity. Database-handler messages
may supply the same explicit flags. This makes new requirements, decisions,
artifacts, results and evidence eligible for later history bundles without
classifying unlinked conversation.

## Replacements and lifecycle

A model change launches a new name, agent ID, run ID, tmux session and runtime
thread. `--replaces-agent` links it to a prior agent bound to the same item. The
old agent row, run, terminal session, messages and results are not modified or
closed. An exited item-bound identity cannot be restarted by an old unscoped
client; a fresh replacement and freshly prepared bundle are required. Exact-run
context lookup and lifecycle events prevent a stale process from adopting the
replacement's identity or context.

Parentless browser/team admission remains a normal base-team addition and does
not consume `maxNewAgents`. A nonempty parent remains a helper and all existing
spawn-enable, lifetime helper allowance and active-agent checks still apply.
Partial team retries reuse the in-memory prepared bundle and deterministic
item-suffixed names only for members that have not launched. Registration or
context failure leaves no partial binding; a host-process failure closes only the
new failed agent record under the existing cleanup behavior. Task-owned groups,
retirement versus closure and run-scoped cleanup receipts are unchanged.

## Compatibility and release boundary

Existing agent, task, message, work-item and history response shapes remain
additive. Unbound clients keep their existing Board/inbox behavior. The new
history routes are a required capability for the human reusable-team item flow;
an older hub returns an error before launching any member and the client does not
fall back to a mutable current-row or prose-derived bundle.

This implementation order authorizes isolated candidate tests only. It does not
authorize a live database migration, hub/CLI install, session restart, production
deployment or closure of any existing project/agent.
