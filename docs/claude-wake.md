# Claude Code inbox wake safety

Bug `wi_8401f9220e95bff5` revision 2, owner order #13130, builder assignment #13219 and handler Start #13226. This source change is a candidate; the order forbids push, merge and deployment.

Updated for bug `wi_9201e1f3901d6fdc` revision 2, order #14020, assignment #14056 and handler Start #14060: needs_input, prompt suggestions and skip logging.

Updated for bug `wi_7757e967b69a5ba6` revision 1, order #14126 and assignment #14142: dialog and input checks read only the active prompt area.

Updated for bug `wi_614f0e656fcc0c35` revision 1, order #14254 and assignment #14311: queued input, and unrecognized transcript records.

Updated for bug `wi_b6b79229c8fec99d` revision 1, order #14836 and assignment #14887: a bounded retry replaces "never retypes", agent windows start at a safe manual size, and an unconfirmed wake reports `stuck`.

Updated for bug `wi_132c8895adfe0886` revision 1, order #15557 and assignment #15583: a turn that an API error ended counts as complete.

Updated for bug `wi_85b4b3b61a6d8655` revision 1, order #20576 and assignment #20587: a transcript of any record count is read to its end from a cached place, and a wake skipped for one reason for ten minutes is reported once.

Updated for bug `wi_22eb717742d79f37` revision 1, order #20871 and assignment #20878: stray terminal replies in the input are cleared before a wake is typed, a wake typed but not submitted stays undelivered, and the transcript snapshot copies the provider block.

Updated for bug `wi_03ce50892a559767` revision 10, order #27407 and assignment #27506: an interrupted turn counts as complete, a stalled turn is detected, reported and (only where a host says so) interrupted once, and TailOS no longer sends the terminal replies that tmux typed into agent inputs. See "Stalled turns" and "Where the stray terminal replies came from".

The host relay can wake an idle Claude Code agent inside its own tmux session. It uses the same broker lease, unread-message path, 15-second spacing, eight attempts per five minutes and shared host request budget as Codex. A broker wake names open obligation sequences; an unread wake names eligible message sequences. Both tell the agent to read `tt inbox --unread --mark-read`. The relay never acknowledges an obligation or advances the read cursor.

An agent in `needs_input` is still woken when new input addressed to it arrives: an answer to its decision request, a directed message, or a new obligation. Until 2026-09-28 the relay skipped Claude agents in `needs_input` without logging anything, so a lead that asked the owner and set `needs_input` never saw the answer. The read cursor, queued-through progress, broker leases and the prompt-hash guard still stop repeat wakes for input already delivered. The pane and transcript checks below still refuse an agent that is busy or shows a prompt. The agent's own status is left unchanged.

Before input, the relay verifies the exact bound task, agent, run and session against one tmux pane and one Claude process descended from that pane. The transcript must be fully read to a completed turn with no pending tools and no freshly queued input. A turn is complete at an assistant `end_turn`, a `result` record, a `system` `turn_duration` record, the record Claude Code writes when an API error ends a response (an `assistant` record with `isApiErrorMessage: true`, an `error` code such as `server_error` or `rate_limit`, model `<synthetic>` and `stop_reason` `stop_sequence`), or the record it writes when Escape interrupts a turn: a `user` record whose content is a list holding one text part, `[Request interrupted by user]` or `[Request interrupted by user for tool use]`. Until 2026-10-07 the interrupt record read as a new prompt, so an interrupted agent stayed "turn in progress" and was never woken (see "Stalled turns"). A prompt a person types is recorded as a string, so typing those words still starts a turn. Such a response runs no tool, so a tool call it recorded is dropped from the pending set, and `turn_duration` likewise clears pending calls because no tool runs after a turn ends. Until 2026-09-30 only `end_turn` and `result` counted, so an agent stopped by an API error stayed "turn in progress" and every wake was skipped until someone woke it by hand (bug `wi_132c8895adfe0886`, order #15557). Claude Code writes a `queue-operation` record when it queues input while busy (`enqueue`), when it takes that input as the next prompt (`dequeue`), and when it drops the input or absorbs it into the running turn (`remove`, with reason `absorbed_mid_turn`, `delivered_to_agent` or none). These records do not change turn state. Input still queued when a turn ends means a new turn is about to start, so the relay refuses with `queued input pending`. Claude Code takes queued input within about 50 ms of a turn ending, so an enqueue with no matching dequeue or remove that is 30 seconds old or older after a completed turn counts as drift. It is logged once and the pane check decides. A real user prompt resets the queued count. Refusals name the actual state: `transcript incomplete` (the final record is still being written), `transcript read reached the 256 MiB bound for one check; continues on the next`, `no completed turn yet`, `turn in progress`, `tool call pending: <tool>`, `queued input pending (N)`, or a malformed-record error. A record type the relay does not recognize never blocks a wake. It is logged once per relay process, with the sorted key names and the short `type`, `subtype`, `operation` and `reason` values but no message or content text. Idleness is then decided from the turn state of the recognized records and from the pane check. Malformed JSON after the last completed turn still refuses. The pane check cannot see Claude's busy spinner above the input box. So if an unrecognized record ever starts a turn, the worst case is that Claude queues the wake text. Confirmation still requires the exact new user record, and the relay never types a second copy beside its own text in the prompt (see the retry rule below). Sanitized real transcripts are in `hub/cmd/tt/testdata/claude-transcript`. The visible pane must show Claude's empty input, with the cursor at the input start. The relay captures the pane with text attributes (`capture-pane -e`). Permission and selection dialogs, and the input itself, are checked only in the active prompt area: from the input box's top rule (the `──` row directly above the last `❯` row) to the end of the capture, faint text included. The transcript above the box is not a prompt, so an answer that quotes "Esc to cancel", "Allow this tool?" or "Do you want to proceed?" no longer blocks a wake. When no rule sits directly above the last `❯` (a real dialog's `❯ 1. Yes` row, a trust prompt, any unknown layout) the whole capture is checked, as before. A dialog refusal names the phrase and its row, for example `Claude pane has a permission or selection prompt: "esc to cancel" in capture row 30 (no input box, whole capture checked)`, and is logged once while the screen is unchanged. After that check, faint (SGR 2) text after the `❯` marker is Claude Code's own drawing, not input: this covers the `Try "…"` placeholder and the prompt suggestion that Claude Code 2.1.284 builds from the previous prompt, for example `Run tt inbox --unread --mark-read.` The relay's literal text replaces a suggestion before Enter, and the owner observed that Enter alone does not submit one, so a suggestion never blocks a wake. Typed text is drawn without the faint attribute and moves the cursor, so it still refuses, even when its words match the suggestion. Faint text followed by normal text, escapes other than SGR in the input area, and unparsable SGR parameters all fail closed. Live captures and their provenance are in `hub/cmd/tt/testdata/claude-pane`. Permission dialogs, choices, menus, prefilled text, multiline input, partial transcripts, unknown states and identity changes skip delivery. The pane and transcript are rechecked before literal text and before Enter.

The relay writes a private run-scoped intent with the prompt hash, pane and transcript offset before typing. It sends literal text, waits at least 100 ms, then sends a separate Enter. A new complete `user` transcript record containing exactly that text, after the saved offset, confirms delivery within five seconds. Old turns, assistant echoes, tool results and partial lines do not confirm. If a send, confirmation or process fails after the intent is saved, the wake is ambiguous. Until 2026-09-29 the relay then never tried again ("prior Claude wake did not confirm; no resend"), so a lane whose Enter did not submit stalled silently. A viewer had shrunk the agent's window to 16x1, so Claude could not render and the text sat unsubmitted.

The relay now retries an unconfirmed wake a bounded number of times, and only while the agent still has unread input (the relay only wakes then). Every pass first rereads the transcript after the saved offset: the exact prompt confirms the wake, and a replaced transcript or a different real user turn abandons the intent so a normal fresh wake can follow. Otherwise nothing is sent before the backoff: 15, 30, 60 and 120 seconds after the previous attempt ends. When a retry is due:

- If Claude is idle and the same pane shows exactly the relay's own wake text, the relay presses Enter alone and never retypes.
- If the input is empty, Claude is idle and the transcript still lacks the prompt, the relay types the current prompt once, which covers the newest messages, with the same checks, 100 ms wait and separate Enter as a first wake.
- If the input holds stray terminal replies, alone or beside the relay's own unsubmitted wake text, the relay clears the whole input (see below), rechecks that it is empty, then types the current prompt once as in the previous case. This counts as a retry.
- Anything else skips as unsafe with its reason and does not count as a retry: other or extra text in the input, a dialog, a busy turn, a changed pane, or a pane too small to inspect.

After four retries the intent is `exhausted`. The relay logs one line, keeps reporting `Claude wake did not confirm after 4 retries; needs attention`, and types nothing for 15 minutes. Then a new cycle of four retries may start, so the relay is bounded but never silent forever. The intent file keeps `attempts`, `firstAt`, `retryAt`, `exhaustedAt` and a bounded `lastRetry` reason. `firstAt` survives retry cycles so the stuck age stays true. Retry outcomes use the same ambiguous, skipped and confirmed classes as a first wake.

The relay never types a wake after text already in the input. A terminal answers queries such as "what are your device attributes" on the pane's input, so when a query reaches a pane whose program did not ask it, the answer lands in Claude's editor as text nobody typed. On 2026-10-01 `?1;2c>0;276;0c` (two device-attribute replies with their escape introducers dropped) sat in agent inputs. Every wake was then skipped as occupied input until the owner helper cleared the panes by hand (bug `wi_22eb717742d79f37`, order #20871). The source was TailOS's own terminal answering tmux's attach question late or twice; TailOS no longer sends those answers (bug `wi_03ce50892a559767`, "Where the stray terminal replies came from" below). The relay still clears, because another terminal on a slow link can do the same. Now, when the empty-input check refuses and the capture shows one readable input box (the `❯` row under its rule, its wrapped rows, the lower rule and only known footer rows), the refusal carries the typed text and the relay classifies it:

- Only terminal replies: device attributes (`?1;2c`, `>0;276;0c`), a keyboard or mode report (`?1u`, `?2026;2$y`) or a cursor position (`12;40R`), each with or without a leading `[`, in any number, ignoring whitespace from wrapping (`claudeStrayReply`). The relay first inspects the pane again expecting exactly that text, which proves the same pane identity, an idle transcript read to its end, and that the input has not changed. It then presses Backspace once per character in one `tmux send-keys` command, waits 100 ms and runs the empty-input check again. Only an input that is then empty, on the same pane with the transcript unchanged, is typed into, through the same recheck, 100 ms wait and separate Enter as any wake.
- The relay's own earlier wake text with terminal replies before or after it, on the retry path only: cleared the same way, own text included, and the current prompt is typed again.
- Anything else, for example an owner's draft: left untouched and skipped as unsafe. The reason names it without quoting it, `Claude cursor is not at an empty input: stray input in the input line (28 characters) is not a terminal reply; left untouched`, and feeds the ten-minute skip report below.

A clear is refused, and the wake skipped as unsafe, when Claude is busy, the pane or transcript changed, the text is longer than 1024 characters (`claudeStrayClearRunes`), or tmux fails. One pass clears at most twice (`claudeStrayClearMax`), so an input that refills cannot hold the relay in a loop: the third time it skips with `stray input in the input line returned after 2 clears in one pass`. Each clear writes one log line, `[tt relay] <time> <agent> Claude wake cleared 14 characters of stray input (terminal replies "?1;2c>0;276;0c") before typing`. The wake intent keeps the same text in `cleared`, at most 240 bytes, and a count in `clears`; a pass that cleared and then skipped keeps it in the skip record's reason. The record holds a character count and at most 40 characters of the replies themselves, never other input text.

A wake that was typed but not submitted is undelivered. Only the exact new user record confirms a wake: text left in the input, with or without stray replies beside it, never does, and every such outcome is reported as `did not confirm` or as an unsafe skip, never as confirmed. It then follows the bounded retries above and ends `exhausted` if the pane never submits, with the `stuck` report below.

Agent windows start at a safe manual size. `tt spawn` creates each agent session at 200x50 and, in the same tmux command, sets that session's `default-size 200x50` and its window's `window-size manual`, so no viewer can attach in between. Global tmux options are never changed, and the policy comes back with every new session after a tmux server restart. Every 15 seconds, before any wake, the relay reconciles Tailterm-owned sessions created before this change. A window that is not `manual`, or is below 80x24, gets the same options plus `resize-window 200x50`, with one log line: `[tt relay] <time> <session> window resized WxH -> 200x50 (window-size was X)`. A manual window at or above 80x24 is left alone, and human sessions are never touched. TailOS attaches agent tiles with `attach-session -f ignore-size` on tmux 3.2 and newer, so a tiny tile never overrides another client. That flag is not a guarantee on its own: on tmux 3.7b, an ignore-size client that was the server's only client still sized a `window-size latest` window to 16x1. The manual policy and minimum size are the controls that protect agents (`TestIgnoreSizeAttachSemantics`). A hidden tile opens its PTY at 200x50 and sends its real size only once it is shown.

The idle check reads the transcript to its end, whatever the record count. The relay process keeps its place per run and transcript path, in memory only (file identity, offset, turn state, pending tool calls, queued-input count, malformed-record state), so the first check of a run reads the whole file and each later check reads only the records appended since. A replaced or shortened file, or a restarted relay, starts again from zero. One check reads at most 256 MiB (`claudeTranscriptReadMax`); a check that reaches the bound, or whose relay tick context ends, refuses, keeps its place, and the next check continues from there. Callers get a copy that shares nothing with the kept state: the pending-call and usage maps, the completed list and the provider block (`ProviderBlock`, a pointer the next check updates in place when the same provider failure repeats) are all copied, so changing a snapshot does not change the kept state and a later check does not change an earlier snapshot. Until 2026-10-01 every check started from zero and stopped after 128 passes of 256 records. A transcript longer than 32768 records therefore never counted as fully read, and every wake to its session was skipped as `transcript incomplete`, with no bound and no report. The owner helper's transcript wrote its 32768th record at 16:49:06Z on 2026-10-01 and the session went unwoken from 09:50 PDT for about two hours, missing owner messages (bug `wi_85b4b3b61a6d8655`, order #20576). The intake blamed tracked background tasks. They only made the transcript long: a running background task writes nothing that holds a turn open. Its launch gets an immediate tool result, its `<task-notification>` arrives as an ordinary queued prompt that starts and ends a normal turn, and the `task_status`, `mode`, `last-prompt` and similar records after a turn do not change turn state. `transcript incomplete` now means only that the final record is partly written; the check waits for its newline and never parses half a record. A wake intent records why typing was safe in `safe`, for example `turn complete, no pending tool call, no fresh queued input; transcript read to its end (36411 records, 61908496 bytes); pane input empty`. Measured on that 61.9 MB, 36411-record transcript on the Mac mini: 1.1 s for the first check, 3 to 4 ms for a later one.

A wake that stays skipped is reported once. The relay keeps a private run-scoped skip record beside the intent (`…claude-wake-skip.json`: reason, `since`, `lastAt`, `escalatedAt`). Only unsafe skips count: a busy turn, a pending tool call, a dialog, occupied input, an unreadable transcript. When the same reason has held for ten minutes (`claudeWakeSkipBound`), the relay posts one NOTICE, "The relay cannot wake a Claude agent that has unread input", naming the agent, run, tmux session, the reason and when it began, and logs `[tt relay] <time> <agent> Claude wake skipped since <time>, escalated once: <reason>`. The post's request identity is the run and the episode start, so a retry or a restarted relay cannot post it twice; a failed post is tried again at most once a minute (`claudeWakeEscalateRetry`). The notice goes to the newest live database handler. When the skipped agent is that handler, or the project has none live, it goes to the newest live owner helper, and with neither to the Board with no recipient; it is never addressed to the agent that cannot be woken. The post uses the relay's configured hub token and its shared host request budget, and costs one agent-list read and one post per episode. It carries no agent identity, so the hub attributes it to the token's caller. The agent is still skipped and the relay keeps trying. A changed reason starts a new episode, so an agent moving between tools is not reported, while one held on a single tool call or dialog for ten minutes is. Six minutes without an attempt (`claudeWakeSkipGap`, longer than the five-minute rate window) also starts a new episode, because the relay attempts a wake only while input is unread. A confirmed wake removes the record. An unconfirmed wake is not a skip: it has the retry and `stuck` reporting described here.

An agent whose pane is below 80x24, or whose Claude wake has stayed unconfirmed for three minutes while it is idle with unread input, reports the `stuck` activity state with the reason ([agent-activity.md](agent-activity.md)).

`tt relay --status` and the agent activity snapshot show the latest confirmed, skipped, failed or ambiguous outcome with bounded reason and message sequences. A repeated identical skip keeps its original timestamp, and a changed wake outcome persists even if execution activity remains `idle`. Provider-limit activity states remain separate from wake outcomes.

Every held-back wake is recorded and logged; none returns silently. This covers a paused project, a superseded run, a closed, exited or retired agent, an offline agent, a full rate window, and every unsafe, failed or ambiguous queue attempt on the broker or inbox path. Each record has the reason and either the message sequences or the read cursor and unread count. The relay log gets one line per change, `[tt relay] <RFC3339 time> <agent> wake skipped: <reason> seqs=[…]` or `after=#N unread=N`, so a 3-second tick cannot flood it. `tt relay --status` adds `skip="…" skip-seqs=[…] skip-at=<time>` and, when no page was read, `skip-after=#N skip-unread=N`. A confirmed wake clears it. The skip record stays in host-local progress: it adds no hub reads or writes and does not enter the activity snapshot. Deferrals are not recorded: the 15-second spacing, unread input the relay already delivered, and messages the inbox path leaves to broker wake jobs. A broker attempt that fails or finds the pane unsafe records its own skip, and an unsafe one returns the message to the inbox path.

`tt spawn` launches Claude agents with `CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=0` in the tmux session environment, and the handler launcher inherits that environment. This is defense in depth, not a dependency: the attribute rule above handles a suggestion if one appears anyway.

The focused Go suite uses synthetic Claude transcripts, isolated hub fixtures and private tmux sockets. The opt-in live check is `TT_LIVE_CLAUDE=1 go test ./cmd/tt -run '^TestClaudeWakeLivePrivateSession$' -count=1 -v` from `hub/`. It creates a local SQLite/HTTP hub and one disposable Claude session on a private tmux socket, checks idle identity, types the stray replies `?1;2c>0;276;0c` into its input, delivers a short wake that must clear them first, records text/Enter/confirmation times and removes the session. It never targets a working agent pane or the live hub. An untrusted test folder can show Claude's trust dialog; the check stops and cleans up without answering it.

Two more opt-in live checks use the same isolation. They run Claude under `tt wrap` from the checkout, with that `tt` on the session's PATH, so the session is online the way a spawned agent is. `TestClaudeWakeLiveNeedsInput` has the session ask a decision with `tt ask`, run `tt event needs_input` and end its turn. It then answers the decision and requires a confirmed wake through the real broker and inbox passes. `TestClaudeWakeLivePromptSuggestion` enables suggestions for that process only (`--settings '{"promptSuggestionEnabled":true}'`; the owner's user settings are not changed). It requires wakes to confirm while a suggestion shows, and real typed text to refuse with a logged skip. `TestClaudeWakeLiveScrollbackDialogWords` has a session quote the dialog words in an answer and requires a message to wake it; a second session, run with `--permission-mode default` for that process only, shows a real Bash permission prompt and must refuse with one logged skip naming the dialog, with the prompt untouched and the command not run. `TT_LIVE_CLAUDE_CWD` picks another trusted folder.

The idle footer allowlist accepts bypass mode (`⏵⏵ …`) and `? for shortcuts`. In default permission mode Claude Code 2.1.284 draws `⏸ manual mode on · ? for shortcuts`, which the allowlist refuses, so an agent in that mode is not woken.

## Stalled turns

Bug `wi_03ce50892a559767` revision 10, owner order #27407, owner answers #27465 and #27466, builder assignment #27506. This source change is a candidate.

On 2026-10-01 a verifier sat in one Claude Code turn for 17.5 minutes with its token counter frozen. The broker's nudge was held as `turn in progress`, the obligation went overdue, and the owner helper pressed Escape and typed the wake by hand. The relay now sees such a turn, reports it, and can recover it where a host allows that.

### The rule

A turn is stalled when all of this holds:

- The agent is a Claude agent whose hub status is `running`, and its transcript has been read to its end.
- The turn is open and no tool call is pending. A pending tool call, however old, is the hung-tool rule's business ([agent-activity.md](agent-activity.md)), never a stall.
- No transcript record and no worktree change is newer than the threshold: 900 seconds, or `TAILTERM_ACTIVITY_STALLED_SECONDS` from 300 to 86400 in the relay's environment. A value outside that range means 900.
- Two captures of the pane at least 60 seconds apart do not show a growing token counter.

The second signal is needed because Claude Code writes a transcript record only when a block of a response is complete. Observed on 2026-10-07 with Claude Code 2.1.292: a text block that streamed for 66 seconds wrote its record only when it ended. A healthy long block is therefore transcript-quiet, and only the status line tells it from a stall. Claude Code draws that line above the input box while it is busy: `✻ Marinating… (7s · ↓ 75 tokens · thinking with high effort)`. The relay reads the counter (`75`, `9.5k`) from the row above the input box, skipping blank rows and `⎿` rows (a tip, a task list), and only when the pane passes the checks a wake makes before typing (no dialog in the prompt area, an empty input, the cursor at its start) and the footer says `esc to interrupt`. A status line in the scrollback of an idle pane is never read.

- Both captures show a counter and the counters differ: the turn is slow, not stalled. The relay watches again from the second capture.
- Both show the same counter on the same pane: a stall the relay may interrupt.
- Either shows no counter: still a stall, because the transcript says so, but one the relay only reports. This covers the first seconds of thinking (`(15s · still thinking with high effort)` has no counter), streaming text (no status line at all), a dialog, a draft in the input, a pane whose identity changed and a capture that failed.

The relay runs the rule for each Claude binding in its loop, before delivery. It reads the activity observer's saved cursor, so it costs no transcript read of its own, and it asks the hub for the agent only when a turn has already been quiet for the threshold: twice per detection, and once more before a resume line. A cursor older than two minutes, or a transcript that has grown past it, decides nothing.

### What every host does

- **Record.** A private run-scoped file beside the wake intent, `…claude-stall.json`: `run`, `count` (stalls of this run), `since` (the last transcript record or worktree change), `detectedAt`, `mode`, `counter` or `unreadable`, `unread` and `wakeSeqs` (the input waiting at detection), `notInterrupted`, `interruptedAt`, `endedAt`, `rewakeAt`, `resumeAt`, `resumeConfirmedAt`, `outcome`, the frozen `notice` with `noticedAt`, `escalatedAt` (set with a repeat's notice) and `clearedAt`.
- **Log.** One relay log line per change, for example `[tt relay] <time> <agent> Claude turn stalled since <time> (stall 1 of run <run>, mode report): no transcript record or worktree change for 17m, no tool call pending, token counter 75 unchanged from <time> to <time>`.
- **Activity.** The agent reports `stuck` with `turn stalled: no output for 17m` for as long as the turn stays open with nothing pending and nothing written.
- **Notice.** One typed NOTICE per stall, subject "A Claude agent's turn has stopped producing output". It names the agent, run and tmux session, the signals, the stall's number in the run, the host setting, and what the relay did or would have done. It goes to the live owner helper; with none, or when the stalled agent is the helper, to the Board with no recipient. It is never addressed to the stalled agent, the database handler or the owner. Its request identity is `claude-stall-<run without prefix>-<since, unix seconds>` and its text is saved in the record before the first attempt, so a second pass, a retry (at most once a minute after a failed post) and a restarted relay replay one request and post once. It is posted as the relay, like the wake-skip notice.

A stall ends when the turn writes a record, starts a tool call or ends. The record keeps the count.

A stalled agent that has unread input can also get the ten-minute wake-skip notice described above. That one goes to the database handler under a `claude-wake-skip-` identity; the two differ in recipient and identity, each posts once, and the stall notice says the earlier one may describe the same episode.

### The host setting

Interrupting is off unless the host says otherwise. The setting is one key in `~/.config/tailterm/relay.json`, beside `hub.json`:

```json
{"claudeStallAction": "interrupt"}
```

`"report"`, a missing file, a file that cannot be read, malformed JSON, a missing key and any other value all mean report: the relay sends no key and types nothing. The key must be spelled exactly `claudeStallAction`; a key in another case is not it. The relay service is started with only `PATH` in its environment, which is why this is a file; the owner or the owner helper edits it by hand, per host, and there is no `tt` command for it. The file is read at each detection, so a change needs no relay restart. The mode is saved in the stall's record at detection: a stall detected under `report` is never interrupted later, and switching affects the next stall only. Each log line and notice states the mode that applied.

### What a host set to interrupt does

Once per stall, and only for the first stall of a run:

1. It captures the pane a third time. The pane, the counter and the checks above must be unchanged, and the transcript must not have grown.
2. It saves the interrupt time in the record, then sends one Escape to that pane with `tmux send-keys`. Saving first means no later pass and no restarted relay can send a second one for the same stall. A key that fails to send is not sent again.
3. It waits up to five seconds, and on later passes up to two minutes, for the transcript to show the turn ended: the interrupt record above.
4. Unread input is then delivered by the relay's normal wake in the same pass or the next, through the usual checks, intent record, 100 ms wait, separate Enter and transcript confirmation. The record keeps that time in `rewakeAt`.
5. If no wake has followed after 30 seconds and the agent has nothing unread, the relay types one fixed line through the same wake path: `Your previous turn stopped producing output and the relay interrupted it. Continue the work you were doing.` The record keeps `resumeAt` and `resumeConfirmedAt`. An unconfirmed line follows the wake path's bounded retries for at most ten minutes and is never typed as a fresh wake twice.

It does not interrupt, and types nothing, when the counter could not be compared, the pane or counter changed before the key, the transcript grew, the session is the owner helper's, or the stall is not the run's first. The record's `notInterrupted` and the notice say which.

A second or later stall in the same run is never interrupted in either mode. Each still posts its own notice, under its own identity, saying it is a repeat and that the agent needs a person.

### Limits

- Escape pressed before a turn's first assistant record writes nothing to the transcript, and Claude Code puts the prompt back in the input box (observed 2026-10-07, Claude Code 2.1.292). The relay then never sees the turn end and types nothing; after two minutes the record says so and the stall stands until a person acts. Escape while thinking after a tool result, or while text streams, writes the interrupt record.
- The status line format belongs to Claude Code. If it changes, readings become unreadable and the relay only reports. The counter is coarse above 1000 (`9.5k`), so a turn adding fewer than about 100 tokens in a minute after 15 quiet minutes would read as stalled.
- Why the 10-01 turn stalled inside the runtime is not known. This rule recovers a stall; it does not prevent one.
- Codex turns have no stall rule.

### Release

The relay half reaches a host when `tt` is reinstalled there and the relay restarts. It then detects and reports only, until `relay.json` on that host is changed.

### Checks

`go test ./cmd/tt -run 'TestClaudeStall|TestClaudeInterruptedTurnEnds' -count=1` from `hub/`. The tests use fake pane operations that record every key and every typed character, the sanitized transcripts `interrupt-after-tool`, `interrupt-streaming` and `interrupt-no-record` in `hub/cmd/tt/testdata/claude-transcript`, and the pane captures `spinner-*`, `streaming-no-status` and `interrupted*` in `hub/cmd/tt/testdata/claude-pane`. No check attaches to, captures or types into a live agent pane.

The opt-in live check is `TT_LIVE_CLAUDE=1 go test ./cmd/tt -run '^TestClaudeStallLivePrivateSession$' -count=1 -v`. It uses the same disposable session, private tmux socket and local hub as the wake checks below. The session runs one tool call and starts thinking; the check reads the token counter from the real pane, sends one Escape, requires the transcript to show the turn ended, and types the resume line through the wake path. Run on 2026-10-07 with Claude Code 2.1.292: the counter read `75`, the turn ended 226 ms after Escape, and the resume line confirmed 310 ms after it was typed. It does not wait 15 minutes; the rule's timing is covered by the fake-clock tests.

## Where the stray terminal replies came from

Bug `wi_03ce50892a559767` (s6, s8, s9, s10). On every attach tmux asks the client's terminal three questions: primary device attributes (`ESC [ c`), secondary device attributes (`ESC [ > c`) and its version (`ESC [ > q`). xterm.js, the terminal in TailOS, answers `ESC [ ? 1 ; 2 c`, `ESC [ > 0 ; 276 ; 0 c` and `ESC P > | xterm.js(6.0.0) ESC \`, and reports each answer through the same event as typed keys. tmux takes the first answers as answers. An answer it no longer expects goes to the active pane as keys, which is how `?1;2c>0;276;0c` reached Claude's editor (the editor dropping the `ESC [` is inferred, not tested).

Two routes produce such an answer, both reproduced on a private tmux socket with real xterm.js in Chromium and WebKit (`tests/terminal-replies-browser.mjs`), where a pane running `cat -v` showed `^[[?1;2c^[[>0;276;0c`:

- **Late.** The link stalls while the answers are in flight. Measured on tmux 3.7b: an answer three seconds after the attach was consumed, and one 5.5 seconds after was typed into the pane. This needs no fault in the client; a link that stalls for a few seconds during an attach is enough, and the owner was on a dropping in-flight connection that morning.
- **Stale.** A view reconnects, and xterm.js answers the old attach's question after the new connection is in place, so the new attach receives two sets. One xterm instance lives across a tab's reconnects and parses what it is given a moment later. In the foreground the old answer is parsed long before the one-second reconnect; a page whose timers run late, such as a background tab, can reverse that. The test clamps the page's timers to stand in for it. This route was reproduced under that condition, not observed in the field.

The fix is in the client (`client/terminal-replies.js`, used by every tab in `client/main.js`):

- On a tmux attach, device-attribute and version answers are never sent. tmux works without them: with no answer, typing reached the pane 0.1 seconds after the attach.
- Any terminal answer (those, a cursor position, a mode or keyboard report, a status, a setting or a colour report) is dropped when it belongs to an earlier connection, and after the tab is disposed. A reconnect queues an empty write behind everything xterm.js already holds; answers produced before xterm.js reaches it are for the old connection.
- Typed keys, paste and mouse reports always pass, and so does an answer to the current connection's question, for example a cursor position a full-screen program asks for.

What it does not cover: a tab opened as a plain shell in which a person starts tmux by hand still sends the attach answers, and so does any other terminal program on a slow link. The relay's clear-before-wake above stays for that reason. The fix reaches users with a TailOS deploy; until it is released and observed, the owner helper's stray-input cleaner stays in place. Not established: that these two routes are the only ones.

Checks: `node --test tests/terminal-replies.test.js` (in `npm test`) and `node tests/terminal-replies-browser.mjs` (`npm run test:terminal-replies`), which starts its own tmux server, SSH server and PTYs and leaves none behind. The browser test is not yet in `verification/matrix.json`; it is run by direct command until that entry is added.

### Focused TailOS pane sizing

Bug `wi_9e5d8194ed093cd4`, owner order #24075, implementation #24163,
replaces the fixed-viewer expectation of `wi_b6b79229c8fec99d` only for an
explicit foreground, visible, focused ordinary agent pane. Spawn still starts
at 200x50 with manual sizing, and every agent attach keeps `ignore-size`.
Hidden, collapsed, minimized, background, Board and closed panes cannot initiate
new claims or program-size updates. Owner decision #24371 (saved revision/scope 5,
handler #24387) permits only an eligible claim already submitted before
hide/dispose to arrive afterward; it keeps the 80x24 floor and releases authority.
This is not transport cancellation; residual `wi_1b1a86c6be5b6bcc` remains held.
Newer-viewer and exact-identity guards still apply. A tiny visible pane crops a
program window of at least 80 columns and 24 usable rows. The existing tmux status rows are deducted
from the visible viewport before clamping; status configuration is untouched.

The latest focus claim accepted by the host wins across browser viewers. An
ordinary resize never claims authority. Same-size refocus and verified reconnect
claim again. Synchronous tmux format guards check session ID/creation, task, agent,
run, non-helper role and the single-pane `agent` window together with a window-local
viewer token. A stale viewer's resize or release cannot change a newer viewer's
size. Renaming retains identity; a reused session name cannot redirect a command.
Hiding or disconnecting conditionally releases authority and retains the last
safe dimensions. Command failures are shown in the browser; they do not trigger
an automatic reclaim loop. The relay already leaves manual windows at or above
the minimum alone, including a safe size smaller than the 200x50 default.
The owner helper and ordinary human shells remain excluded. An ordinary launcher
pane later adopted as an agent first reattaches with `ignore-size`, preserving its
exact target, tab and run binding outside Home (lead clarification #24225). A
failed replacement stays ineligible and shows the SSH failure.

Acceptance uses a private tmux socket and a PTY/SIGWINCH program through fixture
SSH in both engines of `tests/static-browser.mjs`, plus controller, command, spawn
and relay regressions. No live owner/agent sessions are test fixtures.

## Tool-call ledger

Feature `wi_f38d51348f280538` revision 3, owner order #25975 with amendments #25980 and #26096 and answers #25999 and #26032, builder assignments #26043 and #26116. The owner helper session was added by bug `wi_b3e8ecd5a2ee01b3` revision 3, owner order #26838 with amendment #26841, builder assignment #26858. The would-deny report was added by feature `wi_23ca1183c7d59c79` revision 2, owner order #31851, builder assignment #31889. The owned-paths report was added by feature `wi_c1ef66c9f6c8aadc` revision 3, owner order #31964, builder assignment #31979. The handler-gate report was added by feature `wi_cbc08bd2b2a419a0` revision 2, owner order #32007 with decision answer #32034, builder assignment #32041. The edit-before-assign report was added by feature `wi_03fe150782cc7a8e` revision 2, owner order #32066, builder assignment #32082. This source change is a candidate.

Every tool call in a Tailterm Claude agent session leaves one private local row: which tool, a digest of its arguments, how it ended, how long it took, and the agent and run. The owner helper's Claude session is covered too (see "Owner helper session"). The ledger is observe-only. It uses Claude Code settings hooks, the same mechanism as the four hooks above. It is not a Claude Code mod; a mod front end can come later. Codex workers are not covered.

A Bash call that matches a protected-operation rule leaves one extra row per matched rule, written when the call is attempted (see "Would-deny report"). The report blocks nothing. Codex sessions are not covered by it either: Codex has only a Stop hook, so no Codex tool call is seen.

An Edit, Write, MultiEdit or NotebookEdit of a file outside the paths of the agent's latest ASSIGN leaves one extra row too (see "Owned-paths report"). That report also blocks nothing and does not cover Codex.

The same four tools, used by a queue builder that has read its inbox and has no ASSIGN cached for its run, leave one extra row for a file inside the repository (see "Edit-before-assign report"). That report blocks nothing and does not cover Codex either.

A Bash call that writes a work item through `tt`, sends a non-GET request to the hub or opens the hub database with `sqlite3` leaves one extra row per rule as well, tagged with who ran it (see "Handler-gate report"). That report blocks nothing and does not cover Codex either.

### Hooks

`tt hooks claude` prints, and `tt host setup` installs into the user-level Claude `settings.json`, three more events: `PreToolUse`, `PostToolUse` and `PostToolUseFailure`. Each runs `tt hook tool` in a group with no matcher, so every tool is seen, and each entry carries `"timeout": 5`: Claude Code abandons the hook after 5 seconds if it ever hangs (see "What Claude Code does when the hook misbehaves"). Host setup adds the timeout to a `tt hook tool` entry installed without it, or with another value. The four older hooks are written exactly as before, with no timeout. Host setup keeps the user's own entries for these events, for example a `PreToolUse` group with a `Bash` matcher, and moves a `tt hook tool` entry out of a matcher group.

`tt hook tool` can never deny, delay, rewrite or fail a tool call:

- It writes nothing to stderr, and nothing to stdout unless secret redaction is switched on for the host (see "Secret redaction"; it is off by default). Even then it cannot deny or fail a call: its only answer is the same output with values replaced.
- It always exits 0. The handler has no return value and `cmdHook` returns nil after it.
- The would-deny report changes none of this. It only appends rows: it prints nothing, has no return value, and a command it cannot read or classify is left unreported.
- The owned-paths report changes none of this either. It only appends rows, and anything it cannot read or judge is left unreported.
- The edit-before-assign report is the same check with a second rule. It only appends rows, and anything it cannot read or judge is left unreported.
- Nor does the handler-gate report. Its rows are appended in the would-deny report's one write, and a command that cannot be read or classified is left unreported.
- It makes no hub request and opens no network connection. A slow or unreachable hub does not matter.
- The whole command returns within 200 ms. The handler reads stdin and does its work in a goroutine and waits at most 150 ms for it; when the handler returns the process exits, which ends work still blocked on stdin or the disk. This holds for stdin that is closed, held open, empty, malformed or oversized.
- With `TAILTERM_AGENT` set, the session is an agent's and nothing below changes it. If `TAILTERM_TASK` or the hub is then missing, it returns before reading stdin and touches no file. An agent id with characters other than letters, digits, `_` and `-` is also a no-op, because the id becomes a directory name.
- With no `TAILTERM_AGENT`, it looks for the owner helper registered from this exact session (see "Owner helper session"). When neither `CLAUDE_CODE_SESSION_ID` nor `CODEX_THREAD_ID` is set, or both are, it returns before touching any file. Otherwise it lists the relay state directory and reads the helper files there. Unless exactly one matches, it returns before reading stdin and writes nothing.

Assumptions behind the 200 ms, not changed by this feature: before any hook runs, `tt` sets `PATH` and reads the small local file `~/.config/tailterm/hub.json` with no deadline, as every `tt` command and the four existing hooks do. A home directory on a stalled filesystem would hold all of them. One exception was measured: the first run of a new `tt` binary file on macOS takes 216 to 358 ms, once after each install, because the system checks a new binary on its first run. Later runs took 6 to 12 ms, and about 160 ms with stdin held open.

When the 150 ms runs out, that invocation writes nothing more. A pre cut off leaves nothing. A post cut off before it claims its pending entry leaves the entry, which later becomes an `unknown` row; a post cut off between claiming the entry and appending the row loses that row.

### Where the rows are

`~/.local/state/tailterm/tool-ledger/<agent id>/`, beside the relay state:

- `ledger.jsonl`: the rows, one JSON object per line.
- `ledger.jsonl.1`: the previous file after a rotation. Nothing older is kept.
- `ledger.lock`: the lock for appends and rotation.
- `pending/`: one small file per call that has started and not finished.
- `owns.json`: the owned paths of the agent's latest ASSIGN, written by `tt inbox` and read by the hook (see "Owned-paths report").
- `owns.lock`: the lock for writing `owns.json` and `inbox.json`.
- `inbox.json`: the marker that this run of the agent has fetched its own messages, written by `tt inbox` and read by the hook (see "Edit-before-assign report").

Directories are 0700 and files 0600. `TAILTERM_TOOL_LEDGER_DIR` replaces the `tool-ledger` directory; tests use it.

### Row fields

| Field | Meaning |
| --- | --- |
| `v` | 1 |
| `time` | UTC RFC 3339 time the row was written |
| `task`, `agent`, `run` | `TAILTERM_TASK`, `TAILTERM_AGENT` and `TAILTERM_RUN` of the session; for the owner helper session, `task`, `agent` and `run` from its helper file |
| `session` | Claude Code `session_id` |
| `tool` | `tool_name`, cut to 128 bytes |
| `toolUseId` | `tool_use_id`, cut to 128 bytes |
| `argsDigest` | lower-case hex SHA-256 of `tool_input` after it is decoded and encoded again by Go's `encoding/json` (object keys sorted, numbers kept as written); empty when there is no `tool_input` |
| `outcome` | `ok` (PostToolUse), `error` (PostToolUseFailure), `interrupted` (failure with `is_interrupt`), `unknown` (a call that started and never finished), `unreadable` (input the hook could not use), `would_deny` (a PreToolUse that matched a protected-operation rule; the call still gets its own row when it ends), `would_report` (a PreToolUse of an edit outside the agent's owned paths, of a queue builder's edit before any ASSIGN is cached for its run, or of a Bash command that matched a handler-gate rule; the call still gets its own row when it ends) |
| `durationMs`, `durationSource` | `duration_ms` from Claude Code (`claude`), else the time since the call's pre hook (`measured`), else absent |
| `oversize` | `true` only when the input passed the 8 MiB cap |
| `denyRule`, `denyKind`, `denySummary` | only on a `would_deny` row: the rule id, the operation kind and a summary of fixed words (see "Would-deny report") |
| `reportRule`, `reportPath`, `assignSeq` | only on a `would_report` row: the rule id, the file's path relative to the repository root cut to 200 bytes, and the number of the ASSIGN it was judged against (see "Owned-paths report"). An `edit-before-assign` row has `reportRule` and `reportPath` and no `assignSeq` (see "Edit-before-assign report"). A handler-gate row has `reportRule` and neither of the other two |
| `reportTag`, `reportSummary`, `agentName` | only on a handler-gate `would_report` row: who ran the command (`handler`, `lead-allowed`, `owner-helper` or `none`), a summary of fixed words, and the agent's name (see "Handler-gate report") |

Claude Code gives no numeric exit code. A Bash command that exits non-zero arrives as PostToolUseFailure, so it is an `error` row.

Never stored, in the ledger or in a pending file: `tool_input`, `tool_response`, `error`, `cwd`, `transcript_path`, file contents, tokens or any environment value. The hook decodes only the fields in the table, and for the owned-paths report `cwd` and the one path field of an edit tool's `tool_input`. Neither is stored, with one exception: the repository-relative path of a reported edit, in `reportPath`. The repository root and `cwd` never reach a row. From a helper file only `task`, `agent` and `run` reach a row; its registration and request fields do not. A `would_deny` row is no exception: its summary is put together from fixed words and a count, and no text is copied from the command. A handler-gate row's summary is built the same way. That row holds one environment value, the agent's name from `TAILTERM_AGENT_NAME`, or the helper file's `name` for the owner helper, and only when it is made of letters, digits, `_`, `-` and `.`. The hub address in `TAILTERM_HUB` is compared against the command and never stored.

### Owner helper session

The owner helper is the owner's own Claude Code session (`docs/owner-helper.md`). Its process environment has no `TAILTERM_AGENT`, `TAILTERM_TASK` or `TAILTERM_RUN`: it takes its identity per command from `tt helper env`. Until this change `tt hook tool` returned at once in that session and none of its calls were recorded (bug `wi_b3e8ecd5a2ee01b3`).

With no agent in the environment, the hook now resolves the helper from this host's files only:

- The session is named by `CLAUDE_CODE_SESSION_ID` (runtime `claude`) or `CODEX_THREAD_ID` (runtime `codex`). Exactly one must be set, and it must be a thread UUID.
- The hook lists the relay state directory (`~/.local/state/tailterm/relay`, or `TAILTERM_RELAY_STATE`) for names ending `.owner-helper.json`, the files `tt helper register` writes, one per hub and project.
- A file matches when its `runtime` and `thread` equal the session's exactly, its `agent` and `task` are well-formed ids, its `run` is not empty, and its file name is the one its own `hub` and `task` give it. A copied or renamed file is not believed.
- Exactly one matching file gives the identity: rows are written to `tool-ledger/<agent>/` with that file's `agent`, `task` and `run`, in the same format and under the same limits as any agent's rows.
- No match writes nothing. Two or more matches (one thread registered as the helper of two projects) are ambiguous and write nothing. A file that is not JSON, is larger than 64 KiB, or lacks any of those fields is not a match. If `TAILTERM_TASK` or `TAILTERM_RUN` happens to be set without an agent, the file must carry the same value.

The hook never asks the hub, never reads tmux session tags and never guesses: the hub and task are not inputs, and nothing is taken from a partial environment identity. The lookup runs inside the same 150 ms wait, before stdin is read. It reads directory names without a stat per entry, and reads at most 16 helper files; a directory with more helper files than that writes nothing. A test runs it with 10,000 unrelated files in the directory inside the 200 ms bound.

The lookup depends on Claude Code giving a hook process the session's own id. Observed on 2026-10-06 with Claude Code 2.1.292: a `claude -p` run in a temporary project, with `PreToolUse` and `PostToolUse` hooks in its `.claude/settings.json` and no `CLAUDE_CODE_SESSION_ID` or Tailterm identity in the parent environment, ran one Bash `true`. In both hook processes `CLAUDE_CODE_SESSION_ID` was set and equal to the payload's `session_id`. An interactive session was not tried. Codex has no tool hook, so a Codex helper session is not ledgered; the `codex` rule exists so the two runtimes are matched the same way.

To check that a helper session is covered, in that session:

```sh
tt helper env --task <project id>    # prints TAILTERM_AGENT=<helper agent id> and TAILTERM_RUN
ls ~/.local/state/tailterm/tool-ledger/<helper agent id>/
tail -n 1 ~/.local/state/tailterm/tool-ledger/<helper agent id>/ledger.jsonl
```

The last row's `agent`, `task` and `run` name the helper and its `session` is the session's id. No directory after the session has made a tool call means one of: the helper was not registered from this session (run `tt helper register`), it was registered again from another session, the host's `tt` is older than this change, or the tool hooks are not installed (`tt host setup`).

### Pairing and bounds

A pre hook writes a pending file of at most 512 bytes with the start time, session, tool-use id, tool name and digest. The post or failure hook removes it and writes the row; the process whose remove succeeds owns the entry. A call with a tool-use id is paired by session and id. A call without one gets its own file in a per-session queue, and a post claims the session's oldest entry with the same tool and digest, so identical overlapping calls pair in start order. A post with no pending entry still writes its row from its own input.

- Input: at most 8 MiB of stdin is read. A larger input, malformed JSON or an event other than the three gives one `unreadable` row with the identity fields and `time` only, and no pending file is created or claimed. So a call whose post is above 8 MiB shows as one `unreadable` row then and one `unknown` row later.
- Pending: before a pre creates its file, entries older than 24 hours and the oldest entries beyond 255 are each written as an `unknown` row and removed, at most 32 per invocation. After any pre there are at most 256 pending files per agent. Pres racing in parallel can pass that by the number racing; the next pre brings it back.
- Ledger: every append takes `flock` on `ledger.lock`, retrying every 2 ms for at most 50 ms. Under the lock the size is read again, and a `ledger.jsonl` of 4 MiB or more is renamed over `ledger.jsonl.1` before the row is appended in one write. If the lock is not obtained the row is appended without rotating. One agent therefore holds about 8 MiB at most.

### Would-deny report

Report-only. On PreToolUse, after the call's pending entry is written, the hook reads the `command` of a `Bash` call and checks it against four rules. Each rule that matches adds one row with `outcome` `would_deny`, the call's `session`, `tool`, `toolUseId` and `argsDigest`, the usual `task`, `agent`, `run` and `time`, and the three `deny` fields. A command that matches two rules leaves two rows. The call is not blocked, delayed past the hook's own wait, or changed, and it gets its ordinary row when it ends. Turning blocking on is a later owner decision and is not built: it would mean exit 2 on PreToolUse and a change to the contract above.

| `denyRule` | Fires when | `denyKind` |
| --- | --- | --- |
| `host-pkill` | the command is `pkill` or `killall` with none of the options that narrow it: `-U`, `-u`, `-P`, `-g`, `-G`, `-s`, `-t`, `-F` for pkill, `-u`, `-t` for killall. `killall -l` and `killall -s` only list and do not fire. | `pkill`, `killall` |
| `deployer-signal` | the command is `kill`, `pkill`, `killall`, or `tmux` with `kill-session`, `kill-window`, `kill-pane`, `send-keys`, `respawn-pane` or `respawn-window` (or a tmux alias of one), and its own words contain `tt-deployer`. That prefix covers `tt-deployer-compact` and the `tt-deployer-<agent>` sessions. | `kill`, `pkill`, `killall`, `tmux-kill-session`, `tmux-send-keys` and so on |
| `old-site-deploy` | `npm`, `pnpm`, `yarn` or `bun` with `run` or `run-script` and the script `deploy:static`; or `node` running a file named `deploy-static.mjs`, or that file run directly. `node --check` does not fire. | `npm-run`, `node-script` |
| `truenas-tailscale` | `ssh` to the TrueNAS host with a remote command that contains `tailscale`, in any case. The host is recognised by a name containing `truenas`, in any case, after an optional `user@`, or by the one address `100.116.238.37`. | `ssh` |

How a command is read: it is split into simple commands at `;`, `&&`, `||`, `|`, `&`, new lines and the bounds of `$( )`, backticks, `( )` and `{ }`. Quotes and backslashes are honoured, so text inside `echo "…"`, a commit message or a board post is an argument and never a command. Heredoc bodies and comments are skipped. Leading `NAME=value` assignments, the shell words `if`, `then`, `else`, `elif`, `do`, `while`, `until` and `!`, and the wrappers `sudo`, `env`, `command`, `exec`, `nohup`, `time`, `timeout` and `xargs` are stepped over, and the command is compared by its base name, so `/usr/bin/pkill` is `pkill`. The argument of `bash -c`, `sh -c`, `zsh -c` and `eval`, and the remote command of any `ssh`, are read as commands in turn, three deep.

These do not fire: `pkill -U self node`, `pkill -P 12345 node`, `kill 12345`, `pgrep -f node`, `echo pkill node`, `tmux has-session -t tt-deployer-compact`, `tmux ls | grep tt-deployer`, `npm run deploy:tailos`, `cat scripts/deploy-static.mjs`, `tailscale status` on this host, `ssh truenas midclt call app.query`, `ssh air tailscale status`. A tool other than Bash never fires, whatever its input holds.

`denySummary` is at most 120 bytes and is built only from:

- the command word the rule matched: `pkill`, `killall`, `kill`, `tmux`, `npm`, `pnpm`, `yarn`, `bun`, `node`, `ssh`, `deploy-static.mjs`;
- a subcommand from a fixed list: the tmux subcommands above, `run`, `run-script`, and after ssh the word `tailscale` and one of `up`, `down`, `set`, `status`, `serve`, `funnel`, `login`, `logout`, `update`, `cert`, `ip`;
- option names from a fixed list, never an option's value: pkill `-f -x -i -n -o -l -a`, killall `-e -I -q -v -w -m`, tmux `-t`, npm `--prefix`, ssh `-o -p -i -l -t`, and a signal that is a known name or a number from 1 to 31;
- the protected name: `tt-deployer`, with its suffix only when the whole word is `tt-deployer-` and up to 32 lower-case letters and digits; `deploy:static`; `deploy-static.mjs`; `truenas` for any host name, or the address above;
- `(+N args)`, the number of other words left out. After eight words the rest are only counted.

Examples: `pkill -9 -f (+1 args)`, `tmux kill-session -t tt-deployer-compact`, `kill tt-deployer (+1 args)`, `npm run deploy:static`, `node deploy-static.mjs (+1 args)`, `ssh truenas tailscale up (+2 args)`. A pattern, path, host, user, option value or any other argument never reaches a row. The finished text is still passed through the secret scan of "Secret redaction", whatever that feature's mode, and anything it finds is written as `[redacted]`; if the scan cannot finish in its time the row is written with an empty summary.

Fail open: nothing extra is written for input the hook cannot read (the `unreadable` cases above), a `tool_input` with no `command` string, a command longer than 64 KiB (it is not classified in part, because a cut inside a quote could invent a match), substitutions nested more than eight deep, a panic in the check, or a check that ends after the 150 ms wait. The pending entry is written before the check, so none of these changes the call's ordinary row.

To count would-deny rows by rule and agent for this host:

```sh
awk 1 ~/.local/state/tailterm/tool-ledger/*/ledger.jsonl* 2>/dev/null | /usr/bin/jq -Rr 'fromjson? | select(.outcome == "would_deny") | "\(.denyRule)\t\(.agent)"' | sort | uniq -c
```

Each line is a count, a rule id and an agent id. It reads the current and the rotated file of every agent and skips a torn line. `awk 1` ends every file with a new line, so a torn last line in one file cannot swallow the first row of the next.

Cost, measured on the Mini on 2026-10-10 by `TestToolDenyCost`, which runs the same hook 100 times with the check and 100 times without and compares the medians: under 0.03 ms for a short command that matches nothing, about 0.1 ms for a match (the extra append), and about 1 ms for a command at the 64 KiB cap, matching or not. The test fails if the check adds 5 ms to the median or if a run with the check does not return, rows written, before the 150 ms deadline.

Limits of the report:

- It reads the command text only. A kill by a bare PID learned in an earlier call, a script that runs pkill, or a variable holding the session name is not seen. So a kill of the deployer by PID is not seen: the hook may not start a process to resolve one.
- `pkill -U` or `-u` counts as narrowed. On the Mini every agent runs as one user, so it is still wide. That needs deciding before blocking is ever switched on.
- TrueNAS is recognised only by a name containing `truenas` or by the address above. Another address, or an alias without `truenas` in it, is not seen.
- `truenas-tailscale` reports reads such as `tailscale status` as well as changes.
- The remote command of an ssh to any host is read as a command, so `ssh air pkill node` is a `host-pkill` row although the Mini is not the target.
- `tmux kill-server`, `kill -1` and `wrangler` called directly are not rules.
- Bash only. Other tools, and Codex sessions, are not covered.

### Owned-paths report

Report-only. It records an edit outside the files an agent was assigned, and blocks nothing.

**What `tt inbox` caches.** Every form of `tt inbox` that fetches messages (`--unread`, the history page, `--board`, `--seq N`, text or `--json`) looks through what it fetched for a typed ASSIGN addressed to its own agent with at least one `owns` entry. It writes the newest one to `owns.json` beside the agent's ledger:

| Key | Meaning |
| --- | --- |
| `v` | 1 |
| `run` | `TAILTERM_RUN` of the `tt inbox` that wrote the file |
| `assignSeq` | the ASSIGN's message number |
| `item`, `itemRevision` | the ASSIGN's primary work-item link, else its first link, else empty and 0 |
| `time` | UTC RFC 3339 time of the write |
| `owns` | the ASSIGN's owned paths as written |
| `overflow` | `true`, with no `owns`, when the ASSIGN has more than 256 entries, an entry over 512 bytes, or a list that would pass the 192 KiB the hook reads |

- It holds no subject, objective, acceptance or other message text.
- The file is 0600 in the 0700 agent directory. It is written to a temporary file and renamed, so a reader sees the old file or the new one.
- The latest ASSIGN wins: a higher message number replaces the cache, so an amended ASSIGN replaces the one before it. An equal or lower number for the same run writes nothing.
- An ASSIGN stays the agent's across a recovery run. A message names an agent, not a run, so a new run of the same agent that reads an ASSIGN caches it under its own run, whatever its number. The hook uses a cache only when its `run` is the hook's own run.
- The read of the current file, the comparison and the rename happen under `flock` on `owns.lock`, tried every 2 ms for at most 200 ms. Without the lock nothing is written, and the next inbox read that fetches the ASSIGN caches it. That 200 ms is the most the cache write can delay `tt inbox`.
- `tt inbox --seq N` fetches the message once more for the cache, after printing it: one extra hub request per `--seq` read made by an agent. That request is given 1 second, so with a hub that does not answer a `--seq` read can end up to 1 second later, and then up to the 200 ms above for the write. The other forms make no extra request.
- It never changes what `tt inbox` prints or returns. A ledger directory that cannot be written, or any other failure, writes nothing.

**What the hook does.** On PreToolUse of `Edit`, `Write`, `MultiEdit` or `NotebookEdit`, after the pending entry is written and after the would-deny report:

1. It reads `owns.json`. With no usable cache it stops.
2. It takes the file from `tool_input`: `file_path` for Edit, Write and MultiEdit, `notebook_path` for NotebookEdit. A relative path is joined to the hook input's `cwd`.
3. It finds the repository root: from `cwd` upwards, the first directory holding `.git`, as a directory or a file (a worktree has a file). At most 64 levels, stat calls only.
4. A file outside that root is ignored.
5. A file inside it that no entry owns adds one row: `outcome` `would_report`, `reportRule` `outside-owned-paths`, `reportPath`, `assignSeq`, and the call's `session`, `tool`, `toolUseId`, `argsDigest`, `task`, `agent`, `run` and `time`.

The call is not blocked, delayed past the hook's own wait, or changed, and it gets its ordinary row when it ends. No process is started and no hub request is made.

**How an entry owns a path.** Three passes, each over every entry; the first hit means owned.

- An entry's literal form is the entry as written with surrounding spaces, a leading `./` and a trailing `/` dropped, and an absolute entry made relative to the root (ignored when it is outside the root). Brackets, parentheses, `*` and `?` are ordinary characters in it. An entry of `.` owns the whole repository.
- Pass 1, literal: the path equals the literal form or lies under it.
- Pass 2, the ` (new)` note: only for an entry ending in exactly ` (new)`, and only when nothing with that whole name exists. The entry without the note is then matched as in pass 1. No other parenthesised text is removed.
- Pass 3, glob: only for an entry containing `*`, `?` or `[`, and only when nothing with that exact name exists. The pattern must match the whole relative path, and `*` does not cross a `/`. A bad pattern matches nothing.

So an entry that names an existing file or directory only ever owns that file or directory: `src (archive)` owns what is under `src (archive)`, never `src/x.go`, and an existing `[id].go` owns itself, never `i.go`.

`reportPath` is the path relative to the repository root, passed through the secret scan of "Secret redaction" whatever that feature's mode, then cut to 200 bytes without splitting a character. File contents are never read or stored.

Fail open: nothing extra is written for a missing, unreadable, malformed or oversized `owns.json`, one marked `overflow`, one with no usable entry, or one written by another run; a missing or relative `cwd`, or one that is not a string; no `.git` above `cwd`; a `tool_input` without its path string; a path scan that cannot finish in its time (a row must carry its path, so none is written); a panic in the check; or a check that ends after the 150 ms wait. The row's append never waits for the ledger lock past that deadline and checks the deadline again immediately before its one write. The pending entry is written before the check, so none of these changes the call's ordinary row.

To count would-report rows by rule and agent for this host:

```sh
awk 1 ~/.local/state/tailterm/tool-ledger/*/ledger.jsonl* 2>/dev/null | /usr/bin/jq -Rr 'fromjson? | select(.outcome == "would_report") | "\(.reportRule)\t\(.agent)"' | sort | uniq -c
```

Each line is a count, a rule id and an agent id, read the same way as the would-deny command above. Handler-gate rows are `would_report` rows too, so this command now counts their three rules beside `outside-owned-paths`; the command under "Handler-gate report" counts them alone, by tag. It counts `edit-before-assign` rows as well; the command under "Edit-before-assign report" lists those alone, by path.

Cost, measured on the Mini on 2026-10-10 by `TestToolScopeCost`, by the method of `TestToolDenyCost`: under 0.01 ms for a Bash call and for an edit with no cache; about 0.06 ms for an owned edit against a 256-entry cache; about 0.15 ms for an edit outside it (the extra append); about 0.8 ms for a Write outside it with 1 MiB of content; about 0.45 ms for an edit outside 256 glob entries (one stat call each). The test fails if the check adds 5 ms to the median or if a run with the check does not return, row written, before the 150 ms deadline.

The path fields were checked against Claude Code 2.1.296 on 2026-10-10: the tool definitions of a live session give `file_path` for Edit and Write and `notebook_path` for NotebookEdit. That version offers no MultiEdit tool; its name remains in the binary's permission lists, and `file_path` is its earlier definition, not confirmed live.

Limits of the report:

- Edits made through Bash (`sed -i`, a redirection, a script, `git apply`) are not seen.
- Codex sessions are not covered.
- An agent that never read its ASSIGN through `tt inbox` has no cache, and nothing is reported for it. An ASSIGN posted as plain text, without the typed envelope, is not cached either.
- Files outside the session's repository are ignored. From a worktree session that includes the main checkout and other worktrees.
- Matching is by name. Nothing is resolved, so a file reached through a symbolic link is judged by the name used, and `/tmp` and `/private/tmp` are different places.
- A later ASSIGN that lists fewer paths replaces the earlier list. Edits to files only the earlier ASSIGN named are then reported, with the later `assignSeq`.
- An entry form the rules above do not know, such as a note other than ` (new)`, owns only its literal name, so edits to the file it meant are reported.
- Nothing removes `owns.json` when an agent's work ends.

### Edit-before-assign report

Report-only. It records a queue builder's edit made before any assignment was cached for its run, and blocks nothing. It does not check for a saved Start, and it asks the hub nothing.

**The rule.** On PreToolUse of `Edit`, `Write`, `MultiEdit` or `NotebookEdit` of a file inside the session's repository, the hook appends one row, `reportRule` `edit-before-assign`, only when all three hold:

1. The agent's name, from `TAILTERM_AGENT_NAME`, has the queue team builder form: `builder-` followed by eight characters of `0-9a-f`, lower case, 16 bytes in all.
2. `inbox.json` exists for this exact run.
3. No `owns.json` exists for this run: the file is missing, or it is a readable cache written by another run.

With this run's `owns.json` present the rule is not looked at, and the owned-paths report applies exactly as before.

**The marker.** After a successful fetch of its agent's own messages, `tt inbox` writes `inbox.json` beside the agent's ledger:

| Key | Meaning |
| --- | --- |
| `v` | 1 |
| `run` | `TAILTERM_RUN` of the `tt inbox` that wrote the file |
| `time` | UTC RFC 3339 time of the write |

- It holds nothing else: no message text, number or count.
- The file is 0600 in the 0700 agent directory, written to a temporary file and renamed.
- It is written at most once per run. A later read of the same run finds it, writes nothing and, with no ASSIGN to cache, takes no lock. A new run of the same agent replaces it once.
- These forms write it: `--unread` (with or without `--mark-read` and `--wait`), the history page (no flags, `--before N`, `--after N`) and `--seq N`, as text or `--json`, whether or not any message came back. For `--seq N` the fetch that counts is the extra one-second request described under "Owned-paths report"; if that request fails or times out the message is still printed and no marker is written.
- These write none: `--board`, which pages the project Board and not the agent's own messages (it still caches an ASSIGN it shows, as before); a fetch that failed; a `tt inbox` with no agent identity, with an agent id that is not a safe directory name, or with no `TAILTERM_RUN`.
- It is written under the same hold of `owns.lock` as `owns.json`, after it. One read that has both files to write waits for the lock once, so the most `tt inbox` is delayed is still the single 200 ms bound. Without the lock neither file is written, and the next read writes them.
- `owns.json` is renamed first, and a cache that could not be written leaves no marker. The hook therefore never finds this run's marker while the ASSIGN fetched by the same read is still uncached.
- It never changes what `tt inbox` prints or returns.

**What the hook does.** In the owned-paths check, when `owns.json` could not be used or is another run's:

1. A session identified by the owner helper's file, a name that is not a builder's, or an empty run stops here, with no file touched.
2. If `owns.json` was not readable, one stat call: a file that is there and cannot be used (unreadable, malformed, oversized, another version, a directory) stops here. Only a missing file goes on.
3. It reads `inbox.json`, at most 512 bytes. Missing, unreadable, malformed, oversized, another version or another run's: it stops.
4. It reads `owns.json` once more. If it is now this run's, an ASSIGN arrived meanwhile, and it stops.
5. It resolves the file and the repository root as the owned-paths report does. A file outside the root is ignored.
6. It appends one row: `outcome` `would_report`, `reportRule` `edit-before-assign`, `reportPath`, and the call's `session`, `tool`, `toolUseId`, `argsDigest`, `task`, `agent`, `run` and `time`. There is no `assignSeq`.

`reportPath` is made as for the owned-paths report: relative to the repository root, through the secret scan, cut to 200 bytes without splitting a character. The call is not blocked, delayed past the hook's own wait, or changed, and it gets its ordinary row when it ends. Nothing is printed, no process is started and no hub request is made. One row is written per edit, so a builder that keeps editing before its ASSIGN writes one row each time; rotation at 4 MiB bounds the file.

Fail open: besides the stops above, nothing extra is written for a missing or relative `cwd`, or one that is not a string; no `.git` above `cwd`; a `tool_input` without its path string; a path scan that cannot finish in its time; a panic in the check; or a check that ends after the 150 ms wait. The append is the owned-paths report's, with the same deadline checks. None of these changes the call's ordinary row.

To list edit-before-assign rows by agent and path for this host:

```sh
awk 1 ~/.local/state/tailterm/tool-ledger/*/ledger.jsonl* 2>/dev/null | /usr/bin/jq -Rr 'fromjson? | select(.reportRule == "edit-before-assign") | "\(.agent)\t\(.reportPath)"' | sort | uniq -c
```

Each line is a count, an agent id and a path. The would-report command under "Owned-paths report" counts these rows too, by rule and agent.

Cost, measured on the Mini on 2026-10-10 by `TestToolScopeBeforeAssignCost`, by the method of `TestToolScopeCost`: about 0.12 ms for a builder's edit that gets the row (one stat call, the marker read, a second open of the missing cache and the append); under 0.01 ms for a builder with no marker, for an agent that is not a builder, and for a Bash call; about 0.06 ms for a builder's owned edit against this run's 256-entry cache, the same as before this rule. The test fails if the check adds 5 ms to the median or if a run with the check does not return, row written, before the 150 ms deadline.

Limits of the report:

- Edits made through Bash (`sed -i`, a redirection, a script, `git apply`) are not seen.
- Codex sessions are not covered.
- Roles other than a builder are not covered, and a builder is recognised by the name convention only. The hook does not ask the hub for the agent's role, so an agent of any role that carries a builder's name is treated as one.
- A builder named outside the convention is not covered: `builder2-facccbae`, a second builder on a team, gets no row.
- A builder that never ran `tt inbox` in its run has no marker, and nothing is reported for it. The same holds for one that only read `--board`.
- A recovery run is reported until it reads its ASSIGN again. `tt inbox --unread` writes the new run's marker but does not fetch an ASSIGN behind the read cursor the earlier run advanced, so `owns.json` stays the earlier run's and every edit is reported until a plain `tt inbox` or `tt inbox --seq N` shows the ASSIGN to the new run.
- An ASSIGN posted as plain text, or a typed one with no `owns` entry, is never cached, so every later edit of that run is reported.
- An ASSIGN whose paths could not be cached within the 200 ms lock wait leaves no marker from that read either; if an earlier read already left one, edits are reported until a later read caches the ASSIGN.
- Files outside the session's repository are ignored, as in the owned-paths report.
- The rule knows nothing of a saved Start, a queue admission or the work order. It reports only that no ASSIGN with owned paths had been cached for the run when the edit was attempted.
- Nothing removes `inbox.json` when an agent's work ends.

### Handler-gate report

Report-only. Work-item reads and writes belong to the project's database handler (AGENTS.md). On PreToolUse the would-deny check also matches the `command` of a `Bash` call, from the same parse, against three rules for going around the handler. Each rule that matches adds one row with `outcome` `would_report`, `reportRule`, `reportTag`, `reportSummary` and `agentName`, the call's `session`, `tool`, `toolUseId` and `argsDigest`, and the usual `task`, `agent`, `run` and `time`. `reportPath` and `assignSeq` are absent. A command that matches all three leaves three rows, and one that also matches a would-deny rule leaves that row as before; all of a call's rows go out in one write. Nothing is blocked, delayed past the hook's own wait, printed or changed, the call gets its ordinary row when it ends, and the would-deny and owned-paths rows are as they were.

A command is read as the would-deny report reads it (see "How a command is read" there): split into simple commands, quotes honoured, heredoc bodies and comments skipped, wrappers stepped over, the base name compared, and the arguments of `bash -c`, `eval` and any `ssh` read in turn. So `sudo /Users/x/.local/bin/tt work-items create …` and `ssh truenas 'sqlite3 …'` match, and `echo "tt work-items create"`, a commit message or a board post never does.

| `reportRule` | Fires when |
| --- | --- |
| `work-item-write` | the command is `tt` and its next words, by position, are a write: `work-items` with `create`, `update`, `dispatch`, `note`, `evidence`, `scope confirm`, `scope save`, `narrative artifact-put`, `narrative link-put`, `narrative coverage-put`, `narrative report-put`, `triage set` or `triage confirm`; `queue action`; `team queue` with `add`, `policy`, `limit`, `provision`, `scope`, `wait set`, `wait clear`, `rebind`, `requeue`, `fail`, `accept`, `integrated`, `replace-lead`, `remove`, `reorder`, `release` or `abandon`; `verification plan` or `verification receipt` |
| `hub-direct-write` | the command is `curl`, `wget`, `http`, `https` or `xh`, its own options make the request a write, and one of its URLs has the hub's authority (both in the same simple command) |
| `hub-db-direct` | the command is `sqlite3` and its database operand is `/mnt/deepfreeze/tailterm-hub` or a path under it. Reads count: the rule is direct access |

Reads never match `work-item-write`: `tt work-items list`, `get`, `receipt`, `revisions`, `messages`, `triage` with flags only, `scope get`, `scope receipt`, every `narrative` read (`overview`, `timeline`, `artifacts`, `artifact-versions`, `artifact-get`, `links`, `coverage`, `reports`, `report-get`, `receipt`); `tt queue list`, `get`, `history`, `changes`, `receipt`; `tt team queue list` and `sweep-worktrees`, `tt team launch`; `tt verification history` and `enrollment`; and every other `tt` command, `tt work-items` alone, or an unknown word in a subcommand position. `tt work-items evidence` is a hub read that writes a private local file; the order lists it as a write, so it matches, and its summary names it. `tt work-items triage set` and `triage confirm` do not exist in this `tt` and match only so a later command is covered.

What makes a request a write:

- curl: `-X` or `--request` with a value other than GET (`-XPOST` and `--request=POST` too), or any of `-d`, `--data`, `--data-raw`, `--data-binary`, `--data-urlencode`, `--json`, `-F`, `--form`, `-T`, `--upload-file`, `-I`, `--head`. HEAD counts as a write: only GET is a read. curl `-G` or `--get` with data is a GET, because the data becomes the query, and does not match unless a non-GET method is also given.
- wget: `--post-data`, `--post-file`, `--body-data`, `--body-file`, or `--method` other than GET.
- `http`, `https`, `xh`: a first word that is a method other than GET, or a request item after the URL that is a body field (its first separator is `=`, `:=` or `@`). A header (`Name:value`) and a query parameter (`name==value`) are not body fields.

What has the hub's authority: a URL is judged by its authority alone, which is what follows an optional `scheme://` and an optional `user:pass@`, up to the first `/`, `?` or `#`. It is the hub when its host and port are those of the `TAILTERM_HUB` environment value (a missing port is 80, or 443 for https), or when its port is 18765 on any host. A word that starts with the text `$TAILTERM_HUB` or `${TAILTERM_HUB}`, followed by the end of the word or by `/`, `?` or `#`, is the hub too. For curl and wget every word that is not an option or an option's value is a URL, and so is the value of curl `--url`; for `http`, `https` and `xh` only the first such word after the method is. The value of `-H`, `-d`, `-o`, `-u` and every other option that takes one is skipped by a fixed table and never read, so the hub's address inside a header, a body, a query, a path or a fragment is not a match, and `@file` is a word: no file is opened.

For `sqlite3` the operand is the first word that is not an option or the value of `-cmd`, `-init`, `-separator`, `-newline`, `-nullvalue`, `-vfs`, `-maxsize`, `-mmap`, `-lookaside`, `-pagecache` or `-heap`. A leading `file:` is dropped, with the empty or `localhost` authority of `file:///path` and `file://localhost/path`, and so is anything from the first `?`. A `file:` URI naming another host does not match. Later words are SQL text and are never judged.

These do not fire: `tt work-items list`, `tt send --subject 'tt work-items create'`, `curl $TAILTERM_HUB/api/tasks`, `curl -X GET http://h:18765/x`, `curl -G -d a=b http://h:18765/x`, `curl -X POST https://example.com/x`, `curl -X POST host:8765`, `curl -H 'X: http://h:18765' https://example.com -d x`, `curl -X POST 'https://example.com/x?next=http://h:18765/y'`, `curl -X POST https://example.com/p:18765/x`, `curl -X POST https://example.com/$TAILTERM_HUB`, `wget http://h:18765/x`, `sqlite3 /tmp/x.db`, `sqlite3 /mnt/deepfreeze/tailterm-hub-backup/hub.db`, `sqlite3 /tmp/x.db "ATTACH '/mnt/deepfreeze/tailterm-hub/hub.db' AS h"`, `sqlite3 -cmd '/mnt/deepfreeze/tailterm-hub/hub.db' /tmp/x.db`, `ls /mnt/deepfreeze/tailterm-hub`. A tool other than Bash never fires.

`reportTag` says who ran the command. It is recorded, and the row is written whatever it is; no tag drops a row. The first that applies:

| `reportTag` | Given when |
| --- | --- |
| `owner-helper` | the session has no agent identity and was ledgered as the owner helper from its helper file (see "Owner helper session"). It acts under the owner's delegation |
| `handler` | the agent's name starts with `db-handler` |
| `lead-allowed` | the agent's name starts with `lead-` and the match is `tt verification plan`, `tt verification receipt`, or the done save with queue acceptance: `tt work-items update` with `--status done` (or `--status=done`) and all of `--worktree`, `--branch` and `--commit` (owner helper answer #18182, AGENTS.md). A lead's other writes are `none` |
| `none` | everything else, an agent with no name included |

The name is `TAILTERM_AGENT_NAME`, or the helper file's `name`. A command holding both a write a lead may make and one it may not is tagged by the one it may not. `agentName` is that name cut to 128 bytes, and absent unless it is made only of letters, digits, `_`, `-` and `.`.

`reportSummary` is at most 120 bytes and is built only from fixed words and a count, as `denySummary` is, and passes the same secret scan:

- `work-item-write`: `tt` and the subcommand words matched; for the done save the option names `--status done --worktree --branch --commit` as well; then `(+N args)`, the number of words after them that are left out. Examples: `tt work-items scope confirm (+9 args)`, `tt team queue wait set (+6 args)`, `tt work-items update --status done --worktree --branch --commit (+4 args)`.
- `hub-direct-write`: the client's name; the method when it is POST, PUT, PATCH, DELETE, HEAD or OPTIONS and the word `method` for any other; `data` when a body option or body field is present; `hub` for the `TAILTERM_HUB` address or variable, or `port-18765`; then `(+N args)`, every word after the client's name but the matched URL. Example: `curl POST data port-18765 (+7 args)`.
- `hub-db-direct`: `sqlite3 hub-db (+N args)`.

An item id, a flag's value, a path, a URL, a host, a user name, a header, a body, a token and the SQL text never reach a row.

No credential is read. The gate opens no file: not `hub.json`, not a file named with `@`, not the database. It takes the hub address from the `TAILTERM_HUB` environment value alone, never from the configured one, and makes no hub request. One read is not the gate's and remains: every `tt` command, `tt hook tool` included, reads `~/.config/tailterm/hub.json` when it starts, before any hook handler runs, which is how the redaction token arrives (decision #32034). Nothing from that file reaches a gate row.

Fail open, as the would-deny report does and by the same path: nothing extra is written for input the hook cannot read, a `tool_input` with no `command` string, a command longer than 64 KiB, substitutions nested more than eight deep, a panic in the check, or a check that ends after the 150 ms wait. A panic inside the gate's own rules ends the gate for that call only: its would-deny rows are still written. The pending entry is written first, so none of these changes the call's ordinary row.

To count handler-gate rows by rule, tag and agent name for this host:

```sh
awk 1 ~/.local/state/tailterm/tool-ledger/*/ledger.jsonl* 2>/dev/null | /usr/bin/jq -Rr 'fromjson? | select(.reportTag != null) | "\(.reportRule)\t\(.reportTag)\t\(.agentName // "-")"' | sort | uniq -c
```

Each line is a count, a rule id, a tag and an agent name, or `-` for a row without one. Rows tagged `none` are the ones to look at; the others are the handler, a lead's allowed commands and the owner helper. It reads every agent's current and rotated file and skips a torn line, as the would-deny command does.

Cost, measured on the Mini on 2026-10-10 by `TestToolGateCost`, by the method of `TestToolDenyCost` with the gate switched on and off and the would-deny check on in both: under 0.01 ms for a short command that matches nothing and for a `tt` command that is no write; about 0.09 ms for one rule or all three (the extra append); under 0.01 ms when the call already writes would-deny rows, since the rows share one write; about 0.05 ms for a 64 KiB command that matches nothing and about 0.2 ms for one that matches all three. The test fails if the gate adds 5 ms to the median (25 ms under the race detector), or if more than 2 of 100 runs with the gate do not return, rows written, before the 150 ms deadline.

Limits of the report:

- It reads the command text only. A write made by a script file the agent runs (python, node, a shell script) is not seen, nor is one made through an MCP tool or the TailOS UI.
- Bash in Claude Code sessions only. Codex sessions are not covered.
- The handler is told by its name prefix, a convention and not a role. A handler named otherwise is reported with tag `none`, and any agent named `db-handler…` is tagged `handler`.
- `lead-allowed` is a name-and-form heuristic. The hook keeps no state and may not ask the hub, so it cannot check that the item or queue entry in the command is that lead's own: a lead running one of the three forms on another item is still tagged `lead-allowed`. A verifier's permitted `tt verification receipt` is tagged `none`.
- The hub is known by the `TAILTERM_HUB` value, the port 18765 and the two spellings of that one variable. A hub address held in another variable, or one on another port with `TAILTERM_HUB` unset, is not seen.
- curl `-K` and other config files are not read, so a method, body or URL given there is not seen. `--data-ascii` and `--form-string` are not in the list of body options.
- `tt` words are matched by position, as `tt` reads them today. A form `tt` does not accept, such as an option before the subcommand, does not match.
- `sqlite3` only, and only by its operand: an `ATTACH` of the hub database from another file, a relative path from inside the hub directory, `cp`, `scp` or `rsync` of the file, and other database clients are not seen. The remote command of an ssh to any host is read, so the rule fires whether or not the host is TrueNAS.
- A request to port 18765 on any host is reported, whatever serves it.
- The hub itself refuses only the backlog steward's work-item writes, so for every other role this report is the only record of a bypass, and it is a report: nothing is denied.

### Known limits

- A denied or abandoned call has a pre and no post. It appears as `unknown` only when a later pre evicts it.
- The digest is unsalted. It hides content, but a short guessable argument such as `ls` can be confirmed by hashing a guess.
- A reader must tolerate a torn last line if a hook is killed during its write.
- Nothing removes the directory of an agent that no longer exists. A retention rule is follow-up work.
- A helper closed on the hub is still ledgered while this host's helper file names the session's thread. Telling would need a hub request, which the hook never makes. The rows are for calls that session really made, under the agent and run the file names. Registering the helper again from another session ends it.
- Every Claude Code session on the host that is not a Tailterm agent now lists the relay state directory once per tool hook, to learn that it is not the helper.
- Hub upload, blocking and budget caps are not part of this version. Protected operations are reported, not denied (see "Would-deny report"). Edits outside an agent's owned paths are reported, not denied, with the limits listed under "Owned-paths report". A queue builder's edits before its ASSIGN are reported, not denied, with the limits listed under "Edit-before-assign report". Writes that go around the database handler are reported, not denied, with the limits listed under "Handler-gate report". The one rewrite is secret redaction, below.

The hook input field names (`hook_event_name`, `session_id`, `tool_name`, `tool_input`, `tool_use_id`, `tool_response`, `duration_ms`, `error`, `is_interrupt`) were read from the installed Claude Code 2.1.291 binary. A live `claude -p` run on 2026-10-06, with a made-up agent identity, an unreachable hub, a temporary ledger directory and the three hooks in a temporary project's `.claude/settings.json`, confirmed the ones a row is built from: a Bash `true` gave an `ok` row and a Bash `false` an `error` row, each with the session id, a `toolu_` tool-use id, a digest and a `claude` duration, and no pending entry was left. `is_interrupt` was not exercised.

### Activation

- The hooks reach the Mini when a release's Mini target runs `tt host setup --from ARTIFACT`. Host setup first renames the new `tt` into place, then merges the three events into `~/.claude/settings.json`. Nothing is active before that step. Any other host gets them only when host setup is run there.
- That file is the user-level settings, so every Claude Code session on the host runs `tt hook tool` on every tool call, the owner's own sessions included. Only sessions with a Tailterm agent identity write rows.
- Running sessions are ledgered from install; no restart is needed. Observed on 2026-10-06 with Claude Code 2.1.291 in `claude -p`, using a project-level `.claude/settings.json`: hooks added while a session's first tool call was running ran for that call's PostToolUse and for every later call. Hooks removed while a session ran stopped running for its later calls. The first row of a session ledgered mid-call has no pre, so its duration comes from Claude Code alone. An interactive session and the user-level file were not tried; expect the same.
- `tt doctor` still checks only the four earlier hooks.

### Rollback

`tt host setup --rollback` restores the previous `tt` and restarts the relay. It does not touch hooks: in a sandbox home the settings file was byte-identical before and after it, and `TestHostSetupRollback` asserts the same. A `tt` from before this feature has no `tool` hook. Checked with that binary: in a Tailterm agent session `tt hook tool` prints `tt: unknown hook "tool"` to stderr and exits 1; outside one it exits 0 silently. Claude Code treats that exit 1 as a non-blocking error (see below), so tool calls keep working, but nothing is recorded and each call runs a failing hook.

To roll back cleanly, remove the hooks first and then roll the binary back:

```sh
f="$HOME/.claude/settings.json"
cp -p "$f" "$f.before-tool-hook-removal"
/usr/bin/jq '
  def ledger: ["PreToolUse", "PostToolUse", "PostToolUseFailure"];
  if (.hooks | type) == "object" then
    .hooks |= with_entries(
      if (.key as $k | ledger | index($k)) and (.value | type) == "array" then
        .value |= (map(if (.hooks | type) == "array"
                       then .hooks |= map(select(((.command? // "") | tostring | test("(^|/)tt.? +hook +tool *$")) | not))
                       else . end)
                   | map(select((.hooks | type) != "array" or (.hooks | length) > 0)))
      else . end)
    | .hooks |= with_entries(select((.key as $k | ledger | index($k) | not) or (.value | length) > 0))
  else . end' "$f.before-tool-hook-removal" > "$f.tmp" && cat "$f.tmp" > "$f" && rm "$f.tmp"
grep -q 'hook tool' "$f" || echo "tool hooks removed"
tt host setup --rollback
```

The filter removes only hook entries whose command is a `tt` binary followed by `hook tool`, then the groups and events that leaves empty. Run against the sandbox settings host setup had written, it removed the three groups, kept the user's own `PreToolUse` group with its `Bash` matcher, the four other tt hooks, every other key and a 20-digit number unchanged, and a second run changed nothing. `cat` into the file keeps its mode and a symlink. Running sessions stop running the hook once the file changes (observed as above). Any later `tt host setup` from a version with the ledger adds the hooks again, so the rolled-back `tt` must be the one used for further setup until the release is retried. To remove the hooks without rolling back, run the same commands without the last line. The ledger files stay; delete `~/.local/state/tailterm/tool-ledger` to remove them.

### What Claude Code does when the hook misbehaves

Observed on 2026-10-06 with Claude Code 2.1.291 in `claude -p`, with a made-up agent identity, an unreachable hub and the hook under test in a temporary project's settings:

| Hook behaviour | What the session did |
| --- | --- |
| Exits 1 with text on stderr (the pre-feature `tt hook tool`) on all three events | Both tool calls ran and returned their normal results. The model saw no hook message. |
| Exits 2 on PreToolUse with text on stderr | The tool call was blocked and the model was shown the stderr text. `tt hook tool` cannot do this: its handler returns nothing and `tt` exits 1 for any error. |
| Does not return, entry has no `timeout` (a PreToolUse hook that sleeps 100 seconds) | Claude Code waited the full 100 seconds, then ran the tool call normally. The limit at which Claude Code gives up by itself was not observed. |
| Does not return, entry has `"timeout": 5` as installed (the same hook on all three events) | Claude Code abandoned the PreToolUse hook and ran the tool call 4.9 seconds after the hook started. The call returned its normal result. The PostToolUse hook was abandoned after 5 seconds in the same way. Neither hook process was left running, and the model saw no hook message. |

So a broken `tt hook tool` cannot deny or rewrite a call, and the only harm it can do is delay, by hanging. The handler's own work cannot hang past 150 ms. What remains is the time before the handler starts: process start and the config read named above. If that ever hangs, the 5 second timeout applies: a tool call is held at most 5 seconds before it runs and at most 5 seconds after, the call still proceeds, and that call gets no row or an incomplete one.

Why 5 seconds: the hook normally takes 6 to 12 ms and at most about 360 ms on the first run after an install, so 5 seconds never fires on a working hook, including on a loaded host, while a hang costs seconds and not minutes. A smaller value was not tested.

The same timeout was not given to the four older hooks, and it is not shown to be safe for them. `tt hook session-start`, `prompt`, `stop` and `notification` each make hub requests with client timeouts of 2 and 5 seconds, several in a row, so a slow hub can take them past 5 seconds while they are working correctly. Cutting off `tt hook stop` would also drop its decision to hold a turn open on unacknowledged work. A timeout for them needs its own measurement and is follow-up work.

Replacing the `tt` binary while hooks run, measured with the candidate binary outside Claude Code: host setup installs by rename, so a call in flight keeps the binary it started with and exits 0, and the next call runs the new file. Across 300 consecutive calls with the binary replaced twice, every call exited 0 with no output and wrote its row. The first call after each replacement took 216 and 228 ms, against a median of 7 ms: macOS checks a new binary file on its first run. So the 200 ms bound is passed once after each install. This was not tried inside a Claude Code session.

## Secret redaction

Feature `wi_79001641d97cd64c` revision 3, owner build order #28624 on the design accepted in #28619, builder assignment #28649. This source change is a candidate. It ships switched off.

When a tool call in a Tailterm Claude session returns output that holds a recognizable token or key, `tt hook tool` can count it (`report`) or replace it with a placeholder such as `[tt-redacted:github-token]` before the model receives it (`redact`). It is a step inside the ledger's existing `PostToolUse` hook: no hook entry is added, removed or changed, and `tt host setup` behaves as before. It is not a Claude Code mod.

**Do not set `redact` on any host until the owner has answered decision D7** (see "The editing risk"). `report` changes no output and is the mode meant for the first week.

### The host setting

One key in `~/.config/tailterm/relay.json`, the file that holds `claudeStallAction`:

```json
{"claudeSecretRedaction": "report"}
```

| Value | Effect |
| --- | --- |
| `"off"`, a missing file, a file that cannot be read, a file over 64 KiB, malformed JSON, a missing key, any other value | Nothing. The hook does not decode the tool's output, writes nothing to stdout, and writes the same ledger row as before this feature. |
| `"report"` | The output is scanned and the matches are counted in the ledger row. Nothing is printed and no output changes. Report mode never calls the rewrite. |
| `"redact"` | Matches in groups A and B are replaced and all matches counted. Not to be used before D7 is answered. |

The key must be spelled exactly `claudeSecretRedaction`; a key in another case is not it. The owner or the owner helper edits the file by hand, per host. There is no `tt` command for it, and neither `tt host setup` nor any release step creates or changes `relay.json`: a release leaves every host off, or at whatever the owner set. The file is read on each `PostToolUse`, so a change applies at the next tool call of every affected session, in both directions, with no restart. `TAILTERM_REDACT_SETTING_FILE` names another file; tests use it.

### Which sessions

The step runs only where the ledger writes a row: Claude sessions with a Tailterm agent identity, and the registered owner helper's Claude session. The owner's other Claude Code sessions on the host return before reading their input, as before, whatever the setting. Codex workers have no tool hook and are not covered. Only `PostToolUse`, a successful call, is scanned.

### What is recognized

A match lies inside one string value of the tool's structured output; object keys are not scanned. Group A is applied before group B, and overlapping matches become one replaced span named after the leftmost.

**Group A, values this host holds** (`host-credential`): the hub token `tt` loaded, and the values of exactly these environment variables, each only when at least 16 characters long: `TAILTERM_TOKEN`, `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`, `OPENAI_API_KEY`, `GITHUB_TOKEN`, `GH_TOKEN`, `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_API_KEY`, `DISCORD_TOKEN`, `DISCORD_BOT_TOKEN`, `TS_AUTHKEY`, `TS_API_KEY`. No other variable is read. The comparison is exact and in memory.

**Group B, token shapes.** Each must start where the previous character is not a letter, digit or underscore. "Exactly" means a longer run of the same characters is a different string and is left alone; "at least" means the whole run is replaced, however long, so no tail is left. A value one character short is not a match: a truncated secret passes.

| Name | Shape | Basis for the bounds |
| --- | --- | --- |
| `private-key` | a `BEGIN … PRIVATE KEY` header line (label of capitals, digits and spaces) through its matching `END` line; with no `END` line, everything to the end of that string value | the PEM format |
| `github-token` | `gh` + one of `p o u s r` + `_` + exactly 36 letters or digits; or `github_pat_` + exactly 82 letters, digits or underscores | prefixes published by GitHub; lengths are the ones in use, not published |
| `anthropic-key` | `sk-ant-` + at least 80 letters, digits, `_` or `-` | observed |
| `openai-key` | `sk-proj-` + at least 40 letters, digits, `_` or `-`; or `sk-` + at least 40 letters or digits, and for this bare form not directly after a `-` either | observed |
| `aws-access-key` | `AKIA` or `ASIA` + exactly 16 capitals or digits | prefixes published by AWS; length is the one in use |
| `slack-token` | `xox` + one of `a b p r s` + `-` + at least 10 letters, digits or `-` | `xoxb-` and `xoxp-` published by Slack; the others are older forms |
| `tailscale-key` | `tskey-` + `auth`, `api` or `client` + `-` + at least 20 letters, digits or `-` | observed |
| `stripe-key` | `sk_live_` or `rk_live_` + at least 24 letters or digits | prefixes published by Stripe |
| `npm-token` | `npm_` + exactly 36 letters or digits | prefix published by npm; length is the one in use |

The vendor pages were read on 2026-10-07. They publish prefixes; none of those read publishes a length. Where a length is "exactly", a vendor that lengthens its tokens makes that shape miss, not misfire. The unpublished lengths are accepted for the report-first release, where these shapes only count (owner helper decision #29099). They are not accepted for rewriting: before `redact` is switched on, the owner decides this together with D7, from the week's counts.

**Group C, counted and never rewritten:** `assigned-secret` (20 or more characters, at least 8 different ones among the first 64, after a name such as `api_key`, `secret`, `token` or `password` and `=` or `:`), `bearer` (the value after an `Authorization: Bearer` header), `jwt` (three dot-separated base64url parts whose first two decode as JSON objects; a candidate that starts inside the one before it is not looked at), `url-password` (the password in a `scheme://user:password@host` address), `aws-secret-key` (40 characters after the AWS secret key name), and two shapes with no vendor-published bounds, moved here from the design's group B: `google-api-key` (`AIza` + exactly 35) and `discord-token` (three dot-separated parts). A group C match that a group A or B match already covers is not counted twice.

Not attempted: bare high-entropy strings with no prefix and no name. They cannot be told from git hashes, digests and ids.

### Fails open

The hook can do two things on `PostToolUse`: print nothing, or print one JSON object carrying the rewritten output and a notice. The rewritten output is the original as Claude Code wrote it, byte for byte, with each match cut out of its string and the placeholder put in its place: escapes, spacing, key order and number forms are not re-encoded. It always exits 0 and never writes to stderr. There is one timer, the ledger's 150 ms: redaction has none of its own and does not lengthen it. The scan gives up 100 ms after the handler started.

| Event | The session gets | Ledger row |
| --- | --- | --- |
| The scan reaches 100 ms | the original output | `redactSkip: "timeout"` |
| `tool_response` is over 512 KiB | the original output, not scanned | `redactSkip: "oversize"` |
| `tool_response` is missing or not JSON | the original output | `redactSkip: "unreadable"` |
| The rewritten value would differ from the original in anything but string text | the original output | `redactSkip: "shape"` |
| The 150 ms runs out anywhere, a blocked stdout included | the original output; the process exits 0 | possibly none |

Fail-open means a secret passes whenever redaction is skipped. The answer is written before the row, because protecting the output matters more than recording it.

The scan is linear in the output: a run of token characters is measured once however many prefixes sit inside it, and a value that could be long is measured only as far as its bound. Every loop also checks the deadline, every 16 steps in a string over 4 KiB. Measured on the Mini at the 95th percentile, over ordinary source text, an output that is nothing but tokens, one of many small strings and 21 texts built to be slow (a refused prefix or a secret's name repeated to the cap), in both modes: over three runs on 2026-10-07 with the host busy (load average about 7), 100 KiB of source text adds 2.1 to 4.0 ms in report mode; the slowest 100 KiB output takes 4.2 to 6.2 ms; the slowest output of the 512 KiB cap takes 17 to 32 ms (tokens only, redact mode), under the 50 ms the cap is required to fit in. The cap was first 1 MiB; one worst case then took 60 ms on the busy host, so it was halved. The reviewer measured the same commit with the Mini at load 14 to 25 on 10 cores: five benchmark runs gave 18 to 33 ms at the cap in four and 142 ms in one, and a denser case, 511 KiB of empty strings, took 46 ms at the 95th percentile and 77 ms at worst. No idle-host measurement exists. The criterion is accepted as met under ordinary load (owner helper decision #29099). A scan that is slow stops at 100 ms, leaves a `timeout` row and passes the original output. The cap is to be set from the report week's `timeout` counts in a follow-up. `TT_REDACT_BENCH=1 go test -run TestToolRedactBenchmark ./cmd/tt` repeats it. Before the scan starts the ledger has already read and decoded its input; on a host where that alone takes most of the 150 ms, a call with an output near the cap can still leave no row.

### What is recorded

Only the existing ledger row gains fields, absent when the setting is off:

| Field | Content |
| --- | --- |
| `redactMode` | `report` or `redact` |
| `redactCount` | number of matches in this output, all groups; absent when the scan was skipped |
| `redactKinds` | match count per pattern name, for example `{"github-token": 1}`; absent when nothing matched |
| `redactSkip` | `timeout`, `oversize`, `shape` or `unreadable` |

Never written anywhere, by the hook, its tests or this document: the matched text, any part of it, its length, its position or a hash of it; the tool output or input; the hub token, environment values or vault content. No new file, hub request, log line or board message exists. The ledger's `argsDigest` is unchanged: a token typed into a command is inside that SHA-256.

In `redact` mode the answer also carries a notice to the model, as hook context: how many values were replaced, and that a placeholder must not be written into any file. It is advice, not a control.

### The editing risk (decision D7, unanswered)

In `redact` mode an agent that reads a file holding a secret sees the placeholder. An exact-match edit that includes the placeholder fails harmlessly. A whole-file write would save the placeholder over the real value. The notice asks the model not to; nothing enforces it. The owner has not yet accepted this risk, chosen to leave the Read tool's output unrewritten, or declined the mode. Until that answer is recorded, `redact` must not be switched on.

### What Claude Code does with the answer

Observed with Claude Code 2.1.293 on 2026-10-07 by the opt-in sandbox probe (`TT_LIVE_CLAUDE=1 go test -run TestSecretRedactionSandboxProbe ./cmd/tt`): disposable `claude -p` sessions in a temporary project, an environment built from nothing (`PATH`, `TERM`, the real `HOME` for Claude Code's own login, sandbox paths, a made-up identity, an unreachable hub, a generated token), and a value generated for each check. Checks 1, 2, 5, 7 and 8 ran the freshly built `tt hook tool`; 3, 4 and 6 ran a test helper that misbehaves.

| Check | Observed |
| --- | --- |
| 1. `redact`, Bash command that prints the value | The model received the placeholder in the tool result and repeated it; the value reached neither. The session transcript holds the placeholder only. No other file Claude Code wrote during the run holds the value. |
| 2. `report`, same command | Output unchanged; the row counted one `github-token`. The transcript holds the value, in the tool-result row and the reply. |
| 3. An answer of the wrong shape (a string for an object) | Claude Code used the original output. |
| 4. A hook that hangs | Claude Code used the original output 5.1 seconds after the call, the entry's timeout. |
| 5. `redact`, a Bash command that prints the value and exits non-zero | The value reached the model and the transcript. `PostToolUseFailure` cannot be rewritten. |
| 6. Half of a JSON answer | Claude Code used the original output. |
| 7. `redact`, the Read tool on a file holding the value | As check 1: placeholder in the tool result, the reply and the transcript. Read's output is an object with `file` and `type`; the value is in `file.content`. |
| 8. `report`, the Read tool | As check 2. |

On that evidence the owner helper resolved decision D5 in #28664: after a rewrite, the transcript holds the rewritten output.

Limits of these findings: four runs in all (two with a test helper doing the rewrite, two with the built `tt`), the haiku model, the Bash and Read tools only, one short single-line file. Other tools' output shapes were not tried. Files were searched only where Claude Code wrote during the run (its home folder, its JSON file, its cache and its temporary folder). **Network telemetry cannot be observed from the host and is treated as unprotected**: Claude Code may send the original output before the hook runs.

The probe touches the real home in two stated ways: Claude Code reads its own login, and writes the disposable sessions' transcript folder, which the probe deletes. The installed `tt hook tool` also runs in those sessions and, given the sandbox environment, wrote only under the sandbox. Before cleanup the probe searches the working tree for every generated value and requires none; after it, the sandbox and the transcript folder must be gone.

### Limits

- Output of a failed tool call is not covered (check 5). A command that prints a token and exits non-zero reaches the model unredacted.
- Tool input, the owner's prompts, pasted text, attachments and hook context are not covered.
- Codex workers, messages posted to the board or the hub, existing transcripts and ledger files, and anything exposed before the setting was switched on are not covered.
- A secret split across two string values, a truncated secret, and a secret with no recognizable prefix or name pass.
- Two tokens of an exact-length shape written with nothing between them are one longer run and pass.
- A private key header with no end line hides the rest of that string value, which is the intent for a cut-off key and a cost when the header is only being quoted.
- If another `PostToolUse` hook also rewrites output, Claude Code runs both on the original and the last answer wins. Today tt's is the only one.
- Report counts measure how often a pattern fires, not whether a hit was a real secret: the ledger stores no text. Judging a hit takes a person looking at that session.
- False positives are measured by a labelled corpus in the default test run: every group B shape is replaced in six contexts, and group B matches nothing in 18,000 generated look-alikes (git hashes, digests, UUIDs, base64 data, message numbers, item ids). The look-alikes come from a fixed seed, so the result is the same on every run. With random look-alikes the bare `sk-` shape matched dash-joined base64url text about once in five runs, which is why it now refuses a start after a `-`. What remains is base64url text that itself begins with `sk-` and 40 letters or digits, about one in a million such strings. Over the repository's tracked files it matches only four files that hold deliberate secret-shaped fixtures, pinned in the test by pattern and count (accepted in #29033): `client/main.js` (one `private-key`, a text box placeholder), `hub/cmd/tt/handoff_test.go` (one `private-key`), `hub/internal/bridge/helper_test.go` (two `aws-access-key`, one `github-token`, one `private-key`, two `slack-token`) and `hub/internal/jev/testdata/redact_corpus.json` (one `github-token`). Any other match, or a change in those counts, fails the test.

### Activation and rollback

A release changes nothing by itself: the new `tt` reads a setting that no host has. To start the report week on a host, the owner or owner helper adds `"claudeSecretRedaction": "report"` to that host's `relay.json`. To stop, remove the key or set `"off"`; it applies at the next tool call. Rolling `tt` back with `tt host setup --rollback` removes the feature with the binary; an older `tt` ignores the key.
