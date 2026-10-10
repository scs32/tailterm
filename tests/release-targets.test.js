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
