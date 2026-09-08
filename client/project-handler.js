import { agentSpawnCommand } from "../shared/tmux-command.js";
import { normalizeHubURL } from "./hub-client.js";
import { AGENT_ID_RE, AGENT_NAME_RE, TASK_ID_RE } from "./task-ref.js";

export const DATABASE_HANDLER_ROLE = "database_handler";
export const MAX_HANDLER_PLANS = 200;
const standardRuntimes = new Set(["codex", "claude", "gemini", "aider"]);
const handlerPrompt =
  "You are this project's database handler. Record requested bugs and features " +
  "using the project's work-item API/CLI, preserve source context, and reply with " +
  "the saved item ID after a successful write. Ask for missing critical details. " +
  "Do not claim a record was saved before the write succeeds. Do not implement " +
  "reported work or launch helpers unless the orchestrator assigns that work.";

function spawnFields(value) {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new Error("Invalid database handler launch fields.");
  const result = {};
  for (const field of [
    "name",
    "run",
    "cwd",
    "runtime",
    "model",
    "permissionMode",
    "prompt",
  ]) {
    const v = value[field] === undefined ? "" : value[field];
    if (typeof v !== "string")
      throw new Error(`Invalid database handler ${field}.`);
    result[field] = v;
  }
  if (!AGENT_NAME_RE.test(result.name) || !result.runtime || !result.cwd)
    throw new Error(
      "Choose a handler name, runtime and absolute project folder.",
    );
  if (value.allowedTools !== undefined && !Array.isArray(value.allowedTools))
    throw new Error("Invalid database handler allowed tools.");
  result.allowedTools = [...(value.allowedTools || [])];
  result.agentRole = DATABASE_HANDLER_ROLE;
  for (const [field, pattern] of [
    ["agentId", AGENT_ID_RE],
    ["expectedRunId", /^run_[0-9a-f]{16}$/],
  ]) {
    if (value[field] === undefined || value[field] === "") continue;
    if (typeof value[field] !== "string" || !pattern.test(value[field]))
      throw new Error(`Invalid database handler ${field}.`);
    result[field] = value[field];
  }
  if (result.expectedRunId && !result.agentId)
    throw new Error("A handler restart requires its saved agent identity.");
  return result;
}

export function normalizeHandlerPlanKey(value) {
  const hub = typeof value?.hub === "string" && normalizeHubURL(value.hub);
  if (
    !hub ||
    typeof value.taskId !== "string" ||
    !TASK_ID_RE.test(value.taskId)
  )
    throw new Error("Choose a valid hub and project for the database handler.");
  return { hub, taskId: value.taskId };
}

function normalizeHandlerSettings(value, key) {
  if (
    !value ||
    typeof value !== "object" ||
    Array.isArray(value) ||
    typeof value.serverId !== "string" ||
    !value.serverId ||
    value.serverId.length > 80 ||
    /[\x00-\x1f\x7f]/.test(value.serverId)
  )
    throw new Error("Choose a valid saved server for the database handler.");
  if (value.fields?.agentRole !== DATABASE_HANDLER_ROLE)
    throw new Error("A handler plan requires the database_handler role.");
  const fields = spawnFields(value.fields);
  agentSpawnCommand({ ...fields, hub: key.hub, task: key.taskId });
  // Retain a missing server ID for recovery UI. Never copy credentials or a
  // mutable server object into the persisted plan, or silently choose a host.
  return { serverId: value.serverId, fields };
}

export function normalizeHandlerPlan(value, servers = []) {
  const key = normalizeHandlerPlanKey(value);
  if (!Array.isArray(servers))
    throw new Error("Invalid saved servers for the database handler.");
  const result = { ...key, ...normalizeHandlerSettings(value, key) };
  if (value.previous !== undefined) {
    if (value.previous?.previous !== undefined)
      throw new Error("A handler plan can retain only one previous setting.");
    result.previous = normalizeHandlerSettings(value.previous, key);
  }
  return result;
}

export function normalizeHandlerPlans(value, servers = []) {
  if (value === undefined) return [];
  if (!Array.isArray(value) || value.length > MAX_HANDLER_PLANS)
    throw new Error(`At most ${MAX_HANDLER_PLANS} database handler plans.`);
  const seen = new Set();
  return value.map((entry) => {
    const plan = normalizeHandlerPlan(entry, servers);
    const key = JSON.stringify([plan.hub, plan.taskId]);
    if (seen.has(key)) throw new Error("Duplicate database handler plan.");
    seen.add(key);
    return plan;
  });
}

export function withDatabaseHandler(plan) {
  if (!Array.isArray(plan) || !plan.length)
    throw new Error(
      "Choose an orchestrator before adding the database handler.",
    );
  if (plan.length > 31)
    throw new Error(
      "Choose at most 31 agents to leave room for the database handler.",
    );
  const names = new Set();
  for (const entry of plan) {
    if (!entry?.server?.id || !AGENT_NAME_RE.test(entry.fields?.name || ""))
      throw new Error("Choose a resolved server and name for every agent.");
    if (entry.fields.agentRole === DATABASE_HANDLER_ROLE)
      throw new Error("The launch plan already includes a database handler.");
    const name = entry.fields.name.toLowerCase();
    if (names.has(name)) throw new Error("Agent names must be unique.");
    names.add(name);
  }
  const first = plan[0];
  let name = "db-handler";
  for (let n = 2; names.has(name.toLowerCase()); n++) name = `db-handler-${n}`;
  const fields = spawnFields({
    ...first.fields,
    name,
    run: standardRuntimes.has(first.fields.runtime)
      ? first.fields.runtime
      : first.fields.run,
    prompt: handlerPrompt,
    agentId: undefined,
    expectedRunId: undefined,
  });
  agentSpawnCommand({
    ...fields,
    hub: "http://localhost",
    task: "tsk_0000000000000000",
  });
  const copies = plan.map(({ server, fields }) => ({
    server,
    fields: {
      ...fields,
      ...(fields.allowedTools && { allowedTools: [...fields.allowedTools] }),
    },
  }));
  copies.splice(1, 0, { server: first.server, fields });
  return copies;
}
