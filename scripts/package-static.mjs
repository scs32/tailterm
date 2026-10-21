import { packageSpeechModel } from "./package-speech-model.mjs";
import { writeReleaseManifest } from "./release-manifest.mjs";
import { SPEECH_RUNTIME } from "../client/speech-config.js";
import {
  readdir,
  readFile,
  writeFile,
  copyFile,
  mkdir,
  unlink,
} from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { gzipSync, brotliCompressSync, constants } from "node:zlib";
const root = new URL("../dist-static/", import.meta.url);
// Go licence notices first, so a missing text stops the build before any
// other work. Each module's texts come from the copies build-wasm.sh made
// beside the module list, while that copy of the list matches; otherwise
// from the module directory. A module with no text fails the build.
const licenseFiles = ["LICENSE", "LICENSE.txt", "LICENSE.md", "COPYING", "NOTICE"];
const goModules = await readFile(
  new URL("../.build/go-modules.txt", import.meta.url),
);
const copied = new URL("../.build/go-licenses/", import.meta.url);
let useCopied = false;
try {
  useCopied = goModules.equals(
    await readFile(new URL("go-modules.txt", copied)),
  );
  if (!useCopied)
    console.error(
      "package-static: .build/go-licenses is from another build; reading licence texts from the module directories instead.",
    );
} catch (error) {
  if (error.code !== "ENOENT") throw error;
}
const readTexts = async (dir) => {
  const texts = [];
  for (const file of licenseFiles) {
    try {
      texts.push([file, await readFile(path.join(dir, file), "utf8")]);
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
    }
  }
  return texts;
};
const modules = [
  ...new Set(goModules.toString("utf8").split("\n").filter(Boolean)),
]
  .sort()
  .map((item) => {
    const at = item.indexOf("|");
    return at < 0
      ? { name: item, dir: "" }
      : { name: item.slice(0, at), dir: item.slice(at + 1) };
  });
let notices = "Go dependencies linked into Tailterm WASM\n";
for (const { name, dir } of modules) {
  let texts = useCopied
    ? await readTexts(path.join(fileURLToPath(copied), "modules", name))
    : [];
  if (!texts.length && dir) texts = await readTexts(dir);
  for (const [file, text] of texts)
    notices += "\n\n" + name + " / " + file + "\n\n" + text;
}
// Checked against the text about to be written: every module has an entry.
const lines = new Set(notices.split("\n"));
const missing = modules.filter(
  ({ name }) => !licenseFiles.some((file) => lines.has(name + " / " + file)),
);
if (missing.length) {
  for (const { name, dir } of missing)
    console.error(
      `package-static: no licence text for Go module ${name} (looked in .build/go-licenses/modules/${name} and ${dir || "no module directory"})`,
    );
  process.exit(1);
}
await mkdir(new URL("licenses/", root), { recursive: true });
await writeFile(new URL("licenses/Go-dependencies.txt", root), notices);
for (const name of await readdir(new URL("assets/", root))) {
  if (!/\.(wasm|js|css)$/.test(name)) continue;
  const url = new URL("assets/" + name, root),
    raw = await readFile(url);
  await writeFile(new URL(url.href + ".gz"), gzipSync(raw, { level: 9 }));
  await writeFile(
    new URL(url.href + ".br"),
    brotliCompressSync(raw, {
      params: { [constants.BROTLI_PARAM_QUALITY]: 5 },
    }),
  );
  if (name.endsWith(".wasm")) {
    console.log(
      `WASM: ${raw.length} bytes raw; ${gzipSync(raw, { level: 9 }).length} bytes gzip; ${(await readFile(new URL(url.href + ".br"))).length} bytes Brotli (q5).`,
    );
    // Cloudflare caps individual assets at 25 MiB. The browser streams and
    // decompresses the gzip file; no application backend or large raw asset.
    await unlink(url);
    await unlink(new URL(url.href + ".br"));
  }
}
await copyFile(
  new URL("../deploy/_headers", import.meta.url),
  new URL("_headers", root),
);
await copyFile(
  new URL("../wasm/LICENSE.tailscale", import.meta.url),
  new URL("LICENSE.tailscale.txt", root),
);
await copyFile(
  new URL("../wasm/PATENTS.tailscale", import.meta.url),
  new URL("PATENTS.tailscale.txt", root),
);
await mkdir(new URL("licenses/", root), { recursive: true });
for (const pkg of [
  "@xterm/xterm",
  "@xterm/addon-fit",
  "@xterm/addon-search",
  "@xterm/addon-web-links",
  "@fontsource/jetbrains-mono",
  "@fontsource/ibm-plex-mono",
  "@fontsource/source-code-pro",
  "qrcode",
  "dijkstrajs",
]) {
  await copyFile(
    new URL(
      pkg === "onnxruntime-web"
        ? "../deploy/LICENSE.onnxruntime.txt"
        : "../node_modules/" +
            pkg +
            (pkg === "dijkstrajs" ? "/LICENSE.md" : "/LICENSE"),
      import.meta.url,
    ),
    new URL(
      "licenses/" + pkg.replaceAll("/", "-").replace("@", "") + ".txt",
      root,
    ),
  );
}
await copyFile(
  new URL("../wasm/LICENSE.go", import.meta.url),
  new URL("licenses/Go.txt", root),
);
console.log(
  "Static distribution prepared in dist-static. Serve it over HTTPS; no application backend is required.",
);

const speechRoot = new URL(`speech-runtime/${SPEECH_RUNTIME}/`, root);
await mkdir(speechRoot, { recursive: true });
for (const name of [
  "ort-wasm-simd-threaded.mjs",
  "ort-wasm-simd-threaded.wasm",
])
  await copyFile(
    new URL("../node_modules/onnxruntime-web/dist/" + name, import.meta.url),
    new URL(name, speechRoot),
  );
for (const pkg of ["@huggingface/transformers", "onnxruntime-web"])
  await copyFile(
    new URL(
      pkg === "onnxruntime-web"
        ? "../deploy/LICENSE.onnxruntime.txt"
        : "../node_modules/" + pkg + "/LICENSE",
      import.meta.url,
    ),
    new URL(
      "licenses/" + pkg.replaceAll("/", "-").replace("@", "") + ".txt",
      root,
    ),
  );

for (const name of ["cascadia-nerd", "fira-nerd"]) {
  await copyFile(
    new URL(`../client/fonts/${name}-LICENSE.txt`, import.meta.url),
    new URL(`licenses/${name}.txt`, root),
  );
}

await packageSpeechModel(root);
await copyFile(
  new URL("../deploy/LICENSE.whisper.txt", import.meta.url),
  new URL("licenses/whisper.txt", root),
);
await writeReleaseManifest(root);
