# Agent registration context admission

Work item `wi_649b1999c31d8fb9` revision 2, bounded work order #1240.
The owner report is #1239; lead approved the concrete API-client scope refinement
in #1247, recorded to the database handler in #1252.

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

## Release scope

Release is pending lead candidate acceptance. The approved release updates the
existing TrueNAS hub through middleware and its existing TCP listener, and
installs matching rebuilt `tt` binaries on Mini and Air with atomic prior-binary
rollback copies. It does not restart relays or change the frontend, Tailscale,
networking, owner sessions, live work-item data or retained launch contexts.
