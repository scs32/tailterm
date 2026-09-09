# Time audit: Enter-to-send

Audit item `wi_481490264d510eec`, order
`wi_481490264d510eec-time-audit-1` (#1017–1018), owner request #1008.
Auditor: lead. This is a read-only evidence review plus this report; no product,
service, model configuration or live test data was changed.

## Finding

The completed **Return sends message** bug (`wi_fc695f0cbc3f7a42`) took
**73 minutes 16 seconds from owner dispatch to final reply**. The main improvement
opportunity is coordination and release preparation, not demonstrated model
slowness. I created an unnecessary **8 minute 39 second permission wait** by
misreading the helper allowance. Repeated evidence collection, additional
approval requests for already assigned work, and an incorrect preview deployment
method added friction afterward.

The observed implementation-through-first-candidate window was **19m17s**,
including tests. The other **53m59s** included setup, review, release and closeout;
that is **not** a claim that all of those minutes were wasted. Independent review,
safe retry tests, target verification and rollback evidence were useful.

## Evidence and method

The handler supplied the completed item at Done revision16 and 216 contiguous
original Board messages (#795–1010), including concurrent work. Snapshot:
`/tmp/tailterm-time-audit-enter-1010/board-795-1010.json`, SHA-256
`ac30d93122351335443718cd503d681d0e31ab8e2e8170c460ebc28d25f6868d`.
The adjacent `enter-final-record.json` is the authoritative item snapshot;
`roster.json` is a current snapshot, not historical proof of worker activity.

Other inspected evidence:

- Product commit `c045440f511d534efdacb2b02d942463191bb715`: one product file
  (`client/board-view.js`, 49 additions/6 deletions) and a 493-line browser fixture.
- Application `d441722a8f7f06d8a097621a24a5e58dcba4ea95`; final report commit
  `48606394112c87e686d8b812d58f77176d50d64d`.
- [Behavior/report](board-enter.md) and [release receipt](releases/tailos-2026-09-09-board-enter.json).
- `.build/worktrees/board-enter/.build/board-enter-evidence/*.log` and
  `.build/worktrees/board-enter/.build/board-enter-independent/qa-report.md`.
- `.build/board-enter-release/mini.json`, asset and public-smoke evidence referenced
  by the release receipt.

All timeline boundaries below use server message timestamps or the recorded
Mini verification timestamp. These are elapsed intervals, **not measured CPU,
model generation, billable time, or continuous human/agent effort**. Git timestamps
corroborate milestones but do not establish when coding began. There were 154
Board messages in #795–948, including other bugs; they cannot all be charged to
this bug. No new tests or provider benchmarks were run for this audit.

The item was created at 17:05:23 UTC, before owner dispatch #795 at 17:18:41.
The audit clock starts at dispatch, not creation. Done was written at 18:31:23,
confirmed in #946 at 18:31:35, and communicated in #948 at 18:31:56.

## Reconciled timeline

September 9, 2026; PDT is UTC minus seven hours. Intervals do not overlap.
Displayed durations are rounded separately; exact intervals sum to 4,395.560s.

| PDT interval | Elapsed | What the evidence establishes |
| --- | ---: | --- |
| 10:18:41–10:35:11 | 16m31s | Owner dispatch #795 to recorded build #820. Includes the mistaken capacity blocker, owner clarification, launch metadata and scheduling. |
| 10:35:11–10:38:59 | 3m48s | Build order to builder's implementation-start announcement #837: launch, introductions, another order retrieval and repository reading. |
| 10:38:59–10:52:49 | 13m50s | Implementation-start announcement to first reported real-hub Chromium/WebKit acceptance pass #847. Coding, fixture creation and testing are not separately timed. |
| 10:52:49–10:58:15 | 5m26s | Additional regressions and first candidate submission #851; unrelated team-launch timeout recorded. |
| 10:58:15–11:05:25 | 7m09s | Exact-hash correction, fresh QA startup/review, repeated evidence collection and independent result #876; these activities overlapped. |
| 11:05:25–11:07:26 | 2m01s | Independent result to release order #883, including lead acceptance and retirement of QA. |
| 11:07:26–11:20:36 | 13m11s | Integration, package/upload, wrong Mini container path, correction, public smoke and Mini asset verification. |
| 11:20:36–11:28:55 | 8m19s | After Mini asset verification: remaining cleanup, rollback/report/receipt preparation and final builder handoff #935. Not proven idle time. |
| 11:28:55–11:31:56 | 3m01s | Lead release review, full saved result, acceptance, separate Done transition and final reply. |

The first candidate arrived 39m35s after dispatch. The remaining 33m41s were
review/release/closeout. The product code did not change after its accepted
candidate; later commits added reports and release records.

## Avoidable delays and their limits

1. **I misclassified ordinary team members as extra helpers.** My #801 asked the
   owner to increase the allowance at 10:20:08. Owner #811 corrected this at
   10:28:47; I withdrew the request in #812. The observed clarification interval
   is **8m39s**, inside the 16m31s setup phase. This was an unnecessary owner
   interruption and queue blocker. Other work continued, so reclaiming exactly
   8m39s from a future end-to-end run is not guaranteed. The fix is to use the
   established normal-team admission rule without asking again.

2. **Already authorized work repeatedly became a new gate.** The builder
   requested the build record after receiving it in the handoff (#833–836).
   QA paused for another retrieval (#863–864); that particular recorded pause was
   only **10 seconds**, not several minutes. The builder requested a scope
   addendum before documenting its own fix (#870), although I had assigned that
   in #858. I clarified in #873 that it could proceed. The assignment-to-clarification
   interval was **3m45s**, but included useful evidence work and is not all idle.
   The worker later asked for QA evidence already on disk (#887–891), and the
   handler requested acceptance already delivered (#944–945, **17 seconds**).
   Keep durable records; stop treating their repeated retrieval as permission.

3. **Evidence was rerun instead of captured once.** After initial success, the
   builder collected fresh transcripts (#861/#870/#872), including a repeated
   unrelated task-form timeout. Each retained timeout waited 30 seconds; the
   second unchanged attempt added at least another 30-second wait, plus unmeasured
   setup. QA also reran Board layout after the builder's layout pass, despite
   the focused assignment. The independent composer check was useful, but its
   report records only **10.10 seconds** for the actual two-engine command.
   The unit log records **0.962 seconds for 121 tests**. These timings do not
   account for fixture development or code review. Capture logs on the first run;
   rerun only for a relevant change, a suspected flaky result or a named gap.

4. **A fabricated full commit suffix caused avoidable reconciliation.** The
   builder reported a nonexistent full hash (#850/#851). I identified the actual
   commit in #852; the builder corrected it in #861, **1m27s later**. QA proceeded
   with the actual hash, so this interval overlaps QA and cannot be added to it.
   Generate candidate IDs and artifact hashes directly from tools; never expand
   a remembered short hash into an invented full identity.

5. **Deployment rediscovered the environment.** The builder attempted an Apple
   Container path and hit missing Rosetta (#911), although Mini already served
   a static directory. I supplied the existing-package method 36 seconds later
   (#914). At #932, **5m24s after that reply**, I observed the resolution still
   unread and sent a directed continuation. The worker was also verifying TailOS;
   this is a delayed checkpoint interval, not evidence of 5m24s doing nothing.
   My attempted `tt resume` was invalid for `needs_input` and required correction
   #934. The initial root build also changed already-served Mini bytes before the
   explicit copy, as the receipt honestly records. Supply the exact current
   listener, served directory, retained package and rollback method in the
   release handoff; build outside the served directory. Check the inbox after
   independent verification when a reported blocker may already be resolved.

6. **Closeout was heavyweight for a small change.** There were **11m20s** from
   the recorded Mini asset check to the final reply, including **8m19s before
   builder handoff** and **3m01s after it**. This included legitimate cleanup,
   rollback checks and writing the final record. Logs do not isolate avoidable
   minutes. A report, receipt template and repeated database narrative should
   share one generated evidence packet, with one explicit release acceptance;
   retain the separate durable Done transition without another conversation.

These windows overlap the timeline and sometimes each other. They must **not**
be summed into a claimed total of wasted time.

## Preserve correctness while shortening the path

Priority recommendations, with ownership:

1. **Lead: one complete handoff, then act.** Include the verified item/order,
   file ownership, exact source, relevant checks, documentation and known release
   method up front. Handler confirmations remain authoritative; routine retrieval
   and writing the assigned report do not create new permission gates.
2. **Builder: save evidence during the first run.** Emit exact commit, commands,
   outcomes, limitations and artifact paths together. Stop unchanged unrelated
   failures after one classification. For this change, retain real Enter/ShiftEnter,
   composition/repeat guards, failed draft/retry and exactly-once message tests.
3. **Reviewer: review the risk, not every suite.** Keep one independent focused
   review for the retry/composer change. Start it as soon as a frozen candidate
   exists while the builder prepares the release packet. Avoid another layout
   matrix when no layout changed and the existing evidence is sound.
4. **Release owner: reuse the verified target procedure.** Prepare the retained
   package away from served files, preserve rollback, and run actual browser smoke
   on each target. Keep the brief exclusive integration/activation slot; do not
   serialize all development or all documentation behind it.
5. **Lead/handler: one result and one acceptance.** Store the complete report,
   attach acceptance, transition Done, verify it, and retire the worker. No
   repeated request for an acceptance already explicitly delivered.

I would first target **10–15 minutes less elapsed time on a comparable small bug**
through these changes. This is a planning estimate, not a measured recoverable
sum or promise: the avoidable allowance wait is the strongest evidence, while
other opportunities overlap useful work. Correctness checks stay in place.

## Models: change the workflow before blaming the builder

The recorded assignments used **gpt-5.6-sol for the builder** (#828) and
**gpt-5.6-terra for independent QA** (#859). No retained per-model token totals,
costs, time-to-first-token or controlled comparison establish that a different
model would have finished this bug faster. The lead's model speed is likewise
not measured by this audit. This report does not recommend a product or quote
current provider pricing.

Keep a capable builder for implementation and one focused reviewer; avoid
additional planner/reviewer identities for a change this small. The first
optimization should be the lead's scheduling and gate decisions. As an
illustration only, halving the entire **13m50s** implementation-to-first-pass
window would save **6m55s (9.4% of the total)** if every other interval stayed
fixed. That window also contains tool execution, so halving model latency alone
would save less. Even an instantaneous first-candidate build would not remove
the setup and release overhead.

For later comparable bugs, record dispatch, first code/test pass, review-ready,
accepted, deployed and Done timestamps plus actual tool durations and any
available usage metrics. Only then compare model assignments under the same
scope/checks. This is a proposed measurement practice, not authorization for
benchmarks, model changes, or more agents now.

## Separate later finding

The later role release discovered that Mini's old Vite preview set
`Content-Encoding: gzip` on a WASM file the client decompresses itself (#972/#973).
Amendment #974 corrected that and the later real Mini browser test passed
(#1002/#1003). Those events occurred after Enter was Done and are **not added**
to its 73m16s timeline. They show why the Enter receipt's Mini raw-asset check
must not be described as a Mini browser-WASM pass: its production browser smoke
was on TailOS. Fewer redundant gates should accompany better targeted evidence,
not weaker release acceptance.
