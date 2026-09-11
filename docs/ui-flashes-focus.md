# Random flashes and focus continuity

Bounded candidate for Bug `wi_16377f2bff53f918` revision 1, work order #2227.

The shared view-refresh boundary now coalesces an incoming repaint while text is
actively being entered, then applies the newest queued render after 120 ms of
typing idle. IME composition remains held until its `compositionend` event, so a
background update cannot replace its editor mid-composition. Pointer, native
picker, keyboard-button, disclosure, draft, selection, and scroll protections
remain unchanged.

The Board composer focus selector is scoped to its actual `#mode-view` host.
It keeps the keyboard-visible accent stroke inset on the textarea instead of
allowing the general focus outline to render as a separate outer frame.

Validation uses only isolated fixtures: the focused refresh unit suite covers
typing idle and composition alongside existing picker and focus continuity
cases; the Board composer Chromium/WebKit fixture passes. The broad dropdown
continuity suite retained two pre-existing API-fixture failures when deleting a
selected synthetic agent (`400 invalid request` in both engines); its other
cases passed. This does not claim physical Safari or owner-device confirmation.
