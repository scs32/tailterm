// Task and agent identifiers shared by the vault, workspace, and task views.
export const TASK_ID_RE = /^tsk_[0-9a-f]{16}$/;
export const AGENT_ID_RE = /^agt_[0-9a-f]{16}$/;
export const AGENT_NAME_RE = /^[a-zA-Z0-9_-]{1,64}$/;
export const normalizeTaskId = (value) =>
  typeof value === "string" && TASK_ID_RE.test(value) ? value : undefined;
// A pane's task binding: {taskId, agentId, agentName}.
export function normalizeTaskRef(value) {
  if (!value || typeof value !== "object") return null;
  const taskId = String(value.taskId || ""),
    agentId = String(value.agentId || ""),
    agentName = String(value.agentName || "").slice(0, 64);
  if (!TASK_ID_RE.test(taskId) || !AGENT_ID_RE.test(agentId)) return null;
  return {
    taskId,
    agentId,
    agentName: AGENT_NAME_RE.test(agentName) ? agentName : "",
  };
}
export function normalizeRuntimes(value) {
  if (!Array.isArray(value)) return [];
  return [
    ...new Set(
      value
        .filter(
          (r) => typeof r === "string" && /^[a-zA-Z0-9._-]{1,64}$/.test(r),
        )
        .slice(0, 32),
    ),
  ];
}
