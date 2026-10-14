import { readFile, writeFile, mkdir, rename, rm } from "node:fs/promises";
import { createHash, randomBytes } from "node:crypto";
import { homedir } from "node:os";
import { isAbsolute, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { SPEECH_MODEL, SPEECH_REVISION } from "../client/speech-config.js";

const defaultManifest = JSON.parse(
  await readFile(
    new URL("../client/speech-model-manifest.json", import.meta.url),
  ),
);
if (
  defaultManifest.model !== SPEECH_MODEL ||
  defaultManifest.revision !== SPEECH_REVISION
)
  throw Error("Speech model configuration and manifest differ.");
const hash = (bytes) => createHash("sha256").update(bytes).digest("hex");
function verify(bytes, item) {
  if (bytes.length !== item.size || hash(bytes) !== item.sha256)
    throw Error("Speech source integrity check failed.");
}
const intact = (bytes, item) =>
  bytes.length === item.size && hash(bytes) === item.sha256;

// The harness-owned cache root shared by every checkout on the host: an
// absolute TAILTERM_VERIFICATION_CACHE, else the user's cache directory. A
// matrix run sets the variable for its checks, whose HOME is the verifier's.
export function verificationCacheRoot(environment = process.env) {
  const named = environment.TAILTERM_VERIFICATION_CACHE;
  if (named && isAbsolute(named)) return named;
  const home = environment.HOME || homedir();
  if (process.platform === "darwin")
    return join(home, "Library", "Caches", "tailterm-verification");
  const xdg = environment.XDG_CACHE_HOME;
  return join(xdg && isAbsolute(xdg) ? xdg : join(home, ".cache"), "tailterm-verification");
}

async function readIfPresent(url) {
  try {
    return await readFile(url);
  } catch (error) {
    if (error.code === "ENOENT") return undefined;
    throw error;
  }
}

// Peers may write the same file at once: each writes its own temporary name
// and renames it over the final one, so a reader sees all or nothing.
async function writeAtomically(url, bytes) {
  await mkdir(new URL("./", url), { recursive: true });
  const temporary = new URL(
    `${url.pathname.split("/").pop()}.${process.pid}.${randomBytes(6).toString("hex")}.tmp`,
    url,
  );
  try {
    await writeFile(temporary, bytes);
    await rename(temporary, url);
  } catch (error) {
    await rm(temporary, { force: true });
    throw error;
  }
}

function downloadCause(error) {
  if (error?.name === "TimeoutError") return "timed out";
  const code = error?.cause?.code || error?.code;
  return code ? `network error ${code}` : `network error: ${error?.message || error}`;
}

// Each file comes from the shared cache, else from this checkout's own
// .build/speech-source/ copy (copied up), else from the network after a
// cache miss. Every source passes the manifest's size and sha256 check.
export async function packageSpeechModel(
  root,
  {
    manifest = defaultManifest,
    cacheRoot = join(verificationCacheRoot(), "speech-model"),
    fetch = globalThis.fetch,
  } = {},
) {
  const shared = new URL(
    `${manifest.revision}/${manifest.model}/`,
    pathToFileURL(join(cacheRoot, "/")),
  );
  const checkout = new URL("../.build/speech-source/", import.meta.url);
  const target = new URL(
    `speech-model/${manifest.revision}/${manifest.model}/`,
    root,
  );
  const counts = { files: 0, shared: 0, checkout: 0, downloaded: 0, bytes: 0 };
  await mkdir(new URL("parts/", target), { recursive: true });
  for (const [name, item] of Object.entries(manifest.files)) {
    if (isAbsolute(name) || name.split("/").includes(".."))
      throw Error("Invalid speech model file name.");
    const cached = new URL(name, shared);
    counts.files++;
    let bytes = await readIfPresent(cached);
    if (bytes && intact(bytes, item)) counts.shared++;
    else {
      bytes = await readIfPresent(new URL(name, checkout));
      if (bytes && intact(bytes, item)) counts.checkout++;
      else {
        let response;
        try {
          response = await fetch(
            `https://huggingface.co/${manifest.model}/resolve/${manifest.revision}/${name}`,
            { signal: AbortSignal.timeout(180000) },
          );
        } catch (error) {
          throw Error(
            `Speech model download failed after a cache miss: ${name} (${downloadCause(error)})`,
            { cause: error },
          );
        }
        if (!response.ok)
          throw Error(
            `Speech model download failed after a cache miss: ${name} (HTTP ${response.status}${response.statusText ? " " + response.statusText : ""})`,
          );
        bytes = Buffer.from(await response.arrayBuffer());
        verify(bytes, item);
        counts.downloaded++;
        counts.bytes += bytes.length;
      }
      await writeAtomically(cached, bytes);
    }
    verify(bytes, item);
    let offset = 0;
    for (const part of item.parts) {
      if (
        !/^parts\/[a-f0-9]{64}\.bin$/.test(part.path) ||
        part.size > 12 * 1024 * 1024
      )
        throw Error("Invalid model part.");
      const chunk = bytes.subarray(offset, offset + part.size);
      verify(chunk, part);
      await writeFile(new URL(part.path, target), chunk);
      offset += part.size;
    }
    if (offset !== bytes.length)
      throw Error("Incomplete speech model manifest.");
  }
  await writeFile(
    new URL("manifest.json", target),
    JSON.stringify(manifest, null, 2) + "\n",
  );
  console.log(
    `Speech model: files=${counts.files} shared=${counts.shared} checkout=${counts.checkout} downloaded=${counts.downloaded} bytes=${counts.bytes} cache=${fileURLToPath(shared)}`,
  );
  console.log(
    "Speech model packaged locally; source and part SHA-256 hashes verified.",
  );
  return counts;
}
