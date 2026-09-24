# Broker phase 2b release: `d6d5155`

Feature `wi_5a203621825434d1`, owner intake #8796, work order #8799
([work order](../../broker-phase-2b.md)). Owner-directed release on September 24,
2026, under the out-of-harness exception in `AGENTS.md`.

The Discord server, application and owner IDs are redacted from the plan and
receipt copies here. They are in the deployed app configuration, and the unredacted
files are in `.build/broker-phase2b-release-d6d5155/`. The pinned receipt SHA-256
(`03608844…`) is of the unredacted receipt.

## Review trail
- **Round one:**
  - Codex D1–D14 and Fable b1–b4 were consolidated as C1–C14 and fixed in `2dfbde7`.
  - Fable's b4 (deploy ownership) did not apply, because the TrueNAS SSH principal is UID 950.
- **Round two:**
  - Both reviewers confirmed C1, C2 and C5–C14.
  - Codex found R1–R4: a backfill cursor could pass an unrecorded message, a replayed nudge
    could fire twice, first-build state lacked new columns, and a stale card window could block
    a replacement card.
  - Fable found the same stale card window, plus a crash window in which a channel's ingest
    cursor could be left empty.
- **Focused fix:** `425ad5e` (R1–R4, with nudge request IDs on the hub) and `4f1fbfd` (the ingest
  cursor is written with the mapping). Each is covered by a regression test.

## Deploys
1. **`4f1fbfd`:** release `20260924-broker-phase2b-4f1fbfd`.
   - **Backup:** `before-broker-phase2b-4f1fbfd.sqlite` (SHA-256 `d342c86a…`), integrity ok, zero
     foreign-key violations.
   - **App:** RUNNING with two services, `hub` (Jev key kept, bridge token added) and
     `discord-bridge` (no published ports; reaches `http://hub:18765`).
   - **Bridge:** created "Tailterm projects" with one channel per open project and registered
     `/status`, `/stalled`, `/nudge`, `/say` and `/reassign`.
2. **Incident, and `d6d5155`:**
   - **Symptom:** during the live check, some button clicks answered "This channel isn't linked
     to a Tailterm project", or failed with 10062 Unknown interaction.
   - **Cause:** a local smoke-test bridge on the Mini had survived cleanup and was connected
     with the same bot token. Every Gateway session receives every interaction, so the two
     bridges raced to answer each click. It was stopped by PID and verified gone.
   - **Fix:** before finding that cause, `d6d5155` scoped interaction reply limits to their own
     interaction. That change is correct but was not the cause.
   - **Redeploy:** as release `20260924-broker-phase2b-d6d5155`, after another verified backup,
     `before-broker-phase2b-d6d5155.sqlite` (SHA-256 `1bd91deb…`).
3. **Rollback check (d16):** the phase-2a hub `5610db7` starts on a database migrated by this
   release, reads a bridged message and accepts posts.
4. **Owner setup change:** the bot was re-invited with Pin Messages (permissions
   `2252126231284816`), which pinning the status card needs. Pinning failed with 50013 until then.

## Production check (d15)
- **Setup:** a one-off project, `tsk_14ff7076e6724f7f` ("Broker phase 2b production check").
  Its only agent was a placeholder (host `verification.invalid`) and it had no lead. An owner
  message to the placeholder (#8896) created an `ack_outcome` obligation.
- **Escalation:** the acknowledgement deadline passed at 12:35:15 PDT. The owner escalation
  (#8898) reached Discord at 12:35:40:
  - it mentioned only the owner, with no `@everyone` ping;
  - it had Nudge, Reassign… and Open in TailOS buttons.
- **Owner controls, used from Discord:**
  - A typed message was posted as #8897 and got ✅.
  - Nudge answered "Nudged".
  - A second Nudge within 2 minutes was refused with the time remaining.
  - Reassign… reported no other running agent.
  - `/stalled` listed #8896.
  - `/say` posted #8899 publicly.
  - Both Discord messages are on the board from node `discord-bridge`.
- **Status cards:** pinned in all three channels after the re-invite.
- **Close:** closing the project moved its channel to "Tailterm archive" with the closing notice.

## Rollback
- **Bridge only:** redeploy with a plan without the Discord fields. The `discord-bridge` service
  is removed, and the hub ignores the absent bridge token.
- **Hub:** point the app at `releases/20260924-broker-phase2a-5610db7`. The phase-2b migration
  only adds tables and a column, and the rollback check above covers it.

## Follow-ups
- Agents use BLOCK to mean "please wait", which creates obligations. #8851 in the shadow week
  escalated to the owner for nothing. This needs a non-obligating wait form (phase 3).
- Escalation subjects fall back to a generic subject when an agent name carries an item suffix
  such as `builder-41b1c632`. The subject validator reads the suffix as a hash.
- `/extend`, `/answer`, `/cancel`, `/resume` and `/pause` are phase 3.
