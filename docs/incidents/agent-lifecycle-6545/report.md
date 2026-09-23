# Agent lifecycle audit — September 14, 2026

Bug `wi_87e95365b37bf342` revision 1; owner request #6542, bounded audit
order #6545, guardrail/disposition order #6557. Lead-recovery owns disposition;
the actual database handler collected native evidence; root performed the join,
host corroboration and analysis.

The concern is supported: most retained helper sessions are not currently
assigned execution work. At **17:41:32 UTC (10:41:32 PDT)** the complete native
roster contained 83 records: 65 closed and 18 not closed. Of those 18, 15 were
item helpers, two were the working shared lead/handler, and one was the old lead
record. The separate local default-tmux snapshot contained 26 sessions, including
the same 15 named helper sessions. Other sessions include operators and unrelated
projects; they are not cleanup targets.

| Retained helpers | Count | Native evidence / disposition needed |
| --- | ---: | --- |
| Reader team | 3 | All current deliveries returned results; source integration accepted, release remains. Assign release responsibility and release eligible predecessors. |
| Cleanup team | 3 | All current deliveries returned results, including QA g13. Lead must disposition acceptance/integration and any precisely identified remaining check. |
| Queue phase team | 3 | All current deliveries returned results. QA still has roster status `running`, despite result at 05:05:57 UTC. |
| Allocation-context QA | 1 | Installed QA result returned; Mini release accepted. Qualify phase release and service ownership for closeout. |
| Legacy server-monitor helpers | 2 | No committed current obligation. Remaining responsibility and close eligibility are unverified. |
| Pause API/UI/QA | 3 | Current native progress recorded; API/UI source work and QA planning evidence exist. Candidate QA remains pending. |

Thus **10 helpers have returned results, two lack a current structured assignment,
and three have progress records**. Progress records are not proof of physical
execution at this instant. Results are not, by themselves, permission to close.
All six bound parent items remain open; that alone does not justify retaining
every member of every team.

The old `lead` record is still `running`, with no committed mandatory action,
while its named `lead` session is absent from the local default socket. The
replacement lead is separate and present. The original lead needs exact host,
role-slot and replacement reconciliation; absence on one socket is not sufficient
authority to close or restart it.

## Causes established and limits

1. **The completion-to-cleanup transition remains disconnected in the installed
   system.** The existing cleanup design explicitly identifies this gap. Native
   cleanup skips agents that are not already `closed`
   (`hub/cmd/tt/cleanup.go`, line 174). A returned result or accepted source can
   leave a helper open without a durable closeout obligation. The cleanup
   candidate addresses this, but the current deployment documentation does not
   establish that it is installed. Per-member acceptance, retained responsibility
   and service eligibility still need qualification.
2. **Terminal delivery and roster status diverge.** Queue QA is a concrete
   example: its latest result is about 12 hours 35 minutes older than this audit,
   but its roster says `running`. Fresh heartbeat/event timestamps cannot repair
   that semantic discrepancy. Native all-action reads show no nonterminal sibling
   for any of the ten result-only helpers.
3. **Historical warnings were substituted for current state.** Handler summaries
   #6526/#6541/#6549 said Pause workers lacked native acknowledgment/progress.
   Fresh payloads #6550 show all three progressing, with acknowledgment and
   progress events. The handler acknowledged the interpretation error in #6554.
   Their earlier escalations remain historical facts, not current phase facts.
4. **Legacy enrollment is incomplete.** Both monitor helpers and the shared-role
   records lack committed current obligations. This proves incomplete structured
   coverage, not that the working lead or handler is idle. The original cause of
   every legacy retention is not established by this audit.

The audit did not close, restart or retire agents, pause a task, alter services,
or infer eligibility from a label. It does not claim the 65 legacy `cleanupDone`
flags independently prove successful receipts and exact target absence.

## Guardrail: every helper must have a durable next disposition

Use the existing successor and member-closeout infrastructure, not another watcher.

1. Returning a result must create or preserve exactly one **lead-owned disposition
   obligation**. Acceptance, focused rework, a named dependency or release is the
   next action; silence is not a completed handoff.
2. Accepting a terminal phase or an acknowledged successor transfer must atomically
   record **either an exact-run cleanup obligation or explicit retention** for
   every affected member. Retention requires actual scope, owner, reason and
   deadline/resume condition. An open parent item is insufficient.
3. The existing relay consumes saved cleanup work after crashes and lost replies.
   Before closing, it revalidates exact identity, independent assignments, shared
   roles and useful-service ownership. Success requires both a native receipt
   and exact target-absence evidence.
4. Existing UI counts and summaries derive from current typed actions and their
   source/version. Returned results cannot count as execution; older alerts cannot
   override newer progress. Unknown legacy coverage remains visible and receives
   an owned reconciliation action.

Lead-recovery owns per-member disposition and delivery of this guardrail under
#6557. The database handler owns saved linkage and evidence. Extend the existing
cleanup/successor and typed-provenance items rather than building duplicate
infrastructure. Already-authorized shipping work remains independent.

Acceptance must demonstrate a crash after acceptance but before cleanup recovers
without a model reminder; lost replies produce one cleanup result; one retained QA
member does not retain all finished builders; replacement runs/shared roles/useful
services survive; and stale alerts cannot corrupt current activity classification.

**Status:** audit and guardrail requirements recorded; installed prevention and
per-member close eligibility are not established. Full service/acceptance/Queue
requalification remains outstanding, so this report is not a blanket cleanup list.

## Evidence

[Structured report](report.json) contains each exact agent/run/binding, native
delivery/phase/order/deadline, classifications, limits and test criteria.
[Original handler manifest](evidence/handler-manifest-original.json) and all 26
raw HTTP responses are retained in this directory; every response hash was checked.
All responses were HTTP 200 and roster identity/status/binding bookends matched.
The endpoint returns the full roster without pagination; no cursor or atomic
cross-source cutoff is invented. Sequential observations remain distinct from an
atomic snapshot. The [host snapshot](evidence/host-snapshot.json) preserves local
session/process metadata without terminal contents.
