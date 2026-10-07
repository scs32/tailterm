// Terminal replies must not reach an agent pane as typed input (bug
// wi_03ce50892a559767, s6 and s8-s10; docs/claude-wake.md).
//
// tmux asks a client's terminal for its device attributes on every attach. It
// takes the first answers as answers and passes any later one to the active
// pane as keys. Two routes produce such an answer from a TailOS terminal view:
//
//   late   the link stalls while the answers are in flight, and they arrive
//          after tmux has stopped waiting (about five seconds on tmux 3.7b);
//   stale  the view reconnects, and the answer to the old attach's question is
//          parsed late and sent on the new connection, so the new attach gets
//          two sets. This needs a page whose timers run late, as in a
//          background tab; the test clamps setTimeout to stand in for that.
//
// Everything here is private to the run: a tmux server on its own socket with
// `-f /dev/null`, panes running `cat -v`, an SSH server on a loopback port and
// a PTY per attach. No live session, pane or hub is touched.
//
// The view below is a tab reduced to its terminal: one xterm for the life of
// the view, a connection replaced on restart, and incoming bytes written only
// while the connection is current, as in client/main.js. Its onData is wired
// by attachTerminalReplies from client/terminal-replies.js, the same call
// client/main.js makes for every tab (tests/terminal-replies.test.js checks
// that call and its restart and dispose sites). On a checkout without that
// module the view sends every onData chunk, which is the wiring of
// client/main.js at 4c9b1d7 (line 2088), and the checks below fail.
import { useDomRenderer } from "./dom-renderer.mjs";
import { once } from "node:events";
import { createServer } from "vite";
import { chromium, webkit } from "@playwright/test";
import { WebSocketServer } from "ws";
import ssh2 from "ssh2";
import { spawn, execFileSync } from "node:child_process";
import { generateKeyPairSync, randomUUID } from "node:crypto";
import { rmSync } from "node:fs";
import assert from "node:assert/strict";

const engines = { chromium, webkit };
// Unset or "both" runs both engines, as the verification matrix runs a
// mode "both" entry; "chromium" or "webkit" runs that one alone.
const requested = process.env.TEST_BROWSER || "both";
if (requested !== "both" && !Object.hasOwn(engines, requested))
  throw new Error("Unknown TEST_BROWSER");
const selected = requested === "both" ? ["chromium", "webkit"] : [requested];

const socket = "tailterm-replies-test-" + randomUUID();
const tmuxEnv = { ...process.env, TERM: "xterm-256color" };
delete tmuxEnv.TMUX;
const tmux = (...args) =>
  execFileSync("tmux", ["-L", socket, "-f", "/dev/null", ...args], {
    encoding: "utf8",
    env: tmuxEnv,
    stdio: ["ignore", "pipe", "pipe"],
  });
const socketPath = () =>
  `${process.env.TMUX_TMPDIR || "/tmp"}/tmux-${process.getuid()}/${socket}`;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const until = async (check, what, ms = 8000) => {
  for (const end = Date.now() + ms; Date.now() < end; await sleep(25))
    if (await check()) return;
  throw new Error("Timed out waiting for " + what);
};

// The commands a fixture SSH session may run, by the name the page asks for.
const commands = {
  "attach:A": `exec tmux -L ${socket} -f /dev/null attach-session -t A`,
  "attach:B": `exec tmux -L ${socket} -f /dev/null attach-session -t B`,
  // A program that asks the terminal for the cursor position, with no tmux.
  plain: `printf '\\033[6n'; exec cat -v`,
};
const pane = (name) =>
  tmux("capture-pane", "-p", "-t", name).replace(/\s/g, "");
const clients = (name) =>
  tmux("list-clients", "-t", name, "-F", "#{client_name}")
    .trim()
    .split("\n")
    .filter(Boolean).length;
const resetPanes = () => {
  for (const name of ["A", "B"]) {
    tmux("respawn-pane", "-k", "-t", name, "cat -v");
    tmux("clear-history", "-t", name);
  }
};

const ptyBridge = `
import os,pty,select,sys,fcntl,termios,struct
pid,fd=pty.fork()
if pid == 0:
 fcntl.ioctl(0,termios.TIOCSWINSZ,struct.pack('HHHH',24,80,0,0))
 os.environ['TERM']='xterm-256color'
 os.environ.pop('TMUX',None)
 os.execvp('/bin/sh',['/bin/sh','-c',sys.argv[1]])
while True:
 ready,_,_=select.select([fd,0],[],[])
 for source in ready:
  try: data=os.read(source,65536)
  except OSError: sys.exit(0)
  if not data: sys.exit(0)
  os.write(1 if source==fd else fd,data)
`;
const children = new Set();
const ssh = new ssh2.Server(
  {
    hostKeys: [
      generateKeyPairSync("rsa", {
        modulusLength: 2048,
        privateKeyEncoding: { type: "pkcs1", format: "pem" },
        publicKeyEncoding: { type: "spki", format: "pem" },
      }).privateKey,
    ],
  },
  (client) => {
    client.on("error", () => {});
    client.on("authentication", (ctx) => ctx.accept());
    client.on("ready", () =>
      client.on("session", (accept) => {
        const session = accept();
        session.on("pty", (accept) => accept());
        session.on("exec", (accept, reject, info) => {
          if (!Object.values(commands).includes(info.command)) return reject();
          const stream = accept();
          const child = spawn("python3", ["-u", "-c", ptyBridge, info.command]);
          children.add(child);
          child.stdout.on(
            "data",
            (data) => stream.writable && stream.write(data),
          );
          child.stderr.on("data", (data) => process.stderr.write(data));
          stream.on(
            "data",
            (data) => child.stdin.writable && child.stdin.write(data),
          );
          stream.on("close", () => child.kill());
          child.on("exit", () => {
            children.delete(child);
            stream.exit(0);
            stream.end();
          });
        });
      }),
    );
  },
);
ssh.listen(0, "127.0.0.1");
await once(ssh, "listening");

// One WebSocket is one SSH connection to the fixture. `plans` shapes the next
// connections in order: cut it right after a question has been delivered to
// the page, or hold what the page sends for a while (a stalled link).
const plans = [];
const connections = [];
const wss = new WebSocketServer({ host: "127.0.0.1", port: 0 });
await once(wss, "listening");
wss.on("connection", (ws, request) => {
  const name = new URL(request.url, "http://fixture").searchParams.get("name");
  const plan = plans.shift() || {};
  const record = { name, up: Buffer.alloc(0), down: Buffer.alloc(0), plan };
  connections.push(record);
  const client = new ssh2.Client();
  const opened = Date.now();
  let stream;
  const waiting = [];
  const forward = (data) => {
    if (stream) stream.write(data);
    else waiting.push(data);
  };
  ws.on("message", (data) => {
    record.up = Buffer.concat([record.up, data]);
    const held = (plan.stallMs || 0) - (Date.now() - opened);
    if (held > 0) setTimeout(() => forward(data), held);
    else forward(data);
  });
  ws.on("close", () => client.end());
  client.on("error", () => ws.close());
  client.on("ready", () =>
    client.exec(
      commands[name] || "exit 1",
      { pty: { rows: 24, cols: 80, term: "xterm-256color" } },
      (error, accepted) => {
        if (error) return ws.close();
        stream = accepted;
        for (const data of waiting.splice(0)) stream.write(data);
        stream.on("data", (data) => {
          record.down = Buffer.concat([record.down, data]);
          if (ws.readyState === 1) ws.send(data);
          if (plan.cutAfter && record.down.includes(plan.cutAfter)) {
            plan.cutAfter = null;
            record.cut = true;
            ws.close();
            client.end();
          }
        });
        stream.on("close", () => ws.close());
      },
    ),
  );
  client.connect({
    host: "127.0.0.1",
    port: ssh.address().port,
    username: "fixture",
    password: "fixture",
  });
});

const server = await createServer({
  configFile: false,
  logLevel: "silent",
  server: { host: "127.0.0.1", port: 0, hmr: false },
});
await server.listen();

const DA1_QUERY = "\x1b[c";
const CPR_QUERY = "\x1b[6n";
// What a terminal answers: DA1 ESC[?..c, DA2 ESC[>..c, XTVERSION ESC P>|..ESC\.
const attributeReplies = (bytes) =>
  bytes
    .toString("latin1")
    .match(/\x1b\[[?>][0-9;]*c|\x1bP>\|[^\x1b]*\x1b\\/g) || [];
const positionReplies = (bytes) =>
  bytes.toString("latin1").match(/\x1b\[[0-9]+;[0-9]+R/g) || [];

let failed = false;
try {
  tmux("new-session", "-d", "-s", "A", "-x", "80", "-y", "24", "cat -v");
  tmux("new-session", "-d", "-s", "B", "-x", "80", "-y", "24", "cat -v");
  for (const name of selected) {
    const browser = await engines[name].launch(
      name === "chromium"
        ? { args: ["--disable-features=LocalNetworkAccessChecks"] }
        : {},
    );
    try {
      const page = await browser.newPage({
        viewport: { width: 1200, height: 850 },
      });
      await useDomRenderer(page);
      page.setDefaultTimeout(10000);
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      await page.route("**/replies-test", (route) =>
        route.fulfill({
          contentType: "text/html",
          body: "<!doctype html><html><body></body></html>",
        }),
      );
      await page.goto(
        `http://127.0.0.1:${server.httpServer.address().port}/replies-test`,
      );
      const gated = await page.evaluate(async (socketURL) => {
        // A background tab runs its timers late. `timerClamp` is the least
        // delay any timer gets while a check simulates that.
        const realTimeout = window.setTimeout.bind(window);
        window.timerClamp = 0;
        window.setTimeout = (fn, ms, ...rest) =>
          realTimeout(fn, Math.max(ms || 0, window.timerClamp), ...rest);
        const { Terminal } =
          await import("/node_modules/@xterm/xterm/lib/xterm.mjs");
        await import("/node_modules/@xterm/xterm/css/xterm.css");
        const replies = await import("/client/terminal-replies.js").catch(
          () => null,
        );
        window.views = {};
        window.createView = (id, tmux) => {
          const el = document.createElement("div");
          document.body.append(el);
          const term = new Terminal({ cols: 80, rows: 24, fontSize: 12 });
          term.open(el);
          const view = { term, el, send: null, generation: 0, disposed: false };
          // The one wiring call a tab makes; without the module, the base's.
          const gate = replies
            ? replies.attachTerminalReplies(term, view, { tmux })
            : (term.onData((data) => view.send?.(data)), null);
          view.restart = (name) => {
            if (view.disposed) return;
            const generation = ++view.generation;
            view.ws?.close();
            view.send = null;
            gate?.restart();
            const live = () => !view.disposed && generation === view.generation;
            const ws = new WebSocket(
              socketURL + "/?name=" + encodeURIComponent(name),
            );
            ws.binaryType = "arraybuffer";
            view.ws = ws;
            ws.onopen = () => {
              if (live()) view.send = (data) => ws.send(data);
            };
            ws.onmessage = (e) => {
              if (!live()) return;
              term.write(new Uint8Array(e.data));
              const first = view.onFirstData;
              view.onFirstData = null;
              first?.();
            };
            ws.onclose = () => {
              if (!live()) return;
              view.send = null;
              const closed = view.onClosed;
              view.onClosed = null;
              closed?.();
            };
          };
          view.dispose = () => {
            view.disposed = true;
            gate?.dispose();
            view.ws?.close();
            term.dispose();
            el.remove();
          };
          views[id] = view;
        };
        return !!replies;
      }, `ws://127.0.0.1:${wss.address().port}`);
      const failures = [];
      const check = (ok, message) => {
        console.log(`${name}: ${ok ? "ok  " : "FAIL"} ${message}`);
        if (!ok) failures.push(message);
      };
      if (!gated)
        console.log(
          name +
            ": client/terminal-replies.js is absent; the view sends every onData chunk, as client/main.js does at 4c9b1d7.",
        );

      // c1, late route: the link holds what the page sends for six seconds
      // during the attach handshake, then recovers.
      resetPanes();
      await page.evaluate(() => createView("late", true));
      plans.push({ stallMs: 6000 });
      let from = connections.length;
      await page.evaluate(() => views.late.restart("attach:A"));
      await until(() => clients("A") === 1, "the stalled attach");
      await sleep(7200);
      check(
        pane("A") === "",
        `c1 late: a six-second stall during the attach handshake leaves pane input ${JSON.stringify(pane("A"))}`,
      );
      await page.evaluate(() => views.late.dispose());
      await until(() => clients("A") === 0, "the stalled view to detach");

      // c1, stale route: attach, cut mid-handshake, reconnect, five times,
      // with the page's timers running late.
      resetPanes();
      await page.evaluate(() => {
        createView("flaky", true);
        window.timerClamp = 400;
      });
      for (let cycle = 1; cycle <= 5; cycle++) {
        from = connections.length;
        plans.push({ cutAfter: DA1_QUERY }, {});
        await page.evaluate(() => {
          views.flaky.onClosed = () => views.flaky.restart("attach:A");
          views.flaky.restart("attach:A");
        });
        await until(
          () => connections.length === from + 2 && clients("A") === 1,
          `reconnect ${cycle}`,
        );
        assert.ok(
          connections[from].cut,
          "The first attach was cut mid-handshake",
        );
        await sleep(1500);
        console.log(
          `${name}:      cycle ${cycle}: pane input ${JSON.stringify(pane("A"))}`,
        );
      }
      check(
        pane("A") === "",
        `c1 stale: attach, cut mid-handshake and reconnect five times leaves pane input ${JSON.stringify(pane("A"))}`,
      );

      // c2: switch a view to another pane mid-handshake, then dispose a view
      // mid-handshake. Nothing may reach either pane.
      await page.evaluate(() => views.flaky.dispose());
      await until(() => clients("A") === 0, "the flaky view to detach");
      resetPanes();
      await page.evaluate(() => createView("switching", true));
      from = connections.length;
      await page.evaluate(() => {
        views.switching.onFirstData = () => views.switching.restart("attach:B");
        views.switching.restart("attach:A");
      });
      await until(
        () => connections.length === from + 2 && clients("B") === 1,
        "the switch to pane B",
      );
      await sleep(1500);
      check(
        pane("A") === "" && pane("B") === "",
        `c2 switch: switching panes mid-handshake leaves pane inputs ${JSON.stringify([pane("A"), pane("B")])}`,
      );
      resetPanes();
      await page.evaluate(() => {
        createView("closing", true);
        views.closing.onFirstData = () => views.closing.dispose();
        views.closing.restart("attach:A");
      });
      await until(
        () =>
          connections.length === from + 3 &&
          connections[from + 2].down.length > 0,
        "the view to be disposed",
      );
      await sleep(1500);
      check(
        pane("A") === "" && pane("B") === "",
        `c2 dispose: disposing a view mid-handshake leaves pane inputs ${JSON.stringify([pane("A"), pane("B")])}`,
      );
      await page.evaluate(() => {
        views.switching.dispose();
        window.timerClamp = 0;
      });
      await until(
        () => clients("A") + clients("B") === 0,
        "the views to detach",
      );

      // c4: on a tmux attach the tmux client is sent no device-attribute or
      // version reply, and typing reaches the pane well inside tmux's wait.
      resetPanes();
      await page.evaluate(() => createView("typing", true));
      from = connections.length;
      const attachedAt = Date.now();
      await page.evaluate(() => views.typing.restart("attach:A"));
      await until(() => clients("A") === 1, "the typing attach");
      await page.locator(".xterm-helper-textarea").last().focus();
      await page.keyboard.type("ok");
      await until(
        () => pane("A").startsWith("ok"),
        "typed text in the pane",
        2500,
      );
      const typedAfter = Date.now() - attachedAt;
      await sleep(1000);
      const sentReplies = attributeReplies(connections[from].up);
      check(
        sentReplies.length === 0 && pane("A") === "ok" && typedAfter < 3000,
        `c4: the tmux client received ${sentReplies.length} device-attribute or version replies; typed text reached the pane after ${typedAfter} ms as ${JSON.stringify(pane("A"))}`,
      );
      await page.evaluate(() => views.typing.dispose());

      // The stale rule alone, without tmux: a program asks for the cursor
      // position, the connection is cut and replaced. The new program must get
      // one answer, to its own question.
      await page.evaluate(() => {
        createView("plain", false);
        window.timerClamp = 400;
      });
      from = connections.length;
      plans.push({ cutAfter: CPR_QUERY }, {});
      await page.evaluate(() => {
        views.plain.onClosed = () => views.plain.restart("plain");
        views.plain.restart("plain");
      });
      await until(() => connections.length === from + 2, "the plain reconnect");
      await sleep(1800);
      const positions = positionReplies(connections[from + 1].up);
      check(
        positions.length === 1,
        `stale rule: after a cut and reconnect the new program received ${positions.length} cursor-position replies (1 is its own)`,
      );
      await page.evaluate(() => {
        views.plain.dispose();
        window.timerClamp = 0;
      });
      assert.deepEqual(errors, [], "The page raised no errors");
      if (failures.length) {
        failed = true;
        console.log(
          `${name}: ${failures.length} terminal-reply check(s) failed.`,
        );
      } else
        console.log(
          name +
            ": terminal replies never reached a pane as input, and typing and current replies still pass.",
        );
    } finally {
      await browser.close();
    }
  }
} finally {
  for (const ws of wss.clients) ws.terminate();
  for (const child of children) child.kill();
  try {
    tmux("kill-server");
  } catch {}
  rmSync(socketPath(), { force: true });
  wss.close();
  ssh.close();
  await server.close();
}
let left = true;
try {
  tmux("list-sessions");
} catch {
  left = false;
}
assert.equal(left, false, "The private tmux server is gone");
if (failed) {
  console.error("Terminal replies reached a pane as typed input.");
  process.exit(1);
}
console.log("No tmux server is left on the private socket.");
