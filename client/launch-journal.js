export const MAX_TEAM_LAUNCH_PLANS = 8;
export const MAX_TEAM_LAUNCH_PLAN_BYTES = 512 * 1024;
export const MAX_TEAM_LAUNCH_PLANS_BYTES = 2 * 1024 * 1024;
const encoder = new TextEncoder();
const FIELD_KEYS = new Set([
  "name",
  "role",
  "serverId",
  "runtime",
  "model",
  "reasoning",
  "approvalMode",
  "sandboxMode",
  "permissionMode",
  "allowedTools",
  "run",
  "cwd",
  "prompt",
  "agentDefinitionId",
  "agentDefinitionRevision",
  "agentRole",
  "agentId",
  "expectedRunId",
  "plannedTeamMembers",
  "workItemTaskId",
  "workItemId",
  "workItemRevision",
  "workOrderTaskId",
  "workOrderMessageSeq",
  "replacesAgentId",
  "folderAmendments",
]);

const bytes = (value) => encoder.encode(JSON.stringify(value)).byteLength;

export function normalizeTeamLaunchPlans(value) {
  if (value === undefined) return [];
  if (!Array.isArray(value) || value.length > MAX_TEAM_LAUNCH_PLANS)
    throw new Error(
      `At most ${MAX_TEAM_LAUNCH_PLANS} unresolved team launch plans.`,
    );
  const ids = new Set();
  const plans = value.map((raw) => {
    if (
      !raw ||
      typeof raw !== "object" ||
      !/^[A-Za-z0-9_-]{1,128}$/.test(raw.id || "") ||
      !["new-project", "add-team"].includes(raw.kind) ||
      typeof raw.scope !== "string" ||
      !/^[a-f0-9]{64}$/.test(raw.scope) ||
      typeof raw.createdAt !== "string" ||
      typeof raw.updatedAt !== "string" ||
      !Array.isArray(raw.members) ||
      !raw.members.length ||
      raw.members.length > 32 ||
      (raw.taskId && !/^tsk_[a-f0-9]{16}$/.test(raw.taskId)) ||
      (raw.teamId && !/^[A-Za-z0-9_-]{1,80}$/.test(raw.teamId)) ||
      (raw.workContextBundle !== undefined &&
        (typeof raw.workContextBundle !== "string" ||
          encoder.encode(raw.workContextBundle).byteLength > 131072))
    )
      throw new Error("Invalid team launch retry plan.");
    if (ids.has(raw.id)) throw new Error("Duplicate team launch retry plan.");
    ids.add(raw.id);
    const plan = structuredClone(raw);
    if (plan.workContextBundle !== undefined) {
      try {
        JSON.parse(plan.workContextBundle);
      } catch {
        throw new Error("Invalid prepared work-item context in retry plan.");
      }
    }
    for (const member of plan.members) {
      if (
        !member ||
        typeof member !== "object" ||
        !/^[A-Za-z0-9_-]{1,80}$/.test(member.serverId || "") ||
        !["unstarted", "uncertain", "started"].includes(member.state) ||
        !member.fields ||
        typeof member.fields !== "object" ||
        Object.keys(member.fields).some((key) => !FIELD_KEYS.has(key)) ||
        !/^[A-Za-z0-9_-]{1,64}$/.test(member.fields.name || "") ||
        !/^agt_[a-f0-9]{16}$/.test(member.fields.agentId || "") ||
        typeof member.fields.run !== "string" ||
        !member.fields.run ||
        member.fields.run.length > 1024 ||
        typeof member.fields.cwd !== "string" ||
        member.fields.cwd.length > 512 ||
        (member.fields.cwd && !member.fields.cwd.startsWith("/")) ||
        (member.fields.agentDefinitionId &&
          !/^[A-Za-z0-9_-]{1,80}$/.test(member.fields.agentDefinitionId)) ||
        (member.fields.agentDefinitionRevision !== undefined &&
          (!Number.isSafeInteger(member.fields.agentDefinitionRevision) ||
            member.fields.agentDefinitionRevision < 1)) ||
        (member.fields.workItemId &&
          (!/^tsk_[a-f0-9]{16}$/.test(member.fields.workItemTaskId || "") ||
            !/^wi_[a-f0-9]{16}$/.test(member.fields.workItemId) ||
            !Number.isSafeInteger(member.fields.workItemRevision) ||
            member.fields.workItemRevision < 1 ||
            !/^tsk_[a-f0-9]{16}$/.test(member.fields.workOrderTaskId || "") ||
            !Number.isSafeInteger(member.fields.workOrderMessageSeq) ||
            member.fields.workOrderMessageSeq < 1 ||
            plan.workContextBundle === undefined)) ||
        (member.state === "started" &&
          (!/^agt_[a-f0-9]{16}$/.test(member.agent?.id || "") ||
            !/^run_[a-f0-9]{16}$/.test(member.agent?.runId || "") ||
            member.agent.id !== member.fields.agentId))
      )
        throw new Error("Invalid team launch retry member.");
    }
    if (bytes(plan) > MAX_TEAM_LAUNCH_PLAN_BYTES)
      throw new Error("A team launch retry plan exceeds 512 KiB.");
    return plan;
  });
  if (bytes(plans) > MAX_TEAM_LAUNCH_PLANS_BYTES)
    throw new Error("Team launch retry storage exceeds 2 MiB.");
  return plans;
}

export function launchPlanForStorage(plan) {
  const contexts = new Set(
    plan.members
      .map((member) => member.fields.workContextBundle)
      .filter((value) => value !== undefined)
      .map((value) =>
        typeof value === "string" ? value : JSON.stringify(value),
      ),
  );
  if (contexts.size > 1)
    throw new Error(
      "A retry plan must use one exact shared work-item context.",
    );
  const context = [...contexts][0];
  const stored = {
    ...structuredClone(plan),
    ...(context !== undefined && {
      workContextBundle:
        typeof context === "string" ? context : JSON.stringify(context),
    }),
    members: plan.members.map(
      ({ server, serverId, fields, state, agent, folderAmendments }) => ({
        serverId: server?.id || serverId,
        state,
        ...(agent && { agent: structuredClone(agent) }),
        ...(folderAmendments && {
          folderAmendments: structuredClone(folderAmendments),
        }),
        fields: Object.fromEntries(
          Object.entries(structuredClone(fields)).filter(([key]) =>
            FIELD_KEYS.has(key),
          ),
        ),
      }),
    ),
  };
  return normalizeTeamLaunchPlans([stored])[0];
}

export function restoreLaunchMembers(plan, servers) {
  return plan.members.map((member) => {
    const server = servers.find((entry) => entry.id === member.serverId);
    if (!server)
      throw new Error(
        `The saved machine for ${member.fields.name} is unavailable.`,
      );
    return {
      server,
      fields: {
        ...structuredClone(member.fields),
        ...(plan.workContextBundle && {
          workContextBundle: plan.workContextBundle,
        }),
      },
      state: member.state || "unstarted",
      ...(member.agent && { agent: structuredClone(member.agent) }),
      ...(member.folderAmendments && {
        folderAmendments: structuredClone(member.folderAmendments),
      }),
    };
  });
}
