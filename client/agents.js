import { AGENT_NAME_RE } from "./task-ref.js";
import { validateReasoning } from "./reasoning.js";
import { agentSpawnCommand } from "../shared/tmux-command.js";

export const AGENT_CATALOG_VERSION = 2;
export const TEAMS_SCHEMA_VERSION = 2;
export const MAX_AGENT_DEFINITIONS = 1000;
export const MAX_TEAMS = 30;
const ID_RE = /^[A-Za-z0-9_-]{1,80}$/;

const newId = (prefix) =>
  `${prefix}_${crypto.randomUUID().replaceAll("-", "")}`;

function text(value, max, label, { required = false, control = false } = {}) {
  if (typeof value !== "string") throw new Error(`Invalid ${label}.`);
  const result = value.trim();
  if (
    (required && !result) ||
    result.length > max ||
    (control && /[\x00-\x1f\x7f]/.test(result))
  )
    throw new Error(`Invalid ${label}.`);
  return result;
}

export function normalizeAgentDefinition(value, index = 0) {
  const invalid = (message, field) => {
    throw Object.assign(new Error(message), { definitionIndex: index, field });
  };
  if (!value || typeof value !== "object" || Array.isArray(value))
    invalid("Invalid agent definition.", "name");
  let name, launchName, role, serverId, runtime, model, run, cwd, prompt;
  try {
    name = text(value.name, 80, "agent name", {
      required: true,
      control: true,
    });
    launchName = text(value.launchName, 64, "launch name", {
      required: true,
      control: true,
    });
    if (!AGENT_NAME_RE.test(launchName))
      throw new Error(
        "Use a launch name of 1–64 letters, numbers, dashes or underscores.",
      );
    role = text(value.role || "", 80, "role", { control: true });
    serverId = text(value.serverId || "", 80, "server", { control: true });
    runtime = text(value.runtime || "generic", 64, "runtime", {
      required: true,
      control: true,
    });
    model = text(value.model || "", 200, "model");
    run = text(
      value.run || (runtime !== "generic" ? runtime : ""),
      1024,
      "agent command",
      { required: true, control: true },
    );
    cwd = text(value.cwd || "", 512, "working directory", { control: true });
    prompt = typeof value.prompt === "string" ? value.prompt : "";
  } catch (error) {
    invalid(
      error.message,
      /launch name/i.test(error.message) ? "launchName" : "name",
    );
  }
  const id = value.id || newId("agent");
  if (!ID_RE.test(id)) invalid("Invalid agent definition ID.", "id");
  const revision = value.revision === undefined ? 1 : value.revision;
  if (!Number.isSafeInteger(revision) || revision < 1)
    invalid("Invalid agent definition revision.", "revision");
  const allowedTools = Array.isArray(value.allowedTools)
    ? [...value.allowedTools]
    : String(value.allowedTools || "")
        .split("\n")
        .map((entry) => entry.trim())
        .filter(Boolean);
  const result = {
    id,
    revision,
    name,
    launchName,
    role,
    serverId,
    runtime,
    model,
    reasoning: String(value.reasoning || ""),
    approvalMode: String(value.approvalMode || ""),
    sandboxMode: String(value.sandboxMode || ""),
    permissionMode: String(value.permissionMode || ""),
    allowedTools,
    run,
    cwd,
    prompt,
  };
  try {
    validateReasoning(result.runtime, result.model, result.reasoning);
    agentSpawnCommand({
      hub: "http://localhost",
      task: "tsk_0000000000000000",
      ...result,
      name: result.launchName,
      cwd:
        result.cwd || result.sandboxMode === "workspace-write"
          ? result.cwd || "/__launch_project__"
          : "",
      prompt: [result.role && `Role: ${result.role}`, result.prompt]
        .filter(Boolean)
        .join("\n\n"),
    });
  } catch (error) {
    const field = /reason/i.test(error.message)
      ? "reasoning"
      : /approval/i.test(error.message)
        ? "approvalMode"
        : /sandbox|workspace/i.test(error.message)
          ? "sandboxMode"
          : /permission/i.test(error.message)
            ? "permissionMode"
            : /allowed tool/i.test(error.message)
              ? "allowedTools"
              : /model/i.test(error.message)
                ? "model"
                : /directory/i.test(error.message)
                  ? "cwd"
                  : /prompt/i.test(error.message)
                    ? "prompt"
                    : /runtime/i.test(error.message)
                      ? "runtime"
                      : "run";
    invalid(error.message, field);
  }
  return result;
}

export function normalizeAgentCatalog(value) {
  if (
    !value ||
    value.version !== AGENT_CATALOG_VERSION ||
    !Array.isArray(value.definitions)
  )
    throw new Error("Unsupported Agents catalog version.");
  if (value.definitions.length > MAX_AGENT_DEFINITIONS)
    throw new Error(`At most ${MAX_AGENT_DEFINITIONS} agent definitions.`);
  const ids = new Set();
  const definitions = value.definitions.map((entry, index) => {
    const definition = normalizeAgentDefinition(entry, index);
    if (ids.has(definition.id))
      throw new Error(`Duplicate agent definition ID: ${definition.id}.`);
    ids.add(definition.id);
    return definition;
  });
  return { version: AGENT_CATALOG_VERSION, definitions };
}

function normalizeTeamReference(value, definitions, memberIndex) {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw Object.assign(
      new Error(`Agent ${memberIndex + 1}: Choose an agent definition.`),
      { memberIndex, field: "agentDefinitionId" },
    );
  const agentDefinitionId = String(value.agentDefinitionId || "");
  const definition = definitions.find(
    (entry) => entry.id === agentDefinitionId,
  );
  if (!definition)
    throw Object.assign(
      new Error(
        `Agent ${memberIndex + 1}: The selected agent definition is missing.`,
      ),
      { memberIndex, field: "agentDefinitionId" },
    );
  const alias = String(value.alias || "").trim();
  if (alias && !AGENT_NAME_RE.test(alias))
    throw Object.assign(
      new Error(
        `Agent ${memberIndex + 1}: Use a valid alias or leave it blank.`,
      ),
      { memberIndex, field: "alias" },
    );
  const role = String(value.role || "").trim();
  if (role.length > 80 || /[\x00-\x1f\x7f]/.test(role))
    throw Object.assign(
      new Error(
        `Agent ${memberIndex + 1}: Keep role overrides under 80 characters.`,
      ),
      { memberIndex, field: "role" },
    );
  return { agentDefinitionId, ...(alias && { alias }), ...(role && { role }) };
}

export function resolveTeamMember(reference, definitions) {
  const definition = definitions.find(
    (entry) => entry.id === reference.agentDefinitionId,
  );
  if (!definition)
    throw new Error("This team references a missing agent definition.");
  return {
    ...structuredClone(definition),
    agentDefinitionId: definition.id,
    agentDefinitionRevision: definition.revision,
    name: reference.alias || definition.launchName,
    role: reference.role || definition.role,
  };
}

export function normalizeReferencedTeam(value, definitions) {
  if (
    !value ||
    typeof value.name !== "string" ||
    !value.name.trim() ||
    value.name.length > 80 ||
    /[\x00-\x1f\x7f]/.test(value.name)
  )
    throw new Error("Enter a team name of up to 80 characters.");
  if (
    !Array.isArray(value.members) ||
    !value.members.length ||
    value.members.length > 32
  )
    throw new Error("A team needs 1–32 agents.");
  const members = value.members.map((entry, index) =>
    normalizeTeamReference(entry, definitions, index),
  );
  const names = new Set();
  const resolved = members.map((entry, index) => {
    const member = resolveTeamMember(entry, definitions);
    const folded = member.name.toLowerCase();
    if (names.has(folded))
      throw Object.assign(
        new Error(
          `Agent ${index + 1}: The resolved name “${member.name}” is already used in this team.`,
        ),
        { memberIndex: index, field: "alias" },
      );
    names.add(folded);
    return member;
  });
  const orchestrator = String(value.orchestrator || resolved[0].name);
  if (!resolved.some((entry) => entry.name === orchestrator))
    throw new Error("Choose a main orchestrator from this team.");
  return {
    id: ID_RE.test(value.id || "") ? value.id : newId("team"),
    name: value.name.trim(),
    swarm: value.swarm === true,
    orchestrator,
    members,
  };
}

function legacyPermission(member) {
  const result = {
    approvalMode: String(member.approvalMode || ""),
    sandboxMode: String(member.sandboxMode || ""),
    permissionMode: String(member.permissionMode || ""),
  };
  if (
    member.runtime === "codex" &&
    !result.approvalMode &&
    !result.sandboxMode
  ) {
    if (result.permissionMode === "on-request") {
      result.approvalMode = "on-request";
      result.permissionMode = "";
    } else if (result.permissionMode === "workspace-auto") {
      result.approvalMode = "never";
      result.sandboxMode = "workspace-write";
      result.permissionMode = "";
    } else if (result.permissionMode === "full-auto") {
      result.approvalMode = "never";
      result.sandboxMode = "danger-full-access";
      result.permissionMode = "";
    }
  }
  return result;
}

export function migrateAgentData(raw) {
  if (!raw || typeof raw !== "object" || Array.isArray(raw))
    throw new Error("Invalid vault contents.");
  if (
    Object.hasOwn(raw, "agentCatalog") ||
    Object.hasOwn(raw, "teamsVersion")
  ) {
    if (raw.teamsVersion !== TEAMS_SCHEMA_VERSION)
      throw new Error("Unsupported Teams schema version.");
    const agentCatalog = normalizeAgentCatalog(raw.agentCatalog);
    if (!Array.isArray(raw.teams) || raw.teams.length > MAX_TEAMS)
      throw new Error(`At most ${MAX_TEAMS} teams.`);
    const ids = new Set();
    const teams = raw.teams.map((entry) => {
      const team = normalizeReferencedTeam(entry, agentCatalog.definitions);
      if (ids.has(team.id)) throw new Error(`Duplicate team ID: ${team.id}.`);
      ids.add(team.id);
      return team;
    });
    return { agentCatalog, teamsVersion: TEAMS_SCHEMA_VERSION, teams };
  }

  const sourceTeams = Object.hasOwn(raw, "teams")
    ? raw.teams
    : (raw.launchProfiles || []).map((entry) => ({
        id: `legacy-${entry.name}`,
        name: entry.name,
        members: [{ ...entry, role: "", prompt: "" }],
      }));
  if (!Array.isArray(sourceTeams) || sourceTeams.length > MAX_TEAMS)
    throw new Error(`At most ${MAX_TEAMS} teams.`);
  const definitions = [];
  const teamIds = new Set();
  const teams = sourceTeams.map((team, teamIndex) => {
    if (
      !team ||
      typeof team.name !== "string" ||
      !team.name.trim() ||
      team.name.length > 80 ||
      /[\x00-\x1f\x7f]/.test(team.name)
    )
      throw new Error(
        `Team ${teamIndex + 1}: Enter a team name of up to 80 characters.`,
      );
    if (
      !Array.isArray(team.members) ||
      !team.members.length ||
      team.members.length > 32
    )
      throw new Error(`Team ${teamIndex + 1}: A team needs 1–32 agents.`);
    const id = ID_RE.test(team.id || "") ? team.id : newId("team");
    if (teamIds.has(id)) throw new Error(`Duplicate team ID: ${id}.`);
    teamIds.add(id);
    const names = new Set();
    const members = team.members.map((member, memberIndex) => {
      const launchName = String(member?.name || "").trim();
      if (!AGENT_NAME_RE.test(launchName))
        throw new Error(
          `Team ${teamIndex + 1}, agent ${memberIndex + 1}: Invalid launch name.`,
        );
      if (names.has(launchName.toLowerCase()))
        throw new Error(
          `Team ${teamIndex + 1}: Duplicate agent name ${launchName}.`,
        );
      names.add(launchName.toLowerCase());
      const definition = normalizeAgentDefinition(
        {
          id: newId("agent"),
          revision: 1,
          name: launchName,
          launchName,
          role: String(member.role || "").trim(),
          serverId: String(member.serverId || ""),
          runtime: String(member.runtime || "generic"),
          model: String(member.model || "").trim(),
          reasoning: String(member.reasoning || ""),
          ...legacyPermission(member),
          allowedTools: member.allowedTools || [],
          run: String(
            member.run ||
              (member.runtime !== "generic" ? member.runtime : "") ||
              "",
          ).trim(),
          cwd: String(member.cwd || "").trim(),
          // Prompts are opaque launch data. Preserve their exact legacy bytes;
          // trimming here would make an otherwise valid migration lossy.
          prompt: String(member.prompt || ""),
        },
        definitions.length,
      );
      definitions.push(definition);
      return { agentDefinitionId: definition.id };
    });
    const orchestrator = String(team.orchestrator || team.members[0].name);
    if (
      !names.has(orchestrator.toLowerCase()) ||
      !team.members.some(
        (entry) => String(entry.name || "").trim() === orchestrator,
      )
    )
      throw new Error(
        `Team ${teamIndex + 1}: Choose an exact main orchestrator.`,
      );
    return {
      id,
      name: team.name.trim(),
      swarm: team.swarm === true,
      orchestrator,
      members,
    };
  });
  return {
    agentCatalog: { version: AGENT_CATALOG_VERSION, definitions },
    teamsVersion: TEAMS_SCHEMA_VERSION,
    teams,
  };
}

export function definitionUsers(definitionId, teams) {
  return teams
    .filter((team) =>
      team.members.some((member) => member.agentDefinitionId === definitionId),
    )
    .map((team) => team.name);
}
