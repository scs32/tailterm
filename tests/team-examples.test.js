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
  assert.match(lead, /send no Start, plan or assignment gate REQUESTs/);
  assert.doesNotMatch(lead, /For each live Start, plan or assignment gate/);
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
  assert.match(lead, /Once it returns its receipt and all team obligations are closed, run tt close --team/);
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

test("the steward proposes a token estimate with every filing and ranking", () => {
  const steward = PROJECT_ROLE_TEMPLATES.backlog_steward.prompt;
  const rule =
    'Estimate: every filing and ranking REQUEST proposes a token estimate for the item with a one-line basis, such as "Small, 2 paths, median of 8 Small items", taken from the lane, the owned-path count and comparable done items in tt usage --calibration; the handler records it.';
  assert.equal(steward.split(rule).length - 1, 1);
  assert.equal(steward.split("token estimate").length - 1, 1);
  assert.equal(steward.split("tt usage --calibration").length - 1, 1);
  // The spawn command caps the prompt at 8192 characters.
  assert.ok(steward.length <= 8192, `steward prompt is ${steward.length} characters`);
  assert.ok(Buffer.byteLength(steward) < 8192);
  // The steward proposes; only the handler saves it.
  assert.doesNotMatch(steward, /--estimate-tokens/);
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
  assert.match(reviewer, /hub-saved eligible receipt judges those criteria/);
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
  assert.match(lead, /send reviewer a REVIEW naming that commit, the scope and the criteria\. Two review rounds/);
  assert.match(lead, /Verify alongside review, at once: freeze the plan on the current tasks-hub tip with tt verification plan and REQUEST the distinct verifier/);
  assert.match(lead, /Each later candidate gets a fresh run, targeted from the previous one for fixes; withdraw the superseded REQUEST/);
  assert.match(lead, /final candidate, rebased onto the current tip, gets the full plan once/);
  assert.match(lead, /hub-saved passing receipt for the exact final SHA/);
  assert.doesNotMatch(lead, /Before acceptance, REQUEST the distinct verifier/);
  assert.match(verifier, /verify-matrix\.mjs targeted CONTEXT_JSON/);
  assert.match(verifier, /never imported and never gates acceptance/);
  assert.match(verifier, /stop its run with SIGINT/);
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

// wi_5bd7ba47f56fab7f: one Start rule for a team queue entry. The fixture
// scenario is the single string this test and the Go test
// TestQueueTeamStartRuleIsSharedByAuditHandlerAndTeamTexts compare against.
test("queue teams, handlers and docs state one Start rule", () => {
  const read = (file) => readFileSync(new URL(file, import.meta.url), "utf8");
  const count = (text, clause) => text.split(clause).length - 1;
  const scenario = JSON.parse(read("./handler-allocation-cases.json")).find(
    (candidate) => candidate.name === "queue-team-start-evidence",
  );
  const [rule, accept] = scenario.worker;
  assert.equal(scenario.lead[0], rule);

  for (const id of ["planned", "small"])
    for (const member of exampleTeam(id).members)
      assert.equal(
        count(member.prompt, rule),
        ["lead", "builder"].includes(member.name) ? 1 : 0,
        `${id}/${member.name}`,
      );

  const go = read("../hub/cmd/tt/coordination.go");
  assert.equal(count(go, rule), 1);
  assert.equal(count(go, accept), 1);

  // wi_2b66e2a634afa39d: every text that states the rule also says which
  // revisions are amendments, in the words of the queue doc.
  const amendment = "An amendment changes owned paths, criteria or scope; any other revision needs no Start.";
  for (const id of ["planned", "small"])
    for (const member of exampleTeam(id).members) {
      const wanted = ["lead", "builder"].includes(member.name) ? 1 : 0;
      assert.equal(count(member.prompt, `${rule} ${amendment}`), wanted, `${id}/${member.name}`);
      assert.equal(count(member.prompt, amendment), wanted, `${id}/${member.name}`);
    }
  assert.equal(count(go, amendment), 1);
  assert.equal(count(go, 'queueTeamStartHandlerRule + " " + queueTeamStartAmendmentRule'), 2);
  assert.ok(read("../hub/internal/teamplan/plan.mjs").includes(amendment), "generated plan.mjs lacks the amendment definition");
  const queueDoc = read("../docs/project-queue.md");
  assert.ok(
    queueDoc.replace(/\s+/g, " ").includes("changes the owned paths, the criteria or the scope"),
    "the queue doc no longer defines a scope amendment",
  );
  assert.equal(count(queueDoc, rule), 1);
  assert.equal(count(queueDoc, accept), 1);
  assert.equal(count(queueDoc, "\n## Queue team Start evidence\n"), 1);
  for (const file of ["../docs/team-launch.md", "../docs/team-examples.md"])
    assert.ok(read(file).includes("project-queue.md#queue-team-start-evidence"), file);
});

// wi_26c0698de7d3eef2: the plan freeze, the receipt import and the done save
// with queue acceptance are validated hub operations the lead and the
// verifier call themselves; the handler keeps intake, scope confirmation,
// completion reports and every case the hub refuses.
test("lead and verifier run the validated hub operations without a handler turn", () => {
  const planned = exampleTeam("planned").members;
  const prompt = (name) => planned.find((member) => member.name === name).prompt;
  const lead = prompt("lead"),
    builder = prompt("builder"),
    verifier = prompt("verifier"),
    handler = prompt("database"),
    small = exampleTeam("small").members[0].prompt;

  for (const [name, text] of [["planned lead", lead], ["small lead", small]]) {
    assert.match(text, /tt verification plan/, name);
    assert.match(text, /tt work-items update --status done/, name);
    assert.match(text, /--worktree/, name);
    assert.match(text, /hub-saved passing receipt for the exact final SHA/, name);
  }
  assert.match(small, /tt verification plan --item ID --file plan\.json --request-id KEY --generation N/);
  assert.match(small, /tt verification receipt --item ID --file receipt\.json --request-id KEY --generation N/);
  assert.match(small, /tt work-items update --status done --worktree DIR --branch B --commit SHA/);
  assert.match(small, /Ask the handler only when the hub refuses, quoting its reason/);
  assert.match(small, /at once freeze the verification plan on the current tasks-hub tip/);
  assert.match(small, /Once your done save returns its receipt and all team obligations are closed, run tt close --team/);
  assert.match(verifier, /Import the receipt yourself: tt verification receipt --item ID --file receipt\.json --request-id KEY --generation N/);
  assert.match(verifier, /notifies lead, reviewer and handler/);
  assert.match(verifier, /Never import a targeted-receipt\.json/);
  assert.match(builder, /Begin only on lead's ASSIGN, which after an amendment follows that Start REQUEST/);

  // A lead sends the handler one Start REQUEST after a scope amendment and
  // nothing else. These are the retired handler-gate phrasings: no lead,
  // builder or verifier prompt asks for a per-step Start, plan or assignment
  // gate, or for a handler freeze, import or done-save turn.
  for (const [name, text] of [["planned lead", lead], ["small lead", small], ["builder", builder], ["verifier", verifier]])
    for (const handlerTurn of [
      /For each live Start, plan or assignment gate/,
      /When requesting a Start or assignment gate/,
      /have the handler freeze/,
      /REQUEST the verification plan/,
      /Send the database handler a typed REQUEST/,
      /to the database handler through a typed REQUEST/,
      /The handler imports/,
      /handler-saved|handler-approved|handler-imported/,
      /handler confirms a terminal item/,
      /Wait for its RESULT --reply-to/,
    ])
      assert.doesNotMatch(text, handlerTurn, name);

  assert.match(handler, /intake, scope confirmation, work items, revisions, orders, completion reports and release receipts/);
  assert.match(handler, /write a feature's completion report/);
  assert.match(handler, /For a queued team the hub validates three operations without you/);
  assert.match(handler, /Do one of them only when lead asks after the hub refused it, quoting the reason, or for a team with no queue entry/);

  for (const member of [...planned, ...exampleTeam("small").members])
    assert.ok(`Role: ${member.role}\n\n${member.prompt}`.length <= 8192, `${member.name} is ${member.prompt.length} characters`);
  const bundle = readFileSync(new URL("../hub/internal/teamplan/plan.mjs", import.meta.url), "utf8");
  assert.ok(bundle.includes("Import the receipt yourself: tt verification receipt"), "generated plan.mjs lacks the verifier's import");
});

// wi_8c5f65b4d6038b43 / order #20747: runs start at once and wait in the
// host lock's ordered waitlist; nobody waits for an idle host by hand.
test("verifier and small lead start the matrix at once and wait in the host waitlist", () => {
  const verifier = exampleTeam("planned").members.find((m) => m.name === "verifier").prompt,
    lead = exampleTeam("small").members.find((m) => m.name === "lead").prompt;
  const tail =
    "; it waits its turn in the host lock's ordered waitlist (position: tt team queue list). Never wait for an idle host by hand (no pgrep or sleep loops), and run no ad hoc tests while your run holds or waits for the host.";
  assert.ok(verifier.includes("Start each run at once" + tail));
  assert.ok(lead.includes("node scripts/verify-matrix.mjs run PLAN_JSON EXTERNAL_LOG_DIRECTORY. Start it at once" + tail));
  for (const prompt of [verifier, lead]) assert.doesNotMatch(prompt, /one matrix run per host/);
  const bundle = readFileSync(new URL("../hub/internal/teamplan/plan.mjs", import.meta.url), "utf8");
  assert.equal(bundle.split(tail).length - 1, 2);
  assert.ok(!bundle.includes("one matrix run per host"));
});

// wi_2430de4c12e43df4 (owner decision #19235): the steward proposes the
// small-change lane for small bugs and names one of three reasons for Planned.
test("the steward proposes the small lane for small bugs and names a reason for Planned", () => {
  const steward = PROJECT_ROLE_TEMPLATES.backlog_steward.prompt;
  const rule =
    "Lane: every queue proposal names the team template. Propose template small for a bug whose fix fits at most three owned paths, including the doc that describes the changed behaviour. Propose Planned delivery for a bug only with one named reason, passed to tt team queue add as --planned-reason: more than three owned paths (paths), a schema or migration change (schema), or a cross-cutting risk you name (risk:TEXT). Features stay Planned.";
  assert.ok(steward.includes(rule), "steward prompt lacks the lane rule");
  for (const reason of [/more than three owned paths \(paths\)/, /a schema or migration change \(schema\)/, /a cross-cutting risk you name \(risk:TEXT\)/])
    assert.match(steward, reason);
  assert.ok(steward.length <= 8192, `steward prompt is ${steward.length} characters`);
  const bundle = readFileSync(new URL("../hub/internal/teamplan/plan.mjs", import.meta.url), "utf8");
  assert.equal(bundle.split(rule).length - 1, 1, "generated plan.mjs lacks the steward lane rule");
});

// wi_2b66e2a634afa39d (FINDING #29036): the unsharded store race no longer
// fits 30 minutes, so the planner and verifier name the sharded command the
// matrix runs and both leads say a store race is sharded. No team or role
// text may carry an unsharded store race command or one under 45 minutes.
test("store race checks name the sharded matrix command", () => {
  const count = (text, clause) => text.split(clause).length - 1;
  const matrix = JSON.parse(readFileSync(new URL("../verification/matrix.json", import.meta.url), "utf8"));
  const shards = matrix.goRaceShards["./internal/store"];
  const full = `A store race check names the sharded command the matrix uses, run from hub/: node ../scripts/verify-matrix.mjs go-race -timeout=45m -shards=./internal/store=${shards} ./internal/store.`;
  const short = "Store race checks run sharded.";
  const members = (id) => Object.fromEntries(exampleTeam(id).members.map((member) => [member.name, member.prompt]));
  const planned = members("planned"),
    small = members("small");
  for (const [name, prompt] of Object.entries(planned)) {
    assert.equal(count(prompt, full), ["planner", "verifier"].includes(name) ? 1 : 0, `planned/${name}`);
    assert.equal(count(prompt, short), name === "lead" ? 1 : 0, `planned/${name}`);
  }
  assert.equal(count(small.lead, short), 1);
  assert.equal(count(small.lead, full), 0);

  // A store race command is the sentence around a -race flag, up to the next
  // one, when it names the store package. It is refused without the shard flag
  // or with a timeout under 45 minutes.
  const unsafe = (text) => {
    const found = [];
    for (const line of text.split("\n"))
      for (let at = line.indexOf("-race"); at >= 0; ) {
        const next = line.indexOf("-race", at + 1);
        const start = Math.max(line.lastIndexOf(". ", at), line.lastIndexOf("; ", at)) + 1;
        const command = line.slice(start, next < 0 ? line.length : next);
        const timeout = /-timeout[= ](\d+)m/.exec(command);
        if (
          command.includes("./internal/store") &&
          (!command.includes("-shards=./internal/store=") || !timeout || Number(timeout[1]) < 45)
        )
          found.push(command.trim());
        at = next;
      }
    return found;
  };
  assert.equal(unsafe("Run go test -race -timeout 30m ./internal/store").length, 1);
  assert.equal(unsafe("Run go test -race ./internal/store").length, 1);
  assert.equal(unsafe("node ../scripts/verify-matrix.mjs go-race -timeout=30m -shards=./internal/store=4 ./internal/store").length, 1);
  assert.equal(unsafe("Run go test -timeout 60m -race ./internal/store").length, 1);
  assert.equal(unsafe(`${full} Then go test -race -timeout 30m ./internal/store`).length, 1);
  assert.equal(unsafe("Run go test -race ./cmd/tt").length, 0);
  assert.equal(unsafe(full).length, 0);
  const texts = [
    ...[...TEAM_EXAMPLES, ...QUEUE_TEAM_TEMPLATES].flatMap((team) =>
      exampleTeam(team.id).members.map((member) => [`${team.id}/${member.name}`, member.prompt]),
    ),
    ...Object.entries(PROJECT_ROLE_TEMPLATES).map(([role, template]) => [role, template.prompt]),
    ["plan.mjs", readFileSync(new URL("../hub/internal/teamplan/plan.mjs", import.meta.url), "utf8")],
  ];
  for (const [name, text] of texts) assert.deepEqual(unsafe(text), [], name);
});
