import test from "node:test";
import assert from "node:assert/strict";
import { normalizeTeam, savedTeams, teamLaunches } from "../client/teams.js";
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
