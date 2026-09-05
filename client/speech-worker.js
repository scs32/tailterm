import { pipeline, env } from "@huggingface/transformers";
import {
  SPEECH_MODEL,
  SPEECH_REVISION,
  SPEECH_CACHE,
  SPEECH_RUNTIME,
} from "./speech-config.js";
env.allowLocalModels = false;
env.useWasmCache = false;
env.cacheKey = SPEECH_CACHE;
env.backends.onnx.wasm.numThreads = 1;
env.backends.onnx.wasm.proxy = false;
const root = `${self.location.origin}/speech-runtime/${SPEECH_RUNTIME}/`;
env.backends.onnx.wasm.wasmPaths = {
  mjs: root + "ort-wasm-simd-threaded.mjs",
  wasm: root + "ort-wasm-simd-threaded.wasm",
};
let recognizer,
  busy = false;
self.onmessage = async ({ data }) => {
  const { id, type, audio } = data;
  if (busy) {
    self.postMessage({ id, type: "error", message: "Speech engine is busy." });
    return;
  }
  busy = true;
  try {
    if (type === "load") {
      recognizer ||= await pipeline(
        "automatic-speech-recognition",
        SPEECH_MODEL,
        {
          revision: SPEECH_REVISION,
          device: "wasm",
          dtype: "q8",
          // ORT 1.26 extended QDQ optimization breaks Whisper tied embeddings.
          session_options: { graphOptimizationLevel: "basic" },
          progress_callback: (p) =>
            self.postMessage({
              id,
              type: "progress",
              message:
                p.status === "progress"
                  ? `Downloading speech model: ${Math.round(p.progress || 0)}% (${p.file})`
                  : "Preparing local speech model...",
            }),
        },
      );
      self.postMessage({ id, type: "result", value: true });
    } else if (type === "transcribe") {
      if (!recognizer) throw Error("Load the speech model first.");
      if (
        !(audio instanceof Float32Array) ||
        !audio.length ||
        audio.length > 16000 * 60
      )
        throw Error("Record up to 60 seconds of speech.");
      const result = await recognizer(audio, {
        chunk_length_s: 30,
        stride_length_s: 5,
        return_timestamps: false,
        max_new_tokens: 256,
      });
      self.postMessage({ id, type: "result", value: result.text.trim() });
    }
  } catch (e) {
    self.postMessage({ id, type: "error", message: e.message || String(e) });
  } finally {
    busy = false;
  }
};
