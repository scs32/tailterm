import test from "node:test";
import assert from "node:assert/strict";
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { spawnSync } from "node:child_process";

const root = resolve(import.meta.dirname, ".."), deploy = join(root, "scripts", "deploy-truenas-hub.py");
const BASE = "/mnt/deepfreeze/tailterm-hub", SHA = "a".repeat(64), SECRET = "SYNTHETIC_PRIVATE_TOKEN";
const compose = () => ({ services: {
  hub: { volumes: [`${BASE}/releases/rel_new-cccccccccccc-hub/tailterm-hub:/opt/tailterm-hub:ro`, `${BASE}/state:/state`, `${BASE}/hub-token:/run/hub-token:ro`] },
  "discord-bridge": { volumes: [`${BASE}/releases/20260929-live/tailterm-discord:/opt/tailterm-discord:ro`, `${BASE}/bridge-state:/state`] },
} });

// The fake route answers only the rollback's remote steps and logs each one.
function run(args, { live = compose(), retainedSha = SHA, fail = "" } = {}) {
  const dir = mkdtempSync(join(tmpdir(), "tailterm-rollback-")), bin = join(dir, "bin"), log = join(dir, "calls.jsonl");
  mkdirSync(bin); writeFileSync(join(dir, "compose.json"), JSON.stringify(live));
  writeFileSync(join(bin, "ssh"), `#!${process.execPath}
const fs=require("fs"),a=process.argv.slice(2),c=a.at(-1);fs.appendFileSync(${JSON.stringify(log)},JSON.stringify(a)+"\\n");process.stderr.write(${JSON.stringify(SECRET)});
if(${JSON.stringify(fail)}&&c.startsWith(${JSON.stringify(fail)}))process.exit(9);
if(c.startsWith("sha256sum "))process.stdout.write(${JSON.stringify(retainedSha)}+"  "+c.slice(10)+"\\n");
else if(c==="midclt call app.config tailterm-hub")process.stdout.write(fs.readFileSync(${JSON.stringify(join(dir, "compose.json"))}));
else if(!c.startsWith("midclt call -j app."))process.exit(3);`);
  chmodSync(join(bin, "ssh"), 0o755);
  const p = spawnSync("python3", [deploy, ...args], { encoding: "utf8", env: { ...process.env, PATH: `${bin}:${process.env.PATH}` } });
  const calls = existsSync(log) ? readFileSync(log, "utf8").trim().split("\n").map(l => JSON.parse(l)) : [];
  return { status: p.status, out: p.stdout, result: p.stdout.trim() ? JSON.parse(p.stdout.trim()) : null, calls };
}
const args = (release = "rel_old-bbbbbbbbbbbb-hub", target = "hub", sha = SHA) => ["--rollback-to", release, "--target", target, "--expect-sha256", sha];

test("hub rollback verifies the retained binary and swaps only the hub executable mount", () => {
  const r = run(args());
  assert.equal(r.status, 0, r.out);
  assert.deepEqual(r.result, { version: 1, status: "rolled-back", phase: "complete", target: "hub", release: "rel_old-bbbbbbbbbbbb-hub", binaryDestination: `${BASE}/releases/rel_old-bbbbbbbbbbbb-hub/tailterm-hub`, sha256: SHA, appName: "tailterm-hub", databaseTouched: false });
  const commands = r.calls.map(a => a.at(-1));
  for (const a of r.calls) assert.deepEqual(a.slice(0, 5), ["-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "truenas"]);
  assert.equal(commands[0], `sha256sum ${BASE}/releases/rel_old-bbbbbbbbbbbb-hub/tailterm-hub`);
  assert.equal(commands[1], "midclt call app.config tailterm-hub");
  assert.match(commands[2], /^midclt call -j app\.update tailterm-hub '/);
  assert.equal(commands[3], "midclt call -j app.start tailterm-hub");
  assert.equal(commands.length, 4);
  const sent = JSON.parse(commands[2].slice("midclt call -j app.update tailterm-hub ".length).replace(/^'|'$/g, "")).custom_compose_config, want = compose();
  want.services.hub.volumes[0] = `${BASE}/releases/rel_old-bbbbbbbbbbbb-hub/tailterm-hub:/opt/tailterm-hub:ro`;
  assert.deepEqual(sent, want);
  assert.ok(!commands.some(c => /sqlite|backup|tailscale|token/i.test(c.replace(`${BASE}/hub-token:/run/hub-token:ro`, ""))));
  assert.ok(!r.out.includes(SECRET));
});

test("bridge rollback keeps the hub mount", () => {
  const r = run(args("20260928-prev", "bridge"));
  assert.equal(r.status, 0, r.out);
  const sent = JSON.parse(r.calls[2].at(-1).slice("midclt call -j app.update tailterm-hub ".length).replace(/^'|'$/g, "")).custom_compose_config;
  assert.equal(sent.services["discord-bridge"].volumes[0], `${BASE}/releases/20260928-prev/tailterm-discord:/opt/tailterm-discord:ro`);
  assert.deepEqual(sent.services.hub, compose().services.hub);
});

test("rollback refuses a missing or mismatched retained binary before any mutation", () => {
  for (const r of [run(args(), { retainedSha: "b".repeat(64) }), run(args(), { fail: "sha256sum" })]) {
    assert.equal(r.status, 2); assert.equal(r.result.status, "failed"); assert.equal(r.result.mutationStarted, false);
    assert.ok(!r.calls.some(a => a.at(-1).startsWith("midclt call -j")));
    assert.ok(!r.out.includes(SECRET));
  }
});

test("rollback refuses bad input and an unrecognized live definition without mutation", () => {
  for (const bad of [args("../x"), args("rel", "hub", "short"), ["--rollback-to", "rel", "--target", "mini", "--expect-sha256", SHA]]) {
    const r = run(bad); assert.notEqual(r.status, 0); assert.equal(r.calls.length, 0);
  }
  const odd = compose(); odd.services.hub.volumes[0] = "/tmp/elsewhere/tailterm-hub:/opt/tailterm-hub:ro";
  for (const live of [odd, { services: {} }, "not json"]) {
    const r = run(args(), { live }); assert.equal(r.status, 2); assert.equal(r.result.mutationStarted, false);
    assert.ok(!r.calls.some(a => a.at(-1).startsWith("midclt call -j")));
  }
  const r = run(args(), { fail: "midclt call -j app.update" });
  assert.equal(r.status, 2); assert.equal(r.result.stage, "middleware-update"); assert.equal(r.result.mutationStarted, true); assert.ok(!r.out.includes(SECRET));
});
