export const PROJECT_PAUSE_VERSION = 1;

export function supportsProjectPauseV1(capabilities) {
  const pause = capabilities?.projectPause;
  return (
    pause?.supported === true &&
    Array.isArray(pause.versions) &&
    pause.versions.includes(PROJECT_PAUSE_VERSION)
  );
}

export function projectPauseState(task, capabilities) {
  if (!supportsProjectPauseV1(capabilities)) return "legacy";
  return ["active", "cleanup_pending", "paused", "resuming"].includes(
    task?.pauseState,
  )
    ? task.pauseState
    : "unknown";
}

export function assertProjectLifecycle(task, expected) {
  if (task?.status !== "open" || task.pauseState !== expected)
    throw new Error(
      expected === "active"
        ? "The project is not in an authoritative active state. Refresh before pausing it."
        : "The project is not fully paused. Refresh before resuming it.",
    );
  if (
    !Number.isSafeInteger(task.lifecycleGeneration) ||
    task.lifecycleGeneration < 0
  )
    throw new Error(
      "The project lifecycle generation is missing. Update the hub before using Pause or Resume.",
    );
}

export function assertPauseEvidence(result, expectedLifecycleGeneration) {
  if (
    result?.version !== PROJECT_PAUSE_VERSION ||
    !["cleanup_pending", "paused"].includes(result.state) ||
    !Number.isSafeInteger(result.pauseGeneration) ||
    result.pauseGeneration < 1 ||
    !Number.isSafeInteger(result.lifecycleGeneration) ||
    result.lifecycleGeneration <= expectedLifecycleGeneration ||
    !Number.isSafeInteger(result.cleanupPending) ||
    result.cleanupPending < 0 ||
    !Array.isArray(result.targets) ||
    !result.targets.every(
      (target) =>
        /^agt_[a-f0-9]{16}$/.test(target?.agentId || "") &&
        /^run_[a-f0-9]{16}$/.test(target?.runId || ""),
    ) ||
    !/^[a-f0-9]{64}$/.test(result.retainedHandoffDigest || "") ||
    !result.receipt
  )
    throw new Error(
      "The hub returned incomplete pause evidence. The project state is unknown; refresh before any lifecycle action.",
    );
  return result;
}

export function assertPauseSnapshot(pause, task) {
  if (
    pause?.version !== PROJECT_PAUSE_VERSION ||
    !["cleanup_pending", "paused"].includes(pause.state) ||
    !Number.isSafeInteger(pause.pauseGeneration) ||
    pause.pauseGeneration < 1 ||
    pause.pauseGeneration !== task?.pauseGeneration ||
    pause.lifecycleGeneration !== task?.lifecycleGeneration ||
    !Array.isArray(pause.targets) ||
    !/^[a-f0-9]{64}$/.test(pause.retainedHandoffDigest || "") ||
    !pause.receipt
  )
    throw new Error(
      "The exact pause snapshot or receipt is unavailable. No lifecycle action was attempted.",
    );
  return pause;
}

export function assertResumeEvidence(result, resume) {
  const admission = result?.resumeAdmission;
  const receipt = result?.receipt;
  if (
    result?.version !== PROJECT_PAUSE_VERSION ||
    !["resuming", "active"].includes(result.state) ||
    result.pauseGeneration !== resume.expectedPauseGeneration ||
    result.lifecycleGeneration !== resume.expectedLifecycleGeneration + 1 ||
    result.retainedHandoffDigest !== resume.retainedHandoffDigest ||
    receipt?.operation !== "resume" ||
    receipt.requestId !== resume.requestId ||
    receipt.taskId !== result.taskId ||
    receipt.pauseGeneration !== result.pauseGeneration ||
    !/^ppr_[a-f0-9]{16}$/.test(receipt.id || "") ||
    admission?.receiptId !== receipt.id ||
    admission.selectedTeamId !== resume.selectedTeamId ||
    admission.orchestrator?.agentId !== resume.orchestratorAgentId ||
    admission.orchestrator?.runId !== resume.orchestratorRunId ||
    admission.orchestrator?.name !== resume.orchestratorName ||
    (result.state === "resuming" && admission.pending !== true) ||
    (result.state === "active" && admission.pending !== false)
  )
    throw new Error(
      "The hub returned incomplete resume admission evidence. No agent was launched.",
    );
  return result;
}

export function assertResumeSnapshot(pause, resume, task) {
  if (
    pause?.version !== PROJECT_PAUSE_VERSION ||
    pause.taskId !== task?.id ||
    pause.pauseGeneration !== resume.expectedPauseGeneration ||
    pause.retainedHandoffDigest !== resume.retainedHandoffDigest ||
    pause.lifecycleGeneration !== task.lifecycleGeneration ||
    !["paused", "resuming", "active"].includes(pause.state)
  )
    throw new Error(
      "The retained resume handoff changed. No saved identity was launched.",
    );
  if (pause.state === "paused") {
    if (resume.state === "confirmed" || pause.resumeAdmission)
      throw new Error(
        "The saved resume receipt is not present in the authoritative project state.",
      );
    return pause;
  }
  const admission = pause.resumeAdmission;
  const confirmed = resume.state === "confirmed";
  if (
    resume.state === "prepared" ||
    !admission ||
    !/^ppr_[a-f0-9]{16}$/.test(admission.receiptId || "") ||
    (confirmed && admission.receiptId !== resume.resumeReceiptId) ||
    admission.selectedTeamId !== resume.selectedTeamId ||
    admission.orchestrator?.agentId !== resume.orchestratorAgentId ||
    admission.orchestrator?.runId !== resume.orchestratorRunId ||
    admission.orchestrator?.name !== resume.orchestratorName ||
    (pause.state === "resuming" && admission.pending !== true) ||
    (pause.state === "active" && admission.pending !== false) ||
    pause.lifecycleGeneration !==
      (confirmed
        ? resume.confirmedLifecycleGeneration
        : resume.expectedLifecycleGeneration + 1)
  )
    throw new Error(
      "The saved resume admission no longer matches the exact fresh orchestrator.",
    );
  return pause;
}

export function pauseStateLabel(task, capabilities, agentLabel) {
  switch (projectPauseState(task, capabilities)) {
    case "cleanup_pending": {
      const pending = Number.isSafeInteger(task.pauseCleanupPending)
        ? task.pauseCleanupPending
        : "Unknown";
      return `Pause cleanup pending · ${pending}`;
    }
    case "paused":
      return "Paused · project preserved";
    case "resuming":
      return "Resume pending · fresh lead admission";
    case "unknown":
      return "Pause status unavailable";
    default:
      return agentLabel;
  }
}
