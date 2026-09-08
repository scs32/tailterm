# Closing a task

Close task stops the task's agent tmux sessions, including retired agents and
helpers, and removes their terminal panes. Messages and task history remain on
the hub. Ordinary sessions added to a task group are not agent sessions and are
left running.

The hub records closure first. The browser then asks each saved SSH host to run
`tt cleanup`; the host's `tt relay` also retries independently of the browser.
An offline host cleans up when its relay reconnects. Closed tasks show the count
of sessions awaiting confirmation and offer **Retry cleanup**. Hosts need the
updated `tt` CLI and a running relay for automatic cleanup without a browser.

`tt` stores private ownership receipts in its relay state directory. Cleanup
matches the hub, task, agent, run, tmux session ID and creation time, and checks
identity again inside tmux before killing. Renaming an owned session does not
prevent cleanup. Reused names or unrelated sessions are never killed by name.
Failed receipts remain on disk and retry even if the session has already gone.

Older sessions are adopted by the relay. For old agent records whose sessions
have already disappeared, Retry cleanup confirms absence on the corresponding
saved host. An unavailable or unverifiable host remains pending rather than
claiming success.

Retire remains different: it disables automatic inbox wake-ups while keeping
the terminal available for inspection. Close task is the explicit end of the
whole task and terminates any running work in its agent sessions.

## Saved history

Use **View history** on a closed task or select it under **Closed tasks** on
Board. The board is read-only: closing a task never deletes its shared messages.
**Download history** exports JSON containing the objective, agent roster, all
retained messages and activity events. Long conversations initially show the
latest 200 messages; **Show full conversation** retrieves the rest. Downloads
always read all pages, independently of the preview.

Terminal scrollback and private conversations inside agent CLIs are not archived
by this feature. The export contains what agents posted to the shared board and
reported to the hub, rather than a full transcript of their terminal sessions.
