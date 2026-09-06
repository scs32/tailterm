import test from "node:test";
import assert from "node:assert/strict";
import { PaneGroups, leaves, tileLayout } from "../client/pane-layout.js";

test("grouping, moving one pane, and pruning preserve each connection exactly once", () => {
  const m = new PaneGroups();
  m.sync(["a", "b", "c", "d"]);
  m.merge("b", "a");
  m.merge("d", "c");
  assert.equal(m.groups.length, 2);
  m.merge("b", "c", { whole: false, axis: "y" });
  assert.deepEqual(leaves(m.group("a").tree), ["a"]);
  assert.deepEqual(leaves(m.group("c").tree), ["c", "b", "d"]);
  assert.equal(m.merge("b", "d"), false);
  m.detach("b");
  assert.equal(m.groups.length, 3);
  m.merge("c", "a");
  assert.deepEqual(m.groups.flatMap((g) => leaves(g.tree)).sort(), [
    "a",
    "b",
    "c",
    "d",
  ]);
  m.sync(["a", "b", "d"]);
  assert.deepEqual(leaves(m.group("a").tree), ["a", "d"]);
  m.sync(["b", "d"]);
  assert.deepEqual(m.group("d").tree, { tab: "d" });
});

test("nested resizing respects minimum sizes, leaves no overlaps, and adapts to portrait", () => {
  const m = new PaneGroups();
  m.sync(["a", "b", "c"]);
  m.merge("b", "a");
  m.merge("c", "b", { axis: "y" });
  const tree = m.group("a").tree;
  tree.ratio = 0.99;
  for (const [width, height] of [
    [1000, 600],
    [360, 700],
    [180, 160],
  ]) {
    const layout = tileLayout(tree, width, height);
    for (const p of layout.panes) {
      assert.ok(p.width >= 180 && p.height >= 120);
      assert.ok(
        p.x >= 0 &&
          p.y >= 0 &&
          p.x + p.width <= layout.width &&
          p.y + p.height <= layout.height,
      );
      for (const q of layout.panes.filter((q) => q !== p))
        assert.ok(
          p.x + p.width <= q.x ||
            q.x + q.width <= p.x ||
            p.y + p.height <= q.y ||
            q.y + q.height <= p.y,
        );
    }
    assert.equal(
      layout.dividers[0].axis,
      width < 540 || width < height ? "y" : "x",
    );
  }
});

test("directional navigation follows geometry without wrapping or crossing diagonally", async () => {
  const { paneNeighbor } = await import("../client/pane-layout.js");
  const panes = [
    { id: "a", x: 0, y: 0, width: 400, height: 400 },
    { id: "b", x: 404, y: 0, width: 400, height: 198 },
    { id: "c", x: 404, y: 202, width: 400, height: 198 },
  ];
  assert.equal(paneNeighbor(panes, "a", "right"), "b");
  assert.equal(paneNeighbor(panes, "b", "down"), "c");
  assert.equal(paneNeighbor(panes, "c", "up"), "b");
  assert.equal(paneNeighbor(panes, "c", "left"), "a");
  assert.equal(paneNeighbor(panes, "b", "right"), null);
  assert.equal(paneNeighbor(panes, "a", "up"), null);
  const portrait = panes.map((p) => ({
    ...p,
    x: p.y,
    y: p.x,
    width: p.height,
    height: p.width,
  }));
  assert.equal(paneNeighbor(portrait, "a", "down"), "b");
});
test("pane shortcuts require the dedicated modifiers and ignore composition", async () => {
  const { paneDirection } = await import("../client/pane-shortcuts.js");
  assert.equal(
    paneDirection({ key: "ArrowLeft", shiftKey: true, altKey: true }),
    "left",
  );
  assert.equal(
    paneDirection({ key: "ArrowDown", ctrlKey: true, altKey: true }),
    "down",
  );
  assert.equal(paneDirection({ key: "ArrowUp", ctrlKey: true }), null);
  assert.equal(
    paneDirection({ key: "ArrowLeft", metaKey: true, altKey: true }),
    null,
  );
  assert.equal(paneDirection({ key: "ArrowLeft", altKey: true }), null);
  assert.equal(
    paneDirection({
      key: "ArrowUp",
      ctrlKey: true,
      altKey: true,
      isComposing: true,
    }),
    null,
  );
});

test("group appearance survives focus/sync and never transfers to a separated session", () => {
  const m = new PaneGroups();
  m.sync(["a", "b", "c"]);
  m.merge("b", "a");
  m.group("a").decoration = { label: "Parent", color: "violet", fill: "amber" };
  m.group("a").active = "a";
  m.sync(["a", "b", "c"]);
  assert.equal(m.group("b").decoration.label, "Parent");
  m.merge("c", "a");
  m.detach("b");
  assert.equal(m.group("a").decoration.label, "Parent");
  assert.equal(m.group("b").decoration, undefined);
  m.detach("c");
  assert.equal(m.group("a").decoration, undefined);
  m.merge("c", "a");
  assert.equal(
    m.group("a").decoration,
    undefined,
    "new parents start independently",
  );
});

test("swapping grouped panes preserves geometry, identity and every connection", () => {
  const m = new PaneGroups();
  m.sync(["a", "b", "c", "outside"]);
  m.merge("b", "a");
  m.merge("c", "b", { axis: "y" });
  const group = m.group("a");
  group.decoration = { label: "Work" };
  group.tree.ratio = 0.3;
  const before = structuredClone(group);
  const boxes = tileLayout(group.tree, 1200, 800).panes;
  assert.equal(m.swap("a", "c"), true);
  assert.equal(m.group("a"), group);
  assert.deepEqual(group.decoration, before.decoration);
  assert.equal(group.active, before.active);
  for (const box of tileLayout(group.tree, 1200, 800).panes) {
    const previous = boxes.find(
      (p) => p.id === ({ a: "c", c: "a" }[box.id] || box.id),
    );
    assert.deepEqual({ ...box, id: previous.id }, previous);
  }
  assert.equal(m.swap("a", "outside"), false);
  assert.equal(m.swap("a", "a"), false);
  assert.equal(m.swap("missing", "a"), false);
  assert.equal(m.swap("a", "c"), true);
  assert.deepEqual(group, before);
});
