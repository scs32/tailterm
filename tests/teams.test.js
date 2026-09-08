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
