# Project lead recovery

Bug `wi_b28100d6cc6b7ceb`, original owner intake #2443, bounded order #2448,
amendments #2450/#2452. Assignee: direct owner workspace Codex session, without
impersonating the offline registered lead. Isolated branch `fix/lead-replacement-ui`
starts at `tasks-hub` `d3fe502`; concurrent capacity/lifecycle/search/monitor
candidates remain separate and unmodified.

Projects now exposes **Replace lead**, also available in Project settings.
Choose an online, ordinary agent without an item binding, or start a fresh agent
with explicit machine, folder, runtime/model and permissions. A fresh candidate
waits for assignment; click **Make lead** when it is online. Its saved assignment
notice instructs it to load `tt brief` and obtain the verified handoff from the
actual database handler. This does not restart the former lead's private thread.
Settings displays the current orchestrator read-only so a stale settings form
cannot inadvertently overwrite a later lead choice.

`POST /v1/tasks/{id}/lead` checks the displayed lead revision/name and previous
exact agent/run, then validates the target exact run, eligibility and presence.
Assignment, directed Board notice, audit event and immutable keyed receipt commit
in one transaction. Exact retries recover that receipt, including after another
assignment or closure; changed-payload retries conflict. Legacy orchestrator
settings updates advance the same revision. New work-item dispatch resolves the
new name and verifies its saved exact run. Existing Queue entries are preserved
for deliberate handler-owned recipient reconciliation; historical traffic is not
rewritten or bulk resent.

Fresh launches use the existing encrypted, credential/machine-scoped local launch
journal with a new `replace-lead` kind. Preallocated identities and frozen settings
survive reload. Uncertain launches reconcile the exact registered identity/run;
they never blindly spawn again. A verified unstarted attempt can be retried.
Unresolved absence or mismatched identity remains an explicit inspection dependency.
No previous agent is retired, closed or relabeled. Handler and worker bindings,
project history, groups, quota and networking remain intact.

Validation on isolated fixtures:

- JavaScript suite: 170/170.
- Full Go store/server suites and vet pass.
- Focused Go race checks for lead and Queue pass, including exact-run routing,
  stale/ABA conflicts, caller/payload retry identity, durable reopen/closure receipts,
  invalid lifecycle/offline/handler eligibility and transaction rollback when the
  notice fails.
- Chromium and WebKit: visible Projects action, existing selection, lost assignment
  response retry, fresh launch with lost response, encrypted reload reconciliation,
  zero duplicate launch, stale selection rejection and 1280/390px layout pass.
  Browser launch commands use synthetic registration, not a real provider or tmux
  session. Existing CLI launch code is unchanged. No native Safari/provider or live
  lead recovery is claimed. The initial WebKit test selected an older fixture
  project; explicit project selection corrected that harness failure.

Release compatibility: hub migration adds `tasks.lead_revision` and
`lead_assignments`; retain both on rollback. Keep this frontend available while an
unresolved `replace-lead` journal exists: older frontends cannot understand that
journal kind. Do not restore old vaults or databases over newer user data. The
production WASM/runtime and CLI remain unchanged. Hub deployment uses existing
TrueNAS middleware/listener; frontend targets only TailOS and the existing Mini
preview. Actual publication and handler acceptance are recorded separately.


## Actual release and live recovery

Released application33284bd under order #2456 to TailOS/e439f3fa and the unchanged
Mini listener PID60799; all three origins match all 81 public assets plus manifest
and pass fresh production Chromium/WASM/synthetic-vault checks. Cloudflare's
`/index.html` returns308; the initial hash probe was corrected to canonical `/`.
The hub release and consistent backup both passed integrity/FK verification.

The owner's urgent amendment #2458 superseded the initial no-live-replacement
boundary. The old tmux session was absent. One fresh `lead-recovery` was launched
with the prior Astra/high/folder/permission settings and atomically assigned by
receipt #2461. Its real online run is `agt_64e8e823485a3434` /
`run_66d063bf724968d0`. Handler verified it and reconciled recipient pins in
#2474/#2475; new lead's substantive handoff acknowledgement is #2476.
See the [actual release receipt](releases/tailos-2026-09-11-lead-recovery.json).
This operational live recovery is distinct from the synthetic browser tests.

New lead accepted the delivered recovery in #2481. Handler #2483 verified Bug
Done revision8, receipt `wir_6536bb90dd2da134`, with complete Board-linked evidence
preserved. Both remain available for the open project.
