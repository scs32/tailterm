import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { chmodSync, mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { probe, hostDeps, waitForTailOSCommit } from "../scripts/release-probe.mjs";
import { readyWindow, waitForTrueNASReady, sanitizeCapture, TRUENAS_READY_WINDOW_MS } from "../scripts/release-probe.mjs";

const BASE = "/mnt/deepfreeze/tailterm-hub", SECRET = "SYNTHETIC_PRIVATE_TOKEN";
const commit = "a".repeat(40), bytes = Buffer.from("hub binary"), sha = createHash("sha256").update(bytes).digest("hex");
const compose = (hubRelease = "rel_x-aaaaaaaaaaaa-hub", bridgeRelease = "20260929-live") => ({ services: {
  hub: { volumes: [`${BASE}/releases/${hubRelease}/tailterm-hub:/opt/tailterm-hub:ro`, `${BASE}/state:/state`, `${BASE}/hub-token:/run/hub-token:ro`], environment: { TOKEN: SECRET } },
  "discord-bridge": { volumes: [`${BASE}/releases/${bridgeRelease}/tailterm-discord:/opt/tailterm-discord:ro`, `${BASE}/discord-token:/run/discord-token:ro`] },
} });
const ids = { hub: "1".repeat(64), "discord-bridge": "2".repeat(64), worker: "3".repeat(64), cron: "4".repeat(64) };
const up = [{ service_name: "hub", state: "running", id: ids.hub }, { service_name: "discord-bridge", state: "running", id: ids["discord-bridge"] }];
// `polls` scripts each readiness read in turn (the last entry repeats): an
// entry overrides state, containers, hub (the tt exit status) or instance
// (the app.get_instance answer). sleep only advances the fake clock. `logs`
// answers each `midclt subscribe` log read and may advance the clock. With
// `slow`, every command takes 1 ms less than the run timeout it was given
// (120 s, the default, when it was given none).
const logLine = data => JSON.stringify({ msg: "added", collection: "app.container_log_follow", fields: { data, timestamp: "2026-10-01T00:00:00Z" } });
const subscribe = id => `midclt subscribe -n 40 -t 8 'app.container_log_follow:{"app_name":"tailterm-hub","container_id":"${id}","tail_lines":40}'`;
const logReads = deps => deps.runs.filter(r => String(r.argv.at(-1)).startsWith("midclt subscribe "));
function fakeHost({ config = compose(), modified = "false", state = "RUNNING", containers = up, hub = 0, relay = "\tstate = running", polls = [{}], logs = () => ({ status: 0, stdout: logLine("listening on :8080") + "\n" }), slow = false } = {}) {
  const calls = [], runs = [], sleeps = [];
  let t = 0, instanceReads = 0, hubReads = 0, logReadCount = 0;
  const poll = n => ({ state, containers, hub, ...polls[Math.min(n, polls.length - 1)] });
  return { calls, runs, sleeps, clock: () => t, uid: () => 501, fetchJSON: async () => ({ commit }), now: () => t, sleep: async ms => { sleeps.push(ms); t += ms; }, run: (argv, opts = {}) => {
    calls.push(argv); runs.push({ argv, opts });
    if (slow) t += (opts.timeout ?? 120000) - 1;
    const cmd = argv.at(-1);
    if (argv[0] === "ssh") {
      assert.deepEqual(argv.slice(0, 6), ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "truenas"]);
      if (cmd === "midclt call app.config tailterm-hub") return { status: 0, stdout: JSON.stringify(config) };
      if (cmd === "midclt call app.get_instance tailterm-hub") { const p = poll(instanceReads++); if (p.instance) return typeof p.instance === "function" ? p.instance(opts) : p.instance; return { status: 0, stdout: JSON.stringify({ state: p.state, active_workloads: { container_details: p.containers } }) }; }
      if (cmd.startsWith("cat ")) { assert.ok(opts.buffer); return { status: 0, stdout: bytes }; }
      if (cmd.startsWith("midclt subscribe ")) { const id = Object.values(ids).find(i => cmd === subscribe(i)); assert.ok(id, "log read for a known 64-hex id only"); return logs({ id, n: logReadCount++, advance: ms => { t += ms; } }); }
    }
    if (argv[0] === "go") return { status: 0, stdout: `${argv[3]}: go1.25\n\tbuild\tvcs.revision=${commit}\n\tbuild\tvcs.modified=${modified}\n\tbuild\t${SECRET}=1\n` };
    if (argv[1] === "projects") return { status: poll(hubReads++).hub, stdout: SECRET };
    if (argv[0] === "launchctl") return { status: 0, stdout: `${relay}\n${SECRET}` };
    return { status: 1, stdout: SECRET };
  } };
}
const config = (extra = {}) => ({ tt: "/opt/tt", targets: { hub: { host: "truenas" }, bridge: { host: "truenas" }, ...extra } });
// One readiness read, as before the window existed.
const windowed = (hubMs, bridgeMs = hubMs) => config({ hub: { host: "truenas", readyWindowMs: hubMs }, bridge: { host: "truenas", readyWindowMs: bridgeMs } });
const once = windowed(0);
const count = (deps, match) => deps.calls.filter(a => match(String(a.at(-1)), a)).length;
const identityReads = deps => [count(deps, c => c === "midclt call app.config tailterm-hub"), count(deps, c => c.startsWith("cat ")), count(deps, (c, a) => a[0] === "go")];
const readyReads = deps => [count(deps, c => c === "midclt call app.get_instance tailterm-hub"), count(deps, (c, a) => a[1] === "projects")];

test("live hub probe reads the mounted release over the truenas route and prints only the checked fields", async () => {
  const deps = fakeHost(), out = await probe(["live", "hub"], config(), deps);
  assert.deepEqual(out, { commit, artifactSHA256: sha, integrity: true, hubResponds: true, migrationsApplied: true, containersRunning: true, release: "rel_x-aaaaaaaaaaaa-hub", waitedMs: 0, polls: 1 });
  assert.ok(!JSON.stringify(out).includes(SECRET));
  assert.ok(deps.calls.some(a => a.at(-1) === `cat ${BASE}/releases/rel_x-aaaaaaaaaaaa-hub/tailterm-hub`));
  assert.deepEqual(deps.calls.find(a => a[1] === "projects"), ["/opt/tt", "projects"]);
  assert.equal((await probe(["live", "bridge"], config(), fakeHost())).release, "20260929-live");
});

test("live hub probe reports a dirty build, a stopped container and an unreachable hub", async () => {
  assert.equal((await probe(["live", "hub"], once, fakeHost({ modified: "true" }))).integrity, false);
  assert.equal((await probe(["live", "hub"], once, fakeHost({ state: "STOPPED" }))).containersRunning, false);
  assert.equal((await probe(["live", "bridge"], once, fakeHost({ containers: [{ service_name: "hub", state: "running" }, { service_name: "discord-bridge", state: "exited" }] }))).containersRunning, false);
  const down = await probe(["live", "hub"], once, fakeHost({ hub: 1 }));
  assert.equal(down.hubResponds, false); assert.equal(down.migrationsApplied, false);
  await assert.rejects(probe(["live", "hub"], config(), fakeHost({ config: { services: { hub: { volumes: ["/tmp/x:/opt/tailterm-hub:ro"] } } } })), /release mount/);
  await assert.rejects(probe(["live", "hub"], { targets: { hub: { host: "truenas; rm -rf /" } } }, fakeHost()), /host reference/);
});

test("rollback probe for hub and bridge requires the expected retained release and hash", async () => {
  const good = ["rollback", "hub", "--expect-release", "rel_x-aaaaaaaaaaaa-hub", "--expect-sha", sha];
  assert.deepEqual(await probe(good, once, fakeHost()), { restored: true, databaseWritesPreserved: true, waitedMs: 0, polls: 1 });
  assert.equal((await probe(["rollback", "hub", "--expect-release", "other", "--expect-sha", sha], once, fakeHost())).restored, false);
  assert.equal((await probe([...good.slice(0, 4), "--expect-sha", "b".repeat(64)], once, fakeHost())).restored, false);
  const moved = compose(); moved.services.hub.volumes[1] = "/tmp/elsewhere:/state";
  assert.equal((await probe(good, once, fakeHost({ config: moved }))).databaseWritesPreserved, false);
  await assert.rejects(probe(["rollback", "hub", "--expect-release", "x"], once, fakeHost()), /expected release/);
});

const down = { hub: 1 }, starting = { state: "DEPLOYING", containers: [{ service_name: "hub", state: "starting", id: ids.hub }], hub: 1 };

test("H1 hub and bridge live probes wait until the containers run and the hub responds", async () => {
  for (const target of ["hub", "bridge"]) {
    // Poll 1: app still deploying. 2: containers up, hub still migrating.
    // 3: hub answers but the bridge container has not started. 4: all up.
    const bridgeLate = { containers: [up[0], { ...up[1], state: "starting" }], hub: target === "bridge" ? 0 : 1 };
    const deps = fakeHost({ polls: [starting, down, bridgeLate, {}] }), out = await probe(["live", target], config(), deps);
    assert.deepEqual(out, { commit, artifactSHA256: sha, integrity: true, hubResponds: true, migrationsApplied: true, containersRunning: true, release: target === "hub" ? "rel_x-aaaaaaaaaaaa-hub" : "20260929-live", waitedMs: 15000, polls: 4 });
    assert.deepEqual(deps.sleeps, [5000, 5000, 5000]);
    assert.ok(!("capture" in out));
    // Identity is read once; only the two readiness reads repeat.
    assert.deepEqual(identityReads(deps), [1, 1, 1]);
    assert.deepEqual(readyReads(deps), [4, 4]);
    for (const r of deps.runs.filter(r => r.argv.at(-1) === "midclt call app.get_instance tailterm-hub" || r.argv[1] === "projects")) assert.equal(r.opts.timeout, 20000);
  }
});

test("H2 a hub that never comes up fails only when the default window ends", async () => {
  assert.equal(TRUENAS_READY_WINDOW_MS, 240000);
  const deps = fakeHost({ hub: 1 }), out = await probe(["live", "hub"], config(), deps);
  assert.equal(out.hubResponds, false); assert.equal(out.migrationsApplied, false); assert.equal(out.containersRunning, true);
  assert.equal(out.waitedMs, 240000); assert.equal(out.polls, 49);
  assert.equal(deps.sleeps.length, 48); assert.ok(deps.sleeps.every(ms => ms === 5000));
  assert.equal(deps.sleeps.reduce((a, b) => a + b, 0), 240000);
  assert.deepEqual(identityReads(deps), [1, 1, 1]);
  assert.deepEqual(out.capture, { app: { state: "RUNNING", containers: [{ service: "hub", state: "running", id: "1".repeat(12) }, { service: "discord-bridge", state: "running", id: "2".repeat(12) }] }, logs: [{ service: "hub", lines: ["listening on :8080"] }, { service: "discord-bridge", lines: ["listening on :8080"] }] });
  // An app.get_instance read that fails, times out, is not JSON or throws is
  // "not ready", not a probe failure.
  const thrower = { get status() { throw new Error(SECRET); } };
  const flaky = fakeHost({ polls: [{ instance: { status: 1, stdout: SECRET } }, { instance: { status: null, stdout: "" } }, { instance: { status: 0, stdout: "<html>" } }, { instance: thrower }, {}] });
  const late = await probe(["live", "hub"], config(), flaky);
  assert.equal(late.containersRunning, true); assert.equal(late.hubResponds, true); assert.equal(late.polls, 5); assert.equal(late.waitedMs, 20000);
  const never = await probe(["live", "bridge"], windowed(10000), fakeHost({ polls: [{ instance: { status: 1, stdout: "" } }] }));
  assert.equal(never.containersRunning, false); assert.equal(never.hubResponds, true); assert.equal(never.waitedMs, 10000);
  assert.deepEqual(never.capture, { app: "unavailable", logs: [] });
  assert.ok(!JSON.stringify([out, late, never]).includes("SYNTHETIC"));
});

test("H3 the readiness window is configurable per target and validated", async () => {
  const one = fakeHost({ hub: 1 }), single = await probe(["live", "hub"], once, one);
  assert.equal(single.hubResponds, false); assert.equal(single.waitedMs, 0); assert.equal(single.polls, 1);
  assert.deepEqual(one.sleeps, []); assert.deepEqual(readyReads(one), [1, 1]);
  const ten = fakeHost({ hub: 1 }), short = await probe(["live", "hub"], windowed(10000), ten);
  assert.equal(short.waitedMs, 10000); assert.equal(short.polls, 3); assert.deepEqual(ten.sleeps, [5000, 5000]);
  // A window that is not a multiple of the interval ends exactly on time.
  const twelve = fakeHost({ hub: 1 });
  assert.equal((await probe(["live", "hub"], windowed(12000), twelve)).waitedMs, 12000); assert.deepEqual(twelve.sleeps, [5000, 5000, 2000]);
  // Each target reads its own key.
  const split = windowed(0, 10000);
  assert.equal((await probe(["live", "bridge"], split, fakeHost({ hub: 1 }))).polls, 3);
  assert.equal((await probe(["live", "hub"], split, fakeHost({ hub: 1 }))).polls, 1);
  assert.equal(readyWindow(config(), "hub"), 240000); assert.equal(readyWindow(split, "bridge"), 10000); assert.equal(readyWindow(windowed(300000), "hub"), 300000);
  const good = ["rollback", "hub", "--expect-release", "rel_x-aaaaaaaaaaaa-hub", "--expect-sha", sha];
  for (const bad of ["240000", -1, 300001, 1.5, null]) {
    for (const argv of [["live", "hub"], ["live", "bridge"], good]) {
      const deps = fakeHost();
      await assert.rejects(probe(argv, windowed(bad), deps), /readiness window/);
      // Refused before any host command runs.
      assert.equal(deps.calls.length, 0);
    }
    await assert.rejects(waitForTrueNASReady("hub", windowed(bad), fakeHost()), /readiness window/);
  }
});

test("H4 the rollback probe waits the same window before it reports not restored", async () => {
  const good = ["rollback", "hub", "--expect-release", "rel_x-aaaaaaaaaaaa-hub", "--expect-sha", sha];
  const late = fakeHost({ polls: [starting, down, down, {}] });
  assert.deepEqual(await probe(good, config(), late), { restored: true, databaseWritesPreserved: true, waitedMs: 15000, polls: 4 });
  assert.deepEqual(late.sleeps, [5000, 5000, 5000]); assert.deepEqual(identityReads(late), [1, 1, 1]); assert.equal(logReads(late).length, 0);
  const bridge = ["rollback", "bridge", "--expect-release", "20260929-live", "--expect-sha", sha];
  assert.deepEqual(await probe(bridge, config(), fakeHost({ polls: [starting, down, down, {}] })), { restored: true, databaseWritesPreserved: true, waitedMs: 15000, polls: 4 });
  // Never up: both verdict shapes end false only after the whole window.
  const hubDown = fakeHost({ hub: 1 }), noHub = await probe(good, config(), hubDown);
  assert.equal(noHub.databaseWritesPreserved, false); assert.equal(noHub.waitedMs, 240000); assert.equal(noHub.polls, 49);
  assert.equal(hubDown.sleeps.reduce((a, b) => a + b, 0), 240000);
  assert.deepEqual(Object.keys(noHub.capture), ["app", "logs"]); assert.equal(noHub.capture.app.state, "RUNNING");
  const stopped = await probe(good, config(), fakeHost({ state: "STOPPED", containers: [], hub: 1 }));
  assert.deepEqual({ restored: stopped.restored, databaseWritesPreserved: stopped.databaseWritesPreserved, waitedMs: stopped.waitedMs }, { restored: false, databaseWritesPreserved: false, waitedMs: 240000 });
  assert.deepEqual(stopped.capture, { app: { state: "STOPPED", containers: [] }, logs: [] });
  // A wrong release, hash, build or state mount cannot be waited into place:
  // one readiness read, no sleep, even while the app is still down.
  const moved = compose(); moved.services.hub.volumes[1] = "/tmp/elsewhere:/state";
  for (const [argv, host] of [[["rollback", "hub", "--expect-release", "other", "--expect-sha", sha], {}], [[...good.slice(0, 4), "--expect-sha", "b".repeat(64)], {}], [good, { modified: "true" }], [good, { config: moved }]]) {
    const deps = fakeHost({ ...host, hub: 1 }), out = await probe(argv, config(), deps);
    assert.equal(out.restored && out.databaseWritesPreserved, false); assert.equal(out.waitedMs, 0); assert.equal(out.polls, 1);
    assert.deepEqual(deps.sleeps, []); assert.deepEqual(readyReads(deps), [1, 1]);
    const healthy = fakeHost(host), wrong = await probe(argv, config(), healthy);
    assert.equal(wrong.restored && wrong.databaseWritesPreserved, false); assert.ok(!("capture" in wrong)); assert.deepEqual(healthy.sleeps, []);
  }
});

test("H5 a not-ready end captures allowlisted app state and redacted log lines, and no capture failure changes the verdict", async () => {
  const opaque = "SYNTHETICa1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6", listSecret = "SYNTHETIC_LIST_VALUE";
  const secretCompose = compose(); secretCompose.services["discord-bridge"].environment = [`DISCORD_TOKEN=${listSecret}`, "MODE=prod"];
  const noisy = { status: 0, stdout: JSON.stringify({ state: "RUNNING", config: { hubToken: SECRET }, notes: `notes ${SECRET}`, portals: { web: `http://x/?t=${SECRET}` }, metadata: { [SECRET]: 1 },
    active_workloads: { container_details: [{ service_name: "hub", state: "running", id: ids.hub, image: SECRET, volume_mounts: [SECRET], port_config: [{ secret: SECRET }] }, { service_name: "discord-bridge", state: "exited", id: ids["discord-bridge"] }], volumes: [SECRET] } }) };
  const hubLines = [...Array.from({ length: 30 }, (_, i) => `old line ${i}`), `env value ${SECRET} and ${listSecret} here`, "token=SYNTHETIC-short", "Authorization: Bearer SYNTHETIC.bearer", `id ${opaque} end`, JSON.stringify({ level: "error", token: "SYNTHETIC-json" }), "\u001b[31mpanic: SYNTHETIC_PRIVATE_TOKEN\u001b[0m\r", "x".repeat(5000), "migration 41 applied", ...Array.from({ length: 21 }, (_, i) => `new line ${i}`)];
  const answer = ({ id }) => id === ids.hub ? { status: 0, stdout: hubLines.map(logLine).join("\n") + "\nnot json\n" + JSON.stringify({ fields: { data: 7 } }) + "\n" } : { status: 0, stdout: "" };
  const deps = fakeHost({ config: secretCompose, hub: 1, polls: [{ instance: noisy }], logs: answer });
  const out = await probe(["live", "hub"], once, deps), text = JSON.stringify(out);
  assert.ok(!text.includes("SYNTHETIC")); assert.ok(!text.includes(opaque)); assert.ok(!text.includes("\\u001b"));
  assert.deepEqual(Object.keys(out.capture), ["app", "logs"]);
  assert.deepEqual(out.capture.app, { state: "RUNNING", containers: [{ service: "hub", state: "running", id: "1".repeat(12) }, { service: "discord-bridge", state: "exited", id: "2".repeat(12) }] });
  const [hubLog, bridgeLog] = out.capture.logs;
  assert.deepEqual(bridgeLog, { service: "discord-bridge", unavailable: "no log lines" });
  assert.deepEqual(Object.keys(hubLog), ["service", "lines"]);
  // 59 lines were read: the newest 40 are kept, each at most 300 characters.
  assert.equal(hubLog.lines.length, 40); assert.ok(hubLog.lines.every(l => typeof l === "string" && l.length <= 300));
  assert.equal(hubLog.lines.at(-1), "new line 20"); assert.ok(!hubLog.lines.includes("old line 0"));
  assert.ok(hubLog.lines.includes("env value [redacted] and [redacted] here")); assert.ok(hubLog.lines.includes("[redacted]"));
  assert.ok(hubLog.lines.includes("id [redacted] end")); assert.ok(hubLog.lines.includes("[31mpanic: [redacted][0m"));
  assert.ok(hubLog.lines.includes("x".repeat(300))); assert.ok(hubLog.lines.includes("migration 41 applied"));
  assert.ok(logReads(deps).every(r => r.opts.timeout === 15000));
  // The same answers through the rollback probe.
  const rolled = await probe(["rollback", "hub", "--expect-release", "rel_x-aaaaaaaaaaaa-hub", "--expect-sha", sha], once, fakeHost({ config: secretCompose, hub: 1, polls: [{ instance: noisy }], logs: answer }));
  assert.deepEqual(rolled.capture, out.capture); assert.ok(!JSON.stringify(rolled).includes("SYNTHETIC"));

  // Log reads that fail, in every way, are recorded and change nothing else.
  const verdict = o => { const { capture, ...rest } = o; return rest; };
  const working = await probe(["live", "hub"], once, fakeHost({ hub: 1 }));
  const failing = async (logs, expected) => {
    const host = fakeHost({ hub: 1, logs }), result = await probe(["live", "hub"], once, host);
    assert.deepEqual(verdict(result), verdict(working));
    assert.deepEqual(result.capture.logs, expected);
    assert.equal(result.capture.app.state, "RUNNING");
    return host;
  };
  const both = reason => [{ service: "hub", unavailable: reason }, { service: "discord-bridge", unavailable: reason }];
  await failing(() => ({ status: 1, stdout: "" }), both("log read failed"));
  const timedOut = await failing(() => ({ status: null, stdout: "" }), both("log read failed"));
  assert.equal(logReads(timedOut).length, 2); assert.ok(logReads(timedOut).every(r => r.opts.timeout === 15000));
  await failing(() => ({ status: 0, stdout: "" }), both("no log lines"));
  await failing(() => ({ status: 0, stdout: `${SECRET}\n{"fields":{}}\n` }), both("no log lines"));
  await failing(() => { throw new Error(SECRET); }, both("log read failed"));
  // Lines followed by a non-zero status (a quiet stream that timed out) are kept.
  await failing(() => ({ status: 1, stdout: logLine("partial") + "\n" }), [{ service: "hub", lines: ["partial"] }, { service: "discord-bridge", lines: ["partial"] }]);
  // The first log read uses up the 45 s budget: the next is skipped, not run.
  const slow = await failing(({ advance }) => { advance(50000); return { status: 0, stdout: logLine("slow") + "\n" }; }, [{ service: "hub", lines: ["slow"] }, { service: "discord-bridge", unavailable: "time budget" }]);
  assert.equal(logReads(slow).length, 1);
  // A later read's timeout is cut to what is left of the budget.
  const cut = await failing(({ advance, n }) => { if (n === 0) advance(35000); return { status: 0, stdout: logLine("slow") + "\n" }; }, [{ service: "hub", lines: ["slow"] }, { service: "discord-bridge", lines: ["slow"] }]);
  assert.deepEqual(logReads(cut).map(r => r.opts.timeout), [15000, 10000]);

  // An id that is not 64 hex is never placed in a command.
  const hostile = [{ service_name: "hub", state: "running", id: "abc'; rm -rf / #" }, { service_name: "discord-bridge", state: "running", id: "A".repeat(64) }, { service_name: "bad name!", state: "x y", id: ["1".repeat(64)] }];
  const odd = fakeHost({ hub: 1, containers: hostile }), oddOut = await probe(["live", "hub"], once, odd);
  assert.equal(logReads(odd).length, 0); assert.ok(!odd.calls.some(a => String(a.at(-1)).includes("rm -rf")));
  assert.deepEqual(oddOut.capture, { app: { state: "RUNNING", containers: [{ service: "hub", state: "running", id: null }, { service: "discord-bridge", state: "running", id: null }, { service: null, state: null, id: null }] }, logs: [{ service: "hub", unavailable: "invalid container id" }, { service: "discord-bridge", unavailable: "invalid container id" }, { service: null, unavailable: "invalid container id" }] });

  // app.get_instance failing on every poll and again in the capture.
  const dead = fakeHost({ hub: 1, polls: [{ instance: { status: 1, stdout: SECRET } }] }), deadOut = await probe(["live", "hub"], windowed(10000), dead);
  assert.deepEqual(deadOut.capture, { app: "unavailable", logs: [] }); assert.equal(deadOut.waitedMs, 10000); assert.equal(deadOut.polls, 3);
  assert.equal(readyReads(dead)[0], 4); assert.equal(dead.runs.filter(r => r.argv.at(-1) === "midclt call app.get_instance tailterm-hub").at(-1).opts.timeout, 15000);
  assert.deepEqual((await probe(["live", "hub"], once, fakeHost({ hub: 1, polls: [{ instance: { get status() { throw new Error(SECRET); } } }] }))).capture, { app: "unavailable", logs: [] });

  // sanitizeCapture alone: any input becomes the fixed shape.
  assert.deepEqual(sanitizeCapture(undefined), { app: "unavailable", logs: [] });
  assert.deepEqual(sanitizeCapture({ app: "anything", logs: "x", extra: SECRET }), { app: "unavailable", logs: [] });
  assert.deepEqual(sanitizeCapture({ app: { state: "running", containers: Array.from({ length: 12 }, () => null), config: SECRET }, logs: Array.from({ length: 6 }, () => ({ service: "hub", unavailable: SECRET })) }),
    { app: { state: null, containers: Array.from({ length: 8 }, () => ({ service: null, state: null, id: null })) }, logs: Array.from({ length: 4 }, () => ({ service: "hub", unavailable: "log read failed" })) });
  assert.deepEqual(sanitizeCapture(out.capture), out.capture);
});

test("H7 every hub and bridge probe command has a run timeout and the worst case stays inside the runner's command timeout", async () => {
  const RUNNER_COMMAND_TIMEOUT_MS = 600000, good = ["rollback", "hub", "--expect-release", "rel_x-aaaaaaaaaaaa-hub", "--expect-sha", sha];
  const four = Object.entries(ids).map(([service_name, id]) => ({ service_name, state: "starting", id }));
  const isIdentity = r => ["midclt call app.config tailterm-hub"].includes(r.argv.at(-1)) || String(r.argv.at(-1)).startsWith("cat ") || r.argv[0] === "go";
  const isReady = r => r.opts.timeout === 20000 && (r.argv.at(-1) === "midclt call app.get_instance tailterm-hub" || r.argv[1] === "projects");
  // The fastest machine gives every command its full timeout.
  const healthy = fakeHost(); await probe(["live", "hub"], config(), healthy);
  assert.deepEqual(healthy.runs.filter(isIdentity).map(r => r.opts.timeout), [30000, 30000, 30000]);
  assert.ok(healthy.runs.every(r => Number.isSafeInteger(r.opts.timeout) && r.opts.timeout > 0 && r.opts.timeout <= 30000));
  // Worst cases: every command hangs until 1 ms before its timeout, at the cap
  // and at the default, with app.get_instance readable (four containers, so
  // four log reads are wanted) and with it readable only by the capture.
  const answer = JSON.stringify({ state: "DEPLOYING", active_workloads: { container_details: four } });
  for (const windowMs of [300000, 240000]) for (const argv of [["live", "hub"], ["live", "bridge"], good]) for (const instance of [() => ({ status: 0, stdout: answer }), opts => opts.timeout === 20000 ? { status: null, stdout: "" } : { status: 0, stdout: answer }]) {
    const deps = fakeHost({ hub: 1, slow: true, polls: [{ instance }] }), out = await probe(argv, windowed(windowMs), deps), label = `${argv.join(" ")} window ${windowMs}`;
    assert.ok(deps.clock() < RUNNER_COMMAND_TIMEOUT_MS - 120000, `${label}: ${deps.clock()} ms`);
    assert.ok(deps.runs.every(r => Number.isSafeInteger(r.opts.timeout) && r.opts.timeout > 0), label);
    const identity = deps.runs.filter(isIdentity), ready = deps.runs.filter(isReady), capture = deps.runs.slice(identity.length + ready.length);
    assert.deepEqual(identity.map(r => r.opts.timeout), [30000, 30000, 30000], label);
    assert.equal(ready.length, out.polls * 2, label);
    // The window is overrun by at most one poll.
    assert.ok(out.waitedMs >= windowMs && out.waitedMs < windowMs + 40000, `${label}: waited ${out.waitedMs}`);
    // The capture's commands fit its 45 s budget together, 15 s at most each.
    assert.ok(capture.length >= 3 && capture.every(r => r.opts.timeout <= 15000), label);
    assert.ok(capture.reduce((a, r) => a + r.opts.timeout, 0) <= 45000, label);
    assert.equal(out.capture.logs.length, 4, label); assert.equal(out.capture.logs.at(-1).unavailable, "time budget", label);
    assert.ok(out.capture.logs.every(l => l.lines || l.unavailable === "time budget"), label);
  }
});

// The real command with fake ssh, go and tt executables first on PATH: no
// production host is contacted. The poll interval is the real 5 s, so the two
// waiting runs go side by side.
test("H6 the probe command waits for a late hub, captures a hub that never comes up and prints nothing for an unreadable config", async () => {
  const dir = mkdtempSync(join(tmpdir(), "probe-ready-cli-")), bin = join(dir, "bin"), node = "#!" + process.execPath + "\n";
  mkdirSync(bin);
  const exe = (name, body) => { writeFileSync(join(bin, name), node + body); chmodSync(join(bin, name), 0o755); };
  exe("ssh", `const c=process.argv.at(-1);if(c==="midclt call app.config tailterm-hub")console.log(${JSON.stringify(JSON.stringify(compose()))});else if(c.startsWith("cat "))process.stdout.write("hub binary");else if(c==="midclt call app.get_instance tailterm-hub")console.log(${JSON.stringify(JSON.stringify({ state: "RUNNING", notes: SECRET, active_workloads: { container_details: up } }))});else{console.error("sudo: a password is required ${SECRET}");process.exit(1);}`);
  exe("go", `console.log("x: go1.25\\n\\tbuild\\tvcs.revision=${commit}\\n\\tbuild\\tvcs.modified=false");`);
  // Fails PROBE_TT_FAILS times, counting its calls in PROBE_TT_COUNT, then succeeds.
  exe("tt", `const fs=require("fs"),f=process.env.PROBE_TT_COUNT,n=Number(fs.existsSync(f)?fs.readFileSync(f,"utf8"):0)+1;fs.writeFileSync(f,String(n));console.error("${SECRET}");process.exit(n>Number(process.env.PROBE_TT_FAILS)?0:1);`);
  const run = (name, windowMs, fails) => new Promise((done, fail) => {
    const cfg = join(dir, name + ".json"); writeFileSync(cfg, JSON.stringify({ tt: join(bin, "tt"), targets: { hub: { host: "truenas", readyWindowMs: windowMs } }, secret: SECRET }));
    const child = spawn(process.execPath, ["scripts/release-probe.mjs", "live", "hub", "--config", cfg], { stdio: ["ignore", "pipe", "pipe"], env: { ...process.env, PATH: bin + ":" + process.env.PATH, PROBE_TT_COUNT: join(dir, name + ".count"), PROBE_TT_FAILS: String(fails) } });
    let stdout = "", stderr = ""; child.stdout.on("data", d => { stdout += d; }); child.stderr.on("data", d => { stderr += d; });
    child.on("error", fail); child.on("close", status => done({ status, stdout, stderr }));
  });
  const [late, never] = await Promise.all([run("late", 12000, 2), run("never", 1000, 1e9)]);
  for (const r of [late, never]) { assert.equal(r.status, 0); assert.equal(r.stderr, ""); assert.equal(r.stdout.trim().split("\n").length, 1); assert.ok(!r.stdout.includes("SYNTHETIC")); }
  const up3 = JSON.parse(late.stdout);
  assert.equal(up3.hubResponds, true); assert.equal(up3.containersRunning, true); assert.equal(up3.commit, commit); assert.equal(up3.polls, 3);
  // Two real 5 s sleeps plus three polls: just over 10 s here. The upper bound
  // leaves room for slow process starts on a busy host.
  assert.ok(up3.waitedMs >= 10000 && up3.waitedMs < 20000, `waitedMs ${up3.waitedMs}`); assert.ok(!("capture" in up3));
  const down = JSON.parse(never.stdout);
  assert.equal(down.hubResponds, false); assert.equal(down.containersRunning, true); assert.equal(down.polls, 2); assert.ok(down.waitedMs >= 1000);
  assert.deepEqual(down.capture, { app: { state: "RUNNING", containers: [{ service: "hub", state: "running", id: "1".repeat(12) }, { service: "discord-bridge", state: "running", id: "2".repeat(12) }] }, logs: [{ service: "hub", unavailable: "log read failed" }, { service: "discord-bridge", unavailable: "log read failed" }] });
  writeFileSync(join(dir, "broken.json"), "{ not json " + SECRET);
  for (const cfg of [join(dir, "missing.json"), join(dir, "broken.json")]) {
    const r = spawnSync(process.execPath, ["scripts/release-probe.mjs", "live", "hub", "--config", cfg], { encoding: "utf8", env: { ...process.env, PATH: bin + ":" + process.env.PATH } });
    assert.equal(r.status, 1); assert.equal(r.stdout, ""); assert.equal(r.stderr, "");
  }
});

test("Mini probe hashes the installed tt, checks the relay and counts only errors written after the deploy marker", async () => {
  const dir = mkdtempSync(join(tmpdir(), "probe-mini-")), install = join(dir, "tt"), log = join(dir, "relay.log");
  writeFileSync(install, bytes); writeFileSync(log, "old error line\n");
  const cfg = { journalDirectory: dir, targets: { mini: { installPath: install, relayLog: log } } };
  assert.deepEqual(await probe(["live", "mini"], cfg, fakeHost()), { commit, artifactSHA256: sha, integrity: true, relayRunning: true, newErrors: 0 });
  writeFileSync(join(dir, "mini-relay-offset.json"), JSON.stringify({ jobId: "rel_x", offset: 15 }));
  writeFileSync(log, "old error line\nrelay ok\nrelay error: lost\n");
  assert.equal((await probe(["live", "mini"], cfg, fakeHost())).newErrors, 1);
  assert.equal((await probe(["live", "mini"], cfg, fakeHost({ relay: "\tstate = not running" }))).relayRunning, false);
  assert.deepEqual(await probe(["rollback", "mini", "--expect-sha", sha], cfg, fakeHost()), { restored: true, databaseWritesPreserved: true });
  assert.equal((await probe(["rollback", "mini", "--expect-sha", "c".repeat(64)], cfg, fakeHost())).restored, false);
});

// A fake release.json and clock: each read returns the next response (the
// last repeats); sleep only advances the clock. No network, no real wait.
function fakeTailOS(responses) {
  let t = 0;
  const fetches = [], sleeps = [];
  return { fetches, sleeps, now: () => t, sleep: async ms => { sleeps.push(ms); t += ms; },
    fetchJSON: async (url, timeoutMs) => { const r = responses[Math.min(fetches.length, responses.length - 1)]; fetches.push({ url, timeoutMs }); if (r instanceof Error) throw r; return r; } };
}
const prior = "c".repeat(40), next = "d".repeat(40), url = "https://tailos.test/release.json";
const tailosConfig = (extra = {}) => ({ targets: { tailos: { url, ...extra } } });
const rollbackTailOS = ["rollback", "tailos", "--expect-commit", prior];

test("TailOS probes read release.json", async () => {
  assert.deepEqual(await probe(["live", "tailos"], {}, fakeHost()), { commit });
  assert.deepEqual(await probe(["rollback", "tailos", "--expect-commit", commit], {}, fakeHost()), { restored: true, databaseWritesPreserved: true, lastCommit: commit, waitedMs: 0 });
  assert.equal((await probe(["rollback", "tailos", "--expect-commit", "b".repeat(40)], tailosConfig({ switchWindowMs: 0 }), fakeHost())).restored, false);
});

test("P1 TailOS rollback probe waits through the domain switch", async () => {
  const deps = fakeTailOS([{ commit: next }, { commit: next }, { commit: next }, { commit: prior }]);
  assert.deepEqual(await probe(rollbackTailOS, tailosConfig(), deps), { restored: true, databaseWritesPreserved: true, lastCommit: prior, waitedMs: 9000 });
  assert.equal(deps.fetches.length, 4); assert.deepEqual(deps.sleeps, [3000, 3000, 3000]);
  assert.ok(deps.fetches.every(f => f.url === url && f.timeoutMs === 10000));
});

test("P2 TailOS rollback probe fails only after the default 90 s window", async () => {
  const deps = fakeTailOS([{ commit: next }]);
  assert.deepEqual(await probe(rollbackTailOS, tailosConfig(), deps), { restored: false, databaseWritesPreserved: true, lastCommit: next, waitedMs: 90000 });
  assert.equal(deps.fetches.length, 31); assert.ok(deps.sleeps.every(ms => ms <= 3000));
  assert.equal(deps.sleeps.reduce((a, b) => a + b, 0), 90000);
});

test("P3 TailOS switch window is configurable and validated", async () => {
  const six = fakeTailOS([{ commit: next }]);
  assert.equal((await probe(rollbackTailOS, tailosConfig({ switchWindowMs: 6000 }), six)).waitedMs, 6000);
  assert.equal(six.fetches.length, 3);
  const once = fakeTailOS([{ commit: next }]);
  assert.equal((await probe(rollbackTailOS, tailosConfig({ switchWindowMs: 0 }), once)).restored, false);
  assert.equal(once.fetches.length, 1); assert.deepEqual(once.sleeps, []);
  for (const bad of ["90000", -1, 300001, 1.5]) await assert.rejects(probe(rollbackTailOS, tailosConfig({ switchWindowMs: bad }), fakeTailOS([{ commit: prior }])), /switch window/);
  await assert.rejects(probe(["rollback", "tailos", "--expect-commit", "junk"], tailosConfig(), fakeTailOS([{ commit: prior }])), /expected commit/);
});

test("P4 unreadable or junk release.json counts as no read and never reaches the output", async () => {
  const junk = "<script>SYNTHETIC_PRIVATE_TOKEN</script>";
  const deps = fakeTailOS([new Error("fetch"), new Error("fetch"), { commit: junk }, { commit: prior }]);
  const out = await probe(rollbackTailOS, tailosConfig(), deps);
  assert.deepEqual(out, { restored: true, databaseWritesPreserved: true, lastCommit: prior, waitedMs: 9000 });
  const never = await probe(rollbackTailOS, tailosConfig({ switchWindowMs: 6000 }), fakeTailOS([new Error("fetch"), { commit: junk }, "html"]));
  assert.deepEqual(never, { restored: false, databaseWritesPreserved: true, lastCommit: null, waitedMs: 6000 });
  assert.ok(!JSON.stringify([out, never]).includes("SYNTHETIC"));
});

test("P5 live tailos stays one read", async () => {
  const deps = fakeTailOS([{ commit: next }, { commit: prior }]);
  assert.deepEqual(await probe(["live", "tailos"], tailosConfig(), deps), { commit: next });
  assert.equal(deps.fetches.length, 1); assert.deepEqual(deps.sleeps, []);
});

test("P6 hostDeps.fetchJSON asks for an uncached read with a timeout", async t => {
  const original = globalThis.fetch, inits = [];
  t.after(() => { globalThis.fetch = original; });
  globalThis.fetch = async (u, init) => { inits.push([u, init]); return inits.length === 1 ? { ok: true, json: async () => ({ commit: prior }) } : { ok: false, json: async () => ({ commit: prior }) }; };
  assert.deepEqual(await hostDeps.fetchJSON(url), { commit: prior });
  await assert.rejects(hostDeps.fetchJSON(url, 5000), /fetch/);
  assert.equal(inits.length, 2);
  for (const [u, init] of inits) { assert.equal(u, url); assert.equal(init.cache, "no-store"); assert.ok(init.signal instanceof AbortSignal); }
});

test("P7 only a string release commit counts as a read", async () => {
  for (const bad of [[prior], [[prior]], { toString: () => prior }]) {
    const deps = fakeTailOS([{ commit: bad }]);
    assert.deepEqual(await waitForTailOSCommit(url, prior, { windowMs: 6000, deps }), { matched: false, lastCommit: null, waitedMs: 6000, polls: 3 });
    assert.deepEqual(await probe(rollbackTailOS, tailosConfig({ switchWindowMs: 6000 }), fakeTailOS([{ commit: bad }])), { restored: false, databaseWritesPreserved: true, lastCommit: null, waitedMs: 6000 });
    await assert.rejects(probe(["live", "tailos"], tailosConfig(), fakeTailOS([{ commit: bad }])), /no release commit/);
  }
  const late = await waitForTailOSCommit(url, prior, { deps: fakeTailOS([{ commit: [next] }, { commit: [prior] }, { commit: prior }]) });
  assert.deepEqual(late, { matched: true, lastCommit: prior, waitedMs: 6000, polls: 3 });
});

test("the probe command prints nothing on failure", () => {
  const dir = mkdtempSync(join(tmpdir(), "probe-cli-")), cfg = join(dir, "config.json");
  writeFileSync(cfg, JSON.stringify({ targets: { mini: { installPath: join(dir, "missing") } }, secret: SECRET }));
  const r = spawnSync(process.execPath, ["scripts/release-probe.mjs", "live", "mini", "--config", cfg], { encoding: "utf8" });
  assert.equal(r.status, 1); assert.equal(r.stdout, ""); assert.equal(r.stderr, "");
});
