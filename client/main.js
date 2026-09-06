import { credentialCache } from "./credential-cache.js";
import { createInactivityLock, IDLE_MINUTES } from "./inactivity.js";
import { createAppearancePreview } from "./appearance-preview.js";
import { normalizeTabDecoration, showTabDecoration } from "./tab-decoration.js";
let appearancePreview;
import { setupTerminalLinks } from "./terminal-links.js";
import { confirmDialog } from "./confirm-dialog.js";
import { setupVoiceDictation } from "./voice-dictation.js";
let voiceDictation;
import { setupPaneShortcuts } from "./pane-shortcuts.js";
import { setupLocalHistory } from "./local-history.js";
import {
  screenLines,
  hasNewText,
  activityTitle,
  connectionNeedsAttention,
} from "./activity.js";
import { tmuxHistoryCommand } from "../shared/tmux-command.js";
import { showCommandPalette } from "./command-palette.js";
import { setupMobileTerminal } from "./mobile-terminal.js";
import { showForgetDevice } from "./forget-device.js";
let imageUploads;
import {
  themes,
  fonts,
  readAppearance,
  saveAppearance,
  terminalAppearance,
  applyChrome,
} from "./appearance.js";
import { setupTerminalInput, fontShortcut } from "./terminal-input.js";
import { setupTooltips } from "./tooltips.js";
import "@fontsource/jetbrains-mono/latin-400.css";
import "@fontsource/ibm-plex-mono/latin-400.css";
import "./fonts.css";
import { setupTabStrip } from "./tab-strip.js";
import { setupPaneGroups } from "./pane-groups.js";
import { setupTerminalView } from "./terminal-view.js";
import { sidebarIcon } from "./sidebar-icons.js";
import logo from "./logo.svg?raw";
import { setupVaultReset } from "./vault-reset.js";
import { showSessionRename } from "./session-rename.js";
import {
  workspaceSnapshot,
  normalizeWorkspace,
  endpointKey,
  sameTarget,
  reconnectable,
  createReconnectController,
} from "./workspace-state.js";
import {
  terminalText,
  downloadBlob,
  showDiagnostics,
} from "./terminal-extras.js";
import { setupImageDrops } from "./image-upload.js";
import { resolveSessionName } from "./session-name.js";
import {
  tmuxCommand as remoteTmuxCommand,
  tmuxListCommand,
  validateSession,
} from "../shared/tmux-command.js";
import { loginPrompt, setKeyImporter } from "./login-prompts.js";
import {
  connectionTransport,
  isAuthenticationFailure,
  findPeer,
  discoveryMode,
} from "./connection-help.js";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { SearchAddon } from "@xterm/addon-search";
import wasmURL from "@tailscale/connect/main.wasm?url";
import "@xterm/xterm/css/xterm.css";
import "./style.css";
const $ = (s) => document.querySelector(s),
  $$ = (s) => [...document.querySelectorAll(s)];
const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const staticMode = import.meta.env.VITE_STATIC === "true";
const localVault = staticMode ? await import("./local-vault.js") : null;
const browserTransport = staticMode ? await import("./browser-ssh.js") : null;
if (staticMode) setKeyImporter((body) => api("/keys", "POST", body));
const owner = crypto.randomUUID();
let appearance = readAppearance();
let paneGroups;
let discoveryPending = false;
applyChrome(appearance);
setupTooltips();
let data = { servers: [], keys: [], sessions: [] },
  selected = null,
  serverFilter = null,
  tabs = [],
  active = null,
  ipn = null,
  netState = "Offline",
  peers = [],
  state = {},
  heartbeat,
  leaseDeadline = 0,
  persistQueue = Promise.resolve(),
  fontSize = appearance.fontSize;
const remoteSnapshots = new Map();
const pendingConnections = new Set();
const remoteRequests = new Map();
let connectionNumber = 0;
let tabStrip;
let restoring = false,
  workspaceReady = false,
  locking = false,
  workspaceTimer,
  savedWorkspace = "";
const reconnects = createReconnectController({
  eligible: (t) =>
    !locking &&
    !t.disposed &&
    t.tmux &&
    !!t.target &&
    navigator.onLine &&
    netState === "Running" &&
    endpointKey(t.server) ===
      endpointKey(data.servers.find((s) => s.id === t.server.id) || {}),
  changed: (t, message) => {
    if (!t.disposed) {
      t.retryMessage = message;
      refreshConnectionAttention(t);
      renderTabs();
    }
  },
  attempt: async (t, count) => {
    t.retryCount = count;
    t.restart(false);
  },
});
function refreshConnectionAttention(t) {
  // Error callbacks decide whether to retry immediately after updating status.
  // Evaluate the final state, not the transient Error before recovery is scheduled.
  queueMicrotask(() => {
    if (t.disposed || locking) return;
    const waitingForNetwork =
      t.transport === "browser" &&
      t.wasConnected &&
      t.tmux &&
      t.target &&
      (!navigator.onLine || netState !== "Running");
    if (connectionNeedsAttention(t, waitingForNetwork))
      t.markActivity?.("Needs attention");
    else if (t.activity === "Needs attention") {
      t.activity = "";
      renderTabs();
    }
  });
}
function scheduleWorkspaceSave() {
  if (!staticMode || !workspaceReady || restoring || locking) return;
  clearTimeout(workspaceTimer);
  workspaceTimer = setTimeout(
    () =>
      void flushWorkspace().catch((e) =>
        notice("Workspace could not be saved: " + e.message),
      ),
    150,
  );
}
async function flushWorkspace() {
  if (!staticMode || !workspaceReady || restoring) return;
  clearTimeout(workspaceTimer);
  const snapshot = workspaceSnapshot(
      tabs,
      paneGroups.model.groups,
      active,
      serverFilter,
    ),
    serialized = JSON.stringify(snapshot);
  if (serialized === savedWorkspace) return;
  await localVault.saveWorkspace(snapshot);
  savedWorkspace = serialized;
}
async function restoreWorkspace(value) {
  const snapshot = normalizeWorkspace(value);
  restoring = true;
  if ($("#workspace")) $("#workspace").dataset.restoring = "true";
  try {
    if (snapshot?.tabs.length) {
      notice("Restoring your terminal workspace...");
      for (const item of snapshot.tabs) {
        const server = data.servers.find((s) => s.id === item.serverId);
        if (!server || endpointKey(server) !== item.endpoint) continue;
        const t = await connect(server, item.tmux, item.session, {
          restoreId: item.id,
          resumeOnly: item.tmux,
          target: item.target,
          decoration: item.decoration,
        });
        if (t) await t.initialReady;
      }
      paneGroups.model.groups = snapshot.groups;
      paneGroups.sync();
      activate(
        tabs.some((t) => t.id === snapshot.active)
          ? snapshot.active
          : tabs[0]?.id,
      );
      notice(
        "Workspace restored. Plain SSH tabs open a fresh shell; tmux sessions resume.",
      );
    }
  } catch (e) {
    notice("Workspace restoration paused: " + e.message);
  } finally {
    serverFilter =
      snapshot?.serverFilter === null || !snapshot
        ? null
        : new Set(
            snapshot.serverFilter.filter((id) =>
              data.servers.some((s) => s.id === id),
            ),
          );
    if (!visibleTabs().length)
      selected = data.servers.find(
        (s) => serverFilter === null || serverFilter.has(s.id),
      )?.id;
    activate(
      visibleTabs().some((t) => t.id === snapshot?.active)
        ? snapshot.active
        : visibleTabs()[0]?.id,
    );
    restoring = false;
    if ($("#workspace")) $("#workspace").dataset.restoring = "false";
    workspaceReady = true;
    scheduleWorkspaceSave();
  }
}
async function api(url, method = "GET", body) {
  if (staticMode) return localVault.localAPI(url, method, body);
  const r = await fetch("/api" + url, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const d = await r.json();
  if (!r.ok) {
    if (r.status === 401 && $("#workspace")) location.reload();
    throw new Error(d.error || "Request failed");
  }
  return d;
}
function notice(message) {
  (document.fullscreenElement || $("#app")).append($("#notice"));
  $("#notice").textContent = message;
  $("#notice").hidden = false;
  clearTimeout(notice.timer);
  notice.timer = setTimeout(() => ($("#notice").hidden = true), 8000);
}
function guard(fn) {
  return (...args) => {
    try {
      return Promise.resolve(fn(...args)).catch((e) => notice(e.message));
    } catch (e) {
      notice(e.message);
    }
  };
}
const icon = `<span class="brand-icon" aria-hidden="true">${logo}</span>`;
$("#app").innerHTML =
  `<div id="notice" role="status" hidden></div><div id="lockscreen"><div class="login-brand">${icon} tailterm <span class="version">PREVIEW 01</span></div><section class="unlock-card"><span class="eyebrow">YOUR PRIVATE TERMINAL WORKSPACE</span><h1>Closer to<br>your servers.</h1><p>A real terminal. Your tailnet. Persistent sessions.<br>Everything you need, right here.</p><form id="unlock"><label for="password">Vault passphrase</label><input id="password" type="password" minlength="14" required autocomplete="current-password" placeholder="At least 14 characters"><button class="primary" id="unlock-button">Unlock workspace <span>↗</span></button></form><p class="fine" id="vault-hint">Checking encrypted vault…</p></section><div class="login-footer"><span>◈ Encrypted at rest</span><span>Powered by Tailscale + WebAssembly</span></div></div>`;
const inactivity = createInactivityLock({
  enabled: () => staticMode && !!$("#workspace"),
  minutes: () => appearance.idleMinutes,
  lock: guard(lock),
});
const status = await api("/status");
$("#vault-hint").textContent = status.initialized
  ? staticMode
    ? "Your passphrase unlocks the encrypted vault on this browser. It is never stored."
    : "Your passphrase decrypts the server vault. It is never stored."
  : "Create a passphrase to initialize your encrypted vault. Keep it safe: there is no password recovery.";
$("#unlock-button").firstChild.textContent = status.initialized
  ? "Unlock workspace "
  : "Create encrypted vault ";
if (staticMode && status.initialized) setupVaultReset(localVault.resetVault);
$("#unlock").onsubmit = async (e) => {
  e.preventDefault();
  const btn = $("#unlock-button");
  btn.disabled = true;
  try {
    data = await api("/unlock", "POST", { password: $("#password").value });
    $("#password").value = "";
    mount();
  } catch (e) {
    notice(e.message);
  } finally {
    btn.disabled = false;
  }
};
if (status.unlocked) {
  data = await api("/data");
  mount();
}
function mount() {
  inactivity.reset();
  const previousWorkspace = data.workspace;
  $("#lockscreen")?.remove();
  $("#app").insertAdjacentHTML(
    "beforeend",
    `<div id="workspace"><aside><div class="brand">${icon}<strong>tailterm</strong><span class="version">01</span></div><div class="sidebar-section"><span>SERVERS</span><button id="all-servers" aria-label="All servers" aria-pressed="true" title="Show sessions from all servers">All</button></div><input id="filter" class="filter" placeholder="⌕  Find a server…" aria-label="Find a server"><nav id="server-list"></nav><button id="discover" class="sidebar-discover">⌕ Discover devices</button><div class="sidebar-bottom"><button id="keys">♧ <span>SSH key vault</span><span id="key-count">0</span></button><button id="lock">↪ <span>Lock workspace</span><kbd>⇧⌘L</kbd></button></div></aside><main><header><div class="header-right"><span class="status-dot" id="tail-dot"></span><span id="tail-status">Tailnet offline</span><button id="tailscale-login">Connect Tailscale ↗</button></div></header><section class="terminal-shell"><div class="terminal-tabs"><div class="tab-strip"><button id="tabs-left" class="tab-scroll" aria-label="Scroll tabs left" title="Scroll tabs left" hidden>‹</button><div id="tabs" role="tablist" aria-label="SSH connections"></div><button id="tabs-right" class="tab-scroll" aria-label="Scroll tabs right" title="Scroll tabs right" hidden>›</button></div><button id="new-tab" class="icon-button" title="Start or resume a session">+</button><div class="terminal-tools"><button id="edit-server" title="Edit selected server" aria-label="Edit selected server">Edit server</button><button id="search-toggle" title="Find in terminal">⌕</button><button id="font-down" title="Smaller text">A−</button><button id="font-up" title="Larger text">A+</button><button id="appearance" title="Appearance\nThemes, fonts, cursor and spacing" aria-label="Appearance">◐</button><button id="fullscreen" title="Fullscreen">⛶</button></div></div><div id="search-bar" hidden><input id="terminal-search" placeholder="Find in scrollback" aria-label="Find in terminal"><button id="find-next">Next ↓</button><button id="search-close">×</button></div><div id="terminal-body"><div id="empty-terminal"><div class="session-launcher"><p id="server-filter-empty" class="fine" hidden></p><span class="eyebrow">YOUR REMOTE WORKSPACE</span><h2>Pick up where you left off.</h2><p class="launcher-intro">Choose a server, then open a fresh workspace or return to a running session.</p><div id="launcher-server" class="server-grid" role="group" aria-label="Session server"></div><div class="launch-section"><div class="launch-section-title"><span class="step-dot">＋</span><div><h3>Start fresh</h3><p>A persistent tmux workspace on <strong id="launch-target"></strong></p></div></div><div class="launch-new"><input id="launcher-name" placeholder="Optional name · leave blank for an automatic ID" aria-label="New tmux session name" maxlength="64"><button id="start-session" class="primary">＋ Start session</button></div></div><div class="resume-heading"><strong>Pick up a session</strong><button id="launcher-refresh" title="Refresh sessions\nQuery this server now; no sessions are changed.">↻ Refresh</button></div><p id="launcher-note"></p><div id="launcher-sessions"></div><div class="launcher-secondary"><button id="launcher-shell">Open plain SSH shell</button><button id="launcher-discover">⌕ Discover devices</button></div></div></div></div><div class="terminal-footer"><span id="terminal-status">○ No active connection</span><div><button id="copy">Copy</button><button id="paste">Paste</button><button id="clear">Clear</button><button id="reconnect">Reconnect</button><span id="dimensions">— × —</span></div></div></section></main></div><dialog id="dialog"></dialog>`,
  );
  if (staticMode) {
    $("#keys").insertAdjacentHTML(
      "afterend",
      '<button id="backup-vault">⇩ <span>Backup & restore</span></button>',
    );
    $("#backup-vault").onclick = backupDialog;
  }
  tabStrip = setupTabStrip();
  $("#keys").innerHTML =
    `${sidebarIcon("key")}<span class="nav-label">SSH keys</span><span id="key-count" class="nav-count">0</span>`;
  $("#lock").innerHTML =
    `${sidebarIcon("lock")}<span class="nav-label">Lock vault</span><kbd>⇧⌘L</kbd>`;
  $("#discover").innerHTML =
    `${sidebarIcon("discover")}<span>Discover devices</span>`;
  if ($("#backup-vault"))
    $("#backup-vault").innerHTML =
      `${sidebarIcon("backup")}<span class="nav-label">Backup & restore</span>`;
  paneGroups = setupPaneGroups({
    getTabs: () => tabs,
    getActive: () => active,
    isVisible: tabVisible,
    activate,
    close: closeTab,
    preferences: () => appearance,
    label: (t) => tabName(t, true),
  });
  const groupButton = document.createElement("button");
  groupButton.id = "group-tabs";
  groupButton.textContent = "▦";
  groupButton.title =
    "Group terminals\nDrag a tab onto another tab to tile them together. Manage groups here.";
  groupButton.setAttribute("aria-label", "Manage terminal groups");
  groupButton.onclick = groupDialog;
  $(".terminal-tools").prepend(groupButton);
  selected = data.servers[0]?.id;
  render();
  $("#appearance").onclick = appearanceDialog;
  $("#start-session").onclick = guard(() =>
    connect(currentServer(), true, $("#launcher-name").value),
  );
  $("#launcher-name").onkeydown = (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      $("#start-session").click();
    }
  };
  $("#launcher-shell").onclick = guard(() =>
    connect(currentServer(), false, ""),
  );
  $("#launcher-refresh").onclick = () => backgroundRemote(selected);
  if (selected) backgroundRemote(selected);
  $("#filter").oninput = renderSidebar;
  $("#all-servers").onclick = () => {
    serverFilter = null;
    applyServerFilter();
  };
  $("#edit-server").onclick = () => {
    const s = currentServer();
    if (s) serverDialog(s);
  };
  $("#new-tab").title = "Start or resume a session";
  $("#new-tab").onclick = () => selectServer(selected, true);
  $("#keys").title =
    "SSH keys\nGenerate, import and manage keys in your encrypted vault";
  $("#tailscale-login").title =
    "Tailscale\nConnect this browser to your tailnet";
  $("#tailscale-login").onclick = guard(startTailscale);
  $("#discover").onclick = $("#launcher-discover").onclick = guard(async () => {
    showPeers();
    await startTailscale();
    renderDiscoveredPeers();
  });
  $("#keys").onclick = keyDialog;
  $("#lock").onclick = guard(lock);
  $("#search-toggle").onclick = () => {
    $("#search-bar").hidden = !$("#search-bar").hidden;
    $("#terminal-search").focus();
  };
  $("#search-close").onclick = () => ($("#search-bar").hidden = true);
  $("#find-next").onclick = () =>
    currentTab()?.search.findNext($("#terminal-search").value);
  $("#terminal-search").onkeydown = (e) => {
    if (e.key === "Enter") $("#find-next").click();
  };
  $("#font-down").onclick = () => setFont(-1);
  $("#font-up").onclick = () => setFont(1);
  setupTerminalView();
  $("#clear").title = "Clear terminal\nClear the visible terminal buffer";
  $("#reconnect").title = "Reconnect\nReconnect this terminal session";
  $("#lock").title =
    "Lock vault\nDisconnect this browser and lock secure storage";
  $("#search-close").title = "Close terminal search";
  $("#find-next").title = "Find next match";
  $("#clear").onclick = () => currentTab()?.term.clear();
  $("#copy").onclick = guard(() => currentTab()?.copy());
  $("#paste").onclick = guard(() => currentTab()?.paste());
  $("#copy").title =
    "Copy terminal text\nShift + drag selects text through tmux. Copy mode also feeds this button.";
  $("#copy").dataset.shortcut = "⌘C / Ctrl+Shift+C";
  $("#paste").title =
    "Paste into this session\nMultiline text gets a preview. Bracketed paste is preserved.";
  $("#paste").dataset.shortcut = "⌘V / Ctrl+Shift+V";
  $("#font-up").dataset.shortcut = "⌘ / Ctrl + +";
  $("#font-down").dataset.shortcut = "⌘ / Ctrl + −";
  $("#new-tab").dataset.shortcut = "⌘ / Ctrl + Shift + K";
  $("#new-tab").setAttribute("aria-label", "Start or resume a session");
  $("#font-up").setAttribute("aria-label", "Larger text");
  $("#font-down").setAttribute("aria-label", "Smaller text");
  const diagnostics = document.createElement("button");
  diagnostics.id = "connection-diagnostics";
  diagnostics.title =
    "Connection diagnostics\nNetwork, SSH, host verification and reconnect status";
  diagnostics.textContent = "Diagnostics";
  diagnostics.onclick = () =>
    showDiagnostics({
      tab: currentTab(),
      server: currentTab()?.server || currentServer(),
      netState,
      peers,
      dialog,
    });
  $(".terminal-footer > div").prepend(diagnostics);
  const download = document.createElement("button");
  download.id = "download-scrollback";
  download.textContent = "Save output";
  download.title = "Save output\nDownload this terminal’s retained scrollback";
  download.onclick = () => {
    const t = currentTab();
    if (t)
      downloadBlob(
        new Blob([terminalText(t.term)], { type: "text/plain;charset=utf-8" }),
        `tailterm-${t.session || "shell"}-${Date.now()}.txt`,
      );
  };
  $(".terminal-footer > div").prepend(download);
  if (staticMode) void restoreWorkspace(previousWorkspace);
  if (staticMode)
    imageUploads = setupImageDrops({
      body: $("#terminal-body"),
      getTabs: () => tabs,
      getActive: () => active,
      dialog,
      close: closeDialog,
      notice,
      command: (t, command) =>
        browserTransport.browserCommand(ipn, t.server, peers, command),
      upload: (t, file, path, progress) => {
        if (
          endpointKey(t.server) !==
          endpointKey(data.servers.find((s) => s.id === t.server.id) || {})
        )
          throw new Error(
            "The server profile changed. Reconnect before uploading.",
          );
        return browserTransport.browserUpload(
          ipn,
          t.server,
          peers,
          file,
          path,
          progress,
        );
      },
    });
  voiceDictation ||= setupVoiceDictation({
    getActive: currentTab,
    available: () => !!$("#workspace") && !locking,
    notice,
  });
  const voiceButton = document.createElement("button");
  voiceButton.id = "voice-dictation";
  voiceButton.textContent = "Mic";
  voiceButton.title =
    "Local voice dictation (Option + Space / Shift + Option + V)";
  voiceButton.setAttribute("aria-label", "Voice dictation");
  voiceButton.onclick = () => voiceDictation.open();
  $(".terminal-footer > div").prepend(voiceButton);
  const commandsButton = document.createElement("button");
  commandsButton.id = "commands";
  commandsButton.textContent = "Commands";
  commandsButton.title = "Commands (Ctrl/Cmd + Shift + P)";
  commandsButton.onclick = openCommands;
  $(".header-right").prepend(commandsButton);
  setupMobileTerminal({
    current: currentTab,
    tabs: visibleTabs,
    activate,
    dialog,
    close: closeDialog,
  });
  if (staticMode) {
    const forgetButton = document.createElement("button");
    forgetButton.id = "forget-device";
    forgetButton.textContent = "Forget this device";
    forgetButton.onclick = () =>
      showForgetDevice({
        dialog,
        backup: backupDialog,
        forget: async () => {
          locking = true;
          credentialCache.clear();
          reconnects.clear();
          imageUploads?.cancel();
          voiceDictation?.cancel();
          tabs.forEach((t) => t.close?.());
          await persistQueue;
          try {
            await localVault.forgetDevice();
            workspaceReady = false;
          } catch (e) {
            locking = false;
            throw e;
          }
          localStorage.removeItem("tailserve.appearance");
          location.reload();
        },
      });
    $(".sidebar-bottom").append(forgetButton);
  }
  $("#reconnect").onclick = guard(() => {
    const t = currentTab();
    if (!t) return;
    if (
      staticMode &&
      t.restart &&
      endpointKey(t.server) === endpointKey(currentServer() || {})
    ) {
      reconnects.cancel(t.id);
      t.retryCount = 0;
      return t.restart(true);
    }
    return connect(
      data.servers.find((s) => s.id === t.server.id) || t.server,
      t.tmux,
      t.session,
      { replace: t, resumeOnly: t.tmux, target: t.target },
    );
  });
}
function currentServer() {
  return data.servers.find((s) => s.id === selected);
}
function currentTab() {
  return tabs.find((t) => t.id === active);
}
function render() {
  renderSidebar();
  const s = currentTab()?.server || currentServer();
  renderLauncher();
  $("#edit-server").disabled = !s;
  $("#key-count").textContent = data.keys.length;
  renderTabs();
  renderRemote();
  renderContext();
  renderClipboard();
  scheduleWorkspaceSave();
  renderBackupStatus();
}
function tabVisible(t) {
  return serverFilter === null || serverFilter.has(t.server.id);
}
function visibleTabs() {
  return tabs.filter(tabVisible);
}
function applyServerFilter(id) {
  if (id) {
    if (serverFilter === null) serverFilter = new Set([id]);
    else if (serverFilter.has(id)) serverFilter.delete(id);
    else serverFilter.add(id);
  }
  const next =
    currentTab() && tabVisible(currentTab()) ? currentTab() : visibleTabs()[0];
  if (!next) {
    selected =
      id && serverFilter?.has(id)
        ? id
        : data.servers.find(
            (s) => serverFilter === null || serverFilter.has(s.id),
          )?.id;
    if (selected) backgroundRemote(selected);
  }
  activate(next?.id);
  requestAnimationFrame(() =>
    (id
      ? document.querySelector(`[data-server="${CSS.escape(id)}"]`)
      : $("#all-servers")
    )?.focus(),
  );
}
function renderSidebar() {
  const q = $("#filter").value.toLowerCase();
  const servers = data.servers.filter((s) =>
    (s.name + s.host + s.group).toLowerCase().includes(q),
  );
  $("#all-servers").setAttribute("aria-pressed", String(serverFilter === null));
  $("#server-list").setAttribute("aria-label", "Filter sessions by server");
  $("#server-list").innerHTML =
    servers
      .map(
        (s) =>
          `<button class="server-item ${serverFilter?.has(s.id) ? "selected" : ""}" data-server="${esc(s.id)}" aria-pressed="${!!serverFilter?.has(s.id)}" title="Filter sessions from ${esc(s.name)}"><span class="node-icon">${sidebarIcon("server")}</span><div><strong>${esc(s.name)}</strong><small>${esc(s.group)} · ${s.mode === "ssh" ? "SSH" : "Auto"}</small></div><span class="node-dot ${tabs.some((t) => t.server.id === s.id && t.status === "Connected") ? "online" : ""}"></span></button>`,
      )
      .join("") ||
    (q
      ? '<p class="sidebar-empty">No matching servers.</p>'
      : '<p class="sidebar-empty">No servers yet.<br>Your workspace starts here.</p>');
  $$("[data-server]").forEach(
    (b) =>
      (b.onclick = () => {
        applyServerFilter(b.dataset.server);
      }),
  );
}
function renderTabs() {
  document.title = activityTitle(tabs);
  for (const t of tabs) t.history?.sync();
  document.body.classList.toggle("terminal-open", visibleTabs().length > 0);
  if (!$("#tabs")) return;
  paneGroups?.sync();
  const scrollPosition = $("#tabs").scrollLeft;
  $("#tabs").innerHTML = (
    paneGroups?.entries() || tabs.map((tab) => ({ tab, ids: [tab.id] }))
  )
    .map(({ tab: remembered, ids, group }) => {
      const t = ids.includes(active) ? currentTab() : remembered;
      const grouped = !!group && !group.tree.tab;
      const decoration = normalizeTabDecoration(
        grouped ? group.decoration : t.decoration,
      );
      const activity = ids
        .map((id) => tabs.find((t) => t.id === id)?.activity)
        .filter(Boolean)
        .join(", ");
      const names = ids.map((id) => {
        const member = tabs.find((t) => t.id === id);
        return tabName(member, true);
      });
      const groupName = [
        decoration.emoji,
        decoration.label || names.join(" + "),
      ]
        .filter(Boolean)
        .join(" ");
      const title = grouped
        ? `${groupName}\n${ids.length} panes: ${names.join(", ")}\nDrag onto another tab to merge groups. × closes the focused pane.`
        : `${tabName(t, true)}\n${t.server.username}@${t.server.host}:${t.server.port}\n${t.status}${t.tmux ? " · tmux launch: " + t.session : ""}\nDrop at a tab edge to reorder; drop in its center to group.`;
      return `<div class="tab ${ids.includes(active) ? "active" : ""} ${grouped ? "group-tab" : ""}" data-tab-color="${decoration.color}" data-tab-fill="${decoration.fill}" style='--tab-font:${esc(fonts[decoration.font]?.family || "var(--terminal-font)")}' draggable="false"><button data-tab="${t.id}" role="tab" aria-selected="${ids.includes(active)}" title="${esc(title)}"><span class="node-dot ${t.status === "Connected" ? "online" : ""}"></span><span class="tab-copy"><span class="tab-name">${grouped ? `<span class="pane-count">▦ ${ids.length}</span> ${esc(groupName)}` : `${esc(tabName(t, true))}`}</span><span class="tab-activity">${esc(activity)}</span><span class="tab-tmux">${grouped ? "Focused: " + esc(tabName(t)) + " · " : ""}${esc(t.status)}${t.tmux ? " · launch: " + esc(t.session) + (t.tmuxVerified ? "" : " (unverified)") : ""}</span></span></button>${staticMode ? `<button data-session-menu="${t.id}" aria-label="Session actions for ${esc(t.session)}" title="Session actions and tab order">...</button>` : ""}<button data-close="${t.id}" aria-label="Close ${esc(t.server.name)} ${grouped ? "focused pane" : "terminal"}">×</button></div>`;
    })
    .join("");
  $$("[data-tab]").forEach((b) => (b.onclick = () => activate(b.dataset.tab)));
  $$("[data-session-menu]").forEach(
    (b) =>
      (b.onclick = () => {
        const t = tabs.find((t) => t.id === b.dataset.sessionMenu);
        if (!t) return;
        const grouped = !paneGroups.model.group(t.id)?.tree.tab;
        dialog(
          "Session actions",
          `<div class="dialog-menu">${t.tmux ? '<button id="rename-tab-session">Rename session</button>' : ""}<button id="decorate-tab">${grouped ? "Group appearance" : "Tab appearance"}</button>${grouped ? '<button id="decorate-pane">Focused session appearance</button>' : ""}<button id="move-tab-left">Move left</button><button id="move-tab-right">Move right</button></div>`,
        );
        if (t.tmux)
          $("#rename-tab-session").onclick = () =>
            renameSession(t.server, t.session, t.target);
        const decorate = (parent) => {
          const currentGroup = paneGroups.model.group(t.id);
          if (parent && (!currentGroup || currentGroup.tree.tab)) return;
          showTabDecoration({
            tab: parent ? currentGroup : t,
            kind: parent ? "group" : "tab",
            name: parent
              ? paneGroups
                  .members(t.id)
                  .map((id) =>
                    tabName(
                      tabs.find((t) => t.id === id),
                      true,
                    ),
                  )
                  .join(" + ")
              : `${t.server.name} #${t.number}`,
            dialog,
            close: closeDialog,
            save: (value) => {
              if (parent) {
                const group = paneGroups.model.group(t.id);
                if (!group || group.tree.tab) return;
                group.decoration = value;
              } else t.decoration = value;
              renderTabs();
              paneGroups.render();
              scheduleWorkspaceSave();
            },
          });
        };
        $("#decorate-tab").onclick = () => decorate(grouped);
        if ($("#decorate-pane"))
          $("#decorate-pane").onclick = () => decorate(false);
        const entries = paneGroups.entries(),
          index = entries.findIndex((entry) => entry.ids.includes(t.id));
        $("#move-tab-left").disabled = index <= 0;
        $("#move-tab-right").disabled = index === entries.length - 1;
        $("#move-tab-left").onclick = () => {
          closeDialog();
          paneGroups.reorder(t.id, entries[index - 1].tab.id, false);
        };
        $("#move-tab-right").onclick = () => {
          closeDialog();
          paneGroups.reorder(t.id, entries[index + 1].tab.id, true);
        };
      }),
  );
  $$("[data-close]").forEach(
    (b) => (b.onclick = () => closeTab(b.dataset.close)),
  );
  $("#tabs").scrollLeft = scrollPosition;
  scheduleWorkspaceSave();
  tabStrip?.update();
  $("#empty-terminal").hidden = !!currentTab();
  paneGroups?.render();
  const t = currentTab();
  $("#reconnect").disabled =
    !t || !["Connected", "Disconnected", "Error"].includes(t.status);
  if ($("#download-scrollback")) $("#download-scrollback").disabled = !t;
  $("#dimensions").textContent = t
    ? `${t.term.cols} × ${t.term.rows}`
    : "— × —";
  $("#terminal-status").textContent = t
    ? `${t.status === "Connected" ? "●" : "○"} ${t.status} · ${t.server.username}@${t.server.host} · ${t.transport === "browser" ? "SSH over Tailscale" : t.transport === "wasm" ? "Tailscale WASM" : "SSH"}${t.tmux ? " · tmux launch: " + t.session : ""}`
    : "○ No active connection";
  if (t?.retryMessage)
    $("#terminal-status").textContent += " · " + t.retryMessage;
}
function renderClipboard() {
  const t = currentTab();
  if (!$("#copy")) return;
  $("#copy").textContent = t?.clipboardPending
    ? "Copy tmux selection ●"
    : "Copy";
  $("#copy").disabled = !t;
  $("#paste").disabled = !t || t.status !== "Connected";
}
function tabName(t, numbered = false) {
  const d = normalizeTabDecoration(t.decoration);
  return [
    d.emoji,
    d.label ||
      (data.servers.find((s) => s.id === t.server.id)?.name || t.server.name) +
        (numbered ? ` #${t.number}` : ""),
  ]
    .filter(Boolean)
    .join(" ");
}
function groupDialog() {
  const t = currentTab();
  if (!t) {
    notice(
      "Open a terminal first, then drag its tab onto another tab to group them.",
    );
    return;
  }
  const ids = paneGroups.members(t.id);
  const others = paneGroups.entries().filter((g) => !g.ids.includes(t.id));
  dialog(
    "Terminal groups",
    `<p>Choose terminals to group, or drag one tab onto another.</p>${
      ids.length > 1
        ? `<h3>This group · ${ids.length} panes</h3><div class="group-choices">${ids
            .map((id) => {
              const p = tabs.find((t) => t.id === id);
              return `<button data-separate="${id}">↗ Separate ${esc(tabName(p, true))}</button>`;
            })
            .join("")}</div>`
        : ""
    }<h3>Group with</h3><div class="group-choices">${others.map((g) => `<button data-merge="${g.tab.id}">▦ ${esc(tabName(g.tab, true))}${g.ids.length > 1 ? " · " + g.ids.length + " panes" : ""}</button>`).join("") || '<p class="fine">Open another terminal with + first.</p>'}</div><details class="dialog-details"><summary>Resizing & keyboard tips</summary><p class="fine">Drag dividers to resize. Focus a divider and use arrow keys; hold Shift for larger steps. Double-click or press Enter to reset that split. Closing a pane disconnects only that SSH connection; remote tmux keeps running.</p></details>`,
  );
  $$("[data-separate]").forEach(
    (b) =>
      (b.onclick = () => {
        $("#dialog").close();
        paneGroups.detach(b.dataset.separate);
      }),
  );
  $$("[data-merge]").forEach(
    (b) =>
      (b.onclick = () => {
        $("#dialog").close();
        paneGroups.merge(t.id, b.dataset.merge);
      }),
  );
}
function renderContext() {
  const t = currentTab(),
    profile = currentServer();
  $("#terminal-status").title = t
    ? `${t.server.username}@${t.server.host}:${t.server.port}${t.title ? " · Terminal title: " + t.title : ""}`
    : profile
      ? `${profile.username}@${profile.host}:${profile.port}`
      : "Select a server";
}
function selectServer(id, draft = false) {
  backgroundRemote(id);
  const existing =
    !draft && [...tabs].reverse().find((t) => t.server.id === id);
  if (existing) {
    activate(existing.id);
    return;
  }
  selected = id;
  active = null;
  tabs.forEach((t) => (t.el.hidden = true));
  $("#launcher-name").value = "";
  render();
}
function activate(id) {
  if (id && !visibleTabs().some((t) => t.id === id)) id = visibleTabs()[0]?.id;
  const previous = currentTab();
  if (previous)
    previous.activitySnapshot = screenLines(previous.term, previous.tmux);
  active = id;
  const t = currentTab();
  if (t) {
    t.activity = "";
    selected = t.server.id;
    $("#launcher-name").value = "";
  }
  render();
  if (t)
    requestAnimationFrame(() => {
      if (t.disposed) return;
      tabStrip?.reveal();
      tabs.filter((t) => !t.el.hidden).forEach((t) => t.fit.fit());
      if (t.history?.isOpen()) t.history.focus();
      else t.term.focus();
    });
}
function disposeTab(t, replacing = false) {
  reconnects.cancel(t.id);
  t.disposed = true;
  clearTimeout(t.activityTimer);
  t.history?.clear();
  t.close?.();
  t.observer.disconnect();
  t.term.dispose();
  t.el.remove();
  tabs = tabs.filter((x) => x !== t);
  if (!replacing) paneGroups?.sync();
}
function closeTab(id) {
  const t = tabs.find((t) => t.id === id);
  if (!t) return;
  const index = tabs.indexOf(t),
    wasActive = active === id;
  const sibling = paneGroups?.members(id).find((member) => member !== id);
  disposeTab(t);
  if (wasActive) {
    const visible = visibleTabs();
    const neighbor =
      visible.find((t) => t.id === sibling) ||
      visible[Math.min(index, visible.length - 1)];
    if (neighbor) activate(neighbor.id);
    else
      selectServer(
        data.servers.find((s) => s.id === selected)?.id || data.servers[0]?.id,
        true,
      );
  } else render();
}
function backgroundRemote(id) {
  const server = data.servers.find((s) => s.id === id);
  if (!server) return;
  void refreshRemote(server).catch(() => {});
}
async function refreshRemote(server, liveOverride) {
  if (remoteRequests.has(server.id)) return remoteRequests.get(server.id);
  const request = collectRemote(server, liveOverride);
  remoteRequests.set(server.id, request);
  renderRemote();
  try {
    return await request;
  } finally {
    remoteRequests.delete(server.id);
    renderRemote();
  }
}
async function collectRemote(server, liveOverride) {
  const focused = currentTab();
  const live =
    liveOverride ||
    (focused?.server.id === server.id && focused.status === "Connected"
      ? focused
      : null) ||
    tabs.find(
      (t) =>
        t.server.id === server.id && t.status === "Connected" && !t.disposed,
    );
  const endpoint = live?.server || server;
  try {
    const result = staticMode
      ? netState === "Running"
        ? await browserTransport.browserTmux(ipn, endpoint, peers)
        : (() => {
            throw new Error("Connect Tailscale to check this server.");
          })()
      : live?.transport === "ssh"
        ? await live.discover()
        : connectionTransport(endpoint, peers, netState === "Running") ===
            "wasm"
          ? await discoverTmux(endpoint)
          : await api(`/servers/${server.id}/tmux`);
    remoteSnapshots.set(server.id, {
      sessions: result.sessions,
      checked: new Date(),
      endpoint: `${endpoint.username}@${endpoint.host}:${endpoint.port}`,
    });
    if (staticMode)
      for (const t of tabs) {
        if (
          !t.tmux ||
          !t.target ||
          t.server.id !== server.id ||
          endpointKey(t.server) !== endpointKey(endpoint)
        )
          continue;
        const found = result.sessions.find((s) =>
          sameTarget(s.target, t.target),
        );
        if (found && t.session !== found.name) {
          const old = t.session;
          t.session = found.name;
          void localVault
            .renameSessionBookmark(server.id, old, found.name)
            .then((updated) => {
              data.sessions = updated.sessions;
            })
            .catch((e) => notice(e.message));
          renderTabs();
        }
      }
    renderRemote();
    return result;
  } catch (e) {
    remoteSnapshots.set(server.id, { error: e.message, checked: new Date() });
    renderRemote();
    throw e;
  }
}
function renderRemote() {
  renderLauncher();
}
function renameSession(server, name, target) {
  showSessionRename({
    name,
    serverName: server.name,
    dialog,
    close: () => {
      closeDialog();
      currentTab()?.term.focus();
    },
    rename: async (nextName) => {
      if (netState !== "Running")
        throw new Error("Connect Tailscale before renaming a session.");
      const current = data.servers.find((s) => s.id === server.id);
      if (
        !current ||
        ["host", "port", "username", "tmuxPath"].some(
          (key) => current[key] !== server[key],
        )
      )
        throw new Error(
          "The server profile changed. Reopen the session list before renaming.",
        );
      await browserTransport.browserRenameTmux(
        ipn,
        server,
        peers,
        name,
        nextName,
        target,
      );
      for (const t of tabs) {
        if (
          t.server.id === server.id &&
          t.tmux &&
          t.session === name &&
          ["host", "port", "username", "tmuxPath"].every(
            (key) => t.server[key] === server[key],
          )
        )
          t.session = nextName;
      }
      // An in-flight discovery may still contain the old name; finish it before refreshing.
      await remoteRequests.get(server.id)?.catch(() => {});
      remoteSnapshots.delete(server.id);
      try {
        const updated = await localVault.renameSessionBookmark(
          server.id,
          name,
          nextName,
        );
        data.sessions = updated.sessions;
      } catch (e) {
        notice(
          `Session renamed on the server, but its bookmark could not be saved: ${e.message}`,
        );
      }
      render();
      backgroundRemote(server.id);
    },
  });
}
function renderLauncher() {
  if (!$("#launcher-server")) return;
  const selector = $("#launcher-server");
  const empty = $("#server-filter-empty");
  empty.hidden = serverFilter === null || visibleTabs().length > 0;
  empty.textContent =
    serverFilter?.size === 0
      ? "No servers selected. Select servers in the sidebar or choose All."
      : "No open sessions match these server filters. Start a session below or choose All.";
  selector.innerHTML =
    data.servers
      .map((s) => {
        const count = tabs.filter(
          (t) => t.server.id === s.id && t.status === "Connected",
        ).length;
        return `<button class="server-card ${s.id === selected ? "chosen" : ""}" data-launch-server="${esc(s.id)}" aria-pressed="${s.id === selected}" title="${esc(s.username)}@${esc(s.host)}:${s.port}\nSelect to check this server’s tmux sessions"><span class="server-card-top"><span class="server-monogram">${esc(s.name.slice(0, 2).toUpperCase())}</span>${count ? `<span class="live-pill">${count} open</span>` : '<span class="server-check">↗</span>'}</span><strong>${esc(s.name)}</strong><small>${esc(s.username)}@${esc(s.host)}</small></button>`;
      })
      .join("") ||
    '<p class="fine">Discover a device to create a workspace.</p>';
  $$("[data-launch-server]").forEach(
    (b) => (b.onclick = () => selectServer(b.dataset.launchServer, true)),
  );
  $("#launch-target").textContent = currentServer()?.name || "your server";
  const server = currentServer(),
    snapshot = remoteSnapshots.get(selected);
  $("#start-session").disabled =
    $("#launcher-shell").disabled =
    $("#launcher-refresh").disabled =
      !server;
  $("#launcher-name").disabled = !server;
  $("#launcher-note").textContent = !server
    ? "Discover a device, set up its SSH connection, then start and resume sessions here."
    : remoteRequests.has(selected)
      ? "Checking this server for tmux sessions…"
      : snapshot?.error
        ? `Could not check sessions: ${snapshot.error} Connect using Start session or Open plain SSH shell to sign in, then refresh.`
        : snapshot?.sessions
          ? `Checked ${snapshot.checked.toLocaleTimeString()} · ${snapshot.sessions.length} sessions`
          : "Choose a server to check its sessions, or start a new workspace above.";
  $("#launcher-sessions").innerHTML =
    (snapshot?.sessions || [])
      .map(
        (s, i) =>
          `<div class="session-card"><button class="session-result" data-resume="${i}"><strong>${esc(s.name)}</strong><span>${s.windows} windows · ${s.attached} attached clients</span><span class="resume-action">Resume →</span></button>${staticMode ? `<button class="session-rename" data-rename-session="${i}" aria-label="Rename session ${esc(s.name)}">Rename</button>` : ""}</div>`,
      )
      .join("") ||
    (!remoteRequests.has(selected) && !snapshot?.error
      ? '<p class="fine">No sessions found yet. Start one with the button above.</p>'
      : "");
  $$("[data-resume]").forEach(
    (b) =>
      (b.onclick = guard(() =>
        connect(
          server,
          true,
          snapshot.sessions[Number(b.dataset.resume)].name,
          {
            resumeOnly: true,
            target: snapshot.sessions[Number(b.dataset.resume)].target,
          },
        ),
      )),
  );
  $$("[data-rename-session]").forEach(
    (b) =>
      (b.onclick = () =>
        renameSession(
          server,
          snapshot.sessions[Number(b.dataset.renameSession)].name,
          snapshot.sessions[Number(b.dataset.renameSession)].target,
        )),
  );
}
async function verifyTmux(t) {
  for (let attempt = 0; attempt < 3; attempt++) {
    await new Promise((resolve) => setTimeout(resolve, 400));
    if (t.disposed || t.status !== "Connected") return;
    try {
      const result = await refreshRemote(t.server, t);
      const remote = result.sessions.find((s) =>
        t.target ? sameTarget(s.target, t.target) : s.name === t.session,
      );
      if (!remote) continue;
      t.session = remote.name;
      t.target = remote.target;
      scheduleWorkspaceSave();
      if (t.disposed) return;
      t.tmuxVerified = true;
      const updated = await api("/sessions", "POST", {
        serverId: t.server.id,
        name: t.session,
        target: t.target,
      });
      data.sessions = updated.sessions;
      if (updated.backup) data.backup = updated.backup;
      render();
      return;
    } catch {}
  }
  if (!t.disposed) {
    t.tmuxVerified = false;
    render();
    notice(
      "SSH connected, but the requested tmux session could not be verified. Check the terminal output and remote sessions.",
    );
  }
}
function updateAppearance() {
  appearancePreview?.update(appearance);
  fontSize = appearance.fontSize;
  applyChrome(appearance);
  saveAppearance(appearance);
  for (const t of tabs) {
    Object.assign(t.term.options, terminalAppearance(appearance));
    if (!t.el.hidden)
      requestAnimationFrame(() => {
        if (!t.disposed) t.fit.fit();
      });
  }
  document.fonts.ready.then(() => {
    tabs.filter((t) => !t.el.hidden).forEach((t) => t.fit.fit());
  });
}
function setFont(delta) {
  appearance.fontSize =
    delta === 0 ? 14 : Math.min(32, Math.max(10, appearance.fontSize + delta));
  updateAppearance();
  const output = $("#font-size-value");
  if (output) output.textContent = appearance.fontSize + " px";
}
function appearanceDialog() {
  dialog(
    "Make it yours",
    `<p class="fine">Changes apply immediately and are saved on this browser.</p>
    <div id="appearance-preview" aria-label="Live terminal font and contrast preview"></div><p class="fine">Live terminal preview · includes light and dark app backgrounds.</p>
    <h3>Color palette</h3><div class="theme-grid">${Object.entries(themes)
      .map(
        ([id, t]) =>
          `<button class="theme-choice ${appearance.theme === id ? "chosen" : ""}" data-theme-choice="${id}" aria-pressed="${appearance.theme === id}" style="--sample-bg:${t.background};--sample-fg:${t.foreground};--sample-accent:${t.accent}"><span class="theme-sample"><span>❯</span> ssh workspace<span class="palette-dots">● ● ●</span></span><strong>${esc(t.name)}</strong></button>`,
      )
      .join("")}</div>
    <h3>Typeface</h3><div class="font-grid">${Object.entries(fonts)
      .map(
        ([id, f]) =>
          `<button data-font-choice="${id}" class="font-choice ${appearance.font === id ? "chosen" : ""}" aria-pressed="${appearance.font === id}"><strong>${esc(f.name)}</strong><span style='font-family:${esc(f.family)}'>0O 1il {} =&gt; ~/work${f.nerd ? " · &#xf07b; &#xe0a0; &#xf120;" : ""}</span><small>${esc(f.description)}</small></button>`,
      )
      .join("")}</div>
    <p class="fine">Fonts are bundled locally except System Mono. Nerd Font Mono symbols fit terminal cells. This renderer displays individual characters rather than programming ligatures.</p>
    <div class="appearance-controls"><div><label>Font size</label><div class="size-stepper"><button id="appearance-smaller" aria-label="Decrease font size">−</button><output id="font-size-value">${appearance.fontSize} px</output><button id="appearance-larger" aria-label="Increase font size">+</button><button id="appearance-reset">Reset</button></div></div><label>Line height<input id="line-height" type="range" min="1" max="1.6" step="0.05" value="${appearance.lineHeight}"></label><label>Padding<input id="terminal-padding" type="range" min="0" max="32" step="2" value="${appearance.padding}"></label></div>
    ${staticMode ? `<h3>Security</h3><label>Lock after inactivity<select id="idle-lock-minutes">${IDLE_MINUTES.map((n) => `<option value="${n}" ${appearance.idleMinutes === n ? "selected" : ""}>${n} minutes</option>`).join("")}</select></label><p class="fine">The vault locks when you stop interacting, including when you return after sleep. Temporary sign-in details expire five minutes after the last connection closes and are cleared when the vault locks.</p>` : ""}
    <h3>Cursor</h3><div class="segmented">${["block", "bar", "underline"].map((c) => `<button data-cursor="${c}" aria-pressed="${appearance.cursorStyle === c}">${c}</button>`).join("")}</div>
    <div class="appearance-toggles">${[
      ["cursorBlink", "Blink cursor"],
      ["focusFollowsMouse", "Focus terminal on hover"],
      ["copyOnSelect", "Copy on selection"],
      ["remoteClipboard", "Receive tmux clipboard (OSC 52)"],
    ]
      .map(
        ([k, label]) =>
          `<label><input type="checkbox" data-preference="${k}" ${appearance[k] ? "checked" : ""}>${label}</label>`,
      )
      .join("")}</div>
    <details class="dialog-details"><summary>Keyboard & selection tips</summary><p class="fine">Switch grouped panes with Option + Shift + arrow keys on Mac, or Ctrl + Alt + arrow keys on Windows/Linux.</p>
    <p class="fine">Hold Shift while dragging to select text when tmux handles the mouse. Plain Ctrl+C still interrupts a command. Remote clipboard read requests are never answered.</p></details>`,
  );
  appearancePreview = createAppearancePreview(
    $("#appearance-preview"),
    appearance,
  );
  const choose = (attribute, key) =>
    $$(`[${attribute}]`).forEach(
      (b) =>
        (b.onclick = () => {
          appearance[key] = b.getAttribute(attribute);
          updateAppearance();
          $$(`[${attribute}]`).forEach((x) => {
            const on = x === b;
            x.classList.toggle("chosen", on);
            x.setAttribute("aria-pressed", String(on));
          });
        }),
    );
  choose("data-theme-choice", "theme");
  choose("data-font-choice", "font");
  choose("data-cursor", "cursorStyle");
  if ($("#idle-lock-minutes"))
    $("#idle-lock-minutes").onchange = (e) => {
      appearance.idleMinutes = Number(e.target.value);
      updateAppearance();
      inactivity.check();
    };
  $("#appearance-smaller").onclick = () => setFont(-1);
  $("#appearance-larger").onclick = () => setFont(1);
  $("#appearance-reset").onclick = () => setFont(0);
  $("#line-height").oninput = (e) => {
    appearance.lineHeight = Number(e.target.value);
    updateAppearance();
  };
  $("#terminal-padding").oninput = (e) => {
    appearance.padding = Number(e.target.value);
    updateAppearance();
  };
  $$("[data-preference]").forEach(
    (el) =>
      (el.onchange = () => {
        appearance[el.dataset.preference] = el.checked;
        updateAppearance();
      }),
  );
}
function tmuxCommand(name, path, resumeOnly = false) {
  return "exec " + remoteTmuxCommand(name, path, resumeOnly) + "\r";
}
async function connect(
  server = currentServer(),
  tmux = true,
  session = $("#launcher-name").value,
  options = {},
) {
  if (!server) throw new Error("Add and select a server first.");
  if (!restoring && !options.replace && serverFilter !== null)
    serverFilter.add(server.id);
  if (tmux) session = resolveSessionName(session);
  if (tmux && !options.replace) {
    const existing = tabs.find(
      (t) =>
        !t.disposed &&
        t.server.id === server.id &&
        t.tmux &&
        t.session === session &&
        !["Disconnected", "Error"].includes(t.status),
    );
    if (existing) {
      activate(existing.id);
      return;
    }
  }
  const pendingKey = `${server.id}:${tmux ? session : "shell"}`;
  if (pendingConnections.has(pendingKey)) return;
  pendingConnections.add(pendingKey);
  try {
    let transport = staticMode
      ? netState === "Running"
        ? "browser"
        : "login"
      : connectionTransport(server, peers, netState === "Running");
    if (transport === "login") {
      await waitForTailscale();
      transport = staticMode
        ? "browser"
        : connectionTransport(server, peers, true);
    }
    const replacementIndex = options.replace
      ? tabs.indexOf(options.replace)
      : -1;
    if (options.replace) {
      if (options.replace.disposed) return;
      disposeTab(options.replace, true);
    }
    const el = document.createElement("div");
    el.className = "terminal-instance";
    $("#terminal-body").append(el);
    const term = new Terminal({
      ...terminalAppearance(appearance),
      scrollback: 15000,
      allowProposedApi: false,
    });
    const fit = new FitAddon(),
      search = new SearchAddon();
    term.loadAddon(fit);
    term.loadAddon(search);
    // FitAddon measures its immediate parent; keep pane padding and borders
    // outside that measured box so the last row stays inside the pane.
    const viewport = document.createElement("div");
    viewport.className = "terminal-viewport";
    el.append(viewport);
    term.open(viewport);
    setupTerminalLinks(term);
    const t = {
      id: options.replace?.id || options.restoreId || crypto.randomUUID(),
      number: options.replace?.number || ++connectionNumber,
      decoration: normalizeTabDecoration(
        options.replace?.decoration || options.decoration,
      ),
      server,
      transport,
      tmux,
      session,
      resumeOnly: !!options.resumeOnly,
      target: options.target,
      wasConnected: false,
      retryCount: 0,
      lastError: "",
      term,
      fit,
      search,
      el,
      status: "Connecting",
      send: null,
    };
    let initialDone;
    t.initialReady = new Promise((resolve) => {
      initialDone = resolve;
    });
    if (replacementIndex >= 0) tabs.splice(replacementIndex, 0, t);
    else tabs.push(t);
    t.observer = new ResizeObserver(() => {
      if (!t.el.hidden && !t.disposed) fit.fit();
    });
    t.observer.observe(el);
    activate(t.id);
    fit.fit();
    term.onTitleChange((title) => {
      t.title = title.slice(0, 200);
      if (active === t.id) renderContext();
    });
    term.onData((d) => t.send?.(d));
    const markActivity = (label) => {
      if (
        t.disposed ||
        (active === t.id &&
          document.visibilityState === "visible" &&
          document.hasFocus())
      )
        return;
      if (t.activity === label || (label === "New output" && t.activity))
        return;
      t.activity = label;
      renderTabs();
    };
    t.activitySnapshot = screenLines(term, tmux);
    term.onWriteParsed(() => {
      if (
        t.disposed ||
        (active === t.id &&
          document.visibilityState === "visible" &&
          document.hasFocus())
      )
        return;
      if (t.activityTimer) return;
      t.activityTimer = setTimeout(() => {
        t.activityTimer = null;
        if (t.disposed) return;
        const next = screenLines(term, tmux);
        if (hasNewText(t.activitySnapshot, next)) markActivity("New output");
        t.activitySnapshot = next;
      }, 500);
    });
    if (staticMode && tmux)
      t.history = setupLocalHistory(t, {
        capture: (tab) => {
          if (
            endpointKey(tab.server) !==
            endpointKey(data.servers.find((s) => s.id === tab.server.id) || {})
          )
            throw new Error(
              "Server profile changed. Reconnect before fetching history.",
            );
          return browserTransport.browserCommand(
            ipn,
            tab.server,
            peers,
            tmuxHistoryCommand(tab.target, tab.server.tmuxPath),
            1024 * 1024,
          );
        },
        notice,
      });
    t.markActivity = markActivity;
    term.onBell(() => markActivity("Bell"));
    term.parser.registerOscHandler(133, (sequence) => {
      if (sequence === "D" || sequence.startsWith("D;"))
        markActivity("Command finished");
      return false;
    });
    term.onResize(({ rows, cols }) => {
      t.activitySnapshot = screenLines(term, tmux);
      t.resize?.(rows, cols);
      if (active === t.id) $("#dimensions").textContent = `${cols} × ${rows}`;
    });
    setupTerminalInput(t, {
      pasteImages: (files) => imageUploads?.open(files, t),
      preferences: () => appearance,
      isActive: (t) => t.id === active,
      notice,
      setFont,
      changed: renderClipboard,
    });
    const update = (status) => {
      if (t.disposed) return;
      t.status = status;
      refreshConnectionAttention(t);
      renderTabs();
      renderSidebar();
      renderClipboard();
    };
    const ready = () => {
      if (t.disposed) return;
      reconnects.cancel(t.id);
      t.retryCount = 0;
      t.retryMessage = "";
      t.lastError = "";
      t.wasConnected = true;
      initialDone();
      update("Connected");
      refreshBackupStatus();
      if (tmux) void verifyTmux(t);
      else backgroundRemote(t.server.id);
    };
    const error = (e) => {
      if (t.disposed) return;
      t.lastError = String(e);
      initialDone();
      term.writeln(
        "\r\n\x1b[31m" + String(e).replace(/[\x00-\x1f\x7f]/g, " ") + "\x1b[0m",
      );
      update("Error");
    };
    const startStandardSSH = () => {
      if (t.disposed) return;
      t.transport = "ssh";
      t.send = null;
      update("Connecting");
      openStandardSSH(t, ready, error, update);
    };
    if (transport === "browser") {
      t.restart = (interactive = true) => {
        if (t.disposed || locking) return;
        const generation = (t.generation = (t.generation || 0) + 1);
        t.connection?.close();
        t.send = null;
        t.retryMessage = interactive ? "" : "Reconnecting...";
        update("Connecting");
        const live = () => !t.disposed && generation === t.generation;
        const connection = browserTransport.browserSSH(ipn, t.server, peers, {
          rows: term.rows,
          cols: term.cols,
          interactive,
          command: tmux
            ? remoteTmuxCommand(
                t.session,
                t.server.tmuxPath,
                t.wasConnected || options.resumeOnly,
                t.target,
              )
            : "",
          onData: (d) => {
            if (live()) {
              term.write(d);
            }
          },
          onInput: (fn) => {
            if (live()) t.send = fn;
          },
          onProgress: (message) => {
            if (live())
              update(
                message === "Verify host" || message === "Signing in"
                  ? message
                  : "Connecting",
              );
          },
          onConnected: () => {
            if (live()) {
              t.networkInterrupted = false;
              ready();
            }
          },
          onWarning: notice,
          onCredentialsSaved: refreshBackupStatus,
        });
        t.connection = connection;
        t.close = () => connection.close();
        t.resize = (r, c) => connection.resize(r, c);
        t.discover = () => browserTransport.browserTmux(ipn, t.server, peers);
        void connection.done
          .then((result) => {
            if (!live()) return;
            t.send = null;
            initialDone();
            if (result?.exitCode < 0 || t.networkInterrupted) {
              error("SSH connection closed unexpectedly.");
              reconnects.schedule(t, t.retryCount);
            } else if (result?.exitCode)
              error(
                "Remote process exited with status " + result.exitCode + ".",
              );
            else if (t.status !== "Error") update("Disconnected");
          })
          .catch((e) => {
            if (!live()) return;
            t.send = null;
            error(e.message);
            if (t.wasConnected && reconnectable(e.message))
              reconnects.schedule(t, t.retryCount);
            else
              t.retryMessage = "Reconnect manually after resolving the error";
          });
      };
      t.restart(true);
    } else if (transport === "wasm") {
      let fallingBack = false;
      try {
        const ssh = ipn.ssh(server.host, server.username, {
          rows: term.rows,
          cols: term.cols,
          timeoutSeconds: 20,
          writeFn: (d) => {
            if (!t.disposed) term.write(d);
          },
          writeErrorFn: (e) => {
            if (t.disposed) return;
            if (isAuthenticationFailure(e) && !t.wasmReady && !fallingBack) {
              fallingBack = true;
              term.writeln(
                "\r\nTailscale SSH unavailable. Continuing with standard SSH…",
              );
              queueMicrotask(() => {
                t.close?.();
                startStandardSSH();
              });
            } else if (!fallingBack) error(e);
          },
          setReadFn: (fn) => {
            t.send = fn;
            if (t.wasmReady && tmux && !t.tmuxSent) {
              t.tmuxSent = true;
              queueMicrotask(() =>
                t.send?.(
                  tmuxCommand(session, server.tmuxPath, options.resumeOnly),
                ),
              );
            }
          },
          onConnectionProgress: (m) => {
            update("Connecting");
            notice(m);
          },
          onConnected: () => {
            if (t.disposed) return;
            t.wasmReady = true;
            ready();
            if (tmux && t.send && !t.tmuxSent) {
              t.tmuxSent = true;
              queueMicrotask(() =>
                t.send?.(
                  tmuxCommand(session, server.tmuxPath, options.resumeOnly),
                ),
              );
            }
          },
          onDone: () => {
            if (fallingBack) return;
            t.send = null;
            if (t.status !== "Error") update("Disconnected");
          },
        });
        t.close = () => ssh.close();
        t.resize = (r, c) => ssh.resize(r, c);
      } catch (e) {
        error(e.message);
      }
    } else startStandardSSH();
    return t;
  } finally {
    pendingConnections.delete(pendingKey);
  }
}
function openStandardSSH(t, ready, error, update) {
  const { server, term, tmux, session } = t;
  const ws = new WebSocket(
    `${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}/terminal`,
  );
  const prompts = new AbortController(),
    requests = new Map();
  const send = (m) => {
    if (ws.readyState === 1) ws.send(JSON.stringify(m));
  };
  t.close = () => {
    prompts.abort();
    ws.close();
  };
  t.send = (d) => send({ type: "input", data: d });
  t.resize = (rows, cols) => send({ type: "resize", rows, cols });
  t.discover = () =>
    new Promise((resolve, reject) => {
      const id = crypto.randomUUID();
      const timer = setTimeout(() => {
        requests.delete(id);
        reject(new Error("Session discovery timed out"));
      }, 20000);
      requests.set(id, { resolve, reject, timer });
      send({ type: "tmux-list", id });
    });
  ws.onopen = () =>
    send({
      type: "connect",
      serverId: server.id,
      tmux,
      session,
      resumeOnly: t.resumeOnly,
      rows: term.rows,
      cols: term.cols,
    });
  ws.onmessage = async (e) => {
    if (t.disposed) return;
    const m = JSON.parse(e.data);
    if (m.type === "data")
      term.write(Uint8Array.from(atob(m.data), (c) => c.charCodeAt(0)));
    else if (m.type === "ready") {
      ready();
      api("/data")
        .then((d) => {
          data = d;
          render();
        })
        .catch((e) => notice(e.message));
    } else if (m.type === "error") error(m.data);
    else if (m.type === "prompt") {
      update(m.data.kind === "host-key" ? "Verify host" : "Signing in");
      const answer = await loginPrompt(m.data, prompts.signal);
      send({ type: "answer", id: m.data.id, ...answer });
    } else if (m.type === "tmux-result") {
      const pending = requests.get(m.data.id);
      if (pending) {
        clearTimeout(pending.timer);
        requests.delete(m.data.id);
        m.data.error
          ? pending.reject(new Error(m.data.error))
          : pending.resolve(m.data);
      }
    }
  };
  ws.onclose = () => {
    prompts.abort();
    for (const r of requests.values()) {
      clearTimeout(r.timer);
      r.reject(new Error("Connection closed"));
    }
    requests.clear();
    t.send = null;
    if (t.status !== "Error") update("Disconnected");
  };
  ws.onerror = () => error("Could not reach the SSH gateway.");
}
async function waitForTailscale() {
  await startTailscale();
  if (netState === "Running") return;
  notice(
    "Finish Tailscale sign-in; your SSH connection will continue automatically.",
  );
  await new Promise((resolve, reject) => {
    const started = Date.now();
    const timer = setInterval(() => {
      if (netState === "Running") {
        clearInterval(timer);
        if ($("#dialog")?.dataset.tailscaleLogin === "true") {
          closeDialog();
        }
        resolve();
      } else if (Date.now() - started > 180000) {
        clearInterval(timer);
        reject(
          new Error(
            "Tailscale sign-in timed out. Connect again when you are ready.",
          ),
        );
      }
    }, 250);
  });
}

function dialog(title, body) {
  appearancePreview?.dispose();
  appearancePreview = null;
  const d = $("#dialog");
  if (d.open) d.close();
  d.oncancel = null;
  d.onclose = () => {
    if (!d.open) {
      appearancePreview?.dispose();
      appearancePreview = null;
    }
  };
  (document.fullscreenElement || $("#app")).append(d);
  delete d.dataset.discovery;
  delete d.dataset.tailscaleLogin;
  d.innerHTML = `<div class="dialog-head"><h2 id="dialog-title">${esc(title)}</h2><button id="dialog-close" aria-label="Close dialog">×</button></div>${body}`;
  d.setAttribute("aria-labelledby", "dialog-title");
  $("#dialog-close").onclick = closeDialog;
  if (!d.open) d.showModal();
}
function closeDialog() {
  appearancePreview?.dispose();
  appearancePreview = null;
  $("#dialog").close();
  $("#dialog").replaceChildren();
}
function serverDialog(s = {}) {
  dialog(
    s.id ? "Edit server" : "Add server",
    `<form id="server-form"><div class="form-grid"><label>Display name<input name="name" value="${esc(s.name)}" placeholder="Production box" required maxlength="80"></label><label>Group<input name="group" value="${esc(s.group || "Personal")}" maxlength="40"></label></div><label>Connection<select name="mode"><option value="auto" ${!s.mode || s.mode === "auto" ? "selected" : ""}>Automatic authentication</option><option value="wasm" ${s.mode === "wasm" ? "selected" : ""}>Prefer Tailscale SSH</option><option value="ssh" ${s.mode === "ssh" ? "selected" : ""}>Standard SSH · key or password</option></select></label><div class="form-grid"><label>Hostname or IP<input name="host" value="${esc(s.host)}" placeholder="server.tailnet.ts.net" required></label><label>Port<input name="port" type="number" min="1" max="65535" value="${s.port || 22}" required></label></div><label>SSH username<input name="username" value="${esc(s.username || "ubuntu")}" required></label><div id="ssh-fields"><label>SSH key<select name="keyId"><option value="">Ask when needed</option>${data.keys.map((k) => `<option value="${esc(k.id)}" ${s.keyId === k.id ? "selected" : ""}>${esc(k.name)}</option>`).join("")}</select></label><details class="dialog-details" id="server-advanced" ${s.tmuxPath || s.fingerprint ? "open" : ""}><summary>Advanced connection options</summary><label>tmux executable (optional)<input name="tmuxPath" value="${esc(s.tmuxPath)}" placeholder="Auto-detect, or /absolute/path/to/tmux"></label><label>SSH host fingerprint (optional)<input name="fingerprint" value="${esc(s.fingerprint)}" placeholder="SHA256:…"></label><p class="fine">Get this through a trusted console on the remote host:<br><code>ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code><br>${staticMode ? "This hostname must be reachable through Tailscale, directly or via an approved subnet route." : "The application server must be able to reach this hostname."}</p></details></div><p class="fine" id="wasm-hint">${staticMode ? "Automatic tries Tailscale SSH, then a key or password." : "Automatic login uses Tailscale SSH when available, then standard SSH if needed."} You’ll verify the host on first connection.</p>${s.hasPassword ? '<label class="remember-password"><input type="checkbox" name="clearPassword"> Forget saved SSH password</label>' : ""}<div class="dialog-actions">${s.id ? '<button type="button" id="delete-server" class="danger">Delete server</button>' : ""}<button class="primary">Save server</button></div></form>`,
  );
  const form = $("#server-form"),
    mode = form.elements.mode;
  const toggle = () => {
    $("#ssh-fields").hidden = false;
    $("#wasm-hint").hidden = mode.value === "ssh";
  };
  mode.onchange = toggle;
  toggle();
  form.onsubmit = guard(async (e) => {
    e.preventDefault();
    const b = Object.fromEntries(new FormData(form));
    b.port = Number(b.port);
    b.tailnet = !!s.tailnet;
    b.clearPassword = !!form.elements.clearPassword?.checked;
    if (s.id) b.id = s.id;
    const d = await api("/servers", "POST", b);
    data = d;
    const id = s.id || d.servers.at(-1).id;
    remoteSnapshots.delete(id);
    closeDialog();
    if (currentTab()?.server.id === id) render();
    else selectServer(id, true);
  });
  if (s.id)
    $("#delete-server").onclick = guard(async () => {
      if (
        !(await confirmDialog({
          title: "Delete server?",
          message: `Remove ${s.name} from this vault and close its terminal connections? Remote tmux sessions keep running.`,
          action: "Delete server",
          destructive: true,
        }))
      )
        return;
      data = await api("/servers/" + s.id, "DELETE");
      for (const t of [...tabs].filter((t) => t.server.id === s.id))
        disposeTab(t);
      remoteSnapshots.delete(s.id);
      closeDialog();
      if (currentTab()) activate(active);
      else selectServer(data.servers[0]?.id, true);
    });
}
function keyDialog() {
  dialog(
    "SSH key vault",
    `<p class="fine">${staticMode ? "Private keys stay on this browser, encrypted with your vault passphrase." : "Private keys stay on the server, encrypted with your vault passphrase."}</p><div class="key-list">${data.keys.map((k) => `<div><strong>${esc(k.name)}</strong><button data-copy-public-key="${esc(k.id)}">Copy public key</button><button data-delete-key="${esc(k.id)}" title="Delete key" aria-label="Delete ${esc(k.name)}">×</button><code>${esc(k.fingerprint)}</code></div>`).join("") || "<p>No keys stored yet.</p>"}</div><div id="public-key-result" hidden><label>Public key<textarea id="public-key-text" readonly rows="3" spellcheck="false"></textarea></label><p class="fine" id="public-key-note"></p></div><form id="generate-key-form"><label>New key name<input name="name" required maxlength="80" placeholder="TrueNAS"></label><p class="fine">Create an Ed25519 key and protect it with your vault passphrase.</p><div class="dialog-actions"><button class="primary" id="generate-key">Generate & save key</button></div></form><details class="dialog-details" id="import-key"><summary>Import an existing key</summary><form id="key-form"><label>Key name<input name="name" required maxlength="80" placeholder="Personal Ed25519"></label><label>Import private key file<input id="key-file" type="file"></label><label>Private key<textarea name="privateKey" required rows="5" spellcheck="false" autocomplete="off" placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea></label><label>Key passphrase (if encrypted)<input name="passphrase" type="password" autocomplete="off"></label><div class="dialog-actions"><button class="primary">Encrypt & save key</button></div></form></details>`,
  );
  $("#generate-key-form").onsubmit = guard(async (e) => {
    e.preventDefault();
    const form = e.target,
      button = form.querySelector("button");
    button.disabled = true;
    button.textContent = "Generating…";
    try {
      data = await api("/keys/generate", "POST", {
        name: form.elements.name.value,
      });
      if (form.isConnected) {
        keyDialog();
        render();
        notice(
          "Key saved. Copy its public key to your server, then select this key in Edit server.",
        );
      }
    } finally {
      button.disabled = false;
      button.textContent = "Generate & save key";
    }
  });
  $$("[data-copy-public-key]").forEach((button) => {
    button.onclick = guard(async () => {
      button.disabled = true;
      try {
        const { publicKey } = await api(
          "/keys/" + button.dataset.copyPublicKey + "/public-key",
        );
        if (!button.isConnected) return;
        $("#public-key-result").hidden = false;
        $("#public-key-text").value = publicKey;
        const note = $("#public-key-note");
        note.textContent =
          "Paste this into your server account’s SSH public key field. In TrueNAS, edit your user and find SSH Public Key.";
        try {
          await navigator.clipboard.writeText(publicKey);
          button.textContent = "Copied";
        } catch {
          note.textContent =
            "Select and copy the public key above, then paste it into your server account’s SSH public key field.";
          $("#public-key-text")?.focus();
          $("#public-key-text")?.select();
        }
      } finally {
        button.disabled = false;
      }
    });
  });
  $("#key-file").onchange = guard(async (e) => {
    const file = e.target.files[0];
    if (file) {
      if (file.size > 64000) throw new Error("Key file is too large.");
      $("#key-form").elements.privateKey.value = await file.text();
    }
  });
  $("#key-form").onsubmit = guard(async (e) => {
    e.preventDefault();
    data = await api(
      "/keys",
      "POST",
      Object.fromEntries(new FormData(e.target)),
    );
    e.target.reset();
    keyDialog();
    render();
  });
  $$("[data-delete-key]").forEach(
    (b) =>
      (b.onclick = guard(async () => {
        if (
          !(await confirmDialog({
            title: "Delete SSH key?",
            message:
              "Remove this private key from the vault? Servers using it will need another key or password.",
            action: "Delete key",
            destructive: true,
          }))
        )
          return;
        data = await api("/keys/" + b.dataset.deleteKey, "DELETE");
        keyDialog();
        render();
      })),
  );
}
function safeAuthURL(raw) {
  const u = new URL(raw);
  if (u.protocol !== "https:" || u.hostname !== "login.tailscale.com")
    throw new Error("Unexpected Tailscale login URL");
  return u.href;
}
let starting = null;
async function startTailscale() {
  if (starting) return starting;
  if (ipn) {
    if (netState === "NeedsLogin") ipn.login();
    else notice("Tailscale: " + netState);
    return;
  }
  starting = (async () => {
    state = await api("/tailscale/claim", "POST", { owner });
    leaseDeadline = staticMode ? Infinity : Date.now() + 40000;
    $("#tail-status").textContent = "Loading WASM…";
    const fatal = (message) => {
      notice(message);
      for (const t of tabs.filter(
        (t) => t.transport === "wasm" || t.transport === "browser",
      ))
        t.close?.();
      clearInterval(heartbeat);
      setTimeout(() => location.reload(), 1500);
    };
    if (!staticMode)
      heartbeat = setInterval(async () => {
        if (Date.now() > leaseDeadline) {
          fatal("Tailscale lease expired; reloading to protect node identity.");
          return;
        }
        try {
          await api("/tailscale/claim", "POST", { owner });
          leaseDeadline = staticMode ? Infinity : Date.now() + 40000;
        } catch (e) {
          fatal(e.message);
        }
      }, 10000);
    const { createIPN } = staticMode
      ? await import("./wasm-runtime.js")
      : await import("@tailscale/connect");
    ipn = await createIPN({
      authKey: "",
      hostname: "tailterm-browser",
      wasmURL,
      panicHandler: fatal,
      stateStorage: {
        getState: (id) => state[id] || "",
        setState: (id, value) => {
          state[id] = value;
          const snapshot = { ...state };
          persistQueue = persistQueue
            .then(() =>
              api("/tailscale/state", "PUT", { owner, state: snapshot }),
            )
            .catch((e) =>
              fatal("Could not save Tailscale identity: " + e.message),
            );
        },
      },
    });
    ipn.run({
      notifyState: (s) => {
        netState = s;
        $("#tail-status").textContent =
          s === "Running" ? "Tailnet connected" : s;
        $("#tail-dot").classList.toggle("online", s === "Running");
        $("#tailscale-login").textContent =
          s === "Running"
            ? "Tailscale connected"
            : s === "NeedsLogin"
              ? "Sign in to Tailscale ↗"
              : "Tailscale ↗";
        $("#tailscale-login").title =
          s === "Running"
            ? "Tailscale connected\nThis browser is connected to your tailnet"
            : "Tailscale\n" + s;
        if (s === "NeedsLogin") ipn.login();
        if (s === "Running") recoverConnections();
        if (s === "Running" && discoveryPending) {
          if ($("#dialog")?.dataset.tailscaleLogin === "true") showPeers();
          else renderDiscoveredPeers();
          discoveryPending = false;
        }
      },
      notifyNetMap: (raw) => {
        peers = JSON.parse(raw).peers || [];
        renderDiscoveredPeers();
      },
      notifyBrowseToURL: (url) => {
        try {
          const href = safeAuthURL(url);
          dialog(
            "Sign in to Tailscale",
            `<p>Authorize this browser node in your tailnet. ${staticMode ? "Your identity is saved in this browser’s encrypted vault." : "Your identity is saved in the encrypted server vault."}</p><a class="primary auth-link" href="${esc(href)}" target="_blank" rel="noopener noreferrer">Continue to Tailscale ↗</a><p class="fine">This window updates automatically after sign-in.</p>`,
          );
          $("#dialog").dataset.tailscaleLogin = "true";
        } catch (e) {
          fatal(e.message);
        }
      },
      notifyPanicRecover: fatal,
    });
  })();
  try {
    await starting;
  } catch (e) {
    clearInterval(heartbeat);
    $("#tail-status").textContent = "Connection failed";
    throw e;
  } finally {
    starting = null;
  }
}
function showPeers() {
  discoveryPending = true;
  dialog(
    "Discover devices",
    `<p>Choose a device to connect.</p><input id="discovery-filter" placeholder="Find a device…" aria-label="Find a discovered device"><p id="discovery-status" class="fine" role="status"></p><div id="discovery-results"></div>`,
  );
  $("#dialog").dataset.discovery = "true";
  $("#discovery-filter").oninput = renderDiscoveredPeers;
  renderDiscoveredPeers();
}
function renderDiscoveredPeers() {
  if (!$("#dialog")?.open || $("#dialog").dataset.discovery !== "true") return;
  const query = $("#discovery-filter").value.trim().toLowerCase();
  $("#discovery-status").textContent =
    netState === "Running"
      ? `${peers.length} devices available`
      : "Connecting to Tailscale… Complete sign-in to discover devices.";
  const matches = peers
    .map((p, index) => ({ p, index }))
    .filter(({ p }) =>
      `${p.name} ${(p.addresses || []).join(" ")}`
        .toLowerCase()
        .includes(query),
    );
  $("#discovery-results").innerHTML =
    matches
      .map(({ p, index }) => {
        const saved = data.servers.find((s) => findPeer([p], s.host));
        return `<button class="session-result discovered-device" data-peer="${index}"><strong>${esc(p.name.replace(/\.$/, ""))}</strong><span>${p.online ? "Online" : "Offline / unknown"} · ${p.tailscaleSSHEnabled ? "Tailscale SSH" : "Standard SSH"}</span><small>${saved ? "Edit saved connection" : "Set up connection"} →</small></button>`;
      })
      .join("") ||
    `<p class="fine">${query ? "No devices match your search." : "No devices available yet. This list updates automatically when Tailscale supplies devices."}</p>`;
  $$("[data-peer]").forEach(
    (b) =>
      (b.onclick = () => {
        const p = peers[Number(b.dataset.peer)];
        const saved = data.servers.find((s) => findPeer([p], s.host));
        serverDialog(
          saved || {
            name: p.name.split(".")[0],
            host: p.name.replace(/\.$/, ""),
            mode: discoveryMode(p),
            tailnet: true,
          },
        );
      }),
  );
}
async function lock() {
  if (locking) return;
  locking = true;
  credentialCache.clear();
  reconnects.clear();
  imageUploads?.cancel();
  voiceDictation?.cancel();
  tabs.forEach((t) => t.close?.());
  try {
    await flushWorkspace();
  } catch (e) {
    notice("Could not save the latest layout: " + e.message);
  }
  clearInterval(heartbeat);
  await persistQueue;
  await api("/lock", "POST", {});
  location.reload();
}
setupPaneShortcuts({
  enabled: () => !!$("#workspace"),
  navigate: (direction) => paneGroups?.navigate(direction),
});
window.addEventListener("keydown", (e) => {
  if (!$("#workspace")) return;
  if (
    (e.metaKey || e.ctrlKey) &&
    e.shiftKey &&
    e.key.toLowerCase() === "p" &&
    !document.querySelector("dialog[open]")
  ) {
    e.preventDefault();
    openCommands();
    return;
  }
  if (
    !document.querySelector("dialog[open]") &&
    !e.target.matches("input,textarea,[contenteditable=true]")
  ) {
    const delta = fontShortcut(e);
    if (delta !== null) {
      e.preventDefault();
      setFont(delta);
      return;
    }
  }
  if (document.querySelector("dialog[open]")) return;
  if (!(e.metaKey || e.ctrlKey) || !e.shiftKey || !$("#workspace")) return;
  const key = e.key.toUpperCase();
  if (key === "K") {
    e.preventDefault();
    selectServer(selected, true);
  }
  if (key === "L") {
    e.preventDefault();
    guard(lock)();
  }
  if (key === "F") {
    e.preventDefault();
    $("#search-toggle").click();
  }
});
window.addEventListener("beforeunload", () => {
  void flushWorkspace();
});
window.addEventListener("pagehide", () => {
  credentialCache.clear();
  voiceDictation?.cancel();
  locking = true;
  reconnects.clear();
  tabs.forEach((t) => t.close?.());
});
setInterval(scheduleWorkspaceSave, 1000);
document.addEventListener("visibilitychange", () => {
  if (
    document.visibilityState === "visible" &&
    ipn &&
    Date.now() > leaseDeadline
  )
    location.reload();
});

function discoverTmux(server) {
  if (netState !== "Running")
    throw new Error("Connect Tailscale before checking remote sessions.");
  return new Promise((resolve, reject) => {
    let ssh,
      read,
      output = "",
      done = false,
      connected = false,
      sent = false;
    const command = `stty -echo; printf '\\036TTBEGIN\\037'; ${tmuxListCommand(server.tmuxPath)} 2>&1 || printf '\\nTTERROR\\n'; printf '\\036TTEND\\037'; exit\r`;
    const sendCommand = () => {
      if (!read || !connected || sent) return;
      sent = true;
      queueMicrotask(() => read(command));
    };
    const finish = (error, result) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      ssh?.close();
      error ? reject(error) : resolve(result);
    };
    const timer = setTimeout(
      () => finish(new Error("Remote tmux discovery timed out.")),
      25000,
    );
    try {
      ssh = ipn.ssh(server.host, server.username, {
        rows: 24,
        cols: 200,
        timeoutSeconds: 20,
        setReadFn: (fn) => {
          read = fn;
          sendCommand();
        },
        onConnectionProgress: () => {},
        writeErrorFn: (err) => finish(new Error(err)),
        onConnected: () => {
          connected = true;
          sendCommand();
        },
        writeFn: (chunk) => {
          output += chunk;
          if (output.length > 65536)
            return finish(new Error("Remote response too large."));
          const start = output.indexOf("\x1eTTBEGIN\x1f"),
            end = output.indexOf("\x1eTTEND\x1f");
          if (start >= 0 && end > start) {
            const body = output.slice(start + 9, end).trim();
            if (body.split(/[\r\n]+/).includes("TTERROR")) {
              finish(
                new Error(
                  body.replace(/(?:^|[\r\n])TTERROR(?:[\r\n]|$)/g, "").trim() ||
                    "Remote tmux discovery failed.",
                ),
              );
              return;
            }
            finish(null, {
              sessions: body
                .split(/[\r\n]+/)
                .filter(Boolean)
                .map((line) => {
                  const [name, windows, attached] = line.split("|");
                  return {
                    name,
                    windows: Number(windows) || 0,
                    attached: Number(attached) || 0,
                  };
                }),
            });
          }
        },
        onDone: () => {
          if (!done)
            finish(new Error("Remote shell ended before discovery completed."));
        },
      });
    } catch (e) {
      finish(e);
    }
  });
}

function backupDialog() {
  dialog(
    "Backup & restore",
    `<p>Save an encrypted copy of your servers, keys and bookmarks. You’ll need the vault passphrase to restore it.</p><button id="export-backup" class="primary">Download encrypted backup</button><details class="dialog-details"><summary>Restore a backup</summary><p class="fine">Restoring replaces the profiles, keys and bookmarks in this browser. Close terminal connections first. Your existing Tailscale identity is preserved.</p><form id="restore-backup"><label>Encrypted backup<input name="file" type="file" accept=".json" required></label><label>Backup passphrase<input name="password" type="password" minlength="14" required autocomplete="off"></label><button class="primary">Review & restore</button></form></details>`,
  );
  const lastBackup = document.createElement("p");
  lastBackup.className = "fine";
  lastBackup.textContent = data.backup?.exported
    ? "Last backup download: " + new Date(data.backup.exported).toLocaleString()
    : "No backup has been downloaded from this browser.";
  $("#export-backup").before(lastBackup);
  $("#export-backup").onclick = guard(async () => {
    const started = new Date().toISOString();
    const backup = await localVault.exportBackup();
    const url = URL.createObjectURL(
      new Blob([JSON.stringify(backup)], { type: "application/json" }),
    );
    const a = document.createElement("a");
    a.href = url;
    a.download =
      "tailterm-backup-" + new Date().toISOString().slice(0, 10) + ".json";
    a.click();
    await localVault.markBackupExported(started);
    data = await api("/data");
    renderBackupStatus();
    lastBackup.textContent =
      "Last backup download: " +
      new Date(data.backup.exported).toLocaleString();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  });
  $("#restore-backup").onsubmit = guard(async (e) => {
    e.preventDefault();
    if (tabs.length)
      throw new Error("Close terminal tabs before restoring a backup.");
    const file = e.target.elements.file.files[0];
    if (file.size > 16 * 1024 * 1024)
      throw new Error("Backup file is too large.");
    if (
      !(await confirmDialog({
        title: "Restore this backup?",
        message:
          "Replace this browser’s saved servers, keys and bookmarks? Your Tailscale identity stays on this browser.",
        action: "Replace & restore",
        destructive: true,
      }))
    )
      return;
    data = await localVault.importBackup(
      JSON.parse(await file.text()),
      e.target.elements.password.value,
    );
    remoteSnapshots.clear();
    closeDialog();
    selectServer(data.servers[0]?.id, true);
    notice("Backup restored.");
  });
}
function refreshBackupStatus() {
  if (!staticMode || locking) return;
  data.backup = localVault.localData().backup;
  renderBackupStatus();
}
function renderBackupStatus() {
  if (!staticMode || !$("#backup-vault")) return;
  $("#backup-vault").title = data.backup?.exported
    ? `Last backup: ${new Date(data.backup.exported).toLocaleString()}`
    : "Backup & restore";
}
function recoverConnections() {
  if (
    !staticMode ||
    locking ||
    restoring ||
    !navigator.onLine ||
    netState !== "Running"
  )
    return;
  for (const t of tabs)
    if (t.networkInterrupted && t.tmux && t.target && !t.disposed)
      reconnects.schedule(t, t.retryCount || 0);
}
window.addEventListener("offline", () => {
  for (const t of tabs)
    if (t.status === "Connected" && t.tmux && t.target) {
      t.networkInterrupted = true;
      t.close?.();
    }
});
window.addEventListener("online", recoverConnections);
document.addEventListener("visibilitychange", () => {
  // Returning to this page is not evidence that healthy SSH sessions are stale.
  // Retry connections already known to be interrupted; keep live sessions intact.
  if (document.visibilityState === "visible" && !inactivity.check())
    recoverConnections();
});
// Timers can be suspended; check on resume before recovery or fresh input.
for (const event of ["keydown", "pointerdown", "pointermove", "wheel"])
  window.addEventListener(
    event,
    (e) => {
      if (inactivity.activity()) {
        e.preventDefault();
        e.stopImmediatePropagation();
      }
    },
    { capture: true, passive: false },
  );
setInterval(() => {
  inactivity.check();
  credentialCache.prune();
}, 15000);
window.addEventListener("focus", () => inactivity.check());
window.addEventListener("pageshow", () => inactivity.check());

function openCommands() {
  const t = currentTab();
  const commands = visibleTabs().map((tab) => ({
    label: `Switch: ${tab.session || "SSH shell"} · ${tab.server.name} #${tab.number}`,
    run: () => activate(tab.id),
  }));
  commands.push({
    label: "Show all servers",
    run: () => $("#all-servers").click(),
  });
  commands.push({ label: "New session", run: () => $("#new-tab").click() });
  if (t) {
    commands.push({
      label: "Voice dictation · Option + Space",
      run: () => voiceDictation.open(),
    });
    commands.push(
      {
        label: "Reconnect current session",
        run: () => $("#reconnect").click(),
      },
      {
        label: "Download terminal output",
        run: () => $("#download-scrollback").click(),
      },
      {
        label: "Connection diagnostics",
        run: () => $("#connection-diagnostics").click(),
      },
    );
    if (staticMode && t.tmux)
      commands.push({
        label: "Rename current session",
        run: () => renameSession(t.server, t.session, t.target),
      });
    if (t.history && t.tmux)
      commands.push({
        label: t.history.isOpen()
          ? "Turn off power scrolling"
          : "Turn on power scrolling",
        run: () =>
          t.history.isOpen() ? t.history.close() : void t.history.open(),
      });
    if (imageUploads)
      commands.push({
        label: "Upload images",
        run: () => imageUploads.choose(),
      });
    for (const other of visibleTabs().filter(
      (x) => x.id !== t.id && !paneGroups.members(t.id).includes(x.id),
    ))
      commands.push({
        label: `Split with: ${other.session || "SSH shell"} · ${other.server.name} #${other.number}`,
        run: () => paneGroups.merge(other.id, t.id, false),
      });
    if (paneGroups.members(t.id).length > 1)
      for (const direction of ["left", "right", "up", "down"])
        commands.push({
          label: `Focus pane ${direction} · Option + Shift / Ctrl + Alt + Arrow`,
          run: () => paneGroups.navigate(direction),
        });
    if (paneGroups.members(t.id).length > 1)
      commands.push({
        label: "Separate current pane into a tab",
        run: () => paneGroups.detach(t.id),
      });
  }
  commands.push(
    { label: "Appearance", run: () => $("#appearance").click() },
    { label: "Lock workspace", run: () => $("#lock").click() },
  );
  if (staticMode)
    commands.push(
      { label: "Backup and restore", run: backupDialog },
      { label: "Forget this device", run: () => $("#forget-device").click() },
    );
  showCommandPalette({ dialog, close: closeDialog, commands });
}

function browserAttentionChanged() {
  const t = currentTab();
  if (!t) return;
  t.activitySnapshot = screenLines(t.term, t.tmux);
  if (document.visibilityState === "visible" && document.hasFocus()) {
    t.activity = "";
    renderTabs();
  }
}
window.addEventListener("blur", browserAttentionChanged);
window.addEventListener("focus", browserAttentionChanged);
document.addEventListener("visibilitychange", browserAttentionChanged);
