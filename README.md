# go-cli

A terminal AI assistant written in Go, configurable around the way you work. Choose your model, instructions, tools, Skills, Hooks, and external services; use approval, saved conversations, and tracing to follow its work.

The CLI and public extension worker API are experimental.

[中文文档](README.zh-CN.md) · [Runtime guide](docs/runtime-guide.md) · [Extension worker API](pkg/extensionapi/v1/README.md)

## Make it your own

| What you want to change | How |
|---|---|
| Model and response style | Choose an OpenAI-compatible endpoint, model, sampling settings, thinking level, and turn limit |
| Working rules | Append to or replace the system prompt; keep team instructions in trusted project configuration |
| Reusable workflows | Load Markdown/YAML Skills, including standard `SKILL.md`; add your own reusable `/command` prompts |
| Tools | Select built-ins, wrap a local command, or connect standard MCP tools |
| Workflow automation | Run pre/post-tool Shell Hooks, with event JSON on stdin |
| Features in another language | Register a JSONL worker for tools, Hooks, commands, prompt material, or complete-response providers |
| Execution and records | Configure approval, compaction, timeouts, session saving, and tracing |

Personal settings live in `~/.go-cli/config.yaml`; project settings use `.go-cli.yaml` or `.go-cli.json` and require source trust. Most settings load at startup. Supported Skill, prompt, and Hook changes can refresh with idle `/reload` in dynamic mode; command files, custom tools, and workers require restart. See [configuration and loading](docs/runtime-guide.md#choose-an-extension-by-what-you-need).

Ordinary customization starts with files and configuration. A [native Go `.so`](docs/runtime-guide.md#optional-native-go-plugins) is an advanced option for trusted code that specifically needs runtime loading and in-process integration.

## Work efficiently

- **Expose capabilities as needed.** Progressive Skills present descriptions first and load selected instructions on demand. Lazy MCP connects a server when selected; default eager MCP connects at startup. Project Skill bodies are still captured for trust checks.
- **Continue and explore alternatives.** Save a conversation, resume its exact physical session, fork or clone a branch, retry a completed answer, or compact older context. These operations retain history; retry does not undo earlier tool effects.
- **Remember project preferences.** Automatic memory is on by default and can carry explicitly stated preferences and facts into later conversations in the same project. Extraction makes an extra model request; normal exit may wait up to 30 seconds. It uses a bounded best-effort queue, so every turn is not guaranteed to be stored. Set `memory.enabled: false` to disable automatic extraction and injection.
- **Delegate focused tasks.** Ask for one child agent or several in parallel, with their own roles, models, tools, and turn limits. Each has its own execution resources and approvals; the tool waits for completion.
- **Choose a smaller interface.** The Go executable needs no separately installed VM. `headless` uses line-oriented interaction; `no_plugin` removes the optional native loader. External services and workers retain their own dependencies.

The optional `edge` profile enables dynamic refresh, progressive Skills, and lazy MCP with configurable capability budgets. It is independent of build tags. These choices control startup work and context size; they are not claims about model latency or overall performance versus other agents.

Skill catalogs reuse preprocessed search text to reduce repeated work and temporary allocations, at the cost of retaining extra search metadata.

Native `.so` calls avoid subprocess communication, but Go code compiled into the CLI already runs directly in process. Plugins are not inherently faster: they require matching platforms/toolchains/dependencies, share the host's crash risk, cannot be reliably unloaded, and need restart for updates. [Build choices and tradeoffs](docs/runtime-guide.md#choose-a-build) explain when to use them.

## Quick start

Requires Go 1.24+; the exact minimum is declared in [go.mod](go.mod).

```sh
go mod download
make build
./bin/go-cli --help

export GO_CLI_PROVIDER_NAME=eino
export GO_CLI_PROVIDER_BASE_URL=https://api.openai.com/v1
export GO_CLI_PROVIDER_API_KEY="your-api-key"
export GO_CLI_MODEL_NAME=your-model-id

./bin/go-cli prompt "Explain the structure of this repository"
./bin/go-cli interactive
```

The local `EinoProvider` uses Go's standard HTTP client, without importing the Eino framework. Keep API keys in environment variables or configured credential storage.

To save conversations and add a working rule, put this in personal configuration:

```yaml
session:
  store_path: .go-cli/sessions
agent:
  append_system_prompt: "Explain meaningful changes before editing."
```

`append_system_prompt` adds to the normal prompt; `system_prompt` replaces it. There is no automatic `SYSTEM.md` discovery. Session paths are relative to your working directory unless absolute. Automatic project memory is separate from session saving.

Review project configuration and extension sources before trusting them:

```sh
# Run inside the project; review the summary and type TRUST when prompted.
./bin/go-cli trust grant
./bin/go-cli trust list
./bin/go-cli interactive
# Revoke using the canonical root shown by trust list:
./bin/go-cli trust revoke /absolute/project/root
```

Trust binds reviewed source content, not every future file at that path. Changed sources may need a new grant and restart or supported refresh. It does not replace approval for individual actions.

## Everyday use

```sh
./bin/go-cli doctor
./bin/go-cli models
./bin/go-cli prompt --output json "Describe this project"
./bin/go-cli session list --store .go-cli/sessions
./bin/go-cli interactive --resume-session <physical-session-id>
```

| Interactive command | Purpose |
|---|---|
| `/help`, `/tools` | See available commands and registered tools |
| `/plan enter`, `/plan exit` | Enter or leave Plan Mode |
| `/session tree`, `/session branches` | Inspect conversation entries and branches |
| `/session resume`, `/session resume <physical-id>` | List saved sessions or open an exact one |
| `/session fork feature`, `/session switch main` | Explore another branch and return |
| `/clear`, `/retry`, `/compact` | Start fresh, retry a completed answer, or reduce older context |
| `/session pending` | Inspect actions whose outcomes are unresolved |
| `/memory list`, `/memory add <text>` | Inspect or add explicit memories |
| `/reload` | Refresh supported dynamic sources when idle |
| `/mcp list`, `/mcp load docs`, `/mcp reload docs` | Inspect, activate, or refresh lazy MCP servers |

Saved history requires `session.store_path`. A physical session ID names a saved conversation file; an entry ID names a point inside it. `@session:<entry-id>` quotes visible history from the selected physical session, including another branch; it does not resume that session. See [sessions and recovery](docs/runtime-guide.md#sessions-and-recovery) for clone, prompt fork, legacy import, and Git-aware navigation.

Cancellation or a lost response can leave an external action's outcome unknown. Inspect pending actions before repeating work; history navigation does not resolve them.

## Add a workflow or external tool

A standard `.go-cli/skills/review/SKILL.md`:

```markdown
---
name: review
description: Review changes for correctness and missing tests.
---
Read the diff, explain concrete risks, and suggest focused checks.
```

The default Skill directory is the first existing directory: `.go-cli/skills`, then `~/.config/go-cli/skills`; `skill.dir` overrides it. To expose descriptions before loading instructions:

```yaml
skill:
  progressive: true
```

For other workflows, choose the smallest extension that fits:

| Need | Guide |
|---|---|
| A `/review` prompt that appends your trailing arguments | [Markdown commands](docs/runtime-guide.md#markdown-slash-commands) |
| A local executable with arguments, environment, cwd, and timeout | [Configured command tools](docs/runtime-guide.md#configured-command-tools) |
| Checks before tools or follow-up after known results | [Shell Hooks](docs/runtime-guide.md#shell-hooks) |
| Tools from an existing service | [MCP setup and real tool calls](docs/runtime-guide.md#mcp-setup-and-execution) |
| A Python, JavaScript, Go, or other subprocess extension | [JSONL workers](docs/runtime-guide.md#jsonl-extension-workers) |
| Focused single or parallel child tasks | [Child agents](docs/runtime-guide.md#delegate-work-to-child-agents) |

Built-in MCP transports, host-owned workers, and the CLI's managed command/Git tools can execute in persistent sessions within their supported completion contracts. Arbitrary native callbacks, replacement registries, and generic client wrappers do not automatically gain that support. Being listed in `/tools` is not a promise of execution in every mode.

## Current boundaries

Automatic memory is scoped to the project and is not learned by child agents or in Plan Mode. Old unscoped records remain available for manual inspection when using their store, but are not automatically injected across projects. [Memory settings](docs/runtime-guide.md#project-memory) explain storage and costs.

Todo, task, and goal state is process-local. Saving or resuming a conversation preserves its records, not those stores. Session snapshots and general schema migration remain unsupported; old JSONL uses explicit strict import. MCP integration exposes tools, not the full resources/prompts surface. Workers provide final responses rather than token streaming and cannot declare RPC Skills.

## About this project

Three stages describe the architecture's evolution:

1. **Configurable core:** Harness, Agent, and AgentLoop connect models, tools, compaction, Skills, MCP, approval, sessions, native plugins, tracing, and the TUI through explicit interfaces.
2. **Recoverable execution:** durable execution records and exact resume strengthen existing sessions; source-bound project trust, controlled cancellation, and owned resources make actions easier to inspect and manage.
3. **Dynamic extensions:** capability generations and idle refresh, progressive Skills, lazy MCP, portable JSONL workers, and headless/edge options support different workloads. Current session branching and project memory extend everyday continuity.

[go-agent-core-v1](https://github.com/PengJunchen/go-agent-core) is the related Agent core SDK. This repository is the complete CLI; its internal Go interfaces are not a public embedding SDK. Author: [pjcmice.com](https://www.pjcmice.com).

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
| [OpenCode v2](https://github.com/anomalyco/opencode) | Service ownership and lifecycle organization (v2 research reference) |
| [DeepSeek Harness / Cordis](https://github.com/deepseek-ai/deepseek-harness) | Harness composition, dependency ownership, and cleanup |
| [Magpie](https://github.com/yetone/magpie) | Separation of request Hooks and plugin contributions |
| [HashiCorp go-plugin](https://github.com/hashicorp/go-plugin) | Versioned subprocess contracts, handshakes, and process ownership |
| [CloudWeGo Eino](https://github.com/cloudwego/eino), [Google ADK-Go](https://github.com/google/adk-go) | Agent and model composition inspiration |

## License

MIT.
