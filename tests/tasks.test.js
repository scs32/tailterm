import { test } from "node:test";
import assert from "node:assert/strict";
import {
  matchServer,
  reconcileTask,
  taskRollup,
  applyEvents,
  MAX_TASK_PANES,
} from "../client/tasks.js";
import { normalizeTaskRef } from "../client/task-ref.js";

const servers = [
  {
    id: "s1",
    name: "Oracle 1",
    host: "oracle1.tail1234.ts.net",
    tailnet: true,
  },
  { id: "s2", name: "NAS", host: "100.116.238.37", tailnet: false },
  { id: "s3", name: "truenas-tailarr", host: "truenas-tailarr", tailnet: true },
  { id: "s4", name: "Oracle 1 plain", host: "oracle1", tailnet: false },
];
const agent = (n, extra = {}) => ({
  id: `agt_${n.toString(16).padStart(16, "0")}`,
  name: `a${n}`,
  host: "oracle1",
  session: `a${n}`,
  status: "running",
  ...extra,
});
const TASK = "tsk_0123456789abcdef";

test("matchServer prefers tailnet hosts, then exact hosts, then names", () => {
  assert.equal(matchServer("oracle1", servers).id, "s1");
  assert.equal(matchServer("ORACLE1.tail1234.ts.net", servers).id, "s1");
  assert.equal(matchServer("truenas-tailarr", servers).id, "s3");
  assert.equal(matchServer("nas", servers).id, "s2");
  assert.equal(matchServer("nowhere", servers), null);
  assert.equal(matchServer("", servers), null);
});

test("reconcileTask opens, adopts, closes, and reports unknown hosts", () => {
  const bound = agent(1);
  const tabs = [
    {
      id: "t1",
      server: servers[0],
      tmux: true,
      session: "a1",
      task: normalizeTaskRef({ taskId: TASK, agentId: bound.id }),
    },
    { id: "t2", server: servers[0], tmux: true, session: "a2" },
    {
      id: "t3",
      server: servers[0],
      tmux: true,
      session: "gone",
      task: normalizeTaskRef({ taskId: TASK, agentId: agent(9).id }),
    },
    { id: "t4", server: servers[0], tmux: true, session: "x", disposed: true },
  ];
  const agents = [
    bound,
    agent(2),
    agent(3),
    agent(4, { host: "mystery" }),
    agent(5, { status: "closed" }),
  ];
  const r = reconcileTask({ taskId: TASK, agents, tabs, servers });
  assert.deepEqual(
    r.open.map((o) => [o.agent.name, o.server.id]),
    [["a3", "s1"]],
  );
  assert.deepEqual(
    r.adopt.map((a) => [a.tab.id, a.agent.name]),
    [["t2", "a2"]],
  );
  assert.deepEqual(
    r.unknown.map((a) => a.name),
    ["a4"],
  );
  assert.deepEqual(
    r.close.map((t) => t.id),
    ["t3"],
  );
});

test("reconcileTask caps the panes it opens", () => {
  const agents = Array.from({ length: 50 }, (_, i) => agent(i + 1));
  const r = reconcileTask({ taskId: TASK, agents, tabs: [], servers });
  assert.equal(r.open.length, MAX_TASK_PANES);
});

test("taskRollup summarizes open agents", () => {
  assert.equal(taskRollup([]), "0 agents");
  assert.equal(
    taskRollup(
      [
        agent(1),
        agent(2, { status: "needs_input" }),
        agent(3, { status: "done" }),
        agent(4, { status: "closed" }),
      ],
      1,
    ),
    "3 agents · 1 needs input · 1 running · 1 done · 1 on unknown host",
  );
});

test("applyEvents updates statuses and raises attention labels", () => {
  const agents = [agent(1), agent(2)];
  const { attention, refresh } = applyEvents(agents, [
    { kind: "done", agentId: agents[0].id },
    { kind: "needs_input", agentId: agents[1].id },
    { kind: "message", agentId: agents[0].id, data: { to: "" } },
    { kind: "message", agentId: agents[1].id, data: { to: agents[0].id } },
    { kind: "agent_added", agentId: "agt_ffffffffffffffff" },
  ]);
  assert.equal(agents[0].status, "done");
  assert.equal(agents[1].status, "needs_input");
  assert.equal(attention.get(agents[0].id), "Task message");
  assert.equal(attention.get(agents[1].id), "Needs attention");
  assert.equal(refresh, true);
  applyEvents(agents, [{ kind: "task_closed" }]);
  assert.ok(agents.every((a) => a.status === "closed"));
});

test("cached terminal task state preserves a reported blocker across a runtime stop", () => {
  const agents = [agent(1)];
  applyEvents(agents, [
    {
      kind: "needs_input",
      agentId: agents[0].id,
      text: "Denied test command",
      data: { reason: "permission" },
    },
  ]);
  const result = applyEvents(agents, [
    { kind: "done", agentId: agents[0].id, data: { runtimeStop: true } },
  ]);
  assert.equal(agents[0].status, "needs_input");
  assert.equal(agents[0].blockedReason, "permission");
  assert.equal(result.attention.size, 0);
  applyEvents(agents, [{ kind: "running", agentId: agents[0].id }]);
  assert.equal(agents[0].status, "running");
  assert.equal(agents[0].blockedReason, "");
});

test("retired agents retain panes and ignore late lifecycle hooks until explicit resume", () => {
  const agents = [
    { id: "worker", status: "retired", host: "oracle1", session: "worker" },
  ];
  const before = reconcileTask({ taskId: "task", agents, tabs: [], servers });
  assert.equal(before.open.length, 1);
  assert.equal(before.close.length, 0);
  applyEvents(
    agents,
    ["running", "done", "needs_input"].map((kind) => ({
      kind,
      agentId: "worker",
    })),
  );
  assert.equal(agents[0].status, "retired");
  applyEvents(agents, [{ kind: "resumed", agentId: "worker" }]);
  assert.equal(agents[0].status, "done");
  applyEvents(agents, [{ kind: "retired", agentId: "worker" }]);
  assert.equal(agents[0].status, "retired");
});
