# Runtime guide

Use this guide to choose an extension, configure it, and understand when changes take effect. For installation and a first conversation, start with the [README](../README.md). The CLI and public worker API are experimental; the `v1` protocol name is not a general stability promise.

## Configuration and trust

Personal settings live in `~/.go-cli/config.yaml`. Project settings use `.go-cli.yaml` or `.go-cli.json` in the project root. The CLI starts from personal/global configuration and environment settings, and merges captured project configuration only after source verification succeeds.

From an interactive terminal inside the project:

```sh
./bin/go-cli trust grant
# Review the captured source summary, then type TRUST.
./bin/go-cli trust list
# Start again to verify and activate the grant:
./bin/go-cli interactive
# Revoke using the canonical path shown by trust list:
./bin/go-cli trust revoke /absolute/project/root
```

`trust grant` takes no path argument. A grant binds captured source content; it is not a blanket execution permission for future files. When sources change, review the new snapshot and grant it again if appropriate. Same-source Skill/Hook edits can be reauthorized and adopted through supported idle `/reload`; incompatible active source identities or changed authorization bindings of existing connections can require restart. Without verified project sources, project configuration and extensions are not applied; personal configuration remains usable.

Source trust does not replace tool approval, Plan Mode, or execution policy. A child agent and a discovered MCP tool still go through their own normal execution checks.

## Choose an extension by what you need

| Need | Extension | Loading | Execution | Update |
|---|---|---|---|---|
| Reusable task instructions | Skill files | Startup; metadata first when progressive | Instructions materialize/use on demand in progressive mode | Supported dynamic `/reload`, otherwise restart |
| Personal/team instructions | Agent prompt settings | Agent assembly | Included in model requests | Supported dynamic `/reload`, otherwise restart |
| A slash prompt you invoke | Markdown command | Interactive command-table initialization | When you type `/name` | Restart |
| Shell action at a runtime event | Configured Shell Hook | Startup registration | At its configured event | Supported dynamic `/reload`, otherwise restart |
| Tools from another program/service | MCP | Default eager connection, or lazy selection | Approved remote tool call | Lazy load/reload; some source changes need restart |
| Features from a separate executable | JSONL worker | Process start and initialize at assembly | Worker protocol method when invoked, subject to host support | Restart |
| Change the host implementation | Go code compiled into the CLI | Build and assembly | Direct calls in process | Rebuild and restart |
| Load Go code at runtime | Optional native `.so` | Startup plugin loading | Calls in the host process | Restart; no unload |

Existing Markdown/YAML Skills and ordinary configuration remain supported. Neither a worker nor a native plugin is required to add instructions, prompts, commands, or Shell Hooks. Internal typed Go interfaces are implementation extension points, not a public embedding SDK.

## Model settings and working rules

Select an OpenAI-compatible endpoint with `provider.name: eino`, `provider.base_url`, and `provider.api_key` (prefer the API-key environment variable shown in the README). Set `model.name` for the model, and use `model.temperature` and `model.max_tokens` for sampling and output limits. These are provider-dependent controls, not a guarantee that every model accepts every value. `agent.thinking` selects the supported reasoning level; `agent.max_iterations` bounds the Agent loop, defaulting to 200 (`-1` removes that bound).

For example, personal configuration can add a rule without replacing the normal assistant prompt:

```yaml
model:
  name: your-model-id
  temperature: 0.2
  max_tokens: 4096
agent:
  append_system_prompt: "Explain meaningful changes before editing."
  max_iterations: 100
```

Provider/model setup loads at startup. Prompt-only changes can use the supported dynamic refresh path described below. Prompt rules guide the model; approval and execution policy enforce action restrictions.

## Skills, prompts, and custom commands

### Skill files

The default Skill directory is the **first existing** directory in this order:

1. `.go-cli/skills`
2. `~/.config/go-cli/skills`

Use `skill.dir` to choose another supported directory. Discovery does not merge both defaults. The regular loader accepts flat Markdown (`review.md`, including `.skill.md`), YAML (`.yaml`/`.yml`), and nested `review/SKILL.md` definitions.

A standard `.go-cli/skills/review/SKILL.md`:

```markdown
---
name: review
description: Review a change for correctness and missing tests.
---
Read the diff, explain concrete risks, and suggest focused checks.
```

A legacy flat YAML definition remains valid, for example `.go-cli/skills/review.yaml`:

```yaml
name: review
description: Review a change for correctness and missing tests.
prompt: Read the diff and explain concrete risks.
```

Choose one definition per Skill name. Regular loading reads full definitions and registers Skill tools at startup. Opt in to progressive loading to index metadata first and materialize the selected instructions on demand:

```yaml
skill:
  dir: .go-cli/skills
  progressive: true
```

`runtime.profile: edge` also enables progressive Skills. Project sources still undergo bounded content capture for trust checks; metadata-first exposure does not mean zero Skill body reads at startup. Discovery or loading a Skill does not itself grant permission to execute the tools described by it.

### Prompt customization

For ordinary preferences, append to the normal assistant instructions:

```yaml
agent:
  append_system_prompt: "Explain meaningful changes before editing."
```

`agent.system_prompt` replaces the base system prompt. It is useful when you intentionally want to define that base yourself. There is no automatic `SYSTEM.md` discovery. Prompt settings load during assembly; supported dynamic refresh rebuilds them for later requests.

### Markdown slash commands

The default command directory is `.go-cli/commands`, falling back to `~/.config/go-cli/commands`; `commands.dir` overrides it. A file `.go-cli/commands/review.md` can contain:

```markdown
---
name: review
description: Review selected changes.
---
Review the changes described below and explain concrete risks.
```

Type `/review the authentication changes` to submit the Markdown body followed by a blank line and `the authentication changes`. Trailing arguments are joined with spaces and appended; the command does not interpolate placeholders. These files store reusable prompts, not standalone shell commands. They load when the interactive command table is built; edit them and restart to refresh the table. Project command files require source trust.

## Configured command tools

Use `tools.custom_tools` when an existing executable can do the job. This avoids writing a plugin or worker protocol. The executable receives the model's `input` as **one literal argument**, appended after configured command arguments and `args`; it is not automatically expanded by a shell.

For a project tool, create an executable `scripts/inspect-project` inside the workspace, then add this to `.go-cli.yaml`:

```yaml
tools:
  custom_tools:
    - name: inspect_project
      description: Inspect a project using the supplied request.
      command:
        - ./scripts/inspect-project
      args:
        - ./assets/schema.json
      env:
        INSPECT_MODE: "summary"
        OPTIONAL_PREFIX: ""
      working_dir: .
      timeout: 20
```

Replace the script and asset with your own files, and make the script executable before granting project trust. This example uses block lists and a string-valued environment map, supported by the configuration loader. `timeout` is in seconds; zero means no configured command timeout. `working_dir` defaults to the host working directory. Only `PATH`, `HOME`, `USER`, `SHELL`, `LANG`, `LC_ALL`, `LC_CTYPE`, and `TERM` are inherited. Non-sensitive configured environment entries overlay that allowed set. Configured names ending in `_KEY`, `_TOKEN`, `_SECRET`, `_PASSWORD`, or `_CREDENTIAL` are filtered case-insensitively (including `_API_KEY`), so values such as `API_TOKEN` are not passed to the command. Quote values such as numbers or booleans when they should be strings.

Custom tools register at startup and require a trusted project even when configured personally. Project-owned entrypoint bytes are captured and executed from a private host path, so later replacement at the original path cannot silently replace the reviewed executable. Consequently `$0`, `__file__`, or sibling lookup relative to the executable can refer to that private path. Pass assets explicitly and resolve them from the working directory, as above. Interpreters, dependencies, and assets are not recursively pinned. Personal configuration can use an installed executable at its normal path.

The CLI's managed command implementation supports persistent execution on Darwin/Linux, with normal approval, Plan Mode, source trust, and cancellation checks. Other platforms retain ordinary command execution but do not automatically gain the same persistent completion support. Change the command definition, environment, executable, or arguments and restart; `/reload` does not replace these tools.

## Shell Hooks

Shell Hooks receive event JSON on stdin. Use them for local workflow integration, such as recording a completed tool event. In the **main configuration**, the nested keys are intentional:

```yaml
hooks:
  hooks:
    - event: post_tool_use
      command: "cat >> .go-cli/tool-events.jsonl"
      timeout: 5s
```

Create the destination directory before using this example. In the separate **project file** `.go-cli/hooks.yaml`, the format instead starts with the Hook array key:

```yaml
hooks:
  - event: post_tool_use
    command: "cat >> .go-cli/tool-events.jsonl"
    timeout: 5s
```

The project file is captured and verified under project source trust. Capture does not recursively pin the interpreters, scripts, dependencies, or assets invoked by a shell command. `command` is a shell command string. `timeout` is a Go duration such as `5s`; the default is `10s`.

| Event | When it fires | Important behavior |
|---|---|---|
| `session_start` | Runtime turn-start callback | Despite the name, this is a turn lifecycle callback, not a once-only persistent-session initializer |
| `pre_tool_use` | Before the source tool is dispatched | Exit zero allows, nonzero denies; ordinary startup errors and timeouts can fail open |
| `post_tool_use` | After the original tool group has finished, for tool calls with known execution results | Separate managed Bash action with its own checks and record |
| `session_end` | Runtime turn-end callback | Not a once-only persistent-session shutdown callback |

Post-tool Shell Hooks use the same explicit runtime action path in ordinary and durable runs. The runtime appends a synthetic assistant/Bash call after the original batch, checks approval, Plan Mode and current source trust independently, and records the action in a durable session when enabled. The UI emits separate `tool_call` and `tool_result` events for the Hook. The JSON input carries `event`, `tool`, `args`, and `result`, including available structured result metadata; it travels over stdin rather than shell interpolation.

A source tool that was not dispatched, or whose outcome is unknown, does not trigger a post-tool Shell Hook. A known tool error can still be observed. Hook failure or timeout does not rerun the original tool. For post-tool Hooks, waiting for approval does not consume the configured execution timeout; actual execution uses the shorter of that timeout and the host Bash execution limit, while parent cancellation still applies, and pre-tool/session Hook timeout semantics are unchanged. Post-tool Hooks require Bash to be available to the current agent. A restricted child without Bash cannot run them.

Pre-tool Hooks are useful workflow checks, but ordinary launch failures or timeouts may allow the tool to continue. Enforce mandatory restrictions with approval and execution policy. Supported Hook changes can be refreshed at idle in dynamic mode.

## MCP setup and execution

MCP connects tools from a separate program or service. Integration is tools-only: Stdio, SSE, and StreamableHTTP transports are supported, but this path does not expose the full resources or prompts surface. Tool names become `mcp__{server}__{tool}`.

For an HTTP service, put this in personal configuration and replace the example URL:

```yaml
mcp:
  loading: lazy
  servers:
    - name: docs
      url: https://your-mcp-service.example/mcp
```

For a local server, personal configuration may name an installed executable:

```yaml
mcp:
  loading: eager
  servers:
    - name: docs
      command: /absolute/path/to/docs-mcp
```

For a **project-owned stdio server**, the entrypoint must be an executable source that can be captured inside the workspace. For example, `.go-cli.yaml` can name an existing executable workspace wrapper:

```yaml
mcp:
  loading: lazy
  servers:
    - name: docs
      command: ./scripts/docs-mcp
```

Grant project trust after creating the wrapper. An outside binary such as `/opt/...` or an externally resolved `npx` command in project configuration can fail source capture. Use personal configuration for external installed commands, or an explicit workspace wrapper. Capturing a wrapper does not pin every interpreter, dependency, or asset it invokes.

### Loading is not execution

- **Default `eager`:** connect configured servers and fetch their tool lists during startup.
- **Opt-in `lazy`:** read server descriptors without connecting every server. `runtime.profile: edge` also selects lazy behavior.
- **`mcp_list`:** inspect local metadata and states without connecting. `query` filters names.
- **`mcp_discover`:** pass `{"server":"docs"}` to activate one configured server. Its schemas are available on the next model request. This can happen during an agent run, and does not grant tool execution authority.
- **`/mcp load docs`:** activate a server from the interactive command surface when idle.
- **`/mcp reload docs`:** replace that server's catalog at idle in lazy mode.
- **`/reload`:** refresh supported source descriptors at idle; it is not a remote tool call.

Actual MCP tool calls pass normal prepared dispatch, trust, approval, and Plan Mode checks. The CLI's built-in HTTP transports and its owned stdio process transport have reviewed request-completion contracts, allowing real `tools/call` execution in durable sessions. Generic adapters, substituted transports, or wrappers do not acquire that contract merely by implementing the same interface.

A valid `CallToolResult` establishes a known result, including a valid result with `isError: true`. A handshake, visible schema, closed channel, or arbitrary JSON response does not. Lost connections, timeouts, malformed responses, and unproved cleanup after dispatch retain an unknown outcome. The host does not automatically replay the call; inspect `/session pending` and the remote service before deciding how to proceed.

## Delegate work to child agents

`dispatch_subagent` is a synchronous model tool. Ask the assistant to delegate a focused task, or supply these arguments through a tool-capable client:

```json
{
  "prompt": "Review the authentication changes and report concrete risks.",
  "role": "reviewer",
  "tools": ["read", "grep"],
  "max_turns": 8,
  "context_mode": "independent"
}
```

Optional `model` chooses a different model; otherwise the child uses the parent model. Supported roles are `researcher`, `implementer`, `reviewer`, and `tester`. An explicit `system_prompt` overrides the role template. `tools` restricts the child tool list; otherwise the role's defaults apply. `max_turns` bounds the loop.

For parallel work, supply a tasks array without a top-level prompt:

```json
{
  "parallel": true,
  "tasks": [
    {"prompt": "Inspect authentication risks.", "role": "reviewer", "context_mode": "inherit"},
    {"prompt": "Identify missing input validation tests.", "role": "tester", "max_turns": 6}
  ]
}
```

`context_mode` defaults to `independent`. `inherit` copies only the parent conversation available immediately before dispatch, as reference data in the task prompt. It does not share a live conversation, copy execution records, or transfer authority. There is no `--subagent-context` flag or global subagent context setting.

Every managed child has a separately assembled runtime with its own run owner, registry, approvals, and close lifecycle. Parent lineage is audit data. Normal configuration and constraints are applied again, and a parent Plan Mode restriction cannot be escaped through delegation. An explicit whitelist such as `mcp__docs__search` can cause the child's own MCP connection to activate before the run; it does not reuse the parent's MCP client or permit.

With `session.store_path` enabled, each child has its own physical session. Single and parallel text results display the nonempty child session IDs; structured metadata retains `physical_session_id` for a single result and `physical_session_ids` for parallel results. Resume a child using its exact ID:

```sh
./bin/go-cli interactive --resume-session <child-physical-session-id>
```

The current user-facing tool waits for results and resource cleanup. Core `Send`, `Interrupt`, and `Wait` methods are lifecycle interfaces, not a promised background-agent UI or set of interactive management commands. Parent cancellation propagates and joins child resources. An external effect whose completion cannot be established stays unknown; cancellation does not manufacture a successful completion.

## JSONL extension workers

Use a worker for a separate executable that contributes tools, existing Hook events, commands, prompt material, or complete-response providers. Put a local descriptor in `extensions.plugin_paths`:

```yaml
extensions:
  enabled: true
  plugin_paths:
    - ./plugins/helper.rpc.json
```

The `.rpc.json` descriptor names a command and pins the executable bytes with SHA-256. See the [public worker API](../pkg/extensionapi/v1/README.md) for its full fields and a minimal descriptor. During assembly the host starts the process, sends `initialize` with version `1.0`, validates declarations, and registers accepted contributions before starting the agent. Change the worker, descriptor, or declarations and restart.

The public package imports only the standard library. Workers may use any language that implements the JSONL contract. Providers return complete responses; the Stream adapter emits a final response, not token deltas. RPC Skill DTO negotiation is unsupported; use standard Skill files instead.

Registration and persistent execution are separate capabilities. The CLI's host-owned JSONL process adapter supports tool calls in persistent sessions: a valid response matched to the request establishes a known request result, including a known tool error. It does not prove every remote side effect succeeded, mean the worker exited, or grant permission to replay. The worker can remain alive for later requests.

Malformed replies, a lost process/connection, partial writes, or cancellation after dispatch can leave the result unknown. Such calls remain pending until inspected through the existing session recovery path. The host owns process and reader cleanup; a later cleanup does not turn an unknown result into success. Darwin/Linux have runtime process-ownership coverage; Windows has cross-build coverage, not equivalent runtime verification. Generic adapters, arbitrary native callbacks, or a tool with the same name do not acquire completion support merely by declaring it.

Workers share the user's OS permissions. Process separation imposes no filesystem, network, CPU, or RSS sandbox. Hash pinning checks entrypoint bytes, not the publisher's identity or separate interpreters and assets. Workers must reserve stdout for protocol frames and honor deadlines. The host does not automatically replay failed calls; loss after dispatch may leave the external outcome unknown.

## Choose a build

`headless` and `no_plugin` are independent Go build tags. Neither selects the runtime profile.

| Build | Tags | Terminal renderer | Optional native Go `.so` loader |
|---|---|---|---|
| Full | none | Rich TUI | Available |
| Full without native plugins | `no_plugin` | Rich TUI | Disabled |
| Headless | `headless` | Line-oriented event consumer | Available |
| Headless without native plugins | `headless,no_plugin` | Line-oriented event consumer | Disabled |

```sh
go build -o ./bin/go-cli ./cmd/cli
go build -tags no_plugin -o ./bin/go-cli-no-plugin ./cmd/cli
go build -tags headless -o ./bin/go-cli-headless ./cmd/cli
go build -tags headless,no_plugin -o ./bin/go-cli-headless-no-plugin ./cmd/cli
```

Headless retains the shared REPL, multi-turn input, final responses, SIGINT cancellation, and synchronous text approval. Fullscreen layout, themes, modal approval, and in-flight keyboard steering require the rich UI. File extensions, MCP, and workers are shared across variants with their own capability limits.

A Linux ARM64 cross-build can use the same entry point:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
  -trimpath -tags headless,no_plugin \
  -o ./bin/go-cli-linux-arm64 ./cmd/cli
```

A cross-build establishes compilation for that target, not execution or resource behavior on a physical device. The [Makefile](../Makefile) target `test-no-plugin` covers the extension/CLI stub packages, not every package or the combined headless build.

### Optional native Go plugins

Compiled-in Go tools and Hooks already use direct in-process calls and do not require `.so` files. They are often simpler when changing and rebuilding the host is acceptable. Native Go plugins provide **runtime loading of Go code**, not an automatic performance advantage over compiled-in code. Prefer ordinary file/configuration or portable process-based extensions unless dynamic Go loading is required and a measured benefit justifies it.

Native loading is most useful in a controlled toolchain for trusted Go code that needs low communication overhead or direct registration through host interfaces. The optional loader accepts local shared objects exposing a `New` or `LoadExtensions` factory with signature `func() ([]Extension, error)`. A native path uses the same `extensions.enabled` and `extensions.plugin_paths` configuration. Native updates require restart; Go plugins cannot be unloaded. Loading an arbitrary `.so` or callback does not establish the completion contract required for persistent tool execution; ordinary execution and persistent completion support remain separate.

Go supports this mechanism on Linux, FreeBSD, and macOS. The host and plugin need matching Go toolchains, build flags, and common dependency versions. Code runs with host privileges, and faults can affect the entire process. Consult the [official Go plugin documentation](https://pkg.go.dev/plugin) before choosing it.

`no_plugin` removes this native loader only. It does not remove file extensions, MCP, or JSONL workers, nor does it expand their execution support. HTTP(S) extension descriptors are rejected, and `grpc://` native-loader paths are unsupported; an MCP URL is a different integration.

## Build tags and runtime profile are separate axes

`runtime.profile` is configuration. Both `default` and `edge` can be paired with any build above, including `no_plugin`.

```yaml
runtime:
  profile: edge
  # Optional positive overrides:
  # max_skills: 128
  # max_mcp_servers: 2
  # max_prompt_bytes: 131072
```

Edge enables dynamic capabilities, progressive Skills, and lazy MCP. Zero or omitted limits use defaults of **128 Skills, 2 active MCP servers, and 128 KiB** of capability prompt/schema budget. Positive explicit values override them. It changes loading and resource policy, not which packages are linked into the executable.

To enable refresh without choosing edge, set `runtime.dynamic: true`. Opting into `skill.progressive: true` or `mcp.loading: lazy` also enables dynamic operation. Use regular loading and restart if dynamic refresh is unnecessary.

## Refresh and restart

Dynamic refresh prepares and validates a complete replacement while idle. If loading fails, the last valid set remains available. A running task keeps the capability descriptions it started with, while each action still checks current trust and policy; revoked permissions cannot survive through an old description. Skill catalogs also retain preprocessed search text to avoid repeated normalization during lookup. This trades extra retained metadata for less repeated computation and temporary allocation; multiple active catalog generations can temporarily retain multiple copies.

| Change | Apply through |
|---|---|
| Supported Skill, agent prompt, and configured Hook changes in dynamic mode | `/reload` when idle |
| Lazy MCP activation | `mcp_discover` during a run, or `/mcp load <server>` when idle |
| Lazy MCP catalog replacement | `/mcp reload <server>` when idle |
| Markdown command files and configured command tools | Restart |
| Worker descriptor, executable, or declarations | Restart |
| Compiled-in Go/native plugin code | Rebuild as needed, then restart |
| Provider/session setup, runtime profile, loading mode, active MCP limits | Restart |
| Eager MCP sources in dynamic mode | Restart |
| Reauthorized same-source Skill/Hook changes | Supported idle `/reload`; follow any candidate restart requirement |
| Incompatible active source identity or existing-connection authorization bindings | Restart and required trust review |

Refresh does not roll back completed external side effects. A late callback, cancellation, or lost response can leave an outcome unknown. Inspect durable pending actions before repeating an operation.

## Sessions and recovery

Set `session.store_path` to save conversation and execution records. An absolute path gives a stable store location across working directories. Saving is automatic; `/save` acknowledges already synced records, while `/load` inspects persisted records. Neither resolves an unknown action or replays a tool.

A **physical session ID** names one saved file. An **entry ID** identifies a history position inside it, and a **branch ID** names an alternative conversation path within the same physical session. Fork and clone create conversation branches, not copied workspaces or independent physical files.

```sh
./bin/go-cli session list --store .go-cli/sessions
./bin/go-cli interactive --resume-session <physical-session-id>
# Choose the latest saved session only if its catalog status is ready:
./bin/go-cli interactive --resume
# Fork at an exact entry in a named saved session, then ask a new question:
./bin/go-cli prompt --resume-session <physical-session-id> --fork <entry-id> "Try another approach"
```

Bare `prompt --fork <entry-id>` can locate its source only when the entry is unique and the relevant catalog is fully inspectable; busy or ambiguous sources are rejected. `--resume-session` is the unambiguous physical-file selection. Interactive `--resume` and `--resume-session` cannot be combined.

While idle in an interactive conversation:

| Command | Effect |
|---|---|
| `/session tree`, `/session branches` | Inspect entries and branches |
| `/session resume` or `/session list` | List physical sessions without opening busy files |
| `/session resume <physical-id>` | Open an exact saved physical session |
| `/session entry <entry-id>` | Select a position inside the current physical session |
| `/session fork feature` | Branch at the selected history point |
| `/session clone feature experiment` | Create and select a new branch from an existing branch tip; an entry ID can replace `feature` |
| `/session switch main` | Select that branch's latest tip |
| `/clear` | Select a new physical session, retaining the previous file |
| `/retry` | Resubmit the latest genuine user prompt from a successfully completed run on a fresh branch (or new file for an empty prefix) |
| `/compact` | Use the configured compactor to reduce older context, retaining the required recent suffix |

For compatibility, `/session resume <current-entry-id>` also selects an entry in the current file; prefer `/session entry` (or `/session resume --entry <id>`) to make that intent clear. `@session:<entry-id>` quotes a visible transcript entry from the selected physical session, including another branch. It does not navigate, execute recorded tools, or expose execution-only journal entries.

`/retry` preserves previous effects and uses fresh execution identities; it is not rollback. `/compact` may make an auxiliary model request when summary compaction is configured, preferring the configured small model. It reports when the conversation already fits. Stored summaries are historical context, not new instructions or execution authority. History operations cannot hide an unresolved run or bypass its pending-action checks. Session snapshots and general schema migration remain unsupported.

For persistent sessions, `compaction.strategy: truncating` replaces an older prefix in the model context with a fixed omission notice, then retains an exact, unchanged recent suffix. It makes no factual summary; the complete older records remain saved. The default `micro_first`/`unified` strategy can use this same truncation when no summarizer is configured, or when the built-in summarizer's own local sizing checks determine that its summary cannot fit. Without a summarizer there is no summary-model request; the truncation fallback itself never requests another summary. Provider errors, cancellation, denied requests, unresolved resource cleanup, and persistence failures still return errors rather than trigger fallback. If the budget cannot accommodate the omission notice while preserving required system instructions, the current input, and complete tool exchanges, compaction fails explicitly. Choose summary compaction when you want a semantic account of older context, or truncation when an honest omission is sufficient.

### Git-aware conversation branches

Ordinary navigation does not change Git branches. To explicitly link a new conversation branch to Git:

```text
/session fork feature --git topic
/session switch feature --git
```

The CLI performs the Git action through its normal managed tool path, with approval, Plan Mode, Hooks, and execution records, without asking the model. The session selection changes only after known successful Git completion. These are two operations rather than one atomic transaction: if Git succeeds but session selection fails, the CLI reports partial completion and does not automatically repeat or roll back Git. `session.git_aware_branch: true` can enable linked Git checkout on later switches; it does not invent links for ordinary branches.

### Import old JSONL explicitly

Old session files are never silently treated as current execution receipts. Inspect first, then import into a new physical file:

```sh
./bin/go-cli session inspect-legacy /absolute/path/old-session.jsonl
./bin/go-cli session import-legacy \
  --source /absolute/path/old-session.jsonl \
  --store .go-cli/sessions
```

Use the returned physical ID with `interactive --resume-session`. Optional `--id <new-physical-id>` chooses the destination name. Import is strict: unknown fields/versions, malformed or partial rows, invalid branches, and unclosed tool groups are rejected. The original file is retained, and an existing destination is not overwritten. Imported closed tool exchanges are conversation history, not proof that this host performed or approved them.

### Unknown outcomes

A tool timeout, cancellation, or broken transport can leave an external outcome unknown. New turns and history mutations remain blocked where the session has unresolved execution. `/session pending` shows the affected records and available recovery instructions. Do not repeat an operation merely because its response was lost. `/clear`, fork, clone, import, or a later process cleanup does not manufacture completion or clear workspace quarantine.

## Project memory

Automatic memory is **enabled by default**, independently of `session.store_path`. After a successfully completed root conversation turn, a background model request can extract explicitly stated project facts and preferences. Later root turns in the same canonical project, including in another process, receive a bounded snapshot of its scoped memories as reference context. Memories are not higher-priority instructions and do not authorize tool actions.

```yaml
memory:
  enabled: false
```

This disables both automatic extraction and prompt injection; explicit `/memory` commands remain available. The setting loads at startup. With automatic memory enabled, the default file is `~/.go-cli/memories/<workspace-id>.jsonl`. `memory.store_path` can select a file; records in a shared explicit file still retain workspace attribution. Disabling automatic memory uses the manual store path (default `~/.go-cli/memories.jsonl`), so choose an explicit path if you want to inspect a particular existing store.

```text
/memory list
/memory add Prefer focused changes and explain their purpose.
/memory search focused
/memory delete <memory-id>
```

Automatic extraction uses `small_model` when configured, otherwise the primary model. This adds model requests, tokens, and possible cost. One extraction runs at a time with only the latest pending turn retained; under load an older pending turn can be skipped. Normal prompt exit or interactive EOF allows up to 30 seconds for graceful completion before cancellation and cleanup. Cancellation or the deadline can discard unfinished extraction. This is best-effort memory, not a guarantee to store every turn.

Only completed user/assistant text is eligible. Tool output, historical imports, compaction summaries, failed/interrupted turns, child agents, and Plan Mode are not automatic learning sources. Children do not receive automatic project-memory injection through their own runtime; explicitly inherited parent conversation remains reference material. Previously stored project memories can still be referenced in root Plan Mode, which does not automatically learn new ones.

Legacy records without workspace attribution remain unchanged and manually visible when that store is selected, but are never automatically injected into another project's prompt. A newly stated fact can be learned with proper project attribution even if an old unscoped record contains the same text. If the optional store is busy or unavailable, the CLI reports the issue and continues without memory for that invocation; JSON output remains separate from diagnostics.

## Execution and platform boundaries

The CLI's managed command/Git/PR tools, state tools, built-in MCP transports, and host-owned worker requests support persistent execution through their specific completion contracts. A generic callback, custom registry replacement, or substituted client does not inherit that contract. A registered name in `/tools` establishes availability for discovery, not universal execution support. A known local process result is not a guarantee that every remote effect was rolled back or completed; uncertain remote results stay unknown.

Darwin and Linux have runtime coverage for managed process ownership. Windows has production cross-build coverage; ordinary-mode fallback execution does not establish equivalent persistent completion support. Native plugin platform support is separately constrained by Go's plugin mechanism.

Todo, task, and goal stores remain process-local, including separate child stores. Their execution records can survive restart, but resuming conversation history does not reconstruct those stores or replay mutations. A Todo receipt also does not promise the terminal has already rendered its latest snapshot.

The `serve` command exposes the private `go-cli-acp-json/1` profile. Its `grpc` setting uses JSON-over-HTTP rather than the official ACP wire protocol. It requires explicit `--approve auto` or `--approve deny`; remote ask approvals and remote TUI are unavailable.

## What informed the design

These references describe selected design inputs, not feature parity, performance rankings, or required dependencies.

| Project | Design input |
|---|---|
| [Pi Coding Agent](https://github.com/earendil-works/pi) | Shared Agent/session core and progressive capability exposure |
| [OpenCode v2](https://github.com/anomalyco/opencode) | Service ownership and lifecycle organization (v2 research reference) |
| [DeepSeek Harness / Cordis](https://github.com/deepseek-ai/deepseek-harness) | Harness composition, service ownership, and cleanup |
| [Magpie](https://github.com/yetone/magpie) | Separation of request Hooks and plugin contributions |
| [HashiCorp go-plugin](https://github.com/hashicorp/go-plugin) | Versioned subprocess contracts, startup handshakes, and process ownership |
| [CloudWeGo Eino](https://github.com/cloudwego/eino), [Google ADK-Go](https://github.com/google/adk-go) | Agent and model composition inspiration |

The CLI uses its own JSONL worker contract. It does not import the reference projects' runtimes, and its local `EinoProvider` is a standard-library OpenAI-compatible adapter.
