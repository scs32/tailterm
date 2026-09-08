export function validateSession(name) {
  if (!/^[a-zA-Z0-9_-]{1,64}$/.test(name))
    throw new Error(
      "Use 1–64 letters, numbers, underscores or dashes for a session name.",
    );
}
export function validateTmuxPath(path = "") {
  if (
    typeof path !== "string" ||
    path.length > 512 ||
    /[\x00-\x1f\x7f]/.test(path) ||
    (path && !path.startsWith("/"))
  )
    throw new Error("The tmux executable must be an absolute path.");
}
export const shellQuote = (value) => "'" + value.replace(/'/g, "'\\''") + "'";
function resolver(path) {
  validateTmuxPath(path);
  if (path)
    return `tailterm_tmux_bin=${shellQuote(path)}; if [ ! -f "$tailterm_tmux_bin" ] || [ ! -x "$tailterm_tmux_bin" ]; then printf 'Configured tmux executable is not executable: %s\\n' "$tailterm_tmux_bin" >&2; exit 127; fi; `;
  return `tailterm_tmux_bin=$(command -v tmux 2>/dev/null || :); if [ ! -f "$tailterm_tmux_bin" ] || [ ! -x "$tailterm_tmux_bin" ]; then for tailterm_tmux_candidate in /opt/homebrew/bin/tmux /usr/local/bin/tmux /usr/bin/tmux /bin/tmux /opt/local/bin/tmux /run/current-system/sw/bin/tmux /home/linuxbrew/.linuxbrew/bin/tmux "$HOME/.nix-profile/bin/tmux" "$HOME/.local/bin/tmux"; do if [ -f "$tailterm_tmux_candidate" ] && [ -x "$tailterm_tmux_candidate" ]; then tailterm_tmux_bin=$tailterm_tmux_candidate; break; fi; done; fi; if [ ! -f "$tailterm_tmux_bin" ] || [ ! -x "$tailterm_tmux_bin" ]; then printf 'Tailterm could not locate tmux in this SSH environment. It may be installed outside PATH. Set its absolute executable path in Edit server.\\nSSH PATH: %s\\n' "$PATH" >&2; exit 127; fi; `;
}
export function validateTarget(target) {
  if (
    !target ||
    !/^\$[0-9]+$/.test(target.id) ||
    !/^[0-9]+$/.test(String(target.created))
  )
    throw new Error("Invalid tmux session identity.");
}
function exactTarget(target) {
  validateTarget(target);
  return `tailterm_tmux_target=${shellQuote(target.id)}; if [ "$("$tailterm_tmux_bin" display-message -p -t "$tailterm_tmux_target" '#{session_id}|#{session_created}' 2>/dev/null)" != ${shellQuote(target.id + "|" + target.created)} ]; then printf 'The original tmux session no longer exists. Choose a session from the launcher.\\n' >&2; exit 1; fi; `;
}
export function validateStartDirectory(cwd) {
  if (
    typeof cwd !== "string" ||
    cwd.length > 1024 ||
    (cwd && (!cwd.startsWith("/") || /[\u0000-\u001f\u007f]/.test(cwd)))
  )
    throw new Error("Start directory must be an absolute path.");
}
export function tmuxCommand(
  name,
  path = "",
  resumeOnly = false,
  target,
  cwd = "",
) {
  validateSession(name);
  validateStartDirectory(cwd);
  const start = cwd && !resumeOnly ? " -c " + shellQuote(cwd) : "";
  return (
    "/bin/sh -c " +
    shellQuote(
      resolver(path) +
        clipboardFeatures() +
        (resumeOnly
          ? target
            ? exactTarget(target)
            : exactSession(name)
          : "") +
        // Browser terminals always support UTF-8, even when SSH has no locale.
        `"$tailterm_tmux_bin" -u $tailterm_tmux_features ${resumeOnly ? 'attach-session -t "$tailterm_tmux_target"' : "new-session -A -s " + shellQuote(name) + start} \\; if-shell -F '#{==:#{set-clipboard},off}' 'set-option -s set-clipboard external' \\; set-option mouse on; tailterm_tmux_status=$?; if [ "$tailterm_tmux_status" -ne 0 ]; then printf 'tmux failed with exit status %s; see its error above.\\n' "$tailterm_tmux_status" >&2; fi; exit "$tailterm_tmux_status"`,
    )
  );
}
function clipboardFeatures() {
  // -T is available in tmux 3.2+. Older tmux uses the terminal's Ms capability.
  // Do not overwrite an existing 'on' clipboard policy or modify .tmux.conf.
  return `tailterm_tmux_features='-T clipboard'; case "$("$tailterm_tmux_bin" -V 2>/dev/null)" in 'tmux 0.'*|'tmux 1.'*|'tmux 2.'*|'tmux 3.0'*|'tmux 3.1'|'tmux 3.1a'|'tmux 3.1b'|'tmux 3.1c') tailterm_tmux_features='' ;; esac; `;
}
export function tmuxListCommand(path = "") {
  return (
    "/bin/sh -c " +
    shellQuote(
      resolver(path) +
        `exec "$tailterm_tmux_bin" list-sessions -F '#{session_name}|#{session_windows}|#{session_attached}|#{session_id}|#{session_created}'`,
    )
  );
}

export function tmuxRenameCommand(name, nextName, path = "", target) {
  validateSession(name);
  validateSession(nextName);
  return (
    "/bin/sh -c " +
    shellQuote(
      resolver(path) +
        (target ? exactTarget(target) : exactSession(name)) +
        `exec "$tailterm_tmux_bin" rename-session -t "$tailterm_tmux_target" -- ${shellQuote(nextName)}`,
    )
  );
}
export function tmuxDirectoryCommand(target, path = "") {
  return (
    "/bin/sh -c " +
    shellQuote(
      resolver(path) +
        exactTarget(target) +
        `"$tailterm_tmux_bin" display-message -p -t "$tailterm_tmux_target" '#{pane_current_path}' | base64`,
    )
  );
}
export function tmuxHistoryCommand(target, path = "") {
  return (
    "/bin/sh -c " +
    shellQuote(
      resolver(path) +
        exactTarget(target) +
        `tailterm_history_pane=$("$tailterm_tmux_bin" display-message -p -t "$tailterm_tmux_target" '#{pane_id}') || exit; exec "$tailterm_tmux_bin" capture-pane -p -e -J -S -5000 -t "$tailterm_history_pane"`,
    )
  );
}

function exactSession(name) {
  return `tailterm_tmux_target=$("$tailterm_tmux_bin" list-sessions -F '#{session_name}|#{session_id}' | while IFS='|' read -r tailterm_tmux_name tailterm_tmux_id; do if [ "$tailterm_tmux_name" = ${shellQuote(name)} ]; then printf '%s' "$tailterm_tmux_id"; break; fi; done); if [ -z "$tailterm_tmux_target" ]; then printf 'That tmux session no longer exists. Start a new session from the launcher.\\n' >&2; exit 1; fi; `;
}

const TT_CANDIDATES = [
  "/usr/local/bin/tt",
  "/opt/homebrew/bin/tt",
  "$HOME/.local/bin/tt",
  "$HOME/bin/tt",
  "/usr/bin/tt",
];
const ttResolver = (onMissing) =>
  `tailterm_tt=$(command -v tt 2>/dev/null || :); if [ ! -x "$tailterm_tt" ]; then for tailterm_tt_candidate in ${TT_CANDIDATES.map(
    (c) => (c.startsWith("$HOME") ? `"${c}"` : c),
  ).join(
    " ",
  )}; do if [ -x "$tailterm_tt_candidate" ]; then tailterm_tt=$tailterm_tt_candidate; break; fi; done; fi; if [ ! -x "$tailterm_tt" ]; then ${onMissing}; fi; `;
// Control characters (including escape) are refused so the approval dialog
// and the remote shell see exactly the text the user typed.
export function validateAgentText(value, max, label) {
  if (
    typeof value !== "string" ||
    value.length > max ||
    /[\u0000-\u001f\u007f]/.test(value)
  )
    throw new Error(`Invalid ${label}.`);
}
// Agent sessions are created host-side by the tt CLI, which registers the
// agent with the hub and starts tmux with the task environment. Tailterm runs
// this over a non-interactive SSH exec and attaches once the hub reports it.
export function agentSpawnCommand({
  hub,
  task,
  name,
  run,
  cwd = "",
  prompt = "",
  runtime = "",
  model = "",
}) {
  if (!/^https?:\/\/[A-Za-z0-9][A-Za-z0-9.:/_-]{0,199}$/.test(hub))
    throw new Error("Invalid hub URL.");
  if (!/^tsk_[0-9a-f]{16}$/.test(task)) throw new Error("Invalid task id.");
  validateSession(name);
  validateAgentText(run, 1024, "agent command");
  if (!run.trim()) throw new Error("Agent command is required.");
  validateAgentText(cwd, 512, "working directory");
  if (cwd && !cwd.startsWith("/"))
    throw new Error("Working directory must be absolute.");
  if (
    typeof prompt !== "string" ||
    prompt.length > 8192 ||
    /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/.test(prompt)
  )
    throw new Error("Invalid prompt.");
  if (runtime && !/^[a-zA-Z0-9._-]{1,64}$/.test(runtime))
    throw new Error("Invalid runtime.");
  if (
    model &&
    (!/^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,199}$/.test(model) ||
      !["claude", "codex", "aider", "gemini"].includes(runtime))
  )
    throw new Error("Enter a valid model name for a supported agent app.");
  const args = [
    "spawn",
    "--json",
    "--hub",
    hub,
    "--task",
    task,
    "--name",
    name,
    "--run",
    run,
  ];
  if (cwd) args.push("--cwd", cwd);
  if (prompt) args.push("--prompt", prompt);
  if (runtime) args.push("--runtime", runtime);
  if (model) args.push("--model", model);
  return (
    "/bin/sh -c " +
    shellQuote(
      ttResolver(
        `printf 'The tt agent CLI is not installed on this server. Install it from the tailterm hub build and retry.\\n' >&2; exit 127`,
      ) + `exec "$tailterm_tt" ${args.map(shellQuote).join(" ")}`,
    )
  );
}
export function agentRuntimesCommand() {
  return (
    "/bin/sh -c " +
    shellQuote(
      ttResolver(`printf '{"missing":true}'; exit 0`) +
        `exec "$tailterm_tt" runtimes --json`,
    )
  );
}
