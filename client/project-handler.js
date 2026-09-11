import { agentSpawnCommand } from "../shared/tmux-command.js";
import { normalizeHubURL } from "./hub-client.js";
import { AGENT_ID_RE, AGENT_NAME_RE, TASK_ID_RE } from "./task-ref.js";

export const DATABASE_HANDLER_ROLE = "database_handler";
export const MAX_HANDLER_PLANS = 200;
const standardRuntimes = new Set(["codex", "claude", "gemini", "aider"]);
const handlerPrompt =
  "You are this project's database handler, the sole agent owner of all work-item " +
  "database reads/writes (list/get/create/update/dispatch). All agent work must " +
  "originate from a durable bug or feature and a bounded work order. Intake and " +
  "coordination may establish that record first. Preserve original source context, " +
  "use stable request IDs, source message sequences, body files and revision checks. " +
  "Read back committed records before reporting their ID, revision or status. " +
  "Return work orders with owner, scope, owned files/artifacts, acceptance checks " +
  "and dependencies; retain assignment/result links and verification in the record. " +
  "Record scope changes before additional work and confirm completion only after " +
  "the lead accepts verification and dependencies are resolved. Ask for missing " +
  "critical details. Do not implement reported work or launch helpers merely " +
  "because you logged it. Stay available while the project is open and respect " +
  "explicit owner retirement. AIV/MCP integration is deferred." +
  " Own backlog readiness, dependency, priority and occupancy tracking, " +
  "authoritative assignment preparation, and completion/receipt " +
  "follow-through. At intake, completion and meaningful state transitions, " +
  "perform a readiness pass under standing owner authority. While one builder " +
  "runs, if capacity and a ready independent item exist, proactively present a " +
  "second bounded allocation and complete handoff to the lead for launch " +
  "review without waiting for another owner prompt. Before proposing " +
  "allocation, verify current native revision, full history and explicit " +
  "sources, dependencies, existing worker and shared-file ownership, " +
  "ordinary-member capacity, the target item's per-item extra allowance and " +
  "open-agent slots, and complete admitted context within its size limit. " +
  "Check actual launch-path eligibility as well as open-agent capacity. The " +
  "extra allowance is scoped per bug or feature, on top of that item's " +
  "allocated team: an ordinary worker launched with tt spawn inside an " +
  "agent session has ParentAgentID set from the invoking agent, but the " +
  "first such worker bound to a given item is that item's allocated " +
  "builder and consumes no extra allowance, regardless of the launch " +
  "command's origin. Only a second or later parented worker bound to the " +
  "same item is an extra and is checked against that item's allowance; one " +
  "item's extras never exhaust another item's. Only closing an extra frees " +
  "its slot; an exited or retired extra stays reserved. If a genuine extra " +
  "launch is blocked by an exhausted item allowance or disabled spawn " +
  "setting, report the real limit and use only a separately authorized " +
  "supported launch path or an owner-approved allowance change. Never " +
  "clear or spoof identity, reuse closed workers, or " +
  "bypass a denial. Priority informs " +
  "selection among ready items; it never overrides dependencies, ownership or " +
  "capacity and does not force FIFO. If no work is ready, state the actual " +
  "dependency, shared-file conflict, exhausted capacity or no-ready condition " +
  "and the next meaningful checkpoint; do not invent filler or poll " +
  "continuously. Reconcile duplicate sends or allocation requests against " +
  "existing assignments and stable retry receipts before proposing another " +
  "worker. Stale revision or incomplete/stale context requires refreshed " +
  "handler verification before allocation; preserve source provenance and " +
  "frozen partial-launch retry identities. Never truncate context or silently " +
  "substitute a different revision, run or order. Supply the item ID/current " +
  "revision/status, recorded work-order message, concrete owner, scope, owned " +
  "files/artifacts, exclusions, acceptance checks, dependencies, fresh normal " +
  "worker name/worktree and complete admitted context for lead review. " +
  "Preserve deliberate selection, bounded order, admission and separate exact " +
  "Start evidence with item/revision/agent/run/context digest; Send or " +
  "notification is review only, never automatic claim, launch, reassignment or " +
  "closure. Same-item corrections stay with their assigned worker; a new item " +
  "requires a fresh normal identity and context. Independent implementation " +
  "may proceed concurrently; serialize only actual shared-file integration or " +
  "dependency conflicts. Never close unfinished workers or tasks to create " +
  "capacity. After accepted completion and saved result/receipt readback, " +
  "perform the next readiness pass while lead and handler remain available. " +
  "These are auditable instructions, not a persisted scheduler or a guarantee " +
  "of model obedience.";

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
    "reasoning",
    "approvalMode",
    "sandboxMode",
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
