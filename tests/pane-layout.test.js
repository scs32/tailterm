import test from "node:test";
import assert from "node:assert/strict";
import { PaneGroups, leaves, tileLayout } from "../client/pane-layout.js";
import { taskMemberIds } from "../client/tasks.js";
import {
  normalizeWorkspace,
  workspaceSnapshot,
} from "../client/workspace-state.js";

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

test("task agents stack workers before growing into two balanced columns", () => {
  const model = new PaneGroups();
  const ids = [
    "lead",
    "second",
    "third",
    "fourth",
    "fifth",
    "sixth",
    "seventh",
    "eighth",
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
    const columns = [...new Set(panes.map((pane) => pane.x))];
    assert.equal(columns.length, count === 1 ? 1 : count < 5 ? 2 : 3);
    if (count >= 3) {
      const heights = columns
        .slice(1)
        .map((x) => panes.filter((pane) => pane.x === x).length);
      assert.ok(heights.every((rows) => rows >= 2));
      assert.ok(Math.max(...heights) - Math.min(...heights) <= 1);
    }
    if (count === 3 || count === 4) {
      assert.equal(group.tree.ratio, 0.5);
      assert.equal(byId.second.x, byId.third.x);
      assert.ok(byId.third.y > byId.second.y);
      if (count === 4) assert.ok(byId.fourth.y > byId.third.y);
    }
    if (count >= 5) {
      assert.equal(group.tree.ratio, 1 / 3);
      assert.equal(byId.second.x, byId.fourth.x);
      assert.ok(byId.fourth.y > byId.second.y);
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

test("saved automatic worker columns migrate by shape and reconnect by agent identity", () => {
  const taskId = "tsk_1111111111111111";
  for (const count of [3, 4, 5]) {
    const agents = Array.from({ length: count }, (_, i) => homeAgentId(i + 1));
    const column = (ids) =>
      ids.length === 1
        ? { agentId: ids[0] }
        : {
            id: `column-${ids[0]}`,
            axis: "y",
            ratio: 1 / ids.length,
            a: { agentId: ids[0] },
            b: column(ids.slice(1)),
          };
    const oldTree = {
      id: "old-root",
      axis: "x",
      ratio: 0.37,
      a: { agentId: agents[0] },
      b: {
        id: "old-workers",
        axis: "x",
        ratio: 0.62,
        a: column(agents.slice(1).filter((_, i) => i % 2 === 0)),
        b: column(agents.slice(1).filter((_, i) => i % 2 === 1)),
      },
    };
    const saved = [
      { taskId, taskLayout: "auto", tree: oldTree, activeAgentId: agents[2] },
    ];
    const restored = new PaneGroups();
    restored.loadProjectLayouts(JSON.parse(JSON.stringify(saved)));
    restored.setTaskMembers(taskId, agents);
    const tabs = [];
    const taskOf = () => taskId;
    const agentOf = (id) => agents[Number(id.slice(4))];
    for (const i of [...agents.keys()].reverse()) {
      tabs.push(`new-${i}`);
      restored.sync(tabs, taskOf, agentOf);
      restored.setTaskOrchestrator(taskId, "new-0");
      restored.isolateTasks(taskOf, agentOf);
    }
    const group = restored.taskGroup(taskId);
    const { panes } = tileLayout(group.tree, 1200, 800);
    const workers = panes.filter((p) => p.id !== "new-0");
    assert.equal(new Set(workers.map((p) => p.x)).size, count < 5 ? 1 : 2);
    assert.equal(group.active, "new-2");
    if (count === 5) {
      assert.deepEqual(
        restored.projectLayoutSnapshot(),
        saved,
        "matching auto shape keeps divider IDs and ratios",
      );
    } else {
      assert.equal(group.tree.ratio, 0.5);
      assert.equal(group.tree.b.axis, "y");
    }
    const before = structuredClone(restored.projectLayoutSnapshot());
    restored.sync(tabs, taskOf, agentOf);
    restored.isolateTasks(taskOf, agentOf);
    assert.deepEqual(restored.projectLayoutSnapshot(), before);
    // A manually retained old arrangement is never migrated.
    const manual = new PaneGroups();
    manual.loadProjectLayouts([{ ...saved[0], taskLayout: "manual" }]);
    manual.setTaskMembers(taskId, agents);
    manual.sync(tabs, taskOf, agentOf);
    manual.setTaskOrchestrator(taskId, "new-0");
    manual.isolateTasks(taskOf, agentOf);
    assert.deepEqual(manual.projectLayoutSnapshot()[0].tree, oldTree);
  }
});

test("stacked small teams keep manual resizing, swaps, guests and narrow layouts", () => {
  for (const count of [3, 4, 5]) {
    const model = new PaneGroups();
    const ids = Array.from({ length: count }, (_, i) => `p${i}`);
    const taskOf = (id) => (ids.includes(id) ? "task" : undefined);
    model.sync([...ids, "shell"], taskOf);
    model.setTaskOrchestrator("task", "p0");
    model.isolateTasks(taskOf);
    const group = model.taskGroup("task");
    for (const [width, height] of [
      [360, 700],
      [800, 1200],
    ]) {
      const layout = tileLayout(group.tree, width, height);
      assert.equal(layout.dividers[0].axis, "y");
      if (width < 540) {
        assert.ok(layout.dividers.every((d) => d.axis === "y"));
        assert.ok(layout.panes.every((p) => p.x === 0 && p.width === width));
      }
      assert.ok(layout.panes.every((p) => p.width >= 180 && p.height >= 120));
    }
    model.customize("p0");
    group.tree.ratio = 0.42;
    group.tree.b.ratio = 0.63;
    model.swap("p1", "p2");
    const before = structuredClone(group.tree);
    model.sync([...ids, "shell"], taskOf);
    model.isolateTasks(taskOf);
    assert.deepEqual(model.taskGroup("task").tree, before);
    assert.equal(model.merge("shell", "p1", { whole: false }), true);
    const guestTree = structuredClone(model.taskGroup("task").tree);
    model.sync([...ids, "shell"], taskOf);
    model.isolateTasks(taskOf);
    assert.deepEqual(model.taskGroup("task").tree, guestTree);
    assert.deepEqual(model.taskGroup("task").guests, ["shell"]);
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
  assert.equal(model.place("lead", "peer", "diagonal"), false);
});

test("directional placement supports left and below", () => {
  const relation = (tree, first, second) =>
    tree.tab
      ? null
      : tree.a.tab === first && tree.b.tab === second
        ? tree.axis
        : relation(tree.a, first, second) || relation(tree.b, first, second);
  const model = new PaneGroups();
  model.sync(["a", "b", "c", "d"]);
  model.merge("b", "a");
  model.merge("c", "b");

  assert.equal(model.place("a", "c", "left"), true);
  assert.equal(relation(model.group("a").tree, "a", "c"), "x");
  assert.equal(model.place("a", "c", "below"), true);
  assert.equal(relation(model.group("a").tree, "c", "a"), "y");
  assert.deepEqual(leaves(model.group("a").tree).sort(), ["a", "b", "c"]);

  assert.equal(model.place("d", "b", "left"), true);
  assert.equal(model.group("d"), model.group("b"));
  assert.equal(relation(model.group("b").tree, "d", "b"), "x");
});

// Continued groups: a group holds at most model.limit panes (default 8).
const project = (count, { limit, lead = "p1", taskId = "task" } = {}) => {
  const model = new PaneGroups();
  if (limit) model.limit = limit;
  const ids = [];
  const add = (id) => {
    ids.push(id);
    model.sync(
      ids,
      () => taskId,
      (x) => x,
    );
    model.setTaskOrchestrator(taskId, lead);
    model.isolateTasks(
      () => taskId,
      (x) => x,
    );
  };
  for (let i = 1; i <= count; i++) add(`p${i}`);
  const close = (id) => {
    ids.splice(ids.indexOf(id), 1);
    model.sync(
      ids,
      () => taskId,
      (x) => x,
    );
    model.isolateTasks(
      () => taskId,
      (x) => x,
    );
  };
  const parts = () =>
    model.series(model.taskGroup(taskId)).map((g) => leaves(g.tree).sort());
  return { model, ids, add, close, parts };
};
const sorted = (...ids) => ids.sort();
const range = (from, to) =>
  sorted(...Array.from({ length: to - from + 1 }, (_, i) => `p${from + i}`));

test("automatic agent continuations stack two or three workers before opening another column", () => {
  for (const limit of [4, 8]) {
    for (const rows of [2, 3, 4]) {
      const { model, parts } = project(limit + rows, { limit });
      const continuation = model.series(model.taskGroup("task"))[1];
      const panes = tileLayout(continuation.tree, 1200, 800).panes;
      const xs = [...new Set(panes.map((p) => p.x))];
      assert.equal(
        xs.length,
        rows < 4 ? 1 : 2,
        `cap ${limit}, ${rows} workers`,
      );
      assert.ok(xs.every((x) => panes.filter((p) => p.x === x).length >= 2));
      assert.deepEqual(parts(), [
        range(1, limit),
        range(limit + 1, limit + rows),
      ]);
      assert.equal(
        new Set(model.groups.flatMap((g) => leaves(g.tree))).size,
        limit + rows,
      );
    }
  }
});

test("saved auto continuations migrate and reconnect while manual and guest shapes stay", async () => {
  const { continuationTree } = await import("../client/pane-cap.js");
  const taskId = "tsk_1111111111111111";
  for (const limit of [4, 8]) {
    for (const rows of [2, 3, 4]) {
      const { model, ids } = project(limit + rows, { limit, taskId });
      const agentOf = (id) => homeAgentId(Number(id.slice(1)));
      const roster = ids.map(agentOf);
      model.setTaskMembers(taskId, roster);
      model.sync(ids, () => taskId, agentOf);
      model.isolateTasks(() => taskId, agentOf);
      const convert = (tree) =>
        tree.tab
          ? { agentId: agentOf(tree.tab) }
          : { ...tree, a: convert(tree.a), b: convert(tree.b) };
      const oldTree = convert(continuationTree(ids.slice(limit)));
      oldTree.ratio = 0.61;
      const saved = model.projectLayoutSnapshot();
      saved[0].continued = [oldTree];
      saved[0].activeAgentId = roster.at(-1);
      const restore = (taskLayout) => {
        const restored = new PaneGroups();
        restored.limit = limit;
        restored.loadProjectLayouts(
          JSON.parse(JSON.stringify([{ ...saved[0], taskLayout }])),
        );
        restored.setTaskMembers(taskId, roster);
        const tabs = [];
        const newAgentOf = (id) =>
          id.startsWith("new-") ? homeAgentId(Number(id.slice(4))) : undefined;
        for (const id of [...ids].reverse()) {
          tabs.push(`new-${id.slice(1)}`);
          restored.sync(tabs, () => taskId, newAgentOf);
          restored.setTaskOrchestrator(taskId, "new-1");
          restored.isolateTasks(() => taskId, newAgentOf);
        }
        return { restored, tabs, newAgentOf };
      };
      const { restored, tabs, newAgentOf } = restore("auto");
      const continuation = restored.series(restored.taskGroup(taskId))[1];
      const panes = tileLayout(continuation.tree, 1200, 800).panes;
      assert.equal(new Set(panes.map((p) => p.x)).size, rows < 4 ? 1 : 2);
      assert.deepEqual(
        leaves(continuation.tree).sort(),
        ids
          .slice(limit)
          .map((id) => `new-${id.slice(1)}`)
          .sort(),
      );
      assert.equal(continuation.active, `new-${limit + rows}`);
      if (rows === 4)
        assert.deepEqual(
          restored.projectLayoutSnapshot()[0].continued[0],
          oldTree,
        );
      else assert.equal(continuation.tree.axis, "y");
      const before = structuredClone(restored.projectLayoutSnapshot());
      restored.sync(tabs, () => taskId, newAgentOf);
      restored.isolateTasks(() => taskId, newAgentOf);
      assert.deepEqual(restored.projectLayoutSnapshot(), before);
      assert.deepEqual(
        restore("manual").restored.projectLayoutSnapshot()[0].continued[0],
        oldTree,
      );
      // A guest joining a small continuation makes it manual and retains its shape.
      const taskOf = (id) => (id === "shell" ? undefined : taskId);
      restored.sync([...tabs, "shell"], taskOf, (id) =>
        id === "shell" ? undefined : newAgentOf(id),
      );
      restored.isolateTasks(taskOf, newAgentOf);
      assert.equal(
        restored.merge("shell", `new-${limit + 1}`, { whole: false }),
        rows < limit,
      );
      if (rows < limit) {
        const guestTree = structuredClone(restored.group("shell").tree);
        restored.sync([...tabs, "shell"], taskOf, (id) =>
          id === "shell" ? undefined : newAgentOf(id),
        );
        restored.isolateTasks(taskOf, newAgentOf);
        assert.deepEqual(restored.group("shell").tree, guestTree);
        assert.deepEqual(restored.group("shell").guests, ["shell"]);
      }
    }
  }
});

test("pane cap rules: limits, suffixes, cascade, arrival and series reorder", async () => {
  const cap = await import("../client/pane-cap.js");
  assert.equal(cap.normalizePaneLimit(undefined), 8);
  assert.equal(cap.normalizePaneLimit("12"), 12);
  assert.equal(cap.normalizePaneLimit(7), 8);
  assert.equal(cap.normalizePaneLimit(100), 8);
  assert.deepEqual(cap.PANE_LIMITS, [4, 6, 8, 10, 12, 16]);
  assert.equal(cap.continuedSuffix(0), "");
  assert.equal(cap.continuedSuffix(1), " (continued)");
  assert.equal(cap.continuedSuffix(2), " (continued 2)");
  assert.deepEqual(cap.cascade([[1, 2, 3, 4, 5], [6]], 2), [
    [1, 2],
    [3, 4],
    [5, 6],
  ]);
  assert.equal(cap.arrivalPart([8, 5], 8), 1);
  assert.equal(cap.arrivalPart([7, 5], 8, 1), 1);
  assert.equal(cap.arrivalPart([7, 8], 8, 1), 0);
  assert.equal(cap.arrivalPart([8, 8], 8), -1);
  assert.deepEqual(leaves(cap.continuationTree(["a"])), ["a"]);
  assert.equal(
    cap.continuationTree(["a", "b"]).axis,
    "x",
    "ordinary continuations retain their two-column rule",
  );
  const tree = cap.continuationTree(["a", "b", "c", "d", "e"]);
  assert.equal(tree.axis, "x");
  assert.deepEqual(leaves(tree.a), ["a", "c", "e"]);
  assert.deepEqual(leaves(tree.b), ["b", "d"]);
  // Absent keys past the limit move on; fresh (just added) keys stay put.
  const plan = cap.planTemplate({
    template: [["a", "b", "x"], ["c"]],
    live: [["a", "b"], ["c"]],
    limit: 3,
    added: ["y"],
  });
  assert.deepEqual(plan.parts, [
    ["a", "b", "y"],
    ["x", "c"],
  ]);
  assert.deepEqual(plan.liveIndex, [0, 1]);
  const [a, p, pc, b] = ["A", "P", "Pc", "B"];
  assert.deepEqual(cap.reorderSeries([a, p, pc, b], [p, pc], [b], true), [
    a,
    b,
    p,
    pc,
  ]);
  assert.equal(cap.reorderSeries([a, p, pc], [p, pc], [pc]), null);
});

test("a project continues past eight panes in arrival order with the orchestrator first", () => {
  const twelve = project(12);
  assert.deepEqual(twelve.parts(), [range(1, 8), range(9, 12)]);
  const [first, second] = twelve.model.series(twelve.model.taskGroup("task"));
  assert.equal(first.part, undefined);
  assert.equal(second.part, 1);
  assert.equal(second.taskId, "task", "continuations keep the project");
  assert.equal(twelve.model.groups.length, 2);
  const twenty = project(20);
  assert.deepEqual(twenty.parts(), [range(1, 8), range(9, 16), range(17, 20)]);
  assert.deepEqual(
    twenty.model.series(twenty.model.taskGroup("task")).map((g) => g.part),
    [undefined, 1, 2],
  );
  const saved = structuredClone(twenty.model.groups);
  twenty.model.sync(
    twenty.ids,
    () => "task",
    (x) => x,
  );
  twenty.model.isolateTasks(
    () => "task",
    (x) => x,
  );
  assert.deepEqual(twenty.model.groups, saved, "repeated sync is stable");
  // The orchestrator arriving ninth still lands in the first part.
  const late = project(12, { lead: "p9" });
  assert.deepEqual(late.parts(), [
    sorted(...range(1, 7), "p9"),
    sorted("p8", ...range(10, 12)),
  ]);
  const lead = late.model.taskGroup("task");
  assert.equal(
    tileLayout(lead.tree, 1200, 800).panes.find((p) => p.id === "p9").height,
    800,
  );
});

test("the pane limit is a setting; lowering it cascades and raising never merges", () => {
  const { model, ids, parts } = project(10, { limit: 4 });
  assert.deepEqual(parts(), [range(1, 4), range(5, 8), range(9, 10)]);
  model.limit = 8;
  model.sync(
    ids,
    () => "task",
    (x) => x,
  );
  model.isolateTasks(
    () => "task",
    (x) => x,
  );
  assert.deepEqual(parts(), [range(1, 4), range(5, 8), range(9, 10)]);
  const standard = project(10);
  standard.model.limit = 4;
  standard.model.sync(
    standard.ids,
    () => "task",
    (x) => x,
  );
  standard.model.isolateTasks(
    () => "task",
    (x) => x,
  );
  assert.deepEqual(standard.parts(), [range(1, 4), range(5, 8), range(9, 10)]);
});

test("closing frees a slot that the next pane fills, and empty parts disappear", () => {
  const { model, add, close, parts } = project(20);
  close("p3");
  assert.deepEqual(parts(), [
    sorted(...range(1, 8).filter((id) => id !== "p3")),
    range(9, 16),
    range(17, 20),
  ]);
  add("p21");
  assert.deepEqual(
    parts()[0],
    sorted(...range(1, 8).filter((id) => id !== "p3"), "p21"),
  );
  assert.deepEqual(parts().slice(1), [range(9, 16), range(17, 20)]);
  for (const id of ["p17", "p18", "p19", "p20"]) close(id);
  assert.equal(parts().length, 2);
  for (const id of range(9, 16)) close(id);
  assert.equal(parts().length, 1);
  const again = project(20);
  for (const id of range(9, 16)) again.close(id);
  const series = again.model.series(again.model.taskGroup("task"));
  assert.deepEqual(
    series.map((g) => g.part),
    [undefined, 1],
  );
  assert.deepEqual(leaves(series[1].tree).sort(), range(17, 20));
  assert.ok(model.groups.every((g) => g.taskId === "task"));
});

test("a manual project layout over the limit keeps its arrangement in the first part", () => {
  const { model, ids, parts } = project(8);
  model.customize("p1");
  model.swap("p2", "p5");
  const before = structuredClone(model.taskGroup("task").tree);
  ids.push("p9", "p10");
  model.sync(
    ids,
    () => "task",
    (x) => x,
  );
  model.isolateTasks(
    () => "task",
    (x) => x,
  );
  assert.deepEqual(model.taskGroup("task").tree, before);
  assert.deepEqual(parts()[1], range(9, 10));
  assert.equal(model.series(model.taskGroup("task"))[1].taskLayout, "manual");
});

test("moves respect the cap: send, make room, own-project parts and other projects", () => {
  const owner = (id) =>
    id.startsWith("q") ? "other" : id.startsWith("p") ? "task" : undefined;
  const { model, ids } = project(12);
  ids.push("q1", "shell");
  model.sync(ids, owner, (x) => x);
  model.isolateTasks(owner, (x) => x);
  const task = () => model.series(model.taskGroup("task"));
  // A project pane moves from "(continued)" into the first part once it has room.
  assert.equal(model.canMerge("p9", "p1", false), true);
  assert.equal(model.canFit("p9", "p1", false), false);
  assert.equal(model.merge("p9", "p1", { whole: false }), false, "full");
  assert.equal(model.canMerge("p9", "q1", false), false, "other project");
  ids.splice(ids.indexOf("p2"), 1);
  model.sync(ids, owner, (x) => x);
  model.isolateTasks(owner, (x) => x);
  assert.equal(model.merge("p9", "p1", { whole: false }), true);
  assert.ok(leaves(task()[0].tree).includes("p9"));
  assert.equal(task()[0].guests.includes("p9"), false, "agents are not guests");
  // A plain pane sent to a full project joins the earliest part with room.
  ids.push("p2");
  model.sync(ids, owner, (x) => x);
  model.isolateTasks(owner, (x) => x);
  assert.equal(leaves(task()[0].tree).length, 8);
  assert.equal(model.send("shell", "p1"), true);
  assert.ok(leaves(task()[1].tree).includes("shell"));
  assert.deepEqual(task()[1].guests, ["shell"]);
  // Make room: the dragged pane takes the target's slot; the target moves on.
  model.detach("shell");
  assert.equal(model.makeRoom("shell", "p4"), true);
  assert.ok(leaves(task()[0].tree).includes("shell"));
  assert.ok(leaves(task()[1].tree).includes("p4"));
  assert.equal(leaves(task()[0].tree).length, 8);
});

test("plain groups continue through Send and leave the series at one pane", () => {
  const model = new PaneGroups();
  const ids = Array.from({ length: 10 }, (_, i) => `s${i}`);
  model.sync(ids);
  for (const id of ids.slice(1, 8)) model.merge(id, "s0");
  model.merge("s9", "s8");
  assert.equal(model.canFit("s8", "s0"), false);
  assert.equal(model.merge("s8", "s0"), false);
  assert.equal(model.send("s8", "s0", true), true);
  const series = () => model.series(model.group("s0"));
  assert.equal(series().length, 2);
  assert.ok(series()[0].series && series()[0].series === series()[1].series);
  assert.deepEqual(leaves(series()[1].tree).sort(), ["s8", "s9"]);
  // A continuation that drops to one pane becomes an ordinary tab.
  model.sync(ids.filter((id) => id !== "s9"));
  assert.equal(model.group("s8").series, undefined);
  assert.equal(model.group("s8").part, undefined);
  assert.equal(model.group("s0").series, undefined, "one part left: no series");
  // The original dropping to one pane hands the name to its continuation.
  const other = new PaneGroups();
  const more = ["a", "b", "c", "d", "e", "f"];
  other.sync(more);
  for (const id of more.slice(1)) other.merge(id, "a");
  other.limit = 4;
  other.sync(more);
  const parts = other.series(other.group("a"));
  assert.equal(parts.length, 2);
  const [first, second] = parts.map((g) => leaves(g.tree));
  const keep = first[0];
  other.sync([keep, ...second]);
  assert.equal(other.group(keep).series, undefined);
  assert.equal(other.group(second[0]).series, undefined);
  assert.equal(other.group(second[0]).part, undefined);
  assert.deepEqual(leaves(other.group(second[0]).tree).sort(), second.sort());
});

test("a standalone plain group over the limit continues on the next sync", () => {
  const model = new PaneGroups();
  model.limit = 16;
  const ids = Array.from({ length: 10 }, (_, i) => `s${i}`);
  model.sync(ids);
  for (const id of ids.slice(1)) model.merge(id, "s0");
  const order = leaves(model.group("s0").tree);
  model.limit = 8;
  model.sync(ids);
  const parts = model.series(model.group("s0"));
  assert.equal(parts.length, 2);
  assert.deepEqual(leaves(parts[0].tree), order.slice(0, 8));
  assert.deepEqual(leaves(parts[1].tree), order.slice(8));
  assert.equal(parts[1].part, 1);
  assert.equal(parts[0].series, parts[1].series);
});

test("a series reorders as one unit and tab moves step over whole series", () => {
  const owner = (id) => (id.startsWith("p") ? "task" : undefined);
  const { model, ids } = project(10);
  ids.unshift("a");
  ids.push("b");
  model.sync(ids, owner, (x) => x);
  model.isolateTasks(owner, (x) => x);
  model.groups = [
    model.group("a"),
    ...model.series(model.taskGroup("task")),
    model.group("b"),
  ];
  const names = () =>
    model.groups.map((g) =>
      g.taskId ? (g.part ? `P${g.part}` : "P") : leaves(g.tree)[0],
    );
  assert.deepEqual(names(), ["a", "P", "P1", "b"]);
  assert.equal(model.reorder("p9", "b", true), true);
  assert.deepEqual(names(), ["a", "b", "P", "P1"]);
  assert.equal(model.reorder("p9", "p1", false), false);
  assert.deepEqual(names(), ["a", "b", "P", "P1"]);
  assert.equal(model.reorder("a", "p9", true), true);
  assert.deepEqual(names(), ["b", "P", "P1", "a"]);
});

test("closing a project unbinds every part", async () => {
  const { unbindGroups } = await import("../client/task-hub.js");
  const { model } = project(12);
  unbindGroups(model.groups, "task");
  assert.ok(model.groups.every((g) => !g.taskId && !g.part && !g.guests));
});

// The home area: only the owner helper's pane, outside every group, never
// counted by the cap. Fixture ids only.
const HOME_TASK = "tsk_2222222222222222";
const OTHER_TASK = "tsk_3333333333333333";
const homeAgentId = (n) => `agt_${String(n).padStart(16, "0")}`;
const HELPER = "agt_00000000000000aa";
const OTHER_HELPER = "agt_00000000000000bb";
// shells are the owner's plain terminals; home is a saved home area to restore.
const homeFixture = (agents, { limit = 8, shells = [], home } = {}) => {
  const model = new PaneGroups();
  model.limit = limit;
  const panes = new Map();
  for (let n = 1; n <= agents; n++)
    panes.set(`p${n}`, { taskId: HOME_TASK, agentId: homeAgentId(n) });
  panes.set("helper", { taskId: HOME_TASK, agentId: HELPER, home: true });
  for (const id of shells) panes.set(id, {});
  const taskOf = (id) => panes.get(id)?.taskId,
    agentOf = (id) => panes.get(id)?.agentId,
    homeOf = (id) => !!panes.get(id)?.home;
  const roster = [
    { id: HELPER, role: "owner_helper", status: "running" },
    ...Array.from({ length: agents }, (_, i) => ({
      id: homeAgentId(i + 1),
      status: "running",
    })),
  ];
  const sync = () => {
    model.setTaskMembers(HOME_TASK, taskMemberIds({ status: "open" }, roster));
    model.sync([...panes.keys()], taskOf, agentOf, homeOf);
    model.isolateTasks(taskOf, agentOf);
  };
  if (home) model.home = structuredClone(home);
  sync();
  const parts = () =>
    model.series(model.taskGroup(HOME_TASK)).map((g) => leaves(g.tree).length);
  // As saved: a JSON round-trip, so an undefined key is not a difference.
  const state = () =>
    JSON.parse(JSON.stringify({ groups: model.groups, home: model.home }));
  return { model, panes, sync, parts, state, roster };
};
// What an older workspace saved: the helper above the owner's shells.
const legacyHome = (...shells) => ({
  tree: shells.reduce(
    (tree, id, i) => ({
      id: `h${i}`,
      axis: "y",
      ratio: 1 - 1 / (i + 2),
      a: tree,
      b: { tab: id },
    }),
    { tab: "helper" },
  ),
  active: shells.at(-1),
  ratio: 0.33,
});

test("the cap and continued groups ignore the home pane", () => {
  const eight = homeFixture(8);
  assert.deepEqual(eight.parts(), [8], "one part, no (continued)");
  assert.equal(eight.model.groups.length, 1);
  assert.deepEqual(leaves(eight.model.home.tree), ["helper"]);
  assert.equal(eight.model.group("helper"), undefined);
  const nine = homeFixture(9);
  assert.deepEqual(nine.parts(), [8, 1]);
  assert.deepEqual(leaves(nine.model.home.tree), ["helper"]);
  for (const { model } of [eight, nine]) {
    const snapshot = JSON.stringify(model.projectLayoutSnapshot());
    assert.ok(!snapshot.includes(HELPER), "the helper never enters a template");
    assert.ok(!snapshot.includes('"helper"'));
  }
  // Repeated syncs are stable.
  nine.sync();
  const before = nine.state();
  nine.sync();
  assert.deepEqual(nine.state(), before);
});

test("an absent helper leaves eight agents in one part at limit 8", () => {
  const { model, panes, sync, parts } = homeFixture(8);
  panes.delete("helper");
  sync();
  assert.deepEqual(parts(), [8]);
  assert.equal(model.home, null);
  const template = model.projectLayoutSnapshot()[0];
  const keys = JSON.stringify(template).match(/agt_[0-9a-f]{16}/g);
  assert.equal(new Set(keys).size, 8);
  assert.ok(!keys.includes(HELPER));
});

test("a new plain terminal never enters home", () => {
  const { model, panes, sync } = homeFixture(2);
  panes.set("s1", {});
  sync();
  assert.deepEqual(leaves(model.home.tree), ["helper"]);
  assert.deepEqual(leaves(model.group("s1").tree), ["s1"]);
});

test("shells saved in home migrate to one ordinary group, once", () => {
  const { model, panes, sync, state, parts } = homeFixture(3, {
    shells: ["s1", "s2"],
    home: legacyHome("s1", "s2"),
  });
  assert.deepEqual(leaves(model.home.tree), ["helper"]);
  assert.deepEqual(model.home, {
    tree: { tab: "helper" },
    active: "helper",
    ratio: 0.33,
  });
  const group = model.group("s1");
  assert.equal(group, model.group("s2"), "one group for both shells");
  assert.deepEqual(leaves(group.tree), ["s1", "s2"], "their old order");
  assert.equal(group.tree.axis, "y", "their old arrangement");
  assert.equal(group.active, "s2");
  assert.equal(group.taskId, undefined, "an ordinary group, not a project's");
  assert.equal(group.guests, undefined);
  assert.deepEqual(parts(), [3], "the project group is untouched");
  // A second sync changes nothing.
  const before = state();
  sync();
  assert.deepEqual(state(), before);
  // Once out, the shells are ordinary: separating one stays separated.
  assert.equal(model.detach("s1"), true);
  sync();
  assert.notEqual(model.group("s1"), model.group("s2"));
  assert.deepEqual(leaves(model.home.tree), ["helper"]);
  panes.delete("helper");
  sync();
  assert.equal(model.home, null);
});

test("home migration: one shell, a closed shell and a non-helper agent", () => {
  // One shell becomes an ordinary tab.
  const one = homeFixture(1, { shells: ["s1"], home: legacyHome("s1") });
  assert.deepEqual(one.model.group("s1").tree, { tab: "s1" });
  assert.deepEqual(leaves(one.model.home.tree), ["helper"]);
  // A saved shell that did not come back is dropped, not regrouped.
  const gone = homeFixture(1, {
    shells: ["s2"],
    home: legacyHome("s1", "s2"),
  });
  assert.equal(gone.model.group("s1"), undefined);
  assert.deepEqual(gone.model.group("s2").tree, { tab: "s2" });
  // No helper pane: the shells still leave and home is empty.
  const alone = homeFixture(0, {
    shells: ["s1", "s2"],
    home: legacyHome("s1", "s2"),
  });
  alone.panes.delete("helper");
  alone.model.home = legacyHome("s1", "s2");
  alone.model.groups = [];
  alone.sync();
  assert.equal(alone.model.home, null);
  assert.deepEqual(leaves(alone.model.group("s1").tree), ["s1", "s2"]);
  // An agent pane the roster says is not the helper joins its project, not
  // the shells' group.
  const agent = homeFixture(2, {
    shells: ["s1"],
    home: {
      tree: {
        id: "h0",
        axis: "y",
        ratio: 0.5,
        a: { tab: "helper" },
        b: {
          id: "h1",
          axis: "y",
          ratio: 0.5,
          a: { tab: "p1" },
          b: { tab: "s1" },
        },
      },
      active: "p1",
      ratio: 0.4,
    },
  });
  assert.deepEqual(leaves(agent.model.home.tree), ["helper"]);
  assert.deepEqual(agent.model.group("s1").tree, { tab: "s1" });
  assert.equal(agent.model.group("p1").taskId, HOME_TASK);
  assert.equal(agent.model.group("p1"), agent.model.group("p2"));
});

test("group operations refuse home panes and leave the model unchanged", () => {
  const { model, panes, sync, state } = homeFixture(3, { shells: ["plain"] });
  // A second project's helper: home tiles one helper per project.
  panes.set("helper2", {
    taskId: OTHER_TASK,
    agentId: OTHER_HELPER,
    home: true,
  });
  sync();
  assert.deepEqual(leaves(model.home.tree), ["helper", "helper2"]);
  const before = state();
  const checks = [
    () => model.canMerge("helper", "p1"),
    () => model.canMerge("p1", "helper", false),
    () => model.canMerge("plain", "helper"),
    () => model.canFit("helper", "plain"),
    () => model.merge("helper", "plain"),
    () => model.merge("plain", "helper", { whole: false }),
    () => model.merge("helper", "p1", { whole: false }),
    () => model.send("helper", "plain"),
    () => model.send("p1", "helper"),
    () => model.makeRoom("helper", "p2"),
    () => model.makeRoom("plain", "helper"),
    () => model.place("helper", "p1", "right"),
    () => model.place("p1", "helper2", "above"),
    () => model.place("plain", "helper", "right"),
    () => model.swap("helper", "helper2"),
    () => model.swap("helper", "p1"),
    () => model.reorder("helper", "plain"),
    () => model.reorder("plain", "helper", true),
    () => model.detach("helper"),
    () => model.canDetach("helper2"),
    () => model.homeSwap("helper", "plain"),
    () => model.homeSwap("plain", "helper"),
    () => model.homePlace("plain", "helper", "left"),
    () => model.homePlace("helper", "p1", "left"),
  ];
  checks.forEach((check, i) => assert.equal(check(), false, `check ${i}`));
  assert.deepEqual(state(), before);
  assert.equal(model.group("helper"), undefined);
  // Nothing moves a pane into or out of home.
  for (const name of ["canHome", "toHome", "leaveHome"])
    assert.equal(model[name], undefined, name);
  assert.equal(model.inHome("plain"), false);
  // Helpers rearrange among themselves.
  assert.equal(model.homeSwap("helper2", "helper"), true);
  assert.deepEqual(leaves(model.home.tree), ["helper2", "helper"]);
  assert.equal(model.homePlace("helper", "helper2", "left"), true);
  assert.deepEqual(leaves(model.home.tree), ["helper", "helper2"]);
  assert.equal(model.home.tree.axis, "x");
  model.rememberActive("helper2");
  assert.equal(model.home.active, "helper2");
  assert.deepEqual(state().groups, before.groups, "the groups never changed");
});

test("home persists across a workspace round-trip", () => {
  const { model, panes, sync } = homeFixture(10, { shells: ["s1", "s2"] });
  model.home.ratio = 0.33;
  model.rememberActive("helper");
  const server = {
    id: "srv",
    host: "synthetic.invalid",
    port: 22,
    username: "fx",
  };
  const tabs = [...panes].map(([id, pane]) => ({
    id,
    server,
    tmux: true,
    session: id,
    wasConnected: true,
    task: pane.agentId
      ? { taskId: HOME_TASK, agentId: pane.agentId }
      : undefined,
  }));
  const saved = normalizeWorkspace(
    JSON.parse(
      JSON.stringify(
        workspaceSnapshot(
          tabs,
          model.groups,
          "s1",
          null,
          [HOME_TASK],
          [],
          model.projectLayoutSnapshot(),
          model.home,
        ),
      ),
    ),
  );
  assert.deepEqual(saved.home, model.home);
  assert.deepEqual(saved.home, {
    tree: { tab: "helper" },
    active: "helper",
    ratio: 0.33,
  });
  const restored = homeFixture(10, { shells: ["s1", "s2"] });
  restored.model.loadProjectLayouts(saved.projectLayouts);
  restored.model.groups = saved.groups;
  restored.model.home = saved.home;
  restored.sync();
  assert.deepEqual(restored.model.home, model.home);
  assert.deepEqual(restored.parts(), [8, 2]);
  sync();
  assert.deepEqual(
    restored.model.groups.map((g) => leaves(g.tree)),
    model.groups.map((g) => leaves(g.tree)),
  );
  // No home: the key is absent.
  const none = workspaceSnapshot(tabs.slice(0, 2), [], null);
  assert.equal("home" in none, false);
  assert.equal("home" in normalizeWorkspace(none), false);
  // A tab in both home and a group stays in home; bad input is dropped or clamped.
  const both = normalizeWorkspace({
    tabs: tabs.slice(0, 3).map((t) => ({
      id: t.id,
      serverId: "srv",
      endpoint: "x",
      tmux: true,
      session: t.id,
    })),
    home: {
      tree: {
        axis: "y",
        ratio: 0.5,
        a: { tab: "p1" },
        b: { tab: "ghost" },
      },
      active: "ghost",
      ratio: 7,
    },
    groups: [
      {
        tree: { axis: "x", ratio: 0.5, a: { tab: "p1" }, b: { tab: "p2" } },
        active: "p1",
      },
    ],
  });
  assert.deepEqual(both.home, {
    tree: { tab: "p1" },
    active: "p1",
    ratio: 0.8,
  });
  assert.deepEqual(
    both.groups.map((g) => g.tree),
    [{ tab: "p2" }],
  );
  const empty = normalizeWorkspace({
    tabs: [],
    home: { tree: { tab: "ghost" }, ratio: "wide" },
  });
  assert.equal("home" in empty, false);
  const defaulted = normalizeWorkspace({
    tabs: [{ id: "a", serverId: "srv", endpoint: "x" }],
    home: { tree: { tab: "a" }, ratio: "wide" },
  });
  assert.equal(defaulted.home.ratio, 0.4);
});
