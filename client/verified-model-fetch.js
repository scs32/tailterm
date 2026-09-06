// The manifest is compiled into the worker. Neither a cached response nor a
// successful HTTP response is trusted until its bytes match that manifest.
export async function verifyModelBytes(bytes, expected) {
  if (bytes.byteLength !== expected.size)
    throw Error("Speech model size mismatch.");
  const digest = Array.from(
    new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)),
    (b) => b.toString(16).padStart(2, "0"),
  ).join("");
  if (digest !== expected.sha256)
    throw Error("Speech model integrity check failed.");
  return bytes;
}

async function readBounded(response, expected) {
  if (!response.ok || !response.body)
    throw Error("Could not load the local speech model.");
  const reader = response.body.getReader();
  const bytes = new Uint8Array(expected.size);
  let offset = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      if (offset + value.length > bytes.length)
        throw Error("Speech model size mismatch.");
      bytes.set(value, offset);
      offset += value.length;
    }
    if (offset !== bytes.length) throw Error("Speech model size mismatch.");
    return await verifyModelBytes(bytes, expected);
  } catch (error) {
    await reader.cancel().catch(() => {});
    throw error;
  } finally {
    reader.releaseLock();
  }
}

export function createVerifiedModelFetch({
  manifest,
  origin,
  cacheName,
  fetcher = fetch,
  cacheStorage = globalThis.caches,
}) {
  const base = new URL(
    `/speech-model/${manifest.revision}/${manifest.model}/`,
    origin,
  );
  return async (input) => {
    const url = new URL(typeof input === "string" ? input : input.url, origin);
    if (
      url.origin !== base.origin ||
      !url.pathname.startsWith(base.pathname) ||
      url.search
    )
      throw Error("Speech model requests must use the bundled local model.");
    const name = url.pathname.slice(base.pathname.length);
    const entry = Object.hasOwn(manifest.files, name) && manifest.files[name];
    // Transformers probes optional files. Missing files must never fall back
    // to a remote model repository.
    if (!entry) return new Response(null, { status: 404 });
    const headers = {
      "Content-Type": name.endsWith(".json")
        ? "application/json"
        : "application/octet-stream",
      "Content-Length": String(entry.size),
    };
    let cache;
    try {
      cache = await cacheStorage?.open(cacheName);
    } catch {
      /* Storage may be disabled. */
    }
    if (cache) {
      try {
        const hit = await cache.match(url.href);
        if (hit)
          return new Response(await readBounded(hit, entry), { headers });
      } catch {
        await cache.delete(url.href).catch(() => {});
      }
    }
    const bytes = new Uint8Array(entry.size);
    let offset = 0;
    for (const part of entry.parts) {
      if (
        !/^parts\/[a-f0-9]{64}\.bin$/.test(part.path) ||
        part.size > 12 * 1024 * 1024 ||
        offset + part.size > bytes.length
      )
        throw Error("Invalid speech model manifest.");
      const response = await fetcher(new URL(part.path, base).href, {
        credentials: "omit",
        redirect: "error",
      });
      const chunk = await readBounded(response, part);
      bytes.set(chunk, offset);
      offset += chunk.length;
    }
    if (offset !== bytes.length) throw Error("Speech model size mismatch.");
    await verifyModelBytes(bytes, entry);
    const response = new Response(bytes, { headers });
    if (cache) await cache.put(url.href, response.clone()).catch(() => {});
    return response;
  };
}
