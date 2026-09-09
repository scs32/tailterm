# Development handoff — September 9, 2026

## September 9 Bugs and Features status filtering — release verified pending final acceptance

Application commit `0c1a3ce62301c415ab51d2c0c3438893702622db` is deployed to
`https://tailos.tailarr.com` (`https://ea5b1191.tailos.pages.dev`, deployment
`ea5b1191-c233-43d8-b9bc-ac766f533ee8`) and served by the unchanged Mini
preview PID 28664 at `http://127.0.0.1:4318`. The immutable deployment, custom
domain and Mini match all 81 served files in the retained package and its
release-manifest SHA-256
`e591b6c3d9d5ea58b9aea3a52e77c3b7832439988214dcdd790e2a48902cddea`.
Real Chromium on TailOS and Mini started production WASM, restored an isolated
synthetic vault/key, preserved matching layout margins and reported no page
errors.

Committing a Bugs or Features status choice now ends both the native-select hold
and its initiating pointer hold. A platform picker that consumes the document
`pointerup` can no longer leave the filtered repaint queued until an outside
click. Native `change`, keyboard typeahead, ordinary matching pointer release,
background-refresh continuity and the other Board/Projects consumers remain
intact. The exact owner browser event trace was unavailable, so the consumed
release is a labelled synthetic causal reproduction rather than claimed native
OS popup automation.

Bug `wi_a65c688c4b09f458` revision 3 and bounded order #1165 are documented in
[the implementation report](status-filter.md) and
[the release receipt](releases/tailos-2026-09-09-status-filter.json). Clean-base
consumed-release cases failed 0/2; the final focused Chromium/WebKit cases passed
12/12, integrated shared-consumer continuity passed 50/50, presentation tests
passed 10/10 and the JavaScript suite passed 130/130. The retained clean source
and package are `.build/releases/status-filter-0c1a3ce`. Rollback is the retained
activity-notices package `.build/releases/activity-notices-8ed8c65` and
deployment `https://c15f5470.tailos.pages.dev`. Neither hub, CLI, schema,
Tailscale, TrueNAS, relays, Air preview, the old site, live tasks/profiles nor
agent sessions changed.

## September 9 terminal activity notice filtering — release verified pending final acceptance

Application commit `8ed8c65ef85006bac04b3d212f89b7b66622a25e` is deployed to
`https://tailos.tailarr.com` (`https://c15f5470.tailos.pages.dev`, deployment
`c15f5470-310a-4dba-b87f-8a014c4a1794`) and served by unchanged Mini preview
PID 28664 at `http://127.0.0.1:4318`. The immutable deployment, custom domain
and Mini match the retained package's 81 served files and release-manifest
SHA-256 `8b0760761535c03271a34899481f72795316ff62f88e3cbc8c4c80b8a3b2fb44`.
Real Chromium on TailOS and Mini started production WASM, restored an isolated
synthetic vault/key, preserved matching layout margins and reported no page
errors.

Background terminal activity now uses rendered text plus terminal position.
Fixed-row spinner, progress, clock and counter repaints stay quiet, while text
appended at the cursor, downward or scrolling output, bells, OSC 133 completion
and actionable connection errors retain their existing per-pane and group/tab
ownership. The clean base reproduced the false alert in Chromium; the identical
focused candidate and retained integrated-source runs passed. Focused unit and
reliability checks pass 11/11; the candidate's complete JavaScript suite passed
129/129 before integration.

Bug `wi_539c0062cebde9b8` revision 3 and bounded order #1166 are documented in
[the implementation report](activity-notices.md) and
[the release receipt](releases/tailos-2026-09-09-activity-notices.json). The
clean source/package is retained at `.build/releases/activity-notices-8ed8c65`.
Rollback is the retained Board-scroll package `.build/releases/board-scroll-c8a7552`
and deployment `https://123605a6.tailos.pages.dev`. Neither hub, CLI, schema,
Tailscale, TrueNAS, relays, Air preview, the old site, live tasks/profiles nor
agent sessions changed.

## September 9 Board history scroll stability — release verified pending final acceptance

Application commit `c8a7552b84460b6ba06dcb0561460a109f2bf5e0` is deployed to
`https://tailos.tailarr.com` (`https://123605a6.tailos.pages.dev`, deployment
`123605a6-bf6e-4c57-9271-04cd349eb4af`) and served by the unchanged Mini
preview PID 28664 at `http://127.0.0.1:4318`. All three origins match the
retained package's 81 served files and manifest SHA-256
`c239f7bf341b23cb4266cd4bcdd97b03912a6b5a2b71f8739538a30edb2bb489`.
Real Chromium on TailOS and Mini started production WASM, restored an isolated
synthetic vault/key, preserved matching layout margins and reported no page
errors.

Board refreshes now preserve the first visible immutable message and its exact
intra-row offset instead of reusing an absolute `scrollTop`. Prepending history,
read updates, incoming messages and an owner send no longer displace a reader
who is away from the bottom; a viewport within the existing 60-pixel follow zone
still follows new content. Composer and structured-decision drafts remain intact.
The focused integrated-source fixture passes in Chromium and WebKit; 128 unit
tests plus the real isolated-hub composer and full decision browser suites also
pass in both engines.

Bug `wi_a1b959960975589a` revision 3 and bounded order #1167 are documented in
[the implementation report](board-scroll.md) and
[the release receipt](releases/tailos-2026-09-09-board-scroll.json). The clean
source/package is retained at `.build/releases/board-scroll-c8a7552`. Rollback
is the retained Safari-mitigation package `.build/releases/safari-css-37bfbd2`
and deployment `https://a2069721.tailos.pages.dev`. Neither hub, CLI, schema,
Tailscale, TrueNAS, relays, Air preview, the old site, live tasks/profiles nor
agent sessions changed.

## September 9 Safari stylesheet recovery — mitigation released, cache rule blocked

Work item `wi_2d5111fd8c42d8e7`, bounded order #1082 and cache-delivery
amendment #1122/#1124 produced a matching isolated failure and recovery test.
Chromium and WebKit remain unstyled when a missing hashed stylesheet's 200 HTML
SPA fallback is cached immutable; both recover on reload when it is revalidated.
That comparison came from an ignored diagnostic fixture; the committed browser
regression verifies the shipped `no-cache` second request and styled reload.
Fresh current-release WebKit did not reproduce the owner report, so the exact
owner-session cause is not claimed.

Application commit `37bfbd25085ef1706d6893891471142ec03ce984` is deployed to
`https://tailos.tailarr.com` (`https://a2069721.tailos.pages.dev`, deployment
`a2069721-e7f0-4e37-a58f-63d9cf0579ad`) and served by unchanged Mini preview
PID 28664 at `http://127.0.0.1:4318`. All three origins match the retained
package's 81 served files and manifest SHA-256
`84d3600051f20e1d3efd7e0d7307202bbca863c4d5807ac36eb5c6fc8609d2cc`.
Chromium and Playwright WebKit passed styled desktop/mobile load and reload,
production WASM startup, and isolated synthetic-vault restoration with preserved
server/session identities and no page errors. Native Safari was unavailable
because remote automation is disabled; no owner preference or profile changed.

The custom domain still overrides packaged `no-cache` asset headers with
`max-age=14400`, including a missing CSS URL served as 200 `text/html`. The same
override applies to a `/bundles` probe, so the tested bundle-path commit
`d8190d7a4d8f9c02722249a731f8db0cf02b3b76` is retained but deliberately
unshipped. Authorized Cloudflare settings/rule reads returned 403, no setting was
changed, and immediate reload recovery remains blocked pending least-privileged
access or an owner-performed host-scoped Dashboard change. See
[the investigation report](safari-css.md) and
[the release/blocker receipt](releases/tailos-2026-09-09-safari-css.json).
Rollback is the retained routing package
`.build/releases/work-item-routing-1e482f6/dist-static` and prior deployment
`https://1967ad21.tailos.pages.dev`; it restores the reproduced immutable-cache
risk. Hub, CLI, database, Tailscale, TrueNAS networking, relays, Air preview,
the old site, live records and owner sessions were unchanged.
The original `safari-css` worker failed before implementation and remains exited
rather than retired; its earlier 409 admission/retirement path admitted nobody.

## September 9 work-item session routing — release verified pending final acceptance

Application `1e482f63c049727f77510d9d8561aee5ca5f7234` is deployed to
`https://tailos.tailarr.com` (`https://1967ad21.tailos.pages.dev`) and served by
the unchanged Stephens-Mini preview at `http://127.0.0.1:4318`. All three
origins match the retained package's 81 served-file hashes and manifest SHA-256
`c482b52b6a799c6d62d77643407010bb61f374ceb44736abdbb2abf928af24d2`.
Real Chromium on every origin started production WASM and restored an isolated
synthetic vault without browser errors. The Mini preview remains PID 28664 and
continues to serve `.wasm.gz` as `application/gzip` without content encoding.

The hub is RUNNING through existing TrueNAS middleware/TCP from
`/mnt/deepfreeze/tailterm-hub/releases/20260909-work-item-routing-1e482f6/tailterm-hub`,
SHA-256 `edaec95e25f62ac0c8b5660ca3841803b7c836df265e4a3ed5cfd68fe296f706`.
The additive history/routing tables, database integrity/FK checks and
authenticated boundary pass. The pre-update online SQLite backup is mode 0600
at `/mnt/deepfreeze/tailterm-hub/backups/before-work-item-routing-20260909T195220Z.sqlite`.
No live task/profile fixture was created or inspected for release testing.

Mini and Air run matching Darwin arm64 `tt` SHA-256
`909220a58f79592479ff49c7f19722f24da8779592d09c17b4d992a21fd7218b`.
Both retain `~/.local/bin/tt-before-work-item-routing-1e482f6` at the prior hash;
relay PIDs 83457 and 912 did not change. Air's webpage preview remains retired.

The release binds each reusable-team worker to one exact item/run, restores only
complete accepted immutable history, filters later traffic through explicit item
links, preserves originals and failed replacement identities, separates normal
base-team admission from parented helper accounting, and makes partial launch
retry correct only the unlaunched member's folder. Bound posts, asks and human
answers retain exact considered attribution after later item revisions only when
the current-run binding and stored history validate.

Integrated Go/vet/race, 128 JavaScript tests and Chromium/WebKit routing,
history, decision and vault suites pass. The first concurrent JavaScript run had
one presentation timing failure; its focused and isolated full reruns passed
without code changes. Complete history over 128 KiB remains unsupported without
truncation, provider commands are synthetic, and current-run association is
shared-workspace provenance rather than per-message cryptographic/runtime
authentication. See [the implementation report](work-item-session-routing.md)
and [the release receipt](releases/tailos-2026-09-09-work-item-routing.json).
The exact clean source/package is retained at
`.build/releases/work-item-routing-1e482f6`; the immediately preceding history
package/deployment, versioned hub binary, database backup and per-host CLI copies
are the rollback chain. Neither Tailscale, networking,
`https://tailterm.tailarr.com`, Air preview, existing sessions nor relays changed.

## September 9 immutable work-item history — release verified

Application `d913d79f5b44b27062aa42c8776b16c6a801babf` is deployed to
`https://tailos.tailarr.com` (`https://a95003ee.tailos.pages.dev`) and served by
the unchanged Stephens-Mini preview at `http://127.0.0.1:4318`. All three origins
match the retained package's 81 served-file hashes and manifest SHA-256
`f68376732f2250d39aeab5f9a687a47aa33424e01f084b44ef4847ac72b896f4`.
Real Chromium on each origin started production WASM and restored an isolated
synthetic vault without browser errors. Cloudflare Web Analytics transforms
browser HTML by adding its beacon, so raw hash verification used `Accept: */*`;
the actual transformed browser path was verified separately.

The hub is RUNNING through existing TrueNAS middleware/TCP from
`/mnt/deepfreeze/tailterm-hub/releases/20260909-work-item-history-d913d79/tailterm-hub`,
SHA-256 `c9a4cc2b07d210798fd69991ecbe39c899dbb6b2300ea9eb42747c101d5e2b47`.
The four additive history/update tables, database integrity/FK checks,
authentication boundary, read-only readiness and invalid history limits pass.
The pre-update SQLite online backup is mode 0600 at
`/mnt/deepfreeze/tailterm-hub/backups/before-work-item-history-20260909T192023Z.sqlite`.
No live task/profile fixture was created or mutated for release testing.

Mini and Air now run the matching Darwin arm64 `tt` SHA-256
`d38d4871f55c32ff1f9eb3a41eaf8077ec5c2104b4866922fe815fd289dd05f5`.
Revision/message pagination and durable update-request help checks pass on both.
Each host retains `~/.local/bin/tt-before-work-item-history-d913d79` at the prior
hash, and relay PIDs stayed unchanged. Air's webpage preview remains retired.

The release adds immutable native/reconstructed revision history, explicit
history gaps, exact historical reads, linked-message pagination, idempotent
update receipts with run attribution, optimistic conflicts and encrypted
draft/retry preservation. Integrated Go vet/test/race, 124 JavaScript tests and
the Chromium/WebKit history/vault suites pass. The original Bugs-only create/edit
report still was not reproduced, so no causal repair is claimed. See
[the behavior report](work-item-history.md) and
[the release receipt](releases/tailos-2026-09-09-work-item-history.json).
The exact clean source/package and all binaries remain at
`.build/releases/work-item-history-d913d79`; the previous orchestrator-role
package/deployment, Board-decisions hub binary, mode-0600 database backup and
per-host CLI copies are the rollback chain. Neither Tailscale, networking,
`https://tailterm.tailarr.com`, agent sessions nor live task lifecycle changed.

## September 9 orchestrator role boundary — release verified pending final record

Application `75f1cdbc5b9689ec6db9a8ba23c7eaa1dcfaa835` is deployed to
`https://tailos.tailarr.com` (`https://451b0332.tailos.pages.dev`) and served by
the Stephens-Mini preview at `http://127.0.0.1:4318`. Both origins match all 81
served asset hashes and manifest SHA-256
`e4c607d59fa4c194916117867c46bbd84fb745a8d8fc1455076f9278a0f3595d`.
Real Chromium on each origin started the production WASM and restored an isolated
synthetic vault without browser errors.

Generated role instructions now limit the main orchestrator to decisions,
planning, routing and evidence review. Builders own implementation, including
shared schema/types and integration work. The sole implementation exception
requires both one non-database team member and disabled spawning; retired or idle
workers, capacity, quota, cost and helper allowance do not qualify. This is an
instruction-level guarantee, not a runtime sandbox or API authorization boundary.

The matching Darwin arm64 `tt` SHA-256 on both Mini and Air is
`ff7829a96a5c7902172991a944f6ab57d0bee1631ba98c8060066e51373c5551`.
Installed flag and task-aware orchestrator/worker briefing checks passed. Each
host retains `~/.local/bin/tt-before-orchestrator-role-75f1cdbc5b96` at prior
SHA-256 `c926e400c25490c9c67ea6cbd24871139b683b0e06b319e42d5edbfbdefe93cf`;
the already-running relays were observed but not restarted.

Mini's prior Vite preview added `Content-Encoding: gzip` to the intentionally
self-decompressed `.wasm.gz`, so raw hashes passed while the required browser
smoke could not. Same-item release amendment
`wi_0edddf71905d6186-release-1-amendment-974` authorized the focused serving
correction. Preview commit `b587741627c54bac80eccb536e7470320a65e3bc` now
serves the unchanged retained package as `application/gzip` without
`Content-Encoding`; focused unit, candidate-port, 81-asset and real-browser
checks pass. Only the identified localhost listener was gracefully replaced.

Feature `wi_0edddf71905d6186`, build order #821, release order #893 and amendment
#980 are documented in [the implementation report](orchestrator-role.md) and
[the release receipt](releases/tailos-2026-09-09-orchestrator-role.json). The
exact clean package and CLI builds are retained at
`.build/releases/orchestrator-role-75f1cdb`. Previous frontend rollback remains
`.build/releases/board-enter-d441722` / `https://cd03fb64.tailos.pages.dev`.
No hub, database, schema, TrueNAS, Tailscale, Air preview, live task/profile or
agent-session change accompanied this release.

## September 9 Board Enter-to-send — release verified

Application `d441722a8f7f06d8a097621a24a5e58dcba4ea95` is deployed to
`https://tailos.tailarr.com` (`https://cd03fb64.tailos.pages.dev`) and served by
the Stephens-Mini preview at `http://127.0.0.1:4318`. Both origins match all 81
served asset hashes and manifest SHA-256
`4da7c372e2dde9813c1fd63afb590aead8f6033360c7a6fb2e3456b63dc15a34`.
Public Chromium checked the exact main script, production WASM and isolated
synthetic-vault restoration without browser errors.

Board's message textarea now sends on bare Enter and keeps Shift+Enter for a
newline. Composition/repeat/pending/empty guards prevent unintended or duplicate
sends. Failed sends retain their draft, recipient, reply, selection and stable
retry identity; a committed response-loss retry stores exactly one message.
Builder and independent Chromium/WebKit evidence passed. Composition and repeat
flags were synthetic browser events; no native OS IME or physical autorepeat pass
is claimed. The larger task-form browser command retains an unrelated team-launch
dialog timeout and is not reported as passing.

Bug `wi_fc695f0cbc3f7a42`, build order #820 and frontend release order #883 are
documented in [the behavior/report](board-enter.md) and
[the release receipt](releases/tailos-2026-09-09-board-enter.json). Clean source
and package are retained at `.build/releases/board-enter-d441722`. The previous
`fd8d10273a46877b6bd7a6eb0c1dd9b2bc228bce` package remains the rollback at
`.build/releases/dropdown-dispatch-fd8d102`.

Mini's Apple Container build path was unavailable because Rosetta is not installed;
no host setup or container change was made. The authorized existing Node preview
serves root `dist-static`, so the exact retained package was synchronized there
without restarting its listener. Air stays retired. Hub, CLI, schema, TrueNAS,
Tailscale, networking, live profiles/tasks and agent sessions were unchanged.

## September 9 dropdown and send-dialog fixes — release verified

Application `fd8d10273a46877b6bd7a6eb0c1dd9b2bc228bce` is now deployed to
`https://tailos.tailarr.com` (`https://14bee768.tailos.pages.dev`) and Mini
`http://127.0.0.1:4318`. Both origins match 81 served asset hashes and manifest
`ec83e3e7faf22b7c15d9c5e499cb0db579537513f4c8669bf39d7e06dc401e2d`.
Public Chromium checked the new commit/main script, real WASM and synthetic
local-vault restoration without errors. This supersedes the older frontend
inventory below; hub and installed CLIs remain at the Board decisions release.

Dropdowns/disclosures retain their interaction state during refresh, and confirmed
bug/feature dispatch closes its originating dialog. Independent browser evidence:
38/38 dropdown cases and 28/28 dispatch cases, plus passing work-item, cache and
layout integration suites. Native OS keyboard observation remains an accepted
coverage limitation, not a claimed manual pass. Temporary QA resources are cleaned;
UI and cache-qa are retired. Release orders #778/#779 and full receipts:
[dropdown and dispatch release](releases/tailos-2026-09-09-dropdown-dispatch.json).
Contracts/outcomes: [dropdown continuity](dropdown-continuity.md) and
[dispatch dialog](dispatch-dialog.md). Prior public rollback package remains at
`.build/releases/board-decisions-39896c9c3a17/dist-static`; current clean release
package is `.build/releases/dropdown-dispatch-fd8d102/dist-static`.

The separate work-item create/edit/history bug remains in investigation. Ordinary
CRUD passed 8/8 on both current and pre-dropdown baselines; no repair is claimed.
The revised storage contract is accepted in
[the history plan](work-item-revision-history-plan.md); owner failure details
requested in #752 remain pending. The next bounded combined history build order must be
recorded before product changes. Owner #791 requests fewer coordination gates;
#794 requires workers dedicated to one item, and #796 limits lead to planning,
decisions and routing. Implementation and shared integration belong to builders. Both deployed UI bugs are confirmed Done by
db-handler #788 (dropdown revision13, send-dialog revision10). Narrative/AIV work is not activated. Air preview remains
retired; no hub/CLI/network change accompanied this frontend release.

## September 9 Board decisions — release verified, Air preview retired

Application `39896c9c3a1779bdd1b6eb77ad2edab6d447cacc` is deployed to
`https://tailos.tailarr.com` (`https://23a174e0.tailos.pages.dev`) and Mini
`http://127.0.0.1:4318`. Both origins match all 81 served asset hashes and manifest
SHA-256 `65cafae10ccfc33ff9133caeb960ce3023780da56da78b59112eb7e22739856d`.
Production Chromium started the real WASM and restored an isolated synthetic
local vault key without browser errors.

Workers use `tt ask` to present explained choices and a recommendation. The owner
explicitly submits a choice or custom answer on Board; the immutable human reply
resolves the request. Pending decisions remain discoverable beyond the latest
200 messages. Retry receipts prevent duplicate answers. See
[the contract](board-decisions.md) for failures, history and draft lifetime.

The hub is RUNNING through existing TrueNAS middleware/TCP at
`/mnt/deepfreeze/tailterm-hub/releases/20260909-board-decisions-39896c9c3a17/tailterm-hub`,
SHA-256 `b74f66108ece08708b0fa2b9275828143bf29133cce4917fdecc2e315a80c183`.
Decision tables, SQLite integrity/FK checks and authenticated read-only readiness
passed. Consistent mode-0600 backup:
`/mnt/deepfreeze/tailterm-hub/backups/before-board-decisions-20260909T145726Z.sqlite`.
The prior A1 hub binary remains available for rollback with additive tables intact;
isolated rollback/re-upgrade preserved receipts and decision metadata.

**Both Mini and Air CLIs** now have SHA-256
`c926e400c25490c9c67ea6cbd24871139b683b0e06b319e42d5edbfbdefe93cf`.
Installed `tt ask --help` and generated decision briefings pass on both hosts.
Each retains `~/.local/bin/tt-before-decisions-39896c9c3a17`; existing relays were
not restarted.

Owner #638 explicitly replaced the Air preview update with retirement, preferring
public TailOS. Release amendment
`wi_65e8fd62e46a4eb8-release-1-air-retirement-1` (#641) records that change. Air's
already-stopped `tailterm-static` container, its three unreferenced static images
(`56ded4133c7a`, `c30436529ecb`, `dev`) and two verified staged static asset folders
were removed. Localhost4318 is closed. Allocated files decreased by
1,213,915,136 bytes; observed free disk space increased by 820,813,824 bytes.
These differ because of filesystem sharing/accounting and background writes.
The separate stopped `tailterm-hub` and `buildkit` containers, shared/base images,
credentials, user data, networking and task sessions remain unchanged. Small
release receipts remain at `~/.local/share/tailterm/web-releases/`, including
`retirement-39896c9c3a17.json`. Do not recreate Air's preview without a new request.

Feature `wi_65e8fd62e46a4eb8` build #500, release #619 and amended Air acceptance
#641 are verified. The database handler owns the saved completion record. Earlier
SSH timeouts and the old preview requirement are historical and resolved by the
owner's amendment and completed Air work. Future releases must follow the current
owner-selected targets rather than treating older two-preview notes as a mandate.

Exact clean source, packaged static assets and both binaries are retained at
`.build/releases/board-decisions-39896c9c3a17` on Mini. Later documentation commits
intentionally differ from the application commit. Previous frontend source
`955bf43358c0d600028295f5466275cbae9e4714` and deployment
`https://f696ab58.tailos.pages.dev` remain rollback references; Air's retired local
images were intentionally removed to reclaim storage.

[Release receipt](releases/tailos-2026-09-09-decisions.json) records per-target
identities, backup and storage measurements. Independent QA passed 32/32 real-hub/
CLI scenarios in Chromium/WebKit, including retries, concurrency, pointer refresh,
project navigation, mobile controls and closed history. Final JS passed 113/113;
Go suite/vet/race and existing project browser compatibility passed. Lead accepted
all worker results and retired ordinary workers after dependencies cleared; lead
and db-handler remain available. No live task/profile test records, Tailscale
changes or task closures were used.

## September 8 message audit foundation — previous hub

Hub source `e95f65044c6dd554f1530a0dccf9d9a209cafd61` is deployed through TrueNAS
middleware at the existing TCP listener. Its binary is
`/mnt/deepfreeze/tailterm-hub/releases/20260908-message-audit-e95f65044c6d/tailterm-hub`,
SHA-256 `01c9e61043597832c7ef30089f9f583c9156c250192778ca6ea2fe9b9298a960`.
The app is RUNNING; additive tables/indexes, SQLite integrity/foreign keys and
authenticated read-only route checks passed. No live task or profile records
were used for acceptance tests, and no Tailscale or relay changes were made.

Feature `wi_abc84eb23688d903`, A1 order #454, adds optional same-project primary
message/item links, recorded order references and recoverable posting receipts.
Exact retries return the original message without another event or agent resume,
even after an item edit, replacement run or project closure. Human cross-project
dispatch preserves source ownership and now stores its typed primary link.
Existing unlinked clients remain compatible. See [the contract](message-audit.md).
The larger structured audit/AIV roadmap remains in progress; no new client
controls, strict enforcement or AIV adapter are included.

Release order `wi_abc84eb23688d903-a1-release-1` (#483) is hub-only. TailOS and
Mini manifests still verify application `955bf43358c0d600028295f5466275cbae9e4714`;
Air's last verified application is the same, but its optional read-only inventory
refresh timed out over SSH during this release. No frontend or host CLI files
changed. The prior audit receipt below remains their full asset verification.

Consistent pre-migration backup:
`/mnt/deepfreeze/tailterm-hub/backups/before-message-audit-20260909T003406Z.sqlite`,
mode 0600, integrity `ok`. Retain the prior Projects-release hub binary for
rollback and keep the additive tables. Isolated old-binary/new-database writes
and re-upgrade preserved receipts and passed integrity/FK checks. Restore the
snapshot only for explicit disaster recovery, accounting for subsequent writes.

[Release receipt](releases/tailos-2026-09-08-message-audit.json) records exact
versions and attributed evidence: full Go suite/vet and store/server race checks;
independent seven-scenario HTTP acceptance with fourteen invalid-reference cases;
101 JavaScript tests; Chromium/WebKit real isolated-hub compatibility. Lead accepted
both workers and retired them after all review dependencies cleared. The database
handler remains available, and lead owns subsequent scope and release coordination.

## September 8 audit workflow release — previous deployment

Application/source commit `955bf43358c0d600028295f5466275cbae9e4714` is deployed to
`https://tailos.tailarr.com` (`https://f696ab58.tailos.pages.dev`) and **both Mini
and Air** at their own `http://127.0.0.1:4318`. All 81 served asset hashes match
on all three origins. Air is now current, resolving the earlier layout deployment
blocker. Its existing stopped frontend container was started and updated with
17 changed assets (~1.47 MB); the hub and network settings were unchanged.

Both hosts have matching CLI SHA-256
`0b82422547a0d386d6b1c212e28419888d2036437d434bddff8ea87b4f56abb2`;
rollback binaries are `~/.local/bin/tt-before-audit-955bf43`. Installed `tt brief`
was verified on both. Existing relays remain running on their prior executable;
no relay restart was required for the generated briefing changes. Current threads
received the new policy on the board (#402).

Feature `wi_207f20d6eefcfa09` implements the instruction/audit workflow: all agent
work needs a durable bug/feature and bounded work order, and all agent work-item
database reads/writes go through the actual database handler. The handler owns
revision-checked provenance and completion tracking and remains available during
ordinary project closeout. This is instructional, not authenticated API role
enforcement. Human UI access remains available.

The [structured audit specification](work-item-audit-spec.md), work orders #408
and #410, proposes typed message/work-item links, versioned orders, immutable
corrections, consistent audit exports and an AIV evidence boundary. Intake and
coordination-message handling are explicit owner decisions before implementation.
No structured schema or MCP integration was implemented in this release.

[Release receipt](releases/tailos-2026-09-08-audit.json) distinguishes policy/CLI
work from layout feature `wi_b5ad1646826c44c5` Air rollout order #417. Validation:
101 JS tests, CLI tests/vet and role/availability cases passed; deployed Chromium
started production WASM and restored an isolated synthetic vault key. The handler
accepted the spec scope review after export-snapshot and phased-order clarifications.
QA's unread assignment was explicitly transferred to lead, who ran the checks;
no independent implementation QA pass is claimed. Later receipt/handoff commits
intentionally trail this application deployment.

## September 8 layout release (previous; Air dependency resolved above)

Application commit `b2e85b814ac742a4100298dca6141c936f182d1e` is deployed to
`https://tailos.tailarr.com` (`https://f9b41492.tailos.pages.dev`) and Mini
`http://127.0.0.1:4318`. Projects, Teams, Bugs and Features now reuse Board's
full-height divided layout and boxed selectors. Project terminal groups retain
exact splits, proportions, pane order and focused agent across same-origin
reloads and reconnects using stable agent identities in the encrypted local vault.
Layouts remain local to a browser origin; they do not sync between devices.

**Historical blocker, resolved by the audit release above:** bounded attempts to
`theAir` (`100.96.77.33:22`) timed out before transferring or changing any files.
Its last verified preview remains `78498da834122f648fac1ee35f6127b50e0fab1d`.
Feature `wi_b5ad1646826c44c5` remains in progress solely for this required preview
update. When Air is reachable, run `python3 scripts/deploy-remote-static.py theAir`
from a clean isolated checkout of the application commit above, with the retained
matching `dist-static` artifacts and Node dependencies available. The script
requires an exact clean HEAD; this later documentation commit deliberately does
not match the deployed manifest. Do not switch or overwrite an active checkout
or rebuild only Air from a different commit. Verify all served hashes and record
the new Air receipt before marking the feature done.

[Release receipt](releases/tailos-2026-09-08-layout.json) records both successful
origins and the Air blocker. All 101 unit tests passed. Chromium/WebKit acceptance
covered 90 independent layout screenshots, encrypted layout restoration and
existing project launch, work-item, cache and layout regressions. All 81 served
assets match on TailOS and Mini; production Chromium started the real WASM and
restored an isolated synthetic vault key. No hub or host CLI changes were needed.

All implementation and QA results were accepted and workers retired; the lead
remains available. The subsequent discipline feature is
`wi_207f20d6eefcfa09`, work order #399: all agent work must have a durable bug or
feature and a bounded work order, with all work-item database reads/writes routed
through the actual database handler. Intake/coordination may establish that record
first. The database handler remains available for the open project and owns
revision-checked audit/completion updates. See root `AGENTS.md` and
[the workflow](project-work-items.md#database-handler). AIV/MCP remains deferred.

## September 8 cache release (previous)

Application commit `78498da834122f648fac1ee35f6127b50e0fab1d` is deployed to
`https://tailos.tailarr.com` (`https://801220fd.tailos.pages.dev`). **Both Mini and
Air** now serve this commit at their own `http://127.0.0.1:4318`.

Previously visited Board, Projects, Bugs, and Features views load encrypted saved
reads immediately and refresh in the background. Saved/offline state is labelled;
failed writes retain drafts and are never queued. Teams remain local vault data.
The cache is bounded, excluded from profile sync/exports, and never drives agent
lifecycle operations. Bugs/Features now use an open-project rail like Board, with
mobile controls. Same-origin project bindings restore even with zero saved panes;
on a fresh origin use Board → project → Terminals to attach existing groups.

Both host CLIs now include the orchestrator assignment-delivery audit in newly
generated briefings. Matching binary SHA-256:
`6103b639e7b1bdd21a7fecb4e7fc2bc7a0f3b75be8c2997a703c321c7d22e3b8`.
Each retains `~/.local/bin/tt-before-cache-78498da83412`; both per-user inbox relays
were restarted and verified running. The hub remains on the Projects release
below; no hub restart, Tailscale change, or live project closure was needed.

Air was updated with 26 changed public assets (~10 MB), retaining its existing
`tailterm-static` container and image `tailterm-static:56ded4133c7a`. The new files
are in the container writable layer: restarting that container preserves them;
recreating from the old image rolls back. Verified delta, previous index/manifest,
and receipt are at `~/.local/share/tailterm/web-releases/78498da83412`. The first
copy-path validation failed before any served writes; the corrected activation
then passed all 81 served asset hashes. `scripts/deploy-remote-static.py theAir`
contains the tested Apple Container directory-path correction for future updates.
It transfers only changed assets after clean-source release verification.

[Release receipt](releases/tailos-2026-09-08-cache.json) records public/local assets,
host binary hashes and acceptance evidence. All 98 unit tests and Chromium/WebKit
cache, vault and project-navigation scenarios passed; production Chromium started
the production WASM and restored an isolated synthetic vault key. Air
localhost also passed a fresh-context mobile navigation check through temporary
SSH forwarding, with Files hidden and no page errors.

Research is complete in [AIV project agents proposal](research/aiv-project-agents-proposal.md)
and [native LLM CLI controls](research/llm-cli-control-reference.md). Neither adds
an MCP adapter or promises untested runtime configuration changes. The unread
analysis also found two remaining cursor edge cases (own-message page starvation
and unbounded read-up-to); this release changes the coordination briefing only.

Later deployment-tool/documentation commits intentionally do not require another
application deployment. Both localhost origins must be verified on future releases;
updating one host CLI or webpage does not update the other host's webpage.

## September 8 Projects release (previous)

Commit `86551cdc901e1d429fc7ced9d8c892f5b19625c2` is deployed to
`https://tailos.tailarr.com` (`https://fcc49680.tailos.pages.dev`) and served by the
Mini local preview at `http://127.0.0.1:4318`. It includes the Projects rename,
project-owned Bugs/Features, directed Send to project receipts, automatically
launched database handlers for new browser projects, recoverable handler setup,
and physical Left/Right Option pane placement (right/above).

The hub is running
`/mnt/deepfreeze/tailterm-hub/releases/20260908-project-work-items-86551cdc901e/tailterm-hub`.
Mini and Air have the matching `tt` build; their per-user inbox relays were
restarted and verified running. Each retains the previous executable as
`~/.local/bin/tt-before-project-work-items-86551cdc901e`. No TrueNAS Tailscale
configuration or live project sessions were changed for testing.

The [release receipt](releases/tailos-2026-09-08-projects.json) records the verified
81 served assets, manifest hash, hub/CLI binary hashes, consistent database backup,
and browser/Go checks. [Projects, Bugs, and Features](project-work-items.md)
describes dispatch semantics and handler recovery. Existing/headless projects
need explicit handler setup because the hub cannot launch SSH sessions itself.

The stale Air preview identified after this release was corrected by the cache
release above. The older receipt retains the observed failure as historical evidence.

Documentation commits after this release intentionally do not require another
application deployment. The sections below preserve earlier handoff history;
this section and its receipt are the current deployment inventory.

## September 8 follow-up release (previous)

Commit `63a341c2de61d4945ac38a5c96e0a9f3d33ef284` is now deployed to
`https://tailos.tailarr.com` (`https://f0a7b147.tailos.pages.dev`). It adds local
QR Tailscale sign-in with the original link and automatic dialog closure,
full-height orchestrator/two-column worker stacking, and human-directed resumption
of online retired agents. The hub executable is now
`/mnt/deepfreeze/tailterm-hub/releases/20260908-owner-resume-63a341c2de61/tailterm-hub`.
Existing host CLIs/relays are compatible and were not restarted. No TrueNAS
Tailscale settings or live tasks were changed for testing. The current Mini has
a local static preview at `http://127.0.0.1:4318`; the older container inventory
below describes the earlier handoff machine.

[Release receipt](releases/tailos-2026-09-08-qr.json) records manifest/asset and hub
binary verification. The pre-update SQLite backup passed integrity verification
and is recorded there. All QR acceptance used synthetic URLs; a real phone scan
with the owner's account is still an owner acceptance check.

The Projects release above completes the follow-ups to this release. The original
restart snapshot below is retained for context; it predates both follow-up releases.

This is the restart guide for the next computer and developer/agent. Read
[Project overview](project-overview.md) for the product model and source map.
The repository root `AGENTS.md` captures the owner's persistent constraints.

## Current state

- Repository: `https://github.com/scs32/tailterm.git`.
- Working branch: **tasks-hub**. It contains the current implementation; `main`
  remains the older Tailterm release.
- At handoff, `origin` had **no tasks-hub branch** and the local branch had no
  upstream. A normal clone of origin alone will not recover this work.
- A portable, self-contained branch bundle is supplied alongside the repository:
  `tailterm-handoff-2026-09-08.bundle`. It includes the documentation commit and
  all ancestors of `tasks-hub`, not ignored credentials, dependencies or builds.
- Last deployed application commit:
  `56ded4133c7a9ec952004dd51fa11e94a6c7d382`.
- Handoff documentation is committed after that release. Production therefore
  intentionally trails the final branch HEAD by documentation-only changes.
  Do not mistake this for an unshipped code change.
- No known unfinished implementation fix was left by this session. No live
  services or user tasks were stopped as part of ending development.

### Deployment inventory

| Component | Location / current version |
| --- | --- |
| Current public application | `https://tailos.tailarr.com`, Cloudflare Pages project `tailos`, production branch setting `main` |
| Pages release | `https://5e02332e.tailos.pages.dev`, commit `56ded4133c7a9ec952004dd51fa11e94a6c7d382` |
| Original application, leave unchanged | `https://tailterm.tailarr.com`, project `tailterm`, commit `d9c884ed6d006d23de69ecb0a4b5f4980e12e40e` |
| Local frontend on old MacBook | `http://127.0.0.1:4318`, Apple container `tailterm-static`, image `tailterm-static:56ded4133c7a` |
| Local rollback image | `tailterm-static:c30436529ecb` |
| Coordination hub | TrueNAS custom app `tailterm-hub`, `http://100.116.238.37:18765` |
| Hub executable | `/mnt/deepfreeze/tailterm-hub/releases/20260908-task-cleanup/tailterm-hub` |
| Hub state | `/mnt/deepfreeze/tailterm-hub/state/hub.sqlite` |
| Hub token, private | `/mnt/deepfreeze/tailterm-hub/hub-token` |
| Host CLI on Air and Mini | `~/.local/bin/tt`, task-cleanup implementation from `c304365` |
| Host credential file, private | `~/.config/tailterm/hub.json` |
| Host service | per-user `com.tailterm.inbox-relay`, running `tt relay` |
| Host relay state | `~/.local/state/tailterm/relay/` |
| macOS relay log | `~/Library/Logs/Tailterm/inbox-relay.log` |

The [retained deployment receipt](releases/tailos-2026-09-08.json) records the
manifest SHA-256 and verification time. All 79 served assets matched the manifest;
the 80th inventory entry is `_headers`, a hosting configuration file. The deployed
frontend passed Chromium and WebKit smoke checks, including WASM startup.

### Last data backup and operational check

A consistent SQLite backup was created on TrueNAS using SQLite's backup API:

`/mnt/deepfreeze/tailterm-hub/backups/handoff-20260908T135025Z.sqlite`

It is mode 600 and passed `PRAGMA integrity_check`. At that snapshot, the hub had
**5 tasks, 12 agents, 1 encrypted profile, 1 open task, and 0 pending cleanups**.
These are real user records. Counts can change after this snapshot. In particular,
there is an open task: do not interpret ending this development session as
permission to close it or terminate its agent.

The earlier pre-cleanup backup remains at
`/mnt/deepfreeze/tailterm-hub/backups/before-task-cleanup-20260908T133133Z.sqlite`.
Keep state, token, and profile service identity together when planning any future
hub migration. This handoff moves development, not the live TrueNAS hub.

## Resume on another computer

### 1. Recover the complete source

Copy the bundle and clone it into a new directory:

```sh
git clone -b tasks-hub /path/to/tailterm-handoff-2026-09-08.bundle tailterm
cd tailterm
git remote rename origin handoff-bundle
git remote add origin https://github.com/scs32/tailterm.git
git status -sb
git log -5 --oneline
```

The renamed bundle remote is only a local recovery source. Do not replace this
checkout with `origin/main`. The owner can later publish the branch with
`git push -u origin tasks-hub`; the handoff did not publish the previously local
branch or merge it into main.

Copying the entire existing repository, including `.git`, is another option.
Avoid copying `node_modules`, generated `.build` caches, and platform-specific
binaries as substitutes for installation/rebuilding.

### 2. Install development prerequisites

Use a supported Node.js release at least 22.12, npm, Git, Python 3, curl, tar,
Go with automatic toolchain downloads enabled, and tmux for native integration
tests. `hub/go.mod` currently requests Go 1.26.6. The pinned Tailscale WASM build
also resolves its required toolchain automatically.

```sh
npm ci
npx playwright install chromium webkit
npm run build:wasm
npm run build:static
npm run preview:static
```

On Linux, browser system libraries may also need Playwright's documented OS
dependency installation. The preview listens on `127.0.0.1:4318`. If that port is
already occupied by an Apple container, use that instance or an explicit alternate
preview port; do not stop unrelated services.

`build:wasm` is required on a clean computer. Static packaging needs the generated
`wasm/tailserve.wasm`, `.build/go-modules.txt`, and its referenced Go module license
files. Copying only a compiled WASM file is insufficient for a fresh static build.
The build pins/downloads Tailscale 1.102.3 and verifies its source checksum. Speech
model/runtime assets are also pinned, downloaded as needed, verified and packaged
locally. Allow several gigabytes for dependencies, build caches and containers.

The old Mac repeatedly ran out of disk space during linking/container import.
Only obsolete Tailterm build artifacts/images were removed. The current local
image and one rollback remain. Do not prune other projects, credential files,
user vaults, or the live hub database to recover development disk space.

### 3. Restore the user's browser profile

Source code and a Git bundle do **not** contain the user's browser vault.

1. On the old browser, check **Profile sync** reports **Synced**; keep an encrypted
   Backup & restore export as an independent recovery copy.
2. Open `https://tailos.tailarr.com` on the new computer. Enter the same username
   and existing vault passphrase. The configured profile username is
   `stephenspeicher`; the passphrase is intentionally not recorded here.
3. Connect/authorize Tailscale for this new browser. Each browser requires its own
   device identity; do not copy the old browser's node identity.
4. The app discovers the profile service through the tailnet or can use the known
   hub address. Restore the existing profile. If the local browser already has
   saved data, choose the desired copy explicitly rather than overwriting it blindly.
5. Check saved servers, keys, Teams and hub configuration. Open task/history records
   come directly from the still-running hub. Current window layout and local
   scrollback do not sync.

A localhost vault and a TailOS vault are different origins. The same profile sync
or encrypted backup flow is needed when changing origins. Existing agent hosts
remain the old machines until their saved server entries are deliberately changed;
moving the development checkout does not move a running agent process or its files.

### 4. Set up host access only if needed

Using the public app does not require installing the hub or relay on the new
computer. If it will also be an agent host, install tmux, the chosen agent CLIs,
their account credentials, and `tt` for its OS/architecture.

```sh
npm run build:tt
```

That script creates Linux arm64/amd64 and Darwin arm64 binaries under `.build/tt/`.
Install the matching binary as `~/.local/bin/tt` with executable permissions.
For Intel macOS, build `GOOS=darwin GOARCH=amd64` explicitly. Provision
`~/.config/tailterm/hub.json` privately with the existing hub URL/token and mode
600. Never put the token into source, pasted prompts, logs or the static build.

On macOS, after installing `tt` and its configuration:

```sh
python3 scripts/install-relay-macos.py
tt doctor
tt relay --status
```

The installer uses a logged-in user's GUI launchd domain. For Linux or headless
hosts, supervise `tt relay` through that host's normal service manager. Confirm
the installed Codex supports the native `queue` operation before assuming idle
agents can be resumed automatically.

Development deployment also needs SSH aliases `mini` and `truenas`, suitable keys,
and account access. `truenas` currently uses `truenas_admin` (UID 950). Mini's OS
hostname is `Stephens-Mini`. Recreate those access settings through the user's
normal secure credential process; they are not included in Git. No changes to
TrueNAS Tailscale are necessary or authorized.

## Build, test and release

### Targeted regression commands

```sh
npm test
npm run test:hub
node tests/task-form-browser.mjs
node tests/teams-vault-browser.mjs
node tests/project-folder-browser.mjs
node tests/profile-sync-browser.mjs
```

Run suites relevant to changes; it is not necessary to repeat every suite for a
small edit. `task-form-browser.mjs` exercises a real isolated hub and private tmux
server, task launch/retry, permissions, retirement, closure/cleanup, and a 205-message
read-only archive/download. `task-history.test.js` checks export pagination across
separate message/event sequence spaces. The handoff release passed 69 JS unit tests
and the expanded task browser fixture in Chromium and WebKit. Hub vet/tests passed
for the last backend change (`c304365`).

For changes to WASM/SSH or wider terminal behavior:

```sh
./scripts/build-wasm.sh --test
npm run build:static
npm run test:static
```

The test-only WASM transport must never enter production. Other focused scripts
under `tests/` cover terminal links, context menus, tmux menus, appearance, speech,
clipboard and connection sizing. Use their documented fixture requirements.

### Publish the current frontend to TailOS

Commit changes first; release verification requires a clean tree and exact HEAD.
Because the handoff adds documentation after the last deployment, the existing
`dist-static/release.json` on the old machine is now intentionally stale for HEAD.
Rebuild before a future deployment; no redeployment was needed for this handoff.

```sh
npm run build:static
npm run verify:release
npx wrangler login
npx wrangler pages deploy dist-static --project-name tailos --branch main --commit-hash "$(git rev-parse HEAD)" --commit-dirty=false
```

`--branch main` here selects the existing Pages project's production deployment;
it does not require switching the source branch away from `tasks-hub` or merging
Git main. **Do not run `npm run deploy:static`: it targets the old `tailterm` project.**
Do not create a replacement Pages project or change DNS; TailOS already works.
Only `dist-static/` is public. Record the returned deployment URL, source hash and
release-manifest hash, and verify `/release.json` plus the served assets. `_headers`
is Pages configuration and is not available as a normal served asset.

Wrangler authenticated successfully on the old Mac through its private local OAuth
configuration; that file is not in the bundle. Authenticate afresh on the new
computer. A direct REST attempt using that local token failed while Wrangler
worked; use Wrangler rather than attempting to repair or expose the old token.

The old machine's detailed deployed-browser checks and receipts under `.build/`
are ignored convenience artifacts. The latest receipt has been copied into
`docs/releases/`. `tests/deployed-browser.mjs` accepts an explicit origin:

```sh
node tests/deployed-browser.mjs https://tailos.tailarr.com
```

Never rely on a script's default hostname for production testing/deployment.
Some browser scripts write preview screenshots; check `git status` afterward.

### Local Apple webpage

If Apple Container is installed and the existing `tailterm-static` container is
present, use:

```sh
python3 scripts/deploy-apple-web.py
```

It verifies the release, builds an image, checks a candidate on port 4319, then
replaces only port 4318's frontend container. It preserves the previous image.
On a new machine without this existing container, use `npm run preview:static`
first; the replacement script expects an existing instance. The hub is independent.

### TrueNAS hub updates

The hub is already running. Do not redeploy it just because development moved.
When a backend change requires an update, first take a consistent SQLite backup
and compile the Linux amd64 binary expected by the middleware deployment script:

```sh
cd hub
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o ../.build/ttbin/tailterm-hub-linux-amd64 ./cmd/tailterm-hub
cd ..
python3 scripts/deploy-truenas-hub.py UNIQUE_RELEASE_NAME --update
```

Use a new release name; do not overwrite a mounted executable. The script changes
only the `tailterm-hub` app through `midclt`, binds its existing private TCP address,
and retains state/token mounts. It must not enable embedded tsnet, mount a Tailscale
socket, install a second node, or modify host Tailscale. The app uses UID/GID 950,
a read-only root filesystem and a 32-agent cap. Rollback points the app at the
previous release path after considering migration compatibility; do not blindly
restore an older database over new user work.

Back up SQLite through its backup API (as done at handoff) or while stopped.
Copying a live `hub.sqlite` without its WAL is not a valid backup procedure. Keep
`profile_meta` and profile history so the sync service identity and revisions survive.

For a compatible hub/CLI rollout, update the hub first, then install the new CLI
atomically on each host and restart only its per-user relay. Preserve the old
binary for rollback. Do not restart all containers or unrelated services.

## Session closeout and next steps

Recent implementation sequence:

- `d277f3b`: retirement without destroying terminals.
- `1ecbcd7`, `0277d1e`: explicit project folders, inheritance and correction on retry.
- `c304365`: durable remote cleanup when closing tasks, pending/retry UI, ownership
  checks, run-scoped receipts, and relay integration.
- `56ded41`: remove the 01 badge/empty-state filler, expose closed conversations,
  read-only archives and complete history downloads.

All changes from this development session are committed. No background coding
agents were spawned. Test fixtures used temporary state and were stopped. Public
TailOS, the local frontend, the TrueNAS hub, and the user's host relays remain
running. Ending this coding conversation should not stop those services or the
open live task.

For the next coding assistant: read this file, the overview and root `AGENTS.md`,
check the branch/worktree, and ask what to work on next. Do not silently resume
old speculative desktop/IDE work, re-enable Files, rebuild the deployment topology,
or migrate the existing hub. The owner may choose transcript capture or other
improvements later; those are not unfinished work from this handoff.
