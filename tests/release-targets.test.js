import test from "node:test";
import assert from "node:assert/strict";
import {execFileSync} from "node:child_process";
import {fileURLToPath} from "node:url";
import {targetsForPaths} from "../scripts/release-targets.mjs";

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

test("every tracked path maps to a target or to none", () => {
  const paths = execFileSync("git", ["ls-files"], {cwd: root, encoding: "utf8"}).split("\n").filter(Boolean);
  assert.ok(paths.length > 0, "git ls-files returned no paths");
  const refused = paths.filter(p => { try { targetsForPaths([p]); return false; } catch { return true; } });
  assert.deepEqual(refused, [], `unmapped tracked paths:\n${refused.join("\n")}`);
});
