import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { spawnSync } from "node:child_process";
import { setupAgentWindowFixture } from "./agent-window-size-fixture.js";
import { endpointKey } from "../client/workspace-state.js";
import { helperReattach, homeAgent, reattachOptions } from "../client/tasks.js";
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
      return command.includes("ready:")
        ? "ready:"
        : command.includes("released")
          ? "released"
          : "sized";
    },
  });
  c.refresh();
  await c.settled();
  assert.equal(commands.length, 2);
  s.cols = 240;
  c.refresh();
  c.refresh();
  await c.settled();
  assert.equal(commands.length, 3);
  assert.match(commands[2], /superseded/);
  c.refresh({ focus: true });
  await c.settled();
  assert.equal(commands.length, 6);
  assert.notEqual(commands[1], commands[5]);
  s.visible = false;
  c.refresh();
  await c.settled();
  assert.equal(commands.length, 7);
  s.cols = 16;
  c.refresh();
  await c.settled();
  assert.equal(commands.length, 7);
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
  finish("ready:");
  await c.settled();
  assert.equal(
    commands.length,
    1,
    "hidden during inspect never sends a mutation",
  );
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

test("ordinary adoption main callback preserves changed/deleted endpoints and reattaches unchanged profiles", () => {
  const source = readFileSync(
    new URL("../client/main.js", import.meta.url),
    "utf8",
  );
  const fn = source.slice(
    source.indexOf("function reattachBoundAgent(t)"),
    source.indexOf("\nfunction tabVisible(t)"),
  );
  for (const change of [
    "host",
    "port",
    "username",
    "tmuxPath",
    "deleted",
    "unchanged",
  ]) {
    const original = {
      id: "profile",
      host: "host-a",
      port: 22,
      username: "user",
      tmuxPath: "",
    };
    const current = { ...original };
    if (!["deleted", "unchanged"].includes(change))
      current[change] = change === "port" ? 23 : "changed";
    const t = {
      ...valid(),
      id: "original",
      server: original,
      task: valid().binding,
      session: "agent",
      tmuxVerified: true,
      attachIgnoresSize: false,
    };
    const calls = [];
    vm.runInNewContext(fn + "\nreattachBoundAgent(t)", {
      t,
      staticMode: true,
      active: t.id,
      data: { servers: change === "deleted" ? [] : [current] },
      endpointKey,
      helperReattach,
      homeAgent,
      reattachOptions,
      notice() {},
      connect(...args) {
        calls.push(args);
        return Promise.resolve();
      },
    });
    assert.equal(calls.length, change === "unchanged" ? 1 : 0, change);
    assert.equal(t.server, original);
    if (calls.length) {
      assert.equal(calls[0][0], current);
      assert.equal(calls[0][3].replace, t);
      assert.equal(calls[0][3].target, t.target);
      assert.equal(calls[0][3].home, false);
    }
  }
});

test("private tmux controllers reject obsolete claims delayed past hide, dispose or reconnect", async () => {
  const f = setupAgentWindowFixture();
  try {
    const binding = valid().binding;
    f.tmux(
      "new-session",
      "-d",
      "-s",
      "delayed",
      "-n",
      "agent",
      "-x",
      "200",
      "-y",
      "50",
      "-e",
      "TAILTERM_TASK=" + binding.taskId,
      "-e",
      "TAILTERM_AGENT=" + binding.agentId,
      "-e",
      "TAILTERM_RUN=" + binding.runId,
      "sleep 60",
    );
    const [id, created] = f
      .tmux(
        "display-message",
        "-p",
        "-t",
        "delayed",
        "#{session_id}|#{session_created}",
      )
      .split("|");
    const run = (command) => {
      const r = spawnSync("/bin/sh", ["-c", command], {
        env: f.env,
        encoding: "utf8",
      });
      assert.equal(r.status, 0, r.stderr);
      return r.stdout;
    };
    const state = () =>
      f.tmux(
        "display-message",
        "-p",
        "-t",
        id + ":agent",
        "#{pane_width}x#{pane_height}|#{@tailterm_size_viewer}",
      );
    for (const [i, transition] of ["hide", "dispose", "reconnect"].entries()) {
      let release, arrived;
      const gate = new Promise((r) => (release = r)),
        arrival = new Promise((r) => (arrived = r));
      const a = { ...valid(), target: { id, created }, path: f.wrapper };
      const b = { ...a, cols: 240, rows: 60 };
      let held = false;
      const A = createAgentWindowSizer({
        snapshot: () => a,
        token: () => "delayed_A_000000000000000" + i,
        execute: async (command) => {
          if (!held && command.includes("resize-window")) {
            held = true;
            arrived();
            await gate;
          }
          return run(command);
        },
      });
      const B = createAgentWindowSizer({
        snapshot: () => b,
        token: () => "delayed_B_000000000000000" + i,
        execute: async (command) => run(command),
      });
      try {
        A.refresh({ focus: true });
        await arrival;
        a.foreground = false;
        if (transition === "hide") a.visible = false;
        if (transition === "dispose") A.dispose();
        if (transition === "reconnect") {
          a.connection++;
          a.connected = false;
        }
        A.refresh();
        B.refresh({ focus: true });
        await B.settled();
        const winner = state();
        assert.match(winner, /^240x59\|delayed_B_/);
        release();
        await A.settled();
        assert.equal(
          state(),
          winner,
          transition + " preserves newer B authority",
        );
        // Release retains the host revision, preventing an empty-token ABA.
        b.visible = false;
        B.refresh();
        await B.settled();
        assert.equal(state(), "240x59|");
      } finally {
        release();
        A.dispose();
        B.dispose();
        await A.settled();
        await B.settled();
      }
    }
  } finally {
    await f.cleanup();
  }
});
