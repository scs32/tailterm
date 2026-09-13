# 512 KiB complete-context and bounded-transport candidate — September 13, 2026

Bug `wi_dd57670ee65d974c` revision 3, executable order **#3622**, and exact
saved Start **#3637** (`que_3d80efd5329575c4` cycle 1/revision 9,
`qrr_e0ac2e22b18fb420` / event 244) authorize this bounded correction. The
builder is `agt_1a2c77ff68a8b01d` / `run_356b3eba006407fb`, context digest
`1a96f46d9e504c63794a0d27c8e1a9d9aa5ac9b396c1524291de7d169ff7934f`.
Work is isolated on `bug/context-bound-api-r2-mini` from accepted baseline
`2dadf19abe84e5957f663f782195413ae217616f`; root and other worktrees remain
unchanged.

The same-item transport supplement **#3662**, delivered in **#3665**, addresses
the narrower Air SSH-exec boundary without changing SSH or host configuration.
When the generated command would exceed 64 KiB, the browser uses its existing
authenticated SFTP session to publish the exact bytes through 32 KiB chunks and
an atomic mode-0600 temporary file. The destination is bound to the agent and
SHA-256 digest. The resulting small SSH command rejects a symlink, wrong mode,
wrong length or wrong digest before calling `tt --work-context-file`; both the
remote trap and browser cleanup cover success, failure and uncertain-response
retry. Smaller contexts retain the existing private base64/file path.

The candidate raises the finite complete serialized-context limit to **524,288
UTF-8 bytes (512 KiB)**. It changes only the shared JavaScript bound and text,
the launch-journal error for its already-derived **1.25 MiB** per-plan bound,
the matching Go API bound and derived **3,162,112-byte** registration envelope,
and the private CLI command-file ceiling to **2,359,296 bytes** (`4 * context +
256 KiB`). The unresolved-journal aggregate remains 2 MiB. Context schema v1,
exact source bytes/digests, current item/run/order bindings, retry identities,
private-file cleanup and all unrelated limits remain unchanged.

Synthetic verification passes the measured 269,315-byte case, exact 524,288
bytes and +1 rejection with three revisions, two or more explicit messages,
ASCII/CJK/HTML/quote/backslash content, exact journal reload/aggregate failure,
HTTP/store readback and digest, wrong-run and duplicate rejection, committed
allocation-intent response loss and unchanged retry, old raw-digest
immutability, command bound/+1 and success/failure cleanup. Focused JavaScript
passes 17/17; focused Go server/store/CLI/spawn passes. Chromium and WebKit pass
the isolated real-hub/private-tmux launch flow with a revision-3 context above
269,315 bytes, encrypted reload, lost worker response, stable identity, exact
readback and the partial-folder-only amendment. Mini passes the original
699,562-byte browser-shaped shell command and the new 2,640-byte staged command.
Actual Air's SFTP protocol plus the staged shell command passes exact
524,288-byte CJK context, mode, byte and digest verification, candidate CLI
exact-bound admission, +1 rejection and cleanup. This clears the initial host
transport finding reported in **#3656/#3657**: Air still rejects the old
699,205-byte one-exec shape, while the bounded staged shape stays below 64 KiB.
The browser WASM/SFTP semantics pass in isolated Chromium and WebKit, but a
physical browser-to-Air run is not claimed and remains a release qualification.

This remains an unreleased candidate pending independent review, handler-saved
acceptance and the explicitly coordinated rollout. The compatibility order is
hub → qualified Mini/Air CLI → frontend, with the updated frontend retained
while oversized journals exist. Old components continue to fail closed at their
own limits; no identity or journal may be regenerated or truncated. Stage-A
preparation **#3671** performed no activation. Production remains on the
deployed 256 KiB release documented below. No root integration, installation,
deployment, live record/profile/provider fixture, relay, lifecycle or production
service change is part of this candidate.

---

# Combined Capacity/context release — September 12, 2026

The reviewed combined application **`d64615bac01f72673c921bcfc25a541d073c2f01`**
is deployed and verified; saved handler acceptance remains pending. Bug
`wi_7e220de54deaef33` revision 1/order **#3022**, Start **#3041** /
`qrr_751e17bce76f70c3`, and Capacity dependency `wi_84dafce5ad044acd` revision
1/order **#2050** retain their original provenance. Execution order **#3195**
was saved as **#3196** after reviewed package and actual Mini/Air qualification.
Root `tasks-hub` was fast-forwarded from `8a37919` to report tip `7708883`.
All shipped bytes remain the frozen application build; no rebuild from the report
commit occurred. Later sections preserve the earlier preparation and historical
release record rather than describing current pending work.

The handler's Stage1 **#3221** gate verifies the exact original agent/run/context/
Queue/Start binding, consistent online backup, integrity and stable profile hashes.
The mode-0600 UID950 backup is
`/mnt/deepfreeze/tailterm-hub/backups/before-context-limit-release-20260912T112000Z.sqlite`,
26,210,304 bytes, SHA-256
`a09023a679d616aa37edfaa81d824c41c74bca4ea07390e88bf0cada9dda2d06`.
The builder did not access the database. Handler post-migration **#3227/#3228**,
corrected artifact metadata **#3235**, confirms integrity `ok`, zero foreign-key
violations, identical `profile_meta`/`profiles`/`profile_history` hashes and counts,
and `allocationIntent.supported=true`, versions `[1]`. The exact 716-byte gate
artifact SHA-256 is
`f5fb7fe28f0d59307ef891cc56cfb950c07680cf6f6720869b144931758a2e85`.

The single running TrueNAS hub now mounts
`/mnt/deepfreeze/tailterm-hub/releases/20260912-context-capacity-d64615b/tailterm-hub`,
27,295,906 bytes, SHA-256
`801f93172221821f1f0788698724d55b92617e4e0eade9667d457643841c58b7`.
Middleware changed only that executable mount; image, UID/GID950, resources,
security, cap32, state/token mounts and private `100.116.238.37:18765` listener
were preserved. The previous immutable `20260911-lead-recovery-33284bd` hub
remains at SHA-256
`dbb0abd3665238d5ffb4e4382f7b3e382345b640db0402d27be370c65583be73`.

Mini and Air both atomically installed `/Users/stephenspeicher/.local/bin/tt`,
mode0755, 6,731,794 bytes, SHA-256
`908034fcd72a13d65d70750fa46010e4eb3f68fda89c65e1312dcb1b38dc7b91`.
Each retains `tt-before-context-capacity-d64615b`, previous SHA-256
`8fb4b792d2945126e90794f8ce0e597ee12fa528599de349cb191d2ba3035ddb`.
Air's earlier command-not-found was noninteractive SSH PATH behavior: its absolute
regular CLI file existed. No absence fallback or original fixture edit was needed.
Actual Air is arm64/Darwin25.6.0 with `kern.argmax=1048576`. Installed-host
qualification uses a separate one-line fixture variant selecting the installed
absolute CLI path; the original frozen qualifier is retained unchanged.
Private staging was removed and its absence verified on Air. Existing relays were
not restarted; their mapped prior executable code may remain until separately
managed. Newly executed CLI commands use the verified installed candidate.

Cloudflare Pages project **tailos** production deployment
**`f8462e21-9547-44d4-90dc-e9721d92a72a`** serves the exact package at
<https://f8462e21.tailos.pages.dev> and <https://tailos.tailarr.com>.
Mini serves identical bytes at <http://127.0.0.1:4318>. Release manifest SHA-256:
`c8583c9f99363432fb63f4f405a757ab201173a4380f493daa6c21ae75ba3ae9`.
All **81 public assets per origin** plus manifest match; all82 local inventory
entries match. Mini used verified copy-on-write staging and atomic directory
exchange, preserving **PID60799/PPID1**. Immediately prior Flashes bytes remain at
`.build/releases/context-capacity-previous-mini-d64615b`, manifest
`dba8628f67bbe9da003dfad11588566d060c22c1b67a182d30aa736d729fbd02`.

Release verification passes actual external256KiB apostrophe/CJK argv, private
script cleanup, UTF-8 boundaries, restored digest/full bytes, committed-response
loss and unchanged intent retry/readback, legacy-intent rejection, and actual
installed Mini/Air CLI oversized file/inline rejection before hub contact.
Only isolated synthetic HTTP/SQLite/host fixtures were used. Fresh Chromium
checks pass on all three origins for production WASM startup, synthetic local
vault key restoration and layout; no signed-in live profile or provider was used.
Pre-release merged JS178, GoAPI/server/store/spawn, focused sanitized CLI race/vet,
and Chromium/WebKit large immutable-journal retry checks remain the source evidence.
Native Safari, owner-device, real-provider and Linux launch-host qualification are
not claimed. Root owner Screenshot and all other worktrees are preserved.
Fresh-launch coordination opened **#3224** and closed **#3236** after activation
verification; no live tasks were closed/retired and no relay/network/Tailscale
configuration changed.

The initial explicit `node_modules/.bin/wrangler` path was absent and performed
no publication. Existing cached `npx --no-install wrangler` version4.131.1 then
published successfully without installation or authentication retries. The handler
corrected its post-migration artifact size from882 to716 bytes in #3235; actual
bytes/hash and contents were verified. These tool/report corrections did not
change the application package.

Full release receipt:
`/tmp/tailterm-context-release-3196/release-receipt.json`, 13,775 bytes,
SHA-256 `db5f8dbd32d138082f1191d755b2641121252822f7bf300706f85a4f6feef5da`.
It binds the source/order/Start/execution gates, exact deployment/native/static
identities, per-host backups, stable-profile evidence, all release logs/screenshots,
and retained command scripts. Review links include hub results **#3225/#3226**,
post-migration gate **#3228**, lead Stage3 approval **#3230**, and verification
announcement **#3236**. Saved revision-checked release acceptance and report
integration remain handler/lead follow-up; this report does not declare either
work item complete.

Retained old binaries and Flashes are recovery inventory. After new allocation
intents or >128KiB immutable plans exist, an older hub/CLI/frontend is not a complete
compatible rollback. Prefer a compatible corrective build. Any rollback needs
handler compatibility assessment and a separately recorded recovery decision;
never truncate/rewrite plans, rewrite existing intent tuples or restore an old
SQLite snapshot over newer work. Keep profile-v2 identity and write fences.

---

# Combined Capacity/context integration — prepared September 12

Bug `wi_7e220de54deaef33` revision 1, original order **#3022**, preserved
Start #3041 / `qrr_751e17bce76f70c3`; integration amendment **#3126**, context
transport correction **#3134/#3138**. Worker/run/context binding below remains
unchanged. The complete dependency handoff #3127 was read and verified at
SHA-256 `ffaae7e402e6db0c04fbfc0e149ddad115c9da7777db95ffdda75cb9ed01e0ae`.

Accepted Capacity base `a8e78594ec01e060c8196859e75345bc5e2c124e`
(Bug `wi_84dafce5ad044acd` revision 1/order #2050, acceptance #3124) plus
accepted context delta `bb12a7ad2e8cf3cb3d15475e5190433182474310` was applied
without conflicts as `acfe537` on isolated branch `context-limit-integration-mini`.
The final **application/source commit is
`d64615bac01f72673c921bcfc25a541d073c2f01`**. Lead source review #3146 passed.
This report is later documentation; it does not rebuild or change that package.

## Merged boundary correction and verification

The requested merged regression reproduced a real boundary mismatch: Capacity's
CLI authored a digest of pretty-printed input, but AddAgent's RawMessage transport
compacted it. Small and >152,973-byte whitespace/HTML/multibyte fixtures both
failed digest parity before the correction. Under #3134/#3138, only new
allocation-intent context handling in `hub/cmd/tt/main.go` and
`hub/cmd/tt/work_context.go` now uses the shared bounded UTF-8/JSON reader and
lossless `json.Compact` before hashing. Keys, numeric and escape lexemes, strings
and full history survive; the raw frozen source/journal is not rewritten.

Actual CLI intent-create → real isolated hub → AddAgent → context readback now
passes. The fixture deliberately commits an intent and drops its HTTP reply;
the unchanged keyed retry succeeds, including later retry after consumption.
Readback proves the original expected run, intended/actual launcher, classification,
context digest and bytes. A repeated admission retains its existing 409 behavior,
and exact agent readback/roster proves there is still one worker. Existing
raw-digest pretty intents are unchanged and fail safely rather than being
rewritten; authoring those old tuples requires separate authorized recovery or
fresh preparation. File and inline inputs both reject oversized contexts before
hub contact. No Capacity policy, tuple schema, retirement, audit format or quota
behavior was changed.

Evidence on the merged source:

- JavaScript **178/178**; full Go API/server/store/spawn packages pass.
- Sanitized focused CLI **race** checks cover the new actual authoring/retry/
  readback path, legacy-intent immutability and bounds, plus Capacity's full-tuple
  generated guidance and supported/unsupported/incompatible capability versions.
- Chromium and WebKit real isolated-hub tests pass the large-context committed
  launch response loss, exact identity recovery, encrypted journal reload,
  unchanged history on partial retry, full 23-message source and readback.
- Mini Darwin arm64 passes actual **external synthetic executable argv** delivery
  at 256 KiB for apostrophe-heavy and multibyte input, as well as private script
  cleanup. This exercises the OS exec boundary, not merely a helper hash.
- Go vet, diff checks, clean source and exact static package verification pass.
  No unrelated full CLI suite or provider test was substituted for focused checks.

Two fixture assumptions were corrected without product changes: ordinary
AddAgent requests do not use the handler-restart `ExpectedRunID` field (Capacity's
saved intent supplies the worker run), and duplicate same-project admission
returns 409 rather than a success replay. An initial unsanitized CLI race run
inherited the current agent's Codex model/reasoning settings and stopped before
capability checks; clearing all `TAILTERM_*` fixture environment made the unchanged
checks pass. The genuine pre-fix digest failure remains in the retained evidence.

## Frozen artifacts

Directory, relative to the project root:
`.build/worktrees/context-limit-integration-mini/.build/context-limit-integration-artifacts/`.
Its `manifest.json` lists the **44 combined changed source paths**, **21 context
delta paths**, exact binary/static hashes and test logs. It is 11,838 bytes,
SHA-256 `7e7fe52c33458009205ddeaa9cb9768f6d4ed765f3d61a09a4190db04e622207`.

| Artifact                   |      Bytes | SHA-256                                                            |
| -------------------------- | ---------: | ------------------------------------------------------------------ |
| `tailterm-hub-linux-amd64` | 27,295,906 | `801f93172221821f1f0788698724d55b92617e4e0eade9667d457643841c58b7` |
| `tt-darwin-arm64`          |  6,731,794 | `908034fcd72a13d65d70750fa46010e4eb3f68fda89c65e1312dcb1b38dc7b91` |
| `dist-static/release.json` |     14,861 | `c8583c9f99363432fb63f4f405a757ab201173a4380f493daa6c21ae75ba3ae9` |

The retained static package verifies **82 assets** against its clean-source
release manifest. It reuses the unchanged cached WASM and dependency/license
inputs; speech source/part hashes are verified by the packager. No test WASM or
provider/live fixture is part of the package.

Native builds initially received the outer repository's incorrect VCS stamp
(`8a37919`, modified=true) from the nested worktree. Those generated binaries
were replaced before handoff. The final binaries were built from the clean,
standalone local clone `/tmp/tailterm-context-release-d64615b`; both report exact
`vcs.revision=d64615bac01f72673c921bcfc25a541d073c2f01` and
`vcs.modified=false`. The rejected hashes and final metadata are retained in the
artifact manifest/evidence. No installed executable was changed.

## Coordinated rollout and rollback plan — not executed

1. Obtain a separately recorded release order and saved combined acceptance.
   Qualify the actual Air launch host with synthetic argv/CLI fixtures; **Air is
   still unverified**. Mini qualification does not qualify Air, Linux launch
   hosts, real provider CLIs, native Safari or owner devices.
2. Schedule a coordinated hub + all launch-capable CLI + frontend window.
   Fresh parented item-bound launches fail closed while a side lags; retain
   saved plans and exact identities, and never relabel or discard them to make
   a mixed version launch succeed. Existing sessions are not regenerated.
3. Through the database handler, obtain a verified consistent online SQLite
   backup before the additive Capacity migrations; preserve state, token and
   profile identity together. No database copy or migration was performed here.
4. Use the existing TrueNAS middleware/TCP deployment path for the exact Linux
   hub binary, with a unique immutable release directory. Atomically install the
   matching Darwin CLI on each qualified host, retaining exact previous binaries.
   No Tailscale, listener or relay redesign is required or authorized by this plan.
5. Publish the exact retained static package to Cloudflare project **tailos** /
   **https://tailos.tailarr.com**, and atomically activate identical bytes on
   Mini **http://127.0.0.1:4318**, preserving **PID60799/PPID1**. Do not use the
   old-site `npm run deploy:static`; the handoff's explicit TailOS command applies.
6. Verify deployed manifests, binaries, capability versions, additive data
   integrity and synthetic supported-host launch behavior, then have the handler
   save release evidence and acceptance. No live work-item test fixture is needed.

Rollback must preserve all additive Capacity allocation intent/binding data and
full context journals. Keep compatible hub/CLI enforcement while new intents or
large journals exist; an old hub/CLI pair cannot enforce the new allocation
contract and old frontends cannot restore larger journals. Prefer a compatible
corrective build. A coordinated rollback requires a separately approved launch
pause/recovery plan and exact previously inventoried binaries/assets; it must
not silently restore weaker admission. Database backups are disaster recovery
only and must not overwrite newer work as a routine code rollback. The existing
Flashes frontend rollback inventory remains in `docs/handoff.md`; no new rollback
activation happened here.

Remaining dependencies: independent package review, handler saved acceptance,
Air qualification, and an explicit root integration/release order. No root merge,
installation, deployment, live record, network/relay/Tailscale/service or preview
mutation occurred. The original builder evidence and historical fix follow.

---

# Complete immutable context admission — September 12 candidate

Bug `wi_7e220de54deaef33` revision 1, bounded order **#3022**, package #3025.
Exact worker `agt_56ec514b7b5e8586` / `run_ddbb3bffb6046750`, admitted context
through #3030, digest `e865e1ea1ffa559ebd0b525d590052c4c2f2382f97e8dcb6774b7b11bc328a4c`.
Handler #3041 verified binding and separate Queue Start
`qrr_751e17bce76f70c3` (entry `que_9232aa3354b8e3b9`, cycle 1/revision 3).
Scope additions: #3049, #3053/#3055 corrected by #3061/#3064, and #3078.
Base: `tasks-hub` `8a37919fc693033598460b287d9fa20b04f7565f`, isolated branch
`fix/context-limit-mini`, `.build/worktrees/context-limit-mini`.

## Supported bound and transport

The handler measured the complete Monitor context at **152,973 bytes**, above
131,072. Only that reported size was used here; no live diagnostic context was
opened or copied into tests. The new bound is **262,144 UTF-8 bytes (256 KiB)**,
the smallest power-of-two increase that admits the measured input and provides
109,171 bytes (71.4%) of history growth. A 192 KiB bound would leave only 28.5%.
This remains a finite admission bound; over-limit histories fail explicitly and
are never truncated, summarized, or replaced with stale context.

The complete serialized bundle is counted before preparation, journal storage,
browser shell transport, CLI file/inline admission and store admission. Go keeps
one API constant; JavaScript shares `shared/work-context.js`. The registration
HTTP envelope remains derived: `6 * 262144 + 16384 = 1589248` bytes; unrelated
requests retain 64 KiB. The per-plan journal limit is **768 KiB**: up to 512 KiB
for the JSON-string-escaped shared context plus 256 KiB for member metadata.
Eight-plan count and **2 MiB aggregate** remain unchanged; aggregate exhaustion
rejects the save before launch. One shared context is retained per team.

Browser launches encode the exact JSON as base64, decode into a mode-0600
private temporary file, and use existing `--work-context-file`. Success, failure
and ordinary termination clean up through shell traps. This avoids nested shell
quote amplification: an exact 256 KiB apostrophe fixture previously failed with
`E2BIG`. Host runtime commands execute from a private, self-removing script rather
than a large shell `-c` argument, preserving terminal stdin. The existing command
file validation bound becomes `4 * 256 KiB + 64 KiB` to cover POSIX quoting and
briefing metadata. Failed script starts clean up too. Hard host termination can
still leave a private temporary file; no background cleanup service is installed.

Admission digests continue to cover the exact **admitted** JSON bytes. The Go
registration encoder compacts RawMessage JSON without HTML escaping. Browser
retry hashing performs the same lossless lexical compaction, preserving numeric
lexemes, key order and existing escape spelling; it also recognizes the exact
historical HTML-safe encoding for old uncertain plans. Neither accepted codec
rewrites saved plans, bindings, agent identities or run IDs. Arbitrary changed
content, revision, order, run or digest still fails reconciliation.

Context HTTP readback and `tt context --json` now emit the stored bundle verbatim,
including internal whitespace and HTML-sensitive characters. The host verifies
its size, UTF-8 JSON and immutable digest before adding it to the launch prompt.
A digest mismatch stops restoration; no alternate context is substituted.

## Component versions and rollback

This is a coordinated frontend/hub/host-CLI update, with no database migration,
new context schema or rewritten launch journal. Publish/install only under a
separate release order; this builder has **not deployed or installed** anything.

| Combination                                 | Behavior                                                                                                                                                                                                                                |
| ------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Updated frontend, hub and host              | Supports complete 256 KiB admission and exact context readback; saved identities survive uncertain retries.                                                                                                                             |
| Old frontend with updated backend           | Old preparer/journal still reject contexts above 128 KiB. Existing smaller plans remain usable.                                                                                                                                         |
| Updated frontend/CLI with old hub           | Old hub rejects the larger context with its native context-limit error before admitting an identity. Older response serialization can also fail the new host's digest check for HTML-sensitive/whitespace input. Upgrade the hub first. |
| Updated frontend/hub with old host CLI      | Old host command limits and quoting/readback behavior cannot guarantee all newly supported payloads. Upgrade each launch host before enabling larger launches; a frontend update does not update hosts.                                 |
| Updated frontend reading an old frozen plan | Preserves its full string and member identity; recognizes current and historical exact digest encodings. Does not rebuild source history on retry.                                                                                      |
| Old frontend reading a new large plan       | Rejects unsupported context/plan size. Retain the updated frontend while these unresolved journals exist; do not discard them to enable rollback.                                                                                       |

Old binaries can read retained additive data, but rolling back the hub restores
its old admission limit; rolling back hosts reintroduces their command limits.
Retain current components while larger launches/journals need recovery. Existing
sessions and frozen briefings are not regenerated by this source change.

Host runtime argument limits remain separate from the JSON transport contract.
Tests ran on Darwin arm64 with observed `kern.argmax=1048576`; real provider CLIs,
Linux launch hosts, native Safari and owner devices were not exercised. No
universal provider/platform prompt-size guarantee is made. A release must verify
the actual supported launch hosts, including runtime argument limits; this item
does not change provider adapters or their input mechanisms.

## Verification and remaining dependencies

All records, messages, contexts, databases, browser vaults and tmux sockets in
these checks are synthetic and isolated.

- 178 JavaScript unit tests pass. Focused tests cover exact measured-size and
  256 KiB UTF-8 boundaries, limit+1 rejection, full source preservation, shell
  quote/HTML/backslash payloads, private file cleanup, journal escape headroom,
  aggregate rejection, frozen uncertain identity and historical/current digests.
- Full Go API/server/store/spawn package tests pass. HTTP checks include exact
  byte/digest readback (including direct HTTP whitespace), unrelated 64 KiB
  rejection, expanded envelope rejection, wrong-run rejection and duplicate
  admission without a second identity. CLI checks cover file/inline parity,
  malformed UTF-8/JSON, restored digest changes and bounded command files.
- Chromium and WebKit real isolated-hub tests pass with 23 complete linked
  messages and context above 152,973 bytes through browser preparation, encrypted
  journal/reload, host launch and API readback. Partial retries preserve the
  frozen full context and only amend the verified-unstarted member's folder.
  The same large-context path loses a committed worker reply, then reconciles
  the exact identity and unchanged full context without a second launch or
  history rebuild.
- Focused Go context/host race tests and Go vet pass. Vite static compilation
  passes using a local copy of the existing ignored WASM artifact; the initial
  clean-worktree attempt failed because that artifact was absent. No WASM rebuild,
  static release packaging or deployed-asset verification is claimed.
- An isolated copy of the actual base hub rejects a synthetic 152,973-byte
  context with its native 409 and leaves only the fixture lead registered.
  Actual base journal code rejects larger context; new journal code preserves
  a smaller old uncertain plan byte-for-byte.

Lead review, any authorized integration/release and handler saved acceptance
remain separate dependencies. This report does not declare the item complete.
No root checkout edit, deployment, live work-item access, preview PID60799 change,
other worktree change, helper spawn, quota change, relay or networking operation
was performed. The historical earlier 64 KiB envelope fix below is preserved.

---

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
