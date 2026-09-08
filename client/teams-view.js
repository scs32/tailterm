import { projectFolderHTML, wireProjectFolder } from "./project-folder.js";
import { agentControlsHTML, wireAgentControls } from "./agent-controls.js";
import { TEAM_EXAMPLES, exampleTeam } from "./team-examples.js";
import { modelPickerHTML, wireModelPicker } from "./model-picker.js";
import { normalizeTeam, MAX_TEAM_MEMBERS } from "./teams.js";
const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
export function createTeamsView(host) {
  let root,
    visible = false;
  const blank = () => ({
    name: "agent1",
    role: "",
    serverId: "",
    runtime: "codex",
    model: "",
    run: "",
    cwd: "",
    prompt: "",
  });
  function edit(existing) {
    const draft = existing
      ? structuredClone(existing)
      : { name: "", members: [blank()] };
    let selected = 0;
    let orchestratorIndex = Math.max(
      0,
      draft.members.findIndex((m) => m.name === draft.orchestrator),
    );
    host.dialog(
      existing?.id ? "Edit team" : "New team",
      `<form id="team-form" novalidate><label>Team name<input id="team-name" maxlength="80" value="${esc(draft.name)}" placeholder="e.g. Code review"></label><label>Main orchestrator<select id="team-orchestrator"></select><span class="fine">Launches first, receives member introductions, assigns work and owns the final result.</span></label><label class="check"><input id="team-swarm" type="checkbox" ${draft.swarm ? "checked" : ""}>Enable swarm</label><p class="fine">Broadcast every new task message to all agents, including helpers. A named recipient still owns the assignment. Applies to the whole task.</p><div class="team-members-bar"><div id="team-members" class="segmented" role="group" aria-label="Team members"></div><button id="team-add-member" type="button">＋ Agent</button></div><div id="team-member-editor"></div><div class="task-submit-area"><p id="team-error" class="fine" role="alert" aria-live="assertive" tabindex="-1"></p><div class="dialog-actions"><button id="team-remove-member" type="button">Remove member</button><button class="primary" type="submit">Save team</button></div></div></form>`,
    );
    const form = document.querySelector("#team-form");
    function capture() {
      const editor = form.querySelector("#team-member-editor");
      for (const el of editor.querySelectorAll("[data-field]"))
        draft.members[selected][el.dataset.field] = el.value;
    }
    function renderEditor() {
      const m = draft.members[selected];
      form.querySelector("#team-orchestrator").innerHTML = draft.members
        .map(
          (member, i) =>
            `<option value="${i}" ${i === orchestratorIndex ? "selected" : ""}>${esc(member.name || "Agent " + (i + 1))}</option>`,
        )
        .join("");
      form.querySelector("#team-members").innerHTML = draft.members
        .map(
          (member, i) =>
            `<button type="button" data-member="${i}" title="${esc(member.name || "Agent " + (i + 1))}" aria-pressed="${selected === i}">${esc(member.name || "Agent " + (i + 1))}</button>`,
        )
        .join("");
      const memberStrip = form.querySelector("#team-members");
      const activeMember = memberStrip.querySelector('[aria-pressed="true"]');
      const stripRect = memberStrip.getBoundingClientRect();
      const activeRect = activeMember.getBoundingClientRect();
      if (activeRect.left < stripRect.left)
        memberStrip.scrollLeft -= stripRect.left - activeRect.left;
      else if (activeRect.right > stripRect.right)
        memberStrip.scrollLeft += activeRect.right - stripRect.right;
      form.querySelector("#team-add-member").disabled =
        draft.members.length >= MAX_TEAM_MEMBERS;
      form.querySelector("#team-remove-member").disabled =
        draft.members.length === 1;
      const known = ["codex", "claude", "aider", "gemini", "generic"];
      if (!known.includes(m.runtime)) known.unshift(m.runtime);
      form.querySelector("#team-member-editor").innerHTML =
        `<div class="appearance-controls"><label>Agent name<input data-field="name" value="${esc(m.name)}" maxlength="64"></label><label>Role<input data-field="role" value="${esc(m.role)}" placeholder="e.g. Reviewer" maxlength="80"></label></div><label>Machine<select data-field="serverId"><option value="">Main machine</option>${m.serverId && !host.getServers().some((s) => s.id === m.serverId) ? `<option value="${esc(m.serverId)}" selected>Missing machine · choose another</option>` : ""}${host
          .getServers()
          .map(
            (s) =>
              `<option value="${esc(s.id)}" ${s.id === m.serverId ? "selected" : ""}>${esc(s.name)}</option>`,
          )
          .join(
            "",
          )}</select></label><div class="appearance-controls"><label>Agent app<select data-field="runtime">${known.map((r) => `<option value="${esc(r)}" ${r === m.runtime ? "selected" : ""}>${r === "generic" ? "Custom command" : esc(r)}</option>`).join("")}</select></label>${modelPickerHTML("team-model", m.runtime, m.model)}</div>${agentControlsHTML(m.runtime, m.permissionMode, m.allowedTools)}${projectFolderHTML("team-cwd", m.cwd, "Project folder override", true)}<p class="fine">Leave blank to choose the project when launching this team.</p><label>Instructions<textarea data-field="prompt" rows="3" maxlength="8192" placeholder="What this member should do on every task…">${esc(m.prompt)}</textarea></label><details class="dialog-details"><summary>Command override</summary><label>Command override<input data-field="run" value="${esc(m.run)}" placeholder="${m.runtime === "generic" ? "your-agent --flag" : esc(m.runtime)}"></label></details>`;
      form.querySelector("#team-cwd").dataset.field = "cwd";
      wireProjectFolder(
        form.querySelector("#team-cwd").closest(".project-folder"),
        host,
        () => {
          const id = form.querySelector("[data-field=serverId]").value;
          return id
            ? host.getServers().find((s) => s.id === id)
            : host.currentServer?.() || host.getServers()[0];
        },
      );
      wireModelPicker(form.querySelector("[data-model-picker]"));
      wireAgentControls(form.querySelector(".agent-controls"), async () => {
        capture();
        if (!host.inspectTools)
          throw new Error(
            "Tool inspection is unavailable in this environment.",
          );
        return host.inspectTools({ ...draft.members[selected] });
      });
      form.querySelector("[data-field=name]").oninput = (e) => {
        form.querySelector("#team-orchestrator").options[selected].textContent =
          e.target.value || "Agent " + (selected + 1);
      };
      form.querySelectorAll("[data-member]").forEach(
        (b) =>
          (b.onclick = () => {
            capture();
            selected = Number(b.dataset.member);
            renderEditor();
          }),
      );
      form.querySelector("[data-field=runtime]").onchange = () => {
        const oldRuntime = m.runtime;
        capture();
        if (draft.members[selected].run === oldRuntime)
          draft.members[selected].run = "";
        draft.members[selected].model = "";
        draft.members[selected].permissionMode = "";
        draft.members[selected].allowedTools = [];
        renderEditor();
      };
    }
    form.querySelector("#team-orchestrator").onchange = (e) => {
      orchestratorIndex = Number(e.target.value);
    };
    form.querySelector("#team-add-member").onclick = () => {
      capture();
      const m = blank();
      let n = 1;
      while (draft.members.some((x) => x.name === "agent" + n)) n++;
      m.name = "agent" + n;
      draft.members.push(m);
      selected = draft.members.length - 1;
      renderEditor();
    };
    form.querySelector("#team-remove-member").onclick = () => {
      capture();
      draft.members.splice(selected, 1);
      if (selected === orchestratorIndex) orchestratorIndex = 0;
      else if (selected < orchestratorIndex) orchestratorIndex--;
      selected = Math.max(0, selected - 1);
      renderEditor();
    };
    form.onsubmit = async (e) => {
      e.preventDefault();
      const button = form.querySelector("button[type=submit]");
      if (button.disabled) return;
      button.disabled = true;
      button.textContent = "Saving…";
      form.querySelector("#team-error").textContent = "";
      try {
        capture();
        draft.name = form.querySelector("#team-name").value;
        draft.swarm = form.querySelector("#team-swarm").checked;
        draft.orchestrator = draft.members[orchestratorIndex].name.trim();
        const team = normalizeTeam(draft);
        if (
          team.members.some(
            (m) =>
              m.serverId && !host.getServers().some((s) => s.id === m.serverId),
          )
        )
          throw new Error("Choose an available server for each agent.");
        button.disabled = true;
        await host.api("/teams", "POST", team);
        await host.reloadData();
        host.closeDialog();
        render();
        host.notice("Saved team " + team.name + ".");
      } catch (error) {
        if (Number.isInteger(error.memberIndex)) {
          selected = error.memberIndex;
          renderEditor();
          const field = form.querySelector(`[data-field="${error.field}"]`);
          if (field) {
            const details = field.closest("details");
            if (details) details.open = true;
            field.setAttribute("aria-invalid", "true");
            field.setAttribute("aria-describedby", "team-error");
          }
        }
        const status = form.querySelector("#team-error");
        status.textContent =
          error.message || "Could not save the team. Try again.";
        status.focus();
        status.scrollIntoView({ block: "nearest" });
      } finally {
        button.disabled = false;
        button.textContent = "Save team";
      }
    };
    renderEditor();
    form.querySelector("#team-name").focus();
  }
  function examples() {
    host.dialog(
      "Team examples",
      `<p class="fine">Nine starting points, including an Astra-led swarm with four Terra workers. All members use the main machine unless you assign another. Customize before saving.</p><div class="team-examples">${TEAM_EXAMPLES.map((e) => `<article class="team-example"><div class="team-example-head"><h3>${esc(e.name)} <span class="fine">${e.members.length} agent${e.members.length === 1 ? "" : "s"}</span></h3><button data-example="${esc(e.id)}">Customize</button></div><p>${esc(e.summary)}</p><p class="fine">${esc(e.fit)}</p><details><summary>Workflow, models &amp; example task</summary><p class="fine">${esc(e.workflow)}</p><p class="fine">${e.members.map((m) => esc(m.name) + " · " + esc(m.model)).join("<br>")}</p><p class="fine">${esc(e.goal)}</p></details></article>`).join("")}</div>`,
    );
    document
      .querySelectorAll("[data-example]")
      .forEach(
        (button) =>
          (button.onclick = () => edit(exampleTeam(button.dataset.example))),
      );
  }
  function render() {
    if (!root || !visible) return;
    const teams = host.getData().teams || [];
    root.innerHTML = `<div class="teams-view"><div class="tasks-head"><div><h2>Teams <span class="count-badge">${teams.length}</span></h2><p class="fine">Reusable agents, roles and instructions. A team can have just one member.</p></div><div class="view-actions"><button id="teams-examples">Examples</button><button id="teams-new" class="primary">＋ New team</button></div></div><div class="team-list">${teams.map((t) => `<article class="team-row" data-team="${esc(t.id)}"><div><h3>${esc(t.name)}${t.swarm ? ' <span class="fine">Swarm</span>' : ""}</h3><p class="fine">${t.members.map((m) => esc(m.role || m.name) + " · " + esc(m.runtime) + (m.model ? " / " + esc(m.model) : "")).join(" &nbsp; · &nbsp; ")}</p></div><div class="view-actions"><button data-new-task="${esc(t.id)}">New task</button><button data-add-team="${esc(t.id)}">Add to task</button><button data-edit-team="${esc(t.id)}">Edit</button><button data-delete-team="${esc(t.id)}" aria-label="Delete ${esc(t.name)}">×</button></div></article>`).join("") || '<div class="tasks-empty"><h3>Build your first team.</h3><p class="fine">Choose each agent’s server, model, role and instructions, then reuse the team across tasks.</p></div>'}</div></div>`;
    root.querySelector("#teams-new").onclick = () => edit();
    root.querySelector("#teams-examples").onclick = examples;
    for (const t of teams) {
      const row = root.querySelector(`[data-team="${t.id}"]`);
      row.querySelector("[data-edit-team]").onclick = () => edit(t);
      row.querySelector("[data-new-task]").onclick = () => host.newTask(t);
      row.querySelector("[data-add-team]").onclick = () => host.addTeam(t);
      row.querySelector("[data-delete-team]").onclick = async () => {
        if (
          !(await host.confirm(
            "Delete team " + t.name + "?",
            "This removes the saved template. Agents already launched keep running.",
          ))
        )
          return;
        try {
          await host.api("/teams/" + t.id, "DELETE");
          await host.reloadData();
          render();
        } catch (e) {
          host.notice(e.message);
        }
      };
    }
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
