import test from "node:test";
import assert from "node:assert/strict";
import {
  createViewRefreshPresentation,
  disclosureStateKey,
  isNativeSelectActivation,
  shouldReleaseViewPointer,
} from "../client/view-refresh-presentation.js";

const tick = () => new Promise((resolve) => setTimeout(resolve, 5));

function fakeRoot(details = []) {
  const listeners = new Map();
  return {
    details,
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
      return selector === "details[data-view-disclosure]" ? this.details : [];
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

test("a native select queues one repaint until its choice is committed", async () => {
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
  await tick();
  assert.equal(renders, 1);
});

test("a native select commit releases a pointer consumed by the platform picker", async () => {
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
  await tick();
  assert.equal(renders, 1);
});

test("a matching pointerup and commit release an ordinary select gesture", async () => {
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
    await tick();
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

test("a keyboard disclosure press finishes before its queued repaint", async () => {
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
  root.dispatch("keyup", { target: summary, key: " " });
  await tick();
  assert.equal(renders, 1);
});

test("open-state observation releases a same-value native selection", async () => {
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
    await tick();
    assert.equal(renders, 1);
  } finally {
    globalThis.CSS = previousCSS;
    globalThis.requestAnimationFrame = previousFrame;
  }
});

test("an explicit filter commit discards a queued stale repaint", async () => {
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
  await tick();
  assert.equal(renders, 0);
});

test("typing holds a repaint until a short idle boundary", async () => {
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
  await new Promise((resolve) => setTimeout(resolve, 140));
  assert.equal(renders, 1);
});

test("composition holds a repaint until the composition finishes", async () => {
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
  await new Promise((resolve) => setTimeout(resolve, 140));
  assert.equal(renders, 0);
  root.dispatch("compositionend", { target: text });
  await new Promise((resolve) => setTimeout(resolve, 140));
  assert.equal(renders, 1);
});

test("interrupt clears a queued composition before hide and remount", async () => {
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
  await new Promise((resolve) => setTimeout(resolve, 140));
  assert.equal(renders, 0);
});
