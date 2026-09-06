import "../wasm/wasm_exec.js";
import wasmURL from "../wasm/tailserve.wasm?url";
let runtime;
let runtimeError;
const failureHandlers = new Set();
function failed(error) {
  runtimeError = error instanceof Error ? error : new Error(String(error));
  for (const handler of failureHandlers) handler(runtimeError.message);
}
export function loadRuntime() {
  if (runtimeError) return Promise.reject(runtimeError);
  return (runtime ||= (async () => {
    const go = new globalThis.Go();
    const compressed = await fetch(wasmURL + ".gz");
    if (!compressed.ok)
      throw new Error(
        `Could not download SSH runtime (${compressed.status}). Reload to retry.`,
      );
    const moduleResponse = new Response(
      compressed.body.pipeThrough(new DecompressionStream("gzip")),
      {
        headers: { "Content-Type": "application/wasm" },
      },
    );
    const result = await WebAssembly.instantiateStreaming(
      moduleResponse,
      go.importObject,
    );
    // The Go entry point registers factories before awaiting its event loop.
    void go
      .run(result.instance)
      .then(
        () => failed(new Error("WASM runtime stopped. Reload the workspace.")),
        failed,
      );
  })().catch((error) => {
    runtime = undefined;
    throw error;
  }));
}
export async function createIPN(config) {
  if (config.panicHandler) failureHandlers.add(config.panicHandler);
  await loadRuntime();
  return globalThis.newIPN(config);
}
export async function validatePrivateKey(raw, passphrase) {
  await loadRuntime();
  const result = globalThis.tailserveValidateKey(raw, passphrase);
  if (result.error) throw new Error(result.error);
  return result;
}

export async function generatePrivateKey() {
  await loadRuntime();
  const result = globalThis.tailtermGenerateKey();
  if (result.error) throw new Error(result.error);
  return result.privateKey;
}
