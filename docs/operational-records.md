# Structured operational records

Feature `wi_618c8ff87e6b8061`, bounded work order **#3915**, implements explicit
normalized operational records within consumer #3694. Owner requirement:
source prose must become structured data before it can drive work. The retained
input is `nart_4f9d8a22f2f06552` v1; that artifact itself was proposed evidence,
not an operational schema or an assignment receipt.

## Authority and storage

`operationalRecords` capability version 1 exposes `propose`, `commit`, and `get`.
A proposal may omit item, assignment, evidence or content fields. Its state is
`proposed`, its source evidence is unverified, and it creates neither an
assignment nor an acknowledgment. Proposals can be revised using an expected
version. Every revision is retained; a committed record cannot be rewritten.

A separate commit transaction requires all typed fields and checks the current
item revision/scope, exact active worker/run and admitted binding, current
directive generation/epoch, source hashes and immutable item associations,
reference versions, permitted actor, and allowed execution phase. It derives
source authors and original text from saved messages. It never infers fields
from prose, adjacent message numbers, roster status, or prior summaries.

The two new tables hold record projections and immutable record versions.
They reuse **delivery_operation_receipts** for proposal and commit retries.
Only committed directive-bound records append to **delivery_events**, with the
existing per-directive event sequence. There is no second event/receipt ledger.
Same-key identical retries recover the original record/version/event/receipt,
even after restart or closure. Changed requests conflict. Rejected commits do
not change the proposal, execution state, events, or receipts.

## Typed path

1. **Instruction**: coordinator/handler/human commits an objective, bounded
   scope and uniquely identified acceptance criteria against an unacknowledged
   current directive. This records the normalized instruction and does not ack.
2. The worker explicitly acknowledges through the existing delivery protocol.
3. **Finding**: exact worker records an observation and reproduction steps,
   referencing an exact committed instruction version.
4. **Candidate**: exact worker references the instruction and applicable
   findings, a 40-character commit, and an immutable candidate-manifest artifact
   by ID/version/SHA-256. The manifest must contain the same commit and explicit
   file paths/digests. An unrelated prose artifact cannot substitute for it.
5. **Verification**: an active exact item-bound test actor submits a criterion,
   candidate version, test ID, command argv, pass/fail outcome and evidence pin.
   The saved evidence must decode as `OperationalTestEvidence` v1 and match all
   those fields, candidate manifest digest, and actor/run. Changing a failed
   outcome to pass without matching retained evidence conflicts.
6. **Result**: the exact worker references one candidate and exactly one
   verification per instruction criterion. This submits a result and updates
   the existing delivery result phase in the same transaction. Failed results
   can be submitted; submission is not acceptance.
7. **Acceptance**: a separate current coordinator/handler/human operation records
   accepted/rejected plus its reason against the exact result version. Accepted
   requires every referenced verification to pass. Workers cannot self-accept.
   Acceptance does not complete the work item, mutate Queue or close workers.

At most one instruction and one result can be committed per directive epoch;
one acceptance decision can be committed per result. A changed instruction or
candidate after a terminal result uses a new current directive, preserving the
old evidence. References are pinned to the same item revision, scope, directive
generation and epoch. A superseded or resumed epoch cannot commit new records
using an older binding. Old `get` results remain historical committed evidence;
`state=committed` is not a promise of current actionability. Every new commit
rechecks current state transactionally.

## API and CLI

- `POST /v1/tasks/{task}/operational-records`: create/revise proposed data.
- `POST /v1/tasks/{task}/operational-records/{record}/commit`: validate and commit.
- `GET /v1/tasks/{task}/operational-records/{record}`: latest immutable version.
  `?version=N` reads an explicit retained version.

Requests use the types in `hub/internal/api/operational_records.go`. HTTP and
CLI reject unknown fields. The CLI returns JSON records and real receipt IDs;
it does not translate a successful request into an unqualified prose claim.

```text
tt operational-record propose --file proposal.json
tt operational-record get RECORD_ID
tt operational-record get --version 1 RECORD_ID
tt operational-record commit --file commit.json RECORD_ID
```

The proposal includes `requestId`, optional `recordId`, `expectedVersion`, and
`data`. Commit includes `requestId` and `expectedVersion`. The CLI supplies the
actor from its exact current environment and rejects a conflicting explicit
actor. Both mutation commands accept `--file -`. New commands first require
`operationalRecords` v1; absent/unknown support fails closed without a prose or
legacy-command fallback.

## Limits

Validation proves retained bytes, source associations, declared evidence and
allowed transitions under the existing shared-workspace identity model. It does
not prove that an external test actually ran, authenticate a Git repository, or
provide cryptographic per-agent identity. Test-runner/tool attestation is not
implemented. Explicit normalized submissions are sufficient; no generic prose
interpreter is added.

Legacy manual directive operations retain their existing contract. Their plain
result text cannot serve as a typed verification or acceptance record. A proposed
operational record alone does not enable or disable a legacy assignment. This
capability makes only the new structured record path authoritative for its own
associations; it does not claim that every existing workflow has migrated.

Scheduling, complete backlog sweeps, GO/wake transport, MCP adapters, UI,
AIV integration, production deployment and task cleanup are outside #3915.
Schema addition retains existing data. Never restore an old database to roll
back; older binaries do not enforce this capability and cannot be presented as
supporting committed operational records.
