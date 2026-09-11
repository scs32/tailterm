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
