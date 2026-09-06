// A production publish must use the exact clean build that was verified.
import "./verify-release.mjs";
import { execFileSync } from "node:child_process";
import { readFile } from "node:fs/promises";
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
execFileSync(
  "npx",
  [
    "wrangler",
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
