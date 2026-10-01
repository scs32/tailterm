# Synthetic time-accounting transcripts

Every file here is hand-made for `usage_time_test.go`. None is copied from a
real session: ids, models, prompts and outputs are fixture values, and all
timestamps are on 2026-01-01. Only the record shapes (types and key names)
follow real Codex rollouts and Claude Code transcripts.

- `codex-overlap.jsonl`: one turn with two overlapping `exec` calls, a
  `token_count`, and record types the parser must ignore.
- `codex-poll.jsonl`, `claude-poll.jsonl`: four turns each. Three requests
  that only wait on the inbox and then acknowledge; one blocking wait; an
  inbox check issued together with `go test`; a non-blocking inbox check
  followed by a text-only request.
- `claude-owner-wait.jsonl`: one turn, ended by `turn_duration`, that holds a
  20 minute inbox wait.
- `codex-order-change.jsonl`, `claude-order-change.jsonl`: one turn with two
  metered requests 20 minutes apart.

`FIXTURE-SECRET-ARG` appears inside commands so tests can prove that no
command text is retained in the cursor, the upload batch or the database.
The turns with more than 32 intervals are generated in the test.
