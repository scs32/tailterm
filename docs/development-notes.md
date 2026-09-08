# Development notes

For the current September 8 handoff, read [handoff](handoff.md) and
[project overview](project-overview.md). They supersede historical deployment
and behavior descriptions below.

## September 8, 2026 — session closeout

The final application release is `56ded41`, deployed to TailOS and localhost.
Closed-task cleanup is durable across host disconnects, with run-scoped receipts;
retirement continues to preserve sessions. Closed tasks now expose read-only
conversations and JSON history exports. Empty-state filler and the 01 badge are
removed. All app changes are committed and tested; the handoff adds documentation
without redeploying. A portable Git bundle carries the local `tasks-hub` branch to
the next computer. Live services and the owner's open task remain running.

## September 7, 2026 — profiles, teams and groups

Implemented behavior is documented in [Shared profiles](profile-sync.md) and
[Tasks and teams](tasks.md). Team templates supersede single-agent saved setups;
a team of one preserves that workflow. Tasks retain separate terminal groups,
with movable ordinary sessions as visual guests. Closed task rows use the same
compact width and typography as the rest of the task list.

Validation uses isolated hub databases and browser contexts, with no test
records added to the deployed hub. At that point deployment used the local Apple webpage
and existing TrueNAS coordination container. The current frontend also runs on
Cloudflare Pages at TailOS.
