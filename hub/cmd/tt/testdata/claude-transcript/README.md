# Claude Code transcripts with queue-operation records

Sanitized transcript excerpts for `claude_transcript_test.go` (bug `wi_614f0e656fcc0c35` revision 1, order #14254, assignment #14311, plan #14295, criteria a2-a6).

- Copied on 2026-09-28 on Stephens-Mini from real Claude Code session transcripts in `~/.claude/projects`. Each file is one contiguous run of records in the original order, with no records added or removed.
- Kept as recorded: record `type`, queue `operation` and `reason`, `timestamp`, assistant `stop_reason`, content part types, tool names, token usage counts, system `subtype` and attachment `type`.
- Replaced: every prompt, answer, thinking block, tool input and tool output with fixed fixture text. Queue `content` and background task notification prompts became `Fixture background notification.`; the originals were Claude Code's tagged notification blocks. `sessionId` is one synthetic UUID. `uuid`, `parentUuid`, tool-use and message ids are renumbered but keep their links.
- Dropped: `cwd`, `gitBranch`, `requestId`, `slug`, and every field of the metadata records (`last-prompt`, `ai-title`, `mode`, `atis-latch`) except `type`.
- Checked with the acceptance a6 grep (home paths, API key and token prefixes, private key headers) over this directory: no matches. This README avoids those strings too, so the grep covers it.

| Fixture | Source | Claude Code | Shape | Idle check |
| --- | --- | --- | --- | --- |
| `queue-idle` | owner session, lines 1654-1696, 2026-09-25 18:56:49-18:57:31Z | 2.1.281 | enqueue, dequeue, then a background task notification prompt starts a turn. An enqueue during the turn is removed with `absorbed_mid_turn`. The tool turn ends with `end_turn`, and metadata records follow. | idle |
| `queue-busy` | owner session, lines 676-681, 2026-09-25 15:41:59-15:42:09Z | 2.1.281 | A prompt, then an assistant `tool_use` whose result has not arrived, then an enqueue. This is the shape of the refusals for #14213 and #14240. | turn in progress |
| `queue-queued` | owner session, lines 5690-5704, 2026-09-27 14:55:45-14:55:56Z | 2.1.281 | An enqueue during a tool turn, then `end_turn` with no dequeue yet: queued input at the end of a turn. In the original, the dequeue and the queued prompt followed. | queued input pending while under 30 s old |
| `queue-remove-noreason` | agent session in another project, lines 726-734, 2026-08-27 04:57:50-04:57:56Z | 2.1.243 | A completed turn, then an enqueue and a `remove` without `reason` (older Claude Code format). | idle |

Tests copy a fixture into a temporary `HOME` and may append synthetic records: a dequeue, a user prompt, an `end_turn`, an unknown record type, or a malformed line.
