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

test("task groups separate legacy mixed panes and gather newly spawned agents", () => {
  const m = new PaneGroups();
  m.sync(["shell", "planner", "other", "helper"]);
  m.merge("planner", "shell");
  m.merge("other", "shell");
  m.group("shell").taskId = "review";
  const owner = (id) =>
    ({ planner: "review", helper: "review", other: "build" })[id];
  m.isolateTasks(owner);
  assert.deepEqual(leaves(m.taskGroup("review").tree).sort(), [
    "helper",
    "planner",
  ]);
  assert.deepEqual(leaves(m.taskGroup("build").tree), ["other"]);
  assert.deepEqual(leaves(m.group("shell").tree), ["shell"]);
  assert.equal(m.group("shell").taskId, undefined);
  assert.equal(m.merge("shell", "planner"), false);
  assert.equal(m.merge("other", "planner"), false);
  assert.equal(m.detach("planner"), false);
  m.sync(["shell", "planner", "other", "helper", "new"]);
  const nextOwner = (id) => (id === "new" ? "review" : owner(id));
  m.isolateTasks(nextOwner);
  const saved = structuredClone(m.groups);
  m.isolateTasks(nextOwner);
  assert.deepEqual(
    m.groups,
    saved,
    "sync must preserve layout and group identity",
  );
  assert.equal(m.groups.length, 3);
  assert.deepEqual(leaves(m.taskGroup("review").tree).sort(), [
    "helper",
    "new",
    "planner",
  ]);
});

test("ordinary session guests survive task reconciliation and can leave without moving agents", () => {
  const m = new PaneGroups();
  const owner = (id) =>
    ({ planner: "review", reviewer: "review", builder: "build" })[id];
  m.sync(["shell", "planner", "reviewer", "builder"], owner);
  m.isolateTasks(owner);
  assert.equal(
    m.merge("shell", "planner"),
    false,
    "a whole group never merges into a task",
  );
  assert.equal(m.merge("shell", "planner", { whole: false }), true);
  m.sync(["shell", "planner", "reviewer", "builder"], owner);
  m.isolateTasks(owner);
  assert.equal(m.group("shell").taskId, "review");
  assert.equal(m.canDetach("planner"), false);
  assert.equal(m.canDetach("shell"), true);
  assert.equal(m.merge("planner", "builder", { whole: false }), false);
  assert.equal(m.merge("planner", "builder"), false);
  assert.equal(m.reorder("planner", "builder"), true);
  assert.equal(m.detach("shell"), true);
  assert.equal(m.group("shell").taskId, undefined);
  assert.deepEqual(m.taskGroup("review").guests, []);
});

test("task agents grow into two worker columns while the orchestrator stays full-height", () => {
  const model = new PaneGroups();
  const ids = [
    "lead",
    "second",
    "third",
    "fourth",
    "fifth",
    "sixth",
    "seventh",
  ];
  for (let count = 1; count <= ids.length; count++) {
    model.sync(ids.slice(0, count), () => "task");
    model.setTaskOrchestrator("task", "lead");
    model.isolateTasks(() => "task");
    const group = model.taskGroup("task");
    const { panes } = tileLayout(group.tree, 1200, 800);
    const byId = Object.fromEntries(panes.map((pane) => [pane.id, pane]));
    assert.equal(byId.lead.x, 0);
    assert.equal(byId.lead.y, 0);
    assert.equal(byId.lead.height, 800);
    assert.equal(new Set(panes.map((pane) => pane.x)).size, Math.min(count, 3));
    if (count >= 4) {
      assert.equal(byId.second.x, byId.fourth.x);
      assert.ok(byId.fourth.y > byId.second.y);
    }
    if (count >= 5) {
      assert.equal(byId.third.x, byId.fifth.x);
      assert.ok(byId.fifth.y > byId.third.y);
    }
    assert.deepEqual(leaves(group.tree).sort(), ids.slice(0, count).sort());
    const saved = structuredClone(model.groups);
    model.isolateTasks(() => "task");
    assert.deepEqual(
      model.groups,
      saved,
      "repeated sync preserves divider IDs and focus",
    );
  }
});

test("task layout uses the named orchestrator even when it arrives after workers", () => {
  const model = new PaneGroups();
  model.sync(
    ["worker1", "worker2", "lead", "worker3", "worker4"],
    () => "task",
  );
  model.isolateTasks(() => "task");
  model.setTaskOrchestrator("task", "lead");
  model.isolateTasks(() => "task");
  const panes = tileLayout(model.taskGroup("task").tree, 1200, 800).panes;
  assert.equal(panes.find((p) => p.id === "lead").height, 800);
  assert.equal(panes.find((p) => p.id === "lead").x, 0);
  assert.equal(
    panes.find((p) => p.id === "worker1").x,
    panes.find((p) => p.id === "worker3").x,
  );
  assert.equal(
    panes.find((p) => p.id === "worker2").x,
    panes.find((p) => p.id === "worker4").x,
  );
});

test("task layout migrates untouched horizontal groups and preserves customized layouts", async () => {
  const { normalizeWorkspace, workspaceSnapshot, endpointKey } =
    await import("../client/workspace-state.js");
  const taskId = "tsk_0123456789abcdef";
  const ids = ["lead", "two", "three", "four", "five"];
  const model = new PaneGroups();
  model.sync(ids);
  model.groups = [
    {
      active: "lead",
      tree: ids.slice(1).reduce(
        (tree, tab) => ({
          id: crypto.randomUUID(),
          axis: "x",
          ratio: 0.5,
          a: tree,
          b: { tab },
        }),
        { tab: ids[0] },
      ),
    },
  ];
  model.isolateTasks(() => taskId);
  assert.equal(model.taskGroup(taskId).taskLayout, "auto");
  assert.equal(
    tileLayout(model.taskGroup(taskId).tree, 1200, 800).panes.find(
      (p) => p.id === "lead",
    ).height,
    800,
  );
  model.customize("lead");
  model.taskGroup(taskId).tree.ratio = 0.42;
  model.swap("two", "four");
  const before = structuredClone(model.taskGroup(taskId).tree);
  const server = { id: "server", host: "fixture", port: 22, username: "test" };
  const tabs = ids.map((id, i) => ({
    id,
    server,
    serverId: server.id,
    endpoint: endpointKey(server),
    task: { taskId, agentId: `agt_${String(i).padStart(16, "0")}` },
  }));
  const saved = normalizeWorkspace(
    workspaceSnapshot(tabs, model.groups, "lead"),
  );
  assert.equal(saved.groups[0].taskLayout, "manual");
  model.groups = saved.groups;
  model.sync(ids, () => taskId);
  model.isolateTasks(() => taskId);
  assert.deepEqual(model.taskGroup(taskId).tree, before);
  model.sync([...ids, "six"], () => taskId);
  model.isolateTasks(() => taskId);
  assert.deepEqual(
    model.taskGroup(taskId).tree.a,
    before,
    "adding a pane preserves the customized subtree",
  );
});

test("legacy task layouts with manual swaps are preserved during migration", () => {
  const model = new PaneGroups();
  model.sync(["lead", "two", "three"]);
  const saved = {
    id: "outer",
    axis: "x",
    ratio: 0.5,
    a: {
      id: "inner",
      axis: "x",
      ratio: 0.5,
      a: { tab: "lead" },
      b: { tab: "three" },
    },
    b: { tab: "two" },
  };
  model.groups = [{ tree: saved, active: "lead", taskId: "task" }];
  model.isolateTasks(() => "task");
  assert.equal(model.taskGroup("task").taskLayout, "manual");
  assert.deepEqual(model.taskGroup("task").tree, saved);
});

test("directional placement reparents one pane and preserves unrelated splits", () => {
  const model = new PaneGroups();
  model.sync(["source", "target", "other"]);
  model.groups = [
    {
      taskId: "task",
      taskLayout: "auto",
      active: "target",
      tree: {
        id: "removed-parent",
        axis: "x",
        ratio: 0.37,
        a: { tab: "source" },
        b: {
          id: "preserved-split",
          axis: "y",
          ratio: 0.62,
          a: { tab: "target" },
          b: { tab: "other" },
        },
      },
    },
  ];

  assert.equal(model.place("source", "target", "right"), true);
  let tree = model.taskGroup("task").tree;
  assert.equal(tree.id, "preserved-split");
  assert.equal(tree.ratio, 0.62);
  assert.equal(tree.a.axis, "x");
  assert.deepEqual([tree.a.a.tab, tree.a.b.tab], ["target", "source"]);
  assert.equal(tree.b.tab, "other");
  assert.equal(model.taskGroup("task").taskLayout, "manual");
  assert.equal(model.taskGroup("task").active, "source");

  assert.equal(model.place("source", "other", "above"), true);
  tree = model.taskGroup("task").tree;
  assert.equal(tree.id, "preserved-split");
  assert.equal(tree.ratio, 0.62);
  assert.equal(tree.a.tab, "target");
  assert.equal(tree.b.axis, "y");
  assert.deepEqual([tree.b.a.tab, tree.b.b.tab], ["source", "other"]);
  assert.deepEqual(leaves(tree).sort(), ["other", "source", "target"]);
});

test("directional placement respects task ownership and guest rules", () => {
  const model = new PaneGroups();
  const owner = (id) => ({ lead: "one", peer: "one", other: "two" })[id];
  model.sync(["lead", "peer", "other", "shell"], owner);
  model.isolateTasks(owner);

  assert.equal(model.place("lead", "other", "right"), false);
  assert.equal(model.place("lead", "peer", "right"), true);
  assert.equal(model.taskGroup("one").taskLayout, "manual");
  assert.equal(model.place("shell", "lead", "above"), true);
  assert.deepEqual(model.taskGroup("one").guests, ["shell"]);
  assert.equal(model.place("missing", "lead", "right"), false);
  assert.equal(model.place("lead", "lead", "above"), false);
  assert.equal(model.place("lead", "peer", "below"), false);
});
