# September 7, 2026 — profiles, teams and groups

Implemented behavior is documented in [Shared profiles](profile-sync.md) and
[Tasks and teams](tasks.md). Team templates supersede single-agent saved setups;
a team of one preserves that workflow. Tasks retain separate terminal groups,
with movable ordinary sessions as visual guests. Closed task rows use the same
compact width and typography as the rest of the task list.

Validation uses isolated hub databases and browser contexts, with no test
records added to the deployed hub. Deployment remains the local Apple webpage
and the existing TrueNAS coordination container.
