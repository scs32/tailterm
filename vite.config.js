import { defineConfig } from "vite";
import { fileURLToPath } from "node:url";
export default defineConfig(({ mode }) => ({
  resolve: {
    alias:
      mode === "static"
        ? [
            {
              find: /^@tailscale\/connect\/main\.wasm(\?url)?$/,
              replacement:
                fileURLToPath(
                  new URL("./wasm/tailserve.wasm", import.meta.url),
                ) + "$1",
            },
          ]
        : [
            {
              find: /^\.\/wasm-runtime\.js$/,
              replacement: fileURLToPath(
                new URL("./client/static-disabled.js", import.meta.url),
              ),
            },
          ],
  },
  define: {
    "import.meta.env.VITE_STATIC": JSON.stringify(
      mode === "static" ? "true" : "false",
    ),
  },
  worker: { format: "es" },
  build: {
    target: "esnext",
    outDir: mode === "static" ? "dist-static" : "dist",
  },
  server: {
    fs: {
      strict: true,
      deny: [
        ".env",
        ".env.*",
        "*.{crt,pem}",
        "**/.git/**",
        "**/data/**",
        "**/*.enc",
        "**/*.enc.tmp",
      ],
    },
  },
  optimizeDeps: { exclude: ["@tailscale/connect"] },
}));
