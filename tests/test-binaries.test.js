import test from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { buildEnvironment, buildMatrixBinaries } from "./test-binaries.mjs";

const tempDir = (t, prefix) => {
  const dir = mkdtempSync(join(tmpdir(), prefix));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
};
const FILTERED = /^(TAILTERM_|CODEX_|TT_|TMUX)/;

test("test binaries: a passed environment loses the TAILTERM_, CODEX_, TT_ and TMUX keys before go env and go build", async (t) => {
  // A stand-in go on the given PATH records every variable each call sees; this
  // process's PATH finds the real go, so only the passed environment reaches it.
  const tools = tempDir(t, "test-binaries-env-tools-"), calls = join(tools, "calls");
  writeFileSync(join(tools, "go"),
    `#!/bin/sh\nmkdir -p '${calls}'\nn=$(ls '${calls}' | wc -l | tr -d ' ')\nenv > '${calls}'/$n-"$1"\n` +
    `if [ "$1" = env ]; then printf '{"GOCACHE":"%s","GOMODCACHE":"%s"}\\n' "$GOCACHE" "$GOMODCACHE"; exit 0; fi\n` +
    `[ "$1" = build ] || exit 1\nout=\nwhile [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done\n` +
    `printf '#!/bin/sh\\nexit 0\\n' > "$out" && chmod +x "$out"\n`,
    { mode: 0o755 });
  const root = tempDir(t, "test-binaries-env-root-");
  mkdirSync(join(root, "hub/cmd/tt"), { recursive: true });
  writeFileSync(join(root, "hub/go.mod"), "module fixture\n\ngo 1.22\n");
  writeFileSync(join(root, "hub/cmd/tt/main.go"), "package main\n\nfunc main() {}\n");
  const caches = tempDir(t, "test-binaries-env-caches-");
  const env = { PATH: tools + ":/usr/bin:/bin", HOME: caches, GOCACHE: join(caches, "build"),
    GOMODCACHE: join(caches, "mod"), ORDINARY_SETTING: "kept",
    TAILTERM_TASK: "dropped", TAILTERM_TEST_BINARIES: "dropped", CODEX_HOME: "dropped", TT_AGENT: "dropped",
    TMUX: "dropped", TMUX_PANE: "dropped" };
  const prefixed = Object.keys(env).filter((key) => FILTERED.test(key));
  assert.deepEqual(prefixed.sort(), ["CODEX_HOME", "TAILTERM_TASK", "TAILTERM_TEST_BINARIES", "TMUX", "TMUX_PANE", "TT_AGENT"]);

  const binaries = await buildMatrixBinaries(root, join(caches, "out"), { env });
  assert.deepEqual(binaries.map((binary) => binary.target), ["hub", "tt"]);
  assert.deepEqual(readdirSync(calls).sort(), ["0-env", "1-build", "2-env", "3-build"]);
  const seen = readdirSync(calls).sort().map((name) => ({
    name, vars: Object.fromEntries(readFileSync(join(calls, name), "utf8").trim().split("\n")
      .map((line) => [line.slice(0, line.indexOf("=")), line.slice(line.indexOf("=") + 1)])) }));
  for (const { name, vars } of seen) {
    assert.deepEqual(Object.keys(vars).filter((key) => FILTERED.test(key)), [], `${name} saw a filtered key`);
    assert.equal(vars.ORDINARY_SETTING, "kept", `${name} lost the ordinary key`);
    assert.equal(vars.GOCACHE, env.GOCACHE);
    assert.equal(vars.HOME, caches);
  }
  // The passed environment gets exactly the filter the inherited one does.
  assert.deepEqual(Object.keys(buildEnvironment(env)).sort(),
    Object.keys(env).filter((key) => !prefixed.includes(key)).sort());
});
