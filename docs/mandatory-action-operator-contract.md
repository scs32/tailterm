# Mandatory-action operator contract

Bug `wi_0c7ab8fc3320b52b` revision 2, bounded implementation order **#4402**
(original owner order **#4328**, recovery amendment **#4335**, and gap
disposition **#4393**) owns this contract. The admitted UI member began after
handler-saved Start **#4483**. This document defines what an operator or future
UI consumer must be able to distinguish. It does not add a frontend gate,
implement backend state, deploy anything, or claim prevention is accepted.

The contract extends the distinctions already made by the
[reliable directive core](reliable-directive-core.md) and
[structured operational records](operational-records.md). Wire names remain the
API builder's responsibility. A UI adapter may map wire values to the operator
states below, but it must not infer missing evidence or collapse distinct states.

## Governing invariant

A mandatory action stays outstanding until one of these durable outcomes exists:

- exact substantive execution evidence;
- an explicit current block with a bounded reason and resume condition;
- a bounded transport or execution failure escalated to an actionable owner; or
- a terminal supersession/cancellation whose authority and replacement are
  retained.

Saving an order, posting or reading a message, Queue selection or Start,
heartbeat, online/Working status, a wake attempt, native queue acceptance, and a
runtime turn ending are evidence about delivery or liveness. None is substantive
execution, acceptance, or permission to clear the action.

## Required identity and provenance

Every operator-visible mandatory action must retain, or explicitly report as
missing, all of the following:

| Field | Operator meaning |
| --- | --- |
| Project, item, and item revision | Durable scope that authorizes the action. |
| Original and current bounded work order | Source authority and any later governing order without rewriting history. |
| Action ID and action kind | Stable identity for the individual required action, not merely its containing assignment. |
| Responsible role | `lead`, `database_handler`, or another explicitly named role; never inferred from message arrival order. |
| Responsible agent and run, when selected | Exact current executor. A role without a selected actionable run is visibly unassigned. |
| Context digest and directive generation/epoch, when applicable | Fence against stale work and stale resume attempts. |
| Source message or typed instruction version | Immutable body from which the action was created; a nearby message number is insufficient. |
| Current state and transition timestamp | Latest durable projection plus the event that produced it. |
| Retry identity and receipt for each mutation | Response-loss recovery without duplicating an action. |
| Dependency or incident references | Exact records controlling block, resume, recurrence, or prevention. |

A complete assignment can contain several mandatory actions with different
owners and dependencies. The enclosing assignment is not complete while any
current required action lacks a terminal authorized outcome.

## Operator states

The labels here are normative presentation states, not proposed wire enum names.
The operator should see the most specific applicable state and its reason.

| Operator state | Required durable evidence | Permitted next action |
| --- | --- | --- |
| Not enrolled | The assignment exists, but no explicit current mandatory-action record covers this item/order/action/role. | Enroll through the authoritative producer path; do not infer coverage from Board prose. |
| Recorded, not started | The action and executor are bound, but there is no exact execution acknowledgment or substantive progress. | Await the configured initial deadline, then lease bounded continuation or escalate. |
| Awaiting acknowledgment | A supported delivery/continuation attempt is pending within its confirmation window. | Wait for exact acknowledgment; do not call transport acceptance execution. |
| Executing | Exact current action progress was saved for the same item/revision/action/run/generation/epoch. | Continue until another substantive checkpoint, block, or result. |
| Planned wait | A known dependency, reason, resume condition, and optional deadline were recorded before waiting. | Keep unrelated authorized actions moving; reevaluate when the condition is met or overdue. |
| Blocked | A current durable block names its class, evidence, owner, and bounded resume condition. | Resolve the dependency or escalate the specific block; no generic keep-going prompt. |
| Recovery hold | Work paused unexpectedly, or a planned wait exceeded its recorded condition, and causal/prevention prerequisites are incomplete. | Complete the incident and prevention gate before resuming the affected action. |
| Ready to resume | The block is resolved and the recovery gate is complete, but exact resume for the current action/epoch has not committed. | Resume once with the stable retry identity; a resolution alone is not a resume. |
| Continuation leased | One exact current action owns the bounded external continuation attempt. | Execute only when the lease says to; replay must not repeat the external call. |
| Transport unconfirmed | A continuation was leased or accepted, but its durable outcome or exact action acknowledgment is absent. | Await the configured report/confirmation deadline; never reset the substantive deadline from transport evidence alone. |
| Escalated | A bounded failure or missing execution produced one durable escalation linked to the action and incident. | Diagnose and assign a concrete correction; escalation itself is not completion. |
| Result reported | The exact executor saved a result for the current action. | Obtain independent verification and authorized acceptance as required. |
| Accepted | Authorized acceptance references the exact result and verification evidence. | The handler may separately update item state when all item criteria are satisfied. |
| Superseded or terminal | Exact authority ended or replaced the action, retaining its prior events and replacement link. | Follow only the new current action; never auto-resume the retained old run. |

Roster `retired`, `exited`, and `closed` are lifecycle facts, not action results.
Retirement is an intentional pause and must never be auto-resumed. Exit or close
does not erase an unfinished action; it requires an explicit replacement,
supersession, or escalation decision.

## Evidence dimensions stay separate

Status summaries, release evidence, and operator views must report these as
independent facts. Unknown or not-applicable values are valid and must not be
upgraded from circumstantial evidence.

1. Exact agent/run admitted with the item/order/context binding.
2. Team execution Start saved by the handler with its Queue receipt and event.
3. Mandatory order/action stored.
4. Exact action executor bound.
5. Delivery or relay code built.
6. Exact code deployed/installed.
7. Relay process loaded that code.
8. Mandatory action enrolled and covered.
9. Wake or continuation leased.
10. Transport invocation attempted.
11. Transport accepted, failed, ambiguous, or invalidated.
12. Exact action acknowledged.
13. Substantive execution observed.
14. Result reported.
15. Independent verification recorded.
16. Authorized acceptance saved.
17. Work-item status transition confirmed by the database handler.

Admission and handler-saved team Start authorize the worker to begin its bounded
scope. Neither enrolls a particular mandatory action nor proves that action was
executed.

For example, `wake accepted` with `substantive execution unknown` is a valid and
important state. So is `result reported` with `acceptance pending`. The UI must
not replace either with a generic green "Done" state.

## Causal-before-resume gate

A planned wait needs a reason and resume condition. It does not require an
incident while still inside that condition. When execution stops unexpectedly,
or a planned wait exceeds its condition, the affected action enters Recovery
hold. Before the action resumes, retain a structured incident containing:

- affected item, revision, work order, action, agent, and run;
- last substantive action and its timestamp;
- expected next action and expected time or trigger;
- observed stop reason, including `unknown` when that is all the evidence shows;
- causal evidence and its provenance;
- contributing conditions distinguished from the established cause;
- unresolved questions;
- whether this is a recurrence and the prior incident/control it references;
- for a recurrence, why the prior control did not prevent this occurrence; and
- immediate recovery steps distinguished from permanent correction and
  demonstrated prevention.

The gate also requires one concrete prevention action with an accountable owner,
bounded work order, and verification criterion. A bug with no assigned action, a
watcher, or a generic "continue" message does not satisfy it. Unknown cause means
diagnosis remains open; it must never be filled with a provider/model assertion
that the evidence does not establish.

The affected action may resume only after the incident and prevention assignment
are durably linked and the current executor/action/epoch is revalidated. Unrelated
authorized actions continue independently.

## Error contract

Failures must identify the layer, retain the exact action identity, and say what
evidence is missing or stale. Operator text should be actionable without
relabeling a local error as a remote outage or a transport receipt as execution.

| Error class | Required operator treatment |
| --- | --- |
| Coverage missing or legacy-unverified | Name the uncovered item/order/action/role and require explicit enrollment. |
| No actionable executor | Show the required role and why no unique current run can act; route assignment through the handler/coordinator. |
| Stale item, run, generation, epoch, or superseded action | Reject before side effects and direct the caller to refresh the authoritative action. |
| Capability incompatible | Fail closed and identify the required capability/version; never fall back to Board prose or lifecycle status. |
| Non-substantive evidence only | State exactly which signal exists and that execution remains unconfirmed. |
| Progress overdue | Show last substantive timestamp, expected next action, deadline, and planned-versus-unexpected classification. |
| Transport failed or ambiguous | Preserve lease/request identity and whether an external call may have occurred; do not blindly retry. |
| Continuation invalidated | Say which current-state change won the race and confirm the external continuation was not invoked when that is proven. |
| Recovery cause missing | Hold affected resume and enumerate missing incident fields; do not invent a cause. |
| Prevention assignment missing | Hold affected resume until owner, bounded order, and criterion are saved. |
| Dependency unresolved | Retain the block owner and resume condition; keep independent actions runnable. |
| Acceptance missing | Present result and verification separately; never mutate the item to Done from a result alone. |
| Escalation unavailable | Record the specific routing conflict, such as no unique actionable orchestrator, as a bounded failure needing coordination. |

An error must never discard a committed receipt. Same retry key plus unchanged
payload recovers the original outcome; changed payload conflicts. Ambiguous
external side effects are reconciled before retry.

## Acceptance matrix

Independent QA should exercise at least these observable contracts with isolated
databases, identities, clocks, runtime adapters, and tmux sockets:

| Scenario | Required observation |
| --- | --- |
| Save-only then turn end | The action remains Recorded, not started; within its configured deadline exactly one supported continuation is leased or one specific escalation is saved. |
| Read/heartbeat/Working/Queue Start | None acknowledges the action, resets its substantive deadline, or marks it complete. |
| Native queue acceptance without execution | Transport is accepted/unconfirmed; a later exact ack/progress confirms execution, otherwise bounded retry/escalation occurs. |
| Duplicate check or lost response | Same key/payload returns the saved receipt and never duplicates the external call, action, result, or escalation. |
| State changes after lease | Progress, block, result, retirement, exit, close, supersession, item revision change, or executor replacement invalidates the stale continuation before invocation when observed by the final fence. |
| Planned dependency wait | No incident is fabricated before the condition; resolution preserves the exact pending action identity, generation/epoch, and retry receipt for one stable resume. |
| Unexpected pause | Bare resume is rejected until the complete causal record and assigned prevention action exist. |
| Unknown cause | `unknown` remains explicit while diagnosis continues; no provider/model cause is inferred. |
| Recurrence | The new incident links the prior incident/control and records why that control failed. |
| Shared lead/handler actions | Each required action has its own owner/state/deadline; completing one does not clear the other. |
| Independent authorized work | A blocked release or recovery action does not suppress an unrelated ready action. |
| Result and acceptance | Result, verification, acceptance, item completion, Queue completion, and worker closeout remain separate transitions. |
| Evidence summary | Admission, handler-saved Start, action enrollment, built, deployed, relay loaded, wake accepted, and substantive execution observed can all be reported independently. |

Passing synthetic state-machine tests proves only those paths. Prevention remains
open until the exact candidate is independently verified, activation evidence is
phase-specific, and the database handler saves the authorized acceptance.

## Compact future presentation

If a later bounded UI order renders this contract, use one compact action row per
required action. Show the specific state, owner, last substantive timestamp, next
deadline/condition, and primary action. Reveal identity, receipts, evidence
dimensions, and incident detail on demand. Selection uses fill, hover uses outline,
control heights align, placeholders stay muted, and empty space stays quiet.

This presentation guidance is not a prerequisite for the backend or QA work in
order #4402 and is not authorization to expose live project data in tests.
