// Explicit launch choices; an empty mode always preserves the host configuration.
export const PERMISSION_MODES = {
  codex: [
    ["", "Host settings"],
    ["on-request", "Ask when needed"],
    ["workspace-auto", "Workspace · no approval prompts"],
    ["full-auto", "Full machine · no approval prompts"],
  ],
  claude: [
    ["", "Host settings"],
    ["acceptEdits", "Accept edits · ask for other actions"],
    ["auto", "Automatic review · may still ask"],
    ["dontAsk", "Preapproved tools · deny instead of asking"],
    ["bypassPermissions", "Bypass permission checks"],
  ],
};
export function validateAgentPermissions(
  runtime,
  mode = "",
  allowedTools = [],
  cwd = "",
  approvalMode = "",
  sandboxMode = "",
) {
  if (
    typeof mode !== "string" ||
    (mode && !PERMISSION_MODES[runtime]?.some(([id]) => id === mode))
  )
    throw new Error("Choose a supported permission mode for this agent app.");
  if (
    typeof approvalMode !== "string" ||
    typeof sandboxMode !== "string" ||
    (approvalMode &&
      (runtime !== "codex" ||
        !["on-request", "never"].includes(approvalMode))) ||
    (sandboxMode &&
      (runtime !== "codex" ||
        !["read-only", "workspace-write", "danger-full-access"].includes(
          sandboxMode,
        )))
  )
    throw new Error(
      "Choose supported approval and sandbox settings for Codex.",
    );
  if (runtime === "codex" && mode && (approvalMode || sandboxMode))
    throw new Error(
      "Use independent approval and sandbox settings, or one legacy permission preset, not both.",
    );
  if (runtime !== "codex" && (approvalMode || sandboxMode))
    throw new Error(
      "Approval and sandbox overrides are currently supported for Codex only.",
    );
  if (
    (mode === "workspace-auto" || sandboxMode === "workspace-write") &&
    !cwd.startsWith("/")
  )
    throw new Error(
      "Workspace permissions require an explicit absolute working directory.",
    );
  if (
    !Array.isArray(allowedTools) ||
    allowedTools.length > 30 ||
    allowedTools.some(
      (t) =>
        typeof t !== "string" ||
        !t.trim() ||
        t.length > 200 ||
        /[\x00-\x1f\x7f]/.test(t),
    )
  )
    throw new Error(
      "Allowed tools must be up to 30 rules, one per line, under 200 characters each.",
    );
  if (allowedTools.length && runtime !== "claude")
    throw new Error(
      "Allowed tool rules are currently supported for Claude Code only.",
    );
}
