import { createReadStream } from "node:fs";
import { stat } from "node:fs/promises";
import { createServer } from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";

const securityHeaders = {
  "Content-Security-Policy":
    "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; connect-src 'self' https: wss:; font-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'",
  "Permissions-Policy":
    "camera=(), microphone=(self), geolocation=(), clipboard-read=(self), clipboard-write=(self)",
  "Referrer-Policy": "no-referrer",
  "X-Content-Type-Options": "nosniff",
};

const contentTypes = new Map([
  [".br", "application/octet-stream"],
  [".css", "text/css; charset=utf-8"],
  [".gz", "application/gzip"],
  [".html", "text/html; charset=utf-8"],
  [".js", "text/javascript; charset=utf-8"],
  [".json", "application/json; charset=utf-8"],
  [".svg", "image/svg+xml"],
  [".wasm", "application/wasm"],
  [".woff", "font/woff"],
  [".woff2", "font/woff2"],
]);

function reply(res, status, body) {
  res.writeHead(status, {
    ...securityHeaders,
    "Cache-Control": "no-cache",
    "Content-Type": "text/plain; charset=utf-8",
    "Content-Length": Buffer.byteLength(body),
  });
  res.end(body);
}

export function createStaticPreviewServer(rootDirectory) {
  const root = path.resolve(rootDirectory);
  const rootPrefix = `${root}${path.sep}`;
  return createServer(async (req, res) => {
    if (req.method !== "GET" && req.method !== "HEAD") {
      res.setHeader("Allow", "GET, HEAD");
      reply(res, 405, "Method not allowed\n");
      return;
    }

    let pathname;
    try {
      pathname = decodeURIComponent(
        new URL(req.url, "http://localhost").pathname,
      );
    } catch {
      reply(res, 400, "Bad request\n");
      return;
    }
    if (pathname.includes("\0")) {
      reply(res, 400, "Bad request\n");
      return;
    }

    const relative = pathname === "/" ? "index.html" : pathname.slice(1);
    let filename = path.resolve(root, relative);
    if (filename !== root && !filename.startsWith(rootPrefix)) {
      reply(res, 403, "Forbidden\n");
      return;
    }

    let metadata;
    try {
      metadata = await stat(filename);
      if (metadata.isDirectory()) {
        filename = path.join(filename, "index.html");
        metadata = await stat(filename);
      }
    } catch {
      if (path.extname(relative)) {
        reply(res, 404, "Not found\n");
        return;
      }
      filename = path.join(root, "index.html");
      try {
        metadata = await stat(filename);
      } catch {
        reply(res, 404, "Not found\n");
        return;
      }
    }

    const extension = path.extname(filename).toLowerCase();
    res.writeHead(200, {
      ...securityHeaders,
      "Cache-Control": "no-cache",
      "Content-Length": metadata.size,
      "Content-Type": contentTypes.get(extension) || "application/octet-stream",
    });
    if (req.method === "HEAD") {
      res.end();
      return;
    }
    createReadStream(filename).pipe(res);
  });
}

if (path.resolve(process.argv[1] || "") === fileURLToPath(import.meta.url)) {
  const root = process.argv[2] || "dist-static";
  const host = process.argv[3] || "127.0.0.1";
  const port = Number(process.argv[4] || 4318);
  if (!Number.isInteger(port) || port < 1 || port > 65535)
    throw new Error("Preview port must be an integer from 1 to 65535");
  const server = createStaticPreviewServer(root);
  server.listen(port, host, () => {
    console.log(`Tailterm static preview: http://${host}:${port}`);
  });
  for (const signal of ["SIGINT", "SIGTERM"])
    process.on(signal, () => server.close(() => process.exit(0)));
}
