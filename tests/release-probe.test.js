import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import { probe, hostDeps } from "../scripts/release-probe.mjs";

const BASE = "/mnt/deepfreeze/tailterm-hub", SECRET = "SYNTHETIC_PRIVATE_TOKEN";
const commit = "a".repeat(40), bytes = Buffer.from("hub binary"), sha = createHash("sha256").update(bytes).digest("hex");
const compose = (hubRelease = "rel_x-aaaaaaaaaaaa-hub", bridgeRelease = "20260929-live") => ({ services: {
  hub: { volumes: [`${BASE}/releases/${hubRelease}/tailterm-hub:/opt/tailterm-hub:ro`, `${BASE}/state:/state`, `${BASE}/hub-token:/run/hub-token:ro`], environment: { TOKEN: SECRET } },
  "discord-bridge": { volumes: [`${BASE}/releases/${bridgeRelease}/tailterm-discord:/opt/tailterm-discord:ro`, `${BASE}/discord-token:/run/discord-token:ro`] },
} });
function fakeHost({ config = compose(), modified = "false", state = "RUNNING", containers = [{ service_name: "hub", state: "running" }, { service_name: "discord-bridge", state: "running" }], hub = 0, relay = "\tstate = running" } = {}) {
  const calls = [];
  return { calls, uid: () => 501, fetchJSON: async () => ({ commit }), now: () => 0, sleep: async () => { throw new Error("unexpected sleep"); }, run: (argv, opts = {}) => {
    calls.push(argv);
    const cmd = argv.at(-1);
    if (argv[0] === "ssh") {
      assert.deepEqual(argv.slice(0, 6), ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "truenas"]);
      if (cmd === "midclt call app.config tailterm-hub") return { status: 0, stdout: JSON.stringify(config) };
      if (cmd === "midclt call app.get_instance tailterm-hub") return { status: 0, stdout: JSON.stringify({ state, active_workloads: { container_details: containers } }) };
      if (cmd.startsWith("cat ")) { assert.ok(opts.buffer); return { status: 0, stdout: bytes }; }
    }
    if (argv[0] === "go") return { status: 0, stdout: `${argv[3]}: go1.25\n\tbuild\tvcs.revision=${commit}\n\tbuild\tvcs.modified=${modified}\n\tbuild\t${SECRET}=1\n` };
    if (argv[1] === "projects") return { status: hub, stdout: SECRET };
    if (argv[0] === "launchctl") return { status: 0, stdout: `${relay}\n${SECRET}` };
    return { status: 1, stdout: SECRET };
  } };
}
const config = (extra = {}) => ({ tt: "/opt/tt", targets: { hub: { host: "truenas" }, bridge: { host: "truenas" }, ...extra } });

test("live hub probe reads the mounted release over the truenas route and prints only the checked fields", async () => {
  const deps = fakeHost(), out = await probe(["live", "hub"], config(), deps);
  assert.deepEqual(out, { commit, artifactSHA256: sha, integrity: true, hubResponds: true, migrationsApplied: true, containersRunning: true, release: "rel_x-aaaaaaaaaaaa-hub" });
  assert.ok(!JSON.stringify(out).includes(SECRET));
  assert.ok(deps.calls.some(a => a.at(-1) === `cat ${BASE}/releases/rel_x-aaaaaaaaaaaa-hub/tailterm-hub`));
  assert.deepEqual(deps.calls.find(a => a[1] === "projects"), ["/opt/tt", "projects"]);
  assert.equal((await probe(["live", "bridge"], config(), fakeHost())).release, "20260929-live");
});

test("live hub probe reports a dirty build, a stopped container and an unreachable hub", async () => {
  assert.equal((await probe(["live", "hub"], config(), fakeHost({ modified: "true" }))).integrity, false);
  assert.equal((await probe(["live", "hub"], config(), fakeHost({ state: "STOPPED" }))).containersRunning, false);
  assert.equal((await probe(["live", "bridge"], config(), fakeHost({ containers: [{ service_name: "hub", state: "running" }, { service_name: "discord-bridge", state: "exited" }] }))).containersRunning, false);
  const down = await probe(["live", "hub"], config(), fakeHost({ hub: 1 }));
  assert.equal(down.hubResponds, false); assert.equal(down.migrationsApplied, false);
  await assert.rejects(probe(["live", "hub"], config(), fakeHost({ config: { services: { hub: { volumes: ["/tmp/x:/opt/tailterm-hub:ro"] } } } })), /release mount/);
  await assert.rejects(probe(["live", "hub"], { targets: { hub: { host: "truenas; rm -rf /" } } }, fakeHost()), /host reference/);
});

test("rollback probe for hub and bridge requires the expected retained release and hash", async () => {
  const good = ["rollback", "hub", "--expect-release", "rel_x-aaaaaaaaaaaa-hub", "--expect-sha", sha];
  assert.deepEqual(await probe(good, config(), fakeHost()), { restored: true, databaseWritesPreserved: true });
  assert.equal((await probe(["rollback", "hub", "--expect-release", "other", "--expect-sha", sha], config(), fakeHost())).restored, false);
  assert.equal((await probe([...good.slice(0, 4), "--expect-sha", "b".repeat(64)], config(), fakeHost())).restored, false);
  const moved = compose(); moved.services.hub.volumes[1] = "/tmp/elsewhere:/state";
  assert.equal((await probe(good, config(), fakeHost({ config: moved }))).databaseWritesPreserved, false);
  await assert.rejects(probe(["rollback", "hub", "--expect-release", "x"], config(), fakeHost()), /expected release/);
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

test("the probe command prints nothing on failure", () => {
  const dir = mkdtempSync(join(tmpdir(), "probe-cli-")), cfg = join(dir, "config.json");
  writeFileSync(cfg, JSON.stringify({ targets: { mini: { installPath: join(dir, "missing") } }, secret: SECRET }));
  const r = spawnSync(process.execPath, ["scripts/release-probe.mjs", "live", "mini", "--config", cfg], { encoding: "utf8" });
  assert.equal(r.status, 1); assert.equal(r.stdout, ""); assert.equal(r.stderr, "");
});
