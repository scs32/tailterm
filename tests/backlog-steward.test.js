import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { PROJECT_ROLE_TEMPLATES, TEAM_EXAMPLES } from "../client/team-examples.js";
import { rosterRole } from "../client/tasks-view.js";
import { MODEL_OPTIONS } from "../client/model-picker.js";

// Backlog steward template (wi_5b4b94dbc9a11e8b, a4).
const steward = PROJECT_ROLE_TEMPLATES.backlog_steward;

test("the backlog steward template runs GPT-6.1 Sol at high reasoning", () => {
  assert.equal(steward.role, "backlog_steward");
  assert.equal(steward.runtime, "codex");
  assert.equal(steward.model, "gpt-6.1-sol");
  assert.equal(steward.reasoning, "high");
  assert.equal(steward.name, "backlog-steward");
  assert.equal(steward.title, "Backlog steward");
  assert.ok(MODEL_OPTIONS[steward.runtime].some(([id]) => id === steward.model));
  assert.ok(Buffer.byteLength(steward.prompt) < 8192);
});

test("the steward is a project role, not a team example", () => {
  assert.equal(TEAM_EXAMPLES.length, 10);
  for (const example of TEAM_EXAMPLES)
    for (const member of example.members)
      assert.notEqual(member.role, "backlog_steward", `${example.id}/${member.name}`);
});

test("the steward prompt teaches ack, handler writes and owner decisions", () => {
  const prompt = steward.prompt;
  assert.match(prompt, /run tt ack SEQ before you start/);
  assert.doesNotMatch(prompt, /do not acknowledge|acknowledge acknowledgements/i);
  assert.match(prompt, /you never create, update or dispatch items yourself/);
  assert.match(prompt, /every write goes through the database handler/);
  assert.match(prompt, /heldForTriage/);
  assert.match(prompt, /Discord \/bug and \/feature/);
  assert.match(prompt, /created by discord-bridge that are still at revision 1/);
  assert.match(prompt, /tt ask decision/);
  assert.match(prompt, /to the owner, or the delegate under a window, for queue reorder/);
  assert.match(prompt, /no per-item records, merges, releases or acceptance/);
  assert.match(prompt, /owner session is the owner's conversation partner and relays owner decisions/);
  assert.match(prompt, /Themes, Open questions, Batches, Pending proposals and Held follow-ups/);
  assert.match(prompt, /ask the owner one question .* only when intent is unclear/i);
});

test("the steward prompt appears verbatim in the team examples documentation", () => {
  const documentation = readFileSync(new URL("../docs/team-examples.md", import.meta.url), "utf8");
  assert.ok(documentation.includes(steward.prompt), "missing exact backlog_steward prompt");
  assert.match(documentation, /## Project roles/);
});

test("the roster labels the backlog steward", () => {
  assert.equal(rosterRole({ role: "backlog_steward" }), " · Backlog steward");
  assert.equal(rosterRole({ role: "database_handler" }), " · Database handler");
  assert.equal(rosterRole({ role: "" }), "");
});
