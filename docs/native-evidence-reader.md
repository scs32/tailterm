# Complete native work-item evidence reader

Feature `wi_618c8ff87e6b8061` revision 5, bounded implementation order **#4235**
(saved by the database handler in **#4245**), adds a read-only evidence collector
for the later handler assessment consumer #3694. The candidate is not deployed.
It does not schedule work, complete an item or Queue entry, assess whether the
retrieved content is correct, or add another watcher.

## One explicit native read

Only the project's registered `database_handler` role may run the command. The
caller chooses the required source families; `current` is mandatory because its
item revision and scope revision anchor the read.

```text
tt work-items evidence \
  --manifest /private/path/item-evidence.json \
  --sources current,revisions,messages,receipt \
  --receipt-request-id stable-update-key \
  --receipt-agent agt_... \
  --limit 32 \
  wi_...
```

The source selector is closed and maps to distinct native APIs:

- `current`: `GET /v1/tasks/{task}/work-items/{item}`
- `revisions`: `GET .../revisions?after=N&limit=N`
- `messages`: `GET .../messages?after=N&limit=N`, optionally with
  `--message-revision`
- `receipt`: `GET .../updates/receipts/{requestId}?agentId=...`

This prevents a current-item response from being relabeled as revision history
and prevents audit summaries from substituting for full linked messages or an
exact keyed mutation receipt. Receipt lookup uses the supplied request key; it
does not infer a receipt from the current revision.

## Manifest and restart behavior

The mode-0600 manifest freezes the task, item, ordered source selection, page
limit, message filter and receipt coordinates. A retry must use the identical
selection. Every HTTP response is retained byte-for-byte in an adjacent private
page directory. The manifest records each page's relative path, byte count,
SHA-256, native operation and parameters, status, start/end timestamps,
validity and returned cursor. Existing pages are size/hash checked before a
resume.

Revision and linked-message reads follow `nextAfter` until zero. Every page must
advance exactly and retain identical native `HistoryCoverage`; its observed
revision must match the current-item anchor. The reader rejects repeated or
nonadvancing cursors, malformed coordinates, changed coverage, incomplete native
revision coverage, missing page files and hash changes. Successfully retained
pages remain in the manifest across a transport or HTTP failure, and a later
identical invocation continues after the last valid cursor rather than starting
the evidence claim over.

The current item is fetched before and after the selected families. A changed
item revision or scope revision makes the manifest incomplete instead of mixing
two observations. These are consistency fences around independently
transactional native reads, not a claim that the hub provides one database
snapshot across every HTTP request. A mutation immediately after the final
bookend can only be observed by a new manifest/read.

## State and interpretation

Each required source is exactly one of:

- `not_fetched`: no native lookup has completed;
- `partial`: at least one valid page exists but native terminal metadata has not
  been reached;
- `absent_after_complete_lookup`: the exact native getter returned 404;
- `error`: transport, HTTP, decoding, cursor, coordinate, coverage or snapshot
  validation failed;
- `verified`: the selected source reached its native terminal condition and all
  retained page bytes passed structural checks.

Manifest `complete` means every requested family is terminal (`verified` or
`absent_after_complete_lookup`). Manifest `verified` is stricter and requires
every family to be `verified`. Thus a completed lookup that proves absence is
not mislabeled as verified evidence. `not_fetched`, `partial`, and `error` can
never support a completeness claim.

This structural verification records source provenance and availability only.
It does not accept a work result or prove that source prose is semantically
sufficient. Linked-message coverage means the hub's explicit work-item links,
not every Board message. The existing shared workspace credential and claimed
agent identity remain integrity checks rather than cryptographic per-agent
authentication.

## Acceptance fixtures

Focused synthetic HTTP fixtures cover page-two recovery, distinct current versus
history getters, exact escaped receipt lookup, a retained transient error and
resume, repeated cursors, an item/scope change at the final bookend, terminal
receipt absence, private raw page hashes and the rule that an incomplete
manifest cannot claim completion. They use no live task, Queue, profile or Board
bytes.
