import recorderURL from "./speech-recorder.js?url&no-inline";
export async function recordSpeech({ cancelled, onLimit, onLevel }) {
  if (!navigator.mediaDevices?.getUserMedia || !globalThis.AudioWorkletNode)
    throw Error(
      "This browser does not support microphone dictation. Try a current Safari or Chrome browser.",
    );
  const stream = await navigator.mediaDevices.getUserMedia({
    audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true },
    video: false,
  });
  let context,
    source,
    node,
    gain,
    timer,
    parts = [],
    length = 0,
    flush;
  const release = () => {
    clearTimeout(timer);
    stream.getTracks().forEach((t) => t.stop());
    source?.disconnect();
    node?.disconnect();
    gain?.disconnect();
    void context?.close().catch(() => {});
  };
  try {
    if (cancelled()) throw Error("Dictation cancelled.");
    context = new AudioContext();
    await context.resume();
    await context.audioWorklet.addModule(recorderURL);
    if (cancelled()) throw Error("Dictation cancelled.");
    node = new AudioWorkletNode(context, "tailterm-dictation-recorder");
    node.port.onmessage = ({ data }) => {
      if (data.flushed) {
        flush?.();
        return;
      }
      if (data.samples && length < context.sampleRate * 60) {
        if (onLevel) {
          const bands = Array.from({ length: 16 }, (_, band) => {
            const start = Math.floor((band * data.samples.length) / 16);
            const end = Math.floor(((band + 1) * data.samples.length) / 16);
            let energy = 0;
            for (let i = start; i < end; i++) energy += data.samples[i] ** 2;
            return Math.min(
              1,
              Math.sqrt(energy / Math.max(1, end - start)) * 8,
            );
          });
          onLevel(bands);
        }

        const piece = data.samples.subarray(
          0,
          Math.max(0, context.sampleRate * 60 - length),
        );
        parts.push(piece);
        length += piece.length;
      }
    };
    source = context.createMediaStreamSource(stream);
    gain = context.createGain();
    gain.gain.value = 0;
    source.connect(node);
    node.connect(gain);
    gain.connect(context.destination);
    timer = setTimeout(onLimit, 60000);
    const snapshot = async () => {
      const rate = context.sampleRate;
      const joined = new Float32Array(length);
      let offset = 0;
      for (const part of parts) {
        joined.set(part, offset);
        offset += part.length;
      }
      const offline = new OfflineAudioContext(
        1,
        Math.ceil((length * 16000) / rate),
        16000,
      );
      const buffer = offline.createBuffer(1, length, rate);
      buffer.copyToChannel(joined, 0);
      const input = offline.createBufferSource();
      input.buffer = buffer;
      input.connect(offline.destination);
      input.start();
      const result = await offline.startRendering();
      return result.getChannelData(0).slice();
    };
    let stopped = false;
    return {
      snapshot,
      cancel: () => {
        stopped = true;
        release();
        parts = [];
      },
      stop: async () => {
        if (stopped) throw Error("Recording already stopped.");
        stopped = true;
        clearTimeout(timer);
        stream.getTracks().forEach((t) => t.stop());
        source.disconnect();
        await new Promise((resolve) => {
          const timeout = setTimeout(resolve, 500);
          flush = () => {
            clearTimeout(timeout);
            resolve();
          };
          node.port.postMessage("flush");
        });
        const rate = context.sampleRate;
        release();
        if (cancelled()) {
          parts = [];
          throw Error("Dictation cancelled.");
        }
        if (length < rate * 0.25)
          throw Error("Record at least a moment of speech.");
        const samples = await snapshot();
        parts = [];
        return samples;
      },
    };
  } catch (e) {
    release();
    throw e;
  }
}
