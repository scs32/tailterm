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
import { gzipSync, brotliCompressSync, constants } from "node:zlib";
const root = new URL("../dist-static/", import.meta.url);
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
  "@fontsource/fira-code",
]) {
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
}
await copyFile(
  new URL("../wasm/LICENSE.go", import.meta.url),
  new URL("licenses/Go.txt", root),
);
const modules = new Set(
  (await readFile(new URL("../.build/go-modules.txt", import.meta.url), "utf8"))
    .split("\n")
    .filter(Boolean),
);
let notices = "Go dependencies linked into Tailterm WASM\n";
for (const item of [...modules].sort()) {
  const [name, dir] = item.split("|");
  for (const file of [
    "LICENSE",
    "LICENSE.txt",
    "LICENSE.md",
    "COPYING",
    "NOTICE",
  ]) {
    try {
      notices +=
        "\n\n" +
        name +
        " / " +
        file +
        "\n\n" +
        (await readFile(path.join(dir, file), "utf8"));
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
    }
  }
}
await writeFile(new URL("licenses/Go-dependencies.txt", root), notices);
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
