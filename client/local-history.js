import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { SearchAddon } from "@xterm/addon-search";
import { setupTerminalLinks } from "./terminal-links.js";
import { historyText } from "./history-format.js";
export function setupLocalHistory(
  t,
  { capture, automatic = () => true, notice = () => {} },
) {
  let panel,
    viewer,
    fit,
    search,
    observer,
    cancelReveal,
    pending = false,
    generation = 0,
    queuedLines = 0,
    bottomPush = 0,
    scrollRemainder = 0,
    quietUntil = 0,
    retryAfter = 0,
    reverseIntent = 0,
    warmCapture = null;
  const toggle = document.createElement("button");
  toggle.className = "power-scroll-toggle";
  toggle.type = "button";
  t.el.append(toggle);
  const sync = () => {
    const on = !!panel;
    toggle.textContent = on ? "Scroll on" : "Scroll off";
    toggle.setAttribute("aria-pressed", String(on));
    toggle.setAttribute(
      "aria-label",
      on ? "Turn off power scrolling" : "Turn on power scrolling",
    );
    toggle.title = on
      ? "Power scrolling on · cached history. Click to return to live."
      : automatic()
        ? "Scroll up for local history · scroll down past the bottom to return live. Shift-scroll uses remote scrolling."
        : "Power scrolling off · click to scroll history locally.";
    toggle.disabled = !on && (!t.target || t.status !== "Connected");
  };
  const clear = () => {
    generation++;
    pending = false;
    queuedLines = bottomPush = scrollRemainder = reverseIntent = 0;
    warmCapture = null;
    cancelReveal?.();
    cancelReveal = null;
    observer?.disconnect();
    observer = null;
    viewer?.dispose();
    viewer = fit = search = null;
    panel?.remove();
    panel = null;
    sync();
  };
  const close = () => {
    quietUntil = performance.now() + 180;
    clear();
    if (!t.disposed) t.term.focus();
  };
  function captureText() {
    const now = performance.now();
    if (warmCapture && (warmCapture.pending || now - warmCapture.time < 1500))
      return warmCapture.promise;
    const entry = { pending: true, time: now };
    entry.promise = Promise.resolve()
      .then(() => capture(t))
      .then(
        (text) => {
          entry.pending = false;
          entry.time = performance.now();
          return text;
        },
        (error) => {
          if (warmCapture === entry) warmCapture = null;
          throw error;
        },
      );
    warmCapture = entry;
    return entry.promise;
  }
  // Warm only on pointer intent, not on a polling loop. New terminal output or
  // closing/locking invalidates the snapshot; in-flight reads are shared.
  t.term.onWriteParsed(() => {
    warmCapture = null;
  });
  t.el.addEventListener("pointerenter", () => {
    if (
      !panel &&
      !t.disposed &&
      automatic() &&
      t.tmux &&
      t.target &&
      t.status === "Connected" &&
      performance.now() >= retryAfter
    )
      void captureText().catch(() => {});
  });
  async function open(initialLines = 0) {
    if (panel || !t.tmux || !t.target || t.disposed || t.status !== "Connected")
      return;
    const token = ++generation;
    queuedLines = initialLines;
    const view = (panel = document.createElement("section"));
    view.className = "local-history history-loading";
    view.setAttribute("aria-label", "Power scrolling history");
    view.innerHTML =
      '<div class="history-terminal" aria-label="Cached tmux output"></div><span class="history-status" role="status">Loading history...</span>';
    const output = view.querySelector(".history-terminal"),
      status = view.querySelector("span");
    t.el.append(view);
    const terminal = (viewer = new Terminal({
      ...options(),
      disableStdin: true,
      cursorBlink: false,
      convertEol: true,
      scrollback: 20000,
    }));
    fit = new FitAddon();
    search = new SearchAddon();
    terminal.loadAddon(fit);
    terminal.loadAddon(search);
    terminal.open(output);
    setupTerminalLinks(terminal);
    terminal.write("\x1b[?25l");
    fit.fit();
    observer = new ResizeObserver(() => {
      if (viewer === terminal && output.clientWidth && output.clientHeight)
        fit.fit();
    });
    observer.observe(output);
    view.onkeydown = (e) => {
      if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        close();
      }
    };
    pending = true;
    sync();
    try {
      const text = await captureText();
      if (token !== generation || t.disposed) return;
      await new Promise((resolve) =>
        terminal.write(historyText(text), resolve),
      );
      if (token !== generation || t.disposed) return;
      status.textContent = `History snapshot · ${new Date().toLocaleTimeString()}`;
      terminal.scrollToBottom();
      // Preserve sub-row trackpad movement across the handoff. Rounding up
      // made even a one-pixel first tick jump an entire text row.
      queuedLines = Math.max(0, queuedLines);
      const steps = Math.trunc(queuedLines);
      if (steps) terminal.scrollLines(-steps);
      scrollRemainder =
        -(queuedLines - steps) *
        t.term.options.fontSize *
        t.term.options.lineHeight;
      queuedLines = 0;
      pending = false;
      // write() completing only means the buffer is parsed. Reveal the cached
      // terminal after its positioned rows are painted, not its initial frame.
      await new Promise((resolve) => {
        const subscription = terminal.onRender(() => finish());
        const finish = () => {
          subscription.dispose();
          cancelReveal = null;
          resolve();
        };
        cancelReveal = finish;
        terminal.refresh(0, terminal.rows - 1);
      });
      if (token !== generation || t.disposed) return;
      view.classList.remove("history-loading");
      if (t.el.contains(document.activeElement)) terminal.focus();
    } catch (e) {
      if (token === generation) {
        if (initialLines) {
          clear();
          quietUntil = performance.now() + 180;
          retryAfter = performance.now() + 5000;
          notice("Could not load local history: " + e.message);
        } else status.textContent = e.message;
      }
    } finally {
      if (token === generation) {
        pending = false;
        sync();
      }
    }
  }
  function options() {
    const o = t.term.options;
    return {
      theme: o.theme,
      fontFamily: o.fontFamily,
      fontSize: o.fontSize,
      fontWeight: o.fontWeight,
      fontWeightBold: o.fontWeightBold,
      lineHeight: o.lineHeight,
      letterSpacing: o.letterSpacing,
      minimumContrastRatio: o.minimumContrastRatio,
    };
  }
  function update() {
    if (!viewer) return;
    Object.assign(viewer.options, options());
    fit.fit();
    const current = viewer;
    document.fonts.ready.then(() => {
      if (viewer === current) fit.fit();
    });
  }
  t.el.addEventListener(
    "wheel",
    (e) => {
      if (
        t.disposed ||
        e.ctrlKey ||
        e.metaKey ||
        e.altKey ||
        e.shiftKey ||
        !e.deltaY ||
        Math.abs(e.deltaY) < Math.abs(e.deltaX) * 0.35 ||
        e.target.closest?.("button, a, input, select, .terminal-link-hint") ||
        document.querySelector("dialog[open]")
      )
        return;
      const now = performance.now();
      const consume = () => {
        e.preventDefault();
        e.stopImmediatePropagation();
      };
      const rowHeight = t.term.options.fontSize * t.term.options.lineHeight;
      const pixels =
        e.deltaY *
        (e.deltaMode === 1
          ? rowHeight
          : e.deltaMode === 2
            ? t.el.clientHeight
            : 1);
      const lines = Math.abs(pixels) / rowHeight;
      // Suppress downward inertia after returning live, but let a deliberate
      // upward reversal start immediately instead of extending a blanket delay.
      if (now < quietUntil) {
        reverseIntent = pixels < 0 ? reverseIntent - pixels : 0;
        if (reverseIntent < 4) {
          consume();
          return;
        }
        quietUntil = 0;
        reverseIntent = 0;
      }

      if (!panel) {
        if (
          !automatic() ||
          pixels >= 0 ||
          now < retryAfter ||
          !t.tmux ||
          !t.target ||
          t.status !== "Connected"
        )
          return;
        consume();
        void open(Math.min(lines, 200));
        return;
      }
      if (pending) {
        consume();
        queuedLines = Math.min(
          200,
          queuedLines + (pixels < 0 ? lines : -lines),
        );
        if (queuedLines * rowHeight <= -32) close();
        return;
      }
      if (!automatic() || !viewer) return;
      consume();
      const buffer = viewer.buffer.active;
      if (pixels > 0 && buffer.viewportY >= buffer.baseY) {
        scrollRemainder = 0;
        bottomPush += pixels;
        if (bottomPush >= 32) close();
      } else {
        bottomPush = 0;
        scrollRemainder += pixels;
        const steps = Math.trunc(scrollRemainder / rowHeight);
        if (steps) {
          scrollRemainder -= steps * rowHeight;
          viewer.scrollLines(steps);
        }
      }
    },
    { capture: true, passive: false },
  );
  // xterm must handle its wheel event before we cancel the browser's fallback
  // scrolling. Cancelling in capture would disable xterm's manual scrollback.
  t.el.addEventListener(
    "wheel",
    (e) => {
      if (!t.disposed && !e.ctrlKey && !e.metaKey) e.preventDefault();
    },
    { passive: false },
  );
  toggle.onclick = () => (panel ? close() : void open());
  sync();
  return {
    open,
    clear,
    close,
    sync,
    update,
    getSelection: () => viewer?.getSelection() || "",
    findNext: (text) => search?.findNext(text),
    isOpen: () => !!panel,
    focus: () => (pending ? toggle.focus() : viewer?.focus()),
  };
}
