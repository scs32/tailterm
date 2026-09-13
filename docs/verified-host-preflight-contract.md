# Verified-host backup and release preflight contract

Status: operator contract for Bug `wi_9cc74828bb3499a4` revision 1, bounded work
order #4410 (original owner source #4328; owner prevention disposition #4393).
The admitted UI documentation run started under handler confirmation #4486,
Queue `que_b7d1a3a355cd5b8b`, receipt `qrr_92911eb8ecd368b2`, event 257.

This contract prevents a command intended for TrueNAS from interpreting the
Mini's local filesystem as the remote system. It governs the existing
handler-owned SQLite backup gate and the release/deployment path that consumes
its receipt. It does not authorize a deployment, a live fixture, a database
operation by anyone other than the handler, or any networking or Tailscale
change.

## Incident and control boundary

The same failure family occurred at least three times:

- #3731 treated an unavailable local `/mnt/deepfreeze/...` path as a release
  blocker; the established remote route then completed the gate in #3743.
- #4077 again checked the remote paths in the local workspace; #4080 corrected
  the route and #4085 completed the remote gate.
- #4320 ran `mkdir` locally on the Mini and reported local `errno 30` as though
  it described TrueNAS; #4322 identified the routing error and #4327 completed
  the remote gate.

The control must therefore be executable and fail closed. Operator reminders or
the mere presence of `truenas` in a command are not host verification.

The protected mutation boundary includes all operation-controlled filesystem,
SQLite writes, and deployment changes. Before the boundary, the implementation
may validate input and perform read-only route, host, executable, filesystem
metadata, and source-database integrity/profile probes. It must not:

- create a backup directory or file;
- open the destination with SQLite or open the source other than read-only,
  after host/executable/source-path verification;
- create or update a hub token;
- create a release directory, upload or stage a binary;
- invoke `app.create`, `app.update`, `app.start`, or another middleware mutation;
- fall back to inspecting the same absolute path on the local machine.

The implementation records that it crossed the boundary before its first
mutation. A rejected preflight reports `mutationStarted: false`.
If the transport loses the structured response after dispatch, the caller must
report mutation state as `unknown`; it cannot safely infer `false` from an absent
receipt.

## Handler and deployment ownership

The control has two separately executable stages with one exact plan identity:

1. The database handler runs the verified-host backup operation. Only this stage
   opens the source or backup with SQLite, creates the backup, performs
   integrity/foreign-key/profile checks, and publishes the immutable receipt.
2. The lead/operator deployment path validates the exact handler receipt against
   the frozen plan and re-verifies the actual execution host before any token,
   release, upload, or middleware mutation. It does not open the source or
   backup as a database and does not create a replacement backup.

The deployment stage cannot run the handler stage implicitly. A command named
"preflight only" must not hide a SQLite backup under lead/operator ownership.
Missing, invalid, mismatched, or unverified handler evidence blocks deployment
without mutation and is reported as a receipt-validation error.

## Structured request

The CLI, function, or JSON surface may vary, but it must construct and validate
one request containing every field below. No field may be recovered from the
caller's current working directory or silently defaulted to a local operation.

| Field | Contract |
| --- | --- |
| `version` | Required version discriminator. Candidate v1 requires integer `1`; unknown versions fail closed. |
| `operation` | Required operation kind, for example `sqlite-online-backup`. |
| `requestId` | Stable retry identity for one exact logical attempt. It binds all canonical request fields. |
| `databaseOwner` | Must name the project handler, `db-handler`; the deployment consumer cannot substitute itself. |
| `targetHost` | Canonical approved execution-host identity. An SSH alias or display label alone is not proof. |
| `route` | Explicit established transport and endpoint. For the current installation this is the existing non-interactive SSH route to alias `truenas`, using argument boundaries and the approved timeout. |
| `sourceDatabase` | Canonical absolute remote path. The current hub source is `/mnt/deepfreeze/tailterm-hub/state/hub.sqlite`. |
| `backupDestination` / `allowedBackupRoot` | Explicit absolute remote backup file and its separately approved root. V1 requires the file directly beneath the root and a bounded safe `.sqlite` name. |
| `targetExecutable` | Explicit absolute target-side executable identity used for the probe and backup operation. Resolution occurs on the verified target, never from local `PATH`. |
| `checks` / `profileTables` | Exact ordered evidence requirements and the exact profile tables whose canonical snapshots must match. |
| `backupOwner` | Exact numeric UID/GID required on the published backup. |
| `deployment` | Exact release, binary, state, token, app, and TCP-listener linkage consumed later by the deployment stage. |

The route is data, not a free-form shell fragment. Host, paths, executable, and
retry identity must retain argument boundaries through transport. Remote script
input should use stdin or another non-interpolated encoding so a path cannot add
shell syntax.

`targetHost` is independent of `route`: the route says how to reach a target;
the host identity proves which target answered. If the installation accepts
aliases, the approved alias-to-canonical mapping must be explicit and the result
must retain both the configured identity and observed identity.
The expected value comes from the reviewed release order or a retained verified
host inventory. A caller must not populate it by probing the route immediately
before the comparison; doing so would make every answering host self-approve.

## Preflight state machine

| State | Required evidence | Allowed next state | Mutation allowed? |
| --- | --- | --- | --- |
| `received` | Complete raw request is available. | `request-validated` or `rejected` | No |
| `request-validated` | Schema, operation, retry binding, route shape, absolute paths, destination root, and executable shape are valid. | `route-reached` or `rejected` | No |
| `route-reached` | The declared route returned a read-only probe response. A local subprocess exit alone is not host proof. | `host-verified` or `rejected` | No |
| `host-verified` | Observed canonical host identity exactly matches the approved expected identity or explicit approved alias mapping. | `executable-verified` or `rejected` | No |
| `executable-verified` | The requested executable resolved and identified itself on that verified host. | `source-verified` or `rejected` | No |
| `source-verified` | On the verified host, the configured source is the canonical expected absolute path and is a readable regular file. This check is filesystem metadata only; SQLite is not opened yet. | `source-content-verified` or `rejected` | No |
| `source-content-verified` | The handler opened the verified source read-only and retained integrity, foreign-key, and canonical profile evidence. | `mutation-started` or `rejected` | No |
| `mutation-started` | The result state is updated immediately before the first destination, SQLite, staging, token, or middleware mutation. | `backup-created`, `failed` | Yes |
| `backup-created` | SQLite's online backup operation completed at the exact destination without overwriting unrelated data. | `verified` or `failed` | Yes |
| `verified` | Backup identity and all requested integrity/profile evidence were produced on the verified host. | terminal success | No further implicit work |
| `rejected` / `failed` | A precise error result and the last verified state are available. | terminal failure, or an explicit same-request retry | No fallback |

Route and identity checks must run in the same remote execution context that
will perform the backup. Probing TrueNAS and then running the operation locally
does not satisfy the contract.

The deployment receipt consumer has a separate state sequence:

| State | Required evidence | Deployment mutation allowed? |
| --- | --- | --- |
| `deployment-received` | Exact plan, handler receipt, release name, and local artifact reference are available. | No |
| `receipt-validated` | Receipt is successful and matches the full canonical plan/retry digest, expected and actual host, executable, source/destination, checks, profile evidence, mode/owner/size/SHA, and completion state. | No |
| `artifact-validated` | The exact local release artifact is present and readable. | No |
| `identity-reverified` | A fresh read-only probe through the same approved route matches the receipt's host and target executable. | No |
| `deployment-mutation-started` | State changes immediately before dispatching the first token/release/upload/middleware command. | Yes |
| `token-ready` | Existing nonempty token was preserved or authorized token creation succeeded. | Yes |
| `release-staged` | Unique release directory and exact binary were safely staged and verified. | Yes |
| `app-updated` | The intended middleware application accepted the exact configuration. | Yes |
| `app-running` | Post-update process, listener, binary, and readiness checks pass. | No further implicit work |

Each deployment failure retains the last completed state, the current in-flight
stage, `backupAlreadyVerified: true`, and a separate deployment mutation state.
A host/executable guard rejection before the first command remains no mutation;
lost transport after dispatching a mutating command is `unknown` unless exact
evidence establishes whether that command ran.

## Retry and destination behavior

- The same `requestId` with the same canonical host, route, source, destination,
  executable, operation, and checks represents the same logical attempt. A
  retry returns an existing verified result or continues only through a proven
  safe recovery path.
- The same `requestId` with any different bound field returns `request-conflict`
  before mutation.
- An existing destination is never blindly overwritten. If it can be proven to
  be the completed artifact for this exact request, return `already-satisfied`
  with its verified evidence. Otherwise return `destination-collision`.
- Response loss must not create a second backup under a new implicit identity.
  Operators reuse the frozen request and retry identity.
- A failed preflight never changes route, host, source, destination, executable,
  permissions, networking, or Tailscale configuration automatically.

## Result and error contract

Every terminal result is structured and includes:

- `version`, `operation`, `requestId`, and terminal `status`;
- `phase`, `classification`, and sanitized `message`;
- configured `expectedHost`, observed host when available, and route identity;
- canonical source, destination, and target-side executable identities;
- `mutationStarted` and the last successfully verified state;
- on success, backup path, byte size, SHA-256, mode, UID/GID, integrity result,
  foreign-key violation count, canonical profile hashes/counts, and a digest
  binding the complete immutable receipt evidence;
- on failure after mutation, which mutation occurred and whether cleanup or an
  operator decision remains necessary.

Never include tokens, private keys, profile envelopes, row contents, vault data,
or unrestricted environment/process output. The current canonical profile
comparison is sorted row objects serialized as compact JSON UTF-8 and SHA-256
for `profile_meta`, `profiles`, and `profile_history`; results retain only table
name, row count, serialized byte count, and digest.

Primary error codes and operator meanings are:

| Code | Meaning and required operator statement |
| --- | --- |
| `invalid-input` | The request is incomplete, unknown, unsafe, or non-canonical. No target operation ran. |
| `request-conflict` | The request/retry identity was reused with different canonical input. No backup operation ran; an existing binding is preserved. |
| `request-in-progress` | The exact request identity is currently locked by another execution. Do not issue a new identity merely to bypass the lock. |
| `route-unavailable` | The declared transport could not establish a usable probe because of spawn, DNS, SSH authentication/host-key, timeout, or connection failure. Remote filesystem state is unknown; this is not a remote filesystem error or proof of a host outage. |
| `preflight-result-missing` | The remote command returned no valid structured result. Mutation state is `unknown` unless separate exact evidence proves otherwise; do not retry under a new identity. |
| `host-verification-failed` | The route answered but did not provide a valid identity proof. No path or database operation ran. |
| `host-mismatch` | The observed canonical host differs from `expectedHost`. State both identities and `mutationStarted: false`; do not inspect either path locally. |
| `executable-unavailable` | The requested executable could not be resolved on the verified host. Do not replace it with a local executable. |
| `executable-mismatch` | The resolved target-side executable is not the requested/approved identity. |
| `source-mismatch` | The configured or resolved source is not the canonical approved source on the verified host. |
| `source-unavailable` | The canonical source is absent, unreadable, or not a regular file on the verified host. |
| `destination-mismatch` | The approved backup root is not the canonical directory reached on the verified host. |
| `destination-unavailable` | The approved backup root is absent or inaccessible on the verified host. |
| `destination-collision` | Existing data is not proven to belong to the exact retry request and was preserved. |
| `destination-unwritable` | A destination operation failed after host and source verification. Report the verified host and actual remote error; do not generalize it to a service or network outage. |
| `backup-failed` | SQLite online backup failed after the mutation boundary. Identify the verified host and whether a partial artifact exists. |
| `integrity-failed` | The created backup failed `PRAGMA integrity_check` or foreign-key acceptance. Do not authorize activation. |
| `verification-failed` | Required mode, owner, size, digest, or profile evidence is absent or inconsistent. Do not authorize activation. |
| `verification-failed` during receipt consumption | The receipt is absent, unreadable, unsuccessful, mismatched, or lacks the complete host/executable/source/backup/mutation evidence. It performs no database or deployment operation. |
| `local-artifact-unavailable` | Receipt validation succeeded, but the local release binary is absent or unreadable. No deployment mutation ran; the verified backup remains valid evidence. |
| `remote-operation-failed` | A mutating deployment command reached the verified remote context and failed. Retain its stage and last completed stage. |

`host-mismatch`, `route-unavailable`, and a verified remote destination error are
distinct. In particular, local `errno 30` cannot be presented as a TrueNAS
failure. The preferred operator summaries are:

- `Host mismatch: expected <expected>, observed <actual>. No files or databases were changed.`
- `Route unavailable before host verification. Remote filesystem state was not established; no files or databases were changed.`
- `Destination write failed on verified host <actual> after preflight. See the sanitized remote error and mutation state.`

## Acceptance matrix

| Scenario | Required result | Required negative evidence |
| --- | --- | --- |
| Synthetic route deliberately executes on the wrong host | `host-mismatch`, expected and actual identities, `mutationStarted: false` | No destination directory/file, SQLite open/connect/backup, token/release staging, binary upload, or middleware mutation |
| Route cannot start, authenticate, verify its host key, or respond before timeout | `route-unavailable` | No path inspection on the local fallback host; no remote-outage claim; no mutation |
| Route dispatches but returns no structured result | `preflight-result-missing`, `mutationState: unknown` | No invented success/failure cause and no retry under a new identity |
| Probe output is absent or malformed | `host-verification-failed` | No executable, source, destination, SQLite, or deploy operation |
| Correct host, wrong/unavailable executable | `executable-mismatch` or `executable-unavailable` | No source/destination/SQLite/deploy mutation |
| Correct host/executable, wrong or missing source | `source-mismatch` or `source-unavailable` | No destination creation, SQLite open, or deploy mutation |
| Existing unrelated destination | `destination-collision` | Existing bytes unchanged; no overwrite |
| Same retry ID and exact request after response loss | Recovered terminal result, `already-satisfied`, or one safe continuation | Exactly one logical backup; no duplicate release/deploy action |
| Same retry ID with one changed bound field | `request-conflict` | No new backup operation; bound records and unrelated destinations remain intact |
| Correct isolated synthetic route | Success with complete sanitized evidence | No local `/mnt` access and no unrequested deployment/network change |
| Deployment receives a missing or changed handler receipt | Receipt-consumer `verification-failed` | No SQLite access, replacement backup, token/release staging, upload, or middleware mutation |
| Actual handler-owned TrueNAS gate | Exact remote host/route/source/destination/executable plus backup mode/owner/size/SHA, integrity/FK, and profile evidence retained | No live fixture content, secret output, database restore, networking/Tailscale change, or activation implied by backup success |

The wrong-host regression must instrument every mutating seam and assert zero
calls, not merely assert that no final backup file is visible. The correct-route
test must also prove that the host probe and operation used the same runner.

## Release gate semantics

A successful backup preflight is evidence for one release prerequisite. It is
not deployment authorization, activation evidence, acceptance, work-item Done,
or permission for another agent to access the live database. The database
handler retains the actual backup/integrity/profile evidence; lead and QA review
it under their recorded orders.

The approved operational topology remains the existing TrueNAS middleware and
private TCP listener. This contract creates no alternative route, does not
install or reconfigure Tailscale, and does not restart unrelated services. Normal
rollback keeps the additive current database; restoring a backup is separate,
explicit disaster recovery because it discards later writes.
