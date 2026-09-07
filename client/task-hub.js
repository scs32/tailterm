// Task hub controller: owns the hub client, per-task event feeds, the mirror
// loop that keeps a task tab's panes in step with the hub's agent list, and
// the task dialogs reachable from the command palette.
import { createHubClient, normalizeHubURL } from "./hub-client.js";
import { normalizeTaskId, AGENT_NAME_RE } from "./task-ref.js";
import {
  reconcileTask,
  taskBinding,
  taskRollup,
  applyEvents,
  matchServer,
} from "./tasks.js";
import {
  agentSpawnCommand,
  agentRuntimesCommand,
} from "../shared/tmux-command.js";

const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const STATUS_DOT = {
  running: "online",
  needs_input: "attention",
  done: "done",
};

export function createTaskHub(host) {
  // host: {getIPN, getData, api, getTabs, getServers, currentTab, currentServer,
  //   connect, closeTab, activate, paneGroups, dialog, closeDialog, notice,
  //   browserCommand, render, scheduleWorkspaceSave, bookmark}
  let client = null;
  const feeds = new Map(); // taskId -> {stop, agents, task, unknown, cursor}
  const hidden = new Set();
  const bound = new Set(); // task ids mirrored into tabs
  const cache = new Map(); // taskId -> {task, agents} for tooltips/dialogs
  let tasksList = [];

  function refresh() {
    const url = host.getData()?.hub?.url;
    const ipn = host.getIPN();
    if (!url || !ipn) {
      client = null;
      stopAll();
      return null;
    }
    if (
      client?.base !== url ||
      client?.token !== (host.getData()?.hub?.token || "")
    ) {
      stopAll();
      client = createHubClient({
        fetchImpl: (u, init) => ipn.fetch(u, init),
        baseURL: url,
        token: host.getData()?.hub?.token || "",
      });
    }
    sync();
    return client;
  }
  const ready = () => !!client;

  function stopAll() {
    for (const feed of feeds.values()) feed.stop();
    feeds.clear();
  }

  // Ensure one feed per bound task; drop feeds whose task is no longer bound.
  function sync() {
    if (!client) return;
    for (const g of host.paneGroups()?.model.groups || [])
      if (g.taskId) bound.add(g.taskId);
    for (const id of bound) if (!feeds.has(id)) startFeed(id);
    for (const [id, feed] of feeds)
      if (!bound.has(id)) {
        feed.stop();
        feeds.delete(id);
      }
  }

  function forgetTask(taskId) {
    feeds.get(taskId)?.stop();
    feeds.delete(taskId);
    bound.delete(taskId);
    cache.delete(taskId);
    for (const group of host.paneGroups()?.model.groups || []) {
      if (group.taskId === taskId) delete group.taskId;
    }
    for (const tab of host.getTabs()) {
      if (tab.task?.taskId !== taskId) continue;
      hidden.delete(tab.task.agentId);
      delete tab.task;
      host.bookmark(tab);
    }
    host.clearTaskBookmarks?.(taskId);
    host.scheduleWorkspaceSave();
    host.render();
  }

  function startFeed(taskId) {
    const feed = {
      taskId,
      agents: [],
      unknown: 0,
      stop() {
        this.stopped = true;
      },
      stopped: false,
    };
    feeds.set(taskId, feed);
    (async () => {
      let detail;
      try {
        detail = await client.getTask(taskId);
      } catch (error) {
        if (feed.stopped) return;
        if (error.status === 404) {
          forgetTask(taskId);
          return;
        }
        if (!feed.error) host.notice(`Task hub: ${error.message}`);
        feed.error = error.message;
        if (!feed.stopped) {
          feeds.delete(taskId);
          setTimeout(() => {
            if (!feed.stopped && client && bound.has(taskId)) sync();
          }, 5000);
        }
        return;
      }
      if (feed.stopped) return;
      feed.task = detail.task;
      feed.agents = detail.agents;
      cache.set(taskId, { task: detail.task, agents: detail.agents });
      if (detail.task.status === "closed") {
        await reconcile(feed);
        return;
      }
      await reconcile(feed);
      if (feed.stopped) return;
      const subscription = client.subscribe(
        taskId,
        async (events) => {
          const { attention, refresh: refetch } = applyEvents(
            feed.agents,
            events,
          );
          if (refetch) {
            try {
              feed.agents = await client.listAgents(taskId);
              cache.set(taskId, { task: feed.task, agents: feed.agents });
            } catch {}
          }
          for (const [agentId, label] of attention) {
            const tab = host
              .getTabs()
              .find((t) => !t.disposed && t.task?.agentId === agentId);
            if (tab) tab.markActivity?.(label);
          }
          if (events.some((e) => e.kind === "task_updated")) {
            try {
              const detail = await client.getTask(taskId);
              feed.task = detail.task;
              cache.set(taskId, { task: feed.task, agents: feed.agents });
            } catch {}
          }
          if (events.some((e) => e.kind === "task_closed")) {
            feed.task = { ...feed.task, status: "closed" };
            cache.set(taskId, { task: feed.task, agents: feed.agents });
          }
          await reconcile(feed);
          host.render();
        },
        {
          after: detail.latestSeq,
          onError: (error) => {
            if (error.status === 404) {
              forgetTask(taskId);
              return;
            }
            feed.error = error.message;
          },
        },
      );
      feed.stop = () => {
        feed.stopped = true;
        subscription.stop();
      };
    })();
  }

  let reconciling = Promise.resolve();
  function reconcile(feed) {
    // Serialize: pane creation is async and two batches must not race.
    reconciling = reconciling.then(() => doReconcile(feed)).catch(() => {});
    return reconciling;
  }
  async function doReconcile(feed) {
    if (feed.stopped) return;
    const r = reconcileTask({
      taskId: feed.taskId,
      agents: feed.agents,
      tabs: host.getTabs(),
      servers: host.getServers(),
      hidden,
    });
    feed.unknown = r.unknown.length;
    for (const { tab, agent } of r.adopt) {
      tab.task = taskBinding(feed.taskId, agent);
      host.bookmark(tab);
    }
    host.paneGroups()?.sync();
    for (const tab of r.close) host.closeTab(tab.id, { fromHub: true });
    for (const { agent, server } of r.open) {
      if (feed.stopped) return;
      try {
        const tab = await host.connect(server, true, agent.session, {
          resumeOnly: true,
          task: taskBinding(feed.taskId, agent),
          quiet: true,
        });
        if (tab) place(feed.taskId, tab);
      } catch (error) {
        host.notice(`Could not open ${agent.name}: ${error.message}`);
      }
    }
    host.scheduleWorkspaceSave();
  }
  // Put a pane into the task's tab, or make its own tab carry the task.
  function place(taskId, tab) {
    const groups = host.paneGroups();
    groups.sync();
    const target = groups.model.taskGroup(taskId);
    const own = groups.model.group(tab.id);
    if (target && own !== target) {
      groups.merge(tab.id, target.active, false);
    } else if (!target && own) own.taskId = taskId;
    groups.sync();
    host.render();
  }

  function attach(taskId) {
    bound.add(taskId);
    sync();
    host.scheduleWorkspaceSave();
    host.render();
  }
  function detach(tabId) {
    const groups = host.paneGroups();
    const group = groups.model.group(tabId);
    if (!group?.taskId) return;
    const taskId = group.taskId;
    delete group.taskId;
    if (!groups.model.groups.some((g) => g.taskId === taskId)) {
      bound.delete(taskId);
      feeds.get(taskId)?.stop();
      feeds.delete(taskId);
    }
    host.scheduleWorkspaceSave();
    host.render();
  }
  function taskOfTab(tabId) {
    return host.paneGroups()?.model.group(tabId)?.taskId;
  }
  function rollup(taskId) {
    const feed = feeds.get(taskId);
    const info = cache.get(taskId);
    if (!info) return `Task ${taskId}`;
    const line = `Task ${info.task.name}: ${taskRollup(info.agents, feed?.unknown || 0)}`;
    return feed?.error ? `${line}\nHub: ${feed.error}` : line;
  }
  function restore(taskIds = [], hiddenIds = []) {
    for (const id of hiddenIds) hidden.add(id);
    for (const id of taskIds) if (normalizeTaskId(id)) bound.add(id);
    sync();
  }

  // Dialogs

  async function loadTasks() {
    tasksList = await client.listTasks();
    return tasksList;
  }
  function formatError(error) {
    return error?.message || String(error);
  }
  function requireHub() {
    if (!client) {
      configure();
      return false;
    }
    return true;
  }

  function configure() {
    const current = host.getData()?.hub?.url || "";
    host.dialog(
      "Task hub",
      `<p class="fine">Tasks and messages live on your coordination hub. Use its address on your private network. The access token is saved in your encrypted vault. Agent hosts also need this token in ~/.config/tailterm/hub.json.</p><label>Hub URL<input id="hub-url" value="${esc(current)}" placeholder="http://tailterm-hub" autocomplete="off" spellcheck="false"></label><label>Access token<input id="hub-token" type="password" value="${esc(host.getData()?.hub?.token || "")}" autocomplete="off" placeholder="Required for a network listener"></label><label>Agent host<select id="hub-config-server">${serverOptions(host.currentServer()?.id)}</select></label><button id="hub-load-host" type="button">Load configuration from server</button><p id="hub-status" class="fine">${current ? "Configured." : "Not configured."}</p><div class="dialog-actions"><button id="hub-test">Test connection</button><button id="hub-save" class="primary">Save</button>${current ? '<button id="hub-clear" class="danger">Remove</button>' : ""}</div>`,
    );
    const status = document.querySelector("#hub-status");
    document.querySelector("#hub-load-host").onclick = async () => {
      status.textContent = "Reading host configuration…";
      try {
        const server = host
          .getServers()
          .find(
            (s) => s.id === document.querySelector("#hub-config-server").value,
          );
        if (!server) throw new Error("Choose a saved server first.");
        const result = JSON.parse(
          await host.browserCommand(
            server,
            'cat "$HOME/.config/tailterm/hub.json"',
            4096,
          ),
        );
        if (
          !normalizeHubURL(result.url) ||
          typeof result.token !== "string" ||
          result.token.length > 512 ||
          /[\x00-\x20\x7f]/.test(result.token)
        )
          throw new Error("Invalid host configuration.");
        document.querySelector("#hub-url").value = result.url;
        document.querySelector("#hub-token").value = result.token;
        status.textContent =
          "Loaded. Test the connection, then Save to your encrypted vault.";
      } catch (error) {
        status.textContent = "Could not load: " + formatError(error);
      }
    };
    document.querySelector("#hub-test").onclick = async () => {
      status.textContent = "Connecting…";
      try {
        const ipn = host.getIPN();
        if (!ipn) throw new Error("Connect Tailscale first.");
        const probe = createHubClient({
          fetchImpl: (u, init) => ipn.fetch(u, init),
          baseURL: document.querySelector("#hub-url").value,
          token: document.querySelector("#hub-token").value.trim(),
        });
        const who = await probe.whoami();
        status.textContent = `Hub sees this browser as ${who.node || "?"} (${who.user || "no user"}).`;
      } catch (error) {
        status.textContent = "Failed: " + formatError(error);
      }
    };
    document.querySelector("#hub-save").onclick = async () => {
      try {
        await host.api("/hub", "POST", {
          url: document.querySelector("#hub-url").value,
          token: document.querySelector("#hub-token").value.trim(),
        });
        await host.reloadData();
        refresh();
        host.closeDialog();
        host.notice(client ? "Task hub saved." : "Task hub removed.");
      } catch (error) {
        status.textContent = formatError(error);
      }
    };
    const clear = document.querySelector("#hub-clear");
    if (clear)
      clear.onclick = async () => {
        await host.api("/hub", "POST", { url: "" });
        await host.reloadData();
        refresh();
        host.closeDialog();
      };
  }

  const serverOptions = (selectedId) =>
    host
      .getServers()
      .map(
        (s) =>
          `<option value="${esc(s.id)}" ${s.id === selectedId ? "selected" : ""}>${esc(s.name)} · ${esc(s.username)}@${esc(s.host)}</option>`,
      )
      .join("");
  const runtimeOptions = (server) => {
    const known = server?.runtimes?.length ? server.runtimes : [];
    const presets = [
      ["claude", "claude"],
      ["codex", "codex"],
      ["aider", "aider"],
      ["gemini", "gemini"],
    ];
    const list = [
      ...known.map((r) => [r, r]),
      ...presets.filter(([r]) => !known.includes(r)),
    ];
    return (
      list
        .map(
          ([value, label], i) =>
            `<option value="${esc(value)}" ${i === 0 ? "selected" : ""}>${esc(label)}${known.includes(value) ? " · installed" : ""}</option>`,
        )
        .join("") + `<option value="">Custom command…</option>`
    );
  };

  function agentFields(server) {
    return `<div class="appearance-controls"><label>Agent name<input id="agent-name" value="agent1" maxlength="64" autocomplete="off" spellcheck="false"></label><label>Runtime<select id="agent-runtime">${runtimeOptions(server)}</select></label></div><label>Assignment<textarea id="agent-prompt" rows="2" placeholder="Optional instructions for this agent"></textarea></label><details class="dialog-details"><summary>Launch options &amp; profiles</summary><label>Launch profile<select id="agent-profile"><option value="">Custom setup</option>${(host.getData().launchProfiles || []).map((p) => `<option value="${esc(p.name)}">${esc(p.name)}</option>`).join("")}</select></label><label>Command<input id="agent-run" placeholder="claude" autocomplete="off" spellcheck="false"></label><label>Working directory<input id="agent-cwd" placeholder="/absolute/project/path (optional)" autocomplete="off" spellcheck="false"></label><button id="agent-save-profile" type="button">Save launch profile</button></details>`;
  }
  function readAgentFields() {
    const runtime = document.querySelector("#agent-runtime").value;
    const run = document.querySelector("#agent-run").value.trim() || runtime;
    return {
      name: document.querySelector("#agent-name").value.trim(),
      runtime: runtime || "generic",
      run,
      cwd: document.querySelector("#agent-cwd").value.trim(),
      prompt: document.querySelector("#agent-prompt").value.trim(),
    };
  }
  function wireAgentFields() {
    const runtime = document.querySelector("#agent-runtime"),
      run = document.querySelector("#agent-run");
    const update = () => {
      run.placeholder = runtime.value || "your-agent --flag";
      if (runtime.value && (!run.value || run.dataset.auto === "true")) {
        run.value = runtime.value;
        run.dataset.auto = "true";
      }
    };
    runtime.onchange = update;
    run.oninput = () => (run.dataset.auto = "false");
    update();
    const profiles = host.getData().launchProfiles || [];
    document.querySelector("#agent-profile").onchange = (e) => {
      const p = profiles.find((p) => p.name === e.target.value);
      if (!p) return;
      const selector = document.querySelector("#task-server");
      selector.value = p.serverId;
      selector.dispatchEvent(new Event("change"));
      document.querySelector("#agent-profile").value = p.name;
      document.querySelector("#agent-runtime").value =
        p.runtime === "generic" ? "" : p.runtime;
      document.querySelector("#agent-run").value = p.run;
      document.querySelector("#agent-run").dataset.auto = "false";
      document.querySelector("#agent-cwd").value = p.cwd;
    };
    const save = document.querySelector("#agent-save-profile");
    if (save)
      save.onclick = async () => {
        try {
          const f = readAgentFields();
          await host.api("/launch-profiles", "POST", {
            ...f,
            serverId: document.querySelector("#task-server").value,
          });
          await host.reloadData();
          host.notice(`Saved launch profile ${f.name}.`);
        } catch (e) {
          host.notice(e.message);
        }
      };
  }

  async function spawn(taskId, server, fields) {
    if (!AGENT_NAME_RE.test(fields.name))
      throw new Error(
        "Agent name: 1–64 letters, numbers, dashes or underscores.",
      );
    const command = agentSpawnCommand({
      hub: client.base,
      task: taskId,
      name: fields.name,
      run: fields.run,
      cwd: fields.cwd,
      prompt: fields.prompt,
      runtime: fields.runtime,
    });
    const out = await host.browserCommand(server, command, 65536);
    let agent;
    try {
      agent = JSON.parse(out);
    } catch {
      throw new Error(
        "tt spawn returned unexpected output: " + out.slice(0, 200),
      );
    }
    if (!agent?.id) throw new Error("tt spawn did not return an agent.");
    return agent;
  }

  function newTask(tabId = host.currentTab()?.id) {
    if (!requireHub()) return;
    const server = host.currentServer() || host.getServers()[0];
    host.dialog(
      "New task",
      `<form id="task-form" novalidate>
      <label>Task name<input id="task-name" placeholder="Review the API changes" maxlength="120" autocomplete="off" required></label>
      <label>Objective<textarea id="task-goal" rows="3" maxlength="8192" placeholder="What should be accomplished?"></textarea></label>
      <label class="check"><input type="checkbox" id="task-with-agent">Start the first agent now</label>
      <fieldset id="task-agent-fields" hidden disabled><label>Server<select id="task-server">${serverOptions(server?.id)}</select></label>${agentFields(server)}</fieldset>
      <label class="check"><input type="checkbox" id="task-allow-spawn">Allow agents to add other agents</label>
      <p class="fine">Agents appear together in a terminal group named after this task.</p>
      <div class="task-submit-area"><p id="task-error" class="fine" role="status" aria-live="polite"></p><div class="dialog-actions"><button type="button" id="task-open-created" hidden>Open created task</button><button type="submit" id="task-create" class="primary">Create task</button></div></div>
    </form>`,
    );
    const form = document.querySelector("#task-form"),
      error = form.querySelector("#task-error"),
      button = form.querySelector("#task-create"),
      withAgent = form.querySelector("#task-with-agent"),
      agentFieldsEl = form.querySelector("#task-agent-fields");
    let pending = false,
      saved = null;
    const open = () => {
      host.closeDialog();
      host.openBoard(saved.id);
    };
    form.querySelector("#task-open-created").onclick = open;
    withAgent.onchange = () => {
      agentFieldsEl.hidden = !withAgent.checked;
      agentFieldsEl.disabled = !withAgent.checked;
      button.textContent = withAgent.checked
        ? "Create task and start agent"
        : "Create task";
    };
    form.onsubmit = async (event) => {
      event.preventDefault();
      if (pending) return;
      error.textContent = "";
      try {
        const name = form.querySelector("#task-name").value.trim();
        if (!name || [...name].length > 120 || /[\x00-\x1f\x7f]/.test(name))
          throw new Error("Enter a task name of up to 120 characters.");
        const target = host
          .getServers()
          .find((s) => s.id === form.querySelector("#task-server").value);
        const fields = withAgent.checked ? readAgentFields() : null;
        if (fields) {
          if (!target) throw new Error("Choose a server for the first agent.");
          // Validate all launch input before creating persistent task records.
          agentSpawnCommand({
            hub: client.base,
            task: "tsk_0000000000000000",
            ...fields,
          });
        }
        pending = true;
        button.disabled = true;
        withAgent.disabled = true;
        error.textContent = saved ? "Retrying agent launch…" : "Creating task…";
        if (!saved) {
          saved = await client.createTask({
            name,
            goal: form.querySelector("#task-goal").value.trim(),
            allowAgentSpawn: form.querySelector("#task-allow-spawn").checked,
          });
          bound.add(saved.id);
          cache.set(saved.id, { task: saved, agents: [] });
          form.querySelector("#task-allow-spawn").disabled = true;
          form.querySelector("#task-name").disabled = true;
          form.querySelector("#task-goal").disabled = true;
          form.querySelector("#task-open-created").hidden = false;
        }
        if (fields) {
          error.textContent = `Task created. Starting ${fields.name} on ${target.name}…`;
          await spawn(saved.id, target, fields);
        }
        sync();
        open();
        host.notice(
          fields
            ? `Task created; ${fields.name} is starting.`
            : "Task created.",
        );
      } catch (e) {
        error.textContent =
          (saved ? "Task created. Agent launch failed: " : "") + formatError(e);
        error.setAttribute("role", "alert");
        error.scrollIntoView({ block: "nearest" });
        button.textContent = saved
          ? "Retry agent launch"
          : withAgent.checked
            ? "Create task and start agent"
            : "Create task";
      } finally {
        pending = false;
        button.disabled = false;
        withAgent.disabled = !!saved;
      }
    };
    wireAgentFields();
    form.querySelector("#task-server").onchange = (e) => {
      const target = host.getServers().find((s) => s.id === e.target.value);
      form.querySelector("#agent-runtime").innerHTML = runtimeOptions(target);
      wireAgentFields();
    };
    form.querySelector("#task-name").focus();
  }

  async function attachTask() {
    if (!requireHub()) return;
    try {
      const tasks = (await loadTasks()).filter((t) => t.status === "open");
      host.dialog(
        "Task terminals",
        `<div class="dialog-menu">${tasks.map((t) => `<button data-open-task="${esc(t.id)}">${esc(t.name)}</button>`).join("") || '<p class="fine">No open tasks.</p>'}</div>`,
      );
      document.querySelectorAll("[data-open-task]").forEach(
        (b) =>
          (b.onclick = () => {
            host.closeDialog();
            attachToCurrent(b.dataset.openTask);
          }),
      );
    } catch (error) {
      host.notice(formatError(error));
    }
  }

  function addAgent(taskId = taskOfTab(host.currentTab()?.id)) {
    if (!requireHub()) return;
    if (!taskId) {
      host.notice("Choose a task first.");
      return;
    }
    const info = cache.get(taskId);
    const server = host.currentServer() || host.getServers()[0];
    host.dialog(
      `Add agent · ${info?.task?.name || taskId}`,
      `<label>Server<select id="task-server">${serverOptions(server?.id)}</select></label>${agentFields(server)}<p id="task-error" class="fine" role="alert"></p><div class="dialog-actions"><button id="agent-start" class="primary">Start agent</button></div>`,
    );
    wireAgentFields();
    document.querySelector("#task-server").onchange = (e) => {
      const s = host.getServers().find((x) => x.id === e.target.value);
      document.querySelector("#agent-runtime").innerHTML = runtimeOptions(s);
      wireAgentFields();
    };
    const startButton = document.querySelector("#agent-start");
    let nextName = 1;
    while (
      info?.agents?.some(
        (a) =>
          a.name === `agent${nextName}` &&
          !["closed", "exited"].includes(a.status),
      )
    )
      nextName++;
    document.querySelector("#agent-name").value = `agent${nextName}`;
    startButton.onclick = async () => {
      if (startButton.disabled) return;
      const error = document.querySelector("#task-error");
      const target = host
        .getServers()
        .find((s) => s.id === document.querySelector("#task-server").value);
      try {
        const fields = readAgentFields();
        if (!target) throw new Error("Choose a server for the agent.");
        startButton.disabled = true;
        error.textContent = `Starting ${fields.name} on ${target.name}…`;
        const agent = await spawn(taskId, target, fields);
        host.closeDialog();
        host.notice(`${agent.name} is starting on ${target.name}.`);
      } catch (e) {
        error.textContent = formatError(e);
        error.scrollIntoView({ block: "nearest" });
      } finally {
        startButton.disabled = false;
      }
    };
  }

  // The board lives in Board mode; the host switches modes and selects the task.
  function board(taskId = taskOfTab(host.currentTab()?.id)) {
    if (!requireHub()) return;
    host.openBoard(taskId || null);
  }
  async function attachToCurrent(taskId) {
    if (!requireHub()) return;
    try {
      const detail = await client.getTask(taskId);
      cache.set(taskId, detail);
      for (const agent of detail.agents) hidden.delete(agent.id);
      attach(taskId);
      const feed = feeds.get(taskId);
      if (feed) {
        feed.task = detail.task;
        feed.agents = detail.agents;
        await reconcile(feed);
      }
      const group = groupOf(taskId);
      if (group) {
        host.showTerminals?.();
        host.activate?.(group.active);
      } else if (detail.agents.some((a) => a.status !== "closed"))
        host.notice(
          "Agent terminals are starting or their servers are unavailable.",
        );
      else addAgent(taskId);
    } catch (error) {
      host.notice(formatError(error));
    }
  }
  async function settings(taskId) {
    if (!requireHub()) return;
    try {
      const { task } = await client.getTask(taskId);
      host.dialog(
        "Task settings",
        `<form id="task-settings"><label>Task name<input id="task-settings-name" maxlength="120" value="${esc(task.name)}" required></label><label>Objective<textarea id="task-settings-goal" rows="3" maxlength="8192">${esc(task.goal)}</textarea></label><label class="check"><input id="task-settings-spawn" type="checkbox" ${task.allowAgentSpawn ? "checked" : ""}>Allow agents to add other agents</label><p class="fine">You can always add agents yourself. Turning this off prevents new helpers; existing agents keep running.</p><p id="task-settings-error" class="fine" role="alert"></p><div class="dialog-actions"><button type="submit" class="primary">Save</button></div></form>`,
      );
      const form = document.querySelector("#task-settings");
      form.onsubmit = async (event) => {
        event.preventDefault();
        const button = form.querySelector("button");
        if (button.disabled) return;
        button.disabled = true;
        try {
          const task = await client.updateTask(taskId, {
            name: form.querySelector("#task-settings-name").value.trim(),
            goal: form.querySelector("#task-settings-goal").value.trim(),
            allowAgentSpawn: form.querySelector("#task-settings-spawn").checked,
          });
          const info = cache.get(taskId);
          cache.set(taskId, { ...info, task });
          const feed = feeds.get(taskId);
          if (feed) feed.task = task;
          host.closeDialog();
          host.render();
          host.notice("Task settings saved.");
        } catch (error) {
          form.querySelector("#task-settings-error").textContent =
            formatError(error);
        } finally {
          button.disabled = false;
        }
      };
    } catch (error) {
      host.notice(formatError(error));
    }
  }
  function groupOf(taskId) {
    return host.paneGroups()?.model.taskGroup(taskId) || null;
  }

  async function refreshRuntimes(server) {
    if (!client) return null;
    const out = await host.browserCommand(
      server,
      agentRuntimesCommand(),
      65536,
    );
    const parsed = JSON.parse(out);
    if (parsed.missing) return null;
    return parsed.runtimes || [];
  }

  function commands() {
    const list = [];
    const tab = host.currentTab();
    const taskId = tab && taskOfTab(tab.id);
    list.push({ label: "Task hub: configure", run: configure });
    if (client) {
      list.push({ label: "Task: new…", run: () => newTask() });
      list.push({
        label: "Task: open terminals…",
        run: () => attachTask(),
      });
      if (taskId) {
        const name = cache.get(taskId)?.task?.name || taskId;
        list.push({ label: `Task ${name}: board`, run: () => board(taskId) });
        list.push({
          label: `Task ${name}: add agent…`,
          run: () => addAgent(taskId),
        });
        list.push({
          label: `Task ${name}: settings`,
          run: () => settings(taskId),
        });
      }
    }
    return list;
  }

  return {
    hideAgent(id) {
      hidden.add(id);
      host.scheduleWorkspaceSave();
    },
    revealAgent(id) {
      hidden.delete(id);
      for (const f of feeds.values()) reconcile(f);
      host.scheduleWorkspaceSave();
    },
    hidden: () => [...hidden],
    client: () => client,
    refresh,
    ready,
    sync,
    groupOf,
    attachToCurrent,
    settings,
    name: (id) => cache.get(id)?.task?.name,
    stopAll,
    attach,
    detach,
    restore,
    rollup,
    taskOfTab,
    bound: () => [...bound],
    configure,
    newTask,
    attachTask,
    addAgent,
    board,
    commands,
    refreshRuntimes,
    matchServer: (h) => matchServer(h, host.getServers()),
  };
}
