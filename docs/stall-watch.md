# Stall watch

Feature `wi_fa5f10477f93c57d` revision 2, owner order #32210, builder assignment #32236.
Follow-up feature `wi_830e10326defc8ee`, owner order #32312, builder assignment #32351: the helper states that send a team notice to the owner, and the hold after a hub start.

On 2026-10-10 the owner helper went quiet and the first owner ping came 4 h 20 min later, because the helper is itself the escalation point for team stalls and nothing watched it. A team that goes quiet as a whole was noticed only through obligation timers, measured in hours. The hub now watches both.

The stall watch **alerts and does nothing else**. An alert starts the diagnosis. Under the recovery prime directive, the record of why work stopped, the incident and the prevention stay with the owner helper or the owner. The hub writes no incident draft (decision D3).

## The two watches

| Watch | Fires when | Threshold (D1) | Who hears it |
| --- | --- | --- | --- |
| Helper watch | the owner helper owes the owner something and shows no post and no `working` report | 20 minutes | the owner, as a Discord ping |
| Team watch | the team of a running queue entry shows no post, no `working` report and no matrix wait | 30 minutes | the owner helper, as a directed notice; the owner instead when the helper is quiet too, offline, not registered, crashed, stuck or blocked by its provider |

- Each watch posts **one notice per episode**. An episode is one stretch of quiet for one subject: the helper, or one queue entry.
- An episode ends silently. No notice says that the subject is back.
- A later episode needs a fresh 20 or 30 minutes. Whatever ended the earlier one restarts the quiet time: a post, a working report, or the end of a never-quiet case (see [Never quiet](#never-quiet)).
- The thresholds are fixed. They are not settings.

### Nights (D2)

From 22:00 to 07:00 Pacific no owner ping is sent. This covers every owner notice of this feature, from either watch.

- At night the notice is a plain channel line with no mention. Its last sentence says that the owner is pinged at 07:00 Pacific if the episode is still open.
- On the first sweep at or after 07:00, an episode that is still open gets exactly one owner ping. A hub that was down at 07:00 sends it on its first daytime sweep, or, for a helper or team held after the hub start, on the first sweep on which it is no longer held (see [After a hub start](#after-a-hub-start)). A team ping says why the owner hears it, as the night line did.
- An episode that ended in the night never pings. Neither does one whose subject is in a never-quiet case at 07:00: closing is decided before pinging.
- A team notice directed to the helper is sent at any hour.
- The zone is `America/Los_Angeles`, so the rule follows daylight saving time.

## Definitions

Only projects that are open and not paused are swept.

**The helper.** The project's agent with role `owner_helper` whose status is not closed, exited or retired. There is at most one. A retired helper is the owner's choice and is never watched.

**Effective activity state.** The state a notice names, and the state the working test uses:

- `offline` when the agent has not heartbeated for 90 seconds, whatever its stored state says;
- otherwise the stored activity state of the agent's current run;
- `unknown` when the run has reported no activity. A notice names `needs_input` instead when the roster status says so.

**Last working time.** One row per run in `stall_watch_progress`, written inside the activity report that the relay already sends:

- a report that enters `working` stores its observed time;
- a report that leaves `working` stores its observed time;
- any other report writes nothing. That includes a repeated `working` report, a wake outcome and a changed reason.

The same row marks a reported matrix wait, and the first report without the wait stores when it ended.

The transcript time and the report time are not used. The first moves on every transcript record and not on worktree-only work; the second moves on wake-only reports. For an offline agent whose stored state is still `working`, the last working time is its last heartbeat.

**Last post.** The newest message posted by the exact run. A post by an earlier run of the same agent does not count.

**Owes the owner.** The helper holds at least one open obligation, other than delivery-only, whose message came from a person: a directed message or a typed request. Hub notices and teammates' requests do not count.

**Helper quiet since.** The latest of: the oldest owed item, the run's last post, its last working time, the registration of its current run, and the end of its last never-quiet case.

**Helper quiet.** The helper owes the owner, no never-quiet case holds, and 20 minutes have passed since "helper quiet since".

**Running entry.** A team queue entry in state `running` that is not released. Its admission time is `admitted_at`, or its creation time when that is empty.

**Team.** The live runs bound to the entry's item at or after its admission, with no project role and a status that is not closed, exited or retired. A run bound before this admission belongs to an earlier team: its posts and working reports neither count as progress nor end an episode. A team with no member is left to the queue's `nothing-running` stall ([project-queue.md](project-queue.md)).

**Team quiet since.** The latest of: the admission, the newest exact-run post by a member, each member's last working time, the newest member binding, and the end of the team's last never-quiet case.

**Team quiet.** No never-quiet case holds and 30 minutes have passed since "team quiet since".

## Never quiet

No episode opens, an open episode ends with no notice, and a deferred 07:00 ping is not sent, in any of these cases.

For a team:

| Case | What the hub reads |
| --- | --- |
| owner pause | the project is paused or closed |
| shared-path wait | the entry has a wait in state `waiting`, `met` or `orphaned` |
| held entry | the entry has a token budget hold in state `held` |
| open owner decision | a member's run has an unanswered `tt ask` |
| working | a member's effective state is `working` |
| matrix wait | a member reports a wait for, or a run on, the matrix host, started less than 4 h 30 min ago |
| entry not running | the entry is queued, launching, finished, failed or released |

For the helper:

- the project is paused or closed;
- its effective state is `working`;
- it reports a matrix wait, as above;
- its current run has an unanswered `tt ask`: the owner already holds the next step.

A helper in `unknown`, or with status `needs_input` and no stored decision request, is **not** exempt. It is pinged and the state is named.

### When a never-quiet case ends

Time spent in a never-quiet case is not quiet time. When the case ends, the quiet time starts afresh:

- no notice is posted at the moment the case ends;
- none is posted until a full 20 or 30 minutes after it;
- an episode that the case closed does not post again when the case ends. A new episode needs the full threshold after that.

The hub reads the end of each case from rows that already exist:

| Case | The end is |
| --- | --- |
| open owner decision | the time of the owner's answer to a request from that run |
| held entry | the time the hold was continued or stopped |
| shared-path wait | the time the wait was closed (resumed, overdue or cleared) |
| matrix wait | the first activity report without the wait, or 4 h 30 min after its start when the relay stopped reporting |
| owner pause | the project's last resume |
| working | the last working time |

These times are compared to the second.

## After a hub start

While the hub is down it stores no heartbeat. On its first sweeps every agent therefore looks offline, and a member that was working looks quiet since its last heartbeat. So the hub process records when it started, and for 60 seconds after that an agent it has not heard from since is **unheard** and is not judged:

- no helper episode opens for an unheard helper, and its 07:00 ping is not sent;
- no team episode opens, and no team 07:00 ping is sent, while the helper or a member of that team is unheard. The helper counts because an unheard helper would read as offline and send the team notice to the owner;
- an agent that has heartbeated since the start is judged at once, on its stored state and its old quiet time;
- after 60 seconds an agent that is still silent is judged as usual, on its last heartbeat and its old quiet time. It is offline once that heartbeat is 90 seconds old, so after a short restart it can read as online, on its stored state, for up to 30 seconds more.

The hold applies to a helper or a team, not to a whole sweep. A sweep inside the 60 seconds still opens an episode, and posts its notice, for a helper or team whose agents have all reported. Closing is not held either: for a held helper or team the sweep still ends an open episode and moves its message cursor; it only opens no episode and sends no 07:00 ping for it.

The hold is 60 seconds, two heartbeat intervals. Agents heartbeat every 30 seconds and the watch sweeps every 30 seconds, so a hold of one interval would let the second sweep race the first heartbeat.

## When an episode ends

A helper episode ends when:

- the exact run posts after the episode opened;
- its last working time is after the episode opened, or it is `working`;
- nothing is owed any more;
- a never-quiet case holds;
- the helper's run was replaced, or the helper was closed, exited or retired.

A team episode ends when a member posts after it opened, a member's last working time is after it opened, a never-quiet case holds, or no member is left.

An ending post is found from the episode's own message cursor, not from a time window. A post made while the hub was down for hours therefore still ends the episode.

## Notices

| Notice | To | Refs | Discord |
| --- | --- | --- | --- |
| Helper quiet, by day, or the 07:00 ping | Board | `stallWatch=helper`, `episode`, `agent`, `run`, `activity`, `recipientKind=owner`, `escalation=owner` | owner ping |
| Helper quiet, at night | Board | the same with `escalation=lead` | plain line |
| Team quiet, helper available | the helper, directed | `stallWatch=team`, `episode`, `entry`, `item` | not mirrored |
| Team quiet, owner, by day, or the 07:00 ping | Board | `stallWatch=team`, `episode`, `entry`, `item`, `recipientKind=owner`, `escalation=owner` | owner ping |
| Team quiet, owner, at night | Board | the same with `escalation=lead` | plain line |

- No notice carries an `obligation` ref, so the owner ping has no Nudge, Extend or Reassign button.
- The night line reuses `escalation=lead` only because the bridge mirrors that value as a line without a mention. It is not addressed to a lead.
- The broker logs the actions `helper-quiet`, `team-quiet`, `helper-quiet-ping` and `team-quiet-ping`.

### What a notice says

Every notice has four parts, in this order:

1. What was observed: who, the effective state, how long quiet and since when, and for the helper how many owner items are owed and the oldest message number.
2. "The hub does not know why it stopped. It typed, restarted and resumed nothing."
3. "Before resuming, record why it stopped (the recovery prime directive: cause, incident, prevention)."
4. One suggestion, introduced by "Once that is recorded, the usual next step for this state is:".

No notice names a cause that the state does not prove. As whole words, restart, resume, prompt and Escape appear only in part 4, after that condition.

| Helper state | Suggested next step, after the record |
| --- | --- |
| `idle`, `finished_silent` | look at the helper's session; if its last turn ended normally, prompt it |
| `stuck`, `hung_tool`, `looping` | look at what the session is waiting on before interrupting it |
| `crashed` | check the session's last output; a restart needs `tt helper register` afterwards |
| `provider_blocked` | the provider step from [provider-blocked.md](provider-blocked.md) |
| `runtime_prompt` | read the prompt showing in the helper's terminal |
| `needs_input`, `unknown` | open the helper's session and see what it shows |
| `offline` | check the relay on the host with `tt doctor` and whether the session is still open |

A team notice suggests reading the team's last messages and looking at its lead's session.

## What the hub does and does not do

- The watch is one sweep on the broker tick, after the shared-path wait sweep.
- A sweep writes only its notices and its own `stall_watch_episodes` rows.
- It never types into a pane, presses Escape, or restarts, closes or resumes an agent.
- It never changes queue or work-item state.
- It adds no hub request. In particular there is no per-tool-call request: the last working time rides on the activity report that the relay already sends.
- **The directed helper notice is delivered like any directed notice.** The hub creates its delivery-only obligation and wake job, and the relay's ordinary delivery wakes the helper to read it. That is the only way the watch reaches a session. The run-end notice ([When a matrix run ends](#when-a-matrix-run-ends)) reaches its agent the same way, and is posted by the activity report, not by a sweep.
- With nothing quiet and no open episode, a sweep makes three reads and no write.
- For a helper or team held after a hub start, a sweep opens no episode and posts nothing. It still closes that subject's ended episode and moves its cursor, and it judges every subject that is not held as usual.
- The decision is made twice. An unlocked read nominates projects; the write transaction then decides again from fresh rows, so a post, a working report, a pause, a hold, a closed obligation or a replaced run that arrives in between stops the notice, including the 07:00 ping.

Both message reads are bounded:

- to open an episode, the sweep reads the project's messages newest first and stops at the first one older than 30 minutes;
- to end an episode, it reads at most 500 messages after the episode's cursor. When all 500 hold no ending post it moves the cursor, so the next sweep continues from there.

## When a matrix run ends

Feature `wi_a598ffa55a326699` revision 2, owner orders #33442 and #33457, builder assignment #33521.

On 2026-10-13 a lead that was its own verifier sat beside a finished receipt for 33 minutes: its watcher's message to itself was refused, and the day before another lead lost its watcher shell to the memory reaper. The team watch was working as written, since it counts 30 minutes from the end of the run. So the hub now tells the agent itself, at the end.

**What triggers it.** The relay on the agent's host reads the host's matrix lock every 15 seconds ([agent-activity.md](agent-activity.md)). When the agent's run is no longer there, because it finished, failed, was withdrawn or was killed and left a dead process behind, the relay's next activity report carries no `matrixWait`. That report posts the notice, in its own transaction. Nothing depends on the shell that started the run, on a child of the agent, or on the agent's turn. The sweep is not involved.

**Who gets it.** For a lock entry that names the agent, the agent whose report lost the wait, whatever its role: a team member, a lead acting as verifier, or the owner helper. A lock entry that names no agent (`agent` empty or `unknown`, as a run started without `TAILTERM_AGENT_NAME` writes it) is the wait of every agent bound to its item, and the relay marks that wait `agentless` (`wi_ab6db76678163f2b`). Its end goes to one member only: the item lead (its open `item_team_leads` row, at that run, not closed or exited), else the earliest-bound live agent on the item. It is posted only from that member's own report; every other member's report posts nothing and wakes no one. A retired agent gets none, and when the recipient is retired nobody gets it in its place.

**The notice.** A directed hub notice with the subject `Your matrix run has ended`, delivered like any directed notice: unread for the agent, with a delivery-only obligation and a wake job due at once. The hub types, restarts and resumes nothing. The text begins `Your verification run`, or `Your team's verification run` for an agentless run, since the receipt may be in another member's log directory.

- It names the run by its item, its role (`running`, or `waiting` for a run that left the waitlist without being seen running) and its start time, and gives the time the report saw it gone.
- **It carries no path and no result.** The hub is told only that the run left the lock ([agent-activity.md](agent-activity.md): no pid, commit, path or output of a run leaves the host). The text says where to look: `receipt.json` (`targeted-receipt.json` for a targeted run) in the log directory the agent gave the run, whose checks are the outcome; with no receipt, `runner.json` there for an exit code and `host-lock.log` for the reason, and the recovery rule's record of why it stopped comes first.
- **It never authorises another run.** The text says so, and suggests none.
- Refs: `action=matrix-run-ended`, `agent`, `run`, `item` when known, `matrixRole`, `matrixSince`, `endedAt`, and `matrixAgentless=true` for an agentless run only. When the earlier report cannot be read, the notice names the agent's bound item and no role or start time.

**Once per run.** The notice is posted by the one stored report that ends the wait. A repeated report, a retried request, a later state change and a moved waitlist position post nothing. A second run gets its own notice.

**A replaced run.** A report that still carries a wait, but another run's, posts the notice for the earlier run: running then waiting, running or waiting with a different start time, or a different item. A waiting run that gets the host is the same run and posts nothing. The agent is still in a wait, so no end of the wait is stored.

**Quiet time.** The notice is the hub's message, not the agent's, so it is no progress for either watch. Quiet time still starts at the end of the run, as in [When a never-quiet case ends](#when-a-never-quiet-case-ends), and the team notice still comes 30 minutes later when the agent does nothing.

**Latency.** One 15-second check and one report: normally under 30 seconds.

**Limits.**

- The lock entry must name the agent (`TAILTERM_AGENT_NAME`) or be agentless with a valid item, and the relay on that host must be running. With the relay down the notice is posted when it next reports.
- An agentless run's notice arrives only when the recipient's own relay sees the run end. A recipient whose relay does not report it, for example one on another host, gets none, and no other member is told instead.
- A run that joins the lock, runs and ends wholly inside one 15-second check is never reported and gets no notice. This is recorded, not fixed. A run seen waiting that then runs and ends inside one check gets the waitlist text.
- Only a confirmed lock read ends a wait. On an unknown read (an absent, unreadable or malformed lock, or one of another version or host) the relay repeats the last confirmed wait the hub holds for that run, so no notice is posted. A wait the relay can no longer confirm ends at the 4 h 30 min bound, for the watch only: no notice is posted then.
- Two relays with different state directories that report for one agent are not coordinated by this feature.
- A dead leader process whose process group lives on reads as ended. The text says the run left the lock, never that it passed.
- An agent that followed its run inside its own turn finds the notice in its inbox as one line.

## Tables

- `stall_watch_progress`: one row per run with its last working time, whether it reports a matrix wait, and when that wait last ended. Written only by the activity report.
- `stall_watch_episodes`: one row per episode with its subject, the state named, quiet-since, open and close times, the close reason, the recipient, the owner state (`deferred` or `pinged`), the notice and ping message numbers and the message cursor. A partial unique index allows one open episode per subject.

The migration only creates these tables. Nothing is altered or back-filled.

## Limits

- **Routing of team notices.** With no live helper, an offline one, or one whose state is `crashed`, `stuck` or `provider_blocked`, the team notice goes to the owner. A helper in `hung_tool`, `looping`, `runtime_prompt`, `finished_silent`, `unknown` or `needs_input` still gets the directed notice.
- **The recipient is chosen once**, when the team episode opens. If the helper goes quiet, crashes, sticks or is blocked afterwards, that team episode does not move to the owner; the helper watch covers a helper that owes the owner.
- **A crashed, stuck or provider-blocked helper that owes nothing is not watched itself.** The owner learns of it from the next team notice, which names the state.
- **Hub downtime counts as quiet time** for an agent that was already quiet. Only the judgement of unheard agents is held, and only for 60 seconds.
- **The hub start is kept in memory**, set by the hub process. The hold follows every start, after a short restart as after a long outage. A relay that takes more than 60 seconds to reach the restarted hub can still be judged offline.
- **Delegated owner requests do not count as owed.** A request routed to the helper during a delegation window is addressed by an agent, so it does not start the helper watch.
- **First deployment.** A run that stopped working before the hub with this feature started has no progress row. Its quiet time then rests on its posts and its oldest owed item, which can make the first notice earlier than a longer history would, never suppress it.
- **An exited helper is not watched.** Its obligations are closed as recipient-gone. A closed pane normally shows as `offline`, which is watched.
- **A provider-blocked helper gets two notices**: the provider-block notice after about two minutes and the helper watch after twenty. They name the same fix.
- **An old unanswered `tt ask` from the helper's current run** keeps the helper watch off until the owner answers it.
- **A replaced run starts a fresh 20 minutes**, because registering the helper is the owner's own action.
- **The watch covers the helper and queue teams only.** The database handler, the deployer and the steward are not watched.

## Owed after release

A daytime live check on a disposable project with a synthetic quiet helper, where the owner confirms one ping. It is not part of the automated tests, which use temporary databases and synthetic agents only.
