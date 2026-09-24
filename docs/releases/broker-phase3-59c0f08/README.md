# Broker phase 3 release: `59c0f08`

Feature `wi_6329a9f143468664`, owner intake #8960, work order #8961
([work order](../../broker-phase-3.md)). Owner-directed release on September 24, 2026,
under the out-of-harness exception in `AGENTS.md`. It was built in the owner's Claude Code
session and reviewed by Codex and Fable subagents. From phase 3.1 on, reviews run on the board.

The Discord server, application and owner IDs are redacted from the plan and receipt copies
here. The unredacted files are in `.build/broker-phase3-release-59c0f08/`, and the pinned
receipt SHA-256 (`da5e6f9b…`) is of the unredacted receipt.

## Review trail
- **Candidate:** `054c11d` (`abf6bb2`, plus a notice-wording fix and gofmt).
- **Round one:** Codex F1–F11 and Fable b1–b4, with overlaps (F3/b2, F5/b3, F6/b1), were fixed
  together with Fable's follow-ups f3–f6 and f9 in `56e79e6`.
  - The Codex review result sat unread in a job log for about 40 minutes before it was collected.
- **Round two:** both reviewers confirmed the round-one fixes. Three items remained:
  - F2: a reissued obligation lost its item links, so a cancellation after reassignment was invisible;
  - F8: the obligation lookup had no upper bound;
  - R1 (= Fable B-R2-1): an exited outgoing lead stopped handing off its role work.
- **Focused fix:** `59c0f08`. It was verified by regression tests that reproduce each reviewer's
  scenario, with no third general review.

## Changes from the work order (recorded in it)
- **Operational records:** retired alongside deliveries rather than re-anchored. They were
  delivery-bound, and production had none. As a result, the pause dialog's `transferred` and
  `detached` service dispositions are unavailable until their evidence is based on typed results.
- **`/pause` and `/unpause`:** dropped. Pause needs an explicit target list and a host-launched lead.
- **Capabilities:** keep their versions, with `writesRetired`, so legacy reads keep working.
- **Schedule-monitor notices:** keep their `system/schedule-monitor` provenance.

## Deploy
1. **Rollback check (c14):** the phase-2b hub `d6d5155` opened a database migrated by this release,
   read its obligations (including a `role:lead` one) and accepted posts.
2. **Backup:** `before-broker-phase3-59c0f08.sqlite` (SHA-256 `e55981b0…`). Integrity ok, zero
   foreign-key violations.
3. **Hub and bridge:** release `20260924-broker-phase3-59c0f08`. The app is RUNNING with both
   services, and capabilities report reliable delivery `writesRetired`.
4. **Close-out on start:**
   - 9 open directives and 5 pending lead dispositions were retired with provenance.
   - One notice in Tailterm Development (#9047) lists the four referenced work items.
   - This matches the dry run on a copy of the production backup.
5. **Mini `tt`:** `59c0f08`, SHA-256 `9343c523…`, installed atomically. The rollback binary is
   `~/.local/bin/tt-before-broker-phase3-59c0f08`. After the relay restart there were zero new
   log lines in 45 seconds.
6. **TailOS:** `59c0f08`, for the database-handler template change.
   - The first deploy went out after `verify:release` had failed, because an empty untracked
     `go.mod` in the repo root made the build "dirty".
   - The file was removed, and TailOS was rebuilt and redeployed from clean source.
   - All 82 assets were verified.

## Production check (c16)
- **Setup:** a one-off project, `tsk_e97b60f9a303281a` ("Broker phase 3 production check"), with
  placeholder `lead-check` and `worker-check` agents (host `verification.invalid`).
- **`role:lead`:** a request addressed to `role:lead` (#9048) was routed to `lead-check` with an
  `ack_outcome` obligation.
- **BLOCK as wait:** a BLOCK from the lead to the worker saying "wait" (#9050) created only a
  delivery obligation, so it could not escalate.
- **From the owner's Discord:**
  - `/extend` moved #9048's deadline one hour, with a board notice (#9051).
  - `/answer` posted a typed answer from the bridge to the asking worker, as a reply to #9049
    (#9052), and closed that obligation `answered` without obliging the asker.
  - `/cancel` closed #9048 `cancelled` with its reason and told the lead (#9053).
- **Close:** closing the project archived `#broker-phase-3-production-check`.

## Rollback
- **Hub:** point the app at `releases/20260924-broker-phase2b-d6d5155` (verified above).
  - The close-out's row changes stay; they are terminal states with provenance.
  - The legacy writers come back, but no delivery is open.
- **Mini:** restore `~/.local/bin/tt-before-broker-phase3-59c0f08`, then
  `launchctl kickstart -k gui/$UID/com.tailterm.inbox-relay`.

## Follow-ups
- Base pause service evidence for `transferred`/`detached` on the target run's typed result;
  until then, the pause dialog should explain why those options are unavailable (Fable f7).
- Owner routes are open to any holder of the workspace token, as are the existing lead and
  pause routes (Codex N1, Fable f2).
- n1 from bug A: digit-leading item-scoped names are not masked in broker subjects.
- **Next:** phase 3.1, acknowledgement discipline (`docs/broker-phase-3.1.md`).
