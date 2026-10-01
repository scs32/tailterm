// Live and rollback probes for the deployment agent. Each prints exactly the
// fields HostAdapter.check or rollback reads. Tokens and environment never
// reach stdout, and host output only as the capture below; any failure prints
// nothing and exits 1.
// The hub and bridge probes poll the app until its containers run and the hub
// answers, for up to targets.hub.readyWindowMs / targets.bridge.readyWindowMs
// (default 240 s, 0 means one read), and report waitedMs and polls. When that
// ends not ready they add `capture`: allowlisted app state and redacted recent
// container log lines, or "unavailable" where a read failed.
//
//   node scripts/release-probe.mjs live <hub|bridge|mini|tailos> --config PRIVATE
//   node scripts/release-probe.mjs rollback <hub|bridge> --expect-release NAME --expect-sha SHA --config PRIVATE
//     (hub and bridge, live and rollback: poll up to the readiness window)
//   node scripts/release-probe.mjs rollback mini [--expect-sha SHA] --config PRIVATE
//   node scripts/release-probe.mjs rollback tailos --expect-commit SHA [--config PRIVATE]
//     (polls release.json up to targets.tailos.switchWindowMs, default 90 s)
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, openSync, readFileSync, readSync, closeSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const BASE = "/mnt/deepfreeze/tailterm-hub", APP = "tailterm-hub";
// target -> compose service, container mount, binary name
const MOUNTS = { hub: ["hub", "/opt/tailterm-hub", "tailterm-hub"], bridge: ["discord-bridge", "/opt/tailterm-discord", "tailterm-discord"] };
const sha256 = b => createHash("sha256").update(b).digest("hex");
const hex = (s, n) => typeof s === "string" && new RegExp(`^[a-f0-9]{${n}}$`).test(s);

// Cloudflare's custom domain takes a few seconds to switch to a new Pages
// deployment, so an expected TailOS commit is polled for a bounded window.
export const TAILOS_SWITCH_WINDOW_MS = 90000, TAILOS_POLL_INTERVAL_MS = 3000, TAILOS_FETCH_TIMEOUT_MS = 10000;
// A TrueNAS app restart plus the hub's migrations takes longer than one read,
// so hub and bridge readiness is polled for a bounded window as well.
export const TRUENAS_READY_WINDOW_MS = 240000, TRUENAS_READY_POLL_MS = 5000, TRUENAS_READY_RUN_TIMEOUT_MS = 20000;
// Each of the three identity reads (app.config, the binary, its build info).
// With the 300 s window cap this keeps a whole probe under the runner's 600 s
// command timeout: 3 x 30 s, the window, one last poll (2 x 20 s) and the
// 45 s capture are at most 475 s.
export const TRUENAS_IDENTITY_RUN_TIMEOUT_MS = 30000;
// The capture after a not-ready end: per-command timeout, total budget, and
// the bounds on what is kept.
export const CAPTURE_RUN_TIMEOUT_MS = 15000, CAPTURE_BUDGET_MS = 45000, CAPTURE_LOG_LINES = 40, CAPTURE_LINE_LENGTH = 300, CAPTURE_LOG_CONTAINERS = 4, CAPTURE_CONTAINERS = 8;
export const CAPTURE_UNAVAILABLE = ["log read failed", "no log lines", "invalid container id", "time budget"];

export const hostDeps = {
  run: (argv, { buffer = false, timeout = 120000 } = {}) => {
    const r = spawnSync(argv[0], argv.slice(1), { encoding: buffer ? "buffer" : "utf8", stdio: ["ignore", "pipe", "pipe"], maxBuffer: 256 * 1024 * 1024, timeout });
    return { status: r.status, stdout: r.stdout };
  },
  fetchJSON: async (url, timeoutMs = TAILOS_FETCH_TIMEOUT_MS) => { const r = await fetch(url, { signal: AbortSignal.timeout(timeoutMs), cache: "no-store" }); if (!r.ok) throw new Error("fetch"); return r.json(); },
  sleep: ms => new Promise(r => setTimeout(r, ms)),
  now: () => Date.now(),
  uid: () => process.getuid(),
};

function ok(deps, argv, opts) {
  const r = deps.run(argv, opts);
  if (r.status !== 0) throw new Error("probe command failed");
  return r.stdout;
}
const sshArgv = (host, command) => ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, command];
const ssh = (deps, host, command, opts) => ok(deps, sshArgv(host, command), opts);

// vcs.revision and vcs.modified from the Go build info embedded in a binary.
export function buildInfo(deps, path, opts) {
  const out = ok(deps, ["go", "version", "-m", path], opts);
  const field = k => out.match(new RegExp(`^\\s*build\\s+${k.replace(".", "\\.")}=(\\S+)$`, "m"))?.[1];
  const commit = field("vcs.revision");
  if (!hex(commit, 40)) throw new Error("no build revision");
  return { commit, integrity: field("vcs.modified") === "false" };
}

// targets.hub.readyWindowMs / targets.bridge.readyWindowMs: 0 means one read.
// The 300 s cap keeps a probe inside the runner's 600 s command timeout.
export function readyWindow(config, target) {
  const w = config?.targets?.[target]?.readyWindowMs;
  if (w === undefined) return TRUENAS_READY_WINDOW_MS;
  if (!Number.isSafeInteger(w) || w < 0 || w > 300000) throw new Error("invalid readiness window");
  return w;
}

// Which retained release the app mounts and what that binary is. Read once:
// waiting cannot change it, so any failure here still fails the probe.
function readIdentity(target, config, deps) {
  const t = config?.targets?.[target];
  if (!/^[A-Za-z0-9._-]+$/.test(t?.host || "")) throw new Error("host reference required");
  const [service, mount, binary] = MOUNTS[target], timeout = TRUENAS_IDENTITY_RUN_TIMEOUT_MS;
  const compose = JSON.parse(ssh(deps, t.host, `midclt call app.config ${APP}`, { timeout }));
  const volumes = compose?.services?.[service]?.volumes || [];
  const path = volumes.map(v => String(v).split(":")).find(p => p[1] === mount)?.[0];
  const release = path?.match(new RegExp(`^${BASE}/releases/([A-Za-z0-9._-]+)/${binary}$`))?.[1];
  if (!release) throw new Error("no retained release mount");
  const bytes = ssh(deps, t.host, `cat ${path}`, { buffer: true, timeout });
  const dir = mkdtempSync(join(tmpdir(), "release-probe-"));
  let info;
  try { writeFileSync(join(dir, binary), bytes, { mode: 0o600 }); info = buildInfo(deps, join(dir, binary), { timeout }); }
  finally { rmSync(dir, { recursive: true, force: true }); }
  const stateMounted = (compose?.services?.hub?.volumes || []).includes(`${BASE}/state:/state`);
  return { host: t.host, service, compose, commit: info.commit, artifactSHA256: sha256(bytes), integrity: info.integrity, release, stateMounted };
}

// One readiness read. A command that fails or cannot be read is "not ready",
// never a probe failure: the app is expected to be down while it restarts.
function readReady(config, host, service, deps) {
  let instance = null, containersRunning = false, hubResponds = false;
  try {
    const r = deps.run(sshArgv(host, `midclt call app.get_instance ${APP}`), { timeout: TRUENAS_READY_RUN_TIMEOUT_MS });
    if (r.status === 0) instance = JSON.parse(r.stdout);
    const containers = (instance?.active_workloads?.container_details || []).filter(c => c.service_name === service);
    containersRunning = instance?.state === "RUNNING" && (!instance?.active_workloads?.container_details || (containers.length > 0 && containers.every(c => c.state === "running")));
  } catch { instance = null; containersRunning = false; }
  // The hub listens only after its migrations, so an authenticated read
  // through the installed tt proves both.
  try { hubResponds = deps.run([config.tt || "tt", "projects"], { timeout: TRUENAS_READY_RUN_TIMEOUT_MS }).status === 0; } catch {}
  return { instance, containersRunning, hubResponds };
}

const REDACTED = "[redacted]", NAME = /^[A-Za-z0-9._-]{1,64}$/;
// Control characters go first; then known environment values, labelled
// credentials, and any long opaque run with both a letter and a digit (which
// also hides commit hashes and container ids); then the line is cut.
function redactLine(line, secrets) {
  let s = String(line).replace(/[\u0000-\u001f\u007f-\u009f]/g, "");
  for (const v of secrets) s = s.split(v).join(REDACTED);
  s = s.replace(/Bearer\s+\S+/gi, REDACTED)
    .replace(/(token|secret|password|passwd|authorization|api[_-]?key)["']?\s*[=:]\s*\S+/gi, REDACTED)
    .replace(/[A-Za-z0-9+\/_=.-]{24,}/g, m => /[A-Za-z]/.test(m) && /[0-9]/.test(m) ? REDACTED : m);
  return s.slice(0, CAPTURE_LINE_LENGTH);
}

// The only shape a capture may have, whatever it was built from:
//   { app: {state, containers: [{service, state, id}]} | "unavailable",
//     logs: [{service, lines: [...]} | {service, unavailable: REASON}] }
// Unknown keys are dropped, names and ids are pattern-checked, log lines are
// redacted and bounded. The runner applies it again before journaling.
export function sanitizeCapture(raw, secrets = []) {
  const name = v => typeof v === "string" && NAME.test(v) ? v : null;
  const known = (Array.isArray(secrets) ? secrets : []).filter(v => typeof v === "string" && v.length >= 6).sort((a, b) => b.length - a.length);
  const list = v => Array.isArray(v) ? v : [];
  const app = raw?.app && typeof raw.app === "object" ? {
    state: typeof raw.app.state === "string" && /^[A-Z_]{1,32}$/.test(raw.app.state) ? raw.app.state : null,
    containers: list(raw.app.containers).slice(0, CAPTURE_CONTAINERS).map(c => ({ service: name(c?.service), state: name(c?.state), id: typeof c?.id === "string" && /^[a-f0-9]{12,64}$/.test(c.id) ? c.id.slice(0, 12) : null })),
  } : "unavailable";
  const logs = list(raw?.logs).slice(0, CAPTURE_LOG_CONTAINERS).map(l => Array.isArray(l?.lines)
    ? { service: name(l.service), lines: l.lines.filter(x => typeof x === "string").slice(-CAPTURE_LOG_LINES).map(x => redactLine(x, known)) }
    : { service: name(l?.service), unavailable: CAPTURE_UNAVAILABLE.includes(l?.unavailable) ? l.unavailable : CAPTURE_UNAVAILABLE[0] });
  return { app, logs };
}

// Every literal environment value of every compose service (map or K=V list).
function composeSecrets(compose) {
  return Object.values(compose?.services || {}).flatMap(service => {
    const env = service?.environment;
    if (Array.isArray(env)) return env.map(e => String(e).split("=").slice(1).join("="));
    return env && typeof env === "object" ? Object.values(env).map(v => String(v)) : [];
  });
}

// App state and recent container logs after a not-ready end, read through the
// middleware as the probe's SSH user (docker logs would need sudo). Best
// effort: it runs after the verdict is fixed and never throws; a read that
// fails, hangs or runs past the budget is recorded as unavailable. Each
// command's timeout is cut to what is left of the budget, so the whole capture
// stays inside it.
function captureTrueNAS(host, compose, instance, deps) {
  try {
    const start = deps.now(), left = () => CAPTURE_BUDGET_MS - (deps.now() - start), timeout = () => Math.min(CAPTURE_RUN_TIMEOUT_MS, left());
    if (!instance) {
      try { const r = deps.run(sshArgv(host, `midclt call app.get_instance ${APP}`), { timeout: timeout() }); if (r.status === 0) instance = JSON.parse(r.stdout); } catch {}
    }
    if (!instance || typeof instance !== "object") return { app: "unavailable", logs: [] };
    const details = Array.isArray(instance.active_workloads?.container_details) ? instance.active_workloads.container_details.slice(0, CAPTURE_CONTAINERS) : [];
    const logs = details.slice(0, CAPTURE_LOG_CONTAINERS).map(c => {
      const service = c?.service_name;
      try {
        // Only a 64-hex id is ever placed in a command.
        if (typeof c?.id !== "string" || !/^[a-f0-9]{64}$/.test(c.id)) return { service, unavailable: "invalid container id" };
        // Under a second left is not worth a read.
        if (left() < 1000) return { service, unavailable: "time budget" };
        const r = deps.run(sshArgv(host, `midclt subscribe -n ${CAPTURE_LOG_LINES} -t 8 'app.container_log_follow:{"app_name":"${APP}","container_id":"${c.id}","tail_lines":${CAPTURE_LOG_LINES}}'`), { timeout: timeout() });
        // The exit status is ignored when lines arrived: a quiet stream ends
        // by timeout after printing what it had.
        const lines = String(r.stdout || "").split("\n").flatMap(l => { try { const d = JSON.parse(l)?.fields?.data; return typeof d === "string" ? [d] : []; } catch { return []; } });
        if (lines.length) return { service, lines };
        return { service, unavailable: r.status === 0 ? "no log lines" : "log read failed" };
      } catch { return { service, unavailable: "log read failed" }; }
    });
    return sanitizeCapture({ app: { state: instance.state, containers: details.map(c => ({ service: c?.service_name, state: c?.state, id: c?.id })) }, logs }, composeSecrets(compose));
  } catch { return { app: "unavailable", logs: [] }; }
}

// Polls readiness until the containers run and the hub responds, or the window
// ends. With `expect`, an identity that does not match is read once and not
// waited on: no wait can make the wrong release the right one. A not-ready end
// adds the capture, which cannot change the verdict fields or waitedMs.
export async function waitForTrueNASReady(target, config, deps = hostDeps, { expect, intervalMs = TRUENAS_READY_POLL_MS } = {}) {
  const window = readyWindow(config, target);
  const { host, service, compose, ...identity } = readIdentity(target, config, deps);
  const identityMatched = !expect || (identity.release === expect.release && identity.artifactSHA256 === expect.artifactSHA256 && identity.integrity && identity.stateMounted);
  const windowMs = identityMatched ? window : 0, start = deps.now();
  let ready, polls = 0, waitedMs;
  for (;;) {
    ready = readReady(config, host, service, deps);
    polls++;
    waitedMs = deps.now() - start;
    if ((ready.containersRunning && ready.hubResponds) || waitedMs >= windowMs) break;
    await deps.sleep(Math.min(intervalMs, windowMs - waitedMs));
  }
  const out = { ...identity, hubResponds: ready.hubResponds, migrationsApplied: ready.hubResponds, containersRunning: ready.containersRunning, identityMatched, waitedMs, polls };
  return ready.containersRunning && ready.hubResponds ? out : { ...out, capture: captureTrueNAS(host, compose, ready.instance, deps) };
}

function relayErrors(config, t) {
  if (!t.relayLog || !existsSync(t.relayLog)) return 0;
  const marker = join(config.journalDirectory || "", "mini-relay-offset.json");
  const size = statSync(t.relayLog).size;
  // Without a deploy marker nothing new has been written since a deploy.
  const offset = existsSync(marker) ? Number(JSON.parse(readFileSync(marker, "utf8")).offset) : size;
  if (!(offset >= 0) || offset >= size) return 0;
  const fd = openSync(t.relayLog, "r"), buf = Buffer.alloc(size - offset);
  try { readSync(fd, buf, 0, buf.length, offset); } finally { closeSync(fd); }
  return buf.toString("utf8").split("\n").filter(l => /\berror\b/i.test(l)).length;
}

function liveMini(config, t, deps) {
  if (!t?.installPath) throw new Error("install path required");
  const bytes = readFileSync(t.installPath), info = buildInfo(deps, t.installPath);
  const relay = deps.run(["launchctl", "print", `gui/${deps.uid()}/${t.relayLabel || "com.tailterm.inbox-relay"}`]);
  return { commit: info.commit, artifactSHA256: sha256(bytes), integrity: info.integrity, relayRunning: relay.status === 0 && /^\s*state = running$/m.test(relay.stdout || ""), newErrors: relayErrors(config, t) };
}

export const tailosURL = config => config?.targets?.tailos?.url || "https://tailos.tailarr.com/release.json";
// targets.tailos.switchWindowMs: 0 means one read. The 300 s cap keeps the
// rollback probe inside the runner's 600 s command timeout.
export function tailosWindow(config) {
  const w = config?.targets?.tailos?.switchWindowMs;
  if (w === undefined) return TAILOS_SWITCH_WINDOW_MS;
  if (!Number.isSafeInteger(w) || w < 0 || w > 300000) throw new Error("invalid TailOS switch window");
  return w;
}

// Polls release.json until it shows the expected commit or the window ends.
// Only a 40-hex commit counts as a read; any other content is never returned.
export async function waitForTailOSCommit(url, expected, { windowMs = TAILOS_SWITCH_WINDOW_MS, intervalMs = TAILOS_POLL_INTERVAL_MS, timeoutMs = TAILOS_FETCH_TIMEOUT_MS, deps = hostDeps } = {}) {
  if (!hex(expected, 40)) throw new Error("expected commit required");
  const start = deps.now();
  let lastCommit = null, polls = 0;
  for (;;) {
    let commit = null;
    try { const r = await deps.fetchJSON(url, timeoutMs); if (hex(r?.commit, 40)) commit = r.commit; } catch {}
    polls++;
    if (commit) lastCommit = commit;
    const waitedMs = deps.now() - start;
    if (commit === expected) return { matched: true, lastCommit, waitedMs, polls };
    if (waitedMs >= windowMs) return { matched: false, lastCommit, waitedMs, polls };
    await deps.sleep(Math.min(intervalMs, windowMs - waitedMs));
  }
}

export async function probe(argv, config, deps = hostDeps) {
  const [mode, target] = argv, flag = name => { const i = argv.indexOf(name); return i < 0 ? undefined : argv[i + 1]; };
  const t = config?.targets?.[target];
  if (mode === "live") {
    if (target === "hub" || target === "bridge") { const { stateMounted, identityMatched, ...out } = await waitForTrueNASReady(target, config, deps); return out; }
    if (target === "mini") return liveMini(config, t, deps);
    if (target === "tailos") { const r = await deps.fetchJSON(tailosURL(config)); if (!hex(r?.commit, 40)) throw new Error("no release commit"); return { commit: r.commit }; }
  }
  if (mode === "rollback") {
    if (target === "hub" || target === "bridge") {
      const release = flag("--expect-release"), expected = flag("--expect-sha");
      if (!/^[A-Za-z0-9._-]+$/.test(release || "") || !hex(expected, 64)) throw new Error("expected release and hash required");
      const live = await waitForTrueNASReady(target, config, deps, { expect: { release, artifactSHA256: expected } });
      return { restored: live.release === release && live.artifactSHA256 === expected && live.integrity && live.containersRunning, databaseWritesPreserved: live.stateMounted && live.hubResponds, waitedMs: live.waitedMs, polls: live.polls, ...(live.capture ? { capture: live.capture } : {}) };
    }
    if (target === "mini") {
      const expected = flag("--expect-sha"), live = liveMini(config, t, deps);
      if (expected !== undefined && !hex(expected, 64)) throw new Error("invalid expected hash");
      // The Mini holds no hub database.
      return { restored: (expected === undefined || live.artifactSHA256 === expected) && live.relayRunning, databaseWritesPreserved: true };
    }
    if (target === "tailos") {
      const expected = flag("--expect-commit");
      if (!hex(expected, 40)) throw new Error("expected commit required");
      const w = await waitForTailOSCommit(tailosURL(config), expected, { windowMs: tailosWindow(config), deps });
      return { restored: w.matched, databaseWritesPreserved: true, lastCommit: w.lastCommit, waitedMs: w.waitedMs };
    }
  }
  throw new Error("usage: release-probe.mjs live|rollback TARGET");
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const argv = process.argv.slice(2), i = argv.indexOf("--config");
    const config = i < 0 ? {} : JSON.parse(readFileSync(argv[i + 1], "utf8"));
    process.stdout.write(JSON.stringify(await probe(argv, config)) + "\n");
  } catch { process.exitCode = 1; }
}
