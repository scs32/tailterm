import test from "node:test";
import assert from "node:assert/strict";
import {
  workspaceSnapshot,
  normalizeWorkspace,
  normalizeSessionFontSize,
  sameTarget,
  reconnectable,
  createReconnectController,
} from "../client/workspace-state.js";
import { terminalText, diagnosticStage } from "../client/terminal-extras.js";
import { validateImageFiles, uploadPath } from "../client/image-upload.js";
import { PaneGroups } from "../client/pane-layout.js";

test("workspace snapshots retain order, layout and identity without credentials or terminal data", () => {
  const model = new PaneGroups();
  model.sync(["a", "b", "c"]);
  model.merge("b", "a");
  model.reorder("c", "a");
  const tabs = ["a", "b", "c"].map((id) => ({
    id,
    server: {
      id: "server",
      host: "box",
      port: 22,
      username: "user",
      password: "secret",
    },
    tmux: true,
    session: id,
    target: { id: "$1", created: "1" },
    term: "private output",
    fontSize: { a: 12, b: 18, c: 24 }[id],
  }));
  const snapshot = normalizeWorkspace(
    workspaceSnapshot(tabs, model.groups, "b"),
  );
  assert.equal(snapshot.groups[0].tree.tab, "c");
  assert.equal(snapshot.active, "b");
  assert.deepEqual(
    snapshot.tabs.map((t) => t.fontSize),
    [12, 18, 24],
  );
  assert.equal(snapshot.groups[1].tree.b.tab, "b");
  assert.doesNotMatch(JSON.stringify(snapshot), /secret|private output/);
  assert.ok(sameTarget(snapshot.tabs[0].target, { id: "$1", created: 1 }));
  assert.ok(!sameTarget(snapshot.tabs[0].target, { id: "$1", created: "2" }));
});
test("reconnection is bounded, cancellable, and excludes authentication or missing session failures", async () => {
  const timers = new Map();
  let next = 0,
    attempts = 0;
  const messages = [];
  const controller = createReconnectController({
    eligible: () => true,
    changed: (t, m) => messages.push(m),
    attempt: () => {
      attempts++;
      throw Error("network timeout");
    },
    setTimer: (fn) => {
      timers.set(++next, fn);
      return next;
    },
    clearTimer: (id) => timers.delete(id),
  });
  controller.schedule({ id: "a" });
  for (let i = 0; i < 6; i++) {
    const task = timers.values().next().value;
    timers.clear();
    if (task) await task();
  }
  assert.equal(attempts, 5);
  assert.equal(messages.at(-1), "Reconnect manually");
  controller.schedule({ id: "b" });
  controller.cancel("b");
  assert.equal(timers.size, 0);
  for (const m of [
    "SSH host fingerprint changed",
    "unable to authenticate",
    "original tmux session no longer exists",
    "SSH sign-in cancelled",
  ])
    assert.equal(reconnectable(m), false);
  assert.equal(reconnectable("SSH connection reset by peer"), true);
});
test("scrollback joins wrapped rows and diagnostics distinguish host verification", () => {
  const values = [
    { isWrapped: false, text: "hello " },
    { isWrapped: true, text: "world" },
    { isWrapped: false, text: "next" },
  ];
  const term = {
    buffer: {
      active: {
        length: 3,
        getLine: (i) =>
          values[i] && {
            ...values[i],
            translateToString: (trim) =>
              trim ? values[i].text.trimEnd() : values[i].text,
          },
      },
    },
  };
  assert.equal(terminalText(term), "hello world\nnext\n");
  assert.equal(
    diagnosticStage("host fingerprint changed"),
    "SSH host verification",
  );
});
test("image drops enforce bounds and filenames while preserving spaces in remote paths", () => {
  validateImageFiles([
    { name: "Screenshot 1.png", type: "image/png", size: 4 },
  ]);
  assert.equal(
    uploadPath("/home/user/project files/", "Screenshot 1.png"),
    "/home/user/project files/Screenshot 1.png",
  );
  for (const file of [
    { name: "../bad.png", size: 4, type: "image/png" },
    { name: "x.png", size: 30 * 1024 * 1024, type: "image/png" },
    { name: "script.sh", size: 4, type: "text/plain" },
  ])
    assert.throws(() => validateImageFiles([file]));
  assert.throws(() => uploadPath("relative", "x.png"));
  assert.throws(() => uploadPath("/tmp\nnext", "x.png"));
});

test("Activity ignores identical redraws, row movement and status-only changes", async () => {
  const { screenLines, hasNewText, activityTitle } =
    await import("../client/activity.js");
  const make = (lines) => ({
    rows: lines.length,
    buffer: {
      active: {
        baseY: 0,
        getLine: (i) => ({ translateToString: () => lines[i] }),
      },
    },
  });
  const before = screenLines(make(["hello", "world", "12:00"]), true);
  assert.equal(
    hasNewText(before, screenLines(make(["world", "hello", "12:01"]), true)),
    false,
  );
  assert.equal(hasNewText(before, ["hello", "new output"]), true);
  assert.match(
    activityTitle([{ activity: "New output" }, { activity: "Bell" }]),
    /^\(2\) Bell/,
  );
  assert.equal(activityTitle([]), "Tailterm · Your servers, one workspace");
});

test("connection attention ignores healthy and recovering sessions but preserves actionable failures", async () => {
  const { connectionNeedsAttention } = await import("../client/activity.js");
  assert.equal(
    connectionNeedsAttention({
      status: "Connected",
      activity: "Needs attention",
    }),
    false,
  );
  assert.equal(connectionNeedsAttention({ status: "Connecting" }), false);
  for (const retryMessage of [
    "Reconnecting in 1s",
    "Reconnecting in 16s",
    "Reconnecting...",
  ])
    assert.equal(
      connectionNeedsAttention({ status: "Error", retryMessage }),
      false,
    );
  assert.equal(
    connectionNeedsAttention(
      { status: "Error", retryMessage: "Reconnect manually" },
      true,
    ),
    false,
  );
  assert.equal(
    connectionNeedsAttention({
      status: "Error",
      retryMessage: "Reconnect manually",
    }),
    true,
  );
  assert.equal(
    connectionNeedsAttention({
      status: "Error",
      retryMessage: "Reconnect manually after resolving the error",
    }),
    true,
  );
  assert.equal(connectionNeedsAttention({ status: "Disconnected" }), true);
});

test("tab decoration is bounded and preserved in encrypted workspace snapshots", async () => {
  const { normalizeTabDecoration } =
    await import("../client/tab-decoration.js");
  const decoration = {
    label: "Build",
    emoji: "🔧",
    color: "amber",
    fill: "blue",
    font: "cascadia",
  };
  const t = {
    id: "a",
    server: { id: "s", host: "box", port: 22, username: "me" },
    tmux: true,
    session: "main",
    decoration,
  };
  assert.deepEqual(
    normalizeWorkspace(workspaceSnapshot([t], [], "a")).tabs[0].decoration,
    decoration,
  );
  assert.deepEqual(
    normalizeTabDecoration({
      label: "\x1bhello",
      color: "red; background:url(x)",
      font: "__proto__",
    }),
    { label: "hello", emoji: "", color: "default", fill: "default", font: "" },
  );
});

test("workspace restores independent parent and session decoration", () => {
  const child = {
    label: "Air",
    color: "blue",
    fill: "blue",
    emoji: "🍎",
    font: "cascadia",
  };
  const parent = {
    label: "Machines",
    color: "violet",
    fill: "amber",
    emoji: "📦",
    font: "fira",
  };
  const tabs = ["a", "b"].map((id) => ({
    id,
    server: { id: "s", host: "box", port: 22, username: "me" },
    tmux: true,
    session: "main",
    decoration: child,
  }));
  const groups = [
    {
      tree: {
        id: "split",
        axis: "x",
        ratio: 0.5,
        a: { tab: "a" },
        b: { tab: "b" },
      },
      active: "a",
      decoration: parent,
    },
  ];
  const restored = normalizeWorkspace(workspaceSnapshot(tabs, groups, "a"));
  assert.deepEqual(restored.groups[0].decoration, parent);
  assert.deepEqual(
    restored.tabs.map((t) => t.decoration),
    [child, child],
  );
});

test("session font size rejects invalid persisted values and supports older workspaces", () => {
  for (const value of [undefined, null, "18", 0, 9, 33, 14.5, NaN])
    assert.equal(normalizeSessionFontSize(value), undefined);
  for (const value of [10, 14, 32])
    assert.equal(normalizeSessionFontSize(value), value);
});
