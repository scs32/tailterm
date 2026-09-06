// xterm drops its document drag/release listeners when tmux changes mouse modes
// while opening a menu. Finish that right-button gesture through the public input
// API after a mode change, without duplicating xterm's normal mouse reports.
export function setupMouseContinuity(term) {
  const element = term.element;
  const screen = element.querySelector(".xterm-screen");
  let held = false,
    recover = false,
    sgr = false,
    pixels = false,
    lastMove;
  const clear = () => {
    held = recover = false;
    lastMove = undefined;
  };
  const modes = (enabled) => (params) => {
    for (const value of params) {
      if (value === 1006) sgr = enabled;
      if (value === 1016) pixels = enabled;
      if (held && !enabled && [9, 1000, 1002, 1003].includes(value))
        recover = true;
    }
    return false; // Let xterm apply the actual mode changes.
  };
  const handlers = [
    term.parser.registerCsiHandler({ prefix: "?", final: "h" }, modes(true)),
    term.parser.registerCsiHandler({ prefix: "?", final: "l" }, modes(false)),
    term.parser.registerEscHandler({ final: "c" }, () => {
      clear();
      sgr = pixels = false;
      return false;
    }),
  ];
  const down = (event) => {
    if (
      event.button === 2 &&
      !event.shiftKey &&
      term.modes.mouseTrackingMode !== "none"
    ) {
      held = true;
      recover = false;
      lastMove = undefined;
    }
  };
  const report = (event, release) => {
    const box = screen.getBoundingClientRect();
    if (!box.width || !box.height) return;
    const col = Math.max(
      1,
      Math.min(
        term.cols,
        Math.floor(((event.clientX - box.left) * term.cols) / box.width) + 1,
      ),
    );
    const row = Math.max(
      1,
      Math.min(
        term.rows,
        Math.floor(((event.clientY - box.top) * term.rows) / box.height) + 1,
      ),
    );
    const button =
      2 |
      (release ? 0 : 32) |
      (event.shiftKey ? 4 : 0) |
      (event.altKey ? 8 : 0) |
      (event.ctrlKey ? 16 : 0);
    const data = `\x1b[<${button};${col};${row}${release ? "m" : "M"}`;
    event.preventDefault();
    event.stopImmediatePropagation();
    if (release || data !== lastMove) term.input(data, true);
    if (!release) lastMove = data;
  };
  const move = (event) => {
    if (
      held &&
      recover &&
      sgr &&
      !pixels &&
      event.buttons & 2 &&
      ["drag", "any"].includes(term.modes.mouseTrackingMode)
    )
      report(event, false);
  };
  const up = (event) => {
    if (event.button !== 2) return;
    if (
      held &&
      recover &&
      sgr &&
      !pixels &&
      ["vt200", "drag", "any"].includes(term.modes.mouseTrackingMode)
    )
      report(event, true);
    clear();
  };
  element.addEventListener("mousedown", down, true);
  document.addEventListener("mousemove", move, true);
  document.addEventListener("mouseup", up, true);
  window.addEventListener("blur", clear);
  term.loadAddon({
    activate() {},
    dispose() {
      clear();
      handlers.forEach((handler) => handler.dispose());
      element.removeEventListener("mousedown", down, true);
      document.removeEventListener("mousemove", move, true);
      document.removeEventListener("mouseup", up, true);
      window.removeEventListener("blur", clear);
    },
  });
}
