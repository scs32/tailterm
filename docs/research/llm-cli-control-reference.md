# Native LLM CLI controls for Tailterm

Research snapshot: September 8, 2026. Assignment: lead #297, following owner #291.

Tailterm can launch the native CLIs with their normal tools, instructions, MCP servers, hooks, and authentication intact. Its current controls expose only part of what those CLIs support. Preserve native defaults when no override is selected; add explicit per-launch controls for the parts the owner wants to vary.

## Evidence and scope

**Installed help verified on Stephens-Mini:** Codex CLI **0.153.4**, Claude Code **2.1.263**, Gemini CLI **0.53.1**. Checks used version/help output, including Codex exec/resume/fork/queue/MCP/plugin help. “Installed” below means the executable advertises the option; it does **not** mean a real session, tool connection, entitlement, or permission transition was exercised. No agents were launched, MCP servers installed, settings changed, or credentials/configuration contents inspected. Examples are literal templates, not executed commands.

**Official documentation verified:** linked vendor documentation below supplies configuration semantics and controls missing from help. Documentation can run ahead of an installed binary. Flags, accepted effort values, model capabilities, managed policies, and operating-system sandbox support must be checked on the eventual launch host.

**Tailterm verified:** read-only inspection of the current checkout. Host discovery, configured tools, live connected tools, and tools actually exposed to one exact thread are separate inventories. `tt tools --runtime codex --json` is useful host information; it is not proof that every listed tool is callable in this thread.

## Important distinctions

- **Permission approval, available tools, and OS sandboxing are separate.** Approving a tool does not remove other tools. A prompt asking an agent to avoid a tool is guidance, not an enforced restriction.
- **MCP is native in Codex and Claude.** A Tailterm MCP adapter could add project operations while retaining native tools. DSH or another proxy is not required to connect MCP servers.
- **System/developer instructions differ from the assignment prompt.** A positional prompt is a user turn. Replacing the native system prompt can remove useful tool guidance; additive instructions usually fit Tailterm better.
- **Messages do not directly reconfigure a process.** Changing the model, permission mode, MCP inventory, or environment requires an actual native control, supported API, or relaunch. A teammate message containing a flag is still text.
- **Exact identity matters.** Native thread/session IDs, Tailterm agent IDs, Tailterm run IDs, and tmux session names represent different things. A fork is a new native conversation; a restart is a new Tailterm run.

## Codex: installed launch and session controls

The following flags were present in installed help. The [official command reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli) is the ongoing reference; use local help to resolve version differences.

| Need | Installed native control | Practical meaning |
| --- | --- | --- |
| Start interactively | `codex [PROMPT]` | Keeps native terminal UI and normal configuration loading. |
| Model/provider | `--model MODEL`; `--oss`; `--local-provider ollama` or `lmstudio` | Model availability depends on the selected provider/account. |
| Per-launch configuration | Repeat `-c 'key=value'`; `--strict-config` | TOML values and dotted paths; strict mode reports unknown keys. |
| Profile | `--profile NAME` | This version advertises a separate `$CODEX_HOME/<name>.config.toml` layer. Do not assume older profile-table syntax. |
| Working directory | `--cd DIR`; repeat `--add-dir DIR` | Sets project discovery context and additional writable roots, subject to sandbox policy. |
| Sandbox | `--sandbox read-only`, `workspace-write`, or `danger-full-access` | Independent of approval policy. |
| Approvals | `--ask-for-approval on-request` or `never` | These are the choices in this installed help. `never` does not itself grant filesystem/network access. |
| Automatic review | `--approve-for-me` | Advertises automatic approval review with workspace-write access. |
| YOLO | `--dangerously-bypass-approvals-and-sandbox` | Disables Codex approval and sandbox boundaries; external OS/provider/organization limits still apply. |
| Hook trust | `--dangerously-bypass-hook-trust` | Separate from ordinary approval/sandbox selection; do not silently add it to YOLO. |
| Web/images/UI | `--search`; `--image FILE`; `--no-alt-screen` | Live web-search tool, image input, and terminal rendering respectively. |
| Feature selection | `--enable FEATURE`; `--disable FEATURE` | Equivalent to feature configuration; not a universal tool allow/deny list. |
| Resume | `codex resume SESSION_ID [PROMPT]` | Prefer the exact native UUID; `--last` is convenience, not reliable routing. |
| Fork | `codex fork SESSION_ID [PROMPT]` | Creates a new conversation from existing history. |
| Queue to a thread | `codex queue --thread UUID --message TEXT` | Exact target; installed queue help also accepts native model/config/permission flags. Changing those through queue was not live-tested. |
| Remote client | `--remote ADDR`; `--remote-auth-token-env ENV_NAME` | Separate native remote/app-server transport; not a Tailterm hub URL. |

**Version trap:** installed help does not advertise `--full-auto`. Tailterm's **full-auto** preset is its own mapping, described below. Use the verified explicit flags in generated commands.

### Configuration, tools, instructions, and environment

These are documentation-verified configuration keys, not newly executed session tests. [Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference).

| Need | Configuration |
| --- | --- |
| Reasoning | `model_reasoning_effort`: documented `minimal`, `low`, `medium`, `high`, `xhigh`; model dependent. |
| Additional developer guidance | `developer_instructions`; `model_instructions_file` replaces built-in instructions. |
| Web tool | `web_search`: `disabled`, `cached`, `indexed`, `live`. |
| Workspace shell network | `sandbox_workspace_write.network_access`. |
| Child environment | `shell_environment_policy.inherit`, `.set`, and filters. |
| Built-in selection | `features.shell_tool`, `tools.view_image`; no observed universal `--tools` flag. |
| Native subagents | `agents.enabled`, `agents.max_concurrent_threads_per_session`, `agents.default_subagent_model`, `agents.default_subagent_reasoning_effort`. |
| Context/output | `model_context_window`, `model_auto_compact_token_limit`, `tool_output_token_limit`; none is a total spend limit. |
| Experimental budget | `features.rollout_budget.enabled` with `.limit_tokens`; under development, not a stable cross-runtime contract. |

`CODEX_HOME` changes Codex's home/configuration location, not just one launch option. Do not change it merely to select MCP servers: that can also change authentication and session discovery. Tool subprocess environment controls do not retroactively change the parent CLI environment. Model-provider credentials remain host responsibilities.

Codex reads project guidance through `AGENTS.md` discovery; more specific directory instructions can supplement broader guidance. Tailterm should preserve discovery and add its role/assignment explicitly. [Codex AGENTS.md](https://learn.chatgpt.com/docs/agent-configuration/agents-md).

Native subagents have their own configuration and concurrency limits. Those limits do not replace Tailterm's shared helper allowance or lifecycle accounting. A design that enables native delegation must decide whether those children appear in Tailterm's roster. [Codex subagents](https://learn.chatgpt.com/docs/agent-configuration/subagents).

Hooks support lifecycle/tool events and can run local commands. Non-managed hooks require trust; hook trust is separate from tool approvals. Preserve hooks unless explicitly disabled, and do not assume an installed plugin is trusted or active. [Codex hooks](https://learn.chatgpt.com/docs/hooks).

Web search and shell network access are different controls. Disabling the search tool does not block network-capable shell or MCP tools. The sandbox and runtime security policy determine those other paths. [Codex security](https://learn.chatgpt.com/docs/security).

### MCP and plugins

Codex supports stdio and HTTP MCP servers. `mcp_servers.NAME` accepts command/arguments/environment or URL/auth-reference configuration. Per-server controls include `enabled`, `enabled_tools`, `disabled_tools`, `required`, `startup_timeout_sec`, and `tool_timeout_sec`. The deny list applies after the allow list. Default startup/tool timeouts are documented as 10/60 seconds. Plugin MCP servers have separate `plugins.PLUGIN.mcp_servers.SERVER` controls. Installed `codex mcp` supports list/get/add/remove/login/logout; `codex plugin` has add/list/marketplace/remove. These management commands can persist changes; per-launch `-c` is the appropriate illustration here. [Codex MCP](https://learn.chatgpt.com/docs/extend/mcp?surface=cli).

There is **no strict “only these MCP servers” flag in the installed help**. Explicitly enabling one server does not disable all other configured or plugin-provided servers. Inventory both sources before claiming an exclusive selection.

```sh
# Assumes these server names already exist in host configuration.
# Other configured servers and plugin servers remain unaffected.
codex --cd '/absolute/project' \
  -c 'mcp_servers.aiv.enabled=true' \
  -c 'mcp_servers.aiv.enabled_tools=["status_get","review_brief"]' \
  -c 'mcp_servers.other.enabled=false' \
  'Review the project evidence.'
```

```sh
# Explicit Codex YOLO for this launch.
codex --dangerously-bypass-approvals-and-sandbox \
  --cd '/absolute/project' 'Implement the assigned change.'

# Separate model effort, workspace write access, and approval selection.
codex --cd '/absolute/project' --model 'MODEL_ID' \
  -c 'model_reasoning_effort="high"' \
  --sandbox workspace-write --ask-for-approval never \
  'Implement the assigned change.'
```

### Headless output, budgets, and resumption

Installed `codex exec` accepts a positional prompt or stdin (`-`), `--json` for JSONL events, `--output-schema FILE`, and `--output-last-message FILE`. `--ephemeral` avoids session persistence. `--skip-git-repo-check` bypasses the repository guard. `--ignore-user-config` skips the user config file but still uses the Codex home for authentication; it is not a verified isolation switch for every project/plugin source. `--ignore-rules` disables rule loading and is inappropriate as an incidental launch default.

Root `--ask-for-approval` is not listed in installed exec help; a configuration override is available. There is no advertised stable universal wall-clock or dollar-budget flag in this help. An external supervisor would need to enforce a deadline and record interrupted/unfinished work correctly; context limits and individual tool timeouts are different budgets.

```sh
codex exec --cd '/absolute/project' --json \
  --sandbox read-only -c 'approval_policy="never"' \
  --output-last-message '/absolute/output/result.txt' \
  'Summarize this repository.'

# Replace the all-zero example with the recorded native UUID.
codex resume '00000000-0000-0000-0000-000000000000' 'Continue the assignment.'
codex fork '00000000-0000-0000-0000-000000000000' 'Explore an alternative.'
codex queue --thread '00000000-0000-0000-0000-000000000000' \
  --message 'Review the new acceptance criteria.'
```

## Claude Code: installed launch and session controls

Unless explicitly labeled documentation-only, these flags appeared in installed help. [Official Claude CLI reference](https://code.claude.com/docs/en/cli-reference).

| Need | Installed native control | Practical meaning |
| --- | --- | --- |
| Interactive/headless | `claude [PROMPT]`; `--print` / `-p` | Print mode supports automation-oriented output/budgets. |
| Model/effort | `--model MODEL`; `--effort low/medium/high/xhigh/max` | Accepted effort is model dependent; environment can override effort, below. |
| Fallback | `--fallback-model MODEL` | Print mode only. |
| Working directory | Start from the desired shell directory; `--add-dir DIR` | No root cwd flag appeared in this help. |
| Permission mode | `--permission-mode manual/acceptEdits/auto/dontAsk/plan/bypassPermissions` | Distinct behaviors; `dontAsk` denies operations that would need prompting. |
| YOLO | `--dangerously-skip-permissions` | Enables bypass mode; not a promise to defeat every restriction. |
| Make bypass selectable | `--allow-dangerously-skip-permissions` | Enables later selection without making it the initial mode. |
| Preapprove tools | `--allowedTools RULE...` | Approval rules; does not remove other available tools. |
| Deny tools/calls | `--disallowedTools RULE...` | Enforced deny rules. |
| Select built-ins | `--tools LIST`; `--tools ""`; `--tools default` | Built-in inventory selection; MCP inventory is separate. |
| MCP selection | `--mcp-config FILE_OR_JSON`; `--strict-mcp-config` | Strict uses only supplied MCP configuration, subject to managed policy. |
| Settings | `--settings FILE_OR_JSON`; `--setting-sources user,project,local` | Per-launch overrides and source selection. |
| System guidance | `--system-prompt TEXT`; `--append-system-prompt TEXT` | Replace versus append; preserve native guidance deliberately. |
| Agents/plugins | `--agents JSON`; `--agent NAME`; repeat `--plugin-dir PATH`; `--plugin-url URL` | Native custom agents and per-session plugin sources. |
| Skills/browser/IDE | `--disable-slash-commands`; `--chrome` / `--no-chrome`; `--ide` | Help describes slash-command disabling as disabling skills. |
| Exact resume/fork | `--resume UUID`; `--fork-session` with resume/continue | Resume existing history versus create a new session from it. |
| Identity/persistence | `--session-id UUID`; `--name NAME`; `--no-session-persistence` | No persistence is print-only and cannot later resume. |
| Native background | `--background`; `attach`, `logs`, `stop`, `rm` commands | Native background-session lifecycle, separate from Tailterm tmux lifecycle. |
| Worktree/tmux | `--worktree`; `--tmux` | Native tmux requires worktree; avoid accidentally nesting lifecycle managers. |

### Permissions, sandbox, and tool availability

Claude deny rules take precedence over ask/allow. A bare tool denial can remove that tool from context; a scoped denial blocks matching calls while leaving other uses available. `CLAUDE.md` instructions are not enforcement. Current documentation says `/permissions` updates take effect on the next tool call, including during a running turn. [Claude permissions](https://code.claude.com/docs/en/permissions).

Bypass mode still respects deny rules and can retain explicit ask rules, user-interaction requests, managed restrictions, and special destructive-operation checks. Starting with `--allow-dangerously-skip-permissions` makes bypass available later; it does not activate it. `dontAsk` is not YOLO. [Permission modes](https://code.claude.com/docs/en/permission-modes).

Claude sandbox configuration (`sandbox.enabled` plus filesystem/network settings) is distinct from permission mode. Do not label `--dangerously-skip-permissions` as guaranteed OS-sandbox removal. [Claude sandboxing](https://code.claude.com/docs/en/sandboxing).

```sh
# Explicit Claude permission bypass for this launch.
cd '/absolute/project' && \
  claude --dangerously-skip-permissions 'Implement the assigned change.'

# Preapproval only: other tools remain available under normal policy.
claude --allowedTools 'Read' 'Bash(git status)' -- 'Review the working tree.'

# Built-in selection is a separate, deliberate restriction.
claude --tools 'Read,Grep,Glob' -- 'Inspect the repository without editing.'
```

### MCP selection without replacing the native agent

Claude supports stdio and HTTP MCP, with local/project/user configuration scopes. `--strict-mcp-config` provides an explicit per-launch selection; supplying an empty object disables configured MCP servers for that invocation. Managed policy can still constrain allowed servers. These options do not require stripping built-in tools or replacing the system prompt. [Claude MCP](https://code.claude.com/docs/en/mcp).

```sh
# Example adapter path only; no adapter is installed by this reference.
claude --strict-mcp-config \
  --mcp-config '{"mcpServers":{"tailterm":{"command":"/absolute/path/tailterm-mcp","args":[]}}}' \
  -- 'Review this project through the selected adapter.'

# No configured MCP servers for this invocation; native built-ins remain.
claude --strict-mcp-config --mcp-config '{"mcpServers":{}}' \
  -- 'Review the local repository.'
```

### Configuration, prompts, hooks, plugins, and subagents

Settings precedence is managed policy, CLI/explicit settings, project-local settings, project settings, then user settings. Some lists merge, so setting an allow list is not necessarily replacement of inherited entries. Environment precedence varies by variable. [Claude settings](https://code.claude.com/docs/en/settings).

Project `CLAUDE.md` and associated memory/rules supply instructions, not a sandbox. Tailterm should preserve normal discovery. [Claude memory](https://code.claude.com/docs/en/memory).

Installed `--system-prompt-snapshot on/off` controls reuse of the recorded system prompt on resume. Where recording is enabled, an existing snapshot can mean new prompt flags are ignored until compaction. Its help notes recording is not enabled everywhere. Documentation additionally lists `--system-prompt-file` and `--append-system-prompt-file`, absent from the inspected root help. Replacement discards native system guidance; append preserves it. [CLI reference](https://code.claude.com/docs/en/cli-reference).

Hooks can enforce pre-tool decisions or collect post-tool evidence and lifecycle events. Hook commands execute on the host; they are a native integration point for an evidence system without intercepting every tool through a proxy. [Claude hooks](https://code.claude.com/docs/en/hooks).

Native subagents have separate prompts, context, tool selection, and settings; custom definitions can be selected at launch. Skill preloading is not a universal tool allow list. Tailterm must explicitly account for native children if it wants one shared helper limit. [Claude subagents](https://code.claude.com/docs/en/sub-agents).

Three installed flags intentionally remove native capabilities and should not become incidental Tailterm defaults:

- `--safe-mode` disables custom instructions, skills, plugins, hooks, MCP, and custom agents, among other customizations.
- `--bare` skips several initialization/customization mechanisms, including automatic CLAUDE.md discovery, hooks, plugin synchronization, and keychain lookup; it has explicit authentication requirements.
- `--restricted` confines access, removes execution tools and WebFetch unless explicitly selected, ignores ordinary settings sources, and refuses bypass mode.

These are help-advertised behaviors, not tested isolation guarantees. Similarly, changing `--setting-sources` can suppress useful native project/user configuration.

### Environment, budgets, timeouts, and structured output

The following environment controls are documentation-verified. They are launch-process controls, not ordinary messages. [Claude environment reference](https://code.claude.com/docs/en/env-vars).

| Variable | Meaning/caveat |
| --- | --- |
| `ANTHROPIC_MODEL` | Default model; CLI and interactive model selection override it. |
| `CLAUDE_CODE_EFFORT_LEVEL` | Overrides effort flags, interactive selection, and settings; supports `auto` as well as explicit levels. |
| `MAX_THINKING_TOKENS` | Fixed thinking budget on compatible models; adaptive thinking differs. |
| `CLAUDE_CODE_MAX_OUTPUT_TOKENS` | Per-request output cap, not total task spend. |
| `API_TIMEOUT_MS` | API-request timeout. |
| `BASH_DEFAULT_TIMEOUT_MS`, `BASH_MAX_TIMEOUT_MS` | Bash execution defaults/ceiling. |
| `MCP_TIMEOUT`, `MCP_TOOL_TIMEOUT` | MCP startup and execution limits; idle timeout is separately configurable. |
| `CLAUDE_CODE_MCP_ALLOWLIST_ENV` | Can limit environment inherited by local MCP subprocesses. |

Installed print mode has `--max-budget-usd`, `--output-format text/json/stream-json`, `--input-format text/stream-json`, `--json-schema JSON`, and streaming partial/hook/subagent event flags. `--permission-prompts none` denies requests that would prompt; it does not grant permissions. `--autocompact` controls context compaction rather than spend.

**Documentation-only:** `--max-turns N` bounds print-mode agent turns; without it the documented default is unlimited. This is not a wall-clock deadline, and queued streaming inputs have their own turn accounting. [CLI reference](https://code.claude.com/docs/en/cli-reference).

```sh
claude --print --model 'MODEL_ID' --effort high \
  --max-budget-usd 2 --output-format json \
  'Summarize the repository.'

claude --resume '00000000-0000-0000-0000-000000000000' \
  'Continue the assignment.'
claude --resume '00000000-0000-0000-0000-000000000000' \
  --fork-session 'Explore an alternative.'
```

Model/effort support varies by model and account. Do not infer entitlement from a dropdown or treat every effort value as supported by every model. [Claude model configuration](https://code.claude.com/docs/en/model-config).

## What can change after launch?

| Control | Ordinary message | Native change path / limitation |
| --- | --- | --- |
| Assignment, priorities, acceptance criteria | Yes, as user guidance | Normal turn/steering; preserve task authorization and identity. |
| Model/reasoning | Request only | Codex `/model` or supported native options; Claude `/model` and `/effort`, subject to environment overrides. |
| Permission rules/mode | Cannot enforce by text | Native permission UI/commands; Claude bypass must have been enabled at launch. |
| Tool/MCP availability | Can request use/avoidance | Native MCP/configuration controls; some changes need reconnect or relaunch. A request is not a verified inventory update. |
| System/developer prompt | Cannot elevate message role | Native configuration/API; Claude resume snapshots complicate prompt replacement. |
| Process environment/cwd | Can ask for shell operations | Child shell changes do not change parent launch environment or global instruction discovery. |
| Budget/deadline | Can request a stopping rule | Enforced native budget or external supervisor required for a hard limit. |
| Resume/fork | Text does not change identity | Explicit native command with recorded session ID; register the resulting Tailterm run. |

Codex documents native `/model`, `/permissions`, `/mcp`, `/hooks`, `/skills`, `/agent`, and `/status` controls. Claude exposes interactive model/effort/permission/MCP/configuration/session commands. Enter them through the native UI or a supported control API, not as a presumed side effect of a board post. [Codex command reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli), [Claude interactive mode](https://code.claude.com/docs/en/interactive-mode).

Installed Codex queue accepts additional native option flags alongside the message. That is a possible future integration surface, but the effect and persistence of changing controls on an already-running exact thread still need isolated testing. Tailterm's current relay does not expose those extra options as board-message controls.

## Tailterm's current surface and gaps

This section describes source-observed behavior, not a proposed feature already delivered.

| Tailterm control | Current native mapping |
| --- | --- |
| Runtime, command, machine/folder, prompt, role | Launches the chosen CLI through SSH/local spawn and tmux with Tailterm environment/briefing. |
| Model | Adds `--model` for Codex, Claude, Aider, Gemini when explicitly selected; omission preserves host choice. |
| Codex Host settings | Adds no permission preset. |
| Codex on-request | `--ask-for-approval on-request`; inherits sandbox selection. |
| Codex workspace-auto | `--sandbox workspace-write --ask-for-approval never -c sandbox_workspace_write.network_access=true`, plus relay directory access; requires absolute cwd. |
| Codex full-auto | `--sandbox danger-full-access --ask-for-approval never`; this is a Tailterm label, not the absent native `--full-auto` flag. |
| Claude permission preset | Adds `--permission-mode acceptEdits/auto/dontAsk/bypassPermissions`, or nothing for Host settings. |
| Claude Allowed tools | Repeated `--allowedTools` approval rules; maximum 30 rules, 200 characters each in current validation. |
| Briefing and assignment | Appends a user prompt for supported runtimes; does not install a system/developer prompt. Generic commands handle their prompt differently. |

Sources: [permission choices](../../shared/agent-permissions.js), [permission arguments](../../hub/cmd/tt/permissions.go), [model arguments](../../hub/cmd/tt/model.go), [agent controls](../../client/agent-controls.js), [launch command construction](../../shared/tmux-command.js), [spawn and briefing](../../hub/cmd/tt/main.go), [team templates](../../client/teams.js), [model picker](../../client/model-picker.js).

Current gaps: no first-class reasoning effort, MCP include/exclude/configuration, built-in tool removal, deny-rule editor, independent network policy, system/developer instruction layer, hook/plugin/native-subagent selection, or run budget/deadline fields. Native CLI configuration can provide many of these today, but the UI does not report them as verified effective per-thread settings.

A custom launch command can carry native flags. Conflicting explicit permission flags and a Tailterm permission preset are rejected, so select Host settings when the custom command owns permissions. Avoid assuming arbitrary flags propagate through every recovery/handler path: the project-handler planner reconstructs supported runtime commands and explicitly inherits selected fields. [Handler planning](../../client/project-handler.js).

Editing a team/template changes future launches, not a running process. `tt resume` restores Tailterm roster eligibility; it is not the same operation as `codex resume` or `claude --resume`. Retiring a worker, closing a task, terminating a process, and forking a native conversation remain distinct.

### Recommended implementation order

1. Preserve native defaults and show **Host settings** distinctly from explicit overrides. Add runtime-version capability detection and a reviewable resolved launch summary, with secret values omitted.
2. Expose model/effort, approval mode, and sandbox/network separately; label Claude approval rules accurately. Add per-launch MCP selection, accounting for Codex plugin servers and Claude strict configuration.
3. Keep Tailterm assignments additive. Introduce system/developer overrides only as explicit advanced controls with clear replacement semantics.
4. Record requested launch controls and observed exact-thread state separately. Test resume/fork/control changes with isolated sessions before promising mutation of running agents.
5. Add budgets and native-child accounting only with explicit lifecycle semantics. A timeout must produce an interrupted outcome, not a fabricated completion.

These are design recommendations, not implementation commitments. Remaining dependencies are the owner's preferred initial control subset, isolated compatibility tests on each host/runtime, and a decision about whether native subagents count as individual Tailterm workers.

## Other Tailterm runtimes: short appendix

Tailterm discovers more executable names than it has first-class model/permission integrations. Discovery alone does not establish working authentication, MCP support, or session-resume semantics. [Runtime discovery](../../hub/internal/spawn/spawn.go).

| Runtime | Evidence and useful controls |
| --- | --- |
| Gemini CLI | **Installed 0.53.1 help:** `--model`, `--prompt`, `--prompt-interactive`, `--sandbox`, `--yolo`, `--approval-mode default/auto_edit/yolo/plan`, `--policy`, `--admin-policy`, `--allowed-mcp-server-names`, `--extensions`, `--resume`, `--session-file`, `--session-id`, `--include-directories`, `--output-format`. `--allowed-tools` is deprecated in favor of policy. Native hooks/skills/MCP commands are present. |
| Aider | **Not installed here; docs only.** `--model`, `--reasoning-effort`, `--thinking-tokens`, `--yes-always`, `--config`, `--env-file`, `--timeout`, history controls. Confirmation bypass is not an OS sandbox. Same MCP contract as Codex/Claude was not established. |
| OpenCode, Goose, Cursor agent, Amp, GitHub Copilot CLI | **Not found on this host PATH.** Recognized by discovery as generic runtimes; no installed-version or live behavior verification. Do not reuse Codex/Claude flags automatically. |

Gemini configuration also documents `GEMINI_SANDBOX`, `GEMINI_SYSTEM_MD` for system-prompt replacement, and project `GEMINI.md` guidance. [Gemini configuration](https://geminicli.com/docs/reference/configuration/). Aider's history/context caps and API timeout are not total task budgets. [Aider options](https://aider.chat/docs/config/options.html), [Aider configuration](https://aider.chat/docs/config.html).

Vendor starting points for later host-specific verification: [OpenCode CLI](https://opencode.ai/docs/cli/), [Cursor CLI parameters](https://cursor.com/docs/cli/reference/parameters), [Amp manual](https://ampcode.com/docs), [Copilot CLI reference](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-command-reference). Goose's documentation fetch did not succeed in this research pass; its flags remain unverified.

## Verification boundary

This is a source/help research artifact. Shell examples were syntax-checked without execution; embedded JSON and relative source links were checked. No native agent run, authentication, MCP connection, destructive mode, mutable-session transition, or spend/deadline enforcement was exercised. The AIV proposal and unrelated working-tree files remain unchanged by this assignment.
