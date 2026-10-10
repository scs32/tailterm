// The clean view Claude Code mod (tools/claude-mods/clean-view, see
// docs/claude-mods.md). The manifest and "nothing loads it" tests always run.
// The validate and row filter tests need a claude binary with `plugin test`
// and skip without one (CI has none). The row filter test runs the engine's
// own test kit on a temporary copy of the mod, so the kit test file below
// never lands in the repository. CLEAN_VIEW_MOD_DIR points that one test at
// another copy of the mod, for a mutation check.
import test from "node:test";
import assert from "node:assert/strict";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const MOD = "tools/claude-mods/clean-view";
const MOD_DIR = join(ROOT, MOD);
const COMPONENTS = ["ToolUse", "ToolResult", "ToolGroup", "ToolProgress"];
// Where a launch or setup path would have to name the mod to load it.
const LAUNCH_PATHS = ["hub", "scripts", "client", "src", "package.json"];
const NO_CLAUDE = "claude with plugin test is not installed";

// An agent's identity must not reach a claude child of this test.
const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith("TAILTERM_")));
const run = (cmd, args) => spawnSync(cmd, args, { cwd: ROOT, env, encoding: "utf8", timeout: 60000 });
const git = (...args) => run("git", args);
const hasClaude = run("claude", ["plugin", "test", "--help"]).status === 0;

const KIT_TEST = `import { test, expect } from 'claude-code/testing'

const PROPS = {
  ToolUse: { tool_use_id: 't1', tool: 'Bash', input: { command: 'ls' }, isRunning: false, isErrored: false, isInterrupted: false },
  ToolResult: { tool_use_id: 't1', tool: 'Bash', output: { stdout: 'x', stderr: '' }, isErrored: false },
  ToolGroup: { calls: [], isActive: false, isExpanded: false },
  ToolProgress: { tool_use_id: 't1', kind: 'background_hint', hint: '(ctrl+b to run in background)' },
} as const
// ToolProgress is drawn on the terminal only.
const SITES = [
  ['terminal', 'ToolUse'], ['terminal', 'ToolResult'], ['terminal', 'ToolGroup'], ['terminal', 'ToolProgress'],
  ['desktop', 'ToolUse'], ['desktop', 'ToolResult'], ['desktop', 'ToolGroup'],
] as const
const HIDDEN = { type: 'Box' }
const ENGINE = { type: 'Text', children: ['ENGINE'] }

for (const [surface, component] of SITES) {
  test(component + ' on ' + surface + ' is hidden and /clean-view toggles it', async ($, on) => {
    // Stands for the engine's own row beneath the mod.
    on('ui.render', async () => ({ type: 'Text', props: {}, children: ['ENGINE'] }) as never)
    const ui = await $.ui.mount({ plugin: 'clean-view', surface, component, props: PROPS[component] as never })
    expect(await ui.drawn()).toEqual(HIDDEN)
    const off = await $.command.run({ command: 'clean-view' })
    expect(off.text).toMatch(/shown/)
    expect(await ui.drawn()).toMatchObject(ENGINE)
    const again = await $.command.run({ command: 'clean-view' })
    expect(again.text).toMatch(/hidden/)
    expect(await ui.drawn()).toEqual(HIDDEN)
    await ui.unmount()
  })
}
`;
const KIT_TEST_COUNT = 7;

test("clean view manifest names its hooks module and state contract, and the mod is four tracked files", () => {
  const manifest = JSON.parse(readFileSync(join(MOD_DIR, ".claude-plugin/plugin.json"), "utf8"));
  assert.equal(manifest.name, "clean-view");
  assert.equal(manifest.types, "./types/index.d.ts");
  assert.deepEqual(JSON.parse(readFileSync(join(MOD_DIR, "hooks/hooks.json"), "utf8")), { modules: ["./register.tsx"] });
  assert.ok(existsSync(join(MOD_DIR, "hooks/register.tsx")));
  assert.ok(existsSync(join(MOD_DIR, "types/index.d.ts")));
  const tracked = git("ls-files", "--", MOD);
  assert.equal(tracked.status, 0, tracked.stderr);
  assert.deepEqual(tracked.stdout.trim().split("\n").sort(), [
    `${MOD}/.claude-plugin/plugin.json`,
    `${MOD}/hooks/hooks.json`,
    `${MOD}/hooks/register.tsx`,
    `${MOD}/types/index.d.ts`,
  ]);
});

test("no launch or setup path names the mods folder or the plugin dirs setting", () => {
  // The release target map names the folder once, to say nothing ships from it.
  const named = git("grep", "-n", "claude-mods", "--", ...LAUNCH_PATHS);
  const lines = named.stdout.split("\n").filter(Boolean);
  assert.equal(lines.length, 1, named.stdout + named.stderr);
  assert.match(lines[0], /^scripts\/release-targets\.mjs:\d+:.*\) continue;/);
  const setting = git("grep", "-l", "CLAUDE_CODE_PLUGIN_DIRS", "--", ...LAUNCH_PATHS);
  // git grep exits 1 when nothing matches.
  assert.equal(setting.status, 1, `the setting is named in: ${setting.stdout}${setting.stderr}`);
  assert.equal(setting.stdout, "");
});

test("claude plugin validate --strict passes and reports the command and the four row kinds", (t) => {
  if (!hasClaude) return t.skip(NO_CLAUDE);
  const out = run("claude", ["plugin", "validate", "--strict", "--json", MOD_DIR]);
  assert.equal(out.status, 0, out.stdout + out.stderr);
  const report = JSON.parse(out.stdout);
  assert.equal(report.success, true);
  const parts = [report.manifest, ...report.contents];
  for (const part of parts) {
    assert.deepEqual(part.errors, [], part.file);
    assert.deepEqual(part.warnings, [], part.file);
    assert.deepEqual(part.gatingHooks, [], part.file);
  }
  const hooks = parts.flatMap(p => p.notes).find(n => n.startsWith("./register.tsx hooks:"));
  assert.ok(hooks, "no hooks note in the report");
  assert.ok(hooks.includes("session.start"), hooks);
  assert.ok(hooks.includes("command.run{command=clean-view}"), hooks);
  assert.ok(hooks.includes(`ui.render{component=${COMPONENTS.join("|")}}`), hooks);
});

test("the mod hides the four row kinds and /clean-view toggles them, under the engine's test kit", (t) => {
  if (!hasClaude) return t.skip(NO_CLAUDE);
  const dir = mkdtempSync(join(tmpdir(), "clean-view-mod-"));
  try {
    const copy = join(dir, "clean-view");
    cpSync(process.env.CLEAN_VIEW_MOD_DIR || MOD_DIR, copy, { recursive: true });
    mkdirSync(join(copy, "tests"), { recursive: true });
    writeFileSync(join(copy, "tests/clean-view.test.ts"), KIT_TEST);
    const out = run("claude", ["plugin", "test", copy]);
    const text = out.stdout + out.stderr;
    assert.equal(out.status, 0, text);
    assert.match(text, new RegExp(`\\b${KIT_TEST_COUNT} pass\\b`), text);
    assert.match(text, /\b0 fail\b/, text);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
