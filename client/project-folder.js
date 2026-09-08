const esc = (value) =>
  String(value ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
export function projectFolderHTML(
  id,
  value = "",
  label = "Project folder",
  optional = false,
) {
  return `<div class="project-folder"><label for="${esc(id)}">${esc(label)}</label><div class="project-folder-input"><input id="${esc(id)}" value="${esc(value)}" placeholder="${optional ? "Use the folder chosen at launch" : "/absolute/project/path"}" autocomplete="off" spellcheck="false"><button type="button" data-folder-browse>Browse…</button></div><div class="project-folder-browser" hidden></div></div>`;
}
// A small read-only SFTP picker. It stays inside the existing form, so browsing
// never replaces a task draft or changes the Files workspace mode.
export function wireProjectFolder(root, host, getServer) {
  const serverKey = (server) =>
    JSON.stringify([server?.id, server?.host, server?.port, server?.username]);
  const input = root.querySelector("input"),
    browser = root.querySelector(".project-folder-browser"),
    toggle = root.querySelector("[data-folder-browse]");
  let generation = 0,
    current = "";
  async function navigate(path) {
    const request = ++generation,
      server = getServer();
    browser.hidden = false;
    browser.textContent = server
      ? `Loading folders on ${server.name}…`
      : "Choose a machine first.";
    if (!server) return;
    const targetServer = serverKey(server);
    let connection;
    try {
      if (!host.openSFTP)
        throw new Error(
          "Folder browsing is unavailable. Enter an absolute path.",
        );
      connection = await host.openSFTP(server);
      connection.done?.catch(() => {});
      const target = await connection.realpath(
        path || (await connection.home()),
      );
      const result = await connection.list(target);
      if (
        request !== generation ||
        !root.isConnected ||
        input.disabled ||
        serverKey(getServer()) !== targetServer
      )
        return;
      current = result.path;
      const directories = result.entries
        .filter((e) => e.isDir && e.name !== "." && e.name !== "..")
        .sort((a, b) => a.name.localeCompare(b.name));
      browser.innerHTML = `<div class="folder-navigation"><button type="button" data-folder-home>Home</button><button type="button" data-folder-up ${current === "/" ? "disabled" : ""}>Up</button><button type="button" data-folder-cancel>Cancel</button></div><p class="fine folder-current"></p><div class="folder-directories">${directories.map((e, i) => `<button type="button" data-folder-entry="${i}">${esc(e.name)}/</button>`).join("") || '<p class="fine">No subfolders.</p>'}</div>${result.truncated ? '<p class="fine">Listing limited. You can enter a full path above.</p>' : ""}<button type="button" data-folder-use>Use this folder</button>`;
      browser.querySelector(".folder-current").textContent =
        `${server.name} · ${current}`;
      browser.querySelector("[data-folder-home]").onclick = () => navigate("");
      browser.querySelector("[data-folder-up]").onclick = () =>
        navigate(current.replace(/\/+$/, "").replace(/\/[^/]*$/, "") || "/");
      browser.querySelector("[data-folder-cancel]").onclick = () => {
        generation++;
        browser.hidden = true;
      };
      browser
        .querySelectorAll("[data-folder-entry]")
        .forEach(
          (button) =>
            (button.onclick = () =>
              navigate(
                (current === "/" ? "" : current) +
                  "/" +
                  directories[Number(button.dataset.folderEntry)].name,
              )),
        );
      browser.querySelector("[data-folder-use]").onclick = () => {
        if (input.disabled || serverKey(getServer()) !== targetServer) {
          browser.textContent = "Machine changed. Browse again.";
          return;
        }
        input.value = current;
        input.dispatchEvent(new Event("input", { bubbles: true }));
        input.dispatchEvent(new Event("change", { bubbles: true }));
        browser.hidden = true;
        generation++;
      };
    } catch (error) {
      if (request === generation && root.isConnected)
        browser.textContent = error.message;
    } finally {
      connection?.close();
    }
  }
  toggle.onclick = () => {
    if (!browser.hidden) {
      generation++;
      browser.hidden = true;
    } else void navigate(input.value.trim());
  };
}
