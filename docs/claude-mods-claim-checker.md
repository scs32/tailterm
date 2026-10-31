# Claim checker for Claude sessions

Before `tt send` posts a RESULT or a lead's accept disposition, it compares the
commits, verification commands and receipt files the message cites against the
sender's own tool-call ledger, and prints what matched, what the ledger
contradicts and what it does not hold (feature `wi_086dd63cab0abde0`, work
order #52735).

**It is a warning only.** The message is posted exactly as it would have been,
whatever the report says. Refusing to post on an unmatched claim would turn a
report into a gate; that needs an owner decision and is not part of this
feature.

**It is local only. AIV bindings and integration remain deferred.** The check
reads one file set on the sender's own host. Nothing new reaches the hub, the
Board or AIV.

## Mechanism

It is not a mod folder and there is nothing to load. The check is part of
`tt send` (`hub/cmd/tt/claim_check.go`, called from `hub/cmd/tt/send.go`), the
one command every typed RESULT and accept disposition passes through. No new
subcommand was added.

The ledger it reads is the one `tt hook tool` writes for Claude Code sessions,
with the verification evidence the evidence capture page describes
([Verification evidence capture](claude-mods-evidence-capture.md)): on the row
of each recognised verification command, the command text, its exit code, an
output digest and the commit it ran at.

1. After the message is validated, and before any hub request, `tt send` asks
   whether the message is checked: every `result`, and a `notice` whose review
   metadata has `mode: disposition` and `disposition: accept`. Every other kind,
   and the `owner-accept`, `owner-decision` and `follow-ups` dispositions, is
   not checked.
2. It reads the sender's own ledger only: `ledger.jsonl.1`, then `ledger.jsonl`,
   in the agent's directory under the ledger root (by default
   `~/.local/state/tailterm/tool-ledger/<agent>/`, or
   `TAILTERM_TOOL_LEDGER_DIR`). Each file is read up to 16 MiB. Only lines
   holding an `evidence` object are decoded; malformed lines and rows of
   another agent are skipped. Nothing is written: no row, no pending file, no
   cache, and the files keep their size and modification time.
3. It extracts the claims from the message's structured fields, matches each
   (below) and prints the report.

The whole check has a 500 ms budget. Past it, the send prints
`claim check skipped: over 500 ms` and posts.

The check reads the envelope and never changes it. The retry identity
`tt send` derives from the envelope is therefore the same with or without a
ledger, and the hub receives the same request body.

### Who is not checked

A sender with no ledger sees nothing and sends exactly what it sends today:

- a Codex worker, or any runtime that does not run `tt hook tool`;
- a session with no agent identity in its environment, or an agent id the
  ledger refuses;
- an agent with neither ledger file on this host.

## Which claims are read

Only structured fields. Free text (`--text`, `--summary`, the subject) is never
parsed.

| claim | read from |
| --- | --- |
| commit | any `refs` value that is one hex token of 7 to 64 characters and not digits alone (a run of digits is a message or receipt number far more often than a commit); evidence entries of type `commit`; `body.candidate` and `review.candidate` when hex. Duplicates are checked once, case-insensitively. |
| command | evidence entries of type `command`, the `"eN: CMD -> OUTCOME"` form, whose command is one isolated verification command (below). |
| receipt | a `refs` value whose key contains `receipt`, or an attachment named like a receipt, that is an absolute path to an existing file named `receipt.json` or `targeted-receipt.json`. |
| count | not checked (see "What is not checked"). |

## Matching rules

### Isolation

A ledger's exit code is the exit code of the whole Bash call. So a command is
credited only when the call ran that one command and nothing else. Before any
parsing, both the cited command and the ledger row's command must pass a
quote-aware scan, read left to right with single and double quotes tracked:

- outside quotes, none of `;` `&` `|` `(` `)` `<` `>` `` ` `` `$` `#` `{` `}`, a
  newline, a carriage return or a backslash;
- inside double quotes, no `$`, `` ` `` or backslash;
- every quote closed.

Then the would-deny parser must read the text as exactly one simple command.
Quoted metacharacters stay allowed, so
`go test ./cmd/tt -run 'ClaimCheck|Send|Ledger' -count=1` is checkable. A cited
command that fails either gate is not checked; a ledger row that fails it never
credits any claim. None of these rows credits a cited `go test ./...`:
`cd hub && go test ./...`, `false && go test ./...; true`,
`go test ./... | tail -3`, `out=$(go test ./...)`, `go test ./... &`,
`go test ./... # x`, `go test ./... > out.txt`, `go test "./...$X"`.

### Commit

Matched when some evidence row's commit starts with the cited value. Otherwise
`unmatched`; when some of your runs recorded no commit, the line says how many,
because a command run after `cd` (or outside a repository) records none by
design.

### Command

A row is a candidate only when it passes isolation and its word list is
identical, word for word, to the cited command's. `go test ./...` never matches
`go test ./... -run TestOne`, nor the reverse. No candidate: `unmatched`.
Otherwise the latest candidate, by time, decides:

- the claimed outcome is a pass (`ok`, `pass`, `passed`, `green`, `clean` or
  `exit 0`, with none of `fail`, `error`, `panic`, `exit 1`-`9`) and the run
  exited non-zero: `contradicted`;
- the claimed outcome is a failure (a failure word and no pass word) and the
  run exited 0: `contradicted`;
- the message cites exactly one commit and the run recorded a different one:
  `contradicted`;
- the run has no exit code recorded (a background start, an unparsed failure):
  `unproven`;
- the outcome reads neither way and the run exited non-zero: `unproven`;
- otherwise `matched`.

Before the outcome is read, TAP's `not ok` counts as a failure word, and a zero
count of failures or errors (`0 failed`, `0 fail`, `0 failures`, `0 errors`,
`no errors`, `no failures`, `no failed`, any case) counts as a pass word. So
`62 passed, 0 failed` and `no errors` are passes. An outcome that still names
both, such as `1 passed, 3 failed`, or neither, such as `62/62`, is judged
neither way.

### Receipt

The cited file is read up to 1 MiB, the hub's limit for a verification body,
and decoded for its commit, its environment and each check's exit code and
known-failure mark. It is eligible as `scripts/verify-matrix.mjs` defines it:
no `VERIFICATION_HOME_CLEANUP_ERROR` and every check exited 0 or is a known
failure.

A row is one of the receipt's runs only when it passes isolation and reads as
`node [options] .../verify-matrix.mjs MODE INPUT OUTPUT [flags]`, with
assignments and wrapper commands dropped and up to two
`verify-matrix-host-lock.mjs exec ... --` wrappers followed, where:

- MODE is `run` for a `receipt.json` or `targeted` for a
  `targeted-receipt.json`; `plan` and every other mode write no receipt;
- OUTPUT is absolute (the ledger keeps no cwd, so a relative one cannot be
  resolved) and, cleaned, is exactly the receipt's directory. Whole-path
  equality: `/tmp/out/run` never matches `/tmp/out/run-other/receipt.json`.

No such run: `unmatched` (expected for a lead whose verifier ran the matrix).
Otherwise the latest decides: exit 0 with an ineligible receipt, or a non-zero
exit with an eligible one, is `contradicted`; a run commit different from the
receipt's is `contradicted`; a single cited commit the receipt does not name is
`contradicted`; no exit code recorded is `unproven`; otherwise `matched`.

## The report

States: `matched`, `contradicted`, `unmatched`, `unproven` (a run exists but
its exit was not recorded) and `not checked`.

- Nothing checkable in the message: a send prints nothing.
- Every checkable claim `matched`: one line,
  `claim check: 3 cited claims match your tool ledger`.
- Anything else: the full list on stderr, worst first, with the `unproven`
  lines always shown:

```
claim check (your local tool ledger; warning only, the message is sent unchanged):
  contradicted  e1 go test ./cmd/tt -run Send -> ok: your latest run exited 1 at 2026-10-31T11:42:03Z
  unproven      e2 go vet ./cmd/tt -> clean: exit not recorded
  unmatched     commit 0123abcd (ref commit): no verification run in your ledger at this commit; 4 of your runs recorded no commit (cd before the command, or outside a repository)
  matched       receipt /abs/out/receipt.json (ref receipt)
  not checked   2 (commands that are not one isolated verification command, receipts without a readable local file)
```

The report quotes only what the message says and what the sender's own ledger
holds, which is already secret-redacted.

## Check before sending: `--claims-only`

`tt send ... --claims-only`, also with `--file`, validates the message, runs
the check and prints the report on stdout. It makes no hub request and sends
nothing. Exit status:

- 0 when every checkable claim is `matched`, or there is none, or the kind is
  not checked, or the agent has no ledger;
- 1 when any claim is `contradicted`, `unmatched` or `unproven`, or the check
  ran out of time (nothing was proven);
- 2, as before, for an invalid message.

`not checked` claims are counted and do not change the status. So a sender can
check, fix the message or rerun the command, and then send.

## What is not checked

- **Counts.** The ledger keeps a SHA-256 of the output, never the output, so
  "62 tests" or "4 pass 0 skipped" cannot be compared with it.
- **Free text.** Only the structured fields above.
- **Codex and other ledger-less senders.** They have no ledger.
- **Other agents' ledgers.** A lead's accept rests mostly on the verifier's
  runs, which are in the verifier's ledger; the check reads the sender's own.
  Reading a teammate's ledger would widen the per-agent ledger boundary.
- **Commits of runs after `cd`.** A call that changes directory records no
  commit, and a compound call is never credited. Run and cite single commands
  from the directory they need.
- **Compound or non-verification commands** cited in evidence.
- **Receipts without a local file**: a generation number, a hub id, a relative
  path, a missing, unreadable or undecodable file, a file whose JSON does not
  end within 1 MiB, or any other file name.
- **Other kinds and dispositions**: assign, request, review, question, a plain
  notice, and the `owner-accept`, `owner-decision` and `follow-ups`
  dispositions.
