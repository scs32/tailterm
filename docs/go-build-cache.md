# Go build cache

Bug `wi_96b23f0f7f151a81`, work order #16321. `~/Library/Caches/go-build` on the
Mini reached 48 GB (#15923) and grew about 0.5 GB an hour (#16305).

## Policy

- Never delete, clear or trim `~/Library/Caches/go-build` or `~/go/pkg`
  (owner rule). No script, test or cleanup runs `go clean -cache`,
  `go clean -modcache` or removes files there.
- Go's own trim is the only eviction: at most once a day, `go` removes cache
  entries unused for 5 days (`trimInterval` and `trimLimit` in
  `cmd/go/internal/cache`).
- Measurements and experiments use a scratch `GOCACHE` (and `GOMODCACHE`) in a
  temporary directory.

## Cause

Without `-trimpath`, `go` adds the package directory to each compile action ID
(`fmt.Fprintf(h, "dir %s\n", p.Dir)` in `cmd/go/internal/work/exec.go`), because
the object file may embed that absolute path. Agents build the same packages in
many worktrees under `.build/worktrees`, so each worktree path gets its own copy
of the repo's package entries. Module-cache dependencies and the standard
library already share one path and are not duplicated.

g1 measurement (planner, go1.26.6 darwin/arm64, Stephens-Mini, 2026-09-30,
commit d4691f3, scratch `GOCACHE` per mode). Two `git archive` copies of `hub/`
in two paths; in each, `go build ./...` then `go test -count=1 -run '^$' ./...`,
path A, then path B, then A again. Values are the cumulative cache size.

| mode | after A | after B (second path) | growth from B | A again |
|---|---|---|---|---|
| no -trimpath | 879,440 KB / 5,048 entries | 1,017,432 KB / 5,330 | +138 MB, +282 entries | +0 |
| -trimpath | 875,516 KB / 5,048 | 875,776 KB / 5,080 | +0.26 MB, +32 entries | +0 |

At 138 MB a path, 113 worktrees hold about 15.6 GB for one snapshot, and a
worktree rebuilt at several commits within the 5-day window adds more, which
matches the observed 48 to 50 GB.

The recipe is `g1-measure.sh` next to the plan in
`tailterm-artifacts/wi_96b23f0f7f151a81/`: run `sh g1-measure.sh SCRATCH [COMMIT]`
from the repo, then remove `SCRATCH` (about 3 GB at peak). It reads `~/go/pkg`
only through a `file://` `GOPROXY` and never touches `~/Library/Caches/go-build`.

## What tt spawn sets

`spawn.Create` sets `GOFLAGS` on every new agent tmux session (`tt spawn` in
`hub/cmd/tt/main.go` and handler launches in `hub/cmd/tt/handler_spawn.go` both
go through it), so builds of the same source in
different worktrees share cache entries:

- The base is the caller's `GOFLAGS` in the session environment when present,
  even if empty; otherwise the `GOFLAGS` of the process running `tt spawn`.
- `-trimpath` is appended unless the base already names it. `-trimpath`,
  `--trimpath` and `-trimpath=true` are kept as they are.
- `-trimpath=false` (or `--trimpath=false`) in the base is an explicit opt-out
  and is kept unchanged.
- A flag on the `go` command line overrides `GOFLAGS`, so
  `go build -trimpath=false ...` opts out for one command.

`-trimpath` changes only recorded file paths: panic stack traces and debug
information show module paths such as
`github.com/scs32/tailterm/hub/internal/spawn/spawn.go` instead of absolute
paths. No Go code in the repo locates files with `runtime.Caller`.

## Not covered

- Sessions already running keep their old environment until they are respawned.
- The owner's shells, owner-helper sessions and anything not launched through
  `spawn.Create`.
- A shell profile or a `go env -w` file (`go env GOENV`) that sets `GOFLAGS`
  inside the session replaces the session value. None exists on the Mini today.

A host-wide `go env -w GOFLAGS=-trimpath` would cover all of these; that is an
owner decision and not part of this change.

## Unaffected

- Matrix runs: `scripts/verify-matrix.mjs` passes a closed environment without
  `GOFLAGS` and gives each run its own `GOCACHE` inside the verifier home, which
  is removed after the run. They never used the shared cache.
- Release builds: `scripts/release-runner.mjs` builds from a stamped clone and,
  like the `build:hub` and `build:tt` package scripts, already passes
  `-trimpath` explicitly. A linux/amd64
  `go build -trimpath -ldflags='-s -w' ./cmd/tt` gives the same sha256 with
  `GOFLAGS` empty and with `GOFLAGS=-trimpath`.

## Expected steady-state size

- About 0.9 GB for each toolchain, target and flag set: the standard library,
  dependencies and one copy of the repo's packages (g1 "after A" row).
- Plus the repo package entries built in the last 5 days: at most about 140 MB
  for each fully distinct source snapshot. Unchanged packages share entries
  across commits and, with `-trimpath`, across worktree paths.
- Cross builds (linux/amd64, wasm) and other toolchain versions each add a
  similar base.

Expect under 10 GB about 5 to 6 days after all running agent sessions have been
respawned. Investigate above 15 GB: look for sessions started before this
change or builds outside `tt spawn`.

## How to check

Read only; never clear the cache to check it:

```sh
du -sh ~/Library/Caches/go-build
tmux show-environment -t =SESSION GOFLAGS   # an agent session: GOFLAGS=... -trimpath
```

To confirm the cause again, rerun the g1 recipe with a scratch `GOCACHE`.
