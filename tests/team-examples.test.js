import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { TEAM_EXAMPLES, exampleTeam } from "../client/team-examples.js";
import { normalizeTeam, teamLaunches } from "../client/teams.js";
import { MODEL_OPTIONS } from "../client/model-picker.js";
test("nine complete examples are portable, launchable, bounded and independently editable", () => {
  assert.equal(TEAM_EXAMPLES.length, 9);
  for (const e of TEAM_EXAMPLES) {
    const team = normalizeTeam(exampleTeam(e.id));
    assert.ok(team.members.length >= 1 && team.members.length <= 5);
    assert.ok(e.fit && e.goal && e.workflow);
    for (const m of team.members) {
      assert.equal(m.serverId, "");
      assert.ok(m.prompt.length > 2500);
      assert.ok(Buffer.byteLength(m.prompt) < 8192);
      assert.ok(MODEL_OPTIONS[m.runtime].some(([id]) => id === m.model));
      assert.match(m.prompt, /inventory useful long-lived services/);
    }
    const plan = teamLaunches(team, [{ id: "main" }]);
    assert.ok(plan.every((p) => p.server.id === "main"));
    const copy = exampleTeam(e.id);
    copy.members[0].prompt = "changed";
    assert.notEqual(exampleTeam(e.id).members[0].prompt, "changed");
  }
});

test("example orchestrators route implementation to builders without erasing worker roles", () => {
  const solo = exampleTeam("solo");
  assert.match(
    solo.members.find((member) => member.name === solo.orchestrator).prompt,
    /both required exception conditions.*sole non-database team member and agent spawning is disabled/s,
  );

  const pair = exampleTeam("pair");
  assert.equal(pair.orchestrator, "reviewer");
  assert.match(
    pair.members.find((member) => member.name === "reviewer").prompt,
    /main orchestrator and a read-only reviewer/,
  );
  assert.match(
    pair.members.find((member) => member.name === "builder").prompt,
    /own implementation and verification/,
  );

  const feature = exampleTeam("feature");
  assert.match(
    feature.members.find((member) => member.name === "lead").prompt,
    /You do not own or edit those files/,
  );
  assert.match(
    feature.members.find((member) => member.name === "api").prompt,
    /ownership of shared schema\/types or final integration/,
  );

  const bug = exampleTeam("bug");
  assert.match(
    bug.members.find((member) => member.name === "fixer").prompt,
    /evidence review, not the patch/,
  );
  assert.match(
    bug.members.find((member) => member.name === "analyst").prompt,
    /repair implementation routed by fixer/,
  );

  const swarm = exampleTeam("swarm");
  assert.match(
    swarm.members.find((member) => member.name === "orchestrator").prompt,
    /retain final decisions and evidence review, not implementation or integration/,
  );
  assert.deepEqual(
    swarm.members
      .filter((member) => member.name.startsWith("worker"))
      .map((member) => member.role),
    ["Swarm worker 1", "Swarm worker 2", "Swarm worker 3", "Swarm worker 4"],
  );
});

test("documented team prompts exactly include every generated template", () => {
  const documentation = readFileSync(
    new URL("../docs/team-examples.md", import.meta.url),
    "utf8",
  );
  for (const team of TEAM_EXAMPLES)
    for (const member of team.members)
      assert.ok(
        documentation.includes(member.prompt),
        `missing exact ${team.id}/${member.name} prompt`,
      );
});
