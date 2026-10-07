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

## Interrupted turns

Sanitized excerpts for `claude_stall_test.go` and the interrupt record in `claudeTurnEnd` (bug `wi_03ce50892a559767` revision 10, order #27407, assignment #27506, manifest step H1, criteria h4-h7 and h12).

- Captured on 2026-10-07 on Stephens-Mini from one disposable Claude Code 2.1.292 session on a private tmux socket (`tmux -L ttfix-03ce -f /dev/null`, 100x30, `env -i` with only HOME, PATH, USER, LANG and TERM=xterm-256color, `CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=0`, `claude --session-id <new uuid>`). Escape was sent with `tmux send-keys Escape`. No agent session was involved and user settings were not changed.
- Sanitized by the same rules as above: prompts, answers, thinking and tool input and output replaced with fixture text, ids renumbered with their links kept, `cwd`, `gitBranch`, `requestId` and metadata fields dropped. Kept as recorded: `isAbortedMidStream`, the `interruptedMessageId` link, and the text of the interrupt record itself, `[Request interrupted by user]`, which is Claude Code's own fixed string and not user content. The same grep finds nothing.

| Fixture | Source lines | Shape | Idle check |
| --- | --- | --- | --- |
| `interrupt-after-tool` | 71-84, 06:52:57-06:54:31Z | A completed turn, then a prompt, a Bash `tool_use` and its result, and five seconds into the thinking that followed, Escape: one `user` record whose content is a list with the single text part `[Request interrupted by user]`. No `turn_duration` follows. This is the shape of a stalled turn after a tool result. Tests drop the last record to get the open, quiet turn and append it again as Claude Code's answer to Escape. | idle, reason `turn interrupted` |
| `interrupt-streaming` | 23-43, 06:49:21-06:50:53Z | A prompt, a completed thinking block, then Escape after 66 seconds of streamed text: the partial text block is written with `isAbortedMidStream`, then the interrupt record. | idle, reason `turn interrupted` |
| `interrupt-no-record` | 4-22, 06:48:43Z | A prompt, then Escape 17 seconds into the first thinking, before any assistant record. Claude Code wrote nothing for the interrupt and put the prompt back in the input box. | turn in progress |

