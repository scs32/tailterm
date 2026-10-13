import { execFileSync } from "node:child_process";
import { diffPaths } from "./verify-matrix.mjs";

const targets = ["hub", "bridge", "mini", "tailos"];
// A fixed reason the release runner may journal and post (its REASON pattern).
function tagged(reason) { const error = new Error(reason); error.releaseReason = reason; return error; }
// The one unknown path a reason may name; any other is counted, never quoted.
const NAMED_PATH = /^[A-Za-z0-9_.\/-]{1,120}$/;
// An unknown source file blocks release rather than silently omitting a target.
// known(path, pending) is asked only about a path no rule here matches: an array
// adds those targets, anything else leaves the path unknown. It never removes a
// target a rule here selects and is never asked about an invalid path.
export function targetsForPaths(paths, known = () => null) {
  const out = new Set(), pending = [];
  for (const p of paths) {
    if (!p || p.startsWith("/") || p.split("/").includes("..") || /[\0\r\n]/.test(p)) throw tagged("Invalid release path");
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
    else pending.push(p);
  }
  const unknown = [];
  for (const p of pending) {
    const vouched = known(p, pending);
    if (Array.isArray(vouched)) { for (const t of vouched) out.add(t); } else unknown.push(p);
  }
  if (unknown.length) {
    const named = unknown.find(p => NAMED_PATH.test(p));
    throw tagged(named ? `Unknown release path: ${named}${unknown.length > 1 ? ` and ${unknown.length - 1} more` : ""}` : `Unknown release path (${unknown.length} path${unknown.length === 1 ? "" : "s"}, names withheld)`);
  }
  return targets.filter(t => out.has(t));
}
// Run in the candidate's checkout: its own map answers one path per call, and
// a path it throws on is unknown.
const CANDIDATE_MAP = `import { pathToFileURL } from "node:url";
import { resolve } from "node:path";
let input = "";
for await (const chunk of process.stdin) input += chunk;
const { targetsForPaths } = await import(pathToFileURL(resolve("scripts/release-targets.mjs")).href);
process.stdout.write(JSON.stringify(JSON.parse(input).map(p => { try { return { targets: targetsForPaths([p]) }; } catch { return { unknown: true }; } })));`;
// What the candidate's own scripts/release-targets.mjs says about paths the
// running map does not know, so a new folder and its rule ship in one release.
// The candidate's map runs in a short child process and is never imported
// here. It is consulted only when the checkout is at the candidate commit with
// scripts/ and tests/ unmodified, so the module and its imports are the
// candidate's files. Any failure (no map in the commit, another HEAD, a failed
// or slow child, output of any other shape, a target outside the four names)
// answers null for every path asked: the path stays unknown. A child still
// running at timeoutMs is killed with SIGKILL, which it cannot trap.
export function candidateKnown(cwd, commit, { timeoutMs = 20000 } = {}) {
  const answers = new Map(), git = (...args) => execFileSync("git", args, { cwd, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
  let usable;
  const ask = paths => {
    try {
      if (usable === undefined) {
        usable = false;
        git("cat-file", "-e", `${commit}:scripts/release-targets.mjs`);
        usable = git("rev-parse", "HEAD").trim() === commit && git("status", "--porcelain", "--untracked-files=no", "--", "scripts", "tests") === "";
      }
      if (!usable) return null;
      const raw = execFileSync(process.execPath, ["--input-type=module", "-e", CANDIDATE_MAP], { cwd, env: { PATH: process.env.PATH || "" }, input: JSON.stringify(paths), encoding: "utf8", stdio: ["pipe", "pipe", "ignore"], timeout: timeoutMs, killSignal: "SIGKILL", maxBuffer: 1024 * 1024 });
      const said = JSON.parse(raw);
      const shaped = e => e && typeof e === "object" && !Array.isArray(e) && Object.keys(e).length === 1 && (e.unknown === true || (Array.isArray(e.targets) && e.targets.every(t => targets.includes(t))));
      return Array.isArray(said) && said.length === paths.length && said.every(shaped) ? said : null;
    } catch { return null; }
  };
  return (path, pending = [path]) => {
    if (!answers.has(path)) {
      const asked = [...new Set([path, ...pending])].filter(p => !answers.has(p)), said = ask(asked);
      asked.forEach((p, i) => answers.set(p, said?.[i].targets ?? null));
    }
    return answers.get(path);
  };
}
export function selectReleaseTargets(cwd, baselines, commit) {
  const known = candidateKnown(cwd, commit);
  return targets.filter(t => {
    if (!/^[a-f0-9]{40}$/.test(baselines[t] || "")) throw tagged("Missing last successful target baseline");
    return targetsForPaths(diffPaths(cwd, baselines[t], commit), known).includes(t);
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
