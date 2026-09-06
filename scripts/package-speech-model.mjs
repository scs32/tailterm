import { readFile, writeFile, mkdir } from "node:fs/promises";
import { createHash } from "node:crypto";
import { SPEECH_MODEL, SPEECH_REVISION } from "../client/speech-config.js";

const manifest = JSON.parse(
  await readFile(
    new URL("../client/speech-model-manifest.json", import.meta.url),
  ),
);
if (manifest.model !== SPEECH_MODEL || manifest.revision !== SPEECH_REVISION)
  throw Error("Speech model configuration and manifest differ.");
const hash = (bytes) => createHash("sha256").update(bytes).digest("hex");
function verify(bytes, item) {
  if (bytes.length !== item.size || hash(bytes) !== item.sha256)
    throw Error("Speech source integrity check failed.");
}
export async function packageSpeechModel(root) {
  const cache = new URL("../.build/speech-source/", import.meta.url);
  const target = new URL(
    `speech-model/${manifest.revision}/${manifest.model}/`,
    root,
  );
  await mkdir(new URL("parts/", target), { recursive: true });
  for (const [name, item] of Object.entries(manifest.files)) {
    const local = new URL(name, cache);
    let bytes;
    try {
      bytes = await readFile(local);
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
      const response = await fetch(
        `https://huggingface.co/${manifest.model}/resolve/${manifest.revision}/${name}`,
        { signal: AbortSignal.timeout(180000) },
      );
      if (!response.ok)
        throw Error(
          `Speech model download failed: ${name} (${response.status})`,
        );
      bytes = Buffer.from(await response.arrayBuffer());
      verify(bytes, item);
      await mkdir(new URL("./", local), { recursive: true });
      await writeFile(local, bytes);
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
    "Speech model packaged locally; source and part SHA-256 hashes verified.",
  );
}
