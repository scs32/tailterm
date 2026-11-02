// Start check for the standalone browser suites that serve dist-static
// (wi_6e2dbfea3fbe54f3 / order #55913). The verification matrix builds
// dist-static from the candidate in its 00-static-build check before any
// browser suite runs. A suite run outside the matrix (a measurement, a
// host-lock exec, a direct node tests/<suite>) used to serve a missing build
// as 404 pages and time out; now it exits 1 at once with one line saying to
// run npm run build:static in this checkout.
//
// Staleness is a warning only, and the suite continues. dist-static/release.json
// records the commit the build came from; when it differs from this checkout's
// HEAD, or names none, one warning line is printed. Limits: commit equality is
// a proxy, so uncommitted edits after the build (or a build from a dirty tree
// at the same HEAD) go unnoticed, and a newer commit that changed no client
// file still warns. Without git the staleness check is skipped. The byte-exact
// check is npm run verify:release, which this helper does not run.
import { existsSync, readFileSync, writeSync } from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";

// Returns the checkout's HEAD commit, or null when git cannot say.
export function gitHead(cwd) {
  try {
    const out = execFileSync("git", ["rev-parse", "HEAD"], {
      cwd,
      stdio: ["ignore", "pipe", "ignore"],
    });
    return String(out).trim() || null;
  } catch {
    return null;
  }
}

// Pure: no exit and no printing. The suites resolve dist-static against the
// working directory, so this does too.
export function checkDistStatic({ cwd = process.cwd(), head = gitHead } = {}) {
  const root = path.resolve(cwd);
  const dist = path.join(root, "dist-static");
  if (!existsSync(path.join(dist, "index.html")))
    return {
      missing: `dist-static is missing: no dist-static/index.html in ${root}. Run npm run build:static in this checkout first (the verification matrix builds it itself).`,
      warning: null,
    };
  let commit;
  try {
    commit = JSON.parse(
      readFileSync(path.join(dist, "release.json"), "utf8"),
    ).commit;
  } catch {}
  if (typeof commit !== "string" || !commit || commit === "unknown")
    return {
      missing: null,
      warning:
        "warning: dist-static has no readable source commit in release.json; run npm run build:static if it may be stale.",
    };
  const current = head(root);
  if (!current || commit === current) return { missing: null, warning: null };
  return {
    missing: null,
    warning: `warning: dist-static was built from ${commit.slice(0, 12)} but this checkout is at ${current.slice(0, 12)}; run npm run build:static to test the current source.`,
  };
}

// The suites call this first. writeSync, not console.error: an asynchronous
// stderr pipe can lose the line when process.exit follows.
export function requireDistStatic(options) {
  const { missing, warning } = checkDistStatic(options);
  if (missing) {
    writeSync(2, missing + "\n");
    process.exit(1);
  }
  if (warning) writeSync(2, warning + "\n");
}
