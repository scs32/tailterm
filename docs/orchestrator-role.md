# Orchestrator role boundary

Work item `wi_0edddf71905d6186`, bounded build order
`wi_0edddf71905d6186-build-1` (Board message #821), changes Tailterm's generated
agent instructions so a main orchestrator decides, plans, routes work and reviews
evidence. Builders own implementation, including shared schemas/types and
integration code. Lead accepted the clean build candidate and its evidence in
message #889; database-handler result recording is linked from message #882.

## Implemented behavior

The sole implementation exception requires both conditions:

1. The orchestrator is the only registered or planned non-database team member.
2. **Allow agents to add other agents** is off.

The launch command carries the complete planned ordinary-member count so the
first agent of a multi-member team is not mistaken for a solo agent before the
remaining members register. Partial retries recompute the union of saved roster
names and planned member names. `tt brief` regenerates the policy from the live
roster and current spawning setting. Database handlers are a system role rather
than an ordinary reusable-team builder and do not affect the sole-member count.

Retired, done, blocked, exited and closed ordinary member identities still count.
An expensive model, idle workers, unavailable capacity, cost, a zero or exhausted
helper quota, or a consumed helper allowance does not create an exception.
Non-orchestrators retain their assigned builder, reviewer, QA, operator or other
role; only builders receive implementation ownership. The shipped example teams
now route shared files and final integration to an explicit builder, and their
documented prompts exactly match the application templates.

This is an instruction-level role boundary. It is not a runtime sandbox or API
authorization control, and release evidence must not claim technical prevention
of orchestrator edits. The exact guarantee is that launch-time and refreshed
briefings state the boundary using the available planned/live membership data.

## Candidate and verification

Clean candidate: `561a0db32d70bf7371db539a3f66506e89f344a5` on
`fix/orchestrator-role-0edddf71`, based on
`4de29d028842a24b391f3e9dc6086e398c10ea7f`.

Changed areas are the generated CLI briefing, the browser-to-CLI planned-member
launch argument, example role templates, focused tests, and this documentation.
No task, work-item or profile schema changed. The duplicate
`TAILTERM_BRIEFING` tmux environment copy was removed to keep long briefings below
tmux's command limit; the identical briefing remains the runtime command argument,
handler retry fingerprints remain based on stable launch inputs, and `tt brief`
remains the refresh path.

Verification on the clean candidate before this report:

- `npm test`: 123/123 passed.
- `npm run test:hub`: `go vet ./...` and `go test ./...` passed for every package.
- `node tests/task-form-browser.mjs`: Chromium and WebKit passed the isolated
  real-hub/private-tmux matrix, including creation, planned team count, handler
  insertion, partial-launch retry without duplicates, permissions,
  retirement/resume, closure and cleanup.
- `git diff --check`: passed.

All fixtures used isolated databases, browser contexts and tmux sockets. No live
work-item/profile data, deployment, installation, network change, service stop or
session teardown was used for build acceptance.

## Release plan

Release requires a separate recorded order and the lead's explicit release of the
shared frontend integration slot. Integrate this commit into the current
`tasks-hub` release candidate only after the active `board-enter` work lands.
Resolve only concrete conflicts in the changed files, inspect the final changed
bytes, and rerun checks implicated by those conflicts rather than inventing a new
broad gate.

The new frontend emits the internal `--planned-team-members` argument. Therefore
the matching `tt` binary must be installed on both Mini and Air before publishing
that frontend. The reverse order is not compatible; the old frontend is compatible
with the new CLI.

1. From the clean integrated commit, run `npm run build:tt` and record the tested
   Darwin arm64 binary SHA-256. Preserve each host's existing `tt` as a uniquely
   named rollback binary, install the matching build atomically on Mini and Air,
   and restart only that user's Tailterm inbox relay.
2. On both hosts, verify the installed checksum, `tt spawn --help` contains
   `--planned-team-members`, relay status is healthy, and an authorized
   task-aware `tt brief` shows the expected orchestrator/worker boundary for its
   current roster. Do not print hub tokens or private configuration.
3. Build/package the static application from that same clean integrated commit
   with `npm run build:static` and `npm run verify:release`. Record the source
   commit, manifest hash and asset inventory.
4. Publish only to Cloudflare Pages project `tailos` using the explicit TailOS
   command in `docs/handoff.md`, then update the Mini local preview through the
   existing Apple webpage procedure. Do not deploy to the old `tailterm` project
   or recreate Air's retired webpage preview.
5. Verify `/release.json`, every served asset, production Chromium startup and an
   isolated synthetic vault restore on TailOS and Mini. Record URLs, hashes,
   installed CLI hashes and the instruction-only limitation with the handler.

No hub binary, database migration, TrueNAS networking or Tailscale change is
needed for this release.

## Rollback

Retain the immediately preceding clean static package, Pages deployment identity,
Mini image and both host CLI backups before activation. If the new frontend fails,
restore the prior TailOS deployment and Mini image; the new CLI may remain because
old frontend/new CLI is compatible. If the CLI itself must be rolled back, restore
the frontend first, then atomically restore both previous CLI binaries and restart
only their per-user relays. Never leave the new frontend paired with an old CLI.
The hub and database have no change to roll back.
