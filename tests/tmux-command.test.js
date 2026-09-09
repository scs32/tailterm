import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import {
  tmuxCommand,
  tmuxListCommand,
  validateTmuxPath,
  tmuxRenameCommand,
} from "../shared/tmux-command.js";

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
    assert.doesNotMatch(launch({}).stdout, /--model/);
    for (const model of ["--help", "$(id)", "a\nb", "two words"])
      assert.throws(() => agentSpawnCommand({ ...fields, model }));
    for (const plannedTeamMembers of [-1, 1.5, 33, "2"])
      assert.throws(() =>
        agentSpawnCommand({ ...fields, plannedTeamMembers }),
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
    assert.match(result.stdout, /--work-context-json\n/);
    assert.throws(
      () => agentSpawnCommand({ ...fields, workOrderMessageSeq: 0 }),
      /routing/,
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
