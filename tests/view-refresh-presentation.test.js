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
  globalThis.addEventListener = (type, listener) =>
    listeners.set(type, listener);
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
    matches(selector) {
      return selector === "[data-view-scroll]" && "viewScroll" in this.dataset;
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

// Replaces the window listeners the presentation registers for this test.
function windowListeners(t) {
  const previous = globalThis.addEventListener;
  const listeners = new Map();
  globalThis.addEventListener = (type, listener) =>
    listeners.set(type, listener);
  t.after(() => {
    if (previous) globalThis.addEventListener = previous;
    else delete globalThis.addEventListener;
  });
  return listeners;
}

// A view with one marked container, already rendered once.
function scrolledView(t) {
  const advance = fakeTimers(t);
  const listeners = windowListeners(t);
  const view = { advance, listeners, renders: 0 };
  view.detail = fakeScroller("detail", 240);
  view.root = fakeRoot([], [view.detail]);
  view.presentation = createViewRefreshPresentation({
    render() {
      view.renders++;
    },
  });
  view.presentation.mount(view.root);
  view.presentation.afterRender("project-a");
  // A real scroll event follows a change of offset.
  view.scroll = () => {
    view.detail.scrollTop += 10;
    view.root.dispatch("scroll", { target: view.detail });
  };
  return view;
}

test("a scroll set from script with no user input does not hold a repaint", (t) => {
  const view = scrolledView(t);
  const { advance, root, detail, presentation } = view;
  view.scroll();
  assert.equal(presentation.beforeRender("project-a"), true);
  presentation.afterRender("project-a");
  // Input elsewhere in the view is not input on the container.
  root.dispatch("pointerdown", {
    target: { closest: () => null },
    button: 0,
    pointerId: 1,
  });
  view.scroll();
  assert.equal(presentation.beforeRender("project-a"), true);
  presentation.afterRender("project-a");
  // A scroll after a key's intent has lapsed does not hold either.
  root.dispatch("keydown", {
    target: { closest: (s) => (s === "[data-view-scroll]" ? detail : null) },
    key: "PageDown",
  });
  advance(SCROLL_IDLE_MS);
  view.scroll();
  assert.equal(presentation.beforeRender("project-a"), true);
  advance(SCROLL_IDLE_MS * 10);
  assert.equal(view.renders, 0);
});

test("a scrollbar drag holds a repaint until release and the scroll is idle", (t) => {
  const view = scrolledView(t);
  const { advance, listeners, root, detail, presentation } = view;
  // A pointer on the container itself is on its scrollbar.
  root.dispatch("pointerdown", { target: detail, button: 0, pointerId: 4 });
  view.scroll();
  assert.equal(presentation.beforeRender("project-a"), false);
  // The drag pauses past the idle time with the button still down.
  advance(SCROLL_IDLE_MS);
  assert.equal(view.renders, 0);
  // A scroll just before release keeps holding until it is idle.
  view.scroll();
  listeners.get("pointerup")({ type: "pointerup", pointerId: 4 });
  advance(SCROLL_IDLE_MS - 1);
  view.scroll();
  advance(SCROLL_IDLE_MS - 1);
  assert.equal(view.renders, 0);
  advance(1);
  assert.equal(view.renders, 1);
  // After release and idle, a script-set scroll no longer holds.
  advance(SCROLL_IDLE_MS);
  view.scroll();
  assert.equal(presentation.beforeRender("project-a"), true);
});

test("a paused scrollbar press holds a repaint until its pointer is released", (t) => {
  const view = scrolledView(t);
  const { advance, listeners, root, detail, presentation } = view;
  const press = (target, pointerId, button = 0) =>
    root.dispatch("pointerdown", { target, button, pointerId });

  press(detail, 5);
  assert.equal(presentation.beforeRender("project-a"), false);
  advance(SCROLL_IDLE_MS * 4);
  // Another pointer's release does not end this press.
  listeners.get("pointerup")({ type: "pointerup", pointerId: 6 });
  advance(SCROLL_IDLE_MS);
  assert.equal(view.renders, 0);
  listeners.get("pointerup")({ type: "pointerup", pointerId: 5 });
  advance();
  assert.equal(view.renders, 1);
  advance(SCROLL_IDLE_MS * 4);
  assert.equal(view.renders, 1);

  // pointercancel and window blur release the press the same way.
  for (const release of [
    () =>
      listeners.get("pointercancel")({ type: "pointercancel", pointerId: 7 }),
    () => listeners.get("blur")({ type: "blur" }),
  ]) {
    press(detail, 7);
    assert.equal(presentation.beforeRender("project-a"), false);
    advance(SCROLL_IDLE_MS * 2);
    release();
    advance();
    assert.equal(presentation.beforeRender("project-a"), true);
    presentation.afterRender("project-a");
  }
  assert.equal(view.renders, 3);

  // A press inside the content, or with another button, does not hold.
  const content = {
    closest: (s) => (s === "[data-view-scroll]" ? detail : null),
    matches: () => false,
  };
  press(content, 8);
  assert.equal(presentation.beforeRender("project-a"), true);
  presentation.afterRender("project-a");
  press(detail, 9, 2);
  assert.equal(presentation.beforeRender("project-a"), true);
});

test("a key during a held scrollbar press does not end the hold", (t) => {
  const view = scrolledView(t);
  const { advance, listeners, root, detail, presentation } = view;
  root.dispatch("pointerdown", { target: detail, button: 0, pointerId: 3 });
  assert.equal(presentation.beforeRender("project-a"), false);
  // Focus inside the container, then a scroll key on the body.
  root.dispatch("keydown", {
    target: { closest: (s) => (s === "[data-view-scroll]" ? detail : null) },
    key: "a",
  });
  listeners.get("keydown")({ target: {}, key: "PageDown" });
  advance(SCROLL_IDLE_MS * 2);
  assert.equal(view.renders, 0);
  listeners.get("pointerup")({ type: "pointerup", pointerId: 3 });
  advance();
  assert.equal(view.renders, 1);
  advance(SCROLL_IDLE_MS * 4);
  assert.equal(view.renders, 1);
});

test("a keyboard scroll holds a repaint until the scroll is idle", (t) => {
  const view = scrolledView(t);
  const { advance, listeners, root, detail, presentation } = view;
  // Focus inside the container.
  root.dispatch("keydown", {
    target: { closest: (s) => (s === "[data-view-scroll]" ? detail : null) },
    key: "ArrowDown",
  });
  view.scroll();
  assert.equal(presentation.beforeRender("project-a"), false);
  advance(SCROLL_IDLE_MS);
  assert.equal(view.renders, 1);

  // Focus on the document body: a scroll key, not any key, scrolls.
  listeners.get("keydown")({ target: {}, key: "a" });
  view.scroll();
  assert.equal(presentation.beforeRender("project-a"), true);
  presentation.afterRender("project-a");
  listeners.get("keydown")({ target: {}, key: "PageDown" });
  view.scroll();
  assert.equal(presentation.beforeRender("project-a"), false);
  advance(SCROLL_IDLE_MS);
  assert.equal(view.renders, 2);
});

test("momentum scrolls after a touch keep holding a repaint", (t) => {
  const view = scrolledView(t);
  const { advance, root, detail, presentation } = view;
  root.dispatch("touchmove", { target: detail });
  assert.equal(presentation.beforeRender("project-a"), false);
  // The finger lifts; only scroll events follow, each inside the idle time.
  for (let frame = 0; frame < 20; frame++) {
    advance(SCROLL_IDLE_MS - 1);
    view.scroll();
  }
  assert.equal(view.renders, 0);
  advance(SCROLL_IDLE_MS);
  assert.equal(view.renders, 1);
  view.scroll();
  assert.equal(presentation.beforeRender("project-a"), true);
});

test("the scroll that restoring an offset causes does not hold a repaint", (t) => {
  const view = scrolledView(t);
  const { advance, listeners, root, presentation } = view;

  // Back-to-back refreshes each restore the offset, each just after a scroll
  // key, so only the guard keeps the restore's scroll from holding. It may
  // arrive during the write or after afterRender returns.
  let node = null;
  for (let refresh = 0; refresh < 3; refresh++) {
    listeners.get("keydown")({ target: {}, key: "PageDown" });
    assert.equal(presentation.beforeRender("project-a"), true);
    node = fakeScroller("detail", 0, 0, () =>
      root.dispatch("scroll", { target: node }),
    );
    root.scrollers = [node];
    presentation.afterRender("project-a");
    assert.deepEqual(node.writes, [
      ["scrollTop", 240],
      ["scrollLeft", 0],
    ]);
    root.dispatch("scroll", { target: node });
  }
  assert.equal(presentation.beforeRender("project-a"), true);
  presentation.afterRender("project-a");

  // A later scroll of the same container by the user still holds.
  root.dispatch("pointerdown", { target: node, button: 0, pointerId: 2 });
  node.scrollTop = 300;
  listeners.get("pointerup")({ type: "pointerup", pointerId: 2 });
  assert.equal(presentation.beforeRender("project-a"), false);
  advance(SCROLL_IDLE_MS);
  assert.equal(view.renders, 1);
});

test("a scroll of an unmarked container does not hold a repaint", (t) => {
  const advance = fakeTimers(t);
  let renders = 0;
  const unmarked = fakeScroller("", 480);
  delete unmarked.dataset.viewScroll;
  const root = fakeRoot([], [fakeScroller("detail")]);
  const presentation = createViewRefreshPresentation({
    render() {
      renders++;
    },
  });
  presentation.mount(root);
  presentation.afterRender("project-a");
  root.dispatch("scroll", { target: unmarked });
  root.dispatch("scroll", { target: { closest: () => null } });
  assert.equal(presentation.beforeRender("project-a"), true);
  advance(SCROLL_IDLE_MS * 10);
  assert.equal(renders, 0);
});

test("the scroll listener is added and removed in the capture phase", () => {
  const calls = [];
  const record = (root) => {
    const add = root.addEventListener;
    const remove = root.removeEventListener;
    root.addEventListener = function (type, listener, options) {
      if (type === "scroll") calls.push(["add", options?.capture]);
      return add.call(this, type, listener, options);
    };
    root.removeEventListener = function (type, listener, options) {
      if (type === "scroll") calls.push(["remove", options?.capture]);
      return remove.call(this, type, listener, options);
    };
    return root;
  };
  const presentation = createViewRefreshPresentation({ render() {} });
  presentation.mount(record(fakeRoot()));
  presentation.mount(record(fakeRoot()));
  assert.deepEqual(calls, [
    ["add", true],
    ["remove", true],
    ["add", true],
  ]);
});

test("a shared scroll container keeps its offset when the context changes", () => {
  const sharedRail = (top = 0) => {
    const node = fakeScroller("rail", top);
    node.dataset.viewScrollShared = "";
    return node;
  };
  const root = fakeRoot([], [sharedRail(150), fakeScroller("detail", 240)]);
  const presentation = createViewRefreshPresentation({ render() {} });
  presentation.mount(root);
  presentation.afterRender("project-a");

  assert.equal(presentation.beforeRender("project-b"), true);
  const rail = sharedRail();
  const detail = fakeScroller("detail");
  root.scrollers = [rail, detail];
  presentation.afterRender("project-b");
  assert.deepEqual(rail.writes, [
    ["scrollTop", 150],
    ["scrollLeft", 0],
  ]);
  assert.deepEqual(detail.writes, []);
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
