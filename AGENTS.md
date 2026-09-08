# Development handoff

Read `docs/handoff.md` and `docs/project-overview.md` before changing this project.
The current work is on `tasks-hub`, not `main`.

Persistent owner instructions:

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
