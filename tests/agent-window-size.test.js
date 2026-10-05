import test from "node:test";
import assert from "node:assert/strict";
import {
  createAgentWindowSizer,
  eligibleAgentViewport,
} from "../client/agent-window-size.js";

const valid = () => ({
  staticMode: true,
  connected: true,
  tmux: true,
  verified: true,
  ignoreSize: true,
  binding: {
    taskId: "tsk_0000000000000001",
    agentId: "agt_0000000000000001",
    runId: "run_0000000000000001",
  },
  mode: "terminals",
  active: true,
  foreground: true,
  visible: true,
  cols: 120,
  rows: 35,
  target: { id: "$1", created: "1234" },
  connection: 1,
});

test("eligibility excludes hidden, background, helper, shell, unverified and obsolete panes", () => {
  assert.equal(eligibleAgentViewport(valid()), true);
  for (const [key, value] of Object.entries({
    staticMode: false,
    connected: false,
    tmux: false,
    verified: false,
    ignoreSize: false,
    home: true,
    disposed: true,
    locked: true,
    mode: "board",
    active: false,
    foreground: false,
    visible: false,
    cols: 0,
    rows: 0,
    target: null,
  }))
    assert.equal(
      eligibleAgentViewport({ ...valid(), [key]: value }),
      false,
      key,
    );
  assert.equal(
    eligibleAgentViewport({
      ...valid(),
      binding: { ...valid().binding, role: "owner_helper" },
    }),
    false,
  );
});

test("same-size focus claims again; resize never claims, and hidden retains no authority", async () => {
  let s = valid(),
    sequence = 0;
  const commands = [];
  const c = createAgentWindowSizer({
    snapshot: () => s,
    token: () => `viewer_${String(++sequence).padStart(16, "0")}`,
    execute: async (command) => {
      commands.push(command);
      return command.includes("released") ? "released" : "sized";
    },
  });
  c.refresh();
  await c.settled();
  assert.equal(commands.length, 1);
  s.cols = 240;
  c.refresh();
  c.refresh();
  await c.settled();
  assert.equal(commands.length, 2);
  assert.match(commands[1], /superseded/);
  c.refresh({ focus: true });
  await c.settled();
  assert.equal(commands.length, 4);
  assert.notEqual(commands[0], commands[3]);
  s.visible = false;
  c.refresh();
  await c.settled();
  assert.equal(commands.length, 5);
  s.cols = 16;
  c.refresh();
  await c.settled();
  assert.equal(commands.length, 5);
  c.dispose();
  await c.settled();
});

test("single flight rechecks current state after awaits and does not retry command failures", async () => {
  let s = valid(),
    finish;
  const commands = [],
    errors = [];
  const c = createAgentWindowSizer({
    snapshot: () => s,
    token: () => "viewer_0000000000000001",
    execute: (command) => {
      commands.push(command);
      return commands.length === 1
        ? new Promise((r) => {
            finish = r;
          })
        : Promise.resolve("released");
    },
    error: (e) => errors.push(e.message),
  });
  c.refresh();
  await new Promise((r) => setTimeout(r, 0));
  s.visible = false;
  s.cols = 16;
  c.refresh();
  finish("sized");
  await c.settled();
  assert.equal(commands.length, 2);
  assert.match(commands[1], /released/);
  assert.deepEqual(errors, []);
  c.dispose();
  await c.settled();
  const fail = createAgentWindowSizer({
    snapshot: valid,
    token: () => "viewer_0000000000000001",
    execute: async () => {
      throw Error("SSH failed");
    },
    error: (e) => errors.push(e.message),
  });
  fail.refresh();
  await fail.settled();
  fail.refresh();
  await fail.settled();
  assert.deepEqual(errors, ["SSH failed"]);
  fail.dispose();
  await fail.settled();
});
