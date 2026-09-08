import { PERMISSION_MODES } from "../shared/agent-permissions.js";
const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const HELP = {
  "": "Uses this app's existing host settings. No permission overrides are added.",
  "on-request":
    "Codex may ask for approval. The host's sandbox settings still apply.",
  "workspace-auto":
    "Codex may edit the specified project and Tailterm relay state, with network access for coordination. Denied commands fail instead of prompting. Set an absolute working directory below. Connector approvals and login can still need attention.",
  "full-auto":
    "Codex commands run without its filesystem sandbox or command approval prompts, with your SSH user's access to the machine. External service policies and login requirements still apply.",
  acceptEdits:
    "Claude accepts file edits; other actions may still require approval.",
  auto: "Claude reviews actions automatically. Explicit ask rules, unavailable auto mode, or repeated denials can still lead to prompts.",
  dontAsk:
    "Claude denies calls that would otherwise prompt. Preapprove the tools it needs using the rules below or host settings; denied calls do not become approvals.",
  bypassPermissions:
    "Claude bypasses ordinary permission checks. Some protected actions, organization rules, and login flows can still require attention.",
};
export function agentControlsHTML(runtime, mode = "", allowed = []) {
  const options = PERMISSION_MODES[runtime] || [["", "Host settings"]];
  const rules = Array.isArray(allowed) ? allowed.join("\n") : allowed;
  return `<details class="dialog-details agent-controls"><summary>Permissions &amp; tools</summary><label>Permissions<select data-field="permissionMode" ${PERMISSION_MODES[runtime] ? "" : "disabled"}>${options.map(([id, label]) => `<option value="${id}" ${id === mode ? "selected" : ""}>${esc(label)}</option>`).join("")}</select></label><p class="fine permission-help">${esc(HELP[mode] || HELP[""])}</p>${runtime === "claude" ? `<label>Preapproved tool rules<textarea data-field="allowedTools" rows="2" placeholder="Read&#10;Bash(tt *)">${esc(rules)}</textarea><span class="fine">Optional Claude allow rules, one per line. These approve calls; they do not remove other tools from the agent.</span></label>` : ""}<button type="button" class="agent-inspect-tools" ${["codex", "claude", "gemini", "aider"].includes(runtime) ? "" : "disabled"}>Inspect host tools</button><div class="agent-tool-inventory fine" role="status" aria-live="polite"></div></details>`;
}
export function wireAgentControls(root, inspect) {
  const mode = root.querySelector("[data-field=permissionMode]");
  mode.onchange = () => {
    root.querySelector(".permission-help").textContent =
      HELP[mode.value] || HELP[""];
  };
  const button = root.querySelector(".agent-inspect-tools"),
    output = root.querySelector(".agent-tool-inventory");
  button.onclick = async () => {
    button.disabled = true;
    output.textContent = "Inspecting the selected host…";
    try {
      const r = await inspect();
      if (!output.isConnected) return;
      output.innerHTML = `<p>${esc(r.runtime)} ${esc(r.version)} · ${esc(r.host)} · ${esc(r.scope)} inventory</p><p>Installed commands: ${esc((r.commands || []).join(", ") || "None reported")}</p>${(r.servers || []).map((s) => `<details><summary>${esc(s.name)} · ${s.tools.length} tools · ${esc(s.runtimeStatus || "connection unverified")} · ${esc(s.authStatus)}</summary><p>${s.tools.map(esc).join("<br>") || "No tool names reported."}</p></details>`).join("")}${(r.notes || []).map((n) => `<p>${esc(n)}</p>`).join("")}`;
    } catch (e) {
      if (output.isConnected)
        output.textContent = e.message || "Tool inspection failed.";
    } finally {
      if (button.isConnected) button.disabled = false;
    }
  };
}
