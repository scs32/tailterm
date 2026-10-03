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
const uiPlan = (context, template = "planned") =>
  teamLaunchPlan({
    team: exampleTeam(template),
    servers: [{ id: "local", name: "fixture" }],
    mainServerId: "local",
    projectFolders: { local: root },
    itemRouting: routingFor(context),
    handler,
  });
const cliPlan = (context, template = "planned") =>
  spawnSync(process.execPath, ["--input-type=module", "-e", source], {
    input: JSON.stringify({
      hub: "http://localhost",
      task,
      item,
      revision: 1,
      order: 7,
      template,
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
test("a feature launch adds a GPT-6 Astra plan reviewer beside the Sol planner", () => {
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
    ["codex", "gpt-6.1-sol"],
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

// wi_f8d48780626165cc: the queue-only small-change lane launches its lead,
// builder and reviewer, lead first; the lead is also the distinct verifier.
test("a small-change launch has three members, lead first, in TailOS and the CLI", () => {
  const bug = contextFor("bug");
  const ui = uiPlan(bug, "small");
  const cli = cliPlan(bug, "small");
  assert.equal(cli.status, 0, cli.stderr);
  assert.deepEqual(JSON.parse(cli.stdout).plan, ui);
  assert.deepEqual(
    ui.map((entry) => entry.fields.name),
    ["lead-22222222", "builder-22222222", "reviewer-22222222"],
  );
  assert.ok(
    ui.every(
      (entry) =>
        !/^(planner|plan-reviewer|verifier|database)-/.test(entry.fields.name),
    ),
  );
  const lead = ui[0].fields.prompt;
  assert.ok(lead.startsWith("Role: Small-change lead and verifier\n\n"));
  assert.ok(lead.length <= 8192, `small lead prompt is ${lead.length} characters`);
  for (const sentence of [
    "run tt ack SEQ before you start",
    "run tt withdraw SEQ --reason TEXT",
    "Never ask through an interactive terminal prompt: ask the owner with tt ask, a teammate with tt send --kind question.",
    "inventory useful long-lived services",
    "node scripts/verify-matrix.mjs run PLAN_JSON EXTERNAL_LOG_DIRECTORY",
    "(1) freeze the verification plan naming you as verifier, (2) import your matrix receipt, (3) save done and accept the queue entry",
    "--work-item ID --work-item-revision N --work-order-message SEQ",
    "never widen: ask the owner with tt ask to requeue the item as Planned delivery",
    "run tt close --team",
    "Send no Start, plan or assignment gate REQUESTs",
  ])
    assert.ok(lead.includes(sentence), `small lead lacks ${sentence}`);
  assert.doesNotMatch(lead, /For each live Start, plan or assignment gate/);
  assert.doesNotMatch(lead, /Ask planner for a plan/);
  // A feature is refused at queue admission; the planner itself only needs
  // the kind, so it still resolves the same three seats.
  assert.equal(uiPlan(context, "small").length, 3);
});
