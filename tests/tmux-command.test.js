import test from "node:test";
import assert from "node:assert/strict";
import {
  mkdtempSync,
  writeFileSync,
  readFileSync,
  rmSync,
  existsSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import {
  tmuxCommand,
  tmuxListCommand,
  validateTmuxPath,
  tmuxRenameCommand,
  tmuxHistoryCommand,
  agentWindowSizeCommand,
  shellQuote,
} from "../shared/tmux-command.js";
import { MAX_WORK_CONTEXT_BYTES } from "../shared/work-context.js";
import {
  readTmuxFormat,
  runBounded,
  sizingReply,
  TMUX_CALL_TIMEOUT_MS,
} from "./agent-window-size-fixture.js";

test("real private tmux sizing serializes claims and rejects stale viewers and identities", async () => {
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-size-"));
  const binary = path.join(dir, "tmux");
  writeFileSync(
    binary,
    `#!/bin/sh\nexec tmux -f /dev/null -S ${shellQuote(dir + "/socket")} "$@"\n`,
    { mode: 0o700 },
  );
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      ([k]) => !k.startsWith("TAILTERM_") && k !== "TMUX",
    ),
  );
  const socket = dir + "/socket";
  const run = (...args) => {
    const r = runBounded(binary, args, { env, socket });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout.trim();
  };
  const shell = (command) => {
    const r = runBounded("/bin/sh", ["-c", command], { env, socket });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout;
  };
  const binding = {
    taskId: "tsk_0000000000000001",
    agentId: "agt_0000000000000001",
    runId: "run_0000000000000001",
  };
  try {
    run(
      "new-session",
      "-d",
      "-s",
      "sizing",
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
      run,
      [
        "display-message",
        "-p",
        "-t",
        "sizing",
        "#{session_id}|#{session_created}",
      ],
      { shape: /^\$\d+\|\d+$/ },
    ).split("|");
    const target = { id, created };
    const params = {
      target,
      binding,
      token: "viewer_0000000000000001",
      cols: 120,
      rows: 35,
      action: "claim",
      expectedRevision: "",
    };
    // A reply cut by a loaded host is run again; a real refusal, whose
    // identity does not hold for this call's target and binding, is not.
    const invoke = (changes = {}) => {
      const call = {
        ...params,
        expectedRevision: readTmuxFormat(run, [
          "display-message",
          "-p",
          "-t",
          id + ":agent",
          "#{@tailterm_size_revision}",
        ]),
        ...changes,
      };
      return {
        stdout: sizingReply({
          run: shell,
          read: run,
          target: call.target,
          binding: call.binding,
        })(agentWindowSizeCommand(call, binary)),
      };
    };
    const size = () =>
      readTmuxFormat(
        run,
        [
          "display-message",
          "-p",
          "-t",
          id + ":agent",
          "#{pane_width}x#{pane_height}",
        ],
        { shape: /^\d+x\d+$/ },
      );
    const globals = run("show-options", "-g") + run("show-options", "-gw");
    assert.equal(invoke().stdout.trim(), "sized");
    assert.equal(size(), "120x34");
    assert.equal(
      invoke({
        token: "viewer_0000000000000002",
        cols: 240,
        rows: 60,
      }).stdout.trim(),
      "sized",
    );
    for (const action of ["resize", "release"]) {
      assert.equal(invoke({ action }).stdout.trim(), "superseded");
      assert.equal(size(), "240x59");
    }
    assert.equal(
      invoke().stdout.trim(),
      "sized",
      "same-size refocus takes authority",
    );
    for (const status of ["off", "on", "2", "5"]) {
      run("set-option", "-t", id, "status", status);
      assert.equal(invoke({ cols: 16, rows: 2 }).stdout.trim(), "sized");
      assert.equal(size(), "80x24");
      assert.equal(invoke().stdout.trim(), "sized");
      assert.equal(
        size(),
        "120x" + (35 - ({ off: 0, on: 1 }[status] ?? +status)),
      );
    }
    const before = size();
    for (const changes of [
      { target: { id, created: "0" } },
      { binding: { ...binding, runId: "run_0000000000000002" } },
    ]) {
      assert.equal(invoke(changes).stdout.trim(), "refused");
      assert.equal(size(), before);
    }
    run("set-environment", "-t", id, "TAILTERM_ROLE", "owner_helper");
    assert.equal(invoke().stdout.trim(), "refused");
    assert.equal(size(), before);
    run("set-environment", "-u", "-t", id, "TAILTERM_ROLE");
    const revision = readTmuxFormat(run, [
      "display-message",
      "-p",
      "-t",
      id + ":agent",
      "#{@tailterm_size_revision}",
    ]);
    const contenders = Array.from({ length: 8 }, (_, i) => ({
      token: `concurrent_${String(i).padStart(16, "0")}`,
      cols: 130 + i,
      rows: 45 + i,
    }));
    await Promise.all(
      contenders.map(
        (c) =>
          new Promise((resolve, reject) => {
            const command = agentWindowSizeCommand(
              { ...params, expectedRevision: revision, ...c },
              binary,
            );
            const p = spawn("/bin/sh", ["-c", command], { env });
            let output = "",
              stderr = "";
            const timer = setTimeout(() => {
              p.kill("SIGKILL");
              reject(
                Error(
                  `tmux command hung after ${TMUX_CALL_TIMEOUT_MS} ms: ${command}`,
                ),
              );
            }, TMUX_CALL_TIMEOUT_MS);
            p.stdout.on("data", (d) => (output += d));
            p.stderr.on("data", (d) => (stderr += d));
            p.on("error", (e) => {
              clearTimeout(timer);
              reject(e);
            });
            p.on("close", (code) => {
              clearTimeout(timer);
              try {
                assert.equal(code, 0, stderr);
                assert.ok(["sized", "superseded"].includes(output.trim()));
                resolve();
              } catch (e) {
                reject(e);
              }
            });
          }),
      ),
    );
    const winner = run(
      "show-option",
      "-w",
      "-v",
      "-t",
      id + ":agent",
      "@tailterm_size_viewer",
    );
    const expected = contenders.find((c) => c.token === winner);
    assert.ok(expected);
    assert.equal(
      size(),
      `${expected.cols}x${expected.rows - 5}`,
      "concurrent claim size and token belong to one viewer",
    );
    run("rename-session", "-t", id, "renamed");
    assert.equal(
      invoke().stdout.trim(),
      "sized",
      "exact identity survives rename",
    );
    run("new-session", "-d", "-s", "sizing", "sleep 60");
    assert.equal(invoke({ cols: 240 }).stdout.trim(), "sized");
    assert.equal(
      readTmuxFormat(
        run,
        ["display-message", "-p", "-t", "sizing", "#{pane_width}"],
        { shape: /^\d+$/ },
      ),
      "80",
      "reused name is untouched",
    );
    run("split-window", "-d", "-t", id + ":agent", "sleep 60");
    assert.equal(
      invoke().stdout.trim(),
      "refused",
      "never sizes an arbitrary split pane",
    );
    assert.equal(
      run("show-options", "-g") + run("show-options", "-gw"),
      globals,
    );
    assert.throws(
      () => agentWindowSizeCommand({ ...params, cols: NaN }),
      /viewport/,
    );
    assert.throws(
      () => agentWindowSizeCommand({ ...params, token: "'unsafe" }),
      /identity/,
    );
    assert.throws(
      () =>
        agentWindowSizeCommand({
          ...params,
          binding: { ...binding, role: "owner_helper" },
        }),
      /identity/,
    );
  } finally {
    try {
      runBounded(binary, ["kill-server"], { env, socket });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  }
});

// tmux 3.7 stops a format expansion at 100 ms and expands the rest to nothing.
// The wrapper makes that happen to the identity guard of the next `cuts`
// sizing commands by putting a format too slow to finish in front of it; every
// later format, including the server's own check of a refusal, is untouched.
test("a cut identity reading is read again and never reported as a refusal", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-cut-"));
  const socket = dir + "/socket",
    binary = path.join(dir, "tmux");
  const slow = Array.from({ length: 14 }).reduce(
    (format) => `#{W:${format}}`,
    "#{window_id}",
  );
  writeFileSync(
    binary,
    `#!/bin/sh
if [ "$1" = if-shell ]; then
  printf 'guard\\n' >> ${shellQuote(dir + "/guards")}
  cuts=$(cat ${shellQuote(dir + "/cuts")})
  if [ "$cuts" -gt 0 ]; then
    printf '%s\\n' "$((cuts - 1))" > ${shellQuote(dir + "/cuts")}
    exec tmux -f /dev/null -S ${shellQuote(socket)} if-shell -F -t "$4" "#{&&:#{!=:${slow},},$5}" "$6" "$7"
  fi
fi
exec tmux -f /dev/null -S ${shellQuote(socket)} "$@"
`,
    { mode: 0o700 },
  );
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      ([k]) => !k.startsWith("TAILTERM_") && k !== "TMUX",
    ),
  );
  const run = (...args) => {
    const r = runBounded(binary, args, { env, socket });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout.trim();
  };
  // Runs one generated command with the next `cuts` identity guards cut.
  const under = (cuts, command) => {
    writeFileSync(dir + "/cuts", cuts + "\n");
    writeFileSync(dir + "/guards", "");
    const r = runBounded("/bin/sh", ["-c", command], { env, socket });
    return {
      status: r.status,
      reply: r.stdout.trim(),
      stderr: r.stderr,
      guards: readFileSync(dir + "/guards", "utf8").split("\n").length - 1,
    };
  };
  const binding = {
    taskId: "tsk_0000000000000001",
    agentId: "agt_0000000000000001",
    runId: "run_0000000000000001",
  };
  try {
    run(
      "new-session",
      "-d",
      "-s",
      "sizing",
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
    // The slow format walks these windows; the guarded one stays single-pane.
    for (let i = 0; i < 3; i++)
      run("new-window", "-d", "-t", "sizing", "sleep 60");
    const [id, created] = readTmuxFormat(
      run,
      [
        "display-message",
        "-p",
        "-t",
        "sizing",
        "#{session_id}|#{session_created}",
      ],
      { shape: /^\$\d+\|\d+$/ },
    ).split("|");
    const target = { id, created };
    const token = "viewer_0000000000000001";
    const sizing = (changes = {}) =>
      agentWindowSizeCommand(
        {
          target,
          binding,
          token,
          action: "resize",
          cols: 120,
          rows: 35,
          ...changes,
        },
        binary,
      );
    const size = () =>
      readTmuxFormat(
        run,
        [
          "display-message",
          "-p",
          "-t",
          id + ":agent",
          "#{window_width}x#{window_height}",
        ],
        { shape: /^\d+x\d+$/ },
      );
    const windowSize = () =>
      run("show-options", "-w", "-v", "-t", id + ":agent", "window-size");
    run("set-option", "-t", id, "status", "off");

    // The wrapper's cut is real: a guard that holds reads false once cut. The
    // counts below are lower bounds, since a loaded host cuts readings too.
    const guard = (cuts) =>
      under(
        cuts,
        `${shellQuote(binary)} if-shell -F -t ${shellQuote(id + ":agent")} '#{==:#{window_name},agent}' 'display-message -p whole' 'display-message -p cut'`,
      ).reply;
    assert.equal(guard(1), "cut");

    const claimed = under(0, sizing({ action: "claim", expectedRevision: "" }));
    assert.deepEqual(
      [claimed.reply, claimed.status, size()],
      ["sized", 0, "120x35"],
      "a whole reading sizes",
    );

    const reread = under(2, sizing({ cols: 150 }));
    assert.deepEqual(
      [reread.reply, reread.status, reread.guards >= 3, size()],
      ["sized", 0, true, "150x35"],
      "two cut readings are read again and the third sizes",
    );

    const before = size();
    for (const action of ["inspect", "claim", "resize", "release", "restore"]) {
      const cut = under(
        99,
        sizing({ action, cols: 170, rows: 40, expectedRevision: token }),
      );
      assert.deepEqual(
        [cut.reply, cut.status, cut.guards],
        ["incomplete", 75, 5],
        action + " with every reading cut",
      );
      assert.match(
        cut.stderr,
        /did not finish reading the agent pane identity/,
      );
      assert.equal(size(), before, action + " changed nothing");
    }
    assert.equal(
      run(
        "show-options",
        "-w",
        "-v",
        "-t",
        id + ":agent",
        "@tailterm_size_viewer",
      ),
      token,
      "a cut release keeps the viewer",
    );

    // A real mismatch is proved by the server in the same command, so it is
    // refused at once whether or not the guard itself was cut.
    const refusals = [
      ["wrong created time", { target: { id, created: "0" } }],
      ["wrong run", { binding: { ...binding, runId: "run_0000000000000002" } }],
      ["missing session", { target: { id: "$99999", created } }],
    ];
    for (const cuts of [0, 1])
      for (const [label, changes] of refusals) {
        const refused = under(cuts, sizing({ cols: 180, ...changes }));
        assert.deepEqual(
          [refused.reply, refused.status, size()],
          ["refused", 0, before],
          `${label} with ${cuts} cut`,
        );
      }

    // The helper policy guards the same way. An owner helper is also the one
    // identity an agent sizing command must refuse.
    run("set-environment", "-t", id, "TAILTERM_ROLE", "owner_helper");
    for (const cuts of [0, 1]) {
      const refused = under(cuts, sizing({ cols: 180 }));
      assert.deepEqual(
        [refused.reply, refused.status, size()],
        ["refused", 0, before],
        `owner helper role with ${cuts} cut`,
      );
    }
    const helper = (changes = {}) =>
      tmuxCommand("sizing", binary, true, target, "", {
        ignoreSize: true,
        helperBinding: { ...binding, role: "owner_helper", ...changes },
      });
    assert.equal(windowSize(), "manual");
    const unchecked = under(99, helper());
    assert.deepEqual([unchecked.status, unchecked.guards], [1, 5]);
    assert.match(unchecked.stderr, /^Helper sizing was not checked: /);
    assert.doesNotMatch(unchecked.stderr, /refused|tmux failed/);
    assert.equal(windowSize(), "manual");
    for (const cuts of [0, 1]) {
      const refused = under(cuts, helper({ runId: "run_0000000000000002" }));
      assert.equal(refused.status, 1);
      assert.match(refused.stderr, /^Helper sizing refused: /);
      assert.doesNotMatch(refused.stderr, /tmux failed/);
      assert.equal(windowSize(), "manual", "a refused helper changes nothing");
    }
    // Past the guard the command attaches, which needs a terminal this test
    // does not have: tmux itself fails, after the policy was applied.
    const ready = under(2, helper());
    assert.ok(ready.guards >= 3, "two cut readings were read again");
    assert.doesNotMatch(ready.stderr, /Helper sizing/);
    assert.match(ready.stderr, /tmux failed with exit status/);
    assert.equal(windowSize(), "latest");
  } finally {
    try {
      runBounded(binary, ["kill-server"], { env, socket });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  }
});

// A private server behind a stand-in tmux that cuts chosen formats the way a
// loaded host does. cut(find, count) puts a format too slow to finish in front
// of `find` in the next `count` commands that carry it: tmux then expands that
// field and everything after it to nothing and still exits 0. Only the first
// place is cut, or every place with { all: true }. Read counts are asserted as
// lower bounds, since a loaded host cuts readings of its own.
function cutFixture(t) {
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-cuts-"));
  const socket = dir + "/socket",
    binary = path.join(dir, "tmux"),
    state = dir + "/cut.json";
  const slow = Array.from({ length: 14 }).reduce(
    (format) => `#{W:${format}}`,
    "#{window_id}",
  );
  writeFileSync(
    dir + "/stand-in.mjs",
    `import { readFileSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
let args = process.argv.slice(2);
const state = JSON.parse(readFileSync(${JSON.stringify(state)}, "utf8"));
const at = state.find ? args.findIndex((a) => a.includes(state.find)) : -1;
if (at >= 0) {
  state.seen++;
  if (state.count > 0) {
    state.count--;
    const cut = () => ${JSON.stringify(`#{?#{!=:${slow},},,}`)} + state.find;
    args = args.map((a, i) =>
      state.all ? a.replaceAll(state.find, cut) : i === at ? a.replace(state.find, cut) : a,
    );
  }
  writeFileSync(${JSON.stringify(state)}, JSON.stringify(state));
}
const r = spawnSync("tmux", ["-f", "/dev/null", "-S", ${JSON.stringify(socket)}, ...args], { stdio: "inherit" });
process.exit(r.status ?? 1);
`,
  );
  writeFileSync(
    binary,
    `#!/bin/sh\nexec ${shellQuote(process.execPath)} ${shellQuote(dir + "/stand-in.mjs")} "$@"\n`,
    { mode: 0o700 },
  );
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      ([k]) => !k.startsWith("TAILTERM_") && k !== "TMUX",
    ),
  );
  const cut = (find = "", count = 0, { all = false } = {}) =>
    writeFileSync(state, JSON.stringify({ find, count, all, seen: 0 }));
  const run = (...args) => {
    cut();
    const r = runBounded(binary, args, { env, socket });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout.trim();
  };
  t.after(() => {
    try {
      cut();
      runBounded(binary, ["kill-server"], { env, socket });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
  const binding = {
    taskId: "tsk_0000000000000001",
    agentId: "agt_0000000000000001",
    runId: "run_0000000000000001",
  };
  run(
    "new-session",
    "-d",
    "-s",
    "lab",
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
    "printf 'LAB-HISTORY\\n'; sleep 60",
  );
  // The slow format walks these windows; the agent window stays current.
  for (let i = 0; i < 3; i++) run("new-window", "-d", "-t", "lab", "sleep 60");
  const window = "lab:agent";
  const read = (format, shape, target = window) =>
    readTmuxFormat(run, ["display-message", "-p", "-t", target, format], {
      shape,
    });
  const [id, created] = read(
    "#{session_id}|#{session_created}",
    /^\$\d+\|\d+$/,
  ).split("|");
  return {
    binary,
    binding,
    run,
    read,
    window,
    target: { id, created },
    size: () => read("#{window_width}x#{window_height}", /^\d+x\d+$/),
    option: (name) => run("show-options", "-w", "-v", "-t", window, name),
    // Runs one generated command with the next `count` readings of find cut.
    under(find, count, command, options) {
      cut(find, count, options);
      const r = runBounded("/bin/sh", ["-c", command], { env, socket });
      return {
        status: r.status,
        stdout: r.stdout,
        reply: r.stdout.trim(),
        stderr: r.stderr,
        reads: JSON.parse(readFileSync(state, "utf8")).seen,
      };
    },
  };
}
const CUT_TEST_TIMEOUT_MS = 120000;

test(
  "a cut session identity is read again, and a lasting cut is never a missing session",
  { timeout: CUT_TEST_TIMEOUT_MS },
  (t) => {
    const f = cutFixture(t);
    // A foreign name with the separator in it never makes the list look cut.
    f.run("new-session", "-d", "-s", "odd|name", "sleep 60");
    const gone = /no longer exists/,
      unread =
        /^tmux did not finish reading the session identity\. Try again\.\n$/;
    const exact = [
      "exact identity",
      "|#{session_created}",
      (target = f.target) => tmuxHistoryCommand(target, f.binary),
      /^The original tmux session no longer exists\. /,
    ];
    const named = [
      "session name",
      "|#{session_id}",
      (name = "lab") => tmuxCommand(name, f.binary, true),
      /^That tmux session no longer exists\. /,
    ];
    const whole = f.under("", 0, exact[2]());
    assert.equal(whole.status, 0, whole.stderr);
    assert.match(whole.stdout, /LAB-HISTORY/);
    for (const [label, find, command] of [exact, named]) {
      const reread = f.under(find, 2, command());
      assert.doesNotMatch(reread.stderr, gone, label);
      assert.doesNotMatch(reread.stderr, /did not finish/, label);
      assert.ok(reread.reads >= 3, `${label}: two cut readings read again`);
      const lasting = f.under(find, 99, command());
      assert.deepEqual([lasting.status, lasting.reads], [75, 5], label);
      assert.match(lasting.stderr, unread, label);
      assert.equal(lasting.stdout, "", label);
    }
    // Past its identity the history command prints the pane; the attach has no
    // terminal here, so tmux itself fails after the session was found.
    const history = f.under(exact[1], 2, exact[2]());
    assert.equal(history.status, 0, history.stderr);
    assert.match(history.stdout, /LAB-HISTORY/);
    assert.match(
      f.under(named[1], 2, named[2]()).stderr,
      /tmux failed with exit status/,
    );
    // A session that is really gone or replaced is still answered as such.
    for (const cuts of [0, 1]) {
      for (const [label, target] of [
        ["wrong created time", { ...f.target, created: "0" }],
        ["missing session", { id: "$99999", created: f.target.created }],
      ]) {
        const refused = f.under(exact[1], cuts, exact[2](target));
        assert.equal(refused.status, 1, `${label} with ${cuts} cut`);
        assert.match(refused.stderr, exact[3], `${label} with ${cuts} cut`);
        assert.equal(refused.stdout, "");
      }
      const absent = f.under(named[1], cuts, named[2]("absent"));
      assert.equal(absent.status, 1, `absent name with ${cuts} cut`);
      assert.match(absent.stderr, named[3], `absent name with ${cuts} cut`);
    }
  },
);

test(
  "a cut helper window reading is read again, and a lasting cut is unchecked, never refused",
  { timeout: CUT_TEST_TIMEOUT_MS },
  (t) => {
    const f = cutFixture(t);
    f.run(
      "set-environment",
      "-t",
      f.target.id,
      "TAILTERM_ROLE",
      "owner_helper",
    );
    // The helper attach names no target: the created time comes from this read.
    const helper = tmuxCommand("lab", f.binary, true, undefined, "", {
      ignoreSize: true,
      helperBinding: { ...f.binding, role: "owner_helper" },
    });
    for (const find of ["|#{session_created}", "#{window_id}|"]) {
      f.run("set-option", "-w", "-t", f.window, "window-size", "manual");
      const lasting = f.under(find, 99, helper);
      assert.match(lasting.stderr, /^Helper sizing was not checked: /, find);
      assert.doesNotMatch(
        lasting.stderr,
        /refused|Invalid helper window identity|tmux failed/,
        find,
      );
      assert.deepEqual([lasting.status, lasting.reads], [1, 5], find);
      assert.equal(f.option("window-size"), "manual", find);
      // Past the policy the attach has no terminal here and tmux itself fails.
      const reread = f.under(find, 2, helper);
      assert.ok(reread.reads >= 3, `${find}: two cut readings read again`);
      assert.doesNotMatch(reread.stderr, /Helper sizing|Invalid/, find);
      assert.match(reread.stderr, /tmux failed with exit status/, find);
      assert.equal(f.option("window-size"), "latest", find);
    }
  },
);

test(
  "a cut pane reading never shows another session's history",
  { timeout: CUT_TEST_TIMEOUT_MS },
  (t) => {
    const f = cutFixture(t);
    f.run(
      "new-session",
      "-d",
      "-s",
      "other",
      "printf 'OTHER-SESSION-SECRET\\n'; sleep 60",
    );
    const history = tmuxHistoryCommand(f.target, f.binary);
    const lasting = f.under("#{pane_id}", 99, history);
    assert.deepEqual(
      [lasting.status, lasting.stdout, lasting.reads],
      [75, "", 5],
    );
    assert.match(
      lasting.stderr,
      /^tmux did not finish reading the pane\. Try again\.\n$/,
    );
    const reread = f.under("#{pane_id}", 2, history);
    assert.equal(reread.status, 0, reread.stderr);
    assert.ok(reread.reads >= 3, "two cut readings read again");
    assert.match(reread.stdout, /LAB-HISTORY/);
    assert.doesNotMatch(reread.stdout, /OTHER-SESSION-SECRET/);
  },
);

test(
  "a cut inspect line is read again and is never answered short",
  { timeout: CUT_TEST_TIMEOUT_MS },
  (t) => {
    const f = cutFixture(t);
    const token = "viewer_0000000000000001";
    const sizing = (action) =>
      agentWindowSizeCommand(
        {
          target: f.target,
          binding: f.binding,
          token,
          action,
          cols: 120,
          rows: 35,
          expectedRevision: "",
        },
        f.binary,
      );
    f.run("set-option", "-t", f.target.id, "status", "off");
    assert.equal(f.under("", 0, sizing("claim")).reply, "sized");
    const ready = `ready:${token}:120x35`;
    assert.equal(f.under("", 0, sizing("inspect")).reply, ready);
    for (const find of [
      "#{@tailterm_size_revision}",
      "#{window_width}",
      "#{window_height}",
    ]) {
      const reread = f.under(find, 2, sizing("inspect"));
      assert.deepEqual(
        [reread.reply, reread.status, reread.reads >= 3],
        [ready, 0, true],
        find + " cut twice",
      );
      const lasting = f.under(find, 99, sizing("inspect"));
      assert.deepEqual(
        [lasting.reply, lasting.status, lasting.reads],
        ["incomplete", 75, 5],
        find + " cut every time",
      );
    }
  },
);

test(
  "a cut comparison never sizes, releases or claims for the wrong viewer",
  { timeout: CUT_TEST_TIMEOUT_MS },
  (t) => {
    const f = cutFixture(t);
    const A = "viewer_000000000000000A",
      B = "viewer_000000000000000B";
    const sizing = (changes) =>
      agentWindowSizeCommand(
        {
          target: f.target,
          binding: f.binding,
          token: A,
          action: "resize",
          cols: 120,
          rows: 35,
          ...changes,
        },
        f.binary,
      );
    const viewer = "#{@tailterm_size_viewer}",
      revision = "#{@tailterm_size_revision}";
    const held = () => [
      f.size(),
      f.option("@tailterm_size_viewer"),
      f.option("@tailterm_size_revision"),
    ];
    f.run("set-option", "-t", f.target.id, "status", "off");
    assert.equal(
      f.under("", 0, sizing({ action: "claim", expectedRevision: "" })).reply,
      "sized",
    );
    const before = held();
    assert.deepEqual(before, ["120x35", A, A]);
    const other = { token: B, cols: 170, rows: 40 };
    // The comparison alone is cut: the server proves the viewer differs.
    for (const [find, changes] of [
      [viewer, { action: "resize" }],
      [viewer, { action: "release" }],
      [viewer, { action: "restore" }],
      [revision, { action: "claim", expectedRevision: B }],
    ]) {
      const label = `${changes.action} by another viewer`;
      const once = f.under(find, 1, sizing({ ...other, ...changes }));
      assert.deepEqual([once.reply, once.status], ["superseded", 0], label);
      assert.deepEqual(held(), before, label + " changed nothing");
      // Its proof is cut too: nothing is decided, and nothing is changed.
      const lasting = f.under(find, 99, sizing({ ...other, ...changes }), {
        all: true,
      });
      assert.deepEqual(
        [lasting.reply, lasting.status, lasting.reads],
        ["incomplete", 75, 5],
        label + " with every reading cut",
      );
      assert.deepEqual(held(), before, label + " changed nothing");
    }
    // The viewer that holds the window is read again and then sized.
    const own = f.under(viewer, 2, sizing({ cols: 150 }), { all: true });
    assert.deepEqual(
      [own.reply, own.status, own.reads >= 3, f.size()],
      ["sized", 0, true, "150x35"],
    );
    // One status branch runs, whichever comparisons were cut.
    f.run("set-option", "-t", f.target.id, "status", "2");
    const status = f.under("#{status}", 1, sizing({ cols: 160, rows: 40 }), {
      all: true,
    });
    assert.deepEqual(
      [status.reply, status.status, f.size()],
      ["sized", 0, "160x38"],
    );
    // A split window is refused even when the pane count is the cut term.
    f.run("split-window", "-d", "-t", f.window, "sleep 60");
    const split = f.under("#{window_panes}", 1, sizing({ cols: 180 }));
    assert.deepEqual(
      [split.reply, split.status, f.size()],
      ["refused", 0, "160x38"],
    );
  },
);

// The static browser suite's SSH stand-in finds the session of an attach by
// this exact comparison, so the by-name lookup has to keep printing it.
test("an attach by name still names its session the way the browser stand-in reads it", () => {
  for (const command of [
    tmuxCommand("sizer", "", true),
    tmuxCommand("sizer", "", true, undefined, "", { ignoreSize: true }),
  ])
    assert.equal(
      command
        .replace(/'\\''/g, "'")
        .match(/"\$tailterm_tmux_name" = '([^']+)'/)?.[1],
      "sizer",
    );
});

test("a hung tmux command fails fast and names the command", async () => {
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-hung-"));
  const socket = dir + "/socket",
    stub = path.join(dir, "tmux");
  // A client that never returns, as at a stuck server's first new-session.
  writeFileSync(stub, "#!/bin/sh\nexec sleep 600\n", { mode: 0o700 });
  const answering = path.join(dir, "answering");
  writeFileSync(answering, "#!/bin/sh\nprintf 'ok %s\\n' \"$1\"\nexit 3\n", {
    mode: 0o700,
  });
  // Stands in for the stuck private server: a detached process in its own
  // group whose command line carries the socket path. The test starts it, so
  // it exists before the bounded call however loaded the host is.
  const server = spawn("/usr/bin/perl", ["-e", "sleep 600", socket], {
    detached: true,
    stdio: "ignore",
  });
  server.unref();
  try {
    const started = Date.now();
    assert.throws(
      () =>
        runBounded(stub, ["new-session", "-d", "-s", "stuck"], {
          timeoutMs: 300,
          socket,
        }),
      (e) => {
        assert.match(
          e.message,
          /^tmux command hung after 300 ms: .*new-session -d -s stuck; killed private server process group\(s\) \d+/,
        );
        assert.ok(e.message.includes(String(server.pid)), e.message);
        return true;
      },
    );
    assert.ok(Date.now() - started < 5000, "the hang is reported fast");
    assert.ok(server.pid > 1);
    let gone = false;
    for (let i = 0; i < 100 && !gone; i++) {
      try {
        process.kill(server.pid, 0);
        await new Promise((r) => setTimeout(r, 20));
      } catch (e) {
        assert.equal(e.code, "ESRCH");
        gone = true;
      }
    }
    assert.ok(gone, "the stand-in server process is gone");
    assert.throws(
      () => runBounded(stub, ["kill-server"], { timeoutMs: 300 }),
      /^Error: tmux command hung after 300 ms: .*kill-server; no private socket was given$/,
    );
    const answered = runBounded(answering, ["list-sessions"], { socket });
    assert.equal(answered.status, 3);
    assert.equal(answered.stdout, "ok list-sessions\n");
    assert.equal(answered.error, undefined);
  } finally {
    try {
      process.kill(server.pid, "SIGKILL");
    } catch {}
    rmSync(dir, { recursive: true, force: true });
  }
});

test("tmux launch resolves SSH PATH and preserves real tmux failures", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-tmux-"));
  try {
    writeFileSync(
      path.join(dir, "tmux"),
      "#!/bin/sh\nprintf '%s\\n' \"$@\"\nprintf 'fixture: terminal initialization failed\\n' >&2\nexit 23\n",
      { mode: 0o700 },
    );
    const result = runBounded("/bin/sh", ["-c", tmuxCommand("work")], {
      encoding: "utf8",
      env: { ...process.env, PATH: dir },
    });
    assert.equal(result.status, 23);
    assert.match(
      result.stdout,
      /^-u\n-T\nclipboard\nnew-session\n-A\n-s\nwork\n;/,
    );
    assert.match(result.stdout, /set-clipboard external/);
    assert.match(result.stderr, /terminal initialization failed/);
    assert.match(result.stderr, /exit status 23/);
    assert.doesNotMatch(result.stderr, /Install tmux/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
test("explicit tmux executable works outside PATH, including spaces and quotes, for launch and discovery", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-tmux-"));
  try {
    const binary = path.join(dir, "custom ' tmux");
    writeFileSync(binary, "#!/bin/sh\nprintf '%s\\n' \"$@\"\n", {
      mode: 0o700,
    });
    const launch = runBounded(
      "/bin/sh",
      ["-c", tmuxCommand("work-01", binary)],
      { encoding: "utf8" },
    );
    assert.equal(launch.status, 0);
    assert.match(
      launch.stdout,
      /^-u\n-T\nclipboard\nnew-session\n-A\n-s\nwork-01\n;/,
    );
    assert.match(launch.stdout, /set-option\nmouse\non/);
    const list = runBounded("/bin/sh", ["-c", tmuxListCommand(binary)], {
      encoding: "utf8",
    });
    assert.equal(list.status, 0);
    assert.equal(
      list.stdout,
      "list-sessions\n-F\n#{session_name}|#{session_windows}|#{session_attached}|#{session_id}|#{session_created}\n",
    );
    const missing = runBounded(
      "/bin/sh",
      ["-c", tmuxCommand("main", path.join(dir, "absent"))],
      { encoding: "utf8" },
    );
    assert.equal(missing.status, 127);
    assert.match(
      missing.stderr,
      /Configured tmux executable is not executable/,
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
test("session and executable validation prevent command injection", () => {
  for (const value of ["../tmux", "tmux", "/bin/tmux\nwhoami"])
    assert.throws(() => validateTmuxPath(value));
  assert.throws(() => tmuxCommand("main'; echo injected"));
  for (const name of ["", "a b", "a;b", "a'b", "a\nb", "x".repeat(65)]) {
    assert.throws(() => tmuxRenameCommand("main", name));
    assert.throws(() => tmuxRenameCommand(name, "new-name"));
  }
});

test("new-session honours an absolute start directory", () => {
  const cmd = tmuxCommand(
    "work",
    "",
    false,
    undefined,
    "/home/testuser/projects",
  );
  assert.ok(cmd.includes("new-session -A -s "));
  assert.ok(cmd.includes("/home/testuser/projects"));
  // Resuming an existing session ignores the directory (no tmux -c flag).
  assert.ok(
    !tmuxCommand(
      "work",
      "",
      true,
      undefined,
      "/home/testuser/projects",
    ).includes("/home/testuser/projects"),
  );
  assert.throws(() => tmuxCommand("work", "", false, undefined, "relative"));
});

test("agent tiles attach with ignore-size on tmux 3.2+ only; other attaches never do", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-ignore-size-"));
  try {
    // The fake reports $FAKE_TMUX_VERSION, lists one session and prints the
    // attach arguments one per line.
    writeFileSync(
      path.join(dir, "tmux"),
      "#!/bin/sh\ncase \"$1\" in -V) printf 'tmux %s\\n' \"$FAKE_TMUX_VERSION\"; exit 0 ;; list-sessions) printf 'work|$4\\n'; exit 0 ;; esac\nprintf '%s\\n' \"$@\"\n",
      { mode: 0o700 },
    );
    const run = (cmd, version) =>
      runBounded("/bin/sh", ["-c", cmd], {
        encoding: "utf8",
        env: { ...process.env, PATH: dir, FAKE_TMUX_VERSION: version },
      });
    const agent = tmuxCommand("work", "", true, undefined, "", {
      ignoreSize: true,
    });
    const modern = run(agent, "3.7b");
    assert.equal(modern.status, 0, modern.stderr);
    assert.match(
      modern.stdout,
      /^-u\n-T\nclipboard\nattach-session\n-f\nignore-size\n-t\n\$4\n;/,
    );
    for (const old of ["3.1c", "2.9"]) {
      const legacy = run(agent, old);
      assert.equal(legacy.status, 0, legacy.stderr);
      assert.match(legacy.stdout, /attach-session\n-t\n\$4\n;/);
      assert.doesNotMatch(legacy.stdout, /ignore-size/);
    }
    // A human attach, a new session and an unflagged resume never ignore size.
    for (const cmd of [
      tmuxCommand("work", "", true),
      tmuxCommand("work", "", false, undefined, "", { ignoreSize: true }),
      tmuxCommand("work"),
    ]) {
      assert.doesNotMatch(cmd, /ignore-size/);
      const result = run(cmd, "3.7b");
      assert.equal(result.status, 0, result.stderr);
      assert.doesNotMatch(result.stdout, /ignore-size/);
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("helper policy is identity guarded, exact-window targeted and shell quoted", () => {
  const dir = mkdtempSync("/tmp/tt-helper-command-");
  const binary = dir + "/tmux ' fixture";
  const binding = {
    taskId: "tsk_0000000000000001",
    agentId: "agt_0000000000000001",
    runId: "run_0000000000000001",
    role: "owner_helper",
  };
  const options = { ignoreSize: true, helperBinding: binding };
  try {
    writeFileSync(
      binary,
      `#!/bin/sh
case "$1" in
-V) printf 'tmux %s\\n' "$FAKE_TMUX_VERSION" ;;
list-sessions) printf 'helper|$4\\n' ;;
display-message) case "$*" in *window_id*) printf '@12|1234\\n' ;; *) printf '$4|1234\\n' ;; esac ;;
if-shell) printf '%s\\n' "$@" > "${dir}/guard"; printf '%s\\n' "$FAKE_HELPER_POLICY" ;;
*) printf '%s\\n' "$@" ;;
esac
`,
      { mode: 0o700 },
    );
    const command = tmuxCommand(
      "helper",
      binary,
      true,
      { id: "$4", created: "1234" },
      "",
      options,
    );
    for (const version of ["3.7b", "3.1c"]) {
      const result = runBounded("/bin/sh", ["-c", command], {
        encoding: "utf8",
        env: {
          ...process.env,
          FAKE_TMUX_VERSION: version,
          FAKE_HELPER_POLICY: "helper-ready",
        },
      });
      assert.equal(result.status, 0, result.stderr);
      assert.match(
        result.stdout,
        /attach-session\n(?:-f\nignore-size\n)?-t\n\$4:@12\n/,
      );
      assert.equal(result.stdout.includes("ignore-size"), version === "3.7b");
      const guard = readFileSync(dir + "/guard", "utf8");
      assert.match(guard, /if-shell\n-F\n-t\n\$4:@12\n/);
      assert.match(guard, /session_created},1234/);
      for (const tag of [
        "TAILTERM_TASK",
        "TAILTERM_AGENT",
        "TAILTERM_RUN",
        "TAILTERM_ROLE",
        "window_linked",
        "window_id",
      ])
        assert.ok(guard.includes(tag), tag);
      assert.match(guard, /set-option -w -t '\$4:@12' window-size latest/);
      assert.doesNotMatch(guard, /resize-window|-g /);
    }
    const refused = runBounded("/bin/sh", ["-c", command], {
      encoding: "utf8",
      env: {
        ...process.env,
        FAKE_TMUX_VERSION: "3.7b",
        FAKE_HELPER_POLICY: "helper-refused",
      },
    });
    assert.equal(refused.status, 1);
    assert.match(refused.stderr, /Helper sizing refused/);
    assert.doesNotMatch(refused.stdout, /attach-session/);
    for (const malformed of [
      { ...binding, role: "ordinary" },
      { ...binding, runId: "" },
      { ...binding, taskId: "bad'$(false)" },
      { ...binding, agentId: "bad" },
    ])
      assert.throws(
        () =>
          tmuxCommand("helper", binary, true, undefined, "", {
            ignoreSize: true,
            helperBinding: malformed,
          }),
        /identity/,
      );
    assert.throws(
      () => tmuxCommand("helper", binary, false, undefined, "", options),
      /existing-session/,
    );
    assert.throws(
      () =>
        tmuxCommand("helper", binary, true, undefined, "", {
          ...options,
          ignoreSize: false,
        }),
      /ignore-size/,
    );
    for (const plain of [
      tmuxCommand("helper"),
      tmuxCommand("helper", "", true),
      tmuxCommand("agent", "", true, undefined, "", { ignoreSize: true }),
    ])
      assert.doesNotMatch(plain, /helper-ready|window-size latest/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("agent launch forwards an explicit model as one argument and omits defaults", async () => {
  const { agentSpawnCommand } = await import("../shared/tmux-command.js");
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-model-"));
  try {
    writeFileSync(path.join(dir, "tt"), "#!/bin/sh\nprintf '%s\\n' \"$@\"\n", {
      mode: 0o700,
    });
    const fields = {
      hub: "http://127.0.0.1:18765",
      task: "tsk_0123456789abcdef",
      name: "reviewer",
      runtime: "codex",
      run: "codex",
      prompt: "First line\nSecond line",
    };
    const launch = (extra) =>
      runBounded(
        "/bin/sh",
        ["-c", agentSpawnCommand({ ...fields, ...extra })],
        {
          encoding: "utf8",
          env: { ...process.env, PATH: dir },
        },
      );
    const selected = launch({ model: "provider/model:latest" });
    assert.equal(selected.status, 0);
    assert.match(selected.stdout, /--model\nprovider\/model:latest\n$/);
    assert.match(
      launch({ plannedTeamMembers: 4 }).stdout,
      /--planned-team-members\n4\n/,
    );
    assert.match(
      launch({ expectedLifecycleGeneration: 7 }).stdout,
      /--expected-lifecycle-generation\n7\n/,
    );
    const resume = launch({
      agentId: "agt_1111111111111111",
      expectedRunId: "run_1111111111111111",
      expectedLifecycleGeneration: 7,
      resumeReceiptId: "ppr_1111111111111111",
    });
    assert.match(resume.stdout, /--resume-receipt-id\nppr_1111111111111111\n/);
    assert.throws(() =>
      agentSpawnCommand({
        ...fields,
        resumeReceiptId: "ppr_1111111111111111",
      }),
    );
    assert.doesNotMatch(launch({}).stdout, /--model/);
    for (const model of ["--help", "$(id)", "a\nb", "two words"])
      assert.throws(() => agentSpawnCommand({ ...fields, model }));
    for (const plannedTeamMembers of [-1, 1.5, 33, "2"])
      assert.throws(() => agentSpawnCommand({ ...fields, plannedTeamMembers }));
    for (const expectedLifecycleGeneration of [-1, 1.5, "2"])
      assert.throws(() =>
        agentSpawnCommand({ ...fields, expectedLifecycleGeneration }),
      );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("agent launch forwards one bounded item context and optional replacement identity", async () => {
  const { agentSpawnCommand } = await import("../shared/tmux-command.js");
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-item-route-"));
  try {
    writeFileSync(path.join(dir, "tt"), "#!/bin/sh\nprintf '%s\\n' \"$@\"\n", {
      mode: 0o700,
    });
    const fields = {
      hub: "http://127.0.0.1:18765",
      task: "tsk_0123456789abcdef",
      name: "reviewer-23456789",
      runtime: "codex",
      run: "codex",
      workItemTaskId: "tsk_0123456789abcdef",
      workItemId: "wi_abcdef0123456789",
      workItemRevision: 4,
      workOrderTaskId: "tsk_0123456789abcdef",
      workOrderMessageSeq: 814,
      replacesAgentId: "agt_1111111111111111",
      workContextBundle: {
        version: 1,
        itemTaskId: "tsk_0123456789abcdef",
        itemId: "wi_abcdef0123456789",
        itemRevision: 4,
        workOrderMessage: { taskId: "tsk_0123456789abcdef", seq: 814 },
        history: { coverage: { complete: true } },
      },
    };
    const result = runBounded("/bin/sh", ["-c", agentSpawnCommand(fields)], {
      encoding: "utf8",
      env: { ...process.env, PATH: dir },
    });
    assert.equal(result.status, 0);
    assert.match(result.stdout, /--work-item\nwi_abcdef0123456789\n/);
    assert.match(result.stdout, /--work-item-revision\n4\n/);
    assert.match(result.stdout, /--work-order-message\n814\n/);
    assert.match(result.stdout, /--replaces-agent\nagt_1111111111111111\n/);
    assert.match(result.stdout, /--work-context-file\n/);
    assert.throws(
      () => agentSpawnCommand({ ...fields, workOrderMessageSeq: 0 }),
      /routing/,
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("browser shell transport preserves measured and 512 KiB UTF-8 contexts", async () => {
  const { agentSpawnCommand } = await import("../shared/tmux-command.js");
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-context-boundary-"));
  try {
    writeFileSync(
      path.join(dir, "tt"),
      '#!/bin/sh\nwhile [ "$#" -gt 0 ]; do if [ "$1" = --work-context-file ]; then shift; /bin/cat "$1"; printf \'%s\\n\' "$1" >&2; exit "${CONTEXT_FAIL:-0}"; fi; shift; done\nexit 1\n',
      { mode: 0o700 },
    );
    for (const size of [269315, MAX_WORK_CONTEXT_BYTES]) {
      for (const fill of ["界", "'", "<", "\\\\"]) {
        const overhead = Buffer.byteLength('{"source":""}');
        const remaining = size - overhead;
        const context =
          '{"source":"' +
          fill.repeat(Math.floor(remaining / Buffer.byteLength(fill))) +
          "x".repeat(remaining % Buffer.byteLength(fill)) +
          '"}';
        assert.equal(Buffer.byteLength(context), size);
        const fields = {
          hub: "http://127.0.0.1:18765",
          task: "tsk_0123456789abcdef",
          name: "synthetic",
          run: "codex",
          runtime: "codex",
          workItemTaskId: "tsk_0123456789abcdef",
          workItemId: "wi_abcdef0123456789",
          workItemRevision: 1,
          workOrderTaskId: "tsk_0123456789abcdef",
          workOrderMessageSeq: 1,
          workContextBundle: context,
        };
        const result = runBounded(
          "/bin/sh",
          ["-c", agentSpawnCommand(fields)],
          {
            encoding: "utf8",
            env: { ...process.env, PATH: dir },
            maxBuffer: 4 * 1024 * 1024,
          },
        );
        assert.equal(
          result.status,
          0,
          `${size}/${fill}: ${result.error || result.stderr}`,
        );
        assert.equal(result.stdout, context);
        assert.equal(
          existsSync(result.stderr.trim()),
          false,
          "private context file survived success",
        );
        const failure = runBounded(
          "/bin/sh",
          ["-c", agentSpawnCommand(fields)],
          {
            encoding: "utf8",
            env: { ...process.env, PATH: dir, CONTEXT_FAIL: "17" },
            maxBuffer: 4 * 1024 * 1024,
          },
        );
        assert.equal(failure.status, 17);
        assert.equal(
          existsSync(failure.stderr.trim()),
          false,
          "private context file survived failure",
        );
        if (size === MAX_WORK_CONTEXT_BYTES) {
          fields.workContextBundle = context + " ";
          assert.throws(() => agentSpawnCommand(fields), /512 KiB/);
        }
      }
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("staged 512 KiB context uses a bounded verified command and always cleans up", async () => {
  const { agentSpawnCommand } = await import("../shared/tmux-command.js");
  const dir = mkdtempSync(path.join(tmpdir(), "tailterm-staged-context-"));
  const agentId = `agt_${randomBytes(8).toString("hex")}`;
  const overhead = Buffer.byteLength('{"source":""}');
  const context =
    '{"source":"' +
    "界".repeat(Math.floor((MAX_WORK_CONTEXT_BYTES - overhead) / 3)) +
    "x".repeat((MAX_WORK_CONTEXT_BYTES - overhead) % 3) +
    '"}';
  const digest = createHash("sha256").update(context).digest("hex");
  const staged = `/tmp/.tailterm-work-context-${agentId}-${digest}.json`;
  try {
    writeFileSync(
      path.join(dir, "tt"),
      '#!/bin/sh\nwhile [ "$#" -gt 0 ]; do if [ "$1" = --work-context-file ]; then shift; /bin/cat "$1"; exit 0; fi; shift; done\nexit 1\n',
      { mode: 0o700 },
    );
    const fields = {
      hub: "http://127.0.0.1:18765",
      task: "tsk_0123456789abcdef",
      name: "synthetic",
      run: "codex",
      runtime: "codex",
      agentId,
      workItemTaskId: "tsk_0123456789abcdef",
      workItemId: "wi_abcdef0123456789",
      workItemRevision: 3,
      workOrderTaskId: "tsk_0123456789abcdef",
      workOrderMessageSeq: 3622,
      workContextBundle: context,
      workContextFile: staged,
      workContextDigest: digest,
    };
    writeFileSync(staged, context, { mode: 0o600 });
    const command = agentSpawnCommand(fields);
    assert.ok(Buffer.byteLength(command) <= 64 * 1024);
    assert.doesNotMatch(
      command,
      new RegExp(Buffer.from(context.slice(0, 48)).toString("base64")),
    );
    const result = runBounded("/bin/sh", ["-c", command], {
      encoding: "utf8",
      env: { ...process.env, PATH: `${dir}:/usr/bin:/bin` },
      maxBuffer: 2 * 1024 * 1024,
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout, context);
    assert.equal(existsSync(staged), false);

    writeFileSync(staged, context.slice(0, -1) + " ", { mode: 0o600 });
    const changed = runBounded("/bin/sh", ["-c", command], {
      encoding: "utf8",
      env: { ...process.env, PATH: `${dir}:/usr/bin:/bin` },
      maxBuffer: 2 * 1024 * 1024,
    });
    assert.equal(changed.status, 1);
    assert.match(changed.stderr, /digest changed/);
    assert.equal(existsSync(staged), false);
  } finally {
    rmSync(staged, { force: true });
    rmSync(dir, { recursive: true, force: true });
  }
});
