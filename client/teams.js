import { AGENT_NAME_RE } from "./task-ref.js";
import { agentSpawnCommand } from "../shared/tmux-command.js";
export const MAX_TEAM_MEMBERS = 8;
export function normalizeTeam(value) {
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
    throw new Error("A team needs 1–8 agents.");
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
      run: String(
        m.run || (m.runtime !== "generic" ? m.runtime : "") || "",
      ).trim(),
      cwd: String(m.cwd || "").trim(),
      prompt: String(m.prompt || "").trim(),
    };
    if (!member.serverId || member.serverId.length > 80)
      invalid("Choose a server.", "serverId");
    if (member.role.length > 80 || /[\x00-\x1f\x7f]/.test(member.role))
      invalid("Keep roles under 80 characters.", "role");
    try {
      agentSpawnCommand({
        hub: "http://localhost",
        task: "tsk_0000000000000000",
        ...member,
        prompt: [member.role && "Role: " + member.role, member.prompt]
          .filter(Boolean)
          .join("\n\n"),
      });
    } catch (error) {
      const field = /model/i.test(error.message)
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
  return {
    id: /^[A-Za-z0-9_-]{1,80}$/.test(value.id || "")
      ? value.id
      : "team_" + crypto.randomUUID().replaceAll("-", ""),
    name: value.name.trim(),
    members,
  };
}
export function savedTeams(data) {
  const input = Array.isArray(data.teams)
    ? data.teams
    : (data.launchProfiles || []).map((p) => ({
        id: "legacy-" + p.name,
        name: p.name,
        members: [{ ...p, role: "", prompt: "" }],
      }));
  const result = [];
  for (const value of input.slice(0, 30)) {
    try {
      const team = normalizeTeam(value);
      if (!result.some((t) => t.id === team.id)) result.push(team);
    } catch {}
  }
  return result;
}
export function teamLaunches(team, servers) {
  return normalizeTeam(team).members.map((member) => {
    const server = servers.find((s) => s.id === member.serverId);
    if (!server)
      throw new Error(
        `Choose an available server for ${member.name} in Teams.`,
      );
    return {
      server,
      fields: {
        ...member,
        prompt: [member.role && "Role: " + member.role, member.prompt]
          .filter(Boolean)
          .join("\n\n"),
      },
    };
  });
}
