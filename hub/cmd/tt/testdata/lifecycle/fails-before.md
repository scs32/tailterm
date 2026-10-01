# Delivery lifecycle test: fails before each of the five fixes

This is the owner-approved substitution for running `TestDeliveryLifecycle` on
the five pre-fix hub commits (decision #18952, delegated owner answer #18955;
item `wi_a747a13aa62f4712`, order #13868). The test cannot compile on those
commits: `7ab5100` has no verification, release or deployment code, and
`8b1f4d4` has no release or deployment code. Instead, five small patches in
`prefix/` each put one pre-fix behaviour back on the candidate's hub, and the
test must fail at runtime, with the error teams hit live, in the subtest named
for that dead end. A compile error does not count.

Recorded once, on 2026-10-01, on Stephens-Mini (go1.26.5 darwin/arm64), against
`hub/cmd/tt/lifecycle_e2e_test.go` at Git blob `be236ed43f3c3cc5adda011ae2d8c73c3e08c852`
on base `16c3648`. The patches are evidence. No test applies them, and they
must never be committed applied.

## How each run was made

In a scratch detached worktree of the candidate, from the repository root, for
each patch:

```sh
git apply hub/cmd/tt/testdata/lifecycle/prefix/fN-*.patch
(cd hub && go test ./cmd/tt -run '^TestDeliveryLifecycle$' -count=1)    # must fail
git apply -R hub/cmd/tt/testdata/lifecycle/prefix/fN-*.patch
(cd hub && go test ./cmd/tt -run '^TestDeliveryLifecycle$' -count=1)    # must pass again
```

The subtests are:

- **A**: `base moved after queueing, plan names the worktree, receipt over 64 KiB, follow-ups before verification`
- **B**: `base moved after queueing, plan names the repository root, receipt over 64 KiB, owner-decision before verification`

Unpatched, the candidate passes:

```text
ok  	github.com/scs32/tailterm/hub/cmd/tt	2.196s
```

## Summary

| Mutation                  | Fix       | Pre-fix commit | Dead end                                           | Fails in | Passes again reversed |
| ------------------------- | --------- | -------------- | -------------------------------------------------- | -------- | --------------------- |
| `prefix/f1-4b5adb2.patch` | `4b5adb2` | `7ab5100`      | owner-decision disposition before verification     | A and B  | yes                   |
| `prefix/f2-c343819.patch` | `c343819` | `8b1f4d4`      | receipt over 64 KiB                                | A and B  | yes                   |
| `prefix/f3-5480dae.patch` | `5480dae` | `9c99ddc`      | base moved after queueing; plan names the worktree | A and B  | yes                   |
| `prefix/f4-f3b1c19.patch` | `f3b1c19` | `dbe99b6`      | follow-ups disposition before verification         | A        | yes                   |
| `prefix/f5-0ce60c0.patch` | `0ce60c0` | `f3b1c19`      | plan names the repository root                     | B        | yes                   |

Three patches fail in both subtests because the behaviour they remove is on
both paths: every variant sends a full-size receipt (f2), ends with the owner's
owner-accept (f1), and is accepted on the moved base (f3). f4 fails only where
the disposition is follow-ups, and f5 only where the plan names the repository
root. f3 restores both halves of its fix together (the plan had to name the
entry's `.git` directory, and acceptance bound the queue-time base), so its
one refusal covers the moved base and the worktree plan; the two halves were
not recorded apart.

A line reading "was refused for another reason" is a refusal the test expects
at an earlier step, reported without stopping so that the step that failed
live still runs and reports its own error.

## f1: before `4b5adb2` (let the owner resolve an owner-decision review disposition)

- Fix: `4b5adb2`. Pre-fix commit (its parent): `7ab5100`.
- Patch: `prefix/f1-4b5adb2.patch`.
- Dead end: owner-decision disposition before verification. Live error: owner-accept refused: unknown disposition.
- Failing subtest: A and B.

With the patch applied (exit 1):

```text
--- FAIL: TestDeliveryLifecycle (1.10s)
    --- FAIL: TestDeliveryLifecycle/base_moved_after_queueing,_plan_names_the_worktree,_receipt_over_64_KiB,_follow-ups_before_verification (0.60s)
        lifecycle_e2e_test.go:84: step 1: queue add: refused until the handler confirms the scope of order #2
        lifecycle_e2e_test.go:84: step 2: scope confirmed by the handler for revision 1 and order #2
        lifecycle_e2e_test.go:84: step 3: queue add: entry tqe_6a0f0c85c698cb92 queued on base B0 a9e6bfea2897b80e96c657ba9ebaa829a4725785
        lifecycle_e2e_test.go:85: step 4: launch: tasks-hub moved to B1 cb8dc7c546fd31c96e83610aa35b0fd90bf67a7d, then lead, builder, reviewer and verifier bound to the item
        lifecycle_e2e_test.go:87: step 5: review round 1: blocker b1 raised on C1 d9379b4e1877a2d3bb9b9e29ce0ef6b2bb3fa0f9
        lifecycle_e2e_test.go:88: step 6: fix: C2 441886033b6a1120ba65f880e9c06350ce862dc1 is the one-line fix for b1
        lifecycle_e2e_test.go:88: step 7: review round 2: b1 resolved on C2, a2 left failed with finding n1
        lifecycle_e2e_test.go:88: step 8: persisted resolution checked: b1 on C2 by result #10, follow-up wi_dd23b7e796521777 filed for n1
        lifecycle_e2e_test.go:89: an agent-authored owner-accept was refused for another reason: hub: 409 conflict: review convergence: unknown disposition (want "owner-accept requires an owner-authored notice")
        lifecycle_e2e_test.go:89: the owner's owner-accept before verification was refused for another reason: hub: 409 conflict: review convergence: unknown disposition (want "passing receipt for current scope and exact accepted candidate required")
        lifecycle_e2e_test.go:89: step 9: disposition: follow-ups recorded for C2 before verification
        lifecycle_e2e_test.go:91: step 10: verification receipt: 69 checks, 3 attempts each (207 attempts), 139178 bytes (over 65536), plan names the worktree
        lifecycle_e2e_test.go:92: the owner's owner-accept of a verified candidate after a follow-ups disposition: hub: 409 conflict: review convergence: unknown disposition
    --- FAIL: TestDeliveryLifecycle/base_moved_after_queueing,_plan_names_the_repository_root,_receipt_over_64_KiB,_owner-decision_before_verification (0.50s)
        lifecycle_e2e_test.go:84: step 1: queue add: refused until the handler confirms the scope of order #2
        lifecycle_e2e_test.go:84: step 2: scope confirmed by the handler for revision 1 and order #2
        lifecycle_e2e_test.go:84: step 3: queue add: entry tqe_966811e28c883e1f queued on base B0 def78467948cc4c167b532a84c0ad63c90e4df47
        lifecycle_e2e_test.go:85: step 4: launch: tasks-hub moved to B1 477efeebd8b561741d32f3a762c7d232e8e8e2ad, then lead, builder, reviewer and verifier bound to the item
        lifecycle_e2e_test.go:87: step 5: review round 1: blocker b1 raised on C1 decf52b602e67015f1848671d7bd7f4717503604
        lifecycle_e2e_test.go:88: step 6: fix: C2 57d0a1629bb84fee690b782a9cf3cdf7b9a88a9b is the one-line fix for b1
        lifecycle_e2e_test.go:88: step 7: review round 2: b1 resolved on C2, a2 left failed with finding n1
        lifecycle_e2e_test.go:88: step 8: persisted resolution checked: b1 on C2 by result #10, follow-up wi_99638e3f9ce99264 filed for n1
        lifecycle_e2e_test.go:89: an agent-authored owner-accept was refused for another reason: hub: 409 conflict: review convergence: unknown disposition (want "owner-accept requires an owner-authored notice")
        lifecycle_e2e_test.go:89: step 9: disposition: owner-decision recorded for C2 before verification
        lifecycle_e2e_test.go:91: step 10: verification receipt: 69 checks, 3 attempts each (207 attempts), 139175 bytes (over 65536), plan names the root
        lifecycle_e2e_test.go:92: the owner's owner-accept of a verified candidate after a owner-decision disposition: hub: 409 conflict: review convergence: unknown disposition
FAIL
FAIL	github.com/scs32/tailterm/hub/cmd/tt	1.435s
FAIL
```

With the patch reversed (exit 0):

```text
ok  	github.com/scs32/tailterm/hub/cmd/tt	2.179s
```

## f2: before `c343819` (allow full verification receipts on the verification route)

- Fix: `c343819`. Pre-fix commit (its parent): `8b1f4d4`.
- Patch: `prefix/f2-c343819.patch`.
- Dead end: receipt over 64 KiB. Live error: receipt save refused by the shared 64 KiB body limit.
- Failing subtest: A and B.

With the patch applied (exit 1):

```text
--- FAIL: TestDeliveryLifecycle (2.09s)
    --- FAIL: TestDeliveryLifecycle/base_moved_after_queueing,_plan_names_the_worktree,_receipt_over_64_KiB,_follow-ups_before_verification (1.04s)
        lifecycle_e2e_test.go:84: step 1: queue add: refused until the handler confirms the scope of order #2
        lifecycle_e2e_test.go:84: step 2: scope confirmed by the handler for revision 1 and order #2
        lifecycle_e2e_test.go:84: step 3: queue add: entry tqe_1867a458773174ee queued on base B0 385e4314637df140673448a3b8db5d1d0e9500b1
        lifecycle_e2e_test.go:85: step 4: launch: tasks-hub moved to B1 9bd0996dc85b122468222a9ff470ea5f10bb09fa, then lead, builder, reviewer and verifier bound to the item
        lifecycle_e2e_test.go:87: step 5: review round 1: blocker b1 raised on C1 63fc9022c7bbb3f44bb93a9266fc2485fb291f05
        lifecycle_e2e_test.go:88: step 6: fix: C2 b5e60fb6fb40ca4e5fc0b489974715d8b27b5c3d is the one-line fix for b1
        lifecycle_e2e_test.go:88: step 7: review round 2: b1 resolved on C2, a2 left failed with finding n1
        lifecycle_e2e_test.go:88: step 8: persisted resolution checked: b1 on C2 by result #10, follow-up wi_a0e6953c3fa6e1ce filed for n1
        lifecycle_e2e_test.go:89: step 9: disposition: follow-ups recorded for C2 before verification
        lifecycle_e2e_test.go:91: tt verification receipt of 139456 bytes (over the shared 65536-byte limit): hub: 400 invalid scope metadata request: body exceeds 65536 bytes
    --- FAIL: TestDeliveryLifecycle/base_moved_after_queueing,_plan_names_the_repository_root,_receipt_over_64_KiB,_owner-decision_before_verification (1.06s)
        lifecycle_e2e_test.go:84: step 1: queue add: refused until the handler confirms the scope of order #2
        lifecycle_e2e_test.go:84: step 2: scope confirmed by the handler for revision 1 and order #2
        lifecycle_e2e_test.go:84: step 3: queue add: entry tqe_fbce1dba0dc3418f queued on base B0 bdd42c9d2cf1cd2f40366adf6440f108b2deffd0
        lifecycle_e2e_test.go:85: step 4: launch: tasks-hub moved to B1 a4badf602863a4a466383027c2d57ad09483f0eb, then lead, builder, reviewer and verifier bound to the item
        lifecycle_e2e_test.go:87: step 5: review round 1: blocker b1 raised on C1 32687d25a489e10d5caadb18cb5537c62b7240b5
        lifecycle_e2e_test.go:88: step 6: fix: C2 1cf564fad8a97ac2824b1e21b1c6f9f2f08bb967 is the one-line fix for b1
        lifecycle_e2e_test.go:88: step 7: review round 2: b1 resolved on C2, a2 left failed with finding n1
        lifecycle_e2e_test.go:88: step 8: persisted resolution checked: b1 on C2 by result #10, follow-up wi_5fdbf0f66b29d4e1 filed for n1
        lifecycle_e2e_test.go:89: step 9: disposition: owner-decision recorded for C2 before verification
        lifecycle_e2e_test.go:91: tt verification receipt of 139453 bytes (over the shared 65536-byte limit): hub: 400 invalid scope metadata request: body exceeds 65536 bytes
FAIL
FAIL	github.com/scs32/tailterm/hub/cmd/tt	2.435s
FAIL
```

With the patch reversed (exit 0):

```text
ok  	github.com/scs32/tailterm/hub/cmd/tt	1.732s
```

## f3: before `5480dae` (accept verified queue teams on their verified base and worktree)

- Fix: `5480dae`. Pre-fix commit (its parent): `9c99ddc`.
- Patch: `prefix/f3-5480dae.patch`.
- Dead end: base moved after queueing; plan names the worktree. Live error: acceptance 409 repository/base mismatch.
- Failing subtest: A and B.

With the patch applied (exit 1):

```text
--- FAIL: TestDeliveryLifecycle (1.40s)
    --- FAIL: TestDeliveryLifecycle/base_moved_after_queueing,_plan_names_the_worktree,_receipt_over_64_KiB,_follow-ups_before_verification (0.74s)
        lifecycle_e2e_test.go:84: step 1: queue add: refused until the handler confirms the scope of order #2
        lifecycle_e2e_test.go:84: step 2: scope confirmed by the handler for revision 1 and order #2
        lifecycle_e2e_test.go:84: step 3: queue add: entry tqe_d3ee71bd6a27b89b queued on base B0 242496dd60263fde969c1458d84767dfdbc4f744
        lifecycle_e2e_test.go:85: step 4: launch: tasks-hub moved to B1 51a076b7c67cc2be4edc56ff6987cc48e5f8c7dd, then lead, builder, reviewer and verifier bound to the item
        lifecycle_e2e_test.go:87: step 5: review round 1: blocker b1 raised on C1 17527a2e378476ef5717fb4f7b1dcfed2e393717
        lifecycle_e2e_test.go:88: step 6: fix: C2 5cacd0ea9da717427a5d1c61252a4a644505acd5 is the one-line fix for b1
        lifecycle_e2e_test.go:88: step 7: review round 2: b1 resolved on C2, a2 left failed with finding n1
        lifecycle_e2e_test.go:88: step 8: persisted resolution checked: b1 on C2 by result #10, follow-up wi_0501f4ecdddf18a9 filed for n1
        lifecycle_e2e_test.go:89: step 9: disposition: follow-ups recorded for C2 before verification
        lifecycle_e2e_test.go:91: step 10: verification receipt: 69 checks, 3 attempts each (207 attempts), 139178 bytes (over 65536), plan names the worktree
        lifecycle_e2e_test.go:92: done save of C2 on the newer base B1 (queued on B0, plan names the worktree): hub: 409 conflict: verification: acceptance repository/base mismatch
    --- FAIL: TestDeliveryLifecycle/base_moved_after_queueing,_plan_names_the_repository_root,_receipt_over_64_KiB,_owner-decision_before_verification (0.66s)
        lifecycle_e2e_test.go:84: step 1: queue add: refused until the handler confirms the scope of order #2
        lifecycle_e2e_test.go:84: step 2: scope confirmed by the handler for revision 1 and order #2
        lifecycle_e2e_test.go:84: step 3: queue add: entry tqe_2558a098d158610a queued on base B0 242496dd60263fde969c1458d84767dfdbc4f744
        lifecycle_e2e_test.go:85: step 4: launch: tasks-hub moved to B1 537530a1ce38f686398ff7b8883c3c1608e0247a, then lead, builder, reviewer and verifier bound to the item
        lifecycle_e2e_test.go:87: step 5: review round 1: blocker b1 raised on C1 dc6c5be50d4e984a57bdcdb851fad8ff6b4e1ea7
        lifecycle_e2e_test.go:88: step 6: fix: C2 6b71447785827a10e03cd2758611dd7a68d4415f is the one-line fix for b1
        lifecycle_e2e_test.go:88: step 7: review round 2: b1 resolved on C2, a2 left failed with finding n1
        lifecycle_e2e_test.go:88: step 8: persisted resolution checked: b1 on C2 by result #10, follow-up wi_09e45aba26b7c9aa filed for n1
        lifecycle_e2e_test.go:89: step 9: disposition: owner-decision recorded for C2 before verification
        lifecycle_e2e_test.go:91: step 10: verification receipt: 69 checks, 3 attempts each (207 attempts), 139453 bytes (over 65536), plan names the root
        lifecycle_e2e_test.go:92: step 11: acceptance: owner-accept for C2, completion report nrpt_3934fcd959546735, item saved done by the owner
        lifecycle_e2e_test.go:93: tt team queue accept of C2 on the newer base B1 (queued on B0, plan names the root): hub: 409 conflict: verification: acceptance repository/base mismatch
FAIL
FAIL	github.com/scs32/tailterm/hub/cmd/tt	1.756s
FAIL
```

With the patch reversed (exit 0):

```text
ok  	github.com/scs32/tailterm/hub/cmd/tt	1.751s
```

## f4: before `f3b1c19` (let the owner resolve a follow-ups review disposition)

- Fix: `f3b1c19`. Pre-fix commit (its parent): `dbe99b6`.
- Patch: `prefix/f4-f3b1c19.patch`.
- Dead end: follow-ups disposition before verification. Live error: owner-accept 409: resolves an owner-decision disposition.
- Failing subtest: A.

With the patch applied (exit 1):

```text
--- FAIL: TestDeliveryLifecycle (1.51s)
    --- FAIL: TestDeliveryLifecycle/base_moved_after_queueing,_plan_names_the_worktree,_receipt_over_64_KiB,_follow-ups_before_verification (0.74s)
        lifecycle_e2e_test.go:84: step 1: queue add: refused until the handler confirms the scope of order #2
        lifecycle_e2e_test.go:84: step 2: scope confirmed by the handler for revision 1 and order #2
        lifecycle_e2e_test.go:84: step 3: queue add: entry tqe_c78455f444ae7a50 queued on base B0 1fce05e79ffc266103c90c4f5aa9f10d0b5ed10a
        lifecycle_e2e_test.go:85: step 4: launch: tasks-hub moved to B1 0968cefcd2c354ea7f6e0f01c9ab8bba7a6491f6, then lead, builder, reviewer and verifier bound to the item
        lifecycle_e2e_test.go:87: step 5: review round 1: blocker b1 raised on C1 d9d2787b35ea8d47799fce432c86037ab668a96c
        lifecycle_e2e_test.go:88: step 6: fix: C2 d0a8d6100894eecf03ca770c29e20895f00c9e0b is the one-line fix for b1
        lifecycle_e2e_test.go:88: step 7: review round 2: b1 resolved on C2, a2 left failed with finding n1
        lifecycle_e2e_test.go:88: step 8: persisted resolution checked: b1 on C2 by result #10, follow-up wi_bf11871bb7f2dde3 filed for n1
        lifecycle_e2e_test.go:89: the owner's owner-accept before verification was refused for another reason: hub: 409 conflict: review convergence: owner-accept resolves an owner-decision disposition (want "passing receipt for current scope and exact accepted candidate required")
        lifecycle_e2e_test.go:89: step 9: disposition: follow-ups recorded for C2 before verification
        lifecycle_e2e_test.go:91: step 10: verification receipt: 69 checks, 3 attempts each (207 attempts), 139456 bytes (over 65536), plan names the worktree
        lifecycle_e2e_test.go:92: the owner's owner-accept of a verified candidate after a follow-ups disposition: hub: 409 conflict: review convergence: owner-accept resolves an owner-decision disposition
FAIL
FAIL	github.com/scs32/tailterm/hub/cmd/tt	1.846s
FAIL
```

With the patch reversed (exit 0):

```text
ok  	github.com/scs32/tailterm/hub/cmd/tt	2.665s
```

## f5: before `0ce60c0` (treat a repository root and its .git directory as one repository)

- Fix: `0ce60c0`. Pre-fix commit (its parent): `f3b1c19`.
- Patch: `prefix/f5-0ce60c0.patch`.
- Dead end: plan names the repository root. Live error: queue accept 409 repository/base mismatch.
- Failing subtest: B.

With the patch applied (exit 1):

```text
--- FAIL: TestDeliveryLifecycle (1.55s)
    --- FAIL: TestDeliveryLifecycle/base_moved_after_queueing,_plan_names_the_repository_root,_receipt_over_64_KiB,_owner-decision_before_verification (0.65s)
        lifecycle_e2e_test.go:84: step 1: queue add: refused until the handler confirms the scope of order #2
        lifecycle_e2e_test.go:84: step 2: scope confirmed by the handler for revision 1 and order #2
        lifecycle_e2e_test.go:84: step 3: queue add: entry tqe_84c9dd9c20f75141 queued on base B0 b93f2f58f4fb93e90eab5da26850f9ff53ae2238
        lifecycle_e2e_test.go:85: step 4: launch: tasks-hub moved to B1 3ea28f4b96576b17b9706fed60f1b4b4879d4f89, then lead, builder, reviewer and verifier bound to the item
        lifecycle_e2e_test.go:87: step 5: review round 1: blocker b1 raised on C1 efe927250eceab59ae50ae1decea41ad673903ca
        lifecycle_e2e_test.go:88: step 6: fix: C2 e1add3a62a7fcaf04834e8405f1a48f4cb348428 is the one-line fix for b1
        lifecycle_e2e_test.go:88: step 7: review round 2: b1 resolved on C2, a2 left failed with finding n1
        lifecycle_e2e_test.go:88: step 8: persisted resolution checked: b1 on C2 by result #10, follow-up wi_715dcb3c4ff1fb50 filed for n1
        lifecycle_e2e_test.go:89: step 9: disposition: owner-decision recorded for C2 before verification
        lifecycle_e2e_test.go:91: step 10: verification receipt: 69 checks, 3 attempts each (207 attempts), 139453 bytes (over 65536), plan names the root
        lifecycle_e2e_test.go:92: step 11: acceptance: owner-accept for C2, completion report nrpt_5f5f8f3cb0c3b991, item saved done by the owner
        lifecycle_e2e_test.go:93: tt team queue accept of C2 on the newer base B1 (queued on B0, plan names the root): hub: 409 conflict: verification: acceptance repository/base mismatch
FAIL
FAIL	github.com/scs32/tailterm/hub/cmd/tt	1.886s
FAIL
```

With the patch reversed (exit 0):

```text
ok  	github.com/scs32/tailterm/hub/cmd/tt	1.821s
```
