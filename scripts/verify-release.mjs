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
// Every Go module linked into the WASM has a licence entry in the notices.
const readRequired = async (url, name) => {
  try {
    return await readFile(url, "utf8");
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
    throw new Error(`Missing ${name}; rebuild before deploying.`);
  }
};
const goModules = await readRequired(
  new URL(".build/go-modules.txt", repo),
  ".build/go-modules.txt",
);
const notices = new Set(
  (
    await readRequired(
      new URL("licenses/Go-dependencies.txt", root),
      "dist-static/licenses/Go-dependencies.txt",
    )
  ).split("\n"),
);
const unlisted = [
  ...new Set(
    goModules
      .split("\n")
      .filter(Boolean)
      .map((item) => item.split("|")[0]),
  ),
]
  .sort()
  .filter(
    (name) =>
      !["LICENSE", "LICENSE.txt", "LICENSE.md", "COPYING", "NOTICE"].some(
        (file) => notices.has(name + " / " + file),
      ),
  );
assert.deepEqual(
  unlisted,
  [],
  "Go-dependencies.txt has no licence entry for: " + unlisted.join(", "),
);
console.log(
  `Verified ${manifest.commit}: ${Object.keys(manifest.files).length} assets match release.json.`,
);
