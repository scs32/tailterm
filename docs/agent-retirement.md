# Agent retirement

The built-in briefing tells the main orchestrator to accept worker results and
verification, resolve outstanding reviews/dependencies, then retire workers that
are no longer needed. At task completion it posts the integrated result, retires
remaining workers (including helpers), checks the roster, and stays available for
the owner. It does not close the task or destroy terminals as routine cleanup.
Workers hand off results before retiring when explicitly released.

- `tt retire NAME` retires a task member on any machine. With no name, retire yourself.
- `tt resume NAME` explicitly makes a retired member available again. Follow with a concrete assignment.
- Both accept `--task ID` before the name when operating outside an agent session.
- Task settings → Agents exposes the same Retire/Resume controls.
- A direct human message to a retired agent also resumes it when its current
  session is online. The message stays unread until the agent retrieves it; the
  existing Codex relay can then wake the same bound run/thread.

Retired agents stay in their terminal group with their results, tmux session and
mailbox intact. The Codex inbox relay stops issuing new wake-ups for them.
Normal running/completion/permission hooks cannot remove retirement. An explicit
Resume or an accepted direct human message to an online retired agent makes it
available again. Human announcements and agent-authored messages leave retirement
intact, including swarm broadcasts. Retired agents cannot spawn additional helpers.

Retirement does not terminate the model process or interrupt an active or already
queued turn. It is not a spend cap. The orchestrator should retire workers after
accepted handoffs, not abandon active assignments. Other runtimes already lack
Tailterm's native Codex wake-up integration; their own external schedulers are
outside this retirement mechanism.

Unread messages remain available and may trigger a wake-up after resumption.
An online session means the hub has a current heartbeat (within its existing
90-second window); it is not a guarantee that a runtime can answer immediately.
Messages to offline retired agents remain stored without resuming them. No
message starts a new process, changes the run/thread binding, or recreates a
closed/exited session. An invalid message does not resume an agent.
Retirement does not refund the lifetime additional-helper allowance or the open
session limit, because it keeps the process/session. Actual session termination
remains a separate explicit action. Exited or closed agents need the appropriate
launch workflow rather than retirement resume.

Prompt guidance applies to new launches and `tt brief` after the host CLI update;
it does not rewrite prompts already loaded by running agents. The lifecycle state
and wake-up suppression apply immediately to existing agents when retired.
