# Development handoff

Read `docs/handoff.md` and `docs/project-overview.md` before changing this project.
The current work is on `tasks-hub`, not `main`.

Persistent owner instructions:

- All implementation, investigation, validation and deployment must originate
  from a durable bug or feature and a bounded work order. Intake and coordination
  may establish that record first. Cite its ID and work-order message in handoffs,
  results and release evidence; route scope changes through the database handler.
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
