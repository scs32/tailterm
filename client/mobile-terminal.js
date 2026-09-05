export function setupMobileTerminal({
  current,
  tabs,
  activate,
  dialog,
  close,
}) {
  const toolbar = document.createElement("div");
  toolbar.className = "mobile-terminal-keys";
  toolbar.setAttribute("aria-label", "Terminal keyboard controls");
  const add = (label, run) => {
    const b = document.createElement("button");
    b.textContent = label;
    b.onpointerdown = (e) => e.preventDefault();
    b.onclick = run;
    toolbar.append(b);
  };
  add("Sessions", () => {
    dialog("Sessions", '<div id="mobile-sessions"></div>');
    for (const t of tabs()) {
      const b = document.createElement("button");
      b.textContent = `${t.session || "SSH shell"} · ${t.server.name} · ${t.status}`;
      b.onclick = () => {
        close();
        activate(t.id);
      };
      document.querySelector("#mobile-sessions").append(b);
    }
  });
  add("Keyboard", () => {
    current()?.history?.close();
    current()?.term.focus();
  });
  for (const [label, key] of [
    ["Esc", "\x1b"],
    ["Tab", "\t"],
    ["Ctrl-C", "\x03"],
    ["↑", "\x1b[A"],
    ["↓", "\x1b[B"],
    ["←", "\x1b[D"],
    ["→", "\x1b[C"],
  ])
    add(label, () => {
      const t = current();
      if (t?.status === "Connected") {
        t.history?.close();
        t.send?.(key);
        t.term.focus();
      }
    });
  add("Select text", () => {
    const t = current();
    if (!t) return;
    dialog(
      "Select terminal text",
      '<p>Select and copy from the retained output below.</p><textarea id="mobile-output" readonly aria-label="Terminal output"></textarea>',
    );
    const lines = [];
    const buffer = t.term.buffer.active;
    for (let i = 0; i < buffer.length; i++)
      lines.push(buffer.getLine(i)?.translateToString(true) || "");
    document.querySelector("#mobile-output").value = lines.join("\n");
  });
  document.querySelector(".terminal-footer").before(toolbar);
  const resize = () => {
    const viewport = window.visualViewport;
    document.documentElement.style.setProperty(
      "--visible-height",
      `${viewport?.height || innerHeight}px`,
    );
    document.body.classList.toggle(
      "mobile-keyboard",
      !!viewport && innerHeight - viewport.height > 120,
    );
  };
  window.visualViewport?.addEventListener("resize", resize);
  window.addEventListener("resize", resize);
  resize();
}
