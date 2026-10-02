// One host lock with an ordered waitlist for verification runs (phase 1,
// wi_6b4af2fb034ec36e, order #18569). Every verify-matrix run and targeted
// run, and supplemental runs started through `exec` below, hold it while they
// run. The lock and waitlist are one JSON file other tools can read. Waiting
// reads that file only: nothing here lists processes. See
// docs/objective-verification.md, "Host lock and waitlist".
import {
  readFileSync,
  writeFileSync,
  appendFileSync,
  mkdirSync,
  linkSync,
  unlinkSync,
  renameSync,
  rmSync,
} from "node:fs";
import { join, dirname, isAbsolute, resolve } from "node:path";
import { homedir, hostname, loadavg, availableParallelism } from "node:os";
import { randomUUID } from "node:crypto";
import { spawn, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

export const PRIORITIES = ["urgent", "high", "normal"];
export const DEFAULT_HOST_WAIT_MS = 240 * 60000;
export const DEFAULT_POLL_MS = 2000;
// No holder keeps the host longer than this, whatever its plan asks for
// (wi_c5cb667695c3614c). A plan's serial sum is far above what parallel lanes
// take, so the cap is what bounds a hung run.
export const DEFAULT_HOLDER_CAP_MS = 120 * 60000;
// After a holder's run timeout, how long a waiter still defers to whatever of
// that run is alive before it takes the lock as a recorded overlap.
export const RUN_TIMEOUT_GRACE_MS = 120000;
// A holder that cannot update the lock file for this long aborts its run.
export const HOLDER_BUSY_ABORT_MS = 30000;
export const EXIT_WAIT_EXPIRED = 75;
export const EXIT_LOCK_UNUSABLE = 78;
const REPEAT_LINE_MS = 60000;
const LOAD_SAMPLE_MS = 60000;
const LOAD_SAMPLES_KEPT = 64;

export class HostLockError extends Error {
  constructor(code, message, exitCode = 1) {
    super(message);
    this.code = code;
    this.exitCode = exitCode;
  }
}

// Resolved when a lock is requested, never at import.
export function lockPath(environment = process.env) {
  const override = environment.TAILTERM_MATRIX_HOST_LOCK;
  if (override) {
    if (!isAbsolute(override))
      throw new Error("TAILTERM_MATRIX_HOST_LOCK must be an absolute path");
    return override;
  }
  return join(environment.HOME || homedir(), ".local/state/tailterm-matrix/host.json");
}
export function lockPaths(file = lockPath()) {
  if (!isAbsolute(file)) throw new Error("Host lock path must be absolute");
  return {
    file,
    journal: join(dirname(file), "host.journal.jsonl"),
    mutex: file + ".lock",
    guard: file + ".lock.reclaim",
  };
}

// --priority, else TAILTERM_MATRIX_PRIORITY, else normal (owner answer #18706).
export function resolvePriority(flag, environment = process.env) {
  const named = (value, what) => {
    if (!PRIORITIES.includes(value))
      throw new Error(what + " must be urgent, high or normal");
    return value;
  };
  if (flag !== undefined && flag !== null)
    return { priority: named(flag, "--priority"), prioritySource: "flag" };
  const fromEnvironment = environment.TAILTERM_MATRIX_PRIORITY;
  if (fromEnvironment !== undefined && fromEnvironment !== "")
    return {
      priority: named(fromEnvironment, "TAILTERM_MATRIX_PRIORITY"),
      prioritySource: "environment",
    };
  return { priority: "normal", prioritySource: "default" };
}
// How long one item priority lookup may take before the run joins at normal.
export const ITEM_PRIORITY_LOOKUP_MS = 15000;
// The work item's priority as tt reports it. Throws with the reason on a
// failed, timed-out or unreadable lookup.
export function lookupItemPriority(
  item,
  environment = process.env,
  timeoutMs = ITEM_PRIORITY_LOOKUP_MS,
) {
  const result = spawnSync("tt", ["work-items", "get", "--json", item], {
    env: environment,
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
    timeout: timeoutMs,
    killSignal: "SIGKILL",
  });
  if (result.error)
    throw new Error(
      result.error.code === "ETIMEDOUT"
        ? `the lookup timed out after ${timeoutMs} ms`
        : "the lookup could not run: " + result.error.message,
    );
  if (result.status !== 0)
    throw new Error(
      "the lookup failed: " +
        ((result.stderr || "").trim().split("\n").pop() ||
          (result.signal ? "signal " + result.signal : "exit " + result.status)),
    );
  let found;
  try {
    found = JSON.parse(result.stdout);
  } catch {
    throw new Error("the lookup returned no JSON");
  }
  return found?.priority;
}
// For a verification run: --priority, else TAILTERM_MATRIX_PRIORITY, else the
// work item's own priority (low joins as normal), else normal with a warning
// (wi_84a87bdf138275ae). The lookup runs only when neither override is set, and
// it never fails the run.
export function resolveRunPriority(flag, item, options = {}) {
  const {
    environment = process.env,
    lookup = lookupItemPriority,
    warn = (line) => console.error(line),
  } = options;
  const explicit = resolvePriority(flag, environment);
  if (explicit.prioritySource !== "default") return explicit;
  let reason;
  if (typeof item !== "string" || !item || item === "unknown")
    reason = "the run names no work item";
  else
    try {
      const found = lookup(item, environment);
      if (PRIORITIES.includes(found))
        return { priority: found, prioritySource: "item" };
      if (found === "low") return { priority: "normal", prioritySource: "item" };
      reason =
        found === undefined || found === null || found === ""
          ? `work item ${item} has no priority`
          : `work item ${item} has the unrecognised priority ${JSON.stringify(found)}`;
    } catch (error) {
      reason = `work item ${item}: ${error?.message || error}`;
    }
  warn(`matrix host: joining at normal priority (default): ${reason}`);
  return explicit;
}
// TAILTERM_MATRIX_HOLDER_CAP_MINUTES in whole minutes, else the default.
export function holderCapMs(environment = process.env) {
  const raw = environment.TAILTERM_MATRIX_HOLDER_CAP_MINUTES;
  if (raw === undefined || raw === "") return DEFAULT_HOLDER_CAP_MS;
  return minutesFlag("TAILTERM_MATRIX_HOLDER_CAP_MINUTES", raw, 1440);
}
const rank = (priority) =>
  priority === "urgent" ? 0 : priority === "high" ? 1 : 2;

// Signal 0 probes without signalling. Only "no such process" counts as gone,
// so a pid or group we may not signal is treated as alive.
function gone(target) {
  try {
    process.kill(target, 0);
    return false;
  } catch (error) {
    return error.code === "ESRCH";
  }
}
export const pidGone = (pid) => !Number.isSafeInteger(pid) || pid <= 0 || gone(pid);
export const groupGone = (pgid) =>
  !Number.isSafeInteger(pgid) || pgid <= 1 || gone(-pgid);

const iso = (ms) => new Date(ms).toISOString();
const sleep = (ms, signal) =>
  new Promise((done) => {
    if (signal?.aborted) return done();
    const finish = () => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", finish);
      done();
    };
    const timer = setTimeout(finish, ms);
    signal?.addEventListener("abort", finish, { once: true });
  });
const pause = new Int32Array(new SharedArrayBuffer(4));
const sleepSync = (ms) => Atomics.wait(pause, 0, 0, ms);

function journal(paths, event, entry = {}, extra = {}) {
  mkdirSync(dirname(paths.journal), { recursive: true, mode: 0o700 });
  appendFileSync(
    paths.journal,
    JSON.stringify({
      at: new Date().toISOString(),
      event,
      id: entry.id ?? null,
      pid: entry.pid ?? process.pid,
      item: entry.item ?? "unknown",
      agent: entry.agent ?? "unknown",
      ...extra,
    }) + "\n",
    { mode: 0o600 },
  );
}
export function readJournal(file = lockPath()) {
  let raw;
  try {
    raw = readFileSync(lockPaths(file).journal, "utf8");
  } catch (error) {
    if (error.code === "ENOENT") return [];
    throw error;
  }
  return raw
    .split("\n")
    .filter(Boolean)
    .map((line) => JSON.parse(line));
}

// The mutex and the reclaim guard are taken the same way: the owner record is
// written to a private file first and linked into place. The link is atomic
// and fails when the name exists, so neither is ever visible without its owner.
function linkOwner(target, owner) {
  const temp = `${target}.${process.pid}.${randomUUID()}.tmp`;
  writeFileSync(temp, JSON.stringify(owner) + "\n", { mode: 0o600, flag: "wx" });
  try {
    linkSync(temp, target);
    return true;
  } catch (error) {
    if (error.code === "EEXIST") return false;
    throw error;
  } finally {
    rmSync(temp, { force: true });
  }
}
function readOwner(target) {
  let raw;
  try {
    raw = readFileSync(target, "utf8");
  } catch (error) {
    if (error.code === "ENOENT") return null;
    throw error;
  }
  try {
    const owner = JSON.parse(raw);
    if (Number.isSafeInteger(owner?.pid) && typeof owner.token === "string")
      return owner;
  } catch {}
  // Never reclaimed: an owner we cannot identify is treated as alive.
  return { pid: null, token: null };
}
function unlinkOwned(target, token) {
  if (readOwner(target)?.token !== token) return false;
  unlinkSync(target);
  return true;
}

// Removes a mutex whose owner pid is gone. Only the guard holder removes it,
// and only after re-reading the same token under the guard, so two reclaimers
// cannot both win and a mutex taken meanwhile is never removed. A guard whose
// own owner is gone is left for the operator (fail closed).
function reclaimMutex(paths, dead, entry) {
  const token = randomUUID();
  if (!linkOwner(paths.guard, { pid: process.pid, token, at: new Date().toISOString() })) {
    const holder = readOwner(paths.guard);
    if (holder && holder.pid !== null && pidGone(holder.pid)) {
      journal(paths, "guard-stale", entry, { file: paths.guard, guardPid: holder.pid });
      throw new HostLockError(
        "guard-stale",
        `Host lock reclaim guard ${paths.guard} was left by pid ${holder.pid}, which is gone; ` +
          "confirm no run is alive, then remove that file",
        EXIT_LOCK_UNUSABLE,
      );
    }
    return;
  }
  try {
    if (readOwner(paths.mutex)?.token === dead.token) {
      unlinkSync(paths.mutex);
      journal(paths, "mutex-recovered", entry, { deadPid: dead.pid, token: dead.token });
    }
  } finally {
    unlinkOwned(paths.guard, token);
  }
}
// One attempt. Returns the token, or null with busy.owner set to who holds it.
export function takeMutex(paths, entry = {}, busy = {}) {
  mkdirSync(dirname(paths.file), { recursive: true, mode: 0o700 });
  for (let attempt = 0; attempt < 2; attempt++) {
    const token = randomUUID();
    if (linkOwner(paths.mutex, { pid: process.pid, token, at: new Date().toISOString() }))
      return token;
    const owner = readOwner(paths.mutex);
    busy.owner = owner;
    if (!owner) continue;
    if (attempt || owner.pid === null || !pidGone(owner.pid)) return null;
    reclaimMutex(paths, owner, entry);
  }
  return null;
}
export const releaseMutex = (paths, token) => unlinkOwned(paths.mutex, token);

function corrupt(paths, entry, why) {
  journal(paths, "corrupt-file", entry, { file: paths.file, reason: why });
  return new HostLockError(
    "corrupt-file",
    `Host lock file ${paths.file} is unusable (${why}); it was left unchanged. ` +
      "Read it, confirm no run is alive, then move it aside",
    EXIT_LOCK_UNUSABLE,
  );
}
function parseState(raw) {
  let state;
  try {
    state = JSON.parse(raw);
  } catch {
    return "not JSON";
  }
  if (!state || typeof state !== "object" || Array.isArray(state)) return "not an object";
  if (state.version !== 1) return "unknown version";
  if (
    !Number.isSafeInteger(state.requestSeq) ||
    !Number.isSafeInteger(state.grantSeq) ||
    !Array.isArray(state.waiters) ||
    !(state.holder === null || (typeof state.holder === "object" && !Array.isArray(state.holder)))
  )
    return "missing fields";
  return state;
}
// A missing file is a free host; anything unreadable or unknown fails closed.
function readState(paths, entry) {
  let raw;
  try {
    raw = readFileSync(paths.file, "utf8");
  } catch (error) {
    if (error.code === "ENOENT")
      return { version: 1, host: hostname(), updatedAt: "", requestSeq: 0, grantSeq: 0, holder: null, waiters: [] };
    throw corrupt(paths, entry, error.code || "unreadable");
  }
  const state = parseState(raw);
  if (typeof state === "string") throw corrupt(paths, entry, state);
  return state;
}
function writeState(paths, state) {
  state.host = hostname();
  state.updatedAt = new Date().toISOString();
  const temp = `${paths.file}.${process.pid}.${randomUUID()}.tmp`;
  writeFileSync(temp, JSON.stringify(state, null, 2) + "\n", { mode: 0o600, flag: "wx" });
  renameSync(temp, paths.file);
}
// Reads without the mutex: updates land by rename, so a reader sees one whole
// version. status and other tools use this.
export function readHostState(file = lockPath()) {
  const paths = lockPaths(file);
  let raw;
  try {
    raw = readFileSync(paths.file, "utf8");
  } catch (error) {
    if (error.code === "ENOENT") return null;
    throw error;
  }
  const state = parseState(raw);
  if (typeof state === "string")
    throw new HostLockError("corrupt-file", `Host lock file ${paths.file} is unusable (${state})`, EXIT_LOCK_UNUSABLE);
  return state;
}

// One synchronous read-modify-write under the mutex. change returns false to
// leave the file untouched. Returns { done, value } or { done: false, owner }.
function updateOnce(paths, entry, change) {
  const busy = {};
  const token = takeMutex(paths, entry, busy);
  if (!token) return { done: false, owner: busy.owner };
  try {
    const state = readState(paths, entry);
    const value = change(state);
    if (value !== false) writeState(paths, state);
    return { done: true, value };
  } finally {
    releaseMutex(paths, token);
  }
}
// One attempt at a caller's own change; exported for tools and tests.
export const updateHostState = (file, change, entry = {}) =>
  updateOnce(lockPaths(file), entry, change);
async function update(paths, entry, change, { withinMs, signal } = {}) {
  const until = Date.now() + (withinMs ?? 0);
  for (;;) {
    const result = updateOnce(paths, entry, change);
    if (result.done || Date.now() >= until || signal?.aborted) return result;
    await sleep(5 + Math.random() * 20, signal);
  }
}
function updateSync(paths, entry, change, withinMs) {
  const until = Date.now() + withinMs;
  for (;;) {
    const result = updateOnce(paths, entry, change);
    if (result.done || Date.now() >= until) return result;
    sleepSync(5);
  }
}

const ordered = (waiters) =>
  waiters.sort((a, b) => rank(a.priority) - rank(b.priority) || a.seq - b.seq);
const describe = (entry) => `${entry.item}/${entry.agent}/pid ${entry.pid}`;
const liveGroups = (holder) => (holder.groups || []).filter((pgid) => !groupGone(pgid));

// Entries of this process still in the file, removed by the exit fallback.
const active = new Map();
let exitHooked = false;
function hookExit() {
  if (exitHooked) return;
  exitHooked = true;
  process.on("exit", () => {
    for (const [id, { paths, entry }] of active) {
      try {
        updateSync(
          paths,
          entry,
          (state) => {
            if (state.holder?.id === id) {
              state.holder = null;
              journal(paths, "release", entry, { fallback: true });
            } else if (state.waiters.some((w) => w.id === id)) {
              state.waiters = state.waiters.filter((w) => w.id !== id);
              journal(paths, "withdrawn", entry, { fallback: true });
            } else return false;
          },
          500,
        );
      } catch {}
    }
  });
}

// Requests the lock and resolves with a lease once it is granted. Options:
// path, priority, prioritySource, kind (run|targeted|exec), item, agent,
// commit, output (logged in the file), recordDirectory (host-lock.log and the
// host-lock.json sidecar), runTimeoutMs, maxWaitMs, pollMs, graceMs, signal,
// print, now, extra (copied into the sidecar), environment (the holder cap).
// The requester clamps its own run timeout to the holder cap, so the deadline
// it records in the file is the one it stops itself at. A waiter never
// shortens the deadline another holder recorded.
export async function acquireHostLock(options = {}) {
  const {
    kind = "run",
    item = "unknown",
    agent = "unknown",
    runTimeoutMs: requestedRunTimeoutMs,
    maxWaitMs = DEFAULT_HOST_WAIT_MS,
    pollMs = DEFAULT_POLL_MS,
    graceMs = RUN_TIMEOUT_GRACE_MS,
    busyAbortMs = HOLDER_BUSY_ABORT_MS,
    signal,
    print = () => {},
    now = Date.now,
    recordDirectory,
  } = options;
  const { priority, prioritySource } =
    options.priority && options.prioritySource
      ? { priority: resolvePriority(options.priority).priority, prioritySource: options.prioritySource }
      : resolvePriority(options.priority);
  if (!Number.isSafeInteger(requestedRunTimeoutMs) || requestedRunTimeoutMs <= 0)
    throw new Error("Host lock needs the run timeout in milliseconds");
  const runTimeoutMs = Math.min(requestedRunTimeoutMs, holderCapMs(options.environment ?? process.env));
  if (!Number.isSafeInteger(maxWaitMs) || maxWaitMs <= 0)
    throw new Error("Invalid host wait bound");
  const paths = lockPaths(options.path ?? lockPath());
  const requestedMs = now();
  const entry = {
    id: randomUUID(),
    seq: 0,
    pid: process.pid,
    kind,
    item,
    agent,
    priority,
    prioritySource,
    requestedAt: iso(requestedMs),
    grantSeqAtRequest: 0,
    overtakenBy: 0,
    runTimeoutMs,
    ...(options.commit ? { commit: options.commit } : {}),
    ...(options.output ? { output: options.output } : {}),
  };
  const record = {
    version: 1,
    lockPath: paths.file,
    id: entry.id,
    seq: 0,
    pid: process.pid,
    kind,
    item,
    agent,
    priority,
    prioritySource,
    requestedAt: entry.requestedAt,
    acquiredAt: null,
    releasedAt: null,
    outcome: "waiting",
    waitMs: 0,
    queuePosition: 0,
    queueLength: 0,
    grantsBeforeStart: 0,
    overtakenBy: 0,
    overlap: 0,
    runTimeoutMs,
    requestedRunTimeoutMs,
    cpuCount: availableParallelism(),
    loadSamples: [],
    ...(options.extra || {}),
  };
  if (recordDirectory) mkdirSync(recordDirectory, { recursive: true });
  const say = (line) => {
    print(line);
    if (recordDirectory)
      appendFileSync(join(recordDirectory, "host-lock.log"), new Date().toISOString() + " " + line + "\n", { mode: 0o600 });
  };
  const saveRecord = () => {
    if (recordDirectory)
      writeFileSync(join(recordDirectory, "host-lock.json"), JSON.stringify(record, null, 2) + "\n", { mode: 0o600 });
  };

  // Grants the lock to entry when it is first and nothing of the previous
  // holder is alive, or that holder is past its run timeout plus grace.
  // Returns true when granted. No run is ever signalled from here.
  let shown = "",
    shownAt = 0,
    busyAt = 0;
  const step = (state, first) => {
    if (first) {
      entry.seq = ++state.requestSeq;
      entry.grantSeqAtRequest = state.grantSeq;
      state.waiters.push({ ...entry });
      journal(paths, "request", entry, { seq: entry.seq, priority, prioritySource, kind });
    }
    const dead = state.waiters.filter((w) => w.id !== entry.id && pidGone(w.pid));
    for (const waiter of dead) journal(paths, "waiter-dropped", waiter, { by: entry.id });
    state.waiters = ordered(state.waiters.filter((w) => !dead.includes(w)));
    const mine = state.waiters.findIndex((w) => w.id === entry.id);
    if (mine < 0)
      throw new HostLockError("lost-request", `Host lock request ${entry.id} is no longer in ${paths.file}`);
    const position = mine + 1,
      length = state.waiters.length,
      holder = state.holder;
    if (first) Object.assign(record, { seq: entry.seq, queuePosition: position, queueLength: length });
    let line = `matrix host: waiting ${position} of ${length}, holder none`,
      overlap = null,
      recovered = null;
    if (holder) {
      const holderGone = pidGone(holder.pid),
        groups = liveGroups(holder);
      const overdue = now() > Date.parse(holder.startedAt) + holder.runTimeoutMs + graceMs;
      if (holderGone && !groups.length) recovered = holder;
      else if (overdue)
        overlap = { reason: "run-timeout", holder: holder.id, item: holder.item, agent: holder.agent, pid: holderGone ? null : holder.pid, groups };
      line =
        `matrix host: waiting ${position} of ${length}, holder ${describe(holder)}` +
        (holderGone && groups.length ? ` gone, check group ${groups.join(",")} still running` : "");
    }
    if (mine === 0 && (!holder || recovered || overlap)) {
      if (recovered)
        journal(paths, "stale-recovered", entry, { reason: "pid-gone", holder: recovered.id, holderPid: recovered.pid, holderItem: recovered.item, holderAgent: recovered.agent });
      if (overlap) journal(paths, "overlap", entry, overlap);
      const queued = state.waiters.shift();
      for (const waiter of state.waiters) if (waiter.seq < queued.seq) waiter.overtakenBy = (waiter.overtakenBy || 0) + 1;
      const acquiredMs = now();
      Object.assign(record, {
        acquiredAt: iso(acquiredMs),
        outcome: "holding",
        waitMs: Math.max(0, acquiredMs - requestedMs),
        grantsBeforeStart: state.grantSeq - queued.grantSeqAtRequest,
        overtakenBy: queued.overtakenBy || 0,
        overlap: overlap ? 1 : 0,
        ...(overlap ? { overlapDetails: overlap } : {}),
        ...(recovered ? { recovered: { reason: "pid-gone", holder: recovered.id, pid: recovered.pid } } : {}),
      });
      if (first && !holder) Object.assign(record, { queuePosition: 0, queueLength: 0 });
      state.grantSeq += 1;
      const { grantSeqAtRequest, overtakenBy, ...held } = queued;
      state.holder = { ...held, startedAt: record.acquiredAt, groups: [] };
      journal(paths, "acquire", entry, { seq: entry.seq, waitMs: record.waitMs, overlap: record.overlap });
      return { granted: true, overlap };
    }
    return { granted: false, line, position, length, holder };
  };
  // Leaves the waitlist. If the file cannot be updated now, the entry stays
  // for the exit fallback, and is dropped by others once this process is gone.
  const leave = (event, extra) => {
    let removed = false;
    try {
      removed = updateSync(
        paths,
        entry,
        (state) => {
          if (!state.waiters.some((w) => w.id === entry.id)) return false;
          state.waiters = state.waiters.filter((w) => w.id !== entry.id);
        },
        2000,
      ).done;
    } catch {}
    if (removed) active.delete(entry.id);
    Object.assign(record, { outcome: event, releasedAt: iso(now()), waitMs: Math.max(0, now() - requestedMs) });
    journal(paths, event, entry, { waitMs: record.waitMs, removed, ...extra });
    saveRecord();
  };

  if (runTimeoutMs < requestedRunTimeoutMs)
    say(`matrix host: run timeout ${requestedRunTimeoutMs} ms clamped to the holder cap of ${runTimeoutMs} ms`);
  hookExit();
  let first = true,
    last = null;
  for (;;) {
    let result;
    try {
      result = await update(paths, entry, (state) => step(state, first), { withinMs: Math.min(pollMs, 1000), signal });
    } catch (error) {
      if (!first) {
        active.delete(entry.id);
        Object.assign(record, { outcome: error.code || "failed", releasedAt: iso(now()) });
        saveRecord();
      }
      throw error;
    }
    if (result.done) {
      if (first) active.set(entry.id, { paths, entry });
      first = false;
      if (result.value.granted) {
        if (result.value.overlap)
          say(
            `matrix host: WARNING overlap, lock taken after run timeout from ${result.value.overlap.item}/${result.value.overlap.agent}` +
              ` (pid ${result.value.overlap.pid ?? "gone"}, check groups ${result.value.overlap.groups.join(",") || "none"} still alive)`,
          );
        say(`matrix host: acquired after ${record.waitMs} ms`);
        break;
      }
      last = result.value;
      if (last.line !== shown || Date.now() - shownAt >= REPEAT_LINE_MS) {
        shown = last.line;
        shownAt = Date.now();
        say(last.line);
      }
    } else if (Date.now() - busyAt >= REPEAT_LINE_MS) {
      busyAt = Date.now();
      say(`matrix host: lock file busy, mutex owner pid ${result.owner?.pid ?? "unknown"}`);
    }
    if (signal?.aborted) {
      if (!first) leave("withdrawn", { reason: String(signal.reason) });
      throw new HostLockError("withdrawn", "Verification interrupted by " + signal.reason + " while waiting for the host lock");
    }
    if (now() - requestedMs >= maxWaitMs) {
      if (!first) leave("wait-expired", last ? { position: last.position, of: last.length } : {});
      throw new HostLockError(
        "wait-expired",
        `Host lock wait expired after ${maxWaitMs} ms` +
          (last ? ` at position ${last.position} of ${last.length}, holder ${last.holder ? describe(last.holder) : "none"}` : ", lock file busy"),
        EXIT_WAIT_EXPIRED,
      );
    }
    await sleep(pollMs, signal);
  }

  // Holding. The holder stops itself at its run timeout through its caller's
  // abort path, records its live check groups, and samples host load.
  const acquiredMs = Date.parse(record.acquiredAt),
    started = performance.now();
  const stop = new AbortController();
  let released = false,
    final = null;
  const sample = () => {
    const [load1, load5] = loadavg();
    record.loadSamples.push({ at: new Date().toISOString(), load1: Number(load1.toFixed(2)), load5: Number(load5.toFixed(2)) });
    if (record.loadSamples.length > LOAD_SAMPLES_KEPT) record.loadSamples.shift();
  };
  sample();
  const sampler = setInterval(sample, LOAD_SAMPLE_MS);
  sampler.unref();
  const selfTimeout = setTimeout(() => {
    if (released) return;
    journal(paths, "run-timeout-abort", entry, { runTimeoutMs });
    say(`matrix host: run timeout of ${runTimeoutMs} ms reached, stopping this run`);
    record.runTimeoutAbort = true;
    stop.abort("run-timeout");
  }, runTimeoutMs);
  selfTimeout.unref();

  const groups = new Set();
  let flushing = null,
    dirty = false,
    warned = false;
  const writeGroups = (state) => {
    if (state.holder?.id !== entry.id) return false;
    state.holder.groups = [...groups];
  };
  const flush = async () => {
    while (dirty && !released) {
      dirty = false;
      try {
        const result = await update(paths, entry, writeGroups, { withinMs: busyAbortMs });
        if (!result.done && !released) {
          journal(paths, "holder-update-failed", entry, { mutexPid: result.owner?.pid ?? null });
          say(`matrix host: lock file busy for ${busyAbortMs} ms (mutex owner pid ${result.owner?.pid ?? "unknown"}), stopping this run`);
          stop.abort("host-lock-busy");
        }
      } catch (error) {
        if (!warned) say("matrix host: WARNING " + error.message);
        warned = true;
      }
    }
    flushing = null;
  };
  const setGroup = (pgid, present) => {
    if (released || !Number.isSafeInteger(pgid) || pgid <= 1) return;
    if (present) groups.add(pgid);
    else groups.delete(pgid);
    // Usually lands at once, so a check group is on file as it starts.
    try {
      if (!flushing && updateOnce(paths, entry, writeGroups).done) return;
    } catch {}
    dirty = true;
    flushing ||= flush();
  };

  // Ends sampling and fixes the values a receipt records, so the sidecar
  // written at release carries the same numbers.
  const finish = (extra = {}) => {
    if (!final) {
      clearInterval(sampler);
      sample();
      final = { durationMs: Math.round(performance.now() - started) };
    }
    Object.assign(record, final, extra);
    return record;
  };
  const release = async (extra = {}) => {
    if (released) return record;
    finish(extra);
    released = true;
    clearTimeout(selfTimeout);
    if (flushing) await flushing;
    let held = false;
    try {
      const result = await update(
        paths,
        entry,
        (state) => {
          if (state.holder?.id !== entry.id) return false;
          state.holder = null;
          held = true;
        },
        { withinMs: busyAbortMs },
      );
      journal(paths, "release", entry, { held, updated: result.done, waitMs: record.waitMs, durationMs: record.durationMs });
      if (!result.done) say(`matrix host: WARNING could not update ${paths.file} at release; the entry is recovered once this process is gone`);
    } catch (error) {
      say("matrix host: WARNING " + error.message);
      record.releaseError = error.message;
    }
    active.delete(entry.id);
    Object.assign(record, { releasedAt: new Date().toISOString(), outcome: "released" });
    saveRecord();
    return record;
  };
  saveRecord();
  return {
    id: entry.id,
    path: paths.file,
    record,
    acquiredMs,
    signal: stop.signal,
    addGroup: (pgid) => setGroup(pgid, true),
    removeGroup: (pgid) => setGroup(pgid, false),
    finish,
    release,
  };
}

// The twelve receipt keys, all strings, from a finished record.
export function receiptKeys(record) {
  return {
    VERIFICATION_HOST_LOCK: record.lockPath,
    VERIFICATION_HOST_PRIORITY: record.priority,
    VERIFICATION_HOST_PRIORITY_SOURCE: record.prioritySource,
    VERIFICATION_HOST_WAIT_MS: String(record.waitMs),
    VERIFICATION_HOST_QUEUE_POSITION: String(record.queuePosition),
    VERIFICATION_HOST_QUEUE_LENGTH: String(record.queueLength),
    VERIFICATION_HOST_GRANTS_BEFORE_START: String(record.grantsBeforeStart),
    VERIFICATION_HOST_OVERTAKEN_BY: String(record.overtakenBy),
    VERIFICATION_HOST_OVERLAP: String(record.overlap),
    VERIFICATION_CHECK_SET: String(record.checkSet),
    VERIFICATION_RUN_DURATION_MS: String(record.durationMs),
    VERIFICATION_HOST_LOAD: record.loadSamples.map((s) => String(s.load1)).join(","),
  };
}

// Runs one supplemental command (a race suite, say) while holding the lock.
// The command gets its own process group, recorded as the holder's group, and
// is stopped at the run timeout. Resolves with the exit code to pass through.
export async function execWithHostLock(argv, options = {}) {
  if (!Array.isArray(argv) || !argv.length) throw new Error("exec needs a command");
  const interrupted = new AbortController();
  let interruptedBy = "";
  const onSignal = (name) => {
    interruptedBy ||= name;
    interrupted.abort(interruptedBy);
  };
  const handlers = ["SIGINT", "SIGTERM"].map((name) => [name, () => onSignal(name)]);
  if (options.handleSignals !== false) for (const [name, handler] of handlers) process.on(name, handler);
  let lease;
  try {
    lease = await acquireHostLock({
      ...options,
      kind: "exec",
      signal: interrupted.signal,
      extra: { checkSet: "supplemental", argv },
    });
    const code = await new Promise((done, fail) => {
      const child = spawn(argv[0], argv.slice(1), { detached: true, stdio: options.stdio || "inherit" });
      let killTimer,
        reason = "";
      const signalGroup = (name) => {
        try {
          process.kill(-child.pid, name);
        } catch {}
      };
      const stop = (why, name) => {
        reason ||= why;
        signalGroup(name);
        killTimer ||= setTimeout(() => signalGroup("SIGKILL"), options.killAfterMs ?? 2000);
      };
      const onInterrupt = () => stop("interrupted", interruptedBy || "SIGTERM");
      const onTimeout = () => stop("timeout", "SIGTERM");
      child.on("error", (error) => {
        clearTimeout(killTimer);
        fail(error);
      });
      child.on("spawn", () => {
        lease.addGroup(child.pid);
        interrupted.signal.addEventListener("abort", onInterrupt, { once: true });
        lease.signal.addEventListener("abort", onTimeout, { once: true });
        if (interrupted.signal.aborted) onInterrupt();
        if (lease.signal.aborted) onTimeout();
      });
      child.on("close", (status, signal) => {
        clearTimeout(killTimer);
        lease.removeGroup(child.pid);
        done(
          reason === "timeout"
            ? 124
            : reason === "interrupted"
              ? interruptedBy === "SIGINT"
                ? 130
                : 143
              : (status ?? (signal === "SIGINT" ? 130 : signal === "SIGTERM" ? 143 : 1)),
        );
      });
    });
    await lease.release({ exitCode: code });
    return code;
  } catch (error) {
    if (lease) await lease.release({ error: error.message });
    if (error.code === "withdrawn") return interruptedBy === "SIGINT" ? 130 : 143;
    throw error;
  } finally {
    for (const [name, handler] of handlers) process.removeListener(name, handler);
  }
}

export function statusText(file = lockPath()) {
  const state = readHostState(file);
  const lines = ["matrix host lock: " + file];
  const tags = (e) => `${e.kind}, ${e.priority}/${e.prioritySource}`;
  if (!state?.holder) lines.push("holder: none");
  else {
    const h = state.holder,
      live = liveGroups(h);
    lines.push(
      `holder: ${describe(h)}${pidGone(h.pid) ? " gone" : ""} (${tags(h)}), started ${h.startedAt}, ` +
        `run timeout ${h.runTimeoutMs} ms, check groups ${(h.groups || []).join(",") || "none"} (alive: ${live.join(",") || "none"})`,
    );
  }
  const waiters = state?.waiters || [];
  lines.push(waiters.length ? "waiters:" : "waiters: none");
  waiters.forEach((w, i) =>
    lines.push(`  ${i + 1} of ${waiters.length}: ${describe(w)} (${tags(w)}), requested ${w.requestedAt}, overtaken by ${w.overtakenBy || 0}`),
  );
  return lines.join("\n");
}

const USAGE =
  "Usage: node scripts/verify-matrix-host-lock.mjs status [--json]\n" +
  "       node scripts/verify-matrix-host-lock.mjs exec --item ID --timeout-minutes N --record /abs/dir\n" +
  "            [--priority urgent|high|normal] [--agent NAME] [--host-wait-minutes N] -- COMMAND [ARGS...]";
export function minutesFlag(name, value, max) {
  if (!/^[1-9][0-9]{0,3}$/.test(value || "") || Number(value) > max)
    throw new Error(`${name} requires a whole number of minutes from 1 to ${max}`);
  return Number(value) * 60000;
}
async function main(args) {
  const [command, ...rest] = args;
  if (command === "status") {
    if (rest.some((flag) => flag !== "--json")) throw new Error(USAGE);
    const file = lockPath();
    console.log(
      rest.includes("--json")
        ? JSON.stringify({ path: file, state: readHostState(file) }, null, 2)
        : statusText(file),
    );
    return 0;
  }
  if (command !== "exec") throw new Error(USAGE);
  const split = rest.indexOf("--");
  if (split < 0 || split === rest.length - 1) throw new Error(USAGE);
  const flags = rest.slice(0, split),
    argv = rest.slice(split + 1);
  const given = {};
  for (let i = 0; i < flags.length; i += 2) {
    if (!["--item", "--timeout-minutes", "--record", "--priority", "--agent", "--host-wait-minutes"].includes(flags[i]) || flags[i + 1] === undefined)
      throw new Error("Unknown or incomplete exec option: " + flags[i] + "\n" + USAGE);
    given[flags[i]] = flags[i + 1];
  }
  if (!given["--item"]) throw new Error("exec requires --item ID");
  if (!given["--record"] || !isAbsolute(given["--record"])) throw new Error("exec requires --record with an absolute directory");
  if (given["--timeout-minutes"] === undefined) throw new Error("exec requires --timeout-minutes N");
  return execWithHostLock(argv, {
    ...resolvePriority(given["--priority"]),
    item: given["--item"],
    agent: given["--agent"] || process.env.TAILTERM_AGENT_NAME || "unknown",
    runTimeoutMs: minutesFlag("--timeout-minutes", given["--timeout-minutes"], 1440),
    maxWaitMs: given["--host-wait-minutes"] === undefined ? DEFAULT_HOST_WAIT_MS : minutesFlag("--host-wait-minutes", given["--host-wait-minutes"], 1440),
    output: given["--record"],
    recordDirectory: given["--record"],
    print: (line) => console.log(line),
  });
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url))
  main(process.argv.slice(2)).then(
    (code) => {
      process.exitCode = code;
    },
    (error) => {
      console.error(error.message);
      process.exitCode = error.exitCode ?? 1;
    },
  );
