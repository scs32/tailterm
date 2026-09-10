import { normalizeHubURL, HubError } from "./hub-client.js";
import { normalizeUsername, profileKeys } from "./profile-crypto.js";
const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );

export function createProfileSync(host, vault) {
  let busy = false,
    stopped = false,
    timer,
    conflict = null,
    message = "Local profile",
    discovered = [],
    scanning = false;
  const checked = new Set(),
    attempted = new Set();
  const state = () => vault.localData().profile || {};
  const active = () => {
    if (stopped || !vault.profileMasterKey())
      throw new Error("Unlock your named profile first.");
  };
  const label = () => {
    const button = document.querySelector("#profile-sync");
    if (!button || stopped) return;
    const p = state();
    button.querySelector(".nav-label").textContent = p.username
      ? `Profile · ${p.username}`
      : "Profile sync";
    button.title = (p.username ? p.username + " · " : "") + message;
    button.dataset.syncState = conflict
      ? "conflict"
      : p.hub
        ? "enabled"
        : "local";
  };
  function report(text) {
    message = text;
    label();
    const el = document.querySelector("#profile-sync-status");
    if (el) el.textContent = text;
  }
  async function request(
    base,
    path,
    { method = "GET", token = "", admin = "", body } = {},
  ) {
    const ipn = host.getIPN();
    if (!ipn) throw new Error("Connect Tailscale first.");
    const headers = {};
    if (token) headers.Authorization = "Profile " + token;
    if (admin) {
      headers.Authorization = "Bearer " + admin;
      if (token) headers["X-Profile-Key"] = token;
    }
    if (body) headers["Content-Type"] = "application/json";
    const r = await ipn.fetch(base + path, {
      method,
      headers,
      body: body ? JSON.stringify(body) : undefined,
      timeoutMs: 5000,
    });
    const text = await r.text();
    let value;
    try {
      value = JSON.parse(text);
    } catch {
      throw new Error("This server does not provide profile sync.");
    }
    if (r.status >= 400)
      throw new HubError(r.status, value.error || "Profile request failed.");
    return value;
  }
  async function service(base) {
    const info = await request(base, "/v1/profiles");
    if (
      info.service !== "tailterm-profiles" ||
      info.version !== 2 ||
      !Array.isArray(info.envelopeVersions) ||
      !info.envelopeVersions.includes(2) ||
      info.minimumWriteEnvelopeVersion !== 2 ||
      !/^profilehub_[0-9a-f]{16}$/.test(info.instanceId)
    )
      throw new Error("Unsupported profile server.");
    return info;
  }
  async function credentials(base, expected) {
    active();
    const master = vault.profileMasterKey(),
      username = state().username;
    const info = await service(base);
    active();
    if (expected && expected !== info.instanceId)
      throw new Error("The profile server identity changed. Sync is paused.");
    const keys = await profileKeys(master, username, info.instanceId);
    active();
    return { keys, username, instanceId: info.instanceId, hub: base };
  }
  const connection = (c, remote) => ({
    hub: c.hub,
    instanceId: c.instanceId,
    autoRestore: true,
    revision: remote.revision,
    updatedAt: remote.updatedAt,
  });
  async function apply(c, remote, snapshot) {
    const payload = await c.keys.open(remote.envelope);
    active();
    await vault.applyRemoteProfile(
      payload,
      connection(c, remote),
      snapshot.serial,
    );
    active();
    conflict = null;
    await host.reloadData();
    report("Synced");
  }
  async function push(c, revision, snapshot) {
    const envelope = await c.keys.seal(snapshot.data);
    active();
    const remote = await request(c.hub, "/v1/profiles/" + c.username, {
      method: "PUT",
      token: c.keys.token,
      body: { revision, envelope },
    });
    active();
    await vault.setProfileConnection(connection(c, remote), snapshot.serial);
    report(state().dirty ? "Changes pending" : "Synced");
  }
  async function tick() {
    if (
      stopped ||
      busy ||
      conflict ||
      !host.getIPN() ||
      !state().hub ||
      !vault.profileMasterKey()
    )
      return;
    busy = true;
    try {
      const p = state(),
        c = await credentials(p.hub, p.instanceId);
      const remote = await request(c.hub, "/v1/profiles/" + c.username, {
        token: c.keys.token,
      });
      active();
      const snapshot = await vault.profileSnapshot();
      active();
      const current = state();
      if (remote.revision < current.revision)
        throw new Error(
          "The server has an older profile revision. Sync is paused.",
        );
      if (remote.revision !== current.revision) {
        if (current.dirty) {
          conflict = { c, remote };
          report("Changes on both devices · review required");
          return;
        }
        await apply(c, remote, snapshot);
      } else if (current.dirty) {
        await push(c, current.revision, snapshot);
      } else report("Synced");
    } catch (e) {
      if (!stopped)
        report(
          e.status === 409
            ? "Another device saved first · retrying"
            : e.message,
        );
    } finally {
      busy = false;
    }
  }
  async function restore(base, { automatic = false } = {}) {
    const c = await credentials(base),
      remote = await request(base, "/v1/profiles/" + c.username, {
        token: c.keys.token,
      });
    active();
    const snapshot = await vault.profileSnapshot();
    active();
    const hasLocal =
      snapshot.data.servers.length ||
      snapshot.data.keys.length ||
      snapshot.data.sessions.length ||
      snapshot.data.hub?.url;
    if (hasLocal) {
      // Restoring onto an existing local profile requires an explicit choice.
      conflict = { c, remote, joining: true };
      report("Saved profile found · review before restoring");
      host.notice(
        "Saved profile found. Open Profile sync to review it alongside this browser’s local settings.",
      );
      if (!automatic) show();
      return;
    }
    await apply(c, remote, snapshot);
    host.notice("Restored profile " + c.username + ".");
  }
  async function discover() {
    if (
      stopped ||
      busy ||
      scanning ||
      state().hub ||
      state().autoRestore === false ||
      !host.getIPN() ||
      !vault.profileMasterKey() ||
      conflict
    )
      return;
    scanning = true;
    try {
      const candidates = [
        host.getData().hub?.url,
        ...host
          .getPeers()
          .filter((p) => p.online !== false)
          .map((p) => {
            const name = (p.name || "").replace(/\.$/, "");
            return name ? "http://" + name + ":18765" : "";
          }),
      ]
        .filter(Boolean)
        .map(normalizeHubURL)
        .filter(Boolean)
        .filter((u) => !checked.has(u))
        .slice(0, 32);
      for (let i = 0; i < candidates.length; i += 4) {
        if (stopped) return;
        const results = await Promise.allSettled(
          candidates.slice(i, i + 4).map(async (hub) => {
            checked.add(hub);
            return { hub, ...(await service(hub)) };
          }),
        );
        for (const r of results)
          if (
            r.status === "fulfilled" &&
            !discovered.some((d) => d.instanceId === r.value.instanceId)
          )
            discovered.push(r.value);
      }
      if (stopped) return;
      if (
        discovered.length === 1 &&
        !attempted.has(discovered[0].instanceId + state().username)
      ) {
        attempted.add(discovered[0].instanceId + state().username);
        busy = true;
        try {
          await restore(discovered[0].hub, { automatic: true });
        } catch (e) {
          if (!stopped)
            report(
              e.status === 403
                ? "Local profile · no matching saved profile"
                : e.message,
            );
        } finally {
          busy = false;
        }
      } else if (discovered.length > 1) report("Choose a profile server");
    } finally {
      scanning = false;
    }
  }
  function schedule() {
    if (stopped) return;
    if (state().hub)
      report(
        host.getIPN()
          ? "Changes pending"
          : "Saved locally · waiting for Tailscale",
      );
    clearTimeout(timer);
    timer = setTimeout(() => {
      void tick();
    }, 800);
  }
  async function action(fn) {
    if (busy)
      throw new Error("Profile sync is already running. Try again shortly.");
    busy = true;
    try {
      await fn();
    } finally {
      busy = false;
    }
  }
  function show() {
    const p = state(),
      hub = p.hub || host.getData().hub?.url || discovered[0]?.hub || "";
    host.dialog(
      "Profile sync",
      `<p class="fine">Your username identifies your profile. Your vault passphrase unlocks it on each computer, after Tailscale connects.</p>
    <form id="profile-sync-form"><label>Username<input id="profile-username" value="${esc(p.username)}" placeholder="e.g. stephen" autocomplete="username" maxlength="64" required ${p.hub ? "readonly" : ""}></label>
    <label>Vault passphrase<input id="profile-password" type="password" autocomplete="current-password" placeholder="${vault.profileMasterKey() ? "Already unlocked · leave blank" : "Your existing vault passphrase"}"></label>
    <label>Profile server<input id="profile-hub" value="${esc(hub)}" placeholder="http://your-hub:18765" list="profile-servers" ${p.hub ? "readonly" : ""}></label>
    <datalist id="profile-servers">${discovered.map((d) => `<option value="${esc(d.hub)}">`).join("")}</datalist>
    <p id="profile-sync-status" class="fine" role="status">${esc(message)}</p>
    ${
      conflict
        ? `<p class="fine">This browser has local settings too. Choose which copy to keep. A downloaded backup preserves this device’s current copy.</p><button type="button" id="profile-backup">Download local backup</button><div class="dialog-actions"><button type="button" id="profile-use-remote">Use server copy</button><button type="button" id="profile-use-local">Keep this device’s copy</button></div>`
        : p.hub
          ? '<div class="dialog-actions"><button type="button" id="profile-disconnect">Stop syncing this device</button><button type="submit" class="primary">Sync now</button></div>'
          : '<div class="dialog-actions"><button type="button" id="profile-find">Find servers</button><button type="submit" name="mode" value="restore">Restore my profile</button><button type="submit" name="mode" value="enable" class="primary">Enable sync</button></div>'
    }
    <details class="dialog-details"><summary>What travels with me?</summary><p class="fine">Saved servers, credentials, session bookmarks, agent setups and appearance. Tailscale device identity, open window layout and temporary credentials stay on this computer. Recent encrypted versions are retained on the server.</p><button type="button" id="profile-recovery">Download previous local copy</button></details></form>`,
    );
    const form = document.querySelector("#profile-sync-form");
    const guarded = (fn) => async (event) => {
      event.preventDefault();
      try {
        await fn(event);
      } catch (e) {
        if (!stopped) report(e.message);
      }
    };
    const identify = async () => {
      if (!host.getIPN()) {
        void host.connect();
        throw new Error(
          "Complete Tailscale sign-in first, then reopen Profile sync.",
        );
      }
      const username = normalizeUsername(
          form.querySelector("#profile-username").value,
        ),
        password = form.querySelector("#profile-password").value;
      if (
        !vault.profileMasterKey() ||
        username !== state().username ||
        password
      )
        await vault.nameProfile(username, password);
      form.querySelector("#profile-password").value = "";
      const base = normalizeHubURL(form.querySelector("#profile-hub").value);
      if (!base) throw new Error("Choose a profile server.");
      return base;
    };
    form.onsubmit = guarded(async (event) => {
      const mode = event.submitter?.value;
      await action(async () => {
        const base = await identify();
        if (state().hub) return;
        if (mode === "restore") {
          await restore(base);
          return;
        }
        const hubConfig = host.getData().hub;
        if (!hubConfig?.token || normalizeHubURL(hubConfig.url) !== base)
          throw new Error(
            "Configure this server as your Task hub first. Its token is only needed to create a profile.",
          );
        await vault.saveProfileAppearance(host.getAppearance());
        const c = await credentials(base),
          snapshot = await vault.profileSnapshot(),
          envelope = await c.keys.seal(snapshot.data);
        active();
        const remote = await request(base, "/v1/profiles/" + c.username, {
          method: "POST",
          admin: hubConfig.token,
          token: c.keys.token,
          body: { envelope },
        });
        active();
        await vault.setProfileConnection(
          connection(c, remote),
          snapshot.serial,
        );
        await host.reloadData();
        report("Synced");
      });
      if (!conflict) {
        await tick();
        show();
      }
    });
    const on = (id, fn) => {
      const b = form.querySelector(id);
      if (b) b.onclick = guarded(fn);
    };
    on("#profile-find", async () => {
      const username = normalizeUsername(
        form.querySelector("#profile-username").value,
      );
      if (!vault.profileMasterKey() || username !== state().username)
        await vault.nameProfile(
          username,
          form.querySelector("#profile-password").value,
        );
      form.querySelector("#profile-password").value = "";
      if (!host.getIPN()) {
        void host.connect();
        return;
      }
      checked.clear();
      attempted.clear();
      await discover();
      show();
    });
    on("#profile-disconnect", () =>
      action(async () => {
        await vault.setProfileConnection({
          hub: "",
          instanceId: "",
          revision: 0,
          autoRestore: false,
        });
        conflict = null;
        report("Local profile");
        await host.reloadData();
        show();
      }),
    );
    on("#profile-backup", async () =>
      host.download(await vault.exportBackup(), "tailterm-local-profile.json"),
    );
    on("#profile-recovery", async () =>
      host.download(
        await vault.exportProfileRecovery(),
        "tailterm-previous-profile.json",
      ),
    );
    on("#profile-use-remote", () =>
      action(async () => {
        const pending = conflict;
        if (
          !(await host.confirm(
            "Use the server profile?",
            "This replaces saved settings in this browser. Your current local copy is retained for recovery.",
          ))
        )
          return;
        const snapshot = await vault.profileSnapshot();
        active();
        const latest = await request(
          pending.c.hub,
          "/v1/profiles/" + pending.c.username,
          { token: pending.c.keys.token },
        );
        await apply(pending.c, latest, snapshot);
        show();
      }),
    );
    on("#profile-use-local", () =>
      action(async () => {
        const pending = conflict;
        if (
          !(await host.confirm(
            "Keep this device’s profile?",
            "This publishes your local settings. The server’s previous version is retained.",
          ))
        )
          return;
        const snapshot = await vault.profileSnapshot();
        active();
        try {
          await push(pending.c, pending.remote.revision, snapshot);
        } catch (error) {
          if (error.status === 409) {
            pending.remote = await request(
              pending.c.hub,
              "/v1/profiles/" + pending.c.username,
              { token: pending.c.keys.token },
            );
            throw new Error(
              "The server changed again. Review your choice and try again.",
            );
          }
          throw error;
        }
        conflict = null;
        show();
      }),
    );
  }
  let discoveryTicks = 0;
  const periodic = setInterval(() => {
    if (!state().hub && !discovered.length && ++discoveryTicks % 4 === 0) {
      checked.clear();
      void discover();
    } else void tick();
  }, 15000);
  window.addEventListener("tailterm-profile-change", schedule);
  window.addEventListener("focus", schedule);
  const connected = () => {
    if (stopped) return;
    if (!host.getIPN()) {
      checked.clear();
      if (state().hub) report("Saved locally · waiting for Tailscale");
      return;
    }
    return state().hub ? tick() : discover();
  };
  label();
  return {
    show,
    connected,
    stop() {
      stopped = true;
      clearInterval(periodic);
      clearTimeout(timer);
      window.removeEventListener("tailterm-profile-change", schedule);
      window.removeEventListener("focus", schedule);
      conflict = null;
    },
  };
}
