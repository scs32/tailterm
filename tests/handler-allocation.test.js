import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync, mkdtempSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import {
  withDatabaseHandler,
  normalizeHandlerPlan,
} from "../client/project-handler.js";
import { agentSpawnCommand } from "../shared/tmux-command.js";

// These scenarios specify the instructions required for each decision. They do
// not execute a model or pretend to implement an allocation scheduler in tests.
const scenarios = JSON.parse(
  readFileSync(new URL("./handler-allocation-cases.json", import.meta.url)),
);
const source = [
  {
    server: { id: "fixture" },
    fields: {
      name: "lead",
      runtime: "codex",
      run: "codex",
      cwd: "/synthetic/repo",
      prompt: "Synthetic lead assignment",
      allowedTools: [],
    },
  },
];
const fields = withDatabaseHandler(source)[1].fields;

for (const scenario of scenarios.filter(
  (scenario) => scenario.handler.length,
)) {
  test(`browser handler instruction contract: ${scenario.name}`, () => {
    for (const clause of scenario.handler) {
      assert.ok(
        fields.prompt.includes(clause),
        `${scenario.situation}: missing ${clause}`,
      );
    }
  });
}

test("browser handler prompt survives real shell argument delivery and frozen recovery", () => {
  const dir = mkdtempSync(path.join(tmpdir(), "tt-allocation-contract-"));
  try {
    // The resolver finds only this fake tt; it prints arguments and cannot call
    // a hub, launch a worker, inspect a profile or read live work-item data.
    writeFileSync(path.join(dir, "tt"), '#!/bin/sh\nprintf "%s\\0" "$@"\n', {
      mode: 0o700,
    });
    const saved = normalizeHandlerPlan({
      hub: "http://fixture.invalid",
      taskId: "tsk_0000000000000001",
      serverId: "fixture",
      fields: { ...fields, agentId: "agt_0000000000000002" },
    });
    const command = agentSpawnCommand({
      ...saved.fields,
      hub: saved.hub,
      task: saved.taskId,
    });
    const output = spawnSync("/bin/sh", ["-c", command], {
      encoding: "utf8",
      env: { PATH: dir },
    });
    assert.equal(output.status, 0, output.stderr);
    const args = output.stdout.split("\0");
    assert.equal(args[0], "spawn");
    assert.equal(args[args.indexOf("--role") + 1], "database_handler");
    assert.equal(args[args.indexOf("--agent-id") + 1], saved.fields.agentId);
    assert.equal(args[args.indexOf("--prompt") + 1], fields.prompt);
    assert.ok(fields.prompt.length <= 8192);
    assert.equal(fields.prompt.includes(source[0].fields.prompt), false);
    const oldPlan = structuredClone(saved);
    oldPlan.fields.prompt = "Previously saved synthetic handler instructions";
    assert.equal(
      normalizeHandlerPlan(oldPlan).fields.prompt,
      oldPlan.fields.prompt,
    );
    assert.equal(normalizeHandlerPlan(saved).fields.prompt, fields.prompt);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
