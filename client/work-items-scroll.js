const SCROLL_IDLE_MS = 120;

const captureScroll = (container) => {
  if (!container) return { scrollTop: 0 };
  const scrollTop = Number(container.scrollTop) || 0,
    bounds = container.getBoundingClientRect?.(),
    anchor = bounds
      ? [...(container.querySelectorAll?.("[data-work-item]") || [])].find(
          (row) => {
            const rect = row.getBoundingClientRect();
            return rect.bottom > bounds.top && rect.top < bounds.bottom;
          },
        )
      : null;
  return {
    scrollTop,
    anchorID: anchor?.dataset.workItem || "",
    anchorOffset: anchor
      ? anchor.getBoundingClientRect().top - bounds.top
      : null,
  };
};

const restoreScroll = (container, saved) => {
  if (!container) return;
  container.scrollTop = saved?.scrollTop || 0;
  if (!saved?.anchorID || saved.anchorOffset === null) return;
  const anchor = [...container.querySelectorAll("[data-work-item]")].find(
    (row) => row.dataset.workItem === saved.anchorID,
  );
  if (!anchor) return;
  const bounds = container.getBoundingClientRect();
  container.scrollTop +=
    anchor.getBoundingClientRect().top - bounds.top - saved.anchorOffset;
};

const captureFocus = (container) => {
  const active = globalThis.document?.activeElement,
    row = active?.closest?.("[data-work-item]"),
    attribute = [...(active?.attributes || [])].find((candidate) =>
      candidate.name.startsWith("data-item-"),
    );
  if (!container?.contains?.(active) || !row) return null;
  const action = attribute?.name.slice("data-item-".length);
  return action ? { itemID: row.dataset.workItem, action } : null;
};

const restoreFocus = (container, saved) => {
  if (!container || !saved) return;
  const row = [...container.querySelectorAll("[data-work-item]")].find(
      (candidate) => candidate.dataset.workItem === saved.itemID,
    ),
    control = row?.querySelector?.(`[data-item-${saved.action}]`);
  control?.focus?.({ preventScroll: true });
};

// A refresh may update the backing item list while a browser-owned scroll is
// still moving. Retain that exact DOM until idle, then repaint the latest state
// and restore the first visible immutable item rather than an absolute offset.
export function createWorkItemsScroll({ getContainer, render, canRender }) {
  let renderedContext = "",
    active = false,
    touchActive = false,
    timer = null,
    renderHeld = false,
    ignoredElement = null,
    ignoredScrollTop = 0;

  function flush() {
    if (!renderHeld || active || !canRender()) return;
    renderHeld = false;
    render();
  }

  function scheduleIdle() {
    clearTimeout(timer);
    timer = null;
    if (touchActive) return;
    timer = setTimeout(() => {
      timer = null;
      active = false;
      flush();
    }, SCROLL_IDLE_MS);
  }

  function hold() {
    active = true;
    scheduleIdle();
  }

  function current(event) {
    const container = event.currentTarget;
    return container === getContainer() ? container : null;
  }

  function note(event) {
    const container = current(event);
    if (!container) return;
    if (
      event.type === "scroll" &&
      container === ignoredElement &&
      Math.abs(container.scrollTop - ignoredScrollTop) < 0.5
    ) {
      ignoredElement = null;
      return;
    }
    ignoredElement = null;
    hold();
  }

  function startTouch(event) {
    if (!current(event)) return;
    touchActive = true;
    active = true;
    clearTimeout(timer);
    timer = null;
  }

  function endTouch(event) {
    if (!current(event)) return;
    touchActive = false;
    if (active) scheduleIdle();
  }

  function interrupt() {
    clearTimeout(timer);
    timer = null;
    renderedContext = "";
    active = false;
    touchActive = false;
    renderHeld = false;
    ignoredElement = null;
  }

  function beforeRender(context) {
    const key = String(context ?? "");
    if (renderedContext && renderedContext !== key) interrupt();
    const old = renderedContext === key ? getContainer() : null;
    if (active && old) {
      renderHeld = true;
      return null;
    }
    renderHeld = false;
    return {
      context: key,
      scroll: captureScroll(old),
      focus: captureFocus(old),
    };
  }

  function afterRender(frame) {
    renderedContext = frame.context;
    const container = getContainer();
    restoreScroll(container, frame.scroll);
    restoreFocus(container, frame.focus);
    if (!container) return;
    ignoredElement = container;
    ignoredScrollTop = container.scrollTop;
    container.addEventListener?.("scroll", note, { passive: true });
    container.addEventListener?.("wheel", note, { passive: true });
    container.addEventListener?.("touchstart", startTouch, { passive: true });
    container.addEventListener?.("touchend", endTouch, { passive: true });
    container.addEventListener?.("touchcancel", endTouch, { passive: true });
  }

  return { beforeRender, afterRender, flush, interrupt };
}
