import { compactWorkContext } from "../shared/work-context.js";

async function contextDigests(value) {
  const admitted = compactWorkContext(value);
  // Pre-envelope-fix hosts used encoding/json's HTML-safe spelling. Retain
  // that exact historical codec for uncertain saved launches; never rewrite
  // the plan or the admitted binding to make a retry match.
  const legacy = admitted.replace(
    /[<>&\u2028\u2029]/g,
    (character) =>
      "\\u" + character.charCodeAt(0).toString(16).padStart(4, "0"),
  );
  return Promise.all(
    [admitted, legacy].map(async (text) => {
      const digest = await crypto.subtle.digest(
        "SHA-256",
        new TextEncoder().encode(text),
      );
      return [...new Uint8Array(digest)]
        .map((byte) => byte.toString(16).padStart(2, "0"))
        .join("");
    }),
  );
}

export async function reconciledAgentProblem(taskId, entry, member, agent) {
  const { fields } = entry;
  if (agent.taskId !== taskId) return "project";
  if (agent.id !== fields.agentId) return "agent identity";
  if (agent.name !== fields.name) return "agent name";
  if (member.agent?.runId && agent.runId !== member.agent.runId)
    return "agent run";
  if (["closed", "exited"].includes(agent.status)) return "lifecycle";
  if ((agent.role || "") !== (fields.agentRole || "")) return "agent role";
  if ((agent.parentAgentId || "") !== "") return "parent identity";
  if ((agent.runtime || "") !== (fields.runtime || "")) return "runtime";
  if ((agent.cwd || "") !== (fields.cwd || "")) return "working directory";
  if (!fields.workItemId)
    return agent.workItem ? "unexpected work-item binding" : "";
  const binding = agent.workItem;
  if (!binding) return "missing work-item binding";
  const expectedDigests = await contextDigests(fields.workContextBundle);
  if (
    binding.agentId !== fields.agentId ||
    binding.runId !== agent.runId ||
    binding.itemTaskId !== fields.workItemTaskId ||
    binding.itemId !== fields.workItemId ||
    binding.itemRevision !== fields.workItemRevision ||
    binding.workOrderMessage?.taskId !== fields.workOrderTaskId ||
    binding.workOrderMessage?.seq !== fields.workOrderMessageSeq ||
    (binding.replacesAgentId || "") !== (fields.replacesAgentId || "") ||
    !expectedDigests.includes(binding.contextDigest)
  )
    return "work-item/order/context binding";
  return "";
}

export async function guardedLaunchEffect(guard, effect) {
  await guard();
  try {
    return await effect();
  } finally {
    await guard();
  }
}
