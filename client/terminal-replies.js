// Terminal replies: what xterm answers when the far end asks it a question.
// xterm reports them through onData, the same event as typed keys, so a reply
// sent to the wrong place, or too late, is taken as typing. tmux asks every
// attaching client for its device attributes and passes an answer it no longer
// expects to the active pane as keys; on 2026-10-01 that left "?1;2c>0;276;0c"
// in agent inputs (bug wi_03ce50892a559767, docs/claude-wake.md).

const ESC = "\x1b";
const ST = `(?:${ESC}\\\\|\x07)`;
// Each reply is one whole onData chunk. Anything else is input.
const kinds = [
  // DA1 ESC[?1;2c, DA2 ESC[>0;276;0c, DA3 ESC P!|00000000 ESC\
  [
    "attributes",
    new RegExp(`^(?:${ESC}\\[[?>][0-9;]*c|${ESC}P!\\|[0-9A-Fa-f]*${ST})$`),
  ],
  // XTVERSION ESC P>|xterm.js(6.0.0) ESC\
  ["version", new RegExp(`^${ESC}P>\\|[^${ESC}\x07]*${ST}$`)],
  // CPR ESC[12;40R and DECXCPR ESC[?12;40R
  ["position", new RegExp(`^${ESC}\\[\\??[0-9]+;[0-9]+(?:;[0-9]+)?R$`)],
  // DSR ESC[0n
  ["status", new RegExp(`^${ESC}\\[[0-9]n$`)],
  // DECRPM ESC[?2026;2$y, kitty keyboard ESC[?1u, window reports ESC[8;24;80t
  [
    "mode",
    new RegExp(
      `^(?:${ESC}\\[\\??[0-9;]+\\$y|${ESC}\\[\\?[0-9]+u|${ESC}\\[[0-9];[0-9]+;[0-9]+t)$`,
    ),
  ],
  // DECRQSS ESC P1$r0m ESC\
  ["setting", new RegExp(`^${ESC}P[01]\\$r[^${ESC}\x07]*${ST}$`)],
  // OSC colour reports ESC]10;rgb:ffff/ffff/ffff ESC\
  ["colour", new RegExp(`^${ESC}\\](?:4;[0-9]+|1[0-9]);[^${ESC}\x07]*${ST}$`)],
];

// The kind of reply an onData chunk is, or "" for typing, paste and mouse.
export function terminalReplyKind(data) {
  if (typeof data !== "string" || data[0] !== ESC) return "";
  for (const [kind, pattern] of kinds) if (pattern.test(data)) return kind;
  return "";
}

// A gate between one terminal's onData and its connection.
//
// Stale replies: a reply belongs to the connection whose bytes asked for it.
// xterm parses what it is given later, so after a reconnect it can still
// answer the old connection's question. `restart` marks the boundary:
// `mark(parsed)` must queue an empty write behind everything already given to
// the terminal and call `parsed` once the terminal has reached it
// (`(parsed) => term.write("", parsed)`). Until then every reply is dropped,
// and after `dispose` every reply is dropped for good.
//
// tmux attaches: device-attribute and version replies are never sent. tmux
// works without them, and an answer that arrives after it has stopped waiting
// (a stalled link is enough) is typed into the pane.
//
// Typing, paste and mouse reports always pass.
//
// attachTerminalReplies below is the one way a terminal is wired to it.
export function createReplyGate({ tmux = false, mark }) {
  let generation = 0,
    parsed = 0,
    disposed = false;
  return {
    restart() {
      const started = ++generation;
      try {
        mark(() => {
          parsed = Math.max(parsed, started);
        });
      } catch {
        // A terminal that cannot take the marker stays closed to replies.
      }
    },
    dispose() {
      disposed = true;
    },
    // The chunk if it may be sent, otherwise "".
    outgoing(data) {
      const kind = terminalReplyKind(data);
      if (!kind) return data;
      if (disposed || parsed !== generation) return "";
      if (tmux && (kind === "attributes" || kind === "version")) return "";
      return data;
    },
  };
}

// Wires a terminal's onData to its view through a gate, and returns the gate.
// `view.send` is the current connection's input function, or null while there
// is none; the view replaces it on every connection. The view must call
// `restart()` whenever it replaces or drops its connection and `dispose()`
// when it goes away. client/main.js wires every tab this way, and the browser
// test (tests/terminal-replies-browser.mjs) wires its view with this same
// function, so what the test proves is what a tab does.
export function attachTerminalReplies(term, view, { tmux = false } = {}) {
  const gate = createReplyGate({
    tmux,
    mark: (parsed) => term.write("", parsed),
  });
  term.onData((data) => {
    const out = gate.outgoing(data);
    if (out) view.send?.(out);
  });
  return gate;
}
