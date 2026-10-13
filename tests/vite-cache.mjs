// Each browser check that starts a Vite server keeps its own dependency cache
// under .build/, keyed by the test file, so checks that run at the same time
// never remove or rewrite each other's optimized dependencies.
import { basename } from "node:path";
import { fileURLToPath } from "node:url";

export const viteCacheDir = (testUrl) =>
  fileURLToPath(
    new URL(
      "../.build/vite-cache/" + basename(fileURLToPath(testUrl), ".mjs"),
      import.meta.url,
    ),
  );
