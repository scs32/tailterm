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
import { spawnSync, spawn } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import {
  tmuxCommand,
  tmuxListCommand,
  validateTmuxPath,
  tmuxRenameCommand,
  agentWindowSizeCommand,
  shellQuote,
} from "../shared/tmux-command.js";
import { MAX_WORK_CONTEXT_BYTES } from "../shared/work-context.js";
import { readTmuxFormat } from "./agent-window-size-fixture.js";

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
  const run = (...args) => {
    const r = spawnSync(binary, args, { env, encoding: "utf8" });
    assert.equal(r.status, 0, r.stderr);
    return r.stdout.trim();
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
    const invoke = (changes = {}) =>
      spawnSync(
        "/bin/sh",
        [
          "-c",
          agentWindowSizeCommand(
            {
              ...params,
              expectedRevision: readTmuxFormat(run, [
                "display-message",
                "-p",
                "-t",
                id + ":agent",
                "#{@tailterm_size_revision}",
              ]),
              ...changes,
            },
            binary,
          ),
        ],
        { env, encoding: "utf8" },
      );
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
            const p = spawn(
              "/bin/sh",
              [
                "-c",
                agentWindowSizeCommand(
                  { ...params, expectedRevision: revision, ...c },
                  binary,
                ),
              ],
              { env },
            );
            let output = "",
              stderr = "";
            p.stdout.on("data", (d) => (output += d));
            p.stderr.on("data", (d) => (stderr += d));
            p.on("error", reject);
            p.on("close", (code) => {
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
    spawnSync(binary, ["kill-server"], { env });
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
    const result = spawnSync("/bin/sh", ["-c", tmuxCommand("work")], {
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
    const launch = spawnSync(
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
    const list = spawnSync("/bin/sh", ["-c", tmuxListCommand(binary)], {
      encoding: "utf8",
    });
    assert.equal(list.status, 0);
    assert.equal(
      list.stdout,
      "list-sessions\n-F\n#{session_name}|#{session_windows}|#{session_attached}|#{session_id}|#{session_created}\n",
    );
    const missing = spawnSync(
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
      spawnSync("/bin/sh", ["-c", cmd], {
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
      const result = spawnSync("/bin/sh", ["-c", command], {
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
    const refused = spawnSync("/bin/sh", ["-c", command], {
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
      spawnSync("/bin/sh", ["-c", agentSpawnCommand({ ...fields, ...extra })], {
        encoding: "utf8",
        env: { ...process.env, PATH: dir },
      });
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
    const result = spawnSync("/bin/sh", ["-c", agentSpawnCommand(fields)], {
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
        const result = spawnSync("/bin/sh", ["-c", agentSpawnCommand(fields)], {
          encoding: "utf8",
          env: { ...process.env, PATH: dir },
          maxBuffer: 4 * 1024 * 1024,
        });
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
        const failure = spawnSync(
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
    const result = spawnSync("/bin/sh", ["-c", command], {
      encoding: "utf8",
      env: { ...process.env, PATH: `${dir}:/usr/bin:/bin` },
      maxBuffer: 2 * 1024 * 1024,
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout, context);
    assert.equal(existsSync(staged), false);

    writeFileSync(staged, context.slice(0, -1) + " ", { mode: 0o600 });
    const changed = spawnSync("/bin/sh", ["-c", command], {
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
