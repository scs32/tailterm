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
    serverId: host.getServers()[0]?.id || "",
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
    host.dialog(
      existing ? "Edit team" : "New team",
      `<form id="team-form" novalidate><label>Team name<input id="team-name" maxlength="80" value="${esc(draft.name)}" placeholder="e.g. Code review"></label><div class="team-members-bar"><div id="team-members" class="segmented" role="group" aria-label="Team members"></div><button id="team-add-member" type="button">＋ Agent</button></div><div id="team-member-editor"></div><p id="team-error" class="fine" role="status"></p><div class="dialog-actions"><button id="team-remove-member" type="button">Remove member</button><button class="primary" type="submit">Save team</button></div></form>`,
    );
    const form = document.querySelector("#team-form");
    function capture() {
      const editor = form.querySelector("#team-member-editor");
      for (const el of editor.querySelectorAll("[data-field]"))
        draft.members[selected][el.dataset.field] = el.value;
    }
    function renderEditor() {
      const m = draft.members[selected];
      form.querySelector("#team-members").innerHTML = draft.members
        .map(
          (member, i) =>
            `<button type="button" data-member="${i}" aria-pressed="${selected === i}">${esc(member.name || "Agent " + (i + 1))}</button>`,
        )
        .join("");
      form.querySelector("#team-add-member").disabled =
        draft.members.length >= MAX_TEAM_MEMBERS;
      form.querySelector("#team-remove-member").disabled =
        draft.members.length === 1;
      const known = ["codex", "claude", "aider", "gemini", "generic"];
      if (!known.includes(m.runtime)) known.unshift(m.runtime);
      form.querySelector("#team-member-editor").innerHTML =
        `<div class="appearance-controls"><label>Agent name<input data-field="name" value="${esc(m.name)}" maxlength="64"></label><label>Role<input data-field="role" value="${esc(m.role)}" placeholder="e.g. Reviewer" maxlength="80"></label></div><label>Server<select data-field="serverId">${!host.getServers().some((s) => s.id === m.serverId) ? '<option value="">Choose a server…</option>' : ""}${host
          .getServers()
          .map(
            (s) =>
              `<option value="${esc(s.id)}" ${s.id === m.serverId ? "selected" : ""}>${esc(s.name)}</option>`,
          )
          .join(
            "",
          )}</select></label><div class="appearance-controls"><label>Agent app<select data-field="runtime">${known.map((r) => `<option value="${esc(r)}" ${r === m.runtime ? "selected" : ""}>${r === "generic" ? "Custom command" : esc(r)}</option>`).join("")}</select></label><label>Model<input data-field="model" value="${esc(m.model)}" placeholder="App default" maxlength="200" ${["codex", "claude", "aider", "gemini"].includes(m.runtime) ? "" : "disabled"}></label></div><label>Instructions<textarea data-field="prompt" rows="3" maxlength="8192" placeholder="What this member should do on every task…">${esc(m.prompt)}</textarea></label><details class="dialog-details"><summary>Command &amp; directory</summary><label>Command override<input data-field="run" value="${esc(m.run)}" placeholder="${m.runtime === "generic" ? "your-agent --flag" : esc(m.runtime)}"></label><label>Working directory<input data-field="cwd" value="${esc(m.cwd)}" placeholder="/absolute/project/path (optional)"></label></details>`;
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
        renderEditor();
      };
    }
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
      selected = Math.max(0, selected - 1);
      renderEditor();
    };
    form.onsubmit = async (e) => {
      e.preventDefault();
      const button = form.querySelector("button[type=submit]");
      if (button.disabled) return;
      try {
        capture();
        draft.name = form.querySelector("#team-name").value;
        const team = normalizeTeam(draft);
        if (
          team.members.some(
            (m) => !host.getServers().some((s) => s.id === m.serverId),
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
        form.querySelector("#team-error").textContent = error.message;
      } finally {
        button.disabled = false;
      }
    };
    renderEditor();
    form.querySelector("#team-name").focus();
  }
  function render() {
    if (!root || !visible) return;
    const teams = host.getData().teams || [];
    root.innerHTML = `<div class="teams-view"><div class="tasks-head"><div><h2>Teams <span class="count-badge">${teams.length}</span></h2><p class="fine">Reusable agents, roles and instructions. A team can have just one member.</p></div><button id="teams-new" class="primary">＋ New team</button></div><div class="team-list">${teams.map((t) => `<article class="team-row" data-team="${esc(t.id)}"><div><h3>${esc(t.name)}</h3><p class="fine">${t.members.map((m) => esc(m.role || m.name) + " · " + esc(m.runtime) + (m.model ? " / " + esc(m.model) : "")).join(" &nbsp; · &nbsp; ")}</p></div><div class="view-actions"><button data-new-task="${esc(t.id)}">New task</button><button data-add-team="${esc(t.id)}">Add to task</button><button data-edit-team="${esc(t.id)}">Edit</button><button data-delete-team="${esc(t.id)}" aria-label="Delete ${esc(t.name)}">×</button></div></article>`).join("") || '<div class="tasks-empty"><h3>Build your first team.</h3><p class="fine">Choose each agent’s server, model, role and instructions, then reuse the team across tasks.</p></div>'}</div></div>`;
    root.querySelector("#teams-new").onclick = () => edit();
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
