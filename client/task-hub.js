// Task hub controller: owns the hub client, per-task event feeds, the mirror
// loop that keeps a task tab's panes in step with the hub's agent list, and
// the task dialogs reachable from the command palette.
import { createHubClient } from "./hub-client.js";
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
    if (client?.base !== url) {
      stopAll();
      client = createHubClient({
        fetchImpl: (u, init) => ipn.fetch(u, init),
        baseURL: url,
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

  function startFeed(taskId) {
    const feed = {
      taskId,
      agents: [],
      unknown: 0,
      stop: () => {},
      stopped: false,
    };
    feeds.set(taskId, feed);
    (async () => {
      let detail;
      try {
        detail = await client.getTask(taskId);
      } catch (error) {
        host.notice(`Task hub: ${error.message}`);
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
          if (events.some((e) => e.kind === "task_closed")) {
            feed.task = { ...feed.task, status: "closed" };
            cache.set(taskId, { task: feed.task, agents: feed.agents });
          }
          await reconcile(feed);
          host.render();
          boardRefresh?.(taskId, events);
        },
        {
          after: detail.latestSeq,
          onError: (error) => {
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
    });
    feed.unknown = r.unknown.length;
    for (const { tab, agent } of r.adopt) {
      tab.task = taskBinding(feed.taskId, agent);
      host.bookmark(tab);
    }
    for (const tab of r.close) host.closeTab(tab.id);
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

  function attach(taskId, tabId) {
    const groups = host.paneGroups();
    const group = groups.model.group(tabId);
    if (!group) return;
    for (const g of groups.model.groups)
      if (g.taskId === taskId) delete g.taskId;
    group.taskId = taskId;
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
  function restore(taskIds = []) {
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
      `<p class="fine">Tasks, agents, and the message board live on a hub that runs as its own Tailscale node. Tailterm reaches it through this browser’s Tailscale connection, so use the hub’s tailnet name.</p><label>Hub URL<input id="hub-url" value="${esc(current)}" placeholder="http://tailterm-hub" autocomplete="off" spellcheck="false"></label><p id="hub-status" class="fine">${current ? "Configured." : "Not configured."}</p><div class="dialog-actions"><button id="hub-test">Test connection</button><button id="hub-save" class="primary">Save</button>${current ? '<button id="hub-clear" class="danger">Remove</button>' : ""}</div>`,
    );
    const status = document.querySelector("#hub-status");
    document.querySelector("#hub-test").onclick = async () => {
      status.textContent = "Connecting…";
      try {
        const ipn = host.getIPN();
        if (!ipn) throw new Error("Connect Tailscale first.");
        const probe = createHubClient({
          fetchImpl: (u, init) => ipn.fetch(u, init),
          baseURL: document.querySelector("#hub-url").value,
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
    return `<div class="appearance-controls"><label>Agent name<input id="agent-name" value="agent1" maxlength="64" autocomplete="off" spellcheck="false"></label><label>Runtime<select id="agent-runtime">${runtimeOptions(server)}</select></label></div><label>Command<input id="agent-run" placeholder="claude" autocomplete="off" spellcheck="false"></label><p class="fine">Runs inside the agent’s tmux window through <code>tt wrap</code>, so any command reports started and done. Claude Code and Codex get richer status through <code>tt hooks</code>.</p><label>Working directory<input id="agent-cwd" placeholder="/home/ubuntu/project (optional)" autocomplete="off" spellcheck="false"></label><label>Prompt<textarea id="agent-prompt" rows="3" placeholder="Optional. Appended to the command as its first argument."></textarea></label>`;
  }
  function readAgentFields() {
    const runtime = document.querySelector("#agent-runtime").value;
    const run = document.querySelector("#agent-run").value.trim() || runtime;
    return {
      name: document.querySelector("#agent-name").value.trim(),
      runtime: runtime || run.split(/\s+/)[0],
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
      `<div class="appearance-controls"><label>Task name<input id="task-name" placeholder="refactor-auth" maxlength="64" autocomplete="off" spellcheck="false"></label><label>First agent on<select id="task-server">${serverOptions(server?.id)}</select></label></div><label>Goal<textarea id="task-goal" rows="2" placeholder="What the team is working toward (optional)"></textarea></label>${agentFields(server)}<label class="check"><input type="checkbox" id="task-attach" ${tabId ? "checked" : ""}>Attach the task to the current tab</label><p id="task-error" class="fine" role="alert"></p><div class="dialog-actions"><button id="task-create" class="primary">Create task and start agent</button><button id="task-create-only">Create task only</button></div>`,
    );
    wireAgentFields();
    document.querySelector("#task-server").onchange = (e) => {
      const s = host.getServers().find((x) => x.id === e.target.value);
      document.querySelector("#agent-runtime").innerHTML = runtimeOptions(s);
      wireAgentFields();
    };
    const error = document.querySelector("#task-error");
    const create = async (withAgent) => {
      error.textContent = "";
      const name = document.querySelector("#task-name").value.trim();
      if (!AGENT_NAME_RE.test(name)) {
        error.textContent =
          "Task name: 1–64 letters, numbers, dashes or underscores.";
        return;
      }
      const serverId = document.querySelector("#task-server").value;
      const target = host.getServers().find((s) => s.id === serverId);
      const attachHere =
        document.querySelector("#task-attach").checked && tabId;
      const fields = withAgent ? readAgentFields() : null;
      try {
        if (withAgent && !AGENT_NAME_RE.test(fields.name))
          throw new Error(
            "Agent name: 1–64 letters, numbers, dashes or underscores.",
          );
        error.textContent = "Creating task…";
        const task = await client.createTask({
          name,
          goal: document.querySelector("#task-goal").value.trim(),
        });
        bound.add(task.id);
        if (attachHere) attach(task.id, tabId);
        if (withAgent) {
          error.textContent = `Starting ${fields.name} on ${target.name}…`;
          await spawn(task.id, target, fields);
        }
        sync();
        host.closeDialog();
        host.notice(
          withAgent
            ? `Task ${task.name} created; ${fields.name} is starting.`
            : `Task ${task.name} created.`,
        );
      } catch (e) {
        error.textContent = formatError(e);
      }
    };
    document.querySelector("#task-create").onclick = () => create(true);
    document.querySelector("#task-create-only").onclick = () => create(false);
  }

  async function attachTask(tabId = host.currentTab()?.id) {
    if (!requireHub() || !tabId) return;
    host.dialog("Attach task", `<p class="fine">Loading tasks…</p>`);
    let tasks;
    try {
      tasks = (await loadTasks()).filter((t) => t.status === "open");
    } catch (e) {
      host.dialog("Attach task", `<p class="fine">${esc(formatError(e))}</p>`);
      return;
    }
    const current = taskOfTab(tabId);
    host.dialog(
      "Attach task",
      `<p class="fine">The tab mirrors every agent on the task: new agent sessions open as panes here and closed ones leave.</p><div class="dialog-menu">${
        tasks.length
          ? tasks
              .map(
                (t) =>
                  `<button data-attach-task="${esc(t.id)}" ${t.id === current ? 'aria-pressed="true"' : ""}>${esc(t.name)}<span class="fine">${esc(t.goal || t.id)}</span></button>`,
              )
              .join("")
          : '<p class="fine">No open tasks yet.</p>'
      }<button id="attach-new-task">New task…</button>${current ? '<button id="attach-detach" class="danger">Detach current task</button>' : ""}</div>`,
    );
    document.querySelectorAll("[data-attach-task]").forEach(
      (b) =>
        (b.onclick = () => {
          attach(b.dataset.attachTask, tabId);
          host.closeDialog();
        }),
    );
    document.querySelector("#attach-new-task").onclick = () => newTask(tabId);
    const detachButton = document.querySelector("#attach-detach");
    if (detachButton)
      detachButton.onclick = () => {
        detach(tabId);
        host.closeDialog();
      };
  }

  function addAgent(taskId = taskOfTab(host.currentTab()?.id)) {
    if (!requireHub()) return;
    if (!taskId) {
      host.notice("Attach a task to this tab first.");
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
    document.querySelector("#agent-start").onclick = async () => {
      const error = document.querySelector("#task-error");
      const target = host
        .getServers()
        .find((s) => s.id === document.querySelector("#task-server").value);
      try {
        const fields = readAgentFields();
        error.textContent = `Starting ${fields.name} on ${target.name}…`;
        const agent = await spawn(taskId, target, fields);
        host.closeDialog();
        host.notice(`${agent.name} is starting on ${target.name}.`);
      } catch (e) {
        error.textContent = formatError(e);
      }
    };
  }

  let boardRefresh = null;
  async function board(taskId = taskOfTab(host.currentTab()?.id)) {
    if (!requireHub()) return;
    if (!taskId) {
      host.notice("Attach a task to this tab first.");
      return;
    }
    host.dialog("Task board", `<p class="fine">Loading…</p>`);
    let detail, messages;
    try {
      [detail, messages] = await Promise.all([
        client.getTask(taskId),
        client.listMessages(taskId, { limit: 200 }),
      ]);
    } catch (e) {
      host.dialog("Task board", `<p class="fine">${esc(formatError(e))}</p>`);
      return;
    }
    cache.set(taskId, { task: detail.task, agents: detail.agents });
    const names = new Map(detail.agents.map((a) => [a.id, a.name]));
    // The feed and a post response can both deliver the same message.
    const addMessages = (list) => {
      const seen = new Set(messages.map((m) => m.seq));
      for (const m of list)
        if (!seen.has(m.seq)) {
          messages.push(m);
          seen.add(m.seq);
        }
      messages.sort((a, b) => a.seq - b.seq);
    };
    const render = () => {
      const agentsHTML = detail.agents
        .map(
          (a) =>
            `<button class="board-agent" data-board-agent="${esc(a.id)}" title="${esc(a.host)} · ${esc(a.session)}"><span class="status-dot ${STATUS_DOT[a.status] || ""}"></span><span>${esc(a.name)}</span><span class="fine">${esc(a.status.replace("_", " "))}${a.unread ? ` · ${a.unread} unread` : ""}</span></button>`,
        )
        .join("");
      const thread = messages.length
        ? messages
            .map((m) => {
              const from = m.from.agentId
                ? names.get(m.from.agentId) || m.from.agentId
                : m.from.user || "you";
              const to = m.to ? ` → ${names.get(m.to) || m.to}` : "";
              return `<div class="board-message"><span class="board-meta">${esc(from)}${esc(to)} · ${esc(new Date(m.createdAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }))}</span><div class="board-text">${esc(m.text)}</div></div>`;
            })
            .join("")
        : '<p class="fine">No messages yet.</p>';
      host.dialog(
        `Task board · ${detail.task.name}`,
        `<div class="board"><aside class="board-agents"><span class="eyebrow">AGENTS</span>${agentsHTML || '<p class="fine">No agents.</p>'}<button id="board-add-agent">＋ Add agent</button></aside><section class="board-thread"><div id="board-messages" class="board-messages">${thread}</div><form id="board-compose"><select id="board-to"><option value="">Everyone</option>${detail.agents
          .filter((a) => a.status !== "closed")
          .map((a) => `<option value="${esc(a.id)}">${esc(a.name)}</option>`)
          .join(
            "",
          )}</select><textarea id="board-text" rows="2" placeholder="Message the task…" maxlength="8192"></textarea><button class="primary" type="submit">Post</button></form></section></div><p class="fine">${esc(detail.task.goal || "")}</p>`,
      );
      const list = document.querySelector("#board-messages");
      list.scrollTop = list.scrollHeight;
      document.querySelectorAll("[data-board-agent]").forEach((b) => {
        b.onclick = () => {
          const tab = host
            .getTabs()
            .find(
              (t) => !t.disposed && t.task?.agentId === b.dataset.boardAgent,
            );
          if (tab) {
            host.closeDialog();
            host.activate(tab.id);
          }
        };
      });
      document.querySelector("#board-add-agent").onclick = () =>
        addAgent(taskId);
      document.querySelector("#board-compose").onsubmit = async (e) => {
        e.preventDefault();
        const text = document.querySelector("#board-text").value.trim();
        if (!text) return;
        try {
          const m = await client.postMessage(taskId, {
            text,
            to: document.querySelector("#board-to").value,
          });
          addMessages([m]);
          document.querySelector("#board-text").value = "";
          render();
        } catch (error) {
          host.notice("Post failed: " + formatError(error));
        }
      };
    };
    render();
    boardRefresh = async (id, events) => {
      if (id !== taskId || !document.querySelector("#board-compose")) {
        if (!document.querySelector("#board-compose")) boardRefresh = null;
        return;
      }
      try {
        const last = messages.at(-1)?.seq || 0;
        const [fresh, agents] = await Promise.all([
          client.listMessages(taskId, { after: last, limit: 200 }),
          events.some((e) => e.kind !== "message")
            ? client.listAgents(taskId)
            : detail.agents,
        ]);
        addMessages(fresh);
        detail.agents = agents;
        for (const a of agents) names.set(a.id, a.name);
        render();
      } catch {}
    };
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
        label: "Task: attach to this tab…",
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
          label: `Task ${name}: detach from this tab`,
          run: () => detach(tab.id),
        });
      }
    }
    return list;
  }

  return {
    refresh,
    ready,
    sync,
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
