import test from "node:test";
import assert from "node:assert/strict";
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
    }
    const plan = teamLaunches(team, [{ id: "main" }]);
    assert.ok(plan.every((p) => p.server.id === "main"));
    const copy = exampleTeam(e.id);
    copy.members[0].prompt = "changed";
    assert.notEqual(exampleTeam(e.id).members[0].prompt, "changed");
  }
});
