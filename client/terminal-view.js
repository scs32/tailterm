export function setupTerminalView() {
  const shell = document.querySelector("#workspace");
  const fullscreen = document.querySelector("#fullscreen");
  const expand = document.createElement("button");
  expand.id = "expand-terminal";
  document
    .querySelector("main > header .header-right")
    .append(expand, fullscreen);
  function update() {
    const expanded = shell.classList.contains("expanded");
    expand.textContent = expanded ? "Restore" : "Expand";
    expand.setAttribute(
      "aria-label",
      expanded ? "Restore workspace layout" : "Expand workspace view",
    );
    expand.setAttribute("aria-pressed", String(expanded));
    expand.title = expanded
      ? "Restore workspace layout\nReturn to the workspace layout."
      : "Expand workspace\nHide the server sidebar and give the current view more room.";
    fullscreen.setAttribute(
      "aria-label",
      document.fullscreenElement ? "Exit fullscreen" : "Enter fullscreen",
    );
    fullscreen.title = document.fullscreenElement
      ? "Exit fullscreen"
      : "Fullscreen\nFill the display and hide browser controls.";
  }
  expand.onclick = async () => {
    shell.classList.toggle("expanded");
    update();
  };
  fullscreen.onclick = async () => {
    try {
      if (document.fullscreenElement) await document.exitFullscreen();
      else await document.documentElement.requestFullscreen();
    } catch {
      // Window expansion remains available if the browser disallows fullscreen.
      fullscreen.title = "Fullscreen is unavailable in this browser.";
      return;
    }
    update();
  };
  document.addEventListener("fullscreenchange", update);
  update();
}
