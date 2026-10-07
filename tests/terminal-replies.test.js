import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import {
  attachTerminalReplies,
  createReplyGate,
  terminalReplyKind,
} from "../client/terminal-replies.js";

const ESC = "\x1b";
const replies = {
  DA1: `${ESC}[?1;2c`,
  DA2: `${ESC}[>0;276;0c`,
  XTVERSION: `${ESC}P>|xterm.js(6.0.0)${ESC}\\`,
  CPR: `${ESC}[12;40R`,
  DECRPM: `${ESC}[?2026;2$y`,
};
const input = {
  keystroke: "a",
  enter: "\r",
  escape: ESC,
  arrow: `${ESC}[A`,
  "application arrow": `${ESC}OA`,
  "function key": `${ESC}[15~`,
  paste: `${ESC}[200~tt inbox --unread${ESC}[201~`,
  "pasted reply text": "?1;2c>0;276;0c",
  "mouse press": `${ESC}[<0;10;5M`,
  "mouse release": `${ESC}[<0;10;5m`,
  "legacy mouse": `${ESC}[M !!`,
  "focus in": `${ESC}[I`,
};

// A terminal that parses what it is given only when the test says so.
function fakeTerminal() {
  const queue = [];
  return {
    mark: (parsed) => queue.push(parsed),
    parse() {
      for (const parsed of queue.splice(0)) parsed();
    },
  };
}

test("replies are recognized and input is not", () => {
  assert.equal(terminalReplyKind(replies.DA1), "attributes");
  assert.equal(terminalReplyKind(replies.DA2), "attributes");
  assert.equal(terminalReplyKind(replies.XTVERSION), "version");
  assert.equal(terminalReplyKind(replies.CPR), "position");
  assert.equal(terminalReplyKind(replies.DECRPM), "mode");
  assert.equal(terminalReplyKind(`${ESC}[?1u`), "mode");
  assert.equal(terminalReplyKind(`${ESC}[0n`), "status");
  assert.equal(terminalReplyKind(`${ESC}P1$r0m${ESC}\\`), "setting");
  assert.equal(
    terminalReplyKind(`${ESC}]11;rgb:0000/0000/0000${ESC}\\`),
    "colour",
  );
  for (const [name, data] of Object.entries(input))
    assert.equal(terminalReplyKind(data), "", name);
  // Two replies in one chunk are not one reply; xterm sends each alone.
  assert.equal(terminalReplyKind(replies.DA1 + "x"), "");
  assert.equal(terminalReplyKind(undefined), "");
});

test("a reply from an old generation is dropped and input always passes", () => {
  for (const tmux of [false, true]) {
    const term = fakeTerminal();
    const gate = createReplyGate({ tmux, mark: term.mark });
    gate.restart();
    term.parse();
    gate.restart(); // reconnect: the old connection's bytes are not parsed yet
    for (const [name, data] of Object.entries(replies))
      assert.equal(gate.outgoing(data), "", `stale ${name}, tmux ${tmux}`);
    for (const [name, data] of Object.entries(input))
      assert.equal(gate.outgoing(data), data, `${name} while stale`);
    term.parse();
    assert.equal(gate.outgoing(replies.CPR), replies.CPR, "current CPR");
    assert.equal(gate.outgoing(replies.DECRPM), replies.DECRPM);
  }
});

test("a marker from an earlier restart does not open a later one", () => {
  const queue = [];
  const gate = createReplyGate({ mark: (parsed) => queue.push(parsed) });
  gate.restart();
  gate.restart();
  queue[0](); // only the first connection's marker has been reached
  assert.equal(gate.outgoing(replies.CPR), "");
  queue[1]();
  assert.equal(gate.outgoing(replies.CPR), replies.CPR);
});

test("a tmux attach is never sent device-attribute or version replies", () => {
  const term = fakeTerminal();
  const gate = createReplyGate({ tmux: true, mark: term.mark });
  gate.restart();
  term.parse();
  for (const name of ["DA1", "DA2", "XTVERSION"])
    assert.equal(gate.outgoing(replies[name]), "", name);
  assert.equal(gate.outgoing(`${ESC}P!|00000000${ESC}\\`), "", "DA3");
  assert.equal(gate.outgoing(replies.CPR), replies.CPR);
  assert.equal(gate.outgoing(replies.DECRPM), replies.DECRPM);
  for (const [name, data] of Object.entries(input))
    assert.equal(gate.outgoing(data), data, name);
  // Without tmux a current reply of any kind is sent.
  const plain = createReplyGate({ mark: term.mark });
  for (const [name, data] of Object.entries(replies))
    assert.equal(plain.outgoing(data), data, name);
});

test("nothing is answered after dispose, or when the marker cannot be queued", () => {
  const term = fakeTerminal();
  const gate = createReplyGate({ mark: term.mark });
  gate.dispose();
  for (const [name, data] of Object.entries(replies))
    assert.equal(gate.outgoing(data), "", name);
  term.parse();
  assert.equal(gate.outgoing(replies.CPR), "");
  const broken = createReplyGate({
    mark: () => {
      throw new Error("write data discarded");
    },
  });
  broken.restart();
  assert.equal(broken.outgoing(replies.CPR), "");
  assert.equal(broken.outgoing("a"), "a");
});

// A terminal as attachTerminalReplies uses it: onData, and writes whose
// callbacks run when the test lets the terminal parse.
function fakeXterm() {
  const writes = [];
  const term = {
    onData: (handler) => (term.emit = handler),
    write: (data, parsed) => writes.push(parsed),
    parse() {
      for (const parsed of writes.splice(0)) parsed?.();
    },
  };
  return term;
}

test("the attach function wires onData to the view's current connection", () => {
  const term = fakeXterm();
  const view = { send: null };
  const gate = attachTerminalReplies(term, view, { tmux: true });
  term.emit("a"); // no connection: nothing to send to, and no error
  const first = [];
  gate.restart();
  view.send = (data) => first.push(data);
  term.parse();
  for (const data of ["l", "s", replies.DA1, replies.DA2, replies.XTVERSION])
    term.emit(data);
  term.emit(replies.CPR);
  assert.deepEqual(first, ["l", "s", replies.CPR]);
  // Reconnect: the old connection's question is answered late.
  const second = [];
  view.send = null;
  gate.restart();
  view.send = (data) => second.push(data);
  term.emit(replies.CPR);
  term.emit("x");
  term.parse();
  term.emit(replies.CPR);
  assert.deepEqual(second, ["x", replies.CPR]);
  gate.dispose();
  term.emit(replies.CPR);
  assert.deepEqual(second, ["x", replies.CPR]);
  // A plain shell tab still answers device attributes on its own connection.
  const plainTerm = fakeXterm();
  const plain = { send: null };
  const sent = [];
  attachTerminalReplies(plainTerm, plain);
  plain.send = (data) => sent.push(data);
  plainTerm.emit(replies.DA1);
  assert.deepEqual(sent, [replies.DA1]);
});

test("every tab is wired by the attach function the browser test uses", () => {
  const read = (name) => readFileSync(new URL(name, import.meta.url), "utf8");
  const main = read("../client/main.js");
  // The only onData handler is the one attachTerminalReplies installs.
  assert.equal(main.match(/\.onData\(/g), null);
  assert.deepEqual(main.match(/attachTerminalReplies\([^\n]*/g), [
    "attachTerminalReplies(term, t, { tmux: !!tmux });",
  ]);
  assert.match(
    main,
    /t\.replies = attachTerminalReplies\(term, t, \{ tmux: !!tmux \}\);/,
  );
  // Each way a connection is replaced marks the boundary, and dispose closes it.
  assert.equal(main.match(/t\.replies\.restart\(\);/g)?.length, 2);
  assert.match(
    main,
    /t\.connection\?\.close\(\);\s*t\.send = null;\s*t\.replies\.restart\(\);/,
  );
  assert.match(
    main,
    /t\.transport = "ssh";\s*t\.send = null;\s*t\.replies\.restart\(\);/,
  );
  assert.match(main, /t\.replies\?\.dispose\(\);\s*t\.close\?\.\(\);/);
  // The browser test wires its view with that same function.
  const browser = read("./terminal-replies-browser.mjs");
  assert.match(browser, /import\("\/client\/terminal-replies\.js"\)/);
  assert.match(
    browser,
    /replies\.attachTerminalReplies\(term, view, \{ tmux \}\)/,
  );
  assert.equal(browser.match(/createReplyGate/g), null);
});
