import { chromium } from "@playwright/test";
import { createServer } from "node:http";
import { readFile, readdir } from "node:fs/promises";
import { once } from "node:events";
import assert from "node:assert/strict";
const csp = (await readFile("deploy/_headers", "utf8"))
  .split("\n")
  .find((x) => x.includes("Content-Security-Policy:"))
  .split("Content-Security-Policy: ")[1];
const server = createServer(async (req, res) => {
  try {
    const name = new URL(req.url, "http://localhost").pathname;
    const file =
      name === "/fixture.wav"
        ? ".build/speech-fixture.wav"
        : "dist-static" + (name === "/" ? "/index.html" : name);
    const body = await readFile(file);
    res.writeHead(200, {
      "Content-Type": file.endsWith(".wasm")
        ? "application/wasm"
        : /\.m?js$/.test(file)
          ? "text/javascript"
          : file.endsWith(".wav")
            ? "audio/wav"
            : "text/html",
      "Content-Security-Policy": csp,
    });
    res.end(body);
  } catch {
    res.writeHead(404);
    res.end();
  }
});
server.listen(0, "127.0.0.1");
await once(server, "listening");
const browser = await chromium.launch({
  args: [
    "--use-fake-device-for-media-stream",
    "--use-fake-ui-for-media-stream",
  ],
});
try {
  const page = await browser.newPage();
  const bad = [];
  const site = process.argv[2] || `http://127.0.0.1:${server.address().port}/`;
  page.context().on("request", (r) => {
    if (
      r.method() !== "GET" ||
      new URL(r.url()).origin !== new URL(site).origin
    )
      bad.push(r.url());
  });
  page.on("console", (m) => {
    if (m.type() === "error") console.error(m.text());
  });
  await page.goto(
    process.argv[2] || `http://127.0.0.1:${server.address().port}/`,
  );
  const worker = (await readdir("dist-static/assets")).find((x) =>
    /^speech-worker-.*\.js$/.test(x),
  );
  const result = await page.evaluate(
    async ({ worker, fixture }) => {
      const w = new Worker("/assets/" + worker, { type: "module" });
      const call = (type, audio) =>
        new Promise((resolve, reject) => {
          const timeout = setTimeout(
            () => reject(Error("Speech timed out")),
            240000,
          );
          w.onerror = (e) => {
            clearTimeout(timeout);
            reject(Error(e.message));
          };
          w.onmessage = ({ data }) => {
            if (data.type === "progress") return;
            clearTimeout(timeout);
            data.type === "error"
              ? reject(Error(data.message))
              : resolve(data.value);
          };
          w.postMessage({ id: 1, type, audio });
        });
      try {
        await call("load");
        const context = new AudioContext({ sampleRate: 16000 });
        const audio = await context.decodeAudioData(
          Uint8Array.from(atob(fixture), (c) => c.charCodeAt(0)).buffer,
        );
        const partial = await call(
          "transcribe",
          audio.getChannelData(0).slice(0, 80000),
        );
        const text = await call("transcribe", audio.getChannelData(0));
        await context.close();
        return {
          text,
          partial,
          caches: await caches.keys(),
          modelFiles: (
            await (await caches.open("tailterm-speech-v1")).keys()
          ).map((r) => r.url),
        };
      } finally {
        w.terminate();
      }
    },
    {
      worker,
      fixture: (await readFile(".build/speech-fixture.wav")).toString("base64"),
    },
  );
  assert.ok(
    result.partial.length > 0,
    "An unfinished recording produces a live draft",
  );
  assert.match(result.text, /ask not what your country can do for you/i);
  assert.ok(result.caches.includes("tailterm-speech-v1"));
  assert.ok(
    result.modelFiles.some((url) =>
      url.includes("encoder_model_quantized.onnx"),
    ),
  );
  assert.ok(
    result.modelFiles.some((url) =>
      url.includes("decoder_model_merged_quantized.onnx"),
    ),
  );
  assert.deepEqual(bad, []);
  const recorder = (await readdir("dist-static/assets")).find((x) =>
    /^speech-recorder-.*\.js$/.test(x),
  );
  assert.ok(recorder, "AudioWorklet must be a self-hosted file under CSP");
  const count = await page.evaluate(async (recorder) => {
    const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    const context = new AudioContext();
    await context.audioWorklet.addModule("/assets/" + recorder);
    const node = new AudioWorkletNode(context, "tailterm-dictation-recorder");
    const source = context.createMediaStreamSource(stream);
    let count = 0;
    node.port.onmessage = (e) => (count += e.data.samples?.length || 0);
    const mute = context.createGain();
    mute.gain.value = 0;
    source.connect(node).connect(mute).connect(context.destination);
    await new Promise((r) => setTimeout(r, 500));
    stream.getTracks().forEach((t) => t.stop());
    await context.close();
    return count;
  }, recorder);
  assert.ok(count > 0);
  console.log(
    "PASS self-hosted WASM speech, verified model cache, same-origin GET requests only, microphone worklet:",
    result.text,
  );
} finally {
  await browser.close();
  server.close();
}
