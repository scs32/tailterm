import test from "node:test";
import assert from "node:assert/strict";
import { gzipSync } from "node:zlib";
import { mkdtemp, rm, mkdir, writeFile } from "node:fs/promises";
import { request } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import { createStaticPreviewServer } from "../scripts/preview-static.mjs";

function rawRequest(url, method = "GET") {
  return new Promise((resolve, reject) => {
    const req = request(
      url,
      { method, headers: { "Accept-Encoding": "identity" } },
      (res) => {
        const chunks = [];
        res.on("data", (chunk) => chunks.push(chunk));
        res.on("end", () =>
          resolve({
            status: res.statusCode,
            headers: res.headers,
            body: Buffer.concat(chunks),
          }),
        );
      },
    );
    req.on("error", reject);
    req.end();
  });
}

test("static preview preserves application-managed gzip bytes and security headers", async () => {
  const root = await mkdtemp(path.join(tmpdir(), "tailterm-preview-"));
  const compressed = gzipSync(Buffer.from("production wasm fixture"));
  await mkdir(path.join(root, "assets"));
  await writeFile(
    path.join(root, "index.html"),
    "<!doctype html><title>Tailterm</title>",
  );
  await writeFile(path.join(root, "assets", "tailserve.wasm.gz"), compressed);
  const server = createStaticPreviewServer(root);
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  try {
    const origin = `http://127.0.0.1:${server.address().port}`;
    const wasm = await rawRequest(`${origin}/assets/tailserve.wasm.gz`);
    assert.equal(wasm.status, 200);
    assert.equal(wasm.headers["content-type"], "application/gzip");
    assert.equal(wasm.headers["content-encoding"], undefined);
    assert.deepEqual(wasm.body, compressed);

    const page = await rawRequest(`${origin}/nested/route`);
    assert.equal(page.status, 200);
    assert.match(page.headers["content-security-policy"], /wasm-unsafe-eval/);
    assert.match(page.body.toString(), /Tailterm/);

    const head = await rawRequest(`${origin}/assets/tailserve.wasm.gz`, "HEAD");
    assert.equal(head.status, 200);
    assert.equal(head.body.length, 0);
    assert.equal(Number(head.headers["content-length"]), compressed.length);
  } finally {
    await new Promise((resolve) => server.close(resolve));
    await rm(root, { recursive: true, force: true });
  }
});
