# Agent registration context admission

Work item `wi_649b1999c31d8fb9` revision 2, bounded work order #1240.
The prerequisite discovery and failed live launch evidence were reported by lead
in #1239, not directly by the human owner. Lead approved the concrete API-client
scope refinement in #1247, recorded to the database handler in #1252.

## Problem and boundary

Agent registration shared the ordinary 64 KiB HTTP decoder even though an
immutable prepared work-item context may contain up to 128 KiB. A synthetic
66,560-byte context produced a 66,861-byte request and reproduced the pre-change
HTTP 413 before any agent identity was admitted.

Only `POST /v1/tasks/{task}/agents` needs a larger transport envelope. The
decoded `contextBundle` remains independently limited to 128 KiB and continues
through all existing semantic, work-item revision, order-link and replacement
validation. Other endpoints retain the 64 KiB request limit.

The Go API client also previously HTML-escaped an embedded `json.RawMessage`.
That can expand one prepared byte to a six-byte escape before transport and make
a valid pre-transport context exceed its advertised bound. Agent registration
now disables HTML escaping only for this API-client operation. No CLI flag,
invocation, frontend request, schema or stored record shape changes.

The registration-only HTTP cap is
`6 * MaxAgentWorkItemContextBytes + 16 KiB`. Six times covers the JSON encoder's
worst HTML-safe expansion; the fixed allowance exceeds the maximum encoded size
of the other validated registration fields and field names. Requests beyond that
cap remain HTTP 413.

## Isolated verification

All fixtures use temporary SQLite databases and synthetic tasks, messages,
work items and contexts. The retained live narrative and Board context files
were not opened or used.

- Before: focused registration test failed with HTTP 413 at context 66,560 bytes
  and request 66,861 bytes.
- After: the same supported-size registration succeeds and retains exact
  agent/run binding and context digest.
- Exact 131,072-byte contexts succeed with HTML-sensitive `<` text and with
  multibyte Unicode text.
- A 131,073-byte bundle reaches independent store validation and returns HTTP
  409 `work-item context exceeds the launch limit`; an invalid bundle returns
  HTTP 400.
- A registration request beyond the bounded transport envelope returns HTTP 413.
- An unrelated oversized task request still returns HTTP 413 at the ordinary
  64 KiB limit.
- An exact-run mismatch returns HTTP 409. Retrying an already admitted item-bound
  request does not create a second identity or change its run.
- Focused server acceptance, work-context store tests, CLI context formatting,
  `go vet` and the focused server race run pass.

The broad `go test ./cmd/tt -count=1` run is not reported as passing. It hit the
existing `TestPostHumanReplyAndLiteralHelp` unexpected-route failure and then
timed out after ten minutes in `TestAskPreservesIdentityContextAndReplayPayload`
while its test server waited on a fixture channel. The focused CLI context test
passed separately; neither broad failure executes the changed registration path.

## Release

Lead accepted candidate `03129acd724a9064fe67576058ca91cf473b9c78`
in #1256 and assigned the release slot. The root `tasks-hub` branch was
fast-forwarded from `ed12c0d` to that exact application commit. The exact clean
source and builds are retained at
`.build/releases/context-admission-03129ac`.

The hub is RUNNING through TrueNAS middleware and the unchanged private TCP
listener from
`/mnt/deepfreeze/tailterm-hub/releases/20260909-context-admission-03129ac/tailterm-hub`,
SHA-256 `f561f7743454e956e9da3bafedea083682ef08008bb84a7ca99e5210b48fe8ae`.
Unauthenticated/authenticated readiness returns 403/200, an unknown exact context
returns 404, and the live database integrity/FK checks pass.

The pre-update online backup is mode 0600 at
`/mnt/deepfreeze/tailterm-hub/backups/before-context-admission-20260910T005733Z.sqlite`,
SHA-256 `df70fc9bbccd975ae31d2aab01b25709b2012277774c8f3b21394409be7bb8fe`.
The previous routing hub binary remains the normal rollback at SHA-256
`edaec95e25f62ac0c8b5660ca3841803b7c836df265e4a3ed5cfd68fe296f706`;
the database snapshot is disaster-recovery evidence, not the normal rollback.

Mini and Air now have matching Darwin arm64 `tt` SHA-256
`d07329684ef558d61737bcbf81bf7aa61271263190d303f8c008b4ebef6cfe0e`.
Each retains `tt-before-context-admission-03129ac` at prior SHA-256
`909220a58f79592479ff49c7f19722f24da8779592d09c17b4d992a21fd7218b`.
Installed help and doctor checks pass; relay PIDs stayed 83457 and 912 and were
not restarted.

Two pre-release invocations incorrectly treated `--help` as a deployment release
name. Each uploaded the same unused candidate artifact before `app.create`
failed with `EEXIST`; initial reports incorrectly said no file was transferred
and were explicitly corrected in #1264/#1265. The unused file was mode 0755,
26,579,106 bytes, SHA-256
`c36530e4b8966c448348d8761c16919ad39ff47108a7e5fdd0804ef2aa7c253e`.
It carried stale `ed12c0d` VCS build metadata and was never referenced by the
running app. Under lead's exact cleanup authorization #1266, its hash and
non-reference were reverified, then only that file and its empty `--help` parent
were removed without recursion. The successful update used the source-inspected
explicit release name and `--update` path.

No frontend, Tailscale, network, listener, relay, owner session, live work-item
data, retained launch context or task lifecycle changed. The two held contexts
remain launch inputs for lead; they were never opened or used as fixtures here.
