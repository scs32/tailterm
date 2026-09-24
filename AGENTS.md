# Development handoff

Read `docs/handoff.md` and `docs/project-overview.md` before changing this project.
The current work is on `tasks-hub`, not `main`.

Persistent owner instructions:

- Recovery prime directive (Bug `wi_0c7ab8fc3320b52b`, owner amendment and
  instruction-maintenance order #4335, original order #4328): never simply
  restart unexpectedly paused work. Before resuming, establish an evidence-backed
  explanation of why execution stopped and retain a structured incident linked
  to its bug: item/order/agent/run, last substantive action and timestamps,
  expected next action, stop reason, causal evidence, contributing conditions,
  and unresolved questions. An unknown cause requires diagnosis, not invented
  certainty. Record a concrete prevention action with an accountable owner,
  bounded work order and verification criterion before resuming. A "keep going"
  message, another watcher, or an unassigned ticket does not satisfy this rule.
  Distinguish immediate recovery from permanent correction and demonstrated
  prevention; keep prevention work open until verification and handler-saved
  acceptance. Link recurrences to the prior incident and explain why its control
  failed. Planned waits and owner-requested pauses retain their known reason and
  resume condition; investigate unexpected delay beyond that condition. Keep
  unrelated authorized work moving.
- All implementation, investigation, validation and deployment must originate
  from a durable bug or feature and a bounded work order. Intake and coordination
  may establish that record first. Cite its ID and work-order message in handoffs,
  results and release evidence; route scope changes through the database handler.
- Owner-directed work outside the harness (owner decision 2026-09-24): when the
  owner explicitly directs a single session, such as Claude Code, to build or
  review work outside the multi-agent harness, a committed work-order document
  in `docs/` may serve as the bounded work order. Record it in the hub as soon as
  practical: an owner-authored intake, a bug or feature citing it, and a
  work-order message naming the document. With no database handler running, that
  session may make those writes with the owner's credentials on the owner's
  explicit instruction. Do not dispatch it to an orchestrator, and state "no agent
  action is required" so recorded work is never started twice. Example: broker
  phase 1, `wi_de84224a37cbef70`, intake #8766, order #8767.
- All agent work-item database reads/writes (including list/get/create/update/
  dispatch) go through the project's actual database handler roster name. Do not
  bypass it with CLI/API/database access if unavailable; arrange authorized
  setup/resume or report the dependency. Human UI access remains available.
- The database handler verifies committed records, preserves source provenance,
  revision checks and retry identities, and records assignment/result links and
  acceptance evidence. Only report item completion after its saved confirmation.
  Keep the handler available while the project remains open; respect explicit
  owner retirement. AIV/MCP integration remains deferred.

- Develop/deploy this branch to `https://tailos.tailarr.com` (Cloudflare project
  `tailos`), plus the local preview when appropriate. Do not deploy it to
  `https://tailterm.tailarr.com`. `npm run deploy:static` targets the old site;
  use the explicit TailOS command in the handoff instead.
- Do not install, configure, restart or otherwise change Tailscale on TrueNAS.
  Its existing networking already works. Hub container updates use TrueNAS
  middleware and the existing TCP listener.
- Keep live task/profile data out of tests. Use isolated databases, browser
  contexts and tmux sockets. Do not print tokens, private keys or vault contents.
- Keep Files hidden until the owner decides to resume that work.
- Preserve task-owned groups, partial-launch retry behavior, exact run/thread
  identity, retirement versus closure, and durable cleanup receipts.
- Keep the UI compact and consistent: selection is fill, hover is outline,
  aligned control heights, muted placeholders, and no decorative empty filler.

These notes do not authorize shutting down production services or closing live
user tasks merely because a development session ends. Follow the owner's current
request, keep useful progress updates, and finish authorized work without repeated
permission questions.
