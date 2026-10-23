// A production publish must use the exact clean build that was verified.
import "./verify-release.mjs";
import { execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import assert from "node:assert/strict";
const repo = new URL("../", import.meta.url);
const git = (...args) =>
  execFileSync("git", args, { cwd: repo, encoding: "utf8" }).trim();
assert.equal(
  git("branch", "--show-current"),
  "main",
  "Publish production from main after merging the checked pull request.",
);
const manifest = JSON.parse(
  await readFile(new URL("../dist-static/release.json", import.meta.url)),
);
// Deploy with the wrangler pinned in package.json, installed by npm ci into
// this checkout, never a copy npx finds or fetches.
const readJSON = async (path) =>
  JSON.parse(await readFile(new URL(path, repo), "utf8"));
const pin = (await readJSON("package.json")).devDependencies?.wrangler;
const wrangler = fileURLToPath(new URL("node_modules/.bin/wrangler", repo));
const installed = existsSync(
  new URL("node_modules/wrangler/package.json", repo),
)
  ? (await readJSON("node_modules/wrangler/package.json")).version
  : null;
assert.ok(
  /^\d+\.\d+\.\d+$/.test(pin || "") &&
    installed === pin &&
    existsSync(wrangler),
  `Pinned wrangler ${pin} is not installed in this checkout; run npm ci.`,
);
execFileSync(
  wrangler,
  [
    "pages",
    "deploy",
    "dist-static",
    "--project-name",
    "tailterm",
    "--branch",
    "main",
    "--commit-hash",
    manifest.commit,
    "--commit-dirty=false",
  ],
  { cwd: repo, stdio: "inherit" },
);
