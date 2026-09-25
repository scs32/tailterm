import test from "node:test";
import assert from "node:assert/strict";
import {
  withDatabaseHandler,
  normalizeHandlerPlan,
  normalizeHandlerPlans,
} from "../client/project-handler.js";

const server = { id: "host", password: "fixture-only-secret" };
const lead = {
  name: "lead",
  runtime: "codex",
  run: "codex --some-lead-only-flag",
  cwd: "/repo",
  model: "custom-model",
  permissionMode: "workspace-auto",
  allowedTools: [],
  prompt: "Lead assignment that must not be copied.",
};
const plan = (fields = lead) => [{ server, fields }];
const stored = () => ({
  hub: "http://fixture-hub:18765/",
  taskId: "tsk_0123456789abcdef",
  serverId: server.id,
  fields: {
    ...withDatabaseHandler(plan())[1].fields,
    agentId: "agt_0123456789abcdef",
  },
});

test("handler launches second as a wake-capable Codex role with its own command/assignment", () => {
  const input = [
    ...plan(),
    { server: { id: "other" }, fields: { ...lead, name: "worker" } },
  ];
  const before = structuredClone(input);
  const result = withDatabaseHandler(input);
  assert.deepEqual(
    result.map((p) => p.fields.name),
    ["lead", "db-handler", "worker"],
  );
  const handler = result[1];
  assert.equal(handler.server, server);
  assert.equal(handler.fields.cwd, lead.cwd);
  assert.equal(handler.fields.runtime, "codex");
  assert.equal(handler.fields.run, "codex");
  assert.equal(handler.fields.model, "");
  assert.equal(handler.fields.reasoning, "");
  assert.equal(handler.fields.permissionMode, "");
  assert.equal(handler.fields.approvalMode, "");
  assert.equal(handler.fields.sandboxMode, "");
  assert.deepEqual(handler.fields.allowedTools, []);
  assert.equal(handler.fields.agentRole, "database_handler");
  assert.match(handler.fields.prompt, /Before each new queued-item record, run tt obligations/);
  assert.match(handler.fields.prompt, /RESULT --reply-to/);
  assert.ok(!handler.fields.prompt.includes(lead.prompt));
  result[0].fields.allowedTools.push("mutated copy");
  handler.fields.allowedTools.push("mutated handler");
  assert.deepEqual(input, before);
});

test("handler names avoid case-insensitive collisions and capacity includes handler", () => {
  const input = ["lead", "DB-HANDLER", "db-handler-2"].map((name) => ({
    server,
    fields: { ...lead, name },
  }));
  assert.equal(withDatabaseHandler(input)[1].fields.name, "db-handler-3");
  const many = Array.from({ length: 31 }, (_, i) => ({
    server,
    fields: { ...lead, name: "agent" + i },
  }));
  assert.equal(withDatabaseHandler(many).length, 32);
  assert.throws(
    () =>
      withDatabaseHandler([
        ...many,
        { server, fields: { ...lead, name: "extra" } },
      ]),
    /31 agents/,
  );
  assert.throws(() => withDatabaseHandler([]), /orchestrator/);
  assert.throws(
    () => withDatabaseHandler(withDatabaseHandler(plan())),
    /already includes/,
  );
});

test("handler does not inherit generic or Claude launch settings", () => {
  const generic = {
    ...lead,
    runtime: "generic",
    run: "custom-run --serve",
    model: "",
    permissionMode: "",
  };
  const genericHandler = withDatabaseHandler(plan(generic))[1].fields;
  assert.equal(genericHandler.runtime, "codex");
  assert.equal(genericHandler.run, "codex");
  const claude = {
    ...lead,
    runtime: "claude",
    run: "claude --extra",
    permissionMode: "dontAsk",
    allowedTools: ["Bash(tt *)"],
  };
  const handler = withDatabaseHandler(plan(claude))[1].fields;
  assert.equal(handler.runtime, "codex");
  assert.equal(handler.run, "codex");
  assert.equal(handler.model, "");
  assert.equal(handler.permissionMode, "");
  assert.deepEqual(handler.allowedTools, []);
});

test("saved plans retain recovery identity, omit unknown/credential fields and preserve missing hosts", () => {
  const input = stored();
  input.fields.expectedRunId = "run_0123456789abcdef";
  input.fields.token = "fixture-token";
  input.fields.password = "fixture-password";
  input.server = server;
  const result = normalizeHandlerPlan(input, []);
  assert.equal(result.hub, "http://fixture-hub:18765");
  assert.equal(result.serverId, "host");
  assert.equal(result.fields.agentId, input.fields.agentId);
  assert.equal(result.fields.expectedRunId, input.fields.expectedRunId);
  assert.equal(result.fields.token, undefined);
  assert.equal(result.fields.password, undefined);
  assert.equal(result.server, undefined);
  assert.deepEqual(normalizeHandlerPlan(result, [server]), result);
});

test("saved plan validation rejects malformed scopes, roles, IDs and unbounded fields", () => {
  for (const patch of [
    { hub: "https://user:password@example.com" },
    { taskId: "project-name" },
    { serverId: "a".repeat(81) },
    { fields: { ...stored().fields, agentRole: "orchestrator" } },
    { fields: { ...stored().fields, agentId: "not-an-agent" } },
    { fields: { ...stored().fields, expectedRunId: "not-a-run" } },
    {
      fields: {
        ...stored().fields,
        agentId: "",
        expectedRunId: "run_0123456789abcdef",
      },
    },
    { fields: { ...stored().fields, cwd: "relative" } },
    { fields: { ...stored().fields, prompt: "a".repeat(8193) } },
    { fields: { ...stored().fields, run: "a".repeat(1025) } },
    { fields: { ...stored().fields, model: 123 } },
    { fields: { ...stored().fields, allowedTools: Array(31).fill("rule") } },
  ])
    assert.throws(() =>
      normalizeHandlerPlan({ ...stored(), ...patch }, [server]),
    );
  assert.throws(() => normalizeHandlerPlans(Array(201).fill(stored())), /200/);
  assert.throws(
    () =>
      normalizeHandlerPlans([
        stored(),
        { ...stored(), hub: "http://fixture-hub:18765" },
      ]),
    /Duplicate/,
  );
});

test("previous handler settings survive normalization with one bounded recovery level", () => {
  const input = stored();
  input.previous = {
    serverId: "removed-host",
    fields: {
      ...input.fields,
      model: "previous-model",
      token: "fixture-secret",
    },
    password: "fixture-secret",
    server,
  };
  const result = normalizeHandlerPlan(input, []);
  assert.deepEqual(result.previous, {
    serverId: "removed-host",
    fields: { ...result.fields, model: "previous-model" },
  });
  assert.deepEqual(normalizeHandlerPlans([result]), [result]);
  result.previous.fields.allowedTools.push("copy-only");
  assert.deepEqual(input.previous.fields.allowedTools, []);
  for (const previous of [
    null,
    [],
    { ...input.previous, serverId: "" },
    { ...input.previous, fields: { ...input.fields, cwd: "relative" } },
    { ...input.previous, fields: { ...input.fields, agentRole: "lead" } },
    {
      ...input.previous,
      fields: { ...input.fields, prompt: "x".repeat(8193) },
    },
    { ...input.previous, previous: input.previous },
  ])
    assert.throws(() => normalizeHandlerPlan({ ...input, previous }));
});
