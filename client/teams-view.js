import { TEAM_EXAMPLES, exampleTeam } from "./team-examples.js";
import {
  migrateAgentData,
  normalizeReferencedTeam,
  resolveTeamMember,
} from "./agents.js";
import { MAX_TEAM_MEMBERS } from "./teams.js";

const esc = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const newId = (prefix) => prefix + crypto.randomUUID().replaceAll("-", "");

export function createTeamsView(host) {
  let root;
  let visible = false;
  let selectedTeam = "";
  const definitions = () => host.getData().agentCatalog?.definitions || [];
  const resolved = (team) =>
    team.members.map((member) => resolveTeamMember(member, definitions()));

  function edit(existing, pendingDefinitions = []) {
    const agents = [...definitions(), ...pendingDefinitions];
    if (!agents.length) {
      host.notice("Create an Agent before creating a team.");
      host.openAgents?.();
      return;
    }
    const draft = existing
      ? structuredClone(existing)
      : {
          id: "",
          name: "",
          swarm: false,
          orchestrator: agents[0].launchName,
          members: [{ agentDefinitionId: agents[0].id, alias: "", role: "" }],
        };
    let selected = 0;
    let orchestratorIndex = Math.max(
      0,
      draft.members.findIndex(
        (member) =>
          (member.alias ||
            agents.find((agent) => agent.id === member.agentDefinitionId)
              ?.launchName) === draft.orchestrator,
      ),
    );
    host.dialog(
      existing?.id ? "Edit team" : "New team",
      '<form id="team-form" novalidate><label>Team name<input id="team-name" maxlength="80" placeholder="e.g. Code review"></label><label>Main orchestrator<select id="team-orchestrator"></select><span class="fine">Launches first and owns the final result.</span></label><label class="check"><input id="team-swarm" type="checkbox">Enable swarm</label><p class="fine">Every member references a reusable Agent. Alias and role are the only team-specific overrides.</p><div class="team-members-bar"><div id="team-members" class="segmented" role="group" aria-label="Team members"></div><button id="team-add-member" type="button">＋ Agent</button></div><div id="team-member-editor"></div><div class="task-submit-area"><p id="team-error" class="fine" role="alert" aria-live="assertive" tabindex="-1"></p><div class="dialog-actions"><button id="team-remove-member" type="button">Remove member</button><button class="primary" type="submit">Save team</button></div></div></form>',
    );
    const form = document.querySelector("#team-form");
    form.querySelector("#team-name").value = draft.name;
    form.querySelector("#team-swarm").checked = draft.swarm;
    const memberName = (member) =>
      member.alias ||
      agents.find((agent) => agent.id === member.agentDefinitionId)
        ?.launchName ||
      "Missing agent";
    const capture = () => {
      for (const el of form.querySelectorAll(
        "#team-member-editor [data-field]",
      ))
        draft.members[selected][el.dataset.field] = el.value;
    };
    function renderEditor() {
      const member = draft.members[selected];
      form.querySelector("#team-orchestrator").innerHTML = draft.members
        .map(
          (item, index) =>
            `<option value="${index}" ${index === orchestratorIndex ? "selected" : ""}>${esc(memberName(item))}</option>`,
        )
        .join("");
      form.querySelector("#team-members").innerHTML = draft.members
        .map(
          (item, index) =>
            `<button type="button" data-member="${index}" aria-pressed="${index === selected}" title="${esc(memberName(item))}">${esc(memberName(item))}</button>`,
        )
        .join("");
      form.querySelector("#team-add-member").disabled =
        draft.members.length >= MAX_TEAM_MEMBERS;
      form.querySelector("#team-remove-member").disabled =
        draft.members.length === 1;
      form.querySelector("#team-member-editor").innerHTML =
        `<label>Agent definition<select data-field="agentDefinitionId">${agents.map((agent) => `<option value="${esc(agent.id)}" ${agent.id === member.agentDefinitionId ? "selected" : ""}>${esc(agent.name)} · ${esc(agent.runtime)}${agent.model ? " / " + esc(agent.model) : ""}</option>`).join("")}</select></label><div class="appearance-controls"><label>Team alias<input data-field="alias" maxlength="64" value="${esc(member.alias)}" placeholder="${esc(agents.find((agent) => agent.id === member.agentDefinitionId)?.launchName || "agent")}"></label><label>Role override<input data-field="role" maxlength="80" value="${esc(member.role)}" placeholder="${esc(agents.find((agent) => agent.id === member.agentDefinitionId)?.role || "Use Agent default")}"></label></div><p class="fine">Runtime, model, reasoning, permissions, machine, folder and instructions come from the selected Agent.</p>`;
      form.querySelectorAll("[data-member]").forEach(
        (button) =>
          (button.onclick = () => {
            capture();
            selected = Number(button.dataset.member);
            renderEditor();
          }),
      );
      for (const el of form.querySelectorAll(
        "#team-member-editor [data-field]",
      ))
        el.oninput = () => {
          capture();
          const option = form.querySelector(
            `#team-orchestrator option[value="${selected}"]`,
          );
          if (option) option.textContent = memberName(draft.members[selected]);
        };
    }
    form.querySelector("#team-orchestrator").onchange = (event) => {
      orchestratorIndex = Number(event.target.value);
    };
    form.querySelector("#team-add-member").onclick = () => {
      capture();
      draft.members.push({
        agentDefinitionId: agents[0].id,
        alias: "",
        role: "",
      });
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
    form.onsubmit = async (event) => {
      event.preventDefault();
      const button = form.querySelector('button[type="submit"]');
      const status = form.querySelector("#team-error");
      button.disabled = true;
      button.textContent = "Saving…";
      status.textContent = "";
      try {
        capture();
        draft.name = form.querySelector("#team-name").value;
        draft.swarm = form.querySelector("#team-swarm").checked;
        draft.orchestrator = memberName(draft.members[orchestratorIndex]);
        const team = normalizeReferencedTeam(draft, agents);
        await host.api(
          "/teams",
          "POST",
          pendingDefinitions.length
            ? { definitions: pendingDefinitions, team }
            : team,
        );
        await host.reloadData();
        selectedTeam = team.id;
        host.closeDialog();
        render();
        host.notice(`Saved team ${team.name}.`);
      } catch (error) {
        if (Number.isInteger(error.memberIndex)) {
          selected = error.memberIndex;
          renderEditor();
          form
            .querySelector(`[data-field="${error.field}"]`)
            ?.setAttribute("aria-invalid", "true");
        }
        status.textContent = error.message || "Could not save the team.";
        status.focus();
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
      `<p class="fine">Customize an example to copy its members into the Agents catalog and create one referenced team atomically.</p><div class="team-examples">${TEAM_EXAMPLES.map((example) => `<article class="team-example"><div class="team-example-head"><h3>${esc(example.name)} <span class="fine">${example.members.length} agents</span></h3><button data-example="${esc(example.id)}">Customize</button></div><p>${esc(example.summary)}</p><p class="fine">${esc(example.fit)}</p></article>`).join("")}</div>`,
    );
    document.querySelectorAll("[data-example]").forEach(
      (button) =>
        (button.onclick = () => {
          const legacy = exampleTeam(button.dataset.example);
          const migrated = migrateAgentData({ teams: [legacy] });
          const created = migrated.agentCatalog.definitions.map(
            (definition) => ({
              ...definition,
              id: newId("agent_"),
              name: `${legacy.name} · ${definition.name}`,
            }),
          );
          const team = {
            id: "",
            name: legacy.name,
            swarm: legacy.swarm,
            orchestrator: legacy.orchestrator,
            members: legacy.members.map((member, index) => ({
              agentDefinitionId: created[index].id,
              alias: member.name,
              role: "",
            })),
          };
          edit(team, created);
        }),
    );
  }

  function render() {
    if (!root || !visible) return;
    const teams = host.getData().teams || [];
    let selected =
      teams.find((team) => team.id === selectedTeam) || teams[0] || null;
    selectedTeam = selected?.id || "";
    let members = [];
    let resolutionError = "";
    if (selected)
      try {
        members = resolved(selected);
      } catch (error) {
        resolutionError = error.message;
      }
    const detail = selected
      ? `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">TEAM</span><h2>${esc(selected.name)}</h2></div><div class="view-actions"><button id="teams-examples">Examples</button><button data-new-task>New project</button><button data-add-team>Add to project</button><button data-edit-team>Edit</button><button data-delete-team>Delete</button></div></div><p class="fine">${selected.members.length} agents${selected.swarm ? " · Swarm enabled" : ""} · Main orchestrator: ${esc(selected.orchestrator)}</p></div><div class="team-list"><article class="team-row"><div><h3>Reusable Agents</h3><p class="fine">${resolutionError ? esc(resolutionError) : members.map((member) => `${esc(member.role || member.name)} · ${esc(member.runtime)}${member.model ? " / " + esc(member.model) : ""}${member.reasoning ? " · " + esc(member.reasoning) : ""}`).join(" &nbsp; · &nbsp; ")}</p></div></article></div>`
      : `<div class="board-head"><div class="view-heading"><div><span class="eyebrow">TEAMS</span><h2>No saved teams.</h2></div><button id="teams-examples">Examples</button></div><p class="fine">Create Agents first, then combine stable references into reusable teams.</p></div>`;
    root.innerHTML = `<div class="board mode-board teams-view"><aside class="board-rail"><div class="board-rail-head"><span class="eyebrow">TEAMS</span><button id="teams-new" title="New team" aria-label="New team">＋</button></div>${teams.map((team) => `<button type="button" data-board-task="${esc(team.id)}" data-team-select="${esc(team.id)}" aria-pressed="${team.id === selectedTeam}" title="${esc(team.name)}"><span class="board-task-name">${esc(team.name)}</span><span class="fine">${team.members.length} agents${team.swarm ? " · Swarm" : ""}</span></button>`).join("")}</aside><section class="board-thread teams-detail">${detail}</section></div>`;
    root.querySelector("#teams-new").onclick = () => edit();
    root.querySelector("#teams-examples").onclick = examples;
    root.querySelectorAll("[data-team-select]").forEach(
      (button) =>
        (button.onclick = () => {
          selectedTeam = button.dataset.teamSelect;
          render();
        }),
    );
    if (!selected) return;
    root.querySelector("[data-edit-team]").onclick = () => edit(selected);
    root.querySelector("[data-new-task]").onclick = () =>
      host.newTask(selected);
    root.querySelector("[data-add-team]").onclick = () =>
      host.addTeam(selected);
    root.querySelector("[data-delete-team]").onclick = async () => {
      if (
        !(await host.confirm(
          `Delete team ${selected.name}?`,
          "Agents and running sessions remain unchanged.",
        ))
      )
        return;
      try {
        await host.api(`/teams/${selected.id}`, "DELETE");
        await host.reloadData();
        selectedTeam = "";
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
