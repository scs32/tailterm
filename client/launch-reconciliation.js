async function contextDigest(value) {
  // The browser hands the prepared JSON to tt as an argument. tt embeds that
  // RawMessage in its Go JSON request; encoding/json compacts it and escapes
  // HTML-sensitive runes before the hub hashes the admitted bytes.
  const serialized = typeof value === "string" ? value : JSON.stringify(value);
  const admitted = JSON.stringify(JSON.parse(serialized)).replace(
    /[<>&\u2028\u2029]/g,
    (character) =>
      ({
        "<": "\\u003c",
        ">": "\\u003e",
        "&": "\\u0026",
        "\u2028": "\\u2028",
        "\u2029": "\\u2029",
      })[character],
  );
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(admitted),
  );
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("");
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
  const expectedDigest = await contextDigest(fields.workContextBundle);
  if (
    binding.agentId !== fields.agentId ||
    binding.runId !== agent.runId ||
    binding.itemTaskId !== fields.workItemTaskId ||
    binding.itemId !== fields.workItemId ||
    binding.itemRevision !== fields.workItemRevision ||
    binding.workOrderMessage?.taskId !== fields.workOrderTaskId ||
    binding.workOrderMessage?.seq !== fields.workOrderMessageSeq ||
    (binding.replacesAgentId || "") !== (fields.replacesAgentId || "") ||
    binding.contextDigest !== expectedDigest
  )
    return "work-item/order/context binding";
  return "";
}

export const taskMatchesCreation = (task, request) =>
  task.status === "open" &&
  task.name === request.name &&
  task.goal === request.goal &&
  task.allowAgentSpawn === request.allowAgentSpawn &&
  task.maxNewAgents === request.maxNewAgents &&
  task.swarm === request.swarm &&
  (task.orchestrator || "") === request.orchestrator;

export function taskCreationMatches(tasks, creation) {
  const known = new Set(creation.knownTaskIds);
  return tasks.filter(
    (task) =>
      !known.has(task.id) && taskMatchesCreation(task, creation.request),
  );
}
