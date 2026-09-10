# Agent retirement and closeout

The built-in briefing tells the main orchestrator to accept worker results and
verification, resolve outstanding reviews/dependencies, then close completed
workers and helpers. Each implementation session is dedicated to one bug or
feature; a new item gets a fresh agent identity and context. The orchestrator and
active database handler stay available while the project remains open. Workers
post their handoff before closing when explicitly released.

- `tt retire NAME` retires a task member on any machine. With no name, retire yourself.
- `tt resume NAME` explicitly makes a retired member available again. Follow with a concrete assignment.
- `tt close NAME` records exact-run closure and safely terminates that worker's
  matching owned tmux session on the current host. With no name, close yourself
  only after the accepted handoff has been posted.
- All three accept `--task ID` before the name when operating outside an agent session.
- Task settings → Agents exposes Retire/Resume and any pending cleanup retry.
- A direct human message to a retired agent also resumes it when its current
  session is online. The message stays unread until the agent retrieves it; the
  existing Codex relay can then wake the same bound run/thread.

Retirement is intentional temporary retention for possible same-item follow-up.
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
session limit, because it keeps the process/session. Exact individual closeout
does release the open-agent slot while leaving the project and saved history
intact. Exited or closed agents need a fresh launch workflow rather than
retirement resume; item-bound work is never reassigned into the old context.

`tt close` first verifies the saved host and current run, records durable closure
intent, then reuses the cleanup receipt path. It never kills by stored session
name alone. A renamed exact session closes; a missing exact session can be
confirmed; reused names, replacement runs and mismatched tmux creation identities
remain open. Receipt delivery is retryable, and success is monotonic.

Before closeout, inventory useful long-lived services descended from the worker's
tmux session. Hand each continuing service to a durable owner or detach it from
the worker session, then reverify its process identity and readiness. An exact
tmux close correctly terminates all remaining descendants; it cannot distinguish
an intentionally retained preview from disposable worker subprocesses.

For important assignments, the orchestrator records the posted message sequence
and recipient, then checks `readUpTo`, status and availability at meaningful
checkpoints and before waiting, retirement or completion. A read cursor proves
retrieval, not completed work. If a required assignment is still unread after a
resume-then-retire race, or its recipient is retired, offline or idle, the
orchestrator makes one explicit recovery decision: resume and reference the
existing assignment when retirement should be reversed, transfer ownership to
another worker, or report a concrete blocker. It does not busy-poll,
duplicate-blast the assignment, override an intentional retirement or owner
instruction, or retire unfinished work merely for idleness.

Prompt guidance applies to new launches and `tt brief` after the host CLI update;
it does not rewrite prompts already loaded by running agents. The lifecycle state
and wake-up suppression apply immediately to existing agents when retired.
