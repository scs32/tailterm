import { diffPaths } from "./verify-matrix.mjs";

const targets = ["hub", "bridge", "mini", "tailos"];
// An unknown source file blocks release rather than silently omitting a target.
export function targetsForPaths(paths) {
  const out = new Set();
  for (const p of paths) {
    if (!p || p.startsWith("/") || p.split("/").includes("..") || /[\0\r\n]/.test(p)) throw new Error("Invalid release path");
    if (/^(docs\/|tests\/|verification\/)|(^|\/)(AGENTS\.md|.*_test\.go)$/.test(p)) continue;
    if (/^hub\/(go\.(mod|sum)|internal\/(api|store|server)\/)/.test(p)) { for (const t of ["hub", "bridge", "mini"]) out.add(t); }
    else if (/^hub\/(cmd\/tailterm-hub\/|internal\/(broker|monitor|jev)\/)/.test(p)) out.add("hub");
    else if (/^hub\/(cmd\/tailterm-discord\/|internal\/bridge\/)/.test(p)) out.add("bridge");
    else if (/^hub\/(cmd\/tt\/|internal\/(spawn|adapters)\/)/.test(p)) out.add("mini");
    else if (/^(client\/|wasm\/|public\/|package(-lock)?\.json$|index\.html$|vite.*\.js$)/.test(p)) out.add("tailos");
    else if (/^scripts\/(deploy-truenas-hub\.py|truenas_release_preflight\.py)$/.test(p)) {out.add("hub");out.add("bridge");}
    else if (/^scripts\/(release-|verify-|install-relay|build-tt)/.test(p)) {for(const t of targets)out.add(t);}
    else if (/^scripts\/(package-static|build-wasm|download-|prepare-|preview-static)/.test(p)) out.add("tailos");
    else if (/^(LICENSE|README\.md|\.gitignore|\.npmrc|\.prettierrc.*)$/.test(p)) continue;
    else throw new Error("Unknown release path");
  }
  return targets.filter(t => out.has(t));
}
export function selectReleaseTargets(cwd, baselines, commit) {
  return targets.filter(t => {
    if (!/^[a-f0-9]{40}$/.test(baselines[t] || "")) throw new Error("Missing last successful target baseline");
    return targetsForPaths(diffPaths(cwd, baselines[t], commit)).includes(t);
  });
}
// The last successful commit per target: the configured baselines overlaid
// with every released receipt, so the deployer and the handler's input
// builder select the same targets for a job.
export function releaseBaselines(baselines, jobs) {
  const out = { ...baselines };
  for (const job of jobs) {
    if (job.receipt?.outcome !== "released") continue;
    for (const t of job.receipt.targets) if (t.outcome === "released") out[t.target] = job.receipt.commit;
  }
  return out;
}
// Conservative: any non-test store source change since the live hub needs a
// backup-copy migration rehearsal.
export function schemaChanged(cwd, hubBaseline, commit) {
  return diffPaths(cwd, hubBaseline, commit).some(p => /^hub\/internal\/store\/.*\.go$/.test(p) && !p.endsWith("_test.go"));
}
