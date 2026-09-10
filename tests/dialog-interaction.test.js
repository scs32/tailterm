import test from "node:test";
import assert from "node:assert/strict";
import {
  installDialogSelectInteraction,
  keepDialogOpenForSelectCancel,
  openDialogSelectOnEnter,
} from "../client/dialog-interaction.js";

function fixture({ native = true, open = true } = {}) {
  const calls = [];
  const control = {
    isConnected: true,
    matches(selector) {
      if (selector === ":open") return open;
      return native && selector.includes("select");
    },
    blur() {
      calls.push(["blur"]);
      open = false;
    },
    focus(options) {
      calls.push(["focus", options]);
    },
  };
  const dialog = {
    open: true,
    ownerDocument: { activeElement: control },
    contains(candidate) {
      return candidate === control;
    },
  };
  const event = {
    preventDefault() {
      calls.push(["preventDefault"]);
    },
  };
  return {
    calls,
    control,
    dialog,
    event,
    setOpen(value) {
      open = value;
    },
  };
}

test("an open native select consumes dialog cancellation and restores focus", () => {
  const previousFrame = globalThis.requestAnimationFrame;
  const frames = [];
  globalThis.requestAnimationFrame = (callback) => frames.push(callback);
  try {
    const state = fixture();
    assert.equal(
      keepDialogOpenForSelectCancel(state.event, state.dialog),
      true,
    );
    assert.deepEqual(state.calls, [["preventDefault"], ["blur"]]);
    frames.shift()();
    assert.deepEqual(state.calls.at(-1), ["focus", { preventScroll: true }]);
  } finally {
    globalThis.requestAnimationFrame = previousFrame;
  }
});

test("ordinary dialog cancellation is not intercepted", () => {
  for (const state of [fixture({ open: false }), fixture({ native: false })]) {
    assert.equal(
      keepDialogOpenForSelectCancel(state.event, state.dialog),
      false,
    );
    assert.deepEqual(state.calls, []);
  }
});

test("the installed handler does not cancel an ordinary dialog Escape", () => {
  const state = fixture({ open: false });
  installDialogSelectInteraction(state.dialog);
  assert.equal(state.dialog.oncancel(state.event), undefined);
  assert.deepEqual(state.calls, []);
});

test("Enter opens a closed dialog select instead of submitting its form", () => {
  const state = fixture({ open: false });
  state.control.disabled = false;
  state.control.closest = (selector) =>
    selector.includes("select") ? state.control : null;
  state.control.showPicker = () => {
    state.calls.push(["showPicker"]);
    state.setOpen(true);
  };
  state.control.click = () => state.calls.push(["click"]);
  const event = {
    key: "Enter",
    target: state.control,
    preventDefault: state.event.preventDefault,
  };
  assert.equal(openDialogSelectOnEnter(event, state.dialog), true);
  assert.deepEqual(state.calls, [["preventDefault"], ["showPicker"]]);
});

test("Enter from an open picker and keys from other controls retain defaults", () => {
  const open = fixture();
  open.control.disabled = false;
  open.control.closest = () => open.control;
  assert.equal(
    openDialogSelectOnEnter(
      { key: "Enter", target: open.control, preventDefault() {} },
      open.dialog,
    ),
    false,
  );
  assert.equal(
    openDialogSelectOnEnter(
      { key: "Escape", target: open.control, preventDefault() {} },
      open.dialog,
    ),
    false,
  );
});

test("Enter falls back to a programmatic click when showPicker is unavailable", () => {
  const state = fixture({ open: false });
  state.control.disabled = false;
  state.control.closest = () => state.control;
  state.control.click = () => state.calls.push(["click"]);
  assert.equal(
    openDialogSelectOnEnter(
      {
        key: "Enter",
        target: state.control,
        preventDefault: state.event.preventDefault,
      },
      state.dialog,
    ),
    true,
  );
  assert.deepEqual(state.calls, [["preventDefault"], ["click"]]);
});

test("Enter falls back to a programmatic click when showPicker rejects the request", () => {
  const state = fixture({ open: false });
  state.control.disabled = false;
  state.control.closest = () => state.control;
  state.control.showPicker = () => {
    throw new Error("picker unavailable");
  };
  state.control.click = () => state.calls.push(["click"]);
  assert.equal(
    openDialogSelectOnEnter(
      {
        key: "Enter",
        target: state.control,
        preventDefault: state.event.preventDefault,
      },
      state.dialog,
    ),
    true,
  );
  assert.deepEqual(state.calls, [["preventDefault"], ["click"]]);
});

test("a closed dialog or disconnected control is not refocused", () => {
  const previousFrame = globalThis.requestAnimationFrame;
  const frames = [];
  globalThis.requestAnimationFrame = (callback) => frames.push(callback);
  try {
    const closed = fixture();
    assert.equal(
      keepDialogOpenForSelectCancel(closed.event, closed.dialog),
      true,
    );
    closed.dialog.open = false;
    frames.shift()();
    assert.equal(
      closed.calls.some(([name]) => name === "focus"),
      false,
    );

    const disconnected = fixture();
    assert.equal(
      keepDialogOpenForSelectCancel(disconnected.event, disconnected.dialog),
      true,
    );
    disconnected.control.isConnected = false;
    frames.shift()();
    assert.equal(
      disconnected.calls.some(([name]) => name === "focus"),
      false,
    );
  } finally {
    globalThis.requestAnimationFrame = previousFrame;
  }
});
