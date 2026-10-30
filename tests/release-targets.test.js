import test, {after} from "node:test";
import assert from "node:assert/strict";
import {execFileSync} from "node:child_process";
import {chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync} from "node:fs";
import {tmpdir} from "node:os";
import {join} from "node:path";
import {fileURLToPath} from "node:url";
import {candidateKnown, configuredBaselineTimes, releaseBaselines, schemaChanged, selectReleaseTargets, targetsForPaths} from "../scripts/release-targets.mjs";
import {buildInputs, deploymentBaselines} from "../scripts/release-inputs.mjs";
import {readBaselines, serveDeployment} from "../scripts/release-runner.mjs";
import {releaseReply, releaseReplySource} from "./release-runner-fixtures.mjs";

const root = fileURLToPath(new URL("..", import.meta.url));
// Fixture repositories and the code under test must use their own working
// directory, never a repository a caller's GIT_DIR, GIT_INDEX_FILE or
// GIT_WORK_TREE names (a git hook sets them), and every temp directory made
// here is removed when the file ends.
for (const k of ["GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE"]) delete process.env[k];
const temps = [];
const tempDir = prefix => { const dir = mkdtempSync(join(tmpdir(), prefix)); temps.push(dir); return dir; };
after(() => { for (const dir of temps) rmSync(dir, {recursive: true, force: true}); });

test("paths that ship or build map to the targets that consume them", () => {
  const table = {
    "hub/internal/teamplan/plan.mjs": ["mini"],
    "hub/internal/teamplan/runner.go": ["mini"],
    "hub/internal/triage/triage.go": ["hub", "mini"],
    "hub/internal/discord/gateway.go": ["bridge"],
    "hub/internal/testverification/fixture.go": [],
    "shared/tmux-command.js": ["tailos"],
    "deploy/_headers": ["tailos"],
    "deploy/LICENSE.whisper.txt": ["tailos"],
    "scripts/package-speech-model.mjs": ["tailos"],
    "scripts/build-team-plan.mjs": [],
    "server/index.js": [],
    "scripts/export-server-vault.mjs": [],
    "scripts/deploy-static.mjs": [],
    "Dockerfile": [],
    "hub/Dockerfile": [],
    "deploy/compose.yaml": [],
    "tools/jev-kit/v_A.go": [],
    "tools/claude-mods/clean-view/hooks/register.tsx": [],
    ".github/workflows/checks.yml": [],
    "hub/README.md": [],
    // Existing rules keep their targets.
    "hub/internal/api/releases.go": ["hub", "bridge", "mini"],
    "hub/cmd/tt/main.go": ["mini"],
    "client/main.js": ["tailos"],
  };
  for (const [p, want] of Object.entries(table)) assert.deepEqual(targetsForPaths([p]), want, p);
});

test("an unknown path still refuses release", () => {
  for (const p of ["unknown.xyz", "hub/internal/newpkg/x.go", "hub/cmd/newcmd/main.go", "scripts/new-tool.mjs"])
    assert.throws(() => targetsForPaths([p]), /Unknown release path/, p);
});

// -z gives each path exactly as stored: without it git quotes and escapes a
// path holding non-ASCII bytes, so a mapped path would look unknown.
const trackedPaths = cwd => execFileSync("git", ["ls-files", "-z"], {cwd, encoding: "utf8"}).split("\0").filter(Boolean);
const refusedPaths = paths => paths.filter(p => { try { targetsForPaths([p]); return false; } catch { return true; } });

test("every tracked path maps to a target or to none", () => {
  const paths = trackedPaths(root);
  assert.ok(paths.length > 0, "git ls-files returned no paths");
  const refused = refusedPaths(paths);
  assert.deepEqual(refused, [], `unmapped tracked paths:\n${refused.join("\n")}`);
});

test("non-ASCII and spaced paths map or refuse by the same rules as ASCII paths", () => {
  const table = {
    "client/caf\u00e9.js": ["tailos"],
    "client/my file.js": ["tailos"],
    "hub/cmd/tt/\u65e5\u672c main.go": ["mini"],
    "docs/r\u00e9sum\u00e9 notes.md": [],
  };
  const unknown = ["unknown/caf\u00e9.xyz", "unknown/my file.xyz", "hub/internal/new pkg/\u00fc.go"];
  for (const [p, want] of Object.entries(table)) assert.deepEqual(targetsForPaths([p]), want, p);
  for (const p of unknown) assert.throws(() => targetsForPaths([p]), /Unknown release path/, p);

  // The same paths, tracked in a real repository and read back the way the tree test reads them.
  const cwd = tempDir("release-targets-paths-");
  execFileSync("git", ["init", "-q"], {cwd});
  const all = [...Object.keys(table), ...unknown];
  for (const p of all) { mkdirSync(join(cwd, p, ".."), {recursive: true}); writeFileSync(join(cwd, p), p); }
  execFileSync("git", ["add", "."], {cwd});
  const tracked = trackedPaths(cwd);
  assert.deepEqual(tracked.toSorted(), all.toSorted());
  assert.deepEqual(refusedPaths(tracked).toSorted(), unknown.toSorted());
});

const all = commit => Object.fromEntries(["hub", "bridge", "mini", "tailos"].map(t => [t, commit]));
const receipt = (commit, targets, settledAt) => ({state: "released", ...(settledAt ? {settledAt} : {}), receipt: {outcome: "released", commit, targets: targets.map(target => ({target, outcome: "released"}))}});
const superseded = (releasedCommit, targets, settledAt) => ({state: "superseded", ...(settledAt ? {settledAt} : {}), supersession: {releasedCommit, release: "hand", ...(targets ? {targets} : {})}});
const sha = c => c.repeat(40);

test("b1 a superseded job advances exactly the targets its hand release shipped", () => {
  const out = releaseBaselines(all(sha("0")), [superseded(sha("h"), ["hub", "bridge"], "2026-09-30T12:00:00Z")]);
  assert.deepEqual(out, {...all(sha("0")), hub: sha("h"), bridge: sha("h")});
});

// A temporary repository: base, then a hand-released CLI change, then a next
// candidate that changes only the TailOS client.
function handReleaseRepo() {
  const cwd = tempDir("release-targets-");
  const git = (...args) => execFileSync("git", args, {cwd, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"]}).trim();
  const commit = (file, text) => { mkdirSync(join(cwd, file, ".."), {recursive: true}); writeFileSync(join(cwd, file), text); git("add", "."); git("commit", "-m", file); return git("rev-parse", "HEAD"); };
  git("init", "-b", "tasks-hub"); git("config", "user.email", "fixture@example.invalid"); git("config", "user.name", "Fixture");
  const base = commit("client/base.js", "base");
  const hand = commit("hub/cmd/tt/main.go", "cli");
  const next = commit("client/a.js", "a");
  return {cwd, base, hand, next};
}

test("b2 after a superseded hand release the next job selects no target it shipped", () => {
  const r = handReleaseRepo();
  assert.deepEqual(selectReleaseTargets(r.cwd, releaseBaselines(all(r.base), []), r.next), ["mini", "tailos"]);
  assert.deepEqual(selectReleaseTargets(r.cwd, releaseBaselines(all(r.base), [superseded(r.hand, ["mini"], "2026-09-30T12:00:00Z")]), r.next), ["tailos"]);
});

test("b2 after a plain released receipt the next job selects no target it shipped", () => {
  const r = handReleaseRepo();
  assert.deepEqual(selectReleaseTargets(r.cwd, releaseBaselines(all(r.base), [receipt(r.hand, ["mini"], "2026-09-30T12:00:00Z")]), r.next), ["tailos"]);
  assert.deepEqual(selectReleaseTargets(r.cwd, releaseBaselines(all(r.base), [receipt(r.hand, ["mini"])]), r.next), ["tailos"], "legacy receipt without settledAt");
});

test("b2 baselines apply in settledAt order, not list order", () => {
  const older = "2026-09-30T10:00:00Z", newer = "2026-09-30T11:00:00Z";
  assert.equal(releaseBaselines(all(sha("0")), [superseded(sha("s"), ["hub"], newer), receipt(sha("r"), ["hub"], older)]).hub, sha("s"), "a newer-settled superseded job beats an older receipt listed later");
  assert.equal(releaseBaselines(all(sha("0")), [receipt(sha("r"), ["hub"], newer), superseded(sha("s"), ["hub"], older)]).hub, sha("r"), "a newer receipt beats an older superseded job listed later");
});

test("b2 trimmed RFC3339Nano times compare as time: .5Z against .123Z and whole seconds", () => {
  assert.equal(releaseBaselines(all(sha("0")), [superseded(sha("s"), ["hub"], "2026-09-30T10:00:00.5Z"), receipt(sha("r"), ["hub"], "2026-09-30T10:00:00.123Z")]).hub, sha("s"));
  assert.equal(releaseBaselines(all(sha("0")), [receipt(sha("r"), ["hub"], "2026-09-30T10:00:00.123Z"), superseded(sha("s"), ["hub"], "2026-09-30T10:00:00.5Z")]).hub, sha("s"));
  // As text "10:00:01Z" sorts after "10:00:01.5Z"; as time it is earlier.
  assert.equal(releaseBaselines(all(sha("0")), [superseded(sha("s"), ["hub"], "2026-09-30T10:00:01.5Z"), receipt(sha("r"), ["hub"], "2026-09-30T10:00:01Z")]).hub, sha("s"));
  assert.throws(() => releaseBaselines(all(sha("0")), [receipt(sha("r"), ["hub"], "not a time")]), /settledAt/);
});

test("b2 legacy rows keep today's baselines", () => {
  const legacy = [receipt(sha("1"), ["hub", "mini"]), superseded(sha("9"), undefined), receipt(sha("2"), ["hub"]), {state: "rolled_back", receipt: {outcome: "rolled_back", commit: sha("8"), targets: [{target: "tailos", outcome: "rolled_back"}]}}];
  assert.deepEqual(releaseBaselines(all(sha("0")), legacy), {...all(sha("0")), hub: sha("2"), mini: sha("1")});
  // A timed event applies after every legacy one, wherever it is listed.
  assert.equal(releaseBaselines(all(sha("0")), [superseded(sha("s"), ["hub"], "2026-09-30T10:00:00Z"), ...legacy]).hub, sha("s"));
});

// c1 (wi_bc1fadbb4a501919): a configured baseline with a baselinesAt time is
// an event at that time, ordered with receipts and superseded jobs.
const NOW = Date.parse("2026-10-28T20:00:00Z");
const timed = (baselines, jobs, at) => releaseBaselines(baselines, jobs, at, NOW);

test("c1 a timed config baseline newer than a receipt wins", () => {
  const jobs = [receipt(sha("r"), ["hub"], "2026-10-28T10:00:00Z")];
  assert.equal(timed(all(sha("c")), jobs, {hub: "2026-10-28T11:00:00Z"}).hub, sha("c"), "newer than a receipt");
  assert.equal(timed(all(sha("c")), jobs, {hub: "2026-10-28T03:30:00-07:00"}).hub, sha("c"), "a zone offset is the same instant as 10:30Z");
  const hand = [superseded(sha("s"), ["mini"], "2026-10-28T10:00:00Z")];
  assert.equal(timed(all(sha("c")), hand, {mini: "2026-10-28T10:00:01Z"}).mini, sha("c"), "newer than a superseded job");
  // The config event sits among the job events by time, wherever they are listed.
  const around = [receipt(sha("1"), ["hub"], "2026-10-28T09:00:00Z"), receipt(sha("2"), ["hub"], "2026-10-28T12:00:00Z")];
  assert.equal(timed(all(sha("c")), around, {hub: "2026-10-28T10:00:00Z"}).hub, sha("2"));
  assert.equal(timed(all(sha("c")), [around[1], around[0]], {hub: "2026-10-28T13:00:00Z"}).hub, sha("c"));
});

test("c1 an older timed config baseline loses", () => {
  assert.equal(timed(all(sha("c")), [receipt(sha("r"), ["hub"], "2026-10-28T10:00:00Z")], {hub: "2026-10-28T09:59:59Z"}).hub, sha("r"), "older than a receipt");
  assert.equal(timed(all(sha("c")), [superseded(sha("s"), ["hub"], "2026-10-28T10:00:00Z")], {hub: "2026-10-28T09:00:00Z"}).hub, sha("s"), "older than a superseded job");
  // A tie to the millisecond goes to the job event, wherever it is listed.
  assert.equal(timed(all(sha("c")), [receipt(sha("r"), ["hub"], "2026-10-28T10:00:00.250Z")], {hub: "2026-10-28T10:00:00.250Z"}).hub, sha("r"), "tie goes to the receipt");
  assert.equal(timed(all(sha("c")), [superseded(sha("s"), ["hub"], "2026-10-28T10:00:00Z")], {hub: "2026-10-28T10:00:00.000Z"}).hub, sha("s"), "tie goes to the superseded job");
  // A timed config event is newer than every legacy (untimed) event.
  assert.equal(timed(all(sha("c")), [receipt(sha("l"), ["hub"])], {hub: "2020-01-01T00:00:00Z"}).hub, sha("c"), "a legacy receipt has no time");
});

test("c1 mixed targets and an untimed target", () => {
  const jobs = [receipt(sha("r"), ["hub", "bridge", "mini", "tailos"], "2026-10-28T10:00:00Z")];
  const config = {hub: sha("a"), bridge: sha("b"), mini: sha("c"), tailos: sha("d")};
  // hub newer wins, bridge older loses, mini and tailos have no time and lose.
  assert.deepEqual(timed(config, jobs, {hub: "2026-10-28T11:00:00Z", bridge: "2026-10-28T09:00:00Z"}), {hub: sha("a"), bridge: sha("r"), mini: sha("r"), tailos: sha("r")});
  // Without baselinesAt every target loses to the receipt, as before.
  assert.deepEqual(timed(config, jobs, undefined), all(sha("r")));
  assert.deepEqual(timed(config, jobs, {}), all(sha("r")));
  // A timed target with no job event keeps its configured commit.
  assert.deepEqual(timed(config, [receipt(sha("r"), ["hub"], "2026-10-28T10:00:00Z")], {mini: "2026-10-28T09:00:00Z"}), {...config, hub: sha("r")});
});

test("c1 invalid or future baseline times are refused", () => {
  const refused = at => assert.equal(reasonOf(() => timed(all(sha("c")), [], at)), "Invalid configured baseline time", String(JSON.stringify(at)));
  for (const at of [null, [], "2026-10-28T10:00:00Z", 5, new Map(), new Date(NOW)]) refused(at);
  refused({prod: "2026-10-28T10:00:00Z"});
  refused({Hub: "2026-10-28T10:00:00Z"});
  for (const time of ["2026-10-28", "2026-10-28T10:00:00", "2026-10-28 10:00:00Z", "2026-10-28T10:00Z", "2026-10-28T10:00:00+0700", "2026-13-01T00:00:00Z", "2026-02-30T00:00:00Z", "2026-10-28T25:00:00Z", "2026-10-27T24:00:00Z", "2026-10-27T24:00:00.000-07:00", "2026-10-27T23:60:00Z", "2026-10-27T23:59:60Z", "Oct 28 2026", "", 1761645600000, null])
    refused({hub: time});
  refused({hub: "2026-10-28T20:05:00.001Z"});
  refused({hub: "2027-10-28T10:00:00Z"});
  // Up to five minutes ahead is allowed: a clock slightly behind the editor's.
  assert.equal(timed(all(sha("c")), [receipt(sha("r"), ["hub"], "2026-10-28T19:00:00Z")], {hub: "2026-10-28T20:05:00Z"}).hub, sha("c"));
  assert.deepEqual(configuredBaselineTimes({tailos: "2024-02-29T12:00:00.123456789+01:00"}, NOW), {tailos: Date.parse("2024-02-29T11:00:00.123Z")});
  assert.deepEqual(configuredBaselineTimes(Object.assign(Object.create(null), {hub: "2026-10-28T10:00:00Z"}), NOW), {hub: Date.parse("2026-10-28T10:00:00Z")});
});

test("c1 the runner and input builder apply baselinesAt", () => {
  const jobs = [receipt(sha("r"), ["hub", "mini"], "2026-10-28T10:00:00.000000500Z"), superseded(sha("s"), ["tailos"], "2026-10-28T10:00:00Z")];
  const config = {hub: sha("a"), bridge: sha("b"), mini: sha("c"), tailos: sha("d")};
  const at = {hub: "2026-10-28T11:00:00Z", mini: "2026-10-28T09:00:00Z", tailos: "2026-10-28T10:30:00Z"};
  const want = {hub: sha("a"), bridge: sha("b"), mini: sha("r"), tailos: sha("d")};
  assert.deepEqual(releaseBaselines(config, jobs, at), want);
  assert.deepEqual(deploymentBaselines(config, jobs, at), want, "the handler input builder and the deployer poll");
  assert.deepEqual(deploymentBaselines(config, jobs), releaseBaselines(config, jobs), "no baselinesAt");
  // The deployer's poll reads the private config file each time, so an edit
  // that adds or changes baselinesAt applies at the next poll.
  const dir = tempDir("release-targets-config-"), path = join(dir, "deploy.json");
  const poll = () => { const {baselines, at: configured} = readBaselines(path); return deploymentBaselines(baselines, jobs, configured); };
  writeFileSync(path, JSON.stringify({version: 1, baselines: config}));
  assert.deepEqual(poll(), releaseBaselines(config, jobs), "an old-format config");
  assert.deepEqual(poll(), {...all(sha("r")), bridge: sha("b"), tailos: sha("s")});
  writeFileSync(path, JSON.stringify({version: 1, baselines: config, baselinesAt: at}));
  assert.deepEqual(poll(), want, "after an edit adds baselinesAt");
  writeFileSync(path, JSON.stringify({version: 1, baselines: config, baselinesAt: {...at, mini: "2026-10-28T12:00:00Z"}}));
  assert.deepEqual(poll(), {...want, mini: sha("c")}, "after an edit changes one time");
  for (const bad of [{hub: "soon"}, {prod: "2026-10-28T11:00:00Z"}, [], null, {hub: "2999-01-01T00:00:00Z"}]) {
    writeFileSync(path, JSON.stringify({version: 1, baselines: config, baselinesAt: bad}));
    assert.equal(reasonOf(() => readBaselines(path)), "Invalid configured baseline time", JSON.stringify(bad));
  }
});

test("c1 the deployer poll applies a baselinesAt edit at the next poll, with no restart", async () => {
  const home = tempDir("release-targets-poll-"), configPath = join(home, "deploy.json"), fakeTT = join(home, "tt");
  const config = {version: 1, enabled: true, cwd: home, journalDirectory: home, tt: fakeTT, baselines: all(sha("a"))};
  const shipped = {id: "rel_done", state: "released", generation: 2, settledAt: "2026-10-28T10:00:00.5Z", receipt: {outcome: "released", commit: sha("r"), targets: ["hub", "mini"].map(target => ({target, outcome: "released"}))}};
  const verified = {id: "rel_next", state: "verified", generation: 1, commit: sha("n")};
  writeFileSync(fakeTT, "#!" + process.execPath + "\n" + releaseReplySource + `const a=process.argv.slice(2);if(['list','get'].includes(a[1]))console.log(JSON.stringify(releaseReply(${JSON.stringify([shipped, verified])},a)));else if(a[1]==='claim')console.log(JSON.stringify(${JSON.stringify({...verified, state: "claimed", generation: 2, agentId: process.env.TAILTERM_AGENT, runId: process.env.TAILTERM_RUN})}));else process.exit(2);`);
  chmodSync(fakeTT, 0o755);
  const seen = [], release = async c => { seen.push(c.baselines); };
  writeFileSync(configPath, JSON.stringify(config));
  await serveDeployment(config, {once: true, configPath, release});
  // The daemon keeps the config it started with; only the file is edited.
  writeFileSync(configPath, JSON.stringify({...config, baselinesAt: {hub: "2026-10-28T11:00:00Z", mini: "2026-10-28T10:00:00.4Z"}}));
  await serveDeployment(config, {once: true, configPath, release});
  assert.deepEqual(seen, [{...all(sha("a")), hub: sha("r"), mini: sha("r")}, {...all(sha("a")), mini: sha("r")}]);
});

test("c1 the handler input builder selects targets from baselinesAt", async () => {
  // mini was hand-released at r.hand after an older receipt for r.base; the
  // next job changes only the client, so a timed config edit ships TailOS only.
  const r = handReleaseRepo();
  const job = {id: "rel_0123abcd", state: "claimed", generation: 4, commit: r.next, verificationDigest: "d".repeat(64), itemId: "wi_fixture"};
  const older = {id: "rel_older", state: "released", generation: 2, settledAt: "2026-10-28T10:00:00Z", receipt: {outcome: "released", commit: r.base, targets: [{target: "mini", outcome: "released"}]}};
  const deps = {tt: argv => JSON.stringify(releaseReply([job, older], argv)), git: argv => execFileSync("git", argv, {cwd: r.cwd, encoding: "utf8"})};
  const config = {version: 1, enabled: true, cwd: r.cwd, journalDirectory: tempDir("release-targets-inputs-"), tt: "tt", baselines: {...all(r.base), mini: r.hand}};
  const targetsOf = async c => Object.keys((await buildInputs(c, job.id, {dryRun: true, deps})).targets);
  assert.deepEqual(await targetsOf(config), ["mini", "tailos"], "an untimed edit loses to the older receipt");
  assert.deepEqual(await targetsOf({...config, baselinesAt: {mini: "2026-10-28T11:00:00Z"}}), ["tailos"]);
  assert.deepEqual(await targetsOf({...config, baselinesAt: {mini: "2026-10-28T09:00:00Z"}}), ["mini", "tailos"], "an older timed edit loses");
  await assert.rejects(targetsOf({...config, baselinesAt: {mini: "yesterday"}}), /Invalid configured baseline time/);
});

// The unknown-path refusal and the candidate's own map (wi_fd1a6890a531e2c1).
const REASON = /^[A-Za-z0-9 ,.:;()_\/-]{1,160}$/;
const reasonOf = fn => { try { fn(); } catch (error) { assert.equal(error.message, error.releaseReason); return error.releaseReason; } assert.fail("did not throw"); };

test("t1 an unknown, invalid or baseline-less selection throws a tagged reason the runner can journal", () => {
  assert.equal(reasonOf(() => targetsForPaths(["zzz/new.js"])), "Unknown release path: zzz/new.js");
  assert.equal(reasonOf(() => targetsForPaths(["client/a.js", "zzz/new.js", "zzz/other.js"])), "Unknown release path: zzz/new.js and 1 more");
  assert.equal(reasonOf(() => targetsForPaths(["zzz/my file.js", "zzz/café.js"])), "Unknown release path (2 paths, names withheld)");
  assert.equal(reasonOf(() => targetsForPaths(["zzz/my file.js"])), "Unknown release path (1 path, names withheld)");
  // The first path that may be named is named, wherever it is listed.
  assert.equal(reasonOf(() => targetsForPaths(["zzz/my file.js", "zzz/ok.js"])), "Unknown release path: zzz/ok.js and 1 more");
  const long = "zzz/" + "a".repeat(116), tooLong = long + "a";
  assert.equal(reasonOf(() => targetsForPaths([long, "zzz/b.js"])), `Unknown release path: ${long} and 1 more`);
  assert.equal(reasonOf(() => targetsForPaths([tooLong])), "Unknown release path (1 path, names withheld)");
  assert.equal(reasonOf(() => targetsForPaths(["../etc/passwd"])), "Invalid release path");
  assert.equal(reasonOf(() => selectReleaseTargets(root, {hub: "x"}, "HEAD")), "Missing last successful target baseline");
  for (const paths of [["zzz/new.js"], ["zzz/new.js", "zzz/b.js"], ["zzz/my file.js"], [long, ...Array.from({length: 1000}, (_, i) => `zzz/${i}.js`)], ["/abs"]]) assert.match(reasonOf(() => targetsForPaths(paths)), REASON);
});

// A stand-in for a candidate's scripts/release-targets.mjs: no imports, and
// the body decides what it says about one path.
const standIn = body => `export function targetsForPaths(paths) {\n  const p = paths[0];\n  ${body}\n}\n`;
const KNOWS_FOLDER = standIn(`if (/^tools\\/newthing\\//.test(p)) return [];\n  throw new Error("Unknown release path");`);
const NEW_FILE = "tools/newthing/a.js", NEW_REASON = "Unknown release path: tools/newthing/a.js";
// A temporary repository whose HEAD is a candidate adding the given files on
// a base that holds one client file.
function candidateRepo(files) {
  const cwd = tempDir("release-targets-candidate-");
  const git = (...args) => execFileSync("git", args, {cwd, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"]}).trim();
  const commit = added => { for (const [file, text] of Object.entries(added)) { mkdirSync(join(cwd, file, ".."), {recursive: true}); writeFileSync(join(cwd, file), text); } git("add", "."); git("commit", "-m", "commit"); return git("rev-parse", "HEAD"); };
  git("init", "-b", "tasks-hub"); git("config", "user.email", "fixture@example.invalid"); git("config", "user.name", "Fixture");
  const base = commit({"client/base.js": "base"}), candidate = commit(files);
  return {cwd, git, commit, base, candidate};
}

test("t2 a path the running map does not know is accepted only when the candidate's own map maps it", () => {
  // (a) the candidate adds the folder and its rule: its map vouches for the folder.
  const a = candidateRepo({[NEW_FILE]: "a", "scripts/release-targets.mjs": KNOWS_FOLDER});
  assert.deepEqual(selectReleaseTargets(a.cwd, all(a.base), a.candidate), ["hub", "bridge", "mini", "tailos"], "the changed map file selects every target");
  // (b) the folder without a rule, and a rule that does not cover the folder.
  const b = candidateRepo({[NEW_FILE]: "a"});
  assert.equal(reasonOf(() => selectReleaseTargets(b.cwd, all(b.base), b.candidate)), NEW_REASON);
  const other = candidateRepo({[NEW_FILE]: "a", "tools/elsewhere/b.js": "b", "scripts/release-targets.mjs": KNOWS_FOLDER});
  assert.equal(reasonOf(() => selectReleaseTargets(other.cwd, all(other.base), other.candidate)), "Unknown release path: tools/elsewhere/b.js");
  // (c) a candidate map that cannot be loaded, or that prints something else.
  for (const text of ["throw new Error('broken map');\n", "export const nothing = 1;\n", KNOWS_FOLDER + "console.log('noise');\n", KNOWS_FOLDER + "process.exit(3);\n"]) {
    const c = candidateRepo({[NEW_FILE]: "a", "scripts/release-targets.mjs": text});
    assert.equal(reasonOf(() => selectReleaseTargets(c.cwd, all(c.base), c.candidate)), NEW_REASON, text);
  }
  // (d) a target outside the four names, or an answer that is not a list.
  for (const body of [`return ["prod"];`, `return ["hub", "prod"];`, `return "hub";`, `return null;`]) {
    const d = candidateRepo({[NEW_FILE]: "a", "scripts/release-targets.mjs": standIn(body)});
    assert.equal(reasonOf(() => selectReleaseTargets(d.cwd, all(d.base), d.candidate)), NEW_REASON, body);
  }
  // (e) the checkout is not the candidate: another HEAD, or an edited map file.
  const e = candidateRepo({[NEW_FILE]: "a", "scripts/release-targets.mjs": KNOWS_FOLDER});
  e.git("checkout", "--detach", e.base);
  assert.equal(reasonOf(() => selectReleaseTargets(e.cwd, all(e.base), e.candidate)), NEW_REASON, "HEAD is the base");
  e.git("checkout", "--detach", e.candidate);
  assert.deepEqual(selectReleaseTargets(e.cwd, all(e.base), e.candidate), ["hub", "bridge", "mini", "tailos"]);
  const strict = candidateRepo({[NEW_FILE]: "a", "scripts/release-targets.mjs": standIn(`throw new Error("Unknown release path");`)});
  writeFileSync(join(strict.cwd, "scripts/release-targets.mjs"), KNOWS_FOLDER);
  assert.equal(reasonOf(() => selectReleaseTargets(strict.cwd, all(strict.base), strict.candidate)), NEW_REASON, "an uncommitted edit to the map");
});

test("fixtures run without inherited git environment", () => {
  // A hook's GIT_DIR, GIT_INDEX_FILE and GIT_WORK_TREE would send the
  // candidate map's git calls to another repository; candidateKnown drops them.
  const a = candidateRepo({[NEW_FILE]: "a", "scripts/release-targets.mjs": KNOWS_FOLDER});
  const inherited = {GIT_DIR: "/nonexistent/tt-git", GIT_INDEX_FILE: "/nonexistent/tt-index", GIT_WORK_TREE: "/nonexistent/tt-tree"};
  Object.assign(process.env, inherited);
  try { assert.deepEqual(targetsForPaths([NEW_FILE], candidateKnown(a.cwd, a.candidate)), []); }
  finally { for (const k of Object.keys(inherited)) delete process.env[k]; }
});

test("t2 a candidate map that traps SIGTERM and never exits is killed at the timeout and leaves the path unknown", () => {
  const hung = candidateRepo({[NEW_FILE]: "a", "scripts/release-targets.mjs": `process.on("SIGTERM", () => {});\nawait new Promise(() => setInterval(() => {}, 1000));\n` + KNOWS_FOLDER});
  const started = Date.now();
  assert.equal(reasonOf(() => targetsForPaths([NEW_FILE], candidateKnown(hung.cwd, hung.candidate, {timeoutMs: 1500}))), NEW_REASON);
  const took = Date.now() - started;
  assert.ok(took >= 1400 && took < 15000, `returned after ${took} ms`);
  // The same map without the wait answers, so the refusal above is the kill.
  const quick = candidateRepo({[NEW_FILE]: "a", "scripts/release-targets.mjs": `process.on("SIGTERM", () => {});\n` + KNOWS_FOLDER});
  assert.deepEqual(targetsForPaths([NEW_FILE], candidateKnown(quick.cwd, quick.candidate)), []);
});

test("t2 the running map is the floor: the candidate's map adds targets for unknown paths and removes none", () => {
  // (f) the candidate's map says nothing ships, for every path it is asked.
  const f = candidateRepo({[NEW_FILE]: "a", "scripts/release-targets.mjs": standIn(`return [];`)});
  const next = f.commit({"client/x.js": "x"});
  assert.deepEqual(selectReleaseTargets(f.cwd, {...all(next), tailos: f.candidate}, next), ["tailos"], "client/x.js still ships TailOS");
  const asked = [];
  assert.deepEqual(targetsForPaths(["client/x.js", "docs/a.md", NEW_FILE, "tools/newthing/b.js"], (p, pending) => { asked.push([p, pending]); return p === NEW_FILE ? ["mini"] : []; }), ["mini", "tailos"]);
  assert.deepEqual(asked, [[NEW_FILE, [NEW_FILE, "tools/newthing/b.js"]], ["tools/newthing/b.js", [NEW_FILE, "tools/newthing/b.js"]]], "only the paths no running rule matches are asked");
  // The invalid path rule is not the candidate's to relax, and an invalid path is never asked.
  for (const p of ["../x.js", "/abs/x.js", "tools/../x.js", "a\nb"]) assert.equal(reasonOf(() => targetsForPaths([NEW_FILE, p], () => [])), "Invalid release path", JSON.stringify(p));
  assert.equal(reasonOf(() => targetsForPaths(["tools/../x.js"], () => assert.fail("asked"))), "Invalid release path");
});

test("t3 an invalid settledAt and a failed git diff throw tagged reasons the runner can journal", () => {
  assert.equal(reasonOf(() => releaseBaselines(all(sha("a")), [{id: "rel_bad", state: "released", settledAt: "not a time", receipt: {outcome: "released", commit: sha("b"), targets: []}}])), "Invalid release settledAt");
  // A baseline commit the repository lacks: git diff fails, and git's own
  // message (which names the commit) is not carried.
  const r = candidateRepo({"client/next.js": "next"}), missing = "f".repeat(40);
  assert.equal(reasonOf(() => selectReleaseTargets(r.cwd, {...all(r.base), mini: missing}, r.candidate)), "git diff failed");
  assert.equal(reasonOf(() => schemaChanged(r.cwd, missing, r.candidate)), "git diff failed");
  assert.equal(schemaChanged(r.cwd, r.base, r.candidate), false, "a diff that works is unchanged");
});
