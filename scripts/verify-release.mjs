import { readFile } from "node:fs/promises";
import { execFileSync } from "node:child_process";
import { assetInventory, sha256 } from "./release-manifest.mjs";
import assert from "node:assert/strict";
const root = new URL("../dist-static/", import.meta.url);
const repo = new URL("../", import.meta.url);
const git = (...args) =>
  execFileSync("git", args, { cwd: repo, encoding: "utf8" }).trim();
const manifest = JSON.parse(await readFile(new URL("release.json", root)));
assert.equal(
  manifest.dirty,
  false,
  "Build was made from uncommitted source; rebuild after committing.",
);
assert.equal(
  git("status", "--porcelain", "--untracked-files=normal"),
  "",
  "Commit or resolve local changes before deploying.",
);
assert.equal(
  manifest.commit,
  git("rev-parse", "HEAD"),
  "Release must match the current source commit.",
);
assert.equal(
  manifest.packageLockSha256,
  sha256(await readFile(new URL("package-lock.json", repo))),
);
assert.deepEqual(
  manifest.files,
  await assetInventory(root),
  "Release assets changed after packaging.",
);
console.log(
  `Verified ${manifest.commit}: ${Object.keys(manifest.files).length} assets match release.json.`,
);
