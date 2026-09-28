const labels = {
  working: "Working", hung_tool: "Hung tool", finished_silent: "Finished silently",
  crashed: "Crashed", looping: "Looping", idle: "Idle", unknown: "Unknown",
};

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
  return `${activityLabel(activity)}${tool}${wake}${evidence} · ${tokenSnapshot(activity.tokens)}`;
}
