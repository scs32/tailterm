import test from "node:test";
import assert from "node:assert/strict";
import { PaneGroups, leaves } from "../client/pane-layout.js";
import {
  normalizeProjectLayouts,
  normalizeWorkspace,
  workspaceSnapshot,
} from "../client/workspace-state.js";

const TASK = "tsk_1111111111111111";
const AGENTS = Array.from(
  { length: 6 },
  (_, index) => `agt_${String(index + 1).padStart(16, "0")}`,
);
const server = {
  id: "fixture-server",
  host: "synthetic.invalid",
  port: 22,
  username: "fixture",
};
const tab = (agentIndex, generation = "old") => ({
  id: `${generation}-pane-${agentIndex}`,
  server,
  tmux: true,
  session: `agent-${agentIndex}`,
  wasConnected: true,
  task: { taskId: TASK, agentId: AGENTS[agentIndex] },
});
const synchronize = (model, tabs) => {
  const byId = new Map(tabs.map((item) => [item.id, item]));
  const taskOf = (id) => byId.get(id)?.task?.taskId;
  const agentOf = (id) => byId.get(id)?.task?.agentId;
  model.sync(
    tabs.map((item) => item.id),
    taskOf,
    agentOf,
  );
  model.isolateTasks(taskOf, agentOf);
};

test("project layout round-trips by task and agent across new pane IDs and arrival order", () => {
  const original = new PaneGroups(),
    tabs = AGENTS.slice(0, 5).map((_, index) => tab(index));
  original.setTaskMembers(TASK, AGENTS.slice(0, 5));
  synchronize(original, tabs);
  original.place(tabs[4].id, tabs[1].id, "above");
  const group = original.taskGroup(TASK);
  group.tree.ratio = 0.37123456789;
  original.remember(tabs[0].id);
  original.rememberActive(tabs[3].id);

  const expected = original.projectLayoutSnapshot();
  const saved = normalizeWorkspace(
    workspaceSnapshot(
      tabs,
      original.groups,
      tabs[3].id,
      null,
      [TASK],
      [],
      expected,
    ),
  );
  assert.deepEqual(saved.projectLayouts, expected);
  assert.equal(saved.projectLayouts[0].tree.ratio, 0.37123456789);

  const restored = new PaneGroups(),
    arriving = [];
  restored.loadProjectLayouts(saved.projectLayouts);
  restored.setTaskMembers(TASK, AGENTS.slice(0, 5));
  for (const index of [4, 2, 0, 3, 1]) {
    arriving.push(tab(index, "new"));
    synchronize(restored, arriving);
  }
  assert.deepEqual(restored.projectLayoutSnapshot(), expected);
  assert.equal(
    arriving.find((item) => item.id === restored.taskGroup(TASK).active).task
      .agentId,
    AGENTS[3],
  );
  assert.deepEqual(
    leaves(restored.taskGroup(TASK).tree).map(
      (id) => arriving.find((item) => item.id === id).task.agentId,
    ),
    [AGENTS[0], AGENTS[4], AGENTS[1], AGENTS[3], AGENTS[2]],
  );
});

test("missing agents retain their slots while new and deleted members reconcile deliberately", () => {
  const model = new PaneGroups(),
    tabs = AGENTS.slice(0, 5).map((_, index) => tab(index));
  model.setTaskMembers(TASK, AGENTS.slice(0, 5));
  synchronize(model, tabs);
  model.place(tabs[4].id, tabs[1].id, "above");
  const beforeMissing = model.projectLayoutSnapshot()[0];

  const present = tabs.filter((_, index) => index !== 2);
  synchronize(model, present);
  assert.deepEqual(
    model.projectLayoutSnapshot()[0],
    beforeMissing,
    "temporary absence must not prune the durable template",
  );
  assert.equal(leaves(model.taskGroup(TASK).tree).length, 4);

  model.setTaskMembers(TASK, AGENTS);
  const withNew = model.projectLayoutSnapshot()[0];
  assert.deepEqual(withNew.tree.a, beforeMissing.tree);
  present.push(tab(5, "late"));
  synchronize(model, present);
  assert.ok(
    leaves(model.taskGroup(TASK).tree).includes("late-pane-5"),
    "a late pane materializes without resetting the customized subtree",
  );

  model.setTaskMembers(
    TASK,
    AGENTS.filter((_, index) => index !== 1),
  );
  assert.doesNotMatch(
    JSON.stringify(model.projectLayoutSnapshot()),
    new RegExp(AGENTS[1]),
  );

  model.setTaskMembers(TASK, []);
  assert.deepEqual(model.projectLayoutSnapshot(), []);
  synchronize(model, present);
  assert.deepEqual(
    model.projectLayoutSnapshot(),
    [],
    "an authoritative empty roster blocks accidental recapture",
  );
  model.setTaskMembers(TASK, AGENTS.slice(0, 1));
  synchronize(model, present.slice(0, 1));
  assert.equal(model.projectLayoutSnapshot()[0].taskId, TASK);
});

test("project layout validation is bounded and rejects unsafe geometry", () => {
  const valid = {
    taskId: TASK,
    taskLayout: "manual",
    activeAgentId: AGENTS[0],
    tree: {
      id: "split",
      axis: "y",
      ratio: 0.123456789,
      a: { agentId: AGENTS[0] },
      b: { agentId: AGENTS[1] },
    },
  };
  assert.equal(normalizeProjectLayouts([valid])[0].tree.ratio, 0.123456789);
  for (const ratio of [0.05, 0.95])
    assert.equal(
      normalizeProjectLayouts([{ ...valid, tree: { ...valid.tree, ratio } }])[0]
        .tree.ratio,
      ratio,
    );
  for (const ratio of [0, 1])
    assert.deepEqual(
      normalizeProjectLayouts([{ ...valid, tree: { ...valid.tree, ratio } }]),
      [],
    );
  assert.deepEqual(
    normalizeProjectLayouts([
      { ...valid, tree: { ...valid.tree, ratio: Number.POSITIVE_INFINITY } },
      { ...valid, taskId: "not-a-task" },
    ]),
    [],
  );
  assert.deepEqual(
    normalizeProjectLayouts([
      {
        ...valid,
        tree: {
          ...valid.tree,
          b: { agentId: AGENTS[0] },
        },
      },
    ]),
    [],
    "one stable identity cannot occupy two project slots",
  );
});

// Continued project groups: the template keeps one tree per part.
const agent = (n) => `agt_${String(n).padStart(16, "0")}`;
const member = (n, generation) => ({
  id: `${generation}-pane-${n}`,
  server,
  tmux: true,
  session: `agent-${n}`,
  wasConnected: true,
  task: { taskId: TASK, agentId: agent(n) },
});
const partsOf = (model, tabs) =>
  model
    .series(model.taskGroup(TASK))
    .map((group) =>
      leaves(group.tree).map(
        (id) => tabs.find((item) => item.id === id).task.agentId,
      ),
    );
const templateParts = (layout) =>
  [layout.tree, ...(layout.continued || [])].map((tree) =>
    JSON.stringify(tree).match(/agt_[0-9a-f]{16}/g),
  );
function arrive(order, members, layouts, generation) {
  const model = new PaneGroups(),
    tabs = [];
  if (layouts) model.loadProjectLayouts(layouts);
  model.setTaskMembers(TASK, members);
  for (const n of order) {
    tabs.push(member(n, generation));
    model.setTaskOrchestrator(
      TASK,
      tabs.find((item) => item.task.agentId === agent(1))?.id,
    );
    synchronize(model, tabs);
  }
  return { model, tabs };
}
const numbers = (from, to) =>
  Array.from({ length: to - from + 1 }, (_, i) => from + i);

test("closing a pane then adding an agent bounds every template part", () => {
  const members = numbers(1, 20).map(agent);
  const { model, tabs } = arrive(numbers(1, 20), members, null, "old");
  assert.deepEqual(
    partsOf(model, tabs).map((part) => part.length),
    [8, 8, 4],
  );
  const closed = tabs.splice(
    tabs.findIndex((item) => item.task.agentId === agent(3)),
    1,
  );
  synchronize(model, tabs);
  assert.deepEqual(
    partsOf(model, tabs).map((part) => part.length),
    [7, 8, 4],
  );
  model.setTaskMembers(TASK, [...members, agent(21)]);
  tabs.push(member(21, "old"));
  synchronize(model, tabs);
  const live = partsOf(model, tabs);
  assert.deepEqual(
    live.map((part) => part.length),
    [8, 8, 4],
  );
  assert.ok(live[0].includes(agent(21)), "the new agent fills the freed slot");
  const template = templateParts(model.projectLayoutSnapshot()[0]);
  assert.equal(template[0].length, 8);
  assert.ok(!template[0].includes(agent(3)));
  assert.ok(template.every((part) => part.length <= 8));
  // Part 1 holds eight live agents, so the closed agent moves one part further.
  const holder = template.findIndex((part) => part.includes(agent(3)));
  assert.equal(template[holder][0], agent(3));
  assert.equal(holder, 2);
  assert.equal(closed.length, 1);
});

test("continued project parts restore identically in any reconnect order", () => {
  const members = numbers(1, 14).map(agent);
  const { model, tabs } = arrive(numbers(1, 14), members, null, "old");
  assert.deepEqual(
    partsOf(model, tabs).map((part) => part.length),
    [8, 6],
  );
  // Close a first-part pane (still a member) and add a new agent.
  tabs.splice(
    tabs.findIndex((item) => item.task.agentId === agent(3)),
    1,
  );
  synchronize(model, tabs);
  const roster = [...members, agent(15)];
  model.setTaskMembers(TASK, roster);
  tabs.push(member(15, "old"));
  synchronize(model, tabs);
  const expected = partsOf(model, tabs).map((part) => [...part].sort());
  assert.ok(expected[0].includes(agent(15)));
  const saved = normalizeWorkspace(
    workspaceSnapshot(
      tabs,
      model.groups,
      tabs[0].id,
      null,
      [TASK],
      [],
      model.projectLayoutSnapshot(),
    ),
  );
  const template = templateParts(saved.projectLayouts[0]);
  assert.equal(template[0].length, 8);
  assert.equal(template[1][0], agent(3));
  for (const [generation, order] of [
    ["a", [15, 14, 2, 9, 1, 12, 5, 7, 11, 4, 13, 6, 10, 8]],
    ["b", [8, 10, 6, 13, 4, 11, 7, 5, 12, 1, 9, 2, 14, 15]],
  ]) {
    const restored = arrive(order, roster, saved.projectLayouts, generation);
    assert.deepEqual(
      partsOf(restored.model, restored.tabs).map((part) => [...part].sort()),
      expected,
      `reconnect order ${generation}`,
    );
    restored.tabs.push(member(3, generation));
    synchronize(restored.model, restored.tabs);
    assert.ok(
      partsOf(restored.model, restored.tabs)[1].includes(agent(3)),
      "the closed agent returns to its recorded part",
    );
  }
});

test("snapshots keep continued parts and validate them", () => {
  const members = numbers(1, 12).map(agent);
  const { model, tabs } = arrive(numbers(1, 12), members, null, "old");
  const groups = structuredClone(model.groups);
  // A legacy snapshot without projectLayouts derives one layout per task.
  const legacy = normalizeWorkspace({
    ...workspaceSnapshot(tabs, groups, tabs[0].id),
    projectLayouts: undefined,
  });
  assert.equal(legacy.projectLayouts.length, 1);
  assert.equal(templateParts(legacy.projectLayouts[0])[0].length, 8);
  assert.equal(legacy.projectLayouts[0].continued.length, 1);
  assert.equal(templateParts(legacy.projectLayouts[0])[1].length, 4);
  assert.deepEqual(
    legacy.groups.map((group) => group.part),
    [undefined, 1],
  );
  // Plain series and part numbers survive; malformed values are dropped.
  const plain = normalizeWorkspace({
    tabs: workspaceSnapshot(tabs, groups, tabs[0].id).tabs,
    groups: [
      { tree: { tab: tabs[0].id }, series: "abc-123", part: 2 },
      { tree: { tab: tabs[1].id }, series: "bad series!", part: 0 },
      { tree: { tab: tabs[2].id }, part: 1.5 },
    ],
  });
  assert.deepEqual(
    plain.groups.map(({ series, part }) => ({ series, part })),
    [
      { series: "abc-123", part: 2 },
      { series: undefined, part: undefined },
      { series: undefined, part: undefined },
    ],
  );
  const layout = model.projectLayoutSnapshot()[0];
  assert.equal(
    normalizeProjectLayouts([layout])[0].continued.length,
    1,
    "continued parts round-trip",
  );
  const duplicate = {
    ...layout,
    continued: [{ agentId: agent(1) }, { agentId: agent(20) }],
  };
  assert.deepEqual(
    normalizeProjectLayouts([duplicate])[0].continued,
    [{ agentId: agent(20) }],
    "a leaf duplicated across parts is rejected",
  );
  const crowded = {
    taskId: TASK,
    tree: { agentId: agent(100) },
    continued: numbers(1, 40).map((n) => ({ agentId: agent(n) })),
  };
  const bounded = normalizeProjectLayouts([crowded])[0];
  assert.equal(
    1 + bounded.continued.length,
    32,
    "all parts share the 32-member budget",
  );
});
