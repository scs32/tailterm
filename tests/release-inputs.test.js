import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, statSync, writeFileSync, existsSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { buildInputs } from "../scripts/release-inputs.mjs";

const BASE = "/mnt/deepfreeze/tailterm-hub", SECRET = "SYNTHETIC_PRIVATE_TOKEN";
const hash = b => createHash("sha256").update(b).digest("hex");
const git = (cwd, ...a) => execFileSync("git", a, { cwd, encoding: "utf8" }).trim();
function repo() {
  const cwd = mkdtempSync(join(tmpdir(), "inputs-git-"));
  git(cwd, "init", "-q", "-b", "tasks-hub"); git(cwd, "config", "user.email", "f@example.invalid"); git(cwd, "config", "user.name", "F");
  mkdirSync(join(cwd, "docs")); writeFileSync(join(cwd, "docs/a.md"), "a"); git(cwd, "add", "."); git(cwd, "commit", "-qm", "base");
  return { cwd, base: git(cwd, "rev-parse", "HEAD") };
}
function commitFile(r, file, text) { mkdirSync(join(r.cwd, file, ".."), { recursive: true }); writeFileSync(join(r.cwd, file), text); git(r.cwd, "add", "."); git(r.cwd, "commit", "-qm", file); return git(r.cwd, "rev-parse", "HEAD"); }
const LIVE = {
  hub: { commit: "1".repeat(40), artifactSHA256: "2".repeat(64), integrity: true, release: "rel_prev-111111111111-hub" },
  bridge: { commit: "1".repeat(40), artifactSHA256: "3".repeat(64), integrity: true, release: "20260929-live" },
  mini: { commit: "1".repeat(40), artifactSHA256: "4".repeat(64), integrity: true },
  tailos: { commit: "5".repeat(40) },
};
function setup(file, { state = "claimed", released = [] } = {}) {
  const r = repo(), commit = commitFile(r, file, "change"), home = mkdtempSync(join(tmpdir(), "inputs-home-")), template = join(home, "plan-template.json");
  writeFileSync(template, JSON.stringify({ version: 1, route: { id: "truenas-ssh" }, databaseOwner: "db-handler", sourceDatabase: `${BASE}/state/hub.sqlite`, deployment: { stateDirectory: `${BASE}/state`, binaryDestination: "stale", bridgeBinaryDestination: "stale", discordTokenPath: `${BASE}/discord-token` } }));
  const config = { version: 1, enabled: true, cwd: r.cwd, journalDirectory: home, tt: "tt", baselines: Object.fromEntries(["hub", "bridge", "mini", "tailos"].map(t => [t, r.base])), targets: { hub: { host: "truenas" } }, inputs: { planTemplate: template } };
  const job = { id: "rel_0123abcd", state, generation: 4, commit, verificationDigest: "d".repeat(64), itemId: "wi_fixture" };
  const calls = [];
  const deps = {
    configPath: join(home, "deploy.json"),
    tt: argv => { calls.push(["tt", ...argv]); return JSON.stringify([job, ...released]); },
    git: argv => git(r.cwd, ...argv),
    probe: async argv => { calls.push(["probe", ...argv]); return LIVE[argv[1]]; },
    preflight: (plan, receipt) => { calls.push(["preflight", plan]); const p = JSON.parse(readFileSync(plan, "utf8")); writeFileSync(receipt, JSON.stringify({ status: "success", backupDestination: p.backupDestination, sha256: hash("backup-" + p.requestId), secret: SECRET })); },
    copyBackup: (remote, local) => { calls.push(["copy", remote]); writeFileSync(local, "backup-" + job.id + "-" + remote.match(/before-rel_0123abcd-(\w+)\.sqlite$/)[1] + "-backup"); },
  };
  return { r, commit, home, config, job, deps, calls };
}

test("a store change deploys hub and bridge from ONE plan and backup with each target's own live rollback", async () => {
  const f = setup("hub/internal/store/x.go");
  const out = await buildInputs(f.config, f.job.id, { deps: f.deps });
  const path = join(f.home, "rel_0123abcd-inputs.json"), raw = readFileSync(path, "utf8"), m = JSON.parse(raw), sha12 = f.commit.slice(0, 12);
  assert.equal(statSync(path).mode & 0o777, 0o600);
  assert.deepEqual(out, { manifest: path, sha256: hash(raw), targets: ["hub", "bridge", "mini"], command: ["tt", "deployment", "inputs", "--job", "rel_0123abcd", "--generation", "4", "--commit", f.commit, "--file", path, "--request-id", `rel_0123abcd-inputs-${sha12}`] });
  assert.deepEqual({ version: m.version, jobId: m.jobId, commit: m.commit, acceptedCommit: m.acceptedCommit, verificationDigest: m.verificationDigest }, { version: 1, jobId: "rel_0123abcd", commit: f.commit, acceptedCommit: f.commit, verificationDigest: "d".repeat(64) });
  const hub = m.targets.hub, bridge = m.targets.bridge, release = `rel_0123abcd-${sha12}-truenas`;
  assert.equal(hub.release, release); assert.equal(hub.backupJobId, "rel_0123abcd"); assert.equal(hub.backup, `${BASE}/backups/before-rel_0123abcd-truenas.sqlite`);
  assert.deepEqual(hub.planTargets, ["hub", "bridge"]); assert.equal(hub.planPath, join(f.home, "rel_0123abcd-truenas-plan.json"));
  assert.equal(hub.preflightReceiptSHA256, hash(readFileSync(hub.preflightReceipt))); assert.equal(hub.backupSHA256, hash("backup-rel_0123abcd-truenas-backup"));
  assert.equal(hub.backupCopy, join(f.home, "rel_0123abcd-truenas-backup.sqlite")); assert.equal(hash(readFileSync(hub.backupCopy)), hub.backupSHA256);
  for (const k of ["release", "planTargets", "backupJobId", "backup", "planPath", "preflightReceipt", "preflightReceiptSHA256", "backupSHA256", "backupCopy"]) assert.deepEqual(bridge[k], hub[k], k);
  assert.deepEqual(f.calls.filter(c => c[0] === "preflight"), [["preflight", hub.planPath]]);
  assert.deepEqual(f.calls.filter(c => c[0] === "copy"), [["copy", hub.backup]]);
  assert.deepEqual(readdirSync(f.home).filter(n => n.endsWith("-plan.json")), ["rel_0123abcd-truenas-plan.json"]);
  assert.deepEqual(hub.rollbackProgram, ["python3", "scripts/deploy-truenas-hub.py", "--rollback-to", "rel_prev-111111111111-hub", "--target", "hub", "--expect-sha256", "2".repeat(64)]);
  assert.deepEqual(hub.rollbackProbe, ["node", "scripts/release-probe.mjs", "rollback", "hub", "--expect-release", "rel_prev-111111111111-hub", "--expect-sha", "2".repeat(64), "--config", f.deps.configPath]);
  assert.deepEqual(bridge.rollbackProgram, ["python3", "scripts/deploy-truenas-hub.py", "--rollback-to", "20260929-live", "--target", "bridge", "--expect-sha256", "3".repeat(64)]);
  assert.deepEqual(bridge.rollbackProbe, ["node", "scripts/release-probe.mjs", "rollback", "bridge", "--expect-release", "20260929-live", "--expect-sha", "3".repeat(64), "--config", f.deps.configPath]);
  assert.equal(hub.rollbackSafe, true); assert.equal(bridge.rollbackSafe, true);
  const planText = readFileSync(hub.planPath, "utf8"), plan = JSON.parse(planText);
  assert.equal(statSync(hub.planPath).mode & 0o777, 0o600);
  assert.deepEqual(plan.deployment.targets, ["hub", "bridge"]); assert.equal(plan.deployment.releaseName, release); assert.equal(plan.backupDestination, hub.backup); assert.equal(plan.requestId, "rel_0123abcd-truenas-backup");
  assert.equal(plan.deployment.binaryDestination, `${BASE}/releases/${release}/tailterm-hub`); assert.equal(plan.deployment.bridgeBinaryDestination, `${BASE}/releases/${release}/tailterm-discord`);
  assert.ok(!planText.includes("rel_prev-111111111111-hub") && !planText.includes("20260929-live"), "no live release is pinned");
  assert.deepEqual(m.targets.mini, { release: `rel_0123abcd-${sha12}-mini`, rollbackSafe: true, rollbackProbe: ["node", "scripts/release-probe.mjs", "rollback", "mini", "--expect-sha", "4".repeat(64), "--config", f.deps.configPath] });
  assert.ok(!raw.includes(SECRET) && !JSON.stringify(out).includes(SECRET));
  await assert.rejects(buildInputs(f.config, f.job.id, { deps: f.deps }), /already written/);
});

test("a single changed TrueNAS target gets its own plan pinning the live partner", async () => {
  for (const [file, t, partner, field, mount] of [["hub/internal/broker/x.go", "hub", "bridge", "bridgeBinaryDestination", "20260929-live/tailterm-discord"], ["hub/internal/bridge/x.go", "bridge", "hub", "binaryDestination", "rel_prev-111111111111-hub/tailterm-hub"]]) {
    const f = setup(file), m = JSON.parse(readFileSync((await buildInputs(f.config, f.job.id, { deps: f.deps })).manifest, "utf8")), sha12 = f.commit.slice(0, 12);
    assert.deepEqual(Object.keys(m.targets), [t]);
    const target = m.targets[t], plan = JSON.parse(readFileSync(target.planPath, "utf8"));
    assert.equal(target.release, `rel_0123abcd-${sha12}-${t}`); assert.deepEqual(target.planTargets, [t]); assert.equal(target.backup, `${BASE}/backups/before-rel_0123abcd-${t}.sqlite`);
    assert.deepEqual(plan.deployment.targets, [t]); assert.equal(plan.deployment[field], `${BASE}/releases/${mount}`, `${partner} keeps its live mount`);
    const own = t === "hub" ? ["binaryDestination", "tailterm-hub"] : ["bridgeBinaryDestination", "tailterm-discord"];
    assert.equal(plan.deployment[own[0]], `${BASE}/releases/${target.release}/${own[1]}`);
    assert.equal(f.calls.filter(c => c[0] === "preflight").length, 1);
  }
});

test("the manifest satisfies the deployer's exact job input binding", async () => {
  const f = setup("client/app.js"), { HostAdapter } = await import("../scripts/release-runner.mjs");
  const out = await buildInputs(f.config, f.job.id, { deps: f.deps });
  const adapter = new HostAdapter(f.config, { ...f.job, inputsCommit: f.commit, inputsDigest: out.sha256 });
  assert.equal(adapter.jobInputs(f.commit).targets.tailos.release, `rel_0123abcd-${f.commit.slice(0, 12)}-tailos`);
  assert.equal(adapter.jobInputs(f.commit).targets.tailos.rollbackSafe, false, "no retained dist for the live TailOS commit");
});

test("TailOS rolls back to the retained dist of the live commit", async () => {
  const f = setup("client/app.js");
  mkdirSync(join(f.home, `tailos-dist-${"5".repeat(40)}`));
  await buildInputs(f.config, f.job.id, { deps: f.deps });
  const t = JSON.parse(readFileSync(join(f.home, "rel_0123abcd-inputs.json"), "utf8")).targets.tailos;
  assert.deepEqual(t.rollbackProgram, ["npx", "wrangler", "pages", "deploy", join(f.home, `tailos-dist-${"5".repeat(40)}`), "--project-name", "tailos", "--branch", "main", "--commit-hash", "5".repeat(40), "--commit-dirty=false"]);
  assert.deepEqual(t.rollbackProbe, ["node", "scripts/release-probe.mjs", "rollback", "tailos", "--expect-commit", "5".repeat(40), "--config", f.deps.configPath]);
  assert.equal(t.rollbackSafe, true);
  assert.ok(!f.calls.some(c => c[0] === "preflight"));
});

test("released receipts move baselines, so nothing selected gives an empty manifest", async () => {
  const f = setup("client/app.js");
  const released = { id: "rel_done", state: "released", receipt: { outcome: "released", commit: f.commit, targets: [{ target: "tailos", outcome: "released" }] } };
  f.deps.tt = () => JSON.stringify([f.job, released]);
  const out = await buildInputs(f.config, f.job.id, { deps: f.deps });
  assert.deepEqual(out.targets, []); assert.deepEqual(JSON.parse(readFileSync(out.manifest, "utf8")).targets, {});
  assert.ok(!f.calls.some(c => c[0] === "probe"));
});

test("dry run prints the planned bindings with no host call or write", async () => {
  const f = setup("hub/internal/api/x.go");
  const out = await buildInputs(f.config, f.job.id, { dryRun: true, deps: f.deps }), sha12 = f.commit.slice(0, 12);
  assert.equal(out.dryRun, true); assert.equal(out.generation, 4); assert.equal(out.schemaChanged, false); assert.equal(out.commit, f.commit); assert.equal(out.acceptedCommit, f.commit);
  assert.deepEqual(Object.keys(out.targets), ["hub", "bridge", "mini"]);
  const shared = { release: `rel_0123abcd-${sha12}-truenas`, backupJobId: "rel_0123abcd", backup: `${BASE}/backups/before-rel_0123abcd-truenas.sqlite`, planPath: join(f.home, "rel_0123abcd-truenas-plan.json"), preflightReceipt: join(f.home, "rel_0123abcd-truenas-preflight.json"), planTargets: ["hub", "bridge"] };
  assert.deepEqual(out.targets.hub, shared); assert.deepEqual(out.targets.bridge, shared);
  assert.deepEqual(f.calls.map(c => c[0]), ["tt"]); assert.deepEqual(readdirSync(f.home), ["plan-template.json"]);
});

test("inputs are refused for an unclaimed job, a changed backup copy, or an integrated commit that is not local", async () => {
  await assert.rejects(buildInputs(setup("client/a.js", { state: "verified" }).config, "rel_0123abcd", { deps: setup("client/a.js", { state: "verified" }).deps }), /claimed/);
  const f = setup("hub/internal/store/x.go"); f.deps.copyBackup = (remote, local) => writeFileSync(local, "tampered");
  await assert.rejects(buildInputs(f.config, f.job.id, { deps: f.deps }), /hash mismatch/);
  assert.ok(!existsSync(join(f.home, "rel_0123abcd-inputs.json")));
  const g = setup("client/a.js"); g.job.integratedCommit = "9".repeat(40);
  await assert.rejects(buildInputs(g.config, g.job.id, { deps: g.deps }));
});

test("the inputs command reports its own refusal without host output", () => {
  const home = mkdtempSync(join(tmpdir(), "inputs-cli-")), cfg = join(home, "c.json");
  writeFileSync(cfg, JSON.stringify({ version: 1, cwd: home, journalDirectory: home, tt: join(home, "missing-tt") }));
  const r = spawnSync(process.execPath, ["scripts/release-inputs.mjs", "--config", cfg, "--job", "rel_abc"], { encoding: "utf8" });
  assert.equal(r.status, 1); assert.equal(r.stdout, ""); assert.equal(r.stderr, "release inputs refused: a host command failed\n");
});
