const labels = {
  working: "Working", hung_tool: "Hung tool", finished_silent: "Finished silently",
  crashed: "Crashed", looping: "Looping", idle: "Idle", unknown: "Unknown",
  runtime_prompt: "Runtime prompt", stuck: "Stuck",
};

// Runtime prompt kinds and policy actions (docs/runtime-prompts.md).
const promptKinds = {
  codex_rate_limit_switch: "Codex rate-limit menu", codex_model_migration: "Codex model migration menu",
  codex_usage_limit: "Codex usage limit prompt", codex_trust: "Codex folder trust prompt",
  claude_permission: "Claude permission dialog", claude_selection: "Claude selection dialog",
  claude_trust: "Claude folder trust dialog", unknown: "Unrecognized runtime prompt",
};
const promptActions = {
  keep_current_never_show: "keep current model, never show again", keep_current: "keep current model",
  use_existing: "use existing model", escalate: "ask the owner", report: "report only",
};

export function runtimePromptDetail(prompt) {
  if (!prompt) return "";
  const kind = promptKinds[prompt.kind] || "Unrecognized runtime prompt";
  const action = promptActions[prompt.action] || prompt.action || "no action";
  return `${kind}: ${action} · ${prompt.outcome || "pending"}${prompt.reason ? ` (${prompt.reason})` : ""}`;
}

export function activityLabel(activity) {
  return activity ? (labels[activity.state] || "Unknown") : "Activity unavailable";
}

export function tokenSnapshot(tokens) {
  if (!tokens) return "Usage unavailable";
  return `Last transition snapshot: ${Number(tokens.total || 0).toLocaleString()} tokens`;
}

export function activityDetail(activity, now = Date.now()) {
  if (!activity) return "Activity unavailable";
  const age = Math.max(0, Math.floor((now - Date.parse(activity.observedAt || "")) / 1000));
  const evidence = Number.isFinite(age) ? ` · observed ${age}s ago` : "";
  const tool = activity.pendingTool ? ` · ${activity.pendingTool}` : "";
  const wake = activity.wake
    ? ` · Claude wake ${activity.wake.status}${activity.wake.messageSeqs?.length ? ` #${activity.wake.messageSeqs.join(", #")}` : ""}${activity.wake.reason ? `: ${activity.wake.reason}` : ""}`
    : "";
  const prompt = activity.prompt ? ` · ${runtimePromptDetail(activity.prompt)}` : "";
  const reason = activity.state === "stuck" && activity.reason ? `: ${activity.reason}` : "";
  return `${activityLabel(activity)}${reason}${prompt}${tool}${wake}${evidence} · ${tokenSnapshot(activity.tokens)}`;
}
