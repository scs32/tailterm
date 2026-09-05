// Pointer capture keeps internal dragging inside the fullscreen top layer and
// avoids browser-native HTML drag handling. It also supports touch and pens.
export function setupPaneDrag(shell, { start, move, drop, cancel }) {
  let pending,
    dragging = false,
    suppressClick = false;
  shell.addEventListener("dragstart", (e) => {
    if (e.target.closest(".tab, .pane-header")) e.preventDefault();
  });
  shell.addEventListener("pointerdown", (e) => {
    if (e.button !== 0 || !e.isPrimary || e.target.closest("dialog")) return;
    const source = e.target.closest(".tab, .pane-header");
    if (
      !source ||
      e.target.closest(
        "[data-close], [data-session-menu], [data-detach], .pane-header button:not(.pane-label)",
      )
    )
      return;
    pending = { source, x: e.clientX, y: e.clientY, pointer: e.pointerId };
  });
  shell.addEventListener("pointermove", (e) => {
    if (!pending || pending.pointer !== e.pointerId) return;
    if (
      !dragging &&
      Math.hypot(e.clientX - pending.x, e.clientY - pending.y) < 6
    )
      return;
    e.preventDefault();
    if (!dragging) {
      dragging = true;
      shell.setPointerCapture(e.pointerId);
      shell.classList.add("dragging-pane");
      start(pending.source);
    }
    move(document.elementFromPoint(e.clientX, e.clientY), e.clientX, e.clientY);
  });
  function finish(e, cancelled = false) {
    if (!pending || (e.pointerId != null && pending.pointer !== e.pointerId))
      return;
    const pointer = pending.pointer,
      wasDragging = dragging;
    pending = null;
    dragging = false;
    if (wasDragging) {
      suppressClick = true;
      setTimeout(() => {
        suppressClick = false;
      }, 0);
      if (cancelled) cancel();
      else drop(document.elementFromPoint(e.clientX, e.clientY));
    }
    shell.classList.remove("dragging-pane");
    if (shell.hasPointerCapture(pointer)) shell.releasePointerCapture(pointer);
  }
  shell.addEventListener("pointerup", (e) => finish(e));
  shell.addEventListener("pointercancel", (e) => finish(e, true));
  shell.addEventListener("lostpointercapture", (e) => finish(e, true));
  shell.addEventListener(
    "click",
    (e) => {
      if (suppressClick) {
        e.preventDefault();
        e.stopImmediatePropagation();
      }
    },
    true,
  );
  document.addEventListener(
    "keydown",
    (e) => {
      if (e.key === "Escape" && dragging) {
        e.preventDefault();
        e.stopPropagation();
        finish({}, true);
      }
    },
    true,
  );
  window.addEventListener("blur", () => finish({}, true));
  document.addEventListener("fullscreenchange", () => finish({}, true));
}
