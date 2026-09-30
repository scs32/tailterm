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
// The prepared context carries the exact item revision, including its kind.
const contextFor = (kind) => ({
  version: 1,
  itemTaskId: task,
  itemId: item,
  itemRevision: 1,
  workOrderMessage: { taskId: task, seq: 7 },
  history: kind === undefined ? {} : { revision: { kind } },
});
const routingFor = (context) => ({
  workItemTaskId: task,
  workItemId: item,
  workItemRevision: 1,
  workOrderTaskId: task,
  workOrderMessageSeq: 7,
  workContextBundle: context,
});
const context = contextFor("feature"),
  routing = routingFor(context);
const handler = {
  role: "database_handler",
  name: "existing-handler",
  status: "running",
};

const source = readFileSync(
  resolve(root, "hub/internal/teamplan/plan.mjs"),
  "utf8",
);
const uiPlan = (context) =>
  teamLaunchPlan({
    team: exampleTeam("planned"),
    servers: [{ id: "local", name: "fixture" }],
    mainServerId: "local",
    projectFolders: { local: root },
    itemRouting: routingFor(context),
    handler,
  });
const cliPlan = (context) =>
  spawnSync(process.execPath, ["--input-type=module", "-e", source], {
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
  });

test("CLI embedded planner and TailOS resolve identical complete member fields", () => {
  const ui = uiPlan(context);
  const cli = cliPlan(context);
  assert.equal(cli.status, 0, cli.stderr);
  const resolved = JSON.parse(cli.stdout);
  assert.deepEqual(resolved.plan, ui);
  assert.deepEqual(resolved.itemRouting, routing);
  assert.deepEqual(
    ui.map((entry) => entry.fields.reasoning),
    ["medium", "high", "high", "high", "high", "high"],
  );
  assert.deepEqual(
    ui.map((entry) => entry.fields.name),
    [
      "lead-22222222",
      "planner-22222222",
      "plan-reviewer-22222222",
      "builder-22222222",
      "verifier-22222222",
      "reviewer-22222222",
    ],
  );
  assert.ok(ui.every((entry) => !entry.fields.name.startsWith("database-")));
});

// wi_ade4aa60c5d9b55e: a feature adds a plan reviewer on another model; the
// planner stays on Claude Opus.
test("a feature launch adds a GPT-6 Astra plan reviewer beside the Opus planner", () => {
  const fields = Object.fromEntries(
    uiPlan(context).map(({ fields }) => [fields.name, fields]),
  );
  const reviewer = fields["plan-reviewer-22222222"];
  assert.deepEqual(
    [reviewer.runtime, reviewer.run, reviewer.model, reviewer.reasoning, reviewer.role],
    ["codex", "codex", "gpt-6-astra", "high", "Plan review"],
  );
  const planner = fields["planner-22222222"];
  assert.deepEqual(
    [planner.runtime, planner.model],
    ["claude", "claude-opus-5-5"],
  );
});

test("a bug launch has five members and no plan reviewer in TailOS and the CLI", () => {
  const bug = contextFor("bug");
  const ui = uiPlan(bug);
  const cli = cliPlan(bug);
  assert.equal(cli.status, 0, cli.stderr);
  assert.deepEqual(JSON.parse(cli.stdout).plan, ui);
  assert.deepEqual(
    ui.map((entry) => entry.fields.name),
    [
      "lead-22222222",
      "planner-22222222",
      "builder-22222222",
      "verifier-22222222",
      "reviewer-22222222",
    ],
  );
  assert.ok(ui.every((entry) => entry.fields.role !== "Plan review"));
});

test("a launch context without the item kind is refused", () => {
  assert.throws(() => uiPlan(contextFor(undefined)), /kind is missing/);
  assert.throws(() => uiPlan(contextFor("task")), /kind is missing/);
  const cli = cliPlan(contextFor(undefined));
  assert.notEqual(cli.status, 0);
  assert.match(cli.stderr, /kind is missing/);
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
