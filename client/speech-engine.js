import { SPEECH_CACHE } from "./speech-config.js";
export function createSpeechEngine(progress) {
  let worker,
    sequence = 0;
  const pending = new Map();
  const cancel = () => {
    worker?.terminate();
    worker = null;
    for (const { reject, timer } of pending.values()) {
      clearTimeout(timer);
      reject(Error("Dictation cancelled."));
    }
    pending.clear();
  };
  const request = (type, audio) =>
    new Promise((resolve, reject) => {
      if (!worker) {
        worker = new Worker(new URL("./speech-worker.js", import.meta.url), {
          type: "module",
        });
        worker.onmessage = ({ data }) => {
          const job = pending.get(data.id);
          if (!job) return;
          if (data.type === "progress") {
            progress(data.message);
            return;
          }
          clearTimeout(job.timer);
          pending.delete(data.id);
          data.type === "error"
            ? job.reject(Error(data.message))
            : job.resolve(data.value);
        };
        worker.onerror = () => {
          for (const job of pending.values()) {
            clearTimeout(job.timer);
            job.reject(
              Error(
                "Local speech engine could not start. Reload and try again.",
              ),
            );
          }
          pending.clear();
          worker?.terminate();
          worker = null;
        };
      }
      const id = ++sequence;
      const timer = setTimeout(
        () => {
          reject(
            Error("Local transcription timed out. Try a shorter recording."),
          );
          pending.delete(id);
          cancel();
        },
        type === "load" ? 300000 : 180000,
      );
      pending.set(id, { resolve, reject, timer });
      worker.postMessage({ id, type, audio }, audio ? [audio.buffer] : []);
    });
  return {
    load: () => request("load"),
    transcribe: (audio) => request("transcribe", audio),
    cancel,
    removeModel: async () => {
      cancel();
      await caches.delete(SPEECH_CACHE);
    },
  };
}
