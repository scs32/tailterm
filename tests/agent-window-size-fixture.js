// Imported by the mandatory static-browser matrix entry; no browser launcher or
// optional switch. Real SSH exec + PTY + private tmux + SIGWINCH program probe.
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { spawn, spawnSync } from "node:child_process";
import { gzipSync } from "node:zlib";
import { openVault, sealVault } from "../client/vault-crypto.js";
import { agentWindowSizeCommand, shellQuote } from "../shared/tmux-command.js";
import { useDomRenderer } from "./dom-renderer.mjs";

const pause = (ms) => new Promise((r) => setTimeout(r, ms));
async function wait(fn, label) {
  for (let i = 0; i < 200; i++) {
    if (await fn()) return;
    await pause(40);
  }
  throw Error("Agent sizing fixture timed out: " + label);
}

// tmux stops expanding a format after 100 ms and leaves the rest empty, still
// exiting 0. A literal placed last survives only when every field before it
// expanded, so read until each line carries it (and matches shape, if given).
export const TMUX_FORMAT_END = "|tt-format-end";
export function readTmuxFormat(
  run,
  args,
  { shape, timeoutMs = 5000, pauseMs = 50 } = {},
) {
  const format = args.at(-1),
    deadline = Date.now() + timeoutMs;
  for (let reads = 1; ; reads++) {
    const value = String(run(...args.slice(0, -1), format + TMUX_FORMAT_END));
    const lines = value.trim().split("\n");
    if (value.trim() && lines.every((line) => line.endsWith(TMUX_FORMAT_END))) {
      const whole = lines.map((line) => line.slice(0, -TMUX_FORMAT_END.length));
      if (!shape || whole.every((line) => shape.test(line)))
        return whole.join("\n");
    }
    const got = `${format} got ${JSON.stringify(value)}`;
    if (Date.now() >= deadline)
      throw Error(`incomplete tmux reading after ${reads} reads: ${got}`);
    process.stderr.write(`tmux format re-read ${reads}: ${got}\n`);
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, pauseMs);
  }
}

// Bounds for the private tmux tests: one synchronous process call, one awaited
// step, and one whole test that starts a private server.
export const TMUX_CALL_TIMEOUT_MS = 20000,
  SIZING_STEP_TIMEOUT_MS = 15000,
  SIZING_TEST_TIMEOUT_MS = 60000;

// The only synchronous process call in the private tmux tests. A call that
// outlives its bound is killed and reported by name. A stuck tmux server has
// ignored SIGTERM, so with a private socket path every process group whose
// command line carries that exact path is killed too. The path sits in a fresh
// temporary directory, so no other server can match it.
export function runBounded(
  file,
  args,
  { timeoutMs = TMUX_CALL_TIMEOUT_MS, socket, ...options } = {},
) {
  const r = spawnSync(file, args, {
    encoding: "utf8",
    ...options,
    timeout: timeoutMs,
    killSignal: "SIGKILL",
  });
  if (r.error?.code !== "ETIMEDOUT") return r;
  const text = [file, ...args].join(" "),
    command = text.length > 600 ? text.slice(0, 600) + "..." : text;
  throw Error(
    `tmux command hung after ${timeoutMs} ms: ${command}; ` +
      (socket
        ? `killed private server process group(s) ${endPrivateServer(socket).join(", ") || "none found"}`
        : "no private socket was given"),
  );
}
function endPrivateServer(socket) {
  const killed = [];
  if (!socket.startsWith("/")) return killed;
  let listing;
  try {
    listing = runBounded("ps", ["-axww", "-o", "pid=,pgid=,command="], {
      timeoutMs: 5000,
      maxBuffer: 64 * 1024 * 1024,
    }).stdout;
  } catch {
    return killed;
  }
  const rows = String(listing || "")
    .split("\n")
    .map((line) => /^\s*(\d+)\s+(\d+)\s+(.*)$/.exec(line))
    .filter(Boolean)
    .map(([, pid, pgid, command]) => ({ pid: +pid, pgid: +pgid, command }));
  // Children of this process share its group; never signal that one.
  const own = rows.find((row) => row.pid === process.pid)?.pgid;
  if (!own) return killed;
  const groups = new Set(
    rows
      .filter(
        (row) =>
          row.command.includes(socket) && row.pgid !== own && row.pgid > 1,
      )
      .map((row) => row.pgid),
  );
  for (const pgid of groups)
    try {
      process.kill(-pgid, "SIGKILL");
      killed.push(pgid);
    } catch {}
  return killed;
}

// Rejects with the name of the awaited step, so a hang reads as that step
// instead of running to the check timeout.
export function step(label, promise, ms = SIZING_STEP_TIMEOUT_MS) {
  let timer;
  return Promise.race([
    promise,
    new Promise((resolve, reject) => {
      timer = setTimeout(
        () =>
          reject(
            Error(
              `Agent sizing test step still pending after ${ms} ms: ${label}`,
            ),
          ),
        ms,
      );
    }),
  ]).finally(() => clearTimeout(timer));
}

// A sizing command guards on one identity format. When tmux cuts that format
// at its 100 ms limit the guard is empty, so the command prints "refused" (or
// nothing, or a short "ready:") and exits 0 having changed nothing: the same
// bytes as a real refusal. So for such a reply the identity is read whole and
// compared here. If it does not hold, the reply is a real refusal and is
// returned at once. If it holds, the reply was cut and the command runs again.
const SIZING_IDENTITY =
  "#{session_id}|#{session_created}|#{TAILTERM_TASK}|#{TAILTERM_AGENT}|#{TAILTERM_RUN}|#{TAILTERM_ROLE}|#{window_name}|#{window_panes}";
const sizingAction = (command) =>
  command.includes("ready:")
    ? "inspect"
    : command.includes("-p restored")
      ? "restore"
      : command.includes("-p released")
        ? "release"
        : command.includes("@tailterm_size_revision},")
          ? "claim"
          : "resize";
export function sizingReply({
  run,
  read,
  target,
  binding,
  timeoutMs = 5000,
  pauseMs = 50,
}) {
  const holds = () => {
    let facts;
    try {
      facts = readTmuxFormat(
        read,
        ["display-message", "-p", "-t", target.id + ":agent", SIZING_IDENTITY],
        { shape: /^[^|]*(?:\|[^|]*){7}$/ },
      );
    } catch (e) {
      // A hung or unreadable server is a fixture failure, not a refusal.
      if (/^(?:tmux command hung|incomplete tmux reading)/.test(e.message))
        throw e;
      return false; // the target does not exist
    }
    const [id, created, task, agent, runId, role, name, panes] =
      facts.split("|");
    return (
      id === target.id &&
      created === String(target.created) &&
      task === binding.taskId &&
      agent === binding.agentId &&
      runId === binding.runId &&
      role !== "owner_helper" &&
      name === "agent" &&
      panes === "1"
    );
  };
  return (command) => {
    const deadline = Date.now() + timeoutMs;
    for (let runs = 1; ; runs++) {
      const output = run(command),
        reply = String(output).trim();
      const suspect =
        reply === "refused" ||
        reply === "" ||
        (reply.startsWith("ready:") &&
          !/^ready:(?:[a-zA-Z0-9_-]{16,64})?:\d+x\d+$/.test(reply));
      if (!suspect || !holds()) return output;
      process.stderr.write(
        `tmux sizing re-run ${runs}: ${JSON.stringify(reply)} while identity holds\n`,
      );
      if (Date.now() >= deadline)
        throw Error(
          `tmux sizing reply stayed ${JSON.stringify(reply)} for ${timeoutMs} ms while identity holds: ${sizingAction(command)}`,
        );
      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, pauseMs);
    }
  };
}

export function setupAgentWindowFixture() {
  const dir = mkdtempSync(tmpdir() + "/tailterm-agent-sizing-");
  const wrapper = dir + "/private-tmux";
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      ([k]) =>
        !k.startsWith("TAILTERM_") && !["TMUX", "TT_TMUX_SOCKET"].includes(k),
    ),
  );
  const processes = new Set(),
    contexts = new Set(),
    commands = [];
  writeFileSync(
    wrapper,
    `#!/bin/sh\nexec tmux -f /dev/null -S ${shellQuote(dir + "/socket")} "$@"\n`,
    { mode: 0o700 },
  );
  const tmux = (...args) => {
    const r = runBounded(wrapper, args, {
      env,
      timeoutMs: 5000,
      socket: dir + "/socket",
    });
    assert.equal(r.status, 0, r.stderr || String(r.error));
    return r.stdout.trim();
  };
  // Fail prerequisite immediately, never silently skip real tmux.
  tmux("new-session", "-d", "-s", "fixture-keepalive", "sleep 3600");
  writeFileSync(
    dir + "/probe.py",
    `import os,sys,signal,termios,tty,json
tty.setraw(0)
count=0
def draw(*args):
 global count
 if args: count+=1
 size=os.get_terminal_size(0)
 with open(sys.argv[1],'w') as f: json.dump(dict(cols=size.columns,rows=size.lines,winch=count),f)
 os.write(1, ('\\033[2J\\033[H'+'SIZE %dx%d WINCH %d'%(size.columns,size.lines,count)+'\\r\\n'+('R'*(size.columns-1))+'E'+'\\r\\n\\r\\nINPUT>').encode())
signal.signal(signal.SIGWINCH,draw)
draw()
while True:
 data=os.read(0,1024)
 if not data: break
 os.write(1,data)
`,
  );
  writeFileSync(
    dir + "/pty-bridge.py",
    `import os,sys,pty,fcntl,termios,struct,subprocess,select,json,base64,signal
master,slave=pty.openpty()
client_tty=os.ttyname(slave)
def resize(rows,cols): fcntl.ioctl(master,termios.TIOCSWINSZ,struct.pack('HHHH',rows,cols,0,0))
resize(int(sys.argv[2]),int(sys.argv[3]))
p=subprocess.Popen(['/bin/sh','-c',sys.argv[1]],stdin=slave,stdout=slave,stderr=slave,start_new_session=True)
os.close(slave)
pending=b''
try:
 while p.poll() is None:
  ready,_,_=select.select([master,0],[],[],.1)
  if master in ready:
   try: data=os.read(master,65536)
   except OSError: break
   if not data: break
   os.write(1,data)
  if 0 in ready:
   data=os.read(0,65536)
   if not data: break
   pending+=data
   while b'\\n' in pending:
    line,pending=pending.split(b'\\n',1)
    message=json.loads(line)
    if 'data' in message: os.write(master,base64.b64decode(message['data']))
    else: resize(message['rows'],message['cols'])
finally:
 subprocess.run([sys.argv[4],'detach-client','-t',client_tty],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=2)
 os.close(master)
 try: p.wait(timeout=2)
 except subprocess.TimeoutExpired:
  p.terminate()
  p.wait(timeout=2)
sys.exit(p.returncode if p.returncode>=0 else 0)
`,
  );
  let claimGate;
  return {
    holdNextClaim() {
      let arrived;
      const gate = {
        arrived: new Promise((r) => (arrived = r)),
        mark: () => arrived(),
        release: () => gate.deliver?.(),
        // The held command's reply once it has run, and the same viewer's
        // release that follows it (a sizer drops a claim it may not keep).
        reply: () => (gate.held?.exited ? gate.held.output.trim() : undefined),
        released: () => {
          const token =
            /@tailterm_size_viewer[^a-zA-Z0-9_-]+([a-zA-Z0-9_-]{16,64})/.exec(
              gate.held?.command || "",
            )?.[1];
          return commands.some(
            (c) =>
              c.exited &&
              !c.pty &&
              c.command.includes(token) &&
              c.command.includes("display-message -p released"),
          );
        },
      };
      claimGate = gate;
      return gate;
    },
    dir,
    wrapper,
    env,
    contexts,
    commands,
    tmux,
    handleExec(session, accept, info, pty) {
      if (!info.command.includes(wrapper)) return false;
      const channel = accept();
      const entry = { command: info.command, pty, output: "" };
      commands.push(entry);
      const deliver = () => {
        const child = pty
          ? spawn(
              "python3",
              [
                dir + "/pty-bridge.py",
                info.command,
                String(pty.rows),
                String(pty.cols),
                wrapper,
              ],
              { env: { ...env, TERM: "xterm-256color" } },
            )
          : spawn("/bin/sh", ["-c", info.command], { env });
        processes.add(child);
        child.stdout.on("data", (d) => {
          if (!pty) entry.output += d;
          if (channel.writable) channel.write(d);
        });
        child.stderr.on("data", (d) => {
          console.error("Private sizing SSH stderr:", d.toString());
          if (channel.stderr.writable) channel.stderr.write(d);
        });
        if (pty) {
          const control = (message) => {
            if (child.stdin.writable)
              child.stdin.write(JSON.stringify(message) + "\n");
          };
          channel.on("data", (d) => control({ data: d.toString("base64") }));
          session.on("window-change", (accept, reject, size) =>
            control({ rows: size.rows, cols: size.cols }),
          );
        }
        channel.on("close", () => {
          child.stdin.end();
          if (!pty) child.kill();
        });
        child.on("close", (code) => {
          entry.exited = true;
          processes.delete(child);
          if (channel.writable) {
            channel.exit(code || 0);
            channel.end();
          }
        });
        child.on("error", (e) => {
          channel.stderr.write(e.message);
          channel.exit(1);
          channel.end();
        });
      };
      if (
        claimGate &&
        !pty &&
        info.command.includes("@tailterm_size_revision") &&
        info.command.includes("resize-window")
      ) {
        const gate = claimGate;
        claimGate = null;
        gate.held = entry;
        gate.deliver = deliver;
        gate.mark();
      } else deliver();
      return true;
    },
    async cleanup() {
      try {
        for (const c of contexts) await c.close();
        runBounded(wrapper, ["kill-server"], { env, socket: dir + "/socket" });
      } finally {
        for (const p of processes) {
          p.stdin.end();
          p.kill();
        }
        await pause(100);
        rmSync(dir, { recursive: true, force: true });
      }
    },
  };
}

export async function exerciseAgentWindowSizing({
  fixture: f,
  browser,
  page: source,
  hub,
  origin,
  engine,
}) {
  const task = hub.api.createTask("resize-fixture");
  const agent = hub.api.addAgent(task.id, {
    name: "resize-probe",
    session: "resize-probe",
    host: "production",
  });
  hub.api.event(task.id, "started", agent.id);
  f.tmux(
    "new-session",
    "-d",
    "-s",
    agent.session,
    "-n",
    "agent",
    "-x",
    "200",
    "-y",
    "50",
    "-e",
    "TAILTERM_TASK=" + task.id,
    "-e",
    "TAILTERM_AGENT=" + agent.id,
    "-e",
    "TAILTERM_RUN=" + agent.runId,
    "python3 " +
      shellQuote(f.dir + "/probe.py") +
      " " +
      shellQuote(f.dir + "/probe.json"),
  );
  f.tmux(
    "set-option",
    "-w",
    "-t",
    agent.session + ":",
    "window-size",
    "manual",
  );
  f.tmux("set-option", "-t", agent.session, "default-size", "200x50");
  f.tmux(
    "new-session",
    "-d",
    "-s",
    "unrelated-default",
    "-n",
    "unrelated",
    "sleep 600",
  );
  const unrelatedSize = () =>
    readTmuxFormat(
      f.tmux,
      [
        "display-message",
        "-p",
        "-t",
        "unrelated-default",
        "#{pane_width}x#{pane_height}|#{@tailterm_size_revision}|#{@tailterm_size_viewer}",
      ],
      { shape: /^\d+x\d+\|[^|]*\|[^|]*$/ },
    );
  const unrelatedBefore = unrelatedSize();
  const [id, created] = readTmuxFormat(
    f.tmux,
    [
      "display-message",
      "-p",
      "-t",
      agent.session,
      "#{session_id}|#{session_created}",
    ],
    { shape: /^\$\d+\|\d+$/ },
  ).split("|");
  const target = { id, created },
    binding = { taskId: task.id, agentId: agent.id, runId: agent.runId };
  const size = () =>
    readTmuxFormat(
      f.tmux,
      [
        "display-message",
        "-p",
        "-t",
        id + ":agent",
        "#{pane_width}x#{pane_height}",
      ],
      { shape: /^\d+x\d+$/ },
    );
  const owner = () =>
    readTmuxFormat(f.tmux, [
      "display-message",
      "-p",
      "-t",
      id + ":agent",
      "#{@tailterm_size_viewer}",
    ]);
  const probe = () => {
    try {
      return JSON.parse(readFileSync(f.dir + "/probe.json", "utf8"));
    } catch {
      return {};
    }
  };
  const record = await source.evaluate(
    () =>
      new Promise((resolve, reject) => {
        const r = indexedDB.open("tailserve", 1);
        r.onsuccess = () => {
          const q = r.result
            .transaction("vault")
            .objectStore("vault")
            .get("encrypted-v2");
          q.onsuccess = () => resolve(q.result);
          q.onerror = reject;
        };
        r.onerror = reject;
      }),
  );
  const pass = "static browser vault passphrase",
    opened = await openVault(record, pass);
  const server = {
    ...opened.data.servers.find((s) => s.name === "Production"),
    tmuxPath: f.wrapper,
    password: "static-ssh-password",
  };
  assert.ok(
    server.fingerprint && server.password,
    "synthetic SSH credentials and trusted fingerprint for private fixture",
  );
  const endpoint = JSON.stringify([
    server.host,
    server.port,
    server.username,
    server.tmuxPath,
  ]);
  const tab = {
    id: "resize-pane",
    serverId: server.id,
    endpoint,
    tmux: true,
    session: agent.session,
    task: binding,
    target,
  };
  const seed = await sealVault(
    {
      servers: [server],
      keys: [],
      tailscale: {},
      hub: { url: origin + "/fixture-hub" },
      sessions: [
        { serverId: server.id, name: agent.session, task: binding, target },
      ],
      workspace: {
        tabs: [tab],
        groups: [{ tree: { tab: tab.id }, active: tab.id, taskId: task.id }],
        active: tab.id,
        tasks: [task.id],
      },
    },
    opened.key,
    opened.salt,
  );
  async function viewer(initialHidden = false) {
    const context = await browser.newContext({
      viewport: { width: 2600, height: 1600 },
    });
    context.setDefaultTimeout(15000);
    context.setDefaultNavigationTimeout(15000);
    f.contexts.add(context);
    await useDomRenderer(context);
    await context.addInitScript((hidden) => {
      if (!hidden || localStorage.getItem("fixture-agent-shown")) return;
      document.addEventListener("DOMContentLoaded", () => {
        const style = document.createElement("style");
        style.textContent =
          "html.fixture-agent-hidden .terminal-viewport { visibility: hidden !important; }";
        document.head.append(style);
        document.documentElement.classList.add("fixture-agent-hidden");
      });
    }, initialHidden);
    await context.addInitScript(
      (url) => {
        globalThis.__tailserveTestSocketURL = url;
      },
      origin.replace("http:", "ws:") + "/fixture-ssh",
    );
    await context.route("**/*.wasm.gz", (route) =>
      route.fulfill({
        body: gzipSync(readFileSync(".build/test.wasm")),
        contentType: "application/gzip",
      }),
    );
    const p = await context.newPage();
    p.on("pageerror", (e) =>
      console.error(`${engine} agent-sizing page error: ${e.message}`),
    );
    await p.goto(origin);
    await p.evaluate(
      (seed) =>
        new Promise((resolve, reject) => {
          const r = indexedDB.open("tailserve", 1);
          r.onupgradeneeded = () => r.result.createObjectStore("vault");
          r.onsuccess = () => {
            const tx = r.result.transaction("vault", "readwrite");
            tx.objectStore("vault").put(seed, "encrypted-v2");
            tx.objectStore("vault").put({ version: 2 }, "active-vault");
            tx.oncomplete = resolve;
            tx.onerror = reject;
          };
          r.onerror = reject;
        }),
      seed,
    );
    await p.reload();
    await p.locator("#password").fill(pass);
    await p.locator("#unlock-button").click();
    await p.locator("#workspace").waitFor();
    try {
      await wait(
        async () =>
          (await p.locator("#terminal-status").textContent()).includes(
            "Connected",
          ),
        "viewer connected",
      );
    } catch (e) {
      console.error(
        `${engine} sizing viewer state`,
        await p.evaluate(() => ({
          status: document.querySelector("#terminal-status")?.textContent,
          notice: document.querySelector("#notice")?.textContent,
          panes: document.querySelectorAll(".terminal-instance").length,
          dialog: document.querySelector("dialog[open]")?.textContent,
        })),
        { privateExecs: f.commands.length },
      );
      throw e;
    }
    await p.bringToFront();
    if (!initialHidden)
      await p.locator(".terminal-instance:not([hidden])").click();
    return p;
  }
  async function dimensions(p) {
    return (await p.locator("#dimensions").innerText())
      .split("×")
      .map((v) => +v.trim());
  }
  async function fit(p, cols, rows, expectSize = true) {
    await p.evaluate(
      ({ cols, rows }) => {
        const pane = document.querySelector(".terminal-instance:not([hidden])"),
          view = pane.querySelector(".terminal-viewport");
        const screen = view.querySelector(".xterm-screen"),
          [oldCols, oldRows] = document
            .querySelector("#dimensions")
            .textContent.split("×")
            .map(Number);
        const box = screen.getBoundingClientRect();
        view.style.width = Math.ceil((box.width / oldCols) * cols + 15) + "px";
        view.style.height = Math.ceil((box.height / oldRows) * rows + 1) + "px";
        pane.style.width = parseFloat(view.style.width) + 26 + "px";
        pane.style.height = parseFloat(view.style.height) + 20 + "px";
      },
      { cols, rows },
    );
    await wait(async () => {
      const [c, r] = await dimensions(p);
      return c === cols && r === rows;
    }, `viewport ${cols}x${rows}`);
    if (!expectSize) return;
    try {
      await wait(
        () => size() === `${Math.max(cols, 80)}x${Math.max(rows - 1, 24)}`,
        `program ${cols}x${rows}`,
      );
    } catch (e) {
      console.error(
        `${engine} sizing program state`,
        { size: size(), claimed: !!owner(), privateExecs: f.commands.length },
        await p.evaluate(() => ({
          focused: document.hasFocus(),
          visibility: document.visibilityState,
          status: document.querySelector("#terminal-status")?.textContent,
          notice: document.querySelector("#notice")?.textContent,
          dimensions: document.querySelector("#dimensions")?.textContent,
          terminal: document
            .querySelector(".terminal-instance .xterm-rows")
            ?.textContent?.slice(-1500),
        })),
      );
      throw e;
    }
  }
  const A = await viewer(true);
  await wait(
    async () =>
      !(await A.locator("#terminal-status").textContent()).includes(
        "unverified",
      ),
    "hidden target verified",
  );
  const initialAttach = f.commands.find(
    (c) => c.pty && c.command.includes("attach-session"),
  );
  assert.ok(initialAttach?.command.includes("ignore-size"));
  assert.equal(initialAttach.pty.cols, 200);
  assert.equal(initialAttach.pty.rows, 50);
  assert.equal(size(), "200x50");
  assert.equal(owner(), "", "initial hidden attach never claims");
  console.log(
    `${engine} agent-sizing a2: real hidden PTY opens at 200x50 ignore-size without a claim`,
  );
  await A.evaluate(() => {
    localStorage.setItem("fixture-agent-shown", "true");
    document.documentElement.classList.remove("fixture-agent-hidden");
  });
  await A.locator(".terminal-instance:not([hidden])").click();
  await fit(A, 240, 60);
  await wait(
    () => probe().cols === 240 && probe().rows === 59 && probe().winch > 0,
    "SIGWINCH grow",
  );
  const firstWinch = probe().winch;
  await fit(A, 120, 35);
  await wait(
    () =>
      probe().cols === 120 && probe().rows === 34 && probe().winch > firstWinch,
    "SIGWINCH shrink",
  );
  await A.locator(
    ".terminal-instance:not([hidden]) .terminal-viewport .xterm-helper-textarea",
  ).focus();
  await A.keyboard.type("i".repeat(130));
  await wait(
    () =>
      f
        .tmux("capture-pane", "-p", "-t", id + ":agent")
        .includes("i".repeat(114)),
    "input received",
  );
  const lines = f.tmux("capture-pane", "-p", "-t", id + ":agent").split("\n");
  assert.equal(lines[3], "INPUT>" + "i".repeat(114));
  assert.equal(lines[4], "i".repeat(16), "input wraps at visible pane width");
  await fit(A, 240, 60);
  await wait(
    async () =>
      (await A.locator(".terminal-viewport .xterm-rows").innerText()).includes(
        "SIZE 240x59",
      ),
    "real tmux output reaches xterm",
  );
  const rendered = await A.locator(
    ".terminal-viewport .xterm-rows > div",
  ).allTextContents();
  assert.equal(
    rendered[1],
    "R".repeat(239) + "E",
    "right edge rendered without tmux dot fill",
  );
  console.log(
    `${engine} agent-sizing a1: grow/shrink, SIGWINCH, real input wrap and right edge pass`,
  );

  const retained = size();
  for (const mutation of ["hidden", "collapse", "minimize"]) {
    await A.evaluate((kind) => {
      const v = document.querySelector(
        ".terminal-instance:not([hidden]) .terminal-viewport",
      );
      if (kind === "hidden") v.hidden = true;
      if (kind === "collapse") v.style.visibility = "collapse";
      if (kind === "minimize") v.style.display = "none";
      window.dispatchEvent(new Event("blur"));
    }, mutation);
    await pause(250);
    assert.equal(size(), retained, mutation + " cannot shrink");
    await A.evaluate(() => {
      const v = document.querySelector(".terminal-viewport");
      v.hidden = false;
      v.style.visibility = "";
      v.style.display = "";
      window.dispatchEvent(new Event("focus"));
    });
    await pause(150);
  }
  await A.locator("[data-mode=board]").click();
  await pause(250);
  assert.equal(size(), retained, "Board keeps size");
  await A.locator("[data-mode=terminals]").click();
  await A.locator(".terminal-instance:not([hidden])").click();
  await fit(A, 16, 2);
  assert.equal(size(), "80x24", "tiny visible viewport keeps usable minimum");
  console.log(
    `${engine} agent-sizing a2: hidden/collapse/minimize/Board and tiny minimum pass`,
  );

  await fit(A, 120, 35);
  const aToken = owner();
  const B = await viewer();
  await fit(B, 240, 60);
  const bToken = owner();
  assert.notEqual(aToken, bToken);
  await fit(A, 140, 40, false);
  await fit(A, 120, 35, false);
  assert.equal(size(), "240x59");
  assert.equal(owner(), bToken, "blurred viewport resize cannot claim");
  const invoke = (action, token, changes = {}) => {
    const r = runBounded(
      "/bin/sh",
      [
        "-c",
        agentWindowSizeCommand(
          {
            target,
            binding,
            action,
            token,
            cols: 120,
            rows: 35,
            expectedRevision: readTmuxFormat(f.tmux, [
              "display-message",
              "-p",
              "-t",
              id + ":agent",
              "#{@tailterm_size_revision}",
            ]),
            ...changes,
          },
          f.wrapper,
        ),
      ],
      { env: f.env, socket: f.dir + "/socket" },
    );
    assert.equal(r.status, 0, r.stderr);
    return r.stdout.trim();
  };
  for (const action of ["resize", "release"]) {
    assert.equal(invoke(action, aToken), "superseded");
    assert.equal(size(), "240x59");
  }
  const delayed = f.holdNextClaim();
  await A.bringToFront();
  await A.locator(".terminal-instance:not([hidden])").click();
  await delayed.arrived;
  await A.locator("[data-mode=board]").click();
  await B.bringToFront();
  await B.locator(".terminal-instance:not([hidden])").click();
  // B drops its old claim before it claims again, so the token is briefly
  // empty: read it once per poll and keep that same read as B's new token.
  let newerB;
  await wait(() => {
    newerB = owner();
    return newerB && newerB !== bToken && size() === "240x59";
  }, "B accepts newer focus while A claim is delayed");
  delayed.release();
  await wait(
    () => delayed.reply() !== undefined,
    "delayed A claim reaches the host",
  );
  assert.equal(
    delayed.reply(),
    "superseded",
    "main integration obsolete A claim is refused",
  );
  await wait(() => delayed.released(), "A releases its refused claim");
  assert.equal(
    size(),
    "240x59",
    "main integration obsolete A claim cannot change B size",
  );
  assert.equal(
    owner(),
    newerB,
    "main integration obsolete A claim cannot release B authority",
  );
  await B.locator("[data-mode=board]").click();
  // A is already in Board from the delayed-claim transition.
  await wait(() => owner() === "", "all hidden release authority");
  assert.equal(
    size(),
    "240x59",
    "all hidden viewers retain the last safe size",
  );
  await A.locator("[data-mode=terminals]").evaluate((el) => el.click());
  await A.bringToFront();
  await A.locator(".terminal-instance:not([hidden])").click();
  await wait(
    () => size() === "120x34" && owner() !== bToken,
    "same-size A refocus wins",
  );
  await B.close();
  await pause(250);
  assert.equal(size(), "120x34", "old viewer detach cannot override focus");
  const beforeReconnect = owner();
  await A.reload();
  await A.locator("#password").fill(pass);
  await A.locator("#unlock-button").click();
  await A.locator("#workspace").waitFor();
  await A.locator(".terminal-instance:not([hidden])").click();
  await wait(() => owner() !== beforeReconnect, "reconnect fresh claim");
  console.log(
    `${engine} agent-sizing a3: two contexts, stale update/release, same-size refocus, detach and reconnect pass`,
  );

  await A.locator("[data-mode=board]").click();
  await pause(200);
  const frozen = size();
  for (const changes of [
    { target: { id, created: "0" } },
    { binding: { ...binding, runId: "run_0000000000000000" } },
  ]) {
    assert.equal(
      invoke("claim", "fixture_0000000000000001", changes),
      "refused",
    );
    assert.equal(size(), frozen);
  }
  f.tmux("set-environment", "-t", id, "TAILTERM_ROLE", "owner_helper");
  assert.equal(invoke("claim", "fixture_0000000000000001"), "refused");
  assert.equal(size(), frozen);
  f.tmux("set-environment", "-u", "-t", id, "TAILTERM_ROLE");
  f.tmux("rename-session", "-t", id, "resize-renamed");
  assert.equal(invoke("claim", "fixture_0000000000000001"), "sized");
  f.tmux(
    "new-session",
    "-d",
    "-s",
    agent.session,
    "-x",
    "90",
    "-y",
    "30",
    "sleep 60",
  );
  assert.equal(
    invoke("claim", "fixture_0000000000000001", { cols: 240 }),
    "sized",
  );
  assert.equal(
    readTmuxFormat(
      f.tmux,
      [
        "display-message",
        "-p",
        "-t",
        agent.session,
        "#{pane_width}x#{pane_height}",
      ],
      { shape: /^\d+x\d+$/ },
    ),
    "90x30",
  );
  const prior = size();
  const fail = runBounded(
    "/bin/sh",
    [
      "-c",
      agentWindowSizeCommand(
        {
          target: { id: "$99999", created },
          binding,
          action: "claim",
          expectedRevision: "",
          token: "fixture_0000000000000001",
          cols: 80,
          rows: 24,
        },
        f.wrapper,
      ),
    ],
    { env: f.env, socket: f.dir + "/socket" },
  );
  assert.equal(fail.stdout.toString().trim(), "refused");
  assert.equal(size(), prior);
  const missingBinary = runBounded(
    "/bin/sh",
    [
      "-c",
      agentWindowSizeCommand(
        {
          target,
          binding,
          action: "claim",
          expectedRevision: "",
          token: "fixture_0000000000000001",
          cols: 80,
          rows: 24,
        },
        f.dir + "/missing-tmux",
      ),
    ],
    { env: f.env, socket: f.dir + "/socket" },
  );
  assert.equal(missingBinary.status, 127);
  assert.equal(size(), prior);
  console.log(
    `${engine} agent-sizing a5: exact session/run/helper guards, rename/reused name, command failure pass`,
  );
  assert.equal(
    unrelatedSize(),
    unrelatedBefore,
    "unrelated default window never resized or claimed",
  );
  await A.close();
  for (const c of f.contexts) await c.close();
  f.contexts.clear();
  hub.api.closeTask(task.id);
}
