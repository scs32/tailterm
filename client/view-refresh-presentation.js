const INTERACTIVE_POINTER_TARGET =
  "button, summary, select, input, textarea, [role=button]";

export const shouldReleaseViewPointer = (activePointer, event) =>
  activePointer !== null &&
  (event.type === "blur" || event.pointerId === activePointer);

export const isNativeSelectActivation = (event) =>
  event.key === " " ||
  event.key === "Enter" ||
  event.key === "ArrowDown" ||
  event.key === "ArrowUp";

export const disclosureStateKey = (context, name) =>
  `${String(context ?? "")}\u0000${String(name ?? "")}`;

// Hub reads may finish at any time, but browser-owned control interactions must
// finish against the DOM that started them. This boundary coalesces repaints
// while a native select/popover or pointer gesture is active and remembers the
// manual state of explicitly keyed disclosures across ordinary refreshes.
export function createViewRefreshPresentation({ render }) {
  let root = null;
  let pointer = null;
  let pointerReleasePending = false;
  let keyboardTarget = null;
  let nativeSelect = null;
  let nativeSelectWatch = 0;
  let queued = false;
  let scheduled = false;
  let renderedContext = "";
  let focusKey = "";
  const disclosure = new Map();

  function openPopover() {
    try {
      const node = root?.querySelector?.(":popover-open");
      return node?.matches?.(":popover-open") ? node : null;
    } catch {
      return null;
    }
  }

  function isHeld() {
    return (
      pointer !== null ||
      pointerReleasePending ||
      Boolean(keyboardTarget?.isConnected) ||
      Boolean(nativeSelect?.isConnected) ||
      Boolean(openPopover())
    );
  }

  function captureDisclosure() {
    root
      ?.querySelectorAll?.("details[data-view-disclosure]")
      .forEach((node) => {
        disclosure.set(
          disclosureStateKey(renderedContext, node.dataset.viewDisclosure),
          node.open,
        );
      });
  }

  function scheduleFlush() {
    if (!queued || scheduled) return;
    scheduled = true;
    setTimeout(() => {
      scheduled = false;
      if (!queued || isHeld()) return;
      queued = false;
      render();
    }, 0);
  }

  function releasePointer(event) {
    if (!shouldReleaseViewPointer(pointer, event)) return;
    pointer = null;
    pointerReleasePending = true;
    setTimeout(() => {
      pointerReleasePending = false;
      scheduleFlush();
    }, 0);
  }

  function onPointerDown(event) {
    const target = event.target?.closest?.(INTERACTIVE_POINTER_TARGET);
    if (event.button !== 0 || !target) return;
    pointer = event.pointerId;
    if (target.matches?.("select") && !target.disabled)
      activateNativeSelect(target);
  }

  function onKeyDown(event) {
    const target = event.target?.closest?.("select");
    if (target && !target.disabled) {
      if (isNativeSelectActivation(event)) activateNativeSelect(target);
      if (event.key === "Escape" && nativeSelect === target)
        setTimeout(() => commit(target), 0);
      return;
    }
    const press = event.target?.closest?.("button, summary, [role=button]");
    if (
      press &&
      !press.disabled &&
      (event.key === " " || event.key === "Enter")
    )
      keyboardTarget = press;
  }

  function onKeyUp(event) {
    if (
      keyboardTarget &&
      event.target === keyboardTarget &&
      (event.key === " " || event.key === "Enter")
    ) {
      keyboardTarget = null;
      pointerReleasePending = true;
      setTimeout(() => {
        pointerReleasePending = false;
        scheduleFlush();
      }, 0);
    }
  }

  function activateNativeSelect(control) {
    nativeSelect = control;
    const watch = ++nativeSelectWatch;
    let sawOpen = false;
    let closedFrames = 0;
    let supportsOpen = false;
    try {
      supportsOpen = Boolean(
        globalThis.CSS?.supports?.("selector(select:open)"),
      );
    } catch {
      // change, focusout, Escape and hide remain the compatibility fallbacks.
    }
    if (!supportsOpen) return;
    const observe = () => {
      if (watch !== nativeSelectWatch || nativeSelect !== control) return;
      if (!control.isConnected) {
        commit(control);
        return;
      }
      let open = false;
      try {
        open = control.matches(":open");
      } catch {
        return;
      }
      if (open) {
        sawOpen = true;
        closedFrames = 0;
      } else if (sawOpen || ++closedFrames >= 4) {
        commit(control);
        return;
      }
      (globalThis.requestAnimationFrame || ((next) => setTimeout(next, 16)))(
        observe,
      );
    };
    (globalThis.requestAnimationFrame || ((next) => setTimeout(next, 16)))(
      observe,
    );
  }

  function onChange(event) {
    if (event.target === nativeSelect) commit(nativeSelect);
  }

  function onFocusOut(event) {
    if (event.target === nativeSelect) commit(nativeSelect);
    if (event.target === keyboardTarget) {
      keyboardTarget = null;
      scheduleFlush();
    }
  }

  function mount(container) {
    if (root === container) return;
    root?.removeEventListener?.("pointerdown", onPointerDown);
    root?.removeEventListener?.("keydown", onKeyDown);
    root?.removeEventListener?.("keyup", onKeyUp);
    root?.removeEventListener?.("change", onChange);
    root?.removeEventListener?.("focusout", onFocusOut);
    root = container;
    root?.addEventListener?.("pointerdown", onPointerDown);
    root?.addEventListener?.("keydown", onKeyDown);
    root?.addEventListener?.("keyup", onKeyUp);
    root?.addEventListener?.("change", onChange);
    root?.addEventListener?.("focusout", onFocusOut);
  }

  function beforeRender(context) {
    if (isHeld()) {
      queued = true;
      return false;
    }
    captureDisclosure();
    const active = globalThis.document?.activeElement;
    focusKey =
      root?.contains?.(active) && active?.dataset?.viewControl
        ? active.dataset.viewControl
        : "";
    renderedContext = String(context ?? "");
    return true;
  }

  function afterRender(context) {
    renderedContext = String(context ?? "");
    root
      ?.querySelectorAll?.("details[data-view-disclosure]")
      .forEach((node) => {
        const key = disclosureStateKey(
          renderedContext,
          node.dataset.viewDisclosure,
        );
        if (disclosure.has(key)) node.open = disclosure.get(key);
        node.ontoggle = () => disclosure.set(key, node.open);
      });
    if (focusKey) {
      const key = focusKey.replaceAll('"', '\\"');
      root?.querySelector?.(`[data-view-control="${key}"]`)?.focus?.();
    }
    focusKey = "";
  }

  function commit(control = nativeSelect, { flush = true } = {}) {
    if (control && nativeSelect && control !== nativeSelect) return;
    nativeSelect = null;
    nativeSelectWatch++;
    if (flush) scheduleFlush();
    else queued = false;
  }

  function settle() {
    scheduleFlush();
  }

  function interrupt({ capture = true } = {}) {
    if (capture) captureDisclosure();
    pointer = null;
    pointerReleasePending = false;
    keyboardTarget = null;
    nativeSelect = null;
    nativeSelectWatch++;
    queued = false;
    scheduled = false;
    focusKey = "";
  }

  globalThis.addEventListener?.("pointerup", releasePointer);
  globalThis.addEventListener?.("pointercancel", releasePointer);
  globalThis.addEventListener?.("blur", (event) => {
    releasePointer(event);
    keyboardTarget = null;
    if (nativeSelect) commit(nativeSelect);
    scheduleFlush();
  });

  return {
    mount,
    beforeRender,
    afterRender,
    commit,
    settle,
    interrupt,
  };
}
