# Runtime prompts

Bug `wi_7d5050c57193db78` revision 1, owner order #13871, lead assignment
#14367 (plan sha256 `4c878ff1…06d8`), lead decision #14370.

An agent's runtime sometimes stops on its own modal prompt instead of at its
input: Codex's rate-limit model menu or model-migration menu, a Claude
permission, selection or folder-trust dialog. Wakes then queue behind the
prompt unseen. The launch-host relay detects this, reports a distinct
`runtime_prompt` activity state, answers the prompt only where the project
policy allows it, and otherwise tells a person once.

## Detection

The relay's activity tick (every 15 seconds per binding, see
[agent-activity.md](agent-activity.md)) captures the pane of a live runtime
only after its transcript has been quiet for 30 seconds, or before its first
turn. `TAILTERM_ACTIVITY_PROMPT_SECONDS` overrides the 30 seconds. The capture
uses the same exact identity as a Claude wake: one pane in the binding's tmux
session, the TAILTERM_* run environment, one live runtime process descended
from the pane, and a pane that is not in a copy or view mode.

The classifier reads only the prompt area at the bottom of the capture:

- Codex: the last row on screen must be a key hint (`enter continue · esc
  quit`, `enter/esc confirm · ctrl+c quit`, `Press enter to …`). The area is
  that footer, the numbered options above it and the title block above those,
  ending at two blank rows or eight rows above the first option. An idle
  composer ends with its status line and never matches.
- Claude: below an idle input box's top rule nothing is a dialog. With no
  input box, the dialog starts at the rule Claude draws above it.

A known kind needs its exact title and option labels. Anything else with a
selection marker before a numbered option or a key-hint phrase is `unknown`.
Transcript text that quotes a menu or dialog above the prompt area is never
read. The report holds only the kind, its fixed label, the policy action, the
outcome, a bounded reason and a 16-byte SHA-256 fingerprint of the normalized
prompt area; no screen text leaves the host. The agent's status, including
`needs_input`, is left as it is. When the prompt goes, normal classification
resumes.

Current coverage is bindings the relay already has: Claude sessions and Codex
threads that have run a `tt` command. A Codex session stuck on a startup menu
before its first `tt` command has no binding yet.

## Policy

| Kind | Runtime | Allowed actions | Default |
| --- | --- | --- | --- |
| `codex_rate_limit_switch` | Codex | `keep_current_never_show`, `keep_current`, `escalate` | `keep_current_never_show` |
| `codex_model_migration` | Codex | `use_existing`, `escalate` | `use_existing` |
| `codex_usage_limit` | Codex | `escalate` | `escalate` |
| `codex_trust` | Codex | `escalate`, `report` | `escalate` (#14370) |
| `claude_permission` | Claude | `escalate` | `escalate` |
| `claude_selection` | Claude | `escalate` | `escalate` |
| `claude_trust` | Claude | `escalate` | `escalate` |
| `unknown` | any | `escalate` | `escalate` |

No action can approve a permission, trust a folder, switch models or request a
limit. `codex_trust` escalates by default because Codex 0.156.1 draws a new
trust layout (`Trust this folder?`) that `startup.go` no longer matches; that
stale match is a separate item.

The policy is per project and revisioned. Revision 0 means the defaults.

```
tt prompt-policy get --task tsk_…
tt prompt-policy set --task tsk_… --revision N --action codex_rate_limit_switch=escalate
```

`set` is the owner's: it refuses inside an agent session, the hub refuses an
agent caller, a stale revision is a conflict and a disallowed action is
refused. The API is `GET/PUT /v1/tasks/{id}/runtime-prompt/policy`. The relay
caches a project's policy for a minute. If the policy cannot be read it
escalates; an older hub without the endpoint gets no `runtime_prompt` reports.

## Answers

Only the Codex keep/use actions type into a pane, with the same safety rules as
a Claude wake:

1. The project is active and the agent run is current, online and not closed,
   exited or retired. At most three answers per agent run in five minutes.
2. A second inspection has the same pane identity and prompt fingerprint.
3. A private run-scoped intent (`<binding>-<run>.runtime-prompt.json` under
   the relay state folder: fingerprint, kind, action, target label, keys,
   phase) is saved before any key, and the prompt is marked attempted.
4. Only navigation keys (`Down` or `Up`) move the selection. The pane is
   captured again and the marker must be on the exact target option label
   before a separate `Enter`.
5. The answer is confirmed when, within five seconds, the pane no longer shows
   that prompt.

Each prompt fingerprint gets at most one attempt per run. A failed or
ambiguous answer is never resent, including after a relay restart mid-answer,
and escalates. A skipped answer (paused project, retired agent, pane or
prompt changed, answer limit) sends no key and is retried on a later tick.

Outcomes: `confirmed`, `failed`, `ambiguous`, `escalated`, `reported`,
`skipped`.

## Escalation

The hub posts one owner-facing notice, plus one notice to the item's lead when
the agent belongs to an item team, for the first report of an agent run and
prompt fingerprint whose outcome is `escalated`, `failed` or `ambiguous`.
Repeats, receipt replays, a prompt that clears and returns, and relay restarts
post nothing more. A new fingerprint posts again. Confirmed answers and
`report` post nothing. `unknown` always escalates.

## Logs and display

- Relay log: one line per change, `[tt relay] <time> <agent> runtime prompt
  <kind>: <action> <outcome> (<reason>)`, and `runtime prompt cleared`.
- `tt relay --status`: `prompt=<kind> action=… outcome=… prompt-at=…`.
- `tt agents`: `activity=runtime_prompt` and a `runtime-prompt kind=…
  action=… outcome=… since=…` line.
- TailOS Projects: the `Runtime prompt` state, with the prompt, action and
  outcome in the agent's detail.

## Checks

`go test ./cmd/tt -run 'TestRuntimePrompt|TestPromptPolicyCLI|TestAgentsRuntimePrompt'`,
`go test ./internal/store ./internal/server -run TestRuntimePrompt`,
`node tests/activity-browser.mjs` and `node --test tests/activity-format.test.js`.
Fixtures and their provenance are in `hub/cmd/tt/testdata/runtime-prompt/`.
`TestRuntimePromptTmuxReplay` drives the native tmux path on a private socket.
The opt-in live check is `TT_LIVE_RUNTIME_PROMPT=1 go test ./cmd/tt -run
'^TestRuntimePromptLive' -count=1 -v`; its recorded run is
[runtime-prompts-live-check.txt](runtime-prompts-live-check.txt).
