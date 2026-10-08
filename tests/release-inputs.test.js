import test from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, statSync, writeFileSync, existsSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { releaseBaselines } from "../scripts/release-targets.mjs";
import { buildInputs, readReleaseSummaries, readReleaseDetail } from "../scripts/release-inputs.mjs";

function releaseReply(jobs,argv) {
 const flag=n=>{const i=argv.indexOf(n);return i<0?undefined:argv[i+1];};
 if(argv[1]==="get")return jobs.find(j=>j.id===flag("--job"))||{id:flag("--job")};
 const view=flag("--view")||"active",terminal=["released","rolled_back","refused","superseded"],limit=+(flag("--limit")||50),after=+(flag("--after")||0);
 const all=jobs.map((j,i)=>{
  const {plan,integratedPlan,integratedVerification,integratedCoverage,integratedMatrix,reconciliations,retryOf,matrixApprovals,repository,...summary}=j;
  if(j.receipt)summary.receipt={commit:j.receipt.commit,outcome:j.receipt.outcome,targets:j.receipt.targets.map(t=>({target:t.target,outcome:t.outcome}))};
  if(j.supersession)summary.supersession={releasedCommit:j.supersession.releasedCommit,targets:j.supersession.targets};
  return {...summary,summary:true,rowId:i+1};
 }).filter(j=>terminal.includes(j.state)===(view==="settled")&&j.rowId>after);
 const rows=all.slice(0,limit);return {version:1,jobs:rows,page:{view,limit,snapshot:"a".repeat(64),nextAfter:all.length>limit?String(rows.at(-1).rowId):""}};
}
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
    tt: argv => { calls.push(["tt", ...argv]); return JSON.stringify(releaseReply([job, ...released],argv)); },
    git: argv => git(r.cwd, ...argv),
    probe: async argv => { calls.push(["probe", ...argv]); return LIVE[argv[1]]; },
    // As the real tool does, a request it has already served is answered
    // "already-satisfied" for the same destination and bytes.
    preflight: (plan, receipt) => { const p = JSON.parse(readFileSync(plan, "utf8")); calls.push(["preflight", plan, p.requestId, p.backupDestination]); writeFileSync(receipt, JSON.stringify({ status: existsSync(receipt) ? "already-satisfied" : "success", backupDestination: p.backupDestination, sha256: hash("backup-" + p.requestId), secret: SECRET })); },
    copyBackup: (remote, local) => { calls.push(["copy", remote]); writeFileSync(local, "backup-" + remote.match(/before-(rel_0123abcd-g\d+-\w+)\.sqlite$/)[1] + "-backup", { flag: "wx", mode: 0o600 }); },
  };
  return { r, commit, home, config, job, deps, calls };
}

test("a store change deploys hub and bridge from ONE plan and backup with each target's own live rollback", async () => {
  const f = setup("hub/internal/store/x.go");
  const out = await buildInputs(f.config, f.job.id, { deps: f.deps });
  const path = join(f.home, "rel_0123abcd-g4-inputs.json"), raw = readFileSync(path, "utf8"), m = JSON.parse(raw), sha12 = f.commit.slice(0, 12);
  assert.equal(statSync(path).mode & 0o777, 0o600);
  assert.deepEqual(out, { manifest: path, sha256: hash(raw), targets: ["hub", "bridge", "mini"], command: ["tt", "deployment", "inputs", "--job", "rel_0123abcd", "--generation", "4", "--commit", f.commit, "--file", path, "--request-id", `rel_0123abcd-g4-inputs-${sha12}`] });
  assert.deepEqual({ ...m, targets: undefined }, { version: 1, jobId: "rel_0123abcd", generation: 4, attempt: 0, commit: f.commit, acceptedCommit: f.commit, verificationDigest: "d".repeat(64), targets: undefined });
  const hub = m.targets.hub, bridge = m.targets.bridge, release = `rel_0123abcd-${sha12}-truenas`;
  assert.equal(hub.release, release); assert.equal(hub.backupJobId, "rel_0123abcd"); assert.equal(hub.backup, `${BASE}/backups/before-rel_0123abcd-g4-truenas.sqlite`);
  assert.deepEqual(hub.planTargets, ["hub", "bridge"]); assert.equal(hub.planPath, join(f.home, "rel_0123abcd-g4-truenas-plan.json")); assert.equal(hub.preflightReceipt, join(f.home, "rel_0123abcd-g4-truenas-preflight.json"));
  assert.equal(hub.preflightReceiptSHA256, hash(readFileSync(hub.preflightReceipt))); assert.equal(hub.backupSHA256, hash("backup-rel_0123abcd-g4-truenas-backup"));
  assert.equal(hub.backupCopy, join(f.home, "rel_0123abcd-g4-truenas-backup.sqlite")); assert.equal(hash(readFileSync(hub.backupCopy)), hub.backupSHA256);
  for (const k of ["release", "planTargets", "backupJobId", "backup", "planPath", "preflightReceipt", "preflightReceiptSHA256", "backupSHA256", "backupCopy"]) assert.deepEqual(bridge[k], hub[k], k);
  assert.deepEqual(f.calls.filter(c => c[0] === "preflight"), [["preflight", hub.planPath, "rel_0123abcd-g4-truenas-backup", hub.backup]]);
  assert.deepEqual(f.calls.filter(c => c[0] === "copy"), [["copy", hub.backup]]);
  assert.deepEqual(readdirSync(f.home).filter(n => n.endsWith("-plan.json")), ["rel_0123abcd-g4-truenas-plan.json"]);
  assert.deepEqual(hub.rollbackProgram, ["python3", "scripts/deploy-truenas-hub.py", "--rollback-to", "rel_prev-111111111111-hub", "--target", "hub", "--expect-sha256", "2".repeat(64)]);
  assert.deepEqual(hub.rollbackProbe, ["node", "scripts/release-probe.mjs", "rollback", "hub", "--expect-release", "rel_prev-111111111111-hub", "--expect-sha", "2".repeat(64), "--config", f.deps.configPath]);
  assert.deepEqual(bridge.rollbackProgram, ["python3", "scripts/deploy-truenas-hub.py", "--rollback-to", "20260929-live", "--target", "bridge", "--expect-sha256", "3".repeat(64)]);
  assert.deepEqual(bridge.rollbackProbe, ["node", "scripts/release-probe.mjs", "rollback", "bridge", "--expect-release", "20260929-live", "--expect-sha", "3".repeat(64), "--config", f.deps.configPath]);
  assert.equal(hub.rollbackSafe, true); assert.equal(bridge.rollbackSafe, true);
  const planText = readFileSync(hub.planPath, "utf8"), plan = JSON.parse(planText);
  assert.equal(statSync(hub.planPath).mode & 0o777, 0o600);
  assert.deepEqual(plan.deployment.targets, ["hub", "bridge"]); assert.equal(plan.deployment.releaseName, release); assert.equal(plan.backupDestination, hub.backup); assert.equal(plan.requestId, "rel_0123abcd-g4-truenas-backup");
  assert.equal(plan.deployment.binaryDestination, `${BASE}/releases/${release}/tailterm-hub`); assert.equal(plan.deployment.bridgeBinaryDestination, `${BASE}/releases/${release}/tailterm-discord`);
  assert.ok(!planText.includes("rel_prev-111111111111-hub") && !planText.includes("20260929-live"), "no live release is pinned");
  assert.deepEqual(m.targets.mini, { release: `rel_0123abcd-${sha12}-mini`, rollbackSafe: true, rollbackProbe: ["node", "scripts/release-probe.mjs", "rollback", "mini", "--expect-sha", "4".repeat(64), "--config", f.deps.configPath] });
  assert.ok(!raw.includes(SECRET) && !JSON.stringify(out).includes(SECRET));
  assert.deepEqual(readdirSync(f.home).filter(n => n !== "plan-template.json").sort(), [...["inputs.json", "truenas-backup.sqlite", "truenas-plan.json", "truenas-preflight.json"].map(n => "rel_0123abcd-g4-" + n), UNKEYED], "every file but the transition copy carries the job and generation");
});

test("a single changed TrueNAS target gets its own plan pinning the live partner", async () => {
  for (const [file, t, partner, field, mount] of [["hub/internal/broker/x.go", "hub", "bridge", "bridgeBinaryDestination", "20260929-live/tailterm-discord"], ["hub/internal/bridge/x.go", "bridge", "hub", "binaryDestination", "rel_prev-111111111111-hub/tailterm-hub"]]) {
    const f = setup(file), m = JSON.parse(readFileSync((await buildInputs(f.config, f.job.id, { deps: f.deps })).manifest, "utf8")), sha12 = f.commit.slice(0, 12);
    assert.deepEqual(Object.keys(m.targets), [t]);
    const target = m.targets[t], plan = JSON.parse(readFileSync(target.planPath, "utf8"));
    assert.equal(target.release, `rel_0123abcd-${sha12}-${t}`); assert.deepEqual(target.planTargets, [t]); assert.equal(target.backup, `${BASE}/backups/before-rel_0123abcd-g4-${t}.sqlite`);
    assert.deepEqual(plan.deployment.targets, [t]); assert.equal(plan.deployment[field], `${BASE}/releases/${mount}`, `${partner} keeps its live mount`);
    const own = t === "hub" ? ["binaryDestination", "tailterm-hub"] : ["bridgeBinaryDestination", "tailterm-discord"];
    assert.equal(plan.deployment[own[0]], `${BASE}/releases/${target.release}/${own[1]}`);
    assert.equal(f.calls.filter(c => c[0] === "preflight").length, 1);
  }
});

test("the manifest satisfies the deployer's exact job input binding", async () => {
  const f = setup("client/app.js"), { HostAdapter } = await import("../scripts/release-runner.mjs");
  const out = await buildInputs(f.config, f.job.id, { deps: f.deps });
  const adapter = new HostAdapter(f.config, { ...f.job, generation: 5, inputsCommit: f.commit, inputsDigest: out.sha256 });
  assert.equal(adapter.jobInputs(f.commit).targets.tailos.release, `rel_0123abcd-${f.commit.slice(0, 12)}-tailos`);
  assert.equal(adapter.jobInputs(f.commit).targets.tailos.rollbackSafe, false, "no retained dist for the live TailOS commit");
});

test("TailOS rolls back to the retained dist of the live commit", async () => {
  const f = setup("client/app.js");
  mkdirSync(join(f.home, `tailos-dist-${"5".repeat(40)}`));
  await buildInputs(f.config, f.job.id, { deps: f.deps });
  const t = JSON.parse(readFileSync(join(f.home, "rel_0123abcd-g4-inputs.json"), "utf8")).targets.tailos;
  assert.deepEqual(t.rollbackProgram, ["npx", "wrangler", "pages", "deploy", join(f.home, `tailos-dist-${"5".repeat(40)}`), "--project-name", "tailos", "--branch", "main", "--commit-hash", "5".repeat(40), "--commit-dirty=false"]);
  assert.deepEqual(t.rollbackProbe, ["node", "scripts/release-probe.mjs", "rollback", "tailos", "--expect-commit", "5".repeat(40), "--config", f.deps.configPath]);
  assert.equal(t.rollbackSafe, true);
  assert.ok(!f.calls.some(c => c[0] === "preflight"));
});

test("released receipts move baselines, so nothing selected gives an empty manifest", async () => {
  const f = setup("client/app.js");
  const released = { id: "rel_done", state: "released", receipt: { outcome: "released", commit: f.commit, targets: [{ target: "tailos", outcome: "released" }] } };
  f.deps.tt = argv => JSON.stringify(releaseReply([f.job, released],argv));
  const out = await buildInputs(f.config, f.job.id, { deps: f.deps });
  assert.deepEqual(out.targets, []); assert.deepEqual(JSON.parse(readFileSync(out.manifest, "utf8")).targets, {});
  assert.ok(!f.calls.some(c => c[0] === "probe"));
});

test("dry run prints the planned bindings with no host call or write", async () => {
  const f = setup("hub/internal/api/x.go");
  const out = await buildInputs(f.config, f.job.id, { dryRun: true, deps: f.deps }), sha12 = f.commit.slice(0, 12);
  assert.equal(out.dryRun, true); assert.equal(out.generation, 4); assert.equal(out.attempt, 0); assert.equal(out.manifest, join(f.home, "rel_0123abcd-g4-inputs.json")); assert.equal(out.command.at(-1), `rel_0123abcd-g4-inputs-${sha12}`); assert.equal(out.schemaChanged, false); assert.equal(out.commit, f.commit); assert.equal(out.acceptedCommit, f.commit);
  assert.deepEqual(Object.keys(out.targets), ["hub", "bridge", "mini"]);
  const shared = { release: `rel_0123abcd-${sha12}-truenas`, backupJobId: "rel_0123abcd", backup: `${BASE}/backups/before-rel_0123abcd-g4-truenas.sqlite`, planPath: join(f.home, "rel_0123abcd-g4-truenas-plan.json"), preflightReceipt: join(f.home, "rel_0123abcd-g4-truenas-preflight.json"), planTargets: ["hub", "bridge"] };
  assert.deepEqual(out.targets.hub, shared); assert.deepEqual(out.targets.bridge, shared);
  assert.deepEqual(f.calls.map(c => c[0]), ["tt","tt","tt"]); assert.deepEqual(readdirSync(f.home), ["plan-template.json"]);
});

// Every file in a journal directory, as its bytes' hash and mode.
const journalFiles = home => Object.fromEntries(readdirSync(home).sort().map(n => [n, { sha256: hash(readFileSync(join(home, n))), mode: statSync(join(home, n)).mode & 0o777 }]));
const SET = ["inputs.json", "truenas-backup.sqlite", "truenas-plan.json", "truenas-preflight.json"], UNKEYED = "rel_0123abcd-inputs.json";
// What the runner from before names carried a generation requires of
// ID-inputs.json (its jobInputs and its backup identity check), restated here
// because that runner is the deployer's loaded code until it restarts.
function earlierRunnerReads(home, job, commit, inputsDigest) {
  const path = join(home, job.id + "-inputs.json"), input = JSON.parse(readFileSync(path, "utf8"));
  assert.equal(hash(readFileSync(path)), inputsDigest, "the file hashes to the imported digest");
  assert.deepEqual([input.version, input.jobId, input.commit, input.acceptedCommit, input.verificationDigest], [1, job.id, commit, job.commit, job.verificationDigest]);
  const { hub, bridge } = input.targets;
  if (hub && bridge) for (const k of ["release", "planPath", "preflightReceipt", "preflightReceiptSHA256", "backup", "backupSHA256"]) assert.equal(hub[k], bridge[k], k);
  for (const t of [hub, bridge].filter(Boolean)) { assert.equal(t.backupJobId, job.id); assert.ok(t.backup.includes(job.id)); }
  return input;
}
const hostCalls = f => f.calls.filter(c => c[0] !== "tt");
// The job as the hub shows it once it was set aside and claimed again with a
// freshly verified commit: one reconciliation and a later generation.
function reclaim(f, generation) { f.job.commit = commitFile(f.r, "hub/internal/store/y.go", "again"); f.job.generation = generation; f.job.reconciliations = [{ disposition: "requeue" }]; return f.job.commit; }

test("a job set aside and claimed again writes a fresh set and leaves the earlier attempt's files as they were", async () => {
  const f = setup("hub/internal/store/x.go"); f.job.generation = 3;
  const first = await buildInputs(f.config, f.job.id, { deps: f.deps }), before = journalFiles(f.home);
  assert.deepEqual(Object.keys(before), ["plan-template.json", ...SET.map(n => "rel_0123abcd-g3-" + n), UNKEYED]);
  assert.equal(before[UNKEYED].sha256, first.sha256, "the first attempt's transition copy");
  const commit = reclaim(f, 8), second = await buildInputs(f.config, f.job.id, { deps: f.deps }), after = journalFiles(f.home);
  assert.deepEqual(Object.keys(after).sort(), ["plan-template.json", ...SET.map(n => "rel_0123abcd-g3-" + n), ...SET.map(n => "rel_0123abcd-g8-" + n), UNKEYED].sort());
  for (const [name, was] of Object.entries(before)) assert.deepEqual(after[name], was, name + " keeps its bytes and mode");
  for (const n of SET.filter(n => n !== "truenas-preflight.json")) assert.equal(after["rel_0123abcd-g8-" + n].mode, 0o600, n);
  assert.equal(second.manifest, join(f.home, "rel_0123abcd-g8-inputs.json")); assert.notEqual(second.sha256, first.sha256);
  assert.deepEqual(second.command, ["tt", "deployment", "inputs", "--job", "rel_0123abcd", "--generation", "8", "--commit", commit, "--file", second.manifest, "--request-id", `rel_0123abcd-g8-inputs-${commit.slice(0, 12)}`]);
  const old = JSON.parse(readFileSync(first.manifest, "utf8")), fresh = JSON.parse(readFileSync(second.manifest, "utf8"));
  assert.deepEqual([old.generation, old.attempt, fresh.generation, fresh.attempt], [3, 0, 8, 1]); assert.equal(fresh.commit, commit);
  assert.equal(fresh.targets.hub.backup, `${BASE}/backups/before-rel_0123abcd-g8-truenas.sqlite`); assert.equal(hash(readFileSync(fresh.targets.hub.backupCopy)), fresh.targets.hub.backupSHA256);
  assert.deepEqual(f.calls.filter(c => c[0] === "preflight").map(c => c.slice(2)), [["rel_0123abcd-g3-truenas-backup", `${BASE}/backups/before-rel_0123abcd-g3-truenas.sqlite`], ["rel_0123abcd-g8-truenas-backup", `${BASE}/backups/before-rel_0123abcd-g8-truenas.sqlite`]]);
});

test("running again at the same generation returns the written manifest with no host call or write", async () => {
  const f = setup("hub/internal/store/x.go"); f.job.generation = 3;
  await buildInputs(f.config, f.job.id, { deps: f.deps }); reclaim(f, 8);
  const out = await buildInputs(f.config, f.job.id, { deps: f.deps }), files = journalFiles(f.home), made = hostCalls(f).length;
  assert.deepEqual(await buildInputs(f.config, f.job.id, { deps: f.deps }), out);
  assert.equal(out.sha256, files["rel_0123abcd-g8-inputs.json"].sha256); assert.deepEqual(out.targets, ["hub", "bridge", "mini"]);
  assert.equal(hostCalls(f).length, made, "no probe, preflight or copy"); assert.deepEqual(journalFiles(f.home), files);
  f.job.verificationDigest = "e".repeat(64);
  await assert.rejects(buildInputs(f.config, f.job.id, { deps: f.deps }), /already written for this generation with a different binding/);
  assert.equal(hostCalls(f).length, made); assert.deepEqual(journalFiles(f.home), files);
});

test("a run that failed in preflight is finished at the same generation with the same plan, request and destination", async () => {
  const f = setup("hub/internal/store/x.go"); reclaim(f, 8);
  const preflight = f.deps.preflight, planPath = join(f.home, "rel_0123abcd-g8-truenas-plan.json"), copy = join(f.home, "rel_0123abcd-g8-truenas-backup.sqlite");
  // The backup was made, then the answer was lost before a receipt was written.
  f.deps.preflight = (plan, receipt) => { preflight(plan, receipt); throw new Error("Synthetic lost preflight answer"); };
  await assert.rejects(buildInputs(f.config, f.job.id, { deps: f.deps }), /Synthetic lost/);
  const plan = journalFiles(f.home)["rel_0123abcd-g8-truenas-plan.json"]; assert.equal(plan.mode, 0o600); assert.ok(!existsSync(join(f.home, "rel_0123abcd-g8-inputs.json")));
  // An unfinished copy of this generation is the one file that is replaced.
  writeFileSync(copy, "half a copy", { mode: 0o600 }); f.deps.preflight = preflight;
  const out = await buildInputs(f.config, f.job.id, { deps: f.deps }), m = JSON.parse(readFileSync(out.manifest, "utf8"));
  assert.deepEqual(journalFiles(f.home)["rel_0123abcd-g8-truenas-plan.json"], plan, "the plan is reused, not rewritten");
  const sent = f.calls.filter(c => c[0] === "preflight"); assert.equal(sent.length, 2); assert.deepEqual(sent[1], sent[0]);
  assert.deepEqual(sent[0], ["preflight", planPath, "rel_0123abcd-g8-truenas-backup", `${BASE}/backups/before-rel_0123abcd-g8-truenas.sqlite`]);
  assert.equal(JSON.parse(readFileSync(m.targets.hub.preflightReceipt, "utf8")).status, "already-satisfied");
  assert.deepEqual(f.calls.filter(c => c[0] === "copy"), [["copy", m.targets.hub.backup]]); assert.equal(hash(readFileSync(copy)), m.targets.hub.backupSHA256);
  assert.equal(m.generation, 8); assert.equal(out.sha256, hash(readFileSync(out.manifest)));

  const g = setup("hub/internal/store/x.go"), served = g.deps.preflight;
  g.deps.preflight = () => { throw new Error("Synthetic preflight failure"); };
  await assert.rejects(buildInputs(g.config, g.job.id, { deps: g.deps }), /Synthetic preflight/);
  const kept = journalFiles(g.home); g.deps.preflight = served;
  writeFileSync(g.config.inputs.planTemplate, JSON.stringify({ ...JSON.parse(readFileSync(g.config.inputs.planTemplate, "utf8")), databaseOwner: "another-owner" }));
  await assert.rejects(buildInputs(g.config, g.job.id, { deps: g.deps }), /^Error: Job plan already written with different inputs$/);
  assert.deepEqual(journalFiles(g.home)["rel_0123abcd-g4-truenas-plan.json"], kept["rel_0123abcd-g4-truenas-plan.json"]); assert.ok(!g.calls.some(c => c[0] === "preflight")); assert.ok(!existsSync(join(g.home, "rel_0123abcd-g4-inputs.json")));
});

test("the manifest's bytes are also left under the name without a generation when that name is free", async () => {
  const f = setup("hub/internal/store/x.go");
  const out = await buildInputs(f.config, f.job.id, { deps: f.deps }), copy = join(f.home, UNKEYED);
  assert.ok(readFileSync(copy).equals(readFileSync(out.manifest))); assert.equal(statSync(copy).mode & 0o777, 0o600); assert.equal(out.command[out.command.indexOf("--file") + 1], out.manifest, "the keyed manifest is what is imported");
  assert.equal(earlierRunnerReads(f.home, f.job, f.commit, out.sha256).generation, 4);
  // Run again: neither file changes, and a missing copy is put back.
  const files = journalFiles(f.home); assert.deepEqual(await buildInputs(f.config, f.job.id, { deps: f.deps }), out); assert.deepEqual(journalFiles(f.home), files);
  rmSync(copy); assert.deepEqual(await buildInputs(f.config, f.job.id, { deps: f.deps }), out); assert.deepEqual(journalFiles(f.home), files);
  // A file of that name from an earlier attempt stays as it is, whatever it holds, and the run succeeds.
  const g = setup("hub/internal/store/x.go"), earlier = join(g.home, UNKEYED); writeFileSync(earlier, "an earlier attempt's manifest", { mode: 0o640 }); const was = journalFiles(g.home)[UNKEYED];
  reclaim(g, 8); const second = await buildInputs(g.config, g.job.id, { deps: g.deps });
  assert.equal(second.manifest, join(g.home, "rel_0123abcd-g8-inputs.json")); assert.deepEqual(journalFiles(g.home)[UNKEYED], was); assert.equal(was.mode, 0o640);
  assert.deepEqual(await buildInputs(g.config, g.job.id, { deps: g.deps }), second); assert.deepEqual(journalFiles(g.home)[UNKEYED], was);
  // A dry run leaves no copy.
  const d = setup("hub/internal/store/x.go"); await buildInputs(d.config, d.job.id, { dryRun: true, deps: d.deps }); assert.deepEqual(readdirSync(d.home), ["plan-template.json"]);
});

test("a job whose inputs are already bound gets no second set, and a dry run still shows the keyed names", async () => {
  const f = setup("hub/internal/store/x.go"); Object.assign(f.job, { inputsCommit: f.commit, inputsDigest: "e".repeat(64) });
  await assert.rejects(buildInputs(f.config, f.job.id, { deps: f.deps }), /^Error: Job inputs already bound; set the job aside before preparing new inputs$/);
  assert.deepEqual(hostCalls(f), []); assert.deepEqual(readdirSync(f.home), ["plan-template.json"]);
  const out = await buildInputs(f.config, f.job.id, { dryRun: true, deps: f.deps });
  assert.equal(out.manifest, join(f.home, "rel_0123abcd-g4-inputs.json")); assert.equal(out.targets.hub.backup, `${BASE}/backups/before-rel_0123abcd-g4-truenas.sqlite`); assert.equal(out.targets.hub.backupCopy, join(f.home, "rel_0123abcd-g4-truenas-backup.sqlite"));
  assert.deepEqual(hostCalls(f), []); assert.deepEqual(readdirSync(f.home), ["plan-template.json"]);
});

test("inputs are refused for an unclaimed job, a changed backup copy, or an integrated commit that is not local", async () => {
  await assert.rejects(buildInputs(setup("client/a.js", { state: "verified" }).config, "rel_0123abcd", { deps: setup("client/a.js", { state: "verified" }).deps }), /claimed/);
  const f = setup("hub/internal/store/x.go"); f.deps.copyBackup = (remote, local) => writeFileSync(local, "tampered");
  await assert.rejects(buildInputs(f.config, f.job.id, { deps: f.deps }), /hash mismatch/);
  assert.ok(!existsSync(join(f.home, "rel_0123abcd-g4-inputs.json")));
  const g = setup("client/a.js"); g.job.integratedCommit = "9".repeat(40);
  await assert.rejects(buildInputs(g.config, g.job.id, { deps: g.deps }));
});

test("the inputs command reports its own refusal without host output", () => {
  const home = mkdtempSync(join(tmpdir(), "inputs-cli-")), cfg = join(home, "c.json");
  writeFileSync(cfg, JSON.stringify({ version: 1, cwd: home, journalDirectory: home, tt: join(home, "missing-tt") }));
  const r = spawnSync(process.execPath, ["scripts/release-inputs.mjs", "--config", cfg, "--job", "rel_abc"], { encoding: "utf8" });
  assert.equal(r.status, 1); assert.equal(r.stdout, ""); assert.equal(r.stderr, "release inputs refused: a host command failed\n");
});


test("complete paged history preserves all four baselines and full claimed detail",async()=>{
 const f=setup("client/app.js"),targets=["hub","bridge","mini","tailos"],history=[];
 for(let i=0;i<810;i++)history.push({id:`rel_hist_${i}`,state:"released",generation:1,receipt:{outcome:"released",commit:f.r.base,targets:targets.map(target=>({target,outcome:"released",artifactSHA256:"b".repeat(64)}))}});
 history[201]={...history[201],settledAt:"2026-10-03T00:00:00.099Z",receipt:{outcome:"released",commit:f.commit,targets:targets.map(target=>({target,outcome:"released"}))}};
 history[402]={...history[402],settledAt:"2026-10-03T00:00:00.1Z",state:"superseded",receipt:null,supersession:{releasedCommit:f.commit,targets:["hub","mini"]}};
 history[603]={...history[603],settledAt:"2026-10-03T00:00:00.100Z",state:"superseded",receipt:null,supersession:{releasedCommit:f.commit,targets:["bridge","tailos"]}};
 history[809]={...history[809],state:"rolled_back",settledAt:"2026-10-04T00:00:00Z",receipt:{outcome:"rolled_back",commit:f.r.base,targets:[{target:"hub",outcome:"rolled_back"}]}};
 history.push({id:"rel_refused",state:"refused",generation:1},{id:"rel_legacy_superseded",state:"superseded",generation:1,supersession:{releasedCommit:f.r.base}});
 const full=[history[0],f.job,...history.slice(1)],calls=[];
 const read=argv=>{calls.push(argv);return JSON.stringify(releaseReply(full,argv));};
 const summaries=readReleaseSummaries(read);
 assert.deepEqual(releaseBaselines(f.config.baselines,summaries),releaseBaselines(f.config.baselines,full));
 assert.deepEqual(releaseBaselines(f.config.baselines,summaries),Object.fromEntries(targets.map(t=>[t,f.commit])));
 assert.ok(calls.filter(x=>x.includes("settled")).length>=5);
 assert.equal(summaries.find(j=>j.id===f.job.id).plan,undefined);
 f.job.plan={commit:f.commit,checks:[{id:"full-detail"}]};f.deps.tt=read;
 const out=await buildInputs(f.config,f.job.id,{deps:f.deps,dryRun:true});assert.deepEqual(out.targets,{});
 assert.equal(calls.filter(x=>x[1]==="get").length,1);assert.deepEqual(readdirSync(f.home),["plan-template.json"]);
});

test("invalid or incomplete history refuses inputs before host writes",async()=>{
 for(const kind of ["legacy","version","view","token","repeat","duplicate","order","empty-continuation"]){
  const f=setup("client/app.js"),reads=[];
  f.deps.tt=argv=>{
   reads.push(argv);const page=releaseReply([f.job],argv);
   if(argv.includes("settled")){
    if(kind==="legacy")return "[]";
    if(kind==="version")page.version=2;
    if(kind==="view")page.page.view="active";
    if(kind==="token")page.page.snapshot="b".repeat(64);
    if(["duplicate","order"].includes(kind))page.jobs=[{...f.job,summary:true,rowId:1,state:"released"}];
    if(kind==="repeat"){page.jobs=[{id:"rel_hist",state:"released",summary:true,rowId:2}];page.page.nextAfter="same";}
    if(kind==="empty-continuation")page.page.nextAfter="more";
   }
   return JSON.stringify(page);
  };
  await assert.rejects(buildInputs(f.config,f.job.id,{deps:f.deps}),/release/i);
  assert.ok(!reads.some(a=>a[1]==="get"));assert.ok(!f.calls.some(a=>["probe","preflight","copy"].includes(a[0])));
  assert.deepEqual(readdirSync(f.home),["plan-template.json"]);
 }
});


test("changed or summary-only detail holds the input builder",async()=>{
 for(const patch of [{summary:true},{id:"rel_other"},{state:"released"},{generation:9}]){
  const f=setup("client/app.js"),read=f.deps.tt;
  f.deps.tt=argv=>argv[1]==="get"?JSON.stringify({...f.job,...patch}):read(argv);
  await assert.rejects(buildInputs(f.config,f.job.id,{deps:f.deps}),/detail changed or invalid/);
  assert.ok(!f.calls.some(a=>["probe","preflight","copy"].includes(a[0])));assert.deepEqual(readdirSync(f.home),["plan-template.json"]);
 }
});
