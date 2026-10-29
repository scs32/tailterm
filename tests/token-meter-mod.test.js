// The token meter Claude Code mod (tools/claude-mods/token-meter, see
// docs/claude-mods-token-meter.md). The manifest and "nothing loads it" tests
// always run. The validate and meter tests depend on the installed Claude
// Code, whose mod API is early access. They run only with
// TOKEN_METER_CLAUDE_CHECK=1 and otherwise skip, so the default unit run starts
// no claude process. Opted in, a claude binary without `plugin test` fails
// them. The meter test runs the engine's own test kit on a temporary copy of
// the mod, so the kit test file below never lands in the repository; its
// turn.complete events are supplied by the test, so it proves the mod's
// arithmetic and display, not how the engine fills usage. TOKEN_METER_MOD_DIR
// points that one test at another copy of the mod, for a mutation check.
import test from "node:test";
import assert from "node:assert/strict";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const MOD = "tools/claude-mods/token-meter";
const MOD_DIR = join(ROOT, MOD);
// Where a launch or setup path would have to name the mod to load it.
const LAUNCH_PATHS = ["hub", "scripts", "client", "server", "shared", "package.json"];
const OPTED_IN = process.env.TOKEN_METER_CLAUDE_CHECK === "1";
const NOT_OPTED_IN = "depends on the installed Claude Code; set TOKEN_METER_CLAUDE_CHECK=1 to run it";

// An agent's identity must not reach a claude child of this test.
const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith("TAILTERM_")));
const run = (cmd, args) => spawnSync(cmd, args, { cwd: ROOT, env, encoding: "utf8", timeout: 60000 });
const git = (...args) => run("git", args);
// Opted in, a missing or changed claude is a failure, not a skip.
const requireClaude = () => {
  const probe = run("claude", ["plugin", "test", "--help"]);
  assert.equal(probe.status, 0, `TOKEN_METER_CLAUDE_CHECK=1 needs a claude binary with plugin test: ${probe.error?.message || probe.stderr || probe.stdout || `exit ${probe.status}`}`);
};

// Totals 900, 34.5k and 1.07M; the session runs 900, 35.4k, 1.11M.
const KIT_TEST = `import { test, expect } from 'claude-code/testing'

const MAIN_A = { input_tokens: 300, output_tokens: 120, cache_read_input_tokens: 400, cache_creation_input_tokens: 80, model: 'm' }
const MAIN_B = { input_tokens: 1000, output_tokens: 2000, cache_read_input_tokens: 30000, cache_creation_input_tokens: 1500, model: 'm' }
const SUB = { input_tokens: 400000, output_tokens: 50000, cache_read_input_tokens: 600000, cache_creation_input_tokens: 20000, model: 'm' }
const TURNS = [
  { turnId: 't1', usage: MAIN_A },
  { turnId: 't2', usage: MAIN_B },
  { turnId: 't3', agentId: 'a1', usage: SUB },
  { turnId: 't4', reason: 'error' },
] as const
const LINES = [
  'tokens: turn 900 · session 900 over 1 turn',
  'tokens: turn 34.5k · session 35.4k over 2 turns',
  'tokens: turn 1.07M · session 1.11M over 3 turns',
  'tokens: turn unavailable · session 1.11M over 4 turns (1 without usage)',
]

// Stands for the engine beneath the mod: the environment it reads, the status
// line it sets, and the turn's own answer.
const engine = (on, vars) => {
  const statuses = []
  on('env.get', (_, e) => ({ value: vars[e.name] }))
  on('ui.status', (_, e) => {
    statuses.push(e.text)
    return { value: undefined }
  })
  on('turn.complete', (_, e) => ({ text: 'ENGINE ' + e.turnId }))
  return statuses
}

const raise = async ($, statuses) => {
  for (const [i, t] of TURNS.entries()) {
    const done = await $.turn.complete({ answer: 'a', durationMs: 1, isAborted: false, reason: 'answer', ...t } as never)
    expect(done.text).toBe('ENGINE ' + t.turnId)
    expect(statuses.length).toBe(i + 1)
  }
}

test('each turn sets one status line with the turn and the running session total, labelled with the item', async ($, on) => {
  const statuses = engine(on, { TAILTERM_WORK_ITEM: 'wi_meter0001', TAILTERM_AGENT_NAME: 'builder-x', TAILTERM_TASK: 'tsk_meter' })
  await raise($, statuses)
  expect(statuses).toEqual(LINES.map(line => line + ' · wi_meter0001'))
  for (const text of statuses) expect(text).not.toMatch(/turn 0\\b/)

  const report = await $.command.run({ command: 'token-meter' })
  expect(report.text).toContain('Last turn: unavailable (no usage reported)')
  expect(report.text).toContain('Session: 1105400 (input 401300, output 52120, cache read 630400, cache write 21580) over 4 turns (1 without usage)')
  expect(report.text).toContain('  main loop: 35400 (input 1300, output 2120, cache read 30400, cache write 1580)')
  expect(report.text).toContain('  subagents: 1070000 (input 400000, output 50000, cache read 600000, cache write 20000)')
  expect(report.text).toContain('Item: wi_meter0001 (agent builder-x)')
  expect(report.text).toContain('tt usage --project tsk_meter --item wi_meter0001')
})

test('with no item set the line and the report carry no item label', async ($, on) => {
  const statuses = engine(on, {})
  await raise($, statuses)
  expect(statuses).toEqual(LINES)

  const report = await $.command.run({ command: 'token-meter' })
  expect(report.text).not.toContain('Item:')
  expect(report.text).toContain('tt usage --project TASK --item ITEM')
})
`;
const KIT_TEST_COUNT = 2;

test("token meter manifest names its hooks module and state contract, and the mod is four tracked files", () => {
  const manifest = JSON.parse(readFileSync(join(MOD_DIR, ".claude-plugin/plugin.json"), "utf8"));
  assert.equal(manifest.name, "token-meter");
  assert.equal(manifest.types, "./types/index.d.ts");
  assert.deepEqual(JSON.parse(readFileSync(join(MOD_DIR, "hooks/hooks.json"), "utf8")), { modules: ["./register.ts"] });
  assert.ok(existsSync(join(MOD_DIR, "hooks/register.ts")));
  assert.ok(existsSync(join(MOD_DIR, "types/index.d.ts")));
  const tracked = git("ls-files", "--", MOD);
  assert.equal(tracked.status, 0, tracked.stderr);
  assert.deepEqual(tracked.stdout.trim().split("\n").sort(), [
    `${MOD}/.claude-plugin/plugin.json`,
    `${MOD}/hooks/hooks.json`,
    `${MOD}/hooks/register.ts`,
    `${MOD}/types/index.d.ts`,
  ]);
});

test("no launch or setup path names the token meter or the plugin dirs setting", () => {
  // git grep exits 1 when nothing matches.
  const named = git("grep", "-l", "token-meter", "--", ...LAUNCH_PATHS);
  assert.equal(named.status, 1, `the mod is named in: ${named.stdout}${named.stderr}`);
  assert.equal(named.stdout, "");
  const setting = git("grep", "-l", "CLAUDE_CODE_PLUGIN_DIRS", "--", ...LAUNCH_PATHS);
  assert.equal(setting.status, 1, `the setting is named in: ${setting.stdout}${setting.stderr}`);
  assert.equal(setting.stdout, "");
});

test("claude plugin validate --strict passes and reports exactly the three observe-only hooks", (t) => {
  if (!OPTED_IN) return t.skip(NOT_OPTED_IN);
  requireClaude();
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
  const hooks = parts.flatMap(p => p.notes).find(n => n.startsWith("./register.ts hooks:"));
  assert.ok(hooks, "no hooks note in the report");
  assert.deepEqual(hooks.slice("./register.ts hooks:".length).split(",").map(s => s.trim()).sort(), [
    "command.run{command=token-meter}",
    "session.start",
    "turn.complete",
  ]);
});

test("the mod meters supplied turns into its status line and /token-meter, under the engine's test kit", (t) => {
  if (!OPTED_IN) return t.skip(NOT_OPTED_IN);
  requireClaude();
  const dir = mkdtempSync(join(tmpdir(), "token-meter-mod-"));
  try {
    const copy = join(dir, "token-meter");
    cpSync(process.env.TOKEN_METER_MOD_DIR || MOD_DIR, copy, { recursive: true });
    mkdirSync(join(copy, "tests"), { recursive: true });
    writeFileSync(join(copy, "tests/token-meter.test.ts"), KIT_TEST);
    const out = run("claude", ["plugin", "test", copy]);
    const text = out.stdout + out.stderr;
    assert.equal(out.status, 0, text);
    assert.match(text, new RegExp(`\\b${KIT_TEST_COUNT} pass\\b`), text);
    assert.match(text, /\b0 fail\b/, text);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
