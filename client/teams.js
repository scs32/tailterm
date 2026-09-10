import { AGENT_NAME_RE } from "./task-ref.js";
import { agentSpawnCommand } from "../shared/tmux-command.js";
import {
  migrateAgentData,
  normalizeReferencedTeam,
  resolveTeamMember,
} from "./agents.js";
export const MAX_TEAM_MEMBERS = 32;
export function itemScopedAgentName(name, itemId) {
  if (!AGENT_NAME_RE.test(name) || !/^wi_[0-9a-f]{16}$/.test(itemId))
    throw new Error("Invalid item-scoped agent identity.");
  const suffix = itemId.slice(-8);
  if (name.length + suffix.length + 1 <= 64) return `${name}-${suffix}`;
  let hash = 2166136261;
  for (const char of name)
    hash = Math.imul(hash ^ char.codePointAt(0), 16777619) >>> 0;
  return `${name.slice(0, 46)}-${hash.toString(16).padStart(8, "0")}-${suffix}`;
}
function normalizeLegacyTeam(value) {
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
    value.members.length > MAX_TEAM_MEMBERS
  )
    throw new Error("A team needs 1–32 agents.");
  const names = new Set();
  const members = value.members.map((m, memberIndex) => {
    const invalid = (message, field) => {
      throw Object.assign(
        new Error(
          `Agent ${memberIndex + 1}${m.name ? " (" + m.name + ")" : ""}: ${message}`,
        ),
        { memberIndex, field },
      );
    };
    const name = String(m.name || "").trim();
    if (!AGENT_NAME_RE.test(name))
      invalid(
        "Use an agent name of 1–64 letters, numbers, dashes or underscores, without spaces.",
        "name",
      );
    if (names.has(name.toLowerCase()))
      invalid(
        `The name "${name}" is already used in this team. Each agent needs a unique name.`,
        "name",
      );
    names.add(name.toLowerCase());
    const member = {
      name,
      role: String(m.role || "").trim(),
      serverId: String(m.serverId || ""),
      runtime: String(m.runtime || "generic"),
      model: String(m.model || "").trim(),
      permissionMode: String(m.permissionMode || ""),
      allowedTools: Array.isArray(m.allowedTools)
        ? m.allowedTools
        : String(m.allowedTools || "")
            .split("\n")
            .map((s) => s.trim())
            .filter(Boolean),
      run: String(
        m.run || (m.runtime !== "generic" ? m.runtime : "") || "",
      ).trim(),
      cwd: String(m.cwd || "").trim(),
      prompt: String(m.prompt || "").trim(),
    };
    if (member.serverId.length > 80) invalid("Choose a server.", "serverId");
    if (member.role.length > 80 || /[\x00-\x1f\x7f]/.test(member.role))
      invalid("Keep roles under 80 characters.", "role");
    try {
      agentSpawnCommand({
        hub: "http://localhost",
        task: "tsk_0000000000000000",
        ...member,
        cwd:
          member.cwd ||
          (member.permissionMode === "workspace-auto"
            ? "/__launch_project__"
            : ""),
        prompt: [member.role && "Role: " + member.role, member.prompt]
          .filter(Boolean)
          .join("\n\n"),
      });
    } catch (error) {
      const field = /permission/i.test(error.message)
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
    return member;
  });
  const orchestrator = value.orchestrator || members[0].name;
  if (!members.some((m) => m.name === orchestrator))
    throw new Error("Choose a main orchestrator from this team.");
  return {
    orchestrator,
    id: /^[A-Za-z0-9_-]{1,80}$/.test(value.id || "")
      ? value.id
      : "team_" + crypto.randomUUID().replaceAll("-", ""),
    name: value.name.trim(),
    swarm: value.swarm === true,
    members,
  };
}
export function normalizeTeam(value, catalog) {
  const definitions = Array.isArray(catalog) ? catalog : catalog?.definitions;
  if (value?.members?.every((member) => member?.agentDefinitionId)) {
    if (!definitions)
      throw new Error("Choose an Agents catalog for this team.");
    return normalizeReferencedTeam(value, definitions);
  }
  return normalizeLegacyTeam(value);
}
export function savedTeams(data) {
  return migrateAgentData(data).teams;
}
export function resolveTeam(team, catalog) {
  const definitions = Array.isArray(catalog) ? catalog : catalog?.definitions;
  const normalized = normalizeTeam(team, definitions);
  return definitions
    ? {
        ...normalized,
        members: normalized.members.map((member) =>
          resolveTeamMember(member, definitions),
        ),
      }
    : normalized;
}
export function teamLaunches(
  team,
  servers,
  mainServerId = servers[0]?.id,
  projectFolders,
  itemRouting = null,
  catalog = null,
) {
  const definitions = Array.isArray(catalog) ? catalog : catalog?.definitions;
  const normalized = normalizeTeam(team, definitions);
  const resolvedMembers = definitions
    ? normalized.members.map((member) => resolveTeamMember(member, definitions))
    : normalized.members;
  const ordered = [...resolvedMembers].sort(
    (a, b) =>
      Number(b.name === normalized.orchestrator) -
      Number(a.name === normalized.orchestrator),
  );
  if (
    itemRouting &&
    (!/^tsk_[0-9a-f]{16}$/.test(itemRouting.workItemTaskId) ||
      !/^wi_[0-9a-f]{16}$/.test(itemRouting.workItemId) ||
      !Number.isSafeInteger(itemRouting.workItemRevision) ||
      itemRouting.workItemRevision < 1 ||
      !/^tsk_[0-9a-f]{16}$/.test(itemRouting.workOrderTaskId) ||
      !Number.isSafeInteger(itemRouting.workOrderMessageSeq) ||
      itemRouting.workOrderMessageSeq < 1 ||
      !itemRouting.workContextBundle)
  )
    throw new Error(
      "Choose a recorded work item and bounded work-order message.",
    );
  return ordered.map((member) => {
    const server = servers.find(
      (s) => s.id === (member.serverId || mainServerId),
    );
    if (!server)
      throw new Error(
        member.serverId
          ? `Choose an available server for ${member.name} in Teams.`
          : `Choose a main machine to launch ${member.name}.`,
      );
    const cwd = member.cwd || projectFolders?.[server.id] || "";
    if (projectFolders && !cwd)
      throw new Error(
        `Choose a project folder on ${server.name} for ${member.name}.`,
      );
    if (cwd)
      agentSpawnCommand({
        hub: "http://localhost",
        task: "tsk_0000000000000000",
        ...member,
        cwd,
      });
    return {
      server,
      fields: {
        name: itemRouting
          ? itemScopedAgentName(member.name, itemRouting.workItemId)
          : member.name,
        role: member.role,
        serverId: member.serverId,
        runtime: member.runtime,
        model: member.model,
        reasoning: member.reasoning || "",
        approvalMode: member.approvalMode || "",
        sandboxMode: member.sandboxMode || "",
        permissionMode: member.permissionMode || "",
        allowedTools: [...(member.allowedTools || [])],
        run: member.run,
        cwd,
        prompt: [member.role && "Role: " + member.role, member.prompt]
          .filter(Boolean)
          .join("\n\n"),
        ...(member.agentDefinitionId && {
          agentDefinitionId: member.agentDefinitionId,
          agentDefinitionRevision: member.agentDefinitionRevision,
        }),
        ...(itemRouting || {}),
      },
    };
  });
}
