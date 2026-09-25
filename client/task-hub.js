import { projectFolderHTML, wireProjectFolder } from "./project-folder.js";
import { agentControlsHTML, wireAgentControls } from "./agent-controls.js";
import { agentToolsCommand } from "../shared/tmux-command.js";
import { modelPickerHTML, wireModelPicker } from "./model-picker.js";
import { resolveTeam, teamLaunches } from "./teams.js";
import { teamLaunchPlan } from "./team-launch-plan.js";
import { withDatabaseHandler } from "./project-handler.js";
import {
  assertCurrentWorkOrderScope,
  prepareWorkItemContext,
} from "./work-item-context.js";
import { serializedWorkContext } from "../shared/work-context.js";
import {
  launchPlanForStorage,
  restoreLaunchMembers,
  serverLaunchScope,
} from "./launch-journal.js";
import {
  guardedLaunchEffect,
  reconciledAgentProblem,
} from "./launch-reconciliation.js";
// Project hub controller: owns the hub client, per-task event feeds, the mirror
// loop that keeps a project tab's panes in step with the hub's agent list, and
// the project dialogs reachable from the command palette.
import { createHubClient, normalizeHubURL } from "./hub-client.js";
import { createCachedHubClient } from "./cached-hub-client.js";
import { normalizeTaskId, AGENT_NAME_RE } from "./task-ref.js";
import {
  assertPauseEvidence,
  assertPauseSnapshot,
  assertProjectLifecycle,
  assertResumeEvidence,
  assertResumeSnapshot,
  supportsProjectPauseV1,
} from "./project-pause.js";
import {
  reconcileTask,
  taskBinding,
  taskRollup,
  applyEvents,
  matchServer,
  openAgent,
} from "./tasks.js";
import {
  agentCleanupCommand,
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

export function addTeamOrchestrator(task, agents, plan) {
  if (
    task.orchestrator &&
    agents.some(
      (agent) => agent.name === task.orchestrator && agent.status !== "closed",
    )
  )
    return null;
  const lead = plan[0]?.fields.name;
  if (!lead) throw new Error("The prepared team has no lead to assign.");
  return lead;
}

export function createTaskHub(host) {
  // host: {getIPN, getData, api, getTabs, getServers, currentTab, currentServer,
  //   connect, closeTab, activate, paneGroups, dialog, closeDialog, notice,
  //   browserCommand, render, scheduleWorkspaceSave, bookmark}
  let client = null,
    viewClient = null,
    connected = false;
  const feeds = new Map(); // taskId -> {stop, agents, task, unknown, cursor}
  const hidden = new Set();
  const bound = new Set(); // task ids mirrored into tabs
  const cache = new Map(); // taskId -> {task, agents} for tooltips/dialogs
  let tasksList = [];
  let launchEpoch = 0;
  const serverHosts = new Map();
  const launchScopeSource = () => {
    const data = host.getData();
    return JSON.stringify([
      client?.base || "",
      data?.hub?.token || "",
      data?.profile?.username || "",
      data?.profile?.instanceId || "",
    ]);
  };
  async function launchScope() {
    const input = launchScopeSource();
    const digest = await crypto.subtle.digest(
      "SHA-256",
      new TextEncoder().encode(input),
    );
    if (input !== launchScopeSource())
      throw new Error(
        "The hub credential or profile changed. Reopen the launch dialog.",
      );
    return [...new Uint8Array(digest)]
      .map((byte) => byte.toString(16).padStart(2, "0"))
      .join("");
  }
  async function prepareLaunchJournal(
    kind,
    taskId,
    plan,
    teamId = "",
    creation = undefined,
    lead = undefined,
    resume = undefined,
  ) {
    const now = new Date().toISOString();
    for (const entry of plan) {
      entry.fields.agentId ||=
        "agt_" + crypto.randomUUID().replaceAll("-", "").slice(0, 16);
      if (host.launchServerProfile)
        entry.server = host.launchServerProfile(entry.server.id);
      entry.serverScope = await serverLaunchScope(entry.server);
    }
    const journal = launchPlanForStorage({
      id: "launch_" + crypto.randomUUID().replaceAll("-", ""),
      kind,
      scope: await launchScope(),
      taskId: taskId || undefined,
      teamId: teamId || undefined,
      createdAt: now,
      updatedAt: now,
      members: plan.map((entry) => ({ ...entry, state: "unstarted" })),
      ...(creation && { creation }),
      ...(lead && { lead }),
      ...(resume && { resume }),
    });
    await guardedJournalEffect(journal, null, () =>
      host.api("/team-launch-plans/validate", "POST", journal),
    );
    return journal;
  }
  async function assertLaunchScope(journal, entry = null) {
    if (!journal) return;
    if ((await launchScope()) !== journal.scope)
      throw new Error(
        "The hub credential or profile changed. Reopen the launch dialog.",
      );
    const members = entry
      ? [
          journal.members.find(
            (member) => member.fields.agentId === entry.fields.agentId,
          ),
        ]
      : journal.members;
    for (const member of members) {
      if (!member?.serverScope)
        throw new Error(
          "The frozen launch plan does not contain an exact machine scope.",
        );
      const current = host.launchServerProfile
        ? host.launchServerProfile(member.serverId)
        : host.getServers().find((server) => server.id === member.serverId);
      if (
        !current ||
        (await serverLaunchScope(current)) !== member.serverScope ||
        (entry &&
          (await serverLaunchScope(entry.server)) !== member.serverScope)
      )
        throw new Error(
          `The saved machine profile for ${member.fields.name} changed. Restore the exact endpoint and credentials or discard the frozen plan.`,
        );
    }
  }
  async function guardedJournalEffect(journal, entry, effect) {
    const epoch = launchEpoch;
    return guardedLaunchEffect(async () => {
      if (epoch !== launchEpoch)
        throw new Error(
          "The launch view changed. Reopen it before continuing.",
        );
      await assertLaunchScope(journal, entry);
      if (epoch !== launchEpoch)
        throw new Error(
          "The launch view changed. Reopen it before continuing.",
        );
    }, effect);
  }
  const saveLaunchJournal = (journal) =>
    guardedJournalEffect(journal, null, () =>
      host.api("/team-launch-plans", "POST", {
        ...journal,
        updatedAt: new Date().toISOString(),
      }),
    );
  async function amendUnstartedFolders(plan, journal, requestedFolders) {
    const nextJournal = structuredClone(journal);
    let changed = false;
    const amended = plan.map((entry) => {
      const member = nextJournal?.members.find(
        (candidate) => candidate.fields.agentId === entry.fields.agentId,
      );
      if (!member || member.state !== "unstarted") return entry;
      if (!Object.hasOwn(requestedFolders, entry.fields.agentId)) return entry;
      const cwd = String(requestedFolders[entry.fields.agentId] || "").trim();
      if (cwd === entry.fields.cwd) return entry;
      agentSpawnCommand({
        hub: client.base,
        task: journal.taskId || "tsk_0000000000000000",
        ...entry.fields,
        cwd,
      });
      const receipt = {
        agentId: entry.fields.agentId,
        from: entry.fields.cwd,
        to: cwd,
        amendedAt: new Date().toISOString(),
      };
      member.fields.cwd = cwd;
      member.folderAmendments = [...(member.folderAmendments || []), receipt];
      changed = true;
      return {
        ...entry,
        fields: { ...entry.fields, cwd },
        folderAmendments: structuredClone(member.folderAmendments),
      };
    });
    if (changed) {
      await saveLaunchJournal(nextJournal);
      Object.assign(journal, nextJournal);
    }
    return amended;
  }
  const probedServers = new Set();
  const serverKey = (s) => JSON.stringify([s.id, s.host, s.port, s.username]);
  function rememberHost(server, name) {
    if (!name || typeof name !== "string" || name.length > 255) return;
    const key = serverKey(server);
    if (!serverHosts.has(key)) serverHosts.set(key, new Set());
    serverHosts.get(key).add(name);
  }
  const taskServers = () =>
    host.getServers().map((s) => ({
      ...s,
      agentHosts: [...(serverHosts.get(serverKey(s)) || [])],
    }));
  async function resolveAgentHosts() {
    await Promise.allSettled(
      host.getServers().map(async (server) => {
        const key = serverKey(server);
        if (probedServers.has(key)) return;
        probedServers.add(key);
        const name = (
          await host.browserCommand(server, "hostname -s", 1024)
        ).trim();
        if (/^[A-Za-z0-9_.-]+$/.test(name)) rememberHost(server, name);
      }),
    );
  }

  function refresh({ resetCache = false } = {}) {
    launchEpoch++;
    const url = normalizeHubURL(host.getData()?.hub?.url);
    const ipn = host.getIPN();
    if (!url) {
      const hadClient = !!client;
      viewClient?.dispose?.();
      viewClient = null;
      client = null;
      connected = false;
      stopAll();
      if (hadClient) host.onClientChange?.();
      return null;
    }
    if (
      client?.base !== url ||
      client?.token !== (host.getData()?.hub?.token || "") ||
      resetCache
    ) {
      stopAll();
      viewClient?.dispose?.();
      client = createHubClient({
        fetchImpl: (u, init) => {
          const network = host.getIPN();
          if (!network)
            throw new Error(
              "Offline. Connect Tailscale to update the project hub.",
            );
          return network.fetch(u, init);
        },
        baseURL: url,
        token: host.getData()?.hub?.token || "",
      });
      viewClient = host.createReadCache
        ? createCachedHubClient({
            client,
            cache: host.createReadCache(),
            online: () => !!host.getIPN(),
          })
        : client;
      host.onClientChange?.();
    }
    if (!!ipn !== connected) viewClient?.refreshConnection?.();
    connected = !!ipn;
    if (ipn) sync();
    else stopAll();
    return client;
  }
  const ready = () => !!client && !!host.getIPN();

  function stopAll() {
    for (const feed of feeds.values()) feed.stop();
    feeds.clear();
  }

  // Ensure one feed per bound task; drop feeds whose task is no longer bound.
  function sync() {
    if (!client || !host.getIPN()) return;
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
    host.paneGroups()?.model.setTaskMembers?.(taskId, []);
    feeds.get(taskId)?.stop();
    feeds.delete(taskId);
    bound.delete(taskId);
    cache.delete(taskId);
    for (const group of host.paneGroups()?.model.groups || []) {
      if (group.taskId === taskId) {
        delete group.taskId;
        delete group.guests;
      }
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
        if (!feed.error) host.notice(`Project hub: ${error.message}`);
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
    host
      .paneGroups()
      ?.model.setTaskMembers?.(
        feed.taskId,
        feed.task?.status === "closed"
          ? []
          : feed.agents.filter(openAgent).map((agent) => agent.id),
      );
    const syncLayout = () => {
      const groups = host.paneGroups();
      const orchestrator = feed.agents.find(
        (agent) =>
          agent.name.toLowerCase() === feed.task?.orchestrator?.toLowerCase(),
      );
      const tab = host
        .getTabs()
        .find(
          (tab) =>
            tab.task?.taskId === feed.taskId &&
            tab.task?.agentId === orchestrator?.id,
        );
      groups?.model.setTaskOrchestrator?.(feed.taskId, tab?.id);
      groups?.sync();
    };
    const reconcileCurrent = () =>
      reconcileTask({
        taskId: feed.taskId,
        agents: feed.agents,
        tabs: host.getTabs(),
        servers: taskServers(),
        hidden,
      });
    let r = reconcileCurrent();
    if (r.unknown.length) {
      await resolveAgentHosts();
      if (feed.stopped) return;
      r = reconcileCurrent();
    }
    if (r.unknown.length && feed.unknown !== r.unknown.length)
      host.notice(
        `Could not open ${r.unknown.length} agent terminal(s): no saved machine matches ${[...new Set(r.unknown.map((a) => a.host))].join(", ")}.`,
      );
    feed.unknown = r.unknown.length;
    for (const { tab, agent } of r.adopt) {
      tab.task = taskBinding(feed.taskId, agent);
      host.bookmark(tab);
    }
    syncLayout();
    for (const tab of r.close) host.closeTab(tab.id, { fromHub: true });
    if (feed.task?.status === "closed") {
      const group = host.paneGroups()?.model.taskGroup(feed.taskId);
      if (group) {
        delete group.taskId;
        delete group.guests;
      }
    }
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
    syncLayout();
    host.scheduleWorkspaceSave();
  }
  // Put a pane into the project's tab, or make its own tab carry the project.
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
    delete group.guests;
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
    if (!info) return `Project ${taskId}`;
    const line = `Project ${info.task.name}: ${taskRollup(info.agents, feed?.unknown || 0)}`;
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
  async function requireProjectPauseV1() {
    const capabilities = await client.capabilities();
    if (!supportsProjectPauseV1(capabilities))
      throw new Error(
        "Pause and Resume require projectPause capability version 1 on this hub.",
      );
    return capabilities.projectPause;
  }
  function previousTeamID(detail) {
    const currentNames = detail.agents
      .filter(
        (agent) =>
          agent.role !== "database_handler" &&
          agent.runId &&
          (!["closed", "exited"].includes(agent.status) || !agent.cleanupDone),
      )
      .map((agent) => agent.name.toLowerCase())
      .sort();
    const matches = (host.getData().teams || []).filter((saved) => {
      try {
        const team = resolveTeam(saved, host.getData().agentCatalog);
        return (
          team.orchestrator.toLowerCase() ===
            detail.task.orchestrator?.toLowerCase() &&
          team.members
            .map((member) => member.name.toLowerCase())
            .sort()
            .every((name, index) => name === currentNames[index]) &&
          team.members.length === currentNames.length
        );
      } catch {
        return false;
      }
    });
    return matches.length === 1 ? matches[0].id : "";
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
      "Project hub",
      `<p class="fine">Projects and messages live on your coordination hub. Use its address on your private network. The access token is saved in your encrypted vault. Agent hosts also need this token in ~/.config/tailterm/hub.json.</p><label>Hub URL<input id="hub-url" value="${esc(current)}" placeholder="http://tailterm-hub" autocomplete="off" spellcheck="false"></label><label>Access token<input id="hub-token" type="password" value="${esc(host.getData()?.hub?.token || "")}" autocomplete="off" placeholder="Required for a network listener"></label><label>Agent host<select id="hub-config-server">${serverOptions(host.currentServer()?.id)}</select></label><button id="hub-load-host" type="button">Load configuration from server</button><p id="hub-status" class="fine">${current ? "Configured." : "Not configured."}</p><div class="dialog-actions"><button id="hub-test">Test connection</button><button id="hub-save" class="primary">Save</button>${current ? '<button id="hub-clear" class="danger">Remove</button>' : ""}</div>`,
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
        host.notice(client ? "Project hub saved." : "Project hub removed.");
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

  function teamProjectFolders(container, getTeam, getMain) {
    const paths = {},
      retryPaths = {};
    const read = () => {
      container
        .querySelectorAll("[data-project-server]")
        .forEach(
          (input) => (paths[input.dataset.projectServer] = input.value.trim()),
        );
      return { ...paths };
    };
    const readRetry = () => {
      container
        .querySelectorAll("[data-launch-agent-id]")
        .forEach(
          (input) =>
            (retryPaths[input.dataset.launchAgentId] = input.value.trim()),
        );
      return { ...retryPaths };
    };
    function render() {
      read();
      const team = getTeam();
      container.hidden = !team;
      if (!team) return;
      const servers = host.getServers();
      const needed = [
        ...new Set(
          team.members
            .filter((m) => !m.cwd)
            .map((m) => m.serverId || getMain()),
        ),
      ];
      container.innerHTML =
        needed
          .map((id, i) => {
            const server = servers.find((s) => s.id === id);
            return projectFolderHTML(
              `team-project-${i}`,
              paths[id] || "",
              `${server?.name || "Missing machine"} · Project folder`,
            );
          })
          .join("") +
        team.members
          .filter((m) => m.cwd)
          .map(
            (m) =>
              `<p class="fine">${esc(m.name)} uses ${esc(m.cwd)} on ${esc(servers.find((s) => s.id === (m.serverId || getMain()))?.name || "Missing machine")} (team override).</p>`,
          )
          .join("") +
        '<p class="fine">Folders are local to each machine. Codex may ask you to trust a folder in its terminal on first use.</p>';
      container.querySelectorAll(".project-folder").forEach((root, i) => {
        root.querySelector("input").dataset.projectServer = needed[i];
        wireProjectFolder(root, host, () =>
          host.getServers().find((s) => s.id === needed[i]),
        );
      });
    }
    function setEditable(serverIDs = null) {
      const editable = serverIDs && new Set(serverIDs);
      container.querySelectorAll(".project-folder").forEach((root) => {
        const input = root.querySelector("[data-project-server]");
        const enabled = !editable || editable.has(input?.dataset.projectServer);
        root
          .querySelectorAll("input, button")
          .forEach((control) => (control.disabled = !enabled));
      });
    }
    function renderRetry(plan, journal) {
      readRetry();
      const pending = plan.filter((entry) =>
        journal?.members.some(
          (member) =>
            member.fields.agentId === entry.fields.agentId &&
            member.state === "unstarted",
        ),
      );
      container.hidden = !pending.length;
      container.innerHTML = pending
        .map((entry, index) =>
          projectFolderHTML(
            `team-retry-project-${index}`,
            retryPaths[entry.fields.agentId] ?? entry.fields.cwd,
            `${entry.fields.name} · ${entry.server.name} · Project folder`,
          ),
        )
        .join("");
      container.querySelectorAll(".project-folder").forEach((root, index) => {
        const entry = pending[index];
        const input = root.querySelector("input");
        input.dataset.launchAgentId = entry.fields.agentId;
        input.dataset.projectServer = entry.server.id;
        wireProjectFolder(root, host, () => entry.server);
      });
    }
    return { read, readRetry, render, renderRetry, setEditable };
  }

  function agentFields(server) {
    return `<div class="agent-launch-fields">
      ${projectFolderHTML("agent-cwd")}
      <p class="fine">Choose a folder on the selected machine. Codex may ask you to trust it in the terminal on first use.</p>
      <div class="appearance-controls"><label>Agent name<input id="agent-name" value="agent1" maxlength="64" autocomplete="off" spellcheck="false"></label><label>Agent app<select id="agent-runtime">${runtimeOptions(server)}</select></label></div>
      <div id="agent-model-picker"></div><div id="agent-controls"></div>
      <p id="agent-model-help" class="fine field-help">Enter a model name or alias available to this app on the selected server.</p>
      <label>Assignment<textarea id="agent-prompt" rows="2" placeholder="Optional instructions for this agent"></textarea></label>
      <details class="dialog-details"><summary>Advanced setup</summary>
        <label>Command override<input id="agent-run" placeholder="claude" autocomplete="off" spellcheck="false"></label>
      </details></div>`;
  }

  function readAgentFields() {
    const runtime = document.querySelector("#agent-runtime").value;
    const run = document.querySelector("#agent-run").value.trim() || runtime;
    return {
      name: document.querySelector("#agent-name").value.trim(),
      runtime: runtime || "generic",
      model: document.querySelector("#agent-model").disabled
        ? ""
        : document.querySelector("#agent-model").value.trim(),
      reasoning:
        document.querySelector('#agent-controls [data-field="reasoning"]')
          ?.value || "",
      run,
      cwd: document.querySelector("#agent-cwd").value.trim(),
      prompt: document.querySelector("#agent-prompt").value.trim(),
      permissionMode: document.querySelector(
        "#agent-controls [data-field=permissionMode]",
      ).value,
      approvalMode:
        document.querySelector('#agent-controls [data-field="approvalMode"]')
          ?.value || "",
      sandboxMode:
        document.querySelector('#agent-controls [data-field="sandboxMode"]')
          ?.value || "",
      allowedTools: (
        document.querySelector("#agent-controls [data-field=allowedTools]")
          ?.value || ""
      )
        .split("\n")
        .map((s) => s.trim())
        .filter(Boolean),
    };
  }
  function wireAgentFields() {
    document.querySelector("#agent-cwd").oninput = () => {
      const error = document.querySelector("#task-error");
      if (error?.textContent.startsWith("Choose a project folder"))
        error.textContent = "";
    };
    wireProjectFolder(
      document.querySelector("#agent-cwd").closest(".project-folder"),
      host,
      () =>
        host
          .getServers()
          .find((s) => s.id === document.querySelector("#task-server")?.value),
    );
    const runtime = document.querySelector("#agent-runtime"),
      run = document.querySelector("#agent-run");
    const update = () => {
      run.placeholder = runtime.value || "your-agent --flag";
      document.querySelector("#agent-controls").innerHTML = agentControlsHTML(
        runtime.value,
        { model: document.querySelector("#agent-model")?.value || "" },
      );
      wireAgentControls(
        document.querySelector("#agent-controls .agent-controls"),
        () =>
          inspectTools({
            runtime: runtime.value,
            cwd: document.querySelector("#agent-cwd").value.trim(),
            serverId: document.querySelector("#task-server").value,
          }),
      );
      const picker = document.querySelector("#agent-model-picker");
      picker.innerHTML = modelPickerHTML("agent-model", runtime.value);
      wireModelPicker(picker);
      const modelChoice = picker.querySelector("select");
      const chooseModel = modelChoice.onchange;
      modelChoice.onchange = () => {
        chooseModel();
        document.querySelector("#agent-controls").innerHTML = agentControlsHTML(
          runtime.value,
          { model: document.querySelector("#agent-model")?.value || "" },
        );
        wireAgentControls(
          document.querySelector("#agent-controls .agent-controls"),
          () =>
            inspectTools({
              runtime: runtime.value,
              cwd: document.querySelector("#agent-cwd").value.trim(),
              serverId: document.querySelector("#task-server").value,
            }),
        );
      };
      document.querySelector("#agent-model-help").textContent = runtime.value
        ? "Choose a model available to this app on the server, or enter a custom model ID. App default keeps its configured model."
        : "For custom apps, include the model option in the command below.";
    };
    runtime.onchange = update;
    update();
  }

  async function spawn(taskId, server, fields) {
    if (!fields.cwd)
      throw new Error("Choose a project folder before launching the agent.");
    if (!AGENT_NAME_RE.test(fields.name))
      throw new Error(
        "Agent name: 1–64 letters, numbers, dashes or underscores.",
      );
    const spawnFields = {
      hub: client.base,
      task: taskId,
      name: fields.name,
      run: fields.run,
      cwd: fields.cwd,
      prompt: fields.prompt,
      runtime: fields.runtime,
      model: fields.model,
      reasoning: fields.reasoning,
      permissionMode: fields.permissionMode,
      approvalMode: fields.approvalMode,
      sandboxMode: fields.sandboxMode,
      allowedTools: fields.allowedTools,
      agentRole: fields.agentRole,
      agentId: fields.agentId,
      expectedRunId: fields.expectedRunId,
      expectedLifecycleGeneration: fields.expectedLifecycleGeneration,
      resumeReceiptId: fields.resumeReceiptId,
      plannedTeamMembers: fields.plannedTeamMembers,
      workItemTaskId: fields.workItemTaskId,
      workItemId: fields.workItemId,
      workItemRevision: fields.workItemRevision,
      workOrderTaskId: fields.workOrderTaskId,
      workOrderMessageSeq: fields.workOrderMessageSeq,
      replacesAgentId: fields.replacesAgentId,
      workContextBundle: fields.workContextBundle,
    };
    let command = agentSpawnCommand(spawnFields),
      stagedContext = "",
      sftp;
    try {
      if (
        fields.workContextBundle != null &&
        new TextEncoder().encode(command).byteLength > 64 * 1024
      ) {
        if (!host.openSFTP)
          throw new Error(
            "This complete work-item context requires SFTP on the selected launch host.",
          );
        const serialized = serializedWorkContext(fields.workContextBundle);
        const data = new TextEncoder().encode(serialized);
        const digest = [
          ...new Uint8Array(await crypto.subtle.digest("SHA-256", data)),
        ]
          .map((byte) => byte.toString(16).padStart(2, "0"))
          .join("");
        stagedContext = `/tmp/.tailterm-work-context-${fields.agentId}-${digest}.json`;
        sftp = await host.openSFTP(server);
        if (!sftp?.write || !sftp?.remove)
          throw new Error(
            "This launch host does not provide the required private file transfer.",
          );
        await sftp.write(stagedContext, {
          size: data.byteLength,
          overwrite: true,
          readChunk: async (offset, length) =>
            data.slice(offset, offset + length),
        });
        command = agentSpawnCommand({
          ...spawnFields,
          workContextFile: stagedContext,
          workContextDigest: digest,
        });
        if (new TextEncoder().encode(command).byteLength > 64 * 1024)
          throw new Error(
            "The verified staged launch command exceeds the 64 KiB SSH limit.",
          );
      }
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
      rememberHost(server, agent.host);
      const candidate = handlerPlan(taskId);
      if (
        fields.agentRole === "database_handler" &&
        candidate?.serverId === server.id &&
        handlerSettingsKey(candidate.fields) === handlerSettingsKey(fields)
      ) {
        await host.api("/project-handler-plans", "POST", {
          hub: client.base,
          taskId,
          serverId: server.id,
          fields,
        });
        await host.reloadData();
      }
      return agent;
    } finally {
      if (sftp) {
        if (stagedContext) {
          try {
            await sftp.remove(stagedContext);
          } catch {}
        }
        sftp.close();
      }
    }
  }

  async function inspectTools(fields) {
    const server = fields.serverId
      ? host.getServers().find((s) => s.id === fields.serverId)
      : host.currentServer() || host.getServers()[0];
    if (!server)
      throw new Error("Choose a saved machine before inspecting tools.");
    const raw = await host.browserCommand(
      server,
      agentToolsCommand(fields.runtime, fields.cwd || ""),
      262144,
    );
    try {
      return JSON.parse(raw);
    } catch {
      throw new Error(
        "The host returned an invalid inventory. Update its tt CLI and retry.",
      );
    }
  }

  function newTask(tabId = host.currentTab()?.id, initialTeam = null) {
    if (!requireHub()) return;
    const server = host.currentServer() || host.getServers()[0];
    const teams = host.getData().teams || [];
    host.dialog(
      "New project",
      `<form id="task-form" novalidate>
      <label>Project name<input id="task-name" placeholder="Review the API changes" maxlength="120" autocomplete="off" required></label>
      <label>Objective<textarea id="task-goal" rows="3" maxlength="8192" placeholder="What should be accomplished?"></textarea></label>
      ${teams.length ? `<label>Team<select id="task-team"><option value="">No team · choose an agent below</option>${teams.map((t) => `<option value="${esc(t.id)}" ${t.id === initialTeam?.id ? "selected" : ""}>${esc(t.name)} · ${t.members.length} agents</option>`).join("")}</select></label>` : ""}
      <label id="task-main-machine" hidden>Main machine<select id="task-main-server">${serverOptions(server?.id)}</select><span class="fine">Used by team members without an assigned machine.</span></label>
      <div id="task-project-folders" hidden></div>
      <label class="check" id="task-manual-agent"><input type="checkbox" id="task-with-agent" checked disabled>Start the project orchestrator</label>
      <fieldset id="task-agent-fields" hidden disabled><label>Server<select id="task-server">${serverOptions(server?.id)}</select></label>${agentFields(server)}</fieldset>
      <label class="check"><input type="checkbox" id="task-swarm">Enable swarm</label><p class="fine">Every new message reaches every agent on this project. Named recipients indicate who should act.</p><label class="check"><input type="checkbox" id="task-allow-spawn">Allow agents to add other agents</label><label>Max new agents<input id="task-max-new-agents" type="number" min="0" max="32" step="1" value="2" disabled></label><p class="fine">Extra agents allowed per bug or feature, on top of that item's allocated team member(s). Closing an extra frees its slot; one item's extras never reduce another's. Agents you add manually do not count.</p>
      <p class="fine">A database handler starts automatically with the orchestrator’s machine, app, model, folder and permissions. It records Bugs and Features and does not use the helper allowance.</p><p class="fine">Agents appear together in a terminal group named after this project.</p>
      <div class="task-submit-area"><p id="task-error" class="fine" role="status" aria-live="polite"></p><div class="dialog-actions"><button type="button" id="task-open-created" hidden>Open created project</button><button type="submit" id="task-create" class="primary">Create project</button></div></div>
    </form>`,
    );
    const form = document.querySelector("#task-form"),
      error = form.querySelector("#task-error"),
      button = form.querySelector("#task-create"),
      withAgent = form.querySelector("#task-with-agent"),
      agentFieldsEl = form.querySelector("#task-agent-fields");
    let pending = false,
      saved = null,
      launchPlan = null,
      inheritedHandler = null,
      launchJournal = null;
    const progress = new Set();
    const teamSelect = form.querySelector("#task-team");
    const projects = teamProjectFolders(
      form.querySelector("#task-project-folders"),
      () => {
        const selectedTeam = teams.find((t) => t.id === teamSelect?.value);
        return selectedTeam
          ? resolveTeam(selectedTeam, host.getData().agentCatalog)
          : null;
      },
      () => form.querySelector("#task-main-server").value,
    );
    form.querySelector("#task-main-server").onchange = projects.render;
    const open = () => {
      host.closeDialog();
      host.openBoard(saved.id);
    };
    const freezeCreation = () => {
      for (const selector of [
        "#task-name",
        "#task-goal",
        "#task-team",
        "#task-main-server",
        "#task-with-agent",
        "#task-swarm",
        "#task-allow-spawn",
        "#task-max-new-agents",
      ]) {
        const control = form.querySelector(selector);
        if (control) control.disabled = true;
      }
      form
        .querySelectorAll(
          "#task-project-folders input, #task-project-folders button",
        )
        .forEach((control) => (control.disabled = true));
    };
    const showUnknownCreation = () => {
      freezeCreation();
      button.disabled = true;
      button.textContent = "Creation status unknown";
      error.textContent =
        "A prior project-creation response is unknown. Tailterm cannot safely identify it from project fields and will not create another project or adopt a possible match. Inspect Projects, then discard this unresolved record only after verifying the outcome.";
      if (form.querySelector("#task-discard-creation")) return;
      const discard = document.createElement("button");
      discard.type = "button";
      discard.id = "task-discard-creation";
      discard.textContent = "Discard unresolved record";
      discard.onclick = async () => {
        if (
          !(await host.confirm(
            "Discard unresolved project launch?",
            "Only do this after verifying that the project was not created.",
          ))
        )
          return;
        await guardedJournalEffect(launchJournal, null, () =>
          host.api(`/team-launch-plans/${launchJournal.id}`, "DELETE"),
        );
        host.closeDialog();
        host.notice(
          "The unresolved launch record was discarded. Open New project to start over.",
        );
      };
      form.querySelector(".dialog-actions").prepend(discard);
    };
    form.querySelector("#task-open-created").onclick = open;
    form.querySelector("#task-allow-spawn").onchange = (e) =>
      (form.querySelector("#task-max-new-agents").disabled = !e.target.checked);
    withAgent.onchange = () => {
      const hasTeam = !!teamSelect?.value;
      form.querySelector("#task-manual-agent").hidden = hasTeam;
      form.querySelector("#task-main-machine").hidden = !teams
        .map((candidate) => resolveTeam(candidate, host.getData().agentCatalog))
        .find((t) => t.id === teamSelect?.value)
        ?.members.some((m) => !m.serverId);
      agentFieldsEl.hidden = hasTeam || !withAgent.checked;
      agentFieldsEl.disabled = hasTeam || !withAgent.checked;
      button.textContent = hasTeam
        ? "Create project and start team"
        : withAgent.checked
          ? "Create project and start agent"
          : "Create project";
    };
    const selectTeam = () => {
      withAgent.onchange();
      projects.render();
      form.querySelector("#task-swarm").checked = !!teams.find(
        (t) => t.id === teamSelect?.value,
      )?.swarm;
    };
    if (teamSelect) teamSelect.onchange = selectTeam;
    selectTeam();
    withAgent.onchange();
    form.onsubmit = async (event) => {
      event.preventDefault();
      if (pending) return;
      pending = true;
      button.disabled = true;
      withAgent.disabled = true;
      error.textContent = "";
      try {
        if (
          !saved &&
          launchJournal?.creation?.state === "uncertain" &&
          !launchJournal.taskId
        ) {
          throw new Error(
            "The prior project-creation response is still unknown. No authoritative creation receipt is available, so Tailterm will not adopt a project or create another one.",
          );
        }
        const name = form.querySelector("#task-name").value.trim();
        if (!name || [...name].length > 120 || /[\x00-\x1f\x7f]/.test(name))
          throw new Error("Enter a project name of up to 120 characters.");
        const maxNewAgents = Number(
          form.querySelector("#task-max-new-agents").value,
        );
        if (
          !Number.isInteger(maxNewAgents) ||
          maxNewAgents < 0 ||
          maxNewAgents > 32
        )
          throw new Error(
            "Max new agents must be a whole number from 0 to 32.",
          );
        const target = host
          .getServers()
          .find((s) => s.id === form.querySelector("#task-server").value);
        const team = teams.find((t) => t.id === teamSelect?.value);
        const createRequest = {
          name,
          goal: form.querySelector("#task-goal").value.trim(),
          allowAgentSpawn: form.querySelector("#task-allow-spawn").checked,
          maxNewAgents,
          swarm: form.querySelector("#task-swarm").checked,
          orchestrator:
            team?.orchestrator ||
            team?.members[0]?.name ||
            (!team && withAgent.checked
              ? form.querySelector("#agent-name").value.trim()
              : ""),
        };
        const fields = !team && withAgent.checked ? readAgentFields() : null;
        if (fields) {
          if (!target) throw new Error("Choose a server for the first agent.");
          if (!fields.cwd)
            throw new Error("Choose a project folder for the first agent.");
          // Validate all launch input before creating persistent task records.
          agentSpawnCommand({
            hub: client.base,
            task: "tsk_0000000000000000",
            ...fields,
          });
        }
        const plan =
          launchPlan ||
          withDatabaseHandler(
            team
              ? teamLaunches(
                  team,
                  host.getServers(),
                  form.querySelector("#task-main-server").value,
                  projects.read(),
                  null,
                  host.getData().agentCatalog,
                )
              : fields
                ? [{ server: target, fields }]
                : [],
          );
        if (launchPlan && launchJournal) {
          launchPlan = await amendUnstartedFolders(
            launchPlan,
            launchJournal,
            projects.readRetry(),
          );
        }
        if (!launchJournal && plan.length && team) {
          const preparedAt = new Date().toISOString();
          launchJournal = await prepareLaunchJournal(
            "new-project",
            saved?.id,
            plan,
            team?.id,
            {
              state: "prepared",
              request: createRequest,
              knownTaskIds: (await client.listTasks()).map((task) => task.id),
              preparedAt,
            },
          );
        }
        if (!saved && launchJournal) await saveLaunchJournal(launchJournal);
        form
          .querySelectorAll(
            "#task-project-folders input, #task-project-folders button",
          )
          .forEach((el) => (el.disabled = true));
        button.disabled = true;
        withAgent.disabled = true;
        error.textContent = saved
          ? "Retrying agent launch…"
          : "Creating project…";
        if (!saved) {
          if (launchJournal?.creation) {
            launchJournal.creation.state = "uncertain";
            launchJournal.creation.attemptedAt = new Date().toISOString();
            await saveLaunchJournal(launchJournal);
          }
          try {
            saved = await guardedJournalEffect(launchJournal, null, () =>
              client.createTask(createRequest),
            );
          } catch (createError) {
            if (
              launchJournal &&
              Number.isInteger(createError?.status) &&
              createError.status >= 400 &&
              createError.status < 500
            ) {
              await guardedJournalEffect(launchJournal, null, () =>
                host.api(`/team-launch-plans/${launchJournal.id}`, "DELETE"),
              );
              launchJournal = null;
              launchPlan = null;
            } else if (launchJournal) {
              showUnknownCreation();
            }
            throw createError;
          }
          launchPlan = plan;
          if (launchJournal) {
            launchJournal.taskId = saved.id;
            launchJournal.creation.state = "confirmed";
            await saveLaunchJournal(launchJournal);
          }
          if (teamSelect) teamSelect.disabled = true;
          form.querySelector("#task-main-server").disabled = true;
          form
            .querySelectorAll(
              "#task-project-folders input, #task-project-folders button",
            )
            .forEach((el) => (el.disabled = true));
          bound.add(saved.id);
          cache.set(saved.id, { task: saved, agents: [] });
          form.querySelector("#task-allow-spawn").disabled = true;
          form.querySelector("#task-swarm").disabled = true;
          form.querySelector("#task-max-new-agents").disabled = true;
          form.querySelector("#task-name").disabled = true;
          form.querySelector("#task-goal").disabled = true;
          form.querySelector("#task-open-created").hidden = false;
        }
        launchPlan ||= plan;
        if (!team && fields && progress.size === 0)
          launchPlan = withDatabaseHandler([{ server: target, fields }]);
        const handler = launchPlan.find(
          (member) => member.fields.agentRole === "database_handler",
        );
        if (inheritedHandler && handler) {
          const amendedFolder = handler.fields.cwd;
          handler.server = inheritedHandler.server;
          handler.fields = {
            ...structuredClone(inheritedHandler.fields),
            cwd: amendedFolder,
          };
        }
        try {
          await launchMembers(
            saved.id,
            launchPlan,
            progress,
            (text) => (error.textContent = text),
            launchJournal,
          );
        } finally {
          // A successful lead launch fixes its handler's inherited settings.
          // A separately receipted folder correction is the only exception.
          if (
            !inheritedHandler &&
            handler &&
            progress.has(launchPlan[0].fields.name)
          )
            inheritedHandler = {
              server: handler.server,
              fields: structuredClone(handler.fields),
            };
        }
        sync();
        open();
        host.notice(
          launchPlan.length
            ? `Project created; ${launchPlan.length} agent${launchPlan.length === 1 ? "" : "s"} starting.`
            : "Project created.",
        );
      } catch (e) {
        const unknownCreation =
          !saved &&
          launchJournal?.creation?.state === "uncertain" &&
          !launchJournal.taskId;
        if (teamSelect?.value && !launchJournal) {
          launchPlan = null;
          form
            .querySelectorAll(
              "#task-project-folders input, #task-project-folders button",
            )
            .forEach((el) => (el.disabled = false));
        }
        if (unknownCreation) showUnknownCreation();
        else
          error.textContent =
            (saved ? "Project created. Agent launch failed: " : "") +
            formatError(e) +
            (progress.size
              ? " Started agents keep their folders. The database handler keeps the lead’s settings; corrections apply to remaining workers."
              : "");
        if (
          saved &&
          launchJournal?.members.some((member) => member.state === "unstarted")
        )
          projects.renderRetry(launchPlan, launchJournal);
        error.setAttribute("role", "alert");
        error.scrollIntoView({ block: "nearest" });
        if (!unknownCreation)
          button.textContent = saved
            ? "Retry agent launch"
            : withAgent.checked
              ? "Create project and start agent"
              : "Create project";
      } finally {
        pending = false;
        button.disabled =
          !saved &&
          launchJournal?.creation?.state === "uncertain" &&
          !launchJournal.taskId;
        withAgent.disabled = true;
      }
    };
    wireAgentFields();
    form.querySelector("#task-server").onchange = (e) => {
      const target = host.getServers().find((s) => s.id === e.target.value);
      form.querySelector("#agent-cwd").value = "";
      form.querySelector("#agent-runtime").innerHTML = runtimeOptions(target);
      wireAgentFields();
    };
    if (initialTeam?.id) {
      pending = true;
      button.disabled = true;
      void (async () => {
        const scope = await launchScope();
        const recovered = (host.getData().teamLaunchPlans || []).find(
          (entry) =>
            entry.kind === "new-project" &&
            entry.scope === scope &&
            entry.teamId === initialTeam.id,
        );
        if (!recovered || !form.isConnected) return;
        if (!recovered.taskId) {
          launchJournal = structuredClone(recovered);
          await assertLaunchScope(launchJournal);
          launchPlan = await restoreLaunchMembers(
            launchJournal,
            host.getServers(),
          );
          const request = launchJournal.creation?.request;
          if (!request)
            throw new Error(
              "The unresolved project record is missing its frozen creation request.",
            );
          form.querySelector("#task-name").value = request.name;
          form.querySelector("#task-goal").value = request.goal;
          form.querySelector("#task-swarm").checked = request.swarm;
          form.querySelector("#task-allow-spawn").checked =
            request.allowAgentSpawn;
          form.querySelector("#task-max-new-agents").value =
            request.maxNewAgents;
          showUnknownCreation();
          return;
        }
        const detail = await guardedJournalEffect(recovered, null, () =>
          client.getTask(recovered.taskId),
        );
        if (!form.isConnected) return;
        saved = detail.task;
        launchJournal = structuredClone(recovered);
        launchPlan = await restoreLaunchMembers(
          launchJournal,
          host.getServers(),
        );
        for (const member of launchJournal.members)
          if (member.state === "started") progress.add(member.fields.name);
        form.querySelector("#task-name").value = saved.name;
        form.querySelector("#task-goal").value = saved.goal || "";
        form.querySelector("#task-name").disabled = true;
        form.querySelector("#task-goal").disabled = true;
        if (teamSelect) teamSelect.disabled = true;
        form.querySelector("#task-open-created").hidden = false;
        error.textContent =
          "Recovered the encrypted frozen launch plan. Retry reconciles uncertain identities before starting unstarted members.";
        button.textContent = "Retry agent launch";
        projects.renderRetry(launchPlan, launchJournal);
      })()
        .catch((error) => {
          if (form.isConnected)
            form.querySelector("#task-error").textContent = formatError(error);
        })
        .finally(() => {
          if (!form.isConnected) return;
          pending = false;
          button.disabled =
            !saved &&
            launchJournal?.creation?.state === "uncertain" &&
            !launchJournal.taskId;
        });
    }
    form.querySelector("#task-name").focus();
  }

  function handlerPlan(taskId) {
    return host
      .getData()
      ?.projectHandlerPlans?.find(
        (p) => p.hub === client.base && p.taskId === taskId,
      );
  }
  function handlerSettingsKey(fields) {
    return JSON.stringify(
      [
        "name",
        "run",
        "cwd",
        "runtime",
        "model",
        "reasoning",
        "approvalMode",
        "sandboxMode",
        "permissionMode",
        "prompt",
        "agentRole",
        "agentId",
        "expectedRunId",
      ]
        .map((key) => fields[key] || "")
        .concat([fields.allowedTools || []]),
    );
  }
  async function saveHandlerPlan(taskId, server, fields) {
    const saved = handlerPlan(taskId);
    fields.agentId ||=
      saved?.fields.agentId ||
      "agt_" + crypto.randomUUID().replaceAll("-", "").slice(0, 16);
    const previous =
      saved?.previous ||
      (saved ? { serverId: saved.serverId, fields: saved.fields } : undefined);
    await host.api("/project-handler-plans", "POST", {
      hub: client.base,
      taskId,
      serverId: server.id,
      fields,
      ...(previous && { previous }),
    });
    await host.reloadData();
  }
  async function setupHandler(taskId) {
    if (!requireHub()) return;
    try {
      const detail = await client.getTask(taskId);
      if (detail.task.status !== "open")
        throw new Error("This project is closed.");
      const current = detail.agents.find((a) => a.role === "database_handler");
      const saved = handlerPlan(taskId);
      if (current && !["exited", "closed"].includes(current.status)) {
        if (current.status === "retired") {
          host.notice(
            "The database handler is retired. Resume it in Project settings when needed.",
          );
          return;
        }
        if (current.online || !saved) {
          host.openBoard(taskId);
          host.notice(
            `Database handler: ${current.name}${current.online ? "" : " · offline; no saved launch settings on this browser"}.`,
          );
          return;
        }
      }
      if (current?.status === "closed")
        throw new Error(
          "The database handler was explicitly closed. Its identity cannot be restarted.",
        );
      const lead = detail.agents.find(
        (a) => a.name.toLowerCase() === detail.task.orchestrator?.toLowerCase(),
      );
      const server = saved
        ? host.getServers().find((s) => s.id === saved.serverId)
        : current
          ? matchServer(current.host, taskServers())
          : (lead && matchServer(lead.host, taskServers())) ||
            host.currentServer() ||
            host.getServers()[0];
      host.dialog(
        `Database handler · ${detail.task.name}`,
        `<form id="project-handler-form"><label>Server<select id="task-server">${serverOptions(server?.id)}</select></label>${agentFields(server)}<p class="fine">${saved ? "Review the saved handler launch settings." : "This project has no saved handler settings. Choose its app, model and folder; app default uses the host’s configuration."}</p><p id="task-error" class="fine" role="status"></p><div class="dialog-actions"><button type="submit" id="handler-start" class="primary">${current ? "Restart" : "Start"} database handler</button></div></form>`,
      );
      const form = document.querySelector("#project-handler-form");
      const agentId =
        current?.id ||
        saved?.fields.agentId ||
        "agt_" + crypto.randomUUID().replaceAll("-", "").slice(0, 16);
      if ((saved || current) && !server) {
        const select = form.querySelector("#task-server");
        select.prepend(
          new Option(
            "Saved machine unavailable · choose a machine",
            "",
            true,
            true,
          ),
        );
        form.querySelector("#task-error").textContent =
          "The saved handler machine is unavailable. Restore its server entry before retrying an existing handler.";
      }
      wireAgentFields();
      const fill = (fields) => {
        const runtime = form.querySelector("#agent-runtime");
        if (
          fields.runtime &&
          ![...runtime.options].some((o) => o.value === fields.runtime)
        )
          runtime.add(new Option(fields.runtime, fields.runtime));
        runtime.value =
          fields.runtime === "generic" ? "" : fields.runtime || runtime.value;
        runtime.onchange();
        for (const [key, value] of Object.entries({
          name: fields.name,
          cwd: fields.cwd,
          run: fields.run,
          model: fields.model,
          reasoning: fields.reasoning,
          prompt: fields.prompt,
        }))
          if (value !== undefined)
            form.querySelector(`#agent-${key}`).value = value;
        if (fields.permissionMode) {
          const select = form.querySelector('[data-field="permissionMode"]');
          select.value = fields.permissionMode;
          select.dispatchEvent(new Event("change"));
        }
        for (const key of ["reasoning", "approvalMode", "sandboxMode"]) {
          const control = form.querySelector(`[data-field="${key}"]`);
          if (control && fields[key] !== undefined) control.value = fields[key];
        }
        const tools = form.querySelector('[data-field="allowedTools"]');
        if (tools) tools.value = (fields.allowedTools || []).join("\n");
      };
      const names = new Set(detail.agents.map((a) => a.name.toLowerCase()));
      let name = current?.name || "db-handler",
        suffix = 1;
      while (!current && names.has(name.toLowerCase()))
        name = `db-handler-${suffix++}`;
      fill(
        saved?.fields || {
          name,
          runtime: current?.runtime || lead?.runtime,
          cwd: current?.cwd || lead?.cwd || "",
        },
      );
      if (saved?.previous) {
        const restore = document.createElement("button");
        restore.type = "button";
        restore.id = "handler-restore-settings";
        restore.textContent = "Restore previous settings";
        form.querySelector(".dialog-actions").prepend(restore);
        restore.onclick = async () => {
          restore.disabled = true;
          try {
            await host.api("/project-handler-plans", "POST", {
              hub: client.base,
              taskId,
              ...saved.previous,
              previous: { serverId: saved.serverId, fields: saved.fields },
            });
            await host.reloadData();
            if (form.isConnected) await setupHandler(taskId);
          } catch (error) {
            form.querySelector("#task-error").textContent = formatError(error);
            restore.disabled = false;
          }
        };
      }
      form.querySelector("#agent-name").disabled = true;
      form.querySelector("#agent-name").value =
        current?.name || saved?.fields.name || name;
      form.querySelector("#task-server").onchange = (e) => {
        const target = host.getServers().find((s) => s.id === e.target.value);
        form.querySelector("#agent-runtime").innerHTML = runtimeOptions(target);
        wireAgentFields();
        form.querySelector("#agent-cwd").value = "";
      };
      form.onsubmit = async (event) => {
        event.preventDefault();
        const button = form.querySelector("#handler-start"),
          error = form.querySelector("#task-error");
        if (button.disabled) return;
        form.querySelectorAll("button").forEach((b) => (b.disabled = true));
        try {
          const target = host
            .getServers()
            .find((s) => s.id === form.querySelector("#task-server").value);
          if (!target) throw new Error("Choose a saved server.");
          const fields = {
            ...readAgentFields(),
            agentRole: "database_handler",
            agentId,
          };
          if (current?.status === "exited") {
            fields.expectedRunId = current.runId;
          }
          agentSpawnCommand({ hub: client.base, task: taskId, ...fields });
          await saveHandlerPlan(taskId, target, fields);
          error.textContent = "Starting database handler…";
          await spawn(taskId, target, fields);
          if (form.isConnected) host.closeDialog();
          attach(taskId);
          host.notice("Database handler started.");
        } catch (e) {
          error.textContent =
            formatError(e) +
            (handlerPlan(taskId)?.previous
              ? " Previous settings are retained; reopen handler setup to restore them."
              : "");
        } finally {
          form.querySelectorAll("button").forEach((b) => (b.disabled = false));
        }
      };
    } catch (e) {
      host.notice(formatError(e));
    }
  }

  async function launchMembers(taskId, plan, progress, report, journal = null) {
    const detail = journal
      ? await guardedJournalEffect(journal, null, () => client.getTask(taskId))
      : await client.getTask(taskId);
    const plannedNames = new Set([
      ...detail.agents
        .filter(
          (agent) =>
            agent.role !== "database_handler" &&
            !["closed", "exited"].includes(agent.status),
        )
        .map((agent) => agent.name.toLowerCase()),
      ...plan
        .filter(({ fields }) => fields.agentRole !== "database_handler")
        .map(({ fields }) => fields.name.toLowerCase()),
    ]);
    const plannedTeamMembers = plannedNames.size;
    let reconciled = false;
    for (const entry of plan) {
      const { fields } = entry;
      const member = journal?.members.find(
        (candidate) => candidate.fields.agentId === fields.agentId,
      );
      const exact = detail.agents.find((agent) => agent.id === fields.agentId);
      if (member && ["uncertain", "started"].includes(member.state)) {
        const pendingResumeOrchestrator =
          journal.kind === "resume-project" &&
          member.state === "uncertain" &&
          detail.task.pauseState === "resuming" &&
          fields.agentId === journal.resume.orchestratorAgentId;
        // A handler is launched after the fresh lead has admitted the Resume
        // barrier. If its host command failed before registration, the exact
        // identity is absent from the authoritative task read. Preserve that
        // identity and retry its unchanged command; do not treat it as a
        // started agent or invent a replacement identity.
        const retryMissingResumeHandler =
          journal.kind === "resume-project" &&
          member.state === "uncertain" &&
          fields.agentRole === "database_handler" &&
          journal.resume.state === "confirmed" &&
          detail.task.pauseState === "active";
        // An exact preallocated worker that is still absent after a confirmed
        // Resume can be retried with its frozen identity. Unlike a replacement,
        // this neither invents an ID nor changes a command; the authoritative
        // read proves that the prior local attempt did not admit the identity.
        const retryMissingResumeMember =
          journal.kind === "resume-project" &&
          member.state === "uncertain" &&
          !fields.agentRole &&
          fields.agentId !== journal.resume.orchestratorAgentId &&
          journal.resume.state === "confirmed" &&
          detail.task.pauseState === "active";
        if (
          !exact &&
          !pendingResumeOrchestrator &&
          !retryMissingResumeHandler &&
          !retryMissingResumeMember
        )
          throw new Error(
            `The saved ${member.state} identity for ${fields.name} is not present. Reconcile the exact agent and run before continuing.`,
          );
        if (retryMissingResumeHandler || retryMissingResumeMember) {
          member.state = "unstarted";
          reconciled = true;
        } else if (exact) {
          const problem = journal
            ? await guardedJournalEffect(journal, entry, () =>
                reconciledAgentProblem(taskId, entry, member, exact),
              )
            : await reconciledAgentProblem(taskId, entry, member, exact);
          if (problem)
            throw new Error(
              `The saved identity for ${fields.name} has a ${problem} mismatch. Inspect the project before continuing.`,
            );
        }
        // Registering the planned fresh lead does not open the project. The
        // host-side tt spawn must verify the exact tmux ownership and consume
        // /resume/confirm. Re-run that same frozen command while the barrier
        // remains resuming, even if AddAgent already saved the identity.
        if (pendingResumeOrchestrator) continue;
        if (member.state === "unstarted") continue;
        if (member.state === "uncertain") {
          member.state = "started";
          member.agent = {
            id: exact.id,
            runId: exact.runId,
            name: exact.name,
          };
          reconciled = true;
        }
        progress.add(fields.name);
        continue;
      }
      if (member?.state === "unstarted" && exact)
        throw new Error(
          `The unstarted record for ${fields.name} already has its preallocated identity in the project. Inspect it before continuing.`,
        );
      if (
        !progress.has(fields.name) &&
        detail.agents.some(
          (agent) =>
            !["closed", "exited"].includes(agent.status) &&
            agent.name.toLowerCase() === fields.name.toLowerCase(),
        )
      )
        throw new Error(
          `An agent named ${fields.name} already exists with a different identity. Open the project to inspect it before launching more agents.`,
        );
    }
    if (reconciled) await saveLaunchJournal(journal);
    for (const entry of plan) {
      const { server, fields } = entry;
      if (fields.agentRole === "database_handler")
        if (!progress.has(fields.name))
          if (journal)
            await guardedJournalEffect(journal, entry, () =>
              saveHandlerPlan(taskId, server, fields),
            );
          else await saveHandlerPlan(taskId, server, fields);
    }
    for (const [i, entry] of plan.entries()) {
      const { server, fields } = entry;
      if (progress.has(fields.name)) continue;
      const journalMember = journal?.members.find(
        (member) => member.fields.agentId === fields.agentId,
      );
      report(
        `Starting ${fields.name} on ${server.name} (${i + 1}/${plan.length})…`,
      );
      const launchFields =
        fields.agentRole === "database_handler"
          ? fields
          : { ...fields, plannedTeamMembers };
      // Do all local argument validation before recording an uncertain host
      // attempt. Browser-side validation failures never reach tt and must not
      // poison a later exact-identity retry.
      agentSpawnCommand({ hub: client.base, task: taskId, ...launchFields });
      if (journal?.kind === "add-team")
        await assertCurrentWorkOrderScope(client, {
          itemTaskId: fields.workItemTaskId,
          itemId: fields.workItemId,
          itemRevision: fields.workItemRevision,
          workOrderMessage: {
            taskId: fields.workOrderTaskId,
            seq: fields.workOrderMessageSeq,
          },
        });
      if (journalMember) {
        journalMember.state = "uncertain";
        await saveLaunchJournal(journal);
      }
      let agent;
      try {
        const effect = () =>
          spawn(
            taskId,
            server,
            launchFields,
          );
        agent = journal
          ? await guardedJournalEffect(journal, entry, effect)
          : await effect();
      } catch (error) {
        const verifiedUnstarted =
          error?.verifiedUnstarted === true ||
          /\bcwd\b[\s\S]{0,1024}\bis not a directory\b/.test(
            error?.message || "",
          );
        if (journalMember && verifiedUnstarted) {
          journalMember.state = "unstarted";
          await saveLaunchJournal(journal);
        }
        throw error;
      }
      if (journalMember) {
        journalMember.state = "started";
        journalMember.agent = {
          id: agent.id,
          runId: agent.runId,
          name: agent.name || fields.name,
        };
        await saveLaunchJournal(journal);
      }
      progress.add(fields.name);
    }
    if (
      journal &&
      journal.kind !== "replace-lead" &&
      journal.members.every((member) => member.state === "started")
    )
      await guardedJournalEffect(journal, null, () =>
        host.api(`/team-launch-plans/${journal.id}`, "DELETE"),
      );
  }

  async function addTeam(team) {
    if (!requireHub()) return;
    try {
      const referencedTeam = team;
      team = resolveTeam(team, host.getData().agentCatalog);
      let plan = null,
        launchJournal = null,
        routing = null;
      const mainServer = host.currentServer() || host.getServers()[0];
      const tasks = (await loadTasks()).filter((t) => t.status === "open");
      host.dialog(
        `Add team · ${team.name}`,
        `<form id="team-launch-form"><label>Project<select id="team-task">${tasks.map((t) => `<option value="${esc(t.id)}">${esc(t.name)}</option>`).join("")}</select></label><label>Bug or feature<select id="team-work-item"></select></label><label>Work-order Board message<input id="team-work-order" type="number" min="1" step="1" placeholder="Message number"></label>${team.members.some((m) => !m.serverId) ? `<label>Main machine<select id="team-main-server">${serverOptions(mainServer?.id)}</select></label>` : ""}<p class="fine">${team.members.map((m) => esc(m.name) + " · " + esc(m.serverId ? host.getServers().find((s) => s.id === m.serverId)?.name || "Missing machine" : "Main machine")).join("<br>")}</p>${team.swarm ? '<p class="fine">Starting this swarm enables broadcast for every agent on the selected project, including its existing agents.</p>' : ""}<p class="fine">Each new member gets a fresh item-scoped name, starts after existing Board history, and restores only the selected item’s durable linked context. The work-order message must be linked to that item. Existing sessions and results remain unchanged.</p><p class="fine">An existing project keeps its main orchestrator; new members introduce themselves to that leader.</p><div id="team-project-folders"></div><p id="team-launch-status" class="fine" role="status">${tasks.length ? "Loading work items…" : "Create a project first."}</p><div class="dialog-actions"><button type="submit" class="primary" disabled>Start team</button></div></form>`,
      );
      const form = document.querySelector("#team-launch-form"),
        selector = form.querySelector("#team-task"),
        itemSelector = form.querySelector("#team-work-item"),
        button = form.querySelector('button[type="submit"]'),
        status = form.querySelector("#team-launch-status");
      let items = [];
      const progress = new Set();
      const projects = teamProjectFolders(
        form.querySelector("#team-project-folders"),
        () => team,
        () => form.querySelector("#team-main-server")?.value || mainServer?.id,
      );
      projects.render();
      const loadItems = async () => {
        button.disabled = true;
        itemSelector.disabled = true;
        status.textContent = tasks.length
          ? "Loading work items…"
          : "Create a project first.";
        items = [];
        itemSelector.replaceChildren();
        if (!selector.value) return;
        try {
          const result = await client.listWorkItems({ task: selector.value });
          items = (result.items || []).filter(
            (item) => !["done", "dismissed"].includes(item.status),
          );
          itemSelector.innerHTML = items
            .map(
              (item) =>
                `<option value="${esc(item.id)}">${esc(item.kind === "bug" ? "Bug" : "Feature")} · ${esc(item.title)} · r${item.revision}</option>`,
            )
            .join("");
          status.textContent = items.length
            ? ""
            : "This project has no active bug or feature to bind the team to.";
          button.disabled = !items.length;
        } catch (error) {
          status.textContent = formatError(error);
        } finally {
          itemSelector.disabled = !items.length;
        }
      };
      selector.onchange = loadItems;
      await loadItems();
      const recover = async () => {
        if (plan || !selector.value) return;
        const scope = await launchScope();
        const savedPlan = (host.getData().teamLaunchPlans || []).find(
          (entry) =>
            entry.kind === "add-team" &&
            entry.scope === scope &&
            entry.taskId === selector.value &&
            entry.teamId === referencedTeam.id,
        );
        if (!savedPlan) return;
        launchJournal = structuredClone(savedPlan);
        await assertLaunchScope(launchJournal);
        plan = await restoreLaunchMembers(launchJournal, host.getServers());
        const frozen = plan.find((member) => member.fields.workItemId)?.fields;
        if (frozen)
          routing = {
            workItemTaskId: frozen.workItemTaskId,
            workItemId: frozen.workItemId,
            workItemRevision: frozen.workItemRevision,
            workOrderTaskId: frozen.workOrderTaskId,
            workOrderMessageSeq: frozen.workOrderMessageSeq,
            workContextBundle: frozen.workContextBundle,
          };
        for (const member of launchJournal.members)
          if (member.state === "started") progress.add(member.fields.name);
        selector.disabled = true;
        itemSelector.disabled = true;
        form.querySelector("#team-work-order").disabled = true;
        const mainChoice = form.querySelector("#team-main-server");
        if (mainChoice) mainChoice.disabled = true;
        projects.renderRetry(plan, launchJournal);
        status.textContent =
          "Recovered the encrypted frozen launch plan. Retry reconciles uncertain identities before starting unstarted members.";
        button.textContent = "Retry remaining agents";
        button.disabled = false;
      };
      await recover();
      selector.onchange = async () => {
        await loadItems();
        await recover();
      };
      if (form.querySelector("#team-main-server"))
        form.querySelector("#team-main-server").onchange = projects.render;
      form.onsubmit = async (e) => {
        e.preventDefault();
        if (button.disabled) return;
        button.disabled = true;
        selector.disabled = true;
        itemSelector.disabled = true;
        form.querySelector("#team-work-order").disabled = true;
        const id = selector.value;
        try {
          if (plan && launchJournal)
            plan = await amendUnstartedFolders(
              plan,
              launchJournal,
              projects.readRetry(),
            );
          const existingTask = launchJournal
            ? await guardedJournalEffect(launchJournal, null, () =>
                client.getTask(id),
              )
            : await client.getTask(id);
          if (!plan) {
            const item = items.find(
              (candidate) => candidate.id === itemSelector.value,
            );
            const workOrderMessageSeq = Number(
              form.querySelector("#team-work-order").value,
            );
            if (
              !item ||
              !Number.isSafeInteger(workOrderMessageSeq) ||
              workOrderMessageSeq < 1
            )
              throw new Error(
                "Choose an active bug or feature and enter its recorded work-order Board message number.",
              );
            routing = {
              workItemTaskId: item.taskId,
              workItemId: item.id,
              workItemRevision: item.revision,
              workOrderTaskId: item.taskId,
              workOrderMessageSeq,
            };
            routing.workContextBundle = await prepareWorkItemContext(client, {
              itemTaskId: item.taskId,
              itemId: item.id,
              itemRevision: item.revision,
              workOrderMessage: {
                taskId: item.taskId,
                seq: workOrderMessageSeq,
              },
            });
            plan = teamLaunchPlan({
              team: referencedTeam,
              servers: host.getServers(),
              mainServerId:
                form.querySelector("#team-main-server")?.value ||
                mainServer?.id,
              projectFolders: projects.read(),
              itemRouting: routing,
              catalog: host.getData().agentCatalog,
              handler: existingTask.agents.find(
                (agent) =>
                  agent.role === "database_handler" &&
                  !["closed", "exited"].includes(agent.status),
              ),
            });
            launchJournal = await prepareLaunchJournal(
              "add-team",
              id,
              plan,
              referencedTeam.id,
            );
            await saveLaunchJournal(launchJournal);
            const mainChoice = form.querySelector("#team-main-server");
            if (mainChoice) mainChoice.disabled = true;
            form
              .querySelectorAll(
                "#team-project-folders input, #team-project-folders button",
              )
              .forEach((el) => (el.disabled = true));
            projects.setEditable([]);
          }
          await assertCurrentWorkOrderScope(client, {
            itemTaskId: routing.workItemTaskId,
            itemId: routing.workItemId,
            itemRevision: routing.workItemRevision,
            workOrderMessage: {
              taskId: routing.workOrderTaskId,
              seq: routing.workOrderMessageSeq,
            },
          });
          const policy = {};
          if (team.swarm) policy.swarm = true;
          const orchestrator = addTeamOrchestrator(
            existingTask.task,
            existingTask.agents,
            plan,
          );
          if (orchestrator) policy.orchestrator = orchestrator;
          if (Object.keys(policy).length)
            await guardedJournalEffect(launchJournal, null, () =>
              client.updateTask(id, policy),
            );
          bound.add(id);
          await launchMembers(
            id,
            plan,
            progress,
            (text) => (status.textContent = text),
            launchJournal,
          );
          sync();
          host.closeDialog();
          host.openBoard(id);
          host.notice(`Team ${team.name} is starting.`);
        } catch (error) {
          status.textContent = formatError(error);
          button.textContent = "Retry remaining agents";
          if (progress.size)
            status.textContent +=
              " Already started agents and the prepared item context stay fixed; correct folders only for the remaining agents.";
          if (
            launchJournal?.members.some(
              (member) => member.state === "unstarted",
            )
          )
            projects.renderRetry(plan, launchJournal);
          if (!progress.size && !launchJournal) {
            selector.disabled = false;
            itemSelector.disabled = !items.length;
            form.querySelector("#team-work-order").disabled = false;
            plan = null;
            routing = null;
            const mainChoice = form.querySelector("#team-main-server");
            if (mainChoice) mainChoice.disabled = false;
            projects.setEditable();
          }
        } finally {
          button.disabled = false;
        }
      };
    } catch (error) {
      host.notice(formatError(error));
    }
  }

  async function attachTask() {
    if (!requireHub()) return;
    try {
      const tasks = (await loadTasks()).filter((t) => t.status === "open");
      host.dialog(
        "Project terminals",
        `<div class="dialog-menu">${tasks.map((t) => `<button data-open-task="${esc(t.id)}">${esc(t.name)}</button>`).join("") || '<p class="fine">No open projects.</p>'}</div>`,
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

  async function replaceLead(taskId) {
    if (!requireHub()) return;
    try {
      const scope = await launchScope();
      let detail = await client.getTask(taskId);
      if (detail.task.status !== "open")
        throw new Error("This project is closed.");
      if (!Number.isSafeInteger(detail.task.leadRevision))
        throw new Error(
          "The project hub needs the lead recovery update before replacing a lead.",
        );
      let journal = (host.getData().teamLaunchPlans || []).find(
        (p) =>
          p.kind === "replace-lead" && p.taskId === taskId && p.scope === scope,
      );
      const recovered = journal?.members[0];
      if (
        recovered?.agent &&
        detail.task.orchestrator === recovered.fields.name
      ) {
        await client.assignLead(taskId, {
          ...journal.lead,
          agentId: recovered.agent.id,
          runId: recovered.agent.runId,
        });
        await guardedJournalEffect(journal, null, () =>
          host.api(`/team-launch-plans/${journal.id}`, "DELETE"),
        );
        await host.reloadData();
        journal = null;
      }
      const previous = detail.agents
        .filter(
          (a) =>
            a.name.toLowerCase() === detail.task.orchestrator?.toLowerCase(),
        )
        .at(-1);
      const expected = journal?.lead || {
        requestId: "lead_" + crypto.randomUUID().replaceAll("-", ""),
        expectedRevision: detail.task.leadRevision,
        expectedName: detail.task.orchestrator || "",
        previousAgentId: previous?.id || "",
        previousRunId: previous?.runId || "",
      };
      const eligible = detail.agents.filter(
        (a) =>
          !a.role &&
          !a.workItem &&
          a.online &&
          ["running", "done", "needs_input"].includes(a.status) &&
          a.name.toLowerCase() !== detail.task.orchestrator?.toLowerCase(),
      );
      const server =
        (previous && matchServer(previous.host, taskServers())) ||
        host.currentServer() ||
        host.getServers()[0];
      host.dialog(
        `Replace lead · ${detail.task.name}`,
        `<form id="replace-lead-form"><p class="fine">Current lead: ${esc(detail.task.orchestrator || "Not assigned")}${previous ? (previous.online ? " · online" : " · offline") : ""}</p><label>Replacement<select id="lead-candidate"><option value="">Start a new agent…</option>${eligible.map((a) => `<option value="${esc(a.id)}">${esc(a.name)} · ${esc(a.host)}</option>`).join("")}</select></label><p class="fine">Choose an online agent without an existing work-item assignment, or start a new one. The previous lead’s session and history stay available.</p><div id="lead-launch"><label>Server<select id="task-server">${serverOptions(server?.id)}</select></label>${agentFields(server)}</div><p id="task-error" class="fine" role="status"></p><div class="dialog-actions"><button type="button" id="lead-refresh">Refresh</button><button type="submit" id="lead-submit" class="primary">Start replacement</button></div></form>`,
      );
      const form = document.querySelector("#replace-lead-form");
      const select = form.querySelector("#lead-candidate");
      const button = form.querySelector("#lead-submit");
      const error = form.querySelector("#task-error");
      wireAgentFields();
      let suffix = 2;
      while (
        detail.agents.some((a) => a.name.toLowerCase() === `lead-${suffix}`)
      )
        suffix++;
      form.querySelector("#agent-name").value = `lead-${suffix}`;
      form.querySelector("#agent-cwd").value = previous?.cwd || "";
      form.querySelector("#task-server").onchange = () => {
        const target = host
          .getServers()
          .find((s) => s.id === form.querySelector("#task-server").value);
        form.querySelector("#agent-cwd").value = "";
        form.querySelector("#agent-runtime").innerHTML = runtimeOptions(target);
        wireAgentFields();
      };
      const pending = journal?.members[0];
      if (pending) {
        select.value = pending.fields.agentId;
        select.disabled = true;
        form.querySelector("#lead-launch").hidden = true;
        const agent = detail.agents.find(
          (a) => a.id === pending.fields.agentId,
        );
        if (
          agent?.runId &&
          pending.agent?.runId &&
          agent.runId !== pending.agent.runId
        )
          throw new Error(
            "The saved replacement run changed. Inspect the project before continuing.",
          );
        error.textContent = `Saved replacement: ${pending.fields.name}. ${agent?.online ? "Ready to assign." : "Refresh after it comes online. An uncertain launch is never repeated automatically."}`;
        button.textContent =
          pending.state === "unstarted" ? "Retry saved launch" : "Make lead";
        button.disabled =
          pending.state !== "unstarted" &&
          !eligible.some((a) => a.id === pending.fields.agentId);
        if (detail.task.orchestrator === pending.fields.name) {
          button.disabled = true;
          error.textContent =
            "This replacement is already the recorded lead. Its launch history is preserved.";
        }
      }
      select.onchange = () => {
        form.querySelector("#lead-launch").hidden = !!select.value;
        button.textContent = select.value ? "Make lead" : "Start replacement";
      };
      form.querySelector("#lead-refresh").onclick = () => replaceLead(taskId);
      let assignment = null;
      form.onsubmit = async (event) => {
        event.preventDefault();
        if (button.disabled) return;
        button.disabled = true;
        form.querySelector("#lead-refresh").disabled = true;
        try {
          if (scope !== (await launchScope()))
            throw new Error(
              "The hub or profile changed. Reopen lead recovery.",
            );
          const candidateId =
            journal?.members[0].fields.agentId || select.value;
          if (
            candidateId &&
            (!journal || journal.members[0].state !== "unstarted")
          ) {
            const current = await client.getTask(taskId);
            const candidate = current.agents.find((a) => a.id === candidateId);
            if (!candidate || !candidate.online)
              throw new Error(
                "The replacement is not online. Refresh before assigning it.",
              );
            if (journal) {
              const plan = await restoreLaunchMembers(
                journal,
                host.getServers(),
              );
              await launchMembers(taskId, plan, new Set(), () => {}, journal);
            }
            const chosenRun =
              journal?.members[0].agent?.runId ||
              eligible.find((a) => a.id === candidateId)?.runId;
            if (candidate.runId !== chosenRun)
              throw new Error(
                "The selected replacement run changed. Refresh before assigning it.",
              );
            assignment ||= {
              ...expected,
              agentId: candidate.id,
              runId: chosenRun,
            };
            const result = await client.assignLead(taskId, assignment);
            // The receipt may describe an earlier committed retry: refresh the
            // authoritative project before displaying its current assignment.
            detail = await client.getTask(taskId);
            cache.set(taskId, detail);
            const feed = feeds.get(taskId);
            if (feed) {
              feed.task = detail.task;
              feed.agents = detail.agents;
            }
            if (journal) {
              await guardedJournalEffect(journal, null, () =>
                host.api(`/team-launch-plans/${journal.id}`, "DELETE"),
              );
              await host.reloadData();
            }
            host.closeDialog();
            host.render();
            host.notice(
              `Lead assignment saved for ${result.task.orchestrator}. Current lead: ${detail.task.orchestrator}.`,
            );
            return;
          }
          const current = await client.getTask(taskId);
          if (
            current.task.leadRevision !== expected.expectedRevision ||
            current.task.orchestrator !== expected.expectedName
          )
            throw new Error(
              "The project lead changed. Refresh before launching a replacement.",
            );
          let plan;
          if (journal)
            plan = await restoreLaunchMembers(journal, host.getServers());
          else {
            const target = host
              .getServers()
              .find((s) => s.id === form.querySelector("#task-server").value);
            if (!target) throw new Error("Choose a server.");
            const fields = readAgentFields();
            if (
              current.agents.some(
                (a) => a.name.toLowerCase() === fields.name.toLowerCase(),
              )
            )
              throw new Error("Choose a fresh name for the replacement lead.");
            fields.prompt =
              "You are a replacement lead candidate. Wait for an explicit saved lead-assignment Board notification before doing project work. Then run tt brief to load your orchestrator role and ask the database handler for the current project handoff. Until assignment, do not act on the worker role in this startup briefing.\n" +
              fields.prompt;
            // Validate before persisting any launch intent.
            agentSpawnCommand({ hub: client.base, task: taskId, ...fields });
            plan = [{ server: target, fields }];
            journal = await prepareLaunchJournal(
              "replace-lead",
              taskId,
              plan,
              "",
              undefined,
              expected,
            );
            await saveLaunchJournal(journal);
            await host.reloadData();
          }
          await launchMembers(
            taskId,
            plan,
            new Set(),
            (text) => {
              error.textContent = text;
            },
            journal,
          );
          await host.reloadData();
          await replaceLead(taskId);
        } catch (e) {
          error.textContent = formatError(e);
          // Freeze an uncertain assignment or launch. Retry uses the exact saved
          // identity and payload; Refresh restores the encrypted launch journal.
          if (journal || assignment) {
            select.disabled = true;
            form
              .querySelectorAll(
                "#lead-launch input, #lead-launch select, #lead-launch textarea, #lead-launch button",
              )
              .forEach((c) => {
                c.disabled = true;
              });
          }
        } finally {
          button.disabled = false;
          form.querySelector("#lead-refresh").disabled = false;
        }
      };
    } catch (error) {
      host.notice(formatError(error));
    }
  }

  function addAgent(taskId = taskOfTab(host.currentTab()?.id)) {
    if (!requireHub()) return;
    if (!taskId) {
      host.notice("Choose a project first.");
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
      document.querySelector("#agent-cwd").value = "";
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
        const existingMembers = new Set(
          (info?.agents || [])
            .filter((agent) => agent.role !== "database_handler")
            .map((agent) => agent.name.toLowerCase()),
        );
        existingMembers.add(fields.name.toLowerCase());
        const agent = await spawn(taskId, target, {
          ...fields,
          plannedTeamMembers: existingMembers.size,
        });
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

  // The board lives in Board mode; the host switches modes and selects the project.
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
      const { task, agents } = await client.getTask(taskId);
      host.dialog(
        "Project settings",
        `<form id="task-settings"><label>Project name<input id="task-settings-name" maxlength="120" value="${esc(task.name)}" required></label><label>Objective<textarea id="task-settings-goal" rows="3" maxlength="8192">${esc(task.goal)}</textarea></label><label>Main orchestrator<input id="task-settings-orchestrator" maxlength="64" value="${esc(task.orchestrator || "")}" readonly placeholder="No lead assigned"></label><button type="button" id="task-settings-replace-lead">Replace lead</button><label class="check"><input id="task-settings-swarm" type="checkbox" ${task.swarm ? "checked" : ""}>Enable swarm</label><p class="fine">Every new message reaches all project agents. Existing messages keep their original delivery scope.</p><label class="check"><input id="task-settings-spawn" type="checkbox" ${task.allowAgentSpawn ? "checked" : ""}>Allow agents to add other agents</label><label>Max new agents<input id="task-settings-max-new-agents" type="number" min="0" max="32" step="1" value="${task.maxNewAgents ?? 2}" ${task.allowAgentSpawn ? "" : "disabled"}></label><p class="fine">Extra agents allowed per bug or feature, on top of that item's allocated team member(s). Closing an extra frees its slot; one item's extras never reduce another's. Agents you add manually do not count.</p><p class="fine">You can always add agents yourself. Turning this off prevents new helpers; existing agents keep running.</p><details class="dialog-details"><summary>Agents</summary><p class="fine">Close an accepted worker after its dependencies resolve. Retire only when you intentionally want to keep the same item session for follow-up.</p><div class="task-agent-lifecycle">${
          agents
            .filter(
              (a) =>
                !["closed", "exited"].includes(a.status) ||
                (a.status === "closed" && !a.cleanupDone),
            )
            .map((a) =>
              a.status === "closed"
                ? `<div data-agent-lifecycle-row="${esc(a.id)}"><span title="${esc(a.cleanupError || "Waiting for the saved host")}">${esc(a.name)} · Cleanup pending</span><button type="button" data-agent-cleanup="${esc(a.id)}">Retry cleanup</button></div>`
                : `<div><span>${esc(a.name)}${a.name.toLowerCase() === task.orchestrator?.toLowerCase() ? " · orchestrator" : ""}</span><button type="button" data-agent-retirement="${esc(a.id)}" data-retired="${a.status === "retired"}">${a.status === "retired" ? "Resume" : "Retire"}</button></div>`,
            )
            .join("") || '<p class="fine">No agents available.</p>'
        }</div></details><p id="task-settings-error" class="fine" role="alert"></p><div class="dialog-actions"><button type="submit" class="primary">Save</button></div></form>`,
      );
      const form = document.querySelector("#task-settings");
      form.querySelector("#task-settings-replace-lead").onclick = () =>
        replaceLead(taskId);
      form.querySelector("#task-settings-spawn").onchange = (e) =>
        (form.querySelector("#task-settings-max-new-agents").disabled =
          !e.target.checked);
      form.querySelectorAll("[data-agent-retirement]").forEach((button) => {
        button.onclick = async () => {
          const retired = button.dataset.retired === "true";
          button.disabled = true;
          const error = form.querySelector("#task-settings-error");
          error.textContent = "";
          try {
            await client.updateAgent(taskId, button.dataset.agentRetirement, {
              status: retired ? "done" : "retired",
            });
            button.dataset.retired = String(!retired);
            button.textContent = retired ? "Retire" : "Resume";
            host.notice(
              retired
                ? "Agent resumed; inbox wake-ups enabled."
                : "Agent retired; terminal retained.",
            );
          } catch (e) {
            error.textContent = formatError(e);
          } finally {
            button.disabled = false;
          }
        };
      });
      form.querySelectorAll("[data-agent-cleanup]").forEach((button) => {
        button.onclick = async () => {
          button.disabled = true;
          const error = form.querySelector("#task-settings-error");
          error.textContent = "";
          try {
            const result = await cleanupAgent(
              taskId,
              button.dataset.agentCleanup,
            );
            if (result.cleanupDone) {
              button.closest("[data-agent-lifecycle-row]")?.remove();
              host.notice("Agent session cleanup confirmed.");
            } else {
              error.textContent =
                result.cleanupErrors?.[0] || "Cleanup remains pending.";
            }
          } catch (e) {
            error.textContent = formatError(e);
          } finally {
            button.disabled = false;
          }
        };
      });
      form.onsubmit = async (event) => {
        event.preventDefault();
        const button = form.querySelector('button[type="submit"]');
        if (button.disabled) return;
        button.disabled = true;
        try {
          const task = await client.updateTask(taskId, {
            name: form.querySelector("#task-settings-name").value.trim(),
            goal: form.querySelector("#task-settings-goal").value.trim(),
            maxNewAgents: Number(
              form.querySelector("#task-settings-max-new-agents").value,
            ),
            allowAgentSpawn: form.querySelector("#task-settings-spawn").checked,
            swarm: form.querySelector("#task-settings-swarm").checked,
          });
          const info = cache.get(taskId);
          cache.set(taskId, { ...info, task });
          const feed = feeds.get(taskId);
          if (feed) feed.task = task;
          host.closeDialog();
          host.render();
          host.notice("Project settings saved.");
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
    rememberHost(server, parsed.host);
    return parsed.runtimes || [];
  }

  function commands() {
    const list = [];
    const tab = host.currentTab();
    const taskId = tab && taskOfTab(tab.id);
    list.push({ label: "Project hub: configure", run: configure });
    if (client) {
      list.push({ label: "Project: new…", run: () => newTask() });
      list.push({
        label: "Project: open terminals…",
        run: () => attachTask(),
      });
      if (taskId) {
        const name = cache.get(taskId)?.task?.name || taskId;
        list.push({
          label: `Project ${name}: board`,
          run: () => board(taskId),
        });
        list.push({
          label: `Project ${name}: add agent…`,
          run: () => addAgent(taskId),
        });
        list.push({
          label: `Project ${name}: settings`,
          run: () => settings(taskId),
        });
      }
    }
    return list;
  }

  async function pauseTask(taskId, changed = () => {}) {
    if (!requireHub()) return;
    try {
      await requireProjectPauseV1();
      const detail = await client.getTask(taskId);
      assertProjectLifecycle(detail.task, "active");
      const targets = detail.agents.filter(
        (agent) =>
          agent.runId &&
          (!["closed", "exited"].includes(agent.status) || !agent.cleanupDone),
      );
      const requestId = "pause_" + crypto.randomUUID().replaceAll("-", "");
      host.dialog(
        `Pause project · ${detail.task.name}`,
        `<form id="project-pause-form" data-testid="pause-project-confirmation"><p class="fine">Pause keeps this project, Board history, Bugs, Features, unfinished work and evidence. It dismisses every current exact-run team member, including the lead and database handler. The project cannot wake, schedule or admit agents until an explicit Resume.</p><p class="fine">Service handling is verified against saved exact-run ownership evidence. An unverified or unresolved handoff keeps the project in cleanup pending.</p><fieldset class="pause-targets"><legend>Exact team and service handoff</legend>${
          targets.length
            ? targets
                .map(
                  (agent) =>
                    `<div class="pause-target" data-pause-target="${esc(agent.id)}"><div><strong>${esc(agent.name)}</strong><span class="fine">${esc(agent.role === "database_handler" ? "Database handler" : agent.name.toLowerCase() === detail.task.orchestrator?.toLowerCase() ? "Lead" : "Team member")} · ${esc(agent.runId)}</span></div><label>Service handling<select data-pause-disposition="${esc(agent.id)}" aria-label="Service handling for ${esc(agent.name)}" required><option value="">Choose…</option><option value="none">No service to preserve</option><option value="transferred">Transferred to another owner</option><option value="detached">Detached and reverified</option><option value="unresolved">Unresolved · keep pause pending</option></select></label><label>Handoff note<input data-pause-note="${esc(agent.id)}" maxlength="512" placeholder="Required unless no service exists"></label><div class="pause-evidence" data-pause-evidence-fields="${esc(agent.id)}" hidden><label>Operational record<input data-pause-evidence-id="${esc(agent.id)}" placeholder="opr_…" autocomplete="off" spellcheck="false"></label><label>Version<input data-pause-evidence-version="${esc(agent.id)}" type="number" min="1" step="1"></label></div></div>`,
                )
                .join("")
            : '<p class="fine">No unsettled exact agent runs are attached to this project.</p>'
        }</fieldset><p id="project-pause-status" class="fine" role="status" aria-live="polite"></p><div class="dialog-actions"><button type="submit" class="danger" data-testid="pause-project-confirm">Pause project</button></div></form>`,
      );
      const form = document.querySelector("#project-pause-form"),
        button = form.querySelector('button[type="submit"]'),
        status = form.querySelector("#project-pause-status");
      let frozenRequest = null;
      form.querySelectorAll("[data-pause-disposition]").forEach((select) => {
        select.onchange = () => {
          const fields = form.querySelector(
            `[data-pause-evidence-fields="${select.dataset.pauseDisposition}"]`,
          );
          fields.hidden = !["transferred", "detached"].includes(select.value);
        };
      });
      form.onsubmit = async (event) => {
        event.preventDefault();
        if (button.disabled) return;
        status.textContent = "";
        try {
          if (!frozenRequest) {
            const dispositions = targets.map((agent) => {
              const serviceDisposition = form.querySelector(
                `[data-pause-disposition="${agent.id}"]`,
              )?.value;
              const handoffNote = form
                .querySelector(`[data-pause-note="${agent.id}"]`)
                ?.value.trim();
              if (!serviceDisposition)
                throw new Error(
                  `Choose service handling for ${agent.name} before pausing.`,
                );
              if (serviceDisposition !== "none" && !handoffNote)
                throw new Error(
                  `Add a handoff note for ${agent.name}, or choose No service to preserve.`,
                );
              const evidenceId = form
                .querySelector(`[data-pause-evidence-id="${agent.id}"]`)
                ?.value.trim();
              const evidenceVersion = Number(
                form.querySelector(
                  `[data-pause-evidence-version="${agent.id}"]`,
                )?.value,
              );
              if (
                ["transferred", "detached"].includes(serviceDisposition) &&
                (!/^opr_[a-f0-9]{16}$/.test(evidenceId || "") ||
                  !Number.isSafeInteger(evidenceVersion) ||
                  evidenceVersion < 1)
              )
                throw new Error(
                  `Enter the committed operational record and version for ${agent.name}.`,
                );
              return {
                agentId: agent.id,
                runId: agent.runId,
                serviceDisposition,
                handoffNote: handoffNote || "",
                ...(["transferred", "detached"].includes(
                  serviceDisposition,
                ) && {
                  serviceEvidence: {
                    id: evidenceId,
                    version: evidenceVersion,
                  },
                }),
              };
            });
            frozenRequest = {
              version: 1,
              requestId,
              expectedLifecycleGeneration: detail.task.lifecycleGeneration,
              ...(previousTeamID(detail) && {
                previousTeamId: previousTeamID(detail),
              }),
              targets: dispositions,
            };
            form
              .querySelectorAll("select,input")
              .forEach((control) => (control.disabled = true));
          }
          button.disabled = true;
          status.textContent =
            "Saving the pause barrier and exact team snapshot…";
          const result = assertPauseEvidence(
            await client.pauseTask(taskId, frozenRequest),
            frozenRequest.expectedLifecycleGeneration,
          );
          host.closeDialog();
          let cleaned = { task: { ...detail.task, ...result }, errors: [] };
          try {
            cleaned = await cleanupPauseTask(taskId);
          } catch (error) {
            cleaned.errors = [formatError(error)];
          }
          host.notice(
            cleaned.task.pauseState === "paused"
              ? `Project ${detail.task.name} is paused. Project history and unfinished work are preserved.`
              : `Project ${detail.task.name} pause saved · ${cleaned.task.pauseCleanupPending ?? result.cleanupPending} exact-run cleanup${(cleaned.task.pauseCleanupPending ?? result.cleanupPending) === 1 ? "" : "s"} pending.${cleaned.errors.length ? " " + cleaned.errors[0] : ""}`,
          );
          await changed();
        } catch (error) {
          status.textContent = formatError(error);
          status.setAttribute("role", "alert");
          if (frozenRequest) button.textContent = "Retry exact pause request";
          button.disabled =
            Number.isInteger(error?.status) &&
            error.status >= 400 &&
            error.status < 500;
        }
      };
      form.querySelector("select, button")?.focus();
    } catch (error) {
      host.notice("Pause unavailable: " + formatError(error));
    }
  }

  async function resolvePauseHandoff(taskId, changed = () => {}) {
    if (!requireHub()) return;
    try {
      await requireProjectPauseV1();
      const [detail, rawPause] = await Promise.all([
        client.getTask(taskId),
        client.getTaskPause(taskId),
      ]);
      if (detail.task.pauseState !== "cleanup_pending")
        throw new Error("Project service handoff is not pending.");
      const pause = assertPauseSnapshot(rawPause, detail.task);
      const targets = pause.targets.filter(
        (target) =>
          !target.serviceVerified || target.serviceDisposition === "unresolved",
      );
      if (!targets.length)
        throw new Error("No unresolved service handoff remains.");
      host.dialog(
        `Resolve service handoff · ${detail.task.name}`,
        `<form id="project-pause-handoff-form" data-testid="pause-handoff-form"><p class="fine">A project becomes fully paused only after the hub verifies each exact run’s service handling. Transferred or detached services require a committed operational record from that same run; browser text alone is not evidence.</p><label>Exact run<select id="pause-handoff-target">${targets.map((target) => `<option value="${esc(target.agentId)}">${esc(target.name)} · ${esc(target.runId)}</option>`).join("")}</select></label><label>Resolution<select id="pause-handoff-disposition" required><option value="none">No service · exact cleanup confirmed</option><option value="transferred">Transferred to another owner</option><option value="detached">Detached and reverified</option></select></label><label>Handoff note<input id="pause-handoff-note" maxlength="512" placeholder="Exact committed result summary"></label><div id="pause-handoff-evidence" class="pause-evidence" hidden><label>Operational record<input id="pause-handoff-evidence-id" placeholder="opr_…" autocomplete="off" spellcheck="false"></label><label>Version<input id="pause-handoff-evidence-version" type="number" min="1" step="1"></label></div><p id="pause-handoff-status" class="fine" role="status" aria-live="polite"></p><div class="dialog-actions"><button type="submit" class="primary" data-testid="pause-handoff-save">Save verified handoff</button></div></form>`,
      );
      const form = document.querySelector("#project-pause-handoff-form"),
        targetSelect = form.querySelector("#pause-handoff-target"),
        disposition = form.querySelector("#pause-handoff-disposition"),
        evidence = form.querySelector("#pause-handoff-evidence"),
        button = form.querySelector('button[type="submit"]'),
        status = form.querySelector("#pause-handoff-status");
      const renderEvidence = () => {
        evidence.hidden = !["transferred", "detached"].includes(
          disposition.value,
        );
      };
      disposition.onchange = renderEvidence;
      renderEvidence();
      let frozenRequest = null;
      form.onsubmit = async (event) => {
        event.preventDefault();
        if (button.disabled) return;
        status.textContent = "";
        try {
          if (!frozenRequest) {
            const target = targets.find(
              (candidate) => candidate.agentId === targetSelect.value,
            );
            if (!target)
              throw new Error("Choose an exact pause target to resolve.");
            const serviceDisposition = disposition.value;
            const handoffNote = form
              .querySelector("#pause-handoff-note")
              .value.trim();
            if (serviceDisposition === "none" && !target.cleanupDone)
              throw new Error(
                "Exact-run cleanup is not confirmed. Retry cleanup before recording no service.",
              );
            let serviceEvidence;
            if (["transferred", "detached"].includes(serviceDisposition)) {
              const id = form
                .querySelector("#pause-handoff-evidence-id")
                .value.trim();
              const version = Number(
                form.querySelector("#pause-handoff-evidence-version").value,
              );
              if (
                !handoffNote ||
                !/^opr_[a-f0-9]{16}$/.test(id) ||
                !Number.isSafeInteger(version) ||
                version < 1
              )
                throw new Error(
                  "Enter the exact committed operational record, version and matching result summary.",
                );
              serviceEvidence = { id, version };
            }
            frozenRequest = {
              version: 1,
              requestId:
                "pause_handoff_" + crypto.randomUUID().replaceAll("-", ""),
              pauseGeneration: pause.pauseGeneration,
              agentId: target.agentId,
              runId: target.runId,
              serviceDisposition,
              handoffNote,
              ...(serviceEvidence && { serviceEvidence }),
            };
            form
              .querySelectorAll("select,input")
              .forEach((control) => (control.disabled = true));
          }
          button.disabled = true;
          status.textContent = "Verifying the exact-run service evidence…";
          const result = assertPauseSnapshot(
            await client.resolvePauseHandoff(taskId, frozenRequest),
            detail.task,
          );
          if (
            result.receipt?.operation !== "handoff" ||
            result.receipt.requestId !== frozenRequest.requestId ||
            !result.targets.some(
              (target) =>
                target.agentId === frozenRequest.agentId &&
                target.runId === frozenRequest.runId &&
                target.serviceVerified === true &&
                target.serviceDisposition === frozenRequest.serviceDisposition,
            )
          )
            throw new Error(
              "The hub did not return a matching exact-run handoff receipt.",
            );
          host.closeDialog();
          host.notice(
            result.state === "paused"
              ? `Project ${detail.task.name} is fully paused.`
              : `Verified service handoff saved · ${result.handoffPending} remaining.`,
          );
          await changed();
        } catch (error) {
          status.textContent = formatError(error);
          status.setAttribute("role", "alert");
          if (frozenRequest) button.textContent = "Retry exact handoff request";
          button.disabled =
            Number.isInteger(error?.status) &&
            error.status >= 400 &&
            error.status < 500;
        }
      };
      targetSelect.focus();
    } catch (error) {
      host.notice("Handoff unavailable: " + formatError(error));
    }
  }

  async function resumeTask(taskId, changed = () => {}) {
    if (!requireHub()) return;
    try {
      await requireProjectPauseV1();
      const [detail, rawPause] = await Promise.all([
        client.getTask(taskId),
        client.getTaskPause(taskId),
      ]);
      const teams = host.getData().teams || [];
      if (!teams.length)
        throw new Error("Save a team before resuming this project.");
      if (!["paused", "resuming", "active"].includes(detail.task.pauseState))
        throw new Error("The project is not ready for Resume.");
      if (detail.task.pauseState === "paused") {
        assertProjectLifecycle(detail.task, "paused");
        assertPauseSnapshot(rawPause, detail.task);
      }
      const mainServer = host.currentServer() || host.getServers()[0];
      const priorTeam = rawPause.previousTeam?.teamId || "";
      const suggested = teams.some((team) => team.id === priorTeam)
        ? priorTeam
        : "";
      host.dialog(
        `Resume project · ${detail.task.name}`,
        `<form id="project-resume-form" data-testid="resume-project-form"><p class="fine">Resume keeps the retained project handoff and prior run history, then starts fresh identities. The previous team is only a suggested default; choose any saved team before continuing.</p><label>Team<select id="resume-team" data-testid="resume-team">${teams.map((team) => `<option value="${esc(team.id)}" ${team.id === suggested ? "selected" : ""}>${esc(team.name)} · ${team.members.length} agents${team.id === suggested ? " · Previous team (suggested)" : ""}</option>`).join("")}</select></label><label id="resume-main-machine" hidden>Main machine<select id="resume-main-server">${serverOptions(mainServer?.id)}</select><span class="fine">Used by team members without an assigned machine.</span></label><div id="resume-project-folders"></div><p class="fine">A fresh database handler is included. Old identities stay closed and are never resurrected.</p><p id="project-resume-status" class="fine" role="status" aria-live="polite"></p><div class="dialog-actions"><button type="submit" class="primary" data-testid="resume-project-confirm">Resume with fresh team</button></div></form>`,
      );
      const form = document.querySelector("#project-resume-form"),
        teamSelect = form.querySelector("#resume-team"),
        mainSelect = form.querySelector("#resume-main-server"),
        mainWrap = form.querySelector("#resume-main-machine"),
        button = form.querySelector('button[type="submit"]'),
        status = form.querySelector("#project-resume-status");
      let team = null,
        plan = null,
        journal = null,
        pause = rawPause;
      const progress = new Set();
      const projects = teamProjectFolders(
        form.querySelector("#resume-project-folders"),
        () => team,
        () => mainSelect.value,
      );
      const persistResumeConfirmation = async (receiptId, generation) => {
        const confirmed = structuredClone(journal);
        confirmed.resume.state = "confirmed";
        confirmed.resume.resumeReceiptId = receiptId;
        confirmed.resume.confirmedLifecycleGeneration = generation;
        for (const member of confirmed.members) {
          member.fields.expectedLifecycleGeneration = generation;
          if (member.fields.agentId === confirmed.resume.orchestratorAgentId)
            member.fields.resumeReceiptId = receiptId;
        }
        await saveLaunchJournal(confirmed);
        journal = confirmed;
        plan = await restoreLaunchMembers(journal, host.getServers());
      };
      const selectTeam = () => {
        try {
          const saved = teams.find(
            (candidate) => candidate.id === teamSelect.value,
          );
          if (!saved) throw new Error("Choose a saved team.");
          team = resolveTeam(saved, host.getData().agentCatalog);
          mainWrap.hidden = !team.members.some((member) => !member.serverId);
          projects.render();
          status.textContent =
            team.id === suggested
              ? "Previous team suggested. Confirm or choose a different saved team."
              : "This team will launch with fresh identities.";
          button.disabled = false;
        } catch (error) {
          team = null;
          status.textContent = formatError(error);
          button.disabled = true;
        }
      };
      teamSelect.onchange = selectTeam;
      mainSelect.onchange = projects.render;
      selectTeam();
      const scope = await launchScope();
      const savedJournal = (host.getData().teamLaunchPlans || []).find(
        (candidate) =>
          candidate.kind === "resume-project" &&
          candidate.taskId === taskId &&
          candidate.scope === scope,
      );
      if (!savedJournal && detail.task.pauseState !== "paused") {
        host.closeDialog();
        throw new Error(
          "The project has a resume admission but its encrypted launch plan is unavailable. Restore the original browser vault before continuing.",
        );
      }
      if (savedJournal) {
        journal = structuredClone(savedJournal);
        await assertLaunchScope(journal);
        const savedTeam = teams.find(
          (candidate) => candidate.id === journal.teamId,
        );
        if (!savedTeam)
          throw new Error(
            "The saved resume team is unavailable. Restore it before continuing.",
          );
        team = resolveTeam(savedTeam, host.getData().agentCatalog);
        teamSelect.value = team.id;
        teamSelect.disabled = true;
        mainSelect.disabled = true;
        plan = await restoreLaunchMembers(journal, host.getServers());
        pause = assertResumeSnapshot(rawPause, journal.resume, detail.task);
        if (
          journal.resume.state !== "confirmed" &&
          ["resuming", "active"].includes(pause.state)
        ) {
          await persistResumeConfirmation(
            pause.resumeAdmission.receiptId,
            pause.lifecycleGeneration,
          );
        }
        for (const member of journal.members)
          if (member.state === "started") progress.add(member.fields.name);
        projects.renderRetry(plan, journal);
        status.textContent =
          pause.state === "paused"
            ? "Recovered the frozen resume request. Retry sends its exact request ID before any launch."
            : pause.state === "resuming"
              ? "Recovered the exact saved resume admission. The fresh orchestrator has not been admitted yet."
              : "Recovered a partial fresh-team launch. Started identities stay fixed; only remaining members are retried.";
        button.textContent =
          journal.resume.state === "confirmed"
            ? "Retry remaining agents"
            : "Retry exact Resume";
      }
      const freeze = () => {
        teamSelect.disabled = true;
        mainSelect.disabled = true;
        form
          .querySelectorAll(
            "#resume-project-folders input, #resume-project-folders button",
          )
          .forEach((control) => (control.disabled = true));
      };
      const confirmResume = async () => {
        if (journal.resume.state === "confirmed") return;
        journal.resume.state = "uncertain";
        await saveLaunchJournal(journal);
        let result;
        try {
          result = assertResumeEvidence(
            await client.resumeTask(taskId, {
              version: 1,
              requestId: journal.resume.requestId,
              expectedPauseGeneration: journal.resume.expectedPauseGeneration,
              expectedLifecycleGeneration:
                journal.resume.expectedLifecycleGeneration,
              retainedHandoffDigest: journal.resume.retainedHandoffDigest,
              selectedTeamId: journal.resume.selectedTeamId,
              orchestrator: {
                agentId: journal.resume.orchestratorAgentId,
                runId: journal.resume.orchestratorRunId,
                name: journal.resume.orchestratorName,
              },
            }),
            journal.resume,
          );
        } catch (error) {
          if (
            Number.isInteger(error?.status) &&
            error.status >= 400 &&
            error.status < 500
          ) {
            await guardedJournalEffect(journal, null, () =>
              host.api(`/team-launch-plans/${journal.id}`, "DELETE"),
            );
            journal = null;
            await host.reloadData();
            host.closeDialog();
            await changed();
            host.notice(
              "Resume was rejected before admission. Refresh the retained project state before trying again.",
            );
          }
          throw error;
        }
        await persistResumeConfirmation(
          result.receipt.id,
          result.lifecycleGeneration,
        );
      };
      form.onsubmit = async (event) => {
        event.preventDefault();
        if (button.disabled) return;
        button.disabled = true;
        try {
          if (plan && journal)
            plan = await amendUnstartedFolders(
              plan,
              journal,
              projects.readRetry(),
            );
          if (!plan) {
            if (!team) throw new Error("Choose a saved team.");
            plan = withDatabaseHandler(
              teamLaunches(
                teams.find((candidate) => candidate.id === team.id),
                host.getServers(),
                mainSelect.value,
                projects.read(),
                null,
                host.getData().agentCatalog,
              ),
            );
            const nextLifecycleGeneration = pause.lifecycleGeneration + 1;
            for (const entry of plan)
              entry.fields.expectedLifecycleGeneration =
                nextLifecycleGeneration;
            const orchestrator = plan.find(
              (entry) => entry.fields.name === team.orchestrator,
            );
            if (!orchestrator || orchestrator.fields.agentRole)
              throw new Error(
                "The selected team does not contain its saved orchestrator.",
              );
            orchestrator.fields.agentId =
              "agt_" + crypto.randomUUID().replaceAll("-", "").slice(0, 16);
            orchestrator.fields.expectedRunId =
              "run_" + crypto.randomUUID().replaceAll("-", "").slice(0, 16);
            journal = await prepareLaunchJournal(
              "resume-project",
              taskId,
              plan,
              team.id,
              undefined,
              undefined,
              {
                state: "prepared",
                requestId: "resume_" + crypto.randomUUID().replaceAll("-", ""),
                expectedPauseGeneration: pause.pauseGeneration,
                expectedLifecycleGeneration: pause.lifecycleGeneration,
                retainedHandoffDigest: pause.retainedHandoffDigest,
                selectedTeamId: team.id,
                orchestratorAgentId: orchestrator.fields.agentId,
                orchestratorRunId: orchestrator.fields.expectedRunId,
                orchestratorName: orchestrator.fields.name,
              },
            );
            await saveLaunchJournal(journal);
            await host.reloadData();
            freeze();
          }
          status.textContent =
            journal.resume.state === "confirmed"
              ? "Launching the remaining fresh identities…"
              : "Saving the exact fresh orchestrator admission…";
          await confirmResume();
          await launchMembers(
            taskId,
            plan,
            progress,
            (text) => (status.textContent = text),
            journal,
          );
          await host.reloadData();
          sync();
          host.closeDialog();
          host.notice(
            `Project ${detail.task.name} resumed with fresh team ${team.name}. Prior identities and history remain preserved.`,
          );
          await changed();
        } catch (error) {
          status.textContent = formatError(error);
          status.setAttribute("role", "alert");
          if (journal) {
            freeze();
            button.textContent =
              journal.resume.state === "confirmed"
                ? "Retry remaining agents"
                : "Retry exact Resume";
            if (
              plan?.some((entry) =>
                journal.members.some(
                  (member) =>
                    member.fields.agentId === entry.fields.agentId &&
                    member.state === "unstarted",
                ),
              )
            )
              projects.renderRetry(plan, journal);
          }
        } finally {
          button.disabled = false;
        }
      };
      teamSelect.focus();
    } catch (error) {
      host.notice("Resume unavailable: " + formatError(error));
    }
  }

  async function cleanupPauseTask(taskId) {
    await requireProjectPauseV1();
    const [detail, rawPause] = await Promise.all([
      client.getTask(taskId),
      client.getTaskPause(taskId),
    ]);
    if (!["cleanup_pending", "paused"].includes(detail.task.pauseState))
      throw new Error("Project pause cleanup is not pending.");
    const pause = assertPauseSnapshot(rawPause, detail.task);
    const byID = new Map(detail.agents.map((agent) => [agent.id, agent]));
    const pending = [];
    for (const target of pause.targets) {
      const agent = byID.get(target.agentId);
      if (!agent || agent.runId !== target.runId)
        throw new Error(
          "A saved pause target no longer matches its exact run. No cleanup was attempted.",
        );
      if (!target.cleanupDone) pending.push(agent);
    }
    const errors = [];
    if (pending.length) {
      const cleanup = await cleanupAgents(taskId, pending);
      errors.push(...cleanup.errors);
    }
    const updated = await client.getTask(taskId);
    return { task: updated.task, errors };
  }

  async function cleanupAgents(taskId, pending) {
    if (pending.some((a) => !matchServer(a.host, taskServers())))
      await resolveAgentHosts();
    const hosts = new Map(),
      errors = [];
    for (const agent of pending) {
      const server = matchServer(agent.host, taskServers());
      if (!server) {
        errors.push(`${agent.host}: saved host unavailable`);
        continue;
      }
      const key = serverKey(server);
      if (!hosts.has(key)) hosts.set(key, { server, agents: [] });
      hosts.get(key).agents.push(agent.id);
    }
    await Promise.all(
      [...hosts.values()].map(async ({ server, agents }) => {
        try {
          const result = JSON.parse(
            await host.browserCommand(
              server,
              agentCleanupCommand(client.base, taskId, agents),
              16384,
            ),
          );
          for (const error of result.errors || [])
            errors.push(`${server.name || server.host}: ${error}`);
        } catch (error) {
          errors.push(`${server.name || server.host}: ${error.message}`);
        }
      }),
    );
    const updated = await client.getTask(taskId);
    return { updated, errors };
  }
  async function cleanupAgent(taskId, agentId) {
    const detail = await client.getTask(taskId);
    const agent = detail.agents.find((a) => a.id === agentId);
    if (!agent || agent.status !== "closed")
      throw new Error("Agent is not closed.");
    if (agent.cleanupDone) return agent;
    const { updated, errors } = await cleanupAgents(taskId, [agent]);
    const current = updated.agents.find((a) => a.id === agentId) || agent;
    return { ...current, cleanupErrors: errors };
  }
  async function cleanupTask(taskId) {
    const detail = await client.getTask(taskId);
    if (detail.task.status !== "closed")
      throw new Error("Project is still open.");
    const pending = detail.agents.filter((a) => !a.cleanupDone);
    const { updated, errors } = await cleanupAgents(taskId, pending);
    return { ...updated.task, cleanupErrors: errors };
  }
  async function closeTask(taskId) {
    const closed = await client.closeTask(taskId);
    try {
      return await cleanupTask(taskId);
    } catch (error) {
      return { ...closed, cleanupErrors: [error.message] };
    }
  }

  return {
    setupHandler,
    pauseTask,
    resolvePauseHandoff,
    resumeTask,
    cleanupPauseTask,
    closeTask,
    cleanupAgent,
    cleanupTask,
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
    viewClient: () => viewClient,
    dispose() {
      stopAll();
      viewClient?.dispose?.();
    },
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
    hasPendingResume: (taskId) =>
      (host.getData().teamLaunchPlans || []).some(
        (plan) => plan.kind === "resume-project" && plan.taskId === taskId,
      ),
    configure,
    newTask,
    inspectTools,
    attachTask,
    addAgent,
    replaceLead,
    addTeam,
    board,
    commands,
    refreshRuntimes,
    matchServer: (h) => matchServer(h, taskServers()),
  };
}
