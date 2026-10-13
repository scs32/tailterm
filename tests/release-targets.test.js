import test from "node:test";
import assert from "node:assert/strict";
import {execFileSync} from "node:child_process";
import {mkdirSync, mkdtempSync, writeFileSync} from "node:fs";
import {tmpdir} from "node:os";
import {join} from "node:path";
import {fileURLToPath} from "node:url";
import {releaseBaselines, selectReleaseTargets, targetsForPaths} from "../scripts/release-targets.mjs";

const root = fileURLToPath(new URL("..", import.meta.url));

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
  const cwd = mkdtempSync(join(tmpdir(), "release-targets-paths-"));
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
  const cwd = mkdtempSync(join(tmpdir(), "release-targets-"));
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
  const cwd = mkdtempSync(join(tmpdir(), "release-targets-candidate-"));
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
