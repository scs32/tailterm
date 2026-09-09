import test from "node:test";
import assert from "node:assert/strict";
import {
  itemScopedAgentName,
  normalizeTeam,
  savedTeams,
  teamLaunches,
} from "../client/teams.js";
const member = {
  name: "planner",
  serverId: "host",
  runtime: "codex",
  model: "model-v2",
  role: "Plan",
  prompt: "Inspect first",
};
test("teams migrate old setups once and preserve explicit empty teams", () => {
  const legacy = {
    name: "review",
    serverId: "host",
    runtime: "codex",
    run: "codex",
    cwd: "/repo",
    model: "model-v2",
  };
  const teams = savedTeams({ launchProfiles: [legacy] });
  assert.equal(teams[0].members[0].model, "model-v2");
  assert.equal(teams[0].members[0].cwd, "/repo");
  assert.deepEqual(savedTeams({ teams: [], launchProfiles: [legacy] }), []);
  assert.deepEqual(savedTeams({ teams, launchProfiles: [legacy] }), teams);
});
test("team validation rejects duplicate names, unsupported models and oversized combined instructions", () => {
  const make = (members) => normalizeTeam({ name: "Review", members });
  assert.throws(() => make([member, { ...member, name: "Planner" }]), /unique/);
  assert.throws(
    () => make([{ ...member, runtime: "generic", run: "" }]),
    /command/,
  );
  assert.throws(
    () => make([{ ...member, runtime: "generic", run: "custom" }]),
    /model/i,
  );
  assert.throws(
    () => make([{ ...member, prompt: "a".repeat(8192) }]),
    /prompt/,
  );
  const team = make([member]);
  assert.throws(() => teamLaunches(team, []), /available server/);
  const [launch] = teamLaunches(team, [{ id: "host" }]);
  assert.equal(launch.fields.prompt, "Role: Plan\n\nInspect first");
  assert.equal(launch.fields.model, "model-v2");
});

test("unassigned members inherit the chosen main machine, explicit assignments remain fixed", () => {
  const servers = [{ id: "a" }, { id: "b" }];
  const team = normalizeTeam({
    name: "Portable",
    members: [
      { ...member, serverId: "" },
      { ...member, name: "reviewer", serverId: "a" },
    ],
  });
  assert.deepEqual(
    teamLaunches(team, servers, "b").map((x) => x.server.id),
    ["b", "a"],
  );
  assert.throws(() => teamLaunches(team, servers, "missing"), /main machine/);
  assert.equal(team.members[0].serverId, "");
});

test("orchestrator launches first and teams support an orchestrator plus ten workers", () => {
  const members = Array.from({ length: 11 }, (_, i) => ({
    ...member,
    name: "agent" + i,
  }));
  const team = normalizeTeam({
    name: "Swarm",
    swarm: true,
    orchestrator: "agent5",
    members,
  });
  assert.equal(team.swarm, true);
  assert.equal(teamLaunches(team, [{ id: "host" }])[0].fields.name, "agent5");
  assert.throws(
    () => normalizeTeam({ ...team, orchestrator: "missing" }),
    /orchestrator/,
  );
  assert.equal(
    normalizeTeam({ name: "Legacy", members: [member] }).orchestrator,
    "planner",
  );
});

test("project folders are scoped to each launch machine and preserve explicit member overrides", () => {
  const machines = [
    { id: "main", name: "Main" },
    { id: "remote", name: "Remote" },
  ];
  const team = {
    name: "Portable",
    members: [
      {
        name: "lead",
        runtime: "codex",
        run: "codex",
        permissionMode: "workspace-auto",
      },
      { name: "worker", runtime: "codex", run: "codex", serverId: "remote" },
      {
        name: "reviewer",
        runtime: "codex",
        run: "codex",
        cwd: "/fixed/review",
      },
    ],
  };
  assert.throws(
    () => teamLaunches(team, machines, "main", {}),
    /Choose a project folder on Main/,
  );
  assert.throws(
    () => teamLaunches(team, machines, "main", { main: "/local/project" }),
    /Choose a project folder on Remote/,
  );
  const plan = teamLaunches(team, machines, "main", {
    main: "/local/project with spaces",
    remote: "/remote/project",
  });
  assert.deepEqual(
    plan.map((p) => p.fields.cwd),
    ["/local/project with spaces", "/remote/project", "/fixed/review"],
  );
  assert.equal(normalizeTeam(team).members[0].cwd, "");
});

test("reusable team launches receive deterministic one-item session identities", () => {
  const team = normalizeTeam({
    name: "Reusable",
    members: [member, { ...member, name: "reviewer", role: "Review" }],
  });
  const routing = {
    workItemTaskId: "tsk_0123456789abcdef",
    workItemId: "wi_abcdef0123456789",
    workItemRevision: 4,
    workOrderTaskId: "tsk_0123456789abcdef",
    workOrderMessageSeq: 814,
    workContextBundle: {
      version: 1,
      itemTaskId: "tsk_0123456789abcdef",
      itemId: "wi_abcdef0123456789",
      itemRevision: 4,
      workOrderMessage: { taskId: "tsk_0123456789abcdef", seq: 814 },
      history: { coverage: { complete: true } },
    },
  };
  const first = teamLaunches(
    team,
    [{ id: "host" }],
    "host",
    undefined,
    routing,
  );
  const retry = teamLaunches(
    team,
    [{ id: "host" }],
    "host",
    undefined,
    routing,
  );
  assert.deepEqual(
    first,
    retry,
    "partial retry must keep exact scoped names and routing",
  );
  assert.deepEqual(
    first.map((launch) => launch.fields.name),
    ["planner-23456789", "reviewer-23456789"],
  );
  assert.deepEqual(first[0].fields.workItemId, routing.workItemId);
  assert.equal(
    itemScopedAgentName("a".repeat(64), routing.workItemId).length,
    64,
  );
  assert.throws(
    () =>
      teamLaunches(team, [{ id: "host" }], "host", undefined, {
        ...routing,
        workOrderMessageSeq: 0,
      }),
    /work-order/,
  );
});
