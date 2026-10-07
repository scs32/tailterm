import test from "node:test";
import assert from "node:assert/strict";
import {
  createViewRefreshPresentation,
  disclosureStateKey,
  isNativeSelectActivation,
  shouldReleaseViewPointer,
} from "../client/view-refresh-presentation.js";

// Mirrors TEXT_INTERACTION_IDLE_MS in the presentation.
const TEXT_IDLE_MS = 120;
// Mirrors SCROLL_GESTURE_IDLE_MS in the presentation.
const SCROLL_IDLE_MS = 150;

// The presentation defers repaints with setTimeout. Mocked timers let each test
// advance that clock itself, so no assertion depends on host scheduling. The
// returned function runs every timer due within ms, including timers queued by
// the ones it runs; with no argument it drains the zero-delay chain.
function fakeTimers(t) {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  return (ms = 0) => t.mock.timers.tick(ms);
}

function fakeRoot(details = [], scrollers = []) {
  const listeners = new Map();
  return {
    details,
    scrollers,
    addEventListener(type, listener) {
      listeners.set(type, listener);
    },
    removeEventListener(type) {
      listeners.delete(type);
    },
    dispatch(type, event) {
      listeners.get(type)?.(event);
    },
    querySelector(selector) {
      if (selector === ":popover-open") return null;
      return null;
    },
    querySelectorAll(selector) {
      if (selector === "details[data-view-disclosure]") return this.details;
      return selector === "[data-view-scroll]" ? this.scrollers : [];
    },
    contains() {
      return false;
    },
  };
}

test("refresh pointer release follows the initiating pointer", () => {
  assert.equal(
    shouldReleaseViewPointer(7, { type: "pointerup", pointerId: 7 }),
    true,
  );
  assert.equal(
    shouldReleaseViewPointer(7, { type: "pointercancel", pointerId: 7 }),
    true,
  );
  assert.equal(shouldReleaseViewPointer(7, { type: "blur" }), true);
  assert.equal(
    shouldReleaseViewPointer(7, { type: "pointerup", pointerId: 8 }),
    false,
  );
});

test("native select activation includes pointer-equivalent keyboard actions", () => {
  for (const key of [" ", "Enter", "ArrowDown", "ArrowUp"])
    assert.equal(isNativeSelectActivation({ key }), true);
  for (const key of ["Escape", "Tab", "a"])
    assert.equal(isNativeSelectActivation({ key }), false);
});

test("manual disclosure state is retained per view context", () => {
  let node = {
    dataset: { viewDisclosure: "closed" },
    open: true,
    ontoggle: null,
  };
  const root = fakeRoot([node]);
  const presentation = createViewRefreshPresentation({ render() {} });
  presentation.mount(root);
  assert.equal(presentation.beforeRender("project-a"), true);
  presentation.afterRender("project-a");
  node.open = false;
  node.ontoggle();

  assert.equal(presentation.beforeRender("project-a"), true);
  node = {
    dataset: { viewDisclosure: "closed" },
    open: true,
    ontoggle: null,
  };
  root.details = [node];
  presentation.afterRender("project-a");
  assert.equal(node.open, false);

  assert.equal(presentation.beforeRender("project-b"), true);
  node = {
    dataset: { viewDisclosure: "closed" },
    open: true,
    ontoggle: null,
  };
  root.details = [node];
  presentation.afterRender("project-b");
  assert.equal(node.open, true);
  assert.notEqual(
    disclosureStateKey("project-a", "closed"),
    disclosureStateKey("project-b", "closed"),
  );
});

test("a native select queues one repaint until its choice is committed", (t) => {
  const advance = fakeTimers(t);
  let renders = 0;
  const root = fakeRoot();
  const presentation = createViewRefreshPresentation({
    render() {
      renders++;
    },
  });
  const select = {
    disabled: false,
    isConnected: true,
    closest(selector) {
      return selector === "select" ? this : null;
    },
  };
  presentation.mount(root);
  root.dispatch("keydown", { target: select, key: "Enter" });
  assert.equal(presentation.beforeRender("project-a"), false);
  assert.equal(presentation.beforeRender("project-a"), false);
  root.dispatch("change", { target: select });
  assert.equal(renders, 0);
  advance();
  assert.equal(renders, 1);
});

test("a native select commit releases a pointer consumed by the platform picker", (t) => {
  const advance = fakeTimers(t);
  let renders = 0;
  const root = fakeRoot();
  const presentation = createViewRefreshPresentation({
    render() {
      renders++;
    },
  });
  const select = {
    disabled: false,
    isConnected: true,
    closest(selector) {
      return selector.includes("select") ? this : null;
    },
    matches(selector) {
      return selector === "select";
    },
  };
  presentation.mount(root);
  root.dispatch("pointerdown", {
    target: select,
    button: 0,
    pointerId: 7,
  });
  assert.equal(presentation.beforeRender("project-a"), false);
  root.dispatch("change", { target: select });
  assert.equal(renders, 0);
  advance();
  assert.equal(renders, 1);
});

test("a matching pointerup and commit release an ordinary select gesture", (t) => {
  const advance = fakeTimers(t);
  const previousAddEventListener = globalThis.addEventListener;
  const listeners = new Map();
  globalThis.addEventListener = (type, listener) => listeners.set(type, listener);
  try {
    let renders = 0;
    const root = fakeRoot();
    const presentation = createViewRefreshPresentation({
      render() {
        renders++;
      },
    });
    const select = {
      disabled: false,
      isConnected: true,
      closest(selector) {
        return selector.includes("select") ? this : null;
      },
      matches(selector) {
        return selector === "select";
      },
    };
    presentation.mount(root);
    root.dispatch("pointerdown", {
      target: select,
      button: 0,
      pointerId: 7,
    });
    assert.equal(presentation.beforeRender("project-a"), false);
    listeners.get("pointerup")({ type: "pointerup", pointerId: 7 });
    root.dispatch("change", { target: select });
    assert.equal(renders, 0);
    advance();
    assert.equal(renders, 1);
  } finally {
    if (previousAddEventListener)
      globalThis.addEventListener = previousAddEventListener;
    else delete globalThis.addEventListener;
  }
});

test("focus alone does not claim that a native popup is open", () => {
  const root = fakeRoot();
  const presentation = createViewRefreshPresentation({ render() {} });
  presentation.mount(root);
  assert.equal(presentation.beforeRender("project-a"), true);
});

test("focused controls are restored without moving their scroll container", () => {
  const previousDocument = globalThis.document;
  const active = { dataset: { viewControl: "status" } };
  let focusOptions = null;
  const replacement = {
    focus(options) {
      focusOptions = options;
    },
  };
  const root = fakeRoot();
  root.contains = (node) => node === active;
  root.querySelector = (selector) =>
    selector === '[data-view-control="status"]' ? replacement : null;
  globalThis.document = { activeElement: active };
  try {
    const presentation = createViewRefreshPresentation({ render() {} });
    presentation.mount(root);
    assert.equal(presentation.beforeRender("project-a"), true);
    presentation.afterRender("project-a");
    assert.deepEqual(focusOptions, { preventScroll: true });
  } finally {
    if (previousDocument) globalThis.document = previousDocument;
    else delete globalThis.document;
  }
});

test("a keyboard disclosure press finishes before its queued repaint", (t) => {
  const advance = fakeTimers(t);
  let renders = 0;
  const root = fakeRoot();
  const presentation = createViewRefreshPresentation({
    render() {
      renders++;
    },
  });
  const summary = {
    disabled: false,
    isConnected: true,
    closest(selector) {
      return selector.includes("summary") ? this : null;
    },
  };
  presentation.mount(root);
  root.dispatch("keydown", { target: summary, key: " " });
  assert.equal(presentation.beforeRender("project-a"), false);
  presentation.settle();
  advance();
  assert.equal(renders, 0);
  root.dispatch("keyup", { target: summary, key: " " });
  assert.equal(renders, 0);
  advance();
  assert.equal(renders, 1);
});

test("open-state observation releases a same-value native selection", (t) => {
  const advance = fakeTimers(t);
  const previousCSS = globalThis.CSS;
  const previousFrame = globalThis.requestAnimationFrame;
  const frames = [];
  let open = true;
  let renders = 0;
  globalThis.CSS = { supports: () => true };
  globalThis.requestAnimationFrame = (callback) => frames.push(callback);
  try {
    const root = fakeRoot();
    const presentation = createViewRefreshPresentation({
      render() {
        renders++;
      },
    });
    const select = {
      disabled: false,
      isConnected: true,
      closest(selector) {
        return selector === "select" ? this : null;
      },
      matches(selector) {
        return selector === ":open" ? open : selector === "select";
      },
    };
    presentation.mount(root);
    root.dispatch("keydown", { target: select, key: "Enter" });
    assert.equal(presentation.beforeRender("project-a"), false);
    frames.shift()();
    open = false;
    frames.shift()();
    assert.equal(renders, 0);
    advance();
    assert.equal(renders, 1);
  } finally {
    globalThis.CSS = previousCSS;
    globalThis.requestAnimationFrame = previousFrame;
  }
});

test("an explicit filter commit discards a queued stale repaint", (t) => {
  const advance = fakeTimers(t);
  let renders = 0;
  const root = fakeRoot();
  const presentation = createViewRefreshPresentation({
    render() {
      renders++;
    },
  });
  const select = {
    disabled: false,
    isConnected: true,
    closest(selector) {
      return selector === "select" ? this : null;
    },
  };
  presentation.mount(root);
  root.dispatch("keydown", { target: select, key: "ArrowDown" });
  assert.equal(presentation.beforeRender("project-a"), false);
  presentation.commit(select, { flush: false });
  presentation.settle();
  advance(TEXT_IDLE_MS * 10);
  assert.equal(renders, 0);
});

test("typing holds a repaint until a short idle boundary", (t) => {
  const advance = fakeTimers(t);
  let renders = 0;
  const root = fakeRoot();
  const presentation = createViewRefreshPresentation({
    render() {
      renders++;
    },
  });
  const text = {
    isConnected: true,
    closest(selector) {
      return selector.includes("textarea") ? this : null;
    },
  };
  presentation.mount(root);
  root.dispatch("input", { target: text });
  assert.equal(presentation.beforeRender("project-a"), false);
  advance(TEXT_IDLE_MS - 1);
  assert.equal(renders, 0);
  advance(1);
  assert.equal(renders, 1);
});

test("composition holds a repaint until the composition finishes", (t) => {
  const advance = fakeTimers(t);
  let renders = 0;
  const root = fakeRoot();
  const presentation = createViewRefreshPresentation({
    render() {
      renders++;
    },
  });
  const text = {
    isConnected: true,
    closest(selector) {
      return selector.includes("textarea") ? this : null;
    },
  };
  presentation.mount(root);
  root.dispatch("compositionstart", { target: text });
  assert.equal(presentation.beforeRender("project-a"), false);
  presentation.settle();
  advance(TEXT_IDLE_MS * 10);
  assert.equal(renders, 0);
  root.dispatch("compositionend", { target: text });
  advance(TEXT_IDLE_MS - 1);
  assert.equal(renders, 0);
  advance(1);
  assert.equal(renders, 1);
});

test("interrupt clears a queued composition before hide and remount", (t) => {
  const advance = fakeTimers(t);
  let renders = 0;
  const oldRoot = fakeRoot();
  const nextRoot = fakeRoot();
  const presentation = createViewRefreshPresentation({
    render() {
      renders++;
    },
  });
  const text = {
    isConnected: true,
    closest(selector) {
      return selector.includes("textarea") ? this : null;
    },
  };
  presentation.mount(oldRoot);
  oldRoot.dispatch("compositionstart", { target: text });
  assert.equal(presentation.beforeRender("project-a"), false);
  presentation.interrupt();
  presentation.mount(nextRoot);
  assert.equal(presentation.beforeRender("project-a"), true);
  presentation.settle();
  advance(TEXT_IDLE_MS * 10);
  assert.equal(renders, 0);
});

// A marked scroll container that records every offset the presentation writes.
function fakeScroller(name, top = 0, left = 0, onWrite = () => {}) {
  const node = {
    dataset: { viewScroll: name },
    writes: [],
    closest(selector) {
      return selector === "[data-view-scroll]" ? this : null;
    },
  };
  for (const [key, value] of [
    ["scrollTop", top],
    ["scrollLeft", left],
  ]) {
    let current = value;
    Object.defineProperty(node, key, {
      get: () => current,
      set(next) {
        current = next;
        node.writes.push([key, next]);
        onWrite(key);
      },
    });
  }
  return node;
}

test("marked scroll containers keep their offsets across a repaint", () => {
  const order = [];
  const disclosure = {
    dataset: { viewDisclosure: "usage" },
    ontoggle: null,
    get open() {
      return true;
    },
    set open(value) {
      order.push("disclosure");
    },
  };
  const root = fakeRoot(
    [disclosure],
    [fakeScroller("detail", 240, 12), fakeScroller("rail", 90)],
  );
  const presentation = createViewRefreshPresentation({ render() {} });
  presentation.mount(root);
  presentation.afterRender("project-a");
  order.length = 0;

  assert.equal(presentation.beforeRender("project-a"), true);
  const detail = fakeScroller("detail", 0, 0, (key) => order.push(key));
  const rail = fakeScroller("rail");
  const unsaved = fakeScroller("other");
  root.scrollers = [detail, rail, unsaved];
  presentation.afterRender("project-a");
  assert.deepEqual(detail.writes, [
    ["scrollTop", 240],
    ["scrollLeft", 12],
  ]);
  assert.deepEqual(rail.writes, [
    ["scrollTop", 90],
    ["scrollLeft", 0],
  ]);
  assert.deepEqual(unsaved.writes, []);
  assert.deepEqual(order, ["disclosure", "scrollTop", "scrollLeft"]);

  // The saved offsets serve one repaint only.
  const later = fakeScroller("detail");
  root.scrollers = [later];
  presentation.afterRender("project-a");
  assert.deepEqual(later.writes, []);
});

test("a changed context or an interrupt starts scroll at the top", () => {
  const root = fakeRoot([], [fakeScroller("detail", 240)]);
  const presentation = createViewRefreshPresentation({ render() {} });
  presentation.mount(root);
  presentation.afterRender("project-a");

  assert.equal(presentation.beforeRender("project-b"), true);
  let next = fakeScroller("detail");
  root.scrollers = [next];
  presentation.afterRender("project-b");
  assert.deepEqual(next.writes, []);

  root.scrollers = [fakeScroller("detail", 240)];
  assert.equal(presentation.beforeRender("project-b"), true);
  presentation.interrupt();
  next = fakeScroller("detail");
  root.scrollers = [next];
  presentation.afterRender("project-b");
  assert.deepEqual(next.writes, []);
});

test("a scroll container at the top is never written", () => {
  const root = fakeRoot([], [fakeScroller("detail")]);
  const presentation = createViewRefreshPresentation({ render() {} });
  presentation.mount(root);
  presentation.afterRender("project-a");
  assert.equal(presentation.beforeRender("project-a"), true);
  const next = fakeScroller("detail");
  root.scrollers = [next];
  presentation.afterRender("project-a");
  assert.deepEqual(next.writes, []);
});

test("a wheel or touch scroll over a marked container holds a repaint until idle", (t) => {
  const advance = fakeTimers(t);
  let renders = 0;
  const root = fakeRoot([], [fakeScroller("detail", 240)]);
  const presentation = createViewRefreshPresentation({
    render() {
      renders++;
    },
  });
  presentation.mount(root);
  const inside = { closest: () => root.scrollers[0] };
  const outside = { closest: () => null };

  root.dispatch("wheel", { target: outside });
  assert.equal(presentation.beforeRender("project-a"), true);
  presentation.afterRender("project-a");

  root.dispatch("wheel", { target: inside });
  assert.equal(presentation.beforeRender("project-a"), false);
  advance(SCROLL_IDLE_MS - 1);
  // Each further event of the gesture restarts the idle time.
  root.dispatch("touchmove", { target: inside });
  assert.equal(presentation.beforeRender("project-a"), false);
  advance(SCROLL_IDLE_MS - 1);
  assert.equal(renders, 0);
  advance(1);
  assert.equal(renders, 1);
  assert.equal(presentation.beforeRender("project-a"), true);

  root.dispatch("wheel", { target: inside });
  presentation.interrupt();
  assert.equal(presentation.beforeRender("project-a"), true);
  advance(SCROLL_IDLE_MS * 10);
  assert.equal(renders, 1);
});

// Board and work-items mark no container: they keep their own scroll handling,
// so the presentation must neither write an offset nor hold a repaint there.
function unmarkedViewKeepsItsBehaviour(t, context) {
  const advance = fakeTimers(t);
  let renders = 0;
  const scrolled = fakeScroller("", 480);
  delete scrolled.dataset.viewScroll;
  scrolled.closest = () => null;
  const root = fakeRoot();
  const selectors = [];
  const queryAll = root.querySelectorAll;
  root.querySelectorAll = function (selector) {
    selectors.push(selector);
    return queryAll.call(this, selector);
  };
  const presentation = createViewRefreshPresentation({
    render() {
      renders++;
    },
  });
  presentation.mount(root);
  presentation.afterRender(context);
  for (const type of ["wheel", "touchmove"]) {
    root.dispatch(type, { target: scrolled });
    assert.equal(presentation.beforeRender(context), true);
    presentation.afterRender(context);
  }
  advance(SCROLL_IDLE_MS * 10);
  assert.equal(renders, 0);
  assert.deepEqual(scrolled.writes, []);
  assert.ok(selectors.includes("[data-view-scroll]"));
}

test("Board shape: no marked container means no scroll write and no hold", (t) => {
  unmarkedViewKeepsItsBehaviour(t, "tsk_1111111111111111");
});

test("work-items shape: no marked container means no scroll write and no hold", (t) => {
  const scope = "";
  unmarkedViewKeepsItsBehaviour(t, scope || "all");
});
