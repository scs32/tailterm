// Files mode: a server-wide file manager over the browser's SFTP session.
// Browse, download, upload, create folders, rename, and delete on one server.
import { downloadBlob } from "./terminal-extras.js";

const esc = (s) =>
  String(s ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
const PATHS_KEY = "tailserve.files.paths";
const MAX_UPLOAD = 4 * 1024 * 1024 * 1024;
const STREAM_THRESHOLD = 256 * 1024 * 1024;
export const NAME_RE = /^[^/\u0000-\u001f\u007f]{1,255}$/;
const errText = (e) => (e && e.message) || String(e);

export function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let value = bytes / 1024,
    i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i++;
  }
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${units[i]}`;
}
export function joinPath(dir, name) {
  return dir === "/" ? "/" + name : dir.replace(/\/+$/, "") + "/" + name;
}
export function parentPath(path) {
  if (path === "/") return "/";
  const parts = path.replace(/\/+$/, "").split("/");
  parts.pop();
  return parts.length <= 1 ? "/" : parts.join("/");
}
export function sortEntries(entries, key, descending) {
  const dir = descending ? -1 : 1;
  return [...entries].sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
    if (key === "size") return (a.size - b.size) * dir;
    if (key === "mtime") return (a.mtime - b.mtime) * dir;
    return a.name.localeCompare(b.name, undefined, { numeric: true }) * dir;
  });
}
const remembered = () => {
  try {
    return JSON.parse(localStorage.getItem(PATHS_KEY) || "{}");
  } catch {
    return {};
  }
};
const remember = (serverId, path) => {
  try {
    localStorage.setItem(
      PATHS_KEY,
      JSON.stringify({ ...remembered(), [serverId]: path }),
    );
  } catch {}
};

export function createFilesView({
  getServers,
  currentServer,
  openSFTP,
  notice,
  confirm,
  dialog,
  closeDialog,
  openTerminal,
}) {
  let root = null,
    server = null,
    sftp = null,
    path = null,
    entries = [],
    truncated = false,
    sort = { key: "name", descending: false },
    busy = "",
    error = "",
    selected = null,
    generation = 0;

  function mount(container) {
    root = container;
  }
  async function show() {
    if (!root) return;
    if (!server) server = currentServer() || getServers()[0] || null;
    render();
    if (server && !sftp) await connect();
  }
  function hide() {
    disconnect();
  }
  function disconnect() {
    generation++;
    try {
      sftp?.close();
    } catch {}
    sftp = null;
  }
  async function connect() {
    const token = ++generation;
    busy = `Connecting to ${server.name}…`;
    error = "";
    render();
    try {
      const session = await openSFTP(server);
      if (token !== generation) {
        session.close();
        return;
      }
      sftp = session;
      session.done
        .catch((e) => {
          if (token === generation) error = e.message;
        })
        .finally(() => {
          if (token === generation) {
            sftp = null;
            busy = "";
            render();
          }
        });
      const start = remembered()[server.id];
      await navigate(start || (await session.home()), token);
    } catch (e) {
      if (token !== generation) return;
      error = errText(e);
      busy = "";
      render();
    }
  }
  async function navigate(target, token = generation) {
    if (!sftp) return;
    busy = "Listing…";
    error = "";
    render();
    try {
      const resolved = await sftp.realpath(target);
      const result = await sftp.list(resolved);
      if (token !== generation) return;
      path = result.path;
      entries = result.entries;
      truncated = result.truncated;
      selected = null;
      remember(server.id, path);
    } catch (e) {
      if (token !== generation) return;
      error = errText(e);
      if (!path) {
        try {
          const home = await sftp.home();
          const result = await sftp.list(home);
          path = result.path;
          entries = result.entries;
        } catch {}
      }
    }
    busy = "";
    render();
  }
  async function refresh() {
    if (path) await navigate(path);
  }
  async function run(label, action) {
    if (!sftp) return;
    busy = label;
    error = "";
    render();
    try {
      await action();
    } catch (e) {
      error = errText(e);
      notice(errText(e));
    }
    busy = "";
    await refresh();
  }

  async function download(entry) {
    const target = joinPath(path, entry.name);
    const chunks = [];
    let writable = null;
    // Small files download directly. Large ones stream to a chosen location
    // when the browser offers the File System Access API.
    if (entry.size > STREAM_THRESHOLD && window.showSaveFilePicker) {
      try {
        const handle = await window.showSaveFilePicker({
          suggestedName: entry.name,
        });
        writable = await handle.createWritable();
      } catch (e) {
        if (e.name === "AbortError") return;
        writable = null;
      }
    }
    await run(`Downloading ${entry.name}…`, async () => {
      await sftp.read(target, async (chunk) => {
        if (writable) await writable.write(chunk);
        else chunks.push(chunk);
        progress(chunk.byteLength);
      });
      if (writable) await writable.close();
      else downloadBlob(new Blob(chunks), entry.name);
    });
  }
  let transferred = 0,
    transferTotal = 0;
  function progress(bytes) {
    transferred += bytes;
    const el = root?.querySelector("#files-progress");
    if (el)
      el.textContent = transferTotal
        ? `${formatSize(transferred)} of ${formatSize(transferTotal)}`
        : formatSize(transferred);
  }
  async function upload(files, overwrite = false) {
    for (const file of files) {
      if (!NAME_RE.test(file.name)) {
        notice(`Skipped ${file.name}: unsupported filename.`);
        continue;
      }
      if (file.size > MAX_UPLOAD) {
        notice(`Skipped ${file.name}: larger than 4 GiB.`);
        continue;
      }
      transferred = 0;
      transferTotal = file.size;
      await run(`Uploading ${file.name}…`, () =>
        sftp.write(joinPath(path, file.name), {
          size: file.size,
          overwrite,
          progress: (offset) => {
            transferred = 0;
            progress(offset);
          },
          readChunk: async (offset, length) =>
            new Uint8Array(
              await file.slice(offset, offset + length).arrayBuffer(),
            ),
        }),
      );
      transferTotal = 0;
    }
  }
  function prompt(title, label, value, onSubmit) {
    dialog(
      title,
      `<form id="files-prompt"><label>${esc(label)}<input id="files-prompt-value" value="${esc(value)}" autocomplete="off" spellcheck="false" required></label><p id="files-prompt-error" class="fine" role="alert"></p><div class="dialog-actions"><button class="primary" type="submit">OK</button></div></form>`,
    );
    const input = document.querySelector("#files-prompt-value");
    input.focus();
    input.select();
    document.querySelector("#files-prompt").onsubmit = async (e) => {
      e.preventDefault();
      const next = input.value.trim();
      if (!NAME_RE.test(next)) {
        document.querySelector("#files-prompt-error").textContent =
          "Names cannot contain slashes or control characters.";
        return;
      }
      closeDialog();
      await onSubmit(next);
    };
  }

  function render() {
    if (!root) return;
    const servers = getServers();
    const options = servers
      .map(
        (s) =>
          `<option value="${esc(s.id)}" ${s.id === server?.id ? "selected" : ""}>${esc(s.name)} · ${esc(s.username)}@${esc(s.host)}</option>`,
      )
      .join("");
    const crumbs = path
      ? path
          .split("/")
          .filter(Boolean)
          .reduce((acc, part) => {
            const full = joinPath(acc.at(-1)?.full || "/", part);
            acc.push({ part, full });
            return acc;
          }, [])
      : [];
    const sorted = sortEntries(entries, sort.key, sort.descending);
    const arrow = (key) =>
      sort.key === key ? (sort.descending ? " ↓" : " ↑") : "";
    const rows = sorted
      .map(
        (e) =>
          `<tr data-entry="${esc(e.name)}" class="${e.isDir ? "is-dir" : ""} ${selected === e.name ? "selected" : ""}" tabindex="0"><td class="files-name"><span class="files-icon">${e.isDir ? "▸" : e.isLink ? "↗" : "·"}</span>${esc(e.name)}</td><td class="files-size">${e.isDir ? "" : formatSize(e.size)}</td><td class="files-mtime">${esc(new Date(e.mtime).toLocaleString([], { dateStyle: "short", timeStyle: "short" }))}</td><td class="files-mode">${esc(e.mode)}</td></tr>`,
      )
      .join("");
    root.innerHTML = `<div class="files-view"><div class="files-bar"><select id="files-server" aria-label="Server">${options || '<option value="">No servers</option>'}</select><nav class="files-crumbs" aria-label="Path"><button data-path="/" title="Root">/</button>${crumbs
      .map((c) => `<button data-path="${esc(c.full)}">${esc(c.part)}</button>`)
      .join(
        '<span class="files-sep">/</span>',
      )}</nav><form id="files-goto"><input id="files-path" value="${esc(path || "")}" placeholder="/absolute/path" aria-label="Go to path" autocomplete="off" spellcheck="false"></form></div><div class="files-actions"><button id="files-up" ${!path || path === "/" ? "disabled" : ""}>↑ Up</button><button id="files-refresh" ${!sftp ? "disabled" : ""}>↻ Refresh</button><button id="files-mkdir" ${!sftp || !path ? "disabled" : ""}>＋ Folder</button><label class="files-upload-label"><input id="files-upload" type="file" multiple hidden ${!sftp || !path ? "disabled" : ""}><span class="button-like">⇡ Upload</span></label><span class="files-spacer"></span>${
      selected
        ? `<button id="files-open" ${sorted.find((e) => e.name === selected)?.isDir ? "" : "hidden"}>Open</button><button id="files-download" ${sorted.find((e) => e.name === selected)?.isDir ? "hidden" : ""}>⇣ Download</button><button id="files-rename">Rename</button><button id="files-delete" class="danger">Delete</button>`
        : ""
    }<button id="files-terminal" ${!path ? "disabled" : ""} title="Open a terminal in this folder">Terminal here</button></div><p class="fine files-status">${esc(busy)}${busy ? ` <span id="files-progress"></span>` : ""}${error ? `<span class="files-error">${esc(error)}</span>` : ""}${truncated ? " Listing truncated at 10,000 entries." : ""}${!busy && !error && path ? `${entries.length} item${entries.length === 1 ? "" : "s"}` : ""}</p><div class="files-table-wrap"><table class="files-table"><thead><tr><th><button data-sort="name">Name${arrow("name")}</button></th><th><button data-sort="size">Size${arrow("size")}</button></th><th><button data-sort="mtime">Modified${arrow("mtime")}</button></th><th>Mode</th></tr></thead><tbody>${
      rows ||
      `<tr><td colspan="4" class="fine">${sftp ? (path ? "Empty folder." : "") : server ? "Not connected." : "Add a server first."}</td></tr>`
    }</tbody></table></div><div class="files-drop fine">Drop files here to upload into ${esc(path || "the current folder")}.</div></div>`;

    root.querySelector("#files-server").onchange = async (e) => {
      server = servers.find((s) => s.id === e.target.value) || null;
      disconnect();
      path = null;
      entries = [];
      if (server) await connect();
      else render();
    };
    root
      .querySelectorAll("[data-path]")
      .forEach((b) => (b.onclick = () => navigate(b.dataset.path)));
    root.querySelector("#files-goto").onsubmit = (e) => {
      e.preventDefault();
      const value = root.querySelector("#files-path").value.trim();
      if (value.startsWith("/")) navigate(value);
      else notice("Paths must be absolute.");
    };
    root.querySelector("#files-up").onclick = () => navigate(parentPath(path));
    root.querySelector("#files-refresh").onclick = refresh;
    root.querySelector("#files-mkdir").onclick = () =>
      prompt("New folder", "Folder name", "new-folder", (name) =>
        run(`Creating ${name}…`, () => sftp.mkdir(joinPath(path, name))),
      );
    root.querySelector("#files-upload").onchange = (e) => {
      const files = [...e.target.files];
      e.target.value = "";
      upload(files);
    };
    root.querySelectorAll("[data-sort]").forEach(
      (b) =>
        (b.onclick = () => {
          const key = b.dataset.sort;
          sort =
            sort.key === key
              ? { key, descending: !sort.descending }
              : { key, descending: false };
          render();
        }),
    );
    root.querySelectorAll("tr[data-entry]").forEach((row) => {
      const entry = entries.find((e) => e.name === row.dataset.entry);
      row.onclick = () => {
        selected = entry.name;
        render();
        root
          .querySelector(`tr[data-entry="${CSS.escape(entry.name)}"]`)
          ?.focus();
      };
      row.ondblclick = () => {
        if (entry.isDir) navigate(joinPath(path, entry.name));
        else download(entry);
      };
      row.onkeydown = (e) => {
        if (e.key === "Enter") row.ondblclick();
      };
    });
    const current = entries.find((e) => e.name === selected);
    if (current) {
      const open = root.querySelector("#files-open");
      if (open) open.onclick = () => navigate(joinPath(path, current.name));
      const dl = root.querySelector("#files-download");
      if (dl) dl.onclick = () => download(current);
      root.querySelector("#files-rename").onclick = () =>
        prompt("Rename", "New name", current.name, (name) =>
          run(`Renaming ${current.name}…`, () =>
            sftp.rename(joinPath(path, current.name), joinPath(path, name)),
          ),
        );
      root.querySelector("#files-delete").onclick = async () => {
        if (
          !(await confirm(
            `Delete ${current.name}?`,
            current.isDir
              ? "Only empty folders can be deleted from here. This cannot be undone."
              : "The file is removed from the server. This cannot be undone.",
          ))
        )
          return;
        await run(`Deleting ${current.name}…`, () =>
          sftp.remove(joinPath(path, current.name)),
        );
      };
    }
    root.querySelector("#files-terminal").onclick = () =>
      openTerminal(server, path);
    const drop = root.querySelector(".files-view");
    drop.ondragover = (e) => {
      if (!sftp || !path) return;
      e.preventDefault();
      drop.classList.add("dragging");
    };
    drop.ondragleave = () => drop.classList.remove("dragging");
    drop.ondrop = (e) => {
      drop.classList.remove("dragging");
      if (!sftp || !path) return;
      e.preventDefault();
      upload([...e.dataTransfer.files]);
    };
  }
  return {
    mount,
    show,
    hide,
    navigate,
    refresh,
    current: () => ({ server, path }),
  };
}
