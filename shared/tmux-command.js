import { serializedWorkContext } from "./work-context.js";
import { validateAgentPermissions } from "./agent-permissions.js";
import { validateReasoning } from "../client/reasoning.js";
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
// A cut reading (see the note above identityHolds) loses its last field, so a
// reading is whole when every field has its strict shape. A cut one decides
// nothing and is read again. A missing session reads the same as a cut one,
// so has-session, which expands no format, tells them apart.
// digitsAfter tests that $tailterm_tmux_field is prefix plus digits. Its
// patterns open with "(" because bash 3.2 misreads a bare ")" inside $( ).
const digitsAfter = (prefix) =>
  `case "$tailterm_tmux_field" in (${prefix || "''"}|${prefix}*[!0-9]*) false ;; ${prefix ? `(${prefix}*) : ;; (*) false ;; ` : ""}esac`;
// Tests that $variable is "<prefix><digits>|<digits>", as an id and a time.
const bothWhole = (variable, prefix) =>
  `{ tailterm_tmux_field=\${${variable}%%|*}; ${digitsAfter(prefix)} && tailterm_tmux_field=\${${variable}#*|} && ${digitsAfter("")}; }`;
function exactTarget(target) {
  validateTarget(target);
  return (
    `tailterm_tmux_target=${shellQuote(target.id)}; ` +
    rereadWhile(
      "tailterm_tmux_identity",
      "cut",
      `tailterm_tmux_identity=$("$tailterm_tmux_bin" display-message -p -t "$tailterm_tmux_target" '#{session_id}|#{session_created}' 2>/dev/null) && if ! ${bothWhole("tailterm_tmux_identity", "\\$")} && "$tailterm_tmux_bin" has-session -t "$tailterm_tmux_target" 2>/dev/null; then tailterm_tmux_identity=cut; fi`,
    ) +
    `if [ "$tailterm_tmux_identity" = cut ]; then ${UNREAD_IDENTITY}; fi; ` +
    `if [ "$tailterm_tmux_identity" != ${shellQuote(target.id + "|" + target.created)} ]; then printf 'The original tmux session no longer exists. Choose a session from the launcher.\\n' >&2; exit 1; fi; `
  );
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
  { ignoreSize = false, helperBinding } = {},
) {
  validateSession(name);
  validateStartDirectory(cwd);
  if (helperBinding && (!resumeOnly || !ignoreSize))
    throw new Error(
      "Helper sizing requires an ignore-size existing-session attach.",
    );
  const helperPolicy = helperBinding
    ? helperWindowPolicy(helperBinding, target)
    : "";
  const start = cwd && !resumeOnly ? " -c " + shellQuote(cwd) : "";
  // PTY size never implicitly controls an agent window. Foreground focus uses
  // the separately guarded sizing command; manual sizing protects hidden tiles.
  const attachFlags =
    ignoreSize && resumeOnly ? "$tailterm_tmux_attach_flags " : "";
  return (
    "/bin/sh -c " +
    shellQuote(
      resolver(path) +
        clipboardFeatures() +
        (attachFlags ? attachFlagsFeature() : "") +
        (resumeOnly
          ? target
            ? exactTarget(target)
            : exactSession(name)
          : "") +
        helperPolicy +
        // Browser terminals always support UTF-8, even when SSH has no locale.
        `"$tailterm_tmux_bin" -u $tailterm_tmux_features ${resumeOnly ? "attach-session " + attachFlags + '-t "$tailterm_tmux_target' + (helperPolicy ? ":$tailterm_tmux_window" : "") + '"' : "new-session -A -s " + shellQuote(name) + start} \\; if-shell -F '#{==:#{set-clipboard},off}' 'set-option -s set-clipboard external' \\; set-option mouse on; tailterm_tmux_status=$?; if [ "$tailterm_tmux_status" -ne 0 ]; then printf 'tmux failed with exit status %s; see its error above.\\n' "$tailterm_tmux_status" >&2; fi; exit "$tailterm_tmux_status"`,
    )
  );
}
// tmux 3.7 stops expanding a format after 100 ms and expands the rest to
// nothing, so on a loaded host a true identity guard reads false. A refusal is
// therefore proved separately: some field differs and the trailing literal,
// which only survives a whole expansion, is still there. Each guard is a list
// of [field, value, wanted] terms, where wanted false means "must differ".
const allOf = (conditions) => conditions.reduce((a, b) => `#{&&:${a},${b}}`);
// A comparison whose operands were cut compares nothing with nothing and reads
// true. The trailing literal is lost with them, so a proved condition is false.
const proved = (condition) => `#{&&:${condition},1}`;
const identityHolds = (terms) =>
  proved(
    allOf(
      terms.map(
        ([field, value, wanted = true]) =>
          `#{${wanted ? "==" : "!="}:#{${field}},${value}}`,
      ),
    ),
  );
const identityDiffers = (terms) =>
  `#{&&:${terms
    .map(
      ([field, value, wanted = true]) =>
        `#{${wanted ? "!=" : "=="}:#{${field}},${value}}`,
    )
    .reduce((a, b) => `#{||:${a},${b}}`)},1}`;
// A guard whose reading was cut changed nothing, so it is run again.
const IDENTITY_READS = 5;
const UNREAD_IDENTITY = `printf 'tmux did not finish reading the session identity. Try again.\\n' >&2; exit 75`;
const rereadWhile = (reply, word, read) =>
  `tailterm_identity_reads=0; while :; do ${read}; if [ "$${reply}" != ${word} ]; then break; fi; tailterm_identity_reads=$((tailterm_identity_reads + 1)); if [ "$tailterm_identity_reads" -ge ${IDENTITY_READS} ]; then break; fi; sleep 0.2 2>/dev/null || sleep 1; done; `;
// Helpers use native client precedence, not the ordinary-agent manual-size
// controller. Resolve the selected window once, then guard and mutate it in a
// synchronous tmux queue. A window linked to another session is unsupported.
const HELPER_UNCHECKED = `printf 'Helper sizing was not checked: tmux did not finish reading the window identity. Try again.\\n' >&2; exit 1`;
function helperWindowPolicy(binding, target) {
  if (
    binding?.role !== "owner_helper" ||
    !/^tsk_[0-9a-f]{16}$/.test(binding.taskId || "") ||
    !/^agt_[0-9a-f]{16}$/.test(binding.agentId || "") ||
    !/^run_[0-9a-f]{16}$/.test(binding.runId || "")
  )
    throw new Error("Invalid helper sizing identity.");
  const identity = [
    ["session_id", "$tailterm_tmux_target"],
    ["session_created", "$tailterm_tmux_created"],
    ["window_id", "$tailterm_tmux_window"],
    ["TAILTERM_TASK", binding.taskId],
    ["TAILTERM_AGENT", binding.agentId],
    ["TAILTERM_RUN", binding.runId],
    ["TAILTERM_ROLE", "owner_helper"],
    ["window_linked", 0],
  ];
  const window = "$tailterm_tmux_target:$tailterm_tmux_window";
  return (
    rereadWhile(
      "tailterm_tmux_window_identity",
      "cut",
      `tailterm_tmux_window_identity=$("$tailterm_tmux_bin" display-message -p -t "$tailterm_tmux_target" '#{window_id}|#{session_created}') || exit 1; if ! ${bothWhole("tailterm_tmux_window_identity", "@")}; then tailterm_tmux_window_identity=cut; fi`,
    ) +
    `if [ "$tailterm_tmux_window_identity" = cut ]; then ${HELPER_UNCHECKED}; fi; ` +
    `tailterm_tmux_window=\${tailterm_tmux_window_identity%%|*}; ` +
    `tailterm_tmux_created=${target ? shellQuote(String(target.created)) : '"${tailterm_tmux_window_identity#*|}"'}; ` +
    `case "$tailterm_tmux_window" in @*[!0-9]*|@|'') printf 'Invalid helper window identity.\\n' >&2; exit 1 ;; @*) ;; *) exit 1 ;; esac; ` +
    rereadWhile(
      "tailterm_helper_policy",
      "helper-incomplete",
      `tailterm_helper_policy=$("$tailterm_tmux_bin" if-shell -F -t "${window}" "${identityHolds(identity)}" "set-option -w -t '${window}' window-size latest ; display-message -p helper-ready" "if-shell -F -t '${window}' '${identityDiffers(identity)}' 'display-message -p helper-refused' 'display-message -p helper-incomplete'") || exit 1`,
    ) +
    `if [ "$tailterm_helper_policy" = helper-incomplete ]; then ${HELPER_UNCHECKED}; fi; ` +
    `if [ "$tailterm_helper_policy" != helper-ready ]; then printf 'Helper sizing refused: session identity changed or window is linked to another session.\\n' >&2; exit 1; fi; `
  );
}
function attachFlagsFeature() {
  // attach-session -f ignore-size needs tmux 3.2+; older tmux attaches plainly.
  return `tailterm_tmux_attach_flags='-f ignore-size'; case "$("$tailterm_tmux_bin" -V 2>/dev/null)" in 'tmux 0.'*|'tmux 1.'*|'tmux 2.'*|'tmux 3.0'*|'tmux 3.1'|'tmux 3.1a'|'tmux 3.1b'|'tmux 3.1c') tailterm_tmux_attach_flags='' ;; esac; `;
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
        // An empty pane target would capture whichever pane tmux picks.
        rereadWhile(
          "tailterm_history_pane",
          "cut",
          `tailterm_history_pane=$("$tailterm_tmux_bin" display-message -p -t "$tailterm_tmux_target" '#{pane_id}') || exit; case "$tailterm_history_pane" in %|%*[!0-9]*) tailterm_history_pane=cut ;; %*) ;; *) tailterm_history_pane=cut ;; esac`,
        ) +
        `if [ "$tailterm_history_pane" = cut ]; then printf 'tmux did not finish reading the pane. Try again.\\n' >&2; exit 75; fi; ` +
        `exec "$tailterm_tmux_bin" capture-pane -p -e -J -S -5000 -t "$tailterm_history_pane"`,
    )
  );
}

// Each if-shell -F branch contains only synchronous tmux commands. The server
// drains that command queue without yielding: identity/token check and mutation
// cannot interleave with another viewer's claim. No shell lock or input injection.
export function agentWindowSizeCommand(
  { target, binding, token, action, cols, rows, expectedRevision },
  path = "",
) {
  validateTarget(target);
  if (
    !/^tsk_[0-9a-f]{16}$/.test(binding?.taskId || "") ||
    !/^agt_[0-9a-f]{16}$/.test(binding?.agentId || "") ||
    !/^run_[0-9a-f]{16}$/.test(binding?.runId || "") ||
    binding?.role === "owner_helper" ||
    !/^[a-zA-Z0-9_-]{16,64}$/.test(token || "") ||
    !["inspect", "claim", "resize", "release", "restore"].includes(action)
  )
    throw new Error("Invalid agent sizing identity or action.");
  if (
    action === "claim" &&
    (typeof expectedRevision !== "string" ||
      (expectedRevision !== "" &&
        !/^[a-zA-Z0-9_-]{16,64}$/.test(expectedRevision)))
  )
    throw new Error("Invalid agent sizing revision.");
  if (
    !["inspect", "release"].includes(action) &&
    (![cols, rows].every(Number.isSafeInteger) ||
      cols < 1 ||
      rows < 1 ||
      cols > 10000 ||
      rows > 10000)
  )
    throw new Error("Invalid agent viewport size.");
  const window = shellQuote(target.id + ":agent");
  const eq = (field, value) => `#{==:#{${field}},${value}}`;
  const identity = [
    ["session_id", target.id],
    ["session_created", target.created],
    ["TAILTERM_TASK", binding.taskId],
    ["TAILTERM_AGENT", binding.agentId],
    ["TAILTERM_RUN", binding.runId],
    ["TAILTERM_ROLE", "owner_helper", false],
    ["window_name", "agent"],
    ["window_panes", 1],
  ];
  let mutate;
  if (action === "inspect") {
    // The reported size lets a claim that lands after its pane hid be undone.
    mutate = `display-message -p -t ${window} 'ready:#{@tailterm_size_revision}:#{window_width}x#{window_height}'`;
  } else if (action === "release") {
    mutate = `set-option -wu -t ${window} @tailterm_size_viewer ; display-message -p released`;
  } else if (action === "restore") {
    // cols and rows are the window size inspect reported, not a viewport.
    mutate = `set-option -w -t ${window} window-size manual ; resize-window -t ${window} -x ${Math.max(80, cols)} -y ${Math.max(24, rows)} ; set-option -wu -t ${window} @tailterm_size_viewer ; display-message -p restored`;
  } else {
    const size = (statusRows) =>
      `set-option -w -t ${window} window-size manual ; resize-window -t ${window} -x ${Math.max(80, cols)} -y ${Math.max(24, rows - statusRows)} ; ` +
      (action === "claim"
        ? `set-option -w -t ${window} @tailterm_size_revision ${shellQuote(token)} ; set-option -w -t ${window} @tailterm_size_viewer ${shellQuote(token)} ; `
        : "") +
      "display-message -p sized";
    // tmux's status occupies client rows outside the window. Keep at least 24
    // usable program rows, including when the viewport itself is tiny.
    mutate = [
      ["off", 0],
      ["on", 1],
      ["2", 2],
      ["3", 3],
      ["4", 4],
      ["5", 5],
    ]
      .map(
        ([value, height]) =>
          `if-shell -F -t ${window} ${shellQuote(proved(eq("status", value)))} ${shellQuote(size(height))}`,
      )
      .join(" ; ");
  }
  // Superseded is proved like a refusal; an unproved one is "incomplete".
  const whileCurrent = (field, value, then) =>
    `if-shell -F -t ${window} ${shellQuote(proved(eq(field, value)))} ${shellQuote(then)} ${shellQuote(`if-shell -F -t ${window} ${shellQuote(proved(`#{!=:#{${field}},${value}}`))} 'display-message -p superseded' 'display-message -p incomplete'`)}`;
  if (action === "claim")
    mutate = whileCurrent("@tailterm_size_revision", expectedRevision, mutate);
  if (["resize", "release", "restore"].includes(action))
    mutate = whileCurrent("@tailterm_size_viewer", token, mutate);
  // A refusal the server could not prove is "incomplete": nothing was changed.
  const refuse = `if-shell -F -t ${window} ${shellQuote(identityDiffers(identity))} 'display-message -p refused' 'display-message -p incomplete'`;
  return (
    "/bin/sh -c " +
    shellQuote(
      resolver(path) +
        rereadWhile(
          "tailterm_size_reply",
          "incomplete",
          // No reply means no branch ran, and a ready line is whole only with
          // its last field, the height.
          `tailterm_size_reply=$("$tailterm_tmux_bin" if-shell -F -t ${window} ${shellQuote(identityHolds(identity))} ${shellQuote(mutate)} ${shellQuote(refuse)}); tailterm_size_status=$?; if [ "$tailterm_size_status" -eq 0 ]; then case "$tailterm_size_reply" in ${action === "inspect" ? "ready:*:*[0-9]x[0-9]*) ;; ''|ready:*" : "''"}) tailterm_size_reply=incomplete ;; esac; fi`,
        ) +
        `if [ -n "$tailterm_size_reply" ]; then printf '%s\\n' "$tailterm_size_reply"; fi; ` +
        `if [ "$tailterm_size_reply" = incomplete ]; then printf 'tmux did not finish reading the agent pane identity; nothing was sized.\\n' >&2; exit 75; fi; ` +
        `exit "$tailterm_size_status"`,
    )
  );
}

// A whole row ends with its session id, whatever its name holds. When the
// name is on no whole row and some row was cut, the session may be the cut
// one, so the list is read again.
function exactSession(name) {
  return (
    rereadWhile(
      "tailterm_tmux_target",
      "cut",
      `tailterm_tmux_target=$("$tailterm_tmux_bin" list-sessions -F '#{session_name}|#{session_id}' | { tailterm_tmux_cut=; while IFS= read -r tailterm_tmux_row; do tailterm_tmux_name=\${tailterm_tmux_row%|*}; tailterm_tmux_field=\${tailterm_tmux_row##*|}; if ${digitsAfter("\\$")}; then if [ "$tailterm_tmux_name" = ${shellQuote(name)} ]; then printf '%s' "$tailterm_tmux_field"; exit 0; fi; else tailterm_tmux_cut=cut; fi; done; printf '%s' "$tailterm_tmux_cut"; })`,
    ) +
    `if [ "$tailterm_tmux_target" = cut ]; then ${UNREAD_IDENTITY}; fi; ` +
    `if [ -z "$tailterm_tmux_target" ]; then printf 'That tmux session no longer exists. Start a new session from the launcher.\\n' >&2; exit 1; fi; `
  );
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
  reasoning = "",
  permissionMode = "",
  approvalMode = "",
  sandboxMode = "",
  allowedTools = [],
  agentRole = "",
  agentId = "",
  expectedRunId = "",
  expectedLifecycleGeneration = 0,
  resumeReceiptId = "",
  plannedTeamMembers = 0,
  workItemTaskId = "",
  workItemId = "",
  workItemRevision = 0,
  workOrderTaskId = "",
  workOrderMessageSeq = 0,
  replacesAgentId = "",
  workContextBundle = null,
  workContextFile = "",
  workContextDigest = "",
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
  validateReasoning(runtime, model, reasoning, run);
  validateAgentPermissions(
    runtime,
    permissionMode,
    allowedTools,
    cwd,
    approvalMode,
    sandboxMode,
  );
  if (agentRole && agentRole !== "database_handler")
    throw new Error("Invalid agent role.");
  if (agentId && !/^agt_[0-9a-f]{16}$/.test(agentId))
    throw new Error("Invalid agent identity.");
  if (expectedRunId && !/^run_[0-9a-f]{16}$/.test(expectedRunId))
    throw new Error("Invalid run identity.");
  if (
    !Number.isSafeInteger(expectedLifecycleGeneration) ||
    expectedLifecycleGeneration < 0
  )
    throw new Error("Invalid project lifecycle generation.");
  if (resumeReceiptId && !/^ppr_[0-9a-f]{16}$/.test(resumeReceiptId))
    throw new Error("Invalid project resume receipt.");
  if (
    resumeReceiptId &&
    (!agentId || !expectedRunId || expectedLifecycleGeneration < 1)
  )
    throw new Error(
      "A project resume receipt requires an exact agent, run and lifecycle generation.",
    );
  if (
    plannedTeamMembers !== 0 &&
    (!Number.isInteger(plannedTeamMembers) ||
      plannedTeamMembers < 1 ||
      plannedTeamMembers > 32)
  )
    throw new Error("Invalid planned team member count.");
  const hasWorkItemRouting =
    workItemTaskId ||
    workItemId ||
    workItemRevision ||
    workOrderTaskId ||
    workOrderMessageSeq ||
    replacesAgentId;
  if (
    hasWorkItemRouting &&
    (!/^tsk_[0-9a-f]{16}$/.test(workItemTaskId) ||
      !/^wi_[0-9a-f]{16}$/.test(workItemId) ||
      !Number.isSafeInteger(workItemRevision) ||
      workItemRevision < 1 ||
      !/^tsk_[0-9a-f]{16}$/.test(workOrderTaskId) ||
      !Number.isSafeInteger(workOrderMessageSeq) ||
      workOrderMessageSeq < 1 ||
      (replacesAgentId && !/^agt_[0-9a-f]{16}$/.test(replacesAgentId)) ||
      agentRole ||
      runtime === "generic")
  )
    throw new Error("Invalid work-item session routing.");
  let workContextJSON = "";
  if (hasWorkItemRouting) {
    workContextJSON = serializedWorkContext(workContextBundle);
    if (
      (workContextFile || workContextDigest) &&
      (!agentId ||
        !/^\/tmp\/\.tailterm-work-context-agt_[0-9a-f]{16}-[0-9a-f]{64}\.json$/.test(
          workContextFile,
        ) ||
        !/^[0-9a-f]{64}$/.test(workContextDigest) ||
        !workContextFile.includes(`-${agentId}-`) ||
        !workContextFile.endsWith(`-${workContextDigest}.json`))
    )
      throw new Error("Invalid staged work-item context.");
  }
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
  if (agentRole) args.push("--role", agentRole);
  if (agentId) args.push("--agent-id", agentId);
  if (expectedRunId) args.push("--expected-run-id", expectedRunId);
  if (expectedLifecycleGeneration)
    args.push(
      "--expected-lifecycle-generation",
      String(expectedLifecycleGeneration),
    );
  if (resumeReceiptId) args.push("--resume-receipt-id", resumeReceiptId);
  if (plannedTeamMembers)
    args.push("--planned-team-members", String(plannedTeamMembers));
  if (hasWorkItemRouting) {
    args.push(
      "--work-item-task",
      workItemTaskId,
      "--work-item",
      workItemId,
      "--work-item-revision",
      String(workItemRevision),
      "--work-order-task",
      workOrderTaskId,
      "--work-order-message",
      String(workOrderMessageSeq),
    );
    if (replacesAgentId) args.push("--replaces-agent", replacesAgentId);
  }
  if (prompt) args.push("--prompt", prompt);
  if (runtime) args.push("--runtime", runtime);
  if (model) args.push("--model", model);
  if (reasoning) args.push("--reasoning", reasoning);
  if (permissionMode) args.push("--permission-mode", permissionMode);
  if (approvalMode) args.push("--approval-mode", approvalMode);
  if (sandboxMode) args.push("--sandbox-mode", sandboxMode);
  if (allowedTools.length)
    args.push("--allowed-tools-json", JSON.stringify(allowedTools));
  const resolve = ttResolver(
    `printf 'The tt agent CLI is not installed on this server. Install it from the tailterm hub build and retry.\\n' >&2; exit 127`,
  );
  let invoke = `exec "$tailterm_tt" ${args.map(shellQuote).join(" ")}`;
  if (hasWorkItemRouting && workContextFile) {
    const contextBytes = new TextEncoder().encode(workContextJSON).byteLength;
    const contextPath = shellQuote(workContextFile);
    invoke =
      `umask 077; tailterm_context=${contextPath}; ` +
      `trap '/bin/rm -f "$tailterm_context"' EXIT; trap 'exit 143' HUP INT TERM; ` +
      `if [ ! -f "$tailterm_context" ] || [ -L "$tailterm_context" ]; then printf 'Staged work-item context is missing or unsafe.\n' >&2; exit 1; fi; ` +
      `tailterm_mode=$(/usr/bin/stat -f '%Lp' "$tailterm_context" 2>/dev/null || :); ` +
      `if [ "$tailterm_mode" != 600 ]; then tailterm_mode=$(/usr/bin/stat -c '%a' "$tailterm_context" 2>/dev/null || :); fi; ` +
      `if [ "$tailterm_mode" != 600 ]; then printf 'Staged work-item context permissions changed.\n' >&2; exit 1; fi; ` +
      `tailterm_size=$(/usr/bin/wc -c < "$tailterm_context" | /usr/bin/tr -d '[:space:]') || exit 1; ` +
      `if [ "$tailterm_size" != ${contextBytes} ]; then printf 'Staged work-item context length changed.\n' >&2; exit 1; fi; ` +
      `tailterm_hash_bin=$(command -v shasum 2>/dev/null || command -v sha256sum 2>/dev/null || :); ` +
      `if [ -z "$tailterm_hash_bin" ]; then printf 'A SHA-256 utility is required to verify staged work-item context.\n' >&2; exit 127; fi; ` +
      `case "$tailterm_hash_bin" in *shasum) tailterm_hash=$($tailterm_hash_bin -a 256 "$tailterm_context") ;; *) tailterm_hash=$($tailterm_hash_bin "$tailterm_context") ;; esac; ` +
      `tailterm_hash=\${tailterm_hash%% *}; ` +
      `if [ "$tailterm_hash" != ${shellQuote(workContextDigest)} ]; then printf 'Staged work-item context digest changed.\n' >&2; exit 1; fi; ` +
      `"$tailterm_tt" ${args.map(shellQuote).join(" ")} --work-context-file "$tailterm_context"; ` +
      `tailterm_result=$?; exit "$tailterm_result"`;
  } else if (hasWorkItemRouting) {
    // Base64 avoids repeated shell-quote expansion of the complete history.
    // The private file uses the existing CLI flag, including on older hosts.
    const encoded = btoa(
      Array.from(new TextEncoder().encode(workContextJSON), (byte) =>
        String.fromCharCode(byte),
      ).join(""),
    );
    invoke =
      `umask 077; tailterm_context=$(/usr/bin/mktemp) || exit 1; ` +
      `trap '/bin/rm -f "$tailterm_context"' EXIT; trap 'exit 143' HUP INT TERM; ` +
      `printf '%s' '${encoded}' | /usr/bin/base64 -d > "$tailterm_context" || exit 1; ` +
      `"$tailterm_tt" ${args.map(shellQuote).join(" ")} --work-context-file "$tailterm_context"; ` +
      `tailterm_result=$?; exit "$tailterm_result"`;
  }
  return "/bin/sh -c " + shellQuote(resolve + invoke);
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

export function agentToolsCommand(runtime, cwd = "") {
  if (!["codex", "claude", "gemini", "aider"].includes(runtime))
    throw new Error("Choose a supported agent app to inspect tools.");
  validateStartDirectory(cwd);
  const args = ["tools", "--runtime", runtime, "--json"];
  if (cwd) args.push("--cwd", cwd);
  return (
    "/bin/sh -c " +
    shellQuote(
      ttResolver(
        "printf 'Update the tt CLI on this host to inspect tools.\\n' >&2; exit 127",
      ) + `exec "$tailterm_tt" ${args.map(shellQuote).join(" ")}`,
    )
  );
}

// Called on the saved SSH host for these agents; the CLI only stops sessions
// whose task/run metadata matches and confirms already-absent local sessions.
export function agentCleanupCommand(hub, task, agents) {
  if (!/^https?:\/\/[A-Za-z0-9][A-Za-z0-9.:/_-]{0,199}$/.test(hub))
    throw new Error("Invalid hub URL.");
  if (
    !/^tsk_[0-9a-f]{16}$/.test(task) ||
    !Array.isArray(agents) ||
    !agents.length ||
    agents.some((id) => !/^agt_[0-9a-f]{16}$/.test(id))
  )
    throw new Error("Invalid cleanup identities.");
  const args = [
    "cleanup",
    "--json",
    "--hub",
    hub,
    "--task",
    task,
    "--agents",
    agents.join(","),
  ];
  return (
    "/bin/sh -c " +
    shellQuote(
      ttResolver(
        "printf 'Update the tt CLI on this host to clean up agent sessions.\\n' >&2; exit 127",
      ) + `exec "$tailterm_tt" ${args.map(shellQuote).join(" ")}`,
    )
  );
}
