# Agent permissions, tools and waiting

Team members and individual task-agent launches have **Permissions & tools**.
The default is **Host settings**, including for saved teams and all example teams.
Existing agents are unaffected. Explicit settings are saved in the encrypted vault
and passed as quoted launch arguments to the selected machine's `tt` CLI.

## Launch controls

| App | Choice | Tailterm launch behavior |
| --- | --- | --- |
| Codex | Host settings | No permission flags; preserve host configuration. |
| Codex | Ask when needed | `--ask-for-approval on-request`; host sandbox configuration remains. |
| Codex | Workspace · no approval prompts | `--sandbox workspace-write --ask-for-approval never`; requires an explicit absolute working directory. Network access is enabled for task communication, and the Tailterm relay directory is an additional writable root. |
| Codex | Full machine · no approval prompts | `--sandbox danger-full-access --ask-for-approval never`; commands have the SSH user's machine access. |
| Claude | Accept edits | `--permission-mode acceptEdits`. Other operations may still ask. |
| Claude | Automatic review | `--permission-mode auto`. It can still prompt or be unavailable. |
| Claude | Preapproved tools | `--permission-mode dontAsk`. A call that would prompt is denied. |
| Claude | Bypass checks | `--permission-mode bypassPermissions`. Does not override every external or managed restriction. |

Claude also accepts optional preapproved tool rules, one per line. Each rule is
passed as its own `--allowedTools` argument. These approve matching calls; they
are not a complete tool allowlist. To restrict built-in tool availability, Claude
has a separate `--tools` control, which can currently be included in the command
override. [Claude CLI reference](https://code.claude.com/docs/en/cli-reference).

A command override with explicit permission flags must use Host settings or
remove those conflicting flags before choosing a preset. Custom wrappers remain
responsible for accepting and applying the arguments they receive. Other runtimes
currently retain host settings; their custom launch flags can be supplied through
the command override. Preset support requires a compatible installed agent CLI.

Same-app helpers inherit their parent's explicit permission preset and Claude
allow rules when those flags are omitted from `tt spawn`. Helpers inherit the parent’s project folder when no directory is supplied,
including helpers using a different app. An explicit
child flag overrides that default. This is launch configuration, not a hub-enforced
privilege ceiling: task agents already run as the selected SSH user. Different
apps do not inherit incompatible permission modes.

**No prompt does not mean every action succeeds.** Codex separates command
approvals from sandbox access; `never` can be used while retaining a sandbox.
Connector/MCP approval rules may still require interaction. [OpenAI: Agent approvals
and security](https://learn.chatgpt.com/docs/agent-approvals-security).

Claude's `dontAsk` denies calls that would prompt, while `auto` can fall back to
asking and bypass mode has exceptions. These controls cannot make an expired
login, startup trust prompt, missing credential, OS permission, or remote service restriction disappear.
[Claude permission modes](https://code.claude.com/docs/en/permission-modes).

## Tool inspection

**Inspect host tools** checks the member's assigned machine, or the currently
selected main machine when assignment is blank. It runs `tt tools --runtime APP
--cwd /project --json` over the existing SSH connection. An unavailable explicit
machine is an error; it does not silently choose another machine.

The report includes the installed app/version and a bounded list of common
installed commands. For Codex it requests the MCP tool registry, reporting server
names, tool names, authentication state and runtime connection state when known.
It omits server URLs, credentials, environment values, tool schemas and descriptions.
No model turn or live task is created, and no advertised tool is called merely to
list it. Server initialization may occur as part of querying the registry.
Inspection has a 25-second overall timeout, including a shorter probe of the
existing daemon, and reports unavailable inventory instead of waiting indefinitely.

Inside an agent, `tt tools --runtime codex --json` first tries its exact
`CODEX_THREAD_ID` through the running Codex daemon. If that cannot answer, it falls
back to a host inventory and labels it accordingly. Pre-launch inspection is a
host inventory: project configuration and permission settings at actual launch
can differ. Claude, Gemini and Aider currently report installed commands/app
version and explicitly say their MCP/built-in tool inventory is not verified.

The structured Codex integration uses `mcpServerStatus/list`, which supports a
thread ID and returns tools and authentication/runtime status. It uses the
experimental app-server protocol and may need adaptation for another CLI version.
[Codex App Server](https://learn.chatgpt.com/docs/app-server).

An installed command is not proof that its sandbox allows it. An advertised MCP
tool is not proof that authentication, service access and a particular operation
will succeed. The report distinguishes names/status from successful-call testing;
it is not a complete enumeration of every built-in, lazy-loaded tool, executable
or language library an agent could eventually use.

## Visible blockers

Agents can report:

```sh
tt event needs_input --reason permission --text "The deployment command was denied."
tt event needs_input --reason authentication --text "The provider login has expired."
tt event needs_input --reason tool --text "The browser tool is unavailable."
```

Tasks and Board display **Permission blocked**, **Login required**, or **Tool
unavailable**, with the explanation in the agent tooltip. Ordinary questions
remain **Needs you**. The reason and explanation persist on the hub and clear on
an explicit running/done transition. Runtime stop hooks do not erase a blocker.
Claude's configured notification hook classifies permission prompts automatically.
The host relay also detects the visible Codex directory-trust startup prompt in
Tailterm-owned panes and reports it without accepting trust or sending terminal input.
Other blockers depend on the agent reporting them; Tailterm does not currently
intercept every runtime approval or detect every stalled subprocess.

Briefings tell agents to report a concrete blocker, tell the orchestrator, avoid
repeating an unchanged denied action, and continue independent permitted work.
Tailterm does not type approval responses into terminals, automatically approve
new permissions, or guarantee that an LLM will follow those instructions. A full
supervisor for runtime approval requests, subprocess timeouts, login health and
reassignment remains additional work.
