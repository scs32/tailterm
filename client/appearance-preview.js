import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { terminalAppearance } from "./appearance.js";

// The preview uses the same renderer, colors and fonts as connected terminals.
export function createAppearancePreview(element, appearance) {
  const term = new Terminal({
    ...terminalAppearance(appearance),
    rows: 7,
    cols: 58,
    disableStdin: true,
    cursorBlink: false,
    scrollback: 0,
  });
  const fit = new FitAddon();
  term.loadAddon(fit);
  term.open(element);
  let disposed = false;
  const resize = () => {
    if (!disposed && element.isConnected && element.clientWidth) fit.fit();
  };
  const observer = new ResizeObserver(resize);
  observer.observe(element);
  term.write(
    [
      "\x1b[?25l\x1b[1m~/workspace\x1b[0m  main  0O 1il {} []",
      "\x1b[32m✓ Connected\x1b[0m  \x1b[33m! Warning\x1b[0m  \x1b[31m× Error\x1b[0m",
      "\x1b[2mMuted output · ready for the next command\x1b[0m",
      "\x1b[48;2;48;48;48m\x1b[38;2;87;82;121m❯ Dark prompt with explicit app colors\x1b[K\x1b[0m",
      "\x1b[48;2;250;244;237m\x1b[38;2;152;147;165m❯ Light prompt with explicit app colors\x1b[K\x1b[0m",
      "Symbols: \uf07b ~/src  \ue0a0 main  \uf120 ssh  \ue0b0 \ue0b2",
      "\x1b[7m [workspace]  1:shell* │ 2:logs │ connected \x1b[0m",
    ].join("\r\n"),
  );
  const update = (value) => {
    Object.assign(term.options, terminalAppearance(value), {
      cursorBlink: false,
    });
    resize();
    document.fonts.ready.then(resize);
  };
  update(appearance);
  return {
    update,
    dispose() {
      disposed = true;
      observer.disconnect();
      term.dispose();
    },
  };
}
