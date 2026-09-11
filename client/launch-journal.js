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

async function digest(value) {
  const result = await crypto.subtle.digest(
    "SHA-256",
    encoder.encode(JSON.stringify(value)),
  );
  return [...new Uint8Array(result)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
}

export const serverLaunchScope = (server) =>
  digest([
    server?.id || "",
    server?.host || "",
    server?.port || 0,
    server?.username || "",
    server?.mode || "",
    server?.tmuxPath || "",
    server?.fingerprint || "",
    server?.keyId || "",
    !!server?.hasPassword,
    server?.credentialRevision || 1,
    !!server?.tailnet,
  ]);

function normalizeCreation(value) {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new Error("Invalid project creation retry state.");
  const request = value.request;
  if (
    !request ||
    typeof request !== "object" ||
    Array.isArray(request) ||
    Object.keys(request).some(
      (key) =>
        ![
          "name",
          "goal",
          "allowAgentSpawn",
          "maxNewAgents",
          "swarm",
          "orchestrator",
        ].includes(key),
    ) ||
    typeof request.name !== "string" ||
    !request.name ||
    request.name.trim() !== request.name ||
    [...request.name].length > 120 ||
    /[\x00-\x1f\x7f]/.test(request.name) ||
    typeof request.goal !== "string" ||
    encoder.encode(request.goal).byteLength > 8192 ||
    typeof request.allowAgentSpawn !== "boolean" ||
    !Number.isInteger(request.maxNewAgents) ||
    request.maxNewAgents < 0 ||
    request.maxNewAgents > 32 ||
    typeof request.swarm !== "boolean" ||
    typeof request.orchestrator !== "string" ||
    (request.orchestrator &&
      !/^[A-Za-z0-9_-]{1,64}$/.test(request.orchestrator)) ||
    !["prepared", "uncertain", "confirmed"].includes(value.state) ||
    !Array.isArray(value.knownTaskIds) ||
    value.knownTaskIds.length > 200 ||
    value.knownTaskIds.some((id) => !/^tsk_[a-f0-9]{16}$/.test(id)) ||
    new Set(value.knownTaskIds).size !== value.knownTaskIds.length ||
    typeof value.preparedAt !== "string" ||
    (value.state !== "prepared" && typeof value.attemptedAt !== "string") ||
    (value.attemptedAt !== undefined && typeof value.attemptedAt !== "string")
  )
    throw new Error("Invalid project creation retry state.");
  return {
    state: value.state,
    request: structuredClone(request),
    knownTaskIds: [...value.knownTaskIds],
    preparedAt: value.preparedAt,
    ...(value.attemptedAt && { attemptedAt: value.attemptedAt }),
  };
}

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
      !["new-project", "add-team", "replace-lead"].includes(raw.kind) ||
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
    if (plan.kind === "new-project") {
      plan.creation = normalizeCreation(plan.creation);
      if (
        (plan.creation.state === "confirmed") !==
        /^tsk_[a-f0-9]{16}$/.test(plan.taskId || "")
      )
        throw new Error("Project creation state does not match its task ID.");
    } else if (plan.creation !== undefined) {
      throw new Error("Only new-project retries can contain creation state.");
    }
    if (plan.kind === "replace-lead") {
      const lead = plan.lead;
      if (
        !plan.taskId ||
        plan.members.length !== 1 ||
        !lead ||
        !Number.isSafeInteger(lead.expectedRevision) ||
        lead.expectedRevision < 0 ||
        typeof lead.expectedName !== "string" ||
        typeof lead.previousAgentId !== "string" ||
        typeof lead.previousRunId !== "string" ||
        !/^[A-Za-z0-9_-]{1,128}$/.test(lead.requestId || "")
      )
        throw new Error("Invalid lead replacement retry plan.");
    }
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
        (member.serverScope !== undefined &&
          !/^[a-f0-9]{64}$/.test(member.serverScope)) ||
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
      ({
        server,
        serverId,
        serverScope,
        fields,
        state,
        agent,
        folderAmendments,
      }) => ({
        serverId: server?.id || serverId,
        ...(serverScope && { serverScope }),
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

export async function restoreLaunchMembers(plan, servers) {
  return Promise.all(
    plan.members.map(async (member) => {
      const server = servers.find((entry) => entry.id === member.serverId);
      if (!server)
        throw new Error(
          `The saved machine for ${member.fields.name} is unavailable.`,
        );
      if (!member.serverScope)
        throw new Error(
          `The saved machine scope for ${member.fields.name} is unavailable. Recreate the frozen launch plan.`,
        );
      if ((await serverLaunchScope(server)) !== member.serverScope)
        throw new Error(
          `The saved machine profile for ${member.fields.name} changed. Restore the exact endpoint and credentials or discard the frozen plan.`,
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
    }),
  );
}
