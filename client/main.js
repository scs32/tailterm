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
import "@fontsource/fira-code/latin-400.css";
import { setupTabStrip } from "./tab-strip.js";
import { setupPaneGroups } from "./pane-groups.js";
import { setupTerminalView } from "./terminal-view.js";
import { sidebarIcon } from "./sidebar-icons.js";
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
const icon = '<span class="brand-icon">▦</span>';
$("#app").innerHTML =
  `<div id="notice" role="status" hidden></div><div id="lockscreen"><div class="login-brand">${icon} tailserve <span class="version">PREVIEW 01</span></div><section class="unlock-card"><span class="eyebrow">YOUR PRIVATE TERMINAL WORKSPACE</span><h1>Closer to<br>your servers.</h1><p>A real terminal. Your tailnet. Persistent sessions.<br>Everything you need, right here.</p><form id="unlock"><label for="password">Vault passphrase</label><input id="password" type="password" minlength="14" required autocomplete="current-password" placeholder="At least 14 characters"><button class="primary" id="unlock-button">Unlock workspace <span>↗</span></button></form><p class="fine" id="vault-hint">Checking encrypted vault…</p></section><div class="login-footer"><span>◈ Encrypted at rest</span><span>Powered by Tailscale + WebAssembly</span></div></div>`;
const status = await api("/status");
$("#vault-hint").textContent = status.initialized
  ? staticMode
    ? "Your passphrase unlocks the encrypted vault on this browser. It is never stored."
    : "Your passphrase decrypts the server vault. It is never stored."
  : "Create a passphrase to initialize your encrypted vault. Keep it safe: there is no password recovery.";
$("#unlock-button").firstChild.textContent = status.initialized
  ? "Unlock workspace "
  : "Create encrypted vault ";
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
  $("#lockscreen")?.remove();
  $("#app").insertAdjacentHTML(
    "beforeend",
    `<div id="workspace"><aside><div class="brand">${icon}<strong>tailserve</strong><span class="version">01</span></div><div class="sidebar-section"><span>SERVERS</span></div><input id="filter" class="filter" placeholder="⌕  Find a server…" aria-label="Find a server"><nav id="server-list"></nav><button id="discover" class="sidebar-discover">⌕ Discover devices</button><div class="sidebar-bottom"><button id="keys">♧ <span>SSH key vault</span><span id="key-count">0</span></button><button id="lock">↪ <span>Lock workspace</span><kbd>⇧⌘L</kbd></button></div></aside><main><header><div class="header-right"><span class="status-dot" id="tail-dot"></span><span id="tail-status">Tailnet offline</span><button id="tailscale-login">Connect Tailscale ↗</button></div></header><section class="terminal-shell"><div class="terminal-tabs"><div class="tab-strip"><button id="tabs-left" class="tab-scroll" aria-label="Scroll tabs left" title="Scroll tabs left" hidden>‹</button><div id="tabs" role="tablist" aria-label="SSH connections"></div><button id="tabs-right" class="tab-scroll" aria-label="Scroll tabs right" title="Scroll tabs right" hidden>›</button></div><button id="new-tab" class="icon-button" title="Start or resume a session">+</button><div class="terminal-tools"><button id="edit-server" title="Edit selected server" aria-label="Edit selected server">Edit server</button><button id="search-toggle" title="Find in terminal">⌕</button><button id="font-down" title="Smaller text">A−</button><button id="font-up" title="Larger text">A+</button><button id="appearance" title="Appearance\nThemes, fonts, cursor and spacing" aria-label="Appearance">◐</button><button id="fullscreen" title="Fullscreen">⛶</button></div></div><div id="search-bar" hidden><input id="terminal-search" placeholder="Find in scrollback" aria-label="Find in terminal"><button id="find-next">Next ↓</button><button id="search-close">×</button></div><div id="terminal-body"><div id="empty-terminal"><div class="session-launcher"><span class="eyebrow">YOUR REMOTE WORKSPACE</span><h2>Pick up where you left off.</h2><p class="launcher-intro">Choose a server, then open a fresh workspace or return to a running session.</p><div id="launcher-server" class="server-grid" role="group" aria-label="Session server"></div><div class="launch-section"><div class="launch-section-title"><span class="step-dot">＋</span><div><h3>Start fresh</h3><p>A persistent tmux workspace on <strong id="launch-target"></strong></p></div></div><div class="launch-new"><input id="launcher-name" placeholder="Optional name · leave blank for an automatic ID" aria-label="New tmux session name" maxlength="64"><button id="start-session" class="primary">＋ Start session</button></div></div><div class="resume-heading"><strong>Pick up a session</strong><button id="launcher-refresh" title="Refresh sessions\nQuery this server now; no sessions are changed.">↻ Refresh</button></div><p id="launcher-note"></p><div id="launcher-sessions"></div><div class="launcher-secondary"><button id="launcher-shell">Open plain SSH shell</button><button id="launcher-discover">⌕ Discover devices</button></div></div></div></div><div class="terminal-footer"><span id="terminal-status">○ No active connection</span><div><button id="copy">Copy</button><button id="paste">Paste</button><button id="clear">Clear</button><button id="reconnect">Reconnect</button><span id="dimensions">— × —</span></div></div></section></main></div><dialog id="dialog"></dialog>`,
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
    activate,
    close: closeTab,
    preferences: () => appearance,
    label: tabName,
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
  $("#edit-server").onclick = () => {
    const s = currentServer();
    if (s) serverDialog(s);
  };
  $("#new-tab").title = "Start or resume a session";
  $("#new-tab").onclick = () => selectServer(selected, true);
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
  $("#reconnect").onclick = guard(() => {
    const t = currentTab();
    if (!t) return;
    return connect(
      data.servers.find((s) => s.id === t.server.id) || t.server,
      t.tmux,
      t.session,
      { replace: t, resumeOnly: t.tmux },
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
}
function renderSidebar() {
  const q = $("#filter").value.toLowerCase();
  const servers = data.servers.filter((s) =>
    (s.name + s.host + s.group).toLowerCase().includes(q),
  );
  $("#server-list").innerHTML =
    servers
      .map(
        (s) =>
          `<button class="server-item ${s.id === selected ? "selected" : ""}" data-server="${esc(s.id)}"><span class="node-icon">${sidebarIcon("server")}</span><div><strong>${esc(s.name)}</strong><small>${esc(s.group)} · ${s.mode === "ssh" ? "SSH" : "Auto"}</small></div><span class="node-dot ${tabs.some((t) => t.server.id === s.id && t.status === "Connected") ? "online" : ""}"></span></button>`,
      )
      .join("") ||
    '<p class="sidebar-empty">No servers yet.<br>Your workspace starts here.</p>';
  $$("[data-server]").forEach(
    (b) =>
      (b.onclick = () => {
        selectServer(b.dataset.server);
      }),
  );
}
function renderTabs() {
  if (!$("#tabs")) return;
  paneGroups?.sync();
  const scrollPosition = $("#tabs").scrollLeft;
  $("#tabs").innerHTML = (
    paneGroups?.entries() || tabs.map((tab) => ({ tab, ids: [tab.id] }))
  )
    .map(({ tab: remembered, ids }) => {
      const t = ids.includes(active) ? currentTab() : remembered;
      const grouped = ids.length > 1;
      const names = ids.map((id) => {
        const member = tabs.find((t) => t.id === id);
        return `${tabName(member)} #${member.number}`;
      });
      const title = grouped
        ? `${ids.length} panes: ${names.join(", ")}\nDrag onto another tab to merge groups. × closes the focused pane.`
        : `${tabName(t)} #${t.number}\n${t.server.username}@${t.server.host}:${t.server.port}\n${t.status}${t.tmux ? " · tmux launch: " + t.session : ""}\nDrag onto another tab to group.`;
      return `<div class="tab ${ids.includes(active) ? "active" : ""} ${grouped ? "group-tab" : ""}" draggable="false"><button data-tab="${t.id}" role="tab" aria-selected="${ids.includes(active)}" title="${esc(title)}"><span class="node-dot ${t.status === "Connected" ? "online" : ""}"></span><span class="tab-copy"><span class="tab-name">${grouped ? `<span class="pane-count">▦ ${ids.length}</span> ${esc(names.join(" + "))}` : `${esc(tabName(t))} #${t.number}`}</span><span class="tab-tmux">${grouped ? "Focused: " + esc(tabName(t)) + " · " : ""}${esc(t.status)}${t.tmux ? " · launch: " + esc(t.session) + (t.tmuxVerified ? "" : " (unverified)") : ""}</span></span></button><button data-close="${t.id}" aria-label="Close ${esc(t.server.name)} ${grouped ? "focused pane" : "terminal"}">×</button></div>`;
    })
    .join("");
  $$("[data-tab]").forEach((b) => (b.onclick = () => activate(b.dataset.tab)));
  $$("[data-close]").forEach(
    (b) => (b.onclick = () => closeTab(b.dataset.close)),
  );
  $("#tabs").scrollLeft = scrollPosition;
  tabStrip?.update();
  $("#empty-terminal").hidden = !!currentTab();
  paneGroups?.render();
  const t = currentTab();
  $("#reconnect").disabled =
    !t || !["Connected", "Disconnected", "Error"].includes(t.status);
  $("#dimensions").textContent = t
    ? `${t.term.cols} × ${t.term.rows}`
    : "— × —";
  $("#terminal-status").textContent = t
    ? `${t.status === "Connected" ? "●" : "○"} ${t.status} · ${t.server.username}@${t.server.host} · ${t.transport === "browser" ? "SSH over Tailscale" : t.transport === "wasm" ? "Tailscale WASM" : "SSH"}${t.tmux ? " · tmux launch: " + t.session : ""}`
    : "○ No active connection";
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
function tabName(t) {
  return data.servers.find((s) => s.id === t.server.id)?.name || t.server.name;
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
    `<p>Drag a tab onto another tab to tile their terminals. Drag a pane’s header to the tab bar to separate it, or use ↗ in its header.</p>${
      ids.length > 1
        ? `<h3>This group · ${ids.length} panes</h3><div class="group-choices">${ids
            .map((id) => {
              const p = tabs.find((t) => t.id === id);
              return `<button data-separate="${id}">↗ Separate ${esc(tabName(p))} #${p.number}</button>`;
            })
            .join("")}</div>`
        : ""
    }<h3>Group with</h3><div class="group-choices">${others.map((g) => `<button data-merge="${g.tab.id}">▦ ${esc(tabName(g.tab))} #${g.tab.number}${g.ids.length > 1 ? " · " + g.ids.length + " panes" : ""}</button>`).join("") || '<p class="fine">Open another terminal with + first.</p>'}</div><p class="fine">Drag dividers to resize. Focus a divider and use arrow keys; hold Shift for larger steps. Double-click or press Enter to reset that split. Closing a pane disconnects only that SSH connection; remote tmux keeps running.</p>`,
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
  active = id;
  const t = currentTab();
  if (t) {
    selected = t.server.id;
    $("#launcher-name").value = "";
  }
  render();
  if (t)
    requestAnimationFrame(() => {
      if (t.disposed) return;
      tabStrip?.reveal();
      tabs.filter((t) => !t.el.hidden).forEach((t) => t.fit.fit());
      t.term.focus();
    });
}
function disposeTab(t, replacing = false) {
  t.disposed = true;
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
    const neighbor =
      tabs.find((t) => t.id === sibling) ||
      tabs[Math.min(index, tabs.length - 1)];
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
function renderLauncher() {
  if (!$("#launcher-server")) return;
  const selector = $("#launcher-server");
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
          `<button class="session-result" data-resume="${i}"><strong>${esc(s.name)}</strong><span>${s.windows} windows · ${s.attached} attached clients</span><span class="resume-action">Resume →</span></button>`,
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
          { resumeOnly: true },
        ),
      )),
  );
}
async function verifyTmux(t) {
  for (let attempt = 0; attempt < 3; attempt++) {
    await new Promise((resolve) => setTimeout(resolve, 400));
    if (t.disposed || t.status !== "Connected") return;
    try {
      const result = await refreshRemote(t.server, t);
      if (!result.sessions.some((s) => s.name === t.session)) continue;
      if (t.disposed) return;
      t.tmuxVerified = true;
      const updated = await api("/sessions", "POST", {
        serverId: t.server.id,
        name: t.session,
      });
      data.sessions = updated.sessions;
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
    `<p class="fine">Live previews. Changes apply to all terminals and are remembered on this browser.</p>
    <h3>Color palette</h3><div class="theme-grid">${Object.entries(themes)
      .map(
        ([id, t]) =>
          `<button class="theme-choice ${appearance.theme === id ? "chosen" : ""}" data-theme-choice="${id}" aria-pressed="${appearance.theme === id}" style="--sample-bg:${t.background};--sample-fg:${t.foreground};--sample-accent:${t.accent}"><span class="theme-sample"><span>❯</span> ssh workspace<span class="palette-dots">● ● ●</span></span><strong>${esc(t.name)}</strong></button>`,
      )
      .join("")}</div>
    <h3>Typeface</h3><div class="font-grid">${Object.entries(fonts)
      .map(
        ([id, f]) =>
          `<button data-font-choice="${id}" class="font-choice ${appearance.font === id ? "chosen" : ""}" aria-pressed="${appearance.font === id}"><strong>${esc(f.name)}</strong><span style='font-family:${esc(f.family)}'>0O 1il {} =&gt; ~/work</span></button>`,
      )
      .join("")}</div>
    <div class="appearance-controls"><div><label>Font size</label><div class="size-stepper"><button id="appearance-smaller" aria-label="Decrease font size">−</button><output id="font-size-value">${appearance.fontSize} px</output><button id="appearance-larger" aria-label="Increase font size">+</button><button id="appearance-reset">Reset</button></div></div><label>Line height<input id="line-height" type="range" min="1" max="1.6" step="0.05" value="${appearance.lineHeight}"></label><label>Padding<input id="terminal-padding" type="range" min="0" max="32" step="2" value="${appearance.padding}"></label></div>
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
    <p class="fine">Hold Shift while dragging to select text when tmux handles the mouse. Plain Ctrl+C still interrupts a command. Remote clipboard read requests are never answered.</p>`,
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
    term.open(el);
    const t = {
      id: options.replace?.id || crypto.randomUUID(),
      number: options.replace?.number || ++connectionNumber,
      server,
      transport,
      tmux,
      session,
      resumeOnly: !!options.resumeOnly,
      term,
      fit,
      search,
      el,
      status: "Connecting",
      send: null,
    };
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
    term.onResize(({ rows, cols }) => {
      t.resize?.(rows, cols);
      if (active === t.id) $("#dimensions").textContent = `${cols} × ${rows}`;
    });
    setupTerminalInput(t, {
      preferences: () => appearance,
      isActive: (t) => t.id === active,
      notice,
      setFont,
      changed: renderClipboard,
    });
    const update = (status) => {
      if (t.disposed) return;
      t.status = status;
      renderTabs();
      renderSidebar();
      renderClipboard();
    };
    const ready = () => {
      if (t.disposed) return;
      update("Connected");
      if (tmux) void verifyTmux(t);
      else backgroundRemote(t.server.id);
    };
    const error = (e) => {
      if (t.disposed) return;
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
      const connection = browserTransport.browserSSH(ipn, server, peers, {
        rows: term.rows,
        cols: term.cols,
        command: tmux
          ? remoteTmuxCommand(session, server.tmuxPath, options.resumeOnly)
          : "",
        onData: (d) => {
          if (!t.disposed) term.write(d);
        },
        onInput: (fn) => {
          t.send = fn;
        },
        onProgress: (message) => {
          if (!t.disposed) {
            update(
              message === "Verify host" || message === "Signing in"
                ? message
                : "Connecting",
            );
          }
        },
        onConnected: ready,
        onWarning: notice,
      });
      t.close = () => connection.close();
      t.resize = (r, c) => connection.resize(r, c);
      t.discover = () => browserTransport.browserTmux(ipn, server, peers);
      void connection.done
        .then((result) => {
          t.send = null;
          if (result?.exitCode)
            error("Remote process exited with status " + result.exitCode + ".");
          else if (t.status !== "Error") update("Disconnected");
        })
        .catch((e) => {
          t.send = null;
          error(e.message);
        });
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
  const d = $("#dialog");
  if (d.open) d.close();
  (document.fullscreenElement || $("#app")).append(d);
  delete d.dataset.discovery;
  delete d.dataset.tailscaleLogin;
  d.innerHTML = `<div class="dialog-head"><h2>${esc(title)}</h2><button id="dialog-close" aria-label="Close dialog">×</button></div>${body}`;
  $("#dialog-close").onclick = closeDialog;
  if (!d.open) d.showModal();
}
function closeDialog() {
  $("#dialog").close();
  $("#dialog").replaceChildren();
}
function serverDialog(s = {}) {
  dialog(
    s.id ? "Edit server" : "Set up SSH connection",
    `<form id="server-form"><div class="form-grid"><label>Display name<input name="name" value="${esc(s.name)}" placeholder="Production box" required maxlength="80"></label><label>Group<input name="group" value="${esc(s.group || "Personal")}" maxlength="40"></label></div><label>Connection<select name="mode"><option value="auto" ${!s.mode || s.mode === "auto" ? "selected" : ""}>Automatic authentication</option><option value="wasm" ${s.mode === "wasm" ? "selected" : ""}>Prefer Tailscale SSH</option><option value="ssh" ${s.mode === "ssh" ? "selected" : ""}>Standard SSH · key or password</option></select></label><div class="form-grid"><label>Hostname or IP<input name="host" value="${esc(s.host)}" placeholder="server.tailnet.ts.net" required></label><label>Port<input name="port" type="number" min="1" max="65535" value="${s.port || 22}" required></label></div><label>SSH username<input name="username" value="${esc(s.username || "ubuntu")}" required></label><div id="ssh-fields"><label>SSH key<select name="keyId"><option value="">Ask when needed</option>${data.keys.map((k) => `<option value="${esc(k.id)}" ${s.keyId === k.id ? "selected" : ""}>${esc(k.name)}</option>`).join("")}</select></label><label>tmux executable (optional)<input name="tmuxPath" value="${esc(s.tmuxPath)}" placeholder="Auto-detect, or /absolute/path/to/tmux"></label><label>SSH host fingerprint (optional)<input name="fingerprint" value="${esc(s.fingerprint)}" placeholder="SHA256:…"></label><p class="fine">Get this through a trusted console on the remote host:<br><code>ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code><br>${staticMode ? "This hostname must be reachable through Tailscale, directly or via an approved subnet route." : "The application server must be able to reach this hostname."}</p></div><p class="fine" id="wasm-hint">${staticMode ? "All connections travel through Tailscale. Automatic uses Tailscale SSH where enabled, otherwise standard SSH authentication." : "Automatic login uses Tailscale SSH when available, then standard SSH if needed."} You will be prompted for a key or password and first-time host verification.</p>${s.hasPassword ? '<label class="remember-password"><input type="checkbox" name="clearPassword"> Forget saved SSH password</label>' : ""}<div class="dialog-actions">${s.id ? '<button type="button" id="delete-server" class="danger">Delete server</button>' : ""}<button class="primary">Save server ↗</button></div></form>`,
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
      if (!confirm(`Delete saved server ${s.name}?`)) return;
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
    `<p class="fine">${staticMode ? "Private keys stay on this browser, encrypted with your vault passphrase." : "Private keys stay on the server, encrypted with your vault passphrase."}</p><div class="key-list">${data.keys.map((k) => `<div><strong>${esc(k.name)}</strong><button data-delete-key="${esc(k.id)}" title="Delete key">×</button><code>${esc(k.fingerprint)}</code></div>`).join("") || "<p>No keys stored yet.</p>"}</div><form id="key-form"><label>Key name<input name="name" required maxlength="80" placeholder="Personal Ed25519"></label><label>Import private key file<input id="key-file" type="file"></label><label>Private key<textarea name="privateKey" required rows="5" spellcheck="false" autocomplete="off" placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"></textarea></label><label>Key passphrase (if encrypted)<input name="passphrase" type="password" autocomplete="off"></label><div class="dialog-actions"><button class="primary">Encrypt & save key</button></div></form>`,
  );
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
        if (!confirm("Delete this stored SSH key?")) return;
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
      hostname: "tailserve-browser",
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
        if (s === "NeedsLogin") ipn.login();
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
            `<p>Authorize this browser node in your tailnet. ${staticMode ? "Your identity is saved in this browser’s encrypted vault." : "Your identity is saved in the encrypted server vault."}</p><a class="primary auth-link" href="${esc(href)}" target="_blank" rel="noopener noreferrer">Continue to Tailscale ↗</a><p class="fine">Return here after signing in. Your tailnet status will update automatically.</p>`,
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
    `<p>Choose a device from your tailnet to configure its SSH connection.</p><input id="discovery-filter" placeholder="Find a device…" aria-label="Find a discovered device"><p id="discovery-status" class="fine" role="status"></p><div id="discovery-results"></div>`,
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
  tabs.forEach((t) => t.close?.());
  clearInterval(heartbeat);
  await persistQueue;
  await api("/lock", "POST", {});
  location.reload();
}
window.addEventListener("keydown", (e) => {
  if (!$("#workspace")) return;
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
window.addEventListener("beforeunload", () => tabs.forEach((t) => t.close?.()));
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
    `<p>Export an encrypted copy of your servers, SSH keys and session bookmarks. Keep your vault passphrase to unlock the backup. Tailscale device identity stays on this browser.</p><button id="export-backup" class="primary">Download encrypted backup</button><hr><h3>Restore a backup</h3><p class="fine">Restoring replaces the profiles, keys and bookmarks in this browser. Close terminal connections first. Your existing Tailscale identity is preserved.</p><form id="restore-backup"><label>Encrypted backup<input name="file" type="file" accept=".json" required></label><label>Backup passphrase<input name="password" type="password" minlength="14" required autocomplete="off"></label><button class="primary">Review & restore</button></form>`,
  );
  $("#export-backup").onclick = guard(async () => {
    const backup = await localVault.exportBackup();
    const url = URL.createObjectURL(
      new Blob([JSON.stringify(backup)], { type: "application/json" }),
    );
    const a = document.createElement("a");
    a.href = url;
    a.download =
      "tailserve-backup-" + new Date().toISOString().slice(0, 10) + ".json";
    a.click();
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
      !confirm(
        "Replace this browser’s saved servers, keys and bookmarks with this backup?",
      )
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
// The browser cannot keep SSH alive indefinitely while suspended. Lock after 15 minutes without local interaction.
let lastInteraction = Date.now();
for (const event of ["keydown", "pointerdown", "pointermove"])
  document.addEventListener(
    event,
    () => {
      lastInteraction = Date.now();
    },
    { passive: true },
  );
setInterval(() => {
  if (
    staticMode &&
    $("#workspace") &&
    Date.now() - lastInteraction > 15 * 60 * 1000
  )
    guard(lock)();
}, 15000);
