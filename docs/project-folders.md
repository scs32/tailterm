# Choosing a project folder

In New task, choose a team and its Main machine. Each machine used by members
without a fixed folder gets a visible Project folder field. Click Browse, navigate
folders on that remote machine, then choose Use this folder. An absolute path can
also be typed directly. A launch requires an explicit folder for every agent;
blank fields no longer silently start agents in the host's home directory.

For a single agent, Project folder appears directly after the machine selector.
Adding a team to an existing task uses the same per-machine folder selection.
Paths are not copied between machines when changing Main machine. Member-specific
folders are displayed as overrides in the launch dialog.

In Teams, Project folder override is a visible optional field. Leave it blank to
keep a team reusable across projects. Workspace permission presets can be saved
without a folder in the team; a real folder is required when launching. New
helpers inherit the parent agent's project folder unless given an explicit --cwd.

Browse is a read-only SFTP folder picker inside the existing dialog. It preserves
the task/team draft, closes its SFTP connection after listing, and does not enable
the Files workspace or upload/change files.

Selecting a folder is separate from trusting its contents. Codex can still show
its directory-trust prompt in the agent terminal when it has not trusted that
project. Tailterm reports that as Permission blocked. The picker does not accept
trust prompts or modify Codex's trust configuration.
