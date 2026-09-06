import { readFile, readdir, writeFile } from "node:fs/promises";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
const repository = new URL("../", import.meta.url);
const git = (...args) =>
  execFileSync("git", args, { cwd: repository, encoding: "utf8" }).trim();
export const sha256 = (bytes) =>
  createHash("sha256").update(bytes).digest("hex");
export async function assetInventory(root, prefix = "") {
  const files = {};
  for (const entry of await readdir(new URL(prefix, root), {
    withFileTypes: true,
  })) {
    const name = prefix + entry.name;
    if (entry.isDirectory())
      Object.assign(files, await assetInventory(root, name + "/"));
    else if (name !== "release.json") {
      if (!entry.isFile()) throw Error("Release assets must be regular files.");
      const bytes = await readFile(new URL(name, root));
      files[name] = { size: bytes.length, sha256: sha256(bytes) };
    }
  }
  return Object.fromEntries(
    Object.entries(files).sort(([a], [b]) => a.localeCompare(b)),
  );
}
export async function writeReleaseManifest(root) {
  const lock = await readFile(new URL("package-lock.json", repository));
  const manifest = {
    schema: 1,
    source: "https://github.com/scs32/tailterm",
    commit: git("rev-parse", "HEAD"),
    dirty: !!git("status", "--porcelain", "--untracked-files=normal"),
    builtAt: new Date().toISOString(),
    node: process.version,
    packageLockSha256: sha256(lock),
    // The exact npm dependency graph and integrity records are pinned by this
    // lock hash. This manifest describes a build, not a signed attestation.
    files: await assetInventory(root),
  };
  await writeFile(
    new URL("release.json", root),
    JSON.stringify(manifest, null, 2) + "\n",
  );
  console.log(
    `Release ${manifest.commit.slice(0, 12)} (${manifest.dirty ? "uncommitted changes; not deployable" : "clean source"}); ${Object.keys(manifest.files).length} asset hashes recorded.`,
  );
}
