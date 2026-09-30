import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { PROJECT_ROLE_TEMPLATES, QUEUE_TEAM_TEMPLATES, TEAM_EXAMPLES, exampleTeam } from "../client/team-examples.js";
import { normalizeTeam, teamLaunches } from "../client/teams.js";
import { MODEL_OPTIONS } from "../client/model-picker.js";
test("ten complete examples are portable, launchable, bounded and independently editable", () => {
  assert.equal(TEAM_EXAMPLES.length, 10);
  for (const e of TEAM_EXAMPLES) {
    const team = normalizeTeam(exampleTeam(e.id));
    assert.ok(team.members.length >= 1 && team.members.length <= 7);
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

test("fresh planned delivery uses a Claude lead without reviewer polling", () => {
  const team = exampleTeam("planned");
  const lead = team.members.find((member) => member.name === "lead");
  const reviewer = team.members.find((member) => member.name === "reviewer");
  assert.equal(lead.runtime, "claude");
  assert.equal(lead.model, "claude-opus-5-5");
  assert.doesNotMatch(reviewer.prompt, /inbox --unread --mark-read --wait|relay cannot wake/i);
  assert.match(reviewer.prompt, /relay can wake your idle Claude session/);
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

test("Planned delivery teaches live gate priority without changing inbox order", () => {
  const team = exampleTeam("planned");
  const lead = team.members.find((member) => member.name === "lead").prompt;
  const handler = team.members.find((member) => member.name === "database").prompt;
  assert.match(lead, /typed REQUEST.*RESULT --reply-to/s);
  for (const role of ["lead", "planner", "plan-reviewer", "builder", "reviewer"]) {
    const prompt = team.members.find((member) => member.name === role).prompt;
    assert.match(prompt, /--work-item ID --work-item-revision N --work-order-message SEQ/, role);
    assert.match(prompt, /--ref alone does not (create )?(the native item )?link/, role);
  }
  assert.match(handler, /Before each new queued-item record, run tt obligations/);
  assert.match(handler, /recheck before the next queued record/i);
  assert.match(handler, /Inbox sequence is unchanged/);
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
  assert.equal(typedPrompts, 7);
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

test("documented project role prompts exactly include every generated template", () => {
  const documentation = readFileSync(
    new URL("../docs/team-examples.md", import.meta.url),
    "utf8",
  );
  for (const [role, template] of Object.entries(PROJECT_ROLE_TEMPLATES)) {
    assert.equal(template.role, role);
    assert.ok(documentation.includes(template.prompt), `missing exact ${role} prompt`);
    assert.match(template.prompt, /run tt ack SEQ before you start/, role);
  }
});

// wi_2c46d9d964b335da: nobody reads an agent's terminal, so every template
// routes questions to the Board and still fits the 8192-character prompt cap.
test("every template asks through the Board, never an interactive terminal prompt", () => {
  const sentence =
    "Never ask through an interactive terminal prompt: ask the owner with tt ask, a teammate with tt send --kind question.";
  const prompts = [
    ...TEAM_EXAMPLES.flatMap((team) => team.members.map((m) => [`${team.id}/${m.name}`, m.prompt])),
    ...Object.entries(PROJECT_ROLE_TEMPLATES).map(([role, t]) => [role, t.prompt]),
  ];
  for (const [name, prompt] of prompts) {
    assert.ok(prompt.includes(sentence), `${name} lacks the Board question rule`);
    assert.ok(prompt.length <= 8192, `${name} prompt is ${prompt.length} characters`);
  }
  const bundle = readFileSync(new URL("../hub/internal/teamplan/plan.mjs", import.meta.url), "utf8");
  assert.ok(bundle.split(sentence).length - 1 >= 2, "generated plan.mjs lacks the Board question rule");
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

test("Planned delivery freezes criteria and never resets the review cap", () => {
  const planned = TEAM_EXAMPLES.find((example) => example.id === "planned");
  const instructions = JSON.stringify(planned);
  assert.match(instructions, /a1…aN unchanged/);
  assert.match(instructions, /Never reset its lifetime count/);
  assert.match(instructions, /mode general/);
  assert.match(instructions, /New non-regression findings become linked follow-ups/);
  assert.match(instructions, /held for triage/);
  assert.match(instructions, /tt send --review-file/);
});

test("Planned delivery marks verification-owned criteria pending for the reviewer", () => {
  const members = exampleTeam("planned").members;
  const lead = members.find((member) => member.name === "lead").prompt;
  const reviewer = members.find((member) => member.name === "reviewer").prompt;
  assert.match(lead, /--verification-criterion aN on ASSIGN and REVIEW/);
  assert.match(lead, /send builder one ASSIGN/);
  assert.match(lead, /do not narrate record bookkeeping on the board yourself/);
  assert.match(reviewer, /Mark verification-owned criteria pending-verification/);
  assert.match(reviewer, /handler-imported eligible receipt judges those criteria/);
});

// Owner trial wi_519d2df4f04c2e1c: only the Planned database handler moves to
// Claude Sonnet 5.5; every other seat keeps its runtime, model and effort.
// wi_ade4aa60c5d9b55e adds the feature plan reviewer on GPT-6 Astra (high).
test("Planned delivery runs the database handler on Sonnet 5.5 and the plan reviewer on Astra", () => {
  const planned = TEAM_EXAMPLES.find((example) => example.id === "planned");
  const seats = Object.fromEntries(
    planned.members.map((m) => [m.name, [m.runtime, m.model, m.reasoning]]),
  );
  assert.deepEqual(seats, {
    lead: ["claude", "claude-opus-5-5", "medium"],
    planner: ["claude", "claude-opus-5-5", "high"],
    "plan-reviewer": ["codex", "gpt-6-astra", "high"],
    builder: ["claude", "claude-opus-5-5", "high"],
    database: ["claude", "claude-sonnet-5-5", "high"],
    verifier: ["claude", "claude-opus-5-5", "high"],
    reviewer: ["claude", "claude-opus-5-5", "high"],
  });
  assert.doesNotMatch(JSON.stringify(planned), /different model family/i);
  const reviewer = planned.members.find((m) => m.name === "reviewer");
  assert.match(reviewer.prompt, /separate session with its own context that did not write the change/);
  assert.match(planned.summary, /independent reviewer in its own session/);
});

// wi_82ed4c6924930bad / order #13844: verification runs alongside review,
// fixes get targeted runs, and the final rebased candidate gets the full plan.
test("Planned delivery verifies alongside review and verifies exactly what merges", () => {
  const planned = TEAM_EXAMPLES.find((example) => example.id === "planned");
  const prompt = (name) =>
    exampleTeam("planned").members.find((member) => member.name === name).prompt;
  const lead = prompt("lead"),
    verifier = prompt("verifier"),
    handler = prompt("database"),
    builder = prompt("builder");
  assert.match(lead, /send reviewer a REVIEW naming that commit, the scope and the criteria, and start its verification at once/);
  assert.match(lead, /Verify alongside review: have the handler freeze the plan on the current tasks-hub tip and REQUEST the distinct verifier/);
  assert.match(lead, /Each later candidate gets a fresh run, targeted from the previous candidate for fixes; withdraw the superseded REQUEST/);
  assert.match(lead, /final candidate, rebased onto the current tip, gets the full plan once/);
  assert.match(lead, /handler-saved passing receipt for the exact final SHA/);
  assert.doesNotMatch(lead, /Before acceptance, REQUEST the distinct verifier/);
  assert.match(verifier, /verify-matrix\.mjs targeted CONTEXT_JSON/);
  assert.match(verifier, /never imported and never gates acceptance/);
  assert.match(verifier, /stop its run with SIGINT/);
  assert.match(verifier, /one matrix run per host at a time/);
  assert.match(verifier, /pick checks yourself; the runner selects them/);
  assert.match(handler, /Freeze each plan's base at the current local tasks-hub tip/);
  assert.match(handler, /Never import a targeted-receipt\.json/);
  assert.match(handler, /owner matrix approval covers every candidate, for any item, while verification\/matrix\.json bytes are unchanged/);
  assert.match(builder, /rebase the candidate onto the current local tasks-hub/);
  assert.match(planned.workflow, /reviewer and distinct verifier start together on each frozen candidate/);
  for (const member of planned.members)
    assert.ok(Buffer.byteLength(member.prompt) < 8192, member.name);
});

test("Planned delivery lead narrows the queue entry once the plan freezes", () => {
  const lead = exampleTeam("planned").members.find((m) => m.name === "lead");
  assert.match(lead.prompt, /Once the plan freezes, narrow the queue entry to its owned files: tt team queue scope --entry ENTRY --owns PATH \(repeat\); widening may wait\./);
  assert.doesNotMatch(lead.prompt, /Scope a queued item with/);
});

// wi_ade4aa60c5d9b55e / order #14014: features get a plan review before the
// builder; bugs go from the plan straight to the builder.
test("Planned delivery reviews a feature plan once before the builder and skips it for a bug", () => {
  const planned = TEAM_EXAMPLES.find((example) => example.id === "planned");
  const members = exampleTeam("planned").members;
  const prompt = (name) => members.find((member) => member.name === name).prompt;
  const lead = prompt("lead"),
    planner = prompt("planner"),
    reviewer = prompt("plan-reviewer");
  const planReview = lead.indexOf("REQUEST plan-reviewer on the plan before any builder ASSIGN");
  assert.ok(planReview > 0, "lead requests the plan review");
  assert.ok(planReview < lead.indexOf("send builder one ASSIGN"), "plan review comes before the builder ASSIGN");
  assert.match(lead, /on blockers, REQUEST one planner revision/);
  assert.match(lead, /then decide: no plan-review loop/);
  assert.match(lead, /Bug: assign builder from the plan directly/);
  assert.match(lead, /Plan and plan review use REQUEST, never ASSIGN \(it freezes a1…aN\) or REVIEW/);
  assert.match(planner, /plan-review blockers, send one revised RESULT that maps each blocker ID/);
  // A RESULT envelope accepts only outcome done or partial (hub/internal/api/envelope.go).
  assert.match(reviewer, /Answer each REQUEST with one RESULT to lead using --outcome done; a RESULT outcome is done or partial, never pass\./);
  assert.match(reviewer, /The text gives the verdict: pass, or numbered plan blockers p1…pN/);
  assert.match(reviewer, /--status for each planned criterion a1…aN .* at least one --evidence entry/);
  assert.match(reviewer, /one focused check of your blocker IDs only, answered by its own RESULT/);
  assert.doesNotMatch(reviewer, /exactly one RESULT|outcome is pass/);
  assert.match(reviewer, /missing acceptance coverage, wrong file ownership, unsafe step or unverifiable step/);
  assert.match(reviewer, /its reason, and evidence/);
  assert.match(reviewer, /never review code/);
  assert.match(reviewer, /There is no second general plan review/);
  assert.match(planned.fit, /Features add a plan reviewer before the builder; bugs go plan → builder/);
  assert.match(planned.workflow, /for a feature, plan reviewer passes the plan or lists blockers/);
  // The spawn command caps "Role: ROLE\n\nPROMPT" at 8192 characters.
  for (const member of members)
    assert.ok(`Role: ${member.role}\n\n${member.prompt}`.length <= 8192, member.name);
});

// wi_f8d48780626165cc: the small-change lane is queue-only, reuses the
// Planned builder and reviewer byte for byte and leaves the gallery alone.
test("queue-only small change reuses Planned seats and stays out of the gallery", () => {
  assert.equal(TEAM_EXAMPLES.length, 10);
  assert.ok(!TEAM_EXAMPLES.some((example) => example.id === "small"));
  assert.deepEqual(QUEUE_TEAM_TEMPLATES.map((template) => template.id), ["small"]);
  const small = exampleTeam("small");
  assert.equal(small.orchestrator, "lead");
  assert.equal(small.swarm, false);
  assert.deepEqual(small.members.map((member) => member.name), ["lead", "builder", "reviewer"]);
  const planned = exampleTeam("planned");
  for (const name of ["builder", "reviewer"])
    assert.deepEqual(
      small.members.find((member) => member.name === name),
      planned.members.find((member) => member.name === name),
      name,
    );
  const lead = small.members[0];
  assert.deepEqual([lead.runtime, lead.model, lead.reasoning], ["claude", "claude-opus-5-5", "high"]);
  assert.match(lead.prompt, /Post with tt send/);
  assert.match(lead.prompt, /run tt ack SEQ before you start/);
  assert.match(lead.prompt, /inventory useful long-lived services/);
  assert.ok(`Role: ${lead.role}\n\n${lead.prompt}`.length <= 8192);
  const copy = exampleTeam("small");
  copy.members[1].prompt = "changed";
  assert.equal(exampleTeam("planned").members.find((member) => member.name === "builder").prompt, planned.members.find((member) => member.name === "builder").prompt);
  assert.notEqual(exampleTeam("small").members[1].prompt, "changed");
});

test("documented small-change lead prompt exactly matches the template", () => {
  const documentation = readFileSync(new URL("../docs/team-launch.md", import.meta.url), "utf8");
  for (const member of exampleTeam("small").members)
    if (member.name === "lead")
      assert.ok(documentation.includes(member.prompt), "missing exact small/lead prompt");
});
