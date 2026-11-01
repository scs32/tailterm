// TailOS Settings (wi_dc04ac6a9ccf7402): opened from the left nav, rendered
// into #mode-view as titled sections from one registry. The Discord section
// edits the bridge's non-secret settings the hub keeps; both tokens stay
// token files on the bridge host and are shown only as configured or not.
import { sidebarIcon } from "./sidebar-icons.js";

const esc = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );

// The hub client's generic request keeps client/hub-client.js (bundled into
// the generated team plan) unchanged.
const readBridgeSettings = (hub) => hub.request("/v1/bridge/settings");
const saveBridgeSettings = (hub, body) =>
  hub.request("/v1/bridge/settings", { method: "PUT", body });

// The five fields, in display order. `list` fields are comma separated.
export const DISCORD_FIELDS = [
  {
    key: "guildId",
    label: "Discord server ID",
    placeholder: "Server (guild) ID, digits only",
  },
  {
    key: "ownerIds",
    label: "Owner user IDs",
    placeholder: "Discord user IDs, comma separated",
    list: true,
  },
  {
    key: "helperTask",
    label: "Owner helper project",
    placeholder: "Project ID (tsk_…); empty turns the helper off",
  },
  {
    key: "helperChannelId",
    label: "Helper channel ID",
    placeholder: "Optional channel ID, digits only",
  },
  {
    key: "tailosUrl",
    label: "TailOS link",
    placeholder: "https://… used in Discord cards",
  },
];

// Fixed refusal codes from the bridge, in plain English.
export const REFUSAL_TEXT = {
  invalid: "the bridge could not use these values",
  "startup-failed":
    "the bridge failed to start on it (the bridge log has the error)",
  "stopped-during-start": "the bridge stopped before it reached Discord",
  "guild-change":
    "it names another Discord server while the bridge has project channels in the current one",
};

const SECRET_TEXT = {
  configured: "Configured",
  "configured-on-bridge-host": "Configured on the bridge host",
  "not-confirmed": "Not confirmed",
};

const fieldValue = (settings, field) => {
  const value = settings?.[field.key];
  return field.list ? (value || []).join(", ") : value || "";
};

const formTime = (iso) => {
  const at = new Date(iso);
  return Number.isNaN(at.getTime())
    ? ""
    : at.toLocaleString([], {
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      });
};

// The source line: what the bridge reports it runs on.
export function bridgeSourceText(view) {
  const b = view?.bridge;
  if (!b) return "Bridge is using: not reported since the hub started.";
  const refusal =
    b.refused > 0
      ? `refused revision ${b.refused}: ${REFUSAL_TEXT[b.reason] || "refused"}`
      : "";
  if (b.source === "none")
    return b.reason === "guild-change" || !b.reason
      ? "Bridge is not connected: the guild changed; save the bound guild again or re-set up (move the bridge state dir aside)."
      : `Bridge is not connected: ${refusal}.`;
  const using =
    b.source === "hub"
      ? `hub revision ${b.applied}`
      : b.source === "last-good"
        ? `last good saved settings (revision ${b.applied})`
        : "deployment values";
  return `Bridge is using: ${using}${refusal ? ` — ${refusal}` : ""}.`;
}

function discordSection({ client, confirm }) {
  let root = null,
    view = null,
    status = "",
    statusKind = "",
    stale = false,
    saving = false;
  const savedRefused = () =>
    !!view?.settings &&
    view?.bridge?.refused > 0 &&
    view.bridge.refused === view.revision;
  // The guild the owner is moving from: the saved one, or with nothing
  // saved yet, the one the bridge reports.
  const baselineGuild = () =>
    view?.settings?.guildId || view?.bridge?.effective?.guildId || "";
  const formSettings = () => {
    const out = {};
    for (const field of DISCORD_FIELDS) {
      const raw = root.querySelector(`[name="${field.key}"]`).value.trim();
      out[field.key] = field.list
        ? raw
            .split(/[\s,]+/)
            .map((v) => v.trim())
            .filter(Boolean)
        : raw;
    }
    return out;
  };
  const initial = () =>
    view?.settings ||
    (view?.bridge && view.bridge.source !== "none"
      ? view.bridge.effective
      : null);
  const dirty = () => {
    const start = initial();
    return DISCORD_FIELDS.some(
      (field) =>
        root.querySelector(`[name="${field.key}"]`).value.trim() !==
        fieldValue(start, field),
    );
  };
  function syncSave() {
    const save = root?.querySelector("[data-settings-save]");
    if (!save) return;
    const retry = savedRefused() && !dirty();
    save.textContent = retry ? "Save again to retry" : "Save";
    save.disabled = saving || stale || !(retry || dirty() || !view?.settings);
  }
  function setStatus(text, kind = "") {
    status = text;
    statusKind = kind;
    const line = root?.querySelector("[data-settings-status]");
    if (line) {
      line.textContent = text;
      line.dataset.kind = kind;
    }
  }
  function render() {
    if (!root) return;
    const start = initial();
    const effective = view?.bridge?.source === "none" ? null : view?.bridge;
    const rows = DISCORD_FIELDS.map((field) => {
      const using = effective ? fieldValue(effective.effective, field) : "";
      return `<tr data-field="${field.key}"><th scope="row"><label for="settings-${field.key}">${esc(field.label)}</label></th><td><input id="settings-${field.key}" name="${field.key}" autocomplete="off" spellcheck="false" placeholder="${esc(field.placeholder)}" value="${esc(fieldValue(start, field))}"></td><td data-effective="${field.key}">${using ? esc(using) : '<span class="settings-unset">not set</span>'}</td></tr>`;
    }).join("");
    const secrets = view?.secrets || {};
    const saved = view?.settings
      ? `Saved revision ${view.revision}${view.updatedAt ? ` · ${esc(formTime(view.updatedAt))}` : ""}`
      : view?.bridge && view.bridge.source !== "none"
        ? "Not saved yet: the fields show the values the bridge reports"
        : "Not saved yet";
    root.innerHTML = `<p class="fine settings-source" data-bridge-source>${esc(bridgeSourceText(view))}${view?.bridge?.reportedAt ? ` <span class="settings-reported">Reported ${esc(formTime(view.bridge.reportedAt))}.</span>` : ""}</p>
<form class="settings-form" data-discord-form novalidate>
<table class="settings-fields"><thead><tr><th scope="col">Setting</th><th scope="col">Saved in TailOS</th><th scope="col">Bridge is using</th></tr></thead><tbody>${rows}</tbody></table>
<dl class="settings-secrets" data-settings-secrets><div><dt>Hub bridge token</dt><dd data-secret="bridgeToken">${esc(SECRET_TEXT[secrets.bridgeToken] || SECRET_TEXT["not-confirmed"])}</dd></div><div><dt>Discord bot token</dt><dd data-secret="discordToken">${esc(SECRET_TEXT[secrets.discordToken] || SECRET_TEXT["not-confirmed"])}</dd></div></dl>
<p class="fine">Both tokens stay in token files on the bridge host and are never entered here. Configured means the bridge has reported since the hub started; it does not prove Discord accepts the bot token. A saved change restarts the bridge within about 30 seconds.</p>
<div class="settings-actions"><button type="submit" class="primary" data-settings-save>Save</button><button type="button" data-settings-reload>Reload</button><span class="fine" data-settings-saved>${saved}</span></div>
<p class="fine settings-status" role="status" data-settings-status></p>
</form>`;
    setStatus(status, statusKind);
    const form = root.querySelector("form");
    form.addEventListener("input", syncSave);
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      void save();
    });
    root.querySelector("[data-settings-reload]").onclick = () => void load();
    syncSave();
  }
  async function load() {
    const hub = client();
    stale = false;
    try {
      view = await readBridgeSettings(hub);
      setStatus("");
      render();
    } catch (error) {
      view = null;
      root.innerHTML = `<p class="fine settings-status" role="status" data-settings-status data-kind="error">${
        error?.status === 404 || error?.status === 405
          ? "This hub has no bridge settings yet; update the hub to change them here."
          : esc(
              `Could not read the bridge settings: ${error?.message || error}`,
            )
      }</p><div class="settings-actions"><button type="button" data-settings-reload>Reload</button></div>`;
      root.querySelector("[data-settings-reload]").onclick = () => void load();
    }
  }
  async function save() {
    if (saving || stale) return;
    const settings = formSettings();
    const from = baselineGuild();
    if (from && settings.guildId !== from) {
      const ok = await confirm({
        title: "Move the bridge to another Discord server?",
        message:
          "While it has project channels in the current server it refuses the change and keeps running there; see Re-setup in the Discord help.",
        action: "Change server",
      });
      if (!ok) return;
    }
    saving = true;
    syncSave();
    setStatus("Saving…");
    try {
      view = await saveBridgeSettings(client(), {
        settings,
        expectedRevision: view?.revision || 0,
      });
      status = `Saved revision ${view.revision}. The bridge restarts to apply it within about 30 seconds.`;
      statusKind = "ok";
      saving = false;
      render();
    } catch (error) {
      saving = false;
      if (error?.status === 409) {
        stale = true;
        setStatus(
          "These settings changed elsewhere. Reload to see the saved values; nothing was saved.",
          "error",
        );
      } else {
        setStatus(
          `${error?.message || error} Nothing was saved.`.trim(),
          "error",
        );
      }
      syncSave();
    }
  }
  return {
    mount(el) {
      root = el;
    },
    load,
  };
}

// The Settings registry: one entry per titled section, in display order.
// Adding a section is one entry: {id, title, create(host) → {mount, load}}.
export const SECTIONS = [
  { id: "discord", title: "Discord", create: discordSection },
];

export function createSettingsView({ client, configure, confirm }) {
  let root = null,
    visible = false;
  return {
    mount(el) {
      root = el;
    },
    hide() {
      visible = false;
    },
    async show() {
      visible = true;
      if (!root) return;
      if (!client()) {
        root.innerHTML =
          '<div class="mode-empty"><span class="eyebrow">SETTINGS</span><h2>Connect a project hub.</h2><button data-settings-configure>Configure project hub</button></div>';
        root.querySelector("button").onclick = configure;
        return;
      }
      root.innerHTML = `<div class="settings-view"><div class="view-heading"><div><span class="eyebrow">TAILOS</span><h2>Settings</h2></div></div>${SECTIONS.map(
        (s) =>
          `<section class="settings-section" data-settings-section="${esc(s.id)}" aria-labelledby="settings-title-${esc(s.id)}"><h3 id="settings-title-${esc(s.id)}">${esc(s.title)}</h3><div data-settings-body></div></section>`,
      ).join("")}</div>`;
      const host = { client, confirm, configure };
      await Promise.all(
        SECTIONS.map((s) => {
          const section = s.create(host);
          section.mount(
            root.querySelector(
              `[data-settings-section="${s.id}"] [data-settings-body]`,
            ),
          );
          return visible ? section.load() : null;
        }),
      );
    },
  };
}

// The left-nav entry: placed in the sidebar's bottom group before Lock.
export function addSettingsNav(bottom, open) {
  const button = document.createElement("button");
  button.id = "settings-nav";
  button.type = "button";
  button.dataset.navMode = "settings";
  button.setAttribute("aria-pressed", "false");
  button.title = "Settings\nDiscord bridge and other TailOS settings";
  button.innerHTML = `${sidebarIcon("settings")}<span class="nav-label">Settings</span>`;
  button.onclick = open;
  bottom.insertBefore(button, bottom.querySelector("#lock"));
  return button;
}
