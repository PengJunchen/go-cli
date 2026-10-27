# go-cli

A terminal assistant written in Go. Ask it to explore a repository, edit code, run commands, or delegate a focused task. It connects to your model endpoint and keeps tool use visible through approval, session records, and tracing.

The CLI and public extension worker API are experimental. The executable needs no separately installed VM; model endpoints, MCP servers, and workers have their own requirements.

[中文文档](README.zh-CN.md) · [Runtime guide](docs/runtime-guide.md) · [Extension worker API](pkg/extensionapi/v1/README.md)

## About this project

The architecture has evolved through three stages:

1. **Core CLI:** Harness, Agent, and AgentLoop connect models, tools, sessions, and context compaction through explicit interfaces.
2. **Reliable execution:** approval and project trust, durable sessions, cancellation and resource ownership, tracing, and the TUI make execution inspectable and controllable.
3. **Dynamic, lightweight extensions:** progressive Skills, lazy MCP discovery, refresh at idle boundaries, and a public JSONL worker API work across full, headless, and `no_plugin` builds. The separate `edge` profile applies capability limits and lazy loading.

[go-agent-core-v1](https://github.com/PengJunchen/go-agent-core) is the related interface-driven Agent core SDK. This repository is the complete CLI; its public worker API serves subprocess extensions. Internal Go interfaces are not a public embedding SDK.

Author: [pjcmice.com](https://www.pjcmice.com)

## Quick start

Requires Go 1.24+; the exact minimum is declared in [go.mod](go.mod).

```sh
go mod download
make build
./bin/go-cli --help
```

Configure an OpenAI-compatible model endpoint, then start a conversation:

```sh
export GO_CLI_PROVIDER_NAME=eino
export GO_CLI_PROVIDER_BASE_URL=https://api.openai.com/v1
export GO_CLI_PROVIDER_API_KEY="your-api-key"
export GO_CLI_MODEL_NAME=your-model-id

./bin/go-cli prompt "Explain the structure of this repository"
./bin/go-cli interactive
```

The local `EinoProvider` uses Go's standard HTTP client; it does not import the Eino framework. Keep API keys in environment variables or configured credential storage.

## Configuration and project trust

Put personal settings in `~/.go-cli/config.yaml`. For example, save conversations and JSONL traces, and add a working preference:

```yaml
session:
  store_path: .go-cli/sessions
tracing:
  enabled: true
  exporter: jsonl
agent:
  append_system_prompt: "Explain meaningful changes before editing."
```

`agent.append_system_prompt` adds instructions to the normal assistant prompt. Use `agent.system_prompt` only to replace that base prompt. There is no automatic `SYSTEM.md` discovery.

Project settings belong in `.go-cli.yaml` or `.go-cli.json`. Project configuration and captured extension sources take effect only after you review and trust them:

```sh
# Run inside the project; review the sources, then type TRUST when prompted.
./bin/go-cli trust grant
./bin/go-cli trust list
./bin/go-cli interactive
# Use the canonical root shown by trust list:
./bin/go-cli trust revoke /absolute/project/root
```

A grant takes effect after verification at a later startup or supported idle refresh; it does not directly change an active run. It authorizes captured sources, not every future file at that path. Changes may require a new grant and, in some cases, restart. Without a valid grant, the CLI continues with personal settings alone. Trusting a source and approving a tool action are separate decisions.

## Everyday use

```sh
./bin/go-cli doctor
./bin/go-cli models
./bin/go-cli prompt --output json "Describe this project"
./bin/go-cli interactive --resume-session <physical-session-id>
```

In an interactive session:

| Command | Use it to |
|---|---|
| `/help` | See commands and their current limits |
| `/tools` | See tools registered for this session |
| `/plan enter`, `/plan exit` | Enter or leave Plan Mode |
| `/session tree`, `/session pending` | Inspect saved sessions and unresolved actions |
| `/reload` | Refresh supported Skills, prompts, and hooks when idle |
| `/mcp list` | See configured MCP servers |
| `/mcp load <server>` | Connect a server in lazy mode |
| `/mcp reload <server>` | Refresh its tool list when idle in lazy mode |

Saved conversations require `session.store_path`. Resume uses the exact physical session ID, including an ID returned by a child agent. If an action's outcome is `unknown`, inspect the pending action before repeating it: cancellation or a lost connection does not prove that an external action stopped.

### Delegate a task

Ask the assistant to delegate an investigation, implementation, review, or test task. The `dispatch_subagent` tool runs one task or several tasks in parallel and returns their results. You can request a role, model, tool list, and turn limit.

Each child has its own conversation, tool registry, approvals, and execution resources. The default `context_mode: independent` starts from the task instructions. `context_mode: inherit` also supplies a snapshot of the parent conversation as reference information; it does not copy permissions or execution records. With session storage enabled, the text result shows each child's physical session ID; structured result metadata also carries the IDs. Use a child ID for separate resume.

The current tool waits for completion; it is not a background-agent management interface. Parent cancellation also cancels and joins child resources; uncertain external results stay `unknown`. See [task examples and boundaries](docs/runtime-guide.md#delegate-work-to-child-agents).

## Extend the assistant

Start with files and configuration for instructions and workflows. Use MCP to connect tools from another service, or a worker when you need a separate program with its own language and dependencies.

| Need | Choose | Where it loads |
|---|---|---|
| Reusable instructions or a workflow | Skill files | `skill.dir`, or the default Skill directory |
| A personal or team rule | `agent.append_system_prompt` | Personal or trusted project configuration |
| A reusable slash prompt | Markdown command | `commands.dir`, or the default command directory |
| An action at a lifecycle event | Shell Hook | Main configuration or `.go-cli/hooks.yaml` |
| Tools supplied by another program/service | MCP server | `mcp.servers` |
| Custom tools, hooks, commands, prompts, or a provider | JSONL worker | `extensions.plugin_paths` |

To refresh supported files and settings without choosing edge, enable `runtime.dynamic: true`; otherwise restart. Progressive Skills and lazy MCP also enable dynamic operation.

### Skills and slash commands

A Skill teaches the assistant how to perform a task. Put a standard Skill at `.go-cli/skills/review/SKILL.md`:

```markdown
---
name: review
description: Review a change for correctness and missing tests.
---
Read the diff, explain concrete risks, and suggest focused checks.
```

The default is the first existing directory: `.go-cli/skills`, then `~/.config/go-cli/skills`. Override it with `skill.dir`. Existing flat Markdown and YAML Skill definitions remain supported. Regular loading reads Skills at startup. To index metadata first and load instructions on demand, opt in:

```yaml
skill:
  dir: .go-cli/skills
  progressive: true
```

For a prompt you want to invoke yourself, put `review.md` in `.go-cli/commands` or `~/.config/go-cli/commands`. It becomes `/review`; `$ARGUMENTS` inserts text after the command. `commands.dir` overrides discovery. Commands load with the interactive command table; restart after editing them. See [file examples](docs/runtime-guide.md#skills-prompts-and-custom-commands).

### Hooks

Hooks run a shell command at `session_start`, `pre_tool_use`, `post_tool_use`, or `session_end`. For example, this personal configuration records completed tool events:

Create the log directory in your working project first:

```sh
mkdir -p .go-cli
```

```yaml
hooks:
  hooks:
    - event: post_tool_use
      command: "cat >> .go-cli/tool-events.jsonl"
      timeout: 5s
```

The command receives event JSON on stdin. A post-tool Hook runs as a separate Bash action after the original tool batch has known outcomes, with its own approval and session record. Hook failure does not rerun the original tool. You have time to read the approval request: the script execution timeout starts when execution begins. Ordinary pre-tool Hook startup errors can allow execution to continue; use approval policy for mandatory restrictions. The [Hook guide](docs/runtime-guide.md#shell-hooks) explains timing and the different project-file format.

### MCP tools

MCP lets the assistant call tools hosted by another program or service. Add a server to personal configuration:

```yaml
mcp:
  loading: lazy
  servers:
    - name: docs
      url: https://your-mcp-service.example/mcp
```

Replace the URL with your service. Omit `loading` for default `eager` behavior: connect and list tools at startup. In `lazy` mode, the assistant uses `mcp_list` for local server metadata and `mcp_discover` to connect a selected server; you can also use `/mcp load docs`. Its schemas appear on the next model request. Discovery does not approve tool execution.

The built-in HTTP and host-owned stdio transports support actual tool calls in persistent sessions. Calls still pass normal approval. A lost connection, timeout, or invalid response can leave an unknown outcome; calls are not automatically replayed. Generic client wrappers do not gain this support automatically. See [MCP setup and execution](docs/runtime-guide.md#mcp-setup-and-execution) for local commands and project source restrictions.

### Extension workers

Register a trusted local `.rpc.json` descriptor to add functionality implemented in a separate program:

```yaml
extensions:
  enabled: true
  plugin_paths:
    - ./plugins/helper.rpc.json
```

The CLI starts the worker and exchanges an `initialize` handshake during startup. Contributions are registered before the agent starts; changing the worker or descriptor requires restart. The [worker API guide](pkg/extensionapi/v1/README.md) includes the descriptor and protocol.

A worker can load, but the tools it contributes currently cannot execute in persistent sessions. Workers share your OS permissions; they are not an OS sandbox. Worker providers return a final response rather than token streaming.

## Build choices

The ordinary build includes the rich terminal UI. `headless` provides line-oriented interaction. `no_plugin` removes only the optional native Go `.so` loader.

| Build | Go tags | Terminal UI | Optional native Go plugin loader |
|---|---|---|---|
| Full | none | Rich TUI | Available |
| Full without native plugins | `no_plugin` | Rich TUI | Disabled |
| Headless | `headless` | Line-oriented | Available |
| Headless without native plugins | `headless,no_plugin` | Line-oriented | Disabled |

```sh
go build -o ./bin/go-cli ./cmd/cli
go build -tags no_plugin -o ./bin/go-cli-no-plugin ./cmd/cli
go build -tags headless -o ./bin/go-cli-headless ./cmd/cli
go build -tags headless,no_plugin -o ./bin/go-cli-headless-no-plugin ./cmd/cli
```

All variants share the Agent core, file extensions, MCP, and workers, with the individual limits described above. Headless retains multi-turn input, final responses, SIGINT cancellation, and text approval; fullscreen rendering and in-flight keyboard steering require the rich UI.

### Load capabilities on demand

Choose the `edge` runtime profile independently of build tags:

```yaml
runtime:
  profile: edge
```

This enables dynamic capabilities, progressive Skills, and lazy MCP. Defaults are **128 Skills, 2 active MCP servers, and 128 KiB** of capability prompt/schema budget; positive `max_skills`, `max_mcp_servers`, and `max_prompt_bytes` override them. It changes loading policy, not linked packages. Project Skill content is still captured for trust checks. See [profiles and refresh](docs/runtime-guide.md#build-tags-and-runtime-profile-are-separate-axes).

### Advanced: native Go plugins

Go code compiled into the host can already call typed tools and Hooks directly in process, without a `.so`. A native plugin is an optional way to load Go code at runtime; ordinary extensions do not require it, and it is not automatically faster than compiled-in code. Consider it only when runtime loading is needed and a measured benefit justifies its constraints.

Native plugins require supported platforms and closely matching toolchains and dependencies. They run with host privileges, can affect the whole process, cannot be unloaded, and need restart for updates. See the [runtime guide](docs/runtime-guide.md#optional-native-go-plugins) and [Go plugin documentation](https://pkg.go.dev/plugin).

## Current limits

Persistent sessions support exact resume, but not durable fork, clone, branch switching, snapshots, or schema migration. Some historical commands are also unavailable; `/help` describes the current surface. `/memory` manages manually entered records without automatic extraction or prompt injection. Todo, task, and goal state is process-local.

Some registered tools, including ordinary custom/worker tools and `git_pr_create`, cannot yet execute in persistent sessions. `/tools` shows registered names, so check the [runtime guide](docs/runtime-guide.md) for execution limits. MCP currently integrates tools only; workers cannot supply Skills through the RPC protocol.

## Development

```sh
make test           # Automated tests with race detection
make test-e2e       # Tests carrying the e2e build tag
make test-no-plugin # Native-loader stub coverage in extension/CLI packages
make test-cov       # Coverage report
make scan           # AST rules
make lint           # golangci-lint
make fmt            # gofmt and goimports
make check          # Formatting, vet, lint, build, tests
make verify         # check plus AST, log, and goroutine checks
```

Production code lives in `cmd/`, `internal/`, and `pkg/`. Automated integration tests and their required fixtures live in `tests/`; build and verification tasks are defined in [Makefile](Makefile). The [documentation index](docs/README.md) links current product and API guides.

## Design references

These projects informed selected design choices and are not required runtime dependencies:

| Project | Design input |
|---|---|
| [Pi Coding Agent](https://github.com/earendil-works/pi) | Shared Agent/session core and progressive capability exposure |
| [OpenCode v2](https://github.com/anomalyco/opencode) | Explicit service ownership, lifecycle scopes, and reload failure boundaries |
| [DeepSeek Harness / Cordis](https://github.com/deepseek-ai/deepseek-harness) | Harness composition, dependency ownership, and cleanup |
| [Magpie](https://github.com/yetone/magpie) | Plugin ownership and separation of hooks from subprocess extensions |
| [HashiCorp go-plugin](https://github.com/hashicorp/go-plugin) | Versioned subprocess contracts, handshakes, and process ownership |
| [CloudWeGo Eino](https://github.com/cloudwego/eino), [Google ADK-Go](https://github.com/google/adk-go) | Agent and model composition inspiration |

## License

MIT.
