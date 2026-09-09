# Safari stylesheet recovery investigation

Work item `wi_2d5111fd8c42d8e7`, bounded order `safari-css-1` in message
#1082, covers the owner report that Safari appeared unstyled after the routing
release. The cache-delivery amendment was requested in #1122 and committed by
the database handler in #1124 as item revision 9.

## Finding

Fresh isolated Playwright WebKit contexts did not reproduce an unstyled current
release at `https://tailos.tailarr.com`: the stylesheet returned 200 as
`text/css`, 1,015 rules loaded, and desktop and mobile layouts were styled on
normal load and reload. This does not disprove the owner observation and is not
native Safari evidence.

A matching failure mechanism was reproduced in isolated Chromium and WebKit
with the ignored diagnostic fixture `.build/safari-css-cache-fixture.mjs`.
Cloudflare Pages serves the application shell as a 200 `text/html` SPA fallback
when a requested hashed stylesheet is absent. If that response is cached as an
immutable asset, the browser rejects it because of its MIME type and continues
to reuse it on reload. Each browser requested the stylesheet once and remained
unstyled with a one-year immutable response. With revalidation enabled, each
browser requested it twice, received the corrected `text/css` response on
reload, and became styled. The committed regression
`tests/static-cache-browser.mjs` retains the supported half of that comparison:
it verifies that the shipped `no-cache` policy performs the second request and
recovers styling on reload; it does not itself rerun the immutable baseline.

The test establishes the failure and recovery behavior, but it cannot prove
that a deploy-time absence or stale hashed URL caused the owner's exact session.
No owner profile, storage, vault, session or live task/profile fixture was read
or changed.

## Shipped mitigation

Commit `37bfbd25085ef1706d6893891471142ec03ce984` is the application actually
released. It includes the original focused fix from
`95fdf867a3b5342a11dfac04f59ff0ca01e9997b`:

- static assets request `Cache-Control: no-cache` rather than immutable caching;
- `:root` sets both `-webkit-text-size-adjust: 100%` and
  `text-size-adjust: 100%`, producing a fresh stylesheet hash and stable mobile
  text sizing; and
- the committed browser regression covers `no-cache` reload recovery in
  Chromium and WebKit; the separate ignored diagnostic fixture supplied the
  immutable failing baseline.

The retained clean package is `.build/releases/safari-css-37bfbd2`. Its
`release.json` SHA-256 is
`84d3600051f20e1d3efd7e0d7307202bbca863c4d5807ac36eb5c6fc8609d2cc`.
The stylesheet is `/assets/index-Bkm6l0FY.css` with SHA-256
`ee70f291a094abe1bff819c0c2b789bb9f594c3b836284483cad719f9f9d9cf1`;
the main script is `/assets/index-C6t4eEsv.js`; and production WASM is
`/assets/tailserve-4uARE7-V.wasm.gz` with SHA-256
`9930c542c204eaa46a5b239460ab07542a66596ea368cbec5e545a113861f41b`.

The exact package is deployed to Cloudflare Pages project `tailos` as
deployment `a2069721-e7f0-4e37-a58f-63d9cf0579ad`, available at
`https://a2069721.tailos.pages.dev`, and is synchronized to the existing Mini
preview at `http://127.0.0.1:4318`. The preview remains PID 28664; it was not
restarted.

All 81 served files matched the retained package on TailOS, the immutable Pages
URL and Mini. Real Playwright Chromium and WebKit on TailOS and Mini loaded the
correct CSS and JavaScript MIME types, loaded 1,015 CSS rules, started production
WASM, generated and restored an isolated synthetic vault, preserved its server
and session identities, and had no page errors. Desktop matching margins,
390-pixel mobile width, selection fill and muted placeholder styling passed.

## Remaining delivery blocker

The canonical custom domain overrides the package header. At the recorded
checkpoint:

| Request | Pages deployment | `tailos.tailarr.com` | Mini |
| --- | --- | --- | --- |
| released CSS | `no-cache, no-cache` | `max-age=14400` | `no-cache` |
| missing CSS fallback | not relied upon | 200 `text/html`, `max-age=14400` | not relied upon |
| `/release.json` | `no-cache` | `no-cache` | `no-cache` |

The four-hour cache lifetime on a missing CSS/JavaScript fallback prevents the
required immediate reload recovery. It also appears on the probe
`/bundles/safari-cache-probe.css`, so moving build output from `/assets` to
`/bundles` is not a supported correction.

The cache-delivery amendment authorized read-only configuration inspection and a
narrow, reversible, host-scoped correction. The installed Wrangler OAuth
identity could list the `tailarr.com` zone, but sanitized reads of browser-cache
TTL, the `http_request_cache_settings` entrypoint ruleset, and active page rules
each returned HTTP 403. No Cloudflare setting was changed and the unchanged
denial was not retried. Resolving this requires either least-privileged
cache/settings read and cache-rules edit access, or an owner-performed Dashboard
inspection/change limited to `tailos.tailarr.com`. Explicitly accepting the
four-hour behavior is possible but does not satisfy immediate reload recovery.

Commit `d8190d7a4d8f9c02722249a731f8db0cf02b3b76` and retained package
`.build/releases/safari-css-d8190d7` preserve the tested `/bundles` experiment.
Its package manifest SHA-256 is
`e0418faf42b1647009e90bcbb606e252363b594f24f2fc29717f53d9eedda9b8`.
It passed release verification, 128 JavaScript tests, the Chromium/WebKit cache
regression, the speech browser test and an isolated preceding-release WebKit
upgrade. It was deliberately not deployed after the custom-domain probe
invalidated its delivery premise.

## Verification boundaries

- `npm test`: 128/128 passed for both released and retained follow-up source.
- `.build/safari-css-cache-fixture.mjs`: the initial ignored diagnostic fixture
  observed the immutable failing baseline and revalidated recovery in Chromium
  and WebKit.
- `node tests/static-cache-browser.mjs`: the committed regression verified the
  shipped `no-cache` second request and styled reload in Chromium and WebKit.
- `npm run verify:release`: all 82 manifest entries passed for both retained
  packages.
- `node tests/deployed-browser.mjs`: Chromium passed TailOS and Mini with
  production WASM and isolated vault restoration.
- Focused deployed WebKit checks passed TailOS and Mini; the isolated
  preceding-release-to-candidate upgrade preserved exact synthetic server and
  session IDs, username and profile appearance.
- `node tests/speech-browser.mjs` passed for the retained `/bundles` candidate
  after supplying its existing ignored synthetic WAV fixture.
- A broader `npm run test:static` attempt passed its preceding static phases but
  stopped at the pre-existing task-form selector `#command-results button`
  (`Task hub: configure`). It was unrelated to this fix and was not repeatedly
  rerun.
- Native Safari 26.5.2 is installed on macOS 26.5.2, but Safari remote
  automation is disabled. No preference or owner browser profile was changed,
  so no native Safari pass is claimed.
- Owner-session recovery and exact owner-session cause remain unconfirmed.

## Rollback and exclusions

The immediate application rollback is the retained routing package
`.build/releases/work-item-routing-1e482f6/dist-static`, commit
`1e482f63c049727f77510d9d8561aee5ca5f7234`, deployed previously at
`https://1967ad21.tailos.pages.dev`. Deploy that exact package explicitly to
Cloudflare project `tailos` and synchronize it to Mini without restarting the
listener. The rollback restores immutable asset headers and therefore also
restores the reproduced stale-fallback risk.

No hub, CLI, database, schema, Tailscale, TrueNAS networking, relay, Air preview,
old `tailterm.tailarr.com`, live profile/task, task lifecycle or owner-session
change was made under this work order.

The original `safari-css` worker failed before implementation and remains exited
rather than retired. Its earlier 409 admission/retirement path admitted nobody;
this recorded replacement did not reuse, alter or retire that identity.
