import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { TEAM_EXAMPLES, exampleTeam } from "../client/team-examples.js";
import { normalizeTeam, teamLaunches } from "../client/teams.js";
import { MODEL_OPTIONS } from "../client/model-picker.js";
test("ten complete examples are portable, launchable, bounded and independently editable", () => {
  assert.equal(TEAM_EXAMPLES.length, 10);
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

test("lead and worker prompts teach withdrawal of superseded requests", () => {
  const team = exampleTeam("planned");
  for (const name of ["lead", "builder"]) {
    const prompt = team.members.find((member) => member.name === name).prompt;
    assert.match(prompt, /supersede your own open request.*tt withdraw SEQ --reason TEXT/);
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

test("Planned delivery members post typed messages with tt send", () => {
  for (const member of exampleTeam("planned").members) {
    assert.match(member.prompt, /Post with tt send/);
    assert.match(member.prompt, /has no send command, post the same fields as text/);
    assert.ok(Buffer.byteLength(member.prompt) < 8192);
  }
});

test("Planned delivery lead has exact terminal team closeout", () => {
  const lead = exampleTeam("planned").members.find((member) => member.name === "lead").prompt;
  assert.match(lead, /handler confirms a terminal item and all team obligations are closed, run tt close --team/);
  assert.match(lead, /item workers before the lead, clears the project orchestrator and preserves the database handler/);
  assert.match(lead, /owner may use tt close --team --task ID/);
});

test("typed team prompts use notices for waits and self-blocks for dependencies", () => {
  let typedPrompts = 0;
  for (const team of TEAM_EXAMPLES)
    for (const member of exampleTeam(team.id).members) {
      if (!member.prompt.includes("BOARD MESSAGE FORMAT")) continue;
      typedPrompts++;
      assert.match(member.prompt, /Use NOTICE to tell someone to wait or share status/, `${team.id}/${member.name}`);
      assert.match(member.prompt, /Use BLOCK only when you yourself are blocked; address it to whoever can unblock you, state what you need, and give the condition for resuming/, `${team.id}/${member.name}`);
    }
  assert.equal(typedPrompts, 5);
});

test("team prompts leave overdue escalation to the broker after one teammate nudge", () => {
  for (const team of TEAM_EXAMPLES)
    for (const member of exampleTeam(team.id).members)
      assert.doesNotMatch(member.prompt, /(?:send|post|escalate).*to the owner with a BLOCK/i, `${team.id}/${member.name}`);

  const lead = exampleTeam("planned").members.find((member) => member.name === "lead").prompt;
  assert.match(lead, /without a reply for 30 minutes, send that teammate one nudge/);
  assert.match(lead, /broker escalates overdue work itself/);
  assert.match(lead, /do not send a message to escalate a teammate's stall to the owner/);
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

// Broker phase 3.1 round one (B1): every member is taught tt ack first, and
// no prompt tells an agent not to acknowledge.
test("every member prompt teaches tt ack and none says not to acknowledge", () => {
  for (const team of TEAM_EXAMPLES)
    for (const member of team.members) {
      assert.match(member.prompt, /run tt ack SEQ before you start/, `${team.id}/${member.name}`);
      assert.doesNotMatch(member.prompt, /do not acknowledge|acknowledge acknowledgements/i, `${team.id}/${member.name}`);
    }
});
