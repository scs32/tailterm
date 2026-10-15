// Each browser check that starts a Vite server keeps its own dependency cache
// under .build/, keyed by the test file, so checks that run at the same time
// never remove or rewrite each other's optimized dependencies. A unit test that
// spawns a browser check sets TAILTERM_VITE_CACHE_ROOT to a temporary directory
// so its run never shares the real check's cache.
import { basename, join } from "node:path";
import { fileURLToPath } from "node:url";

export const viteCacheDir = (testUrl) => {
  const name = basename(fileURLToPath(testUrl), ".mjs");
  const root = process.env.TAILTERM_VITE_CACHE_ROOT;
  if (root) return join(root, name);
  return fileURLToPath(new URL("../.build/vite-cache/" + name, import.meta.url));
};
