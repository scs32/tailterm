# Random flashes and focus continuity

Bounded candidate for Bug `wi_16377f2bff53f918` revision 1, work order #2227.

The shared view-refresh boundary now coalesces an incoming repaint while text is
actively being entered, then applies the newest queued render after 120 ms of
typing idle. IME composition remains held until its `compositionend` event, so a
background update cannot replace its editor mid-composition. `interrupt()` and
a changed presentation mount clear the text/composition references and timer,
so hide, disconnect, or remount cannot leave a queued refresh starved.
Pointer, native picker, keyboard-button, disclosure, draft, selection, and
scroll protections remain unchanged.

The Board composer replaces its offset `outline` with an inset 2px accent
shadow. Keyboard focus remains visibly stronger than hover, but there is no
separate outer frame. The Board fixture checks the actual focused composer in
both Chromium and WebKit: the reviewed baseline was a 2px accent outline with
no shadow, while this candidate computes no outline and an inset shadow.

Validation uses only isolated fixtures: the focused refresh unit suite covers
typing idle, composition, and an interrupted composition followed by remount,
alongside existing picker and focus continuity cases; the Board composer
Chromium/WebKit fixture passes. The broad dropdown
continuity suite retained two pre-existing API-fixture failures when deleting a
selected synthetic agent (`400 invalid request` in both engines); its other
cases passed. This does not claim physical Safari or owner-device confirmation.

## Integration preparation

This preparation is bounded to work order #2227 and integration supplement
#2650, selected in #2656 and started by handler message #2660 with receipt
`qrr_bdecdfaf0cb41501`. The isolated branch
`prep/ui-flashes-integration` began at
`c8ef0c1a29a534939689d2bc6f45869e7092aac4` and cleanly cherry-picked accepted
Flashes commits `f49f22d9abd509f20b06975ee52b5ad3c5a61e29` followed by
`f0fc26fcf0a0f0a278b99fb8dd5b39205790c212`. The resulting combined candidate
is `727d91b775aa5fea60ae6245461834d4bf2f44fb`.

The isolated candidate passed `npm test` (176 tests), the Board composer
Chromium/WebKit fixture, the live Search Chromium/WebKit fixture, and the
work-items scroll Chromium/WebKit fixture across Bugs and Features. `build:wasm`
completed. `build:static` completed Vite's bundle stage but package assembly
could not copy the absent installed file `node_modules/@xterm/xterm/LICENSE`;
therefore no static manifest/hash/entry count or `verify:release` result exists.
No install, retry, source change, deployment, or root-worktree write was made
to work around that environment dependency. This report-only update is separate
from the frozen application/package candidate.

Handler clarification #2678 subsequently authorized `npm ci` in this isolated
worktree using the unchanged committed lockfile. It completed with no tracked
`package.json` or lockfile change. The rerun then completed `build:static` and
`verify:release`: `dist-static/release.json` has 82 entries and SHA-256
`dba8628f67bbe9da003dfad11588566d060c22c1b67a182d30aa736d729fbd02`.
Verification reported all 82 assets matching the report-only source commit
`ff1381697f6079f45b970daa93c0b214e0b28f30`. This resolves only the local
packaging evidence blocker; it does not publish or alter the frozen application
candidate.
