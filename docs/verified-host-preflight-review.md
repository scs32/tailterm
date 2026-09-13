# Verified-host preflight design review

Review scope: Bug `wi_9cc74828bb3499a4` revision 1, bounded order #4410,
handler Start #4486. This is a documentation-only operator/API/QA review. It
does not modify backend or UI code, run live fixtures, or authorize deployment.

Normative behavior is defined in
[the verified-host preflight contract](verified-host-preflight-contract.md).
This document records the baseline design findings and the evidence required to
accept the API worker's implementation.

## Baseline finding

The current deployment helper, `scripts/deploy-truenas-hub.py`, has a fixed SSH
argument vector for alias `truenas`, but it does not prove the identity of the
host that answered before mutation. Its first remote operation may create a hub
token, followed by release-directory creation and binary upload. The established
route therefore exists, but route selection and actual-host verification are not
the same control.

The prior backup gates were performed as handler-owned remote scripts rather
than one enforced repository operation. Board evidence shows a repeating causal
sequence:

| False blocker | Correction | Demonstrated remote gate |
| --- | --- | --- |
| #3731 | #3733 | #3743 |
| #4077 | #4080 | #4085 |
| #4320 | #4322 | #4327 |

The established cause is execution-context confusion: a TrueNAS absolute path
was interpreted on the Mini. The repeated successful correction shows that the
existing route was available; the local missing/read-only results did not prove
a TrueNAS filesystem or service failure. Instructions failed as a permanent
control because the same action remained executable locally.

## Required implementation shape

The implementation may be a reusable module plus thin commands, or equally
testable seams, provided the deployment and backup callers cannot bypass it.
Review will reject a helper that is documented but optional. It will also reject
a deployment command that performs SQLite backup work under the lead/operator's
identity; the handler-owned backup producer and deployment receipt consumer are
separate paths.

The minimum design is:

1. Parse one structured request and bind its stable retry identity.
   A deployment caller validates its local release name and artifact before
   starting a remote backup, so a missing local build cannot create an
   unnecessary backup and then report a pre-deployment failure.
2. Select the declared runner from the explicit route; never infer local from
   the current directory or from source-path availability.
3. Use that runner for a read-only host identity probe.
4. Verify the target-side executable and source filesystem metadata on the same
   verified host.
5. Expose an explicit handler-side mutation boundary and only then create the destination or
   open SQLite.
6. Publish one structured, sanitized handler receipt with a precise result class.
7. Make deployment validate that exact receipt and recheck the host, without
   opening either database or creating a backup.
8. Place receipt validation and the host recheck before all deployment-side
   writes, including conditional token creation, release staging, upload, and
   middleware updates.

The route should remain an argv sequence or typed transport object. Passing
host, source, destination, or executable through string-form shell
interpolation would reintroduce ambiguity and quoting/injection risk.

## Interface review checklist

The API handoff must identify the exact implementation and test paths and answer
each item below.

| Review item | Acceptance question |
| --- | --- |
| Required fields | Are expected host, route, source DB, destination, executable, operation, schema version, and retry identity all required and separately represented? |
| Canonicalization | Are paths and host/executable identities canonicalized before retry comparison, with unsafe/relative destinations rejected? |
| Runner identity | Does the same route/runner perform both the host probe and the operation? |
| Host proof | Is observed host identity compared to approved expected identity rather than trusting the SSH alias? |
| Expected-host provenance | Is the expected identity supplied by the reviewed order/inventory rather than learned from the same route immediately before comparison? |
| Local fallback | Can any failure cause a local `stat`, `mkdir`, SQLite open, or executable lookup for the remote request? The accepted answer is no. |
| Mutation ordering | Are host, executable, and source verification complete before destination creation, SQLite open, token/release staging, upload, or middleware mutation? |
| Error taxonomy | Are host mismatch, route failure, probe failure, source failure, destination failure, backup failure, and verification failure distinguishable in machine-readable output? |
| Operator wording | Does every rejected preflight state what is known, what is unknown, and whether mutation began without upgrading an inference into a remote outage? |
| Retry safety | Does exact replay avoid duplicate/overwrite behavior, and does changed input under the same retry identity fail before mutation? |
| Destination safety | Does an unproven existing destination remain untouched? |
| Evidence hygiene | Are only sanitized identities, counts, sizes, digests, modes, and check results emitted? Is the exact saved receipt-bytes digest handed off separately from the receipt? |
| Ownership | Is SQLite backup/integrity/profile work executable only through the handler path, while deployment merely validates its exact receipt? |
| Integration | Are backup preflight and receipt validation mandatory in their actual handler/deployment entry points rather than separate unused commands? |

## QA evidence required

Independent QA should use injected or synthetic runners and isolated temporary
paths/databases. It must not use live task/profile data.

1. Wrong-host test: make the route return a valid but different host identity.
   Assert `host-mismatch`, `mutationStarted: false`, and zero invocations of
   every filesystem/database/deployment mutator.
2. Routing test: make spawn, authentication/host-key, and timeout failures
   return `route-unavailable`. Assert that the implementation never examines the
   remote absolute paths locally.
3. Probe test: return missing/malformed identity data. Assert
   `host-verification-failed` before executable/source access.
4. Lost-result test: dispatch the remote command but return no structured
   result. Assert `preflight-result-missing` with unknown mutation state; do not
   infer a route outage or retry under a different identity.
5. Executable/source tests: verify each distinct failure is target-scoped and
   occurs before destination creation or SQLite open.
6. Destination collision tests: pre-create unrelated bytes and also inject a
   competing destination immediately before publication; prove neither is
   overwritten.
7. Retry tests: exact request replay produces one logical backup/recovered
   result; one changed bound field produces `request-conflict` without mutation.
8. Correct-route test: prove the same synthetic runner performs probe and
   operation, and verify the full success evidence shape.
9. Ownership test: invoke the deployment receipt consumer and assert zero SQLite
   opens, backup creation, or replacement-receipt writes.
10. Deployment-order test: supply a missing, changed, or invalid receipt and spy
   on token creation, release mkdir, upload, and middleware mutation; none may
   occur.
11. External-pin test: change receipt evidence and recompute its internal
    evidence digest. Supply the original handler-saved receipt-bytes SHA-256 and
    assert `verification-failed` before JSON acceptance, remote probe, or
    mutation. A byte-exact receipt with the exact pin must pass this gate.
12. Actual remote qualification, owned by the handler: retain host/route/source/
   destination/executable, mode 0600 and UID/GID, size/SHA-256,
   `integrity_check`, foreign-key count, canonical profile comparison, and the
   exact saved receipt-bytes SHA-256 delivered to the consumer. This is release
   evidence, not a live test fixture or proof of deployment.

## Review status

Candidate review: **the isolated API/QA implementation satisfies this v1 design
contract; live qualification and release acceptance remain open**.

The reviewed API candidate is commit `1246727966ae1b5c9aafc2515fed529af20e5bed`
on top of `b1d3285`, including focused corrections `a952a7a`, `49d60f9`, and
`4121796`.
Its implementation paths are
`scripts/truenas_release_preflight.py` and `scripts/deploy-truenas-hub.py`; its
primary regression path is `tests/truenas-release-preflight.test.js`.
Independent QA is commit `96bcff78a4cecac8b244be57fff920b0c0d08a5f`,
including its earlier v1 coverage commits and
`tests/truenas_release_preflight_test.py`.

Review verification:

- `node --test tests/truenas-release-preflight.test.js` at API commit `1246727`
  passed 9/9 tests. It covers wrong host/no mutation, noncanonical source,
  correct-route backup and exact replay, publication race/no clobber, tampered
  receipt rejection, external receipt-pin enforcement, distinct
  route/host/executable failures, mandatory receipt-before-deployment, and
  deployment mutation-state separation.
- Independent QA commit `96bcff7` was overlaid on API commit `1246727` in an
  isolated temporary worktree. `python3 -m unittest
  tests.truenas_release_preflight_test` passed 10/10 tests. The QA commit alone
  intentionally lacks the API worker's script and is not claimed as a
  standalone runnable integration branch.
- `git diff --check b1b14d70..1246727` passed.

The implementation uses the exact v1 names `version`, `requestId`,
`targetHost`, `targetExecutable`, and `backupDestination`. It spells changed
input under one retry identity `request-conflict`, and receipt-consumer failures
`verification-failed`; the normative contract records those machine-facing
names. The handler may open the already host- and path-verified source read-only
to capture integrity/profile evidence before it crosses the first write
boundary. No destination SQLite open or filesystem creation occurs before that
boundary.

This review did not deploy, contact TrueNAS, use live data, or create actual
remote backup evidence. Qualified handler execution must still retain the exact
remote receipt and its separately delivered byte pin, and lead/QA must
separately record release acceptance. API and independent QA both cover the
external-pin gate. Synthetic passing checks establish the candidate's behavior,
not permanent prevention in the loaded production path.

### Findings routed during implementation

The following findings were resolved in the reviewed candidate:

- #4531 required an explicit target-side executable, operation/checks, approved
  backup root, safe SSH destination syntax, complete UID/GID/profile evidence,
  exhaustive mutation state, and stable request-ID binding. `b1d3285` provides
  the exact plan schema, fixed SSH argv, canonical evidence, and central binding.
- #4543 required failures after a completed backup to retain that fact and
  required the deployment helper to keep executable mode. Deployment failures
  retain `backupAlreadyVerified`/`backupMutationCompleted`; upload applies mode
  0755.
- #4558/#4560 required the exact established `truenas` route and no-clobber
  publication under a competing-destination race. The deployment consumer
  requires the exact `truenas-ssh` route, and link-based exclusive publication
  is covered by both API and independent QA race tests.
- #4588 accepted those findings and added the handler-producer/deployment-
  consumer ownership split required by lead #4526. Only the handler command
  imports/opens SQLite; deployment requires `--plan` plus
  `--preflight-receipt` and validates rather than recreates the backup.
- #4618 required receipt validation to check source as well as backup evidence,
  actual host/executable identity, mutation/completion state, and artifact size.
  Follow-ups `a952a7a`, `49d60f9`, and `4121796` enforce each field, canonical
  executable equality, and an immutable-evidence digest, with tampered-receipt
  regression coverage.
- #4622 required in-flight deployment failure to distinguish a guard rejection
  from unknown or actual mutation after a mutating remote command was dispatched.
  The API regression distinguishes guard `not-started`, transport `unknown`, and
  remote-command `started`, retaining the last completed stage in each case.
- #4679 required a consumer trust anchor outside the self-described receipt.
  `1246727` makes the handler output the exact saved receipt-bytes SHA-256,
  requires deployment to receive it through
  `--preflight-receipt-sha256`, validates it before parsing or remote action,
  and retains it in deployment success. Its regression changes the receipt and
  recomputes the internal evidence digest; the original external pin still
  rejects the changed bytes before a remote probe.

No design finding above is waived. Remaining dependencies are operational:
handler-owned execution and saved remote evidence/pin, integration of the
reviewed commits into the release candidate, independent release acceptance,
and proof that the loaded production path uses the gate.
