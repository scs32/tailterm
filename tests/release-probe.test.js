import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { probe, hostDeps, waitForTailOSCommit } from "../scripts/release-probe.mjs";
import { readyWindow, waitForTrueNASReady, TRUENAS_READY_WINDOW_MS } from "../scripts/release-probe.mjs";

const BASE = "/mnt/deepfreeze/tailterm-hub", SECRET = "SYNTHETIC_PRIVATE_TOKEN";
const commit = "a".repeat(40), bytes = Buffer.from("hub binary"), sha = createHash("sha256").update(bytes).digest("hex");
const compose = (hubRelease = "rel_x-aaaaaaaaaaaa-hub", bridgeRelease = "20260929-live") => ({ services: {
  hub: { volumes: [`${BASE}/releases/${hubRelease}/tailterm-hub:/opt/tailterm-hub:ro`, `${BASE}/state:/state`, `${BASE}/hub-token:/run/hub-token:ro`], environment: { TOKEN: SECRET } },
  "discord-bridge": { volumes: [`${BASE}/releases/${bridgeRelease}/tailterm-discord:/opt/tailterm-discord:ro`, `${BASE}/discord-token:/run/discord-token:ro`] },
} });
const ids = { hub: "1".repeat(64), "discord-bridge": "2".repeat(64) };
const up = [{ service_name: "hub", state: "running", id: ids.hub }, { service_name: "discord-bridge", state: "running", id: ids["discord-bridge"] }];
// `polls` scripts each readiness read in turn (the last entry repeats): an
// entry overrides state, containers, hub (the tt exit status) or instance
// (the app.get_instance answer). sleep only advances the fake clock.
function fakeHost({ config = compose(), modified = "false", state = "RUNNING", containers = up, hub = 0, relay = "\tstate = running", polls = [{}] } = {}) {
  const calls = [], runs = [], sleeps = [];
  let t = 0, instanceReads = 0, hubReads = 0;
  const poll = n => ({ state, containers, hub, ...polls[Math.min(n, polls.length - 1)] });
  return { calls, runs, sleeps, uid: () => 501, fetchJSON: async () => ({ commit }), now: () => t, sleep: async ms => { sleeps.push(ms); t += ms; }, run: (argv, opts = {}) => {
    calls.push(argv); runs.push({ argv, opts });
    const cmd = argv.at(-1);
    if (argv[0] === "ssh") {
      assert.deepEqual(argv.slice(0, 6), ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "truenas"]);
      if (cmd === "midclt call app.config tailterm-hub") return { status: 0, stdout: JSON.stringify(config) };
      if (cmd === "midclt call app.get_instance tailterm-hub") { const p = poll(instanceReads++); return p.instance || { status: 0, stdout: JSON.stringify({ state: p.state, active_workloads: { container_details: p.containers } }) }; }
      if (cmd.startsWith("cat ")) { assert.ok(opts.buffer); return { status: 0, stdout: bytes }; }
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
  // An app.get_instance read that fails, times out, is not JSON or throws is
  // "not ready", not a probe failure.
  const thrower = { get status() { throw new Error(SECRET); } };
  const flaky = fakeHost({ polls: [{ instance: { status: 1, stdout: SECRET } }, { instance: { status: null, stdout: "" } }, { instance: { status: 0, stdout: "<html>" } }, { instance: thrower }, {}] });
  const late = await probe(["live", "hub"], config(), flaky);
  assert.equal(late.containersRunning, true); assert.equal(late.hubResponds, true); assert.equal(late.polls, 5); assert.equal(late.waitedMs, 20000);
  const never = await probe(["live", "bridge"], windowed(10000), fakeHost({ polls: [{ instance: { status: 1, stdout: "" } }] }));
  assert.equal(never.containersRunning, false); assert.equal(never.hubResponds, true); assert.equal(never.waitedMs, 10000);
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
