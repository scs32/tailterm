# Synthetic provider-failure transcripts

Fixtures for the `TestActivityProviderBlocked*` tests (bug `wi_72f41bd375032cf0` revision 1, order #11854, assignment #19880, criteria a1-a4 and a14). The two Codex retry-limit and overload fixtures: bug `wi_ce7310979bc45479`, order #46760, assignment #46804, criterion a1.

Every file is written by hand. Nothing is copied from a session: ids, prompts, replies and times are synthetic. Only the record shapes and the error texts follow what was observed on 2026-09-25 and 2026-09-26, and for the two later Codex fixtures on 2026-10-25 and 2026-10-26.

| Fixture | Runtime | Error fields | Text |
| --- | --- | --- | --- |
| `claude-fable-limit` | Claude Code | `error` `rate_limit`, `apiErrorStatus` 429 | the observed Fable limit message, three failed prompts |
| `claude-session-limit` | Claude Code | `error` `rate_limit`, `apiErrorStatus` 429 | the observed session limit message, three failed prompts |
| `claude-login-expired` | Claude Code | `error` `authentication_failed`, no status | the observed login message, one failed prompt |
| `claude-server-error` | Claude Code | `error` `server_error`, no status | the observed server error message, two failed prompts, then a normal reply |
| `codex-401` | Codex | `task_complete.error` | the observed 401 message; the key in it is made up and is not a key |
| `codex-503` | Codex | `task_complete.error` | `unexpected status 503`, two failed turns, then a normal turn. This shape was not observed; it follows the 401 form. |
| `codex-429` | Codex | `task_complete.error` | `unexpected status 429`, two failed turns. Not observed either. |
| `codex-retry-limit-429` | Codex | `task_complete.error`, `codex_error_info` `{"response_too_many_failed_attempts":{"http_status_code":429}}` | the observed `exceeded retry limit, last status: 429 Too Many Requests`, two failed turns five minutes apart, then a normal turn |
| `codex-server-overloaded` | Codex | `task_complete.error`, `codex_error_info` `server_overloaded` | the observed `Selected model is at capacity. Please try a different model.`, two failed turns, then a normal turn |

The Claude error records carry model `<synthetic>`, as Claude Code writes them; the real model is on the ordinary assistant record before them.
