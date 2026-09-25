import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { exampleTeam } from "../client/team-examples.js";
import { teamLaunchPlan } from "../client/team-launch-plan.js";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const task = "tsk_1111111111111111",
  item = "wi_2222222222222222";
const context = {
  version: 1,
  itemTaskId: task,
  itemId: item,
  itemRevision: 1,
  workOrderMessage: { taskId: task, seq: 7 },
  history: {},
};
const routing = {
  workItemTaskId: task,
  workItemId: item,
  workItemRevision: 1,
  workOrderTaskId: task,
  workOrderMessageSeq: 7,
  workContextBundle: context,
};
const handler = {
  role: "database_handler",
  name: "existing-handler",
  status: "running",
};

test("CLI embedded planner and TailOS resolve identical complete member fields", () => {
  const ui = teamLaunchPlan({
    team: exampleTeam("planned"),
    servers: [{ id: "local", name: "fixture" }],
    mainServerId: "local",
    projectFolders: { local: root },
    itemRouting: routing,
    handler,
  });
  const source = readFileSync(
    resolve(root, "hub/internal/teamplan/plan.mjs"),
    "utf8",
  );
  const cli = spawnSync(
    process.execPath,
    ["--input-type=module", "-e", source],
    {
      input: JSON.stringify({
        hub: "http://localhost",
        task,
        item,
        revision: 1,
        order: 7,
        template: "planned",
        cwd: root,
        host: "fixture",
        handler,
        workContextBundle: context,
      }),
      encoding: "utf8",
    },
  );
  assert.equal(cli.status, 0, cli.stderr);
  const resolved = JSON.parse(cli.stdout);
  assert.deepEqual(resolved.plan, ui);
  assert.deepEqual(resolved.itemRouting, routing);
  assert.deepEqual(
    ui.map((entry) => entry.fields.reasoning),
    ["medium", "high", "medium", "high"],
  );
  assert.deepEqual(
    ui.map((entry) => entry.fields.name),
    [
      "lead-22222222",
      "planner-22222222",
      "builder-22222222",
      "reviewer-22222222",
    ],
  );
  assert.ok(ui.every((entry) => !entry.fields.name.startsWith("database-")));
});

test("shared planner requires the existing database handler", () => {
  assert.throws(
    () =>
      teamLaunchPlan({
        team: exampleTeam("planned"),
        servers: [{ id: "local" }],
        mainServerId: "local",
        itemRouting: routing,
      }),
    /Set up database handler/,
  );
});
