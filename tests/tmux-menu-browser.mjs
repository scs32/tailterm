import { useDomRenderer } from "./dom-renderer.mjs";
import { once } from "node:events";
import { createServer } from "vite";
import { chromium, webkit } from "@playwright/test";
import { WebSocketServer } from "ws";
import { spawn, execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import assert from "node:assert/strict";
const socket = "tailterm-menu-test-" + randomUUID();
const server = await createServer({
  configFile: false,
  server: { host: "127.0.0.1", port: 0 },
});
await server.listen();
const wss = new WebSocketServer({ host: "127.0.0.1", port: 0 });
await once(wss, "listening");
const children = new Set();
wss.on("connection", (ws) => {
  const child = spawn(
    "python3",
    [
      "-u",
      "-c",
      `
import os,pty,select,sys,fcntl,termios,struct
pid,fd=pty.fork()
if pid == 0:
 fcntl.ioctl(0,termios.TIOCSWINSZ,struct.pack('HHHH',24,80,0,0))
 os.environ['TERM']='xterm-256color'
 os.execvp('tmux',['tmux','-L',sys.argv[1],'-f','/dev/null','new-session','-A','-s','fixture','/bin/sh',';','set-option','-g','mouse','on'])
while True:
 ready,_,_=select.select([fd,0],[],[])
 for source in ready:
  try: data=os.read(source,65536)
  except OSError: sys.exit(0)
  if not data: sys.exit(0)
  os.write(1 if source==fd else fd,data)
`,
      socket,
    ],
    { env: { ...process.env, TMUX: "" } },
  );
  children.add(child);
  child.stdout.on("data", (data) => {
    if (ws.readyState === 1) ws.send(data);
  });
  child.stderr.on("data", (data) => process.stderr.write(data));
  ws.on("message", (data) => child.stdin.write(data));
  ws.on("close", () => child.kill());
  child.on("exit", () => {
    children.delete(child);
    ws.close();
  });
});
try {
  for (const engine of [chromium, webkit]) {
    const browser = await engine.launch(
      engine === chromium
        ? { args: ["--disable-features=LocalNetworkAccessChecks"] }
        : {},
    );
    try {
      const page = await browser.newPage({
        viewport: { width: 1200, height: 850 },
      });
      await useDomRenderer(page);
      page.setDefaultTimeout(10000);
      page.on("pageerror", (e) => console.error(e.message));
      await page.route("**/menu-test", (r) =>
        r.fulfill({
          contentType: "text/html",
          body: '<!doctype html><html><body><div id="terminal"></div></body></html>',
        }),
      );
      await page.goto(
        `http://127.0.0.1:${server.httpServer.address().port}/menu-test`,
      );
      await page.evaluate(async (socketURL) => {
        const { Terminal } =
          await import("/node_modules/@xterm/xterm/lib/xterm.mjs");
        await import("/node_modules/@xterm/xterm/css/xterm.css");
        const { setupTerminalContextMenu } =
          await import("/client/terminal-input.js");
        window.term = new Terminal({ cols: 80, rows: 24, fontSize: 16 });
        term.open(document.querySelector("#terminal"));
        setupTerminalContextMenu(term);
        const ws = new WebSocket(socketURL);
        ws.onerror = () => console.log("PTY websocket failed", socketURL);
        ws.binaryType = "arraybuffer";
        window.sent = [];
        term.onData((data) => {
          sent.push(data);
          ws.send(data);
        });
        ws.onmessage = (e) => term.write(new Uint8Array(e.data));
        window.readScreen = () =>
          Array.from(
            { length: 24 },
            (_, i) =>
              term.buffer.active
                .getLine(term.buffer.active.viewportY + i)
                ?.translateToString(true) || "",
          );
      }, `ws://127.0.0.1:${wss.address().port}`);
      await page
        .waitForFunction(() => term.modes.mouseTrackingMode !== "none")
        .catch(async (e) => {
          console.error(await page.evaluate(() => readScreen()));
          throw e;
        });
      const point = async (col, row) =>
        page.locator(".xterm-screen").evaluate(
          (el, [col, row]) => {
            const b = el.getBoundingClientRect();
            return {
              x: b.x + ((col + 0.5) * b.width) / 80,
              y: b.y + ((row + 0.5) * b.height) / 24,
            };
          },
          [col, row],
        );
      const start = await point(20, 6);
      await page.mouse.move(start.x, start.y);
      await page.mouse.down({ button: "right" });
      await page
        .waitForFunction(() =>
          readScreen().some((line) => line.includes("Horizontal Split")),
        )
        .catch(async (e) => {
          console.log(
            await page.evaluate(() => ({ screen: readScreen(), sent })),
          );
          throw e;
        });
      const target = await page.evaluate(() => {
        const lines = readScreen(),
          row = lines.findIndex((line) => line.includes("Horizontal Split"));
        return { row, col: lines[row].indexOf("Horizontal Split") + 4 };
      });
      const end = await point(target.col, target.row);
      await page.mouse.move(end.x, end.y, { steps: 12 });
      await page.waitForTimeout(150);
      await page.mouse.up({ button: "right" });
      await page
        .waitForFunction(
          () => !readScreen().some((line) => line.includes("Horizontal Split")),
        )
        .catch(async (e) => {
          console.log(
            await page.evaluate(() => ({
              screen: readScreen(),
              sent,
              mode: term.modes.mouseTrackingMode,
            })),
          );
          throw e;
        });
      const panes = execFileSync(
        "tmux",
        ["-L", socket, "list-panes", "-t", "fixture", "-F", "#{pane_id}"],
        { encoding: "utf8" },
      )
        .trim()
        .split("\n");
      assert.equal(panes.length, 2, "Menu selection actually creates a split");
      console.log(
        engine.name() +
          ": real tmux right-button hold, move, release successfully selects Horizontal Split.",
      );
      execFileSync("tmux", ["-L", socket, "kill-pane", "-t", panes[1]]);
    } finally {
      await browser.close();
    }
  }
} finally {
  for (const ws of wss.clients) ws.terminate();
  for (const child of children) child.kill();
  try {
    execFileSync("tmux", ["-L", socket, "kill-server"]);
  } catch {}
  wss.close();
  await server.close();
}
