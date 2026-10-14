# Hub reachability watch

Feature `wi_e493b596a86e0e2f` revision 2, owner order #33825, builder assignment #33885.
Prevention order of incident `nart_839519190317d9ba`.
Review follow-ups `wi_7de9f3f64dff405d` revision 2, order #34265.

On 2026-10-13 the Mini had no route to any tailnet peer from about 17:26 to 17:39 Pacific. Every agent on it was cut off from the hub, and nothing could tell the owner: every alarm of the project, and the Discord bridge, live in the hub. The relay on each host now watches whether that host reaches the hub, and tells the owner by a path that makes no hub call.

The watch **reads and reports, and does nothing else**. It changes no network setting, recovers no tunnel, and sends no key to any session. An alert starts the diagnosis. Under the recovery prime directive, the record of why the host lost the hub stays with the owner helper or the owner.

Code: `hub/cmd/tt/hub_reach_watch.go`, started from `cmdRelay` in `hub/cmd/tt/relay.go`.

## What it watches

- The long-running relay starts the watch once, on a timer of its own. `tt relay --once` and `tt relay --status` do not start it. The relay loop slows down exactly when the hub does not answer, so the watch does not run on that loop.
- Every 30 seconds it sends one request to the hub named in `~/.config/tailterm/hub.json`: `GET /v1/whoami` with the host's hub token, a 5 second timeout, and redirects not followed.
- The request is a read and is not counted against the relay's request budget. A drained budget would make it wait, and that would read as a failure.
- With no hub in `hub.json` the watch idles and says so once in the relay log.

**A call failed** when there was no HTTP response, or the status was outside 200 to 299. So 401, 503, 302 and 304 all fail.

**A call succeeded** on any status from 200 to 299. The body is not parsed: at most 64 KiB is read and discarded.

## Three minutes

- "Failing since" is the time of the first failed call after a success. Any success clears it.
- An episode **opens** on the first failed call at least 3 minutes after "failing since". At 30 second spacing that is the seventh failure in a row.
- If the previous call is more than 2 minutes old and no episode is open (the host slept, or the relay was stopped), "failing since" restarts at this call. A gap never opens an episode by itself.
- The 3 minutes are fixed. They are not a setting.

## Cause class

After each failed call, and only then, the watch runs two read-only commands, each with a 3 second timeout. A healthy host runs neither.

| Reading | Command (macOS) | Values |
| --- | --- | --- |
| route | `route -n get HOST` | `tunnel` when the interface starts with `utun`; `not-tunnel` for any other interface or no route; `unknown` when the output cannot be read |
| dns | `scutil --dns` | `present` when a nameserver is `100.100.100.100`; `missing` when none is; `unknown` when the output cannot be read |

- The route reading applies only when the hub host is a tailnet address (`100.64.0.0/10` or `fd7a:115c:a1e0::/48`) or a `.ts.net` name. For any other hub it is `unknown`.
- On a system other than macOS both readings are `unknown`.

The class of a failed call is the first matching row:

| Order | Condition | Class | Detail |
| --- | --- | --- | --- |
| 1 | an HTTP answer outside 200 to 299 | `http_error` | the status |
| 2 | no answer, and route is `not-tunnel` | `no_tunnel_route` | `timeout`, `refused` or `other` |
| 3 | no answer, and dns is `missing` | `tailnet_dns_missing` | `timeout`, `refused` or `other` |
| 4 | no answer, anything else | `tcp_timeout_or_refused` | `timeout`, `refused` or `other` |

An episode records the class and both readings at the call that opened it, and those of its latest failed call. The watch states readings. It does not claim why a route or a resolver is missing.

## When an episode opens

Day or night, the relay:

1. writes the marker file `HUB-UNREACHABLE` in the relay state directory (`~/.local/state/tailterm/relay/`). It names the host, the hub address, failing since, opened at, the class and readings at opening and at the latest failed call, and the state of the owner alert. Its last line is "The relay changed no network setting and recovered nothing.";
2. writes one relay log line: `[tt relay] <time> HUB UNREACHABLE since <time>: <class> (route=<r> dns=<d>)`.

While the episode is open, later failed calls update the latest class in the marker. There is no second log line and never a second alert. The marker exists exactly while an episode is open.

## The owner alert

**Channel.** A direct message from the project's Discord bot to the owner, sent straight to Discord from the host with the bot token already on it. No hub call is on this path. It uses Discord's REST interface only and opens no Gateway session, so the bridge's session is not disturbed.

**One alert per episode.** The alert says what was observed (host, hub address, failing since, class, readings), that the relay changed no network setting and recovered nothing, that the reason must be recorded before recovering, and where the marker file is. Its last line is a reference: `ref hub-reach alert <episode>`.

**Retries.** While the episode is open and it is day, a failed alert is tried again on each of the next ticks up to 10 attempts, then at most once per 5 minutes.

### Nights

From 22:00 to 07:00 Pacific **no Discord call is made**: no alert, no recovery line, no retry and no read. The rule is the stall watch's ([stall-watch.md](stall-watch.md#nights-d2)), and the zone is `America/Los_Angeles`, so it follows daylight saving time.

- An episode that opens by day is the owner's from its opening.
- An episode that opens at night gets the marker and the log line at once. It becomes the owner's on the first **failed** call at or after 07:00, and its one alert is sent then.
- An episode that opens and ends inside one night never reaches Discord. It gets the log lines and the Board notice only.
- A Discord line that is owed when night falls is held, and sent on the first tick at or after 07:00. So a day alert followed by a night recovery sends the recovery line at 07:00, and a send that failed at 21:59 is not tried again at 22:00.

## Recovery

On the first successful call of an open episode, the relay:

1. removes the marker and writes one log line with the start, the end, the duration and the class;
2. owes one **Discord recovery line**, if the episode is the owner's. It names the host, when the outage began, when the hub answered again, the duration and the class. It is sent whether or not the alert was delivered. After recovery only the recovery line is sent or read; an alert not delivered by then is never tried again, and the recovery line says what became of it:
   - alert `delivered`: nothing more;
   - alert `unknown` (its send got no answer and no read has settled it): the alert becomes `unconfirmed`, and the line says "The alert for this outage got no answer from Discord and may not have been delivered.";
   - alert `owed` (never sent, or every send refused): the alert becomes `dropped`, and the line says "No alert was delivered for this outage.";
3. owes one **Board notice** per task, for every episode, visible to the owner or not.

**The Board notice** is a typed NOTICE posted as the relay, for each task that has a valid binding on this host for the watched hub (at most 5).

- It is always directed: to the task's live owner helper, else to its live primary database handler. A notice with no recipient would wake every agent of the task. With neither live, the notice stays owed.
- Its refs are `hubReach=recovered`, `host`, `cause`, `durationSeconds` and `episode`. It carries no `escalation` and no `obligation` ref, so the bridge sends no ping for it.
- Its request ID is `hub-reach-<host>-<episode start>-<task>`. The hub stores request IDs, so a repeat after a lost answer is safe.
- It is not a Discord call, so it is posted at any hour.

**Retries.** A recovery line and a Board notice are tried at most once per 5 minutes for 24 hours, then given up with one log line. For a recovery line held through a night the 24 hours start at 07:00. A Board notice is not tried on a tick whose hub call failed.

## A send whose answer was lost

The alert and the recovery line of one episode never share an identity:

| Message | Nonce | Last line |
| --- | --- | --- |
| alert | `hra-<host hash>-<episode start>` | `ref hub-reach alert <episode>` |
| recovery | `hrr-<host hash>-<episode start>` | `ref hub-reach recovery <episode>` |

Each is sent with Discord's enforced nonce, which covers a repeat within a few minutes. The last line is the durable identity.

The host hash is 8 hexadecimal digits of the FNV-1a hash of the host name, and the episode start is in Unix seconds, so a nonce is 23 characters, within Discord's 25. It is the same for one host and episode across a relay restart, and two hosts whose outages start in the same second do not share one: Discord would otherwise return the first host's message for the second host's send.

1. Before a send, the delivery is saved as `attempting`.
2. Discord answers with success: `delivered`. Discord answers with an error status: the message was not created, so the delivery is `owed` again.
3. No answer (a timeout, a connection error, or a relay stopped mid-send): `unknown`.
4. An `unknown` delivery is never sent again directly. On its next turn the watch reads the channel: up to 100 messages from one minute before the episode opened. If a message of the bot's ends with the reference, the delivery is `delivered`. If the read succeeds and none does, it is sent in the same turn. If the read fails, it stays `unknown`.
5. An alert still `unknown` (or `attempting`) when the episode closes becomes `unconfirmed`: it is not read or sent again, it keeps no episode owed, and the recovery line says it may not have been delivered.

## State and relay restart

`hub-reach.json` in the relay state directory (mode 0600) holds "failing since", the open episode, and the closed episodes that still have a delivery owed, each with its own deliveries. At most 20 closed episodes are kept. A 21st drops the oldest with one log line that names its start and what was not delivered.

The file is read when the relay starts. An open episode stays open, nothing delivered is sent again, and the next call either continues the episode or closes it with the duration from its original start. A healthy host does not rewrite the file.

`tt relay --status` ends with `hub-reach: ok`, or `hub-reach: UNREACHABLE since <time> cause=<class> visible=<yes|no> alert=<state>`, and `hub-reach: N closed episodes with deliveries owed` when there are any.

## Settings

Three keys of `~/.config/tailterm/relay.json`, read by exact name on every tick, so a change needs no relay restart:

| Key | Meaning | Default |
| --- | --- | --- |
| `hubReachDiscordOwner` | the owner's Discord user ID, 17 to 20 digits | none: detection, the marker, the log and the Board notice still work, and one log line per episode says that no owner is configured (once per episode while the relay runs, however many episodes owe a line; a relay restart may repeat it once) |
| `hubReachDiscordTokenFile` | the bot token file | `~/.config/discord/token` |
| `hubReachWatch` | `"off"` stops the watch | on |

- The owner's ID is host configuration. It is not committed: the repository is public.
- The token is read from its file for each call. It is kept in no state file, written to no log, and never part of an error text. A token file that others can read is still used, with one warning that names the path and the mode.
- No new account or secret is involved.

## What it never does

- It runs no command but `route -n get` and `scutil --dns`.
- It changes no network or tunnel setting and does not bring the tunnel back.
- It sends no key to a session and restarts or resumes no agent.
- It writes an incident record for nobody. The alert asks for one.

## Limits

- **Reachability, not correctness.** Any 2xx counts, and the body is not read. An answer from something else on the hub's address would count as the hub.
- **The whole internet down.** The marker and the log are written; the alert is retried while the episode is open; the recovery line is owed in any case and says that no alert was delivered, or, when the alert's send got no answer, that it may not have been delivered.
- **Flapping.** A hub that is down 3 minutes and up 30 seconds, again and again, gives an alert and a recovery line each time.
- **The read covers 100 messages.** If more than 100 later messages are in the DM before the check, a send whose answer was lost may repeat once.
- **Each host alerts for itself.** A second host with the same settings sends its own lines; the text names the host, and the nonces carry a hash of it.
- **A reply to the alert** in the DM goes to the owner helper as helper chat, through the hub, once the hub is reachable.
- **Cause classes on Linux.** Both readings are `unknown`, so an unanswered call is `tcp_timeout_or_refused`.
- **A stopped relay watches nothing.** The watch lives in the relay process.

## Release and live check

- Release to the Mini only, by day. At release the owner helper adds `hubReachDiscordOwner` to the Mini's `relay.json` by hand.
- Until that key is set, the watch detects, marks and logs, and sends nothing to Discord.
- A live check is owed after release and is planned with the owner present: it is the first real Discord message of this feature. No test posts to Discord, runs `route` or `scutil`, or reads the real token.
