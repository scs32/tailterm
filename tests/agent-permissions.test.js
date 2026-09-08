import test from "node:test";
import assert from "node:assert/strict";
import { normalizeTeam } from "../client/teams.js";
import {
  agentSpawnCommand,
  agentToolsCommand,
} from "../shared/tmux-command.js";
const m = {
  name: "worker",
  runtime: "codex",
  run: "codex",
  cwd: "/work",
  permissionMode: "workspace-auto",
};
test("permission modes are portable team fields and explicit launch arguments", () => {
  const t = normalizeTeam({ name: "Team", members: [m] });
  assert.equal(t.members[0].permissionMode, "workspace-auto");
  const cmd = agentSpawnCommand({
    hub: "http://hub",
    task: "tsk_0000000000000001",
    ...t.members[0],
  });
  assert.match(cmd, /--permission-mode/);
  assert.match(cmd, /workspace-auto/);
  assert.throws(
    () => normalizeTeam({ name: "Team", members: [{ ...m, cwd: "" }] }),
    /absolute working directory/,
  );
  assert.throws(
    () =>
      normalizeTeam({ name: "Team", members: [{ ...m, runtime: "gemini" }] }),
    /permission mode/,
  );
  const claude = normalizeTeam({
    name: "Claude",
    members: [
      {
        ...m,
        runtime: "claude",
        run: "claude",
        permissionMode: "dontAsk",
        allowedTools: "Read\nBash(tt *)",
      },
    ],
  });
  assert.deepEqual(claude.members[0].allowedTools, ["Read", "Bash(tt *)"]);
  assert.match(agentToolsCommand("codex", "/work"), /--runtime/);
  assert.throws(() => agentToolsCommand("unknown"), /supported agent/);
});
