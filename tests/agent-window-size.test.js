import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { spawnSync } from "node:child_process";
import { agentWindowSizeCommand } from "../shared/tmux-command.js";
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

test("exact revision inspection across two tagged targets and unrelated default preserves refocus reconnect and grow", async () => {
  const f = setupAgentWindowFixture();
  const controllers = [];
  try {
    const fixtures = ["target-X", "target-Y"].map((name, i) => {
      const binding = {
        ...valid().binding,
        agentId: "agt_000000000000000" + (i + 1),
      };
      f.tmux(
        "new-session",
        "-d",
        "-s",
        name,
        "-n",
        "agent",
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
          name,
          "#{session_id}|#{session_created}",
        )
        .split("|");
      return { ...valid(), binding, target: { id, created }, path: f.wrapper };
    });
    f.tmux("new-session", "-d", "-s", "unrelated-default", "sleep 60");
    const unrelated = () =>
      f.tmux(
        "display-message",
        "-p",
        "-t",
        "unrelated-default",
        "#{pane_width}x#{pane_height}|#{@tailterm_size_revision}|#{@tailterm_size_viewer}",
      );
    const before = unrelated();
    const run = (command) => {
      const r = spawnSync("/bin/sh", ["-c", command], {
        env: f.env,
        encoding: "utf8",
      });
      assert.equal(r.status, 0, r.stderr);
      return r.stdout.trim();
    };
    for (const [i, s] of fixtures.entries()) {
      let seq = 0;
      const token = () =>
        "target_viewer_00000000000" + i + String(++seq).padStart(4, "0");
      const state = () =>
        f.tmux(
          "display-message",
          "-p",
          "-t",
          s.target.id + ":agent",
          "#{pane_width}x#{pane_height}|#{@tailterm_size_viewer}|#{@tailterm_size_revision}",
        );
      const c = createAgentWindowSizer({
        snapshot: () => s,
        token,
        execute: async (command) => run(command),
      });
      controllers.push(c);
      c.refresh({ focus: true });
      await c.settled();
      const revision = f.tmux(
        "display-message",
        "-p",
        "-t",
        s.target.id + ":agent",
        "#{@tailterm_size_revision}",
      );
      assert.ok(revision);
      assert.equal(
        run(
          agentWindowSizeCommand(
            { ...s, token: revision, action: "inspect" },
            f.wrapper,
          ),
        ),
        "ready:" + revision + ":120x34",
      );
      c.refresh({ focus: true });
      await c.settled();
      assert.match(state(), /^120x34\|target_viewer_/);
      assert.ok(
        !state().includes(revision),
        "same-size focus accepts a new exact-target token",
      );
      s.cols = 240;
      s.rows = 60;
      c.refresh();
      await c.settled();
      assert.match(state(), /^240x59\|target_viewer_/);
      s.connected = false;
      c.refresh();
      await c.settled();
      const oldRevision = f.tmux(
        "display-message",
        "-p",
        "-t",
        s.target.id + ":agent",
        "#{@tailterm_size_revision}",
      );
      s.connection++;
      s.connected = true;
      c.refresh({ focus: true });
      await c.settled();
      assert.match(state(), /^240x59\|target_viewer_/);
      assert.ok(!state().includes(oldRevision));
      s.cols = 120;
      s.rows = 35;
      c.refresh();
      await c.settled();
      assert.match(state(), /^120x34\|target_viewer_/);
      assert.equal(unrelated(), before, "unrelated default window untouched");
    }
  } finally {
    for (const c of controllers) {
      c.dispose();
      await c.settled();
    }
    await f.cleanup();
  }
});

test("a claim delivered after hide or dispose is restored to the prior size with no viewer and no further hidden work", async (t) => {
  const f = setupAgentWindowFixture();
  try {
    for (const [i, scenario] of [
      { transition: "hide", cols: 120, rows: 35 },
      { transition: "dispose", cols: 120, rows: 35 },
      { transition: "hide", cols: 16, rows: 2 },
      { transition: "dispose", cols: 16, rows: 2 },
    ].entries()) {
      const { transition, cols, rows } = scenario;
      const commands = [];
      const binding = valid().binding;
      f.tmux(
        "new-session",
        "-d",
        "-s",
        "residual-" + i,
        "-n",
        "agent",
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
          "residual-" + i,
          "#{session_id}|#{session_created}",
        )
        .split("|");
      f.tmux("set-option", "-w", "-t", id + ":agent", "window-size", "manual");
      f.tmux("resize-window", "-t", id + ":agent", "-x", "200", "-y", "50");
      const s = {
        ...valid(),
        cols,
        rows,
        target: { id, created },
        path: f.wrapper,
      };
      const state = () =>
        f.tmux(
          "display-message",
          "-p",
          "-t",
          id + ":agent",
          "#{pane_width}x#{pane_height}|#{@tailterm_size_viewer}",
        );
      const revision = () =>
        f.tmux(
          "display-message",
          "-p",
          "-t",
          id + ":agent",
          "#{@tailterm_size_revision}",
        );
      let release,
        arrived,
        held = false;
      const gate = new Promise((r) => (release = r)),
        arrival = new Promise((r) => (arrived = r));
      const c = createAgentWindowSizer({
        snapshot: () => s,
        token: () => "residual_viewer_000000000" + i,
        execute: async (command) => {
          commands.push(command);
          if (!held && command.includes("resize-window")) {
            held = true;
            arrived();
            await gate;
          }
          const r = spawnSync("/bin/sh", ["-c", command], {
            env: f.env,
            encoding: "utf8",
          });
          assert.equal(r.status, 0, r.stderr);
          return r.stdout;
        },
      });
      try {
        const before = state();
        assert.equal(before, "200x50|");
        c.refresh({ focus: true });
        await arrival;
        if (transition === "dispose") c.dispose();
        else {
          s.visible = false;
          s.foreground = false;
          c.refresh();
        }
        release();
        await c.settled();
        assert.equal(
          state(),
          before,
          "delivered obsolete claim is undone and holds no authority",
        );
        assert.equal(commands.length, 3, "inspect, claim and one restore");
        assert.match(
          commands[1],
          new RegExp(`-x ${Math.max(80, cols)} -y ${Math.max(24, rows - 1)} `),
        );
        assert.match(commands[2], /-x 200 -y 50 .*restored/);
        assert.equal(
          revision(),
          "residual_viewer_000000000" + i,
          "restore keeps the advanced revision",
        );
        const deliveredCount = commands.length;
        s.visible = false;
        s.foreground = false;
        s.cols = 240;
        s.rows = 60;
        c.refresh();
        c.refresh({ focus: true });
        await c.settled();
        assert.equal(
          commands.length,
          deliveredCount,
          "no new hidden/disposed query, claim, mutation or retry",
        );
        t.diagnostic(
          "delayed claim restored: " +
            transition +
            " submitted " +
            cols +
            "x" +
            rows +
            " -> " +
            state() +
            "; no further hidden work",
        );
      } finally {
        release();
        c.dispose();
        await c.settled();
      }
    }
  } finally {
    await f.cleanup();
  }
});

test("restore yields to a newer viewer and a visible pane never restores", async () => {
  const f = setupAgentWindowFixture();
  try {
    const binding = valid().binding;
    f.tmux(
      "new-session",
      "-d",
      "-s",
      "restore-race",
      "-n",
      "agent",
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
        "restore-race",
        "#{session_id}|#{session_created}",
      )
      .split("|");
    f.tmux("set-option", "-w", "-t", id + ":agent", "window-size", "manual");
    f.tmux("resize-window", "-t", id + ":agent", "-x", "200", "-y", "50");
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
    const a = { ...valid(), target: { id, created }, path: f.wrapper };
    const b = { ...a, cols: 240, rows: 60 };
    const sent = [];
    let release, arrived;
    const gate = new Promise((r) => (release = r)),
      arrival = new Promise((r) => (arrived = r));
    const A = createAgentWindowSizer({
      snapshot: () => a,
      token: () => "restore_A_0000000000000001",
      execute: async (command) => {
        sent.push(command);
        if (command.includes("resize-window") && sent.length === 2)
          a.visible = false; // hidden while the claim is in transport
        if (command.includes("restored")) {
          arrived();
          await gate;
        }
        return run(command);
      },
    });
    const seen = [];
    const B = createAgentWindowSizer({
      snapshot: () => b,
      token: () => "restore_B_0000000000000001",
      execute: async (command) => {
        seen.push(command);
        return run(command);
      },
    });
    try {
      A.refresh({ focus: true });
      await arrival;
      assert.equal(state(), "120x34|restore_A_0000000000000001");
      B.refresh({ focus: true });
      await B.settled();
      const winner = state();
      assert.equal(winner, "240x59|restore_B_0000000000000001");
      release();
      await A.settled();
      assert.equal(state(), winner, "superseded restore changes nothing");
      assert.equal(sent.length, 3, "no command follows the restore");
      b.cols = 250;
      B.refresh();
      B.refresh({ focus: true });
      await B.settled();
      assert.match(state(), /^250x59\|restore_B_/);
      assert.ok(
        seen.every((command) => !command.includes("restored")),
        "visible eligible pane resizes and refocuses without restoring",
      );
    } finally {
      release();
      A.dispose();
      B.dispose();
      await A.settled();
      await B.settled();
    }
    const s = { ...valid(), target: { id, created } };
    const restore = agentWindowSizeCommand({
      ...s,
      token: "restore_A_0000000000000001",
      action: "restore",
      cols: 16,
      rows: 2,
    });
    assert.match(restore, /-x 80 -y 24 /);
    for (const guard of [
      "TAILTERM_TASK",
      "TAILTERM_AGENT",
      "TAILTERM_RUN",
      "session_created",
      "window_panes",
      "@tailterm_size_viewer",
    ])
      assert.ok(restore.includes(guard), guard);
    assert.ok(!restore.includes("@tailterm_size_revision"));
    for (const bad of [
      { cols: 0 },
      { rows: 10001 },
      { cols: "200" },
      { token: "short" },
      { binding: { ...s.binding, role: "owner_helper" } },
    ])
      assert.throws(() =>
        agentWindowSizeCommand({
          ...s,
          token: "restore_A_0000000000000001",
          action: "restore",
          cols: 200,
          rows: 50,
          ...bad,
        }),
      );
  } finally {
    await f.cleanup();
  }
});

test("a resize delivered after hide, dispose or reconnect is put back to the last visible size and never overwrites a newer viewer", async (t) => {
  const f = setupAgentWindowFixture();
  try {
    const binding = valid().binding;
    const run = (command) => {
      const r = spawnSync("/bin/sh", ["-c", command], {
        env: f.env,
        encoding: "utf8",
      });
      assert.equal(r.status, 0, r.stderr);
      return r.stdout;
    };
    for (const [i, scenario] of [
      { transition: "hide" },
      { transition: "dispose" },
      { transition: "reconnect" },
      { transition: "hide", newer: "before delivery" },
      { transition: "hide", newer: "after delivery" },
    ].entries()) {
      const { transition, newer } = scenario;
      f.tmux(
        "new-session",
        "-d",
        "-s",
        "late-resize-" + i,
        "-n",
        "agent",
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
          "late-resize-" + i,
          "#{session_id}|#{session_created}",
        )
        .split("|");
      const state = () =>
        f.tmux(
          "display-message",
          "-p",
          "-t",
          id + ":agent",
          "#{pane_width}x#{pane_height}|#{@tailterm_size_viewer}",
        );
      const a = { ...valid(), target: { id, created }, path: f.wrapper };
      const b = { ...a, cols: 200, rows: 51 };
      const sent = [];
      let release, arrived;
      const gate = new Promise((r) => (release = r)),
        arrival = new Promise((r) => (arrived = r));
      // Hold the resize itself, or the command that follows its delivery.
      const heldAt = newer === "after delivery" ? 4 : 3;
      const A = createAgentWindowSizer({
        snapshot: () => a,
        token: () => "late_resize_A_00000000000" + i,
        execute: async (command) => {
          sent.push(command);
          if (newer === "after delivery" && sent.length === 3) hide();
          if (sent.length === heldAt) {
            arrived();
            await gate;
          }
          return run(command);
        },
      });
      const B = createAgentWindowSizer({
        snapshot: () => b,
        token: () => "late_resize_B_00000000000" + i,
        execute: async (command) => run(command),
      });
      function hide() {
        a.foreground = false;
        if (transition === "hide") a.visible = false;
        if (transition === "dispose") A.dispose();
        if (transition === "reconnect") {
          a.connection++;
          a.connected = false;
        }
        A.refresh();
      }
      try {
        A.refresh({ focus: true });
        await A.settled();
        assert.equal(state(), "120x34|late_resize_A_00000000000" + i);
        a.cols = 240;
        a.rows = 60;
        A.refresh();
        await arrival;
        if (newer === "after delivery")
          // The pane hid in transport and the resize landed; a newer viewer
          // now claims before A's next command is delivered.
          assert.equal(state(), "240x59|late_resize_A_00000000000" + i);
        else {
          assert.match(sent[2], /-x 240 -y 59 .*sized/);
          assert.equal(
            state(),
            "120x34|late_resize_A_00000000000" + i,
            "the resize is still held in transport",
          );
          hide();
        }
        let expected = "120x34|";
        if (newer) {
          B.refresh({ focus: true });
          await B.settled();
          expected = "200x50|late_resize_B_00000000000" + i;
          assert.equal(state(), expected);
        }
        release();
        await A.settled();
        assert.equal(
          state(),
          expected,
          transition +
            (newer ? ", newer viewer " + newer : "") +
            ": the delayed size does not stand",
        );
        assert.doesNotMatch(state(), /^240x59/);
        if (!newer) {
          assert.equal(sent.length, 5, "inspect, claim, resize, undo, release");
          assert.match(sent[3], /-x 120 -y 34 .*sized/);
          assert.match(sent[4], /released/);
        }
        const count = sent.length;
        a.cols = 250;
        A.refresh();
        await A.settled();
        assert.equal(sent.length, count, "no further hidden work");
        assert.equal(state(), expected);
        t.diagnostic(
          "delayed resize: " +
            transition +
            (newer ? ", newer viewer " + newer : "") +
            " -> " +
            state(),
        );
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
