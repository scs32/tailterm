import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, rmSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import {
  tmuxCommand,
  tmuxListCommand,
  validateTmuxPath,
  tmuxRenameCommand,
} from "../shared/tmux-command.js";
import { MAX_WORK_CONTEXT_BYTES } from "../shared/work-context.js";

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
