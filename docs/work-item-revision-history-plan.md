# Work-item revision history plan

Work item: `wi_f0d39d54a76011c7`
Planning order: `wi_f0d39d54a76011c7-plan-1`
Inspection baseline: `tasks-hub` at `54b25f1d8080ac5e85370497b5dedc7a2a44063c` on
`Stephens-Mini`, `/Users/stephenspeicher/projects/tailterm`
Scope: read-only inspection and a proposed server/data contract. This artifact does
not change product code, schema, live data, deployment, or the separate narrative
report/AIV roadmap.

## Lead acceptance and user flow

Planning order #740; API result #789 accepted after review #771. Lead verified the
reported artifact hashes and reran the isolated sizing program successfully.
This is an accepted implementation contract, not an implemented or released fix.

The owner opens Bugs or Features, creates or edits an item and saves it. A confirmed
save becomes a new immutable revision with the original actor and time. From the
item, a compact history control will list revisions and display the selected
version and its explicitly linked Board messages. Later edits never change that
selected version. Closed projects allow history reads. Concurrent changes retain
the draft and explain the conflict; an uncertain save can recover its original
receipt without another edit. Missing old evidence is labelled as a gap.

Lead UI investigation used a real throwaway hub with synthetic data. Both current
54b25f1 and pre-dropdown c0396e1 passed 8/8 cases each: Bugs/Features,
Chromium/WebKit, 1440/390px, actual create/edit, independent persisted-value reads,
revision increments and reload. This does not reproduce or repair the reported
Bugs failure. Owner failure details requested in #752 remain pending; history work
can proceed independently, but the original bug cannot be called fully repaired.
Detailed evidence: `.build/work-item-history/ui-findings.md`,
`crud-investigation/results.json` and `crud-baseline/results.json` beneath that
artifact directory. No live owner data was used.

Lead implementation clarifications:

- Keep existing create/dispatch response shapes unchanged. Fetch an exact revision
  when immutable content is needed; do not add an optional second response design.
- Encode all public Go fields with explicit existing lower-camel JSON conventions.
  A history page and its coverage must come from one SQLite read transaction;
  concurrent later edits may appear on later pages without repeating earlier rows.
- Classify reconstruction before inserting rows. If the final replay disagrees
  with the current row, that disputed final revision is not a verified prefix row;
  insert its explicitly labelled checkpoint instead. If a previously materialized
  immutable row already occupies that revision with different content, preserve it,
  record the discrepancy and return partial coverage; never overwrite it or fail
  the whole hub just to insert a checkpoint. Prospective edits still snapshot their
  actual resulting current state. Cover both cases with isolated corruption tests.
- Legacy source/change records and exact A1/dispatch coordinates are retained even
  when their content disagrees. Link disagreement must not silently replace a
  verified snapshot or label unrelated discussion as known history.
- Draft/key continuity in the later browser slice must use the existing encrypted,
  credential-scoped vault where persistence is required, with bounded storage and
  explicit dismissal cleanup. No plaintext local storage or offline mutation queue.

## Bounded delivery stages and ownership

1. **History foundation:** additive snapshots/gaps/state, migration/reconciliation,
   snapshots in existing create/PATCH transactions, bounded revision/gap/message
   readers and matching Go client. Root owns shared schema/types and integration;
   API owns store/service/client implementation files under explicit assignment.
   Acceptance is the snapshot/read/conversation/migration/byte-budget matrix below,
   including actual old/new/old/new executable compatibility. Current clients keep
   working. No keyed update, browser UI, CLI commands or deployment in this stage.
2. **Recoverable editing and history UI:** after foundation acceptance, a separately
   recorded order activates keyed updates/receipt recovery, CLI history controls
   and compact shared Bugs/Features UI. Root owns shared contracts/integration, API
   server/data files, UI client files, and an independent reviewer tests behavior
   against an isolated real hub. Exercise failures, concurrent edits, lost responses,
   history after reload, mobile layout and closed-project reads.
3. **Release and final acceptance:** record a release order for required hub/CLI and
   TailOS/Mini changes, preserve additive rollback and verify deployed identities.
   Resolve the reported Bugs failure with its actual sequence if available; do not
   turn passing fresh CRUD cases into a causal repair claim. Save the complete final
   write-up through db-handler before any Done transition.

Each stage needs its own committed work order before product changes. Scope remains
this bug; narrative reports, historical-comment writes, broad dispatch viewing,
versioned exports and AIV stay separate. Planning worker acceptance releases the
worker until an explicit resume with a concrete implementation assignment.

## Outcome

The current database retains enough information to reconstruct the normal edit
history of every work item created through the shipped store: revision 1 records a
full core-field map, and later change rows record the new values of every supplied
field with an actor and timestamp. That history is not exposed through the store,
HTTP API, Go client, browser client, CLI, or task-history export, and it is not
protected by a uniqueness constraint or a full immutable snapshot at each revision.

The smallest safe implementation is:

1. Add an immutable, columnar `work_item_revisions` snapshot table and transactionally
   materialize every future create/edit into it.
2. Backfill each item by replaying its existing change chain. A malformed item's
   verified prefix, exact original rows and current-row checkpoint remain available
   with an explicit gap record; the anomaly does not disable unrelated hub work and
   never causes current text to be copied backward into an unknown revision.
3. Keep legacy `PATCH` working, but introduce a distinct keyed update endpoint for
   new clients. A distinct route is important: an old server would ignore an additive
   JSON `requestId` on the existing PATCH and could commit an unrecoverable edit while
   the new client mistakenly believed it was idempotent.
4. Expose bounded revision reads and truthful message-to-revision relationships. Use
   only the creation source and structured A1 links; never infer a relationship by
   parsing message text or reply chains.
5. Preserve actor claims exactly while labelling the current shared-workspace trust
   boundary. The stored agent/run is useful provenance, not proof that the process
   holding that identity authored the request.

## What is retained today

| Record | Retained evidence | Exact limits |
| --- | --- | --- |
| `work_items` | Current kind, title, description, status, priority, revision, optional source message, created/updated actor and timestamps. | It is one mutable row. `GetWorkItem` and lists return only this current projection plus the latest dispatch. See [`migrate.go:41-61`](../hub/internal/store/migrate.go#L41) and [`work_items.go:17,67-73,138-205`](../hub/internal/store/work_items.go#L17). |
| `work_item_changes` | Global change sequence, item ID, item revision, `created`/`updated`, JSON field values, declared agent ID, caller node/user and occurrence time. | Revision 1 stores every core field and source message; later rows store only supplied new values. There is an index on `(item_id, seq)` but no unique `(item_id, revision)`, no task ID, run ID, request key, payload hash, old value, schema version or public reader. See [`migrate.go:62-73`](../hub/internal/store/migrate.go#L62), [`work_items.go:263-265`](../hub/internal/store/work_items.go#L263), and [`work_items.go:311-365`](../hub/internal/store/work_items.go#L311). |
| `work_item_dispatches` | Every dispatch ID, considered item revision, raw JSON item snapshot, destination task/agent, destination message sequence, declared agent, caller and time. | Public reads expose only the latest dispatch metadata. The stored snapshot has no explicit format version, is not read on replay, and is not returned by an API. A replay returns the original dispatch but attaches it to the current, possibly later item projection. See [`migrate.go:74-88`](../hub/internal/store/migrate.go#L74), [`work_items.go:92-101`](../hub/internal/store/work_items.go#L92), and [`work_items.go:393-408,477-498`](../hub/internal/store/work_items.go#L393). |
| `work_item_requests` | Create and dispatch request key, operation, payload hash, item ID, optional dispatch ID and time. | Update has no request record. Keys are project/operation scoped, not caller scoped. Create replay identifies the same item but returns its current projection; dispatch replay identifies the same dispatch/message but also returns the current item. See [`migrate.go:89-98`](../hub/internal/store/migrate.go#L89), [`work_items.go:217-246`](../hub/internal/store/work_items.go#L217), and [`work_items.go:384-414`](../hub/internal/store/work_items.go#L384). |
| `source_message_seq` | The same-project message explicitly supplied when the item was created. The store verifies that a message exists in the owning project and that a nonempty declared sender belongs to it. | It is not a database foreign key and is not attached to an item revision in public output. Human source messages have an empty declared agent and are accepted. See [`migrate.go:51`](../hub/internal/store/migrate.go#L51) and [`work_items.go:121-135,251-258`](../hub/internal/store/work_items.go#L121). |
| A1 `message_work_item_links` | One creation-time primary work-item link per message, including the exact item project, item ID and revision considered. Optional work-order message is retained. Cross-project dispatch writes the source-owned link on the destination message. | The foreign key proves the item, not the historical revision. New ordinary posts currently require the item to be at that revision. There are no related links, corrections, link history, or historical-revision posting mode. See [`migrate.go:99-116`](../hub/internal/store/migrate.go#L99), [`message_audit.go:90-153`](../hub/internal/store/message_audit.go#L90), and [`work_items.go:462-481`](../hub/internal/store/work_items.go#L462). |
| Messages/events/export | Messages are immutable and message projections include A1 creation context. Work-item create/update events contain item ID/revision and changed field names. | Events are pruned to the newest 10,000 per project and are not the revision authority. Version-1 history export contains task, roster, messages and events only; it omits work items, change rows and dispatch rows, and its streams are read separately. See [`store.go:890-910`](../hub/internal/store/store.go#L890), [`task-history.js:1-33`](../client/task-history.js#L1), and [`task-cleanup.md:29-40`](task-cleanup.md#L29). |

### Current transaction and concurrency behavior

- Create writes the current item, revision-1 change, create receipt and event in one
  transaction. Exact create-key replay is checked before task closure, so it remains
  recoverable after closure; a changed payload conflicts. See
  [`work_items.go:208-277`](../hub/internal/store/work_items.go#L208).
- Update checks the expected revision in memory and again in the SQL `UPDATE ... WHERE
  revision=?`, then writes the patch change and event in the same transaction. One of
  two concurrent updates wins and the other conflicts. It has no request key, so a
  lost successful response followed by the same PATCH conflicts rather than recovers.
  See [`work_items.go:280-372`](../hub/internal/store/work_items.go#L280) and the
  concurrency fixture at [`work_items_test.go:127-187`](../hub/internal/store/work_items_test.go#L127).
- Dispatch writes the destination message, A1 item link, message/resume events,
  dispatch row, dispatch receipt and source event in one transaction. Its stable
  dispatch identity survives replay. See [`work_items.go:375-498`](../hub/internal/store/work_items.go#L375)
  and the rollback fixtures at [`work_items_test.go:296-339`](../hub/internal/store/work_items_test.go#L296).
- The Go and browser clients expose only list/get/create/update/dispatch. The CLI has
  no history command and its update command has no request-key flag. See
  [`work_items_client.go:9-47`](../hub/internal/api/work_items_client.go#L9),
  [`hub-client.js:71-81`](../client/hub-client.js#L71), and
  [`work_items.go:184-240`](../hub/cmd/tt/work_items.go#L184).

## Gaps the implementation must close

1. There is no supported way to list or fetch a historical item revision.
2. The patch chain can normally reconstruct history, but the schema does not enforce
   one change per item revision or store a validated full snapshot for each revision.
3. Update response loss is ambiguous. Retrying the expected revision after a commit
   receives 409, and neither the server nor client can prove that the caller's first
   payload produced the new revision.
4. Existing create/dispatch retry identity is durable, but their `Item` response is a
   floating current projection. Callers cannot use it as an immutable receipt for the
   original content without separately reading a historical snapshot.
5. Source, structured discussion and dispatch links exist in different records and
   have no item-oriented query. Messages without an explicit source/A1 relationship
   cannot be assigned to a revision honestly.
6. Existing task-history export omits work-item revisions and dispatches; activity
   events cannot fill the gap because they are pruned. A transactionally consistent
   complete export is already separate roadmap work and should not be smuggled into
   this bug.
7. Current caller attribution is not per-agent authorization. In the deployed TCP
   mode one bearer token maps every request to fixed `workspace`/`owner`; direct
   tsnet mode can resolve a tailnet node/user. In either case `agentId` is a
   request-declared same-project identity, not a credential bound to that agent. See
   [`token.go:11-24`](../hub/internal/server/token.go#L11),
   [`tailterm-hub/main.go:63-84,105-123`](../hub/cmd/tailterm-hub/main.go#L63), and
   [`work_items.go:104-118`](../hub/internal/store/work_items.go#L104).

## Proposed immutable revision model

Add a new table rather than changing the meaning of `work_item_changes`. The existing
change rows remain the original audit evidence; the new rows are self-contained,
queryable materializations tied back to that evidence.

```sql
CREATE TABLE work_item_revisions (
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK (revision > 0),
  item_seq INTEGER NOT NULL,
  item_kind TEXT NOT NULL,
  title TEXT NOT NULL,
  description TEXT NOT NULL,
  status TEXT NOT NULL,
  priority TEXT NOT NULL,
  source_message_seq INTEGER NOT NULL DEFAULT 0,
  created_agent TEXT NOT NULL DEFAULT '',
  created_node TEXT NOT NULL,
  created_user TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_agent TEXT NOT NULL DEFAULT '',
  updated_run_id TEXT NOT NULL DEFAULT '',
  updated_node TEXT NOT NULL,
  updated_user TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  attribution_kind TEXT NOT NULL CHECK
    (attribution_kind IN ('shared_workspace_claim')),
  change_kind TEXT NOT NULL CHECK
    (change_kind IN ('created','updated','checkpoint')),
  changed_fields TEXT,
  source_change_seq INTEGER UNIQUE,
  provenance TEXT NOT NULL CHECK
    (provenance IN
      ('native','reconstructed_change_log','current_row_checkpoint')),
  PRIMARY KEY (item_task_id,item_id,revision),
  FOREIGN KEY (item_task_id,item_id) REFERENCES work_items(task_id,id),
  FOREIGN KEY (source_change_seq) REFERENCES work_item_changes(seq)
);
CREATE INDEX work_item_revisions_item
  ON work_item_revisions(item_task_id,item_id,revision);

CREATE TABLE work_item_history_gaps (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  first_revision INTEGER NOT NULL CHECK (first_revision > 0),
  last_revision INTEGER NOT NULL CHECK (last_revision >= first_revision),
  reason_code TEXT NOT NULL,
  reason_detail TEXT NOT NULL DEFAULT '',
  detected_at TEXT NOT NULL,
  FOREIGN KEY (item_task_id,item_id) REFERENCES work_items(task_id,id),
  UNIQUE (item_task_id,item_id,first_revision,last_revision,reason_code),
  CHECK (length(reason_detail) <= 512)
);

CREATE TABLE work_item_history_state (
  item_task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  observed_current_revision INTEGER NOT NULL,
  latest_materialized_revision INTEGER NOT NULL,
  complete INTEGER NOT NULL CHECK (complete IN (0,1)),
  checked_at TEXT NOT NULL,
  PRIMARY KEY (item_task_id,item_id),
  FOREIGN KEY (item_task_id,item_id) REFERENCES work_items(task_id,id)
);
```

Notes:

- Use columns for the fields the application queries and validates; do not make a
  versionless JSON blob the only historical authority. `changed_fields` is a bounded
  JSON array of field names for display, while the full snapshot supplies values.
- Preserve both creation attribution and this revision's update attribution in every
  row so a historical read does not depend on the mutable current row. Revision 1 has
  identical created/updated attribution and time.
- `updated_run_id` is empty for existing rows. New agent CLI writes may supply the
  current `TAILTERM_RUN`; the store validates that it matches the declared agent's
  then-current roster row at first execution. The immutable snapshot and receipt
  preserve that original value across a later restart. `attribution_kind` remains
  `shared_workspace_claim`: the check catches stale/mismatched declarations but does
  not turn the shared token into cryptographic per-agent authorship.
- `source_change_seq` is present for native and reconstructed rows. It may be null
  only for `current_row_checkpoint`, which records exactly the mutable state observed
  at upgrade/reconciliation time without claiming the missing patch chain proved how
  that state was reached. Enforce this relationship in application validation (or a
  provenance-aware SQL `CHECK`).
- A checkpoint uses `change_kind=checkpoint` and NULL `changed_fields`; an empty list
  would falsely say that no fields changed. Historical JSON omits the unknown list.
- Gap `reason_code` is a bounded enum such as `missing_revision`,
  `duplicate_revision`, `invalid_change_json`, `final_projection_mismatch`, or
  `unresolved_revision_link`; `reason_detail` contains no work-item/message text.
  Original `work_item_changes`, dispatch snapshots and A1 rows remain untouched.
- Work-item revision changes cover create and content/status/priority edits. Dispatch
  and discussion do not increment the item revision; they point to the immutable
  revision they considered.
- Retain all revisions. The requirement says every edit remains available, so do not
  prune them with the bounded event stream. Page reads and include storage-growth
  measurements in release evidence.

Do not add a destructive edit/delete API. A wrong later value is corrected by another
CAS revision, leaving both snapshots. This bug does not implement the broader A2 link
correction ledger or the separate feature-report/narrative schema.

## Keyed update and response-loss contract

Keep existing `PATCH /v1/tasks/{task}/work-items/{item}` for old clients. It keeps its
current CAS behavior and also writes the new immutable snapshot transactionally, but
it remains explicitly **unkeyed and non-recoverable**.

New clients use a distinct route:

```text
POST /v1/tasks/{task}/work-items/{item}/updates
GET  /v1/tasks/{task}/work-items/{item}/updates/receipts/{requestId}?agentId=...
```

Proposed request and response shapes:

```go
type CreateWorkItemUpdate struct {
    ExpectedRevision int64   `json:"expectedRevision"`
    Title            *string `json:"title,omitempty"`
    Description      *string `json:"description,omitempty"`
    Status           *string `json:"status,omitempty"`
    Priority         *string `json:"priority,omitempty"`
    AgentID          string  `json:"agentId,omitempty"`
    RunID            string  `json:"runId,omitempty"`
    RequestID        string  `json:"requestId"`
}

type WorkItemUpdateReceipt struct {
    ID             string    `json:"id"`
    RequestID      string    `json:"requestId"`
    TaskID         string    `json:"taskId"`
    ItemID         string    `json:"itemId"`
    ResultRevision int64     `json:"resultRevision"`
    CreatedAt      time.Time `json:"createdAt"`
}

type WorkItemUpdateResult struct {
    Revision WorkItemRevision      `json:"revision"`
    Receipt  WorkItemUpdateReceipt `json:"receipt"`
}
```

Both successful POST and receipt GET return the exact same
`WorkItemUpdateResult`: the immutable result revision plus the original receipt.
Receipt GET does not attach the current item projection and never substitutes the
recovering caller's current agent/run metadata for the stored original attribution.

Store receipts in a dedicated table:

```sql
CREATE TABLE work_item_update_requests (
  receipt_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  item_id TEXT NOT NULL,
  agent_id TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  by_node TEXT NOT NULL,
  by_user TEXT NOT NULL,
  request_id TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  result_revision INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (task_id,agent_id,by_node,by_user,request_id),
  FOREIGN KEY (task_id,item_id,result_revision)
    REFERENCES work_item_revisions(item_task_id,item_id,revision)
);
```

Semantics:

- Validate the static task/item/key/field shape, then check the actor-scoped receipt
  and operation-tagged canonical payload hash **before** task closure, current
  revision, item state or agent lifecycle checks.
- Exact key and exact original payload return the original immutable result revision and
  receipt with 200, without another row, event or notification. Same key with a
  changed item, expected revision, actor/run or field intent returns 409.
- A new request validates the open task, same-project item and declared actor/run,
  then performs SQL revision CAS. Stale expected revision returns 409. Missing item
  or receipt returns 404; malformed fields/key return 400.
- First execution returns 201. Exact POST replay and receipt GET both return 200 with
  the same body shape and values as that original result; their status is the only
  difference.
- Commit current-row CAS, old `work_item_changes` patch, new full snapshot, update
  receipt and work-item event in one transaction. Injected failure at any boundary
  must leave all five unchanged.
- The receipt GET is the recovery path after the originating agent has retired or
  restarted: it returns the stored original run/actor attribution and does not
  relabel the operation as the caller's new run. Re-POSTing under a new run is a
  changed payload and conflicts.
- The browser generates one key per serialized intent and retains it with the draft
  across an ambiguous failure, refresh and navigation. Editing the payload generates
  a new key. The handler CLI requires `--request-id` for the new route and supplies
  its current run ID. Neither client blindly retries with a new key.
- An old server returns 404 for `/updates`; the new client must retain the draft and
  report that the hub needs the matching capability. It must never fall back to
  legacy PATCH, because doing so would silently discard the recovery guarantee.
- Old clients continue PATCHing a new server. Their committed revisions are complete
  historical snapshots but correctly show no operation receipt/run ID.

Existing create and dispatch request semantics remain compatible. Add an immutable
`createdRevision` to create responses and `revisionSnapshot` to dispatch responses,
or require clients to fetch the referenced revision when they need stable content.
Do not redefine their existing `item` field as historical: current replay code has
always allowed that projection to move. The original item/dispatch/message identities
remain unchanged.

## Historical read and conversation contracts

Proposed additive types:

```go
type WorkItemRevision struct {
    ItemID, TaskID, Kind, Title, Description, Status, Priority string
    ItemSeq, Revision, SourceMessageSeq                         int64
    CreatedBy, UpdatedBy                                       Sender
    CreatedAt, UpdatedAt                                       time.Time
    UpdatedRunID, AttributionKind, ChangeKind, Provenance       string
    ChangedFields                                              []string `json:"changedFields,omitempty"`
}

type WorkItemRevisionList struct {
    Revisions []WorkItemRevision `json:"revisions"`
    NextAfter int64              `json:"nextAfter,omitempty"`
    Coverage  HistoryCoverage    `json:"coverage"`
}

type HistoryCoverage struct {
    Complete                bool        `json:"complete"`
    ObservedCurrentRevision int64       `json:"observedCurrentRevision"`
    LatestMaterialized      int64       `json:"latestMaterializedRevision"`
    SnapshotCount           int64       `json:"snapshotCount"`
    GapCount                int64       `json:"gapCount"`
    FirstGap                *HistoryGap `json:"firstGap,omitempty"`
    ConversationLinks       string      `json:"conversationLinks"`
}

type HistoryGap struct {
    Seq           int64     `json:"seq"`
    FirstRevision int64     `json:"firstRevision"`
    LastRevision  int64     `json:"lastRevision"`
    ReasonCode    string    `json:"reasonCode"`
    Detail        string    `json:"detail,omitempty"`
    DetectedAt    time.Time `json:"detectedAt"`
}

type HistoryGapList struct {
    Gaps      []HistoryGap `json:"gaps"`
    NextAfter int64        `json:"nextAfter,omitempty"`
}

type WorkItemMessageLink struct {
    ItemRevision     int64   `json:"itemRevision"`
    RevisionCoverage string  `json:"revisionCoverage"`
    Source           bool    `json:"source,omitempty"`
    Relationship     string  `json:"relationship,omitempty"`
    Message          Message `json:"message"`
}

type WorkItemMessageList struct {
    Links     []WorkItemMessageLink `json:"links"`
    NextAfter int64                 `json:"nextAfter,omitempty"`
    Coverage  HistoryCoverage       `json:"coverage"`
}
```

Routes:

```text
GET /v1/tasks/{task}/work-items/{item}/revisions?after=0&limit=32
GET /v1/tasks/{task}/work-items/{item}/revisions/{revision}
GET /v1/tasks/{task}/work-items/{item}/history-gaps?after=0&limit=32
GET /v1/tasks/{task}/work-items/{item}/messages?revision=&after=0&limit=32
```

- Return revisions by revision and gaps by durable gap sequence, both ascending;
  `nextAfter` is the last returned revision or gap sequence only when another
  eligible row exists. Message `after` remains the
  destination message sequence. Closed projects remain readable. Cursor types are
  strict and never reused across revision, gap, message or event streams.
- Use a default count of 32, maximum requested count of 64, and a fixed encoded-body
  budget of 3 MiB for every revision, gap and linked-message list. The server stops
  before either bound, sets `nextAfter` when a count or byte cut leaves another row,
  and guarantees forward progress. It measures the exact JSON candidate including
  `coverage` and the encoder's final newline (the current writer uses `Encode` at
  [`server.go:94-97`](../hub/internal/server/server.go#L94)); one valid record must
  fit or the server returns a bounded integrity error rather than an empty cursor
  loop.
- Exact revision GET returns 200 for a verified or checkpoint snapshot, whose
  provenance is explicit. An item that does not exist, or a revision outside its
  observed range, returns 404. A revision inside a recorded gap with no snapshot
  returns 409 with a bounded `HistoryGap` payload, so callers do not confuse known
  missing evidence with a nonexistent item/revision.
- The 3 MiB body limit deliberately stays under the existing ordinary Go client's
  4 MiB reader at [`client.go:39-40,67`](../hub/internal/api/client.go#L39).
  Count-only paging is insufficient: descriptions/messages accept 8 KiB at
  [`types.go:15-21`](../hub/internal/api/types.go#L15), and Go JSON escaping can
  expand characters such as `<`, `>` and `&`. The prior decision reader needed a
  special 16 MiB path for exactly this class of escaped-page overflow; history must
  instead remain compatible with the normal reader. See
  [`decisions.go:13-16,145-148`](../hub/internal/api/decisions.go#L13) and
  [`decisions_test.go:13-54`](../hub/internal/api/decisions_test.go#L13).
- The item-oriented message query unions only two evidence-backed relations:
  `source_message_seq` as `source=true,itemRevision=1`, and A1 rows matching
  `(item_task_id,item_id)` with their stored revision/relationship. De-duplicate a
  source message that also has an A1 row without losing either relation flag.
- Returned messages keep their actual destination `taskId`; this matters for
  cross-project dispatch. Existing workspace visibility is unchanged. Do not rewrite
  the message or make the item's current revision replace its stored revision.
  `revisionCoverage` is `verified`, `checkpoint`, or `gap`. If an exact source/A1
  link lands in a recorded history gap, return its unchanged revision and honest
  coverage; do not discard or retarget the link.
- Do not infer links from `wi_` strings, prose, recipient, `replyTo`, work-order text
  or temporal proximity. `coverage` must say that legacy unlinked discussion is
  unknown. The absence of an explicit link means “not recorded,” not “unrelated.”
- New discussion about the current revision keeps using A1. This slice does not add
  a historical-comment write: attaching a new message to an older revision would
  need a dedicated operation that verifies the snapshot and reuses the A1
  message/link/receipt transaction. Do not relax ordinary posting, because work
  orders and scope changes still require current revision. Correcting an already
  stored link remains A2 scope.
- Add `tt work-items revisions`, `tt work-items get --revision N`, and
  `tt work-items messages [--revision N]`. Human UI may use the same reads for a
  compact timeline. The existing current get/list shapes remain unchanged.

`HistoryCoverage.complete` is true only when every revision from 1 through
`observedCurrentRevision` has a verified snapshot and there are no gap rows.
`conversationLinks` is always `explicit_only` because unlinked legacy prose is not
classified. Counts of explicit A1 messages, creation sources and dispatches may be
added as fixed-size scalars; never embed an unbounded gap or message collection in
the coverage object. The separately paged gap route is the complete anomaly view.

## Migration and backfill

Run the following against an isolated copy before deployment, then execute the
additive DDL and per-item classification in one SQLite transaction after a consistent
production backup:

1. Treat unreadable core tables, failed DDL/transaction commits, SQLite integrity or
   foreign-key failures, and violated current-table structural invariants as fatal
   database failures. Abort startup for those because safe ordinary operation is not
   established. A bad historical change payload, duplicate/gapped item revision,
   current-row comparison mismatch, or unresolved A1/dispatch revision is instead a
   bounded **item-history anomaly**; it must not disable other items, messages or hub
   operations.
2. For each item, load changes ordered by revision and global sequence. Parse revision
   1 as the complete allowed core-field set, then apply consecutive later JSON maps as
   patches, rejecting unknown fields/types and preserving original strings. Insert
   every provable snapshot with the original actor/time and
   `provenance=reconstructed_change_log`; never use migration time as edit time.
3. If replay reaches and exactly matches the mutable current row, mark that item's
   coverage complete. Validate A1/dispatch references and dispatch snapshots without
   rewriting them. A reference mismatch records a bounded per-item anomaly while the
   original source, dispatch and A1 evidence remains exact.
4. If replay stops or diverges, retain its longest verified snapshot prefix. Record
   the unverified revision interval and reason in `work_item_history_gaps`, then store
   the mutable row exactly once at its actual current revision with
   `provenance=current_row_checkpoint` and no source-change claim. A checkpoint says
   “this was current state observed at upgrade,” not that earlier missing revisions
   contained this text. Never copy the checkpoint backward or manufacture an actor,
   timestamp, changed-field list or source link for a gap.
5. Commit complete and partial items together, with one `work_item_history_state` row
   per item. History reads continue for verified snapshots and checkpoints, returning
   `complete=false` plus paged exact gaps. Ordinary current-row create/edit/dispatch,
   messages and unrelated hub work stay available. Every new-server edit after the
   checkpoint writes a complete native snapshot, so prospective history is exact
   even while an older interval remains explicitly unknown.
6. On every new-binary startup, reconcile changes newer than the latest trusted
   snapshot/checkpoint. Consecutive old-binary changes are replayed into reconstructed
   snapshots. A new malformed/gapped suffix creates another gap and current-row
   checkpoint rather than aborting the hub. Do not overwrite an existing immutable
   snapshot or erase/merge gaps merely because a later checkpoint exists.

The new tables are additive and remain on application rollback. The old binary
ignores them and keeps current reads/writes working. New history reads and keyed
updates are capability-gated by their distinct routes; after rollback they fail
without mutating data. Do not add triggers that prevent the old binary from writing
its existing change records, because that would make rollback unsafe.

Representative compatibility fixture:

1. Old binary creates healthy item A and item B, edits each twice, dispatches each and
   posts exact A1 links. In the isolated copy only, remove or corrupt one B change row
   while leaving its mutable current row and original link/dispatch rows intact.
2. New binary upgrades without refusing service: A reconstructs completely; B exposes
   its verified prefix, bounded gap and current checkpoint. Both items' current reads
   and ordinary messages work. A new keyed B edit becomes an exact native snapshot
   after the checkpoint, and receipt GET returns byte-equivalent result/receipt data
   after simulated response loss.
3. Old binary reopens the migrated database and reads/updates/dispatches A and B
   normally. A new client pointed at it attempts `/updates`, receives 404, retains its
   draft/key and performs no legacy PATCH fallback.
4. New binary reopens again, reconstructs the consecutive old-binary suffix after the
   last trusted snapshot/checkpoint, preserves B's older gap and prior keyed receipt,
   and leaves A complete. A separately corrupted new suffix creates another B gap and
   checkpoint without taking A or ordinary hub APIs offline.
5. Every phase passes `integrity_check` plus `foreign_key_check`; current projections,
   original change rows, exact source/dispatch/A1 coordinates and receipt identities
   remain unchanged except for the deliberate normal edits.

Rollback restore of the pre-migration backup is disaster recovery only because it
would discard subsequent writes. Normal binary rollback retains additive tables.

## Acceptance matrix for an implementation order

- Exact snapshots: create plus multi-field, same-value, empty-description, Unicode
  and every status/priority edit produce consecutive immutable rows and accurate
  actor/time/changed-field metadata.
- Historical reads: page beyond 100 revisions; exact revision reads; closed-project
  reads; current list/get responses unchanged; no history-event cursor reuse. Pages
  of maximum-length descriptions/messages containing worst-case HTML-escaped bytes
  stop at or below 3 MiB, decode through the ordinary 4 MiB client, advance cursors,
  and yield every record exactly once across count- and byte-limited boundaries.
- CAS/retry: concurrent different keyed updates yield one winner; concurrent exact
  duplicates return one receipt; changed key intent conflicts; exact POST replay
  succeeds after another edit/project closure, and receipt GET recovers after agent
  retirement/restart, all with zero extra effects.
- Failure injection: fail current row, change row, snapshot, receipt and event inserts
  independently and assert no partial revision/receipt/event/notification.
- Conversation: creation source maps to revision 1; current and older explicit A1
  links return their original revision; cross-project dispatch returns its real
  destination message task; ordinary unlinked replies stay unclassified.
- Migration: normal chain reconstruction; malformed/gapped/duplicate history becomes
  exact per-item gaps/checkpoints without hub-wide failure; fatal structural failures
  still abort; old/new/old/new suffix reconciliation preserves gaps and prospective
  snapshots; dispatch/A1 coordinates, encrypted profiles/agents/cleanup/decisions are
  unchanged; SQLite integrity/FKs stay clean.
- Clients: CLI body files remain shell-safe; new update requires a retained key; old
  CLI/browser PATCH still works; new-to-old endpoint mismatch never falls back; lost
  response recovery is tested through an actual socket close.
- Browser: Bugs and Features share the same service contract; create/edit drafts and
  request keys survive failure/refresh; history labels backfill provenance and
  explicit-only conversation coverage without claiming a full private transcript.
- Export: existing version-1 task export remains byte/shape compatible. A future
  versioned, materialized audit export must be a separate accepted order and include
  work-item streams from one consistent snapshot.

## Accepted scope choices and exclusions

1. **Full snapshot table, not patch-only reads.** It costs roughly one
   description copy per edit but makes exact revision reads, receipt replay and A1
   validation simple and bounded. Patch rows remain the original evidence.
2. **Distinct keyed `/updates` route.** Adding `requestId` only to PATCH
   is unsafe during server rollback because the old decoder accepts and ignores
   unknown JSON fields. Preserve PATCH as the explicitly legacy operation.
3. **Truthful explicit-only conversation history.** Expose creation
   source and A1 links now. Do not parse prose or absorb A2 link corrections, typed
   orders, final reports or AIV into this bug.
4. **Retain all revisions without pruning.** If storage growth later
   becomes material, archive immutable snapshots as a separately designed operation;
   never silently drop the history this feature promises.
5. No new historical-comment write route in this slice. Reads truthfully expose all
   existing source/A1 links and their coverage; attaching new messages to an older
   revision remains separately ordered work.
6. No broader dispatch-history viewer in this slice. Preserve and validate every
   dispatch coordinate/snapshot, while the UI is limited to revisions and explicitly
   linked messages; broader feature-history remains `wi_0535c67103994980` scope.

## Verification performed for this plan

The product tree was read only. No live database, work item, task, profile, service or
deployment target was accessed or changed.

```text
git branch --show-current
  tasks-hub
git rev-parse HEAD
  54b25f1d8080ac5e85370497b5dedc7a2a44063c

cd hub
go test -count=1 ./internal/store -run
  TestWorkItem(CRUDScopesRevisionsAndRetryReceipts|ConcurrentRetriesAndRevisionCAS|
  SourceMessageMustBelongToProjectMember|DispatchIsAtomicIdempotentAndResumesHumanTarget|
  WritesRollbackWithoutPartialReceipts|MigrationPreservesExistingAgentData)$
  PASS (0.384s)
go test -count=1 ./internal/server -run '^TestWorkItemHTTP'
  PASS (0.564s)
go test -count=1 ./cmd/tt -run '^TestWorkItemsCLI'
  PASS (0.815s)
```

An isolated Go sizing check at
[`history-size-budget.go`](../.build/work-item-history/history-size-budget.go) modeled 64 maximum 8 KiB
descriptions and 64 valid decision-shaped messages containing worst-case HTML-escaped
text using `encoding/json` and the final encoder newline:

```text
go run .build/work-item-history/history-size-budget.go
  budget=3145728 client=4194304
  revision64=3215589 revisionFit=62 revisionPage=3115122
  message64=5578776 messageFit=36 messagePage=3138143
```

This confirms count 64 alone can cross 3 MiB for both projections, while the proposed
exact-byte cutoff returns a nonempty decodable page below 4 MiB.

Those fixtures confirm the documented baseline only. They do not prove the proposed
migration/API because this order explicitly forbids product/schema implementation.
