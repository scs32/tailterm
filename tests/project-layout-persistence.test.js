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
