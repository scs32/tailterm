import { agentControlsHTML, wireAgentControls } from "./agent-controls.js";
import { modelPickerHTML, wireModelPicker } from "./model-picker.js";
import { projectFolderHTML, wireProjectFolder } from "./project-folder.js";
import { definitionUsers, normalizeAgentDefinition } from "./agents.js";

const esc = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );

export function createAgentsView(host) {
  let root;
  let visible = false;
  let selectedId = "";
  const definitions = () => host.getData().agentCatalog?.definitions || [];

  function edit(existing = {}) {
    const draft = {
      id: existing.id || "",
      revision: existing.revision || 1,
      name: existing.name || "",
      launchName: existing.launchName || "agent1",
      role: existing.role || "",
      serverId: existing.serverId || "",
      runtime: existing.runtime || "codex",
      model: existing.model || "",
      reasoning: existing.reasoning || "",
      approvalMode: existing.approvalMode || "",
      sandboxMode: existing.sandboxMode || "",
      permissionMode: existing.permissionMode || "",
      allowedTools: existing.allowedTools || [],
      run: existing.run || "",
      cwd: existing.cwd || "",
      prompt: existing.prompt || "",
    };
    host.dialog(
      existing.id ? "Edit agent" : "New agent",
      '<form id="agent-library-form" novalidate><div id="agent-library-editor"></div><div class="task-submit-area"><p id="agent-library-error" class="fine" role="alert" aria-live="assertive" tabindex="-1"></p><div class="dialog-actions"><button class="primary" type="submit">Save agent</button></div></div></form>',
    );
    const form = document.querySelector("#agent-library-form");
    const capture = () => {
      for (const el of form.querySelectorAll("[data-field]")) {
        if (el.dataset.field === "allowedTools") continue;
        draft[el.dataset.field] = el.value;
      }
      const tools = form.querySelector('[data-field="allowedTools"]');
      if (tools)
        draft.allowedTools = tools.value
          .split("\n")
          .map((v) => v.trim())
          .filter(Boolean);
    };
    const renderEditor = () => {
      const runtimes = ["codex", "claude", "aider", "gemini", "generic"];
      if (!runtimes.includes(draft.runtime)) runtimes.unshift(draft.runtime);
      form.querySelector("#agent-library-editor").innerHTML =
        `<div class="appearance-controls"><label>Display name<input data-field="name" maxlength="80" value="${esc(draft.name)}" placeholder="Implementation"></label><label>Launch name<input data-field="launchName" maxlength="64" value="${esc(draft.launchName)}" placeholder="builder"></label></div><label>Default role<input data-field="role" maxlength="80" value="${esc(draft.role)}" placeholder="e.g. Builder"></label><label>Machine<select data-field="serverId"><option value="">Main machine</option>${draft.serverId && !host.getServers().some((s) => s.id === draft.serverId) ? `<option value="${esc(draft.serverId)}" selected>Missing machine · choose another</option>` : ""}${host
          .getServers()
          .map(
            (s) =>
              `<option value="${esc(s.id)}" ${s.id === draft.serverId ? "selected" : ""}>${esc(s.name)}</option>`,
          )
          .join(
            "",
          )}</select></label><div class="appearance-controls"><label>Agent app<select data-field="runtime">${runtimes.map((runtime) => `<option value="${esc(runtime)}" ${runtime === draft.runtime ? "selected" : ""}>${runtime === "generic" ? "Custom command" : esc(runtime)}</option>`).join("")}</select></label>${modelPickerHTML("agent-library-model", draft.runtime, draft.model)}</div>${agentControlsHTML(draft.runtime, draft.permissionMode, draft.allowedTools, draft)}${projectFolderHTML("agent-library-cwd", draft.cwd, "Project folder override", true)}<p class="fine">Leave blank to choose the project folder when launching a team.</p><label>Instructions<textarea data-field="prompt" rows="6" maxlength="8192" placeholder="Reusable instructions for this agent…">${esc(draft.prompt)}</textarea></label><details class="dialog-details"><summary>Command override</summary><label>Command override<input data-field="run" maxlength="1024" value="${esc(draft.run)}" placeholder="${draft.runtime === "generic" ? "your-agent --flag" : esc(draft.runtime)}"></label></details>`;
      form.querySelector("#agent-library-cwd").dataset.field = "cwd";
      wireProjectFolder(
        form.querySelector("#agent-library-cwd").closest(".project-folder"),
        host,
        () => {
          const id = form.querySelector('[data-field="serverId"]').value;
          return id
            ? host.getServers().find((s) => s.id === id)
            : host.currentServer?.() || host.getServers()[0];
        },
      );
      wireModelPicker(form.querySelector("[data-model-picker]"));
      const modelChoice = form.querySelector("#agent-library-model-choice");
      const chooseModel = modelChoice.onchange;
      modelChoice.onchange = () => {
        chooseModel();
        capture();
        draft.reasoning = "";
        if (modelChoice.value !== "__custom") renderEditor();
      };
      wireAgentControls(form.querySelector(".agent-controls"), async () => {
        capture();
        if (!host.inspectTools)
          throw new Error(
            "Tool inspection is unavailable in this environment.",
          );
        return host.inspectTools(draft);
      });
      form.querySelector('[data-field="runtime"]').onchange = () => {
        const previous = draft.runtime;
        capture();
        if (draft.run === previous) draft.run = "";
        Object.assign(draft, {
          model: "",
          reasoning: "",
          approvalMode: "",
          sandboxMode: "",
          permissionMode: "",
          allowedTools: [],
        });
        renderEditor();
      };
    };
    form.onsubmit = async (event) => {
      event.preventDefault();
      const button = form.querySelector('button[type="submit"]');
      const status = form.querySelector("#agent-library-error");
      button.disabled = true;
      button.textContent = "Saving…";
      status.textContent = "";
      try {
        capture();
        const definition = normalizeAgentDefinition(draft);
        if (
          definition.serverId &&
          !host.getServers().some((s) => s.id === definition.serverId)
        )
          throw new Error("Choose an available machine.");
        await host.api("/agents", "POST", definition);
        await host.reloadData();
        selectedId = definition.id;
        host.closeDialog();
        render();
        host.notice(`Saved agent ${definition.name}.`);
      } catch (error) {
        status.textContent = error.message || "Could not save the agent.";
        status.focus();
      } finally {
        button.disabled = false;
        button.textContent = "Save agent";
      }
    };
    renderEditor();
    form.querySelector('[data-field="name"]').focus();
  }

  function render() {
    if (!root || !visible) return;
    const agents = definitions();
    const teams = host.getData().teams || [];
    let selected =
      agents.find((agent) => agent.id === selectedId) || agents[0] || null;
    selectedId = selected?.id || "";
    const uses = selected ? definitionUsers(selected.id, teams) : [];
    const detail = selected
      ? `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">AGENT</span><h2>${esc(selected.name)}</h2></div><div class="view-actions"><button data-edit-agent>Edit</button><button data-delete-agent>Delete</button></div></div><p class="fine">${esc(selected.runtime)}${selected.model ? " / " + esc(selected.model) : " / inherited model"} · reasoning ${esc(selected.reasoning || "inherited")} · revision ${selected.revision}</p></div><div class="agent-detail-list"><article class="team-row"><div><h3>Launch identity · ${esc(selected.launchName)}</h3><p class="fine">${esc(selected.role || "No default role")} · ${selected.serverId ? "Saved machine" : "Main machine"}${selected.cwd ? " · folder override" : " · project folder"}</p></div></article><article class="team-row"><div><h3>Used by ${uses.length} team${uses.length === 1 ? "" : "s"}</h3><p class="fine">${uses.length ? uses.map(esc).join(" · ") : "Not currently referenced."}</p></div></article><article class="team-row"><div><h3>Instructions</h3><p class="fine agent-prompt-preview">${esc(selected.prompt || "No reusable instructions.")}</p></div></article></div>`
      : `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">AGENTS</span><h2>No saved agents.</h2></div></div><p class="fine">Create a reusable agent once, then add it to any team.</p></div>`;
    root.innerHTML = `<div class="board mode-board agents-view"><aside class="board-rail"><div class="board-rail-head"><span class="eyebrow">AGENTS</span><button id="agents-new" title="New agent" aria-label="New agent">＋</button></div>${agents.map((agent) => `<button type="button" data-board-task="${esc(agent.id)}" data-agent-select="${esc(agent.id)}" aria-pressed="${agent.id === selectedId}" title="${esc(agent.name)}"><span class="board-task-name">${esc(agent.name)}</span><span class="fine">${esc(agent.runtime)}${agent.reasoning ? " · " + esc(agent.reasoning) : ""}</span></button>`).join("")}</aside><section class="board-thread agents-detail">${detail}</section></div>`;
    root.querySelector("#agents-new").onclick = () => edit();
    root.querySelectorAll("[data-agent-select]").forEach(
      (button) =>
        (button.onclick = () => {
          selectedId = button.dataset.agentSelect;
          render();
        }),
    );
    if (!selected) return;
    root.querySelector("[data-edit-agent]").onclick = () => edit(selected);
    root.querySelector("[data-delete-agent]").onclick = async () => {
      if (
        !(await host.confirm(
          `Delete agent ${selected.name}?`,
          uses.length
            ? `This agent is used by ${uses.join(", ")}. Remove those team references first.`
            : "Saved teams and running agents are otherwise unchanged.",
        ))
      )
        return;
      try {
        await host.api(`/agents/${selected.id}`, "DELETE");
        selectedId = "";
        await host.reloadData();
        render();
      } catch (error) {
        host.notice(error.message);
      }
    };
  }
  return {
    mount(container) {
      root = container;
    },
    show() {
      visible = true;
      render();
    },
    hide() {
      visible = false;
    },
    refresh: render,
    edit,
  };
}
