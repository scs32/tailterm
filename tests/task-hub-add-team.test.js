import test from "node:test";
import assert from "node:assert/strict";
import { addTeamOrchestrator } from "../client/task-hub.js";
import { exampleTeam } from "../client/team-examples.js";
import { teamLaunchPlan } from "../client/team-launch-plan.js";

const plan = [{ fields: { name: "team-lead-41b1c632" } }];

test("Add team assigns its prepared item-scoped lead when the project lead is missing", () => {
  for (const orchestrator of ["", "lead", "closed-lead"]) {
    const agents = [
      { name: "lead", status: "closed" },
      { name: "closed-lead", status: "closed" },
      { name: "unrelated", status: "running" },
    ];
    assert.equal(
      addTeamOrchestrator({ orchestrator }, agents, plan),
      "team-lead-41b1c632",
      orchestrator,
    );
  }
});

test("Add team retains an existing non-closed project lead", () => {
  for (const status of [
    "running",
    "done",
    "needs_input",
    "retired",
    "exited",
  ]) {
    assert.equal(
      addTeamOrchestrator(
        { orchestrator: "existing-lead" },
        [{ name: "existing-lead", status }],
        plan,
      ),
      null,
      status,
    );
  }
});

test("Add team refuses a prepared team without a lead", () => {
  assert.throws(
    () => addTeamOrchestrator({ orchestrator: "lead" }, [], []),
    /prepared team has no lead/,
  );
});

test("Add team can pass the shared Planned delivery plan to its lead selector", () => {
  const item = "wi_3333333333333333";
  const resolved = teamLaunchPlan({
    team: exampleTeam("planned"),
    servers: [{ id: "local", name: "fixture" }],
    mainServerId: "local",
    projectFolders: { local: "/tmp/fixture" },
    itemRouting: {
      workItemTaskId: "tsk_1111111111111111",
      workItemId: item,
      workItemRevision: 1,
      workOrderTaskId: "tsk_1111111111111111",
      workOrderMessageSeq: 7,
      workContextBundle: { version: 1 },
    },
    handler: { role: "database_handler", status: "running" },
  });
  assert.equal(
    addTeamOrchestrator(
      { orchestrator: "prior" },
      [{ name: "prior", status: "running" }],
      resolved,
    ),
    null,
  );
  assert.equal(
    addTeamOrchestrator(
      { orchestrator: "prior" },
      [{ name: "prior", status: "closed" }],
      resolved,
    ),
    "lead-33333333",
  );
});
