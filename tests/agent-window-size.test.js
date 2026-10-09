import test, { after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import vm from "node:vm";
import { agentWindowSizeCommand } from "../shared/tmux-command.js";
import {
  exitIfStepsStayPending,
  readTmuxFormat,
  runBounded,
  setupAgentWindowFixture,
  sizingReply,
  SIZING_TEST_TIMEOUT_MS,
  step,
  TMUX_FORMAT_END,
} from "./agent-window-size-fixture.js";
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

// Bound for a test that starts no private server. With the fixture's bound on
// the others, every test in this file ends within its own timeout.
const UNIT_TEST_TIMEOUT_MS = 20000;

// A step that timed out and never settled would keep this file's process
// running past its failed test; end it by name instead.
after(() => exitIfStepsStayPending());

// Runs one sizing command for this target and binding against the fixture's
// private server: bounded, and run again when a loaded host cut the reply.
// raw wraps the real run, for a test that injects replies.
function sizingRun(f, { target, binding }, { raw, ...options } = {}) {
  const real = (command) => {
    const r = runBounded("/bin/sh", ["-c", command], {
      env: f.env,
      socket: f.dir + "/socket",
    });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout;
  };
  return sizingReply({
    run: raw ? raw(real) : real,
    read: f.tmux,
    target,
    binding,
    ...options,
  });
}

test(
  "eligibility excludes hidden, background, helper, shell, unverified and obsolete panes",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  () => {
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
  },
);

test(
  "same-size focus claims again; resize never claims, and hidden retains no authority",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  async () => {
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
    await step(
      "same-size focus: the sizer settles after its first refresh",
      c.settled(),
    );
    assert.equal(commands.length, 2);
    s.cols = 240;
    c.refresh();
    c.refresh();
    await step(
      "same-size focus: the sizer settles after two resize refreshes",
      c.settled(),
    );
    assert.equal(commands.length, 3);
    assert.match(commands[2], /superseded/);
    c.refresh({ focus: true });
    await step(
      "same-size focus: the sizer settles after a focus refresh",
      c.settled(),
    );
    assert.equal(commands.length, 6);
    assert.notEqual(commands[1], commands[5]);
    s.visible = false;
    c.refresh();
    await step("same-size focus: the sizer settles after hiding", c.settled());
    assert.equal(commands.length, 7);
    s.cols = 16;
    c.refresh();
    await step(
      "same-size focus: the sizer settles after a hidden resize",
      c.settled(),
    );
    assert.equal(commands.length, 7);
    c.dispose();
    await step("same-size focus: the sizer settles after dispose", c.settled());
  },
);

test(
  "single flight rechecks current state after awaits and does not retry command failures",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  async () => {
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
    await step(
      "single flight: the sizer settles after its held inspect is answered",
      c.settled(),
    );
    assert.equal(
      commands.length,
      1,
      "hidden during inspect never sends a mutation",
    );
    assert.deepEqual(errors, []);
    c.dispose();
    await step("single flight: the sizer settles after dispose", c.settled());
    const fail = createAgentWindowSizer({
      snapshot: valid,
      token: () => "viewer_0000000000000001",
      execute: async () => {
        throw Error("SSH failed");
      },
      error: (e) => errors.push(e.message),
    });
    fail.refresh();
    await step(
      "single flight: the failing sizer settles after its first refresh",
      fail.settled(),
    );
    fail.refresh();
    await step(
      "single flight: the failing sizer settles after its second refresh",
      fail.settled(),
    );
    assert.deepEqual(errors, ["SSH failed"]);
    fail.dispose();
    await step(
      "single flight: the failing sizer settles after dispose",
      fail.settled(),
    );
  },
);

test(
  "ordinary adoption main callback preserves changed/deleted endpoints and reattaches unchanged profiles",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  () => {
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
  },
);

test(
  "private tmux controllers reject obsolete claims delayed past hide, dispose or reconnect",
  { timeout: SIZING_TEST_TIMEOUT_MS },
  async () => {
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
      const [id, created] = readTmuxFormat(
        f.tmux,
        [
          "display-message",
          "-p",
          "-t",
          "delayed",
          "#{session_id}|#{session_created}",
        ],
        { shape: /^\$\d+\|\d+$/ },
      ).split("|");
      const run = sizingRun(f, { target: { id, created }, binding });
      const state = () =>
        readTmuxFormat(
          f.tmux,
          [
            "display-message",
            "-p",
            "-t",
            id + ":agent",
            "#{pane_width}x#{pane_height}|#{@tailterm_size_viewer}",
          ],
          { shape: /^\d+x\d+\|[^|]*$/ },
        );
      for (const [i, transition] of [
        "hide",
        "dispose",
        "reconnect",
      ].entries()) {
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
          await step(
            `A's claim reaches the gate before ${transition}`,
            arrival,
          );
          a.foreground = false;
          if (transition === "hide") a.visible = false;
          if (transition === "dispose") A.dispose();
          if (transition === "reconnect") {
            a.connection++;
            a.connected = false;
          }
          A.refresh();
          B.refresh({ focus: true });
          await step(
            `B settles after focus while A is held, ${transition}`,
            B.settled(),
          );
          const winner = state();
          assert.match(winner, /^240x59\|delayed_B_/);
          release();
          await step(
            `A settles after its held claim is released, ${transition}`,
            A.settled(),
          );
          assert.equal(
            state(),
            winner,
            transition + " preserves newer B authority",
          );
          // Release retains the host revision, preventing an empty-token ABA.
          b.visible = false;
          B.refresh();
          await step(`B settles after hiding, ${transition}`, B.settled());
          assert.equal(state(), "240x59|");
        } finally {
          release();
          A.dispose();
          B.dispose();
          await step(`A settles in cleanup, ${transition}`, A.settled());
          await step(`B settles in cleanup, ${transition}`, B.settled());
        }
      }
    } finally {
      await f.cleanup();
    }
  },
);

test(
  "exact revision inspection across two tagged targets and unrelated default preserves refocus reconnect and grow",
  { timeout: SIZING_TEST_TIMEOUT_MS },
  async () => {
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
        const [id, created] = readTmuxFormat(
          f.tmux,
          [
            "display-message",
            "-p",
            "-t",
            name,
            "#{session_id}|#{session_created}",
          ],
          { shape: /^\$\d+\|\d+$/ },
        ).split("|");
        return {
          ...valid(),
          binding,
          target: { id, created },
          path: f.wrapper,
        };
      });
      f.tmux("new-session", "-d", "-s", "unrelated-default", "sleep 60");
      const unrelated = () =>
        readTmuxFormat(
          f.tmux,
          [
            "display-message",
            "-p",
            "-t",
            "unrelated-default",
            "#{pane_width}x#{pane_height}|#{@tailterm_size_revision}|#{@tailterm_size_viewer}",
          ],
          { shape: /^\d+x\d+\|[^|]*\|[^|]*$/ },
        );
      const before = unrelated();
      for (const [i, s] of fixtures.entries()) {
        const reply = sizingRun(f, s),
          run = (command) => reply(command).trim();
        let seq = 0;
        const token = () =>
          "target_viewer_00000000000" + i + String(++seq).padStart(4, "0");
        const state = () =>
          readTmuxFormat(
            f.tmux,
            [
              "display-message",
              "-p",
              "-t",
              s.target.id + ":agent",
              "#{pane_width}x#{pane_height}|#{@tailterm_size_viewer}|#{@tailterm_size_revision}",
            ],
            { shape: /^\d+x\d+\|[^|]*\|[^|]*$/ },
          );
        const c = createAgentWindowSizer({
          snapshot: () => s,
          token,
          execute: async (command) => run(command),
        });
        controllers.push(c);
        c.refresh({ focus: true });
        await step(`target ${i} settles after its first focus`, c.settled());
        const revision = readTmuxFormat(f.tmux, [
          "display-message",
          "-p",
          "-t",
          s.target.id + ":agent",
          "#{@tailterm_size_revision}",
        ]);
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
        await step(`target ${i} settles after a same-size focus`, c.settled());
        assert.match(state(), /^120x34\|target_viewer_/);
        assert.ok(
          !state().includes(revision),
          "same-size focus accepts a new exact-target token",
        );
        s.cols = 240;
        s.rows = 60;
        c.refresh();
        await step(`target ${i} settles after growing`, c.settled());
        assert.match(state(), /^240x59\|target_viewer_/);
        s.connected = false;
        c.refresh();
        await step(`target ${i} settles after disconnecting`, c.settled());
        const oldRevision = readTmuxFormat(f.tmux, [
          "display-message",
          "-p",
          "-t",
          s.target.id + ":agent",
          "#{@tailterm_size_revision}",
        ]);
        s.connection++;
        s.connected = true;
        c.refresh({ focus: true });
        await step(
          `target ${i} settles after its reconnect focus`,
          c.settled(),
        );
        assert.match(state(), /^240x59\|target_viewer_/);
        assert.ok(!state().includes(oldRevision));
        s.cols = 120;
        s.rows = 35;
        c.refresh();
        await step(`target ${i} settles after shrinking`, c.settled());
        assert.match(state(), /^120x34\|target_viewer_/);
        assert.equal(unrelated(), before, "unrelated default window untouched");
      }
    } finally {
      for (const c of controllers) {
        c.dispose();
        await step("a target controller settles in cleanup", c.settled());
      }
      await f.cleanup();
    }
  },
);

test(
  "a claim delivered after hide or dispose is restored to the prior size with no viewer and no further hidden work",
  { timeout: SIZING_TEST_TIMEOUT_MS },
  async (t) => {
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
        const [id, created] = readTmuxFormat(
          f.tmux,
          [
            "display-message",
            "-p",
            "-t",
            "residual-" + i,
            "#{session_id}|#{session_created}",
          ],
          { shape: /^\$\d+\|\d+$/ },
        ).split("|");
        f.tmux(
          "set-option",
          "-w",
          "-t",
          id + ":agent",
          "window-size",
          "manual",
        );
        f.tmux("resize-window", "-t", id + ":agent", "-x", "200", "-y", "50");
        const s = {
          ...valid(),
          cols,
          rows,
          target: { id, created },
          path: f.wrapper,
        };
        const state = () =>
          readTmuxFormat(
            f.tmux,
            [
              "display-message",
              "-p",
              "-t",
              id + ":agent",
              "#{pane_width}x#{pane_height}|#{@tailterm_size_viewer}",
            ],
            { shape: /^\d+x\d+\|[^|]*$/ },
          );
        const revision = () =>
          readTmuxFormat(f.tmux, [
            "display-message",
            "-p",
            "-t",
            id + ":agent",
            "#{@tailterm_size_revision}",
          ]);
        const run = sizingRun(f, s);
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
            return run(command);
          },
        });
        try {
          const before = state();
          assert.equal(before, "200x50|");
          c.refresh({ focus: true });
          await step(
            `residual ${i}: the claim reaches the gate before ${transition}`,
            arrival,
          );
          if (transition === "dispose") c.dispose();
          else {
            s.visible = false;
            s.foreground = false;
            c.refresh();
          }
          release();
          await step(
            `residual ${i}: the sizer settles after its held claim is released`,
            c.settled(),
          );
          assert.equal(
            state(),
            before,
            "delivered obsolete claim is undone and holds no authority",
          );
          assert.equal(commands.length, 3, "inspect, claim and one restore");
          assert.match(
            commands[1],
            new RegExp(
              `-x ${Math.max(80, cols)} -y ${Math.max(24, rows - 1)} `,
            ),
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
          await step(
            `residual ${i}: the sizer settles after a hidden refresh and focus`,
            c.settled(),
          );
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
          await step(
            `residual ${i}: the sizer settles in cleanup`,
            c.settled(),
          );
        }
      }
    } finally {
      await f.cleanup();
    }
  },
);

test(
  "restore yields to a newer viewer and a visible pane never restores",
  { timeout: SIZING_TEST_TIMEOUT_MS },
  async () => {
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
      const [id, created] = readTmuxFormat(
        f.tmux,
        [
          "display-message",
          "-p",
          "-t",
          "restore-race",
          "#{session_id}|#{session_created}",
        ],
        { shape: /^\$\d+\|\d+$/ },
      ).split("|");
      f.tmux("set-option", "-w", "-t", id + ":agent", "window-size", "manual");
      f.tmux("resize-window", "-t", id + ":agent", "-x", "200", "-y", "50");
      const run = sizingRun(f, { target: { id, created }, binding });
      const state = () =>
        readTmuxFormat(
          f.tmux,
          [
            "display-message",
            "-p",
            "-t",
            id + ":agent",
            "#{pane_width}x#{pane_height}|#{@tailterm_size_viewer}",
          ],
          { shape: /^\d+x\d+\|[^|]*$/ },
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
        await step("A's restore reaches the gate", arrival);
        assert.equal(state(), "120x34|restore_A_0000000000000001");
        B.refresh({ focus: true });
        await step(
          "B settles after focus while A's restore is held",
          B.settled(),
        );
        const winner = state();
        assert.equal(winner, "240x59|restore_B_0000000000000001");
        release();
        await step("A settles after its held restore is released", A.settled());
        assert.equal(state(), winner, "superseded restore changes nothing");
        assert.equal(sent.length, 3, "no command follows the restore");
        b.cols = 250;
        B.refresh();
        B.refresh({ focus: true });
        await step("B settles after a resize and refocus", B.settled());
        assert.match(state(), /^250x59\|restore_B_/);
        assert.ok(
          seen.every((command) => !command.includes("restored")),
          "visible eligible pane resizes and refocuses without restoring",
        );
      } finally {
        release();
        A.dispose();
        B.dispose();
        await step("restore race: A settles in cleanup", A.settled());
        await step("restore race: B settles in cleanup", B.settled());
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
  },
);

test(
  "a resize delivered after hide, dispose or reconnect is put back to the last visible size and never overwrites a newer viewer",
  { timeout: SIZING_TEST_TIMEOUT_MS },
  async (t) => {
    const f = setupAgentWindowFixture();
    try {
      const binding = valid().binding;
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
        const [id, created] = readTmuxFormat(
          f.tmux,
          [
            "display-message",
            "-p",
            "-t",
            "late-resize-" + i,
            "#{session_id}|#{session_created}",
          ],
          { shape: /^\$\d+\|\d+$/ },
        ).split("|");
        const state = () =>
          readTmuxFormat(
            f.tmux,
            [
              "display-message",
              "-p",
              "-t",
              id + ":agent",
              "#{pane_width}x#{pane_height}|#{@tailterm_size_viewer}",
            ],
            { shape: /^\d+x\d+\|[^|]*$/ },
          );
        const a = { ...valid(), target: { id, created }, path: f.wrapper };
        const b = { ...a, cols: 200, rows: 51 };
        const run = sizingRun(f, a);
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
          await step(
            `late resize ${i}: A settles after its first focus`,
            A.settled(),
          );
          assert.equal(state(), "120x34|late_resize_A_00000000000" + i);
          a.cols = 240;
          a.rows = 60;
          A.refresh();
          await step(
            `late resize ${i}: A's held command reaches the gate`,
            arrival,
          );
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
            await step(`late resize ${i}: B settles after focus`, B.settled());
            expected = "200x50|late_resize_B_00000000000" + i;
            assert.equal(state(), expected);
          }
          release();
          await step(
            `late resize ${i}: A settles after its held command is released`,
            A.settled(),
          );
          assert.equal(
            state(),
            expected,
            transition +
              (newer ? ", newer viewer " + newer : "") +
              ": the delayed size does not stand",
          );
          assert.doesNotMatch(state(), /^240x59/);
          if (!newer) {
            assert.equal(
              sent.length,
              5,
              "inspect, claim, resize, undo, release",
            );
            assert.match(sent[3], /-x 120 -y 34 .*sized/);
            assert.match(sent[4], /released/);
          }
          const count = sent.length;
          a.cols = 250;
          A.refresh();
          await step(
            `late resize ${i}: A settles after a hidden resize`,
            A.settled(),
          );
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
          await step(`late resize ${i}: A settles in cleanup`, A.settled());
          await step(`late resize ${i}: B settles in cleanup`, B.settled());
        }
      }
    } finally {
      await f.cleanup();
    }
  },
);

// One private tmux window at 200x50 and a sizer whose numbered commands can be
// held or lost: "request" never reaches the host, "reply" lands and then fails.
function lossySizer(f, name, inject) {
  const binding = valid().binding;
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
  const [id, created] = readTmuxFormat(
    f.tmux,
    ["display-message", "-p", "-t", name, "#{session_id}|#{session_created}"],
    { shape: /^\$\d+\|\d+$/ },
  ).split("|");
  f.tmux("set-option", "-w", "-t", id + ":agent", "window-size", "manual");
  f.tmux("resize-window", "-t", id + ":agent", "-x", "200", "-y", "50");
  const s = { ...valid(), target: { id, created }, path: f.wrapper };
  const run = sizingRun(f, s, inject);
  const sent = [],
    errors = [],
    hooks = {};
  const tokens = [];
  const sizer = createAgentWindowSizer({
    snapshot: () => s,
    token: () =>
      tokens[
        tokens.push(
          (name.replace(/-/g, "_") + "_A_000000000000").slice(0, 30) +
            String(tokens.length).padStart(2, "0"),
        ) - 1
      ],
    error: (e) => errors.push(e.message),
    execute: async (command) => {
      sent.push(command);
      const hook = hooks[sent.length] || {};
      await hook.before?.();
      if (hook.lose === "request") throw Error("transport lost");
      const out = run(command);
      if (hook.lose === "reply") throw Error("transport lost");
      return out;
    },
  });
  return {
    s,
    sent,
    errors,
    hooks,
    sizer,
    get token() {
      return tokens.at(-1);
    },
    run,
    state: () =>
      readTmuxFormat(
        f.tmux,
        [
          "display-message",
          "-p",
          "-t",
          id + ":agent",
          "#{pane_width}x#{pane_height}|#{@tailterm_size_viewer}",
        ],
        { shape: /^\d+x\d+\|[^|]*$/ },
      ),
    hide() {
      s.visible = false;
      s.foreground = false;
      sizer.refresh();
    },
    async resize(cols, rows) {
      s.cols = cols;
      s.rows = rows;
      sizer.refresh();
      await step(name + " settles after a resize", sizer.settled());
    },
    async focus() {
      sizer.refresh({ focus: true });
      await step(name + " settles after focus", sizer.settled());
    },
    async end() {
      sizer.dispose();
      await step(name + " settles after dispose", sizer.settled());
    },
  };
}

test(
  "a restore that is lost, or that follows a lost claim reply, still puts the prior size back and never overwrites a newer viewer",
  { timeout: SIZING_TEST_TIMEOUT_MS },
  async (t) => {
    const f = setupAgentWindowFixture();
    try {
      for (const transition of ["hide", "dispose"]) {
        const x = lossySizer(f, "lost-restore-" + transition);
        try {
          x.hooks[2] = {
            before: () =>
              transition === "hide" ? x.hide() : x.sizer.dispose(),
          };
          x.hooks[3] = { lose: "request" };
          await x.focus();
          assert.equal(x.state(), "200x50|", transition);
          assert.equal(
            x.sent.length,
            4,
            "inspect, claim, lost restore, restore",
          );
          assert.match(x.sent[3], /-x 200 -y 50 .*restored/);
          assert.deepEqual(
            x.errors,
            [],
            "a retry that succeeds reports nothing",
          );
          await x.resize(250, 60);
          assert.equal(x.sent.length, 4, "no further hidden work");
          t.diagnostic(`lost restore: ${transition} -> ${x.state()}`);
        } finally {
          await x.end();
        }
      }
      const reply = lossySizer(f, "lost-claim-reply");
      try {
        reply.hooks[2] = { before: () => reply.hide(), lose: "reply" };
        await reply.focus();
        assert.equal(reply.state(), "200x50|");
        assert.equal(reply.sent.length, 3, "inspect, claim, restore");
        assert.match(reply.sent[2], /-x 200 -y 50 .*restored/);
        assert.equal(reply.errors.length, 1);
        t.diagnostic(`claim reply lost after hide -> ${reply.state()}`);
      } finally {
        await reply.end();
      }
      const a = lossySizer(f, "lost-restore-newer");
      const b = { ...a.s, cols: 240, rows: 60 };
      const B = createAgentWindowSizer({
        snapshot: () => b,
        token: () => "lost_restore_newer_B_00000000001",
        execute: async (command) => a.run(command),
      });
      try {
        a.hooks[2] = { before: () => a.hide() };
        a.hooks[3] = { lose: "request" };
        a.hooks[4] = {
          before: async () => {
            B.refresh({ focus: true });
            await step(
              "lost-restore-newer: B settles after focus inside A's restore retry",
              B.settled(),
            );
          },
        };
        await a.focus();
        assert.equal(a.state(), "240x59|lost_restore_newer_B_00000000001");
        assert.equal(a.sent.length, 4, "the superseded retry ends the restore");
        await a.focus();
        assert.equal(a.sent.length, 4);
        assert.equal(a.state(), "240x59|lost_restore_newer_B_00000000001");
      } finally {
        B.dispose();
        await step("lost-restore-newer: B settles in cleanup", B.settled());
        await a.end();
      }
    } finally {
      await f.cleanup();
    }
  },
);

test(
  "a claim or visible resize whose reply is lost is the size put back after a later resize lands past hide",
  { timeout: SIZING_TEST_TIMEOUT_MS },
  async (t) => {
    const f = setupAgentWindowFixture();
    try {
      const claim = lossySizer(f, "lost-visible-claim");
      try {
        claim.hooks[2] = { lose: "reply" };
        await claim.focus();
        assert.equal(claim.state(), "120x34|" + claim.token);
        assert.equal(claim.sent.length, 2, "a visible pane is not restored");
        claim.hooks[3] = { before: () => claim.hide() };
        await claim.resize(240, 60);
        assert.equal(claim.state(), "120x34|");
        assert.equal(
          claim.sent.length,
          5,
          "inspect, claim, resize, undo, release",
        );
        assert.match(claim.sent[2], /-x 240 -y 59 .*sized/);
        assert.match(claim.sent[3], /-x 120 -y 34 .*sized/);
        assert.match(claim.sent[4], /released/);
        t.diagnostic(`claim reply lost while visible -> ${claim.state()}`);
      } finally {
        await claim.end();
      }
      const resize = lossySizer(f, "lost-visible-resize");
      try {
        await resize.focus();
        resize.hooks[3] = { lose: "reply" };
        await resize.resize(200, 51);
        assert.equal(resize.state(), "200x50|" + resize.token);
        assert.equal(resize.sent.length, 3, "a visible pane is not restored");
        resize.hooks[4] = { before: () => resize.hide() };
        await resize.resize(240, 60);
        assert.equal(resize.state(), "200x50|");
        assert.match(resize.sent[3], /-x 240 -y 59 .*sized/);
        assert.match(resize.sent[4], /-x 200 -y 50 .*sized/);
        assert.match(resize.sent[5], /released/);
        assert.equal(resize.sent.length, 6);
        t.diagnostic(`resize reply lost while visible -> ${resize.state()}`);
      } finally {
        await resize.end();
      }
    } finally {
      await f.cleanup();
    }
  },
);

test(
  "a lost corrective resize is retried at once, then reported and kept for the next focus or lifecycle change",
  { timeout: SIZING_TEST_TIMEOUT_MS },
  async (t) => {
    const f = setupAgentWindowFixture();
    try {
      const once = lossySizer(f, "lost-undo-once");
      try {
        await once.focus();
        once.hooks[3] = { before: () => once.hide() };
        once.hooks[4] = { lose: "request" };
        await once.resize(240, 60);
        assert.equal(once.state(), "120x34|");
        assert.equal(once.sent.length, 6, "resize, lost undo, undo, release");
        assert.match(once.sent[4], /-x 120 -y 34 .*sized/);
        assert.match(once.sent[5], /released/);
        assert.deepEqual(once.errors, []);
        t.diagnostic(`corrective resize lost once -> ${once.state()}`);
      } finally {
        await once.end();
      }
      const twice = lossySizer(f, "lost-undo-twice");
      try {
        await twice.focus();
        twice.hooks[3] = { before: () => twice.hide() };
        twice.hooks[4] = twice.hooks[5] = { lose: "request" };
        await twice.resize(240, 60); // settled() resolves with the undo unsent
        assert.deepEqual(twice.errors, ["transport lost"]);
        assert.equal(twice.sent.length, 5, "one retry, then no loop");
        assert.equal(twice.state(), "240x59|" + twice.token);
        await twice.resize(250, 60);
        assert.equal(twice.sent.length, 5, "layout alone does not retry");
        await twice.focus();
        assert.equal(twice.state(), "120x34|");
        assert.equal(
          twice.sent.length,
          7,
          "the kept claim is undone, released",
        );
        assert.match(twice.sent[5], /-x 120 -y 34 .*sized/);
        assert.match(twice.sent[6], /released/);
        await twice.focus();
        assert.equal(twice.sent.length, 7, "no further hidden work");
        t.diagnostic(
          `corrective resize lost twice, next focus -> ${twice.state()}`,
        );
      } finally {
        await twice.end();
      }
      // A kept claim neither blocks nor later undoes the pane's own next claim.
      const shown = lossySizer(f, "lost-undo-shown");
      try {
        await shown.focus();
        const first = shown.token;
        shown.hooks[3] = { before: () => shown.hide() };
        for (const n of [4, 5, 6, 7]) shown.hooks[n] = { lose: "request" };
        await shown.resize(240, 60);
        shown.s.visible = shown.s.foreground = true;
        await shown.focus();
        assert.deepEqual(shown.errors, ["transport lost", "transport lost"]);
        assert.equal(shown.sent.length, 9, "two lost rounds, inspect, claim");
        assert.notEqual(shown.token, first);
        assert.equal(shown.state(), "240x59|" + shown.token);
        await shown.focus();
        assert.equal(
          shown.sent.length,
          14,
          "the old undo and release, then release, inspect, claim",
        );
        assert.match(shown.sent[9], /-x 120 -y 34 .*sized/);
        for (const old of shown.sent.slice(9, 11))
          assert.ok(old.includes(first), "the old token is superseded");
        assert.equal(shown.state(), "240x59|" + shown.token);
        t.diagnostic(`kept claim, pane shown again -> ${shown.state()}`);
      } finally {
        await shown.end();
      }
    } finally {
      await f.cleanup();
    }
  },
);

test(
  "a cut sizing reply is run again while the identity holds, a lasting cut throws and a real refusal is returned at once",
  { timeout: SIZING_TEST_TIMEOUT_MS },
  async () => {
    const f = setupAgentWindowFixture();
    try {
      // What a loaded host returns when tmux cuts the format: a short inspect,
      // the else branch of a cut identity, and no status branch at all. Each is
      // returned once, without contacting tmux, as the first reply to that
      // kind of command, so a real cut during this test moves none of them.
      const cut = { inspect: "ready::200x\n", claim: "refused\n", resize: "" };
      const kind = (command) =>
        command.includes("ready:")
          ? "inspect"
          : command.includes("@tailterm_size_revision},")
            ? "claim"
            : command.includes("resize-window")
              ? "resize"
              : "other";
      // The re-run lines go to stderr. They are collected here instead, so a
      // count of that line in a run's output counts real cut replies only.
      const lines = [],
        write = process.stderr.write;
      process.stderr.write = (chunk, ...rest) =>
        String(chunk).startsWith("tmux sizing re-run")
          ? (lines.push(String(chunk)), true)
          : write.call(process.stderr, chunk, ...rest);
      let runs = 0,
        injected = 0;
      const x = lossySizer(f, "cut-reply", {
        raw: (real) => (command) => {
          runs++;
          const reply = cut[kind(command)];
          if (reply === undefined) return real(command);
          delete cut[kind(command)];
          injected++;
          return reply;
        },
      });
      try {
        await x.focus();
        assert.deepEqual(x.errors, [], "a cut reply is not a host refusal");
        assert.equal(x.sent.length, 2, "inspect and claim are each sent once");
        assert.equal(x.state(), "120x34|" + x.token);
        await x.resize(240, 60);
        assert.deepEqual(x.errors, []);
        assert.equal(x.sent.length, 3);
        assert.equal(x.state(), "240x59|" + x.token);
        assert.equal(injected, 3, "each kind of cut reply was returned once");
        assert.ok(runs >= 6, "each cut reply cost one more run");
        // A first re-run here can only follow an injected reply.
        const first = (line) => line.startsWith("tmux sizing re-run 1:");
        assert.deepEqual(lines.filter(first), [
          'tmux sizing re-run 1: "ready::200x" while identity holds\n',
          'tmux sizing re-run 1: "refused" while identity holds\n',
          'tmux sizing re-run 1: "" while identity holds\n',
        ]);
        const resize = agentWindowSizeCommand(
          { ...x.s, token: x.token, action: "resize" },
          f.wrapper,
        );
        // A host that refuses a valid identity every time still fails.
        const stuck = sizingRun(f, x.s, {
          raw: () => () => "refused\n",
          timeoutMs: 150,
          pauseMs: 10,
        });
        const beforeStuck = lines.length;
        assert.throws(
          () => stuck(resize),
          /^Error: tmux sizing reply stayed "refused" for 150 ms while identity holds: resize$/,
        );
        assert.equal(
          lines[beforeStuck],
          'tmux sizing re-run 1: "refused" while identity holds\n',
          "a reply that stays cut is written before it throws",
        );
        // A real refusal: the identity does not hold, so nothing runs twice.
        const written = lines.length;
        let refusals = 0;
        const refusing = sizingRun(f, x.s, {
          raw: (real) => (command) => (refusals++, real(command)),
        });
        f.tmux(
          "set-environment",
          "-t",
          x.s.target.id,
          "TAILTERM_ROLE",
          "owner_helper",
        );
        try {
          assert.equal(refusing(resize).trim(), "refused");
          assert.equal(refusals, 1, "a real refusal is returned at once");
          assert.equal(lines.length, written, "and is not reported as cut");
          assert.equal(x.state(), "240x59|" + x.token);
        } finally {
          f.tmux("set-environment", "-u", "-t", x.s.target.id, "TAILTERM_ROLE");
        }
      } finally {
        process.stderr.write = write;
        await x.end();
      }
    } finally {
      await f.cleanup();
    }
  },
);

test(
  "a pending step is rejected by name within its bound, for a gate and for a command that never returns",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  async () => {
    const started = Date.now();
    assert.equal(await step("a step that resolves", Promise.resolve(7), 40), 7);
    let open;
    await assert.rejects(
      step("A's claim reaches the gate", new Promise((r) => (open = r)), 40),
      /^Error: Agent sizing test step still pending after 40 ms: A's claim reaches the gate$/,
    );
    open(); // nothing stays pending behind this test
    const waiting = [];
    let answering = false;
    const c = createAgentWindowSizer({
      snapshot: valid,
      token: () => "viewer_0000000000000001",
      execute: (command) =>
        answering
          ? Promise.resolve(command.includes("ready:") ? "ready:" : "sized")
          : new Promise((r) => waiting.push(r)),
    });
    c.refresh({ focus: true });
    try {
      await assert.rejects(
        step("the sizer settles after focus", c.settled(), 40),
        /^Error: Agent sizing test step still pending after 40 ms: the sizer settles after focus$/,
      );
    } finally {
      // settled() polls while a command is held: answer it before leaving.
      answering = true;
      for (const answer of waiting) answer("ready:");
      c.dispose();
      await step("the sizer settles once its command is answered", c.settled());
    }
    assert.ok(Date.now() - started < 5000, "both hangs are reported fast");
  },
);

test(
  "a pending step that keeps the event loop alive ends its test file by name, and a file with none exits untouched",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  () => {
    const dir = mkdtempSync(tmpdir() + "/tailterm-pending-step-");
    const script = dir + "/held.mjs";
    const url = (file) => JSON.stringify(new URL(file, import.meta.url).href);
    // A test file of its own: with "held" its sizer's command never returns,
    // so settled() polls for good and only the after hook can end the process.
    writeFileSync(
      script,
      `import test, { after } from "node:test";
import { step, exitIfStepsStayPending } from ${url("./agent-window-size-fixture.js")};
import { createAgentWindowSizer } from ${url("../client/agent-window-size.js")};
after(() => exitIfStepsStayPending(200));
const held = process.argv[2] === "held";
test("child", async () => {
  const c = createAgentWindowSizer({
    snapshot: () => (${JSON.stringify(valid())}),
    token: () => "viewer_0000000000000001",
    execute: (command) =>
      held
        ? new Promise(() => {})
        : Promise.resolve(command.includes("ready:") ? "ready:" : "sized"),
  });
  c.refresh({ focus: true });
  await step("the child sizer settles after focus", c.settled(), 100);
  c.dispose();
  await step("the child sizer settles after dispose", c.settled(), 100);
});
`,
    );
    const env = Object.fromEntries(
      Object.entries(process.env).filter(([k]) => !k.startsWith("NODE_TEST")),
    );
    try {
      const started = Date.now();
      const stuck = runBounded(process.execPath, [script, "held"], {
        env,
        timeoutMs: 10000,
      });
      assert.equal(stuck.status, 1, stuck.stderr);
      assert.match(
        stuck.stderr,
        /^Agent sizing test file ended by force, step still pending: the child sizer settles after focus$/m,
      );
      assert.match(stuck.stdout, /child/, "its test had already reported");
      assert.ok(Date.now() - started < 5000, "the file ends fast");
      const clean = runBounded(process.execPath, [script], {
        env,
        timeoutMs: 10000,
      });
      assert.equal(clean.status, 0, clean.stdout + clean.stderr);
      assert.doesNotMatch(clean.stderr, /ended by force/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  },
);

test(
  "a teardown the host refuses or never answers is dropped after a bounded number of rounds",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  async () => {
    for (const mode of ["refuses", "never answers"]) {
      // Hidden pane whose host is gone: focus changes must not repeat forever.
      let s = valid(),
        sequence = 0,
        dead = false;
      const commands = [],
        errors = [];
      const c = createAgentWindowSizer({
        snapshot: () => s,
        token: () => `viewer_${String(++sequence).padStart(16, "0")}`,
        error: (e) => errors.push(e.message),
        execute: async (command) => {
          commands.push(command);
          if (command.includes("ready:")) return "ready::200x50";
          if (!/resize-window|restored/.test(command) && !dead)
            return "released";
          if (!dead || command.includes("@tailterm_size_revision},"))
            return "sized";
          if (mode === "refuses") return "refused";
          throw Error("transport lost");
        },
      });
      c.refresh({ focus: true });
      await step(
        `teardown ${mode}: the sizer settles after its first focus`,
        c.settled(),
      );
      assert.equal(commands.length, 2, mode);
      dead = true;
      s.visible = false;
      c.refresh();
      await step(
        `teardown ${mode}: the sizer settles after the host dies and the pane hides`,
        c.settled(),
      );
      for (let i = 0; i < 3; i++) {
        c.refresh({ focus: true });
        await step(
          `teardown ${mode}: the sizer settles after hidden focus ${i}`,
          c.settled(),
        );
      }
      // One refusal is final; a silent host gets three rounds of two attempts.
      assert.equal(commands.length, mode === "refuses" ? 3 : 8, mode);
      assert.equal(errors.length, mode === "refuses" ? 1 : 3, mode);
      for (let i = 0; i < 20; i++) {
        c.refresh({ focus: true });
        await step(
          `teardown ${mode}: the sizer settles after settled focus ${i}`,
          c.settled(),
        );
      }
      assert.equal(commands.length, mode === "refuses" ? 3 : 8, mode);
      assert.equal(errors.length, mode === "refuses" ? 1 : 3, mode);

      // Ten identity changes, each leaving a claim on a target that is gone.
      s = { ...valid(), connection: 2 };
      c.refresh({ focus: true });
      await step(
        `teardown ${mode}: the sizer settles after a focus on connection 2`,
        c.settled(),
      );
      for (let i = 3; i < 13; i++) {
        s = { ...s, connection: i };
        const before = commands.length;
        c.refresh();
        await step(
          `teardown ${mode}: the sizer settles after the change to connection ${i}`,
          c.settled(),
        );
        assert.ok(
          commands.length - before <= 3 * 2 + 2,
          mode + ": at most three kept claims are retried per change",
        );
      }
      s.visible = false;
      for (let i = 0; i < 4; i++) {
        c.refresh({ focus: true });
        await step(
          `teardown ${mode}: the sizer settles after hidden focus ${i} with kept claims`,
          c.settled(),
        );
      }
      const count = commands.length,
        reported = errors.length;
      for (let i = 0; i < 20; i++) {
        c.refresh({ focus: true });
        await step(
          `teardown ${mode}: the sizer settles after final focus ${i}`,
          c.settled(),
        );
      }
      assert.equal(commands.length, count, mode + ": nothing is owed any more");
      assert.equal(errors.length, reported, mode);
      c.dispose();
      await step(
        `teardown ${mode}: the sizer settles after dispose`,
        c.settled(),
      );
      assert.equal(commands.length, count, mode);
    }
  },
);

test(
  "tmux format read returns a complete reading after a cut one",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  () => {
    const formats = [],
      values = ["240x", "240x59|viewer_1" + TMUX_FORMAT_END];
    const run = (...args) => (formats.push(args.at(-1)), values.shift());
    assert.equal(
      readTmuxFormat(run, ["display-message", "-p", "#{a}x#{b}|#{c}"], {
        pauseMs: 1,
      }),
      "240x59|viewer_1",
    );
    assert.deepEqual(
      formats,
      Array(2).fill("#{a}x#{b}|#{c}" + TMUX_FORMAT_END),
    );
  },
);

test(
  "tmux format read keeps a legitimately empty field on the first read",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  () => {
    let reads = 0;
    const run = () => (reads++, "240x59|" + TMUX_FORMAT_END + "\n");
    assert.equal(
      readTmuxFormat(run, ["display-message", "-p", "#{a}|#{c}"], {
        shape: /^\d+x\d+\|[^|]*$/,
      }),
      "240x59|",
    );
    assert.equal(reads, 1);
    assert.equal(
      readTmuxFormat(() => TMUX_FORMAT_END, ["-p", "#{c}"]),
      "",
    );
  },
);

test(
  "tmux format read re-reads a complete reading that fails its shape",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  () => {
    const values = ["x59", "240x59", "1x2\n3x"].map((v) =>
      v.replaceAll(/$/gm, TMUX_FORMAT_END),
    );
    const run = () => values.shift();
    assert.equal(
      readTmuxFormat(run, ["-p", "#{a}x#{b}"], {
        shape: /^\d+x\d+$/,
        pauseMs: 1,
      }),
      "240x59",
    );
    assert.throws(
      () =>
        readTmuxFormat(run, ["list-windows", "-F", "#{a}x#{b}"], {
          shape: /^\d+x\d+$/,
          timeoutMs: 0,
        }),
      /incomplete tmux reading after 1 reads/,
    );
  },
);

test(
  "tmux format read throws loudly when every reading stays cut",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  () => {
    let reads = 0;
    const started = Date.now();
    assert.throws(
      () =>
        readTmuxFormat(() => (reads++, "240x"), ["-p", "#{a}x#{b}"], {
          timeoutMs: 60,
          pauseMs: 5,
        }),
      (e) =>
        /^incomplete tmux reading after \d+ reads: #\{a\}x#\{b\} got "240x"$/.test(
          e.message,
        ) && e.message.includes(`after ${reads} reads`),
    );
    assert.ok(reads > 1 && Date.now() - started < 2000, String(reads));
    assert.throws(
      () => readTmuxFormat(() => "", ["-p", "#{a}"], { timeoutMs: 0 }),
      /incomplete tmux reading after 1 reads: #\{a\} got ""/,
    );
  },
);

test(
  "tmux format read lets the runner's own error through",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  () => {
    let reads = 0;
    const run = () => {
      reads++;
      throw Error("no server running");
    };
    assert.throws(
      () => readTmuxFormat(run, ["-p", "#{a}"]),
      /^Error: no server running$/,
    );
    assert.equal(reads, 1);
  },
);

test(
  "a reply tmux cut short is reported as incomplete, and only refused as a refusal",
  { timeout: UNIT_TEST_TIMEOUT_MS },
  async () => {
    const revision = "viewer_0000000000000009";
    const replies = [
      [
        "incomplete",
        /^Agent pane sizing got an incomplete reply from the host\.$/,
      ],
      ["", /incomplete reply/],
      [`ready:${revision}:`, /incomplete reply/],
      ["refused", /^Agent pane sizing was refused by the host\.$/],
    ];
    // The reply answers the first read of the window, or the claim after it.
    for (const site of ["inspect", "claim"])
      for (const [reply, message] of replies) {
        const errors = [],
          label = `${site} answered ${JSON.stringify(reply)}`;
        const c = createAgentWindowSizer({
          snapshot: valid,
          token: () => "viewer_0000000000000001",
          error: (e) => errors.push(e.message),
          execute: async (command) =>
            site === "claim" && command.includes("ready:")
              ? "ready::200x50"
              : reply + "\n",
        });
        c.refresh({ focus: true });
        await step(`${label}: the sizer settles`, c.settled());
        assert.equal(errors.length, 1, label);
        assert.match(errors[0], message, label);
        assert.equal(/refused/.test(errors[0]), reply === "refused", label);
        c.dispose();
        await step(`${label}: the sizer settles once disposed`, c.settled());
      }
  },
);
