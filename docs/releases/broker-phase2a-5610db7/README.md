# Broker phase 2a release: `5610db7`

Feature `wi_07c6b8b7201ae27c`, owner intake #8771, work order #8772
([work order](../../broker-phase-2a.md)). Owner-directed release on September 24,
2026, under the out-of-harness exception in `AGENTS.md`.

## Review trail
- **Round one:** Codex F1–F14 and Fable f1–f13 consolidated as B1–B17, fixed in `74c7c35`.
- **Round two:** both reviewers chose a focused fix (N1–N3 plus B2, B3, B13–B15, B17), made in `d989b1d`.
- **Focused verification (Fable):** all items verified. It found R1 (a stable ack ID trapped blocked
  work), fixed in `1af162d`. R2, where directed owner free text can wake twice, is a logged
  follow-up; it never drops a wake.

## Deploy, incident and fix
1. **First hub deploy:** hub `1af162d` deployed as release `20260924-broker-phase2a-1af162d`,
   with a verified backup (integrity ok, zero foreign-key violations) and the Jev key mount kept.
2. **Incident:** installing `tt` `1af162d` on the Mini and restarting `com.tailterm.inbox-relay`
   exposed a problem the single-binding tests missed. The relay leased broker wakes (a write)
   for every saved binding every 3-second pass. Dozens of those bindings are stale, from the
   paused project, and this saturated the node's write limiter: 439 `429 rate limited` log lines.
   The relay was rolled back to the phase-1 `tt` (`tt-before-broker-phase2a-1af162d`)
   within minutes. After that, no new 429 lines appeared and the hub stayed healthy.
3. **Fix:**
   - `a477a61`: the relay leases only for live bindings (active project; current, online and
     not retired run), checks each binding at most every 10 s, and backs off a minute on 429.
     The hub answers a nothing-due lease without its write lock.
   - `5610db7`: a follow-up so the run fence runs before that precheck. `a477a61` had been
     pushed with `TestWakeJobLeasing` failing; it was caught on the next full run.
4. **Redeploy:** the hub was rebuilt from clean `5610db7` (SHA-256 `5aafce9b…`, hash on TrueNAS
   matches) and deployed as release `20260924-broker-phase2a-5610db7` after another verified
   backup. It is RUNNING with the key mount.
5. **Mini `tt`:** `5610db7`, SHA-256 `c5ee52d2…`, installed atomically. Rollback binaries:
   `tt-before-broker-phase2a-1af162d` (phase 1).
6. **Relay:** restarted, with no new 429 lines and zero relay log lines over the next 60 s.

## Production check
- **Setup:** a one-off project, `tsk_9e78fdb5737da261` ("Broker phase 2a production check"),
  with placeholder lead and builder agents (host `verification.invalid`, never woken).
- **Result:** an assignment created an `ack_outcome` obligation. The builder's current run
  acknowledged it; the lead's ack attempt was refused. A typed result from the builder's run
  closed it with outcome `result`. Both typed posts got shadow checks.
- **Cleanup:** the project and agents were closed right after, with history kept.

## Rollback
- **Hub:** point the app at `releases/20260924-broker-phase1-jev-06c915a`. The phase-2a
  migration only adds tables, and a phase-1 build opening a phase-2a database was verified
  (acceptance b13).
- **Mini:** restore `~/.local/bin/tt-before-broker-phase2a-1af162d` and
  `launchctl kickstart -k gui/$UID/com.tailterm.inbox-relay`.
