# Mandatory-action design review

This review is the UI/operator member's documentation deliverable for Bug
`wi_0c7ab8fc3320b52b` revision 2 and bounded order **#4402**, after exact Start
**#4483**. It reviews the contract against source
`b1b14d70cb6f96d033e4da9e1aeb650174066cbf`, owner orders **#4328** and
**#4335**, and owner gap disposition **#4393**. It does not own shared schema,
backend integration, QA execution, deployment, or a cosmetic frontend change.

The normative operator behavior is in the
[mandatory-action operator contract](mandatory-action-operator-contract.md).

## Finding

The current manual directive and follow-through foundation already provides the
right primitive shape: immutable item/run/order binding, explicit v2 enrollment,
generation and execution-epoch fences, substantive progress timestamps, bounded
queue leases, durable transport outcomes, stable retry receipts, and one durable
escalation. Structured operational records already keep instruction, finding,
candidate, verification, result, and acceptance separate.

The Bug remains real because that foundation protects an explicitly enrolled
item-worker delivery. The observed #4314/#4316 failure included mandatory actions
owned by the coordinator and database handler. Saving the containing order and
ending the turn left those actions with no durable per-action owner/state. Loading
the worker relay or interpreting Board prose cannot supply the missing enrollment.
The current foundation also does not by itself enforce the owner #4335
causal-before-resume gate or prove independent ready work was dispatched.

## Reuse, do not fork

The implementation should reuse these established seams:

- the existing required-delivery identity, generation, epoch, event, and receipt
  ledger for exact executor delivery;
- current assignment and delivery coverage for exact-run fencing;
- follow-through check/report for bounded external continuation and transport
  reconciliation;
- typed operational records for source, findings, candidate, verification,
  result, and acceptance; and
- existing lifecycle, Queue, and Board mechanisms only for their documented
  meanings.

Do not create a second watcher, a parallel receipt/event ledger, or a prose parser.
Do not treat Queue acceptance, a heartbeat, inbox consumption, or a lifecycle
event as action progress. Existing item-worker behavior must remain compatible.

## Required implementation boundary

The API builder owns the wire model and integration. Whatever names it chooses,
the authoritative model needs to express:

1. one stable mandatory-action identity beneath a bounded order;
2. the exact source instruction and current item revision;
3. a responsible role and, when selected, one exact agent/run;
4. current action state, last substantive timestamp, and next expected action;
5. a planned dependency reason and resume condition, when applicable;
6. an unexpected-stop incident link and causal/prevention gate state;
7. one accountable prevention action, bounded order, and verification criterion;
8. exact continuation lease/outcome and retry receipt identity; and
9. terminal result, verification, acceptance, supersession, or escalation links.

The server, not a UI or LLM, must validate current item/revision/order/action/run/
generation/epoch before state-changing operations. Source message text may be
retained, but action creation and transitions must use typed fields and explicit
associations.

## Read model for operator consumers

A later UI/API adapter should be able to build one coherent snapshot containing:

```text
scope
  project + item@revision + original/current order
action
  action id + kind + source + required role + selected agent/run
execution
  state + generation/epoch + last substantive timestamp + next deadline
dependency
  planned reason + resume condition + resolution, if any
recovery
  incident + causal completeness + prevention assignment + recurrence link
transport
  lease + attempt + outcome + confirmation deadline + receipt
evidence
  admitted/team-started/action-stored/bound/built/deployed/loaded/enrolled/
  wake/ack/progress/result/verification/acceptance/item-status facts
history
  immutable transitions, supersession, and retry receipts
```

This is a view contract, not a JSON schema. Missing fields must remain explicit
as missing/unknown. Consumers must not join unrelated current roster or adjacent
Board state and call it proof.

For a mutating control, the client refreshes the coherent current snapshot,
submits the exact expected revision/action/run/generation/epoch with a stable
request ID, and trusts the mutation response or recovered receipt. A previous GET
can become stale; the mutation's transaction is the final authority.

## Transition review

### Enrollment and initial execution

Action persistence must occur independently of the conversation turn. A saved
order that declares three required actions creates or is followed by three
explicit typed action records; it does not become one vague assignment state.
Each action receives its own executor, deadline, and outcome. If the executor is
not yet selected, that is an actionable unassigned state, not coverage.

Only an exact acknowledgment, progress, block, or result changes substantive
execution state. Existing v2 defaults may remain policy defaults rather than an
owner-facing SLA. The persisted policy must be readable so tests and operators
can explain why an action is or is not due.

### Planned waits and independent work

A planned wait is data: reason, dependency, resume condition, owner, and optional
deadline. While valid, it is not an unexplained stall and must not generate a
false incident. The same containing order may have other ready actions; a blocked
action must not suppress their dispatch or progress.

Once the dependency resolves, resolution alone does not prove the executor
resumed. The exact pending action becomes Ready to resume and uses a stable,
epoch-fenced resume/continuation operation.

### Unexpected stops and recurrences

An overdue action with no valid planned wait becomes an unexpected-stop candidate.
The supported runtime observation remains `unknown` when the installed runtime
cannot prove idle prompt or active tool. The system may lease a bounded
continuation to establish execution, but an actual recovery resume after a known
unexpected pause must satisfy the owner #4335 incident and prevention gate. A
generic wake cannot erase or satisfy that gate.

The incident keeps observations separate from causal conclusions. A recurrence
must reference the prior incident and identify why its control failed. Immediate
recovery may restore progress; permanent correction remains open until its own
criterion is verified and handler acceptance is saved.

### Transport and response loss

Database lease and external runtime call cannot be atomic. Preserve the current
safe pattern: a check transaction owns at most one lease; replay returns
`execute=false`; the final pre-call read invalidates changed work; an accepted
call stays unconfirmed until action acknowledgment/progress; missing or ambiguous
reports are escalated instead of blindly repeating the external call; a lost
report response retries only the durable report with the same identity.

### Results and acceptance

An executor result is evidence, not acceptance. Independent verification, handler
acceptance, item status, Queue completion, and worker lifecycle remain separate.
The parent Feature remains open for deferred scope. A result must not silently
clear sibling mandatory actions.

## Error review

API errors should be typed or at least stably classifiable by clients. They need
to distinguish:

- invalid input from stale current-state conflicts;
- absent/legacy enrollment from an enrolled action waiting for evidence;
- missing executor from retired/terminal executor;
- planned dependency from unexpected overdue execution;
- missing causal record from missing prevention assignment;
- failed transport from ambiguous transport and accepted-but-unconfirmed
  transport;
- invalidated lease from a call that may already have happened; and
- result submission from verification, acceptance, and item completion.

Messages should name the exact stale or missing dimension. A generic conflict is
insufficient for operator recovery when the server can safely report that the
item revision, executor run, generation, epoch, lifecycle, dependency, or
recovery gate changed.

No error path should mutate filesystem, database, runtime, Queue, or lifecycle
state before its relevant identity and authority checks. External side effects
that cannot be transactionally fenced must record ambiguity and require
reconciliation.

## QA handoff

Independent QA should treat the acceptance matrix in the operator contract as
the behavioral source. In addition to current follow-through regressions, the
candidate needs negative/race/retry coverage for:

- multiple sibling actions with lead and handler owners;
- save-only/turn-end with no substantive event;
- role selected but no actionable exact run;
- read, heartbeat, Working, Queue Start, and queue acceptance as non-progress;
- block resolution racing a stale resume;
- incident incomplete, prevention owner/order/criterion missing, and unknown
  cause preserved;
- recurrence missing its prior-control failure explanation;
- duplicate continuation, response loss, stale report, and ambiguous call;
- independent ready action continuing while a sibling waits;
- stale item revision, action generation/epoch, run replacement, retirement,
  exit, close, supersession, and result races; and
- result without verification/acceptance/item completion.

Use isolated databases, clocks, runtime adapters, browser contexts, and tmux
sockets. Do not use live tasks, profiles, services, or owner data. Tests should
assert both the durable record and absence of forbidden side effects.

## Acceptance and release evidence

The candidate can satisfy this Bug only when evidence identifies the exact
source and separately reports:

- exact agent/run/item/order/context admission;
- handler-saved team Start with Queue receipt/event;
- implementation built;
- exact candidate independently tested;
- hub/CLI/relay components deployed or explicitly not deployed;
- relay loaded the expected code or explicitly not loaded;
- real mandatory actions enrolled or explicitly not enrolled;
- continuation/wake accepted or failed;
- substantive execution observed or still unknown;
- causal-before-resume and recurrence tests passed;
- independent actions remained dispatchable;
- authorized acceptance saved by the handler; and
- parent Feature status preserved.

Synthetic checks, installed binaries, queue acceptance, or this design review do
not by themselves prove the incident prevented. Release and deployment remain
outside the UI member's order #4402 scope.

## Review disposition

The design is acceptable if it extends the existing directive and operational
record foundations with typed per-action ownership and the recovery gate, while
retaining their identity, receipt, lifecycle, and acceptance separations. It is
not acceptable if it relies on another watcher, Board prose inference, generic
keep-going messages, a UI prerequisite, or a single enclosing assignment flag
that lets one completed action hide another unfinished action.

This disposition is a contract review for the API and QA peers. It is not code
acceptance and cannot close the Bug.
