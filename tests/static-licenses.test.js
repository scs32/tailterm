// wi_34a97d2f211f9be0 / order #42188: Go licence texts are copied at WASM
// build time, and static packaging and release verification fail closed on a
// module without one. Temp directories only; a stand-in go replaces the
// toolchain, so no build or download runs.
import test from "node:test";
import assert from "node:assert/strict";
import { spawnSync, execFileSync } from "node:child_process";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { assetInventory, sha256 } from "../scripts/release-manifest.mjs";

const repo = fileURLToPath(new URL("../", import.meta.url));
const childEnv = (extra = {}) => ({
  ...Object.fromEntries(
    Object.entries(process.env).filter(([key]) => !/^(TAILTERM_|GIT_|GO)/.test(key)),
  ),
  ...extra,
});
function tempDir(t, prefix) {
  const dir = mkdtempSync(join(tmpdir(), prefix));
  t.after(() => {
    execFileSync("chmod", ["-R", "u+w", dir]);
    rmSync(dir, { recursive: true, force: true });
  });
  return dir;
}
const write = (path, text, mode) => {
  mkdirSync(join(path, ".."), { recursive: true });
  writeFileSync(path, text);
  if (mode) chmodSync(path, mode);
};
const version = /^source="\$root\/\.build\/tailscale-(\d+\.\d+\.\d+)"$/m.exec(
  readFileSync(join(repo, "scripts/build-wasm.sh"), "utf8"),
)[1];
const GO = `#!/bin/sh
case "$1" in
  env) if [ "$2" = GOROOT ]; then echo "$FAKE_GOROOT"; else echo "fake-$2"; fi ;;
  build) out=; while [ $# -gt 0 ]; do if [ "$1" = -o ]; then out=$2; fi; shift; done; printf built > "$out" ;;
  list) cat "$FAKE_GO_LIST" ;;
  *) echo "fake go: $*" >&2; exit 2 ;;
esac
`;

// A fake checkout with the real build and packaging scripts, its Tailscale
// source already extracted, and a read-only module "cache" like Go's.
function fixture(t) {
  const dir = tempDir(t, "static-licenses-"),
    cwd = join(dir, "checkout"),
    cache = join(dir, "cache"),
    bin = join(dir, "bin"),
    goroot = join(dir, "goroot"),
    list = join(dir, "go-list.txt");
  for (const name of [
    "scripts/build-wasm.sh",
    "scripts/package-static.mjs",
    "scripts/package-speech-model.mjs",
    "scripts/release-manifest.mjs",
    "client/speech-config.js",
    "client/speech-model-manifest.json",
  ]) {
    mkdirSync(join(cwd, name, ".."), { recursive: true });
    copyFileSync(join(repo, name), join(cwd, name));
  }
  for (const name of ["upstream", "ssh", "upload", "sftp", "fixture"])
    write(join(cwd, "wasm", name + "_js.go"), "package main\n");
  const source = join(cwd, ".build", "tailscale-" + version);
  mkdirSync(join(source, "cmd/tsconnect/wasm"), { recursive: true });
  write(join(source, "LICENSE"), "Tailscale licence\n");
  write(join(goroot, "lib/wasm/wasm_exec.js"), "// wasm_exec\n");
  write(join(goroot, "LICENSE"), "Go licence\n");
  write(join(bin, "go"), GO, 0o755);
  const modules = {
    "a.example/a": { LICENSE: "A licence\n" },
    "b.example/b": { "LICENSE.txt": "B licence\n", NOTICE: "B notice\n" },
  };
  for (const [name, files] of Object.entries(modules))
    for (const [file, text] of Object.entries(files))
      write(join(cache, name, file), text, 0o444);
  for (const name of Object.keys(modules)) chmodSync(join(cache, name), 0o555);
  const lines = {
    a: `a.example/a|${join(cache, "a.example/a")}`,
    b: `b.example/b|${join(cache, "b.example/b")}`,
    ts: `tailscale.com|${source}`,
  };
  // As go list -deps prints it: one line per package, blank for the
  // standard library.
  const setList = (keys) =>
    writeFileSync(list, keys.flatMap((key) => ["", lines[key], lines[key]]).join("\n") + "\n");
  setList(["ts", "b", "a"]);
  const env = childEnv({
    PATH: bin + ":" + process.env.PATH,
    FAKE_GOROOT: goroot,
    FAKE_GO_LIST: list,
  });
  const run = (argv) => spawnSync(argv[0], argv.slice(1), { cwd, env, encoding: "utf8" });
  return {
    cwd,
    cache,
    lines,
    setList,
    build: () => run(["sh", "scripts/build-wasm.sh"]),
    pack: () => run([process.execPath, "scripts/package-static.mjs"]),
  };
}
const built = (f) => {
  const result = f.build();
  assert.equal(result.status, 0, result.stderr);
};
const copiedDir = (f) => join(f.cwd, ".build/go-licenses");
const notices = (f) => readFileSync(join(f.cwd, "dist-static/licenses/Go-dependencies.txt"), "utf8");
const expected = (texts) =>
  "Go dependencies linked into Tailterm WASM\n" +
  texts.map(([name, file, text]) => `\n\n${name} / ${file}\n\n${text}`).join("");
const missingLines = (stderr) => stderr.split("\n").filter((line) => line.includes("no licence text"));

test("t1 build-wasm.sh copies every listed module's licence texts beside a key copy of the list", (t) => {
  const f = fixture(t);
  built(f);
  const dir = copiedDir(f);
  const list = readFileSync(join(f.cwd, ".build/go-modules.txt"));
  assert.deepEqual(readFileSync(join(dir, "go-modules.txt")), list);
  const files = (name) => readdirSync(join(dir, "modules", name)).sort();
  assert.deepEqual(files("a.example/a"), ["LICENSE"]);
  assert.deepEqual(files("b.example/b"), ["LICENSE.txt", "NOTICE"]);
  assert.deepEqual(files("tailscale.com"), ["LICENSE"]);
  assert.equal(readFileSync(join(dir, "modules/b.example/b/NOTICE"), "utf8"), "B notice\n");
  assert.deepEqual(readdirSync(join(f.cwd, ".build")).filter((name) => name.endsWith(".tmp")), []);
  // The copies are writable, so the next build replaces the directory.
  f.setList(["ts", "b"]);
  built(f);
  assert.equal(existsSync(join(dir, "modules/a.example/a")), false);
  assert.equal(existsSync(join(dir, "modules/b.example/b/LICENSE.txt")), true);
  assert.deepEqual(readFileSync(join(dir, "go-modules.txt")), readFileSync(join(f.cwd, ".build/go-modules.txt")));
  assert.deepEqual(readdirSync(join(f.cwd, ".build")).filter((name) => name.endsWith(".tmp")), []);
});

test("t2 package-static packages the copied texts after the module cache is removed", (t) => {
  const f = fixture(t);
  built(f);
  execFileSync("chmod", ["-R", "u+w", f.cache]);
  rmSync(f.cache, { recursive: true });
  // No dist-static/assets: the script stops there, after the notices.
  const result = f.pack();
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /ENOENT.*assets/);
  assert.deepEqual(missingLines(result.stderr), []);
  assert.equal(
    notices(f),
    expected([
      ["a.example/a", "LICENSE", "A licence\n"],
      ["b.example/b", "LICENSE.txt", "B licence\n"],
      ["b.example/b", "NOTICE", "B notice\n"],
      ["tailscale.com", "LICENSE", "Tailscale licence\n"],
    ]),
  );
});

test("t3 package-static ignores copies made for another module list and reads the module directories", (t) => {
  const f = fixture(t);
  built(f);
  const license = join(f.cache, "a.example/a/LICENSE");
  chmodSync(join(f.cache, "a.example/a"), 0o755);
  chmodSync(license, 0o644);
  writeFileSync(license, "A licence, newer\n");
  writeFileSync(join(copiedDir(f), "go-modules.txt"), "another build\n");
  const result = f.pack();
  assert.match(result.stderr, /\.build\/go-licenses is from another build/);
  assert.deepEqual(missingLines(result.stderr), []);
  assert.match(notices(f), /a\.example\/a \/ LICENSE\n\nA licence, newer\n/);
});

test("t4 package-static fails closed, naming each module with no licence text anywhere", (t) => {
  const f = fixture(t);
  const c = join(f.cache, "c.example/c");
  write(join(c, "README"), "no licence here\n");
  f.lines.c = `c.example/c|${c}`;
  f.setList(["ts", "c", "b", "a"]);
  built(f);
  const result = f.pack();
  assert.equal(result.status, 1, result.stderr);
  const lines = missingLines(result.stderr);
  assert.equal(lines.length, 1, result.stderr);
  assert.match(lines[0], /Go module c\.example\/c \(looked in \.build\/go-licenses\/modules\/c\.example\/c and .*c\.example\/c\)/);
  assert.equal(existsSync(join(f.cwd, "dist-static/licenses/Go-dependencies.txt")), false);
  assert.doesNotMatch(result.stdout, /Static distribution prepared/);
});

test("t5 verify-release refuses notices without an entry for every listed module", async (t) => {
  const dir = tempDir(t, "static-licenses-release-");
  for (const name of ["scripts/verify-release.mjs", "scripts/release-manifest.mjs"]) {
    mkdirSync(join(dir, name, ".."), { recursive: true });
    copyFileSync(join(repo, name), join(dir, name));
  }
  write(join(dir, "package-lock.json"), "{}\n");
  write(join(dir, ".gitignore"), "dist-static/\n.build/\n");
  const env = childEnv();
  const git = (...args) =>
    execFileSync("git", ["-c", "user.name=t", "-c", "user.email=t@example.invalid", ...args], { cwd: dir, env, encoding: "utf8" }).trim();
  git("init", "-q");
  git("add", ".");
  git("commit", "-q", "-m", "fixture");
  const site = join(dir, "dist-static");
  write(join(site, "index.html"), "<!doctype html>\n");
  write(join(dir, ".build/go-modules.txt"), "\na.example/a|/gone/a\na.example/a|/gone/a\nb.example/b|/gone/b\n");
  const verify = async (text) => {
    write(join(site, "licenses/Go-dependencies.txt"), text);
    write(join(site, "release.json"), JSON.stringify({
      schema: 1,
      commit: git("rev-parse", "HEAD"),
      dirty: false,
      packageLockSha256: sha256(readFileSync(join(dir, "package-lock.json"))),
      files: await assetInventory(pathToFileURL(site + "/")),
    }));
    return spawnSync(process.execPath, ["scripts/verify-release.mjs"], { cwd: dir, env, encoding: "utf8" });
  };
  const full = expected([["a.example/a", "LICENSE", "A\n"], ["b.example/b", "COPYING", "B\n"]]);
  const ok = await verify(full);
  assert.equal(ok.status, 0, ok.stderr);
  assert.match(ok.stdout, /^Verified /);
  const short = await verify(expected([["a.example/a", "LICENSE", "A\n"]]));
  assert.notEqual(short.status, 0);
  assert.match(short.stderr, /no licence entry for: b\.example\/b\b/);
  assert.doesNotMatch(short.stderr, /for: .*a\.example/);
  rmSync(join(dir, ".build/go-modules.txt"));
  const unlisted = await verify(full);
  assert.notEqual(unlisted.status, 0);
  assert.match(unlisted.stderr, /Missing \.build\/go-modules\.txt/);
});


// wi_2ee62670a765ceb0 / order #50444: the container build packages the same
// copied texts, so its web stage takes both from the wasm stage before it
// builds.
test("t6 Dockerfile.static copies the module list and licence texts into the web stage before build:static", () => {
  const lines = readFileSync(join(repo, "Dockerfile.static"), "utf8").split("\n");
  const start = lines.findIndex((line) => /^FROM \S+ AS web$/.test(line));
  assert.notEqual(start, -1, "no web stage");
  let end = lines.findIndex((line, i) => i > start && /^FROM /.test(line));
  if (end === -1) end = lines.length;
  const stage = lines.slice(start + 1, end);
  const at = (text) => stage.indexOf(text);
  const build = at("RUN npm run build:static");
  assert.notEqual(build, -1, "no build:static in the web stage");
  for (const copy of [
    "COPY --from=wasm /app/.build/go-modules.txt .build/go-modules.txt",
    "COPY --from=wasm /app/.build/go-licenses .build/go-licenses",
  ]) {
    assert.notEqual(at(copy), -1, `web stage lacks: ${copy}`);
    assert.ok(at(copy) < build, `after build:static: ${copy}`);
  }
});
