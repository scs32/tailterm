// Pointer capture keeps internal dragging inside the fullscreen top layer and
// avoids browser-native HTML drag handling. It also supports touch and pens.
export function setupPaneDrag(shell, { start, move, drop, cancel }) {
  let pending,
    dragging = false,
    suppressClick = false,
    optionSequence = 0,
    lastPointer = null;
  const optionKeys = new Map();
  const optionSide = (event) => {
    if (event.code === "AltLeft") return "left";
    if (event.code === "AltRight") return "right";
    if (event.key !== "Alt") return null;
    if (event.location === KeyboardEvent.DOM_KEY_LOCATION_LEFT) return "left";
    if (event.location === KeyboardEvent.DOM_KEY_LOCATION_RIGHT) return "right";
    return null;
  };
  const placement = () => {
    const latest = [...optionKeys.entries()].sort(
      (a, b) => b[1].order - a[1].order,
    )[0]?.[1]?.side;
    return latest === "left" ? "right" : latest === "right" ? "above" : null;
  };
  const gesture = () => ({ placement: placement() });
  const updateMove = () => {
    if (!dragging || !lastPointer) return;
    move(
      document.elementFromPoint(lastPointer.x, lastPointer.y),
      lastPointer.x,
      lastPointer.y,
      gesture(),
    );
  };
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
    lastPointer = { x: e.clientX, y: e.clientY };
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
    move(
      document.elementFromPoint(e.clientX, e.clientY),
      e.clientX,
      e.clientY,
      gesture(),
    );
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
      else drop(document.elementFromPoint(e.clientX, e.clientY), gesture());
    }
    lastPointer = null;
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
      const side = optionSide(e);
      if (side) {
        const code = e.code || `Alt-${e.location}`;
        if (!optionKeys.has(code))
          optionKeys.set(code, { side, order: ++optionSequence });
        if (dragging) e.preventDefault();
        updateMove();
      }
      if (e.key === "Escape" && dragging) {
        e.preventDefault();
        e.stopPropagation();
        finish({}, true);
      }
    },
    true,
  );
  document.addEventListener(
    "keyup",
    (e) => {
      const side = optionSide(e);
      if (!side) return;
      optionKeys.delete(e.code || `Alt-${e.location}`);
      if (dragging) e.preventDefault();
      updateMove();
    },
    true,
  );
  window.addEventListener("blur", () => {
    optionKeys.clear();
    finish({}, true);
  });
  document.addEventListener("fullscreenchange", () => finish({}, true));
}
