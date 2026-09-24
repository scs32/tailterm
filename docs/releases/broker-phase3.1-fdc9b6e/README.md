# Broker phase 3.1 release: `fdc9b6e`

Acknowledgement discipline ([work order](../../broker-phase-3.1.md)), item
`wi_466f89ac2304a4a9`. Owner-directed release on September 24, 2026. It was built in the
owner's Claude Code session, an owner-approved exception recorded in the work order. **Both
review rounds ran on the board**, in the "Broker review" project (`tsk_1bcd0573791a2f6c`),
through Tailterm reviewer agents on the deployed phase-3 hub.

The Discord server, application and owner IDs are redacted from the plan and receipt copies
here. The unredacted files are in `.build/broker-phase3.1-release-fdc9b6e/`, and the pinned
receipt SHA-256 (`4f06a485…`) is of the unredacted receipt.

## Review trail
- **Candidate:** `fc36cf0`, against base `aa63c98`. The base already includes the merged bug A
  fix (`64e8654`, `aa63c98`), which ships in this hub release.
- **Round one:** REVIEW #9100 (fable-reviewer) and #9101 (codex-reviewer).
  - codex-reviewer first blocked (#9103) waiting for database-handler verification that a
    review-only project does not have; the owner cleared it (#9104).
  - **Codex (#9118):**
    - R1-C1: the gate read only 10 obligations.
    - R1-C2: the stop hook let a continued turn end with unacknowledged work.
    - R1-C3: latency was measured from creation, and the median of an even sample was wrong.
    - R1-C4: five spawn test failures, traced to the reviewer's inherited `TAILTERM_*`
      environment.
  - **Fable (#9121):**
    - B1: the templates and briefing still said "do not acknowledge".
    - B2: decision requests bypassed the gate.
    - Ten follow-ups.
  - All fixed together in `6235f27`, with follow-ups 1, 4–8 and 10 applied and 2, 3 and 9
    recorded as rules.
- **Round two:** REVIEW #9123 and #9124.
  - **Fable (#9138):** verified, mergeable. Its evidence: every new test fails when applied
    to `fc36cf0`.
  - **Codex (#9132):** one remaining item, R2-C3-Q. A queue-first acknowledgement stamps
    `delivered_at` at the ack time, so latency read zero.
- **Focused fix:** `fdc9b6e`. Its regression tests fail without the fix. The disposition was
  posted to both reviewers (#9142, #9143), with no third general review.

## Deploy
1. **Rollback safety:** 3.1 has no schema change, so the phase-3 hub (`59c0f08`) opens the
   database unchanged.
2. **Backup:** `before-broker-phase3.1-fdc9b6e.sqlite` (SHA-256 `0f281e8b…`). Integrity ok,
   zero foreign-key violations.
3. **Hub and bridge:** release `20260924-broker-phase3.1-fdc9b6e`, `deployed`/`success`. Both
   services are running, and capabilities still report reliable delivery `writesRetired`.
   - Hub SHA-256: `cea51265…`.
   - Bridge SHA-256: `cbd608b4…`.
4. **Mini `tt`:** `fdc9b6e`, SHA-256 `7ff0ce96…`, installed atomically. The rollback binary
   is `~/.local/bin/tt-before-broker-phase3.1-fdc9b6e`.
   - `tt hooks codex --install` added the Stop hook to `~/.codex/hooks.json`, which had no
     hooks before.
   - After the relay restart there were zero new log lines in 45 seconds. The
     `connection refused` lines just before it are from the hub restart.
5. **TailOS:** `fdc9b6e`, for the template wording. Built from clean source, all 82 assets
   verified.

## Production check (k11)
- **Setup:** a one-off project, `tsk_52a9b146e1172932` ("Broker phase 3.1 production check").
  Its placeholder agents were `lead-check`, `late-check` and `prompt-check`, on host
  `verification.invalid`.
- **Assignments:** the lead assigned #9144 to `late-check` and #9145 to `prompt-check`. Both
  created queued `ack_outcome` obligations. The check waited past the 2-minute grace period.
- **Post before acknowledging:** `late-check`'s notice was refused with `409 unacknowledged`:
  "you have unacknowledged work, so this write was refused: #9144 from lead-check (Production
  check assignment for late-check). Run `tt ack 9144` (for each), or reply to it with
  `tt send --reply-to 9144`, then retry."
- **After `tt ack 9144`:** the retry with the same request ID posted (#9146).
- **Reply first:** `prompt-check` replied to #9145 with a question (#9147). The reply was
  accepted and acknowledged #9145, and its next notice posted (#9148).
- **Plain Codex session:** `codex exec` on the Mini, outside Tailterm and with the hook
  installed, ran to completion with no trust prompt and no block.
- **Close:** the agents and the project were closed.

## Rollback
- **Hub and bridge:** point the app at `releases/20260924-broker-phase3-59c0f08`.
- **Mini:**
  - restore `~/.local/bin/tt-before-broker-phase3.1-fdc9b6e`;
  - remove the Stop entry from `~/.codex/hooks.json`;
  - run `launchctl kickstart -k gui/$UID/com.tailterm.inbox-relay`.

## Follow-ups
- A Codex agent's live turn-end block has not yet been seen on a real Codex team. Watch for it
  on the next shadow-week team, bug B.
- The Codex hook command is `tt hook stop` by name, so it relies on `tt` being on the agent's
  PATH.
- The installed-hook check expects a `tt` path without spaces (Fable, round two).
- Carried from phase 3:
  - pause service evidence on typed results (f7);
  - owner routes open to any holder of the workspace token;
  - digit-leading scoped names (bug A n1).
