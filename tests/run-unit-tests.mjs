// Runs tests/*.test.js with node:test, longest files first, so the files that
// set npm-unit's wall time start at once instead of in name order
// (wi_f60103c0670388c8, order #44897). node --test sorts its file list
// whatever order it is given; run() keeps this one. A listed name that no
// longer exists is skipped; a new file runs after the list in name order.
import {run} from "node:test";
import {spec} from "node:test/reporters";
import {readdirSync} from "node:fs";
import {availableParallelism} from "node:os";
import {dirname,join} from "node:path";
import {fileURLToPath} from "node:url";

// Measured longest first; keep the slowest at the top when timings change.
const LONGEST=["verification-2.test.js","release-runner-2.test.js","release-runner-5.test.js","verification-4.test.js","release-runner-4.test.js","verification.test.js","release-runner.test.js","verification-5.test.js","release-runner-6.test.js","verification-6.test.js","release-runner-3.test.js","tmux-command.test.js","verification-3.test.js","matrix-host-lock.test.js"];
const dir=dirname(fileURLToPath(import.meta.url)),names=readdirSync(dir).filter(n=>n.endsWith(".test.js")).sort();
const files=[...LONGEST.filter(n=>names.includes(n)),...names.filter(n=>!LONGEST.includes(n))].map(n=>join(dir,n));
const flag=process.argv.indexOf("--concurrency"),concurrency=flag<0?Math.max(1,availableParallelism()-1):Number(process.argv[flag+1]);
// A stop (the matrix's SIGTERM on timeout) cancels the running files, so the
// spec reporter still names them and prints the summary, as node --test does.
const stop=new AbortController();for(const s of ["SIGTERM","SIGINT"])process.on(s,()=>{process.exitCode=1;stop.abort();});
const stream=run({files,concurrency,signal:stop.signal});
stream.on("test:fail",()=>{process.exitCode=1;});
stream.compose(spec).pipe(process.stdout);
