import { tmuxDirectoryCommand, shellQuote } from "../shared/tmux-command.js";

export function validateImageFiles(files) {
  if (!files.length || files.length > 10)
    throw new Error("Choose between 1 and 10 images.");
  for (const file of files) {
    if (
      !/^image\//.test(file.type) &&
      !/\.(png|jpe?g|gif|webp|heic|heif|avif|bmp|tiff?)$/i.test(file.name)
    )
      throw new Error("Only image files can be dropped here.");
    if (file.size < 1 || file.size > 20 * 1024 * 1024)
      throw new Error("Each image must be between 1 byte and 20 MiB.");
    if (
      !file.name ||
      file.name.length > 255 ||
      /[\x00-\x1f\x7f/]/.test(file.name) ||
      [".", ".."].includes(file.name)
    )
      throw new Error(
        "Choose a filename without slashes or control characters.",
      );
  }
  if (files.reduce((sum, f) => sum + f.size, 0) > 100 * 1024 * 1024)
    throw new Error("Upload up to 100 MiB at a time.");
}
export function uploadPath(directory, name) {
  if (
    !directory.startsWith("/") ||
    /[\x00-\x1f\x7f]/.test(directory) ||
    directory.length > 3800
  )
    throw new Error(
      "Choose an absolute remote folder without control characters.",
    );
  return directory.replace(/\/$/, "") + "/" + name;
}
export function setupImageDrops({
  body,
  getTabs,
  getActive,
  command,
  upload,
  dialog,
  close,
  notice,
}) {
  const isFiles = (event) =>
    [...(event.dataTransfer?.types || [])].includes("Files");
  // Prevent the browser navigating away when a file misses a terminal pane.
  document.addEventListener("dragover", (e) => {
    if (isFiles(e)) e.preventDefault();
  });
  document.addEventListener("drop", (e) => {
    if (isFiles(e)) e.preventDefault();
  });
  body.addEventListener("dragover", (e) => {
    if (!isFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "copy";
    body.classList.add("file-drop-active");
  });
  body.addEventListener("dragleave", (e) => {
    if (!body.contains(e.relatedTarget))
      body.classList.remove("file-drop-active");
  });
  let current, cancelActive;
  const tray = document.createElement("button");
  tray.id = "upload-tray";
  tray.hidden = true;
  document.querySelector(".terminal-footer > div").prepend(tray);
  tray.onclick = () => current?.showModal();
  const open = async (files, t) => {
    if (current?.dataset.busy === "true") {
      notice("Finish or cancel the current upload first.");
      if (!current.open) current.showModal();
      return;
    }
    if (!t || t.disposed || t.status !== "Connected")
      return notice("Choose a connected terminal before uploading.");
    if (document.querySelector("dialog[open]"))
      return notice("Close the open dialog before uploading an image.");
    try {
      validateImageFiles(files);
    } catch (e) {
      return notice(e.message);
    }
    current?.remove();
    const modal = document.createElement("dialog");
    modal.id = "upload-dialog";
    modal.setAttribute("aria-label", "Upload images");
    modal.innerHTML =
      '<div class="dialog-head"><h2>Upload images</h2><button id="upload-hide" aria-label="Hide uploads">×</button></div>' +
      '<p id="upload-destination"></p><p class="fine">Transfer over SSH. Existing files won’t be overwritten.</p><label for="upload-directory">Remote folder</label><input id="upload-directory" placeholder="/absolute/remote/folder"><p id="upload-note" role="status">Checking the active tmux pane’s folder...</p><div id="upload-files"></div><label class="remember-password"><input id="upload-insert" type="checkbox" checked> Insert file paths without pressing Enter</label><div class="dialog-actions"><button id="upload-cancel">Cancel</button><button id="upload-start" class="primary">Upload images</button></div>';
    current = modal;
    (document.fullscreenElement || document.body).append(modal);
    modal.showModal();
    const hide = () => modal.close();
    modal.querySelector("#upload-hide").onclick = hide;
    tray.hidden = false;
    tray.textContent = "Uploads: ready";
    const directory = modal.querySelector("#upload-directory"),
      start = modal.querySelector("#upload-start"),
      cancel = modal.querySelector("#upload-cancel"),
      note = modal.querySelector("#upload-note");
    modal.querySelector("#upload-destination").textContent =
      `${t.server.username}@${t.server.host} — ${t.tmux ? t.session : "SSH shell"}`;
    const rows = files.map((file) => {
      const row = document.createElement("p");
      row.textContent = `${file.name} (${Math.ceil(file.size / 1024)} KiB)`;
      modal.querySelector("#upload-files").append(row);
      return row;
    });
    let busy = false,
      cancelled = false,
      transfer;
    start.disabled = true;
    const successful = new Map();
    const cancelTransfer = () => {
      if (busy) {
        cancelled = true;
        transfer?.close();
        note.textContent = "Cancelling upload...";
      } else hide();
    };
    cancel.onclick = cancelTransfer;
    cancelActive = () => {
      cancelled = true;
      transfer?.close();
    };
    modal.oncancel = () => {}; // Escape hides the dialog; transfers continue.
    if (t.tmux && t.target) {
      try {
        const encoded = await command(
          t,
          tmuxDirectoryCommand(t.target, t.server.tmuxPath),
        );
        const value = new TextDecoder()
          .decode(
            Uint8Array.from(atob(encoded.replace(/\s/g, "")), (c) =>
              c.charCodeAt(0),
            ),
          )
          .replace(/\n$/, "");
        if (!directory.value) directory.value = value;
        note.textContent = "Review this folder before uploading.";
      } catch (e) {
        note.textContent =
          "Could not detect the folder. Enter it above: " + e.message;
      }
    } else
      note.textContent =
        "Enter the destination folder. Automatic detection requires a verified tmux session.";
    start.disabled = false;
    start.onclick = async () => {
      if (busy) return;
      if (t.disposed || t.status !== "Connected") {
        note.textContent =
          "The destination terminal disconnected. Reconnect and drop the files again.";
        return;
      }
      let paths;
      try {
        paths = files.map((f) => uploadPath(directory.value, f.name));
      } catch (e) {
        note.textContent = e.message;
        return;
      }
      busy = true;
      modal.dataset.busy = "true";
      tray.textContent = "Uploading...";
      cancelled = false;
      start.disabled = directory.disabled = true;
      try {
        for (const [i, file] of files.entries()) {
          if (successful.has(file)) continue;
          if (cancelled || t.disposed) throw new Error("Upload cancelled.");
          transfer = upload(t, file, paths[i], (count) => {
            tray.textContent = `Uploading ${i + 1}/${files.length}: ${Math.round((count / file.size) * 100)}%`;
            rows[i].textContent =
              `${file.name}: ${Math.round((count / file.size) * 100)}%`;
          });
          const timer = setTimeout(() => transfer.close(), 120000);
          let result;
          try {
            result = await transfer.done;
          } finally {
            clearTimeout(timer);
          }
          if (!result || result.exitCode !== 0)
            throw new Error(
              "Upload cancelled or not confirmed. A temporary .tailterm-upload file may remain in the destination folder.",
            );
          successful.set(file, paths[i]);
          rows[i].textContent = `${file.name}: uploaded`;
          if (cancelled)
            throw new Error(
              "The current file finished before cancellation. No path was inserted.",
            );
        }
        const pathsText = [...successful.values()].map(shellQuote).join(" ");
        const insert = modal.querySelector("#upload-insert").checked;
        if (
          insert &&
          modal.open &&
          !t.el.querySelector(".local-history") &&
          !t.disposed &&
          t.status === "Connected" &&
          getActive() === t.id
        ) {
          t.term.paste(pathsText);
          note.textContent = "Uploaded. Paths inserted without pressing Enter.";
        } else
          note.textContent =
            "Uploaded. Copy the paths below to use them in your terminal.";
        const output = document.createElement("textarea");
        output.readOnly = true;
        output.value = pathsText;
        output.setAttribute("aria-label", "Uploaded image paths");
        modal.querySelector("#upload-files").append(output);
        tray.textContent = "Uploads: complete";
        start.hidden = true;
        cancel.textContent = "Done";
      } catch (e) {
        note.textContent = e.message;
        tray.textContent = cancelled
          ? "Uploads: cancelled"
          : "Uploads: needs attention";
      } finally {
        busy = false;
        modal.dataset.busy = "false";
        start.disabled = directory.disabled = false;
        transfer = null;
      }
    };
  };
  body.addEventListener("drop", (e) => {
    if (!isFiles(e)) return;
    e.preventDefault();
    body.classList.remove("file-drop-active");
    void open(
      [...e.dataTransfer.files],
      getTabs().find((t) => !t.el.hidden && t.el.contains(e.target)),
    );
  });
  body.addEventListener(
    "paste",
    (e) => {
      const files = [...(e.clipboardData?.files || [])].filter((f) =>
        /^image\//.test(f.type),
      );
      if (!files.length) return;
      e.preventDefault();
      e.stopImmediatePropagation();
      void open(
        files.map(
          (f, i) =>
            new File(
              [f],
              `clipboard-${Date.now()}-${i}.${f.type.split("/")[1].replace(/[^a-z0-9]/gi, "") || "png"}`,
              { type: f.type },
            ),
        ),
        getTabs().find((t) => t.el.contains(e.target)),
      );
    },
    true,
  );
  return {
    open,
    cancel: () => cancelActive?.(),
    choose: () => {
      const t = getTabs().find((t) => t.id === getActive());
      const picker = document.createElement("input");
      picker.type = "file";
      picker.accept = "image/*";
      picker.multiple = true;
      picker.onchange = () => void open([...picker.files], t);
      picker.click();
    },
  };
}
