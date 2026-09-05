export function setupTerminalView() {
  const shell = document.querySelector(".terminal-shell");
  const fullscreen = document.querySelector("#fullscreen");
  const expand = document.createElement("button");
  expand.id = "expand-terminal";
  fullscreen.before(expand);
  function update() {
    const expanded = shell.classList.contains("expanded");
    expand.textContent = expanded ? "Restore" : "Expand";
    expand.setAttribute(
      "aria-label",
      expanded ? "Restore terminal size" : "Expand terminal in browser window",
    );
    expand.setAttribute("aria-pressed", String(expanded));
    expand.title = expanded
      ? "Restore terminal size\nReturn to the workspace layout."
      : "Expand terminal\nFill the browser window; keep the browser tabs and address bar.";
    fullscreen.setAttribute(
      "aria-label",
      document.fullscreenElement ? "Exit fullscreen" : "Enter fullscreen",
    );
    fullscreen.title = document.fullscreenElement
      ? "Exit fullscreen"
      : "Fullscreen\nFill the display and hide browser controls.";
  }
  expand.onclick = async () => {
    if (document.fullscreenElement) await document.exitFullscreen();
    shell.classList.toggle("expanded");
    update();
  };
  fullscreen.onclick = async () => {
    try {
      if (document.fullscreenElement) await document.exitFullscreen();
      else await shell.requestFullscreen();
    } catch {
      // Window expansion remains available if the browser disallows fullscreen.
      shell.classList.add("expanded");
    }
    update();
  };
  document.addEventListener("fullscreenchange", update);
  update();
}
