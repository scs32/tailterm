# Verification evidence capture for Claude sessions

When a Claude Code session runs a recognised verification command through
Bash, the tool-call ledger records, on that call's own row, the exact command,
its exit code, a SHA-256 of its output and the git commit it ran at (feature
`wi_95d33b4e3248bac5`, work order #51365).

**This is local evidence only. AIV bindings and integration remain deferred.**
Nothing is sent to the hub, the Board or AIV. The fields sit in the agent's
private ledger file on its own host, next to the rows the ledger already
writes.

## Mechanism

It is not a mod folder and there is nothing to load. It extends `tt hook tool`,
the ledger hook that host setup already installs for `PreToolUse`,
`PostToolUse` and `PostToolUseFailure` (see the tool-call ledger section of
`docs/claude-wake.md`). The code is in `hub/cmd/tt/tool_ledger.go`.

1. On `PreToolUse` of a Bash call, the hook classifies the command (below). For
   a verification command it reads the commit `HEAD` names in the repository
   holding the payload's `cwd`, and keeps it in the call's pending file
   beside the start time it already keeps.
2. On the call's `PostToolUse` or `PostToolUseFailure`, the hook adds one
   `evidence` object to the row it already writes. No new row kind is added and
   `v` stays 1. Every other call's row is exactly what it was.

The hook contract is unchanged: it prints nothing, never writes to stderr,
always exits 0 and returns within its 150 ms deadline, so it cannot deny or
delay a call. It starts no process: the commit is read from files, not from
`git`.

## Which commands count (D1)

Only a Bash call whose command has at least one of these simple commands, after
assignments and wrappers (`env`, `time`, `timeout`, `nice`, `sudo`, `nohup`,
`exec`, `command`, `xargs`) are dropped. The program is matched by its base
name, so `/usr/local/go/bin/go` counts as `go`.

| kind | form |
| --- | --- |
| `go-test` | `go test ...`, also after `-C DIR` or `-C=DIR` |
| `go-vet` | `go vet ...`, the same way |
| `npm-test` | `npm test`, `npm t`, `npm run test`, `npm run test:*`, `npm run-script test`, with any package-manager options first; the same words for `pnpm`, `yarn` and `bun` |
| `node-test` | `node ... --test ...`, the `--test` option before the script, or with no script |
| `verify-matrix` | `node .../verify-matrix.mjs ...`, or `.../verify-matrix.mjs ...` run directly |

The host-lock wrapper is looked through in its real form,
`node scripts/verify-matrix-host-lock.mjs exec [options] -- COMMAND [ARGS...]`:
the words after its first `--` are classified the same way, up to two wrappers
deep. Any other subcommand of that script (`status`, ...) is not a verification
command, and neither is an `exec` with no `--`. A `verify-matrix.mjs` match is
exact, so the wrapper script itself never counts as `verify-matrix`.

The command is split by the same parser the would-deny report uses, so text in
quotes, arguments and heredoc bodies are never commands: `echo "go test"`,
`pkill -f "go test"` and `git commit -m 'go test'` do not count. A command
substitution does run, so `out=$(go test ./x)` counts. Commands joined with
`&&`, `;` or a pipe are all read: `go vet ./... && go test ./...` gives both
kinds. `kinds` lists each kind once, in the order first seen.

What does not count:

- text handed to a nested shell, `bash -c '...'`, `sh -c '...'` or `eval`;
- a command over 64 KiB, or one nested too deep for the parser;
- any tool other than Bash;
- **Codex sessions.** Only Claude Code runs this hook. Most team verifiers run on
  Codex today, so most matrix runs leave no evidence row.

## The `evidence` field

| field | meaning |
| --- | --- |
| `kinds` | the verification kinds the command runs, always present |
| `command` | the whole command as written, each secret span replaced by `[redacted]`; never cut, so text present is the whole command |
| `commandRedacted` | how many spans were replaced; absent when none |
| `commandSkip` | only when there is no `command`: `oversize` (over 16 KiB) or `timeout` (the secret scan did not finish within its budget) |
| `exitCode` | 0 to 255; absent when unknown |
| `exitSource` | `success` (a `PostToolUse`) or `error-text` (read from the failure's text) |
| `outputDigest` | 64 hex characters, defined below |
| `outputSource` | `stdout+stderr` or `error` |
| `outputSkip` | only when there is no digest: `background`, `unreadable` or `timeout` |
| `commit` | 40 or 64 lower-case hex characters; absent outside a repository |
| `commitSource` | `pre` (read when the call started) or `post` (read at the post) |

The digests:

- `PostToolUse`: SHA-256 of `tool_response.stdout`, one NUL byte, then
  `tool_response.stderr` (a missing or null stderr is empty). A stdout that is
  not a string gives `outputSkip: "unreadable"`.
- `PostToolUseFailure`: SHA-256 of the `error` string. An error that is not a
  string gives `outputSkip: "unreadable"`.
- A background start (`run_in_background: true`) gives `outputSkip:
  "background"` and no exit code: what returned is the start, not the
  command's end.
- When the output cannot be decoded within 120 ms of the hook's start,
  `outputSkip: "timeout"`. A `PostToolUse` then still has `exitCode` 0.

The digest is of what Claude Code handed the hook, before any secret
redaction rewrites the output the model sees.

## Where the commit comes from (D3)

The hook walks up from the payload's `cwd` to the first directory holding
`.git`, a directory or a `gitdir:` file (a linked worktree or a submodule),
and reads `HEAD`. A symbolic `HEAD` is followed through the loose ref in the
worktree's git directory, then the common one named by `commondir`, then
`packed-refs`. Only a full lower-case hex object id is stored. An unborn
branch, a ref outside `refs/`, any other `HEAD` text and a repository whose
refs are stored as a reftable give no commit.

The commit is read when the call starts and kept in its pending file, so a
command that commits or checks out still records the commit it started on
(`commitSource: "pre"`). When there is no pending entry, because the pre was
cut off, the pending bound was reached or the entry would pass its 512 bytes
with the commit in it, the post reads the commit itself (`commitSource:
"post"`).

**Whether the tree was dirty is not recorded.** That needs a `git` process, and
the hook starts none. A commit says which tree the command started from, not
that the tree matched it.

## Exit codes (D4)

Claude Code gives the hook no numeric exit code. A Bash command that exits 0
arrives as `PostToolUse`, recorded as `exitCode: 0`, `exitSource: "success"`.
One that exits non-zero arrives as `PostToolUseFailure`.

The forms used come from recorded Claude Code transcripts on the Mini, as the
design survey for this feature found them: the failed Bash text began
`Exit code N` in the tool result's content (185 rows, values 1, 2, 28 and 128)
or `Error: Exit code N` in the transcript's `toolUseResult`. **The hook payload's own `error` text was not observed**;
no live session was run for this feature. So a failure whose `error` begins
`Exit code N` or `Error: Exit code N`, with N from 0 to 255 followed by the end
of the text or a space, records `exitCode: N`, `exitSource: "error-text"`.

Any other failure text, an interrupted call (`is_interrupt`) and a background
start record no `exitCode` and no `exitSource`. A missing exit code is the safe
failure: a wrong one is never recorded. If real failure rows turn out to have
no `exitCode`, that is the first thing to check (`jq 'select(.evidence and
.outcome=="error")'`).

## What is stored and what is never stored

Stored: the fields above. The one exception to the ledger's rule that tool
input is never stored is `command`: the words of the command, literal paths
included, are stored as written. Every span the existing secret scan finds is
replaced by `[redacted]` first: values this host holds (group A, the hub token
and the listed environment variables), prefixed tokens and private keys
(group B), and assigned secrets such as `password=...` (group C, which the
output redaction only counts). The scan is the one in
`hub/cmd/tt/tool_redact.go`; this feature holds no pattern of its own.
`argsDigest` already pins the exact original input.

Never stored, in a row or a pending file: the output (`stdout`, `stderr`), the
`error` text, the payload's `cwd` and `transcript_path`, the repository root,
the git directory and every other path the hook finds from them.

`docs/claude-wake.md` still says `tool_input` is never stored. For verification
calls that is no longer true of the command text; reconciling that page is
left to a separate item, since this one does not own it.

## Reading it

```
jq -c 'select(.evidence)' ~/.local/state/tailterm/tool-ledger/<agent>/ledger.jsonl
```

Each line is one verification call: `toolUseId`, `time`, `outcome`, and its
`evidence`. To check a claim such as "go test passed at abc123", find the row
whose `evidence.command` is the command quoted, whose `evidence.exitCode` is 0
and whose `evidence.commit` is the commit named. The digest lets two people
compare an output they both hold without the ledger keeping it.

## When it is live (D5)

It is always on, with no setting, like the ledger itself. It becomes live on a
host when a release ships this `tt` there. From then on every Claude agent
session, and a Claude owner-helper session, records evidence with no install
step. Until then nothing changes on any host.

## What has been checked

From `hub/`, `go test ./cmd/tt -run 'Ledger' -count=1` runs the existing
ledger tests and these new ones, all with an isolated ledger directory, temp
git repositories and no live session:

- `TestToolLedgerEvidenceClassify`: the D1 table, wrappers, nested shells and
  the 64 KiB cap.
- `TestToolLedgerEvidenceCommit`: a plain repository and a subdirectory, a
  linked worktree, packed refs, a detached `HEAD` and a SHA-256 repository
  against `git rev-parse HEAD`; no repository, an unborn branch, a broken
  `HEAD` and a ref outside `refs/` give none.
- `TestToolLedgerEvidenceRow`: the whole field for a passing command and for
  `Exit code 3` and `Error: Exit code 128` failures.
- `TestToolLedgerEvidenceOnlyVerification`: other Bash calls and other tools
  keep exactly their old rows.
- `TestToolLedgerEvidenceStoresNoOutput`, `TestToolLedgerEvidenceRedactsCommand`
  and `TestToolLedgerEvidenceRedactTimeout`: nothing of the output, error, cwd,
  transcript or repository path is stored; group A, B and C spans are
  replaced; oversize and timeout give no text.
- `TestToolLedgerEvidenceCommitAtStart`: a command that moves `HEAD` records the
  commit it started at, with `git` off the hook's `PATH`.
- `TestToolLedgerEvidenceExitCode`: the exit-code forms, interrupted,
  background and unreadable failures.
- `TestToolLedgerEvidenceWithinBound`: held stdin and 4 MiB of output return
  inside the 200 ms bound, silently; the pending entry stays within 512 bytes.

Not checked: a live Claude Code session's hook payload (see exit codes above),
and Codex sessions, which this does not cover.
