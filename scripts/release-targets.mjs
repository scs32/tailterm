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
    else if (/^hub\/internal\/triage\//.test(p)) { out.add("hub"); out.add("mini"); } // imported by tailterm-hub and tt
    else if (/^hub\/internal\/discord\//.test(p)) out.add("bridge"); // only tailterm-discord imports it
    else if (/^hub\/internal\/teamplan\//.test(p)) out.add("mini"); // only tt imports teamplan; plan.mjs is go:embed'ed into tt
    else if (/^(client\/|wasm\/|public\/|package(-lock)?\.json$|index\.html$|vite.*\.js$)/.test(p)) out.add("tailos");
    else if (/^scripts\/(deploy-truenas-hub\.py|truenas_release_preflight\.py)$/.test(p)) {out.add("hub");out.add("bridge");}
    else if (/^scripts\/(release-|verify-|install-relay|build-tt)/.test(p)) {for(const t of targets)out.add(t);}
    else if (/^scripts\/(package-static|build-wasm|download-|prepare-|preview-static)/.test(p)) out.add("tailos");
    else if (/^shared\//.test(p)) out.add("tailos"); // bundled into the static client through client/ imports
    else if (/^scripts\/package-speech-model\.mjs$/.test(p)) out.add("tailos"); // imported by package-static during build:static
    else if (/^deploy\/(_headers|LICENSE\.(onnxruntime|whisper)\.txt)$/.test(p)) out.add("tailos"); // copied into dist-static by package-static
    else if (/^(LICENSE|README\.md|\.gitignore|\.npmrc|\.prettierrc.*)$/.test(p)) continue;
    else if (/^hub\/internal\/testverification\//.test(p)) continue; // test fixture; imported only by _test.go files
    else if (/^scripts\/build-team-plan\.mjs$/.test(p)) continue; // generator; its committed output plan.mjs maps to mini and npm test --check keeps them in sync
    else if (/^(server\/|scripts\/export-server-vault\.mjs$)/.test(p)) continue; // old Node server for tailterm.tailarr.com and its vault export, not a release target
    else if (/^scripts\/(deploy-static\.mjs|deploy-remote-static\.py|deploy-apple-web\.py|cloudflare-domain\.mjs|container-dev\.sh)$/.test(p)) continue; // manual, old-site and local container tooling; release deploys tailos with wrangler
    else if (/^((hub\/)?(Dockerfile|\.dockerignore)|Dockerfile\.static|deploy\/(Caddyfile(\.container)?|compose\.yaml))$/.test(p)) continue; // container images and example deployments; hub/bridge ship binaries via deploy-truenas-hub.py
    else if (/^tools\/jev-kit\//.test(p)) continue; // separate evaluation kit module; nothing ships from it
    else if (/^tools\/claude-mods\//.test(p)) continue; // Claude Code mods loaded only by --plugin-dir; nothing ships from them
    else if (/^(\.github\/workflows\/[^/]+\.ya?ml|hub\/README\.md)$/.test(p)) continue; // CI configuration and documentation
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
// with every released receipt and every superseded job that cites a recorded
// hand release (its released commit, for the targets that record shipped), so
// the deployer and the handler's input builder select the same targets for a
// job. Events apply in settledAt order, later winning; legacy events without
// settledAt apply first, in list order, and a legacy superseded job without
// targets is ignored. settledAt is RFC3339Nano with trimmed zeros, which does
// not sort as text, so it is compared as time.
export function releaseBaselines(baselines, jobs) {
  const out = { ...baselines };
  const events = [];
  jobs.forEach((job, index) => {
    const at = job.settledAt ? Date.parse(job.settledAt) : null;
    if (job.settledAt && Number.isNaN(at)) throw new Error("Invalid release settledAt");
    if (job.receipt?.outcome === "released") {
      const shipped = job.receipt.targets.filter(t => t.outcome === "released").map(t => t.target);
      events.push({at, index, commit: job.receipt.commit, targets: shipped});
    } else if (job.state === "superseded" && job.supersession?.targets?.length) {
      events.push({at, index, commit: job.supersession.releasedCommit, targets: job.supersession.targets});
    }
  });
  events.sort((a, b) => {
    if ((a.at === null) !== (b.at === null)) return a.at === null ? -1 : 1;
    return (a.at ?? 0) - (b.at ?? 0) || a.index - b.index;
  });
  for (const e of events) for (const t of e.targets) out[t] = e.commit;
  return out;
}
// Conservative: any non-test store source change since the live hub needs a
// backup-copy migration rehearsal.
export function schemaChanged(cwd, hubBaseline, commit) {
  return diffPaths(cwd, hubBaseline, commit).some(p => /^hub\/internal\/store\/.*\.go$/.test(p) && !p.endsWith("_test.go"));
}
